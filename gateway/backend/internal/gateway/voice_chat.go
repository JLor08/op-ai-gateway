// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"slices"
	"strings"
)

// The codes of the voice-chat authorize endpoint's refusals (API surface). They are
// portal-namespaced: the gate runs only on the portal's ticket POST, never on a /v1
// endpoint, so there is no bearer twin to share a surface-neutral code with.
const (
	// voiceChatASRUnavailable: the chosen live-transcription model cannot take a voice
	// chat -- unknown or not reachable by the caller, not transcription-capable, not a
	// streaming model, behind an agent that has not declared fullduplex_proxy, or its
	// child could not be started.
	voiceChatASRUnavailable = "portal.voice_chat_asr_unavailable"
	// voiceChatLLMUnavailable: the chosen chat model is unknown, not reachable by the
	// caller, serves no chat (an audio- or images-only child), or its child could not be
	// started.
	voiceChatLLMUnavailable = "portal.voice_chat_llm_unavailable"
	// voiceChatTTSUnavailable: the chosen speech model is unknown, not reachable by the
	// caller, not speech-capable, not an offerable streaming model, or its child could
	// not be started.
	voiceChatTTSUnavailable = "portal.voice_chat_tts_unavailable"
	// voiceChatCoLoadFailed: the three models cannot be held resident together -- an
	// agent refused to admit one of them (runtime.admission_blocked), starting one
	// evicted another, or residency could not be verified at all. The remedy is the
	// operator's: the pairwise co-residency matrix and the GPU VRAM budgets.
	voiceChatCoLoadFailed = "portal.voice_chat_coload_failed"
	// voiceChatTicketFailed: the ticket could not be minted (no store, or the random
	// source or the encoding failed). 500.
	voiceChatTicketFailed = "portal.voice_chat_ticket_failed"
	// voiceChatAudioSettingsInvalid: the chat's stored speech settings cannot ride a
	// voice-chat ticket -- they do not decode as speech settings, or their JSON is over
	// voiceTicketAudioMaxBytes. Raised by the ticket endpoint before the gate, never by
	// the gate itself. 400: the user's to change in the chat's speech settings.
	voiceChatAudioSettingsInvalid = "portal.voice_chat_audio_settings_invalid"
	// voiceChatNoRealtimeModel: every candidate of the ASR or the TTS leg was measured
	// slower than the real-time threshold a conversation needs
	// (routing.ErrNoRealtimeCandidate). 409, with the leg named in the message
	// ("voice chat <leg> leg: ..."): the operator's remedy is another model or a faster
	// host, and the user's another model for that leg. Surface-neutral (not portal.*),
	// since a voice turn's /v1 loopback answers it too when a model is re-measured
	// slower mid-session.
	voiceChatNoRealtimeModel = "voice_chat.no_realtime_model"
)

// voiceGatePath is the path of the request the gate builds for the shared inference
// gate: the portal endpoint the gate runs under. It only labels log lines -- nothing is
// routed on it.
const voiceGatePath = "/api/portal/voice-chat-ticket"

// voiceAdmissionBlockedCode is the agent router's code for "I would not admit this
// child": the co-residency matrix, the process-count limit or the per-GPU VRAM budget
// refused it, or a pinned sibling holds the room. It is a 503 that a retry does not cure.
const voiceAdmissionBlockedCode = "runtime.admission_blocked"

// voiceLeg names one of the three legs of the hands-free loop.
type voiceLeg string

const (
	voiceLegASR voiceLeg = "asr"
	voiceLegLLM voiceLeg = "llm"
	voiceLegTTS voiceLeg = "tts"
)

// The causes a leg is refused for, kept distinct so a log line and a test can tell
// them apart. The gate wraps each in a "voice chat <leg> leg" error naming the leg.
var (
	errVoiceModelRequired       = errors.New("a model name is required")
	errVoiceASRNotStreaming     = errors.New("the model's mapping has no transcription_streaming verdict of yes, so it cannot take a live session")
	errVoiceASRAgentUnsupported = errors.New("the model's agent has not declared " + fullduplexProxyFeature)
	errVoiceTTSNotStreaming     = errors.New("the model's mapping has no speech_streaming verdict of yes, so it cannot stream speech")
	errVoiceTTSNotOfferable     = errors.New("the model is a clone model with no named preset voice to speak in")

	errVoiceNoRuntimeEnsure = errors.New("the provider cannot start a model through an agent router's ensure route")
	errVoiceNoResidencyRead = errors.New("the provider cannot read which models an agent has loaded")
	errVoiceNotCoResident   = errors.New("the models are not resident together: an agent evicted one of them while starting another, so the co-residency matrix or the GPU VRAM budget does not allow all three at once")
)

// voiceLegError is a leg's refusal that carries the leg itself, for a refusal whose answer
// names it (voiceChatNoRealtimeModel). It reads as every other leg refusal does ("voice
// chat <leg> leg: <cause>") and unwraps to its cause.
type voiceLegError struct {
	leg voiceLeg
	err error
}

func (e *voiceLegError) Error() string { return fmt.Sprintf("voice chat %s leg: %v", e.leg, e.err) }

func (e *voiceLegError) Unwrap() error { return e.err }

// voiceGateLeg is one resolved leg of the gate: its name, its resolved routing target,
// the code the gate refuses with when this leg's model cannot be used, and how the leg is
// selected again with an instance excluded that the agent refused as gone.
type voiceGateLeg struct {
	leg         voiceLeg
	target      routing.Target
	unavailable string
	reselect    func(lost routing.Target) (routing.Target, error)
}

// agentManaged reports whether the leg's model is started and evicted by a ServerAgent:
// only such a target has an ensure route and an eviction model. It is decided on the
// TARGET's provider, not on whether its server happens to run an agent, exactly as
// liveTranscriptionTargetServed does.
func (l voiceGateLeg) agentManaged() bool {
	return l.target.Provider == routing.ProviderServerAgent
}

// voiceCoLoadGate is the admission gate of Voice Chat mode: it proves, at mode enable,
// that the three chosen models -- the live-transcription ASR, the chat LLM and the
// streaming TTS -- can be resident TOGETHER, so the first turn does not reload or evict
// (the feature's anti-goal). It returns the three resolved targets and an empty code on
// success; on a refusal it returns zero targets (a Target carries the sealed upstream
// credential, so a refused gate hands none back), exactly one of the voiceChat* codes
// and an error carrying the cause, for the log and for naming the leg. Refusal causes
// are never for the client's eyes verbatim; the caller words its own response from the
// code.
//
// Three sequential ensure calls do NOT prove co-residency: each can "succeed" by
// evicting the one before it. So the gate has three steps, in order:
//
//  1. Resolve + gate each leg for the shape it will be called on, WITHOUT spending
//     admission (inferenceGate, never inferencePreflight -- the same no-admit split
//     the live-transcription ticket mint uses) and without ever waiting in the
//     admission queue (inference.Request.NoPark): the gate answers the browser's
//     ticket POST, and a leg whose every instance is busy is selected onto the least
//     loaded one at once, signalling the demand for another -- a voice chat about to
//     start is demand already known. The per-turn requests are the ones that queue.
//     ASR as the live relay is gated
//     (transcription capability, the effective-served flavor, a streaming verdict, and
//     an agent that declared fullduplex_proxy), the LLM as /v1/chat/completions is
//     (resolveTranslateTarget), the TTS as the streaming speech relay is (speech
//     capability, the effective-served flavor, a streaming verdict, and an offerable
//     model). The ASR and TTS legs must also keep up with real time
//     (inference.Request.RealtimeRequired): a leg whose every candidate was measured
//     too slow is voiceChatNoRealtimeModel, naming the leg. Any other failure is that
//     leg's *Unavailable code, and nothing has been started.
//     The gate spends no admission of its own: each /v1 core spends it per turn.
//  2. Ensure each AGENT-MANAGED leg (provider.RuntimeEnsurer, through the same
//     ensureLoadAttempt the load core uses, so the same deadline bounds it): this
//     warms the instance step 1 selected. An instance k >= 1 the agent refuses as
//     gone (isInstanceGoneError: it retired it since the telemetry that reported it)
//     moves the leg once, to a sibling selected with it excluded, which is ensured
//     instead. runtime.admission_blocked is the agent saying it will not admit the
//     child, which is a co-load verdict (voiceChatCoLoadFailed); any other failure is
//     that leg's model being unavailable. One at a time, in leg order: starts race on
//     the agent's admission, and a failure ends the gate, so nothing later is started
//     for a mode that cannot be enabled.
//  3. Verify residency: read each agent's loaded set (provider.LoadedModelLister, the
//     agent router's own /running, or the application's loaded_models_path --
//     routing.EffectiveLoadedModelsProbe, exactly as the load core and the warmer read
//     it) and require every agent-managed leg's model to be in it NOW. If starting the
//     third evicted the first, it is missing and the gate refuses. The set is read
//     live after the ensures, not taken from an agent report that may lag them, and
//     once per agent, so one snapshot answers "resident together". This reuses the
//     agent's own admission outcome; the co-residency matrix and the VRAM arithmetic
//     are deliberately NOT re-derived here (agent-runtime-manager §5.2: subtle,
//     test-pinned, and direction-sensitive). A residency that cannot be read is not
//     one the gate may assume, so it refuses.
//
// A direct (non-agent) leg has no ensure route and no eviction model: it takes step 1
// and skips steps 2 and 3. The check is topology-agnostic: an LLM on another server is
// trivially resident there, while ASR and TTS must both be resident on the audio host.
//
// ctx bounds the whole gate (each ensure also runs under the load loop's own deadline),
// and a cancelled ctx ends it. token is a pointer for the reason resolveTarget's is.
// The request the shared inference gate needs is built here from ctx: the mint is a
// browser POST, which carries none of the internal server-override headers (the edge
// blanks them), so the token's own override is what governs, as it would for the
// per-turn hops.
func (s *Server) voiceCoLoadGate(ctx context.Context, token *auth.Token, asrModel, llmModel, ttsModel string) (asr, llm, tts routing.Target, code string, err error) {
	refuse := func(code string, err error) (routing.Target, routing.Target, routing.Target, string, error) {
		return routing.Target{}, routing.Target{}, routing.Target{}, code, err
	}
	if token == nil {
		return refuse(voiceChatTicketFailed, errors.New("voice chat gate: no authenticated token"))
	}
	if err := ctx.Err(); err != nil {
		return refuse(voiceChatCoLoadFailed, err)
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, voiceGatePath, http.NoBody)
	if err != nil {
		return refuse(voiceChatTicketFailed, fmt.Errorf("voice chat gate: build request: %w", err))
	}

	// Step 1: resolve and gate every leg before anything is started.
	legs := make([]voiceGateLeg, 0, 3)
	for _, step := range []struct {
		leg         voiceLeg
		model       string
		unavailable string
		resolve     func(*http.Request, *auth.Token, string, []string) (routing.Target, error)
	}{
		{voiceLegASR, asrModel, voiceChatASRUnavailable, s.resolveVoiceASR},
		{voiceLegLLM, llmModel, voiceChatLLMUnavailable, s.resolveVoiceLLM},
		{voiceLegTTS, ttsModel, voiceChatTTSUnavailable, s.resolveVoiceTTS},
	} {
		if strings.TrimSpace(step.model) == "" {
			return refuse(step.unavailable, fmt.Errorf("voice chat %s leg: %w", step.leg, errVoiceModelRequired))
		}
		target, err := step.resolve(r, token, step.model, nil)
		if errors.Is(err, routing.ErrNoRealtimeCandidate) {
			slog.Debug("voice chat gate: leg measured too slow for real time", "leg", step.leg, "model", step.model)
			return refuse(voiceChatNoRealtimeModel, &voiceLegError{leg: step.leg, err: err})
		}
		if err != nil {
			slog.Debug("voice chat gate: leg refused", "leg", step.leg, "model", step.model, "err", err)
			return refuse(step.unavailable, fmt.Errorf("voice chat %s leg: %w", step.leg, err))
		}
		reselect := func(lost routing.Target) (routing.Target, error) {
			return step.resolve(r, token, step.model, []string{lost.InstanceKey})
		}
		legs = append(legs, voiceGateLeg{leg: step.leg, target: target, unavailable: step.unavailable, reselect: reselect})
	}

	// Steps 2 and 3: warm the agent-managed legs, then prove they are resident together.
	if code, err := s.ensureVoiceLegs(ctx, legs); err != nil {
		return refuse(code, err)
	}
	if err := s.verifyVoiceCoResidency(ctx, legs); err != nil {
		slog.Warn("voice chat gate: the models are not resident together", "err", err)
		return refuse(voiceChatCoLoadFailed, err)
	}
	return legs[0].target, legs[1].target, legs[2].target, "", nil
}

// resolveVoiceASR resolves and gates the ASR leg exactly as the live-transcription
// ticket mint does without admission: the shared gate over the transcription shape, the
// target resolve with its effective-served-flavor conjunct, the streaming verdict, and
// the fullduplex_proxy declaration of a server_agent target's agent. The relay the
// orchestrator opens per turn is the bearer /v1/audio/transcriptions/live, so a model
// it would refuse there is refused here. exclude is the instances the selection must not
// pick (voiceInferenceGate).
//
// The leg must keep up with real time (RealtimeRequired), as every turn's relay will.
func (s *Server) resolveVoiceASR(r *http.Request, token *auth.Token, model string, exclude []string) (routing.Target, error) {
	req, err := s.voiceInferenceGate(r, *token, transcriptionInferenceShape(model), exclude)
	if err != nil {
		return routing.Target{}, err
	}
	req.RealtimeRequired = true
	target, err := s.resolveLiveTranscriptionTarget(r.Context(), token, req)
	if err != nil {
		return routing.Target{}, err
	}
	if !s.mappingStreamsTranscription(r.Context(), target.RouteID) {
		return routing.Target{}, errVoiceASRNotStreaming
	}
	if !s.liveTranscriptionTargetServed(target) {
		return routing.Target{}, errVoiceASRAgentUnsupported
	}
	return target, nil
}

// resolveVoiceLLM resolves and gates the LLM leg as /v1/chat/completions will be called
// per turn: a streamed chat shape through the shared gate, then resolveTranslateTarget,
// which refuses a target that serves images or audio only. exclude is as for
// resolveVoiceASR.
func (s *Server) resolveVoiceLLM(r *http.Request, token *auth.Token, model string, exclude []string) (routing.Target, error) {
	req, err := s.voiceInferenceGate(r, *token, inferenceShape{apiFlavor: "openai_chat_completions", endpoint: endpointChat, model: model, stream: true}, exclude)
	if err != nil {
		return routing.Target{}, err
	}
	return s.resolveTranslateTarget(r, token, req)
}

// resolveVoiceTTS resolves and gates the TTS leg as the streaming speech relay will be
// called: the shared gate over the speech shape (the speech capability), the target
// resolve with its effective-served-flavor conjunct (resolveSpeechTarget's), a
// speech_streaming verdict of yes (mappingStreamsSpeech: the stream the loop plays), and
// an offerable model (routing.CapabilityRowsOfferableTTS: a clone model only with a named
// preset to speak in, the rule the portal's voice picker and streaming gate apply).
// exclude is as for resolveVoiceASR, and the leg must keep up with real time like the ASR
// leg (RealtimeRequired).
func (s *Server) resolveVoiceTTS(r *http.Request, token *auth.Token, model string, exclude []string) (routing.Target, error) {
	req, err := s.voiceInferenceGate(r, *token, speechInferenceShape(model, ""), exclude)
	if err != nil {
		return routing.Target{}, err
	}
	req.RealtimeRequired = true
	return s.voiceTTSTarget(r, token, req)
}

// checkVoiceTTS is the voice-chat replay's check of its speech model (the synth
// endpoint, before its own admitted relay): the TTS leg's rules (voiceTTSTarget) over a
// resolve that only checks (inference.Request.CheckOnly). It never waits in the
// admission queue and signals no demand -- the relay it precedes resolves, parks and
// signals on its own, so a check that did either would wait twice, or ask for an
// instance twice, for one synthesis. The replay is no live conversation, so it is not
// held to real time.
func (s *Server) checkVoiceTTS(r *http.Request, token *auth.Token, model string) error {
	req, err := s.voiceShapeGate(r, *token, speechInferenceShape(model, ""))
	if err != nil {
		return err
	}
	req.CheckOnly = true
	_, err = s.voiceTTSTarget(r, token, req)
	return err
}

// voiceTTSTarget resolves an already-gated speech request and holds the target to the
// TTS leg's rules: the effective-served flavor, a speech_streaming verdict of yes and an
// offerable model (see resolveVoiceTTS).
func (s *Server) voiceTTSTarget(r *http.Request, token *auth.Token, req inference.Request) (routing.Target, error) {
	target, err := s.resolveTarget(r.Context(), token, req)
	if err != nil {
		return routing.Target{}, err
	}
	if !targetServesFlavor(target, req.APIFlavor) {
		return routing.Target{}, routing.ErrNoModelRoute
	}
	if !s.mappingStreamsSpeech(r.Context(), target.RouteID) {
		return routing.Target{}, errVoiceTTSNotStreaming
	}
	offerable, err := s.voiceTTSOfferable(r.Context(), target.RouteID)
	if err != nil {
		return routing.Target{}, fmt.Errorf("read the model's voices: %w", err)
	}
	if !offerable {
		return routing.Target{}, errVoiceTTSNotOfferable
	}
	return target, nil
}

// voiceTTSOfferable reports whether the mapping routeID may be offered as a speech
// model: (speech, yes) and either not a clone model or one with at least one named
// preset in its voices cache (routing.CapabilityRowsOfferableTTS). A voices row that
// does not decode counts as no named voice, as it does for the picker.
func (s *Server) voiceTTSOfferable(ctx context.Context, routeID string) (bool, error) {
	if s.Routes == nil {
		return false, errors.New("no routing store")
	}
	rows, err := s.Routes.MappingCapabilities(ctx, routeID)
	if err != nil {
		return false, err
	}
	cached, err := s.Routes.ModelMappingCacheForMappings(ctx, []string{routeID}, routing.ModelMappingCacheVoices)
	if err != nil {
		return false, err
	}
	names, namesErr := routing.VoiceNamesFromCacheRows(cached[routeID])
	return routing.CapabilityRowsOfferableTTS(rows, namesErr == nil && len(names) > 0), nil
}

// voiceInferenceGate is voiceShapeGate for one leg of the co-load gate. The request it
// returns never parks (NoPark): the gate only selects and must answer the ticket POST
// without waiting behind the requests that will use the model, and with every instance
// busy it signals the demand for another itself -- a voice chat about to start is demand
// already known. exclude is the instances (routing.InstanceKey) its selection must not
// pick: one the agent refused as gone when the gate re-selects a leg, nil otherwise.
func (s *Server) voiceInferenceGate(r *http.Request, token auth.Token, shape inferenceShape, exclude []string) (inference.Request, error) {
	req, err := s.voiceShapeGate(r, token, shape)
	if err != nil {
		return inference.Request{}, err
	}
	req.NoPark = true
	req.ExcludeInstanceKeys = exclude
	return req, nil
}

// voiceShapeGate runs the shared inference gate (inferenceGate: model override and
// redirect, the server-override re-auth, the model allowlist and session extraction, but
// NOT the principal's admission) for shape, and returns the gated request. The gate
// answers a refusal by writing to a ResponseWriter; there is no client response here, so
// what it would have written is captured and returned as the error.
func (s *Server) voiceShapeGate(r *http.Request, token auth.Token, shape inferenceShape) (inference.Request, error) {
	rec := &voiceGateWriter{header: http.Header{}}
	pf, handled := s.inferenceGate(rec, r, token, nil, shape)
	if handled {
		return inference.Request{}, rec.refusal()
	}
	return pf.Req, nil
}

// voiceGateWriter is the http.ResponseWriter the shared inference gate writes a refusal
// into when no client is waiting for it. It keeps the status and the body, and nothing
// is sent anywhere.
type voiceGateWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (w *voiceGateWriter) Header() http.Header { return w.header }

func (w *voiceGateWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *voiceGateWriter) Write(p []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.body.Write(p)
}

// refusal is the error for what the gate wrote: the HTTP status and the stable code of
// its apierror body when it has one.
func (w *voiceGateWriter) refusal() error {
	var body apierror.Body
	if json.Unmarshal(w.body.Bytes(), &body) == nil && body.Error.Code != "" {
		return fmt.Errorf("inference gate refused the model (HTTP %d, %s)", w.status, body.Error.Code)
	}
	return fmt.Errorf("inference gate refused the model (HTTP %d)", w.status)
}

// ensureVoiceLegs is step 2 of the gate: it starts each agent-managed leg through the
// agent router's ensure route, one after the other, and returns the code to refuse with
// and the cause on the first failure. A direct leg has no ensure route and is skipped. A
// leg re-selected off a gone instance (ensureVoiceLeg) has its target replaced in legs, so
// the residency check and the gate's answer name the instance that was warmed.
func (s *Server) ensureVoiceLegs(ctx context.Context, legs []voiceGateLeg) (string, error) {
	var ensurer provider.RuntimeEnsurer
	for i := range legs {
		l := &legs[i]
		if !l.agentManaged() {
			continue
		}
		if ensurer == nil {
			var ok bool
			if ensurer, ok = s.Provider.(provider.RuntimeEnsurer); !ok {
				return voiceChatCoLoadFailed, fmt.Errorf("voice chat %s leg: %w", l.leg, errVoiceNoRuntimeEnsure)
			}
		}
		if code, err := s.ensureVoiceLeg(ctx, ensurer, l); err != nil {
			return code, err
		}
	}
	return "", nil
}

// ensureVoiceLeg starts one agent-managed leg through the ensure route and returns, on a
// failure, the code to refuse with and the cause. An instance k >= 1 refused as gone is
// left once for a sibling (reselectVoiceLeg), which replaces l.target. A caller that went
// away mid-start, or an agent that did not admit the leg, is a co-load failure; any other
// start failure -- a second gone instance among them -- is the leg's own unavailable code.
func (s *Server) ensureVoiceLeg(ctx context.Context, ensurer provider.RuntimeEnsurer, l *voiceGateLeg) (string, error) {
	err := s.ensureVoiceTarget(ctx, ensurer, l.target)
	// The gate's own selection excludes nothing, so its first gone instance may always
	// be left: the once-rule is that the re-selection is never re-selected again.
	if err != nil && ctx.Err() == nil && l.target.Instance() >= 1 && isInstanceGoneError(err) {
		err = s.reselectVoiceLeg(ctx, ensurer, l, err)
	}
	if err == nil {
		return "", nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		// The caller went away mid-start; the failure says nothing about the leg.
		return voiceChatCoLoadFailed, fmt.Errorf("voice chat %s leg: %w", l.leg, ctxErr)
	}
	wrapped := fmt.Errorf("voice chat %s leg: %w", l.leg, err)
	var routerErr *provider.RouterError
	if errors.As(err, &routerErr) && routerErr.Code == voiceAdmissionBlockedCode {
		slog.Warn("voice chat gate: the agent did not admit a leg", "leg", l.leg, "model", l.target.ProviderModel, "server", s.serverName(l.target.ServerID), "err", err)
		return voiceChatCoLoadFailed, wrapped
	}
	slog.Warn("voice chat gate: a leg could not be started", "leg", l.leg, "model", l.target.ProviderModel, "server", s.serverName(l.target.ServerID), "err", err)
	return l.unavailable, wrapped
}

// ensureVoiceTarget is one ensure of target through the ensure route, under the load
// loop's deadline. A llama.cpp RPC cluster head is started through the cluster
// orchestration instead -- members first, joined if a start of it runs already -- under
// the same deadline: the agent refuses the plain ensure for a head.
func (s *Server) ensureVoiceTarget(ctx context.Context, ensurer provider.RuntimeEnsurer, target routing.Target) error {
	// The spec words a failure (its start timeout) and tells a cluster head apart; a
	// failed read is no reason to refuse to start the model.
	var spec routing.RuntimeSpec
	if s.Routes != nil {
		spec, _, _ = s.Routes.RuntimeSpecByMapping(ctx, target.RouteID)
	}
	bound := s.newLoadBound(target)
	if spec.ClusterEnabled {
		cctx, cancel := context.WithDeadline(ctx, bound.deadline)
		defer cancel()
		return s.clusters.Ensure(cctx, spec.ID)
	}
	return s.ensureLoadAttempt(ensurer, target, spec)(ctx, bound)
}

// reselectVoiceLeg moves leg l off its selected instance, which the agent refused as gone
// with the error gone: the leg is selected again with that instance excluded, l.target
// becomes the new selection, and that is ensured -- unless it is a direct target, which has
// no ensure route. A re-selection that routing refuses ends the leg with gone, the answer
// the instance it could not leave gave, as a stream that cannot move ends.
func (s *Server) reselectVoiceLeg(ctx context.Context, ensurer provider.RuntimeEnsurer, l *voiceGateLeg, gone error) error {
	target, err := l.reselect(l.target)
	if err != nil {
		slog.Warn("voice chat gate: the selected instance is gone and re-selection failed", "leg", l.leg, "instance", l.target.InstanceKey, "err", err)
		return gone
	}
	slog.Info("voice chat gate: the selected instance is gone; re-selected once", "leg", l.leg, "lost", l.target.InstanceKey, "instance", target.InstanceKey)
	l.target = target
	if !l.agentManaged() {
		return nil
	}
	return s.ensureVoiceTarget(ctx, ensurer, target)
}

// voiceLoadedKey identifies one loaded-set read: the server, and the path and format it
// is read with. Two legs on one agent share a key, so one read answers for both.
type voiceLoadedKey struct{ serverID, path, format string }

// verifyVoiceCoResidency is step 3 of the gate: every agent-managed leg's model must be
// in its agent's CURRENT loaded set. It returns nil when there is nothing to verify (no
// agent-managed leg) or all are resident, errVoiceNotCoResident (wrapped, naming the
// legs that are missing) when starting a later leg evicted an earlier one, and any other
// error when the set could not be read -- a residency that cannot be read is not assumed.
func (s *Server) verifyVoiceCoResidency(ctx context.Context, legs []voiceGateLeg) error {
	agentLegs := agentManagedVoiceLegs(legs)
	if len(agentLegs) == 0 {
		return nil
	}
	lister, ok := s.Provider.(provider.LoadedModelLister)
	if !ok {
		return errVoiceNoResidencyRead
	}
	snapshots := map[voiceLoadedKey][]string{}
	var missing []string
	for _, l := range agentLegs {
		loaded, err := s.voiceLegLoadedModels(ctx, lister, snapshots, l)
		if err != nil {
			return err
		}
		if !slices.Contains(loaded, l.target.ProviderModel) {
			missing = append(missing, fmt.Sprintf("%s (%s)", l.leg, l.target.ProviderModel))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("voice chat: %s not resident after all legs were started: %w", strings.Join(missing, ", "), errVoiceNotCoResident)
	}
	return nil
}

// agentManagedVoiceLegs is the legs that run on an agent-managed server, in order: the
// only ones whose residency the gate verifies.
func agentManagedVoiceLegs(legs []voiceGateLeg) []voiceGateLeg {
	var agentLegs []voiceGateLeg
	for _, l := range legs {
		if l.agentManaged() {
			agentLegs = append(agentLegs, l)
		}
	}
	return agentLegs
}

// voiceLegLoadedModels is the set of models the agent of one leg has loaded now. Two legs
// that read the same server with the same path and format share one read: snapshots holds
// the sets already read, keyed by what they were read with, and a fresh read is added to it.
func (s *Server) voiceLegLoadedModels(ctx context.Context, lister provider.LoadedModelLister, snapshots map[voiceLoadedKey][]string, l voiceGateLeg) ([]string, error) {
	path, format, err := s.voiceLoadedModelsProbe(ctx, l.target)
	if err != nil {
		return nil, fmt.Errorf("voice chat %s leg: %w", l.leg, err)
	}
	key := voiceLoadedKey{serverID: l.target.ServerID, path: path, format: format}
	if loaded, seen := snapshots[key]; seen {
		return loaded, nil
	}
	loaded, err := lister.LoadedModels(s.upstreamAuthCtx(ctx, l.target), l.target, path, format)
	if err != nil {
		return nil, fmt.Errorf("voice chat %s leg: read the models the agent has loaded: %w", l.leg, err)
	}
	snapshots[key] = loaded
	return loaded, nil
}

// voiceLoadedModelsProbe resolves the loaded-models path and format an agent-managed
// target's server is read with: routing.EffectiveLoadedModelsProbe over the target's own
// application (reached through its mapping), so an application's loaded_models_path
// replaces the router's /running exactly as it does for the load core and the warmer.
func (s *Server) voiceLoadedModelsProbe(ctx context.Context, target routing.Target) (path, format string, err error) {
	if s.Routes == nil {
		return "", "", errors.New("no routing store")
	}
	mapping, err := s.Routes.MappingByID(ctx, target.RouteID)
	if err != nil {
		return "", "", fmt.Errorf("read the model's mapping: %w", err)
	}
	app, err := s.Routes.ApplicationByID(ctx, mapping.ApplicationID)
	if err != nil {
		return "", "", fmt.Errorf("read the model's application: %w", err)
	}
	path, format = routing.EffectiveLoadedModelsProbe(app)
	if strings.TrimSpace(path) == "" {
		return "", "", errors.New("the model's application has no loaded-models path to read")
	}
	return path, format, nil
}
