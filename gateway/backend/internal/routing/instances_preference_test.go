// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"testing"
	"time"
)

// A request may name the instance it would like to be served on
// (inference.Request.PreferredInstanceKey) -- a voice chat's leg, the instance it
// used last. The preference is advisory: it is honoured only for an instance that
// is a current candidate, under its cap and through every filter the request's
// candidates pass, and otherwise the request is selected as it would be without
// it -- never refused because of it.

// preferenceResolver is the instance fixture with instances 1 and 2 of map_inst
// running beside instance 0, all three loaded and free, and every instance gate's
// source wired: the plain selection serves instance 0.
func preferenceResolver(t *testing.T, activity map[string]int) (*Resolver, *fakeAdmission, *fakeDemandSink) {
	t.Helper()
	now := instanceCapacityNow
	r := NewResolver(liveInstanceStore(t, now), func() time.Time { return now }, nil)
	r.SetInstanceSource(&fakeInstanceSource{views: map[string]InstanceView{"map_inst": {Replicable: true, Parallelism: 1, Running: []int{1, 2}}}})
	r.SetLoadedModelChecker(&fakeLoaded{byServer: map[string][]string{"srv_inst": {"qwen2.5", "qwen2.5#1", "qwen2.5#2"}}})
	r.SetInstanceActivityChecker(&fakeInstanceActivity{byKey: activity})
	adm := timeoutAdmission()
	sink := &fakeDemandSink{}
	r.SetAdmissionController(adm)
	r.SetDemandSink(sink)
	return r, adm, sink
}

func preferring(req inference.Request, key string) inference.Request {
	req.PreferredInstanceKey = key
	return req
}

func TestPreferredInstanceChosenWhenFree(t *testing.T) {
	r, adm, sink := preferenceResolver(t, map[string]int{})
	if got, err := r.Resolve(context.Background(), auth.Token{}, chatRequest("qwen-coder")); err != nil || got.InstanceKey != "map_inst#0" {
		t.Fatalf("without a preference: %q, %v; want map_inst#0, the plain selection", got.InstanceKey, err)
	}
	for _, key := range []string{"map_inst#2", "map_inst#1", "map_inst#0"} {
		got, err := r.Resolve(context.Background(), auth.Token{}, preferring(chatRequest("qwen-coder"), key))
		if err != nil || got.InstanceKey != key {
			t.Fatalf("preferring %s: %q, %v; want the preferred instance", key, got.InstanceKey, err)
		}
		if got.ProviderModel != InstanceAddress("qwen2.5", got.Instance()) {
			t.Fatalf("preferring %s: provider model %q, want the instance's own address", key, got.ProviderModel)
		}
	}
	if adm.calls != 0 || len(sink.got) != 0 {
		t.Fatalf("waits %d, demand signals %v; want neither while the preferred instance is free", adm.calls, sink.got)
	}

	// A live session's preferred instance with its live slot free is served too.
	slots := &fakeLiveSlots{held: map[string]bool{}}
	r.SetLiveSlotChecker(slots)
	if got, err := r.Resolve(context.Background(), auth.Token{}, preferring(liveRequest("qwen-coder"), "map_inst#1")); err != nil || got.InstanceKey != "map_inst#1" {
		t.Fatalf("live session preferring map_inst#1: %q, %v; want it", got.InstanceKey, err)
	}
}

func TestPreferredInstanceIgnoredWhenBusy(t *testing.T) {
	t.Run("at its cap", func(t *testing.T) {
		r, adm, sink := preferenceResolver(t, map[string]int{"map_inst#2": 1})
		got, err := r.Resolve(context.Background(), auth.Token{}, preferring(chatRequest("qwen-coder"), "map_inst#2"))
		if err != nil || got.InstanceKey != "map_inst#0" {
			t.Fatalf("preferring the busy map_inst#2: %q, %v; want the plain selection map_inst#0", got.InstanceKey, err)
		}
		if adm.calls != 0 || len(sink.got) != 0 {
			t.Fatalf("waits %d, demand signals %v; want neither: other instances are free", adm.calls, sink.got)
		}
	})

	t.Run("its live slot held", func(t *testing.T) {
		r, _, _ := preferenceResolver(t, map[string]int{})
		r.SetLiveSlotChecker(&fakeLiveSlots{held: map[string]bool{"map_inst#1": true}})
		got, err := r.Resolve(context.Background(), auth.Token{}, preferring(liveRequest("qwen-coder"), "map_inst#1"))
		if err != nil || got.InstanceKey != "map_inst#0" {
			t.Fatalf("live session preferring the held map_inst#1: %q, %v; want map_inst#0, whose slot is free", got.InstanceKey, err)
		}
	})

	t.Run("every instance busy parks as without a preference", func(t *testing.T) {
		r, adm, sink := preferenceResolver(t, map[string]int{"map_inst#0": 1, "map_inst#1": 1, "map_inst#2": 1})
		_, err := r.Resolve(context.Background(), auth.Token{}, preferring(chatRequest("qwen-coder"), "map_inst#2"))
		if !errors.Is(err, ErrAdmissionQueueTimeout) || adm.calls != 1 || len(sink.got) != 1 {
			t.Fatalf("Resolve = %v after %d waits and %d signals; want the queue's timeout after one park and its demand signal", err, adm.calls, len(sink.got))
		}
	})

	t.Run("a request that must not park takes the least loaded instance", func(t *testing.T) {
		r, adm, _ := preferenceResolver(t, map[string]int{"map_inst#0": 2, "map_inst#1": 1, "map_inst#2": 2})
		req := preferring(chatRequest("qwen-coder"), "map_inst#2")
		req.NoPark = true
		got, err := r.Resolve(context.Background(), auth.Token{}, req)
		if err != nil || got.InstanceKey != "map_inst#1" || adm.calls != 0 {
			t.Fatalf("no-park Resolve preferring the busy map_inst#2: %q, %v after %d waits; want map_inst#1 without a wait", got.InstanceKey, err, adm.calls)
		}
	})
}

func TestPreferredInstanceIgnoredWhenGone(t *testing.T) {
	t.Run("no longer running", func(t *testing.T) {
		r, _, _ := preferenceResolver(t, map[string]int{})
		got, err := r.Resolve(context.Background(), auth.Token{}, preferring(chatRequest("qwen-coder"), "map_inst#3"))
		if err != nil || got.InstanceKey != "map_inst#0" {
			t.Fatalf("preferring the retired map_inst#3: %q, %v; want the plain selection map_inst#0", got.InstanceKey, err)
		}
	})

	t.Run("excluded as gone", func(t *testing.T) {
		r, _, _ := preferenceResolver(t, map[string]int{})
		req := preferring(chatRequest("qwen-coder"), "map_inst#2")
		req.ExcludeInstanceKeys = []string{"map_inst#2"}
		got, err := r.Resolve(context.Background(), auth.Token{}, req)
		if err != nil || got.InstanceKey != "map_inst#0" {
			t.Fatalf("preferring the excluded map_inst#2: %q, %v; want map_inst#0", got.InstanceKey, err)
		}
	})

	t.Run("an unknown key", func(t *testing.T) {
		r, _, _ := preferenceResolver(t, map[string]int{})
		got, err := r.Resolve(context.Background(), auth.Token{}, preferring(chatRequest("qwen-coder"), "map_other#1"))
		if err != nil || got.InstanceKey != "map_inst#0" {
			t.Fatalf("preferring a key of no candidate: %q, %v; want map_inst#0", got.InstanceKey, err)
		}
	})

	t.Run("dropped by the real-time gate", func(t *testing.T) {
		mem := NewMemoryStore()
		addSpeechMapping(t, mem, "srv_a", "app_a", "map_a", "tts")
		addSpeechMapping(t, mem, "srv_c", "app_c", "map_c", "tts")
		setAudioRate(t, mem, "map_a", 0.9)
		r := NewResolver(mem, voiceClock(), nil)
		r.SetAudioRealtimeMinFactor(1.2)
		plain, realtime := realtimeReq("tts")
		// The control: a request without the marker is served on the instance it prefers,
		// against the plain selection's choice (map_a wins on its audio score).
		if got := resolveVoice(t, r, preferring(plain, "map_c#0")); got.InstanceKey != "map_c#0" {
			t.Fatalf("plain request preferring map_c#0 routed to %q, want it", got.InstanceKey)
		}
		if got := resolveVoice(t, r, preferring(realtime, "map_a#0")); got.InstanceKey != "map_c#0" {
			t.Fatalf("real-time request preferring the too-slow map_a#0 routed to %q, want map_c#0", got.InstanceKey)
		}
		setAudioRate(t, mem, "map_c", 1.1)
		if _, err := r.Resolve(context.Background(), voicePrincipal, preferring(realtime, "map_a#0")); !errors.Is(err, ErrNoRealtimeCandidate) {
			t.Fatalf("every candidate too slow: err = %v, want ErrNoRealtimeCandidate, as without a preference", err)
		}
	})
}
