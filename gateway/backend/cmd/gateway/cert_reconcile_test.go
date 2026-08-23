// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/config"
	"op-ai-gateway/internal/gateway"
	"op-ai-gateway/internal/portal"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// fakeCertReconciler is a certReconciler stub that counts calls, letting the
// tests below observe the loop's cadence without a real portal.Service.
type fakeCertReconciler struct{ calls atomic.Int32 }

func (f *fakeCertReconciler) ReconcileCertificates(context.Context) { f.calls.Add(1) }

func TestCertReconcileLoopRunsImmediatelyThenOnTheTick(t *testing.T) {
	f := &fakeCertReconciler{}
	cancel := startCertReconcileLoop(f, 20*time.Millisecond, nil)
	defer cancel()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if f.calls.Load() >= 3 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("reconcile ran %d times, want >= 3 (immediate + ticks)", f.calls.Load())
}

// TestCertReconcileLoopStopsOnCancel proves two things, in order: (1) the
// loop actually DOES work -- a positive precondition, waited for via polling
// rather than a fixed sleep -- and only then (2) that cancel() stops further
// work. Without the precondition, an emptied loop body (e.g. a mutation that
// deletes runCertPass's call, or a broken ctx.Done() check that returns
// immediately) would leave calls at 0 both before AND after cancel(), and
// "0 == 0" would pass the no-further-growth assertion vacuously -- exactly
// the failure mode a bare before/after comparison can't catch.
func TestCertReconcileLoopStopsOnCancel(t *testing.T) {
	f := &fakeCertReconciler{}
	cancel := startCertReconcileLoop(f, 5*time.Millisecond, nil)

	// Positive precondition: poll (condition-based, no fixed "warm-up" sleep)
	// until the loop has actually run the reconciler a few times. Requiring
	// more than one call also rules out a mutation that runs the loop's
	// startup pass but breaks the ticker (e.g. a body that returns instead of
	// looping) -- that would produce a single call and then look identical to
	// "stopped" to the check below.
	const minCallsBeforeCancel = 3
	precondDeadline := time.Now().Add(2 * time.Second)
	for f.calls.Load() < minCallsBeforeCancel {
		if time.Now().After(precondDeadline) {
			t.Fatalf("reconciler was called %d times within 2s, want >= %d before cancel -- "+
				"the loop may not be running its body at all", f.calls.Load(), minCallsBeforeCancel)
		}
		time.Sleep(1 * time.Millisecond)
	}
	before := f.calls.Load()
	cancel()

	// The "no further growth" half legitimately needs a bounded OBSERVATION
	// window: there is no condition to poll for when proving something does
	// NOT happen, so a fixed sleep is the right tool here (unlike the
	// polling precondition above, which waits for something that DOES
	// happen and so can return as soon as it's true).
	time.Sleep(100 * time.Millisecond)
	if after := f.calls.Load(); after != before {
		t.Fatalf("loop kept running after cancel: %d -> %d", before, after)
	}
}

// assertACMEChallengeStoreWired is a narrow structural check: it catches a
// driver that leaves ServerDeps.ACMEChallenges nil outright, and it proves
// that gateway.New(deps) really wires deps.ACMEChallenges into the public
// /.well-known/acme-challenge/ handler (not e.g. a stale/ignored field).
//
// It does NOT prove that portal.ServiceDeps.ACMEChallenges received the SAME
// instance as ServerDeps.ACMEChallenges -- it writes directly into the deps
// value and reads back through the gateway built from that same value, so a
// driver that hands portal.ServiceDeps a SECOND, distinct
// certissue.NewMemoryChallengeStore() while ServerDeps keeps the original
// would still pass this check (deps.ACMEChallenges is untouched by that
// mutation). That divergent-wiring failure mode -- the portal PUTs a token's
// key authorization into one map while the public handler reads an empty one,
// so every ACME order 404s silently -- is caught only by
// TestMemoryDepsPortalAndGatewaySharesOneACMEChallengeStore below, which
// drives a REAL order through portal.Service.ReconcileCertificates end to end.
func assertACMEChallengeStoreWired(t *testing.T, deps gateway.ServerDeps) {
	t.Helper()
	if deps.ACMEChallenges == nil {
		t.Fatal("ServerDeps.ACMEChallenges is nil, want the shared challenge store")
	}
	const token = "wiring-check-token"
	const keyAuth = "wiring-check-token.thumbprint-xyz"
	deps.ACMEChallenges.Put(token, keyAuth)

	srv := gateway.New(deps)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/acme-challenge/"+token, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /.well-known/acme-challenge/%s = %d, want 200 (body %q)", token, rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != keyAuth {
		t.Fatalf("challenge response body = %q, want the exact key authorization %q", got, keyAuth)
	}
}

func TestMemoryDepsWiresSharedACMEChallengeStore(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_DEV_TOKEN", "")
	cfg := config.Config{Addr: "127.0.0.1:8080", DBDriver: "memory"}
	deps, cleanup, err := memoryDeps(cfg)
	if err != nil {
		t.Fatalf("memoryDeps returned %v", err)
	}
	t.Cleanup(func() { _ = cleanup() })
	assertACMEChallengeStoreWired(t, deps)
}

func TestSqliteDepsWiresSharedACMEChallengeStore(t *testing.T) {
	cfg := config.Config{
		Addr:        "127.0.0.1:8080",
		DBDriver:    "sqlite",
		SQLitePath:  filepath.Join(t.TempDir(), "gateway.db"),
		AutoMigrate: true,
	}
	deps, cleanup, err := sqliteDeps(cfg)
	if err != nil {
		t.Fatalf("sqliteDeps returned %v", err)
	}
	t.Cleanup(func() { _ = cleanup() })
	assertACMEChallengeStoreWired(t, deps)
}

// TestMemoryDepsPortalAndGatewaySharesOneACMEChallengeStore is the honest,
// end-to-end proof that memoryDeps hands the SAME certissue.ChallengeStore
// instance to portal.ServiceDeps.ACMEChallenges (the PUT side, written by a
// real in-flight ACME order) and gateway.ServerDeps.ACMEChallenges (the GET
// side, read by the public /.well-known/acme-challenge/{token} handler).
//
// It drives the real production call chain -- portal.Service.
// ReconcileCertificates -> issueCertificate -> a real certissue.ACMEClient.
// Obtain -- against a fake ACME directory (copied test-only code from
// internal/certissue/fakedir_test.go, see acme_fakedir_test.go) whose
// challenge-validation step makes a REAL HTTP GET to the gateway's OWN public
// handler for the key authorization the order just wrote. That GET can only
// succeed if the store the portal wrote into and the store the gateway reads
// from are the identical instance.
//
// The mutation this is built to catch: give portal.ServiceDeps a SECOND,
// distinct certissue.NewMemoryChallengeStore() in memoryDeps while
// gateway.ServerDeps keeps the original. Under that mutation the portal
// writes the token into its own private map, the public handler's map stays
// empty, the fake directory's challenge fetch 404s, authzOK never flips true,
// and this test fails -- with nothing else (no log, no error surfaced to any
// caller) pointing at the cause, exactly as the brief describes.
//
// The test does not require the order to fully complete (issuance is not
// asserted) -- reaching a validated HTTP-01 challenge is the proof of a
// shared store, and asserting only that keeps the test from being brittle
// against unrelated ACME-flow details (finalize/cert-download quirks of the
// fake directory).
func TestMemoryDepsPortalAndGatewaySharesOneACMEChallengeStore(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_DEV_TOKEN", "")
	cfg := config.Config{Addr: "127.0.0.1:8080", DBDriver: "memory"}
	deps, cleanup, err := memoryDeps(cfg)
	if err != nil {
		t.Fatalf("memoryDeps returned %v", err)
	}
	t.Cleanup(func() { _ = cleanup() })

	// The REAL gateway server, serving the public /.well-known/acme-challenge/
	// route off deps.ACMEChallenges -- exactly as it would in production.
	gwSrv := httptest.NewServer(gateway.New(deps))
	t.Cleanup(gwSrv.Close)

	// The fake directory's /chal/1 handler GETs
	// <challengeBase>/.well-known/acme-challenge/<token> -- point it at the
	// real gateway server above, not a bespoke test double.
	fd := newFakeDir(t, gwSrv.URL)

	// Bounded: under the divergent-store mutation this test is built to catch,
	// the challenge validation never succeeds and the ACME client's
	// WaitAuthorization polls indefinitely on an unbounded context -- a
	// timeout here turns that into a fast, clean test failure (a canceled
	// order, reported as a normal issuance failure by ReconcileCertificates)
	// instead of a multi-minute hang killed by the test runner's own timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	enabled := true
	email := "acme-test@example.test"
	directoryURL := fd.base() + "/directory"
	baseDomain := "int.example.test"
	scope := "all"
	publicDomains := []string{"public.example.test"}
	// Smallest configuration that makes ReconcileCertificates place exactly one
	// order: a "public" desired-domain entry bypasses the ACME
	// under-base-domain check (see desiredCertificates/ReconcileCertificates in
	// service_certificates.go), so no NetBird server/gateway-peer setup is
	// needed -- cert_base_domain only has to be non-empty to pass the
	// module-usable gate.
	if _, err := deps.Portal.UpdateSystemSettings(ctx, auth.Token{Scopes: []string{"system"}}, portal.UpdateSystemSettingsRequest{
		CertEnabled:            &enabled,
		ACMEEmail:              &email,
		ACMEDirectoryURL:       &directoryURL,
		CertBaseDomain:         &baseDomain,
		CertServerScope:        &scope,
		CertManagePublicDomain: &enabled,
		CertPublicDomains:      &publicDomains,
	}); err != nil {
		t.Fatalf("UpdateSystemSettings: %v", err)
	}

	deps.Portal.ReconcileCertificates(ctx)

	if !fd.authzOK {
		t.Fatal("fake ACME directory never observed a successful HTTP-01 challenge " +
			"fetch through the gateway's public handler -- portal.ServiceDeps." +
			"ACMEChallenges and gateway.ServerDeps.ACMEChallenges are not the same " +
			"instance (or one of them is nil)")
	}
}

// TestCertReconcileLoopBoundsAStalledACMEPass is the fix-round-2 regression: it
// proves runCertPass's context.WithTimeout (certPassDeadline, wrapping every
// call to portal.Service.ReconcileCertificates) actually bounds a pass whose
// ACME authorization never resolves, instead of letting the reconcile
// goroutine -- and the portal.Service.certMu it holds for the pass's whole
// duration -- hang forever.
//
// It drives the loop through startCertReconcileLoop (the real production
// entry point in main.go, not a bare ReconcileCertificates call) against a
// fake ACME directory whose /authz/1 endpoint reports "pending" FOREVER
// (fakeDir.neverValidate, see acme_fakedir_test.go) -- modeling exactly the
// "authorization that stays pending forever" failure mode the fix targets. A
// SHORT loop interval is injected (not the production 900s/10-min constants)
// so certPassDeadline (= min(certPassTimeout, interval)) bounds each pass to
// a fraction of a second, keeping the test fast while proving the identical
// code path that governs the real 10-minute bound in production.
//
// The assertion is behavioral, not a mock call count: within a bounded wait,
// the certificate row for the desired domain must show status "error" (i.e.
// ReconcileCertificates actually RETURNED and recorded a failure via
// recordCertFailure) rather than never appearing at all. Without the fix (a
// bare r.ReconcileCertificates(ctx) using the loop's own long-lived,
// never-expiring context), the underlying x/crypto/acme WaitAuthorization
// poll loop never sees the authorization leave "pending" and never returns,
// so no row is ever written within the wait window and this test times out
// via t.Fatal -- a clean failure, not a hang, because the TEST's own wait
// loop is itself bounded (see the loop below); this was verified directly
// (see the fix report) by re-running this exact test against a build where
// runCertPass's context.WithTimeout was removed.
func TestCertReconcileLoopBoundsAStalledACMEPass(t *testing.T) {
	// certPassMinDeadline (production default 2 minutes, see cert_reconcile.go)
	// floors every pass's deadline regardless of how short the loop interval
	// is -- lower it here so this test's short interval below still produces a
	// short per-pass bound, keeping the test fast; certPassDeadline itself is
	// exercised at its real production value by TestCertPassDeadline below.
	prevMinDeadline := certPassMinDeadline
	certPassMinDeadline = 200 * time.Millisecond
	t.Cleanup(func() { certPassMinDeadline = prevMinDeadline })

	t.Setenv("OP_AI_GATEWAY_DEV_TOKEN", "")
	// A SHORT interval for memoryDeps' OWN cert-reconcile loop, which this test
	// unavoidably runs alongside the explicit one it starts below. It matters
	// because the UpdateSystemSettings call further down now TRIGGERS that internal
	// loop (portal.ServiceDeps.OnCertSettingsChanged), and a triggered pass is
	// bounded by certPassDeadline(ITS OWN interval) -- left at the
	// certReconcileMinInterval floor (1 minute) that stalled pass would hold
	// portal.Service's certMu for a whole minute, and every pass of the test's own
	// loop would block on that uninterruptible lock instead of recording the
	// bounded failure this test waits for. One second keeps BOTH loops' passes
	// short, so the property under test (a stalled pass is deadline-bounded and
	// records a failure) is still what decides the outcome. Removing runCertPass's
	// context.WithTimeout still hangs both loops forever and still fails here.
	cfg := config.Config{Addr: "127.0.0.1:8080", DBDriver: "memory", CertReconcileIntervalSeconds: 1}
	deps, cleanup, err := memoryDeps(cfg)
	if err != nil {
		t.Fatalf("memoryDeps returned %v", err)
	}
	t.Cleanup(func() { _ = cleanup() })

	gwSrv := httptest.NewServer(gateway.New(deps))
	t.Cleanup(gwSrv.Close)

	fd := newFakeDir(t, gwSrv.URL)
	fd.neverValidate = true

	setupCtx, cancelSetup := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelSetup()
	enabled := true
	email := "acme-test@example.test"
	directoryURL := fd.base() + "/directory"
	baseDomain := "int.example.test"
	scope := "all"
	publicDomains := []string{"public.example.test"}
	if _, err := deps.Portal.UpdateSystemSettings(setupCtx, auth.Token{Scopes: []string{"system"}}, portal.UpdateSystemSettingsRequest{
		CertEnabled:            &enabled,
		ACMEEmail:              &email,
		ACMEDirectoryURL:       &directoryURL,
		CertBaseDomain:         &baseDomain,
		CertServerScope:        &scope,
		CertManagePublicDomain: &enabled,
		CertPublicDomains:      &publicDomains,
	}); err != nil {
		t.Fatalf("UpdateSystemSettings: %v", err)
	}

	// A SHORT interval, injected only for test speed: certPassDeadline caps
	// the pass timeout at min(certPassTimeout, interval), so this bounds the
	// stalled pass to well under a second -- production behavior is governed
	// by the SAME certPassDeadline function, just with the real (900s/10min)
	// constants, per the coordinator's "keep production behaviour on the
	// constant" instruction: no injectable timeout was added, only a short
	// interval, since certPassDeadline already derives the bound from it.
	const shortInterval = 300 * time.Millisecond
	cancelLoop := startCertReconcileLoop(deps.Portal, shortInterval, nil)
	defer cancelLoop()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		certs, err := deps.Portal.CertificatesView(context.Background())
		if err == nil {
			for _, c := range certs {
				if c.Domain == "public.example.test" && c.Status == "error" {
					if c.LastError == "" {
						t.Fatal("certificate row status=error but LastError is empty")
					}
					return // pass bounded + failure recorded: the fix works.
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("reconcile pass never recorded a bounded failure for the stalled ACME " +
		"order within 10s -- the pass may be hanging (certPassDeadline not bounding it)")
}

// TestCertPassDeadline pins certPassDeadline's exact behavior at the REAL
// production constants (certPassTimeout=10min, certPassMinDeadline=2min): a
// pure, fast unit test independent of the loop/goroutine machinery above.
//
// The certPassMinDeadline floor (fix M1) exists because a pass cut off at a
// too-short deadline can be cancelled AFTER an ACME order already succeeded
// but BEFORE the certificate is persisted -- the next pass then re-orders the
// same domain and burns Let's Encrypt's weekly duplicate-certificate limit.
// Without the floor, a short reconcile interval (the config floor is 60s)
// would produce an equally short per-pass deadline; this test proves that can
// no longer happen.
func TestCertPassDeadline(t *testing.T) {
	cases := []struct {
		name     string
		interval time.Duration
		want     time.Duration
	}{
		{"zero interval falls back to certPassTimeout", 0, certPassTimeout},
		{"negative interval falls back to certPassTimeout", -5 * time.Second, certPassTimeout},
		{"interval at the config floor (60s) is raised to certPassMinDeadline", 60 * time.Second, certPassMinDeadline},
		{"interval just below certPassMinDeadline is raised to it", certPassMinDeadline - time.Second, certPassMinDeadline},
		{"interval exactly at certPassMinDeadline passes through unchanged", certPassMinDeadline, certPassMinDeadline},
		{"interval between the floor and the ceiling passes through unchanged", 5 * time.Minute, 5 * time.Minute},
		{"interval above certPassTimeout is capped at it", 20 * time.Minute, certPassTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := certPassDeadline(tc.interval); got != tc.want {
				t.Fatalf("certPassDeadline(%s) = %s, want %s", tc.interval, got, tc.want)
			}
		})
	}
	// The floor must never itself be non-positive, or context.WithTimeout would
	// expire the pass immediately regardless of the interval passed in.
	if certPassMinDeadline <= 0 {
		t.Fatalf("certPassMinDeadline = %s, want > 0", certPassMinDeadline)
	}
}
