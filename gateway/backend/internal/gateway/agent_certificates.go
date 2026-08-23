// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"errors"
	"log/slog"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/portal"
	"strings"
)

// handleAgentCertificate serves ONE ServerAgent the certificate material for its OWN
// server: the leaf chain, its private key and the public trust bundle.
//
// This is the only route in this gateway that hands a private key to a network
// client, so two properties are load-bearing:
//
//   - The target server comes ONLY from the agent token (the same
//     ExtractBearerSecret -> HashSecret -> LookupAgentToken prologue
//     handleAgentDownload uses, with the same three responses). There is no
//     parameter, path segment or body field that can redirect the lookup, so one
//     agent can never read another server's key.
//   - The response is never cacheable (Cache-Control: no-store). The path is served
//     through the shipped nginx configs' generic `location /api/` block, so only this
//     header stands between the key and a future proxy_cache on that location.
//
// A conditional GET (If-None-Match against the opaque ETag, which covers BOTH the
// leaf and the trust bundle) answers 304 with no body -- the steady state, so an
// idle fleet does not stream keys around on every poll.
//
// On the public mux this route is wrapped by the netbird_only gate (see routes());
// on the agent mux the NetBird-bound listener itself is the boundary.
func (s *Server) handleAgentCertificate(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	principal, ok := s.authenticateAgent(w, r)
	if !ok {
		return
	}
	serverID := principal.ServerID
	if s.Portal == nil {
		writeJSON(w, http.StatusNotFound, apierror.Response("certificate.not_found", "no certificate available for this server", ""))
		return
	}
	dto, err := s.Portal.AgentCertificate(r.Context(), serverID)
	if err != nil {
		switch {
		case errors.Is(err, portal.ErrCertificateNotFound):
			// "Nothing to install right now" -- the agent MUST leave its existing
			// files alone rather than treat this as a revocation.
			writeJSON(w, http.StatusNotFound, apierror.Response("certificate.not_found", "no certificate available for this server", ""))
		case errors.Is(err, portal.ErrCertKeyRequired):
			// The operator has to learn which variable is missing; the agent can do
			// nothing about it.
			slog.Warn("agent certificate unavailable: certificate encryption key missing", "server_id", serverID)
			writeJSON(w, http.StatusInternalServerError, apierror.Response("system.cert_key_required", "OP_AI_GATEWAY_CERT_ENCRYPTION_KEY must be set to read certificate private keys", ""))
		default:
			// Deliberately static: err.Error() could carry store internals.
			slog.Error("agent certificate read failed", "server_id", serverID, "err", err)
			writeJSON(w, http.StatusInternalServerError, apierror.Response("certificate.read_failed", "could not read the certificate", ""))
		}
		return
	}

	etag := `"` + dto.ETag + `"`
	w.Header().Set("ETag", etag)
	// The body contains a private key: no cache, anywhere, ever.
	w.Header().Set("Cache-Control", "no-store")
	if etagMatches(r.Header.Get("If-None-Match"), dto.ETag) {
		slog.Debug("agent certificate unchanged", "server_id", serverID, "fingerprint", dto.Fingerprint)
		w.WriteHeader(http.StatusNotModified)
		return
	}

	// Exactly ONE audit line per key handed out -- never the key, the chain or the
	// token.
	slog.Info("agent certificate served", "server_id", serverID, "domain", dto.Domain,
		"fingerprint", dto.Fingerprint, "not_after", dto.NotAfter)
	writeJSON(w, http.StatusOK, map[string]any{
		"domain":        dto.Domain,
		"fingerprint":   dto.Fingerprint,
		"fullchain_pem": dto.FullchainPEM,
		"key_pem":       dto.KeyPEM,
		"ca_bundle_pem": dto.CABundlePEM,
		"etag":          dto.ETag,
		"not_before":    dto.NotBefore,
		"not_after":     dto.NotAfter,
	})
}

// etagMatches implements the If-None-Match comparison for our single opaque
// validator: a comma-separated list, optional weak prefix and optional quotes are all
// tolerated, and "*" matches any existing representation (RFC 9110).
func etagMatches(header, etag string) bool {
	header = strings.TrimSpace(header)
	if header == "" || etag == "" {
		return false
	}
	if header == "*" {
		return true
	}
	for _, raw := range strings.Split(header, ",") {
		candidate := strings.TrimSpace(raw)
		candidate = strings.TrimPrefix(candidate, "W/")
		candidate = strings.Trim(candidate, `"`)
		if candidate == etag {
			return true
		}
	}
	return false
}
