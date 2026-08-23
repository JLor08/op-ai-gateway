// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"math"
	"time"
)

const (
	baseScore              = 1000.0
	priorityMultiplier     = 20.0
	activeRequestsPenalty  = 25.0
	queueDepthPenalty      = 20.0
	latencyMSPenalty       = 0.2
	errorRatePenalty       = 200.0
	staleTelemetryPenalty  = 500.0
	staleTelemetryDuration = 2 * time.Minute

	// Per-model metric tiebreak (P4a). Folded into the score AFTER the health/load
	// viability gate and BOUNDED (each throughput term clamped, MTP a flat bonus) so
	// speed/MTP refine the ranking among already-viable candidates — competing with
	// priority/weight — without ever overriding a health/load penalty or rescuing a
	// non-viable server. The total tiebreak is <= 100 points, well below
	// errorRatePenalty (200) and staleTelemetryPenalty (500), and ~5 priority levels
	// (priorityMultiplier). Weights are tunable. All-zero metrics contribute 0 (the
	// no-op invariant: an unmeasured mapping scores exactly as before P4).
	genThroughputWeight      = 0.25 // points per generation tok/s ...
	genThroughputBonusCap    = 50.0 // ... clamped here (cap reached at 200 tok/s)
	promptThroughputWeight   = 0.01 // points per prefill tok/s ...
	promptThroughputBonusCap = 20.0 // ... clamped here (cap reached at 2000 tok/s)
	mtpBonus                 = 30.0 // flat bonus for an MTP model
)

type Route struct {
	ID             string
	Model          string
	Provider       string
	Endpoint       string
	Priority       int
	Weight         int
	Healthy        bool
	ActiveRequests int
	QueueDepth     int
	LatencyMS      int
	ErrorRate      float64
	TelemetryAt    time.Time

	GenTokensPerSecond    float64
	PromptTokensPerSecond float64
	IsMTP                 bool

	// Capacity metrics (CP1/CP2) + the current server load, used by the effective-speed
	// tiebreak. All zero => the flat P4a behaviour (the no-op invariant).
	RecommendedConcurrency       int     // latency-knee concurrency (>=1 when known)
	MaxConcurrency               int     // OOM-safe ceiling (0 = unknown)
	GenTokensPerSecondAtCapacity float64 // per-request gen tok/s at RecommendedConcurrency
	CurrentLoad                  int     // in-flight requests on this route's server (k)
}

// scoringRoute builds the Route used to score one candidate against `model`, given its
// server telemetry (tel, with hasTel reporting whether telemetry was found) and the
// current load k (in-flight requests, from activity tracking when available, else
// tel.ActiveRequests). Shared by argmaxByScore and ScoreModelServers so the two callers
// score identically off the same field assembly.
func scoringRoute(c MappingCandidate, tel ServerTelemetry, hasTel bool, k int, model string) Route {
	route := Route{
		ID:             c.Mapping.ID,
		Model:          model,
		Provider:       c.Application.Type,
		Endpoint:       ApplicationEndpoint(c.Server, c.Application),
		Priority:       c.Application.Priority,
		Weight:         c.Application.Weight,
		Healthy:        true,
		ActiveRequests: tel.ActiveRequests,
		QueueDepth:     tel.QueueDepth,
		LatencyMS:      tel.LatencyMS,
		ErrorRate:      tel.ErrorRate,

		GenTokensPerSecond:    c.Mapping.GenTokensPerSecond,
		PromptTokensPerSecond: c.Mapping.PromptTokensPerSecond,
		IsMTP:                 c.Mapping.IsMTP,

		RecommendedConcurrency:       c.Mapping.RecommendedConcurrency,
		MaxConcurrency:               c.Mapping.MaxConcurrency,
		GenTokensPerSecondAtCapacity: c.Mapping.GenTokensPerSecondAtCapacity,
		CurrentLoad:                  k,
	}
	if hasTel {
		route.TelemetryAt = tel.ReportedAt
	}
	return route
}

func Select(routes []Route, model string, now time.Time) (Route, float64, bool) {
	var best Route
	var bestScore float64
	found := false
	for _, route := range routes {
		score, ok := Score(route, model, now)
		if !ok {
			continue
		}
		if !found || score > bestScore {
			best = route
			bestScore = score
			found = true
		}
	}
	return best, bestScore, found
}

func Score(route Route, model string, now time.Time) (float64, bool) {
	if route.Model != model || !route.Healthy {
		return 0, false
	}
	if !validTelemetry(route) {
		return 0, false
	}
	score := baseScore
	score += float64(route.Priority) * priorityMultiplier
	score += float64(route.Weight)
	score -= float64(route.ActiveRequests) * activeRequestsPenalty
	score -= float64(route.QueueDepth) * queueDepthPenalty
	score -= float64(route.LatencyMS) * latencyMSPenalty
	score -= route.ErrorRate * errorRatePenalty
	if route.TelemetryAt.IsZero() || now.Sub(route.TelemetryAt) > staleTelemetryDuration {
		score -= staleTelemetryPenalty
	}
	// Health/load/staleness viability gate FIRST: a candidate over-degraded on these
	// live signals is non-viable regardless of how fast it benchmarked. The bounded
	// speed/MTP tiebreak below applies only to an already-viable candidate and must
	// never rescue a dead one (which would also defeat the prefer-loaded fail-open).
	if score <= 0 {
		return 0, false
	}
	score += metricTiebreak(route)
	return score, true
}

// effectiveGenTPS models a route's per-request generation speed at its CURRENT load
// (CurrentLoad = k in-flight). It is piecewise-linear: gen_tokens_per_second at
// concurrency 1, gen_tokens_per_second_at_capacity at recommended_concurrency, linearly
// interpolated between and extrapolated beyond with the same slope (clamped >= 0).
// Unknown metrics collapse to the flat gen_tokens_per_second (the no-op invariant): an
// unmeasured mapping (GenTokensPerSecondAtCapacity <= 0 or RecommendedConcurrency <= 1)
// or an idle/unknown load (k <= 1) yields exactly GenTokensPerSecond, so metricTiebreak
// stays byte-identical to P4a.
func effectiveGenTPS(route Route) float64 {
	g1 := route.GenTokensPerSecond
	if g1 <= 0 {
		return g1 // unknown speed => 0, no bonus (matches clampBonus's v<=0 guard)
	}
	gCap := route.GenTokensPerSecondAtCapacity
	rec := route.RecommendedConcurrency
	if gCap <= 0 || rec <= 1 || route.CurrentLoad <= 1 {
		return g1 // no capacity curve, or idle => flat single-request speed
	}
	slope := (gCap - g1) / float64(rec-1) // per-extra-concurrent-request change (usually negative)
	eff := g1 + slope*float64(route.CurrentLoad-1)
	if eff < 0 {
		eff = 0
	}
	return eff
}

// metricTiebreak returns a bounded refinement (<= genThroughputBonusCap +
// promptThroughputBonusCap + mtpBonus) from a mapping's measured speed/MTP metrics. Each
// throughput term is clamped so an implausibly large stored value cannot dominate;
// all-zero metrics return 0 (the no-op invariant). The generation-throughput term is now
// load-aware: it uses effectiveGenTPS(route), which shrinks the single-request gen tok/s
// toward gen_tokens_per_second_at_capacity as the route's server fills (CurrentLoad rises).
// With no capacity curve or an idle server it collapses to the flat P4a gen value.
func metricTiebreak(route Route) float64 {
	bonus := clampBonus(effectiveGenTPS(route), genThroughputWeight, genThroughputBonusCap)
	bonus += clampBonus(route.PromptTokensPerSecond, promptThroughputWeight, promptThroughputBonusCap)
	if route.IsMTP {
		bonus += mtpBonus
	}
	return bonus
}

// clampBonus returns min(v*weight, capValue) for a finite v > 0, else 0. Guards NaN/Inf and
// negative inputs defensively — metrics are validated non-negative at the write boundary,
// but the scorer must not let a corrupt value poison the argmax comparison.
func clampBonus(v, weight, capValue float64) float64 {
	if math.IsNaN(v) || v <= 0 {
		return 0
	}
	b := v * weight
	if b > capValue { // also true for +Inf
		return capValue
	}
	return b
}

func validTelemetry(route Route) bool {
	if route.ActiveRequests < 0 || route.QueueDepth < 0 || route.LatencyMS < 0 {
		return false
	}
	if math.IsNaN(route.ErrorRate) || math.IsInf(route.ErrorRate, 0) {
		return false
	}
	if route.ErrorRate < 0 || route.ErrorRate > 1 {
		return false
	}
	return true
}
