package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// HostToolPaths contains the exact host executables whose command-line
// semantics were exercised by CapabilityService. These paths are operational
// metadata, not host secrets, and travel with ExecutionPlan.
type HostToolPaths struct {
	Profile  string
	Shell    string
	Test     string
	Readlink string
	Stat     string
	Head     string
	Find     string
	Getent   string
	Setpriv  string
	Env      string
	Mkdir    string
	Chown    string
	Chmod    string
	Mktemp   string
	Tee      string
	Sync     string
	Mv       string
	Rm       string
	Rmdir    string
	Date     string
}

var hostToolCandidates = map[string][]string{
	"shell":    {"/bin/sh", "/usr/bin/sh"},
	"test":     {"/usr/bin/test", "/bin/test"},
	"readlink": {"/usr/bin/readlink", "/bin/readlink"},
	"stat":     {"/usr/bin/stat", "/bin/stat"},
	"head":     {"/usr/bin/head", "/bin/head"},
	"find":     {"/usr/bin/find", "/bin/find"},
	"getent":   {"/usr/bin/getent", "/bin/getent"},
	"setpriv":  {"/usr/bin/setpriv", "/bin/setpriv"},
	"env":      {"/usr/bin/env", "/bin/env"},
	"mkdir":    {"/usr/bin/mkdir", "/bin/mkdir"},
	"chown":    {"/usr/bin/chown", "/bin/chown"},
	"chmod":    {"/usr/bin/chmod", "/bin/chmod"},
	"mktemp":   {"/usr/bin/mktemp", "/bin/mktemp"},
	"tee":      {"/usr/bin/tee", "/bin/tee"},
	"sync":     {"/usr/bin/sync", "/bin/sync"},
	"mv":       {"/usr/bin/mv", "/bin/mv"},
	"rm":       {"/usr/bin/rm", "/bin/rm"},
	"rmdir":    {"/usr/bin/rmdir", "/bin/rmdir"},
	"date":     {"/usr/bin/date", "/bin/date"},
}

// CapabilityService probes the host only through HostCommandRunner.
type CapabilityService struct {
	runner HostCommandRunner
}

func NewCapabilityService(runner HostCommandRunner) *CapabilityService {
	return &CapabilityService{runner: runner}
}

// Probe returns a stable ordered report. Probe output is intentionally never
// returned: host names, filesystem identities, and command errors are sensitive
// deployment data rather than user-facing diagnostics.
func (s *CapabilityService) Probe(ctx context.Context) CapabilityReport {
	toolchain := s.probeHostToolchain(ctx)
	return CapabilityReport{Results: []CapabilityResult{
		s.probe(ctx, CapHostNamespace, []string{"/usr/bin/env", "true"}, nil),
		s.probe(ctx, CapHostPID1, []string{"/usr/bin/test", "-d", "/proc/1"}, nil),
		s.probe(ctx, CapHostHostname, []string{"/usr/bin/env", "hostname"}, map[string]string{"hostname": "present"}),
		s.probe(ctx, CapHostRoot, []string{"/usr/bin/env", "stat", "-c", "%d", "/"}, map[string]string{"rootDevice": "present"}),
		s.probeBoundingSet(ctx),
		s.probe(ctx, CapIdentityDrop, []string{"/usr/bin/env", "setpriv", "--version"}, nil),
		s.probe(ctx, CapAccountLookup, []string{"/usr/bin/env", "getent", "--version"}, nil),
		s.probe(ctx, CapShell, []string{"/bin/sh", "-n"}, nil),
		s.probe(ctx, CapSystemd, []string{"/usr/bin/env", "systemctl", "--version"}, nil),
		s.probe(ctx, CapJournald, []string{"/usr/bin/env", "journalctl", "--version"}, nil),
		s.probeCron(ctx),
		s.probe(ctx, CapRsync, []string{"/usr/bin/env", "rsync", "--version"}, nil),
		s.probe(ctx, CapRclone, []string{"/usr/bin/env", "rclone", "version"}, nil),
		s.probe(ctx, CapSignalControl, []string{"/bin/kill", "-0", "1"}, nil),
		toolchain,
	}}
}

func (s *CapabilityService) probeHostToolchain(ctx context.Context) CapabilityResult {
	tools, err := s.discoverHostTools(ctx)
	if err != nil {
		return unavailableCapability(CapHostToolchain)
	}
	if err := s.exerciseHostTools(ctx, tools); err != nil {
		return unavailableCapability(CapHostToolchain)
	}
	return availableCapability(CapHostToolchain, tools.details())
}

func (s *CapabilityService) discoverHostTools(ctx context.Context) (HostToolPaths, error) {
	testPath := ""
	for _, candidate := range hostToolCandidates["test"] {
		if result := s.runner.Run(ctx, candidate, "-d", "/"); result.Err == nil {
			testPath = candidate
			break
		}
	}
	if testPath == "" {
		return HostToolPaths{}, errors.New("host test utility unavailable")
	}
	resolve := func(name string) (string, error) {
		for _, candidate := range hostToolCandidates[name] {
			if result := s.runner.Run(ctx, testPath, "-x", candidate); result.Err == nil {
				return candidate, nil
			}
		}
		return "", fmt.Errorf("host tool %s unavailable", name)
	}
	values := make(map[string]string, len(hostToolCandidates))
	for _, name := range []string{"shell", "readlink", "stat", "head", "find", "getent", "setpriv", "env", "mkdir", "chown", "chmod", "mktemp", "tee", "sync", "mv", "rm", "rmdir", "date"} {
		path, err := resolve(name)
		if err != nil {
			return HostToolPaths{}, err
		}
		values[name] = path
	}
	profileResult := s.runner.Run(ctx, values["stat"], "--help")
	if profileResult.Err != nil {
		return HostToolPaths{}, errors.New("host stat help unavailable")
	}
	profileHelp := append(bytes.Clone(profileResult.Stdout), profileResult.Stderr...)
	profile := ""
	if bytes.Contains(profileHelp, []byte("BusyBox")) {
		profile = "busybox"
	} else if bytes.Contains(profileHelp, []byte("GNU coreutils")) {
		profile = "gnu"
	} else {
		return HostToolPaths{}, errors.New("host toolchain profile unsupported")
	}
	return HostToolPaths{
		Profile: profile, Shell: values["shell"], Test: testPath,
		Readlink: values["readlink"], Stat: values["stat"], Head: values["head"],
		Find: values["find"], Getent: values["getent"], Setpriv: values["setpriv"],
		Env: values["env"], Mkdir: values["mkdir"], Chown: values["chown"],
		Chmod: values["chmod"], Mktemp: values["mktemp"], Tee: values["tee"],
		Sync: values["sync"], Mv: values["mv"], Rm: values["rm"], Rmdir: values["rmdir"], Date: values["date"],
	}, nil
}

func (s *CapabilityService) exerciseHostTools(ctx context.Context, tools HostToolPaths) error {
	checks := []struct {
		argv         []string
		shortOptions string
		longOptions  []string
		operands     []string
	}{
		{argv: []string{tools.Readlink, "-m", "--", "/"}},
		{argv: []string{tools.Stat, "-c", "%d:%i:%f:%u:%g", "--", "/"}},
		{argv: []string{tools.Stat, "-c", "%f:%u:%g:%a", "--", "/"}},
		{argv: []string{tools.Head, "-c", "2", "--", "/proc/version"}},
		{argv: []string{tools.Find, "/", "-mindepth", "1", "-maxdepth", "1", "-print", "-quit"}},
		{argv: []string{tools.Getent, "passwd", "0"}},
		{argv: []string{tools.Getent, "group", "0"}},
		{argv: []string{tools.Env, "-i", "--", "PATH=" + hostLauncherPATH, "/bin/true"}},
		{argv: []string{tools.Setpriv, "--help"}, longOptions: []string{"--reuid", "--regid", "--init-groups", "--reset-env"}},
		{argv: []string{tools.Mkdir, "--help"}, operands: []string{"DIRECTORY"}},
		{argv: []string{tools.Mktemp, "--help"}, operands: []string{"TEMPLATE"}},
		{argv: []string{tools.Tee, "--help"}, operands: []string{"FILE"}},
		{argv: []string{tools.Sync, "--help"}, shortOptions: "f", operands: []string{"FILE"}},
		{argv: []string{tools.Mv, "--help"}, shortOptions: "Tf", operands: []string{"SOURCE", "DEST"}},
		{argv: []string{tools.Chown, "--help"}, operands: []string{"OWNER", "FILE"}},
		{argv: []string{tools.Chmod, "--help"}, operands: []string{"MODE", "FILE"}},
		{argv: []string{tools.Rm, "--help"}, shortOptions: "f", operands: []string{"FILE"}},
		{argv: []string{tools.Rmdir, "--help"}, operands: []string{"DIRECTORY"}},
		{argv: []string{tools.Date, "-u", "+%Y%m%d-%H%M%S"}},
	}
	for _, check := range checks {
		result := s.runner.Run(ctx, check.argv...)
		if result.Err != nil {
			return fmt.Errorf("host tool semantics unsupported: %s", filepath.Base(check.argv[0]))
		}
		help := string(result.Stdout) + string(result.Stderr)
		for index := 0; index < len(check.shortOptions); index++ {
			option := check.shortOptions[index]
			if !helpSupportsShortOption(help, option) {
				return fmt.Errorf("host tool option unsupported: %s -%c", filepath.Base(check.argv[0]), option)
			}
		}
		for _, option := range check.longOptions {
			if !strings.Contains(help, option) {
				return fmt.Errorf("host tool option unsupported: %s %s", filepath.Base(check.argv[0]), option)
			}
		}
		for _, operand := range check.operands {
			if !strings.Contains(help, operand) {
				return fmt.Errorf("host tool operand unsupported: %s %s", filepath.Base(check.argv[0]), operand)
			}
		}
	}
	return nil
}

func helpSupportsShortOption(help string, option byte) bool {
	for index := 0; index+1 < len(help); index++ {
		if help[index] == '-' && help[index+1] == option && (index == 0 || help[index-1] != '-') {
			return true
		}
	}
	for start := 0; start < len(help); start++ {
		if help[start] != '[' {
			continue
		}
		end := strings.IndexByte(help[start:], ']')
		if end < 0 {
			continue
		}
		group := help[start : start+end+1]
		if strings.HasPrefix(group, "[-") && strings.ContainsRune(group[2:], rune(option)) {
			return true
		}
		start += end
	}
	return false
}

func (t HostToolPaths) details() map[string]string {
	return map[string]string{
		"profile": t.Profile, "shell": t.Shell, "test": t.Test,
		"readlink": t.Readlink, "stat": t.Stat, "head": t.Head, "find": t.Find,
		"getent": t.Getent, "setpriv": t.Setpriv, "env": t.Env,
		"mkdir": t.Mkdir, "chown": t.Chown, "chmod": t.Chmod,
		"mktemp": t.Mktemp, "tee": t.Tee, "sync": t.Sync,
		"mv": t.Mv, "rm": t.Rm, "rmdir": t.Rmdir, "date": t.Date,
	}
}

func (r CapabilityReport) HostTools() (HostToolPaths, error) {
	result := r.ByCode(CapHostToolchain)
	if result.Status != CapabilityAvailable {
		return HostToolPaths{}, errors.New("host toolchain capability unavailable")
	}
	tool := func(name string) (string, error) {
		value := result.Details[name]
		if !filepath.IsAbs(value) || filepath.Clean(value) != value {
			return "", fmt.Errorf("host tool path %s is invalid", name)
		}
		return value, nil
	}
	values := make(map[string]string, 19)
	for _, name := range []string{"shell", "test", "readlink", "stat", "head", "find", "getent", "setpriv", "env", "mkdir", "chown", "chmod", "mktemp", "tee", "sync", "mv", "rm", "rmdir", "date"} {
		value, err := tool(name)
		if err != nil {
			return HostToolPaths{}, err
		}
		values[name] = value
	}
	profile := result.Details["profile"]
	if profile != "gnu" && profile != "busybox" {
		return HostToolPaths{}, errors.New("host toolchain profile is invalid")
	}
	return HostToolPaths{
		Profile: profile, Shell: values["shell"], Test: values["test"],
		Readlink: values["readlink"], Stat: values["stat"], Head: values["head"],
		Find: values["find"], Getent: values["getent"], Setpriv: values["setpriv"],
		Env: values["env"], Mkdir: values["mkdir"], Chown: values["chown"],
		Chmod: values["chmod"], Mktemp: values["mktemp"], Tee: values["tee"],
		Sync: values["sync"], Mv: values["mv"], Rm: values["rm"], Rmdir: values["rmdir"], Date: values["date"],
	}, nil
}

func (s *CapabilityService) probeCron(ctx context.Context) CapabilityResult {
	if result := s.runner.Run(ctx, "/usr/bin/test", "-x", "/usr/sbin/cron"); result.Err == nil {
		return availableCapability(CapCron, map[string]string{"implementation": "vixie"})
	}
	if result := s.runner.Run(ctx, "/usr/bin/env", "crond", "--help"); result.Err == nil && bytes.Contains(result.Stdout, []byte("BusyBox")) {
		return availableCapability(CapCron, map[string]string{"implementation": "busybox"})
	}
	if result := s.runner.Run(ctx, "/usr/bin/env", "crond", "-V"); result.Err == nil {
		return availableCapability(CapCron, map[string]string{"implementation": "cronie"})
	}
	return unavailableCapability(CapCron)
}

func (s *CapabilityService) probe(ctx context.Context, code CapabilityCode, argv []string, details map[string]string) CapabilityResult {
	result := s.runner.Run(ctx, argv...)
	if result.Err != nil {
		return unavailableCapability(code)
	}
	return availableCapability(code, details)
}

func (s *CapabilityService) probeBoundingSet(ctx context.Context) CapabilityResult {
	result := s.runner.Run(ctx, "/usr/bin/env", "grep", "^CapBnd:", "/proc/self/status")
	if result.Err != nil || !bytes.HasPrefix(result.Stdout, []byte("CapBnd:")) {
		return unavailableCapability(CapCapabilityBounding)
	}
	return availableCapability(CapCapabilityBounding, map[string]string{"boundingSet": "present"})
}

func availableCapability(code CapabilityCode, details map[string]string) CapabilityResult {
	return CapabilityResult{
		Code:       code,
		Status:     CapabilityAvailable,
		MessageKey: string(code) + ".available",
		Details:    details,
	}
}

func unavailableCapability(code CapabilityCode) CapabilityResult {
	return CapabilityResult{
		Code:       code,
		Status:     CapabilityUnavailable,
		MessageKey: string(code) + ".unavailable",
		Details:    map[string]string{"reason": "probe_failed"},
	}
}

// ByCode returns the matching stable capability result, or an unavailable
// synthetic result for an unknown code.
func (r CapabilityReport) ByCode(code CapabilityCode) CapabilityResult {
	for _, result := range r.Results {
		if result.Code == code {
			return result
		}
	}
	return unavailableCapability(code)
}

// Require reports every requested capability that is not available.
func (r CapabilityReport) Require(codes ...CapabilityCode) error {
	missing := make([]string, 0, len(codes))
	for _, code := range codes {
		if r.ByCode(code).Status != CapabilityAvailable {
			missing = append(missing, string(code))
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("required capabilities unavailable: %s", strings.Join(missing, ", "))
}
