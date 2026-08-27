package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestExecutionHandleStopsWholeProcessGroup(t *testing.T) {
	requireLinuxProcessGroups(t)
	childReady := filepath.Join(t.TempDir(), "child-ready")
	childPIDPath := filepath.Join(t.TempDir(), "child-pid")
	executor := localTestExecutor("spawn-child", childReady, childPIDPath)

	h, err := executor.Start(context.Background(), helperPlan(), LogSink{})
	if err != nil {
		t.Fatal(err)
	}
	waitForFile(t, childReady)
	childPID := readPID(t, childPIDPath)

	if err := h.Stop(context.Background(), 40*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	result := h.Wait()
	if result.Status != RunKilled {
		t.Fatalf("status = %q, want %q; result=%#v", result.Status, RunKilled, result)
	}
	if !result.KillEscalated {
		t.Fatalf("KillEscalated = false; result=%#v", result)
	}
	waitForProcessGone(t, childPID)
}

func TestExecutionReapsLeaderOnlyAfterLastGroupSignal(t *testing.T) {
	requireLinuxProcessGroups(t)
	childReady := filepath.Join(t.TempDir(), "child-ready")
	childPIDPath := filepath.Join(t.TempDir(), "child-pid")
	executor := localTestExecutor("spawn-child", childReady, childPIDPath)

	var released, signaledAfterRelease atomic.Bool
	var reapCalls atomic.Int32
	realSignal := executor.signalGroup
	executor.signalGroup = func(pgid int, signal syscall.Signal) error {
		if released.Load() {
			signaledAfterRelease.Store(true)
		}
		return realSignal(pgid, signal)
	}
	realReap := executor.reapCommand
	executor.reapCommand = func(command *exec.Cmd) error {
		reapCalls.Add(1)
		released.Store(true)
		return realReap(command)
	}

	h, err := executor.Start(context.Background(), helperPlan(), LogSink{})
	if err != nil {
		t.Fatal(err)
	}
	waitForFile(t, childReady)
	childPID := readPID(t, childPIDPath)
	if err := h.Stop(context.Background(), 30*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	result := h.Wait()
	if result.Status != RunKilled || !result.KillEscalated {
		t.Fatalf("result = %#v", result)
	}
	if signaledAfterRelease.Load() || reapCalls.Load() != 1 {
		t.Fatalf("signal after identity release=%v reap calls=%d", signaledAfterRelease.Load(), reapCalls.Load())
	}
	waitForProcessGone(t, childPID)
}

func TestExecutionUnknownGroupStateCleansUpBeforeReap(t *testing.T) {
	requireLinuxProcessGroups(t)
	executor := localTestExecutor("exit", "0")
	probeErr := errors.New("process group probe unavailable")
	executor.groupHasLiveMember = func(int) (bool, error) {
		return false, probeErr
	}

	var released, signaledAfterRelease atomic.Bool
	var reapCalls atomic.Int32
	var signals []syscall.Signal
	executor.signalGroup = func(_ int, signal syscall.Signal) error {
		if released.Load() {
			signaledAfterRelease.Store(true)
		}
		signals = append(signals, signal)
		return nil
	}
	realReap := executor.reapCommand
	executor.reapCommand = func(command *exec.Cmd) error {
		reapCalls.Add(1)
		released.Store(true)
		return realReap(command)
	}
	plan := helperPlan()
	plan.Timeout = 0
	plan.StopGrace = 10 * time.Millisecond

	h, err := executor.Start(context.Background(), plan, LogSink{})
	if err != nil {
		t.Fatal(err)
	}
	result := waitForResult(t, h, 2*time.Second)
	if result.Status != RunSucceeded || !result.KillEscalated {
		t.Fatalf("result = %#v, want bounded successful exit with cleanup escalation", result)
	}
	if !errors.Is(result.ControlError, probeErr) {
		t.Fatalf("ControlError = %v, want %v", result.ControlError, probeErr)
	}
	if got, want := signals, []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL}; !equalSignals(got, want) {
		t.Fatalf("signals = %v, want %v", got, want)
	}
	if signaledAfterRelease.Load() || reapCalls.Load() != 1 {
		t.Fatalf("signal after identity release=%v reap calls=%d", signaledAfterRelease.Load(), reapCalls.Load())
	}
}

func TestExecutionTimeoutEscalatesTERMToKILL(t *testing.T) {
	requireLinuxProcessGroups(t)
	ready := filepath.Join(t.TempDir(), "ready")
	plan := helperPlan()
	plan.Timeout = 40 * time.Millisecond
	plan.StopGrace = 25 * time.Millisecond
	h, err := localTestExecutor("ignore-term", ready).Start(context.Background(), plan, LogSink{})
	if err != nil {
		t.Fatal(err)
	}
	waitForFile(t, ready)

	result := h.Wait()
	if result.Status != RunTimedOut || !result.KillEscalated {
		t.Fatalf("result = %#v, want timed_out with KILL escalation", result)
	}
}

func TestExecutionHandleTERMCanStopWithoutEscalation(t *testing.T) {
	requireLinuxProcessGroups(t)
	ready := filepath.Join(t.TempDir(), "ready")
	plan := helperPlan()
	h, err := localTestExecutor("term-exit", ready).Start(context.Background(), plan, LogSink{})
	if err != nil {
		t.Fatal(err)
	}
	waitForFile(t, ready)

	if err := h.Stop(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
	result := h.Wait()
	if result.Status != RunKilled || result.KillEscalated {
		t.Fatalf("result = %#v, want graceful killed result", result)
	}
}

func TestExecutionHandleConcurrentWaitReturnsCachedTerminalResult(t *testing.T) {
	sink := LogSink{}
	h, err := localTestExecutor("exit", "7").Start(context.Background(), helperPlan(), sink)
	if err != nil {
		t.Fatal(err)
	}

	const callers = 32
	results := make(chan RunResult, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- h.Wait()
		}()
	}
	wg.Wait()
	close(results)

	for result := range results {
		if result.Status != RunFailed || result.ExitCode != 7 || result.Err == nil {
			t.Fatalf("Wait result = %#v, want cached exit 7 failure", result)
		}
	}
	again := h.Wait()
	if again.Status != RunFailed || again.ExitCode != 7 {
		t.Fatalf("later Wait result = %#v", again)
	}
}

func TestExecutionHandleConcurrentStopIsIdempotent(t *testing.T) {
	requireLinuxProcessGroups(t)
	ready := filepath.Join(t.TempDir(), "ready")
	h, err := localTestExecutor("ignore-term", ready).Start(context.Background(), helperPlan(), LogSink{})
	if err != nil {
		t.Fatal(err)
	}
	waitForFile(t, ready)

	const callers = 24
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- h.Stop(context.Background(), 30*time.Millisecond)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Stop: %v", err)
		}
	}
	result := h.Wait()
	if result.Status != RunKilled || !result.KillEscalated {
		t.Fatalf("result = %#v", result)
	}
	if err := h.Stop(context.Background(), 0); err != nil {
		t.Fatalf("Stop after completion: %v", err)
	}
}

func TestExecutionManualStopTakesPriorityWhileTimeoutTerminationIsActive(t *testing.T) {
	requireLinuxProcessGroups(t)
	ready := filepath.Join(t.TempDir(), "ready")
	termSeen := filepath.Join(t.TempDir(), "term-seen")
	plan := helperPlan()
	plan.Timeout = 500 * time.Millisecond
	plan.StopGrace = 50 * time.Millisecond
	h, err := localTestExecutor("observe-term", ready, termSeen).Start(context.Background(), plan, LogSink{})
	if err != nil {
		t.Fatal(err)
	}
	waitForFile(t, ready)
	waitForFile(t, termSeen)

	if err := h.Stop(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
	result := h.Wait()
	if result.Status != RunKilled || !result.KillEscalated {
		t.Fatalf("result = %#v, want manual stop to outrank active timeout", result)
	}
}

func TestExecutionManualStopOutranksTimeoutAfterLeaderExit(t *testing.T) {
	requireLinuxProcessGroups(t)
	childReady := filepath.Join(t.TempDir(), "child-ready")
	childPIDPath := filepath.Join(t.TempDir(), "child-pid")
	executor := localTestExecutor("spawn-child", childReady, childPIDPath)
	leaderObserved := make(chan struct{})
	var observedOnce sync.Once
	realGroupCheck := executor.groupHasLiveMember
	executor.groupHasLiveMember = func(pgid int) (bool, error) {
		observedOnce.Do(func() { close(leaderObserved) })
		return realGroupCheck(pgid)
	}
	plan := helperPlan()
	plan.Timeout = 500 * time.Millisecond
	plan.StopGrace = 100 * time.Millisecond

	h, err := executor.Start(context.Background(), plan, LogSink{})
	if err != nil {
		t.Fatal(err)
	}
	waitForFile(t, childReady)
	select {
	case <-leaderObserved:
	case <-time.After(2 * time.Second):
		t.Fatal("leader exit was not observed")
	}
	if err := h.Stop(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
	if result := h.Wait(); result.Status != RunKilled || !result.KillEscalated {
		t.Fatalf("result = %#v, want manual priority during child cleanup", result)
	}
}

func TestExecutionExitObservedBeforeStopKeepsExitStatus(t *testing.T) {
	h, err := localTestExecutor("exit", "0").Start(context.Background(), helperPlan(), LogSink{})
	if err != nil {
		t.Fatal(err)
	}
	first := h.Wait()
	if first.Status != RunSucceeded {
		t.Fatalf("first Wait = %#v", first)
	}
	if err := h.Stop(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if again := h.Wait(); again.Status != RunSucceeded {
		t.Fatalf("status changed after late Stop: %#v", again)
	}
}

func TestExecutionContextCancellationUsesGracefulGroupStop(t *testing.T) {
	requireLinuxProcessGroups(t)
	ready := filepath.Join(t.TempDir(), "ready")
	ctx, cancel := context.WithCancel(context.Background())
	plan := helperPlan()
	plan.StopGrace = 25 * time.Millisecond
	h, err := localTestExecutor("ignore-term", ready).Start(ctx, plan, LogSink{})
	if err != nil {
		t.Fatal(err)
	}
	waitForFile(t, ready)
	cancel()

	result := h.Wait()
	if result.Status != RunKilled || !result.KillEscalated {
		t.Fatalf("result = %#v, want canceled context to use graceful kill machine", result)
	}
}

func TestExecutionStartFailureReturnsTerminalStartFailedResult(t *testing.T) {
	executor := newHostExecutor(func(...string) []string {
		return []string{filepath.Join(t.TempDir(), "does-not-exist")}
	})
	h, err := executor.Start(context.Background(), helperPlan(), LogSink{})
	if err == nil {
		t.Fatal("Start error = nil")
	}
	if h == nil {
		t.Fatal("Start returned nil handle; start_failed result is not observable")
	}
	result := h.Wait()
	if result.Status != RunStartFailed || result.Err == nil || h.PID() != 0 {
		t.Fatalf("result = %#v, pid=%d", result, h.PID())
	}
	if err := h.Stop(context.Background(), 0); err != nil {
		t.Fatalf("Stop on start_failed handle: %v", err)
	}
}

func TestExecutionContextCanceledBeforeStartDoesNotLaunch(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h, err := localTestExecutor("touch", marker).Start(ctx, helperPlan(), LogSink{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Start error = %v, want context.Canceled", err)
	}
	if h == nil || h.Wait().Status != RunStartFailed {
		t.Fatalf("handle/result = %#v", h)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("helper launched despite canceled context: %v", statErr)
	}
}

func TestExecutionUsesOnlyPlanLauncherEnvironment(t *testing.T) {
	t.Setenv("SYNCBRIDGE_MUST_NOT_LEAK", "container-secret")
	plan := helperPlan()
	plan.LauncherEnvironment = []string{"ONLY=expected"}
	sink := NewLogSink(16)
	h, err := localTestExecutor("environment").Start(context.Background(), plan, sink)
	if err != nil {
		t.Fatal(err)
	}
	if result := h.Wait(); result.Status != RunSucceeded {
		t.Fatalf("result = %#v", result)
	}
	if got := drainLogSink(sink); got != "ONLY=expected\n" {
		t.Fatalf("helper environment = %q", got)
	}
}

func TestHostExecutorDoesNotResolveNsenterFromProcessPATH(t *testing.T) {
	maliciousDir := t.TempDir()
	marker := filepath.Join(maliciousDir, "launched")
	malicious := filepath.Join(maliciousDir, "nsenter")
	script := fmt.Sprintf("#!/bin/sh\n/usr/bin/touch %q\n", marker)
	if err := os.WriteFile(malicious, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", maliciousDir)

	h, err := NewHostExecutor().Start(context.Background(), helperPlan(), LogSink{})
	if err != nil {
		t.Fatal(err)
	}
	_ = h.Wait()
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("process PATH controlled the nsenter executable: %v", err)
	}
}

func TestExecutionUsesHostLauncherEnvironmentWhenPlanEnvironmentIsAbsent(t *testing.T) {
	plan := helperPlan()
	plan.LauncherEnvironment = nil
	sink := NewLogSink(16)
	h, err := localTestExecutor("environment").Start(context.Background(), plan, sink)
	if err != nil {
		t.Fatal(err)
	}
	if result := h.Wait(); result.Status != RunSucceeded {
		t.Fatalf("result = %#v", result)
	}
	want := append([]string(nil), HostLauncherEnvironment()...)
	sort.Strings(want)
	if got := strings.Split(strings.TrimSuffix(drainLogSink(sink), "\n"), "\n"); !equalStrings(got, want) {
		t.Fatalf("helper environment = %#v, want %#v", got, want)
	}
}

func TestExecutionPassesOnlyInstalledWrapperPathToHostArgv(t *testing.T) {
	const wrapper = "/var/lib/syncbridge/instances/node-a/jobs/7/run.sh"
	var got []string
	executor := newHostExecutor(func(argv ...string) []string {
		got = append([]string(nil), argv...)
		return helperCommand("exit", "0")
	})
	plan := helperPlan()
	plan.WrapperPath = wrapper
	h, err := executor.Start(context.Background(), plan, LogSink{})
	if err != nil {
		t.Fatal(err)
	}
	if result := h.Wait(); result.Status != RunSucceeded {
		t.Fatalf("result = %#v", result)
	}
	if !equalStrings(got, []string{wrapper}) {
		t.Fatalf("HostArgv inputs = %#v, want wrapper path only", got)
	}
}

func TestExecutionCreatesDedicatedProcessGroup(t *testing.T) {
	requireLinuxProcessGroups(t)
	sink := NewLogSink(1)
	h, err := localTestExecutor("process-group").Start(context.Background(), helperPlan(), sink)
	if err != nil {
		t.Fatal(err)
	}
	if result := h.Wait(); result.Status != RunSucceeded {
		t.Fatalf("result = %#v", result)
	}
	var pid, pgid int
	logs := drainLogSink(sink)
	if _, err := fmt.Sscanf(strings.TrimSpace(logs), "%d %d", &pid, &pgid); err != nil {
		t.Fatalf("parse process group %q: %v", logs, err)
	}
	if pid != h.PID() || pgid != pid {
		t.Fatalf("pid=%d pgid=%d handle pid=%d", pid, pgid, h.PID())
	}
}

func TestExecutionMergesStdoutStderrAndSplitsLFCRAndCRLF(t *testing.T) {
	sink := NewLogSink(4)
	h, err := localTestExecutor("mixed-logs").Start(context.Background(), helperPlan(), sink)
	if err != nil {
		t.Fatal(err)
	}
	if result := h.Wait(); result.Status != RunSucceeded || result.LogError != nil {
		t.Fatalf("result = %#v", result)
	}
	if got, want := drainLogSink(sink), "stdout-1\nstderr-1\nstdout-2\ntail\n"; got != want {
		t.Fatalf("merged logs = %q, want %q", got, want)
	}
}

func TestExecutionClosesLogSinkWhenStreamingEnds(t *testing.T) {
	sink := NewLogSink(8)
	logsDone := make(chan string, 1)
	go func() {
		var logs strings.Builder
		for line := range sink.Lines() {
			logs.Write(line)
		}
		logsDone <- logs.String()
	}()
	h, err := localTestExecutor("mixed-logs").Start(context.Background(), helperPlan(), sink)
	if err != nil {
		t.Fatal(err)
	}
	if result := h.Wait(); result.Status != RunSucceeded {
		t.Fatalf("result = %#v", result)
	}
	select {
	case got := <-logsDone:
		if want := "stdout-1\nstderr-1\nstdout-2\ntail\n"; got != want {
			t.Fatalf("logs = %q, want %q", got, want)
		}
	case <-time.After(100 * time.Millisecond):
		close(sink.state.lines)
		<-logsDone
		t.Fatal("LogSink.Lines was not closed after streaming ended")
	}
}

func TestExecutionRejectsSequentialLogSinkReuse(t *testing.T) {
	sink := NewLogSink(1)
	executor := localTestExecutor("exit", "0")

	first, err := executor.Start(context.Background(), helperPlan(), sink)
	if err != nil {
		t.Fatal(err)
	}
	if result := first.Wait(); result.Status != RunSucceeded {
		t.Fatalf("first result = %#v", result)
	}

	second, err := executor.Start(context.Background(), helperPlan(), sink)
	if err == nil {
		t.Fatal("reused LogSink Start error = nil")
	}
	if result := second.Wait(); result.Status != RunStartFailed {
		t.Fatalf("reused LogSink result = %#v", result)
	}
}

func TestExecutionConcurrentLogSinkStartsHaveOneOwner(t *testing.T) {
	sink := NewLogSink(1)
	executor := localTestExecutor("exit", "0")
	type outcome struct {
		handle ExecutionHandle
		err    error
	}
	ready := make(chan struct{})
	outcomes := make(chan outcome, 2)
	for range 2 {
		go func() {
			<-ready
			handle, err := executor.Start(context.Background(), helperPlan(), sink)
			outcomes <- outcome{handle: handle, err: err}
		}()
	}
	close(ready)

	owners, rejected := 0, 0
	for range 2 {
		outcome := <-outcomes
		result := outcome.handle.Wait()
		if outcome.err == nil {
			owners++
			if result.Status != RunSucceeded {
				t.Fatalf("owner result = %#v", result)
			}
			continue
		}
		rejected++
		if result.Status != RunStartFailed {
			t.Fatalf("rejected result = %#v", result)
		}
	}
	if owners != 1 || rejected != 1 {
		t.Fatalf("owners=%d rejected=%d, want 1 each", owners, rejected)
	}
}

func TestExecutionStartFailureReleasesLogSinkReservation(t *testing.T) {
	sink := NewLogSink(1)
	executor := newHostExecutor(func(...string) []string {
		return []string{filepath.Join(t.TempDir(), "does-not-exist")}
	})

	failed, err := executor.Start(context.Background(), helperPlan(), sink)
	if err == nil || failed.Wait().Status != RunStartFailed {
		t.Fatalf("failed Start: result=%#v err=%v", failed.Wait(), err)
	}
	executor.hostArgv = func(...string) []string { return helperCommand("exit", "0") }
	retry, err := executor.Start(context.Background(), helperPlan(), sink)
	if err != nil {
		t.Fatalf("retry after command.Start failure: %v", err)
	}
	if result := retry.Wait(); result.Status != RunSucceeded {
		t.Fatalf("retry result = %#v", result)
	}

	reused, err := executor.Start(context.Background(), helperPlan(), sink)
	if err == nil || reused.Wait().Status != RunStartFailed {
		t.Fatalf("reuse after successful retry: result=%#v err=%v", reused.Wait(), err)
	}
}

func TestExecutionReportsOversizedLogLineAndStillReapsProcess(t *testing.T) {
	sink := NewLogSink(1)
	h, err := localTestExecutor("long-line").Start(context.Background(), helperPlan(), sink)
	if err != nil {
		t.Fatal(err)
	}
	result := h.Wait()
	if result.Status != RunSucceeded || result.LogError == nil {
		t.Fatalf("result = %#v, want successful process with visible log error", result)
	}
	logs := drainLogSink(sink)
	if !strings.Contains(logs, "log stream error") {
		t.Fatalf("log stream diagnostic missing: %q", logs)
	}
}

func TestExecutionFullLogSinkNeverBlocksStopOrWait(t *testing.T) {
	requireLinuxProcessGroups(t)
	ready := filepath.Join(t.TempDir(), "ready")
	sink := NewLogSink(1)
	h, err := localTestExecutor("flood-ignore-term", ready).Start(context.Background(), helperPlan(), sink)
	if err != nil {
		t.Fatal(err)
	}
	waitForFile(t, ready)

	stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := h.Stop(stopCtx, 20*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	result := h.Wait()
	if result.Status != RunKilled || result.DroppedLogLines == 0 {
		t.Fatalf("result = %#v, want killed execution with dropped bounded logs", result)
	}
}

// TestHostExecutorHelperProcess is executed in a fresh process by the tests
// above. Its stdout and stderr therefore exercise the real exec/pipe path.
func TestHostExecutorHelperProcess(t *testing.T) {
	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 || separator+1 >= len(os.Args) {
		return
	}
	args := os.Args[separator+1:]
	switch args[0] {
	case "spawn-child":
		term := make(chan os.Signal, 1)
		signalNotifyTERM(term)
		childReady, childPIDPath := args[1], args[2]
		child := exec.Command(os.Args[0], "-test.run=^TestHostExecutorHelperProcess$", "--", "child-ignore-term", childReady)
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			syscall.Exit(91)
		}
		if err := os.WriteFile(childPIDPath, []byte(fmt.Sprintf("%d", child.Process.Pid)), 0o600); err != nil {
			syscall.Exit(92)
		}
		waitForPathInHelper(childReady)
		<-term
		syscall.Exit(0)
	case "child-ignore-term":
		signalIgnoreTERM()
		if err := os.WriteFile(args[1], []byte("ready"), 0o600); err != nil {
			syscall.Exit(93)
		}
		select {}
	case "ignore-term":
		signalIgnoreTERM()
		if err := os.WriteFile(args[1], []byte("ready"), 0o600); err != nil {
			syscall.Exit(94)
		}
		select {}
	case "observe-term":
		term := make(chan os.Signal, 1)
		signalNotifyTERM(term)
		if err := os.WriteFile(args[1], []byte("ready"), 0o600); err != nil {
			syscall.Exit(94)
		}
		<-term
		if err := os.WriteFile(args[2], []byte("term"), 0o600); err != nil {
			syscall.Exit(94)
		}
		select {}
	case "flood-ignore-term":
		signalIgnoreTERM()
		for i := range 256 {
			fmt.Fprintf(os.Stdout, "log-%03d\n", i)
		}
		if err := os.WriteFile(args[1], []byte("ready"), 0o600); err != nil {
			syscall.Exit(94)
		}
		select {}
	case "term-exit":
		term := make(chan os.Signal, 1)
		signalNotifyTERM(term)
		if err := os.WriteFile(args[1], []byte("ready"), 0o600); err != nil {
			syscall.Exit(95)
		}
		<-term
		fmt.Fprintln(os.Stdout, "terminated")
	case "exit":
		code := 0
		if _, err := fmt.Sscanf(args[1], "%d", &code); err != nil {
			syscall.Exit(96)
		}
		syscall.Exit(code)
	case "touch":
		if err := os.WriteFile(args[1], []byte("started"), 0o600); err != nil {
			syscall.Exit(97)
		}
	case "environment":
		environment := os.Environ()
		sort.Strings(environment)
		for _, entry := range environment {
			fmt.Fprintln(os.Stdout, entry)
		}
	case "process-group":
		fmt.Fprintf(os.Stdout, "%d %d\n", os.Getpid(), syscall.Getpgrp())
	case "mixed-logs":
		fmt.Fprint(os.Stdout, "stdout-1\r")
		fmt.Fprint(os.Stderr, "stderr-1\n")
		fmt.Fprint(os.Stdout, "stdout-2\r\n")
		fmt.Fprint(os.Stderr, "tail")
	case "long-line":
		_, _ = os.Stdout.Write(bytes.Repeat([]byte{'x'}, MaxExecutionLogLineBytes+1))
	default:
		syscall.Exit(98)
	}
	syscall.Exit(0)
}

func helperPlan() ExecutionPlan {
	return ExecutionPlan{
		WrapperPath:         "/var/lib/syncbridge/instances/node-a/jobs/7/run.sh",
		LauncherEnvironment: HostLauncherEnvironment(),
		StopGrace:           100 * time.Millisecond,
	}
}

func localTestExecutor(args ...string) *HostExecutor {
	return newHostExecutor(func(...string) []string { return helperCommand(args...) })
}

func helperCommand(args ...string) []string {
	argv := []string{os.Args[0], "-test.run=^TestHostExecutorHelperProcess$", "--"}
	return append(argv, args...)
}

func drainLogSink(sink LogSink) string {
	var logs strings.Builder
	for {
		select {
		case line, ok := <-sink.Lines():
			if !ok {
				return logs.String()
			}
			logs.Write(line)
		default:
			return logs.String()
		}
	}
}

func waitForResult(t *testing.T, h ExecutionHandle, timeout time.Duration) RunResult {
	t.Helper()
	done := make(chan RunResult, 1)
	go func() { done <- h.Wait() }()
	select {
	case result := <-done:
		return result
	case <-time.After(timeout):
		t.Fatal("timed out waiting for execution result")
		return RunResult{}
	}
}

func equalSignals(left, right []syscall.Signal) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func requireLinuxProcessGroups(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("host executor process-group contract is Linux-specific")
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func waitForPathInHelper(path string) {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	syscall.Exit(99)
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	if _, err := fmt.Sscanf(string(b), "%d", &pid); err != nil {
		t.Fatalf("parse child pid %q: %v", b, err)
	}
	return pid
}

func waitForProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if processGoneOrZombie(pid) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("child process %d survived process-group stop", pid)
}

func processGoneOrZombie(pid int) bool {
	err := syscall.Kill(pid, 0)
	if errors.Is(err, syscall.ESRCH) {
		return true
	}
	stat, readErr := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if readErr != nil {
		return errors.Is(readErr, os.ErrNotExist)
	}
	fields := strings.Fields(string(stat))
	return len(fields) > 2 && fields[2] == "Z"
}

func equalStrings(a, b []string) bool {
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

func signalIgnoreTERM() { signal.Ignore(syscall.SIGTERM) }

func signalNotifyTERM(ch chan<- os.Signal) { signal.Notify(ch, syscall.SIGTERM) }
