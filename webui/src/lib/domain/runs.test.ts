import { describe, expect, it } from 'vitest';
import { initialRunState, reduceRunEvent, rsyncProgress } from './runs';
import type { RunEvent } from './types';

const running: RunEvent = {
  id: 1,
  kind: 'state',
  run: {
    id: 'run-a', jobId: 3, revision: 2, status: 'running', origin: 'manual', dryRun: false,
    requestedAt: '2026-09-14T00:00:00Z', startedAt: '2026-09-14T00:00:01Z', exitCode: -1
  }
};

describe('run event reduction', () => {
  it('tracks current runs and appends bounded log lines', () => {
    let state = reduceRunEvent(initialRunState(), running);
    state = reduceRunEvent(state, { ...running, id: 2, kind: 'log', log: btoa('  1.00G  42%  20.00MB/s  0:00:30 (xfr#2, to-chk=58/100)\n') });
    expect(state.byJob[3]?.status).toBe('running');
    expect(state.logs['run-a']).toHaveLength(1);
    expect(state.lastEventId).toBe(2);
  });

  it('extracts reliable rsync progress and ignores ir-chk scan percentages', () => {
    expect(rsyncProgress('1.00G 42% 20.00MB/s 0:00:30 (xfr#2, to-chk=58/100)')).toMatchObject({ percent: 42, speed: '20.00MB/s', eta: '0:00:30' });
    expect(rsyncProgress('20.00M 10% 5.00MB/s 0:00:01 (xfr#1, ir-chk=950/1000)')).toMatchObject({ scanning: true, percent: null, eta: null });
  });
});
