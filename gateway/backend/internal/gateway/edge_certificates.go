// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"errors"
	"log/slog"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/portal"
	"time"
)

const (
	codeEdgeCertificateNotFound = "certificate.not_found"
	msgNoEdgeCertificateIssued  = "no edge certificate has been issued yet"

	// contentTypeHeader is the response header name set by the PEM/CA-bundle
	// download handlers below.
	contentTypeHeader = "Content-Type"
)

// handleSystemEdgeCertificate reports the state of the gateway's OWN edge (nginx)
// certificate (system scope, GET-only): its configuration, its timing, and how it
// reaches nginx. The DTO carries NO key material and NO PEM -- the same rule the
// other certificate DTOs follow.
//
// It ALSO merges in the plaintext-refusal gate's live state (require_https/
// https_observed/last_encrypted_at/last_plain_at) -- fields EdgeCertificateView
// cannot fill itself, because the arming precondition's observation tracker
// (s.edgeScheme) is in-process state that lives on *gateway.Server, not on
// portal.Service. This handler is the one place that holds both, so it is the
// one place that assembles the combined view the portal panel renders.
func (s *Server) handleSystemEdgeCertificate(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireWebScope(w, r, "system"); !ok {
		return
	}
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	dto, err := s.Portal.EdgeCertificateView(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, apierror.Response("certificate.edge_failed", "could not read the edge certificate", ""))
		return
	}
	dto.RequireHTTPS = s.Portal.CertEdgeRequireHTTPSChecked(r.Context())
	observed, lastEncrypted, lastPlain := s.edgeScheme.Seen(time.Now(), edgeSchemeObservationWindow)
	dto.HTTPSObserved = observed
	if !lastEncrypted.IsZero() {
		t := lastEncrypted.UTC()
		dto.LastEncryptedAt = &t
	}
	if !lastPlain.IsZero() {
		t := lastPlain.UTC()
		dto.LastPlainAt = &t
	}
	writeJSON(w, http.StatusOK, dto)
}

// handleSystemEdgeCertificateReissue marks ONLY the edge row due, so the next
// reconcile pass re-issues it with the currently configured EDGE issuer mode
// (system scope, POST-only; the portal confirm-gates the click). The stored
// certificate keeps serving until the replacement arrives. Nothing issued yet ->
// 404 rather than a success the operator would then wait on forever.
func (s *Server) handleSystemEdgeCertificateReissue(w http.ResponseWriter, r *http.Request) {
	token, ok := s.requireWebScope(w, r, "system")
	if !ok {
		return
	}
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if err := s.Portal.ReissueEdgeCertificate(r.Context(), token); err != nil {
		if errors.Is(err, portal.ErrPrincipalForbidden) {
			writeJSON(w, http.StatusForbidden, apierror.Response(portal.CodePrincipalForbidden, notAllowedMsg, ""))
			return
		}
		if errors.Is(err, portal.ErrCertificateNotFound) {
			writeJSON(w, http.StatusNotFound, apierror.Response(codeEdgeCertificateNotFound, msgNoEdgeCertificateIssued, ""))
			return
		}
		writeJSON(w, http.StatusInternalServerError, apierror.Response("certificate.reissue_failed", "could not mark the edge certificate for re-issue", ""))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleSystemEdgeCertificateBundle serves the edge certificate's PUBLIC material
// -- its full chain plus, in the self_signed mode, the internal root the upstream
// proxy has to trust (system scope, GET-only). Public certificates only, so it is
// allowed unconditionally; the private key is the separate, gated endpoint below.
func (s *Server) handleSystemEdgeCertificateBundle(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireWebScope(w, r, "system"); !ok {
		return
	}
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	pemBundle, err := s.Portal.EdgeCertificateBundlePEM(r.Context())
	if err != nil {
		if errors.Is(err, portal.ErrCertificateNotFound) {
			writeJSON(w, http.StatusNotFound, apierror.Response(codeEdgeCertificateNotFound, msgNoEdgeCertificateIssued, ""))
			return
		}
		writeJSON(w, http.StatusInternalServerError, apierror.Response("certificate.edge_bundle_failed", "could not read the edge certificate bundle", ""))
		return
	}
	w.Header().Set(contentTypeHeader, "application/x-pem-file")
	w.Header().Set("Content-Disposition", `attachment; filename="edge-fullchain.pem"`)
	_, _ = w.Write([]byte(pemBundle))
}

// handleSystemEdgeCertificateKey serves the edge certificate's PRIVATE KEY.
//
// This is the FIRST endpoint in this gateway that deliberately returns private
// key material, so its gate is narrow and its use is audited:
//   - system scope only. That scope is granted ONLY to an elevated system_admin
//     (sessionPrincipal in auth.go appends it for role==system_admin && elevated)
//     and NO API token can ever hold it (validateTokenScopes accepts exactly
//     "gateway:use" and "admin" and rejects everything else) -- both verified in
//     the source, not assumed.
//   - refused with 409 whenever the gateway can write the key to its own nginx
//     itself (an output directory is configured and its last write did not fail):
//     the key may only travel when there is no safe local path.
//   - every successful call writes ONE slog.Warn audit line identifying the
//     caller -- never the key.
//
// The pre-existing rule "no DTO carries key material" is untouched: the response
// is text (application/x-pem-file), the bare key and nothing else, and no DTO in
// this feature has a field for it.
func (s *Server) handleSystemEdgeCertificateKey(w http.ResponseWriter, r *http.Request) {
	tok, ok := s.requireWebScope(w, r, "system")
	if !ok {
		return
	}
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	keyPEM, err := s.Portal.EdgeCertificateKeyPEM(r.Context())
	if err != nil {
		switch {
		case errors.Is(err, portal.ErrEdgeKeyManaged):
			writeJSON(w, http.StatusConflict, apierror.Response("certificate.edge_key_managed",
				"the gateway delivers this key to its own nginx; there is no download", ""))
		case errors.Is(err, portal.ErrCertificateNotFound):
			writeJSON(w, http.StatusNotFound, apierror.Response(codeEdgeCertificateNotFound, msgNoEdgeCertificateIssued, ""))
		case errors.Is(err, portal.ErrCertKeyRequired):
			// The stored key cannot be opened because the certificate encryption key
			// is missing. Naming the variable turns an opaque 500 into an actionable
			// 400 (the reconcile surfaces the same cause in cert_last_error).
			writeJSON(w, http.StatusBadRequest, apierror.Response("system.cert_key_required",
				"OP_AI_GATEWAY_CERT_ENCRYPTION_KEY must be set to read certificate private keys", ""))
		default:
			writeJSON(w, http.StatusInternalServerError, apierror.Response("certificate.edge_key_failed",
				"could not read the edge certificate key", ""))
		}
		return
	}
	// The audit line: WHO downloaded the key, and when (slog stamps the time). The
	// key itself is never an attribute -- only the caller's id.
	slog.Warn("edge certificate private key downloaded", "user_id", tok.UserID, "token_id", tok.ID)
	w.Header().Set(contentTypeHeader, "application/x-pem-file")
	w.Header().Set("Content-Disposition", `attachment; filename="edge-key.pem"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(keyPEM))
}

// handleSystemEdgeCertificateProbe runs the synthetic TLS self-probe against
// the gateway's OWN edge (nginx) listener (system scope, POST-only -- it is
// an active network action, not a passive read, so it does not belong on a
// bare GET). This is the diagnostic an operator has BEFORE any real traffic
// has proven the fronting proxy speaks TLS at all: it reports not just
// success/failure but the CAUSE (unreachable, still the bootstrap
// certificate, a name mismatch, an untrusted chain, or expiry).
//
// The gateway cannot reach its own nginx's :443 by itself in either bundled
// topology (in compose the backend shares the NetBird sidecar's network
// namespace while `web` is a separate service; in Kubernetes
// `op-gateway-web` is its own Deployment) -- so an unconfigured probe target
// (OP_AI_GATEWAY_CERT_EDGE_PROBE_TARGET unset) is 409, not 500: this is a
// deployment fact the caller cannot retry past.
func (s *Server) handleSystemEdgeCertificateProbe(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireWebScope(w, r, "system"); !ok {
		return
	}
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	dto, err := s.Portal.ProbeEdgeTLS(r.Context())
	if err != nil {
		if errors.Is(err, portal.ErrEdgeProbeNotConfigured) {
			writeJSON(w, http.StatusConflict, apierror.Response("certificate.edge_probe_not_configured",
				"OP_AI_GATEWAY_CERT_EDGE_PROBE_TARGET is not set; the gateway cannot reach its own nginx by itself", ""))
			return
		}
		writeJSON(w, http.StatusInternalServerError, apierror.Response("certificate.edge_probe_failed",
			"could not run the edge TLS self-probe", ""))
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// handleSystemEdgeProxyConfig serves the generated nginx configuration for the
// reverse proxy in front of the gateway (system scope, GET-only, text/plain). It
// reveals internal names and IPs -- hence the system scope -- and contains no key
// material. It is generated from stored settings plus ONE cached resolution: the
// gateway's own mesh FQDN when cert_gateway_domain is unset and the internal mode
// is acme (portal.Service.cachedGatewayPeerDNS, 60s TTL, fail-open), so a GET in
// the steady state makes no NetBird call and a NetBird outage can neither fail
// this endpoint nor silently drop that name.
func (s *Server) handleSystemEdgeProxyConfig(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireWebScope(w, r, "system"); !ok {
		return
	}
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	cfg, err := s.Portal.EdgeProxyConfig(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, apierror.Response("certificate.edge_proxy_config_failed",
			"could not generate the proxy configuration", ""))
		return
	}
	w.Header().Set(contentTypeHeader, "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(cfg))
}
