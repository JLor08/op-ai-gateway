// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"context"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"testing"
	"time"
)

// TestResolverAffinityReadsLiveProgressCapabilityRowNotFrozenColumn pins that
// resolveAffinity (the sticky-pin path, taken on every request within an
// affinity's TTL after the first) reads a mapping's LIVE "live_progress"
// capability row, not ModelMapping.LiveProgressSupport -- the pre-migration-78
// column Task 3 stopped writing.
//
// The scenario: pin an affinity with the mapping's verdict undetermined (no
// capability row at all, so the first Resolve's fresh candidate also reports
// ""), then have a probe write a "yes" row -- exactly what happens when a
// probe determines live-progress support sometime after a session's first
// request. A THIRD Resolve, still served entirely from the affinity pin (same
// token/model/flavor, well inside AffinityTTLSeconds), must see the NEW
// verdict. Before this fix resolveAffinity built its synthetic
// MappingCandidate from mapping.LiveProgressSupport (frozen at "" from the
// mapping's own row, which no writer has touched since #49-3), so it would
// have kept reporting "" for the rest of the affinity's TTL regardless of
// what the capability table said.
func TestResolverAffinityReadsLiveProgressCapabilityRowNotFrozenColumn(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now) // srv_fast/app_fast/map_fast, AffinityTTLSeconds=1800
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	token := auth.Token{ID: "tok_dev", UserID: "usr_dev", Active: true}
	req := inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"}

	// 1) First Resolve: no capability row yet -> fresh candidate (through
	// ActiveMappingsForModel) reports LiveProgressSupport == "" and pins the
	// affinity to app_fast/srv_fast/map_fast.
	first, err := resolver.Resolve(ctx, token, req)
	if err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	if first.LiveProgressSupport != "" {
		t.Fatalf("first.LiveProgressSupport = %q, want \"\" (no capability row yet)", first.LiveProgressSupport)
	}

	// 2) A probe determines live-progress support after the pin was created --
	// exactly the sequence that exposes a frozen read: the mapping's OWN
	// LiveProgressSupport column is never touched by this write (that writer
	// was deleted in Task 3); only the model_mapping_capabilities row changes.
	if err := store.UpsertMappingCapabilities(ctx, "map_fast", []CapabilityRow{
		{Capability: CapabilityLiveProgress, Verdict: CapabilityYes, Source: CapabilitySourceLlamaCppProps, CheckedAt: now},
	}); err != nil {
		t.Fatalf("UpsertMappingCapabilities: %v", err)
	}

	// 3) Second Resolve, same token/model/flavor, still within the TTL: this
	// MUST be served by resolveAffinity (the sticky-pin path), not a fresh
	// candidate lookup -- confirmed below by asserting it lands on the same
	// server. The affinity-served Target must report the NEW verdict.
	second, err := resolver.Resolve(ctx, token, req)
	if err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if second.ServerID != first.ServerID {
		t.Fatalf("second.ServerID = %q, want %q (must be served by the affinity pin)", second.ServerID, first.ServerID)
	}
	if second.LiveProgressSupport != "supported" {
		t.Fatalf("second.LiveProgressSupport = %q, want supported (the pin must read the LIVE capability row, not the frozen mapping column)", second.LiveProgressSupport)
	}
}
