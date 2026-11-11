// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Manual llama.cpp RPC member specs: a ggml-rpc-server the operator
// configures on a server by hand, which a cluster head in manual member mode
// then names. Mirrors the Go RPCMemberSpecDTO / PutRPCMemberSpecRequest
// (portal/service_rpc_members.go).

import { type Fetcher, request, requestNoContent } from './transport';
import type { DeviceBackend, RuntimeSpecGPU } from './runtime';

export interface RPCMemberSpec {
  id: string;
  server_id: string;
  enabled: boolean;
  binary: string;
  // Operator args (e.g. -t 8). -H/-p/-d/-c belong to the agent and are
  // refused with runtime_spec.member_flag_reserved.
  args: string[];
  env: Record<string, string>;
  // Empty = CPU member. vram_measured_mb is always 0 here (what a member used
  // is kept per head on the membership rows) and ignored on write.
  gpus: RuntimeSpecGPU[];
  cache_enabled: boolean;
  device_backend: DeviceBackend;
  ram_estimate_mb: number;
  // Admin-tier group ids whose heads may use the member; any admin-tier group
  // may be named. Empty = heads linked to the member server's own admin
  // groups.
  allowed_head_groups: string[];
  // Set while a head's membership names this spec.
  used_by_head_spec_id?: string;
}

// A full-document write. enabled and allowed_head_groups are optional: absent
// keeps the stored value (enabled defaults to true on create).
export type PutRPCMemberSpecRequest = Omit<
  RPCMemberSpec,
  'id' | 'server_id' | 'used_by_head_spec_id' | 'enabled' | 'allowed_head_groups'
> & {
  enabled?: boolean;
  allowed_head_groups?: string[];
};

export function rpcMembersApi(fetcher: Fetcher) {
  const base = (serverId: string) =>
    `/api/portal/servers/${encodeURIComponent(serverId)}/rpc-members`;
  return {
    rpcMemberSpecs: (serverId: string) =>
      request<{ data: RPCMemberSpec[] }>(fetcher, base(serverId)),
    createRPCMemberSpec: (serverId: string, body: PutRPCMemberSpecRequest) =>
      request<RPCMemberSpec>(fetcher, base(serverId), { method: 'POST', body }),
    updateRPCMemberSpec: (serverId: string, specId: string, body: PutRPCMemberSpecRequest) =>
      request<RPCMemberSpec>(fetcher, `${base(serverId)}/${encodeURIComponent(specId)}`, {
        method: 'PUT',
        body,
      }),
    // 204 on success; 409 runtime.cluster_member_in_use while a head uses it.
    deleteRPCMemberSpec: (serverId: string, specId: string) =>
      requestNoContent(fetcher, `${base(serverId)}/${encodeURIComponent(specId)}`, {
        method: 'DELETE',
      }),
  };
}
