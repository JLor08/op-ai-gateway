// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"math"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/usage"
	"testing"
	"time"
)

const whTol = 1e-9

func almostEqual(a, b, tol float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= tol
}

func requireCloseWh(t *testing.T, label string, got, want float64) {
	t.Helper()
	if math.IsNaN(got) || math.IsInf(got, 0) {
		t.Fatalf("%s: got non-finite value %v", label, got)
	}
	if !almostEqual(got, want, whTol) {
		t.Fatalf("%s: got %v, want %v (diff %v)", label, got, want, got-want)
	}
}

func fptr(v float64) *float64 { return &v }

// constPowerSamples builds one telemetry sample per second (a dense, 1s-cadence
// series matching the ServerAgent's real cadence) from base to base+seconds,
// inclusive on both ends, each reporting a single GPU at the given wattage.
func constPowerSamples(base time.Time, seconds int, watts float64) []routing.TelemetrySample {
	out := make([]routing.TelemetrySample, 0, seconds+1)
	for i := 0; i <= seconds; i++ {
		out = append(out, routing.TelemetrySample{
			ReportedAt: base.Add(time.Duration(i) * time.Second),
			GPUs:       []routing.GPUSample{{PowerW: watts}},
		})
	}
	return out
}

// sampleAt builds a single telemetry sample at base+offset seconds reporting a
// single GPU at the given wattage.
func sampleAt(base time.Time, offsetSec int, watts float64) routing.TelemetrySample {
	return routing.TelemetrySample{
		ReportedAt: base.Add(time.Duration(offsetSec) * time.Second),
		GPUs:       []routing.GPUSample{{PowerW: watts}},
	}
}

// siblingEvent builds a usage.Event whose own request window is
// [createdAt-latency, createdAt].
func siblingEvent(createdAt time.Time, latency time.Duration) usage.Event {
	return usage.Event{CreatedAt: createdAt, LatencyMS: latency.Milliseconds()}
}

// ---------------------------------------------------------------------------
// effectivePue
// ---------------------------------------------------------------------------

func TestEnergyEffectivePue(t *testing.T) {
	cases := []struct {
		name          string
		cfgPue        float64
		sysDefaultPue float64
		want          float64
	}{
		{"server value wins", 1.3, 1.1, 1.3},
		{"falls back to system default", 0, 1.4, 1.4},
		{"falls back to 1.0 when both unset", 0, 0, 1.0},
		{"negative server value treated as unset", -1, 1.2, 1.2},
		{"negative everything falls to 1.0", -1, -2, 1.0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := effectivePue(ServerEnergyConfig{Pue: c.cfgPue}, c.sysDefaultPue)
			if got != c.want {
				t.Fatalf("effectivePue(pue=%v, sysDefault=%v) = %v, want %v", c.cfgPue, c.sysDefaultPue, got, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// serverPowerW
// ---------------------------------------------------------------------------

func TestEnergyServerPowerW(t *testing.T) {
	cases := []struct {
		name   string
		sample routing.TelemetrySample
		pue    float64
		want   float64
	}{
		{
			name: "system power wins over gpu+cpu sum",
			sample: routing.TelemetrySample{
				SystemPowerW: fptr(300),
				GPUs:         []routing.GPUSample{{PowerW: 50}},
				CPUPowerW:    fptr(20),
			},
			pue:  1.0,
			want: 300,
		},
		{
			name: "gpu+cpu sum when system nil",
			sample: routing.TelemetrySample{
				GPUs:      []routing.GPUSample{{PowerW: 50}, {PowerW: 30}},
				CPUPowerW: fptr(20),
			},
			pue:  1.0,
			want: 100,
		},
		{
			name:   "all-zero sample -> 0",
			sample: routing.TelemetrySample{},
			pue:    1.0,
			want:   0,
		},
		{
			name: "negative/NaN GPU and +Inf CPU ignored",
			sample: routing.TelemetrySample{
				GPUs:      []routing.GPUSample{{PowerW: -10}, {PowerW: math.NaN()}, {PowerW: 40}},
				CPUPowerW: fptr(math.Inf(1)),
			},
			pue:  1.0,
			want: 40,
		},
		{
			name: "pue multiplies the gpu+cpu sum",
			sample: routing.TelemetrySample{
				GPUs: []routing.GPUSample{{PowerW: 50}},
			},
			pue:  1.5,
			want: 75,
		},
		{
			name: "pue multiplies a system reading too",
			sample: routing.TelemetrySample{
				SystemPowerW: fptr(200),
			},
			pue:  1.5,
			want: 300,
		},
		{
			name: "zero system power falls through to gpu+cpu sum",
			sample: routing.TelemetrySample{
				SystemPowerW: fptr(0),
				GPUs:         []routing.GPUSample{{PowerW: 60}},
			},
			pue:  1.0,
			want: 60,
		},
		{
			name: "negative system power falls through to gpu+cpu sum",
			sample: routing.TelemetrySample{
				SystemPowerW: fptr(-5),
				GPUs:         []routing.GPUSample{{PowerW: 30}},
				CPUPowerW:    fptr(5),
			},
			pue:  1.0,
			want: 35,
		},
		{
			name: "+Inf system power falls through to gpu+cpu sum",
			sample: routing.TelemetrySample{
				SystemPowerW: fptr(math.Inf(1)),
				GPUs:         []routing.GPUSample{{PowerW: 25}},
			},
			pue:  1.0,
			want: 25,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := serverPowerW(c.sample, c.pue)
			if got != c.want {
				t.Fatalf("serverPowerW() = %v, want %v", got, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// concurrencyBreakpoints
// ---------------------------------------------------------------------------

func TestEnergyConcurrencyBreakpointsSolo(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	start := base
	end := base.Add(10 * time.Second)

	got := concurrencyBreakpoints(start, end, nil)
	if len(got) != 1 {
		t.Fatalf("len(intervals) = %d, want 1: %+v", len(got), got)
	}
	if !got[0].from.Equal(start) || !got[0].to.Equal(end) || got[0].n != 1 {
		t.Fatalf("interval = %+v, want {from:%v to:%v n:1}", got[0], start, end)
	}
}

func TestEnergyConcurrencyBreakpointsFullOverlap(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	start := base
	end := base.Add(10 * time.Second)

	// Sibling window starts well before `start` and ends well after `end`,
	// so it overlaps [start,end] in full and introduces no interior cut.
	sib := siblingEvent(end.Add(100*time.Second), 200*time.Second)

	got := concurrencyBreakpoints(start, end, []usage.Event{sib})
	if len(got) != 1 {
		t.Fatalf("len(intervals) = %d, want 1: %+v", len(got), got)
	}
	if !got[0].from.Equal(start) || !got[0].to.Equal(end) || got[0].n != 2 {
		t.Fatalf("interval = %+v, want {from:%v to:%v n:2}", got[0], start, end)
	}
}

func TestEnergyConcurrencyBreakpointsHalfOverlap(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	start := base
	end := base.Add(10 * time.Second)
	mid := base.Add(5 * time.Second)

	// Sibling covers exactly the first half: [start, mid].
	sib := siblingEvent(mid, 5*time.Second)

	got := concurrencyBreakpoints(start, end, []usage.Event{sib})
	if len(got) != 2 {
		t.Fatalf("len(intervals) = %d, want 2: %+v", len(got), got)
	}
	if !got[0].from.Equal(start) || !got[0].to.Equal(mid) || got[0].n != 2 {
		t.Fatalf("interval[0] = %+v, want {from:%v to:%v n:2}", got[0], start, mid)
	}
	if !got[1].from.Equal(mid) || !got[1].to.Equal(end) || got[1].n != 1 {
		t.Fatalf("interval[1] = %+v, want {from:%v to:%v n:1}", got[1], mid, end)
	}
}

func TestEnergyConcurrencyBreakpointsNestedPartial(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	start := base
	end := base.Add(10 * time.Second)

	// A: nested fully inside [start,end] -> [2s, 8s].
	a := siblingEvent(base.Add(8*time.Second), 6*time.Second)
	// B: starts inside, extends past the window end -> [5s, end+3s].
	b := siblingEvent(end.Add(3*time.Second), 8*time.Second)

	got := concurrencyBreakpoints(start, end, []usage.Event{a, b})

	want := []struct {
		fromSec, toSec, n int
	}{
		{0, 2, 1},
		{2, 5, 2},
		{5, 8, 3},
		{8, 10, 2},
	}
	if len(got) != len(want) {
		t.Fatalf("len(intervals) = %d, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		wantFrom := base.Add(time.Duration(w.fromSec) * time.Second)
		wantTo := base.Add(time.Duration(w.toSec) * time.Second)
		if !got[i].from.Equal(wantFrom) || !got[i].to.Equal(wantTo) || got[i].n != w.n {
			t.Fatalf("interval[%d] = %+v, want {from:%v to:%v n:%d}", i, got[i], wantFrom, wantTo, w.n)
		}
	}
}

func TestEnergyConcurrencyBreakpointsDegenerateWindow(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	if got := concurrencyBreakpoints(base, base, nil); got != nil {
		t.Fatalf("zero-width window: got %+v, want nil", got)
	}
	if got := concurrencyBreakpoints(base, base.Add(-time.Second), nil); got != nil {
		t.Fatalf("negative window: got %+v, want nil", got)
	}
}

// ---------------------------------------------------------------------------
// ComputeEnergy - Tier 1 (measured)
// ---------------------------------------------------------------------------

func TestEnergyComputeTier1ConstantSolo(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	start := base
	end := base.Add(36 * time.Second)
	ev := usage.Event{CreatedAt: end, LatencyMS: 36000, OutputTokens: 1000}
	samples := constPowerSamples(start, 36, 100)
	cfg := ServerEnergyConfig{}
	idleW := 40.0

	res := ComputeEnergy(ev, samples, nil, cfg, idleW, 0, 0)
	if res.Source != "measured" {
		t.Fatalf("Source = %q, want %q (res=%+v)", res.Source, "measured", res)
	}
	requireCloseWh(t, "WhTotal", res.WhTotal, 1.0)
	requireCloseWh(t, "WhMarginal", res.WhMarginal, 0.6)
	if res.WhMarginal > res.WhTotal+whTol {
		t.Fatalf("WhMarginal (%v) > WhTotal (%v)", res.WhMarginal, res.WhTotal)
	}
}

func TestEnergyComputeTier1ConcurrencyHalvesShare(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	start := base
	end := base.Add(36 * time.Second)
	ev := usage.Event{CreatedAt: end, LatencyMS: 36000}
	samples := constPowerSamples(start, 36, 100)
	cfg := ServerEnergyConfig{}
	idleW := 40.0

	// A sibling whose window fully covers [start,end] -> N=2 throughout.
	sib := siblingEvent(end.Add(100*time.Second), 200*time.Second)

	res := ComputeEnergy(ev, samples, []usage.Event{sib}, cfg, idleW, 0, 0)
	if res.Source != "measured" {
		t.Fatalf("Source = %q, want %q", res.Source, "measured")
	}
	requireCloseWh(t, "WhTotal", res.WhTotal, 0.5)
	requireCloseWh(t, "WhMarginal", res.WhMarginal, 0.3)
}

func TestEnergyComputeTier1GapFallsBackToTier2(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	start := base
	end := base.Add(36 * time.Second)
	ev := usage.Event{CreatedAt: end, LatencyMS: 36000}

	// Dense 1s-cadence coverage for [0,15] and [19,36], leaving a 4s gap
	// between the samples at 15s and 19s - bigger than maxSampleGap (2s).
	samples := append(constPowerSamples(start, 15, 100), constPowerSamples(start.Add(19*time.Second), 17, 100)...)

	cfg := ServerEnergyConfig{EstimatedWatts: 50}
	idleW := 10.0

	res := ComputeEnergy(ev, samples, nil, cfg, idleW, 0, 0)
	if res.Source != "estimated" {
		t.Fatalf("Source = %q, want %q (a >maxSampleGap gap must fall through to Tier 2)", res.Source, "estimated")
	}
	requireCloseWh(t, "WhTotal", res.WhTotal, 50.0*36/3600)
	requireCloseWh(t, "WhMarginal", res.WhMarginal, 40.0*36/3600)
}

func TestEnergyComputeTier1GapAtExactlyThresholdStillMeasured(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	start := base
	end := base.Add(4 * time.Second)
	ev := usage.Event{CreatedAt: end, LatencyMS: 4000}

	// Samples exactly maxSampleGap (2s) apart - allowed (not >maxSampleGap).
	samples := []routing.TelemetrySample{
		sampleAt(start, 0, 100),
		sampleAt(start, 2, 100),
		sampleAt(start, 4, 100),
	}
	cfg := ServerEnergyConfig{EstimatedWatts: 999} // would prove Tier1 didn't fire if wrongly skipped
	idleW := 0.0

	res := ComputeEnergy(ev, samples, nil, cfg, idleW, 0, 0)
	if res.Source != "measured" {
		t.Fatalf("Source = %q, want %q (exactly-threshold gap must still be Tier 1)", res.Source, "measured")
	}
	requireCloseWh(t, "WhTotal", res.WhTotal, 100.0*4/3600)
}

func TestEnergyComputeTier1StepInterpolation(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	start := base
	end := base.Add(4 * time.Second)
	ev := usage.Event{CreatedAt: end, LatencyMS: 4000}

	// Power holds at 100W from [0,2), then steps to 200W for [2,4).
	samples := []routing.TelemetrySample{
		sampleAt(start, 0, 100),
		sampleAt(start, 2, 200),
		sampleAt(start, 4, 200),
	}
	cfg := ServerEnergyConfig{}
	idleW := 0.0

	res := ComputeEnergy(ev, samples, nil, cfg, idleW, 0, 0)
	if res.Source != "measured" {
		t.Fatalf("Source = %q, want %q", res.Source, "measured")
	}
	want := (100.0*2 + 200.0*2) / 3600
	requireCloseWh(t, "WhTotal", res.WhTotal, want)
	requireCloseWh(t, "WhMarginal", res.WhMarginal, want) // idle=0 -> marginal==total
}

func TestEnergyComputeTier1AllZeroPowerFallsThrough(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	start := base
	end := base.Add(10 * time.Second)
	ev := usage.Event{CreatedAt: end, LatencyMS: 10000}

	// Dense, gapless coverage, but every sample reports zero power (a
	// misconfigured/absent power sensor) -> Tier 1 must NOT claim "measured
	// 0 Wh"; it should fall through to Tier 2/3.
	samples := constPowerSamples(start, 10, 0)
	cfg := ServerEnergyConfig{EstimatedWatts: 30}
	idleW := 0.0

	res := ComputeEnergy(ev, samples, nil, cfg, idleW, 0, 0)
	if res.Source != "estimated" {
		t.Fatalf("Source = %q, want %q (all-zero power coverage must fall through)", res.Source, "estimated")
	}
	requireCloseWh(t, "WhTotal", res.WhTotal, 30.0*10/3600)
}

// ---------------------------------------------------------------------------
// ComputeEnergy - Tier 2 (estimated)
// ---------------------------------------------------------------------------

func TestEnergyComputeTier2FlatSolo(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	end := base.Add(10 * time.Second)
	ev := usage.Event{CreatedAt: end, LatencyMS: 10000}
	cfg := ServerEnergyConfig{EstimatedWatts: 80}
	idleW := 20.0

	res := ComputeEnergy(ev, nil, nil, cfg, idleW, 0, 0)
	if res.Source != "estimated" {
		t.Fatalf("Source = %q, want %q", res.Source, "estimated")
	}
	requireCloseWh(t, "WhTotal", res.WhTotal, 80.0*10/3600)
	requireCloseWh(t, "WhMarginal", res.WhMarginal, 60.0*10/3600)
}

func TestEnergyComputeTier2ConcurrencyHalvesShare(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	end := base.Add(10 * time.Second)
	ev := usage.Event{CreatedAt: end, LatencyMS: 10000}
	cfg := ServerEnergyConfig{EstimatedWatts: 80}
	idleW := 20.0
	sib := siblingEvent(end.Add(100*time.Second), 200*time.Second)

	res := ComputeEnergy(ev, nil, []usage.Event{sib}, cfg, idleW, 0, 0)
	if res.Source != "estimated" {
		t.Fatalf("Source = %q, want %q", res.Source, "estimated")
	}
	requireCloseWh(t, "WhTotal", res.WhTotal, 40.0*10/3600)
	requireCloseWh(t, "WhMarginal", res.WhMarginal, 30.0*10/3600)
}

func TestEnergyComputeMarginalFloorsAtZeroWhenIdleExceedsPower(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	start := base
	end := base.Add(10 * time.Second)

	t.Run("tier1", func(t *testing.T) {
		ev := usage.Event{CreatedAt: end, LatencyMS: 10000}
		samples := constPowerSamples(start, 10, 20) // 20W
		res := ComputeEnergy(ev, samples, nil, ServerEnergyConfig{}, 50 /* idle > power */, 0, 0)
		if res.Source != "measured" {
			t.Fatalf("Source = %q, want %q", res.Source, "measured")
		}
		requireCloseWh(t, "WhTotal", res.WhTotal, 20.0*10/3600)
		requireCloseWh(t, "WhMarginal", res.WhMarginal, 0)
	})

	t.Run("tier2", func(t *testing.T) {
		ev := usage.Event{CreatedAt: end, LatencyMS: 10000}
		res := ComputeEnergy(ev, nil, nil, ServerEnergyConfig{EstimatedWatts: 20}, 50, 0, 0)
		if res.Source != "estimated" {
			t.Fatalf("Source = %q, want %q", res.Source, "estimated")
		}
		requireCloseWh(t, "WhTotal", res.WhTotal, 20.0*10/3600)
		requireCloseWh(t, "WhMarginal", res.WhMarginal, 0)
	})
}

func TestEnergyComputeTier2ZeroDurationStillEstimated(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	ev := usage.Event{CreatedAt: base, LatencyMS: 0} // start==end, no real window
	cfg := ServerEnergyConfig{EstimatedWatts: 50}

	res := ComputeEnergy(ev, nil, nil, cfg, 10, 0, 0)
	if res.Source != "estimated" {
		t.Fatalf("Source = %q, want %q", res.Source, "estimated")
	}
	requireCloseWh(t, "WhTotal", res.WhTotal, 0)
	requireCloseWh(t, "WhMarginal", res.WhMarginal, 0)
}

// ---------------------------------------------------------------------------
// ComputeEnergy - Tier 3 (modeled)
// ---------------------------------------------------------------------------

func TestEnergyComputeTier3MappingCoeff(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	ev := usage.Event{CreatedAt: base.Add(5 * time.Second), LatencyMS: 5000, OutputTokens: 500}
	cfg := ServerEnergyConfig{} // no EstimatedWatts -> Tier 2 unavailable

	res := ComputeEnergy(ev, nil, nil, cfg, 0, 0.01, 0.05)
	if res.Source != "modeled" {
		t.Fatalf("Source = %q, want %q", res.Source, "modeled")
	}
	requireCloseWh(t, "WhTotal", res.WhTotal, 5.0)
	requireCloseWh(t, "WhMarginal", res.WhMarginal, 5.0)
}

func TestEnergyComputeTier3SysDefaultFallback(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	ev := usage.Event{CreatedAt: base.Add(5 * time.Second), LatencyMS: 5000, OutputTokens: 250}
	cfg := ServerEnergyConfig{}

	res := ComputeEnergy(ev, nil, nil, cfg, 0, 0, 0.02)
	if res.Source != "modeled" {
		t.Fatalf("Source = %q, want %q", res.Source, "modeled")
	}
	requireCloseWh(t, "WhTotal", res.WhTotal, 5.0)

	// A negative mapping coefficient is treated the same as "unset" -> falls
	// back to the system default too.
	res2 := ComputeEnergy(ev, nil, nil, cfg, 0, -1, 0.02)
	requireCloseWh(t, "WhTotal (negative coeff)", res2.WhTotal, 5.0)
}

func TestEnergyComputeTier3CoeffZeroStillModeled(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	ev := usage.Event{CreatedAt: base.Add(5 * time.Second), LatencyMS: 5000, OutputTokens: 100}
	cfg := ServerEnergyConfig{}

	res := ComputeEnergy(ev, nil, nil, cfg, 0, 0, 0)
	if res.Source != "modeled" {
		t.Fatalf("Source = %q, want %q (coeff==0 must still stamp Source, not skip it)", res.Source, "modeled")
	}
	requireCloseWh(t, "WhTotal", res.WhTotal, 0)
	requireCloseWh(t, "WhMarginal", res.WhMarginal, 0)
}

// ---------------------------------------------------------------------------
// Guards
// ---------------------------------------------------------------------------

func TestEnergyComputeGuardsNaNInfSampleIgnored(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	start := base
	end := base.Add(2 * time.Second)
	ev := usage.Event{CreatedAt: end, LatencyMS: 2000}

	samples := []routing.TelemetrySample{
		{
			ReportedAt: start,
			GPUs:       []routing.GPUSample{{PowerW: math.NaN()}, {PowerW: 100}},
			CPUPowerW:  fptr(math.Inf(1)),
		},
		sampleAt(start, 2, 100),
	}
	cfg := ServerEnergyConfig{}

	res := ComputeEnergy(ev, samples, nil, cfg, 0, 0, 0)
	if res.Source != "measured" {
		t.Fatalf("Source = %q, want %q", res.Source, "measured")
	}
	if math.IsNaN(res.WhTotal) || math.IsInf(res.WhTotal, 0) {
		t.Fatalf("WhTotal is non-finite: %v", res.WhTotal)
	}
	requireCloseWh(t, "WhTotal", res.WhTotal, 100.0*2/3600)
}

func TestEnergyComputeZeroOrNegativeLatencySkipsTier1(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	start := base
	end := base.Add(20 * time.Second)
	// Samples that WOULD qualify for Tier 1 if the window were real.
	samples := constPowerSamples(start, 20, 100)

	t.Run("zero latency, estimated available", func(t *testing.T) {
		ev := usage.Event{CreatedAt: end, LatencyMS: 0}
		cfg := ServerEnergyConfig{EstimatedWatts: 50}
		res := ComputeEnergy(ev, samples, nil, cfg, 10, 0, 0)
		if res.Source != "estimated" {
			t.Fatalf("Source = %q, want %q (Tier 1 must be skipped on a degenerate window)", res.Source, "estimated")
		}
		requireCloseWh(t, "WhTotal", res.WhTotal, 0)
	})

	t.Run("zero latency, modeled fallback", func(t *testing.T) {
		ev := usage.Event{CreatedAt: end, LatencyMS: 0, OutputTokens: 100}
		cfg := ServerEnergyConfig{}
		res := ComputeEnergy(ev, samples, nil, cfg, 10, 0.03, 0)
		if res.Source != "modeled" {
			t.Fatalf("Source = %q, want %q", res.Source, "modeled")
		}
		requireCloseWh(t, "WhTotal", res.WhTotal, 3.0)
	})

	t.Run("negative latency behaves like zero", func(t *testing.T) {
		ev := usage.Event{CreatedAt: end, LatencyMS: -500}
		cfg := ServerEnergyConfig{EstimatedWatts: 50}
		res := ComputeEnergy(ev, samples, nil, cfg, 10, 0, 0)
		if res.Source != "estimated" {
			t.Fatalf("Source = %q, want %q", res.Source, "estimated")
		}
		requireCloseWh(t, "WhTotal", res.WhTotal, 0)
	})
}

func TestEnergyComputeMarginalNeverExceedsTotal(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	start := base
	end := base.Add(12 * time.Second)

	fixtures := []struct {
		name string
		res  EnergyResult
	}{
		{
			"tier1 solo",
			ComputeEnergy(usage.Event{CreatedAt: end, LatencyMS: 12000}, constPowerSamples(start, 12, 77), nil, ServerEnergyConfig{}, 33, 0, 0),
		},
		{
			"tier1 with sibling",
			ComputeEnergy(usage.Event{CreatedAt: end, LatencyMS: 12000}, constPowerSamples(start, 12, 77), []usage.Event{siblingEvent(end.Add(50*time.Second), 100*time.Second)}, ServerEnergyConfig{}, 33, 0, 0),
		},
		{
			"tier2 solo",
			ComputeEnergy(usage.Event{CreatedAt: end, LatencyMS: 12000}, nil, nil, ServerEnergyConfig{EstimatedWatts: 90}, 33, 0, 0),
		},
		{
			"tier3",
			ComputeEnergy(usage.Event{CreatedAt: end, LatencyMS: 12000, OutputTokens: 42}, nil, nil, ServerEnergyConfig{}, 0, 0.01, 0),
		},
	}
	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			if f.res.WhMarginal > f.res.WhTotal+whTol {
				t.Fatalf("%s: WhMarginal (%v) > WhTotal (%v)", f.name, f.res.WhMarginal, f.res.WhTotal)
			}
			if f.res.WhTotal < 0 || f.res.WhMarginal < 0 {
				t.Fatalf("%s: negative energy: total=%v marginal=%v", f.name, f.res.WhTotal, f.res.WhMarginal)
			}
		})
	}
}
