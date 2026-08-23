// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"math"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/usage"
	"sort"
	"time"
)

// maxSampleGap bounds how far apart two consecutive power samples (or a
// sample and a request-window boundary) may be for Tier 1 ("measured")
// coverage of that boundary/gap to still count as continuous. The
// ServerAgent reports telemetry on a 1s cadence (P2 T1), so 2x that cadence
// tolerates one missed tick without silently degrading a measured attribution
// into a wrong one; a bigger gap falls through to Tier 2/3 instead.
const maxSampleGap = 2 * time.Second

// ServerEnergyConfig carries the per-server, operator-set energy knobs
// (mirroring routing.AIServer.EstimatedWatts/IdleWatts/Pue) that this pure
// engine needs. It exists so ComputeEnergy doesn't need to import/depend on
// the full AIServer shape; a caller typically builds one directly from a
// routing.AIServer.
//
// NOTE: IdleWatts is carried here for symmetry with routing.AIServer (and so
// a caller can round-trip one), but ComputeEnergy itself takes the EFFECTIVE
// idle wattage to use for the marginal calculation as an explicit `idleW`
// parameter — resolving "cfg.IdleWatts, else a system-wide default" is left
// to the caller (the same way a system-wide PUE default, if any, should be
// baked into cfg.Pue before calling — see effectivePue, which IS applied
// inside this engine because ComputeEnergy has no separate `pue` argument).
type ServerEnergyConfig struct {
	EstimatedWatts float64
	IdleWatts      float64
	Pue            float64
}

// EnergyResult is one usage event's attributed energy, as computed by
// ComputeEnergy.
type EnergyResult struct {
	// WhTotal is the event's attributed share of the server's total power
	// draw over its request window, in watt-hours.
	WhTotal float64
	// WhMarginal is the same share, but of power draw ABOVE the server's
	// idle baseline (max(0, power-idle)) — the energy this request cost
	// beyond simply keeping the server powered on. WhMarginal <= WhTotal by
	// construction: idle subtraction only ever shrinks the integrand.
	WhMarginal float64
	// Source records which tier produced the result: "measured" (Tier 1,
	// from real per-server power telemetry), "estimated" (Tier 2, from the
	// server's configured EstimatedWatts), or "modeled" (Tier 3, from a
	// Wh-per-output-token coefficient). ComputeEnergy always sets one of
	// these three — there is no "unknown"/empty Source.
	Source string
}

// interval is a half-open [from,to) span of wall-clock time during which n
// requests (the target plus its concurrently-running siblings) were being
// served by the server. Produced by concurrencyBreakpoints, consumed by
// ComputeEnergy's Tier 1/2 integration as the "shared cost" divisor.
type interval struct {
	from, to time.Time
	n        int
}

// powerStep is a half-open [from,to) span of wall-clock time during which a
// server's total power draw (after PUE) held constant at watts, per a
// step-interpolated reading of routing.TelemetrySample history. Produced by
// buildPowerSteps, consumed by ComputeEnergy's Tier 1 integration.
type powerStep struct {
	from, to time.Time
	watts    float64
}

// effectivePue resolves the datacenter Power Usage Effectiveness multiplier
// to apply on top of a server's own power draw: the server's own configured
// value if set (>0), else the system-wide default if set (>0), else 1.0 (no
// PUE overhead assumed). A non-positive value in either input (0, negative,
// or — defensively — non-finite) is treated as "unset" and falls through.
func effectivePue(cfg ServerEnergyConfig, sysDefaultPue float64) float64 {
	if v := cfg.Pue; v > 0 && !math.IsInf(v, 0) {
		return v
	}
	if v := sysDefaultPue; v > 0 && !math.IsInf(v, 0) {
		return v
	}
	return 1.0
}

// sanitizeWatts treats a NaN/±Inf/negative wattage reading as 0, so a single
// misbehaving sensor degrades gracefully instead of poisoning a sum.
func sanitizeWatts(w float64) float64 {
	if math.IsNaN(w) || math.IsInf(w, 0) || w < 0 {
		return 0
	}
	return w
}

// serverPowerW derives one telemetry sample's whole-server power draw in
// watts (before PUE), then applies pue. A positive, finite SystemPowerW (a
// real host-level power reading) is authoritative and wins outright over the
// GPU+CPU sum, which would otherwise double-count host overhead already
// folded into a system-level reading; any other SystemPowerW value (nil,
// non-positive, or non-finite) falls through to the GPU+CPU sum instead of
// short-circuiting to garbage. base==0 means "no usable power reading in
// this sample" — callers use this to decide Tier 1 coverage.
func serverPowerW(sample routing.TelemetrySample, pue float64) float64 {
	if sample.SystemPowerW != nil {
		if v := *sample.SystemPowerW; v > 0 && !math.IsInf(v, 0) {
			return v * pue
		}
	}
	var base float64
	for _, g := range sample.GPUs {
		base += sanitizeWatts(g.PowerW)
	}
	if sample.CPUPowerW != nil {
		base += sanitizeWatts(*sample.CPUPowerW)
	}
	return base * pue
}

// concurrencyBreakpoints computes the piecewise-constant concurrency N(t) —
// how many requests the server was serving — across [start,end], the target
// event's own request window, for use as the "shared cost" divisor in
// ComputeEnergy's Tier 1/2 integration.
//
// The target's own request counts across the WHOLE of [start,end] (N is
// never < 1, even with no siblings). siblings must be every OTHER event on
// the same server whose own window [sib.CreatedAt-latency, sib.CreatedAt]
// could overlap [start,end] — it must NOT include the target event itself.
// (A caller sourcing candidates from routing.Store's
// UsageEventsForServerWindow, which returns every event overlapping
// [start,end] INCLUDING the target, must filter the target out by ID before
// calling this.) Only the portion of a sibling's window that falls strictly
// within (start,end) contributes a concurrency boundary; a sibling's window
// touching start/end exactly at a single point, or lying entirely outside
// [start,end], contributes nothing.
//
// Returns intervals covering exactly [start,end], contiguous and ordered,
// each with n>=1. Returns nil if end<=start (no real window to breakpoint).
func concurrencyBreakpoints(start, end time.Time, siblings []usage.Event) []interval {
	if !end.After(start) {
		return nil
	}

	cuts := make([]time.Time, 0, len(siblings)*2)
	for _, sib := range siblings {
		sStart, sEnd := siblingWindow(sib)
		if sStart.After(start) && sStart.Before(end) {
			cuts = append(cuts, sStart)
		}
		if sEnd.After(start) && sEnd.Before(end) {
			cuts = append(cuts, sEnd)
		}
	}
	sort.Slice(cuts, func(i, j int) bool { return cuts[i].Before(cuts[j]) })

	bounds := make([]time.Time, 0, len(cuts)+2)
	bounds = append(bounds, start)
	bounds = append(bounds, cuts...)
	bounds = append(bounds, end)

	out := make([]interval, 0, len(bounds))
	for i := 0; i < len(bounds)-1; i++ {
		a, b := bounds[i], bounds[i+1]
		if !b.After(a) {
			continue // a duplicate/zero-width cut coincident with a boundary
		}
		mid := a.Add(b.Sub(a) / 2)
		n := 1 // the target itself, always present
		for _, sib := range siblings {
			sStart, sEnd := siblingWindow(sib)
			if mid.After(sStart) && mid.Before(sEnd) {
				n++
			}
		}
		out = append(out, interval{from: a, to: b, n: n})
	}
	return out
}

// siblingWindow returns a usage.Event's own request window
// [createdAt-latency, createdAt].
func siblingWindow(ev usage.Event) (time.Time, time.Time) {
	return ev.CreatedAt.Add(-time.Duration(ev.LatencyMS) * time.Millisecond), ev.CreatedAt
}

// ComputeEnergy computes one usage event's attributed energy via a tiered
// hybrid, trying each tier in order and always producing a result (Source is
// never empty, so a caller never needs its own final fallback):
//
//   - Tier 1 ("measured"): real per-server power telemetry (samples), when it
//     gives continuous coverage of the event's [start,end] request window.
//   - Tier 2 ("estimated"): the server's configured EstimatedWatts (cfg),
//     when Tier 1 doesn't apply and EstimatedWatts is set.
//   - Tier 3 ("modeled"): a Wh-per-output-token coefficient (mappingCoeff,
//     falling back to sysDefaultWhPerToken), when neither above applies.
//
// start/end are derived from ev as [CreatedAt-LatencyMS, CreatedAt] (request
// END minus its duration). A zero or negative LatencyMS yields no real
// window (end<=start): Tier 1 is always skipped in that case (there is
// nothing to integrate over), and Tier 2 still runs — producing WhTotal=0,
// Source="estimated" — before falling to Tier 3 if EstimatedWatts is unset.
//
// samples is the server's power-telemetry history; ComputeEnergy sorts a
// copy internally, so caller order doesn't matter. siblings is every OTHER
// usage.Event on ev's server whose own window could overlap ev's — it must
// NOT include ev itself (see concurrencyBreakpoints). cfg carries the
// server's EstimatedWatts/Pue (IdleWatts is carried for symmetry but not
// read here — see the ServerEnergyConfig doc); idleW is the EFFECTIVE idle
// draw, in watts, to subtract for the marginal figure. mappingCoeff is the
// mapping's own Wh/output-token coefficient (<=0 = unset, falls back to
// sysDefaultWhPerToken).
func ComputeEnergy(ev usage.Event, samples []routing.TelemetrySample, siblings []usage.Event, cfg ServerEnergyConfig, idleW, mappingCoeff, sysDefaultWhPerToken float64) EnergyResult {
	start := ev.CreatedAt.Add(-time.Duration(ev.LatencyMS) * time.Millisecond)
	end := ev.CreatedAt
	pue := effectivePue(cfg, 0)

	if end.After(start) {
		if res, ok := measuredEnergy(start, end, samples, siblings, pue, idleW); ok {
			return res
		}
	}
	if cfg.EstimatedWatts > 0 {
		return estimatedEnergy(start, end, siblings, cfg.EstimatedWatts, idleW)
	}
	return modeledEnergy(ev, mappingCoeff, sysDefaultWhPerToken)
}

// measuredEnergy attempts Tier 1: it sorts a copy of samples, checks that
// they give gapless coverage of [start,end] with at least one positive
// power reading in range, and — only if so — integrates power/concurrency
// over the window. ok is false whenever Tier 1 does not apply (no samples, a
// coverage gap wider than maxSampleGap, or every in-range reading is 0),
// in which case res is the zero value and the caller falls through to
// Tier 2/3.
func measuredEnergy(start, end time.Time, samples []routing.TelemetrySample, siblings []usage.Event, pue, idleW float64) (EnergyResult, bool) {
	if len(samples) == 0 {
		return EnergyResult{}, false
	}
	sorted := append([]routing.TelemetrySample(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ReportedAt.Before(sorted[j].ReportedAt) })

	if !tier1CoverageOK(start, end, sorted) {
		return EnergyResult{}, false
	}

	steps := buildPowerSteps(start, end, sorted, pue)
	if len(steps) == 0 {
		return EnergyResult{}, false
	}
	anyPositive := false
	for _, st := range steps {
		if st.watts > 0 {
			anyPositive = true
			break
		}
	}
	if !anyPositive {
		return EnergyResult{}, false
	}

	cc := concurrencyBreakpoints(start, end, siblings)
	total, marginal := integrateSteps(steps, cc, idleW)
	return EnergyResult{
		WhTotal:    cleanNonNeg(total),
		WhMarginal: cleanNonNeg(marginal),
		Source:     "measured",
	}, true
}

// tier1CoverageOK reports whether sorted (already sorted ascending by
// ReportedAt, non-empty) covers [start,end] with no gap wider than
// maxSampleGap: the lead gap (start to the first sample), the trail gap (the
// last sample to end), and every internal gap between consecutive samples
// that overlaps [start,end] at all.
func tier1CoverageOK(start, end time.Time, sorted []routing.TelemetrySample) bool {
	n := len(sorted)
	if gap := sorted[0].ReportedAt.Sub(start); gap > maxSampleGap {
		return false
	}
	if gap := end.Sub(sorted[n-1].ReportedAt); gap > maxSampleGap {
		return false
	}
	for i := 0; i < n-1; i++ {
		a, b := sorted[i].ReportedAt, sorted[i+1].ReportedAt
		if a.Before(end) && b.After(start) { // this gap overlaps the window
			if gap := b.Sub(a); gap > maxSampleGap {
				return false
			}
		}
	}
	return true
}

// buildPowerSteps step-interpolates sorted telemetry samples into a list of
// powerStep, clipped to [start,end]: sample i's power holds from its own
// ReportedAt until the next sample's ReportedAt (the first sample's power is
// extended back to start, the last sample's power is extended forward to
// end — the caller has already verified via tier1CoverageOK that those
// extensions are within maxSampleGap). A degenerate (zero-width, after
// clipping) step is dropped.
func buildPowerSteps(start, end time.Time, sorted []routing.TelemetrySample, pue float64) []powerStep {
	n := len(sorted)
	steps := make([]powerStep, 0, n)
	for i := 0; i < n; i++ {
		from := sorted[i].ReportedAt
		var to time.Time
		if i+1 < n {
			to = sorted[i+1].ReportedAt
		} else {
			to = end
		}
		if i == 0 {
			from = start
		}
		if from.Before(start) {
			from = start
		}
		if to.After(end) {
			to = end
		}
		if !to.After(from) {
			continue
		}
		steps = append(steps, powerStep{from: from, to: to, watts: serverPowerW(sorted[i], pue)})
	}
	return steps
}

// integrateSteps merges the power partition (steps) and the concurrency
// partition (cc) into their common set of boundaries, then, for every
// resulting sub-interval, integrates power/n (total) and
// max(0,power-idleW)/n (marginal) over its duration.
func integrateSteps(steps []powerStep, cc []interval, idleW float64) (total, marginal float64) {
	bounds := make([]time.Time, 0, len(steps)*2+len(cc)*2)
	for _, s := range steps {
		bounds = append(bounds, s.from, s.to)
	}
	for _, c := range cc {
		bounds = append(bounds, c.from, c.to)
	}
	sort.Slice(bounds, func(i, j int) bool { return bounds[i].Before(bounds[j]) })

	uniq := make([]time.Time, 0, len(bounds))
	for i, b := range bounds {
		if i == 0 || !b.Equal(bounds[i-1]) {
			uniq = append(uniq, b)
		}
	}

	for i := 0; i < len(uniq)-1; i++ {
		a, b := uniq[i], uniq[i+1]
		dt := b.Sub(a).Seconds()
		if dt <= 0 {
			continue
		}
		p := powerAt(steps, a, b)
		n := concurrencyAt(cc, a, b)
		if n < 1 {
			n = 1
		}
		total += (p / float64(n)) * dt / 3600
		marginal += (math.Max(0, p-idleW) / float64(n)) * dt / 3600
	}
	return total, marginal
}

// powerAt returns the watts of the powerStep containing the midpoint of
// [a,b) (every sub-interval produced by integrateSteps lies entirely within
// exactly one step, by construction of its shared boundaries). 0 if none
// matches (should not happen given that construction; a safe default).
func powerAt(steps []powerStep, a, b time.Time) float64 {
	mid := a.Add(b.Sub(a) / 2)
	for _, s := range steps {
		if !mid.Before(s.from) && mid.Before(s.to) {
			return s.watts
		}
	}
	return 0
}

// concurrencyAt returns the n of the interval containing the midpoint of
// [a,b), mirroring powerAt. 1 if none matches (a safe default — the target
// alone).
func concurrencyAt(cc []interval, a, b time.Time) int {
	mid := a.Add(b.Sub(a) / 2)
	for _, c := range cc {
		if !mid.Before(c.from) && mid.Before(c.to) {
			return c.n
		}
	}
	return 1
}

// estimatedEnergy implements Tier 2: the same concurrency-shared integration
// as Tier 1, but over a flat `watts` for the whole window instead of a
// per-sample power curve. A degenerate window (end<=start) yields dt=0 for
// every interval (concurrencyBreakpoints returns nil), so WhTotal/WhMarginal
// are both 0 — Source is still "estimated" (Tier 2 ran; it just integrated
// over nothing).
func estimatedEnergy(start, end time.Time, siblings []usage.Event, watts, idleW float64) EnergyResult {
	cc := concurrencyBreakpoints(start, end, siblings)
	var total, marginal float64
	for _, c := range cc {
		dt := c.to.Sub(c.from).Seconds()
		n := c.n
		if n < 1 {
			n = 1
		}
		total += (watts / float64(n)) * dt / 3600
		marginal += (math.Max(0, watts-idleW) / float64(n)) * dt / 3600
	}
	return EnergyResult{
		WhTotal:    cleanNonNeg(total),
		WhMarginal: cleanNonNeg(marginal),
		Source:     "estimated",
	}
}

// modeledEnergy implements Tier 3: WhTotal = WhMarginal = coeff *
// ev.OutputTokens, where coeff is mappingCoeff if positive, else
// sysDefaultWhPerToken (which may itself be 0 — a coeff of 0 still produces
// Source="modeled" with WhTotal=0, rather than no result at all, so a
// reconciler can stamp the event once and move on).
func modeledEnergy(ev usage.Event, mappingCoeff, sysDefaultWhPerToken float64) EnergyResult {
	coeff := mappingCoeff
	if coeff <= 0 {
		coeff = sysDefaultWhPerToken
	}
	wh := cleanNonNeg(coeff * float64(ev.OutputTokens))
	return EnergyResult{WhTotal: wh, WhMarginal: wh, Source: "modeled"}
}

// cleanNonNeg runs a computed watt-hour figure through cleanFloat (NaN/±Inf
// -> 0, defined in benchmark_runner.go) and additionally floors a negative
// result to 0. ComputeEnergy's tiers are constructed to never go negative on
// sane inputs, but this keeps a future change (or a pathological input, e.g.
// negative OutputTokens) from ever surfacing a negative energy figure.
func cleanNonNeg(v float64) float64 {
	v = cleanFloat(v)
	if v < 0 {
		return 0
	}
	return v
}
