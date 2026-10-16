// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"op-ai-gateway/internal/routing"
	"strings"
	"testing"
)

// The audio_only marker is the speech twin of images_only
// (service_mapping_images_only_test.go) and shares its fixtures: the
// instrumented spec store, the application / mapping / spec seeders and the
// read counters. The two markers come out of one spec read, so every listing
// here also pins that a server_agent application still costs exactly one
// RuntimeSpecsByApplication read, and a create or update one
// RuntimeSpecByMapping read, with both markers filled.

// listFlavorMarkers lists appID's mappings and returns each one's markers.
func listFlavorMarkers(t *testing.T, svc *Service, appID string) map[string]mappingFlavorMarkers {
	t.Helper()
	list, err := svc.ListMappings(context.Background(), ownerToken(), appID)
	if err != nil {
		t.Fatalf("ListMappings: %v", err)
	}
	got := make(map[string]mappingFlavorMarkers, len(list.Data))
	for _, dto := range list.Data {
		got[dto.ID] = mappingFlavorMarkers{ImagesOnly: dto.ImagesOnly, AudioOnly: dto.AudioOnly}
	}
	return got
}

// TestListMappingsAudioOnlyFollowsTheEffectiveFlavors: for a server_agent
// application the spec's flavors decide whenever the mapping has a spec, one
// stored with no flavors included, and the application's otherwise, exactly as
// images_only does. Each listing costs one RuntimeSpecsByApplication read.
func TestListMappingsAudioOnlyFollowsTheEffectiveFlavors(t *testing.T) {
	svc, routes := newImagesOnlyFixture(t)
	audio := []string{routing.APIFlavorOpenAIAudio}
	text := []string{routing.APIFlavorOpenAI}
	both := seedImagesOnlyApp(t, svc, routes, routing.ProviderServerAgent, []string{routing.APIFlavorOpenAI, routing.APIFlavorOpenAIAudio})
	specAudio := seedImagesOnlyMapping(t, routes, both.ID, "spec-audio")
	putImagesOnlySpec(t, routes, specAudio, audio)
	specText := seedImagesOnlyMapping(t, routes, both.ID, "spec-text")
	putImagesOnlySpec(t, routes, specText, text)
	bothNoSpec := seedImagesOnlyMapping(t, routes, both.ID, "both-no-spec")
	audioApp := seedImagesOnlyApp(t, svc, routes, routing.ProviderServerAgent, audio)
	audioNoSpec := seedImagesOnlyMapping(t, routes, audioApp.ID, "audio-no-spec")
	specEmpty := seedImagesOnlyMapping(t, routes, audioApp.ID, "spec-empty")
	putImagesOnlySpec(t, routes, specEmpty, []string{})

	for _, tc := range []struct {
		appID string
		want  map[string]bool
	}{
		{appID: both.ID, want: map[string]bool{specAudio: true, specText: false, bothNoSpec: false}},
		{appID: audioApp.ID, want: map[string]bool{audioNoSpec: true, specEmpty: false}},
	} {
		routes.byApp, routes.byMapping = 0, 0
		got := listFlavorMarkers(t, svc, tc.appID)
		for id, want := range tc.want {
			if got[id].AudioOnly != want {
				t.Errorf("%s audio_only = %v, want %v", id, got[id].AudioOnly, want)
			}
			if got[id].ImagesOnly {
				t.Errorf("%s images_only = true, want false: no mapping here lists openai_images", id)
			}
		}
		if routes.byApp != 1 || routes.byMapping != 0 {
			t.Errorf("%s spec reads = (by application %d, by mapping %d), want (1, 0)", tc.appID, routes.byApp, routes.byMapping)
		}
	}
}

// TestListMappingsFlavorMarkersAreIndependent: each marker names its own
// non-text flavor and neither text flavor, and they are separate answers. An
// application that lists both non-text flavors is both images-only and
// audio-only (routing.FlavorsAreAudioOnly says so), and one that lists a text
// flavor beside openai_audio is neither. No spec is read for a non-agent
// application.
func TestListMappingsFlavorMarkersAreIndependent(t *testing.T) {
	svc, routes := newImagesOnlyFixture(t)
	for _, tc := range []struct {
		name    string
		appType string
		flavors []string
		want    mappingFlavorMarkers
	}{
		{"audio only", routing.ProviderAudioCpp, []string{routing.APIFlavorOpenAIAudio}, mappingFlavorMarkers{AudioOnly: true}},
		{"images only", routing.ProviderStableDiffusionCpp, []string{routing.APIFlavorOpenAIImages}, mappingFlavorMarkers{ImagesOnly: true}},
		{"both non-text flavors", routing.ProviderAudioCpp, []string{routing.APIFlavorOpenAIImages, routing.APIFlavorOpenAIAudio}, mappingFlavorMarkers{ImagesOnly: true, AudioOnly: true}},
		{"text beside audio", routing.ProviderAudioCpp, []string{routing.APIFlavorOpenAI, routing.APIFlavorOpenAIAudio}, mappingFlavorMarkers{}},
		{"anthropic beside audio", routing.ProviderAudioCpp, []string{routing.APIFlavorAnthropic, routing.APIFlavorOpenAIAudio}, mappingFlavorMarkers{}},
		{"text only", routing.ProviderVLLM, []string{routing.APIFlavorOpenAI}, mappingFlavorMarkers{}},
	} {
		app := seedImagesOnlyApp(t, svc, routes, tc.appType, tc.flavors)
		id := seedImagesOnlyMapping(t, routes, app.ID, strings.ReplaceAll(tc.name, " ", "-"))
		routes.byApp, routes.byMapping = 0, 0
		if got := listFlavorMarkers(t, svc, app.ID)[id]; got != tc.want {
			t.Errorf("%s: markers = %+v, want %+v", tc.name, got, tc.want)
		}
		if routes.byApp != 0 || routes.byMapping != 0 {
			t.Errorf("%s: spec reads = (by application %d, by mapping %d), want none for a non-agent application", tc.name, routes.byApp, routes.byMapping)
		}
	}
}

// TestListMappingsAudioOnlyIsFalseWhenTheSpecReadFails: a failed spec read
// reports false for BOTH markers, even where the application's own flavors are
// audio-only, and the listing itself still succeeds. The WARN it logs is
// captured so the test's own output stays pristine.
func TestListMappingsAudioOnlyIsFalseWhenTheSpecReadFails(t *testing.T) {
	svc, routes := newImagesOnlyFixture(t)
	app := seedImagesOnlyApp(t, svc, routes, routing.ProviderServerAgent, []string{routing.APIFlavorOpenAIAudio})
	mappingID := seedImagesOnlyMapping(t, routes, app.ID, "voice")
	putImagesOnlySpec(t, routes, mappingID, []string{routing.APIFlavorOpenAIAudio})
	routes.err = errors.New("spec store unavailable")

	var got map[string]mappingFlavorMarkers
	logged := captureSlog(t, func() {
		got = listFlavorMarkers(t, svc, app.ID)
	})
	if got[mappingID] != (mappingFlavorMarkers{}) {
		t.Fatalf("markers = %+v after a failed spec read, want both false", got[mappingID])
	}
	if !strings.Contains(logged, "level=WARN") || !strings.Contains(logged, "runtime spec read failed") || !strings.Contains(logged, "spec store unavailable") {
		t.Fatalf("log output = %q, want a WARN naming the failure", logged)
	}
}

// TestModelMappingDTOAudioOnlyWireName pins the wire name the portal reads, and
// that the key is always present beside images_only: false is sent, not
// omitted.
func TestModelMappingDTOAudioOnlyWireName(t *testing.T) {
	for _, audioOnly := range []bool{true, false} {
		raw, err := json.Marshal(mappingDTO(routing.ModelMapping{ID: "map_x"}, nil, mappingFlavorMarkers{AudioOnly: audioOnly}))
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if want := fmt.Sprintf(`"audio_only":%t`, audioOnly); !strings.Contains(string(raw), want) {
			t.Fatalf("mapping DTO JSON = %s, want %s", raw, want)
		}
		if !strings.Contains(string(raw), `"images_only":false`) {
			t.Fatalf("mapping DTO JSON = %s, want images_only still present", raw)
		}
	}
}

// TestCreateMappingReportsAudioOnly: the create response carries the marker,
// with one spec read for a server_agent application and none for any other.
func TestCreateMappingReportsAudioOnly(t *testing.T) {
	cases := []struct {
		name      string
		appType   string
		flavors   []string
		want      bool
		specReads int
	}{
		{name: "agent-audio", appType: routing.ProviderServerAgent, flavors: []string{routing.APIFlavorOpenAIAudio}, want: true, specReads: 1},
		{name: "agent-text", appType: routing.ProviderServerAgent, flavors: []string{routing.APIFlavorOpenAI}, want: false, specReads: 1},
		{name: "audiocpp", appType: routing.ProviderAudioCpp, flavors: []string{routing.APIFlavorOpenAIAudio}, want: true, specReads: 0},
		{name: "text", appType: routing.ProviderVLLM, flavors: []string{routing.APIFlavorOpenAI}, want: false, specReads: 0},
	}
	svc, routes := newImagesOnlyFixture(t)
	for _, tc := range cases {
		app := seedImagesOnlyApp(t, svc, routes, tc.appType, tc.flavors)
		routes.byApp, routes.byMapping = 0, 0
		dto, err := svc.CreateMapping(context.Background(), ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: tc.name, AppModelName: tc.name})
		if err != nil {
			t.Fatalf("%s: CreateMapping: %v", tc.name, err)
		}
		if dto.AudioOnly != tc.want || dto.ImagesOnly {
			t.Errorf("%s: audio_only/images_only = %v/%v, want %v/false", tc.name, dto.AudioOnly, dto.ImagesOnly, tc.want)
		}
		if routes.byApp != 0 || routes.byMapping != tc.specReads {
			t.Errorf("%s: spec reads = (by application %d, by mapping %d), want (0, %d)", tc.name, routes.byApp, routes.byMapping, tc.specReads)
		}
	}
}

// TestUpdateMappingReportsAudioOnly: the update response carries the marker
// from the mapping's spec, which overrides the application's flavors in both
// directions, and a failed spec read reports false without failing the update.
func TestUpdateMappingReportsAudioOnly(t *testing.T) {
	ctx := context.Background()
	svc, routes := newImagesOnlyFixture(t)
	app := seedImagesOnlyApp(t, svc, routes, routing.ProviderServerAgent, []string{routing.APIFlavorOpenAIAudio})
	created, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "m", AppModelName: "m"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	update := func() ModelMappingDTO {
		t.Helper()
		routes.byApp, routes.byMapping = 0, 0
		dto, err := svc.UpdateMapping(ctx, ownerToken(), created.ID, UpdateMappingRequest{})
		if err != nil {
			t.Fatalf("UpdateMapping: %v", err)
		}
		if routes.byApp != 0 || routes.byMapping != 1 {
			t.Fatalf("spec reads = (by application %d, by mapping %d), want (0, 1)", routes.byApp, routes.byMapping)
		}
		return dto
	}

	putImagesOnlySpec(t, routes, created.ID, []string{routing.APIFlavorOpenAI})
	if update().AudioOnly {
		t.Fatalf("audio_only = true under a text spec, want false: the spec overrides the application's audio-only flavors")
	}
	putImagesOnlySpec(t, routes, created.ID, []string{routing.APIFlavorOpenAIAudio})
	if dto := update(); !dto.AudioOnly || dto.ImagesOnly {
		t.Fatalf("audio_only/images_only = %v/%v under an audio-only spec, want true/false", dto.AudioOnly, dto.ImagesOnly)
	}
	routes.err = errors.New("spec store unavailable")
	var afterFailedRead ModelMappingDTO
	logged := captureSlog(t, func() {
		afterFailedRead = update()
	})
	if afterFailedRead.AudioOnly {
		t.Fatalf("audio_only = true after a failed spec read, want false")
	}
	if !strings.Contains(logged, "level=WARN") || !strings.Contains(logged, "runtime spec read failed") || !strings.Contains(logged, "spec store unavailable") {
		t.Fatalf("log output = %q, want a WARN naming the failure", logged)
	}
}
