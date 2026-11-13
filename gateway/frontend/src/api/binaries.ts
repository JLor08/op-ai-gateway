// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// A server's binary inventory: the llama-server / ggml-rpc-server binaries
// the agent reports (read-only except for a version override) plus rows the
// operator adds by hand. Mirrors the Go ServerBinaryDTO / PutServerBinaryRequest
// (portal/service_binaries.go).

import { type Fetcher, buildQueryString, request, requestNoContent } from './transport';

export type ServerBinaryKind = 'llama_server' | 'rpc_server' | 'vllm' | 'other';

export interface ServerBinary {
  path: string;
  // One of ServerBinaryKind; typed wider so a kind a newer gateway adds still
  // reads (the portal shows it as Other).
  kind: string;
  // Detected (agent) or entered (manual) version, build and commit.
  version: string;
  version_override: string;
  // The version the gateway acts on: the override when set, else version.
  effective_version: string;
  build: string;
  commit: string;
  // How the version was determined ("agent", "path" or "manual") and who owns
  // the row ("agent" or "manual").
  version_source: string;
  source: string;
  // Whether the agent's binary allow-list admits the path.
  allowed: boolean;
  reported_at: string;
  // Where the entry runs: "native", or "wsl" for a wsl://<distro>/<path>
  // allow-list entry. A row from an older gateway lacks these; read it as
  // native.
  execution: 'native' | 'wsl';
  // The WSL distro, '' for a native row.
  distro: string;
  // The path to execute: the Linux path for a WSL row, path for a native row.
  exec_path: string;
}

// POST: every field but path is the row's content. PUT: an absent field is
// left unchanged; kind/version/build/commit apply to a manual row only,
// version_override ("" clears it) to any row. path travels in the body (a
// path in the URL would need encoding).
export interface PutServerBinaryRequest {
  path: string;
  kind?: ServerBinaryKind;
  version?: string;
  build?: string;
  commit?: string;
  version_override?: string;
}

export function binariesApi(fetcher: Fetcher) {
  const base = (serverId: string) => `/api/portal/servers/${encodeURIComponent(serverId)}/binaries`;
  return {
    serverBinaries: (serverId: string) =>
      request<{ data: ServerBinary[] }>(fetcher, base(serverId)),
    createServerBinary: (serverId: string, body: PutServerBinaryRequest) =>
      request<ServerBinary>(fetcher, base(serverId), { method: 'POST', body }),
    updateServerBinary: (serverId: string, body: PutServerBinaryRequest) =>
      request<ServerBinary>(fetcher, base(serverId), { method: 'PUT', body }),
    // Removes a manual row; 204 on success, 409 binary.agent_owned for an
    // agent-reported one.
    deleteServerBinary: (serverId: string, path: string) =>
      requestNoContent(fetcher, `${base(serverId)}${buildQueryString({ path })}`, {
        method: 'DELETE',
      }),
  };
}
