// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// TestParseVendorAccountUsage table-tests the tolerant vendor rate-limit header
// parser: the Anthropic unified headers (0..1 fraction -> 0..100 percent, one
// unified reset), the x-codex headers (0..100 percent as-is, per-window resets, a
// credit string), and -- crucially -- that a MISSING or garbage value leaves the
// snapshot at its unknown sentinel (-1 / nil / "") rather than a fabricated 0, and
// that a request carrying none of a provider's headers (or a non-vendor provider)
// yields ok=false so the caller skips the upsert.
func TestParseVendorAccountUsage(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	epoch := int64(1760000000)
	epoch2 := int64(1760500000)
	at := time.Unix(epoch, 0).UTC()
	at2 := time.Unix(epoch2, 0).UTC()

	hdr := func(kv map[string]string) http.Header {
		h := http.Header{}
		for k, v := range kv {
			h.Set(k, v)
		}
		return h
	}
	ptr := func(tm time.Time) *time.Time { return &tm }

	cases := []struct {
		name     string
		provider string
		headers  http.Header
		wantOK   bool
		want     routing.VendorAccountUsage
	}{
		{
			name:     "anthropic full",
			provider: routing.ProviderVendorAnthropic,
			headers: hdr(map[string]string{
				"anthropic-ratelimit-unified-5h-utilization": "0.42",
				"anthropic-ratelimit-unified-7d-utilization": "0.1",
				"anthropic-ratelimit-unified-reset":          "1760000000",
			}),
			wantOK: true,
			want:   routing.VendorAccountUsage{AccountID: "acc", FiveHourPct: 42, WeeklyPct: 10, FiveHourResetAt: ptr(at), UpdatedAt: now},
		},
		{
			name:     "anthropic partial leaves the rest unknown",
			provider: routing.ProviderVendorAnthropic,
			headers:  hdr(map[string]string{"anthropic-ratelimit-unified-5h-utilization": "0"}),
			wantOK:   true,
			// A real 0% five-hour utilization is distinct from the -1 unknown weekly.
			want: routing.VendorAccountUsage{AccountID: "acc", FiveHourPct: 0, WeeklyPct: -1, UpdatedAt: now},
		},
		{
			name:     "anthropic over-range fraction clamps to 100",
			provider: routing.ProviderVendorAnthropic,
			headers:  hdr(map[string]string{"anthropic-ratelimit-unified-5h-utilization": "1.5"}),
			wantOK:   true,
			// 1.5 * 100 = 150 -> clamped to 100 (a real reading can never exceed 100).
			want: routing.VendorAccountUsage{AccountID: "acc", FiveHourPct: 100, WeeklyPct: -1, UpdatedAt: now},
		},
		{
			name:     "openai full",
			provider: routing.ProviderVendorOpenAI,
			headers: hdr(map[string]string{
				"x-codex-primary-used-percent":   "73.5",
				"x-codex-primary-reset-at":       "1760000000",
				"x-codex-secondary-used-percent": "12",
				"x-codex-secondary-reset-at":     "1760500000",
				"x-codex-credits-balance":        "42.50",
			}),
			wantOK: true,
			want: routing.VendorAccountUsage{
				AccountID: "acc", FiveHourPct: 73.5, WeeklyPct: 12,
				FiveHourResetAt: ptr(at), WeeklyResetAt: ptr(at2), CreditBalance: "42.50", UpdatedAt: now,
			},
		},
		{
			name:     "openai garbage percent is unknown not zero",
			provider: routing.ProviderVendorOpenAI,
			headers: hdr(map[string]string{
				"x-codex-primary-used-percent": "n/a",
				"x-codex-credits-balance":      "7.00",
			}),
			wantOK: true,
			// The unparseable percent stays -1 (unknown); only the credit was found.
			want: routing.VendorAccountUsage{AccountID: "acc", FiveHourPct: -1, WeeklyPct: -1, CreditBalance: "7.00", UpdatedAt: now},
		},
		{
			name:     "anthropic NaN is unknown not stored",
			provider: routing.ProviderVendorAnthropic,
			headers: hdr(map[string]string{
				"anthropic-ratelimit-unified-5h-utilization": "NaN",
				"anthropic-ratelimit-unified-7d-utilization": "0.1",
			}),
			wantOK: true,
			// strconv.ParseFloat accepts "NaN"; stored, it would make the usage DTO
			// unmarshalable as JSON (a 500 on the detail GET), so it stays -1 (unknown).
			want: routing.VendorAccountUsage{AccountID: "acc", FiveHourPct: -1, WeeklyPct: 10, UpdatedAt: now},
		},
		{
			name:     "openai NaN percent is unknown not stored",
			provider: routing.ProviderVendorOpenAI,
			headers: hdr(map[string]string{
				"x-codex-primary-used-percent":   "NaN",
				"x-codex-secondary-used-percent": "nan",
				"x-codex-credits-balance":        "7.00",
			}),
			wantOK: true,
			want:   routing.VendorAccountUsage{AccountID: "acc", FiveHourPct: -1, WeeklyPct: -1, CreditBalance: "7.00", UpdatedAt: now},
		},
		{
			name:     "no recognized headers -> not ok",
			provider: routing.ProviderVendorAnthropic,
			headers:  hdr(map[string]string{"content-type": "application/json"}),
			wantOK:   false,
		},
		{
			name:     "non-vendor provider -> not ok",
			provider: routing.ProviderOllama,
			headers:  hdr(map[string]string{"x-codex-primary-used-percent": "50"}),
			wantOK:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseVendorAccountUsage(tc.provider, "acc", tc.headers, now)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (got %+v)", ok, tc.wantOK, got)
			}
			if !tc.wantOK {
				return
			}
			if got.AccountID != tc.want.AccountID || got.FiveHourPct != tc.want.FiveHourPct ||
				got.WeeklyPct != tc.want.WeeklyPct || got.CreditBalance != tc.want.CreditBalance ||
				!got.UpdatedAt.Equal(tc.want.UpdatedAt) {
				t.Fatalf("scalar mismatch:\n got  %+v\n want %+v", got, tc.want)
			}
			if !timePtrEqual(got.FiveHourResetAt, tc.want.FiveHourResetAt) {
				t.Fatalf("FiveHourResetAt = %v, want %v", got.FiveHourResetAt, tc.want.FiveHourResetAt)
			}
			if !timePtrEqual(got.WeeklyResetAt, tc.want.WeeklyResetAt) {
				t.Fatalf("WeeklyResetAt = %v, want %v", got.WeeklyResetAt, tc.want.WeeklyResetAt)
			}
		})
	}
}

func timePtrEqual(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// seedVendorAPIKeyAccount creates an active api-key vendor account directly in the
// store so the scraper has an FK target to upsert against.
func seedVendorAPIKeyAccount(t *testing.T, store *routing.MemoryStore, id, vendor string) {
	t.Helper()
	now := time.Now().UTC()
	if err := store.CreateVendorAccount(context.Background(), routing.VendorAccount{
		ID: id, OwnerUserID: "u1", Vendor: vendor, AuthType: routing.VendorAuthAPIKey,
		Name: id, Status: routing.VendorAccountStatusActive, APIKey: "enc:k", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateVendorAccount: %v", err)
	}
}

// TestScrapeVendorAccountUsageUpsertsForVendorTarget proves the end-to-end scrape:
// for a vendor target with scrapeable upstream headers the per-account snapshot is
// upserted; a target with no VendorAccountID upserts nothing; a vendor target whose
// response carries no recognized headers upserts nothing (no clobber); and an
// upsert against a missing account (the FK failure) is swallowed, never faulting
// the caller.
func TestScrapeVendorAccountUsageUpsertsForVendorTarget(t *testing.T) {
	store := routing.NewMemoryStore()
	seedVendorAPIKeyAccount(t, store, "acc_oai", routing.VendorOpenAI)
	s := &Server{Routes: store}

	headers := http.Header{}
	headers.Set("x-codex-primary-used-percent", "55")
	headers.Set("x-codex-primary-reset-at", "1760000000")

	t.Run("vendor target with headers upserts", func(t *testing.T) {
		target := routing.Target{Provider: routing.ProviderVendorOpenAI, VendorAccountID: "acc_oai"}
		s.scrapeVendorAccountUsage(target, headers)
		got, ok, err := store.VendorAccountUsageByID(context.Background(), "acc_oai")
		if err != nil || !ok {
			t.Fatalf("snapshot after scrape: ok = %v, err = %v", ok, err)
		}
		if got.FiveHourPct != 55 || got.FiveHourResetAt == nil {
			t.Fatalf("snapshot = %+v (5h reset %v), want FiveHourPct 55 with a reset", got, got.FiveHourResetAt)
		}
	})

	t.Run("non-vendor target upserts nothing", func(t *testing.T) {
		seedVendorAPIKeyAccount(t, store, "acc_unused", routing.VendorOpenAI)
		target := routing.Target{Provider: routing.ProviderOllama, ServerID: "srv1"} // no VendorAccountID
		s.scrapeVendorAccountUsage(target, headers)
		if _, ok, _ := store.VendorAccountUsageByID(context.Background(), "acc_unused"); ok {
			t.Fatal("a non-vendor target must not upsert any snapshot")
		}
	})

	t.Run("no recognized headers upserts nothing", func(t *testing.T) {
		seedVendorAPIKeyAccount(t, store, "acc_nohdr", routing.VendorOpenAI)
		target := routing.Target{Provider: routing.ProviderVendorOpenAI, VendorAccountID: "acc_nohdr"}
		bare := http.Header{}
		bare.Set("content-type", "application/json")
		s.scrapeVendorAccountUsage(target, bare)
		if _, ok, _ := store.VendorAccountUsageByID(context.Background(), "acc_nohdr"); ok {
			t.Fatal("a response with no recognized rate-limit headers must not upsert (no clobber)")
		}
	})

	t.Run("upsert failure is swallowed", func(t *testing.T) {
		// A vendor target naming an account that does not exist: the FK upsert fails
		// with ErrNotFound; the scrape must swallow it (no panic, no propagation).
		target := routing.Target{Provider: routing.ProviderVendorOpenAI, VendorAccountID: "ghost"}
		s.scrapeVendorAccountUsage(target, headers) // must not panic
		if _, ok, _ := store.VendorAccountUsageByID(context.Background(), "ghost"); ok {
			t.Fatal("a failed upsert must leave no snapshot")
		}
	})
}

// TestScrapeVendorAccountUsageMergesOverStoredSnapshot proves the passive scrape
// MERGES over the stored per-account snapshot instead of overwriting the whole
// row: a response carrying only the primary/secondary window headers (no
// x-codex-credits-balance, which is the common case) must leave a previously
// stored CreditBalance -- e.g. one the active fetch wrote -- and any window the
// response did not mention intact, while the windows it does report are updated.
// The response headers never carry the Business spend control (#195) either, so a
// stored spend snapshot must survive the scrape whole: parseVendorAccountUsage
// leaves SpendUsedPct at the unknown -1 (the zero value would be a known 0 % and
// overwrite the stored percent in the merge).
func TestScrapeVendorAccountUsageMergesOverStoredSnapshot(t *testing.T) {
	ctx := context.Background()
	store := routing.NewMemoryStore()
	seedVendorAPIKeyAccount(t, store, "acc_merge", routing.VendorOpenAI)
	s := &Server{Routes: store}
	target := routing.Target{Provider: routing.ProviderVendorOpenAI, VendorAccountID: "acc_merge"}

	// Seeded in the past relative to the real clock: the scrape stamps UpdatedAt from
	// time.Now(), so a fixed calendar instant would make the .After check below
	// depend on the runner's date.
	storedAt := time.Now().Add(-time.Hour).UTC()
	storedFiveReset := time.Date(2026, 10, 8, 14, 0, 0, 0, time.UTC)
	storedWeekReset := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	storedSpendReset := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if err := store.UpsertVendorAccountUsage(ctx, routing.VendorAccountUsage{
		AccountID: "acc_merge", FiveHourPct: 10, FiveHourResetAt: &storedFiveReset,
		WeeklyPct: 20, WeeklyResetAt: &storedWeekReset, CreditBalance: "42.50", UpdatedAt: storedAt,
		SpendUnit: "credit", SpendLimit: "6000", SpendUsed: "1500", SpendRemaining: "4500",
		SpendUsedPct: 25, SpendResetAt: &storedSpendReset, CreditStatus: "has_credits",
	}); err != nil {
		t.Fatalf("seed usage snapshot: %v", err)
	}

	// Primary window only: no secondary window, no credit balance.
	headers := http.Header{}
	headers.Set("x-codex-primary-used-percent", "55")
	headers.Set("x-codex-primary-reset-at", "1760000000")
	s.scrapeVendorAccountUsage(target, headers)

	got, ok, err := store.VendorAccountUsageByID(ctx, "acc_merge")
	if err != nil || !ok {
		t.Fatalf("snapshot after scrape: ok = %v, err = %v", ok, err)
	}
	if got.CreditBalance != "42.50" {
		t.Fatalf("CreditBalance = %q, want the stored %q kept (the scrape had no credits header)", got.CreditBalance, "42.50")
	}
	if got.FiveHourPct != 55 {
		t.Fatalf("FiveHourPct = %v, want 55 from the scrape", got.FiveHourPct)
	}
	if want := time.Unix(1760000000, 0).UTC(); got.FiveHourResetAt == nil || !got.FiveHourResetAt.Equal(want) {
		t.Fatalf("FiveHourResetAt = %v, want %v from the scrape", got.FiveHourResetAt, want)
	}
	if got.WeeklyPct != 20 || got.WeeklyResetAt == nil || !got.WeeklyResetAt.Equal(storedWeekReset) {
		t.Fatalf("weekly = %v reset %v, want the stored 20 / %v kept (the scrape had no secondary headers)", got.WeeklyPct, got.WeeklyResetAt, storedWeekReset)
	}
	if got.SpendUnit != "credit" || got.SpendLimit != "6000" || got.SpendUsed != "1500" || got.SpendRemaining != "4500" ||
		got.SpendUsedPct != 25 || got.SpendResetAt == nil || !got.SpendResetAt.Equal(storedSpendReset) || got.CreditStatus != "has_credits" {
		t.Fatalf("spend snapshot = %+v, want the stored spend control kept whole (the scrape carries none)", got)
	}
	if !got.UpdatedAt.After(storedAt) {
		t.Fatalf("UpdatedAt = %v, want it advanced past the stored %v (it comes from the scrape)", got.UpdatedAt, storedAt)
	}
}

// failingUsageReadStore is a routing.Store whose usage-snapshot READ always fails,
// to prove the scrape's merge step is as best-effort as the rest of the scrape.
type failingUsageReadStore struct {
	*routing.MemoryStore
}

func (failingUsageReadStore) VendorAccountUsageByID(context.Context, string) (routing.VendorAccountUsage, bool, error) {
	return routing.VendorAccountUsage{}, false, errors.New("usage read unavailable")
}

// TestScrapeVendorAccountUsageReadErrorFallsBackToParsedSnapshot proves that a
// failure to read the stored snapshot (the merge's only new I/O) never faults the
// scrape: it is swallowed and the parsed snapshot is upserted exactly as before.
func TestScrapeVendorAccountUsageReadErrorFallsBackToParsedSnapshot(t *testing.T) {
	mem := routing.NewMemoryStore()
	seedVendorAPIKeyAccount(t, mem, "acc_rd", routing.VendorOpenAI)
	s := &Server{Routes: failingUsageReadStore{MemoryStore: mem}}

	headers := http.Header{}
	headers.Set("x-codex-primary-used-percent", "55")
	s.scrapeVendorAccountUsage(routing.Target{Provider: routing.ProviderVendorOpenAI, VendorAccountID: "acc_rd"}, headers) // must not panic

	// Read it back through the underlying store (the wrapper's read always fails).
	got, ok, err := mem.VendorAccountUsageByID(context.Background(), "acc_rd")
	if err != nil || !ok {
		t.Fatalf("snapshot after a failed merge read: ok = %v, err = %v; want the parsed snapshot upserted", ok, err)
	}
	if got.FiveHourPct != 55 || got.WeeklyPct != -1 || got.CreditBalance != "" || got.SpendUsedPct != -1 {
		t.Fatalf("snapshot = %+v, want exactly the parsed one (5h 55, weekly unknown, no credit, spend unknown)", got)
	}
}

// TestRecordUsageScrapesVendorUsage ties recordUsage to the scrape: recording a
// usage event for a vendor target with upstream rate-limit headers upserts the
// snapshot, while a self-hosted target leaves no snapshot behind.
func TestRecordUsageScrapesVendorUsage(t *testing.T) {
	store := routing.NewMemoryStore()
	seedVendorAPIKeyAccount(t, store, "acc_anthropic", routing.VendorAnthropic)
	s := NewTestServer()
	s.Routes = store

	headers := http.Header{}
	headers.Set("anthropic-ratelimit-unified-5h-utilization", "0.8")
	headers.Set("anthropic-ratelimit-unified-reset", "1760000000")

	target := routing.Target{RouteID: "vendor:acc_anthropic:claude", Provider: routing.ProviderVendorAnthropic, VendorAccountID: "acc_anthropic"}
	s.recordUsage(time.Now(), auth.Token{ID: "tok", UserID: "usr_v"}, inference.Request{Model: "claude"}, target, provider.Response{}, "", "success", usageMeta{}, "req_scrape", nil, headers)

	got, ok, err := store.VendorAccountUsageByID(context.Background(), "acc_anthropic")
	if err != nil || !ok {
		t.Fatalf("snapshot after recordUsage: ok = %v, err = %v", ok, err)
	}
	if got.FiveHourPct != 80 {
		t.Fatalf("FiveHourPct = %v, want 80 (0.8 fraction scaled)", got.FiveHourPct)
	}
}

// TestHeaderOnlySinkCapturesVendorHeadersWithoutCapture is the fork-resolution
// guard: the translate path can scrape a vendor account's rate-limit headers even
// when payload capture is OFF, because complete/beginStream attach a HEADER-ONLY
// capture sink (respCap 0, no body buffered) for a vendor target. This exercises
// that chain against a real provider client + HTTP stub: the respCap-0 sink records
// the upstream response headers, and scrapeVendorAccountUsage then upserts the
// snapshot from them.
func TestHeaderOnlySinkCapturesVendorHeadersWithoutCapture(t *testing.T) {
	// A stub Anthropic Messages endpoint that returns the rate-limit headers.
	withRateLimitHeaders := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("anthropic-ratelimit-unified-5h-utilization", "0.25")
		w.Header().Set("anthropic-ratelimit-unified-reset", "1760000000")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer withRateLimitHeaders.Close()

	store := routing.NewMemoryStore()
	seedVendorAPIKeyAccount(t, store, "acc_h", routing.VendorAnthropic)
	s := &Server{Routes: store}

	target := routing.Target{
		Provider: routing.ProviderVendorAnthropic, Endpoint: withRateLimitHeaders.URL,
		Model: "claude", ProviderModel: "claude-3", APIFlavor: routing.APIFlavorAnthropic,
		VendorAccountID: "acc_h", APIFlavors: []string{routing.APIFlavorAnthropic},
	}

	// A HEADER-ONLY sink (respCap 0), exactly as complete/beginStream attach for a
	// non-capturing vendor target.
	sink := provider.NewCaptureSink(0)
	ctx := provider.WithCaptureSink(context.Background(), sink)
	client := provider.NewAnthropicClient(withRateLimitHeaders.Client())
	if _, err := client.Complete(ctx, target, dispatchReq()); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// The respCap-0 sink still recorded the response headers; feed them to the
	// scraper exactly as recordUsage does.
	s.scrapeVendorAccountUsage(target, sink.ResponseHeaders())
	got, ok, err := store.VendorAccountUsageByID(context.Background(), "acc_h")
	if err != nil || !ok {
		t.Fatalf("snapshot after header-only scrape: ok = %v, err = %v", ok, err)
	}
	if got.FiveHourPct != 25 || got.FiveHourResetAt == nil {
		t.Fatalf("snapshot = %+v (5h reset %v), want FiveHourPct 25 with a reset", got, got.FiveHourResetAt)
	}
}
