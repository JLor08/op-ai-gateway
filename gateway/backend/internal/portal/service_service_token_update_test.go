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
		DecisionsEmulation:           boolPtr(true),
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
	if !got.ClaudeModelAliases || !got.ClaudeEffortCompatibility || !got.DecisionsConfidenceNormalize || !got.DecisionsEmulation || !got.SendUsageLimits || !got.CodexModelSwitchPrompt {
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
	if rec.Name != "Renamed" || !rec.ClaudeEffortCompatibility || !rec.DecisionsEmulation || !rec.CodexModelSwitchPrompt || rec.UnknownModelFallback != "model-b" || rec.Status != store.TokenStatusActive {
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
		DecisionsEmulation: true, SendUsageLimits: true, CodexModelSwitchPrompt: true,
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

// TestUpdateServiceTokenTogglesDecisionsEmulation (#380): decisions_emulation is
// an ungated update field exactly like decisions_confidence_normalize. An
// inference-only bearer may switch it on and off; the stored record, the DTO and
// the live bearer (the request path reads the opt-in there) all follow, and an
// update without the field keeps it.
func TestUpdateServiceTokenTogglesDecisionsEmulation(t *testing.T) {
	ctx := context.Background()
	svc, dir, tokenStore, _ := newServiceAccountsTestService(t, time.Now().UTC())
	created := createTestService(t, ctx, svc)
	tok := mintServiceToken(t, ctx, svc, created.ID, CreateServiceTokenRequest{Name: "E"})

	got, err := svc.UpdateServiceToken(ctx, svcFullToken(), created.ID, tok.Token.ID, UpdateServiceTokenRequest{DecisionsEmulation: boolPtr(true)})
	if err != nil {
		t.Fatalf("UpdateServiceToken(on): %v", err)
	}
	if !got.DecisionsEmulation || got.DecisionsConfidenceNormalize {
		t.Fatalf("after on: %+v, want only decisions_emulation set", got)
	}
	if rec, err := dir.TokenByID(ctx, tok.Token.ID); err != nil || !rec.DecisionsEmulation {
		t.Fatalf("stored record = %+v err=%v, want DecisionsEmulation", rec, err)
	}
	if live, ok := tokenStore.LookupBearer("Bearer " + tok.Secret); !ok || !live.DecisionsEmulation {
		t.Fatalf("live bearer = %+v ok=%v, want DecisionsEmulation", live, ok)
	}

	// Absent keeps.
	got, err = svc.UpdateServiceToken(ctx, svcFullToken(), created.ID, tok.Token.ID, UpdateServiceTokenRequest{Name: strPtr("E2")})
	if err != nil || !got.DecisionsEmulation {
		t.Fatalf("after an unrelated edit: %+v err=%v, want decisions_emulation kept", got, err)
	}

	// Present false replaces.
	got, err = svc.UpdateServiceToken(ctx, svcFullToken(), created.ID, tok.Token.ID, UpdateServiceTokenRequest{DecisionsEmulation: boolPtr(false)})
	if err != nil || got.DecisionsEmulation {
		t.Fatalf("after off: %+v err=%v", got, err)
	}
	if live, ok := tokenStore.LookupBearer("Bearer " + tok.Secret); !ok || live.DecisionsEmulation {
		t.Fatalf("live bearer = %+v ok=%v, want DecisionsEmulation cleared", live, ok)
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

// TestUpdateServiceTokenDoesNotRevalidateUnchangedValues: a service token is
// edited by several principals, and its service's allowlist moves, so a stored
// model-valued setting can be one the editor could not set today. Sending it
// back UNCHANGED (the portal dialog writes the whole form back) must not turn an
// unrelated edit into a 400; a CHANGED value is still validated as on create.
func TestUpdateServiceTokenDoesNotRevalidateUnchangedValues(t *testing.T) {
	ctx := context.Background()
	svc, dir, _, _ := newServiceAccountsTestService(t, time.Now().UTC())
	created := narrowServiceWithAllowlist(t, ctx, svc, "model-a")
	tok := mintServiceToken(t, ctx, svc, created.ID, CreateServiceTokenRequest{Name: "S"})

	// Plant values no editor could set now: an override target and a rule
	// target that no longer exist, and a redirect fallback outside the allowlist.
	staleRules := map[string]store.ModelOverrideRule{"alias": {To: "gone-model"}}
	rec, err := dir.TokenByID(ctx, tok.Token.ID)
	if err != nil {
		t.Fatalf("TokenByID: %v", err)
	}
	rec.ModelOverride = "gone-model"
	rec.ModelOverrideMap = store.EncodeModelOverrideRules(staleRules)
	rec.UnknownModelRedirect, rec.UnknownModelRedirectBlocked, rec.UnknownModelFallback = true, true, "model-b"
	if err := dir.UpdateTokenMetadata(ctx, rec); err != nil {
		t.Fatalf("UpdateTokenMetadata: %v", err)
	}

	// The whole form written back with one real change succeeds and keeps them.
	got, err := svc.UpdateServiceToken(ctx, svcFullToken(), created.ID, tok.Token.ID, UpdateServiceTokenRequest{
		ModelOverride:               strPtr("gone-model"),
		ModelOverrideMap:            &staleRules,
		UnknownModelRedirect:        boolPtr(true),
		UnknownModelRedirectBlocked: boolPtr(true),
		UnknownModelFallback:        strPtr(" model-b "),
		LogCommunication:            boolPtr(true),
	})
	if err != nil {
		t.Fatalf("write-back of unchanged values: %v", err)
	}
	if !got.LogCommunication || got.ModelOverride != "gone-model" || got.ModelOverrideMap["alias"].To != "gone-model" || got.UnknownModelFallback != "model-b" {
		t.Fatalf("got = %+v, want the edit with the stored values kept", got)
	}

	// Each CHANGED value is validated again.
	otherRules := map[string]store.ModelOverrideRule{"alias": {To: "also-gone"}}
	for name, req := range map[string]UpdateServiceTokenRequest{
		"override":      {ModelOverride: strPtr("also-gone")},
		"rule target":   {ModelOverrideMap: &otherRules},
		"fallback":      {UnknownModelFallback: strPtr("also-gone")},
		"redirect trio": {UnknownModelRedirectBlocked: boolPtr(false)}, // keeps the stale fallback, now re-checked
	} {
		if _, err := svc.UpdateServiceToken(ctx, svcFullToken(), created.ID, tok.Token.ID, req); !errors.Is(err, ErrTokenModelOverrideInvalid) {
			t.Fatalf("%s: err = %v, want ErrTokenModelOverrideInvalid for a changed value", name, err)
		}
	}
}
