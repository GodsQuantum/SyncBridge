package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

type wrapperToolPaths struct {
	readlink string
	head     string
	stat     string
	find     string
	getent   string
	setpriv  string
	env      string
	date     string
	rm       string
}

// WrapperRenderer renders deterministic root-owned POSIX shell wrappers. Tool
// paths are fixed by the application; the unexported field only permits the
// behavioral test to replace host privilege/account tools with harmless fakes.
type WrapperRenderer struct {
	tools wrapperToolPaths
}

// WrapperStore installs rendered wrappers exclusively through the host command
// runner. No container-local access to /var/lib is performed.
type WrapperStore struct {
	runner   HostCommandRunner
	renderer WrapperRenderer
}

func NewWrapperStore(runner HostCommandRunner, renderer WrapperRenderer) *WrapperStore {
	return &WrapperStore{runner: runner, renderer: renderer}
}

// Install durably replaces the managed wrapper using a same-directory staging
// file. Every host operation is one direct argv; data reaches the host only as
// RunInput stdin. On failure only a validated staging pathname is removed.
// Revalidation narrows races to a residual root-only TOCTOU: an already
// privileged host process could still replace a component between direct argv.
func (s *WrapperStore) Install(ctx context.Context, plan ExecutionPlan) (string, error) {
	if s == nil || s.runner == nil {
		return "", errors.New("host command runner is required")
	}
	if err := validateManagedWrapperPath(plan); err != nil {
		return "", err
	}
	rendered, err := s.renderer.Render(plan)
	if err != nil {
		return "", fmt.Errorf("render host wrapper: %w", err)
	}
	target := plan.WrapperPath
	dir := filepath.Dir(target)
	tools := plan.HostTools
	if err := validateStoreTools(tools); err != nil {
		return "", err
	}
	if err := s.requireCanonicalHostTarget(ctx, tools, target); err != nil {
		return "", err
	}
	if err := s.secureManagedHierarchy(ctx, tools, dir); err != nil {
		return "", err
	}
	if err := s.validateTarget(ctx, tools, target); err != nil {
		return "", err
	}

	if err := s.revalidateManagedHierarchy(ctx, tools, dir); err != nil {
		return "", err
	}
	result := s.runner.Run(ctx, tools.Mktemp, filepath.Join(dir, ".run.sh.tmp-XXXXXXXX"))
	if result.Err != nil {
		return "", errors.New("create host wrapper staging file")
	}
	tempPath, err := validateWrapperTempPath(dir, result.Stdout)
	if err != nil {
		return "", err
	}
	renamed := false
	defer func() {
		if !renamed {
			_ = s.run(context.WithoutCancel(ctx), tools.Rm, "-f", tempPath)
		}
	}()

	if err := s.validateStagingFile(ctx, tools, tempPath); err != nil {
		return "", err
	}
	if result := s.runner.RunInput(ctx, rendered, tools.Tee, tempPath); result.Err != nil {
		return "", errors.New("write host wrapper staging file")
	}
	if err := s.validateStagingFile(ctx, tools, tempPath); err != nil {
		return "", err
	}
	if err := s.run(ctx, tools.Chown, "0:0", tempPath); err != nil {
		return "", fmt.Errorf("chown host wrapper staging file: %w", err)
	}
	if err := s.validateStagingFile(ctx, tools, tempPath); err != nil {
		return "", err
	}
	if err := s.run(ctx, tools.Chmod, "0700", tempPath); err != nil {
		return "", fmt.Errorf("chmod host wrapper staging file: %w", err)
	}
	if err := s.validateRegularRootFile(ctx, tools, tempPath, true); err != nil {
		return "", err
	}
	if err := s.run(ctx, tools.Sync, "-f", tempPath); err != nil {
		return "", fmt.Errorf("fsync host wrapper staging file: %w", err)
	}
	if err := s.revalidateManagedHierarchy(ctx, tools, dir); err != nil {
		return "", err
	}
	if err := s.validateTarget(ctx, tools, target); err != nil {
		return "", err
	}
	if err := s.validateRegularRootFile(ctx, tools, tempPath, true); err != nil {
		return "", err
	}
	if err := s.run(ctx, tools.Mv, "-T", "-f", tempPath, target); err != nil {
		return "", fmt.Errorf("rename host wrapper staging file: %w", err)
	}
	renamed = true
	if err := s.validateRegularRootFile(ctx, tools, target, true); err != nil {
		return "", err
	}
	if err := s.revalidateManagedHierarchy(ctx, tools, dir); err != nil {
		return "", err
	}
	if err := s.run(ctx, tools.Sync, "-f", dir); err != nil {
		return "", fmt.Errorf("fsync host wrapper directory: %w", err)
	}
	return target, nil
}

func (s *WrapperStore) run(ctx context.Context, argv ...string) error {
	if result := s.runner.Run(ctx, argv...); result.Err != nil {
		return result.Err
	}
	return nil
}

func (s *WrapperStore) requireCanonicalHostTarget(ctx context.Context, tools HostToolPaths, target string) error {
	result := s.runner.Run(ctx, tools.Readlink, "-m", target)
	if result.Err != nil {
		return errors.New("canonicalize host wrapper target")
	}
	resolved, err := singleOutputLine(result.Stdout)
	if err != nil {
		return fmt.Errorf("canonicalize host wrapper target: %w", err)
	}
	if resolved != target {
		return fmt.Errorf("host wrapper target escapes through a symlink: %q resolves to %q", target, resolved)
	}
	return nil
}

func validateStoreTools(tools HostToolPaths) error {
	if tools.Profile != "gnu" && tools.Profile != "busybox" {
		return errors.New("host toolchain profile is invalid")
	}
	for _, path := range []string{tools.Test, tools.Readlink, tools.Stat, tools.Mkdir, tools.Chown, tools.Chmod, tools.Mktemp, tools.Tee, tools.Sync, tools.Mv, tools.Rm} {
		if err := validateAbsoluteCanonicalPath(path); err != nil {
			return fmt.Errorf("wrapper store tool path: %w", err)
		}
	}
	return nil
}

func managedDirectoryComponents(dir string) ([]string, error) {
	const root = "/var/lib/syncbridge"
	if dir != root && !strings.HasPrefix(dir, root+"/") {
		return nil, errors.New("wrapper directory is outside the managed root")
	}
	components := []string{"/var", "/var/lib", root}
	current := root
	for _, name := range strings.Split(strings.TrimPrefix(dir, root+"/"), "/") {
		if name == "" {
			continue
		}
		current = filepath.Join(current, name)
		components = append(components, current)
	}
	return components, nil
}

func (s *WrapperStore) secureManagedHierarchy(ctx context.Context, tools HostToolPaths, dir string) error {
	components, err := managedDirectoryComponents(dir)
	if err != nil {
		return err
	}
	for index, component := range components {
		metadata, exists := s.statPath(ctx, tools, component)
		if exists {
			if err := s.validateSafeDirectory(ctx, tools, component); err != nil {
				return err
			}
			continue
		}
		_ = metadata
		if index < 2 {
			return fmt.Errorf("required host directory is missing: %s", component)
		}
		for _, parent := range components[:index] {
			if err := s.validateSafeDirectory(ctx, tools, parent); err != nil {
				return err
			}
		}
		if err := s.run(ctx, tools.Mkdir, component); err != nil {
			return fmt.Errorf("create managed directory: %w", err)
		}
		if err := s.validateSafeDirectory(ctx, tools, component); err != nil {
			return err
		}
		if err := s.run(ctx, tools.Chown, "0:0", component); err != nil {
			return fmt.Errorf("chown managed directory: %w", err)
		}
		if err := s.validateSafeDirectory(ctx, tools, component); err != nil {
			return err
		}
		if err := s.run(ctx, tools.Chmod, "0700", component); err != nil {
			return fmt.Errorf("chmod managed directory: %w", err)
		}
		if err := s.validateSafeDirectory(ctx, tools, component); err != nil {
			return err
		}
	}
	return nil
}

func (s *WrapperStore) revalidateManagedHierarchy(ctx context.Context, tools HostToolPaths, dir string) error {
	components, err := managedDirectoryComponents(dir)
	if err != nil {
		return err
	}
	for _, component := range components {
		if err := s.validateSafeDirectory(ctx, tools, component); err != nil {
			return err
		}
	}
	return nil
}

func (s *WrapperStore) validateSafeDirectory(ctx context.Context, tools HostToolPaths, path string) error {
	if err := s.requireCanonicalHostTarget(ctx, tools, path); err != nil {
		return err
	}
	metadata, exists := s.statPath(ctx, tools, path)
	if !exists || metadata.fileType != 0o040000 {
		return fmt.Errorf("managed path is not a directory: %s", path)
	}
	if metadata.uid != 0 || metadata.gid != 0 {
		return fmt.Errorf("managed directory is not root-owned: %s", path)
	}
	if metadata.permissions&0o022 != 0 {
		return fmt.Errorf("managed directory is group/other writable: %s", path)
	}
	return nil
}

func (s *WrapperStore) validateTarget(ctx context.Context, tools HostToolPaths, target string) error {
	if err := s.requireCanonicalHostTarget(ctx, tools, target); err != nil {
		return err
	}
	_, exists := s.statPath(ctx, tools, target)
	if !exists {
		return nil
	}
	return s.validateRegularRootFile(ctx, tools, target, false)
}

func (s *WrapperStore) validateStagingFile(ctx context.Context, tools HostToolPaths, path string) error {
	return s.validateRegularRootFile(ctx, tools, path, false)
}

func (s *WrapperStore) validateRegularRootFile(ctx context.Context, tools HostToolPaths, path string, require0700 bool) error {
	if err := s.requireCanonicalHostTarget(ctx, tools, path); err != nil {
		return err
	}
	metadata, exists := s.statPath(ctx, tools, path)
	if !exists || metadata.fileType != 0o100000 {
		return fmt.Errorf("managed file is not regular: %s", path)
	}
	if metadata.uid != 0 || metadata.gid != 0 {
		return fmt.Errorf("managed file is not root-owned: %s", path)
	}
	if require0700 && metadata.permissions != 0o700 {
		return fmt.Errorf("managed wrapper mode is %04o, want 0700", metadata.permissions)
	}
	if !require0700 && metadata.permissions&0o022 != 0 {
		return fmt.Errorf("managed file is group/other writable: %s", path)
	}
	return nil
}

type hostPathMetadata struct {
	fileType    uint64
	uid, gid    int
	permissions uint64
}

func (s *WrapperStore) statPath(ctx context.Context, tools HostToolPaths, path string) (hostPathMetadata, bool) {
	result := s.runner.Run(ctx, tools.Stat, "-c", "%f:%u:%g:%a", path)
	if result.Err != nil {
		return hostPathMetadata{}, false
	}
	line, err := singleOutputLine(result.Stdout)
	if err != nil {
		return hostPathMetadata{}, false
	}
	parts := strings.Split(line, ":")
	if len(parts) != 4 {
		return hostPathMetadata{}, false
	}
	mode, modeErr := strconv.ParseUint(parts[0], 16, 32)
	uid, uidErr := strconv.Atoi(parts[1])
	gid, gidErr := strconv.Atoi(parts[2])
	permissions, permissionsErr := strconv.ParseUint(parts[3], 8, 32)
	if uidErr != nil || gidErr != nil || modeErr != nil || permissionsErr != nil {
		return hostPathMetadata{}, false
	}
	return hostPathMetadata{fileType: mode & 0o170000, uid: uid, gid: gid, permissions: permissions}, true
}

func validateManagedWrapperPath(plan ExecutionPlan) error {
	if err := validateAbsoluteCanonicalPath(plan.WrapperPath); err != nil {
		return fmt.Errorf("managed wrapper path: %w", err)
	}
	const prefix = "/var/lib/syncbridge/instances/"
	if !strings.HasPrefix(plan.WrapperPath, prefix) {
		return errors.New("wrapper path is outside the managed root")
	}
	parts := strings.Split(strings.TrimPrefix(plan.WrapperPath, prefix), "/")
	if len(parts) != 4 || !instanceIDPattern.MatchString(parts[0]) || parts[1] != "jobs" || parts[3] != "run.sh" {
		return errors.New("wrapper path does not match the managed job layout")
	}
	jobID, err := strconv.Atoi(parts[2])
	if err != nil || jobID <= 0 || jobID != plan.JobID {
		return errors.New("wrapper path job ID does not match the execution plan")
	}
	return nil
}

func validateWrapperTempPath(dir string, output []byte) (string, error) {
	tempPath, err := singleOutputLine(output)
	if err != nil {
		return "", fmt.Errorf("parse host wrapper staging path: %w", err)
	}
	if err := validateAbsoluteCanonicalPath(tempPath); err != nil {
		return "", fmt.Errorf("host wrapper staging path: %w", err)
	}
	base := filepath.Base(tempPath)
	if filepath.Dir(tempPath) != dir || !strings.HasPrefix(base, ".run.sh.tmp-") || base == ".run.sh.tmp-" {
		return "", errors.New("host returned an unsafe wrapper staging path")
	}
	return tempPath, nil
}

func NewWrapperRenderer() WrapperRenderer {
	return WrapperRenderer{}
}

// ShellQuote returns one POSIX shell word whose value is exactly input.
func ShellQuote(input string) string {
	return "'" + strings.ReplaceAll(input, "'", "'\"'\"'") + "'"
}

func (r WrapperRenderer) Render(plan ExecutionPlan) ([]byte, error) {
	r.tools = wrapperToolPaths{
		readlink: plan.HostTools.Readlink,
		head:     plan.HostTools.Head,
		stat:     plan.HostTools.Stat,
		find:     plan.HostTools.Find,
		getent:   plan.HostTools.Getent,
		setpriv:  plan.HostTools.Setpriv,
		env:      plan.HostTools.Env,
		date:     plan.HostTools.Date,
		rm:       plan.HostTools.Rm,
	}
	if err := r.validate(plan); err != nil {
		return nil, err
	}

	var b strings.Builder
	b.WriteString("#!/bin/sh\nset -eu\n\n")
	b.WriteString("fail() {\n    printf '%s\\n' \"$1\" >&2\n    exit 126\n}\n\n")
	b.WriteString("single_line() {\n    case \"$1\" in\n        *'\n'*) return 1 ;;\n        *) return 0 ;;\n    esac\n}\n\n")
	b.WriteString("validate_canonical_path() {\n")
	b.WriteString("    expected_path=$1\n")
	fmt.Fprintf(&b, "    resolved_path=$(%s %s %s \"$expected_path\") || fail %s\n", ShellQuote(r.tools.readlink), ShellQuote("-m"), ShellQuote("--"), ShellQuote("path cannot be canonicalized"))
	b.WriteString("    [ \"$resolved_path\" = \"$expected_path\" ] || fail 'path is no longer canonical'\n")
	b.WriteString("}\n\n")
	b.WriteString("capture_path_snapshot() {\n")
	b.WriteString("    snapshot_path=$1\n")
	b.WriteString("    if [ -e \"$snapshot_path\" ] || [ -L \"$snapshot_path\" ]; then\n")
	fmt.Fprintf(&b, "        %s %s %s %s \"$snapshot_path\" || return 1\n", ShellQuote(r.tools.stat), ShellQuote("-c"), ShellQuote("%d:%i:%f:%u:%g"), ShellQuote("--"))
	b.WriteString("    else\n        printf '%s\\n' missing\n    fi\n}\n\n")
	fmt.Fprintf(&b, "PATH=%s\nexport PATH\numask %04o\n", ShellQuote(jobExecutionPATH), plan.Umask)
	for _, path := range wrapperValidationPaths(plan) {
		fmt.Fprintf(&b, "validate_canonical_path %s\n", ShellQuote(path))
	}
	fmt.Fprintf(&b, "cd %s\n\n", ShellQuote(plan.WorkingDirectory))

	if plan.ActionType == ActionScript {
		r.renderScriptValidation(&b, plan.SourcePath)
		b.WriteString("script_snapshot=$(capture_path_snapshot \"$script_path\") || fail 'script identity cannot be read'\n")
		b.WriteString("single_line \"$script_snapshot\" || fail 'script identity is ambiguous'\n\n")
	} else if plan.ActionType == ActionSync {
		r.renderSyncValidation(&b, plan)
	}
	switch plan.Identity.Mode {
	case IdentityScriptOwner:
		r.renderScriptOwnerIdentity(&b, plan.SourcePath)
	case IdentityFixed:
		r.renderFixedIdentity(&b, plan.Identity)
	}
	if plan.NonEmptySourceDir != "" {
		r.renderNonEmptySourceCheck(&b, plan.NonEmptySourceDir)
	}
	r.renderFinalGuards(&b, plan)
	if plan.ActionType == ActionSync && plan.Sync.Backup {
		r.renderRuntimeBackup(&b, plan)
	}
	r.renderExec(&b, plan)
	return []byte(b.String()), nil
}

func (r WrapperRenderer) validate(plan ExecutionPlan) error {
	if plan.HostTools.Profile != "gnu" && plan.HostTools.Profile != "busybox" {
		return errors.New("wrapper host toolchain profile is invalid")
	}
	for name, path := range map[string]string{
		"working directory": plan.WorkingDirectory,
		"wrapper":           plan.WrapperPath,
	} {
		if err := validateAbsoluteCanonicalPath(path); err != nil {
			return fmt.Errorf("%s path: %w", name, err)
		}
	}
	for name, path := range map[string]string{
		"source":                 plan.SourcePath,
		"destination":            plan.DestinationPath,
		"non-empty source guard": plan.NonEmptySourceDir,
	} {
		if path != "" {
			if err := validateAbsoluteCanonicalPath(path); err != nil {
				return fmt.Errorf("%s path: %w", name, err)
			}
		}
	}
	if err := validateArgv(plan.Argv); err != nil {
		return err
	}
	if plan.Umask > 0o777 {
		return errors.New("wrapper umask is invalid")
	}
	if !slices.Equal(plan.LauncherEnvironment, HostLauncherEnvironment()) {
		return errors.New("wrapper launcher environment is invalid")
	}
	switch plan.ActionType {
	case ActionScript:
		if plan.DryRun || plan.SourcePath == "" || plan.Argv[0] != plan.SourcePath || plan.DestinationPath != "" || plan.NonEmptySourceDir != "" || plan.Command != "" || plan.SyncEngine != "" || plan.SyncMode != "" || plan.Sync != (SyncAction{}) {
			return errors.New("script plan fields are inconsistent")
		}
	case ActionCommand:
		if plan.DryRun || strings.TrimSpace(plan.Command) == "" || plan.SourcePath != "" || plan.DestinationPath != "" || plan.NonEmptySourceDir != "" || plan.SyncEngine != "" || plan.SyncMode != "" || plan.Sync != (SyncAction{}) || !slices.Equal(plan.Argv, []string{plan.HostTools.Shell, "-c", plan.Command}) {
			return errors.New("command plan fields are inconsistent")
		}
	case ActionSync:
		wantArgv, destructive, err := syncArgv(plan.Sync, plan.DryRun, plan.RequestedAt)
		if err != nil || plan.Command != "" || plan.SourcePath == "" || plan.DestinationPath == "" || plan.SyncEngine != plan.Sync.Engine || plan.SyncMode != plan.Sync.Mode || plan.SourcePath != plan.Sync.Source || plan.DestinationPath != plan.Sync.Dest || !slices.Equal(plan.Argv, wantArgv) {
			return errors.New("sync plan fields are inconsistent")
		}
		if plan.SourcePath == plan.DestinationPath || pathWithin(plan.SourcePath, plan.DestinationPath) || pathWithin(plan.DestinationPath, plan.SourcePath) {
			return errors.New("sync paths overlap")
		}
		if destructive != (plan.NonEmptySourceDir == plan.SourcePath) {
			return errors.New("sync destructive guard is inconsistent")
		}
	default:
		return errors.New("wrapper action type is invalid")
	}
	switch plan.Identity.Mode {
	case IdentityScriptOwner:
		if plan.ActionType != ActionScript {
			return errors.New("script-owner wrapper requires a script action")
		}
	case IdentityFixed:
		if !accountNamePattern.MatchString(plan.Identity.User) || !accountNamePattern.MatchString(plan.Identity.Group) || plan.Identity.UID < 0 || plan.Identity.GID < 0 {
			return errors.New("fixed wrapper identity is invalid")
		}
	default:
		return errors.New("wrapper identity mode is invalid")
	}

	seenEnvironment := make(map[string]bool, len(plan.Environment))
	hasPATH := false
	for _, entry := range plan.Environment {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || !environmentNamePattern.MatchString(name) || strings.IndexByte(value, 0) >= 0 {
			return fmt.Errorf("invalid wrapper environment entry %q", entry)
		}
		if name == "HOME" || name == "USER" || name == "LOGNAME" || name == "SHELL" {
			return fmt.Errorf("wrapper environment name %q is reserved", name)
		}
		if seenEnvironment[name] {
			return fmt.Errorf("duplicate wrapper environment name %q", name)
		}
		seenEnvironment[name] = true
		if name == "PATH" {
			hasPATH = value == jobExecutionPATH
			if !hasPATH {
				return errors.New("wrapper PATH must use the fixed host execution path")
			}
		}
	}
	if !hasPATH {
		return errors.New("wrapper environment requires an explicit PATH")
	}
	for _, path := range []string{plan.HostTools.Shell, r.tools.readlink, r.tools.head, r.tools.stat, r.tools.find, r.tools.getent, r.tools.setpriv, r.tools.env, r.tools.date, r.tools.rm} {
		if err := validateAbsoluteCanonicalPath(path); err != nil {
			return fmt.Errorf("wrapper tool path: %w", err)
		}
	}
	return nil
}

func wrapperValidationPaths(plan ExecutionPlan) []string {
	paths := []string{plan.WorkingDirectory}
	seen := map[string]bool{plan.WorkingDirectory: true}
	for _, path := range []string{plan.SourcePath, plan.DestinationPath} {
		if path != "" && !seen[path] {
			seen[path] = true
			paths = append(paths, path)
		}
	}
	return paths
}

func (r WrapperRenderer) renderScriptValidation(b *strings.Builder, path string) {
	fmt.Fprintf(b, "script_path=%s\n", ShellQuote(path))
	b.WriteString("[ -f \"$script_path\" ] || fail 'script is not a regular file'\n")
	b.WriteString("[ -x \"$script_path\" ] || fail 'script is not executable'\n")
	fmt.Fprintf(b, "magic=$(%s %s %s %s \"$script_path\") || fail %s\n", ShellQuote(r.tools.head), ShellQuote("-c"), ShellQuote("2"), ShellQuote("--"), ShellQuote("script shebang cannot be read"))
	b.WriteString("[ \"$magic\" = '#!' ] || fail 'script has no shebang'\n")
}

func (r WrapperRenderer) renderScriptOwnerIdentity(b *strings.Builder, _ string) {
	b.WriteString("file_identity=${script_snapshot#*:*:*:}\n")
	b.WriteString("case \"$file_identity\" in\n    *:*) ;;\n    *) fail 'script owner is malformed' ;;\nesac\n")
	b.WriteString("run_uid=${file_identity%%:*}\nrun_gid=${file_identity#*:}\n")
	b.WriteString("case \"$run_uid\" in ''|*[!0-9]*) fail 'script UID is invalid' ;; esac\n")
	b.WriteString("case \"$run_gid\" in ''|*[!0-9]*) fail 'script GID is invalid' ;; esac\n")
	b.WriteString("[ \"$run_uid\" != 0 ] && [ \"$run_gid\" != 0 ] || fail 'script owner may not resolve to root'\n\n")

	fmt.Fprintf(b, "passwd_entry=$(%s %s \"$run_uid\") || fail %s\n", ShellQuote(r.tools.getent), ShellQuote("passwd"), ShellQuote("script UID has no host account"))
	b.WriteString("single_line \"$passwd_entry\" || fail 'script UID has multiple host accounts'\n")
	b.WriteString("IFS=: read -r run_user _ account_uid account_gid _ run_home run_shell <<EOF\n$passwd_entry\nEOF\n")
	b.WriteString("case \"$account_uid\" in ''|*[!0-9]*) fail 'host account UID is invalid' ;; esac\n")
	b.WriteString("case \"$account_gid\" in ''|*[!0-9]*) fail 'host account GID is invalid' ;; esac\n")
	b.WriteString("[ \"$account_uid\" = \"$run_uid\" ] || fail 'host account UID changed'\n")
	b.WriteString("[ -n \"$run_user\" ] && [ -n \"$run_home\" ] && [ -n \"$run_shell\" ] || fail 'host account is incomplete'\n")
	fmt.Fprintf(b, "passwd_by_name=$(%s %s \"$run_user\") || fail %s\n", ShellQuote(r.tools.getent), ShellQuote("passwd"), ShellQuote("host account name cannot be resolved"))
	b.WriteString("single_line \"$passwd_by_name\" || fail 'host account name is ambiguous'\n")
	b.WriteString("[ \"$passwd_by_name\" = \"$passwd_entry\" ] || fail 'host account name and UID disagree'\n\n")

	fmt.Fprintf(b, "group_entry=$(%s %s \"$run_gid\") || fail %s\n", ShellQuote(r.tools.getent), ShellQuote("group"), ShellQuote("script GID has no host group"))
	b.WriteString("single_line \"$group_entry\" || fail 'script GID has multiple host groups'\n")
	b.WriteString("IFS=: read -r run_group _ group_gid _ <<EOF\n$group_entry\nEOF\n")
	b.WriteString("case \"$group_gid\" in ''|*[!0-9]*) fail 'host group GID is invalid' ;; esac\n")
	b.WriteString("[ \"$group_gid\" = \"$run_gid\" ] || fail 'host group GID changed'\n")
	b.WriteString("[ -n \"$run_group\" ] || fail 'host group is incomplete'\n")
	fmt.Fprintf(b, "group_by_name=$(%s %s \"$run_group\") || fail %s\n", ShellQuote(r.tools.getent), ShellQuote("group"), ShellQuote("host group name cannot be resolved"))
	b.WriteString("single_line \"$group_by_name\" || fail 'host group name is ambiguous'\n")
	b.WriteString("[ \"$group_by_name\" = \"$group_entry\" ] || fail 'host group name and GID disagree'\n\n")
}

func (r WrapperRenderer) renderFixedIdentity(b *strings.Builder, identity Identity) {
	fmt.Fprintf(b, "expected_user=%s\nexpected_uid=%s\nexpected_group=%s\nexpected_gid=%s\n", ShellQuote(identity.User), ShellQuote(strconv.Itoa(identity.UID)), ShellQuote(identity.Group), ShellQuote(strconv.Itoa(identity.GID)))
	fmt.Fprintf(b, "passwd_entry=$(%s %s \"$expected_user\") || fail %s\n", ShellQuote(r.tools.getent), ShellQuote("passwd"), ShellQuote("fixed host account name cannot be resolved"))
	b.WriteString("single_line \"$passwd_entry\" || fail 'fixed host account name is ambiguous'\n")
	b.WriteString("IFS=: read -r run_user _ run_uid account_gid _ run_home run_shell <<EOF\n$passwd_entry\nEOF\n")
	b.WriteString("case \"$run_uid\" in ''|*[!0-9]*) fail 'fixed host account UID is invalid' ;; esac\n")
	b.WriteString("case \"$account_gid\" in ''|*[!0-9]*) fail 'fixed host account GID is invalid' ;; esac\n")
	b.WriteString("[ \"$run_user\" = \"$expected_user\" ] && [ \"$run_uid\" = \"$expected_uid\" ] || fail 'fixed host account name and UID disagree'\n")
	b.WriteString("[ -n \"$run_home\" ] && [ -n \"$run_shell\" ] || fail 'fixed host account is incomplete'\n")
	fmt.Fprintf(b, "passwd_by_uid=$(%s %s \"$expected_uid\") || fail %s\n", ShellQuote(r.tools.getent), ShellQuote("passwd"), ShellQuote("fixed host account UID cannot be resolved"))
	b.WriteString("single_line \"$passwd_by_uid\" || fail 'fixed host account UID is ambiguous'\n")
	b.WriteString("[ \"$passwd_by_uid\" = \"$passwd_entry\" ] || fail 'fixed host account UID was reassigned'\n\n")

	fmt.Fprintf(b, "group_entry=$(%s %s \"$expected_group\") || fail %s\n", ShellQuote(r.tools.getent), ShellQuote("group"), ShellQuote("fixed host group name cannot be resolved"))
	b.WriteString("single_line \"$group_entry\" || fail 'fixed host group name is ambiguous'\n")
	b.WriteString("IFS=: read -r run_group _ run_gid _ <<EOF\n$group_entry\nEOF\n")
	b.WriteString("case \"$run_gid\" in ''|*[!0-9]*) fail 'fixed host group GID is invalid' ;; esac\n")
	b.WriteString("[ \"$run_group\" = \"$expected_group\" ] && [ \"$run_gid\" = \"$expected_gid\" ] || fail 'fixed host group name and GID disagree'\n")
	fmt.Fprintf(b, "group_by_gid=$(%s %s \"$expected_gid\") || fail %s\n", ShellQuote(r.tools.getent), ShellQuote("group"), ShellQuote("fixed host group GID cannot be resolved"))
	b.WriteString("single_line \"$group_by_gid\" || fail 'fixed host group GID is ambiguous'\n")
	b.WriteString("[ \"$group_by_gid\" = \"$group_entry\" ] || fail 'fixed host group GID was reassigned'\n\n")
}

func (r WrapperRenderer) renderNonEmptySourceCheck(b *strings.Builder, source string) {
	fmt.Fprintf(b, "source_path=%s\n", ShellQuote(source))
	b.WriteString("[ -d \"$source_path\" ] || fail 'destructive sync source is missing'\n")
	fmt.Fprintf(b, "first_entry=$(%s \"$source_path\" %s %s %s %s %s %s) || fail %s\n", ShellQuote(r.tools.find), ShellQuote("-mindepth"), ShellQuote("1"), ShellQuote("-maxdepth"), ShellQuote("1"), ShellQuote("-print"), ShellQuote("-quit"), ShellQuote("destructive sync source cannot be read"))
	b.WriteString("[ -n \"$first_entry\" ] || fail 'destructive sync source is empty'\n\n")
}

func (r WrapperRenderer) renderSyncValidation(b *strings.Builder, plan ExecutionPlan) {
	fmt.Fprintf(b, "source_path=%s\ndestination_path=%s\n", ShellQuote(plan.SourcePath), ShellQuote(plan.DestinationPath))
	b.WriteString("[ -d \"$source_path\" ] || fail 'sync source is missing'\n")
	b.WriteString("if [ -e \"$destination_path\" ] || [ -L \"$destination_path\" ]; then [ -d \"$destination_path\" ] || fail 'sync destination is not a directory'; fi\n")
	b.WriteString("source_snapshot=$(capture_path_snapshot \"$source_path\") || fail 'sync source identity cannot be read'\n")
	b.WriteString("destination_snapshot=$(capture_path_snapshot \"$destination_path\") || fail 'sync destination identity cannot be read'\n")
	b.WriteString("single_line \"$source_snapshot\" && single_line \"$destination_snapshot\" || fail 'sync path identity is ambiguous'\n\n")
}

func (r WrapperRenderer) renderFinalGuards(b *strings.Builder, plan ExecutionPlan) {
	switch plan.ActionType {
	case ActionScript:
		b.WriteString("validate_canonical_path \"$script_path\"\n")
		b.WriteString("final_script_snapshot=$(capture_path_snapshot \"$script_path\") || fail 'script identity cannot be revalidated'\n")
		b.WriteString("[ \"$final_script_snapshot\" = \"$script_snapshot\" ] || fail 'script changed before privilege drop'\n")
		b.WriteString("[ -f \"$script_path\" ] && [ -x \"$script_path\" ] || fail 'script type changed before privilege drop'\n\n")
	case ActionSync:
		b.WriteString("validate_canonical_path \"$source_path\"\nvalidate_canonical_path \"$destination_path\"\n")
		b.WriteString("final_source_snapshot=$(capture_path_snapshot \"$source_path\") || fail 'sync source identity cannot be revalidated'\n")
		b.WriteString("final_destination_snapshot=$(capture_path_snapshot \"$destination_path\") || fail 'sync destination identity cannot be revalidated'\n")
		b.WriteString("[ \"$final_source_snapshot\" = \"$source_snapshot\" ] || fail 'sync source changed before privilege drop'\n")
		b.WriteString("[ \"$final_destination_snapshot\" = \"$destination_snapshot\" ] || fail 'sync destination changed before privilege drop'\n")
		b.WriteString("[ -d \"$source_path\" ] || fail 'sync source type changed before privilege drop'\n")
		b.WriteString("if [ \"$destination_snapshot\" != missing ]; then [ -d \"$destination_path\" ] || fail 'sync destination type changed before privilege drop'; fi\n")
		if plan.NonEmptySourceDir != "" {
			r.renderNonEmptySourceCheck(b, plan.NonEmptySourceDir)
		}
	}
}

func (r WrapperRenderer) renderRuntimeBackup(b *strings.Builder, plan ExecutionPlan) {
	backupRoot := filepath.Join(plan.DestinationPath, ".sb-backup")
	fmt.Fprintf(b, "backup_stamp=$(%s %s %s) || fail %s\n", ShellQuote(r.tools.date), ShellQuote("-u"), ShellQuote("+%Y%m%d-%H%M%S"), ShellQuote("backup timestamp cannot be generated"))
	fmt.Fprintf(b, "backup_root=%s\n", ShellQuote(backupRoot))
	b.WriteString("backup_dir=\"$backup_root/$backup_stamp-$$\"\n")
	keep := 0
	if !plan.DryRun {
		keep = plan.Sync.BackupKeep
	}
	fmt.Fprintf(b, "backup_keep=%s\n\n", ShellQuote(strconv.Itoa(keep)))
}

func (r WrapperRenderer) renderBackupRetention(b *strings.Builder) {
	b.WriteString("if [ \"$run_status\" -eq 0 ] && [ \"$backup_keep\" -gt 0 ]; then\n")
	b.WriteString("    set -- \"$backup_root\"/[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]-[0-9][0-9][0-9][0-9][0-9][0-9]-[0-9]*\n")
	b.WriteString("    if [ \"$1\" != \"$backup_root/[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]-[0-9][0-9][0-9][0-9][0-9][0-9]-[0-9]*\" ]; then\n")
	b.WriteString("        while [ \"$#\" -gt \"$backup_keep\" ]; do\n")
	b.WriteString("            candidate=$1\n")
	b.WriteString("            [ -d \"$candidate\" ] || fail 'backup retention candidate is not a directory'\n")
	fmt.Fprintf(b, "            %s %s %s \"$candidate\" || fail %s\n", ShellQuote(r.tools.rm), ShellQuote("-r"), ShellQuote("-f"), ShellQuote("backup retention removal failed"))
	b.WriteString("            shift\n")
	b.WriteString("        done\n")
	b.WriteString("    fi\n")
	b.WriteString("fi\n")
}

func (r WrapperRenderer) renderExec(b *strings.Builder, plan ExecutionPlan) {
	if plan.ActionType == ActionSync && plan.Sync.Backup {
		b.WriteString("if ")
	} else {
		b.WriteString("exec ")
	}
	fmt.Fprintf(b, "%s %s \"$run_uid\" %s \"$run_gid\" %s %s %s \\\n", ShellQuote(r.tools.setpriv), ShellQuote("--reuid"), ShellQuote("--regid"), ShellQuote("--init-groups"), ShellQuote("--reset-env"), ShellQuote("--"))
	fmt.Fprintf(b, "    %s %s %s \\\n", ShellQuote(r.tools.env), ShellQuote("-i"), ShellQuote("--"))
	b.WriteString("    \"HOME=$run_home\" \\\n    \"USER=$run_user\" \\\n    \"LOGNAME=$run_user\" \\\n    \"SHELL=$run_shell\" \\\n")
	for _, entry := range plan.Environment {
		fmt.Fprintf(b, "    %s \\\n", ShellQuote(entry))
	}
	for index, arg := range plan.Argv {
		word := ShellQuote(arg)
		if arg == syncBackupDirToken {
			word = `"$backup_dir"`
		}
		if index == len(plan.Argv)-1 {
			fmt.Fprintf(b, "    %s", word)
		} else {
			fmt.Fprintf(b, "    %s \\\n", word)
		}
	}
	if plan.ActionType != ActionSync || !plan.Sync.Backup {
		b.WriteByte('\n')
		return
	}
	b.WriteString("; then\n    run_status=0\nelse\n    run_status=$?\nfi\n")
	r.renderBackupRetention(b)
	b.WriteString("exit \"$run_status\"\n")
}
