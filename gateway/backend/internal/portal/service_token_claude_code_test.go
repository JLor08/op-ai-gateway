// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/base64"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/store"
	"regexp"
	"strings"
	"testing"
	"time"
)

var (
	opaqueSecretRE = regexp.MustCompile(`^opaigw_[0-9a-f]{64}$`)
	claudeCodeRE   = regexp.MustCompile(`^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.opaigw_[0-9a-f]{64}$`)
)

// ccFixture is a Service over a memory directory whose bearer store the test
// can authenticate against, with the real secret generator.
type ccFixture struct {
	svc   *Service
	dir   *MemoryDirectory
	auth  *auth.TokenStore
	owner auth.Token
}

func newCCFixture(t *testing.T) *ccFixture {
	t.Helper()
	ctx := context.Background()
	bearers := auth.NewTokenStore()
	dir := NewMemoryDirectory(bearers)
	if err := dir.CreateUser(ctx, store.User{
		ID: "usr_1", Email: "u1@example.test", DisplayName: "U1", Role: "user",
		Status: store.UserStatusActive, PreferredLanguage: "de",
		CreatedAt: offeringTime, UpdatedAt: offeringTime,
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	svc := NewService(ServiceDeps{Users: dir, Tokens: dir, Clock: func() time.Time { return offeringTime }})
	return &ccFixture{svc: svc, dir: dir, auth: bearers, owner: auth.Token{UserID: "usr_1", Scopes: []string{"gateway:use"}}}
}

func (f *ccFixture) authenticates(secret string) (auth.Token, bool) {
	return f.auth.LookupBearer("Bearer " + secret)
}

func (f *ccFixture) stored(t *testing.T, id string) store.TokenRecord {
	t.Helper()
	rec, err := f.dir.TokenByID(context.Background(), id)
	if err != nil {
		t.Fatalf("TokenByID(%s): %v", id, err)
	}
	return rec
}

func decodeSegment(t *testing.T, seg string) string {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		t.Fatalf("segment %q is not base64url: %v", seg, err)
	}
	return string(b)
}

func TestCreateTokenClaudeCodeUsageDisplaySecretShape(t *testing.T) {
	ctx := context.Background()
	f := newCCFixture(t)

	on, err := f.svc.CreateToken(ctx, f.owner, CreateTokenRequest{
		Name: "cc", Scopes: []string{"gateway:use"}, ClaudeCodeUsageDisplay: true,
	})
	if err != nil {
		t.Fatalf("CreateToken(on): %v", err)
	}
	parts := strings.Split(on.Secret, ".")
	if len(parts) != 3 {
		t.Fatalf("secret %q has %d dot-separated parts, want 3", on.Secret, len(parts))
	}
	if got := decodeSegment(t, parts[0]); got != `{"alg":"none","typ":"JWT"}` {
		t.Errorf("header = %s", got)
	}
	if got := decodeSegment(t, parts[1]); got != `{"aud":"claude-gateway"}` {
		t.Errorf("payload = %s", got)
	}
	if !opaqueSecretRE.MatchString(parts[2]) {
		t.Errorf("third segment %q is not an opaigw_ secret", parts[2])
	}
	if !on.Token.ClaudeCodeUsageDisplay {
		t.Errorf("DTO claude_code_usage_display = false, want true")
	}
	if stored := f.stored(t, on.Token.ID); !stored.ClaudeCodeUsageDisplay || stored.SecretHash != auth.HashSecret(on.Secret) {
		t.Errorf("stored record = %+v, want the flag and the hash of the full secret", stored)
	}
	// The whole string is the bearer: the gateway authenticates it opaquely.
	if resolved, ok := f.authenticates(on.Secret); !ok || resolved.ID != on.Token.ID {
		t.Errorf("bearer lookup ok=%v token=%+v", ok, resolved)
	}

	off, err := f.svc.CreateToken(ctx, f.owner, CreateTokenRequest{Name: "plain", Scopes: []string{"gateway:use"}})
	if err != nil {
		t.Fatalf("CreateToken(off): %v", err)
	}
	if !opaqueSecretRE.MatchString(off.Secret) || off.Token.ClaudeCodeUsageDisplay {
		t.Errorf("flag off: secret %q dto=%+v, want an opaque secret", off.Secret, off.Token)
	}
}

func TestRotateTokenKeepsAndSwitchesShape(t *testing.T) {
	ctx := context.Background()
	f := newCCFixture(t)

	plain, err := f.svc.CreateToken(ctx, f.owner, CreateTokenRequest{Name: "p", Scopes: []string{"gateway:use"}})
	if err != nil {
		t.Fatal(err)
	}
	cc, err := f.svc.CreateToken(ctx, f.owner, CreateTokenRequest{Name: "c", Scopes: []string{"gateway:use"}, ClaudeCodeUsageDisplay: true})
	if err != nil {
		t.Fatal(err)
	}

	// rotate(nil) keeps the shape both ways.
	rotatedPlain, err := f.svc.RotateToken(ctx, f.owner, plain.Token.ID, RotateTokenRequest{})
	if err != nil {
		t.Fatalf("RotateToken(plain): %v", err)
	}
	if !opaqueSecretRE.MatchString(rotatedPlain.Secret) || rotatedPlain.Token.ClaudeCodeUsageDisplay {
		t.Errorf("plain rotation changed the shape: %q %+v", rotatedPlain.Secret, rotatedPlain.Token)
	}
	if _, ok := f.authenticates(plain.Secret); ok {
		t.Error("old plain secret still authenticates")
	}
	rotatedCC, err := f.svc.RotateToken(ctx, f.owner, cc.Token.ID, RotateTokenRequest{})
	if err != nil {
		t.Fatalf("RotateToken(cc): %v", err)
	}
	if !claudeCodeRE.MatchString(rotatedCC.Secret) || !strings.HasPrefix(rotatedCC.Secret, claudeCodeTokenSecretPrefix) || !rotatedCC.Token.ClaudeCodeUsageDisplay {
		t.Errorf("claude-code rotation lost the shape: %q %+v", rotatedCC.Secret, rotatedCC.Token)
	}
	if _, ok := f.authenticates(cc.Secret); ok {
		t.Error("old claude-code secret still authenticates")
	}

	// rotate(true) on an opaque token -> JWT shape and stored true.
	toOn, err := f.svc.RotateToken(ctx, f.owner, plain.Token.ID, RotateTokenRequest{ClaudeCodeUsageDisplay: boolPtr(true)})
	if err != nil {
		t.Fatalf("RotateToken(->on): %v", err)
	}
	if !strings.HasPrefix(toOn.Secret, claudeCodeTokenSecretPrefix) || !toOn.Token.ClaudeCodeUsageDisplay || !f.stored(t, plain.Token.ID).ClaudeCodeUsageDisplay {
		t.Errorf("rotate(true): %q %+v", toOn.Secret, toOn.Token)
	}
	if _, ok := f.authenticates(rotatedPlain.Secret); ok {
		t.Error("previous secret still authenticates after rotate(true)")
	}
	if resolved, ok := f.authenticates(toOn.Secret); !ok || resolved.ID != plain.Token.ID {
		t.Errorf("new secret does not authenticate: ok=%v", ok)
	}

	// rotate(false) -> opaque and stored false.
	toOff, err := f.svc.RotateToken(ctx, f.owner, plain.Token.ID, RotateTokenRequest{ClaudeCodeUsageDisplay: boolPtr(false)})
	if err != nil {
		t.Fatalf("RotateToken(->off): %v", err)
	}
	if !opaqueSecretRE.MatchString(toOff.Secret) || toOff.Token.ClaudeCodeUsageDisplay || f.stored(t, plain.Token.ID).ClaudeCodeUsageDisplay {
		t.Errorf("rotate(false): %q %+v", toOff.Secret, toOff.Token)
	}
	if _, ok := f.authenticates(toOn.Secret); ok {
		t.Error("JWT-shaped secret still authenticates after rotate(false)")
	}
}

func TestUpdateTokenClaudeCodeUsageDisplayRequiresRotation(t *testing.T) {
	ctx := context.Background()
	f := newCCFixture(t)
	cc, err := f.svc.CreateToken(ctx, f.owner, CreateTokenRequest{Name: "c", Scopes: []string{"gateway:use"}, ClaudeCodeUsageDisplay: true})
	if err != nil {
		t.Fatal(err)
	}
	before := f.stored(t, cc.Token.ID)

	// The same value is a no-op next to a real change.
	dto, err := f.svc.UpdateToken(ctx, f.owner, cc.Token.ID, UpdateTokenRequest{ClaudeCodeUsageDisplay: boolPtr(true), LogCommunication: boolPtr(true)})
	if err != nil {
		t.Fatalf("UpdateToken(same value): %v", err)
	}
	if !dto.ClaudeCodeUsageDisplay || !f.stored(t, cc.Token.ID).ClaudeCodeUsageDisplay {
		t.Fatalf("same value changed the flag: %+v", dto)
	}

	// A different value is refused and the whole request writes nothing.
	f2 := f.stored(t, cc.Token.ID)
	_, err = f.svc.UpdateToken(ctx, f.owner, cc.Token.ID, UpdateTokenRequest{ClaudeCodeUsageDisplay: boolPtr(false), Name: strPtr("renamed")})
	if !errors.Is(err, ErrTokenClaudeCodeUsageDisplayRequiresRotation) {
		t.Fatalf("UpdateToken(different value) = %v, want ErrTokenClaudeCodeUsageDisplayRequiresRotation", err)
	}
	after := f.stored(t, cc.Token.ID)
	if after != f2 || after.Name != before.Name || !after.ClaudeCodeUsageDisplay {
		t.Fatalf("refused update wrote something: before=%+v after=%+v", f2, after)
	}
	// ...and the other direction on an opaque token.
	plain, err := f.svc.CreateToken(ctx, f.owner, CreateTokenRequest{Name: "p", Scopes: []string{"gateway:use"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.UpdateToken(ctx, f.owner, plain.Token.ID, UpdateTokenRequest{ClaudeCodeUsageDisplay: boolPtr(true)}); !errors.Is(err, ErrTokenClaudeCodeUsageDisplayRequiresRotation) {
		t.Fatalf("UpdateToken(false->true) = %v, want the sentinel", err)
	}
	if f.stored(t, plain.Token.ID).ClaudeCodeUsageDisplay {
		t.Fatal("refused update switched the flag on")
	}
}

func TestTwoClaudeCodeTokensOneUser(t *testing.T) {
	ctx := context.Background()
	f := newCCFixture(t)
	a, err := f.svc.CreateToken(ctx, f.owner, CreateTokenRequest{Name: "a", Scopes: []string{"gateway:use"}, ClaudeCodeUsageDisplay: true})
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	b, err := f.svc.CreateToken(ctx, f.owner, CreateTokenRequest{Name: "b", Scopes: []string{"gateway:use"}, ClaudeCodeUsageDisplay: true})
	if err != nil {
		t.Fatalf("create b: %v", err)
	}
	if a.Secret == b.Secret {
		t.Fatal("two tokens share a secret")
	}
	list, err := f.svc.ListTokens(ctx, f.owner)
	if err != nil {
		t.Fatal(err)
	}
	flagged := 0
	for _, tok := range list.Data {
		if tok.ClaudeCodeUsageDisplay {
			flagged++
		}
	}
	if flagged != 2 {
		t.Fatalf("listed %d claude-code tokens, want 2", flagged)
	}
	for name, tok := range map[string]CreateTokenResponse{"a": a, "b": b} {
		if resolved, ok := f.authenticates(tok.Secret); !ok || resolved.ID != tok.Token.ID {
			t.Fatalf("token %s does not authenticate as itself: ok=%v %+v", name, ok, resolved)
		}
	}
	rotatedA, err := f.svc.RotateToken(ctx, f.owner, a.Token.ID, RotateTokenRequest{})
	if err != nil {
		t.Fatalf("rotate a: %v", err)
	}
	if _, ok := f.authenticates(a.Secret); ok {
		t.Error("a's old secret still authenticates")
	}
	if resolved, ok := f.authenticates(b.Secret); !ok || resolved.ID != b.Token.ID {
		t.Error("rotating a broke b")
	}
	if resolved, ok := f.authenticates(rotatedA.Secret); !ok || resolved.ID != a.Token.ID {
		t.Error("a's new secret does not authenticate")
	}
}
