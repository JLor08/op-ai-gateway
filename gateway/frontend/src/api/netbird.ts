// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { type Fetcher, request } from './transport';
import type { PortalServer } from './servers';

// Live NetBird transport status (GET /api/system/netbird/status). Reports whether
// the NetBird-bound agent listener is actually up (the deployment capability behind
// the netbird_only switch) so the operator can see if flipping netbird_only ON will
// really isolate inbound.
export type NetbirdStatus = {
  agent_listener_active: boolean;
  agent_listener_addr: string;
  netbird_only: boolean;
  gateway_peer_id: string;
  gateway_peer_connected: boolean;
  // gateway_peer_name is the peer's CURRENT (live) NetBird name, from the same
  // best-effort GetPeer used for gateway_peer_connected. Empty when there is no
  // peer id or the lookup failed.
  gateway_peer_name: string;
  // True when a shared setup-key file is configured (a self-enroll sidecar is
  // wired) — gates the "Sidecar enrollen" button.
  sidecar_enroll_available: boolean;
  // Policy-management diagnostics (mirrors the system-settings fields, plus the
  // live drift/inventory counters the reconcile loop observes).
  manage_policies: boolean;
  policy_scope: string;
  effective_policy_scope: string;
  deny_by_default: boolean;
  deny_by_default_enforce: boolean;
  managed_policy_count: number;
  default_policy_present: boolean;
  default_policy_enabled: boolean;
  deny_by_default_drift: boolean;
};

// Live validity of the current NetBird admin API token (GET
// /api/system/netbird/token-status, system scope). known=false when the
// module is off/unconfigured or the token's owning user/id can't be resolved
// (e.g. a manually-linked token shared with other tokens) — the fields are
// then meaningless placeholders and should not be rendered.
export type NetbirdTokenStatus = {
  known: boolean;
  name: string;
  expiration_date: string;
  days_remaining: number;
  last_used: string;
};

// Outcome of a token rotation (POST /api/system/netbird/rotate-token, system
// scope). The new token's plaintext value is NEVER returned — only its
// (non-secret) expiry. old_deleted reports whether the previous token was
// cleaned up; old_unknown reports that its id couldn't be determined (so it
// was left alone and should be removed manually in the NetBird dashboard).
export type RotateNetbirdTokenResult = {
  expiration_date: string;
  days_remaining: number;
  old_deleted: boolean;
  old_unknown: boolean;
};

// NetBird account network settings (GET/PUT /api/system/netbird/network, system
// scope). Live editor of the NetBird account — NOT gateway system_settings.
export type NetbirdNetwork = {
  dns_domain: string;
  network_range: string;
  network_range_v6: string;
  ipv6_enabled_groups: string[];
};

export function netbirdApi(fetcher: Fetcher) {
  return {
    // Test the NetBird admin connection. With no args it tests the SAVED config;
    // pass url/token to test unsaved credentials (never persisted server-side).
    testNetbird: (override?: { url?: string; token?: string }) =>
      request<{ ok: boolean; error?: string }>(fetcher, '/api/system/netbird/test', {
        method: 'POST',
        ...(override ? { body: override } : {}),
      }),
    // Run a real unprivileged ICMP echo from the gateway to a selected server
    // (owner-or-admin, gateway:use scope). ok=false carries a human-readable
    // error; latency_ms is the round-trip time on success.
    pingServer: (id: string) =>
      request<{ ok: boolean; latency_ms?: number; error?: string }>(
        fetcher,
        `/api/portal/servers/${encodeURIComponent(id)}/ping`,
        { method: 'POST' },
      ),
    // Live NetBird transport status (see NetbirdStatus above): whether the
    // NetBird-bound agent listener is up + the selected gateway peer's link state.
    netbirdStatus: () => request<NetbirdStatus>(fetcher, '/api/system/netbird/status'),
    // Live validity of the current NetBird admin API token (see
    // NetbirdTokenStatus above); a pure read, never returns the token value.
    netbirdTokenStatus: () =>
      request<NetbirdTokenStatus>(fetcher, '/api/system/netbird/token-status'),
    // Create+verify+switch to a fresh NetBird admin API token, best-effort
    // deleting the old one; rolls back (keeps the old token active) on any
    // failure. The new token's value is never returned.
    rotateNetbirdToken: () =>
      request<RotateNetbirdTokenResult>(fetcher, '/api/system/netbird/rotate-token', {
        method: 'POST',
      }),
    regenerateNetbirdKey: (serverId: string) =>
      request<{ setup_key: string; netbird_setup_command?: string }>(
        fetcher,
        `/api/portal/servers/${encodeURIComponent(serverId)}/netbird/setup-key`,
        { method: 'POST' },
      ),
    // Read the NetBird account's network settings (system scope).
    netbirdNetwork: () => request<NetbirdNetwork>(fetcher, '/api/system/netbird/network'),
    // Write the NetBird account's network settings (read-modify-write on the
    // backend; system scope).
    updateNetbirdNetwork: (body: NetbirdNetwork) =>
      request<NetbirdNetwork>(fetcher, '/api/system/netbird/network', { method: 'PUT', body }),
    // List the NetBird groups (id + name) for the settings group picker (system scope).
    netbirdGroups: () =>
      request<{ data: { id: string; name: string }[] }>(fetcher, '/api/system/netbird/groups'),
    // List the NetBird peers (id + name + dns_label + connected) for the
    // linkage-editor peer picker (system scope). Leaks only these four fields.
    netbirdPeers: () =>
      request<{ data: { id: string; name: string; dns_label: string; connected: boolean }[] }>(
        fetcher,
        '/api/system/netbird/peers',
      ),
    // Mint a one-off setup key for the GATEWAY's own NetBird peer (system scope,
    // display-once — the value is never persisted/returned again).
    createGatewaySetupKey: () =>
      request<{ setup_key: string; netbird_setup_command: string }>(
        fetcher,
        '/api/system/netbird/gateway-setup-key',
        { method: 'POST' },
      ),
    // Mint a setup key AND write it to the shared-volume key file so the custom
    // NetBird sidecar self-enrolls (system scope). The key + command are also
    // returned display-once as a copy-paste fallback. 409 netbird.key_file_not_configured
    // when OP_AI_GATEWAY_NETBIRD_KEY_FILE is not set.
    enrollGatewaySidecar: () =>
      request<{ setup_key: string; netbird_setup_command: string }>(
        fetcher,
        '/api/system/netbird/enroll-sidecar',
        { method: 'POST' },
      ),
    // A boolean-only module-enabled flag readable by any portal user (gateway:use) —
    // gates the NetBird UI (create checkbox / enroll action / connection column) for
    // non-system-admins without leaking the url/token/group. `enabled` = fully
    // configured (checkbox + url + token); `module_enabled` = the RAW checkbox
    // state, used to gate the NetBird nav item/view as soon as it's flipped on
    // (before url/token are configured) so a system-admin can reach the view to
    // finish configuring it.
    netbirdEnabled: () =>
      request<{
        enabled: boolean;
        module_enabled: boolean;
        netbird_only: boolean;
        manage_policies: boolean;
        effective_policy_scope: string;
        deny_by_default: boolean;
      }>(fetcher, '/api/portal/netbird/enabled'),
    // System-admin linkage editor: set a server's netbird-enabled flag + peer id +
    // the peer's POLICY group ids (to link a manually-created NetBird peer + push
    // its group membership) + whether the peer is treated as gateway-created
    // (peerManaged — governs the delete pre-selection + whether the setup key may
    // be regenerated) + the per-server policy-management override
    // ("" / "include" / "exclude"). Returns the updated server DTO.
    setServerNetbird: (
      id: string,
      enabled: boolean,
      peerId: string,
      groupIds: string[],
      peerManaged: boolean,
      policyOverride: string,
      allowPing: boolean,
      pingExclude: boolean,
    ) =>
      request<PortalServer>(fetcher, `/api/system/servers/${encodeURIComponent(id)}/netbird`, {
        method: 'PUT',
        body: {
          netbird_enabled: enabled,
          netbird_peer_id: peerId,
          netbird_group_ids: groupIds,
          netbird_peer_managed: peerManaged,
          netbird_policy_override: policyOverride,
          netbird_allow_ping: allowPing,
          netbird_ping_exclude: pingExclude,
        },
      }),
  };
}
