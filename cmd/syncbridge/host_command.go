package main

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
)

const hostLauncherPATH = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// HostLauncherEnvironment is the complete environment permitted to cross the
// nsenter process boundary. Returning a fresh slice prevents callers from
// mutating the environment used by later host commands or executions.
func HostLauncherEnvironment() []string {
	return []string{
		"HOME=/root",
		"LANG=C",
		"LC_ALL=C",
		"LOGNAME=root",
		"PATH=" + hostLauncherPATH,
		"SHELL=/bin/sh",
		"TZ=UTC",
		"USER=root",
	}
}

// CommandResult is the captured outcome of one host command.
type CommandResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
	Err      error
}

// HostCommandRunner executes direct argv in the host namespaces. Implementations
// must launch the namespace-entry process with HostLauncherEnvironment and no
// inherited variables. RunInput is deliberately byte-oriented so callers can
// feed host tools without building a shell command from input data.
type HostCommandRunner interface {
	Run(ctx context.Context, argv ...string) CommandResult
	RunInput(ctx context.Context, input []byte, argv ...string) CommandResult
}

// HostArgv prefixes a host command with the exact production namespace entry.
func HostArgv(command ...string) []string {
	base := []string{"nsenter", "--target", "1", "--mount", "--uts", "--ipc", "--net", "--pid", "--root", "--wd=/", "--"}
	return append(base, command...)
}

// NewHostCommandRunner returns the production host namespace command runner.
func NewHostCommandRunner() HostCommandRunner {
	return newHostCommandRunner(HostArgv)
}

type hostCommandRunner struct {
	hostArgv func(...string) []string
}

func newHostCommandRunner(hostArgv func(...string) []string) *hostCommandRunner {
	return &hostCommandRunner{hostArgv: hostArgv}
}

func (r *hostCommandRunner) Run(ctx context.Context, argv ...string) CommandResult {
	return r.RunInput(ctx, nil, argv...)
}

func (r *hostCommandRunner) RunInput(ctx context.Context, input []byte, argv ...string) CommandResult {
	commandArgv := r.hostArgv(argv...)
	if len(commandArgv) == 0 {
		return CommandResult{Err: errors.New("host command argv is empty")}
	}

	command := exec.CommandContext(ctx, commandArgv[0], commandArgv[1:]...)
	command.Env = HostLauncherEnvironment()
	command.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	result := CommandResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), Err: err}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		result.ExitCode = exitError.ExitCode()
	}
	return result
}
