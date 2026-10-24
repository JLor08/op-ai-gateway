// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
	"fmt"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// The voice-chat co-load gate is tested against a REAL routing.Resolver over a memory
// store (the same seam every gateway gate test uses) and two provider fakes: an agent
// router that can be told to refuse an ensure or to swap a resident model out, and a
// provider that cannot read the loaded set at all. Between them every branch of the
// gate -- resolve+gate, ensure, residency -- is provable without a live agent.

const (
	voiceGateAgentServer = "srv-voice-agent"
	voiceGateLLMServer   = "srv-voice-llm"
)

// voiceGateProbe is one LoadedModels question the fake agent was asked.
type voiceGateProbe struct{ server, path, format string }

// voiceGateFake is the agent router as the gate sees it through the provider: the
// ensure route (EnsureRuntimeModel) and the loaded-set read (LoadedModels, the router's
// /running). It models the one thing the gate exists to catch: slots > 0 makes a server a
// swapper that keeps only its slots most recently ensured models, so an ensure
// "succeeds" while it evicts an earlier one -- exactly the false pass of three
// sequential ensure calls.
type voiceGateFake struct {
	mu sync.Mutex
	// ensureErr is the error ensure answers per upstream model name; absent means success.
	ensureErr map[string]error
	// slots is how many models a server keeps resident at once: 0 is unlimited.
	slots int
	// probeErr, when set, is what every LoadedModels read answers with.
	probeErr error

	resident map[string][]string // server id -> resident upstream models, oldest first
	ensured  []string            // upstream models ensure was asked for, in order
	probes   []voiceGateProbe
}

func (f *voiceGateFake) Complete(context.Context, routing.Target, inference.Request) (provider.Response, error) {
	return provider.Response{}, nil
}

func (f *voiceGateFake) EnsureRuntimeModel(_ context.Context, target routing.Target) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensured = append(f.ensured, target.ProviderModel)
	if err := f.ensureErr[target.ProviderModel]; err != nil {
		return err
	}
	if f.resident == nil {
		f.resident = map[string][]string{}
	}
	held := slices.DeleteFunc(f.resident[target.ServerID], func(m string) bool { return m == target.ProviderModel })
	held = append(held, target.ProviderModel)
	if f.slots > 0 && len(held) > f.slots {
		held = held[len(held)-f.slots:]
	}
	f.resident[target.ServerID] = held
	return nil
}

func (f *voiceGateFake) LoadedModels(_ context.Context, target routing.Target, statusPath, format string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probes = append(f.probes, voiceGateProbe{server: target.ServerID, path: statusPath, format: format})
	if f.probeErr != nil {
		return nil, f.probeErr
	}
	return slices.Clone(f.resident[target.ServerID]), nil
}

// voiceGateEnsureOnly can warm a model and nothing else: it is no
// provider.LoadedModelLister, so the gate has no way to read what is resident.
type voiceGateEnsureOnly struct{}

func (voiceGateEnsureOnly) Complete(context.Context, routing.Target, inference.Request) (provider.Response, error) {
	return provider.Response{}, nil
}

func (voiceGateEnsureOnly) EnsureRuntimeModel(context.Context, routing.Target) error { return nil }

// voiceGateNoEnsure can read the loaded set but cannot start a model.
type voiceGateNoEnsure struct{}

func (voiceGateNoEnsure) Complete(context.Context, routing.Target, inference.Request) (provider.Response, error) {
	return provider.Response{}, nil
}

func (voiceGateNoEnsure) LoadedModels(context.Context, routing.Target, string, string) ([]string, error) {
	return nil, nil
}

// voiceGateWorld is the routing world the gate resolves against.
//
// srv-voice-agent runs one server_agent application that manages the ASR and TTS children
// (their runtime specs list openai_audio only, as a real audio child's does), plus, with
// llmOnAgent, the LLM child too. Without it the LLM is a direct application on its own
// server, which has no ensure route and no eviction model. The gateway names are:
//
//	voice-asr            transcription yes, transcription_streaming yes (the one that serves)
//	voice-asr-offline    transcription yes, transcription_streaming no
//	voice-asr-textonly   a streaming ASR whose runtime spec lists only text flavors
//	voice-tts            speech yes, speech_streaming yes
//	voice-tts-buffered   speech yes, speech_streaming no
//	voice-tts-clone      speech yes, speech_streaming yes, but a clone with no named preset
//	voice-llm            a chat model
type voiceGateWorld struct {
	srv    *Server
	routes *routing.MemoryStore
	token  auth.Token
}

func newVoiceGateWorld(t *testing.T, prov provider.Client, llmOnAgent bool) *voiceGateWorld {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	routes := routing.NewMemoryStore()
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	server := func(id, providerType, endpoint string) {
		must("CreateAIServer "+id, routes.CreateAIServer(ctx, routing.AIServer{ID: id, Name: id, Domain: id + ".example.test", Provider: providerType, Endpoint: endpoint, Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}))
		must("UpsertTelemetry "+id, routes.UpsertTelemetry(ctx, routing.ServerTelemetry{ServerID: id, ReportedAt: now, LatencyMS: 100, ProviderHealth: `{}`, Capabilities: `{}`, RawSummary: `{}`, UpdatedAt: now}))
	}
	mapping := func(appID, id, gateway, upstream string, flavors []string, caps ...[2]string) {
		must("CreateMapping "+id, routes.CreateMapping(ctx, routing.ModelMapping{ID: id, ApplicationID: appID, GatewayModelName: gateway, AppModelName: upstream, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
		if flavors != nil {
			must("UpsertRuntimeSpec "+id, routes.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{ID: "spec-" + id, MappingID: id, APIFlavors: flavors, ResponsesMode: routing.EndpointModeDisabled, MessagesMode: routing.EndpointModeDisabled, CreatedAt: now, UpdatedAt: now}))
		}
		var rows []routing.CapabilityRow
		for _, c := range caps {
			rows = append(rows, routing.CapabilityRow{Capability: c[0], Verdict: c[1], Source: routing.CapabilitySourceManual, CheckedAt: now})
		}
		if rows != nil {
			must("UpsertMappingCapabilities "+id, routes.UpsertMappingCapabilities(ctx, id, rows))
		}
	}
	yes, no := routing.CapabilityYes, routing.CapabilityNo
	audio := []string{routing.APIFlavorOpenAIAudio}

	server(voiceGateAgentServer, routing.ProviderVLLM, "http://agent.example.test:8081")
	must("CreateApplication agent", routes.CreateApplication(ctx, routing.Application{ID: "app-voice-agent", ServerID: voiceGateAgentServer, Type: routing.ProviderServerAgent, Port: 8081, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorOpenAIAudio}, Priority: 10, Weight: 50, TimeoutMS: 600000, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	mapping("app-voice-agent", "route-voice-asr", "voice-asr", "whisper_voice", audio, [2]string{routing.CapabilityTranscription, yes}, [2]string{routing.CapabilityTranscriptionStreaming, yes})
	mapping("app-voice-agent", "route-voice-asr-offline", "voice-asr-offline", "whisper_offline", audio, [2]string{routing.CapabilityTranscription, yes}, [2]string{routing.CapabilityTranscriptionStreaming, no})
	mapping("app-voice-agent", "route-voice-asr-textonly", "voice-asr-textonly", "whisper_textonly", []string{routing.APIFlavorOpenAI}, [2]string{routing.CapabilityTranscription, yes}, [2]string{routing.CapabilityTranscriptionStreaming, yes})
	mapping("app-voice-agent", "route-voice-tts", "voice-tts", "tts_voice", audio, [2]string{routing.CapabilitySpeech, yes}, [2]string{routing.CapabilitySpeechStreaming, yes})
	mapping("app-voice-agent", "route-voice-tts-buffered", "voice-tts-buffered", "tts_buffered", audio, [2]string{routing.CapabilitySpeech, yes}, [2]string{routing.CapabilitySpeechStreaming, no})
	mapping("app-voice-agent", "route-voice-tts-clone", "voice-tts-clone", "tts_clone", audio, [2]string{routing.CapabilitySpeech, yes}, [2]string{routing.CapabilitySpeechStreaming, yes}, [2]string{routing.CapabilitySpeechClone, yes})
	if llmOnAgent {
		mapping("app-voice-agent", "route-voice-llm", "voice-llm", "llm_voice", []string{routing.APIFlavorOpenAI})
	} else {
		server(voiceGateLLMServer, routing.ProviderMock, "mock://voice-llm")
		must("CreateApplication llm", routes.CreateApplication(ctx, routing.Application{ID: "app-voice-llm", ServerID: voiceGateLLMServer, Type: routing.ProviderMock, Port: 8100, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 10, Weight: 50, TimeoutMS: 30000, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
		mapping("app-voice-llm", "route-voice-llm", "voice-llm", "llm_voice", nil)
	}

	srv := New(ServerDeps{Routes: routes, Provider: prov})
	// The literal is what an agent puts on the wire, so a typo in the gateway's constant
	// fails here rather than agreeing with itself.
	srv.AgentFeatures.Set(voiceGateAgentServer, []string{"runtime_manager", "fullduplex_proxy"})
	return &voiceGateWorld{srv: srv, routes: routes, token: auth.Token{ID: "tok_voice", UserID: "usr_voice", Active: true}}
}

// gate runs the gate for the three gateway model names.
func (w *voiceGateWorld) gate(asr, llm, tts string) (routing.Target, routing.Target, routing.Target, string, error) {
	return w.srv.voiceCoLoadGate(context.Background(), &w.token, asr, llm, tts)
}

// requireRefused asserts a refusal: exactly wantCode, a non-nil error, and no resolved
// target handed back (a target carries the sealed upstream credential, so a refused
// gate must not leak a half-resolved one).
func requireVoiceGateRefused(t *testing.T, asr, llm, tts routing.Target, code string, err error, wantCode string) {
	t.Helper()
	if code != wantCode {
		t.Fatalf("code = %q (err %v), want %q", code, err, wantCode)
	}
	if err == nil {
		t.Fatalf("refused with %q but no error carrying the cause", wantCode)
	}
	for leg, target := range map[string]routing.Target{"asr": asr, "llm": llm, "tts": tts} {
		if !reflect.DeepEqual(target, routing.Target{}) {
			t.Fatalf("refused gate returned a non-zero %s target: %+v", leg, target)
		}
	}
}

// All three models resolve, the agent-managed ones are ensured, and the agent's loaded
// set holds them together: the gate answers no code and the three resolved targets.
// The direct LLM leg has no ensure route, so it is never ensured and never expected in
// the loaded set -- which here, being on another server, it is not.
func TestVoiceCoLoadGatePassesWhenAllResident(t *testing.T) {
	fake := &voiceGateFake{}
	w := newVoiceGateWorld(t, fake, false)

	asr, llm, tts, code, err := w.gate("voice-asr", "voice-llm", "voice-tts")
	if code != "" || err != nil {
		t.Fatalf("gate = (%q, %v), want a pass", code, err)
	}
	if asr.Model != "voice-asr" || asr.ProviderModel != "whisper_voice" || asr.ServerID != voiceGateAgentServer || asr.RouteID != "route-voice-asr" {
		t.Fatalf("asr target = %+v", asr)
	}
	if llm.Model != "voice-llm" || llm.ProviderModel != "llm_voice" || llm.ServerID != voiceGateLLMServer {
		t.Fatalf("llm target = %+v", llm)
	}
	if tts.Model != "voice-tts" || tts.ProviderModel != "tts_voice" || tts.ServerID != voiceGateAgentServer || tts.RouteID != "route-voice-tts" {
		t.Fatalf("tts target = %+v", tts)
	}
	if want := []string{"whisper_voice", "tts_voice"}; !slices.Equal(fake.ensured, want) {
		t.Fatalf("ensured = %v, want %v: the agent-managed legs, in leg order, and never the direct LLM", fake.ensured, want)
	}
	// ONE read of the agent's loaded set serves both of its legs: the residency verdict
	// is a single snapshot of "resident together", not two reads that could straddle a
	// swap. The agent router's own /running route, in its own format, is what is asked.
	wantProbe := voiceGateProbe{server: voiceGateAgentServer, path: routing.AgentRouterLoadedModelsPath, format: routing.AgentRouterLoadedModelsFormat}
	if len(fake.probes) != 1 || fake.probes[0] != wantProbe {
		t.Fatalf("loaded-set reads = %+v, want exactly one: %+v", fake.probes, wantProbe)
	}
}

// With the LLM on the agent too, all three children are ensured and all three must
// be in the one loaded set.
func TestVoiceCoLoadGatePassesWithThreeAgentLegs(t *testing.T) {
	fake := &voiceGateFake{}
	w := newVoiceGateWorld(t, fake, true)

	_, _, _, code, err := w.gate("voice-asr", "voice-llm", "voice-tts")
	if code != "" || err != nil {
		t.Fatalf("gate = (%q, %v), want a pass", code, err)
	}
	if want := []string{"whisper_voice", "llm_voice", "tts_voice"}; !slices.Equal(fake.ensured, want) {
		t.Fatalf("ensured = %v, want %v", fake.ensured, want)
	}
	if len(fake.probes) != 1 {
		t.Fatalf("loaded-set reads = %+v, want one snapshot of the one agent", fake.probes)
	}
}

// An application's own loaded_models_path replaces the router's /running
// (routing.EffectiveLoadedModelsProbe), so the residency read honours it exactly as the
// load core and the warmer do.
func TestVoiceCoLoadGateReadsTheApplicationsLoadedModelsPath(t *testing.T) {
	ctx := context.Background()
	fake := &voiceGateFake{}
	w := newVoiceGateWorld(t, fake, false)
	app, err := w.routes.ApplicationByID(ctx, "app-voice-agent")
	if err != nil {
		t.Fatalf("ApplicationByID: %v", err)
	}
	app.LoadedModelsPath, app.LoadedModelsFormat = "/loaded", "openai"
	if err := w.routes.UpdateApplication(ctx, app); err != nil {
		t.Fatalf("UpdateApplication: %v", err)
	}

	if _, _, _, code, err := w.gate("voice-asr", "voice-llm", "voice-tts"); code != "" || err != nil {
		t.Fatalf("gate = (%q, %v), want a pass", code, err)
	}
	want := voiceGateProbe{server: voiceGateAgentServer, path: "/loaded", format: "openai"}
	if len(fake.probes) != 1 || fake.probes[0] != want {
		t.Fatalf("loaded-set reads = %+v, want one: %+v", fake.probes, want)
	}
}

// A leg that cannot serve its role is refused with ITS code, before anything is warmed:
// an unknown model, an ASR that does not stream (or whose agent has not declared
// fullduplex_proxy, or whose spec excludes audio), a TTS that does not stream or is not
// offerable, and a model of the wrong kind for the LLM leg.
func TestVoiceCoLoadGateRefusesUnroutableLeg(t *testing.T) {
	cases := []struct {
		name         string
		asr, llm, tt string
		prepare      func(w *voiceGateWorld)
		want         string
		// cause is the specific refusal the gate must carry, so a leg refused for the wrong
		// reason (rather than the one under test) fails here.
		cause error
	}{
		{name: "unknown asr", asr: "no-such-asr", llm: "voice-llm", tt: "voice-tts", want: voiceChatASRUnavailable, cause: routing.ErrNoModelRoute},
		{name: "unknown llm", asr: "voice-asr", llm: "no-such-llm", tt: "voice-tts", want: voiceChatLLMUnavailable, cause: routing.ErrNoModelRoute},
		{name: "unknown tts", asr: "voice-asr", llm: "voice-llm", tt: "no-such-tts", want: voiceChatTTSUnavailable, cause: routing.ErrNoModelRoute},
		{name: "blank asr", asr: " ", llm: "voice-llm", tt: "voice-tts", want: voiceChatASRUnavailable, cause: errVoiceModelRequired},
		{name: "blank llm", asr: "voice-asr", llm: "", tt: "voice-tts", want: voiceChatLLMUnavailable, cause: errVoiceModelRequired},
		{name: "blank tts", asr: "voice-asr", llm: "voice-llm", tt: "\t", want: voiceChatTTSUnavailable, cause: errVoiceModelRequired},
		{name: "asr that does not stream", asr: "voice-asr-offline", llm: "voice-llm", tt: "voice-tts", want: voiceChatASRUnavailable, cause: errVoiceASRNotStreaming},
		{name: "asr whose spec excludes audio", asr: "voice-asr-textonly", llm: "voice-llm", tt: "voice-tts", want: voiceChatASRUnavailable, cause: routing.ErrNoModelRoute},
		{
			name: "asr whose agent has not declared fullduplex_proxy", asr: "voice-asr", llm: "voice-llm", tt: "voice-tts",
			prepare: func(w *voiceGateWorld) {
				w.srv.AgentFeatures.Set(voiceGateAgentServer, []string{"runtime_manager"})
			},
			want: voiceChatASRUnavailable, cause: errVoiceASRAgentUnsupported,
		},
		{name: "tts that does not stream", asr: "voice-asr", llm: "voice-llm", tt: "voice-tts-buffered", want: voiceChatTTSUnavailable, cause: errVoiceTTSNotStreaming},
		{name: "clone tts with no named preset", asr: "voice-asr", llm: "voice-llm", tt: "voice-tts-clone", want: voiceChatTTSUnavailable, cause: errVoiceTTSNotOfferable},
		{name: "asr model named for the tts leg", asr: "voice-asr", llm: "voice-llm", tt: "voice-asr", want: voiceChatTTSUnavailable, cause: routing.ErrModelNotCapable},
		{name: "tts model named for the asr leg", asr: "voice-tts", llm: "voice-llm", tt: "voice-tts", want: voiceChatASRUnavailable, cause: routing.ErrModelNotCapable},
		{name: "audio model named for the llm leg", asr: "voice-asr", llm: "voice-asr", tt: "voice-tts", want: voiceChatLLMUnavailable, cause: routing.ErrNoModelRoute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &voiceGateFake{}
			w := newVoiceGateWorld(t, fake, false)
			if tc.prepare != nil {
				tc.prepare(w)
			}
			asr, llm, tts, code, err := w.gate(tc.asr, tc.llm, tc.tt)
			requireVoiceGateRefused(t, asr, llm, tts, code, err, tc.want)
			if !errors.Is(err, tc.cause) {
				t.Fatalf("err = %v, want it to carry %v", err, tc.cause)
			}
			if len(fake.ensured) != 0 || len(fake.probes) != 0 {
				t.Fatalf("a refused leg still touched the agent: ensured %v, reads %+v", fake.ensured, fake.probes)
			}
		})
	}
}

// A clone TTS that does ship a named preset is offerable (CapabilityRowsOfferableTTS),
// so the same mapping that was refused above passes once its voices cache lists one.
func TestVoiceCoLoadGateAcceptsAPresetCloneTTS(t *testing.T) {
	fake := &voiceGateFake{}
	w := newVoiceGateWorld(t, fake, false)
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	if err := w.routes.UpsertModelMappingCache(context.Background(), routing.ModelMappingCacheRow{MappingID: "route-voice-tts-clone", CacheType: routing.ModelMappingCacheVoices, Payload: `["alba"]`, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertModelMappingCache: %v", err)
	}

	if _, _, tts, code, err := w.gate("voice-asr", "voice-llm", "voice-tts-clone"); code != "" || err != nil || tts.ProviderModel != "tts_clone" {
		t.Fatalf("gate = (%q, %v) with tts %+v, want a pass on the preset clone", code, err, tts)
	}
}

// An agent that answers an ensure with runtime.admission_blocked (a 503 the router does
// not expect a retry to cure) has refused to admit that child: the three cannot be
// resident together, and the gate says so rather than reporting a leg as unroutable.
func TestVoiceCoLoadGateRefusesWhenEnsureBlocked(t *testing.T) {
	blocked := &provider.RouterError{Code: "runtime.admission_blocked", Err: fmt.Errorf("%w: upstream status 503", provider.ErrUpstreamStarting)}
	for _, tc := range []struct {
		name        string
		blockedOn   string
		wantEnsured []string
	}{
		{"tts blocked after asr was warmed", "tts_voice", []string{"whisper_voice", "tts_voice"}},
		{"asr blocked: nothing later is attempted", "whisper_voice", []string{"whisper_voice"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &voiceGateFake{ensureErr: map[string]error{tc.blockedOn: blocked}}
			w := newVoiceGateWorld(t, fake, false)

			asr, llm, tts, code, err := w.gate("voice-asr", "voice-llm", "voice-tts")
			requireVoiceGateRefused(t, asr, llm, tts, code, err, voiceChatCoLoadFailed)
			var routerErr *provider.RouterError
			if !errors.As(err, &routerErr) || routerErr.Code != "runtime.admission_blocked" {
				t.Fatalf("err = %v, want it to carry the router's runtime.admission_blocked", err)
			}
			if !slices.Equal(fake.ensured, tc.wantEnsured) {
				t.Fatalf("ensured = %v, want %v", fake.ensured, tc.wantEnsured)
			}
			if len(fake.probes) != 0 {
				t.Fatalf("a blocked ensure still reached the residency read: %+v", fake.probes)
			}
		})
	}
}

// An ensure that fails for any OTHER reason -- the child did not start, started too
// slowly, the agent is unreachable or does not manage the model -- makes that leg's
// model unavailable, and names that leg in its code; it is not a co-residency verdict.
func TestVoiceCoLoadGateNamesTheLegWhoseEnsureFailed(t *testing.T) {
	startFailed := &provider.RouterError{Code: "runtime.start_failed", Err: fmt.Errorf("%w: upstream status 502", provider.ErrUnavailable)}
	for _, tc := range []struct {
		name    string
		model   string
		err     error
		want    string
		llmOnAg bool
	}{
		{"asr child failed to start", "whisper_voice", startFailed, voiceChatASRUnavailable, false},
		{"tts child timed out", "tts_voice", fmt.Errorf("%w: ensure route: deadline", provider.ErrTimeout), voiceChatTTSUnavailable, false},
		{"tts agent unreachable", "tts_voice", fmt.Errorf("%w: connection refused", provider.ErrUnavailable), voiceChatTTSUnavailable, false},
		{"llm child failed to start", "llm_voice", startFailed, voiceChatLLMUnavailable, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &voiceGateFake{ensureErr: map[string]error{tc.model: tc.err}}
			w := newVoiceGateWorld(t, fake, tc.llmOnAg)

			asr, llm, tts, code, err := w.gate("voice-asr", "voice-llm", "voice-tts")
			requireVoiceGateRefused(t, asr, llm, tts, code, err, tc.want)
			if !errors.Is(err, tc.err) {
				t.Fatalf("err = %v, want it to wrap the ensure failure %v", err, tc.err)
			}
		})
	}
}

// THE honest-gate test. Every ensure "succeeds", but the agent can keep only one child
// resident, so ensuring the TTS evicted the ASR. Three sequential ensure calls would
// pass this; the residency read does not, and refuses -- the first turn would otherwise
// reload and evict (the anti-goal).
func TestVoiceCoLoadGateRefusesWhenNotCoResident(t *testing.T) {
	for _, tc := range []struct {
		name       string
		slots      int
		llmOnAgent bool
		wantEvict  string
	}{
		{"asr and tts on a one-slot agent", 1, false, "whisper_voice"},
		{"three children on a two-slot agent", 2, true, "whisper_voice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &voiceGateFake{slots: tc.slots}
			w := newVoiceGateWorld(t, fake, tc.llmOnAgent)

			asr, llm, tts, code, err := w.gate("voice-asr", "voice-llm", "voice-tts")
			requireVoiceGateRefused(t, asr, llm, tts, code, err, voiceChatCoLoadFailed)
			if len(fake.ensured) < 2 {
				t.Fatalf("ensured = %v: every agent-managed leg is ensured before residency is judged", fake.ensured)
			}
			if !strings.Contains(err.Error(), tc.wantEvict) || !strings.Contains(err.Error(), "asr") {
				t.Fatalf("err = %q, want it to name the leg (asr) and model (%s) that is no longer resident", err, tc.wantEvict)
			}
			if len(fake.probes) != 1 {
				t.Fatalf("loaded-set reads = %+v, want the one snapshot", fake.probes)
			}
		})
	}
}

// A residency the gate cannot read is not one it may assume: if the loaded set cannot be
// read (the agent errors, or the provider cannot list loaded models at all) the gate
// refuses rather than pass on the strength of the ensure calls alone.
func TestVoiceCoLoadGateRefusesWhenResidencyCannotBeVerified(t *testing.T) {
	t.Run("the loaded-set read fails", func(t *testing.T) {
		fake := &voiceGateFake{probeErr: fmt.Errorf("%w: connection refused", provider.ErrUnavailable)}
		w := newVoiceGateWorld(t, fake, false)
		asr, llm, tts, code, err := w.gate("voice-asr", "voice-llm", "voice-tts")
		requireVoiceGateRefused(t, asr, llm, tts, code, err, voiceChatCoLoadFailed)
		if !errors.Is(err, provider.ErrUnavailable) {
			t.Fatalf("err = %v, want it to wrap the read failure", err)
		}
	})
	t.Run("the provider cannot list loaded models", func(t *testing.T) {
		w := newVoiceGateWorld(t, voiceGateEnsureOnly{}, false)
		asr, llm, tts, code, err := w.gate("voice-asr", "voice-llm", "voice-tts")
		requireVoiceGateRefused(t, asr, llm, tts, code, err, voiceChatCoLoadFailed)
	})
}

// An agent-managed leg is warmed through the ensure route, so a provider that has none
// cannot satisfy the gate.
func TestVoiceCoLoadGateRefusesWhenTheProviderCannotEnsure(t *testing.T) {
	w := newVoiceGateWorld(t, voiceGateNoEnsure{}, false)
	asr, llm, tts, code, err := w.gate("voice-asr", "voice-llm", "voice-tts")
	requireVoiceGateRefused(t, asr, llm, tts, code, err, voiceChatCoLoadFailed)
}

// The gate resolves and gates without spending the principal's admission: each /v1 core
// the loop calls spends its own, per turn. A principal allowed one request a minute passes
// the gate repeatedly, and its one request is still unspent afterwards.
func TestVoiceCoLoadGateSpendsNoAdmission(t *testing.T) {
	w := newVoiceGateWorld(t, &voiceGateFake{}, false)
	limits := newFakePrincipalStore()
	principal := Principal{Type: routing.PrincipalTypeUser, ID: w.token.UserID}
	limits.setConfig(principal, routing.LimitConfig{RateRequests: 1, RateWindowSeconds: 60})
	now := utc(2026, 10, 24, 12, 0, 0)
	w.srv.Limiter = NewPrincipalLimiter(limits, PrincipalLimiterOptions{Now: clockAt(&now)})

	for i := range 3 {
		if _, _, _, code, err := w.gate("voice-asr", "voice-llm", "voice-tts"); code != "" || err != nil {
			t.Fatalf("gate %d = (%q, %v), want a pass: it must not spend the principal's rate admission", i, code, err)
		}
	}
	if allow, reason, _ := w.srv.Limiter.Admit(context.Background(), principal); !allow {
		t.Fatalf("the principal's one request was spent (%s) by the gate", reason)
	}
}

// A cancelled request stops the gate: the ensure calls hold for a whole cold start, so
// the gate honours its context rather than outliving the browser that asked.
func TestVoiceCoLoadGateStopsOnACancelledContext(t *testing.T) {
	fake := &voiceGateFake{}
	w := newVoiceGateWorld(t, fake, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	asr, llm, tts, code, err := w.srv.voiceCoLoadGate(ctx, &w.token, "voice-asr", "voice-llm", "voice-tts")
	if code == "" {
		t.Fatal("a cancelled gate passed")
	}
	if !reflect.DeepEqual(asr, routing.Target{}) || !reflect.DeepEqual(llm, routing.Target{}) || !reflect.DeepEqual(tts, routing.Target{}) {
		t.Fatalf("a cancelled gate returned targets: %+v %+v %+v", asr, llm, tts)
	}
	if err == nil {
		t.Fatal("a cancelled gate refused without an error")
	}
	if len(fake.ensured) != 0 {
		t.Fatalf("a cancelled gate still ensured %v", fake.ensured)
	}
}

// The five codes are the portal's wire contract: the frontend and the docs key on these
// exact strings.
func TestVoiceChatErrorCodesAreStable(t *testing.T) {
	for got, want := range map[string]string{
		voiceChatASRUnavailable: "portal.voice_chat_asr_unavailable",
		voiceChatLLMUnavailable: "portal.voice_chat_llm_unavailable",
		voiceChatTTSUnavailable: "portal.voice_chat_tts_unavailable",
		voiceChatCoLoadFailed:   "portal.voice_chat_coload_failed",
		voiceChatTicketFailed:   "portal.voice_chat_ticket_failed",
	} {
		if got != want {
			t.Fatalf("code = %q, want %q", got, want)
		}
	}
}
