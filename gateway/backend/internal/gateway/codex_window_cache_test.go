// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"net/http"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"strconv"
	"testing"
	"time"
)

func codexWindowHeaders(now time.Time, primaryOffset, secondaryOffset time.Duration) http.Header {
	h := http.Header{}
	h.Set("x-codex-primary-used-percent", "42.5")
	h.Set("x-codex-primary-window-minutes", "300")
	h.Set("x-codex-primary-reset-at", strconv.FormatInt(now.Add(primaryOffset).Unix(), 10))
	h.Set("x-codex-secondary-used-percent", "17")
	h.Set("x-codex-secondary-window-minutes", "10080")
	h.Set("x-codex-secondary-reset-at", strconv.FormatInt(now.Add(secondaryOffset).Unix(), 10))
	h.Set("x-codex-limit-name", "codex")
	h.Set("x-codex-credits-balance", "5")
	h.Set("x-codex-active-limit", "codex")
	h.Set("retry-after", "30")
	return h
}

func TestCodexCacheAttachCopiesFutureWindows(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	c := newRateLimitWindowCache(10)
	c.ObserveCodex("user:a", "acct1", codexWindowHeaders(now, time.Hour, 24*time.Hour), now)

	dst := http.Header{}
	if !c.AttachCodex("user:a", dst, now) {
		t.Fatal("AttachCodex = false, want true")
	}
	want := map[string]string{
		"x-codex-primary-used-percent":     "42.5",
		"x-codex-primary-window-minutes":   "300",
		"x-codex-primary-reset-at":         strconv.FormatInt(now.Add(time.Hour).Unix(), 10),
		"x-codex-secondary-used-percent":   "17",
		"x-codex-secondary-window-minutes": "10080",
		"x-codex-secondary-reset-at":       strconv.FormatInt(now.Add(24*time.Hour).Unix(), 10),
		"x-codex-limit-name":               "codex",
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

func TestCodexCacheNeverAttachesCreditsOr429Headers(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	c := newRateLimitWindowCache(10)
	c.ObserveCodex("user:a", "acct1", codexWindowHeaders(now, time.Hour, 24*time.Hour), now)
	dst := http.Header{}
	c.AttachCodex("user:a", dst, now)
	for _, k := range []string{"x-codex-credits-balance", "x-codex-active-limit", "retry-after"} {
		if v := dst.Get(k); v != "" {
			t.Errorf("%s attached as %q", k, v)
		}
	}
}

func TestCodexCacheDropsExpiredWindows(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	c := newRateLimitWindowCache(10)
	c.ObserveCodex("user:a", "acct1", codexWindowHeaders(now, -time.Minute, 24*time.Hour), now)
	dst := http.Header{}
	if !c.AttachCodex("user:a", dst, now) {
		t.Fatal("AttachCodex = false, want true")
	}
	if dst.Get("x-codex-primary-used-percent") != "" || dst.Get("x-codex-primary-reset-at") != "" {
		t.Errorf("expired primary attached: %v", dst)
	}
	if dst.Get("x-codex-secondary-used-percent") != "17" || dst.Get("x-codex-limit-name") != "codex" {
		t.Errorf("secondary or limit-name missing: %v", dst)
	}
}

func TestCodexCacheAllExpiredPrunes(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	c := newRateLimitWindowCache(10)
	c.ObserveCodex("user:a", "acct1", codexWindowHeaders(now, time.Hour, 2*time.Hour), now)
	dst := http.Header{}
	if c.AttachCodex("user:a", dst, now.Add(3*time.Hour)) {
		t.Fatal("AttachCodex = true after all windows reset")
	}
	if len(dst) != 0 {
		t.Errorf("wrote headers: %v", dst)
	}
	if _, ok := c.entries["user:a"]; ok {
		t.Error("entry not pruned")
	}
}

func TestCodexCacheIgnoresObserveWithoutUsedPercent(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	c := newRateLimitWindowCache(10)
	c.ObserveCodex("user:a", "acct1", codexWindowHeaders(now, time.Hour, 24*time.Hour), now)
	credits := http.Header{}
	credits.Set("x-codex-credits-balance", "9")
	c.ObserveCodex("user:a", "acct1", credits, now)
	dst := http.Header{}
	if !c.AttachCodex("user:a", dst, now) || dst.Get("x-codex-primary-used-percent") != "42.5" {
		t.Errorf("previous entry lost: %v", dst)
	}
}

func TestCodexCacheSkipsUnparseableReset(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	for _, bad := range []string{"1795290566.5", "-5", "soon"} {
		c := newRateLimitWindowCache(10)
		h := codexWindowHeaders(now, time.Hour, 24*time.Hour)
		h.Set("x-codex-primary-reset-at", bad)
		c.ObserveCodex("user:a", "acct1", h, now)
		dst := http.Header{}
		if !c.AttachCodex("user:a", dst, now) {
			t.Fatalf("reset %q: AttachCodex = false", bad)
		}
		if dst.Get("x-codex-primary-used-percent") != "" {
			t.Errorf("reset %q: primary attached", bad)
		}
		if dst.Get("x-codex-secondary-used-percent") != "17" {
			t.Errorf("reset %q: secondary not attached", bad)
		}
	}
}

func TestCodexCacheExtraFamilies(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	reset := strconv.FormatInt(now.Add(time.Hour).Unix(), 10)
	h := codexWindowHeaders(now, time.Hour, 24*time.Hour)
	h.Set("x-codex-other-primary-used-percent", "5")
	h.Set("x-codex-other-primary-window-minutes", "60")
	h.Set("x-codex-other-primary-reset-at", reset)
	h.Set("x-codex-other-limit-name", "gpt-5.3-codex-spark")
	c := newRateLimitWindowCache(10)
	c.ObserveCodex("user:a", "acct1", h, now)
	dst := http.Header{}
	c.AttachCodex("user:a", dst, now)
	for k, v := range map[string]string{
		"x-codex-other-primary-used-percent":   "5",
		"x-codex-other-primary-window-minutes": "60",
		"x-codex-other-primary-reset-at":       reset,
		"x-codex-other-limit-name":             "gpt-5.3-codex-spark",
		"x-codex-primary-used-percent":         "42.5",
		"x-codex-limit-name":                   "codex",
	} {
		if got := dst.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if dst.Get("x-codex-other-secondary-used-percent") != "" {
		t.Error("absent other secondary attached")
	}
}

func TestCodexCacheFirstValueOfMultiValuedHeader(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	h := codexWindowHeaders(now, time.Hour, 24*time.Hour)
	h["X-Codex-Primary-Used-Percent"] = []string{"42.5", "99"}
	h["X-CODEX-SECONDARY-USED-PERCENT"] = []string{"17", "88"}
	delete(h, "X-Codex-Secondary-Used-Percent")
	c := newRateLimitWindowCache(10)
	c.ObserveCodex("user:a", "acct1", h, now)
	dst := http.Header{}
	c.AttachCodex("user:a", dst, now)
	if got := dst.Get("x-codex-primary-used-percent"); got != "42.5" {
		t.Errorf("primary = %q, want first value", got)
	}
	if got := dst.Values("x-codex-primary-used-percent"); len(got) != 1 {
		t.Errorf("primary values = %v, want one", got)
	}
}

func TestCodexAndAnthropicEntriesCoexist(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s := &Server{rateLimitWindows: newRateLimitWindowCache(10), codexRateLimitWindows: newRateLimitWindowCache(10)}
	s.rateLimitWindows.Observe("user:a", "acct1", windowHeaders(now, time.Hour, 24*time.Hour), now)
	s.codexRateLimitWindows.ObserveCodex("user:a", "acct2", codexWindowHeaders(now, time.Hour, 24*time.Hour), now)

	a := http.Header{}
	if !s.rateLimitWindows.Attach("user:a", a, now) {
		t.Fatal("Anthropic attach = false")
	}
	for k := range a {
		if len(k) >= 7 && (k[:7] == "X-Codex") {
			t.Errorf("Attach wrote %s", k)
		}
	}
	if a.Get(rlp+"5h-utilization") != "0.42" {
		t.Errorf("Anthropic windows lost: %v", a)
	}
	x := http.Header{}
	if !s.codexRateLimitWindows.AttachCodex("user:a", x, now) || x.Get("x-codex-primary-used-percent") != "42.5" {
		t.Errorf("Codex windows lost: %v", x)
	}
	if x.Get(rlp+"5h-utilization") != "" {
		t.Error("AttachCodex wrote Anthropic headers")
	}
}

func TestCodexCacheCapAndForget(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	c := newRateLimitWindowCache(2)
	c.ObserveCodex("user:a", "acct1", codexWindowHeaders(now, time.Hour, 24*time.Hour), now)
	c.ObserveCodex("user:b", "acct1", codexWindowHeaders(now, time.Hour, 24*time.Hour), now.Add(time.Second))
	c.ObserveCodex("user:c", "acct1", codexWindowHeaders(now, time.Hour, 24*time.Hour), now.Add(2*time.Second))
	if _, ok := c.entries["user:a"]; ok {
		t.Error("oldest entry not evicted")
	}
	if len(c.entries) != 2 {
		t.Errorf("entries = %d, want 2", len(c.entries))
	}
	c.Forget("user:b", "other-account")
	if _, ok := c.entries["user:b"]; !ok {
		t.Error("Forget from another account dropped the entry")
	}
	c.Forget("user:b", "acct1")
	if _, ok := c.entries["user:b"]; ok {
		t.Error("Forget from the same account kept the entry")
	}
}

func TestObserveCodexRateLimitWindowsGatesOnEntitlement(t *testing.T) {
	now := time.Now()
	s := &Server{codexRateLimitWindows: newRateLimitWindowCache(10)}
	owner := auth.Token{ID: "tok_o", UserID: "usr_o", Active: true}
	member := auth.Token{ID: "tok_m", UserID: "usr_m", Active: true}
	target := routing.Target{Provider: routing.ProviderVendorOpenAI, VendorAccountID: "acct1", VendorAccountOwnerUserID: "usr_o"}
	h := codexWindowHeaders(now, time.Hour, 24*time.Hour)

	s.observeCodexRateLimitWindows(member, target, h)
	if len(s.codexRateLimitWindows.entries) != 0 {
		t.Fatal("unentitled caller cached windows")
	}
	s.observeCodexRateLimitWindows(owner, target, h)
	dst := http.Header{}
	s.attachCodexRateLimitWindows(owner, dst)
	if dst.Get("x-codex-primary-used-percent") != "42.5" {
		t.Fatalf("owner windows not attached: %v", dst)
	}
	s.observeCodexRateLimitWindows(owner, target, http.Header{"X-Codex-Credits-Balance": {"1"}})
	s.observeCodexRateLimitWindows(auth.Token{}, target, h)
	other := routing.Target{Provider: routing.ProviderVendorAnthropic, VendorAccountID: "acct1", VendorAccountOwnerUserID: "usr_o"}
	s.observeCodexRateLimitWindows(member, other, h)
	if len(s.codexRateLimitWindows.entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(s.codexRateLimitWindows.entries))
	}
}
