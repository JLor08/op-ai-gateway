// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"context"
	"op-ai-gateway/internal/gateway"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"slices"
	"sync"
	"testing"
	"time"
)

// audioBenchmarkRecorder records every automatic audio benchmark the health loop asked for
// (by mapping id) and answers answers[id], or answer for a mapping not in answers (its zero
// value is gateway.AudioAutoRunDeferred: nothing started, ask again).
type audioBenchmarkRecorder struct {
	mu      sync.Mutex
	asked   []string
	answer  gateway.AudioAutoRunAnswer
	answers map[string]gateway.AudioAutoRunAnswer
}

func (r *audioBenchmarkRecorder) trigger(_ context.Context, _ routing.AIServer, _ routing.Application, mp routing.ModelMapping) gateway.AudioAutoRunAnswer {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asked = append(r.asked, mp.ID)
	if a, ok := r.answers[mp.ID]; ok {
		return a
	}
	return r.answer
}

// calls returns the mappings asked about since the last call, and forgets them.
func (r *audioBenchmarkRecorder) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.asked
	r.asked = nil
	return out
}

// TestHealthAsksForTheAutomaticAudioBenchmark: on every cycle the health loop asks the
// gateway's one-time automatic audio benchmark for the speech and transcription mappings of
// a reachable audio application that still lack what it measures, by the gateway's own rule
// (gateway.AudioAutoRunWanted) -- no audio metrics yet, or a replicable spec without a ramp
// result -- and for nothing else: not a mapping without an audio verdict, one with a ramp
// result, one with metrics whose spec is not replicable (an operator-run audio.cpp server
// has no spec at all), a locked one, an inactive one, or one of an unreachable application.
// It stops asking on a server once a run was started there: one run holds a server at a
// time.
func TestHealthAsksForTheAutomaticAudioBenchmark(t *testing.T) {
	stamp := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	speech := provider.AudioCppVerdicts{Speech: routing.CapabilityYes, Transcription: routing.CapabilityNo, Loaded: true}
	asr := provider.AudioCppVerdicts{Speech: routing.CapabilityNo, Transcription: routing.CapabilityYes}
	mapping := func(id, model string, edit func(*routing.ModelMapping)) routing.ModelMapping {
		mp := audioCppMapping(id, model)
		if edit != nil {
			edit(&mp)
		}
		return mp
	}
	newRunner := func(t *testing.T) (*appHealthRunner, *fakeHealthStore, *audioCppFakeProber, *audioBenchmarkRecorder) {
		t.Helper()
		runner, st, prober := audioCppProbeRunner(t,
			map[string]provider.AudioCppVerdicts{"tts": speech, "asr": asr, "done": speech, "partial": speech, "locked": speech, "off": speech, "music": {}},
			mapping("m_none", "music", nil),
			mapping("m_tts", "tts", nil),
			mapping("m_asr", "asr", nil),
			mapping("m_done", "done", func(mp *routing.ModelMapping) {
				mp.AudioMetricsUpdatedAt, mp.AudioConcurrencyLimit = &stamp, routing.AudioLimitPerformance
			}),
			mapping("m_partial", "partial", func(mp *routing.ModelMapping) { mp.AudioMetricsUpdatedAt = &stamp }),
			mapping("m_locked", "locked", func(mp *routing.ModelMapping) { mp.MetricsLocked = true }),
			mapping("m_off", "off", func(mp *routing.ModelMapping) { mp.Status = routing.ServerStatusDisabled }),
		)
		// An audio.cpp server the gateway routes audio to, and nothing else.
		st.apps["s1"][0].APIFlavors = []string{routing.APIFlavorOpenAIAudio}
		rec := &audioBenchmarkRecorder{}
		runner.audioBenchmark = rec.trigger
		return runner, st, prober, rec
	}

	t.Run("every candidate is asked while none starts", func(t *testing.T) {
		runner, _, _, rec := newRunner(t)
		runCapabilityCycle(runner)
		if got, want := rec.calls(), []string{"m_tts", "m_asr"}; !slices.Equal(got, want) {
			t.Fatalf("asked = %v, want %v", got, want)
		}
	})
	t.Run("a started run ends the cycle's asking on the server", func(t *testing.T) {
		runner, _, _, rec := newRunner(t)
		rec.answer = gateway.AudioAutoRunStarted
		runCapabilityCycle(runner)
		if got := rec.calls(); !slices.Equal(got, []string{"m_tts"}) {
			t.Fatalf("asked = %v, want only m_tts", got)
		}
	})
	t.Run("an unreachable application is not asked about", func(t *testing.T) {
		runner, _, prober, rec := newRunner(t)
		prober.down["http://s1.local:8001"] = true
		runCapabilityCycle(runner)
		if got := rec.calls(); len(got) != 0 {
			t.Fatalf("asked = %v, want none", got)
		}
	})
	t.Run("a runner without the trigger asks nothing", func(t *testing.T) {
		runner, _, _, _ := newRunner(t)
		runner.audioBenchmark = nil
		runCapabilityCycle(runner) // must not panic
	})
}

// TestHealthAsksAboutAReplicableSpecWithoutARampResult: on a server_agent application the
// loop applies the gateway's rule with the mapping's launch spec. A mapping with audio
// metrics but no ramp result is asked about only when its spec is replicable -- the run
// would measure its real-time concurrency -- and not when it is not, where the gateway
// would refuse it on every tick; a mapping with no audio metrics is asked about whatever
// its spec. The launch specs are read once per application and cycle, and not at all when
// no mapping can still lack anything.
func TestHealthAsksAboutAReplicableSpecWithoutARampResult(t *testing.T) {
	stamp := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	partial := func(id string) routing.ModelMapping {
		mp := routing.ModelMapping{ID: id, ApplicationID: "a1", GatewayModelName: "g-" + id, AppModelName: id, Status: routing.ServerStatusActive}
		mp.AudioMetricsUpdatedAt = &stamp
		return mp
	}
	audio := []string{routing.APIFlavorOpenAIAudio}
	newStore := func(mappings ...routing.ModelMapping) (*fakeHealthStore, *audioBenchmarkReads) {
		app := serverAgentApp("a1", "s1", 8001)
		app.APIFlavors = []string{routing.APIFlavorOpenAI, routing.APIFlavorOpenAIAudio}
		st := newHealthTestStore(app)
		st.mappings = map[string][]routing.ModelMapping{"a1": mappings}
		st.runtimeSpecs = []routing.RuntimeSpec{
			{ID: "rs_repl", MappingID: "m_repl", Enabled: true, APIFlavors: audio, Replicable: true, MinInstances: 1, MaxInstances: 4},
			{ID: "rs_flat", MappingID: "m_flat", Enabled: true, APIFlavors: audio},
			{ID: "rs_new", MappingID: "m_new", Enabled: true, APIFlavors: audio},
			{ID: "rs_done", MappingID: "m_done", Enabled: true, APIFlavors: audio, Replicable: true},
		}
		st.caps = map[string]map[string]routing.CapabilityRow{}
		for _, mp := range mappings {
			st.caps[mp.ID] = map[string]routing.CapabilityRow{routing.CapabilitySpeech: {Capability: routing.CapabilitySpeech, Verdict: routing.CapabilityYes}}
		}
		return st, &audioBenchmarkReads{fakeHealthStore: st}
	}
	ask := func(t *testing.T, mappings ...routing.ModelMapping) ([]string, int) {
		t.Helper()
		st, counter := newStore(mappings...)
		rec := &audioBenchmarkRecorder{}
		runner := &appHealthRunner{store: counter, audioBenchmark: rec.trigger}
		runner.startAudioBenchmarks(context.Background(), routing.AIServer{ID: "s1"}, st.apps["s1"], []bool{true})
		return rec.calls(), counter.specReads()
	}
	newMapping := routing.ModelMapping{ID: "m_new", ApplicationID: "a1", GatewayModelName: "g-m_new", AppModelName: "m_new", Status: routing.ServerStatusActive}
	if got, reads := ask(t, newMapping, partial("m_repl"), partial("m_flat")); !slices.Equal(got, []string{"m_new", "m_repl"}) || reads != 1 {
		t.Fatalf("asked = %v after %d spec reads, want [m_new m_repl] after one", got, reads)
	}
	done := partial("m_done")
	done.AudioConcurrencyLimit = routing.AudioLimitCap
	if got, reads := ask(t, done); len(got) != 0 || reads != 0 {
		t.Fatalf("asked = %v after %d spec reads, want nothing asked and no spec read for a mapping with a ramp result", got, reads)
	}
}

// TestHealthSkipsChatModelsForTheAutomaticAudioBenchmark: a mapping whose effective flavors
// serve a text flavor is a chat model and keeps its text benchmark, whatever audio verdict it
// has -- an omni model that also transcribes, on an agent child or a direct application -- so
// the loop never asks about it (the gateway's own rule, gateway.AudioTargetFlavors) and never
// reads its capability rows, on any cycle. Nor does it for a mapping the gateway routes no
// audio to. An audio-only child with a verdict is asked about.
func TestHealthSkipsChatModelsForTheAutomaticAudioBenchmark(t *testing.T) {
	agent := serverAgentApp("a1", "s1", 8001)
	agent.APIFlavors = []string{routing.APIFlavorOpenAI, routing.APIFlavorOpenAIAudio}
	direct := audioCppApp("a2", "s1", 8002)
	direct.APIFlavors = []string{routing.APIFlavorOpenAI, routing.APIFlavorOpenAIAudio}
	noAudio := audioCppApp("a3", "s1", 8003)
	noAudio.APIFlavors = []string{routing.APIFlavorOpenAIImages}
	st := newHealthTestStore(agent, direct, noAudio)
	mp := func(id, appID string) routing.ModelMapping {
		return routing.ModelMapping{ID: id, ApplicationID: appID, GatewayModelName: "g-" + id, AppModelName: id, Status: routing.ServerStatusActive}
	}
	st.mappings = map[string][]routing.ModelMapping{
		"a1": {mp("m_omni", "a1"), mp("m_chat", "a1"), mp("m_tts", "a1")},
		"a2": {mp("m_direct_omni", "a2")},
		"a3": {mp("m_images", "a3")},
	}
	st.runtimeSpecs = []routing.RuntimeSpec{
		{ID: "rs_omni", MappingID: "m_omni", Enabled: true, APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorOpenAIAudio}},
		{ID: "rs_chat", MappingID: "m_chat", Enabled: true, APIFlavors: []string{routing.APIFlavorOpenAI}},
		{ID: "rs_tts", MappingID: "m_tts", Enabled: true, APIFlavors: []string{routing.APIFlavorOpenAIAudio}},
	}
	transcribes := map[string]routing.CapabilityRow{routing.CapabilityTranscription: {Capability: routing.CapabilityTranscription, Verdict: routing.CapabilityYes}}
	st.caps = map[string]map[string]routing.CapabilityRow{
		"m_omni": transcribes, "m_direct_omni": transcribes, "m_images": transcribes,
		"m_tts": {routing.CapabilitySpeech: {Capability: routing.CapabilitySpeech, Verdict: routing.CapabilityYes}},
	}
	reads := &audioBenchmarkReads{fakeHealthStore: st}
	rec := &audioBenchmarkRecorder{}
	runner := &appHealthRunner{store: reads, audioBenchmark: rec.trigger}
	for cycle := 1; cycle <= 2; cycle++ {
		runner.startAudioBenchmarks(context.Background(), routing.AIServer{ID: "s1"}, st.apps["s1"], []bool{true, true, true})
		if got := rec.calls(); !slices.Equal(got, []string{"m_tts"}) {
			t.Fatalf("cycle %d: asked = %v, want only the audio-only child m_tts", cycle, got)
		}
	}
	if got := reads.capReadsOf(); !slices.Equal(got, []string{"m_tts", "m_tts"}) {
		t.Fatalf("capability reads = %v, want m_tts's alone, once per cycle", got)
	}
}

// TestHealthAsksAboutASettledMappingOnce: the gateway's answer decides whether the loop asks
// about a mapping again. A mapping whose run was started, or that the gateway refused for what
// the mapping is (gateway.AudioAutoRunSettled: a clone model without a preset voice, say), is
// asked about once: on the cycles after, nothing is read for it and it is not asked about, for
// as long as the process runs. A mapping the gateway deferred (another run, live traffic) is
// asked about on every cycle. The one fact of a settled refusal that changes without an
// operator is a clone model's presets, which reach the voices cache through this loop's own
// write: a CHANGED voices list for a settled mapping has it asked about again, an unchanged one
// does not.
func TestHealthAsksAboutASettledMappingOnce(t *testing.T) {
	app := serverAgentApp("a1", "s1", 8001)
	app.APIFlavors = []string{routing.APIFlavorOpenAIAudio}
	st := newHealthTestStore(app)
	mp := func(id string) routing.ModelMapping {
		return routing.ModelMapping{ID: id, ApplicationID: "a1", GatewayModelName: "g-" + id, AppModelName: id, Status: routing.ServerStatusActive}
	}
	st.mappings = map[string][]routing.ModelMapping{"a1": {mp("m_clone"), mp("m_busy"), mp("m_run")}}
	speech := map[string]routing.CapabilityRow{routing.CapabilitySpeech: {Capability: routing.CapabilitySpeech, Verdict: routing.CapabilityYes}}
	st.caps = map[string]map[string]routing.CapabilityRow{"m_clone": speech, "m_busy": speech, "m_run": speech}
	reads := &audioBenchmarkReads{fakeHealthStore: st}
	rec := &audioBenchmarkRecorder{answers: map[string]gateway.AudioAutoRunAnswer{
		"m_clone": gateway.AudioAutoRunSettled,
		"m_busy":  gateway.AudioAutoRunDeferred,
		"m_run":   gateway.AudioAutoRunStarted,
	}}
	runner := &appHealthRunner{store: reads, audioBenchmark: rec.trigger, now: time.Now}
	cycle := func(t *testing.T, want ...string) {
		t.Helper()
		reads.capReadsOf() // forget earlier reads
		runner.startAudioBenchmarks(context.Background(), routing.AIServer{ID: "s1"}, st.apps["s1"], []bool{true})
		if got := rec.calls(); !slices.Equal(got, want) {
			t.Fatalf("asked = %v, want %v", got, want)
		}
		if got := reads.capReadsOf(); !slices.Equal(got, want) {
			t.Fatalf("capability reads = %v, want %v (nothing read for a settled mapping)", got, want)
		}
	}

	cycle(t, "m_clone", "m_busy", "m_run")
	cycle(t, "m_busy")
	cycle(t, "m_busy")

	// A changed voices list (the first one cached: none) has the settled mapping asked about
	// again once; the gateway settles it again. The same list again changes nothing.
	if changed, err := runner.writeVoicesCache(context.Background(), "m_clone", nil); err != nil || !changed {
		t.Fatalf("first voices write = %v, %v; want a change", changed, err)
	}
	cycle(t, "m_clone", "m_busy")
	if changed, err := runner.writeVoicesCache(context.Background(), "m_clone", nil); err != nil || changed {
		t.Fatalf("second voices write = %v, %v; want no change", changed, err)
	}
	cycle(t, "m_busy")
	// Presets cached for the clone model: asked about again, once.
	if changed, err := runner.writeVoicesCache(context.Background(), "m_clone", []provider.Voice{{Name: "alba"}}); err != nil || !changed {
		t.Fatalf("presets write = %v, %v; want a change", changed, err)
	}
	rec.answers["m_clone"] = gateway.AudioAutoRunStarted
	cycle(t, "m_clone")
	cycle(t, "m_busy")
}

// audioBenchmarkReads counts what the automatic audio benchmark step reads of a
// fakeHealthStore: the launch-spec listings, and the capability rows by mapping.
type audioBenchmarkReads struct {
	*fakeHealthStore
	mu    sync.Mutex
	specs int
	caps  []string
}

func (c *audioBenchmarkReads) RuntimeSpecsByApplication(ctx context.Context, appID string) ([]routing.RuntimeSpec, error) {
	c.mu.Lock()
	c.specs++
	c.mu.Unlock()
	return c.fakeHealthStore.RuntimeSpecsByApplication(ctx, appID)
}

func (c *audioBenchmarkReads) MappingCapabilities(ctx context.Context, mappingID string) ([]routing.CapabilityRow, error) {
	c.mu.Lock()
	c.caps = append(c.caps, mappingID)
	c.mu.Unlock()
	return c.fakeHealthStore.MappingCapabilities(ctx, mappingID)
}

func (c *audioBenchmarkReads) specReads() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.specs
}

// capReadsOf returns the mappings whose capability rows were read since the last call, and
// forgets them.
func (c *audioBenchmarkReads) capReadsOf() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.caps
	c.caps = nil
	return out
}
