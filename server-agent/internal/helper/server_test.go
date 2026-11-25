// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package helper

import (
	"io"
	"net/http"
	"net/http/httptest"
	"op-ai-server-agent/internal/collector"
	"op-ai-server-agent/internal/sample"
	"strings"
	"testing"
	"time"
)

func get(h http.Handler, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func TestHelperRAPLCache(t *testing.T) {
	now := time.Unix(1000, 0)
	calls := 0
	h := newHandler(func() []collector.HelperRAPLDomain {
		calls++
		return []collector.HelperRAPLDomain{{Zone: "intel-rapl:0", Name: "package-0", EnergyUJ: uint64(calls)}}
	}, nil, func() time.Time { return now }, nil)
	for range 5 {
		now = now.Add(10 * time.Millisecond)
		if rec := get(h, "GET", collector.HelperRAPLPath); rec.Code != 200 {
			t.Fatalf("status %d", rec.Code)
		}
	}
	if calls != 1 {
		t.Fatalf("rapl called %d times within the window, want 1", calls)
	}
	now = now.Add(251 * time.Millisecond)
	rec := get(h, "GET", collector.HelperRAPLPath)
	if calls != 2 {
		t.Fatalf("rapl called %d times after the window, want 2", calls)
	}
	if rec.Header().Get("Cache-Control") != "no-store" || !strings.Contains(rec.Body.String(), "package-0") {
		t.Fatalf("headers %v body %s", rec.Header(), rec.Body)
	}
}

func TestHelperMemoryModulesEndpoint(t *testing.T) {
	now := func() time.Time { return time.Unix(1, 0) }
	empty := newHandler(func() []collector.HelperRAPLDomain { return nil }, nil, now, nil)
	if b, _ := io.ReadAll(get(empty, "GET", collector.HelperMemoryModulesPath).Body); strings.TrimSpace(string(b)) != `{"modules":[]}` {
		t.Fatalf("empty body %s", b)
	}
	h := newHandler(func() []collector.HelperRAPLDomain { return nil }, []sample.MemoryModule{{}}, now, nil)
	rec := get(h, "GET", collector.HelperMemoryModulesPath)
	if rec.Code != 200 || !strings.HasPrefix(rec.Body.String(), `{"modules":[{`) {
		t.Fatalf("code %d body %s", rec.Code, rec.Body)
	}
}

func TestHelperMethodAndPath(t *testing.T) {
	h := newHandler(func() []collector.HelperRAPLDomain { return nil }, nil, time.Now, nil)
	rec := get(h, "POST", collector.HelperRAPLPath)
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET" {
		t.Fatalf("POST: %d %v", rec.Code, rec.Header())
	}
	if rec := get(h, "GET", "/v1/energy"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown path: %d", rec.Code)
	}
}

func TestHelperRateLimit(t *testing.T) {
	now := time.Unix(1, 0)
	h := newHandler(func() []collector.HelperRAPLDomain { return nil }, nil, func() time.Time { return now }, nil)
	limited := 0
	for range 41 {
		if get(h, "GET", collector.HelperRAPLPath).Code == http.StatusTooManyRequests {
			limited++
		}
	}
	if limited == 0 {
		t.Fatal("no request was rate limited")
	}
	now = now.Add(time.Second)
	if rec := get(h, "GET", collector.HelperRAPLPath); rec.Code != 200 {
		t.Fatalf("bucket did not refill: %d", rec.Code)
	}
}
