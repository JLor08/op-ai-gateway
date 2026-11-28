// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"reflect"
	"testing"
	"time"
)

// UpdateServiceToken (#421): a field-level edit of a service token's per-token
// options. Every field is a pointer — nil keeps, non-nil replaces — and each is
// validated with exactly the validator the create path uses, under the same
// object gate and the same management rule for claude_effort_compatibility.

// mintServiceToken creates a token on serviceID as the admin and fails the test
// on any error.
func mintServiceToken(t *testing.T, ctx context.Context, svc *Service, serviceID string, req CreateServiceTokenRequest) CreateServiceTokenResponse {
	t.Helper()
	resp, err := svc.CreateServiceToken(ctx, svcAdminToken(), serviceID, req)
	if err != nil {
		t.Fatalf("CreateServiceToken: %v", err)
	}
	return resp
}

func TestUpdateServiceTokenReplacesEveryEditableField(t *testing.T) {
	ctx := context.Background()
	svc, dir, _, _ := newServiceAccountsTestService(t, time.Now().UTC())
	created := createTestService(t, ctx, svc)
	tok := mintServiceToken(t, ctx, svc, created.ID, CreateServiceTokenRequest{Name: "T1"})

	rules := map[string]store.ModelOverrideRule{"model-a": {To: "model-b"}}
	got, err := svc.UpdateServiceToken(ctx, svcFullToken(), created.ID, tok.Token.ID, UpdateServiceTokenRequest{
		Name:                         strPtr("  Renamed  "),
		ModelOverride:                strPtr("model-a"),
		ModelOverrideMap:             &rules,
		LogCommunication:             boolPtr(true),
		Secret:                       boolPtr(true),
		UnknownModelRedirect:         boolPtr(true),
		UnknownModelRedirectBlocked:  boolPtr(true),
		UnknownModelFallback:         strPtr("model-b"),
		ClaudeModelAliases:           boolPtr(true),
		ClaudeEffortCompatibility:    boolPtr(true),
		DecisionsConfidenceNormalize: boolPtr(true),
		SendUsageLimits:              boolPtr(true),
		CodexModelSwitchPrompt:       boolPtr(true),
	})
	if err != nil {
		t.Fatalf("UpdateServiceToken: %v", err)
	}
	if got.ID != tok.Token.ID || got.ServiceID != created.ID || got.Name != "Renamed" {
		t.Fatalf("identity/name = %+v", got)
	}
	if got.ModelOverride != "model-a" || got.ModelOverrideMap["model-a"].To != "model-b" {
		t.Fatalf("overrides = %q %+v", got.ModelOverride, got.ModelOverrideMap)
	}
	if !got.LogCommunication || !got.Secret || !got.UnknownModelRedirect || !got.UnknownModelRedirectBlocked || got.UnknownModelFallback != "model-b" {
		t.Fatalf("logging/secret/redirect = %+v", got)
	}
	if !got.ClaudeModelAliases || !got.ClaudeEffortCompatibility || !got.DecisionsConfidenceNormalize || !got.SendUsageLimits || !got.CodexModelSwitchPrompt {
		t.Fatalf("switches = %+v", got)
	}
	if got.SecretPrefix != tok.Token.SecretPrefix || got.SecretPrefix == "" {
		t.Fatalf("SecretPrefix = %q, want unchanged %q", got.SecretPrefix, tok.Token.SecretPrefix)
	}
	if len(got.Scopes) != 1 || got.Scopes[0] != serviceLLMInvokeScope {
		t.Fatalf("Scopes = %v, want the fixed [llm:invoke]", got.Scopes)
	}

	// Persisted, and the list reads the same back.
	rec, err := dir.TokenByID(ctx, tok.Token.ID)
	if err != nil {
		t.Fatalf("TokenByID: %v", err)
	}
	if rec.Name != "Renamed" || !rec.ClaudeEffortCompatibility || !rec.CodexModelSwitchPrompt || rec.UnknownModelFallback != "model-b" || rec.Status != store.TokenStatusActive {
		t.Fatalf("stored record = %+v", rec)
	}
	list, err := svc.ListServiceTokens(ctx, svcAdminToken(), created.ID)
	if err != nil || len(list) != 1 || !reflect.DeepEqual(list[0].ModelOverrideMap, got.ModelOverrideMap) || list[0].Name != "Renamed" {
		t.Fatalf("ListServiceTokens = %+v err=%v", list, err)
	}
}

func TestUpdateServiceTokenAbsentFieldsKeepTheirValues(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := newServiceAccountsTestService(t, time.Now().UTC())
	created := createTestService(t, ctx, svc)
	tok := mintServiceToken(t, ctx, svc, created.ID, CreateServiceTokenRequest{
		Name: "Keep", ModelOverride: "model-a", LogCommunication: true, Secret: true,
		UnknownModelRedirect: true, UnknownModelFallback: "model-b",
		ClaudeModelAliases: true, ClaudeEffortCompatibility: true, DecisionsConfidenceNormalize: true,
		SendUsageLimits: true, CodexModelSwitchPrompt: true,
	})

	// Only one field changes; everything else stays as created.
	got, err := svc.UpdateServiceToken(ctx, svcAdminToken(), created.ID, tok.Token.ID, UpdateServiceTokenRequest{SendUsageLimits: boolPtr(false)})
	if err != nil {
		t.Fatalf("UpdateServiceToken: %v", err)
	}
	want := tok.Token
	want.SendUsageLimits = false
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("after a one-field update:\n got %+v\nwant %+v", got, want)
	}

	// An empty request is a no-op, not an error.
	again, err := svc.UpdateServiceToken(ctx, svcAdminToken(), created.ID, tok.Token.ID, UpdateServiceTokenRequest{})
	if err != nil || !reflect.DeepEqual(again, want) {
		t.Fatalf("empty update = %+v err=%v, want %+v", again, err, want)
	}
}

func TestUpdateServiceTokenRedirectValidatedAsOneUnit(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := newServiceAccountsTestService(t, time.Now().UTC())
	created := createTestService(t, ctx, svc)
	tok := mintServiceToken(t, ctx, svc, created.ID, CreateServiceTokenRequest{
		Name: "R", UnknownModelRedirect: true, UnknownModelRedirectBlocked: true, UnknownModelFallback: "model-a",
	})
	// Switching the redirect off clears both sub-settings, exactly as on create.
	got, err := svc.UpdateServiceToken(ctx, svcAdminToken(), created.ID, tok.Token.ID, UpdateServiceTokenRequest{UnknownModelRedirect: boolPtr(false)})
	if err != nil {
		t.Fatalf("UpdateServiceToken: %v", err)
	}
	if got.UnknownModelRedirect || got.UnknownModelRedirectBlocked || got.UnknownModelFallback != "" {
		t.Fatalf("redirect off left sub-settings: %+v", got)
	}
	// An unknown fallback is refused.
	if _, err := svc.UpdateServiceToken(ctx, svcAdminToken(), created.ID, tok.Token.ID, UpdateServiceTokenRequest{
		UnknownModelRedirect: boolPtr(true), UnknownModelFallback: strPtr("no-such-model"),
	}); !errors.Is(err, ErrTokenModelOverrideInvalid) {
		t.Fatalf("unknown fallback err = %v, want ErrTokenModelOverrideInvalid", err)
	}
}

func TestUpdateServiceTokenRejectsAFallbackOutsideTheAllowlist(t *testing.T) {
	ctx := context.Background()
	svc, dir, _, _ := newServiceAccountsTestService(t, time.Now().UTC())
	created := narrowServiceWithAllowlist(t, ctx, svc, "model-a")
	tok := mintServiceToken(t, ctx, svc, created.ID, CreateServiceTokenRequest{Name: "N"})

	// model-b is callable by the admin but outside the service's allowlist.
	if _, err := svc.UpdateServiceToken(ctx, svcAdminToken(), created.ID, tok.Token.ID, UpdateServiceTokenRequest{
		UnknownModelRedirect: boolPtr(true), UnknownModelFallback: strPtr("model-b"),
	}); !errors.Is(err, ErrTokenModelOverrideInvalid) {
		t.Fatalf("err = %v, want ErrTokenModelOverrideInvalid for a fallback the allowlist blocks", err)
	}
	rec, err := dir.TokenByID(ctx, tok.Token.ID)
	if err != nil || rec.UnknownModelRedirect || rec.UnknownModelFallback != "" {
		t.Fatalf("a refused update wrote the redirect: %+v err=%v", rec, err)
	}
	got, err := svc.UpdateServiceToken(ctx, svcAdminToken(), created.ID, tok.Token.ID, UpdateServiceTokenRequest{
		UnknownModelRedirect: boolPtr(true), UnknownModelFallback: strPtr("model-a"),
	})
	if err != nil || got.UnknownModelFallback != "model-a" {
		t.Fatalf("allowlisted fallback = %+v err=%v", got, err)
	}
}

func TestUpdateServiceTokenRejectsInvalidValues(t *testing.T) {
	ctx := context.Background()
	svc, dir, _, _ := newServiceAccountsTestService(t, time.Now().UTC())
	created := createTestService(t, ctx, svc)
	tok := mintServiceToken(t, ctx, svc, created.ID, CreateServiceTokenRequest{Name: "V"})

	badRules := map[string]store.ModelOverrideRule{"model-a": {To: "no-such-model"}}
	cases := []struct {
		name string
		req  UpdateServiceTokenRequest
		want error
	}{
		{"blank name", UpdateServiceTokenRequest{Name: strPtr("   ")}, ErrServiceValidation},
		{"unknown override", UpdateServiceTokenRequest{ModelOverride: strPtr("no-such-model")}, ErrTokenModelOverrideInvalid},
		{"unknown rule target", UpdateServiceTokenRequest{ModelOverrideMap: &badRules}, ErrTokenModelOverrideInvalid},
		{"unreachable vendor account", UpdateServiceTokenRequest{VendorAccess: namedServiceAccess("acc_unknown")}, ErrTokenVendorAccessInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A valid field rides along to prove nothing is half-written.
			tc.req.LogCommunication = boolPtr(true)
			if _, err := svc.UpdateServiceToken(ctx, svcAdminToken(), created.ID, tok.Token.ID, tc.req); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			rec, err := dir.TokenByID(ctx, tok.Token.ID)
			if err != nil || rec.Name != "V" || rec.LogCommunication {
				t.Fatalf("a refused update wrote something: %+v err=%v", rec, err)
			}
		})
	}
}

func TestUpdateServiceTokenObjectGateAndCrossService(t *testing.T) {
	ctx := context.Background()
	svc, _, _, _ := newServiceAccountsTestService(t, time.Now().UTC())
	svcA := createTestService(t, ctx, svc)
	svcB, err := svc.CreateService(ctx, svcAdminToken(), CreateServiceRequest{Name: "Other Service", AdminGroupIDs: []string{testServiceAdminGroupID}})
	if err != nil {
		t.Fatalf("CreateService (B): %v", err)
	}
	tok := mintServiceToken(t, ctx, svc, svcA.ID, CreateServiceTokenRequest{Name: "A1"})
	rename := UpdateServiceTokenRequest{Name: strPtr("hijacked")}

	if _, err := svc.UpdateServiceToken(ctx, svcAdminToken(), svcB.ID, tok.Token.ID, rename); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("cross-service = %v, want ErrTokenNotFound", err)
	}
	if _, err := svc.UpdateServiceToken(ctx, svcAdminToken(), svcA.ID, "tok_missing", rename); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("unknown token = %v, want ErrTokenNotFound", err)
	}
	if _, err := svc.UpdateServiceToken(ctx, svcAdminToken(), svcA.ID, "  ", rename); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("blank token id = %v, want ErrTokenNotFound", err)
	}
	if _, err := svc.UpdateServiceToken(ctx, svcStrangerToken(), svcA.ID, tok.Token.ID, rename); !errors.Is(err, ErrServiceNotFound) {
		t.Fatalf("stranger = %v, want ErrServiceNotFound", err)
	}
	// A USER token's id addressed through a service path is not a service token.
	userTok := "tok_user_one"
	if err := svc.tokens.CreatePlainToken(ctx, store.TokenRecord{
		ID: userTok, UserID: "usr_admin", Name: "u", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`,
	}, "secret-user-one"); err != nil {
		t.Fatalf("CreatePlainToken: %v", err)
	}
	if _, err := svc.UpdateServiceToken(ctx, svcAdminToken(), svcA.ID, userTok, rename); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("user token via service path = %v, want ErrTokenNotFound", err)
	}

	// A Token-Delegate (no settings rights) may edit tokens — the *Tokens* gate.
	got, err := svc.UpdateServiceToken(ctx, svcTokenDelToken(), svcA.ID, tok.Token.ID, UpdateServiceTokenRequest{Name: strPtr("by token delegate")})
	if err != nil || got.Name != "by token delegate" {
		t.Fatalf("token delegate update = %+v err=%v", got, err)
	}
}

// TestUpdateServiceTokenEffortFlagRequiresManagementPrincipal mirrors
// UpdateToken's rule: ANY value of claude_effort_compatibility (on or off) from
// an inference-only bearer is refused before anything is written — the whole
// request, not just the flag.
func TestUpdateServiceTokenEffortFlagRequiresManagementPrincipal(t *testing.T) {
	ctx := context.Background()
	svc, dir, _, _ := newServiceAccountsTestService(t, time.Now().UTC())
	created := createTestService(t, ctx, svc)
	tok := mintServiceToken(t, ctx, svc, created.ID, CreateServiceTokenRequest{Name: "E", ClaudeEffortCompatibility: true})

	delegateBearer := svcFullToken()
	delegateBearer.ID = "tok_delegate_bearer"
	strangerBearer := svcStrangerToken()
	strangerBearer.ID = "tok_stranger_bearer"

	for _, value := range []bool{false, true} {
		if _, err := svc.UpdateServiceToken(ctx, delegateBearer, created.ID, tok.Token.ID, UpdateServiceTokenRequest{
			Name: strPtr("escalated"), ClaudeEffortCompatibility: boolPtr(value),
		}); !errors.Is(err, ErrPrincipalForbidden) {
			t.Fatalf("delegate bearer, effort=%v: err = %v, want ErrPrincipalForbidden", value, err)
		}
	}
	rec, err := dir.TokenByID(ctx, tok.Token.ID)
	if err != nil || rec.Name != "E" || !rec.ClaudeEffortCompatibility {
		t.Fatalf("a refused update wrote something: %+v err=%v", rec, err)
	}
	if _, err := svc.UpdateServiceToken(ctx, strangerBearer, created.ID, tok.Token.ID, UpdateServiceTokenRequest{
		ClaudeEffortCompatibility: boolPtr(false),
	}); !errors.Is(err, ErrServiceNotFound) {
		t.Fatalf("stranger bearer = %v, want ErrServiceNotFound (no existence leak)", err)
	}
	// The same bearer may still edit everything else.
	if got, err := svc.UpdateServiceToken(ctx, delegateBearer, created.ID, tok.Token.ID, UpdateServiceTokenRequest{Name: strPtr("plain")}); err != nil || got.Name != "plain" || !got.ClaudeEffortCompatibility {
		t.Fatalf("delegate bearer without the flag = %+v err=%v", got, err)
	}
	// A delegate session may flip it.
	if got, err := svc.UpdateServiceToken(ctx, svcFullToken(), created.ID, tok.Token.ID, UpdateServiceTokenRequest{ClaudeEffortCompatibility: boolPtr(false)}); err != nil || got.ClaudeEffortCompatibility {
		t.Fatalf("delegate session effort off = %+v err=%v", got, err)
	}
}

// TestUpdateServiceTokenMemoryDriverKeepsServiceStateOnTheBearer: the memory
// driver's UpdateTokenMetadata rebuilds the cached bearer from the token row
// alone — Active from the token status, no AllowedModels. Without the
// mirrorServiceTokenState push after the write, editing a token of a DISABLED
// service would re-activate it, and any edit would drop the allowlist.
func TestUpdateServiceTokenMemoryDriverKeepsServiceStateOnTheBearer(t *testing.T) {
	ctx := context.Background()
	svc, _, tokenStore, _ := newServiceAccountsTestService(t, time.Now().UTC())
	created := narrowServiceWithAllowlist(t, ctx, svc, "model-a")
	tok := mintServiceToken(t, ctx, svc, created.ID, CreateServiceTokenRequest{Name: "M"})

	// Active service: an edit keeps the allowlist and the secret on the bearer.
	if _, err := svc.UpdateServiceToken(ctx, svcAdminToken(), created.ID, tok.Token.ID, UpdateServiceTokenRequest{ClaudeModelAliases: boolPtr(true)}); err != nil {
		t.Fatalf("UpdateServiceToken: %v", err)
	}
	live, ok := tokenStore.LookupBearer("Bearer " + tok.Secret)
	if !ok {
		t.Fatal("the edited token no longer resolves under its unchanged secret")
	}
	if !live.IsService() || !live.ClaudeModelAliases {
		t.Fatalf("live token = %+v, want a service token carrying the edit", live)
	}
	if !reflect.DeepEqual(live.AllowedModels, []string{"model-a"}) {
		t.Fatalf("live AllowedModels = %v, want [model-a] kept across the edit", live.AllowedModels)
	}

	// Disabled service: an edit must not re-activate the bearer.
	disabled := routing.ServerStatusDisabled
	if _, err := svc.UpdateService(ctx, svcAdminToken(), created.ID, UpdateServiceRequest{Status: &disabled}); err != nil {
		t.Fatalf("UpdateService(disable): %v", err)
	}
	if _, err := svc.UpdateServiceToken(ctx, svcAdminToken(), created.ID, tok.Token.ID, UpdateServiceTokenRequest{Name: strPtr("edited while disabled")}); err != nil {
		t.Fatalf("UpdateServiceToken(disabled service): %v", err)
	}
	if _, ok := tokenStore.LookupBearer("Bearer " + tok.Secret); ok {
		t.Fatal("editing a disabled service's token re-activated its bearer")
	}
}
