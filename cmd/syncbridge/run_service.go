package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	ErrOverlap      = errors.New("job already has an active run")
	ErrRunNotFound  = errors.New("run not found")
	ErrRunNotActive = errors.New("run is not active")
)

const (
	RunQueued         RunStatus = "queued"
	RunRunning        RunStatus = "running"
	RunSkippedOverlap RunStatus = "skipped_overlap"
)

// RunPlanCompiler is the narrow boundary used to compile an immutable job snapshot.
type RunPlanCompiler interface {
	Compile(context.Context, Job, RunRequest) (ExecutionPlan, error)
}

// WrapperInstaller installs the compiled wrapper before execution starts.
type WrapperInstaller interface {
	Install(context.Context, ExecutionPlan) (string, error)
}

type StartRunInput struct {
	JobID    int
	Revision uint64
	Origin   RunOrigin
	DryRun   bool
}

type RunSnapshot struct {
	ID              string    `json:"id"`
	JobID           int       `json:"jobId"`
	Revision        uint64    `json:"revision"`
	Status          RunStatus `json:"status"`
	Origin          RunOrigin `json:"origin"`
	DryRun          bool      `json:"dryRun"`
	RequestedAt     time.Time `json:"requestedAt"`
	StartedAt       time.Time `json:"startedAt,omitempty"`
	FinishedAt      time.Time `json:"finishedAt,omitempty"`
	EffectiveUID    int       `json:"effectiveUid,omitempty"`
	EffectiveGID    int       `json:"effectiveGid,omitempty"`
	ExitCode        int       `json:"exitCode"`
	KillEscalated   bool      `json:"killEscalated,omitempty"`
	DroppedLogLines uint64    `json:"droppedLogLines,omitempty"`
	LogError        string    `json:"logError,omitempty"`
	ControlError    string    `json:"controlError,omitempty"`
	HistoryError    string    `json:"historyError,omitempty"`
	Error           string    `json:"error,omitempty"`
}

type RunFilter struct {
	JobID  int
	Status RunStatus
	Limit  int
}

type RunEventKind string

const (
	RunEventState RunEventKind = "state"
	RunEventLog   RunEventKind = "log"
)

type RunEvent struct {
	ID   uint64       `json:"id"`
	Kind RunEventKind `json:"kind"`
	Run  RunSnapshot  `json:"run"`
	Log  []byte       `json:"log,omitempty"`
}

type RunSubscription struct {
	Events <-chan RunEvent
	Close  func()
}

type runState struct {
	snapshot      RunSnapshot
	job           Job
	log           *LogBuffer
	handle        ExecutionHandle
	ctx           context.Context
	cancel        context.CancelFunc
	cancelOnce    sync.Once
	starting      bool
	stopRequested bool
}

// RunService owns run state. Jobs are copied at reservation time; it never
// retains a repository-owned job pointer.
type RunService struct {
	mu              sync.RWMutex
	jobs            *JobRepository
	compiler        RunPlanCompiler
	installer       WrapperInstaller
	executor        Executor
	limits          LogLimits
	historyPath     string
	historyOwner    FileOwner
	historyAppend   func(uint64, RunSnapshot, []byte) error
	historyMu       sync.Mutex
	historyCond     *sync.Cond
	historyNext     uint64
	historySequence uint64
	terminalHook    func(Job, RunSnapshot)
	runs            map[string]*runState
	active          map[int]string
	pending         map[int]string
	eventID         uint64
	events          []RunEvent
	subscribers     map[uint64]chan RunEvent
	nextSubID       uint64
}

func NewRunService(jobs *JobRepository, compiler RunPlanCompiler, installer WrapperInstaller, executor Executor, limits LogLimits) *RunService {
	if limits.MaxRuns <= 0 {
		limits.MaxRuns = 100
	}
	if limits.MaxAge <= 0 {
		limits.MaxAge = 24 * time.Hour
	}
	service := &RunService{
		jobs: jobs, compiler: compiler, installer: installer, executor: executor, limits: limits,
		historyPath: histPath(), historyOwner: FileOwner{UID: fileUID, GID: fileGID},
		runs: make(map[string]*runState), active: make(map[int]string), pending: make(map[int]string),
		subscribers: make(map[uint64]chan RunEvent), historyNext: 1,
	}
	service.historyAppend = func(_ uint64, run RunSnapshot, logs []byte) error {
		return appendRunHistory(service.historyPath, service.historyOwner, run, logs)
	}
	service.terminalHook = handleRunTerminalEffects
	service.historyCond = sync.NewCond(&service.historyMu)
	return service
}

func (s *RunService) Start(ctx context.Context, input StartRunInput) (RunSnapshot, error) {
	if ctx == nil {
		return RunSnapshot{}, errors.New("start context is nil")
	}
	if s == nil || s.jobs == nil || s.compiler == nil || s.installer == nil || s.executor == nil {
		return RunSnapshot{}, errors.New("run service is not configured")
	}
	job, ok := s.jobs.Get(input.JobID)
	if !ok {
		return RunSnapshot{}, ErrJobNotFound
	}
	if input.Revision != 0 && input.Revision != job.Revision {
		return RunSnapshot{}, ErrRevisionConflict
	}
	if input.Origin == "" {
		input.Origin = RunOriginManual
	}
	id, err := newRunID()
	if err != nil {
		return RunSnapshot{}, err
	}
	runContext, cancel := context.WithCancel(context.WithoutCancel(ctx))
	run := &runState{snapshot: RunSnapshot{
		ID: id, JobID: job.ID, Revision: job.Revision, Status: RunQueued, Origin: input.Origin,
		DryRun: input.DryRun, RequestedAt: time.Now().UTC(), ExitCode: -1,
	}, job: cloneJob(job), log: NewLogBuffer(s.limits), ctx: runContext, cancel: cancel}

	s.mu.Lock()
	var replaced RunSnapshot
	var replacedLogs []byte
	replacedID := ""
	replacedSequence := uint64(0)
	if activeID := s.active[job.ID]; activeID != "" {
		if job.Execution.Overlap != "queue-latest" {
			// Rejection is still an observable terminal run.
			run.cancelOnce.Do(run.cancel)
			run.snapshot.Status = RunSkippedOverlap
			run.snapshot.FinishedAt = time.Now().UTC()
			run.snapshot.Error = ErrOverlap.Error()
			s.runs[id] = run
			s.publishLocked(run.snapshot)
			sequence := s.nextHistoryLocked()
			s.mu.Unlock()
			s.persistHistory(sequence, run.snapshot, nil)
			return run.snapshot, ErrOverlap
		}
		if previous := s.pending[job.ID]; previous != "" {
			previousRun := s.runs[previous]
			if s.finishLocked(previousRun, RunResult{Status: RunSkippedOverlap, ExitCode: -1, Err: ErrOverlap}) {
				replaced, replacedLogs, replacedID, replacedSequence = previousRun.snapshot, previousRun.log.Bytes(), previous, s.nextHistoryLocked()
			}
		}
		s.runs[id] = run
		s.pending[job.ID] = id
		s.publishLocked(run.snapshot)
		reserved := run.snapshot
		s.mu.Unlock()
		if replacedID != "" {
			s.persistHistory(replacedSequence, replaced, replacedLogs)
		}
		return reserved, nil
	}
	s.runs[id] = run
	s.active[job.ID] = id
	s.publishLocked(run.snapshot)
	reserved := run.snapshot
	s.mu.Unlock()
	go s.execute(runContext, id)
	return reserved, nil
}

func (s *RunService) execute(ctx context.Context, id string) {
	s.mu.RLock()
	run := s.runs[id]
	var job Job
	var request RunRequest
	if run != nil {
		job = cloneJob(run.job)
		request = RunRequest{RunID: run.snapshot.ID, JobID: run.snapshot.JobID, Revision: run.snapshot.Revision, Origin: run.snapshot.Origin, DryRun: run.snapshot.DryRun, RequestedAt: run.snapshot.RequestedAt}
	}
	s.mu.RUnlock()
	if run == nil {
		return
	}
	plan, err := s.compiler.Compile(ctx, job, request)
	if !s.queued(id) {
		return
	}
	if err == nil {
		plan.WrapperPath, err = s.installer.Install(ctx, plan)
	}
	if !s.queued(id) {
		return
	}
	if err != nil {
		s.finish(id, RunResult{Status: RunStartFailed, ExitCode: -1, Err: err})
		return
	}
	sink := NewLogSink(256)
	consumerDone := make(chan struct{})
	// The service is the consumer; it starts before the executor can produce.
	go func() {
		defer close(consumerDone)
		for line := range sink.Lines() {
			run.log.Append(line)
			s.publishLog(id, line)
		}
	}()
	s.mu.Lock()
	if current := s.runs[id]; current == nil || current.snapshot.Status != RunQueued {
		s.mu.Unlock()
		close(sink.channel())
		return
	}
	run.starting = true
	s.mu.Unlock()
	handle, err := s.executor.Start(ctx, plan, sink)
	if err != nil {
		close(sink.channel())
		<-consumerDone
		s.finish(id, RunResult{Status: RunStartFailed, ExitCode: -1, Err: err})
		return
	}
	s.mu.Lock()
	stopRequested := false
	if current := s.runs[id]; current != nil && current.snapshot.Status == RunQueued {
		current.handle = handle
		current.starting = false
		stopRequested = current.stopRequested
		current.snapshot.Status = RunRunning
		current.snapshot.StartedAt = time.Now().UTC()
		current.snapshot.EffectiveUID = plan.Identity.UID
		current.snapshot.EffectiveGID = plan.Identity.GID
		s.publishLocked(current.snapshot)
	}
	s.mu.Unlock()
	if stopRequested {
		_ = handle.Stop(context.Background(), 0)
	}
	result := handle.Wait()
	<-consumerDone
	s.finish(id, result)
}

func (s *RunService) queued(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	run := s.runs[id]
	return run != nil && run.snapshot.Status == RunQueued
}

func (s *RunService) finish(id string, result RunResult) {
	s.mu.Lock()
	run := s.runs[id]
	finished := s.finishLocked(run, result)
	var snapshot RunSnapshot
	var logs []byte
	var job Job
	sequence := uint64(0)
	if finished {
		snapshot, logs, job = run.snapshot, run.log.Bytes(), cloneJob(run.job)
		sequence = s.nextHistoryLocked()
	}
	s.mu.Unlock()
	if finished {
		s.persistHistory(sequence, snapshot, logs)
		if s.terminalHook != nil {
			s.terminalHook(job, snapshot)
		}
	}
}

func (s *RunService) finishLocked(run *runState, result RunResult) bool {
	if run == nil || isTerminalRunStatus(run.snapshot.Status) {
		return false
	}
	run.cancelOnce.Do(run.cancel)
	run.snapshot.Status = result.Status
	if run.snapshot.Status == "" {
		run.snapshot.Status = RunFailed
	}
	run.snapshot.ExitCode = result.ExitCode
	run.snapshot.KillEscalated = result.KillEscalated
	run.snapshot.DroppedLogLines = result.DroppedLogLines
	if result.LogError != nil {
		run.snapshot.LogError = result.LogError.Error()
	}
	if result.ControlError != nil {
		run.snapshot.ControlError = result.ControlError.Error()
	}
	run.snapshot.FinishedAt = time.Now().UTC()
	if result.Err != nil {
		run.snapshot.Error = result.Err.Error()
	}
	if s.active[run.snapshot.JobID] == run.snapshot.ID {
		delete(s.active, run.snapshot.JobID)
	}
	if s.pending[run.snapshot.JobID] == run.snapshot.ID {
		delete(s.pending, run.snapshot.JobID)
	}
	s.publishLocked(run.snapshot)
	s.trimLocked()
	if next := s.pending[run.snapshot.JobID]; next != "" {
		delete(s.pending, run.snapshot.JobID)
		s.active[run.snapshot.JobID] = next
		go s.execute(s.runs[next].ctx, next)
	}
	return true
}

func appendRunHistory(path string, owner FileOwner, run RunSnapshot, logs []byte) error {
	duration := 0
	if !run.StartedAt.IsZero() && !run.FinishedAt.IsZero() {
		duration = int(run.FinishedAt.Sub(run.StartedAt).Seconds())
	}
	note := run.Error
	if note == "" {
		note = errNote(strings.Split(string(logs), "\n"))
	}
	return appendHistoryAtomicAt(path, owner, run.JobID, RunRecord{TS: run.FinishedAt.Format(time.RFC3339), Status: string(run.Status), Dur: duration, Note: note})
}

func isTerminalRunStatus(status RunStatus) bool {
	switch status {
	case RunSucceeded, RunFailed, RunKilled, RunTimedOut, RunStartFailed, RunSkippedOverlap:
		return true
	}
	return false
}

func (s *RunService) Stop(ctx context.Context, runID string) error {
	if ctx == nil {
		return errors.New("stop context is nil")
	}
	s.mu.Lock()
	run := s.runs[runID]
	if run == nil {
		s.mu.Unlock()
		return ErrRunNotFound
	}
	handle := run.handle
	status := run.snapshot.Status
	if status == RunQueued {
		if run.starting {
			run.stopRequested = true
			run.cancelOnce.Do(run.cancel)
			s.mu.Unlock()
			return nil
		}
		finished := s.finishLocked(run, RunResult{Status: RunKilled, ExitCode: -1})
		var snapshot RunSnapshot
		var logs []byte
		sequence := uint64(0)
		if finished {
			snapshot, logs = run.snapshot, run.log.Bytes()
			sequence = s.nextHistoryLocked()
		}
		s.mu.Unlock()
		if finished {
			s.persistHistory(sequence, snapshot, logs)
		}
		return nil
	}
	s.mu.Unlock()
	if status != RunRunning || handle == nil {
		return ErrRunNotActive
	}
	return handle.Stop(ctx, 0)
}

func (s *RunService) persistHistory(sequence uint64, snapshot RunSnapshot, logs []byte) {
	s.historyMu.Lock()
	for sequence != s.historyNext {
		s.historyCond.Wait()
	}
	defer func() {
		s.historyNext++
		s.historyCond.Broadcast()
		s.historyMu.Unlock()
	}()
	err := s.historyAppend(sequence, snapshot, logs)
	if err != nil {
		s.mu.Lock()
		if current := s.runs[snapshot.ID]; current != nil && current.snapshot.ID == snapshot.ID {
			current.snapshot.HistoryError = err.Error()
			s.publishLocked(current.snapshot)
		}
		s.mu.Unlock()
	}
}

func (s *RunService) nextHistoryLocked() uint64 { s.historySequence++; return s.historySequence }

func (s *RunService) publishLog(id string, line []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if run := s.runs[id]; run != nil && !isTerminalRunStatus(run.snapshot.Status) {
		s.eventID++
		event := RunEvent{ID: s.eventID, Kind: RunEventLog, Run: run.snapshot, Log: append([]byte(nil), line...)}
		s.events = append(s.events, cloneRunEvent(event))
		maxEvents := s.limits.MaxRuns * 8
		if len(s.events) > maxEvents {
			s.events = s.events[len(s.events)-maxEvents:]
		}
		for _, subscriber := range s.subscribers {
			select {
			case subscriber <- cloneRunEvent(event):
			default:
			}
		}
	}
}

func (s *RunService) Get(id string) (RunSnapshot, bool) {
	s.prune()
	s.mu.RLock()
	defer s.mu.RUnlock()
	run, ok := s.runs[id]
	if !ok {
		return RunSnapshot{}, false
	}
	return run.snapshot, true
}

func (s *RunService) List(filter RunFilter) []RunSnapshot {
	s.prune()
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]RunSnapshot, 0, len(s.runs))
	for _, run := range s.runs {
		if filter.JobID != 0 && filter.JobID != run.snapshot.JobID || filter.Status != "" && filter.Status != run.snapshot.Status {
			continue
		}
		result = append(result, run.snapshot)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].RequestedAt.After(result[j].RequestedAt) })
	if filter.Limit > 0 && len(result) > filter.Limit {
		result = result[:filter.Limit]
	}
	return result
}

func (s *RunService) Logs(id string) ([]byte, bool) {
	s.prune()
	s.mu.RLock()
	run, ok := s.runs[id]
	s.mu.RUnlock()
	if !ok {
		return nil, false
	}
	return run.log.Bytes(), true
}

func (s *RunService) prune() { s.mu.Lock(); s.trimLocked(); s.mu.Unlock() }

func (s *RunService) Subscribe(afterEventID uint64) RunSubscription {
	s.mu.Lock()
	replay := make([]RunEvent, 0, len(s.events))
	for _, event := range s.events {
		if event.ID > afterEventID {
			replay = append(replay, cloneRunEvent(event))
		}
	}
	channel := make(chan RunEvent, len(replay)+64)
	for _, event := range replay {
		channel <- cloneRunEvent(event)
	}
	s.nextSubID++
	id := s.nextSubID
	s.subscribers[id] = channel
	s.mu.Unlock()
	var once sync.Once
	return RunSubscription{Events: channel, Close: func() {
		once.Do(func() {
			s.mu.Lock()
			if subscriber, ok := s.subscribers[id]; ok {
				delete(s.subscribers, id)
				close(subscriber)
			}
			s.mu.Unlock()
		})
	}}
}

func (s *RunService) publishLocked(snapshot RunSnapshot) {
	s.eventID++
	event := RunEvent{ID: s.eventID, Kind: RunEventState, Run: snapshot}
	s.events = append(s.events, event)
	maxEvents := s.limits.MaxRuns * 8
	if len(s.events) > maxEvents {
		s.events = s.events[len(s.events)-maxEvents:]
	}
	for _, subscriber := range s.subscribers {
		select {
		case subscriber <- event:
		default:
		}
	}
}

func cloneRunEvent(event RunEvent) RunEvent {
	event.Log = append([]byte(nil), event.Log...)
	return event
}

func (s *RunService) trimLocked() {
	cutoff := time.Now().Add(-s.limits.MaxAge)
	for id, run := range s.runs {
		run.log.TrimBefore(cutoff)
		if isTerminalRunStatus(run.snapshot.Status) && run.snapshot.FinishedAt.Before(cutoff) {
			delete(s.runs, id)
		}
	}
	terminal := make([]*runState, 0)
	for _, run := range s.runs {
		if isTerminalRunStatus(run.snapshot.Status) {
			terminal = append(terminal, run)
		}
	}
	sort.Slice(terminal, func(i, j int) bool { return terminal[i].snapshot.FinishedAt.Before(terminal[j].snapshot.FinishedAt) })
	for len(terminal) > s.limits.MaxRuns {
		delete(s.runs, terminal[0].snapshot.ID)
		terminal = terminal[1:]
	}
}

func newRunID() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("generate run ID: %w", err)
	}
	return hex.EncodeToString(bytes), nil
}
