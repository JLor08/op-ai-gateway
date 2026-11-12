// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/json"
	"fmt"
	"op-ai-gateway/internal/routing"
	"strconv"
	"strings"
	"testing"
)

// The decisions_only marker is audio_only's decision twin and shares its
// fixtures (service_mapping_images_only_test.go).

func TestMappingDTOMarksDecisionsOnly(t *testing.T) {
	svc, routes := newImagesOnlyFixture(t)
	dec := []string{routing.APIFlavorDecisions}
	agent := seedImagesOnlyApp(t, svc, routes, routing.ProviderServerAgent, []string{routing.APIFlavorOpenAI, routing.APIFlavorDecisions})
	specDec := seedImagesOnlyMapping(t, routes, agent.ID, "spec-dec")
	putImagesOnlySpec(t, routes, specDec, dec)
	specText := seedImagesOnlyMapping(t, routes, agent.ID, "spec-text")
	putImagesOnlySpec(t, routes, specText, []string{routing.APIFlavorOpenAI, routing.APIFlavorDecisions})
	noSpec := seedImagesOnlyMapping(t, routes, agent.ID, "no-spec")

	routes.byApp, routes.byMapping = 0, 0
	got := listFlavorMarkers(t, svc, agent.ID)
	if !got[specDec].DecisionsOnly || got[specText].DecisionsOnly || got[noSpec].DecisionsOnly {
		t.Fatalf("decisions_only = %v/%v/%v, want true/false/false", got[specDec].DecisionsOnly, got[specText].DecisionsOnly, got[noSpec].DecisionsOnly)
	}
	if got[specDec].AudioOnly || got[specDec].ImagesOnly {
		t.Fatalf("a decisions-only mapping must not be marked images/audio-only: %+v", got[specDec])
	}
	if routes.byApp != 1 || routes.byMapping != 0 {
		t.Fatalf("spec reads = (by application %d, by mapping %d), want (1, 0)", routes.byApp, routes.byMapping)
	}

	// A non-agent application reads no spec.
	app := seedImagesOnlyApp(t, svc, routes, routing.ProviderAudioCpp, dec)
	id := seedImagesOnlyMapping(t, routes, app.ID, "plain")
	routes.byApp, routes.byMapping = 0, 0
	if m := listFlavorMarkers(t, svc, app.ID)[id]; !m.DecisionsOnly {
		t.Fatalf("non-agent decisions-only mapping: markers = %+v", m)
	}
	if routes.byApp != 0 || routes.byMapping != 0 {
		t.Fatalf("spec reads = (%d, %d), want none", routes.byApp, routes.byMapping)
	}

	// Create reports it with one by-mapping read for an agent application.
	created, err := svc.CreateMapping(context.Background(), ownerToken(), seedImagesOnlyApp(t, svc, routes, routing.ProviderServerAgent, dec).ID, CreateMappingRequest{GatewayModelName: "c", AppModelName: "c"})
	if err != nil || !created.DecisionsOnly {
		t.Fatalf("CreateMapping = %+v, %v; want decisions_only true", created, err)
	}
}

func TestModelMappingDTODecisionsOnlyWireName(t *testing.T) {
	for _, v := range []bool{true, false} {
		raw, err := json.Marshal(mappingDTO(routing.ModelMapping{ID: "map_x"}, nil, mappingFlavorMarkers{DecisionsOnly: v}))
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if want := fmt.Sprintf(`"decisions_only":%t`, v); !strings.Contains(string(raw), want) {
			t.Fatalf("mapping DTO JSON = %s, want %s", raw, want)
		}
	}
}

// TestModelDTODecisionIsAlwaysOnTheWire: the decision flag is always present,
// like its sibling fold flags (speech, transcription, ...): a model that is no
// decision model sends "decision": false, never an omitted key.
func TestModelDTODecisionIsAlwaysOnTheWire(t *testing.T) {
	for _, want := range []bool{false, true} {
		raw, err := json.Marshal(ModelDTO{ID: "m", Decision: want})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(raw, &keys); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		got, ok := keys["decision"]
		if !ok || string(got) != strconv.FormatBool(want) {
			t.Fatalf("decision on the wire = %s (present %v), want %v", got, ok, want)
		}
	}
}
