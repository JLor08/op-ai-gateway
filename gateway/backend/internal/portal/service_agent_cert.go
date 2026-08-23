// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"op-ai-gateway/internal/routing"
	"strings"
	"time"
)

// AgentCertificateDTO is the material ONE ServerAgent may fetch for its OWN server:
// the leaf chain, its private key, and the public trust bundle. It is the only thing
// in this gateway that hands a private key to a network client, so it never travels
// anywhere else -- no portal DTO embeds it, and the tracing decorator records only
// the method name + error.
//
// ETag is an OPAQUE validator over BOTH halves (see agentCertETag): the leaf AND the
// bundle. A leaf-only validator would make a CA rotation invisible, because a
// rotation deliberately publishes the new root BEFORE re-signing any leaf -- every
// leaf fingerprint is then unchanged, the conditional GET would answer 304, and the
// new root would never reach the agent.
type AgentCertificateDTO struct {
	Domain       string
	Fingerprint  string
	FullchainPEM string
	KeyPEM       string
	CABundlePEM  string
	ETag         string
	NotBefore    *time.Time
	NotAfter     *time.Time
}

// GatewayMeshCertificateMaterial is the gateway's internal, process-only mesh
// listener material. It is deliberately not a portal DTO and carries no JSON
// tags: the private key is consumed only by cmd/gateway's tls.Certificate holder.
type GatewayMeshCertificateMaterial struct {
	Domain            string
	FullchainPEM      string
	KeyPEM            string
	CABundlePEM       string
	Fingerprint       string
	IssuerFingerprint string
	NotAfter          time.Time
}

// GatewayMeshCertificate returns one complete, currently usable kind=gateway
// certificate for the mesh listener. An absent/off/incomplete row is a normal
// ErrCertificateNotFound no-op; key-open failures retain their stable typed error
// so the listener manager can keep serving its last-good holder unchanged.
func (s *Service) GatewayMeshCertificate(ctx context.Context) (GatewayMeshCertificateMaterial, error) {
	// Read the raw checkbox through the settings store rather than the bool-only UI
	// accessor: CertModuleChecked intentionally collapses a store error to false,
	// but this runtime material path must preserve every settings read error.
	if s.settings == nil {
		return GatewayMeshCertificateMaterial{}, ErrCertificateNotFound
	}
	values, err := s.settings.SystemSettings(ctx)
	if err != nil {
		return GatewayMeshCertificateMaterial{}, err
	}
	if !CertEnabled(values) {
		return GatewayMeshCertificateMaterial{}, ErrCertificateNotFound
	}
	if s.routes == nil {
		return GatewayMeshCertificateMaterial{}, ErrCertificateNotFound
	}
	rows, err := s.routes.Certificates(ctx)
	if err != nil {
		return GatewayMeshCertificateMaterial{}, err
	}
	now := s.clock().UTC()
	var cert routing.Certificate
	found := false
	for _, row := range rows {
		if !gatewayMeshCertValid(row, now) {
			continue
		}
		if !found || freshCertRow(row, cert) {
			cert, found = row, true
		}
	}
	if !found {
		return GatewayMeshCertificateMaterial{}, ErrCertificateNotFound
	}
	keyPEM, err := s.openCertSecret(cert.KeySealed)
	if err != nil {
		return GatewayMeshCertificateMaterial{}, err
	}
	if keyPEM == "" {
		return GatewayMeshCertificateMaterial{}, ErrCertificateNotFound
	}
	bundle, err := certificateCABundlePEM(values, now)
	if err != nil {
		// The listener consumes only the leaf/key. The public CA bundle is additive
		// bootstrap material for later config generation, so a bundle-only read
		// failure must not suppress an otherwise complete serving certificate.
		bundle = ""
	}
	return GatewayMeshCertificateMaterial{
		Domain:            cert.Domain,
		FullchainPEM:      cert.FullchainPEM,
		KeyPEM:            keyPEM,
		CABundlePEM:       bundle,
		Fingerprint:       cert.Fingerprint,
		IssuerFingerprint: cert.IssuerFingerprint,
		NotAfter:          cert.NotAfter,
	}, nil
}

func gatewayMeshCertValid(cert routing.Certificate, now time.Time) bool {
	return cert.Kind == "gateway" && cert.Status == "active" &&
		strings.TrimSpace(cert.Domain) != "" && strings.TrimSpace(cert.Fingerprint) != "" &&
		!cert.NotBefore.IsZero() && !cert.NotBefore.After(now) && certValid(cert, now)
}

// agentCertETag builds the opaque conditional-GET validator: the leaf fingerprint
// plus a digest of the trust bundle, so a change in EITHER is a change.
func agentCertETag(leafFingerprint, caBundlePEM string) string {
	sum := sha256.Sum256([]byte(caBundlePEM))
	return leafFingerprint + "-" + hex.EncodeToString(sum[:])
}

// AgentCertificate returns the certificate material for serverID -- the server the
// caller's agent token is bound to. The caller (the gateway handler) has already
// derived serverID from that token; there is deliberately no parameter here that
// could redirect the lookup.
//
// ErrCertificateNotFound means "nothing to install right now" and MUST leave the
// agent's existing files alone: the module is off, the server has no certificate, or
// the row carries no usable/unexpired material.
func (s *Service) AgentCertificate(ctx context.Context, serverID string) (AgentCertificateDTO, error) {
	if serverID == "" {
		return AgentCertificateDTO{}, ErrCertificateNotFound
	}
	// The module gate is the RAW cert_enabled checkbox, NOT CertSettings' ok.
	// CertSettings reports !ok as soon as the mode-dependent mandatory field is
	// missing (e.g. acme without acme_email) -- which is not "module off". Gating on
	// that would withdraw an already-issued, still-valid certificate from a running
	// agent because an unrelated setting is incomplete: exactly the continuity break
	// this feature forbids.
	if !s.CertModuleChecked(ctx) {
		return AgentCertificateDTO{}, ErrCertificateNotFound
	}
	if s.routes == nil {
		return AgentCertificateDTO{}, ErrCertificateNotFound
	}
	cert, err := s.agentCertRow(ctx, serverID)
	if err != nil {
		return AgentCertificateDTO{}, err
	}
	keyPEM, err := s.openCertSecret(cert.KeySealed)
	if err != nil {
		// Includes ErrCertKeyRequired. Never fall through to a response with an
		// empty key: the agent would install a chain it cannot serve.
		return AgentCertificateDTO{}, err
	}
	if keyPEM == "" {
		return AgentCertificateDTO{}, ErrCertificateNotFound
	}
	bundle, err := s.CertificateCABundlePEM(ctx)
	if err != nil {
		// An absent internal CA is NOT an error: the bundle is empty exactly when no
		// internal root is stored (cert_ca_cert unset). That is mode-INDEPENDENT --
		// switching to acme deliberately keeps the internal root alive while its
		// leaves live, so an acme-mode gateway commonly still ships a bundle.
		if !errors.Is(err, ErrCertificateNotFound) {
			return AgentCertificateDTO{}, err
		}
		bundle = ""
	}
	dto := AgentCertificateDTO{
		Domain:       cert.Domain,
		Fingerprint:  cert.Fingerprint,
		FullchainPEM: cert.FullchainPEM,
		KeyPEM:       keyPEM,
		CABundlePEM:  bundle,
		ETag:         agentCertETag(cert.Fingerprint, bundle),
	}
	dto.NotBefore = timePtr(cert.NotBefore)
	dto.NotAfter = timePtr(cert.NotAfter)
	return dto, nil
}

// agentCertRow picks WHICH row to serve for a server, and deliberately does NOT use
// routing.Store.CertificateByServer: that returns the alphabetically first domain
// linked to the server, and there is no unique constraint on server_id. A failed
// re-issue after a rename can leave a second, MATERIAL-LESS row behind (see
// recordCertFailure) -- if its name sorts first, CertificateByServer hands the agent
// an empty row and the working certificate becomes invisible. That exact confusion
// was Plan A's one shipped defect (edgeRow, finding I1).
//
// So: consider every row linked to the server, prefer one with usable, unexpired
// material, and among those prefer the FRESHEST -- the domain is only the final
// deterministic tiebreak. Ordering by domain alone would deterministically serve
// the STALE certificate whenever a rename leaves two simultaneously-valid rows
// behind (the prune's delete is best-effort and merely logged on failure), and
// the agent would then install a chain for a name the server no longer answers to.
func (s *Service) agentCertRow(ctx context.Context, serverID string) (routing.Certificate, error) {
	rows, err := s.routes.Certificates(ctx)
	if err != nil {
		return routing.Certificate{}, err
	}
	now := s.clock().UTC()
	var best routing.Certificate
	found := false
	for _, row := range rows {
		if row.ServerID != serverID || !certValid(row, now) {
			continue
		}
		if !found || freshCertRow(row, best) {
			best, found = row, true
		}
	}
	if !found {
		return routing.Certificate{}, ErrCertificateNotFound
	}
	return best, nil
}

// freshCertRow reports whether row should win over best: newer material first
// (NotBefore, then IssuedAt), the lower domain only as the final tiebreak so the
// choice stays deterministic when two rows are equally fresh.
func freshCertRow(row, best routing.Certificate) bool {
	if !row.NotBefore.Equal(best.NotBefore) {
		return row.NotBefore.After(best.NotBefore)
	}
	if !row.IssuedAt.Equal(best.IssuedAt) {
		return row.IssuedAt.After(best.IssuedAt)
	}
	return row.Domain < best.Domain
}
