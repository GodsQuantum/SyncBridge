package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

type watcher interface {
	Events() <-chan fsnotify.Event
	Errors() <-chan error
	Add(string) error
	Close() error
}

type watcherFactory interface{ New() (watcher, error) }

type fsnotifyFactory struct{}

func (fsnotifyFactory) New() (watcher, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	return fsnotifyWatcher{w}, nil
}

type fsnotifyWatcher struct{ *fsnotify.Watcher }

func (w fsnotifyWatcher) Events() <-chan fsnotify.Event { return w.Watcher.Events }
func (w fsnotifyWatcher) Errors() <-chan error          { return w.Watcher.Errors }

type watchTimer interface{ Stop() bool }

type watchTicker interface {
	C() <-chan time.Time
	Stop()
}

type watchClock interface {
	AfterFunc(time.Duration, func()) watchTimer
	NewTicker(time.Duration) watchTicker
}

type realWatchClock struct{}

func (realWatchClock) AfterFunc(delay time.Duration, fn func()) watchTimer {
	return time.AfterFunc(delay, fn)
}
func (realWatchClock) NewTicker(delay time.Duration) watchTicker {
	return realWatchTicker{time.NewTicker(delay)}
}

type realWatchTicker struct{ *time.Ticker }

func (t realWatchTicker) C() <-chan time.Time { return t.Ticker.C }

type watchSpec struct {
	revision   uint64
	generation uint64
	source     string
	viewSource string
	mode       string
	debounce   time.Duration
	poll       time.Duration
	globs      []string
}

type watchHandle struct {
	ctx     context.Context
	spec    watchSpec
	cancel  context.CancelFunc
	watcher watcher
	workers sync.WaitGroup
	opMu    sync.Mutex
	timer   watchTimer
}

// WatchManager owns fsnotify, polling, timers, and every watcher goroutine.
// It only turns a valid trigger into one immutable RunService request.
type WatchManager struct {
	mu          sync.Mutex
	reconcileMu sync.Mutex
	callbacks   sync.WaitGroup
	ctx         context.Context
	cancel      context.CancelFunc
	starter     RunStarter
	factory     watcherFactory
	clock       watchClock
	checkSource func(string) error
	resolvePath func(string) (string, error)
	active      map[int]*watchHandle
	degraded    map[int]bool
	errors      map[int]error
	nextGen     uint64
	closed      bool
}

func NewWatchManager(ctx context.Context, starter RunStarter) *WatchManager {
	if ctx == nil {
		panic("watch manager context is nil")
	}
	manager := newWatchManager(ctx, starter, fsnotifyFactory{}, realWatchClock{}, watchSourceExists)
	manager.resolvePath = hostFilesystemPath
	return manager
}

func newWatchManager(ctx context.Context, starter RunStarter, factory watcherFactory, clock watchClock, checkSource func(string) error) *WatchManager {
	child, cancel := context.WithCancel(ctx)
	return &WatchManager{ctx: child, cancel: cancel, starter: starter, factory: factory, clock: clock, checkSource: checkSource, resolvePath: func(path string) (string, error) { return path, nil }, active: make(map[int]*watchHandle), degraded: make(map[int]bool), errors: make(map[int]error)}
}

func newWatchManagerForTest(starter RunStarter, factory watcherFactory, clock watchClock, checkSource func(string) error) *WatchManager {
	return newWatchManager(context.Background(), starter, factory, clock, checkSource)
}

// Reconcile atomically publishes complete watcher sets. Any invalid replacement
// leaves its prior valid watcher in place and exposes the error.
func (m *WatchManager) Reconcile(jobs []Job) error {
	if m == nil || m.starter == nil || m.factory == nil || m.clock == nil || m.checkSource == nil || m.resolvePath == nil {
		return errors.New("watch manager is not configured")
	}
	desired := make(map[int]watchSpec)
	invalid := make(map[int]error)
	for _, job := range jobs {
		if !schedulerEligible(job, TriggerWatch) {
			continue
		}
		spec, err := watchSpecFor(job)
		if err == nil {
			spec.viewSource, err = m.resolvePath(spec.source)
		}
		if err == nil {
			err = m.checkSource(spec.viewSource)
		}
		if err != nil {
			invalid[job.ID] = err
			continue
		}
		desired[job.ID] = spec
	}

	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return errors.New("watch manager is closed")
	}
	create := make(map[int]watchSpec)
	for id, want := range desired {
		current := m.active[id]
		if current != nil && sameWatchSpec(current.spec, want) && !m.degraded[id] {
			continue
		}
		m.nextGen++
		want.generation = m.nextGen
		create[id] = want
	}
	m.mu.Unlock()

	prepared := make(map[int]*watchHandle, len(create))
	for id, spec := range create {
		handle, err := m.newHandle(spec)
		if err != nil {
			for _, ready := range prepared {
				ready.stop()
			}
			m.publishError(id, nil, err, false)
			return fmt.Errorf("job %d: %w", id, err)
		}
		prepared[id] = handle
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		for _, ready := range prepared {
			ready.stop()
		}
		return errors.New("watch manager is closed")
	}
	next := make(map[int]*watchHandle, len(desired))
	stopped := make([]*watchHandle, 0)
	for id, current := range m.active {
		want, wanted := desired[id]
		if _, bad := invalid[id]; bad {
			next[id] = current
			continue
		}
		if !wanted {
			stopped = append(stopped, current)
			continue
		}
		if ready := prepared[id]; ready != nil {
			next[id] = ready
			stopped = append(stopped, current)
			continue
		}
		if sameWatchSpec(current.spec, want) && !m.degraded[id] {
			next[id] = current
		}
	}
	for id := range desired {
		if _, exists := next[id]; !exists {
			next[id] = prepared[id]
		}
	}
	m.active = next
	for id, err := range invalid {
		m.errors[id] = err
	}
	for id := range desired {
		if _, bad := invalid[id]; !bad {
			delete(m.errors, id)
			delete(m.degraded, id)
		}
	}
	m.mu.Unlock()
	for _, handle := range stopped {
		handle.stop()
	}
	for id, handle := range prepared {
		m.start(id, handle)
	}
	return errors.Join(mapErrors(invalid)...)
}

func (m *WatchManager) newHandle(spec watchSpec) (*watchHandle, error) {
	ctx, cancel := context.WithCancel(m.ctx)
	handle := &watchHandle{ctx: ctx, spec: spec, cancel: cancel}
	if err := m.checkSource(spec.viewSource); err != nil {
		handle.stop()
		return nil, err
	}
	if spec.mode == "event" || spec.mode == "hybrid" {
		w, err := m.factory.New()
		if err != nil {
			cancel()
			return nil, err
		}
		handle.watcher = w
		if parent := filepath.Dir(spec.viewSource); parent != spec.viewSource {
			if err := w.Add(parent); err != nil {
				handle.stop()
				return nil, err
			}
		}
		if err := handle.addTree(spec.viewSource); err != nil {
			handle.stop()
			return nil, err
		}
	}
	return handle, nil
}

func (m *WatchManager) start(id int, handle *watchHandle) {
	if handle.watcher != nil {
		handle.workers.Add(1)
		go func() {
			defer handle.workers.Done()
			for {
				select {
				case event, ok := <-handle.watcher.Events():
					if !ok {
						return
					}
					m.handleEvent(id, handle, event)
				case err, ok := <-handle.watcher.Errors():
					if !ok {
						return
					}
					m.degrade(id, handle, err)
				case <-handle.ctx.Done():
					return
				}
			}
		}()
	}
	if handle.spec.mode == "poll" || handle.spec.mode == "hybrid" {
		handle.workers.Add(1)
		go m.poll(id, handle)
	}
}

func (m *WatchManager) handleEvent(id int, handle *watchHandle, event fsnotify.Event) {
	if samePath(event.Name, handle.spec.viewSource) {
		if event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
			m.degrade(id, handle, errors.New("watch root disappeared"))
			return
		}
		if event.Op&fsnotify.Create != 0 {
			m.rearmRoot(id, handle)
			return
		}
	}
	if !watchPathWithin(handle.spec.viewSource, event.Name) {
		return
	}
	if event.Op&fsnotify.Create != 0 {
		info, err := os.Stat(event.Name)
		if err != nil {
			m.degrade(id, handle, err)
			return
		}
		if info.IsDir() && watchPathWithin(handle.spec.viewSource, event.Name) {
			if err := handle.addTree(event.Name); err != nil {
				m.degrade(id, handle, err)
				return
			}
		}
	}
	if matchesWatch(handle.spec.globs, event.Name) {
		m.signalHandle(id, handle)
	}
}

func (m *WatchManager) rearmRoot(id int, handle *watchHandle) {
	if err := m.checkSource(handle.spec.viewSource); err != nil {
		m.degrade(id, handle, err)
		return
	}
	if err := handle.addTree(handle.spec.viewSource); err != nil {
		m.degrade(id, handle, err)
		return
	}
	m.mu.Lock()
	if m.active[id] == handle && !m.closed {
		delete(m.degraded, id)
		delete(m.errors, id)
	}
	m.mu.Unlock()
}

func (m *WatchManager) poll(id int, handle *watchHandle) {
	defer handle.workers.Done()
	last, err := directorySignature(handle.ctx, handle.spec.viewSource, handle.spec.globs)
	if err != nil {
		m.degrade(id, handle, err)
	}
	ticker := m.clock.NewTicker(handle.spec.poll)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C():
			next, err := directorySignature(handle.ctx, handle.spec.viewSource, handle.spec.globs)
			if err != nil {
				m.degrade(id, handle, err)
				continue
			}
			if err == nil && next != last {
				last = next
				m.signalHandle(id, handle)
			}
		case <-handle.ctx.Done():
			return
		}
	}
}

func (m *WatchManager) signal(id int, revision uint64, _ string) {
	m.mu.Lock()
	handle := m.active[id]
	ok := handle != nil && handle.spec.revision == revision && !m.closed && !m.degraded[id]
	m.mu.Unlock()
	if ok {
		m.signalHandle(id, handle)
	}
}

func (m *WatchManager) signalHandle(id int, handle *watchHandle) {
	m.mu.Lock()
	ok := !m.closed && m.active[id] == handle && !m.degraded[id]
	m.mu.Unlock()
	if !ok {
		return
	}
	handle.opMu.Lock()
	defer handle.opMu.Unlock()
	select {
	case <-handle.ctx.Done():
		return
	default:
	}
	if handle.timer != nil {
		handle.timer.Stop()
	}
	handle.timer = m.clock.AfterFunc(handle.spec.debounce, func() { m.fire(id, handle) })
}

func (m *WatchManager) fire(id int, handle *watchHandle) {
	m.mu.Lock()
	if m.closed || m.active[id] != handle || m.degraded[id] {
		m.mu.Unlock()
		return
	}
	m.callbacks.Add(1)
	ctx := m.ctx
	m.mu.Unlock()

	if err := m.checkSource(handle.spec.viewSource); err != nil {
		m.degrade(id, handle, err)
		m.callbacks.Done()
		return
	}
	_, err := m.starter.Start(ctx, StartRunInput{JobID: id, Revision: handle.spec.revision, Origin: RunOriginWatch})
	if err != nil && !errors.Is(err, ErrOverlap) {
		m.publishError(id, handle, err, false)
	}
	m.callbacks.Done()
}

func (m *WatchManager) degrade(id int, handle *watchHandle, err error) {
	if err == nil {
		return
	}
	m.publishError(id, handle, err, true)
}

func (m *WatchManager) publishError(id int, handle *watchHandle, err error, degraded bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || (handle != nil && m.active[id] != handle) {
		return
	}
	m.errors[id] = err
	if degraded {
		m.degraded[id] = true
	}
}

func (m *WatchManager) Active(id int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.active[id]
	return ok
}

func (m *WatchManager) LastError(id int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.errors[id]
}

func (m *WatchManager) Close() {
	if m == nil {
		return
	}
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	m.cancel()
	handles := make([]*watchHandle, 0, len(m.active))
	for _, handle := range m.active {
		handles = append(handles, handle)
	}
	m.active = make(map[int]*watchHandle)
	m.mu.Unlock()
	for _, handle := range handles {
		handle.stop()
	}
	m.callbacks.Wait()
}

func (h *watchHandle) addTree(root string) error {
	h.opMu.Lock()
	defer h.opMu.Unlock()
	select {
	case <-h.ctx.Done():
		return context.Canceled
	default:
	}
	return addWatchDirectories(h.watcher, root)
}

func (h *watchHandle) stop() {
	h.cancel()
	h.opMu.Lock()
	if h.timer != nil {
		h.timer.Stop()
	}
	if h.watcher != nil {
		_ = h.watcher.Close()
	}
	h.opMu.Unlock()
	h.workers.Wait()
}

func watchSpecFor(job Job) (watchSpec, error) {
	if job.Action.Type != ActionSync || job.Action.Sync.Source == "" {
		return watchSpec{}, errors.New("watch job source is required")
	}
	mode := job.WatchMode
	if mode == "" {
		mode = "hybrid"
	}
	if mode != "event" && mode != "poll" && mode != "hybrid" {
		return watchSpec{}, errors.New("watch mode must be event, poll, or hybrid")
	}
	debounce := time.Duration(job.Debounce) * time.Second
	if debounce <= 0 {
		debounce = 10 * time.Second
	}
	poll := time.Duration(job.PollSec) * time.Second
	if poll <= 0 {
		poll = 5 * time.Minute
	}
	return watchSpec{revision: job.Revision, source: job.Action.Sync.Source, mode: mode, debounce: debounce, poll: poll, globs: splitCSV(job.WatchGlob)}, nil
}

func sameWatchSpec(a, b watchSpec) bool {
	if a.revision != b.revision || a.source != b.source || a.viewSource != b.viewSource || a.mode != b.mode || a.debounce != b.debounce || a.poll != b.poll || len(a.globs) != len(b.globs) {
		return false
	}
	for i := range a.globs {
		if a.globs[i] != b.globs[i] {
			return false
		}
	}
	return true
}

func watchSourceExists(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("watch source is not a directory")
	}
	return nil
}

func addWatchDirectories(w watcher, root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return w.Add(path)
		}
		return nil
	})
}

func matchesWatch(globs []string, path string) bool {
	if len(globs) == 0 {
		return true
	}
	name := filepath.Base(path)
	for _, glob := range globs {
		if ok, _ := filepath.Match(strings.TrimSpace(glob), name); ok {
			return true
		}
	}
	return false
}

// directorySignature streams deterministic matching metadata into SHA-256 while
// bounding filesystem work and honoring watcher cancellation.
func directorySignature(ctx context.Context, root string, globs []string) (string, error) {
	return directorySignatureWithLimit(ctx, root, globs, 100_000)
}

func directorySignatureWithLimit(ctx context.Context, root string, globs []string, limit int) (string, error) {
	if ctx == nil {
		return "", errors.New("directory signature context is nil")
	}
	if limit <= 0 {
		return "", errors.New("directory signature limit must be positive")
	}
	hash := sha256.New()
	entries := 0
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		entries++
		if entries > limit {
			return fmt.Errorf("directory signature entry limit exceeded: %d", limit)
		}
		if samePath(path, root) || !matchesWatch(globs, path) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(hash, "%s\x00%v\x00%d\x00%d\n", rel, info.Mode().Type(), info.Size(), info.ModTime().UnixNano())
		return nil
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func samePath(a, b string) bool { return filepath.Clean(a) == filepath.Clean(b) }

func watchPathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
