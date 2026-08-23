// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/certissue"
	"op-ai-gateway/internal/routing"
	"sync/atomic"
	"testing"
	"time"
)

// certTriggerEnv mirrors certEnv (memory routing store + VOLATILE settings store
// so sealCertSecret takes the "plain:" branch and no encryption key is needed)
// but wires an OnCertSettingsChanged hook, which certEnv cannot do because the
// hook must be present at NewService time — before the settings PUT under test.
func certTriggerEnv(t *testing.T, hook func()) (*Service, context.Context) {
	t.Helper()
	svc := NewService(ServiceDeps{
		Routes:                routing.NewMemoryStore(),
		SystemSettings:        NewMemorySystemSettings(),
		SettingsVolatile:      true,
		ACMEChallenges:        certissue.NewMemoryChallengeStore(),
		OnCertSettingsChanged: hook,
	})
	return svc, context.Background()
}

// TestUpdateSystemSettingsTriggersCertReconcileOnlyOnCertChange pins the gating:
// the settings PUT is shared with ~30 unrelated fields, so the certificate
// subsystem must be poked exactly when a cert_*/acme_* field is actually carried
// (UpdateSystemSettingsRequest.touchesCert) and never otherwise. A theme change
// must not kick a reconcile pass — that pass takes portal.Service's certMu and can
// place ACME orders.
func TestUpdateSystemSettingsTriggersCertReconcileOnlyOnCertChange(t *testing.T) {
	var calls atomic.Int32
	svc, ctx := certTriggerEnv(t, func() { calls.Add(1) })

	mode := IssuerModeSelfSigned
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CertIssuerMode: &mode}); err != nil {
		t.Fatalf("cert settings save: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("trigger fired %d times after a cert-relevant save, want exactly 1 -- "+
			"without it the stale cert_last_error note survives until the next ticker", got)
	}

	// An UNRELATED field must not fire it at all (the count must stay where it was).
	theme := "matrix"
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{Theme: &theme}); err != nil {
		t.Fatalf("unrelated settings save: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("trigger fired %d times total, want still 1 -- an unrelated settings save "+
			"must not kick a certificate reconcile pass", got)
	}
}

// TestUpdateSystemSettingsCertTriggerNilHookIsNoOp: the hook is optional (any
// deps literal that omits it, and every test fixture in this package, leaves it
// nil). A cert-relevant save must then still succeed — the periodic loop is the
// backstop — rather than nil-panic the whole settings endpoint.
func TestUpdateSystemSettingsCertTriggerNilHookIsNoOp(t *testing.T) {
	svc, ctx := certTriggerEnv(t, nil)

	on := true
	mode := IssuerModeSelfSigned
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		CertEnabled:    &on,
		CertIssuerMode: &mode,
	}); err != nil {
		t.Fatalf("cert settings save with a nil hook: %v", err)
	}
}

// TestUpdateSystemSettingsCertTriggerDoesNotClearTheNote is the deliberate
// NON-behaviour: the trigger must not touch cert_last_error itself. A pass stays
// the single writer of that value. Clearing it on save would show the operator
// "all fine" for the whole duration of the pass even when the configuration is
// still broken -- and if the trigger never lands (hook nil, buffer full, loop
// stopped), "all fine" forever.
func TestUpdateSystemSettingsCertTriggerDoesNotClearTheNote(t *testing.T) {
	// The hook is deliberately inert here: it stands in for "the trigger was
	// delivered but the pass has not run yet", which is the window this asserts.
	svc, ctx := certTriggerEnv(t, func() {})

	on := true
	acme := IssuerModeACME
	email := ""
	base := "int.example.test"
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		CertEnabled:    &on,
		CertIssuerMode: &acme,
		ACMEEmail:      &email,
		CertBaseDomain: &base,
	}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	svc.ReconcileCertificates(ctx) // writes the note (gate 1: no usable issuer mode)
	values, err := svc.settings.SystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if CertLastError(values) != certNoUsableIssuerMessage {
		t.Fatalf("precondition: the pass must have written the note, got %q", CertLastError(values))
	}

	selfSigned := IssuerModeSelfSigned
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CertIssuerMode: &selfSigned}); err != nil {
		t.Fatalf("switch mode: %v", err)
	}

	values, err = svc.settings.SystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := CertLastError(values); got != certNoUsableIssuerMessage {
		t.Fatalf("cert_last_error = %q after the save, want it UNCHANGED (%q) -- the save must "+
			"not clear the note; only a pass that got past the abort gates may",
			got, certNoUsableIssuerMessage)
	}
}

// TestCertSettingsChangeClearsStaleNoteWithoutWaitingForTheTicker is THE reported
// regression, end to end through the real settings PUT.
//
// An operator switched cert_issuer_mode to self_signed and the banner kept saying
// "certificate reconcile skipped: no issuer mode is fully configured -- the
// internal issuer mode is \"acme\" but acme_email is empty ...". That string is a
// static constant describing the condition that TRIGGERED it, so it kept
// asserting the exact state the operator had just fixed. The note is written and
// cleared only by ReconcileCertificates, and nothing made a settings change run
// one -- so it stood until the next tick of the reconcile loop (default 900s).
//
// The hook is wired EXACTLY as cmd/gateway wires it (non-blocking send on a
// buffered(1) channel), and the goroutine below stands in for the reconcile loop
// that consumes it. No ticker is involved anywhere in this test: if the chain
// PUT -> hook -> pass is broken at any link, the note is never cleared and this
// fails on its own bounded deadline.
func TestCertSettingsChangeClearsStaleNoteWithoutWaitingForTheTicker(t *testing.T) {
	trigger := make(chan struct{}, 1)
	svc, ctx := certTriggerEnv(t, func() {
		select {
		case trigger <- struct{}{}:
		default:
		}
	})
	// A stub issuer: this scenario must never place a real order. Reaching it at
	// all would mean the pass got FURTHER than the note-clearing point, which is
	// still past the gates -- but assert loudly rather than silently allow it.
	svc.cert.issuer = func(_ context.Context, _ CertSettings, want desiredCert) (certissue.Result, error) {
		t.Errorf("unexpected issuance attempt for %q", want.Domain)
		return certissue.Result{}, nil
	}

	// The broken state the operator was in: module on, internal mode acme, no
	// acme_email -> the FIRST abort gate fires and records the note.
	on := true
	acme := IssuerModeACME
	email := ""
	base := "int.example.test"
	gw := ""
	scope := "all"
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		CertEnabled:       &on,
		CertIssuerMode:    &acme,
		ACMEEmail:         &email,
		CertBaseDomain:    &base,
		CertGatewayDomain: &gw,
		CertServerScope:   &scope,
	}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	// Drain the trigger this setup save produced: the assertion below must be
	// carried by the CORRECTIVE save's own trigger, not by a leftover one.
	select {
	case <-trigger:
	default:
	}

	svc.ReconcileCertificates(ctx)
	values, err := svc.settings.SystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := CertLastError(values); got != certNoUsableIssuerMessage {
		t.Fatalf("precondition: a pass in the broken state must record the note, got %q", got)
	}

	// The stand-in for cmd/gateway's cert-reconcile loop: one goroutine, so a pass
	// can never overlap another.
	loopCtx, stopLoop := context.WithCancel(ctx)
	defer stopLoop()
	passes := make(chan struct{}, 8)
	go func() {
		for {
			select {
			case <-loopCtx.Done():
				return
			case <-trigger:
				svc.ReconcileCertificates(loopCtx)
				select {
				case passes <- struct{}{}:
				default:
				}
			}
		}
	}()

	// The corrective change the operator made.
	selfSigned := IssuerModeSelfSigned
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CertIssuerMode: &selfSigned}); err != nil {
		t.Fatalf("switch to self_signed: %v", err)
	}

	select {
	case <-passes:
	case <-time.After(10 * time.Second):
		t.Fatal("the corrective settings save never ran a reconcile pass -- the settings PUT " +
			"does not trigger one, so the stale note stands until the periodic ticker (900s)")
	}

	values, err = svc.settings.SystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := CertLastError(values); got != "" {
		t.Fatalf("cert_last_error = %q after switching the issuer mode to self_signed, want it "+
			"CLEARED -- the note must not outlive the change that fixed it", got)
	}
}
