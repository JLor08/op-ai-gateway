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
// a reachable audio application that still lack what it measures -- no audio metrics yet,
// or no ramp result (whether that matters, a replicable spec, the gateway decides) -- and
// for nothing else: not a mapping without an audio verdict, one with a ramp result, a
// locked one, an inactive one, or one of an unreachable application. It stops asking on a
// server once a run was started there: one run holds a server at a time.
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
		if got, want := rec.calls(), []string{"m_tts", "m_asr", "m_partial"}; !slices.Equal(got, want) {
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
