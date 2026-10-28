// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
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

// What the SHARED inference gate refuses -- a service token whose model allowlist excludes
// a leg's model (403 model.not_allowed), a token server override its owner may no longer
// manage (403 server_override.forbidden) -- happens inside inferenceGate, which answers by
// writing to a ResponseWriter. The gate has no client to answer, so it captures that
// answer from the writer it builds and the request it synthesizes from ctx; these cases
// are the only ones that run that path end to end (every refusal in the unroutable table
// above is the resolver's or the mapping's). Each refusal is that leg's *Unavailable code,
// carries the status and the gate's own stable code, hands back no target, and has not
// touched the agent.
func TestVoiceCoLoadGateRefusesWhatTheInferenceGateRefuses(t *testing.T) {
	serviceToken := func(allowed ...string) auth.Token {
		return auth.Token{ID: "tok_svc", Kind: "service", ServiceID: "svc_voice", ServiceName: "Voice", Active: true, AllowedModels: allowed}
	}
	all := []string{"voice-asr", "voice-llm", "voice-tts"}
	cases := []struct {
		name     string
		token    auth.Token
		stubAuth bool // install a portal that rejects every server override
		want     string
		wantCode string
	}{
		{name: "allowlist excludes the asr model", token: serviceToken("voice-llm", "voice-tts"), want: voiceChatASRUnavailable, wantCode: "model.not_allowed"},
		{name: "allowlist excludes the llm model", token: serviceToken("voice-asr", "voice-tts"), want: voiceChatLLMUnavailable, wantCode: "model.not_allowed"},
		{name: "allowlist excludes the tts model", token: serviceToken("voice-asr", "voice-llm"), want: voiceChatTTSUnavailable, wantCode: "model.not_allowed"},
		{
			name:     "token server override the owner may not manage",
			token:    auth.Token{ID: "tok_pinned", UserID: "usr_voice", Active: true, ServerOverride: voiceGateAgentServer},
			stubAuth: true, want: voiceChatASRUnavailable, wantCode: "server_override.forbidden",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &voiceGateFake{}
			w := newVoiceGateWorld(t, fake, false)
			w.token = tc.token
			stub := &fakePortalServerOverrideAuth{allow: map[string]bool{}}
			if tc.stubAuth {
				w.srv.Portal = stub
			}

			asr, llm, tts, code, err := w.gate("voice-asr", "voice-llm", "voice-tts")
			requireVoiceGateRefused(t, asr, llm, tts, code, err, tc.want)
			if !strings.Contains(err.Error(), "HTTP 403") || !strings.Contains(err.Error(), tc.wantCode) {
				t.Fatalf("err = %q, want the gate's HTTP 403 and its code %s", err, tc.wantCode)
			}
			if tc.stubAuth && stub.calls != 1 {
				t.Fatalf("AuthorizeServerManage calls = %d, want 1: the refusal must be the gate's re-authorization", stub.calls)
			}
			if len(fake.ensured) != 0 || len(fake.probes) != 0 {
				t.Fatalf("a refused leg still touched the agent: ensured %v, reads %+v", fake.ensured, fake.probes)
			}
		})
	}

	// The positive control: the same service token with every model allowed passes, so the
	// refusals above are the allowlist's and not the token kind's.
	t.Run("allowlist naming all three models passes", func(t *testing.T) {
		w := newVoiceGateWorld(t, &voiceGateFake{}, false)
		w.token = serviceToken(all...)
		if _, _, _, code, err := w.gate("voice-asr", "voice-llm", "voice-tts"); code != "" || err != nil {
			t.Fatalf("gate = (%q, %v), want a pass", code, err)
		}
	})
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

// The six codes are the portal's wire contract: the frontend and the docs key on these
// exact strings.
func TestVoiceChatErrorCodesAreStable(t *testing.T) {
	for got, want := range map[string]string{
		voiceChatASRUnavailable:       "portal.voice_chat_asr_unavailable",
		voiceChatLLMUnavailable:       "portal.voice_chat_llm_unavailable",
		voiceChatTTSUnavailable:       "portal.voice_chat_tts_unavailable",
		voiceChatCoLoadFailed:         "portal.voice_chat_coload_failed",
		voiceChatTicketFailed:         "portal.voice_chat_ticket_failed",
		voiceChatAudioSettingsInvalid: "portal.voice_chat_audio_settings_invalid",
	} {
		if got != want {
			t.Fatalf("code = %q, want %q", got, want)
		}
	}
}

// The voice-chat ticket endpoint (POST /api/portal/voice-chat-ticket) is driven through the
// real mux over the gate's own routing world, so the status and code a refusal reaches the
// browser with are the ones the real gate produced, not a stub's. What the endpoint adds in
// front of the gate -- the portal principal, the CSRF header, the chat the ticket is for --
// is added to that world here.

const (
	voiceTicketPath = "/api/portal/voice-chat-ticket"

	// The two bearers of the endpoint's fixture. The service token carries gateway:use ON
	// PURPOSE: the portal gate refuses a service token by its KIND, so a refusal here proves
	// the kind check and not a missing scope.
	voiceTicketBearer        = "voice-secret"
	voiceTicketServiceBearer = "voice-service-secret"
)

// voiceTicketWorld is the co-load gate's world plus the principals and the chat store the
// ticket endpoint reads in front of the gate.
type voiceTicketWorld struct {
	*voiceGateWorld
	directory *portal.MemoryDirectory
	chats     *portal.Service
}

func newVoiceTicketWorld(t *testing.T, prov provider.Client) *voiceTicketWorld {
	t.Helper()
	w := newVoiceGateWorld(t, prov, false)
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	tokens := auth.NewTokenStore()
	tokens.AddPlainToken(auth.Token{ID: "tok_voice", UserID: "usr_voice", ProjectID: "proj_voice", Name: "Voice", Active: true, Scopes: []string{"gateway:use"}}, voiceTicketBearer)
	tokens.AddPlainToken(auth.Token{ID: "tok_svc", Kind: "service", ServiceID: "svc_voice", ServiceName: "Voice", Active: true, Scopes: []string{"gateway:use"}}, voiceTicketServiceBearer)
	directory := portal.NewMemoryDirectory(tokens)
	cipher, err := capture.New(testCaptureKey)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	svc := portal.NewService(portal.ServiceDeps{
		Users: directory, Tokens: directory, Usage: usage.NewRecorder(), Routes: w.routes,
		Clock: func() time.Time { return now }, ModelLister: provider.NewMock(),
		Chats: store.NewMemoryChatStore(0), Cipher: cipher,
	})
	w.srv.Tokens = tokens
	w.srv.Portal = svc
	return &voiceTicketWorld{voiceGateWorld: w, directory: directory, chats: svc}
}

// newChat creates a chat owned by owner whose stored settings are the given JSON object, and
// returns its id.
func (w *voiceTicketWorld) newChat(t *testing.T, owner, settings string) string {
	t.Helper()
	created, err := w.chats.CreateChat(context.Background(), auth.Token{UserID: owner}, portal.CreateChatRequest{
		Content: json.RawMessage(`{"settings":` + settings + `,"messages":[]}`),
	})
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	return created.ID
}

// voiceTicketBody is the request body for the three gateway model names.
func voiceTicketBody(chatID, asr, llm, tts string) string {
	body, err := json.Marshal(map[string]string{"chat_id": chatID, "asr_model": asr, "llm_model": llm, "tts_model": tts})
	if err != nil {
		panic(err)
	}
	return string(body)
}

// voiceTicketBodyVoice is voiceTicketBody with the voice the speech model is to answer in.
func voiceTicketBodyVoice(chatID, asr, llm, tts, voice string) string {
	body, err := json.Marshal(map[string]string{"chat_id": chatID, "asr_model": asr, "llm_model": llm, "tts_model": tts, "voice": voice})
	if err != nil {
		panic(err)
	}
	return string(body)
}

func (w *voiceTicketWorld) post(body string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, voiceTicketPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	w.srv.ServeHTTP(rec, req)
	return rec
}

// postBearer posts body as the fixture's user token.
func (w *voiceTicketWorld) postBearer(body string) *httptest.ResponseRecorder {
	return w.post(body, map[string]string{"Authorization": "Bearer " + voiceTicketBearer})
}

// redeem reads the ticket out of a 200 response and redeems it on the server's own store,
// which is what the WebSocket upgrade does with it.
func (w *voiceTicketWorld) redeem(t *testing.T, rec *httptest.ResponseRecorder) voiceTicketClaims {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Ticket string `json:"ticket"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Ticket == "" {
		t.Fatalf("response carries no ticket (%v): %s", err, rec.Body.String())
	}
	claims, err := w.srv.voiceTickets.Redeem(body.Ticket)
	if err != nil {
		t.Fatalf("the minted ticket does not redeem on the server's own store: %v", err)
	}
	return claims
}

// requireNothingStarted fails when the fake agent was asked to start or read anything: a
// refusal that precedes the gate must not have cost a cold start.
func requireNothingStarted(t *testing.T, fake *voiceGateFake) {
	t.Helper()
	if len(fake.ensured) != 0 || len(fake.probes) != 0 {
		t.Fatalf("the agent was touched (ensured %v, reads %+v) by a request refused before the gate", fake.ensured, fake.probes)
	}
}

// voiceGateStalledEnsure is an agent whose ensure answers nothing until its context ends:
// the cold start that never finishes.
type voiceGateStalledEnsure struct{ voiceGateFake }

func (f *voiceGateStalledEnsure) EnsureRuntimeModel(ctx context.Context, target routing.Target) error {
	f.mu.Lock()
	f.ensured = append(f.ensured, target.ProviderModel)
	f.mu.Unlock()
	<-ctx.Done()
	return ctx.Err()
}

// A pass mints a ticket that carries what the WebSocket will need and never takes from the
// client: the three resolved targets, the chat, who asked, the chat's stored speech settings
// and the one PCM shape the browser streams. The response is the ticket and nothing else.
func TestVoiceTicketEndpointMintsOnGatePass(t *testing.T) {
	fake := &voiceGateFake{}
	w := newVoiceTicketWorld(t, fake)
	chatID := w.newChat(t, "usr_voice", `{"kind":"speech","model":"voice-tts","audio":{"voice":"alba","speed":1.25,"language":"de","stream":false,"passthrough":{"instructions":"calm"}}}`)

	rec := w.postBearer(voiceTicketBody(chatID, "voice-asr", "voice-llm", "voice-tts"))

	claims := w.redeem(t, rec)
	if claims.ASR.Model != "voice-asr" || claims.ASR.ProviderModel != "whisper_voice" || claims.ASR.ServerID != voiceGateAgentServer || claims.ASR.RouteID != "route-voice-asr" {
		t.Fatalf("ASR target = %+v", claims.ASR)
	}
	if claims.LLM.Model != "voice-llm" || claims.LLM.ProviderModel != "llm_voice" || claims.LLM.ServerID != voiceGateLLMServer {
		t.Fatalf("LLM target = %+v", claims.LLM)
	}
	if claims.TTS.Model != "voice-tts" || claims.TTS.ProviderModel != "tts_voice" || claims.TTS.ServerID != voiceGateAgentServer || claims.TTS.RouteID != "route-voice-tts" {
		t.Fatalf("TTS target = %+v", claims.TTS)
	}
	if claims.ChatID != chatID || claims.UserID != "usr_voice" || claims.TokenID != "tok_voice" || claims.ProjectID != "proj_voice" || claims.ServiceID != "" {
		t.Fatalf("chat/user/token/project/service = %q/%q/%q/%q/%q", claims.ChatID, claims.UserID, claims.TokenID, claims.ProjectID, claims.ServiceID)
	}
	speed, stream := 1.25, false
	wantAudio := portal.ChatAudioSettings{
		Voice: "alba", Speed: &speed, Language: "de", Stream: &stream,
		Passthrough: map[string]json.RawMessage{"instructions": json.RawMessage(`"calm"`)},
	}
	if !reflect.DeepEqual(claims.Audio, wantAudio) {
		t.Fatalf("Audio = %+v, want the chat's stored speech settings %+v", claims.Audio, wantAudio)
	}
	if claims.SampleRate != 16000 || claims.Channels != 1 || claims.SampleFormat != "s16le" {
		t.Fatalf("PCM = %d/%d/%q, want the 16 kHz mono s16le the browser streams", claims.SampleRate, claims.Channels, claims.SampleFormat)
	}
	if want := []string{"whisper_voice", "tts_voice"}; !slices.Equal(fake.ensured, want) {
		t.Fatalf("ensured = %v, want the gate to have warmed %v", fake.ensured, want)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if len(body) != 1 || body["ticket"] == nil {
		t.Fatalf("response = %v, want exactly one key, ticket: the claims stay encrypted inside it", body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
}

// A chat that stores no speech settings (a plain text chat, which PrepareChatRun clears them
// from) is still voice-chattable: the ticket carries the zero settings, i.e. the model's own
// default voice.
func TestVoiceTicketEndpointMintsDefaultAudioForAChatWithoutSpeechSettings(t *testing.T) {
	for name, settings := range map[string]string{
		"empty settings":   `{}`,
		"text settings":    `{"model":"voice-llm","temperature":0.7}`,
		"null audio":       `{"audio":null}`,
		"null settings":    `null`,
		"settings missing": ``,
	} {
		t.Run(name, func(t *testing.T) {
			w := newVoiceTicketWorld(t, &voiceGateFake{})
			content := `{"messages":[]}`
			if settings != "" {
				content = `{"settings":` + settings + `,"messages":[]}`
			}
			created, err := w.chats.CreateChat(context.Background(), auth.Token{UserID: "usr_voice"}, portal.CreateChatRequest{Content: json.RawMessage(content)})
			if err != nil {
				t.Fatalf("CreateChat: %v", err)
			}

			claims := w.redeem(t, w.postBearer(voiceTicketBody(created.ID, "voice-asr", "voice-llm", "voice-tts")))

			if !reflect.DeepEqual(claims.Audio, portal.ChatAudioSettings{}) {
				t.Fatalf("Audio = %+v, want the zero settings", claims.Audio)
			}
		})
	}
}

// The voice the speech model answers in rides the request (#318): the portal's voice dropdown
// picks it, the chat's stored settings.audio (a speech thread's own) has none of it for a text
// thread. The ticket's speech settings -- which the voice run's TTS leg reads -- take it, over a
// stored voice, and keep everything else of the stored settings.
func TestVoiceTicketEndpointMintsTheRequestedVoice(t *testing.T) {
	fake := &voiceGateFake{}
	w := newVoiceTicketWorld(t, fake)
	chatID := w.newChat(t, "usr_voice", `{"audio":{"voice":"stored","speed":1.25,"language":"de"}}`)

	claims := w.redeem(t, w.postBearer(voiceTicketBodyVoice(chatID, "voice-asr", "voice-llm", "voice-tts", "marius")))

	speed := 1.25
	if want := (portal.ChatAudioSettings{Voice: "marius", Speed: &speed, Language: "de"}); !reflect.DeepEqual(claims.Audio, want) {
		t.Fatalf("Audio = %+v, want the requested voice over the stored one and the rest as stored (%+v)", claims.Audio, want)
	}
}

// A chat with no speech settings (a text thread) gets the requested voice and nothing else.
func TestVoiceTicketEndpointMintsTheRequestedVoiceForAChatWithoutSpeechSettings(t *testing.T) {
	w := newVoiceTicketWorld(t, &voiceGateFake{})
	chatID := w.newChat(t, "usr_voice", `{"model":"voice-llm"}`)

	claims := w.redeem(t, w.postBearer(voiceTicketBodyVoice(chatID, "voice-asr", "voice-llm", "voice-tts", "marius")))

	if want := (portal.ChatAudioSettings{Voice: "marius"}); !reflect.DeepEqual(claims.Audio, want) {
		t.Fatalf("Audio = %+v, want %+v", claims.Audio, want)
	}
}

// No voice in the request (an older portal, or a plain model left on its own default) changes
// nothing: the stored settings stand, and a blank or whitespace-only voice is no voice.
func TestVoiceTicketEndpointKeepsTheStoredVoiceWhenNoneIsRequested(t *testing.T) {
	for name, body := range map[string]func(chatID string) string{
		"no voice member": func(chatID string) string { return voiceTicketBody(chatID, "voice-asr", "voice-llm", "voice-tts") },
		"an empty voice": func(chatID string) string {
			return voiceTicketBodyVoice(chatID, "voice-asr", "voice-llm", "voice-tts", "")
		},
		"a blank voice": func(chatID string) string {
			return voiceTicketBodyVoice(chatID, "voice-asr", "voice-llm", "voice-tts", " \t ")
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := newVoiceTicketWorld(t, &voiceGateFake{})
			chatID := w.newChat(t, "usr_voice", `{"audio":{"voice":"stored"}}`)

			claims := w.redeem(t, w.postBearer(body(chatID)))

			if claims.Audio.Voice != "stored" {
				t.Fatalf("voice = %q, want the stored voice", claims.Audio.Voice)
			}
		})
	}
}

// The requested voice is user-typed free text like the stored one, so it counts against the
// same cap: a voice that would push the ticket's speech settings past voiceTicketAudioMaxBytes
// is refused as unusable settings BEFORE the gate's cold start, and one at the cap mints.
func TestVoiceTicketEndpointCapsTheRequestedVoice(t *testing.T) {
	const voiceEnvelope = 12 // len(`{"voice":""}`)
	atCap := strings.Repeat("v", voiceTicketAudioMaxBytes-voiceEnvelope)

	t.Run("at the cap", func(t *testing.T) {
		w := newVoiceTicketWorld(t, &voiceGateFake{})
		chatID := w.newChat(t, "usr_voice", `{}`)

		claims := w.redeem(t, w.postBearer(voiceTicketBodyVoice(chatID, "voice-asr", "voice-llm", "voice-tts", atCap)))

		if claims.Audio.Voice != atCap {
			t.Fatalf("voice length = %d, want the cap-sized voice carried whole", len(claims.Audio.Voice))
		}
	})
	t.Run("one byte over", func(t *testing.T) {
		fake := &voiceGateFake{}
		w := newVoiceTicketWorld(t, fake)
		chatID := w.newChat(t, "usr_voice", `{}`)

		rec := w.postBearer(voiceTicketBodyVoice(chatID, "voice-asr", "voice-llm", "voice-tts", atCap+"v"))

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
		}
		requireErrorCode(t, rec.Body.String(), voiceChatAudioSettingsInvalid)
		requireNoTicket(t, rec)
		requireNothingStarted(t, fake)
	})
}

// A preset-clone speech model has no default voice of its own, so a ticket that names none for it
// is minted on its first preset (routing.FirstPresetVoice, the voices cache's smallest named
// voice): the portal pre-selects that one itself, and this is the belt for a request that could
// not (the voices were still loading, or an older portal). A voice that IS named, or a stored
// one, wins; a plain model keeps its own default voice, whatever its cache lists.
func TestVoiceTicketEndpointDefaultsAPresetCloneToItsFirstPreset(t *testing.T) {
	const clone, plain = "voice-tts-clone", "voice-tts"
	cache := func(w *voiceTicketWorld, mappingID, payload string) {
		t.Helper()
		row := routing.ModelMappingCacheRow{MappingID: mappingID, CacheType: routing.ModelMappingCacheVoices, Payload: payload, UpdatedAt: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
		if err := w.routes.UpsertModelMappingCache(context.Background(), row); err != nil {
			t.Fatalf("UpsertModelMappingCache %s: %v", mappingID, err)
		}
	}
	for _, tc := range []struct {
		name      string
		tts       string
		settings  string
		requested string
		want      string
	}{
		{"a clone with no voice named", clone, `{}`, "", "alba"},
		{"a clone with a blank voice named", clone, `{}`, "  ", "alba"},
		{"a clone with a requested voice", clone, `{}`, "zeta", "zeta"},
		{"a clone with a stored voice", clone, `{"audio":{"voice":"stored"}}`, "", "stored"},
		{"a plain model with voices cached", plain, `{}`, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newVoiceTicketWorld(t, &voiceGateFake{})
			// Unsorted on purpose: the first preset is the smallest name, not the first listed.
			cache(w, "route-voice-tts-clone", `["zeta","alba"]`)
			cache(w, "route-voice-tts", `["marius","alba"]`)
			chatID := w.newChat(t, "usr_voice", tc.settings)

			claims := w.redeem(t, w.postBearer(voiceTicketBodyVoice(chatID, "voice-asr", "voice-llm", tc.tts, tc.requested)))

			if claims.Audio.Voice != tc.want {
				t.Fatalf("voice = %q, want %q", claims.Audio.Voice, tc.want)
			}
		})
	}
}

// The model names are trimmed the way the other audio gates trim them.
func TestVoiceTicketEndpointTrimsTheModelNames(t *testing.T) {
	w := newVoiceTicketWorld(t, &voiceGateFake{})
	chatID := w.newChat(t, "usr_voice", `{}`)

	claims := w.redeem(t, w.postBearer(voiceTicketBody("  "+chatID+"  ", "  voice-asr ", " voice-llm", "voice-tts\t")))

	if claims.ASR.RouteID != "route-voice-asr" || claims.LLM.RouteID != "route-voice-llm" || claims.TTS.RouteID != "route-voice-tts" || claims.ChatID != chatID {
		t.Fatalf("routes/chat = %q/%q/%q/%q", claims.ASR.RouteID, claims.LLM.RouteID, claims.TTS.RouteID, claims.ChatID)
	}
}

// Every mint is its own ticket, so one cannot stand in for another.
func TestVoiceTicketEndpointEveryMintIsDistinct(t *testing.T) {
	w := newVoiceTicketWorld(t, &voiceGateFake{})
	chatID := w.newChat(t, "usr_voice", `{}`)
	body := voiceTicketBody(chatID, "voice-asr", "voice-llm", "voice-tts")

	first, second := w.redeem(t, w.postBearer(body)), w.redeem(t, w.postBearer(body))

	if first.Nonce == second.Nonce {
		t.Fatalf("two mints share the nonce %q", first.Nonce)
	}
}

// A gate refusal is the gate's own code at the status its kind maps to -- a leg that cannot
// serve its role is the caller's to change (400), models that cannot be held together are a
// conflict of the deployment (409) -- and nothing is minted. The message is worded here from
// the code: the cause (the agent's reply, an internal model name) is for the log, never the
// client.
func TestVoiceTicketEndpointRefusesLeg(t *testing.T) {
	blocked := &provider.RouterError{Code: voiceAdmissionBlockedCode, Err: fmt.Errorf("%w: upstream status 503", provider.ErrUpstreamStarting)}
	for _, tc := range []struct {
		name          string
		fake          *voiceGateFake
		asr, llm, tts string
		wantStatus    int
		wantCode      string
	}{
		{"an unknown asr model", &voiceGateFake{}, "no-such-asr", "voice-llm", "voice-tts", http.StatusBadRequest, voiceChatASRUnavailable},
		{"an asr model that does not stream", &voiceGateFake{}, "voice-asr-offline", "voice-llm", "voice-tts", http.StatusBadRequest, voiceChatASRUnavailable},
		{"an unknown chat model", &voiceGateFake{}, "voice-asr", "no-such-llm", "voice-tts", http.StatusBadRequest, voiceChatLLMUnavailable},
		{"an audio model in the llm slot", &voiceGateFake{}, "voice-asr", "voice-asr", "voice-tts", http.StatusBadRequest, voiceChatLLMUnavailable},
		{"an unknown speech model", &voiceGateFake{}, "voice-asr", "voice-llm", "no-such-tts", http.StatusBadRequest, voiceChatTTSUnavailable},
		{"a speech model that does not stream", &voiceGateFake{}, "voice-asr", "voice-llm", "voice-tts-buffered", http.StatusBadRequest, voiceChatTTSUnavailable},
		{"an agent that refuses to admit a child", &voiceGateFake{ensureErr: map[string]error{"tts_voice": blocked}}, "voice-asr", "voice-llm", "voice-tts", http.StatusConflict, voiceChatCoLoadFailed},
		{"a start that evicts an earlier child", &voiceGateFake{slots: 1}, "voice-asr", "voice-llm", "voice-tts", http.StatusConflict, voiceChatCoLoadFailed},
		{"a residency that cannot be read", &voiceGateFake{probeErr: errors.New("agent unreachable")}, "voice-asr", "voice-llm", "voice-tts", http.StatusConflict, voiceChatCoLoadFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newVoiceTicketWorld(t, tc.fake)
			chatID := w.newChat(t, "usr_voice", `{}`)

			rec := w.postBearer(voiceTicketBody(chatID, tc.asr, tc.llm, tc.tts))

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			requireErrorCode(t, rec.Body.String(), tc.wantCode)
			requireNoTicket(t, rec)
			for _, leak := range []string{"admission_blocked", "evicted", "unreachable", "whisper_", "tts_voice", "llm_voice", "srv-voice"} {
				if strings.Contains(rec.Body.String(), leak) {
					t.Fatalf("body = %s, want the cause kept out of the client's response (found %q)", rec.Body.String(), leak)
				}
			}
		})
	}
}

// Every code the gate can refuse with has a row: the two kinds of unavailable leg are the
// caller's (400), a pairing that cannot be held resident is a conflict (409), and a ticket
// that could not be issued is the server's (500), as is any code nobody mapped.
func TestVoiceTicketEndpointMapsEveryGateCodeToItsStatus(t *testing.T) {
	for code, want := range map[string]int{
		voiceChatASRUnavailable:       http.StatusBadRequest,
		voiceChatLLMUnavailable:       http.StatusBadRequest,
		voiceChatTTSUnavailable:       http.StatusBadRequest,
		voiceChatAudioSettingsInvalid: http.StatusBadRequest,
		voiceChatCoLoadFailed:         http.StatusConflict,
		voiceChatTicketFailed:         http.StatusInternalServerError,
		"portal.voice_chat_unmapped":  http.StatusInternalServerError,
	} {
		status, body := voiceChatRefusal(code)
		if status != want {
			t.Fatalf("%s: status = %d, want %d", code, status, want)
		}
		if body.Error.Message == "" {
			t.Fatalf("%s: no message", code)
		}
		wantCode := code
		if code == "portal.voice_chat_unmapped" {
			wantCode = voiceChatTicketFailed // an unforeseen code is never echoed
		}
		if body.Error.Code != wantCode {
			t.Fatalf("%s: body code = %q, want %q", code, body.Error.Code, wantCode)
		}
	}
}

// THE CSRF DEFENSE, as for the live-transcription ticket: the WebSocket upgrade cannot carry
// X-OP-CSRF, so this POST is the only place the portal's protection is applied to the voice
// path. A session cookie without the header (or with a wrong one) is 403 and starts nothing;
// the same cookie with it is the positive control and mints the logged-in user's ticket.
func TestVoiceTicketEndpointRequiresCSRF(t *testing.T) {
	fake := &voiceGateFake{}
	w := newVoiceTicketWorld(t, fake)
	withLiveTicketSessions(w.srv, w.directory)
	seedLoginUser(t, w.directory, "usr_session", "voice@example.test", "password-1", "user")
	cookie := loginCookie(t, w.srv, "voice@example.test", "password-1")
	cookieHeader := cookie.Name + "=" + cookie.Value
	chatID := w.newChat(t, "usr_session", `{}`)
	body := voiceTicketBody(chatID, "voice-asr", "voice-llm", "voice-tts")

	for _, tc := range []struct {
		name string
		hdr  map[string]string
	}{
		{"no CSRF header", map[string]string{"Cookie": cookieHeader}},
		{"a CSRF header that is not the portal's value", map[string]string{"Cookie": cookieHeader, csrfHeaderName: "0"}},
		{"an empty CSRF header", map[string]string{"Cookie": cookieHeader, csrfHeaderName: ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := w.post(body, tc.hdr)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body = %s", rec.Code, rec.Body.String())
			}
			requireErrorCode(t, rec.Body.String(), "auth.csrf_required")
			requireNoTicket(t, rec)
			requireNothingStarted(t, fake)
		})
	}

	claims := w.redeem(t, w.post(body, map[string]string{"Cookie": cookieHeader, csrfHeaderName: "1"}))
	// The session principal carries the user and no token row.
	if claims.UserID != "usr_session" || claims.TokenID != "" || claims.ChatID != chatID {
		t.Fatalf("UserID/TokenID/ChatID = %q/%q/%q, want usr_session, no token and the chat", claims.UserID, claims.TokenID, claims.ChatID)
	}
}

// A service token is the gateway's "LLM-invoke only" principal and never reaches a portal
// route, whatever scopes it carries: refused 401 before the body is read, nothing started.
func TestVoiceTicketEndpointRejectsServiceToken(t *testing.T) {
	fake := &voiceGateFake{}
	w := newVoiceTicketWorld(t, fake)
	chatID := w.newChat(t, "usr_voice", `{}`)

	rec := w.post(voiceTicketBody(chatID, "voice-asr", "voice-llm", "voice-tts"), map[string]string{"Authorization": "Bearer " + voiceTicketServiceBearer})

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.String(), "auth.invalid_token")
	requireNoTicket(t, rec)
	requireNothingStarted(t, fake)
}

// No credentials, an unknown bearer and a token without gateway:use are each refused before
// anything is resolved or started.
func TestVoiceTicketEndpointRequiresAuthenticationAndScope(t *testing.T) {
	for _, tc := range []struct {
		name       string
		hdr        map[string]string
		wantStatus int
		wantCode   string
	}{
		{"no credentials", nil, http.StatusUnauthorized, "auth.invalid_token"},
		{"an unknown bearer", map[string]string{"Authorization": "Bearer not-a-token"}, http.StatusUnauthorized, "auth.invalid_token"},
		{"a token without gateway:use", map[string]string{"Authorization": "Bearer invoke-secret"}, http.StatusForbidden, "auth.insufficient_scope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &voiceGateFake{}
			w := newVoiceTicketWorld(t, fake)
			w.srv.Tokens.(*auth.TokenStore).AddPlainToken(auth.Token{ID: "tok_invoke", UserID: "usr_voice", Active: true, Scopes: []string{"llm:invoke"}}, "invoke-secret")
			chatID := w.newChat(t, "usr_voice", `{}`)

			rec := w.post(voiceTicketBody(chatID, "voice-asr", "voice-llm", "voice-tts"), tc.hdr)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			requireErrorCode(t, rec.Body.String(), tc.wantCode)
			requireNoTicket(t, rec)
			requireNothingStarted(t, fake)
		})
	}
}

// The ticket is minted on POST only: it creates something, and a GET is CSRF-exempt.
func TestVoiceTicketEndpointRefusesOtherMethods(t *testing.T) {
	w := newVoiceTicketWorld(t, &voiceGateFake{})
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, voiceTicketPath, nil)
		req.Header.Set("Authorization", "Bearer "+voiceTicketBearer)
		rec := httptest.NewRecorder()
		w.srv.ServeHTTP(rec, req)

		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: status = %d, want 405", method, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow != http.MethodPost {
			t.Fatalf("%s: Allow = %q, want POST", method, allow)
		}
	}
}

// A blank model on any leg is the endpoint family's own 400 (model.name_required), before
// the chat is read or anything is started: it is not a leg that "cannot be used".
func TestVoiceTicketEndpointRefusesABlankModel(t *testing.T) {
	for _, tc := range []struct {
		name          string
		asr, llm, tts string
	}{
		{"asr", "", "voice-llm", "voice-tts"},
		{"llm", "voice-asr", "   ", "voice-tts"},
		{"tts", "voice-asr", "voice-llm", "\t"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &voiceGateFake{}
			w := newVoiceTicketWorld(t, fake)
			chatID := w.newChat(t, "usr_voice", `{}`)

			rec := w.postBearer(voiceTicketBody(chatID, tc.asr, tc.llm, tc.tts))

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
			}
			requireErrorCode(t, rec.Body.String(), audioVoicesModelRequired)
			requireNoTicket(t, rec)
			requireNothingStarted(t, fake)
		})
	}
}

// A body that is not one JSON object of the right shape is the shared request.invalid_json.
func TestVoiceTicketEndpointRefusesAMalformedBody(t *testing.T) {
	for _, body := range []string{``, `not json`, `[]`, `{"chat_id":7}`, `{"asr_model":["a"]}`, voiceTicketBody("c", "a", "b", "c") + `{}`} {
		w := newVoiceTicketWorld(t, &voiceGateFake{})

		rec := w.postBearer(body)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%q: status = %d, want 400; body = %s", body, rec.Code, rec.Body.String())
		}
		requireErrorCode(t, rec.Body.String(), codeRequestInvalidJSON)
		requireNoTicket(t, rec)
	}
}

// The chat is owner-scoped like every portal chat read: a blank id, an unknown chat and
// another user's chat are the same 404 portal.chat_not_found (no probe of which ids exist),
// and the chat is read BEFORE the gate, so none of them costs a cold start.
func TestVoiceTicketEndpointRefusesAChatThatIsNotTheCallers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		chat  func(w *voiceTicketWorld) string
		model string
	}{
		{"a blank chat id", func(*voiceTicketWorld) string { return "  " }, "voice-asr"},
		{"an unknown chat", func(*voiceTicketWorld) string { return "chat_does_not_exist" }, "voice-asr"},
		{"another user's chat", func(w *voiceTicketWorld) string { return w.newChat(t, "usr_someone_else", `{}`) }, "voice-asr"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &voiceGateFake{}
			w := newVoiceTicketWorld(t, fake)

			rec := w.postBearer(voiceTicketBody(tc.chat(w), tc.model, "voice-llm", "voice-tts"))

			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
			}
			requireErrorCode(t, rec.Body.String(), "portal.chat_not_found")
			requireNoTicket(t, rec)
			requireNothingStarted(t, fake)
		})
	}
}

// The ticket rides a Sec-WebSocket-Protocol header, which a proxy refuses past its header-line
// limit (8 KB at nginx's default), so the chat's speech settings are capped: their JSON may be
// voiceTicketAudioMaxBytes at most, voice and language included, because they are free text a
// user types. At the cap the ticket mints and stays well under the proxy limit; one byte over,
// the request is refused 400 BEFORE the gate, rather than minting a ticket the proxy would
// drop.
func TestVoiceTicketEndpointCapsTheChatsAudioSettings(t *testing.T) {
	// {"voice":"..."} is len(`{"voice":""}`) = 12 bytes around the name.
	const voiceEnvelope = 12
	atCap := strings.Repeat("v", voiceTicketAudioMaxBytes-voiceEnvelope)

	t.Run("at the cap", func(t *testing.T) {
		w := newVoiceTicketWorld(t, &voiceGateFake{})
		chatID := w.newChat(t, "usr_voice", `{"audio":{"voice":"`+atCap+`"}}`)

		rec := w.postBearer(voiceTicketBody(chatID, "voice-asr", "voice-llm", "voice-tts"))

		claims := w.redeem(t, rec)
		if claims.Audio.Voice != atCap {
			t.Fatalf("voice length = %d, want the cap-sized voice carried whole", len(claims.Audio.Voice))
		}
		var body struct {
			Ticket string `json:"ticket"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		t.Logf("ticket at the settings cap: %d bytes", len(body.Ticket))
		requireTicketFitsAProxyHeaderLine(t, body.Ticket)
	})

	for name, settings := range map[string]string{
		"one byte over":      `{"audio":{"voice":"` + atCap + `v"}}`,
		"an oversized value": `{"audio":{"passthrough":{"instructions":"` + strings.Repeat("x", 3*voiceTicketAudioMaxBytes) + `"}}}`,
		"many small keys": func() string {
			var keys []string
			for i := 0; i < 400; i++ {
				keys = append(keys, fmt.Sprintf(`"k%03d":%d`, i, i))
			}
			return `{"audio":{"passthrough":{` + strings.Join(keys, ",") + `}}}`
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			fake := &voiceGateFake{}
			w := newVoiceTicketWorld(t, fake)
			chatID := w.newChat(t, "usr_voice", settings)

			rec := w.postBearer(voiceTicketBody(chatID, "voice-asr", "voice-llm", "voice-tts"))

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
			}
			requireErrorCode(t, rec.Body.String(), voiceChatAudioSettingsInvalid)
			requireNoTicket(t, rec)
			requireNothingStarted(t, fake)
		})
	}
}

// voiceProxyHeaderLineBytes is the longest header line a proxy accepts at its default (nginx's
// large_client_header_buffers, 8 KB). The ticket rides one Sec-WebSocket-Protocol line.
const voiceProxyHeaderLineBytes = 8 * 1024

// requireTicketFitsAProxyHeaderLine fails unless the ticket is within the endpoint's own
// ceiling AND that ceiling leaves the header name and the fixed subprotocol (and a margin) room
// on a default proxy's header line.
func requireTicketFitsAProxyHeaderLine(t *testing.T, ticket string) {
	t.Helper()
	if voiceProxyHeaderLineBytes-voiceTicketMaxBytes < 1024 {
		t.Fatalf("voiceTicketMaxBytes = %d leaves under 1 KiB of the %d-byte proxy header line for the header name and subprotocol", voiceTicketMaxBytes, voiceProxyHeaderLineBytes)
	}
	if len(ticket) > voiceTicketMaxBytes {
		t.Fatalf("ticket = %d bytes, want at most the %d-byte ceiling", len(ticket), voiceTicketMaxBytes)
	}
}

// voiceSealedCredential is a stand-in for an application's sealed upstream credential of
// length n ("enc:" and base64, as capture.SealSecret writes it). A real one is ~110
// characters for an API key and 200+ for a long token; each of the three legs carries one.
func voiceSealedCredential(n int) string {
	return "enc:" + strings.Repeat("Q", n-len("enc:"))
}

// withSealedCredentials gives every application of the world a sealed upstream credential of n
// characters, so each resolved target of a ticket carries one, as it does in a deployment whose
// upstream applications are authenticated.
func (w *voiceTicketWorld) withSealedCredentials(t *testing.T, n int) {
	t.Helper()
	ctx := context.Background()
	for _, id := range []string{"app-voice-agent", "app-voice-llm"} {
		app, err := w.routes.ApplicationByID(ctx, id)
		if err != nil {
			t.Fatalf("ApplicationByID %s: %v", id, err)
		}
		app.APIToken = voiceSealedCredential(n)
		if err := w.routes.UpdateApplication(ctx, app); err != nil {
			t.Fatalf("UpdateApplication %s: %v", id, err)
		}
	}
}

// A deployment whose three upstream applications are authenticated seals a credential into
// every target of the ticket, and that is the part of the ticket that varies most. Typical
// settings mint with ~164-character credentials (an API key) and with 260-character ones (a
// long token), and so do settings that fill the cap: the cap was chosen so the user's part can
// never push a ticket past what a proxy header line holds, which is only discovered AFTER the
// gate's cold start.
func TestVoiceTicketEndpointMintsWithRealisticSealedCredentials(t *testing.T) {
	const typicalSettings = `{"audio":{"voice":"alba","speed":1.0,"language":"de","passthrough":{"instructions":"speak calmly"}}}`
	const voiceEnvelope = 12 // len(`{"voice":""}`)
	atCap := `{"audio":{"voice":"` + strings.Repeat("v", voiceTicketAudioMaxBytes-voiceEnvelope) + `"}}`
	for _, tc := range []struct {
		name       string
		credential int
		settings   string
	}{
		{"typical settings, 164-character credentials", 164, typicalSettings},
		{"typical settings, 260-character credentials", 260, typicalSettings},
		{"settings at the cap, 164-character credentials", 164, atCap},
		{"settings at the cap, 260-character credentials", 260, atCap},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newVoiceTicketWorld(t, &voiceGateFake{})
			w.withSealedCredentials(t, tc.credential)
			chatID := w.newChat(t, "usr_voice", tc.settings)

			rec := w.postBearer(voiceTicketBody(chatID, "voice-asr", "voice-llm", "voice-tts"))

			claims := w.redeem(t, rec)
			for leg, target := range map[string]routing.Target{"asr": claims.ASR, "llm": claims.LLM, "tts": claims.TTS} {
				if len(target.APIToken) != tc.credential {
					t.Fatalf("%s target credential = %d characters, want the %d the application carries", leg, len(target.APIToken), tc.credential)
				}
			}
			var body struct {
				Ticket string `json:"ticket"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			t.Logf("ticket: %d bytes (ceiling %d)", len(body.Ticket), voiceTicketMaxBytes)
			requireTicketFitsAProxyHeaderLine(t, body.Ticket)
		})
	}
}

// Stored speech settings that do not decode as speech settings are as unusable as oversized
// ones: the same 400, not a silent fall back to the default voice the user did not pick.
func TestVoiceTicketEndpointRefusesUnreadableAudioSettings(t *testing.T) {
	for name, settings := range map[string]string{
		"audio is a string":            `{"audio":"alba"}`,
		"speed is not a number":        `{"audio":{"speed":"fast"}}`,
		"passthrough is not an object": `{"audio":{"passthrough":[1,2]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			fake := &voiceGateFake{}
			w := newVoiceTicketWorld(t, fake)
			chatID := w.newChat(t, "usr_voice", settings)

			rec := w.postBearer(voiceTicketBody(chatID, "voice-asr", "voice-llm", "voice-tts"))

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
			}
			requireErrorCode(t, rec.Body.String(), voiceChatAudioSettingsInvalid)
			requireNoTicket(t, rec)
			requireNothingStarted(t, fake)
		})
	}
}

// The gate can block for a whole cold start (it ensures models), so the endpoint bounds it
// with its own deadline, and a gate that outlives it refuses as a co-load failure -- the models
// could not be made resident in time -- not as an unavailable leg and not by hanging. The
// stalled agent answers nothing until its context ends, so only the deadline ends the call.
func TestVoiceTicketEndpointBoundsTheGateWithADeadline(t *testing.T) {
	fake := &voiceGateStalledEnsure{}
	w := newVoiceTicketWorld(t, fake)
	w.srv.voiceCoLoadTimeout = 150 * time.Millisecond
	chatID := w.newChat(t, "usr_voice", `{}`)

	start := time.Now()
	rec := w.postBearer(voiceTicketBody(chatID, "voice-asr", "voice-llm", "voice-tts"))
	elapsed := time.Since(start)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.String(), voiceChatCoLoadFailed)
	requireNoTicket(t, rec)
	if elapsed > 10*time.Second {
		t.Fatalf("the request took %v, want the %v deadline to have ended it", elapsed, w.srv.voiceCoLoadTimeout)
	}
	if want := []string{"whisper_voice"}; !slices.Equal(fake.ensured, want) {
		t.Fatalf("ensured = %v, want the stalled first start only", fake.ensured)
	}
}

// voiceGateSlowEnsure is an agent whose ensure takes delay to start a child, then succeeds: a
// cold start that is slow but works.
type voiceGateSlowEnsure struct {
	voiceGateFake
	delay time.Duration
}

func (f *voiceGateSlowEnsure) EnsureRuntimeModel(ctx context.Context, target routing.Target) error {
	select {
	case <-time.After(f.delay):
	case <-ctx.Done():
		return ctx.Err()
	}
	return f.voiceGateFake.EnsureRuntimeModel(ctx, target)
}

// postOverTheWire posts body to the ticket endpoint of a real listener as the fixture's user
// token and returns what the CLIENT receives. A dropped connection is a fatal error here, not a
// status: that is exactly what a server-side timeout does to a request that outlives it.
func postOverTheWire(t *testing.T, ts *httptest.Server, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+voiceTicketPath, strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+voiceTicketBearer)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("the client got no answer, the connection was dropped before it: %v", err)
	}
	defer resp.Body.Close()
	var buf strings.Builder
	if _, err := io.Copy(&buf, resp.Body); err != nil {
		t.Fatalf("reading the answer: %v", err)
	}
	return resp.StatusCode, buf.String()
}

// serveWithShortTimeouts serves the world over a real listener whose read and write timeouts are
// timeout, standing in for newHTTPServer's 30 s control-plane deadlines (armed when the headers
// are read) at a scale a test can outlast. httptest.ResponseRecorder has no deadline at all, so
// only a real connection can show what the server's timeouts do to a long request.
func (w *voiceTicketWorld) serveWithShortTimeouts(t *testing.T, timeout time.Duration) *httptest.Server {
	t.Helper()
	ts := httptest.NewUnstartedServer(w.srv)
	ts.Config.ReadTimeout = timeout
	ts.Config.WriteTimeout = timeout
	ts.Start()
	t.Cleanup(ts.Close)
	return ts
}

// The gate may hold the request for a whole cold start, longer than the server's read and write
// timeouts (30 s in newHTTPServer, armed at header read). A cold start slower than both must
// still end in the ticket on the wire: the handler lifts the deadlines before the gate. Here the
// timeouts are 150 ms and the two agent-managed starts take 2 x 300 ms.
func TestVoiceTicketEndpointOutlivesTheServerTimeouts(t *testing.T) {
	fake := &voiceGateSlowEnsure{delay: 300 * time.Millisecond}
	w := newVoiceTicketWorld(t, fake)
	ts := w.serveWithShortTimeouts(t, 150*time.Millisecond)
	chatID := w.newChat(t, "usr_voice", `{}`)

	start := time.Now()
	status, body := postOverTheWire(t, ts, voiceTicketBody(chatID, "voice-asr", "voice-llm", "voice-tts"))
	elapsed := time.Since(start)

	if elapsed < 2*150*time.Millisecond {
		t.Fatalf("the request took %v, want it to have outlived the server's 150 ms timeouts", elapsed)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", status, body)
	}
	var parsed struct {
		Ticket string `json:"ticket"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil || parsed.Ticket == "" {
		t.Fatalf("response carries no ticket (%v): %s", err, body)
	}
	claims, err := w.srv.voiceTickets.Redeem(parsed.Ticket)
	if err != nil {
		t.Fatalf("the ticket does not redeem: %v", err)
	}
	if claims.ChatID != chatID || claims.ASR.ProviderModel != "whisper_voice" {
		t.Fatalf("claims = chat %q, asr %+v", claims.ChatID, claims.ASR)
	}
}

// The refusal of a gate that runs out of its own deadline is delivered the same way: the 409
// reaches the client although the server's timeouts expired long before the gate's deadline.
func TestVoiceTicketEndpointDeliversADeadlineRefusalPastTheServerTimeouts(t *testing.T) {
	fake := &voiceGateStalledEnsure{}
	w := newVoiceTicketWorld(t, fake)
	w.srv.voiceCoLoadTimeout = 500 * time.Millisecond
	ts := w.serveWithShortTimeouts(t, 150*time.Millisecond)
	chatID := w.newChat(t, "usr_voice", `{}`)

	status, body := postOverTheWire(t, ts, voiceTicketBody(chatID, "voice-asr", "voice-llm", "voice-tts"))

	if status != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", status, body)
	}
	requireErrorCode(t, body, voiceChatCoLoadFailed)
}

// voiceStalledCapabilities is a routing store whose verdict read answers nothing until its
// context ends: the slow read a deadline can catch a leg in while it is still being resolved,
// before anything is started.
type voiceStalledCapabilities struct{ routing.Store }

func (voiceStalledCapabilities) MappingCapabilities(ctx context.Context, _ string) ([]routing.CapabilityRow, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// A deadline that fires while a leg is still being RESOLVED (not started) is the same
// co-load refusal: the caller sees one answer for "the gate did not finish in time", whichever
// step it was in when the time ran out. Left to the gate alone, the stalled verdict read here
// is a streaming ASR "that does not stream" and answers asr_unavailable.
func TestVoiceTicketEndpointMapsADeadlineDuringResolutionToCoLoadFailed(t *testing.T) {
	fake := &voiceGateFake{}
	w := newVoiceTicketWorld(t, fake)
	w.srv.Routes = voiceStalledCapabilities{Store: w.routes}
	w.srv.voiceCoLoadTimeout = 150 * time.Millisecond
	chatID := w.newChat(t, "usr_voice", `{}`)

	rec := w.postBearer(voiceTicketBody(chatID, "voice-asr", "voice-llm", "voice-tts"))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.String(), voiceChatCoLoadFailed)
	requireNoTicket(t, rec)
	requireNothingStarted(t, fake)
}

// A server with no ticket store (a bare Server that never went through New) fails closed with
// a 500 rather than panicking -- and before the gate, so it has not warmed three models for a
// ticket it cannot issue.
func TestVoiceTicketEndpointWithoutAStoreFailsClosed(t *testing.T) {
	fake := &voiceGateFake{}
	w := newVoiceTicketWorld(t, fake)
	w.srv.voiceTickets = nil
	chatID := w.newChat(t, "usr_voice", `{}`)

	rec := w.postBearer(voiceTicketBody(chatID, "voice-asr", "voice-llm", "voice-tts"))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.String(), voiceChatTicketFailed)
	requireNoTicket(t, rec)
	requireNothingStarted(t, fake)
}

// The same fail-closed answer for a server that has no portal service to read the chat from.
func TestVoiceTicketEndpointWithoutAPortalFailsClosed(t *testing.T) {
	fake := &voiceGateFake{}
	w := newVoiceTicketWorld(t, fake)
	w.srv.Portal = nil

	rec := w.postBearer(voiceTicketBody("chat_x", "voice-asr", "voice-llm", "voice-tts"))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.String(), voiceChatTicketFailed)
	requireNoTicket(t, rec)
	requireNothingStarted(t, fake)
}

// A ticket that would not fit a proxy's header line is never handed out, whatever made it
// big: here a mapping whose upstream name is pathologically long, which no limit on the
// chat's own settings can prevent. The mint is refused 500 (the operator's to fix, and
// logged), instead of a ticket the WebSocket upgrade could never present.
func TestVoiceTicketEndpointRefusesATicketTooLargeForAProxyHeader(t *testing.T) {
	w := newVoiceTicketWorld(t, &voiceGateFake{})
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	if err := w.routes.UpdateMapping(context.Background(), routing.ModelMapping{ID: "route-voice-asr", ApplicationID: "app-voice-agent", GatewayModelName: "voice-asr", AppModelName: strings.Repeat("m", 3*voiceTicketMaxBytes), Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("UpdateMapping: %v", err)
	}
	chatID := w.newChat(t, "usr_voice", `{}`)

	rec := w.postBearer(voiceTicketBody(chatID, "voice-asr", "voice-llm", "voice-tts"))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", rec.Code, rec.Body.String())
	}
	requireErrorCode(t, rec.Body.String(), voiceChatTicketFailed)
	requireNoTicket(t, rec)
}

// New builds the store the endpoint mints on, so a real server has one, and the gate deadline
// defaults to the documented 120 s unless a deployment sets one.
func TestNewServerWiresAVoiceTicketStoreAndTheCoLoadDeadline(t *testing.T) {
	srv := NewTestServer()
	if srv.voiceTickets == nil {
		t.Fatal("New left voiceTickets nil, want a store")
	}
	if srv.voiceCoLoadTimeout != 120*time.Second || DefaultVoiceCoLoadTimeout != 120*time.Second {
		t.Fatalf("voiceCoLoadTimeout = %v (default %v), want 120s", srv.voiceCoLoadTimeout, DefaultVoiceCoLoadTimeout)
	}
	if set := New(ServerDeps{VoiceCoLoadTimeout: 5 * time.Minute}); set.voiceCoLoadTimeout != 5*time.Minute {
		t.Fatalf("voiceCoLoadTimeout = %v, want the deployment's 5m", set.voiceCoLoadTimeout)
	}
	if neg := New(ServerDeps{VoiceCoLoadTimeout: -time.Second}); neg.voiceCoLoadTimeout != DefaultVoiceCoLoadTimeout {
		t.Fatalf("voiceCoLoadTimeout = %v, want the default for a non-positive value", neg.voiceCoLoadTimeout)
	}
}
