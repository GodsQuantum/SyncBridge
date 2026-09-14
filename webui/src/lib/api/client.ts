const LOCAL_ONLY = ['/api/auth/', '/api/remotes', '/api/remote/'];

export function buildApiPath(remotePrefix: string, path: string): string {
  if (!remotePrefix || !path.startsWith('/api/')) return path;
  if (LOCAL_ONLY.some((prefix) => path.startsWith(prefix))) return path;
  return `${remotePrefix}${path}`;
}

export class ApiClient {
  constructor(public remotePrefix = '', private readonly fetcher: typeof fetch = fetch) {}
  path(path: string): string { return buildApiPath(this.remotePrefix, path); }
  async request<T>(path: string, init?: RequestInit): Promise<T> {
    const response = await this.fetcher(this.path(path), init);
    if (!response.ok) throw new Error((await response.text()) || `${response.status} ${response.statusText}`);
    if (response.status === 204) return undefined as T;
    return response.json() as Promise<T>;
  }
}
