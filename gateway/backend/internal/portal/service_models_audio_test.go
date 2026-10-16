// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/usage"
	"reflect"
	"slices"
	"testing"
)

// offerSpeechModel seeds a server + application of appType declaring appFlavors
// + one active mapping named gateway, with the given capability rows (none for
// a model that was never probed) and, when specFlavors is non-nil, a runtime
// spec carrying specFlavors -- the spec is a server_agent child's own flavor
// authority once resolved (routing.Resolver.targetFrom). It is the speech
// counterpart of offerModelImage and offerAgentModelImage, which seed the image
// verdict only.
func offerSpeechModel(t *testing.T, rs *routing.MemoryStore, appType string, appFlavors, specFlavors []string, gateway string, rows ...routing.CapabilityRow) {
	t.Helper()
	ctx := context.Background()
	srvID, appID, mappingID := "srv_"+gateway, "app_"+gateway, "map_"+gateway
	port := 8000
	if appType == routing.ProviderServerAgent {
		port = 8081
	}
	if err := rs.CreateAIServer(ctx, routing.AIServer{ID: srvID, Name: srvID, Domain: srvID + ".test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: offeringTime, UpdatedAt: offeringTime}); err != nil {
		t.Fatalf("CreateAIServer %s: %v", srvID, err)
	}
	if err := rs.CreateApplication(ctx, routing.Application{ID: appID, ServerID: srvID, Type: appType, Port: port, Scheme: "http", APIFlavors: appFlavors, Status: routing.ServerStatusActive, CreatedAt: offeringTime, UpdatedAt: offeringTime}); err != nil {
		t.Fatalf("CreateApplication %s: %v", appID, err)
	}
	if err := rs.CreateMapping(ctx, routing.ModelMapping{ID: mappingID, ApplicationID: appID, GatewayModelName: gateway, AppModelName: gateway, Status: routing.ServerStatusActive, CreatedAt: offeringTime, UpdatedAt: offeringTime}); err != nil {
		t.Fatalf("CreateMapping %s: %v", gateway, err)
	}
	if len(rows) > 0 {
		if err := rs.UpsertMappingCapabilities(ctx, mappingID, rows); err != nil {
			t.Fatalf("UpsertMappingCapabilities %s: %v", mappingID, err)
		}
	}
	if specFlavors != nil {
		if err := rs.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{ID: appID + "_spec", MappingID: mappingID, Binary: "/opt/audiocpp/audiocpp_server", Args: "[]", Env: "{}", APIFlavors: specFlavors}); err != nil {
			t.Fatalf("UpsertRuntimeSpec %s: %v", mappingID, err)
		}
	}
}

// speechRow is one speech capability row of the given verdict.
func speechRow(verdict string) routing.CapabilityRow {
	return routing.CapabilityRow{Capability: routing.CapabilitySpeech, Verdict: verdict, Source: routing.CapabilitySourceManual, CheckedAt: offeringTime}
}

// speechOfferingStore seeds one model per shape the speech gate tells apart,
// plus two groups, for the ModelOffering tests below. It is the speech twin of
// capableOfferingStore.
func speechOfferingStore(t *testing.T) *routing.MemoryStore {
	t.Helper()
	audio := []string{routing.APIFlavorOpenAIAudio}
	textAndAudio := []string{routing.APIFlavorOpenAI, routing.APIFlavorOpenAIAudio}
	textOnly := []string{routing.APIFlavorOpenAI}
	imageYes := routing.CapabilityRow{Capability: routing.CapabilityImage, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceManual, CheckedAt: offeringTime}
	rs := routing.NewMemoryStore()
	offerSpeechModel(t, rs, routing.ProviderAudioCpp, audio, nil, "tts-yes", speechRow(routing.CapabilityYes))
	offerSpeechModel(t, rs, routing.ProviderAudioCpp, audio, nil, "tts-no", speechRow(routing.CapabilityNo))
	offerSpeechModel(t, rs, routing.ProviderAudioCpp, audio, nil, "tts-unprobed")
	offerSpeechModel(t, rs, routing.ProviderAudioCpp, audio, nil, "tts-image-verdict-only", imageYes)
	offerSpeechModel(t, rs, routing.ProviderAudioCpp, textAndAudio, nil, "tts-also-text", speechRow(routing.CapabilityYes))
	offerSpeechModel(t, rs, routing.ProviderVLLM, textOnly, nil, "text-flavor", speechRow(routing.CapabilityYes))
	offerSpeechModel(t, rs, routing.ProviderServerAgent, textAndAudio, audio, "agent-both", speechRow(routing.CapabilityYes))
	offerSpeechModel(t, rs, routing.ProviderServerAgent, textAndAudio, textOnly, "agent-spec-narrows", speechRow(routing.CapabilityYes))
	offerSpeechModel(t, rs, routing.ProviderServerAgent, textAndAudio, nil, "agent-no-spec", speechRow(routing.CapabilityYes))
	offerGroup(t, rs, "grp_pure", "pure-speech-group", "tts-yes", "agent-both")
	offerGroup(t, rs, "grp_mixed", "mixed-speech-group", "tts-yes", "tts-no")
	return rs
}

// TestKnownAPIFlavorsListOpenAIAudio: perNameFlavors asks its rule about every
// entry, so a flavor missing here is a flavor no listing reports -- an audio-only
// model would reach the portal with no flavors at all, and callableModelNames
// (which unions the set) would refuse it as a model-valued token setting. The
// set is documented as sorted, so openai_audio sits between openai and
// openai_images.
func TestKnownAPIFlavorsListOpenAIAudio(t *testing.T) {
	if !slices.Contains(knownAPIFlavors, routing.APIFlavorOpenAIAudio) {
		t.Fatalf("knownAPIFlavors = %v, want it to contain %q", knownAPIFlavors, routing.APIFlavorOpenAIAudio)
	}
	if !slices.IsSorted(knownAPIFlavors) {
		t.Fatalf("knownAPIFlavors = %v, want the documented sorted order", knownAPIFlavors)
	}
	for _, kept := range []string{routing.APIFlavorAnthropic, routing.APIFlavorOpenAI, routing.APIFlavorOpenAIImages} {
		if !slices.Contains(knownAPIFlavors, kept) {
			t.Fatalf("knownAPIFlavors = %v, lost %q", knownAPIFlavors, kept)
		}
	}
	// The seed fallback stays text-only: audio is opt-in everywhere.
	if isSeedAPIFlavor(routing.APIFlavorOpenAIAudio) {
		t.Fatalf("seedAPIFlavors = %v, want no openai_audio: a seed that claimed speech would be offered for requests it cannot serve", seedAPIFlavors)
	}
}

// TestModelsListsAnAudioOnlyModelWithItsFlavor: the listing carries
// openai_audio through for an audio-only model, which the OpenAI chat listing
// keeps out (an audio-only model offered for chat would fail) while the
// openai_audio listing names it.
func TestModelsListsAnAudioOnlyModelWithItsFlavor(t *testing.T) {
	ctx := context.Background()
	rs := routing.NewMemoryStore()
	offerSpeechModel(t, rs, routing.ProviderAudioCpp, []string{routing.APIFlavorOpenAIAudio}, nil, "pocket-tts", speechRow(routing.CapabilityYes))
	offerModel(t, rs, "srv_text", "TextBox", "app_text", []string{routing.APIFlavorOpenAI}, "qwen3-32b", "qwen3-32b", routing.ServerStatusActive)
	svc := offerSvc(rs, nil)
	token := auth.Token{UserID: "usr_1"}

	byID := modelsByID(svc.Models(ctx, token))
	tts, ok := byID["pocket-tts"]
	if !ok {
		t.Fatalf("pocket-tts missing from Models(): %#v", byID)
	}
	if !reflect.DeepEqual(tts.Flavors, []string{routing.APIFlavorOpenAIAudio}) {
		t.Fatalf("pocket-tts flavors = %#v, want exactly [openai_audio]", tts.Flavors)
	}
	if got := svc.ModelsForFlavor(ctx, token, routing.APIFlavorOpenAI); !reflect.DeepEqual(got, []string{"qwen3-32b"}) {
		t.Fatalf("ModelsForFlavor(openai) = %#v, want [qwen3-32b] -- no audio-only model", got)
	}
	if got := svc.ModelsForFlavor(ctx, token, routing.APIFlavorOpenAIAudio); !reflect.DeepEqual(got, []string{"pocket-tts"}) {
		t.Fatalf("ModelsForFlavor(openai_audio) = %#v, want [pocket-tts]", got)
	}
	callable := svc.callableModelNames(ctx, token)
	for _, name := range []string{"pocket-tts", "qwen3-32b"} {
		if _, ok := callable[name]; !ok {
			t.Errorf("callableModelNames = %#v, want it to contain %s: a name valid for only one flavor is still a valid setting", callable, name)
		}
	}
}

// TestModelOfferingSpeechCapableIsGatedOnTheSpeechVerdict is the load-bearing
// pin of the speech gate: the openai_audio flavor is offered as a redirect
// CANDIDATE (ModelOffering.Capable, asked with [speech]) only behind a
// (speech, yes) verdict, on top of the flavor halves -- the application's
// openai_audio and, for an agent child, the spec's. A model with no verdict, a
// "no", or a different capability's verdict is callable for the flavor and is
// still never a candidate, which keeps the redirect from turning a legible
// unknown-model answer into a model_not_capable 404 about a model the client
// never named.
func TestModelOfferingSpeechCapableIsGatedOnTheSpeechVerdict(t *testing.T) {
	ctx := context.Background()
	svc := offerSvc(speechOfferingStore(t), nil)
	token := auth.Token{UserID: "usr_1"}

	off := svc.ModelOfferingFor(ctx, token, routing.APIFlavorOpenAIAudio, []string{routing.CapabilitySpeech})

	want := map[string]bool{
		"tts-yes":                true,
		"tts-no":                 false, // callable for the flavor, but its verdict is no
		"tts-unprobed":           false, // callable for the flavor, but never probed: unknown is not capable
		"tts-image-verdict-only": false, // another capability's yes is not speech
		"tts-also-text":          true,  // audio next to a text flavor still serves speech
		"agent-both":             true,
		"agent-no-spec":          true, // no spec: the application is the authority
		"pure-speech-group":      true,
		"mixed-speech-group":     false, // one member lacks the verdict
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
		if _, ok := off.Callable[name]; ok {
			t.Errorf("%s is in Callable(openai_audio), want not: it fails the openai_audio flavor half", name)
		}
		if _, ok := off.Existing[name]; ok {
			t.Errorf("%s is in Existing(openai_audio), want not: it fails the openai_audio flavor half", name)
		}
		if _, ok := off.Capable[name]; ok {
			t.Errorf("%s is capable, want not", name)
		}
	}
	for name := range off.Capable {
		if _, ok := off.Callable[name]; !ok {
			t.Errorf("%s is Capable but not Callable: Capable must be a subset", name)
		}
	}
}

// With no required capability Capable is Callable, and a capability that has no
// fold (or one that does beside one that does not) is carried by nothing:
// fail-closed, while Callable and Existing are unaffected.
func TestModelOfferingSpeechCapableWithoutAFoldOrWithNoRequirement(t *testing.T) {
	ctx := context.Background()
	svc := offerSvc(speechOfferingStore(t), nil)
	token := auth.Token{UserID: "usr_1"}

	none := svc.ModelOfferingFor(ctx, token, routing.APIFlavorOpenAIAudio, nil)
	if !reflect.DeepEqual(none.Capable, none.Callable) {
		t.Fatalf("Capable = %#v, want Callable %#v with no required capability", none.Capable, none.Callable)
	}
	for _, required := range [][]string{{"teleportation"}, {routing.CapabilitySpeech, "teleportation"}} {
		off := svc.ModelOfferingFor(ctx, token, routing.APIFlavorOpenAIAudio, required)
		if len(off.Capable) != 0 {
			t.Errorf("Capable(%v) = %#v, want empty for a capability without a fold", required, off.Capable)
		}
		if len(off.Callable) == 0 || len(off.Existing) == 0 {
			t.Errorf("Callable/Existing(%v) = %#v/%#v, want both populated", required, off.Callable, off.Existing)
		}
	}
}

// A speech verdict does not make a model a candidate for another capability:
// the image fold stays the image fold, and an image request's candidates are
// still decided by openai_images and the image verdict alone.
func TestModelOfferingSpeechVerdictDoesNotLeakIntoTheImageFold(t *testing.T) {
	ctx := context.Background()
	svc := offerSvc(speechOfferingStore(t), nil)
	token := auth.Token{UserID: "usr_1"}

	if off := svc.ModelOfferingFor(ctx, token, routing.APIFlavorOpenAIImages, []string{routing.CapabilityImage}); len(off.Capable) != 0 {
		t.Fatalf("Capable(openai_images, [image]) = %#v, want empty: no seeded model serves openai_images", off.Capable)
	}
	if off := svc.ModelOfferingFor(ctx, token, routing.APIFlavorOpenAIAudio, []string{routing.CapabilityImage}); len(off.Capable) != 0 {
		t.Fatalf("Capable(openai_audio, [image]) = %#v, want empty: the image fold needs openai_images, so a speech model's image verdict offers nothing", off.Capable)
	}
}

// A failed spec read must not be read as "no spec" for the speech fold, which
// would fall back to the application's flavors and report an agent child
// capable. It drops that application's mappings from Capable only, while an
// ordinary audio application beside it is unaffected, and Callable fails open
// to the application's own flavors, the same split the image fold makes.
func TestModelOfferingSpeechWithholdsAnAgentModelWhenItsSpecReadFails(t *testing.T) {
	ctx := context.Background()
	audio := []string{routing.APIFlavorOpenAIAudio}
	textAndAudio := []string{routing.APIFlavorOpenAI, routing.APIFlavorOpenAIAudio}
	rs := routing.NewMemoryStore()
	offerSpeechModel(t, rs, routing.ProviderServerAgent, textAndAudio, audio, "agent-voice", speechRow(routing.CapabilityYes))
	offerSpeechModel(t, rs, routing.ProviderAudioCpp, audio, nil, "plain-voice", speechRow(routing.CapabilityYes))
	store := &specReadStore{MemoryStore: rs, err: errors.New("spec read down"), calls: map[string]int{}}
	svc := offerSvc(store, nil)

	var off ModelOffering
	logged := captureSlog(t, func() {
		off = svc.ModelOfferingFor(ctx, auth.Token{UserID: "usr_1"}, routing.APIFlavorOpenAIAudio, []string{routing.CapabilitySpeech})
	})
	if _, ok := off.Callable["agent-voice"]; !ok {
		t.Errorf("Callable lacks agent-voice, want it: the application declares openai_audio and the spec is unreadable (fail-open): %#v", off.Callable)
	}
	if want := map[string]struct{}{"plain-voice": {}}; !reflect.DeepEqual(off.Capable, want) {
		t.Errorf("Capable = %#v, want %#v -- the speech fold fails closed for the unreadable application", off.Capable, want)
	}
	if logged == "" {
		t.Error("the spec-read degrade was not logged")
	}
}

// A failed capability read gives the all-or-nothing empty offering, so the
// redirect declines instead of guessing which candidate synthesises speech.
func TestModelOfferingSpeechIsEmptyWhenTheCapabilityReadFails(t *testing.T) {
	ctx := context.Background()
	store := &capabilityReadStore{MemoryStore: speechOfferingStore(t), err: errors.New("capability read down")}
	svc := offerSvc(store, nil)

	off := svc.ModelOfferingFor(ctx, auth.Token{UserID: "usr_1"}, routing.APIFlavorOpenAIAudio, []string{routing.CapabilitySpeech})
	if len(off.Callable) != 0 || len(off.Capable) != 0 || len(off.Existing) != 0 {
		t.Fatalf("offering = %#v, want every set empty on a failed capability read", off)
	}
}

// With no routing store the seed models answer, and they are text models: no
// seed is a speech candidate, callable or existing under openai_audio.
func TestSeedModelsAreNeverSpeechCandidates(t *testing.T) {
	svc := NewService(ServiceDeps{Usage: usage.NewRecorder()})
	token := auth.Token{UserID: "usr_1"}

	if got := svc.ModelsForFlavor(context.Background(), token, routing.APIFlavorOpenAIAudio); len(got) != 0 {
		t.Fatalf("ModelsForFlavor(openai_audio) with no store = %#v, want none", got)
	}
	off := svc.ModelOfferingFor(context.Background(), token, routing.APIFlavorOpenAIAudio, []string{routing.CapabilitySpeech})
	if len(off.Callable) != 0 || len(off.Capable) != 0 || len(off.Existing) != 0 {
		t.Fatalf("ModelOfferingFor(openai_audio) with no store = %#v, want every set empty", off)
	}
}
