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
	"sort"
	"strings"
	"time"
)

// codeCertInvalid is the shared code for the certificate-override validation
// errors below; each call site supplies its own specific message.
const codeCertInvalid = "cert.invalid"

// notAllowedMsg is the generic 403 message paired with
// portal.CodePrincipalForbidden for the pre-emptive checks below (mirrors
// sharedErrorMap's ErrPrincipalForbidden row in error_map.go).
const notAllowedMsg = "not allowed"

// certPortal is the narrow slice of portal.API the certificates group
// (this file + public_certificates.go) actually calls, enumerated by
// grepping every s.Portal. call site in both files. Declaring it here
// documents the group's true portal dependency and compile-checks it
// independently of portal.API's other 190+ methods; portal.API satisfies it
// structurally, so no production wiring changes. See certPortal() below.
type certPortal interface {
	CertModuleChecked(context.Context) bool
	CertificatesView(context.Context) ([]portal.CertificateDTO, error)
	GatewayCARotationPendingServers(context.Context) []portal.CertificateServerRefDTO
	CertMeshRequireTLSChecked(context.Context) bool
	MeshTLSPendingServers(context.Context) []portal.CertificateServerRefDTO
	HTTPSSwitchUnreachableApps(context.Context) []portal.HTTPSSwitchUnreachableDTO
	RenewCertificateNow(context.Context, auth.Token, string) error
	SetServerCertificateOverride(context.Context, auth.Token, string, string) (portal.ServerDTO, error)
	SetServerHTTPSSwitchOverride(context.Context, auth.Token, string, string) (portal.ServerDTO, error)
	CertificateCAView(context.Context) (portal.CertificateCADTO, error)
	CertificateCABundlePEM(context.Context) (string, error)
	ReissueAllCertificates(context.Context, auth.Token) error
	RotateCertificateCA(context.Context, auth.Token) error
	PublicCertificateBundlePEM(context.Context, string) (string, error)
	PublicCertificateKeyPEM(context.Context, string) (string, error)
}

// certPortal returns s.Portal narrowed to the certificates group's portal
// surface. s.Portal itself stays a portal.API (ServerDeps/Server are
// unchanged) — this accessor is purely a compile-time documentation/check
// boundary for the call sites in this file and public_certificates.go.
func (s *Server) certPortal() certPortal {
	return s.Portal
}

type certificateMeshStatus struct {
	TLSActive                bool                             `json:"tls_active"`
	Address                  string                           `json:"address,omitempty"`
	Fingerprint              string                           `json:"fingerprint,omitempty"`
	NotAfter                 *time.Time                       `json:"not_after,omitempty"`
	CARotationPendingServers []portal.CertificateServerRefDTO `json:"ca_rotation_pending_servers"`
	// RequireTLS is the stored cert_mesh_require_tls switch; TLSObserved is whether
	// a fresh-enough TLS hop exists to ARM it (the frontend disables the toggle
	// until then); TLSPendingServers names every token-server the switch would lock
	// out (latest mesh hop not TLS), for the arming confirm dialog. See spec §6.
	RequireTLS        bool                             `json:"require_tls"`
	TLSObserved       bool                             `json:"tls_observed"`
	TLSPendingServers []portal.CertificateServerRefDTO `json:"tls_pending_servers"`
}

func normalizeCertificateServerRefs(refs []portal.CertificateServerRefDTO) []portal.CertificateServerRefDTO {
	byID := make(map[string]portal.CertificateServerRefDTO, len(refs))
	for _, ref := range refs {
		if old, ok := byID[ref.ID]; !ok || ref.Name < old.Name {
			byID[ref.ID] = ref
		}
	}
	out := make([]portal.CertificateServerRefDTO, 0, len(byID))
	for _, ref := range byID {
		out = append(out, ref)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name == out[j].Name {
			return out[i].ID < out[j].ID
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// handleACMEChallenge answers the ACME HTTP-01 challenge on the PUBLIC listener.
// It is deliberately unauthenticated (Let's Encrypt validates anonymously) and
// deliberately NOT gated by netbird_only: public reachability over the plain
// internet is this feature's precondition, so the challenge path must stay
// reachable regardless of the NetBird-only transport toggle. It can only ever
// answer a token belonging to the gateway's OWN in-flight orders (a process-local
// map, no filesystem access, no traversal possible) -- an unknown, empty, or
// path-shaped token is a plain 404, never a 500 or a filesystem probe.
func (s *Server) handleACMEChallenge(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if s.acmeChallenges == nil {
		http.NotFound(w, r)
		return
	}
	token := strings.TrimPrefix(r.URL.Path, "/.well-known/acme-challenge/")
	if token == "" || strings.Contains(token, "/") {
		http.NotFound(w, r)
		return
	}
	keyAuth, ok := s.acmeChallenges.Get(token)
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(keyAuth))
}

// handlePortalCertificatesEnabled reports whether the certificate module's
// checkbox is on, for any portal user (gateway:use, GET-only). It returns ONLY
// that boolean -- not url/email/mode -- so a non-system-admin's shell can gate
// the nav item without needing (and without being granted) the system-scoped
// settings read.
func (s *Server) handlePortalCertificatesEnabled(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireWebScope(w, r, scopeGatewayUse); !ok {
		return
	}
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"module_enabled": s.certPortal().CertModuleChecked(r.Context())})
}

// handleSystemCertificates lists every managed certificate (system scope,
// GET-only). CertificatesView never returns a nil slice, so "data" is always a
// JSON array -- never null -- even with zero certificates.
func (s *Server) handleSystemCertificates(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireWebScope(w, r, "system"); !ok {
		return
	}
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	certs, err := s.certPortal().CertificatesView(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, apierror.Response("certificate.list_failed", "could not list certificates", ""))
		return
	}
	state := s.AgentListenerTLSState()
	mesh := certificateMeshStatus{
		TLSActive:                state.Active,
		CARotationPendingServers: normalizeCertificateServerRefs(s.certPortal().GatewayCARotationPendingServers(r.Context())),
		RequireTLS:               s.certPortal().CertMeshRequireTLSChecked(r.Context()),
		TLSObserved:              s.AgentTransport.AnyTLSWithin(time.Now(), meshGateObservationWindow),
		TLSPendingServers:        normalizeCertificateServerRefs(s.certPortal().MeshTLSPendingServers(r.Context())),
	}
	if state.Active {
		mesh.Address = state.Address
		mesh.Fingerprint = state.Fingerprint
		if !state.NotAfter.IsZero() {
			notAfter := state.NotAfter.UTC()
			mesh.NotAfter = &notAfter
		}
	}
	// https_switch is a SIBLING of mesh, not part of it: the https-auto-switch
	// is its own P4 feature (agent TLS proxy + scheme switch) and the portal
	// already renders it in its own box, independent of the agent-mesh-port
	// topology mesh describes. The list is empty in the overwhelmingly common
	// case; it is non-empty exactly when the gateway is refusing to downgrade
	// an application to plaintext and that application is therefore down.
	writeJSON(w, http.StatusOK, map[string]any{
		"data": certs,
		"mesh": mesh,
		"https_switch": map[string]any{
			"unreachable_apps": s.certPortal().HTTPSSwitchUnreachableApps(r.Context()),
		},
	})
}

// handleSystemCertificateRenew clears one domain's backoff so the next reconcile
// pass retries it immediately (system scope, POST-only, body {"domain":"..."}).
// An unknown domain is a 404 certificate.not_found -- it never silently no-ops.
func (s *Server) handleSystemCertificateRenew(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireWebScope(w, r, "system")
	if !ok {
		return
	}
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	raw, ok := readRawJSON(w, r)
	if !ok {
		return
	}
	var req struct {
		Domain string `json:"domain"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
		return
	}
	if err := s.certPortal().RenewCertificateNow(r.Context(), token, req.Domain); err != nil {
		if errors.Is(err, portal.ErrPrincipalForbidden) {
			writeJSON(w, http.StatusForbidden, apierror.Response(portal.CodePrincipalForbidden, notAllowedMsg, ""))
			return
		}
		if errors.Is(err, portal.ErrCertificateNotFound) {
			writeJSON(w, http.StatusNotFound, apierror.Response("certificate.not_found", "certificate not found", ""))
			return
		}
		writeJSON(w, http.StatusInternalServerError, apierror.Response("certificate.renew_failed", "could not schedule the renewal", ""))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handlePortalServerCertificate sets a server's certificate management
// opt-in/opt-out (PUT /api/portal/servers/{id}/certificate). The dispatcher
// (handlePortalServerItem) has already resolved and scope-checked token, so
// this handler does NOT re-authenticate -- it only enforces the method and
// validates/persists the body. Gated by the shared server-manage chokepoint
// inside the service (authorizeServer): an unknown or unauthorized server id
// yields the same 404 as a non-existent one (no existence leak), and an
// out-of-range override value is a 400, never a silent write.
func (s *Server) handlePortalServerCertificate(w http.ResponseWriter, r *http.Request, token auth.Token, serverID string) {
	if !requireMethod(w, r, http.MethodPut) {
		return
	}
	raw, ok := readRawJSON(w, r)
	if !ok {
		return
	}
	var req struct {
		CertificateOverride string `json:"certificate_override"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
		return
	}
	dto, err := s.certPortal().SetServerCertificateOverride(r.Context(), token, serverID, req.CertificateOverride)
	if err != nil {
		if errors.Is(err, portal.ErrCertInvalid) {
			writeJSON(w, http.StatusBadRequest, apierror.Response(codeCertInvalid, "invalid certificate override", ""))
			return
		}
		writePortalServerError(w, err, "server.update_failed")
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// handlePortalServerHTTPSSwitchOverride sets a server's https-auto-switch
// opt-in/opt-out (PUT /api/portal/servers/{id}/https-switch). Mirrors
// handlePortalServerCertificate exactly: the dispatcher (handlePortalServerItem)
// has already resolved and scope-checked token, so this handler does NOT
// re-authenticate -- it only enforces the method and validates/persists the
// body. Gated by the shared server-manage chokepoint inside the service
// (authorizeServer): an unknown or unauthorized server id yields the same 404
// as a non-existent one (no existence leak), and an out-of-range override
// value is a 400, never a silent write.
func (s *Server) handlePortalServerHTTPSSwitchOverride(w http.ResponseWriter, r *http.Request, token auth.Token, serverID string) {
	if !requireMethod(w, r, http.MethodPut) {
		return
	}
	raw, ok := readRawJSON(w, r)
	if !ok {
		return
	}
	var req struct {
		HTTPSSwitchOverride string `json:"https_switch_override"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, apierror.Response(codeRequestInvalidJSON, err.Error(), ""))
		return
	}
	dto, err := s.certPortal().SetServerHTTPSSwitchOverride(r.Context(), token, serverID, req.HTTPSSwitchOverride)
	if err != nil {
		if errors.Is(err, portal.ErrCertInvalid) {
			writeJSON(w, http.StatusBadRequest, apierror.Response(codeCertInvalid, "invalid https-switch override", ""))
			return
		}
		writePortalServerError(w, err, "server.update_failed")
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// handleSystemCertificateCA reports the internal CA and hands out the trust
// bundle (system scope, GET-only). PUBLIC certificates only -- the CA private
// key never leaves the process; CertificateCAView/CertificateCABundlePEM carry
// no key material by construction (a regression test in certificates_test.go
// pins this). When no internal CA exists yet (e.g. the issuer is "acme", or
// self_signed but no reconcile pass has run), bundle_pem is simply "".
func (s *Server) handleSystemCertificateCA(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireWebScope(w, r, "system"); !ok {
		return
	}
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	ca, err := s.certPortal().CertificateCAView(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, apierror.Response("certificate.ca_failed", "could not read the internal CA", ""))
		return
	}
	bundle := ""
	if ca.Present {
		if b, bErr := s.certPortal().CertificateCABundlePEM(r.Context()); bErr == nil {
			bundle = b
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ca": ca, "bundle_pem": bundle})
}

// handleSystemCertificateReissueAll marks every managed certificate due so the
// next reconcile passes re-issue them with the CURRENTLY configured issuer
// (system scope, POST-only). The reconcile itself deliberately never forces
// this on a mode switch -- this endpoint is the operator's explicit "switch the
// issuer now, the clients are ready" action.
func (s *Server) handleSystemCertificateReissueAll(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireWebScope(w, r, "system")
	if !ok {
		return
	}
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if err := s.certPortal().ReissueAllCertificates(r.Context(), token); err != nil {
		if errors.Is(err, portal.ErrPrincipalForbidden) {
			writeJSON(w, http.StatusForbidden, apierror.Response(portal.CodePrincipalForbidden, notAllowedMsg, ""))
			return
		}
		writeJSON(w, http.StatusInternalServerError, apierror.Response("certificate.reissue_failed", "could not mark the certificates for re-issue", ""))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleSystemCertificateCARotate forces an internal-CA rotation now (system
// scope, POST-only). Only meaningful in the self_signed issuer mode -- rotating
// a CA that ACME mode never uses would be a no-op that looks like success, so
// the service rejects it with ErrCertInvalid, mapped here to 400. The leaves are
// re-signed by the following reconcile passes (issuer-fingerprint mismatch), so
// the new root is published before anything depends on it. A running reconcile
// pass holding the CA lock maps to ErrCertReconcileInProgress -> 409 (review
// finding F1.2): RotateCertificateCA uses a non-blocking TryLock internally, so
// this request never just hangs for the duration of that pass.
func (s *Server) handleSystemCertificateCARotate(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireWebScope(w, r, "system")
	if !ok {
		return
	}
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if err := s.certPortal().RotateCertificateCA(r.Context(), token); err != nil {
		if errors.Is(err, portal.ErrPrincipalForbidden) {
			writeJSON(w, http.StatusForbidden, apierror.Response(portal.CodePrincipalForbidden, notAllowedMsg, ""))
			return
		}
		if errors.Is(err, portal.ErrCertInvalid) {
			writeJSON(w, http.StatusBadRequest, apierror.Response(codeCertInvalid, "the internal CA is only available in the self-signed issuer mode", ""))
			return
		}
		if errors.Is(err, portal.ErrCertReconcileInProgress) {
			writeJSON(w, http.StatusConflict, apierror.Response("certificate.reconcile_in_progress", "a certificate reconcile pass is currently running; retry in a moment", ""))
			return
		}
		// The new root is generated fine but cannot be sealed for storage: a
		// disk-backed store has no OP_AI_GATEWAY_CERT_ENCRYPTION_KEY. Naming the
		// variable turns an opaque 500 into an actionable 400 (the reconcile
		// surfaces the same cause in cert_last_error).
		if errors.Is(err, portal.ErrCertKeyRequired) {
			writeJSON(w, http.StatusBadRequest, apierror.Response("system.cert_key_required", "OP_AI_GATEWAY_CERT_ENCRYPTION_KEY must be set to store certificate private keys on a disk-backed store", ""))
			return
		}
		writeJSON(w, http.StatusInternalServerError, apierror.Response("certificate.ca_rotate_failed", "could not rotate the internal CA", ""))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
