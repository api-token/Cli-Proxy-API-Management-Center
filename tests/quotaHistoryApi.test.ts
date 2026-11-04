import { afterEach, describe, expect, test } from 'bun:test';
import { apiClient } from '@/services/api/client';

describe('quota history plugin route', () => {
  afterEach(() => apiClient.setConfig({ apiBase: '', managementKey: '' }));

  test('resolves plugin routes against the server root, outside the v8 prefix', () => {
    apiClient.setConfig({ apiBase: 'http://127.0.0.1:8317/', managementKey: 'key' });
    expect(apiClient.pluginRouteUrl('/v0/management/quota-history')).toBe(
      'http://127.0.0.1:8317/v0/management/quota-history'
    );
  });

  test('leaves the path relative when no connection is configured', () => {
    apiClient.setConfig({ apiBase: '', managementKey: '' });
    expect(apiClient.pluginRouteUrl('/v0/management/quota-history')).toBe(
      '/v0/management/quota-history'
    );
  });
});
