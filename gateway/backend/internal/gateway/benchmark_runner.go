// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
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

// benchmarkDefaultStreamIdle is the idle budget of each benchmark streaming call
// when the edge idle timeout (s.streamIdleTimeout) is disabled
// (benchmarkStreamIdle). A benchmark run has NO client to end it (it executes on
// context.Background), so the watchdog must ALWAYS be on: otherwise a stalled
// upstream (a cold model load/swap that never emits) would hang streamOnce
// forever, so run.finish() (deferred in runBenchmark) never runs, so the server
// stays flagged busy and is permanently excluded from routing. The idle timer
// resets on every event, but an SSE comment counts only before the first event
// and only when the stream's first-data budget (coldStartBudget) exceeds idle. So
// a cold start that emits nothing is bounded by that budget while the upstream
// keeps the connection alive, and by idle when it goes silent
// (watchBenchmarkStream).
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
	// spec is the RESOLVED RuntimeSpec for a server_agent mapping (zero for any
	// other app type, or for a server_agent mapping with no spec yet). Every
	// benchmark/capacity/probe Target builder below resolves its APIToken/
	// APITokenHeader via routing.SpecUpstreamAuth(spec, app) instead of reading
	// app.APIToken directly (security review I3), mirroring
	// routing.Resolver.targetFrom's live-request path: a set/random-mode
	// server_agent child's SEALED spec token is used, everything else falls
	// back to the app token unchanged. Callers populate it at construction
	// (benchmarkTargetFor, from the spec the caller read; the model warmer
	// through benchmarkSpecFor); benchmarkTarget itself never loads it.
	spec routing.RuntimeSpec
	// liveProgressSupport is the mapping's live-progress capability verdict --
	// "" (never determined) | "supported" | "unsupported" -- resolved from its
	// model_mapping_capabilities row at construction
	// (benchmarkLiveProgressSupport), the same way spec is. It is what
	// benchmarkTargetReq puts on Target.LiveProgressSupport for
	// provider.wantsLiveProgress to decide on.
	//
	// It is a FIELD here rather than a read inside benchmarkTargetReq because
	// that builder is pure and is called repeatedly per run (a capacity run
	// is 4 levels x 16 concurrent = 64 streams): resolving the verdict once
	// per target keeps the store read out of the per-stream path, and keeps
	// the builder table-testable with no store at all.
	liveProgressSupport string
	// loadWithoutGenerating marks a target whose model the load core
	// (ensureResidentForRun) starts through the agent's ensure route instead
	// of loading it by generating: a server_agent child that serves only
	// images, on a server whose agent declared runtime_ensure (loadRefusal).
	// The Load starter sets it (loadTargetFor); the VRAM run copies it from
	// its plan (vramRunPlanned.ensure) onto the target it loads.
	loadWithoutGenerating bool
	// mayPreStop marks a target of a manual speed or both run
	// (benchmarkMayPreStop, called by startBenchmark): before its cold pass,
	// the run may stop every running model of the server's agent application
	// (preStopServer). No other constructor sets it, so a scheduled, capacity,
	// vision, Load, VRAM or context-probe run never runs the stop-all (a VRAM
	// run force-stops the launch specs through its own drain, vramDrain).
	mayPreStop bool
	// overrides is the run's override state on its server
	// (benchmarkOverrides), shared by every target of the run. runBenchmark
	// attaches it to each server_agent target with mayPreStop; it is nil on
	// every other target.
	overrides *benchmarkOverrides
}

// streamOnce issues one streaming request and returns time-to-first-token + the
// terminal usage. For upstreams that report no timings it derives a generation rate
// (output tokens / seconds from first token to completion) into usage.TokensPerSecond.
// Note: there is NO wall-clock fallback for PromptPerSecond (prefill rate) — an
// upstream that reports no timings leaves it unknown (0).
//
// The stream runs under the benchmark stream watchdog (watchBenchmarkStream) with
// the target's first-data budget, coldStartBudget.
func (s *Server) streamOnce(ctx context.Context, streamer provider.StreamingClient, target routing.Target, req inference.Request) (time.Duration, inference.Usage, error) {
	return s.streamOnceWithin(ctx, streamer, target, req, coldStartBudget(target, s.benchmarkStreamIdle()))
}

// streamOnceWithin is streamOnce with an explicit first-data budget for the
// watchdog. The load loop's text attempt (loadAttemptFor) calls it directly,
// to cut the attempt's budget to what remains of the loop's own bound.
func (s *Server) streamOnceWithin(ctx context.Context, streamer provider.StreamingClient, target routing.Target, req inference.Request, budget time.Duration) (time.Duration, inference.Usage, error) {
	start := time.Now()
	var firstAt time.Time
	var gotFirst bool
	var usage inference.Usage
	streamErr := s.watchBenchmarkStream(ctx, streamer, target, req, budget, func(ev inference.StreamEvent) error {
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
	// Floor the generation window at minGatewayRateWindow (request_progress.go), the
	// same guard passthrough_usage_scan.go's Anthropic fallback uses, and for the
	// same reason: a warm pass whose whole completion arrives microseconds after the
	// first token divides an exact output-token count by a window of ~0 and yields an
	// implausible rate. It is WORSE here than on either sibling site: measureMapping's
	// result flows straight into UpdateMappingBenchmarkMetrics (below), which
	// overwrites mapping.GenTokensPerSecond with every positive sample -- not the EWMA
	// blend UpdateMappingOpportunisticMetrics applies to a live sample. There is no damping
	// at all, so one implausible benchmark sample would replace the routing value the
	// scorer and a model group's MinTokensPerSecond gate read outright, with nothing
	// to average it back out.
	if usage.TokensPerSecond == 0 && gotFirst {
		usage.TokensPerSecond = flooredRate(usage.OutputTokens, end.Sub(firstAt))
	}
	return ttft, usage, nil
}

// mappingIsImagesOnly reads a mapping's runtime spec (mappingRuntimeSpec)
// and reports whether its EFFECTIVE flavors are images-only
// (mappingSpecIsImagesOnly). The model warmer asks it before it sends a
// candidate a chat prompt, because such a mapping cannot answer one.
//
// Unlike benchmarkSpecFor it keeps "no spec" apart from a failed read: the
// first means the application's flavors, the second means the answer is
// unknown, which it reports as an error so the caller can skip rather than
// guess.
func (s *Server) mappingIsImagesOnly(ctx context.Context, app routing.Application, mappingID string) (bool, error) {
	spec, hasSpec, err := s.mappingRuntimeSpec(ctx, app, mappingID)
	if err != nil {
		return false, err
	}
	return mappingSpecIsImagesOnly(app, spec, hasSpec), nil
}

// benchmarkSpecFor resolves the RuntimeSpec the model warmer's
// benchmarkTarget carries (its .spec field) for later auth resolution via
// routing.SpecUpstreamAuth, mirroring routing.Resolver.targetFrom's
// live-request rule: only a server_agent application's mapping can have a
// spec at all, so any other app type returns a zero spec WITHOUT touching the
// store. A mapping with no spec yet, or a spec-store error, also returns zero
// (best-effort -- a warm is a load-ahead that must never fail because a spec
// lookup hiccupped; the warmer has already skipped a candidate whose spec
// could not be classified, through mappingIsImagesOnly). A zero spec makes
// SpecUpstreamAuth fall back to the app token: today's pre-Runtime-Spec-
// API-Token behaviour, unchanged for every non-server_agent or no-spec case.
// The manual runs and the scheduler read fail-closed instead
// (mappingRuntimeSpec) and hand the spec to benchmarkTargetFor.
func (s *Server) benchmarkSpecFor(ctx context.Context, app routing.Application, mappingID string) routing.RuntimeSpec {
	if app.Type != routing.ProviderServerAgent {
		return routing.RuntimeSpec{}
	}
	spec, ok, err := s.Routes.RuntimeSpecByMapping(ctx, mappingID)
	if err != nil || !ok {
		return routing.RuntimeSpec{}
	}
	return spec
}

// benchmarkLiveProgressSupport resolves a mapping's live-progress verdict
// from its model_mapping_capabilities row, in the vocabulary
// Target.LiveProgressSupport speaks ("" | "supported" | "unsupported").
//
// It exists because the benchmark path has no MappingCandidate: the four
// endpoint handlers and the scheduler start from a plain
// routing.ModelMapping (an authorized benchmark view, or
// MappingsByApplication), and neither of those joins the capability table
// the way ActiveMappingsForModel does. So this is a dedicated KEYED read,
// mirroring routing.Resolver.resolveAffinity's for exactly the same reason
// -- and it translates through the same routing.LiveProgressSupportFromVerdict
// both store drivers use, so the benchmark path cannot drift into its own
// reading of a "no" row.
//
// The verdict MATTERS MORE here than on a live request, which is why the
// column it replaced could not simply be dropped: RouteID is deliberately ""
// on this path and an empty RouteID is never memoized, so a mapping already
// detected as "unsupported" would otherwise pay a 400 plus a retry on every
// single stream of every run, forever, with the rejection memo unable to
// suppress a single one. (benchmarkTargetReq's own comment carries the rest
// of that argument.)
//
// The cost is one keyed read per TARGET -- not per stream: the result is
// cached on benchmarkTarget.liveProgressSupport at construction, so a
// capacity run's 64 streams share the one read. Best-effort, like
// benchmarkSpecFor above: a read failure degrades to "" (never determined),
// the same value an absent row produces, rather than failing a run over a
// parameter hint. A nil s.Routes returns "" without dereferencing it, so a
// Server built without a store still builds targets.
func (s *Server) benchmarkLiveProgressSupport(ctx context.Context, mappingID string) string {
	if s.Routes == nil {
		return ""
	}
	rows, err := s.Routes.MappingCapabilities(ctx, mappingID)
	if err != nil {
		return ""
	}
	row, ok := routing.CapabilityRowsByName(rows)[routing.CapabilityLiveProgress]
	if !ok {
		return ""
	}
	return routing.LiveProgressSupportFromVerdict(row.Verdict)
}

// benchmarkTargetFor builds a benchmarkTarget from the runtime spec its
// caller already read (mappingRuntimeSpec: spec, and hasSpec whether the
// mapping has one) and the mapping's live-progress capability verdict
// (benchmarkLiveProgressSupport), which it reads itself.
//
// It takes the spec instead of reading it, so a starter's refusal check and
// the target's credential (routing.SpecUpstreamAuth) see the same row: a
// second read could see a different one, or fail after the first succeeded.
// The spec is kept only for a server_agent application's mapping that has
// one, the one case routing.Resolver.targetFrom applies a spec in; any other
// target carries a zero spec, which makes SpecUpstreamAuth fall back to the
// application token.
//
// Every construction site that starts from a plain routing.ModelMapping goes
// through it -- the four benchmark/probe/load/vram endpoint handlers and the
// scheduler -- so the verdict cannot be filled at four sites and forgotten
// at the fifth. (The model warmer is the one exception: it already holds a
// routing.MappingCandidate, whose LiveProgressSupport came from
// ActiveMappingsForModel's join, so it fills the field from that instead of
// paying a second read for the same row.)
func (s *Server) benchmarkTargetFor(ctx context.Context, server routing.AIServer, app routing.Application, mapping routing.ModelMapping, spec routing.RuntimeSpec, hasSpec bool) benchmarkTarget {
	tgt := benchmarkTarget{
		server:              server,
		app:                 app,
		mapping:             mapping,
		liveProgressSupport: s.benchmarkLiveProgressSupport(ctx, mapping.ID),
	}
	if hasSpec && app.Type == routing.ProviderServerAgent {
		tgt.spec = spec
	}
	return tgt
}

// benchmarkTargetReq builds the routing.Target + a base inference.Request (from the first
// benchmark prompt) a benchmark issues for a mapping. Shared by the speed (measureMapping)
// and capacity (measureMappingCapacity) paths so both hit an identical target/request.
func benchmarkTargetReq(tgt benchmarkTarget) (routing.Target, inference.Request) {
	apiToken, apiTokenHeader := routing.SpecUpstreamAuth(tgt.spec, tgt.app)
	// LiveProgressSpecType mirrors routing.Resolver.targetFrom exactly: the
	// resolved shape of the launch spec, and ONLY for a server_agent app --
	// for anything else the field stays "" (wantsLiveProgress never reads it
	// there, and a "custom" filled in from a zero spec would be a claim about
	// a child that does not exist).
	liveProgressSpecType := ""
	if tgt.app.Type == routing.ProviderServerAgent {
		liveProgressSpecType = string(routing.EffectiveRuntimeSpecType(tgt.spec))
	}
	target := routing.Target{
		Provider:       tgt.app.Type,
		Endpoint:       routing.ApplicationEndpoint(tgt.server, tgt.app),
		Model:          tgt.mapping.GatewayModelName,
		ProviderModel:  tgt.mapping.AppModelName,
		Timeout:        time.Duration(tgt.app.TimeoutMS) * time.Millisecond,
		APIToken:       apiToken,
		APITokenHeader: apiTokenHeader,
		// The live-progress decision inputs, for the same reason the live
		// request path carries them (provider.wantsLiveProgress's three-layer
		// rule) -- and they matter MORE here than on a live request. RouteID
		// is deliberately "" on this path, and an empty RouteID is never
		// memoized, so the rejection memo can NEVER suppress a repeat: a
		// mapping already detected as "unsupported" would otherwise pay a 400
		// plus a retry on every single one of these streams, forever. A
		// capacity run is 4 levels x 16 concurrent = 64 streams per run.
		// Setting them also restores the parameters for a server_agent child
		// whose resolved spec type genuinely implies a tolerant upstream,
		// which #51's provider-keyed allow-list sent and the three-layer rule
		// otherwise dropped on this path.
		//
		// The measured throughput figure is unaffected either way:
		// streamOnce reads usage only from the single StreamEventCompleted
		// event, which carries the LAST usage-bearing chunk's cumulative
		// numbers (the terminal one), and falls back to its own wall-clock
		// floor when that reports no rate. These parameters only ADD
		// mid-stream chunks, which streamOnce ignores entirely (they reach
		// StreamProgress on delta events, not Usage).
		//
		// The verdict comes from tgt.liveProgressSupport -- the mapping's
		// model_mapping_capabilities row, resolved once per target at
		// construction (benchmarkLiveProgressSupport). This builder stays
		// pure: it never reads the store, which is what lets it be called
		// per stream.
		LiveProgressSupport:  tgt.liveProgressSupport,
		LiveProgressSpecType: liveProgressSpecType,
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
// deltas. Same watchdog (watchBenchmarkStream), first-data budget and upstream
// credential as streamOnce.
func (s *Server) streamCollect(ctx context.Context, streamer provider.StreamingClient, target routing.Target, req inference.Request) (string, error) {
	var sb strings.Builder
	err := s.watchBenchmarkStream(ctx, streamer, target, req, coldStartBudget(target, s.benchmarkStreamIdle()), func(ev inference.StreamEvent) error {
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
	// budget rather than on the first probe. A var so tests can shorten it. The
	// loop's bound is the larger of this and the stream's first-data budget
	// (loadUntilServable), and a load that blocks instead of answering 503 is
	// bounded by the stream watchdog of its attempt (watchBenchmarkStream), or,
	// for an ensure attempt, by the loop's bound itself (ensureLoadAttempt).
	coldLoadResidentMaxWait = 5 * time.Minute
	// coldLoadCallTimeout is a defensive per-call bound for the loaded-probe and the unload
	// when the app carries no positive Timeout — so a wedged upstream can NEVER hang the
	// benchmark run (which would permanently exclude the server from routing). App timeouts
	// are validated positive today; this floor keeps the guarantee independent of that.
	coldLoadCallTimeout = 30 * time.Second
)

// coldStart is ensureColdLoad's verdict for the cold pass.
type coldStart struct {
	// confirmed: the next request starts the target from cold, so the cold-minus-warm delta is its
	// load time. For a server_agent target whose run stops (preStopServer) that means that no
	// model of the application had a process and the target's own row read stopped, or that the
	// agent reported every model the stop-all stopped without a process, the target's own row
	// stopped with pid 0 when it had read backoff, start_failed or crashed, and the clear was
	// written. When every row was quiet at the selection, every launch spec the run unpinned, the
	// target aside, also read stopped with pid 0. For one whose run does not stop it means that
	// the router does not list it, its own status row reads stopped with pid 0 or is absent, and
	// every other row of a non-empty status snapshot reads stopped with pid 0 (agentColdStart).
	// For any other target the loaded-models probe confirmed it absent (ensureColdLoadByEviction).
	confirmed bool
	// rideOutStop: the target's own force_stopped was or may have been written, so the cold
	// pass rides the router's 503 until the clear reaches the agent (coldPassAfterStop).
	rideOutStop bool
}

// ensureColdLoad best-effort guarantees the target model is NOT resident, so the next request
// is a genuine cold load. Its result is confirmed only when it CONFIRMED a cold state (model
// verified absent, or verified not-loaded to begin with). It is unconfirmed when it cannot
// confirm — the caller then reports load-time as unknown rather than a bogus value. The one
// error is a stop-all's failed clear (preStopServer), which may leave launch specs
// force_stopped and ends the measurement before any pass; every other failure degrades to an
// unconfirmed start, and throughput is measured regardless. All calls are bare (no client
// bearer token), safe because a benchmark run idle-gates + routing-excludes the server.
//
// It has two branches:
//   - A server_agent application is never evicted. The agent router has no unload route, and a
//     sibling swap would load an unrelated model and evict the target only under a closed
//     co-residency matrix. A manual speed or both run that may stop runs the stop-all instead
//     (preStopServer): it stops every running model of the application, the target included,
//     and confirms a cold state once the agent reports them all without a process. In any
//     other run agentColdStart asks the router's loaded set and the agent's runtime status,
//     and only a target known not to be resident confirms a cold state, and only when every
//     other model of the application reads stopped in the agent's runtime status. A resident
//     target, one whose residency cannot be read, and one next to a model that does not read
//     stopped get no load time there.
//   - Every other application goes through ensureColdLoadByEviction: an explicit unload where the
//     provider supports it, else a sibling swap, each confirmed through the loaded-models probe.
func (s *Server) ensureColdLoad(ctx context.Context, tgt benchmarkTarget) (coldStart, error) {
	if tgt.app.Type == routing.ProviderServerAgent {
		return s.agentColdStart(ctx, tgt)
	}
	return coldStart{confirmed: s.ensureColdLoadByEviction(ctx, tgt)}, nil
}

// ensureColdLoadByEviction is ensureColdLoad for an application that is not server_agent: it
// confirms a model that is not loaded as cold, and evicts a loaded one, first through the
// provider's unload and then through a sibling swap, confirming either through the
// loaded-models probe (the application's loaded_models_path; without one it cannot confirm).
func (s *Server) ensureColdLoadByEviction(ctx context.Context, tgt benchmarkTarget) bool {
	path, format := routing.EffectiveLoadedModelsProbe(tgt.app)
	lister, hasLister := s.Provider.(provider.LoadedModelLister)
	if !hasLister || strings.TrimSpace(path) == "" {
		return false // no way to observe loaded-state => cannot confirm cold
	}
	model := tgt.mapping.AppModelName
	if strings.TrimSpace(model) == "" {
		return false
	}
	ctx, probeTarget := s.coldProbeTarget(ctx, tgt)
	loaded, err := modelLoaded(ctx, lister, probeTarget, path, format, model)
	if err != nil {
		return false
	}
	if !loaded {
		return true // already cold
	}
	if s.unloadForColdLoad(ctx, lister, probeTarget, path, format, model) {
		return true
	}
	return s.swapForColdLoad(ctx, tgt, lister, probeTarget, path, format)
}

// coldProbeTarget is the target the cold pass's loaded-state reads and its unload use, with the
// context they run under. Their HTTP calls are bounded by the target's Timeout, so an application
// without a positive timeout gets coldLoadCallTimeout: a wedged upstream can never hang the run,
// which would permanently exclude the server. The context carries the application's per-app
// upstream credential, so the unload, the loaded-probe and the sibling-swap stream (via
// streamOnce) carry it too (fail-open).
func (s *Server) coldProbeTarget(ctx context.Context, tgt benchmarkTarget) (context.Context, routing.Target) {
	probeTarget, _ := benchmarkTargetReq(tgt)
	if probeTarget.Timeout <= 0 {
		probeTarget.Timeout = coldLoadCallTimeout
	}
	return s.upstreamAuthCtx(ctx, probeTarget), probeTarget
}

// unloadForColdLoad unloads model where the provider supports an explicit unload, and reports
// whether the loaded-models probe then confirmed it gone.
func (s *Server) unloadForColdLoad(ctx context.Context, lister provider.LoadedModelLister, probeTarget routing.Target, path, format, model string) bool {
	unloader, ok := s.Provider.(provider.ModelUnloader)
	if !ok {
		return false
	}
	done, _ := unloader.UnloadModel(ctx, probeTarget, model)
	return done && s.waitModelUnloaded(ctx, lister, probeTarget, path, format, model)
}

// swapForColdLoad is the swap workaround: it streams a sibling model on the same application to
// evict tgt's model on a single-slot swapper, and reports whether the loaded-models probe then
// confirmed it gone.
func (s *Server) swapForColdLoad(ctx context.Context, tgt benchmarkTarget, lister provider.LoadedModelLister, probeTarget routing.Target, path, format string) bool {
	sib, ok := s.benchmarkSiblingModel(ctx, tgt)
	if !ok {
		return false
	}
	streamer, ok := s.Provider.(provider.StreamingClient)
	if !ok {
		return false
	}
	sibTarget, sibReq := benchmarkTargetReq(tgt)
	sibTarget.Model, sibTarget.ProviderModel, sibReq.Model = sib, sib, sib
	sibReq.MaxTokens = 1                                     // minimal — we only want the swap
	_, _, _ = s.streamOnce(ctx, streamer, sibTarget, sibReq) // best-effort; loads sib, evicts model
	return s.waitModelUnloaded(ctx, lister, probeTarget, path, format, tgt.mapping.AppModelName)
}

// modelLoaded reports whether model is in the loaded set the lister reads from path in format.
func modelLoaded(ctx context.Context, lister provider.LoadedModelLister, target routing.Target, path, format, model string) (bool, error) {
	names, err := lister.LoadedModels(ctx, target, path, format)
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

// agentColdStart is ensureColdLoad for a server_agent target. It confirms a cold start only on
// a server where the load time can mean one thing, the model's own load on an otherwise empty
// server.
//   - A target of a manual speed or both run that may stop (mayPreStop, and the run's
//     benchmarkOverrides allow stops) gets that server from the stop-all (preStopServer): every
//     running model of the application is stopped, the target included, and the overrides are
//     cleared again before the cold pass.
//   - Any other target is measured as it finds the server: the target is known not to be
//     resident and its own status row cannot delay its start (agentTargetResident), and every
//     other model of the application reads stopped in the agent's runtime status
//     (benchmarkOthersStopped). A neighbour that runs would be evicted inside the cold pass
//     under a closed co-residency matrix, and one that can start by itself might start inside
//     it. This branch writes nothing and returns no error.
func (s *Server) agentColdStart(ctx context.Context, tgt benchmarkTarget) (coldStart, error) {
	if tgt.mayPreStop && tgt.overrides != nil && tgt.overrides.stopsAllowed {
		return s.preStopServer(ctx, tgt)
	}
	resident, known := s.agentTargetResident(ctx, tgt)
	if !known || resident || !benchmarkOthersStopped(s.RuntimeStatus.statusSnapshot(tgt.server.ID), tgt.spec.ID) {
		return coldStart{}, nil
	}
	return coldStart{confirmed: true}, nil
}

// agentTargetResident reports whether tgt's model is resident on its server_agent application,
// and whether that is known at all. It evicts nothing and starts nothing.
//
// It reads the loaded set from routing.EffectiveLoadedModelsProbe: the agent router's /running,
// or a loaded_models_path set on the application through the API. A model listed there is
// resident. /running lists only a running child, so the agent's runtime status for tgt's spec is
// read next (agentSpecResidency).
//
// known is false when there is nothing to ask (no app model name, a provider that cannot list
// loaded models), when the loaded-set read failed, or when the spec's status cannot confirm a
// cold start; resident is then false the way an unanswered question is false.
func (s *Server) agentTargetResident(ctx context.Context, tgt benchmarkTarget) (resident, known bool) {
	model := tgt.mapping.AppModelName
	lister, ok := s.Provider.(provider.LoadedModelLister)
	if strings.TrimSpace(model) == "" || !ok {
		return false, false
	}
	ctx, probeTarget := s.coldProbeTarget(ctx, tgt)
	path, format := routing.EffectiveLoadedModelsProbe(tgt.app)
	loaded, err := modelLoaded(ctx, lister, probeTarget, path, format, model)
	if err != nil {
		return false, false
	}
	if loaded {
		return true, true
	}
	return s.agentSpecResidency(tgt)
}

// agentSpecStateStopped is the runtime-status state of a spec with no child that waits for
// nothing: a request for it starts a genuine cold load at once.
const agentSpecStateStopped = "stopped"

// agentSpecResidency is agentTargetResident's reading of the agent's most recent runtime-status
// snapshot, once the router's loaded set has not listed tgt's model. The row is matched by spec
// id, never by model name:
//   - a state with a live process (vramStatesWithProcess: running, starting, draining), or any
//     process id, is resident: a child /running does not list yet or no longer lists, or a
//     start_failed child whose process is still up;
//   - any other state than stopped cannot confirm a cold start: a request for a spec in backoff
//     waits for its backoff timer, and that wait would land in the load time, while
//     not_permitted and pending_vram_unknown refuse the request first;
//   - stopped, a spec the agent has not reported yet, and a target without a spec are not
//     resident: /running has already said so.
func (s *Server) agentSpecResidency(tgt benchmarkTarget) (resident, known bool) {
	if tgt.spec.ID == "" {
		return false, true
	}
	for _, row := range s.RuntimeStatus.statusSnapshot(tgt.server.ID) {
		if row.SpecID != tgt.spec.ID {
			continue
		}
		switch {
		case vramStatesWithProcess[row.State] || row.PID > 0:
			return true, true
		case row.State != agentSpecStateStopped:
			return false, false
		default:
			return false, true
		}
	}
	return false, true
}

// benchmarkOthersStopped reports whether rows is not empty and every row of it reads stopped
// with pid 0, except the row of specID itself, which agentSpecResidency has already read. A
// target without a spec (specID "") has no row of its own, so every row counts, one without a
// spec id included. A neighbour in backoff, not_permitted or pending_vram_unknown has no process
// now, but a pinned or force_running one can start one by itself: when its backoff timer fires,
// or at the next admission wake, such as the target's own release, possibly inside the target's
// passes. This is stricter on purpose than the stop path's benchmarkRowQuiet, which only asks
// whether a row has a process to stop. An empty snapshot is no evidence that nothing runs. The
// agent reports every spec of its document in every frame, so a non-empty snapshot holds every
// spec it manages, but statusSnapshot returns nil both for a server the gateway has no runtime
// status for yet, for example right after a gateway start, and for an agent that manages no
// spec: no row at all cannot tell an empty server from an unknown one.
func benchmarkOthersStopped(rows []RuntimeStatusDTO, specID string) bool {
	if len(rows) == 0 {
		return false
	}
	for _, row := range rows {
		if specID != "" && row.SpecID == specID {
			continue
		}
		if row.State != agentSpecStateStopped || row.PID != 0 {
			return false
		}
	}
	return true
}

// waitModelUnloaded polls the loaded set until `model` is absent, bounded by coldLoadMaxWait.
// A probe error is treated as "not yet confirmed" and retried until the deadline (then false).
func (s *Server) waitModelUnloaded(ctx context.Context, lister provider.LoadedModelLister, target routing.Target, path, format, model string) bool {
	deadline := time.Now().Add(coldLoadMaxWait)
	for {
		loaded, err := modelLoaded(ctx, lister, target, path, format, model)
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
//
// The swap sends that sibling a chat prompt, so a sibling that serves only images
// (mappingIsImagesOnly) is passed over, and so is one whose runtime spec cannot be read,
// since whether it serves only images is then unknown.
func (s *Server) benchmarkSiblingModel(ctx context.Context, tgt benchmarkTarget) (string, bool) {
	mappings, err := s.Routes.MappingsByApplication(ctx, tgt.app.ID)
	if err != nil {
		return "", false
	}
	for _, m := range mappings {
		if m.Status != routing.ServerStatusActive || strings.TrimSpace(m.AppModelName) == "" || m.AppModelName == tgt.mapping.AppModelName {
			continue
		}
		if imagesOnly, err := s.mappingIsImagesOnly(ctx, tgt.app, m.ID); err != nil || imagesOnly {
			continue
		}
		return m.AppModelName, true
	}
	return "", false
}

// measureMapping streams the benchmark prompt twice (cold then warm) and returns the
// measured metrics. Cold TTFT (first request, may trigger a load) minus warm TTFT
// approximates load time; throughput comes from the warm request. The load time is recorded
// only for a cold start ensureColdLoad confirmed: unknown, never bogus. For a server_agent
// target that is a cold start on a server where no other model of the application runs: after
// the run's stop-all (preStopServer), or, in a run that does not stop, when every other model
// reads stopped. So the load time does not include making room: after the stop-all only a
// request can start a neighbour once the agent holds the lifted pins (beginBenchmarkUnpin),
// and a stop-all that cannot tell that it does confirms only when every formerly pinned
// neighbour reads stopped with pid 0 (benchmarkStopSet). An error from ensureColdLoad ends
// the measurement before any pass.
func (s *Server) measureMapping(ctx context.Context, tgt benchmarkTarget) (BenchmarkResult, error) {
	res := BenchmarkResult{MappingID: tgt.mapping.ID, GatewayModelName: tgt.mapping.GatewayModelName}
	streamer, ok := s.Provider.(provider.StreamingClient)
	if !ok {
		return res, errBenchmarkNoStreaming
	}
	target, req := benchmarkTargetReq(tgt)
	// Force a genuine cold start so the cold-minus-warm delta is a real load time (not first-call
	// jitter on an already-resident model). cold.confirmed is false when a cold state could not be
	// guaranteed/verified (no loaded-tracking configured, eviction unavailable, or verification
	// timed out). For a server_agent target whose run stops, it is false when the stop-all stopped
	// nothing it had to (an ineligible spec, traffic in flight, no fresh status frame), when what
	// it stopped was not confirmed quiet or its clear was taken over, or when a launch spec the
	// run unpinned, the target aside, reads other than stopped with pid 0 while the run has no
	// sign that the agent holds the lifted pins (preStopServer). For one whose run does not stop,
	// it is false when the target is resident at the start or its residency cannot be read, when
	// another model of its application does not read stopped in the agent's runtime status, or on
	// a server without any runtime status. Then we do NOT emit a load time (unknown), never a
	// bogus value. Throughput is measured regardless. An error, a stop-all's failed clear, is
	// this target's result error, not the run's (of the stop-all's and the unpin's failures, only
	// a failed re-pin reaches the run's error, endBenchmarkUnpin): no pass runs, and
	// measureSpeedTarget writes nothing. When the stop-all wrote, or may have written,
	// the target's own force_stopped (cold.rideOutStop), the cold pass rides the router's 503
	// until the clear reaches the agent (coldPassAfterStop).
	cold, err := s.ensureColdLoad(ctx, tgt)
	if err != nil {
		res.Error = err.Error()
		return res, err
	}
	var coldTTFT time.Duration
	if cold.rideOutStop {
		coldTTFT, err = s.coldPassAfterStop(ctx, streamer, target, req)
	} else {
		coldTTFT, _, err = s.streamOnce(ctx, streamer, target, req)
	}
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
	if cold.confirmed && coldTTFT > 0 && warmTTFT > 0 && coldTTFT > warmTTFT {
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
// "capacity"), or "vision" (the image-acceptance probe: on a definitive verdict, writes
// the mapping's `vision` capability row, sourced vision_benchmark, subject to the same
// precedence rank every capability writer obeys -- routing.WritableCapabilityRows -- so
// it never overwrites an operator's CapabilitySourceManual verdict; always appends a
// kind=="vision" history row regardless — success or inconclusive). An empty mode is
// treated as "speed".
//
// A manual speed or both run's server_agent targets (mayPreStop) share one override
// state (benchmarkOverrides): beginBenchmarkUnpin decides once whether the run may stop
// the server's running agent models before each agent target's cold pass
// (preStopServer) and unpins the server's pinned launch specs for the run, and
// endBenchmarkUnpin pins them again and rewrites the override lease at the end.
func (s *Server) runBenchmark(ctx context.Context, run *benchmarkRun, serverID string, targets []benchmarkTarget, mode string) {
	var runErr string
	defer func() {
		run.finish(runErr)
		s.Benchmarks.publish(serverID, run.snapshot()) // terminal frame after finish so subscribers see Running=false
	}()
	// Registered after the finish defer, so it runs first, while the run still holds the
	// server's reservation and operator writes to its launch specs stay refused.
	ov := s.beginBenchmarkUnpin(ctx, run, serverID, targets)
	defer s.endBenchmarkUnpin(ctx, run, serverID, ov, &runErr)
	for _, tgt := range targets {
		if tgt.mayPreStop && tgt.app.Type == routing.ProviderServerAgent {
			tgt.overrides = ov
		}
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
			res, speedErr = s.measureBothTarget(ctx, tgt, run, serverID)
		case "vision":
			res = s.runVisionTarget(ctx, tgt)
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

// measureBothTarget is runBenchmark's "both" mode for one target: speed first, then
// capacity, so capacity's UpdateMappingCapacityMetrics runs LAST and metrics_source ends
// "capacity". The two results are merged so the poll sees both the speed and capacity
// scalars for the mapping. speedErr is the speed measurement's own error, captured before
// the merge: the speed-history row records only it, so a successful speed benchmark whose
// capacity ramp failed is not mislabeled as failed.
func (s *Server) measureBothTarget(ctx context.Context, tgt benchmarkTarget, run *benchmarkRun, serverID string) (res BenchmarkResult, speedErr string) {
	res = s.measureSpeedTarget(ctx, tgt)
	speedErr = res.Error // capture BEFORE the merge below
	capRes := s.measureCapacityTarget(ctx, tgt, run, serverID)
	res.MaxConcurrency = capRes.MaxConcurrency
	res.RecommendedConcurrency = capRes.RecommendedConcurrency
	res.GenTokensPerSecondAtCapacity = capRes.GenTokensPerSecondAtCapacity
	if res.Error == "" {
		res.Error = capRes.Error
	}
	return res, speedErr
}

// runVisionTarget is runBenchmark's "vision" mode for one target: the image-acceptance
// probe (measureVisionTarget), the capability write-back of a definitive verdict, and the
// target's vision-history row.
func (s *Server) runVisionTarget(ctx context.Context, tgt benchmarkTarget) BenchmarkResult {
	dataURL, tokens := pickVisionImage() // random embedded asset
	res := s.measureVisionTarget(ctx, tgt, s.Portal.VisionProbeMode(ctx), dataURL, tokens)
	if res.VisionCapable != nil {
		// A definitive verdict is projected as the mapping's `vision`
		// capability row, sourced CapabilitySourceVisionBenchmark
		// (#49-3), rank 2 (routing.capabilitySourceRank): a real
		// measurement -- the gateway sent an actual image to the
		// actual upstream and read the actual answer -- outranks a
		// probe (rank 1), but NOT an operator's CapabilitySourceManual
		// verdict (rank 3). So, like every other capability writer,
		// this reads the mapping's current rows and asks
		// routing.WritableCapabilityRows which of them it may
		// actually write, instead of writing unconditionally.
		//
		// An operator explicitly starting this run authorizes
		// MEASURING, not overwriting whatever verdict is already on
		// file -- a migrated pre-78 manual row is exactly as
		// reachable here as it is for a probe (migration 78 maps a
		// mapping whose metrics_source was 'manual' onto
		// CapabilitySourceManual), and unconditionally overwriting it
		// would be a net LOSS of protection versus the
		// metrics_locked guard this table replaced. An INCONCLUSIVE
		// probe (VisionCapable nil) writes nothing at all -- "unknown"
		// is the absence of a row, so there is no way for it to clear
		// a stored verdict, which the pre-row vision_capable bool
		// could not express.
		//
		// Unlike the bool it replaces this write carries no
		// metrics_locked guard, because the table does not have one:
		// routing.MappingStore.UpsertMappingCapabilities carries the
		// argument for why a capability is not a number an operator
		// pins. Best-effort like the history row below: a failed read
		// writes nothing at all (writing blind would be exactly the
		// overwrite the rank rule forbids), and a failed write is
		// logged and never fails the run.
		verdict := routing.CapabilityNo
		if *res.VisionCapable {
			verdict = routing.CapabilityYes
		}
		reported := []routing.CapabilityRow{{
			Capability: routing.CapabilityVision, Verdict: verdict,
			Source: routing.CapabilitySourceVisionBenchmark, CheckedAt: time.Now().UTC(),
		}}
		if stored, err := s.Routes.MappingCapabilities(ctx, tgt.mapping.ID); err != nil {
			slog.Debug("vision benchmark: capability read failed", "mapping_id", tgt.mapping.ID, "err", err)
		} else if rows := routing.WritableCapabilityRows(reported, routing.CapabilityRowsByName(stored)); len(rows) > 0 {
			if err := s.Routes.UpsertMappingCapabilities(ctx, tgt.mapping.ID, rows); err != nil {
				slog.Debug("vision benchmark: capability write-back failed", "mapping_id", tgt.mapping.ID, "err", err)
			}
		}
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
	return res
}

// benchmarkContextAttribution says how benchmarkContextSize attributes a
// probe answer to the target mapping.
type benchmarkContextAttribution int

const (
	// contextByPath is the speed run's rule: a {model} path attributes the
	// probe's answer directly (provider.PickModelContextSize), any other path
	// by exact name.
	contextByPath benchmarkContextAttribution = iota
	// contextDirect is the context probe's rule: always
	// provider.PickModelContextSize, because it has just loaded this model.
	contextDirect
)

// benchmarkContextSource says which source answered benchmarkContextSize.
type benchmarkContextSource int

const (
	// contextSourceNone: nothing answered, and the size is 0 (unknown).
	contextSourceNone benchmarkContextSource = iota
	// contextSourceProbe: the synchronous probe answered. measureSpeedTarget
	// writes this size to the mapping.
	contextSourceProbe
	// contextSourceTelemetry: the agent's status stream answered. It is never
	// written here, because the agent ingest (writeBackRuntimeContext) owns
	// that write.
	contextSourceTelemetry
)

// benchmarkTelemetryContextWait bounds how long benchmarkContextSize waits for
// a fresh agent status frame. A guess to validate on hardware, like
// vramMeasuredWaitBound: two default 1 s agent ticks plus the agent's 2 s
// collect timeout. A var so tests can shorten it.
var benchmarkTelemetryContextWait = 5 * time.Second

// benchmarkContextSize returns the target model's context size for a benchmark
// result, and which source gave it. Both callers ask right after a stream to
// the model, so it is resident.
//
// The synchronous probe comes first (benchmarkProbeContextSize). For a
// server_agent application the agent's own per-spec probe is the fallback
// (benchmarkTelemetryContextSize): it covers the spec types whose child has no
// llama.cpp-shaped /props, such as vLLM, TGI and Ollama. Nothing found is
// (0, contextSourceNone), which the history row shows as unknown.
func (s *Server) benchmarkContextSize(ctx context.Context, tgt benchmarkTarget, attr benchmarkContextAttribution) (int, benchmarkContextSource) {
	if size := s.benchmarkProbeContextSize(ctx, tgt, attr); size > 0 {
		return size, contextSourceProbe
	}
	if size := s.benchmarkTelemetryContextSize(ctx, tgt); size > 0 {
		return size, contextSourceTelemetry
	}
	return 0, contextSourceNone
}

// benchmarkContextInBounds reports whether size is a context size a benchmark
// accepts: positive, and no larger than benchmarkMaxContextSize.
func benchmarkContextInBounds(size int) bool {
	return size > 0 && size <= benchmarkMaxContextSize
}

// benchmarkProbeContextSize asks the application's effective context probe
// path (routing.EffectiveContextProbePath) for the target model's context
// size, with the mapping's credential (routing.SpecUpstreamAuth). For a
// server_agent application without a stored path, on an agent that declares
// runtime_upstream_props, that is the router's /upstream/{model}/props: it
// forwards to the running child's /props with the credential intact, so a
// key-protected llama.cpp child answers too. It returns 0 when there is no
// path, the probe fails, or the answer is out of bounds. A 401 or 403 is the
// one failure it logs: the gateway holds that credential, so the spec's token
// is wrong.
func (s *Server) benchmarkProbeContextSize(ctx context.Context, tgt benchmarkTarget, attr benchmarkContextAttribution) int {
	path := routing.EffectiveContextProbePath(tgt.app, s.AgentFeatures.Has(tgt.server.ID, RuntimeUpstreamPropsFeature))
	prober, ok := s.Provider.(provider.ModelInfoProber)
	if !ok || path == "" {
		return 0
	}
	apiToken, apiTokenHeader := routing.SpecUpstreamAuth(tgt.spec, tgt.app)
	pt := routing.Target{Provider: tgt.app.Type, Endpoint: routing.ApplicationEndpoint(tgt.server, tgt.app), Timeout: time.Duration(tgt.app.TimeoutMS) * time.Millisecond, APIToken: apiToken, APITokenHeader: apiTokenHeader}
	pctx := s.upstreamAuthCtx(ctx, pt)
	infos, err := prober.ProbeModelInfo(pctx, pt, provider.ExpandModelPath(path, tgt.mapping.AppModelName))
	if err != nil {
		if errors.Is(err, provider.ErrAuthRejected) {
			slog.Warn("benchmark: context probe rejected by the upstream (401/403): check the runtime spec's API token", "mapping_id", tgt.mapping.ID, "err", err)
		}
		return 0
	}
	direct := attr == contextDirect || strings.Contains(path, "{model}")
	if size := benchmarkAttributedContextSize(infos, tgt.mapping.AppModelName, direct); benchmarkContextInBounds(size) {
		return size
	}
	return 0
}

// The agent's context_probe values that settle the answer: "na" (the spec
// configures no context probe) and "router" (llama-server in router mode,
// which has no measurable context). No later frame changes either.
const (
	agentContextProbeNA     = "na"
	agentContextProbeRouter = "router"
)

// benchmarkTelemetryContextSize reads the target spec's context size from the
// agent's runtime-status stream: the size the agent's own loopback probe
// found (ContextSize, with ContextProbe saying how that probe went). It
// answers only for a server_agent target with a launch spec, on an agent that
// declares runtime_model_probe, the same gate the catalog applies before it
// trusts these fields.
//
// Rows are matched by spec_id, never by model name, because two specs can
// serve the same upstream name. The snapshot answers first
// (benchmarkSnapshotContextSize). Otherwise the first fresh frame whose row
// is running and carries a probe result is final
// (benchmarkFrameContextSize): the agent re-probes a failure on every
// collect, so one fresh unreachable is as good as it gets. A done ctx, a
// closed stream or benchmarkTelemetryContextWait ends the wait with 0.
func (s *Server) benchmarkTelemetryContextSize(ctx context.Context, tgt benchmarkTarget) int {
	if tgt.app.Type != routing.ProviderServerAgent || tgt.spec.ID == "" || !s.AgentFeatures.Has(tgt.server.ID, runtimeModelProbeFeature) {
		return 0
	}
	snap, frames, unsub := s.RuntimeStatus.subscribe(tgt.server.ID)
	defer unsub()
	if size, final := benchmarkSnapshotContextSize(snap, tgt.spec.ID); final {
		return size
	}
	timer := time.NewTimer(benchmarkTelemetryContextWait)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return 0
		case <-timer.C:
			return 0
		case frame, open := <-frames:
			if !open {
				return 0
			}
			if size, final := benchmarkFrameContextSize(frame, tgt.spec.ID); final {
				return size
			}
		}
	}
}

// benchmarkSpecStatus returns specID's row of one status frame.
func benchmarkSpecStatus(frame []RuntimeStatusDTO, specID string) (RuntimeStatusDTO, bool) {
	for _, row := range frame {
		if row.SpecID == specID {
			return row, true
		}
	}
	return RuntimeStatusDTO{}, false
}

// benchmarkSnapshotContextSize decides on the snapshot a subscription starts
// from. A running row with an in-bounds size answers. A row whose probe is
// "na" or "router" answers 0, since no fresh frame would change it. Anything
// else, including an unreachable probe from before the benchmark loaded the
// model, is not final.
func benchmarkSnapshotContextSize(snap []RuntimeStatusDTO, specID string) (size int, final bool) {
	row, ok := benchmarkSpecStatus(snap, specID)
	switch {
	case !ok:
		return 0, false
	case row.State == "running" && benchmarkContextInBounds(row.ContextSize):
		return row.ContextSize, true
	case row.ContextProbe == agentContextProbeNA || row.ContextProbe == agentContextProbeRouter:
		return 0, true
	default:
		return 0, false
	}
}

// benchmarkFrameContextSize decides on one fresh frame: a running row with a
// probe result is final, with its size when in bounds and 0 otherwise.
func benchmarkFrameContextSize(frame []RuntimeStatusDTO, specID string) (size int, final bool) {
	row, ok := benchmarkSpecStatus(frame, specID)
	if !ok || row.State != "running" || row.ContextProbe == "" {
		return 0, false
	}
	if benchmarkContextInBounds(row.ContextSize) {
		return row.ContextSize, true
	}
	return 0, true
}

// benchmarkAttributedContextSize picks the target model's size out of a probe
// answer. direct attributes a per-model answer to the model itself
// (provider.PickModelContextSize: a name match, else the first positive size).
// Otherwise the answer describes whatever the upstream has loaded, so only an
// entry named exactly like the model counts, and the last in-bounds one wins.
func benchmarkAttributedContextSize(infos []provider.ModelInfo, model string, direct bool) int {
	if direct {
		return provider.PickModelContextSize(infos, model)
	}
	size := 0
	for _, info := range infos {
		if info.Name == model && benchmarkContextInBounds(info.ContextSize) {
			size = info.ContextSize
		}
	}
	return size
}

// runContextProbe warm-loads the target's model (forcing a load if not resident) then reads its
// context size (benchmarkContextSize), and REPORTS the size through the run status. It does NOT
// persist (the frontend fills the form field; the user saves manually). Reuses the benchmark
// server reservation so it is mutually exclusive with benchmarks + live traffic.
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
	//    on first request). Reuses streamOnce and its watchdog (watchBenchmarkStream).
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
	// 2) Read the context size (the model is now resident), always attributed directly to THIS
	//    mapping, because we just loaded it (contextDirect). No store write.
	res.ContextSize, _ = s.benchmarkContextSize(ctx, tgt, contextDirect)
}

// measureSpeedTarget runs the speed benchmark for one target: measure throughput/load,
// read the context size (benchmarkContextSize), and persist (lock-respecting). It returns
// the BenchmarkResult; the caller handles history/addResult/publish.
func (s *Server) measureSpeedTarget(ctx context.Context, tgt benchmarkTarget) BenchmarkResult {
	res, err := s.measureMapping(ctx, tgt)
	if err == nil {
		// Read the context FIRST (the model is resident from measureMapping's warm
		// pass, so no loaded-registry gate is needed, unlike the health-loop pass).
		// Only a probe answer is written here (UpdateMappingContextProbe sets
		// metrics_source='probe'); the agent ingest already writes a telemetry
		// answer (writeBackRuntimeContext).
		size, src := s.benchmarkContextSize(ctx, tgt, contextByPath)
		res.ContextSize = size
		if src == contextSourceProbe {
			_ = s.Routes.UpdateMappingContextProbe(ctx, tgt.mapping.ID, size, time.Now().UTC())
		}
		// Benchmark metrics LAST, so a run that measured a positive value ends
		// metrics_source "benchmark"; one that measured nothing writes nothing
		// (UpdateMappingBenchmarkMetrics), so a probed context then leaves "probe".
		// Both writes skip a locked mapping. This write never touches
		// context_size, so a probed context survives.
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
