// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/usage"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// This file is the shared store CONFORMANCE suite: every subtest below runs
// against BOTH dialects via forEachDialect — sqlite always (a fresh temp-file
// DB per subtest), postgres only when OP_AI_GATEWAY_TEST_POSTGRES_DSN is set
// (a schema-dropped, freshly migrated DB per subtest). It is the strongest
// proof that the unified store behaves identically on both dialects.

// forEachDialect runs run against a freshly migrated *SQLStore for each
// available dialect: sqlite unconditionally, postgres only when the test DSN
// env var is set (otherwise that subtest is skipped, not failed).
func forEachDialect(t *testing.T, run func(t *testing.T, s *SQLStore)) {
	t.Run("sqlite", func(t *testing.T) {
		s, err := OpenSQLite(filepath.Join(t.TempDir(), "c.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if err := s.Migrate(context.Background()); err != nil {
			t.Fatal(err)
		}
		run(t, s)
	})
	t.Run("postgres", func(t *testing.T) {
		dsn := os.Getenv("OP_AI_GATEWAY_TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("set OP_AI_GATEWAY_TEST_POSTGRES_DSN to run postgres conformance tests")
		}
		ctx := context.Background()
		s, err := OpenPostgres(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		// Clean slate so the suite is deterministic against a reused DB.
		if err := dropAllTables(ctx, s); err != nil {
			t.Fatal(err)
		}
		if err := s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		run(t, s)
	})
}

// dropAllTables gives postgres a clean slate (sqlite subtests already get one
// via a fresh temp file, so this is a no-op call site for sqlite — it is only
// ever invoked from the postgres branch above).
func dropAllTables(ctx context.Context, s *SQLStore) error {
	_, err := s.db.ExecContext(ctx, `drop schema public cascade; create schema public;`)
	return err
}

// deleteUserForTest deletes a user row directly (there is no public
// store.DeleteUser), so the FK-cascade test can trigger ON DELETE CASCADE.
func deleteUserForTest(ctx context.Context, s *SQLStore, id string) error {
	_, err := s.db.ExecContext(ctx, s.dl.rebind(`delete from users where id = ?`), id)
	return err
}

func newTestUser(id, email string, now time.Time) User {
	return User{
		ID:                id,
		Email:             email,
		DisplayName:       "Test User " + id,
		Role:              "user",
		Status:            UserStatusActive,
		PreferredLanguage: "de",
		CreatedAt:         now,
		UpdatedAt:         now,
	}
}

// --- 1. User CRUD + unique + not-found ------------------------------------

func TestConformanceUserCRUDAndUnique(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		u := newTestUser("u1", "a@example.test", now)
		if err := s.CreateUser(ctx, u); err != nil {
			t.Fatalf("create user: %v", err)
		}
		got, err := s.UserByID(ctx, "u1")
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		if got.Email != "a@example.test" || got.DisplayName != u.DisplayName {
			t.Fatalf("read back mismatch: %+v", got)
		}

		// dup email -> ErrConflict
		u2 := u
		u2.ID = "u2"
		if err := s.CreateUser(ctx, u2); err != ErrConflict {
			t.Fatalf("expected ErrConflict on dup email, got %v", err)
		}

		// missing -> ErrNotFound
		if _, err := s.UserByID(ctx, "nope"); err != ErrNotFound {
			t.Fatalf("expected ErrNotFound, got %v", err)
		}

		// update + list round-trip
		u.DisplayName = "Updated Name"
		u.UpdatedAt = now.Add(time.Minute)
		if err := s.UpdateUser(ctx, u); err != nil {
			t.Fatalf("update user: %v", err)
		}
		got, err = s.UserByID(ctx, "u1")
		if err != nil || got.DisplayName != "Updated Name" {
			t.Fatalf("update not applied: %v %+v", err, got)
		}
		list, err := s.ListUsers(ctx)
		if err != nil || len(list) != 1 {
			t.Fatalf("list users: %v %+v", err, list)
		}
	})
}

// --- 1b. User TOTP columns round-trip --------------------------------------

func TestConformanceUserTOTPRoundTrip(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		u := newTestUser("u-totp", "totp@example.test", now)
		if err := s.CreateUser(ctx, u); err != nil {
			t.Fatalf("create user: %v", err)
		}

		confirmedAt := now.Add(time.Minute)
		u.TOTPSecret = "enc:abc"
		u.TOTPPendingSecret = "enc:pending-abc"
		u.TOTPEnabled = true
		u.TOTPConfirmedAt = &confirmedAt
		u.UpdatedAt = now.Add(time.Minute)
		if err := s.UpdateUser(ctx, u); err != nil {
			t.Fatalf("update user (set totp): %v", err)
		}
		got, err := s.UserByID(ctx, "u-totp")
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		if got.TOTPSecret != "enc:abc" || got.TOTPPendingSecret != "enc:pending-abc" || !got.TOTPEnabled || got.TOTPConfirmedAt == nil ||
			!got.TOTPConfirmedAt.Equal(confirmedAt) {
			t.Fatalf("totp fields not persisted: %+v", got)
		}

		u.TOTPSecret = ""
		u.TOTPPendingSecret = ""
		u.TOTPEnabled = false
		u.TOTPConfirmedAt = nil
		u.UpdatedAt = now.Add(2 * time.Minute)
		if err := s.UpdateUser(ctx, u); err != nil {
			t.Fatalf("update user (clear totp): %v", err)
		}
		got, err = s.UserByID(ctx, "u-totp")
		if err != nil {
			t.Fatalf("read back after clear: %v", err)
		}
		if got.TOTPSecret != "" || got.TOTPPendingSecret != "" || got.TOTPEnabled || got.TOTPConfirmedAt != nil {
			t.Fatalf("totp fields not cleared: %+v", got)
		}
	})
}

// --- 2. FK cascade: deleting a user cascades to that user's chats ----------

func TestConformanceForeignKeyCascade(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		if err := s.CreateUser(ctx, newTestUser("u1", "b@example.test", now)); err != nil {
			t.Fatalf("create user: %v", err)
		}
		if err := s.CreateChat(ctx, Chat{
			ID: "c1", UserID: "u1", Title: "t", KeyVersion: 0,
			Blob: []byte("{}"), CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create chat: %v", err)
		}
		if _, err := s.ChatByID(ctx, "c1"); err != nil {
			t.Fatalf("chat should exist before delete: %v", err)
		}

		if err := deleteUserForTest(ctx, s, "u1"); err != nil {
			t.Fatalf("delete user: %v", err)
		}

		if _, err := s.ChatByID(ctx, "c1"); err != ErrNotFound {
			t.Fatalf("expected chat cascade-deleted, got %v", err)
		}
	})
}

// --- 3. Token repo + bearer -------------------------------------------------

func TestConformanceTokenRepoAndBearer(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		if err := s.CreateUser(ctx, newTestUser("u1", "tok@example.test", now)); err != nil {
			t.Fatalf("create user: %v", err)
		}

		tok := TokenRecord{
			ID: "tok1", UserID: "u1", Name: "primary", CreatedAt: now, UpdatedAt: now,
			ServerOverride: "srv_x", ServerOverrideForceUnreachable: true,
		}
		if err := s.CreatePlainToken(ctx, tok, "supersecretvalue1"); err != nil {
			t.Fatalf("create token: %v", err)
		}

		got, err := s.TokenByID(ctx, "tok1")
		if err != nil || got.UserID != "u1" || got.Status != TokenStatusActive {
			t.Fatalf("token by id: %v %+v", err, got)
		}
		if got.ServerOverride != "srv_x" || !got.ServerOverrideForceUnreachable {
			t.Fatalf("server_override not persisted on create: %+v", got)
		}

		byUser, err := s.TokensByUser(ctx, "u1")
		if err != nil || len(byUser) != 1 {
			t.Fatalf("tokens by user: %v %+v", err, byUser)
		}

		// dup secret_hash -> ErrConflict (same plaintext secret => same hash);
		// this row is never actually inserted.
		tokDup := TokenRecord{ID: "tokdup", UserID: "u1", Name: "dup", CreatedAt: now, UpdatedAt: now}
		if err := s.CreatePlainToken(ctx, tokDup, "supersecretvalue1"); err != ErrConflict {
			t.Fatalf("expected ErrConflict on dup secret hash, got %v", err)
		}

		// tok2 IS persisted (a distinct secret) and carries NO server override:
		// the empty-string/false defaults must survive round-trip (asserted
		// below), independent of tok1's non-empty override.
		tok2 := TokenRecord{ID: "tok2", UserID: "u1", Name: "no-override", CreatedAt: now, UpdatedAt: now}
		if err := s.CreatePlainToken(ctx, tok2, "supersecretvalue2"); err != nil {
			t.Fatalf("create token (no override): %v", err)
		}

		// LookupBearer resolves the token from the Authorization header and
		// carries the server-override fields onto the returned auth.Token.
		resolved, ok := s.LookupBearer("Bearer supersecretvalue1")
		if !ok || resolved.ID != "tok1" || !resolved.Active {
			t.Fatalf("lookup bearer: ok=%v resolved=%+v", ok, resolved)
		}
		if resolved.ServerOverride != "srv_x" || !resolved.ServerOverrideForceUnreachable {
			t.Fatalf("lookup bearer did not carry server_override: %+v", resolved)
		}

		// UpdateTokenMetadata changes an existing server_override in place...
		updated := got
		updated.ServerOverride = "srv_y"
		updated.ServerOverrideForceUnreachable = false
		updated.UpdatedAt = now.Add(time.Minute)
		if err := s.UpdateTokenMetadata(ctx, updated); err != nil {
			t.Fatalf("update token metadata (change override): %v", err)
		}
		afterChange, err := s.TokenByID(ctx, "tok1")
		if err != nil || afterChange.ServerOverride != "srv_y" || afterChange.ServerOverrideForceUnreachable {
			t.Fatalf("server_override change not persisted: %v %+v", err, afterChange)
		}
		// ...and clears it back to the empty-default via the same path.
		cleared := afterChange
		cleared.ServerOverride = ""
		cleared.ServerOverrideForceUnreachable = false
		cleared.UpdatedAt = now.Add(2 * time.Minute)
		if err := s.UpdateTokenMetadata(ctx, cleared); err != nil {
			t.Fatalf("update token metadata (clear override): %v", err)
		}
		afterClear, err := s.TokenByID(ctx, "tok1")
		if err != nil || afterClear.ServerOverride != "" || afterClear.ServerOverrideForceUnreachable {
			t.Fatalf("server_override not cleared: %v %+v", err, afterClear)
		}

		// The token created without a server override reads back the
		// empty-string/false defaults (never inherits tok1's values).
		gotTok2, err := s.TokenByID(ctx, "tok2")
		if err != nil || gotTok2.ServerOverride != "" || gotTok2.ServerOverrideForceUnreachable {
			t.Fatalf("token without override must default empty/false: %v %+v", err, gotTok2)
		}

		// Wrong secret does not resolve.
		if _, ok := s.LookupBearer("Bearer wrong-secret-value"); ok {
			t.Fatal("expected lookup bearer to fail for wrong secret")
		}

		// RotateTokenSecret replaces the secret in place: the old bearer stops
		// resolving, the new one resolves the same token id, secret_prefix updates.
		rotatedAt := now.Add(time.Hour)
		if err := s.RotateTokenSecret(ctx, "tok1", auth.HashSecret("rotated-secret-value"), "rotated-", rotatedAt); err != nil {
			t.Fatalf("rotate token secret: %v", err)
		}
		if _, ok := s.LookupBearer("Bearer supersecretvalue1"); ok {
			t.Fatal("old secret must stop working after rotate")
		}
		reRotated, ok := s.LookupBearer("Bearer rotated-secret-value")
		if !ok || reRotated.ID != "tok1" {
			t.Fatalf("rotated secret lookup: ok=%v resolved=%+v", ok, reRotated)
		}
		afterRotate, err := s.TokenByID(ctx, "tok1")
		if err != nil || afterRotate.SecretPrefix != "rotated-" {
			t.Fatalf("secret_prefix not updated: %v %+v", err, afterRotate)
		}
		// Missing id -> ErrNotFound.
		if err := s.RotateTokenSecret(ctx, "nope", auth.HashSecret("x"), "x", rotatedAt); err != ErrNotFound {
			t.Fatalf("rotate missing id = %v, want ErrNotFound", err)
		}
	})
}

// TestConformanceTokenRedirectSettingsRoundTrip proves the four new columns
// (last_used_model, unknown_model_redirect, unknown_model_redirect_blocked,
// unknown_model_fallback) round-trip through CreatePlainToken/TokenByID on
// both dialects, alongside the existing ModelOverrideMap rules codec.
func TestConformanceTokenRedirectSettingsRoundTrip(t *testing.T) {
	forEachDialect(t, testTokenRedirectSettingsRoundTrip)
}

func testTokenRedirectSettingsRoundTrip(t *testing.T, s *SQLStore) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	if err := s.CreateUser(ctx, newTestUser("usr_redirect", "redirect@example.test", now)); err != nil {
		t.Fatalf("create user: %v", err)
	}
	rec := TokenRecord{
		ID: "tok_redirect", UserID: "usr_redirect", Name: "redirect", CreatedAt: now, UpdatedAt: now,
		ModelOverrideMap:            EncodeModelOverrideRules(map[string]ModelOverrideRule{"gpt-4o": {To: "qwen3-32b", Offer: true}}),
		UnknownModelRedirect:        true,
		UnknownModelRedirectBlocked: true,
		UnknownModelFallback:        "fallback-model",
		LastUsedModel:               "qwen3-32b",
	}
	if err := s.CreatePlainToken(ctx, rec, "redirect-secret-value"); err != nil {
		t.Fatalf("CreatePlainToken: %v", err)
	}
	got, err := s.TokenByID(ctx, "tok_redirect")
	if err != nil {
		t.Fatalf("TokenByID: %v", err)
	}
	if !got.UnknownModelRedirect || !got.UnknownModelRedirectBlocked {
		t.Fatalf("switches lost: %+v", got)
	}
	if got.UnknownModelFallback != "fallback-model" || got.LastUsedModel != "qwen3-32b" {
		t.Fatalf("fallback/last-used lost: %+v", got)
	}
	rules := DecodeModelOverrideRules(got.ModelOverrideMap)
	if !rules["gpt-4o"].Offer || rules["gpt-4o"].To != "qwen3-32b" {
		t.Fatalf("rules lost: %#v", rules)
	}
}

// TestConformanceLegacyOverrideMapReadsThroughBothTokenPaths is the store-level
// half of the design's trickiest promise: a row written by the PRE-BRANCH
// binary, read back after migration 63, yields rules with both listing switches
// false and loses nothing.
//
// The decoder's own unit test proves the codec. This proves the COLUMN: the
// legacy JSON goes into api_tokens.model_override_map directly — the way an
// upgraded database really holds it, never through this branch's encoder — and
// comes back out through both real read paths, on every dialect. TokenByID and
// LookupBearer decode independently (one into a TokenRecord, one straight onto
// an auth.Token via AuthModelOverrideRules), so a decode wired into only one of
// them would pass every test that asks just one.
func TestConformanceLegacyOverrideMapReadsThroughBothTokenPaths(t *testing.T) {
	forEachDialect(t, testLegacyOverrideMapReadsThroughBothTokenPaths)
}

func testLegacyOverrideMapReadsThroughBothTokenPaths(t *testing.T, s *SQLStore) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	if err := s.CreateUser(ctx, newTestUser("usr_legacy", "legacy@example.test", now)); err != nil {
		t.Fatalf("create user: %v", err)
	}
	rec := TokenRecord{ID: "tok_legacy", UserID: "usr_legacy", Name: "legacy", CreatedAt: now, UpdatedAt: now}
	if err := s.CreatePlainToken(ctx, rec, "legacy-secret-value"); err != nil {
		t.Fatalf("CreatePlainToken: %v", err)
	}
	// Exactly what a pre-branch binary left in the column: a plain
	// requested->model string map, written past this branch's encoder on purpose.
	const legacy = `{"gpt-4o":"qwen3-32b","o3":"coder"}`
	if _, err := s.exec(ctx, `update api_tokens set model_override_map = ? where id = ?`, legacy, "tok_legacy"); err != nil {
		t.Fatalf("seed legacy column: %v", err)
	}
	want := map[string]ModelOverrideRule{"gpt-4o": {To: "qwen3-32b"}, "o3": {To: "coder"}}

	got, err := s.TokenByID(ctx, "tok_legacy")
	if err != nil {
		t.Fatalf("TokenByID: %v", err)
	}
	if got.ModelOverrideMap != legacy {
		t.Fatalf("TokenByID column = %q, want the untouched legacy value %q", got.ModelOverrideMap, legacy)
	}
	if rules := DecodeModelOverrideRules(got.ModelOverrideMap); !reflect.DeepEqual(rules, want) {
		t.Fatalf("TokenByID rules = %#v, want %#v", rules, want)
	}

	token, ok := s.LookupBearer("Bearer legacy-secret-value")
	if !ok {
		t.Fatal("LookupBearer failed for the legacy token")
	}
	if len(token.ModelOverrideRules) != len(want) {
		t.Fatalf("LookupBearer rules = %#v, want %#v", token.ModelOverrideRules, want)
	}
	for name, w := range want {
		rule, ok := token.ModelOverrideRules[name]
		if !ok {
			t.Fatalf("LookupBearer lost rule %q: %#v", name, token.ModelOverrideRules)
		}
		if rule.To != w.To {
			t.Fatalf("LookupBearer rule %q target = %q, want %q", name, rule.To, w.To)
		}
		// The load-bearing part: a legacy row must default BOTH switches to
		// false, so no pre-existing token's listing changes under the new binary.
		if rule.Offer || rule.HideTarget {
			t.Fatalf("legacy rule %q gained a listing switch: %#v", name, rule)
		}
	}
}

// TestConformanceTokenDefaultsUnchanged proves a token created without
// touching any of the new fields reads back exactly as it did before this
// feature: redirect off, no fallback, no last-used model.
func TestConformanceTokenDefaultsUnchanged(t *testing.T) {
	forEachDialect(t, testTokenDefaultsUnchanged)
}

func testTokenDefaultsUnchanged(t *testing.T, s *SQLStore) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	if err := s.CreateUser(ctx, newTestUser("usr_plain", "plain@example.test", now)); err != nil {
		t.Fatalf("create user: %v", err)
	}
	rec := TokenRecord{ID: "tok_plain", UserID: "usr_plain", Name: "plain", CreatedAt: now, UpdatedAt: now}
	if err := s.CreatePlainToken(ctx, rec, "plain-secret-value"); err != nil {
		t.Fatalf("CreatePlainToken: %v", err)
	}
	got, err := s.TokenByID(ctx, "tok_plain")
	if err != nil {
		t.Fatalf("TokenByID: %v", err)
	}
	if got.UnknownModelRedirect || got.UnknownModelRedirectBlocked ||
		got.UnknownModelFallback != "" || got.LastUsedModel != "" {
		t.Fatalf("defaults changed: %+v", got)
	}
}

// --- 4. Sessions + set-password tokens -------------------------------------

func TestConformanceSessionsAndSetPasswordTokens(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		if err := s.CreateUser(ctx, newTestUser("u1", "sess@example.test", now)); err != nil {
			t.Fatalf("create user: %v", err)
		}

		sess := Session{
			ID: "sess1", UserID: "u1", SecretHash: "hash-sess-1",
			CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour), LastSeenAt: now,
		}
		if err := s.CreateSession(ctx, sess); err != nil {
			t.Fatalf("create session: %v", err)
		}
		got, err := s.SessionBySecret(ctx, "hash-sess-1")
		if err != nil || got.ID != "sess1" {
			t.Fatalf("session by secret: %v %+v", err, got)
		}

		// elevation round-trip (system-admin step-up): set a future elevated_until,
		// read it back, then clear it (zero -> NULL).
		future := now.Add(15 * time.Minute).Truncate(time.Second)
		if err := s.SetSessionElevation(ctx, sess.ID, future); err != nil {
			t.Fatalf("SetSessionElevation: %v", err)
		}
		got, err = s.SessionBySecret(ctx, "hash-sess-1")
		if err != nil {
			t.Fatalf("SessionBySecret after elevate: %v", err)
		}
		if !got.ElevatedUntil.Equal(future) {
			t.Fatalf("ElevatedUntil = %v, want %v", got.ElevatedUntil, future)
		}
		if err := s.SetSessionElevation(ctx, sess.ID, time.Time{}); err != nil {
			t.Fatalf("SetSessionElevation clear: %v", err)
		}
		got, err = s.SessionBySecret(ctx, "hash-sess-1")
		if err != nil {
			t.Fatalf("SessionBySecret after clear: %v", err)
		}
		if !got.ElevatedUntil.IsZero() {
			t.Fatalf("ElevatedUntil after clear = %v, want zero", got.ElevatedUntil)
		}

		touched := now.Add(time.Minute)
		if err := s.TouchSession(ctx, "sess1", touched); err != nil {
			t.Fatalf("touch session: %v", err)
		}
		got, err = s.SessionBySecret(ctx, "hash-sess-1")
		if err != nil || !got.LastSeenAt.Equal(touched) {
			t.Fatalf("touch not applied: %v %+v", err, got)
		}
		if err := s.DeleteSession(ctx, "sess1"); err != nil {
			t.Fatalf("delete session: %v", err)
		}
		if _, err := s.SessionBySecret(ctx, "hash-sess-1"); err != ErrNotFound {
			t.Fatalf("expected ErrNotFound after delete, got %v", err)
		}

		spt := SetPasswordToken{
			ID: "spt1", UserID: "u1", SecretHash: "hash-spt-1",
			ExpiresAt: now.Add(time.Hour), CreatedAt: now,
		}
		if err := s.CreateSetPasswordToken(ctx, spt); err != nil {
			t.Fatalf("create set-password token: %v", err)
		}
		gotSPT, err := s.SetPasswordTokenBySecret(ctx, "hash-spt-1")
		if err != nil || gotSPT.ID != "spt1" || gotSPT.UsedAt != nil {
			t.Fatalf("set-password token by secret: %v %+v", err, gotSPT)
		}
		if err := s.MarkSetPasswordTokenUsed(ctx, "spt1", now.Add(time.Minute)); err != nil {
			t.Fatalf("mark used: %v", err)
		}
		gotSPT, err = s.SetPasswordTokenBySecret(ctx, "hash-spt-1")
		if err != nil || gotSPT.UsedAt == nil {
			t.Fatalf("used_at not persisted: %v %+v", err, gotSPT)
		}
	})
}

// --- 5. Routing: server -> application -> mapping + FK contract ------------

func TestConformanceRoutingServerApplicationMapping(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		srv := routing.AIServer{
			ID: "srv1", Name: "Server 1", Domain: "srv1.local", Provider: routing.ProviderOllama,
			Endpoint: "http://srv1.local:11434", Status: routing.ServerStatusActive,
			HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateAIServer(ctx, srv); err != nil {
			t.Fatalf("create server: %v", err)
		}

		app := routing.Application{
			ID: "app1", ServerID: "srv1", Type: "ollama", Port: 11434, Scheme: "http",
			APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 1, Weight: 1,
			TimeoutMS: 30000, AffinityTTLSeconds: 300, Status: routing.ServerStatusActive,
			HealthCheckMode: routing.HealthCheckModeAlwaysReachable,
			CreatedAt:       now, UpdatedAt: now,
		}
		if err := s.CreateApplication(ctx, app); err != nil {
			t.Fatalf("create application: %v", err)
		}

		mapping := routing.ModelMapping{
			ID: "map1", ApplicationID: "app1", GatewayModelName: "gpt-4o-mini",
			AppModelName: "llama3", Status: routing.ServerStatusActive,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateMapping(ctx, mapping); err != nil {
			t.Fatalf("create mapping: %v", err)
		}

		candidates, err := s.ActiveMappingsForModel(ctx, "gpt-4o-mini", routing.APIFlavorOpenAI)
		if err != nil {
			t.Fatalf("active mappings: %v", err)
		}
		if len(candidates) != 1 {
			t.Fatalf("expected 1 candidate, got %d: %+v", len(candidates), candidates)
		}
		c := candidates[0]
		if c.Server.ID != "srv1" || c.Application.ID != "app1" || c.Mapping.ID != "map1" {
			t.Fatalf("unexpected candidate: %+v", c)
		}

		// A flavor the application does not serve yields no candidates.
		none, err := s.ActiveMappingsForModel(ctx, "gpt-4o-mini", routing.APIFlavorAnthropic)
		if err != nil {
			t.Fatalf("active mappings (other flavor): %v", err)
		}
		if len(none) != 0 {
			t.Fatalf("expected 0 candidates for unserved flavor, got %d", len(none))
		}

		// FK violation: an application referencing a missing server. The store's
		// contract (see sqlite_applications.go CreateApplication) is to surface
		// this as ErrNotFound, distinct from a plain unique-constraint ErrConflict.
		orphan := routing.Application{
			ID: "app-orphan", ServerID: "does-not-exist", Type: "ollama", Port: 9999,
			Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI},
			Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateApplication(ctx, orphan); err != ErrNotFound {
			t.Fatalf("expected ErrNotFound for FK violation on missing server, got %v", err)
		}

		// A genuine unique violation (duplicate port on the same server) must
		// still classify as ErrConflict, not ErrNotFound.
		dupPort := routing.Application{
			ID: "app-dup-port", ServerID: "srv1", Type: "ollama", Port: 11434,
			Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI},
			Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateApplication(ctx, dupPort); err != ErrConflict {
			t.Fatalf("expected ErrConflict for dup (server_id, port), got %v", err)
		}
	})
}

// TestConformanceApplicationNativePassthroughFlags verifies the per-application
// native-passthrough flags round-trip through create, update, direct read, and the
// routing join on both dialects.
func TestConformanceApplicationNativePassthroughFlags(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		if err := s.CreateAIServer(ctx, routing.AIServer{
			ID: "srv1", Name: "Server 1", Domain: "srv1.local", Provider: routing.ProviderVLLM,
			Endpoint: "http://srv1.local:8000", Status: routing.ServerStatusActive,
			HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create server: %v", err)
		}
		app := routing.Application{
			ID: "app1", ServerID: "srv1", Type: "vllm", Port: 8000, Scheme: "http",
			APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 1, Weight: 1,
			TimeoutMS: 30000, AffinityTTLSeconds: 300, Status: routing.ServerStatusActive,
			HealthCheckMode: routing.HealthCheckModeAlwaysReachable,
			NativeResponses: true, NativeMessages: false,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateApplication(ctx, app); err != nil {
			t.Fatalf("create application: %v", err)
		}

		got, err := s.ApplicationByID(ctx, "app1")
		if err != nil {
			t.Fatalf("application by id: %v", err)
		}
		if !got.NativeResponses || got.NativeMessages {
			t.Fatalf("after create: NativeResponses=%v NativeMessages=%v, want true/false", got.NativeResponses, got.NativeMessages)
		}

		// Flip both and update.
		got.NativeResponses = false
		got.NativeMessages = true
		got.UpdatedAt = now.Add(time.Minute)
		if err := s.UpdateApplication(ctx, got); err != nil {
			t.Fatalf("update application: %v", err)
		}
		got, err = s.ApplicationByID(ctx, "app1")
		if err != nil {
			t.Fatalf("application by id (2): %v", err)
		}
		if got.NativeResponses || !got.NativeMessages {
			t.Fatalf("after update: NativeResponses=%v NativeMessages=%v, want false/true", got.NativeResponses, got.NativeMessages)
		}

		// The routing join must carry the flags too.
		if err := s.CreateMapping(ctx, routing.ModelMapping{
			ID: "map1", ApplicationID: "app1", GatewayModelName: "gpt", AppModelName: "m",
			Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create mapping: %v", err)
		}
		candidates, err := s.ActiveMappingsForModel(ctx, "gpt", routing.APIFlavorOpenAI)
		if err != nil || len(candidates) != 1 {
			t.Fatalf("active mappings: err=%v n=%d", err, len(candidates))
		}
		if candidates[0].Application.NativeResponses || !candidates[0].Application.NativeMessages {
			t.Fatalf("join flags: NativeResponses=%v NativeMessages=%v, want false/true", candidates[0].Application.NativeResponses, candidates[0].Application.NativeMessages)
		}
	})
}

// TestConformanceApplicationBenchmarkModes verifies the per-application P5
// benchmark-mode columns (benchmark_schedule_enabled +
// benchmark_schedule_interval_seconds + opportunistic_metrics_enabled) round-trip
// through create, update, direct read, and the routing join on both dialects.
func TestConformanceApplicationBenchmarkModes(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		if err := s.CreateAIServer(ctx, routing.AIServer{
			ID: "srv1", Name: "Server 1", Domain: "srv1.local", Provider: routing.ProviderVLLM,
			Endpoint: "http://srv1.local:8000", Status: routing.ServerStatusActive,
			HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create server: %v", err)
		}
		// Default (unset) round-trips as the zero "feature off" values.
		def := routing.Application{
			ID: "app_def", ServerID: "srv1", Type: "vllm", Port: 8001, Scheme: "http",
			APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 1, Weight: 1,
			TimeoutMS: 30000, AffinityTTLSeconds: 300, Status: routing.ServerStatusActive,
			HealthCheckMode: routing.HealthCheckModeAlwaysReachable,
			CreatedAt:       now, UpdatedAt: now,
		}
		if err := s.CreateApplication(ctx, def); err != nil {
			t.Fatalf("create default application: %v", err)
		}
		gotDef, err := s.ApplicationByID(ctx, "app_def")
		if err != nil {
			t.Fatalf("application by id (default): %v", err)
		}
		if gotDef.BenchmarkScheduleEnabled || gotDef.BenchmarkScheduleIntervalSeconds != 0 || gotDef.OpportunisticMetricsEnabled {
			t.Fatalf("default benchmark modes = %v/%d/%v, want false/0/false",
				gotDef.BenchmarkScheduleEnabled, gotDef.BenchmarkScheduleIntervalSeconds, gotDef.OpportunisticMetricsEnabled)
		}

		app := routing.Application{
			ID: "app1", ServerID: "srv1", Type: "vllm", Port: 8000, Scheme: "http",
			APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 1, Weight: 1,
			TimeoutMS: 30000, AffinityTTLSeconds: 300, Status: routing.ServerStatusActive,
			HealthCheckMode:          routing.HealthCheckModeAlwaysReachable,
			BenchmarkScheduleEnabled: true, BenchmarkScheduleIntervalSeconds: 3600, OpportunisticMetricsEnabled: true,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateApplication(ctx, app); err != nil {
			t.Fatalf("create application: %v", err)
		}
		got, err := s.ApplicationByID(ctx, "app1")
		if err != nil {
			t.Fatalf("application by id: %v", err)
		}
		if !got.BenchmarkScheduleEnabled || got.BenchmarkScheduleIntervalSeconds != 3600 || !got.OpportunisticMetricsEnabled {
			t.Fatalf("after create: %v/%d/%v, want true/3600/true",
				got.BenchmarkScheduleEnabled, got.BenchmarkScheduleIntervalSeconds, got.OpportunisticMetricsEnabled)
		}

		// Flip all three and update.
		got.BenchmarkScheduleEnabled = false
		got.BenchmarkScheduleIntervalSeconds = 900
		got.OpportunisticMetricsEnabled = false
		got.UpdatedAt = now.Add(time.Minute)
		if err := s.UpdateApplication(ctx, got); err != nil {
			t.Fatalf("update application: %v", err)
		}
		got, err = s.ApplicationByID(ctx, "app1")
		if err != nil {
			t.Fatalf("application by id (2): %v", err)
		}
		if got.BenchmarkScheduleEnabled || got.BenchmarkScheduleIntervalSeconds != 900 || got.OpportunisticMetricsEnabled {
			t.Fatalf("after update: %v/%d/%v, want false/900/false",
				got.BenchmarkScheduleEnabled, got.BenchmarkScheduleIntervalSeconds, got.OpportunisticMetricsEnabled)
		}

		// The routing join must carry the columns too.
		if err := s.CreateMapping(ctx, routing.ModelMapping{
			ID: "map1", ApplicationID: "app1", GatewayModelName: "gpt", AppModelName: "m",
			Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create mapping: %v", err)
		}
		candidates, err := s.ActiveMappingsForModel(ctx, "gpt", routing.APIFlavorOpenAI)
		if err != nil || len(candidates) != 1 {
			t.Fatalf("active mappings: err=%v n=%d", err, len(candidates))
		}
		ca := candidates[0].Application
		if ca.BenchmarkScheduleEnabled || ca.BenchmarkScheduleIntervalSeconds != 900 || ca.OpportunisticMetricsEnabled {
			t.Fatalf("join benchmark modes: %v/%d/%v, want false/900/false",
				ca.BenchmarkScheduleEnabled, ca.BenchmarkScheduleIntervalSeconds, ca.OpportunisticMetricsEnabled)
		}
	})
}

// TestConformanceApplicationAdmissionQueueTimeout verifies the per-application CP4
// admission_queue_timeout_seconds column round-trips through create, direct read,
// and the routing join on both dialects, and that an unset field reads back 0.
func TestConformanceApplicationAdmissionQueueTimeout(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		if err := s.CreateAIServer(ctx, routing.AIServer{
			ID: "srv1", Name: "Server 1", Domain: "srv1.local", Provider: routing.ProviderVLLM,
			Endpoint: "http://srv1.local:8000", Status: routing.ServerStatusActive,
			HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create server: %v", err)
		}
		// Default (unset) round-trips as 0 = "wait until the client aborts".
		def := routing.Application{
			ID: "app_def", ServerID: "srv1", Type: "vllm", Port: 8001, Scheme: "http",
			APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 1, Weight: 1,
			TimeoutMS: 30000, AffinityTTLSeconds: 300, Status: routing.ServerStatusActive,
			HealthCheckMode: routing.HealthCheckModeAlwaysReachable,
			CreatedAt:       now, UpdatedAt: now,
		}
		if err := s.CreateApplication(ctx, def); err != nil {
			t.Fatalf("create default application: %v", err)
		}
		gotDef, err := s.ApplicationByID(ctx, "app_def")
		if err != nil {
			t.Fatalf("application by id (default): %v", err)
		}
		if gotDef.AdmissionQueueTimeoutSeconds != 0 {
			t.Fatalf("default admission_queue_timeout_seconds = %d, want 0", gotDef.AdmissionQueueTimeoutSeconds)
		}

		app := routing.Application{
			ID: "app1", ServerID: "srv1", Type: "vllm", Port: 8000, Scheme: "http",
			APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 1, Weight: 1,
			TimeoutMS: 30000, AffinityTTLSeconds: 300, AdmissionQueueTimeoutSeconds: 45,
			Status:          routing.ServerStatusActive,
			HealthCheckMode: routing.HealthCheckModeAlwaysReachable,
			CreatedAt:       now, UpdatedAt: now,
		}
		if err := s.CreateApplication(ctx, app); err != nil {
			t.Fatalf("create application: %v", err)
		}
		got, err := s.ApplicationByID(ctx, "app1")
		if err != nil {
			t.Fatalf("application by id: %v", err)
		}
		if got.AdmissionQueueTimeoutSeconds != 45 {
			t.Fatalf("after create: admission_queue_timeout_seconds = %d, want 45", got.AdmissionQueueTimeoutSeconds)
		}

		// Update to a new value and re-read.
		got.AdmissionQueueTimeoutSeconds = 120
		got.UpdatedAt = now.Add(time.Minute)
		if err := s.UpdateApplication(ctx, got); err != nil {
			t.Fatalf("update application: %v", err)
		}
		got, err = s.ApplicationByID(ctx, "app1")
		if err != nil {
			t.Fatalf("application by id (2): %v", err)
		}
		if got.AdmissionQueueTimeoutSeconds != 120 {
			t.Fatalf("after update: admission_queue_timeout_seconds = %d, want 120", got.AdmissionQueueTimeoutSeconds)
		}

		// The routing join must carry the column too.
		if err := s.CreateMapping(ctx, routing.ModelMapping{
			ID: "map1", ApplicationID: "app1", GatewayModelName: "gpt", AppModelName: "m",
			Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create mapping: %v", err)
		}
		candidates, err := s.ActiveMappingsForModel(ctx, "gpt", routing.APIFlavorOpenAI)
		if err != nil || len(candidates) != 1 {
			t.Fatalf("active mappings: err=%v n=%d", err, len(candidates))
		}
		if ca := candidates[0].Application; ca.AdmissionQueueTimeoutSeconds != 120 {
			t.Fatalf("join admission_queue_timeout_seconds = %d, want 120", ca.AdmissionQueueTimeoutSeconds)
		}
	})
}

// TestConformanceServerAppPathAndUpstreamToken exercises the migration-v17 columns:
// ai_servers.server_path_suffix and applications.app_path_suffix/api_token/
// api_token_header. It asserts they survive Create -> Get (server + application),
// survive an Update, and — critically — arrive on the ActiveMappingsForModel join
// candidate (the thread the resolver builds a Target from). The store treats
// api_token as an opaque sealed string (it never decrypts).
func TestConformanceServerAppPathAndUpstreamToken(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		srv := routing.AIServer{
			ID: "srv1", Name: "Server 1", Domain: "srv1.local", ServerPathSuffix: "gateway",
			Provider: routing.ProviderVLLM, Endpoint: "http://srv1.local:8000",
			Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateAIServer(ctx, srv); err != nil {
			t.Fatalf("create server: %v", err)
		}
		gotSrv, err := s.AIServerByID(ctx, "srv1")
		if err != nil {
			t.Fatalf("server by id: %v", err)
		}
		if gotSrv.ServerPathSuffix != "gateway" {
			t.Fatalf("after create: server_path_suffix = %q, want %q", gotSrv.ServerPathSuffix, "gateway")
		}
		// Update the server suffix and re-read.
		gotSrv.ServerPathSuffix = "gw2"
		gotSrv.UpdatedAt = now.Add(time.Minute)
		if err := s.UpdateAIServer(ctx, gotSrv); err != nil {
			t.Fatalf("update server: %v", err)
		}
		gotSrv, err = s.AIServerByID(ctx, "srv1")
		if err != nil {
			t.Fatalf("server by id (2): %v", err)
		}
		if gotSrv.ServerPathSuffix != "gw2" {
			t.Fatalf("after update: server_path_suffix = %q, want %q", gotSrv.ServerPathSuffix, "gw2")
		}

		app := routing.Application{
			ID: "app1", ServerID: "srv1", Type: "vllm", Port: 8000, Scheme: "http",
			APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 1, Weight: 1,
			TimeoutMS: 30000, AffinityTTLSeconds: 300, Status: routing.ServerStatusActive,
			HealthCheckMode: routing.HealthCheckModeAlwaysReachable,
			AppPathSuffix:   "v1beta", APIToken: "enc:c2VhbGVk", APITokenHeader: "x-api-key",
			CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateApplication(ctx, app); err != nil {
			t.Fatalf("create application: %v", err)
		}
		gotApp, err := s.ApplicationByID(ctx, "app1")
		if err != nil {
			t.Fatalf("application by id: %v", err)
		}
		if gotApp.AppPathSuffix != "v1beta" || gotApp.APIToken != "enc:c2VhbGVk" || gotApp.APITokenHeader != "x-api-key" {
			t.Fatalf("after create: app suffix/token/header = %q/%q/%q", gotApp.AppPathSuffix, gotApp.APIToken, gotApp.APITokenHeader)
		}
		// Update the app fields (incl. clearing the token) and re-read.
		gotApp.AppPathSuffix = "v2"
		gotApp.APIToken = ""
		gotApp.APITokenHeader = ""
		gotApp.UpdatedAt = now.Add(time.Minute)
		if err := s.UpdateApplication(ctx, gotApp); err != nil {
			t.Fatalf("update application: %v", err)
		}
		gotApp, err = s.ApplicationByID(ctx, "app1")
		if err != nil {
			t.Fatalf("application by id (2): %v", err)
		}
		if gotApp.AppPathSuffix != "v2" || gotApp.APIToken != "" || gotApp.APITokenHeader != "" {
			t.Fatalf("after update: app suffix/token/header = %q/%q/%q", gotApp.AppPathSuffix, gotApp.APIToken, gotApp.APITokenHeader)
		}
		// Set the token again so the join assertion below has a non-empty value.
		gotApp.APIToken = "plain:sk-123"
		gotApp.APITokenHeader = "authorization"
		gotApp.AppPathSuffix = "final"
		if err := s.UpdateApplication(ctx, gotApp); err != nil {
			t.Fatalf("update application (3): %v", err)
		}

		// The routing join (ActiveMappingsForModel) must carry all four columns —
		// the resolver builds Target from this candidate.
		if err := s.CreateMapping(ctx, routing.ModelMapping{
			ID: "map1", ApplicationID: "app1", GatewayModelName: "gpt", AppModelName: "m",
			Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create mapping: %v", err)
		}
		cands, err := s.ActiveMappingsForModel(ctx, "gpt", routing.APIFlavorOpenAI)
		if err != nil || len(cands) != 1 {
			t.Fatalf("active mappings: err=%v n=%d", err, len(cands))
		}
		c := cands[0]
		if c.Server.ServerPathSuffix != "gw2" {
			t.Fatalf("join server_path_suffix = %q, want %q", c.Server.ServerPathSuffix, "gw2")
		}
		if c.Application.AppPathSuffix != "final" || c.Application.APIToken != "plain:sk-123" || c.Application.APITokenHeader != "authorization" {
			t.Fatalf("join app suffix/token/header = %q/%q/%q", c.Application.AppPathSuffix, c.Application.APIToken, c.Application.APITokenHeader)
		}
		// The composed endpoint reflects both suffixes.
		if got := routing.ApplicationEndpoint(c.Server, c.Application); got != "http://srv1.local:8000/gw2/final" {
			t.Fatalf("ApplicationEndpoint = %q, want %q", got, "http://srv1.local:8000/gw2/final")
		}
	})
}

// TestConformanceServerNetbirdColumns exercises the migration v18 columns: all 5
// round-trip through create -> AIServerByID/AIServers/ServersByOwner, and the two
// narrow writes touch ONLY their intended columns (UpdateServerNetbirdKey ->
// setup_key_id + group_id; UpdateServerNetbirdState -> domain + peer_id +
// connected), leaving the others intact. Both dialects.
func TestConformanceServerNetbirdColumns(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		srv := routing.AIServer{
			ID: "srvnb", Name: "NB Server", Domain: "old.local",
			Provider: routing.ProviderVLLM, Endpoint: "http://old.local:8000",
			Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
			NetbirdEnabled: true, NetbirdSetupKeyID: "sk-1", NetbirdGroupID: "grp-1",
			NetbirdPeerID: "peer-1", NetbirdConnected: true,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateUser(ctx, newTestUser("usr_owner", "owner@example.test", now)); err != nil {
			t.Fatalf("create user: %v", err)
		}
		if err := s.CreateAIServer(ctx, srv); err != nil {
			t.Fatalf("create server: %v", err)
		}
		if err := s.SetServerOwners(ctx, "srvnb", []string{"usr_owner"}); err != nil {
			t.Fatalf("set owners: %v", err)
		}

		assertNetbird := func(label string, got routing.AIServer, enabled bool, keyID, groupID, peerID string, connected bool) {
			t.Helper()
			if got.NetbirdEnabled != enabled || got.NetbirdSetupKeyID != keyID || got.NetbirdGroupID != groupID ||
				got.NetbirdPeerID != peerID || got.NetbirdConnected != connected {
				t.Fatalf("%s: netbird = {enabled:%v key:%q grp:%q peer:%q conn:%v}, want {%v %q %q %q %v}",
					label, got.NetbirdEnabled, got.NetbirdSetupKeyID, got.NetbirdGroupID, got.NetbirdPeerID, got.NetbirdConnected,
					enabled, keyID, groupID, peerID, connected)
			}
		}

		// All 5 round-trip through every read path (both scan sites).
		got, err := s.AIServerByID(ctx, "srvnb")
		if err != nil {
			t.Fatalf("by id: %v", err)
		}
		assertNetbird("AIServerByID", got, true, "sk-1", "grp-1", "peer-1", true)

		all, err := s.AIServers(ctx)
		if err != nil || len(all) != 1 {
			t.Fatalf("AIServers: err=%v n=%d", err, len(all))
		}
		assertNetbird("AIServers", all[0], true, "sk-1", "grp-1", "peer-1", true)

		owned, err := s.ServersByOwner(ctx, "usr_owner")
		if err != nil || len(owned) != 1 {
			t.Fatalf("ServersByOwner: err=%v n=%d", err, len(owned))
		}
		assertNetbird("ServersByOwner", owned[0], true, "sk-1", "grp-1", "peer-1", true)

		// UpdateServerNetbirdKey touches ONLY enabled + setup_key_id + group_id.
		if err := s.UpdateServerNetbirdKey(ctx, "srvnb", true, "sk-2", "grp-2"); err != nil {
			t.Fatalf("update netbird key: %v", err)
		}
		got, err = s.AIServerByID(ctx, "srvnb")
		if err != nil {
			t.Fatalf("by id after key: %v", err)
		}
		assertNetbird("after UpdateServerNetbirdKey", got, true, "sk-2", "grp-2", "peer-1", true)
		if got.Domain != "old.local" {
			t.Fatalf("after key: domain = %q, want unchanged old.local", got.Domain)
		}

		// UpdateServerNetbirdState touches ONLY domain + peer_id + connected.
		if err := s.UpdateServerNetbirdState(ctx, "srvnb", "peer.netbird.cloud", "peer-2", false); err != nil {
			t.Fatalf("update netbird state: %v", err)
		}
		got, err = s.AIServerByID(ctx, "srvnb")
		if err != nil {
			t.Fatalf("by id after state: %v", err)
		}
		assertNetbird("after UpdateServerNetbirdState", got, true, "sk-2", "grp-2", "peer-2", false)
		if got.Domain != "peer.netbird.cloud" {
			t.Fatalf("after state: domain = %q, want peer.netbird.cloud", got.Domain)
		}

		// Unknown id is ErrNotFound for both narrow writes.
		if err := s.UpdateServerNetbirdKey(ctx, "nope", true, "x", "y"); err != ErrNotFound {
			t.Fatalf("UpdateServerNetbirdKey(unknown) = %v, want ErrNotFound", err)
		}
		if err := s.UpdateServerNetbirdState(ctx, "nope", "d", "p", true); err != ErrNotFound {
			t.Fatalf("UpdateServerNetbirdState(unknown) = %v, want ErrNotFound", err)
		}
	})
}

// TestConformanceServerNetbirdLink exercises the Task 2 store writes: the
// extended UpdateServerNetbirdKey FLIPS netbird_enabled on (enroll) while
// touching only enabled/setup_key_id/group_id, and UpdateServerNetbirdLink sets
// enabled + peer_id and RESETS netbird_connected — leaving domain/key/group
// intact. Both dialects.
func TestConformanceServerNetbirdLink(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		// A plain (non-NetBird) server with a good domain, no peer/connected.
		srv := routing.AIServer{
			ID: "srvlink", Name: "Link Server", Domain: "keep.local",
			Provider: routing.ProviderVLLM, Endpoint: "http://keep.local:8000",
			Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
			NetbirdEnabled: false,
			CreatedAt:      now, UpdatedAt: now,
		}
		if err := s.CreateAIServer(ctx, srv); err != nil {
			t.Fatalf("create server: %v", err)
		}

		// Enroll: UpdateServerNetbirdKey(enabled=true) flips the flag + sets key/group,
		// touching NOTHING else (domain/peer/connected unchanged).
		if err := s.UpdateServerNetbirdKey(ctx, "srvlink", true, "sk-9", "grp-9"); err != nil {
			t.Fatalf("update netbird key (enroll): %v", err)
		}
		got, err := s.AIServerByID(ctx, "srvlink")
		if err != nil {
			t.Fatalf("by id after enroll: %v", err)
		}
		if !got.NetbirdEnabled || got.NetbirdSetupKeyID != "sk-9" || got.NetbirdGroupID != "grp-9" {
			t.Fatalf("after enroll = {enabled:%v key:%q grp:%q}, want {true sk-9 grp-9}", got.NetbirdEnabled, got.NetbirdSetupKeyID, got.NetbirdGroupID)
		}
		if got.Domain != "keep.local" || got.NetbirdPeerID != "" || got.NetbirdConnected {
			t.Fatalf("after enroll leaked other cols: domain=%q peer=%q conn=%v", got.Domain, got.NetbirdPeerID, got.NetbirdConnected)
		}

		// Simulate a synced/connected state so the link write can reset it.
		if err := s.UpdateServerNetbirdState(ctx, "srvlink", "peer.netbird.cloud", "peer-old", true); err != nil {
			t.Fatalf("update netbird state: %v", err)
		}

		// UpdateServerNetbirdLink sets enabled + peer_id and RESETS connected to
		// false, leaving domain/key/group intact.
		if err := s.UpdateServerNetbirdLink(ctx, "srvlink", true, "peer-new"); err != nil {
			t.Fatalf("update netbird link: %v", err)
		}
		got, err = s.AIServerByID(ctx, "srvlink")
		if err != nil {
			t.Fatalf("by id after link: %v", err)
		}
		if !got.NetbirdEnabled || got.NetbirdPeerID != "peer-new" || got.NetbirdConnected {
			t.Fatalf("after link = {enabled:%v peer:%q conn:%v}, want {true peer-new false}", got.NetbirdEnabled, got.NetbirdPeerID, got.NetbirdConnected)
		}
		if got.Domain != "peer.netbird.cloud" || got.NetbirdSetupKeyID != "sk-9" || got.NetbirdGroupID != "grp-9" {
			t.Fatalf("after link leaked other cols: domain=%q key=%q grp=%q", got.Domain, got.NetbirdSetupKeyID, got.NetbirdGroupID)
		}

		// Disable via the link editor: enabled=false, peer cleared, connected stays reset.
		if err := s.UpdateServerNetbirdLink(ctx, "srvlink", false, ""); err != nil {
			t.Fatalf("update netbird link (disable): %v", err)
		}
		got, err = s.AIServerByID(ctx, "srvlink")
		if err != nil {
			t.Fatalf("by id after disable: %v", err)
		}
		if got.NetbirdEnabled || got.NetbirdPeerID != "" || got.NetbirdConnected {
			t.Fatalf("after disable = {enabled:%v peer:%q conn:%v}, want {false \"\" false}", got.NetbirdEnabled, got.NetbirdPeerID, got.NetbirdConnected)
		}

		// Unknown id is ErrNotFound.
		if err := s.UpdateServerNetbirdLink(ctx, "nope", true, "p"); err != ErrNotFound {
			t.Fatalf("UpdateServerNetbirdLink(unknown) = %v, want ErrNotFound", err)
		}
	})
}

// TestConformanceServerNetbirdGroups exercises the migration-v19 netbird_group_ids
// column: it round-trips through create -> every read path (AIServerByID /
// AIServers / ServersByOwner, both scan sites), and UpdateServerNetbirdGroups
// writes ONLY that one column (leaving domain + the other netbird columns intact)
// and clears on "". Both dialects.
func TestConformanceServerNetbirdGroups(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		const initial = `[{"id":"gA","name":"A"},{"id":"gB","name":"B"}]`
		srv := routing.AIServer{
			ID: "srvgrp", Name: "Grp Server", Domain: "keep.local",
			Provider: routing.ProviderVLLM, Endpoint: "http://keep.local:8000",
			Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
			NetbirdEnabled: true, NetbirdSetupKeyID: "sk-1", NetbirdGroupID: "trk-1",
			NetbirdPeerID: "peer-1", NetbirdConnected: true, NetbirdGroupIDs: initial,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateUser(ctx, newTestUser("usr_grp", "grp@example.test", now)); err != nil {
			t.Fatalf("create user: %v", err)
		}
		if err := s.CreateAIServer(ctx, srv); err != nil {
			t.Fatalf("create server: %v", err)
		}
		if err := s.SetServerOwners(ctx, "srvgrp", []string{"usr_grp"}); err != nil {
			t.Fatalf("set owners: %v", err)
		}

		// Round-trips through all three read paths (both scan sites).
		got, err := s.AIServerByID(ctx, "srvgrp")
		if err != nil {
			t.Fatalf("by id: %v", err)
		}
		if got.NetbirdGroupIDs != initial {
			t.Fatalf("AIServerByID group ids = %q, want %q", got.NetbirdGroupIDs, initial)
		}
		all, err := s.AIServers(ctx)
		if err != nil || len(all) != 1 || all[0].NetbirdGroupIDs != initial {
			t.Fatalf("AIServers group ids: err=%v n=%d val=%q", err, len(all), func() string {
				if len(all) > 0 {
					return all[0].NetbirdGroupIDs
				}
				return "<none>"
			}())
		}
		owned, err := s.ServersByOwner(ctx, "usr_grp")
		if err != nil || len(owned) != 1 || owned[0].NetbirdGroupIDs != initial {
			t.Fatalf("ServersByOwner group ids: err=%v n=%d", err, len(owned))
		}

		// UpdateServerNetbirdGroups touches ONLY netbird_group_ids.
		const next = `[{"id":"gC","name":"C"}]`
		if err := s.UpdateServerNetbirdGroups(ctx, "srvgrp", next); err != nil {
			t.Fatalf("update netbird groups: %v", err)
		}
		got, err = s.AIServerByID(ctx, "srvgrp")
		if err != nil {
			t.Fatalf("by id after groups: %v", err)
		}
		if got.NetbirdGroupIDs != next {
			t.Fatalf("after update group ids = %q, want %q", got.NetbirdGroupIDs, next)
		}
		if got.Domain != "keep.local" || got.NetbirdSetupKeyID != "sk-1" || got.NetbirdGroupID != "trk-1" ||
			got.NetbirdPeerID != "peer-1" || !got.NetbirdConnected {
			t.Fatalf("update netbird groups leaked other cols: %+v", got)
		}

		// Clears on "".
		if err := s.UpdateServerNetbirdGroups(ctx, "srvgrp", ""); err != nil {
			t.Fatalf("clear netbird groups: %v", err)
		}
		got, err = s.AIServerByID(ctx, "srvgrp")
		if err != nil {
			t.Fatalf("by id after clear: %v", err)
		}
		if got.NetbirdGroupIDs != "" {
			t.Fatalf("after clear group ids = %q, want empty", got.NetbirdGroupIDs)
		}

		// Unknown id is ErrNotFound.
		if err := s.UpdateServerNetbirdGroups(ctx, "nope", "[]"); err != ErrNotFound {
			t.Fatalf("UpdateServerNetbirdGroups(unknown) = %v, want ErrNotFound", err)
		}
	})
}

// TestConformanceServerNetbirdPeerManaged exercises the migration-v20
// netbird_peer_managed provenance column: it round-trips through create
// (managed=true) -> every read path (AIServerByID / AIServers / ServersByOwner,
// both scan sites), the narrow UpdateServerNetbirdPeerManaged writer flips it to
// false touching ONLY that column, and a subsequent FULL UpdateAIServer preserves
// the flag (the full-row writer threads the column). It also sets a distinct
// netbird_group_ids value so a SELECT/scan column transposition is detectable.
// Both dialects.
func TestConformanceServerNetbirdPeerManaged(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		const groups = `[{"id":"gA","name":"A"}]`
		srv := routing.AIServer{
			ID: "srvpm", Name: "PM Server", Domain: "keep.local",
			Provider: routing.ProviderVLLM, Endpoint: "http://keep.local:8000",
			Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
			NetbirdEnabled: true, NetbirdSetupKeyID: "sk-1", NetbirdGroupID: "trk-1",
			NetbirdPeerID: "peer-1", NetbirdConnected: true, NetbirdGroupIDs: groups,
			NetbirdPeerManaged: true,
			CreatedAt:          now, UpdatedAt: now,
		}
		if err := s.CreateUser(ctx, newTestUser("usr_pm", "pm@example.test", now)); err != nil {
			t.Fatalf("create user: %v", err)
		}
		if err := s.CreateAIServer(ctx, srv); err != nil {
			t.Fatalf("create server: %v", err)
		}
		if err := s.SetServerOwners(ctx, "srvpm", []string{"usr_pm"}); err != nil {
			t.Fatalf("set owners: %v", err)
		}

		// Round-trips through all three read paths (both scan sites). The distinct
		// group value guards against a peer_managed/group_ids column transposition.
		got, err := s.AIServerByID(ctx, "srvpm")
		if err != nil {
			t.Fatalf("by id: %v", err)
		}
		if !got.NetbirdPeerManaged || got.NetbirdGroupIDs != groups {
			t.Fatalf("AIServerByID managed=%v groups=%q, want true %q", got.NetbirdPeerManaged, got.NetbirdGroupIDs, groups)
		}
		all, err := s.AIServers(ctx)
		if err != nil || len(all) != 1 || !all[0].NetbirdPeerManaged || all[0].NetbirdGroupIDs != groups {
			t.Fatalf("AIServers managed round-trip: err=%v n=%d", err, len(all))
		}
		owned, err := s.ServersByOwner(ctx, "usr_pm")
		if err != nil || len(owned) != 1 || !owned[0].NetbirdPeerManaged || owned[0].NetbirdGroupIDs != groups {
			t.Fatalf("ServersByOwner managed round-trip: err=%v n=%d", err, len(owned))
		}

		// UpdateServerNetbirdPeerManaged touches ONLY netbird_peer_managed.
		if err := s.UpdateServerNetbirdPeerManaged(ctx, "srvpm", false); err != nil {
			t.Fatalf("update netbird peer managed: %v", err)
		}
		got, err = s.AIServerByID(ctx, "srvpm")
		if err != nil {
			t.Fatalf("by id after managed write: %v", err)
		}
		if got.NetbirdPeerManaged {
			t.Fatalf("after update managed = true, want false")
		}
		if got.Domain != "keep.local" || got.NetbirdSetupKeyID != "sk-1" || got.NetbirdGroupID != "trk-1" ||
			got.NetbirdPeerID != "peer-1" || !got.NetbirdConnected || got.NetbirdGroupIDs != groups {
			t.Fatalf("update netbird peer managed leaked other cols: %+v", got)
		}

		// A subsequent full UpdateAIServer preserves the current (false) flag, then
		// flip it back to true via the full writer to prove the column is threaded.
		got.UpdatedAt = now.Add(time.Minute)
		if err := s.UpdateAIServer(ctx, got); err != nil {
			t.Fatalf("full update (preserve false): %v", err)
		}
		reread, err := s.AIServerByID(ctx, "srvpm")
		if err != nil {
			t.Fatalf("by id after full update: %v", err)
		}
		if reread.NetbirdPeerManaged {
			t.Fatalf("full update did not preserve managed=false")
		}
		reread.NetbirdPeerManaged = true
		if err := s.UpdateAIServer(ctx, reread); err != nil {
			t.Fatalf("full update (set true): %v", err)
		}
		got, err = s.AIServerByID(ctx, "srvpm")
		if err != nil {
			t.Fatalf("by id after full update (2): %v", err)
		}
		if !got.NetbirdPeerManaged {
			t.Fatalf("full update did not persist managed=true")
		}

		// Unknown id is ErrNotFound.
		if err := s.UpdateServerNetbirdPeerManaged(ctx, "nope", true); err != ErrNotFound {
			t.Fatalf("UpdateServerNetbirdPeerManaged(unknown) = %v, want ErrNotFound", err)
		}
	})
}

// TestConformanceServerNetbirdPolicyOverride exercises the migration-v21
// netbird_policy_override column: it round-trips through create -> every read
// path (AIServerByID / AIServers / ServersByOwner), the narrow
// UpdateServerNetbirdPolicyOverride writer touches ONLY that column (a distinct
// netbird_group_ids value guards against a column transposition), and an unknown
// id is ErrNotFound. Both dialects.
func TestConformanceServerNetbirdPolicyOverride(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		const groups = `[{"id":"g1","name":"n1"}]`
		srv := routing.AIServer{
			ID: "s-po", Name: "srv-po", Domain: "d",
			Provider: routing.ProviderVLLM, Endpoint: "http://e",
			Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
			NetbirdGroupIDs:       groups,
			NetbirdPolicyOverride: "exclude",
			CreatedAt:             now, UpdatedAt: now,
		}
		if err := s.CreateUser(ctx, newTestUser("usr_po", "po@example.test", now)); err != nil {
			t.Fatalf("create user: %v", err)
		}
		if err := s.CreateAIServer(ctx, srv); err != nil {
			t.Fatalf("CreateAIServer: %v", err)
		}
		if err := s.SetServerOwners(ctx, srv.ID, []string{"usr_po"}); err != nil {
			t.Fatalf("SetServerOwners: %v", err)
		}

		got, err := s.AIServerByID(ctx, srv.ID)
		if err != nil {
			t.Fatalf("AIServerByID: %v", err)
		}
		if got.NetbirdPolicyOverride != "exclude" {
			t.Fatalf("AIServerByID override = %q, want exclude", got.NetbirdPolicyOverride)
		}
		all, err := s.AIServers(ctx)
		if err != nil || len(all) != 1 || all[0].NetbirdPolicyOverride != "exclude" || all[0].NetbirdGroupIDs != groups {
			t.Fatalf("AIServers row wrong: err=%v %+v", err, all)
		}
		owned, err := s.ServersByOwner(ctx, "usr_po")
		if err != nil || len(owned) != 1 || owned[0].NetbirdPolicyOverride != "exclude" {
			t.Fatalf("ServersByOwner override wrong: err=%v %+v", err, owned)
		}

		// The narrow writer touches ONLY netbird_policy_override.
		if err := s.UpdateServerNetbirdPolicyOverride(ctx, srv.ID, "include"); err != nil {
			t.Fatalf("UpdateServerNetbirdPolicyOverride: %v", err)
		}
		got, err = s.AIServerByID(ctx, srv.ID)
		if err != nil {
			t.Fatalf("AIServerByID after write: %v", err)
		}
		if got.NetbirdPolicyOverride != "include" || got.NetbirdGroupIDs != groups {
			t.Fatalf("narrow write clobbered a sibling column: %+v", got)
		}

		// Unknown id is ErrNotFound.
		if err := s.UpdateServerNetbirdPolicyOverride(ctx, "nope", "include"); err != ErrNotFound {
			t.Fatalf("UpdateServerNetbirdPolicyOverride(unknown) = %v, want ErrNotFound", err)
		}
	})
}

// TestConformanceServerNetbirdAllowPing exercises the migration-v24
// netbird_allow_ping bool column: it round-trips through create (allow=true) ->
// every read path (AIServerByID / AIServers / ServersByOwner, both scan sites), the
// narrow UpdateServerNetbirdAllowPing writer flips it to false touching ONLY that
// column, and a subsequent FULL UpdateAIServer preserves + re-sets the flag (proving
// the full-row writer threads the column). A distinct netbird_policy_override value
// guards against a column transposition. Unknown id is ErrNotFound. Both dialects.
func TestConformanceServerNetbirdAllowPing(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		srv := routing.AIServer{
			ID: "s-ap", Name: "srv-ap", Domain: "d",
			Provider: routing.ProviderVLLM, Endpoint: "http://e",
			Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
			NetbirdPolicyOverride: "exclude",
			NetbirdAllowPing:      true,
			CreatedAt:             now, UpdatedAt: now,
		}
		if err := s.CreateUser(ctx, newTestUser("usr_ap", "ap@example.test", now)); err != nil {
			t.Fatalf("create user: %v", err)
		}
		if err := s.CreateAIServer(ctx, srv); err != nil {
			t.Fatalf("CreateAIServer: %v", err)
		}
		if err := s.SetServerOwners(ctx, srv.ID, []string{"usr_ap"}); err != nil {
			t.Fatalf("SetServerOwners: %v", err)
		}

		// Round-trips through all three read paths (both scan sites). The distinct
		// override value guards against an allow_ping/policy_override transposition.
		got, err := s.AIServerByID(ctx, srv.ID)
		if err != nil {
			t.Fatalf("AIServerByID: %v", err)
		}
		if !got.NetbirdAllowPing || got.NetbirdPolicyOverride != "exclude" {
			t.Fatalf("AIServerByID allow=%v override=%q, want true \"exclude\"", got.NetbirdAllowPing, got.NetbirdPolicyOverride)
		}
		all, err := s.AIServers(ctx)
		if err != nil || len(all) != 1 || !all[0].NetbirdAllowPing || all[0].NetbirdPolicyOverride != "exclude" {
			t.Fatalf("AIServers allow round-trip: err=%v %+v", err, all)
		}
		owned, err := s.ServersByOwner(ctx, "usr_ap")
		if err != nil || len(owned) != 1 || !owned[0].NetbirdAllowPing || owned[0].NetbirdPolicyOverride != "exclude" {
			t.Fatalf("ServersByOwner allow round-trip: err=%v %+v", err, owned)
		}

		// The narrow writer touches ONLY netbird_allow_ping.
		if err := s.UpdateServerNetbirdAllowPing(ctx, srv.ID, false); err != nil {
			t.Fatalf("UpdateServerNetbirdAllowPing: %v", err)
		}
		got, err = s.AIServerByID(ctx, srv.ID)
		if err != nil {
			t.Fatalf("AIServerByID after write: %v", err)
		}
		if got.NetbirdAllowPing || got.NetbirdPolicyOverride != "exclude" {
			t.Fatalf("narrow write clobbered a sibling column: %+v", got)
		}

		// The narrow writer must honor its argument in BOTH directions: write true
		// back and re-read true, so a mutant hardcoding false cannot survive.
		if err := s.UpdateServerNetbirdAllowPing(ctx, srv.ID, true); err != nil {
			t.Fatalf("UpdateServerNetbirdAllowPing(true): %v", err)
		}
		got, err = s.AIServerByID(ctx, srv.ID)
		if err != nil {
			t.Fatalf("AIServerByID after true write: %v", err)
		}
		if !got.NetbirdAllowPing || got.NetbirdPolicyOverride != "exclude" {
			t.Fatalf("narrow write(true) did not set allow_ping or clobbered a sibling: %+v", got)
		}
		// Reset to false for the full-row preservation check below.
		if err := s.UpdateServerNetbirdAllowPing(ctx, srv.ID, false); err != nil {
			t.Fatalf("UpdateServerNetbirdAllowPing(reset false): %v", err)
		}
		got, err = s.AIServerByID(ctx, srv.ID)
		if err != nil {
			t.Fatalf("AIServerByID after reset: %v", err)
		}

		// A full UpdateAIServer preserves the current (false) flag, then flip it back
		// to true via the full writer to prove the column is threaded end-to-end.
		got.UpdatedAt = now.Add(time.Minute)
		if err := s.UpdateAIServer(ctx, got); err != nil {
			t.Fatalf("full update (preserve false): %v", err)
		}
		reread, err := s.AIServerByID(ctx, srv.ID)
		if err != nil {
			t.Fatalf("AIServerByID after full update: %v", err)
		}
		if reread.NetbirdAllowPing {
			t.Fatalf("full update did not preserve allow_ping=false")
		}
		reread.NetbirdAllowPing = true
		if err := s.UpdateAIServer(ctx, reread); err != nil {
			t.Fatalf("full update (set true): %v", err)
		}
		got, err = s.AIServerByID(ctx, srv.ID)
		if err != nil {
			t.Fatalf("AIServerByID after full update (2): %v", err)
		}
		if !got.NetbirdAllowPing {
			t.Fatalf("full update did not persist allow_ping=true")
		}

		// Unknown id is ErrNotFound.
		if err := s.UpdateServerNetbirdAllowPing(ctx, "nope", true); err != ErrNotFound {
			t.Fatalf("UpdateServerNetbirdAllowPing(unknown) = %v, want ErrNotFound", err)
		}
	})
}

// TestConformanceServerEnergyConfigNarrowWriter exercises the narrow
// UpdateServerEnergyConfig writer. Four DISTINCT numeric values plus a distinct
// price_unit guard against an inter-column transposition; a distinct non-energy
// sibling (agent_presence_timeout_seconds) guards against an energy<->presence
// transposition; the narrow writer must touch ONLY the five energy columns and
// honor every argument. (The column-threading round-trip lives in
// TestConformanceServerEnergyConfig below.)
func TestConformanceServerEnergyConfigNarrowWriter(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		srv := routing.AIServer{
			ID: "s-en", Name: "srv-en", Domain: "d",
			Provider: routing.ProviderVLLM, Endpoint: "http://e",
			Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
			AgentPresenceTimeoutSeconds: 42,
			EstimatedWatts:              100,
			IdleWatts:                   25,
			PricePerKwh:                 0.3,
			Pue:                         1.5,
			PriceUnit:                   "usd",
			CreatedAt:                   now, UpdatedAt: now,
		}
		if err := s.CreateUser(ctx, newTestUser("usr_en", "en@example.test", now)); err != nil {
			t.Fatalf("create user: %v", err)
		}
		if err := s.CreateAIServer(ctx, srv); err != nil {
			t.Fatalf("CreateAIServer: %v", err)
		}
		if err := s.SetServerOwners(ctx, srv.ID, []string{"usr_en"}); err != nil {
			t.Fatalf("SetServerOwners: %v", err)
		}

		energyOK := func(g routing.AIServer, est, idle, price, pue float64, unit string) bool {
			return g.EstimatedWatts == est && g.IdleWatts == idle && g.PricePerKwh == price && g.Pue == pue &&
				g.PriceUnit == unit && g.AgentPresenceTimeoutSeconds == 42
		}

		// Round-trips through all three read paths (both scan sites).
		got, err := s.AIServerByID(ctx, srv.ID)
		if err != nil {
			t.Fatalf("AIServerByID: %v", err)
		}
		if !energyOK(got, 100, 25, 0.3, 1.5, "usd") {
			t.Fatalf("AIServerByID energy round-trip: %+v", got)
		}
		all, err := s.AIServers(ctx)
		if err != nil || len(all) != 1 || !energyOK(all[0], 100, 25, 0.3, 1.5, "usd") {
			t.Fatalf("AIServers energy round-trip: err=%v %+v", err, all)
		}
		owned, err := s.ServersByOwner(ctx, "usr_en")
		if err != nil || len(owned) != 1 || !energyOK(owned[0], 100, 25, 0.3, 1.5, "usd") {
			t.Fatalf("ServersByOwner energy round-trip: err=%v %+v", err, owned)
		}

		// The narrow writer sets all five to NEW distinct values and touches ONLY
		// the five energy columns (the sibling presence-timeout must survive).
		if err := s.UpdateServerEnergyConfig(ctx, srv.ID, 200, 60, 0.12, 1.25, "eur"); err != nil {
			t.Fatalf("UpdateServerEnergyConfig: %v", err)
		}
		got, err = s.AIServerByID(ctx, srv.ID)
		if err != nil {
			t.Fatalf("AIServerByID after write: %v", err)
		}
		if !energyOK(got, 200, 60, 0.12, 1.25, "eur") {
			t.Fatalf("narrow write did not set all five (or clobbered the sibling): %+v", got)
		}

		// A full UpdateAIServer preserves + re-threads the columns end-to-end.
		got.UpdatedAt = now.Add(time.Minute)
		got.EstimatedWatts = 333
		got.PriceUnit = "usd_cent"
		if err := s.UpdateAIServer(ctx, got); err != nil {
			t.Fatalf("full update: %v", err)
		}
		reread, err := s.AIServerByID(ctx, srv.ID)
		if err != nil {
			t.Fatalf("AIServerByID after full update: %v", err)
		}
		if !energyOK(reread, 333, 60, 0.12, 1.25, "usd_cent") {
			t.Fatalf("full update did not thread the energy columns: %+v", reread)
		}

		// Unknown id is ErrNotFound.
		if err := s.UpdateServerEnergyConfig(ctx, "nope", 1, 1, 1, 1, "eur"); err != ErrNotFound {
			t.Fatalf("UpdateServerEnergyConfig(unknown) = %v, want ErrNotFound", err)
		}
	})
}

// TestConformanceServerNetbirdPingExclude exercises the migration-v25
// netbird_ping_exclude bool column: it round-trips through create (exclude=true) ->
// every read path (AIServerByID / AIServers / ServersByOwner, both scan sites), the
// narrow UpdateServerNetbirdPingExclude writer flips it to false touching ONLY that
// column, and a subsequent FULL UpdateAIServer preserves + re-sets the flag (proving
// the full-row writer threads the column). The immediately-adjacent netbird_allow_ping
// (kept distinctly false) guards against a column transposition, and a second server
// created without the flag confirms it defaults false. Unknown id is ErrNotFound.
// Both dialects.
func TestConformanceServerNetbirdPingExclude(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		srv := routing.AIServer{
			ID: "s-pe", Name: "srv-pe", Domain: "d",
			Provider: routing.ProviderVLLM, Endpoint: "http://e",
			Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
			NetbirdAllowPing:   false,
			NetbirdPingExclude: true,
			CreatedAt:          now, UpdatedAt: now,
		}
		if err := s.CreateUser(ctx, newTestUser("usr_pe", "pe@example.test", now)); err != nil {
			t.Fatalf("create user: %v", err)
		}
		if err := s.CreateAIServer(ctx, srv); err != nil {
			t.Fatalf("CreateAIServer: %v", err)
		}
		if err := s.SetServerOwners(ctx, srv.ID, []string{"usr_pe"}); err != nil {
			t.Fatalf("SetServerOwners: %v", err)
		}

		// A second server created WITHOUT the flag must default to false.
		def := routing.AIServer{
			ID: "s-pe2", Name: "srv-pe2", Domain: "d",
			Provider: routing.ProviderVLLM, Endpoint: "http://e",
			Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateAIServer(ctx, def); err != nil {
			t.Fatalf("CreateAIServer(default): %v", err)
		}
		gotDef, err := s.AIServerByID(ctx, def.ID)
		if err != nil {
			t.Fatalf("AIServerByID(default): %v", err)
		}
		if gotDef.NetbirdPingExclude {
			t.Fatalf("ping_exclude defaulted to true, want false")
		}

		// Round-trips through all three read paths (both scan sites). The adjacent
		// allow_ping value guards against an allow_ping/ping_exclude transposition.
		got, err := s.AIServerByID(ctx, srv.ID)
		if err != nil {
			t.Fatalf("AIServerByID: %v", err)
		}
		if !got.NetbirdPingExclude || got.NetbirdAllowPing {
			t.Fatalf("AIServerByID exclude=%v allow=%v, want true false", got.NetbirdPingExclude, got.NetbirdAllowPing)
		}
		all, err := s.AIServers(ctx)
		if err != nil {
			t.Fatalf("AIServers: %v", err)
		}
		var listed *routing.AIServer
		for i := range all {
			if all[i].ID == srv.ID {
				listed = &all[i]
			}
		}
		if listed == nil || !listed.NetbirdPingExclude || listed.NetbirdAllowPing {
			t.Fatalf("AIServers exclude round-trip: %+v", listed)
		}
		owned, err := s.ServersByOwner(ctx, "usr_pe")
		if err != nil || len(owned) != 1 || !owned[0].NetbirdPingExclude || owned[0].NetbirdAllowPing {
			t.Fatalf("ServersByOwner exclude round-trip: err=%v %+v", err, owned)
		}

		// The narrow writer touches ONLY netbird_ping_exclude.
		if err := s.UpdateServerNetbirdPingExclude(ctx, srv.ID, false); err != nil {
			t.Fatalf("UpdateServerNetbirdPingExclude: %v", err)
		}
		got, err = s.AIServerByID(ctx, srv.ID)
		if err != nil {
			t.Fatalf("AIServerByID after write: %v", err)
		}
		if got.NetbirdPingExclude || got.NetbirdAllowPing {
			t.Fatalf("narrow write clobbered a sibling column: %+v", got)
		}

		// The narrow writer must honor its argument in BOTH directions: write true
		// back and re-read true, so a mutant hardcoding false cannot survive.
		if err := s.UpdateServerNetbirdPingExclude(ctx, srv.ID, true); err != nil {
			t.Fatalf("UpdateServerNetbirdPingExclude(true): %v", err)
		}
		got, err = s.AIServerByID(ctx, srv.ID)
		if err != nil {
			t.Fatalf("AIServerByID after true write: %v", err)
		}
		if !got.NetbirdPingExclude || got.NetbirdAllowPing {
			t.Fatalf("narrow write(true) did not set ping_exclude or clobbered a sibling: %+v", got)
		}
		// Reset to false for the full-row preservation check below.
		if err := s.UpdateServerNetbirdPingExclude(ctx, srv.ID, false); err != nil {
			t.Fatalf("UpdateServerNetbirdPingExclude(reset false): %v", err)
		}
		got, err = s.AIServerByID(ctx, srv.ID)
		if err != nil {
			t.Fatalf("AIServerByID after reset: %v", err)
		}

		// A full UpdateAIServer preserves the current (false) flag, then flip it back
		// to true via the full writer to prove the column is threaded end-to-end.
		got.UpdatedAt = now.Add(time.Minute)
		if err := s.UpdateAIServer(ctx, got); err != nil {
			t.Fatalf("full update (preserve false): %v", err)
		}
		reread, err := s.AIServerByID(ctx, srv.ID)
		if err != nil {
			t.Fatalf("AIServerByID after full update: %v", err)
		}
		if reread.NetbirdPingExclude {
			t.Fatalf("full update did not preserve ping_exclude=false")
		}
		reread.NetbirdPingExclude = true
		if err := s.UpdateAIServer(ctx, reread); err != nil {
			t.Fatalf("full update (set true): %v", err)
		}
		got, err = s.AIServerByID(ctx, srv.ID)
		if err != nil {
			t.Fatalf("AIServerByID after full update (2): %v", err)
		}
		if !got.NetbirdPingExclude {
			t.Fatalf("full update did not persist ping_exclude=true")
		}

		// Unknown id is ErrNotFound.
		if err := s.UpdateServerNetbirdPingExclude(ctx, "nope", true); err != ErrNotFound {
			t.Fatalf("UpdateServerNetbirdPingExclude(unknown) = %v, want ErrNotFound", err)
		}
	})
}

// TestConformanceServerAgentPresenceTimeoutSeconds exercises the migration-v31
// ai_servers.agent_presence_timeout_seconds int column (0 = follow the
// system-wide default): it round-trips a non-zero value through create ->
// every read path (AIServerByID / AIServers / ServersByOwner, both scan
// sites), a second server created without the field defaults to 0, and a
// full UpdateAIServer changes + persists the value end-to-end. Both dialects.
func TestConformanceServerAgentPresenceTimeoutSeconds(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		srv := routing.AIServer{
			ID: "s-apt", Name: "srv-apt", Domain: "d",
			Provider: routing.ProviderVLLM, Endpoint: "http://e",
			Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
			AgentPresenceTimeoutSeconds: 42,
			CreatedAt:                   now, UpdatedAt: now,
		}
		if err := s.CreateUser(ctx, newTestUser("usr_apt", "apt@example.test", now)); err != nil {
			t.Fatalf("create user: %v", err)
		}
		if err := s.CreateAIServer(ctx, srv); err != nil {
			t.Fatalf("CreateAIServer: %v", err)
		}
		if err := s.SetServerOwners(ctx, srv.ID, []string{"usr_apt"}); err != nil {
			t.Fatalf("SetServerOwners: %v", err)
		}

		// A second server created WITHOUT the field must default to 0.
		def := routing.AIServer{
			ID: "s-apt2", Name: "srv-apt2", Domain: "d",
			Provider: routing.ProviderVLLM, Endpoint: "http://e",
			Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateAIServer(ctx, def); err != nil {
			t.Fatalf("CreateAIServer(default): %v", err)
		}
		gotDef, err := s.AIServerByID(ctx, def.ID)
		if err != nil {
			t.Fatalf("AIServerByID(default): %v", err)
		}
		if gotDef.AgentPresenceTimeoutSeconds != 0 {
			t.Fatalf("agent_presence_timeout_seconds defaulted to %d, want 0", gotDef.AgentPresenceTimeoutSeconds)
		}

		// Round-trips through all three read paths (both scan sites).
		got, err := s.AIServerByID(ctx, srv.ID)
		if err != nil {
			t.Fatalf("AIServerByID: %v", err)
		}
		if got.AgentPresenceTimeoutSeconds != 42 {
			t.Fatalf("AIServerByID agent_presence_timeout_seconds = %d, want 42", got.AgentPresenceTimeoutSeconds)
		}
		all, err := s.AIServers(ctx)
		if err != nil {
			t.Fatalf("AIServers: %v", err)
		}
		var listed *routing.AIServer
		for i := range all {
			if all[i].ID == srv.ID {
				listed = &all[i]
			}
		}
		if listed == nil || listed.AgentPresenceTimeoutSeconds != 42 {
			t.Fatalf("AIServers agent_presence_timeout_seconds round-trip: %+v", listed)
		}
		owned, err := s.ServersByOwner(ctx, "usr_apt")
		if err != nil || len(owned) != 1 || owned[0].AgentPresenceTimeoutSeconds != 42 {
			t.Fatalf("ServersByOwner agent_presence_timeout_seconds round-trip: err=%v %+v", err, owned)
		}

		// A full UpdateAIServer changes + persists a new value end-to-end.
		got.AgentPresenceTimeoutSeconds = 99
		got.UpdatedAt = now.Add(time.Minute)
		if err := s.UpdateAIServer(ctx, got); err != nil {
			t.Fatalf("full update: %v", err)
		}
		reread, err := s.AIServerByID(ctx, srv.ID)
		if err != nil {
			t.Fatalf("AIServerByID after full update: %v", err)
		}
		if reread.AgentPresenceTimeoutSeconds != 99 {
			t.Fatalf("full update did not persist agent_presence_timeout_seconds=99, got %d", reread.AgentPresenceTimeoutSeconds)
		}

		// Back to 0 (follow-system) round-trips too.
		reread.AgentPresenceTimeoutSeconds = 0
		reread.UpdatedAt = now.Add(2 * time.Minute)
		if err := s.UpdateAIServer(ctx, reread); err != nil {
			t.Fatalf("full update (reset to 0): %v", err)
		}
		final, err := s.AIServerByID(ctx, srv.ID)
		if err != nil {
			t.Fatalf("AIServerByID after reset: %v", err)
		}
		if final.AgentPresenceTimeoutSeconds != 0 {
			t.Fatalf("full update did not persist agent_presence_timeout_seconds=0, got %d", final.AgentPresenceTimeoutSeconds)
		}
	})
}

// TestConformanceServerEnergyConfig proves the four additive per-server energy
// columns (migration v35: estimated_watts, idle_watts, price_per_kwh, pue —
// all real, default 0 = "unset / use default") round-trip through create ->
// every read path (AIServerByID / AIServers / ServersByOwner, both scan
// sites), a second server created without the fields defaults to all-0, and a
// full UpdateAIServer changes + persists the values end-to-end. Purely
// additive: no engine consumes these yet, and routing never reads them (they
// are absent from the ActiveMappingsForModel candidate join). Both dialects.
func TestConformanceServerEnergyConfig(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		srv := routing.AIServer{
			ID: "s-nrg", Name: "srv-nrg", Domain: "d",
			Provider: routing.ProviderVLLM, Endpoint: "http://e",
			Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
			EstimatedWatts: 350.5, IdleWatts: 40.25, PricePerKwh: 0.32, Pue: 1.4,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateUser(ctx, newTestUser("usr_nrg", "nrg@example.test", now)); err != nil {
			t.Fatalf("create user: %v", err)
		}
		if err := s.CreateAIServer(ctx, srv); err != nil {
			t.Fatalf("CreateAIServer: %v", err)
		}
		if err := s.SetServerOwners(ctx, srv.ID, []string{"usr_nrg"}); err != nil {
			t.Fatalf("SetServerOwners: %v", err)
		}

		// A second server created WITHOUT the fields must default to all-0.
		def := routing.AIServer{
			ID: "s-nrg2", Name: "srv-nrg2", Domain: "d",
			Provider: routing.ProviderVLLM, Endpoint: "http://e",
			Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateAIServer(ctx, def); err != nil {
			t.Fatalf("CreateAIServer(default): %v", err)
		}
		gotDef, err := s.AIServerByID(ctx, def.ID)
		if err != nil {
			t.Fatalf("AIServerByID(default): %v", err)
		}
		if gotDef.EstimatedWatts != 0 || gotDef.IdleWatts != 0 || gotDef.PricePerKwh != 0 || gotDef.Pue != 0 {
			t.Fatalf("energy config defaulted to %+v, want all-0", gotDef)
		}

		// Round-trips through all three read paths (both scan sites).
		got, err := s.AIServerByID(ctx, srv.ID)
		if err != nil {
			t.Fatalf("AIServerByID: %v", err)
		}
		if got.EstimatedWatts != 350.5 || got.IdleWatts != 40.25 || got.PricePerKwh != 0.32 || got.Pue != 1.4 {
			t.Fatalf("AIServerByID energy config = %+v, want 350.5/40.25/0.32/1.4", got)
		}
		all, err := s.AIServers(ctx)
		if err != nil {
			t.Fatalf("AIServers: %v", err)
		}
		var listed *routing.AIServer
		for i := range all {
			if all[i].ID == srv.ID {
				listed = &all[i]
			}
		}
		if listed == nil || listed.EstimatedWatts != 350.5 || listed.IdleWatts != 40.25 || listed.PricePerKwh != 0.32 || listed.Pue != 1.4 {
			t.Fatalf("AIServers energy config round-trip: %+v", listed)
		}
		owned, err := s.ServersByOwner(ctx, "usr_nrg")
		if err != nil || len(owned) != 1 || owned[0].EstimatedWatts != 350.5 || owned[0].IdleWatts != 40.25 ||
			owned[0].PricePerKwh != 0.32 || owned[0].Pue != 1.4 {
			t.Fatalf("ServersByOwner energy config round-trip: err=%v %+v", err, owned)
		}

		// A full UpdateAIServer changes + persists new values end-to-end.
		got.EstimatedWatts = 500
		got.IdleWatts = 60
		got.PricePerKwh = 0.45
		got.Pue = 1.6
		got.UpdatedAt = now.Add(time.Minute)
		if err := s.UpdateAIServer(ctx, got); err != nil {
			t.Fatalf("full update: %v", err)
		}
		reread, err := s.AIServerByID(ctx, srv.ID)
		if err != nil {
			t.Fatalf("AIServerByID after full update: %v", err)
		}
		if reread.EstimatedWatts != 500 || reread.IdleWatts != 60 || reread.PricePerKwh != 0.45 || reread.Pue != 1.6 {
			t.Fatalf("full update did not persist energy config, got %+v", reread)
		}

		// Back to all-0 (unset) round-trips too.
		reread.EstimatedWatts, reread.IdleWatts, reread.PricePerKwh, reread.Pue = 0, 0, 0, 0
		reread.UpdatedAt = now.Add(2 * time.Minute)
		if err := s.UpdateAIServer(ctx, reread); err != nil {
			t.Fatalf("full update (reset to 0): %v", err)
		}
		final, err := s.AIServerByID(ctx, srv.ID)
		if err != nil {
			t.Fatalf("AIServerByID after reset: %v", err)
		}
		if final.EstimatedWatts != 0 || final.IdleWatts != 0 || final.PricePerKwh != 0 || final.Pue != 0 {
			t.Fatalf("full update did not persist energy config reset to 0, got %+v", final)
		}
	})
}

// --- 5b. Routing: Telemetry Samples (rich per-server perf history) ----------

// Regression: UpsertTelemetry must accept byte counts > int4 max (~2.1e9). On a
// host with >2 GB RAM/VRAM the value overflowed the server_telemetry int4 columns
// on postgres ("greater than maximum value for int4"), 500ing every agent POST;
// sqlite (64-bit INTEGER) never hit it. Migration v4 + the bigint baseline fix it.
func TestConformanceTelemetryLargeByteCounts(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		srv := routing.AIServer{
			ID: "srvbig", Name: "Big", Domain: "big.local", Provider: routing.ProviderOllama,
			Endpoint: "http://big.local:11434", Status: routing.ServerStatusActive,
			HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateAIServer(ctx, srv); err != nil {
			t.Fatalf("create server: %v", err)
		}
		const bigRAM = int64(10068131840)  // ~10 GB, the exact value from the field report
		const bigVRAM = int64(51539607552) // ~48 GB
		tel := routing.ServerTelemetry{
			ServerID: "srvbig", ReportedAt: now, AgentVersion: "0.1.0", OS: "linux", Arch: "amd64",
			RAMUsedBytes: bigRAM, RAMTotalBytes: bigRAM + 1, VRAMUsedBytes: bigVRAM, VRAMTotalBytes: bigVRAM + 1,
			ProviderHealth: "{}", Capabilities: "{}", RawSummary: "{}", UpdatedAt: now,
		}
		if err := s.UpsertTelemetry(ctx, tel); err != nil {
			t.Fatalf("UpsertTelemetry with >int4 byte counts: %v", err)
		}
		got, ok, err := s.TelemetryByServer(ctx, "srvbig")
		if err != nil || !ok {
			t.Fatalf("TelemetryByServer ok=%v err=%v", ok, err)
		}
		if got.RAMUsedBytes != bigRAM || got.VRAMTotalBytes != bigVRAM+1 {
			t.Fatalf("byte counts did not round-trip: ram=%d vram_total=%d", got.RAMUsedBytes, got.VRAMTotalBytes)
		}
	})
}

// Regression for the exact field-upgrade path: an existing postgres DB whose
// server_telemetry byte columns are still int4 (deployed before this fix) must be
// widened to bigint by migration v4 so subsequent >2 GB telemetry upserts succeed.
// The fresh-migrate conformance path can't exercise this (its baseline is already
// bigint), so this test reverts the columns to int4 to simulate the old schema,
// confirms the overflow, runs migration4Up, and confirms the upsert then succeeds.
func TestMigration4UpgradesInt4ToBigint(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		if s.dl.name() != "postgres" {
			t.Skip("postgres-only: the int4 overflow + ALTER COLUMN TYPE bigint do not apply to sqlite")
		}
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		if err := s.CreateAIServer(ctx, routing.AIServer{
			ID: "srvup", Name: "Up", Domain: "up.local", Provider: routing.ProviderOllama,
			Endpoint: "http://up.local:11434", Status: routing.ServerStatusActive,
			HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create server: %v", err)
		}
		// Simulate a pre-v4 deployment: revert the byte columns to int4.
		if _, err := s.db.ExecContext(ctx, `alter table server_telemetry
			alter column ram_used_bytes type integer,
			alter column ram_total_bytes type integer,
			alter column vram_used_bytes type integer,
			alter column vram_total_bytes type integer`); err != nil {
			t.Fatalf("revert columns to int4: %v", err)
		}
		big := routing.ServerTelemetry{
			ServerID: "srvup", ReportedAt: now, ProviderHealth: "{}", Capabilities: "{}",
			RawSummary: "{}", UpdatedAt: now, RAMUsedBytes: 10068131840,
		}
		// With int4 columns the upsert must fail (reproduces the reported 500).
		if err := s.UpsertTelemetry(ctx, big); err == nil {
			t.Fatal("expected int4 overflow before migration v4, got nil")
		}
		// Apply migration v4's widening.
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if err := migration4Up(ctx, tx, s.dl); err != nil {
			_ = tx.Rollback()
			t.Fatalf("migration4Up: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		// The same upsert now succeeds.
		if err := s.UpsertTelemetry(ctx, big); err != nil {
			t.Fatalf("UpsertTelemetry after v4 widening: %v", err)
		}
	})
}

// TestMigration43WidensRealToDoublePrecision is the regression for the postgres
// float32/float64 precision-loss bug. The v1 baseline declared every float64-backed
// column (energy_wh, tokens_per_second, cost_budget_amount, ...) as `real` (float32,
// ~7 significant digits), so a SUM aggregation drifted — the /api/portal/usage/groups
// SUM(energy_wh) folded 0.1 + 1.0 into 1.1000000014901161 instead of 1.1. The
// corrected baseline uses `double precision` (proven here by the fresh-DB
// assertions), and migration v43 widens pre-existing postgres deployments (proven by
// reverting EVERY float column to `real`, confirming the loss, then migrating and
// confirming an exact round-trip). SQLite's REAL is already 8-byte and it has no
// ALTER COLUMN TYPE, so this path is postgres-only. Mirrors
// TestMigration4UpgradesInt4ToBigint.
func TestMigration43WidensRealToDoublePrecision(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		if s.dl.name() != "postgres" {
			t.Skip("postgres-only: sqlite REAL is already 8-byte and has no ALTER COLUMN TYPE")
		}
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		// 0.1 is not exactly representable in float32; a `real` column rounds it,
		// while a `double precision` column stores the float64 value exactly.
		const want = 0.1

		columnsOfType := func(dataType string) [][2]string {
			t.Helper()
			rows, err := s.db.QueryContext(ctx,
				`select table_name, column_name from information_schema.columns
				 where table_schema = 'public' and data_type = $1
				 order by table_name, column_name`, dataType)
			if err != nil {
				t.Fatalf("query %q columns: %v", dataType, err)
			}
			defer rows.Close()
			var out [][2]string
			for rows.Next() {
				var tbl, col string
				if err := rows.Scan(&tbl, &col); err != nil {
					t.Fatalf("scan: %v", err)
				}
				out = append(out, [2]string{tbl, col})
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("rows: %v", err)
			}
			return out
		}
		// The column's type changes under us (double precision -> real -> double
		// precision), which would invalidate a single reused prepared SELECT plan
		// with pgx ("cached plan must not change result type"). Inline the trusted
		// literal id so each phase gets a distinct, separately-planned query text —
		// production never mixes DDL and this DML on one cached statement.
		readEnergy := func(id string) float64 {
			t.Helper()
			var got float64
			if err := s.db.QueryRowContext(ctx, `select energy_wh from usage_events where id = '`+id+`'`).Scan(&got); err != nil {
				t.Fatalf("read energy_wh %s: %v", id, err)
			}
			return got
		}
		rec := func(id string, e float64) {
			t.Helper()
			s.Record(usage.Event{
				ID: id, UserID: "u1", TokenID: "t", SessionID: "sess", Model: "m",
				Host: "h", Status: "ok", HTTPStatus: 200, EnergyWh: e, CreatedAt: now,
			})
			if err := s.LastUsageError(); err != nil {
				t.Fatalf("record %s: %v", id, err)
			}
		}

		// --- Fresh (corrected baseline): no float32 columns, exact round-trip. ---
		if leftover := columnsOfType("real"); len(leftover) != 0 {
			t.Fatalf("corrected baseline still has real (float32) columns: %v", leftover)
		}
		rec("fresh", want)
		if got := readEnergy("fresh"); got != want {
			t.Fatalf("fresh double precision round-trip: got %.17g, want %.17g", got, want)
		}

		// --- Simulate a fully pre-v43 deployment: revert EVERY float column to real. ---
		floats := columnsOfType("double precision")
		if len(floats) == 0 {
			t.Fatal("expected double precision columns in the corrected baseline, found none")
		}
		for _, c := range floats {
			if _, err := s.db.ExecContext(ctx, "alter table "+c[0]+" alter column "+c[1]+" type real"); err != nil {
				t.Fatalf("revert %s.%s to real: %v", c[0], c[1], err)
			}
		}
		rec("lossy", want)
		if got := readEnergy("lossy"); got == want {
			t.Fatalf("expected float32 precision loss on a real column, unexpectedly got exact %.17g", got)
		}

		// --- Apply migration v43's widening. ---
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if err := migration43Up(ctx, tx, s.dl); err != nil {
			_ = tx.Rollback()
			t.Fatalf("migration43Up: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}

		// Completeness: v43 must widen EVERY float column, so no `real` column may
		// remain (guards against an incomplete column list in migration43Up).
		if leftover := columnsOfType("real"); len(leftover) != 0 {
			t.Fatalf("real (float32) columns still present after migration v43: %v", leftover)
		}
		// A fresh write now round-trips exactly on the widened column.
		rec("migrated", want)
		if got := readEnergy("migrated"); got != want {
			t.Fatalf("post-migration double precision round-trip: got %.17g, want %.17g", got, want)
		}
	})
}

func TestConformanceTelemetrySamples(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		srv := routing.AIServer{
			ID: "srv1", Name: "Server 1", Domain: "srv1.local", Provider: routing.ProviderOllama,
			Endpoint: "http://srv1.local:11434", Status: routing.ServerStatusActive,
			HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateAIServer(ctx, srv); err != nil {
			t.Fatalf("create server: %v", err)
		}

		base := now
		for i := 0; i < 6; i++ {
			sample := routing.TelemetrySample{
				ServerID:   "srv1",
				ReportedAt: base.Add(time.Duration(i) * time.Minute),
				CPUUtilPct: float64(10 + i),
				GPUs: []routing.GPUSample{
					{Index: 0, Name: "RTX 4090", UtilPct: 88, TempC: 71},
				},
				Net: []routing.NetSample{
					{Name: "eth0", RxBytes: 1000, TxBytes: 2000},
				},
			}
			if err := s.InsertTelemetrySample(ctx, sample); err != nil {
				t.Fatalf("insert sample %d: %v", i, err)
			}
		}

		// Full window: all 6, ascending reported_at, nested GPU/Net round-trip on [0].
		got, err := s.TelemetrySamples(ctx, "srv1", base, base.Add(10*time.Minute), 100)
		if err != nil {
			t.Fatalf("telemetry samples: %v", err)
		}
		if len(got) != 6 {
			t.Fatalf("expected 6 samples, got %d", len(got))
		}
		for i := 1; i < len(got); i++ {
			if got[i].ReportedAt.Before(got[i-1].ReportedAt) {
				t.Fatalf("samples not ascending at %d: %v before %v", i, got[i].ReportedAt, got[i-1].ReportedAt)
			}
		}
		if len(got[0].GPUs) != 1 || got[0].GPUs[0].Name != "RTX 4090" || got[0].GPUs[0].TempC != 71 {
			t.Fatalf("gpu round-trip mismatch: %+v", got[0].GPUs)
		}
		if len(got[0].Net) != 1 || got[0].Net[0].RxBytes != 1000 {
			t.Fatalf("net round-trip mismatch: %+v", got[0].Net)
		}

		// Window filter: [base+2m, base+4m] inclusive → 3 samples.
		win, err := s.TelemetrySamples(ctx, "srv1", base.Add(2*time.Minute), base.Add(4*time.Minute), 100)
		if err != nil {
			t.Fatalf("window query: %v", err)
		}
		if len(win) != 3 {
			t.Fatalf("expected 3 samples in [+2m,+4m], got %d", len(win))
		}

		// Decimation: cap to 3, still ascending, first == oldest, last == newest.
		dec, err := s.TelemetrySamples(ctx, "srv1", base, base.Add(10*time.Minute), 3)
		if err != nil {
			t.Fatalf("decimated query: %v", err)
		}
		// Exactly 3 evenly-spaced points, not a loose 1..3 bound: decimation maps
		// i in {0,1,2} onto all[i*(n-1)/(limit-1)] = indices {0,2,5} =
		// {base, base+2m, base+5m}. Asserting the exact count AND the interior
		// index is what actually protects the sampling contract — an
		// endpoints-only regression ([base, base+5m]) would pass a loose bound.
		if len(dec) != 3 {
			t.Fatalf("expected exactly 3 decimated samples, got %d", len(dec))
		}
		for i := 1; i < len(dec); i++ {
			if !dec[i].ReportedAt.After(dec[i-1].ReportedAt) {
				t.Fatalf("decimated not strictly ascending at %d: %v not after %v", i, dec[i].ReportedAt, dec[i-1].ReportedAt)
			}
		}
		if !dec[1].ReportedAt.Equal(base.Add(2 * time.Minute)) {
			t.Fatalf("decimated interior mis-spaced: got %v want %v", dec[1].ReportedAt, base.Add(2*time.Minute))
		}
		if !dec[0].ReportedAt.Equal(base) {
			t.Fatalf("decimated first is not oldest: got %v want %v", dec[0].ReportedAt, base)
		}
		if !dec[len(dec)-1].ReportedAt.Equal(base.Add(5 * time.Minute)) {
			t.Fatalf("decimated last is not newest: got %v want %v", dec[len(dec)-1].ReportedAt, base.Add(5*time.Minute))
		}

		// Prune: drop samples older than base+3m → only [+3m,+4m,+5m] remain.
		if err := s.PruneTelemetrySamples(ctx, base.Add(3*time.Minute)); err != nil {
			t.Fatalf("prune: %v", err)
		}
		after, err := s.TelemetrySamples(ctx, "srv1", base, base.Add(10*time.Minute), 100)
		if err != nil {
			t.Fatalf("post-prune query: %v", err)
		}
		if len(after) != 3 {
			t.Fatalf("expected 3 samples after prune, got %d", len(after))
		}
		if !after[0].ReportedAt.Equal(base.Add(3 * time.Minute)) {
			t.Fatalf("post-prune oldest should be +3m, got %v", after[0].ReportedAt)
		}

		// FK: inserting for an unknown server classifies as ErrNotFound.
		orphan := routing.TelemetrySample{ServerID: "nope", ReportedAt: now}
		if err := s.InsertTelemetrySample(ctx, orphan); err != ErrNotFound {
			t.Fatalf("expected ErrNotFound for unknown server, got %v", err)
		}
	})
}

// TestConformanceTelemetrySamplesSubSecondDistinct proves the store preserves
// sub-second precision on reported_at end to end. The energy reconciler
// integrates per-server power over each request's time window, and the agent's
// default collection cadence is now 1s — if the store truncated reported_at to
// whole seconds, two samples taken less than a second apart on the same server
// would collapse to the identical timestamp on read-back, making them
// indistinguishable to any consumer keying off it.
func TestConformanceTelemetrySamplesSubSecondDistinct(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		srv := routing.AIServer{
			ID: "srvsub", Name: "Sub", Domain: "srvsub.local", Provider: routing.ProviderOllama,
			Endpoint: "http://srvsub.local:11434", Status: routing.ServerStatusActive,
			HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateAIServer(ctx, srv); err != nil {
			t.Fatalf("create server: %v", err)
		}

		t1 := now.Add(250 * time.Millisecond)
		t2 := now.Add(750 * time.Millisecond)
		if err := s.InsertTelemetrySample(ctx, routing.TelemetrySample{ServerID: "srvsub", ReportedAt: t1, CPUUtilPct: 10}); err != nil {
			t.Fatalf("insert sample 1: %v", err)
		}
		if err := s.InsertTelemetrySample(ctx, routing.TelemetrySample{ServerID: "srvsub", ReportedAt: t2, CPUUtilPct: 20}); err != nil {
			t.Fatalf("insert sample 2: %v", err)
		}

		got, err := s.TelemetrySamples(ctx, "srvsub", now, now.Add(time.Second), 100)
		if err != nil {
			t.Fatalf("telemetry samples: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("expected 2 samples, got %d", len(got))
		}
		if got[0].ReportedAt.Equal(got[1].ReportedAt) {
			t.Fatalf("sub-second samples collapsed to the same reported_at on read-back: %v == %v", got[0].ReportedAt, got[1].ReportedAt)
		}
		if !got[0].ReportedAt.Equal(t1) {
			t.Fatalf("sample 1 reported_at did not round-trip sub-second: got %v want %v", got[0].ReportedAt, t1)
		}
		if !got[1].ReportedAt.Equal(t2) {
			t.Fatalf("sample 2 reported_at did not round-trip sub-second: got %v want %v", got[1].ReportedAt, t2)
		}
		if got[0].CPUUtilPct != 10 || got[1].CPUUtilPct != 20 {
			t.Fatalf("samples out of order after sub-second round-trip: %+v", got)
		}
	})
}

func TestConformanceServerAvailabilitySamples(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		mkServer := func(id string) {
			srv := routing.AIServer{
				ID: id, Name: id, Domain: id + ".local", Provider: routing.ProviderOllama,
				Endpoint: "http://" + id + ".local:11434", Status: routing.ServerStatusActive,
				HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
			}
			if err := s.CreateAIServer(ctx, srv); err != nil {
				t.Fatalf("create server %s: %v", id, err)
			}
		}
		mkServer("srv1")
		mkServer("srv2")

		base := now
		insert := func(serverID string, offset time.Duration, health string, reachable, active int, agent bool) {
			sample := routing.ServerAvailabilitySample{
				ServerID:       serverID,
				ReportedAt:     base.Add(offset),
				Health:         health,
				ReachableCount: reachable,
				ActiveCount:    active,
				AgentReporting: agent,
			}
			if err := s.InsertServerAvailabilitySample(ctx, sample); err != nil {
				t.Fatalf("insert %s @%v: %v", serverID, offset, err)
			}
		}
		hasAt := func(got []routing.ServerAvailabilitySample, offset time.Duration) bool {
			for _, g := range got {
				if g.ReportedAt.Equal(base.Add(offset)) {
					return true
				}
			}
			return false
		}

		// --- Reduction: contiguous same-state run collapses to its endpoints,
		// keeping the redundant middle-of-run heartbeat OUT while every state
		// transition stays IN. The healthy/agent=true run [+0m,+1m,+2m] must
		// collapse so +1m (equal to BOTH neighbors) is dropped.
		insert("srv1", 0, routing.HealthHealthy, 1, 1, true)                // run start
		insert("srv1", time.Minute, routing.HealthHealthy, 1, 1, true)      // redundant heartbeat
		insert("srv1", 2*time.Minute, routing.HealthHealthy, 1, 1, true)    // run end (pre-transition)
		insert("srv1", 3*time.Minute, routing.HealthUnhealthy, 0, 0, true)  // transition: health
		insert("srv1", 4*time.Minute, routing.HealthUnhealthy, 0, 0, false) // transition: agent presence
		insert("srv1", 5*time.Minute, routing.HealthHealthy, 1, 1, false)   // transition: health back

		got, err := s.ServerAvailabilitySamples(ctx, "srv1", base.Add(-time.Minute), base.Add(6*time.Minute), 10000)
		if err != nil {
			t.Fatalf("availability samples: %v", err)
		}
		if len(got) != 5 {
			t.Fatalf("expected 5 reduced samples, got %d: %+v", len(got), got)
		}
		if hasAt(got, time.Minute) {
			t.Fatalf("redundant contiguous heartbeat @+1m should be dropped: %+v", got)
		}
		for _, off := range []time.Duration{0, 2 * time.Minute, 3 * time.Minute, 4 * time.Minute, 5 * time.Minute} {
			if !hasAt(got, off) {
				t.Fatalf("expected transition/endpoint sample @%v kept: %+v", off, got)
			}
		}
		// Round-trip of a scalar field on the first sample.
		if got[0].Health != routing.HealthHealthy || !got[0].AgentReporting || got[0].ReachableCount != 1 || got[0].ActiveCount != 1 {
			t.Fatalf("first sample round-trip mismatch: %+v", got[0])
		}
		// gap_before: srv1 is sampled continuously (<= 1m spacing everywhere), so no
		// reduced sample may carry GapBefore (a within-floor predecessor yields false).
		for _, g := range got {
			if g.GapBefore {
				t.Fatalf("srv1 has no >gap-floor spacing; no reduced sample should carry GapBefore: %+v", g)
			}
		}

		// --- Gap boundary: a same-state sample separated by > gap floor keeps
		// BOTH boundaries. On srv2 the healthy/agent=false run [+0m,+1m,+2m]
		// drops the contiguous +1m heartbeat, but +2m and the far +42m (same
		// state, 40m apart) both survive because the > gap-floor gap is a
		// boundary that must not collapse.
		insert("srv2", 0, routing.HealthHealthy, 1, 0, false)
		insert("srv2", time.Minute, routing.HealthHealthy, 1, 0, false)    // redundant heartbeat
		insert("srv2", 2*time.Minute, routing.HealthHealthy, 1, 0, false)  // gap boundary (near side)
		insert("srv2", 42*time.Minute, routing.HealthHealthy, 1, 0, false) // gap boundary (far side, 40m gap)

		gap, err := s.ServerAvailabilitySamples(ctx, "srv2", base.Add(-time.Minute), base.Add(50*time.Minute), 10000)
		if err != nil {
			t.Fatalf("gap query: %v", err)
		}
		if len(gap) != 3 {
			t.Fatalf("expected 3 gap-reduced samples, got %d: %+v", len(gap), gap)
		}
		if hasAt(gap, time.Minute) {
			t.Fatalf("redundant contiguous heartbeat @+1m should be dropped (gap): %+v", gap)
		}
		if !hasAt(gap, 2*time.Minute) || !hasAt(gap, 42*time.Minute) {
			t.Fatalf("both gap boundaries (@+2m and @+42m) must be kept: %+v", gap)
		}
		// gap_before flag: the FAR side of the > gap-floor spacing (@+42m, whose raw
		// predecessor @+2m is 40m away) carries GapBefore=true; the NEAR side (@+2m,
		// whose raw predecessor @+1m is 1m away) stays false. Set only on read by the
		// reduction. Removing the reduce pre-pass makes both false and fails this.
		findAt := func(got []routing.ServerAvailabilitySample, offset time.Duration) (routing.ServerAvailabilitySample, bool) {
			for _, g := range got {
				if g.ReportedAt.Equal(base.Add(offset)) {
					return g, true
				}
			}
			return routing.ServerAvailabilitySample{}, false
		}
		if far, ok := findAt(gap, 42*time.Minute); !ok || !far.GapBefore {
			t.Fatalf("expected @+42m GapBefore=true (raw >gap-floor predecessor): %+v", gap)
		}
		if near, ok := findAt(gap, 2*time.Minute); !ok || near.GapBefore {
			t.Fatalf("expected @+2m GapBefore=false (within-floor raw predecessor): %+v", gap)
		}

		// --- NetBird dimension: a connected->disconnected->connected run with health
		// + agent HELD CONSTANT must be preserved as transitions by the state key, and
		// the netbird_connected column must round-trip. A state key that ignored
		// NetbirdConnected would collapse the middle sample (n=3, i=1) and drop to 2.
		mkServer("srv3")
		insert3 := func(offset time.Duration, connected bool) {
			sample := routing.ServerAvailabilitySample{
				ServerID:         "srv3",
				ReportedAt:       base.Add(offset),
				Health:           routing.HealthHealthy,
				ReachableCount:   1,
				ActiveCount:      1,
				AgentReporting:   true,
				NetbirdConnected: connected,
			}
			if err := s.InsertServerAvailabilitySample(ctx, sample); err != nil {
				t.Fatalf("insert srv3 @%v: %v", offset, err)
			}
		}
		insert3(0, true)             // connected (run start)
		insert3(time.Minute, false)  // transition: disconnected
		insert3(2*time.Minute, true) // transition: connected back
		nb, err := s.ServerAvailabilitySamples(ctx, "srv3", base.Add(-time.Minute), base.Add(3*time.Minute), 10000)
		if err != nil {
			t.Fatalf("srv3 availability samples: %v", err)
		}
		if len(nb) != 3 {
			t.Fatalf("expected 3 NetBird-transition samples (state key must fold NetbirdConnected), got %d: %+v", len(nb), nb)
		}
		if !nb[0].NetbirdConnected || nb[1].NetbirdConnected || !nb[2].NetbirdConnected {
			t.Fatalf("NetBird connectivity did not round-trip through the reduction: %+v", nb)
		}

		// --- FK: inserting for an unknown server classifies as ErrNotFound.
		orphan := routing.ServerAvailabilitySample{ServerID: "nope", ReportedAt: now, Health: routing.HealthHealthy}
		if err := s.InsertServerAvailabilitySample(ctx, orphan); err != ErrNotFound {
			t.Fatalf("expected ErrNotFound for unknown server, got %v", err)
		}

		// --- Prune: drop samples older than base+3m on srv1 → only the raw
		// samples >= base+3m remain ([+3m,+4m,+5m]).
		if err := s.PruneServerAvailabilitySamples(ctx, base.Add(3*time.Minute)); err != nil {
			t.Fatalf("prune: %v", err)
		}
		after, err := s.ServerAvailabilitySamples(ctx, "srv1", base.Add(-time.Minute), base.Add(6*time.Minute), 10000)
		if err != nil {
			t.Fatalf("post-prune query: %v", err)
		}
		if len(after) != 3 {
			t.Fatalf("expected 3 samples after prune, got %d: %+v", len(after), after)
		}
		if !after[0].ReportedAt.Equal(base.Add(3 * time.Minute)) {
			t.Fatalf("post-prune oldest should be +3m, got %v", after[0].ReportedAt)
		}
	})
}

func TestConformanceServerHardware(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		srv := routing.AIServer{
			ID: "hw1", Name: "hw1", Domain: "hw1.local", Provider: routing.ProviderOllama,
			Endpoint: "http://hw1.local:11434", Status: routing.ServerStatusActive,
			HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateAIServer(ctx, srv); err != nil {
			t.Fatalf("create server: %v", err)
		}

		// Absent -> (zero, false, nil).
		if _, ok, err := s.ServerHardwareByServer(ctx, "hw1"); err != nil || ok {
			t.Fatalf("ServerHardwareByServer(absent) = ok=%v err=%v, want ok=false", ok, err)
		}

		// Insert, then round-trip.
		first := routing.ServerHardware{
			ServerID: "hw1", CollectedAt: now, ReportJSON: `{"agent_version":"1.0"}`, UpdatedAt: now,
		}
		if err := s.UpsertServerHardware(ctx, first); err != nil {
			t.Fatalf("UpsertServerHardware(insert): %v", err)
		}
		got, ok, err := s.ServerHardwareByServer(ctx, "hw1")
		if err != nil || !ok {
			t.Fatalf("ServerHardwareByServer = ok=%v err=%v, want ok=true", ok, err)
		}
		if got.ReportJSON != `{"agent_version":"1.0"}` || !got.CollectedAt.Equal(now) {
			t.Fatalf("round-trip = %#v", got)
		}

		// Upsert overwrites the same server row (still exactly one; report replaced).
		second := routing.ServerHardware{
			ServerID: "hw1", CollectedAt: now.Add(time.Minute), ReportJSON: `{"agent_version":"2.0"}`, UpdatedAt: now.Add(time.Minute),
		}
		if err := s.UpsertServerHardware(ctx, second); err != nil {
			t.Fatalf("UpsertServerHardware(overwrite): %v", err)
		}
		got, ok, err = s.ServerHardwareByServer(ctx, "hw1")
		if err != nil || !ok || got.ReportJSON != `{"agent_version":"2.0"}` {
			t.Fatalf("after overwrite = %#v ok=%v err=%v", got, ok, err)
		}

		// Missing server -> ErrNotFound (FK violation classified).
		orphan := routing.ServerHardware{ServerID: "nope", CollectedAt: now, ReportJSON: "{}", UpdatedAt: now}
		if err := s.UpsertServerHardware(ctx, orphan); err != ErrNotFound {
			t.Fatalf("UpsertServerHardware(missing server) = %v, want ErrNotFound", err)
		}
	})
}

func TestConformanceBenchmarkRuns(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		srv := routing.AIServer{
			ID: "srv1", Name: "Server 1", Domain: "srv1.local", Provider: routing.ProviderOllama,
			Endpoint: "http://srv1.local:11434", Status: routing.ServerStatusActive,
			HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateAIServer(ctx, srv); err != nil {
			t.Fatalf("create server: %v", err)
		}
		app := routing.Application{
			ID: "app1", ServerID: "srv1", Type: routing.ProviderVLLM, Port: 8000, Scheme: "http",
			APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateApplication(ctx, app); err != nil {
			t.Fatalf("create application: %v", err)
		}
		mapping := routing.ModelMapping{
			ID: "map1", ApplicationID: "app1", GatewayModelName: "gw", AppModelName: "up",
			Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateMapping(ctx, mapping); err != nil {
			t.Fatalf("create mapping: %v", err)
		}

		// Three runs for one mapping, distinct gen tps + ascending timestamps.
		for i := 0; i < 3; i++ {
			run := routing.BenchmarkRun{
				MappingID:             "map1",
				ServerID:              "srv1",
				CreatedAt:             now.Add(time.Duration(i) * time.Minute),
				GenTokensPerSecond:    float64(10 + i),
				PromptTokensPerSecond: float64(100 + i),
				LoadTimeMS:            i * 5,
				ContextSize:           4096,
			}
			if err := s.InsertBenchmarkRun(ctx, run); err != nil {
				t.Fatalf("insert run %d: %v", i, err)
			}
		}

		// Newest-first: run at +2m (gen 12) leads, then +1m (11), then base (10).
		got, err := s.BenchmarkRunsByMapping(ctx, "map1", 50)
		if err != nil {
			t.Fatalf("runs by mapping: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("expected 3 runs, got %d", len(got))
		}
		for i := 1; i < len(got); i++ {
			if got[i].CreatedAt.After(got[i-1].CreatedAt) {
				t.Fatalf("runs not newest-first at %d: %v after %v", i, got[i].CreatedAt, got[i-1].CreatedAt)
			}
		}
		if got[0].GenTokensPerSecond != 12 {
			t.Fatalf("newest run gen tps = %v, want 12", got[0].GenTokensPerSecond)
		}
		// Every run gets a store-generated id (the runner never sets one) — pins
		// the SQLite/memory parity so a memory-backed dev deployment isn't blank.
		for i, r := range got {
			if r.ID == "" {
				t.Fatalf("run %d has empty id (store did not generate one)", i)
			}
		}
		if got[0].PromptTokensPerSecond != 102 || got[0].ContextSize != 4096 {
			t.Fatalf("field round-trip mismatch: %+v", got[0])
		}

		// Limit caps the result.
		if lim, err := s.BenchmarkRunsByMapping(ctx, "map1", 2); err != nil {
			t.Fatalf("limited query: %v", err)
		} else if len(lim) != 2 {
			t.Fatalf("expected 2 runs with limit=2, got %d", len(lim))
		}

		// FK: inserting for an unknown mapping classifies as ErrNotFound.
		orphan := routing.BenchmarkRun{MappingID: "nope", ServerID: "srv1", CreatedAt: now}
		if err := s.InsertBenchmarkRun(ctx, orphan); err != ErrNotFound {
			t.Fatalf("expected ErrNotFound for unknown mapping, got %v", err)
		}

		// Prune: drop runs older than base+2m → only the +2m run remains.
		if err := s.PruneBenchmarkRuns(ctx, now.Add(2*time.Minute)); err != nil {
			t.Fatalf("prune: %v", err)
		}
		after, err := s.BenchmarkRunsByMapping(ctx, "map1", 50)
		if err != nil {
			t.Fatalf("post-prune query: %v", err)
		}
		if len(after) != 1 {
			t.Fatalf("expected 1 run after prune, got %d", len(after))
		}
		if !after[0].CreatedAt.Equal(now.Add(2 * time.Minute)) {
			t.Fatalf("post-prune survivor should be +2m, got %v", after[0].CreatedAt)
		}

		// Capacity-kind row round-trips kind + the opaque curve JSON; a speed row
		// (kind left empty by the caller) reads back defaulted to "speed".
		curve := `{"max_concurrency":8,"recommended_concurrency":4,"gen_tokens_per_second_at_capacity":42.5,"memory_observed":true,"levels":[{"concurrency":1,"per_request_tokens_per_second":50}]}`
		if err := s.InsertBenchmarkRun(ctx, routing.BenchmarkRun{
			MappingID: "map1", ServerID: "srv1", CreatedAt: now.Add(10 * time.Minute),
			Kind: "capacity", CapacityCurve: curve,
		}); err != nil {
			t.Fatalf("insert capacity run: %v", err)
		}
		runs, err := s.BenchmarkRunsByMapping(ctx, "map1", 50)
		if err != nil {
			t.Fatalf("list runs after capacity: %v", err)
		}
		var capRun *routing.BenchmarkRun
		sawSpeed := false
		for i := range runs {
			if runs[i].Kind == "capacity" {
				capRun = &runs[i]
			} else if runs[i].Kind == "speed" {
				sawSpeed = true
			}
		}
		if !sawSpeed {
			t.Fatal("expected the earlier speed rows to read back with kind defaulted to \"speed\"")
		}
		if capRun == nil {
			t.Fatal("capacity run not found in history")
		}
		if capRun.CapacityCurve != curve {
			t.Errorf("capacity_curve round-trip mismatch:\n got %q\nwant %q", capRun.CapacityCurve, curve)
		}
	})
}

// TestConformanceBenchmarkRunsVisionCapable verifies the migration-v33
// vision_capable column on model_mapping_benchmarks: a kind=="vision" history row
// round-trips its definitive verdict (both true and false) through
// InsertBenchmarkRun/BenchmarkRunsByMapping, on both dialects.
func TestConformanceBenchmarkRunsVisionCapable(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		srv := routing.AIServer{
			ID: "srv1", Name: "Server 1", Domain: "srv1.local", Provider: routing.ProviderOllama,
			Endpoint: "http://srv1.local:11434", Status: routing.ServerStatusActive,
			HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateAIServer(ctx, srv); err != nil {
			t.Fatalf("create server: %v", err)
		}
		app := routing.Application{
			ID: "app1", ServerID: "srv1", Type: routing.ProviderVLLM, Port: 8000, Scheme: "http",
			APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateApplication(ctx, app); err != nil {
			t.Fatalf("create application: %v", err)
		}
		mapping := routing.ModelMapping{
			ID: "map1", ApplicationID: "app1", GatewayModelName: "gw", AppModelName: "up",
			Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateMapping(ctx, mapping); err != nil {
			t.Fatalf("create mapping: %v", err)
		}

		// A definitive vision-capable=true run round-trips.
		if err := s.InsertBenchmarkRun(ctx, routing.BenchmarkRun{
			MappingID: "map1", ServerID: "srv1", CreatedAt: now,
			Kind: "vision", VisionCapable: true,
		}); err != nil {
			t.Fatalf("insert vision-capable run: %v", err)
		}
		// A definitive vision-capable=false run round-trips distinctly.
		if err := s.InsertBenchmarkRun(ctx, routing.BenchmarkRun{
			MappingID: "map1", ServerID: "srv1", CreatedAt: now.Add(time.Minute),
			Kind: "vision", VisionCapable: false,
		}); err != nil {
			t.Fatalf("insert vision-not-capable run: %v", err)
		}

		runs, err := s.BenchmarkRunsByMapping(ctx, "map1", 50)
		if err != nil {
			t.Fatalf("runs by mapping: %v", err)
		}
		if len(runs) != 2 {
			t.Fatalf("expected 2 runs, got %d", len(runs))
		}
		// Newest-first: the +1m (false) run leads, then the base (true) run.
		if runs[0].Kind != "vision" || runs[0].VisionCapable {
			t.Fatalf("newest run = %+v, want kind=vision VisionCapable=false", runs[0])
		}
		if runs[1].Kind != "vision" || !runs[1].VisionCapable {
			t.Fatalf("oldest run = %+v, want kind=vision VisionCapable=true", runs[1])
		}
	})
}

// --- 6. Usage: Record + Query + Stats ---------------------------------------

func TestConformanceUsageRecordQueryStats(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		_ = ctx
		now := time.Now().UTC().Truncate(time.Second)

		evt := usage.Event{
			ID: "evt1", UserID: "u1", TokenID: "tok1", SessionID: "sess_1",
			SessionSource: "codex", AgentID: "agent_1",
			APIFlavor: "openai", Model: "gpt-4o-mini", RequestedModel: "gpt-oss-20b", Provider: "ollama", Host: "srv1.local",
			InputTokens: 10, OutputTokens: 20, TotalTokens: 30, LatencyMS: 123,
			HTTPStatus: 200, Status: "ok", CreatedAt: now,
		}
		if err := s.Record(evt); err != nil {
			t.Fatalf("record usage: %v", err)
		}
		if err := s.LastUsageError(); err != nil {
			t.Fatalf("record usage: %v", err)
		}

		page, err := s.Query(usage.Query{UserID: "u1", Page: 1, Limit: 25})
		if err != nil {
			t.Fatalf("Query returned err: %v", err)
		}
		if page.Total != 1 || len(page.Data) != 1 || page.Data[0].ID != "evt1" {
			t.Fatalf("query mismatch: %+v", page)
		}
		// session_id/session_source/agent_id/requested_model must survive the
		// round-trip on the Query (scanUsageRows) path.
		if got := page.Data[0]; got.SessionID != "sess_1" || got.SessionSource != "codex" || got.AgentID != "agent_1" {
			t.Fatalf("session provenance mismatch (query): session_id=%q session_source=%q agent_id=%q", got.SessionID, got.SessionSource, got.AgentID)
		}
		if got := page.Data[0].RequestedModel; got != "gpt-oss-20b" {
			t.Fatalf("RequestedModel (query) = %q, want gpt-oss-20b", got)
		}
		// ... and on the All (scanUsageEvents) path.
		all := s.All()
		if len(all) != 1 {
			t.Fatalf("all events = %d, want 1", len(all))
		}
		if got := all[0]; got.SessionID != "sess_1" || got.SessionSource != "codex" || got.AgentID != "agent_1" {
			t.Fatalf("session provenance mismatch (all): session_id=%q session_source=%q agent_id=%q", got.SessionID, got.SessionSource, got.AgentID)
		}
		if got := all[0].RequestedModel; got != "gpt-oss-20b" {
			t.Fatalf("RequestedModel (all) = %q, want gpt-oss-20b", got)
		}

		// Substring filter on session_source: "codex" matches, "claude-code" does not.
		hit, err := s.Query(usage.Query{UserID: "u1", SessionSource: "codex", Page: 1, Limit: 25})
		if err != nil {
			t.Fatalf("Query returned err: %v", err)
		}
		if hit.Total != 1 || len(hit.Data) != 1 || hit.Data[0].ID != "evt1" {
			t.Fatalf("session_source=codex filter mismatch: %+v", hit)
		}
		miss, err := s.Query(usage.Query{UserID: "u1", SessionSource: "claude-code", Page: 1, Limit: 25})
		if err != nil {
			t.Fatalf("Query returned err: %v", err)
		}
		if miss.Total != 0 || len(miss.Data) != 0 {
			t.Fatalf("session_source=claude-code filter should return no rows: %+v", miss)
		}

		stats, err := s.Stats(usage.Query{UserID: "u1"})
		if err != nil {
			t.Fatalf("Stats returned err: %v", err)
		}
		if stats.Totals.TotalRequests != 1 || stats.Totals.InputTokens != 10 || stats.Totals.OutputTokens != 20 {
			t.Fatalf("stats mismatch: %+v", stats.Totals)
		}

		// requested_model is reachable by the free-text q search and is a
		// whitelisted sort key (issue #7) -- mirrors
		// usage.TestRequestedModelRoundTripSearchAndSort against the SQL driver.
		evt2 := usage.Event{
			ID: "evt2", UserID: "u1", Model: "gpt-4o-mini", RequestedModel: "claude-sonnet",
			Provider: "ollama", Host: "srv1.local", HTTPStatus: 200, Status: "ok",
			CreatedAt: now.Add(time.Minute),
		}
		if err := s.Record(evt2); err != nil {
			t.Fatalf("record usage evt2: %v", err)
		}

		qPage, err := s.Query(usage.Query{UserID: "u1", Q: "gpt-oss", Page: 1, Limit: 25})
		if err != nil {
			t.Fatalf("Query(q=gpt-oss) returned err: %v", err)
		}
		if qPage.Total != 1 || len(qPage.Data) != 1 || qPage.Data[0].ID != "evt1" {
			t.Fatalf("q=gpt-oss matched %+v, want exactly evt1", qPage)
		}

		sortPage, err := s.Query(usage.Query{UserID: "u1", Sort: "requested_model", Order: "asc", Page: 1, Limit: 25})
		if err != nil {
			t.Fatalf("Query(sort=requested_model) returned err: %v", err)
		}
		if len(sortPage.Data) != 2 || sortPage.Data[0].RequestedModel != "claude-sonnet" || sortPage.Data[1].RequestedModel != "gpt-oss-20b" {
			t.Fatalf("sort order = %+v, want [claude-sonnet gpt-oss-20b]", sortPage.Data)
		}
	})
}

// --- 6a2. Usage: UsageGroups aggregation GROUP BY (dimension, host) ---------

// TestConformanceUsageGroups verifies UsageGroups aggregates the filtered set
// GROUP BY (dimension, host) — one bucket per (group value, host) — with the
// per-bucket count/error/token sums, summed energy, and min/max created_at all
// correct on both dialects, and that an exact filter composes into the same
// aggregation. The test folds buckets by Key (summing) exactly as the portal
// layer will, so the cross-host fold (a session served by two servers) is
// exercised. Error events use Status="error" AND http_status>=400 so the SQL
// approximation (lower(status)='error' or http_status>=400) and the Go IsError
// agree.
func TestConformanceUsageGroups(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		t0 := now
		t1 := now.Add(1 * time.Minute)
		t2 := now.Add(2 * time.Minute)
		t3 := now.Add(3 * time.Minute)

		// 4 events across 2 servers (distinct host id + server_name), 2 sessions,
		// 2 models, one error, non-zero energy, distinct created_at.
		events := []usage.Event{
			// sess_a on srv1 (server-one), gpt-4o
			{
				ID: "e1", UserID: "u1", TokenID: "tok1", SessionID: "sess_a", Model: "gpt-4o", Host: "srv1", ServerName: "server-one",
				InputTokens: 10, OutputTokens: 20, CachedTokens: 2, CacheWriteTokens: 1, EnergyWh: 0.5, Status: "ok", HTTPStatus: 200, CreatedAt: t0,
			},
			{
				ID: "e2", UserID: "u1", TokenID: "tok1", SessionID: "sess_a", Model: "gpt-4o", Host: "srv1", ServerName: "server-one",
				InputTokens: 5, OutputTokens: 7, CachedTokens: 1, CacheWriteTokens: 0, EnergyWh: 0.25, Status: "ok", HTTPStatus: 200, CreatedAt: t1,
			},
			// sess_b on srv2 (server-two), claude, ERROR
			{
				ID: "e3", UserID: "u1", TokenID: "tok1", SessionID: "sess_b", Model: "claude", Host: "srv2", ServerName: "server-two",
				InputTokens: 100, OutputTokens: 200, CachedTokens: 10, CacheWriteTokens: 5, EnergyWh: 1.0, Status: "error", HTTPStatus: 500, CreatedAt: t2,
			},
			// sess_b on srv1 (server-one), gpt-4o — same session, DIFFERENT host
			{
				ID: "e4", UserID: "u1", TokenID: "tok1", SessionID: "sess_b", Model: "gpt-4o", Host: "srv1", ServerName: "server-one",
				InputTokens: 3, OutputTokens: 4, CachedTokens: 0, CacheWriteTokens: 0, EnergyWh: 0.1, Status: "ok", HTTPStatus: 200, CreatedAt: t3,
			},
		}
		for _, e := range events {
			s.Record(e)
			if err := s.LastUsageError(); err != nil {
				t.Fatalf("record %s: %v", e.ID, err)
			}
		}

		// foldByKey sums buckets sharing a Key (as the portal layer does), and
		// tracks the min FirstAt / max LastAt across the folded buckets.
		type folded struct {
			count, errCount, in, out, cached, cacheWrite int
			energy                                       float64
			first, last                                  time.Time
			hosts                                        map[string]bool
		}
		foldByKey := func(buckets []usage.GroupBucket) map[string]*folded {
			m := make(map[string]*folded)
			for _, b := range buckets {
				f := m[b.Key]
				if f == nil {
					f = &folded{first: b.FirstAt, last: b.LastAt, hosts: map[string]bool{}}
					m[b.Key] = f
				}
				f.count += b.Count
				f.errCount += b.ErrorCount
				f.in += b.InputTokens
				f.out += b.OutputTokens
				f.cached += b.CachedTokens
				f.cacheWrite += b.CacheWriteTokens
				f.energy += b.EnergyWh
				if b.FirstAt.Before(f.first) {
					f.first = b.FirstAt
				}
				if b.LastAt.After(f.last) {
					f.last = b.LastAt
				}
				f.hosts[b.Host] = true
			}
			return m
		}

		base := usage.Query{UserID: "u1"}

		// --- group by session -------------------------------------------------
		sessBuckets, err := s.UsageGroups(ctx, base, "session")
		if err != nil {
			t.Fatalf("UsageGroups(session): %v", err)
		}
		// sess_b spans two hosts (srv1 + srv2) -> two raw buckets before folding.
		if len(sessBuckets) != 3 {
			t.Fatalf("session buckets = %d, want 3 (sess_a/srv1, sess_b/srv1, sess_b/srv2): %+v", len(sessBuckets), sessBuckets)
		}
		sess := foldByKey(sessBuckets)
		if a := sess["sess_a"]; a == nil {
			t.Fatalf("sess_a bucket missing: %+v", sess)
		} else if a.count != 2 || a.errCount != 0 || a.in != 15 || a.out != 27 || a.cached != 3 || a.cacheWrite != 1 || a.energy != 0.75 {
			t.Fatalf("sess_a folded mismatch: %+v", a)
		} else if !a.first.Equal(t0) || !a.last.Equal(t1) {
			t.Fatalf("sess_a first/last = %v/%v, want %v/%v", a.first, a.last, t0, t1)
		}
		if b := sess["sess_b"]; b == nil {
			t.Fatalf("sess_b bucket missing: %+v", sess)
		} else if b.count != 2 || b.errCount != 1 || b.in != 103 || b.out != 204 || b.cached != 10 || b.cacheWrite != 5 || b.energy != 1.1 {
			t.Fatalf("sess_b folded mismatch: %+v", b)
		} else if !b.first.Equal(t2) || !b.last.Equal(t3) {
			t.Fatalf("sess_b first/last = %v/%v, want %v/%v", b.first, b.last, t2, t3)
		} else if !b.hosts["srv1"] || !b.hosts["srv2"] {
			t.Fatalf("sess_b should span srv1+srv2, got %+v", b.hosts)
		}

		// --- group by server (server_name) -> assert the right Host per server -
		srvBuckets, err := s.UsageGroups(ctx, base, "server")
		if err != nil {
			t.Fatalf("UsageGroups(server): %v", err)
		}
		if len(srvBuckets) != 2 {
			t.Fatalf("server buckets = %d, want 2: %+v", len(srvBuckets), srvBuckets)
		}
		hostByServer := make(map[string]string)
		countByServer := make(map[string]int)
		for _, b := range srvBuckets {
			hostByServer[b.Key] = b.Host
			countByServer[b.Key] = b.Count
		}
		if hostByServer["server-one"] != "srv1" || hostByServer["server-two"] != "srv2" {
			t.Fatalf("server->host mismatch: %+v", hostByServer)
		}
		if countByServer["server-one"] != 3 || countByServer["server-two"] != 1 {
			t.Fatalf("server counts mismatch: server-one=%d server-two=%d", countByServer["server-one"], countByServer["server-two"])
		}

		// --- exact filter composes: SessionIDExact=sess_a, group by model ------
		q := base
		q.SessionIDExact = "sess_a"
		modelBuckets, err := s.UsageGroups(ctx, q, "model")
		if err != nil {
			t.Fatalf("UsageGroups(model, sess_a): %v", err)
		}
		// Only e1+e2 (sess_a, gpt-4o, srv1) -> a single bucket; claude never appears.
		if len(modelBuckets) != 1 {
			t.Fatalf("model buckets under sess_a = %d, want 1: %+v", len(modelBuckets), modelBuckets)
		}
		mb := modelBuckets[0]
		if mb.Key != "gpt-4o" || mb.Host != "srv1" || mb.Count != 2 || mb.InputTokens != 15 || mb.OutputTokens != 27 {
			t.Fatalf("model bucket (sess_a) mismatch: %+v", mb)
		}

		// --- invalid group_by is an error ------------------------------------
		if _, err := s.UsageGroups(ctx, base, "bogus"); err == nil {
			t.Fatalf("UsageGroups(bogus) should error")
		}
	})
}

// TestConformanceUsageGroupsServiceDimension proves usageGroupColumn's "service"
// whitelist entry maps to the service_id column (service accounts, Phase 1),
// folds correctly across two hosts like every other dimension, and that an empty
// service_id (ordinary user/session usage — the overwhelming default) forms its
// own group rather than erroring or being silently dropped, on both dialects.
func TestConformanceUsageGroupsServiceDimension(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		events := []usage.Event{
			// svc_1 spans two hosts -> two raw buckets before folding.
			{ID: "e1", UserID: "u1", ServiceID: "svc_1", ServiceName: "Nightly Batch", Host: "srv1", InputTokens: 10, OutputTokens: 20, CreatedAt: now},
			{ID: "e2", UserID: "u1", ServiceID: "svc_1", ServiceName: "Nightly Batch", Host: "srv2", InputTokens: 3, OutputTokens: 4, CreatedAt: now.Add(time.Minute)},
			// A different service, one host.
			{ID: "e3", UserID: "u1", ServiceID: "svc_2", ServiceName: "Other Svc", Host: "srv1", InputTokens: 1, OutputTokens: 1, CreatedAt: now},
			// Ordinary user-token usage (no service attribution): empty service_id.
			{ID: "e4", UserID: "u1", Host: "srv1", InputTokens: 100, OutputTokens: 200, CreatedAt: now},
		}
		for _, e := range events {
			s.Record(e)
			if err := s.LastUsageError(); err != nil {
				t.Fatalf("record %s: %v", e.ID, err)
			}
		}

		base := usage.Query{UserID: "u1"}
		buckets, err := s.UsageGroups(ctx, base, "service")
		if err != nil {
			t.Fatalf("UsageGroups(service): %v", err)
		}
		if len(buckets) != 4 {
			t.Fatalf("service buckets = %d, want 4 (svc_1/srv1, svc_1/srv2, svc_2/srv1, ''/srv1): %+v", len(buckets), buckets)
		}
		countByKey := map[string]int{}
		inputByKey := map[string]int{}
		for _, b := range buckets {
			countByKey[b.Key] += b.Count
			inputByKey[b.Key] += b.InputTokens
		}
		if countByKey["svc_1"] != 2 || inputByKey["svc_1"] != 13 {
			t.Fatalf("svc_1 folded = count=%d input=%d, want count=2 input=13", countByKey["svc_1"], inputByKey["svc_1"])
		}
		if countByKey["svc_2"] != 1 || inputByKey["svc_2"] != 1 {
			t.Fatalf("svc_2 folded = count=%d input=%d, want count=1 input=1", countByKey["svc_2"], inputByKey["svc_2"])
		}
		if countByKey[""] != 1 || inputByKey[""] != 100 {
			t.Fatalf("empty-service (user-token usage) folded = count=%d input=%d, want count=1 input=100", countByKey[""], inputByKey[""])
		}
	})
}

// TestConformanceUsageGroupsProjectDimension proves usageGroupColumn's
// "project" whitelist entry maps to the project_id column (design spec §7),
// folds correctly across two hosts like every other dimension, and that an
// empty project_id (ordinary/unattributed usage — the overwhelming default)
// forms its own group rather than erroring or being silently dropped, on both
// dialects. Also proves the ProjectIDExact drill-down filter and the
// ProjectIDs applyUsageScope (§8) IN-list — including its EMPTY-slice-means-
// zero-rows security guard — compose with UsageGroups/Query identically.
func TestConformanceUsageGroupsProjectDimension(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		events := []usage.Event{
			// proj_1 spans two hosts -> two raw buckets before folding.
			{ID: "e1", UserID: "u1", ProjectID: "proj_1", ProjectName: "Widgets", Host: "srv1", InputTokens: 10, OutputTokens: 20, CreatedAt: now},
			{ID: "e2", UserID: "u1", ProjectID: "proj_1", ProjectName: "Widgets", Host: "srv2", InputTokens: 3, OutputTokens: 4, CreatedAt: now.Add(time.Minute)},
			// A different project, one host.
			{ID: "e3", UserID: "u1", ProjectID: "proj_2", ProjectName: "Gadgets", Host: "srv1", InputTokens: 1, OutputTokens: 1, CreatedAt: now},
			// Ordinary usage with no project attribution: empty project_id.
			{ID: "e4", UserID: "u1", Host: "srv1", InputTokens: 100, OutputTokens: 200, CreatedAt: now},
		}
		for _, e := range events {
			s.Record(e)
			if err := s.LastUsageError(); err != nil {
				t.Fatalf("record %s: %v", e.ID, err)
			}
		}

		base := usage.Query{UserID: "u1"}
		buckets, err := s.UsageGroups(ctx, base, "project")
		if err != nil {
			t.Fatalf("UsageGroups(project): %v", err)
		}
		if len(buckets) != 4 {
			t.Fatalf("project buckets = %d, want 4 (proj_1/srv1, proj_1/srv2, proj_2/srv1, ''/srv1): %+v", len(buckets), buckets)
		}
		countByKey, inputByKey := map[string]int{}, map[string]int{}
		for _, b := range buckets {
			countByKey[b.Key] += b.Count
			inputByKey[b.Key] += b.InputTokens
		}
		if countByKey["proj_1"] != 2 || inputByKey["proj_1"] != 13 {
			t.Fatalf("proj_1 folded = count=%d input=%d, want count=2 input=13", countByKey["proj_1"], inputByKey["proj_1"])
		}
		if countByKey["proj_2"] != 1 || inputByKey["proj_2"] != 1 {
			t.Fatalf("proj_2 folded = count=%d input=%d, want count=1 input=1", countByKey["proj_2"], inputByKey["proj_2"])
		}
		if countByKey[""] != 1 || inputByKey[""] != 100 {
			t.Fatalf("empty-project folded = count=%d input=%d, want count=1 input=100", countByKey[""], inputByKey[""])
		}

		// --- ProjectIDExact drill-down composes with UsageGroups ---------------
		q := base
		q.HasProjectIDExact, q.ProjectIDExact = true, "proj_1"
		hostBuckets, err := s.UsageGroups(ctx, q, "server")
		if err != nil {
			t.Fatalf("UsageGroups(server, proj_1): %v", err)
		}
		var total int
		for _, b := range hostBuckets {
			total += b.Count
		}
		if total != 2 {
			t.Fatalf("server buckets under proj_1 total = %d, want 2 (e1+e2, not e3/e4): %+v", total, hostBuckets)
		}

		// --- ProjectIDs IN-list (applyUsageScope §8 scope) composes with Query --
		if got, err := s.Query(usage.Query{UserID: "u1", ProjectIDs: []string{"proj_1", "proj_2"}}); err != nil || got.Total != 3 {
			t.Fatalf("Query ProjectIDs=[proj_1,proj_2] total = %d, want 3 (e1+e2+e3)", got.Total)
		}
		// The security-critical guard: a non-nil EMPTY slice matches ZERO rows,
		// never "no filter" -- a caller who is a member of no project must never
		// see any project-attributed (or unattributed) row via this predicate.
		if got, err := s.Query(usage.Query{UserID: "u1", ProjectIDs: []string{}}); err != nil || got.Total != 0 {
			t.Fatalf("Query ProjectIDs=[] total = %d, want 0", got.Total)
		}
	})
}

// --- 6b. Usage: energy writer + reconcile queries (P2 T2) -------------------

// TestConformanceUpdateUsageEventEnergy verifies the energy writer sets exactly
// the three energy columns by id, and is a benign no-op on a missing id, on both
// dialects.
func TestConformanceUpdateUsageEventEnergy(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		evt := usage.Event{
			ID: "evt1", UserID: "u1", Model: "gpt-4o-mini", Provider: "ollama",
			Host: "srv1", OutputTokens: 20, LatencyMS: 100, CreatedAt: now,
		}
		s.Record(evt)
		if err := s.LastUsageError(); err != nil {
			t.Fatalf("record usage: %v", err)
		}

		if err := s.UpdateUsageEventEnergy(ctx, "evt1", 0.5, 0.2, "measured"); err != nil {
			t.Fatalf("update usage event energy: %v", err)
		}
		all := s.All()
		if len(all) != 1 {
			t.Fatalf("all events = %d, want 1", len(all))
		}
		got := all[0]
		if got.EnergyWh != 0.5 || got.EnergyMarginalWh != 0.2 || got.EnergySource != "measured" {
			t.Fatalf("event energy = %+v, want 0.5/0.2/measured", got)
		}
		// Every other column must be untouched by the energy write.
		if got.Model != "gpt-4o-mini" || got.OutputTokens != 20 {
			t.Fatalf("update usage event energy clobbered other columns: %+v", got)
		}

		// A write on a MISSING event id is a benign no-op (0 rows affected).
		if err := s.UpdateUsageEventEnergy(ctx, "does-not-exist", 9, 9, "modeled"); err != nil {
			t.Fatalf("update usage event energy (missing) = %v, want nil (benign no-op)", err)
		}
	})
}

// TestConformanceUnpricedUsageEvents verifies the reconciler's source query:
// only energy_source=="" events inside [notBefore, notAfter], oldest-first,
// capped at limit — on both dialects.
func TestConformanceUnpricedUsageEvents(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		base := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)

		seed := func(id string, at time.Time, source string) {
			s.Record(usage.Event{ID: id, UserID: "u1", Host: "srv1", CreatedAt: at, EnergySource: source})
			if err := s.LastUsageError(); err != nil {
				t.Fatalf("record usage %s: %v", id, err)
			}
		}
		seed("priced", base, "measured")
		seed("too_old", base.Add(-2*time.Hour), "")
		seed("too_new", base.Add(2*time.Hour), "")
		seed("unpriced_2", base.Add(10*time.Minute), "")
		seed("unpriced_1", base.Add(5*time.Minute), "")

		got, err := s.UnpricedUsageEvents(ctx, base.Add(-time.Hour), base.Add(time.Hour), 10)
		if err != nil {
			t.Fatalf("unpriced usage events: %v", err)
		}
		if len(got) != 2 || got[0].ID != "unpriced_1" || got[1].ID != "unpriced_2" {
			t.Fatalf("ids = %v, want [unpriced_1 unpriced_2] (oldest-first, energy_source=\"\" only, in window)", usageEventIDs(got))
		}

		// limit caps the result to the oldest N.
		limited, err := s.UnpricedUsageEvents(ctx, base.Add(-time.Hour), base.Add(time.Hour), 1)
		if err != nil {
			t.Fatalf("unpriced usage events (limited): %v", err)
		}
		if len(limited) != 1 || limited[0].ID != "unpriced_1" {
			t.Fatalf("limited = %v, want [unpriced_1]", usageEventIDs(limited))
		}

		// A non-positive limit falls back to a default cap rather than 0 rows.
		unbounded, err := s.UnpricedUsageEvents(ctx, base.Add(-time.Hour), base.Add(time.Hour), 0)
		if err != nil {
			t.Fatalf("unpriced usage events (limit=0): %v", err)
		}
		if len(unbounded) != 2 {
			t.Fatalf("limit=0 ids = %v, want the 2 in-window rows (default cap, not zero)", usageEventIDs(unbounded))
		}

		// A second call (e.g. the next reconcile tick, after the reconciler has
		// stamped the previous batch) sees none of the now-priced rows: proves
		// energy_source=="" is the idempotency gate, not this test alone.
		for _, e := range got {
			if err := s.UpdateUsageEventEnergy(ctx, e.ID, 1, 1, "modeled"); err != nil {
				t.Fatalf("stamp %s: %v", e.ID, err)
			}
		}
		after, err := s.UnpricedUsageEvents(ctx, base.Add(-time.Hour), base.Add(time.Hour), 10)
		if err != nil {
			t.Fatalf("unpriced usage events (after stamping): %v", err)
		}
		if len(after) != 0 {
			t.Fatalf("after stamping ids = %v, want none (a stamped modeled-0 row must not be reprocessed)", usageEventIDs(after))
		}
	})
}

// TestConformanceUsageEventsForServerWindow verifies the concurrency-sibling
// query: events on serverID whose own [created_at-latency, created_at] window
// overlaps [from, to] — including a foreign server exclusion and a
// partial-overlap inclusion — on both dialects.
func TestConformanceUsageEventsForServerWindow(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()

		seed := func(id, host string, latencyMS int64, at time.Time) {
			s.Record(usage.Event{ID: id, UserID: "u1", Host: host, LatencyMS: latencyMS, CreatedAt: at})
			if err := s.LastUsageError(); err != nil {
				t.Fatalf("record usage %s: %v", id, err)
			}
		}
		// A 10s request ending at 10:00:10 (window [10:00:00,10:00:10]) on "srv1".
		seed("req_a", "srv1", 10000, time.Date(2026, 8, 6, 10, 0, 10, 0, time.UTC))
		// Same window, a DIFFERENT server — must be excluded.
		seed("req_other_server", "srv2", 10000, time.Date(2026, 8, 6, 10, 0, 10, 0, time.UTC))
		// Entirely before the query window on srv1 — must be excluded.
		seed("req_before", "srv1", 1000, time.Date(2026, 8, 6, 9, 0, 1, 0, time.UTC))
		// Entirely after the query window on srv1 — must be excluded.
		seed("req_after", "srv1", 1000, time.Date(2026, 8, 6, 11, 0, 1, 0, time.UTC))
		// Only partially overlaps the query window (starts before, ends inside) —
		// must be INCLUDED (overlap, not containment).
		seed("req_partial", "srv1", 20000, time.Date(2026, 8, 6, 10, 0, 5, 0, time.UTC))
		// Starts INSIDE the query window (10:00:15) but its created_at (=end) is
		// AFTER `to` (10:00:30 > 10:00:20) — it is still running past the window.
		// Overlap holds (start <= to) so this must be INCLUDED. This is the
		// mirror case of req_partial (which starts before `from`): a naive SQL
		// prefetch that widens only the LOWER created_at bound drops this row
		// before the Go overlap filter ever sees it.
		seed("req_ends_after", "srv1", 15000, time.Date(2026, 8, 6, 10, 0, 30, 0, time.UTC))

		from := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)
		to := time.Date(2026, 8, 6, 10, 0, 20, 0, time.UTC)
		got, err := s.UsageEventsForServerWindow(ctx, "srv1", from, to)
		if err != nil {
			t.Fatalf("usage events for server window: %v", err)
		}
		ids := usageEventIDs(got)
		if len(ids) != 3 || !usageEventIDsContain(ids, "req_a") || !usageEventIDsContain(ids, "req_partial") || !usageEventIDsContain(ids, "req_ends_after") {
			t.Fatalf("ids = %v, want [req_a req_partial req_ends_after]", ids)
		}
	})
}

func usageEventIDs(events []usage.Event) []string {
	ids := make([]string, len(events))
	for i, e := range events {
		ids[i] = e.ID
	}
	return ids
}

func usageEventIDsContain(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// --- 7. Capture + chat round-trip -------------------------------------------

func TestConformanceCaptureAndChatRoundTrip(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		if err := s.CreateUser(ctx, newTestUser("u1", "cap@example.test", now)); err != nil {
			t.Fatalf("create user: %v", err)
		}

		evt := usage.Event{
			ID: "evt-cap", UserID: "u1", TokenID: "tok1", SessionID: "sess1",
			APIFlavor: "openai", Model: "gpt-4o-mini", Provider: "ollama", Host: "srv1.local",
			HTTPStatus: 200, Status: "ok", CreatedAt: now,
		}
		s.Record(evt)
		if err := s.LastUsageError(); err != nil {
			t.Fatalf("record usage: %v", err)
		}

		capture := Capture{
			UsageEventID: "evt-cap", KeyVersion: 1, Blob: []byte("sealed-bytes-here"),
			CreatedAt: now, Secret: false,
		}
		if err := s.SaveCapture(ctx, capture); err != nil {
			t.Fatalf("save capture: %v", err)
		}
		gotCapture, err := s.Capture(ctx, "evt-cap")
		if err != nil {
			t.Fatalf("read capture: %v", err)
		}
		if string(gotCapture.Blob) != "sealed-bytes-here" || gotCapture.OwnerUserID != "u1" {
			t.Fatalf("capture round-trip mismatch: %+v", gotCapture)
		}

		// chat CRUD
		if err := s.CreateChat(ctx, Chat{
			ID: "chat1", UserID: "u1", Title: "hello", KeyVersion: 0,
			Blob: []byte("{}"), CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create chat: %v", err)
		}
		chatRow, err := s.ChatByID(ctx, "chat1")
		if err != nil || chatRow.Title != "hello" {
			t.Fatalf("chat by id: %v %+v", err, chatRow)
		}
		summaries, err := s.ChatsByUser(ctx, "u1")
		if err != nil || len(summaries) != 1 || summaries[0].ID != "chat1" {
			t.Fatalf("chats by user: %v %+v", err, summaries)
		}
	})
}

// --- 8. System settings + UI prefs upsert -----------------------------------

func TestConformanceSystemSettingsAndUIPreferencesUpsert(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		if err := s.CreateUser(ctx, newTestUser("u1", "prefs@example.test", now)); err != nil {
			t.Fatalf("create user: %v", err)
		}

		// system_settings: insert then upsert (ON CONFLICT DO UPDATE).
		if err := s.SetSystemSetting(ctx, "health_check_interval_seconds", "30", now); err != nil {
			t.Fatalf("set system setting: %v", err)
		}
		if err := s.SetSystemSetting(ctx, "health_check_interval_seconds", "60", now.Add(time.Minute)); err != nil {
			t.Fatalf("upsert system setting: %v", err)
		}
		settings, err := s.SystemSettings(ctx)
		if err != nil || settings["health_check_interval_seconds"] != "60" {
			t.Fatalf("system settings not upserted: %v %+v", err, settings)
		}

		// user_ui_preferences: insert then upsert (ON CONFLICT DO UPDATE).
		if err := s.SetUIPreference(ctx, "u1", "activity.columns", `{"a":1}`); err != nil {
			t.Fatalf("set ui preference: %v", err)
		}
		if err := s.SetUIPreference(ctx, "u1", "activity.columns", `{"a":2}`); err != nil {
			t.Fatalf("upsert ui preference: %v", err)
		}
		prefs, err := s.UIPreferences(ctx, "u1")
		if err != nil || len(prefs) != 1 || prefs[0].ValueJSON != `{"a":2}` {
			t.Fatalf("ui preference not upserted: %v %+v", err, prefs)
		}
	})
}

// --- 9. Case-insensitive filter: the ILIKE proof ----------------------------

func TestConformanceUsageQueryCaseInsensitiveModelFilter(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		now := time.Now().UTC().Truncate(time.Second)
		evt := usage.Event{
			ID: "evt-ci", UserID: "u1", TokenID: "tok1", SessionID: "sess1",
			APIFlavor: "openai", Model: "GPT-XYZ", Provider: "ollama", Host: "srv1.local",
			HTTPStatus: 200, Status: "ok", CreatedAt: now,
		}
		if err := s.Record(evt); err != nil {
			t.Fatalf("record usage: %v", err)
		}
		if err := s.LastUsageError(); err != nil {
			t.Fatalf("record usage: %v", err)
		}

		// sqlite's LIKE is ASCII case-insensitive; postgres needs ILIKE. dl.ilike()
		// is exactly the seam that makes this filter match on both dialects.
		page, err := s.Query(usage.Query{UserID: "u1", Model: "gpt-xyz", Page: 1, Limit: 25})
		if err != nil {
			t.Fatalf("Query returned err: %v", err)
		}
		if page.Total != 1 || len(page.Data) != 1 {
			t.Fatalf("expected case-insensitive model match, got %+v", page)
		}
		if page.Data[0].Model != "GPT-XYZ" {
			t.Fatalf("unexpected matched row: %+v", page.Data[0])
		}

		// The free-text Q filter goes through the same ilike() seam.
		pageQ, err := s.Query(usage.Query{UserID: "u1", Q: "gpt-xyz", Page: 1, Limit: 25})
		if err != nil {
			t.Fatalf("Query returned err: %v", err)
		}
		if pageQ.Total != 1 {
			t.Fatalf("expected case-insensitive Q match, got %+v", pageQ)
		}

		// A non-matching needle must not match on either dialect.
		none, err := s.Query(usage.Query{UserID: "u1", Model: "totally-different", Page: 1, Limit: 25})
		if err != nil {
			t.Fatalf("Query returned err: %v", err)
		}
		if none.Total != 0 {
			t.Fatalf("expected no match, got %+v", none)
		}
	})
}

// TestConformanceServerOwnersRoundTrip covers SetServerOwners/ServerOwners/
// ServersByOwner on both dialects. It also passes a DUPLICATE owner id to
// exercise the `on conflict do nothing` insert (the statement that was
// previously the sqlite-only `insert or ignore`, invalid on postgres).
func TestConformanceServerOwnersRoundTrip(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		for _, id := range []string{"ux", "uy"} {
			if err := s.CreateUser(ctx, User{ID: id, Email: id + "@example.test", DisplayName: id, Role: "user", Status: "active", PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
				t.Fatalf("create user %s: %v", id, err)
			}
		}
		if err := s.CreateAIServer(ctx, routing.AIServer{ID: "srvo", Name: "Owned", Domain: "o.local", Provider: routing.ProviderOllama, Endpoint: "http://o.local:11434", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("create server: %v", err)
		}
		// Duplicate "ux" hits the on-conflict-do-nothing path.
		if err := s.SetServerOwners(ctx, "srvo", []string{"ux", "uy", "ux"}); err != nil {
			t.Fatalf("set owners: %v", err)
		}
		owners, err := s.ServerOwners(ctx, "srvo")
		if err != nil {
			t.Fatalf("server owners: %v", err)
		}
		if len(owners) != 2 {
			t.Fatalf("expected 2 distinct owners, got %d: %v", len(owners), owners)
		}
		byOwner, err := s.ServersByOwner(ctx, "ux")
		if err != nil || len(byOwner) != 1 || byOwner[0].ID != "srvo" {
			t.Fatalf("servers by owner: %v %+v", err, byOwner)
		}
		// Re-set replaces (delete-then-insert): ux only.
		if err := s.SetServerOwners(ctx, "srvo", []string{"ux"}); err != nil {
			t.Fatalf("re-set owners: %v", err)
		}
		owners, _ = s.ServerOwners(ctx, "srvo")
		if len(owners) != 1 || owners[0] != "ux" {
			t.Fatalf("expected only ux after re-set, got %v", owners)
		}
	})
}

// TestConformanceForeignKeyViolationMapsNotFound pins strict dialect parity for
// the insert paths that reference another table: an FK violation (a missing
// referenced row) must map to ErrNotFound on BOTH sqlite and postgres — not
// ErrConflict on sqlite (whose FK error text also contains "constraint failed")
// and a generic error on postgres (SQLSTATE 23503, not the unique 23505).
func TestConformanceForeignKeyViolationMapsNotFound(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		const missing = "usr_does_not_exist"

		if err := s.CreateChat(ctx, Chat{ID: "c_fk", UserID: missing, Title: "x", KeyVersion: 0, Blob: []byte("{}"), CreatedAt: now, UpdatedAt: now}); err != ErrNotFound {
			t.Fatalf("CreateChat missing user = %v, want ErrNotFound", err)
		}
		if err := s.CreateSession(ctx, Session{ID: "s_fk", UserID: missing, SecretHash: "h_fk", CreatedAt: now, ExpiresAt: now.Add(time.Hour), LastSeenAt: now}); err != ErrNotFound {
			t.Fatalf("CreateSession missing user = %v, want ErrNotFound", err)
		}
		if err := s.CreatePlainToken(ctx, TokenRecord{ID: "t_fk", UserID: missing, Name: "n", Status: "active", Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}, "fk-secret-value"); err != ErrNotFound {
			t.Fatalf("CreatePlainToken missing user = %v, want ErrNotFound", err)
		}
		// SaveCapture references usage_events(id); a missing usage event is an FK violation.
		if err := s.SaveCapture(ctx, Capture{UsageEventID: "evt_missing", KeyVersion: 0, Blob: []byte("x"), CreatedAt: now}); err != ErrNotFound {
			t.Fatalf("SaveCapture missing usage event = %v, want ErrNotFound", err)
		}
	})
}

// TestConformanceModelMappingMetricsRoundTrip verifies the per-model-mapping
// performance-metric columns (migration v9) round-trip through create, a direct
// read (MappingByID), and the routing join (ActiveMappingsForModel) on both
// dialects.
func TestConformanceModelMappingMetricsRoundTrip(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		srv := routing.AIServer{
			ID: "srv1", Name: "Server 1", Domain: "srv1.local", Provider: routing.ProviderOllama,
			Endpoint: "http://srv1.local:11434", Status: routing.ServerStatusActive,
			HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateAIServer(ctx, srv); err != nil {
			t.Fatalf("create server: %v", err)
		}

		app := routing.Application{
			ID: "app1", ServerID: "srv1", Type: "ollama", Port: 11434, Scheme: "http",
			APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 1, Weight: 1,
			TimeoutMS: 30000, AffinityTTLSeconds: 300, Status: routing.ServerStatusActive,
			HealthCheckMode: routing.HealthCheckModeAlwaysReachable,
			CreatedAt:       now, UpdatedAt: now,
		}
		if err := s.CreateApplication(ctx, app); err != nil {
			t.Fatalf("create application: %v", err)
		}

		metricsAt := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
		mapping := routing.ModelMapping{
			ID: "map1", ApplicationID: "app1", GatewayModelName: "gpt-4o-mini",
			AppModelName: "llama3", Status: routing.ServerStatusActive,
			GenTokensPerSecond:           42.5,
			PromptTokensPerSecond:        1300.25,
			LoadTimeMS:                   8200,
			ContextSize:                  131072,
			IsMTP:                        true,
			MetricsLocked:                false,
			MetricsUpdatedAt:             &metricsAt,
			MetricsSource:                "manual",
			MaxConcurrency:               16,
			RecommendedConcurrency:       12,
			GenTokensPerSecondAtCapacity: 640.5,
			CreatedAt:                    now, UpdatedAt: now,
		}
		if err := s.CreateMapping(ctx, mapping); err != nil {
			t.Fatalf("create mapping: %v", err)
		}

		got, err := s.MappingByID(ctx, "map1")
		if err != nil {
			t.Fatalf("mapping by id: %v", err)
		}
		if got.GenTokensPerSecond != 42.5 {
			t.Fatalf("GenTokensPerSecond = %v, want 42.5", got.GenTokensPerSecond)
		}
		if got.PromptTokensPerSecond != 1300.25 {
			t.Fatalf("PromptTokensPerSecond = %v, want 1300.25", got.PromptTokensPerSecond)
		}
		if got.LoadTimeMS != 8200 {
			t.Fatalf("LoadTimeMS = %v, want 8200", got.LoadTimeMS)
		}
		if got.ContextSize != 131072 {
			t.Fatalf("ContextSize = %v, want 131072", got.ContextSize)
		}
		if !got.IsMTP {
			t.Fatalf("IsMTP = %v, want true", got.IsMTP)
		}
		if got.MetricsLocked {
			t.Fatalf("MetricsLocked = %v, want false", got.MetricsLocked)
		}
		if got.MetricsSource != "manual" {
			t.Fatalf("MetricsSource = %q, want %q", got.MetricsSource, "manual")
		}
		if got.MetricsUpdatedAt == nil || !got.MetricsUpdatedAt.Equal(metricsAt) {
			t.Fatalf("MetricsUpdatedAt = %v, want %v", got.MetricsUpdatedAt, metricsAt)
		}
		if got.MaxConcurrency != 16 {
			t.Fatalf("MaxConcurrency = %v, want 16", got.MaxConcurrency)
		}
		if got.RecommendedConcurrency != 12 {
			t.Fatalf("RecommendedConcurrency = %v, want 12", got.RecommendedConcurrency)
		}
		if got.GenTokensPerSecondAtCapacity != 640.5 {
			t.Fatalf("GenTokensPerSecondAtCapacity = %v, want 640.5", got.GenTokensPerSecondAtCapacity)
		}

		cands, err := s.ActiveMappingsForModel(ctx, "gpt-4o-mini", routing.APIFlavorOpenAI)
		if err != nil {
			t.Fatalf("active mappings: %v", err)
		}
		if len(cands) != 1 {
			t.Fatalf("expected 1 candidate, got %d: %+v", len(cands), cands)
		}
		cm := cands[0].Mapping
		if cm.ContextSize != 131072 {
			t.Fatalf("candidate ContextSize = %v, want 131072", cm.ContextSize)
		}
		if !cm.IsMTP {
			t.Fatalf("candidate IsMTP = %v, want true", cm.IsMTP)
		}
		if cm.MetricsLocked {
			t.Fatalf("candidate MetricsLocked = %v, want false", cm.MetricsLocked)
		}
		if cm.MetricsSource != "manual" {
			t.Fatalf("candidate MetricsSource = %q, want %q", cm.MetricsSource, "manual")
		}
		if cm.MetricsUpdatedAt == nil || !cm.MetricsUpdatedAt.Equal(metricsAt) {
			t.Fatalf("candidate MetricsUpdatedAt = %v, want %v", cm.MetricsUpdatedAt, metricsAt)
		}
		if cm.MaxConcurrency != 16 || cm.RecommendedConcurrency != 12 || cm.GenTokensPerSecondAtCapacity != 640.5 {
			t.Fatalf("candidate capacity metrics = %d/%d/%v, want 16/12/640.5", cm.MaxConcurrency, cm.RecommendedConcurrency, cm.GenTokensPerSecondAtCapacity)
		}

		// UpdateMapping must persist the metric columns too: mutate a couple of
		// distinct metrics and confirm they round-trip through the UPDATE + read.
		mapping.ContextSize = 65536
		mapping.IsMTP = false
		mapping.MaxConcurrency = 32
		mapping.RecommendedConcurrency = 24
		mapping.GenTokensPerSecondAtCapacity = 900
		mapping.UpdatedAt = now.Add(time.Minute)
		if err := s.UpdateMapping(ctx, mapping); err != nil {
			t.Fatalf("update mapping: %v", err)
		}
		updated, err := s.MappingByID(ctx, "map1")
		if err != nil {
			t.Fatalf("mapping by id after update: %v", err)
		}
		if updated.ContextSize != 65536 {
			t.Fatalf("updated ContextSize = %v, want 65536", updated.ContextSize)
		}
		if updated.IsMTP {
			t.Fatalf("updated IsMTP = %v, want false", updated.IsMTP)
		}
		if updated.MaxConcurrency != 32 || updated.RecommendedConcurrency != 24 || updated.GenTokensPerSecondAtCapacity != 900 {
			t.Fatalf("updated capacity metrics = %d/%d/%v, want 32/24/900", updated.MaxConcurrency, updated.RecommendedConcurrency, updated.GenTokensPerSecondAtCapacity)
		}
	})
}

// TestConformanceModelMappingEnergyWhPerTokenRoundTrip verifies the
// model_mappings.energy_wh_per_token column (migration v36) round-trips
// through create, a direct read (MappingByID), the routing join
// (ActiveMappingsForModel), and an update, on both dialects.
func TestConformanceModelMappingEnergyWhPerTokenRoundTrip(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		srv := routing.AIServer{
			ID: "srv1", Name: "Server 1", Domain: "srv1.local", Provider: routing.ProviderOllama,
			Endpoint: "http://srv1.local:11434", Status: routing.ServerStatusActive,
			HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateAIServer(ctx, srv); err != nil {
			t.Fatalf("create server: %v", err)
		}

		app := routing.Application{
			ID: "app1", ServerID: "srv1", Type: "ollama", Port: 11434, Scheme: "http",
			APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 1, Weight: 1,
			TimeoutMS: 30000, AffinityTTLSeconds: 300, Status: routing.ServerStatusActive,
			HealthCheckMode: routing.HealthCheckModeAlwaysReachable,
			CreatedAt:       now, UpdatedAt: now,
		}
		if err := s.CreateApplication(ctx, app); err != nil {
			t.Fatalf("create application: %v", err)
		}

		mapping := routing.ModelMapping{
			ID: "map1", ApplicationID: "app1", GatewayModelName: "gpt-4o-mini",
			AppModelName: "llama3", Status: routing.ServerStatusActive,
			EnergyWhPerToken: 0.0025,
			CreatedAt:        now, UpdatedAt: now,
		}
		if err := s.CreateMapping(ctx, mapping); err != nil {
			t.Fatalf("create mapping: %v", err)
		}

		got, err := s.MappingByID(ctx, "map1")
		if err != nil {
			t.Fatalf("mapping by id: %v", err)
		}
		if got.EnergyWhPerToken != 0.0025 {
			t.Fatalf("EnergyWhPerToken = %v, want 0.0025", got.EnergyWhPerToken)
		}

		cands, err := s.ActiveMappingsForModel(ctx, "gpt-4o-mini", routing.APIFlavorOpenAI)
		if err != nil {
			t.Fatalf("active mappings: %v", err)
		}
		if len(cands) != 1 {
			t.Fatalf("expected 1 candidate, got %d: %+v", len(cands), cands)
		}
		if cands[0].Mapping.EnergyWhPerToken != 0.0025 {
			t.Fatalf("candidate EnergyWhPerToken = %v, want 0.0025", cands[0].Mapping.EnergyWhPerToken)
		}

		// UpdateMapping must persist the column too.
		mapping.EnergyWhPerToken = 0.0091
		mapping.UpdatedAt = now.Add(time.Minute)
		if err := s.UpdateMapping(ctx, mapping); err != nil {
			t.Fatalf("update mapping: %v", err)
		}
		updated, err := s.MappingByID(ctx, "map1")
		if err != nil {
			t.Fatalf("mapping by id after update: %v", err)
		}
		if updated.EnergyWhPerToken != 0.0091 {
			t.Fatalf("updated EnergyWhPerToken = %v, want 0.0091", updated.EnergyWhPerToken)
		}
	})
}

// TestConformanceUpdateMappingContextProbe verifies the context-probe write path:
// it stamps context_size + provenance ("probe") only while the mapping is
// unlocked, and is a benign no-op when the mapping is locked or missing, on both
// dialects.
func TestConformanceUpdateMappingContextProbe(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		srv := routing.AIServer{
			ID: "srv1", Name: "Server 1", Domain: "srv1.local", Provider: routing.ProviderOllama,
			Endpoint: "http://srv1.local:11434", Status: routing.ServerStatusActive,
			HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateAIServer(ctx, srv); err != nil {
			t.Fatalf("create server: %v", err)
		}

		app := routing.Application{
			ID: "app1", ServerID: "srv1", Type: "ollama", Port: 11434, Scheme: "http",
			APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 1, Weight: 1,
			TimeoutMS: 30000, AffinityTTLSeconds: 300, Status: routing.ServerStatusActive,
			HealthCheckMode: routing.HealthCheckModeAlwaysReachable,
			CreatedAt:       now, UpdatedAt: now,
		}
		if err := s.CreateApplication(ctx, app); err != nil {
			t.Fatalf("create application: %v", err)
		}

		mapping := routing.ModelMapping{
			ID: "map1", ApplicationID: "app1", GatewayModelName: "gpt-4o-mini",
			AppModelName: "up", Status: routing.ServerStatusActive,
			ContextSize: 0, MetricsLocked: false,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateMapping(ctx, mapping); err != nil {
			t.Fatalf("create mapping: %v", err)
		}

		// A probe on an unlocked mapping stamps context_size + provenance.
		probeAt := time.Date(2026, 7, 23, 13, 0, 0, 0, time.UTC)
		if err := s.UpdateMappingContextProbe(ctx, "map1", 131072, probeAt); err != nil {
			t.Fatalf("update mapping context probe: %v", err)
		}
		got, err := s.MappingByID(ctx, "map1")
		if err != nil {
			t.Fatalf("mapping by id: %v", err)
		}
		if got.ContextSize != 131072 {
			t.Fatalf("ContextSize = %v, want 131072", got.ContextSize)
		}
		if got.MetricsSource != "probe" {
			t.Fatalf("MetricsSource = %q, want %q", got.MetricsSource, "probe")
		}
		if got.MetricsUpdatedAt == nil || !got.MetricsUpdatedAt.Equal(probeAt) {
			t.Fatalf("MetricsUpdatedAt = %v, want %v", got.MetricsUpdatedAt, probeAt)
		}

		// Lock the mapping (manual pin) via UpdateMapping.
		mapping.ContextSize = 4096
		mapping.MetricsLocked = true
		mapping.MetricsSource = "manual"
		mapping.UpdatedAt = now.Add(time.Minute)
		if err := s.UpdateMapping(ctx, mapping); err != nil {
			t.Fatalf("update mapping (lock): %v", err)
		}

		// A probe on a LOCKED mapping is a no-op (no error, no change).
		laterAt := time.Date(2026, 7, 23, 14, 0, 0, 0, time.UTC)
		if err := s.UpdateMappingContextProbe(ctx, "map1", 999999, laterAt); err != nil {
			t.Fatalf("update mapping context probe (locked): %v", err)
		}
		locked, err := s.MappingByID(ctx, "map1")
		if err != nil {
			t.Fatalf("mapping by id (locked): %v", err)
		}
		if locked.ContextSize != 4096 {
			t.Fatalf("locked ContextSize = %v, want 4096 (probe must not overwrite a lock)", locked.ContextSize)
		}
		if locked.MetricsSource != "manual" {
			t.Fatalf("locked MetricsSource = %q, want %q (probe must not overwrite a lock)", locked.MetricsSource, "manual")
		}

		// A probe on a MISSING mapping is a benign no-op.
		if err := s.UpdateMappingContextProbe(ctx, "does-not-exist", 8192, laterAt); err != nil {
			t.Fatalf("update mapping context probe (missing) = %v, want nil (benign no-op)", err)
		}
	})
}

// TestConformanceUpdateMappingBenchmarkMetrics verifies the benchmark-metrics
// write path: it stamps gen/prompt throughput + load time + provenance
// ("benchmark") only while the mapping is unlocked, and is a benign no-op when
// the mapping is locked or missing, on both dialects.
func TestConformanceUpdateMappingBenchmarkMetrics(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		srv := routing.AIServer{
			ID: "srv1", Name: "Server 1", Domain: "srv1.local", Provider: routing.ProviderOllama,
			Endpoint: "http://srv1.local:11434", Status: routing.ServerStatusActive,
			HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateAIServer(ctx, srv); err != nil {
			t.Fatalf("create server: %v", err)
		}

		app := routing.Application{
			ID: "app1", ServerID: "srv1", Type: "ollama", Port: 11434, Scheme: "http",
			APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 1, Weight: 1,
			TimeoutMS: 30000, AffinityTTLSeconds: 300, Status: routing.ServerStatusActive,
			HealthCheckMode: routing.HealthCheckModeAlwaysReachable,
			CreatedAt:       now, UpdatedAt: now,
		}
		if err := s.CreateApplication(ctx, app); err != nil {
			t.Fatalf("create application: %v", err)
		}

		mapping := routing.ModelMapping{
			ID: "m1", ApplicationID: "app1", GatewayModelName: "gpt-4o-mini",
			AppModelName: "up", Status: routing.ServerStatusActive,
			MetricsLocked: false,
			CreatedAt:     now, UpdatedAt: now,
		}
		if err := s.CreateMapping(ctx, mapping); err != nil {
			t.Fatalf("create mapping: %v", err)
		}

		// A benchmark write on an unlocked mapping stamps the metrics + provenance.
		benchAt := time.Date(2026, 7, 23, 13, 0, 0, 0, time.UTC)
		if err := s.UpdateMappingBenchmarkMetrics(ctx, "m1", 55.5, 1200.0, 8300, benchAt); err != nil {
			t.Fatalf("update mapping benchmark metrics: %v", err)
		}
		got, err := s.MappingByID(ctx, "m1")
		if err != nil {
			t.Fatalf("mapping by id: %v", err)
		}
		if got.GenTokensPerSecond != 55.5 {
			t.Fatalf("GenTokensPerSecond = %v, want 55.5", got.GenTokensPerSecond)
		}
		if got.PromptTokensPerSecond != 1200.0 {
			t.Fatalf("PromptTokensPerSecond = %v, want 1200.0", got.PromptTokensPerSecond)
		}
		if got.LoadTimeMS != 8300 {
			t.Fatalf("LoadTimeMS = %v, want 8300", got.LoadTimeMS)
		}
		if got.MetricsSource != "benchmark" {
			t.Fatalf("MetricsSource = %q, want %q", got.MetricsSource, "benchmark")
		}
		if got.MetricsUpdatedAt == nil || !got.MetricsUpdatedAt.Equal(benchAt) {
			t.Fatalf("MetricsUpdatedAt = %v, want %v", got.MetricsUpdatedAt, benchAt)
		}

		// Lock the mapping (manual pin) via UpdateMapping.
		mapping.GenTokensPerSecond = 1
		mapping.PromptTokensPerSecond = 1
		mapping.LoadTimeMS = 1
		mapping.MetricsLocked = true
		mapping.MetricsSource = "manual"
		mapping.UpdatedAt = now.Add(time.Minute)
		if err := s.UpdateMapping(ctx, mapping); err != nil {
			t.Fatalf("update mapping (lock): %v", err)
		}

		// A benchmark write on a LOCKED mapping is a no-op (no error, no change).
		laterAt := time.Date(2026, 7, 23, 14, 0, 0, 0, time.UTC)
		if err := s.UpdateMappingBenchmarkMetrics(ctx, "m1", 999, 999, 999, laterAt); err != nil {
			t.Fatalf("update mapping benchmark metrics (locked): %v", err)
		}
		locked, err := s.MappingByID(ctx, "m1")
		if err != nil {
			t.Fatalf("mapping by id (locked): %v", err)
		}
		if locked.GenTokensPerSecond != 1 {
			t.Fatalf("locked GenTokensPerSecond = %v, want 1 (benchmark must not overwrite a lock)", locked.GenTokensPerSecond)
		}
		if locked.MetricsSource != "manual" {
			t.Fatalf("locked MetricsSource = %q, want %q (benchmark must not overwrite a lock)", locked.MetricsSource, "manual")
		}

		// A benchmark write on a MISSING mapping is a benign no-op.
		if err := s.UpdateMappingBenchmarkMetrics(ctx, "does-not-exist", 42, 42, 42, laterAt); err != nil {
			t.Fatalf("update mapping benchmark metrics (missing) = %v, want nil (benign no-op)", err)
		}
	})
}

// TestConformanceMappingOpportunisticMetrics verifies the opportunistic EWMA
// write path: it seeds a stored-0 column from the first positive sample, blends
// subsequent samples, leaves a column with a non-positive sample untouched,
// stamps provenance ("opportunistic") + metrics_updated_at, and is a benign
// no-op when the mapping is locked, on both dialects.
func TestConformanceMappingOpportunisticMetrics(t *testing.T) {
	const tol = 1e-9
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		srv := routing.AIServer{
			ID: "srv1", Name: "Server 1", Domain: "srv1.local", Provider: routing.ProviderOllama,
			Endpoint: "http://srv1.local:11434", Status: routing.ServerStatusActive,
			HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateAIServer(ctx, srv); err != nil {
			t.Fatalf("create server: %v", err)
		}

		app := routing.Application{
			ID: "app1", ServerID: "srv1", Type: "ollama", Port: 11434, Scheme: "http",
			APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 1, Weight: 1,
			TimeoutMS: 30000, AffinityTTLSeconds: 300, Status: routing.ServerStatusActive,
			HealthCheckMode: routing.HealthCheckModeAlwaysReachable,
			CreatedAt:       now, UpdatedAt: now,
		}
		if err := s.CreateApplication(ctx, app); err != nil {
			t.Fatalf("create application: %v", err)
		}

		mapping := routing.ModelMapping{
			ID: "m1", ApplicationID: "app1", GatewayModelName: "gpt-4o-mini",
			AppModelName: "up", Status: routing.ServerStatusActive,
			MetricsLocked: false,
			CreatedAt:     now, UpdatedAt: now,
		}
		if err := s.CreateMapping(ctx, mapping); err != nil {
			t.Fatalf("create mapping: %v", err)
		}

		// (a) A first positive sample against a stored 0 seeds the value directly.
		at1 := time.Date(2026, 7, 23, 13, 0, 0, 0, time.UTC)
		if err := s.UpdateMappingOpportunisticMetrics(ctx, "m1", 100, 0, 0.2, at1); err != nil {
			t.Fatalf("update opportunistic metrics (seed): %v", err)
		}
		got, err := s.MappingByID(ctx, "m1")
		if err != nil {
			t.Fatalf("mapping by id: %v", err)
		}
		if diff := got.GenTokensPerSecond - 100; diff < -tol || diff > tol {
			t.Fatalf("GenTokensPerSecond = %v, want 100 (seed on first positive)", got.GenTokensPerSecond)
		}
		// (c) A non-positive gen sample here would have skipped; the prompt sample
		// was 0 too so prompt stays 0 (independent per column).
		if got.PromptTokensPerSecond != 0 {
			t.Fatalf("PromptTokensPerSecond = %v, want 0 (non-positive sample must not touch it)", got.PromptTokensPerSecond)
		}
		// (e) Provenance + timestamp are stamped.
		if got.MetricsSource != "opportunistic" {
			t.Fatalf("MetricsSource = %q, want %q", got.MetricsSource, "opportunistic")
		}
		if got.MetricsUpdatedAt == nil || !got.MetricsUpdatedAt.Equal(at1) {
			t.Fatalf("MetricsUpdatedAt = %v, want %v", got.MetricsUpdatedAt, at1)
		}

		// (b) A second positive sample blends: 0.2*200 + 0.8*100 = 120.
		at2 := time.Date(2026, 7, 23, 14, 0, 0, 0, time.UTC)
		if err := s.UpdateMappingOpportunisticMetrics(ctx, "m1", 200, 0, 0.2, at2); err != nil {
			t.Fatalf("update opportunistic metrics (blend): %v", err)
		}
		got, err = s.MappingByID(ctx, "m1")
		if err != nil {
			t.Fatalf("mapping by id: %v", err)
		}
		if diff := got.GenTokensPerSecond - 120; diff < -tol || diff > tol {
			t.Fatalf("GenTokensPerSecond = %v, want 120 (EWMA blend)", got.GenTokensPerSecond)
		}

		// (c) A gen sample of 0 leaves gen unchanged even though the row is updated
		// (a positive prompt sample seeds prompt).
		at3 := time.Date(2026, 7, 23, 15, 0, 0, 0, time.UTC)
		if err := s.UpdateMappingOpportunisticMetrics(ctx, "m1", 0, 300, 0.2, at3); err != nil {
			t.Fatalf("update opportunistic metrics (gen-skip): %v", err)
		}
		got, err = s.MappingByID(ctx, "m1")
		if err != nil {
			t.Fatalf("mapping by id: %v", err)
		}
		if diff := got.GenTokensPerSecond - 120; diff < -tol || diff > tol {
			t.Fatalf("GenTokensPerSecond = %v, want 120 (non-positive gen sample must not change it)", got.GenTokensPerSecond)
		}
		if diff := got.PromptTokensPerSecond - 300; diff < -tol || diff > tol {
			t.Fatalf("PromptTokensPerSecond = %v, want 300 (seed on first positive)", got.PromptTokensPerSecond)
		}

		// (d) Lock the mapping; an opportunistic write is a benign no-op.
		mapping.GenTokensPerSecond = got.GenTokensPerSecond
		mapping.PromptTokensPerSecond = got.PromptTokensPerSecond
		mapping.MetricsLocked = true
		mapping.MetricsSource = "manual"
		mapping.UpdatedAt = now.Add(time.Minute)
		if err := s.UpdateMapping(ctx, mapping); err != nil {
			t.Fatalf("update mapping (lock): %v", err)
		}
		at4 := time.Date(2026, 7, 23, 16, 0, 0, 0, time.UTC)
		if err := s.UpdateMappingOpportunisticMetrics(ctx, "m1", 9999, 9999, 0.2, at4); err != nil {
			t.Fatalf("update opportunistic metrics (locked): %v", err)
		}
		locked, err := s.MappingByID(ctx, "m1")
		if err != nil {
			t.Fatalf("mapping by id (locked): %v", err)
		}
		if diff := locked.GenTokensPerSecond - 120; diff < -tol || diff > tol {
			t.Fatalf("locked GenTokensPerSecond = %v, want 120 (opportunistic must not overwrite a lock)", locked.GenTokensPerSecond)
		}
		if locked.MetricsSource != "manual" {
			t.Fatalf("locked MetricsSource = %q, want %q (opportunistic must not overwrite a lock)", locked.MetricsSource, "manual")
		}

		// A write on a MISSING mapping is a benign no-op.
		if err := s.UpdateMappingOpportunisticMetrics(ctx, "does-not-exist", 42, 42, 0.2, at4); err != nil {
			t.Fatalf("update opportunistic metrics (missing) = %v, want nil (benign no-op)", err)
		}
	})
}

// TestConformanceMappingEnergyEWMA verifies the energy-calibration write path
// (P2 T2): it EWMA-blends energy_wh_per_token from a live per-token sample +
// stamps provenance ("energy") only while the mapping is unlocked, and is a
// benign no-op when the mapping is locked or missing, on both dialects. Mirrors
// TestConformanceMappingOpportunisticMetrics's structure exactly.
func TestConformanceMappingEnergyEWMA(t *testing.T) {
	const tol = 1e-9
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		srv := routing.AIServer{
			ID: "srv1", Name: "Server 1", Domain: "srv1.local", Provider: routing.ProviderOllama,
			Endpoint: "http://srv1.local:11434", Status: routing.ServerStatusActive,
			HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateAIServer(ctx, srv); err != nil {
			t.Fatalf("create server: %v", err)
		}

		app := routing.Application{
			ID: "app1", ServerID: "srv1", Type: "ollama", Port: 11434, Scheme: "http",
			APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 1, Weight: 1,
			TimeoutMS: 30000, AffinityTTLSeconds: 300, Status: routing.ServerStatusActive,
			HealthCheckMode: routing.HealthCheckModeAlwaysReachable,
			CreatedAt:       now, UpdatedAt: now,
		}
		if err := s.CreateApplication(ctx, app); err != nil {
			t.Fatalf("create application: %v", err)
		}

		mapping := routing.ModelMapping{
			ID: "m1", ApplicationID: "app1", GatewayModelName: "gpt-4o-mini",
			AppModelName: "up", Status: routing.ServerStatusActive,
			MetricsLocked: false,
			CreatedAt:     now, UpdatedAt: now,
		}
		if err := s.CreateMapping(ctx, mapping); err != nil {
			t.Fatalf("create mapping: %v", err)
		}

		// (a) A first positive sample against a stored 0 seeds the value directly.
		at1 := time.Date(2026, 8, 6, 13, 0, 0, 0, time.UTC)
		if err := s.UpdateMappingEnergyEWMA(ctx, "m1", 0.002, 0.2, at1); err != nil {
			t.Fatalf("update energy ewma (seed): %v", err)
		}
		got, err := s.MappingByID(ctx, "m1")
		if err != nil {
			t.Fatalf("mapping by id: %v", err)
		}
		if diff := got.EnergyWhPerToken - 0.002; diff < -tol || diff > tol {
			t.Fatalf("EnergyWhPerToken = %v, want 0.002 (seed on first positive)", got.EnergyWhPerToken)
		}
		// (e) Provenance + timestamp are stamped.
		if got.MetricsSource != "energy" {
			t.Fatalf("MetricsSource = %q, want %q", got.MetricsSource, "energy")
		}
		if got.MetricsUpdatedAt == nil || !got.MetricsUpdatedAt.Equal(at1) {
			t.Fatalf("MetricsUpdatedAt = %v, want %v", got.MetricsUpdatedAt, at1)
		}

		// (b) A second positive sample blends: 0.2*0.004 + 0.8*0.002 = 0.0024.
		at2 := time.Date(2026, 8, 6, 14, 0, 0, 0, time.UTC)
		if err := s.UpdateMappingEnergyEWMA(ctx, "m1", 0.004, 0.2, at2); err != nil {
			t.Fatalf("update energy ewma (blend): %v", err)
		}
		got, err = s.MappingByID(ctx, "m1")
		if err != nil {
			t.Fatalf("mapping by id: %v", err)
		}
		if diff := got.EnergyWhPerToken - 0.0024; diff < -tol || diff > tol {
			t.Fatalf("EnergyWhPerToken = %v, want 0.0024 (EWMA blend)", got.EnergyWhPerToken)
		}

		// (c) A non-positive sample leaves the coefficient unchanged (but the row
		// is still touched — provenance/timestamp still advance, matching the
		// opportunistic writer's per-column CASE semantics).
		at3 := time.Date(2026, 8, 6, 15, 0, 0, 0, time.UTC)
		if err := s.UpdateMappingEnergyEWMA(ctx, "m1", 0, 0.2, at3); err != nil {
			t.Fatalf("update energy ewma (skip): %v", err)
		}
		got, err = s.MappingByID(ctx, "m1")
		if err != nil {
			t.Fatalf("mapping by id: %v", err)
		}
		if diff := got.EnergyWhPerToken - 0.0024; diff < -tol || diff > tol {
			t.Fatalf("EnergyWhPerToken = %v, want 0.0024 (non-positive sample must not change it)", got.EnergyWhPerToken)
		}
		if got.MetricsUpdatedAt == nil || !got.MetricsUpdatedAt.Equal(at3) {
			t.Fatalf("MetricsUpdatedAt = %v, want %v (still stamped even on a skipped sample)", got.MetricsUpdatedAt, at3)
		}

		// (d) Lock the mapping; an energy-EWMA write is a benign no-op.
		mapping.EnergyWhPerToken = got.EnergyWhPerToken
		mapping.MetricsLocked = true
		mapping.MetricsSource = "manual"
		mapping.UpdatedAt = now.Add(time.Minute)
		if err := s.UpdateMapping(ctx, mapping); err != nil {
			t.Fatalf("update mapping (lock): %v", err)
		}
		at4 := time.Date(2026, 8, 6, 16, 0, 0, 0, time.UTC)
		if err := s.UpdateMappingEnergyEWMA(ctx, "m1", 9.999, 0.2, at4); err != nil {
			t.Fatalf("update energy ewma (locked): %v", err)
		}
		locked, err := s.MappingByID(ctx, "m1")
		if err != nil {
			t.Fatalf("mapping by id (locked): %v", err)
		}
		if diff := locked.EnergyWhPerToken - 0.0024; diff < -tol || diff > tol {
			t.Fatalf("locked EnergyWhPerToken = %v, want 0.0024 (energy EWMA must not overwrite a lock)", locked.EnergyWhPerToken)
		}
		if locked.MetricsSource != "manual" {
			t.Fatalf("locked MetricsSource = %q, want %q (energy EWMA must not overwrite a lock)", locked.MetricsSource, "manual")
		}

		// A write on a MISSING mapping is a benign no-op.
		if err := s.UpdateMappingEnergyEWMA(ctx, "does-not-exist", 0.1, 0.2, at4); err != nil {
			t.Fatalf("update energy ewma (missing) = %v, want nil (benign no-op)", err)
		}
	})
}

// TestConformanceMappingCapacityMetrics verifies the capacity-benchmark write
// path: it stamps max/recommended concurrency + gen-tok/s-at-capacity +
// provenance ("capacity") only while the mapping is unlocked, and is a benign
// no-op when the mapping is locked or missing, on both dialects.
func TestConformanceMappingCapacityMetrics(t *testing.T) {
	const tol = 1e-9
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		srv := routing.AIServer{
			ID: "srv1", Name: "Server 1", Domain: "srv1.local", Provider: routing.ProviderOllama,
			Endpoint: "http://srv1.local:11434", Status: routing.ServerStatusActive,
			HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateAIServer(ctx, srv); err != nil {
			t.Fatalf("create server: %v", err)
		}

		app := routing.Application{
			ID: "app1", ServerID: "srv1", Type: "ollama", Port: 11434, Scheme: "http",
			APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 1, Weight: 1,
			TimeoutMS: 30000, AffinityTTLSeconds: 300, Status: routing.ServerStatusActive,
			HealthCheckMode: routing.HealthCheckModeAlwaysReachable,
			CreatedAt:       now, UpdatedAt: now,
		}
		if err := s.CreateApplication(ctx, app); err != nil {
			t.Fatalf("create application: %v", err)
		}

		mapping := routing.ModelMapping{
			ID: "m1", ApplicationID: "app1", GatewayModelName: "gpt-4o-mini",
			AppModelName: "up", Status: routing.ServerStatusActive,
			MetricsLocked: false,
			CreatedAt:     now, UpdatedAt: now,
		}
		if err := s.CreateMapping(ctx, mapping); err != nil {
			t.Fatalf("create mapping: %v", err)
		}

		// A capacity write on an unlocked mapping stamps the metrics + provenance.
		capAt := time.Date(2026, 7, 24, 13, 0, 0, 0, time.UTC)
		if err := s.UpdateMappingCapacityMetrics(ctx, "m1", 8, 5, 42.5, capAt); err != nil {
			t.Fatalf("update mapping capacity metrics: %v", err)
		}
		got, err := s.MappingByID(ctx, "m1")
		if err != nil {
			t.Fatalf("mapping by id: %v", err)
		}
		if got.MaxConcurrency != 8 {
			t.Fatalf("MaxConcurrency = %v, want 8", got.MaxConcurrency)
		}
		if got.RecommendedConcurrency != 5 {
			t.Fatalf("RecommendedConcurrency = %v, want 5", got.RecommendedConcurrency)
		}
		if diff := got.GenTokensPerSecondAtCapacity - 42.5; diff < -tol || diff > tol {
			t.Fatalf("GenTokensPerSecondAtCapacity = %v, want 42.5", got.GenTokensPerSecondAtCapacity)
		}
		if got.MetricsSource != "capacity" {
			t.Fatalf("MetricsSource = %q, want %q", got.MetricsSource, "capacity")
		}
		if got.MetricsUpdatedAt == nil || !got.MetricsUpdatedAt.Equal(capAt) {
			t.Fatalf("MetricsUpdatedAt = %v, want %v", got.MetricsUpdatedAt, capAt)
		}

		// Lock the mapping (manual pin) via UpdateMapping.
		mapping.MaxConcurrency = 1
		mapping.RecommendedConcurrency = 1
		mapping.GenTokensPerSecondAtCapacity = 1
		mapping.MetricsLocked = true
		mapping.MetricsSource = "manual"
		mapping.UpdatedAt = now.Add(time.Minute)
		if err := s.UpdateMapping(ctx, mapping); err != nil {
			t.Fatalf("update mapping (lock): %v", err)
		}

		// A capacity write on a LOCKED mapping is a no-op (no error, no change).
		laterAt := time.Date(2026, 7, 24, 14, 0, 0, 0, time.UTC)
		if err := s.UpdateMappingCapacityMetrics(ctx, "m1", 999, 999, 999, laterAt); err != nil {
			t.Fatalf("update mapping capacity metrics (locked): %v", err)
		}
		locked, err := s.MappingByID(ctx, "m1")
		if err != nil {
			t.Fatalf("mapping by id (locked): %v", err)
		}
		if locked.MaxConcurrency != 1 {
			t.Fatalf("locked MaxConcurrency = %v, want 1 (capacity must not overwrite a lock)", locked.MaxConcurrency)
		}
		if locked.MetricsSource != "manual" {
			t.Fatalf("locked MetricsSource = %q, want %q (capacity must not overwrite a lock)", locked.MetricsSource, "manual")
		}

		// A capacity write on a MISSING mapping is a benign no-op.
		if err := s.UpdateMappingCapacityMetrics(ctx, "does-not-exist", 42, 42, 42, laterAt); err != nil {
			t.Fatalf("update mapping capacity metrics (missing) = %v, want nil (benign no-op)", err)
		}
	})
}

// TestConformanceUpdateMappingVisionCapable verifies the vision-capability write
// path: it stamps vision_capable + provenance ("vision") only while the mapping
// is unlocked (a definitive "not capable" result can also be written), and is a
// benign no-op when the mapping is locked or missing, on both dialects.
func TestConformanceUpdateMappingVisionCapable(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		srv := routing.AIServer{
			ID: "srv1", Name: "Server 1", Domain: "srv1.local", Provider: routing.ProviderOllama,
			Endpoint: "http://srv1.local:11434", Status: routing.ServerStatusActive,
			HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateAIServer(ctx, srv); err != nil {
			t.Fatalf("create server: %v", err)
		}

		app := routing.Application{
			ID: "app1", ServerID: "srv1", Type: "ollama", Port: 11434, Scheme: "http",
			APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 1, Weight: 1,
			TimeoutMS: 30000, AffinityTTLSeconds: 300, Status: routing.ServerStatusActive,
			HealthCheckMode: routing.HealthCheckModeAlwaysReachable,
			CreatedAt:       now, UpdatedAt: now,
		}
		if err := s.CreateApplication(ctx, app); err != nil {
			t.Fatalf("create application: %v", err)
		}

		mapping := routing.ModelMapping{
			ID: "map1", ApplicationID: "app1", GatewayModelName: "gpt-4o-mini",
			AppModelName: "up", Status: routing.ServerStatusActive,
			VisionCapable: false, MetricsLocked: false,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateMapping(ctx, mapping); err != nil {
			t.Fatalf("create mapping: %v", err)
		}

		// Default (never probed) is not vision-capable.
		got, err := s.MappingByID(ctx, "map1")
		if err != nil {
			t.Fatalf("mapping by id: %v", err)
		}
		if got.VisionCapable {
			t.Fatalf("VisionCapable = %v, want false (default)", got.VisionCapable)
		}

		// A write on an unlocked mapping stamps vision_capable + provenance.
		visionAt := time.Date(2026, 8, 5, 13, 0, 0, 0, time.UTC)
		if err := s.UpdateMappingVisionCapable(ctx, "map1", true, visionAt); err != nil {
			t.Fatalf("update mapping vision capable: %v", err)
		}
		got, err = s.MappingByID(ctx, "map1")
		if err != nil {
			t.Fatalf("mapping by id: %v", err)
		}
		if !got.VisionCapable {
			t.Fatalf("VisionCapable = %v, want true", got.VisionCapable)
		}
		if got.MetricsSource != "vision" {
			t.Fatalf("MetricsSource = %q, want %q", got.MetricsSource, "vision")
		}
		if got.MetricsUpdatedAt == nil || !got.MetricsUpdatedAt.Equal(visionAt) {
			t.Fatalf("MetricsUpdatedAt = %v, want %v", got.MetricsUpdatedAt, visionAt)
		}

		// A definitive "not capable" result can also be written.
		notCapableAt := time.Date(2026, 8, 5, 13, 30, 0, 0, time.UTC)
		if err := s.UpdateMappingVisionCapable(ctx, "map1", false, notCapableAt); err != nil {
			t.Fatalf("update mapping vision capable (false): %v", err)
		}
		got, err = s.MappingByID(ctx, "map1")
		if err != nil {
			t.Fatalf("mapping by id: %v", err)
		}
		if got.VisionCapable {
			t.Fatalf("VisionCapable = %v, want false (definitive not-capable write)", got.VisionCapable)
		}

		// Lock the mapping (manual pin) via UpdateMapping.
		mapping.VisionCapable = true
		mapping.MetricsLocked = true
		mapping.MetricsSource = "manual"
		mapping.UpdatedAt = now.Add(time.Minute)
		if err := s.UpdateMapping(ctx, mapping); err != nil {
			t.Fatalf("update mapping (lock): %v", err)
		}

		// A write on a LOCKED mapping is a no-op (no error, no change).
		laterAt := time.Date(2026, 8, 5, 14, 0, 0, 0, time.UTC)
		if err := s.UpdateMappingVisionCapable(ctx, "map1", false, laterAt); err != nil {
			t.Fatalf("update mapping vision capable (locked): %v", err)
		}
		locked, err := s.MappingByID(ctx, "map1")
		if err != nil {
			t.Fatalf("mapping by id (locked): %v", err)
		}
		if !locked.VisionCapable {
			t.Fatalf("locked VisionCapable = %v, want true (vision write must not overwrite a lock)", locked.VisionCapable)
		}
		if locked.MetricsSource != "manual" {
			t.Fatalf("locked MetricsSource = %q, want %q (vision write must not overwrite a lock)", locked.MetricsSource, "manual")
		}

		// A write on a MISSING mapping is a benign no-op.
		if err := s.UpdateMappingVisionCapable(ctx, "does-not-exist", true, laterAt); err != nil {
			t.Fatalf("update mapping vision capable (missing) = %v, want nil (benign no-op)", err)
		}
	})
}

// --- Model groups (migration v22) ------------------------------------------

func TestConformanceModelGroups(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		grp := routing.ModelGroup{
			ID: "grp1", GatewayModelName: "fast-coder", DisplayName: "Fast Coder",
			Status: routing.ServerStatusActive, FailoverMode: "sticky", Traversal: "breadth",
			CreatedAt: now, UpdatedAt: now,
			// Non-default values must round-trip through every driver.
			LoadedOnly: true, MemberOrder: routing.MemberOrderSpeed,
			ClimbSpeedMarginPercent: 35, MinTokensPerSecond: 12.5,
			MinSpeedFallback: routing.MinSpeedFallbackIgnore,
		}
		if err := s.CreateModelGroup(ctx, grp); err != nil {
			t.Fatalf("create model group: %v", err)
		}

		// Duplicate id -> ErrConflict.
		if err := s.CreateModelGroup(ctx, grp); err != ErrConflict {
			t.Fatalf("create dup group = %v, want ErrConflict", err)
		}

		got, err := s.ModelGroupByID(ctx, "grp1")
		if err != nil {
			t.Fatalf("group by id: %v", err)
		}
		if got.GatewayModelName != "fast-coder" || got.FailoverMode != "sticky" || got.Status != routing.ServerStatusActive {
			t.Fatalf("unexpected group: %+v", got)
		}
		if got.Traversal != "breadth" {
			t.Fatalf("unexpected traversal on create: %+v", got)
		}
		if !got.LoadedOnly || got.MemberOrder != routing.MemberOrderSpeed ||
			got.ClimbSpeedMarginPercent != 35 || got.MinTokensPerSecond != 12.5 ||
			got.MinSpeedFallback != routing.MinSpeedFallbackIgnore {
			t.Fatalf("group settings did not round-trip: %+v", got)
		}

		// Set members with out-of-order priorities [2,0,1]; read must be priority-ordered.
		members := []routing.GroupMember{
			{GroupID: "grp1", MemberGatewayName: "model-c", Priority: 2, CreatedAt: now},
			{GroupID: "grp1", MemberGatewayName: "model-a", Priority: 0, CreatedAt: now},
			{GroupID: "grp1", MemberGatewayName: "model-b", Priority: 1, CreatedAt: now},
		}
		if err := s.SetGroupMembers(ctx, "grp1", members); err != nil {
			t.Fatalf("set group members: %v", err)
		}
		ordered, err := s.GroupMembersByGroup(ctx, "grp1")
		if err != nil {
			t.Fatalf("group members: %v", err)
		}
		if len(ordered) != 3 {
			t.Fatalf("expected 3 members, got %d: %+v", len(ordered), ordered)
		}
		wantOrder := []string{"model-a", "model-b", "model-c"}
		for i, m := range ordered {
			if m.MemberGatewayName != wantOrder[i] {
				t.Fatalf("member[%d] = %q, want %q (priority order): %+v", i, m.MemberGatewayName, wantOrder[i], ordered)
			}
			if m.ID == "" {
				t.Fatalf("member[%d] has empty generated id: %+v", i, m)
			}
		}

		// SetGroupMembers REPLACES: a new set fully replaces the old members.
		if err := s.SetGroupMembers(ctx, "grp1", []routing.GroupMember{
			{GroupID: "grp1", MemberGatewayName: "only", Priority: 0, CreatedAt: now},
		}); err != nil {
			t.Fatalf("replace group members: %v", err)
		}
		replaced, err := s.GroupMembersByGroup(ctx, "grp1")
		if err != nil {
			t.Fatalf("group members after replace: %v", err)
		}
		if len(replaced) != 1 || replaced[0].MemberGatewayName != "only" {
			t.Fatalf("expected only [only] after replace, got %+v", replaced)
		}

		// A duplicate member_gateway_name within one set -> ErrConflict.
		if err := s.SetGroupMembers(ctx, "grp1", []routing.GroupMember{
			{GroupID: "grp1", MemberGatewayName: "dup", Priority: 0, CreatedAt: now},
			{GroupID: "grp1", MemberGatewayName: "dup", Priority: 1, CreatedAt: now},
		}); err != ErrConflict {
			t.Fatalf("set duplicate members = %v, want ErrConflict", err)
		}

		// SetGroupMembers on an unknown group -> ErrNotFound (even for an empty set).
		if err := s.SetGroupMembers(ctx, "no-such-group", nil); err != ErrNotFound {
			t.Fatalf("set members on missing group = %v, want ErrNotFound", err)
		}

		// Update the group + list.
		updated := got
		updated.DisplayName = "Fast Coder v2"
		updated.FailoverMode = "climb_up"
		updated.Status = routing.ServerStatusDisabled
		updated.Traversal = "depth"
		updated.UpdatedAt = now.Add(time.Hour)
		if err := s.UpdateModelGroup(ctx, updated); err != nil {
			t.Fatalf("update model group: %v", err)
		}
		reread, err := s.ModelGroupByID(ctx, "grp1")
		if err != nil {
			t.Fatalf("group by id after update: %v", err)
		}
		if reread.DisplayName != "Fast Coder v2" || reread.FailoverMode != "climb_up" || reread.Status != routing.ServerStatusDisabled {
			t.Fatalf("update not applied: %+v", reread)
		}
		if reread.Traversal != "depth" {
			t.Fatalf("update not applied to traversal: %+v", reread)
		}
		// UpdateModelGroup left the five settings fields untouched (updated was
		// copied from got, which already carried the non-default values).
		if !reread.LoadedOnly || reread.MemberOrder != routing.MemberOrderSpeed ||
			reread.ClimbSpeedMarginPercent != 35 || reread.MinTokensPerSecond != 12.5 ||
			reread.MinSpeedFallback != routing.MinSpeedFallbackIgnore {
			t.Fatalf("group settings did not survive update round-trip: %+v", reread)
		}

		// climb_speed_margin_percent is never store-defaulted (unlike the other
		// four fields): 0 is a valid, meaningful margin ("no margin required, any
		// faster candidate wins"), distinct from the documented default of 20
		// that the API layer supplies when a caller omits the field. An explicit
		// 0 must survive UpdateModelGroup, not get silently bumped to 20.
		zeroMargin := reread
		zeroMargin.ClimbSpeedMarginPercent = 0
		zeroMargin.UpdatedAt = now.Add(2 * time.Hour)
		if err := s.UpdateModelGroup(ctx, zeroMargin); err != nil {
			t.Fatalf("update model group climb margin to zero: %v", err)
		}
		rereadZero, err := s.ModelGroupByID(ctx, "grp1")
		if err != nil {
			t.Fatalf("group by id after zero-margin update: %v", err)
		}
		if rereadZero.ClimbSpeedMarginPercent != 0 {
			t.Fatalf("update did not preserve explicit zero climb margin: %+v", rereadZero)
		}
		if !rereadZero.LoadedOnly || rereadZero.MemberOrder != routing.MemberOrderSpeed ||
			rereadZero.MinTokensPerSecond != 12.5 || rereadZero.MinSpeedFallback != routing.MinSpeedFallbackIgnore {
			t.Fatalf("unrelated settings fields changed by zero-margin update: %+v", rereadZero)
		}

		// A second group + ModelGroups list ordering. ClimbSpeedMarginPercent is
		// set explicitly to 0 (its Go zero value) to pin that Create persists it
		// literally rather than store-defaulting it to 20.
		if err := s.CreateModelGroup(ctx, routing.ModelGroup{
			ID: "grp2", GatewayModelName: "aaa-first", DisplayName: "Alpha",
			Status: routing.ServerStatusActive, FailoverMode: "sticky", CreatedAt: now, UpdatedAt: now,
			ClimbSpeedMarginPercent: 0,
		}); err != nil {
			t.Fatalf("create group 2: %v", err)
		}
		all, err := s.ModelGroups(ctx)
		if err != nil {
			t.Fatalf("list groups: %v", err)
		}
		if len(all) != 2 || all[0].ID != "grp2" || all[1].ID != "grp1" {
			// ordered by gateway_model_name: "aaa-first" < "fast-coder".
			t.Fatalf("unexpected group list order: %+v", all)
		}
		// grp2 was created without LoadedOnly/MemberOrder/MinTokensPerSecond/
		// MinSpeedFallback; those four must read back the documented defaults
		// (reproducing pre-feature behavior exactly). ClimbSpeedMarginPercent's
		// explicit 0 must read back as 0, NOT the documented default of 20 —
		// the store never substitutes it (0 is a valid margin in its own
		// right; the API layer, not the store, supplies 20 for an omitted
		// field).
		if all[0].LoadedOnly || all[0].MemberOrder != routing.MemberOrderPriority ||
			all[0].ClimbSpeedMarginPercent != 0 ||
			all[0].MinTokensPerSecond != 0 || all[0].MinSpeedFallback != routing.MinSpeedFallbackError {
			t.Fatalf("group settings defaults not applied: %+v", all[0])
		}

		// UpdateModelGroup on a missing id -> ErrNotFound.
		if err := s.UpdateModelGroup(ctx, routing.ModelGroup{ID: "nope", GatewayModelName: "x", Status: routing.ServerStatusActive, UpdatedAt: now}); err != ErrNotFound {
			t.Fatalf("update missing group = %v, want ErrNotFound", err)
		}

		// DeleteModelGroup cascades its members.
		if err := s.DeleteModelGroup(ctx, "grp1"); err != nil {
			t.Fatalf("delete group: %v", err)
		}
		cascaded, err := s.GroupMembersByGroup(ctx, "grp1")
		if err != nil {
			t.Fatalf("group members after delete: %v", err)
		}
		if len(cascaded) != 0 {
			t.Fatalf("expected members cascaded on group delete, got %+v", cascaded)
		}
		if _, err := s.ModelGroupByID(ctx, "grp1"); err != ErrNotFound {
			t.Fatalf("group by id after delete = %v, want ErrNotFound", err)
		}
		// Deleting a missing group -> ErrNotFound.
		if err := s.DeleteModelGroup(ctx, "grp1"); err != ErrNotFound {
			t.Fatalf("delete missing group = %v, want ErrNotFound", err)
		}
	})
}

func TestConformanceModelSettings(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		// Missing -> (_, false, nil).
		if _, ok, err := s.ModelSettingByName(ctx, "model-x"); err != nil || ok {
			t.Fatalf("missing setting = ok:%v err:%v, want (false,nil)", ok, err)
		}

		// Upsert creates.
		if err := s.UpsertModelSetting(ctx, routing.ModelSetting{
			GatewayModelName: "model-x", Visibility: "hidden", CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("upsert (create): %v", err)
		}
		got, ok, err := s.ModelSettingByName(ctx, "model-x")
		if err != nil || !ok {
			t.Fatalf("setting by name after create: ok=%v err=%v", ok, err)
		}
		if got.Visibility != "hidden" {
			t.Fatalf("visibility = %q, want hidden", got.Visibility)
		}

		// Upsert updates the existing row (by name).
		if err := s.UpsertModelSetting(ctx, routing.ModelSetting{
			GatewayModelName: "model-x", Visibility: "locked", CreatedAt: now, UpdatedAt: now.Add(time.Hour),
		}); err != nil {
			t.Fatalf("upsert (update): %v", err)
		}
		reread, ok, err := s.ModelSettingByName(ctx, "model-x")
		if err != nil || !ok || reread.Visibility != "locked" {
			t.Fatalf("visibility after update = %q ok=%v err=%v, want locked", reread.Visibility, ok, err)
		}

		// A second setting + list (ordered by name).
		if err := s.UpsertModelSetting(ctx, routing.ModelSetting{
			GatewayModelName: "aaa-model", Visibility: "shown", CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("upsert second: %v", err)
		}
		all, err := s.ModelSettings(ctx)
		if err != nil {
			t.Fatalf("list settings: %v", err)
		}
		if len(all) != 2 || all[0].GatewayModelName != "aaa-model" || all[1].GatewayModelName != "model-x" {
			t.Fatalf("unexpected settings list: %+v", all)
		}
	})
}

// TestConformanceAffinityResolvedModel round-trips the route_affinity.resolved_model
// column (migration v22) through Upsert/Affinity on both dialects.
func TestConformanceAffinityResolvedModel(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		if err := s.CreateUser(ctx, newTestUser("u1", "aff@example.test", now)); err != nil {
			t.Fatalf("create user: %v", err)
		}
		if err := s.CreatePlainToken(ctx, TokenRecord{ID: "tok1", UserID: "u1", Name: "t", CreatedAt: now, UpdatedAt: now}, "affinity-secret-value"); err != nil {
			t.Fatalf("create token: %v", err)
		}
		if err := s.CreateAIServer(ctx, routing.AIServer{
			ID: "srv1", Name: "S", Domain: "srv1.local", Provider: routing.ProviderOllama,
			Endpoint: "http://srv1.local:11434", Status: routing.ServerStatusActive,
			HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create server: %v", err)
		}
		if err := s.CreateApplication(ctx, routing.Application{
			ID: "app1", ServerID: "srv1", Type: "ollama", Port: 11434, Scheme: "http",
			APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 1, Weight: 1,
			TimeoutMS: 30000, AffinityTTLSeconds: 300, Status: routing.ServerStatusActive,
			HealthCheckMode: routing.HealthCheckModeAlwaysReachable, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create application: %v", err)
		}

		aff := routing.RouteAffinity{
			ID: "aff1", APITokenID: "tok1", UserID: "u1",
			Model: "fast-coder", ResolvedModel: "model-b", APIFlavor: routing.APIFlavorOpenAI,
			SessionID: "sess1", ApplicationID: "app1", ServerID: "srv1",
			ExpiresAt: now.Add(time.Hour), LastUsedAt: now, CreatedAt: now, UpdatedAt: now,
		}
		if err := s.UpsertAffinity(ctx, aff); err != nil {
			t.Fatalf("upsert affinity: %v", err)
		}
		got, ok, err := s.Affinity(ctx, routing.AffinityKey{APITokenID: "tok1", Model: "fast-coder", APIFlavor: routing.APIFlavorOpenAI, SessionID: "sess1"})
		if err != nil || !ok {
			t.Fatalf("read affinity: ok=%v err=%v", ok, err)
		}
		if got.ResolvedModel != "model-b" {
			t.Fatalf("ResolvedModel = %q, want model-b", got.ResolvedModel)
		}

		// Re-pin to a different resolved model (the upsert must update resolved_model).
		aff.ResolvedModel = "model-a"
		aff.UpdatedAt = now.Add(time.Minute)
		if err := s.UpsertAffinity(ctx, aff); err != nil {
			t.Fatalf("re-upsert affinity: %v", err)
		}
		got2, ok, err := s.Affinity(ctx, routing.AffinityKey{APITokenID: "tok1", Model: "fast-coder", APIFlavor: routing.APIFlavorOpenAI, SessionID: "sess1"})
		if err != nil || !ok || got2.ResolvedModel != "model-a" {
			t.Fatalf("ResolvedModel after re-pin = %q ok=%v err=%v, want model-a", got2.ResolvedModel, ok, err)
		}
	})
}

// TestConformanceAffinityServiceTokenUserIDNullable proves migration v42's
// fix for a CRITICAL production bug the Phase-2 e2e discovered: a service
// token has no user (auth.Token.UserID == ""), and the resolver's
// UpsertAffinity always sets RouteAffinity.UserID = token.UserID. Before
// migration v42, route_affinity.user_id was `not null references users(id)
// on delete cascade` — writing "" against that column is checked against
// the referenced table (unlike a genuine SQL NULL, which the FK constraint
// exempts), and users("") never exists, so the insert failed the FK check
// on both sqlite (foreign_keys=ON) and postgres. That made EVERY
// service-token inference request 502 in production (memory-store mode has
// no FK enforcement, so it was invisible there — exactly why the Phase-1
// e2e never caught it).
//
// This test is the RED/GREEN proof at the store layer: run it against a
// pre-fix checkout (route_affinity.user_id still NOT NULL) and
// UpsertAffinity below fails with a foreign-key/not-null violation; against
// the fix it succeeds and round-trips. It also proves a normal user-token
// affinity is entirely unaffected: it round-trips with its user_id intact
// and the row still cascade-deletes when the owning user is deleted (only
// the NOT NULL was dropped — the FK itself, and the lookup key
// unique(api_token_id, model, api_flavor, session_id), are untouched).
func TestConformanceAffinityServiceTokenUserIDNullable(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		if err := s.CreateAIServer(ctx, routing.AIServer{
			ID: "srv_aff42", Name: "S42", Domain: "srv42.local", Provider: routing.ProviderOllama,
			Endpoint: "http://srv42.local:11434", Status: routing.ServerStatusActive,
			HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create server: %v", err)
		}
		if err := s.CreateApplication(ctx, routing.Application{
			ID: "app_aff42", ServerID: "srv_aff42", Type: "ollama", Port: 11434, Scheme: "http",
			APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 1, Weight: 1,
			TimeoutMS: 30000, AffinityTTLSeconds: 300, Status: routing.ServerStatusActive,
			HealthCheckMode: routing.HealthCheckModeAlwaysReachable, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create application: %v", err)
		}

		// --- Service-token affinity: UserID == "" — this is the bug reproduction. ---
		if err := s.CreateService(ctx, routing.Service{ID: "svc_aff42", Name: "Batch", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("create service: %v", err)
		}
		svcTok := TokenRecord{
			ID: "tok_svc_aff42", ServiceID: "svc_aff42", Kind: TokenKindService,
			Name: "batch", Status: TokenStatusActive, Scopes: `["llm:invoke"]`,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreatePlainToken(ctx, svcTok, "svc-aff42-secret"); err != nil {
			t.Fatalf("create service token: %v", err)
		}

		svcAff := routing.RouteAffinity{
			ID: "aff_svc42", APITokenID: "tok_svc_aff42", UserID: "", // <- the bug: a service token has no user.
			Model: "fast-coder", APIFlavor: routing.APIFlavorOpenAI, SessionID: "sess_svc42",
			ApplicationID: "app_aff42", ServerID: "srv_aff42",
			ExpiresAt: now.Add(time.Hour), LastUsedAt: now, CreatedAt: now, UpdatedAt: now,
		}
		if err := s.UpsertAffinity(ctx, svcAff); err != nil {
			t.Fatalf("UpsertAffinity for a service token (UserID=\"\") must succeed, got %v -- "+
				"this is exactly the production 502: an empty user_id against a NOT NULL FK column referencing users(id)", err)
		}
		got, ok, err := s.Affinity(ctx, routing.AffinityKey{APITokenID: "tok_svc_aff42", Model: "fast-coder", APIFlavor: routing.APIFlavorOpenAI, SessionID: "sess_svc42"})
		if err != nil || !ok {
			t.Fatalf("read service-token affinity: ok=%v err=%v", ok, err)
		}
		if got.UserID != "" {
			t.Fatalf("service-token affinity UserID = %q, want empty", got.UserID)
		}
		if got.APITokenID != "tok_svc_aff42" || got.ServerID != "srv_aff42" || got.ApplicationID != "app_aff42" {
			t.Fatalf("service-token affinity round-trip mismatch: %+v", got)
		}

		// Re-upsert (the on-conflict update path) must also tolerate an empty UserID.
		svcAff.UpdatedAt = now.Add(time.Minute)
		svcAff.ServerID = "srv_aff42"
		if err := s.UpsertAffinity(ctx, svcAff); err != nil {
			t.Fatalf("re-upsert service-token affinity: %v", err)
		}

		// --- Normal user-token affinity: unaffected, still cascades on user delete. ---
		if err := s.CreateUser(ctx, newTestUser("u_aff42", "aff42@example.test", now)); err != nil {
			t.Fatalf("create user: %v", err)
		}
		if err := s.CreatePlainToken(ctx, TokenRecord{ID: "tok_user_aff42", UserID: "u_aff42", Name: "u", CreatedAt: now, UpdatedAt: now}, "user-aff42-secret"); err != nil {
			t.Fatalf("create user token: %v", err)
		}
		userAff := routing.RouteAffinity{
			ID: "aff_user42", APITokenID: "tok_user_aff42", UserID: "u_aff42",
			Model: "fast-coder", APIFlavor: routing.APIFlavorOpenAI, SessionID: "sess_user42",
			ApplicationID: "app_aff42", ServerID: "srv_aff42",
			ExpiresAt: now.Add(time.Hour), LastUsedAt: now, CreatedAt: now, UpdatedAt: now,
		}
		if err := s.UpsertAffinity(ctx, userAff); err != nil {
			t.Fatalf("UpsertAffinity for a user token: %v", err)
		}
		gotUser, ok, err := s.Affinity(ctx, routing.AffinityKey{APITokenID: "tok_user_aff42", Model: "fast-coder", APIFlavor: routing.APIFlavorOpenAI, SessionID: "sess_user42"})
		if err != nil || !ok || gotUser.UserID != "u_aff42" {
			t.Fatalf("user-token affinity round-trip: ok=%v err=%v got=%+v", ok, err, gotUser)
		}

		// Deleting the user must still cascade-delete the row (the FK reference
		// to users(id) stays -- only its NOT NULL was dropped).
		if err := deleteUserForTest(ctx, s, "u_aff42"); err != nil {
			t.Fatalf("delete user: %v", err)
		}
		if _, ok, err := s.Affinity(ctx, routing.AffinityKey{APITokenID: "tok_user_aff42", Model: "fast-coder", APIFlavor: routing.APIFlavorOpenAI, SessionID: "sess_user42"}); err != nil || ok {
			t.Fatalf("expected user-token affinity cascade-deleted, ok=%v err=%v", ok, err)
		}

		// The unrelated service-token affinity must survive the user delete.
		if _, ok, err := s.Affinity(ctx, routing.AffinityKey{APITokenID: "tok_svc_aff42", Model: "fast-coder", APIFlavor: routing.APIFlavorOpenAI, SessionID: "sess_svc42"}); err != nil || !ok {
			t.Fatalf("service-token affinity should survive an unrelated user delete: ok=%v err=%v", ok, err)
		}
	})
}

// TestMigration42RouteAffinityNullableUpgrade exercises migration42Up's real
// schema change directly (mirroring TestMigration4UpgradesInt4ToBigint /
// TestConformanceMigration40ServiceAccountsRebuild): it takes a fully
// migrated store, downgrades route_affinity back to the pre-v42 shape
// (user_id NOT NULL — safe, since at that point the table is still empty),
// proves the pre-fix repro (a service-token UpsertAffinity, whose write path
// already converts an empty UserID to a genuine SQL NULL, still fails
// because NOT NULL rejects that NULL outright), applies migration42Up, and
// proves the same call now succeeds on BOTH dialects (sqlite's drop+recreate
// path and postgres's ALTER COLUMN path).
func TestMigration42RouteAffinityNullableUpgrade(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		if err := s.CreateAIServer(ctx, routing.AIServer{
			ID: "srv_m42", Name: "M42", Domain: "m42.local", Provider: routing.ProviderOllama,
			Endpoint: "http://m42.local:11434", Status: routing.ServerStatusActive,
			HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create server: %v", err)
		}
		if err := s.CreateApplication(ctx, routing.Application{
			ID: "app_m42", ServerID: "srv_m42", Type: "ollama", Port: 11434, Scheme: "http",
			APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 1, Weight: 1,
			TimeoutMS: 30000, AffinityTTLSeconds: 300, Status: routing.ServerStatusActive,
			HealthCheckMode: routing.HealthCheckModeAlwaysReachable, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create application: %v", err)
		}
		if err := s.CreateService(ctx, routing.Service{ID: "svc_m42", Name: "M42 svc", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("create service: %v", err)
		}
		svcTok := TokenRecord{
			ID: "tok_svc_m42", ServiceID: "svc_m42", Kind: TokenKindService,
			Name: "batch", Status: TokenStatusActive, Scopes: `["llm:invoke"]`,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreatePlainToken(ctx, svcTok, "svc-m42-secret"); err != nil {
			t.Fatalf("create service token: %v", err)
		}

		// Downgrade route_affinity to the pre-v42 shape. Safe: nothing has been
		// inserted into it yet at this point.
		if s.dl.name() == "sqlite" {
			ts := s.dl.timestampType()
			for _, stmt := range []string{
				`drop table if exists route_affinity`,
				`create table route_affinity (
					id text primary key,
					api_token_id text not null references api_tokens(id) on delete cascade,
					user_id text not null references users(id) on delete cascade,
					model text not null,
					api_flavor text not null,
					session_id text not null,
					application_id text not null references applications(id) on delete cascade,
					server_id text not null references ai_servers(id) on delete cascade,
					expires_at ` + ts + ` not null,
					last_used_at ` + ts + ` not null,
					created_at ` + ts + ` not null,
					updated_at ` + ts + ` not null,
					resolved_model text not null default '',
					unique(api_token_id, model, api_flavor, session_id)
				)`,
				`create index if not exists idx_route_affinity_lookup on route_affinity(api_token_id, model, api_flavor, session_id, expires_at)`,
			} {
				if _, err := s.db.ExecContext(ctx, stmt); err != nil {
					t.Fatalf("downgrade route_affinity (sqlite): %v", err)
				}
			}
		} else {
			if _, err := s.db.ExecContext(ctx, `alter table route_affinity alter column user_id set not null`); err != nil {
				t.Fatalf("downgrade route_affinity (postgres): %v", err)
			}
		}

		svcAff := routing.RouteAffinity{
			ID: "aff_m42", APITokenID: "tok_svc_m42", UserID: "",
			Model: "fast-coder", APIFlavor: routing.APIFlavorOpenAI, SessionID: "sess_m42",
			ApplicationID: "app_m42", ServerID: "srv_m42",
			ExpiresAt: now.Add(time.Hour), LastUsedAt: now, CreatedAt: now, UpdatedAt: now,
		}
		// Pre-v42 shape: the NOT NULL rejects the NULL the write path already
		// produces for an empty UserID (reproduces the production 502).
		if err := s.UpsertAffinity(ctx, svcAff); err == nil {
			t.Fatal("expected UpsertAffinity to fail against the pre-v42 (NOT NULL) shape, got nil")
		}

		// Apply migration42Up's real fix.
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if err := migration42Up(ctx, tx, s.dl); err != nil {
			_ = tx.Rollback()
			t.Fatalf("migration42Up: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}

		// The same call now succeeds and round-trips.
		if err := s.UpsertAffinity(ctx, svcAff); err != nil {
			t.Fatalf("UpsertAffinity after migration42Up: %v", err)
		}
		got, ok, err := s.Affinity(ctx, routing.AffinityKey{APITokenID: "tok_svc_m42", Model: "fast-coder", APIFlavor: routing.APIFlavorOpenAI, SessionID: "sess_m42"})
		if err != nil || !ok || got.UserID != "" {
			t.Fatalf("read affinity after migration42Up: ok=%v err=%v got=%+v", ok, err, got)
		}
	})
}

func TestConformanceTelemetrySamplesPower(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		srv := routing.AIServer{
			ID: "srvp", Name: "P", Domain: "p.local", Provider: routing.ProviderOllama,
			Endpoint: "http://p.local:11434", Status: routing.ServerStatusActive,
			HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateAIServer(ctx, srv); err != nil {
			t.Fatalf("create server: %v", err)
		}

		cpu := 65.5
		sys := 180.0
		fp := func(v float64) *float64 { return &v }
		samples := []routing.TelemetrySample{
			{ServerID: "srvp", ReportedAt: now.Add(0 * time.Minute), CPUPowerW: fp(cpu), SystemPowerW: fp(sys)},
			{ServerID: "srvp", ReportedAt: now.Add(1 * time.Minute), CPUPowerW: fp(cpu), SystemPowerW: nil},
			{ServerID: "srvp", ReportedAt: now.Add(2 * time.Minute), CPUPowerW: nil, SystemPowerW: nil},
		}
		for i, sm := range samples {
			if err := s.InsertTelemetrySample(ctx, sm); err != nil {
				t.Fatalf("insert %d: %v", i, err)
			}
		}

		got, err := s.TelemetrySamples(ctx, "srvp", now, now.Add(10*time.Minute), 100)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("want 3 samples, got %d", len(got))
		}
		// row0: both set.
		if got[0].CPUPowerW == nil || *got[0].CPUPowerW != cpu || got[0].SystemPowerW == nil || *got[0].SystemPowerW != sys {
			t.Fatalf("row0 power round-trip: cpu=%v system=%v", got[0].CPUPowerW, got[0].SystemPowerW)
		}
		// row1: cpu set, system NULL.
		if got[1].CPUPowerW == nil || *got[1].CPUPowerW != cpu {
			t.Fatalf("row1 cpu round-trip: %v", got[1].CPUPowerW)
		}
		if got[1].SystemPowerW != nil {
			t.Fatalf("row1 system should be nil (NULL), got %v", *got[1].SystemPowerW)
		}
		// row2: both NULL.
		if got[2].CPUPowerW != nil || got[2].SystemPowerW != nil {
			t.Fatalf("row2 both should be nil, got cpu=%v system=%v", got[2].CPUPowerW, got[2].SystemPowerW)
		}
	})
}

func TestConformanceTelemetrySamplesTemp(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		srv := routing.AIServer{
			ID: "srvt", Name: "T", Domain: "t.local", Provider: routing.ProviderOllama,
			Endpoint: "http://t.local:11434", Status: routing.ServerStatusActive,
			HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateAIServer(ctx, srv); err != nil {
			t.Fatalf("create server: %v", err)
		}

		temp := 58.5
		fp := func(v float64) *float64 { return &v }
		samples := []routing.TelemetrySample{
			{ServerID: "srvt", ReportedAt: now.Add(0 * time.Minute), CPUTempC: fp(temp)},
			{ServerID: "srvt", ReportedAt: now.Add(1 * time.Minute), CPUTempC: nil},
		}
		for i, sm := range samples {
			if err := s.InsertTelemetrySample(ctx, sm); err != nil {
				t.Fatalf("insert %d: %v", i, err)
			}
		}

		got, err := s.TelemetrySamples(ctx, "srvt", now, now.Add(10*time.Minute), 100)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("want 2 samples, got %d", len(got))
		}
		// row0: set.
		if got[0].CPUTempC == nil || *got[0].CPUTempC != temp {
			t.Fatalf("row0 temp round-trip: %v", got[0].CPUTempC)
		}
		// row1: NULL.
		if got[1].CPUTempC != nil {
			t.Fatalf("row1 temp should be nil, got %v", *got[1].CPUTempC)
		}
	})
}

// --- Phase 1 service accounts (migration v40) -------------------------------

func TestConformanceServicesCRUDDelegatesAndAllowlist(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		for _, id := range []string{"del_full", "del_token"} {
			if err := s.CreateUser(ctx, newTestUser(id, id+"@example.test", now)); err != nil {
				t.Fatalf("create user %s: %v", id, err)
			}
		}

		svc := routing.Service{ID: "svc1", Name: "Nightly Batch", Description: "cron job", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}
		if err := s.CreateService(ctx, svc); err != nil {
			t.Fatalf("create service: %v", err)
		}
		// Duplicate id -> ErrConflict.
		if err := s.CreateService(ctx, svc); err != ErrConflict {
			t.Fatalf("create dup service = %v, want ErrConflict", err)
		}

		got, err := s.ServiceByID(ctx, "svc1")
		if err != nil {
			t.Fatalf("service by id: %v", err)
		}
		if got.Name != "Nightly Batch" || got.Status != routing.ServerStatusActive {
			t.Fatalf("unexpected service: %+v", got)
		}

		// Delegates: a Full-Delegate + a Token-Delegate.
		delegates := []routing.ServiceDelegate{
			{UserID: "del_full", CanManageSettings: true},
			{UserID: "del_token", CanManageSettings: false},
		}
		if err := s.SetServiceDelegates(ctx, "svc1", delegates); err != nil {
			t.Fatalf("set service delegates: %v", err)
		}
		listed, err := s.ServiceDelegates(ctx, "svc1")
		if err != nil {
			t.Fatalf("service delegates: %v", err)
		}
		if len(listed) != 2 {
			t.Fatalf("expected 2 delegates, got %d: %+v", len(listed), listed)
		}
		// Ordered by user_id: "del_full" < "del_token".
		if listed[0].UserID != "del_full" || !listed[0].CanManageSettings {
			t.Fatalf("delegate[0] = %+v, want del_full CanManageSettings=true", listed[0])
		}
		if listed[1].UserID != "del_token" || listed[1].CanManageSettings {
			t.Fatalf("delegate[1] = %+v, want del_token CanManageSettings=false", listed[1])
		}

		// ServicesByDelegate finds it for EITHER stage.
		for _, userID := range []string{"del_full", "del_token"} {
			byDelegate, err := s.ServicesByDelegate(ctx, userID)
			if err != nil || len(byDelegate) != 1 || byDelegate[0].ID != "svc1" {
				t.Fatalf("services by delegate %s: err=%v %+v", userID, err, byDelegate)
			}
		}
		// A non-delegate user sees nothing.
		none, err := s.ServicesByDelegate(ctx, "usr_not_a_delegate")
		if err != nil || len(none) != 0 {
			t.Fatalf("services by non-delegate: err=%v %+v", err, none)
		}

		// SetServiceDelegates REPLACES: a new set fully replaces the old.
		if err := s.SetServiceDelegates(ctx, "svc1", []routing.ServiceDelegate{{UserID: "del_full", CanManageSettings: true}}); err != nil {
			t.Fatalf("replace delegates: %v", err)
		}
		replaced, err := s.ServiceDelegates(ctx, "svc1")
		if err != nil || len(replaced) != 1 || replaced[0].UserID != "del_full" {
			t.Fatalf("delegates after replace: err=%v %+v", err, replaced)
		}

		// A duplicate UserID within one set -> ErrConflict.
		if err := s.SetServiceDelegates(ctx, "svc1", []routing.ServiceDelegate{
			{UserID: "del_full", CanManageSettings: true},
			{UserID: "del_full", CanManageSettings: false},
		}); err != ErrConflict {
			t.Fatalf("set duplicate delegates = %v, want ErrConflict", err)
		}

		// SetServiceDelegates on an unknown service -> ErrNotFound (even for an empty set).
		if err := s.SetServiceDelegates(ctx, "no-such-service", nil); err != ErrNotFound {
			t.Fatalf("set delegates on missing service = %v, want ErrNotFound", err)
		}

		// Model allowlist: empty by default.
		emptyAllow, err := s.ServiceAllowedModels(ctx, "svc1")
		if err != nil || len(emptyAllow) != 0 {
			t.Fatalf("initial allowlist: err=%v %+v, want empty", err, emptyAllow)
		}
		if err := s.SetServiceAllowedModels(ctx, "svc1", []string{"llama3", "gpt-4o"}); err != nil {
			t.Fatalf("set allowed models: %v", err)
		}
		allowed, err := s.ServiceAllowedModels(ctx, "svc1")
		if err != nil {
			t.Fatalf("service allowed models: %v", err)
		}
		// Ordered by gateway_model_name: "gpt-4o" < "llama3".
		if len(allowed) != 2 || allowed[0] != "gpt-4o" || allowed[1] != "llama3" {
			t.Fatalf("unexpected allowlist: %+v", allowed)
		}
		// A duplicate model name within one set -> ErrConflict.
		if err := s.SetServiceAllowedModels(ctx, "svc1", []string{"dup", "dup"}); err != ErrConflict {
			t.Fatalf("set duplicate allowed models = %v, want ErrConflict", err)
		}
		// SetServiceAllowedModels on an unknown service -> ErrNotFound (even for an empty set).
		if err := s.SetServiceAllowedModels(ctx, "no-such-service", nil); err != ErrNotFound {
			t.Fatalf("set allowed models on missing service = %v, want ErrNotFound", err)
		}
		// Re-set to empty means "all models allowed" again.
		if err := s.SetServiceAllowedModels(ctx, "svc1", nil); err != nil {
			t.Fatalf("clear allowed models: %v", err)
		}
		cleared, err := s.ServiceAllowedModels(ctx, "svc1")
		if err != nil || len(cleared) != 0 {
			t.Fatalf("allowlist after clear: err=%v %+v, want empty", err, cleared)
		}

		// Update + a second service + Services() list ordering.
		updated := got
		updated.Name = "Nightly Batch v2"
		updated.Description = "renamed"
		updated.Status = routing.ServerStatusDisabled
		updated.UpdatedAt = now.Add(time.Hour)
		if err := s.UpdateService(ctx, updated); err != nil {
			t.Fatalf("update service: %v", err)
		}
		reread, err := s.ServiceByID(ctx, "svc1")
		if err != nil {
			t.Fatalf("service by id after update: %v", err)
		}
		if reread.Name != "Nightly Batch v2" || reread.Status != routing.ServerStatusDisabled {
			t.Fatalf("update not applied: %+v", reread)
		}
		if reread.CreatedAt.Unix() != got.CreatedAt.Unix() {
			t.Fatalf("UpdateService must not touch created_at: got %v, want %v", reread.CreatedAt, got.CreatedAt)
		}

		if err := s.CreateService(ctx, routing.Service{ID: "svc0", Name: "Alpha", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("create service 2: %v", err)
		}
		all, err := s.Services(ctx)
		if err != nil {
			t.Fatalf("list services: %v", err)
		}
		if len(all) != 2 || all[0].ID != "svc0" || all[1].ID != "svc1" {
			t.Fatalf("unexpected service list order (want id-ordered): %+v", all)
		}

		// UpdateService on a missing id -> ErrNotFound.
		if err := s.UpdateService(ctx, routing.Service{ID: "nope", Name: "x", UpdatedAt: now}); err != ErrNotFound {
			t.Fatalf("update missing service = %v, want ErrNotFound", err)
		}

		// DeleteService cascades delegates + allowlist.
		if err := s.SetServiceAllowedModels(ctx, "svc1", []string{"llama3"}); err != nil {
			t.Fatalf("re-set allowed models: %v", err)
		}
		if err := s.DeleteService(ctx, "svc1"); err != nil {
			t.Fatalf("delete service: %v", err)
		}
		if _, err := s.ServiceByID(ctx, "svc1"); err != ErrNotFound {
			t.Fatalf("service by id after delete = %v, want ErrNotFound", err)
		}
		cascadedDelegates, err := s.ServiceDelegates(ctx, "svc1")
		if err != nil || len(cascadedDelegates) != 0 {
			t.Fatalf("delegates after delete: err=%v %+v, want empty (cascaded)", err, cascadedDelegates)
		}
		cascadedAllow, err := s.ServiceAllowedModels(ctx, "svc1")
		if err != nil || len(cascadedAllow) != 0 {
			t.Fatalf("allowlist after delete: err=%v %+v, want empty (cascaded)", err, cascadedAllow)
		}
		// Deleting a missing service -> ErrNotFound.
		if err := s.DeleteService(ctx, "svc1"); err != ErrNotFound {
			t.Fatalf("delete missing service = %v, want ErrNotFound", err)
		}
	})
}

// TestConformanceServiceTokenInsertAndDisabledGate proves, on BOTH dialects,
// that a service token is inserted with a genuine SQL NULL user_id (the FK
// constraint would reject an empty-string value against the nonexistent ""
// user), and that LookupBearer rejects every token of a disabled service.
func TestConformanceServiceTokenInsertAndDisabledGate(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		if err := s.CreateService(ctx, routing.Service{ID: "svc_tok", Name: "Service", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("create service: %v", err)
		}
		rec := TokenRecord{
			ID: "tok_svc_cf", ServiceID: "svc_tok", Kind: TokenKindService,
			Name: "batch", Status: TokenStatusActive, Scopes: `["llm:invoke"]`,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreatePlainToken(ctx, rec, "svc-conformance-secret"); err != nil {
			t.Fatalf("create service token: %v", err)
		}

		got, err := s.TokenByID(ctx, "tok_svc_cf")
		if err != nil {
			t.Fatalf("token by id: %v", err)
		}
		if got.UserID != "" || got.ServiceID != "svc_tok" || got.Kind != TokenKindService {
			t.Fatalf("token by id = %+v, want UserID='' ServiceID=svc_tok Kind=service", got)
		}

		byService, err := s.TokensByService(ctx, "svc_tok")
		if err != nil || len(byService) != 1 || byService[0].ID != "tok_svc_cf" {
			t.Fatalf("tokens by service: err=%v %+v", err, byService)
		}

		tok, ok := s.LookupBearer("Bearer svc-conformance-secret")
		if !ok || !tok.IsService() || tok.ServiceID != "svc_tok" || tok.ServiceName != "Service" {
			t.Fatalf("lookup bearer (active service): ok=%v tok=%+v", ok, tok)
		}

		// Disable the service: every one of its tokens is rejected immediately.
		disabled := routing.Service{ID: "svc_tok", Name: "Service", Status: routing.ServerStatusDisabled, CreatedAt: now, UpdatedAt: now.Add(time.Minute)}
		if err := s.UpdateService(ctx, disabled); err != nil {
			t.Fatalf("disable service: %v", err)
		}
		if _, ok := s.LookupBearer("Bearer svc-conformance-secret"); ok {
			t.Fatal("lookup bearer resolved a token of a disabled service, want rejected")
		}
	})
}

// TestConformanceMigration40ServiceAccountsRebuild proves migration v40's
// SQLite api_tokens rebuild is lossless AND does not cascade-wipe a
// route_affinity row that references the rebuilt token.
//
// NOTE on how the pre-v40 shape is constructed: baselineUp (migration 1) was
// itself edited to build the FINAL (v40) api_tokens shape directly, so a
// database that ran the CURRENT migration list end-to-end (as forEachDialect
// always does) never actually has an old-shape api_tokens table to upgrade —
// migration40Up's own `hasKind` guard would just skip the rebuild as a no-op.
// To exercise the real rebuild path, this test takes a fully-migrated store
// (so services/route_affinity/etc. all exist) and DOWNGRADES api_tokens back
// to the pre-v40 shape itself — safely, since at that point api_tokens is
// still empty (nothing to cascade-wipe) — inserts the fixture token + a
// route_affinity row referencing it, and only THEN calls the real
// migration40RawUp directly (mirroring how TestMigration4UpgradesInt4ToBigint
// exercises migration4Up: revert a narrow slice of schema, call the specific
// migration function, verify the fix).
func TestConformanceMigration40ServiceAccountsRebuild(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		if s.dl.name() != "sqlite" {
			t.Skip("sqlite-only: this pins the SQLite table-rebuild path specifically; postgres never rebuilds (a plain ALTER COLUMN suffices, no cascade risk)")
		}
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		if err := s.CreateUser(ctx, newTestUser("u_pre40", "pre40@example.test", now)); err != nil {
			t.Fatalf("create user: %v", err)
		}

		// Downgrade api_tokens to the pre-v40 shape (user_id NOT NULL, no
		// service_id/kind). Safe: api_tokens has zero rows at this point, so the
		// DROP has nothing to cascade onto yet.
		for _, stmt := range []string{
			`create table api_tokens_downgrade (
				id text primary key,
				user_id text not null references users(id) on delete cascade,
				name text not null,
				secret_hash text not null unique,
				secret_prefix text not null,
				status text not null,
				scopes text not null,
				expires_at timestamp,
				last_used_at timestamp,
				created_at timestamp not null,
				updated_at timestamp not null,
				model_override text not null default '',
				model_override_map text not null default '',
				log_communication integer not null default 0,
				secret integer not null default 0
			)`,
			`drop table api_tokens`,
			`alter table api_tokens_downgrade rename to api_tokens`,
			`create index idx_api_tokens_hash on api_tokens(secret_hash)`,
		} {
			if _, err := s.db.ExecContext(ctx, stmt); err != nil {
				t.Fatalf("downgrade api_tokens (%q): %v", stmt, err)
			}
		}
		var hasKindBeforeDowngrade int
		_ = s.db.QueryRow(`select count(*) from pragma_table_info('api_tokens') where name='kind'`).Scan(&hasKindBeforeDowngrade)
		if hasKindBeforeDowngrade != 0 {
			t.Fatalf("downgrade did not remove the kind column (test setup bug)")
		}

		// Insert the pre-v40 fixture token via the now-old-shape table (raw SQL —
		// CreatePlainToken's Go code targets the NEW schema).
		if _, err := s.exec(ctx, `insert into api_tokens (
			id, user_id, name, secret_hash, secret_prefix, status, scopes,
			expires_at, last_used_at, created_at, updated_at, model_override,
			model_override_map, log_communication, secret
		) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			"tok_pre40", "u_pre40", "Pre-v40 Token", auth.HashSecret("pre40-secret"), "pre40sec",
			TokenStatusActive, `["gateway:use"]`, nil, nil, now, now, "override-model", `{"a":"b"}`, true, true,
		); err != nil {
			t.Fatalf("raw pre-v40 token insert: %v", err)
		}

		// A route_affinity row referencing that token (on delete cascade) — the
		// FK-cascade-on-DROP hazard migration40's rebuild must not trigger.
		if err := s.CreateAIServer(ctx, routing.AIServer{
			ID: "srv_pre40", Name: "S", Domain: "srv-pre40.local", Provider: routing.ProviderOllama,
			Endpoint: "http://srv-pre40.local:11434", Status: routing.ServerStatusActive,
			HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create server: %v", err)
		}
		if err := s.CreateApplication(ctx, routing.Application{
			ID: "app_pre40", ServerID: "srv_pre40", Type: "ollama", Port: 11434, Scheme: "http",
			APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 1, Weight: 1,
			TimeoutMS: 30000, AffinityTTLSeconds: 300, Status: routing.ServerStatusActive,
			HealthCheckMode: routing.HealthCheckModeAlwaysReachable, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create application: %v", err)
		}
		aff := routing.RouteAffinity{
			ID: "aff_pre40", APITokenID: "tok_pre40", UserID: "u_pre40",
			Model: "fast-coder", APIFlavor: routing.APIFlavorOpenAI, SessionID: "sess1",
			ApplicationID: "app_pre40", ServerID: "srv_pre40",
			ExpiresAt: now.Add(time.Hour), LastUsedAt: now, CreatedAt: now, UpdatedAt: now,
		}
		if err := s.UpsertAffinity(ctx, aff); err != nil {
			t.Fatalf("upsert affinity: %v", err)
		}
		var affBefore int
		if err := s.db.QueryRow(`select count(*) from route_affinity where api_token_id = ?`, "tok_pre40").Scan(&affBefore); err != nil || affBefore != 1 {
			t.Fatalf("affinity fixture not present before migration: count=%d err=%v", affBefore, err)
		}

		// forEachDialect's initial Migrate() already recorded schema_migrations
		// version 40 (it ran the full, current migration list before handing
		// back the store) — clear that bookkeeping row so the direct rawUp call
		// below can insert its own without hitting the primary-key conflict.
		if _, err := s.db.ExecContext(ctx, `delete from schema_migrations where version = 40`); err != nil {
			t.Fatalf("clear schema_migrations for re-run: %v", err)
		}

		// Run the REAL migration40RawUp directly against this downgraded shape
		// (bypassing schema_migrations bookkeeping/version-skip logic, mirroring
		// TestMigration4UpgradesInt4ToBigint's direct migration4Up call).
		if err := migration40RawUp(ctx, s, 40, "service_accounts"); err != nil {
			t.Fatalf("migration40RawUp: %v", err)
		}

		// This test exercises ONLY migration40RawUp's rebuild in isolation (it
		// re-derives the exact pre-v45 table shape), but the current Go store
		// code (TokenByID/scanToken) always assumes the FULL v1..v56 schema. A
		// real upgrade path runs every migration in order, so v45's additive
		// api_tokens.project_id AND v56's server_override/
		// server_override_force_unreachable would already be present by the
		// time anything calls TokenByID again — reapply them here to match that
		// reality before exercising the current code below (mirrors
		// migration45Up's + migration56Up's ALTERs).
		if _, err := s.db.ExecContext(ctx, `alter table api_tokens add column project_id text references projects(id) on delete set null`); err != nil {
			t.Fatalf("reapply api_tokens.project_id after migration40RawUp: %v", err)
		}
		if _, err := s.db.ExecContext(ctx, `alter table api_tokens add column server_override text not null default ''`); err != nil {
			t.Fatalf("reapply api_tokens.server_override after migration40RawUp: %v", err)
		}
		if _, err := s.db.ExecContext(ctx, `alter table api_tokens add column server_override_force_unreachable integer not null default 0`); err != nil {
			t.Fatalf("reapply api_tokens.server_override_force_unreachable after migration40RawUp: %v", err)
		}
		// ...and v63's last-used-model marker + unknown-model redirect settings.
		if _, err := s.db.ExecContext(ctx, `alter table api_tokens add column last_used_model text not null default ''`); err != nil {
			t.Fatalf("reapply api_tokens.last_used_model after migration40RawUp: %v", err)
		}
		if _, err := s.db.ExecContext(ctx, `alter table api_tokens add column unknown_model_redirect integer not null default 0`); err != nil {
			t.Fatalf("reapply api_tokens.unknown_model_redirect after migration40RawUp: %v", err)
		}
		if _, err := s.db.ExecContext(ctx, `alter table api_tokens add column unknown_model_redirect_blocked integer not null default 0`); err != nil {
			t.Fatalf("reapply api_tokens.unknown_model_redirect_blocked after migration40RawUp: %v", err)
		}
		if _, err := s.db.ExecContext(ctx, `alter table api_tokens add column unknown_model_fallback text not null default ''`); err != nil {
			t.Fatalf("reapply api_tokens.unknown_model_fallback after migration40RawUp: %v", err)
		}

		// The token's OWN data survived the rebuild losslessly, via the NEW
		// Go code path (TokenByID/scanToken, which now reads the rebuilt shape).
		got, err := s.TokenByID(ctx, "tok_pre40")
		if err != nil {
			t.Fatalf("token by id after migration 40: %v", err)
		}
		if got.UserID != "u_pre40" || got.Name != "Pre-v40 Token" || got.Status != TokenStatusActive {
			t.Fatalf("token core fields lost: %+v", got)
		}
		if got.Scopes != `["gateway:use"]` || got.ModelOverride != "override-model" || got.ModelOverrideMap != `{"a":"b"}` {
			t.Fatalf("token metadata fields lost: %+v", got)
		}
		if !got.LogCommunication || !got.Secret {
			t.Fatalf("token flag fields lost: %+v", got)
		}
		if got.ServiceID != "" || got.Kind != TokenKindUser {
			t.Fatalf("existing row must land as a plain user token: ServiceID=%q Kind=%q", got.ServiceID, got.Kind)
		}
		// The secret still resolves (secret_hash carried through unchanged).
		if _, ok := s.LookupBearer("Bearer pre40-secret"); !ok {
			t.Fatal("LookupBearer failed to resolve the pre-existing secret after migration 40")
		}

		// THE key assertion: the route_affinity row is INTACT — the rebuild's
		// DROP TABLE api_tokens did not cascade-delete it via the FK.
		var affAfter int
		if err := s.db.QueryRow(`select count(*) from route_affinity where api_token_id = ?`, "tok_pre40").Scan(&affAfter); err != nil {
			t.Fatalf("count route_affinity after migration: %v", err)
		}
		if affAfter != 1 {
			t.Fatalf("route_affinity row LOST across the api_tokens rebuild (cascade-on-DROP hazard): count=%d, want 1", affAfter)
		}
		gotAff, ok, err := s.Affinity(ctx, routing.AffinityKey{APITokenID: "tok_pre40", Model: "fast-coder", APIFlavor: routing.APIFlavorOpenAI, SessionID: "sess1"})
		if err != nil || !ok {
			t.Fatalf("route_affinity row unreadable via the Go API after migration 40: ok=%v err=%v", ok, err)
		}
		if gotAff.ID != "aff_pre40" {
			t.Fatalf("unexpected surviving affinity: %+v", gotAff)
		}

		// The new schema shape works going forward: a service token inserts fine.
		if err := s.CreateService(ctx, routing.Service{ID: "svc_after40", Name: "After", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("create service after migration 40: %v", err)
		}
		if err := s.CreatePlainToken(ctx, TokenRecord{
			ID: "tok_after40", ServiceID: "svc_after40", Kind: TokenKindService,
			Name: "after", Status: TokenStatusActive, Scopes: `["llm:invoke"]`, CreatedAt: now, UpdatedAt: now,
		}, "after-secret-value"); err != nil {
			t.Fatalf("create service token after migration 40: %v", err)
		}

		// Foreign-key enforcement was correctly restored (not left permanently
		// disabled on the pooled connection): a bearer FK violation still maps
		// to ErrNotFound after migration40RawUp returns.
		if err := s.CreatePlainToken(ctx, TokenRecord{
			ID: "tok_fk_check", UserID: "usr_does_not_exist", Name: "n", Status: TokenStatusActive,
			Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now,
		}, "fk-check-secret-value"); err != ErrNotFound {
			t.Fatalf("CreatePlainToken with a missing user after migration 40 = %v, want ErrNotFound (foreign_keys must be back ON)", err)
		}
	})
}

// --- Phase 2 (principal limits): principal_limits CRUD + UsageAggregateSince ---

// TestConformancePrincipalLimits round-trips routing.LimitConfig through
// PrincipalLimits/SetPrincipalLimits/DeletePrincipalLimits for BOTH principal
// types, proves the composite (principal_type, principal_id) primary key keeps
// a service and a user with the SAME id fully independent, that an upsert
// overwrites in place, that deleting a missing row is a benign no-op, and that
// a never-configured principal reads back as (zero LimitConfig, false, nil) —
// the "no limits" no-op contract PrincipalLimiter (a later task) relies on.
func TestConformancePrincipalLimits(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()

		// Never configured -> ok=false, zero value, no error.
		cfg, ok, err := s.PrincipalLimits(ctx, routing.PrincipalTypeService, "svc_pl1")
		if err != nil {
			t.Fatalf("PrincipalLimits (unconfigured): %v", err)
		}
		if ok || cfg != (routing.LimitConfig{}) {
			t.Fatalf("PrincipalLimits (unconfigured) = (%+v, %v), want (zero, false)", cfg, ok)
		}

		svcCfg := routing.LimitConfig{
			RateRequests: 10, RateWindowSeconds: 60,
			RequestQuota: 1000, RequestQuotaPeriod: "day",
			// > int4 max (2147483647), proving TokenQuota round-trips as bigint.
			TokenQuota:       5_000_000_000,
			TokenQuotaPeriod: "month",
			CostBudget:       12.5, CostBudgetPeriod: "month",
		}
		if err := s.SetPrincipalLimits(ctx, routing.PrincipalTypeService, "svc_pl1", svcCfg); err != nil {
			t.Fatalf("SetPrincipalLimits(service): %v", err)
		}
		got, ok, err := s.PrincipalLimits(ctx, routing.PrincipalTypeService, "svc_pl1")
		if err != nil || !ok {
			t.Fatalf("PrincipalLimits(service) after set: ok=%v err=%v", ok, err)
		}
		if got != svcCfg {
			t.Fatalf("PrincipalLimits(service) round-trip = %+v, want %+v", got, svcCfg)
		}

		// A user row with the SAME id must be a fully independent row (composite PK).
		userCfg := routing.LimitConfig{RateRequests: 5, RateWindowSeconds: 30}
		if err := s.SetPrincipalLimits(ctx, routing.PrincipalTypeUser, "svc_pl1", userCfg); err != nil {
			t.Fatalf("SetPrincipalLimits(user): %v", err)
		}
		gotUser, ok, err := s.PrincipalLimits(ctx, routing.PrincipalTypeUser, "svc_pl1")
		if err != nil || !ok || gotUser != userCfg {
			t.Fatalf("PrincipalLimits(user) = (%+v, %v, %v), want (%+v, true, nil)", gotUser, ok, err, userCfg)
		}
		gotSvcAgain, ok, err := s.PrincipalLimits(ctx, routing.PrincipalTypeService, "svc_pl1")
		if err != nil || !ok || gotSvcAgain != svcCfg {
			t.Fatalf("service row disturbed by the user row's insert: (%+v, %v, %v)", gotSvcAgain, ok, err)
		}

		// Upsert overwrites the existing row in place (same PK, new values).
		updated := routing.LimitConfig{RateRequests: 20, RateWindowSeconds: 120, CostBudget: 99, CostBudgetPeriod: "hour"}
		if err := s.SetPrincipalLimits(ctx, routing.PrincipalTypeService, "svc_pl1", updated); err != nil {
			t.Fatalf("SetPrincipalLimits (overwrite): %v", err)
		}
		got, ok, err = s.PrincipalLimits(ctx, routing.PrincipalTypeService, "svc_pl1")
		if err != nil || !ok || got != updated {
			t.Fatalf("overwrite did not stick: (%+v, %v, %v), want (%+v, true, nil)", got, ok, err, updated)
		}

		// Delete removes the row; a subsequent read is (zero, false, nil) again.
		if err := s.DeletePrincipalLimits(ctx, routing.PrincipalTypeService, "svc_pl1"); err != nil {
			t.Fatalf("DeletePrincipalLimits: %v", err)
		}
		if _, ok, err := s.PrincipalLimits(ctx, routing.PrincipalTypeService, "svc_pl1"); err != nil || ok {
			t.Fatalf("PrincipalLimits after delete = (_, %v, %v), want ok=false", ok, err)
		}
		// Deleting an already-missing row is a benign no-op, not an error.
		if err := s.DeletePrincipalLimits(ctx, routing.PrincipalTypeService, "svc_pl1"); err != nil {
			t.Fatalf("DeletePrincipalLimits (already gone) = %v, want nil (idempotent no-op)", err)
		}
		// The user row for the SAME id survives deleting the service row.
		if _, ok, err := s.PrincipalLimits(ctx, routing.PrincipalTypeUser, "svc_pl1"); err != nil || !ok {
			t.Fatalf("user row lost when deleting the service row with the same id: ok=%v err=%v", ok, err)
		}
	})
}

// TestConformanceUsageAggregateSince proves UsageAggregateSince sums requests,
// tokens, and price-weighted cost correctly, per §6.2/§12 of the design spec:
//   - Filters by service_id vs user_id depending on principalType (no
//     cross-principal leakage — a different principal's rows never count).
//   - created_at >= since excludes older rows (the calendar-period cutoff).
//   - tokens is the SUM of total_tokens across many int4-safe individual rows
//     whose SUM exceeds int4 (2147483647) — proving the aggregate is read back
//     as a correct int64 (bigint), not truncated/overflowed on either dialect.
//   - cost is energy_wh summed PER HOST then weighted by that host's own
//     ai_servers.price_per_kwh (when set), else the system-wide
//     energy_default_price_per_kwh fallback — mirroring the portal layer's
//     per-server-price-weighted cost so a cost_budget threshold is
//     apples-to-apples with the rest of the app's cost displays.
//   - An unrecognized principalType is an error.
func TestConformanceUsageAggregateSince(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		since := now
		before := since.Add(-time.Hour) // excluded: older than the cutoff
		t0 := since                     // included: exactly at the cutoff (>=)
		t1 := since.Add(time.Minute)
		t2 := since.Add(2 * time.Minute)

		// srv_priced has its OWN price_per_kwh (0.30); srv_default has none set
		// (0), so its share falls back to the system-wide default (0.20).
		if err := s.CreateAIServer(ctx, routing.AIServer{
			ID: "srv_priced", Name: "srv-priced", Domain: "priced.example.test",
			Provider: routing.ProviderOllama, Endpoint: "http://priced.example.test:11434",
			Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
			PricePerKwh: 0.30, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create srv_priced: %v", err)
		}
		if err := s.CreateAIServer(ctx, routing.AIServer{
			ID: "srv_default", Name: "srv-default", Domain: "default.example.test",
			Provider: routing.ProviderOllama, Endpoint: "http://default.example.test:11434",
			Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create srv_default: %v", err)
		}
		if err := s.SetSystemSetting(ctx, "energy_default_price_per_kwh", "0.20", now); err != nil {
			t.Fatalf("set energy_default_price_per_kwh: %v", err)
		}

		record := func(e usage.Event) {
			t.Helper()
			s.Record(e)
			if err := s.LastUsageError(); err != nil {
				t.Fatalf("record usage event %s: %v", e.ID, err)
			}
		}

		// Excluded: a row for the SAME service, but BEFORE the cutoff.
		record(usage.Event{
			ID: "agg_before", ServiceID: "svc_agg", Host: "srv_priced",
			TotalTokens: 900_000_000, EnergyWh: 1000, Status: "ok", HTTPStatus: 200, CreatedAt: before,
		})
		// Excluded: a row for a DIFFERENT service (no cross-principal leakage).
		record(usage.Event{
			ID: "agg_other_svc", ServiceID: "svc_other", Host: "srv_priced",
			TotalTokens: 900_000_000, EnergyWh: 1000, Status: "ok", HTTPStatus: 200, CreatedAt: t1,
		})

		// Included: three rows on svc_agg, each individually int4-safe
		// (900,000,000 < 2,147,483,647), whose SUM (2,700,000,000) exceeds int4.
		record(usage.Event{
			ID: "agg_1", ServiceID: "svc_agg", Host: "srv_priced",
			TotalTokens: 900_000_000, EnergyWh: 1000, Status: "ok", HTTPStatus: 200, CreatedAt: t0,
		})
		record(usage.Event{
			ID: "agg_2", ServiceID: "svc_agg", Host: "srv_priced",
			TotalTokens: 900_000_000, EnergyWh: 1000, Status: "ok", HTTPStatus: 200, CreatedAt: t1,
		})
		record(usage.Event{
			ID: "agg_3", ServiceID: "svc_agg", Host: "srv_default",
			TotalTokens: 900_000_000, EnergyWh: 500, Status: "ok", HTTPStatus: 200, CreatedAt: t2,
		})

		requests, tokens, cost, err := s.UsageAggregateSince(ctx, routing.PrincipalTypeService, "svc_agg", since)
		if err != nil {
			t.Fatalf("UsageAggregateSince(service): %v", err)
		}
		if requests != 3 {
			t.Fatalf("requests = %d, want 3 (before-cutoff + other-service rows excluded)", requests)
		}
		const wantTokens int64 = 2_700_000_000 // 3 * 900,000,000; > int4 max (2,147,483,647)
		if tokens != wantTokens {
			t.Fatalf("tokens = %d, want %d (bigint sum across int4-safe rows)", tokens, wantTokens)
		}
		// srv_priced: 2000 Wh @ 0.30/kWh = 0.6; srv_default: 500 Wh @ the 0.20
		// system default (its own price_per_kwh is unset) = 0.1. Total 0.7.
		// Tolerance widened past 1e-9 (vs. portal's in-memory approxEq): on
		// postgres, price_per_kwh/energy_wh are `real` (float4) columns, so a
		// value like 0.30 round-trips with float32-rounding noise (~1e-8 here)
		// once promoted back to float64 — an existing, accepted characteristic
		// of those column types (see ai_servers.price_per_kwh), not a bug.
		const wantCost = 0.7
		if diff := cost - wantCost; diff > 1e-4 || diff < -1e-4 {
			t.Fatalf("cost = %v, want %v", cost, wantCost)
		}

		// A principal with zero matching rows reads back as an honest empty
		// aggregate, never an error.
		requests, tokens, cost, err = s.UsageAggregateSince(ctx, routing.PrincipalTypeUser, "usr_no_usage", since)
		if err != nil || requests != 0 || tokens != 0 || cost != 0 {
			t.Fatalf("no-usage aggregate = (%d, %d, %v, %v), want (0, 0, 0, nil)", requests, tokens, cost, err)
		}

		// Per-user aggregation (user_id column) is exercised independently of
		// service_id, on the SAME table.
		record(usage.Event{
			ID: "agg_user_1", UserID: "usr_agg", Host: "srv_priced",
			TotalTokens: 100, EnergyWh: 100, Status: "ok", HTTPStatus: 200, CreatedAt: t0,
		})
		record(usage.Event{
			ID: "agg_user_before", UserID: "usr_agg", Host: "srv_priced",
			TotalTokens: 100, EnergyWh: 100, Status: "ok", HTTPStatus: 200, CreatedAt: before,
		})
		uRequests, uTokens, uCost, err := s.UsageAggregateSince(ctx, routing.PrincipalTypeUser, "usr_agg", since)
		if err != nil {
			t.Fatalf("UsageAggregateSince(user): %v", err)
		}
		if uRequests != 1 || uTokens != 100 {
			t.Fatalf("user aggregate = (%d, %d), want (1, 100)", uRequests, uTokens)
		}
		const wantUserCost = 0.03 // 100 Wh @ 0.30/kWh
		if diff := uCost - wantUserCost; diff > 1e-4 || diff < -1e-4 {
			t.Fatalf("user cost = %v, want %v", uCost, wantUserCost)
		}

		// An unrecognized principal type is an error (never silently zero).
		if _, _, _, err := s.UsageAggregateSince(ctx, "bogus", "x", since); err == nil {
			t.Fatal("UsageAggregateSince(bogus principal type) should error")
		}
	})
}

// TestConformanceUserGroups is the store-contract suite for the user-groups
// schema (migration v44): group CRUD (incl. the parent/owner FK round-trip),
// membership (incl. an invited row), the UserGroupsForUser tier/state filter
// (an invited membership must NOT satisfy a "member" state filter), managers,
// and the ON DELETE CASCADE from a parent group down to its child group's own
// membership/manager rows.
func TestConformanceUserGroups(t *testing.T) {
	forEachDialect(t, testUserGroups)
}

func testUserGroups(t *testing.T, s *SQLStore) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	for _, id := range []string{"usr_g1", "usr_g2"} {
		if err := s.CreateUser(ctx, User{ID: id, Email: id + "@x", DisplayName: id, Role: "user", Status: UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("create user %s: %v", id, err)
		}
	}

	sys := UserGroup{ID: "ugrp_s", Tier: GroupTierSystem, Name: "Sys", CreatedAt: now, UpdatedAt: now}
	if err := s.CreateUserGroup(ctx, sys); err != nil {
		t.Fatalf("create system group: %v", err)
	}
	adm := UserGroup{ID: "ugrp_a", Tier: GroupTierAdmin, Name: "Adm", ParentGroupID: "ugrp_s", OwnerUserID: "usr_g1", CreatedAt: now, UpdatedAt: now}
	if err := s.CreateUserGroup(ctx, adm); err != nil {
		t.Fatalf("create admin group: %v", err)
	}

	// Duplicate id -> ErrConflict.
	if err := s.CreateUserGroup(ctx, adm); err != ErrConflict {
		t.Fatalf("create duplicate group id = %v, want ErrConflict", err)
	}
	// A group referencing a nonexistent parent/owner -> ErrNotFound (FK violation).
	if err := s.CreateUserGroup(ctx, UserGroup{ID: "ugrp_bad_parent", Tier: GroupTierAdmin, Name: "Bad", ParentGroupID: "ugrp_missing", CreatedAt: now, UpdatedAt: now}); err != ErrNotFound {
		t.Fatalf("create group with missing parent = %v, want ErrNotFound", err)
	}

	got, err := s.UserGroupByID(ctx, "ugrp_a")
	if err != nil {
		t.Fatalf("UserGroupByID: %v", err)
	}
	if got.ParentGroupID != "ugrp_s" || got.OwnerUserID != "usr_g1" || got.Tier != GroupTierAdmin || got.Name != "Adm" {
		t.Fatalf("roundtrip: %+v", got)
	}
	// System group has NO parent/owner — both must read back empty (real SQL NULL).
	gotSys, err := s.UserGroupByID(ctx, "ugrp_s")
	if err != nil {
		t.Fatalf("UserGroupByID(system): %v", err)
	}
	if gotSys.ParentGroupID != "" || gotSys.OwnerUserID != "" {
		t.Fatalf("system group parent/owner not empty: %+v", gotSys)
	}
	if _, err := s.UserGroupByID(ctx, "ugrp_missing"); err != ErrNotFound {
		t.Fatalf("UserGroupByID(missing) = %v, want ErrNotFound", err)
	}

	// Update: rename + change owner; ParentGroupID/Tier are not writable via Update.
	updated := got
	updated.Name = "Adm Renamed"
	updated.OwnerUserID = "usr_g2"
	updated.UpdatedAt = now.Add(time.Minute)
	if err := s.UpdateUserGroup(ctx, updated); err != nil {
		t.Fatalf("UpdateUserGroup: %v", err)
	}
	got, err = s.UserGroupByID(ctx, "ugrp_a")
	if err != nil || got.Name != "Adm Renamed" || got.OwnerUserID != "usr_g2" {
		t.Fatalf("after update: %+v err=%v", got, err)
	}
	if err := s.UpdateUserGroup(ctx, UserGroup{ID: "ugrp_missing", Name: "x", UpdatedAt: now}); err != ErrNotFound {
		t.Fatalf("UpdateUserGroup(missing) = %v, want ErrNotFound", err)
	}

	// ListUserGroupsByTier / ChildUserGroups. The store is already fully
	// migrated (forEachDialect), so migration v44's own seeded
	// DefaultAdminGroupID is ALSO admin-tier — assert presence of ours, not
	// an exact count.
	byTier, err := s.ListUserGroupsByTier(ctx, GroupTierAdmin)
	if err != nil {
		t.Fatalf("ListUserGroupsByTier(admin): %v", err)
	}
	var sawOurAdminGroup bool
	for _, g := range byTier {
		if g.ID == "ugrp_a" {
			sawOurAdminGroup = true
		}
	}
	if !sawOurAdminGroup {
		t.Fatalf("ListUserGroupsByTier(admin) missing ugrp_a: %+v", byTier)
	}
	children, err := s.ChildUserGroups(ctx, "ugrp_s")
	if err != nil || len(children) != 1 || children[0].ID != "ugrp_a" {
		t.Fatalf("ChildUserGroups(ugrp_s) = %+v err=%v", children, err)
	}

	// Members: usr_g1 is a full member, usr_g2 is invited (by usr_g1).
	if err := s.SetUserGroupMember(ctx, "ugrp_a", "usr_g1", GroupStateMember, ""); err != nil {
		t.Fatalf("SetUserGroupMember(member): %v", err)
	}
	if err := s.SetUserGroupMember(ctx, "ugrp_a", "usr_g2", GroupStateInvited, "usr_g1"); err != nil {
		t.Fatalf("SetUserGroupMember(invited): %v", err)
	}
	// SetUserGroupMember against a nonexistent group -> ErrNotFound.
	if err := s.SetUserGroupMember(ctx, "ugrp_missing", "usr_g1", GroupStateMember, ""); err != ErrNotFound {
		t.Fatalf("SetUserGroupMember(missing group) = %v, want ErrNotFound", err)
	}

	members, err := s.UserGroupMembers(ctx, "ugrp_a")
	if err != nil {
		t.Fatalf("UserGroupMembers: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("members = %d, want 2: %+v", len(members), members)
	}
	var sawInvited bool
	for _, m := range members {
		if m.UserID == "usr_g2" {
			sawInvited = true
			if m.State != GroupStateInvited || m.InvitedBy != "usr_g1" {
				t.Fatalf("invited member wrong: %+v", m)
			}
		}
	}
	if !sawInvited {
		t.Fatalf("invited member usr_g2 not found in %+v", members)
	}

	// Upsert (SetUserGroupMember again) changes state in place, no duplicate row.
	if err := s.SetUserGroupMember(ctx, "ugrp_a", "usr_g2", GroupStateMember, ""); err != nil {
		t.Fatalf("SetUserGroupMember(upsert to member): %v", err)
	}
	members, _ = s.UserGroupMembers(ctx, "ugrp_a")
	if len(members) != 2 {
		t.Fatalf("members after upsert = %d, want 2 (no duplicate row): %+v", len(members), members)
	}

	// UserGroupsForUser: usr_g1 is a member of ugrp_a (admin tier).
	gs, err := s.UserGroupsForUser(ctx, "usr_g1", GroupTierAdmin, GroupStateMember)
	if err != nil || len(gs) != 1 || gs[0].ID != "ugrp_a" {
		t.Fatalf("UserGroupsForUser(usr_g1, admin, member) = %+v err=%v", gs, err)
	}
	// Any-tier/any-state lookup (both args "") also finds it.
	gsAny, err := s.UserGroupsForUser(ctx, "usr_g1", "", "")
	if err != nil || len(gsAny) != 1 || gsAny[0].ID != "ugrp_a" {
		t.Fatalf("UserGroupsForUser(usr_g1, any, any) = %+v err=%v", gsAny, err)
	}
	// usr_g2 was upserted to "member" above, so it now DOES satisfy a member filter.
	gs2, err := s.UserGroupsForUser(ctx, "usr_g2", GroupTierAdmin, GroupStateMember)
	if err != nil || len(gs2) != 1 {
		t.Fatalf("UserGroupsForUser(usr_g2, admin, member) after upsert = %+v err=%v", gs2, err)
	}
	// Re-invite usr_g2 to prove an invited membership does NOT satisfy a
	// "member" state filter (the state discriminator is load-bearing).
	if err := s.SetUserGroupMember(ctx, "ugrp_a", "usr_g2", GroupStateInvited, "usr_g1"); err != nil {
		t.Fatalf("SetUserGroupMember(re-invite): %v", err)
	}
	gs3, err := s.UserGroupsForUser(ctx, "usr_g2", GroupTierAdmin, GroupStateMember)
	if err != nil {
		t.Fatalf("UserGroupsForUser(usr_g2, admin, member) after re-invite: %v", err)
	}
	if len(gs3) != 0 {
		t.Fatalf("invited state leaked into member filter: %+v", gs3)
	}

	// RemoveUserGroupMember.
	if err := s.RemoveUserGroupMember(ctx, "ugrp_a", "usr_g2"); err != nil {
		t.Fatalf("RemoveUserGroupMember: %v", err)
	}
	members, _ = s.UserGroupMembers(ctx, "ugrp_a")
	if len(members) != 1 || members[0].UserID != "usr_g1" {
		t.Fatalf("members after remove = %+v, want only usr_g1", members)
	}

	// Managers.
	if err := s.SetUserGroupManager(ctx, "ugrp_a", "usr_g2"); err != nil {
		t.Fatalf("SetUserGroupManager: %v", err)
	}
	// Idempotent re-set (on-conflict-do-nothing) does not error or duplicate.
	if err := s.SetUserGroupManager(ctx, "ugrp_a", "usr_g2"); err != nil {
		t.Fatalf("SetUserGroupManager(re-set): %v", err)
	}
	if err := s.SetUserGroupManager(ctx, "ugrp_missing", "usr_g1"); err != ErrNotFound {
		t.Fatalf("SetUserGroupManager(missing group) = %v, want ErrNotFound", err)
	}
	mgrs, err := s.UserGroupManagers(ctx, "ugrp_a")
	if err != nil || len(mgrs) != 1 || mgrs[0] != "usr_g2" {
		t.Fatalf("UserGroupManagers = %+v err=%v", mgrs, err)
	}
	if err := s.RemoveUserGroupManager(ctx, "ugrp_a", "usr_g2"); err != nil {
		t.Fatalf("RemoveUserGroupManager: %v", err)
	}
	mgrs, _ = s.UserGroupManagers(ctx, "ugrp_a")
	if len(mgrs) != 0 {
		t.Fatalf("managers after remove = %+v, want empty", mgrs)
	}
	// Re-add a manager so the cascade assertion below has something to prove wrong.
	if err := s.SetUserGroupManager(ctx, "ugrp_a", "usr_g1"); err != nil {
		t.Fatalf("SetUserGroupManager(re-add): %v", err)
	}

	// Cascade: deleting the system group removes the admin child + its
	// member/manager rows (ON DELETE CASCADE from user_groups.parent_group_id
	// down to user_group_members/user_group_managers via group_id).
	if err := s.DeleteUserGroup(ctx, "ugrp_s"); err != nil {
		t.Fatalf("DeleteUserGroup(system): %v", err)
	}
	if _, err := s.UserGroupByID(ctx, "ugrp_a"); err != ErrNotFound {
		t.Fatalf("child group not cascade-deleted: %v", err)
	}
	if m, err := s.UserGroupMembers(ctx, "ugrp_a"); err != nil || len(m) != 0 {
		t.Fatalf("members not cascaded: %+v err=%v", m, err)
	}
	if m, err := s.UserGroupManagers(ctx, "ugrp_a"); err != nil || len(m) != 0 {
		t.Fatalf("managers not cascaded: %+v err=%v", m, err)
	}
	if err := s.DeleteUserGroup(ctx, "ugrp_s"); err != ErrNotFound {
		t.Fatalf("DeleteUserGroup(already deleted) = %v, want ErrNotFound", err)
	}
}

// TestConformanceUserGroupManagerPerms exercises the per-Admin-Group
// co-manager permission flags added by migration v48 (UserGroupManagerPerm /
// UserGroupManagerPerms / SetUserGroupManagerPermissions), extended by
// migration v49 (CanManageServers), migration v51 (CanManageServices), and
// migration v53 (CanManageResources), independent of testUserGroups above.
func TestConformanceUserGroupManagerPerms(t *testing.T) {
	forEachDialect(t, testUserGroupManagerPerms)
}

func testUserGroupManagerPerms(t *testing.T, s *SQLStore) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	for _, id := range []string{"usr_gp1", "usr_gp2"} {
		if err := s.CreateUser(ctx, User{ID: id, Email: id + "@x", DisplayName: id, Role: "user", Status: UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("create user %s: %v", id, err)
		}
	}
	sys := UserGroup{ID: "ugrp_gp_sys", Tier: GroupTierSystem, Name: "Sys", CreatedAt: now, UpdatedAt: now}
	if err := s.CreateUserGroup(ctx, sys); err != nil {
		t.Fatalf("create system group: %v", err)
	}
	adm := UserGroup{ID: "ugrp_gp_adm", Tier: GroupTierAdmin, Name: "Adm", ParentGroupID: "ugrp_gp_sys", OwnerUserID: "usr_gp1", CreatedAt: now, UpdatedAt: now}
	if err := s.CreateUserGroup(ctx, adm); err != nil {
		t.Fatalf("create admin group: %v", err)
	}

	// A group with no co-managers yet -> empty (non-nil) slice.
	if perms, err := s.UserGroupManagerPerms(ctx, "ugrp_gp_adm"); err != nil || len(perms) != 0 {
		t.Fatalf("UserGroupManagerPerms(none yet) = %+v err=%v", perms, err)
	}

	// SetUserGroupManagerPermissions against a manager row that does not
	// exist yet (group exists, but usr_gp2 is not a co-manager) -> ErrNotFound.
	if err := s.SetUserGroupManagerPermissions(ctx, "ugrp_gp_adm", UserGroupManagerPerm{UserID: "usr_gp2", CanManageUsers: true, CanManageGroup: false, CanManageServers: true, CanManageServices: true, CanManageResources: true}); err != ErrNotFound {
		t.Fatalf("SetUserGroupManagerPermissions(no row) = %v, want ErrNotFound", err)
	}

	// SetUserGroupManager's insert picks up the column DEFAULT (1/1/1/1/1) —
	// a fresh co-manager row starts with FULL rights, matching today's
	// pre-migration behavior byte-for-byte (extended to can_manage_servers by
	// migration v49, to can_manage_services by migration v51, and to
	// can_manage_resources by migration v53).
	if err := s.SetUserGroupManager(ctx, "ugrp_gp_adm", "usr_gp2"); err != nil {
		t.Fatalf("SetUserGroupManager: %v", err)
	}
	perms, err := s.UserGroupManagerPerms(ctx, "ugrp_gp_adm")
	if err != nil {
		t.Fatalf("UserGroupManagerPerms: %v", err)
	}
	if len(perms) != 1 || perms[0].UserID != "usr_gp2" || !perms[0].CanManageUsers || !perms[0].CanManageGroup || !perms[0].CanManageServers || !perms[0].CanManageServices || !perms[0].CanManageResources {
		t.Fatalf("UserGroupManagerPerms(fresh insert) = %+v, want one row usr_gp2 true/true/true/true/true", perms)
	}

	// Narrow the flags: CanManageUsers stays true, CanManageGroup flips
	// false, CanManageServers stays true, CanManageServices stays true,
	// CanManageResources stays true (untouched by this call).
	if err := s.SetUserGroupManagerPermissions(ctx, "ugrp_gp_adm", UserGroupManagerPerm{UserID: "usr_gp2", CanManageUsers: true, CanManageGroup: false, CanManageServers: true, CanManageServices: true, CanManageResources: true}); err != nil {
		t.Fatalf("SetUserGroupManagerPermissions: %v", err)
	}
	perms, err = s.UserGroupManagerPerms(ctx, "ugrp_gp_adm")
	if err != nil {
		t.Fatalf("UserGroupManagerPerms(after narrow): %v", err)
	}
	if len(perms) != 1 || perms[0].UserID != "usr_gp2" || !perms[0].CanManageUsers || perms[0].CanManageGroup || !perms[0].CanManageServers || !perms[0].CanManageServices || !perms[0].CanManageResources {
		t.Fatalf("UserGroupManagerPerms(after narrow) = %+v, want usr_gp2 true/false/true/true/true", perms)
	}

	// Narrow CanManageServers INDEPENDENTLY of the other three: widen
	// CanManageGroup back to true while flipping CanManageServers to false —
	// proves each flag is settable without disturbing the others
	// (admin-group permissions Phase B, spec 2026-08-10).
	if err := s.SetUserGroupManagerPermissions(ctx, "ugrp_gp_adm", UserGroupManagerPerm{UserID: "usr_gp2", CanManageUsers: true, CanManageGroup: true, CanManageServers: false, CanManageServices: true, CanManageResources: true}); err != nil {
		t.Fatalf("SetUserGroupManagerPermissions(narrow servers only): %v", err)
	}
	perms, err = s.UserGroupManagerPerms(ctx, "ugrp_gp_adm")
	if err != nil {
		t.Fatalf("UserGroupManagerPerms(after independent servers-narrow): %v", err)
	}
	if len(perms) != 1 || perms[0].UserID != "usr_gp2" || !perms[0].CanManageUsers || !perms[0].CanManageGroup || perms[0].CanManageServers || !perms[0].CanManageServices || !perms[0].CanManageResources {
		t.Fatalf("UserGroupManagerPerms(after independent servers-narrow) = %+v, want usr_gp2 true/true/false/true/true", perms)
	}

	// Narrow CanManageServices INDEPENDENTLY of the other three: widen
	// CanManageServers back to true while flipping CanManageServices to
	// false — proves the FOURTH flag is settable without disturbing the
	// others, in either direction (admin-group permissions Phase C, spec
	// 2026-08-10).
	if err := s.SetUserGroupManagerPermissions(ctx, "ugrp_gp_adm", UserGroupManagerPerm{UserID: "usr_gp2", CanManageUsers: true, CanManageGroup: true, CanManageServers: true, CanManageServices: false, CanManageResources: true}); err != nil {
		t.Fatalf("SetUserGroupManagerPermissions(narrow services only): %v", err)
	}
	perms, err = s.UserGroupManagerPerms(ctx, "ugrp_gp_adm")
	if err != nil {
		t.Fatalf("UserGroupManagerPerms(after independent services-narrow): %v", err)
	}
	if len(perms) != 1 || perms[0].UserID != "usr_gp2" || !perms[0].CanManageUsers || !perms[0].CanManageGroup || !perms[0].CanManageServers || perms[0].CanManageServices || !perms[0].CanManageResources {
		t.Fatalf("UserGroupManagerPerms(after independent services-narrow) = %+v, want usr_gp2 true/true/true/false/true", perms)
	}

	// Narrow CanManageResources INDEPENDENTLY of the other four: widen
	// CanManageServices back to true while flipping CanManageResources to
	// false — proves the FIFTH flag is settable without disturbing the
	// others, in either direction (Resource Groups Phase 1, spec 2026-08-11).
	if err := s.SetUserGroupManagerPermissions(ctx, "ugrp_gp_adm", UserGroupManagerPerm{UserID: "usr_gp2", CanManageUsers: true, CanManageGroup: true, CanManageServers: true, CanManageServices: true, CanManageResources: false}); err != nil {
		t.Fatalf("SetUserGroupManagerPermissions(narrow resources only): %v", err)
	}
	perms, err = s.UserGroupManagerPerms(ctx, "ugrp_gp_adm")
	if err != nil {
		t.Fatalf("UserGroupManagerPerms(after independent resources-narrow): %v", err)
	}
	if len(perms) != 1 || perms[0].UserID != "usr_gp2" || !perms[0].CanManageUsers || !perms[0].CanManageGroup || !perms[0].CanManageServers || !perms[0].CanManageServices || perms[0].CanManageResources {
		t.Fatalf("UserGroupManagerPerms(after independent resources-narrow) = %+v, want usr_gp2 true/true/true/true/false", perms)
	}

	// A SECOND co-manager also starts at 1/1/1/1/1, and is unaffected by
	// usr_gp2's narrowed flags — each row's permissions are independent.
	if err := s.SetUserGroupManager(ctx, "ugrp_gp_adm", "usr_gp1"); err != nil {
		t.Fatalf("SetUserGroupManager(second): %v", err)
	}
	perms, err = s.UserGroupManagerPerms(ctx, "ugrp_gp_adm")
	if err != nil {
		t.Fatalf("UserGroupManagerPerms(two managers): %v", err)
	}
	if len(perms) != 2 {
		t.Fatalf("UserGroupManagerPerms(two managers) = %+v, want 2 rows", perms)
	}
	byUser := map[string]UserGroupManagerPerm{}
	for _, p := range perms {
		byUser[p.UserID] = p
	}
	if p, ok := byUser["usr_gp1"]; !ok || !p.CanManageUsers || !p.CanManageGroup || !p.CanManageServers || !p.CanManageServices || !p.CanManageResources {
		t.Fatalf("usr_gp1 perms = %+v, want true/true/true/true/true (fresh default)", p)
	}
	if p, ok := byUser["usr_gp2"]; !ok || !p.CanManageUsers || !p.CanManageGroup || !p.CanManageServers || !p.CanManageServices || p.CanManageResources {
		t.Fatalf("usr_gp2 perms = %+v, want true/true/true/true/false (narrowed earlier, unaffected by the new row)", p)
	}

	// A re-set of an EXISTING manager (on-conflict-do-nothing) must not reset
	// its already-narrowed permission flags back to the default.
	if err := s.SetUserGroupManager(ctx, "ugrp_gp_adm", "usr_gp2"); err != nil {
		t.Fatalf("SetUserGroupManager(re-set existing): %v", err)
	}
	perms, err = s.UserGroupManagerPerms(ctx, "ugrp_gp_adm")
	if err != nil {
		t.Fatalf("UserGroupManagerPerms(after re-set): %v", err)
	}
	for _, p := range perms {
		if p.UserID == "usr_gp2" && p.CanManageResources {
			t.Fatalf("re-set of an existing manager reset its narrowed flags: %+v", perms)
		}
	}

	// SetUserGroupManagerPermissions against a nonexistent group -> ErrNotFound.
	if err := s.SetUserGroupManagerPermissions(ctx, "ugrp_gp_missing", UserGroupManagerPerm{UserID: "usr_gp1", CanManageUsers: true, CanManageGroup: true, CanManageServers: true, CanManageServices: true, CanManageResources: true}); err != ErrNotFound {
		t.Fatalf("SetUserGroupManagerPermissions(missing group) = %v, want ErrNotFound", err)
	}

	// Removing a manager also drops its permission row (no dangling perms
	// for a userID no longer returned by UserGroupManagers).
	if err := s.RemoveUserGroupManager(ctx, "ugrp_gp_adm", "usr_gp1"); err != nil {
		t.Fatalf("RemoveUserGroupManager: %v", err)
	}
	perms, err = s.UserGroupManagerPerms(ctx, "ugrp_gp_adm")
	if err != nil || len(perms) != 1 || perms[0].UserID != "usr_gp2" {
		t.Fatalf("UserGroupManagerPerms(after remove) = %+v err=%v, want only usr_gp2", perms, err)
	}
}

func TestConformanceProjects(t *testing.T) {
	forEachDialect(t, testProjects)
}

func testProjects(t *testing.T, s *SQLStore) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	for _, id := range []string{"usr_p1", "usr_p2", "usr_p3"} {
		if err := s.CreateUser(ctx, User{ID: id, Email: id + "@x", DisplayName: id, Role: "user", Status: UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("create user %s: %v", id, err)
		}
	}

	// A user-group with usr_p2 as a member (environment context for the
	// group assignment below; group MEMBERSHIP itself is not consulted by
	// the store-layer ProjectsByOwnerOrMember — that composition happens in
	// the service, see Task 3). grpA is deleted mid-test to prove the
	// group-delete cascade; grpB is an independent second group kept until
	// the final project-delete cascade so that assertion covers >1 row too.
	grpA := UserGroup{ID: "ugrp_proj_a", Tier: GroupTierSystem, Name: "ProjGroupA", CreatedAt: now, UpdatedAt: now}
	if err := s.CreateUserGroup(ctx, grpA); err != nil {
		t.Fatalf("create user group A: %v", err)
	}
	if err := s.SetUserGroupMember(ctx, grpA.ID, "usr_p2", GroupStateMember, ""); err != nil {
		t.Fatalf("SetUserGroupMember: %v", err)
	}
	grpB := UserGroup{ID: "ugrp_proj_b", Tier: GroupTierSystem, Name: "ProjGroupB", CreatedAt: now, UpdatedAt: now}
	if err := s.CreateUserGroup(ctx, grpB); err != nil {
		t.Fatalf("create user group B: %v", err)
	}

	proj := Project{ID: "proj_1", Name: "Alpha", Description: "d", OwnerUserID: "usr_p1", CreatedAt: now, UpdatedAt: now}
	if err := s.CreateProject(ctx, proj); err != nil {
		t.Fatalf("create project: %v", err)
	}
	// Duplicate id -> ErrConflict.
	if err := s.CreateProject(ctx, proj); err != ErrConflict {
		t.Fatalf("create duplicate project id = %v, want ErrConflict", err)
	}
	// A project referencing a nonexistent owner -> ErrNotFound (FK violation).
	if err := s.CreateProject(ctx, Project{ID: "proj_bad_owner", Name: "Bad", OwnerUserID: "usr_missing", CreatedAt: now, UpdatedAt: now}); err != ErrNotFound {
		t.Fatalf("create project with missing owner = %v, want ErrNotFound", err)
	}

	got, err := s.ProjectByID(ctx, proj.ID)
	if err != nil {
		t.Fatalf("ProjectByID: %v", err)
	}
	if got.Name != "Alpha" || got.OwnerUserID != "usr_p1" || got.Description != "d" {
		t.Fatalf("roundtrip: %+v", got)
	}
	if _, err := s.ProjectByID(ctx, "proj_missing"); err != ErrNotFound {
		t.Fatalf("ProjectByID(missing) = %v, want ErrNotFound", err)
	}

	// Update: rename + change description + change owner.
	updated := got
	updated.Name = "Alpha Renamed"
	updated.Description = "d2"
	updated.OwnerUserID = "usr_p2"
	updated.UpdatedAt = now.Add(time.Minute)
	if err := s.UpdateProject(ctx, updated); err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}
	got, err = s.ProjectByID(ctx, proj.ID)
	if err != nil || got.Name != "Alpha Renamed" || got.Description != "d2" || got.OwnerUserID != "usr_p2" {
		t.Fatalf("after update: %+v err=%v", got, err)
	}
	if err := s.UpdateProject(ctx, Project{ID: "proj_missing", Name: "x", UpdatedAt: now}); err != ErrNotFound {
		t.Fatalf("UpdateProject(missing) = %v, want ErrNotFound", err)
	}
	// Restore owner to usr_p1 for the ownership assertions below.
	got.OwnerUserID = "usr_p1"
	if err := s.UpdateProject(ctx, got); err != nil {
		t.Fatalf("UpdateProject(restore owner): %v", err)
	}

	// ListProjects.
	list, err := s.ListProjects(ctx)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	var sawProj bool
	for _, p := range list {
		if p.ID == proj.ID {
			sawProj = true
		}
	}
	if !sawProj {
		t.Fatalf("ListProjects missing %s: %+v", proj.ID, list)
	}

	// Direct member usr_p2.
	if err := s.SetProjectMember(ctx, proj.ID, "usr_p2"); err != nil {
		t.Fatalf("SetProjectMember: %v", err)
	}
	// SetProjectMember against a nonexistent project -> ErrNotFound.
	if err := s.SetProjectMember(ctx, "proj_missing", "usr_p2"); err != ErrNotFound {
		t.Fatalf("SetProjectMember(missing project) = %v, want ErrNotFound", err)
	}
	// Idempotent re-set (on-conflict-do-nothing) does not error or duplicate.
	if err := s.SetProjectMember(ctx, proj.ID, "usr_p2"); err != nil {
		t.Fatalf("SetProjectMember(re-set): %v", err)
	}
	members, err := s.ProjectMembers(ctx, proj.ID)
	if err != nil || len(members) != 1 || members[0] != "usr_p2" {
		t.Fatalf("ProjectMembers = %+v err=%v", members, err)
	}

	// Assign both groups.
	if err := s.SetProjectGroup(ctx, proj.ID, grpA.ID); err != nil {
		t.Fatalf("SetProjectGroup(A): %v", err)
	}
	if err := s.SetProjectGroup(ctx, proj.ID, grpB.ID); err != nil {
		t.Fatalf("SetProjectGroup(B): %v", err)
	}
	// SetProjectGroup against a nonexistent project -> ErrNotFound.
	if err := s.SetProjectGroup(ctx, "proj_missing", grpA.ID); err != ErrNotFound {
		t.Fatalf("SetProjectGroup(missing project) = %v, want ErrNotFound", err)
	}
	groups, err := s.ProjectGroups(ctx, proj.ID)
	if err != nil || len(groups) != 2 {
		t.Fatalf("ProjectGroups = %+v err=%v, want 2", groups, err)
	}

	// ProjectsByOwnerOrMember: owner (usr_p1), direct member (usr_p2), and a
	// non-member/non-owner (usr_p3) that gets none.
	byOwner, err := s.ProjectsByOwnerOrMember(ctx, "usr_p1")
	if err != nil || len(byOwner) != 1 || byOwner[0].ID != proj.ID {
		t.Fatalf("ProjectsByOwnerOrMember(owner) = %+v err=%v", byOwner, err)
	}
	byMember, err := s.ProjectsByOwnerOrMember(ctx, "usr_p2")
	if err != nil || len(byMember) != 1 || byMember[0].ID != proj.ID {
		t.Fatalf("ProjectsByOwnerOrMember(member) = %+v err=%v", byMember, err)
	}
	byNone, err := s.ProjectsByOwnerOrMember(ctx, "usr_p3")
	if err != nil || len(byNone) != 0 {
		t.Fatalf("ProjectsByOwnerOrMember(nonmember) = %+v err=%v, want empty", byNone, err)
	}

	// ProjectsByGroup, both groups.
	byGroupA, err := s.ProjectsByGroup(ctx, grpA.ID)
	if err != nil || len(byGroupA) != 1 || byGroupA[0].ID != proj.ID {
		t.Fatalf("ProjectsByGroup(A) = %+v err=%v", byGroupA, err)
	}
	byGroupB, err := s.ProjectsByGroup(ctx, grpB.ID)
	if err != nil || len(byGroupB) != 1 || byGroupB[0].ID != proj.ID {
		t.Fatalf("ProjectsByGroup(B) = %+v err=%v", byGroupB, err)
	}

	// Direct RemoveProjectGroup, then re-add B so the project-delete cascade
	// assertion further below still covers >1 row.
	if err := s.RemoveProjectGroup(ctx, proj.ID, grpB.ID); err != nil {
		t.Fatalf("RemoveProjectGroup(B): %v", err)
	}
	groups, _ = s.ProjectGroups(ctx, proj.ID)
	if len(groups) != 1 || groups[0] != grpA.ID {
		t.Fatalf("groups after direct remove = %+v, want only A", groups)
	}
	if err := s.SetProjectGroup(ctx, proj.ID, grpB.ID); err != nil {
		t.Fatalf("SetProjectGroup(B re-add): %v", err)
	}

	// Deleting user-GROUP A cascades: its project_groups row disappears,
	// leaving group B and the member untouched (ON DELETE CASCADE from
	// project_groups.group_id -> user_groups(id), migration45Up).
	if err := s.DeleteUserGroup(ctx, grpA.ID); err != nil {
		t.Fatalf("DeleteUserGroup(A): %v", err)
	}
	groups, err = s.ProjectGroups(ctx, proj.ID)
	if err != nil || len(groups) != 1 || groups[0] != grpB.ID {
		t.Fatalf("ProjectGroups after group delete = %+v err=%v, want only B (cascade)", groups, err)
	}
	members, err = s.ProjectMembers(ctx, proj.ID)
	if err != nil || len(members) != 1 || members[0] != "usr_p2" {
		t.Fatalf("members should be untouched by an unrelated group delete: %+v err=%v", members, err)
	}

	// Delete the project -> member + remaining group row cascade-gone (ON
	// DELETE CASCADE from project_members.project_id/project_groups.
	// project_id -> projects(id), migration45Up).
	if err := s.DeleteProject(ctx, proj.ID); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if _, err := s.ProjectByID(ctx, proj.ID); err != ErrNotFound {
		t.Fatalf("project not deleted: %v", err)
	}
	if m, err := s.ProjectMembers(ctx, proj.ID); err != nil || len(m) != 0 {
		t.Fatalf("members not cascaded on project delete: %+v err=%v", m, err)
	}
	if g, err := s.ProjectGroups(ctx, proj.ID); err != nil || len(g) != 0 {
		t.Fatalf("groups not cascaded on project delete: %+v err=%v", g, err)
	}
	if err := s.DeleteProject(ctx, proj.ID); err != ErrNotFound {
		t.Fatalf("DeleteProject(already deleted) = %v, want ErrNotFound", err)
	}

	// --- coupled_group_id round-trip + CoupledProjectsByGroup + group-delete SET NULL ---
	if err := s.CreateUser(ctx, User{ID: "usr_c", Email: "usr_c@x", DisplayName: "usr_c", Role: "user", Status: UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create user usr_c: %v", err)
	}
	grpC := UserGroup{ID: "ugrp_coupled", Tier: GroupTierSystem, Name: "GroupC", OwnerUserID: "usr_c", CreatedAt: now, UpdatedAt: now}
	if err := s.CreateUserGroup(ctx, grpC); err != nil {
		t.Fatalf("create coupled user group: %v", err)
	}
	if err := s.CreateProject(ctx, Project{ID: "prj_coupled", Name: "Coupled", OwnerUserID: "", CoupledGroupID: grpC.ID, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create coupled project: %v", err)
	}
	// Also add the mirror project_groups row (a coupled project carries its group).
	if err := s.SetProjectGroup(ctx, "prj_coupled", grpC.ID); err != nil {
		t.Fatalf("set project group: %v", err)
	}
	gotCoupled, err := s.ProjectByID(ctx, "prj_coupled")
	if err != nil || gotCoupled.CoupledGroupID != grpC.ID {
		t.Fatalf("coupled round-trip: %+v err=%v", gotCoupled, err)
	}
	byGroup, err := s.CoupledProjectsByGroup(ctx, grpC.ID)
	if err != nil || len(byGroup) != 1 || byGroup[0].ID != "prj_coupled" {
		t.Fatalf("CoupledProjectsByGroup = %+v err=%v", byGroup, err)
	}
	// Deleting the group SET-NULLs coupled_group_id AND cascade-deletes the project_groups row.
	if err := s.DeleteUserGroup(ctx, grpC.ID); err != nil {
		t.Fatalf("delete coupled group: %v", err)
	}
	afterCoupled, err := s.ProjectByID(ctx, "prj_coupled")
	if err != nil || afterCoupled.CoupledGroupID != "" {
		t.Fatalf("after group delete coupled_group_id = %q err=%v, want empty", afterCoupled.CoupledGroupID, err)
	}
	if groups, _ := s.ProjectGroups(ctx, "prj_coupled"); len(groups) != 0 {
		t.Fatalf("project_groups not cascade-deleted: %+v", groups)
	}
	if left, _ := s.CoupledProjectsByGroup(ctx, grpC.ID); len(left) != 0 {
		t.Fatalf("CoupledProjectsByGroup after delete = %+v, want empty", left)
	}
}

// TestConformanceTokensByProject proves, on BOTH dialects, that
// SQLiteStore.TokensByProject returns exactly the USER tokens whose
// project_id equals the given project (newest first, mirroring
// TokensByService), that a token attached to a DIFFERENT project (or no
// project) is excluded, and that an empty projectID returns an empty slice
// without matching any NULL/empty project_id row.
func TestConformanceTokensByProject(t *testing.T) {
	forEachDialect(t, testTokensByProject)
}

func testTokensByProject(t *testing.T, s *SQLStore) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	for _, id := range []string{"usr_tbp1", "usr_tbp2"} {
		if err := s.CreateUser(ctx, User{ID: id, Email: id + "@x", DisplayName: id, Role: "user", Status: UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("create user %s: %v", id, err)
		}
	}
	projA := Project{ID: "proj_tbp_a", Name: "TBP-A", OwnerUserID: "usr_tbp1", CreatedAt: now, UpdatedAt: now}
	if err := s.CreateProject(ctx, projA); err != nil {
		t.Fatalf("create project A: %v", err)
	}
	projB := Project{ID: "proj_tbp_b", Name: "TBP-B", OwnerUserID: "usr_tbp1", CreatedAt: now, UpdatedAt: now}
	if err := s.CreateProject(ctx, projB); err != nil {
		t.Fatalf("create project B: %v", err)
	}

	// Two tokens (different owners) attached to project A, one to project B,
	// one attached to neither. Distinct CreatedAt so newest-first is observable.
	mk := func(id, userID, projectID string, at time.Time) TokenRecord {
		return TokenRecord{ID: id, UserID: userID, ProjectID: projectID, Name: id, Status: TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: at, UpdatedAt: at}
	}
	if err := s.CreatePlainToken(ctx, mk("tok_tbp1", "usr_tbp1", projA.ID, now), "secret-tbp1"); err != nil {
		t.Fatalf("create tok_tbp1: %v", err)
	}
	if err := s.CreatePlainToken(ctx, mk("tok_tbp2", "usr_tbp2", projA.ID, now.Add(time.Minute)), "secret-tbp2"); err != nil {
		t.Fatalf("create tok_tbp2: %v", err)
	}
	if err := s.CreatePlainToken(ctx, mk("tok_tbp3", "usr_tbp1", projB.ID, now.Add(2*time.Minute)), "secret-tbp3"); err != nil {
		t.Fatalf("create tok_tbp3: %v", err)
	}
	if err := s.CreatePlainToken(ctx, mk("tok_tbp4", "usr_tbp1", "", now.Add(3*time.Minute)), "secret-tbp4"); err != nil {
		t.Fatalf("create tok_tbp4 (unattached): %v", err)
	}

	got, err := s.TokensByProject(ctx, projA.ID)
	if err != nil {
		t.Fatalf("TokensByProject(A): %v", err)
	}
	if len(got) != 2 || got[0].ID != "tok_tbp2" || got[1].ID != "tok_tbp1" {
		t.Fatalf("TokensByProject(A) = %+v, want [tok_tbp2, tok_tbp1] (newest first)", got)
	}
	if got[0].UserID != "usr_tbp2" || got[1].UserID != "usr_tbp1" {
		t.Fatalf("TokensByProject(A) owners = %+v", got)
	}

	gotB, err := s.TokensByProject(ctx, projB.ID)
	if err != nil || len(gotB) != 1 || gotB[0].ID != "tok_tbp3" {
		t.Fatalf("TokensByProject(B) = %+v err=%v, want [tok_tbp3]", gotB, err)
	}

	gotEmpty, err := s.TokensByProject(ctx, "")
	if err != nil || len(gotEmpty) != 0 {
		t.Fatalf("TokensByProject(\"\") = %+v err=%v, want empty", gotEmpty, err)
	}

	gotMissing, err := s.TokensByProject(ctx, "proj_missing")
	if err != nil || len(gotMissing) != 0 {
		t.Fatalf("TokensByProject(missing project) = %+v err=%v, want empty", gotMissing, err)
	}
}

// TestConformanceServerAdminGroups is the store-contract suite for the
// server<->admin-group linkage (admin-group permissions Phase B, migration
// v50, Task 2): SetServerAdminGroup/RemoveServerAdminGroup/ServerAdminGroups/
// ServersByAdminGroups + the per-server system_group_id containment root
// (UpdateServerSystemGroup). Covers the idempotent upsert, the multi-group
// listing order, the reverse lookup (incl. the empty-input short-circuit),
// BOTH foreign-key cascades (server delete AND group delete), and the
// system_group_id round-trip via AIServerByID.
func TestConformanceServerAdminGroups(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		if err := s.CreateUser(ctx, User{ID: "usr_sag_owner", Email: "sag-owner@x", DisplayName: "Owner", Role: "admin", Status: UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("create owner: %v", err)
		}
		sysGroup := UserGroup{ID: "ugrp_sag_sys", Tier: GroupTierSystem, Name: "SAG System", CreatedAt: now, UpdatedAt: now}
		if err := s.CreateUserGroup(ctx, sysGroup); err != nil {
			t.Fatalf("create system group: %v", err)
		}
		g1 := UserGroup{ID: "ugrp_sag_a", Tier: GroupTierAdmin, Name: "SAG Admin A", ParentGroupID: sysGroup.ID, OwnerUserID: "usr_sag_owner", CreatedAt: now, UpdatedAt: now}
		if err := s.CreateUserGroup(ctx, g1); err != nil {
			t.Fatalf("create admin group g1: %v", err)
		}
		g2 := UserGroup{ID: "ugrp_sag_b", Tier: GroupTierAdmin, Name: "SAG Admin B", ParentGroupID: sysGroup.ID, OwnerUserID: "usr_sag_owner", CreatedAt: now, UpdatedAt: now}
		if err := s.CreateUserGroup(ctx, g2); err != nil {
			t.Fatalf("create admin group g2: %v", err)
		}

		if err := s.CreateAIServer(ctx, routing.AIServer{ID: "srv_sag", Name: "SAG Server", Domain: "sag.local", Provider: routing.ProviderOllama, Endpoint: "http://sag.local:11434", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("create server: %v", err)
		}

		// SetServerAdminGroup(s,g1) + SetServerAdminGroup(s,g2) -> {g1,g2}.
		// A repeat SetServerAdminGroup(s,g1) hits the on-conflict-do-nothing
		// path (idempotent, no error, no duplicate row).
		if err := s.SetServerAdminGroup(ctx, "srv_sag", g1.ID); err != nil {
			t.Fatalf("set admin group g1: %v", err)
		}
		if err := s.SetServerAdminGroup(ctx, "srv_sag", g1.ID); err != nil {
			t.Fatalf("re-set admin group g1 (idempotent): %v", err)
		}
		if err := s.SetServerAdminGroup(ctx, "srv_sag", g2.ID); err != nil {
			t.Fatalf("set admin group g2: %v", err)
		}
		groups, err := s.ServerAdminGroups(ctx, "srv_sag")
		if err != nil {
			t.Fatalf("server admin groups: %v", err)
		}
		if len(groups) != 2 || groups[0] != g1.ID || groups[1] != g2.ID {
			t.Fatalf("server admin groups = %v, want [%s %s]", groups, g1.ID, g2.ID)
		}

		// A missing server -> ErrNotFound (FK violation); a missing group ->
		// ErrNotFound too.
		if err := s.SetServerAdminGroup(ctx, "srv_sag_missing", g1.ID); err != ErrNotFound {
			t.Fatalf("set admin group on missing server = %v, want ErrNotFound", err)
		}
		if err := s.SetServerAdminGroup(ctx, "srv_sag", "ugrp_missing"); err != ErrNotFound {
			t.Fatalf("set admin group with missing group = %v, want ErrNotFound", err)
		}

		// ServersByAdminGroups([g1]) returns the server.
		byG1, err := s.ServersByAdminGroups(ctx, []string{g1.ID})
		if err != nil {
			t.Fatalf("servers by admin groups(g1): %v", err)
		}
		if len(byG1) != 1 || byG1[0].ID != "srv_sag" {
			t.Fatalf("servers by admin groups(g1) = %+v, want [srv_sag]", byG1)
		}

		// ServersByAdminGroups is deduped: a server linked to BOTH g1 and g2
		// still appears exactly once when both are queried.
		byBoth, err := s.ServersByAdminGroups(ctx, []string{g1.ID, g2.ID})
		if err != nil {
			t.Fatalf("servers by admin groups(g1,g2): %v", err)
		}
		if len(byBoth) != 1 || byBoth[0].ID != "srv_sag" {
			t.Fatalf("servers by admin groups(g1,g2) = %+v, want a single srv_sag", byBoth)
		}

		// ServersByAdminGroups([]) -> empty, no query issued.
		byEmpty, err := s.ServersByAdminGroups(ctx, []string{})
		if err != nil || len(byEmpty) != 0 {
			t.Fatalf("servers by admin groups([]) = %+v err=%v, want empty", byEmpty, err)
		}

		// RemoveServerAdminGroup(s,g1) leaves {g2}. A repeat remove is a no-op
		// (non-error).
		if err := s.RemoveServerAdminGroup(ctx, "srv_sag", g1.ID); err != nil {
			t.Fatalf("remove admin group g1: %v", err)
		}
		if err := s.RemoveServerAdminGroup(ctx, "srv_sag", g1.ID); err != nil {
			t.Fatalf("re-remove admin group g1 (no-op): %v", err)
		}
		groups, err = s.ServerAdminGroups(ctx, "srv_sag")
		if err != nil || len(groups) != 1 || groups[0] != g2.ID {
			t.Fatalf("server admin groups after remove = %v err=%v, want [%s]", groups, err, g2.ID)
		}

		// UpdateServerSystemGroup round-trips via AIServerByID.
		if err := s.UpdateServerSystemGroup(ctx, "srv_sag", sysGroup.ID); err != nil {
			t.Fatalf("update server system group: %v", err)
		}
		got, err := s.AIServerByID(ctx, "srv_sag")
		if err != nil {
			t.Fatalf("AIServerByID: %v", err)
		}
		if got.SystemGroupID != sysGroup.ID {
			t.Fatalf("AIServerByID SystemGroupID = %q, want %q", got.SystemGroupID, sysGroup.ID)
		}
		if err := s.UpdateServerSystemGroup(ctx, "srv_sag_missing", sysGroup.ID); err != ErrNotFound {
			t.Fatalf("update server system group on missing server = %v, want ErrNotFound", err)
		}

		// Re-link g1 so the delete-cascade assertions below exercise a
		// server with 2 groups (g1 re-added, g2 already present).
		if err := s.SetServerAdminGroup(ctx, "srv_sag", g1.ID); err != nil {
			t.Fatalf("re-link admin group g1: %v", err)
		}

		// Deleting group g2 (FK ON DELETE CASCADE) removes ITS server_admin_groups
		// row only — g1's link survives.
		if err := s.DeleteUserGroup(ctx, g2.ID); err != nil {
			t.Fatalf("delete group g2: %v", err)
		}
		groups, err = s.ServerAdminGroups(ctx, "srv_sag")
		if err != nil || len(groups) != 1 || groups[0] != g1.ID {
			t.Fatalf("server admin groups after group delete = %v err=%v, want [%s]", groups, err, g1.ID)
		}

		// Deleting the server (FK ON DELETE CASCADE) removes every remaining
		// server_admin_groups row for it.
		if err := s.DeleteAIServer(ctx, "srv_sag"); err != nil {
			t.Fatalf("delete server: %v", err)
		}
		groups, err = s.ServerAdminGroups(ctx, "srv_sag")
		if err != nil || len(groups) != 0 {
			t.Fatalf("server admin groups after server delete = %v err=%v, want empty", groups, err)
		}
	})
}

// TestConformanceServiceAdminGroups is the SERVICES analog of
// TestConformanceServerAdminGroups (admin-group permissions Phase C, spec
// 2026-08-10 — Task 2, migration v52): a service links to admin groups via
// service_admin_groups exactly like an AI-server links via
// server_admin_groups, and carries the same system_group_id containment
// root.
func TestConformanceServiceAdminGroups(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		if err := s.CreateUser(ctx, User{ID: "usr_sag2_owner", Email: "sag2-owner@x", DisplayName: "Owner", Role: "admin", Status: UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("create owner: %v", err)
		}
		sysGroup := UserGroup{ID: "ugrp_sag2_sys", Tier: GroupTierSystem, Name: "SAG2 System", CreatedAt: now, UpdatedAt: now}
		if err := s.CreateUserGroup(ctx, sysGroup); err != nil {
			t.Fatalf("create system group: %v", err)
		}
		g1 := UserGroup{ID: "ugrp_sag2_a", Tier: GroupTierAdmin, Name: "SAG2 Admin A", ParentGroupID: sysGroup.ID, OwnerUserID: "usr_sag2_owner", CreatedAt: now, UpdatedAt: now}
		if err := s.CreateUserGroup(ctx, g1); err != nil {
			t.Fatalf("create admin group g1: %v", err)
		}
		g2 := UserGroup{ID: "ugrp_sag2_b", Tier: GroupTierAdmin, Name: "SAG2 Admin B", ParentGroupID: sysGroup.ID, OwnerUserID: "usr_sag2_owner", CreatedAt: now, UpdatedAt: now}
		if err := s.CreateUserGroup(ctx, g2); err != nil {
			t.Fatalf("create admin group g2: %v", err)
		}

		if err := s.CreateService(ctx, routing.Service{ID: "svc_sag2", Name: "SAG2 Service", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("create service: %v", err)
		}

		// SetServiceAdminGroup(svc,g1) + SetServiceAdminGroup(svc,g2) -> {g1,g2}.
		// A repeat SetServiceAdminGroup(svc,g1) hits the on-conflict-do-nothing
		// path (idempotent, no error, no duplicate row).
		if err := s.SetServiceAdminGroup(ctx, "svc_sag2", g1.ID); err != nil {
			t.Fatalf("set admin group g1: %v", err)
		}
		if err := s.SetServiceAdminGroup(ctx, "svc_sag2", g1.ID); err != nil {
			t.Fatalf("re-set admin group g1 (idempotent): %v", err)
		}
		if err := s.SetServiceAdminGroup(ctx, "svc_sag2", g2.ID); err != nil {
			t.Fatalf("set admin group g2: %v", err)
		}
		groups, err := s.ServiceAdminGroups(ctx, "svc_sag2")
		if err != nil {
			t.Fatalf("service admin groups: %v", err)
		}
		if len(groups) != 2 || groups[0] != g1.ID || groups[1] != g2.ID {
			t.Fatalf("service admin groups = %v, want [%s %s]", groups, g1.ID, g2.ID)
		}

		// A missing service -> ErrNotFound (FK violation); a missing group ->
		// ErrNotFound too.
		if err := s.SetServiceAdminGroup(ctx, "svc_sag2_missing", g1.ID); err != ErrNotFound {
			t.Fatalf("set admin group on missing service = %v, want ErrNotFound", err)
		}
		if err := s.SetServiceAdminGroup(ctx, "svc_sag2", "ugrp_missing"); err != ErrNotFound {
			t.Fatalf("set admin group with missing group = %v, want ErrNotFound", err)
		}

		// ServicesByAdminGroups([g1]) returns the service.
		byG1, err := s.ServicesByAdminGroups(ctx, []string{g1.ID})
		if err != nil {
			t.Fatalf("services by admin groups(g1): %v", err)
		}
		if len(byG1) != 1 || byG1[0].ID != "svc_sag2" {
			t.Fatalf("services by admin groups(g1) = %+v, want [svc_sag2]", byG1)
		}

		// ServicesByAdminGroups is deduped: a service linked to BOTH g1 and g2
		// still appears exactly once when both are queried.
		byBoth, err := s.ServicesByAdminGroups(ctx, []string{g1.ID, g2.ID})
		if err != nil {
			t.Fatalf("services by admin groups(g1,g2): %v", err)
		}
		if len(byBoth) != 1 || byBoth[0].ID != "svc_sag2" {
			t.Fatalf("services by admin groups(g1,g2) = %+v, want a single svc_sag2", byBoth)
		}

		// ServicesByAdminGroups([]) -> empty, no query issued.
		byEmpty, err := s.ServicesByAdminGroups(ctx, []string{})
		if err != nil || len(byEmpty) != 0 {
			t.Fatalf("services by admin groups([]) = %+v err=%v, want empty", byEmpty, err)
		}

		// RemoveServiceAdminGroup(svc,g1) leaves {g2}. A repeat remove is a
		// no-op (non-error).
		if err := s.RemoveServiceAdminGroup(ctx, "svc_sag2", g1.ID); err != nil {
			t.Fatalf("remove admin group g1: %v", err)
		}
		if err := s.RemoveServiceAdminGroup(ctx, "svc_sag2", g1.ID); err != nil {
			t.Fatalf("re-remove admin group g1 (no-op): %v", err)
		}
		groups, err = s.ServiceAdminGroups(ctx, "svc_sag2")
		if err != nil || len(groups) != 1 || groups[0] != g2.ID {
			t.Fatalf("service admin groups after remove = %v err=%v, want [%s]", groups, err, g2.ID)
		}

		// UpdateServiceSystemGroup round-trips via ServiceByID.
		if err := s.UpdateServiceSystemGroup(ctx, "svc_sag2", sysGroup.ID); err != nil {
			t.Fatalf("update service system group: %v", err)
		}
		got, err := s.ServiceByID(ctx, "svc_sag2")
		if err != nil {
			t.Fatalf("ServiceByID: %v", err)
		}
		if got.SystemGroupID != sysGroup.ID {
			t.Fatalf("ServiceByID SystemGroupID = %q, want %q", got.SystemGroupID, sysGroup.ID)
		}
		if err := s.UpdateServiceSystemGroup(ctx, "svc_sag2_missing", sysGroup.ID); err != ErrNotFound {
			t.Fatalf("update service system group on missing service = %v, want ErrNotFound", err)
		}

		// Re-link g1 so the delete-cascade assertions below exercise a
		// service with 2 groups (g1 re-added, g2 already present).
		if err := s.SetServiceAdminGroup(ctx, "svc_sag2", g1.ID); err != nil {
			t.Fatalf("re-link admin group g1: %v", err)
		}

		// Deleting group g2 (FK ON DELETE CASCADE) removes ITS
		// service_admin_groups row only — g1's link survives.
		if err := s.DeleteUserGroup(ctx, g2.ID); err != nil {
			t.Fatalf("delete group g2: %v", err)
		}
		groups, err = s.ServiceAdminGroups(ctx, "svc_sag2")
		if err != nil || len(groups) != 1 || groups[0] != g1.ID {
			t.Fatalf("service admin groups after group delete = %v err=%v, want [%s]", groups, err, g1.ID)
		}

		// Deleting the service (FK ON DELETE CASCADE) removes every
		// remaining service_admin_groups row for it.
		if err := s.DeleteService(ctx, "svc_sag2"); err != nil {
			t.Fatalf("delete service: %v", err)
		}
		groups, err = s.ServiceAdminGroups(ctx, "svc_sag2")
		if err != nil || len(groups) != 0 {
			t.Fatalf("service admin groups after service delete = %v err=%v, want empty", groups, err)
		}
	})
}

// TestConformanceResourceGroups is the RESOURCE-GROUPS analog of
// TestConformanceServerAdminGroups/TestConformanceServiceAdminGroups (Resource
// Groups Phase 1, spec 2026-08-11 — Task 2, migration v54). resource_groups is
// a brand-new entity carrying BOTH an admin-group management linkage
// (resource_group_admin_groups, mirroring service_admin_groups) AND a server
// membership linkage (resource_group_servers, an n:m set of AI-servers that
// belong to the resource group) plus the same system_group_id containment
// root every other Phase-B/C-linked entity carries.
func TestConformanceResourceGroups(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		if err := s.CreateUser(ctx, User{ID: "usr_rg_owner", Email: "rg-owner@x", DisplayName: "Owner", Role: "admin", Status: UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("create owner: %v", err)
		}
		sysGroup := UserGroup{ID: "ugrp_rg_sys", Tier: GroupTierSystem, Name: "RG System", CreatedAt: now, UpdatedAt: now}
		if err := s.CreateUserGroup(ctx, sysGroup); err != nil {
			t.Fatalf("create system group: %v", err)
		}
		g1 := UserGroup{ID: "ugrp_rg_a", Tier: GroupTierAdmin, Name: "RG Admin A", ParentGroupID: sysGroup.ID, OwnerUserID: "usr_rg_owner", CreatedAt: now, UpdatedAt: now}
		if err := s.CreateUserGroup(ctx, g1); err != nil {
			t.Fatalf("create admin group g1: %v", err)
		}
		g2 := UserGroup{ID: "ugrp_rg_b", Tier: GroupTierAdmin, Name: "RG Admin B", ParentGroupID: sysGroup.ID, OwnerUserID: "usr_rg_owner", CreatedAt: now, UpdatedAt: now}
		if err := s.CreateUserGroup(ctx, g2); err != nil {
			t.Fatalf("create admin group g2: %v", err)
		}

		for _, id := range []string{"srv_rg_1", "srv_rg_2", "srv_rg_3"} {
			if err := s.CreateAIServer(ctx, routing.AIServer{ID: id, Name: "RG Server " + id, Domain: id + ".local", Provider: routing.ProviderOllama, Endpoint: "http://" + id + ".local:11434", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
				t.Fatalf("create server %s: %v", id, err)
			}
		}

		rg := routing.ResourceGroup{ID: "rgrp_1", Name: "RG One", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}
		if err := s.CreateResourceGroup(ctx, rg); err != nil {
			t.Fatalf("create resource group: %v", err)
		}

		// ResourceGroupByID round-trips name/status/system_group_id (empty at
		// create).
		got, err := s.ResourceGroupByID(ctx, "rgrp_1")
		if err != nil {
			t.Fatalf("ResourceGroupByID: %v", err)
		}
		if got.Name != "RG One" || got.Status != routing.ServerStatusActive || got.SystemGroupID != "" {
			t.Fatalf("ResourceGroupByID = %+v, want name=RG One status=active system_group_id=''", got)
		}

		// CreateResourceGroup with a duplicate id -> ErrConflict.
		if err := s.CreateResourceGroup(ctx, rg); err != ErrConflict {
			t.Fatalf("create duplicate resource group = %v, want ErrConflict", err)
		}

		// ResourceGroups lists every resource group (sorted by id).
		all, err := s.ResourceGroups(ctx)
		if err != nil || len(all) != 1 || all[0].ID != "rgrp_1" {
			t.Fatalf("ResourceGroups = %+v err=%v, want [rgrp_1]", all, err)
		}

		// SetResourceGroupAdminGroup(rg,g1) + (rg,g2) -> {g1,g2}. A repeat
		// SetResourceGroupAdminGroup(rg,g1) hits the on-conflict-do-nothing path
		// (idempotent, no error, no duplicate row).
		if err := s.SetResourceGroupAdminGroup(ctx, "rgrp_1", g1.ID); err != nil {
			t.Fatalf("set admin group g1: %v", err)
		}
		if err := s.SetResourceGroupAdminGroup(ctx, "rgrp_1", g1.ID); err != nil {
			t.Fatalf("re-set admin group g1 (idempotent): %v", err)
		}
		if err := s.SetResourceGroupAdminGroup(ctx, "rgrp_1", g2.ID); err != nil {
			t.Fatalf("set admin group g2: %v", err)
		}
		groups, err := s.ResourceGroupAdminGroups(ctx, "rgrp_1")
		if err != nil {
			t.Fatalf("resource group admin groups: %v", err)
		}
		if len(groups) != 2 || groups[0] != g1.ID || groups[1] != g2.ID {
			t.Fatalf("resource group admin groups = %v, want [%s %s]", groups, g1.ID, g2.ID)
		}

		// A missing resource group -> ErrNotFound (FK violation); a missing
		// group -> ErrNotFound too.
		if err := s.SetResourceGroupAdminGroup(ctx, "rgrp_missing", g1.ID); err != ErrNotFound {
			t.Fatalf("set admin group on missing resource group = %v, want ErrNotFound", err)
		}
		if err := s.SetResourceGroupAdminGroup(ctx, "rgrp_1", "ugrp_missing"); err != ErrNotFound {
			t.Fatalf("set admin group with missing group = %v, want ErrNotFound", err)
		}

		// ResourceGroupsByAdminGroups([g1]) returns rgrp_1.
		byG1, err := s.ResourceGroupsByAdminGroups(ctx, []string{g1.ID})
		if err != nil {
			t.Fatalf("resource groups by admin groups(g1): %v", err)
		}
		if len(byG1) != 1 || byG1[0].ID != "rgrp_1" {
			t.Fatalf("resource groups by admin groups(g1) = %+v, want [rgrp_1]", byG1)
		}

		// ResourceGroupsByAdminGroups is deduped: linked to BOTH g1 and g2
		// still appears exactly once when both are queried.
		byBoth, err := s.ResourceGroupsByAdminGroups(ctx, []string{g1.ID, g2.ID})
		if err != nil {
			t.Fatalf("resource groups by admin groups(g1,g2): %v", err)
		}
		if len(byBoth) != 1 || byBoth[0].ID != "rgrp_1" {
			t.Fatalf("resource groups by admin groups(g1,g2) = %+v, want a single rgrp_1", byBoth)
		}

		// ResourceGroupsByAdminGroups([]) -> empty, no query issued.
		byEmpty, err := s.ResourceGroupsByAdminGroups(ctx, []string{})
		if err != nil || len(byEmpty) != 0 {
			t.Fatalf("resource groups by admin groups([]) = %+v err=%v, want empty", byEmpty, err)
		}

		// RemoveResourceGroupAdminGroup(rg,g1) leaves {g2}. A repeat remove is
		// a no-op (non-error).
		if err := s.RemoveResourceGroupAdminGroup(ctx, "rgrp_1", g1.ID); err != nil {
			t.Fatalf("remove admin group g1: %v", err)
		}
		if err := s.RemoveResourceGroupAdminGroup(ctx, "rgrp_1", g1.ID); err != nil {
			t.Fatalf("re-remove admin group g1 (no-op): %v", err)
		}
		groups, err = s.ResourceGroupAdminGroups(ctx, "rgrp_1")
		if err != nil || len(groups) != 1 || groups[0] != g2.ID {
			t.Fatalf("resource group admin groups after remove = %v err=%v, want [%s]", groups, err, g2.ID)
		}

		// SetResourceGroupServer(rg,s1) + (rg,s2) -> {s1,s2}.
		// ResourceGroupsByServer(s1) returns rg. A repeat set is idempotent.
		if err := s.SetResourceGroupServer(ctx, "rgrp_1", "srv_rg_1"); err != nil {
			t.Fatalf("set server s1: %v", err)
		}
		if err := s.SetResourceGroupServer(ctx, "rgrp_1", "srv_rg_1"); err != nil {
			t.Fatalf("re-set server s1 (idempotent): %v", err)
		}
		if err := s.SetResourceGroupServer(ctx, "rgrp_1", "srv_rg_2"); err != nil {
			t.Fatalf("set server s2: %v", err)
		}
		serverIDs, err := s.ResourceGroupServers(ctx, "rgrp_1")
		if err != nil {
			t.Fatalf("resource group servers: %v", err)
		}
		if len(serverIDs) != 2 || serverIDs[0] != "srv_rg_1" || serverIDs[1] != "srv_rg_2" {
			t.Fatalf("resource group servers = %v, want [srv_rg_1 srv_rg_2]", serverIDs)
		}
		byServer, err := s.ResourceGroupsByServer(ctx, "srv_rg_1")
		if err != nil {
			t.Fatalf("resource groups by server: %v", err)
		}
		if len(byServer) != 1 || byServer[0].ID != "rgrp_1" {
			t.Fatalf("resource groups by server = %+v, want [rgrp_1]", byServer)
		}

		// A missing resource group / missing server -> ErrNotFound (FK
		// violation) on SetResourceGroupServer.
		if err := s.SetResourceGroupServer(ctx, "rgrp_missing", "srv_rg_1"); err != ErrNotFound {
			t.Fatalf("set server on missing resource group = %v, want ErrNotFound", err)
		}
		if err := s.SetResourceGroupServer(ctx, "rgrp_1", "srv_rg_missing"); err != ErrNotFound {
			t.Fatalf("set server with missing server = %v, want ErrNotFound", err)
		}

		// RemoveResourceGroupServer(rg,s2) leaves {s1}. A repeat remove of an
		// already-absent link is a no-op (non-error).
		if err := s.RemoveResourceGroupServer(ctx, "rgrp_1", "srv_rg_2"); err != nil {
			t.Fatalf("remove server s2: %v", err)
		}
		if err := s.RemoveResourceGroupServer(ctx, "rgrp_1", "srv_rg_2"); err != nil {
			t.Fatalf("re-remove server s2 (no-op): %v", err)
		}
		serverIDs, err = s.ResourceGroupServers(ctx, "rgrp_1")
		if err != nil || len(serverIDs) != 1 || serverIDs[0] != "srv_rg_1" {
			t.Fatalf("resource group servers after remove = %v err=%v, want [srv_rg_1]", serverIDs, err)
		}

		// UpdateResourceGroupSystemGroup round-trips via ResourceGroupByID.
		if err := s.UpdateResourceGroupSystemGroup(ctx, "rgrp_1", sysGroup.ID); err != nil {
			t.Fatalf("update resource group system group: %v", err)
		}
		got, err = s.ResourceGroupByID(ctx, "rgrp_1")
		if err != nil {
			t.Fatalf("ResourceGroupByID: %v", err)
		}
		if got.SystemGroupID != sysGroup.ID {
			t.Fatalf("ResourceGroupByID SystemGroupID = %q, want %q", got.SystemGroupID, sysGroup.ID)
		}
		if err := s.UpdateResourceGroupSystemGroup(ctx, "rgrp_missing", sysGroup.ID); err != ErrNotFound {
			t.Fatalf("update resource group system group on missing resource group = %v, want ErrNotFound", err)
		}

		// UpdateResourceGroup changes name/status but never created_at, and
		// never touches system_group_id (mirrors UpdateService/UpdateAIServer
		// not overwriting a targeted-UPDATE-only column via the full-row path
		// — here system_group_id is set via UpdateResourceGroupSystemGroup
		// only, so UpdateResourceGroup must preserve it too).
		if err := s.UpdateResourceGroup(ctx, routing.ResourceGroup{ID: "rgrp_1", Name: "RG One Renamed", Status: routing.ServerStatusDisabled, CreatedAt: now.Add(time.Hour), UpdatedAt: now.Add(time.Minute)}); err != nil {
			t.Fatalf("update resource group: %v", err)
		}
		got, err = s.ResourceGroupByID(ctx, "rgrp_1")
		if err != nil {
			t.Fatalf("ResourceGroupByID after update: %v", err)
		}
		if got.Name != "RG One Renamed" || got.Status != routing.ServerStatusDisabled {
			t.Fatalf("ResourceGroupByID after update = %+v, want name=RG One Renamed status=disabled", got)
		}
		if !got.CreatedAt.Equal(now) {
			t.Fatalf("ResourceGroupByID CreatedAt after update = %v, want unchanged %v", got.CreatedAt, now)
		}
		if err := s.UpdateResourceGroup(ctx, routing.ResourceGroup{ID: "rgrp_missing", Name: "x", Status: routing.ServerStatusActive, UpdatedAt: now}); err != ErrNotFound {
			t.Fatalf("update missing resource group = %v, want ErrNotFound", err)
		}

		// Re-link g1 and s3 so the delete-cascade assertions below exercise a
		// resource group with 2 admin-group links (g1 re-added, g2 already
		// present) and 2 server links (s1 already present, s3 newly added).
		if err := s.SetResourceGroupAdminGroup(ctx, "rgrp_1", g1.ID); err != nil {
			t.Fatalf("re-link admin group g1: %v", err)
		}
		if err := s.SetResourceGroupServer(ctx, "rgrp_1", "srv_rg_3"); err != nil {
			t.Fatalf("link server s3: %v", err)
		}

		// Deleting group g2 (FK ON DELETE CASCADE) removes ITS
		// resource_group_admin_groups row only — g1's link survives, and the
		// server links are untouched.
		if err := s.DeleteUserGroup(ctx, g2.ID); err != nil {
			t.Fatalf("delete group g2: %v", err)
		}
		groups, err = s.ResourceGroupAdminGroups(ctx, "rgrp_1")
		if err != nil || len(groups) != 1 || groups[0] != g1.ID {
			t.Fatalf("resource group admin groups after group delete = %v err=%v, want [%s]", groups, err, g1.ID)
		}
		serverIDs, err = s.ResourceGroupServers(ctx, "rgrp_1")
		if err != nil || len(serverIDs) != 2 || serverIDs[0] != "srv_rg_1" || serverIDs[1] != "srv_rg_3" {
			t.Fatalf("resource group servers after group delete = %v err=%v, want [srv_rg_1 srv_rg_3] (untouched)", serverIDs, err)
		}

		// Deleting server s1 (FK ON DELETE CASCADE) removes ITS
		// resource_group_servers row only — s3's link survives, and the
		// admin-group link is untouched.
		if err := s.DeleteAIServer(ctx, "srv_rg_1"); err != nil {
			t.Fatalf("delete server s1: %v", err)
		}
		serverIDs, err = s.ResourceGroupServers(ctx, "rgrp_1")
		if err != nil || len(serverIDs) != 1 || serverIDs[0] != "srv_rg_3" {
			t.Fatalf("resource group servers after server delete = %v err=%v, want [srv_rg_3]", serverIDs, err)
		}
		groups, err = s.ResourceGroupAdminGroups(ctx, "rgrp_1")
		if err != nil || len(groups) != 1 || groups[0] != g1.ID {
			t.Fatalf("resource group admin groups after server delete = %v err=%v, want [%s] (untouched)", groups, err, g1.ID)
		}

		// Deleting the resource group (FK ON DELETE CASCADE, both join
		// tables) removes every remaining resource_group_admin_groups AND
		// resource_group_servers row for it.
		if err := s.DeleteResourceGroup(ctx, "rgrp_1"); err != nil {
			t.Fatalf("delete resource group: %v", err)
		}
		groups, err = s.ResourceGroupAdminGroups(ctx, "rgrp_1")
		if err != nil || len(groups) != 0 {
			t.Fatalf("resource group admin groups after resource group delete = %v err=%v, want empty", groups, err)
		}
		serverIDs, err = s.ResourceGroupServers(ctx, "rgrp_1")
		if err != nil || len(serverIDs) != 0 {
			t.Fatalf("resource group servers after resource group delete = %v err=%v, want empty", serverIDs, err)
		}
		if _, err := s.ResourceGroupByID(ctx, "rgrp_1"); err != ErrNotFound {
			t.Fatalf("ResourceGroupByID after delete = %v, want ErrNotFound", err)
		}
		if err := s.DeleteResourceGroup(ctx, "rgrp_1"); err != ErrNotFound {
			t.Fatalf("delete missing resource group = %v, want ErrNotFound", err)
		}
	})
}

// TestConformanceResourceGroupProvisions is the PROVISIONING analog of
// TestConformanceResourceGroups (Resource Groups Phase 2, spec 2026-08-12 —
// Task 1, migration v55, resource_group_provisions). Unlike
// resource_group_admin_groups (management) and resource_group_servers
// (membership) — both FK'd to a single well-known table — a provision's
// target_id is POLYMORPHIC (no FK; its meaning depends on target_kind), so
// this exercises the idempotent set/remove, the atomic all-or-nothing
// replace, the (kind, target)-ordered listing, the reverse lookup by target,
// the fleet-wide "has any provision" set, and the FK-ON-DELETE-CASCADE from
// resource_groups.
func TestConformanceResourceGroupProvisions(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)

		rg := routing.ResourceGroup{ID: "rgrp_prov_1", Name: "RG Prov One", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}
		if err := s.CreateResourceGroup(ctx, rg); err != nil {
			t.Fatalf("create resource group: %v", err)
		}
		other := routing.ResourceGroup{ID: "rgrp_prov_2", Name: "RG Prov Two", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}
		if err := s.CreateResourceGroup(ctx, other); err != nil {
			t.Fatalf("create second resource group: %v", err)
		}

		// SetResourceGroupProvision is idempotent: setting the exact same
		// (kind, target) twice leaves exactly ONE row.
		if err := s.SetResourceGroupProvision(ctx, rg.ID, routing.ProvisionKindUser, "usr_1"); err != nil {
			t.Fatalf("set provision: %v", err)
		}
		if err := s.SetResourceGroupProvision(ctx, rg.ID, routing.ProvisionKindUser, "usr_1"); err != nil {
			t.Fatalf("set provision idempotent: %v", err)
		}
		got, err := s.ResourceGroupProvisions(ctx, rg.ID)
		if err != nil {
			t.Fatalf("resource group provisions: %v", err)
		}
		if len(got) != 1 || got[0] != (routing.ResourceGroupProvision{Kind: routing.ProvisionKindUser, TargetID: "usr_1"}) {
			t.Fatalf("resource group provisions = %+v, want exactly one user:usr_1", got)
		}

		// Adding a SECOND kind yields 2 rows, sorted by (kind, target) —
		// "admin_group" < "user" lexicographically.
		if err := s.SetResourceGroupProvision(ctx, rg.ID, routing.ProvisionKindAdminGroup, "ugrp_1"); err != nil {
			t.Fatalf("set second provision: %v", err)
		}
		got, err = s.ResourceGroupProvisions(ctx, rg.ID)
		if err != nil {
			t.Fatalf("resource group provisions after second set: %v", err)
		}
		want := []routing.ResourceGroupProvision{
			{Kind: routing.ProvisionKindAdminGroup, TargetID: "ugrp_1"},
			{Kind: routing.ProvisionKindUser, TargetID: "usr_1"},
		}
		if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("resource group provisions sorted = %+v, want %+v", got, want)
		}

		// A missing resource group -> ErrNotFound (FK violation on
		// resource_group_id). target_id itself is polymorphic — no FK to
		// violate on that side.
		if err := s.SetResourceGroupProvision(ctx, "rgrp_prov_missing", routing.ProvisionKindUser, "usr_1"); err != ErrNotFound {
			t.Fatalf("set provision on missing resource group = %v, want ErrNotFound", err)
		}

		// ResourceGroupIDsByProvisionTargets(user, [usr_1]) finds rg.
		rgs, err := s.ResourceGroupIDsByProvisionTargets(ctx, routing.ProvisionKindUser, []string{"usr_1"})
		if err != nil {
			t.Fatalf("resource group ids by provision targets: %v", err)
		}
		if len(rgs) != 1 || rgs[0] != rg.ID {
			t.Fatalf("resource group ids by provision targets(user,[usr_1]) = %v, want [%s]", rgs, rg.ID)
		}

		// An unrelated target id -> no match (empty, not an error).
		none, err := s.ResourceGroupIDsByProvisionTargets(ctx, routing.ProvisionKindUser, []string{"nope"})
		if err != nil || len(none) != 0 {
			t.Fatalf("resource group ids by provision targets(user,[nope]) = %v err=%v, want empty", none, err)
		}

		// The SAME target id under a DIFFERENT kind does not match — kind is
		// part of the lookup key.
		wrongKind, err := s.ResourceGroupIDsByProvisionTargets(ctx, routing.ProvisionKindService, []string{"usr_1"})
		if err != nil || len(wrongKind) != 0 {
			t.Fatalf("resource group ids by provision targets(service,[usr_1]) = %v err=%v, want empty", wrongKind, err)
		}

		// An empty targetIDs -> empty, without issuing a query.
		empty, err := s.ResourceGroupIDsByProvisionTargets(ctx, routing.ProvisionKindUser, nil)
		if err != nil || len(empty) != 0 {
			t.Fatalf("resource group ids by provision targets(user,[]) = %v err=%v, want empty", empty, err)
		}

		// ProvisionedResourceGroupIDs includes rg (has provisions) but NOT
		// other (has none yet).
		prov, err := s.ProvisionedResourceGroupIDs(ctx)
		if err != nil {
			t.Fatalf("provisioned resource group ids: %v", err)
		}
		if !prov[rg.ID] {
			t.Fatalf("provisioned resource group ids = %v, want %s present", prov, rg.ID)
		}
		if prov[other.ID] {
			t.Fatalf("provisioned resource group ids = %v, want %s absent", prov, other.ID)
		}

		// RemoveResourceGroupProvision drops exactly the targeted pair,
		// leaving the other. A repeat remove of an already-absent link is a
		// no-op (non-error).
		if err := s.RemoveResourceGroupProvision(ctx, rg.ID, routing.ProvisionKindAdminGroup, "ugrp_1"); err != nil {
			t.Fatalf("remove provision: %v", err)
		}
		if err := s.RemoveResourceGroupProvision(ctx, rg.ID, routing.ProvisionKindAdminGroup, "ugrp_1"); err != nil {
			t.Fatalf("re-remove provision (no-op): %v", err)
		}
		got, err = s.ResourceGroupProvisions(ctx, rg.ID)
		if err != nil || len(got) != 1 || got[0] != (routing.ResourceGroupProvision{Kind: routing.ProvisionKindUser, TargetID: "usr_1"}) {
			t.Fatalf("resource group provisions after remove = %+v err=%v, want exactly one user:usr_1", got, err)
		}

		// SetResourceGroupProvisions(rg, []) clears the whole set.
		if err := s.SetResourceGroupProvisions(ctx, rg.ID, nil); err != nil {
			t.Fatalf("clear provisions: %v", err)
		}
		got, err = s.ResourceGroupProvisions(ctx, rg.ID)
		if err != nil || len(got) != 0 {
			t.Fatalf("resource group provisions after clear = %+v err=%v, want empty", got, err)
		}
		prov, err = s.ProvisionedResourceGroupIDs(ctx)
		if err != nil {
			t.Fatalf("provisioned resource group ids after clear: %v", err)
		}
		if prov[rg.ID] {
			t.Fatalf("provisioned resource group ids after clear = %v, want %s absent", prov, rg.ID)
		}

		// SetResourceGroupProvisions(rg, [a,b]) REPLACES the (now-empty) set
		// atomically. A duplicate pair within the input collapses to one row.
		replacement := []routing.ResourceGroupProvision{
			{Kind: routing.ProvisionKindUserGroup, TargetID: "grp_a"},
			{Kind: routing.ProvisionKindService, TargetID: "svc_b"},
			{Kind: routing.ProvisionKindService, TargetID: "svc_b"}, // duplicate, should collapse
		}
		if err := s.SetResourceGroupProvisions(ctx, rg.ID, replacement); err != nil {
			t.Fatalf("replace provisions: %v", err)
		}
		got, err = s.ResourceGroupProvisions(ctx, rg.ID)
		if err != nil {
			t.Fatalf("resource group provisions after replace: %v", err)
		}
		wantReplaced := []routing.ResourceGroupProvision{
			{Kind: routing.ProvisionKindService, TargetID: "svc_b"},
			{Kind: routing.ProvisionKindUserGroup, TargetID: "grp_a"},
		}
		if len(got) != 2 || got[0] != wantReplaced[0] || got[1] != wantReplaced[1] {
			t.Fatalf("resource group provisions after replace = %+v, want %+v", got, wantReplaced)
		}

		// SetResourceGroupProvisions on a missing resource group -> ErrNotFound.
		if err := s.SetResourceGroupProvisions(ctx, "rgrp_prov_missing", nil); err != ErrNotFound {
			t.Fatalf("replace provisions on missing resource group = %v, want ErrNotFound", err)
		}

		// Deleting the resource group (FK ON DELETE CASCADE) removes every
		// remaining resource_group_provisions row for it, and it drops out of
		// ProvisionedResourceGroupIDs.
		if err := s.DeleteResourceGroup(ctx, rg.ID); err != nil {
			t.Fatalf("delete resource group: %v", err)
		}
		after, err := s.ResourceGroupProvisions(ctx, rg.ID)
		if err != nil || len(after) != 0 {
			t.Fatalf("resource group provisions after resource group delete = %+v err=%v, want empty", after, err)
		}
		prov, err = s.ProvisionedResourceGroupIDs(ctx)
		if err != nil {
			t.Fatalf("provisioned resource group ids after resource group delete: %v", err)
		}
		if prov[rg.ID] {
			t.Fatalf("provisioned resource group ids after resource group delete = %v, want %s absent", prov, rg.ID)
		}
	})
}

func TestConformanceCertificates(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		if _, err := s.CertificateByDomain(ctx, "nope.example.test"); err == nil {
			t.Fatal("expected ErrNotFound for an unknown domain")
		}
		issued := time.Now().UTC().Truncate(time.Second)
		// Every time.Time field below gets its OWN distinct offset from `issued`
		// (and fullchain_pem/key_sealed/server_id are distinct strings) so that a
		// dropped or transposed column -- e.g. next_attempt_at scanned into
		// not_before, or fullchain_pem swapped with key_sealed -- fails a
		// round-trip assertion instead of silently reading back a value that
		// happens to equal a sibling column's value.
		cert := routing.Certificate{
			Domain:            "gw.int.example.test",
			Kind:              "gateway",
			FullchainPEM:      "-----BEGIN CERTIFICATE-----\nFULLCHAIN\n-----END CERTIFICATE-----\n",
			KeySealed:         "enc:c2VhbGVk",
			Fingerprint:       "ab12",
			IssuerFingerprint: "cd34",
			NotBefore:         issued.Add(-2 * time.Minute),
			NotAfter:          issued.Add(90 * 24 * time.Hour),
			IssuedAt:          issued.Add(-30 * time.Second),
			Status:            "active",
			CreatedAt:         issued,
			UpdatedAt:         issued,
		}
		if err := s.UpsertCertificate(ctx, cert); err != nil {
			t.Fatal(err)
		}
		got, err := s.CertificateByDomain(ctx, "gw.int.example.test")
		if err != nil {
			t.Fatal(err)
		}
		if got.Kind != "gateway" || got.KeySealed != "enc:c2VhbGVk" || got.Status != "active" {
			t.Fatalf("round trip mismatch: %+v", got)
		}
		if got.IssuerFingerprint != "cd34" {
			t.Fatalf("issuer_fingerprint = %q, want cd34 (column-ordering guard)", got.IssuerFingerprint)
		}
		if got.FullchainPEM != cert.FullchainPEM {
			t.Fatalf("fullchain_pem = %q, want %q (column-ordering guard vs key_sealed)", got.FullchainPEM, cert.FullchainPEM)
		}
		if !got.NotAfter.UTC().Equal(cert.NotAfter) {
			t.Fatalf("not_after = %v, want %v", got.NotAfter.UTC(), cert.NotAfter)
		}
		if !got.NotBefore.UTC().Equal(cert.NotBefore) {
			t.Fatalf("not_before = %v, want %v (column-ordering guard vs not_after)", got.NotBefore.UTC(), cert.NotBefore)
		}
		if !got.IssuedAt.UTC().Equal(cert.IssuedAt) {
			t.Fatalf("issued_at = %v, want %v (column-ordering guard vs not_before/created_at)", got.IssuedAt.UTC(), cert.IssuedAt)
		}
		// Upsert on the same domain REPLACES (no duplicate row) and keeps created_at.
		cert.Status = "error"
		cert.LastError = "boom"
		cert.AttemptCount = 3
		cert.NextAttemptAt = issued.Add(time.Hour)
		cert.UpdatedAt = issued.Add(time.Minute)
		if err := s.UpsertCertificate(ctx, cert); err != nil {
			t.Fatal(err)
		}
		all, err := s.Certificates(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(all) != 1 || all[0].Status != "error" || all[0].AttemptCount != 3 || all[0].LastError != "boom" {
			t.Fatalf("Certificates() = %+v, want exactly one updated row", all)
		}
		if !all[0].CreatedAt.UTC().Equal(issued) {
			t.Fatalf("created_at must survive an upsert, got %v", all[0].CreatedAt.UTC())
		}
		if !all[0].NextAttemptAt.UTC().Equal(cert.NextAttemptAt) {
			t.Fatalf("next_attempt_at = %v, want %v (column-ordering guard: a dropped backoff column "+
				"would silently retry a failed domain on every pass)", all[0].NextAttemptAt.UTC(), cert.NextAttemptAt)
		}
		// A zero-time row (skipped, never issued) round-trips as zero, not as an error.
		if err := s.UpsertCertificate(ctx, routing.Certificate{
			Domain: "outside.example.org", Kind: "server", Status: "skipped",
			LastError: "not under base domain", CreatedAt: issued, UpdatedAt: issued,
		}); err != nil {
			t.Fatal(err)
		}
		skipped, err := s.CertificateByDomain(ctx, "outside.example.org")
		if err != nil {
			t.Fatal(err)
		}
		if !skipped.NotAfter.IsZero() || skipped.Status != "skipped" {
			t.Fatalf("skipped row = %+v, want zero not_after", skipped)
		}
		// server_id FK cascade: deleting the server removes its certificate row.
		srv := routing.AIServer{ID: "srv-cert-1", Name: "srv", Domain: "s1.int.example.test", Provider: "vllm", Status: "active", CreatedAt: issued, UpdatedAt: issued}
		if err := s.CreateAIServer(ctx, srv); err != nil {
			t.Fatal(err)
		}
		if err := s.UpsertCertificate(ctx, routing.Certificate{
			Domain: "s1.int.example.test", Kind: "server", ServerID: srv.ID, Status: "pending", CreatedAt: issued, UpdatedAt: issued,
		}); err != nil {
			t.Fatal(err)
		}
		bySrv, err := s.CertificateByServer(ctx, srv.ID)
		if err != nil {
			t.Fatalf("CertificateByServer: %v", err)
		}
		if bySrv.ServerID != srv.ID {
			t.Fatalf("server_id = %q, want %q (column-ordering guard)", bySrv.ServerID, srv.ID)
		}
		if bySrv.Domain != "s1.int.example.test" {
			t.Fatalf("CertificateByServer returned domain %q, want s1.int.example.test", bySrv.Domain)
		}
		// F1.4: there is no unique constraint on server_id, so a second row can
		// end up linked to the same server. CertificateByServer must pick the
		// SAME row deterministically -- the lowest domain -- every time, not
		// whichever row the engine happens to return first.
		if err := s.UpsertCertificate(ctx, routing.Certificate{
			Domain: "a0.int.example.test", Kind: "server", ServerID: srv.ID, Status: "pending", CreatedAt: issued, UpdatedAt: issued,
		}); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 3; i++ {
			// a0.int.example.test sorts BEFORE s1.int.example.test but was
			// inserted SECOND -- insertion order and domain order disagree on
			// purpose, so a missing ORDER BY (which would return whatever the
			// engine's natural scan order happens to be, i.e. insertion order)
			// is caught rather than accidentally matching by coincidence.
			again, err := s.CertificateByServer(ctx, srv.ID)
			if err != nil {
				t.Fatalf("CertificateByServer with two rows linked to %s: %v", srv.ID, err)
			}
			if again.Domain != "a0.int.example.test" {
				t.Fatalf("CertificateByServer call %d with two rows linked to the same server = %q, want the lowest domain a0.int.example.test (nondeterministic pick)", i, again.Domain)
			}
		}
		if err := s.DeleteCertificate(ctx, "a0.int.example.test"); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteAIServer(ctx, srv.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CertificateByDomain(ctx, "s1.int.example.test"); err == nil {
			t.Fatal("expected the certificate row to cascade away with its server")
		}
		if err := s.DeleteCertificate(ctx, "gw.int.example.test"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CertificateByDomain(ctx, "gw.int.example.test"); err == nil {
			t.Fatal("expected ErrNotFound after DeleteCertificate")
		}
	})
}

func TestConformanceServerCertificateOverride(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		srv := routing.AIServer{
			ID: "srv-co-1", Name: "co", Domain: "co.int.example.test", Provider: "vllm",
			Status: "active", CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateAIServer(ctx, srv); err != nil {
			t.Fatal(err)
		}
		got, err := s.AIServerByID(ctx, srv.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.CertificateOverride != "" {
			t.Fatalf("fresh server CertificateOverride = %q, want empty", got.CertificateOverride)
		}
		// The narrow writer touches ONLY this column.
		if err := s.UpdateServerCertificateOverride(ctx, srv.ID, "include"); err != nil {
			t.Fatal(err)
		}
		got, err = s.AIServerByID(ctx, srv.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.CertificateOverride != "include" {
			t.Fatalf("CertificateOverride = %q, want include", got.CertificateOverride)
		}
		if got.Name != "co" || got.Domain != "co.int.example.test" || got.Provider != "vllm" {
			t.Fatalf("narrow writer clobbered other columns: %+v", got)
		}
		// A full UpdateAIServer round-trips the value (column-ordering guard).
		got.CertificateOverride = "exclude"
		got.Name = "co2"
		if err := s.UpdateAIServer(ctx, got); err != nil {
			t.Fatal(err)
		}
		back, err := s.AIServerByID(ctx, srv.ID)
		if err != nil {
			t.Fatal(err)
		}
		if back.CertificateOverride != "exclude" || back.Name != "co2" {
			t.Fatalf("round trip = %+v, want exclude/co2", back)
		}
		// The list query selects the column too.
		all, err := s.AIServers(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var found bool
		for _, sv := range all {
			if sv.ID == srv.ID {
				found = true
				if sv.CertificateOverride != "exclude" {
					t.Fatalf("AIServers() override = %q, want exclude", sv.CertificateOverride)
				}
			}
		}
		if !found {
			t.Fatal("server missing from AIServers()")
		}
		if err := s.UpdateServerCertificateOverride(ctx, "does-not-exist", "include"); err == nil {
			t.Fatal("expected ErrNotFound for an unknown server")
		}
	})
}

// TestConformanceServerHTTPSSwitchOverride is TestConformanceServerCertificateOverride's
// P4 analogue for ai_servers.https_switch_override.
func TestConformanceServerHTTPSSwitchOverride(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		srv := routing.AIServer{
			ID: "srv-hso-1", Name: "hso", Domain: "hso.int.example.test", Provider: "vllm",
			Status: "active", CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateAIServer(ctx, srv); err != nil {
			t.Fatal(err)
		}
		got, err := s.AIServerByID(ctx, srv.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.HTTPSSwitchOverride != "" {
			t.Fatalf("fresh server HTTPSSwitchOverride = %q, want empty", got.HTTPSSwitchOverride)
		}
		// The narrow writer touches ONLY this column.
		if err := s.UpdateServerHTTPSSwitchOverride(ctx, srv.ID, "include"); err != nil {
			t.Fatal(err)
		}
		got, err = s.AIServerByID(ctx, srv.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.HTTPSSwitchOverride != "include" {
			t.Fatalf("HTTPSSwitchOverride = %q, want include", got.HTTPSSwitchOverride)
		}
		if got.Name != "hso" || got.Domain != "hso.int.example.test" || got.Provider != "vllm" {
			t.Fatalf("narrow writer clobbered other columns: %+v", got)
		}
		// A full UpdateAIServer round-trips the value (column-ordering guard).
		got.HTTPSSwitchOverride = "exclude"
		got.Name = "hso2"
		if err := s.UpdateAIServer(ctx, got); err != nil {
			t.Fatal(err)
		}
		back, err := s.AIServerByID(ctx, srv.ID)
		if err != nil {
			t.Fatal(err)
		}
		if back.HTTPSSwitchOverride != "exclude" || back.Name != "hso2" {
			t.Fatalf("round trip = %+v, want exclude/hso2", back)
		}
		// The list query selects the column too.
		all, err := s.AIServers(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var found bool
		for _, sv := range all {
			if sv.ID == srv.ID {
				found = true
				if sv.HTTPSSwitchOverride != "exclude" {
					t.Fatalf("AIServers() override = %q, want exclude", sv.HTTPSSwitchOverride)
				}
			}
		}
		if !found {
			t.Fatal("server missing from AIServers()")
		}
		// The owner-scoped list query selects the same shared column too:
		// ServersByOwner (and ServersByAdminGroups, which shares scanAIServers with
		// it) uses a scoped SELECT distinct from AIServers(), so a column-list
		// divergence there would drop the override and be caught here.
		if err := s.CreateUser(ctx, newTestUser("usr_hso", "hso@example.test", now)); err != nil {
			t.Fatal(err)
		}
		if err := s.SetServerOwners(ctx, srv.ID, []string{"usr_hso"}); err != nil {
			t.Fatal(err)
		}
		owned, err := s.ServersByOwner(ctx, "usr_hso")
		if err != nil {
			t.Fatal(err)
		}
		var carried bool
		for _, sv := range owned {
			if sv.ID == srv.ID {
				carried = sv.HTTPSSwitchOverride == "exclude"
			}
		}
		if !carried {
			t.Fatalf("ServersByOwner did not carry https_switch_override: %+v", owned)
		}
		if err := s.UpdateServerHTTPSSwitchOverride(ctx, "does-not-exist", "include"); err == nil {
			t.Fatal("expected ErrNotFound for an unknown server")
		}
	})
}
