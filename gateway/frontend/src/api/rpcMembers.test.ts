// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it, vi } from 'vitest';
import { createPortalApi, PortalApiError, type PutRPCMemberSpecRequest } from '../api';

function jsonResponse(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

const body: PutRPCMemberSpecRequest = {
  enabled: true,
  binary: '/opt/llama/rpc-server',
  args: ['-t', '8'],
  env: {},
  gpus: [{ index: 0, vram_estimate_mb: 8000, vram_measured_mb: 0 }],
  cache_enabled: false,
  device_backend: 'auto',
  ram_estimate_mb: 0,
  allowed_head_groups: ['grp_1'],
};

describe('rpcMembersApi', () => {
  it('lists a server\'s manual member specs from {"data":[...]}', async () => {
    const fetcher = vi.fn().mockResolvedValue(jsonResponse({ data: [{ id: 'rpcm_1' }] }));
    const got = await createPortalApi(fetcher).rpcMemberSpecs('srv/1');
    expect(fetcher.mock.calls[0][0]).toBe('/api/portal/servers/srv%2F1/rpc-members');
    expect(got.data).toHaveLength(1);
  });

  it('POSTs a create with the full body', async () => {
    const fetcher = vi.fn().mockResolvedValue(jsonResponse({ id: 'rpcm_1' }, 201));
    await createPortalApi(fetcher).createRPCMemberSpec('srv_1', body);
    const [url, init] = fetcher.mock.calls[0];
    expect(url).toBe('/api/portal/servers/srv_1/rpc-members');
    expect(init.method).toBe('POST');
    expect(init.headers['X-OP-CSRF']).toBe('1');
    expect(JSON.parse(init.body)).toEqual(body);
  });

  it('PUTs an update to the spec id, URL-encoded', async () => {
    const fetcher = vi.fn().mockResolvedValue(jsonResponse({ id: 'rpcm/1' }));
    await createPortalApi(fetcher).updateRPCMemberSpec('srv_1', 'rpcm/1', body);
    const [url, init] = fetcher.mock.calls[0];
    expect(url).toBe('/api/portal/servers/srv_1/rpc-members/rpcm%2F1');
    expect(init.method).toBe('PUT');
  });

  it('DELETE resolves on 204 without a body', async () => {
    const fetcher = vi.fn().mockResolvedValue(new Response(null, { status: 204 }));
    await expect(
      createPortalApi(fetcher).deleteRPCMemberSpec('srv_1', 'rpcm_1'),
    ).resolves.toBeUndefined();
    const [url, init] = fetcher.mock.calls[0];
    expect(url).toBe('/api/portal/servers/srv_1/rpc-members/rpcm_1');
    expect(init.method).toBe('DELETE');
    expect(init.headers['X-OP-CSRF']).toBe('1');
  });

  it('DELETE surfaces the in-use refusal as a PortalApiError with its code', async () => {
    const fetcher = vi
      .fn()
      .mockResolvedValue(
        jsonResponse({ error: { code: 'runtime.cluster_member_in_use', message: 'x' } }, 409),
      );
    await expect(createPortalApi(fetcher).deleteRPCMemberSpec('s', 'r')).rejects.toMatchObject({
      status: 409,
      code: 'runtime.cluster_member_in_use',
    });
    expect(PortalApiError).toBeDefined();
  });
});

describe('serversApi RPC cluster settings', () => {
  it('PATCHes the LAN address and member consent fields', async () => {
    const fetcher = vi.fn().mockResolvedValue(jsonResponse({ id: 'srv_1' }));
    await createPortalApi(fetcher).updateServer('srv_1', {
      lan_address: '192.168.1.5',
      lan_bind_only: true,
      rpc_member_enabled: true,
      rpc_allowed_head_groups: ['grp_1'],
    });
    const [, init] = fetcher.mock.calls[0];
    expect(init.method).toBe('PATCH');
    expect(JSON.parse(init.body)).toEqual({
      lan_address: '192.168.1.5',
      lan_bind_only: true,
      rpc_member_enabled: true,
      rpc_allowed_head_groups: ['grp_1'],
    });
  });
});
