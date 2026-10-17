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

// ModelDTO.Speech is the listing's speech flag, the twin of Image: the speech
// fold (speechFlagsByName) ANDed across a model's mappings, so the portal chat
// can switch its composer to a speech turn only for a model the speech endpoint
// would serve. Every shape the fold tells apart is pinned, because each is a
// distinct decision -- and the flag is independent of Image and Vision.
func TestModelsResponseSpeechFlag(t *testing.T) {
	ctx := context.Background()
	svc := offerSvc(speechOfferingStore(t), nil)
	byID := modelsByID(svc.Models(ctx, auth.Token{UserID: "usr_1"}))

	for _, tc := range []struct {
		name   string
		speech bool
	}{
		{"tts-yes", true},
		{"tts-no", false},                 // an explicit (speech, no) verdict
		{"tts-unprobed", false},           // never probed: unknown is not capable
		{"tts-image-verdict-only", false}, // another capability's yes is not speech
		{"tts-also-text", true},           // audio next to a text flavor still serves speech
		{"text-flavor", false},            // speech:yes, but no openai_audio on the application
		{"agent-both", true},              // spec and application both declare openai_audio
		{"agent-spec-narrows", false},     // the spec drops openai_audio the application declares
		{"agent-no-spec", true},           // no spec: the application is the flavor authority
		{"pure-speech-group", true},       // every offerable member qualifies
		{"mixed-speech-group", false},     // one member lacks the verdict
	} {
		got, ok := byID[tc.name]
		if !ok {
			t.Fatalf("%s missing from Models(): %#v", tc.name, byID)
		}
		if got.Speech != tc.speech {
			t.Errorf("%s speech = %v, want %v", tc.name, got.Speech, tc.speech)
		}
	}
}

// Speech is independent of image and vision: an image model is not a speech
// model and a speech model is not an image model, whatever each looks like to
// the other fold. This is the specific error the capability vocabulary warns
// about for image/vision, applied to the third axis.
func TestModelsResponseSpeechIsIndependentOfImageAndVision(t *testing.T) {
	ctx := context.Background()
	rs := routing.NewMemoryStore()
	offerSpeechModel(t, rs, routing.ProviderAudioCpp, []string{routing.APIFlavorOpenAIAudio}, nil, "voice", speechRow(routing.CapabilityYes))
	offerModelImage(t, rs, "srv_sd", "SdBox", "app_sd", imageServingFlavors, "sd", "sd-up", true)
	offerModelVision(t, rs, "srv_vis", "VisBox", "app_vis", []string{routing.APIFlavorOpenAI}, "vis", "vis-up", true)
	offerModel(t, rs, "srv_text", "TextBox", "app_text", []string{routing.APIFlavorOpenAI}, "qwen3-32b", "qwen3-32b", routing.ServerStatusActive)
	byID := modelsByID(offerSvc(rs, nil).Models(ctx, auth.Token{UserID: "usr_1"}))

	for _, tc := range []struct {
		name                   string
		speech, image, visible bool
	}{
		{"voice", true, false, false},
		{"sd", false, true, false}, // image:yes is not speech
		{"vis", false, false, true},
		{"qwen3-32b", false, false, false},
	} {
		got, ok := byID[tc.name]
		if !ok {
			t.Fatalf("%s missing from Models(): %#v", tc.name, byID)
		}
		if got.Speech != tc.speech || got.Image != tc.image || got.Vision != tc.visible {
			t.Errorf("%s speech=%v image=%v vision=%v, want speech=%v image=%v vision=%v",
				tc.name, got.Speech, got.Image, got.Vision, tc.speech, tc.image, tc.visible)
		}
	}
}

// The flag is an AND across a model's mappings, fail-closed, like image: a
// model on server A (speech:yes) and server B (speech:no or never probed) is
// advertised false, the safe direction. "both-yes" is what pins the fail-closed
// SEED: two yes mappings only reach true because the accumulator starts true on
// a model's first view.
func TestModelsResponseSpeechAndAcrossMappings(t *testing.T) {
	ctx := context.Background()
	audio := []string{routing.APIFlavorOpenAIAudio}
	rs := routing.NewMemoryStore()
	// offerSpeechModel derives its server, application and mapping ids from the
	// gateway name, so it cannot seed two mappings of ONE name, which is what
	// the per-name AND needs.
	offerSharedSpeechMapping := func(srvID, appID, mappingID, gateway string, rows ...routing.CapabilityRow) {
		t.Helper()
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

	offerSharedSpeechMapping("srv_a", "app_a", "map_a", "split", speechRow(routing.CapabilityYes))
	offerSharedSpeechMapping("srv_b", "app_b", "map_b", "split", speechRow(routing.CapabilityNo))
	offerSharedSpeechMapping("srv_c", "app_c", "map_c", "unprobed-second", speechRow(routing.CapabilityYes))
	offerSharedSpeechMapping("srv_d", "app_d", "map_d", "unprobed-second")
	offerSharedSpeechMapping("srv_e", "app_e", "map_e", "both-yes", speechRow(routing.CapabilityYes))
	offerSharedSpeechMapping("srv_f", "app_f", "map_f", "both-yes", speechRow(routing.CapabilityYes))
	byID := modelsByID(offerSvc(rs, nil).Models(ctx, auth.Token{UserID: "usr_1"}))

	if byID["split"].Speech {
		t.Error("split speech = true, want false -- one mapping says no, and the fold is an AND")
	}
	if byID["unprobed-second"].Speech {
		t.Error("unprobed-second speech = true, want false -- a never-probed mapping ANDs in as false")
	}
	if !byID["both-yes"].Speech {
		t.Error("both-yes speech = false, want true -- the fail-closed seed must let two yes mappings reach true")
	}
}

// An override alias carries its target's Speech flag under the alias name, like
// the Image flag it sits beside: the alias row is a display of the target's row
// filed under another name, not a separate fold. It exists because the alias
// redirect (inheritTargetRow in service.go) wires each flag in on its own -- a
// build that wired only the older flags would pass every alias test that
// exists (none of them look at Speech) while leaving speech silently false
// behind every alias, and an alias onto a speech model would never open the
// chat's speech composer.
func TestModelsResponseSpeechAliasRedirect(t *testing.T) {
	ctx := context.Background()
	svc := offerSvc(speechOfferingStore(t), nil)
	token := tokenWithRules(map[string]store.ModelOverrideRule{
		"alias-voice":  {To: "tts-yes", Offer: true},
		"alias-silent": {To: "tts-no", Offer: true},
	})
	byID := modelsByID(svc.Models(ctx, token))

	for name, want := range map[string]bool{"alias-voice": true, "alias-silent": false} {
		got, ok := byID[name]
		if !ok {
			t.Fatalf("%s missing from Models(): %#v", name, byID)
		}
		if got.Speech != want {
			t.Errorf("%s speech = %v, want %v (its target's flag)", name, got.Speech, want)
		}
	}
}

// The listing's Speech flag and ModelOffering.Capable for a speech request are
// one fold (speechFlagsByName): every callable name is capable exactly when the
// listing says Speech, so the chat cannot offer a speech turn the redirect
// would not, nor the reverse. The explicit expectations in
// TestModelsResponseSpeechFlag pin each shape so agreement cannot come from
// both sides being wrong together.
func TestModelsSpeechIsTheOfferingsSpeechFold(t *testing.T) {
	ctx := context.Background()
	svc := offerSvc(speechOfferingStore(t), nil)
	token := auth.Token{UserID: "usr_1"}

	off := svc.ModelOfferingFor(ctx, token, routing.APIFlavorOpenAIAudio, []string{routing.CapabilitySpeech})
	byID := modelsByID(svc.Models(ctx, token))
	if len(off.Callable) == 0 {
		t.Fatal("precondition: Callable(openai_audio) is empty")
	}
	for name := range off.Callable {
		if _, got := off.Capable[name]; got != byID[name].Speech {
			t.Errorf("%s capable = %v, listing Speech = %v -- the two must be one fold", name, got, byID[name].Speech)
		}
	}
}

// The admin management listing (ManageModels, ?manage=1) is built by the same
// function and carries the same flag.
func TestManageModelsCarriesSpeech(t *testing.T) {
	svc := offerSvc(speechOfferingStore(t), nil)
	byID := modelsByID(svc.ManageModels(context.Background(), auth.Token{UserID: "usr_1"}))
	if !byID["tts-yes"].Speech {
		t.Error("ManageModels tts-yes speech = false, want true")
	}
	if byID["tts-no"].Speech {
		t.Error("ManageModels tts-no speech = true, want false")
	}
}

// A failed capability read degrades the listing to speech=false for every model
// (fail-closed, never fail-open) and says so, naming speech among the withheld
// flags: the same contract vision, image and image_edit already carry.
func TestModelsSpeechIsWithheldWhenTheCapabilityReadFails(t *testing.T) {
	ctx := context.Background()
	rs := speechOfferingStore(t)

	working := offerSvc(rs, nil)
	if !modelsByID(working.Models(ctx, auth.Token{UserID: "usr_1"}))["tts-yes"].Speech {
		t.Fatal("precondition: tts-yes speech = false with a WORKING capability read, want true")
	}

	failing := offerSvc(&capabilityReadStore{MemoryStore: rs, err: errors.New("capability table unavailable")}, nil)
	var byID map[string]ModelDTO
	logged := captureSlog(t, func() {
		byID = modelsByID(failing.Models(ctx, auth.Token{UserID: "usr_1"}))
	})
	model, ok := byID["tts-yes"]
	if !ok {
		t.Fatalf("tts-yes missing from the listing: the capability read must DEGRADE, not drop models (%#v)", byID)
	}
	if model.Speech {
		t.Error("tts-yes speech = true after a failed capability read, want false (fail-closed)")
	}
	if !strings.Contains(logged, "capability read failed") || !strings.Contains(logged, "speech") {
		t.Errorf("log = %q, want the degrade logged with speech named among the withheld flags", logged)
	}
}

// A failed spec read must not be read as "no spec" for the speech flag, which
// would fall back to the application's flavors and report an agent child
// capable. It withholds speech for that application's models only; an ordinary
// audio application beside it is unaffected, since it has no spec to read.
func TestModelsSpeechWithholdsAnAgentModelWhenItsSpecReadFails(t *testing.T) {
	ctx := context.Background()
	audio := []string{routing.APIFlavorOpenAIAudio}
	textAndAudio := []string{routing.APIFlavorOpenAI, routing.APIFlavorOpenAIAudio}
	rs := routing.NewMemoryStore()
	offerSpeechModel(t, rs, routing.ProviderServerAgent, textAndAudio, audio, "agent-voice", speechRow(routing.CapabilityYes))
	offerSpeechModel(t, rs, routing.ProviderAudioCpp, audio, nil, "plain-voice", speechRow(routing.CapabilityYes))
	failing := offerSvc(&specReadStore{MemoryStore: rs, err: errors.New("spec read down"), calls: map[string]int{}}, nil)

	var byID map[string]ModelDTO
	captureSlog(t, func() {
		byID = modelsByID(failing.Models(ctx, auth.Token{UserID: "usr_1"}))
	})
	if byID["agent-voice"].Speech {
		t.Error("agent-voice speech = true, want false -- its spec could not be read, so the relay's flavor stage is unknown")
	}
	if !byID["plain-voice"].Speech {
		t.Error("plain-voice speech = false, want true -- an ordinary application has no spec to read")
	}
}
