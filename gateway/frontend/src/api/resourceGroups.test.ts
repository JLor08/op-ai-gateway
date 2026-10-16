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

// The resource-group MANAGER side of a shared vendor account (#183, spec
// 2026-10-15 section 9): the vendor-account counterpart of the server-membership
// pair (resourceGroupServerCandidates / setResourceGroupServers).
describe('resourceGroupsApi vendor accounts', () => {
  it('GETs the vendor-account candidates from the {data:[...]} envelope, id URL-encoded', async () => {
    const candidates = [
      { id: 'va_1', name: 'Team OpenAI' },
      { id: 'va_2', name: 'Team Anthropic' },
    ];
    const fetcher = vi.fn().mockResolvedValue(jsonResponse({ data: candidates }));
    const api = createPortalApi(fetcher);

    const resp = await api.resourceGroupVendorAccountCandidates('rg/1');

    expect(fetcher).toHaveBeenCalledWith(
      '/api/portal/resource-groups/rg%2F1/vendor-account-candidates',
      { headers: {}, credentials: 'include' },
    );
    expect(resp).toEqual(candidates);
  });

  it('PUTs .../vendor-accounts with the full replacement set as vendor_account_ids and returns the group', async () => {
    const group = { id: 'rg/1', vendor_accounts: [{ id: 'va_1', name: 'Team OpenAI' }] };
    const fetcher = vi.fn().mockResolvedValue(jsonResponse(group));
    const api = createPortalApi(fetcher);

    const resp = await api.setResourceGroupVendorAccounts('rg/1', ['va_1']);

    const [url, init] = fetcher.mock.calls[0];
    expect(url).toBe('/api/portal/resource-groups/rg%2F1/vendor-accounts');
    expect(init.method).toBe('PUT');
    expect(init.headers['X-OP-CSRF']).toBe('1');
    expect(JSON.parse(init.body)).toEqual({ vendor_account_ids: ['va_1'] });
    expect(resp).toMatchObject({ vendor_accounts: [{ id: 'va_1', name: 'Team OpenAI' }] });
  });

  it('sends an empty vendor_account_ids array (never null) to drop every link', async () => {
    const fetcher = vi.fn().mockResolvedValue(jsonResponse({ id: 'rg_1', vendor_accounts: [] }));
    const api = createPortalApi(fetcher);

    await api.setResourceGroupVendorAccounts('rg_1', []);

    const [, init] = fetcher.mock.calls[0];
    expect(JSON.parse(init.body)).toEqual({ vendor_account_ids: [] });
  });
});
