// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The voice-chat gate and the live-transcription ticket mint only select: they
// answer a browser POST, and the requests that will use the models come later,
// per turn. So they never wait in the admission queue -- with every instance busy
// they take the least loaded one and signal the demand for another -- and the gate
// warms the instance it selected, moving once to a sibling when the agent says that
// instance is gone. These tests drive the real Server and resolver over the
// instance fixture (live_instances_test.go), with the voice gate's fake agent as
// the provider for the ensures and the residency read.

// countingAdmission wraps the server's admission queue and counts the waits. stacks
// keeps the goroutine stack each wait began on, so a test can tell which caller parked.
type countingAdmission struct {
	inner  routing.AdmissionController
	mu     sync.Mutex
	waits  int
	stacks []string
}

func (a *countingAdmission) WaitForSlot(ctx context.Context, serverIDs []string, timeout time.Duration) error {
	a.mu.Lock()
	a.waits++
	a.stacks = append(a.stacks, string(debug.Stack()))
	a.mu.Unlock()
	return a.inner.WaitForSlot(ctx, serverIDs, timeout)
}

// waitedIn reports whether a wait began inside a call of fn (a function name as a
// stack prints it, e.g. "gateway.(*Server).relaySpeech(").
func (a *countingAdmission) waitedIn(fn string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.ContainsFunc(a.stacks, func(s string) bool { return strings.Contains(s, fn) })
}

func (a *countingAdmission) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.waits
}

// voiceInstanceWorld is the instance fixture set up for the voice gate: asr-inst,
// chat-inst and tts-inst replicable on one agent, tts-inst streaming its speech,
// the gate's fake agent as the provider, a recording demand sink and an
// admission queue whose waits are counted.
type voiceInstanceWorld struct {
	srv   *Server
	agent *voiceGateFake
	sink  *recordingDemandSink
	adm   *countingAdmission
	token auth.Token
}

func newVoiceInstanceWorld(t *testing.T) *voiceInstanceWorld {
	t.Helper()
	srv := newInstanceRouterServer(t, newInstanceAgentRouter(t), true)
	routes, ok := srv.Routes.(*routing.MemoryStore)
	if !ok {
		t.Fatalf("routes are %T, want the memory store", srv.Routes)
	}
	now := time.Now().UTC()
	if err := routes.UpsertMappingCapabilities(context.Background(), liveInstTTSMap, []routing.CapabilityRow{
		{Capability: routing.CapabilitySpeech, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceManual, CheckedAt: now},
		{Capability: routing.CapabilitySpeechStreaming, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceManual, CheckedAt: now},
	}); err != nil {
		t.Fatalf("UpsertMappingCapabilities tts: %v", err)
	}
	agent := &voiceGateFake{ensureErr: map[string]error{}}
	srv.Provider = agent
	sink := &recordingDemandSink{}
	srv.Resolver.SetDemandSink(sink)
	adm := &countingAdmission{inner: srv.Active.admission}
	srv.Resolver.SetAdmissionController(adm)
	return &voiceInstanceWorld{srv: srv, agent: agent, sink: sink, adm: adm, token: auth.Token{ID: "tok_dev", UserID: "usr_dev", Active: true}}
}

// publish reports asr-inst's instances asr and tts-inst's instances tts running
// (chat-inst runs its instance 0 only).
func (w *voiceInstanceWorld) publish(asr, tts []int) {
	var rows []RuntimeStatusDTO
	for _, k := range asr {
		rows = append(rows, liveInstanceRow(liveInstASRSpec, liveInstASRUp, k, 0))
	}
	for _, k := range tts {
		rows = append(rows, liveInstanceRow(liveInstTTSSpec, liveInstTTSUp, k, 0))
	}
	w.srv.RuntimeStatus.publish(liveInstServer, rows)
}

// busy puts n requests in flight on the gateway instance key.
func (w *voiceInstanceWorld) busy(t *testing.T, instanceKey string, n int) {
	t.Helper()
	for i := range n {
		id := fmt.Sprintf("busy-%s-%d", instanceKey, i)
		w.srv.Active.Add(ActiveRequest{ID: id, ServerID: liveInstServer, InstanceKey: instanceKey})
		t.Cleanup(func() { w.srv.Active.Remove(id) })
	}
}

// gate runs the voice-chat gate for the fixture's three models under a context
// that ends a gate which waits, so a regression fails instead of hanging. Whether
// it parked is read off the admission queue's wait count (w.adm), not off a clock.
func (w *voiceInstanceWorld) gate(t *testing.T) (asr, llm, tts routing.Target, code string, err error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return w.srv.voiceCoLoadGate(ctx, &w.token, "asr-inst", "chat-inst", "tts-inst")
}

func (w *voiceInstanceWorld) ensured() []string {
	w.agent.mu.Lock()
	defer w.agent.mu.Unlock()
	return slices.Clone(w.agent.ensured)
}

// Every instance of a leg busy: the gate answers at once with a target on the
// least loaded instance, never waits in the admission queue, and signals the
// demand for another instance of that leg's model once.
func TestVoiceGateNeverParks(t *testing.T) {
	for _, leg := range []struct {
		name, model, mapping string
		pick                 func(asr, tts routing.Target) routing.Target
	}{
		{"asr", "asr-inst", liveInstASRMap, func(asr, _ routing.Target) routing.Target { return asr }},
		{"tts", "tts-inst", liveInstTTSMap, func(_, tts routing.Target) routing.Target { return tts }},
	} {
		t.Run(leg.name, func(t *testing.T) {
			w := newVoiceInstanceWorld(t)
			w.publish([]int{0, 1}, []int{0, 1})
			w.busy(t, leg.mapping+"#0", 2)
			w.busy(t, leg.mapping+"#1", 1)

			asr, _, tts, code, err := w.gate(t)
			if err != nil || code != "" {
				t.Fatalf("gate = %q, %v; want it passed", code, err)
			}
			if waits := w.adm.count(); waits != 0 {
				t.Fatalf("admission waits = %d, want none", waits)
			}
			if got := leg.pick(asr, tts).InstanceKey; got != leg.mapping+"#1" {
				t.Fatalf("%s target = %q, want %s#1, the least loaded instance", leg.name, got, leg.mapping)
			}
			signals := w.sink.signals()
			if len(signals) != 1 || signals[0].Model != leg.model || !slices.Equal(signals[0].MappingIDs, []string{leg.mapping}) {
				t.Fatalf("demand signals = %+v, want one for %s naming %s", signals, leg.model, leg.mapping)
			}
		})
	}
}

// The gate warms the instance it selected -- m#1 when instance 0 is busy -- not
// the mapping's instance 0, and proves that instance resident.
func TestVoiceGateEnsuresSelectedInstance(t *testing.T) {
	w := newVoiceInstanceWorld(t)
	w.publish([]int{0, 1}, []int{0, 1})
	w.busy(t, liveInstASRMap+"#0", 1)
	w.busy(t, liveInstTTSMap+"#0", 1)

	asr, _, tts, code, err := w.gate(t)
	if err != nil || code != "" {
		t.Fatalf("gate = %q, %v; want it passed", code, err)
	}
	if asr.InstanceKey != liveInstASRMap+"#1" || tts.InstanceKey != liveInstTTSMap+"#1" {
		t.Fatalf("targets = %q, %q; want the free instances %s#1 and %s#1", asr.InstanceKey, tts.InstanceKey, liveInstASRMap, liveInstTTSMap)
	}
	want := []string{liveInstASRUp + "#1", liveInstChatUp, liveInstTTSUp + "#1"}
	if got := w.ensured(); !slices.Equal(got, want) {
		t.Fatalf("ensured = %v, want %v: each leg's selected instance", got, want)
	}
	if signals := w.sink.signals(); len(signals) != 0 {
		t.Fatalf("demand signals = %+v, want none (a free instance was selected)", signals)
	}
}

// The agent refusing the selected instance k >= 1 as gone (a 409
// runtime.instance_* or a 404) moves the leg once, to a sibling, which the gate
// warms instead; a second gone instance, or a refusal of instance 0, is the leg's
// own unavailable answer as before.
func TestVoiceGateReselectsVanishedInstance(t *testing.T) {
	gone := map[string]error{
		"409 instance_no_capacity":    provider.RouterStatusError(http.StatusConflict, []byte(`{"error":{"code":"runtime.instance_no_capacity","message":"gone"}}`)),
		"409 instance_demand_unknown": provider.RouterStatusError(http.StatusConflict, []byte(`{"error":{"code":"runtime.instance_demand_unknown","message":"gone"}}`)),
		"404 model_not_managed":       provider.RouterStatusError(http.StatusNotFound, []byte(`{"error":{"code":"runtime.model_not_managed","message":"gone"}}`)),
	}
	for name, refusal := range gone {
		t.Run(name, func(t *testing.T) {
			w := newVoiceInstanceWorld(t)
			w.publish([]int{0, 1}, []int{0})
			w.busy(t, liveInstASRMap+"#0", 1)
			w.agent.ensureErr[liveInstASRUp+"#1"] = refusal

			asr, _, _, code, err := w.gate(t)
			if err != nil || code != "" {
				t.Fatalf("gate = %q, %v; want it passed on the sibling", code, err)
			}
			if asr.InstanceKey != liveInstASRMap+"#0" {
				t.Fatalf("asr target = %q, want %s#0, the sibling of the gone instance", asr.InstanceKey, liveInstASRMap)
			}
			if got := w.ensured(); len(got) < 2 || !slices.Equal(got[:2], []string{liveInstASRUp + "#1", liveInstASRUp}) {
				t.Fatalf("ensured = %v, want the gone instance, then its sibling", got)
			}
		})
	}
	t.Run("the tts leg", func(t *testing.T) {
		w := newVoiceInstanceWorld(t)
		w.publish([]int{0}, []int{0, 1})
		w.busy(t, liveInstTTSMap+"#0", 1)
		w.agent.ensureErr[liveInstTTSUp+"#1"] = gone["409 instance_no_capacity"]

		_, _, tts, code, err := w.gate(t)
		if err != nil || code != "" {
			t.Fatalf("gate = %q, %v; want it passed on the sibling", code, err)
		}
		if tts.InstanceKey != liveInstTTSMap+"#0" {
			t.Fatalf("tts target = %q, want %s#0, the sibling of the gone instance", tts.InstanceKey, liveInstTTSMap)
		}
		want := []string{liveInstASRUp, liveInstChatUp, liveInstTTSUp + "#1", liveInstTTSUp}
		if got := w.ensured(); !slices.Equal(got, want) {
			t.Fatalf("ensured = %v, want %v: the gone tts instance, then its sibling", got, want)
		}
	})
	t.Run("only once", func(t *testing.T) {
		w := newVoiceInstanceWorld(t)
		w.publish([]int{0, 1, 2}, []int{0})
		w.busy(t, liveInstASRMap+"#0", 1)
		w.agent.ensureErr[liveInstASRUp+"#1"] = gone["409 instance_no_capacity"]
		w.agent.ensureErr[liveInstASRUp+"#2"] = gone["409 instance_no_capacity"]

		asr, llm, tts, code, err := w.gate(t)
		requireVoiceGateRefused(t, asr, llm, tts, code, err, voiceChatASRUnavailable)
		if got := w.ensured(); len(got) != 2 {
			t.Fatalf("ensured = %v, want two attempts: the selection and its one re-selection", got)
		}
	})
	t.Run("instance 0 is not re-selected", func(t *testing.T) {
		w := newVoiceInstanceWorld(t)
		w.publish([]int{0, 1}, []int{0})
		w.busy(t, liveInstASRMap+"#1", 1)
		w.agent.ensureErr[liveInstASRUp] = gone["404 model_not_managed"]

		asr, llm, tts, code, err := w.gate(t)
		requireVoiceGateRefused(t, asr, llm, tts, code, err, voiceChatASRUnavailable)
		if got := w.ensured(); !slices.Equal(got, []string{liveInstASRUp}) {
			t.Fatalf("ensured = %v, want instance 0 only", got)
		}
	})
	t.Run("admission blocked stays a co-load failure", func(t *testing.T) {
		w := newVoiceInstanceWorld(t)
		w.publish([]int{0, 1}, []int{0})
		w.busy(t, liveInstASRMap+"#0", 1)
		w.agent.ensureErr[liveInstASRUp+"#1"] = provider.RouterStatusError(http.StatusServiceUnavailable, []byte(`{"error":{"code":"runtime.admission_blocked","message":"no room"}}`))

		asr, llm, tts, code, err := w.gate(t)
		requireVoiceGateRefused(t, asr, llm, tts, code, err, voiceChatCoLoadFailed)
		if got := w.ensured(); !slices.Equal(got, []string{liveInstASRUp + "#1"}) {
			t.Fatalf("ensured = %v, want the one refused instance and no re-selection", got)
		}
	})
}

// The live-transcription ticket mint with every instance busy answers at once:
// it never waits in the admission queue, and signals the demand for another
// instance once. The session parks later, at the relay, if it has to.
func TestLiveTicketMintNeverParks(t *testing.T) {
	w := newVoiceInstanceWorld(t)
	w.publish([]int{0, 1}, nil)
	w.busy(t, liveInstASRMap+"#0", 2)
	w.busy(t, liveInstASRMap+"#1", 1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	body := `{"model":"asr-inst","sample_rate":16000,"channels":1,"sample_format":"s16le"}`
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/portal/transcription-live-ticket", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer dev-secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	w.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("mint status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if waits := w.adm.count(); waits != 0 {
		t.Fatalf("admission waits = %d, want none", waits)
	}
	signals := w.sink.signals()
	if len(signals) != 1 || signals[0].Model != "asr-inst" || !slices.Equal(signals[0].MappingIDs, []string{liveInstASRMap}) {
		t.Fatalf("demand signals = %+v, want one for asr-inst naming %s", signals, liveInstASRMap)
	}
}

// A run synthesizes its speech sections one at a time when its TTS target is an
// instance of a replicable spec, so its own second section never looks like demand
// for another instance. Every other target keeps the configured concurrency.
func TestVoiceTTSConcurrencyOneForInstances(t *testing.T) {
	cases := []struct {
		name   string
		target routing.Target
		want   int
	}{
		{"an instance k >= 1 of a replicable spec", routing.Target{InstanceKey: "route-tts#1", InstanceCapable: true}, 1},
		{"instance 0 of a replicable spec", routing.Target{InstanceKey: "route-tts#0", InstanceCapable: true}, 1},
		{"a mapping of a spec that is not replicable", routing.Target{InstanceKey: "route-tts#0"}, 2},
		{"a target that addresses no instance", routing.Target{}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := voiceTTSConcurrency(tc.target, 2); got != tc.want {
				t.Fatalf("voiceTTSConcurrency = %d, want %d", got, tc.want)
			}
		})
	}
}

// A run whose TTS target is an instance of a replicable spec keeps one synthesis
// in flight at a time, though the configured cap is two: a burst of sections is
// spoken one after another.
func TestVoiceRunOnATTSInstanceSynthesizesOneSectionAtATime(t *testing.T) {
	script, _ := burstScript()
	prov := defaultVoiceProvider()
	prov.llmScript = script
	prov.speechBodies = make(chan []byte, 2*burstSections)
	prov.speechRelease = make(chan struct{})
	prov.speechInflight, prov.speechPeak = new(atomic.Int32), new(atomic.Int32)
	srv, owner, chatID, _, _ := newVoiceRunTestServerProv(t, prov)
	srv.voiceTiming = voiceTiming{ttsConcurrency: 2}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(prov.speechRelease) }) }
	t.Cleanup(release)

	tts := voiceTTSTarget
	tts.InstanceKey, tts.InstanceCapable = "route-tts#1", true
	history, err := srv.Portal.VoiceRunHistory(context.Background(), owner, chatID, "unused")
	if err != nil {
		t.Fatalf("VoiceRunHistory: %v", err)
	}
	run, runCtx, err := srv.reserveRun(owner, chatID)
	if err != nil {
		t.Fatalf("reserveRun: %v", err)
	}
	_, ch, unsub := run.subscribe()
	t.Cleanup(unsub)
	srv.launchRun(runCtx, owner, run, PrepareRunResult{
		History:  history,
		Settings: portal.ChatRunSettings{Model: voiceLLMTarget.Model, Kind: chatRunKindVoice},
		Voice:    &voiceRunParams{pendingText: "unused", tts: tts},
	})

	if !voicePoll(10*time.Second, func() bool { return prov.speechInflight.Load() >= 1 }) {
		t.Fatal("no synthesis reached the TTS")
	}
	if voicePoll(150*time.Millisecond, func() bool { return prov.speechInflight.Load() > 1 }) {
		t.Fatalf("%d syntheses in flight at once on one TTS instance, want one", prov.speechInflight.Load())
	}
	release()
	drainRunAudio(t, ch)
	if got := run.statusValue(); got != "completed" {
		t.Fatalf("status = %q, want completed", got)
	}
	if peak := prov.speechPeak.Load(); peak != 1 {
		t.Fatalf("peak concurrent syntheses = %d, want 1", peak)
	}
}

// The branches of a leg's re-selection off a gone instance that the gate tests
// above do not reach: a re-selection routing refuses ends the leg with the gone
// instance's own error, a re-selected direct target is not ensured, and a caller
// gone mid-start re-selects nothing.
func TestVoiceGateReselectionEdges(t *testing.T) {
	goneErr := provider.RouterStatusError(http.StatusConflict, []byte(`{"error":{"code":"runtime.instance_no_capacity","message":"gone"}}`))
	gone := routing.Target{Provider: routing.ProviderServerAgent, RouteID: liveInstASRMap, ServerID: liveInstServer, ProviderModel: liveInstASRUp + "#1", InstanceKey: liveInstASRMap + "#1"}
	newLeg := func(reselect func(routing.Target) (routing.Target, error)) *voiceGateLeg {
		return &voiceGateLeg{leg: voiceLegASR, target: gone, unavailable: voiceChatASRUnavailable, reselect: reselect}
	}

	t.Run("a refused re-selection answers the gone error", func(t *testing.T) {
		w := newVoiceInstanceWorld(t)
		w.agent.ensureErr[gone.ProviderModel] = goneErr
		leg := newLeg(func(routing.Target) (routing.Target, error) { return routing.Target{}, routing.ErrNoHealthyHost })
		code, err := w.srv.ensureVoiceLeg(context.Background(), w.agent, leg)
		if code != voiceChatASRUnavailable || !errors.Is(err, goneErr) || errors.Is(err, routing.ErrNoHealthyHost) {
			t.Fatalf("ensureVoiceLeg = %q, %v; want the leg unavailable with the gone instance's error", code, err)
		}
		if leg.target.InstanceKey != gone.InstanceKey {
			t.Fatalf("target = %+v, want the gone instance kept: nothing else was selected", leg.target)
		}
	})

	t.Run("a direct re-selected target is not ensured", func(t *testing.T) {
		w := newVoiceInstanceWorld(t)
		w.agent.ensureErr[gone.ProviderModel] = goneErr
		direct := routing.Target{Provider: routing.ProviderAudioCpp, RouteID: "route-direct", ServerID: "srv-direct", ProviderModel: "asr-direct"}
		leg := newLeg(func(routing.Target) (routing.Target, error) { return direct, nil })
		if code, err := w.srv.ensureVoiceLeg(context.Background(), w.agent, leg); code != "" || err != nil {
			t.Fatalf("ensureVoiceLeg = %q, %v; want it passed on the direct target", code, err)
		}
		if leg.target.ProviderModel != direct.ProviderModel || leg.target.RouteID != direct.RouteID {
			t.Fatalf("target = %+v, want the re-selected direct target", leg.target)
		}
		if got := w.ensured(); !slices.Equal(got, []string{gone.ProviderModel}) {
			t.Fatalf("ensured = %v, want only the gone instance: a direct target has no ensure route", got)
		}
	})

	t.Run("a cancelled caller re-selects nothing", func(t *testing.T) {
		w := newVoiceInstanceWorld(t)
		w.agent.ensureErr[gone.ProviderModel] = goneErr
		reselected := false
		leg := newLeg(func(routing.Target) (routing.Target, error) { reselected = true; return routing.Target{}, nil })
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		code, err := w.srv.ensureVoiceLeg(ctx, w.agent, leg)
		if code != voiceChatCoLoadFailed || !errors.Is(err, context.Canceled) {
			t.Fatalf("ensureVoiceLeg = %q, %v; want a co-load failure carrying the cancellation", code, err)
		}
		if reselected {
			t.Fatal("the leg was re-selected for a caller that went away")
		}
	})
}

// The replay's synth pre-check with every TTS instance busy answers at once: it
// never waits in the admission queue, and signals no demand -- the relay after it
// parks, and signals, if it must.
func TestVoiceSynthPreCheckNeitherParksNorSignals(t *testing.T) {
	w := newVoiceInstanceWorld(t)
	w.publish(nil, []int{0, 1})
	w.busy(t, liveInstTTSMap+"#0", 1)
	w.busy(t, liveInstTTSMap+"#1", 1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/portal/voice-chat-synth", http.NoBody)
	if err := w.srv.checkVoiceTTS(r, &w.token, "tts-inst"); err != nil {
		t.Fatalf("checkVoiceTTS: %v, want the model accepted", err)
	}
	if waits := w.adm.count(); waits != 0 {
		t.Fatalf("admission waits = %d, want none", waits)
	}
	if signals := w.sink.signals(); len(signals) != 0 {
		t.Fatalf("demand signals = %+v, want none from a check", signals)
	}
}

// The replay's synth endpoint at capacity: its pre-check neither waits in the admission
// queue nor signals demand, and holds the model to no real-time threshold (a replay is
// not a conversation); only its own relay parks -- once, signalling the demand once --
// and is served when an instance frees.
func TestVoiceSynthEndpointAtCapacity(t *testing.T) {
	w := newVoiceInstanceWorld(t)
	w.srv.Provider = provider.NewOpenAICompatibleClient(http.DefaultClient) // the agent router serves the synthesis
	w.publish(nil, []int{0, 1})
	setVoiceAudioRate(t, w.srv, liveInstTTSMap, 0.5)
	w.busy(t, liveInstTTSMap+"#0", 1)
	w.srv.Active.Add(ActiveRequest{ID: "synth-busy-1", ServerID: liveInstServer, InstanceKey: liveInstTTSMap + "#1"})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/portal/voice-chat-synth", bytes.NewBufferString(`{"text":"say it again","tts_model":"tts-inst"}`))
		r.Header.Set("Authorization", "Bearer dev-secret")
		r.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		w.srv.ServeHTTP(rec, r)
		done <- rec
	}()
	// The relay parks: the one wait, with the one demand signal its park sends.
	if !voicePoll(5*time.Second, func() bool { return w.adm.count() == 1 }) {
		t.Fatalf("admission waits = %d, want the relay's one", w.adm.count())
	}
	if !w.adm.waitedIn("gateway.(*Server).relaySpeech(") || w.adm.waitedIn("gateway.(*Server).checkVoiceTTS(") {
		t.Fatal("the wait was not the relay's: the pre-check must never park")
	}
	if signals := w.sink.signals(); len(signals) != 1 || signals[0].Model != "tts-inst" {
		t.Fatalf("demand signals = %+v, want the relay's one park signal for tts-inst", signals)
	}
	w.srv.Active.Remove("synth-busy-1")

	var rec *httptest.ResponseRecorder
	select {
	case rec = <-done:
	case <-ctx.Done():
		t.Fatal("the synth request was not served once an instance freed")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("synth = %d %s, want 200: a replay is not held to real time", rec.Code, rec.Body.String())
	}
	if waits, signals := w.adm.count(), w.sink.signals(); waits != 1 || len(signals) != 1 {
		t.Fatalf("admission waits %d, demand signals %+v; want only the relay's one each", waits, signals)
	}
}
