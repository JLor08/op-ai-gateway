// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"context"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"slices"
	"sync"
	"testing"
	"time"
)

// audioBenchmarkRecorder records every automatic audio benchmark the health loop asked for
// (by mapping id) and answers started unless told otherwise.
type audioBenchmarkRecorder struct {
	mu         sync.Mutex
	asked      []string
	notStarted bool
}

func (r *audioBenchmarkRecorder) trigger(_ context.Context, _ routing.AIServer, _ routing.Application, mp routing.ModelMapping) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asked = append(r.asked, mp.ID)
	return !r.notStarted
}

func (r *audioBenchmarkRecorder) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.asked...)
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
		rec := &audioBenchmarkRecorder{notStarted: true}
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
		rec.notStarted = false
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
// its spec. The launch specs are read only for a mapping the answer depends on, and once
// per application and cycle.
func TestHealthAsksAboutAReplicableSpecWithoutARampResult(t *testing.T) {
	stamp := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	partial := func(id string) routing.ModelMapping {
		mp := routing.ModelMapping{ID: id, ApplicationID: "a1", GatewayModelName: "g-" + id, AppModelName: id, Status: routing.ServerStatusActive}
		mp.AudioMetricsUpdatedAt = &stamp
		return mp
	}
	newStore := func(mappings ...routing.ModelMapping) (*fakeHealthStore, *specReadCounter) {
		st := newHealthTestStore(serverAgentApp("a1", "s1", 8001))
		st.mappings = map[string][]routing.ModelMapping{"a1": mappings}
		st.runtimeSpecs = []routing.RuntimeSpec{
			{ID: "rs_repl", MappingID: "m_repl", Enabled: true, Replicable: true, MinInstances: 1, MaxInstances: 4},
			{ID: "rs_flat", MappingID: "m_flat", Enabled: true},
			{ID: "rs_new", MappingID: "m_new", Enabled: true},
		}
		st.caps = map[string]map[string]routing.CapabilityRow{}
		for _, mp := range mappings {
			st.caps[mp.ID] = map[string]routing.CapabilityRow{routing.CapabilitySpeech: {Capability: routing.CapabilitySpeech, Verdict: routing.CapabilityYes}}
		}
		return st, &specReadCounter{fakeHealthStore: st}
	}
	ask := func(t *testing.T, mappings ...routing.ModelMapping) ([]string, int) {
		t.Helper()
		st, counter := newStore(mappings...)
		rec := &audioBenchmarkRecorder{notStarted: true}
		runner := &appHealthRunner{store: counter, audioBenchmark: rec.trigger}
		runner.startAudioBenchmarks(context.Background(), routing.AIServer{ID: "s1"}, st.apps["s1"], []bool{true})
		return rec.calls(), counter.reads()
	}
	newMapping := routing.ModelMapping{ID: "m_new", ApplicationID: "a1", GatewayModelName: "g-m_new", AppModelName: "m_new", Status: routing.ServerStatusActive}
	if got, reads := ask(t, newMapping, partial("m_repl"), partial("m_flat")); !slices.Equal(got, []string{"m_new", "m_repl"}) || reads != 1 {
		t.Fatalf("asked = %v after %d spec reads, want [m_new m_repl] after one", got, reads)
	}
	if got, reads := ask(t, newMapping); !slices.Equal(got, []string{"m_new"}) || reads != 0 {
		t.Fatalf("asked = %v after %d spec reads, want [m_new] with no spec read", got, reads)
	}
}

// specReadCounter counts the launch-spec reads of a fakeHealthStore.
type specReadCounter struct {
	*fakeHealthStore
	mu sync.Mutex
	n  int
}

func (c *specReadCounter) RuntimeSpecsByApplication(ctx context.Context, appID string) ([]routing.RuntimeSpec, error) {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	return c.fakeHealthStore.RuntimeSpecsByApplication(ctx, appID)
}

func (c *specReadCounter) reads() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}
