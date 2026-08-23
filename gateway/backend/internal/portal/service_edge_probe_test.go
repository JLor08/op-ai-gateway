// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"op-ai-gateway/internal/certissue"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// edgeProbeEnv mirrors certEnv (service_certificates_test.go) but also wires
// the probe target -- certEnv has no field for it, and every other
// certificate test deliberately does not need one.
func edgeProbeEnv(t *testing.T, target string) (*Service, context.Context) {
	t.Helper()
	svc := NewService(ServiceDeps{
		Routes:              routing.NewMemoryStore(),
		SystemSettings:      NewMemorySystemSettings(),
		SettingsVolatile:    true,
		ACMEChallenges:      certissue.NewMemoryChallengeStore(),
		CertEdgeProbeTarget: target,
	})
	return svc, context.Background()
}

// startProbeServer starts a bare TLS listener presenting cert and returns its
// address. Deliberately NOT an httptest.Server and deliberately reading
// nothing beyond the handshake: ProbeEdgeTLS sends no HTTP request at all
// (see its doc comment for why), so the fake "nginx" on the other end has
// nothing to serve -- only a TLS handshake to complete.
func startProbeServer(t *testing.T, cert tls.Certificate) string {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatalf("start TLS listener: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_ = c.(*tls.Conn).Handshake()
			}(c)
		}
	}()
	return ln.Addr().String()
}

// tlsCertFromResult builds a tls.Certificate from a certissue.Result the same
// way any real TLS server loads a chain+key pair off disk: X509KeyPair reads
// EVERY PEM block in FullchainPEM as the chain (leaf first, exactly how
// selfsigned.go writes it) and pairs it with the leaf's own key.
func tlsCertFromResult(t *testing.T, res certissue.Result) tls.Certificate {
	t.Helper()
	cert, err := tls.X509KeyPair([]byte(res.FullchainPEM), []byte(res.KeyPEM))
	if err != nil {
		t.Fatalf("build tls.Certificate from certissue.Result: %v", err)
	}
	return cert
}

// bootstrapLikeCert mints a throwaway self-signed leaf whose Subject Common
// Name is edgeBootstrapCN and which carries NO SANs at all -- mirroring
// exactly what gateway/deploy/nginx-cert-entrypoint.sh writes on a
// from-scratch boot (`openssl req -x509 -subj "/CN=$BOOTSTRAP_CN"`, no
// `-addext subjectAltName`).
func bootstrapLikeCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: edgeBootstrapCN},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(30 * 24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func TestProbeEdgeTLSReturnsErrNotConfiguredWhenNoTargetIsSet(t *testing.T) {
	svc, ctx := edgeProbeEnv(t, "")
	if _, err := svc.ProbeEdgeTLS(ctx); !errors.Is(err, ErrEdgeProbeNotConfigured) {
		t.Fatalf("ProbeEdgeTLS with no target = %v, want ErrEdgeProbeNotConfigured", err)
	}
}

func TestProbeEdgeTLSReportsUnreachableWhenNothingListens(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	// Nothing listens at addr any more -> a connection there refuses fast.
	svc, ctx := edgeProbeEnv(t, addr)
	dto, err := svc.ProbeEdgeTLS(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dto.OK {
		t.Fatal("want ok=false for an unreachable target")
	}
	if dto.Reason != edgeProbeUnreachable {
		t.Fatalf("reason = %q, want %q", dto.Reason, edgeProbeUnreachable)
	}
	if dto.Target != addr {
		t.Fatalf("target = %q, want %q", dto.Target, addr)
	}
}

func TestProbeEdgeTLSDetectsTheBootstrapCertificate(t *testing.T) {
	addr := startProbeServer(t, bootstrapLikeCert(t))
	svc, ctx := edgeProbeEnv(t, addr)
	enableACME(t, svc, ctx, "all")
	enableEdge(t, svc, ctx, IssuerModeSelfSigned, "edge.example")

	dto, err := svc.ProbeEdgeTLS(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dto.OK {
		t.Fatal("want ok=false for the throwaway bootstrap certificate")
	}
	if dto.Reason != edgeProbeBootstrap {
		t.Fatalf("reason = %q, want %q", dto.Reason, edgeProbeBootstrap)
	}
	if dto.Subject != edgeBootstrapCN {
		t.Fatalf("subject = %q, want the bootstrap CN", dto.Subject)
	}
}

func TestProbeEdgeTLSReportsNameMismatchWithFoundSANs(t *testing.T) {
	ca, caCertPEM, _, err := certissue.NewCA("Test CA", 24*365*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	res, err := ca.IssueFor([]string{"other.example"}, time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	addr := startProbeServer(t, tlsCertFromResult(t, res))

	svc, ctx := edgeProbeEnv(t, addr)
	enableACME(t, svc, ctx, "all")
	enableEdge(t, svc, ctx, IssuerModeSelfSigned, "edge.example")
	if err := svc.settings.SetSystemSetting(ctx, certCACertKey, caCertPEM, svc.clock()); err != nil {
		t.Fatal(err)
	}

	dto, err := svc.ProbeEdgeTLS(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dto.OK {
		t.Fatal("want ok=false for a name mismatch")
	}
	if dto.Reason != edgeProbeNameMismatch {
		t.Fatalf("reason = %q, want %q", dto.Reason, edgeProbeNameMismatch)
	}
	if len(dto.SANs) != 1 || dto.SANs[0] != "other.example" {
		t.Fatalf("SANs = %v, want [other.example] -- the response must list what it actually found", dto.SANs)
	}
	if dto.ExpectedName != "edge.example" {
		t.Fatalf("expected_name = %q, want edge.example", dto.ExpectedName)
	}
}

func TestProbeEdgeTLSReportsChainUntrustedWhenIssuedByADifferentCA(t *testing.T) {
	_, trustedCAPEM, _, err := certissue.NewCA("Trusted CA", 24*365*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	rogueCA, _, _, err := certissue.NewCA("Rogue CA", 24*365*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	res, err := rogueCA.IssueFor([]string{"edge.example"}, time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	addr := startProbeServer(t, tlsCertFromResult(t, res))

	svc, ctx := edgeProbeEnv(t, addr)
	enableACME(t, svc, ctx, "all")
	enableEdge(t, svc, ctx, IssuerModeSelfSigned, "edge.example")
	// The stored CA bundle is the TRUSTED one -- the presented leaf was signed
	// by a completely different (rogue) root, so it must never verify.
	if err := svc.settings.SetSystemSetting(ctx, certCACertKey, trustedCAPEM, svc.clock()); err != nil {
		t.Fatal(err)
	}

	dto, err := svc.ProbeEdgeTLS(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dto.OK {
		t.Fatal("want ok=false for a chain signed by a different CA")
	}
	if dto.Reason != edgeProbeUntrustedChain {
		t.Fatalf("reason = %q, want %q", dto.Reason, edgeProbeUntrustedChain)
	}
}

func TestProbeEdgeTLSReportsExpired(t *testing.T) {
	ca, caCertPEM, _, err := certissue.NewCA("Test CA", 24*365*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// now is 48h in the PAST, so a 1h leaf's NotAfter is ~47h behind the real
	// wall clock ProbeEdgeTLS actually verifies against.
	res, err := ca.IssueFor([]string{"edge.example"}, time.Hour, time.Now().Add(-48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	addr := startProbeServer(t, tlsCertFromResult(t, res))

	svc, ctx := edgeProbeEnv(t, addr)
	enableACME(t, svc, ctx, "all")
	enableEdge(t, svc, ctx, IssuerModeSelfSigned, "edge.example")
	if err := svc.settings.SetSystemSetting(ctx, certCACertKey, caCertPEM, svc.clock()); err != nil {
		t.Fatal(err)
	}

	dto, err := svc.ProbeEdgeTLS(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dto.OK {
		t.Fatal("want ok=false for an expired certificate")
	}
	if dto.Reason != edgeProbeExpired {
		t.Fatalf("reason = %q, want %q", dto.Reason, edgeProbeExpired)
	}
}

func TestProbeEdgeTLSReportsOKForAValidSelfSignedChain(t *testing.T) {
	ca, caCertPEM, _, err := certissue.NewCA("Test CA", 24*365*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	res, err := ca.IssueFor([]string{"edge.example"}, 90*24*time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	addr := startProbeServer(t, tlsCertFromResult(t, res))

	svc, ctx := edgeProbeEnv(t, addr)
	enableACME(t, svc, ctx, "all")
	enableEdge(t, svc, ctx, IssuerModeSelfSigned, "edge.example")
	if err := svc.settings.SetSystemSetting(ctx, certCACertKey, caCertPEM, svc.clock()); err != nil {
		t.Fatal(err)
	}

	dto, err := svc.ProbeEdgeTLS(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !dto.OK {
		t.Fatalf("want ok=true, got reason=%q message=%q", dto.Reason, dto.Message)
	}
	if dto.ExpectedName != "edge.example" {
		t.Fatalf("expected_name = %q, want edge.example", dto.ExpectedName)
	}
	if dto.Subject == "" || dto.NotAfter == nil {
		t.Fatalf("a successful probe must still describe what it found: subject=%q not_after=%v", dto.Subject, dto.NotAfter)
	}
}

// TestEdgeProbeAnchorNeverFallsBackToNilForSelfSigned is the actual
// mutation-proof for the "self_signed silently falls back to the system
// trust store" review finding (Important 1). It cannot be proven at the
// network/TLS level: doing so would require a certificate that genuinely
// verifies against the REAL system trust store, which a hermetic test has
// no way to forge (it would need the private key of an actual public root
// CA). edgeProbeAnchor is the exact, isolated decision point the fix
// touches, so this test asserts its return value directly.
func TestEdgeProbeAnchorNeverFallsBackToNilForSelfSigned(t *testing.T) {
	// The exact failure state Important 1 described: self_signed mode, no
	// usable CA bundle (e.g. ensureCA never ran). The pool must be non-nil
	// AND empty -- an empty pool can never successfully verify anything, so
	// Verify fails deterministically instead of silently trying the system
	// store.
	pool := edgeProbeAnchor(IssuerModeSelfSigned, "")
	if pool == nil {
		t.Fatal("edgeProbeAnchor(self_signed, \"\") returned nil -- Verify would silently fall back to the system trust store, which is exactly the bug this guards against")
	}
	empty := x509.NewCertPool()
	if !pool.Equal(empty) {
		t.Fatalf("edgeProbeAnchor(self_signed, \"\") must return an EMPTY pool, got one that is not equal to a fresh empty pool")
	}

	// Self-signed WITH a real stored bundle: the pool must actually contain
	// it (the fix must not degrade the normal, working case).
	_, caCertPEM, _, err := certissue.NewCA("Test CA", 24*365*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	pool = edgeProbeAnchor(IssuerModeSelfSigned, caCertPEM)
	want := x509.NewCertPool()
	want.AppendCertsFromPEM([]byte(caCertPEM))
	if pool == nil || !pool.Equal(want) {
		t.Fatalf("edgeProbeAnchor(self_signed, <bundle>) did not return a pool containing exactly that CA")
	}

	// acme (or any non-self_signed mode) must stay nil -- that IS the
	// intended system-trust-store fallback, and must not be broken by this
	// fix.
	if got := edgeProbeAnchor(IssuerModeACME, ""); got != nil {
		t.Fatalf("edgeProbeAnchor(acme, \"\") = %v, want nil (the deliberate system-trust-store fallback)", got)
	}
}

// TestProbeEdgeTLSSelfSignedWithNoStoredCABundleNeverReportsOK exercises the
// same failure state end-to-end through the real TLS dial: self_signed mode
// configured, but no CA bundle stored at all (as if ensureCA never ran).
// The presented leaf here is otherwise perfectly valid and correctly named
// -- only the missing anchor is wrong -- and the probe must still refuse to
// report ok. This does not by itself distinguish "nil" from "a non-nil
// empty pool" (see the note on TestEdgeProbeAnchorNeverFallsBackToNilForSelfSigned
// for why that specific distinction can only be proven in isolation), but it
// does pin the end-to-end behavior of the exact scenario Important 1
// described.
func TestProbeEdgeTLSSelfSignedWithNoStoredCABundleNeverReportsOK(t *testing.T) {
	ca, _, _, err := certissue.NewCA("Test CA", 24*365*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	res, err := ca.IssueFor([]string{"edge.example"}, 90*24*time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	addr := startProbeServer(t, tlsCertFromResult(t, res))

	svc, ctx := edgeProbeEnv(t, addr)
	enableACME(t, svc, ctx, "all")
	enableEdge(t, svc, ctx, IssuerModeSelfSigned, "edge.example")
	// Deliberately NOT storing any CA bundle.

	dto, err := svc.ProbeEdgeTLS(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dto.OK {
		t.Fatal("want ok=false: self_signed mode with no stored CA bundle must never report ok, no matter what nginx happens to still be serving")
	}
	if dto.Reason != edgeProbeUntrustedChain {
		t.Fatalf("reason = %q, want %q", dto.Reason, edgeProbeUntrustedChain)
	}
}

func TestProbeEdgeTLSInACMEModeUsesTheSystemTrustStore(t *testing.T) {
	ca, _, _, err := certissue.NewCA("Test CA", 24*365*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	res, err := ca.IssueFor([]string{"edge.example"}, 90*24*time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	addr := startProbeServer(t, tlsCertFromResult(t, res))

	svc, ctx := edgeProbeEnv(t, addr)
	enableACME(t, svc, ctx, "all")
	enableEdge(t, svc, ctx, IssuerModeACME, "edge.example")
	// Deliberately NOT storing this test CA as the internal CA bundle: an acme
	// edge mode must verify against the SYSTEM trust store only, never the
	// internal CA (which is independent and may not even exist).

	dto, err := svc.ProbeEdgeTLS(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dto.OK {
		t.Fatal("want ok=false: a throwaway test CA is never in the real system trust store")
	}
	if dto.Reason != edgeProbeUntrustedChain {
		t.Fatalf("reason = %q, want %q", dto.Reason, edgeProbeUntrustedChain)
	}
}
