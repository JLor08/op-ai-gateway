// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"op-ai-gateway/internal/routing"
	"testing"
)

// consentRevokingStore revokes the server's cluster membership while the
// telemetry write is in flight: the portal save that lands between the
// ingest's server read and its last-seen write.
type consentRevokingStore struct {
	routing.Store
}

func (s consentRevokingStore) UpsertTelemetry(ctx context.Context, t routing.ServerTelemetry) error {
	srv, err := s.AIServerByID(ctx, t.ServerID)
	if err != nil {
		return err
	}
	srv.RPCMemberEnabled, srv.RPCAllowedHeadGroups = false, "[]"
	if err := s.Store.UpdateAIServer(ctx, srv); err != nil {
		return err
	}
	return s.Store.UpsertTelemetry(ctx, t)
}

// TestIngestTelemetryKeepsConcurrentConsentChange pins that a telemetry
// sample stamps only the server's last-seen time: a membership revoked while
// the sample is stored stays revoked.
func TestIngestTelemetryKeepsConcurrentConsentChange(t *testing.T) {
	srv := NewTestServer()
	ctx := context.Background()
	const id = "mock-host-qwen"
	server, err := srv.Routes.AIServerByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	server.RPCMemberEnabled, server.RPCAllowedHeadGroups = true, `["ugrp_a"]`
	if err := srv.Routes.UpdateAIServer(ctx, server); err != nil {
		t.Fatal(err)
	}
	srv.Routes = consentRevokingStore{srv.Routes}
	req, raw := ingestReq(t, `{"host":{"cpu_util_pct":10}}`)
	if err := srv.ingestTelemetrySample(ctx, id, req, raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	got, err := srv.Routes.AIServerByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.RPCMemberEnabled || got.RPCAllowedHeadGroups != "[]" {
		t.Fatalf("the sample wrote the old consent back: enabled=%v groups=%q", got.RPCMemberEnabled, got.RPCAllowedHeadGroups)
	}
	if got.LastSeenAt == nil {
		t.Fatal("the sample did not stamp last_seen_at")
	}
}
