import { fireEvent, render, screen } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import JobCard from './JobCard.svelte';
import type { Job, RunSnapshot } from '../domain/types';

const job: Job = {
  schemaVersion: 2, id: 1, revision: 2, name: 'Backup media', enabled: true,
  action: { type: 'sync', sync: { engine: 'rsync', source: '/src', dest: '/dst', mode: 'mirror', compare: 'time' } },
  identity: { mode: 'fixed', user: 'user', uid: 1000, group: 'user', gid: 1000 },
  execution: { overlap: 'skip' }, trigger: 'manual', scheduler: { owner: 'syncbridge' }
};
const running: RunSnapshot = {
  id: 'run-1', jobId: 1, revision: 2, status: 'running', origin: 'manual', dryRun: false,
  requestedAt: '2026-09-14T00:00:00Z', startedAt: '2026-09-14T00:00:01Z', exitCode: -1
};

describe('JobCard', () => {
  it('uses stop as the primary action while a job is running', async () => {
    const onStop = vi.fn();
    const onRun = vi.fn();
    render(JobCard, { job, run: running, logs: [], lang: 'en', onStop, onRun });
    expect(screen.queryByRole('button', { name: 'Run now' })).toBeNull();
    await fireEvent.click(screen.getByRole('button', { name: 'Stop' }));
    expect(onStop).toHaveBeenCalledWith(running);
    expect(onRun).not.toHaveBeenCalled();
  });
});
