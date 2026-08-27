package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"
)

var (
	ErrRevisionConflict     = errors.New("job revision conflict")
	ErrJobNotFound          = errors.New("job not found")
	ErrCorruptJobRepository = errors.New("corrupt job repository")
)

type jobStore struct {
	SchemaVersion int   `json:"schemaVersion"`
	Next          int   `json:"next"`
	Jobs          []Job `json:"jobs"`
}

type legacyJobStore struct {
	Next int   `json:"next"`
	Jobs []Job `json:"jobs"`
}

// JobRepository owns v2 job snapshots and their durable persistence.
type JobRepository struct {
	mu    sync.RWMutex
	path  string
	owner FileOwner
	next  int
	jobs  map[int]Job
}

// OpenJobRepository loads path, migrating a legacy v1 file to schema v2 when
// necessary. The exact legacy bytes are backed up before the v2 file is written.
func OpenJobRepository(path string, owner FileOwner) (*JobRepository, error) {
	repo := &JobRepository{path: path, owner: owner, next: 1, jobs: make(map[int]Job)}
	if err := repo.withExclusiveFileLock(func() error { return repo.loadLocked() }); err != nil {
		return nil, err
	}
	return repo, nil
}

func (r *JobRepository) List() []Job {
	r.mu.RLock()
	defer r.mu.RUnlock()
	jobs := make([]Job, 0, len(r.jobs))
	for _, job := range r.jobs {
		jobs = append(jobs, cloneJob(job))
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].ID < jobs[j].ID })
	return jobs
}

func (r *JobRepository) Get(id int) (Job, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	job, ok := r.jobs[id]
	return cloneJob(job), ok
}

func (r *JobRepository) Create(job Job) (Job, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var created Job
	err := r.withExclusiveFileLock(func() error {
		if err := r.loadLocked(); err != nil {
			return err
		}
		created = cloneJob(job)
		created.SchemaVersion = 2
		created.ID = r.next
		created.Revision = 1
		if err := r.persistCandidateLocked(r.next+1, withJob(r.jobs, created)); err != nil {
			return err
		}
		r.next++
		r.jobs[created.ID] = created
		return nil
	})
	return cloneJob(created), err
}

func (r *JobRepository) Update(id int, revision uint64, job Job) (Job, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var updated Job
	if err := r.withExclusiveFileLock(func() error {
		if err := r.loadLocked(); err != nil {
			return err
		}
		current, ok := r.jobs[id]
		if !ok {
			return ErrJobNotFound
		}
		if current.Revision != revision {
			return ErrRevisionConflict
		}
		updated = cloneJob(job)
		updated.SchemaVersion = 2
		updated.ID = id
		updated.Revision = current.Revision + 1
		if err := r.persistCandidateLocked(r.next, withJob(r.jobs, updated)); err != nil {
			return err
		}
		r.jobs[id] = updated
		return nil
	}); err != nil {
		return Job{}, err
	}
	return cloneJob(updated), nil
}

func (r *JobRepository) Delete(id int, revision uint64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.withExclusiveFileLock(func() error {
		if err := r.loadLocked(); err != nil {
			return err
		}
		current, ok := r.jobs[id]
		if !ok {
			return ErrJobNotFound
		}
		if current.Revision != revision {
			return ErrRevisionConflict
		}
		candidate := make(map[int]Job, len(r.jobs)-1)
		for key, job := range r.jobs {
			if key != id {
				candidate[key] = job
			}
		}
		if err := r.persistCandidateLocked(r.next, candidate); err != nil {
			return err
		}
		delete(r.jobs, id)
		return nil
	})
}

func (r *JobRepository) loadLocked() error {
	data, err := os.ReadFile(r.path)
	if errors.Is(err, os.ErrNotExist) {
		r.next = 1
		r.jobs = make(map[int]Job)
		return nil
	}
	if err != nil {
		return fmt.Errorf("read job repository: %w", err)
	}
	if len(data) == 0 {
		return fmt.Errorf("%w: empty file", ErrCorruptJobRepository)
	}
	var header struct {
		SchemaVersion *int `json:"schemaVersion"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return fmt.Errorf("%w: invalid JSON: %v", ErrCorruptJobRepository, err)
	}
	if header.SchemaVersion == nil {
		return r.migrateLegacyLocked(data)
	}
	if *header.SchemaVersion != 2 {
		return fmt.Errorf("%w: unsupported schema version %d", ErrCorruptJobRepository, *header.SchemaVersion)
	}
	var store jobStore
	if err := json.Unmarshal(data, &store); err != nil {
		return fmt.Errorf("%w: invalid v2 JSON: %v", ErrCorruptJobRepository, err)
	}
	if err := r.replaceLocked(store.Next, store.Jobs); err != nil {
		return err
	}
	return nil
}

func (r *JobRepository) migrateLegacyLocked(data []byte) error {
	var legacy legacyJobStore
	if err := json.Unmarshal(data, &legacy); err != nil {
		return fmt.Errorf("%w: invalid legacy JSON: %v", ErrCorruptJobRepository, err)
	}
	if legacy.Next < 1 {
		return fmt.Errorf("%w: invalid legacy next id", ErrCorruptJobRepository)
	}
	jobs := make([]Job, 0, len(legacy.Jobs))
	for _, job := range legacy.Jobs {
		job.SchemaVersion = 2
		job.Revision = 1
		job.Enabled = !job.Disabled
		job.Action = Action{Type: ActionSync, Sync: SyncAction{
			Engine: job.Engine, Source: job.Source, Dest: job.Dest, Mode: job.Mode,
			Compare: job.Compare, Bwlimit: job.Bwlimit, Backup: job.Backup, BackupKeep: job.BackupKeep,
			MaxDel: job.MaxDel, SkipNew: job.SkipNew, SysBackup: job.SysBackup, Exclude: job.Exclude,
		}}
		if job.Kind == "command" {
			job.Action = Action{Type: ActionCommand, Command: job.Command}
		}
		job.Scheduler.Owner = SchedulerSyncBridge
		// Legacy jobs never persisted a verified host execution identity. They
		// must remain inert until an operator reviews and selects one explicitly.
		job.NeedsReview = true
		if job.Backend == "system" {
			job.Scheduler.Owner = SchedulerSystem
		}
		jobs = append(jobs, job)
	}
	if err := r.replaceLocked(legacy.Next, jobs); err != nil {
		return err
	}
	backup := fmt.Sprintf("%s.v1.%s.bak", r.path, time.Now().UTC().Format("20060102T150405.000000000Z"))
	if err := AtomicWriteFile(backup, data, 0o600, r.owner); err != nil {
		return fmt.Errorf("write v1 migration backup: %w", err)
	}
	return r.persistCandidateLocked(r.next, r.jobs)
}

func (r *JobRepository) replaceLocked(next int, jobs []Job) error {
	if next < 1 {
		return fmt.Errorf("%w: invalid next id", ErrCorruptJobRepository)
	}
	r.jobs = make(map[int]Job, len(jobs))
	r.next = next
	for _, job := range jobs {
		if job.ID < 1 || job.SchemaVersion != 2 {
			return fmt.Errorf("%w: invalid job %#v", ErrCorruptJobRepository, job)
		}
		if _, exists := r.jobs[job.ID]; exists {
			return fmt.Errorf("%w: duplicate job id %d", ErrCorruptJobRepository, job.ID)
		}
		if job.ID >= r.next {
			r.next = job.ID + 1
		}
		r.jobs[job.ID] = cloneJob(job)
	}
	return nil
}

func (r *JobRepository) persistCandidateLocked(next int, jobs map[int]Job) error {
	list := make([]Job, 0, len(jobs))
	for _, job := range jobs {
		list = append(list, cloneJob(job))
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	data, err := json.MarshalIndent(jobStore{SchemaVersion: 2, Next: next, Jobs: list}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal job repository: %w", err)
	}
	data = append(data, '\n')
	if err := AtomicWriteFile(r.path, data, 0o600, r.owner); err != nil {
		return fmt.Errorf("persist job repository: %w", err)
	}
	return nil
}

func (r *JobRepository) withExclusiveFileLock(fn func() error) error {
	lockPath := r.path + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return fmt.Errorf("create repository lock directory: %w", err)
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open repository lock: %w", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock repository: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	return fn()
}

func withJob(jobs map[int]Job, job Job) map[int]Job {
	candidate := make(map[int]Job, len(jobs)+1)
	for id, current := range jobs {
		candidate[id] = current
	}
	candidate[job.ID] = job
	return candidate
}
