package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// MaxExecutionLogLineBytes bounds memory used for one stdout/stderr line.
const (
	MaxExecutionLogLineBytes = 64 * 1024
	hostNsenterPath          = "/usr/bin/nsenter"
)

// RunStatus is the terminal outcome of an execution.
type RunStatus string

const (
	RunSucceeded   RunStatus = "succeeded"
	RunFailed      RunStatus = "failed"
	RunKilled      RunStatus = "killed"
	RunTimedOut    RunStatus = "timed_out"
	RunStartFailed RunStatus = "start_failed"
)

// LogSink is a bounded, non-blocking delivery queue for normalized,
// newline-terminated stdout/stderr records. A sink returned by NewLogSink is
// single-use: after a successful Start, the executor closes Lines when
// streaming ends. A zero LogSink is a reusable drop-only sink. The executor
// never waits for a consumer; full or absent queues increment
// RunResult.DroppedLogLines.
type LogSink struct{ state *logSinkState }

type logSinkState struct {
	lines    chan []byte
	reserved atomic.Bool
}

func NewLogSink(maxLines int) LogSink {
	return LogSink{state: &logSinkState{lines: make(chan []byte, max(maxLines, 0))}}
}

func (s LogSink) Lines() <-chan []byte { return s.channel() }

func (s LogSink) channel() chan []byte {
	if s.state == nil {
		return nil
	}
	return s.state.lines
}

func (s LogSink) reserve() bool {
	return s.state == nil || s.state.reserved.CompareAndSwap(false, true)
}

func (s LogSink) release() {
	if s.state != nil {
		s.state.reserved.Store(false)
	}
}

// RunResult is immutable after Wait returns.
type RunResult struct {
	Status          RunStatus
	ExitCode        int
	KillEscalated   bool
	DroppedLogLines uint64
	Err             error
	LogError        error
	ControlError    error
}

type ExecutionHandle interface {
	PID() int
	Wait() RunResult
	Stop(context.Context, time.Duration) error
}

type Executor interface {
	Start(context.Context, ExecutionPlan, LogSink) (ExecutionHandle, error)
}

// HostExecutor launches installed wrappers in the host namespaces.
type HostExecutor struct {
	hostArgv           func(...string) []string
	commandPath        string
	waitLeader         func(int) error
	signalGroup        func(int, syscall.Signal) error
	groupHasLiveMember func(int) (bool, error)
	reapCommand        func(*exec.Cmd) error
}

func NewHostExecutor() *HostExecutor {
	executor := newHostExecutor(HostArgv)
	executor.commandPath = hostNsenterPath
	return executor
}

func newHostExecutor(hostArgv func(...string) []string) *HostExecutor {
	return &HostExecutor{
		hostArgv:           hostArgv,
		waitLeader:         waitLeaderExit,
		signalGroup:        signalProcessGroup,
		groupHasLiveMember: processGroupHasLiveMember,
		reapCommand:        func(command *exec.Cmd) error { return command.Wait() },
	}
}

func (e *HostExecutor) Start(ctx context.Context, plan ExecutionPlan, sink LogSink) (ExecutionHandle, error) {
	if ctx == nil {
		return startFailedHandle(errors.New("execution context is nil"))
	}
	if e == nil || e.hostArgv == nil {
		return startFailedHandle(errors.New("host argv builder is required"))
	}
	argv := e.hostArgv(plan.WrapperPath)
	if len(argv) == 0 {
		return startFailedHandle(errors.New("host execution argv is empty"))
	}

	reader, writer, err := os.Pipe()
	if err != nil {
		return startFailedHandle(fmt.Errorf("create execution log pipe: %w", err))
	}
	commandPath := argv[0]
	if e.commandPath != "" {
		commandPath = e.commandPath
	}
	command := exec.CommandContext(ctx, commandPath, argv[1:]...)
	command.Args = append([]string(nil), argv...)
	// CommandContext's default cancellation is Process.Kill. The controller
	// below owns every cancellation so TERM/grace/KILL always targets the group.
	command.Cancel = nil
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Stdout = writer
	command.Stderr = writer
	if len(plan.LauncherEnvironment) == 0 {
		command.Env = HostLauncherEnvironment()
	} else {
		command.Env = append([]string(nil), plan.LauncherEnvironment...)
	}

	if !sink.reserve() {
		_ = reader.Close()
		_ = writer.Close()
		return startFailedHandle(errors.New("log sink is already owned by an execution"))
	}
	if err := command.Start(); err != nil {
		sink.release()
		_ = reader.Close()
		_ = writer.Close()
		return startFailedHandle(err)
	}
	_ = writer.Close()
	handle := &executionHandle{
		pid:                command.Process.Pid,
		done:               make(chan struct{}),
		stopRequests:       make(chan stopRequest),
		signalGroup:        e.signalGroup,
		groupHasLiveMember: e.groupHasLiveMember,
	}
	streamDone := make(chan logStreamResult, 1)
	controlDone := make(chan controlResult, 1)
	leaderExited := make(chan error, 1)
	go func() {
		logs := streamExecutionLogs(reader, sink)
		if lines := sink.channel(); lines != nil {
			close(lines)
		}
		streamDone <- logs
		_ = reader.Close()
	}()
	go func() { leaderExited <- e.waitLeader(handle.pid) }()
	go handle.control(ctx, plan.Timeout, plan.StopGrace, leaderExited, controlDone)
	go handle.reap(command, e.reapCommand, streamDone, controlDone)
	return handle, nil
}

type executionHandle struct {
	pid                int
	done               chan struct{}
	stopRequests       chan stopRequest
	signalGroup        func(int, syscall.Signal) error
	groupHasLiveMember func(int) (bool, error)
	result             RunResult
}

func (h *executionHandle) PID() int { return h.pid }

func (h *executionHandle) Wait() RunResult {
	<-h.done
	return h.result
}

func (h *executionHandle) Stop(ctx context.Context, grace time.Duration) error {
	if ctx == nil {
		return errors.New("stop context is nil")
	}
	request := stopRequest{reason: terminationManual, grace: max(grace, 0)}
	select {
	case <-h.done:
		return h.result.ControlError
	case h.stopRequests <- request:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-h.done:
		return h.result.ControlError
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *executionHandle) reap(command *exec.Cmd, reapCommand func(*exec.Cmd) error, streamDone <-chan logStreamResult, controlDone <-chan controlResult) {
	control := <-controlDone
	waitErr := reapCommand(command) // The sole final reap.
	logs := <-streamDone
	exitCode := -1
	if command.ProcessState != nil {
		exitCode = command.ProcessState.ExitCode()
	}
	status := RunFailed
	switch {
	case control.initiated && control.reason == terminationManual:
		status = RunKilled
	case control.initiated && control.reason == terminationTimeout:
		status = RunTimedOut
	case exitCode == 0:
		status = RunSucceeded
	}
	h.result = RunResult{
		Status:          status,
		ExitCode:        exitCode,
		KillEscalated:   control.killEscalated,
		DroppedLogLines: logs.dropped,
		Err:             waitErr,
		LogError:        logs.err,
		ControlError:    control.err,
	}
	close(h.done)
}

type terminationReason uint8

const (
	terminationTimeout terminationReason = iota + 1
	terminationManual
)

type stopRequest struct {
	reason terminationReason
	grace  time.Duration
}

type controlResult struct {
	reason        terminationReason
	initiated     bool
	killEscalated bool
	err           error
}

func (h *executionHandle) control(ctx context.Context, timeout, defaultGrace time.Duration, leaderExited <-chan error, done chan<- controlResult) {
	var result controlResult
	var timeoutTimer, graceTimer *time.Timer
	var groupPoll *time.Ticker
	var timeoutC, graceC, pollC <-chan time.Time
	contextDone := ctx.Done()
	leaderExitC := leaderExited
	leaderObserved := false
	observationDone := false
	killSent := false
	probeErrorRecorded := false
	if timeout > 0 {
		timeoutTimer = time.NewTimer(timeout)
		timeoutC = timeoutTimer.C
	}
	defer func() {
		stopTimer(timeoutTimer)
		stopTimer(graceTimer)
		if groupPoll != nil {
			groupPoll.Stop()
		}
		done <- result
	}()

	probeGroup := func() bool {
		live, err := h.groupHasLiveMember(h.pid)
		if err == nil {
			return live
		}
		if !probeErrorRecorded {
			result.err = errors.Join(result.err, fmt.Errorf("inspect process group: %w", err))
			probeErrorRecorded = true
		}
		return true // Unknown: retain ownership and clean up conservatively.
	}

	initiate := func(request stopRequest) bool {
		if result.initiated {
			if request.reason > result.reason {
				result.reason = request.reason
			}
			return true
		}
		if err := h.signalGroup(h.pid, syscall.SIGTERM); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return false
			}
			result.err = errors.Join(result.err, fmt.Errorf("signal process group TERM: %w", err))
		}
		result.initiated = true
		result.reason = request.reason
		stopTimer(timeoutTimer)
		timeoutTimer, timeoutC = nil, nil
		graceTimer = time.NewTimer(max(request.grace, 0))
		graceC = graceTimer.C
		return true
	}

	for {
		select {
		case err := <-leaderExitC:
			leaderExitC = nil
			observationDone = true
			if err != nil {
				result.err = errors.Join(result.err, fmt.Errorf("observe leader exit: %w", err))
				if !result.initiated || killSent {
					return
				}
				continue
			}
			leaderObserved = true
			if killSent || !probeGroup() {
				return
			}
			if !result.initiated && !initiate(stopRequest{grace: defaultGrace}) {
				return
			}
			groupPoll = time.NewTicker(5 * time.Millisecond)
			pollC = groupPoll.C
		case request := <-h.stopRequests:
			initiate(request)
		case <-contextDone:
			reason := terminationManual
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				reason = terminationTimeout
			}
			initiate(stopRequest{reason: reason, grace: defaultGrace})
			contextDone = nil
		case <-timeoutC:
			initiate(stopRequest{reason: terminationTimeout, grace: defaultGrace})
			timeoutC = nil
		case <-pollC:
			if !probeGroup() {
				return
			}
		case <-graceC:
			if leaderObserved && !probeGroup() {
				return
			}
			if err := h.signalGroup(h.pid, syscall.SIGKILL); err != nil {
				if !errors.Is(err, syscall.ESRCH) {
					result.err = errors.Join(result.err, fmt.Errorf("signal process group KILL: %w", err))
				}
			} else {
				result.killEscalated = true
			}
			killSent = true
			graceC, pollC = nil, nil
			if observationDone {
				return
			}
		}
	}
}

func signalProcessGroup(pgid int, signal syscall.Signal) error {
	if pgid <= 0 {
		return syscall.ESRCH
	}
	return syscall.Kill(-pgid, signal)
}

func waitLeaderExit(pid int) error {
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

func processGroupHasLiveMember(pgid int) (bool, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false, fmt.Errorf("read /proc: %w", err)
	}
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		stat, err := os.ReadFile("/proc/" + entry.Name() + "/stat")
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("read /proc/%s/stat: %w", entry.Name(), err)
		}
		text := string(stat)
		end := strings.LastIndexByte(text, ')')
		if end < 0 {
			return false, fmt.Errorf("parse /proc/%s/stat: missing command terminator", entry.Name())
		}
		fields := strings.Fields(text[end+1:])
		if len(fields) < 3 {
			return false, fmt.Errorf("parse /proc/%s/stat: missing process group", entry.Name())
		}
		group, err := strconv.Atoi(fields[2])
		if err != nil {
			return false, fmt.Errorf("parse /proc/%s/stat process group: %w", entry.Name(), err)
		}
		if group == pgid && fields[0] != "Z" {
			return true, nil
		}
	}
	return false, nil
}

func stopTimer(timer *time.Timer) {
	if timer != nil {
		timer.Stop()
	}
}

func startFailedHandle(err error) (*executionHandle, error) {
	done := make(chan struct{})
	close(done)
	return &executionHandle{
		done:   done,
		result: RunResult{Status: RunStartFailed, ExitCode: -1, Err: err},
	}, err
}

type logStreamResult struct {
	err     error
	dropped uint64
}

func streamExecutionLogs(reader io.Reader, sink LogSink) logStreamResult {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), MaxExecutionLogLineBytes+1)
	scanner.Split(executionLineSplitter())
	lines := sink.channel()
	var result logStreamResult
	for scanner.Scan() {
		line := append(append([]byte(nil), scanner.Bytes()...), '\n')
		select {
		case lines <- line:
		default:
			result.dropped++
		}
	}
	scanErr := scanner.Err()
	if scanErr != nil {
		scanErr = fmt.Errorf("scan execution log (maximum line %d bytes): %w", MaxExecutionLogLineBytes, scanErr)
		select {
		case lines <- []byte("[syncbridge] log stream error: " + scanErr.Error() + "\n"):
		default:
			result.dropped++
		}
		_, drainErr := io.Copy(io.Discard, reader)
		if drainErr != nil {
			scanErr = errors.Join(scanErr, fmt.Errorf("drain execution log: %w", drainErr))
		}
	}
	result.err = scanErr
	return result
}

func executionLineSplitter() bufio.SplitFunc {
	skipLF := false
	return func(data []byte, atEOF bool) (advance int, token []byte, err error) {
		if skipLF && len(data) > 0 {
			skipLF = false
			if data[0] == '\n' {
				return 1, nil, nil
			}
		}
		for i, b := range data {
			if b != '\n' && b != '\r' {
				continue
			}
			advance = i + 1
			if b == '\r' {
				if advance < len(data) && data[advance] == '\n' {
					advance++
				} else if advance == len(data) && !atEOF {
					skipLF = true
				}
			}
			return advance, data[:i], nil
		}
		if atEOF && len(data) > 0 {
			return len(data), data, nil
		}
		return 0, nil, nil
	}
}
