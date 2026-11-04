// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"op-ai-gateway/internal/portal"
	"testing"
	"time"
)

// runtimeConfigReadSpy wraps a portal.API and reports every
// AgentRuntimeConfig derive on reads: the first thing a runtime-config push
// pass does once its gates let it through, so a read is the observable sign
// that a push for that server ran.
type runtimeConfigReadSpy struct {
	portal.API
	reads chan string
}

func (p *runtimeConfigReadSpy) AgentRuntimeConfig(ctx context.Context, serverID string) (portal.AgentRuntimeConfigDTO, error) {
	select {
	case p.reads <- serverID:
	default:
	}
	return p.API.AgentRuntimeConfig(ctx, serverID)
}

// spyRuntimeConfigPushes swaps srv.Portal for a runtimeConfigReadSpy over it.
func spyRuntimeConfigPushes(srv *Server) *runtimeConfigReadSpy {
	spy := &runtimeConfigReadSpy{API: srv.Portal, reads: make(chan string, 64)}
	srv.Portal = spy
	return spy
}

// expectPush fails unless a push pass for serverID derives a document within
// a generous bound.
func (p *runtimeConfigReadSpy) expectPush(t *testing.T, step, serverID string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case got := <-p.reads:
			if got == serverID {
				return
			}
		case <-deadline:
			t.Fatalf("%s: no runtime-config push for %s", step, serverID)
		}
	}
}

// expectNoPush fails if a push pass for serverID derives a document within a
// short window. A push runs at once on its own goroutine, so the window only
// has to cover scheduling.
func (p *runtimeConfigReadSpy) expectNoPush(t *testing.T, step, serverID string) {
	t.Helper()
	window := time.After(200 * time.Millisecond)
	for {
		select {
		case got := <-p.reads:
			if got == serverID {
				t.Fatalf("%s: a runtime-config push ran for %s, want none", step, serverID)
			}
		case <-window:
			return
		}
	}
}

// TestAgentFeatureRuntimeInstancesNamesOneFeature pins the portal's spelling
// of the agent's runtime_instances declaration to this package's: the portal
// builds each agent's document by it, the ingest records it, and two literals
// that drifted apart would push every agent the document of one without it.
func TestAgentFeatureRuntimeInstancesNamesOneFeature(t *testing.T) {
	if portal.AgentFeatureRuntimeInstances != runtimeInstancesFeature {
		t.Fatalf("portal.AgentFeatureRuntimeInstances = %q, runtimeInstancesFeature = %q, want one name",
			portal.AgentFeatureRuntimeInstances, runtimeInstancesFeature)
	}
}

// TestIngestPushesTheRuntimeConfigWhenRuntimeInstancesFlips pins the push that
// follows a change of the agent's runtime_instances declaration: the
// runtime-config document carries a replicable spec's instance settings only
// for an agent that declares it, so the declaration is an input of the
// document, and a sample that changes it -- the first sample after a gateway
// start included, before which nothing was known -- pushes the new document
// at once instead of leaving it to the agent's next poll. A sample that
// repeats the declaration pushes nothing.
func TestIngestPushesTheRuntimeConfigWhenRuntimeInstancesFlips(t *testing.T) {
	srv := NewTestServer()
	spy := spyRuntimeConfigPushes(srv)
	ingest := func(t *testing.T, features string) {
		t.Helper()
		req, raw := ingestReq(t, `{"host":{"cpu_util_pct":1},"capabilities":{"features":[`+features+`]}}`)
		if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
			t.Fatalf("ingest: %v", err)
		}
	}

	ingest(t, `"runtime_manager"`)
	spy.expectNoPush(t, "a first sample without runtime_instances", "mock-host-qwen")
	ingest(t, `"runtime_manager","runtime_instances"`)
	spy.expectPush(t, "runtime_instances declared", "mock-host-qwen")
	ingest(t, `"runtime_manager","runtime_instances"`)
	spy.expectNoPush(t, "the same declaration again", "mock-host-qwen")
	ingest(t, `"runtime_manager"`)
	spy.expectPush(t, "runtime_instances withdrawn", "mock-host-qwen")
}
