// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"strings"
	"time"
)

// certEdgeProbeTimeout bounds the ENTIRE synthetic TLS self-probe -- TCP
// connect plus the TLS handshake as one ceiling (via a context.WithTimeout
// wrapping both DialContext and HandshakeContext below). Five seconds is
// generous for what is, in both bundled topologies, a hop to an ADJACENT
// container/pod (in compose the backend reaches `web` over the compose
// network; in Kubernetes op-gateway-web is a Service on the same cluster
// network) -- normally sub-second -- while still bounding a genuinely wedged
// listener (a firewall black-holing the SYN, or a proxy that accepts the TCP
// connection and never completes the handshake) to a small, predictable cost
// on the one request that triggered it. It is not configurable: this is a
// diagnostic action, not a tunable integration point, and a longer bound
// would make an operator's manual "probe now" click hang unhelpfully.
const certEdgeProbeTimeout = 5 * time.Second

// edgeBootstrapCN is the EXACT Subject Common Name the entrypoint wrapper
// stamps onto its throwaway self-signed pair on a from-scratch boot
// (gateway/deploy/nginx-cert-entrypoint.sh, BOOTSTRAP_CN: `openssl req -x509
// -subj "/CN=$BOOTSTRAP_CN"`, no SAN at all) so a verifying peer can
// recognise -- and reject -- it by name alone. Kept as a literal, not a
// shared constant with the shell script (different languages, no import path
// between them); the wrapper's own shell test
// (nginx-cert-entrypoint.test.sh) pins the shell side of this string, and a
// change to either side without the other is caught by nothing but careful
// reading -- which is exactly why both sides quote it verbatim in a comment.
const edgeBootstrapCN = "OP AI Gateway BOOTSTRAP - not trusted"

// ErrEdgeProbeNotConfigured is returned by ProbeEdgeTLS when no probe target
// is configured (OP_AI_GATEWAY_CERT_EDGE_PROBE_TARGET is empty). This is a
// DEPLOYMENT fact, not something a caller can retry past: the backend cannot
// reach its own nginx's :443 listener by itself in either bundled topology --
// in docker-compose the backend shares the NetBird sidecar's network
// namespace while `web` is its own service, and in Kubernetes
// `op-gateway-web` is its own Deployment on the cluster network. Mapped to
// HTTP 409 by the gateway handler.
var ErrEdgeProbeNotConfigured = errors.New("portal: no edge TLS probe target is configured")

// The EdgeProbeDTO.Reason values below name the CAUSE of a failed probe, in
// the same order ProbeEdgeTLS checks for them. There is deliberately no
// "reason=ok" constant: OK=true with Reason=="" is the only success shape, so
// a caller that merely checks OK is always correct without also having to
// know a magic string.
const (
	// edgeProbeUnreachable covers BOTH a failed TCP connect and a failed TLS
	// handshake -- from a diagnostic's point of view "the target refused the
	// connection" and "the target never finished negotiating TLS" call for
	// the same operator action (check that something is actually listening
	// and speaking TLS on that address).
	edgeProbeUnreachable = "unreachable"
	// edgeProbeBootstrap: the peer is still presenting the throwaway pair the
	// nginx entrypoint wrapper writes before the gateway has ever delivered a
	// real certificate -- see edgeBootstrapCN.
	edgeProbeBootstrap = "bootstrap_certificate"
	// edgeProbeNameMismatch: the presented certificate does not cover the
	// configured edge name (EdgeProbeDTO.SANs lists what it covers instead).
	edgeProbeNameMismatch = "name_mismatch"
	// edgeProbeUntrustedChain: the presented certificate does not verify
	// against the applicable anchor (the internal CA bundle in self_signed
	// mode, the system trust store in acme mode) for any reason OTHER than
	// expiry -- which gets its own, more specific reason below.
	edgeProbeUntrustedChain = "chain_untrusted"
	// edgeProbeExpired: the presented certificate is outside its own validity
	// window (x509.CertificateInvalidError{Reason: x509.Expired}), checked
	// SEPARATELY from the general chain-verify failure above because "your
	// certificate expired" is a far more actionable answer than "the chain
	// does not verify" when both would technically be true.
	edgeProbeExpired = "expired"
)

// EdgeProbeDTO is the result of ONE synthetic TLS handshake against the
// gateway's own edge (nginx) listener. It never carries key material -- the
// probe only ever reads a PUBLIC certificate chain off the wire, exactly like
// any other TLS client -- and it names the CAUSE of a failure rather than
// merely reporting ok=false, because "the certificate is still the bootstrap
// pair" and "the proxy never terminates TLS at all" call for entirely
// different operator action.
type EdgeProbeDTO struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
	// Message is a short, human-readable restatement of Reason plus the
	// concrete detail (which address, which name, which underlying error) for
	// direct display; Reason is the stable machine key a caller should
	// actually branch on.
	Message string `json:"message,omitempty"`

	Target string `json:"target"`
	// ExpectedName is the FIRST configured edge name (the one the leaf's SANs
	// are checked against) -- empty when no edge name is configured at all,
	// in which case the probe still runs (reachability, bootstrap-detection,
	// chain trust) but the name check is skipped rather than reported as a
	// mismatch against nothing.
	ExpectedName string `json:"expected_name,omitempty"`

	// Subject/SANs/NotAfter describe the certificate the probe actually
	// received. Filled whenever the handshake completed far enough to
	// produce one -- i.e. every failure case EXCEPT "unreachable" -- so a
	// name-mismatch or untrusted-chain answer still shows exactly what WAS
	// presented, not just that something was wrong with it.
	Subject  string     `json:"subject,omitempty"`
	SANs     []string   `json:"sans,omitempty"`
	NotAfter *time.Time `json:"not_after,omitempty"`
}

// ProbeEdgeTLS performs ONE synthetic TLS handshake against the gateway's
// OWN edge (nginx) listener and reports whether it terminates encryption
// with a usable certificate -- the diagnostic an operator needs BEFORE any
// real traffic has proven the fronting proxy speaks TLS at all (the
// cert_edge_require_https arming precondition needs exactly that proof, but
// only ever gets it passively, from traffic that happens to arrive).
//
// It deliberately sends NO HTTP request of any kind -- only a bare TLS
// handshake. Everything this method reports is available from the completed
// handshake alone (tls.Conn.ConnectionState().PeerCertificates), and that
// design choice sidesteps a real problem: nginx's :443 AND :80 server blocks
// blank the X-OP-Edge-Self-Probe header (edgeSchemeSelfProbeHeaderName,
// internal/gateway/edge_scheme.go) in every one of their header-setting
// blocks, exactly like every other internal marker header -- so a probe that
// sent an HTTP request THROUGH nginx back to this gateway would arrive with
// its self-marking header already stripped, and would therefore be counted
// as a genuine encrypted observation by countsAsObservation. That would
// self-satisfy the very precondition this probe exists to test, on the
// strength of the gateway convincing itself rather than any evidence about
// the real proxy. A pure handshake has no such request to strip a header
// from, so the marker/exclusion machinery in internal/gateway/edge_scheme.go
// is simply never engaged by this path -- it is kept, unused here, as
// defence in depth for a HYPOTHETICAL future prober that does send a
// request.
//
// No lock is held across the network dial: CertSettings and
// CertificateCABundlePEM below only read the settings store (grep-verified —
// neither takes certMu nor edgeMu), so there is nothing to release before the
// handshake and nothing the handshake could block that would, in turn, block
// a concurrent certificate operation.
func (s *Service) ProbeEdgeTLS(ctx context.Context) (EdgeProbeDTO, error) {
	if s.cert.edgeProbeTarget == "" {
		return EdgeProbeDTO{}, ErrEdgeProbeNotConfigured
	}
	dto := EdgeProbeDTO{Target: s.cert.edgeProbeTarget}

	set, _, err := s.CertSettings(ctx)
	if err != nil {
		return EdgeProbeDTO{}, err
	}
	if names := configuredEdgeNames(set); len(names) > 0 {
		dto.ExpectedName = names[0]
	}

	// The verification anchor follows the EDGE issuer mode specifically (not
	// the internal mode, which is independent -- see modeFor's doc comment)
	// and is computed by edgeProbeAnchor -- see its doc comment for why a
	// self_signed mode with no usable CA bundle must get a deliberate
	// NON-NIL EMPTY pool rather than nil.
	mode := set.modeFor(certEdgeKind)
	var caBundle string
	if mode == IssuerModeSelfSigned {
		if bundle, caErr := s.CertificateCABundlePEM(ctx); caErr == nil {
			caBundle = bundle
		}
		// caErr != nil (typically ErrCertificateNotFound: no CA created yet)
		// leaves caBundle "" -- edgeProbeAnchor still returns a non-nil empty
		// pool for self_signed in that case, never nil.
	}
	roots := edgeProbeAnchor(mode, caBundle)

	probeCtx, cancel := context.WithTimeout(ctx, certEdgeProbeTimeout)
	defer cancel()
	rawConn, err := (&net.Dialer{}).DialContext(probeCtx, "tcp", s.cert.edgeProbeTarget)
	if err != nil {
		dto.Reason = edgeProbeUnreachable
		dto.Message = "could not reach " + s.cert.edgeProbeTarget + ": " + err.Error()
		return dto, nil
	}
	tlsConn := tls.Client(rawConn, &tls.Config{
		ServerName: dto.ExpectedName,
		// Verified manually below (bootstrap / name / chain / expiry, in that
		// order) so every failure mode still has the presented certificate to
		// describe -- Go's own verification would abort the handshake before
		// any of that is ever visible.
		InsecureSkipVerify: true,
	})
	defer tlsConn.Close()
	if err := tlsConn.HandshakeContext(probeCtx); err != nil {
		dto.Reason = edgeProbeUnreachable
		dto.Message = "TLS handshake with " + s.cert.edgeProbeTarget + " failed: " + err.Error()
		return dto, nil
	}

	certs := tlsConn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		dto.Reason = edgeProbeUnreachable
		dto.Message = "the TLS handshake completed but the peer presented no certificate"
		return dto, nil
	}
	leaf := certs[0]
	dto.Subject = leaf.Subject.CommonName
	dto.SANs = edgeProbeCertSANs(leaf)
	notAfter := leaf.NotAfter.UTC()
	dto.NotAfter = &notAfter

	if leaf.Subject.CommonName == edgeBootstrapCN {
		dto.Reason = edgeProbeBootstrap
		dto.Message = "nginx is still presenting its throwaway bootstrap certificate -- the gateway has not delivered a real one yet"
		return dto, nil
	}

	if dto.ExpectedName != "" {
		if hostErr := leaf.VerifyHostname(dto.ExpectedName); hostErr != nil {
			dto.Reason = edgeProbeNameMismatch
			dto.Message = "the presented certificate does not cover " + dto.ExpectedName + " (found: " + strings.Join(dto.SANs, ", ") + ")"
			return dto, nil
		}
	}

	intermediates := x509.NewCertPool()
	for _, c := range certs[1:] {
		intermediates.AddCert(c)
	}
	if _, verr := leaf.Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediates,
		CurrentTime:   s.clock().UTC(),
	}); verr != nil {
		var invalid x509.CertificateInvalidError
		if errors.As(verr, &invalid) && invalid.Reason == x509.Expired {
			dto.Reason = edgeProbeExpired
			dto.Message = "the presented certificate is expired or not yet valid: " + verr.Error()
			return dto, nil
		}
		dto.Reason = edgeProbeUntrustedChain
		dto.Message = "the presented certificate chain does not verify: " + verr.Error()
		return dto, nil
	}

	dto.OK = true
	if dto.ExpectedName != "" {
		dto.Message = "the edge listener presents a valid, trusted certificate for " + dto.ExpectedName
	} else {
		dto.Message = "the edge listener presents a valid, trusted certificate"
	}
	return dto, nil
}

// edgeProbeAnchor returns the trust anchor ProbeEdgeTLS verifies the
// presented certificate against for the given EDGE issuer mode.
//
// For every mode OTHER than self_signed (i.e. acme) it returns nil, which
// makes x509.Certificate.Verify fall back to the platform/system trust
// store on its own -- exactly the anchor a publicly-issued (Let's Encrypt)
// leaf must be checked against.
//
// For self_signed it ALWAYS returns a non-nil pool, even when caBundlePEM is
// empty (no internal CA has been created yet, or it could not be read). This
// is deliberate, not a fallback-to-something-reasonable: falling through to
// nil here -- and therefore to the system trust store -- would let a STALE,
// still publicly-trusted leaf that nginx happens to still be serving (e.g.
// because an operator switched the edge mode to self_signed while ensureCA
// could not run -- say OP_AI_GATEWAY_CERT_ENCRYPTION_KEY is unset on a disk
// store, so no self-signed material was ever delivered and the previous
// acme-issued chain is still live) verify successfully against the SYSTEM
// store and report ok:true -- falsely confirming "self-signed is working"
// in the one state this probe exists to catch: the self-signed CA was never
// actually consulted. An empty pool can never successfully verify anything,
// so the probe instead reports the honest, bounded chain_untrusted.
func edgeProbeAnchor(mode string, caBundlePEM string) *x509.CertPool {
	if mode != IssuerModeSelfSigned {
		return nil
	}
	pool := x509.NewCertPool()
	if caBundlePEM != "" {
		pool.AppendCertsFromPEM([]byte(caBundlePEM))
	}
	return pool
}

// edgeProbeCertSANs lists the DNS names and IP addresses on leaf, in that
// order. Used only to populate EdgeProbeDTO.SANs so a name-mismatch answer
// can show exactly what it found.
func edgeProbeCertSANs(leaf *x509.Certificate) []string {
	out := make([]string, 0, len(leaf.DNSNames)+len(leaf.IPAddresses))
	out = append(out, leaf.DNSNames...)
	for _, ip := range leaf.IPAddresses {
		out = append(out, ip.String())
	}
	return out
}
