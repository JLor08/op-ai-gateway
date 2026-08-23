// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/netbird"
	"op-ai-gateway/internal/ping"
	"op-ai-gateway/internal/portal"
	"strings"
	"time"
)

// pingTimeout bounds the unprivileged ICMP echo the ping action runs against a
// selected server's domain. A slow/unreachable host simply yields {ok:false} at
// HTTP 200 within this window (never blocks the panel).
const pingTimeout = 5 * time.Second

// netbirdStatusPeerTimeout bounds the best-effort GetPeer call the status
// endpoint makes to report the gateway peer's connection state. It is short
// because the status panel must stay snappy; a slow/unreachable admin API simply
// yields gateway_peer_connected=false (no leak).
const netbirdStatusPeerTimeout = 5 * time.Second

// handleSystemNetbirdTest verifies the NetBird settings by pinging the admin
// API. With no request body it tests the SAVED settings (save first); with an
// optional JSON body `{url?, token?}` it tests those values instead (each
// falling back to the stored value when omitted/null), so the operator can
// verify unsaved credentials before saving them. It mirrors handleSystemSMTPTest:
// the response is {ok, error?} at HTTP 200 — it never echoes the token, and a
// failure (not configured, auth, transport) is reported as {ok:false, error} so
// the frontend can show a success/error toast.
func (s *Server) handleSystemNetbirdTest(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	token, ok := s.requireWebScope(w, r, "system")
	if !ok {
		return
	}
	var override *portal.NetbirdTestOverride
	if r.ContentLength != 0 {
		raw, ok := readRawJSON(w, r)
		if !ok {
			return
		}
		if len(strings.TrimSpace(string(raw))) > 0 {
			var body struct {
				URL   *string `json:"url"`
				Token *string `json:"token"`
			}
			if err := json.Unmarshal(raw, &body); err != nil {
				writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
				return
			}
			override = &portal.NetbirdTestOverride{URL: body.URL, Token: body.Token}
		}
	}
	if err := s.Portal.TestNetbird(r.Context(), token, override); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleSystemNetbirdNetwork reads (GET) or writes (PUT) the NetBird account's
// network settings (dns_domain / network_range / network_range_v6 /
// ipv6_enabled_groups). System scope. The admin token is never echoed. On error
// it reuses the shared netbird-error→HTTP mapping (module-disabled → 409,
// range-invalid → 400, netbird.ErrAuth → 502, else 500).
func (s *Server) handleSystemNetbirdNetwork(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireWebScope(w, r, "system")
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		dto, err := s.Portal.NetbirdNetwork(r.Context())
		if err != nil {
			writePortalServerError(w, err, "netbird.network_failed")
			return
		}
		writeJSON(w, http.StatusOK, dto)
	case http.MethodPut:
		raw, ok := readRawJSON(w, r)
		if !ok {
			return
		}
		var req portal.NetbirdNetworkDTO
		if err := json.Unmarshal(raw, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
			return
		}
		dto, err := s.Portal.SetNetbirdNetwork(r.Context(), token, req)
		if err != nil {
			writePortalServerError(w, err, "netbird.network_failed")
			return
		}
		writeJSON(w, http.StatusOK, dto)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, apierror.Response(codeRequestMethodNotAllowed, msgMethodNotAllowed, ""))
	}
}

// handleSystemNetbirdGatewaySetupKey mints a one-off NetBird setup key for the
// GATEWAY's own peer (system scope, POST-only). The response carries the key + the
// ready-to-paste `netbird up` console command (display-once); the admin token is
// never echoed. On error it reuses the shared netbird-error→HTTP mapping
// (module-disabled → 409, netbird.ErrAuth → 502, else 500).
func (s *Server) handleSystemNetbirdGatewaySetupKey(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	token, ok := s.requireWebScope(w, r, "system")
	if !ok {
		return
	}
	key, command, err := s.Portal.CreateGatewaySetupKey(r.Context(), token)
	if err != nil {
		writePortalServerError(w, err, "netbird.setup_key_failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"setup_key": key, "netbird_setup_command": command})
}

// handleSystemNetbirdEnrollSidecar mints a one-off NetBird gateway setup key AND
// writes it to the configured shared-volume key file so a waiting NetBird sidecar
// can self-enroll (system scope, POST-only). The response carries the key + the
// ready-to-paste `netbird up` console command as a display-once fallback; the admin
// token is never echoed. On error it reuses the shared netbird-error→HTTP mapping
// (key-file-not-configured / module-disabled → 409, netbird.ErrAuth → 502, else 500).
func (s *Server) handleSystemNetbirdEnrollSidecar(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	token, ok := s.requireWebScope(w, r, "system")
	if !ok {
		return
	}
	key, command, err := s.Portal.EnrollGatewaySidecar(r.Context(), token)
	if err != nil {
		writePortalServerError(w, err, "netbird.enroll_sidecar_failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"setup_key": key, "netbird_setup_command": command})
}

// netbirdGroupDTO is the safe subset of a NetBird group exposed to the settings
// group picker: id + name only (never the token or peer list).
type netbirdGroupDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// handleSystemNetbirdGroups lists the NetBird groups (id + name) for the
// settings group picker. System scope, GET-only. On error it reuses the shared
// netbird-error→HTTP mapping (module-disabled → 409, netbird.ErrAuth → 502, else
// 500) — the same mapping the regenerate/setup-key path uses. The admin token is
// never echoed.
func (s *Server) handleSystemNetbirdGroups(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireWebScope(w, r, "system"); !ok {
		return
	}
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	groups, err := s.Portal.NetbirdGroups(r.Context())
	if err != nil {
		writePortalServerError(w, err, "netbird.groups_failed")
		return
	}
	out := make([]netbirdGroupDTO, 0, len(groups))
	for _, g := range groups {
		out = append(out, netbirdGroupDTO{ID: g.ID, Name: g.Name})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

// netbirdPeerDTO is the safe subset of a NetBird peer exposed to the
// linkage-editor peer picker: id + name + dns_label + connected only. It never
// leaks the ssh/expiration internals (or the token).
type netbirdPeerDTO struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	DNSLabel  string `json:"dns_label"`
	Connected bool   `json:"connected"`
}

// handleSystemNetbirdPeers lists the NetBird peers (id/name/dns_label/connected)
// for the linkage-editor peer picker. System scope, GET-only. On error it reuses
// the shared netbird-error→HTTP mapping (module-disabled → 409, netbird.ErrAuth →
// 502, else 500). The admin token is never echoed; the ssh/expiration internals
// are never exposed.
func (s *Server) handleSystemNetbirdPeers(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireWebScope(w, r, "system"); !ok {
		return
	}
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	peers, err := s.Portal.NetbirdPeers(r.Context())
	if err != nil {
		writePortalServerError(w, err, "netbird.peers_failed")
		return
	}
	out := make([]netbirdPeerDTO, 0, len(peers))
	for _, p := range peers {
		out = append(out, netbirdPeerDTO{ID: p.ID, Name: p.Name, DNSLabel: p.DNSLabel, Connected: p.Connected})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

// handlePortalNetbirdEnabled reports whether the NetBird module is enabled, for
// any portal user (gateway:use scope, GET-only). It returns ONLY the boolean —
// never the url, token, or group — so a non-system-admin's UI can show/hide the
// NetBird actions without reading system settings.
func (s *Server) handlePortalNetbirdEnabled(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireWebScope(w, r, scopeGatewayUse); !ok {
		return
	}
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	pc := s.Portal.NetbirdPolicyContext(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":                s.Portal.NetbirdModuleEnabled(r.Context()),
		"module_enabled":         s.Portal.NetbirdModuleChecked(r.Context()),
		"netbird_only":           s.Portal.NetbirdOnly(r.Context()),
		"manage_policies":        pc.ManagePolicies,
		"effective_policy_scope": pc.EffectivePolicyScope,
		"deny_by_default":        pc.DenyByDefault,
	})
}

// handleSystemServerNetbird is the system-admin NetBird linkage editor:
// PUT /api/system/servers/{id}/netbird sets a server's netbird enabled flag +
// peer id (to link a manually-created NetBird peer to an existing server).
// System scope, PUT-only; a path that is not {id}/netbird → 404. It does not
// call NetBird — the sync loop reconciles from the new peer id on its next tick.
// Errors reuse the shared server-error mapping: unknown id → 404 (no leak),
// disable-on-domainless → 400.
func (s *Server) handleSystemServerNetbird(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireWebScope(w, r, "system")
	if !ok {
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/system/servers/"), "/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[0] == "" {
		writeJSON(w, http.StatusNotFound, apierror.Response(portal.CodeServerNotFound, msgServerNotFound, ""))
		return
	}
	if parts[1] != "netbird" {
		writeJSON(w, http.StatusNotFound, apierror.Response(portal.CodeServerNotFound, msgServerNotFound, ""))
		return
	}
	if !requireMethod(w, r, http.MethodPut) {
		return
	}
	raw, ok := readRawJSON(w, r)
	if !ok {
		return
	}
	var body struct {
		NetbirdEnabled        bool     `json:"netbird_enabled"`
		NetbirdPeerID         string   `json:"netbird_peer_id"`
		NetbirdGroupIDs       []string `json:"netbird_group_ids"`
		NetbirdPeerManaged    bool     `json:"netbird_peer_managed"`
		NetbirdPolicyOverride string   `json:"netbird_policy_override"`
		NetbirdAllowPing      bool     `json:"netbird_allow_ping"`
		NetbirdPingExclude    bool     `json:"netbird_ping_exclude"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
		return
	}
	dto, err := s.Portal.SetServerNetbird(r.Context(), token, parts[0], body.NetbirdEnabled, body.NetbirdPeerID, body.NetbirdGroupIDs, body.NetbirdPeerManaged, body.NetbirdPolicyOverride, body.NetbirdAllowPing, body.NetbirdPingExclude)
	if err != nil {
		writePortalServerError(w, err, "server.update_failed")
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// handlePortalServerPing runs an unprivileged ICMP echo from the gateway to a
// server's domain: POST /api/portal/servers/{id}/ping (gateway:use, owner-or-admin,
// POST-only). Ownership is enforced via Portal.GetServer (unknown / not-visible id →
// 404 no-leak). Reports {ok, latency_ms} / {ok:false, error} at HTTP 200 — a missing
// domain, an ICMP-unavailable environment, or an unreachable host are non-fatal.
func (s *Server) handlePortalServerPing(w http.ResponseWriter, r *http.Request, token auth.Token, id string) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	server, err := s.Portal.GetServer(r.Context(), token, id)
	if err != nil {
		writePortalServerError(w, err, portal.CodeServerNotFound)
		return
	}
	if server.Domain == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "server has no domain to ping"})
		return
	}
	rtt, perr := ping.Host(r.Context(), server.Domain, pingTimeout)
	if perr != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": perr.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "latency_ms": rtt.Milliseconds()})
}

// systemNetbirdStatusDTO reports the NetBird-only transport state to the
// System-Settings status panel: whether the agent listener bound (deployment
// capability), its address, the runtime netbird_only switch, the selected
// gateway peer, and whether that peer looks connected. It never carries the
// token or any peer internals.
type systemNetbirdStatusDTO struct {
	AgentListenerActive  bool   `json:"agent_listener_active"`
	AgentListenerAddr    string `json:"agent_listener_addr"`
	NetbirdOnly          bool   `json:"netbird_only"`
	GatewayPeerID        string `json:"gateway_peer_id"`
	GatewayPeerConnected bool   `json:"gateway_peer_connected"`
	// GatewayPeerName is the peer's CURRENT (live) NetBird name, from the same
	// best-effort GetPeer used for GatewayPeerConnected. Empty when there is no
	// peer id or the lookup fails — never a new NetBird call, never the token.
	GatewayPeerName string `json:"gateway_peer_name"`
	// SidecarEnrollAvailable is true when a shared setup-key file is configured
	// (OP_AI_GATEWAY_NETBIRD_KEY_FILE) — i.e. an autonomous-enroll sidecar is wired.
	// The frontend only shows the "Sidecar enrollen" button when this is true.
	SidecarEnrollAvailable bool `json:"sidecar_enroll_available"`

	// Policy-management state (T4). ManagePolicies + the two scope fields + the two
	// deny-by-default flags come from the persisted system settings. The three
	// "count/present/enabled" fields + DenyByDefaultDrift are computed on-demand from
	// ONE best-effort ListPolicies (all zero/false on any NetBird error — the panel
	// never blocks and the token is never leaked).
	ManagePolicies       bool   `json:"manage_policies"`
	PolicyScope          string `json:"policy_scope"`
	EffectivePolicyScope string `json:"effective_policy_scope"`
	DenyByDefault        bool   `json:"deny_by_default"`
	DenyByDefaultEnforce bool   `json:"deny_by_default_enforce"`
	ManagedPolicyCount   int    `json:"managed_policy_count"`
	DefaultPolicyPresent bool   `json:"default_policy_present"`
	DefaultPolicyEnabled bool   `json:"default_policy_enabled"`
	DenyByDefaultDrift   bool   `json:"deny_by_default_drift"`
}

// handleSystemNetbirdStatus reports the NetBird-only transport status (system
// scope, GET-only). agent_listener_active/addr come from the fields main sets at
// startup; netbird_only + gateway_peer_id from the persisted settings;
// gateway_peer_connected is a BEST-EFFORT GetPeer against the selected peer
// (module off / empty id / any error → false, so the panel never blocks and the
// token is never leaked).
func (s *Server) handleSystemNetbirdStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireWebScope(w, r, "system"); !ok {
		return
	}
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	ctx := r.Context()
	peerID := s.Portal.NetbirdGatewayPeerID(ctx)
	connected := false
	peerName := ""
	// Best-effort GetPeer for the gateway peer's connection state + a single
	// ListPolicies for the policy-management panel (managed count + Default-policy
	// drift). Both reuse the stored config; any error leaves the derived fields
	// zero/false and never blocks the panel or leaks the token.
	managedCount := 0
	defaultPresent := false
	defaultEnabled := false
	if cfg, ok, err := s.Portal.NetbirdConfig(ctx); err == nil && ok {
		ncfg := netbird.Config{URL: cfg.URL, Token: cfg.Token}
		if peerID != "" {
			if peer, err := netbird.GetPeer(ctx, ncfg, netbirdStatusPeerTimeout, peerID); err == nil {
				connected = peer.Connected
				peerName = peer.Name
			}
		}
		if policies, err := netbird.ListPolicies(ctx, ncfg, netbirdStatusPeerTimeout); err == nil {
			for _, p := range policies {
				if strings.HasPrefix(p.Name, portal.NetbirdManagedPolicyPrefix) {
					managedCount++
				}
				if p.Name == portal.NetbirdDefaultPolicyName {
					defaultPresent = true
					defaultEnabled = p.Enabled
				}
			}
		}
	}
	settings := s.Portal.SystemSettingsView(ctx)
	deny := settings.NetbirdDenyByDefault
	writeJSON(w, http.StatusOK, systemNetbirdStatusDTO{
		AgentListenerActive:    s.AgentListenerActive(),
		AgentListenerAddr:      s.AgentListenerAddr(),
		NetbirdOnly:            s.Portal.NetbirdOnly(ctx),
		GatewayPeerID:          peerID,
		GatewayPeerConnected:   connected,
		GatewayPeerName:        peerName,
		SidecarEnrollAvailable: s.Portal.NetbirdKeyFileConfigured(),
		ManagePolicies:         settings.NetbirdManagePolicies,
		PolicyScope:            settings.NetbirdPolicyScope,
		EffectivePolicyScope:   settings.NetbirdEffectivePolicyScope,
		DenyByDefault:          deny,
		DenyByDefaultEnforce:   settings.NetbirdDenyByDefaultEnforce,
		ManagedPolicyCount:     managedCount,
		DefaultPolicyPresent:   defaultPresent,
		DefaultPolicyEnabled:   defaultEnabled,
		DenyByDefaultDrift:     deny && defaultPresent && defaultEnabled,
	})
}

// handleSystemNetbirdTokenStatus reports the current admin API token's live
// validity for display (system scope, GET-only). It never errors on an
// unconfigured module (the service returns known:false at 200); it never leaks
// the token value.
func (s *Server) handleSystemNetbirdTokenStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireWebScope(w, r, "system"); !ok {
		return
	}
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	dto, err := s.Portal.NetbirdTokenStatus(r.Context())
	if err != nil {
		writePortalServerError(w, err, "netbird.token_status_failed")
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// handleSystemNetbirdRotateToken creates a fresh NetBird admin API token,
// verifies it, switches the stored config to it, and best-effort deletes the
// old one — rolling back (deleting the orphan new token, keeping the old one
// active) on any failure (system scope, POST-only). Errors reuse the shared
// netbird-error mapping (module-disabled -> 409, netbird.ErrAuth -> 502). The
// new token value is never echoed.
func (s *Server) handleSystemNetbirdRotateToken(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	token, ok := s.requireWebScope(w, r, "system")
	if !ok {
		return
	}
	res, err := s.Portal.RotateNetbirdToken(r.Context(), token)
	if err != nil {
		writePortalServerError(w, err, "netbird.token_rotate_failed")
		return
	}
	writeJSON(w, http.StatusOK, res)
}
