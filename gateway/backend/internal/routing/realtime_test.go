// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"context"
	"errors"
	"op-ai-gateway/internal/inference"
	"slices"
	"testing"
)

// The real-time gate of a voice chat's speech and transcription legs: a request
// that must keep up with real time (inference.Request.RealtimeRequired) is never
// routed to a mapping measured slower than the threshold, an unmeasured mapping
// stays eligible, and a model whose every candidate is too slow is refused with
// ErrNoRealtimeCandidate rather than served behind the conversation.

// setAudioRate stores rate as mapping id's measured audio rate, as a speed run would.
func setAudioRate(t *testing.T, mem *MemoryStore, id string, rate float64) {
	t.Helper()
	if err := mem.UpdateMappingAudioMetrics(context.Background(), id, AudioMetrics{RateX: rate}, "audio_speed", voiceNow); err != nil {
		t.Fatalf("UpdateMappingAudioMetrics %s: %v", id, err)
	}
}

func realtimeReq(model string) (plain, realtime inference.Request) {
	plain = speechReq(model)
	realtime = speechReq(model)
	realtime.RealtimeRequired = true
	return plain, realtime
}

func TestRealtimeGateDropsSlowCandidates(t *testing.T) {
	t.Run("filter", func(t *testing.T) {
		r := NewResolver(NewMemoryStore(), voiceClock(), nil)
		r.SetAudioRealtimeMinFactor(1.2)
		cands := []MappingCandidate{
			{Mapping: ModelMapping{ID: "slow", AudioRateX: 0.9}},
			{Mapping: ModelMapping{ID: "fast", AudioRateX: 1.5}},
			{Mapping: ModelMapping{ID: "unmeasured"}},
		}
		_, req := realtimeReq("tts")
		got, err := r.filterRealtime(cands, req)
		if err != nil {
			t.Fatalf("filterRealtime: %v", err)
		}
		var ids []string
		for _, c := range got {
			ids = append(ids, c.Mapping.ID)
		}
		if !slices.Equal(ids, []string{"fast", "unmeasured"}) {
			t.Fatalf("kept = %v, want [fast unmeasured]", ids)
		}
		if len(cands) != 3 || cands[0].Mapping.ID != "slow" {
			t.Fatalf("the caller's candidates were changed: %+v", cands)
		}
		plain, _ := realtimeReq("tts")
		if got, _ := r.filterRealtime(cands, plain); len(got) != 3 {
			t.Fatalf("a request without the marker kept %d candidates, want all 3", len(got))
		}
	})

	t.Run("standard path", func(t *testing.T) {
		mem := NewMemoryStore()
		addSpeechMapping(t, mem, "srv_a", "app_a", "map_a", "tts")
		addSpeechMapping(t, mem, "srv_c", "app_c", "map_c", "tts")
		setAudioRate(t, mem, "map_a", 0.9)
		r := NewResolver(mem, voiceClock(), nil)
		r.SetAudioRealtimeMinFactor(1.2)
		plain, realtime := realtimeReq("tts")
		// The control: without the marker the measured map_a wins on its audio score.
		if got := resolveVoice(t, r, plain); got.RouteID != "map_a" {
			t.Fatalf("plain request routed to %q, want map_a", got.RouteID)
		}
		if got := resolveVoice(t, r, realtime); got.RouteID != "map_c" {
			t.Fatalf("real-time request routed to %q, want map_c (unmeasured stays eligible)", got.RouteID)
		}
		setAudioRate(t, mem, "map_c", 1.1)
		if _, err := r.Resolve(context.Background(), voicePrincipal, realtime); !errors.Is(err, ErrNoRealtimeCandidate) {
			t.Fatalf("every candidate too slow: err = %v, want ErrNoRealtimeCandidate", err)
		}
		if got := resolveVoice(t, r, plain); got.RouteID == "" {
			t.Fatal("a plain request must still be served by a slow model")
		}
	})

	t.Run("group path", func(t *testing.T) {
		mem := voiceGroupStore(t, nil, nil)
		setAudioRate(t, mem, "map_a", 0.9)
		setAudioRate(t, mem, "map_b", 1.5)
		r := NewResolver(mem, voiceClock(), nil)
		r.SetAudioRealtimeMinFactor(1.2)
		r.SetGroupResolver(voiceGroup(GroupPolicy{FailoverMode: "sticky"}))
		plain, realtime := realtimeReq("tts-group")
		if got := resolveVoice(t, r, plain); got.ServerID != "srv_a" {
			t.Fatalf("plain group request served by %q, want the first member's srv_a", got.ServerID)
		}
		if got := resolveVoice(t, r, realtime); got.ServerID != "srv_b" {
			t.Fatalf("real-time group request served by %q, want srv_b (the first member is too slow)", got.ServerID)
		}
		setAudioRate(t, mem, "map_b", 1.0)
		if _, err := r.Resolve(context.Background(), voicePrincipal, realtime); !errors.Is(err, ErrNoRealtimeCandidate) {
			t.Fatalf("every member too slow: err = %v, want ErrNoRealtimeCandidate", err)
		}
	})

	t.Run("server override", func(t *testing.T) {
		mem := NewMemoryStore()
		addSpeechMapping(t, mem, "srv_a", "app_a", "map_a", "tts")
		setAudioRate(t, mem, "map_a", 0.9)
		r := NewResolver(mem, voiceClock(), nil)
		r.SetAudioRealtimeMinFactor(1.2)
		plain, realtime := realtimeReq("tts")
		plain.ServerOverrideID, realtime.ServerOverrideID = "srv_a", "srv_a"
		if _, err := r.Resolve(context.Background(), voicePrincipal, plain); err != nil {
			t.Fatalf("plain override: %v", err)
		}
		if _, err := r.Resolve(context.Background(), voicePrincipal, realtime); !errors.Is(err, ErrNoRealtimeCandidate) {
			t.Fatalf("override to a too-slow server: err = %v, want ErrNoRealtimeCandidate", err)
		}
	})

	t.Run("no threshold gates nothing", func(t *testing.T) {
		mem := NewMemoryStore()
		addSpeechMapping(t, mem, "srv_a", "app_a", "map_a", "tts")
		setAudioRate(t, mem, "map_a", 0.2)
		r := NewResolver(mem, voiceClock(), nil)
		_, realtime := realtimeReq("tts")
		if got := resolveVoice(t, r, realtime); got.RouteID != "map_a" {
			t.Fatalf("routed to %q, want map_a", got.RouteID)
		}
	})
}
