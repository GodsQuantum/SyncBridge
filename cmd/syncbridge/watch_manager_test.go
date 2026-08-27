package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

type fakeWatcher struct {
	events chan fsnotify.Event
	errors chan error
	closed bool
	mu     sync.Mutex
	adds   []string
	addErr map[string]error
	added  chan string
}

func newFakeWatcher() *fakeWatcher {
	return &fakeWatcher{events: make(chan fsnotify.Event, 16), errors: make(chan error, 1)}
}

func (w *fakeWatcher) Events() <-chan fsnotify.Event { return w.events }
func (w *fakeWatcher) Errors() <-chan error          { return w.errors }
func (w *fakeWatcher) Add(path string) error {
	w.mu.Lock()
	err := w.addErr[path]
	if err == nil {
		w.adds = append(w.adds, path)
	}
	added := w.added
	w.mu.Unlock()
	if err == nil && added != nil {
		select {
		case added <- path:
		default:
		}
	}
	return err
}
func (w *fakeWatcher) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.closed {
		w.closed = true
		close(w.events)
		close(w.errors)
	}
	return nil
}

type fakeWatchClock struct {
	mu            sync.Mutex
	timers        []*fakeTimer
	added         chan struct{}
	ticks         chan time.Time
	tickerCreated chan struct{}
}

type fakeTimer struct {
	mu      sync.Mutex
	fn      func()
	stopped bool
}

func (c *fakeWatchClock) AfterFunc(_ time.Duration, fn func()) watchTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{fn: fn}
	c.timers = append(c.timers, t)
	if c.added != nil {
		c.added <- struct{}{}
	}
	return t
}

func (c *fakeWatchClock) FireAll() {
	c.mu.Lock()
	timers := append([]*fakeTimer(nil), c.timers...)
	c.mu.Unlock()
	for _, timer := range timers {
		timer.mu.Lock()
		stopped := timer.stopped
		timer.mu.Unlock()
		if !stopped {
			timer.fn()
		}
	}
}

func (t *fakeTimer) Stop() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	wasRunning := !t.stopped
	t.stopped = true
	return wasRunning
}

type fakeTicker struct{ ch chan time.Time }

func (t *fakeTicker) C() <-chan time.Time { return t.ch }
func (t *fakeTicker) Stop()               {}

func (c *fakeWatchClock) NewTicker(time.Duration) watchTicker {
	if c.tickerCreated != nil {
		c.tickerCreated <- struct{}{}
	}
	if c.ticks != nil {
		return &fakeTicker{ch: c.ticks}
	}
	return &fakeTicker{ch: make(chan time.Time)}
}

func TestWatchReconcileStopsRemovedWatcher(t *testing.T) {
	watcher := newFakeWatcher()
	watches := &fakeWatcherFactory{watcher: watcher}
	manager := newWatchManagerForTest(&recordingStarter{}, watches, &fakeWatchClock{}, func(string) error { return nil })
	if err := manager.Reconcile([]Job{watchJob(t, 3, 1)}); err != nil {
		t.Fatal(err)
	}
	if !manager.Active(3) {
		t.Fatal("watcher inactive")
	}
	if err := manager.Reconcile(nil); err != nil {
		t.Fatal(err)
	}
	if manager.Active(3) || !watcher.closed {
		t.Fatal("watcher still active")
	}
}

func TestWatchEventAndPollShareOneDebounce(t *testing.T) {
	starter := &recordingStarter{}
	watcher := newFakeWatcher()
	clock := &fakeWatchClock{added: make(chan struct{}, 2)}
	manager := newWatchManagerForTest(starter, &fakeWatcherFactory{watcher: watcher}, clock, func(string) error { return nil })
	t.Cleanup(manager.Close)
	job := watchJob(t, 3, 2)
	if err := manager.Reconcile([]Job{job}); err != nil {
		t.Fatal(err)
	}
	watcher.events <- fsnotify.Event{Name: job.Action.Sync.Source + "/file", Op: fsnotify.Write}
	select {
	case <-clock.added:
	case <-time.After(time.Second):
		t.Fatal("fsnotify event did not reach debounce")
	}
	manager.signal(3, 2, "poll")
	clock.FireAll()
	if got := starter.count(3); got != 1 {
		t.Fatalf("Start calls = %d, want 1", got)
	}
}

func TestWatchMissingSourceIsObservableWithoutStart(t *testing.T) {
	starter := &recordingStarter{}
	manager := newWatchManagerForTest(starter, &fakeWatcherFactory{watcher: newFakeWatcher()}, &fakeWatchClock{}, func(string) error { return os.ErrNotExist })
	if err := manager.Reconcile([]Job{watchJob(t, 3, 1)}); err == nil {
		t.Fatal("missing source was accepted")
	}
	if manager.LastError(3) == nil || starter.count(3) != 0 {
		t.Fatal("missing source was not observable without a run")
	}
}

func TestWatchErrorIsObservable(t *testing.T) {
	watcher := newFakeWatcher()
	starter := &recordingStarter{}
	clock := &fakeWatchClock{}
	manager := newWatchManagerForTest(starter, &fakeWatcherFactory{watcher: watcher}, clock, func(string) error { return nil })
	t.Cleanup(manager.Close)
	if err := manager.Reconcile([]Job{watchJob(t, 3, 1)}); err != nil {
		t.Fatal(err)
	}
	watcher.errors <- errors.New("mount disappeared")
	deadline := time.After(time.Second)
	for manager.LastError(3) == nil {
		select {
		case <-deadline:
			t.Fatal("watch error was not observable")
		default:
			runtime.Gosched()
		}
	}
	watcher.events <- fsnotify.Event{Name: "file", Op: fsnotify.Write}
	clock.FireAll()
	if got := starter.count(3); got != 0 {
		t.Fatalf("Start calls after fsnotify error = %d", got)
	}
}

func TestWatchRemovalBlocksQueuedCallback(t *testing.T) {
	starter := &recordingStarter{}
	clock := &fakeWatchClock{}
	manager := newWatchManagerForTest(starter, &fakeWatcherFactory{watcher: newFakeWatcher()}, clock, func(string) error { return nil })
	if err := manager.Reconcile([]Job{watchJob(t, 3, 1)}); err != nil {
		t.Fatal(err)
	}
	manager.signal(3, 1, "event")
	if err := manager.Reconcile(nil); err != nil {
		t.Fatal(err)
	}
	clock.FireAll()
	if got := starter.count(3); got != 0 {
		t.Fatalf("Start calls after removal = %d", got)
	}
}

func TestWatchReconcileIsIdempotent(t *testing.T) {
	factory := &fakeWatcherFactory{watcher: newFakeWatcher()}
	manager := newWatchManagerForTest(&recordingStarter{}, factory, &fakeWatchClock{}, func(string) error { return nil })
	t.Cleanup(manager.Close)
	job := watchJob(t, 3, 1)
	if err := manager.Reconcile([]Job{job}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Reconcile([]Job{job}); err != nil {
		t.Fatal(err)
	}
	if factory.news != 1 {
		t.Fatalf("watcher allocations = %d, want 1", factory.news)
	}
}

func TestWatchCloseBlocksQueuedCallback(t *testing.T) {
	starter := &recordingStarter{}
	watcher := newFakeWatcher()
	clock := &fakeWatchClock{}
	manager := newWatchManagerForTest(starter, &fakeWatcherFactory{watcher: watcher}, clock, func(string) error { return nil })
	if err := manager.Reconcile([]Job{watchJob(t, 3, 2)}); err != nil {
		t.Fatal(err)
	}
	manager.signal(3, 2, "event")
	manager.Close()
	manager.Close()
	clock.FireAll()
	if got := starter.count(3); got != 0 {
		t.Fatalf("Start calls after Close = %d", got)
	}
}

func TestWatchReconcileDoesNotWaitForStart(t *testing.T) {
	starter := &blockingStarter{entered: make(chan struct{}, 1), release: make(chan struct{})}
	clock := &fakeWatchClock{}
	manager := newWatchManagerForTest(starter, &fakeWatcherFactory{watcher: newFakeWatcher()}, clock, func(string) error { return nil })
	if err := manager.Reconcile([]Job{watchJob(t, 3, 1)}); err != nil {
		t.Fatal(err)
	}
	manager.signal(3, 1, "event")
	go clock.FireAll()
	<-starter.entered
	done := make(chan error, 1)
	go func() { done <- manager.Reconcile(nil) }()
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

func TestWatchCloseWaitsForClaimWithoutStateLock(t *testing.T) {
	starter := &blockingStarter{entered: make(chan struct{}, 1), release: make(chan struct{})}
	clock := &fakeWatchClock{}
	manager := newWatchManagerForTest(starter, &fakeWatcherFactory{watcher: newFakeWatcher()}, clock, func(string) error { return nil })
	if err := manager.Reconcile([]Job{watchJob(t, 3, 1)}); err != nil {
		t.Fatal(err)
	}
	manager.signal(3, 1, "event")
	go clock.FireAll()
	<-starter.entered
	closed := make(chan struct{})
	go func() { manager.Close(); close(closed) }()
	returned := make(chan struct{})
	go func() { _ = manager.LastError(3); close(returned) }()
	select {
	case <-returned:
	case <-time.After(time.Second):
		close(starter.release)
		t.Fatal("Close held watch state while waiting")
	}
	close(starter.release)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not finish after Start")
	}
}

func TestWatchEventOnlyRearmsRenamedRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	starter := &recordingStarter{}
	watcher := newFakeWatcher()
	clock := &fakeWatchClock{added: make(chan struct{}, 1)}
	manager := newWatchManagerForTest(starter, &fakeWatcherFactory{watcher: watcher}, clock, watchSourceExists)
	t.Cleanup(manager.Close)
	job := watchJobAt(t, 3, 1, root, "event")
	if err := manager.Reconcile([]Job{job}); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(root, root+"-old"); err != nil {
		t.Fatal(err)
	}
	watcher.events <- fsnotify.Event{Name: root, Op: fsnotify.Rename}
	waitFor(t, func() bool { return manager.LastError(3) != nil })
	if err := os.MkdirAll(filepath.Join(root, "new", "tree"), 0755); err != nil {
		t.Fatal(err)
	}
	watcher.events <- fsnotify.Event{Name: root, Op: fsnotify.Create}
	waitFor(t, func() bool { return watcher.hasAdd(filepath.Join(root, "new", "tree")) })
	file := filepath.Join(root, "new", "tree", "file")
	if err := os.WriteFile(file, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	watcher.events <- fsnotify.Event{Name: file, Op: fsnotify.Write}
	select {
	case <-clock.added:
	case <-time.After(time.Second):
		t.Fatal("rearmed watcher did not debounce event")
	}
	clock.FireAll()
	if got := starter.count(3); got != 1 {
		t.Fatalf("Start calls after root rearm = %d", got)
	}
}

func TestWatchIgnoresSiblingEventsFromParentWatch(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	clock := &fakeWatchClock{}
	starter := &recordingStarter{}
	watcher := newFakeWatcher()
	manager := newWatchManagerForTest(starter, &fakeWatcherFactory{watcher: watcher}, clock, watchSourceExists)
	t.Cleanup(manager.Close)
	if err := manager.Reconcile([]Job{watchJobAt(t, 3, 1, root, "event")}); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(parent, "sibling")
	if err := os.Mkdir(sibling, 0755); err != nil {
		t.Fatal(err)
	}
	watcher.events <- fsnotify.Event{Name: sibling, Op: fsnotify.Create}
	watcher.events <- fsnotify.Event{Name: filepath.Join(sibling, "file"), Op: fsnotify.Write}
	waitFor(t, func() bool { return len(watcher.adds) >= 2 })
	clock.FireAll()
	if got := starter.count(3); got != 0 {
		t.Fatalf("sibling event started %d runs", got)
	}
}

func TestWatchAddErrorDegradesAndBlocksStart(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "new")
	watcher := newFakeWatcher()
	watcher.addErr = map[string]error{sub: errors.New("inotify limit")}
	starter := &recordingStarter{}
	clock := &fakeWatchClock{}
	manager := newWatchManagerForTest(starter, &fakeWatcherFactory{watcher: watcher}, clock, watchSourceExists)
	t.Cleanup(manager.Close)
	if err := manager.Reconcile([]Job{watchJobAt(t, 3, 1, root, "event")}); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(sub, 0755); err != nil {
		t.Fatal(err)
	}
	watcher.events <- fsnotify.Event{Name: sub, Op: fsnotify.Create}
	waitFor(t, func() bool { return manager.LastError(3) != nil })
	watcher.events <- fsnotify.Event{Name: filepath.Join(root, "file"), Op: fsnotify.Write}
	clock.FireAll()
	if got := starter.count(3); got != 0 {
		t.Fatalf("Start calls while degraded = %d", got)
	}
}

func TestWatchPollSignatureDetectsSameSizeChange(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("aa"), 0644); err != nil {
		t.Fatal(err)
	}
	first, err := directorySignature(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(file, time.Unix(100, 1), time.Unix(100, 1)); err != nil {
		t.Fatal(err)
	}
	second, err := directorySignature(context.Background(), root, nil)
	if err != nil || first == second {
		t.Fatalf("signature did not detect same-size change: %q %q %v", first, second, err)
	}
	if _, err := directorySignature(context.Background(), filepath.Join(root, "missing"), nil); err == nil {
		t.Fatal("scan error was hidden")
	}
}

func TestWatchSignatureUsesTheSameGlobAsEvents(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "clip.mp4"), []byte("a"), 0644); err != nil {
		t.Fatal(err)
	}
	first, err := directorySignature(context.Background(), root, []string{"*.mp4"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ignored.md"), []byte("a"), 0644); err != nil {
		t.Fatal(err)
	}
	second, err := directorySignature(context.Background(), root, []string{"*.mp4"})
	if err != nil || second != first {
		t.Fatalf("ignored file changed signature: %q %q %v", first, second, err)
	}
	if err := os.WriteFile(filepath.Join(root, "next.mp4"), []byte("a"), 0644); err != nil {
		t.Fatal(err)
	}
	third, err := directorySignature(context.Background(), root, []string{"*.mp4"})
	if err != nil || third == second {
		t.Fatalf("matching file did not change signature: %q %q %v", second, third, err)
	}
}

func TestWatchSignatureHonorsLimitAndCancellation(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a"), []byte("a"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := directorySignatureWithLimit(context.Background(), root, nil, 1); err == nil {
		t.Fatal("scan limit was ignored")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := directorySignatureWithLimit(ctx, root, nil, 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled scan error = %v", err)
	}
}

func TestWatchPollScanErrorBlocksStart(t *testing.T) {
	root := t.TempDir()
	clock := &fakeWatchClock{ticks: make(chan time.Time, 1), tickerCreated: make(chan struct{}, 1)}
	starter := &recordingStarter{}
	manager := newWatchManagerForTest(starter, &fakeWatcherFactory{watcher: newFakeWatcher()}, clock, watchSourceExists)
	t.Cleanup(manager.Close)
	if err := manager.Reconcile([]Job{watchJobAt(t, 3, 1, root, "poll")}); err != nil {
		t.Fatal(err)
	}
	<-clock.tickerCreated
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	clock.ticks <- time.Now()
	waitFor(t, func() bool { return manager.LastError(3) != nil })
	if got := starter.count(3); got != 0 {
		t.Fatalf("Start calls after scan error = %d", got)
	}
}

func TestWatchPollTriggersForRealSignatureChange(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("aa"), 0644); err != nil {
		t.Fatal(err)
	}
	clock := &fakeWatchClock{ticks: make(chan time.Time, 1), tickerCreated: make(chan struct{}, 1), added: make(chan struct{}, 1)}
	starter := &recordingStarter{}
	manager := newWatchManagerForTest(starter, &fakeWatcherFactory{watcher: newFakeWatcher()}, clock, watchSourceExists)
	t.Cleanup(manager.Close)
	if err := manager.Reconcile([]Job{watchJobAt(t, 3, 1, root, "poll")}); err != nil {
		t.Fatal(err)
	}
	<-clock.tickerCreated
	if err := os.Chtimes(file, time.Unix(101, 1), time.Unix(101, 1)); err != nil {
		t.Fatal(err)
	}
	clock.ticks <- time.Now()
	select {
	case <-clock.added:
	case <-time.After(time.Second):
		t.Fatal("poll did not reach debounce")
	}
	clock.FireAll()
	if got := starter.count(3); got != 1 {
		t.Fatalf("Start calls after poll = %d", got)
	}
}

func TestWatchStaleRevisionCallbackCannotStart(t *testing.T) {
	root := t.TempDir()
	oldWatcher, newWatcher := newFakeWatcher(), newFakeWatcher()
	clock := &fakeWatchClock{added: make(chan struct{}, 1)}
	starter := &recordingStarter{}
	manager := newWatchManagerForTest(starter, &fakeWatcherFactory{watchers: []*fakeWatcher{oldWatcher, newWatcher}}, clock, watchSourceExists)
	t.Cleanup(manager.Close)
	first := watchJobAt(t, 3, 1, root, "event")
	if err := manager.Reconcile([]Job{first}); err != nil {
		t.Fatal(err)
	}
	oldWatcher.events <- fsnotify.Event{Name: filepath.Join(root, "file"), Op: fsnotify.Write}
	select {
	case <-clock.added:
	case <-time.After(time.Second):
		t.Fatal("event did not create old callback")
	}
	second := first
	second.Revision = 2
	if err := manager.Reconcile([]Job{second}); err != nil {
		t.Fatal(err)
	}
	clock.FireAll()
	if got := starter.count(3); got != 0 {
		t.Fatalf("stale callback started revision %d", starter.calls[0].Revision)
	}
}

func (w *fakeWatcher) hasAdd(path string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, added := range w.adds {
		if added == path {
			return true
		}
	}
	return false
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.After(time.Second)
	for !condition() {
		select {
		case <-deadline:
			t.Fatal("condition was not reached")
		default:
			runtime.Gosched()
		}
	}
}

type fakeWatcherFactory struct {
	watcher  *fakeWatcher
	watchers []*fakeWatcher
	news     int
	mu       sync.Mutex
}

func (f *fakeWatcherFactory) New() (watcher, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.news++
	if len(f.watchers) >= f.news {
		return f.watchers[f.news-1], nil
	}
	return f.watcher, nil
}

func watchJob(t *testing.T, id int, revision uint64) Job {
	return watchJobAt(t, id, revision, t.TempDir(), "hybrid")
}

func watchJobAt(t *testing.T, id int, revision uint64, source, mode string) Job {
	t.Helper()
	return Job{
		ID: id, Revision: revision, Enabled: true, Trigger: TriggerWatch,
		Scheduler: SchedulerPolicy{Owner: SchedulerSyncBridge},
		Action:    Action{Type: ActionSync, Sync: SyncAction{Source: source}},
		WatchMode: mode, Debounce: 1, PollSec: 60,
	}
}

func TestWatchManagerResolvesHostPathBeforeWatching(t *testing.T) {
	root := t.TempDir()
	watcher := newFakeWatcher()
	manager := newWatchManagerForTest(&recordingStarter{}, &fakeWatcherFactory{watcher: watcher}, &fakeWatchClock{}, watchSourceExists)
	manager.resolvePath = func(path string) (string, error) {
		if path != "/srv/incoming" {
			t.Fatalf("logical source = %q", path)
		}
		return root, nil
	}
	defer manager.Close()

	job := watchJobAt(t, 3, 1, "/srv/incoming", "event")
	if err := manager.Reconcile([]Job{job}); err != nil {
		t.Fatal(err)
	}
	if !watcher.hasAdd(root) {
		t.Fatalf("resolved host root %q was not watched", root)
	}
}
