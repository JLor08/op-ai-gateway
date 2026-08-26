// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"sync"
	"testing"
	"time"
)

// seedServerAgentApplication inserts a server_agent-typed application
// directly at the store layer for serverID. The portal's own
// CreateApplication.normalizeApplicationType does not accept "server_agent"
// as a creatable type (routing.ProviderServerAgent applications are
// provisioned by the agent-runtime-manager feature outside the generic
// application form — a later task's concern), so runtime-spec tests seed it
// directly on the store, mirroring seedServerTestGroups' direct-store
// seeding of admin groups for the same reason.
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
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "m", AppModelName: "m"})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}

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

// seedTwoMappings creates a server_agent application on server and two
// mappings on it, for co-residency tests that need a pair of mapping ids
// belonging to the SAME application.
func seedTwoMappings(t *testing.T, svc *Service, routeStore *routing.MemoryStore, serverID string, now time.Time) (routing.Application, ModelMappingDTO, ModelMappingDTO) {
	t.Helper()
	ctx := context.Background()
	app := seedServerAgentApplication(t, routeStore, serverID, now)
	m1, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "m1", AppModelName: "m1"})
	if err != nil {
		t.Fatalf("CreateMapping m1: %v", err)
	}
	m2, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "m2", AppModelName: "m2"})
	if err != nil {
		t.Fatalf("CreateMapping m2: %v", err)
	}
	return app, m1, m2
}

func TestSetCoResidencyCanonicalizesPair(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app, m1, m2 := seedTwoMappings(t, svc, routeStore, server.ID, now)

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
	app, m1, m2 := seedTwoMappings(t, svc, routeStore, server.ID, now)

	_, err := svc.SetCoResidency(ctx, ownerToken(), app.ID, SetCoResidencyRequest{
		Pairs: [][2]string{{m1.ID, m2.ID}, {m2.ID, m1.ID}},
	})
	if !errors.Is(err, ErrCoResidencyPairInvalid) {
		t.Fatalf("err = %v, want ErrCoResidencyPairInvalid", err)
	}
}

func TestSetCoResidencyRejectsSameMappingTwice(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app, m1, _ := seedTwoMappings(t, svc, routeStore, server.ID, now)

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
	app, m1, _ := seedTwoMappings(t, svc, routeStore, server.ID, now)

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
	app, m1, m2 := seedTwoMappings(t, svc, routeStore, server.ID, now)

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
// is rejected with ErrServerManagedRuntimeOnly, but the "server_agent" type
// itself is exempt from THIS gate -- it may still fail for the unrelated,
// pre-existing reason that CreateApplication does not yet accept
// "server_agent" as a creatable type (Task 5 -- registering server_agent
// applications through the generic create form is deliberately a later
// task's concern), but that failure must never be misreported as the
// managed-runtime-only violation.
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

	_, err = svc.CreateApplication(ctx, ownerToken(), server.ID, CreateApplicationRequest{Type: routing.ProviderServerAgent, Port: 9000, Scheme: "http"})
	if errors.Is(err, ErrServerManagedRuntimeOnly) {
		t.Fatalf("server_agent create on managed-only server incorrectly gated: err = %v", err)
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
