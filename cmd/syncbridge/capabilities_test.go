package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestHostArgvNeverUsesShell(t *testing.T) {
	got := HostArgv("/usr/bin/printf", "%s", "a; touch /tmp/never")
	want := []string{"nsenter", "--target", "1", "--mount", "--uts", "--ipc", "--net", "--pid", "--root", "--wd=/", "--", "/usr/bin/printf", "%s", "a; touch /tmp/never"}
	if !slices.Equal(want, got) {
		t.Fatalf("want %q, got %q", want, got)
	}
}

func TestHostCommandRunnerRunInputPassesOpaqueBytesWithoutAShell(t *testing.T) {
	const marker = "host-runner-input-helper"
	if slices.Contains(os.Args, marker) {
		if got, want := os.Args[len(os.Args)-1], "a; touch /tmp/never"; got != want {
			os.Exit(2)
		}
		input, err := os.ReadFile("/dev/stdin")
		if err != nil {
			os.Exit(3)
		}
		_, _ = os.Stdout.Write(input)
		os.Exit(0)
	}

	runner := newHostCommandRunner(func(_ ...string) []string {
		return []string{os.Args[0], "-test.run=TestHostCommandRunnerRunInputPassesOpaqueBytesWithoutAShell", "--", marker, "a; touch /tmp/never"}
	})
	input := []byte("opaque; $(touch /tmp/never)\x00bytes")
	result := runner.RunInput(context.Background(), input, "ignored")
	if result.Err != nil {
		t.Fatalf("RunInput: %v", result.Err)
	}
	if !bytes.Equal(result.Stdout, input) {
		t.Fatalf("want %q, got %q", input, result.Stdout)
	}
}

func TestHostCommandRunnerUsesOnlyExplicitLauncherEnvironment(t *testing.T) {
	const marker = "host-launcher-environment-helper"
	if slices.Contains(os.Args, marker) {
		want := []string{
			"HOME=/root",
			"LANG=C",
			"LC_ALL=C",
			"LOGNAME=root",
			"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
			"SHELL=/bin/sh",
			"TZ=UTC",
			"USER=root",
		}
		got := os.Environ()
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			_, _ = os.Stderr.WriteString("unexpected launcher environment: " + strings.Join(got, ","))
			os.Exit(2)
		}
		os.Exit(0)
	}

	t.Setenv("SYNCBRIDGE_CONTAINER_SECRET", "must-not-cross-nsenter")
	runner := newHostCommandRunner(func(_ ...string) []string {
		return []string{os.Args[0], "-test.run=TestHostCommandRunnerUsesOnlyExplicitLauncherEnvironment", "--", marker}
	})
	result := runner.Run(context.Background(), "/usr/bin/true")
	if result.Err != nil {
		t.Fatalf("host runner inherited parent environment: %v: %s", result.Err, result.Stderr)
	}
}

func TestCapabilitiesHostToolchainProfiles(t *testing.T) {
	for _, profile := range []string{"gnu", "busybox"} {
		t.Run(profile, func(t *testing.T) {
			runner := newSemanticToolchainRunner(profile)
			report := NewCapabilityService(runner).Probe(context.Background())
			result := report.ByCode(CapHostToolchain)
			if result.Status != CapabilityAvailable || result.Details["profile"] != profile {
				t.Fatalf("host toolchain result = %#v; argv=%#v", result, runner.argvs)
			}
			got, err := report.HostTools()
			if err != nil {
				t.Fatal(err)
			}
			want := semanticToolPaths(profile)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("host tools = %#v, want %#v", got, want)
			}
			for _, required := range [][]string{
				{want.Readlink, "-m", "--", "/"},
				{want.Stat, "-c", "%f:%u:%g:%a", "--", "/"},
				{want.Head, "-c", "2", "--", "/proc/version"},
				{want.Find, "/", "-mindepth", "1", "-maxdepth", "1", "-print", "-quit"},
				{want.Mkdir, "--help"},
				{want.Mktemp, "--help"},
				{want.Tee, "--help"},
				{want.Sync, "--help"},
				{want.Mv, "--help"},
				{want.Chown, "--help"},
				{want.Chmod, "--help"},
				{want.Rm, "--help"},
			} {
				if !containsArgv(runner.argvs, required) {
					t.Fatalf("toolchain probe did not exercise store operation %#v", required)
				}
			}
			for _, argv := range runner.argvs {
				for index, arg := range argv[:len(argv)-1] {
					if (arg == "sh" || strings.HasSuffix(arg, "/sh")) && argv[index+1] == "-c" {
						t.Fatalf("toolchain probe composed a shell command: %#v", argv)
					}
				}
			}
		})
	}
}

func TestCapabilitiesBlockUnknownHostToolProfile(t *testing.T) {
	runner := newSemanticToolchainRunner("gnu")
	runner.statHelp = "vendor stat implementation\n"

	report := NewCapabilityService(runner).Probe(context.Background())
	if got := report.ByCode(CapHostToolchain); got.Status != CapabilityUnavailable {
		t.Fatalf("unknown host tool profile = %#v; argv=%#v", got, runner.argvs)
	}
}

func TestCapabilitiesHostToolchainProbeNeverMutatesForHostileToolOutput(t *testing.T) {
	runner := newSemanticToolchainRunner("gnu")
	runner.hostileMktempOutput = "/etc/syncbridge-probe-target\n"
	report := NewCapabilityService(runner).Probe(context.Background())
	if got := report.ByCode(CapHostToolchain); got.Status != CapabilityUnavailable {
		t.Fatalf("read-only host toolchain report = %#v; argv=%#v", got, runner.argvs)
	}
	if _, err := report.HostTools(); err == nil {
		t.Fatal("invalid mktemp help exposed host tool paths")
	}
	for _, argv := range runner.argvs {
		if mutatingToolchainProbe(argv, runner.paths) {
			t.Fatalf("host toolchain probe emitted mutating argv %#v", argv)
		}
		for _, arg := range argv {
			if arg == "/etc/syncbridge-probe-target" {
				t.Fatalf("hostile tool output was reused as an argv target: %#v", argv)
			}
		}
	}
}

func mutatingToolchainProbe(argv []string, tools HostToolPaths) bool {
	if len(argv) == 0 {
		return false
	}
	for _, tool := range []string{tools.Setpriv, tools.Mkdir, tools.Mktemp, tools.Tee, tools.Sync, tools.Mv, tools.Chown, tools.Chmod, tools.Rm, tools.Rmdir} {
		if argv[0] == tool {
			return !slices.Equal(argv, []string{tool, "--help"})
		}
	}
	return false
}

func containsArgv(values [][]string, want []string) bool {
	for _, value := range values {
		if slices.Equal(value, want) {
			return true
		}
	}
	return false
}

func TestCapabilitiesBlockHostToolchainWithoutStoreSyntaxContract(t *testing.T) {
	contracts := []string{
		"mkdir_directory", "mktemp_template", "tee_file",
		"sync_force", "sync_file",
		"mv_no_target_directory", "mv_force", "mv_source", "mv_dest",
		"chown_owner", "chown_file", "chmod_mode", "chmod_file",
		"rm_force", "rm_file", "rmdir_directory",
	}
	for _, profile := range []string{"gnu", "busybox"} {
		for _, contract := range contracts {
			t.Run(profile+"/"+contract, func(t *testing.T) {
				runner := newSemanticToolchainRunner(profile)
				runner.missingContract = contract
				report := NewCapabilityService(runner).Probe(context.Background())
				if got := report.ByCode(CapHostToolchain); got.Status != CapabilityUnavailable {
					t.Fatalf("toolchain missing %s = %#v; argv=%#v", contract, got, runner.argvs)
				}
				if _, err := report.HostTools(); err == nil {
					t.Fatal("unavailable toolchain exposed host tool paths")
				}
			})
		}
	}
}

func TestCapabilitiesBlocksWhenSetprivMissing(t *testing.T) {
	runner := &fakeHostRunner{missingExecutables: map[string]bool{"setpriv": true}}
	report := NewCapabilityService(runner).Probe(context.Background())
	if got := report.ByCode(CapIdentityDrop).Status; got != CapabilityUnavailable {
		t.Fatalf("want %q, got %q", CapabilityUnavailable, got)
	}
}

func TestCapabilitiesProbeCronVariantMatrix(t *testing.T) {
	tests := []struct {
		name        string
		results     map[string]CommandResult
		wantStatus  CapabilityStatus
		wantVariant string
	}{
		{
			name: "Debian Ubuntu Vixie cron",
			results: map[string]CommandResult{
				commandKey("/usr/bin/test", "-x", "/usr/sbin/cron"): {},
			},
			wantStatus:  CapabilityAvailable,
			wantVariant: "vixie",
		},
		{
			name: "Alpine BusyBox crond after cron is absent",
			results: map[string]CommandResult{
				commandKey("/usr/bin/env", "crond", "--help"): {Stdout: []byte("BusyBox v1.36\n")},
			},
			wantStatus:  CapabilityAvailable,
			wantVariant: "busybox",
		},
		{
			name: "Fedora RHEL Cronie crond after earlier variants are absent",
			results: map[string]CommandResult{
				commandKey("/usr/bin/env", "crond", "-V"): {Stdout: []byte("cronie 1.7\n")},
			},
			wantStatus:  CapabilityAvailable,
			wantVariant: "cronie",
		},
		{
			name:       "all cron variants absent",
			results:    map[string]CommandResult{},
			wantStatus: CapabilityUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &fakeHostRunner{runResults: tt.results, strictResults: true}
			report := NewCapabilityService(runner).Probe(context.Background())
			got := report.ByCode(CapCron)
			if got.Status != tt.wantStatus {
				t.Fatalf("status: want %q, got %#v", tt.wantStatus, got)
			}
			if got.Details["implementation"] != tt.wantVariant {
				t.Fatalf("implementation: want %q, got %#v", tt.wantVariant, got)
			}
			if tt.wantStatus == CapabilityAvailable {
				if err := report.Require(CapCron); err != nil {
					t.Fatalf("compatible cron rejected: %v", err)
				}
			}
		})
	}
}

func TestCapabilitiesProbeUsesDirectHostArgvAndRedactsHostDetails(t *testing.T) {
	runner := &fakeHostRunner{}
	report := NewCapabilityService(runner).Probe(context.Background())
	if got := report.ByCode(CapHostHostname); got.Status != CapabilityAvailable || got.Details["hostname"] != "present" {
		t.Fatalf("hostname result: %#v", got)
	}
	if got := report.ByCode(CapHostRoot); got.Status != CapabilityAvailable || got.Details["rootDevice"] != "present" {
		t.Fatalf("root result: %#v", got)
	}
	for _, argv := range runner.argvs {
		for i, arg := range argv[:len(argv)-1] {
			if (arg == "sh" || strings.HasSuffix(arg, "/sh")) && argv[i+1] == "-c" {
				t.Fatalf("probe must use a direct argv, got %q", argv)
			}
		}
	}
	if err := report.Require(CapHostNamespace, CapIdentityDrop); err != nil {
		t.Fatalf("available capabilities rejected: %v", err)
	}
}

func TestCapabilityReportRequireNamesUnavailableCodes(t *testing.T) {
	report := CapabilityReport{Results: []CapabilityResult{{Code: CapIdentityDrop, Status: CapabilityUnavailable}}}
	if err := report.Require(CapIdentityDrop); err == nil || err.Error() != "required capabilities unavailable: identity_drop" {
		t.Fatalf("unexpected error: %v", err)
	}
}

type fakeHostRunner struct {
	missingExecutables map[string]bool
	argvs              [][]string
	runResults         map[string]CommandResult
	strictResults      bool
}

type semanticToolchainRunner struct {
	profile             string
	missingContract     string
	hostileMktempOutput string
	statHelp            string
	paths               HostToolPaths
	argvs               [][]string
}

func newSemanticToolchainRunner(profile string) *semanticToolchainRunner {
	return &semanticToolchainRunner{profile: profile, paths: semanticToolPaths(profile)}
}

func semanticToolPaths(profile string) HostToolPaths {
	prefix := "/usr/bin/"
	if profile == "busybox" {
		prefix = "/bin/"
	}
	return HostToolPaths{
		Profile:  profile,
		Shell:    "/bin/sh",
		Test:     prefix + "test",
		Readlink: prefix + "readlink",
		Stat:     prefix + "stat",
		Head:     prefix + "head",
		Find:     prefix + "find",
		Getent:   prefix + "getent",
		Setpriv:  prefix + "setpriv",
		Env:      prefix + "env",
		Mkdir:    prefix + "mkdir",
		Chown:    prefix + "chown",
		Chmod:    prefix + "chmod",
		Mktemp:   prefix + "mktemp",
		Tee:      prefix + "tee",
		Sync:     prefix + "sync",
		Mv:       prefix + "mv",
		Rm:       prefix + "rm",
		Rmdir:    prefix + "rmdir",
		Date:     prefix + "date",
	}
}

func (r *semanticToolchainRunner) Run(ctx context.Context, argv ...string) CommandResult {
	return r.RunInput(ctx, nil, argv...)
}

func (r *semanticToolchainRunner) RunInput(_ context.Context, _ []byte, argv ...string) CommandResult {
	r.argvs = append(r.argvs, append([]string(nil), argv...))
	if len(argv) == 0 {
		return CommandResult{Err: errors.New("empty argv")}
	}
	if argv[0] == "/usr/bin/test" || argv[0] == "/bin/test" {
		if argv[0] != r.paths.Test {
			return CommandResult{ExitCode: 127, Err: errors.New("test tool absent")}
		}
		if len(argv) == 3 && argv[1] == "-d" && argv[2] == "/" {
			return CommandResult{}
		}
	}
	if argv[0] == r.paths.Test && len(argv) == 3 && argv[1] == "-x" {
		if slices.Contains(hostToolPathValues(r.paths), argv[2]) {
			return CommandResult{}
		}
		return CommandResult{ExitCode: 1, Err: errors.New("not executable")}
	}
	if argv[0] == r.paths.Mktemp {
		if slices.Contains(argv, "--help") && r.hostileMktempOutput != "" {
			return CommandResult{Stdout: []byte(r.hostileMktempOutput)}
		}
		return CommandResult{Stdout: []byte(r.toolHelp("mktemp"))}
	}
	if argv[0] == r.paths.Readlink {
		return CommandResult{Stdout: []byte("/\n")}
	}
	if argv[0] == r.paths.Stat && slices.Contains(argv, "--help") {
		if r.statHelp != "" {
			return CommandResult{Stdout: []byte(r.statHelp)}
		}
		if r.profile == "busybox" {
			return CommandResult{Stdout: []byte("BusyBox v1.37 stat\n")}
		}
		return CommandResult{Stdout: []byte("GNU coreutils stat\n")}
	}
	if argv[0] == r.paths.Stat {
		return CommandResult{Stdout: []byte("1:2:81c0:0:0\n")}
	}
	if argv[0] == r.paths.Getent && len(argv) >= 3 {
		if argv[1] == "passwd" {
			return CommandResult{Stdout: []byte("root:x:0:0:root:/root:/bin/sh\n")}
		}
		return CommandResult{Stdout: []byte("root:x:0:\n")}
	}
	if argv[0] == r.paths.Setpriv && slices.Contains(argv, "--help") {
		return CommandResult{Stdout: []byte("usage: setpriv --reuid UID --regid GID --init-groups --reset-env\n")}
	}
	for name, tool := range map[string]string{
		"mkdir": r.paths.Mkdir, "tee": r.paths.Tee, "sync": r.paths.Sync,
		"mv": r.paths.Mv, "chown": r.paths.Chown, "chmod": r.paths.Chmod,
		"rm": r.paths.Rm, "rmdir": r.paths.Rmdir,
	} {
		if argv[0] == tool && slices.Contains(argv, "--help") {
			return CommandResult{Stdout: []byte(r.toolHelp(name))}
		}
	}
	if strings.HasSuffix(argv[0], "/grep") {
		return CommandResult{Stdout: []byte("CapBnd: 000001ffffffffff\n")}
	}
	return CommandResult{Stdout: []byte("probe-ok\n")}
}

func (r *semanticToolchainRunner) toolHelp(tool string) string {
	help := map[string]map[string]string{
		"gnu": {
			"mkdir":  "Usage: mkdir [OPTION]... DIRECTORY...\n",
			"mktemp": "Usage: mktemp [OPTION]... [TEMPLATE]\n",
			"tee":    "Usage: tee [OPTION]... [FILE]...\n",
			"sync":   "Usage: sync [OPTION] [FILE]...\n  -f, --file-system  sync the containing file systems\n",
			"mv":     "Usage: mv [OPTION]... [-T] SOURCE DEST\n  -f, --force\n  -T, --no-target-directory\n",
			"chown":  "Usage: chown [OPTION]... OWNER FILE...\n",
			"chmod":  "Usage: chmod [OPTION]... MODE FILE...\n",
			"rm":     "Usage: rm [OPTION]... [FILE]...\n  -f, --force\n",
			"rmdir":  "Usage: rmdir [OPTION]... DIRECTORY...\n",
		},
		"busybox": {
			"mkdir":  "Usage: mkdir [-m MODE] [-p] DIRECTORY...\n",
			"mktemp": "Usage: mktemp [-dt] [-p DIR] [TEMPLATE]\n",
			"tee":    "Usage: tee [-ai] [FILE]...\n",
			"sync":   "Usage: sync [-df] [FILE]...\n",
			"mv":     "Usage: mv [-finT] SOURCE DEST\n",
			"chown":  "Usage: chown [-RhLHPcvf] OWNER FILE...\n",
			"chmod":  "Usage: chmod [-Rcvf] MODE FILE...\n",
			"rm":     "Usage: rm [-irf] FILE...\n",
			"rmdir":  "Usage: rmdir [-p] DIRECTORY...\n",
		},
	}[r.profile][tool]
	replacements := map[string]map[string]string{
		"mkdir_directory":        {"DIRECTORY": ""},
		"mktemp_template":        {"TEMPLATE": ""},
		"tee_file":               {"FILE": ""},
		"sync_force":             {"  -f,": "  ,", "[-df]": "[-d]"},
		"sync_file":              {"FILE": ""},
		"mv_no_target_directory": {"-T": "", "[-finT]": "[-fin]"},
		"mv_force":               {" -f": "", "[-finT]": "[-inT]"},
		"mv_source":              {"SOURCE": ""},
		"mv_dest":                {"DEST": ""},
		"chown_owner":            {"OWNER": ""},
		"chown_file":             {"FILE": ""},
		"chmod_mode":             {"MODE": ""},
		"chmod_file":             {"FILE": ""},
		"rm_force":               {"  -f,": "  ,", "[-irf]": "[-ir]"},
		"rm_file":                {"FILE": ""},
		"rmdir_directory":        {"DIRECTORY": ""},
	}
	for old, replacement := range replacements[r.missingContract] {
		help = strings.ReplaceAll(help, old, replacement)
	}
	return help
}

func hostToolPathValues(tools HostToolPaths) []string {
	return []string{
		tools.Shell, tools.Test, tools.Readlink, tools.Stat, tools.Head, tools.Find,
		tools.Getent, tools.Setpriv, tools.Env, tools.Mkdir, tools.Chown, tools.Chmod,
		tools.Mktemp, tools.Tee, tools.Sync, tools.Mv, tools.Rm, tools.Rmdir, tools.Date,
	}
}

func (r *fakeHostRunner) Run(ctx context.Context, argv ...string) CommandResult {
	return r.RunInput(ctx, nil, argv...)
}

func (r *fakeHostRunner) RunInput(_ context.Context, _ []byte, argv ...string) CommandResult {
	r.argvs = append(r.argvs, append([]string(nil), argv...))
	if result, ok := r.runResults[commandKey(argv...)]; ok {
		return result
	}
	if r.strictResults {
		return CommandResult{ExitCode: 127, Err: errors.New("missing executable")}
	}
	for _, arg := range argv {
		if r.missingExecutables[arg] {
			return CommandResult{ExitCode: 127, Err: errors.New("missing executable")}
		}
	}
	return CommandResult{Stdout: []byte("redacted-host-value\n")}
}

func commandKey(argv ...string) string {
	return strings.Join(argv, "\x00")
}
