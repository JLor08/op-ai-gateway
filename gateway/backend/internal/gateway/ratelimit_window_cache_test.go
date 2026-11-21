// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"net/http"
	"op-ai-gateway/internal/auth"
	"strconv"
	"testing"
	"time"
)

const rlp = "anthropic-ratelimit-unified-"

func windowHeaders(now time.Time, fiveHourOffset, sevenDayOffset time.Duration) http.Header {
	h := http.Header{}
	h.Set(rlp+"status", "allowed")
	h.Set(rlp+"reset", strconv.FormatInt(now.Add(sevenDayOffset).Unix(), 10))
	h.Set(rlp+"representative-claim", "five_hour")
	h.Set(rlp+"5h-utilization", "0.42")
	h.Set(rlp+"5h-reset", strconv.FormatInt(now.Add(fiveHourOffset).Unix(), 10))
	h.Set(rlp+"5h-status", "allowed")
	h.Set(rlp+"7d-utilization", "0.10")
	h.Set(rlp+"7d-reset", strconv.FormatInt(now.Add(sevenDayOffset).Unix(), 10))
	h.Set(rlp+"7d-status", "allowed")
	h.Set(rlp+"7d-surpassed-threshold", "0.1")
	return h
}

func TestWindowCacheAttachCopiesFutureWindows(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	c := newRateLimitWindowCache(10)
	c.Observe("user:a", windowHeaders(now, time.Hour, 24*time.Hour), now)

	dst := http.Header{}
	if !c.Attach("user:a", dst, now) {
		t.Fatal("Attach = false, want true")
	}
	want := map[string]string{
		rlp + "5h-utilization":         "0.42",
		rlp + "5h-reset":               strconv.FormatInt(now.Add(time.Hour).Unix(), 10),
		rlp + "5h-status":              "allowed",
		rlp + "7d-utilization":         "0.10",
		rlp + "7d-reset":               strconv.FormatInt(now.Add(24*time.Hour).Unix(), 10),
		rlp + "7d-status":              "allowed",
		rlp + "7d-surpassed-threshold": "0.1",
		rlp + "status":                 "allowed",
		rlp + "representative-claim":   "five_hour",
		rlp + "reset":                  strconv.FormatInt(now.Add(time.Hour).Unix(), 10),
	}
	for k, v := range want {
		if got := dst.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if len(dst) != len(want) {
		t.Errorf("attached %d headers, want %d: %v", len(dst), len(want), dst)
	}
}

func TestWindowCacheNeverRejected(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	c := newRateLimitWindowCache(10)
	h := windowHeaders(now, time.Hour, 24*time.Hour)
	h.Set(rlp+"status", "rejected")
	h.Set(rlp+"5h-status", "rejected")
	c.Observe("user:a", h, now)

	dst := http.Header{}
	if !c.Attach("user:a", dst, now) {
		t.Fatal("Attach = false")
	}
	if got := dst.Get(rlp + "status"); got != "allowed_warning" {
		t.Errorf("top-level status = %q, want allowed_warning", got)
	}
	if got := dst.Get(rlp + "5h-status"); got != "rejected" {
		t.Errorf("5h status = %q, want the window value copied verbatim", got)
	}
}

func TestWindowCacheDropsExpiredWindows(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	c := newRateLimitWindowCache(10)
	c.Observe("user:a", windowHeaders(now, time.Hour, 24*time.Hour), now)

	later := now.Add(2 * time.Hour)
	dst := http.Header{}
	if !c.Attach("user:a", dst, later) {
		t.Fatal("Attach = false, want true (7d still open)")
	}
	if dst.Get(rlp+"5h-reset") != "" || dst.Get(rlp+"5h-utilization") != "" {
		t.Errorf("expired 5h window attached: %v", dst)
	}
	if dst.Get(rlp+"7d-utilization") != "0.10" {
		t.Errorf("7d window missing: %v", dst)
	}
	if dst.Get(rlp+"representative-claim") != "" || dst.Get(rlp+"reset") != "" {
		t.Errorf("claim five_hour copied though 5h is gone: %v", dst)
	}
}

func TestWindowCacheAllExpiredPrunes(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	c := newRateLimitWindowCache(10)
	c.Observe("user:a", windowHeaders(now, time.Hour, 2*time.Hour), now)

	later := now.Add(3 * time.Hour)
	if c.Attach("user:a", http.Header{}, later) {
		t.Fatal("Attach = true with every window expired")
	}
	c.mu.Lock()
	_, present := c.entries["user:a"]
	c.mu.Unlock()
	if present {
		t.Error("expired entry not deleted")
	}
	if c.Attach("user:a", http.Header{}, now) {
		t.Error("second Attach found an entry")
	}
}

func TestWindowCacheIgnoresHeaderlessObserve(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	c := newRateLimitWindowCache(10)
	c.Observe("user:a", windowHeaders(now, time.Hour, 24*time.Hour), now)
	c.Observe("user:a", http.Header{"Content-Type": {"application/json"}}, now.Add(time.Minute))

	dst := http.Header{}
	if !c.Attach("user:a", dst, now) || dst.Get(rlp+"5h-utilization") != "0.42" {
		t.Errorf("earlier entry lost: %v", dst)
	}
}

func TestWindowCacheLastAccountWins(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	c := newRateLimitWindowCache(10)
	c.Observe("user:a", windowHeaders(now, time.Hour, 24*time.Hour), now)
	second := windowHeaders(now, 2*time.Hour, 48*time.Hour)
	second.Set(rlp+"5h-utilization", "0.77")
	second.Del(rlp + "7d-surpassed-threshold")
	c.Observe("user:a", second, now.Add(time.Second))

	dst := http.Header{}
	if !c.Attach("user:a", dst, now) {
		t.Fatal("Attach = false")
	}
	if got := dst.Get(rlp + "5h-utilization"); got != "0.77" {
		t.Errorf("5h utilization = %q, want the second observation's 0.77", got)
	}
	if dst.Get(rlp+"7d-surpassed-threshold") != "" {
		t.Errorf("stale threshold from the first observation survived: %v", dst)
	}
}

func TestWindowCacheNeverAttachesOverageOrRetry(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	c := newRateLimitWindowCache(10)
	h := windowHeaders(now, time.Hour, 24*time.Hour)
	h.Set(rlp+"overage-status", "rejected")
	h.Set(rlp+"overage-reset", "123")
	h.Set(rlp+"fallback-percentage", "0.5")
	h.Set("Retry-After", "30")
	h.Set("X-Should-Retry", "true")
	c.Observe("user:a", h, now)

	dst := http.Header{}
	c.Attach("user:a", dst, now)
	for _, k := range []string{rlp + "overage-status", rlp + "overage-reset", rlp + "fallback-percentage", "Retry-After", "X-Should-Retry"} {
		if dst.Get(k) != "" {
			t.Errorf("%s attached", k)
		}
	}
}

func TestWindowCacheCap(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	c := newRateLimitWindowCache(2)
	c.Observe("user:a", windowHeaders(now, time.Hour, 24*time.Hour), now)
	c.Observe("user:b", windowHeaders(now, time.Hour, 24*time.Hour), now.Add(time.Second))
	c.Observe("user:c", windowHeaders(now, time.Hour, 24*time.Hour), now.Add(2*time.Second))

	if c.Attach("user:a", http.Header{}, now) {
		t.Error("oldest entry user:a survived the cap")
	}
	if !c.Attach("user:b", http.Header{}, now) || !c.Attach("user:c", http.Header{}, now) {
		t.Error("newer entries evicted")
	}
}

func TestWindowCachePrincipalKey(t *testing.T) {
	if _, ok := principalKey(auth.Token{}); ok {
		t.Error("anonymous token produced a key")
	}
	if k, ok := principalKey(auth.Token{UserID: "u1"}); !ok || k != "user:u1" {
		t.Errorf("user key = %q, %v", k, ok)
	}
	if k, ok := principalKey(auth.Token{Kind: "service", ServiceID: "s1"}); !ok || k != "service:s1" {
		t.Errorf("service key = %q, %v", k, ok)
	}
}
