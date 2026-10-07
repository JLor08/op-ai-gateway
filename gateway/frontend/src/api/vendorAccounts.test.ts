// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it, vi } from 'vitest';
import { createPortalApi } from '../api';

function jsonResponse(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

describe('vendorAccountsApi', () => {
  it('reads the boolean-only master flag from /enabled (not the {id} item route)', async () => {
    const fetcher = vi.fn().mockResolvedValue(jsonResponse({ module_enabled: true }));
    const api = createPortalApi(fetcher);

    const resp = await api.vendorAccountsEnabled();

    expect(fetcher).toHaveBeenCalledWith('/api/portal/vendor-accounts/enabled', {
      headers: {},
      credentials: 'include',
    });
    expect(resp).toEqual({ module_enabled: true });
  });

  it('lists the caller accounts from the {data:[...]} envelope', async () => {
    const fetcher = vi.fn().mockResolvedValue(jsonResponse({ data: [{ id: 'va_1' }] }));
    const api = createPortalApi(fetcher);

    const resp = await api.vendorAccounts();

    expect(fetcher).toHaveBeenCalledWith('/api/portal/vendor-accounts', {
      headers: {},
      credentials: 'include',
    });
    expect(resp.data).toEqual([{ id: 'va_1' }]);
  });

  it('POSTs the create body, including the write-only api_key, as JSON', async () => {
    const fetcher = vi.fn().mockResolvedValue(jsonResponse({ id: 'va_1' }, 201));
    const api = createPortalApi(fetcher);

    await api.createVendorAccount({
      vendor: 'openai',
      auth_type: 'api_key',
      name: 'Work',
      api_key: 'sk-test',
    });

    const [url, init] = fetcher.mock.calls[0];
    expect(url).toBe('/api/portal/vendor-accounts');
    expect(init.method).toBe('POST');
    expect(init.headers['X-OP-CSRF']).toBe('1');
    expect(JSON.parse(init.body)).toEqual({
      vendor: 'openai',
      auth_type: 'api_key',
      name: 'Work',
      api_key: 'sk-test',
    });
  });

  it('PATCHes /{id} with the id URL-encoded and omits api_key when it is not sent', async () => {
    const fetcher = vi.fn().mockResolvedValue(jsonResponse({ id: 'va/1' }));
    const api = createPortalApi(fetcher);

    await api.updateVendorAccount('va/1', { name: 'Renamed', status: 'disabled' });

    const [url, init] = fetcher.mock.calls[0];
    expect(url).toBe('/api/portal/vendor-accounts/va%2F1');
    expect(init.method).toBe('PATCH');
    expect(JSON.parse(init.body)).toEqual({ name: 'Renamed', status: 'disabled' });
  });

  it('DELETEs /{id}', async () => {
    const fetcher = vi.fn().mockResolvedValue(jsonResponse({ ok: true }));
    const api = createPortalApi(fetcher);

    const resp = await api.deleteVendorAccount('va_1');

    const [url, init] = fetcher.mock.calls[0];
    expect(url).toBe('/api/portal/vendor-accounts/va_1');
    expect(init.method).toBe('DELETE');
    expect(resp.ok).toBe(true);
  });
});
