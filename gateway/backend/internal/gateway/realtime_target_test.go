// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"op-ai-gateway/internal/routing"
	"testing"
)

// realtimeTargetServed: only an agent-launched target whose agent declared
// realtime_proxy may serve a realtime session, fail-closed (a nil registry, an
// unreported server, a sample without the name). Unlike the transcription twin,
// a direct target is NOT served: the route exists only on the agent's router.
func TestRealtimeTargetServed(t *testing.T) {
	agent := routing.Target{Provider: routing.ProviderServerAgent, ServerID: "srv-1"}
	external := routing.Target{Provider: routing.ProviderAudioCpp, ServerID: "srv-1"}
	declared := &Server{AgentFeatures: NewAgentFeaturesRegistry()}
	declared.AgentFeatures.Set("srv-1", []string{"realtime_proxy"})
	other := &Server{AgentFeatures: NewAgentFeaturesRegistry()}
	other.AgentFeatures.Set("srv-1", []string{"runtime_manager", "multipart_proxy", "fullduplex_proxy"})
	for _, tc := range []struct {
		name   string
		srv    *Server
		target routing.Target
		want   bool
	}{
		{"an agent that declared it", declared, agent, true},
		{"an agent that reported without it", other, agent, false},
		{"an agent on a server that never reported", &Server{AgentFeatures: NewAgentFeaturesRegistry()}, agent, false},
		{"an agent with no registry at all", &Server{}, agent, false},
		{"an external target on a server that declared it", declared, external, false},
		{"an external target, nothing declared", &Server{AgentFeatures: NewAgentFeaturesRegistry()}, external, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.srv.realtimeTargetServed(tc.target); got != tc.want {
				t.Fatalf("realtimeTargetServed = %v, want %v", got, tc.want)
			}
		})
	}
}

// The realtime capability is on the agent-ingest reserved list: no probe source
// may write it, so an agent-reported realtime verdict never becomes a row.
func TestIngestDropsAnAgentReportedRealtimeVerdict(t *testing.T) {
	ctx := context.Background()
	for _, verdict := range []string{routing.CapabilityYes, routing.CapabilityNo} {
		t.Run(verdict, func(t *testing.T) {
			srv := NewTestServer()
			spec := "rspec_rt_" + verdict
			seedRuntimeIngestSpec(t, srv, spec)
			mappingID := "map_" + spec
			req, raw := ingestReq(t, capabilitiesBody(spec,
				`{"verdicts":[{"name":"`+routing.CapabilityRealtime+`","verdict":"`+verdict+`"},`+
					`{"name":"vision","verdict":"yes"}],"source":"`+routing.CapabilitySourceLlamaCppProps+`"}`))
			if err := srv.ingestTelemetrySample(ctx, "mock-host-qwen", req, raw); err != nil {
				t.Fatalf("ingest: %v", err)
			}
			if row, ok := capabilityRow(t, srv, mappingID, routing.CapabilityRealtime); ok {
				t.Fatalf("a realtime row exists (%+v), want none", row)
			}
			assertCapabilityRow(t, srv, mappingID, routing.CapabilityVision, routing.CapabilityYes, routing.CapabilitySourceLlamaCppProps)
		})
	}
}
