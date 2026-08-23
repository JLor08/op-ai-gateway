// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/usage"
	"strings"
	"testing"
	"time"
)

func TestRecordUsagePublishesActivity(t *testing.T) {
	srv := NewTestServer()
	ch := srv.UsageEvents.Register()
	defer srv.UsageEvents.Unregister(ch)

	req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions",
		strings.NewReader(`{"model":"qwen-coder","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("completion status = %d, body = %s", rec.Code, rec.Body.String())
	}

	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("recordUsage did not publish an activity signal")
	}
}

func TestParseUsageQueryClampsAndMapsRange(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	req := httptest.NewRequest(http.MethodGet,
		"/api/portal/usage?page=0&limit=999&sort=bogus&order=sideways&status=weird&range=7d&scope=all&model=qwen&server=srv1&q=hello",
		nil)

	q := parseUsageQuery(req, now)

	if q.Page != 1 {
		t.Fatalf("page = %d, want 1 (invalid clamped)", q.Page)
	}
	if q.Limit != 25 {
		t.Fatalf("limit = %d, want 25 (invalid -> default)", q.Limit)
	}
	if q.Sort != "created_at" {
		t.Fatalf("sort = %q, want created_at (not in whitelist -> default)", q.Sort)
	}
	if q.Order != "desc" {
		t.Fatalf("order = %q, want desc (invalid -> default)", q.Order)
	}
	if q.Status != "" {
		t.Fatalf("status = %q, want empty (invalid dropped)", q.Status)
	}
	if !q.From.Equal(now.Add(-7 * 24 * time.Hour)) {
		t.Fatalf("from = %v, want now-7d", q.From)
	}
	if !q.To.IsZero() {
		t.Fatalf("to = %v, want zero (open end)", q.To)
	}
	if !q.ScopeAll {
		t.Fatalf("scope=all must set the ScopeAll intent")
	}
	if q.Model != "qwen" || q.Server != "srv1" || q.Q != "hello" {
		t.Fatalf("filters = %q/%q/%q", q.Model, q.Server, q.Q)
	}
}

func TestParseUsageQueryReadsTokenAndUserFilter(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)

	// Real token id present -> HasTokenFilter true, TokenID = the value.
	q := parseUsageQuery(httptest.NewRequest(http.MethodGet,
		"/api/portal/usage?token_id=tok_42&user_id=usr_9", nil), now)
	if !q.HasTokenFilter || q.TokenID != "tok_42" {
		t.Fatalf("token filter = %+v, want HasTokenFilter=true TokenID=tok_42", q)
	}
	if q.FilterUserID != "usr_9" {
		t.Fatalf("FilterUserID = %q, want usr_9", q.FilterUserID)
	}

	// The chat/no-token sentinel -> HasTokenFilter true, TokenID "".
	none := parseUsageQuery(httptest.NewRequest(http.MethodGet,
		"/api/portal/usage?token_id="+NoTokenWire, nil), now)
	if !none.HasTokenFilter || none.TokenID != "" {
		t.Fatalf("__none__ = %+v, want HasTokenFilter=true TokenID=''", none)
	}

	// Absent token_id -> no token filter at all.
	absent := parseUsageQuery(httptest.NewRequest(http.MethodGet, "/api/portal/usage", nil), now)
	if absent.HasTokenFilter {
		t.Fatalf("absent token_id must leave HasTokenFilter false, got %+v", absent)
	}

	// Time-series parser reads the same two params.
	ts, _ := parseUsageTimeSeriesQuery(httptest.NewRequest(http.MethodGet,
		"/api/portal/usage/timeseries?token_id="+NoTokenWire+"&user_id=usr_9", nil), now)
	if !ts.HasTokenFilter || ts.TokenID != "" || ts.FilterUserID != "usr_9" {
		t.Fatalf("timeseries parse = %+v", ts)
	}
}

func TestParseUsageQueryExactPresenceFlags(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)

	// The __empty__ sentinel -> presence flag true, value "" (empty-key expansion).
	empty := parseUsageQuery(httptest.NewRequest(http.MethodGet,
		"/api/portal/usage?session_id_exact=__empty__", nil), now)
	if !empty.HasSessionIDExact || empty.SessionIDExact != "" {
		t.Fatalf("__empty__ = %+v, want HasSessionIDExact=true SessionIDExact=''", empty)
	}

	// A normal value -> presence flag true, value carried (trimmed).
	val := parseUsageQuery(httptest.NewRequest(http.MethodGet,
		"/api/portal/usage?session_id_exact=+foo+", nil), now)
	if !val.HasSessionIDExact || val.SessionIDExact != "foo" {
		t.Fatalf("value = %+v, want HasSessionIDExact=true SessionIDExact='foo'", val)
	}

	// Absent -> no presence flag, empty value (no filter).
	absent := parseUsageQuery(httptest.NewRequest(http.MethodGet, "/api/portal/usage", nil), now)
	if absent.HasSessionIDExact || absent.SessionIDExact != "" {
		t.Fatalf("absent = %+v, want HasSessionIDExact=false SessionIDExact=''", absent)
	}

	// Server + model share the same sentinel/flag handling.
	sm := parseUsageQuery(httptest.NewRequest(http.MethodGet,
		"/api/portal/usage?server_exact=__empty__&model_exact=gpt", nil), now)
	if !sm.HasServerExact || sm.ServerExact != "" {
		t.Fatalf("server __empty__ = %+v, want HasServerExact=true ServerExact=''", sm)
	}
	if !sm.HasModelExact || sm.ModelExact != "gpt" {
		t.Fatalf("model value = %+v, want HasModelExact=true ModelExact='gpt'", sm)
	}
}

func TestParseUsageQueryRangeAllOpensWindowAndDefaults(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)

	all := parseUsageQuery(httptest.NewRequest(http.MethodGet,
		"/api/portal/usage?range=all&limit=50&sort=latency_ms&order=asc&status=error", nil), now)
	if !all.From.IsZero() {
		t.Fatalf("range=all From = %v, want zero (no window)", all.From)
	}
	if all.Limit != 50 || all.Sort != "latency_ms" || all.Order != "asc" || all.Status != "error" {
		t.Fatalf("valid values not honored: %#v", all)
	}

	def := parseUsageQuery(httptest.NewRequest(http.MethodGet, "/api/portal/usage", nil), now)
	if !def.From.Equal(now.Add(-30 * 24 * time.Hour)) {
		t.Fatalf("default range From = %v, want now-30d", def.From)
	}
	if def.Page != 1 || def.Limit != 25 || def.Sort != "created_at" || def.Order != "desc" {
		t.Fatalf("defaults = %#v", def)
	}
	if def.ScopeAll {
		t.Fatalf("missing scope must not set ScopeAll")
	}
}

func TestParseUsageQueryParsesOwnerTimeAndNumericFilters(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	req := httptest.NewRequest(http.MethodGet,
		"/api/portal/usage?range=all&owner=+Ada+"+
			"&time_from=2026-07-16T23:04:05Z"+ // RFC3339
			"&time_to=2026-07-17T08:30"+ // datetime-local short form (no seconds)
			"&total_tokens_min=10&total_tokens_max=100"+
			"&latency_ms_max=250"+
			"&prompt_per_second_min=1.5"+
			"&input_tokens_min=not-a-number"+ // junk: ignored
			"&output_tokens_min=", // empty: ignored
		nil)

	q := parseUsageQuery(req, now)

	if q.Owner != "Ada" {
		t.Fatalf("owner = %q, want trimmed \"Ada\"", q.Owner)
	}
	if !q.TimeFrom.Equal(time.Date(2026, 7, 16, 23, 4, 5, 0, time.UTC)) {
		t.Fatalf("time_from = %v, want 2026-07-16T23:04:05Z (RFC3339)", q.TimeFrom)
	}
	if !q.TimeTo.Equal(time.Date(2026, 7, 17, 8, 30, 0, 0, time.UTC)) {
		t.Fatalf("time_to = %v, want 2026-07-17T08:30 parsed in UTC (short form)", q.TimeTo)
	}
	if got := q.NumericMin["total_tokens"]; got != 10 {
		t.Fatalf("total_tokens_min = %v, want 10", got)
	}
	if got := q.NumericMax["total_tokens"]; got != 100 {
		t.Fatalf("total_tokens_max = %v, want 100", got)
	}
	if got := q.NumericMax["latency_ms"]; got != 250 {
		t.Fatalf("latency_ms_max = %v, want 250", got)
	}
	if got := q.NumericMin["prompt_per_second"]; got != 1.5 {
		t.Fatalf("prompt_per_second_min = %v, want 1.5", got)
	}
	if _, ok := q.NumericMin["input_tokens"]; ok {
		t.Fatalf("input_tokens_min was junk and must be ignored, got %v", q.NumericMin["input_tokens"])
	}
	if _, ok := q.NumericMin["output_tokens"]; ok {
		t.Fatalf("empty output_tokens_min must be ignored, got %v", q.NumericMin["output_tokens"])
	}
	// latency_ms had no _min -> the min map must not carry it.
	if _, ok := q.NumericMin["latency_ms"]; ok {
		t.Fatalf("latency_ms_min absent, must not be in NumericMin")
	}
}

func TestParseUsageQueryParsesTextStreamAndCachedTokensFilters(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	req := httptest.NewRequest(http.MethodGet,
		"/api/portal/usage?range=all"+
			"&req_path=+%2Fv1%2Fchat+"+ // "/v1/chat" padded with spaces -> trimmed
			"&content_type=text%2Fevent-stream"+
			"&provider_model=qwen"+
			"&provider_path=%2Fv1%2Fchat%2Fcompletions"+
			"&cached_tokens_min=5&cached_tokens_max=50"+
			"&stream=true",
		nil)

	q := parseUsageQuery(req, now)

	if q.ReqPath != "/v1/chat" {
		t.Fatalf("req_path = %q, want trimmed \"/v1/chat\"", q.ReqPath)
	}
	if q.ContentType != "text/event-stream" {
		t.Fatalf("content_type = %q, want text/event-stream", q.ContentType)
	}
	if q.ProviderModel != "qwen" {
		t.Fatalf("provider_model = %q, want qwen", q.ProviderModel)
	}
	if q.ProviderPath != "/v1/chat/completions" {
		t.Fatalf("provider_path = %q, want /v1/chat/completions", q.ProviderPath)
	}
	if got := q.NumericMin["cached_tokens"]; got != 5 {
		t.Fatalf("cached_tokens_min = %v, want 5", got)
	}
	if got := q.NumericMax["cached_tokens"]; got != 50 {
		t.Fatalf("cached_tokens_max = %v, want 50", got)
	}
	if q.Stream != "true" {
		t.Fatalf("stream = %q, want \"true\"", q.Stream)
	}
}

func TestParseUsageQueryStreamTriState(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	mk := func(raw string) usage.Query {
		return parseUsageQuery(httptest.NewRequest(http.MethodGet, "/api/portal/usage?range=all"+raw, nil), now)
	}
	if q := mk("&stream=true"); q.Stream != "true" {
		t.Fatalf("stream=true -> %q, want \"true\"", q.Stream)
	}
	if q := mk("&stream=false"); q.Stream != "false" {
		t.Fatalf("stream=false -> %q, want \"false\"", q.Stream)
	}
	if q := mk("&stream=maybe"); q.Stream != "" {
		t.Fatalf("stream=maybe -> %q, want \"\" (junk ignored)", q.Stream)
	}
	if q := mk(""); q.Stream != "" {
		t.Fatalf("absent stream -> %q, want \"\" (no filter)", q.Stream)
	}
}

func TestParseUsageQueryTimeSecondsShortFormAndJunkIgnored(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	req := httptest.NewRequest(http.MethodGet,
		"/api/portal/usage?range=all&time_from=2026-07-16T23:04:05&time_to=garbage", nil)

	q := parseUsageQuery(req, now)

	if !q.TimeFrom.Equal(time.Date(2026, 7, 16, 23, 4, 5, 0, time.UTC)) {
		t.Fatalf("time_from = %v, want 2026-07-16T23:04:05 parsed in UTC (short form with seconds)", q.TimeFrom)
	}
	if !q.TimeTo.IsZero() {
		t.Fatalf("time_to = %v, want zero (unparseable ignored)", q.TimeTo)
	}
	if q.Owner != "" {
		t.Fatalf("owner = %q, want empty (absent)", q.Owner)
	}
	if q.NumericMin != nil || q.NumericMax != nil {
		t.Fatalf("no numeric params -> maps stay nil, got min=%v max=%v", q.NumericMin, q.NumericMax)
	}
}

func loginCookie(t *testing.T, srv *Server, email, password string) *http.Cookie {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login",
		strings.NewReader(`{"email":"`+email+`","password":"`+password+`"}`))
	req.Header.Set(csrfHeaderName, "1")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login status = %d, body = %s", rec.Code, rec.Body.String())
	}
	return sessionCookie(t, rec.Result())
}

func TestPortalUsageReturnsPagedEnvelope(t *testing.T) {
	srv := NewTestServer()
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		srv.Usage.Record(usage.Event{ID: fmt.Sprintf("req_%d", i), UserID: "usr_dev", TokenID: "tok_dev", Model: "qwen-coder", Status: "success", HTTPStatus: 200, CreatedAt: now})
	}
	req := httptest.NewRequest(http.MethodGet, "/api/portal/usage?range=all&limit=25&page=1", nil)
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var page usage.Page
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if page.Total != 3 || page.Page != 1 || page.Limit != 25 || len(page.Data) != 3 {
		t.Fatalf("page = %#v", page)
	}
}

func TestPortalUsageStatusFilterUsesErrorPredicate(t *testing.T) {
	srv := NewTestServer()
	now := time.Now().UTC()
	srv.Usage.Record(usage.Event{ID: "ok", UserID: "usr_dev", Status: "success", HTTPStatus: 200, CreatedAt: now})
	srv.Usage.Record(usage.Event{ID: "streamfail", UserID: "usr_dev", Status: "error", HTTPStatus: 200, CreatedAt: now})
	req := httptest.NewRequest(http.MethodGet, "/api/portal/usage?range=all&status=error", nil)
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)
	var page usage.Page
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, rec.Body.String())
	}
	if page.Total != 1 || len(page.Data) != 1 || page.Data[0].ID != "streamfail" {
		t.Fatalf("status=error must match status==error even with http_status 200: %#v", page)
	}
}

func TestPortalUsageStatsReturnsTotals(t *testing.T) {
	srv := NewTestServer()
	now := time.Now().UTC()
	srv.Usage.Record(usage.Event{ID: "a", UserID: "usr_dev", Status: "success", HTTPStatus: 200, InputTokens: 3, OutputTokens: 7, TotalTokens: 10, CreatedAt: now})
	srv.Usage.Record(usage.Event{ID: "b", UserID: "usr_dev", Status: "error", HTTPStatus: 500, CreatedAt: now})
	req := httptest.NewRequest(http.MethodGet, "/api/portal/usage/stats?range=all", nil)
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var stats usage.Stats
	if err := json.Unmarshal(rec.Body.Bytes(), &stats); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if stats.Totals.TotalRequests != 2 || stats.Totals.ErrorCount != 1 || stats.Totals.InputTokens != 3 || stats.Totals.OutputTokens != 7 {
		t.Fatalf("totals = %#v", stats.Totals)
	}
}

func TestPortalUsageScopeAllForSystemAdmin(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_sys", "sys@example.test", "password-1", "system_admin")
	now := time.Now().UTC()
	srv.Usage.Record(usage.Event{ID: "mine", UserID: "usr_sys", Status: "success", HTTPStatus: 200, CreatedAt: now})
	srv.Usage.Record(usage.Event{ID: "theirs", UserID: "usr_other", Status: "success", HTTPStatus: 200, CreatedAt: now})
	cookie := loginCookie(t, srv, "sys@example.test", "password-1")

	req := httptest.NewRequest(http.MethodGet, "/api/portal/usage?range=all&scope=all", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)
	var page usage.Page
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, rec.Body.String())
	}
	if page.Total != 2 {
		t.Fatalf("system admin scope=all total = %d, want 2 (%#v)", page.Total, page)
	}
}

func TestPortalUsageScopeAllForPlainAdmin(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_adm", "adm@example.test", "password-1", "admin")
	now := time.Now().UTC()
	srv.Usage.Record(usage.Event{ID: "mine", UserID: "usr_adm", Status: "success", HTTPStatus: 200, CreatedAt: now})
	srv.Usage.Record(usage.Event{ID: "theirs", UserID: "usr_other", Status: "success", HTTPStatus: 200, CreatedAt: now})
	cookie := loginCookie(t, srv, "adm@example.test", "password-1")

	req := httptest.NewRequest(http.MethodGet, "/api/portal/usage?range=all&scope=all", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)
	var page usage.Page
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, rec.Body.String())
	}
	if page.Total != 2 {
		t.Fatalf("plain admin scope=all total = %d, want 2 (%#v)", page.Total, page)
	}
}

func TestPortalUsageEventsEmitsActivityFrame(t *testing.T) {
	srv := NewTestServer()
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/api/portal/usage/events", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() { srv.ServeHTTP(rec, req); close(done) }()

	time.Sleep(50 * time.Millisecond) // let the handler subscribe
	srv.UsageEvents.Publish()
	time.Sleep(50 * time.Millisecond) // let the frame get written
	cancel()
	<-done

	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
	if !strings.Contains(rec.Body.String(), "event: activity\ndata: {}\n\n") {
		t.Fatalf("missing activity frame, body = %q", rec.Body.String())
	}
}

// Over a real connection (where SetWriteDeadline is honored) an activity frame published
// AFTER the server's WriteTimeout window must still reach the client — only possible
// because the handler cleared the connection write deadline.
func TestPortalUsageEventsSurvivesServerWriteTimeout(t *testing.T) {
	srv := NewTestServer()
	httpSrv := &http.Server{Handler: srv, WriteTimeout: 150 * time.Millisecond}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() { _ = httpSrv.Serve(ln) }()
	defer httpSrv.Close()

	req, _ := http.NewRequest(http.MethodGet, "http://"+ln.Addr().String()+"/api/portal/usage/events", nil)
	req.Header.Set("Authorization", "Bearer dev-secret")
	resp, err := (&http.Client{}).Do(req) // no client Timeout: it would abort the long read
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}

	frames := make(chan string, 1)
	go func() {
		buf := make([]byte, 512)
		var acc strings.Builder
		for {
			n, rerr := resp.Body.Read(buf)
			if n > 0 {
				acc.Write(buf[:n])
				if strings.Contains(acc.String(), "event: activity") {
					frames <- acc.String()
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	// Wait past the 150ms WriteTimeout, THEN publish. Without the deadline clear the
	// connection is already dead and the frame never arrives.
	time.Sleep(300 * time.Millisecond)
	srv.UsageEvents.Publish()

	select {
	case f := <-frames:
		if !strings.Contains(f, "event: activity") {
			t.Fatalf("unexpected frame: %q", f)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("activity frame did not arrive after the WriteTimeout window (write deadline not cleared?)")
	}
}

// errUsagePortalFailure is the error errUsagePortal's Usage/UsageStats/
// UsageTimeSeries all return, simulating a genuine usage-store failure (ST-2).
var errUsagePortalFailure = errors.New("usage store unavailable")

// errUsagePortal is a portal.API stub (embeds a nil interface; only the
// overridden methods are ever called) whose Usage/UsageStats/UsageTimeSeries
// always fail. It exercises the ST-2 500-mapping: before ST-2 these methods
// had no error return at all, so a genuine store failure was indistinguishable
// from "no matching rows" and silently rendered as an empty 200 — the gateway
// handlers must now surface it as a 500 instead.
type errUsagePortal struct {
	portal.API
}

func (errUsagePortal) Usage(auth.Token, usage.Query) (usage.Page, error) {
	return usage.Page{}, errUsagePortalFailure
}

func (errUsagePortal) UsageStats(auth.Token, usage.Query) (usage.Stats, error) {
	return usage.Stats{}, errUsagePortalFailure
}

func (errUsagePortal) UsageTimeSeries(auth.Token, usage.Query, int) (usage.TimeSeries, error) {
	return usage.TimeSeries{}, errUsagePortalFailure
}

// TestPortalUsageEndpointsMap500OnStoreError proves a real Usage/UsageStats/
// UsageTimeSeries store error surfaces to the client as HTTP 500, never a
// silent empty 200 (the exact regression ST-2 fixes: a DB incident during
// this window must be visibly distinguishable from "genuinely no traffic").
func TestPortalUsageEndpointsMap500OnStoreError(t *testing.T) {
	srv := NewTestServer()
	srv.Portal = errUsagePortal{}

	for _, tc := range []struct {
		name     string
		path     string
		wantCode string
	}{
		{"usage", "/api/portal/usage?range=all", "usage.query_failed"},
		{"usage_stats", "/api/portal/usage/stats?range=all", "usage.stats_failed"},
		{"usage_timeseries", "/api/portal/usage/timeseries?window=5m", "usage.timeseries_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.Header.Set("Authorization", "Bearer dev-secret")
			rec := httptest.NewRecorder()

			srv.ServeHTTP(rec, req)

			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("%s status = %d, body = %s, want 500", tc.name, rec.Code, rec.Body.String())
			}
			var body struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("%s: unmarshal: %v (%s)", tc.name, err, rec.Body.String())
			}
			if body.Error.Code != tc.wantCode {
				t.Fatalf("%s: error code = %q, want %q", tc.name, body.Error.Code, tc.wantCode)
			}
		})
	}
}
