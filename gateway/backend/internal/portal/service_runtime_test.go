// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/json"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"strings"
	"sync"
	"testing"
	"time"
)

// seedServerAgentApplication inserts a server_agent-typed application
// directly at the store layer for serverID, bypassing CreateApplication's
// validation/timeout-default/policy-reconciliation path entirely. Runtime-spec
// tests use this to get a minimal, fully-controlled fixture application
// without depending on (or re-asserting) CreateApplication's own behavior --
// mirroring seedServerTestGroups' direct-store seeding of admin groups for
// the same reason. (Historically this was also the ONLY way to get a
// server_agent application at all, since CreateApplication.
// normalizeApplicationType did not accept "server_agent" as a creatable
// type before Task 10; that gate is gone now, but the direct-store seed
// remains the right tool for these tests' purposes.)
func seedServerAgentApplication(t *testing.T, routeStore *routing.MemoryStore, serverID string, now time.Time) routing.Application {
	t.Helper()
	app := routing.Application{
		ID:         "app_" + compactRandomHex(16),
		ServerID:   serverID,
		Type:       routing.ProviderServerAgent,
		Port:       9000,
		Scheme:     "http",
		APIFlavors: []string{routing.APIFlavorOpenAI},
		Status:     routing.ServerStatusActive,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := routeStore.CreateApplication(context.Background(), app); err != nil {
		t.Fatalf("seed server_agent application: %v", err)
	}
	return app
}

func TestPutRuntimeSpecRoundTrip(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "qwen", AppModelName: "qwen"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}

	dto, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{
		Enabled: true,
		Binary:  "/usr/local/bin/llama-server",
		Args:    []string{"--model", "/models/q.gguf"},
		Env:     map[string]string{"CUDA_VISIBLE_DEVICES": "${AGENT_ENV:GPU_IDS}"},
		GPUs:    []RuntimeSpecGPUDTO{{Index: 0, VRAMEstimateMB: 8000}},
	})
	if err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	if !dto.Configured || dto.ID == "" {
		t.Fatalf("dto = %#v, want Configured with a non-empty ID", dto)
	}
	if dto.MappingID != mapping.ID {
		t.Fatalf("mapping id = %q, want %q", dto.MappingID, mapping.ID)
	}
	// Defaults applied on empty/zero fields.
	if dto.HealthPath != "/health" {
		t.Fatalf("health path default = %q, want /health", dto.HealthPath)
	}
	if dto.HealthTimeoutSeconds != 5 {
		t.Fatalf("health timeout default = %d, want 5", dto.HealthTimeoutSeconds)
	}
	if dto.StartupTimeoutSeconds != 180 {
		t.Fatalf("startup timeout default = %d, want 180", dto.StartupTimeoutSeconds)
	}
	// Args/env round-trip.
	if len(dto.Args) != 2 || dto.Args[0] != "--model" || dto.Args[1] != "/models/q.gguf" {
		t.Fatalf("args = %#v", dto.Args)
	}
	if dto.Env["CUDA_VISIBLE_DEVICES"] != "${AGENT_ENV:GPU_IDS}" {
		t.Fatalf("env = %#v, want the placeholder preserved verbatim", dto.Env)
	}
	if len(dto.GPUs) != 1 || dto.GPUs[0].Index != 0 || dto.GPUs[0].VRAMEstimateMB != 8000 || dto.GPUs[0].VRAMMeasuredMB != 0 {
		t.Fatalf("gpus = %#v", dto.GPUs)
	}

	// Read back via GetRuntimeSpec.
	got, err := svc.GetRuntimeSpec(ctx, ownerToken(), mapping.ID)
	if err != nil {
		t.Fatalf("GetRuntimeSpec: %v", err)
	}
	if got.ID != dto.ID || got.Binary != dto.Binary || len(got.GPUs) != 1 {
		t.Fatalf("GetRuntimeSpec round trip = %#v, want %#v", got, dto)
	}

	// A second PUT (upsert) preserves id/created_at and never lets the
	// client clobber vram_measured_mb -- simulate a prior agent measurement,
	// then PUT again with a different estimate AND an explicit (bogus)
	// VRAMMeasuredMB on the wire, and confirm the measured value survives
	// untouched. The request deliberately sends a non-zero VRAMMeasuredMB
	// here (rather than leaving it at zero) so this assertion cannot be
	// satisfied by accident just because Go's zero value happens to look
	// like "field omitted" -- it pins that the field is read from the
	// stored row, never from the request, at all.
	if err := routeStore.UpdateRuntimeSpecGPUMeasured(ctx, dto.ID, 0, 7500); err != nil {
		t.Fatalf("seed measured value: %v", err)
	}
	dto2, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{
		Enabled: true,
		Binary:  "/usr/local/bin/llama-server",
		Args:    []string{"--model", "/models/q.gguf"},
		GPUs:    []RuntimeSpecGPUDTO{{Index: 0, VRAMEstimateMB: 9000, VRAMMeasuredMB: 99999}},
	})
	if err != nil {
		t.Fatalf("PutRuntimeSpec (second): %v", err)
	}
	if dto2.ID != dto.ID {
		t.Fatalf("id changed on upsert: got %q, want %q", dto2.ID, dto.ID)
	}
	if len(dto2.GPUs) != 1 || dto2.GPUs[0].VRAMEstimateMB != 9000 {
		t.Fatalf("estimate not updated: gpus = %#v", dto2.GPUs)
	}
	if dto2.GPUs[0].VRAMMeasuredMB != 7500 {
		t.Fatalf("measured value clobbered: got %d, want preserved 7500 (request sent bogus 99999)", dto2.GPUs[0].VRAMMeasuredMB)
	}
}

func TestPutRuntimeSpecValidation(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	agentApp := seedServerAgentApplication(t, routeStore, server.ID, now)
	agentMapping, err := svc.CreateMapping(ctx, ownerToken(), agentApp.ID, CreateMappingRequest{GatewayModelName: "m1", AppModelName: "m1"})
	if err != nil {
		t.Fatalf("CreateMapping (agent): %v", err)
	}
	vllmApp, err := svc.CreateApplication(ctx, ownerToken(), server.ID, CreateApplicationRequest{Type: routing.ProviderVLLM, Port: 8000, Scheme: "https"})
	if err != nil {
		t.Fatalf("CreateApplication (vllm): %v", err)
	}
	vllmMapping, err := svc.CreateMapping(ctx, ownerToken(), vllmApp.ID, CreateMappingRequest{GatewayModelName: "m2", AppModelName: "m2"})
	if err != nil {
		t.Fatalf("CreateMapping (vllm): %v", err)
	}

	base := func() PutRuntimeSpecRequest {
		return PutRuntimeSpecRequest{Binary: "/usr/bin/thing"}
	}

	cases := []struct {
		name      string
		mappingID string
		mutate    func(PutRuntimeSpecRequest) PutRuntimeSpecRequest
		wantErr   error
	}{
		{
			name:      "empty binary",
			mappingID: agentMapping.ID,
			mutate:    func(r PutRuntimeSpecRequest) PutRuntimeSpecRequest { r.Binary = ""; return r },
			wantErr:   ErrRuntimeSpecBinaryRequired,
		},
		{
			name:      "relative binary",
			mappingID: agentMapping.ID,
			mutate:    func(r PutRuntimeSpecRequest) PutRuntimeSpecRequest { r.Binary = "bin/thing"; return r },
			wantErr:   ErrRuntimeSpecBinaryRequired,
		},
		{
			name:      "negative idle timeout",
			mappingID: agentMapping.ID,
			mutate:    func(r PutRuntimeSpecRequest) PutRuntimeSpecRequest { r.IdleTimeoutSeconds = -1; return r },
			wantErr:   ErrRuntimeSpecTuningInvalid,
		},
		{
			name:      "bad admin state",
			mappingID: agentMapping.ID,
			mutate:    func(r PutRuntimeSpecRequest) PutRuntimeSpecRequest { r.AdminState = "bogus"; return r },
			wantErr:   ErrRuntimeSpecAdminStateInvalid,
		},
		{
			name:      "duplicate gpu index",
			mappingID: agentMapping.ID,
			mutate: func(r PutRuntimeSpecRequest) PutRuntimeSpecRequest {
				r.GPUs = []RuntimeSpecGPUDTO{{Index: 0, VRAMEstimateMB: 100}, {Index: 0, VRAMEstimateMB: 200}}
				return r
			},
			wantErr: ErrRuntimeSpecGPUInvalid,
		},
		{
			name:      "spec on non-server_agent app",
			mappingID: vllmMapping.ID,
			mutate:    func(r PutRuntimeSpecRequest) PutRuntimeSpecRequest { return r },
			wantErr:   ErrRuntimeSpecNotServerAgent,
		},
		{
			name:      "bad responses_mode",
			mappingID: agentMapping.ID,
			mutate:    func(r PutRuntimeSpecRequest) PutRuntimeSpecRequest { r.ResponsesMode = "bogus"; return r },
			wantErr:   ErrRuntimeSpecEndpointModeInvalid,
		},
		{
			name:      "bad api_flavor",
			mappingID: agentMapping.ID,
			mutate: func(r PutRuntimeSpecRequest) PutRuntimeSpecRequest {
				r.APIFlavors = []string{"openai", "bogus"}
				return r
			},
			wantErr: ErrRuntimeSpecFlavorInvalid,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.PutRuntimeSpec(ctx, ownerToken(), tc.mappingID, tc.mutate(base()))
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestRuntimeSpecDTOCarriesFlavorsAndModes pins that RuntimeSpecDTO/
// PutRuntimeSpecRequest carry api_flavors + responses_mode/messages_mode,
// and that both PutRuntimeSpec and GetRuntimeSpec round-trip them.
func TestRuntimeSpecDTOCarriesFlavorsAndModes(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "qwen", AppModelName: "qwen"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}

	dto, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{
		Enabled:       true,
		Binary:        "/usr/local/bin/llama-server",
		APIFlavors:    []string{routing.APIFlavorOpenAI},
		ResponsesMode: string(routing.EndpointModeTranslate),
		MessagesMode:  string(routing.EndpointModeDisabled),
	})
	if err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	if len(dto.APIFlavors) != 1 || dto.APIFlavors[0] != routing.APIFlavorOpenAI {
		t.Fatalf("api_flavors = %#v", dto.APIFlavors)
	}
	if dto.ResponsesMode != string(routing.EndpointModeTranslate) || dto.MessagesMode != string(routing.EndpointModeDisabled) {
		t.Fatalf("modes = %q/%q", dto.ResponsesMode, dto.MessagesMode)
	}
	got, err := svc.GetRuntimeSpec(ctx, ownerToken(), mapping.ID)
	if err != nil {
		t.Fatalf("GetRuntimeSpec: %v", err)
	}
	if len(got.APIFlavors) != 1 || got.ResponsesMode != string(routing.EndpointModeTranslate) {
		t.Fatalf("read-back = %#v", got)
	}
}

// TestPutRuntimeSpecModeAndFlavorDefaults pins the backend defaults applied
// when a PUT omits api_flavors/responses_mode/messages_mode: flavors default
// to both, and each mode defaults to passthrough (mirrors the application
// create-path default, Task A2).
func TestPutRuntimeSpecModeAndFlavorDefaults(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "m", AppModelName: "m"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	dto, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{
		Enabled: true, Binary: "/usr/local/bin/llama-server",
		// APIFlavors / ResponsesMode / MessagesMode absent
	})
	if err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	if len(dto.APIFlavors) != 2 {
		t.Fatalf("api_flavors default = %#v, want both", dto.APIFlavors)
	}
	if dto.ResponsesMode != string(routing.EndpointModePassthrough) ||
		dto.MessagesMode != string(routing.EndpointModePassthrough) {
		t.Fatalf("mode defaults = %q/%q, want passthrough", dto.ResponsesMode, dto.MessagesMode)
	}
}

// TestPutRuntimeSpecDoesNotInheritAppModes pins the "no backend inheritance"
// contract (spec §5.4/§12): the runtime-spec PUT is a full-document upsert
// with no field inheritance from the parent server_agent application -- the
// frontend pre-fills a create form from the app's current values, but the
// backend only ever supplies its own passthrough/both defaults for absent
// fields. seedServerAgentApplication's app carries a non-default APIFlavors
// ([openai] only, no anthropic) and zero-value (i.e. "") modes; a PUT that
// omits the trio must land on the backend's OWN defaults, not the parent
// app's stored values.
func TestPutRuntimeSpecDoesNotInheritAppModes(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	// Seed a server_agent app whose modes are non-default (disabled).
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "m", AppModelName: "m"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	dto, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{
		Enabled: true, Binary: "/usr/local/bin/llama-server",
	})
	if err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	// Backend default is passthrough, NOT the app's stored value.
	if dto.ResponsesMode != string(routing.EndpointModePassthrough) {
		t.Fatalf("responses_mode = %q, want passthrough (no backend inheritance)", dto.ResponsesMode)
	}
	// Backend default flavors is both, NOT the app's stored [openai]-only value.
	if len(dto.APIFlavors) != 2 {
		t.Fatalf("api_flavors = %#v, want both (no backend inheritance from app's [openai]-only value)", dto.APIFlavors)
	}
}

// TestAgentRuntimeConfigOmitsFlavorsAndModes GUARDS spec §9: the agent-wire
// AgentRuntimeConfig/AgentRuntimeSpecDTO must never gain api_flavors/
// responses_mode/messages_mode -- those three fields are gateway-side only,
// decided before dispatch to the agent, and never enter the runtime.Spec the
// agent's local router consumes. Marshals the JSON for a spec carrying
// non-default flavors/modes and asserts none of the three keys leak through.
func TestAgentRuntimeConfigOmitsFlavorsAndModes(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "m", AppModelName: "m"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{
		Enabled: true, Binary: "/usr/local/bin/llama-server",
		APIFlavors: []string{routing.APIFlavorOpenAI}, ResponsesMode: string(routing.EndpointModeDisabled),
	}); err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	cfg, err := svc.AgentRuntimeConfig(ctx, server.ID)
	if err != nil {
		t.Fatalf("AgentRuntimeConfig: %v", err)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, k := range []string{"api_flavors", "responses_mode", "messages_mode"} {
		if strings.Contains(string(raw), k) {
			t.Fatalf("agent runtime-config leaked %q: %s", k, raw)
		}
	}
}

// TestDeleteRuntimeSpecAllowedAfterApplicationRetyped pins a fix (review
// finding on the first cut of this file): a runtime spec must remain
// deletable even after its owning application is retyped away from
// server_agent through the ordinary UpdateApplication path, which has no
// check against the application's CURRENT type. Before the fix,
// DeleteRuntimeSpec mirrored PutRuntimeSpec's server_agent gate, which made
// such a spec permanently stuck: PutRuntimeSpec would refuse to touch it
// (gated) and DeleteRuntimeSpec would too, with DeleteApplication itself
// never cascade-cleaning specs -- the only remaining escape was
// DeleteMapping, far more destructive than clearing one stray spec. A
// cleanup operation must never be blockable by the same gate that prevents
// creating a NEW dependency on server_agent semantics.
func TestDeleteRuntimeSpecAllowedAfterApplicationRetyped(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "m", AppModelName: "m"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{Binary: "/bin/x"}); err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}

	retype := routing.ProviderVLLM
	if _, err := svc.UpdateApplication(ctx, ownerToken(), app.ID, UpdateApplicationRequest{Type: &retype}); err != nil {
		t.Fatalf("UpdateApplication (retype to vllm): %v", err)
	}

	// GET stays permissive regardless of the (now non-server_agent) type.
	if dto, err := svc.GetRuntimeSpec(ctx, ownerToken(), mapping.ID); err != nil || !dto.Configured {
		t.Fatalf("GetRuntimeSpec after retype = %#v, %v, want Configured:true, nil err", dto, err)
	}

	// PutRuntimeSpec stays gated -- creating/replacing still needs server_agent.
	if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{Binary: "/bin/y"}); !errors.Is(err, ErrRuntimeSpecNotServerAgent) {
		t.Fatalf("PutRuntimeSpec after retype = %v, want ErrRuntimeSpecNotServerAgent", err)
	}

	// DeleteRuntimeSpec must still succeed: this is the fix under test.
	if err := svc.DeleteRuntimeSpec(ctx, ownerToken(), mapping.ID); err != nil {
		t.Fatalf("DeleteRuntimeSpec after retype = %v, want success", err)
	}
	dto, err := svc.GetRuntimeSpec(ctx, ownerToken(), mapping.ID)
	if err != nil {
		t.Fatalf("GetRuntimeSpec after delete: %v", err)
	}
	if dto.Configured {
		t.Fatalf("Configured = true after delete, want false")
	}
}

// TestRuntimeSpecAuthorization proves the runtime-spec endpoints share
// authorizeMapping's exact chain: system scope / owner / admin-group
// delegate (co-manager with CanManageServers) succeed, and everyone else --
// including a plain "admin"-scope principal with NO ownership or delegation
// on this particular server -- collapses to ErrMappingNotFound (404-no-leak,
// never a distinguishable 403).
func TestRuntimeSpecAuthorization(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore, dir := newServerTestServiceWithDir(t, now)

	if err := dir.CreateUser(ctx, store.User{ID: "usr_manager", Email: "usr_manager@example.test", DisplayName: "Manager", Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateUser usr_manager: %v", err)
	}
	// A FRESH admin group, deliberately distinct from testAdminGroupID (which
	// usr_admin/adminToken() owns): usr_admin must have NO relationship to
	// this one, so adminToken() proves it needs actual delegation, not just
	// the "admin" scope. usr_manager is granted full co-manager rights
	// (SetUserGroupManager's default, including CanManageServers) on it.
	const delegateGroupID = "ugrp_runtime_delegate"
	if err := dir.CreateUserGroup(ctx, store.UserGroup{
		ID: delegateGroupID, Tier: store.GroupTierAdmin, Name: "Runtime Delegate",
		ParentGroupID: testSystemGroupID, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create delegate group: %v", err)
	}
	if err := dir.SetUserGroupMember(ctx, delegateGroupID, "usr_manager", store.GroupStateMember, ""); err != nil {
		t.Fatalf("add usr_manager to delegate group: %v", err)
	}
	if err := dir.SetUserGroupManager(ctx, delegateGroupID, "usr_manager"); err != nil {
		t.Fatalf("promote usr_manager: %v", err)
	}

	// systemToken() bypasses the admin-group-linkage "must already manage
	// this group" check (validateAdminGroupScope), so it can create a server
	// linked to delegateGroupID even though usr_system manages nothing.
	server, err := svc.CreateServer(ctx, systemToken(), CreateServerRequest{
		Name: "S", Domain: "s.example.test",
		OwnerIDs: []string{"usr_owner"}, AdminGroupIDs: []string{delegateGroupID},
	})
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "m", AppModelName: "m"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}

	// otherToken(): no ownership, no group relationship at all -> not-found
	// on every method, before any spec exists.
	if _, err := svc.GetRuntimeSpec(ctx, otherToken(), mapping.ID); !errors.Is(err, ErrMappingNotFound) {
		t.Fatalf("otherToken GET = %v, want ErrMappingNotFound", err)
	}
	if _, err := svc.PutRuntimeSpec(ctx, otherToken(), mapping.ID, PutRuntimeSpecRequest{Binary: "/bin/x"}); !errors.Is(err, ErrMappingNotFound) {
		t.Fatalf("otherToken PUT = %v, want ErrMappingNotFound", err)
	}
	if err := svc.DeleteRuntimeSpec(ctx, otherToken(), mapping.ID); !errors.Is(err, ErrMappingNotFound) {
		t.Fatalf("otherToken DELETE = %v, want ErrMappingNotFound", err)
	}

	// adminToken() alone: "admin" scope only, no "system" scope, and no
	// ownership/delegation on THIS server (unlike testAdminGroupID) -> also
	// not-found on every method.
	if _, err := svc.GetRuntimeSpec(ctx, adminToken(), mapping.ID); !errors.Is(err, ErrMappingNotFound) {
		t.Fatalf("adminToken GET = %v, want ErrMappingNotFound", err)
	}
	if _, err := svc.PutRuntimeSpec(ctx, adminToken(), mapping.ID, PutRuntimeSpecRequest{Binary: "/bin/x"}); !errors.Is(err, ErrMappingNotFound) {
		t.Fatalf("adminToken PUT = %v, want ErrMappingNotFound", err)
	}
	if err := svc.DeleteRuntimeSpec(ctx, adminToken(), mapping.ID); !errors.Is(err, ErrMappingNotFound) {
		t.Fatalf("adminToken DELETE = %v, want ErrMappingNotFound", err)
	}

	// ownerToken() (usr_owner is in OwnerIDs) succeeds -- creates the spec.
	if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{Binary: "/bin/x"}); err != nil {
		t.Fatalf("ownerToken PUT: %v", err)
	}

	// The admin-group delegate (usr_manager, co-manager with CanManageServers
	// on delegateGroupID) also succeeds, on both a read and a write.
	managerToken := auth.Token{UserID: "usr_manager", Scopes: []string{"gateway:use"}}
	spec, err := svc.GetRuntimeSpec(ctx, managerToken, mapping.ID)
	if err != nil {
		t.Fatalf("manager GET: %v", err)
	}
	if !spec.Configured {
		t.Fatalf("manager GET spec.Configured = false, want true (owner already created it)")
	}
	if err := svc.DeleteRuntimeSpec(ctx, managerToken, mapping.ID); err != nil {
		t.Fatalf("manager DELETE: %v", err)
	}
}

func TestGetRuntimeSpecUnconfigured(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "m", AppModelName: "m"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}

	dto, err := svc.GetRuntimeSpec(ctx, ownerToken(), mapping.ID)
	if err != nil {
		t.Fatalf("GetRuntimeSpec: %v", err)
	}
	if dto.Configured {
		t.Fatalf("Configured = true, want false (no spec created yet)")
	}
	if dto.MappingID != mapping.ID {
		t.Fatalf("mapping id = %q, want %q", dto.MappingID, mapping.ID)
	}
	if dto.GPUs == nil || len(dto.GPUs) != 0 {
		t.Fatalf("gpus = %#v, want non-nil empty", dto.GPUs)
	}
	if dto.Args == nil || len(dto.Args) != 0 {
		t.Fatalf("args = %#v, want non-nil empty", dto.Args)
	}
	if dto.Env == nil || len(dto.Env) != 0 {
		t.Fatalf("env = %#v, want non-nil empty", dto.Env)
	}
}

func TestPutRuntimeSpecFiresRuntimeChangedHook(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	dir := NewMemoryDirectory(auth.NewTokenStore())
	for _, u := range []string{"usr_admin", "usr_owner", "usr_other"} {
		if err := dir.CreateUser(ctx, store.User{ID: u, Email: u + "@example.test", DisplayName: u, Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateUser %s: %v", u, err)
		}
	}
	seedServerTestGroups(t, dir, now)
	routeStore := routing.NewMemoryStore()
	var mu sync.Mutex
	var calls []string
	svc := NewService(ServiceDeps{
		Users: dir, Groups: dir, Routes: routeStore, Clock: func() time.Time { return now },
		OnRuntimeConfigChanged: func(serverID string) {
			mu.Lock()
			defer mu.Unlock()
			calls = append(calls, serverID)
		},
	})

	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	// Store-seeded: going through svc.CreateMapping would fire the hook too
	// (see notifyRuntimeChangedForMapping), and this test is about the SPEC
	// write paths alone.
	mapping := seedMapping(t, routeStore, app.ID, "m", now)

	if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{Binary: "/bin/x"}); err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	mu.Lock()
	gotAfterPut := append([]string(nil), calls...)
	mu.Unlock()
	if len(gotAfterPut) != 1 || gotAfterPut[0] != server.ID {
		t.Fatalf("calls after PUT = %#v, want exactly [%q]", gotAfterPut, server.ID)
	}

	if err := svc.DeleteRuntimeSpec(ctx, ownerToken(), mapping.ID); err != nil {
		t.Fatalf("DeleteRuntimeSpec: %v", err)
	}
	mu.Lock()
	gotAfterDelete := append([]string(nil), calls...)
	mu.Unlock()
	if len(gotAfterDelete) != 2 || gotAfterDelete[1] != server.ID {
		t.Fatalf("calls after DELETE = %#v, want exactly [%q, %q]", gotAfterDelete, server.ID, server.ID)
	}
}

// --- Task 6: co-residency matrix -------------------------------------------

// seedMapping writes one ACTIVE mapping straight to the store, exactly as
// CreateMapping would for a request carrying no metrics (no MetricsSource
// stamp, status defaulted to active, IsMTP derived from the name). Used
// wherever a test needs a mapping to EXIST rather than to exercise the
// create path -- notably in the runtime-config-changed hook tests, where
// going through svc.CreateMapping would itself fire the hook (a mapping
// under the server_agent application is a runtime-config input; see
// notifyRuntimeChangedForMapping) and blur what the assertion is about.
// Mirrors seedServerAgentApplication, which exists for the same reason.
func seedMapping(t *testing.T, routeStore *routing.MemoryStore, appID, name string, now time.Time) ModelMappingDTO {
	t.Helper()
	mapping := routing.ModelMapping{
		ID:               "map_" + compactRandomHex(16),
		ApplicationID:    appID,
		GatewayModelName: name,
		AppModelName:     name,
		Status:           routing.ServerStatusActive,
		IsMTP:            routing.IsMTPModelName(name),
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := routeStore.CreateMapping(context.Background(), mapping); err != nil {
		t.Fatalf("seed mapping %q: %v", name, err)
	}
	return mappingDTO(mapping)
}

// seedTwoMappings creates a server_agent application on server and two
// mappings on it, for co-residency tests that need a pair of mapping ids
// belonging to the SAME application.
func seedTwoMappings(t *testing.T, routeStore *routing.MemoryStore, serverID string, now time.Time) (routing.Application, ModelMappingDTO, ModelMappingDTO) {
	t.Helper()
	app := seedServerAgentApplication(t, routeStore, serverID, now)
	return app, seedMapping(t, routeStore, app.ID, "m1", now), seedMapping(t, routeStore, app.ID, "m2", now)
}

func TestSetCoResidencyCanonicalizesPair(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app, m1, m2 := seedTwoMappings(t, routeStore, server.ID, now)

	// Submit the pair with the higher id first -- SetCoResidency must sort it
	// server-side so the client never has to care about ordering.
	a, b := m1.ID, m2.ID
	if a > b {
		a, b = b, a
	}
	dto, err := svc.SetCoResidency(ctx, ownerToken(), app.ID, SetCoResidencyRequest{Pairs: [][2]string{{b, a}}})
	if err != nil {
		t.Fatalf("SetCoResidency: %v", err)
	}
	if len(dto.Pairs) != 1 || dto.Pairs[0][0] != a || dto.Pairs[0][1] != b {
		t.Fatalf("pairs = %#v, want canonical [[%q,%q]]", dto.Pairs, a, b)
	}

	got, err := svc.GetCoResidency(ctx, ownerToken(), app.ID)
	if err != nil {
		t.Fatalf("GetCoResidency: %v", err)
	}
	if len(got.Pairs) != 1 || got.Pairs[0][0] != a || got.Pairs[0][1] != b {
		t.Fatalf("GetCoResidency pairs = %#v, want canonical [[%q,%q]]", got.Pairs, a, b)
	}
}

func TestGetCoResidencyEmptyIsNonNil(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)

	got, err := svc.GetCoResidency(ctx, ownerToken(), app.ID)
	if err != nil {
		t.Fatalf("GetCoResidency: %v", err)
	}
	if got.Pairs == nil || len(got.Pairs) != 0 {
		t.Fatalf("pairs = %#v, want non-nil empty", got.Pairs)
	}
}

// TestSetCoResidencyRejectsDuplicateAfterNormalization pins the exact case
// called out by the task brief: submitting the same unordered pair twice,
// once in each order, must be rejected as a duplicate AFTER normalization --
// not silently accepted as "two different pairs".
func TestSetCoResidencyRejectsDuplicateAfterNormalization(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app, m1, m2 := seedTwoMappings(t, routeStore, server.ID, now)

	_, err := svc.SetCoResidency(ctx, ownerToken(), app.ID, SetCoResidencyRequest{
		Pairs: [][2]string{{m1.ID, m2.ID}, {m2.ID, m1.ID}},
	})
	if !errors.Is(err, ErrCoResidencyPairInvalid) {
		t.Fatalf("err = %v, want ErrCoResidencyPairInvalid", err)
	}

	// The EXACT duplicate (same pair, same order) must be rejected by the same
	// validation. Asserted explicitly rather than assumed from the reversed
	// case above: the store layer now also rejects it (the composite primary
	// key on the SQL side, a matching guard in MemoryStore), and this is the
	// portal-level half of that pair of guards -- the one an operator
	// actually sees, with the honest 400 code instead of a store conflict.
	_, err = svc.SetCoResidency(ctx, ownerToken(), app.ID, SetCoResidencyRequest{
		Pairs: [][2]string{{m1.ID, m2.ID}, {m1.ID, m2.ID}},
	})
	if !errors.Is(err, ErrCoResidencyPairInvalid) {
		t.Fatalf("exact duplicate pair: err = %v, want ErrCoResidencyPairInvalid", err)
	}
}

func TestSetCoResidencyRejectsSameMappingTwice(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app, m1, _ := seedTwoMappings(t, routeStore, server.ID, now)

	_, err := svc.SetCoResidency(ctx, ownerToken(), app.ID, SetCoResidencyRequest{
		Pairs: [][2]string{{m1.ID, m1.ID}},
	})
	if !errors.Is(err, ErrCoResidencyPairInvalid) {
		t.Fatalf("err = %v, want ErrCoResidencyPairInvalid", err)
	}
}

// TestSetCoResidencyRejectsForeignMapping pins that a pair naming a mapping
// belonging to a DIFFERENT application is invalid -- verified against the
// application's own mappings, not merely a global existence check.
func TestSetCoResidencyRejectsForeignMapping(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app, m1, _ := seedTwoMappings(t, routeStore, server.ID, now)

	// A separate server (seedServerAgentApplication hardcodes port 9000, so a
	// second application on the SAME server would collide on port instead)
	// with its own server_agent application and mapping -- "foreign" only in
	// the sense that matters here: a mapping id that exists but belongs to a
	// DIFFERENT application than app.
	otherServer := createTestServer(t, svc, "S2", "s2.example.test")
	otherApp := seedServerAgentApplication(t, routeStore, otherServer.ID, now)
	foreign, err := svc.CreateMapping(ctx, ownerToken(), otherApp.ID, CreateMappingRequest{GatewayModelName: "f", AppModelName: "f"})
	if err != nil {
		t.Fatalf("CreateMapping foreign: %v", err)
	}

	_, err = svc.SetCoResidency(ctx, ownerToken(), app.ID, SetCoResidencyRequest{
		Pairs: [][2]string{{m1.ID, foreign.ID}},
	})
	if !errors.Is(err, ErrCoResidencyPairInvalid) {
		t.Fatalf("err = %v, want ErrCoResidencyPairInvalid", err)
	}
}

func TestSetCoResidencyFiresRuntimeChangedHookAndClearsOnEmpty(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	dir := NewMemoryDirectory(auth.NewTokenStore())
	for _, u := range []string{"usr_admin", "usr_owner", "usr_other"} {
		if err := dir.CreateUser(ctx, store.User{ID: u, Email: u + "@example.test", DisplayName: u, Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateUser %s: %v", u, err)
		}
	}
	seedServerTestGroups(t, dir, now)
	routeStore := routing.NewMemoryStore()
	var mu sync.Mutex
	var calls []string
	svc := NewService(ServiceDeps{
		Users: dir, Groups: dir, Routes: routeStore, Clock: func() time.Time { return now },
		OnRuntimeConfigChanged: func(serverID string) {
			mu.Lock()
			defer mu.Unlock()
			calls = append(calls, serverID)
		},
	})
	server := createTestServer(t, svc, "S", "s.example.test")
	app, m1, m2 := seedTwoMappings(t, routeStore, server.ID, now)

	if _, err := svc.SetCoResidency(ctx, ownerToken(), app.ID, SetCoResidencyRequest{Pairs: [][2]string{{m1.ID, m2.ID}}}); err != nil {
		t.Fatalf("SetCoResidency: %v", err)
	}
	// Clearing the set (empty Pairs) is a valid write too and must also fire
	// the hook -- an empty PUT is how an operator removes every rule.
	if _, err := svc.SetCoResidency(ctx, ownerToken(), app.ID, SetCoResidencyRequest{}); err != nil {
		t.Fatalf("SetCoResidency (clear): %v", err)
	}
	mu.Lock()
	got := append([]string(nil), calls...)
	mu.Unlock()
	if len(got) != 2 || got[0] != server.ID || got[1] != server.ID {
		t.Fatalf("calls = %#v, want exactly [%q, %q]", got, server.ID, server.ID)
	}
	final, err := svc.GetCoResidency(ctx, ownerToken(), app.ID)
	if err != nil {
		t.Fatalf("GetCoResidency: %v", err)
	}
	if len(final.Pairs) != 0 {
		t.Fatalf("pairs after clear = %#v, want empty", final.Pairs)
	}
}

// --- Task 6: per-GPU VRAM budgets -------------------------------------------

func TestSetServerGPUBudgetsRoundTrip(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")

	dto, err := svc.SetServerGPUBudgets(ctx, ownerToken(), server.ID, SetGPUBudgetsRequest{
		Budgets: []GPUBudgetDTO{{Index: 1, BudgetMB: 4000}, {Index: 0, BudgetMB: 8000}},
	})
	if err != nil {
		t.Fatalf("SetServerGPUBudgets: %v", err)
	}
	if len(dto) != 2 || dto[0].Index != 0 || dto[0].BudgetMB != 8000 || dto[1].Index != 1 || dto[1].BudgetMB != 4000 {
		t.Fatalf("dto = %#v, want ordered by index", dto)
	}
	// No telemetry sample exists -- expected_* stay empty rather than failing.
	if dto[0].ExpectedUUID != "" || dto[0].ExpectedName != "" {
		t.Fatalf("dto[0] = %#v, want empty expected_* (no telemetry sample)", dto[0])
	}

	got, err := svc.GetServerGPUBudgets(ctx, ownerToken(), server.ID)
	if err != nil {
		t.Fatalf("GetServerGPUBudgets: %v", err)
	}
	if len(got) != 2 || got[0].BudgetMB != 8000 || got[1].BudgetMB != 4000 {
		t.Fatalf("GetServerGPUBudgets round trip = %#v", got)
	}
}

func TestGetServerGPUBudgetsEmptyIsNonNil(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")

	got, err := svc.GetServerGPUBudgets(ctx, ownerToken(), server.ID)
	if err != nil {
		t.Fatalf("GetServerGPUBudgets: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("budgets = %#v, want non-nil empty", got)
	}
}

func TestSetServerGPUBudgetsValidation(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")

	cases := []struct {
		name    string
		budgets []GPUBudgetDTO
	}{
		{"negative index", []GPUBudgetDTO{{Index: -1, BudgetMB: 100}}},
		{"negative budget_mb", []GPUBudgetDTO{{Index: 0, BudgetMB: -1}}},
		{"duplicate index", []GPUBudgetDTO{{Index: 0, BudgetMB: 100}, {Index: 0, BudgetMB: 200}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.SetServerGPUBudgets(ctx, ownerToken(), server.ID, SetGPUBudgetsRequest{Budgets: tc.budgets})
			if !errors.Is(err, ErrGPUBudgetInvalid) {
				t.Fatalf("err = %v, want ErrGPUBudgetInvalid", err)
			}
		})
	}
}

// TestSetServerGPUBudgetsZeroIsAcceptedAndMeansUnconstrained pins the other
// half of the validation contract, which TestSetServerGPUBudgetsValidation
// only implies: `BudgetMB < 0` is refused, and `BudgetMB == 0` is therefore
// ACCEPTED on purpose. It is not an oversight to be tightened later --
// `routing.ServerGPUBudget.BudgetMB` defines 0 as "no budget for this GPU" =
// unconstrained, identical to a GPU with no row at all, and the agent's
// admission policy (runtime.Admit in server-agent/internal/runtime/policy.go)
// honours that by skipping any index whose budget is <= 0.
//
// So a row an operator cleared to 0 is a no-op, not an error and not a hard
// refusal of every model on that card. Making this method reject 0 instead
// would break the runtime screen, whose limits form seeds a brand-new budget
// row at 0 MB and turns a cleared MB input into 0, and it would leave the
// gateway and the agent disagreeing about a value the store already persists.
func TestSetServerGPUBudgetsZeroIsAcceptedAndMeansUnconstrained(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")

	dto, err := svc.SetServerGPUBudgets(ctx, ownerToken(), server.ID, SetGPUBudgetsRequest{
		Budgets: []GPUBudgetDTO{{Index: 0, BudgetMB: 0}, {Index: 1, BudgetMB: 8000}},
	})
	if err != nil {
		t.Fatalf("SetServerGPUBudgets with budget_mb 0 = %v, want nil: 0 is unconstrained, not invalid", err)
	}
	if len(dto) != 2 || dto[0].Index != 0 || dto[0].BudgetMB != 0 {
		t.Fatalf("dto = %#v, want the 0 budget stored verbatim at index 0", dto)
	}

	// It must also SURVIVE the round trip as 0 -- silently dropping the row
	// would reach the same admission outcome today by accident, while
	// destroying the operator's row and the expected_* drift snapshot on it.
	got, err := svc.GetServerGPUBudgets(ctx, ownerToken(), server.ID)
	if err != nil {
		t.Fatalf("GetServerGPUBudgets: %v", err)
	}
	if len(got) != 2 || got[0].Index != 0 || got[0].BudgetMB != 0 || got[1].BudgetMB != 8000 {
		t.Fatalf("round trip = %#v, want [{0 0} {1 8000}]", got)
	}
}

// TestSetServerGPUBudgetsSnapshotsExpectedAndNeverOverwrites pins the drift-
// detector contract: expected_uuid/expected_name are captured from the
// latest telemetry sample ONLY when the budget row is first created, and a
// later PUT -- even one whose request carries different (here: deliberately
// bogus) values, and even after a NEWER telemetry sample reports a
// different card at the same index -- must never overwrite them.
func TestSetServerGPUBudgetsSnapshotsExpectedAndNeverOverwrites(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")

	if err := routeStore.InsertTelemetrySample(ctx, routing.TelemetrySample{
		ServerID: server.ID, ReportedAt: now,
		GPUs: []routing.GPUSample{{Index: 0, UUID: "GPU-111", Name: "RTX 4090"}},
	}); err != nil {
		t.Fatalf("seed telemetry sample: %v", err)
	}

	dto, err := svc.SetServerGPUBudgets(ctx, ownerToken(), server.ID, SetGPUBudgetsRequest{
		Budgets: []GPUBudgetDTO{{Index: 0, BudgetMB: 8000}},
	})
	if err != nil {
		t.Fatalf("SetServerGPUBudgets (create): %v", err)
	}
	if len(dto) != 1 || dto[0].ExpectedUUID != "GPU-111" || dto[0].ExpectedName != "RTX 4090" {
		t.Fatalf("dto = %#v, want expected_uuid/expected_name snapshotted from telemetry", dto)
	}

	// A later telemetry sample reports a DIFFERENT card at the same index
	// (the renumbering/renaming scenario the drift detector exists for).
	if err := routeStore.InsertTelemetrySample(ctx, routing.TelemetrySample{
		ServerID: server.ID, ReportedAt: now.Add(time.Minute),
		GPUs: []routing.GPUSample{{Index: 0, UUID: "GPU-222", Name: "RTX 5090"}},
	}); err != nil {
		t.Fatalf("seed second telemetry sample: %v", err)
	}

	// The second PUT changes budget_mb and sends bogus expected_* values on
	// the wire (a client would normally just echo back the GET response, but
	// this pins that the server NEVER trusts the request for these fields,
	// on a create OR an update).
	dto2, err := svc.SetServerGPUBudgets(ctx, ownerToken(), server.ID, SetGPUBudgetsRequest{
		Budgets: []GPUBudgetDTO{{Index: 0, BudgetMB: 9000, ExpectedUUID: "bogus-uuid", ExpectedName: "bogus-name"}},
	})
	if err != nil {
		t.Fatalf("SetServerGPUBudgets (update): %v", err)
	}
	if len(dto2) != 1 || dto2[0].BudgetMB != 9000 {
		t.Fatalf("budget_mb not updated: dto2 = %#v", dto2)
	}
	if dto2[0].ExpectedUUID != "GPU-111" || dto2[0].ExpectedName != "RTX 4090" {
		t.Fatalf("expected_* clobbered: got %#v, want the ORIGINAL snapshot (GPU-111/RTX 4090) preserved", dto2[0])
	}
}

func TestSetServerGPUBudgetsFiresRuntimeChangedHook(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	dir := NewMemoryDirectory(auth.NewTokenStore())
	for _, u := range []string{"usr_admin", "usr_owner", "usr_other"} {
		if err := dir.CreateUser(ctx, store.User{ID: u, Email: u + "@example.test", DisplayName: u, Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateUser %s: %v", u, err)
		}
	}
	seedServerTestGroups(t, dir, now)
	routeStore := routing.NewMemoryStore()
	var mu sync.Mutex
	var calls []string
	svc := NewService(ServiceDeps{
		Users: dir, Groups: dir, Routes: routeStore, Clock: func() time.Time { return now },
		OnRuntimeConfigChanged: func(serverID string) {
			mu.Lock()
			defer mu.Unlock()
			calls = append(calls, serverID)
		},
	})
	server := createTestServer(t, svc, "S", "s.example.test")

	if _, err := svc.SetServerGPUBudgets(ctx, ownerToken(), server.ID, SetGPUBudgetsRequest{Budgets: []GPUBudgetDTO{{Index: 0, BudgetMB: 1000}}}); err != nil {
		t.Fatalf("SetServerGPUBudgets: %v", err)
	}
	mu.Lock()
	got := append([]string(nil), calls...)
	mu.Unlock()
	if len(got) != 1 || got[0] != server.ID {
		t.Fatalf("calls = %#v, want exactly [%q]", got, server.ID)
	}
}

// --- Task 6: server runtime flags -------------------------------------------

func TestServerRuntimeFlagsCreateAndUpdateRoundTrip(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, _ := newServerTestService(t, now)

	maxProc := 3
	managed := true
	dto, err := svc.CreateServer(ctx, adminToken(), CreateServerRequest{
		Name: "S", Domain: "s.example.test", OwnerIDs: []string{"usr_owner"},
		AdminGroupIDs:       []string{testAdminGroupID},
		RuntimeMaxProcesses: &maxProc, ManagedRuntimeOnly: &managed,
	})
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	if dto.RuntimeMaxProcesses != 3 || !dto.ManagedRuntimeOnly {
		t.Fatalf("create dto = %#v, want RuntimeMaxProcesses=3, ManagedRuntimeOnly=true", dto)
	}

	newMax := 5
	unmanaged := false
	updated, err := svc.UpdateServer(ctx, ownerToken(), dto.ID, UpdateServerRequest{
		RuntimeMaxProcesses: &newMax, ManagedRuntimeOnly: &unmanaged,
	})
	if err != nil {
		t.Fatalf("UpdateServer: %v", err)
	}
	if updated.RuntimeMaxProcesses != 5 || updated.ManagedRuntimeOnly {
		t.Fatalf("update dto = %#v, want RuntimeMaxProcesses=5, ManagedRuntimeOnly=false", updated)
	}
}

func TestCreateServerRuntimeMaxProcessesNegativeInvalid(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, _ := newServerTestService(t, now)
	neg := -1
	_, err := svc.CreateServer(ctx, adminToken(), CreateServerRequest{
		Name: "S", Domain: "s.example.test", OwnerIDs: []string{"usr_owner"},
		AdminGroupIDs: []string{testAdminGroupID}, RuntimeMaxProcesses: &neg,
	})
	if !errors.Is(err, ErrServerRuntimeLimitInvalid) {
		t.Fatalf("err = %v, want ErrServerRuntimeLimitInvalid", err)
	}
}

func TestUpdateServerRuntimeMaxProcessesNegativeInvalid(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	neg := -1
	_, err := svc.UpdateServer(ctx, ownerToken(), server.ID, UpdateServerRequest{RuntimeMaxProcesses: &neg})
	if !errors.Is(err, ErrServerRuntimeLimitInvalid) {
		t.Fatalf("err = %v, want ErrServerRuntimeLimitInvalid", err)
	}
}

// TestCreateApplicationManagedRuntimeOnlyGate pins the managed-runtime-only
// gate: a non-server_agent application create on a ManagedRuntimeOnly server
// is rejected with ErrServerManagedRuntimeOnly, while a "server_agent" create
// on that same server is exempt from the gate and now (Task 10 registered
// "server_agent" as a creatable application type) genuinely SUCCEEDS -- that
// is the whole point of the ManagedRuntimeOnly flag: such a server hosts
// agent-managed model processes and nothing else. (Before Task 10 this test
// could only assert that the server_agent create failed for the separate,
// pre-existing reason that CreateApplication did not yet accept
// "server_agent" as a creatable type at all; now that gate is gone, the test
// asserts the real, positive behavior instead.)
func TestCreateApplicationManagedRuntimeOnlyGate(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, _ := newServerTestService(t, now)
	managed := true
	server, err := svc.CreateServer(ctx, adminToken(), CreateServerRequest{
		Name: "S", Domain: "s.example.test", OwnerIDs: []string{"usr_owner"},
		AdminGroupIDs: []string{testAdminGroupID}, ManagedRuntimeOnly: &managed,
	})
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}

	_, err = svc.CreateApplication(ctx, ownerToken(), server.ID, CreateApplicationRequest{Type: routing.ProviderVLLM, Port: 8000, Scheme: "https"})
	if !errors.Is(err, ErrServerManagedRuntimeOnly) {
		t.Fatalf("vllm create on managed-only server: err = %v, want ErrServerManagedRuntimeOnly", err)
	}

	dto, err := svc.CreateApplication(ctx, ownerToken(), server.ID, CreateApplicationRequest{Type: routing.ProviderServerAgent, Port: 9000, Scheme: "http"})
	if err != nil {
		t.Fatalf("server_agent create on managed-only server: err = %v, want success", err)
	}
	if dto.Type != routing.ProviderServerAgent {
		t.Fatalf("dto.Type = %q, want server_agent", dto.Type)
	}

	// A normal (non-managed-only) server is unaffected.
	plain := createTestServer(t, svc, "S2", "s2.example.test")
	if _, err := svc.CreateApplication(ctx, ownerToken(), plain.ID, CreateApplicationRequest{Type: routing.ProviderVLLM, Port: 8000, Scheme: "https"}); err != nil {
		t.Fatalf("vllm create on a plain server: %v, want success", err)
	}
}

// --- Task 6: runtime timeout warning ----------------------------------------

func TestRuntimeWarningsTimeoutBelowStartup(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "m", AppModelName: "m"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}

	// No runtime spec yet -- non-nil empty, no warning.
	warnings, err := svc.RuntimeWarnings(ctx, ownerToken(), app.ID)
	if err != nil {
		t.Fatalf("RuntimeWarnings: %v", err)
	}
	if warnings == nil || len(warnings) != 0 {
		t.Fatalf("warnings = %#v, want non-nil empty", warnings)
	}

	// An ENABLED spec with a 60s startup timeout (60000ms) exceeds the
	// application's stored TimeoutMS (seedServerAgentApplication leaves it at
	// the zero value) -- must warn.
	if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{
		Binary: "/bin/x", Enabled: true, StartupTimeoutSeconds: 60,
	}); err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	warnings, err = svc.RuntimeWarnings(ctx, ownerToken(), app.ID)
	if err != nil {
		t.Fatalf("RuntimeWarnings: %v", err)
	}
	if len(warnings) != 1 || warnings[0] != "timeout_ms_below_startup_timeout" {
		t.Fatalf("warnings = %#v, want [timeout_ms_below_startup_timeout]", warnings)
	}

	// Raising the application's TimeoutMS to/above the 60000ms threshold
	// clears the warning.
	raised := 90000
	if _, err := svc.UpdateApplication(ctx, ownerToken(), app.ID, UpdateApplicationRequest{TimeoutMS: &raised}); err != nil {
		t.Fatalf("UpdateApplication: %v", err)
	}
	warnings, err = svc.RuntimeWarnings(ctx, ownerToken(), app.ID)
	if err != nil {
		t.Fatalf("RuntimeWarnings: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings after raising timeout_ms = %#v, want none", warnings)
	}
}

// TestRuntimeWarningsIgnoresDisabledSpecs pins the "ENABLED specs" wording:
// a disabled spec's startup timeout must never count toward the max.
func TestRuntimeWarningsIgnoresDisabledSpecs(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "m", AppModelName: "m"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	// Enabled:false with a huge startup timeout -- must not trigger the
	// warning even though the application's TimeoutMS (zero value here) is
	// nowhere near it.
	if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{
		Binary: "/bin/x", Enabled: false, StartupTimeoutSeconds: 600,
	}); err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	warnings, err := svc.RuntimeWarnings(ctx, ownerToken(), app.ID)
	if err != nil {
		t.Fatalf("RuntimeWarnings: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %#v, want none (spec is disabled)", warnings)
	}
}

// --- Task 7: agent runtime-config assembly ----------------------------------

// TestAgentRuntimeConfigAssembly pins the GET /api/agent/v1/runtime-config
// wire contract (spec §11): RouterListen is the server_agent application's
// Port, only ENABLED specs appear, coresidency pairs are translated from
// mapping ids to spec ids (dropping any pair touching a disabled/missing
// spec), per-GPU VRAMMB prefers the measured value over the estimate, and a
// server with no server_agent application yet gets the fully empty document
// -- never an error.
func TestAgentRuntimeConfigAssembly(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")

	// Before any server_agent application exists, the agent must be able to
	// poll harmlessly: a fully empty, non-nil, stably-etagged document.
	empty, err := svc.AgentRuntimeConfig(ctx, server.ID)
	if err != nil {
		t.Fatalf("AgentRuntimeConfig (no app): %v", err)
	}
	if empty.RouterListen != 0 || empty.MaxProcesses != 0 {
		t.Fatalf("empty dto = %#v, want RouterListen=0, MaxProcesses=0", empty)
	}
	if empty.Specs == nil || len(empty.Specs) != 0 {
		t.Fatalf("empty.Specs = %#v, want non-nil empty", empty.Specs)
	}
	if empty.Coresident == nil || len(empty.Coresident) != 0 {
		t.Fatalf("empty.Coresident = %#v, want non-nil empty", empty.Coresident)
	}
	if empty.GPUBudgets == nil || len(empty.GPUBudgets) != 0 {
		t.Fatalf("empty.GPUBudgets = %#v, want non-nil empty", empty.GPUBudgets)
	}
	if empty.ETag == "" {
		t.Fatalf("empty dto must still carry a stable etag")
	}
	emptyAgain, err := svc.AgentRuntimeConfig(ctx, server.ID)
	if err != nil {
		t.Fatalf("AgentRuntimeConfig (no app, second call): %v", err)
	}
	if emptyAgain.ETag != empty.ETag {
		t.Fatalf("empty etag not stable: %q vs %q", empty.ETag, emptyAgain.ETag)
	}

	app := seedServerAgentApplication(t, routeStore, server.ID, now) // Port 9000
	maxProc := 3
	if _, err := svc.UpdateServer(ctx, ownerToken(), server.ID, UpdateServerRequest{RuntimeMaxProcesses: &maxProc}); err != nil {
		t.Fatalf("UpdateServer: %v", err)
	}
	if _, err := svc.SetServerGPUBudgets(ctx, ownerToken(), server.ID, SetGPUBudgetsRequest{
		Budgets: []GPUBudgetDTO{{Index: 1, BudgetMB: 21500}, {Index: 0, BudgetMB: 46000}},
	}); err != nil {
		t.Fatalf("SetServerGPUBudgets: %v", err)
	}

	mappingA, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "qwen-coder", AppModelName: "qwen2.5-coder-32b"})
	if err != nil {
		t.Fatalf("CreateMapping A: %v", err)
	}
	specA, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingA.ID, PutRuntimeSpecRequest{
		Enabled: true,
		Binary:  "/usr/bin/vllm",
		Args:    []string{"--tensor-parallel-size", "2"},
		Env:     map[string]string{"HF_TOKEN": "${AGENT_ENV:HF_TOKEN}"},
		GPUs:    []RuntimeSpecGPUDTO{{Index: 0, VRAMEstimateMB: 22000}, {Index: 1, VRAMEstimateMB: 21500}},
	})
	if err != nil {
		t.Fatalf("PutRuntimeSpec A: %v", err)
	}
	// GPU 0 has a measured value on file -- must win over the estimate. GPU 1
	// has no measurement -- must fall back to the estimate.
	if err := routeStore.UpdateRuntimeSpecGPUMeasured(ctx, specA.ID, 0, 22500); err != nil {
		t.Fatalf("seed measured value: %v", err)
	}

	mappingB, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "llama-small", AppModelName: "llama-3-8b"})
	if err != nil {
		t.Fatalf("CreateMapping B: %v", err)
	}
	specB, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingB.ID, PutRuntimeSpecRequest{
		Enabled: true,
		Binary:  "/usr/bin/llama-server",
		GPUs:    []RuntimeSpecGPUDTO{{Index: 0, VRAMEstimateMB: 8000}},
	})
	if err != nil {
		t.Fatalf("PutRuntimeSpec B: %v", err)
	}

	mappingDisabled, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "disabled-model", AppModelName: "disabled-model"})
	if err != nil {
		t.Fatalf("CreateMapping disabled: %v", err)
	}
	if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingDisabled.ID, PutRuntimeSpecRequest{
		Enabled: false,
		Binary:  "/usr/bin/disabled-server",
	}); err != nil {
		t.Fatalf("PutRuntimeSpec disabled: %v", err)
	}

	// Coresidency: A+B is a real allowed pair; A+disabled must be dropped
	// (its spec is disabled, hence absent from the wire document).
	if _, err := svc.SetCoResidency(ctx, ownerToken(), app.ID, SetCoResidencyRequest{
		Pairs: [][2]string{{mappingA.ID, mappingB.ID}, {mappingA.ID, mappingDisabled.ID}},
	}); err != nil {
		t.Fatalf("SetCoResidency: %v", err)
	}

	dto, err := svc.AgentRuntimeConfig(ctx, server.ID)
	if err != nil {
		t.Fatalf("AgentRuntimeConfig: %v", err)
	}
	if dto.RouterListen != app.Port {
		t.Fatalf("router_listen = %d, want the server_agent application's port %d", dto.RouterListen, app.Port)
	}
	if dto.MaxProcesses != 3 {
		t.Fatalf("max_processes = %d, want 3", dto.MaxProcesses)
	}
	if len(dto.GPUBudgets) != 2 || dto.GPUBudgets[0].Index != 0 || dto.GPUBudgets[0].BudgetMB != 46000 ||
		dto.GPUBudgets[1].Index != 1 || dto.GPUBudgets[1].BudgetMB != 21500 {
		t.Fatalf("gpu_budgets = %#v, want ordered by index [{0 46000} {1 21500}]", dto.GPUBudgets)
	}
	if len(dto.Specs) != 2 {
		t.Fatalf("specs = %#v, want exactly 2 (the disabled spec must be absent)", dto.Specs)
	}
	var gotA, gotB *AgentRuntimeSpecDTO
	for i := range dto.Specs {
		switch dto.Specs[i].ID {
		case specA.ID:
			gotA = &dto.Specs[i]
		case specB.ID:
			gotB = &dto.Specs[i]
		}
	}
	if gotA == nil || gotB == nil {
		t.Fatalf("specs = %#v, want both specA (%s) and specB (%s)", dto.Specs, specA.ID, specB.ID)
	}
	if gotA.Model != "qwen-coder" || gotA.UpstreamModel != "qwen2.5-coder-32b" {
		t.Fatalf("specA model/upstream_model = %q/%q, want qwen-coder/qwen2.5-coder-32b", gotA.Model, gotA.UpstreamModel)
	}
	if gotA.Binary != "/usr/bin/vllm" || len(gotA.Args) != 2 || gotA.Args[0] != "--tensor-parallel-size" {
		t.Fatalf("specA binary/args = %#v", gotA)
	}
	if gotA.Env["HF_TOKEN"] != "${AGENT_ENV:HF_TOKEN}" {
		t.Fatalf("specA env = %#v, want the placeholder preserved verbatim", gotA.Env)
	}
	if len(gotA.GPUs) != 2 || gotA.GPUs[0].Index != 0 || gotA.GPUs[0].VRAMMB != 22500 ||
		gotA.GPUs[1].Index != 1 || gotA.GPUs[1].VRAMMB != 21500 {
		t.Fatalf("specA gpus = %#v, want [{0 22500} {1 21500}] (measured overrides estimate on GPU 0, estimate used on GPU 1)", gotA.GPUs)
	}
	if gotA.HealthPath != "/health" || gotA.HealthTimeoutSeconds != 5 || gotA.StartupTimeoutSeconds != 180 {
		t.Fatalf("specA defaults = %#v", gotA)
	}
	if gotB.Model != "llama-small" || gotB.UpstreamModel != "llama-3-8b" {
		t.Fatalf("specB model/upstream_model = %q/%q", gotB.Model, gotB.UpstreamModel)
	}

	if len(dto.Coresident) != 1 {
		t.Fatalf("coresident = %#v, want exactly 1 pair (the disabled-spec pair must be dropped)", dto.Coresident)
	}
	pair := dto.Coresident[0]
	if !((pair[0] == specA.ID && pair[1] == specB.ID) || (pair[0] == specB.ID && pair[1] == specA.ID)) {
		t.Fatalf("coresident[0] = %#v, want {%s,%s} in either order", pair, specA.ID, specB.ID)
	}
	for _, p := range dto.Coresident {
		if p[0] == mappingDisabled.ID || p[1] == mappingDisabled.ID {
			t.Fatalf("coresident leaked a MAPPING id instead of a SPEC id: %#v", p)
		}
	}

	if dto.ETag == "" {
		t.Fatalf("dto.ETag must not be empty")
	}
	dtoAgain, err := svc.AgentRuntimeConfig(ctx, server.ID)
	if err != nil {
		t.Fatalf("AgentRuntimeConfig (second call): %v", err)
	}
	if dtoAgain.ETag != dto.ETag {
		t.Fatalf("etag not stable across identical calls: %q vs %q", dto.ETag, dtoAgain.ETag)
	}
	if dtoAgain.ETag == empty.ETag {
		t.Fatalf("a populated config must not share the empty config's etag")
	}
}

// TestAgentRuntimeConfigUnknownServerIsEmptyNotError mirrors AgentProxyRoutes'
// "a missing/unreadable server resolves to the safe empty default" posture: a
// serverID naming no AIServer at all must never surface as an error to the
// agent-token holder.
func TestAgentRuntimeConfigUnknownServerIsEmptyNotError(t *testing.T) {
	svc, _ := newServerTestService(t, time.Now())
	dto, err := svc.AgentRuntimeConfig(context.Background(), "no-such-server")
	if err != nil {
		t.Fatalf("AgentRuntimeConfig: %v", err)
	}
	if dto.Specs == nil || len(dto.Specs) != 0 || dto.RouterListen != 0 {
		t.Fatalf("dto = %#v, want the fully empty document", dto)
	}
}

// TestAgentRuntimeConfigEnvNullNormalizedToEmptyObject pins the nil-vs-empty
// wire-shape defect class (already caught twice on this branch, and fixed a
// third time in agentRuntimeSpecDTO/runtimeSpecDTO/the gateway handler's
// defensive loop as part of this test): a stored spec whose env column holds
// the literal JSON "null" -- something PutRuntimeSpec itself can never
// produce (it always marshals a non-nil map before storing), but a
// plausible outcome of a direct store write or a future writer that
// bypasses PutRuntimeSpec's normalization -- must still marshal as
// "env":{} on the wire, never "env":null. Asserted on the marshaled JSON
// BYTES, not the Go map, since the whole point is the wire shape a
// JSON-strict agent parser sees.
func TestAgentRuntimeConfigEnvNullNormalizedToEmptyObject(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "m", AppModelName: "m"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	// Write directly at the store layer: PutRuntimeSpec always marshals a
	// non-nil map for Env, so the literal "null" string can only be produced
	// by bypassing it.
	spec := routing.RuntimeSpec{
		ID:        "rspec_" + compactRandomHex(16),
		MappingID: mapping.ID,
		Enabled:   true,
		Binary:    "/usr/bin/x",
		Args:      "[]",
		Env:       "null",
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := routeStore.UpsertRuntimeSpec(ctx, spec); err != nil {
		t.Fatalf("seed spec with literal null env: %v", err)
	}

	dto, err := svc.AgentRuntimeConfig(ctx, server.ID)
	if err != nil {
		t.Fatalf("AgentRuntimeConfig: %v", err)
	}
	raw, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("marshal dto: %v", err)
	}
	if strings.Contains(string(raw), `"env":null`) {
		t.Fatalf("wire body = %s, want \"env\":{} not \"env\":null", raw)
	}
	if !strings.Contains(string(raw), `"env":{}`) {
		t.Fatalf("wire body = %s, want an explicit \"env\":{}", raw)
	}
}

// TestGetRuntimeSpecEnvNullNormalizedToEmptyObject covers the sibling
// portal-facing path (runtimeSpecDTO, Task 5) that has the identical
// nil-vs-empty gap the review flagged: GetRuntimeSpec must never marshal
// "env":null either.
func TestGetRuntimeSpecEnvNullNormalizedToEmptyObject(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "m", AppModelName: "m"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	spec := routing.RuntimeSpec{
		ID:        "rspec_" + compactRandomHex(16),
		MappingID: mapping.ID,
		Enabled:   true,
		Binary:    "/usr/bin/x",
		Args:      "[]",
		Env:       "null",
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := routeStore.UpsertRuntimeSpec(ctx, spec); err != nil {
		t.Fatalf("seed spec with literal null env: %v", err)
	}

	dto, err := svc.GetRuntimeSpec(ctx, ownerToken(), mapping.ID)
	if err != nil {
		t.Fatalf("GetRuntimeSpec: %v", err)
	}
	raw, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("marshal dto: %v", err)
	}
	if strings.Contains(string(raw), `"env":null`) {
		t.Fatalf("wire body = %s, want \"env\":{} not \"env\":null", raw)
	}
	if !strings.Contains(string(raw), `"env":{}`) {
		t.Fatalf("wire body = %s, want an explicit \"env\":{}", raw)
	}
}

// --- Task 9: ServerRuntimeReportView -----------------------------------------

// TestServerRuntimeReportViewAbsent proves the hardware-panel model: a
// server with no runtime report EVER stored returns Available:false (not an
// error), with a non-nil-but-empty AgentFeatures even though no telemetry
// has been reported either.
func TestServerRuntimeReportViewAbsent(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")

	dto, err := svc.ServerRuntimeReportView(ctx, ownerToken(), server.ID)
	if err != nil {
		t.Fatalf("ServerRuntimeReportView: %v", err)
	}
	if dto.Available {
		t.Fatalf("dto = %#v, want Available=false", dto)
	}
	if dto.CollectedAt != "" || dto.UpdatedAt != "" || len(dto.Report) != 0 {
		t.Fatalf("absent report must leave the report fields empty: %#v", dto)
	}
	if dto.AgentVersion != "" {
		t.Fatalf("agent_version = %q, want empty (no telemetry)", dto.AgentVersion)
	}
	if dto.AgentFeatures == nil || len(dto.AgentFeatures) != 0 {
		t.Fatalf("agent_features = %#v, want non-nil empty", dto.AgentFeatures)
	}
}

// TestServerRuntimeReportViewFound proves a stored report is surfaced
// verbatim (this method never parses/rewrites Report -- that is the ingest
// layer's job) alongside agent_version/agent_features parsed from the
// server's latest telemetry row, independent of the report itself.
func TestServerRuntimeReportViewFound(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")

	if err := routeStore.UpsertTelemetry(ctx, routing.ServerTelemetry{
		ServerID: server.ID, ReportedAt: now, AgentVersion: "2.0.1",
		Capabilities: `{"features":["runtime_manager"]}`, ProviderHealth: "{}", RawSummary: "{}", UpdatedAt: now,
	}); err != nil {
		t.Fatalf("UpsertTelemetry: %v", err)
	}
	reportJSON := `{"source":"file","collected_at":"2026-07-11T12:00:00Z","config":{"specs":[]}}`
	if err := routeStore.UpsertServerRuntimeReport(ctx, routing.ServerRuntimeReport{
		ServerID: server.ID, CollectedAt: now, ReportJSON: reportJSON, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("UpsertServerRuntimeReport: %v", err)
	}

	dto, err := svc.ServerRuntimeReportView(ctx, ownerToken(), server.ID)
	if err != nil {
		t.Fatalf("ServerRuntimeReportView: %v", err)
	}
	if !dto.Available || dto.CollectedAt == "" || dto.UpdatedAt == "" {
		t.Fatalf("dto = %#v, want Available with timestamps", dto)
	}
	if string(dto.Report) != reportJSON {
		t.Fatalf("report = %s, want the stored blob verbatim: %s", dto.Report, reportJSON)
	}
	if dto.AgentVersion != "2.0.1" {
		t.Fatalf("agent_version = %q, want 2.0.1", dto.AgentVersion)
	}
	if len(dto.AgentFeatures) != 1 || dto.AgentFeatures[0] != "runtime_manager" {
		t.Fatalf("agent_features = %#v", dto.AgentFeatures)
	}
}

// TestServerRuntimeReportViewMalformedCapabilitiesYieldsEmptyFeatures proves
// a garbled/forward-incompatible telemetry capabilities blob never rejects
// the whole read -- it degrades to an empty (non-nil) feature list, the same
// tolerant-parse discipline the gateway ingest layer's parseAgentCapabilities
// uses.
func TestServerRuntimeReportViewMalformedCapabilitiesYieldsEmptyFeatures(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	if err := routeStore.UpsertTelemetry(ctx, routing.ServerTelemetry{
		ServerID: server.ID, ReportedAt: now, AgentVersion: "1.0.0",
		Capabilities: `"not an object"`, ProviderHealth: "{}", RawSummary: "{}", UpdatedAt: now,
	}); err != nil {
		t.Fatalf("UpsertTelemetry: %v", err)
	}

	dto, err := svc.ServerRuntimeReportView(ctx, ownerToken(), server.ID)
	if err != nil {
		t.Fatalf("ServerRuntimeReportView: %v", err)
	}
	if dto.AgentVersion != "1.0.0" {
		t.Fatalf("agent_version = %q, want 1.0.0 (unaffected by the malformed capabilities)", dto.AgentVersion)
	}
	if dto.AgentFeatures == nil || len(dto.AgentFeatures) != 0 {
		t.Fatalf("agent_features = %#v, want non-nil empty on malformed capabilities", dto.AgentFeatures)
	}
}

// TestServerRuntimeReportViewForeignUserForbidden proves authorizeServer
// gates this read like every other server-scoped one: a non-owner gets
// ErrServerNotFound (404-no-leak upstream), never the report contents.
func TestServerRuntimeReportViewForeignUserForbidden(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	if err := routeStore.UpsertServerRuntimeReport(ctx, routing.ServerRuntimeReport{
		ServerID: server.ID, CollectedAt: now, ReportJSON: `{}`, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("UpsertServerRuntimeReport: %v", err)
	}

	if _, err := svc.ServerRuntimeReportView(ctx, otherToken(), server.ID); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("err = %v, want ErrServerNotFound", err)
	}
}

// failAIServerByIDStore injects an AIServerByID failure onto an otherwise-real
// routing.Store, mirroring service_agent_proxy_routes_test.go's
// failApplicationsByServerStore.
type failAIServerByIDStore struct {
	routing.Store
	err error
}

func (s *failAIServerByIDStore) AIServerByID(context.Context, string) (routing.AIServer, error) {
	return routing.AIServer{}, s.err
}

// TestAgentRuntimeConfigPropagatesTransientServerReadFailure is C1's covering
// test. A TRANSIENT store failure on the AIServer read -- a dropped Postgres
// connection, a SQLite contention window, or the context.DeadlineExceeded
// PushRuntimeConfig's own 5s bound produces -- must surface as an error, NOT
// as the empty document with err == nil.
//
// The empty-document-with-nil-error answer is not merely imprecise, it is the
// most destructive value this function can return: the agent parses it
// happily, overwrites its on-disk cache with it, tears down its bound router
// listener and drains every running spec. And because it comes back with
// err == nil, it also walks straight through gateway.PushRuntimeConfig's
// never-push-on-error guard, so the WS path pushes the teardown with no HTTP
// status involved at all. See AgentRuntimeConfig's own doc comment.
func TestAgentRuntimeConfigPropagatesTransientServerReadFailure(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	transient := errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"connection failure", transient},
		// The PushRuntimeConfig trigger specifically: that method bounds this
		// call with pushRuntimeConfigTimeout, so a slow store surfaces here as
		// a deadline, not as a store-shaped error.
		{"context deadline", context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, routeStore := newServerTestService(t, now)
			server := createTestServer(t, svc, "S", "s.example.test")
			seedServerAgentApplication(t, routeStore, server.ID, now)

			failing := NewService(ServiceDeps{
				Users:  svc.users,
				Groups: svc.groups,
				Routes: &failAIServerByIDStore{Store: routeStore, err: tc.err},
				Clock:  func() time.Time { return now },
			})
			dto, err := failing.AgentRuntimeConfig(context.Background(), server.ID)
			if err == nil {
				t.Fatalf("AgentRuntimeConfig returned dto = %#v with a nil error; a transient store failure must be propagated, never collapsed into the empty document", dto)
			}
			if !errors.Is(err, tc.err) {
				t.Fatalf("err = %v, want it to wrap %v", err, tc.err)
			}
		})
	}
}

// TestAgentRuntimeConfigVRAMLockedServesTheOperatorEstimate is the escape
// hatch for the measurement ratchet, and the scenario is the one an operator
// actually lands in.
//
// The loop is closed and one-way: the agent measures its own child, the
// gateway stores it, this DTO prefers measured over estimate, the agent reads
// it back as the spec's OWN DECLARED DEMAND, and Admit's rule 3 answers a
// demand above the GPU's budget with a TERMINAL not_permitted. A 24 GB card
// budgeted at 20000 MB for headroom, an 18000 MB estimate that served fine,
// and one 22000 MB measurement (llama.cpp with a large KV cache) is enough:
// from then on every start of a model that had been working is refused, with
// no operator action having occurred anywhere.
//
// And it could not be undone. PutRuntimeSpec deliberately copies the stored
// measured value forward and ignores what the request sends; the write-back
// skips values <= 0; and the spec never runs again, so no newer measurement
// is ever taken. Raising the budget above what the card physically has is a
// capitulation, not a lever, and deleting and re-adding the GPU row across
// two saves is not something an operator can be expected to discover.
//
// vram_locked is that lever, and this is what makes it one: it must stop the
// gateway SERVING the measurement, not merely stop the agent WRITING it.
// Locked, the operator's own estimate is authoritative in both directions --
// which is the plain meaning of "locked" and the only reading under which the
// flag is any use to someone staring at a spec that will not start.
func TestAgentRuntimeConfigVRAMLockedServesTheOperatorEstimate(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)

	// 20000 MB of a 24 GB card: the headroom is deliberate.
	if _, err := svc.SetServerGPUBudgets(ctx, ownerToken(), server.ID, SetGPUBudgetsRequest{
		Budgets: []GPUBudgetDTO{{Index: 0, BudgetMB: 20000}},
	}); err != nil {
		t.Fatalf("SetServerGPUBudgets: %v", err)
	}
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "llama", AppModelName: "llama-3-8b"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	base := PutRuntimeSpecRequest{
		Enabled: true,
		Binary:  "/usr/bin/llama-server",
		GPUs:    []RuntimeSpecGPUDTO{{Index: 0, VRAMEstimateMB: 18000}},
	}
	spec, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, base)
	if err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	// The agent measured its own child at 22000 MB and wrote it back.
	if err := routeStore.UpdateRuntimeSpecGPUMeasured(ctx, spec.ID, 0, 22000); err != nil {
		t.Fatalf("seed measured value: %v", err)
	}

	// Precondition: the trap is armed. The agent is handed a declared demand
	// of 22000 MB against its own 20000 MB budget -- terminal not_permitted.
	dto, err := svc.AgentRuntimeConfig(ctx, server.ID)
	if err != nil {
		t.Fatalf("AgentRuntimeConfig: %v", err)
	}
	if len(dto.Specs) != 1 || len(dto.Specs[0].GPUs) != 1 {
		t.Fatalf("dto.Specs = %#v, want one spec with one GPU", dto.Specs)
	}
	if got := dto.Specs[0].GPUs[0].VRAMMB; got != 22000 {
		t.Fatalf("unlocked vram_mb = %d, want the measured 22000 -- this test's premise (measured wins) does not hold", got)
	}

	// The operator's lever: pin the VRAM numbers. Same estimate, same GPU row.
	locked := base
	locked.VRAMLocked = true
	if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, locked); err != nil {
		t.Fatalf("PutRuntimeSpec (locked): %v", err)
	}

	dto2, err := svc.AgentRuntimeConfig(ctx, server.ID)
	if err != nil {
		t.Fatalf("AgentRuntimeConfig (locked): %v", err)
	}
	if got := dto2.Specs[0].GPUs[0].VRAMMB; got != 18000 {
		t.Fatalf("locked vram_mb = %d, want the operator's own estimate 18000 -- BUG F3: vram_locked stops the agent WRITING a measurement but the gateway keeps SERVING the stored one, so a spec made terminally not_permitted by a measurement has no operator lever at all", got)
	}

	// The measurement itself stays on file: it is the evidence that tells the
	// operator WHY they had to intervene, and the portal renders it.
	gpus, err := routeStore.RuntimeSpecGPUs(ctx, spec.ID)
	if err != nil || len(gpus) != 1 {
		t.Fatalf("RuntimeSpecGPUs: gpus=%#v err=%v", gpus, err)
	}
	if gpus[0].VRAMMeasuredMB != 22000 {
		t.Fatalf("stored vram_measured_mb = %d, want the measurement preserved at 22000 -- locking pins which number the agent is TOLD, it must not erase the observation", gpus[0].VRAMMeasuredMB)
	}
	if gpus[0].VRAMEstimateMB != 18000 {
		t.Fatalf("stored vram_estimate_mb = %d, want 18000", gpus[0].VRAMEstimateMB)
	}

	// Unlocking hands the measurement back: the agent stays the owner of the
	// number, the operator only chooses whether to be governed by it.
	if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, base); err != nil {
		t.Fatalf("PutRuntimeSpec (unlocked again): %v", err)
	}
	dto3, err := svc.AgentRuntimeConfig(ctx, server.ID)
	if err != nil {
		t.Fatalf("AgentRuntimeConfig (unlocked again): %v", err)
	}
	if got := dto3.Specs[0].GPUs[0].VRAMMB; got != 22000 {
		t.Fatalf("re-unlocked vram_mb = %d, want the measured 22000 back", got)
	}
}

// --- set_visible_devices -----------------------------------------------------

// seedVisibleDevicesMapping builds the server + server_agent application +
// mapping the set_visible_devices tests below all need, and returns the
// mapping id.
func seedVisibleDevicesMapping(t *testing.T, svc *Service, routeStore *routing.MemoryStore, now time.Time) string {
	t.Helper()
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	mapping, err := svc.CreateMapping(context.Background(), ownerToken(), app.ID, CreateMappingRequest{
		GatewayModelName: "qwen", AppModelName: "qwen-upstream",
	})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	return mapping.ID
}

// TestPutRuntimeSpecSetVisibleDevicesRoundTrip proves the flag survives the
// whole write path — request -> store -> portal DTO -> the agent's
// runtime-config document. The last hop is the one that matters: the flag is
// useless unless the AGENT sees it, and the agent is the only party that knows
// which visibility variable this host's hardware wants.
func TestPutRuntimeSpecSetVisibleDevicesRoundTrip(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "qwen", AppModelName: "qwen-upstream"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}

	dto, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{
		Enabled:           true,
		Binary:            "/usr/local/bin/llama-server",
		SetVisibleDevices: true,
		GPUs:              []RuntimeSpecGPUDTO{{Index: 2, VRAMEstimateMB: 8000}, {Index: 5, VRAMEstimateMB: 8000}},
	})
	if err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	if !dto.SetVisibleDevices {
		t.Fatalf("dto.SetVisibleDevices = false, want true")
	}
	got, err := svc.GetRuntimeSpec(ctx, ownerToken(), mapping.ID)
	if err != nil {
		t.Fatalf("GetRuntimeSpec: %v", err)
	}
	if !got.SetVisibleDevices {
		t.Fatalf("GetRuntimeSpec().SetVisibleDevices = false, want true")
	}

	cfg, err := svc.AgentRuntimeConfig(ctx, server.ID)
	if err != nil {
		t.Fatalf("AgentRuntimeConfig: %v", err)
	}
	if len(cfg.Specs) != 1 {
		t.Fatalf("runtime-config specs = %#v, want exactly one", cfg.Specs)
	}
	if !cfg.Specs[0].SetVisibleDevices {
		t.Fatalf("the runtime-config document dropped set_visible_devices: %#v", cfg.Specs[0])
	}
	// The gateway carries the flag and NOTHING about the variable name: it is
	// hardware-agnostic and cannot know whether this host runs NVIDIA or AMD.
	if len(cfg.Specs[0].Env) != 0 {
		t.Errorf("the gateway injected env %#v; resolving the visibility variable is the agent's job alone", cfg.Specs[0].Env)
	}

	// Turning it back off is an ordinary full-document upsert.
	off, err := svc.PutRuntimeSpec(ctx, ownerToken(), mapping.ID, PutRuntimeSpecRequest{
		Binary: "/usr/local/bin/llama-server",
		GPUs:   []RuntimeSpecGPUDTO{{Index: 2, VRAMEstimateMB: 8000}},
	})
	if err != nil {
		t.Fatalf("PutRuntimeSpec (off): %v", err)
	}
	if off.SetVisibleDevices {
		t.Errorf("set_visible_devices survived a PUT that did not ask for it; this endpoint is a full-document replace")
	}
}

// TestPutRuntimeSpecSetVisibleDevicesRequiresGPUs is trap 1 at the save
// boundary: an empty visible-devices value means NOTHING is visible, so the
// option with no GPU rows is refused rather than persisted and handed to the
// agent. (The agent refuses it again at launch, for the file-mode path that
// never passes through here — see validateRuntimeSpecVisibleDevices.)
func TestPutRuntimeSpecSetVisibleDevicesRequiresGPUs(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	mappingID := seedVisibleDevicesMapping(t, svc, routeStore, now)

	for _, gpus := range [][]RuntimeSpecGPUDTO{nil, {}} {
		_, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, PutRuntimeSpecRequest{
			Binary:            "/usr/local/bin/llama-server",
			SetVisibleDevices: true,
			GPUs:              gpus,
		})
		if !errors.Is(err, ErrRuntimeSpecVisibleDevicesNoGPUs) {
			t.Fatalf("PutRuntimeSpec(gpus=%#v) error = %v, want ErrRuntimeSpecVisibleDevicesNoGPUs", gpus, err)
		}
	}

	// Validate-before-mutate: the refused write must have persisted nothing.
	got, err := svc.GetRuntimeSpec(ctx, ownerToken(), mappingID)
	if err != nil {
		t.Fatalf("GetRuntimeSpec: %v", err)
	}
	if got.Configured {
		t.Fatalf("a refused write created a spec row: %#v", got)
	}

	// The same option with a GPU row is accepted — the refusal is about the
	// empty list, not about the option.
	if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, PutRuntimeSpecRequest{
		Binary:            "/usr/local/bin/llama-server",
		SetVisibleDevices: true,
		GPUs:              []RuntimeSpecGPUDTO{{Index: 0}},
	}); err != nil {
		t.Fatalf("PutRuntimeSpec with one gpu row: %v", err)
	}

	// And a spec with NO gpus and the option OFF stays perfectly valid: that
	// is every CPU-only and every pre-feature spec.
	if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, PutRuntimeSpecRequest{
		Binary: "/usr/local/bin/llama-server",
	}); err != nil {
		t.Fatalf("PutRuntimeSpec with no gpus and the option off: %v", err)
	}
}

// TestPutRuntimeSpecSetVisibleDevicesConflictsWithEnv is trap 3 at the save
// boundary: the option and a hand-set visibility variable are two sources for
// one value, and a spec where both are present reads identically whichever one
// wins. Refused, not silently resolved.
//
// The refusal is vendor-independent — all three owned variables, on every
// host — because this gateway cannot know what hardware the target server has,
// and the agent's rule is deliberately written to be the same one.
func TestPutRuntimeSpecSetVisibleDevicesConflictsWithEnv(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	mappingID := seedVisibleDevicesMapping(t, svc, routeStore, now)

	for _, key := range []string{"CUDA_VISIBLE_DEVICES", "ROCR_VISIBLE_DEVICES", "HIP_VISIBLE_DEVICES"} {
		t.Run(key, func(t *testing.T) {
			_, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, PutRuntimeSpecRequest{
				Binary:            "/usr/local/bin/llama-server",
				SetVisibleDevices: true,
				Env:               map[string]string{key: "0,1"},
				GPUs:              []RuntimeSpecGPUDTO{{Index: 0}},
			})
			if !errors.Is(err, ErrRuntimeSpecVisibleDevicesConflict) {
				t.Fatalf("PutRuntimeSpec with env %s error = %v, want ErrRuntimeSpecVisibleDevicesConflict", key, err)
			}
		})
		t.Run(key+" is fine with the option off", func(t *testing.T) {
			dto, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, PutRuntimeSpecRequest{
				Binary: "/usr/local/bin/llama-server",
				Env:    map[string]string{key: "0,1"},
				GPUs:   []RuntimeSpecGPUDTO{{Index: 0}},
			})
			if err != nil {
				t.Fatalf("a hand-set %s with the option off must still save: %v", key, err)
			}
			if dto.Env[key] != "0,1" {
				t.Errorf("env = %#v, want the operator's own %s preserved verbatim", dto.Env, key)
			}
		})
	}

	// ONEAPI_DEVICE_SELECTOR is deliberately NOT owned: composing it with the
	// option (built from ${HOST_GPU_IDS}) is the documented escape hatch for a
	// runtime the agent has no vendor mapping for.
	t.Run("ONEAPI_DEVICE_SELECTOR composes", func(t *testing.T) {
		if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), mappingID, PutRuntimeSpecRequest{
			Binary:            "/usr/local/bin/llama-server",
			SetVisibleDevices: true,
			Env:               map[string]string{"ONEAPI_DEVICE_SELECTOR": "level_zero:${HOST_GPU_IDS}"},
			GPUs:              []RuntimeSpecGPUDTO{{Index: 0}},
		}); err != nil {
			t.Fatalf("ONEAPI_DEVICE_SELECTOR must compose with set_visible_devices: %v", err)
		}
	})
}
