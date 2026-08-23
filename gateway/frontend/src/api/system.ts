// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { type Fetcher, request, requestText, subscribeSSE } from './transport';
import type { CurrencyUnit, PortalServer } from './servers';

// A single gateway log line (mirrors internal/logbuffer.Record). The bearer/agent
// token is never present in `attrs`. `t` is an RFC3339 timestamp; `level` is one
// of "DEBUG"|"INFO"|"WARN"|"ERROR" (as emitted by slog).
export type LogRecord = {
  t: string;
  level: string;
  msg: string;
  attrs?: Record<string, unknown>;
};

// OpenTelemetry method-level tracing on/off (GET/PUT /api/system/tracing).
// otlp_endpoint_set reports whether an OTLP exporter endpoint is configured
// (informational only — enabling tracing without one is still accepted).
export type TracingStatus = {
  enabled: boolean;
  otlp_endpoint_set: boolean;
};

// A selectable theme option surfaced to the theme picker: either a built-in
// or an externally loaded theme (see ExternalThemeData). id is what gets
// sent back on save; name is the display label.
export type ThemeOption = { id: string; name: string };

// The public, pre-auth payload of GET /api/system/theme's "data" field --
// present only when the active theme resolves to an externally loaded theme
// (source === "external"); null for a built-in theme (the frontend uses its
// own compiled copy). Mirrors the backend's theme.Theme (gateway/backend/
// internal/theme/theme.go) as marshaled onto the wire: light/dark use
// `omitempty` on the Go side, so either may be absent.
export type ExternalThemeData = {
  id: string;
  name: string;
  productName: string;
  font: string;
  brand: { type: 'text' | 'image'; text: string; title: string };
  light?: Record<string, string>;
  dark?: Record<string, string>;
  hasFavicon: boolean;
  hasLogo: boolean;
};

export type SystemSettings = {
  theme: string;
  available_themes: ThemeOption[];
  language: string;
  available_languages: string[];
  capture_retention_days: number;
  capture_enabled: boolean;
  capture_override: boolean;
  health_check_interval_seconds: number;
  // System-wide default window (seconds) for "the ServerAgent is delivering
  // values"; a per-server agent_presence_timeout_seconds override (0 = follow
  // this) mirrors the health-check-interval pattern.
  agent_presence_timeout_seconds: number;
  smtp_enabled: boolean;
  smtp_host: string;
  smtp_port: number;
  smtp_username: string;
  smtp_password_set: boolean;
  smtp_from: string;
  smtp_from_name: string;
  smtp_tls_mode: string;
  totp_mode: string;
  // Route-affinity session key mode: "client_session" (default; key sessions on
  // the extracted client/agent session id) or "legacy_header" (explicit header).
  route_affinity_session_mode: string;
  // Vision-capability detection mode for the benchmark's "vision" probe:
  // "accept" = the upstream accepted an image without erroring is enough;
  // "verify" = additionally verify the model's answer reflects the image content.
  vision_probe_mode: string;
  // Energy-attribution defaults (purely additive — no engine consumes these
  // yet; a later phase falls back to them when a per-mapping/per-server value
  // is unknown). All default 0 = "unset / no default".
  energy_default_price_per_kwh: number;
  energy_default_pue: number;
  energy_default_wh_per_token: number;
  // Currency conversion factor (USD per 1 EUR) driving USD-unit availability
  // across the app; <= 0 means USD units are unavailable. System-wide default
  // price unit new servers/mappings display/enter cost in.
  currency_usd_per_eur: number;
  energy_default_price_unit: CurrencyUnit;
  // NetBird module. netbird_token_set reports token presence only — never the value.
  netbird_enabled: boolean;
  netbird_url: string;
  netbird_groups: string[];
  netbird_token_set: boolean;
  // NetBird-only transport: when on, the gateway↔AI-server plane is restricted to
  // the NetBird overlay (outbound off-mesh refusal + inbound public-listener reject).
  // netbird_gateway_peer_id selects which NetBird peer represents the gateway (the
  // agent-listener bind target, resolved at startup — takes effect on restart).
  netbird_only: boolean;
  netbird_gateway_peer_id: string;
  // netbird_gateway_peer_name is applied to the selected gateway peer automatically
  // (rename via NetBird), before/after enroll. Empty = no rename.
  netbird_gateway_peer_name: string;
  // NetBird policy management: when on, the gateway maintains least-privilege
  // access policies (gateway → server, app ports only) per the configured scope.
  // netbird_policy_scope is the operator's choice (auto/all/selected);
  // netbird_effective_policy_scope is the READ-ONLY resolved scope (auto is
  // resolved against netbird_deny_by_default) — never send it back in a PUT.
  netbird_manage_policies: boolean;
  netbird_policy_scope: string;
  netbird_effective_policy_scope: string;
  // Deny-by-default: gateway-managed policies default new peers to deny access
  // unless explicitly selected; _enforce actively corrects drift back to that
  // posture (vs. only setting the default for newly created policies).
  netbird_deny_by_default: boolean;
  netbird_deny_by_default_enforce: boolean;
  // Cadence (seconds) for the peer-sync loop and the policy-reconcile loop.
  netbird_peer_sync_interval_seconds: number;
  netbird_reconcile_interval_seconds: number;
  // Ping-allow: when on, the gateway maintains ICMP allow policies so the
  // gateway peer is pingable (op-gw-ping-gateway) / all NetBird servers are
  // pingable from the gateway (op-gw-ping-servers).
  netbird_allow_ping_gateway: boolean;
  netbird_allow_ping_all_servers: boolean;
  // Auto-rotation threshold (days before expiry) for the NetBird admin API
  // token; 0 disables auto-rotation (manual rotation via rotateNetbirdToken
  // still works).
  netbird_token_rotate_before_days: number;
  // When on, the agent-token curl download is reachable only over the NetBird
  // network (the portal file download stays available regardless).
  netbird_agent_download_only: boolean;
  // System-admin step-up mode: whether entering system-admin mode requires
  // re-entering the account password (default true).
  system_admin_mode_require_password: boolean;
  // Resource-group provisioning enforcement (Phase 2, spec
  // 2026-08-12-resource-groups-phase-2-provisioning). Off (default) =
  // provisioning is an additional, opt-in grant on top of the existing
  // access model. On = deny-by-default -- only provisioned
  // users/groups/services may use a resource group's member servers.
  resource_provisioning_enforce: boolean;
  // TLS-certificate management module. cert_enabled is the ONLY certificate
  // field the System-Settings view saves; every other cert_*/acme_* field
  // below is saved from the dedicated CertificateSettings view.
  cert_enabled: boolean;
  // Issuer: "acme" (Let's Encrypt, publicly trusted) or "self_signed" (an
  // internal CA the gateway generates + rotates itself).
  cert_issuer_mode: 'acme' | 'self_signed';
  // self_signed-mode: the lifetime (days) issued leaf certificates get, and
  // how many days before the internal CA's own expiry it auto-rotates.
  cert_self_signed_validity_days: number;
  cert_ca_renew_before_days: number;
  // acme-mode: the account email + the ACME directory URL (production /
  // staging / a custom endpoint).
  acme_email: string;
  acme_directory_url: string;
  // Shared, issuer-independent config. cert_base_domain/_gateway_domain empty
  // = derive from NetBird / the gateway peer. cert_server_scope mirrors the
  // NetBird policy-override scope ("all" except opted-out servers, or "selected"
  // servers only). cert_manage_public_domain + cert_public_domains cover an
  // OPTIONAL additional public-facing domain set (always a non-null array).
  // cert_renew_before_days is the leaf-certificate renewal window (days).
  cert_base_domain: string;
  cert_gateway_domain: string;
  cert_server_scope: 'all' | 'selected';
  cert_manage_public_domain: boolean;
  cert_public_domains: string[];
  cert_renew_before_days: number;
  // The edge (gateway-nginx) certificate's OWN switch/issuer/name set -- a
  // THIRD field the backend returns on every GET (service_system_settings.go
  // SystemSettingsDTO.CertEdgeEnabled/_IssuerMode/_Names), saved ONLY from
  // EdgeCertificatePanel's own disjoint PUT partition (never alongside
  // cert_enabled or the internal cert_*/acme_* fields above). Optional here
  // (fix round 1, MINOR 3) so existing SystemSettings test fixtures across the
  // codebase, which predate this feature, need no changes -- EdgeCertificatePanel
  // itself never reads this GET type; it seeds its edit form from
  // api.edgeCertificate() instead, whose enabled/issuer_mode/names mirror these
  // exact values.
  cert_edge_enabled?: boolean;
  cert_edge_issuer_mode?: 'acme' | 'self_signed';
  cert_edge_names?: string[];
  // The plaintext-refusal gate switch (Plan B) -- also on this DTO for
  // completeness with the backend, but EdgeCertificatePanel does not read it
  // here: it always seeds from api.edgeCertificate()'s own require_https field
  // (which is the SAME stored value, merged with the live observation state).
  cert_edge_require_https?: boolean;
  // P3 mesh plaintext-refusal switch. Like cert_edge_require_https it is sent
  // ALONE (never bundled), because the backend re-checks the arming precondition
  // (a ServerAgent seen over TLS recently) on every PUT that sets it true.
  cert_mesh_require_tls?: boolean;
  // Encrypted agent-port mode (Task 1, agent-mesh-tls-port). `cert_mesh_tls_mode`
  // is the WRITABLE runtime override: "" follows the ENV default, "combined"
  // multiplexes TLS+plaintext on one listener, "separate" runs a dedicated
  // TLS-only listener alongside the plaintext one. `cert_mesh_tls_port` and
  // `cert_mesh_tls_separate_active` are READ-ONLY (server-computed): the
  // effective TLS port and whether a separate TLS listener is actually active
  // right now (mode alone doesn't guarantee it -- e.g. no cert yet).
  cert_mesh_tls_mode?: '' | 'combined' | 'separate';
  cert_mesh_tls_port?: number;
  cert_mesh_tls_separate_active?: boolean;
  // U-T4 public-unification fields (backend since U-T3). CertPublicIssuerMode:
  // "" means "follow cert_issuer_mode" (the internal issuer). ACMEWeeklyLimit is
  // the per-week issuance ceiling for the GLOBAL (shared) ACME account; 0 = no
  // limit set. CertEdgeACMEShared/CertPublicACMEShared: true (the default) means
  // that context re-uses the global ACME account (email/directory/weekly limit
  // above); false means it uses its own via the sibling *_email/*_directory_url/
  // *_weekly_limit fields. All optional (fix round 1, MINOR 3 precedent) so
  // existing SystemSettings fixtures across the codebase, which predate this
  // feature, need no changes.
  cert_public_issuer_mode?: 'acme' | 'self_signed' | '';
  acme_weekly_limit?: number;
  cert_edge_acme_shared?: boolean;
  cert_edge_acme_email?: string;
  cert_edge_acme_directory_url?: string;
  cert_edge_acme_weekly_limit?: number;
  cert_public_acme_shared?: boolean;
  cert_public_acme_email?: string;
  cert_public_acme_directory_url?: string;
  cert_public_acme_weekly_limit?: number;
  // P4 global https-auto-switch mode: "manual" (default, never auto-switches),
  // "auto" (every eligible application switches to https unless a per-server
  // HTTPSSwitchOverride opts it out), "selected" (only servers whose override
  // is "include" switch). cert_proxy_listen_port_base is the auto-assign floor
  // for a managed application's proxy_listen_port (default 8600). Optional so
  // existing SystemSettings fixtures across the test suite compile unchanged.
  cert_https_switch_mode?: 'manual' | 'auto' | 'selected';
  cert_proxy_listen_port_base?: number;
};

// TLS-certificate management (certificates feature). One row per managed
// certificate (gateway / a NetBird server / an optional public domain); the
// optional fields are omitted by the backend when absent (never null).
export type CertificateRow = {
  domain: string;
  kind: string;
  server_id?: string;
  server_name?: string;
  status: string;
  fingerprint?: string;
  not_before?: string;
  not_after?: string;
  issued_at?: string;
  next_attempt_at?: string;
  attempt_count: number;
  last_error?: string;
  // What the server's ServerAgent last reported as ACTUALLY installed (Phase 2
  // distribution). Only ever set for a kind="server" row that has a report:
  // `installed` is true exactly when the reported leaf fingerprint EQUALS the
  // issued one, so a stale install reads as "reported, but different". All four
  // absent means "never reported" — NOT "not installed": the report registry is
  // in-memory, so a gateway restart erases every report without changing
  // anything on any server.
  installed?: boolean;
  installed_fingerprint?: string;
  installed_at?: string;
  installed_mode?: string;
  // What the gateway observed for this server's ServerAgent hop on the mesh
  // listener. Only set for a kind="server" row that has an observation:
  // `transport` is "tls" iff the newest hop arrived over HTTPS/WSS, "plain" iff
  // it arrived over HTTP/WS. Both absent means "never observed" — NOT "plain":
  // the observation is in-memory, so a gateway restart erases it too.
  transport?: 'tls' | 'plain';
  transport_at?: string;
};

export type CertificateMeshStatus = {
  tls_active: boolean;
  address?: string;
  fingerprint?: string;
  not_after?: string;
  ca_rotation_pending_servers: Array<{ id: string; name: string }>;
  // P3 mesh plaintext-refusal gate (cert_mesh_require_tls). `require_tls` is the
  // stored switch; `tls_observed` is whether a fresh-enough TLS hop exists to ARM
  // it (the toggle is disabled until then); `tls_pending_servers` names every
  // token-server the switch would lock out (latest mesh hop not TLS).
  require_tls?: boolean;
  tls_observed?: boolean;
  tls_pending_servers?: Array<{ id: string; name: string }>;
};

// The gateway's internal CA (self_signed issuer mode). present=false means no
// CA has been generated yet; the other fields are then meaningless placeholders.
// previous_fingerprint/_not_after are populated only right after a rotation,
// while the previous root still rides the distributed bundle.
export type CertificateCA = {
  present: boolean;
  subject?: string;
  fingerprint?: string;
  not_before?: string;
  not_after?: string;
  previous_fingerprint?: string;
  previous_not_after?: string;
  // Module-level abort note (backend cert_last_error): set when a reconcile pass
  // gave up before it could place or renew ANY order -- a disk store with no
  // OP_AI_GATEWAY_CERT_ENCRYPTION_KEY (the internal CA's key can't be sealed),
  // or no base domain configured/resolvable. Present regardless of `present` and
  // regardless of the issuer mode; empty once a later pass gets past both gates.
  last_error?: string;
};

// The gateway's OWN edge (nginx) certificate -- the TLS leg between the upstream
// reverse proxy and this gateway's own nginx. A fully separate row/mode from the
// internal (mesh) certificates above: its own issuer mode, its own name set, and
// -- what no other certificate has -- HOW it reaches nginx (delivery_mode). It
// carries no key/PEM material of any kind (that travels only over the dedicated
// bundle/key text endpoints below).
export type EdgeCertificate = {
  enabled: boolean;
  issuer_mode?: string;
  // The configured SAN list, normalized (trimmed/lowercased/deduped); never
  // absent, always at least [] (backend guarantee: "Never nil").
  names: string[];
  // Domain..last_error describe the STORED row; all absent when nothing has been
  // issued yet (domain === undefined, not "").
  domain?: string;
  status?: string;
  fingerprint?: string;
  not_before?: string;
  not_after?: string;
  issued_at?: string;
  last_error?: string;
  // "local" (the gateway writes the cert/key straight to its own nginx's disk) or
  // "download" (no safe local path -- e.g. a k8s deployment with no shared
  // volume -- so the operator must fetch bundle/key by hand). Always one of the two.
  delivery_mode: 'local' | 'download';
  output_dir?: string;
  // Present only once a local write has actually SUCCEEDED at least once; absent
  // (never a null/false) means "not written yet" -- the ONLY signal, under
  // delivery_mode "local", distinguishing "already on nginx's disk" from "will be
  // at the next reconcile pass". write_error, when non-empty, is the reason a
  // configured output_dir most recently fell back to "download".
  write_error?: string;
  written_at?: string;
  // True exactly when the key-download endpoint would succeed (local delivery is
  // impossible AND a key is actually stored) -- gate the download button on this
  // bit rather than re-deriving the rule.
  key_download_available: boolean;
  // The configured primary edge name when it collides with a name the gateway
  // already manages an internal certificate for (gateway/server/public domain) --
  // in that state the reconcile keeps the internal provenance and produces no
  // edge row at all. Absent/empty means no conflict was found (not "impossible").
  name_conflict?: string;
  // The plaintext-refusal gate (cert_edge_require_https, Plan B) -- merged onto
  // this SAME resource by the gateway handler (not computed by
  // EdgeCertificateView), so the panel has everything it needs from one fetch.
  // require_https mirrors the stored switch; https_observed is whether an
  // encrypted hop was seen within the arming window -- the ONE fact the switch's
  // "turn on" transition is gated on, both server-side (ArmEdgeRequireHTTPS) and
  // here (disable the control while false). last_encrypted_at/last_plain_at are
  // the raw last-seen timestamps for either hop (absent = never observed).
  require_https: boolean;
  https_observed: boolean;
  last_encrypted_at?: string;
  last_plain_at?: string;
};

// One synthetic TLS handshake against the gateway's OWN edge (nginx) listener
// (POST /api/system/certificates/edge/probe) -- the diagnostic an operator has
// BEFORE any real traffic has proven the fronting proxy speaks TLS at all.
// ok=true with reason=="" is the only success shape; otherwise reason names the
// CAUSE (unreachable / bootstrap_certificate / name_mismatch / chain_untrusted /
// expired) so the portal can show more than pass/fail. Never carries key
// material -- only what a TLS handshake alone reveals (the peer certificate).
export type EdgeTLSProbeResult = {
  ok: boolean;
  reason?: string;
  message?: string;
  target: string;
  expected_name?: string;
  subject?: string;
  sans?: string[];
  not_after?: string;
};

export function systemApi(fetcher: Fetcher) {
  return {
    // System-admin log view (see LogRecord above). `logs` is the ring snapshot +
    // current level; `setLogLevel` changes the live level at runtime.
    logs: () => request<{ records: LogRecord[]; level: string }>(fetcher, '/api/system/logs'),
    setLogLevel: (level: string) =>
      request<{ level: string }>(fetcher, '/api/system/logs/level', {
        method: 'PUT',
        body: { level },
      }),
    // OpenTelemetry method-level tracing (see TracingStatus above). getTracing
    // reads the current state; setTracing flips the live sampler and echoes the
    // resulting state back (mirrors the level get/set pair above).
    getTracing: () => request<TracingStatus>(fetcher, '/api/system/tracing'),
    setTracing: (enabled: boolean) =>
      request<TracingStatus>(fetcher, '/api/system/tracing', { method: 'PUT', body: { enabled } }),
    // Subscribe to the live gateway log SSE. The stream sends a `snapshot` frame
    // ({records, level}) on connect, then a `record` frame (one LogRecord) per
    // Append. Mirrors subscribeServerPerf (withCredentials, named-event listeners,
    // exp-backoff reconnect, idempotent unsubscribe); a malformed frame is
    // swallowed rather than thrown out of the handler.
    subscribeLogs: (
      onSnapshot: (records: LogRecord[], level: string) => void,
      onRecord: (record: LogRecord) => void,
      onStatus?: (status: 'open' | 'error') => void,
    ): (() => void) =>
      subscribeSSE(
        '/api/system/logs/events',
        {
          snapshot: (e) => {
            try {
              const parsed = JSON.parse(e.data) as {
                records?: LogRecord[];
                level?: string;
              };
              onSnapshot(parsed.records ?? [], parsed.level ?? 'info');
            } catch {
              // ignore a malformed frame
            }
          },
          record: (e) => {
            try {
              onRecord(JSON.parse(e.data) as LogRecord);
            } catch {
              // ignore a malformed frame
            }
          },
        },
        { onOpen: () => onStatus?.('open'), onError: () => onStatus?.('error') },
      ),
    // Lightweight reachability probe against the public /healthz endpoint. Used to
    // confirm a dropped SSE really means the backend is down (vs. a transient / auth
    // blip) before locking the UI, and to detect recovery. Never throws.
    checkHealth: async (): Promise<boolean> => {
      try {
        const res = await fetch('/healthz', {
          method: 'GET',
          cache: 'no-store',
          credentials: 'same-origin',
        });
        return res.ok;
      } catch {
        return false;
      }
    },
    getSystemSettings: () => request<SystemSettings>(fetcher, '/api/system/settings'),
    updateSystemSettings: (body: {
      theme?: string;
      language?: string;
      capture_retention_days?: number;
      capture_enabled?: boolean;
      capture_override?: boolean;
      health_check_interval_seconds?: number;
      agent_presence_timeout_seconds?: number;
      smtp_enabled?: boolean;
      smtp_host?: string;
      smtp_port?: number;
      smtp_username?: string;
      smtp_password?: string;
      smtp_from?: string;
      smtp_from_name?: string;
      smtp_tls_mode?: string;
      totp_mode?: string;
      route_affinity_session_mode?: string;
      vision_probe_mode?: string;
      // Energy-attribution defaults (0 resets to "unset / no default").
      energy_default_price_per_kwh?: number;
      energy_default_pue?: number;
      energy_default_wh_per_token?: number;
      // Currency conversion factor (USD per 1 EUR) + system-wide default price unit.
      currency_usd_per_eur?: number;
      energy_default_price_unit?: CurrencyUnit;
      // NetBird: netbird_token is nil = keep / "" = clear / value = replace.
      netbird_enabled?: boolean;
      netbird_url?: string;
      netbird_groups?: string[];
      netbird_token?: string;
      // NetBird-only transport toggle + the selected gateway peer id.
      netbird_only?: boolean;
      netbird_gateway_peer_id?: string;
      // The gateway peer's desired name (empty = no rename).
      netbird_gateway_peer_name?: string;
      // NetBird policy management (netbird_effective_policy_scope is READ-ONLY
      // and intentionally NOT accepted here).
      netbird_manage_policies?: boolean;
      netbird_policy_scope?: string;
      netbird_deny_by_default?: boolean;
      netbird_deny_by_default_enforce?: boolean;
      netbird_peer_sync_interval_seconds?: number;
      netbird_reconcile_interval_seconds?: number;
      // Ping-allow toggles (gateway pingable / all servers pingable).
      netbird_allow_ping_gateway?: boolean;
      netbird_allow_ping_all_servers?: boolean;
      // Auto-rotation threshold (days before expiry); 0 = auto-rotation off.
      netbird_token_rotate_before_days?: number;
      // Restrict the agent-token curl download to the NetBird network.
      netbird_agent_download_only?: boolean;
      // System-admin step-up mode: require a password re-entry to elevate.
      system_admin_mode_require_password?: boolean;
      // Resource-group provisioning enforcement (off = opt-in grant, on =
      // deny-by-default).
      resource_provisioning_enforce?: boolean;
      // TLS-certificate management. cert_enabled is saved ONLY from the
      // System-Settings view; every other field below is saved ONLY from the
      // dedicated CertificateSettings view (a disjoint partition — no field
      // is ever sent by both views).
      cert_enabled?: boolean;
      cert_issuer_mode?: 'acme' | 'self_signed';
      cert_self_signed_validity_days?: number;
      cert_ca_renew_before_days?: number;
      acme_email?: string;
      acme_directory_url?: string;
      cert_base_domain?: string;
      cert_gateway_domain?: string;
      cert_server_scope?: 'all' | 'selected';
      cert_manage_public_domain?: boolean;
      cert_public_domains?: string[];
      cert_renew_before_days?: number;
      // The edge (gateway-nginx) certificate's OWN switch/issuer/name set -- a
      // THIRD disjoint partition, saved ONLY from EdgeCertificatePanel and never
      // alongside cert_enabled or the internal cert_*/acme_* fields above.
      cert_edge_enabled?: boolean;
      cert_edge_issuer_mode?: 'acme' | 'self_signed';
      cert_edge_names?: string[];
      // The plaintext-refusal gate switch (Plan B) -- a FOURTH, independently
      // applied field: EdgeCertificatePanel sends it ALONE (never bundled with
      // the three fields above), because the backend re-checks the arming
      // precondition on every PUT that sets this field to true. Combining it
      // with an unrelated edit (e.g. renaming cert_edge_names) would re-run
      // that check on every such save and could reject an otherwise-unrelated
      // change the moment the encrypted-observation window happens to lapse.
      cert_edge_require_https?: boolean;
      // P3 mesh plaintext-refusal switch -- sent ALONE, same reasoning as
      // cert_edge_require_https (the backend re-checks the arming precondition).
      cert_mesh_require_tls?: boolean;
      // Encrypted agent-port mode -- the only writable field of the three added
      // for the agent-mesh-tls-port feature; cert_mesh_tls_port/
      // cert_mesh_tls_separate_active are response-only (see SystemSettings
      // above) and never appear in a PUT body. Sent alone, like
      // cert_mesh_require_tls just above.
      cert_mesh_tls_mode?: '' | 'combined' | 'separate';
      // U-T4 public-unification fields -- see the matching comment on
      // SystemSettings above for the exact semantics.
      cert_public_issuer_mode?: 'acme' | 'self_signed' | '';
      acme_weekly_limit?: number;
      cert_edge_acme_shared?: boolean;
      cert_edge_acme_email?: string;
      cert_edge_acme_directory_url?: string;
      cert_edge_acme_weekly_limit?: number;
      cert_public_acme_shared?: boolean;
      cert_public_acme_email?: string;
      cert_public_acme_directory_url?: string;
      cert_public_acme_weekly_limit?: number;
      // P4 global https-auto-switch mode + proxy-listen-port floor -- see the
      // matching comment on SystemSettings above for the exact semantics.
      cert_https_switch_mode?: 'manual' | 'auto' | 'selected';
      cert_proxy_listen_port_base?: number;
    }) => request<SystemSettings>(fetcher, '/api/system/settings', { method: 'PUT', body }),
    testSmtp: (body: { to?: string }) =>
      request<{ ok: boolean; error?: string }>(fetcher, '/api/system/smtp/test', {
        method: 'POST',
        body,
      }),
    // The effective system-wide health_check_interval_seconds, readable by any
    // portal user (gateway:use) so the application form can show the live
    // "Standard" cadence without the system-scoped settings endpoint.
    healthCheckInterval: () =>
      request<{ health_check_interval_seconds: number }>(
        fetcher,
        '/api/portal/health-check-interval',
      ),
    // The effective system-wide agent_presence_timeout_seconds default, readable
    // by any portal user (gateway:use) so the per-server field can show the live
    // "Standard" value without the system-scoped settings endpoint.
    agentPresenceTimeout: () =>
      request<{ seconds: number }>(fetcher, '/api/portal/agent-presence-timeout'),
    // The currency conversion factor (USD per 1 EUR), readable by any portal
    // user (gateway:use) so cost displays/entry can offer USD units without the
    // system-scoped settings endpoint. usd_per_eur <= 0 means USD is unavailable.
    getCurrency: () => request<{ usd_per_eur: number }>(fetcher, '/api/portal/currency'),
    // TLS-certificate management (certificates feature). certificatesEnabled is
    // portal-scoped (any authenticated user, gateway:use) and returns ONLY the
    // raw module_enabled checkbox -- no config leak -- so it gates the nav item
    // / view the same way netbirdEnabled's module_enabled does.
    certificatesEnabled: () =>
      request<{ module_enabled: boolean }>(fetcher, '/api/portal/certificates/enabled'),
    // The full certificate inventory (system scope). Remaining validity is
    // computed client-side from not_after so it never goes stale. mesh is
    // optional so a rolling upgrade against an older backend stays typable.
    certificates: () =>
      request<{ data: CertificateRow[]; mesh?: CertificateMeshStatus }>(
        fetcher,
        '/api/system/certificates',
      ),
    // Trigger an immediate renewal attempt for one certificate by domain (system
    // scope). 404 certificate.not_found for an unknown domain.
    renewCertificate: (domain: string) =>
      request<{ ok: boolean }>(fetcher, '/api/system/certificates/renew', {
        method: 'POST',
        body: { domain },
      }),
    // The gateway's internal CA (self_signed issuer mode) + the distributed trust
    // bundle PEM (current root, plus the previous one during a rotation window).
    certificateCA: () =>
      request<{ ca: CertificateCA; bundle_pem: string }>(fetcher, '/api/system/certificates/ca'),
    // Rotate the internal CA: generates a new root; the previous one stays in the
    // bundle until it expires. 400 in acme mode (there is no internal CA to rotate).
    rotateCertificateCA: () =>
      request<{ ok: boolean }>(fetcher, '/api/system/certificates/ca/rotate', { method: 'POST' }),
    // Force an immediate re-issue pass over every managed certificate (e.g. after
    // switching the issuer, once clients trust the new one).
    reissueAllCertificates: () =>
      request<{ ok: boolean }>(fetcher, '/api/system/certificates/reissue-all', { method: 'POST' }),
    // Per-server certificate-management opt-in/opt-out override ("" / "include" /
    // "exclude", mirrors setServerNetbird's policy override). Returns the updated
    // server DTO.
    setServerCertificateOverride: (id: string, override: string) =>
      request<PortalServer>(fetcher, `/api/portal/servers/${encodeURIComponent(id)}/certificate`, {
        method: 'PUT',
        body: { certificate_override: override },
      }),
    // Per-server https-auto-switch opt-in/opt-out override ("" / "include" /
    // "exclude", mirrors setServerCertificateOverride). Returns the updated
    // server DTO.
    setServerHTTPSSwitchOverride: (id: string, override: string) =>
      request<PortalServer>(fetcher, `/api/portal/servers/${encodeURIComponent(id)}/https-switch`, {
        method: 'PUT',
        body: { https_switch_override: override },
      }),
    // The gateway's OWN edge (nginx) certificate: its configuration, its stored
    // row and its delivery state (system scope, GET-only, read-only).
    edgeCertificate: () => request<EdgeCertificate>(fetcher, '/api/system/certificates/edge'),
    // Marks ONLY the edge row due so the next reconcile pass re-issues it with the
    // currently configured edge issuer mode. 404 certificate.not_found when
    // nothing has been issued yet.
    reissueEdgeCertificate: () =>
      request<{ ok: boolean }>(fetcher, '/api/system/certificates/edge/reissue', {
        method: 'POST',
      }),
    // The three text/PEM endpoints below deliberately do NOT go through
    // request<T> (which expects JSON): mirrors downloadAgentBinary -- a bare
    // fetcher call, credentials included, and on !response.ok a PortalApiError
    // carrying the JSON error body's CODE, so the caller can tell e.g. the 409
    // certificate.edge_key_managed apart from a generic failure.
    //
    // The edge certificate's PUBLIC material (full chain, plus the internal root
    // in self_signed mode) -- no key, so no extra gate beyond the system scope.
    edgeCertificateBundle: () => requestText(fetcher, '/api/system/certificates/edge/bundle'),
    // The edge certificate's PRIVATE KEY. 409 certificate.edge_key_managed when
    // the gateway can deliver it to its own nginx itself (no download then
    // exists); 400 system.cert_key_required when the stored key cannot be opened
    // because OP_AI_GATEWAY_CERT_ENCRYPTION_KEY is missing/wrong.
    edgeCertificateKey: () => requestText(fetcher, '/api/system/certificates/edge/key'),
    // The public-domain certificate's material (P3 Task 9 endpoints) -- same
    // bare-fetcher/PortalApiError shape as the edge bundle/key above. domain
    // must be one of the currently configured cert_public_domains; the key
    // response carries Cache-Control: no-store (mirrors the edge key).
    publicCertificateBundle: (domain: string) =>
      requestText(fetcher, `/api/system/certificates/public/${encodeURIComponent(domain)}/bundle`),
    publicCertificateKey: (domain: string) =>
      requestText(fetcher, `/api/system/certificates/public/${encodeURIComponent(domain)}/key`),
    // The generated nginx configuration for the reverse proxy IN FRONT OF the
    // gateway -- a point-in-time snapshot of stored settings (plus one cached,
    // best-effort live name resolution); re-generating later can differ.
    edgeProxyConfig: () => requestText(fetcher, '/api/system/certificates/edge/proxy-config'),
    // The synthetic TLS self-probe (system scope, POST-only -- an active network
    // action). 409 certificate.edge_probe_not_configured when
    // OP_AI_GATEWAY_CERT_EDGE_PROBE_TARGET is unset -- the gateway cannot reach
    // its own nginx's :443 listener by itself in either bundled topology.
    probeEdgeTLS: () =>
      request<EdgeTLSProbeResult>(fetcher, '/api/system/certificates/edge/probe', {
        method: 'POST',
      }),
    getPublicTheme: () =>
      request<{ theme: string; source: 'builtin' | 'external'; data: ExternalThemeData | null }>(
        fetcher,
        '/api/system/theme',
      ),
  };
}
