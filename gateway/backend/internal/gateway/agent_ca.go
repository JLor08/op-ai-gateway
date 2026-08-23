// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"log/slog"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/portal"
)

const maxAgentCABundleBytes = 1 << 20

func (s *Server) handleAgentCA(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	principal, ok := s.authenticateAgent(w, r)
	if !ok {
		return
	}
	if s.Portal == nil {
		writeJSON(w, http.StatusNotFound, apierror.Response("certificate.ca_not_found", "no certificate CA bundle available", ""))
		return
	}
	bundle, err := s.Portal.CertificateCABundlePEM(r.Context())
	if err != nil {
		if errors.Is(err, portal.ErrCertificateNotFound) {
			writeJSON(w, http.StatusNotFound, apierror.Response("certificate.ca_not_found", "no certificate CA bundle available", ""))
			return
		}
		slog.Error("agent CA bundle read failed", "server_id", principal.ServerID, "err", err)
		writeJSON(w, http.StatusInternalServerError, apierror.Response("certificate.ca_read_failed", "could not read the certificate CA bundle", ""))
		return
	}
	if len(bundle) == 0 || len(bundle) > maxAgentCABundleBytes || !publicCertificateBundleOnly([]byte(bundle)) {
		slog.Error("agent CA bundle validation failed", "server_id", principal.ServerID)
		writeJSON(w, http.StatusInternalServerError, apierror.Response("certificate.ca_read_failed", "could not read the certificate CA bundle", ""))
		return
	}
	sum := sha256.Sum256([]byte(bundle))
	etag := hex.EncodeToString(sum[:])
	w.Header().Set("ETag", `"`+etag+`"`)
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(bundle))
}

func publicCertificateBundleOnly(raw []byte) bool {
	const certificateBegin = "-----BEGIN CERTIFICATE-----"
	rest := bytes.TrimSpace(raw)
	count := 0
	for len(rest) > 0 {
		if !bytes.HasPrefix(rest, []byte(certificateBegin)) {
			return false
		}
		block, next := pem.Decode(rest)
		if block == nil {
			return false
		}
		if block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return false
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil || !cert.IsCA {
			return false
		}
		count++
		rest = bytes.TrimSpace(next)
	}
	return count > 0
}
