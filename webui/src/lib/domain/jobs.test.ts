import { describe, expect, it } from 'vitest';
import { cloneJobInput, formToJobInput, jobToForm, newJobForm, setJobActionType } from './jobs';
import type { Job } from './types';

const job: Job = {
  schemaVersion: 2,
  id: 4,
  revision: 9,
  name: 'Media mirror',
  enabled: true,
  action: {
    type: 'sync',
    sync: {
      engine: 'rsync', source: '/srv/source', dest: '/srv/dest', mode: 'mirror', compare: 'checksum',
      bwlimit: '50M', backup: true, backupKeep: 5, maxDel: 100, skipNew: true, sysBackup: false, exclude: '*.tmp'
    }
  },
  identity: { mode: 'fixed', user: 'operator', uid: 1000, group: 'operator', gid: 1000 },
  execution: { timeoutSeconds: 3600, stopGraceSeconds: 5, overlap: 'skip', environment: [], umask: 27 },
  trigger: 'cron',
  cron: '0 3 * * *',
  scheduler: { owner: 'syncbridge' },
  watchGlob: '', watchMode: 'hybrid', debounce: 3, pollSec: 300
};

describe('job form conversion', () => {
  it('round-trips the editable v2 job fields', () => {
    const form = jobToForm(job);
    const input = formToJobInput(form);
    expect(input).toMatchObject({
      name: 'Media mirror', enabled: true, trigger: 'cron', cron: '0 3 * * *',
      scheduler: { owner: 'syncbridge' }, identity: job.identity,
      action: { type: 'sync', sync: job.action.sync },
      execution: { timeoutSeconds: 3600, stopGraceSeconds: 5, overlap: 'skip', umask: 27 }
    });
  });

  it('clones a job as disabled without carrying server identity fields', () => {
    const clone = cloneJobInput(job);
    expect(clone.name).toBe('Media mirror (copy)');
    expect(clone.enabled).toBe(false);
    expect(clone).not.toHaveProperty('id');
    expect(clone).not.toHaveProperty('revision');
    expect(clone).not.toHaveProperty('schemaVersion');
  });

  it('restores fixed identity when leaving script-owner actions', () => {
    let form = newJobForm();
    form = setJobActionType(form, 'script');
    expect(form.identity.mode).toBe('script-owner');
    form = setJobActionType(form, 'sync');
    expect(form.identity.mode).toBe('fixed');
    form = setJobActionType(setJobActionType(form, 'script'), 'command');
    expect(form.identity.mode).toBe('fixed');
  });

  it('mirrors the sync source into the top-level watch source required by API v1', () => {
    const form = jobToForm({ ...job, trigger: 'watch', source: '' });
    const input = formToJobInput(form);
    expect(input.source).toBe('/srv/source');
  });

});
