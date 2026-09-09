// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/usage"
	"strings"
	"testing"
	"time"
)

// TestModelsResponseVisionAndAcrossMappings: when the same gateway model name is
// offered by two mappings (on different servers) and ONE of them is NOT
// vision-capable, the DTO reports vision=false (AND across all offering
// mappings, fail-closed). A model whose sole mapping is vision-capable reports
// vision=true. "not vision-capable" is exercised BOTH ways a mapping can fail
// to advertise vision now that the verdict lives on a row rather than a bool
// column: an explicit "no" row, and NO row at all (never probed) -- both must
// AND in as false, which is the fail-closed guarantee this table's row-
// absence-means-unknown model exists to preserve (routing.CapabilityRow's own
// doc-comment).
func TestModelsResponseVisionAndAcrossMappings(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	routeStore := routing.NewMemoryStore()
	mustServer := func(id, name string) {
		if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: id, Name: name, Domain: id + ".test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateAIServer %s: %v", id, err)
		}
	}
	mustApp := func(id, srv string, port int) {
		if err := routeStore.CreateApplication(ctx, routing.Application{ID: id, ServerID: srv, Type: routing.ProviderVLLM, Port: port, Scheme: "https", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateApplication %s: %v", id, err)
		}
	}
	// mustMap creates the mapping with NO row written at all when verdict=="" --
	// exercising "never probed", the ABSENCE case -- and writes an explicit
	// "vision" capability row (source vision_benchmark, arbitrary here — the
	// fold only cares about the verdict) for "yes"/"no".
	mustMap := func(id, app, gateway, verdict string) {
		if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: id, ApplicationID: app, GatewayModelName: gateway, AppModelName: gateway, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateMapping %s: %v", id, err)
		}
		if verdict == "" {
			return
		}
		if err := routeStore.UpsertMappingCapabilities(ctx, id, []routing.CapabilityRow{{
			Capability: routing.CapabilityVision, Verdict: verdict,
			Source: routing.CapabilitySourceVisionBenchmark, CheckedAt: now,
		}}); err != nil {
			t.Fatalf("UpsertMappingCapabilities %s: %v", id, err)
		}
	}
	// m1: two mappings, one vision-capable, one explicitly NOT -> AND -> false.
	mustServer("srv_a", "GPU-A")
	mustApp("app_a", "srv_a", 8000)
	mustMap("m_a", "app_a", "m1", routing.CapabilityYes)
	mustServer("srv_b", "GPU-B")
	mustApp("app_b", "srv_b", 8000)
	mustMap("m_b", "app_b", "m1", routing.CapabilityNo)
	// m2: single mapping, vision-capable -> true.
	mustServer("srv_c", "GPU-C")
	mustApp("app_c", "srv_c", 8000)
	mustMap("m_c", "app_c", "m2", routing.CapabilityYes)
	// m3: two mappings, one vision-capable, one NEVER PROBED (no row at all)
	// -> AND -> false. This is the fail-closed regression this table exists
	// to prevent: a missing row must never silently count as capable.
	mustServer("srv_d", "GPU-D")
	mustApp("app_d", "srv_d", 8000)
	mustMap("m_d", "app_d", "m3", routing.CapabilityYes)
	mustServer("srv_e", "GPU-E")
	mustApp("app_e", "srv_e", 8000)
	mustMap("m_e", "app_e", "m3", "")

	svc := NewService(ServiceDeps{Usage: usage.NewRecorder(), Routes: routeStore, Clock: func() time.Time { return now }})
	got := svc.Models(ctx, auth.Token{UserID: "usr_1"})
	byID := modelsByID(got)

	if byID["m1"].Vision {
		t.Fatalf("m1 vision = true, want false (AND with one explicit non-vision mapping)")
	}
	if !byID["m2"].Vision {
		t.Fatalf("m2 vision = false, want true (sole mapping is vision-capable)")
	}
	if byID["m3"].Vision {
		t.Fatalf("m3 vision = true, want false (AND with one NEVER-PROBED mapping -- fail-closed)")
	}
}

// TestModelsResponseVisionReadsTheVisionRowAndNoOther: the fold picks the
// mapping's "vision" row out of a row SET that also holds other
// capabilities, and a mapping carrying other rows but no vision row still
// folds to false.
//
// It exists because the models listing asks each mapping exactly one
// capability question, so its vision row is picked out in one pass over the
// batch read instead of by keying every mapping's whole row set by name
// inside the per-view loop. That pass carries the name comparison the map
// lookup used to do, and the rest of this file's cases only ever write a
// vision row -- so nothing there would notice a pass that simply returned
// whichever row came first. "tools" sorts before "vision", which is exactly
// the order that would let such a mistake pass unnoticed.
func TestModelsResponseVisionReadsTheVisionRowAndNoOther(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	routeStore := routing.NewMemoryStore()
	offer := func(srvID, appID, mappingID, gateway string, rows ...routing.CapabilityRow) {
		t.Helper()
		if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: srvID, Name: srvID, Domain: srvID + ".test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateAIServer %s: %v", srvID, err)
		}
		if err := routeStore.CreateApplication(ctx, routing.Application{ID: appID, ServerID: srvID, Type: routing.ProviderVLLM, Port: 8000, Scheme: "https", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateApplication %s: %v", appID, err)
		}
		if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: mappingID, ApplicationID: appID, GatewayModelName: gateway, AppModelName: gateway, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateMapping %s: %v", mappingID, err)
		}
		if err := routeStore.UpsertMappingCapabilities(ctx, mappingID, rows); err != nil {
			t.Fatalf("UpsertMappingCapabilities %s: %v", mappingID, err)
		}
	}
	row := func(capability, verdict string) routing.CapabilityRow {
		return routing.CapabilityRow{
			Capability: capability, Verdict: verdict,
			Source: routing.CapabilitySourceLlamaCppProps, CheckedAt: now,
		}
	}
	// m1: a tools=yes row and NO vision row -> false (never probed for
	// vision, whatever else is known about it).
	offer("srv_t", "app_t", "map_t", "m1", row(routing.CapabilityTools, routing.CapabilityYes))
	// m2: the same tools row PLUS vision=yes -> true.
	offer("srv_u", "app_u", "map_u", "m2",
		row(routing.CapabilityTools, routing.CapabilityYes),
		row(routing.CapabilityVision, routing.CapabilityYes))

	svc := NewService(ServiceDeps{Usage: usage.NewRecorder(), Routes: routeStore, Clock: func() time.Time { return now }})
	byID := modelsByID(svc.Models(ctx, auth.Token{UserID: "usr_1"}))
	if _, ok := byID["m1"]; !ok {
		t.Fatalf("m1 missing from the listing (%#v)", byID)
	}
	if byID["m1"].Vision {
		t.Fatalf("m1 vision = true, want false -- its only row is tools=yes, and no vision row means never probed")
	}
	if !byID["m2"].Vision {
		t.Fatalf("m2 vision = false, want true -- its vision=yes row must be found among the other capability rows")
	}
}

// TestModelsResponseCapabilityReadFailureDegradesAndLogs: a failing bulk
// capability read in the models listing must NOT fail the listing -- the
// models still come back, but fail-CLOSED, with vision withheld from every
// one of them (the fold AND-s a missing row in as false) -- and it must LOG.
// This degrade is a gateway-wide, fail-closed outage of one feature: the
// portal chat's image attach gates on ModelOption.vision, so it silently
// disappears for every model while the page otherwise renders perfectly
// normally. Unlogged, there is nothing at all to diagnose it from.
func TestModelsResponseCapabilityReadFailureDegradesAndLogs(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	routeStore := routing.NewMemoryStore()
	// A single mapping with an explicit vision=yes row: the listing reports
	// vision=true when the read works, so the degrade is observable.
	offerModelVision(t, routeStore, "srv_v", "BoxV", "app_v", []string{routing.APIFlavorOpenAI}, "m1", "m1-up", true)

	working := NewService(ServiceDeps{Usage: usage.NewRecorder(), Routes: routeStore, Clock: func() time.Time { return now }})
	if !modelsByID(working.Models(ctx, auth.Token{UserID: "usr_1"}))["m1"].Vision {
		t.Fatalf("precondition: m1 vision = false with a WORKING capability read, want true")
	}

	failing := &failingCapabilityStore{MemoryStore: routeStore, err: errors.New("capability table unavailable")}
	svc := NewService(ServiceDeps{Usage: usage.NewRecorder(), Routes: failing, Clock: func() time.Time { return now }})
	var got ModelsResponse
	logged := captureSlog(t, func() {
		got = svc.Models(ctx, auth.Token{UserID: "usr_1"})
	})
	byID := modelsByID(got)
	model, ok := byID["m1"]
	if !ok {
		t.Fatalf("m1 missing from the listing: the capability read must DEGRADE, not drop models (%#v)", byID)
	}
	if model.Vision {
		t.Fatalf("m1 vision = true after a failed capability read, want false (fail-closed)")
	}
	if !strings.Contains(logged, "capability read failed") || !strings.Contains(logged, "capability table unavailable") {
		t.Fatalf("log output = %q, want a warning naming the failure -- withholding vision gateway-wide must leave a diagnostic trail", logged)
	}
}

// TestModelsResponseVisionGroupAggregation: a group's vision flag is the AND of
// its offerable members' vision flags — false if ANY offerable member is not
// vision-capable, and false (fail-closed) for a group with an empty offerable
// member set.
func TestModelsResponseVisionGroupAggregation(t *testing.T) {
	ctx := context.Background()
	rs := routing.NewMemoryStore()
	// m1: vision-capable. m2: NOT vision-capable.
	offerModelVision(t, rs, "srv_1", "Box1", "app_1", []string{routing.APIFlavorOpenAI}, "m1", "m1-up", true)
	offerModelVision(t, rs, "srv_2", "Box2", "app_2", []string{routing.APIFlavorOpenAI}, "m2", "m2-up", false)

	// Mixed group {m1, m2} -> AND -> false.
	offerGroup(t, rs, "grp_mixed", "mixed-group", "m1", "m2")
	// Pure group {m2 only, vision-capable} -> true.
	offerGroup(t, rs, "grp_vision", "vision-group", "m2v")
	offerModelVision(t, rs, "srv_3", "Box3", "app_3", []string{routing.APIFlavorOpenAI}, "m2v", "m2v-up", true)
	// Empty group (no offerable members) -> false (fail-closed).
	offerGroup(t, rs, "grp_empty", "empty-group", "ghost")

	svc := offerSvc(rs, nil)
	byID := modelsByID(svc.Models(ctx, auth.Token{UserID: "usr_1"}))

	mixed, ok := byID["mixed-group"]
	if !ok {
		t.Fatalf("mixed-group missing: %#v", byID)
	}
	if mixed.Vision {
		t.Fatalf("mixed-group vision = true, want false (m2 is not vision-capable)")
	}

	visionGroup, ok := byID["vision-group"]
	if !ok {
		t.Fatalf("vision-group missing: %#v", byID)
	}
	if !visionGroup.Vision {
		t.Fatalf("vision-group vision = false, want true (sole member is vision-capable)")
	}

	// The empty group has no offerable members, so it is not offered at all —
	// confirm it's absent (not merely false) since ghost has no active mapping.
	if _, ok := byID["empty-group"]; ok {
		t.Fatalf("empty-group should not be offered (no offerable members): %#v", byID)
	}
}

// offerModelVision seeds a server + application + one active mapping with an
// explicit "vision" capability row (yes/no per vision), mirroring offerModel
// (service_model_groups_offering_test.go) but threading a vision verdict
// through. The verdict is a ROW: the fold under test (modelsResponse) reads
// model_mapping_capabilities, and the vision_capable column it replaced is
// gone (migration 79).
func offerModelVision(t *testing.T, rs *routing.MemoryStore, srvID, srvName, appID string, flavors []string, gateway, appModel string, vision bool) {
	t.Helper()
	ctx := context.Background()
	if err := rs.CreateAIServer(ctx, routing.AIServer{ID: srvID, Name: srvName, Domain: srvID + ".test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: offeringTime, UpdatedAt: offeringTime}); err != nil {
		t.Fatalf("CreateAIServer %s: %v", srvID, err)
	}
	if err := rs.CreateApplication(ctx, routing.Application{ID: appID, ServerID: srvID, Type: routing.ProviderVLLM, Port: 8000, Scheme: "https", APIFlavors: flavors, Status: routing.ServerStatusActive, CreatedAt: offeringTime, UpdatedAt: offeringTime}); err != nil {
		t.Fatalf("CreateApplication %s: %v", appID, err)
	}
	mappingID := appID + "_map"
	if err := rs.CreateMapping(ctx, routing.ModelMapping{ID: mappingID, ApplicationID: appID, GatewayModelName: gateway, AppModelName: appModel, Status: routing.ServerStatusActive, CreatedAt: offeringTime, UpdatedAt: offeringTime}); err != nil {
		t.Fatalf("CreateMapping %s: %v", gateway, err)
	}
	verdict := routing.CapabilityNo
	if vision {
		verdict = routing.CapabilityYes
	}
	if err := rs.UpsertMappingCapabilities(ctx, mappingID, []routing.CapabilityRow{{
		Capability: routing.CapabilityVision, Verdict: verdict,
		Source: routing.CapabilitySourceVisionBenchmark, CheckedAt: offeringTime,
	}}); err != nil {
		t.Fatalf("UpsertMappingCapabilities %s: %v", mappingID, err)
	}
}
