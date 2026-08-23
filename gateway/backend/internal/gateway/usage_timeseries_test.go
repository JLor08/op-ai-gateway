// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/usage"
	"testing"
	"time"
)

func TestParseUsageTimeSeriesQueryDefaultsAndWhitelist(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)

	// Defaults: unknown window -> 5m, unknown bucket -> 5s, no scope -> own.
	q, bucket := parseUsageTimeSeriesQuery(httptest.NewRequest(http.MethodGet,
		"/api/portal/usage/timeseries?window=bogus&bucket=999", nil), now)
	if !q.From.Equal(now.Add(-5*time.Minute)) || !q.To.Equal(now) {
		t.Fatalf("default window From/To = %v/%v, want now-5m/now", q.From, q.To)
	}
	if bucket != 5 {
		t.Fatalf("default bucket = %d, want 5", bucket)
	}
	if q.ScopeAll {
		t.Fatalf("no scope must not set ScopeAll")
	}

	// Whitelisted window + bucket + scope. Covers the full window set incl. the
	// newly added coarse windows (30m..1y).
	cases := []struct {
		window string
		want   time.Duration
	}{
		{"5m", 5 * time.Minute},
		{"15m", 15 * time.Minute},
		{"30m", 30 * time.Minute},
		{"1h", time.Hour},
		{"6h", 6 * time.Hour},
		{"12h", 12 * time.Hour},
		{"1d", 24 * time.Hour},
		{"1w", 7 * 24 * time.Hour},
		{"2w", 14 * 24 * time.Hour},
		{"1mo", 30 * 24 * time.Hour},
		{"3mo", 90 * 24 * time.Hour},
		{"6mo", 180 * 24 * time.Hour},
		{"1y", 365 * 24 * time.Hour},
	}
	for _, c := range cases {
		q, _ := parseUsageTimeSeriesQuery(httptest.NewRequest(http.MethodGet,
			"/api/portal/usage/timeseries?window="+c.window+"&scope=all", nil), now)
		if !q.From.Equal(now.Add(-c.want)) {
			t.Fatalf("window=%s From = %v, want now-%v", c.window, q.From, c.want)
		}
		if !q.To.Equal(now) {
			t.Fatalf("window=%s To = %v, want now", c.window, q.To)
		}
		if !q.ScopeAll {
			t.Fatalf("scope=all must set the ScopeAll intent for window=%s", c.window)
		}
	}
	// Whitelisted bucket set incl. the newly added coarse buckets (180..2592000).
	for _, b := range []struct {
		raw  string
		want int
	}{
		{"1", 1},
		{"5", 5},
		{"10", 10},
		{"30", 30},
		{"60", 60},
		{"180", 180},
		{"900", 900},
		{"3600", 3600},
		{"21600", 21600},
		{"43200", 43200},
		{"86400", 86400},
		{"604800", 604800},
		{"1209600", 1209600},
		{"2592000", 2592000},
	} {
		_, got := parseUsageTimeSeriesQuery(httptest.NewRequest(http.MethodGet,
			"/api/portal/usage/timeseries?bucket="+b.raw, nil), now)
		if got != b.want {
			t.Fatalf("bucket=%s -> %d, want %d", b.raw, got, b.want)
		}
	}
	// A syntactically valid integer that is NOT in the allowed set falls back to
	// the default (5), same as a missing/non-integer value.
	for _, raw := range []string{"7", "0", "-5", "2", "1000000000000"} {
		_, got := parseUsageTimeSeriesQuery(httptest.NewRequest(http.MethodGet,
			"/api/portal/usage/timeseries?bucket="+raw, nil), now)
		if got != 5 {
			t.Fatalf("out-of-set bucket=%s -> %d, want default 5", raw, got)
		}
	}
}

func TestParseUsageTimeSeriesQueryServer(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	q, _ := parseUsageTimeSeriesQuery(httptest.NewRequest(http.MethodGet,
		"/api/portal/usage/timeseries?server=host_vllm", nil), now)
	if q.Server != "host_vllm" {
		t.Fatalf("q.Server = %q, want host_vllm", q.Server)
	}
}

func TestPortalUsageTimeSeriesDTOShape(t *testing.T) {
	srv := NewTestServer()
	now := time.Now().UTC()
	srv.Usage.Record(usage.Event{ID: "a", UserID: "usr_dev", HTTPStatus: 200, InputTokens: 50, OutputTokens: 25, CreatedAt: now.Add(-2 * time.Second)})

	req := httptest.NewRequest(http.MethodGet, "/api/portal/usage/timeseries?window=5m&bucket=5", nil)
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var ts usage.TimeSeries
	if err := json.Unmarshal(rec.Body.Bytes(), &ts); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, rec.Body.String())
	}
	if ts.Points == nil {
		t.Fatalf("points must be non-null: %s", rec.Body.String())
	}
	if ts.BucketSeconds != 5 {
		t.Fatalf("bucket_seconds = %d, want 5", ts.BucketSeconds)
	}
	// window 5m / bucket 5s -> 60 buckets; the event contributes one connection.
	total := 0
	for _, p := range ts.Points {
		total += p.Connections
	}
	if total != 1 {
		t.Fatalf("total connections = %d, want 1", total)
	}
}

func TestPortalUsageTimeSeriesRejectsNonGet(t *testing.T) {
	srv := NewTestServer()
	req := httptest.NewRequest(http.MethodPost, "/api/portal/usage/timeseries", nil)
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405", rec.Code)
	}
}

func TestPortalUsageTimeSeriesScopeAllForAdmin(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_adm", "adm@example.test", "password-1", "admin")
	now := time.Now().UTC()
	srv.Usage.Record(usage.Event{ID: "mine", UserID: "usr_adm", HTTPStatus: 200, CreatedAt: now.Add(-2 * time.Second)})
	srv.Usage.Record(usage.Event{ID: "theirs", UserID: "usr_other", HTTPStatus: 200, CreatedAt: now.Add(-2 * time.Second)})
	cookie := loginCookie(t, srv, "adm@example.test", "password-1")

	req := httptest.NewRequest(http.MethodGet, "/api/portal/usage/timeseries?window=5m&bucket=5&scope=all", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	var ts usage.TimeSeries
	if err := json.Unmarshal(rec.Body.Bytes(), &ts); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, rec.Body.String())
	}
	total := 0
	for _, p := range ts.Points {
		total += p.Connections
	}
	if total != 2 {
		t.Fatalf("admin scope=all total connections = %d, want 2", total)
	}
}

func TestPortalUsageTimeSeriesScopeAllPinnedForNonAdmin(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_plain", "plain@example.test", "password-1", "user")
	now := time.Now().UTC()
	srv.Usage.Record(usage.Event{ID: "mine", UserID: "usr_plain", HTTPStatus: 200, CreatedAt: now.Add(-2 * time.Second)})
	srv.Usage.Record(usage.Event{ID: "theirs", UserID: "usr_other", HTTPStatus: 200, CreatedAt: now.Add(-2 * time.Second)})
	cookie := loginCookie(t, srv, "plain@example.test", "password-1")

	req := httptest.NewRequest(http.MethodGet, "/api/portal/usage/timeseries?window=5m&bucket=5&scope=all", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	var ts usage.TimeSeries
	if err := json.Unmarshal(rec.Body.Bytes(), &ts); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, rec.Body.String())
	}
	total := 0
	for _, p := range ts.Points {
		total += p.Connections
	}
	if total != 1 {
		t.Fatalf("non-admin scope=all total connections = %d, want 1 (pinned to own)", total)
	}
}
