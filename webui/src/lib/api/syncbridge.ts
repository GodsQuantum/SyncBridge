import { ApiClient } from './client';
import type {
  BrowseResult, CapabilityReport, ImportInventory, Job, JobInput, RemoteInstance,
  RunRecord, RunSnapshot, Settings, SystemInventory, SystemItem, TrashEntry
} from '../domain/types';

const jsonHeaders = { 'Content-Type': 'application/json' };
export const api = (remotePrefix = '') => new ApiClient(remotePrefix);

export const jobs = (client: ApiClient) => client.request<Job[]>('/api/v1/jobs');
export const capabilities = (client: ApiClient) => client.request<CapabilityReport>('/api/v1/capabilities');
export const history = (client: ApiClient, id: number) => client.request<RunRecord[]>(`/api/v1/jobs/${id}/history`);
export const browse = (client: ApiClient, path: string) => client.request<BrowseResult>(`/api/browse?path=${encodeURIComponent(path)}`);

export const createJob = (client: ApiClient, input: JobInput) => client.request<Job>('/api/v1/jobs', {
  method: 'POST', headers: jsonHeaders, body: JSON.stringify(input)
});
export const updateJob = (client: ApiClient, current: Job, input: JobInput) => client.request<Job>(`/api/v1/jobs/${current.id}`, {
  method: 'PUT', headers: { ...jsonHeaders, 'If-Match': `"${current.revision}"` }, body: JSON.stringify(input)
});
export const deleteJob = (client: ApiClient, current: Job) => client.request<void>(`/api/v1/jobs/${current.id}`, {
  method: 'DELETE', headers: { 'If-Match': `"${current.revision}"` }
});
export const startRun = (client: ApiClient, current: Job, dryRun: boolean) => client.request<{ run: RunSnapshot }>(`/api/v1/jobs/${current.id}/runs`, {
  method: 'POST', headers: jsonHeaders, body: JSON.stringify({ revision: current.revision, dryRun })
});
export const stopRun = (client: ApiClient, runId: string) => client.request<{ run: RunSnapshot }>(`/api/v1/runs/${runId}/stop`, {
  method: 'POST', headers: jsonHeaders, body: '{}'
});

export const remotes = (client: ApiClient) => client.request<RemoteInstance[]>('/api/remotes');
export const addRemote = (client: ApiClient, input: { name: string; url: string; user: string; password: string }) => client.request<{ id: number; name: string }>('/api/remotes', {
  method: 'POST', headers: jsonHeaders, body: JSON.stringify(input)
});
export const removeRemote = (client: ApiClient, id: number) => client.request<{ deleted: number }>(`/api/remotes/${id}`, { method: 'DELETE' });

export const getSettings = (client: ApiClient) => client.request<Settings>('/api/settings');
export const saveSettings = (client: ApiClient, input: Settings & { test?: boolean }) => client.request<{ ok: boolean; testError: string }>('/api/settings', {
  method: 'POST', headers: jsonHeaders, body: JSON.stringify(input)
});
export const systemScan = (client: ApiClient) => client.request<SystemInventory>('/api/system/scan');
export const importScan = (client: ApiClient) => client.request<ImportInventory>('/api/import/scan');
export const systemToggle = (client: ApiClient, item: SystemItem) => client.request<{ state: string }>('/api/system/toggle', {
  method: 'POST', headers: jsonHeaders, body: JSON.stringify(item)
});
export const systemDelete = (client: ApiClient, items: SystemItem[]) => client.request<Array<{ name: string; state: string; error?: string }>>('/api/system/delete', {
  method: 'POST', headers: jsonHeaders, body: JSON.stringify({ confirm: 'delete', items })
});
export const systemTrash = (client: ApiClient) => client.request<TrashEntry[]>('/api/system/trash');
export const systemRestore = (client: ApiClient, items: TrashEntry[]) => client.request<Array<{ name: string; state: string; error?: string }>>('/api/system/restore', {
  method: 'POST', headers: jsonHeaders, body: JSON.stringify({ items })
});
