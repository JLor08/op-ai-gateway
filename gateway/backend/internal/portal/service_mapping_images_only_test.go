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
	"time"
)

var imagesOnlyNow = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

// imagesOnlySpecStore wraps a *routing.MemoryStore and instruments the two
// runtime-spec reads the images_only marker makes: it counts them and, with
// err set, fails them. Every other method is the embedded store's own.
type imagesOnlySpecStore struct {
	*routing.MemoryStore
	err       error
	byApp     int
	byMapping int
}

func (s *imagesOnlySpecStore) RuntimeSpecsByApplication(ctx context.Context, appID string) ([]routing.RuntimeSpec, error) {
	s.byApp++
	if s.err != nil {
		return nil, s.err
	}
	return s.MemoryStore.RuntimeSpecsByApplication(ctx, appID)
}

func (s *imagesOnlySpecStore) RuntimeSpecByMapping(ctx context.Context, mappingID string) (routing.RuntimeSpec, bool, error) {
	s.byMapping++
	if s.err != nil {
		return routing.RuntimeSpec{}, false, s.err
	}
	return s.MemoryStore.RuntimeSpecByMapping(ctx, mappingID)
}

// newImagesOnlyFixture builds a Service over an instrumented store.
func newImagesOnlyFixture(t *testing.T) (*Service, *imagesOnlySpecStore) {
	t.Helper()
	routes := &imagesOnlySpecStore{MemoryStore: routing.NewMemoryStore()}
	return newServerTestServiceWithRoutes(t, imagesOnlyNow, routes), routes
}

// seedImagesOnlyApp stores an active application of appType declaring flavors
// on a server of its own, owned by ownerToken's principal (a server holds at
// most one server_agent application).
func seedImagesOnlyApp(t *testing.T, svc *Service, routes *imagesOnlySpecStore, appType string, flavors []string) routing.Application {
	t.Helper()
	name := compactRandomHex(8)
	server := createTestServer(t, svc, name, name+".example.test")
	app := routing.Application{
		ID: "app_" + name, ServerID: server.ID, Type: appType, Port: 9000, Scheme: "http",
		APIFlavors: flavors, Status: routing.ServerStatusActive, CreatedAt: imagesOnlyNow, UpdatedAt: imagesOnlyNow,
	}
	if err := routes.CreateApplication(context.Background(), app); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	return app
}

// seedImagesOnlyMapping stores an active mapping named name under appID.
func seedImagesOnlyMapping(t *testing.T, routes *imagesOnlySpecStore, appID, name string) string {
	t.Helper()
	id := "map_" + name
	if err := routes.CreateMapping(context.Background(), routing.ModelMapping{
		ID: id, ApplicationID: appID, GatewayModelName: name, AppModelName: name,
		Status: routing.ServerStatusActive, CreatedAt: imagesOnlyNow, UpdatedAt: imagesOnlyNow,
	}); err != nil {
		t.Fatalf("CreateMapping %s: %v", name, err)
	}
	return id
}

// putImagesOnlySpec stores mappingID's runtime spec declaring flavors.
func putImagesOnlySpec(t *testing.T, routes *imagesOnlySpecStore, mappingID string, flavors []string) {
	t.Helper()
	if err := routes.UpsertRuntimeSpec(context.Background(), routing.RuntimeSpec{
		ID: "rspec_" + mappingID, MappingID: mappingID, Enabled: true, Binary: "/usr/bin/model-server", Args: "[]", Env: "{}",
		APIFlavors: flavors, CreatedAt: imagesOnlyNow, UpdatedAt: imagesOnlyNow,
	}); err != nil {
		t.Fatalf("UpsertRuntimeSpec %s: %v", mappingID, err)
	}
}

// listImagesOnly lists appID's mappings and returns each one's ImagesOnly.
func listImagesOnly(t *testing.T, svc *Service, appID string) map[string]bool {
	t.Helper()
	list, err := svc.ListMappings(context.Background(), ownerToken(), appID)
	if err != nil {
		t.Fatalf("ListMappings: %v", err)
	}
	got := make(map[string]bool, len(list.Data))
	for _, dto := range list.Data {
		got[dto.ID] = dto.ImagesOnly
	}
	return got
}

// TestListMappingsImagesOnlyFollowsTheEffectiveFlavors: for a server_agent
// application the spec's flavors decide whenever the mapping has a spec, one
// stored with no flavors included, and the application's otherwise. Each
// listing costs one RuntimeSpecsByApplication read.
func TestListMappingsImagesOnlyFollowsTheEffectiveFlavors(t *testing.T) {
	svc, routes := newImagesOnlyFixture(t)
	both := seedImagesOnlyApp(t, svc, routes, routing.ProviderServerAgent, []string{routing.APIFlavorOpenAI, routing.APIFlavorOpenAIImages})
	specImages := seedImagesOnlyMapping(t, routes, both.ID, "spec-images")
	putImagesOnlySpec(t, routes, specImages, []string{routing.APIFlavorOpenAIImages})
	specText := seedImagesOnlyMapping(t, routes, both.ID, "spec-text")
	putImagesOnlySpec(t, routes, specText, []string{routing.APIFlavorOpenAI})
	bothNoSpec := seedImagesOnlyMapping(t, routes, both.ID, "both-no-spec")
	images := seedImagesOnlyApp(t, svc, routes, routing.ProviderServerAgent, []string{routing.APIFlavorOpenAIImages})
	imagesNoSpec := seedImagesOnlyMapping(t, routes, images.ID, "images-no-spec")
	specEmpty := seedImagesOnlyMapping(t, routes, images.ID, "spec-empty")
	putImagesOnlySpec(t, routes, specEmpty, []string{})

	for _, tc := range []struct {
		appID string
		want  map[string]bool
	}{
		{appID: both.ID, want: map[string]bool{specImages: true, specText: false, bothNoSpec: false}},
		{appID: images.ID, want: map[string]bool{imagesNoSpec: true, specEmpty: false}},
	} {
		routes.byApp, routes.byMapping = 0, 0
		got := listImagesOnly(t, svc, tc.appID)
		for id, want := range tc.want {
			if got[id] != want {
				t.Errorf("%s images_only = %v, want %v", id, got[id], want)
			}
		}
		if routes.byApp != 1 || routes.byMapping != 0 {
			t.Errorf("%s spec reads = (by application %d, by mapping %d), want (1, 0)", tc.appID, routes.byApp, routes.byMapping)
		}
	}
}

// TestListMappingsImagesOnlyReadsNoSpecForOtherApplications: an application
// other than server_agent answers from its own flavors without a spec read.
func TestListMappingsImagesOnlyReadsNoSpecForOtherApplications(t *testing.T) {
	svc, routes := newImagesOnlyFixture(t)
	sd := seedImagesOnlyApp(t, svc, routes, routing.ProviderStableDiffusionCpp, []string{routing.APIFlavorOpenAIImages})
	sdMapping := seedImagesOnlyMapping(t, routes, sd.ID, "sd")
	text := seedImagesOnlyApp(t, svc, routes, routing.ProviderVLLM, []string{routing.APIFlavorOpenAI})
	textMapping := seedImagesOnlyMapping(t, routes, text.ID, "text")

	routes.byApp, routes.byMapping = 0, 0
	if got := listImagesOnly(t, svc, sd.ID); !got[sdMapping] {
		t.Fatalf("stable_diffusion_cpp mapping images_only = false, want true")
	}
	if got := listImagesOnly(t, svc, text.ID); got[textMapping] {
		t.Fatalf("text mapping images_only = true, want false")
	}
	if routes.byApp != 0 || routes.byMapping != 0 {
		t.Fatalf("spec reads = (by application %d, by mapping %d), want none", routes.byApp, routes.byMapping)
	}
}

// TestListMappingsImagesOnlyIsFalseWhenTheSpecReadFails: a failed spec read
// reports false, even where the application's own flavors are images-only,
// and the listing itself still succeeds. The WARN it logs is captured so the
// test's own output stays pristine.
func TestListMappingsImagesOnlyIsFalseWhenTheSpecReadFails(t *testing.T) {
	svc, routes := newImagesOnlyFixture(t)
	app := seedImagesOnlyApp(t, svc, routes, routing.ProviderServerAgent, []string{routing.APIFlavorOpenAIImages})
	mappingID := seedImagesOnlyMapping(t, routes, app.ID, "sd")
	putImagesOnlySpec(t, routes, mappingID, []string{routing.APIFlavorOpenAIImages})
	routes.err = errors.New("spec store unavailable")

	var got map[string]bool
	logged := captureSlog(t, func() {
		got = listImagesOnly(t, svc, app.ID)
	})
	if got[mappingID] {
		t.Fatalf("images_only = true after a failed spec read, want false")
	}
	if !strings.Contains(logged, "level=WARN") || !strings.Contains(logged, "runtime spec read failed") || !strings.Contains(logged, "spec store unavailable") {
		t.Fatalf("log output = %q, want a WARN naming the failure", logged)
	}
}

// TestModelMappingDTOImagesOnlyWireName pins the wire name the portal reads,
// and that the key is always present: false is sent, not omitted.
func TestModelMappingDTOImagesOnlyWireName(t *testing.T) {
	for _, imagesOnly := range []bool{true, false} {
		raw, err := json.Marshal(mappingDTO(routing.ModelMapping{ID: "map_x"}, nil, imagesOnly))
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if want := fmt.Sprintf(`"images_only":%t`, imagesOnly); !strings.Contains(string(raw), want) {
			t.Fatalf("mapping DTO JSON = %s, want %s", raw, want)
		}
	}
}

// TestCreateMappingReportsImagesOnly: the create response carries the
// marker, with one spec read for a server_agent application and none for any
// other.
func TestCreateMappingReportsImagesOnly(t *testing.T) {
	cases := []struct {
		name      string
		appType   string
		flavors   []string
		want      bool
		specReads int
	}{
		{name: "agent-images", appType: routing.ProviderServerAgent, flavors: []string{routing.APIFlavorOpenAIImages}, want: true, specReads: 1},
		{name: "agent-text", appType: routing.ProviderServerAgent, flavors: []string{routing.APIFlavorOpenAI}, want: false, specReads: 1},
		{name: "sd", appType: routing.ProviderStableDiffusionCpp, flavors: []string{routing.APIFlavorOpenAIImages}, want: true, specReads: 0},
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
		if dto.ImagesOnly != tc.want {
			t.Errorf("%s: images_only = %v, want %v", tc.name, dto.ImagesOnly, tc.want)
		}
		if routes.byApp != 0 || routes.byMapping != tc.specReads {
			t.Errorf("%s: spec reads = (by application %d, by mapping %d), want (0, %d)", tc.name, routes.byApp, routes.byMapping, tc.specReads)
		}
	}
}

// TestUpdateMappingReportsImagesOnly: the update response carries the marker
// from the mapping's spec, which overrides the application's flavors in both
// directions, and a failed spec read reports false without failing the
// update.
func TestUpdateMappingReportsImagesOnly(t *testing.T) {
	ctx := context.Background()
	svc, routes := newImagesOnlyFixture(t)
	app := seedImagesOnlyApp(t, svc, routes, routing.ProviderServerAgent, []string{routing.APIFlavorOpenAIImages})
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
	if update().ImagesOnly {
		t.Fatalf("images_only = true under a text spec, want false: the spec overrides the application's images-only flavors")
	}
	putImagesOnlySpec(t, routes, created.ID, []string{routing.APIFlavorOpenAIImages})
	if !update().ImagesOnly {
		t.Fatalf("images_only = false under an images-only spec, want true")
	}
	routes.err = errors.New("spec store unavailable")
	var afterFailedRead ModelMappingDTO
	logged := captureSlog(t, func() {
		afterFailedRead = update()
	})
	if afterFailedRead.ImagesOnly {
		t.Fatalf("images_only = true after a failed spec read, want false")
	}
	if !strings.Contains(logged, "level=WARN") || !strings.Contains(logged, "runtime spec read failed") || !strings.Contains(logged, "spec store unavailable") {
		t.Fatalf("log output = %q, want a WARN naming the failure", logged)
	}
}
