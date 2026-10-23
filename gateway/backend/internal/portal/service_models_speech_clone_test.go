// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"testing"
)

// speechCloneRow is one speech_clone capability row of the given verdict,
// attributed to the audio.cpp probe source that actually writes it.
func speechCloneRow(verdict string) routing.CapabilityRow {
	return routing.CapabilityRow{Capability: routing.CapabilitySpeechClone, Verdict: verdict, Source: routing.CapabilitySourceAudioCppModels, CheckedAt: offeringTime}
}

// ModelDTO.SpeechClone marks a speech model that synthesises in a REFERENCE
// voice (audio.cpp task "clone"), which a text-only chat composer cannot drive
// (#252). It refines Speech without changing it: a clone model stays Speech:true
// (the direct /v1/audio/speech endpoint still serves it) while SpeechClone:true
// lets the chat picker exclude it. The fold is the deliberate INVERSE of the
// fail-closed Speech fold -- it answers "should the picker HIDE this?" -- so it
// is fail-OPEN: a plain tts model and a speech model whose task is unknown are
// both SpeechClone:false and stay offered; only a known clone model is true.
func TestModelsResponseSpeechCloneFlag(t *testing.T) {
	ctx := context.Background()
	audio := []string{routing.APIFlavorOpenAIAudio}
	rs := routing.NewMemoryStore()
	offerSpeechModel(t, rs, routing.ProviderAudioCpp, audio, nil, "tts", speechRow(routing.CapabilityYes), speechCloneRow(routing.CapabilityNo))
	offerSpeechModel(t, rs, routing.ProviderAudioCpp, audio, nil, "clone", speechRow(routing.CapabilityYes), speechCloneRow(routing.CapabilityYes))
	offerSpeechModel(t, rs, routing.ProviderAudioCpp, audio, nil, "tts-unclassified", speechRow(routing.CapabilityYes))
	byID := modelsByID(offerSvc(rs, nil).Models(ctx, auth.Token{UserID: "usr_1"}))

	for _, tc := range []struct {
		name                string
		speech, speechClone bool
	}{
		{"tts", true, false},              // plain synthesiser: offered
		{"clone", true, true},             // clone model: Speech stays true, but hidden from chat
		{"tts-unclassified", true, false}, // no speech_clone row -> fail-open, offered
	} {
		got, ok := byID[tc.name]
		if !ok {
			t.Fatalf("%s missing from Models(): %#v", tc.name, byID)
		}
		if got.Speech != tc.speech || got.SpeechClone != tc.speechClone {
			t.Errorf("%s speech=%v speechClone=%v, want speech=%v speechClone=%v", tc.name, got.Speech, got.SpeechClone, tc.speech, tc.speechClone)
		}
	}
}

// offerSharedAudioMapping seeds ONE active audio.cpp mapping on its own server and
// application, so several calls can put more than one mapping under the same
// gateway model name (offerSpeechModel names its rows after the model and so can
// seed only one per name). rows are the mapping's capability rows.
func offerSharedAudioMapping(t *testing.T, rs *routing.MemoryStore, srvID, appID, mappingID, gateway string, rows ...routing.CapabilityRow) {
	t.Helper()
	ctx := context.Background()
	audio := []string{routing.APIFlavorOpenAIAudio}
	if err := rs.CreateAIServer(ctx, routing.AIServer{ID: srvID, Name: srvID, Domain: srvID + ".test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: offeringTime, UpdatedAt: offeringTime}); err != nil {
		t.Fatalf("CreateAIServer %s: %v", srvID, err)
	}
	if err := rs.CreateApplication(ctx, routing.Application{ID: appID, ServerID: srvID, Type: routing.ProviderAudioCpp, Port: 8000, Scheme: "http", APIFlavors: audio, Status: routing.ServerStatusActive, CreatedAt: offeringTime, UpdatedAt: offeringTime}); err != nil {
		t.Fatalf("CreateApplication %s: %v", appID, err)
	}
	if err := rs.CreateMapping(ctx, routing.ModelMapping{ID: mappingID, ApplicationID: appID, GatewayModelName: gateway, AppModelName: gateway, Status: routing.ServerStatusActive, CreatedAt: offeringTime, UpdatedAt: offeringTime}); err != nil {
		t.Fatalf("CreateMapping %s: %v", mappingID, err)
	}
	if len(rows) > 0 {
		if err := rs.UpsertMappingCapabilities(ctx, mappingID, rows); err != nil {
			t.Fatalf("UpsertMappingCapabilities %s: %v", mappingID, err)
		}
	}
}

// The fold is an OR across a name's mappings, the inverse of the fail-closed AND
// the Speech fold uses: a model mapped onto one tts server and one clone server
// is SpeechClone:true, because a chat send to that name could route to the clone
// mapping and fail. Two tts mappings stay false -- the pin that the accumulator
// seeds false and only a real clone mapping raises it.
func TestModelsResponseSpeechCloneIsOrAcrossMappings(t *testing.T) {
	ctx := context.Background()
	rs := routing.NewMemoryStore()
	offerSharedAudioMapping(t, rs, "srv_a", "app_a", "map_a", "mixed", speechRow(routing.CapabilityYes), speechCloneRow(routing.CapabilityNo))
	offerSharedAudioMapping(t, rs, "srv_b", "app_b", "map_b", "mixed", speechRow(routing.CapabilityYes), speechCloneRow(routing.CapabilityYes))
	offerSharedAudioMapping(t, rs, "srv_c", "app_c", "map_c", "both-tts", speechRow(routing.CapabilityYes), speechCloneRow(routing.CapabilityNo))
	offerSharedAudioMapping(t, rs, "srv_d", "app_d", "map_d", "both-tts", speechRow(routing.CapabilityYes), speechCloneRow(routing.CapabilityNo))
	byID := modelsByID(offerSvc(rs, nil).Models(ctx, auth.Token{UserID: "usr_1"}))

	if !byID["mixed"].Speech || !byID["mixed"].SpeechClone {
		t.Errorf("mixed speech=%v speechClone=%v, want both true -- one clone mapping hides the name", byID["mixed"].Speech, byID["mixed"].SpeechClone)
	}
	if byID["both-tts"].SpeechClone {
		t.Error("both-tts speechClone = true, want false -- two tts mappings must not raise the OR off a false seed")
	}
}

// A group is a clone group when ANY offerable member is one (the OR twin of the
// group's fail-closed AND for serve-folds): the group's Speech stays true (every
// member synthesises) while SpeechClone:true hides the group from the picker.
func TestModelsResponseSpeechCloneGroupIsOr(t *testing.T) {
	ctx := context.Background()
	audio := []string{routing.APIFlavorOpenAIAudio}
	rs := routing.NewMemoryStore()
	offerSpeechModel(t, rs, routing.ProviderAudioCpp, audio, nil, "g-tts", speechRow(routing.CapabilityYes), speechCloneRow(routing.CapabilityNo))
	offerSpeechModel(t, rs, routing.ProviderAudioCpp, audio, nil, "g-clone", speechRow(routing.CapabilityYes), speechCloneRow(routing.CapabilityYes))
	offerGroup(t, rs, "grp_clone", "clone-group", "g-tts", "g-clone")
	byID := modelsByID(offerSvc(rs, nil).Models(ctx, auth.Token{UserID: "usr_1"}))

	grp, ok := byID["clone-group"]
	if !ok {
		t.Fatalf("clone-group missing from Models(): %#v", byID)
	}
	if !grp.Speech || !grp.SpeechClone {
		t.Errorf("clone-group speech=%v speechClone=%v, want both true -- one clone member hides the group", grp.Speech, grp.SpeechClone)
	}
}

// An override alias carries its target's SpeechClone under the alias name, like
// every other capability flag the alias redirect wires in (inheritTargetRow): an
// alias onto a clone model must be hidden from the picker exactly as its target
// is, or it would re-open the broken affordance behind a second name.
func TestModelsResponseSpeechCloneAliasRedirect(t *testing.T) {
	ctx := context.Background()
	audio := []string{routing.APIFlavorOpenAIAudio}
	rs := routing.NewMemoryStore()
	offerSpeechModel(t, rs, routing.ProviderAudioCpp, audio, nil, "clone-target", speechRow(routing.CapabilityYes), speechCloneRow(routing.CapabilityYes))
	offerSpeechModel(t, rs, routing.ProviderAudioCpp, audio, nil, "tts-target", speechRow(routing.CapabilityYes), speechCloneRow(routing.CapabilityNo))
	token := tokenWithRules(map[string]store.ModelOverrideRule{
		"alias-clone": {To: "clone-target", Offer: true},
		"alias-tts":   {To: "tts-target", Offer: true},
	})
	byID := modelsByID(offerSvc(rs, nil).Models(ctx, token))

	for name, want := range map[string]bool{"alias-clone": true, "alias-tts": false} {
		got, ok := byID[name]
		if !ok {
			t.Fatalf("%s missing from Models(): %#v", name, byID)
		}
		if got.SpeechClone != want {
			t.Errorf("%s speechClone = %v, want %v (its target's flag)", name, got.SpeechClone, want)
		}
	}
}
