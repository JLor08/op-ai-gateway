// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// The seeded completion mapping (see seedGatewayTestRoutes) that recordUsage
// attributes usage to via Target.RouteID.
const seedMappingID = "route_mock_qwen"

func mappingGenTPS(t *testing.T, srv *Server) float64 {
	t.Helper()
	m, err := srv.Routes.MappingByID(context.Background(), seedMappingID)
	if err != nil {
		t.Fatalf("mapping by id: %v", err)
	}
	return m.GenTokensPerSecond
}

// A successful inference on a target whose application has opportunistic metrics
// enabled EWMA-updates the served mapping's throughput from the usage event.
func TestRecordUsageOpportunisticUpdatesOnSuccess(t *testing.T) {
	srv := NewTestServer()
	target := routing.Target{RouteID: seedMappingID, ServerID: "mock-host-comp", OpportunisticMetrics: true}
	resp := provider.Response{Usage: inference.Usage{TokensPerSecond: 42, PromptPerSecond: 500}}

	srv.recordUsage(time.Now(), auth.Token{UserID: "usr_x"}, inference.Request{Model: "qwen-coder"}, target, resp, "", "success", usageMeta{}, "req_1", nil)

	if got := mappingGenTPS(t, srv); got != 42 {
		t.Fatalf("GenTokensPerSecond = %v, want 42 (seeded on first positive success sample)", got)
	}
	m, _ := srv.Routes.MappingByID(context.Background(), seedMappingID)
	if m.MetricsSource != "opportunistic" {
		t.Fatalf("MetricsSource = %q, want %q", m.MetricsSource, "opportunistic")
	}
}

// No update happens when the flag is off, the status is an error, or throughput
// is zero — in each case the mapping's metric stays at its seeded 0.
func TestRecordUsageOpportunisticGate(t *testing.T) {
	cases := []struct {
		name   string
		flag   bool
		status string
		resp   provider.Response
	}{
		{"flag off", false, "success", provider.Response{Usage: inference.Usage{TokensPerSecond: 42, PromptPerSecond: 500}}},
		{"error status", true, "error", provider.Response{Usage: inference.Usage{TokensPerSecond: 42, PromptPerSecond: 500}}},
		{"zero throughput", true, "success", provider.Response{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := NewTestServer()
			target := routing.Target{RouteID: seedMappingID, ServerID: "mock-host-comp", OpportunisticMetrics: tc.flag}
			srv.recordUsage(time.Now(), auth.Token{UserID: "usr_x"}, inference.Request{Model: "qwen-coder"}, target, tc.resp, "", tc.status, usageMeta{}, "req_g", nil)
			if got := mappingGenTPS(t, srv); got != 0 {
				t.Fatalf("GenTokensPerSecond = %v, want 0 (no opportunistic update)", got)
			}
		})
	}
}
