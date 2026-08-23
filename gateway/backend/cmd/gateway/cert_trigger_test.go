// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"context"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/config"
	"op-ai-gateway/internal/portal"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// blockingCertReconciler is a certReconciler that blocks each pass until it is
// released, and records the MAXIMUM number of passes ever in flight at once —
// the property the trigger design must never break (the loop goroutine is the
// only caller of runCertPass).
type blockingCertReconciler struct {
	entered chan struct{} // one send per pass start
	release chan struct{} // one receive per pass, unblocking it

	mu       sync.Mutex
	inFlight int
	maxSeen  int
	total    int
}

func newBlockingCertReconciler(capacity int) *blockingCertReconciler {
	return &blockingCertReconciler{
		entered: make(chan struct{}, capacity),
		release: make(chan struct{}, capacity),
	}
}

func (b *blockingCertReconciler) ReconcileCertificates(context.Context) {
	b.mu.Lock()
	b.inFlight++
	b.total++
	if b.inFlight > b.maxSeen {
		b.maxSeen = b.inFlight
	}
	b.mu.Unlock()

	b.entered <- struct{}{}
	<-b.release

	b.mu.Lock()
	b.inFlight--
	b.mu.Unlock()
}

func (b *blockingCertReconciler) stats() (maxSeen, total int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.maxSeen, b.total
}

// awaitPass waits for one pass to START, or fails.
func (b *blockingCertReconciler) awaitPass(t *testing.T, what string) {
	t.Helper()
	select {
	case <-b.entered:
	case <-time.After(5 * time.Second):
		t.Fatalf("no reconcile pass started within 5s (%s)", what)
	}
}

// TestCertReconcileLoopRunsAnExtraPassOnTrigger: a send on the trigger channel
// must run a pass IMMEDIATELY, out of band from the ticker. The interval here is
// an hour, so a pass observed within seconds can only have come from the trigger
// — the whole point of the fix (the production default is 900s).
func TestCertReconcileLoopRunsAnExtraPassOnTrigger(t *testing.T) {
	b := newBlockingCertReconciler(8)
	trigger := make(chan struct{}, 1)
	cancel := startCertReconcileLoop(b, time.Hour, trigger)
	defer func() {
		cancel()
		close(b.release) // let any parked pass finish so the goroutine exits
	}()

	// The loop's own immediate startup pass.
	b.awaitPass(t, "startup pass")
	b.release <- struct{}{}

	certReconcileTriggerFunc(trigger)()
	b.awaitPass(t, "triggered pass -- the trigger channel is not consumed by the loop")
	b.release <- struct{}{}

	if _, total := b.stats(); total < 2 {
		t.Fatalf("ran %d passes, want >= 2 (startup + the triggered one)", total)
	}
}

// TestCertReconcileTriggerNeverBlocksAndNeverOverlapsPasses covers the two
// concurrency requirements at once, against a pass that is deliberately stuck:
//
//  1. The trigger func must return immediately even while a pass is in flight.
//     It is called INLINE from the settings PUT, and a pass holds portal.Service's
//     certMu for its whole duration (up to certPassTimeout, 10 min) and may place
//     ACME orders — so a blocking trigger would hang the operator's save.
//  2. Two saves in quick succession must not produce two overlapping passes, and
//     must not leave a queue of goroutines parked on certMu.
func TestCertReconcileTriggerNeverBlocksAndNeverOverlapsPasses(t *testing.T) {
	b := newBlockingCertReconciler(8)
	trigger := make(chan struct{}, 1)
	fire := certReconcileTriggerFunc(trigger)
	cancel := startCertReconcileLoop(b, time.Hour, trigger)
	defer func() {
		cancel()
		close(b.release)
	}()

	// Park the startup pass: from here on a pass is in flight and the loop
	// goroutine is busy — the worst case for both requirements.
	b.awaitPass(t, "startup pass")

	// Requirement 1: both sends return promptly with a pass stuck.
	done := make(chan struct{})
	go func() {
		fire()
		fire()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the settings-change trigger blocked while a reconcile pass was in flight -- " +
			"it is called inline from the settings PUT, so this would hang the operator's save")
	}

	// Requirement 2: release the stuck pass; the two rapid triggers must have
	// coalesced into at most ONE extra pass, and no two passes may ever overlap.
	b.release <- struct{}{}
	b.awaitPass(t, "the coalesced extra pass")
	b.release <- struct{}{}

	// Give any (incorrectly) queued third pass a chance to appear.
	select {
	case <-b.entered:
		t.Fatal("a third pass ran: two rapid saves must coalesce into at most one extra pass")
	case <-time.After(300 * time.Millisecond):
	}

	maxSeen, total := b.stats()
	if maxSeen != 1 {
		t.Fatalf("max concurrent reconcile passes = %d, want 1 -- passes must never overlap "+
			"(they contend on portal.Service's certMu)", maxSeen)
	}
	if total != 2 {
		t.Fatalf("ran %d passes, want exactly 2 (startup + one coalesced extra)", total)
	}
}

// TestCertReconcileTriggerFuncIsNonBlockingWhenNobodyListens: the hook must be
// safe even with no reader at all (a loop that already exited on shutdown). The
// buffer takes the first send, every further one is dropped — never a block, and
// never a panic that would surface as a failed settings PUT.
func TestCertReconcileTriggerFuncIsNonBlockingWhenNobodyListens(t *testing.T) {
	fire := certReconcileTriggerFunc(make(chan struct{}, 1))
	done := make(chan struct{})
	go func() {
		for range 5 {
			fire()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("certReconcileTriggerFunc blocked with no reader -- it must always drop instead")
	}
}

// TestCertReconcileLoopNilTriggerIsANoOp: trigger may be nil (a caller that wants
// no trigger; a receive on a nil channel simply never fires). The loop must still
// run its startup pass and its ticks rather than deadlock or panic.
func TestCertReconcileLoopNilTriggerIsANoOp(t *testing.T) {
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
	t.Fatalf("reconcile ran %d times with a nil trigger, want >= 3 (startup + ticks)", f.calls.Load())
}

// awaitCertNote polls the certificate CA view until want matches, or fails. Reads
// through the real portal.API the HTTP layer uses, so it observes exactly what the
// portal panel's banner would render.
func awaitCertNote(t *testing.T, api portal.API, want func(string) bool, what string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	last := "<never read>"
	for time.Now().Before(deadline) {
		ca, err := api.CertificateCAView(context.Background())
		if err == nil {
			last = ca.LastError
			if want(ca.LastError) {
				return
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("%s -- cert_last_error is still %q after 15s", what, last)
}

// TestMemoryDepsWiresCertSettingsChangeTrigger proves the memory driver's wiring
// end to end, INCLUDING the note being cleared again — the operator's actual
// complaint. The memory settings store is volatile, so sealing a CA key takes the
// "plain:" branch and the self_signed pass can get all the way past the gates.
func TestMemoryDepsWiresCertSettingsChangeTrigger(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_DEV_TOKEN", "")
	// A one-hour reconcile cadence: any observation within seconds below is the
	// TRIGGER, never the ticker (production default is 900s).
	cfg := config.Config{Addr: "127.0.0.1:8080", DBDriver: "memory", CertReconcileIntervalSeconds: 3600}
	deps, cleanup, err := memoryDeps(cfg)
	if err != nil {
		t.Fatalf("memoryDeps returned %v", err)
	}
	t.Cleanup(func() { _ = cleanup() })

	ctx := context.Background()
	on := true
	acme := portal.IssuerModeACME
	email := ""
	base := "int.example.test"
	gw := ""
	scope := "all"
	started := time.Now()
	if _, err := deps.Portal.UpdateSystemSettings(ctx, auth.Token{Scopes: []string{"system"}}, portal.UpdateSystemSettingsRequest{
		CertEnabled:       &on,
		CertIssuerMode:    &acme,
		ACMEEmail:         &email,
		CertBaseDomain:    &base,
		CertGatewayDomain: &gw,
		CertServerScope:   &scope,
	}); err != nil {
		t.Fatalf("enable certificates: %v", err)
	}
	awaitCertNote(t, deps.Portal, func(s string) bool { return s != "" },
		"enabling certificates in a broken shape never ran a reconcile pass -- "+
			"memoryDeps does not wire OnCertSettingsChanged into the cert-reconcile loop")

	// The corrective change: the note must go away without waiting for the ticker.
	selfSigned := portal.IssuerModeSelfSigned
	if _, err := deps.Portal.UpdateSystemSettings(ctx, auth.Token{Scopes: []string{"system"}}, portal.UpdateSystemSettingsRequest{
		CertIssuerMode: &selfSigned,
	}); err != nil {
		t.Fatalf("switch to self_signed: %v", err)
	}
	awaitCertNote(t, deps.Portal, func(s string) bool { return s == "" },
		"switching the issuer mode to self_signed never cleared the stale note")

	if elapsed := time.Since(started); elapsed >= time.Hour {
		t.Fatalf("took %v, i.e. long enough for the ticker to be the explanation", elapsed)
	}
}

// TestSqliteDepsWiresCertSettingsChangeTrigger is the same wiring proof for the
// sqlite driver. It asserts only that the note APPEARS: on a DISK store without
// OP_AI_GATEWAY_CERT_ENCRYPTION_KEY no private key can be sealed, so a
// self_signed pass legitimately keeps a note (certSealBlocked -> clearCertLastError
// re-asserts it) rather than clearing — that behaviour is not this test's subject.
// The note appearing at all is already only explicable by the trigger.
func TestSqliteDepsWiresCertSettingsChangeTrigger(t *testing.T) {
	cfg := config.Config{
		Addr:                         "127.0.0.1:8080",
		DBDriver:                     "sqlite",
		SQLitePath:                   filepath.Join(t.TempDir(), "gateway.db"),
		AutoMigrate:                  true,
		CertReconcileIntervalSeconds: 3600,
	}
	deps, cleanup, err := sqliteDeps(cfg)
	if err != nil {
		t.Fatalf("sqliteDeps returned %v", err)
	}
	t.Cleanup(func() { _ = cleanup() })

	on := true
	acme := portal.IssuerModeACME
	email := ""
	base := "int.example.test"
	gw := ""
	scope := "all"
	if _, err := deps.Portal.UpdateSystemSettings(context.Background(), auth.Token{Scopes: []string{"system"}}, portal.UpdateSystemSettingsRequest{
		CertEnabled:       &on,
		CertIssuerMode:    &acme,
		ACMEEmail:         &email,
		CertBaseDomain:    &base,
		CertGatewayDomain: &gw,
		CertServerScope:   &scope,
	}); err != nil {
		t.Fatalf("enable certificates: %v", err)
	}
	awaitCertNote(t, deps.Portal, func(s string) bool { return s != "" },
		"enabling certificates in a broken shape never ran a reconcile pass -- "+
			"sqliteDeps does not wire OnCertSettingsChanged into the cert-reconcile loop")
}

// TestAllThreeDriversWireCertSettingsChangeTrigger is the completeness guard the
// two behavioural tests above cannot give: postgres is the DEFAULT production
// driver and needs a live server, so it has no behavioural test here. A hook wired
// in only some driver paths is a real defect class in this file (see the
// postgresDeps cipher-wiring precedent), so this pins the count by source: the
// ONE portal.NewService call must pass OnCertSettingsChanged, and the ONE
// startCertReconcileLoop call must pass the channel that hook sends on.
//
// Since CMP-1, memoryDeps/sqliteDeps/postgresDeps share ONE body
// (buildRuntime, reached directly by memoryDeps and via sqlDeps by
// sqliteDeps/postgresDeps) instead of each inlining this wiring separately,
// so there is now exactly one portal.NewService call site rather than three;
// this test additionally pins each driver's call chain into that shared body
// so a driver that stopped reaching it would still be caught.
func TestAllThreeDriversWireCertSettingsChangeTrigger(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	body := string(src)

	newServiceCalls := strings.Count(body, "portal.NewService(portal.ServiceDeps{")
	if newServiceCalls != 1 {
		t.Fatalf("found %d portal.NewService call sites, expected 1 (buildRuntime, shared by memory/sqlite/postgres)", newServiceCalls)
	}

	if got := strings.Count(body, "OnCertSettingsChanged: certReconcileTriggerFunc(certReconcileTrigger)"); got != 1 {
		t.Fatalf("OnCertSettingsChanged is wired %d times, want 1 (buildRuntime) -- a certificate "+
			"settings change would silently do nothing", got)
	}
	if got := strings.Count(body, "certReconcileTrigger := make(chan struct{}, 1)"); got != 1 {
		t.Fatalf("certReconcileTrigger is declared %d times, want 1 (buildRuntime)", got)
	}
	if got := strings.Count(body, "startCertReconcileLoop(portalService, time.Duration(cfg.CertReconcileIntervalSeconds)*time.Second, certReconcileTrigger)"); got != 1 {
		t.Fatalf("the cert-reconcile loop is fed the trigger channel %d times, want 1 (buildRuntime) -- "+
			"otherwise the hook sends into a channel nothing ever reads", got)
	}

	assertAllDriversReachBuildRuntime(t)
}
