// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"reflect"
	"strings"
	"testing"
)

// transcriptionRow is one transcription capability row of the given verdict, the
// twin of speechRow.
func transcriptionRow(verdict string) routing.CapabilityRow {
	return routing.CapabilityRow{Capability: routing.CapabilityTranscription, Verdict: verdict, Source: routing.CapabilitySourceManual, CheckedAt: offeringTime}
}

// transcriptionOfferingStore seeds one model per shape the transcription fold
// tells apart, plus two groups. It is speechOfferingStore's twin: the flavor
// half is the same openai_audio rule, only the verdict row differs, so every
// shape speech pins is pinned again here for transcription.
func transcriptionOfferingStore(t *testing.T) *routing.MemoryStore {
	t.Helper()
	audio := []string{routing.APIFlavorOpenAIAudio}
	textAndAudio := []string{routing.APIFlavorOpenAI, routing.APIFlavorOpenAIAudio}
	textOnly := []string{routing.APIFlavorOpenAI}
	rs := routing.NewMemoryStore()
	offerSpeechModel(t, rs, routing.ProviderAudioCpp, audio, nil, "asr-yes", transcriptionRow(routing.CapabilityYes))
	offerSpeechModel(t, rs, routing.ProviderAudioCpp, audio, nil, "asr-no", transcriptionRow(routing.CapabilityNo))
	offerSpeechModel(t, rs, routing.ProviderAudioCpp, audio, nil, "asr-unprobed")
	offerSpeechModel(t, rs, routing.ProviderAudioCpp, audio, nil, "asr-speech-verdict-only", speechRow(routing.CapabilityYes))
	offerSpeechModel(t, rs, routing.ProviderAudioCpp, textAndAudio, nil, "asr-also-text", transcriptionRow(routing.CapabilityYes))
	offerSpeechModel(t, rs, routing.ProviderVLLM, textOnly, nil, "text-flavor", transcriptionRow(routing.CapabilityYes))
	offerSpeechModel(t, rs, routing.ProviderServerAgent, textAndAudio, audio, "agent-both", transcriptionRow(routing.CapabilityYes))
	offerSpeechModel(t, rs, routing.ProviderServerAgent, textAndAudio, textOnly, "agent-spec-narrows", transcriptionRow(routing.CapabilityYes))
	offerSpeechModel(t, rs, routing.ProviderServerAgent, textAndAudio, nil, "agent-no-spec", transcriptionRow(routing.CapabilityYes))
	offerGroup(t, rs, "grp_pure_asr", "pure-asr-group", "asr-yes", "agent-both")
	offerGroup(t, rs, "grp_mixed_asr", "mixed-asr-group", "asr-yes", "asr-no")
	return rs
}

// ModelDTO.Transcription is the listing's transcription flag, speech's twin: the
// transcription fold (transcriptionFlagsByName) ANDed across a model's mappings,
// so the portal chat can switch its composer to a transcription turn only for a
// model the transcription endpoint would serve. Every shape the fold tells apart
// is pinned, because each is a distinct decision.
func TestModelsResponseTranscriptionFlag(t *testing.T) {
	ctx := context.Background()
	svc := offerSvc(transcriptionOfferingStore(t), nil)
	byID := modelsByID(svc.Models(ctx, auth.Token{UserID: "usr_1"}))

	for _, tc := range []struct {
		name          string
		transcription bool
	}{
		{"asr-yes", true},
		{"asr-no", false},                  // an explicit (transcription, no) verdict
		{"asr-unprobed", false},            // never probed: unknown is not capable
		{"asr-speech-verdict-only", false}, // another capability's yes is not transcription
		{"asr-also-text", true},            // audio next to a text flavor still serves transcription
		{"text-flavor", false},             // transcription:yes, but no openai_audio on the application
		{"agent-both", true},               // spec and application both declare openai_audio
		{"agent-spec-narrows", false},      // the spec drops openai_audio the application declares
		{"agent-no-spec", true},            // no spec: the application is the flavor authority
		{"pure-asr-group", true},           // every offerable member qualifies
		{"mixed-asr-group", false},         // one member lacks the verdict
	} {
		got, ok := byID[tc.name]
		if !ok {
			t.Fatalf("%s missing from Models(): %#v", tc.name, byID)
		}
		if got.Transcription != tc.transcription {
			t.Errorf("%s transcription = %v, want %v", tc.name, got.Transcription, tc.transcription)
		}
	}
}

// Transcription, Speech, Image and Vision are four independent axes. A pure-ASR
// mapping (transcription:yes, speech:no) folds transcription=true, speech=false;
// a pure-TTS mapping the reverse; a model with both verdicts is both. Another
// capability's yes verdict never makes a model a transcription candidate.
func TestModelsResponseTranscriptionIsIndependentOfSpeechImageAndVision(t *testing.T) {
	ctx := context.Background()
	rs := routing.NewMemoryStore()
	audio := []string{routing.APIFlavorOpenAIAudio}
	offerSpeechModel(t, rs, routing.ProviderAudioCpp, audio, nil, "pure-asr", transcriptionRow(routing.CapabilityYes), speechRow(routing.CapabilityNo))
	offerSpeechModel(t, rs, routing.ProviderAudioCpp, audio, nil, "pure-tts", speechRow(routing.CapabilityYes), transcriptionRow(routing.CapabilityNo))
	offerSpeechModel(t, rs, routing.ProviderAudioCpp, audio, nil, "asr-and-tts", transcriptionRow(routing.CapabilityYes), speechRow(routing.CapabilityYes))
	offerModelImage(t, rs, "srv_sd", "SdBox", "app_sd", imageServingFlavors, "sd", "sd-up", true)
	offerModelVision(t, rs, "srv_vis", "VisBox", "app_vis", []string{routing.APIFlavorOpenAI}, "vis", "vis-up", true)
	offerModel(t, rs, "srv_text", "TextBox", "app_text", []string{routing.APIFlavorOpenAI}, "qwen3-32b", "qwen3-32b", routing.ServerStatusActive)
	byID := modelsByID(offerSvc(rs, nil).Models(ctx, auth.Token{UserID: "usr_1"}))

	for _, tc := range []struct {
		name                                  string
		transcription, speech, image, visible bool
	}{
		{"pure-asr", true, false, false, false},
		{"pure-tts", false, true, false, false},
		{"asr-and-tts", true, true, false, false},
		{"sd", false, false, true, false}, // image:yes is not transcription
		{"vis", false, false, false, true},
		{"qwen3-32b", false, false, false, false},
	} {
		got, ok := byID[tc.name]
		if !ok {
			t.Fatalf("%s missing from Models(): %#v", tc.name, byID)
		}
		if got.Transcription != tc.transcription || got.Speech != tc.speech || got.Image != tc.image || got.Vision != tc.visible {
			t.Errorf("%s transcription=%v speech=%v image=%v vision=%v, want transcription=%v speech=%v image=%v vision=%v",
				tc.name, got.Transcription, got.Speech, got.Image, got.Vision, tc.transcription, tc.speech, tc.image, tc.visible)
		}
	}
}

// The flag is an AND across a model's mappings, fail-closed, like speech: a model
// on server A (transcription:yes) and server B (transcription:no or never
// probed) is advertised false, the safe direction. "both-yes" pins the
// fail-closed SEED: two yes mappings only reach true because the accumulator
// starts true on a model's first view.
func TestModelsResponseTranscriptionAndAcrossMappings(t *testing.T) {
	ctx := context.Background()
	audio := []string{routing.APIFlavorOpenAIAudio}
	rs := routing.NewMemoryStore()
	// offerSpeechModel derives its server, application and mapping ids from the
	// gateway name, so it cannot seed two mappings of ONE name, which is what the
	// per-name AND needs.
	offerSharedMapping := func(srvID, appID, mappingID, gateway string, rows ...routing.CapabilityRow) {
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

	offerSharedMapping("srv_a", "app_a", "map_a", "split", transcriptionRow(routing.CapabilityYes))
	offerSharedMapping("srv_b", "app_b", "map_b", "split", transcriptionRow(routing.CapabilityNo))
	offerSharedMapping("srv_c", "app_c", "map_c", "unprobed-second", transcriptionRow(routing.CapabilityYes))
	offerSharedMapping("srv_d", "app_d", "map_d", "unprobed-second")
	offerSharedMapping("srv_e", "app_e", "map_e", "both-yes", transcriptionRow(routing.CapabilityYes))
	offerSharedMapping("srv_f", "app_f", "map_f", "both-yes", transcriptionRow(routing.CapabilityYes))
	byID := modelsByID(offerSvc(rs, nil).Models(ctx, auth.Token{UserID: "usr_1"}))

	if byID["split"].Transcription {
		t.Error("split transcription = true, want false -- one mapping says no, and the fold is an AND")
	}
	if byID["unprobed-second"].Transcription {
		t.Error("unprobed-second transcription = true, want false -- a never-probed mapping ANDs in as false")
	}
	if !byID["both-yes"].Transcription {
		t.Error("both-yes transcription = false, want true -- the fail-closed seed must let two yes mappings reach true")
	}
}

// An override alias carries its target's Transcription flag under the alias
// name, like the Speech flag it sits beside: the alias redirect
// (inheritTargetRow in service.go) wires each flag in on its own, so a build that
// wired only the older flags would leave transcription silently false behind
// every alias and an alias onto an ASR model would never open the chat's
// transcription composer.
func TestModelsResponseTranscriptionAliasRedirect(t *testing.T) {
	ctx := context.Background()
	svc := offerSvc(transcriptionOfferingStore(t), nil)
	token := tokenWithRules(map[string]store.ModelOverrideRule{
		"alias-listen": {To: "asr-yes", Offer: true},
		"alias-deaf":   {To: "asr-no", Offer: true},
	})
	byID := modelsByID(svc.Models(ctx, token))

	for name, want := range map[string]bool{"alias-listen": true, "alias-deaf": false} {
		got, ok := byID[name]
		if !ok {
			t.Fatalf("%s missing from Models(): %#v", name, byID)
		}
		if got.Transcription != want {
			t.Errorf("%s transcription = %v, want %v (its target's flag)", name, got.Transcription, want)
		}
	}
}

// The listing's Transcription flag and ModelOffering.Capable for a transcription
// request are one fold (transcriptionFlagsByName): every callable name is
// capable exactly when the listing says Transcription, so the chat cannot offer
// a transcription turn the redirect would not, nor the reverse. The explicit
// expectations pin each shape so agreement cannot come from both sides being
// wrong together.
func TestModelsTranscriptionIsTheOfferingsTranscriptionFold(t *testing.T) {
	ctx := context.Background()
	svc := offerSvc(transcriptionOfferingStore(t), nil)
	token := auth.Token{UserID: "usr_1"}

	off := svc.ModelOfferingFor(ctx, token, routing.APIFlavorOpenAIAudio, []string{routing.CapabilityTranscription})
	byID := modelsByID(svc.Models(ctx, token))
	if len(off.Callable) == 0 {
		t.Fatal("precondition: Callable(openai_audio) is empty")
	}
	for name := range off.Callable {
		if _, got := off.Capable[name]; got != byID[name].Transcription {
			t.Errorf("%s capable = %v, listing Transcription = %v -- the two must be one fold", name, got, byID[name].Transcription)
		}
	}

	want := map[string]bool{
		"asr-yes":                 true,
		"asr-no":                  false, // callable for the flavor, but its verdict is no
		"asr-unprobed":            false, // callable for the flavor, but never probed
		"asr-speech-verdict-only": false, // another capability's yes is not transcription
		"asr-also-text":           true,
		"agent-both":              true,
		"agent-no-spec":           true,
		"pure-asr-group":          true,
		"mixed-asr-group":         false,
	}
	for name, capable := range want {
		if _, ok := off.Callable[name]; !ok {
			t.Errorf("%s missing from Callable(openai_audio): %#v", name, off.Callable)
			continue
		}
		if _, got := off.Capable[name]; got != capable {
			t.Errorf("%s capable = %v, want %v", name, got, capable)
		}
	}
	// text-flavor's application does not declare openai_audio; agent-spec-narrows'
	// spec does not. Both fail the flavor half, however capable their verdict.
	for _, name := range []string{"text-flavor", "agent-spec-narrows"} {
		if _, ok := off.Capable[name]; ok {
			t.Errorf("%s is capable, want not: it fails the openai_audio flavor half", name)
		}
	}
	for name := range off.Capable {
		if _, ok := off.Callable[name]; !ok {
			t.Errorf("%s is Capable but not Callable: Capable must be a subset", name)
		}
	}
}

// A request requiring both audio capabilities gets the intersection of the two
// folds, and a speech request never offers a transcription-only model (nor the
// reverse): the two verdicts are separate rows read by separate folds.
func TestModelOfferingTranscriptionAndSpeechFoldsAreIndependent(t *testing.T) {
	ctx := context.Background()
	audio := []string{routing.APIFlavorOpenAIAudio}
	rs := routing.NewMemoryStore()
	offerSpeechModel(t, rs, routing.ProviderAudioCpp, audio, nil, "pure-asr", transcriptionRow(routing.CapabilityYes), speechRow(routing.CapabilityNo))
	offerSpeechModel(t, rs, routing.ProviderAudioCpp, audio, nil, "pure-tts", speechRow(routing.CapabilityYes), transcriptionRow(routing.CapabilityNo))
	offerSpeechModel(t, rs, routing.ProviderAudioCpp, audio, nil, "asr-and-tts", transcriptionRow(routing.CapabilityYes), speechRow(routing.CapabilityYes))
	svc := offerSvc(rs, nil)
	token := auth.Token{UserID: "usr_1"}

	for _, tc := range []struct {
		required []string
		want     map[string]struct{}
	}{
		{[]string{routing.CapabilityTranscription}, map[string]struct{}{"pure-asr": {}, "asr-and-tts": {}}},
		{[]string{routing.CapabilitySpeech}, map[string]struct{}{"pure-tts": {}, "asr-and-tts": {}}},
		{[]string{routing.CapabilityTranscription, routing.CapabilitySpeech}, map[string]struct{}{"asr-and-tts": {}}},
	} {
		off := svc.ModelOfferingFor(ctx, token, routing.APIFlavorOpenAIAudio, tc.required)
		if !reflect.DeepEqual(off.Capable, tc.want) {
			t.Errorf("Capable(%v) = %#v, want %#v", tc.required, off.Capable, tc.want)
		}
	}
}

// A transcription verdict does not make a model a candidate for another
// capability: the image fold stays the image fold.
func TestModelOfferingTranscriptionVerdictDoesNotLeakIntoTheImageFold(t *testing.T) {
	ctx := context.Background()
	svc := offerSvc(transcriptionOfferingStore(t), nil)
	token := auth.Token{UserID: "usr_1"}

	if off := svc.ModelOfferingFor(ctx, token, routing.APIFlavorOpenAIAudio, []string{routing.CapabilityImage}); len(off.Capable) != 0 {
		t.Fatalf("Capable(openai_audio, [image]) = %#v, want empty: a transcription model's verdict offers nothing to the image fold", off.Capable)
	}
}

// The admin management listing (ManageModels, ?manage=1) is built by the same
// function and carries the same flag.
func TestManageModelsCarriesTranscription(t *testing.T) {
	svc := offerSvc(transcriptionOfferingStore(t), nil)
	byID := modelsByID(svc.ManageModels(context.Background(), auth.Token{UserID: "usr_1"}))
	if !byID["asr-yes"].Transcription {
		t.Error("ManageModels asr-yes transcription = false, want true")
	}
	if byID["asr-no"].Transcription {
		t.Error("ManageModels asr-no transcription = true, want false")
	}
}

// A failed capability read degrades the listing to transcription=false for every
// model (fail-closed, never fail-open) and says so, naming transcription among
// the withheld flags: the same contract vision, image, image_edit and speech
// already carry.
func TestModelsTranscriptionIsWithheldWhenTheCapabilityReadFails(t *testing.T) {
	ctx := context.Background()
	rs := transcriptionOfferingStore(t)

	working := offerSvc(rs, nil)
	if !modelsByID(working.Models(ctx, auth.Token{UserID: "usr_1"}))["asr-yes"].Transcription {
		t.Fatal("precondition: asr-yes transcription = false with a WORKING capability read, want true")
	}

	failing := offerSvc(&capabilityReadStore{MemoryStore: rs, err: errors.New("capability table unavailable")}, nil)
	var byID map[string]ModelDTO
	logged := captureSlog(t, func() {
		byID = modelsByID(failing.Models(ctx, auth.Token{UserID: "usr_1"}))
	})
	model, ok := byID["asr-yes"]
	if !ok {
		t.Fatalf("asr-yes missing from the listing: the capability read must DEGRADE, not drop models (%#v)", byID)
	}
	if model.Transcription {
		t.Error("asr-yes transcription = true after a failed capability read, want false (fail-closed)")
	}
	if !strings.Contains(logged, "capability read failed") || !strings.Contains(logged, "transcription") {
		t.Errorf("log = %q, want the degrade logged with transcription named among the withheld flags", logged)
	}
}

// A failed spec read must not be read as "no spec" for the transcription flag,
// which would fall back to the application's flavors and report an agent child
// capable. It withholds transcription for that application's models only; an
// ordinary audio application beside it is unaffected, since it has no spec to
// read.
func TestModelsTranscriptionWithholdsAnAgentModelWhenItsSpecReadFails(t *testing.T) {
	ctx := context.Background()
	audio := []string{routing.APIFlavorOpenAIAudio}
	textAndAudio := []string{routing.APIFlavorOpenAI, routing.APIFlavorOpenAIAudio}
	rs := routing.NewMemoryStore()
	offerSpeechModel(t, rs, routing.ProviderServerAgent, textAndAudio, audio, "agent-asr", transcriptionRow(routing.CapabilityYes))
	offerSpeechModel(t, rs, routing.ProviderAudioCpp, audio, nil, "plain-asr", transcriptionRow(routing.CapabilityYes))
	failing := offerSvc(&specReadStore{MemoryStore: rs, err: errors.New("spec read down"), calls: map[string]int{}}, nil)

	var byID map[string]ModelDTO
	captureSlog(t, func() {
		byID = modelsByID(failing.Models(ctx, auth.Token{UserID: "usr_1"}))
	})
	if byID["agent-asr"].Transcription {
		t.Error("agent-asr transcription = true, want false -- its spec could not be read, so the relay's flavor stage is unknown")
	}
	if !byID["plain-asr"].Transcription {
		t.Error("plain-asr transcription = false, want true -- an ordinary application has no spec to read")
	}
}

// The same failures drop the unknown-model redirect's candidates: a failed
// capability read gives the all-or-nothing empty offering, and a failed spec read
// withholds the agent model from Capable only (Callable fails open).
func TestModelOfferingTranscriptionFailsClosedOnReadFailures(t *testing.T) {
	ctx := context.Background()
	token := auth.Token{UserID: "usr_1"}
	required := []string{routing.CapabilityTranscription}

	capFail := offerSvc(&capabilityReadStore{MemoryStore: transcriptionOfferingStore(t), err: errors.New("capability read down")}, nil)
	off := capFail.ModelOfferingFor(ctx, token, routing.APIFlavorOpenAIAudio, required)
	if len(off.Callable) != 0 || len(off.Capable) != 0 || len(off.Existing) != 0 {
		t.Errorf("offering = %#v, want every set empty on a failed capability read", off)
	}

	audio := []string{routing.APIFlavorOpenAIAudio}
	textAndAudio := []string{routing.APIFlavorOpenAI, routing.APIFlavorOpenAIAudio}
	rs := routing.NewMemoryStore()
	offerSpeechModel(t, rs, routing.ProviderServerAgent, textAndAudio, audio, "agent-asr", transcriptionRow(routing.CapabilityYes))
	offerSpeechModel(t, rs, routing.ProviderAudioCpp, audio, nil, "plain-asr", transcriptionRow(routing.CapabilityYes))
	specFail := offerSvc(&specReadStore{MemoryStore: rs, err: errors.New("spec read down"), calls: map[string]int{}}, nil)
	captureSlog(t, func() {
		off = specFail.ModelOfferingFor(ctx, token, routing.APIFlavorOpenAIAudio, required)
	})
	if _, ok := off.Callable["agent-asr"]; !ok {
		t.Errorf("Callable lacks agent-asr, want it (fail-open on the application's own flavors): %#v", off.Callable)
	}
	if want := map[string]struct{}{"plain-asr": {}}; !reflect.DeepEqual(off.Capable, want) {
		t.Errorf("Capable = %#v, want %#v -- the transcription fold fails closed for the unreadable application", off.Capable, want)
	}
}
