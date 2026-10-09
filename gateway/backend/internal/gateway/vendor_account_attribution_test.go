// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/usage"
	"strconv"
	"testing"
	"time"
)

// TestRecordUsageAttributesVendorAccountID proves recordUsage stamps the usage
// event with the serving target's VendorAccountID (M6a per-account attribution),
// the same shape ServiceID/ProjectID attribution already follows: it is read off
// the resolved Target, needing no extra store round-trip.
func TestRecordUsageAttributesVendorAccountID(t *testing.T) {
	srv := NewTestServer()
	target := routing.Target{RouteID: "vendor:acc_x:gpt-4o", Provider: routing.ProviderVendorOpenAI, VendorAccountID: "acc_x"}
	srv.recordUsage(time.Now(), auth.Token{ID: "tok", UserID: "usr_v"}, inference.Request{Model: "gpt-4o"}, target, provider.Response{}, "", "success", usageMeta{}, "req_vendor_attr", nil, nil)

	events := srv.Usage.ByUser("usr_v")
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].AccountID != "acc_x" {
		t.Fatalf("recorded AccountID = %q, want acc_x", events[0].AccountID)
	}
}

// TestRecordUsageVendorAccountIDEmptyForNonVendor is the no-op-invariant
// regression: a self-hosted target carries no VendorAccountID, so the recorded
// event's AccountID must be "" (the overwhelming default, byte-identical to
// pre-M6a behavior).
func TestRecordUsageVendorAccountIDEmptyForNonVendor(t *testing.T) {
	srv := NewTestServer()
	target := routing.Target{RouteID: "map_1", Provider: routing.ProviderOllama, ServerID: "srv1"}
	srv.recordUsage(time.Now(), auth.Token{ID: "tok", UserID: "usr_s"}, inference.Request{Model: "m"}, target, provider.Response{}, "", "success", usageMeta{}, "req_selfhosted_attr", nil, nil)

	events := srv.Usage.ByUser("usr_s")
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].AccountID != "" {
		t.Fatalf("self-hosted event AccountID = %q, want empty", events[0].AccountID)
	}
}

// vendorRateTarget is a vendor-account (subscription/api-key) target: the only
// kind whose provider reports no timings and so the only kind the generation-window
// tokens/s fallback in recordUsage may fill in.
func vendorRateTarget() routing.Target {
	return routing.Target{RouteID: "vendor:acc_x:gpt-4o", Provider: routing.ProviderVendorOpenAI, VendorAccountID: "acc_x"}
}

// recordOneUsage records a single usage event for user and returns it, so the
// fallback tests below read the stored row rather than any in-memory intermediate.
func recordOneUsage(t *testing.T, srv *Server, user string, target routing.Target, resp provider.Response, meta usageMeta) usage.Event {
	t.Helper()
	srv.recordUsage(time.Now(), auth.Token{ID: "tok", UserID: user}, inference.Request{Model: "gpt-4o"}, target, resp, "", "success", meta, "req_"+user, nil, nil)
	events := srv.Usage.ByUser(user)
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	return events[0]
}

// TestRecordUsageVendorDerivesTokensPerSecondFromGenerationWindow is the #182
// fix: a vendor translate-stream row arrives with Usage.TokensPerSecond == 0 (no
// vendor sends timings), which the Activity list rendered as an em-dash. With a
// real first-token stamp (GenStart) the row carries output_tokens over the
// generation window, floored exactly like the chat run's own end-of-chat figure
// so the Activity row equals what the user saw.
func TestRecordUsageVendorDerivesTokensPerSecondFromGenerationWindow(t *testing.T) {
	srv := NewTestServer()
	genStart := time.Now().Add(-2 * time.Second)
	resp := provider.Response{Usage: inference.Usage{InputTokens: 10, OutputTokens: 200, TotalTokens: 210}}

	ev := recordOneUsage(t, srv, "usr_vrate", vendorRateTarget(), resp, usageMeta{GenStart: genStart})

	// genEnd is the event's own CreatedAt stamp, so the expectation is exact.
	want := flooredRate(200, ev.CreatedAt.Sub(genStart))
	if want <= 0 || want > 100 {
		t.Fatalf("test setup: expected rate = %v, want in (0, 100] for 200 tokens over >= 2s", want)
	}
	if ev.TokensPerSecond != want {
		t.Fatalf("TokensPerSecond = %v, want %v (flooredRate over the generation window)", ev.TokensPerSecond, want)
	}
}

// TestRecordUsageTokensPerSecondFallbackScope pins every condition the fallback
// is gated on. Each case would yield a non-zero rate if its one guard were
// missing, so a zero (or untouched) result proves that guard is load-bearing.
func TestRecordUsageTokensPerSecondFallbackScope(t *testing.T) {
	window := time.Now().Add(-2 * time.Second)
	selfHosted := routing.Target{RouteID: "map_1", Provider: routing.ProviderOllama, ServerID: "srv1"}
	cases := []struct {
		name   string
		target routing.Target
		resp   provider.Response
		meta   usageMeta
		want   float64
	}{
		{
			// A self-hosted row with no reported rate means "the server did not
			// say", and a gateway-derived number there would pollute the routing
			// EWMA input and the speed histogram with a different quantity.
			name:   "non-vendor target stays zero",
			target: selfHosted,
			resp:   provider.Response{Usage: inference.Usage{OutputTokens: 200}},
			meta:   usageMeta{GenStart: window},
			want:   0,
		},
		{
			// A measurement the provider made is never replaced by an estimate.
			name:   "provider-supplied rate is not overwritten",
			target: vendorRateTarget(),
			resp:   provider.Response{Usage: inference.Usage{OutputTokens: 200, TokensPerSecond: 37.5}},
			meta:   usageMeta{GenStart: window},
			want:   37.5,
		},
		{
			// Tool-only turns never stamp a first token and non-stream callers
			// have no window; there is nothing honest to divide by.
			name:   "no generation window stays zero",
			target: vendorRateTarget(),
			resp:   provider.Response{Usage: inference.Usage{OutputTokens: 200}},
			meta:   usageMeta{},
			want:   0,
		},
		{
			// Non-token-metered (image) rows must carry zero in every
			// token-denominated column (usage.ValidateBillingXOR). OutputTokens is
			// deliberately non-zero here so the BillingUnit guard is proven on its
			// own -- which makes this an XOR-violating row, and recordUsage's
			// (expected) "billing contract violated" Error log appears in the output.
			name:   "non-token billing unit stays zero",
			target: vendorRateTarget(),
			resp:   provider.Response{Usage: inference.Usage{OutputTokens: 200}},
			meta:   usageMeta{GenStart: window, BillingUnit: usage.BillingUnitImage, BillingQuantity: 1},
			want:   0,
		},
		{
			name:   "no output tokens stays zero",
			target: vendorRateTarget(),
			resp:   provider.Response{Usage: inference.Usage{InputTokens: 10}},
			meta:   usageMeta{GenStart: window},
			want:   0,
		},
		{
			// flooredRate's own floor: a sub-50ms window is nonsense, not a rate.
			name:   "window below the rate floor (GenStart in the future) stays zero",
			target: vendorRateTarget(),
			resp:   provider.Response{Usage: inference.Usage{OutputTokens: 200}},
			meta:   usageMeta{GenStart: time.Now().Add(time.Hour)},
			want:   0,
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := NewTestServer()
			ev := recordOneUsage(t, srv, "usr_scope"+strconv.Itoa(i), tc.target, tc.resp, tc.meta)
			if ev.TokensPerSecond != tc.want {
				t.Fatalf("TokensPerSecond = %v, want %v", ev.TokensPerSecond, tc.want)
			}
		})
	}
}

// TestRecordUsageDerivedTokensPerSecondNeverFeedsTheRoutingEWMA proves the derived
// rate lives on the usage EVENT only. The opportunistic EWMA reads
// resp.Usage.TokensPerSecond (0 here), so it must not move even though the stored
// row now carries a positive rate. The opt-in is switched ON and the row is a
// success so the EWMA WOULD fire on a positive sample: a test with the gate off
// would pass whatever the fallback did to resp.Usage.
func TestRecordUsageDerivedTokensPerSecondNeverFeedsTheRoutingEWMA(t *testing.T) {
	srv := NewTestServer()
	target := vendorRateTarget()
	target.RouteID = seedMappingID
	target.OpportunisticMetrics = true
	resp := provider.Response{Usage: inference.Usage{OutputTokens: 200}}

	ev := recordOneUsage(t, srv, "usr_ewma", target, resp, usageMeta{GenStart: time.Now().Add(-2 * time.Second)})

	if ev.TokensPerSecond <= 0 {
		t.Fatalf("test setup: TokensPerSecond = %v, want a derived positive rate", ev.TokensPerSecond)
	}
	if got := mappingGenTPS(t, srv); got != 0 {
		t.Fatalf("mapping GenTokensPerSecond = %v, want 0 (the derived event rate must not reach the routing EWMA)", got)
	}
}

// finishSession builds the minimum streamSession that finish() touches.
func finishSession(srv *Server, target routing.Target, progress *requestProgress) *streamSession {
	return &streamSession{
		s:        srv,
		w:        httptest.NewRecorder(),
		r:        httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil),
		token:    auth.Token{ID: "tok", UserID: "usr_fin"},
		req:      inference.Request{Model: "gpt-4o", Stream: true},
		id:       "req_fin",
		start:    time.Now().Add(-3 * time.Second),
		target:   target,
		progress: progress,
	}
}

// TestStreamSessionFinishPassesFirstTokenWindowToUsage pins the wiring between the
// live progress stamp and the usage fallback: finish() hands the first-token
// instant to recordUsage, so a vendor stream whose provider reported no rate is
// recorded with output_tokens over [first token, end].
func TestStreamSessionFinishPassesFirstTokenWindowToUsage(t *testing.T) {
	srv := NewTestServer()
	progress := &requestProgress{}
	firstToken := time.Now().Add(-2 * time.Second)
	progress.observeDelta(firstToken, nil)

	finishSession(srv, vendorRateTarget(), progress).finish(inference.Usage{OutputTokens: 200, TotalTokens: 200}, "success", "")

	events := srv.Usage.ByUser("usr_fin")
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	want := flooredRate(200, events[0].CreatedAt.Sub(progress.firstTokenAt()))
	if want <= 0 {
		t.Fatalf("test setup: expected rate = %v, want positive", want)
	}
	if events[0].TokensPerSecond != want {
		t.Fatalf("TokensPerSecond = %v, want %v", events[0].TokensPerSecond, want)
	}
}

// TestStreamSessionFinishToolOnlyStreamHasNoRate: a stream that never produced a
// text/reasoning delta (tool-only turn) has no first-token stamp, so the vendor
// row stays at 0 rather than inventing a window.
func TestStreamSessionFinishToolOnlyStreamHasNoRate(t *testing.T) {
	srv := NewTestServer()

	finishSession(srv, vendorRateTarget(), &requestProgress{}).finish(inference.Usage{OutputTokens: 200, TotalTokens: 200}, "success", "")

	events := srv.Usage.ByUser("usr_fin")
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].TokensPerSecond != 0 {
		t.Fatalf("TokensPerSecond = %v, want 0 for a stream with no first-token stamp", events[0].TokensPerSecond)
	}
}
