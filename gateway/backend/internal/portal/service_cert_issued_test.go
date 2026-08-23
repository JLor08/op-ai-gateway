// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/certissue"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// certIssuedEnv mirrors certEnv (service_certificates_test.go) but wires an
// OnCertificateIssued hook, which certEnv cannot do because the hook must be
// present at NewService time -- before the reconcile pass under test. Mirrors
// certTriggerEnv in service_cert_trigger_test.go for the analogous
// OnCertSettingsChanged hook.
func certIssuedEnv(t *testing.T, hook func(serverID, fingerprint string)) (*Service, context.Context) {
	t.Helper()
	svc := NewService(ServiceDeps{
		Routes:              routing.NewMemoryStore(),
		SystemSettings:      NewMemorySystemSettings(),
		SettingsVolatile:    true,
		ACMEChallenges:      certissue.NewMemoryChallengeStore(),
		OnCertificateIssued: hook,
	})
	return svc, context.Background()
}

// certIssuedCall records one OnCertificateIssued invocation.
type certIssuedCall struct {
	serverID    string
	fingerprint string
}

// TestIssueAndStoreFiresOnCertificateIssuedOnlyForServerRows is the Phase 2
// distribution doorbell's core contract, proven with a SPY (not by inspecting
// the Kind clause in isolation, which is not mutation-falsifiable: only a
// "server" row ever carries a non-empty ServerID, so ServerID != "" alone
// already selects exactly the right rows and Kind=="server" is defensive
// belt-and-braces -- see the comment at the call site). One reconcile pass
// wants all four certificate kinds at once (gateway/server/public/edge); the
// hook must fire EXACTLY ONCE, for the server row, carrying that row's ACTUAL
// stored fingerprint, and must NOT fire for the other three.
func TestIssueAndStoreFiresOnCertificateIssuedOnlyForServerRows(t *testing.T) {
	var calls []certIssuedCall
	svc, ctx := certIssuedEnv(t, func(serverID, fingerprint string) {
		calls = append(calls, certIssuedCall{serverID, fingerprint})
	})

	on := true
	mode := IssuerModeACME
	email := "ops@example.test"
	base := "int.example.test"
	gw := "gw.int.example.test"
	scope := "all"
	managePublic := true
	publicDomains := []string{"public.example.test"}
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{
		CertEnabled:            &on,
		CertIssuerMode:         &mode,
		ACMEEmail:              &email,
		CertBaseDomain:         &base,
		CertGatewayDomain:      &gw,
		CertServerScope:        &scope,
		CertManagePublicDomain: &managePublic,
		CertPublicDomains:      &publicDomains,
	}); err != nil {
		t.Fatalf("enable: %v", err)
	}
	enableEdge(t, svc, ctx, IssuerModeSelfSigned, "edge.int.example.test")
	mustCreateNetbirdServer(t, svc, ctx, "srv-hook", "server.int.example.test", "")

	svc.cert.issuer = func(_ context.Context, _ CertSettings, want desiredCert) (certissue.Result, error) {
		return selfSigned(t, want.Domain, time.Now().UTC(), 90*24*time.Hour), nil
	}

	svc.ReconcileCertificates(ctx)

	if len(calls) != 1 {
		t.Fatalf("OnCertificateIssued fired %d times, want exactly 1 (the server row only), got %+v", len(calls), calls)
	}
	if calls[0].serverID != "srv-hook" {
		t.Fatalf("serverID = %q, want %q", calls[0].serverID, "srv-hook")
	}
	got, err := svc.routes.CertificateByDomain(ctx, "server.int.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if got.Fingerprint == "" {
		t.Fatal("precondition: the server certificate must actually have been stored with a fingerprint")
	}
	if calls[0].fingerprint != got.Fingerprint {
		t.Fatalf("fingerprint = %q, want the server row's actual stored fingerprint %q", calls[0].fingerprint, got.Fingerprint)
	}
}

// TestIssueAndStoreCertificateIssuedHookNilIsNoOp: the hook is optional --
// every pre-existing certificate-test fixture (certEnv et al.) omits it. A
// successful server-certificate issuance must still succeed with a nil hook
// rather than nil-panic the whole reconcile pass.
func TestIssueAndStoreCertificateIssuedHookNilIsNoOp(t *testing.T) {
	svc, ctx := certIssuedEnv(t, nil)
	enableACME(t, svc, ctx, "all")
	mustCreateNetbirdServer(t, svc, ctx, "srv-nil-hook", "server.int.example.test", "")
	svc.cert.issuer = func(_ context.Context, _ CertSettings, want desiredCert) (certissue.Result, error) {
		return selfSigned(t, want.Domain, time.Now().UTC(), 90*24*time.Hour), nil
	}

	svc.ReconcileCertificates(ctx) // must not panic with a nil hook

	got, err := svc.routes.CertificateByDomain(ctx, "server.int.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "active" {
		t.Fatalf("status = %q, want active -- the nil hook must not have broken the reconcile pass itself", got.Status)
	}
}

// TestIssueAndStoreDoesNotFireOnCertificateIssuedOnFailure: a failed order (or
// any other failure path through issueAndStore) must never fire the doorbell
// -- there is nothing new for the agent to fetch, and firing it would be a
// false signal.
func TestIssueAndStoreDoesNotFireOnCertificateIssuedOnFailure(t *testing.T) {
	var calls []certIssuedCall
	svc, ctx := certIssuedEnv(t, func(serverID, fingerprint string) {
		calls = append(calls, certIssuedCall{serverID, fingerprint})
	})
	enableACME(t, svc, ctx, "all")
	mustCreateNetbirdServer(t, svc, ctx, "srv-fail", "server.int.example.test", "")
	svc.cert.issuer = func(context.Context, CertSettings, desiredCert) (certissue.Result, error) {
		return certissue.Result{}, errors.New("boom")
	}

	svc.ReconcileCertificates(ctx)

	if len(calls) != 0 {
		t.Fatalf("OnCertificateIssued fired on a FAILED issuance: %+v", calls)
	}
}
