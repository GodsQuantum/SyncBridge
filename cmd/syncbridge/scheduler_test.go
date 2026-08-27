package main

import (
	"context"
	"errors"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"
)

type recordingStarter struct {
	mu    sync.Mutex
	calls []StartRunInput
}

func (s *recordingStarter) Start(_ context.Context, input StartRunInput) (RunSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, input)
	return RunSnapshot{JobID: input.JobID, Revision: input.Revision}, nil
}

func (s *recordingStarter) count(id int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, input := range s.calls {
		if input.JobID == id {
			count++
		}
	}
	return count
}

type fakeCron struct {
	mu      sync.Mutex
	next    int
	entries map[int]func()
	specs   []string
	onAdd   func()
}

func (c *fakeCron) AddFunc(spec string, fn func()) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.next++
	c.specs = append(c.specs, spec)
	if c.entries == nil {
		c.entries = make(map[int]func())
	}
	c.entries[c.next] = fn
	id := c.next
	onAdd := c.onAdd
	c.mu.Unlock()
	if onAdd != nil {
		onAdd()
	}
	c.mu.Lock()
	return id, nil
}

func (c *fakeCron) callback(id int) func() {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.entries[id]
}

type blockingStarter struct {
	entered chan struct{}
	release chan struct{}
}

func (s *blockingStarter) Start(context.Context, StartRunInput) (RunSnapshot, error) {
	s.entered <- struct{}{}
	<-s.release
	return RunSnapshot{}, nil
}

func (c *fakeCron) Remove(id int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, id)
}

func (c *fakeCron) Fire(id int) {
	c.mu.Lock()
	fn := c.entries[id]
	c.mu.Unlock()
	if fn != nil {
		fn()
	}
}

func TestSchedulerFiresExactlyOnceWithImmutableRequest(t *testing.T) {
	starter := &recordingStarter{}
	clock := &fakeCron{}
	scheduler := newSchedulerForTest(starter, clock)
	job := cronJob(7, 4, "* * * * *")
	if err := scheduler.Reconcile([]Job{job}); err != nil {
		t.Fatal(err)
	}
	job.Revision = 99
	clock.Fire(1)
	if got := starter.count(7); got != 1 {
		t.Fatalf("Start calls = %d, want 1", got)
	}
	starter.mu.Lock()
	got := starter.calls[0]
	starter.mu.Unlock()
	if got.Revision != 4 || got.Origin != RunOriginCron {
		t.Fatalf("Start input = %#v", got)
	}
}

func TestSchedulerKeepsExistingEntryForInvalidUpdate(t *testing.T) {
	starter := &recordingStarter{}
	clock := &fakeCron{}
	scheduler := newSchedulerForTest(starter, clock)
	if err := scheduler.Reconcile([]Job{cronJob(7, 1, "* * * * *")}); err != nil {
		t.Fatal(err)
	}
	if err := scheduler.Reconcile([]Job{cronJob(7, 2, "bad")}); err == nil {
		t.Fatal("invalid cron was accepted")
	}
	clock.Fire(1)
	if got := starter.count(7); got != 1 {
		t.Fatalf("existing schedule was destroyed: %d calls", got)
	}
}

func TestSchedulerDoesNotRetryOverlap(t *testing.T) {
	starter := &overlapStarter{}
	clock := &fakeCron{}
	scheduler := newSchedulerForTest(starter, clock)
	if err := scheduler.Reconcile([]Job{cronJob(7, 1, "* * * * *")}); err != nil {
		t.Fatal(err)
	}
	clock.Fire(1)
	if starter.calls != 1 {
		t.Fatalf("Start calls = %d, want 1", starter.calls)
	}
}

func TestSchedulerCloseBlocksCapturedCallback(t *testing.T) {
	starter := &recordingStarter{}
	clock := &fakeCron{}
	scheduler := newSchedulerForTest(starter, clock)
	if err := scheduler.Reconcile([]Job{cronJob(7, 1, "* * * * *")}); err != nil {
		t.Fatal(err)
	}
	scheduler.Close()
	scheduler.Close()
	clock.Fire(1)
	if got := starter.count(7); got != 0 {
		t.Fatalf("Start calls after Close = %d", got)
	}
}

func TestSchedulerReconcileIsIdempotent(t *testing.T) {
	starter := &recordingStarter{}
	clock := &fakeCron{}
	scheduler := newSchedulerForTest(starter, clock)
	job := cronJob(7, 1, "* * * * *")
	if err := scheduler.Reconcile([]Job{job}); err != nil {
		t.Fatal(err)
	}
	if err := scheduler.Reconcile([]Job{job}); err != nil {
		t.Fatal(err)
	}
	clock.Fire(1)
	if got := starter.count(7); got != 1 {
		t.Fatalf("Start calls = %d, want 1", got)
	}
}

func TestSchedulerNormalizesFiveAndSixFieldSpecs(t *testing.T) {
	clock := &fakeCron{}
	scheduler := newSchedulerForTest(&recordingStarter{}, clock)
	if err := scheduler.Reconcile([]Job{cronJob(7, 1, "* * * * *"), cronJob(8, 1, "5 * * * * *")}); err != nil {
		t.Fatal(err)
	}
	clock.mu.Lock()
	got := append([]string(nil), clock.specs...)
	clock.mu.Unlock()
	sort.Strings(got)
	if !slices.Equal(got, []string{"0 * * * * *", "5 * * * * *"}) {
		t.Fatalf("normalized specs = %q", got)
	}
}

func TestSchedulerReconcileDoesNotWaitForStart(t *testing.T) {
	starter := &blockingStarter{entered: make(chan struct{}, 1), release: make(chan struct{})}
	clock := &fakeCron{}
	scheduler := newSchedulerForTest(starter, clock)
	if err := scheduler.Reconcile([]Job{cronJob(7, 1, "* * * * *")}); err != nil {
		t.Fatal(err)
	}
	go clock.Fire(1)
	<-starter.entered
	done := make(chan error, 1)
	go func() { done <- scheduler.Reconcile([]Job{cronJob(7, 2, "* * * * *")}) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		close(starter.release)
		t.Fatal("Reconcile waited for Start")
	}
	close(starter.release)
}

func TestSchedulerCloseWaitsForClaimWithoutStateLock(t *testing.T) {
	starter := &blockingStarter{entered: make(chan struct{}, 1), release: make(chan struct{})}
	clock := &fakeCron{}
	scheduler := newSchedulerForTest(starter, clock)
	if err := scheduler.Reconcile([]Job{cronJob(7, 1, "* * * * *")}); err != nil {
		t.Fatal(err)
	}
	go clock.Fire(1)
	<-starter.entered
	closed := make(chan struct{})
	go func() { scheduler.Close(); close(closed) }()
	returned := make(chan struct{})
	go func() { _ = scheduler.LastError(7); close(returned) }()
	select {
	case <-returned:
	case <-time.After(time.Second):
		close(starter.release)
		t.Fatal("Close held scheduler state while waiting")
	}
	close(starter.release)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not finish after Start")
	}
}

func TestSchedulerUpdateRejectsCapturedSameRevisionCallback(t *testing.T) {
	starter := &recordingStarter{}
	clock := &fakeCron{}
	scheduler := newSchedulerForTest(starter, clock)
	if err := scheduler.Reconcile([]Job{cronJob(7, 1, "* * * * *")}); err != nil {
		t.Fatal(err)
	}
	stale := clock.callback(1)
	if err := scheduler.Reconcile([]Job{cronJob(7, 1, "5 * * * * *")}); err != nil {
		t.Fatal(err)
	}
	stale()
	if got := starter.count(7); got != 0 {
		t.Fatalf("stale callback started %d runs", got)
	}
}

type overlapStarter struct{ calls int }

func (s *overlapStarter) Start(context.Context, StartRunInput) (RunSnapshot, error) {
	s.calls++
	return RunSnapshot{Status: RunSkippedOverlap}, errors.New("job already has an active run")
}

func cronJob(id int, revision uint64, spec string) Job {
	return Job{ID: id, Revision: revision, Enabled: true, Trigger: TriggerCron, Cron: spec, Scheduler: SchedulerPolicy{Owner: SchedulerSyncBridge}}
}

func TestSchedulerIgnoresJobsNeedingReview(t *testing.T) {
	clock := &fakeCron{}
	scheduler := newSchedulerForTest(&recordingStarter{}, clock)
	job := cronJob(7, 1, "* * * * *")
	job.NeedsReview = true
	if err := scheduler.Reconcile([]Job{job}); err != nil {
		t.Fatal(err)
	}
	clock.mu.Lock()
	defer clock.mu.Unlock()
	if len(clock.entries) != 0 {
		t.Fatalf("scheduled needs-review job: %#v", clock.entries)
	}
}
