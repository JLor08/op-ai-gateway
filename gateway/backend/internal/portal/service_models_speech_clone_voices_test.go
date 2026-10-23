// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"strings"
	"testing"
)

// cacheVoices writes a mapping's scope-"" voices cache row (the one the voices
// writers keep) with the given raw payload.
func cacheVoices(t *testing.T, rs *routing.MemoryStore, mappingID, payload string) {
	t.Helper()
	row := routing.ModelMappingCacheRow{MappingID: mappingID, CacheType: routing.ModelMappingCacheVoices, Payload: payload, UpdatedAt: offeringTime}
	if err := rs.UpsertModelMappingCache(context.Background(), row); err != nil {
		t.Fatalf("UpsertModelMappingCache %s: %v", mappingID, err)
	}
}

// cloneVoicesMapping seeds one audio mapping under gateway with a (speech, yes)
// verdict and the given speech_clone verdict, plus -- when voices is non-empty --
// its voices cache payload. mappingID doubles as the server and application suffix.
func cloneVoicesMapping(t *testing.T, rs *routing.MemoryStore, mappingID, gateway, cloneVerdict, voices string) {
	t.Helper()
	offerSharedAudioMapping(t, rs, "srv_"+mappingID, "app_"+mappingID, mappingID, gateway, speechRow(routing.CapabilityYes), speechCloneRow(cloneVerdict))
	if voices != "" {
		cacheVoices(t, rs, mappingID, voices)
	}
}

// ModelDTO.SpeechCloneVoices is the chat picker's signal that a clone speech
// model is configured for it: every one of the name's clone mappings has preset
// voices cached, so a text-only composer can drive it without a reference voice.
// The picker offers `speech && (!speech_clone || speech_clone_voices)`.
//
// The fold is a fail-CLOSED AND over the name's mappings -- NOT an OR -- because
// it decides what to OFFER: a name whose mappings mix a preset clone and a
// reference-only clone must stay hidden, since a send could route to the
// reference-only mapping and fail upstream. A name with no clone mapping is true
// vacuously, which is harmless: the picker only reads it when speech_clone is set.
func TestModelsResponseSpeechCloneVoicesFold(t *testing.T) {
	ctx := context.Background()
	rs := routing.NewMemoryStore()

	// A single clone mapping, by what its voices cache says.
	cloneVoicesMapping(t, rs, "map_preset", "preset", routing.CapabilityYes, `["Frank"]`)
	cloneVoicesMapping(t, rs, "map_reference", "reference-only", routing.CapabilityYes, `[]`)
	cloneVoicesMapping(t, rs, "map_norow", "no-row", routing.CapabilityYes, "")
	cloneVoicesMapping(t, rs, "map_garbled", "garbled", routing.CapabilityYes, `not json`)

	// A name backing several mappings: the AND is over ALL of them.
	cloneVoicesMapping(t, rs, "map_mix_a", "preset-and-reference", routing.CapabilityYes, `["Frank"]`)
	cloneVoicesMapping(t, rs, "map_mix_b", "preset-and-reference", routing.CapabilityYes, `[]`)
	cloneVoicesMapping(t, rs, "map_two_a", "two-presets", routing.CapabilityYes, `["Frank"]`)
	cloneVoicesMapping(t, rs, "map_two_b", "two-presets", routing.CapabilityYes, `["Anna", "Ben"]`)
	cloneVoicesMapping(t, rs, "map_tp_a", "tts-and-preset", routing.CapabilityNo, "")
	cloneVoicesMapping(t, rs, "map_tp_b", "tts-and-preset", routing.CapabilityYes, `["Frank"]`)
	cloneVoicesMapping(t, rs, "map_tr_a", "tts-and-reference", routing.CapabilityNo, "")
	cloneVoicesMapping(t, rs, "map_tr_b", "tts-and-reference", routing.CapabilityYes, `[]`)

	// A name with no clone mapping at all.
	cloneVoicesMapping(t, rs, "map_tts", "plain-tts", routing.CapabilityNo, "")
	// A (speech_clone, yes) verdict on a mapping that does not serve audio is no
	// clone mapping (viewServesAudio is false), so it neither sets speech_clone nor
	// lowers speech_clone_voices.
	offerSpeechModel(t, rs, routing.ProviderAudioCpp, []string{routing.APIFlavorOpenAI}, nil, "text-only", speechCloneRow(routing.CapabilityYes))

	byID := modelsByID(offerSvc(rs, nil).Models(ctx, auth.Token{UserID: "usr_1"}))

	for _, tc := range []struct {
		name                     string
		speechClone, cloneVoices bool
	}{
		{"preset", true, true},                // preset clone: offered
		{"reference-only", true, false},       // clone with [] -> needs a reference voice, hidden
		{"no-row", true, false},               // clone never fetched -> unknown, fail closed
		{"garbled", true, false},              // undecodable row -> not a preset clone
		{"preset-and-reference", true, false}, // one reference-only mapping hides the name
		{"two-presets", true, true},           // every clone mapping has presets
		{"tts-and-preset", true, true},        // the plain-tts mapping never lowers it
		{"tts-and-reference", true, false},    // a send could still route to the reference-only mapping
		{"plain-tts", false, true},            // no clone mapping: vacuously true
		{"text-only", false, true},            // clone verdict on a non-audio mapping: not a clone mapping
	} {
		got, ok := byID[tc.name]
		if !ok {
			t.Fatalf("%s missing from Models(): %#v", tc.name, byID)
		}
		if got.SpeechClone != tc.speechClone || got.SpeechCloneVoices != tc.cloneVoices {
			t.Errorf("%s speechClone=%v speechCloneVoices=%v, want speechClone=%v speechCloneVoices=%v",
				tc.name, got.SpeechClone, got.SpeechCloneVoices, tc.speechClone, tc.cloneVoices)
		}
	}
}

// A group's speech_clone_voices is the fail-closed AND of its offerable members'
// flags (groupCapabilityFlags, NOT the OR twin speech_clone itself uses): one
// reference-only clone member keeps the group hidden even though another member
// has presets, because a send to the group could fail over to it.
func TestModelsResponseSpeechCloneVoicesGroupIsAnd(t *testing.T) {
	ctx := context.Background()
	audio := []string{routing.APIFlavorOpenAIAudio}
	rs := routing.NewMemoryStore()
	offerSpeechModel(t, rs, routing.ProviderAudioCpp, audio, nil, "g-preset", speechRow(routing.CapabilityYes), speechCloneRow(routing.CapabilityYes))
	cacheVoices(t, rs, "map_g-preset", `["Frank"]`)
	offerSpeechModel(t, rs, routing.ProviderAudioCpp, audio, nil, "g-reference", speechRow(routing.CapabilityYes), speechCloneRow(routing.CapabilityYes))
	cacheVoices(t, rs, "map_g-reference", `[]`)
	offerSpeechModel(t, rs, routing.ProviderAudioCpp, audio, nil, "g-tts", speechRow(routing.CapabilityYes), speechCloneRow(routing.CapabilityNo))
	offerGroup(t, rs, "grp_hidden", "hidden-group", "g-preset", "g-reference")
	offerGroup(t, rs, "grp_presets", "preset-group", "g-preset", "g-tts")
	offerGroup(t, rs, "grp_tts", "tts-group", "g-tts")
	byID := modelsByID(offerSvc(rs, nil).Models(ctx, auth.Token{UserID: "usr_1"}))

	for _, tc := range []struct {
		name                     string
		speechClone, cloneVoices bool
	}{
		{"hidden-group", true, false}, // a reference-only member: hidden
		{"preset-group", true, true},  // preset clone + plain tts: offered
		{"tts-group", false, true},    // no clone member: vacuously true
	} {
		got, ok := byID[tc.name]
		if !ok {
			t.Fatalf("%s missing from Models(): %#v", tc.name, byID)
		}
		if got.SpeechClone != tc.speechClone || got.SpeechCloneVoices != tc.cloneVoices {
			t.Errorf("%s speechClone=%v speechCloneVoices=%v, want speechClone=%v speechCloneVoices=%v",
				tc.name, got.SpeechClone, got.SpeechCloneVoices, tc.speechClone, tc.cloneVoices)
		}
	}
}

// An override alias carries its target's speech_clone_voices under the alias
// name, like every other capability flag the alias redirect wires in: an alias
// onto a reference-only clone model must stay hidden from the picker exactly as
// its target is, and an alias onto a preset clone model must be offered.
func TestModelsResponseSpeechCloneVoicesAliasRedirect(t *testing.T) {
	ctx := context.Background()
	audio := []string{routing.APIFlavorOpenAIAudio}
	rs := routing.NewMemoryStore()
	offerSpeechModel(t, rs, routing.ProviderAudioCpp, audio, nil, "preset-target", speechRow(routing.CapabilityYes), speechCloneRow(routing.CapabilityYes))
	cacheVoices(t, rs, "map_preset-target", `["Frank"]`)
	offerSpeechModel(t, rs, routing.ProviderAudioCpp, audio, nil, "reference-target", speechRow(routing.CapabilityYes), speechCloneRow(routing.CapabilityYes))
	cacheVoices(t, rs, "map_reference-target", `[]`)
	token := tokenWithRules(map[string]store.ModelOverrideRule{
		"alias-preset":    {To: "preset-target", Offer: true},
		"alias-reference": {To: "reference-target", Offer: true},
	})
	byID := modelsByID(offerSvc(rs, nil).Models(ctx, token))

	for name, want := range map[string]bool{"alias-preset": true, "alias-reference": false} {
		got, ok := byID[name]
		if !ok {
			t.Fatalf("%s missing from Models(): %#v", name, byID)
		}
		if !got.SpeechClone || got.SpeechCloneVoices != want {
			t.Errorf("%s speechClone=%v speechCloneVoices=%v, want speechClone=true speechCloneVoices=%v (its target's flags)", name, got.SpeechClone, got.SpeechCloneVoices, want)
		}
	}
}

// voicesReadStore counts the bulk voices-cache reads and fails them on demand,
// leaving every other store call to the embedded MemoryStore.
type voicesReadStore struct {
	*routing.MemoryStore
	err   error
	calls int
}

func (s *voicesReadStore) ModelMappingCacheForMappings(ctx context.Context, ids []string, cacheType string) (map[string][]routing.ModelMappingCacheRow, error) {
	if cacheType == routing.ModelMappingCacheVoices {
		s.calls++
		if s.err != nil {
			return nil, s.err
		}
	}
	return s.MemoryStore.ModelMappingCacheForMappings(ctx, ids, cacheType)
}

// The listing reads the voices cache in ONE batch however many mappings it folds
// over (no per-mapping read, the same N+1 guard the capability batch has).
func TestModelsResponseSpeechCloneVoicesIsOneBatchRead(t *testing.T) {
	rs := &voicesReadStore{MemoryStore: routing.NewMemoryStore()}
	cloneVoicesMapping(t, rs.MemoryStore, "map_a", "alpha", routing.CapabilityYes, `["Frank"]`)
	cloneVoicesMapping(t, rs.MemoryStore, "map_b", "beta", routing.CapabilityYes, `[]`)
	cloneVoicesMapping(t, rs.MemoryStore, "map_c", "gamma", routing.CapabilityNo, "")

	byID := modelsByID(offerSvc(rs, nil).Models(context.Background(), auth.Token{UserID: "usr_1"}))
	if rs.calls != 1 {
		t.Errorf("voices cache reads = %d for 3 mappings, want exactly 1 batch read", rs.calls)
	}
	if !byID["alpha"].SpeechCloneVoices || byID["beta"].SpeechCloneVoices {
		t.Errorf("alpha speechCloneVoices=%v beta=%v, want true/false", byID["alpha"].SpeechCloneVoices, byID["beta"].SpeechCloneVoices)
	}
}

// A failed voices read degrades FAIL-CLOSED and says so in the log: every clone
// model drops to speech_clone_voices=false (hidden from the chat picker) rather
// than being offered on unknown voices, while a name with no clone mapping is
// unaffected. The listing itself still comes back.
func TestModelsResponseSpeechCloneVoicesReadFailureFailsClosedAndLogs(t *testing.T) {
	rs := &voicesReadStore{MemoryStore: routing.NewMemoryStore(), err: errors.New("cache table unavailable")}
	cloneVoicesMapping(t, rs.MemoryStore, "map_preset", "preset", routing.CapabilityYes, `["Frank"]`)
	cloneVoicesMapping(t, rs.MemoryStore, "map_tts", "plain-tts", routing.CapabilityNo, "")

	var byID map[string]ModelDTO
	logged := captureSlog(t, func() {
		byID = modelsByID(offerSvc(rs, nil).Models(context.Background(), auth.Token{UserID: "usr_1"}))
	})

	if got, ok := byID["preset"]; !ok || !got.SpeechClone || got.SpeechCloneVoices {
		t.Errorf("preset = %#v (listed=%v), want speechClone=true speechCloneVoices=false on a failed voices read", got, ok)
	}
	if got, ok := byID["plain-tts"]; !ok || got.SpeechClone || !got.SpeechCloneVoices {
		t.Errorf("plain-tts = %#v (listed=%v), want speechClone=false speechCloneVoices=true", got, ok)
	}
	if !strings.Contains(logged, "cache table unavailable") || !strings.Contains(logged, "voices") {
		t.Errorf("log = %q, want a warning naming the voices read and its error", logged)
	}
}
