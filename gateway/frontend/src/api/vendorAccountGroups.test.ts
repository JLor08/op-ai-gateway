// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it, vi } from 'vitest';
import { createPortalApi } from '../api';
import type { VendorAccountGroup } from './vendorAccountGroups';

function jsonResponse(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

const group: VendorAccountGroup = {
  id: 'vag_1',
  vendor: 'anthropic',
  name: 'Team',
  model_prefix: '',
  status: 'active',
  thresholds: { five_hour_pct: 99, weekly_pct: 99, credits_pct: 99 },
  active_member_id: 'va_1',
  members: [
    {
      account_id: 'va_1',
      account_name: 'Max',
      auth_type: 'subscription',
      owner_name: '',
      position: 1,
      state: 'active',
      reason: '',
      cooldowns: [],
    },
  ],
  created_at: '2026-11-24T10:00:00Z',
  updated_at: '2026-11-24T10:00:00Z',
};

describe('vendorAccountGroupsApi', () => {
  it('lists the groups from the {groups:[...]} envelope', async () => {
    const fetcher = vi.fn().mockResolvedValue(jsonResponse({ groups: [group] }));
    const api = createPortalApi(fetcher);

    const resp = await api.vendorAccountGroups();

    expect(fetcher).toHaveBeenCalledWith('/api/portal/vendor-account-groups', {
      headers: {},
      credentials: 'include',
    });
    expect(resp.groups[0].members[0].state).toBe('active');
  });

  it('reads one group by encoded id', async () => {
    const fetcher = vi.fn().mockResolvedValue(jsonResponse(group));
    const api = createPortalApi(fetcher);

    const resp = await api.vendorAccountGroup('vag/1');

    expect(fetcher.mock.calls[0][0]).toBe('/api/portal/vendor-account-groups/vag%2F1');
    expect(resp.id).toBe('vag_1');
  });

  it('POSTs the create body as JSON', async () => {
    const fetcher = vi.fn().mockResolvedValue(jsonResponse(group, 201));
    const api = createPortalApi(fetcher);

    await api.createVendorAccountGroup({
      name: 'Team',
      vendor: 'anthropic',
      threshold_five_hour_pct: 99,
      account_ids: ['va_1'],
    });

    const [url, init] = fetcher.mock.calls[0];
    expect(url).toBe('/api/portal/vendor-account-groups');
    expect(init.method).toBe('POST');
    expect(init.headers['X-OP-CSRF']).toBe('1');
    expect(JSON.parse(init.body)).toEqual({
      name: 'Team',
      vendor: 'anthropic',
      threshold_five_hour_pct: 99,
      account_ids: ['va_1'],
    });
  });

  it('PATCHes the update body', async () => {
    const fetcher = vi.fn().mockResolvedValue(jsonResponse(group));
    const api = createPortalApi(fetcher);

    await api.updateVendorAccountGroup('vag_1', { name: 'Renamed', status: 'disabled' });

    const [url, init] = fetcher.mock.calls[0];
    expect(url).toBe('/api/portal/vendor-account-groups/vag_1');
    expect(init.method).toBe('PATCH');
    expect(JSON.parse(init.body)).toEqual({ name: 'Renamed', status: 'disabled' });
  });

  it('DELETEs a group', async () => {
    const fetcher = vi.fn().mockResolvedValue(jsonResponse({ ok: true }));
    const api = createPortalApi(fetcher);

    await api.deleteVendorAccountGroup('vag_1');

    const [url, init] = fetcher.mock.calls[0];
    expect(url).toBe('/api/portal/vendor-account-groups/vag_1');
    expect(init.method).toBe('DELETE');
  });

  it('PUTs the ordered member ids as {account_ids}', async () => {
    const fetcher = vi.fn().mockResolvedValue(jsonResponse(group));
    const api = createPortalApi(fetcher);

    await api.setVendorAccountGroupMembers('vag_1', ['va_2', 'va_1']);

    const [url, init] = fetcher.mock.calls[0];
    expect(url).toBe('/api/portal/vendor-account-groups/vag_1/members');
    expect(init.method).toBe('PUT');
    expect(JSON.parse(init.body)).toEqual({ account_ids: ['va_2', 'va_1'] });
  });
});
