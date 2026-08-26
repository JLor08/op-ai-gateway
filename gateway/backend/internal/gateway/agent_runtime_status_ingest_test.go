// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"op-ai-gateway/internal/routing"
	"strings"
	"testing"
	"time"
)

// seedRuntimeIngestSpec creates a server_agent application + mapping on the
// existing "mock-host-qwen" test server (seedGatewayTestRoutes seeds it with
// no application/mapping of its own, so this never collides) and a runtime
// spec with one GPU row (index 0, a 20000 MB operator estimate, measured
// starting at 0). vramLocked controls the spec's VRAMLocked flag, the input
// the write-back path under test consults.
func seedRuntimeIngestSpec(t *testing.T, srv *Server, specID string, vramLocked bool) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	appID, mappingID := "app_"+specID, "map_"+specID
	if err := srv.Routes.CreateApplication(ctx, routing.Application{
		ID: appID, ServerID: "mock-host-qwen", Type: routing.ProviderServerAgent, Port: 8081, Scheme: "http",
		APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create application: %v", err)
	}
	if err := srv.Routes.CreateMapping(ctx, routing.ModelMapping{
		ID: mappingID, ApplicationID: appID, GatewayModelName: "runtime-model", AppModelName: "runtime-model",
		Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create mapping: %v", err)
	}
	spec := routing.RuntimeSpec{
		ID: specID, MappingID: mappingID, Enabled: true, Binary: "/usr/bin/vllm", Args: "[]", Env: "{}",
		VRAMLocked: vramLocked, CreatedAt: now, UpdatedAt: now,
	}
	if err := srv.Routes.UpsertRuntimeSpec(ctx, spec); err != nil {
		t.Fatalf("upsert runtime spec: %v", err)
	}
	if err := srv.Routes.SetRuntimeSpecGPUs(ctx, specID, []routing.RuntimeSpecGPU{
		{SpecID: specID, GPUIndex: 0, VRAMEstimateMB: 20000},
	}); err != nil {
		t.Fatalf("set spec gpus: %v", err)
	}
}

// TestIngestTelemetrySamplePublishesRuntimeStatus proves a telemetry sample's
// runtimes array reaches a live RuntimeStatus subscriber as a full-snapshot
// `update` -- the ingest-time half of the SSE contract handleRuntimeEvents
// serves.
func TestIngestTelemetrySamplePublishesRuntimeStatus(t *testing.T) {
	srv := NewTestServer()
	seedRuntimeIngestSpec(t, srv, "rspec_status", false)

	_, ch, unsub := srv.RuntimeStatus.subscribe("mock-host-qwen")
	defer unsub()

	since := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	body := `{"host":{"cpu_util_pct":1},"runtimes":[{"spec_id":"rspec_status","model":"qwen-coder","state":"running","since":"2026-08-20T10:00:00Z","pid":4242,"port":9001,"in_flight":2,"restarts":1}]}`
	req, raw := ingestReq(t, body)
	if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	select {
	case statuses := <-ch:
		if len(statuses) != 1 {
			t.Fatalf("published statuses = %#v, want one entry", statuses)
		}
		got := statuses[0]
		if got.SpecID != "rspec_status" || got.Model != "qwen-coder" || got.State != "running" ||
			got.PID != 4242 || got.Port != 9001 || got.InFlight != 2 || got.Restarts != 1 || !got.Since.Equal(since) {
			t.Fatalf("published status = %#v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the runtime-status publish")
	}
}

// TestIngestTelemetrySampleRuntimesAbsentPublishesEmptySnapshot proves a
// legacy payload with no "runtimes" key still decodes (additive/optional
// field) and publishes an EMPTY status snapshot rather than erroring or
// leaving a stale prior snapshot in place. (The registry's own snapshot copy
// may come back as a bare nil for an empty set -- same as
// serverPerfRegistry.subscribe -- the non-nil-on-the-wire guarantee is
// enforced downstream by nonNilRuntimeStatuses; see runtime_registry_test.go.)
func TestIngestTelemetrySampleRuntimesAbsentPublishesEmptySnapshot(t *testing.T) {
	srv := NewTestServer()
	// First seed a NON-empty snapshot so an absent "runtimes" on the next
	// sample provably clears it, rather than this test passing vacuously
	// against an already-empty registry.
	srv.RuntimeStatus.publish("mock-host-qwen", []RuntimeStatusDTO{{SpecID: "stale"}})

	req, raw := ingestReq(t, validIngestAgentBody) // no "runtimes" field at all
	if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	snap, _, unsub := srv.RuntimeStatus.subscribe("mock-host-qwen")
	defer unsub()
	if len(snap) != 0 {
		t.Fatalf("snapshot = %#v, want empty (the absent runtimes must replace, not preserve, the prior snapshot)", snap)
	}
}

// TestIngestTelemetrySampleRuntimeClampsStderrTail proves an over-long stderr_tail
// on a runtime's last_error is clamped to maxRuntimeStderrTail bytes before
// it reaches the volatile status registry.
func TestIngestTelemetrySampleRuntimeClampsStderrTail(t *testing.T) {
	srv := NewTestServer()
	seedRuntimeIngestSpec(t, srv, "rspec_stderr", false)

	long := strings.Repeat("x", maxRuntimeStderrTail+500)
	body := `{"host":{"cpu_util_pct":1},"runtimes":[{"spec_id":"rspec_stderr","state":"crashed",` +
		`"last_error":{"message":"boom","exit_code":1,"failures":3,"stderr_tail":"` + long + `"}}]}`
	req, raw := ingestReq(t, body)
	if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	snap, _, unsub := srv.RuntimeStatus.subscribe("mock-host-qwen")
	defer unsub()
	if len(snap) != 1 || snap[0].LastError == nil {
		t.Fatalf("snapshot = %#v, want one entry with a last_error", snap)
	}
	if got := len(snap[0].LastError.StderrTail); got != maxRuntimeStderrTail {
		t.Fatalf("stderr_tail length = %d, want clamped to %d", got, maxRuntimeStderrTail)
	}
	if snap[0].LastError.ExitCode != 1 || snap[0].LastError.Failures != 3 || snap[0].LastError.Message != "boom" {
		t.Fatalf("last_error other fields not preserved: %#v", snap[0].LastError)
	}
}

// TestIngestTelemetrySampleRuntimeVRAMWriteBack proves a sample's measured VRAM is
// written back onto the spec's GPU row (VRAMMeasuredMB) without disturbing
// the operator's own VRAMEstimateMB.
func TestIngestTelemetrySampleRuntimeVRAMWriteBack(t *testing.T) {
	srv := NewTestServer()
	seedRuntimeIngestSpec(t, srv, "rspec_vram", false)

	body := `{"host":{"cpu_util_pct":1},"runtimes":[{"spec_id":"rspec_vram","state":"running","gpus":[{"index":0,"vram_measured_mb":21234}]}]}`
	req, raw := ingestReq(t, body)
	if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	gpus, err := srv.Routes.RuntimeSpecGPUs(context.Background(), "rspec_vram")
	if err != nil || len(gpus) != 1 {
		t.Fatalf("RuntimeSpecGPUs: gpus=%#v err=%v", gpus, err)
	}
	if gpus[0].VRAMMeasuredMB != 21234 {
		t.Fatalf("VRAMMeasuredMB = %d, want 21234", gpus[0].VRAMMeasuredMB)
	}
	if gpus[0].VRAMEstimateMB != 20000 {
		t.Fatalf("VRAMEstimateMB = %d, want untouched 20000", gpus[0].VRAMEstimateMB)
	}
}

// TestIngestTelemetrySampleRuntimeVRAMWriteBackSkippedWhenLocked proves a
// VRAMLocked spec's GPU rows are left alone even when the sample carries a
// measured value -- the operator's pinned numbers win.
func TestIngestTelemetrySampleRuntimeVRAMWriteBackSkippedWhenLocked(t *testing.T) {
	srv := NewTestServer()
	seedRuntimeIngestSpec(t, srv, "rspec_locked", true)

	body := `{"host":{"cpu_util_pct":1},"runtimes":[{"spec_id":"rspec_locked","state":"running","gpus":[{"index":0,"vram_measured_mb":99999}]}]}`
	req, raw := ingestReq(t, body)
	if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	gpus, err := srv.Routes.RuntimeSpecGPUs(context.Background(), "rspec_locked")
	if err != nil || len(gpus) != 1 {
		t.Fatalf("RuntimeSpecGPUs: gpus=%#v err=%v", gpus, err)
	}
	if gpus[0].VRAMMeasuredMB != 0 {
		t.Fatalf("VRAMMeasuredMB = %d, want untouched 0 (spec is VRAMLocked)", gpus[0].VRAMMeasuredMB)
	}
}

// TestIngestTelemetrySampleRuntimeVRAMWriteBackToleratesUnknownSpec proves a
// runtime sample naming a spec_id with no corresponding row (deleted out
// from under an in-flight sample, or simply a stale/bogus id) never rejects
// the sample -- the write-back is best-effort only.
func TestIngestTelemetrySampleRuntimeVRAMWriteBackToleratesUnknownSpec(t *testing.T) {
	srv := NewTestServer()
	body := `{"host":{"cpu_util_pct":1},"runtimes":[{"spec_id":"rspec_ghost","state":"running","gpus":[{"index":0,"vram_measured_mb":5000}]}]}`
	req, raw := ingestReq(t, body)
	if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest must succeed even when the sample names an unknown spec_id: %v", err)
	}
	// The status publish must still have happened -- the unknown spec only
	// affects the (best-effort) VRAM write-back, never the status snapshot.
	snap, _, unsub := srv.RuntimeStatus.subscribe("mock-host-qwen")
	defer unsub()
	if len(snap) != 1 || snap[0].SpecID != "rspec_ghost" {
		t.Fatalf("snapshot = %#v, want the runtime status published regardless", snap)
	}
}
