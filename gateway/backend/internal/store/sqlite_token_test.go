// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"database/sql"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

func TestSQLiteCreatePlainTokenStoresOnlyHash(t *testing.T) {
	ctx := context.Background()
	st := openTokenTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC)

	if err := st.CreatePlainToken(ctx, testTokenRecord(now), "plain-secret"); err != nil {
		t.Fatalf("CreatePlainToken returned %v", err)
	}

	var secretHash string
	if err := st.db.QueryRow(`select secret_hash from api_tokens where id = ?`, "tok_1").Scan(&secretHash); err != nil {
		t.Fatalf("QueryRow returned %v", err)
	}
	if secretHash == "plain-secret" {
		t.Fatalf("secret_hash stored plaintext secret")
	}
	if secretHash != auth.HashSecret("plain-secret") {
		t.Fatalf("secret_hash = %q, want %q", secretHash, auth.HashSecret("plain-secret"))
	}
}

func TestSQLiteLookupBearerFindsActiveToken(t *testing.T) {
	ctx := context.Background()
	st := openTokenTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC)

	if err := st.CreatePlainToken(ctx, testTokenRecord(now), "plain-secret"); err != nil {
		t.Fatalf("CreatePlainToken returned %v", err)
	}

	token, ok := st.LookupBearer("Bearer plain-secret")

	if !ok {
		t.Fatalf("LookupBearer returned ok=false")
	}
	if token.ID != "tok_1" || token.UserID != "usr_1" || token.Name != "Dev Token" {
		t.Fatalf("token = %#v", token)
	}
	if !token.Active {
		t.Fatalf("token.Active = false, want true")
	}
}

func TestSQLiteLookupBearerReturnsScopes(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	if err := st.CreateUser(ctx, User{
		ID:                "usr_scope",
		Email:             "scope@example.test",
		DisplayName:       "Scope User",
		Role:              "admin",
		Status:            UserStatusActive,
		PreferredLanguage: "de",
		CreatedAt:         now,
		UpdatedAt:         now,
	}); err != nil {
		t.Fatalf("CreateUser returned %v", err)
	}
	if err := st.CreatePlainToken(ctx, TokenRecord{
		ID:        "tok_scope",
		UserID:    "usr_scope",
		Name:      "Scope Token",
		Status:    TokenStatusActive,
		Scopes:    `["gateway:use","admin"]`,
		CreatedAt: now,
		UpdatedAt: now,
	}, "scope-secret"); err != nil {
		t.Fatalf("CreatePlainToken returned %v", err)
	}

	token, ok := st.LookupBearer("Bearer scope-secret")

	if !ok {
		t.Fatalf("LookupBearer returned ok=false")
	}
	if !token.HasScope("gateway:use") || !token.HasScope("admin") {
		t.Fatalf("Scopes = %#v, want gateway:use and admin", token.Scopes)
	}
}

func TestSQLiteLookupBearerRejectsDisabledToken(t *testing.T) {
	ctx := context.Background()
	st := openTokenTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC)
	record := testTokenRecord(now)
	record.Status = TokenStatusDisabled

	if err := st.CreatePlainToken(ctx, record, "plain-secret"); err != nil {
		t.Fatalf("CreatePlainToken returned %v", err)
	}

	_, ok := st.LookupBearer("Bearer plain-secret")

	if ok {
		t.Fatalf("LookupBearer returned ok=true for disabled token")
	}
}

func TestSQLiteLookupBearerRejectsExpiredToken(t *testing.T) {
	ctx := context.Background()
	st := openTokenTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC)
	expiredAt := time.Now().Add(-time.Hour).UTC()
	record := testTokenRecord(now)
	record.ExpiresAt = &expiredAt

	if err := st.CreatePlainToken(ctx, record, "plain-secret"); err != nil {
		t.Fatalf("CreatePlainToken returned %v", err)
	}

	_, ok := st.LookupBearer("Bearer plain-secret")

	if ok {
		t.Fatalf("LookupBearer returned ok=true for expired token")
	}
}

func TestSQLiteLookupBearerUpdatesLastUsedAt(t *testing.T) {
	ctx := context.Background()
	st := openTokenTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC)

	if err := st.CreatePlainToken(ctx, testTokenRecord(now), "plain-secret"); err != nil {
		t.Fatalf("CreatePlainToken returned %v", err)
	}
	before := time.Now().UTC().Add(-time.Second)

	if _, ok := st.LookupBearer("Bearer plain-secret"); !ok {
		t.Fatalf("LookupBearer returned ok=false")
	}

	record, err := st.TokenByID(ctx, "tok_1")
	if err != nil {
		t.Fatalf("TokenByID returned %v", err)
	}
	if record.LastUsedAt == nil {
		t.Fatalf("LastUsedAt = nil")
	}
	if record.LastUsedAt.Before(before) {
		t.Fatalf("LastUsedAt = %s, want after %s", record.LastUsedAt, before)
	}
}

func TestSQLiteTokensByUserReturnsOnlyUserTokensNewestFirst(t *testing.T) {
	ctx := context.Background()
	st := openTokenTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC)
	first := testTokenRecord(now)
	first.ID = "tok_first"
	first.Name = "First"
	first.CreatedAt = now
	first.UpdatedAt = now
	second := testTokenRecord(now)
	second.ID = "tok_second"
	second.Name = "Second"
	second.CreatedAt = now.Add(time.Minute)
	second.UpdatedAt = second.CreatedAt
	other := testTokenRecord(now)
	other.ID = "tok_other"
	other.UserID = "usr_other"
	other.Name = "Other"

	if err := st.CreateUser(ctx, User{ID: "usr_other", Email: "other@example.test", DisplayName: "Other", Role: "user", Status: UserStatusActive, PreferredLanguage: "en", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateUser other returned %v", err)
	}
	if err := st.CreatePlainToken(ctx, first, "first-secret"); err != nil {
		t.Fatalf("CreatePlainToken first returned %v", err)
	}
	if err := st.CreatePlainToken(ctx, second, "second-secret"); err != nil {
		t.Fatalf("CreatePlainToken second returned %v", err)
	}
	if err := st.CreatePlainToken(ctx, other, "other-secret"); err != nil {
		t.Fatalf("CreatePlainToken other returned %v", err)
	}

	records, err := st.TokensByUser(ctx, "usr_1")
	if err != nil {
		t.Fatalf("TokensByUser returned %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("tokens = %d, want 2", len(records))
	}
	if records[0].ID != "tok_second" || records[1].ID != "tok_first" {
		t.Fatalf("token order = %#v", records)
	}
	if records[0].SecretHash == "" || records[0].SecretPrefix == "" {
		t.Fatalf("token secret metadata missing: %#v", records[0])
	}
}

func TestSQLiteCreatePlainTokenRejectsMissingUserOnNewConnection(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()
	st.db.SetMaxOpenConns(2)
	conn, err := st.db.Conn(ctx)
	if err != nil {
		t.Fatalf("Conn returned %v", err)
	}
	defer conn.Close()

	now := time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC)
	record := testTokenRecord(now)
	record.UserID = "usr_missing"

	err = st.CreatePlainToken(ctx, record, "plain-secret")

	// A missing user is a FOREIGN KEY violation (the pragma must be active on the
	// fresh connection). It maps to ErrNotFound — the referenced row is missing —
	// consistently on both dialects (an FK violation is not a unique/ErrConflict).
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("CreatePlainToken error = %v, want ErrNotFound", err)
	}
}

func TestSQLiteStoreImplementsAuthBearerStore(t *testing.T) {
	var _ auth.BearerStore = (*SQLiteStore)(nil)
}

func TestSQLiteUpdateTokenMetadataPersistsAndAffectsLookup(t *testing.T) {
	ctx := context.Background()
	st := openTokenTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC)
	if err := st.CreatePlainToken(ctx, testTokenRecord(now), "plain-secret"); err != nil {
		t.Fatalf("CreatePlainToken returned %v", err)
	}

	updated := testTokenRecord(now)
	updated.Name = "Renamed"
	updated.Scopes = `["gateway:use","admin"]`
	updated.Status = TokenStatusDisabled
	updated.UpdatedAt = now.Add(time.Minute)
	if err := st.UpdateTokenMetadata(ctx, updated); err != nil {
		t.Fatalf("UpdateTokenMetadata returned %v", err)
	}

	record, err := st.TokenByID(ctx, "tok_1")
	if err != nil {
		t.Fatalf("TokenByID returned %v", err)
	}
	if record.Name != "Renamed" || record.Status != TokenStatusDisabled || record.Scopes != `["gateway:use","admin"]` {
		t.Fatalf("record = %#v", record)
	}
	if _, ok := st.LookupBearer("Bearer plain-secret"); ok {
		t.Fatalf("disabled token should not authenticate")
	}
}

func TestSQLiteUpdateTokenMetadataUnknownIDReturnsNotFound(t *testing.T) {
	ctx := context.Background()
	st := openTokenTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC)
	record := testTokenRecord(now)
	record.ID = "tok_missing"

	err := st.UpdateTokenMetadata(ctx, record)

	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateTokenMetadata error = %v, want ErrNotFound", err)
	}
}

func TestSQLiteRotateTokenSecretReplacesSecretAndPrefix(t *testing.T) {
	ctx := context.Background()
	st := openTokenTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 7, 17, 10, 0, 0, 0, time.UTC)
	if err := st.CreatePlainToken(ctx, testTokenRecord(now), "plain-secret"); err != nil {
		t.Fatalf("CreatePlainToken returned %v", err)
	}

	rotatedAt := now.Add(time.Hour)
	if err := st.RotateTokenSecret(ctx, "tok_1", auth.HashSecret("rotated-secret"), "rotated-", rotatedAt); err != nil {
		t.Fatalf("RotateTokenSecret returned %v", err)
	}

	// Verify the stored row BEFORE any LookupBearer call: LookupBearer has a
	// side effect — it stamps last_used_at/updated_at = time.Now() on every
	// successful lookup — which would otherwise overwrite the rotate's
	// updated_at and make the UpdatedAt assertion impossible.
	rec, err := st.TokenByID(ctx, "tok_1")
	if err != nil {
		t.Fatalf("TokenByID returned %v", err)
	}
	if rec.SecretPrefix != "rotated-" || !rec.UpdatedAt.Equal(rotatedAt) {
		t.Fatalf("rec = %#v", rec)
	}

	// Old secret stops working, new secret resolves the same token id.
	if _, ok := st.LookupBearer("Bearer plain-secret"); ok {
		t.Fatalf("old secret still authenticates after rotate")
	}
	tok, ok := st.LookupBearer("Bearer rotated-secret")
	if !ok || tok.ID != "tok_1" {
		t.Fatalf("rotated secret lookup: ok=%v tok=%#v", ok, tok)
	}
}

func TestSQLiteRotateTokenSecretUnknownIDReturnsNotFound(t *testing.T) {
	ctx := context.Background()
	st := openTokenTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 7, 17, 10, 0, 0, 0, time.UTC)
	err := st.RotateTokenSecret(ctx, "tok_missing", auth.HashSecret("x"), "x", now)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("RotateTokenSecret error = %v, want ErrNotFound", err)
	}
}

func TestSQLiteDeleteTokenRemovesRow(t *testing.T) {
	ctx := context.Background()
	st := openTokenTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC)
	if err := st.CreatePlainToken(ctx, testTokenRecord(now), "plain-secret"); err != nil {
		t.Fatalf("CreatePlainToken returned %v", err)
	}

	if err := st.DeleteToken(ctx, "tok_1"); err != nil {
		t.Fatalf("DeleteToken returned %v", err)
	}

	if _, err := st.TokenByID(ctx, "tok_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("TokenByID after delete = %v, want ErrNotFound", err)
	}
	if _, ok := st.LookupBearer("Bearer plain-secret"); ok {
		t.Fatalf("deleted token should not authenticate")
	}
	if err := st.DeleteToken(ctx, "tok_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second DeleteToken = %v, want ErrNotFound", err)
	}
}

func TestSQLiteDeleteTokenCascadesRouteAffinity(t *testing.T) {
	ctx := context.Background()
	st := openTokenTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC)
	if err := st.CreatePlainToken(ctx, testTokenRecord(now), "plain-secret"); err != nil {
		t.Fatalf("CreatePlainToken returned %v", err)
	}
	if err := st.CreateAIServer(ctx, routing.AIServer{ID: "host_1", Name: "Host", Provider: routing.ProviderMock, Endpoint: "mock://h", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer returned %v", err)
	}
	if err := st.CreateApplication(ctx, routing.Application{ID: "app_1", ServerID: "host_1", Type: routing.ProviderMock, Port: 8000, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 10, Weight: 50, TimeoutMS: 30000, AffinityTTLSeconds: 1800, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication returned %v", err)
	}
	affinity := routing.RouteAffinity{ID: "aff_1", APITokenID: "tok_1", UserID: "usr_1", Model: "qwen-coder", APIFlavor: "openai", SessionID: "", ApplicationID: "app_1", ServerID: "host_1", ExpiresAt: now.Add(time.Hour), LastUsedAt: now, CreatedAt: now, UpdatedAt: now}
	if err := st.UpsertAffinity(ctx, affinity); err != nil {
		t.Fatalf("UpsertAffinity returned %v", err)
	}

	if err := st.DeleteToken(ctx, "tok_1"); err != nil {
		t.Fatalf("DeleteToken returned %v", err)
	}

	if _, ok, err := st.Affinity(ctx, routing.AffinityKey{APITokenID: "tok_1", Model: "qwen-coder", APIFlavor: "openai", SessionID: ""}); err != nil || ok {
		t.Fatalf("affinity after token delete: ok=%v err=%v, want ok=false", ok, err)
	}
}

func TestSQLiteTokenModelOverrideRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := openTokenTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 7, 14, 10, 0, 0, 0, time.UTC)

	rec := testTokenRecord(now)
	rec.ModelOverride = "gpt-oss-20b"
	rec.LogCommunication = true
	if err := st.CreatePlainToken(ctx, rec, "plain-secret"); err != nil {
		t.Fatalf("CreatePlainToken returned %v", err)
	}

	got, err := st.TokenByID(ctx, rec.ID)
	if err != nil {
		t.Fatalf("TokenByID returned %v", err)
	}
	if got.ModelOverride != "gpt-oss-20b" || !got.LogCommunication {
		t.Fatalf("got ModelOverride=%q LogCommunication=%v", got.ModelOverride, got.LogCommunication)
	}

	tok, ok := st.LookupBearer("Bearer plain-secret")
	if !ok || tok.ModelOverride != "gpt-oss-20b" || !tok.LogCommunication {
		t.Fatalf("LookupBearer ModelOverride=%q LogCommunication=%v ok=%v", tok.ModelOverride, tok.LogCommunication, ok)
	}

	got.ModelOverride = ""
	got.LogCommunication = false
	if err := st.UpdateTokenMetadata(ctx, got); err != nil {
		t.Fatalf("UpdateTokenMetadata returned %v", err)
	}
	after, _ := st.TokenByID(ctx, rec.ID)
	if after.ModelOverride != "" || after.LogCommunication {
		t.Fatalf("after update: ModelOverride=%q LogCommunication=%v", after.ModelOverride, after.LogCommunication)
	}
}

func TestSQLiteTokenModelOverrideMapRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := openTokenTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 7, 14, 10, 0, 0, 0, time.UTC)

	rec := testTokenRecord(now)
	rec.ModelOverride = "catch-all-model"
	rec.ModelOverrideMap = `{"gpt-4o":"qwen-coder","claude":"qwen-coder"}`
	if err := st.CreatePlainToken(ctx, rec, "plain-secret"); err != nil {
		t.Fatalf("CreatePlainToken returned %v", err)
	}

	got, err := st.TokenByID(ctx, rec.ID)
	if err != nil {
		t.Fatalf("TokenByID returned %v", err)
	}
	if got.ModelOverrideMap != rec.ModelOverrideMap {
		t.Fatalf("stored map = %q, want %q", got.ModelOverrideMap, rec.ModelOverrideMap)
	}

	// LookupBearer decodes the JSON column into auth.Token.ModelOverrideMap.
	tok, ok := st.LookupBearer("Bearer plain-secret")
	if !ok || len(tok.ModelOverrideMap) != 2 || tok.ModelOverrideMap["gpt-4o"] != "qwen-coder" || tok.ModelOverride != "catch-all-model" {
		t.Fatalf("LookupBearer map=%#v override=%q ok=%v", tok.ModelOverrideMap, tok.ModelOverride, ok)
	}

	// Clearing the map via update round-trips to the empty string.
	got.ModelOverrideMap = ""
	if err := st.UpdateTokenMetadata(ctx, got); err != nil {
		t.Fatalf("UpdateTokenMetadata returned %v", err)
	}
	after, _ := st.TokenByID(ctx, rec.ID)
	if after.ModelOverrideMap != "" {
		t.Fatalf("after clear: map=%q, want empty", after.ModelOverrideMap)
	}
	tok2, _ := st.LookupBearer("Bearer plain-secret")
	if len(tok2.ModelOverrideMap) != 0 {
		t.Fatalf("after clear LookupBearer map=%#v, want empty", tok2.ModelOverrideMap)
	}
}

func TestSQLiteTokenSecretRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := openTokenTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC)

	rec := testTokenRecord(now)
	rec.Secret = true
	if err := st.CreatePlainToken(ctx, rec, "plain-secret"); err != nil {
		t.Fatalf("CreatePlainToken returned %v", err)
	}

	got, err := st.TokenByID(ctx, rec.ID)
	if err != nil {
		t.Fatalf("TokenByID returned %v", err)
	}
	if !got.Secret {
		t.Fatalf("got Secret=%v, want true", got.Secret)
	}

	tok, ok := st.LookupBearer("Bearer plain-secret")
	if !ok || !tok.Secret {
		t.Fatalf("LookupBearer Secret=%v ok=%v, want true/true", tok.Secret, ok)
	}

	got.Secret = false
	if err := st.UpdateTokenMetadata(ctx, got); err != nil {
		t.Fatalf("UpdateTokenMetadata returned %v", err)
	}
	after, _ := st.TokenByID(ctx, rec.ID)
	if after.Secret {
		t.Fatalf("after update: Secret=%v, want false", after.Secret)
	}
}

// --- Phase 1 service accounts: service tokens (migration v40) --------------

func testServiceRecord(now time.Time) routing.Service {
	return routing.Service{
		ID: "svc_1", Name: "Nightly Batch", Description: "cron job",
		Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
	}
}

// TestSQLiteCreatePlainTokenServiceTokenHasNullUserID proves a service token
// (Kind=TokenKindService, ServiceID set) is persisted with a genuine SQL NULL
// user_id (not an empty string, which would fail the FK's reference-existence
// check) and defaults Kind when left empty.
func TestSQLiteCreatePlainTokenServiceTokenHasNullUserID(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)

	if err := st.CreateService(ctx, testServiceRecord(now)); err != nil {
		t.Fatalf("CreateService returned %v", err)
	}

	rec := TokenRecord{
		ID: "tok_svc", ServiceID: "svc_1", Kind: TokenKindService,
		Name: "batch token", Status: TokenStatusActive, Scopes: `["llm:invoke"]`,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := st.CreatePlainToken(ctx, rec, "svc-secret-value"); err != nil {
		t.Fatalf("CreatePlainToken returned %v", err)
	}

	var userID sql.NullString
	var serviceID sql.NullString
	var kind string
	if err := st.db.QueryRow(`select user_id, service_id, kind from api_tokens where id = ?`, "tok_svc").Scan(&userID, &serviceID, &kind); err != nil {
		t.Fatalf("QueryRow returned %v", err)
	}
	if userID.Valid {
		t.Fatalf("user_id = %q (valid), want a genuine SQL NULL for a service token", userID.String)
	}
	if !serviceID.Valid || serviceID.String != "svc_1" {
		t.Fatalf("service_id = %+v, want valid svc_1", serviceID)
	}
	if kind != TokenKindService {
		t.Fatalf("kind = %q, want %q", kind, TokenKindService)
	}

	// TokenByID/coalesce round-trip: UserID reads back "" (not a Go-side crash
	// on the NULL), ServiceID/Kind carried through.
	got, err := st.TokenByID(ctx, "tok_svc")
	if err != nil {
		t.Fatalf("TokenByID returned %v", err)
	}
	if got.UserID != "" || got.ServiceID != "svc_1" || got.Kind != TokenKindService {
		t.Fatalf("TokenByID = %+v, want UserID='' ServiceID=svc_1 Kind=service", got)
	}
}

// TestSQLiteCreatePlainTokenDefaultsKindToUser proves an existing caller that
// never sets Kind/ServiceID (every pre-Phase-1 call site) still gets a normal
// user token: Kind defaults to TokenKindUser, service_id stays NULL, user_id
// is set — i.e. the new columns are a no-op for the pre-existing code path.
func TestSQLiteCreatePlainTokenDefaultsKindToUser(t *testing.T) {
	ctx := context.Background()
	st := openTokenTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)

	if err := st.CreatePlainToken(ctx, testTokenRecord(now), "plain-secret"); err != nil {
		t.Fatalf("CreatePlainToken returned %v", err)
	}

	got, err := st.TokenByID(ctx, "tok_1")
	if err != nil {
		t.Fatalf("TokenByID returned %v", err)
	}
	if got.Kind != TokenKindUser {
		t.Fatalf("Kind = %q, want %q (default)", got.Kind, TokenKindUser)
	}
	if got.ServiceID != "" {
		t.Fatalf("ServiceID = %q, want empty for a user token", got.ServiceID)
	}
	if got.UserID != "usr_1" {
		t.Fatalf("UserID = %q, want usr_1", got.UserID)
	}
}

func TestSQLiteTokensByServiceReturnsOnlyServiceTokensNewestFirst(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)

	if err := st.CreateService(ctx, testServiceRecord(now)); err != nil {
		t.Fatalf("CreateService returned %v", err)
	}
	if err := st.CreateService(ctx, routing.Service{ID: "svc_2", Name: "Other", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateService svc_2 returned %v", err)
	}
	if err := st.CreateUser(ctx, newTestUser("usr_x", "svc-owner@example.test", now)); err != nil {
		t.Fatalf("CreateUser returned %v", err)
	}

	first := TokenRecord{ID: "tok_svc_a", ServiceID: "svc_1", Kind: TokenKindService, Name: "a", Status: TokenStatusActive, Scopes: `["llm:invoke"]`, CreatedAt: now, UpdatedAt: now}
	second := TokenRecord{ID: "tok_svc_b", ServiceID: "svc_1", Kind: TokenKindService, Name: "b", Status: TokenStatusActive, Scopes: `["llm:invoke"]`, CreatedAt: now.Add(time.Minute), UpdatedAt: now.Add(time.Minute)}
	otherService := TokenRecord{ID: "tok_svc_other", ServiceID: "svc_2", Kind: TokenKindService, Name: "other-svc", Status: TokenStatusActive, Scopes: `["llm:invoke"]`, CreatedAt: now, UpdatedAt: now}
	userToken := TokenRecord{ID: "tok_user", UserID: "usr_x", Name: "user", Status: TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}

	for _, rec := range []struct {
		tok    TokenRecord
		secret string
	}{
		{first, "secret-a"}, {second, "secret-b"}, {otherService, "secret-other"}, {userToken, "secret-user"},
	} {
		if err := st.CreatePlainToken(ctx, rec.tok, rec.secret); err != nil {
			t.Fatalf("CreatePlainToken %s returned %v", rec.tok.ID, err)
		}
	}

	records, err := st.TokensByService(ctx, "svc_1")
	if err != nil {
		t.Fatalf("TokensByService returned %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("tokens = %d, want 2: %+v", len(records), records)
	}
	if records[0].ID != "tok_svc_b" || records[1].ID != "tok_svc_a" {
		t.Fatalf("token order = %#v, want newest first [tok_svc_b, tok_svc_a]", records)
	}
	for _, r := range records {
		if r.ServiceID != "svc_1" || r.Kind != TokenKindService || r.UserID != "" {
			t.Fatalf("unexpected record shape: %+v", r)
		}
	}

	// A service with no tokens returns an empty, non-nil slice.
	none, err := st.TokensByService(ctx, "svc_2_no_tokens")
	if err != nil || none == nil || len(none) != 0 {
		t.Fatalf("TokensByService (no tokens) = %v %v, want empty non-nil slice", none, err)
	}
}

// TestSQLiteLookupBearerServiceToken proves LookupBearer resolves Kind/
// ServiceID/ServiceName/AllowedModels for a service token, and that an EMPTY
// allowlist means every model is allowed (AllowedModels comes back empty, not
// an error) while a NON-empty allowlist round-trips in full.
func TestSQLiteLookupBearerServiceToken(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)

	if err := st.CreateService(ctx, testServiceRecord(now)); err != nil {
		t.Fatalf("CreateService returned %v", err)
	}
	rec := TokenRecord{ID: "tok_svc", ServiceID: "svc_1", Kind: TokenKindService, Name: "batch", Status: TokenStatusActive, Scopes: `["llm:invoke"]`, CreatedAt: now, UpdatedAt: now}
	if err := st.CreatePlainToken(ctx, rec, "svc-secret-value"); err != nil {
		t.Fatalf("CreatePlainToken returned %v", err)
	}

	tok, ok := st.LookupBearer("Bearer svc-secret-value")
	if !ok {
		t.Fatalf("LookupBearer ok=false, want true")
	}
	if !tok.IsService() {
		t.Fatalf("IsService() = false, want true: %+v", tok)
	}
	if tok.ServiceID != "svc_1" || tok.ServiceName != "Nightly Batch" {
		t.Fatalf("ServiceID/ServiceName = %q/%q, want svc_1/Nightly Batch", tok.ServiceID, tok.ServiceName)
	}
	if len(tok.AllowedModels) != 0 {
		t.Fatalf("AllowedModels = %v, want empty (no allowlist = all models allowed)", tok.AllowedModels)
	}
	if !tok.HasScope("llm:invoke") {
		t.Fatalf("Scopes = %v, want llm:invoke", tok.Scopes)
	}

	// Set an allowlist; it round-trips through the NEXT LookupBearer (not
	// cached from the first call).
	if err := st.SetServiceAllowedModels(ctx, "svc_1", []string{"llama3", "gpt-4o"}); err != nil {
		t.Fatalf("SetServiceAllowedModels returned %v", err)
	}
	tok2, ok := st.LookupBearer("Bearer svc-secret-value")
	if !ok {
		t.Fatalf("LookupBearer (with allowlist) ok=false, want true")
	}
	want := []string{"gpt-4o", "llama3"} // ServiceAllowedModels orders by gateway_model_name
	if len(tok2.AllowedModels) != len(want) || tok2.AllowedModels[0] != want[0] || tok2.AllowedModels[1] != want[1] {
		t.Fatalf("AllowedModels = %v, want %v", tok2.AllowedModels, want)
	}
}

// TestSQLiteLookupBearerRejectsDisabledServiceTokens proves disabling a
// service immediately rejects ALL of its tokens at LookupBearer, without
// touching the token rows themselves (the join/status-check approach the
// design chose over per-token status mirroring, so it cannot drift).
func TestSQLiteLookupBearerRejectsDisabledServiceTokens(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)

	if err := st.CreateService(ctx, testServiceRecord(now)); err != nil {
		t.Fatalf("CreateService returned %v", err)
	}
	rec := TokenRecord{ID: "tok_svc", ServiceID: "svc_1", Kind: TokenKindService, Name: "batch", Status: TokenStatusActive, Scopes: `["llm:invoke"]`, CreatedAt: now, UpdatedAt: now}
	if err := st.CreatePlainToken(ctx, rec, "svc-secret-value"); err != nil {
		t.Fatalf("CreatePlainToken returned %v", err)
	}

	if _, ok := st.LookupBearer("Bearer svc-secret-value"); !ok {
		t.Fatalf("LookupBearer ok=false while the service is active, want true")
	}

	svc := testServiceRecord(now)
	svc.Status = routing.ServerStatusDisabled
	svc.UpdatedAt = now.Add(time.Minute)
	if err := st.UpdateService(ctx, svc); err != nil {
		t.Fatalf("UpdateService (disable) returned %v", err)
	}

	if _, ok := st.LookupBearer("Bearer svc-secret-value"); ok {
		t.Fatalf("LookupBearer ok=true after disabling the service, want false — the token row itself was never touched")
	}

	// The token row is unchanged (still status=active) — proving the reject
	// comes from the service-status join, not a per-token flag.
	stillActive, err := st.TokenByID(ctx, "tok_svc")
	if err != nil || stillActive.Status != TokenStatusActive {
		t.Fatalf("token row after disable = %+v err=%v, want Status=active (untouched)", stillActive, err)
	}

	// Re-enabling immediately un-blocks it.
	svc.Status = routing.ServerStatusActive
	svc.UpdatedAt = now.Add(2 * time.Minute)
	if err := st.UpdateService(ctx, svc); err != nil {
		t.Fatalf("UpdateService (re-enable) returned %v", err)
	}
	if _, ok := st.LookupBearer("Bearer svc-secret-value"); !ok {
		t.Fatalf("LookupBearer ok=false after re-enabling the service, want true")
	}
}

// TestSQLiteLookupBearerRejectsServiceKindWithNoServiceID guards the
// data-integrity fail-closed branch: a kind="service" row with an empty
// service_id (which should never happen through CreatePlainToken, but could
// via a raw write) must never resolve as a valid, unbound service token.
func TestSQLiteLookupBearerRejectsServiceKindWithNoServiceID(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)

	rec := TokenRecord{ID: "tok_orphan", Kind: TokenKindService, Name: "orphan", Status: TokenStatusActive, Scopes: `["llm:invoke"]`, CreatedAt: now, UpdatedAt: now}
	if err := st.CreatePlainToken(ctx, rec, "orphan-secret-value"); err != nil {
		t.Fatalf("CreatePlainToken returned %v", err)
	}

	if _, ok := st.LookupBearer("Bearer orphan-secret-value"); ok {
		t.Fatal("LookupBearer resolved a kind=service token with no service_id, want false")
	}

	_ = ctx
}

// TestSQLiteTokenProjectIDRoundTrips proves api_tokens.project_id (Task 5)
// round-trips through CreatePlainToken -> TokenByID/scanToken.
func TestSQLiteTokenProjectIDRoundTrips(t *testing.T) {
	ctx := context.Background()
	st := openTokenTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 8, 9, 10, 0, 0, 0, time.UTC)

	if err := st.CreateProject(ctx, Project{
		ID: "proj_1", Name: "Alpha", OwnerUserID: "usr_1", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateProject returned %v", err)
	}

	rec := testTokenRecord(now)
	rec.ProjectID = "proj_1"
	if err := st.CreatePlainToken(ctx, rec, "proj-secret"); err != nil {
		t.Fatalf("CreatePlainToken returned %v", err)
	}

	got, err := st.TokenByID(ctx, "tok_1")
	if err != nil {
		t.Fatalf("TokenByID returned %v", err)
	}
	if got.ProjectID != "proj_1" {
		t.Fatalf("ProjectID = %q, want %q", got.ProjectID, "proj_1")
	}

	// A token created with NO project_id round-trips as an empty string, not
	// a NULL-scan error (the coalesce(project_id,'') in tokenColumns).
	rec2 := testTokenRecord(now)
	rec2.ID = "tok_noproj"
	if err := st.CreatePlainToken(ctx, rec2, "noproj-secret"); err != nil {
		t.Fatalf("CreatePlainToken (no project) returned %v", err)
	}
	got2, err := st.TokenByID(ctx, "tok_noproj")
	if err != nil {
		t.Fatalf("TokenByID (no project) returned %v", err)
	}
	if got2.ProjectID != "" {
		t.Fatalf("ProjectID = %q, want empty", got2.ProjectID)
	}
}

// TestSQLiteUpdateTokenMetadataChangesProjectID proves UpdateTokenMetadata
// can both reassign a token to a different project and clear the
// attribution back to "" (Task 5).
func TestSQLiteUpdateTokenMetadataChangesProjectID(t *testing.T) {
	ctx := context.Background()
	st := openTokenTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 8, 9, 10, 0, 0, 0, time.UTC)

	for _, p := range []Project{
		{ID: "proj_a", Name: "A", OwnerUserID: "usr_1", CreatedAt: now, UpdatedAt: now},
		{ID: "proj_b", Name: "B", OwnerUserID: "usr_1", CreatedAt: now, UpdatedAt: now},
	} {
		if err := st.CreateProject(ctx, p); err != nil {
			t.Fatalf("CreateProject(%s) returned %v", p.ID, err)
		}
	}

	rec := testTokenRecord(now)
	rec.ProjectID = "proj_a"
	if err := st.CreatePlainToken(ctx, rec, "proj-secret"); err != nil {
		t.Fatalf("CreatePlainToken returned %v", err)
	}

	rec.ProjectID = "proj_b"
	rec.UpdatedAt = now.Add(time.Minute)
	if err := st.UpdateTokenMetadata(ctx, rec); err != nil {
		t.Fatalf("UpdateTokenMetadata (reassign) returned %v", err)
	}
	got, err := st.TokenByID(ctx, "tok_1")
	if err != nil || got.ProjectID != "proj_b" {
		t.Fatalf("ProjectID after reassign = %q err=%v, want proj_b", got.ProjectID, err)
	}

	rec.ProjectID = ""
	rec.UpdatedAt = now.Add(2 * time.Minute)
	if err := st.UpdateTokenMetadata(ctx, rec); err != nil {
		t.Fatalf("UpdateTokenMetadata (clear) returned %v", err)
	}
	got, err = st.TokenByID(ctx, "tok_1")
	if err != nil || got.ProjectID != "" {
		t.Fatalf("ProjectID after clear = %q err=%v, want empty", got.ProjectID, err)
	}
}

// TestSQLiteRotateTokenSecretPreservesProjectID proves RotateTokenSecret
// touches only the secret columns, leaving project_id untouched (Task 5).
func TestSQLiteRotateTokenSecretPreservesProjectID(t *testing.T) {
	ctx := context.Background()
	st := openTokenTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 8, 9, 10, 0, 0, 0, time.UTC)

	if err := st.CreateProject(ctx, Project{
		ID: "proj_1", Name: "Alpha", OwnerUserID: "usr_1", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateProject returned %v", err)
	}
	rec := testTokenRecord(now)
	rec.ProjectID = "proj_1"
	if err := st.CreatePlainToken(ctx, rec, "proj-secret"); err != nil {
		t.Fatalf("CreatePlainToken returned %v", err)
	}

	if err := st.RotateTokenSecret(ctx, "tok_1", auth.HashSecret("new-secret"), "newsecpr", now.Add(time.Minute)); err != nil {
		t.Fatalf("RotateTokenSecret returned %v", err)
	}

	got, err := st.TokenByID(ctx, "tok_1")
	if err != nil {
		t.Fatalf("TokenByID returned %v", err)
	}
	if got.ProjectID != "proj_1" {
		t.Fatalf("ProjectID after rotate = %q, want proj_1 (rotation must not touch it)", got.ProjectID)
	}
	if _, ok := st.LookupBearer("Bearer new-secret"); !ok {
		t.Fatal("LookupBearer failed to resolve the rotated secret")
	}
}

// TestSQLiteLookupBearerResolvesProjectName proves LookupBearer resolves
// auth.Token.ProjectName from the projects table at lookup time (Task 5,
// mirroring how ServiceName is resolved for a service token) -- so a project
// rename is reflected immediately, without touching the token row.
func TestSQLiteLookupBearerResolvesProjectName(t *testing.T) {
	ctx := context.Background()
	st := openTokenTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 8, 9, 10, 0, 0, 0, time.UTC)

	if err := st.CreateProject(ctx, Project{
		ID: "proj_1", Name: "Alpha", OwnerUserID: "usr_1", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateProject returned %v", err)
	}
	rec := testTokenRecord(now)
	rec.ProjectID = "proj_1"
	if err := st.CreatePlainToken(ctx, rec, "proj-secret"); err != nil {
		t.Fatalf("CreatePlainToken returned %v", err)
	}

	got, ok := st.LookupBearer("Bearer proj-secret")
	if !ok {
		t.Fatal("LookupBearer returned ok=false")
	}
	if got.ProjectID != "proj_1" || got.ProjectName != "Alpha" {
		t.Fatalf("ProjectID/ProjectName = %q/%q, want proj_1/Alpha", got.ProjectID, got.ProjectName)
	}

	if err := st.UpdateProject(ctx, Project{
		ID: "proj_1", Name: "Alpha Renamed", OwnerUserID: "usr_1", CreatedAt: now, UpdatedAt: now.Add(time.Minute),
	}); err != nil {
		t.Fatalf("UpdateProject returned %v", err)
	}
	got, ok = st.LookupBearer("Bearer proj-secret")
	if !ok || got.ProjectName != "Alpha Renamed" {
		t.Fatalf("ProjectName after rename = %q ok=%v, want %q", got.ProjectName, ok, "Alpha Renamed")
	}
}

// TestSQLiteLookupBearerEmptyProjectIDYieldsEmptyProjectName proves a token
// with no project attribution resolves an empty ProjectName, not a spurious
// value or an error (Task 5).
func TestSQLiteLookupBearerEmptyProjectIDYieldsEmptyProjectName(t *testing.T) {
	ctx := context.Background()
	st := openTokenTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 8, 9, 10, 0, 0, 0, time.UTC)

	if err := st.CreatePlainToken(ctx, testTokenRecord(now), "plain-secret"); err != nil {
		t.Fatalf("CreatePlainToken returned %v", err)
	}
	got, ok := st.LookupBearer("Bearer plain-secret")
	if !ok {
		t.Fatal("LookupBearer returned ok=false")
	}
	if got.ProjectID != "" || got.ProjectName != "" {
		t.Fatalf("ProjectID/ProjectName = %q/%q, want empty/empty", got.ProjectID, got.ProjectName)
	}
}

func openTokenTestSQLite(t *testing.T) *SQLiteStore {
	t.Helper()
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	now := time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC)
	user := User{
		ID:                "usr_1",
		Email:             "admin@example.test",
		DisplayName:       "Admin",
		Role:              "admin",
		Status:            UserStatusActive,
		PreferredLanguage: "de",
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if err := st.CreateUser(ctx, user); err != nil {
		_ = st.Close()
		t.Fatalf("CreateUser returned %v", err)
	}
	return st
}

func testTokenRecord(now time.Time) TokenRecord {
	return TokenRecord{
		ID:        "tok_1",
		UserID:    "usr_1",
		Name:      "Dev Token",
		Status:    TokenStatusActive,
		Scopes:    `["gateway:use"]`,
		CreatedAt: now,
		UpdatedAt: now,
	}
}
