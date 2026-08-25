// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/store"
	"testing"
	"time"
)

func TestMemoryDirectoryFindsUsersAndTokens(t *testing.T) {
	authStore := auth.NewTokenStore()
	dir := NewMemoryDirectory(authStore)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	user := store.User{ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev User", Role: "admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}
	token := store.TokenRecord{ID: "tok_dev", UserID: "usr_dev", Name: "Dev Token", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}

	dir.AddUser(user)
	if err := dir.CreatePlainToken(context.Background(), token, "dev-secret"); err != nil {
		t.Fatalf("CreatePlainToken returned %v", err)
	}

	gotUser, err := dir.UserByID(context.Background(), "usr_dev")
	if err != nil {
		t.Fatalf("UserByID returned %v", err)
	}
	if gotUser.Email != "dev@example.test" {
		t.Fatalf("Email = %q", gotUser.Email)
	}

	tokens, err := dir.TokensByUser(context.Background(), "usr_dev")
	if err != nil {
		t.Fatalf("TokensByUser returned %v", err)
	}
	if len(tokens) != 1 || tokens[0].ID != "tok_dev" {
		t.Fatalf("tokens = %#v", tokens)
	}
	if tokens[0].SecretHash == "" {
		t.Fatalf("SecretHash is empty")
	}
	if tokens[0].SecretPrefix != "dev-secr" {
		t.Fatalf("SecretPrefix = %q, want dev-secr", tokens[0].SecretPrefix)
	}

	authToken, ok := authStore.LookupBearer("Bearer dev-secret")
	if !ok {
		t.Fatalf("auth store did not accept created token")
	}
	if authToken.ID != "tok_dev" || authToken.UserID != "usr_dev" {
		t.Fatalf("auth token = %#v", authToken)
	}
}

func TestMemoryDirectoryRejectsExpiredBearerTokens(t *testing.T) {
	authStore := auth.NewTokenStore()
	dir := NewMemoryDirectory(authStore)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	expiredAt := now.Add(-time.Minute)
	token := store.TokenRecord{
		ID:        "tok_expired",
		UserID:    "usr_dev",
		Name:      "Expired Token",
		Status:    store.TokenStatusActive,
		Scopes:    `["gateway:use"]`,
		ExpiresAt: &expiredAt,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := dir.CreatePlainToken(context.Background(), token, "expired-secret"); err != nil {
		t.Fatalf("CreatePlainToken returned %v", err)
	}

	if _, ok := authStore.LookupBearer("Bearer expired-secret"); ok {
		t.Fatalf("auth store accepted expired token")
	}
}

func TestMemoryDirectoryCreatePlainTokenCopiesScopesToAuthStore(t *testing.T) {
	authStore := auth.NewTokenStore()
	directory := NewMemoryDirectory(authStore)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	if err := directory.CreatePlainToken(context.Background(), store.TokenRecord{
		ID:        "tok_scope",
		UserID:    "usr_dev",
		Name:      "Scoped",
		Status:    store.TokenStatusActive,
		Scopes:    `["gateway:use","admin"]`,
		CreatedAt: now,
		UpdatedAt: now,
	}, "secret"); err != nil {
		t.Fatalf("CreatePlainToken returned %v", err)
	}

	token, ok := authStore.LookupBearer("Bearer secret")

	if !ok {
		t.Fatalf("LookupBearer returned ok=false")
	}
	if !token.HasScope("admin") {
		t.Fatalf("Scopes = %#v, want admin", token.Scopes)
	}
}

func TestMemoryDirectoryUsersAndSessions(t *testing.T) {
	ctx := context.Background()
	dir := NewMemoryDirectory(auth.NewTokenStore())
	now := time.Now().UTC()
	user := store.User{ID: "usr_m", Email: "M@Example.test", DisplayName: "M", Role: "user", Status: store.UserStatusInvited, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}
	if err := dir.CreateUser(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := dir.CreateUser(ctx, user); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate id should conflict, got %v", err)
	}
	byEmail, err := dir.UserByEmail(ctx, "m@example.test")
	if err != nil || byEmail.ID != "usr_m" {
		t.Fatalf("lookup by normalized email failed: %+v %v", byEmail, err)
	}
	byEmail.Status = store.UserStatusActive
	byEmail.PasswordHash = "hash"
	if err := dir.UpdateUser(ctx, byEmail); err != nil {
		t.Fatalf("update user: %v", err)
	}
	users, err := dir.ListUsers(ctx)
	if err != nil || len(users) != 1 || users[0].Status != store.UserStatusActive {
		t.Fatalf("list users unexpected: %+v %v", users, err)
	}

	if err := dir.CreateSession(ctx, store.Session{ID: "s1", UserID: "usr_m", SecretHash: "sh", CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastSeenAt: now}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	session, err := dir.SessionBySecret(ctx, "sh")
	if err != nil || session.ID != "s1" {
		t.Fatalf("session lookup failed: %+v %v", session, err)
	}
	if err := dir.DeleteSessionsByUser(ctx, "usr_m"); err != nil {
		t.Fatalf("delete sessions: %v", err)
	}
	if _, err := dir.SessionBySecret(ctx, "sh"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("session should be revoked, got %v", err)
	}

	if err := dir.CreateSetPasswordToken(ctx, store.SetPasswordToken{ID: "t1", UserID: "usr_m", SecretHash: "th", ExpiresAt: now.Add(time.Hour), CreatedAt: now}); err != nil {
		t.Fatalf("create set-password token: %v", err)
	}
	tok, err := dir.SetPasswordTokenBySecret(ctx, "th")
	if err != nil || tok.UsedAt != nil {
		t.Fatalf("set-password token lookup failed: %+v %v", tok, err)
	}
	if err := dir.MarkSetPasswordTokenUsed(ctx, "t1", now.Add(time.Minute)); err != nil {
		t.Fatalf("mark used: %v", err)
	}
	tok, _ = dir.SetPasswordTokenBySecret(ctx, "th")
	if tok.UsedAt == nil {
		t.Fatal("token should be used")
	}
}

func TestMemoryDirectoryUpdateTokenMetadataSyncsBearerStore(t *testing.T) {
	ctx := context.Background()
	authStore := auth.NewTokenStore()
	dir := NewMemoryDirectory(authStore)
	now := time.Now().UTC()
	if err := dir.CreatePlainToken(ctx, store.TokenRecord{ID: "tok_1", UserID: "usr_1", Name: "Old", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}, "secret-1"); err != nil {
		t.Fatalf("CreatePlainToken returned %v", err)
	}

	if err := dir.UpdateTokenMetadata(ctx, store.TokenRecord{ID: "tok_1", UserID: "usr_1", Name: "Old", Status: store.TokenStatusDisabled, Scopes: `["gateway:use"]`, UpdatedAt: now}); err != nil {
		t.Fatalf("UpdateTokenMetadata disable returned %v", err)
	}
	if _, ok := authStore.LookupBearer("Bearer secret-1"); ok {
		t.Fatalf("disabled token should not authenticate")
	}

	if err := dir.UpdateTokenMetadata(ctx, store.TokenRecord{ID: "tok_1", UserID: "usr_1", Name: "New", Status: store.TokenStatusActive, Scopes: `["gateway:use","admin"]`, UpdatedAt: now}); err != nil {
		t.Fatalf("UpdateTokenMetadata re-enable returned %v", err)
	}
	tok, ok := authStore.LookupBearer("Bearer secret-1")
	if !ok || tok.Name != "New" || !tok.HasScope("admin") {
		t.Fatalf("token = %#v ok=%v", tok, ok)
	}
	record, err := dir.TokenByID(ctx, "tok_1")
	if err != nil || record.Name != "New" || record.Status != store.TokenStatusActive {
		t.Fatalf("record = %#v err=%v", record, err)
	}
}

func TestMemoryDirectoryUpdateTokenMetadataUnknownIDReturnsNotFound(t *testing.T) {
	dir := NewMemoryDirectory(auth.NewTokenStore())
	err := dir.UpdateTokenMetadata(context.Background(), store.TokenRecord{ID: "tok_missing", Status: store.TokenStatusActive})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("UpdateTokenMetadata error = %v, want ErrNotFound", err)
	}
}

// TestMemoryDirectorySetTokenLastUsedModelSyncsBearerStore proves the memory
// driver's narrow LastUsedModel write reaches BOTH of its copies: the
// TokenRecord map (read via TokenByID) and the mirrored auth.Token the bearer
// store hands back on lookup (read via LookupBearer) — mirroring
// UpdateTokenMetadata's own carry-through, but for this single field.
func TestMemoryDirectorySetTokenLastUsedModelSyncsBearerStore(t *testing.T) {
	ctx := context.Background()
	authStore := auth.NewTokenStore()
	dir := NewMemoryDirectory(authStore)
	now := time.Now().UTC()
	if err := dir.CreatePlainToken(ctx, store.TokenRecord{
		ID: "tok_1", UserID: "usr_1", Name: "T", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`,
		CreatedAt: now, UpdatedAt: now, LastUsedModel: "qwen3-32b",
	}, "secret-1"); err != nil {
		t.Fatalf("CreatePlainToken returned %v", err)
	}

	if err := dir.SetTokenLastUsedModel(ctx, "tok_1", "llama-70b"); err != nil {
		t.Fatalf("SetTokenLastUsedModel returned %v", err)
	}

	record, err := dir.TokenByID(ctx, "tok_1")
	if err != nil {
		t.Fatalf("TokenByID returned %v", err)
	}
	if record.LastUsedModel != "llama-70b" {
		t.Fatalf("TokenRecord.LastUsedModel = %q, want %q", record.LastUsedModel, "llama-70b")
	}

	tok, ok := authStore.LookupBearer("Bearer secret-1")
	if !ok {
		t.Fatalf("LookupBearer returned ok=false")
	}
	if tok.LastUsedModel != "llama-70b" {
		t.Fatalf("bearer LastUsedModel = %q, want %q", tok.LastUsedModel, "llama-70b")
	}
}

func TestMemoryDirectorySetTokenLastUsedModelUnknownIDReturnsNotFound(t *testing.T) {
	dir := NewMemoryDirectory(auth.NewTokenStore())
	err := dir.SetTokenLastUsedModel(context.Background(), "tok_missing", "llama-70b")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("SetTokenLastUsedModel error = %v, want ErrNotFound", err)
	}
}

func TestMemoryDirectoryTokenModelOverride(t *testing.T) {
	ctx := context.Background()
	tokens := auth.NewTokenStore()
	dir := NewMemoryDirectory(tokens)
	now := time.Date(2026, 7, 14, 10, 0, 0, 0, time.UTC)
	dir.AddUser(store.User{ID: "usr_1", Email: "a@b.test", DisplayName: "A", Role: "user", Status: store.UserStatusActive, CreatedAt: now, UpdatedAt: now})

	rec := store.TokenRecord{ID: "tok_1", UserID: "usr_1", Name: "T", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now, ModelOverride: "gpt-oss-20b", LogCommunication: true}
	if err := dir.CreatePlainToken(ctx, rec, "sekret"); err != nil {
		t.Fatalf("CreatePlainToken: %v", err)
	}
	got, _ := dir.TokenByID(ctx, "tok_1")
	if got.ModelOverride != "gpt-oss-20b" || !got.LogCommunication {
		t.Fatalf("got %#v", got)
	}
	tok, ok := tokens.LookupBearer("Bearer sekret")
	if !ok || tok.ModelOverride != "gpt-oss-20b" || !tok.LogCommunication {
		t.Fatalf("bearer ModelOverride=%q LogCommunication=%v ok=%v", tok.ModelOverride, tok.LogCommunication, ok)
	}

	rec.ModelOverride = "qwen-coder"
	rec.UpdatedAt = now.Add(time.Minute)
	if err := dir.UpdateTokenMetadata(ctx, rec); err != nil {
		t.Fatalf("UpdateTokenMetadata: %v", err)
	}
	tok, ok = tokens.LookupBearer("Bearer sekret")
	if !ok || tok.ModelOverride != "qwen-coder" || !tok.LogCommunication {
		t.Fatalf("after update bearer ModelOverride=%q LogCommunication=%v ok=%v", tok.ModelOverride, tok.LogCommunication, ok)
	}
}

// TestMemoryDirectoryTokenRedirectSettingsCarryThrough proves the memory
// driver carries LastUsedModel/UnknownModelRedirect/UnknownModelRedirectBlocked/
// UnknownModelFallback into the mirrored auth.Token the bearer store hands back
// on lookup:
//   - CreatePlainToken's initial mirror carries all FOUR (checked via the FIRST
//     LookupBearer)
//   - UpdateTokenMetadata's selective field-copy + rebuilt mirror carries the
//     three SETTINGS (checked via the SECOND LookupBearer, after flipping each
//     of them). LastUsedModel is deliberately not among them — it has exactly
//     one writer, SetTokenLastUsedModel; see
//     TestMemoryDirectoryUpdateTokenMetadataLeavesLastUsedModel
//
// Unlike TestMemoryDirectoryTokenModelOverride (which only exercises
// ModelOverride/LogCommunication), this test's bearer-store assertions would
// keep passing at their PRE-update values even if the corresponding
// existing.X = token.X lines were deleted from UpdateTokenMetadata, UNLESS
// the create and update fixtures use DIFFERENT values for every field, which
// they do here on purpose.
func TestMemoryDirectoryTokenRedirectSettingsCarryThrough(t *testing.T) {
	ctx := context.Background()
	tokens := auth.NewTokenStore()
	dir := NewMemoryDirectory(tokens)
	now := time.Date(2026, 7, 14, 10, 0, 0, 0, time.UTC)
	dir.AddUser(store.User{ID: "usr_1", Email: "a@b.test", DisplayName: "A", Role: "user", Status: store.UserStatusActive, CreatedAt: now, UpdatedAt: now})

	rec := store.TokenRecord{
		ID: "tok_1", UserID: "usr_1", Name: "T", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now,
		LastUsedModel: "qwen3-32b", UnknownModelRedirect: true, UnknownModelRedirectBlocked: true, UnknownModelFallback: "fallback-model",
	}
	if err := dir.CreatePlainToken(ctx, rec, "sekret"); err != nil {
		t.Fatalf("CreatePlainToken: %v", err)
	}

	// CreatePlainToken's mirror carries the four fields onto the bearer entry.
	tok, ok := tokens.LookupBearer("Bearer sekret")
	if !ok {
		t.Fatalf("LookupBearer returned ok=false")
	}
	if tok.LastUsedModel != "qwen3-32b" || !tok.UnknownModelRedirect || !tok.UnknownModelRedirectBlocked || tok.UnknownModelFallback != "fallback-model" {
		t.Fatalf("after create, bearer token = %#v", tok)
	}

	// UpdateTokenMetadata flips every one of the four fields to a DIFFERENT
	// value than the create fixture used, so a missing copy/rebuild line
	// would leave the stale create-time value behind rather than accidentally
	// matching by coincidence.
	rec.UnknownModelRedirect = false
	rec.UnknownModelRedirectBlocked = false
	rec.UnknownModelFallback = "other-fallback"
	rec.UpdatedAt = now.Add(time.Minute)
	if err := dir.UpdateTokenMetadata(ctx, rec); err != nil {
		t.Fatalf("UpdateTokenMetadata: %v", err)
	}

	// The TokenRecord side (existing.X = token.X field copy).
	got, err := dir.TokenByID(ctx, "tok_1")
	if err != nil {
		t.Fatalf("TokenByID: %v", err)
	}
	if got.UnknownModelRedirect || got.UnknownModelRedirectBlocked || got.UnknownModelFallback != "other-fallback" {
		t.Fatalf("after update, TokenRecord = %#v", got)
	}

	// The mirrored auth.Token side (UpdateTokenMetadata's rebuilt bearer entry).
	tok2, ok := tokens.LookupBearer("Bearer sekret")
	if !ok {
		t.Fatalf("LookupBearer returned ok=false after update")
	}
	if tok2.UnknownModelRedirect {
		t.Fatalf("after update, bearer UnknownModelRedirect = true, want false")
	}
	if tok2.UnknownModelRedirectBlocked {
		t.Fatalf("after update, bearer UnknownModelRedirectBlocked = true, want false")
	}
	if tok2.UnknownModelFallback != "other-fallback" {
		t.Fatalf("after update, bearer UnknownModelFallback = %q, want %q", tok2.UnknownModelFallback, "other-fallback")
	}
}

// TestMemoryDirectoryUpdateTokenMetadataLeavesLastUsedModel is the memory
// driver's half of the same invariant the SQL driver carries (see
// TestSQLiteUpdateTokenMetadataLeavesLastUsedModel): the marker is written by
// SetTokenLastUsedModel alone, so a metadata update built from a stale record
// must not roll it back — in the stored record OR in the mirrored bearer entry
// the update rebuilds.
func TestMemoryDirectoryUpdateTokenMetadataLeavesLastUsedModel(t *testing.T) {
	ctx := context.Background()
	tokens := auth.NewTokenStore()
	dir := NewMemoryDirectory(tokens)
	now := time.Date(2026, 7, 14, 10, 0, 0, 0, time.UTC)
	dir.AddUser(store.User{ID: "usr_1", Email: "a@b.test", DisplayName: "A", Role: "user", Status: store.UserStatusActive, CreatedAt: now, UpdatedAt: now})
	rec := store.TokenRecord{ID: "tok_1", UserID: "usr_1", Name: "T", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}
	if err := dir.CreatePlainToken(ctx, rec, "sekret"); err != nil {
		t.Fatalf("CreatePlainToken: %v", err)
	}
	if err := dir.SetTokenLastUsedModel(ctx, "tok_1", "qwen3-32b"); err != nil {
		t.Fatalf("SetTokenLastUsedModel: %v", err)
	}
	stale := rec // read before the marker write: LastUsedModel is still ""
	stale.Name = "renamed"
	stale.UpdatedAt = now.Add(time.Minute)
	if err := dir.UpdateTokenMetadata(ctx, stale); err != nil {
		t.Fatalf("UpdateTokenMetadata: %v", err)
	}
	got, err := dir.TokenByID(ctx, "tok_1")
	if err != nil {
		t.Fatalf("TokenByID: %v", err)
	}
	if got.LastUsedModel != "qwen3-32b" {
		t.Fatalf("TokenRecord LastUsedModel = %q, want qwen3-32b (metadata update reverted the marker)", got.LastUsedModel)
	}
	if got.Name != "renamed" {
		t.Fatalf("Name = %q, want renamed (the update itself must still apply)", got.Name)
	}
	tok, ok := tokens.LookupBearer("Bearer sekret")
	if !ok {
		t.Fatalf("LookupBearer returned ok=false")
	}
	if tok.LastUsedModel != "qwen3-32b" {
		t.Fatalf("bearer LastUsedModel = %q, want qwen3-32b", tok.LastUsedModel)
	}
}

func TestMemoryDirectoryTokenSecret(t *testing.T) {
	ctx := context.Background()
	tokens := auth.NewTokenStore()
	dir := NewMemoryDirectory(tokens)
	now := time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC)
	dir.AddUser(store.User{ID: "usr_1", Email: "a@b.test", DisplayName: "A", Role: "user", Status: store.UserStatusActive, CreatedAt: now, UpdatedAt: now})

	rec := store.TokenRecord{ID: "tok_1", UserID: "usr_1", Name: "T", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now, Secret: true}
	if err := dir.CreatePlainToken(ctx, rec, "sekret"); err != nil {
		t.Fatalf("CreatePlainToken: %v", err)
	}
	got, _ := dir.TokenByID(ctx, "tok_1")
	if !got.Secret {
		t.Fatalf("got %#v", got)
	}
	tok, ok := tokens.LookupBearer("Bearer sekret")
	if !ok || !tok.Secret {
		t.Fatalf("bearer Secret=%v ok=%v", tok.Secret, ok)
	}

	rec.Secret = false
	rec.UpdatedAt = now.Add(time.Minute)
	if err := dir.UpdateTokenMetadata(ctx, rec); err != nil {
		t.Fatalf("UpdateTokenMetadata: %v", err)
	}
	tok, ok = tokens.LookupBearer("Bearer sekret")
	if !ok || tok.Secret {
		t.Fatalf("after update bearer Secret=%v ok=%v", tok.Secret, ok)
	}
}

func TestMemoryDirectoryDeleteTokenRemovesFromBearerStore(t *testing.T) {
	ctx := context.Background()
	authStore := auth.NewTokenStore()
	dir := NewMemoryDirectory(authStore)
	now := time.Now().UTC()
	if err := dir.CreatePlainToken(ctx, store.TokenRecord{ID: "tok_1", UserID: "usr_1", Name: "T", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}, "secret-1"); err != nil {
		t.Fatalf("CreatePlainToken returned %v", err)
	}

	if err := dir.DeleteToken(ctx, "tok_1"); err != nil {
		t.Fatalf("DeleteToken returned %v", err)
	}

	if _, err := dir.TokenByID(ctx, "tok_1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("TokenByID after delete = %v, want ErrNotFound", err)
	}
	if _, ok := authStore.LookupBearer("Bearer secret-1"); ok {
		t.Fatalf("deleted token should not authenticate")
	}
	if err := dir.DeleteToken(ctx, "tok_missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delete unknown = %v, want ErrNotFound", err)
	}
}

func TestMemoryDirectoryRotateTokenSecretSyncsBearerStore(t *testing.T) {
	ctx := context.Background()
	authStore := auth.NewTokenStore()
	dir := NewMemoryDirectory(authStore)
	now := time.Date(2026, 7, 17, 10, 0, 0, 0, time.UTC)
	if err := dir.CreatePlainToken(ctx, store.TokenRecord{ID: "tok_1", UserID: "usr_1", Name: "T", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}, "old-secret"); err != nil {
		t.Fatalf("CreatePlainToken returned %v", err)
	}

	rotatedAt := now.Add(time.Hour)
	if err := dir.RotateTokenSecret(ctx, "tok_1", auth.HashSecret("new-secret"), "new-secr", rotatedAt); err != nil {
		t.Fatalf("RotateTokenSecret returned %v", err)
	}

	if _, ok := authStore.LookupBearer("Bearer old-secret"); ok {
		t.Fatalf("old secret must stop authenticating after rotate")
	}
	tok, ok := authStore.LookupBearer("Bearer new-secret")
	if !ok || tok.ID != "tok_1" {
		t.Fatalf("new secret lookup: ok=%v tok=%#v", ok, tok)
	}
	rec, err := dir.TokenByID(ctx, "tok_1")
	if err != nil || rec.SecretPrefix != "new-secr" || !rec.UpdatedAt.Equal(rotatedAt) {
		t.Fatalf("record = %#v err=%v", rec, err)
	}
}

func TestMemoryDirectoryRotateTokenSecretUnknownIDReturnsNotFound(t *testing.T) {
	dir := NewMemoryDirectory(auth.NewTokenStore())
	err := dir.RotateTokenSecret(context.Background(), "tok_missing", auth.HashSecret("x"), "x", time.Now().UTC())
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("RotateTokenSecret error = %v, want ErrNotFound", err)
	}
}
