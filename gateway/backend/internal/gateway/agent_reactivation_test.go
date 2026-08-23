// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"sync"
	"testing"
	"time"
)

func TestMaybeFireReactivation(t *testing.T) {
	now := time.Unix(7000, 0)
	reg := NewAgentPresenceRegistry(0)
	reg.now = func() time.Time { return now }
	var fired []string
	s := &Server{
		AgentPresence:      reg,
		onAgentReactivated: func(id string) { fired = append(fired, id) },
		// Portal left nil -> system default (portal.DefaultAgentPresenceTimeoutSeconds = 15s).
	}
	srv := routing.AIServer{ID: "srv-1"} // no per-server override -> effective window 15s

	// First report: never seen -> reactivation.
	s.maybeFireReactivation(context.Background(), srv)
	// Within 15s -> no reactivation.
	now = now.Add(10 * time.Second)
	s.maybeFireReactivation(context.Background(), srv)
	// After a >15s gap -> reactivation.
	now = now.Add(20 * time.Second)
	s.maybeFireReactivation(context.Background(), srv)
	if len(fired) != 2 || fired[0] != "srv-1" || fired[1] != "srv-1" {
		t.Fatalf("want two reactivations for srv-1, got %v", fired)
	}
}

func TestMaybeFireReactivationPerServerWindow(t *testing.T) {
	now := time.Unix(8000, 0)
	reg := NewAgentPresenceRegistry(0)
	reg.now = func() time.Time { return now }
	var fired int
	s := &Server{AgentPresence: reg, onAgentReactivated: func(string) { fired++ }}
	// Per-server override 5s (< system default 15s).
	srv := routing.AIServer{ID: "srv-2", AgentPresenceTimeoutSeconds: 5}

	s.maybeFireReactivation(context.Background(), srv) // first -> fire (1)
	now = now.Add(10 * time.Second)                    // 10 > 5 window
	s.maybeFireReactivation(context.Background(), srv) // -> fire (2)
	if fired != 2 {
		t.Fatalf("per-server 5s window should have fired twice, got %d", fired)
	}
}

type fakePortalAgentPresence struct {
	portal.API // embedded nil interface; only the overridden method is ever called
	mu         sync.Mutex
	calls      int
	value      int
}

// fakePortalAgentPresence covers agentIngestPortal, the narrow portal surface
// agent_ingest.go's systemAgentPresenceDefault actually calls (GW-6).
var _ agentIngestPortal = (*fakePortalAgentPresence)(nil)

func (f *fakePortalAgentPresence) ActiveAgentPresenceTimeoutSeconds(context.Context) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.value
}

func TestMaybeFireReactivationUsesLivePortalDefaultCached(t *testing.T) {
	now := time.Unix(9000, 0)
	reg := NewAgentPresenceRegistry(0)
	reg.now = func() time.Time { return now }
	var fired int
	fp := &fakePortalAgentPresence{value: 50} // live system default = 50s (not the hardcoded 15)
	s := &Server{AgentPresence: reg, Portal: fp, onAgentReactivated: func(string) { fired++ }}
	srv := routing.AIServer{ID: "srv-1"} // no per-server override -> effective window = 50s

	// First report: reactivation (never seen); reads the live default.
	s.maybeFireReactivation(context.Background(), srv)
	// A 40s gap is < the live 50s window -> NOT a reactivation. If the code used the
	// hardcoded 15s default instead of the live 50, this WOULD re-fire.
	now = now.Add(40 * time.Second)
	s.maybeFireReactivation(context.Background(), srv)
	if fired != 1 {
		t.Fatalf("with a live 50s system default, a 40s gap must not re-fire; fired=%d", fired)
	}
	// The settings read is cached across the two ingests (TTL), not read per-call.
	if fp.calls != 1 {
		t.Fatalf("system default must be cached across ingests, got %d reads", fp.calls)
	}
}

func TestMaybeFireReactivationNilHookIsNoop(t *testing.T) {
	reg := NewAgentPresenceRegistry(0)
	s := &Server{AgentPresence: reg} // no hook
	// Must not panic and must still stamp presence.
	s.maybeFireReactivation(context.Background(), routing.AIServer{ID: "srv-3"})
	if !reg.Reporting("srv-3") {
		t.Fatal("presence must be stamped even without a hook")
	}
}
