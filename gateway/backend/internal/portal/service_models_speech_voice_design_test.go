// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"testing"
	"time"
)

// voiceDesignRow is one speech_voice_design capability row of the given verdict,
// the twin of embeddingRow.
func voiceDesignRow(verdict string) routing.CapabilityRow {
	return routing.CapabilityRow{Capability: routing.CapabilitySpeechVoiceDesign, Verdict: verdict, Source: routing.CapabilitySourceManual, CheckedAt: offeringTime}
}

// The capability name is part of the stored vocabulary (the operator's mapping
// form writes it verbatim) and of the portal contract, so the string is pinned.
func TestSpeechVoiceDesignCapabilityName(t *testing.T) {
	if routing.CapabilitySpeechVoiceDesign != "speech_voice_design" {
		t.Errorf("CapabilitySpeechVoiceDesign = %q, want %q", routing.CapabilitySpeechVoiceDesign, "speech_voice_design")
	}
}

// ModelDTO.SpeechVoiceDesign is a (speech_voice_design, yes) verdict ANDed across
// a model's mappings, fail-closed, a group's flag the AND of its members, and an
// alias carries its target's flag. A "no" row and no row at all are both false,
// and another speech refinement's yes never sets it.
func TestModelsResponseSpeechVoiceDesignFlag(t *testing.T) {
	ctx := context.Background()
	audio := []string{routing.APIFlavorOpenAIAudio}
	rs := routing.NewMemoryStore()
	offerSpeechModel(t, rs, routing.ProviderAudioCpp, audio, nil, "vd-yes", speechRow(routing.CapabilityYes), voiceDesignRow(routing.CapabilityYes))
	offerSpeechModel(t, rs, routing.ProviderAudioCpp, audio, nil, "vd-yes-2", speechRow(routing.CapabilityYes), voiceDesignRow(routing.CapabilityYes))
	offerSpeechModel(t, rs, routing.ProviderAudioCpp, audio, nil, "vd-no", speechRow(routing.CapabilityYes), voiceDesignRow(routing.CapabilityNo))
	offerSpeechModel(t, rs, routing.ProviderAudioCpp, audio, nil, "vd-missing", speechRow(routing.CapabilityYes))
	offerSpeechModel(t, rs, routing.ProviderAudioCpp, audio, nil, "vd-clone-only", speechRow(routing.CapabilityYes), routing.CapabilityRow{Capability: routing.CapabilitySpeechClone, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceManual, CheckedAt: offeringTime})
	offerGroup(t, rs, "grp_pure_vd", "pure-vd-group", "vd-yes", "vd-yes-2")
	offerGroup(t, rs, "grp_mixed_vd", "mixed-vd-group", "vd-yes", "vd-missing")
	token := tokenWithRules(map[string]store.ModelOverrideRule{
		"alias-vd":    {To: "vd-yes", Offer: true},
		"alias-no-vd": {To: "vd-missing", Offer: true},
	})
	byID := modelsByID(offerSvc(rs, nil).Models(ctx, token))

	for _, tc := range []struct {
		name string
		want bool
	}{
		{"vd-yes", true},
		{"vd-no", false},
		{"vd-missing", false},    // no row: unknown is not designable
		{"vd-clone-only", false}, // another refinement's yes is not this one
		{"pure-vd-group", true},
		{"mixed-vd-group", false}, // one member lacks the verdict: AND
		{"alias-vd", true},
		{"alias-no-vd", false},
	} {
		got, ok := byID[tc.name]
		if !ok {
			t.Fatalf("%s missing from Models(): %#v", tc.name, byID)
		}
		if got.SpeechVoiceDesign != tc.want {
			t.Errorf("%s speech_voice_design=%v, want %v", tc.name, got.SpeechVoiceDesign, tc.want)
		}
	}
}

// The flag is an AND across a model's mappings: one yes mapping beside one
// without the row is advertised false.
func TestModelsResponseSpeechVoiceDesignAndAcrossMappings(t *testing.T) {
	ctx := context.Background()
	rs := routing.NewMemoryStore()
	offer := func(srvID, appID, mappingID, gateway string, rows ...routing.CapabilityRow) {
		t.Helper()
		if err := rs.CreateAIServer(ctx, routing.AIServer{ID: srvID, Name: srvID, Domain: srvID + ".test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: offeringTime, UpdatedAt: offeringTime}); err != nil {
			t.Fatalf("CreateAIServer %s: %v", srvID, err)
		}
		if err := rs.CreateApplication(ctx, routing.Application{ID: appID, ServerID: srvID, Type: routing.ProviderAudioCpp, Port: 8000, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAIAudio}, Status: routing.ServerStatusActive, CreatedAt: offeringTime, UpdatedAt: offeringTime}); err != nil {
			t.Fatalf("CreateApplication %s: %v", appID, err)
		}
		if err := rs.CreateMapping(ctx, routing.ModelMapping{ID: mappingID, ApplicationID: appID, GatewayModelName: gateway, AppModelName: gateway, Status: routing.ServerStatusActive, CreatedAt: offeringTime, UpdatedAt: offeringTime}); err != nil {
			t.Fatalf("CreateMapping %s: %v", mappingID, err)
		}
		if err := rs.UpsertMappingCapabilities(ctx, mappingID, rows); err != nil {
			t.Fatalf("UpsertMappingCapabilities %s: %v", mappingID, err)
		}
	}
	offer("srv_a", "app_a", "map_a", "split", speechRow(routing.CapabilityYes), voiceDesignRow(routing.CapabilityYes))
	offer("srv_b", "app_b", "map_b", "split", speechRow(routing.CapabilityYes))
	offer("srv_c", "app_c", "map_c", "both-yes", speechRow(routing.CapabilityYes), voiceDesignRow(routing.CapabilityYes))
	offer("srv_d", "app_d", "map_d", "both-yes", speechRow(routing.CapabilityYes), voiceDesignRow(routing.CapabilityYes))
	byID := modelsByID(offerSvc(rs, nil).Models(ctx, auth.Token{UserID: "usr_1"}))

	if byID["split"].SpeechVoiceDesign {
		t.Error("split speech_voice_design=true, want false -- a mapping without the row ANDs in as false")
	}
	if !byID["both-yes"].SpeechVoiceDesign {
		t.Error("both-yes speech_voice_design=false, want true -- two yes mappings must reach true")
	}
}

// speech_voice_design is an operator-only manual verdict: the mapping update
// accepts a yes (and a no -- it is not a reserved pair), the returned DTO lists
// the row, and an empty verdict resets it.
func TestMappingAcceptsAManualSpeechVoiceDesignVerdict(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	for _, verdict := range []string{routing.CapabilityYes, routing.CapabilityNo} {
		t.Run(verdict, func(t *testing.T) {
			recorder := &capabilityResetRecorder{MemoryStore: routing.NewMemoryStore()}
			fx := newCapabilityTestFixture(t, now, recorder)
			dto, err := fx.svc.UpdateMapping(ctx, ownerToken(), fx.mappingID, UpdateMappingRequest{
				CapabilityVerdicts: map[string]string{routing.CapabilitySpeechVoiceDesign: verdict},
			})
			if err != nil {
				t.Fatalf("UpdateMapping: %v", err)
			}
			rows := routing.CapabilityRowsByName(mustMappingCapabilities(t, recorder.MemoryStore, fx.mappingID))
			if got := rows[routing.CapabilitySpeechVoiceDesign]; got.Verdict != verdict || got.Source != routing.CapabilitySourceManual {
				t.Fatalf("speech_voice_design row = %+v, want a manual %s", got, verdict)
			}
			found := false
			for _, c := range dto.Capabilities {
				if c.Capability == routing.CapabilitySpeechVoiceDesign && c.Verdict == verdict {
					found = true
				}
			}
			if !found {
				t.Fatalf("returned DTO capabilities = %+v, want a %s row for %s", dto.Capabilities, verdict, routing.CapabilitySpeechVoiceDesign)
			}

			if _, err := fx.svc.UpdateMapping(ctx, ownerToken(), fx.mappingID, UpdateMappingRequest{
				CapabilityVerdicts: map[string]string{routing.CapabilitySpeechVoiceDesign: ""},
			}); err != nil {
				t.Fatalf("UpdateMapping (reset): %v", err)
			}
			if _, ok := routing.CapabilityRowsByName(mustMappingCapabilities(t, recorder.MemoryStore, fx.mappingID))[routing.CapabilitySpeechVoiceDesign]; ok {
				t.Fatal("speech_voice_design row survived its reset")
			}
		})
	}
}
