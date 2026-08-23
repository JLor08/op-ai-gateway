// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package usage

import (
	"context"
	"testing"
	"time"
)

func TestRecorderImplementsStore(t *testing.T) {
	var _ Store = (*Recorder)(nil)
}

func TestRecorderStoresUsageByUser(t *testing.T) {
	rec := NewRecorder()
	event := Event{
		ID:           "req_1",
		UserID:       "usr_1",
		TokenID:      "tok_1",
		Model:        "qwen-coder",
		Provider:     "mock",
		Host:         "mock-host",
		InputTokens:  2,
		OutputTokens: 3,
		TotalTokens:  5,
		LatencyMS:    42,
		Status:       "success",
		CreatedAt:    time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	}

	rec.Record(event)

	events := rec.ByUser("usr_1")
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].TotalTokens != 5 {
		t.Fatalf("TotalTokens = %d, want 5", events[0].TotalTokens)
	}
}

func TestRecorderByUserFiltersAndPreservesEvents(t *testing.T) {
	rec := NewRecorder()
	first := Event{
		ID:           "req_1",
		UserID:       "usr_1",
		TokenID:      "tok_1",
		SessionID:    "sess_1",
		APIFlavor:    "openai",
		Model:        "qwen-coder",
		Provider:     "mock",
		Host:         "mock-host",
		InputTokens:  2,
		OutputTokens: 3,
		TotalTokens:  5,
		LatencyMS:    42,
		Status:       "success",
		ErrorCode:    "",
		CreatedAt:    time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	}
	second := Event{
		ID:           "req_2",
		UserID:       "usr_2",
		TokenID:      "tok_2",
		SessionID:    "sess_2",
		APIFlavor:    "anthropic",
		Model:        "claude-sonnet",
		Provider:     "mock",
		Host:         "other-host",
		InputTokens:  7,
		OutputTokens: 11,
		TotalTokens:  18,
		LatencyMS:    64,
		Status:       "error",
		ErrorCode:    "provider_error",
		CreatedAt:    time.Date(2026, 7, 9, 13, 0, 0, 0, time.UTC),
	}

	rec.Record(first)
	rec.Record(second)

	events := rec.ByUser("usr_1")
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0] != first {
		t.Fatalf("event = %#v, want %#v", events[0], first)
	}

	events[0].TotalTokens = 99

	stored := rec.ByUser("usr_1")
	if len(stored) != 1 {
		t.Fatalf("stored events = %d, want 1", len(stored))
	}
	if stored[0] != first {
		t.Fatalf("stored event = %#v, want %#v", stored[0], first)
	}
}

func TestRecorderPreservesRouteID(t *testing.T) {
	recorder := NewRecorder()
	recorder.Record(Event{ID: "req_1", UserID: "usr_dev", TokenID: "tok_dev", RouteID: "route_qwen", Model: "qwen-coder"})

	events := recorder.All()

	if events[0].RouteID != "route_qwen" {
		t.Fatalf("RouteID = %q, want route_qwen", events[0].RouteID)
	}
}

func TestRecorderAllReturnsCopy(t *testing.T) {
	rec := NewRecorder()
	event := Event{
		ID:          "req_1",
		UserID:      "usr_1",
		TokenID:     "tok_1",
		TotalTokens: 5,
		CreatedAt:   time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	}
	rec.Record(event)

	events := rec.All()
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	events[0].TotalTokens = 99

	stored := rec.All()
	if len(stored) != 1 {
		t.Fatalf("stored events = %d, want 1", len(stored))
	}
	if stored[0].TotalTokens != 5 {
		t.Fatalf("stored TotalTokens = %d, want 5", stored[0].TotalTokens)
	}
}

func recorderEvent(id, userID string) Event {
	return Event{
		ID:              id,
		UserID:          userID,
		TokenID:         "tok_" + userID,
		Model:           "qwen-coder",
		Provider:        "mock",
		Host:            "mock-host",
		ServerName:      "GPU-1",
		TokenName:       "T-" + userID,
		TotalTokens:     5,
		LatencyMS:       42,
		HTTPStatus:      200,
		Status:          "success",
		PromptPerSecond: 10,
		TokensPerSecond: 20,
		CreatedAt:       time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC),
	}
}

func TestRecorderQueryFiltersSortsAndPaginates(t *testing.T) {
	rec := NewRecorder()

	a := recorderEvent("req_a", "usr_1")
	a.LatencyMS = 10
	a.CreatedAt = time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	b := recorderEvent("req_b", "usr_1")
	b.LatencyMS = 30
	b.CreatedAt = time.Date(2026, 7, 10, 13, 0, 0, 0, time.UTC)
	other := recorderEvent("req_c", "usr_2") // must be filtered out (own scope)
	rec.Record(a)
	rec.Record(b)
	rec.Record(other)

	// Own scope: only usr_1 rows, newest-first default (created_at desc).
	page, err := rec.Query(Query{UserID: "usr_1"})
	if err != nil {
		t.Fatalf("Query returned err: %v", err)
	}
	if page.Total != 2 || len(page.Data) != 2 {
		t.Fatalf("own total/len = %d/%d, want 2/2", page.Total, len(page.Data))
	}
	if page.Data[0].ID != "req_b" || page.Data[1].ID != "req_a" {
		t.Fatalf("default order = %s,%s, want req_b,req_a", page.Data[0].ID, page.Data[1].ID)
	}
	if page.Page != 1 || page.Limit != 25 || page.TotalPages != 1 {
		t.Fatalf("page meta = %#v", page)
	}
	if page.Data[0].UserName != "" {
		t.Fatalf("own scope must not resolve UserName, got %q", page.Data[0].UserName)
	}

	// Sort by latency ascending.
	asc, err := rec.Query(Query{UserID: "usr_1", Sort: "latency_ms", Order: "asc"})
	if err != nil {
		t.Fatalf("Query returned err: %v", err)
	}
	if asc.Data[0].ID != "req_a" || asc.Data[1].ID != "req_b" {
		t.Fatalf("latency asc = %s,%s, want req_a,req_b", asc.Data[0].ID, asc.Data[1].ID)
	}

	// Invalid sort/order/limit/page clamp to defaults.
	clamped, err := rec.Query(Query{UserID: "usr_1", Sort: "bogus", Order: "sideways", Limit: 7, Page: 0})
	if err != nil {
		t.Fatalf("Query returned err: %v", err)
	}
	if clamped.Limit != 25 || clamped.Page != 1 {
		t.Fatalf("clamp = limit %d page %d, want 25/1", clamped.Limit, clamped.Page)
	}
	if clamped.Data[0].ID != "req_b" {
		t.Fatalf("bogus sort should fall back to created_at desc, got %s", clamped.Data[0].ID)
	}

	// Exact model filter misses.
	if miss, err := rec.Query(Query{UserID: "usr_1", Model: "no-such-model"}); err != nil || miss.Total != 0 || len(miss.Data) != 0 {
		t.Fatalf("model miss total/len = %d/%d, want 0/0", miss.Total, len(miss.Data))
	}
}

func TestRecorderQueryStablePaginationOverTies(t *testing.T) {
	rec := NewRecorder()
	tie := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 30; i++ {
		e := recorderEvent("req_"+string(rune('a'+i/26))+string(rune('a'+i%26)), "usr_1")
		e.CreatedAt = tie // identical timestamp -> id tiebreak decides order
		rec.Record(e)
	}
	p1, err := rec.Query(Query{UserID: "usr_1", Sort: "created_at", Order: "asc", Page: 1, Limit: 25})
	if err != nil {
		t.Fatalf("Query returned err: %v", err)
	}
	p2, err := rec.Query(Query{UserID: "usr_1", Sort: "created_at", Order: "asc", Page: 2, Limit: 25})
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
	for _, r := range append(append([]Row{}, p1.Data...), p2.Data...) {
		if seen[r.ID] {
			t.Fatalf("id %s appeared on two pages (unstable tiebreak)", r.ID)
		}
		seen[r.ID] = true
	}
	if len(seen) != 30 {
		t.Fatalf("union across pages = %d ids, want 30", len(seen))
	}
}

func TestRecorderQueryScopeAllUsesNameResolver(t *testing.T) {
	rec := NewRecorder()
	rec.ResolveUserName = func(userID string) string {
		return map[string]string{"usr_1": "Ada Admin", "usr_2": "Bob Builder"}[userID]
	}
	rec.Record(recorderEvent("req_a", "usr_1"))
	rec.Record(recorderEvent("req_b", "usr_2"))

	all, err := rec.Query(Query{ScopeAll: true, Sort: "token_name", Order: "asc"})
	if err != nil {
		t.Fatalf("Query returned err: %v", err)
	}
	if all.Total != 2 {
		t.Fatalf("ScopeAll total = %d, want 2 (cross-user)", all.Total)
	}
	names := map[string]string{}
	for _, r := range all.Data {
		names[r.UserID] = r.UserName
	}
	if names["usr_1"] != "Ada Admin" || names["usr_2"] != "Bob Builder" {
		t.Fatalf("resolved names = %#v", names)
	}
}

// queryIDs extracts the row ids from a Recorder.Query result. err is accepted
// (not just Page) so every `queryIDs(rec.Query(...))` call site keeps working
// unchanged now that Query returns (Page, error); the memory Recorder's Query
// never actually errors, so a non-nil err here is a test bug, not an expected
// path — panic rather than silently ignore it.
func queryIDs(page Page, err error) []string {
	if err != nil {
		panic(err)
	}
	ids := make([]string, 0, len(page.Data))
	for _, r := range page.Data {
		ids = append(ids, r.ID)
	}
	return ids
}

func TestRecorderQueryServerFilter(t *testing.T) {
	rec := NewRecorder()

	named := recorderEvent("req_named", "usr_1") // ServerName=GPU-1, Host=mock-host
	fallback := recorderEvent("req_fallback", "usr_1")
	fallback.ServerName = "" // no server name -> host is the fallback identity
	fallback.Host = "mock-host"
	rec.Record(named)
	rec.Record(fallback)

	// server_name substring hits only the named row (fallback has no server_name).
	if got := queryIDs(rec.Query(Query{UserID: "usr_1", Server: "GPU-1"})); len(got) != 1 || got[0] != "req_named" {
		t.Fatalf("Server=GPU-1 ids = %v, want [req_named]", got)
	}

	// Host substring matches BOTH rows: host is now checked even when
	// ServerName is non-empty, so a server routed via ServerName=GPU-1 but
	// actually reaching host mock-host is still findable by host.
	got := queryIDs(rec.Query(Query{UserID: "usr_1", Server: "mock-host"}))
	if len(got) != 2 {
		t.Fatalf("Server=mock-host ids = %v, want 2 ids (req_named, req_fallback)", got)
	}
	seen := map[string]bool{}
	for _, id := range got {
		seen[id] = true
	}
	if !seen["req_named"] || !seen["req_fallback"] {
		t.Fatalf("Server=mock-host ids = %v, want both req_named and req_fallback", got)
	}
}

func TestRecorderQueryModelServerFilterSubstringCaseInsensitive(t *testing.T) {
	rec := NewRecorder()

	gpu := recorderEvent("req_gpu", "usr_1") // Model=qwen-coder, ServerName=GPU-1, Host=mock-host
	edge := recorderEvent("req_edge", "usr_1")
	edge.Model = "claude-sonnet"
	edge.ServerName = "GPU-2"
	edge.Host = "edge-node-7"
	bare := recorderEvent("req_bare", "usr_1")
	bare.Model = "llama-3"
	bare.ServerName = ""
	bare.Host = "bare-host"
	rec.Record(gpu)
	rec.Record(edge)
	rec.Record(bare)

	// Model: case-insensitive substring, not just an exact match.
	if got := queryIDs(rec.Query(Query{UserID: "usr_1", Model: "QWEN"})); len(got) != 1 || got[0] != "req_gpu" {
		t.Fatalf("Model=QWEN ids = %v, want [req_gpu]", got)
	}
	if got := queryIDs(rec.Query(Query{UserID: "usr_1", Model: "coder"})); len(got) != 1 || got[0] != "req_gpu" {
		t.Fatalf("Model=coder (suffix substring) ids = %v, want [req_gpu]", got)
	}

	// Server: substring over server_name matches every row whose name contains it.
	if got := queryIDs(rec.Query(Query{UserID: "usr_1", Server: "gpu"})); len(got) != 2 {
		t.Fatalf("Server=gpu ids = %v, want 2 ids (req_gpu, req_edge)", got)
	}

	// ServerExact: an EXACT (case-insensitive) server_name match — no substring,
	// no host fallback — so the per-server Performance view for "GPU-1" does NOT
	// also pull "GPU-2" traffic (the substring `Server:"gpu"` above matches both).
	if got := queryIDs(rec.Query(Query{UserID: "usr_1", ServerExact: "GPU-1"})); len(got) != 1 || got[0] != "req_gpu" {
		t.Fatalf("ServerExact=GPU-1 ids = %v, want [req_gpu] only (not GPU-2)", got)
	}
	if got := queryIDs(rec.Query(Query{UserID: "usr_1", ServerExact: "gpu-1"})); len(got) != 1 || got[0] != "req_gpu" {
		t.Fatalf("ServerExact=gpu-1 (case-insensitive) ids = %v, want [req_gpu]", got)
	}
	if got := queryIDs(rec.Query(Query{UserID: "usr_1", ServerExact: "GPU"})); len(got) != 0 {
		t.Fatalf("ServerExact=GPU ids = %v, want none (no server is named exactly \"GPU\")", got)
	}

	// Server: host is checked even when server_name is set and does not match --
	// the bug this task fixes (previously host was only consulted when
	// server_name was empty).
	if got := queryIDs(rec.Query(Query{UserID: "usr_1", Server: "edge"})); len(got) != 1 || got[0] != "req_edge" {
		t.Fatalf("Server=edge ids = %v, want [req_edge] (matched via host despite non-empty ServerName)", got)
	}

	// Server: host fallback stays case-insensitive when server_name is empty.
	if got := queryIDs(rec.Query(Query{UserID: "usr_1", Server: "BARE-HOST"})); len(got) != 1 || got[0] != "req_bare" {
		t.Fatalf("Server=BARE-HOST ids = %v, want [req_bare]", got)
	}
}

func TestRecorderQueryStatusFilter(t *testing.T) {
	rec := NewRecorder()

	ok := recorderEvent("req_ok", "usr_1") // Status=success, HTTPStatus=200
	errHTTP := recorderEvent("req_err_http", "usr_1")
	errHTTP.Status = "success"
	errHTTP.HTTPStatus = 503 // >=400 -> error even with a success status string
	errStream := recorderEvent("req_err_stream", "usr_1")
	errStream.Status = "error"
	errStream.HTTPStatus = 200 // mid-stream failure still carries 200
	rec.Record(ok)
	rec.Record(errHTTP)
	rec.Record(errStream)

	errIDs := queryIDs(rec.Query(Query{UserID: "usr_1", Status: "error", Sort: "http_status", Order: "asc"}))
	if len(errIDs) != 2 || errIDs[0] != "req_err_stream" || errIDs[1] != "req_err_http" {
		t.Fatalf("Status=error ids = %v, want [req_err_stream req_err_http]", errIDs)
	}

	okIDs := queryIDs(rec.Query(Query{UserID: "usr_1", Status: "success"}))
	if len(okIDs) != 1 || okIDs[0] != "req_ok" {
		t.Fatalf("Status=success ids = %v, want [req_ok] (complement of error)", okIDs)
	}
}

func TestRecorderQueryFreeTextSearch(t *testing.T) {
	rec := NewRecorder()

	e := recorderEvent("req_a", "usr_1") // Host=mock-host, TokenName=T-usr_1
	rec.Record(e)

	// Case-insensitive match on host.
	if got, err := rec.Query(Query{UserID: "usr_1", Q: "MOCK-HOST"}); err != nil || got.Total != 1 {
		t.Fatalf("Q=MOCK-HOST total = %d, want 1 (case-insensitive host match)", got.Total)
	}
	// Case-insensitive match on token_name.
	if got, err := rec.Query(Query{UserID: "usr_1", Q: "t-USR_1"}); err != nil || got.Total != 1 {
		t.Fatalf("Q=t-USR_1 total = %d, want 1 (case-insensitive token_name match)", got.Total)
	}
	// Absent text matches nothing.
	if got, err := rec.Query(Query{UserID: "usr_1", Q: "no-such-substring"}); err != nil || got.Total != 0 {
		t.Fatalf("Q=no-such-substring total = %d, want 0", got.Total)
	}
}

func TestRecorderQueryTimeWindow(t *testing.T) {
	rec := NewRecorder()

	tA := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	tB := time.Date(2026, 7, 10, 13, 0, 0, 0, time.UTC)
	tC := time.Date(2026, 7, 10, 14, 0, 0, 0, time.UTC)
	a := recorderEvent("req_a", "usr_1")
	a.CreatedAt = tA
	b := recorderEvent("req_b", "usr_1")
	b.CreatedAt = tB
	c := recorderEvent("req_c", "usr_1")
	c.CreatedAt = tC
	rec.Record(a)
	rec.Record(b)
	rec.Record(c)

	// Both bounds are inclusive: From=To=tB returns exactly the boundary row.
	if got := queryIDs(rec.Query(Query{UserID: "usr_1", From: tB, To: tB})); len(got) != 1 || got[0] != "req_b" {
		t.Fatalf("From=To=tB ids = %v, want [req_b] (inclusive boundaries)", got)
	}
	// One nanosecond past tB excludes tB (and tA), leaving only tC.
	if got := queryIDs(rec.Query(Query{UserID: "usr_1", From: tB.Add(time.Nanosecond)})); len(got) != 1 || got[0] != "req_c" {
		t.Fatalf("From=tB+1ns ids = %v, want [req_c] (just-outside excluded)", got)
	}
	// One nanosecond before tB excludes tB (and tC), leaving only tA.
	if got := queryIDs(rec.Query(Query{UserID: "usr_1", To: tB.Add(-time.Nanosecond)})); len(got) != 1 || got[0] != "req_a" {
		t.Fatalf("To=tB-1ns ids = %v, want [req_a] (just-outside excluded)", got)
	}
}

func TestRecorderQueryOwnerFilter(t *testing.T) {
	rec := NewRecorder()
	rec.ResolveUserName = func(userID string) string {
		return map[string]string{"usr_ada": "Ada Admin", "usr_bob": "Bob Builder"}[userID]
	}
	rec.Record(recorderEvent("req_ada", "usr_ada"))
	rec.Record(recorderEvent("req_bob", "usr_bob"))

	// ScopeAll: owner matches the resolved name, case-insensitively. "admin"
	// appears only in the name "Ada Admin", not in the user_id, so this isolates
	// name matching.
	if got := queryIDs(rec.Query(Query{ScopeAll: true, Owner: "ADMIN"})); len(got) != 1 || got[0] != "req_ada" {
		t.Fatalf("Owner=ADMIN (name) ids = %v, want [req_ada]", got)
	}
	// ScopeAll: owner also matches the raw user_id substring.
	if got := queryIDs(rec.Query(Query{ScopeAll: true, Owner: "usr_bob"})); len(got) != 1 || got[0] != "req_bob" {
		t.Fatalf("Owner=usr_bob (id) ids = %v, want [req_bob]", got)
	}
	// Own scope: no name available, so only the user_id is matched.
	if got := queryIDs(rec.Query(Query{UserID: "usr_ada", Owner: "usr_ada"})); len(got) != 1 || got[0] != "req_ada" {
		t.Fatalf("own-scope Owner=usr_ada ids = %v, want [req_ada]", got)
	}
	// Own scope: a name-only needle (not in the user_id) must NOT match — no join.
	if got := queryIDs(rec.Query(Query{UserID: "usr_ada", Owner: "Admin"})); len(got) != 0 {
		t.Fatalf("own-scope Owner=Admin (name only) ids = %v, want [] (no users join)", got)
	}
}

func TestRecorderQueryNumericRangeFilters(t *testing.T) {
	rec := NewRecorder()
	mk := func(id string, total int, latency int64, pps float64) Event {
		e := recorderEvent(id, "usr_1")
		e.TotalTokens = total
		e.LatencyMS = latency
		e.PromptPerSecond = pps
		return e
	}
	rec.Record(mk("req_lo", 5, 50, 1))
	rec.Record(mk("req_mid", 50, 150, 2.5))
	rec.Record(mk("req_hi", 500, 400, 9))

	// min-only.
	if got := queryIDs(rec.Query(Query{UserID: "usr_1", NumericMin: map[string]float64{"total_tokens": 50}, Sort: "total_tokens", Order: "asc"})); len(got) != 2 || got[0] != "req_mid" || got[1] != "req_hi" {
		t.Fatalf("total_tokens>=50 ids = %v, want [req_mid req_hi]", got)
	}
	// max-only.
	if got := queryIDs(rec.Query(Query{UserID: "usr_1", NumericMax: map[string]float64{"latency_ms": 150}, Sort: "latency_ms", Order: "asc"})); len(got) != 2 || got[0] != "req_lo" || got[1] != "req_mid" {
		t.Fatalf("latency_ms<=150 ids = %v, want [req_lo req_mid]", got)
	}
	// range (min AND max) on a float column, plus a second column ANDed in.
	q := Query{
		UserID:     "usr_1",
		NumericMin: map[string]float64{"prompt_per_second": 2, "total_tokens": 10},
		NumericMax: map[string]float64{"prompt_per_second": 5},
	}
	if got := queryIDs(rec.Query(q)); len(got) != 1 || got[0] != "req_mid" {
		t.Fatalf("2<=pps<=5 AND total>=10 ids = %v, want [req_mid]", got)
	}
	// An unknown column id in the map is ignored (whitelist-gated).
	if got, err := rec.Query(Query{UserID: "usr_1", NumericMin: map[string]float64{"bogus_col": 999}}); err != nil || got.Total != 3 {
		t.Fatalf("unknown numeric column must be ignored, total = %d, want 3", got.Total)
	}
}

func TestRecorderQueryReqPathContentTypeProviderModelFilters(t *testing.T) {
	rec := NewRecorder()

	chat := recorderEvent("req_chat", "usr_1")
	chat.ReqPath = "/v1/chat/completions"
	chat.ContentType = "text/event-stream"
	chat.ProviderModel = "qwen-72b"
	embed := recorderEvent("req_embed", "usr_1")
	embed.ReqPath = "/v1/embeddings"
	embed.ContentType = "application/json"
	embed.ProviderModel = "bge-large"
	rec.Record(chat)
	rec.Record(embed)

	// req_path: case-insensitive substring.
	if got := queryIDs(rec.Query(Query{UserID: "usr_1", ReqPath: "CHAT"})); len(got) != 1 || got[0] != "req_chat" {
		t.Fatalf("ReqPath=CHAT ids = %v, want [req_chat]", got)
	}
	// content_type: substring.
	if got := queryIDs(rec.Query(Query{UserID: "usr_1", ContentType: "event-stream"})); len(got) != 1 || got[0] != "req_chat" {
		t.Fatalf("ContentType=event-stream ids = %v, want [req_chat]", got)
	}
	if got := queryIDs(rec.Query(Query{UserID: "usr_1", ContentType: "json"})); len(got) != 1 || got[0] != "req_embed" {
		t.Fatalf("ContentType=json ids = %v, want [req_embed]", got)
	}
	// provider_model: case-insensitive substring.
	if got := queryIDs(rec.Query(Query{UserID: "usr_1", ProviderModel: "BGE"})); len(got) != 1 || got[0] != "req_embed" {
		t.Fatalf("ProviderModel=BGE ids = %v, want [req_embed]", got)
	}
	// A miss narrows to nothing.
	if got, err := rec.Query(Query{UserID: "usr_1", ReqPath: "no-such-path"}); err != nil || got.Total != 0 {
		t.Fatalf("ReqPath miss total = %d, want 0", got.Total)
	}
}

func TestRecorderQueryStreamTriState(t *testing.T) {
	rec := NewRecorder()
	streamed := recorderEvent("req_stream", "usr_1")
	streamed.Stream = true
	plain := recorderEvent("req_plain", "usr_1")
	plain.Stream = false
	rec.Record(streamed)
	rec.Record(plain)

	// stream="true" keeps only streamed rows.
	if got := queryIDs(rec.Query(Query{UserID: "usr_1", Stream: "true"})); len(got) != 1 || got[0] != "req_stream" {
		t.Fatalf("Stream=true ids = %v, want [req_stream]", got)
	}
	// stream="false" keeps only non-streamed rows.
	if got := queryIDs(rec.Query(Query{UserID: "usr_1", Stream: "false"})); len(got) != 1 || got[0] != "req_plain" {
		t.Fatalf("Stream=false ids = %v, want [req_plain]", got)
	}
	// Empty stream filter -> no narrowing (both rows).
	if got, err := rec.Query(Query{UserID: "usr_1", Stream: ""}); err != nil || got.Total != 2 {
		t.Fatalf("Stream=\"\" total = %d, want 2 (no filter)", got.Total)
	}
}

func TestRecorderQueryCachedTokensNumericFilter(t *testing.T) {
	rec := NewRecorder()
	mk := func(id string, cached int) Event {
		e := recorderEvent(id, "usr_1")
		e.CachedTokens = cached
		return e
	}
	rec.Record(mk("req_lo", 5))
	rec.Record(mk("req_mid", 50))
	rec.Record(mk("req_hi", 500))

	// cached_tokens is now whitelisted for min/max range filtering.
	if got := queryIDs(rec.Query(Query{UserID: "usr_1", NumericMin: map[string]float64{"cached_tokens": 50}, NumericMax: map[string]float64{"cached_tokens": 100}, Sort: "created_at", Order: "asc"})); len(got) != 1 || got[0] != "req_mid" {
		t.Fatalf("50<=cached_tokens<=100 ids = %v, want [req_mid]", got)
	}
	if got, err := rec.Query(Query{UserID: "usr_1", NumericMin: map[string]float64{"cached_tokens": 50}}); err != nil || got.Total != 2 {
		t.Fatalf("cached_tokens>=50 total = %d, want 2 (req_mid, req_hi)", got.Total)
	}
}

func TestRecorderQueryTimeBounds(t *testing.T) {
	rec := NewRecorder()
	tA := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	tB := time.Date(2026, 7, 10, 13, 0, 0, 0, time.UTC)
	tC := time.Date(2026, 7, 10, 14, 0, 0, 0, time.UTC)
	for id, ts := range map[string]time.Time{"req_a": tA, "req_b": tB, "req_c": tC} {
		e := recorderEvent(id, "usr_1")
		e.CreatedAt = ts
		rec.Record(e)
	}

	// TimeFrom/TimeTo are ANDed with From/To; here they carve out the middle row.
	if got := queryIDs(rec.Query(Query{UserID: "usr_1", TimeFrom: tB, TimeTo: tB})); len(got) != 1 || got[0] != "req_b" {
		t.Fatalf("TimeFrom=TimeTo=tB ids = %v, want [req_b] (inclusive)", got)
	}
	// TimeFrom composes with an existing From (the stricter bound wins).
	if got := queryIDs(rec.Query(Query{UserID: "usr_1", From: tA, TimeFrom: tC})); len(got) != 1 || got[0] != "req_c" {
		t.Fatalf("From=tA + TimeFrom=tC ids = %v, want [req_c] (AND of both lower bounds)", got)
	}
}

func TestRecorderQueryHugePageDoesNotPanic(t *testing.T) {
	rec := NewRecorder()
	rec.Record(recorderEvent("req_a", "usr_1"))
	rec.Record(recorderEvent("req_b", "usr_1"))

	// A huge page would overflow (page-1)*limit to a negative start and panic
	// on the slice bounds; it must instead yield an empty page past the last.
	got, err := rec.Query(Query{UserID: "usr_1", Page: 400000000000000000, Limit: 25})
	if err != nil {
		t.Fatalf("Query returned err: %v", err)
	}
	if got.Total != 2 || len(got.Data) != 0 {
		t.Fatalf("huge page total/len = %d/%d, want 2/0 (empty page past the last)", got.Total, len(got.Data))
	}
}

func TestRecorderStats(t *testing.T) {
	rec := NewRecorder()

	ok1 := recorderEvent("req_1", "usr_1")
	ok1.InputTokens, ok1.OutputTokens, ok1.CachedTokens = 5, 7, 2
	ok1.PromptPerSecond, ok1.TokensPerSecond = 10, 20

	ok2 := recorderEvent("req_2", "usr_1")
	ok2.InputTokens, ok2.OutputTokens, ok2.CachedTokens = 3, 1, 0
	ok2.PromptPerSecond, ok2.TokensPerSecond = 30, 40

	// Streaming failure: status="error" but http_status=200, and 0 speeds.
	streamErr := recorderEvent("req_3", "usr_1")
	streamErr.Status = "error"
	streamErr.HTTPStatus = 200
	streamErr.InputTokens, streamErr.OutputTokens, streamErr.CachedTokens = 1, 0, 0
	streamErr.PromptPerSecond, streamErr.TokensPerSecond = 0, 0

	rec.Record(ok1)
	rec.Record(ok2)
	rec.Record(streamErr)

	stats, err := rec.Stats(Query{UserID: "usr_1"})
	if err != nil {
		t.Fatalf("Stats returned err: %v", err)
	}
	if stats.Totals.TotalRequests != 3 {
		t.Fatalf("TotalRequests = %d, want 3", stats.Totals.TotalRequests)
	}
	if stats.Totals.ErrorCount != 1 {
		t.Fatalf("ErrorCount = %d, want 1 (streaming error counts)", stats.Totals.ErrorCount)
	}
	if stats.Totals.CachedTokens != 2 || stats.Totals.InputTokens != 9 || stats.Totals.OutputTokens != 8 {
		t.Fatalf("totals = %#v", stats.Totals)
	}
	// Histograms exclude the 0-speed error row: N=2 non-zero each.
	promptCount := 0
	for _, b := range stats.PromptPerSecond.Bins {
		promptCount += b.Count
	}
	if promptCount != 2 {
		t.Fatalf("prompt histogram total count = %d, want 2 (zeros excluded)", promptCount)
	}
	if !approxEqual(stats.PromptPerSecond.Min, 10) || !approxEqual(stats.PromptPerSecond.Max, 30) {
		t.Fatalf("prompt min/max = %v/%v, want 10/30", stats.PromptPerSecond.Min, stats.PromptPerSecond.Max)
	}
	if !approxEqual(stats.TokensPerSecond.Min, 20) || !approxEqual(stats.TokensPerSecond.Max, 40) {
		t.Fatalf("tokens min/max = %v/%v, want 20/40", stats.TokensPerSecond.Min, stats.TokensPerSecond.Max)
	}
}

// StatTotals.TotalEnergyWh is a plain SUM(EnergyWh) over the filtered set,
// alongside the existing CachedTokens/InputTokens/OutputTokens sums.
func TestRecorderStatsTotalEnergyWh(t *testing.T) {
	rec := NewRecorder()
	a := recorderEvent("req_1", "usr_1")
	a.EnergyWh = 1.5
	b := recorderEvent("req_2", "usr_1")
	b.EnergyWh = 2.5
	other := recorderEvent("req_3", "usr_2")
	other.EnergyWh = 99 // different user, excluded from own-scope stats
	rec.Record(a)
	rec.Record(b)
	rec.Record(other)

	stats, err := rec.Stats(Query{UserID: "usr_1"})
	if err != nil {
		t.Fatalf("Stats returned err: %v", err)
	}
	if !approxEqual(stats.Totals.TotalEnergyWh, 4.0) {
		t.Fatalf("TotalEnergyWh = %v, want 4.0 (1.5+2.5, other user excluded)", stats.Totals.TotalEnergyWh)
	}
}

// EnergyByServer sums EnergyWh grouped by Host over the matchUsage-filtered
// set (same predicate as Stats), and a host with no matching rows is absent
// from the returned map (never a zero entry).
func TestRecorderEnergyByServer(t *testing.T) {
	rec := NewRecorder()
	a := recorderEvent("req_1", "usr_1")
	a.Host = "srv_a"
	a.EnergyWh = 1.5
	b := recorderEvent("req_2", "usr_1")
	b.Host = "srv_a"
	b.EnergyWh = 0.5
	c := recorderEvent("req_3", "usr_1")
	c.Host = "srv_b"
	c.EnergyWh = 3.0
	other := recorderEvent("req_4", "usr_2")
	other.Host = "srv_c"
	other.EnergyWh = 42 // different user, excluded from own-scope query

	for _, e := range []Event{a, b, c, other} {
		rec.Record(e)
	}

	got, err := rec.EnergyByServer(context.Background(), Query{UserID: "usr_1"})
	if err != nil {
		t.Fatalf("EnergyByServer: %v", err)
	}
	if !approxEqual(got["srv_a"], 2.0) {
		t.Fatalf("srv_a = %v, want 2.0 (1.5+0.5)", got["srv_a"])
	}
	if !approxEqual(got["srv_b"], 3.0) {
		t.Fatalf("srv_b = %v, want 3.0", got["srv_b"])
	}
	if _, ok := got["srv_c"]; ok {
		t.Fatalf("srv_c present in own-scope map, want absent (belongs to usr_2)")
	}
}

// UsageGroups aggregates GROUP BY (dimension, host): one bucket per (group
// value, host), with count/error/token/energy sums and min/max created_at. This
// mirrors the sqlite conformance test's data so the memory + SQL paths are
// proven to fold to the same aggregates. An unknown dimension is an error.
func TestRecorderUsageGroups(t *testing.T) {
	rec := NewRecorder()
	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	t0, t1, t2, t3 := base, base.Add(time.Minute), base.Add(2*time.Minute), base.Add(3*time.Minute)

	mk := func(id, sess, model, host, server string, in, out, cached, cw int, energy float64, status string, http int, at time.Time) Event {
		return Event{
			ID: id, UserID: "u1", TokenID: "tok1", SessionID: sess, Model: model, Host: host, ServerName: server,
			InputTokens: in, OutputTokens: out, CachedTokens: cached, CacheWriteTokens: cw, EnergyWh: energy,
			Status: status, HTTPStatus: http, CreatedAt: at,
		}
	}
	events := []Event{
		mk("e1", "sess_a", "gpt-4o", "srv1", "server-one", 10, 20, 2, 1, 0.5, "ok", 200, t0),
		mk("e2", "sess_a", "gpt-4o", "srv1", "server-one", 5, 7, 1, 0, 0.25, "ok", 200, t1),
		mk("e3", "sess_b", "claude", "srv2", "server-two", 100, 200, 10, 5, 1.0, "error", 500, t2),
		mk("e4", "sess_b", "gpt-4o", "srv1", "server-one", 3, 4, 0, 0, 0.1, "ok", 200, t3),
	}
	for _, e := range events {
		rec.Record(e)
	}

	// fold buckets by Key (as the portal layer does).
	type folded struct {
		count, errCount, in, out, cached, cw int
		energy                               float64
		first, last                          time.Time
		hosts                                map[string]bool
	}
	fold := func(bs []GroupBucket) map[string]*folded {
		m := map[string]*folded{}
		for _, b := range bs {
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
			f.cw += b.CacheWriteTokens
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

	sessBuckets, err := rec.UsageGroups(context.Background(), Query{UserID: "u1"}, "session")
	if err != nil {
		t.Fatalf("UsageGroups(session): %v", err)
	}
	if len(sessBuckets) != 3 {
		t.Fatalf("session buckets = %d, want 3: %+v", len(sessBuckets), sessBuckets)
	}
	sess := fold(sessBuckets)
	if a := sess["sess_a"]; a == nil || a.count != 2 || a.errCount != 0 || a.in != 15 || a.out != 27 || a.cached != 3 || a.cw != 1 || !approxEqual(a.energy, 0.75) || !a.first.Equal(t0) || !a.last.Equal(t1) {
		t.Fatalf("sess_a folded mismatch: %+v", a)
	}
	if b := sess["sess_b"]; b == nil || b.count != 2 || b.errCount != 1 || b.in != 103 || b.out != 204 || b.cached != 10 || b.cw != 5 || !approxEqual(b.energy, 1.1) || !b.first.Equal(t2) || !b.last.Equal(t3) || !b.hosts["srv1"] || !b.hosts["srv2"] {
		t.Fatalf("sess_b folded mismatch (cross-host): %+v", b)
	}

	srvBuckets, err := rec.UsageGroups(context.Background(), Query{UserID: "u1"}, "server")
	if err != nil {
		t.Fatalf("UsageGroups(server): %v", err)
	}
	hostBy := map[string]string{}
	for _, b := range srvBuckets {
		hostBy[b.Key] = b.Host
	}
	if hostBy["server-one"] != "srv1" || hostBy["server-two"] != "srv2" {
		t.Fatalf("server->host mismatch: %+v", hostBy)
	}

	// Exact filter composes.
	q := Query{UserID: "u1", SessionIDExact: "sess_a"}
	modelBuckets, err := rec.UsageGroups(context.Background(), q, "model")
	if err != nil {
		t.Fatalf("UsageGroups(model, sess_a): %v", err)
	}
	if len(modelBuckets) != 1 || modelBuckets[0].Key != "gpt-4o" || modelBuckets[0].Count != 2 {
		t.Fatalf("model buckets under sess_a mismatch: %+v", modelBuckets)
	}

	if _, err := rec.UsageGroups(context.Background(), Query{UserID: "u1"}, "bogus"); err == nil {
		t.Fatalf("UsageGroups(bogus) should error")
	}
}

func TestRecorderTokenFilter(t *testing.T) {
	rec := NewRecorder()
	rec.Record(Event{ID: "req_tok", UserID: "usr_1", TokenID: "tok_a", HTTPStatus: 200})
	rec.Record(Event{ID: "req_chat", UserID: "usr_1", TokenID: "", HTTPStatus: 200})

	if got := queryIDs(rec.Query(Query{UserID: "usr_1", HasTokenFilter: true, TokenID: "tok_a"})); len(got) != 1 || got[0] != "req_tok" {
		t.Fatalf("token=tok_a ids=%v, want [req_tok]", got)
	}
	if got := queryIDs(rec.Query(Query{UserID: "usr_1", HasTokenFilter: true, TokenID: ""})); len(got) != 1 || got[0] != "req_chat" {
		t.Fatalf("empty token ids=%v, want [req_chat]", got)
	}
	if got := queryIDs(rec.Query(Query{UserID: "usr_1"})); len(got) != 2 {
		t.Fatalf("no filter ids=%v, want 2", got)
	}
}

// TestRecorderUsageGroupsProjectDimension proves usageGroupKey's "project"
// whitelist entry maps to Event.ProjectID and folds correctly across hosts —
// the memory-store twin of TestConformanceUsageGroupsServiceDimension.
func TestRecorderUsageGroupsProjectDimension(t *testing.T) {
	rec := NewRecorder()
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	rec.Record(Event{ID: "e1", UserID: "u1", ProjectID: "proj_1", ProjectName: "Widgets", Host: "srv1", InputTokens: 10, OutputTokens: 20, CreatedAt: now})
	rec.Record(Event{ID: "e2", UserID: "u1", ProjectID: "proj_1", ProjectName: "Widgets", Host: "srv2", InputTokens: 3, OutputTokens: 4, CreatedAt: now.Add(time.Minute)})
	rec.Record(Event{ID: "e3", UserID: "u1", ProjectID: "proj_2", ProjectName: "Gadgets", Host: "srv1", InputTokens: 1, OutputTokens: 1, CreatedAt: now})
	rec.Record(Event{ID: "e4", UserID: "u1", Host: "srv1", InputTokens: 100, OutputTokens: 200, CreatedAt: now}) // no project

	buckets, err := rec.UsageGroups(context.Background(), Query{UserID: "u1"}, "project")
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
}

// TestRecorderProjectFilters proves ProjectIDExact/HasProjectIDExact (drill-
// down) and ProjectIDs (the applyUsageScope §8 IN-list) on the memory store,
// mirroring the SQL-store behavior exactly — including the security-critical
// case that a non-nil EMPTY ProjectIDs slice matches ZERO rows.
func TestRecorderProjectFilters(t *testing.T) {
	rec := NewRecorder()
	rec.Record(Event{ID: "req_a", UserID: "usr_1", ProjectID: "proj_a", HTTPStatus: 200})
	rec.Record(Event{ID: "req_b", UserID: "usr_1", ProjectID: "proj_b", HTTPStatus: 200})
	rec.Record(Event{ID: "req_c", UserID: "usr_1", ProjectID: "", HTTPStatus: 200})

	if got := queryIDs(rec.Query(Query{UserID: "usr_1", HasProjectIDExact: true, ProjectIDExact: "proj_a"})); len(got) != 1 || got[0] != "req_a" {
		t.Fatalf("ProjectIDExact=proj_a ids=%v, want [req_a]", got)
	}
	if got := queryIDs(rec.Query(Query{UserID: "usr_1", HasProjectIDExact: true, ProjectIDExact: ""})); len(got) != 1 || got[0] != "req_c" {
		t.Fatalf("HasProjectIDExact empty ids=%v, want [req_c]", got)
	}
	if got := queryIDs(rec.Query(Query{UserID: "usr_1", ProjectIDs: []string{"proj_a", "proj_b"}})); len(got) != 2 {
		t.Fatalf("ProjectIDs=[a,b] ids=%v, want 2", got)
	}
	// The security-critical guard: a non-nil EMPTY slice must match ZERO rows.
	if got := queryIDs(rec.Query(Query{UserID: "usr_1", ProjectIDs: []string{}})); len(got) != 0 {
		t.Fatalf("ProjectIDs=[] ids=%v, want 0 (member-of-nothing must see nothing)", got)
	}
	// nil ProjectIDs: no filter.
	if got := queryIDs(rec.Query(Query{UserID: "usr_1"})); len(got) != 3 {
		t.Fatalf("nil ProjectIDs ids=%v, want 3", got)
	}
}

func TestRecorderUpdateUsageEventEnergy(t *testing.T) {
	rec := NewRecorder()
	rec.Record(Event{ID: "req_1", UserID: "usr_1", CreatedAt: time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)})

	if err := rec.UpdateUsageEventEnergy(context.Background(), "req_1", 0.5, 0.2, "measured"); err != nil {
		t.Fatalf("update energy: %v", err)
	}
	events := rec.All()
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].EnergyWh != 0.5 || events[0].EnergyMarginalWh != 0.2 || events[0].EnergySource != "measured" {
		t.Fatalf("event energy = %+v, want 0.5/0.2/measured", events[0])
	}

	// A missing id is a benign no-op (mirrors a 0-row SQL UPDATE).
	if err := rec.UpdateUsageEventEnergy(context.Background(), "does-not-exist", 9, 9, "modeled"); err != nil {
		t.Fatalf("update energy (missing) = %v, want nil (benign no-op)", err)
	}
	if rec.All()[0].EnergyWh != 0.5 {
		t.Fatalf("missing-id update must not touch other rows")
	}
}

func TestRecorderUnpricedUsageEvents(t *testing.T) {
	rec := NewRecorder()
	base := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)
	rec.Record(Event{ID: "priced", CreatedAt: base, EnergySource: "measured"})
	rec.Record(Event{ID: "too_old", CreatedAt: base.Add(-2 * time.Hour), EnergySource: ""})
	rec.Record(Event{ID: "too_new", CreatedAt: base.Add(2 * time.Hour), EnergySource: ""})
	rec.Record(Event{ID: "unpriced_2", CreatedAt: base.Add(10 * time.Minute), EnergySource: ""})
	rec.Record(Event{ID: "unpriced_1", CreatedAt: base.Add(5 * time.Minute), EnergySource: ""})

	got, err := rec.UnpricedUsageEvents(context.Background(), base.Add(-time.Hour), base.Add(time.Hour), 10)
	if err != nil {
		t.Fatalf("unpriced events: %v", err)
	}
	if len(got) != 2 || got[0].ID != "unpriced_1" || got[1].ID != "unpriced_2" {
		t.Fatalf("ids = %v, want [unpriced_1 unpriced_2] (oldest-first, energy_source=\"\" only, in window)", idsOf(got))
	}

	// limit caps the result to the oldest N.
	limited, err := rec.UnpricedUsageEvents(context.Background(), base.Add(-time.Hour), base.Add(time.Hour), 1)
	if err != nil {
		t.Fatalf("unpriced events (limited): %v", err)
	}
	if len(limited) != 1 || limited[0].ID != "unpriced_1" {
		t.Fatalf("limited = %v, want [unpriced_1]", idsOf(limited))
	}

	// A non-positive limit falls back to defaultUnpricedEventsLimit rather than
	// returning zero rows.
	unbounded, err := rec.UnpricedUsageEvents(context.Background(), base.Add(-time.Hour), base.Add(time.Hour), 0)
	if err != nil {
		t.Fatalf("unpriced events (limit=0): %v", err)
	}
	if len(unbounded) != 2 {
		t.Fatalf("limit=0 ids = %v, want the 2 in-window rows (default cap, not zero)", idsOf(unbounded))
	}
}

func TestRecorderUsageEventsForServerWindow(t *testing.T) {
	rec := NewRecorder()
	// A 10s request ending at 10:00:10 (window [10:00:00,10:00:10]) on server "srv1".
	rec.Record(Event{ID: "req_a", Host: "srv1", LatencyMS: 10000, CreatedAt: time.Date(2026, 8, 6, 10, 0, 10, 0, time.UTC)})
	// Same window, but a DIFFERENT server — must be excluded.
	rec.Record(Event{ID: "req_other_server", Host: "srv2", LatencyMS: 10000, CreatedAt: time.Date(2026, 8, 6, 10, 0, 10, 0, time.UTC)})
	// Entirely before the query window on srv1 — must be excluded.
	rec.Record(Event{ID: "req_before", Host: "srv1", LatencyMS: 1000, CreatedAt: time.Date(2026, 8, 6, 9, 0, 1, 0, time.UTC)})
	// Entirely after the query window on srv1 — must be excluded.
	rec.Record(Event{ID: "req_after", Host: "srv1", LatencyMS: 1000, CreatedAt: time.Date(2026, 8, 6, 11, 0, 1, 0, time.UTC)})
	// Only partially overlaps the query window (starts before, ends inside) —
	// must be INCLUDED (overlap, not containment).
	rec.Record(Event{ID: "req_partial", Host: "srv1", LatencyMS: 20000, CreatedAt: time.Date(2026, 8, 6, 10, 0, 5, 0, time.UTC)})

	from := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)
	to := time.Date(2026, 8, 6, 10, 0, 20, 0, time.UTC)
	got, err := rec.UsageEventsForServerWindow(context.Background(), "srv1", from, to)
	if err != nil {
		t.Fatalf("events for server window: %v", err)
	}
	ids := idsOf(got)
	if len(ids) != 2 || !containsID(ids, "req_a") || !containsID(ids, "req_partial") {
		t.Fatalf("ids = %v, want [req_a req_partial]", ids)
	}
}

func idsOf(events []Event) []string {
	ids := make([]string, len(events))
	for i, e := range events {
		ids[i] = e.ID
	}
	return ids
}

func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
