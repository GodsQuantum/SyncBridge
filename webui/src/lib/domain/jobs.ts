import type { Action, Identity, Job, JobInput, SchedulerOwner, Trigger } from './types';

export interface JobForm {
  name: string; enabled: boolean; action: Action; identity: Identity;
  timeoutSeconds: number; stopGraceSeconds: number; overlap: 'skip' | 'queue-latest';
  environment: string[]; umask: number; trigger: Trigger; schedulerOwner: SchedulerOwner;
  cron: string; source: string; watchGlob: string; watchMode: string; debounce: number; pollSec: number;
}

export function jobToForm(job: Job): JobForm {
  return {
    name: job.name, enabled: job.enabled, action: structuredClone(job.action), identity: structuredClone(job.identity),
    timeoutSeconds: job.execution.timeoutSeconds ?? 0, stopGraceSeconds: job.execution.stopGraceSeconds ?? 5,
    overlap: job.execution.overlap ?? 'skip', environment: [...(job.execution.environment ?? [])], umask: job.execution.umask ?? 0o027,
    trigger: job.trigger, schedulerOwner: job.scheduler.owner, cron: job.cron ?? '', source: job.source || job.action.sync?.source || '',
    watchGlob: job.watchGlob ?? '', watchMode: job.watchMode ?? 'hybrid', debounce: job.debounce ?? 3, pollSec: job.pollSec ?? 300
  };
}

export function formToJobInput(form: JobForm): JobInput {
  return {
    name: form.name.trim(), enabled: form.enabled, action: structuredClone(form.action), identity: structuredClone(form.identity),
    execution: { timeoutSeconds: form.timeoutSeconds, stopGraceSeconds: form.stopGraceSeconds, overlap: form.overlap,
      environment: [...form.environment], umask: form.umask },
    trigger: form.trigger, scheduler: { owner: form.schedulerOwner }, cron: form.cron.trim(),
    source: form.action.type === 'sync' ? (form.action.sync?.source ?? '').trim() : form.source.trim(),
    watchGlob: form.watchGlob.trim(), watchMode: form.watchMode, debounce: form.debounce, pollSec: form.pollSec
  };
}

export function cloneJobInput(job: Job): JobInput {
  const form = jobToForm(job);
  form.name = `${form.name} (copy)`;
  form.enabled = false;
  return formToJobInput(form);
}

export function setJobActionType(form: JobForm, type: Action['type']): JobForm {
  const next = structuredClone(form);
  if (type === 'sync') {
    next.action = { type, sync: { engine: 'rsync', source: '', dest: '', mode: 'add', compare: 'time', backup: false, backupKeep: 0, maxDel: 0, skipNew: false, sysBackup: false, exclude: '' } };
    next.identity.mode = 'fixed';
  } else if (type === 'command') {
    next.action = { type, command: '' };
    next.identity.mode = 'fixed';
  } else {
    next.action = { type, scriptPath: '', scriptArgs: [] };
    next.identity.mode = 'script-owner';
  }
  return next;
}

export function newJobForm(): JobForm {
  return {
    name: '', enabled: true,
    action: { type: 'sync', sync: { engine: 'rsync', source: '', dest: '', mode: 'add', compare: 'time', backup: false, backupKeep: 0, maxDel: 0, skipNew: false, sysBackup: false, exclude: '' } },
    identity: { mode: 'fixed', user: '', uid: 1000, group: '', gid: 1000 },
    timeoutSeconds: 0, stopGraceSeconds: 5, overlap: 'skip', environment: [], umask: 27,
    trigger: 'manual', schedulerOwner: 'syncbridge', cron: '', source: '', watchGlob: '', watchMode: 'hybrid', debounce: 3, pollSec: 300
  };
}
