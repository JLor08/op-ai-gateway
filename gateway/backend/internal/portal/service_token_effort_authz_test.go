// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/store"
	"reflect"
	"testing"
	"time"
)

// The claude_effort_compatibility switch changes what a token's holder is
// allowed to reach (the gateway delivers Claude Code's effort control to it),
// so it is a MANAGEMENT setting: only an authorized portal session or an
// admin-scope bearer may set it. An inference-only bearer — including a token
// editing the flag on ITSELF, and a token being created with the opt-in — must
// be refused, and refused BEFORE any write so a rejected request cannot leave
// this or any other field half-updated.
//
// The principals these tests use mirror what the gateway really builds:
//   - a session/loopback principal (sessionPrincipal) carries no token id, so
//     ID == "" (f.owner and svcFullToken() are exactly that);
//   - a bearer principal (LookupBearer) carries the token's own id, so ID != "".

// effortInferenceBearer is an inference-only bearer: a real token id, the base
// gateway:use scope every user token holds, and no admin scope.
func effortInferenceBearer(tokenID string) auth.Token {
	return auth.Token{ID: tokenID, UserID: "usr_1", Scopes: []string{"gateway:use"}}
}

// effortAdminBearer is a bearer that was legitimately minted with the admin
// scope.
func effortAdminBearer(tokenID string) auth.Token {
	return auth.Token{ID: tokenID, UserID: "usr_1", Scopes: []string{"gateway:use", "admin"}}
}

// writeCountingTokens wraps a TokenRepository and counts every MUTATING call
// that reaches it, so "rejected before any write" is proven by the store never
// being asked to write, not merely by its state happening to look unchanged.
type writeCountingTokens struct {
	TokenRepository
	writes int
}

func (w *writeCountingTokens) CreatePlainToken(ctx context.Context, token store.TokenRecord, secret string) error {
	w.writes++
	return w.TokenRepository.CreatePlainToken(ctx, token, secret)
}

func (w *writeCountingTokens) UpdateTokenMetadata(ctx context.Context, token store.TokenRecord) error {
	w.writes++
	return w.TokenRepository.UpdateTokenMetadata(ctx, token)
}

func (w *writeCountingTokens) DeleteToken(ctx context.Context, id string) error {
	w.writes++
	return w.TokenRepository.DeleteToken(ctx, id)
}

func (w *writeCountingTokens) RotateTokenSecret(ctx context.Context, id, secretHash, secretPrefix string, updatedAt time.Time) error {
	w.writes++
	return w.TokenRepository.RotateTokenSecret(ctx, id, secretHash, secretPrefix, updatedAt)
}

func (w *writeCountingTokens) SetTokenLastUsedModel(ctx context.Context, tokenID, model string) error {
	w.writes++
	return w.TokenRepository.SetTokenLastUsedModel(ctx, tokenID, model)
}

// newEffortAuthzFixture is the token-settings fixture with its service rebuilt
// over a write-counting store.
func newEffortAuthzFixture(t *testing.T) (*tokenSettingsFixture, *writeCountingTokens) {
	t.Helper()
	f := newTokenSettingsFixture(t, openAIModel("qwen3-32b"))
	spy := &writeCountingTokens{TokenRepository: f.dir}
	f.svc = NewService(ServiceDeps{Users: f.dir, Tokens: spy, Routes: f.rs, Clock: func() time.Time { return offeringTime }})
	return f, spy
}

func storedTokenRecord(t *testing.T, dir *MemoryDirectory, id string) store.TokenRecord {
	t.Helper()
	record, err := dir.TokenByID(context.Background(), id)
	if err != nil {
		t.Fatalf("TokenByID %s: %v", id, err)
	}
	return record
}

func tokenCountForUser(t *testing.T, dir *MemoryDirectory, userID string) int {
	t.Helper()
	records, err := dir.TokensByUser(context.Background(), userID)
	if err != nil {
		t.Fatalf("TokensByUser %s: %v", userID, err)
	}
	return len(records)
}

// TestCreateTokenClaudeEffortCompatibilityRejectsInferenceBearer: creating a
// token WITH the opt-in is refused for an inference-only bearer, and nothing is
// written — no token row, no store call at all.
func TestCreateTokenClaudeEffortCompatibilityRejectsInferenceBearer(t *testing.T) {
	ctx := context.Background()
	f, spy := newEffortAuthzFixture(t)

	_, err := f.svc.CreateToken(ctx, effortInferenceBearer("tok_caller"), CreateTokenRequest{
		Name: "escalated", Scopes: []string{"gateway:use"}, ClaudeEffortCompatibility: true,
	})
	if !errors.Is(err, ErrPrincipalForbidden) {
		t.Fatalf("CreateToken(inference bearer, effort on) = %v, want ErrPrincipalForbidden", err)
	}
	if spy.writes != 0 {
		t.Fatalf("a rejected create reached the store %d time(s), want 0 writes", spy.writes)
	}
	if n := tokenCountForUser(t, f.dir, "usr_1"); n != 0 {
		t.Fatalf("a rejected create left %d token row(s), want 0", n)
	}
}

// TestUpdateTokenClaudeEffortCompatibilityRejectsInferenceBearer: an
// inference-only bearer can neither turn the flag on nor off — on ITS OWN
// token, on a sibling token, and in combination with other fields. The request
// is rejected whole: the record is byte-for-byte what it was and the store was
// never asked to write.
func TestUpdateTokenClaudeEffortCompatibilityRejectsInferenceBearer(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name string
		// stored is the flag the target token starts with.
		stored bool
		// selfBearer: the caller IS the target token (a token editing itself);
		// otherwise it is a different token of the same user.
		selfBearer bool
		req        func() UpdateTokenRequest
	}{
		{"on itself: enable", false, true, func() UpdateTokenRequest {
			return UpdateTokenRequest{ClaudeEffortCompatibility: boolPtr(true)}
		}},
		{"on itself: disable", true, true, func() UpdateTokenRequest {
			return UpdateTokenRequest{ClaudeEffortCompatibility: boolPtr(false)}
		}},
		{"on a sibling token: enable", false, false, func() UpdateTokenRequest {
			return UpdateTokenRequest{ClaudeEffortCompatibility: boolPtr(true)}
		}},
		{"on a sibling token: disable", true, false, func() UpdateTokenRequest {
			return UpdateTokenRequest{ClaudeEffortCompatibility: boolPtr(false)}
		}},
		{"enable together with other fields: nothing partially applied", false, true, func() UpdateTokenRequest {
			return UpdateTokenRequest{
				Name:                      strPtr("renamed"),
				LogCommunication:          boolPtr(true),
				Secret:                    boolPtr(true),
				ClaudeModelAliases:        boolPtr(true),
				ClaudeEffortCompatibility: boolPtr(true),
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, spy := newEffortAuthzFixture(t)
			target, err := f.svc.CreateToken(ctx, f.owner, CreateTokenRequest{
				Name: "target", Scopes: []string{"gateway:use"}, ClaudeEffortCompatibility: tc.stored,
			})
			if err != nil {
				t.Fatalf("seed CreateToken: %v", err)
			}
			callerID := "tok_other"
			if tc.selfBearer {
				callerID = target.Token.ID
			}
			before := storedTokenRecord(t, f.dir, target.Token.ID)
			spy.writes = 0

			_, err = f.svc.UpdateToken(ctx, effortInferenceBearer(callerID), target.Token.ID, tc.req())
			if !errors.Is(err, ErrPrincipalForbidden) {
				t.Fatalf("UpdateToken(inference bearer) = %v, want ErrPrincipalForbidden", err)
			}
			if spy.writes != 0 {
				t.Fatalf("a rejected update reached the store %d time(s), want 0 writes", spy.writes)
			}
			after := storedTokenRecord(t, f.dir, target.Token.ID)
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("a rejected update changed the stored record:\nbefore: %+v\nafter:  %+v", before, after)
			}
			if after.ClaudeEffortCompatibility != tc.stored {
				t.Fatalf("stored claude_effort_compatibility = %v, want it left at %v", after.ClaudeEffortCompatibility, tc.stored)
			}
		})
	}
}

// TestClaudeEffortCompatibilityAllowedForManagementPrincipals: a portal session
// (no token id) and an admin-scope bearer can both set the flag, on create and
// on update, in both directions — the gate must not lock out the very
// principals the setting is reserved for.
func TestClaudeEffortCompatibilityAllowedForManagementPrincipals(t *testing.T) {
	ctx := context.Background()
	principals := []struct {
		name  string
		owner func(f *tokenSettingsFixture) auth.Token
	}{
		{"session", func(f *tokenSettingsFixture) auth.Token { return f.owner }},
		{"admin bearer", func(*tokenSettingsFixture) auth.Token { return effortAdminBearer("tok_admin") }},
	}
	for _, p := range principals {
		t.Run(p.name, func(t *testing.T) {
			f, _ := newEffortAuthzFixture(t)
			owner := p.owner(f)

			created, err := f.svc.CreateToken(ctx, owner, CreateTokenRequest{
				Name: "managed", Scopes: []string{"gateway:use"}, ClaudeEffortCompatibility: true,
			})
			if err != nil {
				t.Fatalf("CreateToken(effort on) = %v, want success", err)
			}
			if !created.Token.ClaudeEffortCompatibility || !storedClaudeEffortCompatibility(t, f.dir, created.Token.ID) {
				t.Fatalf("create did not persist the flag: %+v", created.Token)
			}

			dto, err := f.svc.UpdateToken(ctx, owner, created.Token.ID, UpdateTokenRequest{ClaudeEffortCompatibility: boolPtr(false)})
			if err != nil {
				t.Fatalf("UpdateToken(effort off) = %v, want success", err)
			}
			if dto.ClaudeEffortCompatibility || storedClaudeEffortCompatibility(t, f.dir, created.Token.ID) {
				t.Fatalf("update did not clear the flag: %+v", dto)
			}

			dto, err = f.svc.UpdateToken(ctx, owner, created.Token.ID, UpdateTokenRequest{ClaudeEffortCompatibility: boolPtr(true)})
			if err != nil {
				t.Fatalf("UpdateToken(effort on) = %v, want success", err)
			}
			if !dto.ClaudeEffortCompatibility || !storedClaudeEffortCompatibility(t, f.dir, created.Token.ID) {
				t.Fatalf("update did not set the flag: %+v", dto)
			}
		})
	}
}

// TestClaudeEffortCompatibilityGateIsScopedToTheFlag: the gate fires only when
// the request SETS this flag. An inference bearer keeps doing everything it
// could do before — creating a token without the flag, creating or updating
// claude_model_aliases (a different, ungated setting), and editing unrelated
// fields — and an update that does not mention the flag leaves a stored value
// (on or off) untouched.
func TestClaudeEffortCompatibilityGateIsScopedToTheFlag(t *testing.T) {
	ctx := context.Background()
	f, _ := newEffortAuthzFixture(t)
	bearer := effortInferenceBearer("tok_caller")

	// Create without the flag: allowed, stored off.
	plain, err := f.svc.CreateToken(ctx, bearer, CreateTokenRequest{Name: "plain", Scopes: []string{"gateway:use"}})
	if err != nil {
		t.Fatalf("CreateToken(bearer, flag omitted) = %v, want success", err)
	}
	if plain.Token.ClaudeEffortCompatibility || storedClaudeEffortCompatibility(t, f.dir, plain.Token.ID) {
		t.Fatalf("a token created without the flag reads on: %+v", plain.Token)
	}

	// claude_model_aliases is NOT gated: a bearer may still create with it on.
	aliased, err := f.svc.CreateToken(ctx, bearer, CreateTokenRequest{
		Name: "aliased", Scopes: []string{"gateway:use"}, ClaudeModelAliases: true,
	})
	if err != nil {
		t.Fatalf("CreateToken(bearer, claude_model_aliases on) = %v, want success", err)
	}
	if !aliased.Token.ClaudeModelAliases || aliased.Token.ClaudeEffortCompatibility {
		t.Fatalf("claude_model_aliases create = %+v, want aliases on and effort off", aliased.Token)
	}

	// Unrelated + claude_model_aliases updates are allowed and leave an OFF flag off.
	dto, err := f.svc.UpdateToken(ctx, bearer, plain.Token.ID, UpdateTokenRequest{
		Name:               strPtr("plain-renamed"),
		LogCommunication:   boolPtr(true),
		ClaudeModelAliases: boolPtr(true),
	})
	if err != nil {
		t.Fatalf("UpdateToken(bearer, unrelated fields) = %v, want success", err)
	}
	if dto.Name != "plain-renamed" || !dto.LogCommunication || !dto.ClaudeModelAliases {
		t.Fatalf("unrelated update did not apply: %+v", dto)
	}
	if dto.ClaudeEffortCompatibility || storedClaudeEffortCompatibility(t, f.dir, plain.Token.ID) {
		t.Fatalf("an unrelated bearer update switched claude_effort_compatibility on: %+v", dto)
	}

	// A flag a management principal set stays on through a bearer's unrelated edit.
	managed, err := f.svc.CreateToken(ctx, f.owner, CreateTokenRequest{
		Name: "managed", Scopes: []string{"gateway:use"}, ClaudeEffortCompatibility: true,
	})
	if err != nil {
		t.Fatalf("seed CreateToken(session, effort on): %v", err)
	}
	dto, err = f.svc.UpdateToken(ctx, effortInferenceBearer(managed.Token.ID), managed.Token.ID, UpdateTokenRequest{LogCommunication: boolPtr(true)})
	if err != nil {
		t.Fatalf("UpdateToken(bearer on itself, unrelated field) = %v, want success", err)
	}
	if !dto.LogCommunication || !dto.ClaudeEffortCompatibility || !storedClaudeEffortCompatibility(t, f.dir, managed.Token.ID) {
		t.Fatalf("a bearer's unrelated edit cleared claude_effort_compatibility: %+v", dto)
	}
}

// TestCreateServiceTokenClaudeEffortCompatibilityRequiresManagementPrincipal is
// the service-token half. A delegate who reaches the service through a BEARER
// (an inference token of theirs) may not mint a token with the opt-in; the same
// delegate through a session, or an admin-scope bearer, may. A principal with
// no access to the service is still told "not found", not "forbidden".
func TestCreateServiceTokenClaudeEffortCompatibilityRequiresManagementPrincipal(t *testing.T) {
	ctx := context.Background()
	svc, dir, _, _ := newServiceAccountsTestService(t, time.Now().UTC())
	created := createTestService(t, ctx, svc)

	count := func() int {
		t.Helper()
		records, err := dir.TokensByService(ctx, created.ID)
		if err != nil {
			t.Fatalf("TokensByService: %v", err)
		}
		return len(records)
	}

	// svcFullToken() is a delegate session principal (ID == ""); the bearer
	// variants below are the same user presenting a token (ID != "").
	delegateBearer := svcFullToken()
	delegateBearer.ID = "tok_delegate_bearer"
	adminBearer := svcAdminToken()
	adminBearer.ID = "tok_admin_bearer"
	strangerBearer := svcStrangerToken()
	strangerBearer.ID = "tok_stranger_bearer"

	if _, err := svc.CreateServiceToken(ctx, delegateBearer, created.ID, CreateServiceTokenRequest{
		Name: "escalated", ClaudeEffortCompatibility: true,
	}); !errors.Is(err, ErrPrincipalForbidden) {
		t.Fatalf("CreateServiceToken(delegate bearer, effort on) = %v, want ErrPrincipalForbidden", err)
	}
	if n := count(); n != 0 {
		t.Fatalf("a rejected service-token create left %d token row(s), want 0", n)
	}

	if _, err := svc.CreateServiceToken(ctx, strangerBearer, created.ID, CreateServiceTokenRequest{
		Name: "probe", ClaudeEffortCompatibility: true,
	}); !errors.Is(err, ErrServiceNotFound) {
		t.Fatalf("CreateServiceToken(stranger bearer, effort on) = %v, want ErrServiceNotFound (no existence leak)", err)
	}

	// Scoped: the same bearer may still mint a service token WITHOUT the flag.
	plain, err := svc.CreateServiceToken(ctx, delegateBearer, created.ID, CreateServiceTokenRequest{Name: "plain"})
	if err != nil {
		t.Fatalf("CreateServiceToken(delegate bearer, flag omitted) = %v, want success", err)
	}
	if plain.Token.ClaudeEffortCompatibility || storedClaudeEffortCompatibility(t, dir, plain.Token.ID) {
		t.Fatalf("a service token minted without the flag reads on: %+v", plain.Token)
	}

	for name, principal := range map[string]auth.Token{
		"delegate session": svcFullToken(),
		"admin bearer":     adminBearer,
	} {
		on, err := svc.CreateServiceToken(ctx, principal, created.ID, CreateServiceTokenRequest{
			Name: "effort-" + name, ClaudeEffortCompatibility: true,
		})
		if err != nil {
			t.Fatalf("CreateServiceToken(%s, effort on) = %v, want success", name, err)
		}
		if !on.Token.ClaudeEffortCompatibility || !storedClaudeEffortCompatibility(t, dir, on.Token.ID) {
			t.Fatalf("CreateServiceToken(%s) did not persist the flag: %+v", name, on.Token)
		}
	}
}

// TestPrincipalMayManageTokenSetting pins the predicate itself, including the
// shapes that must NOT qualify: any bearer (non-empty ID) without the admin
// scope, whatever other scopes it holds.
func TestPrincipalMayManageTokenSetting(t *testing.T) {
	cases := []struct {
		name  string
		owner auth.Token
		want  bool
	}{
		{"session user (no token id)", auth.Token{UserID: "usr_1", Scopes: []string{"gateway:use"}}, true},
		{"session admin (no token id)", auth.Token{UserID: "usr_1", Scopes: []string{"gateway:use", "admin"}}, true},
		{"admin bearer", auth.Token{ID: "tok_1", UserID: "usr_1", Scopes: []string{"gateway:use", "admin"}}, true},
		{"system-and-admin bearer", auth.Token{ID: "tok_1", UserID: "usr_1", Scopes: []string{"gateway:use", "admin", "system"}}, true},
		{"inference bearer (gateway:use)", auth.Token{ID: "tok_1", UserID: "usr_1", Scopes: []string{"gateway:use"}}, false},
		{"inference bearer (llm:invoke)", auth.Token{ID: "tok_1", UserID: "usr_1", Scopes: []string{"llm:invoke"}}, false},
		{"bearer with no scopes", auth.Token{ID: "tok_1", UserID: "usr_1"}, false},
		{"bearer with system but not admin", auth.Token{ID: "tok_1", UserID: "usr_1", Scopes: []string{"gateway:use", "system"}}, false},
	}
	for _, tc := range cases {
		if got := principalMayManageTokenSetting(tc.owner); got != tc.want {
			t.Errorf("principalMayManageTokenSetting(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}
