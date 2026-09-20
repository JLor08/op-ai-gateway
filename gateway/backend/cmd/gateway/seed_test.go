// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"context"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"testing"
	"time"
)

func TestSeedDefaultServerResolvesSeededModels(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	store := routing.NewMemoryStore()
	if err := seedDefaultServer(ctx, store, now, "", ""); err != nil {
		t.Fatalf("seedDefaultServer: %v", err)
	}
	resolver := routing.NewResolver(store, func() time.Time { return now }, nil)
	for _, model := range []string{"qwen-coder", "gpt-oss-20b"} {
		target, err := resolver.Resolve(ctx, auth.Token{ID: "tok", UserID: "usr", Active: true}, inference.Request{Model: model, APIFlavor: "openai_chat"})
		if err != nil {
			t.Fatalf("Resolve(%s): %v", model, err)
		}
		if target.Provider != routing.ProviderMock {
			t.Fatalf("Resolve(%s) provider = %q, want mock", model, target.Provider)
		}
	}
	if err := seedDefaultServer(ctx, store, now, "", ""); err != nil {
		t.Fatalf("seedDefaultServer (second run): %v", err)
	}
}

func TestSeedDefaultServerSeedsAgentToken(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	store := routing.NewMemoryStore()
	if err := seedDefaultServer(ctx, store, now, "agent-secret", ""); err != nil {
		t.Fatalf("seedDefaultServer: %v", err)
	}
	serverID, ok, err := store.LookupAgentToken(ctx, auth.HashSecret("agent-secret"))
	if err != nil || !ok || serverID != "mock-server" {
		t.Fatalf("LookupAgentToken = %q ok=%v err=%v", serverID, ok, err)
	}
	if err := seedDefaultServer(ctx, store, now, "agent-secret", ""); err != nil {
		t.Fatalf("seedDefaultServer (reseed): %v", err)
	}
	serverID, ok, err = store.LookupAgentToken(ctx, auth.HashSecret("agent-secret"))
	if err != nil || !ok || serverID != "mock-server" {
		t.Fatalf("LookupAgentToken after reseed = %q ok=%v err=%v", serverID, ok, err)
	}
	store2 := routing.NewMemoryStore()
	if err := seedDefaultServer(ctx, store2, now, "", ""); err != nil {
		t.Fatalf("seedDefaultServer empty secret: %v", err)
	}
	if _, ok, _ := store2.AgentTokenByServer(ctx, "mock-server"); ok {
		t.Fatalf("empty secret should not seed an agent token")
	}
}

// TestSeedDevAdminGroupGrantsDevPrincipalAManageableAdminGroup proves the
// fix for issue #122's product gap: a fresh `make dev` (memory-mode) gateway
// seeded no admin-tier group at all, so GET /api/portal/groups returned
// {"admin":[]} and the invite form's submit button (disabled whenever
// adminGroupMissing, i.e. no admin group is auto-selectable) stayed
// permanently disabled -- a brand-new deployment could not invite anyone.
//
// Asserts the PROPERTY through the same call the portal makes
// (portal.Service.ListGroups), not the storage mechanism: the dev principal's
// group landscape must hold exactly one system-tier group and exactly one
// admin-tier group, the admin group's ParentGroupID must point at that
// system group (an admin group with no/foreign parent is a shape
// Service.createAdminGroup can never produce -- see seedDevAdminGroup's own
// doc comment), and the admin group's CanManageUsers must be true for the
// dev principal -- the flag the invite form's auto-select (and thus its
// submit-enabled state) depends on.
//
// devPrincipal deliberately carries NO "system" scope: sessionPrincipal
// (internal/gateway/auth.go) grants that only to a system_admin whose
// session is currently ELEVATED, which a plain password login never is. Had
// this test used a "system"-scoped principal, CanManageUsers would come out
// true via groupDTO's isSystem fallback branch regardless of whether
// ownership was ever actually wired -- proving nothing about the seed. Using
// the real, non-elevated shape means CanManageUsers can only come from
// groupDTO's OWNER branch, which is what this seed is supposed to establish.
func TestSeedDevAdminGroupGrantsDevPrincipalAManageableAdminGroup(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

	dir := portal.NewMemoryDirectory(auth.NewTokenStore())
	if err := dir.CreateUser(ctx, store.User{
		ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev User",
		Role: "system_admin", Status: store.UserStatusActive, PreferredLanguage: "de",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create dev user: %v", err)
	}

	svc := portal.NewService(portal.ServiceDeps{
		Users: dir, Groups: dir, Routes: routing.NewMemoryStore(),
		Clock: func() time.Time { return now },
	})
	devPrincipal := auth.Token{UserID: "usr_dev", Scopes: []string{"gateway:use", "admin"}}

	// Before the seed: this is the exact bug -- no system or admin group
	// exists yet.
	before, err := svc.ListGroups(ctx, devPrincipal)
	if err != nil {
		t.Fatalf("ListGroups (before seed): %v", err)
	}
	if len(before.System) != 0 || len(before.Admin) != 0 {
		t.Fatalf("before seedDevAdminGroup: landscape = %+v, want both System and Admin empty (setup assumption violated)", before)
	}

	if err := seedDevAdminGroup(ctx, dir, now, "usr_dev"); err != nil {
		t.Fatalf("seedDevAdminGroup: %v", err)
	}

	after, err := svc.ListGroups(ctx, devPrincipal)
	if err != nil {
		t.Fatalf("ListGroups (after seed): %v", err)
	}
	if len(after.System) != 1 {
		t.Fatalf("after seedDevAdminGroup: landscape.System = %d groups, want exactly 1 (createAdminGroup's self-owned path requires the creator to be a member of exactly one system group): %+v", len(after.System), after.System)
	}
	if len(after.Admin) != 1 {
		t.Fatalf("after seedDevAdminGroup: landscape.Admin = %d groups, want exactly 1: %+v", len(after.Admin), after.Admin)
	}
	if got, want := after.Admin[0].ParentGroupID, after.System[0].ID; got == "" || got != want {
		t.Fatalf("after seedDevAdminGroup: Admin[0].ParentGroupID = %q, want the seeded system group's id %q -- an orphaned/foreign-parented admin group is a shape createAdminGroup can never produce", got, want)
	}
	if !after.Admin[0].CanManageUsers {
		t.Fatalf("after seedDevAdminGroup: landscape.Admin[0].CanManageUsers = false, want true -- with no \"system\" scope on this principal, this can only come from group OWNERSHIP, which is what the invite form's auto-select/submit-enabled state depends on")
	}

	// Idempotence: mirror seedDefaultServer's guarantee that running the seed
	// twice is safe and never creates a second group of either tier.
	if err := seedDevAdminGroup(ctx, dir, now, "usr_dev"); err != nil {
		t.Fatalf("seedDevAdminGroup (second run): %v", err)
	}
	reseeded, err := svc.ListGroups(ctx, devPrincipal)
	if err != nil {
		t.Fatalf("ListGroups (after reseed): %v", err)
	}
	if len(reseeded.System) != 1 || len(reseeded.Admin) != 1 {
		t.Fatalf("after reseed: landscape = %+v, want exactly 1 System and 1 Admin group (idempotence)", reseeded)
	}
}

// modelVisionByID runs the exact call the portal's chat picker uses
// (gateway/backend/internal/gateway/portal_model_endpoints.go's
// handlePortalModels calls Service.Models for every non-admin-management
// request) and returns each offered model's id -> vision flag. Asserting
// through this call, rather than reading the capability row directly, is
// what proves the fixture is actually visible on the path the frontend
// reads -- an AND-fold bug between the row and the DTO would otherwise slip
// past a row-level assertion.
func modelVisionByID(ctx context.Context, t *testing.T, svc *portal.Service) map[string]bool {
	t.Helper()
	resp := svc.Models(ctx, auth.Token{UserID: "usr_dev", Scopes: []string{"gateway:use"}})
	byID := make(map[string]bool, len(resp.Data))
	for _, m := range resp.Data {
		byID[m.ID] = m.Vision
	}
	return byID
}

// TestSeedDefaultServerMarksExactlyOneModelVisionCapable proves the fix for
// chat.spec.ts's "chat remembers a sent image..." e2e failure: neither
// dev-seeded model ever had a vision capability row, so GET
// /api/portal/models reported vision=false for BOTH gpt-oss-20b and
// qwen-coder (verified live against a running e2e gateway), and ChatStore's
// own image-support guard then correctly refused any send with an attached
// image -- a dev/e2e gateway could never exercise the image-attach path at
// all.
//
// The seed marks gpt-oss-20b (and ONLY gpt-oss-20b) vision-capable via a
// manual CapabilityRow -- a dev-fixture verdict about a model this same seed
// already invents out of nothing, exactly like inventing the mock server and
// application it belongs to; "manual" is the honest source because it is the
// same path a real operator uses (the Model Servers UI's vision-capable
// toggle), not a probe result. qwen-coder is deliberately left without one so
// a dev gateway can still exercise the NON-vision path by hand (the attach
// button's disabled state + tooltip, the clear-attachments-on-model-switch
// effect, and the edit/regenerate history guard) -- seeding vision on every
// model would make that half unreachable.
func TestSeedDefaultServerMarksExactlyOneModelVisionCapable(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	rs := routing.NewMemoryStore()
	if err := seedDefaultServer(ctx, rs, now, "", ""); err != nil {
		t.Fatalf("seedDefaultServer: %v", err)
	}
	svc := portal.NewService(portal.ServiceDeps{
		Usage: usage.NewRecorder(), Routes: rs, Clock: func() time.Time { return now },
	})

	byID := modelVisionByID(ctx, t, svc)
	if !byID["gpt-oss-20b"] {
		t.Fatalf("gpt-oss-20b vision (through Service.Models) = false, want true -- the dev fixture must mark exactly one seeded model vision-capable so the image-attach path is exercisable")
	}
	if byID["qwen-coder"] {
		t.Fatalf("qwen-coder vision (through Service.Models) = true, want false -- the seed deliberately leaves ONE model non-vision-capable so the non-vision UI path stays reachable in a dev gateway")
	}

	// Idempotence: mirror seedDefaultServer's existing guarantee -- reseeding
	// must not flip, duplicate, or otherwise disturb the verdict.
	if err := seedDefaultServer(ctx, rs, now, "", ""); err != nil {
		t.Fatalf("seedDefaultServer (second run): %v", err)
	}
	reseeded := modelVisionByID(ctx, t, svc)
	if !reseeded["gpt-oss-20b"] || reseeded["qwen-coder"] {
		t.Fatalf("after reseed: gpt-oss-20b vision=%v qwen-coder vision=%v, want true/false unchanged", reseeded["gpt-oss-20b"], reseeded["qwen-coder"])
	}
}
