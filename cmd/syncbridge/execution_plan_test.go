package main

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestCompileScriptOwnerResolvesAtRunTime(t *testing.T) {
	job := validScriptJob("/srv/scripts/backup.sh")
	plan, err := newTestCompiler(validScriptInspector()).Compile(context.Background(), job, RunRequest{RunID: "run-1", Origin: RunOriginManual})
	if err != nil {
		t.Fatal(err)
	}

	if plan.Identity.Mode != IdentityScriptOwner {
		t.Fatalf("identity = %#v, want script-owner policy", plan.Identity)
	}
	if want := []string{"/srv/scripts/backup.sh", "--target", "daily;$(touch /tmp/pwn)"}; !reflect.DeepEqual(plan.Argv, want) {
		t.Fatalf("argv = %#v, want %#v", plan.Argv, want)
	}
	if plan.WrapperPath != "/var/lib/syncbridge/instances/node-a/jobs/7/run.sh" {
		t.Fatalf("wrapper path = %q", plan.WrapperPath)
	}
	if plan.Timeout != 2*time.Minute || plan.StopGrace != 15*time.Second || plan.Umask != 0o027 {
		t.Fatalf("execution policy = %#v", plan)
	}
}

func TestCompileUsesDirectArgvForEachAction(t *testing.T) {
	tests := []struct {
		name string
		job  Job
		want []string
	}{
		{
			name: "command is the only shell action",
			job: validFixedJob(Action{
				Type:    ActionCommand,
				Command: "printf '%s\\n' \"$HOME\"; touch /tmp/intentional",
			}),
			want: []string{"/bin/sh", "-c", "printf '%s\\n' \"$HOME\"; touch /tmp/intentional"},
		},
		{
			name: "rsync is direct",
			job: validFixedJob(Action{
				Type: ActionSync,
				Sync: SyncAction{Engine: "rsync", Source: "/srv/source", Dest: "/srv/destination", Mode: "mirror"},
			}),
			want: []string{"rsync", "-a", "--info=progress2,stats2", "--human-readable", "--delete", "--", "/srv/source/", "/srv/destination/"},
		},
		{
			name: "rclone is direct",
			job: validFixedJob(Action{
				Type: ActionSync,
				Sync: SyncAction{Engine: "rclone", Source: "/srv/source", Dest: "/srv/destination", Mode: "move"},
			}),
			want: []string{"rclone", "move", "/srv/source", "/srv/destination", "--progress", "--stats=1s", "--stats-one-line"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := newTestCompiler(validFixedInspector()).Compile(context.Background(), tt.job, RunRequest{RunID: "run-2", Origin: RunOriginManual})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(plan.Argv, tt.want) {
				t.Fatalf("argv = %#v, want %#v", plan.Argv, tt.want)
			}
		})
	}
}

func TestCompileRejectsUnsafeScriptAndWorkingDirectory(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Job, *fakeHostInspector)
	}{
		{name: "relative script", mutate: func(job *Job, _ *fakeHostInspector) { job.Action.ScriptPath = "srv/backup.sh" }},
		{name: "lexically noncanonical script", mutate: func(job *Job, _ *fakeHostInspector) { job.Action.ScriptPath = "/srv/scripts/../backup.sh" }},
		{name: "host canonical mismatch", mutate: func(_ *Job, inspector *fakeHostInspector) {
			info := inspector.paths["/srv/scripts/backup.sh"]
			info.CanonicalPath = "/real/backup.sh"
			inspector.paths["/srv/scripts/backup.sh"] = info
		}},
		{name: "not regular", mutate: func(_ *Job, inspector *fakeHostInspector) {
			info := inspector.paths["/srv/scripts/backup.sh"]
			info.Regular = false
			info.Directory = true
			inspector.paths["/srv/scripts/backup.sh"] = info
		}},
		{name: "not executable", mutate: func(_ *Job, inspector *fakeHostInspector) {
			info := inspector.paths["/srv/scripts/backup.sh"]
			info.Executable = false
			inspector.paths["/srv/scripts/backup.sh"] = info
		}},
		{name: "missing shebang", mutate: func(_ *Job, inspector *fakeHostInspector) {
			info := inspector.paths["/srv/scripts/backup.sh"]
			info.HasShebang = false
			inspector.paths["/srv/scripts/backup.sh"] = info
		}},
		{name: "relative working directory", mutate: func(job *Job, _ *fakeHostInspector) { job.Execution.WorkingDirectory = "srv/scripts" }},
		{name: "working directory is a file", mutate: func(_ *Job, inspector *fakeHostInspector) {
			info := inspector.paths["/srv/scripts"]
			info.Directory = false
			info.Regular = true
			inspector.paths["/srv/scripts"] = info
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := validScriptJob("/srv/scripts/backup.sh")
			inspector := validScriptInspector()
			tt.mutate(&job, inspector)
			if _, err := newTestCompiler(inspector).Compile(context.Background(), job, RunRequest{RunID: "run-3", Origin: RunOriginManual}); err == nil {
				t.Fatal("unsafe plan compiled successfully")
			}
		})
	}
}

func TestCompileRejectsIncoherentIdentity(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Job, *fakeHostInspector)
	}{
		{name: "command cannot follow script owner", mutate: func(job *Job, _ *fakeHostInspector) { job.Identity = Identity{Mode: IdentityScriptOwner} }},
		{name: "fixed user uid mismatch", mutate: func(_ *Job, inspector *fakeHostInspector) {
			inspector.users["backup"] = HostUser{Name: "backup", UID: 2001, GID: 1002, Home: "/home/backup", Shell: "/bin/sh"}
		}},
		{name: "fixed group gid mismatch", mutate: func(_ *Job, inspector *fakeHostInspector) {
			inspector.groups["backup"] = HostGroup{Name: "backup", GID: 2002}
		}},
		{name: "script uid has no account", mutate: func(_ *Job, inspector *fakeHostInspector) { delete(inspector.users, "1001") }},
		{name: "script gid has no group", mutate: func(_ *Job, inspector *fakeHostInspector) { delete(inspector.groups, "1002") }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var job Job
			var inspector *fakeHostInspector
			if strings.HasPrefix(tt.name, "script") {
				job = validScriptJob("/srv/scripts/backup.sh")
				inspector = validScriptInspector()
			} else {
				job = validFixedJob(Action{Type: ActionCommand, Command: "id"})
				inspector = validFixedInspector()
			}
			tt.mutate(&job, inspector)
			if _, err := newTestCompiler(inspector).Compile(context.Background(), job, RunRequest{RunID: "run-4", Origin: RunOriginManual}); err == nil {
				t.Fatal("incoherent identity compiled successfully")
			}
		})
	}
}

func TestCompileRejectsUnsafeSyncPaths(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Job, *fakeHostInspector)
	}{
		{name: "relative source", mutate: func(job *Job, _ *fakeHostInspector) { job.Action.Sync.Source = "srv/source" }},
		{name: "same source and destination", mutate: func(job *Job, _ *fakeHostInspector) { job.Action.Sync.Dest = "/srv/source" }},
		{name: "destination nested in source", mutate: func(job *Job, inspector *fakeHostInspector) {
			job.Action.Sync.Dest = "/srv/source/archive"
			inspector.paths["/srv/source/archive"] = HostPathInfo{CanonicalPath: "/srv/source/archive", Exists: false}
		}},
		{name: "destination is root ancestor", mutate: func(job *Job, _ *fakeHostInspector) { job.Action.Sync.Dest = "/" }},
		{name: "destination is immediate ancestor", mutate: func(job *Job, _ *fakeHostInspector) { job.Action.Sync.Dest = "/srv" }},
		{name: "destructive source missing", mutate: func(_ *Job, inspector *fakeHostInspector) {
			info := inspector.paths["/srv/source"]
			info.Exists = false
			info.Directory = false
			inspector.paths["/srv/source"] = info
		}},
		{name: "destructive source empty", mutate: func(_ *Job, inspector *fakeHostInspector) {
			info := inspector.paths["/srv/source"]
			info.Empty = true
			inspector.paths["/srv/source"] = info
		}},
		{name: "source is not directory", mutate: func(_ *Job, inspector *fakeHostInspector) {
			info := inspector.paths["/srv/source"]
			info.Directory = false
			info.Regular = true
			inspector.paths["/srv/source"] = info
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := validFixedJob(Action{Type: ActionSync, Sync: SyncAction{Engine: "rsync", Source: "/srv/source", Dest: "/srv/destination", Mode: "mirror"}})
			inspector := validFixedInspector()
			tt.mutate(&job, inspector)
			if _, err := newTestCompiler(inspector).Compile(context.Background(), job, RunRequest{RunID: "run-5", Origin: RunOriginManual}); err == nil {
				t.Fatal("unsafe sync plan compiled successfully")
			}
		})
	}
}

func TestCompileRejectsInvalidEnvironmentAndBounds(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Job)
	}{
		{name: "malformed environment entry", mutate: func(job *Job) { job.Execution.Environment = []string{"NO_EQUALS"} }},
		{name: "invalid environment name", mutate: func(job *Job) { job.Execution.Environment = []string{"BAD-NAME=value"} }},
		{name: "reserved identity environment", mutate: func(job *Job) { job.Execution.Environment = []string{"HOME=/tmp"} }},
		{name: "execution path override", mutate: func(job *Job) { job.Execution.Environment = []string{"PATH=/tmp/untrusted"} }},
		{name: "negative timeout", mutate: func(job *Job) { job.Execution.TimeoutSeconds = -1 }},
		{name: "timeout above bound", mutate: func(job *Job) { job.Execution.TimeoutSeconds = 604801 }},
		{name: "timeout integer overflow", mutate: func(job *Job) { job.Execution.TimeoutSeconds = int(^uint(0) >> 1) }},
		{name: "negative grace", mutate: func(job *Job) { job.Execution.StopGraceSeconds = -1 }},
		{name: "grace above bound", mutate: func(job *Job) { job.Execution.StopGraceSeconds = 301 }},
		{name: "invalid umask", mutate: func(job *Job) { job.Execution.Umask = 0o1000 }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := validFixedJob(Action{Type: ActionCommand, Command: "id"})
			tt.mutate(&job)
			if _, err := newTestCompiler(validFixedInspector()).Compile(context.Background(), job, RunRequest{RunID: "run-6", Origin: RunOriginManual}); err == nil {
				t.Fatal("invalid execution policy compiled successfully")
			}
		})
	}
}

func TestCompileBlocksUnreviewedOrLegacySnapshots(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Job)
	}{
		{name: "legacy schema", mutate: func(job *Job) { job.SchemaVersion = 1 }},
		{name: "needs review", mutate: func(job *Job) { job.NeedsReview = true }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := validFixedJob(Action{Type: ActionCommand, Command: "id"})
			tt.mutate(&job)
			if _, err := newTestCompiler(validFixedInspector()).Compile(context.Background(), job, RunRequest{RunID: "run-review", Origin: RunOriginManual}); err == nil {
				t.Fatal("unsafe snapshot compiled successfully")
			}
		})
	}
}

func TestCompileRequiresActionCapabilities(t *testing.T) {
	compiler := NewPlanCompiler("node-a", CapabilityReport{Results: []CapabilityResult{
		{Code: CapHostNamespace, Status: CapabilityAvailable},
		{Code: CapHostPID1, Status: CapabilityAvailable},
		{Code: CapHostRoot, Status: CapabilityAvailable},
		{Code: CapIdentityDrop, Status: CapabilityUnavailable},
		{Code: CapAccountLookup, Status: CapabilityAvailable},
		{Code: CapShell, Status: CapabilityAvailable},
	}}, validFixedInspector())

	_, err := compiler.Compile(context.Background(), validFixedJob(Action{Type: ActionCommand, Command: "id"}), RunRequest{RunID: "run-7", Origin: RunOriginManual})
	if err == nil || !strings.Contains(err.Error(), string(CapIdentityDrop)) {
		t.Fatalf("missing capability error = %v", err)
	}
}

func TestCompileRejectsDryRunOutsideSync(t *testing.T) {
	tests := []struct {
		name      string
		job       Job
		inspector HostInspector
	}{
		{name: "script", job: validScriptJob("/srv/scripts/backup.sh"), inspector: validScriptInspector()},
		{name: "command", job: validFixedJob(Action{Type: ActionCommand, Command: "id"}), inspector: validFixedInspector()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := newTestCompiler(tt.inspector).Compile(context.Background(), tt.job, RunRequest{RunID: "run-dry", Origin: RunOriginManual, DryRun: true})
			if err == nil {
				t.Fatal("non-sync dry run compiled successfully")
			}
		})
	}

	job := validFixedJob(Action{Type: ActionSync, Sync: SyncAction{Engine: "rsync", Source: "/srv/source", Dest: "/srv/destination", Mode: "add"}})
	plan, err := newTestCompiler(validFixedInspector()).Compile(context.Background(), job, RunRequest{RunID: "run-sync-dry", Origin: RunOriginManual, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(plan.Argv, "--dry-run") {
		t.Fatalf("sync dry-run argv = %#v", plan.Argv)
	}
}

func TestCompileRejectsUnknownRunOrigin(t *testing.T) {
	job := validFixedJob(Action{Type: ActionCommand, Command: "id"})
	_, err := newTestCompiler(validFixedInspector()).Compile(context.Background(), job, RunRequest{RunID: "run-origin", Origin: RunOrigin("remote\n")})
	if err == nil {
		t.Fatal("unknown run origin compiled successfully")
	}
}

func TestCompileRejectsRootScriptOwner(t *testing.T) {
	for _, field := range []string{"uid", "gid"} {
		t.Run(field, func(t *testing.T) {
			inspector := validScriptInspector()
			info := inspector.paths["/srv/scripts/backup.sh"]
			if field == "uid" {
				info.UID = 0
				inspector.users["0"] = HostUser{Name: "root", UID: 0, GID: 0, Home: "/root", Shell: "/bin/sh"}
			} else {
				info.GID = 0
				inspector.groups["0"] = HostGroup{Name: "root", GID: 0}
			}
			inspector.paths["/srv/scripts/backup.sh"] = info
			_, err := newTestCompiler(inspector).Compile(context.Background(), validScriptJob("/srv/scripts/backup.sh"), RunRequest{RunID: "run-root-owner", Origin: RunOriginManual})
			if err == nil {
				t.Fatal("script-owner accepted root UID or GID")
			}
		})
	}
}

func TestCompileCarriesProbedToolsAndLauncherEnvironment(t *testing.T) {
	plan, err := newTestCompiler(validFixedInspector()).Compile(context.Background(), validFixedJob(Action{Type: ActionCommand, Command: "id"}), RunRequest{RunID: "run-tools", Origin: RunOriginManual})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.HostTools, testHostTools()) {
		t.Fatalf("plan tools = %#v, want %#v", plan.HostTools, testHostTools())
	}
	if !reflect.DeepEqual(plan.LauncherEnvironment, HostLauncherEnvironment()) {
		t.Fatalf("launcher environment = %#v", plan.LauncherEnvironment)
	}
}

func TestCompileRequiresProbedHostToolchain(t *testing.T) {
	report := fullyAvailableCapabilities()
	for index := range report.Results {
		if report.Results[index].Code == CapHostToolchain {
			report.Results[index].Status = CapabilityUnavailable
		}
	}
	_, err := NewPlanCompiler("node-a", report, validFixedInspector()).Compile(context.Background(), validFixedJob(Action{Type: ActionCommand, Command: "id"}), RunRequest{RunID: "run-no-tools", Origin: RunOriginManual})
	if err == nil || !strings.Contains(err.Error(), string(CapHostToolchain)) {
		t.Fatalf("missing host toolchain error = %v", err)
	}
}

func TestRunnerHostInspectorUsesDirectArgvWithUntrustedPath(t *testing.T) {
	path := "/srv/$(touch /tmp/pwn);backup.sh"
	runner := &fakeHostRunner{strictResults: true, runResults: map[string]CommandResult{
		commandKey("/usr/bin/readlink", "-m", "--", path):         {Stdout: []byte(path + "\n")},
		commandKey("/usr/bin/stat", "-c", "%f:%u:%g", "--", path): {Stdout: []byte("81ed:1001:1002\n")},
		commandKey("/usr/bin/head", "-c", "2", "--", path):        {Stdout: []byte("#!")},
	}}

	info, err := NewRunnerHostInspector(runner, testHostTools()).InspectPath(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Regular || !info.Executable || !info.HasShebang {
		t.Fatalf("path info = %#v", info)
	}
	for _, argv := range runner.argvs {
		if strings.Join(argv, " ") == "/bin/sh -c "+path {
			t.Fatalf("untrusted path reached a shell command: %#v", argv)
		}
		if argv[len(argv)-1] != path {
			t.Fatalf("path was not kept as one argv element: %#v", argv)
		}
	}
}

func validScriptJob(scriptPath string) Job {
	return Job{
		SchemaVersion: 2,
		ID:            7,
		Revision:      3,
		Name:          "nightly script",
		Action: Action{
			Type:       ActionScript,
			ScriptPath: scriptPath,
			ScriptArgs: []string{"--target", "daily;$(touch /tmp/pwn)"},
		},
		Identity: Identity{Mode: IdentityScriptOwner},
		Execution: ExecutionPolicy{
			WorkingDirectory: "/srv/scripts",
			Environment:      []string{"BACKUP_KIND=nightly"},
			Umask:            0o027,
			TimeoutSeconds:   120,
			StopGraceSeconds: 15,
		},
	}
}

func validFixedJob(action Action) Job {
	return Job{
		SchemaVersion: 2,
		ID:            8,
		Revision:      4,
		Name:          "fixed job",
		Action:        action,
		Identity:      Identity{Mode: IdentityFixed, User: "backup", UID: 1001, Group: "backup", GID: 1002},
		Execution: ExecutionPolicy{
			WorkingDirectory: "/srv",
			Environment:      []string{"TZ=Europe/Paris", "LANG=C", "CUSTOM=value with spaces"},
			Umask:            0o027,
			TimeoutSeconds:   300,
			StopGraceSeconds: 20,
		},
	}
}

func newTestCompiler(inspector HostInspector) *PlanCompiler {
	return NewPlanCompiler("node-a", fullyAvailableCapabilities(), inspector)
}

func fullyAvailableCapabilities() CapabilityReport {
	codes := []CapabilityCode{
		CapHostNamespace,
		CapHostPID1,
		CapHostRoot,
		CapIdentityDrop,
		CapAccountLookup,
		CapShell,
		CapRsync,
		CapRclone,
		CapHostToolchain,
	}
	results := make([]CapabilityResult, 0, len(codes))
	for _, code := range codes {
		result := CapabilityResult{Code: code, Status: CapabilityAvailable}
		if code == CapHostToolchain {
			result.Details = testHostTools().details()
		}
		results = append(results, result)
	}
	return CapabilityReport{Results: results}
}

func testHostTools() HostToolPaths {
	return semanticToolPaths("gnu")
}

func validScriptInspector() *fakeHostInspector {
	return &fakeHostInspector{
		paths: map[string]HostPathInfo{
			"/srv/scripts/backup.sh": {CanonicalPath: "/srv/scripts/backup.sh", Exists: true, Regular: true, Executable: true, HasShebang: true, UID: 1001, GID: 1002},
			"/srv/scripts":           {CanonicalPath: "/srv/scripts", Exists: true, Directory: true},
		},
		users:  map[string]HostUser{"1001": {Name: "backup", UID: 1001, GID: 1002, Home: "/home/backup", Shell: "/bin/sh"}},
		groups: map[string]HostGroup{"1002": {Name: "backup", GID: 1002}},
	}
}

func validFixedInspector() *fakeHostInspector {
	return &fakeHostInspector{
		paths: map[string]HostPathInfo{
			"/":                {CanonicalPath: "/", Exists: true, Directory: true},
			"/srv":             {CanonicalPath: "/srv", Exists: true, Directory: true},
			"/srv/source":      {CanonicalPath: "/srv/source", Exists: true, Directory: true, Empty: false},
			"/srv/destination": {CanonicalPath: "/srv/destination", Exists: true, Directory: true},
		},
		users:  map[string]HostUser{"backup": {Name: "backup", UID: 1001, GID: 1002, Home: "/home/backup", Shell: "/bin/sh"}},
		groups: map[string]HostGroup{"backup": {Name: "backup", GID: 1002}},
	}
}

type fakeHostInspector struct {
	paths  map[string]HostPathInfo
	users  map[string]HostUser
	groups map[string]HostGroup
}

func (f *fakeHostInspector) InspectPath(_ context.Context, path string) (HostPathInfo, error) {
	info, ok := f.paths[path]
	if !ok {
		return HostPathInfo{}, errors.New("path not found")
	}
	return info, nil
}

func (f *fakeHostInspector) LookupUser(_ context.Context, nameOrID string) (HostUser, error) {
	user, ok := f.users[nameOrID]
	if !ok {
		return HostUser{}, errors.New("user not found")
	}
	return user, nil
}

func (f *fakeHostInspector) LookupGroup(_ context.Context, nameOrID string) (HostGroup, error) {
	group, ok := f.groups[nameOrID]
	if !ok {
		return HostGroup{}, errors.New("group not found")
	}
	return group, nil
}

func TestCompilePreservesAdvancedRsyncOptions(t *testing.T) {
	job := validFixedJob(Action{Type: ActionSync, Sync: SyncAction{
		Engine: "rsync", Source: "/srv/source", Dest: "/srv/destination", Mode: "mirror",
		Compare: "checksum", Bwlimit: "30M", Backup: true, BackupKeep: 3, MaxDel: 25, SkipNew: true, SysBackup: true, Exclude: "*.tmp,cache/**",
	}})
	requested := time.Date(2026, 8, 27, 12, 34, 56, 0, time.UTC)
	plan, err := newTestCompiler(validFixedInspector()).Compile(context.Background(), job, RunRequest{RunID: "run-advanced-rsync", Origin: RunOriginManual, RequestedAt: requested})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"rsync", "-a", "--info=progress2,stats2", "--human-readable", "-HAX", "--numeric-ids", "--fake-super",
		"--delete", "--checksum", "--bwlimit", "30M", "--backup", "--backup-dir", "__SYNCBRIDGE_BACKUP_DIR__",
		"--exclude", ".sb-backup", "--max-delete", "25", "--update", "--exclude", "*.tmp", "--exclude", "cache/**", "--", "/srv/source/", "/srv/destination/",
	}
	if !reflect.DeepEqual(plan.Argv, want) {
		t.Fatalf("argv = %#v, want %#v", plan.Argv, want)
	}
	if plan.Sync != job.Action.Sync {
		t.Fatalf("plan sync = %#v, want %#v", plan.Sync, job.Action.Sync)
	}
}

func TestCompilePreservesAdvancedRcloneOptions(t *testing.T) {
	job := validFixedJob(Action{Type: ActionSync, Sync: SyncAction{
		Engine: "rclone", Source: "/srv/source", Dest: "/srv/destination", Mode: "mirror",
		Compare: "checksum", Bwlimit: "8M", Backup: true, MaxDel: 7, SkipNew: true, Exclude: "*.part,cache/**",
	}})
	requested := time.Date(2026, 8, 27, 12, 34, 56, 0, time.UTC)
	plan, err := newTestCompiler(validFixedInspector()).Compile(context.Background(), job, RunRequest{RunID: "run-advanced-rclone", Origin: RunOriginManual, RequestedAt: requested})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"rclone", "sync", "/srv/source", "/srv/destination", "--progress", "--stats=1s", "--stats-one-line", "--checksum", "--bwlimit", "8M",
		"--backup-dir", "__SYNCBRIDGE_BACKUP_DIR__", "--exclude", ".sb-backup/**", "--max-delete", "7", "--update", "--exclude", "*.part", "--exclude", "cache/**",
	}
	if !reflect.DeepEqual(plan.Argv, want) {
		t.Fatalf("argv = %#v, want %#v", plan.Argv, want)
	}
}

func TestValidateV2JobRejectsInvalidAdvancedSyncOptions(t *testing.T) {
	base := validFixedJob(Action{Type: ActionSync, Sync: SyncAction{Engine: "rsync", Source: "/srv/source", Dest: "/srv/destination", Mode: "add"}})
	base.Name = "sync"
	base.Trigger = TriggerManual
	base.Scheduler = SchedulerPolicy{Owner: SchedulerSyncBridge}
	base.Execution.Overlap = "skip"
	tests := []struct {
		name   string
		mutate func(*Job)
	}{
		{name: "compare", mutate: func(j *Job) { j.Action.Sync.Compare = "content-ish" }},
		{name: "bwlimit", mutate: func(j *Job) { j.Action.Sync.Bwlimit = "30M;rm" }},
		{name: "backup keep", mutate: func(j *Job) { j.Action.Sync.BackupKeep = -1 }},
		{name: "max delete", mutate: func(j *Job) { j.Action.Sync.MaxDel = -1 }},
		{name: "system backup with rclone", mutate: func(j *Job) { j.Action.Sync.Engine = "rclone"; j.Action.Sync.SysBackup = true }},
		{name: "exclude nul", mutate: func(j *Job) { j.Action.Sync.Exclude = "*.tmp\x00bad" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := cloneJob(base)
			tt.mutate(&job)
			if err := validateV2Job(job); err == nil {
				t.Fatalf("invalid advanced sync option accepted: %#v", job.Action.Sync)
			}
		})
	}
}
