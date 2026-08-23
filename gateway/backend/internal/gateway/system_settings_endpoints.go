// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
)

// settingsPortal is the narrow slice of portal.API this file's handlers
// actually call, enumerated by grepping every s.Portal. call site here.
// Declaring it here documents the group's true portal dependency and
// compile-checks it independently of portal.API's other 190+ methods;
// portal.API satisfies it structurally, so no production wiring changes.
// See settingsPortal() below.
type settingsPortal interface {
	PublicThemeView(context.Context) portal.ThemePublicView
	SystemSettingsView(context.Context) portal.SystemSettingsDTO
	CertMeshRequireTLSChecked(context.Context) bool
	UpdateSystemSettings(context.Context, auth.Token, portal.UpdateSystemSettingsRequest) (portal.SystemSettingsDTO, error)
}

// settingsPortal returns s.Portal narrowed to the system-settings group's
// portal surface. s.Portal itself stays a portal.API (ServerDeps/Server are
// unchanged) — this accessor is purely a compile-time documentation/check
// boundary for the call sites in this file.
func (s *Server) settingsPortal() settingsPortal {
	return s.Portal
}

func (s *Server) handleSystemTheme(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	writeJSON(w, http.StatusOK, s.settingsPortal().PublicThemeView(r.Context()))
}

func (s *Server) handleSystemSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if _, ok := s.requireWebScope(w, r, "system"); !ok {
			return
		}
		writeJSON(w, http.StatusOK, s.settingsPortal().SystemSettingsView(r.Context()))
	case http.MethodPut:
		token, ok := s.requireWebScope(w, r, "system")
		if !ok {
			return
		}
		raw, ok := readRawJSON(w, r)
		if !ok {
			return
		}
		var req portal.UpdateSystemSettingsRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
			return
		}
		// The arming precondition for the plaintext gate: switching
		// cert_edge_require_https ON is only allowed against observed encrypted
		// traffic AND over an encrypted hop of its own, so an operator can neither
		// arm a total lockout before the fronting proxy's TLS listener has ever
		// worked, nor arm one against somebody else's TLS while their own route
		// stays plaintext. Checked BEFORE the write, so a refusal stores nothing.
		// Turning it OFF is never gated -- disarming must always be possible.
		if req.CertEdgeRequireHTTPS != nil && *req.CertEdgeRequireHTTPS {
			if err := s.ArmEdgeRequireHTTPS(r); err != nil {
				if errors.Is(err, errEdgeArmHopPlaintext) {
					writeJSON(w, http.StatusBadRequest, apierror.Response("certificate.edge_arm_requires_https",
						"this request itself reached the gateway unencrypted; arm the gate over https so the route you use cannot lock you out", ""))
					return
				}
				writeJSON(w, http.StatusBadRequest, apierror.Response("certificate.edge_https_not_observed",
					"no encrypted request has reached this gateway recently; verify the reverse proxy terminates TLS before requiring https", ""))
				return
			}
		}
		// The mesh gate's arming precondition: switching cert_mesh_require_tls ON is
		// only allowed once at least one ServerAgent has authenticated over TLS on the
		// mesh listener recently, so an operator cannot arm a fleet-wide lockout before
		// any agent has proven it can speak TLS. Checked BEFORE the write, so a refusal
		// stores nothing. Turning it OFF is never gated. Unlike the edge gate there is
		// no own-hop check -- the operator always arms from the portal, never over the
		// mesh listener.
		// Only the false->true TRANSITION is gated: a PUT that re-sets an
		// already-armed switch is an idempotent no-op and must not re-run the
		// observation precondition (which would 400 an unchanged value once the
		// in-memory observation lapses). Reading the stored value is why this key is
		// documented as "send it isolated".
		if req.CertMeshRequireTLS != nil && *req.CertMeshRequireTLS && !s.settingsPortal().CertMeshRequireTLSChecked(r.Context()) {
			if err := s.ArmMeshRequireTLS(); err != nil {
				writeJSON(w, http.StatusBadRequest, apierror.Response("certificate.mesh_tls_not_observed",
					"no ServerAgent has connected over TLS to the mesh listener recently; upgrade at least one agent to https before requiring TLS", ""))
				return
			}
		}
		dto, err := s.settingsPortal().UpdateSystemSettings(r.Context(), token, req)
		if err != nil {
			if errors.Is(err, portal.ErrPrincipalForbidden) {
				writeJSON(w, http.StatusForbidden, apierror.Response(portal.CodePrincipalForbidden, notAllowedMsg, ""))
				return
			}
			if errors.Is(err, portal.ErrThemeInvalid) {
				writeJSON(w, http.StatusBadRequest, apierror.Response("system.theme_invalid", "unknown theme", ""))
				return
			}
			if errors.Is(err, portal.ErrLanguageInvalid) {
				writeJSON(w, http.StatusBadRequest, apierror.Response("system.language_invalid", "unknown language", ""))
				return
			}
			if errors.Is(err, portal.ErrRetentionInvalid) {
				writeJSON(w, http.StatusBadRequest, apierror.Response("system.retention_invalid", "capture retention must be 1-365 days", ""))
				return
			}
			if errors.Is(err, portal.ErrHealthCheckIntervalInvalid) {
				writeJSON(w, http.StatusBadRequest, apierror.Response("system.health_check_interval_invalid", "health check interval must be 5-3600 seconds", ""))
				return
			}
			if errors.Is(err, portal.ErrAgentPresenceTimeoutInvalid) {
				writeJSON(w, http.StatusBadRequest, apierror.Response("system.agent_presence_timeout_invalid", "agent presence timeout must be 3-3600 seconds", ""))
				return
			}
			if errors.Is(err, portal.ErrTotpModeInvalid) {
				writeJSON(w, http.StatusBadRequest, apierror.Response("system.totp_mode_invalid", "totp mode must be off, optional or required", ""))
				return
			}
			if errors.Is(err, portal.ErrVisionProbeModeInvalid) {
				writeJSON(w, http.StatusBadRequest, apierror.Response("system.vision_probe_mode_invalid", "vision probe mode must be accept or verify", ""))
				return
			}
			if errors.Is(err, portal.ErrRouteAffinitySessionModeInvalid) {
				writeJSON(w, http.StatusBadRequest, apierror.Response("system.route_affinity_session_mode_invalid", "route affinity session mode must be client_session or legacy_header", ""))
				return
			}
			if errors.Is(err, portal.ErrEnergyDefaultInvalid) {
				writeJSON(w, http.StatusBadRequest, apierror.Response("system.energy_default_invalid", "energy defaults must be non-negative", ""))
				return
			}
			if errors.Is(err, portal.ErrSMTPConfigIncomplete) {
				writeJSON(w, http.StatusBadRequest, apierror.Response("system.smtp_config_incomplete", "smtp host, port and from are required when enabling", ""))
				return
			}
			if errors.Is(err, portal.ErrSMTPFromInvalid) {
				writeJSON(w, http.StatusBadRequest, apierror.Response("system.smtp_from_invalid", "smtp from address is invalid", ""))
				return
			}
			if errors.Is(err, portal.ErrSMTPPortInvalid) {
				writeJSON(w, http.StatusBadRequest, apierror.Response("system.smtp_port_invalid", "smtp port must be 1-65535", ""))
				return
			}
			if errors.Is(err, portal.ErrSMTPTLSModeInvalid) {
				writeJSON(w, http.StatusBadRequest, apierror.Response("system.smtp_tls_mode_invalid", "smtp tls mode must be starttls, ssl or none", ""))
				return
			}
			if errors.Is(err, portal.ErrSMTPKeyRequired) {
				writeJSON(w, http.StatusBadRequest, apierror.Response("system.smtp_key_required", "an encryption key is required to store an smtp password on a disk-backed store", ""))
				return
			}
			if errors.Is(err, portal.ErrNetbirdConfigIncomplete) {
				writeJSON(w, http.StatusBadRequest, apierror.Response("system.netbird_config_incomplete", "netbird url and token are required when enabling", ""))
				return
			}
			if errors.Is(err, portal.ErrNetbirdURLInvalid) {
				writeJSON(w, http.StatusBadRequest, apierror.Response("system.netbird_url_invalid", "netbird url must be a valid absolute http(s) url", ""))
				return
			}
			if errors.Is(err, portal.ErrNetbirdKeyRequired) {
				writeJSON(w, http.StatusBadRequest, apierror.Response("system.netbird_key_required", "an encryption key is required to store a netbird token on a disk-backed store", ""))
				return
			}
			if errors.Is(err, portal.ErrNetbirdIntervalOrder) {
				writeJSON(w, http.StatusBadRequest, apierror.Response("system.netbird_interval_order", "netbird peer-sync interval must be <= the reconcile interval (both >= 10s)", ""))
				return
			}
			if errors.Is(err, portal.ErrNetbirdTokenRotateBeforeInvalid) {
				writeJSON(w, http.StatusBadRequest, apierror.Response("system.netbird_token_rotate_before_invalid", "netbird token rotate-before-days must be >= 0", ""))
				return
			}
			if errors.Is(err, portal.ErrCertInvalid) {
				writeJSON(w, http.StatusBadRequest, apierror.Response("cert.invalid", "invalid certificate settings", ""))
				return
			}
			writeJSON(w, http.StatusInternalServerError, apierror.Response("system.settings_update_failed", "settings update failed", ""))
			return
		}
		// Live-apply the affinity session-mode to the resolver so a save takes
		// effect without a restart (read straight off the returned DTO).
		if s.Resolver != nil {
			s.Resolver.SetAffinitySessionMode(dto.RouteAffinitySessionMode == "legacy_header")
		}
		// Drop the plaintext gate's cached switch read so the change takes effect on
		// the very next request rather than after edgeSchemeSwitchTTL -- which
		// matters most in the DISARMING direction, where the operator is trying to
		// get back in.
		if req.CertEdgeRequireHTTPS != nil {
			s.invalidateEdgeRequireHTTPSCache()
		}
		// Same reasoning for the mesh gate's cached switch.
		if req.CertMeshRequireTLS != nil {
			s.invalidateMeshRequireTLSCache()
		}
		writeJSON(w, http.StatusOK, dto)
	default:
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPut)
		writeJSON(w, http.StatusMethodNotAllowed, apierror.Response(codeRequestMethodNotAllowed, msgMethodNotAllowed, ""))
	}
}
