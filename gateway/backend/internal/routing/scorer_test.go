// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"math"
	"testing"
	"time"
)

func TestSelectRoutePrefersHealthyLowLoadFastRoute(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	routes := []Route{
		{ID: "slow", Model: "qwen-coder", Priority: 10, Weight: 50, Healthy: true, ActiveRequests: 1, LatencyMS: 900, TelemetryAt: now},
		{ID: "fast", Model: "qwen-coder", Priority: 10, Weight: 50, Healthy: true, ActiveRequests: 1, LatencyMS: 120, TelemetryAt: now},
	}

	selected, score, ok := Select(routes, "qwen-coder", now)

	if !ok {
		t.Fatalf("Select returned ok=false")
	}
	if selected.ID != "fast" {
		t.Fatalf("selected = %s, want fast", selected.ID)
	}
	if score <= 0 {
		t.Fatalf("score = %f, want positive", score)
	}
}

func TestSelectRouteRejectsUnhealthyRoutes(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	routes := []Route{{ID: "bad", Model: "qwen-coder", Priority: 100, Weight: 100, Healthy: false, TelemetryAt: now}}

	_, _, ok := Select(routes, "qwen-coder", now)

	if ok {
		t.Fatalf("Select returned ok=true for unhealthy route")
	}
}

func TestSelectRoutePenalizesStaleTelemetry(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	routes := []Route{
		{ID: "stale", Model: "qwen-coder", Priority: 10, Weight: 50, Healthy: true, LatencyMS: 100, TelemetryAt: now.Add(-10 * time.Minute)},
		{ID: "fresh", Model: "qwen-coder", Priority: 10, Weight: 50, Healthy: true, LatencyMS: 150, TelemetryAt: now},
	}

	selected, _, ok := Select(routes, "qwen-coder", now)

	if !ok {
		t.Fatalf("Select returned ok=false")
	}
	if selected.ID != "fresh" {
		t.Fatalf("selected = %s, want fresh", selected.ID)
	}
}

func TestSelectRoutePenalizesMissingTelemetry(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	routes := []Route{
		{ID: "unknown", Model: "qwen-coder", Priority: 10, Weight: 50, Healthy: true, LatencyMS: 100},
		{ID: "fresh", Model: "qwen-coder", Priority: 10, Weight: 50, Healthy: true, LatencyMS: 150, TelemetryAt: now},
	}

	selected, _, ok := Select(routes, "qwen-coder", now)

	if !ok {
		t.Fatalf("Select returned ok=false")
	}
	if selected.ID != "fresh" {
		t.Fatalf("selected = %s, want fresh", selected.ID)
	}
}

func TestScoreRejectsInvalidTelemetry(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		route Route
	}{
		{
			name:  "negative active requests",
			route: Route{ID: "bad", Model: "qwen-coder", Healthy: true, ActiveRequests: -1, TelemetryAt: now},
		},
		{
			name:  "negative queue depth",
			route: Route{ID: "bad", Model: "qwen-coder", Healthy: true, QueueDepth: -1, TelemetryAt: now},
		},
		{
			name:  "negative latency",
			route: Route{ID: "bad", Model: "qwen-coder", Healthy: true, LatencyMS: -1, TelemetryAt: now},
		},
		{
			name:  "negative error rate",
			route: Route{ID: "bad", Model: "qwen-coder", Healthy: true, ErrorRate: -0.01, TelemetryAt: now},
		},
		{
			name:  "error rate over one",
			route: Route{ID: "bad", Model: "qwen-coder", Healthy: true, ErrorRate: 1.01, TelemetryAt: now},
		},
		{
			name:  "nan error rate",
			route: Route{ID: "bad", Model: "qwen-coder", Healthy: true, ErrorRate: math.NaN(), TelemetryAt: now},
		},
		{
			name:  "positive infinite error rate",
			route: Route{ID: "bad", Model: "qwen-coder", Healthy: true, ErrorRate: math.Inf(1), TelemetryAt: now},
		},
		{
			name:  "negative infinite error rate",
			route: Route{ID: "bad", Model: "qwen-coder", Healthy: true, ErrorRate: math.Inf(-1), TelemetryAt: now},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ok := Score(tt.route, "qwen-coder", now)

			if ok {
				t.Fatalf("Score returned ok=true for invalid telemetry")
			}
		})
	}
}

func TestScoreRejectsModelMismatch(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	route := Route{ID: "codellama", Model: "codellama", Healthy: true, TelemetryAt: now}

	_, ok := Score(route, "qwen-coder", now)

	if ok {
		t.Fatalf("Score returned ok=true for wrong model")
	}
}

func TestSelectRouteUsesPriorityAndWeight(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	routes := []Route{
		{ID: "standard", Model: "qwen-coder", Priority: 10, Weight: 50, Healthy: true, LatencyMS: 100, TelemetryAt: now},
		{ID: "preferred", Model: "qwen-coder", Priority: 12, Weight: 70, Healthy: true, LatencyMS: 100, TelemetryAt: now},
	}

	selected, _, ok := Select(routes, "qwen-coder", now)

	if !ok {
		t.Fatalf("Select returned ok=false")
	}
	if selected.ID != "preferred" {
		t.Fatalf("selected = %s, want preferred", selected.ID)
	}
}

func TestSelectRoutePenalizesQueueDepthAndErrorRate(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	routes := []Route{
		{ID: "busy-erroring", Model: "qwen-coder", Priority: 10, Weight: 50, Healthy: true, QueueDepth: 8, LatencyMS: 100, ErrorRate: 0.5, TelemetryAt: now},
		{ID: "quiet-stable", Model: "qwen-coder", Priority: 10, Weight: 50, Healthy: true, QueueDepth: 1, LatencyMS: 100, ErrorRate: 0.05, TelemetryAt: now},
	}

	selected, _, ok := Select(routes, "qwen-coder", now)

	if !ok {
		t.Fatalf("Select returned ok=false")
	}
	if selected.ID != "quiet-stable" {
		t.Fatalf("selected = %s, want quiet-stable", selected.ID)
	}
}

func TestScoreRejectsNonPositiveScores(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	route := Route{ID: "overloaded", Model: "qwen-coder", Healthy: true, QueueDepth: 100, LatencyMS: 1000, ErrorRate: 1, TelemetryAt: now}

	score, ok := Score(route, "qwen-coder", now)

	if ok {
		t.Fatalf("Score returned ok=true, score=%f", score)
	}
}

func TestSelectRoutePrefersHigherGenThroughput(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	routes := []Route{
		{ID: "no-metrics", Model: "qwen-coder", Priority: 10, Weight: 50, Healthy: true, LatencyMS: 100, TelemetryAt: now},
		{ID: "fast-gen", Model: "qwen-coder", Priority: 10, Weight: 50, Healthy: true, LatencyMS: 100, TelemetryAt: now, GenTokensPerSecond: 100},
	}

	selected, _, ok := Select(routes, "qwen-coder", now)

	if !ok {
		t.Fatalf("Select returned ok=false")
	}
	if selected.ID != "fast-gen" {
		t.Fatalf("selected = %s, want fast-gen (higher gen throughput)", selected.ID)
	}
}

func TestSelectRoutePrefersMTP(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	routes := []Route{
		{ID: "plain", Model: "qwen-coder", Priority: 10, Weight: 50, Healthy: true, LatencyMS: 100, TelemetryAt: now},
		{ID: "mtp", Model: "qwen-coder", Priority: 10, Weight: 50, Healthy: true, LatencyMS: 100, TelemetryAt: now, IsMTP: true},
	}

	selected, _, ok := Select(routes, "qwen-coder", now)

	if !ok {
		t.Fatalf("Select returned ok=false")
	}
	if selected.ID != "mtp" {
		t.Fatalf("selected = %s, want mtp", selected.ID)
	}
}

// The P4a no-op invariant: routes with all metric fields at zero score exactly as
// before, so a lower-latency route wins on the pre-existing telemetry term alone.
func TestScoreZeroMetricsUnchanged(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	routes := []Route{
		{ID: "slow", Model: "qwen-coder", Priority: 10, Weight: 50, Healthy: true, LatencyMS: 900, TelemetryAt: now},
		{ID: "fast", Model: "qwen-coder", Priority: 10, Weight: 50, Healthy: true, LatencyMS: 120, TelemetryAt: now},
	}

	selected, _, ok := Select(routes, "qwen-coder", now)

	if !ok {
		t.Fatalf("Select returned ok=false")
	}
	if selected.ID != "fast" {
		t.Fatalf("selected = %s, want fast (zero metrics contribute nothing)", selected.ID)
	}
}

// The bounded speed/MTP tiebreak must never let a benchmarked-fast but erroring server
// outscore a healthy idle one: it is folded in AFTER the health/load terms and capped
// well below errorRatePenalty (200). Both are viable; the healthy one must win.
func TestScoreTiebreakDoesNotOverrideErrorPenalty(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	healthy := Route{ID: "healthy", Model: "qwen-coder", Priority: 10, Weight: 50, Healthy: true, ErrorRate: 0, TelemetryAt: now}
	erroringFast := Route{ID: "erroring-fast", Model: "qwen-coder", Priority: 10, Weight: 50, Healthy: true, ErrorRate: 1.0, TelemetryAt: now, GenTokensPerSecond: 1e6, PromptTokensPerSecond: 1e6}

	scoreHealthy, okHealthy := Score(healthy, "qwen-coder", now)
	scoreErroring, okErroring := Score(erroringFast, "qwen-coder", now)

	if !okHealthy || !okErroring {
		t.Fatalf("expected both viable, got okHealthy=%v okErroring=%v", okHealthy, okErroring)
	}
	if !(scoreHealthy > scoreErroring) {
		t.Fatalf("healthy score %f should beat erroring-fast score %f (tiebreak must not override the error penalty)", scoreHealthy, scoreErroring)
	}
}

// A server driven non-viable by load ALONE (before any metric term) must stay non-viable:
// the tiebreak is applied only after the score<=0 gate, so a huge measured throughput +
// MTP cannot rescue a dead server past the gate (which would defeat the prefer-loaded
// fail-open spillover in the resolver).
func TestScoreTiebreakDoesNotRescueNonViable(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	// base 1000 + 10*20 + 50 - 60*25 = 1250 - 1500 = -250 <= 0 on load alone.
	route := Route{ID: "overloaded-fast", Model: "qwen-coder", Priority: 10, Weight: 50, Healthy: true, ActiveRequests: 60, TelemetryAt: now, GenTokensPerSecond: 1e6, PromptTokensPerSecond: 1e6, IsMTP: true}

	score, ok := Score(route, "qwen-coder", now)

	if ok {
		t.Fatalf("Score returned ok=true (score=%f): the tiebreak must not rescue a load-non-viable server", score)
	}
}

// The total tiebreak is capped: an implausibly large stored throughput (and MTP) can add
// at most genThroughputBonusCap + promptThroughputBonusCap + mtpBonus over the same route
// with all metrics zero.
func TestScoreTiebreakBounded(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	base := Route{ID: "r", Model: "qwen-coder", Priority: 10, Weight: 50, Healthy: true, LatencyMS: 100, TelemetryAt: now}
	maxed := base
	maxed.GenTokensPerSecond = 1e9
	maxed.PromptTokensPerSecond = 1e9
	maxed.IsMTP = true

	scoreBase, okBase := Score(base, "qwen-coder", now)
	scoreMaxed, okMaxed := Score(maxed, "qwen-coder", now)

	if !okBase || !okMaxed {
		t.Fatalf("expected both viable, got okBase=%v okMaxed=%v", okBase, okMaxed)
	}
	want := genThroughputBonusCap + promptThroughputBonusCap + mtpBonus
	if got := scoreMaxed - scoreBase; got != want {
		t.Fatalf("tiebreak delta = %f, want %f (total tiebreak must be capped)", got, want)
	}
}

// A fast lower-priority server CAN beat an idle higher-priority one for a SMALL priority
// delta (user requirement), but a LARGE priority delta beyond the tiebreak band is NOT
// overcome by the capped bonus (priority still dominates).
func TestScoreTiebreakBeatsSmallPriorityDelta(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)

	// Small delta: fast p=10 with GenTokensPerSecond 200 (+50) vs idle p=12 (+40 priority).
	fastLow := Route{ID: "fast-low", Model: "qwen-coder", Priority: 10, Weight: 50, Healthy: true, LatencyMS: 100, TelemetryAt: now, GenTokensPerSecond: 200}
	idleHigh := Route{ID: "idle-high", Model: "qwen-coder", Priority: 12, Weight: 50, Healthy: true, LatencyMS: 100, TelemetryAt: now}
	selected, _, ok := Select([]Route{idleHigh, fastLow}, "qwen-coder", now)
	if !ok {
		t.Fatalf("Select returned ok=false")
	}
	if selected.ID != "fast-low" {
		t.Fatalf("selected = %s, want fast-low (fast beats a small priority delta)", selected.ID)
	}

	// Large delta: fast p=10 at the MAX +100 bonus vs idle p=16 (+120 priority). Priority wins.
	fastMaxed := Route{ID: "fast-maxed", Model: "qwen-coder", Priority: 10, Weight: 50, Healthy: true, LatencyMS: 100, TelemetryAt: now, GenTokensPerSecond: 1e9, PromptTokensPerSecond: 1e9, IsMTP: true}
	idleFarHigher := Route{ID: "idle-far-higher", Model: "qwen-coder", Priority: 16, Weight: 50, Healthy: true, LatencyMS: 100, TelemetryAt: now}
	selected2, _, ok2 := Select([]Route{fastMaxed, idleFarHigher}, "qwen-coder", now)
	if !ok2 {
		t.Fatalf("Select returned ok=false")
	}
	if selected2.ID != "idle-far-higher" {
		t.Fatalf("selected = %s, want idle-far-higher (a large priority delta beats the capped tiebreak)", selected2.ID)
	}
}

// The clamp guards NaN/Inf in a stored metric: NaN contributes 0, +Inf contributes exactly
// the cap, and the resulting score is a normal finite comparable value (no panic).
func TestScoreTiebreakNaNInfGuard(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	baseline := Route{ID: "baseline", Model: "qwen-coder", Priority: 10, Weight: 50, Healthy: true, LatencyMS: 100, TelemetryAt: now}
	scoreBaseline, ok := Score(baseline, "qwen-coder", now)
	if !ok {
		t.Fatalf("baseline not viable")
	}

	nanRoute := baseline
	nanRoute.GenTokensPerSecond = math.NaN()
	scoreNaN, okNaN := Score(nanRoute, "qwen-coder", now)
	if !okNaN {
		t.Fatalf("NaN route not viable")
	}
	if math.IsNaN(scoreNaN) || math.IsInf(scoreNaN, 0) {
		t.Fatalf("NaN route score = %f, want finite", scoreNaN)
	}
	if scoreNaN != scoreBaseline {
		t.Fatalf("NaN gen throughput contributed %f, want 0 (score %f vs baseline %f)", scoreNaN-scoreBaseline, scoreNaN, scoreBaseline)
	}

	infRoute := baseline
	infRoute.GenTokensPerSecond = math.Inf(1)
	scoreInf, okInf := Score(infRoute, "qwen-coder", now)
	if !okInf {
		t.Fatalf("Inf route not viable")
	}
	if math.IsNaN(scoreInf) || math.IsInf(scoreInf, 0) {
		t.Fatalf("Inf route score = %f, want finite", scoreInf)
	}
	if got := scoreInf - scoreBaseline; got != genThroughputBonusCap {
		t.Fatalf("+Inf gen throughput contributed %f, want the cap %f", got, genThroughputBonusCap)
	}
}

func TestSelectRouteKeepsFirstRouteOnEqualScore(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	routes := []Route{
		{ID: "first", Model: "qwen-coder", Priority: 10, Weight: 50, Healthy: true, ActiveRequests: 1, LatencyMS: 100, TelemetryAt: now},
		{ID: "second", Model: "qwen-coder", Priority: 10, Weight: 50, Healthy: true, ActiveRequests: 1, LatencyMS: 100, TelemetryAt: now},
	}

	selected, _, ok := Select(routes, "qwen-coder", now)

	if !ok {
		t.Fatalf("Select returned ok=false")
	}
	if selected.ID != "first" {
		t.Fatalf("selected = %s, want first", selected.ID)
	}
}

func TestEffectiveGenTPSNoCapacityMetricsIsFlat(t *testing.T) {
	r := Route{GenTokensPerSecond: 100, CurrentLoad: 8}
	if got := effectiveGenTPS(r); got != 100 {
		t.Fatalf("effectiveGenTPS = %v, want 100 (flat, no capacity metrics)", got)
	}
}

func TestEffectiveGenTPSIdleIsFlat(t *testing.T) {
	r := Route{GenTokensPerSecond: 100, GenTokensPerSecondAtCapacity: 40, RecommendedConcurrency: 4, CurrentLoad: 1}
	if got := effectiveGenTPS(r); got != 100 {
		t.Fatalf("effectiveGenTPS(k=1) = %v, want 100", got)
	}
}

func TestEffectiveGenTPSInterpolatesWithLoad(t *testing.T) {
	// gen@1=100, gen@cap=40 at rec=4 => slope = (40-100)/3 = -20/req.
	r := Route{GenTokensPerSecond: 100, GenTokensPerSecondAtCapacity: 40, RecommendedConcurrency: 4}
	r.CurrentLoad = 2
	if got := effectiveGenTPS(r); got != 80 {
		t.Fatalf("k=2 => %v, want 80", got)
	}
	r.CurrentLoad = 4
	if got := effectiveGenTPS(r); got != 40 {
		t.Fatalf("k=4 => %v, want 40", got)
	}
	r.CurrentLoad = 7 // extrapolate: 100-120 = -20 -> clamped 0
	if got := effectiveGenTPS(r); got != 0 {
		t.Fatalf("k=7 => %v, want 0 (clamped)", got)
	}
}

func TestMetricTiebreakLoadShrinksBonus(t *testing.T) {
	base := Route{Model: "m", Healthy: true, TelemetryAt: time.Now(), GenTokensPerSecond: 100, GenTokensPerSecondAtCapacity: 20, RecommendedConcurrency: 4}
	idle := base
	idle.CurrentLoad = 1
	loaded := base
	loaded.CurrentLoad = 4
	si, _ := Score(idle, "m", time.Now())
	sl, _ := Score(loaded, "m", time.Now())
	if !(si > sl) {
		t.Fatalf("idle score %v should exceed loaded score %v (effective-speed shrinks under load)", si, sl)
	}
}

func TestScoreNoOpWhenNoCapacityMetrics(t *testing.T) {
	r0 := Route{Model: "m", Healthy: true, TelemetryAt: time.Now(), GenTokensPerSecond: 100}
	r8 := r0
	r8.CurrentLoad = 8
	s0, _ := Score(r0, "m", time.Now())
	s8, _ := Score(r8, "m", time.Now())
	if s0 != s8 {
		t.Fatalf("no-capacity-metrics score must not depend on load: %v vs %v", s0, s8)
	}
}
