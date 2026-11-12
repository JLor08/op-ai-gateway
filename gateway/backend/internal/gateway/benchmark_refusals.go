// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
)

// runtimeEnsureFeature is the agent-DECLARED capability name meaning: this
// agent's router answers POST /ensure/{model}, which starts a managed child
// and waits until it is healthy without forwarding a request to it. It is not
// one of this gateway's own advertised features (gatewayAgentFeatures): the
// gateway only consumes it, through s.AgentFeatures.Has, to decide whether an
// images-only or audio-only agent child can be loaded at all (loadRefusal).
const runtimeEnsureFeature = "runtime_ensure"

// The refusals a manual run makes on a mapping it cannot run as asked,
// before it reserves the server. Each is 409 at the trigger: the
// request is well-formed, and what conflicts with it is the mapping's
// configuration or its agent. They are stable API error codes, each with its
// own portal label.
const (
	// codeBenchmarkImagesOnly: the mapping serves only images
	// (mappingSpecIsImagesOnly), and the run would send it a chat prompt it
	// cannot answer, or it is a Load of a mapping that nothing can start
	// without a request. An application- or server-scope run answers it too
	// when every mapping in its scope serves only images, so it has nothing
	// to measure (refuseNothingRunnable).
	codeBenchmarkImagesOnly = "benchmark.images_only"
	msgBenchmarkImagesOnly  = "this model, or every model in the run's scope, serves only images, so no chat prompt is sent"

	// codeBenchmarkAudioOnly is codeBenchmarkImagesOnly for a mapping that
	// serves only audio, speech or transcription (mappingSpecIsAudioOnly): the
	// refusal of the runs that would send it a chat prompt -- the vision probe,
	// the context probe and a Load that cannot start it without one. The speed,
	// capacity and both modes measure such a mapping's audio instead
	// (benchmarkViewTarget) and never answer it: one they cannot measure is
	// refused with codeBenchmarkSpeechUnavailable (msgBenchmarkAudioUnmeasurable).
	codeBenchmarkAudioOnly = "benchmark.audio_only"
	msgBenchmarkAudioOnly  = "this model, or every model in the run's scope, serves audio only, so no chat prompt is sent"

	// codeBenchmarkDecisionsOnly is codeBenchmarkImagesOnly for a mapping that
	// serves only decisions (mappingSpecIsDecisionsOnly): every run that would
	// send it a chat prompt is refused. Checked after the images and audio
	// refusals, so a list naming those too keeps their codes.
	codeBenchmarkDecisionsOnly = "benchmark.decisions_only"
	msgBenchmarkDecisionsOnly  = "this model, or every model in the run's scope, serves decisions only, so no chat prompt is sent"

	// codeBenchmarkSpeechUnavailable: the SPEECH benchmark (?mode=speech) has no
	// target in the run's scope. It is the speech benchmark's own refusal, the twin of
	// the two above for the one mode that inverts their audio-only rule: it runs on
	// text-to-speech mappings, so what it refuses is a scope with none -- a mapping
	// that does not synthesize speech, or a voice-cloning model that lists no preset
	// voice and so cannot synthesize without a reference voice (speechBenchmarkSkip). An application- or server-scope
	// run answers it too when every mapping in its scope is skipped.
	codeBenchmarkSpeechUnavailable = "benchmark.speech_unavailable"
	msgBenchmarkSpeechUnavailable  = "this model, or every model in the run's scope, cannot be benchmarked for speech: it does not synthesize speech, or it clones a voice and needs a reference"

	// msgBenchmarkAudioUnmeasurable is codeBenchmarkSpeechUnavailable's message for the
	// speed, capacity and both modes: every view of the run's scope serves audio only, and
	// none of them can be measured as audio (refuseNothingRunnable) -- the gateway routes it
	// no audio request, it has no speech or transcription verdict yet, or it is a clone model
	// that lists no preset voice, so it cannot synthesize without a reference.
	msgBenchmarkAudioUnmeasurable = "this model, or every model in the run's scope, serves audio only and cannot be measured yet: it has no speech or transcription verdict, or it clones a voice and lists no preset voice"

	// msgBenchmarkScopeUnmeasurable is codeBenchmarkSpeechUnavailable's message for a
	// speed, capacity or both run whose scope has nothing runnable and mixes the two kinds
	// that cannot be measured (refuseNothingRunnable): views that serve only images and
	// audio-only views whose audio cannot be measured yet. Neither single-kind answer would
	// be true -- images_only would tell the operator that the audio model serves images --
	// so the message names both, under the audio code: an audio model is in the scope, and
	// what it lacks (a verdict, a preset voice) is what can still change. The portal shows
	// one label for this code that states all three cases, so the wording here is for API
	// clients and logs.
	msgBenchmarkScopeUnmeasurable = "no model in the run's scope can be measured: each serves only images, or serves audio only and cannot be measured yet (it has no speech or transcription verdict, or it clones a voice and lists no preset voice)"

	// codeBenchmarkAgentEnsureUnsupported: an images-only agent child loads
	// only through the agent's ensure route, and the server's agent has not
	// declared runtime_ensure. That is an older agent, or one that has not
	// reported since the gateway restarted: the feature registry is in memory.
	// An audio-only or decisions-only agent child is refused with the same
	// code, and for now with this message's images wording: the audio and
	// decision agent paths are not built yet.
	codeBenchmarkAgentEnsureUnsupported = "benchmark.agent_ensure_unsupported"
	msgBenchmarkAgentEnsureUnsupported  = "this model serves only images, and starting it without a chat prompt needs the agent feature runtime_ensure (agent 0.8.0 or newer), or the agent has not reported since the gateway restarted"

	// codeBenchmarkSpecForceStopped: the mapping's runtime spec carries the
	// admin override force_stopped. The agent refuses every start while it is
	// set, and a Load must not override desired state (ADR-026).
	codeBenchmarkSpecForceStopped = "benchmark.spec_force_stopped"
	msgBenchmarkSpecForceStopped  = "this model's runtime spec is force-stopped; clear the admin override before loading it"

	// codeClusterHeadUnsupported: the mapping's spec is a llama.cpp RPC
	// cluster head. A head starts only members first, through the cluster
	// orchestration, never by a Load's own ensure or chat prompt, and a VRAM
	// run would measure the head's own share of the model alone. The Load,
	// the VRAM run, the speed, capacity and both modes and the audio
	// benchmark refuse it (an application- or server-scope run skips it,
	// benchmarkSkippedClusterHead).
	codeClusterHeadUnsupported = "runtime.cluster_head_unsupported"
	msgClusterHeadUnsupported  = "this model is a llama.cpp RPC cluster head, which loads and benchmarks do not support yet; it starts with its members when a request needs it"
)

// The refusal sentinels, one per code above. writeBenchmarkError maps each to
// its 409 (benchmarkErrRows).
var (
	errBenchmarkImagesOnly             = errors.New(codeBenchmarkImagesOnly)
	errBenchmarkAudioOnly              = errors.New(codeBenchmarkAudioOnly)
	errBenchmarkDecisionsOnly          = errors.New(codeBenchmarkDecisionsOnly)
	errBenchmarkSpeechUnavailable      = errors.New(codeBenchmarkSpeechUnavailable)
	errBenchmarkAudioUnmeasurable      = errors.New(codeBenchmarkSpeechUnavailable + " (audio)")
	errBenchmarkScopeUnmeasurable      = errors.New(codeBenchmarkSpeechUnavailable + " (images and audio)")
	errBenchmarkAgentEnsureUnsupported = errors.New(codeBenchmarkAgentEnsureUnsupported)
	errBenchmarkSpecForceStopped       = errors.New(codeBenchmarkSpecForceStopped)
	errBenchmarkClusterHeadUnsupported = errors.New(codeClusterHeadUnsupported)
)

// The reasons loadRefusal gives, a closed set. Each is the last part of the
// error code the Load starter answers for it (benchmark.<reason>, and
// runtime.<reason> for the cluster head's, through loadRefusalErrs).
const (
	loadRefusalImagesOnly             = "images_only"
	loadRefusalAudioOnly              = "audio_only"
	loadRefusalDecisionsOnly          = "decisions_only"
	loadRefusalAgentEnsureUnsupported = "agent_ensure_unsupported"
	loadRefusalSpecForceStopped       = "spec_force_stopped"
	loadRefusalClusterHead            = "cluster_head_unsupported"
)

// loadRefusalErrs maps each loadRefusal reason to the sentinel the Load
// starter answers.
var loadRefusalErrs = map[string]error{
	loadRefusalImagesOnly:             errBenchmarkImagesOnly,
	loadRefusalAudioOnly:              errBenchmarkAudioOnly,
	loadRefusalDecisionsOnly:          errBenchmarkDecisionsOnly,
	loadRefusalAgentEnsureUnsupported: errBenchmarkAgentEnsureUnsupported,
	loadRefusalSpecForceStopped:       errBenchmarkSpecForceStopped,
	loadRefusalClusterHead:            errBenchmarkClusterHeadUnsupported,
}

// benchmarkSkippedImagesOnly is BenchmarkResult.Skipped for a mapping an
// application- or server-scope run did not measure because it serves only
// images, one value of that field's closed set.
const benchmarkSkippedImagesOnly = "images_only"

// benchmarkSkippedAudioOnly is benchmarkSkippedImagesOnly for a mapping that
// serves only audio (speech or transcription): the vision probe's skip of every
// such mapping, and the speed, capacity and both modes' skip of one they cannot
// measure as audio either -- no speech or transcription verdict, or no audio
// request routed to it (benchmarkViewTarget).
const benchmarkSkippedAudioOnly = "audio_only"

// benchmarkSkippedDecisionsOnly is benchmarkSkippedImagesOnly for a mapping that
// serves only decisions: every run skips it, since it answers no chat prompt and
// has no audio to measure.
const benchmarkSkippedDecisionsOnly = "decisions_only"

// benchmarkSkippedClusterHead is BenchmarkResult.Skipped for a mapping whose spec
// is a llama.cpp RPC cluster head, which no benchmark measures yet
// (codeClusterHeadUnsupported).
const benchmarkSkippedClusterHead = "cluster_head"

// The two values of BenchmarkResult.Skipped a speech run (?mode=speech) records,
// for a mapping an application- or server-scope run did not measure
// (partitionSpeechBenchmarkViews): benchmarkSkippedNotSpeech when it does not
// synthesize speech (a chat model, a transcriber, an unprobed model), and
// benchmarkSkippedSpeechClone when it is a voice-cloning model with no preset voice
// to name (#306), whose synthesis needs a reference voice the benchmark does not have.
const (
	benchmarkSkippedNotSpeech   = "not_speech"
	benchmarkSkippedSpeechClone = "speech_clone"
)

// benchmarkSpecUnreadable prefixes the error an application- or server-scope
// run records for a mapping whose runtime spec could not be read.
const benchmarkSpecUnreadable = "runtime spec unreadable; not benchmarked (no chat prompt sent): "

// mappingRuntimeSpec is the fail-closed read of a mapping's runtime spec that
// the manual runs and the benchmark scheduler decide on. Only a server_agent
// application's mapping can have a spec, so any other application answers
// (zero, false, nil) without a store read. A server_agent mapping answers the
// store's own (spec, ok, err): no spec is (zero, false, nil), and a failed
// read is an error the caller refuses or skips on. Guessing the
// application's flavors instead would send the very chat prompt the
// images-only check exists to stop.
func (s *Server) mappingRuntimeSpec(ctx context.Context, app routing.Application, mappingID string) (routing.RuntimeSpec, bool, error) {
	if app.Type != routing.ProviderServerAgent {
		return routing.RuntimeSpec{}, false, nil
	}
	return s.Routes.RuntimeSpecByMapping(ctx, mappingID)
}

// mappingSpecIsImagesOnly reports whether a mapping's EFFECTIVE flavors are
// images-only (routing.FlavorsAreImagesOnly), resolved by
// routing.EffectiveFields from a spec the caller already read, with the
// precedence routing.Resolver.targetFrom applies: a server_agent mapping's
// spec is the authority whenever it has one, even one stored as [], and the
// application's list stands otherwise.
func mappingSpecIsImagesOnly(app routing.Application, spec routing.RuntimeSpec, hasSpec bool) bool {
	return routing.FlavorsAreImagesOnly(routing.EffectiveFields(app, spec, hasSpec).APIFlavors)
}

// mappingSpecIsAudioOnly is mappingSpecIsImagesOnly's speech twin: whether a
// mapping's EFFECTIVE flavors are audio-only (routing.FlavorsAreAudioOnly),
// resolved the same way, so a server_agent mapping's spec is the authority
// whenever it has one and the application's list stands otherwise.
func mappingSpecIsAudioOnly(app routing.Application, spec routing.RuntimeSpec, hasSpec bool) bool {
	return routing.FlavorsAreAudioOnly(routing.EffectiveFields(app, spec, hasSpec).APIFlavors)
}

// mappingSpecIsDecisionsOnly is mappingSpecIsAudioOnly's decision twin: whether a
// mapping's EFFECTIVE flavors are decisions-only (routing.FlavorsAreDecisionsOnly),
// resolved the same way. Every check of it comes after the images and audio ones.
func mappingSpecIsDecisionsOnly(app routing.Application, spec routing.RuntimeSpec, hasSpec bool) bool {
	return routing.FlavorsAreDecisionsOnly(routing.EffectiveFields(app, spec, hasSpec).APIFlavors)
}

// AudioTargetFlavors is the flavor half of the decision whether a mapping is an audio target
// of the speed, capacity and both modes (benchmarkViewTarget): its EFFECTIVE flavors serve
// audio only -- not images-only, and no text flavor, so a chat model that also transcribes
// or speaks keeps its text benchmark -- and the gateway routes it audio requests
// (routing.MappingServesAPIFlavor for openai_audio). Whether it is one then depends on its
// capability rows. It reads nothing but its arguments (the spec the caller read, hasSpec
// whether the mapping has one), so the app-health loop applies it before it reads any
// capability row of a mapping it asks the automatic audio run about.
func AudioTargetFlavors(app routing.Application, spec routing.RuntimeSpec, hasSpec bool) bool {
	return !mappingSpecIsImagesOnly(app, spec, hasSpec) && mappingSpecIsAudioOnly(app, spec, hasSpec) &&
		routing.MappingServesAPIFlavor(app, routing.EffectiveFields(app, spec, hasSpec), routing.APIFlavorOpenAIAudio)
}

// loadRefusal is the one decision whether a Load of a mapping can succeed:
// "" when it can, else one of the loadRefusal* reasons. It reads nothing but
// its arguments: the mapping's application, the spec the caller already read
// (hasSpec: whether the mapping has one), and ensureSupported, whether the
// mapping's server declared runtime_ensure.
//
// A mapping that serves only images, only audio or only decisions cannot
// load by generating, because the Load's one-token chat completion is exactly
// what it cannot answer:
//   - a server_agent child loads through the agent's ensure route instead, so
//     it is refused only while its agent has not declared that route;
//   - on any other application nothing can start it without a request, so it
//     is refused outright, with the reason for its kind (a list naming several
//     non-text flavors takes the first of images-only, audio-only,
//     decisions-only).
//
// Then a spec carrying the admin override force_stopped refuses every Load,
// text, images-only, audio-only and decisions-only alike. The non-text
// reasons come first because clearing the override would not make such a Load
// possible.
//
// A llama.cpp RPC cluster head is refused before all of them: it starts only
// members first, through the cluster orchestration, which a Load does not run,
// and no other setting of the spec changes that.
func loadRefusal(app routing.Application, spec routing.RuntimeSpec, hasSpec bool, ensureSupported bool) string {
	agent := app.Type == routing.ProviderServerAgent
	if agent && hasSpec && spec.ClusterEnabled {
		return loadRefusalClusterHead
	}
	imagesOnly := mappingSpecIsImagesOnly(app, spec, hasSpec)
	audioOnly := mappingSpecIsAudioOnly(app, spec, hasSpec)
	decisionsOnly := mappingSpecIsDecisionsOnly(app, spec, hasSpec)
	if imagesOnly || audioOnly || decisionsOnly {
		if !agent {
			switch {
			case imagesOnly:
				return loadRefusalImagesOnly
			case audioOnly:
				return loadRefusalAudioOnly
			}
			return loadRefusalDecisionsOnly
		}
		if !ensureSupported {
			return loadRefusalAgentEnsureUnsupported
		}
	}
	if agent && hasSpec && spec.AdminState == vramAdminStateForceStopped {
		return loadRefusalSpecForceStopped
	}
	return ""
}

// loadTargetFor builds a Load's target from the spec its starter read, for a
// mapping loadRefusal did not refuse. Such a mapping, if it serves only
// images, only audio or only decisions, is a server_agent child whose agent
// declared runtime_ensure, so its target loads without generating.
func (s *Server) loadTargetFor(ctx context.Context, v portal.BenchmarkTargetView, spec routing.RuntimeSpec, hasSpec bool) benchmarkTarget {
	tgt := s.benchmarkTargetFor(ctx, v.Server, v.App, v.Mapping, spec, hasSpec)
	tgt.loadWithoutGenerating = mappingSpecIsImagesOnly(v.App, spec, hasSpec) || mappingSpecIsAudioOnly(v.App, spec, hasSpec) || mappingSpecIsDecisionsOnly(v.App, spec, hasSpec)
	return tgt
}

// refuseBusyServer answers 409 benchmark.already_running when serverID
// already has a run, and reports whether it did. Every manual starter asks
// right after authorizing and before it reads a spec: a VRAM run's drain
// stores force_stopped on every enabled spec, so a starter that read first
// would answer a refusal about that run's own override.
func (s *Server) refuseBusyServer(w http.ResponseWriter, serverID string) bool {
	if !s.Benchmarks.ServerBusy(serverID) {
		return false
	}
	writeJSON(w, http.StatusConflict, apierror.Response(codeBenchmarkAlreadyRunning, msgBenchmarkAlreadyRunning, ""))
	return true
}

// starterSpec is a mapping-scope starter's one fail-closed read of the
// mapping's runtime spec (mappingRuntimeSpec). The starter's refusals and its
// run's target are both built from what it returns, so the target carries the
// row the check saw. A failed read answers 500 benchmark.request_failed and
// is logged with the mapping id; ok is false then, and the starter returns
// without reserving the server.
func (s *Server) starterSpec(ctx context.Context, w http.ResponseWriter, v portal.BenchmarkTargetView) (spec routing.RuntimeSpec, hasSpec, ok bool) {
	spec, hasSpec, err := s.mappingRuntimeSpec(ctx, v.App, v.Mapping.ID)
	if err != nil {
		slog.Warn("benchmark: runtime spec read failed; run refused", "mapping_id", v.Mapping.ID, "err", err)
		writeBenchmarkRequestFailed(w)
		return routing.RuntimeSpec{}, false, false
	}
	return spec, hasSpec, true
}

// benchmarkViewPartition is startBenchmark's split of a scope's views, made
// before the reservation (partitionBenchmarkViews).
type benchmarkViewPartition struct {
	// runnable are the targets the run measures, in view order.
	runnable []benchmarkTarget
	// recorded are the results the run records before it measures anything,
	// in view order: a skip for each view that serves only images or only
	// audio it cannot measure, and an error for each view a read failed for.
	// Neither gets a history row.
	recorded []BenchmarkResult
	// unreadable counts the views a read failed for: the runtime spec, or for
	// an audio view of the speed, capacity and both modes its capability rows
	// or voices cache.
	unreadable int
	// imagesOnly counts the views skipped because they serve only images.
	// audioOnly counts the views the vision probe skips because they serve only
	// audio, and audioUnmeasurable the audio-only views the speed, capacity and
	// both modes skip because they cannot measure their audio either (no speech
	// or transcription verdict, or a clone model without a preset voice).
	// refuseNothingRunnable names the kind. clusterHeads counts the views skipped
	// because they are cluster heads.
	imagesOnly, audioOnly, audioUnmeasurable, clusterHeads int
	// decisionsOnly counts the views skipped because they serve only decisions.
	decisionsOnly int
}

// partitionBenchmarkViews sorts each view (benchmarkViewTarget) into runnable,
// skipped (it serves only images, or only audio it cannot be measured on, so it
// cannot answer the run's chat prompt) or unreadable (a read failed, so whether
// it serves only images or only audio, or what audio it serves, is unknown). With
// measureAudio -- the speed, capacity and both modes -- an audio-only speech or
// transcription mapping is a runnable audio target, and an audio-only view it
// cannot measure is counted as audioUnmeasurable; without it, the vision probe's
// partition, every audio-only view is skipped and counted as audioOnly. A view
// naming both non-text flavors is skipped as images-only. A runnable target is
// built from the spec that was read. An unreadable view is logged with its
// mapping id.
func (s *Server) partitionBenchmarkViews(ctx context.Context, views []portal.BenchmarkTargetView, measureAudio bool) benchmarkViewPartition {
	var p benchmarkViewPartition
	for _, v := range views {
		tgt, skipped, err := s.benchmarkViewTarget(ctx, v, measureAudio)
		var unreadable *benchmarkViewUnreadable
		switch {
		case errors.As(err, &unreadable) && unreadable.what == benchmarkViewSpec:
			slog.Warn("benchmark: runtime spec read failed; mapping not benchmarked", "mapping_id", v.Mapping.ID, "err", unreadable.err)
			p.unreadable++
			p.recorded = append(p.recorded, BenchmarkResult{MappingID: v.Mapping.ID, GatewayModelName: v.Mapping.GatewayModelName, Error: benchmarkSpecUnreadable + unreadable.err.Error()})
		case err != nil:
			slog.Warn("audio benchmark: a read failed; mapping not benchmarked", "mapping_id", v.Mapping.ID, "err", err)
			p.unreadable++
			p.recorded = append(p.recorded, BenchmarkResult{MappingID: v.Mapping.ID, GatewayModelName: v.Mapping.GatewayModelName, Error: err.Error()})
		case skipped == benchmarkSkippedImagesOnly:
			p.imagesOnly++
			p.recorded = append(p.recorded, BenchmarkResult{MappingID: v.Mapping.ID, GatewayModelName: v.Mapping.GatewayModelName, Skipped: skipped})
		case skipped == benchmarkSkippedDecisionsOnly:
			p.decisionsOnly++
			p.recorded = append(p.recorded, BenchmarkResult{MappingID: v.Mapping.ID, GatewayModelName: v.Mapping.GatewayModelName, Skipped: skipped})
		case skipped == benchmarkSkippedClusterHead:
			p.clusterHeads++
			p.recorded = append(p.recorded, BenchmarkResult{MappingID: v.Mapping.ID, GatewayModelName: v.Mapping.GatewayModelName, Skipped: skipped})
		case skipped != "":
			if measureAudio {
				p.audioUnmeasurable++
			} else {
				p.audioOnly++
			}
			p.recorded = append(p.recorded, BenchmarkResult{MappingID: v.Mapping.ID, GatewayModelName: v.Mapping.GatewayModelName, Skipped: skipped})
		default:
			p.runnable = append(p.runnable, tgt)
		}
	}
	return p
}

// What a view's classification could not read (benchmarkViewUnreadable.what).
const (
	benchmarkViewSpec         = "runtime spec"
	benchmarkViewCapabilities = "capability"
	benchmarkViewVoices       = "voices cache"
)

// benchmarkViewUnreadable is a view benchmarkViewTarget cannot classify because a read
// failed: what it read, and why that failed.
type benchmarkViewUnreadable struct {
	what string
	err  error
}

func (e *benchmarkViewUnreadable) Error() string {
	if e.what == benchmarkViewSpec {
		return benchmarkSpecUnreadable + e.err.Error()
	}
	return "audio benchmark: " + e.what + " unreadable; not benchmarked: " + e.err.Error()
}

func (e *benchmarkViewUnreadable) Unwrap() error { return e.err }

// benchmarkViewTarget is the one classification of a view the speed, capacity and both modes
// make, shared by the manual starter's partition, the scheduler and the health loop's
// automatic audio run. It answers the view's target, or why it is skipped (one of the closed
// BenchmarkResult.Skipped values), or a *benchmarkViewUnreadable when a read it decides on
// failed -- what the view is is then unknown, and is not guessed at. The runtime spec is read
// once (mappingRuntimeSpec) and the target is built from it.
//   - A view whose spec is a llama.cpp RPC cluster head is skipped
//     (benchmarkSkippedClusterHead): it starts only through the cluster
//     orchestration, which a run does not drive.
//   - A view that serves only images is skipped (benchmarkSkippedImagesOnly).
//   - A view that serves only decisions, and is not audio-only, is skipped
//     (benchmarkSkippedDecisionsOnly): it answers no chat prompt and has no audio.
//   - A view that serves a text flavor (it is not audio-only, mappingSpecIsAudioOnly) is a
//     chat model and a text target, exactly as before audio was measured: one with a speech
//     or transcription verdict as well -- an omni model that also transcribes, say -- keeps
//     its text benchmark, and its capability rows are not read.
//   - With measureAudio, an audio-only view the gateway would route audio to
//     (AudioTargetFlavors) has its capability rows read: one whose role is speech or
//     transcription is an audio target
//     (audioTargetOf), and a TTS clone model synthesizes with its preset voice
//     (speechBenchmarkPresetVoice) -- one with no preset cannot synthesize without a
//     reference and is skipped (benchmarkSkippedSpeechClone).
//   - Every other audio-only view -- no speech or transcription verdict, no audio request
//     routed to it, or measureAudio off -- is skipped (benchmarkSkippedAudioOnly): it cannot
//     answer a chat prompt either.
func (s *Server) benchmarkViewTarget(ctx context.Context, v portal.BenchmarkTargetView, measureAudio bool) (benchmarkTarget, string, error) {
	spec, hasSpec, err := s.mappingRuntimeSpec(ctx, v.App, v.Mapping.ID)
	if err != nil {
		return benchmarkTarget{}, "", &benchmarkViewUnreadable{what: benchmarkViewSpec, err: err}
	}
	if v.App.Type == routing.ProviderServerAgent && hasSpec && spec.ClusterEnabled {
		return benchmarkTarget{}, benchmarkSkippedClusterHead, nil
	}
	if mappingSpecIsImagesOnly(v.App, spec, hasSpec) {
		return benchmarkTarget{}, benchmarkSkippedImagesOnly, nil
	}
	if !mappingSpecIsAudioOnly(v.App, spec, hasSpec) {
		if mappingSpecIsDecisionsOnly(v.App, spec, hasSpec) {
			return benchmarkTarget{}, benchmarkSkippedDecisionsOnly, nil
		}
		return s.benchmarkTargetFor(ctx, v.Server, v.App, v.Mapping, spec, hasSpec), "", nil
	}
	if measureAudio && AudioTargetFlavors(v.App, spec, hasSpec) {
		return s.benchmarkAudioViewTarget(ctx, v, spec, hasSpec)
	}
	return benchmarkTarget{}, benchmarkSkippedAudioOnly, nil
}

// benchmarkAudioViewTarget is benchmarkViewTarget for an audio-only view the gateway routes
// audio to, with audio measured: its capability rows decide whether it is an audio target, a
// TTS clone model without a preset voice is skipped (benchmarkSkippedSpeechClone), and a view
// with no speech or transcription role is skipped as audio-only (benchmarkSkippedAudioOnly).
func (s *Server) benchmarkAudioViewTarget(ctx context.Context, v portal.BenchmarkTargetView, spec routing.RuntimeSpec, hasSpec bool) (benchmarkTarget, string, error) {
	rows, err := s.Routes.MappingCapabilities(ctx, v.Mapping.ID)
	if err != nil {
		return benchmarkTarget{}, "", &benchmarkViewUnreadable{what: benchmarkViewCapabilities, err: err}
	}
	at := audioTargetOf(rows, routing.EffectiveRuntimeSpecType(spec))
	if at.role == "" {
		return benchmarkTarget{}, benchmarkSkippedAudioOnly, nil
	}
	voice := ""
	if at.role == routing.RoleTTS {
		if voice, err = s.speechBenchmarkPresetVoice(ctx, v.Mapping.ID, rows); err != nil {
			return benchmarkTarget{}, "", &benchmarkViewUnreadable{what: benchmarkViewVoices, err: err}
		}
		if !routing.CapabilityRowsOfferableTTS(rows, voice != "") {
			return benchmarkTarget{}, benchmarkSkippedSpeechClone, nil
		}
	}
	tgt := s.benchmarkTargetFor(ctx, v.Server, v.App, v.Mapping, spec, hasSpec)
	tgt.audio, tgt.voice = at, voice
	return tgt, "", nil
}

// partitionBenchmarkViewsFor is the split a run of mode makes: the speech
// benchmark's own (partitionSpeechBenchmarkViews), where only speech runs; the
// vision probe's, where audio is refused; and for the speed, capacity and both
// modes the one where a speech or transcription mapping is measured as audio
// (partitionBenchmarkViews with measureAudio).
func (s *Server) partitionBenchmarkViewsFor(ctx context.Context, views []portal.BenchmarkTargetView, mode string) benchmarkViewPartition {
	switch mode {
	case benchmarkModeSpeech:
		return s.partitionSpeechBenchmarkViews(ctx, views)
	case "vision":
		return s.partitionBenchmarkViews(ctx, views, false)
	default:
		return s.partitionBenchmarkViews(ctx, views, true)
	}
}

// refuseNothingRunnableFor answers a partition with no runnable view the way the
// run's mode does: refuseNothingRunnableSpeech for the speech benchmark,
// refuseNothingRunnable for every other mode.
func (p benchmarkViewPartition) refuseNothingRunnableFor(w http.ResponseWriter, mode string) bool {
	if mode == benchmarkModeSpeech {
		return p.refuseNothingRunnableSpeech(w)
	}
	return p.refuseNothingRunnable(w)
}

// refuseNothingRunnable answers a partition with no runnable view, and
// reports whether it did, with the answer that is true of the whole scope:
//   - 500 benchmark.request_failed when a read failed, because then any
//     refusal would be a guess;
//   - for the speed, capacity and both modes, 409 benchmark.speech_unavailable
//     whenever a skipped view serves only audio they cannot measure: with the
//     audio message (msgBenchmarkAudioUnmeasurable) when every skipped view
//     is such audio, and with the message that names both kinds
//     (msgBenchmarkScopeUnmeasurable) when images-only views are skipped
//     beside it -- images_only would claim the audio model serves images;
//   - 409 benchmark.audio_only when every skipped view serves only audio and
//     the run is the vision probe;
//   - 409 benchmark.images_only otherwise: every skipped view serves only
//     images, or the vision probe skipped images-only views beside audio-only
//     ones (both refuse its chat prompt), or none was skipped at all.
func (p benchmarkViewPartition) refuseNothingRunnable(w http.ResponseWriter) bool {
	switch {
	case len(p.runnable) > 0:
		return false
	case p.unreadable > 0:
		writeBenchmarkRequestFailed(w)
	case p.clusterHeads > 0 && p.imagesOnly+p.audioOnly+p.audioUnmeasurable+p.decisionsOnly == 0:
		writeBenchmarkError(w, errBenchmarkClusterHeadUnsupported)
	case p.audioUnmeasurable > 0 && p.imagesOnly > 0:
		writeBenchmarkError(w, errBenchmarkScopeUnmeasurable)
	case p.imagesOnly > 0:
		writeBenchmarkError(w, errBenchmarkImagesOnly)
	case p.audioUnmeasurable > 0:
		writeBenchmarkError(w, errBenchmarkAudioUnmeasurable)
	case p.audioOnly > 0:
		writeBenchmarkError(w, errBenchmarkAudioOnly)
	case p.decisionsOnly > 0:
		writeBenchmarkError(w, errBenchmarkDecisionsOnly)
	default:
		writeBenchmarkError(w, errBenchmarkImagesOnly)
	}
	return true
}
