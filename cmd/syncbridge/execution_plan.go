package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	defaultExecutionTimeout = time.Hour
	maxExecutionTimeout     = 7 * 24 * time.Hour
	defaultStopGrace        = 10 * time.Second
	maxStopGrace            = 5 * time.Minute
	defaultUmask            = 0o022
	jobExecutionPATH        = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
)

var (
	environmentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	accountNamePattern     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*[$]?$`)
	runIDPattern           = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)
	instanceIDPattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	bandwidthLimitPattern  = regexp.MustCompile(`(?i)^[0-9]+(?:\.[0-9]+)?[kmgtpe]?(?:i?b)?$`)
)

// RunOrigin identifies the source of an immutable execution request.
type RunOrigin string

const (
	RunOriginManual RunOrigin = "manual"
	RunOriginCron   RunOrigin = "cron"
	RunOriginWatch  RunOrigin = "watch"
	RunOriginSystem RunOrigin = "system"
)

// RunRequest is the immutable request metadata copied into an execution plan.
// JobID and Revision may be zero when the caller already supplied the exact job
// snapshot; non-zero values are checked for stale or mismatched requests.
type RunRequest struct {
	RunID       string
	JobID       int
	Revision    uint64
	Origin      RunOrigin
	DryRun      bool
	RequestedAt time.Time
}

// ExecutionPlan is the complete validated input consumed by the wrapper and
// host executor. Argv and Environment contain individual arguments, never a
// composed shell command.
type ExecutionPlan struct {
	RunID               string
	JobID               int
	Revision            uint64
	Origin              RunOrigin
	DryRun              bool
	RequestedAt         time.Time
	ActionType          ActionType
	Command             string
	SyncEngine          string
	SyncMode            string
	Sync                SyncAction
	Argv                []string
	Identity            Identity
	Environment         []string
	WorkingDirectory    string
	Umask               uint32
	Timeout             time.Duration
	StopGrace           time.Duration
	WrapperPath         string
	SourcePath          string
	DestinationPath     string
	NonEmptySourceDir   string
	HostTools           HostToolPaths
	LauncherEnvironment []string
}

// HostPathInfo describes a path as observed in the host mount namespace.
type HostPathInfo struct {
	CanonicalPath string
	Exists        bool
	Regular       bool
	Directory     bool
	Executable    bool
	HasShebang    bool
	Empty         bool
	UID           int
	GID           int
}

// HostUser and HostGroup are account database records from the host namespace.
type HostUser struct {
	Name  string
	UID   int
	GID   int
	Home  string
	Shell string
}

type HostGroup struct {
	Name string
	GID  int
}

// HostInspector performs every path and account lookup in the host namespace.
// Implementations must not inspect container-local paths or account databases.
type HostInspector interface {
	InspectPath(context.Context, string) (HostPathInfo, error)
	LookupUser(context.Context, string) (HostUser, error)
	LookupGroup(context.Context, string) (HostGroup, error)
}

// PlanCompiler validates immutable job snapshots and emits safe direct argv.
type PlanCompiler struct {
	instanceID   string
	capabilities CapabilityReport
	inspector    HostInspector
}

func NewPlanCompiler(instanceID string, capabilities CapabilityReport, inspector HostInspector) *PlanCompiler {
	return &PlanCompiler{instanceID: instanceID, capabilities: capabilities, inspector: inspector}
}

// Compile validates job data against the current host namespace. Dynamic script
// ownership is intentionally not frozen in the returned plan: the wrapper
// resolves and verifies it again immediately before every execution.
func (c *PlanCompiler) Compile(ctx context.Context, job Job, request RunRequest) (ExecutionPlan, error) {
	if c == nil || c.inspector == nil {
		return ExecutionPlan{}, errors.New("host inspector is required")
	}
	if !instanceIDPattern.MatchString(c.instanceID) {
		return ExecutionPlan{}, errors.New("instance ID is invalid")
	}
	if job.ID <= 0 {
		return ExecutionPlan{}, errors.New("job ID must be positive")
	}
	if job.SchemaVersion != 2 {
		return ExecutionPlan{}, errors.New("only validated schema-v2 jobs can be compiled")
	}
	if job.NeedsReview {
		return ExecutionPlan{}, errors.New("job requires review before execution")
	}
	if request.JobID != 0 && request.JobID != job.ID {
		return ExecutionPlan{}, errors.New("run request job ID does not match snapshot")
	}
	if request.Revision != 0 && request.Revision != job.Revision {
		return ExecutionPlan{}, errors.New("run request revision does not match snapshot")
	}
	if !runIDPattern.MatchString(request.RunID) {
		return ExecutionPlan{}, errors.New("run ID is invalid")
	}
	switch request.Origin {
	case RunOriginManual, RunOriginCron, RunOriginWatch, RunOriginSystem:
	default:
		return ExecutionPlan{}, errors.New("run origin is invalid")
	}
	if request.DryRun && job.Action.Type != ActionSync {
		return ExecutionPlan{}, errors.New("dry run is supported only for sync actions")
	}

	required := []CapabilityCode{
		CapHostNamespace,
		CapHostPID1,
		CapHostRoot,
		CapIdentityDrop,
		CapAccountLookup,
		CapShell,
		CapHostToolchain,
	}
	switch job.Action.Type {
	case ActionSync:
		switch job.Action.Sync.Engine {
		case "rsync":
			required = append(required, CapRsync)
		case "rclone":
			required = append(required, CapRclone)
		}
	}
	if err := c.capabilities.Require(required...); err != nil {
		return ExecutionPlan{}, fmt.Errorf("compile execution plan: %w", err)
	}
	hostTools, err := c.capabilities.HostTools()
	if err != nil {
		return ExecutionPlan{}, fmt.Errorf("compile execution plan: %w", err)
	}

	workingDirectory := job.Execution.WorkingDirectory
	if workingDirectory == "" {
		workingDirectory = "/"
	}
	workingInfo, err := c.inspectCanonicalPath(ctx, workingDirectory)
	if err != nil {
		return ExecutionPlan{}, fmt.Errorf("working directory: %w", err)
	}
	if !workingInfo.Exists || !workingInfo.Directory {
		return ExecutionPlan{}, errors.New("working directory is not an existing directory")
	}

	identity, err := c.validateIdentity(ctx, job)
	if err != nil {
		return ExecutionPlan{}, err
	}
	requestedAt := request.RequestedAt
	if requestedAt.IsZero() {
		requestedAt = time.Now().UTC()
	}
	argv, sourcePath, destinationPath, nonEmptySource, err := c.compileAction(ctx, job.Action, identity, request.DryRun, requestedAt)
	if err != nil {
		return ExecutionPlan{}, err
	}
	environment, err := compileEnvironment(job, request)
	if err != nil {
		return ExecutionPlan{}, err
	}
	timeout, stopGrace, umask, err := compileExecutionBounds(job.Execution)
	if err != nil {
		return ExecutionPlan{}, err
	}

	return ExecutionPlan{
		RunID:               request.RunID,
		JobID:               job.ID,
		Revision:            job.Revision,
		Origin:              request.Origin,
		DryRun:              request.DryRun,
		RequestedAt:         requestedAt,
		ActionType:          job.Action.Type,
		Command:             job.Action.Command,
		SyncEngine:          job.Action.Sync.Engine,
		SyncMode:            job.Action.Sync.Mode,
		Sync:                job.Action.Sync,
		Argv:                argv,
		Identity:            identity,
		Environment:         environment,
		WorkingDirectory:    workingDirectory,
		Umask:               umask,
		Timeout:             timeout,
		StopGrace:           stopGrace,
		WrapperPath:         fmt.Sprintf("/var/lib/syncbridge/instances/%s/jobs/%d/run.sh", c.instanceID, job.ID),
		SourcePath:          sourcePath,
		DestinationPath:     destinationPath,
		NonEmptySourceDir:   nonEmptySource,
		HostTools:           hostTools,
		LauncherEnvironment: HostLauncherEnvironment(),
	}, nil
}

func (c *PlanCompiler) validateIdentity(ctx context.Context, job Job) (Identity, error) {
	identity := job.Identity
	switch identity.Mode {
	case IdentityScriptOwner:
		if job.Action.Type != ActionScript {
			return Identity{}, errors.New("script-owner identity is only valid for scripts")
		}
		info, err := c.inspectCanonicalPath(ctx, job.Action.ScriptPath)
		if err != nil {
			return Identity{}, fmt.Errorf("script path: %w", err)
		}
		if !info.Exists || !info.Regular || !info.Executable || !info.HasShebang {
			return Identity{}, errors.New("script must be a regular executable file with a shebang")
		}
		if info.UID == 0 || info.GID == 0 {
			return Identity{}, errors.New("script-owner cannot dynamically select root UID or GID")
		}
		user, err := c.inspector.LookupUser(ctx, strconv.Itoa(info.UID))
		if err != nil || user.UID != info.UID || user.Name == "" {
			return Identity{}, errors.New("script owner UID has no coherent host account")
		}
		group, err := c.inspector.LookupGroup(ctx, strconv.Itoa(info.GID))
		if err != nil || group.GID != info.GID || group.Name == "" {
			return Identity{}, errors.New("script owner GID has no coherent host group")
		}
		return Identity{Mode: IdentityScriptOwner}, nil
	case IdentityFixed:
		if !accountNamePattern.MatchString(identity.User) || !accountNamePattern.MatchString(identity.Group) || identity.UID < 0 || identity.GID < 0 {
			return Identity{}, errors.New("fixed identity is incomplete or invalid")
		}
		user, err := c.inspector.LookupUser(ctx, identity.User)
		if err != nil || user.Name != identity.User || user.UID != identity.UID {
			return Identity{}, errors.New("fixed user name and UID do not match the host account")
		}
		group, err := c.inspector.LookupGroup(ctx, identity.Group)
		if err != nil || group.Name != identity.Group || group.GID != identity.GID {
			return Identity{}, errors.New("fixed group name and GID do not match the host group")
		}
		return identity, nil
	default:
		return Identity{}, errors.New("identity mode must be script-owner or fixed")
	}
}

func (c *PlanCompiler) compileAction(ctx context.Context, action Action, identity Identity, dryRun bool, requestedAt time.Time) ([]string, string, string, string, error) {
	switch action.Type {
	case ActionScript:
		info, err := c.inspectCanonicalPath(ctx, action.ScriptPath)
		if err != nil {
			return nil, "", "", "", fmt.Errorf("script path: %w", err)
		}
		if !info.Exists || !info.Regular || !info.Executable || !info.HasShebang {
			return nil, "", "", "", errors.New("script must be a regular executable file with a shebang")
		}
		argv := make([]string, 1, len(action.ScriptArgs)+1)
		argv[0] = action.ScriptPath
		argv = append(argv, action.ScriptArgs...)
		if err := validateArgv(argv); err != nil {
			return nil, "", "", "", err
		}
		return argv, action.ScriptPath, "", "", nil
	case ActionCommand:
		if identity.Mode != IdentityFixed {
			return nil, "", "", "", errors.New("command actions require a fixed identity")
		}
		if strings.TrimSpace(action.Command) == "" || strings.IndexByte(action.Command, 0) >= 0 {
			return nil, "", "", "", errors.New("command is empty or invalid")
		}
		tools, err := c.capabilities.HostTools()
		if err != nil {
			return nil, "", "", "", err
		}
		return []string{tools.Shell, "-c", action.Command}, "", "", "", nil
	case ActionSync:
		if identity.Mode != IdentityFixed {
			return nil, "", "", "", errors.New("sync actions require a fixed identity")
		}
		return c.compileSync(ctx, action.Sync, dryRun, requestedAt)
	default:
		return nil, "", "", "", errors.New("unknown action type")
	}
}

func (c *PlanCompiler) compileSync(ctx context.Context, syncAction SyncAction, dryRun bool, requestedAt time.Time) ([]string, string, string, string, error) {
	if err := validateSyncActionOptions(syncAction); err != nil {
		return nil, "", "", "", err
	}
	source, err := c.inspectCanonicalPath(ctx, syncAction.Source)
	if err != nil {
		return nil, "", "", "", fmt.Errorf("sync source: %w", err)
	}
	destination, err := c.inspectCanonicalPath(ctx, syncAction.Dest)
	if err != nil {
		return nil, "", "", "", fmt.Errorf("sync destination: %w", err)
	}
	if source.CanonicalPath == destination.CanonicalPath {
		return nil, "", "", "", errors.New("sync source and destination must differ")
	}
	if pathWithin(source.CanonicalPath, destination.CanonicalPath) {
		return nil, "", "", "", errors.New("sync destination must not be inside source")
	}
	if pathWithin(destination.CanonicalPath, source.CanonicalPath) {
		return nil, "", "", "", errors.New("sync destination must not be an ancestor of source")
	}
	if !source.Exists || !source.Directory {
		return nil, "", "", "", errors.New("sync source must be an existing directory")
	}
	if destination.Exists && !destination.Directory {
		return nil, "", "", "", errors.New("sync destination must be a directory when it exists")
	}
	argv, destructive, err := syncArgv(syncAction, dryRun, requestedAt)
	if err != nil {
		return nil, "", "", "", err
	}
	if destructive && source.Empty {
		return nil, "", "", "", errors.New("destructive sync source must not be empty")
	}
	if err := validateArgv(argv); err != nil {
		return nil, "", "", "", err
	}
	if destructive {
		return argv, syncAction.Source, syncAction.Dest, syncAction.Source, nil
	}
	return argv, syncAction.Source, syncAction.Dest, "", nil
}

func validateSyncActionOptions(syncAction SyncAction) error {
	if syncAction.Engine != "rsync" && syncAction.Engine != "rclone" {
		return errors.New("sync engine must be rsync or rclone")
	}
	if syncAction.Mode != "add" && syncAction.Mode != "mirror" && syncAction.Mode != "move" {
		return errors.New("sync mode must be add, mirror, or move")
	}
	if syncAction.Compare != "" && syncAction.Compare != "time" && syncAction.Compare != "checksum" {
		return errors.New("sync compare must be time or checksum")
	}
	if syncAction.Bwlimit != "" && !bandwidthLimitPattern.MatchString(syncAction.Bwlimit) {
		return errors.New("sync bandwidth limit is invalid")
	}
	if syncAction.BackupKeep < 0 {
		return errors.New("sync backup retention must be non-negative")
	}
	if syncAction.BackupKeep > 0 && !syncAction.Backup {
		return errors.New("sync backup retention requires backup")
	}
	if syncAction.MaxDel < 0 {
		return errors.New("sync max-delete must be non-negative")
	}
	if syncAction.MaxDel > 0 && syncAction.Mode != "mirror" {
		return errors.New("sync max-delete is valid only in mirror mode")
	}
	if syncAction.SysBackup && syncAction.Engine != "rsync" {
		return errors.New("system backup is supported only by rsync")
	}
	if strings.IndexByte(syncAction.Exclude, 0) >= 0 || strings.IndexFunc(syncAction.Exclude, func(r rune) bool { return r < 0x20 && r != '\t' }) >= 0 {
		return errors.New("sync exclude contains control characters")
	}
	for _, pattern := range splitCSV(syncAction.Exclude) {
		if strings.HasPrefix(pattern, "-") {
			return errors.New("sync exclude patterns may not start with a dash")
		}
	}
	return nil
}

const syncBackupDirToken = "__SYNCBRIDGE_BACKUP_DIR__"

func syncArgv(syncAction SyncAction, dryRun bool, requestedAt time.Time) ([]string, bool, error) {
	if err := validateSyncActionOptions(syncAction); err != nil {
		return nil, false, err
	}
	destructive := syncAction.Mode == "mirror" || syncAction.Mode == "move"
	backupDir := syncBackupDirToken
	bwlimit := strings.TrimSuffix(strings.TrimSuffix(syncAction.Bwlimit, "B"), "b")

	if syncAction.Engine == "rsync" {
		argv := []string{"rsync", "-a", "--info=progress2,stats2", "--human-readable"}
		if syncAction.SysBackup {
			argv = append(argv, "-HAX", "--numeric-ids", "--fake-super")
		}
		switch syncAction.Mode {
		case "mirror":
			argv = append(argv, "--delete")
		case "move":
			argv = append(argv, "--remove-source-files")
		}
		if syncAction.Compare == "checksum" {
			argv = append(argv, "--checksum")
		}
		if bwlimit != "" {
			argv = append(argv, "--bwlimit", bwlimit)
		}
		if syncAction.Backup {
			argv = append(argv, "--backup", "--backup-dir", backupDir, "--exclude", ".sb-backup")
		}
		if syncAction.MaxDel > 0 {
			argv = append(argv, "--max-delete", strconv.Itoa(syncAction.MaxDel))
		}
		if syncAction.SkipNew {
			argv = append(argv, "--update")
		}
		for _, pattern := range splitCSV(syncAction.Exclude) {
			argv = append(argv, "--exclude", pattern)
		}
		if dryRun {
			argv = append(argv, "--dry-run", "--itemize-changes")
		}
		argv = append(argv, "--", directoryArg(syncAction.Source), directoryArg(syncAction.Dest))
		return argv, destructive, nil
	}

	verb := map[string]string{"add": "copy", "mirror": "sync", "move": "move"}[syncAction.Mode]
	argv := []string{"rclone", verb, syncAction.Source, syncAction.Dest, "--progress", "--stats=1s", "--stats-one-line"}
	if syncAction.Compare == "checksum" {
		argv = append(argv, "--checksum")
	}
	if bwlimit != "" {
		argv = append(argv, "--bwlimit", bwlimit)
	}
	if syncAction.Backup {
		argv = append(argv, "--backup-dir", backupDir, "--exclude", ".sb-backup/**")
	}
	if syncAction.MaxDel > 0 {
		argv = append(argv, "--max-delete", strconv.Itoa(syncAction.MaxDel))
	}
	if syncAction.SkipNew {
		argv = append(argv, "--update")
	}
	for _, pattern := range splitCSV(syncAction.Exclude) {
		argv = append(argv, "--exclude", pattern)
	}
	if dryRun {
		argv = append(argv, "--dry-run")
	}
	return argv, destructive, nil
}

func (c *PlanCompiler) inspectCanonicalPath(ctx context.Context, value string) (HostPathInfo, error) {
	if err := validateAbsoluteCanonicalPath(value); err != nil {
		return HostPathInfo{}, err
	}
	info, err := c.inspector.InspectPath(ctx, value)
	if err != nil {
		return HostPathInfo{}, fmt.Errorf("inspect host path: %w", err)
	}
	if info.CanonicalPath != value {
		return HostPathInfo{}, fmt.Errorf("path is not host-canonical: %q resolves to %q", value, info.CanonicalPath)
	}
	return info, nil
}

func validateAbsoluteCanonicalPath(value string) error {
	if value == "" || !filepath.IsAbs(value) || filepath.Clean(value) != value {
		return errors.New("path must be absolute and lexically canonical")
	}
	if strings.IndexByte(value, 0) >= 0 || strings.IndexFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return errors.New("path contains control characters")
	}
	return nil
}

func pathWithin(parent, candidate string) bool {
	if parent == "/" {
		return candidate != "/"
	}
	return strings.HasPrefix(candidate, parent+"/")
}

func directoryArg(value string) string {
	if value == "/" || strings.HasSuffix(value, "/") {
		return value
	}
	return value + "/"
}

func validateArgv(argv []string) error {
	if len(argv) == 0 || argv[0] == "" {
		return errors.New("action argv is empty")
	}
	for _, arg := range argv {
		if strings.IndexByte(arg, 0) >= 0 {
			return errors.New("action argv contains a NUL byte")
		}
	}
	return nil
}

func compileExecutionBounds(policy ExecutionPolicy) (time.Duration, time.Duration, uint32, error) {
	if policy.TimeoutSeconds < 0 || policy.TimeoutSeconds > int(maxExecutionTimeout/time.Second) {
		return 0, 0, 0, errors.New("timeout must be between 1 second and 7 days, or zero for the default")
	}
	if policy.StopGraceSeconds < 0 || policy.StopGraceSeconds > int(maxStopGrace/time.Second) {
		return 0, 0, 0, errors.New("stop grace must be between 1 and 300 seconds, or zero for the default")
	}
	if policy.Umask > 0o777 {
		return 0, 0, 0, errors.New("umask must be between 0000 and 0777")
	}
	timeout := time.Duration(policy.TimeoutSeconds) * time.Second
	if timeout == 0 {
		timeout = defaultExecutionTimeout
	}
	stopGrace := time.Duration(policy.StopGraceSeconds) * time.Second
	if stopGrace == 0 {
		stopGrace = defaultStopGrace
	}
	umask := policy.Umask
	if umask == 0 {
		umask = defaultUmask
	}
	return timeout, stopGrace, umask, nil
}

func compileEnvironment(job Job, request RunRequest) ([]string, error) {
	values := map[string]string{
		"PATH":                    jobExecutionPATH,
		"TZ":                      "UTC",
		"LANG":                    "C",
		"LC_ALL":                  "C",
		"SYNCBRIDGE_RUN_ID":       request.RunID,
		"SYNCBRIDGE_JOB_ID":       strconv.Itoa(job.ID),
		"SYNCBRIDGE_JOB_REVISION": strconv.FormatUint(job.Revision, 10),
		"SYNCBRIDGE_RUN_ORIGIN":   string(request.Origin),
		"SYNCBRIDGE_DRY_RUN":      strconv.FormatBool(request.DryRun),
	}
	reserved := map[string]bool{
		"HOME": true, "USER": true, "LOGNAME": true, "SHELL": true, "PATH": true,
		"SYNCBRIDGE_RUN_ID": true, "SYNCBRIDGE_JOB_ID": true,
		"SYNCBRIDGE_JOB_REVISION": true, "SYNCBRIDGE_RUN_ORIGIN": true,
		"SYNCBRIDGE_DRY_RUN": true,
	}
	seen := make(map[string]bool, len(job.Execution.Environment))
	for _, entry := range job.Execution.Environment {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || !environmentNamePattern.MatchString(name) || strings.IndexByte(value, 0) >= 0 {
			return nil, fmt.Errorf("invalid environment entry %q", entry)
		}
		if reserved[name] {
			return nil, fmt.Errorf("environment name %q is reserved", name)
		}
		if seen[name] {
			return nil, fmt.Errorf("duplicate environment name %q", name)
		}
		seen[name] = true
		values[name] = value
	}

	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	environment := make([]string, 0, len(names))
	for _, name := range names {
		environment = append(environment, name+"="+values[name])
	}
	return environment, nil
}

// runnerHostInspector obtains filesystem and NSS facts only through direct host
// argv. It never consults the container filesystem or account database.
type runnerHostInspector struct {
	runner HostCommandRunner
	tools  HostToolPaths
}

func NewRunnerHostInspector(runner HostCommandRunner, tools HostToolPaths) HostInspector {
	return &runnerHostInspector{runner: runner, tools: tools}
}

func (i *runnerHostInspector) InspectPath(ctx context.Context, path string) (HostPathInfo, error) {
	if i == nil || i.runner == nil {
		return HostPathInfo{}, errors.New("host command runner is required")
	}
	canonicalResult := i.runner.Run(ctx, i.tools.Readlink, "-m", "--", path)
	if canonicalResult.Err != nil {
		return HostPathInfo{}, errors.New("canonicalize host path")
	}
	canonical, err := singleOutputLine(canonicalResult.Stdout)
	if err != nil {
		return HostPathInfo{}, fmt.Errorf("canonicalize host path: %w", err)
	}
	info := HostPathInfo{CanonicalPath: canonical}

	statResult := i.runner.Run(ctx, i.tools.Stat, "-c", "%f:%u:%g", "--", path)
	if statResult.Err != nil {
		missingResult := i.runner.Run(ctx, i.tools.Test, "!", "-e", path)
		if missingResult.Err == nil {
			return info, nil
		}
		return HostPathInfo{}, errors.New("stat host path")
	}
	statLine, err := singleOutputLine(statResult.Stdout)
	if err != nil {
		return HostPathInfo{}, fmt.Errorf("stat host path: %w", err)
	}
	parts := strings.Split(statLine, ":")
	if len(parts) != 3 {
		return HostPathInfo{}, errors.New("stat host path returned malformed data")
	}
	mode, err := strconv.ParseUint(parts[0], 16, 32)
	if err != nil {
		return HostPathInfo{}, errors.New("stat host path returned an invalid mode")
	}
	uid, err := strconv.Atoi(parts[1])
	if err != nil || uid < 0 {
		return HostPathInfo{}, errors.New("stat host path returned an invalid UID")
	}
	gid, err := strconv.Atoi(parts[2])
	if err != nil || gid < 0 {
		return HostPathInfo{}, errors.New("stat host path returned an invalid GID")
	}
	info.Exists = true
	info.Regular = mode&0o170000 == 0o100000
	info.Directory = mode&0o170000 == 0o040000
	info.Executable = mode&0o111 != 0
	info.UID = uid
	info.GID = gid

	if info.Regular {
		headResult := i.runner.Run(ctx, i.tools.Head, "-c", "2", "--", path)
		if headResult.Err != nil {
			return HostPathInfo{}, errors.New("read host file shebang")
		}
		info.HasShebang = string(headResult.Stdout) == "#!"
	}
	if info.Directory {
		findResult := i.runner.Run(ctx, i.tools.Find, path, "-mindepth", "1", "-maxdepth", "1", "-print", "-quit")
		if findResult.Err != nil {
			return HostPathInfo{}, errors.New("inspect host directory contents")
		}
		info.Empty = len(findResult.Stdout) == 0
	}
	return info, nil
}

func (i *runnerHostInspector) LookupUser(ctx context.Context, nameOrID string) (HostUser, error) {
	if i == nil || i.runner == nil {
		return HostUser{}, errors.New("host command runner is required")
	}
	result := i.runner.Run(ctx, i.tools.Getent, "passwd", nameOrID)
	if result.Err != nil {
		return HostUser{}, errors.New("lookup host user")
	}
	line, err := singleOutputLine(result.Stdout)
	if err != nil {
		return HostUser{}, fmt.Errorf("lookup host user: %w", err)
	}
	fields := strings.Split(line, ":")
	if len(fields) != 7 {
		return HostUser{}, errors.New("host user record is malformed")
	}
	uid, err := strconv.Atoi(fields[2])
	if err != nil || uid < 0 {
		return HostUser{}, errors.New("host user UID is invalid")
	}
	gid, err := strconv.Atoi(fields[3])
	if err != nil || gid < 0 {
		return HostUser{}, errors.New("host user GID is invalid")
	}
	if fields[0] == "" || fields[5] == "" || fields[6] == "" {
		return HostUser{}, errors.New("host user record is incomplete")
	}
	return HostUser{Name: fields[0], UID: uid, GID: gid, Home: fields[5], Shell: fields[6]}, nil
}

func (i *runnerHostInspector) LookupGroup(ctx context.Context, nameOrID string) (HostGroup, error) {
	if i == nil || i.runner == nil {
		return HostGroup{}, errors.New("host command runner is required")
	}
	result := i.runner.Run(ctx, i.tools.Getent, "group", nameOrID)
	if result.Err != nil {
		return HostGroup{}, errors.New("lookup host group")
	}
	line, err := singleOutputLine(result.Stdout)
	if err != nil {
		return HostGroup{}, fmt.Errorf("lookup host group: %w", err)
	}
	fields := strings.Split(line, ":")
	if len(fields) != 4 {
		return HostGroup{}, errors.New("host group record is malformed")
	}
	gid, err := strconv.Atoi(fields[2])
	if err != nil || gid < 0 || fields[0] == "" {
		return HostGroup{}, errors.New("host group record is invalid")
	}
	return HostGroup{Name: fields[0], GID: gid}, nil
}

func singleOutputLine(output []byte) (string, error) {
	line := strings.TrimSuffix(string(output), "\n")
	if line == "" || strings.ContainsAny(line, "\r\n") {
		return "", errors.New("expected exactly one output line")
	}
	return line, nil
}
