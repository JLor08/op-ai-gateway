// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package auth

import "testing"

func TestTokenStoreImplementsBearerStore(t *testing.T) {
	var _ BearerStore = (*TokenStore)(nil)
}

func TestTokenStoreFindsActiveToken(t *testing.T) {
	store := NewTokenStore()
	store.AddPlainToken(Token{ID: "tok_1", UserID: "usr_1", Name: "dev token", Active: true}, "secret")

	token, ok := store.LookupBearer("Bearer secret")

	if !ok {
		t.Fatalf("LookupBearer returned ok=false")
	}
	if token.ID != "tok_1" || token.UserID != "usr_1" {
		t.Fatalf("token = %#v", token)
	}
}

func TestTokenStoreRekeyTokenMovesToNewHash(t *testing.T) {
	s := NewTokenStore()
	s.AddPlainToken(Token{ID: "tok_1", UserID: "u1", Active: true, Scopes: []string{"gateway:use"}}, "old-secret")

	s.RekeyToken("tok_1", HashSecret("new-secret"))

	if _, ok := s.LookupBearer("Bearer old-secret"); ok {
		t.Fatalf("old secret must stop resolving after rekey")
	}
	tok, ok := s.LookupBearer("Bearer new-secret")
	if !ok || tok.ID != "tok_1" {
		t.Fatalf("new secret lookup: ok=%v tok=%#v", ok, tok)
	}
	// Unknown id is a no-op.
	s.RekeyToken("tok_missing", HashSecret("z"))
}

// TestTokenStoreSetLastUsedModelUpdatesLookupWithoutTouchingOtherFields
// mirrors TestTokenStoreRekeyTokenMovesToNewHash's shape but for the narrow
// single-field mutator: the field changes, everything else (here: Name) is
// left as-is, and an unknown id is a documented no-op rather than a panic.
func TestTokenStoreSetLastUsedModelUpdatesLookupWithoutTouchingOtherFields(t *testing.T) {
	s := NewTokenStore()
	s.AddPlainToken(Token{ID: "tok_1", Name: "dev token", Active: true, LastUsedModel: "qwen3-32b"}, "secret")

	s.SetLastUsedModel("tok_1", "llama-70b")

	tok, ok := s.LookupBearer("Bearer secret")
	if !ok {
		t.Fatalf("LookupBearer returned ok=false")
	}
	if tok.LastUsedModel != "llama-70b" {
		t.Fatalf("LastUsedModel = %q, want %q", tok.LastUsedModel, "llama-70b")
	}
	if tok.Name != "dev token" {
		t.Fatalf("SetLastUsedModel disturbed Name: %q", tok.Name)
	}
	// Unknown id is a no-op.
	s.SetLastUsedModel("tok_missing", "z")
}

func TestTokenHasScope(t *testing.T) {
	token := Token{ID: "tok_dev", UserID: "usr_dev", Active: true, Scopes: []string{"gateway:use", "admin"}}

	if !token.HasScope("gateway:use") {
		t.Fatalf("HasScope(gateway:use) = false, want true")
	}
	if !token.HasScope("admin") {
		t.Fatalf("HasScope(admin) = false, want true")
	}
	if token.HasScope("agent:report") {
		t.Fatalf("HasScope(agent:report) = true, want false")
	}
}

func TestMemoryTokenStoreReturnsScopes(t *testing.T) {
	store := NewTokenStore()
	store.AddPlainToken(Token{ID: "tok_dev", UserID: "usr_dev", Active: true, Scopes: []string{"gateway:use"}}, "secret")

	token, ok := store.LookupBearer("Bearer secret")

	if !ok {
		t.Fatalf("LookupBearer returned ok=false")
	}
	if len(token.Scopes) != 1 || token.Scopes[0] != "gateway:use" {
		t.Fatalf("Scopes = %#v, want [gateway:use]", token.Scopes)
	}
	token.Scopes[0] = "mutated"
	again, ok := store.LookupBearer("Bearer secret")
	if !ok {
		t.Fatalf("second LookupBearer returned ok=false")
	}
	if again.Scopes[0] != "gateway:use" {
		t.Fatalf("stored scopes were mutated: %#v", again.Scopes)
	}
}

func TestTokenStoreRejectsInactiveToken(t *testing.T) {
	store := NewTokenStore()
	store.AddPlainToken(Token{ID: "tok_1", UserID: "usr_1", Name: "disabled", Active: false}, "secret")

	_, ok := store.LookupBearer("Bearer secret")

	if ok {
		t.Fatalf("LookupBearer returned ok=true for inactive token")
	}
}

func TestTokenStoreRejectsMalformedHeader(t *testing.T) {
	store := NewTokenStore()

	_, ok := store.LookupBearer("secret")

	if ok {
		t.Fatalf("LookupBearer returned ok=true for malformed header")
	}
}

func TestTokenStoreRejectsUnknownBearerSecret(t *testing.T) {
	store := NewTokenStore()
	store.AddPlainToken(Token{ID: "tok_1", UserID: "usr_1", Name: "dev token", Active: true}, "secret")

	_, ok := store.LookupBearer("Bearer other-secret")

	if ok {
		t.Fatalf("LookupBearer returned ok=true for unknown bearer secret")
	}
}

func TestTokenStoreRejectsBlankBearerSecret(t *testing.T) {
	store := NewTokenStore()

	_, ok := store.LookupBearer("Bearer   ")

	if ok {
		t.Fatalf("LookupBearer returned ok=true for blank bearer secret")
	}
}

func TestExtractBearerSecretTrimsBearerValue(t *testing.T) {
	secret, ok := ExtractBearerSecret("Bearer   secret-value  ")

	if !ok {
		t.Fatalf("ExtractBearerSecret returned ok=false")
	}
	if secret != "secret-value" {
		t.Fatalf("secret = %q, want secret-value", secret)
	}
}

func TestHashSecretIsStableSHA256Hex(t *testing.T) {
	hash := HashSecret("secret")

	if hash != "2bb80d537b1da3e38bd30361aa855686bde0eacd7162fef6a25fe97bf527a25b" {
		t.Fatalf("hash = %q", hash)
	}
}

func TestTokenStoreUpdateTokenReflectsInLookup(t *testing.T) {
	store := NewTokenStore()
	store.AddPlainToken(Token{ID: "tok_1", UserID: "usr_1", Name: "Old", Active: true, Scopes: []string{"gateway:use"}}, "secret-1")

	store.UpdateToken(Token{ID: "tok_1", UserID: "usr_1", Name: "New", Active: true, Scopes: []string{"gateway:use", "admin"}})

	got, ok := store.LookupBearer("Bearer secret-1")
	if !ok {
		t.Fatalf("LookupBearer returned ok=false")
	}
	if got.Name != "New" || !got.HasScope("admin") {
		t.Fatalf("token = %#v", got)
	}
}

func TestTokenStoreUpdateTokenCanDisable(t *testing.T) {
	store := NewTokenStore()
	store.AddPlainToken(Token{ID: "tok_1", UserID: "usr_1", Name: "T", Active: true, Scopes: []string{"gateway:use"}}, "secret-1")

	store.UpdateToken(Token{ID: "tok_1", UserID: "usr_1", Name: "T", Active: false, Scopes: []string{"gateway:use"}})

	if _, ok := store.LookupBearer("Bearer secret-1"); ok {
		t.Fatalf("disabled token should not authenticate")
	}
}

func TestTokenStoreRemoveToken(t *testing.T) {
	store := NewTokenStore()
	store.AddPlainToken(Token{ID: "tok_1", UserID: "usr_1", Name: "T", Active: true, Scopes: []string{"gateway:use"}}, "secret-1")

	store.RemoveToken("tok_1")

	if _, ok := store.LookupBearer("Bearer secret-1"); ok {
		t.Fatalf("removed token should not authenticate")
	}
}
