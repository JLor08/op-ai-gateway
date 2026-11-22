// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"testing"
	"time"
)

func usageSwitchesFixture(t *testing.T) (*Service, *MemoryDirectory, *auth.TokenStore) {
	t.Helper()
	ctx := context.Background()
	tokens := auth.NewTokenStore()
	dir := NewMemoryDirectory(tokens)
	if err := dir.CreateUser(ctx, store.User{
		ID: "usr_1", Email: "u1@example.test", DisplayName: "U1", Role: "user",
		Status: store.UserStatusActive, PreferredLanguage: "de",
		CreatedAt: offeringTime, UpdatedAt: offeringTime,
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	svc := NewService(ServiceDeps{Users: dir, Tokens: dir, Routes: routing.NewMemoryStore(), Clock: func() time.Time { return offeringTime }})
	return svc, dir, tokens
}

// TestTokenUsageSwitchesReachAuthToken pins create/update/bearer for the two
// usage-limit switches (#420) on the memory driver and through the service:
// a token created with both on resolves to an auth.Token with both on, an
// absent PATCH field keeps the value, an explicit false turns one off, and the
// switches are ungated (an inference-only bearer may set both).
func TestTokenUsageSwitchesReachAuthToken(t *testing.T) {
	ctx := context.Background()
	svc, _, tokens := usageSwitchesFixture(t)
	owner := auth.Token{UserID: "usr_1", Scopes: []string{"gateway:use"}}

	created, err := svc.CreateToken(ctx, owner, CreateTokenRequest{
		Name: "uls", Scopes: []string{"gateway:use"}, SendUsageLimits: true, CodexModelSwitchPrompt: true,
	})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if !created.Token.SendUsageLimits || !created.Token.CodexModelSwitchPrompt {
		t.Fatalf("create DTO = %+v, want both switches true", created.Token)
	}
	bearer := "Bearer " + created.Secret
	if tok, ok := tokens.LookupBearer(bearer); !ok || !tok.SendUsageLimits || !tok.CodexModelSwitchPrompt {
		t.Fatalf("bearer ok=%v send=%v prompt=%v, want true/true", ok, tok.SendUsageLimits, tok.CodexModelSwitchPrompt)
	}

	// An unrelated PATCH keeps both.
	dto, err := svc.UpdateToken(ctx, owner, created.Token.ID, UpdateTokenRequest{Name: func() *string { s := "renamed"; return &s }()})
	if err != nil {
		t.Fatalf("UpdateToken(name): %v", err)
	}
	if !dto.SendUsageLimits || !dto.CodexModelSwitchPrompt {
		t.Fatalf("a PATCH without the fields cleared them: %+v", dto)
	}

	// Explicit false on one leaves the other.
	dto, err = svc.UpdateToken(ctx, owner, created.Token.ID, UpdateTokenRequest{SendUsageLimits: boolPtr(false)})
	if err != nil {
		t.Fatalf("UpdateToken(send=false): %v", err)
	}
	if dto.SendUsageLimits || !dto.CodexModelSwitchPrompt {
		t.Fatalf("after send=false DTO = %+v, want send=false prompt=true", dto)
	}
	if tok, ok := tokens.LookupBearer(bearer); !ok || tok.SendUsageLimits || !tok.CodexModelSwitchPrompt {
		t.Fatalf("after send=false bearer ok=%v send=%v prompt=%v, want false/true", ok, tok.SendUsageLimits, tok.CodexModelSwitchPrompt)
	}
	dto, err = svc.UpdateToken(ctx, owner, created.Token.ID, UpdateTokenRequest{CodexModelSwitchPrompt: boolPtr(false)})
	if err != nil {
		t.Fatalf("UpdateToken(prompt=false): %v", err)
	}
	if dto.SendUsageLimits || dto.CodexModelSwitchPrompt {
		t.Fatalf("after both off DTO = %+v", dto)
	}
	if tok, ok := tokens.LookupBearer(bearer); !ok || tok.SendUsageLimits || tok.CodexModelSwitchPrompt {
		t.Fatalf("after both off bearer ok=%v send=%v prompt=%v, want false/false", ok, tok.SendUsageLimits, tok.CodexModelSwitchPrompt)
	}

	// Run-as resolves the stored switches too.
	dto, err = svc.UpdateToken(ctx, owner, created.Token.ID, UpdateTokenRequest{SendUsageLimits: boolPtr(true), CodexModelSwitchPrompt: boolPtr(true)})
	if err != nil || !dto.SendUsageLimits || !dto.CodexModelSwitchPrompt {
		t.Fatalf("UpdateToken(both on) err=%v dto=%+v", err, dto)
	}
	runAs, err := svc.AuthorizeRunAsToken(ctx, owner, created.Token.ID)
	if err != nil {
		t.Fatalf("AuthorizeRunAsToken: %v", err)
	}
	if !runAs.SendUsageLimits || !runAs.CodexModelSwitchPrompt {
		t.Fatalf("run-as send=%v prompt=%v, want true/true", runAs.SendUsageLimits, runAs.CodexModelSwitchPrompt)
	}

	// Ungated: a gateway-use-only bearer may set both on create and update.
	inference := auth.Token{ID: "tok_caller", UserID: "usr_1", Scopes: []string{"gateway:use"}}
	own, err := svc.CreateToken(ctx, inference, CreateTokenRequest{
		Name: "own", Scopes: []string{"gateway:use"}, SendUsageLimits: true, CodexModelSwitchPrompt: true,
	})
	if err != nil || !own.Token.SendUsageLimits || !own.Token.CodexModelSwitchPrompt {
		t.Fatalf("inference-only create err=%v token=%+v", err, own.Token)
	}
	if dto, err = svc.UpdateToken(ctx, inference, own.Token.ID, UpdateTokenRequest{SendUsageLimits: boolPtr(false)}); err != nil || dto.SendUsageLimits {
		t.Fatalf("inference-only update err=%v dto=%+v", err, dto)
	}
}

// TestCreateServiceTokenUsageSwitches pins the service-token half: both
// switches are accepted on create, appear in the create DTO and the listing,
// and the bearer resolves to an auth.Token carrying them (create-only, #421).
func TestCreateServiceTokenUsageSwitches(t *testing.T) {
	ctx := context.Background()
	svc, _, tokens, _ := newServiceAccountsTestService(t, time.Now().UTC())
	created := createTestService(t, ctx, svc)

	on, err := svc.CreateServiceToken(ctx, svcAdminToken(), created.ID, CreateServiceTokenRequest{
		Name: "uls-on", SendUsageLimits: true, CodexModelSwitchPrompt: true,
	})
	if err != nil {
		t.Fatalf("CreateServiceToken(on): %v", err)
	}
	if !on.Token.SendUsageLimits || !on.Token.CodexModelSwitchPrompt {
		t.Fatalf("create DTO = %+v, want both true", on.Token)
	}
	if tok, ok := tokens.LookupBearer("Bearer " + on.Secret); !ok || !tok.SendUsageLimits || !tok.CodexModelSwitchPrompt {
		t.Fatalf("bearer ok=%v send=%v prompt=%v, want true/true", ok, tok.SendUsageLimits, tok.CodexModelSwitchPrompt)
	}
	off, err := svc.CreateServiceToken(ctx, svcAdminToken(), created.ID, CreateServiceTokenRequest{Name: "uls-off"})
	if err != nil {
		t.Fatalf("CreateServiceToken(off): %v", err)
	}
	if off.Token.SendUsageLimits || off.Token.CodexModelSwitchPrompt {
		t.Fatalf("default create DTO = %+v, want both false", off.Token)
	}
	list, err := svc.ListServiceTokens(ctx, svcAdminToken(), created.ID)
	if err != nil {
		t.Fatalf("ListServiceTokens: %v", err)
	}
	got := map[string][2]bool{}
	for _, dto := range list {
		got[dto.ID] = [2]bool{dto.SendUsageLimits, dto.CodexModelSwitchPrompt}
	}
	if got[on.Token.ID] != [2]bool{true, true} || got[off.Token.ID] != [2]bool{false, false} {
		t.Fatalf("listing switches = %v", got)
	}
}
