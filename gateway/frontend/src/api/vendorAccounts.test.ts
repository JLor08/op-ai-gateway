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

  it('POSTs the token import to .../connect/import with the id URL-encoded and omits unset optionals', async () => {
    const fetcher = vi.fn().mockResolvedValue(jsonResponse({ id: 'va/1' }));
    const api = createPortalApi(fetcher);

    await api.connectVendorAccountImport('va/1', { access_token: 'tok-a' });

    const [url, init] = fetcher.mock.calls[0];
    expect(url).toBe('/api/portal/vendor-accounts/va%2F1/connect/import');
    expect(init.method).toBe('POST');
    expect(init.headers['X-OP-CSRF']).toBe('1');
    expect(JSON.parse(init.body)).toEqual({ access_token: 'tok-a' });
  });

  it('sends the optional refresh token and expiry of an import when given', async () => {
    const fetcher = vi.fn().mockResolvedValue(jsonResponse({ id: 'va_1' }));
    const api = createPortalApi(fetcher);

    await api.connectVendorAccountImport('va_1', {
      access_token: 'tok-a',
      refresh_token: 'tok-r',
      expires_at: '2026-10-08T10:00:00.000Z',
    });

    expect(JSON.parse(fetcher.mock.calls[0][1].body)).toEqual({
      access_token: 'tok-a',
      refresh_token: 'tok-r',
      expires_at: '2026-10-08T10:00:00.000Z',
    });
  });

  it('POSTs .../connect/begin with no body and returns the authorize URL', async () => {
    const fetcher = vi
      .fn()
      .mockResolvedValue(jsonResponse({ authorize_url: 'https://vendor.example/authorize?x=1' }));
    const api = createPortalApi(fetcher);

    const resp = await api.beginVendorAccountConnect('va_1');

    const [url, init] = fetcher.mock.calls[0];
    expect(url).toBe('/api/portal/vendor-accounts/va_1/connect/begin');
    expect(init.method).toBe('POST');
    expect(init.headers['X-OP-CSRF']).toBe('1');
    expect(init.body).toBeUndefined();
    expect(resp.authorize_url).toBe('https://vendor.example/authorize?x=1');
  });

  it('POSTs the pasted code to .../connect/complete as {code}', async () => {
    const fetcher = vi
      .fn()
      .mockResolvedValue(jsonResponse({ id: 'va_1', subscription_connected: true }));
    const api = createPortalApi(fetcher);

    const resp = await api.completeVendorAccountConnect('va_1', 'abc#state');

    const [url, init] = fetcher.mock.calls[0];
    expect(url).toBe('/api/portal/vendor-accounts/va_1/connect/complete');
    expect(init.method).toBe('POST');
    expect(init.headers['X-OP-CSRF']).toBe('1');
    expect(JSON.parse(init.body)).toEqual({ code: 'abc#state' });
    expect(resp.subscription_connected).toBe(true);
  });

  it('GETs one account by its URL-encoded id (the post-connect refresh)', async () => {
    const fetcher = vi
      .fn()
      .mockResolvedValue(jsonResponse({ id: 'va/1', subscription_connected: true }));
    const api = createPortalApi(fetcher);

    const resp = await api.vendorAccount('va/1');

    expect(fetcher).toHaveBeenCalledWith('/api/portal/vendor-accounts/va%2F1', {
      headers: {},
      credentials: 'include',
    });
    expect(resp.subscription_connected).toBe(true);
  });

  it('POSTs the optional model_prefix of a create and a PATCH, and omits it when not sent', async () => {
    // A fresh Response per call: a body can be read once.
    const fetcher = vi.fn().mockImplementation(async () => jsonResponse({ id: 'va_1' }));
    const api = createPortalApi(fetcher);

    await api.createVendorAccount({
      vendor: 'openai',
      auth_type: 'subscription',
      name: 'Work',
      model_prefix: 'chatgpt/',
    });
    await api.updateVendorAccount('va_1', { model_prefix: '' });
    await api.updateVendorAccount('va_1', { name: 'Renamed' });

    expect(JSON.parse(fetcher.mock.calls[0][1].body)).toEqual({
      vendor: 'openai',
      auth_type: 'subscription',
      name: 'Work',
      model_prefix: 'chatgpt/',
    });
    // "" is the explicit clear and must survive serialization.
    expect(JSON.parse(fetcher.mock.calls[1][1].body)).toEqual({ model_prefix: '' });
    expect(JSON.parse(fetcher.mock.calls[2][1].body)).toEqual({ name: 'Renamed' });
  });

  it('POSTs base_url on a create when one is given, and leaves it out when it is not', async () => {
    const fetcher = vi.fn().mockImplementation(async () => jsonResponse({ id: 'va_1' }));
    const api = createPortalApi(fetcher);

    await api.createVendorAccount({
      vendor: 'openai_compatible',
      auth_type: 'api_key',
      name: 'Local gateway',
      api_key: 'sk-test',
      base_url: 'https://llm.example.com',
    });
    // A preset left on its default root sends no base_url at all.
    await api.createVendorAccount({
      vendor: 'xai',
      auth_type: 'api_key',
      name: 'Grok',
      api_key: 'sk-test',
    });

    expect(JSON.parse(fetcher.mock.calls[0][1].body)).toEqual({
      vendor: 'openai_compatible',
      auth_type: 'api_key',
      name: 'Local gateway',
      api_key: 'sk-test',
      base_url: 'https://llm.example.com',
    });
    const preset = JSON.parse(fetcher.mock.calls[1][1].body);
    expect(preset).toEqual({
      vendor: 'xai',
      auth_type: 'api_key',
      name: 'Grok',
      api_key: 'sk-test',
    });
    expect('base_url' in preset).toBe(false);
  });

  it('returns the base_url the backend stored on the account', async () => {
    const fetcher = vi
      .fn()
      .mockResolvedValue(jsonResponse({ id: 'va_1', vendor: 'xai', base_url: 'https://api.x.ai' }));
    const api = createPortalApi(fetcher);

    const account = await api.vendorAccount('va_1');

    expect(account.base_url).toBe('https://api.x.ai');
  });

  it('POSTs .../models/refresh with the id URL-encoded and no body, and returns the account and outcome', async () => {
    const answer = {
      account: {
        id: 'va/1',
        model_prefix: 'chatgpt/',
        models: [
          {
            gateway_model: 'chatgpt/gpt-6-luna',
            upstream_model: 'gpt-6-luna',
            api_flavor: 'openai_responses',
            display_name: 'GPT-6 Luna',
          },
        ],
      },
      refresh: { status: 'ok', discovered: 1, detail: 'discovered 1 models' },
    };
    const fetcher = vi.fn().mockResolvedValue(jsonResponse(answer));
    const api = createPortalApi(fetcher);

    const resp = await api.refreshModels('va/1');

    const [url, init] = fetcher.mock.calls[0];
    expect(url).toBe('/api/portal/vendor-accounts/va%2F1/models/refresh');
    expect(init.method).toBe('POST');
    expect(init.headers['X-OP-CSRF']).toBe('1');
    expect(init.body).toBeUndefined();
    expect(resp).toEqual(answer);
  });

  it('surfaces a refused models refresh as a PortalApiError with the backend code', async () => {
    const fetcher = vi.fn().mockResolvedValue(
      jsonResponse(
        {
          error: {
            code: 'vendor_account.credential_unreadable',
            message: 'the stored credential could not be read; reconnect the account',
          },
        },
        409,
      ),
    );
    const api = createPortalApi(fetcher);

    await expect(api.refreshModels('va_1')).rejects.toMatchObject({
      status: 409,
      code: 'vendor_account.credential_unreadable',
    });
  });

  it('POSTs .../usage/refresh with no body and no query for the on-view (lazy) call', async () => {
    const answer = {
      usage: null,
      refresh: { status: 'fresh', detail: 'the usage was pulled recently' },
    };
    const fetcher = vi.fn().mockResolvedValue(jsonResponse(answer));
    const api = createPortalApi(fetcher);

    const resp = await api.refreshUsage('va/1');

    const [url, init] = fetcher.mock.calls[0];
    expect(url).toBe('/api/portal/vendor-accounts/va%2F1/usage/refresh');
    expect(init.method).toBe('POST');
    expect(init.headers['X-OP-CSRF']).toBe('1');
    expect(init.body).toBeUndefined();
    expect(resp).toEqual(answer);
  });

  it('adds ?force=1 to the usage refresh only when asked to force it past the server TTL', async () => {
    // A fresh Response per call: a body can only be read once.
    const fetcher = vi.fn().mockImplementation(async () =>
      jsonResponse({
        usage: null,
        refresh: { status: 'ok', detail: 'usage refreshed' },
      }),
    );
    const api = createPortalApi(fetcher);

    await api.refreshUsage('va_1', { force: true });
    await api.refreshUsage('va_1', { force: false });
    await api.refreshUsage('va_1', {});

    expect(fetcher.mock.calls.map(([url]) => url)).toEqual([
      '/api/portal/vendor-accounts/va_1/usage/refresh?force=1',
      '/api/portal/vendor-accounts/va_1/usage/refresh',
      '/api/portal/vendor-accounts/va_1/usage/refresh',
    ]);
  });

  it('surfaces a refused usage refresh as a PortalApiError with the backend code', async () => {
    const fetcher = vi
      .fn()
      .mockResolvedValue(
        jsonResponse(
          { error: { code: 'vendor_account.usage_refresh_failed', message: 'raw server text' } },
          500,
        ),
      );
    const api = createPortalApi(fetcher);

    await expect(api.refreshUsage('va_1')).rejects.toMatchObject({
      status: 500,
      code: 'vendor_account.usage_refresh_failed',
    });
  });

  it('POSTs .../connect/device/begin with no body and returns the user code and verification URL', async () => {
    const fetcher = vi.fn().mockResolvedValue(
      jsonResponse({
        user_code: 'ABCD-EFGH',
        verification_url: 'https://auth.example/codex/device',
      }),
    );
    const api = createPortalApi(fetcher);

    const resp = await api.beginVendorAccountDeviceConnect('va_1');

    const [url, init] = fetcher.mock.calls[0];
    expect(url).toBe('/api/portal/vendor-accounts/va_1/connect/device/begin');
    expect(init.method).toBe('POST');
    expect(init.headers['X-OP-CSRF']).toBe('1');
    expect(init.body).toBeUndefined();
    expect(resp).toEqual({
      user_code: 'ABCD-EFGH',
      verification_url: 'https://auth.example/codex/device',
    });
  });

  it('POSTs .../connect/device/poll with no body and returns {connected}', async () => {
    const fetcher = vi.fn().mockResolvedValue(jsonResponse({ connected: false }));
    const api = createPortalApi(fetcher);

    const resp = await api.pollVendorAccountDeviceConnect('va/1');

    const [url, init] = fetcher.mock.calls[0];
    expect(url).toBe('/api/portal/vendor-accounts/va%2F1/connect/device/poll');
    expect(init.method).toBe('POST');
    expect(init.headers['X-OP-CSRF']).toBe('1');
    expect(init.body).toBeUndefined();
    expect(resp).toEqual({ connected: false });
  });

  it('surfaces a transient poll failure as a PortalApiError carrying the upstream code', async () => {
    const fetcher = vi
      .fn()
      .mockResolvedValue(
        jsonResponse(
          { error: { code: 'vendor_account.connect_upstream_failed', message: 'try again later' } },
          502,
        ),
      );
    const api = createPortalApi(fetcher);

    await expect(api.pollVendorAccountDeviceConnect('va_1')).rejects.toMatchObject({
      name: 'PortalApiError',
      status: 502,
      code: 'vendor_account.connect_upstream_failed',
    });
  });

  it('POSTs .../check with no body, the id URL-encoded, and returns the verdict as served', async () => {
    const verdict = {
      status: 'invalid',
      detail: 'the vendor rejected the credential (401)',
      checked_at: '2026-10-08T10:00:00Z',
    };
    const fetcher = vi.fn().mockResolvedValue(jsonResponse(verdict));
    const api = createPortalApi(fetcher);

    const resp = await api.testConnection('va/1');

    const [url, init] = fetcher.mock.calls[0];
    expect(url).toBe('/api/portal/vendor-accounts/va%2F1/check');
    expect(init.method).toBe('POST');
    expect(init.headers['X-OP-CSRF']).toBe('1');
    expect(init.body).toBeUndefined();
    expect(resp).toEqual(verdict);
  });

  it('surfaces a failed check as a PortalApiError carrying vendor_account.check_failed', async () => {
    const fetcher = vi
      .fn()
      .mockResolvedValue(
        jsonResponse(
          { error: { code: 'vendor_account.check_failed', message: 'check failed' } },
          500,
        ),
      );
    const api = createPortalApi(fetcher);

    await expect(api.testConnection('va_1')).rejects.toMatchObject({
      name: 'PortalApiError',
      status: 500,
      code: 'vendor_account.check_failed',
    });
  });

  // Sharing (#183, spec 2026-10-15): the share-level lifecycle and the
  // management-group editor's two calls.
  it('PUTs .../share-level with the target level and the management groups of a raise', async () => {
    const fetcher = vi.fn().mockResolvedValue(jsonResponse({ id: 'va/1', share_level: 'shared' }));
    const api = createPortalApi(fetcher);

    const resp = await api.setVendorAccountShareLevel('va/1', 'shared', ['ag_1', 'ag_2']);

    const [url, init] = fetcher.mock.calls[0];
    expect(url).toBe('/api/portal/vendor-accounts/va%2F1/share-level');
    expect(init.method).toBe('PUT');
    expect(init.headers['X-OP-CSRF']).toBe('1');
    expect(JSON.parse(init.body)).toEqual({ level: 'shared', admin_group_ids: ['ag_1', 'ag_2'] });
    expect(resp).toMatchObject({ share_level: 'shared' });
  });

  it('PUTs .../share-level with the level alone for a lower (the groups are ignored there)', async () => {
    const fetcher = vi.fn().mockResolvedValue(jsonResponse({ id: 'va_1', share_level: 'none' }));
    const api = createPortalApi(fetcher);

    await api.setVendorAccountShareLevel('va_1', 'none');

    const [, init] = fetcher.mock.calls[0];
    expect(JSON.parse(init.body)).toEqual({ level: 'none' });
  });

  it('PUTs .../admin-groups with the full replacement set and returns the account', async () => {
    const fetcher = vi.fn().mockResolvedValue(jsonResponse({ id: 'va/1' }));
    const api = createPortalApi(fetcher);

    await api.setVendorAccountAdminGroups('va/1', ['ag_b']);

    const [url, init] = fetcher.mock.calls[0];
    expect(url).toBe('/api/portal/vendor-accounts/va%2F1/admin-groups');
    expect(init.method).toBe('PUT');
    expect(init.headers['X-OP-CSRF']).toBe('1');
    expect(JSON.parse(init.body)).toEqual({ admin_group_ids: ['ag_b'] });
  });

  it('GETs the management-group candidates and unwraps the {data:[...]} envelope', async () => {
    const candidate = {
      id: 'ag_a',
      name: 'Group A',
      parent_group_id: 'sg_1',
      parent_group_name: 'Ops',
    };
    const fetcher = vi.fn().mockResolvedValue(jsonResponse({ data: [candidate] }));
    const api = createPortalApi(fetcher);

    const resp = await api.vendorAccountAdminGroupCandidates();

    expect(fetcher).toHaveBeenCalledWith('/api/portal/vendor-account-admin-group-candidates', {
      headers: {},
      credentials: 'include',
    });
    expect(resp).toEqual([candidate]);
  });

  // Owner self-service resource-group link (#183, spec 2026-10-15 section 9): the
  // vendor-account analogue of the server-owner trio in api/servers.ts.
  it('GETs the resource groups an account can join from the {data:[...]} envelope, id URL-encoded', async () => {
    const groups = [
      { id: 'rg-1', name: 'Alpha', member: true },
      { id: 'rg-2', name: 'Beta', member: false },
    ];
    const fetcher = vi.fn().mockResolvedValue(jsonResponse({ data: groups }));
    const api = createPortalApi(fetcher);

    const resp = await api.vendorAccountResourceGroups('va/1');

    expect(fetcher).toHaveBeenCalledWith('/api/portal/vendor-accounts/va%2F1/resource-groups', {
      headers: {},
      credentials: 'include',
    });
    expect(resp).toEqual(groups);
  });

  it('PUTs .../resource-groups/{rgId} to join, both ids URL-encoded, with the CSRF header', async () => {
    const fetcher = vi.fn().mockResolvedValue(jsonResponse({ ok: true }));
    const api = createPortalApi(fetcher);

    const resp = await api.joinVendorAccountResourceGroup('va/1', 'rg/2');

    const [url, init] = fetcher.mock.calls[0];
    expect(url).toBe('/api/portal/vendor-accounts/va%2F1/resource-groups/rg%2F2');
    expect(init.method).toBe('PUT');
    expect(init.headers['X-OP-CSRF']).toBe('1');
    expect(init.body).toBeUndefined();
    expect(resp.ok).toBe(true);
  });

  it('DELETEs .../resource-groups/{rgId} to leave, both ids URL-encoded, with the CSRF header', async () => {
    const fetcher = vi.fn().mockResolvedValue(jsonResponse({ ok: true }));
    const api = createPortalApi(fetcher);

    const resp = await api.leaveVendorAccountResourceGroup('va/1', 'rg/2');

    const [url, init] = fetcher.mock.calls[0];
    expect(url).toBe('/api/portal/vendor-accounts/va%2F1/resource-groups/rg%2F2');
    expect(init.method).toBe('DELETE');
    expect(init.headers['X-OP-CSRF']).toBe('1');
    expect(resp.ok).toBe(true);
  });
});
