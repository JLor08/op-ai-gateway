// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/usage"
	"testing"
	"time"
)

// TestModelsResponseVisionAndAcrossMappings: when the same gateway model name is
// offered by two mappings (on different servers) and ONE of them is NOT
// vision-capable, the DTO reports vision=false (AND across all offering
// mappings, fail-closed). A model whose sole mapping is vision-capable reports
// vision=true.
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
	mustMap := func(id, app, gateway string, vision bool) {
		if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: id, ApplicationID: app, GatewayModelName: gateway, AppModelName: gateway, Status: routing.ServerStatusActive, VisionCapable: vision, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateMapping %s: %v", id, err)
		}
	}
	// m1: two mappings, one vision-capable, one not -> AND -> false.
	mustServer("srv_a", "GPU-A")
	mustApp("app_a", "srv_a", 8000)
	mustMap("m_a", "app_a", "m1", true)
	mustServer("srv_b", "GPU-B")
	mustApp("app_b", "srv_b", 8000)
	mustMap("m_b", "app_b", "m1", false)
	// m2: single mapping, vision-capable -> true.
	mustServer("srv_c", "GPU-C")
	mustApp("app_c", "srv_c", 8000)
	mustMap("m_c", "app_c", "m2", true)

	svc := NewService(ServiceDeps{Usage: usage.NewRecorder(), Routes: routeStore, Clock: func() time.Time { return now }})
	got := svc.Models(ctx, auth.Token{UserID: "usr_1"})
	byID := modelsByID(got)

	if byID["m1"].Vision {
		t.Fatalf("m1 vision = true, want false (AND with one non-vision mapping)")
	}
	if !byID["m2"].Vision {
		t.Fatalf("m2 vision = false, want true (sole mapping is vision-capable)")
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
// explicit vision_capable flag, mirroring offerModel (service_model_groups_offering_test.go)
// but threading VisionCapable through.
func offerModelVision(t *testing.T, rs *routing.MemoryStore, srvID, srvName, appID string, flavors []string, gateway, appModel string, vision bool) {
	t.Helper()
	ctx := context.Background()
	if err := rs.CreateAIServer(ctx, routing.AIServer{ID: srvID, Name: srvName, Domain: srvID + ".test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: offeringTime, UpdatedAt: offeringTime}); err != nil {
		t.Fatalf("CreateAIServer %s: %v", srvID, err)
	}
	if err := rs.CreateApplication(ctx, routing.Application{ID: appID, ServerID: srvID, Type: routing.ProviderVLLM, Port: 8000, Scheme: "https", APIFlavors: flavors, Status: routing.ServerStatusActive, CreatedAt: offeringTime, UpdatedAt: offeringTime}); err != nil {
		t.Fatalf("CreateApplication %s: %v", appID, err)
	}
	if err := rs.CreateMapping(ctx, routing.ModelMapping{ID: appID + "_map", ApplicationID: appID, GatewayModelName: gateway, AppModelName: appModel, Status: routing.ServerStatusActive, VisionCapable: vision, CreatedAt: offeringTime, UpdatedAt: offeringTime}); err != nil {
		t.Fatalf("CreateMapping %s: %v", gateway, err)
	}
}
