package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/robfig/cron/v3"
)

// RunStarter is the only execution boundary used by triggers.
type RunStarter interface {
	Start(context.Context, StartRunInput) (RunSnapshot, error)
}

type cronEngine interface {
	AddFunc(string, func()) (int, error)
	Remove(int)
}

type cronCloser interface{ Stop() context.Context }

type robfigCron struct{ *cron.Cron }

func (c robfigCron) AddFunc(spec string, fn func()) (int, error) {
	id, err := c.Cron.AddFunc(spec, fn)
	return int(id), err
}

func (c robfigCron) Remove(id int) { c.Cron.Remove(cron.EntryID(id)) }

type scheduledJob struct {
	entry      int
	revision   uint64
	spec       string
	generation uint64
}

// Scheduler owns only SyncBridge cron registrations. It never executes jobs.
type Scheduler struct {
	mu          sync.Mutex
	reconcileMu sync.Mutex
	callbacks   sync.WaitGroup
	ctx         context.Context
	cancel      context.CancelFunc
	starter     RunStarter
	cron        cronEngine
	entries     map[int]scheduledJob
	errors      map[int]error
	nextGen     uint64
	closed      bool
}

func NewScheduler(ctx context.Context, starter RunStarter) *Scheduler {
	if ctx == nil {
		panic("scheduler context is nil")
	}
	c := cron.New(cron.WithSeconds())
	c.Start()
	return newScheduler(ctx, starter, robfigCron{c})
}

func newScheduler(ctx context.Context, starter RunStarter, engine cronEngine) *Scheduler {
	child, cancel := context.WithCancel(ctx)
	return &Scheduler{ctx: child, cancel: cancel, starter: starter, cron: engine, entries: make(map[int]scheduledJob), errors: make(map[int]error)}
}

func newSchedulerForTest(starter RunStarter, engine cronEngine) *Scheduler {
	return newScheduler(context.Background(), starter, engine)
}

// Reconcile serializes external cron changes without holding the state lock.
// Invalid replacements preserve their prior valid registration.
func (s *Scheduler) Reconcile(jobs []Job) error {
	if s == nil || s.starter == nil || s.cron == nil {
		return errors.New("scheduler is not configured")
	}
	desired := make(map[int]scheduledJob)
	invalid := make(map[int]error)
	for _, job := range jobs {
		if !schedulerEligible(job, TriggerCron) {
			continue
		}
		spec, err := normalizeCronSpec(job.Cron)
		if err != nil {
			invalid[job.ID] = err
			continue
		}
		desired[job.ID] = scheduledJob{revision: job.Revision, spec: spec}
	}

	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("scheduler is closed")
	}
	next := make(map[int]scheduledJob, len(desired))
	added := make(map[int]scheduledJob)
	removed := make([]int, 0)
	for id, current := range s.entries {
		want, wanted := desired[id]
		if _, bad := invalid[id]; bad {
			next[id] = current
			continue
		}
		if !wanted {
			removed = append(removed, current.entry)
			continue
		}
		if current.revision == want.revision && current.spec == want.spec {
			next[id] = current
			continue
		}
		s.nextGen++
		want.generation = s.nextGen
		next[id] = want
		added[id] = want
		removed = append(removed, current.entry)
	}
	for id, want := range desired {
		if _, exists := s.entries[id]; exists {
			continue
		}
		s.nextGen++
		want.generation = s.nextGen
		next[id] = want
		added[id] = want
	}
	s.mu.Unlock()

	for id, want := range added {
		id, want := id, want
		entry, err := s.cron.AddFunc(want.spec, func() { s.fire(id, want.revision, want.generation) })
		if err != nil {
			for _, installed := range added {
				if installed.entry != 0 {
					s.cron.Remove(installed.entry)
				}
			}
			s.mu.Lock()
			if !s.closed {
				s.errors[id] = err
			}
			s.mu.Unlock()
			return fmt.Errorf("job %d: %w", id, err)
		}
		want.entry = entry
		added[id] = want
		next[id] = want
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		for _, installed := range added {
			s.cron.Remove(installed.entry)
		}
		return errors.New("scheduler is closed")
	}
	s.entries = next
	for id, err := range invalid {
		s.errors[id] = err
	}
	for id := range desired {
		if _, bad := invalid[id]; !bad {
			delete(s.errors, id)
		}
	}
	s.mu.Unlock()
	for _, entry := range removed {
		s.cron.Remove(entry)
	}
	return errors.Join(mapErrors(invalid)...)
}

func (s *Scheduler) fire(id int, revision, generation uint64) {
	s.mu.Lock()
	entry, ok := s.entries[id]
	if !ok || s.closed || entry.revision != revision || entry.generation != generation {
		s.mu.Unlock()
		return
	}
	s.callbacks.Add(1)
	ctx := s.ctx
	s.mu.Unlock()

	_, err := s.starter.Start(ctx, StartRunInput{JobID: id, Revision: revision, Origin: RunOriginCron})
	s.mu.Lock()
	current, ok := s.entries[id]
	if err != nil && !errors.Is(err, ErrOverlap) && ok && current.generation == generation {
		s.errors[id] = err
	}
	s.mu.Unlock()
	s.callbacks.Done()
}

func (s *Scheduler) LastError(id int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.errors[id]
}

func (s *Scheduler) Close() {
	if s == nil {
		return
	}
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.entries = make(map[int]scheduledJob)
	s.cancel()
	closer, ok := s.cron.(cronCloser)
	s.mu.Unlock()
	if ok {
		<-closer.Stop().Done()
	}
	s.callbacks.Wait()
}

func schedulerEligible(job Job, trigger Trigger) bool {
	return job.Enabled && !job.NeedsReview && job.Scheduler.Owner == SchedulerSyncBridge && job.Trigger == trigger
}

func normalizeCronSpec(spec string) (string, error) {
	fields := strings.Fields(spec)
	if len(fields) == 5 {
		return "0 " + strings.Join(fields, " "), nil
	}
	if len(fields) == 6 {
		return strings.Join(fields, " "), nil
	}
	return "", errors.New("cron expression must have five or six fields")
}

func mapErrors(values map[int]error) []error {
	errs := make([]error, 0, len(values))
	for id, err := range values {
		errs = append(errs, fmt.Errorf("job %d: %w", id, err))
	}
	return errs
}
