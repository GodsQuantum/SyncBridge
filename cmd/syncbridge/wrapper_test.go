package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestWrapperStoreInstallsAtomicallyThroughHostRunner(t *testing.T) {
	plan := goldenFixedSyncPlan()
	dir := "/var/lib/syncbridge/instances/node-a/jobs/8"
	tempPath := dir + "/.run.sh.tmp-safe1234"
	runner := &recordingStoreRunner{tempPath: tempPath}
	store := NewWrapperStore(runner, NewWrapperRenderer())

	installedPath, err := store.Install(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if installedPath != plan.WrapperPath {
		t.Fatalf("installed path = %q, want %q", installedPath, plan.WrapperPath)
	}
	rendered, err := NewWrapperRenderer().Render(plan)
	if err != nil {
		t.Fatal(err)
	}
	foundInput := false
	foundRename := false
	for _, call := range runner.calls {
		if reflect.DeepEqual(call.argv, []string{plan.HostTools.Tee, tempPath}) && bytes.Equal(call.input, rendered) {
			foundInput = true
		}
		if reflect.DeepEqual(call.argv, []string{plan.HostTools.Mv, "-T", "-f", tempPath, plan.WrapperPath}) {
			foundRename = true
		}
	}
	if !foundInput || !foundRename {
		t.Fatalf("store omitted direct staging input or atomic rename: %#v", runner.calls)
	}
	for _, want := range [][]string{
		{plan.HostTools.Chown, "0:0", tempPath},
		{plan.HostTools.Chmod, "0700", tempPath},
		{plan.HostTools.Sync, "-f", tempPath},
		{plan.HostTools.Mv, "-T", "-f", tempPath, plan.WrapperPath},
		{plan.HostTools.Sync, "-f", dir},
	} {
		if !storeCallsContain(runner.calls, want) {
			t.Fatalf("store omitted minimal argv %#v: calls=%#v", want, runner.calls)
		}
	}
	assertStoreMutationArgvMinimal(t, runner.calls, plan.HostTools)
	for _, call := range runner.calls {
		for index, arg := range call.argv[:len(call.argv)-1] {
			if (arg == "sh" || strings.HasSuffix(arg, "/sh")) && call.argv[index+1] == "-c" {
				t.Fatalf("store used a composed shell command: %#v", call.argv)
			}
		}
	}
}

func TestWrapperStoreCleansOnlyValidatedTempOnFailure(t *testing.T) {
	plan := goldenScriptOwnerPlan()
	dir := "/var/lib/syncbridge/instances/node-a/jobs/7"
	tempPath := dir + "/.run.sh.tmp-safe5678"
	runner := &recordingStoreRunner{tempPath: tempPath, failCommand: "/usr/bin/sync\x00-f\x00" + tempPath}

	_, err := NewWrapperStore(runner, NewWrapperRenderer()).Install(context.Background(), plan)
	if err == nil {
		t.Fatal("install succeeded despite file fsync failure")
	}
	last := runner.calls[len(runner.calls)-1]
	if want := []string{"/usr/bin/rm", "-f", tempPath}; !reflect.DeepEqual(last.argv, want) {
		t.Fatalf("cleanup call = %#v, want %#v", last.argv, want)
	}
	for _, call := range runner.calls {
		if reflect.DeepEqual(call.argv, []string{"/usr/bin/mv", "-T", "-f", tempPath, plan.WrapperPath}) {
			t.Fatal("failed staging file was renamed into place")
		}
		if slicesContain(call.argv, "-r") || slicesContain(call.argv, "-R") || slicesContain(call.argv, "--recursive") {
			t.Fatalf("cleanup became recursive: %#v", call.argv)
		}
	}
}

func TestWrapperStoreRejectsUntrustedTempPathWithoutDeletingIt(t *testing.T) {
	plan := goldenScriptOwnerPlan()
	runner := &recordingStoreRunner{tempPath: "/tmp/untrusted"}

	_, err := NewWrapperStore(runner, NewWrapperRenderer()).Install(context.Background(), plan)
	if err == nil {
		t.Fatal("store accepted mktemp path outside the job directory")
	}
	for _, call := range runner.calls {
		if len(call.argv) > 0 && call.argv[0] == "/usr/bin/rm" {
			t.Fatalf("store deleted untrusted path: %#v", call.argv)
		}
	}
}

func TestWrapperStoreRejectsPathOutsideManagedRoot(t *testing.T) {
	plan := goldenFixedSyncPlan()
	plan.WrapperPath = "/etc/cron.d/syncbridge"
	runner := &recordingStoreRunner{}

	_, err := NewWrapperStore(runner, NewWrapperRenderer()).Install(context.Background(), plan)
	if err == nil {
		t.Fatal("store accepted wrapper path outside managed root")
	}
	if len(runner.calls) != 0 {
		t.Fatalf("invalid target caused host mutations: %#v", runner.calls)
	}
}

func TestWrapperStoreBehaviorInstallsAtomicFileUnderSafeHierarchy(t *testing.T) {
	plan := goldenFixedSyncPlan()
	runner := newMappedFilesystemRunner(t)
	target := runner.mapPath(plan.WrapperPath)
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("old wrapper\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := NewWrapperStore(runner, NewWrapperRenderer()).Install(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	want, err := NewWrapperRenderer().Render(plan)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("installed wrapper does not match rendered plan")
	}
	entries, err := os.ReadDir(filepath.Dir(target))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".run.sh.tmp-") {
			t.Fatalf("staging file remained after install: %s", entry.Name())
		}
	}
	if !runner.called(plan.HostTools.Mv, "-T", "-f") {
		t.Fatal("atomic replacement did not disable destination-directory interpretation")
	}
	assertStoreMutationArgvMinimal(t, runner.calls, plan.HostTools)
}

func TestWrapperStoreBehaviorRejectsUnsafeHierarchyAndTarget(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, *mappedFilesystemRunner, ExecutionPlan)
	}{
		{name: "ancestor symlink", setup: func(t *testing.T, runner *mappedFilesystemRunner, _ ExecutionPlan) {
			outside := filepath.Join(runner.root, "outside")
			if err := os.MkdirAll(outside, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, runner.mapPath("/var/lib/syncbridge")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "ancestor non root", setup: func(t *testing.T, runner *mappedFilesystemRunner, _ ExecutionPlan) {
			path := runner.mapPath("/var/lib/syncbridge")
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
			runner.ownerOverrides["/var/lib/syncbridge"] = "1000:0"
		}},
		{name: "ancestor writable", setup: func(t *testing.T, runner *mappedFilesystemRunner, _ ExecutionPlan) {
			path := runner.mapPath("/var/lib/syncbridge")
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0o777); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "target directory", setup: func(t *testing.T, runner *mappedFilesystemRunner, plan ExecutionPlan) {
			if err := os.MkdirAll(runner.mapPath(plan.WrapperPath), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := goldenFixedSyncPlan()
			runner := newMappedFilesystemRunner(t)
			tt.setup(t, runner, plan)
			if _, err := NewWrapperStore(runner, NewWrapperRenderer()).Install(context.Background(), plan); err == nil {
				t.Fatal("unsafe host filesystem state was accepted")
			}
			if info, err := os.Lstat(runner.mapPath(plan.WrapperPath)); err == nil && info.Mode().IsRegular() {
				t.Fatal("unsafe state was replaced with a wrapper")
			}
		})
	}
}

func TestWrapperScriptOwnerGolden(t *testing.T) {
	assertWrapperGolden(t, "script-owner.golden", goldenScriptOwnerPlan())
}

func TestWrapperFixedSyncGolden(t *testing.T) {
	assertWrapperGolden(t, "fixed-sync.golden", goldenFixedSyncPlan())
}

func TestWrapperPreservesArgvAndClearsInheritedEnvironment(t *testing.T) {
	tempDir := t.TempDir()
	getentPath := filepath.Join(tempDir, "getent")
	setprivPath := filepath.Join(tempDir, "setpriv")
	payloadPath := filepath.Join(tempDir, "payload.sh")
	wrapperPath := filepath.Join(tempDir, "run.sh")
	outputPath := filepath.Join(tempDir, "argv.txt")
	pwnOne := filepath.Join(tempDir, "pwn-one")
	pwnTwo := filepath.Join(tempDir, "pwn-two")

	writeExecutable(t, getentPath, `#!/bin/sh
[ "${LEAKED_SECRET+set}" != set ] || exit 97
case "$1:$2" in
	passwd:runner|passwd:1234) printf '%s\n' 'runner:x:1234:2345::/home/runner:/bin/sh' ;;
	group:runner|group:2345) printf '%s\n' 'runner:x:2345:' ;;
	*) exit 2 ;;
esac
`)
	writeExecutable(t, setprivPath, `#!/bin/sh
while [ "$#" -gt 0 ]; do
	case "$1" in
		--reuid|--regid) shift 2 ;;
		--init-groups|--reset-env) shift ;;
		--) shift; break ;;
		*) exit 2 ;;
	esac
done
exec "$@"
`)
	writeExecutable(t, payloadPath, `#!/bin/sh
printf '%s\n' "$#" "$1" "$2" "${LEAKED_SECRET-unset}" > "$OUTPUT_PATH"
`)

	argumentOne := "$(touch " + pwnOne + ")"
	argumentTwo := "x'; touch " + pwnTwo + "; #"
	plan := ExecutionPlan{
		RunID:               "run-behavior",
		JobID:               9,
		Revision:            1,
		ActionType:          ActionScript,
		Argv:                []string{payloadPath, argumentOne, argumentTwo},
		Identity:            Identity{Mode: IdentityFixed, User: "runner", UID: 1234, Group: "runner", GID: 2345},
		Environment:         []string{"OUTPUT_PATH=" + outputPath, "PATH=" + jobExecutionPATH, "TZ=UTC", "LANG=C", "LC_ALL=C"},
		WorkingDirectory:    tempDir,
		Umask:               0o027,
		WrapperPath:         wrapperPath,
		SourcePath:          payloadPath,
		HostTools:           testHostTools(),
		LauncherEnvironment: HostLauncherEnvironment(),
	}
	plan.HostTools.Getent = getentPath
	plan.HostTools.Setpriv = setprivPath
	renderer := NewWrapperRenderer()
	rendered, err := renderer.Render(plan)
	if err != nil {
		t.Fatal(err)
	}
	writeExecutableBytes(t, wrapperPath, rendered)

	t.Setenv("LEAKED_SECRET", "must-not-reach-root-helper")
	command := exec.CommandContext(context.Background(), "/bin/sh", wrapperPath)
	command.Env = plan.LauncherEnvironment
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("run wrapper: %v\n%s\n--- wrapper ---\n%s", err, output, rendered)
	}
	got, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("2\n%s\n%s\nunset\n", argumentOne, argumentTwo)
	if string(got) != want {
		t.Fatalf("payload argv/environment = %q, want %q", got, want)
	}
	for _, path := range []string{pwnOne, pwnTwo} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("injected command created %q (stat error %v)", path, err)
		}
	}
}

func TestWrapperScriptOwnerResolvesEveryRunAndBlocksUnknownUID(t *testing.T) {
	tempDir := t.TempDir()
	identityPath := filepath.Join(tempDir, "identity")
	dropLogPath := filepath.Join(tempDir, "drop.log")
	getentPath := filepath.Join(tempDir, "getent")
	statPath := filepath.Join(tempDir, "stat")
	setprivPath := filepath.Join(tempDir, "setpriv")
	payloadPath := filepath.Join(tempDir, "payload.sh")
	wrapperPath := filepath.Join(tempDir, "run.sh")

	writeExecutable(t, statPath, fmt.Sprintf(`#!/bin/sh
identity=$(cat %s)
case "$2" in
	%%d:%%i:%%f:%%u:%%g) printf '1:2:81ed:%%s\n' "$identity" ;;
	%%u:%%g) printf '%%s\n' "$identity" ;;
	*) exit 2 ;;
esac
`, ShellQuote(identityPath)))
	writeExecutable(t, getentPath, `#!/bin/sh
case "$1:$2" in
	passwd:1234|passwd:first) printf '%s\n' 'first:x:1234:2345::/srv/first:/bin/sh' ;;
	passwd:5678|passwd:second) printf '%s\n' 'second:x:5678:6789::/srv/second:/bin/bash' ;;
	passwd:0|passwd:root) printf '%s\n' 'root:x:0:0::/root:/bin/sh' ;;
	group:2345|group:first) printf '%s\n' 'first:x:2345:' ;;
	group:6789|group:second) printf '%s\n' 'second:x:6789:' ;;
	group:0|group:root) printf '%s\n' 'root:x:0:' ;;
	*) exit 2 ;;
esac
`)
	writeExecutable(t, setprivPath, fmt.Sprintf(`#!/bin/sh
uid=
gid=
while [ "$#" -gt 0 ]; do
	case "$1" in
		--reuid) uid=$2; shift 2 ;;
		--regid) gid=$2; shift 2 ;;
		--init-groups|--reset-env) shift ;;
		--) shift; break ;;
		*) exit 2 ;;
	esac
done
printf '%%s:%%s\n' "$uid" "$gid" >> %s
exec "$@"
`, ShellQuote(dropLogPath)))
	writeExecutable(t, payloadPath, "#!/bin/sh\nexit 0\n")

	plan := ExecutionPlan{
		RunID:               "run-owner",
		JobID:               10,
		Revision:            1,
		ActionType:          ActionScript,
		Argv:                []string{payloadPath},
		Identity:            Identity{Mode: IdentityScriptOwner},
		Environment:         []string{"LANG=C", "LC_ALL=C", "PATH=" + jobExecutionPATH, "TZ=UTC"},
		WorkingDirectory:    tempDir,
		Umask:               0o027,
		WrapperPath:         wrapperPath,
		SourcePath:          payloadPath,
		HostTools:           testHostTools(),
		LauncherEnvironment: HostLauncherEnvironment(),
	}
	plan.HostTools.Stat = statPath
	plan.HostTools.Getent = getentPath
	plan.HostTools.Setpriv = setprivPath
	renderer := NewWrapperRenderer()
	rendered, err := renderer.Render(plan)
	if err != nil {
		t.Fatal(err)
	}
	writeExecutableBytes(t, wrapperPath, rendered)
	run := func(wantSuccess bool) {
		t.Helper()
		command := exec.Command("/bin/sh", wrapperPath)
		command.Env = plan.LauncherEnvironment
		err := command.Run()
		if wantSuccess && err != nil {
			t.Fatalf("wrapper failed: %v", err)
		}
		if !wantSuccess && err == nil {
			t.Fatal("wrapper accepted an unknown script UID")
		}
	}

	if err := os.WriteFile(identityPath, []byte("1234:2345\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(true)
	if err := os.WriteFile(identityPath, []byte("5678:6789\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(true)
	if err := os.WriteFile(identityPath, []byte("0:0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(false)
	if err := os.WriteFile(identityPath, []byte("9999:9999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(false)

	got, err := os.ReadFile(dropLogPath)
	if err != nil {
		t.Fatal(err)
	}
	if want := "1234:2345\n5678:6789\n"; string(got) != want {
		t.Fatalf("runtime identities = %q, want %q", got, want)
	}
}

func TestWrapperFixedIdentityMismatchBlocksBeforePrivilegeDrop(t *testing.T) {
	tempDir := t.TempDir()
	getentPath := filepath.Join(tempDir, "getent")
	setprivPath := filepath.Join(tempDir, "setpriv")
	payloadPath := filepath.Join(tempDir, "payload.sh")
	wrapperPath := filepath.Join(tempDir, "run.sh")
	dropMarker := filepath.Join(tempDir, "drop-called")
	mismatchState := filepath.Join(tempDir, "mismatch")

	writeExecutable(t, getentPath, fmt.Sprintf(`#!/bin/sh
mismatch=$(cat %s)
case "$mismatch:$1:$2" in
	user:passwd:backup) printf '%%s\n' 'backup:x:9999:1002::/home/backup:/bin/sh' ;;
	group:passwd:backup|group:passwd:1001) printf '%%s\n' 'backup:x:1001:1002::/home/backup:/bin/sh' ;;
	group:group:backup) printf '%%s\n' 'backup:x:9999:' ;;
	*) exit 2 ;;
esac
`, ShellQuote(mismatchState)))
	writeExecutable(t, setprivPath, "#!/bin/sh\ntouch "+ShellQuote(dropMarker)+"\nexit 99\n")
	writeExecutable(t, payloadPath, "#!/bin/sh\nexit 0\n")

	plan := ExecutionPlan{
		RunID:               "run-fixed-mismatch",
		JobID:               11,
		Revision:            1,
		ActionType:          ActionScript,
		Argv:                []string{payloadPath},
		Identity:            Identity{Mode: IdentityFixed, User: "backup", UID: 1001, Group: "backup", GID: 1002},
		Environment:         []string{"LANG=C", "LC_ALL=C", "PATH=" + jobExecutionPATH, "TZ=UTC"},
		WorkingDirectory:    tempDir,
		Umask:               0o027,
		WrapperPath:         wrapperPath,
		SourcePath:          payloadPath,
		HostTools:           testHostTools(),
		LauncherEnvironment: HostLauncherEnvironment(),
	}
	plan.HostTools.Getent = getentPath
	plan.HostTools.Setpriv = setprivPath
	renderer := NewWrapperRenderer()
	rendered, err := renderer.Render(plan)
	if err != nil {
		t.Fatal(err)
	}
	writeExecutableBytes(t, wrapperPath, rendered)

	for _, mismatch := range []string{"user", "group"} {
		if err := os.WriteFile(mismatchState, []byte(mismatch+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		command := exec.Command("/bin/sh", wrapperPath)
		command.Env = plan.LauncherEnvironment
		if err := command.Run(); err == nil {
			t.Fatalf("wrapper accepted fixed %s mismatch", mismatch)
		}
		if _, err := os.Stat(dropMarker); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("fixed %s mismatch reached setpriv (stat error %v)", mismatch, err)
		}
	}
}

func TestWrapperBlocksScriptMutationBeforePrivilegeDrop(t *testing.T) {
	tempDir := t.TempDir()
	countPath := filepath.Join(tempDir, "stat-count")
	dropMarker := filepath.Join(tempDir, "drop-called")
	statPath := filepath.Join(tempDir, "stat")
	getentPath := filepath.Join(tempDir, "getent")
	setprivPath := filepath.Join(tempDir, "setpriv")
	payloadPath := filepath.Join(tempDir, "payload.sh")
	wrapperPath := filepath.Join(tempDir, "run.sh")
	if err := os.WriteFile(countPath, []byte("0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, statPath, fmt.Sprintf(`#!/bin/sh
case "$2" in
	%%d:%%i:%%f:%%u:%%g)
		count=$(cat %s); count=$((count + 1)); printf '%%s\n' "$count" > %s
		if [ "$count" -eq 1 ]; then printf '1:2:81ed:1234:2345\n'; else printf '1:3:81ed:1234:2345\n'; fi ;;
	%%u:%%g) printf '1234:2345\n' ;;
	*) exit 2 ;;
esac
`, ShellQuote(countPath), ShellQuote(countPath)))
	writeExecutable(t, getentPath, `#!/bin/sh
case "$1:$2" in
	passwd:runner|passwd:1234) printf '%s\n' 'runner:x:1234:2345::/home/runner:/bin/sh' ;;
	group:runner|group:2345) printf '%s\n' 'runner:x:2345:' ;;
	*) exit 2 ;;
esac
`)
	writeExecutable(t, setprivPath, "#!/bin/sh\ntouch "+ShellQuote(dropMarker)+"\nexit 99\n")
	writeExecutable(t, payloadPath, "#!/bin/sh\nexit 0\n")

	plan := ExecutionPlan{
		RunID: "run-script-mutation", JobID: 12, Revision: 1, Origin: RunOriginManual,
		ActionType: ActionScript, Argv: []string{payloadPath}, SourcePath: payloadPath,
		Identity:         Identity{Mode: IdentityFixed, User: "runner", UID: 1234, Group: "runner", GID: 2345},
		Environment:      []string{"LANG=C", "LC_ALL=C", "PATH=" + jobExecutionPATH, "TZ=UTC"},
		WorkingDirectory: tempDir, Umask: 0o027, WrapperPath: wrapperPath,
		HostTools: testHostTools(), LauncherEnvironment: HostLauncherEnvironment(),
	}
	plan.HostTools.Stat = statPath
	plan.HostTools.Getent = getentPath
	plan.HostTools.Setpriv = setprivPath
	rendered, err := NewWrapperRenderer().Render(plan)
	if err != nil {
		t.Fatal(err)
	}
	writeExecutableBytes(t, wrapperPath, rendered)
	command := exec.Command("/bin/sh", wrapperPath)
	command.Env = plan.LauncherEnvironment
	if err := command.Run(); err == nil {
		t.Fatal("wrapper accepted a script changed between identity probes")
	}
	if _, err := os.Stat(dropMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("script mutation reached setpriv (stat error %v)", err)
	}
}

func TestWrapperRepeatsDestructiveSyncGuardAfterIdentityLookup(t *testing.T) {
	tempDir := t.TempDir()
	sourcePath := filepath.Join(tempDir, "source")
	destinationPath := filepath.Join(tempDir, "destination")
	findCountPath := filepath.Join(tempDir, "find-count")
	dropMarker := filepath.Join(tempDir, "drop-called")
	findPath := filepath.Join(tempDir, "find")
	getentPath := filepath.Join(tempDir, "getent")
	setprivPath := filepath.Join(tempDir, "setpriv")
	wrapperPath := filepath.Join(tempDir, "run.sh")
	for _, path := range []string{sourcePath, destinationPath} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(findCountPath, []byte("0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, findPath, fmt.Sprintf(`#!/bin/sh
count=$(cat %s); count=$((count + 1)); printf '%%s\n' "$count" > %s
if [ "$count" -eq 1 ]; then printf 'entry\n'; fi
`, ShellQuote(findCountPath), ShellQuote(findCountPath)))
	writeExecutable(t, getentPath, `#!/bin/sh
case "$1:$2" in
	passwd:backup|passwd:1001) printf '%s\n' 'backup:x:1001:1002::/home/backup:/bin/sh' ;;
	group:backup|group:1002) printf '%s\n' 'backup:x:1002:' ;;
	*) exit 2 ;;
esac
`)
	writeExecutable(t, setprivPath, "#!/bin/sh\ntouch "+ShellQuote(dropMarker)+"\nexit 99\n")
	plan := ExecutionPlan{
		RunID: "run-sync-mutation", JobID: 13, Revision: 1, Origin: RunOriginManual,
		ActionType: ActionSync, SyncEngine: "rsync", SyncMode: "mirror",
		Sync:       SyncAction{Engine: "rsync", Source: sourcePath, Dest: destinationPath, Mode: "mirror"},
		Argv:       []string{"rsync", "-a", "--info=progress2,stats2", "--human-readable", "--delete", "--", sourcePath + "/", destinationPath + "/"},
		SourcePath: sourcePath, DestinationPath: destinationPath, NonEmptySourceDir: sourcePath,
		Identity:         Identity{Mode: IdentityFixed, User: "backup", UID: 1001, Group: "backup", GID: 1002},
		Environment:      []string{"LANG=C", "LC_ALL=C", "PATH=" + jobExecutionPATH, "TZ=UTC"},
		WorkingDirectory: tempDir, Umask: 0o027, WrapperPath: wrapperPath,
		HostTools: testHostTools(), LauncherEnvironment: HostLauncherEnvironment(),
	}
	plan.HostTools.Find = findPath
	plan.HostTools.Getent = getentPath
	plan.HostTools.Setpriv = setprivPath
	rendered, err := NewWrapperRenderer().Render(plan)
	if err != nil {
		t.Fatal(err)
	}
	writeExecutableBytes(t, wrapperPath, rendered)
	command := exec.Command("/bin/sh", wrapperPath)
	command.Env = plan.LauncherEnvironment
	if err := command.Run(); err == nil {
		t.Fatal("wrapper accepted a source emptied during identity lookup")
	}
	if _, err := os.Stat(dropMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("emptied sync source reached setpriv (stat error %v)", err)
	}
}

func TestWrapperBlocksSyncPathMutationBeforePrivilegeDrop(t *testing.T) {
	tempDir := t.TempDir()
	sourcePath := filepath.Join(tempDir, "source")
	destinationPath := filepath.Join(tempDir, "destination")
	countPath := filepath.Join(tempDir, "destination-stat-count")
	dropMarker := filepath.Join(tempDir, "drop-called")
	statPath := filepath.Join(tempDir, "stat")
	getentPath := filepath.Join(tempDir, "getent")
	setprivPath := filepath.Join(tempDir, "setpriv")
	wrapperPath := filepath.Join(tempDir, "run.sh")
	for _, path := range []string{sourcePath, destinationPath} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(countPath, []byte("0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, statPath, fmt.Sprintf(`#!/bin/sh
case "$4" in
	%s) count=$(cat %s); count=$((count + 1)); printf '%%s\n' "$count" > %s
		if [ "$count" -eq 1 ]; then printf '1:20:41c0:0:0\n'; else printf '1:21:41c0:0:0\n'; fi ;;
	*) printf '1:10:41c0:0:0\n' ;;
esac
`, ShellQuote(destinationPath), ShellQuote(countPath), ShellQuote(countPath)))
	writeExecutable(t, getentPath, `#!/bin/sh
case "$1:$2" in
	passwd:backup|passwd:1001) printf '%s\n' 'backup:x:1001:1002::/home/backup:/bin/sh' ;;
	group:backup|group:1002) printf '%s\n' 'backup:x:1002:' ;;
	*) exit 2 ;;
esac
`)
	writeExecutable(t, setprivPath, "#!/bin/sh\ntouch "+ShellQuote(dropMarker)+"\nexit 99\n")
	plan := ExecutionPlan{
		RunID: "run-sync-path-mutation", JobID: 14, Revision: 1, Origin: RunOriginManual,
		ActionType: ActionSync, SyncEngine: "rsync", SyncMode: "add",
		Sync:       SyncAction{Engine: "rsync", Source: sourcePath, Dest: destinationPath, Mode: "add"},
		Argv:       []string{"rsync", "-a", "--info=progress2,stats2", "--human-readable", "--", sourcePath + "/", destinationPath + "/"},
		SourcePath: sourcePath, DestinationPath: destinationPath,
		Identity:         Identity{Mode: IdentityFixed, User: "backup", UID: 1001, Group: "backup", GID: 1002},
		Environment:      []string{"LANG=C", "LC_ALL=C", "PATH=" + jobExecutionPATH, "TZ=UTC"},
		WorkingDirectory: tempDir, Umask: 0o027, WrapperPath: wrapperPath,
		HostTools: testHostTools(), LauncherEnvironment: HostLauncherEnvironment(),
	}
	plan.HostTools.Stat = statPath
	plan.HostTools.Getent = getentPath
	plan.HostTools.Setpriv = setprivPath
	rendered, err := NewWrapperRenderer().Render(plan)
	if err != nil {
		t.Fatal(err)
	}
	writeExecutableBytes(t, wrapperPath, rendered)
	command := exec.Command("/bin/sh", wrapperPath)
	command.Env = plan.LauncherEnvironment
	if err := command.Run(); err == nil {
		t.Fatal("wrapper accepted a changed sync destination")
	}
	if _, err := os.Stat(dropMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("changed sync destination reached setpriv (stat error %v)", err)
	}
}

func TestWrapperQuotesEveryArgument(t *testing.T) {
	tests := map[string]string{
		"":                   "''",
		"plain":              "'plain'",
		"$(touch /tmp/pwn)":  "'$(touch /tmp/pwn)'",
		"x'y":                "'x'\"'\"'y'",
		"line one\nline two": "'line one\nline two'",
	}
	for input, want := range tests {
		if got := ShellQuote(input); got != want {
			t.Errorf("ShellQuote(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestWrapperRejectsInvalidPlanBeforeRendering(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ExecutionPlan)
	}{
		{name: "empty argv", mutate: func(plan *ExecutionPlan) { plan.Argv = nil }},
		{name: "relative working directory", mutate: func(plan *ExecutionPlan) { plan.WorkingDirectory = "srv" }},
		{name: "relative wrapper", mutate: func(plan *ExecutionPlan) { plan.WrapperPath = "run.sh" }},
		{name: "unsupported identity", mutate: func(plan *ExecutionPlan) { plan.Identity.Mode = "mystery" }},
		{name: "reserved environment", mutate: func(plan *ExecutionPlan) { plan.Environment = append(plan.Environment, "HOME=/tmp") }},
		{name: "malformed environment", mutate: func(plan *ExecutionPlan) { plan.Environment = append(plan.Environment, "BAD-NAME=x") }},
		{name: "relative host tool", mutate: func(plan *ExecutionPlan) { plan.HostTools.Stat = "stat" }},
		{name: "sync argv engine mismatch", mutate: func(plan *ExecutionPlan) { plan.Argv[0] = "rclone" }},
		{name: "sync source missing", mutate: func(plan *ExecutionPlan) { plan.SourcePath = "" }},
		{name: "sync destination ancestor", mutate: func(plan *ExecutionPlan) { plan.DestinationPath = "/" }},
		{name: "destructive guard mismatch", mutate: func(plan *ExecutionPlan) { plan.NonEmptySourceDir = "" }},
		{name: "dry run command", mutate: func(plan *ExecutionPlan) {
			plan.ActionType = ActionCommand
			plan.Command = "id"
			plan.Argv = []string{plan.HostTools.Shell, "-c", "id"}
			plan.SourcePath = ""
			plan.DestinationPath = ""
			plan.NonEmptySourceDir = ""
			plan.SyncEngine = ""
			plan.SyncMode = ""
			plan.DryRun = true
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := goldenFixedSyncPlan()
			tt.mutate(&plan)
			if _, err := NewWrapperRenderer().Render(plan); err == nil {
				t.Fatal("invalid plan rendered successfully")
			}
		})
	}
}

func assertWrapperGolden(t *testing.T, name string, plan ExecutionPlan) {
	t.Helper()
	want, err := os.ReadFile(filepath.Join("testdata", "wrappers", name))
	if err != nil {
		t.Fatal(err)
	}
	got, err := NewWrapperRenderer().Render(plan)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("wrapper differs from hand-written golden %s\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

func goldenScriptOwnerPlan() ExecutionPlan {
	return ExecutionPlan{
		RunID:               "run-1",
		JobID:               7,
		Revision:            3,
		Origin:              RunOriginManual,
		ActionType:          ActionScript,
		Argv:                []string{"/srv/scripts/backup.sh", "--target", "daily;$(touch /tmp/pwn)"},
		Identity:            Identity{Mode: IdentityScriptOwner},
		Environment:         []string{"BACKUP_KIND=nightly", "LANG=C", "LC_ALL=C", "PATH=" + jobExecutionPATH, "SYNCBRIDGE_DRY_RUN=false", "SYNCBRIDGE_JOB_ID=7", "SYNCBRIDGE_JOB_REVISION=3", "SYNCBRIDGE_RUN_ID=run-1", "SYNCBRIDGE_RUN_ORIGIN=manual", "TZ=UTC"},
		WorkingDirectory:    "/srv/scripts",
		Umask:               0o027,
		WrapperPath:         "/var/lib/syncbridge/instances/node-a/jobs/7/run.sh",
		SourcePath:          "/srv/scripts/backup.sh",
		HostTools:           testHostTools(),
		LauncherEnvironment: HostLauncherEnvironment(),
	}
}

func goldenFixedSyncPlan() ExecutionPlan {
	return ExecutionPlan{
		RunID:               "run-2",
		JobID:               8,
		Revision:            4,
		Origin:              RunOriginCron,
		RequestedAt:         time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC),
		ActionType:          ActionSync,
		Sync:                SyncAction{Engine: "rsync", Source: "/srv/source", Dest: "/srv/destination", Mode: "mirror"},
		Argv:                []string{"rsync", "-a", "--info=progress2,stats2", "--human-readable", "--delete", "--", "/srv/source/", "/srv/destination/"},
		Identity:            Identity{Mode: IdentityFixed, User: "backup", UID: 1001, Group: "backup", GID: 1002},
		Environment:         []string{"CUSTOM=value with spaces", "LANG=C", "LC_ALL=C", "PATH=" + jobExecutionPATH, "SYNCBRIDGE_DRY_RUN=false", "SYNCBRIDGE_JOB_ID=8", "SYNCBRIDGE_JOB_REVISION=4", "SYNCBRIDGE_RUN_ID=run-2", "SYNCBRIDGE_RUN_ORIGIN=cron", "TZ=Europe/Paris"},
		WorkingDirectory:    "/srv",
		Umask:               0o027,
		WrapperPath:         "/var/lib/syncbridge/instances/node-a/jobs/8/run.sh",
		SourcePath:          "/srv/source",
		DestinationPath:     "/srv/destination",
		NonEmptySourceDir:   "/srv/source",
		SyncEngine:          "rsync",
		SyncMode:            "mirror",
		HostTools:           testHostTools(),
		LauncherEnvironment: HostLauncherEnvironment(),
	}
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	writeExecutableBytes(t, path, []byte(contents))
}

func writeExecutableBytes(t *testing.T, path string, contents []byte) {
	t.Helper()
	if err := os.WriteFile(path, contents, 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestShellQuoteRoundTripsThroughPOSIXShell(t *testing.T) {
	inputs := []string{"", "plain", "$(false)", "x'y", "semi;colon", "line one\nline two"}
	for _, input := range inputs {
		command := exec.Command("/bin/sh", "-c", "printf '%s' "+ShellQuote(input))
		got, err := command.Output()
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != input {
			t.Fatalf("shell round trip = %q, want %q", got, input)
		}
	}
}

type storeRunnerCall struct {
	input []byte
	argv  []string
}

func storeCallsContain(calls []storeRunnerCall, want []string) bool {
	for _, call := range calls {
		if reflect.DeepEqual(call.argv, want) {
			return true
		}
	}
	return false
}

func assertStoreMutationArgvMinimal(t *testing.T, calls []storeRunnerCall, tools HostToolPaths) {
	t.Helper()
	mutating := []string{tools.Mkdir, tools.Mktemp, tools.Tee, tools.Chown, tools.Chmod, tools.Sync, tools.Mv, tools.Rm}
	for _, call := range calls {
		if len(call.argv) == 0 || !slicesContain(mutating, call.argv[0]) {
			continue
		}
		if slicesContain(call.argv, "--") {
			t.Fatalf("validated absolute store path used unnecessary --: %#v", call.argv)
		}
		if call.argv[0] == tools.Sync && (len(call.argv) != 3 || call.argv[1] != "-f" || !filepath.IsAbs(call.argv[2])) {
			t.Fatalf("store sync argv does not match probed -f FILE contract: %#v", call.argv)
		}
	}
}

type recordingStoreRunner struct {
	tempPath    string
	failCommand string
	calls       []storeRunnerCall
	renamed     bool
}

// mappedFilesystemRunner executes direct argv against a temporary filesystem
// root while simulating root ownership metadata. It deliberately has no shell
// composition path and launches every helper with the production environment.
type mappedFilesystemRunner struct {
	root           string
	ownerOverrides map[string]string
	calls          []storeRunnerCall
}

func newMappedFilesystemRunner(t *testing.T) *mappedFilesystemRunner {
	t.Helper()
	runner := &mappedFilesystemRunner{root: t.TempDir(), ownerOverrides: map[string]string{}}
	if err := os.MkdirAll(runner.mapPath("/var/lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	return runner
}

func (r *mappedFilesystemRunner) mapPath(path string) string {
	if strings.HasPrefix(path, "/var/") || path == "/var" {
		return r.root + path
	}
	return path
}

func (r *mappedFilesystemRunner) logicalPath(path string) string {
	return strings.TrimPrefix(path, r.root)
}

func (r *mappedFilesystemRunner) called(prefix ...string) bool {
	for _, call := range r.calls {
		if len(call.argv) >= len(prefix) && reflect.DeepEqual(call.argv[:len(prefix)], prefix) {
			return true
		}
	}
	return false
}

func (r *mappedFilesystemRunner) Run(ctx context.Context, argv ...string) CommandResult {
	return r.runInput(ctx, nil, argv...)
}

func (r *mappedFilesystemRunner) RunInput(ctx context.Context, input []byte, argv ...string) CommandResult {
	return r.runInput(ctx, input, argv...)
}

func (r *mappedFilesystemRunner) runInput(ctx context.Context, input []byte, argv ...string) CommandResult {
	r.calls = append(r.calls, storeRunnerCall{input: bytes.Clone(input), argv: append([]string(nil), argv...)})
	if len(argv) == 0 {
		return CommandResult{ExitCode: -1, Err: errors.New("empty argv")}
	}
	translated := append([]string(nil), argv...)
	for index := 1; index < len(translated); index++ {
		translated[index] = r.mapPath(translated[index])
	}
	if argv[0] == "/usr/bin/chown" || argv[0] == "/bin/chown" {
		return CommandResult{}
	}
	if (argv[0] == "/usr/bin/stat" || argv[0] == "/bin/stat") && (len(argv) == 4 || len(argv) == 5) && argv[1] == "-c" && argv[2] == "%f:%u:%g:%a" {
		last := len(argv) - 1
		info, err := os.Lstat(translated[last])
		if err != nil {
			return CommandResult{ExitCode: 1, Err: err}
		}
		owner := r.ownerOverrides[argv[last]]
		if owner == "" {
			owner = "0:0"
		}
		fileType := uint64(0o100000)
		if info.IsDir() {
			fileType = 0o040000
		}
		if info.Mode()&os.ModeSymlink != 0 {
			fileType = 0o120000
		}
		return CommandResult{Stdout: []byte(strconv.FormatUint(fileType|uint64(info.Mode().Perm()), 16) + ":" + owner + ":" + strconv.FormatUint(uint64(info.Mode().Perm()), 8) + "\n")}
	}
	command := exec.CommandContext(ctx, translated[0], translated[1:]...)
	command.Env = HostLauncherEnvironment()
	if input != nil {
		command.Stdin = bytes.NewReader(input)
	}
	stdout, err := command.Output()
	if err != nil {
		return CommandResult{ExitCode: 1, Err: err}
	}
	stdout = []byte(strings.ReplaceAll(string(stdout), r.root, ""))
	return CommandResult{Stdout: stdout}
}

func (r *recordingStoreRunner) Run(ctx context.Context, argv ...string) CommandResult {
	return r.RunInput(ctx, nil, argv...)
}

func (r *recordingStoreRunner) RunInput(_ context.Context, input []byte, argv ...string) CommandResult {
	call := storeRunnerCall{input: append([]byte(nil), input...), argv: append([]string(nil), argv...)}
	r.calls = append(r.calls, call)
	if strings.Join(argv, "\x00") == r.failCommand {
		return CommandResult{ExitCode: 1, Err: errors.New("injected host command failure")}
	}
	if (len(argv) == 5 || len(argv) == 6) && strings.HasSuffix(argv[0], "/mv") {
		r.renamed = true
	}
	if len(argv) > 0 && argv[0] == "/usr/bin/mktemp" {
		return CommandResult{Stdout: []byte(r.tempPath + "\n")}
	}
	if (len(argv) == 3 || len(argv) == 4) && argv[0] == "/usr/bin/readlink" {
		return CommandResult{Stdout: []byte(argv[len(argv)-1] + "\n")}
	}
	if (len(argv) == 4 || len(argv) == 5) && strings.HasSuffix(argv[0], "/stat") && argv[2] == "%f:%u:%g:%a" {
		path := argv[len(argv)-1]
		if strings.HasSuffix(path, "/run.sh") && !r.renamed {
			return CommandResult{ExitCode: 1, Err: errors.New("missing")}
		}
		mode := "700"
		fileType := "81c0"
		if path == "/var" || path == "/var/lib" {
			mode = "755"
			fileType = "41ed"
		} else if !strings.Contains(path, ".run.sh.tmp-") && !strings.HasSuffix(path, "/run.sh") {
			fileType = "41c0"
		}
		return CommandResult{Stdout: []byte(fileType + ":0:0:" + mode + "\n")}
	}
	if len(argv) > 0 && argv[0] == "/usr/bin/tee" && (len(argv) == 2 || len(argv) == 3) {
		return CommandResult{Stdout: bytes.Clone(input)}
	}
	return CommandResult{}
}

func slicesContain(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestWrapperBackupDirectoryIsCreatedAtExecutionTimeWithRetention(t *testing.T) {
	plan := goldenFixedSyncPlan()
	plan.Sync.Backup = true
	plan.Sync.BackupKeep = 2
	plan.Argv, _, _ = syncArgv(plan.Sync, false, plan.RequestedAt)
	rendered, err := NewWrapperRenderer().Render(plan)
	if err != nil {
		t.Fatal(err)
	}
	text := string(rendered)
	for _, want := range []string{
		"backup_stamp=$('/usr/bin/date' '-u' '+%Y%m%d-%H%M%S')",
		"backup_root='/srv/destination/.sb-backup'",
		"backup_dir=\"$backup_root/$backup_stamp-$$\"",
		"\"$backup_dir\"",
		"backup_keep='2'",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("wrapper missing runtime backup behavior %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "'__SYNCBRIDGE_BACKUP_DIR__'") {
		t.Fatalf("wrapper rendered backup token literally:\n%s", text)
	}
}

func TestWrapperDryRunBackupDisablesRetention(t *testing.T) {
	plan := goldenFixedSyncPlan()
	plan.Sync.Backup = true
	plan.Sync.BackupKeep = 5
	plan.DryRun = true
	plan.Argv, _, _ = syncArgv(plan.Sync, true, plan.RequestedAt)
	rendered, err := NewWrapperRenderer().Render(plan)
	if err != nil {
		t.Fatal(err)
	}
	text := string(rendered)
	if !strings.Contains(text, "backup_keep='0'") {
		t.Fatalf("dry-run wrapper may prune backups:\n%s", text)
	}
	if !strings.Contains(text, `"$backup_dir"`) {
		t.Fatalf("dry-run wrapper did not resolve runtime backup directory:\n%s", text)
	}
}
