// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"strings"
	"sync"
	"time"
)

// errBenchmarkCapacityUnavailable is returned when the ramp could not sustain even one
// concurrent request (MaxConcurrency == 0), so there is nothing safe to persist.
var errBenchmarkCapacityUnavailable = errors.New("benchmark: capacity unavailable (no viable concurrency level)")

// capacityResult is the distilled outcome of a concurrency-ramp capacity benchmark.
type capacityResult struct {
	// MaxConcurrency is the highest concurrency level that passed EVERY stop-check
	// (the OOM-safe hard ceiling). 0 means even a single request could not be served.
	MaxConcurrency int
	// RecommendedConcurrency is the highest level whose mean per-request latency stayed
	// within 1.5x the single-request (level-1) latency — the "no visible slowdown" knee.
	RecommendedConcurrency int
	// GenTokensPerSecondAtCapacity is the mean per-request generation rate measured at
	// the RecommendedConcurrency level.
	GenTokensPerSecondAtCapacity float64
	// MemoryObserved reports whether a real memory/saturation signal (agent telemetry OR
	// an upstream probe) governed the ramp, vs the latency-collapse fallback.
	MemoryObserved bool
	// Levels is every ATTEMPTED concurrency level's measurement (including the one
	// that tripped a stop-check, whose StopReason is set). Used to persist + display
	// the capacity curve (CP2b).
	Levels []routing.CapacityLevel
}

// capacityDefaultVRAMMarginPct / capacityDefaultMaxConcurrency / capacityDefaultSettle
// mirror gateway.New's defaults so a directly-constructed *Server (a test) still ramps
// sanely without wiring the config.
const (
	capacityDefaultVRAMMarginPct  = 10
	capacityDefaultMaxConcurrency = 64
	capacityDefaultSettle         = 5 * time.Second
	// capacityLatencyCollapseFactor is the latency-collapse stop threshold: a level whose
	// mean per-request latency exceeds this multiple of the level-1 latency is treated as
	// collapse (the upstream is thrashing) and stops the ramp. It is ALWAYS evaluated as one
	// signal of the additive stop-chain — NOT only a no-agent/no-probe fallback — so it is
	// the primary ceiling detector on the common llama.cpp+ServerAgent target, where VRAM is
	// pre-allocated (flat) and the VRAM-margin guard therefore never trips. Tunable: lower =
	// more conservative (stops the ramp sooner).
	capacityLatencyCollapseFactor = 4.0
	// capacityRecommendLatencyFactor is the knee: the recommended level is the highest whose
	// mean latency stays within this multiple of the level-1 latency. DISTINCT from the
	// collapse factor above (this defines recommended_concurrency, not the hard ceiling).
	capacityRecommendLatencyFactor = 1.5
	// capacityRampMaxTokens widens the capacity ramp's generation beyond the 64-token speed
	// prompt so a concurrent burst lasts long enough to (a) sample saturation while the N
	// requests are in flight and (b) reveal latency degradation. Only the capacity path uses
	// it; the shared speed builder's token budget is untouched.
	capacityRampMaxTokens = 128
	// capacitySamplerInterval is how often the during-burst saturation sampler polls the
	// upstream memory probe while a level's N requests are in flight.
	capacitySamplerInterval = 250 * time.Millisecond
)

// capacityOneReq is one request's measurement within a concurrency level (its latency,
// generation rate, and error, if any).
type capacityOneReq struct {
	latencyMS int64
	genTPS    float64
	err       error
}

// measureMappingCapacity runs an OOM-safe concurrency ramp against a mapping and returns
// the distilled capacity metrics. It NEVER escalates past a tripped stop-check, and never
// crosses the VRAM/RAM safety margin (the hard OOM guard). The memory mode is decided once
// at the start: agent telemetry (VRAM/RAM free-fraction) if an agent reports for the server,
// an upstream saturation probe if the app has a capacity probe path, else a latency-collapse
// fallback. A warm pass first ensures the model is resident so the ramp measures steady state.
func (s *Server) measureMappingCapacity(ctx context.Context, tgt benchmarkTarget, onLevel func(concurrency int)) (capacityResult, error) {
	var res capacityResult
	streamer, ok := s.Provider.(provider.StreamingClient)
	if !ok {
		return res, errBenchmarkNoStreaming
	}
	target, req := benchmarkTargetReq(tgt)
	// Attach the app's per-app upstream credential once so the memory probe (and the
	// warm/ramp streamOnce calls) carry it (fail-open on a decrypt error / no token).
	ctx = s.upstreamAuthCtx(ctx, target)
	// Widen the generation for the capacity ramp so a concurrent burst lasts long enough to
	// sample saturation (peak) + reveal latency degradation. req is a local value, so bumping
	// MaxTokens here does NOT affect the shared speed builder.
	req.MaxTokens = capacityRampMaxTokens

	// Warm pass: ensure the model is loaded so the ramp measures a resident model, not a
	// cold load/swap. Ignore everything but a hard error (upstream unreachable).
	if _, _, err := s.streamOnce(ctx, streamer, target, req); err != nil {
		return res, err
	}

	// Decide the memory mode ONCE at start. A known VRAM total OR RAM total qualifies: a
	// CPU-only llama.cpp host has no GPU (VRAMTotalBytes=0) so RAM is the relevant memory —
	// requiring VRAM>0 would skip the OOM guard on exactly the host where RAM is the ceiling.
	t0, agentOK, _ := s.Routes.TelemetryByServer(ctx, tgt.server.ID)
	agentOK = agentOK && (t0.VRAMTotalBytes > 0 || t0.RAMTotalBytes > 0)
	memProber, hasProber := s.Provider.(provider.MemoryProber)
	probePath := strings.TrimSpace(tgt.app.CapacityProbePath)
	upstreamProbe := hasProber && probePath != ""
	res.MemoryObserved = agentOK || upstreamProbe

	marginPct := s.capacityVRAMMarginPct
	if marginPct <= 0 {
		marginPct = capacityDefaultVRAMMarginPct
	}
	maxN := s.capacityMaxConcurrency
	if maxN <= 0 {
		maxN = capacityDefaultMaxConcurrency
	}
	settle := s.capacitySettle
	if settle <= 0 {
		settle = time.Duration(s.capacitySettleSeconds) * time.Second
	}
	if settle <= 0 {
		settle = capacityDefaultSettle
	}
	// Each level is bounded so a stalled level self-terminates (mirrors the per-call idle
	// watchdog inside streamOnce). max(app timeout, the always-on benchmark idle default).
	perLevelBudget := time.Duration(tgt.app.TimeoutMS) * time.Millisecond
	if perLevelBudget < benchmarkDefaultStreamIdle {
		perLevelBudget = benchmarkDefaultStreamIdle
	}
	// lastReportedAt advances past each fresh telemetry sample so the next level waits for a
	// strictly-newer reading (not a stale cached one).
	lastReportedAt := t0.ReportedAt

	var (
		lastGood            int
		level1LatencyMS     int64
		recommended         int
		genTPSAtRecommended float64
	)

	for n := 1; n <= maxN; n *= 2 {
		if ctx.Err() != nil { // prompt cancel / shutdown
			break
		}
		if onLevel != nil {
			onLevel(n)
		}

		levelCtx, cancel := context.WithTimeout(ctx, perLevelBudget)

		// During-burst saturation sampler. llama.cpp's requests_processing/requests_deferred
		// (and /slots occupancy) are INSTANTANEOUS gauges, so they must be read WHILE the N
		// requests are in flight — a post-drain reading always sees an idle server (0) and the
		// saturation stop-check could never fire. While the burst runs, poll the memory probe
		// and record the PEAK occupancy across samples. Only started when an upstream probe is
		// configured; otherwise samplerDone is pre-closed so the join below is a no-op.
		var peakDeferred, peakProcessing, peakTotalSlots int
		done := make(chan struct{})
		samplerDone := make(chan struct{})
		if upstreamProbe {
			go func() {
				defer close(samplerDone)
				sample := func() {
					sm, _ := memProber.ProbeServerMemory(ctx, target, probePath, "")
					if !sm.OK {
						return
					}
					if sm.RequestsDeferred > peakDeferred {
						peakDeferred = sm.RequestsDeferred
					}
					if sm.RequestsProcessing > peakProcessing {
						peakProcessing = sm.RequestsProcessing
					}
					if sm.TotalSlots > peakTotalSlots {
						peakTotalSlots = sm.TotalSlots
					}
				}
				sample() // immediate reading; the ticker catches the peak mid-burst
				ticker := time.NewTicker(capacitySamplerInterval)
				defer ticker.Stop()
				for {
					select {
					case <-done:
						return
					case <-ctx.Done():
						return
					case <-ticker.C:
						sample()
					}
				}
			}()
		} else {
			close(samplerDone)
		}

		// Fire n concurrent streaming requests, recording each one's latency + gen rate.
		reqs := make([]capacityOneReq, n)
		var wg sync.WaitGroup
		wg.Add(n)
		for i := 0; i < n; i++ {
			go func(i int) {
				defer wg.Done()
				start := time.Now()
				_, usage, err := s.streamOnce(levelCtx, streamer, target, req)
				reqs[i] = capacityOneReq{latencyMS: time.Since(start).Milliseconds(), genTPS: usage.TokensPerSecond, err: err}
			}(i)
		}
		wg.Wait()
		close(done)   // stop the sampler now that the burst is over
		<-samplerDone // wait for it to finish writing the peaks (happens-before the reads below)
		cancel()

		errCount := 0
		var sumLatency int64
		var sumGen float64
		successes := 0
		for _, rq := range reqs {
			if rq.err != nil {
				errCount++
				continue
			}
			successes++
			sumLatency += rq.latencyMS
			sumGen += rq.genTPS
		}
		var meanLatencyMS int64
		var meanGenTPS float64
		if successes > 0 {
			meanLatencyMS = sumLatency / int64(successes)
			meanGenTPS = sumGen / float64(successes)
		}
		if n == 1 {
			level1LatencyMS = meanLatencyMS
		}

		// Per-level stop classification. EVERY applicable signal is evaluated each level (a
		// mutually exclusive switch would skip latency-collapse whenever an agent OR probe
		// exists — which on the flat-VRAM llama.cpp+agent target leaves NO working ceiling).
		// Two DISTINCT upstream signals must NOT be conflated: requests actually QUEUED
		// (deferred>0) means this level is OVER capacity -> discard it; all slots busy but
		// NOTHING queued (processing>=total, deferred<=0) means this level IS the slot ceiling
		// and it served fine -> COUNT it, then stop escalating (a higher level would only
		// queue). Conflating them discarded a fully-served level, so --parallel 1 (the llama.cpp
		// default) wrongly reported "capacity unavailable" and power-of-2 slot counts
		// under-reported 2x.
		errStop := errCount > 0 // (i) a failed/5xx request => the server can't sustain this level
		memStop := false        // (ii) VRAM/RAM margin — the hard OOM guard
		var freshTelemetry routing.ServerTelemetry
		if agentOK {
			// Wait for a telemetry sample that reflects THIS level's load (fresh, past
			// lastReportedAt), then check the free fraction against the safety margin. No fresh
			// sample => stale => stop conservatively. llama.cpp VRAM is pre-allocated (flat), so
			// this is an OOM backstop, not the primary ceiling — hence the additive checks below.
			if fresh, fok := s.waitFreshTelemetry(ctx, tgt.server.ID, lastReportedAt, settle); !fok {
				memStop = true
			} else {
				lastReportedAt = fresh.ReportedAt
				freshTelemetry = fresh
				memStop = memoryMarginBreached(fresh, marginPct)
			}
		}
		// (iii) Upstream queueing — requests were actually DEFERRED at the PEAK while the burst
		// was in flight (not post-drain). This is OVER capacity, so this level is discarded.
		queueStop := upstreamProbe && peakDeferred > 0
		// (iv) Latency collapse — the mean per-request latency blew past the collapse multiple
		// of the single-request latency. ALWAYS checked (the only ceiling with no probe + flat
		// VRAM).
		latStop := level1LatencyMS > 0 && float64(meanLatencyMS) > capacityLatencyCollapseFactor*float64(level1LatencyMS)
		// All slots busy but NOTHING queued: this level IS the slot ceiling and it served fine.
		atSlotCeiling := upstreamProbe && peakTotalSlots > 0 && peakProcessing >= peakTotalSlots && peakDeferred <= 0

		// Record this attempted level for the capacity curve (CP2b). aggregateGenTPS =
		// sum of successful per-request rates (total generation throughput at this level).
		var aggregateGenTPS float64
		for _, rq := range reqs {
			if rq.err == nil {
				aggregateGenTPS += rq.genTPS
			}
		}
		level := routing.CapacityLevel{
			Concurrency:               n,
			AggregateTokensPerSecond:  aggregateGenTPS,
			PerRequestTokensPerSecond: meanGenTPS,
			MeanLatencyMS:             meanLatencyMS,
			Successes:                 successes,
			Errors:                    errCount,
			RequestsDeferred:          peakDeferred,
			RequestsProcessing:        peakProcessing,
			TotalSlots:                peakTotalSlots,
		}
		if agentOK && !freshTelemetry.ReportedAt.IsZero() {
			level.VRAMFreePct = vramFreeFrac(freshTelemetry) * 100
			level.RAMFreePct = ramFreeFrac(freshTelemetry) * 100
		}
		switch {
		case errStop:
			level.StopReason = "error"
		case memStop:
			level.StopReason = "memory"
		case queueStop:
			level.StopReason = "queue"
		case latStop:
			level.StopReason = "latency"
		case atSlotCeiling:
			level.StopReason = "slot_ceiling"
		}
		res.Levels = append(res.Levels, level)

		if errStop || memStop || queueStop || latStop {
			// If the ONLY reason was upstream queueing and the probe reported the slot count,
			// that slot count is the authoritative -np ceiling (the server served all its slots
			// and queued the extra) — report it as max, more accurate than the last power-of-2
			// that fit under it.
			if queueStop && !errStop && !memStop && !latStop && peakTotalSlots > lastGood {
				lastGood = peakTotalSlots
			}
			break // discard this (over-capacity) level; never escalate past a tripped check
		}

		// This level is good (served all N requests within every safety signal).
		lastGood = n
		// Track the recommended (latency-knee) level: the highest level whose mean latency
		// stayed within 1.5x the level-1 latency. level1LatencyMS<=0 (unmeasurable) => accept.
		if level1LatencyMS <= 0 || float64(meanLatencyMS) <= capacityRecommendLatencyFactor*float64(level1LatencyMS) {
			recommended = n
			genTPSAtRecommended = meanGenTPS
		}
		if atSlotCeiling {
			// Served every slot with nothing queued — this level is the ceiling; a higher level
			// would only queue. Stop escalating, but (unlike the discard branch above) this
			// level was COUNTED as good.
			if peakTotalSlots > 0 && peakTotalSlots < lastGood {
				lastGood = peakTotalSlots // n overshot the reported slot ceiling (e.g. /slots without a deferred signal); report the true -np
			}
			break
		}
	}

	res.MaxConcurrency = lastGood
	if lastGood == 0 {
		// Even a single concurrent request failed — no capacity to report; the caller
		// must NOT persist. Surface it as an error so it lands in the result's Error.
		return res, errBenchmarkCapacityUnavailable
	}
	if recommended < 1 {
		recommended = 1
	}
	if recommended > lastGood {
		recommended = lastGood
	}
	res.RecommendedConcurrency = recommended
	res.GenTokensPerSecondAtCapacity = genTPSAtRecommended
	return res, nil
}

// waitFreshTelemetry sleeps the settle interval (to let the agent report the new load), then
// polls TelemetryByServer until it sees a sample reported strictly after `after`, up to a
// ceiling of ~3x settle. Returns (sample, true) on a fresh sample, or (zero, false) if the
// context is cancelled or no fresh sample arrives within the ceiling (treated as stale =>
// stop conservatively by the caller). The settle-wait is deliberate and distinct from the
// per-call idle watchdog.
func (s *Server) waitFreshTelemetry(ctx context.Context, serverID string, after time.Time, settle time.Duration) (routing.ServerTelemetry, bool) {
	if !sleepCtx(ctx, settle) {
		return routing.ServerTelemetry{}, false
	}
	ceiling := 3 * settle
	deadline := time.Now().Add(ceiling)
	for {
		t, ok, _ := s.Routes.TelemetryByServer(ctx, serverID)
		if ok && t.ReportedAt.After(after) {
			return t, true
		}
		if time.Now().After(deadline) {
			return routing.ServerTelemetry{}, false
		}
		if !sleepCtx(ctx, settle) {
			return routing.ServerTelemetry{}, false
		}
	}
}

// sleepCtx sleeps for d, returning false if the context is cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// memoryMarginBreached reports whether a KNOWN VRAM or RAM total's free fraction has fallen
// below the safety margin (percent) — the hard OOM guard for the capacity ramp. A resource
// with no reported total (<=0) is treated as unconstrained (never trips), so a VRAM-only host
// is guarded by VRAM, a CPU-only host by RAM, and a mixed host by both.
func memoryMarginBreached(t routing.ServerTelemetry, marginPct int) bool {
	if t.VRAMTotalBytes > 0 && vramFreeFrac(t)*100 < float64(marginPct) {
		return true
	}
	if t.RAMTotalBytes > 0 && ramFreeFrac(t)*100 < float64(marginPct) {
		return true
	}
	return false
}

// vramFreeFrac / ramFreeFrac return the free fraction (0..1) of VRAM/RAM from a telemetry
// sample; a non-positive total yields 1 (unknown => "fully free", never trips the margin).
func vramFreeFrac(t routing.ServerTelemetry) float64 {
	if t.VRAMTotalBytes <= 0 {
		return 1
	}
	return 1 - float64(t.VRAMUsedBytes)/float64(t.VRAMTotalBytes)
}

func ramFreeFrac(t routing.ServerTelemetry) float64 {
	if t.RAMTotalBytes <= 0 {
		return 1
	}
	return 1 - float64(t.RAMUsedBytes)/float64(t.RAMTotalBytes)
}
