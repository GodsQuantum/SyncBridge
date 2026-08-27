package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestRunServiceReservesBeforeReturning(t *testing.T) {
	svc, executor := newBlockedRunService(t, "skip")
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := svc.Start(context.Background(), StartRunInput{JobID: 1, Revision: 1, Origin: RunOriginManual})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	waitForStarts(t, executor, 1)
	if got := executor.Starts(); got != 1 {
		t.Fatalf("executor starts = %d, want 1", got)
	}
	if got := countErrors(errs, ErrOverlap); got != 1 {
		t.Fatalf("overlap errors = %d, want 1", got)
	}
}

func TestRunServiceSkipReturnsPersistedTerminalSnapshot(t *testing.T) {
	svc, executor := newBlockedRunService(t, "skip")
	_, err := svc.Start(context.Background(), StartRunInput{JobID: 1, Revision: 1, Origin: RunOriginManual})
	if err != nil {
		t.Fatal(err)
	}
	waitForStarts(t, executor, 1)
	skipped, err := svc.Start(context.Background(), StartRunInput{JobID: 1, Revision: 1, Origin: RunOriginCron})
	if !errors.Is(err, ErrOverlap) {
		t.Fatalf("Start error = %v", err)
	}
	if skipped.Status != RunSkippedOverlap || skipped.ID == "" {
		t.Fatalf("skipped = %#v", skipped)
	}
	if got, ok := svc.Get(skipped.ID); !ok || got.Status != RunSkippedOverlap {
		t.Fatalf("Get = %#v, %v", got, ok)
	}
}

func TestRunServiceHistoryPersistenceFollowsTerminalSequence(t *testing.T) {
	svc, _ := newBlockedRunService(t, "skip")
	first := make(chan struct{})
	releaseFirst := make(chan struct{})
	var mu sync.Mutex
	var order []uint64
	svc.historyAppend = func(sequence uint64, _ RunSnapshot, _ []byte) error {
		if sequence == 1 {
			close(first)
			<-releaseFirst
		}
		mu.Lock()
		order = append(order, sequence)
		mu.Unlock()
		return nil
	}
	done1 := make(chan struct{})
	go func() { defer close(done1); svc.persistHistory(1, RunSnapshot{}, nil) }()
	<-first
	done2 := make(chan struct{})
	go func() { defer close(done2); svc.persistHistory(2, RunSnapshot{}, nil) }()
	select {
	case <-done2:
		t.Fatal("sequence 2 persisted before sequence 1")
	default:
	}
	close(releaseFirst)
	<-done1
	<-done2
	mu.Lock()
	defer mu.Unlock()
	if got, want := order, []uint64{1, 2}; !equalUint64s(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestRunServiceHistorySequenceAdvancesAfterError(t *testing.T) {
	svc, _ := newBlockedRunService(t, "skip")
	var mu sync.Mutex
	var order []uint64
	svc.historyAppend = func(sequence uint64, _ RunSnapshot, _ []byte) error {
		mu.Lock()
		order = append(order, sequence)
		mu.Unlock()
		if sequence == 1 {
			return errors.New("disk")
		}
		return nil
	}
	svc.persistHistory(1, RunSnapshot{}, nil)
	svc.persistHistory(2, RunSnapshot{}, nil)
	mu.Lock()
	defer mu.Unlock()
	if got, want := order, []uint64{1, 2}; !equalUint64s(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestRunServiceHistorySequenceAdvancesAfterPanic(t *testing.T) {
	svc, _ := newBlockedRunService(t, "skip")
	panicValue := errors.New("history panic")
	order := make(chan uint64, 2)
	svc.historyAppend = func(sequence uint64, _ RunSnapshot, _ []byte) error {
		order <- sequence
		if sequence == 1 {
			panic(panicValue)
		}
		return nil
	}

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		svc.persistHistory(1, RunSnapshot{}, nil)
	}()
	if recovered != panicValue {
		t.Fatalf("recovered panic = %v, want %v", recovered, panicValue)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.persistHistory(2, RunSnapshot{}, nil)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("sequence 2 blocked after sequence 1 panic")
	}

	if first, second := <-order, <-order; first != 1 || second != 2 {
		t.Fatalf("order = [%d %d], want [1 2]", first, second)
	}
	svc.historyMu.Lock()
	defer svc.historyMu.Unlock()
	if got := svc.historyNext; got != 3 {
		t.Fatalf("historyNext = %d, want 3", got)
	}
}

func equalUint64s(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRunServiceStopDuringExecutorStartWaitsForAttachedHandle(t *testing.T) {
	repo := newTestRepository(t)
	if _, err := repo.Create(Job{Name: "run", Action: Action{Type: ActionCommand, Command: "true"}}); err != nil {
		t.Fatal(err)
	}
	executor := newStartingExecutor()
	svc := NewRunService(repo, testCompiler{}, testInstaller{}, executor, testLimits())
	run, err := svc.Start(context.Background(), StartRunInput{JobID: 1, Revision: 1, Origin: RunOriginManual})
	if err != nil {
		t.Fatal(err)
	}
	<-executor.entered
	if err := svc.Stop(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	close(executor.release)
	waitForRunStatus(t, svc, run.ID, RunKilled)
	if got := executor.StopCalls(); got != 1 {
		t.Fatalf("Stop calls = %d, want 1", got)
	}
}

func TestRunServiceAcceptedRunOutlivesRequestContext(t *testing.T) {
	svc, executor := newBlockedRunService(t, "skip")
	ctx, cancel := context.WithCancel(context.Background())
	run, err := svc.Start(ctx, StartRunInput{JobID: 1, Revision: 1, Origin: RunOriginManual})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	waitForStarts(t, executor, 1)
	if got, _ := svc.Get(run.ID); got.Status != RunRunning {
		t.Fatalf("run = %#v", got)
	}
}

func TestRunServiceReplaysAllRetainedEvents(t *testing.T) {
	svc, executor := newBlockedRunService(t, "skip")
	for index := range 25 {
		run, err := svc.Start(context.Background(), StartRunInput{JobID: 1, Revision: 1, Origin: RunOriginManual})
		if err != nil {
			t.Fatal(err)
		}
		waitForStarts(t, executor, index+1)
		executor.finish(RunResult{Status: RunSucceeded, ExitCode: 0})
		waitForRunStatus(t, svc, run.ID, RunSucceeded)
	}
	subscription := svc.Subscribe(0)
	defer subscription.Close()
	count := 0
	for range svc.events {
		select {
		case <-subscription.Events:
			count++
		default:
			t.Fatal("replay dropped")
		}
	}
	if count != len(svc.events) {
		t.Fatalf("replay = %d, retained = %d", count, len(svc.events))
	}
}

func TestRunServiceCompilesInstallsThenStarts(t *testing.T) {
	svc, executor := newBlockedRunService(t, "skip")
	run, err := svc.Start(context.Background(), StartRunInput{JobID: 1, Revision: 1, Origin: RunOriginManual})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != RunQueued || run.ID == "" {
		t.Fatalf("reserved run = %#v", run)
	}
	waitForStarts(t, executor, 1)
	if got := executor.Starts(); got != 1 {
		t.Fatalf("executor starts = %d, want 1", got)
	}
}

func TestRunServiceInstallFailureIsTerminalStartFailure(t *testing.T) {
	svc, _ := newRunServiceForTest(t, "skip", errors.New("install failed"))
	run, err := svc.Start(context.Background(), StartRunInput{JobID: 1, Revision: 1, Origin: RunOriginManual})
	if err != nil {
		t.Fatal(err)
	}
	waitForRunStatus(t, svc, run.ID, RunStartFailed)
}

func TestRunServiceExecutorStartFailureIsTerminalStartFailure(t *testing.T) {
	repo := newTestRepository(t)
	if _, err := repo.Create(Job{Name: "run", Action: Action{Type: ActionCommand, Command: "true"}, Execution: ExecutionPolicy{Overlap: "skip"}}); err != nil {
		t.Fatal(err)
	}
	svc := NewRunService(repo, testCompiler{}, testInstaller{}, failingExecutor{}, LogLimits{MaxBytes: 64, MaxLineBytes: 16, MaxRuns: 8, MaxAge: time.Hour})
	run, err := svc.Start(context.Background(), StartRunInput{JobID: 1, Revision: 1, Origin: RunOriginManual})
	if err != nil {
		t.Fatal(err)
	}
	waitForRunStatus(t, svc, run.ID, RunStartFailed)
}

func TestRunServiceQueueLatestKeepsOnlyNewestPendingRequest(t *testing.T) {
	svc, executor := newBlockedRunService(t, "queue-latest")
	first, err := svc.Start(context.Background(), StartRunInput{JobID: 1, Revision: 1, Origin: RunOriginManual})
	if err != nil {
		t.Fatal(err)
	}
	waitForStarts(t, executor, 1)
	second, err := svc.Start(context.Background(), StartRunInput{JobID: 1, Revision: 1, Origin: RunOriginCron})
	if err != nil {
		t.Fatal(err)
	}
	third, err := svc.Start(context.Background(), StartRunInput{JobID: 1, Revision: 1, Origin: RunOriginWatch})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID || second.ID == third.ID {
		t.Fatalf("queue IDs are not distinct: %q %q %q", first.ID, second.ID, third.ID)
	}
	waitForRunStatus(t, svc, second.ID, RunSkippedOverlap)
	executor.finish(RunResult{Status: RunSucceeded, ExitCode: 0})
	waitForRunStatus(t, svc, third.ID, RunRunning)
	waitForStarts(t, executor, 2)
	if got := executor.Starts(); got != 2 {
		t.Fatalf("executor starts = %d, want 2", got)
	}
}

func TestRunServiceRejectsDryRunForNonSyncBeforeExecutor(t *testing.T) {
	svc, executor := newBlockedRunService(t, "skip")
	run, err := svc.Start(context.Background(), StartRunInput{JobID: 1, Revision: 1, Origin: RunOriginManual, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	waitForRunStatus(t, svc, run.ID, RunStartFailed)
	if got := executor.Starts(); got != 0 {
		t.Fatalf("executor starts = %d, want 0", got)
	}
}

func TestRunServicePersistsTerminalHistoryAtomically(t *testing.T) {
	previousDataDir := dataDir
	previousUID, previousGID := fileUID, fileGID
	dataDir = t.TempDir()
	fileUID, fileGID = os.Getuid(), os.Getgid()
	t.Cleanup(func() {
		dataDir = previousDataDir
		fileUID, fileGID = previousUID, previousGID
	})
	svc, executor := newBlockedRunService(t, "skip")
	run, err := svc.Start(context.Background(), StartRunInput{JobID: 1, Revision: 1, Origin: RunOriginManual})
	if err != nil {
		t.Fatal(err)
	}
	waitForStarts(t, executor, 1)
	executor.finish(RunResult{Status: RunSucceeded, ExitCode: 0})
	waitForRunStatus(t, svc, run.ID, RunSucceeded)
	deadline := time.Now().Add(time.Second)
	for {
		got := loadHistory()["1"]
		if len(got) == 1 && got[0].Status == string(RunSucceeded) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("history = %#v", got)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRunServiceQueuedStopPreventsExecutorStart(t *testing.T) {
	repo := newTestRepository(t)
	if _, err := repo.Create(Job{Name: "run", Action: Action{Type: ActionCommand, Command: "true"}}); err != nil {
		t.Fatal(err)
	}
	compiler := blockingCompiler{entered: make(chan struct{}), release: make(chan struct{})}
	executor := &blockedExecutor{}
	svc := NewRunService(repo, compiler, testInstaller{}, executor, LogLimits{MaxBytes: 64, MaxLineBytes: 16, MaxRuns: 8, MaxAge: time.Hour})
	run, err := svc.Start(context.Background(), StartRunInput{JobID: 1, Revision: 1, Origin: RunOriginManual})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-compiler.entered:
	case <-time.After(time.Second):
		t.Fatal("compile did not start")
	}
	if err := svc.Stop(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	close(compiler.release)
	waitForRunStatus(t, svc, run.ID, RunKilled)
	if got := executor.Starts(); got != 0 {
		t.Fatalf("executor starts = %d, want 0", got)
	}
}

func TestRunServiceEventsAreMonotonicAndSlowSubscribersDoNotBlock(t *testing.T) {
	svc, executor := newBlockedRunService(t, "skip")
	subscription := svc.Subscribe(0)
	defer subscription.Close()
	run, err := svc.Start(context.Background(), StartRunInput{JobID: 1, Revision: 1, Origin: RunOriginManual})
	if err != nil {
		t.Fatal(err)
	}
	waitForStarts(t, executor, 1)
	executor.finish(RunResult{Status: RunSucceeded, ExitCode: 0})
	waitForRunStatus(t, svc, run.ID, RunSucceeded)
	var previous uint64
	for range 3 {
		select {
		case event := <-subscription.Events:
			if event.ID <= previous {
				t.Fatalf("event IDs = %d then %d", previous, event.ID)
			}
			previous = event.ID
		case <-time.After(time.Second):
			t.Fatal("missing run event")
		}
	}
}

func countErrors(errs <-chan error, want error) int {
	count := 0
	for err := range errs {
		if errors.Is(err, want) {
			count++
		}
	}
	return count
}

func waitForRunStatus(t *testing.T, svc *RunService, id string, want RunStatus) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if run, ok := svc.Get(id); ok && run.Status == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	run, _ := svc.Get(id)
	t.Fatalf("run %q = %#v, want status %q", id, run, want)
}

func waitForStarts(t *testing.T, executor *blockedExecutor, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if executor.Starts() >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("executor starts = %d, want at least %d", executor.Starts(), want)
}

func newBlockedRunService(t *testing.T, overlap string) (*RunService, *blockedExecutor) {
	t.Helper()
	svc, executor := newRunServiceForTest(t, overlap, nil)
	return svc, executor
}

func newRunServiceForTest(t *testing.T, overlap string, installErr error) (*RunService, *blockedExecutor) {
	t.Helper()
	repo := newTestRepository(t)
	job, err := repo.Create(Job{
		Name: "run", Action: Action{Type: ActionCommand, Command: "true"},
		Identity:  Identity{Mode: IdentityFixed, User: "user", UID: 1000, Group: "user", GID: 1000},
		Execution: ExecutionPolicy{Overlap: overlap},
	})
	if err != nil {
		t.Fatal(err)
	}
	if job.ID != 1 || job.Revision != 1 {
		t.Fatalf("job = %#v", job)
	}
	executor := &blockedExecutor{}
	return NewRunService(repo, testCompiler{}, testInstaller{err: installErr}, executor, testLimits()), executor
}

func testLimits() LogLimits {
	return LogLimits{MaxBytes: 1024, MaxLineBytes: 64, MaxRuns: 64, MaxAge: time.Hour}
}

type testCompiler struct{}

func (testCompiler) Compile(_ context.Context, job Job, request RunRequest) (ExecutionPlan, error) {
	if request.DryRun && job.Action.Type != ActionSync {
		return ExecutionPlan{}, errors.New("dry run is supported only for sync actions")
	}
	return ExecutionPlan{RunID: request.RunID, JobID: job.ID, Revision: job.Revision, WrapperPath: "/tmp/run.sh"}, nil
}

type blockingCompiler struct {
	entered chan struct{}
	release chan struct{}
}

func (c blockingCompiler) Compile(ctx context.Context, job Job, request RunRequest) (ExecutionPlan, error) {
	close(c.entered)
	select {
	case <-c.release:
	case <-ctx.Done():
		return ExecutionPlan{}, ctx.Err()
	}
	return testCompiler{}.Compile(ctx, job, request)
}

type testInstaller struct{ err error }

func (i testInstaller) Install(_ context.Context, plan ExecutionPlan) (string, error) {
	if i.err != nil {
		return "", i.err
	}
	return plan.WrapperPath, nil
}

type failingExecutor struct{}

func (failingExecutor) Start(context.Context, ExecutionPlan, LogSink) (ExecutionHandle, error) {
	return nil, errors.New("start failed")
}

type blockedExecutor struct {
	mu      sync.Mutex
	handles []*blockedHandle
	starts  int
}

func (e *blockedExecutor) Start(_ context.Context, _ ExecutionPlan, sink LogSink) (ExecutionHandle, error) {
	h := &blockedHandle{done: make(chan RunResult, 1), sink: sink}
	e.mu.Lock()
	e.handles = append(e.handles, h)
	e.starts++
	e.mu.Unlock()
	return h, nil
}

func (e *blockedExecutor) Starts() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.starts
}

func (e *blockedExecutor) finish(result RunResult) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.handles) == 0 {
		return
	}
	e.handles[0].finish(result)
	e.handles = e.handles[1:]
}

type blockedHandle struct {
	done chan RunResult
	sink LogSink
}

func (h *blockedHandle) PID() int        { return 1 }
func (h *blockedHandle) Wait() RunResult { return <-h.done }
func (h *blockedHandle) Stop(context.Context, time.Duration) error {
	h.finish(RunResult{Status: RunKilled, ExitCode: -1})
	return nil
}
func (h *blockedHandle) finish(result RunResult) {
	if lines := h.sink.channel(); lines != nil {
		close(lines)
	}
	h.done <- result
}

type startingExecutor struct {
	entered chan struct{}
	release chan struct{}
	mu      sync.Mutex
	stops   int
}

func newStartingExecutor() *startingExecutor {
	return &startingExecutor{entered: make(chan struct{}), release: make(chan struct{})}
}
func (e *startingExecutor) Start(_ context.Context, _ ExecutionPlan, sink LogSink) (ExecutionHandle, error) {
	close(e.entered)
	<-e.release
	return &startingHandle{executor: e, sink: sink, done: make(chan RunResult, 1)}, nil
}
func (e *startingExecutor) StopCalls() int { e.mu.Lock(); defer e.mu.Unlock(); return e.stops }

type startingHandle struct {
	executor *startingExecutor
	sink     LogSink
	done     chan RunResult
}

func (h *startingHandle) PID() int        { return 1 }
func (h *startingHandle) Wait() RunResult { return <-h.done }
func (h *startingHandle) Stop(context.Context, time.Duration) error {
	h.executor.mu.Lock()
	h.executor.stops++
	h.executor.mu.Unlock()
	close(h.sink.channel())
	h.done <- RunResult{Status: RunKilled, ExitCode: -1}
	return nil
}

func TestRunServiceInvokesTerminalHookWithReservedJobSnapshot(t *testing.T) {
	svc, executor := newBlockedRunService(t, "skip")
	called := make(chan struct {
		job Job
		run RunSnapshot
	}, 1)
	svc.terminalHook = func(job Job, run RunSnapshot) {
		called <- struct {
			job Job
			run RunSnapshot
		}{job: job, run: run}
	}
	run, err := svc.Start(context.Background(), StartRunInput{JobID: 1, Revision: 1, Origin: RunOriginManual})
	if err != nil {
		t.Fatal(err)
	}
	waitForStarts(t, executor, 1)
	executor.finish(RunResult{Status: RunSucceeded, ExitCode: 0})
	waitForRunStatus(t, svc, run.ID, RunSucceeded)
	select {
	case got := <-called:
		if got.job.ID != 1 || got.run.ID != run.ID || got.run.Status != RunSucceeded {
			t.Fatalf("terminal hook = %#v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("terminal hook was not invoked")
	}
}

func TestRotateBackupsHostKeepsNewestDirectories(t *testing.T) {
	oldRoot := hostRootView
	hostRootView = t.TempDir()
	t.Cleanup(func() { hostRootView = oldRoot })
	base := filepath.Join(hostRootView, "srv", "dest", ".sb-backup")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"20260825-010101", "20260826-010101", "20260827-010101"} {
		if err := os.Mkdir(filepath.Join(base, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := rotateBackupsHost("/srv/dest", 2); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	want := []string{"20260826-010101", "20260827-010101"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("backup dirs = %v, want %v", names, want)
	}
}
