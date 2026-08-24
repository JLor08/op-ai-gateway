// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/json"
	"errors"
	"op-ai-gateway/internal/store"
	"testing"
	"time"
)

// The unknown-model redirect settings on the SERVICE-token path. The settings
// themselves, their validation rule and the set they are validated against are
// shared with the user-token path (see service_token_test.go); what these tests
// pin is that the service path is wired to them at all, plus the one thing that
// is genuinely different here — which principal's reach the fallback is checked
// against.

// seedServiceTokenRecord writes a service-token record straight through the
// store. The marker it seeds is not settable through the API by design, so a
// fixture for it cannot go through the API.
func seedServiceTokenRecord(t *testing.T, dir *MemoryDirectory, serviceID, id, lastUsedModel string, now time.Time) {
	t.Helper()
	if err := dir.CreatePlainToken(context.Background(), store.TokenRecord{
		ID: id, ServiceID: serviceID, Kind: store.TokenKindService, Name: id,
		Status: store.TokenStatusActive, Scopes: `["llm:invoke"]`,
		LastUsedModel: lastUsedModel, CreatedAt: now, UpdatedAt: now,
	}, "secret-"+id); err != nil {
		t.Fatalf("CreatePlainToken %s: %v", id, err)
	}
}

func TestCreateServiceTokenRejectsUnknownFallback(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := newServiceAccountsTestService(t, time.Now().UTC())
	created := createTestService(t, ctx, svc)
	_, err := svc.CreateServiceToken(ctx, svcAdminToken(), created.ID, CreateServiceTokenRequest{
		Name: "T1", UnknownModelRedirect: true, UnknownModelFallback: "no-such-model",
	})
	if !errors.Is(err, ErrTokenModelOverrideInvalid) {
		t.Fatalf("err = %v, want ErrTokenModelOverrideInvalid", err)
	}
}

func TestCreateServiceTokenAcceptsGroupAsFallback(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	svc, _, _, rs := newServiceAccountsTestService(t, now)
	offerGroup(t, rs, "grp_svc_fast", "fast-group", "model-a")
	created := createTestService(t, ctx, svc)
	resp, err := svc.CreateServiceToken(ctx, svcAdminToken(), created.ID, CreateServiceTokenRequest{
		Name: "T1", UnknownModelRedirect: true, UnknownModelRedirectBlocked: true, UnknownModelFallback: "fast-group",
	})
	if err != nil {
		t.Fatalf("group fallback rejected: %v", err)
	}
	if !resp.Token.UnknownModelRedirect || !resp.Token.UnknownModelRedirectBlocked || resp.Token.UnknownModelFallback != "fast-group" {
		t.Fatalf("token dto = %+v", resp.Token)
	}
	list, err := svc.ListServiceTokens(ctx, svcAdminToken(), created.ID)
	if err != nil {
		t.Fatalf("ListServiceTokens: %v", err)
	}
	if len(list) != 1 || list[0].UnknownModelFallback != "fast-group" || !list[0].UnknownModelRedirect {
		t.Fatalf("listed token = %+v", list)
	}
}

// TestCreateServiceTokenFallbackIsCheckedAgainstThePrincipal pins the ruling
// for this path: the fallback is validated against the CREATING PRINCIPAL's
// callable set — exactly like the override target beside it — and NOT against
// the service's own allowlist. A token that does not exist yet has no
// reachability to compute. This is safe because the redirect's result passes
// every admission gate at request time, the service allowlist included, so
// save-time validation is a usability check and never the security boundary.
func TestCreateServiceTokenFallbackIsCheckedAgainstThePrincipal(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	svc, _, _, _ := newServiceAccountsTestService(t, now)
	created, err := svc.CreateService(ctx, svcAdminToken(), CreateServiceRequest{
		Name:          "Narrow Bot",
		Delegates:     []ServiceDelegateInput{{UserID: "usr_full", CanManageSettings: true}},
		AllowedModels: []string{"model-a"},
		AdminGroupIDs: []string{testServiceAdminGroupID},
	})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	// model-b is outside the service's allowlist but callable by the principal:
	// accepted at save time, and refused (not silently rerouted) per request.
	resp, err := svc.CreateServiceToken(ctx, svcAdminToken(), created.ID, CreateServiceTokenRequest{
		Name: "T1", UnknownModelRedirect: true, UnknownModelFallback: "model-b",
	})
	if err != nil {
		t.Fatalf("fallback outside the service allowlist rejected at save time: %v", err)
	}
	if resp.Token.UnknownModelFallback != "model-b" {
		t.Fatalf("fallback = %q, want model-b", resp.Token.UnknownModelFallback)
	}
}

func TestCreateServiceTokenClearsSettingsWhenRedirectOff(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := newServiceAccountsTestService(t, time.Now().UTC())
	created := createTestService(t, ctx, svc)
	resp, err := svc.CreateServiceToken(ctx, svcAdminToken(), created.ID, CreateServiceTokenRequest{
		Name: "T1", UnknownModelRedirect: false,
		UnknownModelRedirectBlocked: true, UnknownModelFallback: "model-a",
	})
	if err != nil {
		t.Fatalf("CreateServiceToken: %v", err)
	}
	if resp.Token.UnknownModelRedirectBlocked || resp.Token.UnknownModelFallback != "" {
		t.Fatalf("settings kept without the redirect: %+v", resp.Token)
	}
}

func TestServiceTokenDTOCarriesLastUsedModel(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	svc, dir, _, _ := newServiceAccountsTestService(t, now)
	created := createTestService(t, ctx, svc)
	seedServiceTokenRecord(t, dir, created.ID, "tok_svc_1", "model-a", now)
	list, err := svc.ListServiceTokens(ctx, svcAdminToken(), created.ID)
	if err != nil {
		t.Fatalf("ListServiceTokens: %v", err)
	}
	if len(list) != 1 || list[0].LastUsedModel != "model-a" {
		t.Fatalf("listed token = %+v, want last_used_model model-a", list)
	}
}

// TestCreateServiceTokenCannotSetLastUsedModel is the read-only guarantee on
// this path: the marker is decoded from a real body that tries to forge it, so
// a field silently added to the request type later fails this test.
func TestCreateServiceTokenCannotSetLastUsedModel(t *testing.T) {
	ctx := context.Background()
	svc, dir, _, _ := newServiceAccountsTestService(t, time.Now().UTC())
	created := createTestService(t, ctx, svc)
	var req CreateServiceTokenRequest
	if err := json.Unmarshal([]byte(`{"name":"T1","last_used_model":"forged-model"}`), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	resp, err := svc.CreateServiceToken(ctx, svcAdminToken(), created.ID, req)
	if err != nil {
		t.Fatalf("CreateServiceToken: %v", err)
	}
	if resp.Token.LastUsedModel != "" {
		t.Fatalf("dto last_used_model = %q, want empty", resp.Token.LastUsedModel)
	}
	record, err := dir.TokenByID(ctx, resp.Token.ID)
	if err != nil {
		t.Fatalf("TokenByID: %v", err)
	}
	if record.LastUsedModel != "" {
		t.Fatalf("record last_used_model = %q, want empty", record.LastUsedModel)
	}
}
