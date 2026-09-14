export type ActionType = 'script' | 'command' | 'sync';
export type Trigger = 'manual' | 'cron' | 'watch';
export type SchedulerOwner = 'syncbridge' | 'system';
export type RunStatus = 'queued' | 'running' | 'succeeded' | 'failed' | 'killed' | 'timed_out' | 'start_failed' | 'skipped_overlap';

export interface SyncAction {
  engine?: string; source?: string; dest?: string; mode?: string; compare?: string;
  bwlimit?: string; backup?: boolean; backupKeep?: number; maxDel?: number;
  skipNew?: boolean; sysBackup?: boolean; exclude?: string;
}
export interface Action { type: ActionType; scriptPath?: string; scriptArgs?: string[]; command?: string; sync?: SyncAction }
export interface Identity { mode: 'fixed' | 'script-owner'; user?: string; uid?: number; group?: string; gid?: number }
export interface ExecutionPolicy {
  workingDirectory?: string; environment?: string[]; umask?: number;
  timeoutSeconds?: number; stopGraceSeconds?: number; overlap?: 'skip' | 'queue-latest';
}
export interface Job {
  schemaVersion: number; id: number; revision: number; name: string; enabled: boolean; needsReview?: boolean;
  action: Action; identity: Identity; execution: ExecutionPolicy; trigger: Trigger;
  scheduler: { owner: SchedulerOwner }; cron?: string; source?: string; watchGlob?: string;
  watchMode?: string; debounce?: number; pollSec?: number;
}
export type JobInput = Omit<Job, 'schemaVersion' | 'id' | 'revision'>;
export interface RunSnapshot {
  id: string; jobId: number; revision: number; status: RunStatus; origin: string; dryRun: boolean;
  requestedAt: string; startedAt?: string; finishedAt?: string; effectiveUid?: number; effectiveGid?: number;
  exitCode: number; killEscalated?: boolean; droppedLogLines?: number; logError?: string;
  controlError?: string; historyError?: string; error?: string;
}
export interface RunEvent { id: number; kind: 'state' | 'log'; run: RunSnapshot; log?: string }

export interface RunRecord { ts: string; status: string; dur: number; note: string }
export interface CapabilityResult { code: string; status: 'available' | 'degraded' | 'unavailable'; messageKey: string; details?: Record<string, string> }
export interface CapabilityReport { results: CapabilityResult[] }
export interface BrowseResult { path: string; parent: string; dirs: string[] }
export interface RemoteInstance { id: number; name: string; url: string; user: string }
export interface Settings { notifyUrl: string; notifyAll: boolean }
export interface SystemItem {
  type: string; name: string; schedule: string; target: string; file: string;
  managed: boolean; class: string; disabled: boolean;
}
export interface SystemInventory { items: SystemItem[]; cronPaths: string[]; systemdPaths: string[] }
export interface ImportFound {
  engine: string; verb: string; source: string; dest: string; cron: string; file: string;
  line: string; local: boolean; warning: string;
}
export interface ImportInventory { found: ImportFound[]; paths: string[] }
export interface TrashEntry {
  ts: string; type: string; name: string; file: string; schedule: string; target: string;
  line: string; unit: string; restorable: boolean;
}
