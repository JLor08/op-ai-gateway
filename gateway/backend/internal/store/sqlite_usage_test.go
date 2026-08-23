// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"encoding/json"
	"op-ai-gateway/internal/usage"
	"strings"
	"testing"
	"time"
)

func TestSQLiteUsageRecordAndQueryByUser(t *testing.T) {
	st := openMigratedTestSQLite(t)
	defer st.Close()
	first := testUsageEvent("req_1", "usr_1", "tok_1", "success")
	second := testUsageEvent("req_2", "usr_2", "tok_2", "error")

	st.Record(first)
	st.Record(second)

	events := st.ByUser("usr_1")
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0] != first {
		t.Fatalf("event = %#v, want %#v", events[0], first)
	}
	if err := st.LastUsageError(); err != nil {
		t.Fatalf("LastUsageError = %v", err)
	}
}

func TestSQLiteUsageAllReturnsCopies(t *testing.T) {
	st := openMigratedTestSQLite(t)
	defer st.Close()
	event := testUsageEvent("req_1", "usr_1", "tok_1", "success")

	st.Record(event)

	events := st.All()
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	events[0].TotalTokens = 99

	stored := st.All()
	if len(stored) != 1 {
		t.Fatalf("stored events = %d, want 1", len(stored))
	}
	if stored[0].TotalTokens != event.TotalTokens {
		t.Fatalf("TotalTokens = %d, want %d", stored[0].TotalTokens, event.TotalTokens)
	}
}

func TestSQLiteUsageByUserFiltersAndPreservesFields(t *testing.T) {
	st := openMigratedTestSQLite(t)
	defer st.Close()
	event := testUsageEvent("req_1", "usr_1", "tok_1", "error")
	event.SessionID = "sess_1"
	event.APIFlavor = "anthropic"
	event.Model = "qwen-coder"
	event.Provider = "mock"
	event.Host = "mock-host"
	event.InputTokens = 7
	event.OutputTokens = 11
	event.TotalTokens = 18
	event.LatencyMS = 64
	event.ErrorCode = "provider.unavailable"

	st.Record(event)

	events := st.ByUser("usr_1")
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0] != event {
		t.Fatalf("event = %#v, want %#v", events[0], event)
	}
}

func TestSQLiteUsageStorePersistsRouteID(t *testing.T) {
	st := openMigratedTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	st.Record(usage.Event{ID: "req_route", UserID: "usr_dev", TokenID: "tok_dev", RouteID: "route_qwen", APIFlavor: "openai_chat", Model: "qwen-coder", Provider: "mock", Host: "mock-host", Status: "success", CreatedAt: now})

	events := st.All()

	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].RouteID != "route_qwen" {
		t.Fatalf("RouteID = %q, want route_qwen", events[0].RouteID)
	}
}

func TestSQLiteUsageStorePersistsProviderPath(t *testing.T) {
	st := openMigratedTestSQLite(t)
	defer st.Close()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	st.Record(usage.Event{ID: "req_pp", UserID: "usr_dev", TokenID: "tok_dev", APIFlavor: "anthropic_messages", Model: "qwen-coder", Provider: "vllm", Host: "h", Status: "success", ReqPath: "/v1/messages", ProviderPath: "/v1/chat/completions", CreatedAt: now})

	// Round-trips through ByUser/All and the parameterized Query, and is filterable.
	if got := st.All(); len(got) != 1 || got[0].ProviderPath != "/v1/chat/completions" {
		t.Fatalf("All ProviderPath = %+v, want /v1/chat/completions", got)
	}
	page, err := st.Query(usage.Query{ScopeAll: true, ProviderPath: "chat/completions"})
	if err != nil {
		t.Fatalf("Query returned err: %v", err)
	}
	if page.Total != 1 || len(page.Data) != 1 || page.Data[0].ProviderPath != "/v1/chat/completions" {
		t.Fatalf("Query with provider_path filter = %+v, want the one row", page)
	}
	if page, err := st.Query(usage.Query{ScopeAll: true, ProviderPath: "responses"}); err != nil || page.Total != 0 {
		t.Fatalf("Query with non-matching provider_path filter returned %d rows, want 0", page.Total)
	}
}

func TestSQLiteStoreImplementsUsageStore(t *testing.T) {
	var _ usage.Store = (*SQLiteStore)(nil)
}

func TestSQLiteUsageStorePersistsEnrichedMetrics(t *testing.T) {
	st := openMigratedTestSQLite(t)
	defer st.Close()
	event := testUsageEvent("req_enriched", "usr_1", "tok_1", "success")
	event.CachedTokens = 4
	event.CacheWriteTokens = 7
	event.PromptPerSecond = 12.5
	event.TokensPerSecond = 34.5
	event.HTTPStatus = 200
	event.ContentType = "text/event-stream"
	event.ReqPath = "/v1/chat/completions"
	event.ProviderModel = "qwen"
	event.Stream = true
	event.TokenName = "T"
	event.ServerName = "S"

	st.Record(event)

	byUser := st.ByUser("usr_1")
	if len(byUser) != 1 {
		t.Fatalf("ByUser events = %d, want 1", len(byUser))
	}
	if byUser[0] != event {
		t.Fatalf("ByUser event = %#v, want %#v", byUser[0], event)
	}

	all := st.All()
	if len(all) != 1 {
		t.Fatalf("All events = %d, want 1", len(all))
	}
	if all[0] != event {
		t.Fatalf("All event = %#v, want %#v", all[0], event)
	}

	got := all[0]
	if got.CachedTokens != 4 {
		t.Fatalf("CachedTokens = %d, want 4", got.CachedTokens)
	}
	if got.CacheWriteTokens != 7 {
		t.Fatalf("CacheWriteTokens = %d, want 7", got.CacheWriteTokens)
	}
	if got.PromptPerSecond != 12.5 {
		t.Fatalf("PromptPerSecond = %v, want 12.5", got.PromptPerSecond)
	}
	if got.TokensPerSecond != 34.5 {
		t.Fatalf("TokensPerSecond = %v, want 34.5", got.TokensPerSecond)
	}
	if got.HTTPStatus != 200 {
		t.Fatalf("HTTPStatus = %d, want 200", got.HTTPStatus)
	}
	if got.ContentType != "text/event-stream" {
		t.Fatalf("ContentType = %q, want text/event-stream", got.ContentType)
	}
	if got.ReqPath != "/v1/chat/completions" {
		t.Fatalf("ReqPath = %q, want /v1/chat/completions", got.ReqPath)
	}
	if got.ProviderModel != "qwen" {
		t.Fatalf("ProviderModel = %q, want qwen", got.ProviderModel)
	}
	if !got.Stream {
		t.Fatalf("Stream = false, want true")
	}
	if got.TokenName != "T" {
		t.Fatalf("TokenName = %q, want T", got.TokenName)
	}
	if got.ServerName != "S" {
		t.Fatalf("ServerName = %q, want S", got.ServerName)
	}
}

func testUsageEvent(id string, userID string, tokenID string, status string) usage.Event {
	return usage.Event{
		ID:           id,
		UserID:       userID,
		TokenID:      tokenID,
		SessionID:    "sess_" + id,
		APIFlavor:    "openai",
		Model:        "qwen-coder",
		Provider:     "mock",
		Host:         "mock-host",
		InputTokens:  2,
		OutputTokens: 3,
		TotalTokens:  5,
		LatencyMS:    42,
		Status:       status,
		ErrorCode:    "",
		CreatedAt:    time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC),
	}
}

func TestSQLiteUsageQueryFiltersSortsAndPaginates(t *testing.T) {
	st := openMigratedTestSQLite(t)
	defer st.Close()

	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	mk := func(id string, latency int64, created time.Time) usage.Event {
		e := testUsageEvent(id, "usr_1", "tok_1", "success")
		e.LatencyMS = latency
		e.HTTPStatus = 200
		e.ServerName = "GPU-1"
		e.CreatedAt = created
		return e
	}
	a := mk("req_a", 10, base)
	b := mk("req_b", 30, base.Add(time.Hour))
	other := testUsageEvent("req_c", "usr_2", "tok_2", "success") // filtered out in own scope
	other.HTTPStatus = 200
	streamErr := mk("req_err", 5, base.Add(2*time.Hour))
	streamErr.Status = "error"
	streamErr.HTTPStatus = 200 // mid-stream error
	st.Record(a)
	st.Record(b)
	st.Record(other)
	st.Record(streamErr)

	// Own scope, default order (created_at desc): req_err, req_b, req_a.
	page, err := st.Query(usage.Query{UserID: "usr_1"})
	if err != nil {
		t.Fatalf("Query returned err: %v", err)
	}
	if page.Total != 3 || len(page.Data) != 3 {
		t.Fatalf("total/len = %d/%d, want 3/3", page.Total, len(page.Data))
	}
	if page.Data[0].ID != "req_err" || page.Data[2].ID != "req_a" {
		t.Fatalf("default order = %s..%s, want req_err..req_a", page.Data[0].ID, page.Data[2].ID)
	}
	if page.Data[0].UserName != "" {
		t.Fatalf("own scope must leave UserName empty, got %q", page.Data[0].UserName)
	}
	if err := st.LastUsageError(); err != nil {
		t.Fatalf("LastUsageError = %v", err)
	}

	// Sort by latency ascending.
	asc, err := st.Query(usage.Query{UserID: "usr_1", Sort: "latency_ms", Order: "asc"})
	if err != nil {
		t.Fatalf("Query returned err: %v", err)
	}
	if asc.Data[0].ID != "req_err" || asc.Data[1].ID != "req_a" || asc.Data[2].ID != "req_b" {
		t.Fatalf("latency asc = %s,%s,%s", asc.Data[0].ID, asc.Data[1].ID, asc.Data[2].ID)
	}

	// status=error uses the combined predicate (streaming error counts).
	errOnly, err := st.Query(usage.Query{UserID: "usr_1", Status: "error"})
	if err != nil {
		t.Fatalf("Query returned err: %v", err)
	}
	if errOnly.Total != 1 || errOnly.Data[0].ID != "req_err" {
		t.Fatalf("status=error = %#v, want single req_err", errOnly.Data)
	}
	succ, err := st.Query(usage.Query{UserID: "usr_1", Status: "success"})
	if err != nil {
		t.Fatalf("Query returned err: %v", err)
	}
	if succ.Total != 2 {
		t.Fatalf("status=success total = %d, want 2", succ.Total)
	}

	// Server substring match over server_name OR host (not exact-equal).
	if got, err := st.Query(usage.Query{UserID: "usr_1", Server: "GPU-1"}); err != nil || got.Total != 3 {
		t.Fatalf("server=GPU-1 total = %d, want 3", got.Total)
	}
	// server_name is "" on req_c/usr_2 only; add an own-scope row with empty server_name + host.
	fallback := mk("req_fb", 1, base.Add(3*time.Hour))
	fallback.ServerName = ""
	fallback.Host = "host-x"
	st.Record(fallback)
	if got, err := st.Query(usage.Query{UserID: "usr_1", Server: "host-x"}); err != nil || got.Total != 1 || got.Data[0].ID != "req_fb" {
		t.Fatalf("server host fallback = %#v, want single req_fb", got.Data)
	}

	// Free-text q over id/model/host/server_name/token_name.
	if got, err := st.Query(usage.Query{UserID: "usr_1", Q: "req_err"}); err != nil || got.Total != 1 || got.Data[0].ID != "req_err" {
		t.Fatalf("q=req_err = %#v", got.Data)
	}

	// Model substring miss (no event's Model contains this).
	if got, err := st.Query(usage.Query{UserID: "usr_1", Model: "no-such"}); err != nil || got.Total != 0 {
		t.Fatalf("model miss total = %d, want 0", got.Total)
	}

	// Time window on created_at (inclusive).
	win, err := st.Query(usage.Query{UserID: "usr_1", From: base.Add(time.Hour), To: base.Add(2 * time.Hour)})
	if err != nil {
		t.Fatalf("Query returned err: %v", err)
	}
	if win.Total != 2 {
		t.Fatalf("window total = %d, want 2 (req_b, req_err)", win.Total)
	}
}

func TestSQLiteUsageQueryOwnerNumericAndTimeFilters(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()

	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	if err := st.CreateUser(ctx, User{ID: "usr_ada", Email: "ada@example.test", DisplayName: "Ada Admin", Role: "user", Status: UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateUser usr_ada: %v", err)
	}
	if err := st.CreateUser(ctx, User{ID: "usr_bob", Email: "bob@example.test", DisplayName: "Bob Builder", Role: "user", Status: UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateUser usr_bob: %v", err)
	}

	mk := func(id, userID string, total int, latency int64, pps float64, created time.Time) usage.Event {
		e := testUsageEvent(id, userID, "tok_"+userID, "success")
		e.HTTPStatus = 200
		e.TotalTokens = total
		e.LatencyMS = latency
		e.PromptPerSecond = pps
		e.CreatedAt = created
		return e
	}
	st.Record(mk("req_ada1", "usr_ada", 5, 50, 1, now))
	st.Record(mk("req_ada2", "usr_ada", 50, 150, 2.5, now.Add(time.Hour)))
	st.Record(mk("req_bob1", "usr_bob", 500, 400, 9, now.Add(2*time.Hour)))

	// Owner over user_id substring (own scope, no join).
	if got, err := st.Query(usage.Query{UserID: "usr_ada", Owner: "usr_ada"}); err != nil || got.Total != 2 {
		t.Fatalf("own-scope Owner=usr_ada total = %d, want 2", got.Total)
	}
	// Owner over the resolved display_name (ScopeAll, users join present).
	nameHit, err := st.Query(usage.Query{ScopeAll: true, Owner: "bob build"})
	if err != nil {
		t.Fatalf("Query returned err: %v", err)
	}
	if nameHit.Total != 1 || nameHit.Data[0].ID != "req_bob1" {
		t.Fatalf("ScopeAll Owner=\"bob build\" = %#v, want single req_bob1 (name match)", nameHit.Data)
	}
	// Owner over user_id also works in ScopeAll.
	if got, err := st.Query(usage.Query{ScopeAll: true, Owner: "usr_ada"}); err != nil || got.Total != 2 {
		t.Fatalf("ScopeAll Owner=usr_ada total = %d, want 2", got.Total)
	}

	// Numeric min-only.
	if got, err := st.Query(usage.Query{ScopeAll: true, NumericMin: map[string]float64{"total_tokens": 50}}); err != nil || got.Total != 2 {
		t.Fatalf("total_tokens>=50 total = %d, want 2 (req_ada2, req_bob1)", got.Total)
	}
	// Numeric max-only.
	if got, err := st.Query(usage.Query{ScopeAll: true, NumericMax: map[string]float64{"latency_ms": 150}}); err != nil || got.Total != 2 {
		t.Fatalf("latency_ms<=150 total = %d, want 2 (req_ada1, req_ada2)", got.Total)
	}
	// Numeric range on a float column.
	rng, err := st.Query(usage.Query{ScopeAll: true, NumericMin: map[string]float64{"prompt_per_second": 2}, NumericMax: map[string]float64{"prompt_per_second": 5}})
	if err != nil {
		t.Fatalf("Query returned err: %v", err)
	}
	if rng.Total != 1 || rng.Data[0].ID != "req_ada2" {
		t.Fatalf("2<=pps<=5 = %#v, want single req_ada2", rng.Data)
	}
	// Stats honors the numeric filter too (shared usageWhere).
	if got, err := st.Stats(usage.Query{ScopeAll: true, NumericMin: map[string]float64{"total_tokens": 50}}); err != nil || got.Totals.TotalRequests != 2 {
		t.Fatalf("Stats total_tokens>=50 requests = %d, want 2", got.Totals.TotalRequests)
	}

	// TimeFrom/TimeTo bounds (inclusive), ANDed with any existing From/To.
	win, err := st.Query(usage.Query{ScopeAll: true, TimeFrom: now.Add(time.Hour), TimeTo: now.Add(time.Hour)})
	if err != nil {
		t.Fatalf("Query returned err: %v", err)
	}
	if win.Total != 1 || win.Data[0].ID != "req_ada2" {
		t.Fatalf("TimeFrom=TimeTo=now+1h = %#v, want single req_ada2", win.Data)
	}
	if err := st.LastUsageError(); err != nil {
		t.Fatalf("LastUsageError = %v", err)
	}
}

func TestSQLiteUsageQueryReqPathStreamAndCachedTokensFilters(t *testing.T) {
	st := openMigratedTestSQLite(t)
	defer st.Close()

	chat := testUsageEvent("req_chat", "usr_1", "tok_1", "success")
	chat.HTTPStatus = 200
	chat.ReqPath = "/v1/chat/completions"
	chat.ContentType = "text/event-stream"
	chat.ProviderModel = "qwen-72b"
	chat.Stream = true
	chat.CachedTokens = 50
	embed := testUsageEvent("req_embed", "usr_1", "tok_1", "success")
	embed.HTTPStatus = 200
	embed.ReqPath = "/v1/embeddings"
	embed.ContentType = "application/json"
	embed.ProviderModel = "bge-large"
	embed.Stream = false
	embed.CachedTokens = 5
	st.Record(chat)
	st.Record(embed)

	// req_path substring, case-insensitive (SQLite LIKE is ASCII case-insensitive).
	if got, err := st.Query(usage.Query{UserID: "usr_1", ReqPath: "CHAT"}); err != nil || got.Total != 1 || got.Data[0].ID != "req_chat" {
		t.Fatalf("req_path=CHAT = %#v, want single req_chat", got.Data)
	}
	// content_type substring (with slash).
	if got, err := st.Query(usage.Query{UserID: "usr_1", ContentType: "event-stream"}); err != nil || got.Total != 1 || got.Data[0].ID != "req_chat" {
		t.Fatalf("content_type=event-stream = %#v, want single req_chat", got.Data)
	}
	// provider_model substring, case-insensitive.
	if got, err := st.Query(usage.Query{UserID: "usr_1", ProviderModel: "BGE"}); err != nil || got.Total != 1 || got.Data[0].ID != "req_embed" {
		t.Fatalf("provider_model=BGE = %#v, want single req_embed", got.Data)
	}
	// stream tri-state: true keeps only the streamed row.
	if got, err := st.Query(usage.Query{UserID: "usr_1", Stream: "true"}); err != nil || got.Total != 1 || got.Data[0].ID != "req_chat" {
		t.Fatalf("stream=true = %#v, want single req_chat", got.Data)
	}
	// stream=false keeps only the non-streamed row.
	if got, err := st.Query(usage.Query{UserID: "usr_1", Stream: "false"}); err != nil || got.Total != 1 || got.Data[0].ID != "req_embed" {
		t.Fatalf("stream=false = %#v, want single req_embed", got.Data)
	}
	// empty stream -> no filter.
	if got, err := st.Query(usage.Query{UserID: "usr_1", Stream: ""}); err != nil || got.Total != 2 {
		t.Fatalf("stream=\"\" total = %d, want 2 (no filter)", got.Total)
	}
	// cached_tokens whitelisted numeric range.
	if got, err := st.Query(usage.Query{UserID: "usr_1", NumericMin: map[string]float64{"cached_tokens": 10}}); err != nil || got.Total != 1 || got.Data[0].ID != "req_chat" {
		t.Fatalf("cached_tokens>=10 = %#v, want single req_chat", got.Data)
	}
	if err := st.LastUsageError(); err != nil {
		t.Fatalf("LastUsageError = %v", err)
	}
}

// TestCrossStoreUsageFilterParity proves the SQLiteStore and the in-memory
// usage.Recorder agree on the new owner/numeric/time filters: the same
// Query selects the same set of ids from both. The recorder's
// ResolveUserName mirrors SQLite's users-join coalesce so ScopeAll name
// matching lines up.
func TestCrossStoreUsageFilterParity(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)

	st := openMigratedTestSQLite(t)
	defer st.Close()
	if err := st.CreateUser(ctx, User{ID: "usr_ada", Email: "ada@example.test", DisplayName: "Ada Admin", Role: "user", Status: UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateUser usr_ada: %v", err)
	}
	if err := st.CreateUser(ctx, User{ID: "usr_bob", Email: "bob@example.test", DisplayName: "Bob Builder", Role: "user", Status: UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateUser usr_bob: %v", err)
	}

	rec := usage.NewRecorder()
	rec.ResolveUserName = func(userID string) string {
		return map[string]string{"usr_ada": "Ada Admin", "usr_bob": "Bob Builder"}[userID]
	}

	mk := func(id, userID string, total int, latency int64, pps float64, created time.Time) usage.Event {
		e := testUsageEvent(id, userID, "tok_"+userID, "success")
		e.HTTPStatus = 200
		e.TotalTokens = total
		e.LatencyMS = latency
		e.PromptPerSecond = pps
		e.CreatedAt = created
		return e
	}
	ada1 := mk("req_ada1", "usr_ada", 5, 50, 1, now)
	ada1.ReqPath, ada1.ContentType, ada1.ProviderModel, ada1.Stream, ada1.CachedTokens = "/v1/chat", "text/event-stream", "qwen", true, 5
	ada2 := mk("req_ada2", "usr_ada", 50, 150, 2.5, now.Add(time.Hour))
	ada2.ReqPath, ada2.ContentType, ada2.ProviderModel, ada2.Stream, ada2.CachedTokens = "/v1/embeddings", "application/json", "bge", false, 50
	bob1 := mk("req_bob1", "usr_bob", 500, 400, 9, now.Add(2*time.Hour))
	bob1.ReqPath, bob1.ContentType, bob1.ProviderModel, bob1.Stream, bob1.CachedTokens = "/v1/chat", "application/json", "qwen", true, 500
	chat := mk("req_chat", "usr_ada", 7, 70, 1.5, now.Add(3*time.Hour))
	chat.TokenID = "" // chat / no-token usage
	events := []usage.Event{ada1, ada2, bob1, chat}
	for _, e := range events {
		st.Record(e)
		rec.Record(e)
	}

	ids := func(page usage.Page, err error) map[string]bool {
		if err != nil {
			t.Fatalf("Query returned err: %v", err)
		}
		out := map[string]bool{}
		for _, r := range page.Data {
			out[r.ID] = true
		}
		return out
	}
	cases := map[string]usage.Query{
		"owner_id_own_scope":  {UserID: "usr_ada", Owner: "usr_ada", Limit: 100},
		"owner_name_all":      {ScopeAll: true, Owner: "bob build", Limit: 100},
		"numeric_min":         {ScopeAll: true, NumericMin: map[string]float64{"total_tokens": 50}, Limit: 100},
		"numeric_max":         {ScopeAll: true, NumericMax: map[string]float64{"latency_ms": 150}, Limit: 100},
		"numeric_range_float": {ScopeAll: true, NumericMin: map[string]float64{"prompt_per_second": 2}, NumericMax: map[string]float64{"prompt_per_second": 5}, Limit: 100},
		"time_window":         {ScopeAll: true, TimeFrom: now.Add(time.Hour), TimeTo: now.Add(time.Hour), Limit: 100},
		"req_path_substr":     {ScopeAll: true, ReqPath: "embeddings", Limit: 100},
		"content_type_substr": {ScopeAll: true, ContentType: "event-stream", Limit: 100},
		"provider_model_sub":  {ScopeAll: true, ProviderModel: "qwen", Limit: 100},
		"stream_true":         {ScopeAll: true, Stream: "true", Limit: 100},
		"stream_false":        {ScopeAll: true, Stream: "false", Limit: 100},
		"cached_tokens_range": {ScopeAll: true, NumericMin: map[string]float64{"cached_tokens": 10}, NumericMax: map[string]float64{"cached_tokens": 100}, Limit: 100},
		"token_specific":      {ScopeAll: true, HasTokenFilter: true, TokenID: "tok_usr_ada", Limit: 100},
		"token_empty_chat":    {ScopeAll: true, HasTokenFilter: true, TokenID: "", Limit: 100},
	}
	for name, q := range cases {
		sqlIDs := ids(st.Query(q))
		memIDs := ids(rec.Query(q))
		if len(sqlIDs) != len(memIDs) {
			t.Fatalf("%s: id-set sizes differ sqlite=%v memory=%v", name, sqlIDs, memIDs)
		}
		for id := range sqlIDs {
			if !memIDs[id] {
				t.Fatalf("%s: sqlite has %s, memory does not (sqlite=%v memory=%v)", name, id, sqlIDs, memIDs)
			}
		}
	}
}

func TestSQLiteUsageQueryStablePaginationOverTies(t *testing.T) {
	st := openMigratedTestSQLite(t)
	defer st.Close()
	tie := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 30; i++ {
		id := "req_" + string(rune('a'+i/26)) + string(rune('a'+i%26))
		e := testUsageEvent(id, "usr_1", "tok_1", "success")
		e.CreatedAt = tie
		st.Record(e)
	}
	p1, err := st.Query(usage.Query{UserID: "usr_1", Sort: "created_at", Order: "asc", Page: 1, Limit: 25})
	if err != nil {
		t.Fatalf("Query returned err: %v", err)
	}
	p2, err := st.Query(usage.Query{UserID: "usr_1", Sort: "created_at", Order: "asc", Page: 2, Limit: 25})
	if err != nil {
		t.Fatalf("Query returned err: %v", err)
	}
	if p1.Total != 30 || p1.TotalPages != 2 {
		t.Fatalf("total/pages = %d/%d, want 30/2", p1.Total, p1.TotalPages)
	}
	if len(p1.Data) != 25 || len(p2.Data) != 5 {
		t.Fatalf("page sizes = %d/%d, want 25/5", len(p1.Data), len(p2.Data))
	}
	seen := map[string]bool{}
	for _, r := range append(append([]usage.Row{}, p1.Data...), p2.Data...) {
		if seen[r.ID] {
			t.Fatalf("id %s on two pages (unstable OFFSET over ties)", r.ID)
		}
		seen[r.ID] = true
	}
	if len(seen) != 30 {
		t.Fatalf("union = %d ids, want 30", len(seen))
	}
}

func TestSQLiteUsageQueryScopeAllResolvesUserName(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()

	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	if err := st.CreateUser(ctx, User{ID: "usr_1", Email: "ada@example.test", DisplayName: "Ada Admin", Role: "user", Status: UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateUser usr_1: %v", err)
	}
	// usr_2 has an empty display_name -> COALESCE must fall back to email.
	if err := st.CreateUser(ctx, User{ID: "usr_2", Email: "bob@example.test", DisplayName: "", Role: "user", Status: UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateUser usr_2: %v", err)
	}
	st.Record(testUsageEvent("req_1", "usr_1", "tok_1", "success"))
	st.Record(testUsageEvent("req_2", "usr_2", "tok_2", "success"))
	// usr_ghost has no users row -> COALESCE falls back to the raw user_id.
	st.Record(testUsageEvent("req_3", "usr_ghost", "tok_3", "success"))

	page, err := st.Query(usage.Query{ScopeAll: true})
	if err != nil {
		t.Fatalf("Query returned err: %v", err)
	}
	if page.Total != 3 {
		t.Fatalf("ScopeAll total = %d, want 3 (cross-user)", page.Total)
	}
	names := map[string]string{}
	for _, r := range page.Data {
		names[r.UserID] = r.UserName
	}
	if names["usr_1"] != "Ada Admin" {
		t.Fatalf("usr_1 name = %q, want Ada Admin", names["usr_1"])
	}
	if names["usr_2"] != "bob@example.test" {
		t.Fatalf("usr_2 name = %q, want email fallback", names["usr_2"])
	}
	if names["usr_ghost"] != "usr_ghost" {
		t.Fatalf("deleted-user name = %q, want raw user_id fallback", names["usr_ghost"])
	}
}

func TestSQLiteUsageStats(t *testing.T) {
	st := openMigratedTestSQLite(t)
	defer st.Close()

	ok1 := testUsageEvent("req_1", "usr_1", "tok_1", "success")
	ok1.InputTokens, ok1.OutputTokens, ok1.CachedTokens = 5, 7, 2
	ok1.CacheWriteTokens = 3
	ok1.HTTPStatus, ok1.PromptPerSecond, ok1.TokensPerSecond = 200, 10, 20
	ok1.EnergyWh = 1.5
	ok2 := testUsageEvent("req_2", "usr_1", "tok_1", "success")
	ok2.InputTokens, ok2.OutputTokens, ok2.CachedTokens = 3, 1, 0
	ok2.HTTPStatus, ok2.PromptPerSecond, ok2.TokensPerSecond = 200, 30, 40
	ok2.EnergyWh = 2.5
	streamErr := testUsageEvent("req_3", "usr_1", "tok_1", "error")
	streamErr.InputTokens, streamErr.OutputTokens, streamErr.CachedTokens = 1, 0, 0
	streamErr.HTTPStatus, streamErr.PromptPerSecond, streamErr.TokensPerSecond = 200, 0, 0 // mid-stream error, 0 speeds
	st.Record(ok1)
	st.Record(ok2)
	st.Record(streamErr)

	stats, err := st.Stats(usage.Query{UserID: "usr_1"})
	if err != nil {
		t.Fatalf("Stats returned err: %v", err)
	}
	if err := st.LastUsageError(); err != nil {
		t.Fatalf("LastUsageError = %v", err)
	}
	if stats.Totals.TotalRequests != 3 || stats.Totals.ErrorCount != 1 {
		t.Fatalf("totals requests/errors = %d/%d, want 3/1", stats.Totals.TotalRequests, stats.Totals.ErrorCount)
	}
	if stats.Totals.CachedTokens != 2 || stats.Totals.CacheWriteTokens != 3 || stats.Totals.InputTokens != 9 || stats.Totals.OutputTokens != 8 {
		t.Fatalf("totals = %#v", stats.Totals)
	}
	if stats.Totals.TotalEnergyWh != 4.0 {
		t.Fatalf("TotalEnergyWh = %v, want 4.0 (1.5+2.5+0)", stats.Totals.TotalEnergyWh)
	}
	promptCount := 0
	for _, b := range stats.PromptPerSecond.Bins {
		promptCount += b.Count
	}
	if promptCount != 2 {
		t.Fatalf("prompt histogram count = %d, want 2 (zeros excluded)", promptCount)
	}
	if stats.PromptPerSecond.Min != 10 || stats.PromptPerSecond.Max != 30 {
		t.Fatalf("prompt min/max = %v/%v, want 10/30", stats.PromptPerSecond.Min, stats.PromptPerSecond.Max)
	}
	if stats.TokensPerSecond.Min != 20 || stats.TokensPerSecond.Max != 40 {
		t.Fatalf("tokens min/max = %v/%v, want 20/40", stats.TokensPerSecond.Min, stats.TokensPerSecond.Max)
	}

	// Empty window -> zeroed stats, empty histograms, no error.
	empty, err := st.Stats(usage.Query{UserID: "no-such-user"})
	if err != nil {
		t.Fatalf("Stats returned err: %v", err)
	}
	if empty.Totals.TotalRequests != 0 || len(empty.PromptPerSecond.Bins) != 0 {
		t.Fatalf("empty stats = %#v", empty)
	}
}

// TestSQLiteEnergyByServer proves EnergyByServer sums energy_wh grouped by
// host over the same WHERE Stats applies, and a host with no matching rows
// is absent from the returned map (never a zero entry).
func TestSQLiteEnergyByServer(t *testing.T) {
	st := openMigratedTestSQLite(t)
	defer st.Close()

	a1 := testUsageEvent("req_1", "usr_1", "tok_1", "success")
	a1.Host = "srv_a"
	a1.EnergyWh = 1.5
	a2 := testUsageEvent("req_2", "usr_1", "tok_1", "success")
	a2.Host = "srv_a"
	a2.EnergyWh = 0.5
	b1 := testUsageEvent("req_3", "usr_1", "tok_1", "success")
	b1.Host = "srv_b"
	b1.EnergyWh = 3.0
	other := testUsageEvent("req_4", "usr_2", "tok_2", "success")
	other.Host = "srv_c"
	other.EnergyWh = 42 // different user, excluded from own-scope query
	for _, e := range []usage.Event{a1, a2, b1, other} {
		st.Record(e)
	}
	if err := st.LastUsageError(); err != nil {
		t.Fatalf("Record: %v", err)
	}

	got, err := st.EnergyByServer(context.Background(), usage.Query{UserID: "usr_1"})
	if err != nil {
		t.Fatalf("EnergyByServer: %v", err)
	}
	if got["srv_a"] != 2.0 {
		t.Fatalf("srv_a = %v, want 2.0 (1.5+0.5)", got["srv_a"])
	}
	if got["srv_b"] != 3.0 {
		t.Fatalf("srv_b = %v, want 3.0", got["srv_b"])
	}
	if _, ok := got["srv_c"]; ok {
		t.Fatalf("srv_c present in own-scope map, want absent (belongs to usr_2)")
	}
}

// A Stats result over an empty or zero-only set must marshal its histogram bins
// to "bins":[] (non-nil), NEVER "bins":null. A null would crash the Activity view
// (SpeedHistogram reads bins.length) — this is the common fresh-gateway state.
func TestSQLiteUsageStatsEmptyMarshalsBinsArray(t *testing.T) {
	st := openMigratedTestSQLite(t)
	defer st.Close()

	// A zero-speed row: histograms drop zeros, so N==0 even though a row matched.
	zero := testUsageEvent("req_zero", "usr_1", "tok_1", "success")
	zero.HTTPStatus, zero.PromptPerSecond, zero.TokensPerSecond = 200, 0, 0
	st.Record(zero)

	// (a) matched-but-zero-only set and (b) genuinely empty set both apply.
	for _, q := range []usage.Query{{UserID: "usr_1"}, {UserID: "no-such-user"}} {
		stats, err := st.Stats(q)
		if err != nil {
			t.Fatalf("Stats returned err: %v", err)
		}
		if stats.PromptPerSecond.Bins == nil || stats.TokensPerSecond.Bins == nil {
			t.Fatalf("nil Bins for query %#v: %#v", q, stats)
		}
		blob, err := json.Marshal(stats)
		if err != nil {
			t.Fatalf("marshal stats: %v", err)
		}
		if strings.Contains(string(blob), `"bins":null`) {
			t.Fatalf("stats marshaled bins:null (crashes Activity view) for %#v: %s", q, blob)
		}
		if !strings.Contains(string(blob), `"bins":[]`) {
			t.Fatalf("want bins:[] for %#v, got: %s", q, blob)
		}
	}
}

// A page far past the end must return an empty Data page (matching the memory
// Recorder), NOT wrap the OFFSET negative/overflow and fall back to the first
// page. The reported Page/Total/TotalPages are unchanged.
func TestSQLiteUsageQueryHugePageReturnsEmpty(t *testing.T) {
	st := openMigratedTestSQLite(t)
	defer st.Close()
	st.Record(testUsageEvent("req_a", "usr_1", "tok_1", "success"))
	st.Record(testUsageEvent("req_b", "usr_1", "tok_1", "success"))

	got, err := st.Query(usage.Query{UserID: "usr_1", Page: 400000000000000000, Limit: 25})
	if err != nil {
		t.Fatalf("Query returned err: %v", err)
	}
	if got.Total != 2 || len(got.Data) != 0 {
		t.Fatalf("huge page total/len = %d/%d, want 2/0 (empty page past the last)", got.Total, len(got.Data))
	}
	if got.Page != 400000000000000000 {
		t.Fatalf("huge page Page = %d, want unchanged 400000000000000000", got.Page)
	}
	if got.TotalPages != 1 {
		t.Fatalf("huge page TotalPages = %d, want 1", got.TotalPages)
	}
	if err := st.LastUsageError(); err != nil {
		t.Fatalf("LastUsageError = %v", err)
	}
}

// TestSQLiteUsageQueryScanAfterHasCaptureColumnRemoval is a scan regression
// test: the has_capture `exists(...)` column was removed from the Query
// SELECT (relocated to Service.Usage via portal.CaptureReader.HasCaptures),
// so the SELECT list and the scanUsageRows Scan destination list must still
// line up 1:1 without it, and Query must keep returning a populated,
// correctly-scanned page.
func TestSQLiteUsageQueryScanAfterHasCaptureColumnRemoval(t *testing.T) {
	st := openMigratedTestSQLite(t)
	defer st.Close()

	st.Record(testUsageEvent("req_a", "usr_1", "tok_1", "success"))
	st.Record(testUsageEvent("req_b", "usr_1", "tok_1", "error"))

	page, err := st.Query(usage.Query{UserID: "usr_1"})
	if err != nil {
		t.Fatalf("Query returned err: %v", err)
	}
	if err := st.LastUsageError(); err != nil {
		t.Fatalf("LastUsageError = %v", err)
	}
	if page.Total != 2 || len(page.Data) != 2 {
		t.Fatalf("page = %#v, want 2 scanned rows", page)
	}
	byID := map[string]usage.Row{}
	for _, row := range page.Data {
		byID[row.ID] = row
	}
	if byID["req_a"].Status != "success" || byID["req_b"].Status != "error" {
		t.Fatalf("scanned rows = %#v, want status fields intact after the column removal", byID)
	}
}

func TestSQLiteUsageQueryModelServerFilterSubstringCaseInsensitive(t *testing.T) {
	st := openMigratedTestSQLite(t)
	defer st.Close()

	gpu := testUsageEvent("req_gpu", "usr_1", "tok_1", "success")
	gpu.Model = "qwen-coder"
	gpu.ServerName = "GPU-1"
	gpu.Host = "mock-host"
	edge := testUsageEvent("req_edge", "usr_1", "tok_1", "success")
	edge.Model = "claude-sonnet"
	edge.ServerName = "GPU-2"
	edge.Host = "edge-node-7"
	bare := testUsageEvent("req_bare", "usr_1", "tok_1", "success")
	bare.Model = "llama-3"
	bare.ServerName = ""
	bare.Host = "bare-host"
	st.Record(gpu)
	st.Record(edge)
	st.Record(bare)

	// Model: case-insensitive substring, not just an exact match.
	if got, err := st.Query(usage.Query{UserID: "usr_1", Model: "QWEN"}); err != nil || got.Total != 1 || got.Data[0].ID != "req_gpu" {
		t.Fatalf("model=QWEN = %#v, want single req_gpu", got.Data)
	}
	if got, err := st.Query(usage.Query{UserID: "usr_1", Model: "coder"}); err != nil || got.Total != 1 || got.Data[0].ID != "req_gpu" {
		t.Fatalf("model=coder (suffix substring) = %#v, want single req_gpu", got.Data)
	}

	// Server: substring over server_name matches every row whose name contains it.
	if got, err := st.Query(usage.Query{UserID: "usr_1", Server: "gpu"}); err != nil || got.Total != 2 {
		t.Fatalf("server=gpu total = %d, want 2 (req_gpu, req_edge)", got.Total)
	}

	// Server: host is checked even when server_name is set and does not match --
	// the bug this task fixes (previously host was only consulted when
	// server_name was empty).
	if got, err := st.Query(usage.Query{UserID: "usr_1", Server: "edge"}); err != nil || got.Total != 1 || got.Data[0].ID != "req_edge" {
		t.Fatalf("server=edge = %#v, want single req_edge (matched via host despite non-empty server_name)", got.Data)
	}

	// Server: host fallback stays case-insensitive when server_name is empty.
	if got, err := st.Query(usage.Query{UserID: "usr_1", Server: "BARE-HOST"}); err != nil || got.Total != 1 || got.Data[0].ID != "req_bare" {
		t.Fatalf("server=BARE-HOST = %#v, want single req_bare", got.Data)
	}
	if err := st.LastUsageError(); err != nil {
		t.Fatalf("LastUsageError = %v", err)
	}
}

// TestSQLiteUsageStorePersistsEnergyFields proves the P1 additive energy_wh/
// energy_marginal_wh/energy_source columns round-trip through Record, ByUser,
// All, and Query — before any computation engine exists, recordUsage leaves
// these at their zero values, but the storage layer must carry a non-zero
// value end to end once a later phase starts populating them.
func TestSQLiteUsageStorePersistsEnergyFields(t *testing.T) {
	st := openMigratedTestSQLite(t)
	defer st.Close()
	event := testUsageEvent("req_energy", "usr_1", "tok_1", "success")
	event.EnergyWh = 12.5
	event.EnergyMarginalWh = 3.25
	event.EnergySource = "measured"

	st.Record(event)

	byUser := st.ByUser("usr_1")
	if len(byUser) != 1 || byUser[0] != event {
		t.Fatalf("ByUser event = %#v, want %#v", byUser, event)
	}

	all := st.All()
	if len(all) != 1 || all[0] != event {
		t.Fatalf("All event = %#v, want %#v", all, event)
	}

	page, err := st.Query(usage.Query{UserID: "usr_1"})
	if err != nil {
		t.Fatalf("Query returned err: %v", err)
	}
	if page.Total != 1 ||
		page.Data[0].EnergyWh != 12.5 ||
		page.Data[0].EnergyMarginalWh != 3.25 ||
		page.Data[0].EnergySource != "measured" {
		t.Fatalf("Query row = %#v, want energy fields round-tripped", page.Data)
	}
}

// TestSQLiteUsageQueryExactFilters proves the SQL usageWhere exact
// (case-insensitive equality) filters ServerExact / SessionIDExact / ModelExact
// select only the exact-value row and NOT a longer sibling that has it as a
// prefix (e.g. "prod" must not also match "prod-eu") — proving the SQL clause,
// not just the in-memory matchUsage path.
func TestSQLiteUsageQueryExactFilters(t *testing.T) {
	st := openMigratedTestSQLite(t)
	defer st.Close()

	a := testUsageEvent("req_a", "usr_1", "tok_1", "success")
	a.ServerName = "prod"
	a.SessionID = "sess_a"
	a.Model = "gpt"
	b := testUsageEvent("req_b", "usr_1", "tok_1", "success")
	b.ServerName = "prod-eu"
	b.SessionID = "sess_ab"
	b.Model = "gpt-4" // hyphen
	// c has an underscore model of the SAME length as b's hyphen model: with a LIKE/
	// ILIKE clause `_` is a single-char wildcard, so ModelExact:"gpt_4" would also
	// match "gpt-4" (over-match). lower()=lower() equality must NOT.
	c := testUsageEvent("req_c", "usr_1", "tok_1", "success")
	c.ServerName = "prod-us"
	c.SessionID = "sess_c"
	c.Model = "gpt_4" // underscore
	st.Record(a)
	st.Record(b)
	st.Record(c)

	if got, err := st.Query(usage.Query{UserID: "usr_1", ServerExact: "prod"}); err != nil || got.Total != 1 || got.Data[0].ID != "req_a" {
		t.Fatalf("ServerExact=prod = %#v, want single req_a (not prod-eu)", got.Data)
	}
	if got, err := st.Query(usage.Query{UserID: "usr_1", SessionIDExact: "sess_a"}); err != nil || got.Total != 1 || got.Data[0].ID != "req_a" {
		t.Fatalf("SessionIDExact=sess_a = %#v, want single req_a (not sess_ab)", got.Data)
	}
	if got, err := st.Query(usage.Query{UserID: "usr_1", ModelExact: "gpt"}); err != nil || got.Total != 1 || got.Data[0].ID != "req_a" {
		t.Fatalf("ModelExact=gpt = %#v, want single req_a (not gpt-4)", got.Data)
	}
	// The underscore regression: exact "gpt_4" must return ONLY req_c, never the
	// same-length hyphen "gpt-4" (which a `_`-wildcard LIKE clause would over-match).
	if got, err := st.Query(usage.Query{UserID: "usr_1", ModelExact: "gpt_4"}); err != nil || got.Total != 1 || got.Data[0].ID != "req_c" {
		t.Fatalf("ModelExact=gpt_4 = %#v, want single req_c (not gpt-4 — no LIKE-wildcard over-match)", got.Data)
	}
	if err := st.LastUsageError(); err != nil {
		t.Fatalf("LastUsageError = %v", err)
	}
}

// TestSQLiteUsageQueryExactFiltersMatchEmpty proves the SQL usageWhere exact clause
// fires on the presence flag with an EMPTY value (empty-key group expansion):
// Query{HasSessionIDExact:true, SessionIDExact:""} must return ONLY the empty-session
// row, not every in-scope row. Mutation: dropping the flag from the gate makes the
// empty filter no-op and returns both rows.
func TestSQLiteUsageQueryExactFiltersMatchEmpty(t *testing.T) {
	st := openMigratedTestSQLite(t)
	defer st.Close()

	empty := testUsageEvent("req_empty", "usr_1", "tok_1", "success")
	empty.SessionID = ""
	empty.ServerName = ""
	empty.Model = ""
	set := testUsageEvent("req_set", "usr_1", "tok_1", "success")
	set.SessionID = "sess_x"
	set.ServerName = "prod"
	set.Model = "gpt"
	st.Record(empty)
	st.Record(set)

	if got, err := st.Query(usage.Query{UserID: "usr_1", HasSessionIDExact: true, SessionIDExact: ""}); err != nil || got.Total != 1 || got.Data[0].ID != "req_empty" {
		t.Fatalf("HasSessionIDExact empty = %#v, want single req_empty", got.Data)
	}
	if got, err := st.Query(usage.Query{UserID: "usr_1", HasServerExact: true, ServerExact: ""}); err != nil || got.Total != 1 || got.Data[0].ID != "req_empty" {
		t.Fatalf("HasServerExact empty = %#v, want single req_empty", got.Data)
	}
	if got, err := st.Query(usage.Query{UserID: "usr_1", HasModelExact: true, ModelExact: ""}); err != nil || got.Total != 1 || got.Data[0].ID != "req_empty" {
		t.Fatalf("HasModelExact empty = %#v, want single req_empty", got.Data)
	}
	// WITHOUT the flag an empty value fires no filter -> both rows (pre-fix behavior).
	if got, err := st.Query(usage.Query{UserID: "usr_1", SessionIDExact: ""}); err != nil || got.Total != 2 {
		t.Fatalf("no-flag empty SessionIDExact total = %d, want 2 (both rows)", got.Total)
	}
	if err := st.LastUsageError(); err != nil {
		t.Fatalf("LastUsageError = %v", err)
	}
}

func TestSQLiteUsageTokenFilter(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	st := openMigratedTestSQLite(t)
	defer st.Close()

	withTok := testUsageEvent("req_tok", "usr_1", "tok_a", "success")
	withTok.HTTPStatus, withTok.CreatedAt = 200, now
	chat := testUsageEvent("req_chat", "usr_1", "", "success") // empty token_id = chat
	chat.HTTPStatus, chat.CreatedAt = 200, now.Add(time.Minute)
	st.Record(withTok)
	st.Record(chat)

	// Specific real token.
	real, err := st.Query(usage.Query{UserID: "usr_1", HasTokenFilter: true, TokenID: "tok_a"})
	if err != nil {
		t.Fatalf("Query returned err: %v", err)
	}
	if real.Total != 1 || real.Data[0].ID != "req_tok" {
		t.Fatalf("token=tok_a total=%d data=%v", real.Total, real.Data)
	}

	// Empty token (chat): HasTokenFilter with TokenID "" must match only req_chat.
	none, err := st.Query(usage.Query{UserID: "usr_1", HasTokenFilter: true, TokenID: ""})
	if err != nil {
		t.Fatalf("Query returned err: %v", err)
	}
	if none.Total != 1 || none.Data[0].ID != "req_chat" {
		t.Fatalf("empty token total=%d data=%v", none.Total, none.Data)
	}

	// No token filter: both rows.
	if all, err := st.Query(usage.Query{UserID: "usr_1"}); err != nil || all.Total != 2 {
		t.Fatalf("no filter total=%d, want 2", all.Total)
	}

	// Stats honors the same predicate.
	if s, err := st.Stats(usage.Query{UserID: "usr_1", HasTokenFilter: true, TokenID: ""}); err != nil || s.Totals.TotalRequests != 1 {
		t.Fatalf("stats empty-token requests=%d, want 1", s.Totals.TotalRequests)
	}
}

// TestSQLiteUsageStorePersistsProjectAttribution proves ProjectID/ProjectName
// (design spec §7) round-trip through Record + every read path — All (raw
// column list) and Query (usageEventColumns + scanUsageRows) — exactly like
// ServiceID/ServiceName. A project-less event (the overwhelming default)
// still round-trips as "".
func TestSQLiteUsageStorePersistsProjectAttribution(t *testing.T) {
	st := openMigratedTestSQLite(t)
	defer st.Close()

	withProject := testUsageEvent("req_proj", "usr_1", "tok_1", "success")
	withProject.ProjectID = "proj_widgets"
	withProject.ProjectName = "Widgets"
	noProject := testUsageEvent("req_noproj", "usr_1", "tok_1", "success")
	st.Record(withProject)
	st.Record(noProject)
	if err := st.LastUsageError(); err != nil {
		t.Fatalf("record: %v", err)
	}

	all := st.All()
	if len(all) != 2 {
		t.Fatalf("All() = %d events, want 2", len(all))
	}
	byID := map[string]usage.Event{}
	for _, e := range all {
		byID[e.ID] = e
	}
	if got := byID["req_proj"]; got != withProject {
		t.Fatalf("All() req_proj = %#v, want %#v", got, withProject)
	}
	if got := byID["req_noproj"]; got.ProjectID != "" || got.ProjectName != "" {
		t.Fatalf("All() req_noproj project fields = %q/%q, want empty", got.ProjectID, got.ProjectName)
	}

	page, err := st.Query(usage.Query{UserID: "usr_1"})
	if err != nil {
		t.Fatalf("Query returned err: %v", err)
	}
	if page.Total != 2 {
		t.Fatalf("Query total = %d, want 2", page.Total)
	}
	for _, row := range page.Data {
		if row.ID == "req_proj" && (row.ProjectID != "proj_widgets" || row.ProjectName != "Widgets") {
			t.Fatalf("Query() req_proj project fields = %q/%q, want proj_widgets/Widgets", row.ProjectID, row.ProjectName)
		}
	}
}

// TestSQLiteUsageQueryProjectExactFilter proves ProjectIDExact/HasProjectIDExact
// narrow to one project, mirroring SessionIDExact/ModelExact — including
// firing on the presence flag with an EMPTY value (pinning the no-project
// bucket), and the memory Recorder must agree via matchUsage.
func TestSQLiteUsageQueryProjectExactFilter(t *testing.T) {
	st := openMigratedTestSQLite(t)
	defer st.Close()

	a := testUsageEvent("req_a", "usr_1", "tok_1", "success")
	a.ProjectID, a.ProjectName = "proj_a", "Project A"
	b := testUsageEvent("req_b", "usr_1", "tok_1", "success")
	b.ProjectID, b.ProjectName = "proj_b", "Project B"
	noProject := testUsageEvent("req_c", "usr_1", "tok_1", "success")
	st.Record(a)
	st.Record(b)
	st.Record(noProject)

	if got, err := st.Query(usage.Query{UserID: "usr_1", HasProjectIDExact: true, ProjectIDExact: "proj_a"}); err != nil || got.Total != 1 || got.Data[0].ID != "req_a" {
		t.Fatalf("ProjectIDExact=proj_a = %#v, want single req_a", got.Data)
	}
	// Presence flag with an EMPTY value pins the no-project bucket, not "no filter".
	if got, err := st.Query(usage.Query{UserID: "usr_1", HasProjectIDExact: true, ProjectIDExact: ""}); err != nil || got.Total != 1 || got.Data[0].ID != "req_c" {
		t.Fatalf("HasProjectIDExact empty = %#v, want single req_c", got.Data)
	}
	// Case-insensitive equality (mirrors every other *Exact filter), not a
	// LIKE-wildcard substring/over-match.
	if got, err := st.Query(usage.Query{UserID: "usr_1", HasProjectIDExact: true, ProjectIDExact: "PROJ_A"}); err != nil || got.Total != 1 || got.Data[0].ID != "req_a" {
		t.Fatalf("ProjectIDExact case-insensitive = %#v, want single req_a", got.Data)
	}
	// No flag, no value: no filter at all.
	if got, err := st.Query(usage.Query{UserID: "usr_1"}); err != nil || got.Total != 3 {
		t.Fatalf("no project filter total = %d, want 3", got.Total)
	}
	if err := st.LastUsageError(); err != nil {
		t.Fatalf("LastUsageError = %v", err)
	}
}

// TestSQLiteUsageQueryProjectIDsInListFilter proves the ProjectIDs scope
// IN-list (applyUsageScope §8) matches any listed project, and — the
// security-critical case — a non-nil EMPTY slice matches ZERO rows rather
// than being treated as "no filter" (a caller who is a member of no project
// must never see any project-attributed row).
func TestSQLiteUsageQueryProjectIDsInListFilter(t *testing.T) {
	st := openMigratedTestSQLite(t)
	defer st.Close()

	a := testUsageEvent("req_a", "usr_1", "tok_1", "success")
	a.ProjectID = "proj_a"
	b := testUsageEvent("req_b", "usr_1", "tok_1", "success")
	b.ProjectID = "proj_b"
	c := testUsageEvent("req_c", "usr_1", "tok_1", "success")
	c.ProjectID = "proj_c"
	st.Record(a)
	st.Record(b)
	st.Record(c)

	if got, err := st.Query(usage.Query{UserID: "usr_1", ProjectIDs: []string{"proj_a", "proj_b"}}); err != nil || got.Total != 2 {
		t.Fatalf("ProjectIDs=[a,b] total = %d, want 2: %+v", got.Total, got.Data)
	}
	// A non-nil EMPTY slice must yield ZERO rows, never "no filter" (nil).
	if got, err := st.Query(usage.Query{UserID: "usr_1", ProjectIDs: []string{}}); err != nil || got.Total != 0 {
		t.Fatalf("ProjectIDs=[] total = %d, want 0 (member-of-nothing must see nothing)", got.Total)
	}
	// nil ProjectIDs (the zero value): no filter at all.
	if got, err := st.Query(usage.Query{UserID: "usr_1"}); err != nil || got.Total != 3 {
		t.Fatalf("nil ProjectIDs total = %d, want 3", got.Total)
	}
	if err := st.LastUsageError(); err != nil {
		t.Fatalf("LastUsageError = %v", err)
	}
}

// TestSQLiteUsageRecordQueryStatsTimeSeriesReturnErrorOnClosedStore proves the
// ST-2 fix: Record/Query/Stats/TimeSeries now return the REAL underlying error
// (here: the closed *sql.DB's sql.ErrConnDone) instead of silently swallowing
// it into the lastUsageError side-channel only. Before ST-2 these methods had
// no error return at all, so a failed insert silently dropped the event and a
// failed Query/Stats/TimeSeries returned an empty result indistinguishable
// from "genuinely no matching rows". LastUsageError must still observe the
// same failures (Step 1's health-visibility side-channel stays wired).
func TestSQLiteUsageRecordQueryStatsTimeSeriesReturnErrorOnClosedStore(t *testing.T) {
	st := openMigratedTestSQLite(t)
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := st.Record(testUsageEvent("req_x", "usr_1", "tok_1", "success")); err == nil {
		t.Fatal("Record on a closed store: want a non-nil error, got nil")
	}
	if _, err := st.Query(usage.Query{UserID: "usr_1"}); err == nil {
		t.Fatal("Query on a closed store: want a non-nil error, got nil")
	}
	if _, err := st.Stats(usage.Query{UserID: "usr_1"}); err == nil {
		t.Fatal("Stats on a closed store: want a non-nil error, got nil")
	}
	if _, err := st.TimeSeries(usage.Query{UserID: "usr_1", From: time.Now(), To: time.Now()}, 10); err == nil {
		t.Fatal("TimeSeries on a closed store: want a non-nil error, got nil")
	}
	// The Step 1 health side-channel must still observe the same failure.
	if err := st.LastUsageError(); err == nil {
		t.Fatal("LastUsageError after failures: want a non-nil error, got nil")
	}
}
