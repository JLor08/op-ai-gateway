// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"testing"
	"time"
)

// fakeGate is the ProvisioningGate test double from the task brief: it allows exactly
// the server ids present in its allow map (a missing id is not allowed) and never
// errors.
type fakeGate struct{ allow map[string]bool }

func (f fakeGate) AllowedServerIDs(_ context.Context, _ auth.Token, ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, id := range ids {
		if f.allow[id] {
			out[id] = true
		}
	}
	return out, nil
}

// TestProvisioningGateNoOpWhenNil confirms the no-op invariant: a resolver with no
// ProvisioningGate installed (the default — SetProvisioningGate never called) resolves
// EXACTLY as before the seam existed, reusing the
// TestResolverRoutesToMappingBackedTarget fixture + assertions.
func TestProvisioningGateNoOpWhenNil(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	// provisioning left unset (nil) deliberately — no SetProvisioningGate call.

	target, err := resolver.Resolve(ctx, auth.Token{ID: "tok_dev", UserID: "usr_dev", Active: true}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve returned %v", err)
	}
	if target.ServerID != "srv_fast" {
		t.Fatalf("target.ServerID = %q, want srv_fast (unaffected by an unset provisioning gate)", target.ServerID)
	}
	if target.RouteID != "map_fast" {
		t.Fatalf("target.RouteID = %q, want map_fast", target.RouteID)
	}
}

// TestProvisioningGateFiltersCandidates: two servers offer the model; the gate allows
// only srv_slow (NOT the higher-scoring srv_fast, which TestResolverRoutesToMappingBackedTarget
// proves wins on an unfiltered pool) -> Resolve must serve srv_slow, never srv_fast.
func TestProvisioningGateFiltersCandidates(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetProvisioningGate(fakeGate{allow: map[string]bool{"srv_slow": true}})

	target, err := resolver.Resolve(ctx, auth.Token{ID: "tok_dev", UserID: "usr_dev", Active: true}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve returned %v", err)
	}
	if target.ServerID != "srv_slow" {
		t.Fatalf("target.ServerID = %q, want srv_slow (the only provisioned server)", target.ServerID)
	}
}

// TestProvisioningGateAllBlockedNoRoute: the gate allows neither candidate server ->
// Resolve must return ErrNoModelRoute (the codebase's no-leak posture: "unknown model"
// rather than a distinguishable "exists but forbidden" signal).
func TestProvisioningGateAllBlockedNoRoute(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetProvisioningGate(fakeGate{allow: map[string]bool{}})

	_, err := resolver.Resolve(ctx, auth.Token{ID: "tok_dev", UserID: "usr_dev", Active: true}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if !errors.Is(err, ErrNoModelRoute) {
		t.Fatalf("Resolve err = %v, want ErrNoModelRoute", err)
	}
}

// TestProvisioningGateAffinityRecheck: a pin to srv_fast is established while the gate
// allows both servers; the gate is then tightened to block srv_fast only. Resolve must
// NOT return the pinned srv_fast — it must fall through to fresh selection and land on
// srv_slow (the only remaining allowed candidate).
//
// Mutation check: removing the affinity re-check in Resolve (returning the
// resolveAffinity target unconditionally once ok==true) makes this test fail —
// resolveAffinity itself has no provisioning awareness, so without the re-check it
// would happily return the now-blocked srv_fast pin.
func TestProvisioningGateAffinityRecheck(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetProvisioningGate(fakeGate{allow: map[string]bool{"srv_fast": true, "srv_slow": true}})
	token := auth.Token{ID: "tok_dev", UserID: "usr_dev", Active: true}
	req := inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"}

	pinned, err := resolver.Resolve(ctx, token, req)
	if err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	if pinned.ServerID != "srv_fast" {
		t.Fatalf("expected the pin to land on srv_fast (higher score), got %q", pinned.ServerID)
	}

	// Tighten the gate: srv_fast is no longer provisioned for this principal.
	resolver.SetProvisioningGate(fakeGate{allow: map[string]bool{"srv_slow": true}})

	target, err := resolver.Resolve(ctx, token, req)
	if err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if target.ServerID == "srv_fast" {
		t.Fatalf("target.ServerID = srv_fast, want the pin IGNORED and fresh selection to land on srv_slow")
	}
	if target.ServerID != "srv_slow" {
		t.Fatalf("target.ServerID = %q, want srv_slow", target.ServerID)
	}
}
