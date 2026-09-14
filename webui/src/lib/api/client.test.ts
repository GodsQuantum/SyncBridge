import { describe, expect, it } from 'vitest';
import { buildApiPath } from './client';

describe('buildApiPath', () => {
  it('leaves local requests unchanged', () => {
    expect(buildApiPath('', '/api/v1/jobs')).toBe('/api/v1/jobs');
  });

  it('prefixes remote operational APIs', () => {
    expect(buildApiPath('/api/remote/7', '/api/v1/jobs/3/runs')).toBe('/api/remote/7/api/v1/jobs/3/runs');
  });

  it('never proxies local authentication and remote-management APIs', () => {
    expect(buildApiPath('/api/remote/7', '/api/auth/status')).toBe('/api/auth/status');
    expect(buildApiPath('/api/remote/7', '/api/remotes')).toBe('/api/remotes');
    expect(buildApiPath('/api/remote/7', '/api/remote/7/api/v1/jobs')).toBe('/api/remote/7/api/v1/jobs');
  });
});
