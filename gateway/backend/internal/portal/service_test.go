// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestCurrentUserReturnsAuthenticatedUserContext(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	users := &fakeUsers{byID: map[string]store.User{
		"usr_1": {
			ID:                "usr_1",
			Email:             "admin@example.test",
			DisplayName:       "Admin User",
			Role:              "admin",
			Status:            store.UserStatusActive,
			PreferredLanguage: "de",
			CreatedAt:         now,
			UpdatedAt:         now,
		},
	}}
	svc := NewService(ServiceDeps{Users: users, Clock: func() time.Time { return now }})

	got, err := svc.CurrentUser(context.Background(), auth.Token{UserID: "usr_1"})
	if err != nil {
		t.Fatalf("CurrentUser returned %v", err)
	}
	if got.ID != "usr_1" || got.Email != "admin@example.test" || got.DisplayName != "Admin User" {
		t.Fatalf("current user = %#v", got)
	}
	if got.Role != "admin" || got.PreferredLanguage != "de" {
		t.Fatalf("role/language = %s/%s", got.Role, got.PreferredLanguage)
	}
}

func TestListTokensRedactsSecretHash(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	tokens := &fakeTokens{records: []store.TokenRecord{
		{
			ID:           "tok_1",
			UserID:       "usr_1",
			Name:         "Portal Token",
			SecretHash:   "must-not-leak",
			SecretPrefix: "opaigw_",
			Status:       store.TokenStatusActive,
			Scopes:       `["gateway:use","admin"]`,
			CreatedAt:    now,
			UpdatedAt:    now,
		},
	}}
	svc := NewService(ServiceDeps{Tokens: tokens, Clock: func() time.Time { return now }})

	got, err := svc.ListTokens(context.Background(), auth.Token{UserID: "usr_1"})
	if err != nil {
		t.Fatalf("ListTokens returned %v", err)
	}
	// ListTokens prepends the synthetic non-deletable ChatSession row, so the
	// real token is the second entry.
	if len(got.Data) != 2 {
		t.Fatalf("tokens = %d, want 2 (synthetic + real)", len(got.Data))
	}
	if got.Data[0].ID != ChatSessionTokenID || !got.Data[0].IsChatSession {
		t.Fatalf("first row must be the synthetic ChatSession: %#v", got.Data[0])
	}
	real := got.Data[1]
	if real.ID != "tok_1" || real.SecretPrefix != "opaigw_" {
		t.Fatalf("token dto = %#v", real)
	}
	if real.SecretHash != "" {
		t.Fatalf("SecretHash leaked: %q", real.SecretHash)
	}
	if len(real.Scopes) != 2 || real.Scopes[0] != "gateway:use" || real.Scopes[1] != "admin" {
		t.Fatalf("scopes = %#v", real.Scopes)
	}
}

func TestCreateTokenValidatesNameAndReturnsOneTimeSecret(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	tokens := &fakeTokens{}
	svc := NewService(ServiceDeps{
		Tokens:          tokens,
		Clock:           func() time.Time { return now },
		SecretGenerator: func() (string, error) { return "opaigw_generated_secret", nil },
		IDGenerator:     func() string { return "tok_generated" },
	})

	_, err := svc.CreateToken(context.Background(), auth.Token{UserID: "usr_1"}, CreateTokenRequest{Name: "   "})
	if !errors.Is(err, ErrTokenNameRequired) {
		t.Fatalf("blank name error = %v, want ErrTokenNameRequired", err)
	}

	got, err := svc.CreateToken(context.Background(), auth.Token{UserID: "usr_1", Scopes: []string{"gateway:use"}}, CreateTokenRequest{
		Name:   " Codex Local ",
		Scopes: []string{"gateway:use"},
	})
	if err != nil {
		t.Fatalf("CreateToken returned %v", err)
	}
	if got.Secret != "opaigw_generated_secret" {
		t.Fatalf("secret = %q", got.Secret)
	}
	if got.Token.ID != "tok_generated" || got.Token.Name != "Codex Local" {
		t.Fatalf("token = %#v", got.Token)
	}
	if len(tokens.created) != 1 {
		t.Fatalf("created tokens = %d, want 1", len(tokens.created))
	}
	if tokens.created[0].Secret != "opaigw_generated_secret" {
		t.Fatalf("created secret = %q", tokens.created[0].Secret)
	}
	if tokens.created[0].Record.UserID != "usr_1" || tokens.created[0].Record.Status != store.TokenStatusActive {
		t.Fatalf("created record = %#v", tokens.created[0].Record)
	}
}

func TestCreateTokenRejectsUnauthorizedOrUnknownScopes(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name        string
		ownerScopes []string
		reqScopes   []string
		wantErr     error
	}{
		{name: "gateway token cannot mint admin", ownerScopes: []string{"gateway:use"}, reqScopes: []string{"gateway:use", "admin"}, wantErr: ErrTokenScopeForbidden},
		{name: "unknown scope rejected", ownerScopes: []string{"gateway:use", "admin"}, reqScopes: []string{"gateway:use", "unknown:scope"}, wantErr: ErrTokenScopeInvalid},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tokens := &fakeTokens{}
			svc := NewService(ServiceDeps{
				Tokens:          tokens,
				Clock:           func() time.Time { return now },
				SecretGenerator: func() (string, error) { return "opaigw_generated_secret", nil },
				IDGenerator:     func() string { return "tok_generated" },
			})

			_, err := svc.CreateToken(context.Background(), auth.Token{UserID: "usr_1", Scopes: tt.ownerScopes}, CreateTokenRequest{
				Name:   "Privileged",
				Scopes: tt.reqScopes,
			})

			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("CreateToken error = %v, want %v", err, tt.wantErr)
			}
			if len(tokens.created) != 0 {
				t.Fatalf("created tokens = %d, want 0", len(tokens.created))
			}
		})
	}
}

func TestCreateTokenAllowsAdminToMintPrivilegedScopes(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	tokens := &fakeTokens{}
	svc := NewService(ServiceDeps{
		Tokens:          tokens,
		Clock:           func() time.Time { return now },
		SecretGenerator: func() (string, error) { return "opaigw_generated_secret", nil },
		IDGenerator:     func() string { return "tok_generated" },
	})

	_, err := svc.CreateToken(context.Background(), auth.Token{UserID: "usr_1", Scopes: []string{"gateway:use", "admin"}}, CreateTokenRequest{
		Name:   "Admin Token",
		Scopes: []string{"admin"},
	})
	if err != nil {
		t.Fatalf("CreateToken returned %v", err)
	}
	if len(tokens.created) != 1 {
		t.Fatalf("created tokens = %d, want 1", len(tokens.created))
	}
	if tokens.created[0].Record.Scopes != `["admin"]` {
		t.Fatalf("created scopes = %s", tokens.created[0].Record.Scopes)
	}
}

func TestUsageAndDashboardAreScopedToAuthenticatedUser(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	usageStore := &fakeUsage{events: []usage.Event{
		{ID: "req_1", UserID: "usr_1", TokenID: "tok_1", Model: "qwen-coder", Provider: "mock", Host: "mock-host", TotalTokens: 10, LatencyMS: 10, Status: "success", CreatedAt: now.Add(-time.Hour)},
		{ID: "req_2", UserID: "usr_1", TokenID: "tok_1", Model: "qwen-coder", Provider: "mock", Host: "mock-host", TotalTokens: 30, LatencyMS: 30, Status: "success", CreatedAt: now.Add(-2 * time.Hour)},
		{ID: "req_old", UserID: "usr_1", TokenID: "tok_1", Model: "qwen-coder", Provider: "mock", Host: "mock-host", TotalTokens: 100, LatencyMS: 100, Status: "success", CreatedAt: now.Add(-25 * time.Hour)},
	}}
	svc := NewService(ServiceDeps{Usage: usageStore, Clock: func() time.Time { return now }})

	page, err := svc.Usage(auth.Token{UserID: "usr_1"}, usage.Query{Page: 1, Limit: 25})
	if err != nil {
		t.Fatalf("Usage returned err: %v", err)
	}
	if page.Total != 3 || len(page.Data) != 3 {
		t.Fatalf("usage page total = %d, data = %d, want 3/3", page.Total, len(page.Data))
	}

	dashboard := svc.Dashboard(context.Background(), auth.Token{UserID: "usr_1"})
	if dashboard.Metrics.Requests24h != 2 {
		t.Fatalf("Requests24h = %d, want 2", dashboard.Metrics.Requests24h)
	}
	if dashboard.Metrics.Tokens24h != 40 {
		t.Fatalf("Tokens24h = %d, want 40", dashboard.Metrics.Tokens24h)
	}
	if dashboard.Metrics.LatencyP95MS != 30 {
		t.Fatalf("LatencyP95MS = %d, want 30", dashboard.Metrics.LatencyP95MS)
	}
	if dashboard.Metrics.HealthyHosts != "mock" {
		t.Fatalf("HealthyHosts = %q, want mock", dashboard.Metrics.HealthyHosts)
	}
	if len(dashboard.Routes) != 1 || dashboard.Routes[0].Status != "active" {
		t.Fatalf("routes = %#v", dashboard.Routes)
	}
}

func TestServiceUsageAllowsAllScopeForPlainAdmin(t *testing.T) {
	rec := usage.NewRecorder()
	rec.Record(usage.Event{ID: "a", UserID: "usr_1", Status: "success", HTTPStatus: 200, CreatedAt: time.Now().UTC()})
	rec.Record(usage.Event{ID: "b", UserID: "usr_2", Status: "success", HTTPStatus: 200, CreatedAt: time.Now().UTC()})
	svc := NewService(ServiceDeps{Usage: rec})

	// A plain admin (admin scope, no system scope) asking for scope=all now sees
	// every user's rows (SP-C corrected the gate to HasScope("admin")).
	page, err := svc.Usage(
		auth.Token{UserID: "usr_1", Scopes: []string{"gateway:use", "admin"}},
		usage.Query{Page: 1, Limit: 25, ScopeAll: true},
	)
	if err != nil {
		t.Fatalf("Usage returned err: %v", err)
	}
	if page.Total != 2 || len(page.Data) != 2 {
		t.Fatalf("plain admin all-scope should see both users: %#v", page)
	}
}

func TestServiceUsagePinsPlainUserToOwnScope(t *testing.T) {
	rec := usage.NewRecorder()
	rec.Record(usage.Event{ID: "a", UserID: "usr_1", Status: "success", HTTPStatus: 200, CreatedAt: time.Now().UTC()})
	rec.Record(usage.Event{ID: "b", UserID: "usr_2", Status: "success", HTTPStatus: 200, CreatedAt: time.Now().UTC()})
	svc := NewService(ServiceDeps{Usage: rec})

	// A gateway:use-only principal must stay pinned to its own rows even when it
	// asks for scope=all — HasScope("admin") is false for it.
	page, err := svc.Usage(
		auth.Token{UserID: "usr_1", Scopes: []string{"gateway:use"}},
		usage.Query{Page: 1, Limit: 25, ScopeAll: true},
	)
	if err != nil {
		t.Fatalf("Usage returned err: %v", err)
	}
	if page.Total != 1 || len(page.Data) != 1 || page.Data[0].UserID != "usr_1" {
		t.Fatalf("plain user all-scope leaked cross-user rows: %#v", page)
	}
}

func TestServiceUsageSystemAdminSeesAllUsers(t *testing.T) {
	rec := usage.NewRecorder()
	rec.Record(usage.Event{ID: "a", UserID: "usr_1", Status: "success", HTTPStatus: 200, CreatedAt: time.Now().UTC()})
	rec.Record(usage.Event{ID: "b", UserID: "usr_2", Status: "success", HTTPStatus: 200, CreatedAt: time.Now().UTC()})
	svc := NewService(ServiceDeps{Usage: rec})

	page, err := svc.Usage(
		auth.Token{UserID: "usr_1", Scopes: []string{"gateway:use", "admin", "system"}},
		usage.Query{Page: 1, Limit: 25, ScopeAll: true},
	)
	if err != nil {
		t.Fatalf("Usage returned err: %v", err)
	}
	if page.Total != 2 {
		t.Fatalf("system admin all-scope total = %d, want 2 (%#v)", page.Total, page)
	}
}

func TestServiceUsageEnrichesHasCaptureFromReader(t *testing.T) {
	rec := usage.NewRecorder()
	rec.Record(usage.Event{ID: "a", UserID: "usr_1", Status: "success", HTTPStatus: 200, CreatedAt: time.Now().UTC()})
	rec.Record(usage.Event{ID: "b", UserID: "usr_1", Status: "success", HTTPStatus: 200, CreatedAt: time.Now().UTC()})
	svc := NewService(ServiceDeps{Usage: rec, Captures: hasCapturesStub{has: map[string]store.CapturePresence{"a": {OwnerUserID: "usr_1"}}}})

	page, err := svc.Usage(auth.Token{UserID: "usr_1", Scopes: []string{"gateway:use"}}, usage.Query{Page: 1, Limit: 25})
	if err != nil {
		t.Fatalf("Usage returned err: %v", err)
	}
	if page.Total != 2 || len(page.Data) != 2 {
		t.Fatalf("page = %#v, want 2 rows", page)
	}
	got := map[string]bool{}
	for _, row := range page.Data {
		got[row.ID] = row.HasCapture
	}
	if !got["a"] {
		t.Fatalf("row a HasCapture = false, want true")
	}
	if got["b"] {
		t.Fatalf("row b HasCapture = true, want false")
	}
}

// TestServiceUsagePresenceHasCaptureVsLocked exercises the SP-2e matrix: for a
// secret vs non-secret capture, the owner always gets has_capture, while a
// different admin gets has_capture only for the non-secret row and a
// capture_locked signal (no content) for the secret one.
func TestServiceUsagePresenceHasCaptureVsLocked(t *testing.T) {
	rec := usage.NewRecorder()
	rec.Record(usage.Event{ID: "sec", UserID: "usr_owner", Status: "success", HTTPStatus: 200, CreatedAt: time.Now().UTC()})
	rec.Record(usage.Event{ID: "plain", UserID: "usr_owner", Status: "success", HTTPStatus: 200, CreatedAt: time.Now().UTC()})
	presence := map[string]store.CapturePresence{
		"sec":   {Secret: true, OwnerUserID: "usr_owner"},
		"plain": {Secret: false, OwnerUserID: "usr_owner"},
	}
	svc := NewService(ServiceDeps{Usage: rec, Captures: hasCapturesStub{has: presence}})

	index := func(page usage.Page, err error) map[string]usage.Row {
		if err != nil {
			t.Fatalf("Usage returned err: %v", err)
		}
		out := map[string]usage.Row{}
		for _, row := range page.Data {
			out[row.ID] = row
		}
		return out
	}

	// Owner sees both captures; neither is locked.
	owner := index(svc.Usage(auth.Token{UserID: "usr_owner", Scopes: []string{"gateway:use"}}, usage.Query{Page: 1, Limit: 25}))
	if !owner["sec"].HasCapture || owner["sec"].CaptureLocked {
		t.Fatalf("owner secret row = %#v, want has_capture=true capture_locked=false", owner["sec"])
	}
	if !owner["plain"].HasCapture || owner["plain"].CaptureLocked {
		t.Fatalf("owner plain row = %#v, want has_capture=true capture_locked=false", owner["plain"])
	}

	// A different admin (all-scope) may open the non-secret capture but only
	// sees the secret one as locked (existence, no content).
	admin := index(svc.Usage(auth.Token{UserID: "usr_admin", Scopes: []string{"gateway:use", "admin"}}, usage.Query{Page: 1, Limit: 25, ScopeAll: true}))
	if admin["sec"].HasCapture || !admin["sec"].CaptureLocked {
		t.Fatalf("admin secret row = %#v, want has_capture=false capture_locked=true", admin["sec"])
	}
	if !admin["plain"].HasCapture || admin["plain"].CaptureLocked {
		t.Fatalf("admin plain row = %#v, want has_capture=true capture_locked=false", admin["plain"])
	}
}

func TestServiceUsageHasCaptureFalseWhenCapturesNil(t *testing.T) {
	rec := usage.NewRecorder()
	rec.Record(usage.Event{ID: "a", UserID: "usr_1", Status: "success", HTTPStatus: 200, CreatedAt: time.Now().UTC()})
	svc := NewService(ServiceDeps{Usage: rec})

	page, err := svc.Usage(auth.Token{UserID: "usr_1", Scopes: []string{"gateway:use"}}, usage.Query{Page: 1, Limit: 25})
	if err != nil {
		t.Fatalf("Usage returned err: %v", err)
	}
	if len(page.Data) != 1 || page.Data[0].HasCapture {
		t.Fatalf("page = %#v, want 1 row with HasCapture=false (no Captures reader configured)", page)
	}
}

func TestServiceUsageHasCaptureBestEffortOnReaderError(t *testing.T) {
	rec := usage.NewRecorder()
	rec.Record(usage.Event{ID: "a", UserID: "usr_1", Status: "success", HTTPStatus: 200, CreatedAt: time.Now().UTC()})
	svc := NewService(ServiceDeps{Usage: rec, Captures: hasCapturesStub{err: errors.New("boom")}})

	page, err := svc.Usage(auth.Token{UserID: "usr_1", Scopes: []string{"gateway:use"}}, usage.Query{Page: 1, Limit: 25})
	if err != nil {
		t.Fatalf("Usage returned err: %v", err)
	}
	if page.Total != 1 || len(page.Data) != 1 {
		t.Fatalf("page = %#v, want the list to still render despite the lookup error", page)
	}
	if page.Data[0].HasCapture {
		t.Fatalf("row HasCapture = true, want false (best-effort fallback on lookup error)")
	}
}

func TestServiceUsageEnrichesHasCaptureFromRealSQLiteAndMemoryStore(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	st.Record(usage.Event{ID: "req_cap", UserID: "usr_1", TokenID: "tok_1", Status: "success", HTTPStatus: 200, CreatedAt: time.Now().UTC()})
	st.Record(usage.Event{ID: "req_nocap", UserID: "usr_1", TokenID: "tok_1", Status: "success", HTTPStatus: 200, CreatedAt: time.Now().UTC()})
	if err := st.LastUsageError(); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// Capture lives in the RAM store only, NOT in SQLite's captures table — so a
	// SQL exists() join would say false; correct enrichment must consult the
	// wired CaptureReader (the MemoryCaptureStore) instead.
	mem := store.NewMemoryCaptureStore(0)
	if err := mem.SaveCapture(ctx, store.Capture{UsageEventID: "req_cap", OwnerUserID: "usr_1", KeyVersion: 0, Blob: []byte("x"), CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("SaveCapture: %v", err)
	}

	svc := NewService(ServiceDeps{Usage: st, Captures: mem})

	page, err := svc.Usage(auth.Token{UserID: "usr_1", Scopes: []string{"gateway:use"}}, usage.Query{Page: 1, Limit: 25})
	if err != nil {
		t.Fatalf("Usage returned err: %v", err)
	}
	if page.Total != 2 || len(page.Data) != 2 {
		t.Fatalf("page = %#v, want 2 rows", page)
	}
	got := map[string]bool{}
	for _, row := range page.Data {
		got[row.ID] = row.HasCapture
	}
	if !got["req_cap"] {
		t.Fatalf("req_cap HasCapture = false, want true (real SQLite usage + real MemoryCaptureStore capture)")
	}
	if got["req_nocap"] {
		t.Fatalf("req_nocap HasCapture = true, want false (no capture stored)")
	}
}

// TestServiceUsagePropagatesStoreError proves Usage/UsageStats/UsageTimeSeries
// return the underlying store error rather than swallowing it (ST-2): before
// ST-2 these Service methods had no error return at all, so a genuine store
// failure was indistinguishable from "no matching rows" all the way up to the
// HTTP response. A closed *store.SQLStore is used as a real, reliably-failing
// usage.Store.
func TestServiceUsagePropagatesStoreError(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	svc := NewService(ServiceDeps{Usage: st})
	token := auth.Token{UserID: "usr_1", Scopes: []string{"gateway:use"}}

	if _, err := svc.Usage(token, usage.Query{Page: 1, Limit: 25}); err == nil {
		t.Fatal("Usage on a closed store: want a non-nil error, got nil")
	}
	if _, err := svc.UsageStats(token, usage.Query{}); err == nil {
		t.Fatal("UsageStats on a closed store: want a non-nil error, got nil")
	}
	if _, err := svc.UsageTimeSeries(token, usage.Query{From: time.Now(), To: time.Now()}, 10); err == nil {
		t.Fatal("UsageTimeSeries on a closed store: want a non-nil error, got nil")
	}
}

func TestServiceUsageStatsDelegatesWithOwnScope(t *testing.T) {
	rec := usage.NewRecorder()
	rec.Record(usage.Event{ID: "a", UserID: "usr_1", Status: "success", HTTPStatus: 200, InputTokens: 3, OutputTokens: 7, TotalTokens: 10, CreatedAt: time.Now().UTC()})
	rec.Record(usage.Event{ID: "b", UserID: "usr_1", Status: "error", HTTPStatus: 200, CreatedAt: time.Now().UTC()})
	rec.Record(usage.Event{ID: "c", UserID: "usr_2", Status: "success", HTTPStatus: 200, CreatedAt: time.Now().UTC()})
	svc := NewService(ServiceDeps{Usage: rec})

	stats, err := svc.UsageStats(auth.Token{UserID: "usr_1", Scopes: []string{"gateway:use"}}, usage.Query{})
	if err != nil {
		t.Fatalf("UsageStats returned err: %v", err)
	}
	if stats.Totals.TotalRequests != 2 {
		t.Fatalf("own-scope total_requests = %d, want 2", stats.Totals.TotalRequests)
	}
	if stats.Totals.ErrorCount != 1 {
		t.Fatalf("error_count = %d, want 1 (status==error counts even with http_status 200)", stats.Totals.ErrorCount)
	}
}

func TestServiceUsageTimeSeriesScope(t *testing.T) {
	from := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	to := from.Add(10 * time.Second)
	mk := func() *usage.Recorder {
		rec := usage.NewRecorder()
		rec.Record(usage.Event{ID: "a", UserID: "usr_1", HTTPStatus: 200, InputTokens: 100, CreatedAt: from.Add(time.Second)})
		rec.Record(usage.Event{ID: "b", UserID: "usr_2", HTTPStatus: 200, InputTokens: 100, CreatedAt: from.Add(2 * time.Second)})
		return rec
	}
	base := usage.Query{From: from, To: to, ScopeAll: true}

	// Owner (no admin scope): pinned to own rows even with scope=all intent.
	own, err := NewService(ServiceDeps{Usage: mk()}).UsageTimeSeries(
		auth.Token{UserID: "usr_1", Scopes: []string{"gateway:use"}}, base, 10)
	if err != nil {
		t.Fatalf("UsageTimeSeries returned err: %v", err)
	}
	if len(own.Points) != 1 || own.Points[0].Connections != 1 {
		t.Fatalf("owner scope=all Connections = %#v, want pinned to own (1)", own.Points)
	}
	if own.BucketSeconds != 10 {
		t.Fatalf("BucketSeconds = %d, want 10", own.BucketSeconds)
	}

	// Admin with scope=all: sees both users.
	all, err := NewService(ServiceDeps{Usage: mk()}).UsageTimeSeries(
		auth.Token{UserID: "usr_1", Scopes: []string{"gateway:use", "admin"}}, base, 10)
	if err != nil {
		t.Fatalf("UsageTimeSeries returned err: %v", err)
	}
	if len(all.Points) != 1 || all.Points[0].Connections != 2 {
		t.Fatalf("admin scope=all Connections = %#v, want 2", all.Points)
	}
}

func TestServiceModelsDerivedFromActiveMappings(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	routeStore := routing.NewMemoryStore()
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "srv_1", Name: "S1", Domain: "s1.test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: "app_1", ServerID: "srv_1", Type: routing.ProviderVLLM, Port: 8000, Scheme: "https", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: "map_1", ApplicationID: "app_1", GatewayModelName: "qwen-coder", AppModelName: "qwen2.5", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: "map_off", ApplicationID: "app_1", GatewayModelName: "hidden", AppModelName: "hidden", Status: routing.ServerStatusDisabled, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping off: %v", err)
	}
	svc := NewService(ServiceDeps{Usage: usage.NewRecorder(), Routes: routeStore, Clock: func() time.Time { return now }})
	got := svc.Models(ctx, auth.Token{UserID: "usr_1"})
	if len(got.Data) != 1 || got.Data[0].ID != "qwen-coder" {
		t.Fatalf("models = %#v, want [qwen-coder]", got.Data)
	}
	if !reflect.DeepEqual(got.Data[0].Flavors, []string{routing.APIFlavorOpenAI}) {
		t.Fatalf("flavors = %#v, want [openai]", got.Data[0].Flavors)
	}
}

// fakeLoadedReader is a LoadedModelReader stub: it maps (appID,serverID) to the
// upstream model names it reports as loaded.
type fakeLoadedReader struct {
	byKey map[string][]string
}

func (f fakeLoadedReader) LoadedAppModels(appID, serverID string) []string {
	return f.byKey[appID+"|"+serverID]
}

func TestServiceModelsMarkLoaded(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	routeStore := routing.NewMemoryStore()
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "srv_1", Name: "GPU-Box", Domain: "s1.test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: "app_1", ServerID: "srv_1", Type: routing.ProviderVLLM, Port: 8000, Scheme: "https", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	// Two gateway models on the same app; only qwen2.5 is reported loaded.
	if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: "m1", ApplicationID: "app_1", GatewayModelName: "qwen-coder", AppModelName: "qwen2.5", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: "m2", ApplicationID: "app_1", GatewayModelName: "llama3", AppModelName: "llama-3.1", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	reader := fakeLoadedReader{byKey: map[string][]string{"app_1|srv_1": {"qwen2.5"}}}
	svc := NewService(ServiceDeps{Usage: usage.NewRecorder(), Routes: routeStore, LoadedModels: reader, Clock: func() time.Time { return now }})
	got := svc.Models(ctx, auth.Token{UserID: "usr_1"})

	byID := map[string]ModelDTO{}
	for _, m := range got.Data {
		byID[m.ID] = m
	}
	if !byID["qwen-coder"].Loaded {
		t.Fatalf("qwen-coder should be loaded: %#v", byID["qwen-coder"])
	}
	if !reflect.DeepEqual(byID["qwen-coder"].LoadedOn, []string{"GPU-Box"}) {
		t.Fatalf("qwen-coder loaded_on = %#v, want [GPU-Box]", byID["qwen-coder"].LoadedOn)
	}
	if byID["llama3"].Loaded {
		t.Fatalf("llama3 should NOT be loaded: %#v", byID["llama3"])
	}
	if len(byID["llama3"].LoadedOn) != 0 {
		t.Fatalf("llama3 loaded_on = %#v, want empty", byID["llama3"].LoadedOn)
	}
}

func TestServiceModelsOfferedOnCount(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	routeStore := routing.NewMemoryStore()
	mustServer := func(id, name string) {
		if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: id, Name: name, Domain: id + ".test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateAIServer %s: %v", id, err)
		}
	}
	mustApp := func(id, srv string, port int) {
		if err := routeStore.CreateApplication(ctx, routing.Application{ID: id, ServerID: srv, Type: routing.ProviderVLLM, Port: port, Scheme: "https", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateApplication %s: %v", id, err)
		}
	}
	mustMap := func(id, app, gateway string) {
		if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: id, ApplicationID: app, GatewayModelName: gateway, AppModelName: gateway, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateMapping %s: %v", id, err)
		}
	}
	// "shared" is offered by two DISTINCT servers; server A offers it via TWO apps
	// (distinct ports; must still count once for that server → per-server de-dup).
	mustServer("srv_a", "GPU-A")
	mustApp("app_a1", "srv_a", 8000)
	mustMap("m_a1", "app_a1", "shared")
	mustApp("app_a2", "srv_a", 8001)
	mustMap("m_a2", "app_a2", "shared")
	mustServer("srv_b", "GPU-B")
	mustApp("app_b", "srv_b", 8000)
	mustMap("m_b", "app_b", "shared")
	// "solo" is offered by ONE server only.
	mustServer("srv_c", "GPU-C")
	mustApp("app_c", "srv_c", 8000)
	mustMap("m_c", "app_c", "solo")

	svc := NewService(ServiceDeps{Usage: usage.NewRecorder(), Routes: routeStore, Clock: func() time.Time { return now }})
	got := svc.Models(ctx, auth.Token{UserID: "usr_1"})

	byID := map[string]ModelDTO{}
	for _, m := range got.Data {
		byID[m.ID] = m
	}
	if byID["shared"].OfferedOnCount != 2 {
		t.Fatalf("shared offered_on_count = %d, want 2 (two servers, per-server de-dup)", byID["shared"].OfferedOnCount)
	}
	if byID["solo"].OfferedOnCount != 1 {
		t.Fatalf("solo offered_on_count = %d, want 1", byID["solo"].OfferedOnCount)
	}
}

// TestServiceModelsOfferedOnCountGroup: a synthetic model group's
// OfferedOnCount is the UNION of its offerable members' servers (de-duped),
// NOT the sum. Member m1 is offered by servers A+B, m2 by servers B+C (they
// OVERLAP on B), so the group's count is 3 (A,B,C), not 4.
func TestServiceModelsOfferedOnCountGroup(t *testing.T) {
	ctx := context.Background()
	rs := routing.NewMemoryStore()
	// m1 on servers A and B; m2 on servers B and C (overlap on B).
	offerModel(t, rs, "srv_a", "A", "app_a", []string{routing.APIFlavorOpenAI}, "m1", "m1-up", routing.ServerStatusActive)
	offerModel(t, rs, "srv_b", "B", "app_b", []string{routing.APIFlavorOpenAI}, "m1", "m1-up", routing.ServerStatusActive)
	// Server B's app also offers m2 (a second mapping on the same app → same server name).
	if err := rs.CreateMapping(ctx, routing.ModelMapping{ID: "app_b_map2", ApplicationID: "app_b", GatewayModelName: "m2", AppModelName: "m2-up", Status: routing.ServerStatusActive, CreatedAt: offeringTime, UpdatedAt: offeringTime}); err != nil {
		t.Fatalf("CreateMapping app_b_map2: %v", err)
	}
	offerModel(t, rs, "srv_c", "C", "app_c", []string{routing.APIFlavorOpenAI}, "m2", "m2-up", routing.ServerStatusActive)
	offerGroup(t, rs, "grp_1", "combo", "m1", "m2")

	byID := modelsByID(offerSvc(rs, nil).Models(ctx, auth.Token{UserID: "usr_1"}))

	grp, ok := byID["combo"]
	if !ok {
		t.Fatalf("group 'combo' missing from Models(): %#v", byID)
	}
	if !grp.IsGroup {
		t.Fatalf("group 'combo' IsGroup=false, want true: %#v", grp)
	}
	if grp.OfferedOnCount != 3 {
		t.Fatalf("group 'combo' offered_on_count = %d, want 3 (A,B,C union, NOT 4)", grp.OfferedOnCount)
	}
	// Sanity: the members' own standalone counts (m1: A,B; m2: B,C).
	if byID["m1"].OfferedOnCount != 2 {
		t.Fatalf("m1 offered_on_count = %d, want 2", byID["m1"].OfferedOnCount)
	}
	if byID["m2"].OfferedOnCount != 2 {
		t.Fatalf("m2 offered_on_count = %d, want 2", byID["m2"].OfferedOnCount)
	}
}

func TestServiceModelsIncludeNonPortalWithFlavorSet(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	routeStore := routing.NewMemoryStore()
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "srv_1", Name: "S1", Domain: "s1.test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: "app_api", ServerID: "srv_1", Type: routing.ProviderVLLM, Port: 8000, Scheme: "https", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: "map_api", ApplicationID: "app_api", GatewayModelName: "api-only", AppModelName: "api-only", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	svc := NewService(ServiceDeps{Usage: usage.NewRecorder(), Routes: routeStore, Clock: func() time.Time { return now }})
	got := svc.Models(ctx, auth.Token{UserID: "usr_1"})
	if len(got.Data) != 1 || got.Data[0].ID != "api-only" {
		t.Fatalf("models = %#v, want single api-only", got.Data)
	}
	if !reflect.DeepEqual(got.Data[0].Flavors, []string{routing.APIFlavorOpenAI}) {
		t.Fatalf("flavors = %#v, want [openai]", got.Data[0].Flavors)
	}
}

func TestServiceModelsForFlavorFiltersByFlavor(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	routeStore := routing.NewMemoryStore()
	mustServer := func(id string) {
		if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: id, Name: id, Domain: id + ".test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateAIServer %s: %v", id, err)
		}
	}
	mustApp := func(id, srv string, flavors []string) {
		if err := routeStore.CreateApplication(ctx, routing.Application{ID: id, ServerID: srv, Type: routing.ProviderVLLM, Port: 8000, Scheme: "https", APIFlavors: flavors, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateApplication %s: %v", id, err)
		}
	}
	mustMap := func(id, app, gateway string) {
		if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: id, ApplicationID: app, GatewayModelName: gateway, AppModelName: gateway, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateMapping %s: %v", id, err)
		}
	}
	mustServer("srv_o")
	mustApp("app_o", "srv_o", []string{routing.APIFlavorOpenAI})
	mustMap("map_o", "app_o", "openai-model")
	mustServer("srv_a")
	mustApp("app_a", "srv_a", []string{routing.APIFlavorAnthropic})
	mustMap("map_a", "app_a", "anthropic-model")

	svc := NewService(ServiceDeps{Usage: usage.NewRecorder(), Routes: routeStore, Clock: func() time.Time { return now }})

	if got := svc.ModelsForFlavor(ctx, auth.Token{UserID: "usr_1"}, routing.APIFlavorOpenAI); !reflect.DeepEqual(got, []string{"openai-model"}) {
		t.Fatalf("openai models = %#v, want [openai-model]", got)
	}
	if got := svc.ModelsForFlavor(ctx, auth.Token{UserID: "usr_1"}, routing.APIFlavorAnthropic); !reflect.DeepEqual(got, []string{"anthropic-model"}) {
		t.Fatalf("anthropic models = %#v, want [anthropic-model]", got)
	}
}

func TestServiceModelsUnionsFlavorsAcrossApplications(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	routeStore := routing.NewMemoryStore()
	seed := func(srv, app string, flavors []string, mapID string) {
		if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: srv, Name: srv, Domain: srv + ".test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("server %s: %v", srv, err)
		}
		if err := routeStore.CreateApplication(ctx, routing.Application{ID: app, ServerID: srv, Type: routing.ProviderVLLM, Port: 8000, Scheme: "https", APIFlavors: flavors, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("app %s: %v", app, err)
		}
		if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: mapID, ApplicationID: app, GatewayModelName: "shared-model", AppModelName: "shared-model", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("map %s: %v", mapID, err)
		}
	}
	seed("srv_o", "app_o", []string{routing.APIFlavorOpenAI}, "map_o")
	seed("srv_ap", "app_ap", []string{routing.APIFlavorAnthropic}, "map_ap")

	svc := NewService(ServiceDeps{Usage: usage.NewRecorder(), Routes: routeStore, Clock: func() time.Time { return now }})
	got := svc.Models(ctx, auth.Token{UserID: "usr_1"})
	if len(got.Data) != 1 || got.Data[0].ID != "shared-model" {
		t.Fatalf("models = %#v, want single shared-model", got.Data)
	}
	if !reflect.DeepEqual(got.Data[0].Flavors, []string{routing.APIFlavorAnthropic, routing.APIFlavorOpenAI}) {
		t.Fatalf("flavors = %#v, want [anthropic openai]", got.Data[0].Flavors)
	}
	if got := svc.ModelsForFlavor(ctx, auth.Token{UserID: "usr_1"}, routing.APIFlavorOpenAI); !reflect.DeepEqual(got, []string{"shared-model"}) {
		t.Fatalf("openai = %#v, want [shared-model]", got)
	}
}

func TestServiceDashboardRoutesFromActiveMappings(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	routeStore := routing.NewMemoryStore()
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "srv_1", Name: "Server One", Domain: "s1.test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: "app_1", ServerID: "srv_1", Type: routing.ProviderVLLM, Port: 8000, Scheme: "https", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: "map_1", ApplicationID: "app_1", GatewayModelName: "qwen-coder", AppModelName: "qwen2.5", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	svc := NewService(ServiceDeps{Usage: usage.NewRecorder(), Routes: routeStore, Clock: func() time.Time { return now }})
	dash := svc.Dashboard(ctx, auth.Token{UserID: "usr_1"})
	if dash.Metrics.HealthyHosts != "1/1" {
		t.Fatalf("healthy hosts = %q, want 1/1", dash.Metrics.HealthyHosts)
	}
	if len(dash.Routes) != 1 {
		t.Fatalf("routes = %#v, want 1", dash.Routes)
	}
	row := dash.Routes[0]
	if row.Model != "qwen-coder" || row.Provider != routing.ProviderVLLM || row.Host != "Server One" || row.Status != "active" {
		t.Fatalf("route row = %#v", row)
	}
}

func TestServiceMappingViewsExcludeIneligibleAndDedupe(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	routeStore := routing.NewMemoryStore()
	seed := func(srvID, srvName, status, health, appID, appStatus, mapID, gateway string) {
		if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: srvID, Name: srvName, Domain: srvID + ".test", Status: status, HealthStatus: health, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateAIServer %s: %v", srvID, err)
		}
		if err := routeStore.CreateApplication(ctx, routing.Application{ID: appID, ServerID: srvID, Type: routing.ProviderVLLM, Port: 8000, Scheme: "https", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: appStatus, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateApplication %s: %v", appID, err)
		}
		if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: mapID, ApplicationID: appID, GatewayModelName: gateway, AppModelName: gateway, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateMapping %s: %v", mapID, err)
		}
	}
	// Two healthy servers both serving qwen-coder (dedup in Models, two dashboard rows).
	seed("srv_a", "A", routing.ServerStatusActive, routing.HealthHealthy, "app_a", routing.ServerStatusActive, "map_a", "qwen-coder")
	seed("srv_b", "B", routing.ServerStatusActive, routing.HealthHealthy, "app_b", routing.ServerStatusActive, "map_b", "qwen-coder")
	// Unhealthy server -> excluded.
	seed("srv_c", "C", routing.ServerStatusActive, routing.HealthUnhealthy, "app_c", routing.ServerStatusActive, "map_c", "unhealthy-model")
	// Disabled server -> excluded.
	seed("srv_d", "D", routing.ServerStatusDisabled, routing.HealthHealthy, "app_d", routing.ServerStatusActive, "map_d", "disabled-server-model")
	// Healthy server but disabled application -> excluded.
	seed("srv_e", "E", routing.ServerStatusActive, routing.HealthHealthy, "app_e", routing.ServerStatusDisabled, "map_e", "disabled-app-model")

	svc := NewService(ServiceDeps{Usage: usage.NewRecorder(), Routes: routeStore, Clock: func() time.Time { return now }})

	models := svc.Models(ctx, auth.Token{UserID: "usr_1"})
	if len(models.Data) != 1 || models.Data[0].ID != "qwen-coder" {
		t.Fatalf("models = %#v, want single deduped qwen-coder (ineligible servers/apps excluded)", models.Data)
	}

	dash := svc.Dashboard(ctx, auth.Token{UserID: "usr_1"})
	// healthy metric counts servers that are active && HealthHealthy: A, B, E => 3 of 5 total.
	if dash.Metrics.HealthyHosts != "3/5" {
		t.Fatalf("healthy hosts = %q, want 3/5", dash.Metrics.HealthyHosts)
	}
	// dashboard rows come from selectable servers with active apps+mappings: only A and B.
	if len(dash.Routes) != 2 {
		t.Fatalf("routes = %#v, want 2 (srv_a + srv_b, qwen-coder)", dash.Routes)
	}
	for _, r := range dash.Routes {
		if r.Model != "qwen-coder" {
			t.Fatalf("unexpected dashboard row %#v", r)
		}
	}
}

func TestAuthorizeRunAsToken(t *testing.T) {
	ctx := context.Background()
	dir := NewMemoryDirectory(auth.NewTokenStore())
	now := time.Now().UTC()
	_ = dir.CreateUser(ctx, store.User{ID: "usr_1", Email: "u1@example.test", DisplayName: "U1", Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	_ = dir.CreateUser(ctx, store.User{ID: "usr_2", Email: "u2@example.test", DisplayName: "U2", Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	if err := dir.CreatePlainToken(ctx, store.TokenRecord{ID: "tok_ok", UserID: "usr_1", Name: "ok", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}, "secret-ok"); err != nil {
		t.Fatalf("create token: %v", err)
	}
	if err := dir.CreatePlainToken(ctx, store.TokenRecord{ID: "tok_other", UserID: "usr_2", Name: "other", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}, "secret-other"); err != nil {
		t.Fatalf("create token: %v", err)
	}
	if err := dir.CreatePlainToken(ctx, store.TokenRecord{ID: "tok_agent", UserID: "usr_1", Name: "agent", Status: store.TokenStatusActive, Scopes: `["agent:report"]`, CreatedAt: now, UpdatedAt: now}, "secret-agent"); err != nil {
		t.Fatalf("create token: %v", err)
	}
	svc := NewService(ServiceDeps{Users: dir, Tokens: dir})
	principal := auth.Token{UserID: "usr_1", Scopes: []string{"gateway:use"}}

	if _, err := svc.AuthorizeRunAsToken(ctx, principal, ""); !errors.Is(err, ErrTokenRequired) {
		t.Fatalf("blank token id should be required, got %v", err)
	}
	if _, err := svc.AuthorizeRunAsToken(ctx, principal, "tok_other"); !errors.Is(err, ErrTokenForbidden) {
		t.Fatalf("another user's token should be forbidden, got %v", err)
	}
	if _, err := svc.AuthorizeRunAsToken(ctx, principal, "tok_agent"); !errors.Is(err, ErrTokenForbidden) {
		t.Fatalf("non gateway:use token should be forbidden, got %v", err)
	}
	runAs, err := svc.AuthorizeRunAsToken(ctx, principal, "tok_ok")
	if err != nil {
		t.Fatalf("valid token should authorize: %v", err)
	}
	if runAs.ID != "tok_ok" || runAs.UserID != "usr_1" || !runAs.HasScope("gateway:use") {
		t.Fatalf("unexpected run-as token: %+v", runAs)
	}

	past := time.Now().Add(-time.Hour)
	if err := dir.CreatePlainToken(ctx, store.TokenRecord{ID: "tok_expired", UserID: "usr_1", Name: "expired", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, ExpiresAt: &past, CreatedAt: now, UpdatedAt: now}, "secret-expired"); err != nil {
		t.Fatalf("create token: %v", err)
	}
	if _, err := svc.AuthorizeRunAsToken(ctx, principal, "tok_expired"); !errors.Is(err, ErrTokenForbidden) {
		t.Fatalf("expired token should be forbidden, got %v", err)
	}
}

func TestCreateTokenRejectsDuplicateNameCaseInsensitive(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	tokens := &fakeTokens{records: []store.TokenRecord{
		{ID: "tok_1", UserID: "usr_1", Name: "Codex Local", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now},
	}}
	svc := NewService(ServiceDeps{
		Tokens:          tokens,
		Clock:           func() time.Time { return now },
		SecretGenerator: func() (string, error) { return "opaigw_secret", nil },
		IDGenerator:     func() string { return "tok_2" },
	})

	_, err := svc.CreateToken(context.Background(), auth.Token{UserID: "usr_1", Scopes: []string{"gateway:use"}}, CreateTokenRequest{Name: "  codex local  ", Scopes: []string{"gateway:use"}})

	if !errors.Is(err, ErrTokenNameConflict) {
		t.Fatalf("CreateToken error = %v, want ErrTokenNameConflict", err)
	}
}

func TestUpdateTokenRenamesChangesScopesAndStatus(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	tokens := &fakeTokens{records: []store.TokenRecord{
		{ID: "tok_1", UserID: "usr_1", Name: "Old", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now},
	}}
	svc := NewService(ServiceDeps{Tokens: tokens, Clock: func() time.Time { return now }})
	owner := auth.Token{UserID: "usr_1", Scopes: []string{"gateway:use", "admin"}}

	name := "New Name"
	scopes := []string{"gateway:use", "admin"}
	status := store.TokenStatusDisabled
	dto, err := svc.UpdateToken(context.Background(), owner, "tok_1", UpdateTokenRequest{Name: &name, Scopes: &scopes, Status: &status})
	if err != nil {
		t.Fatalf("UpdateToken returned %v", err)
	}
	if dto.Name != "New Name" || dto.Status != store.TokenStatusDisabled {
		t.Fatalf("dto = %#v", dto)
	}
	if len(dto.Scopes) != 2 || dto.Scopes[1] != "admin" {
		t.Fatalf("scopes = %#v", dto.Scopes)
	}
	stored, _ := tokens.TokenByID(context.Background(), "tok_1")
	if stored.Name != "New Name" || stored.Status != store.TokenStatusDisabled || stored.Scopes != `["gateway:use","admin"]` {
		t.Fatalf("stored = %#v", stored)
	}
}

func TestUpdateTokenValidatesOwnershipNameStatusAndScopes(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	newTokens := func() *fakeTokens {
		return &fakeTokens{records: []store.TokenRecord{
			{ID: "tok_1", UserID: "usr_1", Name: "Alpha", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now},
			{ID: "tok_2", UserID: "usr_1", Name: "Beta", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now},
			{ID: "tok_other", UserID: "usr_2", Name: "Gamma", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now},
		}}
	}
	owner := auth.Token{UserID: "usr_1", Scopes: []string{"gateway:use"}}
	dupName := "beta"
	blankName := "   "
	badStatus := "banished"
	privileged := []string{"admin"}

	cases := []struct {
		name    string
		tokenID string
		req     UpdateTokenRequest
		wantErr error
	}{
		{name: "unknown token", tokenID: "tok_missing", req: UpdateTokenRequest{Name: strPtr("x")}, wantErr: ErrTokenNotFound},
		{name: "not owned", tokenID: "tok_other", req: UpdateTokenRequest{Name: strPtr("x")}, wantErr: ErrTokenNotFound},
		{name: "blank name", tokenID: "tok_1", req: UpdateTokenRequest{Name: &blankName}, wantErr: ErrTokenNameRequired},
		{name: "duplicate name", tokenID: "tok_1", req: UpdateTokenRequest{Name: &dupName}, wantErr: ErrTokenNameConflict},
		{name: "invalid status", tokenID: "tok_1", req: UpdateTokenRequest{Status: &badStatus}, wantErr: ErrTokenStatusInvalid},
		{name: "forbidden scope", tokenID: "tok_1", req: UpdateTokenRequest{Scopes: &privileged}, wantErr: ErrTokenScopeForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := NewService(ServiceDeps{Tokens: newTokens(), Clock: func() time.Time { return now }})
			_, err := svc.UpdateToken(context.Background(), owner, tc.tokenID, tc.req)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("UpdateToken error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestUpdateTokenStatusOnlyPreservesNameAndScopes(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	tokens := &fakeTokens{records: []store.TokenRecord{
		{ID: "tok_1", UserID: "usr_1", Name: "Keep Me", Status: store.TokenStatusActive, Scopes: `["gateway:use","admin"]`, CreatedAt: now, UpdatedAt: now},
	}}
	svc := NewService(ServiceDeps{Tokens: tokens, Clock: func() time.Time { return now }})
	owner := auth.Token{UserID: "usr_1", Scopes: []string{"gateway:use", "admin"}}

	status := store.TokenStatusDisabled
	dto, err := svc.UpdateToken(context.Background(), owner, "tok_1", UpdateTokenRequest{Status: &status})
	if err != nil {
		t.Fatalf("UpdateToken returned %v", err)
	}
	if dto.Name != "Keep Me" || len(dto.Scopes) != 2 || dto.Status != store.TokenStatusDisabled {
		t.Fatalf("dto = %#v", dto)
	}
	stored, _ := tokens.TokenByID(context.Background(), "tok_1")
	if stored.Name != "Keep Me" || stored.Scopes != `["gateway:use","admin"]` || stored.Status != store.TokenStatusDisabled {
		t.Fatalf("stored = %#v", stored)
	}
}

func TestDeleteTokenRemovesOwnedAndRejectsOthers(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	tokens := &fakeTokens{records: []store.TokenRecord{
		{ID: "tok_1", UserID: "usr_1", Name: "Mine", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now},
		{ID: "tok_other", UserID: "usr_2", Name: "Theirs", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now},
	}}
	svc := NewService(ServiceDeps{Tokens: tokens, Clock: func() time.Time { return now }})
	owner := auth.Token{UserID: "usr_1", Scopes: []string{"gateway:use"}}

	if err := svc.DeleteToken(context.Background(), owner, "tok_other"); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("delete not-owned = %v, want ErrTokenNotFound", err)
	}
	if err := svc.DeleteToken(context.Background(), owner, "tok_1"); err != nil {
		t.Fatalf("delete owned returned %v", err)
	}
	if _, err := tokens.TokenByID(context.Background(), "tok_1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("token still present after delete: %v", err)
	}
}

func TestRotateTokenReturnsFreshSecretAndPersistsHash(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	tokens := &fakeTokens{records: []store.TokenRecord{
		{ID: "tok_1", UserID: "usr_1", Name: "Mine", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, SecretPrefix: "opaigw_o", CreatedAt: now, UpdatedAt: now},
	}}
	svc := NewService(ServiceDeps{
		Tokens:          tokens,
		Clock:           func() time.Time { return now },
		SecretGenerator: func() (string, error) { return "opaigw_rotated_secret", nil },
	})
	owner := auth.Token{UserID: "usr_1", Scopes: []string{"gateway:use"}}

	got, err := svc.RotateToken(context.Background(), owner, "tok_1")
	if err != nil {
		t.Fatalf("RotateToken returned %v", err)
	}
	if got.Secret != "opaigw_rotated_secret" {
		t.Fatalf("secret = %q", got.Secret)
	}
	// secretPrefix = first 8 chars of the new secret.
	if got.Token.ID != "tok_1" || got.Token.SecretPrefix != "opaigw_r" {
		t.Fatalf("token = %#v", got.Token)
	}
	stored, _ := tokens.TokenByID(context.Background(), "tok_1")
	if stored.SecretHash != auth.HashSecret("opaigw_rotated_secret") {
		t.Fatalf("stored hash = %q, want rotated hash", stored.SecretHash)
	}
	if stored.SecretPrefix != "opaigw_r" {
		t.Fatalf("stored prefix = %q", stored.SecretPrefix)
	}
}

func TestRotateTokenRejectsForeignOwnerChatSessionAndMissing(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	tokens := &fakeTokens{records: []store.TokenRecord{
		{ID: "tok_other", UserID: "usr_2", Name: "Theirs", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now},
	}}
	svc := NewService(ServiceDeps{
		Tokens:          tokens,
		Clock:           func() time.Time { return now },
		SecretGenerator: func() (string, error) { return "opaigw_x", nil },
	})
	owner := auth.Token{UserID: "usr_1", Scopes: []string{"gateway:use"}}

	if _, err := svc.RotateToken(context.Background(), owner, "tok_other"); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("foreign owner = %v, want ErrTokenNotFound", err)
	}
	if _, err := svc.RotateToken(context.Background(), owner, ChatSessionTokenID); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("chat-session = %v, want ErrTokenNotFound", err)
	}
	if _, err := svc.RotateToken(context.Background(), owner, "tok_missing"); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("missing = %v, want ErrTokenNotFound", err)
	}
}

func TestListTokensPrependsSyntheticChatSession(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	users := &fakeUsers{byID: map[string]store.User{
		"usr_1": {ID: "usr_1", Email: "u@example.test", DisplayName: "U", Role: "user", Status: store.UserStatusActive, ChatLogCommunication: true, ChatSecret: true},
	}}
	tokens := &fakeTokens{records: []store.TokenRecord{
		{ID: "tok_1", UserID: "usr_1", Name: "Real", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now},
	}}
	svc := NewService(ServiceDeps{Users: users, Tokens: tokens, Clock: func() time.Time { return now }})

	got, err := svc.ListTokens(context.Background(), auth.Token{UserID: "usr_1"})
	if err != nil {
		t.Fatalf("ListTokens returned %v", err)
	}
	if len(got.Data) != 2 {
		t.Fatalf("tokens = %d, want 2 (synthetic + real)", len(got.Data))
	}
	chat := got.Data[0]
	if chat.ID != ChatSessionTokenID || !chat.IsChatSession || chat.Deletable {
		t.Fatalf("synthetic row = %#v", chat)
	}
	if !chat.LogCommunication || !chat.Secret {
		t.Fatalf("synthetic row must carry the user's chat flags: %#v", chat)
	}
	if chat.Status != store.TokenStatusActive || len(chat.Scopes) != 0 {
		t.Fatalf("synthetic row status/scopes = %#v", chat)
	}
	real := got.Data[1]
	if real.ID != "tok_1" || real.IsChatSession || !real.Deletable {
		t.Fatalf("real row = %#v", real)
	}
}

func TestListTokensSyntheticRowSurvivesUserLookupError(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	users := &fakeUsers{byID: map[string]store.User{}} // user missing -> UserByID errors
	tokens := &fakeTokens{records: []store.TokenRecord{
		{ID: "tok_1", UserID: "usr_1", Name: "Real", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now},
	}}
	svc := NewService(ServiceDeps{Users: users, Tokens: tokens, Clock: func() time.Time { return now }})

	got, err := svc.ListTokens(context.Background(), auth.Token{UserID: "usr_1"})
	if err != nil {
		t.Fatalf("ListTokens must not fail on user lookup error: %v", err)
	}
	if len(got.Data) != 2 || got.Data[0].ID != ChatSessionTokenID {
		t.Fatalf("synthetic row missing on lookup error: %#v", got.Data)
	}
	if got.Data[0].LogCommunication || got.Data[0].Secret {
		t.Fatalf("lookup error must fall back to false chat flags: %#v", got.Data[0])
	}
}

func TestUserTokensPrependsChatPseudoForAnyUser(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	users := &fakeUsers{byID: map[string]store.User{
		"usr_target": {ID: "usr_target", ChatLogCommunication: true, ChatSecret: true},
	}}
	tokens := &fakeTokens{records: []store.TokenRecord{
		{ID: "tok_t", UserID: "usr_target", Name: "Target Token", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now},
	}}
	svc := NewService(ServiceDeps{Users: users, Tokens: tokens, Clock: func() time.Time { return now }})

	got, err := svc.UserTokens(context.Background(), adminToken(), "usr_target")
	if err != nil {
		t.Fatalf("UserTokens returned %v", err)
	}
	if len(got.Data) != 2 {
		t.Fatalf("tokens = %d, want 2 (synthetic + real)", len(got.Data))
	}
	if got.Data[0].ID != ChatSessionTokenID || !got.Data[0].IsChatSession {
		t.Fatalf("first row must be the ChatSession pseudo: %#v", got.Data[0])
	}
	if !got.Data[0].LogCommunication || !got.Data[0].Secret {
		t.Fatalf("pseudo must carry the target user's chat flags: %#v", got.Data[0])
	}
	if got.Data[1].ID != "tok_t" {
		t.Fatalf("real token missing: %#v", got.Data[1])
	}
}

func TestChatSessionTokenNotDeletableOrEditable(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	tokens := &fakeTokens{}
	svc := NewService(ServiceDeps{Tokens: tokens, Clock: func() time.Time { return now }})
	owner := auth.Token{UserID: "usr_1", Scopes: []string{"gateway:use"}}

	if err := svc.DeleteToken(context.Background(), owner, ChatSessionTokenID); !errors.Is(err, ErrTokenNotDeletable) {
		t.Fatalf("delete chat-session = %v, want ErrTokenNotDeletable", err)
	}
	if _, err := svc.UpdateToken(context.Background(), owner, ChatSessionTokenID, UpdateTokenRequest{Name: strPtr("x")}); !errors.Is(err, ErrTokenNotDeletable) {
		t.Fatalf("update chat-session = %v, want ErrTokenNotDeletable", err)
	}
}

type fakeUsers struct {
	byID map[string]store.User
}

func (f *fakeUsers) UserByID(ctx context.Context, id string) (store.User, error) {
	if user, ok := f.byID[id]; ok {
		return user, nil
	}
	return store.User{}, store.ErrNotFound
}

func (f *fakeUsers) ListUsers(ctx context.Context) ([]store.User, error) {
	out := make([]store.User, 0, len(f.byID))
	for _, u := range f.byID {
		out = append(out, u)
	}
	return out, nil
}

type createdToken struct {
	Record store.TokenRecord
	Secret string
}

type fakeTokens struct {
	records []store.TokenRecord
	created []createdToken
}

func (f *fakeTokens) TokensByUser(ctx context.Context, userID string) ([]store.TokenRecord, error) {
	out := make([]store.TokenRecord, 0)
	for _, record := range f.records {
		if record.UserID == userID {
			out = append(out, record)
		}
	}
	return out, nil
}

func (f *fakeTokens) TokensByService(ctx context.Context, serviceID string) ([]store.TokenRecord, error) {
	out := make([]store.TokenRecord, 0)
	for _, record := range f.records {
		if record.ServiceID == serviceID {
			out = append(out, record)
		}
	}
	return out, nil
}

func (f *fakeTokens) TokensByProject(ctx context.Context, projectID string) ([]store.TokenRecord, error) {
	out := make([]store.TokenRecord, 0)
	if projectID == "" {
		return out, nil
	}
	for _, record := range f.records {
		if record.ProjectID == projectID {
			out = append(out, record)
		}
	}
	return out, nil
}

func (f *fakeTokens) TokenByID(ctx context.Context, id string) (store.TokenRecord, error) {
	for _, record := range f.records {
		if record.ID == id {
			return record, nil
		}
	}
	return store.TokenRecord{}, store.ErrNotFound
}

func (f *fakeTokens) CreatePlainToken(ctx context.Context, token store.TokenRecord, secret string) error {
	f.created = append(f.created, createdToken{Record: token, Secret: secret})
	f.records = append(f.records, token)
	return nil
}

func (f *fakeTokens) UpdateTokenMetadata(ctx context.Context, token store.TokenRecord) error {
	for i, record := range f.records {
		if record.ID == token.ID {
			f.records[i] = token
			return nil
		}
	}
	return store.ErrNotFound
}

func (f *fakeTokens) RotateTokenSecret(ctx context.Context, id, secretHash, secretPrefix string, updatedAt time.Time) error {
	for i, record := range f.records {
		if record.ID == id {
			f.records[i].SecretHash = secretHash
			f.records[i].SecretPrefix = secretPrefix
			f.records[i].UpdatedAt = updatedAt
			return nil
		}
	}
	return store.ErrNotFound
}

func (f *fakeTokens) DeleteToken(ctx context.Context, id string) error {
	for i, record := range f.records {
		if record.ID == id {
			f.records = append(f.records[:i], f.records[i+1:]...)
			return nil
		}
	}
	return store.ErrNotFound
}

type fakeUsage struct {
	events []usage.Event
}

func (f *fakeUsage) Record(event usage.Event) error {
	f.events = append(f.events, event)
	return nil
}

func (f *fakeUsage) ByUser(userID string) []usage.Event {
	out := make([]usage.Event, 0)
	for _, event := range f.events {
		if event.UserID == userID {
			out = append(out, event)
		}
	}
	return out
}

func (f *fakeUsage) All() []usage.Event {
	out := make([]usage.Event, len(f.events))
	copy(out, f.events)
	return out
}

func (f *fakeUsage) Query(q usage.Query) (usage.Page, error) {
	return f.recorder().Query(q)
}

func (f *fakeUsage) Stats(q usage.Query) (usage.Stats, error) {
	return f.recorder().Stats(q)
}

func (f *fakeUsage) TimeSeries(q usage.Query, bucketSecs int) (usage.TimeSeries, error) {
	return f.recorder().TimeSeries(q, bucketSecs)
}

func (f *fakeUsage) UpdateUsageEventEnergy(ctx context.Context, id string, energyWh, marginalWh float64, source string) error {
	rec := f.recorder()
	if err := rec.UpdateUsageEventEnergy(ctx, id, energyWh, marginalWh, source); err != nil {
		return err
	}
	f.events = rec.All()
	return nil
}

func (f *fakeUsage) UnpricedUsageEvents(ctx context.Context, notBefore, notAfter time.Time, limit int) ([]usage.Event, error) {
	return f.recorder().UnpricedUsageEvents(ctx, notBefore, notAfter, limit)
}

func (f *fakeUsage) UsageEventsForServerWindow(ctx context.Context, serverID string, from, to time.Time) ([]usage.Event, error) {
	return f.recorder().UsageEventsForServerWindow(ctx, serverID, from, to)
}

func (f *fakeUsage) EnergyByServer(ctx context.Context, q usage.Query) (map[string]float64, error) {
	return f.recorder().EnergyByServer(ctx, q)
}

func (f *fakeUsage) UsageGroups(ctx context.Context, q usage.Query, groupBy string) ([]usage.GroupBucket, error) {
	return f.recorder().UsageGroups(ctx, q, groupBy)
}

// recorder replays the fake's events into a real usage.Recorder so Query/Stats
// reuse the production filter/sort/paginate/histogram semantics.
func (f *fakeUsage) recorder() *usage.Recorder {
	rec := usage.NewRecorder()
	for _, event := range f.events {
		rec.Record(event)
	}
	return rec
}

func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }

func TestCreateTokenModelOverride(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	dir := NewMemoryDirectory(auth.NewTokenStore())
	if err := dir.CreateUser(ctx, store.User{ID: "usr_1", Email: "u1@example.test", DisplayName: "U1", Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	svc := NewService(ServiceDeps{Users: dir, Tokens: dir, Clock: func() time.Time { return now }})
	owner := auth.Token{UserID: "usr_1", Scopes: []string{"gateway:use"}}
	const seedModel = "gpt-oss-20b"

	resp, err := svc.CreateToken(ctx, owner, CreateTokenRequest{Name: "T1", ModelOverride: seedModel, LogCommunication: true})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if resp.Token.ModelOverride != seedModel || !resp.Token.LogCommunication {
		t.Fatalf("dto = %#v", resp.Token)
	}

	_, err = svc.CreateToken(ctx, owner, CreateTokenRequest{Name: "T2", ModelOverride: "does-not-exist"})
	if !errors.Is(err, ErrTokenModelOverrideInvalid) {
		t.Fatalf("want ErrTokenModelOverrideInvalid, got %v", err)
	}

	if _, err := svc.CreateToken(ctx, owner, CreateTokenRequest{Name: "T3"}); err != nil {
		t.Fatalf("empty override CreateToken: %v", err)
	}
}

func TestCreateTokenModelOverrideMap(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	dir := NewMemoryDirectory(auth.NewTokenStore())
	if err := dir.CreateUser(ctx, store.User{ID: "usr_1", Email: "u1@example.test", DisplayName: "U1", Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	svc := NewService(ServiceDeps{Users: dir, Tokens: dir, Clock: func() time.Time { return now }})
	owner := auth.Token{UserID: "usr_1", Scopes: []string{"gateway:use"}}
	const seedModel = "gpt-oss-20b"

	// A valid map (each value a known model) + a catch-all + fully-empty rows dropped.
	resp, err := svc.CreateToken(ctx, owner, CreateTokenRequest{
		Name:             "M1",
		ModelOverride:    seedModel, // catch-all
		ModelOverrideMap: map[string]store.ModelOverrideRule{"gpt-4o": {To: seedModel}, "claude": {To: seedModel}, "": {}},
	})
	if err != nil {
		t.Fatalf("CreateToken with map: %v", err)
	}
	if len(resp.Token.ModelOverrideMap) != 2 || resp.Token.ModelOverrideMap["gpt-4o"].To != seedModel || resp.Token.ModelOverrideMap["claude"].To != seedModel {
		t.Fatalf("dto map = %#v, want 2 entries mapping to %q (empty row dropped)", resp.Token.ModelOverrideMap, seedModel)
	}
	if resp.Token.ModelOverride != seedModel {
		t.Fatalf("catch-all = %q, want %q", resp.Token.ModelOverride, seedModel)
	}
	// The map round-trips through a re-fetch (store scan → DTO).
	list, err := svc.ListTokens(ctx, owner)
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	var found bool
	for _, d := range list.Data {
		if d.ID == resp.Token.ID {
			found = true
			if len(d.ModelOverrideMap) != 2 {
				t.Fatalf("refetched map = %#v, want 2 entries", d.ModelOverrideMap)
			}
		}
	}
	if !found {
		t.Fatalf("created token not in list")
	}

	// A map value that is not a known model is rejected.
	if _, err := svc.CreateToken(ctx, owner, CreateTokenRequest{Name: "M2", ModelOverrideMap: map[string]store.ModelOverrideRule{"x": {To: "does-not-exist"}}}); !errors.Is(err, ErrTokenModelOverrideInvalid) {
		t.Fatalf("unknown map target: want ErrTokenModelOverrideInvalid, got %v", err)
	}
	// A half-filled row (key without a value) is rejected.
	if _, err := svc.CreateToken(ctx, owner, CreateTokenRequest{Name: "M3", ModelOverrideMap: map[string]store.ModelOverrideRule{"x": {}}}); !errors.Is(err, ErrTokenModelOverrideInvalid) {
		t.Fatalf("half-filled row: want ErrTokenModelOverrideInvalid, got %v", err)
	}
}

func TestUpdateTokenModelOverrideMap(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	dir := NewMemoryDirectory(auth.NewTokenStore())
	if err := dir.CreateUser(ctx, store.User{ID: "usr_1", Email: "u1@example.test", DisplayName: "U1", Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	svc := NewService(ServiceDeps{Users: dir, Tokens: dir, Clock: func() time.Time { return now }})
	owner := auth.Token{UserID: "usr_1", Scopes: []string{"gateway:use"}}
	const seedModel = "gpt-oss-20b"

	created, err := svc.CreateToken(ctx, owner, CreateTokenRequest{Name: "U"})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	// Patch in a map.
	m := map[string]store.ModelOverrideRule{"alias": {To: seedModel}}
	dto, err := svc.UpdateToken(ctx, owner, created.Token.ID, UpdateTokenRequest{ModelOverrideMap: &m})
	if err != nil {
		t.Fatalf("UpdateToken map: %v", err)
	}
	if len(dto.ModelOverrideMap) != 1 || dto.ModelOverrideMap["alias"].To != seedModel {
		t.Fatalf("patched map = %#v", dto.ModelOverrideMap)
	}
	// Clearing it (empty map) removes all entries.
	empty := map[string]store.ModelOverrideRule{}
	dto, err = svc.UpdateToken(ctx, owner, created.Token.ID, UpdateTokenRequest{ModelOverrideMap: &empty})
	if err != nil {
		t.Fatalf("UpdateToken clear map: %v", err)
	}
	if len(dto.ModelOverrideMap) != 0 {
		t.Fatalf("cleared map = %#v, want empty", dto.ModelOverrideMap)
	}
	// An invalid target rejects the patch.
	bad := map[string]store.ModelOverrideRule{"x": {To: "nope"}}
	if _, err := svc.UpdateToken(ctx, owner, created.Token.ID, UpdateTokenRequest{ModelOverrideMap: &bad}); !errors.Is(err, ErrTokenModelOverrideInvalid) {
		t.Fatalf("want ErrTokenModelOverrideInvalid, got %v", err)
	}
}

func TestTokenSecretFlag(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	dir := NewMemoryDirectory(auth.NewTokenStore())
	if err := dir.CreateUser(ctx, store.User{ID: "usr_1", Email: "u1@example.test", DisplayName: "U1", Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	svc := NewService(ServiceDeps{Users: dir, Tokens: dir, Clock: func() time.Time { return now }})
	owner := auth.Token{UserID: "usr_1", Scopes: []string{"gateway:use"}}

	resp, err := svc.CreateToken(ctx, owner, CreateTokenRequest{Name: "T1", Secret: true})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if !resp.Token.Secret {
		t.Fatalf("create dto Secret = false, want true (%#v)", resp.Token)
	}

	runAs, err := svc.AuthorizeRunAsToken(ctx, owner, resp.Token.ID)
	if err != nil {
		t.Fatalf("AuthorizeRunAsToken: %v", err)
	}
	if !runAs.Secret {
		t.Fatalf("runAs.Secret = false, want true (run-as must carry the token secret flag)")
	}

	dto, err := svc.UpdateToken(ctx, owner, resp.Token.ID, UpdateTokenRequest{Secret: boolPtr(false)})
	if err != nil {
		t.Fatalf("UpdateToken: %v", err)
	}
	if dto.Secret {
		t.Fatalf("update dto Secret = true, want false (%#v)", dto)
	}
}

func TestUpdateTokenModelOverride(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	dir := NewMemoryDirectory(auth.NewTokenStore())
	if err := dir.CreateUser(ctx, store.User{ID: "usr_1", Email: "u1@example.test", DisplayName: "U1", Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	svc := NewService(ServiceDeps{Users: dir, Tokens: dir, Clock: func() time.Time { return now }})
	owner := auth.Token{UserID: "usr_1", Scopes: []string{"gateway:use"}}
	const seedModel = "gpt-oss-20b"

	created, err := svc.CreateToken(ctx, owner, CreateTokenRequest{Name: "T1"})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	dto, err := svc.UpdateToken(ctx, owner, created.Token.ID, UpdateTokenRequest{ModelOverride: strPtr(seedModel), LogCommunication: boolPtr(true)})
	if err != nil {
		t.Fatalf("UpdateToken: %v", err)
	}
	if dto.ModelOverride != seedModel || !dto.LogCommunication {
		t.Fatalf("dto = %#v", dto)
	}

	if _, err := svc.UpdateToken(ctx, owner, created.Token.ID, UpdateTokenRequest{ModelOverride: strPtr("nope")}); !errors.Is(err, ErrTokenModelOverrideInvalid) {
		t.Fatalf("want ErrTokenModelOverrideInvalid, got %v", err)
	}
}

// testSystemGroupID/testAdminGroupID are the fixed ids of the system/admin
// group pair seedServerTestGroups plants: usr_admin OWNS testAdminGroupID, so
// adminToken() satisfies CreateServer's serverManageGroupIDs gate (Phase B,
// spec 2026-08-10) and every CreateServerRequest literal in this package
// that needs to succeed can reference AdminGroupIDs: []string{testAdminGroupID}.
const (
	testSystemGroupID = "ugrp_srvtest_sys"
	testAdminGroupID  = "ugrp_srvtest_admin"
)

// seedServerTestGroups plants the fixed system/admin group pair (see
// testSystemGroupID/testAdminGroupID) directly at the store layer (no FK
// checks on MemoryDirectory) into dir, owned by usr_admin. Every
// newServerTestService* variant calls this so adminToken()'s CreateServer
// calls satisfy the Phase B admin-group-linkage gate/requirement.
func seedServerTestGroups(t *testing.T, dir *MemoryDirectory, now time.Time) {
	t.Helper()
	if err := dir.CreateUserGroup(context.Background(), store.UserGroup{
		ID: testSystemGroupID, Tier: store.GroupTierSystem, Name: "Test System",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed system group: %v", err)
	}
	if err := dir.CreateUserGroup(context.Background(), store.UserGroup{
		ID: testAdminGroupID, Tier: store.GroupTierAdmin, Name: "Test Admin",
		ParentGroupID: testSystemGroupID, OwnerUserID: "usr_admin",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed admin group: %v", err)
	}
	// serverManageGroupIDs enumerates via UserGroupsForUser, a MEMBERSHIP
	// query -- an owner who isn't ALSO a member row (mirroring CreateGroup's
	// real auto-enroll-owner-as-member behavior) would not surface, so a bare
	// OwnerUserID assignment above is not enough.
	if err := dir.SetUserGroupMember(context.Background(), testAdminGroupID, "usr_admin", store.GroupStateMember, ""); err != nil {
		t.Fatalf("seed admin group owner membership: %v", err)
	}
}

func newServerTestService(t *testing.T, now time.Time) (*Service, *routing.MemoryStore) {
	t.Helper()
	svc, routeStore, _ := newServerTestServiceWithDir(t, now)
	return svc, routeStore
}

// newServerTestServiceWithDir mirrors newServerTestService but additionally
// returns the underlying MemoryDirectory, for a test that needs to seed an
// EXTRA group beyond the fixed testSystemGroupID/testAdminGroupID pair (e.g.
// one usr_admin/adminToken() deliberately does NOT manage).
func newServerTestServiceWithDir(t *testing.T, now time.Time) (*Service, *routing.MemoryStore, *MemoryDirectory) {
	t.Helper()
	dir := NewMemoryDirectory(auth.NewTokenStore())
	for _, u := range []string{"usr_admin", "usr_owner", "usr_other"} {
		if err := dir.CreateUser(context.Background(), store.User{ID: u, Email: u + "@example.test", DisplayName: u, Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateUser %s: %v", u, err)
		}
	}
	seedServerTestGroups(t, dir, now)
	routeStore := routing.NewMemoryStore()
	svc := NewService(ServiceDeps{Users: dir, Groups: dir, Routes: routeStore, Clock: func() time.Time { return now }})
	return svc, routeStore, dir
}

func adminToken() auth.Token {
	return auth.Token{UserID: "usr_admin", Scopes: []string{"gateway:use", "admin"}}
}

// systemToken is a system_admin-scoped principal (isSystem-true), for the
// PT-2 Part 2 internal-authz guards on the token-less mutating methods that
// require "system" rather than plain "admin" (SetServerNetbird,
// UpdateSystemSettings).
func systemToken() auth.Token {
	return auth.Token{UserID: "usr_system", Scopes: []string{"gateway:use", "admin", "system"}}
}

func ownerToken() auth.Token { return auth.Token{UserID: "usr_owner", Scopes: []string{"gateway:use"}} }

func otherToken() auth.Token { return auth.Token{UserID: "usr_other", Scopes: []string{"gateway:use"}} }

func TestValidateTokenScopesRejectsAgentReport(t *testing.T) {
	admin := auth.Token{UserID: "usr_admin", Scopes: []string{"gateway:use", "admin"}}
	if _, err := validateTokenScopes(admin, []string{"agent:report"}); !errors.Is(err, ErrTokenScopeInvalid) {
		t.Fatalf("agent:report scope = %v, want ErrTokenScopeInvalid", err)
	}
	if _, err := validateTokenScopes(admin, []string{"admin", "gateway:use"}); err != nil {
		t.Fatalf("admin/gateway:use = %v, want nil", err)
	}
}

func TestCreateServerAdminOnlyWithDomainAndOwners(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)

	// non-admin cannot create
	if _, err := svc.CreateServer(context.Background(), ownerToken(), CreateServerRequest{Name: "S", Domain: "s.example.test"}); !errors.Is(err, ErrServerForbidden) {
		t.Fatalf("owner create err = %v, want ErrServerForbidden", err)
	}
	// domain required
	if _, err := svc.CreateServer(context.Background(), adminToken(), CreateServerRequest{Name: "S", Domain: "  "}); !errors.Is(err, ErrServerDomainRequired) {
		t.Fatalf("blank domain err = %v", err)
	}
	// unknown owner rejected
	if _, err := svc.CreateServer(context.Background(), adminToken(), CreateServerRequest{Name: "S", Domain: "s.example.test", OwnerIDs: []string{"usr_ghost"}}); !errors.Is(err, ErrServerOwnerInvalid) {
		t.Fatalf("ghost owner err = %v", err)
	}
	// happy path
	dto, err := svc.CreateServer(context.Background(), adminToken(), CreateServerRequest{Name: "GPU 1", Domain: "gpu1.example.test", OwnerIDs: []string{"usr_owner"}, Status: "active", AdminGroupIDs: []string{testAdminGroupID}})
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	if dto.Name != "GPU 1" || dto.Domain != "gpu1.example.test" || len(dto.Owners) != 1 || dto.Owners[0].ID != "usr_owner" {
		t.Fatalf("dto = %#v", dto)
	}
}

// TestListServersScopedByRole: a SYSTEM-scope principal sees every server
// unconditionally, incl. the ungrouped+unowned one; a PLAIN admin (no
// "system" scope) is NO LONGER globally visible (Phase B, spec 2026-08-10,
// Decision 4) -- with neither an admin-group link nor ownership on either
// server they see NOTHING, the intentional behavior change replacing the
// prior "any admin manages every server" global bypass. An owner still sees
// only what they own.
func TestListServersScopedByRole(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	svc, _, dir := newServerTestServiceWithDir(t, now)
	// A SEPARATE admin group usr_admin (adminToken()) neither owns nor
	// co-manages -- linking the two test servers to testAdminGroupID (which
	// adminToken() DOES manage, per seedServerTestGroups) would make them
	// visible to the "plain admin" branch below and defeat the point of this
	// test, so a system-scope actor links them to this OTHER group instead.
	otherSG := store.UserGroup{ID: "ugrp_other_sys", Tier: store.GroupTierSystem, Name: "OtherSys", CreatedAt: now, UpdatedAt: now}
	if err := dir.CreateUserGroup(context.Background(), otherSG); err != nil {
		t.Fatalf("create other system group: %v", err)
	}
	otherAG := store.UserGroup{ID: "ugrp_other_admin", Tier: store.GroupTierAdmin, Name: "OtherAdmin", ParentGroupID: otherSG.ID, OwnerUserID: "usr_other", CreatedAt: now, UpdatedAt: now}
	if err := dir.CreateUserGroup(context.Background(), otherAG); err != nil {
		t.Fatalf("create other admin group: %v", err)
	}
	if _, err := svc.CreateServer(context.Background(), systemAdminToken(), CreateServerRequest{Name: "Owned", Domain: "o.example.test", OwnerIDs: []string{"usr_owner"}, AdminGroupIDs: []string{otherAG.ID}}); err != nil {
		t.Fatalf("create owned: %v", err)
	}
	if _, err := svc.CreateServer(context.Background(), systemAdminToken(), CreateServerRequest{Name: "Unowned", Domain: "u.example.test", AdminGroupIDs: []string{otherAG.ID}}); err != nil {
		t.Fatalf("create unowned: %v", err)
	}
	sysList, err := svc.ListServers(context.Background(), systemAdminToken())
	if err != nil {
		t.Fatalf("ListServers(system): %v", err)
	}
	if len(sysList.Data) != 2 {
		t.Fatalf("system sees %d, want 2", len(sysList.Data))
	}
	adminList, err := svc.ListServers(context.Background(), adminToken())
	if err != nil {
		t.Fatalf("ListServers(admin): %v", err)
	}
	if len(adminList.Data) != 0 {
		t.Fatalf("plain admin (no system scope, no group/ownership link) sees %d, want 0", len(adminList.Data))
	}
	ownerList, err := svc.ListServers(context.Background(), ownerToken())
	if err != nil {
		t.Fatalf("ListServers(owner): %v", err)
	}
	if len(ownerList.Data) != 1 || ownerList.Data[0].Name != "Owned" {
		t.Fatalf("owner list = %#v", ownerList.Data)
	}
}

func TestUpdateAndDeleteServerRBAC(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)
	created, _ := svc.CreateServer(context.Background(), adminToken(), CreateServerRequest{Name: "S", Domain: "s.example.test", OwnerIDs: []string{"usr_owner"}, AdminGroupIDs: []string{testAdminGroupID}})

	// non-owner non-admin: not found
	newName := "Hacked"
	if _, err := svc.UpdateServer(context.Background(), otherToken(), created.ID, UpdateServerRequest{Name: &newName}); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("other update err = %v", err)
	}
	// owner may edit name/domain/status
	okName := "Renamed"
	if _, err := svc.UpdateServer(context.Background(), ownerToken(), created.ID, UpdateServerRequest{Name: &okName}); err != nil {
		t.Fatalf("owner update: %v", err)
	}
	// owner may NOT change owners
	if _, err := svc.UpdateServer(context.Background(), ownerToken(), created.ID, UpdateServerRequest{OwnerIDs: &[]string{"usr_other"}}); !errors.Is(err, ErrServerForbidden) {
		t.Fatalf("owner changing owners err = %v, want ErrServerForbidden", err)
	}
	// a system-scope principal may change owners (Phase B: a plain admin with
	// no ownership/group link now gets ErrServerNotFound here too -- see
	// TestAuthorizeServerMatrix).
	if _, err := svc.UpdateServer(context.Background(), systemAdminToken(), created.ID, UpdateServerRequest{OwnerIDs: &[]string{"usr_other"}}); err != nil {
		t.Fatalf("system-admin change owners: %v", err)
	}
	// other (now owner) can delete; unrelated cannot
	if _, err := svc.DeleteServer(context.Background(), ownerToken(), created.ID, false); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("ex-owner delete err = %v, want ErrServerNotFound", err)
	}
	if _, err := svc.DeleteServer(context.Background(), systemAdminToken(), created.ID, false); err != nil {
		t.Fatalf("system-admin delete: %v", err)
	}
}

func TestUpdateServerValidatesOwnersBeforePersisting(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)
	created, _ := svc.CreateServer(context.Background(), adminToken(), CreateServerRequest{Name: "Original", Domain: "orig.example.test", OwnerIDs: []string{"usr_owner"}, AdminGroupIDs: []string{testAdminGroupID}})

	newName := "ShouldNotStick"
	ghost := []string{"usr_ghost"}
	if _, err := svc.UpdateServer(context.Background(), systemAdminToken(), created.ID, UpdateServerRequest{Name: &newName, OwnerIDs: &ghost}); !errors.Is(err, ErrServerOwnerInvalid) {
		t.Fatalf("update err = %v, want ErrServerOwnerInvalid", err)
	}
	got, err := svc.GetServer(context.Background(), systemAdminToken(), created.ID)
	if err != nil {
		t.Fatalf("GetServer: %v", err)
	}
	if got.Name != "Original" {
		t.Fatalf("name must be unchanged after failed owner validation, got %q", got.Name)
	}
}

func TestUpdateServerRejectsBlankNameAndBadStatus(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)
	created, _ := svc.CreateServer(context.Background(), adminToken(), CreateServerRequest{Name: "S", Domain: "s.example.test", OwnerIDs: []string{"usr_owner"}, AdminGroupIDs: []string{testAdminGroupID}})
	blank := "   "
	if _, err := svc.UpdateServer(context.Background(), ownerToken(), created.ID, UpdateServerRequest{Name: &blank}); !errors.Is(err, ErrServerNameRequired) {
		t.Fatalf("blank name err = %v", err)
	}
	bad := "banished"
	if _, err := svc.UpdateServer(context.Background(), ownerToken(), created.ID, UpdateServerRequest{Status: &bad}); !errors.Is(err, ErrServerStatusInvalid) {
		t.Fatalf("bad status err = %v", err)
	}
}

func TestAuthorizeRunAsTokenCarriesModelOverride(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	dir := NewMemoryDirectory(auth.NewTokenStore())
	if err := dir.CreateUser(ctx, store.User{ID: "usr_1", Email: "u1@example.test", DisplayName: "U1", Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	svc := NewService(ServiceDeps{Users: dir, Tokens: dir, Clock: func() time.Time { return now }})
	owner := auth.Token{UserID: "usr_1", Scopes: []string{"gateway:use"}}

	resp, err := svc.CreateToken(ctx, owner, CreateTokenRequest{Name: "ovr", ModelOverride: "gpt-oss-20b"})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	runAs, err := svc.AuthorizeRunAsToken(ctx, owner, resp.Token.ID)
	if err != nil {
		t.Fatalf("AuthorizeRunAsToken: %v", err)
	}
	if runAs.ModelOverride != "gpt-oss-20b" {
		t.Fatalf("runAs.ModelOverride = %q, want gpt-oss-20b", runAs.ModelOverride)
	}

	plain, err := svc.CreateToken(ctx, owner, CreateTokenRequest{Name: "plain"})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	runAs2, err := svc.AuthorizeRunAsToken(ctx, owner, plain.Token.ID)
	if err != nil {
		t.Fatalf("AuthorizeRunAsToken: %v", err)
	}
	if runAs2.ModelOverride != "" {
		t.Fatalf("runAs2.ModelOverride = %q, want empty", runAs2.ModelOverride)
	}
}

func TestAuthorizeRunAsTokenCarriesLogCommunication(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	dir := NewMemoryDirectory(auth.NewTokenStore())
	if err := dir.CreateUser(ctx, store.User{ID: "usr_1", Email: "u1@example.test", DisplayName: "U1", Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	svc := NewService(ServiceDeps{Users: dir, Tokens: dir, Clock: func() time.Time { return now }})
	owner := auth.Token{UserID: "usr_1", Scopes: []string{"gateway:use"}}

	logging, err := svc.CreateToken(ctx, owner, CreateTokenRequest{Name: "logcap", LogCommunication: true})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	runAs, err := svc.AuthorizeRunAsToken(ctx, owner, logging.Token.ID)
	if err != nil {
		t.Fatalf("AuthorizeRunAsToken: %v", err)
	}
	if !runAs.LogCommunication {
		t.Fatalf("runAs.LogCommunication = false, want true")
	}

	plain, err := svc.CreateToken(ctx, owner, CreateTokenRequest{Name: "plain"})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	runAs2, err := svc.AuthorizeRunAsToken(ctx, owner, plain.Token.ID)
	if err != nil {
		t.Fatalf("AuthorizeRunAsToken: %v", err)
	}
	if runAs2.LogCommunication {
		t.Fatalf("runAs2.LogCommunication = true, want false")
	}
}

type stubCaptureReader struct{}

func (stubCaptureReader) Capture(ctx context.Context, usageEventID string) (store.CaptureRow, error) {
	return store.CaptureRow{}, nil
}

func (stubCaptureReader) HasCaptures(ctx context.Context, ids []string) (map[string]store.CapturePresence, error) {
	return map[string]store.CapturePresence{}, nil
}

func (stubCaptureReader) DeleteCapture(ctx context.Context, usageEventID string) error {
	return nil
}

func (stubCaptureReader) SetCaptureSecret(ctx context.Context, usageEventID string, secret bool) error {
	return nil
}

// hasCapturesStub is a minimal CaptureReader double for exercising
// Service.Usage's has_capture enrichment in isolation from any real store:
// Capture is unused (Service.Usage never calls it), HasCaptures returns the
// injected map/error so tests can control the lookup outcome directly.
type hasCapturesStub struct {
	has map[string]store.CapturePresence
	err error
}

func (hasCapturesStub) Capture(ctx context.Context, usageEventID string) (store.CaptureRow, error) {
	return store.CaptureRow{}, nil
}

func (h hasCapturesStub) HasCaptures(ctx context.Context, ids []string) (map[string]store.CapturePresence, error) {
	return h.has, h.err
}

func (hasCapturesStub) DeleteCapture(ctx context.Context, usageEventID string) error {
	return nil
}

func (hasCapturesStub) SetCaptureSecret(ctx context.Context, usageEventID string, secret bool) error {
	return nil
}

// *store.SQLiteStore must satisfy the portal capture read interface (P6 wires it).
var (
	_ CaptureReader = (*store.SQLiteStore)(nil)
	_ CaptureReader = (*store.MemoryCaptureStore)(nil)
)

// Both stores must satisfy the per-user chat store interface: SQLiteStore
// (persistent) and MemoryChatStore (volatile RAM fallback).
var (
	_ ChatStore = (*store.SQLiteStore)(nil)
	_ ChatStore = (*store.MemoryChatStore)(nil)
)

// *store.SQLiteStore must satisfy the per-user UI preferences store interface.
var _ UIPreferencesStore = (*store.SQLiteStore)(nil)

func TestNewServiceCapturesNilUnlessProvided(t *testing.T) {
	if NewService(ServiceDeps{}).captures != nil {
		t.Fatalf("captures = non-nil, want nil when unset (fail-closed)")
	}
	if NewService(ServiceDeps{Captures: stubCaptureReader{}}).captures == nil {
		t.Fatalf("captures = nil, want the provided reader propagated")
	}
}

func TestDisplayNamesResolvesDedupesAndSkips(t *testing.T) {
	users := &fakeUsers{byID: map[string]store.User{
		"usr_1": {ID: "usr_1", DisplayName: "Alice Admin"},
		"usr_2": {ID: "usr_2", DisplayName: "Bob Builder"},
		"usr_3": {ID: "usr_3", DisplayName: ""}, // known but no display name
	}}
	svc := NewService(ServiceDeps{Users: users})

	// Includes a known id twice (dedupe), an empty id, an unknown id, and a
	// known id whose display name is empty.
	got := svc.DisplayNames(context.Background(), []string{
		"usr_1", "usr_1", "usr_2", "", "usr_unknown", "usr_3",
	})

	want := map[string]string{
		"usr_1": "Alice Admin",
		"usr_2": "Bob Builder",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DisplayNames = %#v, want %#v", got, want)
	}
}

func TestDisplayNamesEmptyInputReturnsEmptyMap(t *testing.T) {
	svc := NewService(ServiceDeps{Users: &fakeUsers{byID: map[string]store.User{}}})
	got := svc.DisplayNames(context.Background(), nil)
	if got == nil || len(got) != 0 {
		t.Fatalf("DisplayNames(nil) = %#v, want empty non-nil map", got)
	}
}

func TestApplyUsageScopeUserAndTokenFilter(t *testing.T) {
	svc := NewService(ServiceDeps{})
	ctx := context.Background()
	admin := auth.Token{UserID: "usr_admin", Scopes: []string{"gateway:use", "admin"}}
	plain := auth.Token{UserID: "usr_plain", Scopes: []string{"gateway:use"}}

	// Admin, scope=all, pinned to a specific user: the pin wins over all-scope.
	q := usage.Query{ScopeAll: true, FilterUserID: "usr_target"}
	svc.applyUsageScope(ctx, &q, admin)
	if q.ScopeAll || q.UserID != "usr_target" {
		t.Fatalf("admin+all+filter = ScopeAll=%v UserID=%q, want false/usr_target", q.ScopeAll, q.UserID)
	}

	// Admin, scope=all, no user pin: stays all-scope.
	all := usage.Query{ScopeAll: true}
	svc.applyUsageScope(ctx, &all, admin)
	if !all.ScopeAll || all.UserID != "" {
		t.Fatalf("admin+all = ScopeAll=%v UserID=%q, want true/''", all.ScopeAll, all.UserID)
	}

	// Non-admin who smuggles a FilterUserID: ignored, pinned to own id.
	sneaky := usage.Query{ScopeAll: true, FilterUserID: "usr_target"}
	svc.applyUsageScope(ctx, &sneaky, plain)
	if sneaky.ScopeAll || sneaky.UserID != "usr_plain" {
		t.Fatalf("non-admin filter must be ignored, got ScopeAll=%v UserID=%q", sneaky.ScopeAll, sneaky.UserID)
	}

	// Token filter is always carried through untouched.
	tok := usage.Query{HasTokenFilter: true, TokenID: ""}
	svc.applyUsageScope(ctx, &tok, plain)
	if !tok.HasTokenFilter || tok.TokenID != "" {
		t.Fatalf("token filter must survive scoping, got %+v", tok)
	}
}

// TestServerPathSuffixRoundTrip: server_path_suffix trims + round-trips through
// create, the DTO, a reload, and an update; a URL-shaped value is rejected 400.
func TestServerPathSuffixRoundTrip(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)

	dto, err := svc.CreateServer(context.Background(), adminToken(), CreateServerRequest{
		Name: "S", Domain: "s.example.test", ServerPathSuffix: "  models/ ", OwnerIDs: []string{"usr_owner"},
		AdminGroupIDs: []string{testAdminGroupID},
	})
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	if dto.ServerPathSuffix != "models/" {
		t.Fatalf("server_path_suffix = %q, want trimmed %q", dto.ServerPathSuffix, "models/")
	}
	// GetServer/UpdateServer as OWNER (usr_owner) below -- ownership is
	// unaffected by the Phase B group-scoping rewrite.
	got, err := svc.GetServer(context.Background(), ownerToken(), dto.ID)
	if err != nil {
		t.Fatalf("GetServer: %v", err)
	}
	if got.ServerPathSuffix != "models/" {
		t.Fatalf("reloaded server_path_suffix = %q", got.ServerPathSuffix)
	}

	// Update replaces the suffix.
	upd, err := svc.UpdateServer(context.Background(), ownerToken(), dto.ID, UpdateServerRequest{
		ServerPathSuffix: strPtr("api/v2"),
	})
	if err != nil {
		t.Fatalf("UpdateServer: %v", err)
	}
	if upd.ServerPathSuffix != "api/v2" {
		t.Fatalf("updated server_path_suffix = %q, want api/v2", upd.ServerPathSuffix)
	}
}

func TestServerPathSuffixRejectsURL(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)

	// create: URL-shaped suffix rejected.
	if _, err := svc.CreateServer(context.Background(), adminToken(), CreateServerRequest{
		Name: "S", Domain: "s.example.test", ServerPathSuffix: "https://evil.example",
	}); !errors.Is(err, ErrPathSuffixInvalid) {
		t.Fatalf("create bad server path err = %v, want ErrPathSuffixInvalid", err)
	}

	// update: URL-shaped suffix rejected.
	dto, err := svc.CreateServer(context.Background(), adminToken(), CreateServerRequest{
		Name: "S2", Domain: "s2.example.test", OwnerIDs: []string{"usr_owner"},
		AdminGroupIDs: []string{testAdminGroupID},
	})
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	if _, err := svc.UpdateServer(context.Background(), ownerToken(), dto.ID, UpdateServerRequest{
		ServerPathSuffix: strPtr("http://x"),
	}); !errors.Is(err, ErrPathSuffixInvalid) {
		t.Fatalf("update bad server path err = %v, want ErrPathSuffixInvalid", err)
	}
}

// seedAvailabilitySamples inserts distinct-state availability samples into the
// route store at now-offsets so the transition-preserving reduction keeps them.
func seedAvailabilitySamples(t *testing.T, routeStore *routing.MemoryStore, serverID string, now time.Time) {
	t.Helper()
	samples := []routing.ServerAvailabilitySample{
		{ServerID: serverID, ReportedAt: now.Add(-30 * time.Minute), Health: routing.HealthHealthy, ReachableCount: 1, ActiveCount: 1, AgentReporting: true},
		{ServerID: serverID, ReportedAt: now.Add(-20 * time.Minute), Health: routing.HealthUnhealthy, ReachableCount: 0, ActiveCount: 1, AgentReporting: true},
		{ServerID: serverID, ReportedAt: now.Add(-10 * time.Minute), Health: routing.HealthHealthy, ReachableCount: 1, ActiveCount: 1, AgentReporting: false},
	}
	for i, s := range samples {
		if err := routeStore.InsertServerAvailabilitySample(context.Background(), s); err != nil {
			t.Fatalf("InsertServerAvailabilitySample[%d]: %v", i, err)
		}
	}
}

// TestServerAvailabilityAuthorizesAndDelegates: an owner reading a window gets an
// owner-gated, ascending, window-bounded slice delegated to the store's
// transition-preserving reduction (three distinct states are all kept).
func TestServerAvailabilityAuthorizesAndDelegates(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	seedAvailabilitySamples(t, routeStore, server.ID, now)

	got, err := svc.ServerAvailability(context.Background(), ownerToken(), server.ID, time.Hour)
	if err != nil {
		t.Fatalf("ServerAvailability (owner): %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3 (three distinct states preserved)", len(got))
	}
	if got[0].Health != routing.HealthHealthy || !got[0].AgentReporting || got[0].ReachableCount != 1 {
		t.Fatalf("got[0] = %#v, want healthy/agent=true/reachable=1", got[0])
	}
	from := now.Add(-time.Hour)
	for i, s := range got {
		if s.ReportedAt.Before(from) || s.ReportedAt.After(now) {
			t.Fatalf("sample[%d].ReportedAt %v outside [%v, %v]", i, s.ReportedAt, from, now)
		}
		if i > 0 && !s.ReportedAt.After(got[i-1].ReportedAt) {
			t.Fatalf("samples not ascending at %d", i)
		}
	}
}

// TestServerAvailabilityAdminAllowed: a SYSTEM-scope principal is authorized
// even without ownership (Phase B, spec 2026-08-10: the system bypass is
// unconditional; a plain admin with no ownership/group link is NOT -- see
// TestAuthorizeServerMatrix).
func TestServerAvailabilityAdminAllowed(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")

	if _, err := svc.ServerAvailability(context.Background(), systemAdminToken(), server.ID, time.Hour); err != nil {
		t.Fatalf("system-admin ServerAvailability = %v, want nil", err)
	}
}

// TestServerAvailabilityNonOwnerNotFound: a plain gateway:use principal who is
// neither owner nor admin gets ErrServerNotFound (no existence leak).
func TestServerAvailabilityNonOwnerNotFound(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")

	if _, err := svc.ServerAvailability(context.Background(), otherToken(), server.ID, time.Hour); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("non-owner ServerAvailability = %v, want ErrServerNotFound", err)
	}
}

// TestServerAvailabilityDefaultsWindow: a window <= 0 defaults to one hour, so a
// sample older than an hour is excluded while an in-window one is returned.
func TestServerAvailabilityDefaultsWindow(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	outOfWindow := now.Add(-90 * time.Minute)
	inWindow := now.Add(-30 * time.Minute)
	for _, s := range []routing.ServerAvailabilitySample{
		{ServerID: server.ID, ReportedAt: outOfWindow, Health: routing.HealthHealthy, ReachableCount: 1, ActiveCount: 1, AgentReporting: true},
		{ServerID: server.ID, ReportedAt: inWindow, Health: routing.HealthUnhealthy, ReachableCount: 0, ActiveCount: 0, AgentReporting: false},
	} {
		if err := routeStore.InsertServerAvailabilitySample(context.Background(), s); err != nil {
			t.Fatalf("InsertServerAvailabilitySample: %v", err)
		}
	}

	got, err := svc.ServerAvailability(context.Background(), ownerToken(), server.ID, 0)
	if err != nil {
		t.Fatalf("ServerAvailability (window 0): %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1 (default 1h window excludes the 90m-old sample)", len(got))
	}
	if !got[0].ReportedAt.Equal(inWindow) {
		t.Fatalf("got[0].ReportedAt = %v, want %v", got[0].ReportedAt, inWindow)
	}
}

// alwaysFailingSystemSettings is a SystemSettingsStore whose read ALWAYS fails,
// for the one case tlsProxyState must get right on pain of hiding an operator's
// switch: a settings glitch may report "unknown", never "out_of_scope".
type alwaysFailingSystemSettings struct {
	SystemSettingsStore
}

func (alwaysFailingSystemSettings) SystemSettings(context.Context) (map[string]string, error) {
	return nil, errors.New("boom: settings unreadable")
}

// TestTLSProxyState pins the four-valued visibility signal, and in particular
// the DIRECTION of its nil-safety, which is INVERTED relative to agentStatus and
// is the whole reason the function exists in this shape: there the pessimistic
// floor is the restrictive value, here the pessimistic floor ("unknown") is the
// one that KEEPS the per-application proxy control visible in the portal.
//
// Only "out_of_scope" hides that control, and only "out_of_scope" is derived
// purely from DURABLE state (a stored server column plus a stored setting), so
// it cannot appear out of nowhere after a restart. Everything volatile —
// including a missing reader, a missing report and an unrecognised mode — lands
// on "unknown" instead, because the agent's cert report is an in-RAM map built
// empty at startup and is therefore absent twice over: after every gateway
// restart, and on a freshly-provisioned proxy-mode agent before its first
// certificate. Those are exactly the moments an operator is most likely to be
// reaching for the control.
func TestTLSProxyState(t *testing.T) {
	ctx := context.Background()
	inScope := routing.AIServer{ID: "srv-in", Name: "In"}
	outOfScope := routing.AIServer{ID: "srv-out", Name: "Out", HTTPSSwitchOverride: "exclude"}

	newSvc := func(t *testing.T, mode string, reports AgentCertReportReader) *Service {
		t.Helper()
		svc, _ := newServerTestService(t, time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC))
		svc.settings = NewMemorySystemSettings()
		if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CertHTTPSSwitchMode: &mode}); err != nil {
			t.Fatalf("set https switch mode: %v", err)
		}
		svc.agentCertReports = reports
		return svc
	}
	reportsWithMode := func(mode string) AgentCertReportReader {
		return &fakeCertReports{byServer: map[string]fakeCertReport{"srv-in": {mode: mode}, "srv-out": {mode: mode}}}
	}

	cases := []struct {
		name    string
		mode    string
		server  routing.AIServer
		reports AgentCertReportReader
		want    string
	}{
		{"out of scope wins over a proxy report", "auto", outOfScope, reportsWithMode("proxy"), "out_of_scope"},
		{"out of scope with no report at all", "auto", outOfScope, nil, "out_of_scope"},
		{"manual mode puts the whole fleet out of scope", "manual", inScope, reportsWithMode("proxy"), "out_of_scope"},
		{"in scope, agent reports proxy", "auto", inScope, reportsWithMode("proxy"), "proxy"},
		{"in scope, agent reports off", "auto", inScope, reportsWithMode("off"), "agent_off"},
		{"in scope, agent reports files", "auto", inScope, reportsWithMode("files"), "agent_off"},
		{"in scope, no report (post-restart, or before the first leaf)", "auto", inScope, &fakeCertReports{byServer: map[string]fakeCertReport{}}, "unknown"},
		{"in scope, nil reader", "auto", inScope, nil, "unknown"},
		{"in scope, empty mode says nothing ABOUT the mode", "auto", inScope, reportsWithMode(""), "unknown"},
		{"in scope, unrecognised mode says nothing ABOUT the mode", "auto", inScope, reportsWithMode("sideways"), "unknown"},
	}
	for _, c := range cases {
		svc := newSvc(t, c.mode, c.reports)
		if got := svc.tlsProxyState(ctx, c.server); got != c.want {
			t.Errorf("%s: tlsProxyState = %q, want %q", c.name, got, c.want)
		}
	}

	// A settings READ ERROR must report "unknown" — never "out_of_scope", which
	// would hide the control on a transient glitch — and the same holds for a
	// nil settings reader.
	svc := newSvc(t, "auto", reportsWithMode("proxy"))
	svc.settings = alwaysFailingSystemSettings{}
	if got := svc.tlsProxyState(ctx, outOfScope); got != "unknown" {
		t.Fatalf("settings error on an out-of-scope server: tlsProxyState = %q, want unknown", got)
	}
	svc.settings = nil
	if got := svc.tlsProxyState(ctx, outOfScope); got != "unknown" {
		t.Fatalf("nil settings reader: tlsProxyState = %q, want unknown", got)
	}
}

// TestServerDTOCarriesTLSProxyState pins that the derived state actually
// reaches the wire, on the ordinary read path, and that a service with no
// settings reader at all still emits the control-preserving "unknown" rather
// than an empty string the frontend would have to guess about.
func TestServerDTOCarriesTLSProxyState(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	if server.TLSProxyState != "unknown" {
		t.Fatalf("tls_proxy_state = %q, want unknown when nothing can be read", server.TLSProxyState)
	}

	mode := "manual"
	svc.settings = NewMemorySystemSettings()
	if _, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{CertHTTPSSwitchMode: &mode}); err != nil {
		t.Fatalf("set mode: %v", err)
	}
	got, err := svc.GetServer(context.Background(), ownerToken(), server.ID)
	if err != nil {
		t.Fatalf("GetServer: %v", err)
	}
	if got.TLSProxyState != "out_of_scope" {
		t.Fatalf("tls_proxy_state = %q, want out_of_scope in manual mode", got.TLSProxyState)
	}
}
