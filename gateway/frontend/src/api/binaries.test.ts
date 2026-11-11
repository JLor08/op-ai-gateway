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

describe('binariesApi', () => {
  it('lists the inventory from {"data":[...]}', async () => {
    const fetcher = vi.fn().mockResolvedValue(jsonResponse({ data: [{ path: '/a' }] }));
    const got = await createPortalApi(fetcher).serverBinaries('srv/1');
    expect(fetcher.mock.calls[0][0]).toBe('/api/portal/servers/srv%2F1/binaries');
    expect(got.data[0].path).toBe('/a');
  });

  it('POSTs a manual row', async () => {
    const fetcher = vi.fn().mockResolvedValue(jsonResponse({ path: '/a' }, 201));
    await createPortalApi(fetcher).createServerBinary('srv_1', {
      path: '/opt/rpc-server',
      kind: 'rpc_server',
      version: 'b1234',
    });
    const [url, init] = fetcher.mock.calls[0];
    expect(url).toBe('/api/portal/servers/srv_1/binaries');
    expect(init.method).toBe('POST');
    expect(JSON.parse(init.body)).toEqual({
      path: '/opt/rpc-server',
      kind: 'rpc_server',
      version: 'b1234',
    });
  });

  it('PUTs an edit with the path in the body, never in the URL', async () => {
    const fetcher = vi.fn().mockResolvedValue(jsonResponse({ path: '/a' }));
    await createPortalApi(fetcher).updateServerBinary('srv_1', {
      path: '/opt/a b',
      version_override: 'b1234',
    });
    const [url, init] = fetcher.mock.calls[0];
    expect(url).toBe('/api/portal/servers/srv_1/binaries');
    expect(init.method).toBe('PUT');
    expect(JSON.parse(init.body)).toEqual({ path: '/opt/a b', version_override: 'b1234' });
  });

  it('DELETEs with the path as an encoded query parameter and accepts 204', async () => {
    const fetcher = vi.fn().mockResolvedValue(new Response(null, { status: 204 }));
    await createPortalApi(fetcher).deleteServerBinary('srv_1', '/opt/a b');
    const [url, init] = fetcher.mock.calls[0];
    expect(url).toBe('/api/portal/servers/srv_1/binaries?path=%2Fopt%2Fa+b');
    expect(init.method).toBe('DELETE');
  });
});
