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

// The image flag is an AND across every offering mapping, fail-closed, exactly
// like vision: a "no" row and a MISSING row both count as not capable. It is
// also INDEPENDENT of vision -- a generator that accepts no image input is
// image=true, vision=false, and conflating the two is the specific error
// routing.CapabilityImage's doc comment warns about.
func TestModelsResponseImageFlag(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
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

	// sd: image=yes and NO vision row -> image true, vision false.
	offer("srv_sd", "app_sd", "map_sd", "sd", row(routing.CapabilityImage, routing.CapabilityYes))
	// vis: vision=yes and NO image row -> image false, vision true.
	offer("srv_v", "app_v", "map_v", "vis", row(routing.CapabilityVision, routing.CapabilityYes))
	// nores: an image=no row -> false, same as absent.
	offer("srv_n", "app_n", "map_n", "nores", row(routing.CapabilityImage, routing.CapabilityNo))

	svc := NewService(ServiceDeps{Usage: usage.NewRecorder(), Routes: routeStore, Clock: func() time.Time { return now }})
	byID := modelsByID(svc.Models(ctx, auth.Token{UserID: "usr_1"}))

	if !byID["sd"].Image {
		t.Fatal("sd image = false, want true -- its image=yes row must be found")
	}
	if byID["sd"].Vision {
		t.Fatal("sd vision = true, want false -- image is not vision; it has no vision row")
	}
	if byID["vis"].Image {
		t.Fatal("vis image = true, want false -- a vision row says nothing about generation")
	}
	if byID["nores"].Image {
		t.Fatal("nores image = true, want false -- an explicit no row is not capable")
	}
}

// A model offered by TWO mappings is image-capable only when BOTH say yes.
// This is the fail-closed half, and it works only because the accumulator is
// seeded to true on a model's first view and then only ever ANDed down.
func TestModelsResponseImageAndAcrossMappings(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	routeStore := routing.NewMemoryStore()
	offerImage := func(srvID, appID, mappingID, gateway string, capable bool) {
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
		verdict := routing.CapabilityNo
		if capable {
			verdict = routing.CapabilityYes
		}
		if err := routeStore.UpsertMappingCapabilities(ctx, mappingID, []routing.CapabilityRow{{
			Capability: routing.CapabilityImage, Verdict: verdict,
			Source: routing.CapabilitySourceLlamaCppProps, CheckedAt: now,
		}}); err != nil {
			t.Fatalf("UpsertMappingCapabilities %s: %v", mappingID, err)
		}
	}

	offerImage("srv_a", "app_a", "map_a", "shared", true)
	offerImage("srv_b", "app_b", "map_b", "shared", false)

	svc := NewService(ServiceDeps{Usage: usage.NewRecorder(), Routes: routeStore, Clock: func() time.Time { return now }})
	byID := modelsByID(svc.Models(ctx, auth.Token{UserID: "usr_1"}))
	if byID["shared"].Image {
		t.Fatal("shared image = true, want false -- one offering mapping says no, and the fold is an AND")
	}
}
