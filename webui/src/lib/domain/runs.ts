import type { RunEvent, RunSnapshot } from './types';

export interface RunState {
  byJob: Record<number, RunSnapshot>;
  byId: Record<string, RunSnapshot>;
  logs: Record<string, string[]>;
  lastEventId: number;
}
export function initialRunState(): RunState { return { byJob: {}, byId: {}, logs: {}, lastEventId: 0 }; }

function decodeLog(value?: string): string {
  if (!value) return '';
  try {
    const binary = atob(value);
    const bytes = Uint8Array.from(binary, (char) => char.charCodeAt(0));
    return new TextDecoder().decode(bytes);
  } catch { return value; }
}

export function reduceRunEvent(state: RunState, event: RunEvent, maxLines = 500): RunState {
  const byJob = { ...state.byJob, [event.run.jobId]: event.run };
  const byId = { ...state.byId, [event.run.id]: event.run };
  const logs = { ...state.logs };
  if (event.kind === 'log' && event.log) {
    const incoming = decodeLog(event.log).split(/\r?\n/).filter(Boolean);
    logs[event.run.id] = [...(logs[event.run.id] ?? []), ...incoming].slice(-maxLines);
  }
  return { byJob, byId, logs, lastEventId: Math.max(state.lastEventId, event.id) };
}

export interface RsyncProgress { scanning: boolean; percent: number | null; speed: string | null; eta: string | null; filePercent: number | null }
export function rsyncProgress(line: string): RsyncProgress {
  const scanning = /ir-chk=/.test(line);
  const percentMatch = line.match(/(\d+)%/), speedMatch = line.match(/([\d.]+\s?[kMGT]?B\/s)/), etaMatch = line.match(/(\d+:\d\d:\d\d)/);
  const toCheck = line.match(/to-chk=(\d+)\/(\d+)/);
  let percent = percentMatch ? Number(percentMatch[1]) : null;
  let eta = etaMatch?.[1] ?? null;
  if (scanning) { percent = null; eta = null; }
  let filePercent: number | null = null;
  if (toCheck) { const remaining = Number(toCheck[1]), total = Number(toCheck[2]); if (total > 0) filePercent = Math.round((total - remaining) / total * 100); }
  return { scanning, percent, speed: speedMatch?.[1].replace(/\s/, '') ?? null, eta, filePercent };
}
