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
// group landscape must hold exactly one admin-tier group, and that group's
// CanManageUsers must be true for the dev principal -- the flag the invite
// form's auto-select (and thus its submit-enabled state) depends on.
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
	devPrincipal := auth.Token{UserID: "usr_dev", Scopes: []string{"gateway:use", "admin", "system"}}

	// Before the seed: this is the exact bug -- no admin group exists yet.
	before, err := svc.ListGroups(ctx, devPrincipal)
	if err != nil {
		t.Fatalf("ListGroups (before seed): %v", err)
	}
	if len(before.Admin) != 0 {
		t.Fatalf("before seedDevAdminGroup: landscape.Admin = %d groups, want 0 (setup assumption violated)", len(before.Admin))
	}

	if err := seedDevAdminGroup(ctx, dir, now, "usr_dev"); err != nil {
		t.Fatalf("seedDevAdminGroup: %v", err)
	}

	after, err := svc.ListGroups(ctx, devPrincipal)
	if err != nil {
		t.Fatalf("ListGroups (after seed): %v", err)
	}
	if len(after.Admin) != 1 {
		t.Fatalf("after seedDevAdminGroup: landscape.Admin = %d groups, want exactly 1: %+v", len(after.Admin), after.Admin)
	}
	if !after.Admin[0].CanManageUsers {
		t.Fatalf("after seedDevAdminGroup: landscape.Admin[0].CanManageUsers = false, want true (this is what the invite form's auto-select/submit-enabled state depends on)")
	}

	// Idempotence: mirror seedDefaultServer's guarantee that running the seed
	// twice is safe and never creates a second group.
	if err := seedDevAdminGroup(ctx, dir, now, "usr_dev"); err != nil {
		t.Fatalf("seedDevAdminGroup (second run): %v", err)
	}
	reseeded, err := svc.ListGroups(ctx, devPrincipal)
	if err != nil {
		t.Fatalf("ListGroups (after reseed): %v", err)
	}
	if len(reseeded.Admin) != 1 {
		t.Fatalf("after reseed: landscape.Admin = %d groups, want exactly 1 (idempotence): %+v", len(reseeded.Admin), reseeded.Admin)
	}
}
