// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"math/rand"
	"op-ai-gateway/internal/gateway/visionassets"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"strings"
	"time"
)

var errBenchmarkNoStreaming = errors.New("benchmark: provider does not support streaming")

// benchmarkMaxContextSize is the ceiling for a probed n_ctx; a larger value is
// treated as a misbehaving upstream and ignored. Mirrors the P2 probe-pass const
// (maxProbedContextSize in cmd/gateway, which lives in package main and is not
// importable here).
const benchmarkMaxContextSize = 100_000_000

// benchmarkDefaultStreamIdle bounds each benchmark streaming call when the edge
// idle timeout (s.streamIdleTimeout) is disabled. A benchmark run has NO client to
// end it (it executes on context.Background), so the watchdog must ALWAYS be on:
// otherwise a stalled upstream (a cold model load/swap that never emits) would hang
// streamOnce forever, so run.finish() (deferred in runBenchmark) never runs, so the
// server stays flagged busy and is permanently excluded from routing. Generous
// because the watchdog resets on EVERY event, so only a true stall (no event for
// this long) trips it — a slow-but-progressing cold load is not killed.
const benchmarkDefaultStreamIdle = 2 * time.Minute

// benchmarkPrompts are the fixed prompts a run issues per mapping. Small + bounded so
// a run is quick; the prompt is sent twice (cold+warm) to separate load/swap time from
// steady-state throughput.
var benchmarkPrompts = []struct {
	text      string
	maxTokens int
}{
	{"Reply with exactly one short sentence about the number 7.", 64},
}

// benchmarkTarget is one mapping to measure (all targets in a run share one server).
type benchmarkTarget struct {
	server  routing.AIServer
	app     routing.Application
	mapping routing.ModelMapping
}

// streamOnce issues one streaming request and returns time-to-first-token + the
// terminal usage. For upstreams that report no timings it derives a generation rate
// (output tokens / seconds from first token to completion) into usage.TokensPerSecond.
// Note: there is NO wall-clock fallback for PromptPerSecond (prefill rate) — an
// upstream that reports no timings leaves it unknown (0).
func (s *Server) streamOnce(ctx context.Context, streamer provider.StreamingClient, target routing.Target, req inference.Request) (time.Duration, inference.Usage, error) {
	// A benchmark run outlives the trigger request (context.Background), so bound each
	// streaming call with an idle watchdog: if no event arrives within `idle`, cancel —
	// otherwise a stalled upstream (the cold-load case a benchmark provokes) would hang
	// forever and leave the server permanently busy/excluded from routing. Any event
	// (progress) resets the timer, so a slow-but-progressing cold load is not killed.
	// Attach the target application's per-app upstream credential (fail-open).
	ctx = s.upstreamAuthCtx(ctx, target)
	idle := s.streamIdleTimeout
	if idle <= 0 {
		idle = benchmarkDefaultStreamIdle // ALWAYS on for benchmarks, even if the edge idle is disabled
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	watchdog := time.AfterFunc(idle, cancel)
	defer watchdog.Stop()

	start := time.Now()
	var firstAt time.Time
	var gotFirst bool
	var usage inference.Usage
	streamErr := streamer.CompleteStream(ctx, target, req, func(ev inference.StreamEvent) error {
		watchdog.Reset(idle) // progress resets the stall timer
		switch ev.Type {
		case inference.StreamEventTextDelta:
			if !gotFirst && (ev.Text != "" || ev.Reasoning != "") {
				firstAt = time.Now()
				gotFirst = true
			}
		case inference.StreamEventCompleted:
			if ev.Usage != nil {
				usage = *ev.Usage
			}
		}
		return nil
	})
	if streamErr != nil {
		return 0, inference.Usage{}, streamErr
	}
	end := time.Now()
	var ttft time.Duration
	if gotFirst {
		ttft = firstAt.Sub(start)
	}
	if usage.TokensPerSecond == 0 && usage.OutputTokens > 0 && gotFirst {
		if genSecs := end.Sub(firstAt).Seconds(); genSecs > 0 {
			usage.TokensPerSecond = float64(usage.OutputTokens) / genSecs
		}
	}
	return ttft, usage, nil
}

// benchmarkTargetReq builds the routing.Target + a base inference.Request (from the first
// benchmark prompt) a benchmark issues for a mapping. Shared by the speed (measureMapping)
// and capacity (measureMappingCapacity) paths so both hit an identical target/request.
func benchmarkTargetReq(tgt benchmarkTarget) (routing.Target, inference.Request) {
	target := routing.Target{
		Provider:       tgt.app.Type,
		Endpoint:       routing.ApplicationEndpoint(tgt.server, tgt.app),
		Model:          tgt.mapping.GatewayModelName,
		ProviderModel:  tgt.mapping.AppModelName,
		Timeout:        time.Duration(tgt.app.TimeoutMS) * time.Millisecond,
		APIToken:       tgt.app.APIToken,
		APITokenHeader: tgt.app.APITokenHeader,
	}
	p := benchmarkPrompts[0]
	req := inference.Request{
		Model:     tgt.mapping.AppModelName,
		MaxTokens: p.maxTokens,
		Stream:    true,
		Messages:  []inference.Message{{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: p.text}}}},
	}
	return target, req
}

// measureVisionTarget determines whether a mapping's model accepts images. It sends
// a text-only baseline first (to tell "upstream down" from "image rejected"), then an
// image request. accept mode: no error on the image request => capable. verify mode:
// additionally the answer must contain the image's known tokens. A nil VisionCapable
// means inconclusive (baseline failed) — the caller writes NOTHING in that case.
func (s *Server) measureVisionTarget(ctx context.Context, tgt benchmarkTarget, mode string, imageDataURL string, verifyTokens []string) BenchmarkResult {
	res := BenchmarkResult{MappingID: tgt.mapping.ID, GatewayModelName: tgt.mapping.GatewayModelName}
	streamer, ok := s.Provider.(provider.StreamingClient)
	if !ok {
		res.Error = errBenchmarkNoStreaming.Error()
		return res
	}
	target, baseReq := benchmarkTargetReq(tgt)
	baseReq.MaxTokens = 1

	// 1) Baseline text request — proves the server/model works at all.
	if _, _, err := s.streamOnce(ctx, streamer, target, baseReq); err != nil {
		res.Error = err.Error() // inconclusive; VisionCapable stays nil
		return res
	}

	// 2) Image request. 32 tokens is enough for the verify-mode answer (naming two
	//    colors); accept mode only needs the request to succeed, so the extra
	//    headroom there is harmless.
	imgReq := baseReq
	imgReq.MaxTokens = 32
	imgReq.Messages = visionImageMessages(mode, imageDataURL)
	answer, err := s.streamCollect(ctx, streamer, target, imgReq)
	if err != nil {
		capable := false
		res.VisionCapable = &capable // definitive: upstream rejected the image
		return res
	}
	capable := true
	if mode == "verify" {
		capable = answerContainsTokens(answer, verifyTokens)
	}
	res.VisionCapable = &capable
	return res
}

// answerContainsTokens is true iff the normalized answer contains EVERY token.
func answerContainsTokens(answer string, tokens []string) bool {
	if len(tokens) == 0 {
		return false
	}
	norm := strings.ToLower(answer)
	for _, tok := range tokens {
		if !strings.Contains(norm, strings.ToLower(tok)) {
			return false
		}
	}
	return true
}

// visionImageMessages builds the single user message with a short prompt and an
// image part. In verify mode the prompt asks for the colors; in accept mode any
// image suffices.
func visionImageMessages(mode string, dataURL string) []inference.Message {
	prompt := "Describe this image in one short sentence."
	if mode == "verify" {
		prompt = "Name the two colors in this image. Answer with the two color words only."
	}
	return []inference.Message{{
		Role: inference.RoleUser,
		Content: []inference.ContentPart{
			{Type: inference.ContentText, Text: prompt},
			{Type: inference.ContentImage, ImageURL: dataURL},
		},
	}}
}

// streamCollect issues one streaming request and returns the concatenated text
// deltas. Same idle-watchdog/auth bounding as streamOnce.
func (s *Server) streamCollect(ctx context.Context, streamer provider.StreamingClient, target routing.Target, req inference.Request) (string, error) {
	ctx = s.upstreamAuthCtx(ctx, target)
	idle := s.streamIdleTimeout
	if idle <= 0 {
		idle = benchmarkDefaultStreamIdle
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	watchdog := time.AfterFunc(idle, cancel)
	defer watchdog.Stop()
	var sb strings.Builder
	err := streamer.CompleteStream(ctx, target, req, func(ev inference.StreamEvent) error {
		watchdog.Reset(idle)
		if ev.Type == inference.StreamEventTextDelta {
			sb.WriteString(ev.Text)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return sb.String(), nil
}

// pickVisionImage picks a random embedded probe image and returns it as a data URL
// plus the color tokens a verify-mode answer must contain.
func pickVisionImage() (dataURL string, tokens []string) {
	all := visionassets.All()
	pick := all[rand.Intn(len(all))]
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(pick.PNG), pick.Tokens
}

// coldLoadPollGap / coldLoadMaxWait bound the wait for a model to actually leave the upstream
// after an unload/swap, so the cold pass measures a real load, not a mid-unload state. Vars
// (not consts) so tests can shorten them.
var (
	coldLoadPollGap = 500 * time.Millisecond
	coldLoadMaxWait = 30 * time.Second
	// coldLoadResidentMaxWait bounds ensureResidentForRun's retry of the load
	// request when the upstream answers "unavailable" (a 503) WHILE it is still
	// loading -- the behaviour of llama-swap and other single-slot swappers, which
	// a VRAM/load benchmark provokes by design (it isolates the target and then
	// asks for it cold). A large model can take minutes to become servable, so
	// this is generous; a genuinely stuck upstream still fails, just after the
	// budget rather than on the first probe. A var so tests can shorten it. A
	// blocking-but-progressing load is bounded separately by streamOnce's own idle
	// watchdog and never reaches this loop.
	coldLoadResidentMaxWait = 5 * time.Minute
	// coldLoadCallTimeout is a defensive per-call bound for the loaded-probe and the unload
	// when the app carries no positive Timeout — so a wedged upstream can NEVER hang the
	// benchmark run (which would permanently exclude the server from routing). App timeouts
	// are validated positive today; this floor keeps the guarantee independent of that.
	coldLoadCallTimeout = 30 * time.Second
)

// ensureColdLoad best-effort guarantees the target model is NOT resident, so the next request
// is a genuine cold load. Returns true only when it CONFIRMED a cold state (model verified
// absent, or verified not-loaded to begin with). Returns false when it cannot confirm — the
// caller then reports load-time as unknown rather than a bogus value. Never errors (all
// failures degrade to false); throughput is measured regardless. All calls are bare (no client
// bearer token), safe because a benchmark run idle-gates + routing-excludes the server.
func (s *Server) ensureColdLoad(ctx context.Context, tgt benchmarkTarget) bool {
	lister, hasLister := s.Provider.(provider.LoadedModelLister)
	if !hasLister || strings.TrimSpace(tgt.app.LoadedModelsPath) == "" {
		return false // no way to observe loaded-state => cannot confirm cold
	}
	model := tgt.mapping.AppModelName
	if strings.TrimSpace(model) == "" {
		return false
	}
	probeTarget, _ := benchmarkTargetReq(tgt) // correct Provider/Endpoint/Timeout for probe+unload
	// Defensive floor: the loaded-probe and the unload bound their HTTP call by
	// probeTarget.Timeout; if the app carries no positive timeout, apply coldLoadCallTimeout
	// so a wedged upstream can never hang the run (which would permanently exclude the server).
	if probeTarget.Timeout <= 0 {
		probeTarget.Timeout = coldLoadCallTimeout
	}
	// Attach the app's per-app upstream credential so the unload + loaded-probe
	// (and the sibling-swap stream via streamOnce) carry it too (fail-open).
	ctx = s.upstreamAuthCtx(ctx, probeTarget)

	loaded, err := modelLoaded(ctx, lister, probeTarget, tgt.app, model)
	if err != nil {
		return false
	}
	if !loaded {
		return true // already cold
	}
	// Loaded → evict. (1) explicit unload where supported.
	if unloader, ok := s.Provider.(provider.ModelUnloader); ok {
		if done, _ := unloader.UnloadModel(ctx, probeTarget, model); done {
			if s.waitModelUnloaded(ctx, lister, probeTarget, tgt.app, model) {
				return true
			}
		}
	}
	// (2) swap workaround: stream a sibling model on the same app to evict the target on a
	// single-slot swapper.
	if sib, ok := s.benchmarkSiblingModel(ctx, tgt); ok {
		if streamer, ok := s.Provider.(provider.StreamingClient); ok {
			sibTarget, sibReq := benchmarkTargetReq(tgt)
			sibTarget.Model, sibTarget.ProviderModel, sibReq.Model = sib, sib, sib
			sibReq.MaxTokens = 1                                     // minimal — we only want the swap
			_, _, _ = s.streamOnce(ctx, streamer, sibTarget, sibReq) // best-effort; loads sib, evicts model
			if s.waitModelUnloaded(ctx, lister, probeTarget, tgt.app, model) {
				return true
			}
		}
	}
	return false // could not force cold
}

// modelLoaded reports whether `model` is in the app's fresh loaded set.
func modelLoaded(ctx context.Context, lister provider.LoadedModelLister, target routing.Target, app routing.Application, model string) (bool, error) {
	names, err := lister.LoadedModels(ctx, target, app.LoadedModelsPath, app.LoadedModelsFormat)
	if err != nil {
		return false, err
	}
	for _, n := range names {
		if n == model {
			return true, nil
		}
	}
	return false, nil
}

// waitModelUnloaded polls the loaded set until `model` is absent, bounded by coldLoadMaxWait.
// A probe error is treated as "not yet confirmed" and retried until the deadline (then false).
func (s *Server) waitModelUnloaded(ctx context.Context, lister provider.LoadedModelLister, target routing.Target, app routing.Application, model string) bool {
	deadline := time.Now().Add(coldLoadMaxWait)
	for {
		loaded, err := modelLoaded(ctx, lister, target, app, model)
		if err == nil && !loaded {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		if !sleepCtx(ctx, coldLoadPollGap) {
			return false // ctx done
		}
	}
}

// benchmarkSiblingModel returns another active mapping's upstream model name on the same
// application (a distinct AppModelName), for the swap-workaround eviction.
func (s *Server) benchmarkSiblingModel(ctx context.Context, tgt benchmarkTarget) (string, bool) {
	mappings, err := s.Routes.MappingsByApplication(ctx, tgt.app.ID)
	if err != nil {
		return "", false
	}
	for _, m := range mappings {
		if m.Status == routing.ServerStatusActive && strings.TrimSpace(m.AppModelName) != "" && m.AppModelName != tgt.mapping.AppModelName {
			return m.AppModelName, true
		}
	}
	return "", false
}

// measureMapping streams the benchmark prompt twice (cold then warm) and returns the
// measured metrics. Cold TTFT (first request, may trigger a load/swap) minus warm TTFT
// approximates load time; throughput comes from the warm request.
func (s *Server) measureMapping(ctx context.Context, tgt benchmarkTarget) (BenchmarkResult, error) {
	res := BenchmarkResult{MappingID: tgt.mapping.ID, GatewayModelName: tgt.mapping.GatewayModelName}
	streamer, ok := s.Provider.(provider.StreamingClient)
	if !ok {
		return res, errBenchmarkNoStreaming
	}
	target, req := benchmarkTargetReq(tgt)
	// Force a genuine cold start so the cold-minus-warm delta is a real load time (not
	// first-call jitter on an already-resident model). coldConfirmed is false when a cold
	// state could not be guaranteed/verified (no loaded-tracking configured, eviction
	// unavailable, or verification timed out) — then we do NOT emit a load time (unknown),
	// never a bogus value. Throughput is measured regardless.
	coldConfirmed := s.ensureColdLoad(ctx, tgt)
	coldTTFT, _, err := s.streamOnce(ctx, streamer, target, req)
	if err != nil {
		res.Error = err.Error()
		return res, err
	}
	warmTTFT, usage, err := s.streamOnce(ctx, streamer, target, req)
	if err != nil {
		res.Error = err.Error()
		return res, err
	}
	// Only record a load time when a cold start was CONFIRMED and BOTH passes produced a
	// first token (a real TTFT is never exactly 0; ttft==0 from streamOnce means "no first
	// token"), so we never record a bogus cold-minus-0 delta or a warm-vs-warm jitter delta.
	if coldConfirmed && coldTTFT > 0 && warmTTFT > 0 && coldTTFT > warmTTFT {
		res.LoadTimeMS = int((coldTTFT - warmTTFT).Milliseconds())
	}
	res.GenTokensPerSecond = usage.TokensPerSecond
	res.PromptTokensPerSecond = usage.PromptPerSecond
	return res, nil
}

// runBenchmark executes a benchmark over targets (all on the run's server), persisting
// each mapping's metrics (lock-respecting) + best-effort re-probing context, and always
// finishes the run (clearing the server-busy state) even on error/cancel. mode selects
// what is measured per target: "speed" (throughput/load, the pre-CP2 behavior), "capacity"
// (the OOM-safe concurrency ramp), "both" (speed then capacity, so metrics_source ends
// "capacity"), or "vision" (the image-acceptance probe: persists vision_capable on the
// mapping only on a definitive verdict, but always appends a kind=="vision" history row
// — success or inconclusive). An empty mode is treated as "speed".
func (s *Server) runBenchmark(ctx context.Context, run *benchmarkRun, serverID string, targets []benchmarkTarget, mode string) {
	var runErr string
	defer func() {
		run.finish(runErr)
		s.Benchmarks.publish(serverID, run.snapshot()) // terminal frame after finish so subscribers see Running=false
	}()
	for _, tgt := range targets {
		if ctx.Err() != nil {
			runErr = "canceled"
			return
		}
		var res BenchmarkResult
		// speedErr is the SPEED measurement's own error, captured before a "both" run
		// merges the capacity error into res.Error (for the live/poll status). The
		// speed-history row must record only the speed error — otherwise a successful
		// speed benchmark whose capacity ramp failed would be mislabeled as failed.
		var speedErr string
		switch mode {
		case "capacity":
			res = s.measureCapacityTarget(ctx, tgt, run, serverID)
		case "both":
			// Speed first, then capacity — capacity's UpdateMappingCapacityMetrics runs
			// LAST so metrics_source ends "capacity". The two results are merged so the
			// poll sees both the speed and capacity scalars for the mapping.
			res = s.measureSpeedTarget(ctx, tgt)
			speedErr = res.Error // capture BEFORE the merge below
			capRes := s.measureCapacityTarget(ctx, tgt, run, serverID)
			res.MaxConcurrency = capRes.MaxConcurrency
			res.RecommendedConcurrency = capRes.RecommendedConcurrency
			res.GenTokensPerSecondAtCapacity = capRes.GenTokensPerSecondAtCapacity
			if res.Error == "" {
				res.Error = capRes.Error
			}
		case "vision":
			dataURL, tokens := pickVisionImage() // random embedded asset
			res = s.measureVisionTarget(ctx, tgt, s.Portal.VisionProbeMode(ctx), dataURL, tokens)
			if res.VisionCapable != nil {
				_ = s.Routes.UpdateMappingVisionCapable(ctx, tgt.mapping.ID, *res.VisionCapable, time.Now().UTC())
			}
			// Always append a vision-history row — success (a definitive verdict) AND an
			// inconclusive probe (VisionCapable nil, res.Error set) — mirroring how the
			// speed path records both outcomes. Best-effort: a history-write error never
			// fails the run.
			_ = s.Routes.InsertBenchmarkRun(ctx, routing.BenchmarkRun{
				MappingID:     tgt.mapping.ID,
				ServerID:      tgt.server.ID,
				CreatedAt:     time.Now().UTC(),
				Kind:          "vision",
				VisionCapable: res.VisionCapable != nil && *res.VisionCapable,
				Error:         res.Error,
			})
		default: // "speed" (and empty)
			res = s.measureSpeedTarget(ctx, tgt)
			speedErr = res.Error
		}
		// Append a speed-history row (success AND failure — speedErr is set on a measure
		// failure). Best-effort: a history-write error never fails the run. A capacity-only
		// or vision-only run measures no speed metrics, so it writes no speed-history row
		// (the capacity history curve is written inside measureCapacityTarget).
		if mode != "capacity" && mode != "vision" {
			_ = s.Routes.InsertBenchmarkRun(ctx, routing.BenchmarkRun{
				MappingID:             tgt.mapping.ID,
				ServerID:              tgt.server.ID,
				CreatedAt:             time.Now().UTC(),
				GenTokensPerSecond:    res.GenTokensPerSecond,
				PromptTokensPerSecond: res.PromptTokensPerSecond,
				LoadTimeMS:            res.LoadTimeMS,
				ContextSize:           res.ContextSize,
				Error:                 speedErr,
			})
		}
		run.addResult(res)
		s.Benchmarks.publish(serverID, run.snapshot()) // progress frame after each measured mapping
	}
}

// runContextProbe warm-loads the target's model (forcing a load if not resident) then probes its
// context size via the app's context_probe_path, and REPORTS the size through the run status. It
// does NOT persist (the frontend fills the form field; the user saves manually). Reuses the
// benchmark server reservation so it is mutually exclusive with benchmarks + live traffic.
func (s *Server) runContextProbe(ctx context.Context, run *benchmarkRun, serverID string, tgt benchmarkTarget) {
	res := BenchmarkResult{MappingID: tgt.mapping.ID, GatewayModelName: tgt.mapping.GatewayModelName}
	// The terminal frame is published in a defer (mirroring runBenchmark's deferred finish), so it
	// runs on EVERY exit — including a panic mid-unwind — and the server is never left reserved. It
	// records the single result, marks the run finished (Running=false → frees the server for
	// ServerBusy/routing), and publishes. It does NOT call Release: like a benchmark, the terminal
	// status must LINGER in the registry so the frontend's benchmarkStatus poll can read
	// results[].context_size after completion (the next TryStart overwrites the lingering entry).
	// Release is ONLY for undoing a reservation whose pre-run idle-gate failed (in the handler).
	defer func() {
		run.addResult(res)
		run.finish(res.Error)
		s.Benchmarks.publish(serverID, run.snapshot())
	}()
	// 1) Warm-load: stream a tiny request so the model becomes resident (llama-swap/llama.cpp load
	//    on first request). Reuses the idle-watchdog-bounded streamOnce.
	streamer, ok := s.Provider.(provider.StreamingClient)
	if !ok {
		res.Error = errBenchmarkNoStreaming.Error()
		return
	}
	target, req := benchmarkTargetReq(tgt)
	if _, _, err := s.streamOnce(ctx, streamer, target, req); err != nil {
		res.Error = err.Error()
		return
	}
	// 2) Probe context (the model is now resident). Reuse the measureSpeedTarget probe pattern, but
	//    always attribute directly to THIS mapping (we just loaded it): {model} expansion + the
	//    shared PickModelContextSize (name-match preferred, else first positive). No store write.
	if prober, ok := s.Provider.(provider.ModelInfoProber); ok && strings.TrimSpace(tgt.app.ContextProbePath) != "" {
		pt := routing.Target{Provider: tgt.app.Type, Endpoint: routing.ApplicationEndpoint(tgt.server, tgt.app), Timeout: time.Duration(tgt.app.TimeoutMS) * time.Millisecond, APIToken: tgt.app.APIToken, APITokenHeader: tgt.app.APITokenHeader}
		pctx := s.upstreamAuthCtx(ctx, pt)
		probePath := provider.ExpandModelPath(tgt.app.ContextProbePath, tgt.mapping.AppModelName)
		if infos, perr := prober.ProbeModelInfo(pctx, pt, probePath); perr == nil {
			if ctxSize := provider.PickModelContextSize(infos, tgt.mapping.AppModelName); ctxSize > 0 && ctxSize <= benchmarkMaxContextSize {
				res.ContextSize = ctxSize
			}
		}
	}
}

// measureSpeedTarget runs the speed benchmark for one target: measure throughput/load,
// re-probe context, and persist (lock-respecting). It returns the BenchmarkResult; the
// caller handles history/addResult/publish. This is the pre-CP2 per-target body.
func (s *Server) measureSpeedTarget(ctx context.Context, tgt benchmarkTarget) BenchmarkResult {
	res, err := s.measureMapping(ctx, tgt)
	if err == nil {
		// Re-probe context FIRST (the model is resident from measureMapping's warm
		// pass); UpdateMappingContextProbe sets metrics_source='probe'.
		if prober, ok := s.Provider.(provider.ModelInfoProber); ok && strings.TrimSpace(tgt.app.ContextProbePath) != "" {
			pt := routing.Target{Provider: tgt.app.Type, Endpoint: routing.ApplicationEndpoint(tgt.server, tgt.app), Timeout: time.Duration(tgt.app.TimeoutMS) * time.Millisecond, APIToken: tgt.app.APIToken, APITokenHeader: tgt.app.APITokenHeader}
			pctx := s.upstreamAuthCtx(ctx, pt)
			// Expand a {model} template with THIS mapping's upstream name so a per-model props endpoint is
			// queried. The benchmark just warmed this exact model (measureMapping's warm pass), so it is
			// resident — no loaded-registry gate is needed here (unlike the health-loop pass).
			probePath := provider.ExpandModelPath(tgt.app.ContextProbePath, tgt.mapping.AppModelName)
			if infos, perr := prober.ProbeModelInfo(pctx, pt, probePath); perr == nil {
				if strings.Contains(tgt.app.ContextProbePath, "{model}") {
					// Per-model endpoint: attribute DIRECTLY to this (resident) mapping, mirroring the
					// health-loop {model} branch (sidesteps the reported-name match).
					if ctxSize := provider.PickModelContextSize(infos, tgt.mapping.AppModelName); ctxSize > 0 && ctxSize <= benchmarkMaxContextSize {
						_ = s.Routes.UpdateMappingContextProbe(ctx, tgt.mapping.ID, ctxSize, time.Now().UTC())
						res.ContextSize = ctxSize
					}
				} else {
					for _, info := range infos {
						if info.Name == tgt.mapping.AppModelName && info.ContextSize > 0 && info.ContextSize <= benchmarkMaxContextSize {
							_ = s.Routes.UpdateMappingContextProbe(ctx, tgt.mapping.ID, info.ContextSize, time.Now().UTC())
							res.ContextSize = info.ContextSize
						}
					}
				}
			}
		}
		// Benchmark metrics LAST so metrics_source ends "benchmark" (this write does
		// not touch context_size, so a probed context survives).
		_ = s.Routes.UpdateMappingBenchmarkMetrics(ctx, tgt.mapping.ID, res.GenTokensPerSecond, res.PromptTokensPerSecond, res.LoadTimeMS, time.Now().UTC())
	}
	return res
}

// measureCapacityTarget runs the OOM-safe concurrency ramp for one target, publishes
// live per-level progress over the run's SSE, appends a capacity-history row (kind
// "capacity" + the marshaled curve; success AND failure, best-effort), and persists
// the distilled capacity scalars onto the mapping (lock-respecting) when a viable level
// was found (MaxConcurrency > 0). A ramp that could not sustain even one concurrent
// request is recorded in history but NOT distilled onto the mapping.
func (s *Server) measureCapacityTarget(ctx context.Context, tgt benchmarkTarget, run *benchmarkRun, serverID string) BenchmarkResult {
	res := BenchmarkResult{MappingID: tgt.mapping.ID, GatewayModelName: tgt.mapping.GatewayModelName}
	onLevel := func(n int) {
		run.setCurrentConcurrency(n)
		s.Benchmarks.publish(serverID, run.snapshot())
	}
	capRes, err := s.measureMappingCapacity(ctx, tgt, onLevel)

	// Sanitize the float metrics: a misbehaving upstream can report a NaN/±Inf tok/s in
	// its timings, which would (a) make json.Marshal fail — silently storing an empty
	// curve — and (b) poison the distilled routing metric that CP3 consumes. cleanFloat
	// coerces NaN/±Inf to 0 (= "unknown"), so the curve always marshals and the metric
	// stays finite.
	genAtCap := cleanFloat(capRes.GenTokensPerSecondAtCapacity)
	levels := make([]routing.CapacityLevel, len(capRes.Levels))
	for i, lv := range capRes.Levels {
		lv.AggregateTokensPerSecond = cleanFloat(lv.AggregateTokensPerSecond)
		lv.PerRequestTokensPerSecond = cleanFloat(lv.PerRequestTokensPerSecond)
		lv.VRAMFreePct = cleanFloat(lv.VRAMFreePct)
		lv.RAMFreePct = cleanFloat(lv.RAMFreePct)
		levels[i] = lv
	}

	// Append a capacity-history row (success AND failure) — best-effort; a history
	// write error never fails the run. The curve carries the distilled scalars + the
	// per-level curve; on failure Levels may be partial and the scalars 0.
	report := routing.CapacityReport{
		MaxConcurrency:               capRes.MaxConcurrency,
		RecommendedConcurrency:       capRes.RecommendedConcurrency,
		GenTokensPerSecondAtCapacity: genAtCap,
		MemoryObserved:               capRes.MemoryObserved,
		Levels:                       levels,
	}
	curveJSON, _ := json.Marshal(report)
	errMsg := ""
	if err != nil {
		errMsg = err.Error()
	}
	_ = s.Routes.InsertBenchmarkRun(ctx, routing.BenchmarkRun{
		MappingID:     tgt.mapping.ID,
		ServerID:      tgt.server.ID,
		CreatedAt:     time.Now().UTC(),
		Kind:          "capacity",
		CapacityCurve: string(curveJSON),
		Error:         errMsg,
	})

	if err != nil {
		res.Error = errMsg
		return res
	}
	res.MaxConcurrency = capRes.MaxConcurrency
	res.RecommendedConcurrency = capRes.RecommendedConcurrency
	res.GenTokensPerSecondAtCapacity = genAtCap
	if capRes.MaxConcurrency > 0 {
		_ = s.Routes.UpdateMappingCapacityMetrics(ctx, tgt.mapping.ID, capRes.MaxConcurrency, capRes.RecommendedConcurrency, genAtCap, time.Now().UTC())
	}
	return res
}

// cleanFloat coerces a NaN/±Inf (e.g. a bogus tok/s from a misbehaving upstream's
// timings) to 0 so a capacity curve always marshals to valid JSON and no NaN/Inf
// reaches a persisted/consumed metric.
func cleanFloat(f float64) float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return f
}
