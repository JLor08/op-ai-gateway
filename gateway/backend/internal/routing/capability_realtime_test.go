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

// realtimeRequest is a /v1/realtime request's routing shape: the coarse audio
// flavor plus the realtime capability.
func realtimeRequest(model string) inference.Request {
	return inference.Request{Model: model, APIFlavor: APIFlavorOpenAIAudio, RequiredCapabilities: []string{CapabilityRealtime}}
}

// TestCapabilityRealtimeName pins the stored name: rows and the portal form
// carry the literal.
func TestCapabilityRealtimeName(t *testing.T) {
	if CapabilityRealtime != "realtime" {
		t.Fatalf("CapabilityRealtime = %q, want %q", CapabilityRealtime, "realtime")
	}
}

// A realtime request resolves only to a mapping that carries a literal
// realtime:yes row on an application serving openai_audio. Every other shape is
// no candidate.
func TestResolveRealtimeNeedsTheAudioFlavorAndAYesRow(t *testing.T) {
	now := instanceCapacityNow
	for _, tc := range []struct {
		name    string
		audio   bool
		verdict string // empty: no row
		want    bool
	}{
		{"audio flavor and a yes row", true, CapabilityYes, true},
		{"audio flavor and a no row", true, CapabilityNo, false},
		{"audio flavor and no row", true, "", false},
		{"yes row but no audio flavor", false, CapabilityYes, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := liveInstanceStore(t, now)
			if !tc.audio {
				app, err := store.ApplicationByID(context.Background(), "app_inst")
				if err != nil {
					t.Fatalf("ApplicationByID: %v", err)
				}
				app.APIFlavors = []string{APIFlavorOpenAI}
				if err := store.UpdateApplication(context.Background(), app); err != nil {
					t.Fatalf("UpdateApplication: %v", err)
				}
			}
			if tc.verdict != "" {
				if err := store.UpsertMappingCapabilities(context.Background(), "map_inst", []CapabilityRow{
					{Capability: CapabilityRealtime, Verdict: tc.verdict, Source: CapabilitySourceManual, CheckedAt: now},
				}); err != nil {
					t.Fatalf("UpsertMappingCapabilities: %v", err)
				}
			}
			r := NewResolver(store, func() time.Time { return now }, nil)
			target, err := r.Resolve(context.Background(), auth.Token{}, realtimeRequest("qwen-coder"))
			if tc.want {
				if err != nil || target.RouteID != "map_inst" {
					t.Fatalf("Resolve = %+v, %v; want map_inst", target, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Resolve = %+v, want no candidate", target)
			}
			if !errors.Is(err, ErrNoModelRoute) {
				t.Logf("refusal: %v", err)
			}
		})
	}
}
