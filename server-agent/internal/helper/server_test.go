// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package helper

import (
	"encoding/json"
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

func monoZero() uint64 { return 0 }

func TestHelperRAPLCache(t *testing.T) {
	start := time.Unix(1000, 0)
	now := start
	calls := 0
	h := newHandler(func() []collector.HelperRAPLDomain {
		calls++
		return []collector.HelperRAPLDomain{{Zone: "intel-rapl:0", Name: "package-0", EnergyUJ: uint64(calls)}}
	}, nil, func() time.Time { return now }, monoZero, nil)
	read := func(offset time.Duration) *httptest.ResponseRecorder {
		now = start.Add(offset)
		rec := get(h, "GET", collector.HelperRAPLPath)
		if rec.Code != 200 {
			t.Fatalf("status %d", rec.Code)
		}
		return rec
	}
	read(0)
	for _, off := range []time.Duration{10, 100, 200, 250} {
		read(off * time.Millisecond)
	}
	if calls != 1 {
		t.Fatalf("rapl called %d times up to +250ms, want 1", calls)
	}
	rec := read(251 * time.Millisecond)
	if calls != 2 {
		t.Fatalf("rapl called %d times at +251ms, want 2", calls)
	}
	if rec.Header().Get("Cache-Control") != "no-store" || !strings.Contains(rec.Body.String(), "package-0") {
		t.Fatalf("headers %v body %s", rec.Header(), rec.Body)
	}
}

// TestHelperRAPLReadNS: every cached response repeats the read_ns of the refresh
// that produced it; a refresh stamps a new one from the monotonic clock.
func TestHelperRAPLReadNS(t *testing.T) {
	start := time.Unix(1000, 0)
	now := start
	mono := uint64(5_000_000_000)
	h := newHandler(func() []collector.HelperRAPLDomain {
		return []collector.HelperRAPLDomain{{Zone: "intel-rapl:0", Name: "package-0"}}
	}, nil, func() time.Time { return now }, func() uint64 { return mono }, nil)
	readNS := func(offset time.Duration) uint64 {
		t.Helper()
		now = start.Add(offset)
		mono += 1_000_000 // the monotonic clock moves on between requests
		rec := get(h, "GET", collector.HelperRAPLPath)
		var body collector.HelperRAPL
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != 200 {
			t.Fatalf("code %d body %s err %v", rec.Code, rec.Body, err)
		}
		if !strings.HasPrefix(rec.Body.String(), `{"read_ns":`) {
			t.Fatalf("read_ns not on the top level first: %s", rec.Body)
		}
		return body.ReadNS
	}
	first := readNS(0)
	if first != 5_001_000_000 {
		t.Fatalf("first read_ns = %d, want 5001000000", first)
	}
	for _, off := range []time.Duration{100, 250} {
		if got := readNS(off * time.Millisecond); got != first {
			t.Fatalf("cached read_ns at +%dms = %d, want %d", off, got, first)
		}
	}
	if got := readNS(251 * time.Millisecond); got != 5_004_000_000 {
		t.Fatalf("refreshed read_ns = %d, want 5004000000", got)
	}
}

func TestHelperMemoryModulesEndpoint(t *testing.T) {
	now := func() time.Time { return time.Unix(1, 0) }
	empty := newHandler(func() []collector.HelperRAPLDomain { return nil }, nil, now, monoZero, nil)
	if b, _ := io.ReadAll(get(empty, "GET", collector.HelperMemoryModulesPath).Body); strings.TrimSpace(string(b)) != `{"modules":[]}` {
		t.Fatalf("empty body %s", b)
	}
	h := newHandler(func() []collector.HelperRAPLDomain { return nil }, []sample.MemoryModule{{}}, now, monoZero, nil)
	rec := get(h, "GET", collector.HelperMemoryModulesPath)
	if rec.Code != 200 || !strings.HasPrefix(rec.Body.String(), `{"modules":[{`) {
		t.Fatalf("code %d body %s", rec.Code, rec.Body)
	}
}

func TestHelperMethodAndPath(t *testing.T) {
	h := newHandler(func() []collector.HelperRAPLDomain { return nil }, nil, time.Now, monoZero, nil)
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
	h := newHandler(func() []collector.HelperRAPLDomain { return nil }, nil, func() time.Time { return now }, monoZero, nil)
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
