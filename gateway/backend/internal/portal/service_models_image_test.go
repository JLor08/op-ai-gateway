// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
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

// offerModelImage seeds a server + application + one active mapping with an
// explicit "image" capability row (yes/no per capable), mirroring
// offerModelVision (service_models_vision_test.go) but for the capability
// under test in this file.
func offerModelImage(t *testing.T, rs *routing.MemoryStore, srvID, srvName, appID string, flavors []string, gateway, appModel string, capable bool) {
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
	if capable {
		verdict = routing.CapabilityYes
	}
	if err := rs.UpsertMappingCapabilities(ctx, mappingID, []routing.CapabilityRow{{
		Capability: routing.CapabilityImage, Verdict: verdict,
		Source: routing.CapabilitySourceLlamaCppProps, CheckedAt: offeringTime,
	}}); err != nil {
		t.Fatalf("UpsertMappingCapabilities %s: %v", mappingID, err)
	}
}

// TestModelsResponseImageGroupAggregation: a group's image flag is the AND of
// its offerable members' image flags -- false if ANY offerable member is not
// image-capable, and false (fail-closed) for a group with an empty offerable
// member set. Mirrors TestModelsResponseVisionGroupAggregation
// (service_models_vision_test.go) exactly, over the image flag instead of
// vision.
//
// It exists because groupImage/its assignment into imageOn (service.go, the
// group-fold and group-entry-assignment sites beside groupVision/visionOn)
// are a sibling dropped in alongside the vision ones, not a shared code path
// -- a build that skipped either one would pass every EXISTING vision test
// unchanged (they never look at Image) while leaving image silently false
// for every model group, exactly the failure this task's brief warned about.
func TestModelsResponseImageGroupAggregation(t *testing.T) {
	ctx := context.Background()
	rs := routing.NewMemoryStore()
	// img1: image-capable. img2: NOT image-capable.
	offerModelImage(t, rs, "srv_img1", "ImgBox1", "app_img1", []string{routing.APIFlavorOpenAI}, "img1", "img1-up", true)
	offerModelImage(t, rs, "srv_img2", "ImgBox2", "app_img2", []string{routing.APIFlavorOpenAI}, "img2", "img2-up", false)

	// Mixed group {img1, img2} -> AND -> false.
	offerGroup(t, rs, "grp_mixed_img", "mixed-image-group", "img1", "img2")
	// Pure group {img3 only, image-capable} -> true.
	offerGroup(t, rs, "grp_pure_img", "pure-image-group", "img3")
	offerModelImage(t, rs, "srv_img3", "ImgBox3", "app_img3", []string{routing.APIFlavorOpenAI}, "img3", "img3-up", true)
	// Empty group (no offerable members) -> false (fail-closed) / absent.
	offerGroup(t, rs, "grp_empty_img", "empty-image-group", "ghost-img")

	svc := offerSvc(rs, nil)
	byID := modelsByID(svc.Models(ctx, auth.Token{UserID: "usr_1"}))

	mixed, ok := byID["mixed-image-group"]
	if !ok {
		t.Fatalf("mixed-image-group missing: %#v", byID)
	}
	if mixed.Image {
		t.Fatalf("mixed-image-group image = true, want false (img2 is not image-capable)")
	}

	pure, ok := byID["pure-image-group"]
	if !ok {
		t.Fatalf("pure-image-group missing: %#v", byID)
	}
	if !pure.Image {
		t.Fatalf("pure-image-group image = false, want true (sole member is image-capable)")
	}

	// The empty group has no offerable members, so it is not offered at all --
	// confirm it's absent (not merely false) since ghost-img has no active mapping.
	if _, ok := byID["empty-image-group"]; ok {
		t.Fatalf("empty-image-group should not be offered (no offerable members): %#v", byID)
	}
}

// TestModelsResponseImageAliasRedirect: an override alias must carry its
// target's Image flag under the alias name, exactly like it already carries
// ContextSize (TestOfferedAliasCarriesTargetContextSize,
// service_model_offering_test.go) and IsGroup
// (TestOfferedAliasOntoAGroupReportsIsGroup) -- the alias entry is a display
// of the target's row filed under a different name, not a separate fold.
//
// It exists because the image alias redirect (the `if v, ok :=
// imageOn[rule.To]; ok` block in service.go, beside the identical vision
// redirect) is a sibling wired in on its own -- a build that wired only the
// vision redirect would pass every alias test that exists today (none of
// them look at Image) while leaving image silently false behind every alias.
func TestModelsResponseImageAliasRedirect(t *testing.T) {
	ctx := context.Background()
	rs := routing.NewMemoryStore()
	offerModelImage(t, rs, "srv_alias_cap", "AliasCapBox", "app_alias_cap", []string{routing.APIFlavorOpenAI}, "sd-capable", "sd-capable-up", true)
	offerModelImage(t, rs, "srv_alias_non", "AliasNonBox", "app_alias_non", []string{routing.APIFlavorOpenAI}, "sd-noncapable", "sd-noncapable-up", false)

	svc := offerSvc(rs, nil)
	token := tokenWithRules(map[string]store.ModelOverrideRule{
		"alias-capable":    {To: "sd-capable", Offer: true},
		"alias-noncapable": {To: "sd-noncapable", Offer: true},
	})
	byID := modelsByID(svc.Models(ctx, token))

	aliasCapable, ok := byID["alias-capable"]
	if !ok {
		t.Fatalf("alias-capable missing from Models(): %#v", byID)
	}
	if !aliasCapable.Image {
		t.Fatalf("alias-capable image = false, want true (its target sd-capable is image-capable)")
	}

	aliasNoncapable, ok := byID["alias-noncapable"]
	if !ok {
		t.Fatalf("alias-noncapable missing from Models(): %#v", byID)
	}
	if aliasNoncapable.Image {
		t.Fatalf("alias-noncapable image = true, want false (its target sd-noncapable is not image-capable)")
	}
}
