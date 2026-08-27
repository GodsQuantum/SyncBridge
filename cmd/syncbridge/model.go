package main

import "encoding/json"

// FileOwner is the owner applied to persisted SyncBridge files.
type FileOwner struct {
	UID int
	GID int
}

// CapabilityCode identifies a host execution prerequisite.
type CapabilityCode string

const (
	CapHostNamespace      CapabilityCode = "host_namespace"
	CapHostPID1           CapabilityCode = "host_pid_1"
	CapHostHostname       CapabilityCode = "host_hostname"
	CapHostRoot           CapabilityCode = "host_root"
	CapCapabilityBounding CapabilityCode = "capability_bounding"
	CapIdentityDrop       CapabilityCode = "identity_drop"
	CapAccountLookup      CapabilityCode = "account_lookup"
	CapShell              CapabilityCode = "shell"
	CapSystemd            CapabilityCode = "systemd"
	CapJournald           CapabilityCode = "journald"
	CapCron               CapabilityCode = "cron"
	CapRsync              CapabilityCode = "rsync"
	CapRclone             CapabilityCode = "rclone"
	CapSignalControl      CapabilityCode = "signal_control"
	CapHostToolchain      CapabilityCode = "host_toolchain"
)

type CapabilityStatus string

const (
	CapabilityAvailable   CapabilityStatus = "available"
	CapabilityDegraded    CapabilityStatus = "degraded"
	CapabilityUnavailable CapabilityStatus = "unavailable"
)

type CapabilityResult struct {
	Code       CapabilityCode    `json:"code"`
	Status     CapabilityStatus  `json:"status"`
	MessageKey string            `json:"messageKey"`
	Details    map[string]string `json:"details,omitempty"`
}

type CapabilityReport struct {
	Results []CapabilityResult `json:"results"`
}

type ActionType string

const (
	ActionScript  ActionType = "script"
	ActionCommand ActionType = "command"
	ActionSync    ActionType = "sync"
)

// Action describes the work a job performs.
type Action struct {
	Type       ActionType `json:"type"`
	ScriptPath string     `json:"scriptPath,omitempty"`
	ScriptArgs []string   `json:"scriptArgs,omitempty"`
	Command    string     `json:"command,omitempty"`
	Sync       SyncAction `json:"sync,omitempty"`
}

// SyncAction contains the v2 sync-specific action fields.
type SyncAction struct {
	Engine     string `json:"engine,omitempty"`
	Source     string `json:"source,omitempty"`
	Dest       string `json:"dest,omitempty"`
	Mode       string `json:"mode,omitempty"`
	Compare    string `json:"compare,omitempty"`
	Bwlimit    string `json:"bwlimit,omitempty"`
	Backup     bool   `json:"backup,omitempty"`
	BackupKeep int    `json:"backupKeep,omitempty"`
	MaxDel     int    `json:"maxDel,omitempty"`
	SkipNew    bool   `json:"skipNew,omitempty"`
	SysBackup  bool   `json:"sysBackup,omitempty"`
	Exclude    string `json:"exclude,omitempty"`
}

type IdentityMode string

const (
	IdentityScriptOwner IdentityMode = "script-owner"
	IdentityFixed       IdentityMode = "fixed"
)

// Identity describes the effective host identity for a job.
type Identity struct {
	Mode  IdentityMode `json:"mode,omitempty"`
	User  string       `json:"user,omitempty"`
	UID   int          `json:"uid,omitempty"`
	Group string       `json:"group,omitempty"`
	GID   int          `json:"gid,omitempty"`
}

// ExecutionPolicy contains execution controls independent of action and trigger.
type ExecutionPolicy struct {
	WorkingDirectory string   `json:"workingDirectory,omitempty"`
	Environment      []string `json:"environment,omitempty"`
	Umask            uint32   `json:"umask,omitempty"`
	TimeoutSeconds   int      `json:"timeoutSeconds,omitempty"`
	StopGraceSeconds int      `json:"stopGraceSeconds,omitempty"`
	Overlap          string   `json:"overlap,omitempty"`
}

// Trigger identifies how a job is triggered. It remains string-backed while the
// legacy scheduler is still wired directly to Job.Trigger.
type Trigger string

const (
	TriggerManual Trigger = "manual"
	TriggerCron   Trigger = "cron"
	TriggerWatch  Trigger = "watch"
)

type SchedulerOwner string

const (
	SchedulerSyncBridge SchedulerOwner = "syncbridge"
	SchedulerSystem     SchedulerOwner = "system"
)

// SchedulerPolicy identifies the owner of the scheduling artifact.
type SchedulerPolicy struct {
	Owner SchedulerOwner `json:"owner"`
}

// Job is the schema-v2 persisted job. The legacy fields below are intentionally
// retained until the compatibility HTTP handlers and scheduler are replaced.
type Job struct {
	SchemaVersion int             `json:"schemaVersion"`
	ID            int             `json:"id"`
	Revision      uint64          `json:"revision"`
	Name          string          `json:"name"`
	Enabled       bool            `json:"enabled"`
	NeedsReview   bool            `json:"needsReview,omitempty"`
	Action        Action          `json:"action"`
	Identity      Identity        `json:"identity"`
	Execution     ExecutionPolicy `json:"execution"`
	Trigger       Trigger         `json:"trigger"`
	Scheduler     SchedulerPolicy `json:"scheduler"`

	// Deprecated: compatibility fields used by the legacy handlers and scheduler.
	Kind       string `json:"kind,omitempty"`
	Command    string `json:"command,omitempty"`
	Backend    string `json:"backend,omitempty"`
	Source     string `json:"source,omitempty"`
	Dest       string `json:"dest,omitempty"`
	Engine     string `json:"engine,omitempty"`
	Mode       string `json:"mode,omitempty"`
	Cron       string `json:"cron,omitempty"`
	WatchGlob  string `json:"watchGlob,omitempty"`
	WatchMode  string `json:"watchMode,omitempty"`
	Debounce   int    `json:"debounce,omitempty"`
	PollSec    int    `json:"pollSec,omitempty"`
	Timeout    int    `json:"timeout,omitempty"`
	Compare    string `json:"compare,omitempty"`
	Bwlimit    string `json:"bwlimit,omitempty"`
	Backup     bool   `json:"backup,omitempty"`
	BackupKeep int    `json:"backupKeep,omitempty"`
	MaxDel     int    `json:"maxDel,omitempty"`
	SkipNew    bool   `json:"skipNew,omitempty"`
	SysBackup  bool   `json:"sysBackup,omitempty"`
	Exclude    string `json:"exclude,omitempty"`
	Disabled   bool   `json:"disabled,omitempty"`
	LastRun    string `json:"lastRun,omitempty"`
	LastStat   string `json:"lastStat,omitempty"`
}

func cloneJob(job Job) Job {
	job.Action.ScriptArgs = append([]string(nil), job.Action.ScriptArgs...)
	job.Execution.Environment = append([]string(nil), job.Execution.Environment...)
	return job
}

// MarshalJSON preserves the v1 wire shape for the still-active compatibility
// handlers. Repository-owned v2 jobs always carry SchemaVersion 2.
func (job Job) MarshalJSON() ([]byte, error) {
	if job.SchemaVersion != 0 {
		type v2Job Job
		return json.Marshal(v2Job(job))
	}
	type legacyJob struct {
		ID          int      `json:"id"`
		Revision    uint64   `json:"revision"`
		NeedsReview bool     `json:"needsReview,omitempty"`
		Identity    Identity `json:"identity"`
		Name        string   `json:"name"`
		Kind        string   `json:"kind"`
		Command     string   `json:"command"`
		Backend     string   `json:"backend"`
		Source      string   `json:"source"`
		Dest        string   `json:"dest"`
		Engine      string   `json:"engine"`
		Mode        string   `json:"mode"`
		Trigger     Trigger  `json:"trigger"`
		Cron        string   `json:"cron"`
		WatchGlob   string   `json:"watchGlob"`
		WatchMode   string   `json:"watchMode"`
		Debounce    int      `json:"debounce"`
		PollSec     int      `json:"pollSec"`
		Timeout     int      `json:"timeout"`
		Compare     string   `json:"compare"`
		Bwlimit     string   `json:"bwlimit"`
		Backup      bool     `json:"backup"`
		BackupKeep  int      `json:"backupKeep"`
		MaxDel      int      `json:"maxDel"`
		SkipNew     bool     `json:"skipNew"`
		SysBackup   bool     `json:"sysBackup"`
		Exclude     string   `json:"exclude"`
		Disabled    bool     `json:"disabled"`
		LastRun     string   `json:"lastRun"`
		LastStat    string   `json:"lastStat"`
	}
	return json.Marshal(legacyJob{
		ID:          job.ID,
		Revision:    job.Revision,
		NeedsReview: job.NeedsReview,
		Identity:    job.Identity,
		Name:        job.Name,
		Kind:        job.Kind,
		Command:     job.Command,
		Backend:     job.Backend,
		Source:      job.Source,
		Dest:        job.Dest,
		Engine:      job.Engine,
		Mode:        job.Mode,
		Trigger:     job.Trigger,
		Cron:        job.Cron,
		WatchGlob:   job.WatchGlob,
		WatchMode:   job.WatchMode,
		Debounce:    job.Debounce,
		PollSec:     job.PollSec,
		Timeout:     job.Timeout,
		Compare:     job.Compare,
		Bwlimit:     job.Bwlimit,
		Backup:      job.Backup,
		BackupKeep:  job.BackupKeep,
		MaxDel:      job.MaxDel,
		SkipNew:     job.SkipNew,
		SysBackup:   job.SysBackup,
		Exclude:     job.Exclude,
		Disabled:    job.Disabled,
		LastRun:     job.LastRun,
		LastStat:    job.LastStat,
	})
}
