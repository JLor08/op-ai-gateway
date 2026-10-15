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

// Service-side resource-group self-service (#183 Phase 0): the service-manager
// analogue of the server-owner trio in api/servers.ts.
describe('servicesApi resource-group self-service', () => {
  it('lists the resource groups of a service from the {data:[...]} envelope, id URL-encoded', async () => {
    const groups = [
      { id: 'rg-1', name: 'Alpha', member: true },
      { id: 'rg-2', name: 'Beta', member: false },
    ];
    const fetcher = vi.fn().mockResolvedValue(jsonResponse({ data: groups }));
    const api = createPortalApi(fetcher);

    const resp = await api.serviceResourceGroups('svc/1');

    expect(fetcher).toHaveBeenCalledWith('/api/portal/services/svc%2F1/resource-groups', {
      headers: {},
      credentials: 'include',
    });
    expect(resp).toEqual(groups);
  });

  it('PUTs .../resource-groups/{rgId} to join, both ids URL-encoded, with the CSRF header', async () => {
    const fetcher = vi.fn().mockResolvedValue(jsonResponse({ ok: true }));
    const api = createPortalApi(fetcher);

    const resp = await api.joinServiceResourceGroup('svc/1', 'rg/2');

    const [url, init] = fetcher.mock.calls[0];
    expect(url).toBe('/api/portal/services/svc%2F1/resource-groups/rg%2F2');
    expect(init.method).toBe('PUT');
    expect(init.headers['X-OP-CSRF']).toBe('1');
    expect(init.body).toBeUndefined();
    expect(resp.ok).toBe(true);
  });

  it('DELETEs .../resource-groups/{rgId} to leave, both ids URL-encoded, with the CSRF header', async () => {
    const fetcher = vi.fn().mockResolvedValue(jsonResponse({ ok: true }));
    const api = createPortalApi(fetcher);

    const resp = await api.leaveServiceResourceGroup('svc/1', 'rg/2');

    const [url, init] = fetcher.mock.calls[0];
    expect(url).toBe('/api/portal/services/svc%2F1/resource-groups/rg%2F2');
    expect(init.method).toBe('DELETE');
    expect(init.headers['X-OP-CSRF']).toBe('1');
    expect(resp.ok).toBe(true);
  });
});
