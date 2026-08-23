// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"log/slog"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/portal"
	"strings"
)

// publicCertificateDomain reads and normalizes the {domain} path value for the
// public-domain export routes. The ServeMux {domain} wildcard is a single path
// segment (it already rejects an embedded slash/traversal), and PathValue returns
// it already unescaped; this only lowercases/trims and rejects an empty or
// slash-bearing value defensively before any store access. The service applies
// the real managed + kind=public gates.
func publicCertificateDomain(w http.ResponseWriter, r *http.Request) (string, bool) {
	domain := strings.ToLower(strings.TrimSpace(r.PathValue("domain")))
	if domain == "" || strings.ContainsAny(domain, "/\\") {
		writeJSON(w, http.StatusBadRequest, apierror.Response("request.invalid_path", "invalid domain", ""))
		return "", false
	}
	return domain, true
}

// publicCertExportErrRows are writePublicCertExportError's mapper-specific
// rows; none of these sentinels are handled by any other mapper.
var publicCertExportErrRows = []errRow{
	{err: portal.ErrPublicCertificateNotManaged, status: http.StatusConflict, code: "certificate.public_not_managed", msg: "public-domain management is off or this domain is not configured"},
	{err: portal.ErrCertificateNotFound, status: http.StatusNotFound, code: "certificate.not_found", msg: "no public certificate for this domain"},
	{err: portal.ErrCertKeyRequired, status: http.StatusBadRequest, code: "system.cert_key_required", msg: "OP_AI_GATEWAY_CERT_ENCRYPTION_KEY must be set to read certificate private keys"},
}

// writePublicCertExportError maps the shared service errors to the exact HTTP
// codes the frontend expects: 409 when the domain is not managed, 404 when there
// is no usable kind=public row (including a name collision with a gateway/edge
// row), 400 when the key cannot be opened for lack of the encryption key.
func writePublicCertExportError(w http.ResponseWriter, err error, failCode, failMsg string) {
	writeMappedError(w, err, publicCertExportErrRows, http.StatusInternalServerError, failCode, failMsg)
}

// handleSystemPublicCertificateBundle serves the PUBLIC chain of a managed
// public-domain certificate (system scope, GET-only), so an upstream reverse
// proxy can serve it and retire its own certbot. No key material, so it is
// allowed unconditionally once the managed/kind gates pass.
func (s *Server) handleSystemPublicCertificateBundle(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireWebScope(w, r, "system"); !ok {
		return
	}
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	domain, ok := publicCertificateDomain(w, r)
	if !ok {
		return
	}
	bundle, err := s.certPortal().PublicCertificateBundlePEM(r.Context(), domain)
	if err != nil {
		writePublicCertExportError(w, err, "certificate.public_bundle_failed", "could not read the public certificate bundle")
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Header().Set("Content-Disposition", `attachment; filename="public-fullchain.pem"`)
	_, _ = w.Write([]byte(bundle))
}

// handleSystemPublicCertificateKey serves the PRIVATE KEY of a managed
// public-domain certificate (system scope, GET-only). Like the edge key download
// it returns key material deliberately, so it is narrowly gated and audited: the
// kind=="public" service gate guarantees a name collision can never leak a mesh or
// edge key, every successful call writes ONE slog.Warn identifying the caller (never
// the key), and Cache-Control: no-store is set.
func (s *Server) handleSystemPublicCertificateKey(w http.ResponseWriter, r *http.Request) {
	tok, ok := s.requireWebScope(w, r, "system")
	if !ok {
		return
	}
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	domain, ok := publicCertificateDomain(w, r)
	if !ok {
		return
	}
	keyPEM, err := s.certPortal().PublicCertificateKeyPEM(r.Context(), domain)
	if err != nil {
		writePublicCertExportError(w, err, "certificate.public_key_failed", "could not read the public certificate key")
		return
	}
	slog.Warn("public certificate private key downloaded", "domain", domain, "user_id", tok.UserID, "token_id", tok.ID)
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Header().Set("Content-Disposition", `attachment; filename="public-key.pem"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(keyPEM))
}
