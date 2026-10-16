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

// Co-manager per-permission flags on the wire (#183, shared vendor accounts):
// the snake_case keys must match the gateway's groupManagerRequest JSON tags
// exactly, including the sixth flag can_manage_vendor_accounts.
describe('groupsApi manager permission bodies', () => {
  it('promoteManager POSTs all six flags, defaulting every one to true', async () => {
    const fetcher = vi.fn().mockResolvedValue(jsonResponse({ ok: true }));
    const api = createPortalApi(fetcher);

    await api.promoteManager('grp/1', 'usr_1');

    const [url, init] = fetcher.mock.calls[0];
    expect(url).toBe('/api/portal/groups/grp%2F1/managers');
    expect(init.method).toBe('POST');
    expect(JSON.parse(init.body)).toEqual({
      user_id: 'usr_1',
      can_manage_users: true,
      can_manage_group: true,
      can_manage_servers: true,
      can_manage_services: true,
      can_manage_resources: true,
      can_manage_vendor_accounts: true,
    });
  });

  it('promoteManager sends an explicit can_manage_vendor_accounts=false', async () => {
    const fetcher = vi.fn().mockResolvedValue(jsonResponse({ ok: true }));
    const api = createPortalApi(fetcher);

    await api.promoteManager('grp_1', 'usr_1', {
      canManageUsers: true,
      canManageGroup: false,
      canManageServers: true,
      canManageServices: false,
      canManageResources: true,
      canManageVendorAccounts: false,
    });

    expect(JSON.parse(fetcher.mock.calls[0][1].body)).toEqual({
      user_id: 'usr_1',
      can_manage_users: true,
      can_manage_group: false,
      can_manage_servers: true,
      can_manage_services: false,
      can_manage_resources: true,
      can_manage_vendor_accounts: false,
    });
  });

  it('setManagerPermissions PATCHes all six flags, mapping canManageVendorAccounts to can_manage_vendor_accounts', async () => {
    const fetcher = vi.fn().mockResolvedValue(jsonResponse({ ok: true }));
    const api = createPortalApi(fetcher);

    await api.setManagerPermissions('grp/1', 'usr/2', {
      canManageUsers: false,
      canManageGroup: true,
      canManageServers: false,
      canManageServices: true,
      canManageResources: false,
      canManageVendorAccounts: true,
    });

    const [url, init] = fetcher.mock.calls[0];
    expect(url).toBe('/api/portal/groups/grp%2F1/managers/usr%2F2');
    expect(init.method).toBe('PATCH');
    expect(JSON.parse(init.body)).toEqual({
      can_manage_users: false,
      can_manage_group: true,
      can_manage_servers: false,
      can_manage_services: true,
      can_manage_resources: false,
      can_manage_vendor_accounts: true,
    });
  });
});
