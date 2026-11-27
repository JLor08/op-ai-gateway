// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// validIngestAgentBody is a minimal, well-formed telemetry payload (no
// server_id; the server id is passed to ingest directly). Named distinctly
// from agent_listener_test.go's package-level validAgentBody const (an
// existing, unrelated NetBird-agent-listener test fixture) to avoid a
// redeclaration in this package.
const validIngestAgentBody = `{"agent_version":"0.1.0","reported_at":"2026-07-11T09:30:00Z","os":"linux","arch":"amd64","cpu_load":0.4,"active_requests":2,"queue_depth":1,"latency_ms":120,"error_rate":0.01,"provider_health":{},"capabilities":{},"host":{"cpu_util_pct":42}}`

func ingestReq(t *testing.T, raw string) (agentTelemetryRequest, json.RawMessage) {
	t.Helper()
	var req agentTelemetryRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return req, json.RawMessage(raw)
}

func TestIngestTelemetrySampleErrorMapping(t *testing.T) {
	ctx := context.Background()

	t.Run("mismatch", func(t *testing.T) {
		srv := NewTestServer()
		req, raw := ingestReq(t, `{"server_id":"other-host","host":{"cpu_util_pct":1}}`)
		err := srv.ingestTelemetrySample(ctx, "mock-host-qwen", req, raw)
		if !errors.Is(err, ErrAgentServerMismatch) {
			t.Fatalf("err = %v, want ErrAgentServerMismatch", err)
		}
	})

	t.Run("invalid payload carries detail", func(t *testing.T) {
		srv := NewTestServer()
		// A negative cpu_util_pct derives a negative cpu_load, which telemetryFromRequest rejects.
		req, raw := ingestReq(t, `{"host":{"cpu_util_pct":-5}}`)
		err := srv.ingestTelemetrySample(ctx, "mock-host-qwen", req, raw)
		if !errors.Is(err, errAgentTelemetryInvalid) {
			t.Fatalf("err = %v, want errAgentTelemetryInvalid", err)
		}
		if err.Error() != "cpu_load must be non-negative" {
			t.Fatalf("err detail = %q, want %q", err.Error(), "cpu_load must be non-negative")
		}
	})

	t.Run("unknown server", func(t *testing.T) {
		srv := NewTestServer()
		req, raw := ingestReq(t, validIngestAgentBody)
		// A server id with no AIServer row.
		err := srv.ingestTelemetrySample(ctx, "ghost-host", req, raw)
		if !errors.Is(err, errAgentUnknownServer) {
			t.Fatalf("err = %v, want errAgentUnknownServer", err)
		}
	})

	t.Run("store error keeps the specific code", func(t *testing.T) {
		srv := NewTestServer()
		srv.Routes = failingSampleStore{srv.Routes}
		req, raw := ingestReq(t, validIngestAgentBody)
		err := srv.ingestTelemetrySample(ctx, "mock-host-qwen", req, raw)
		var se *storeTelemetryError
		if !errors.As(err, &se) {
			t.Fatalf("err = %v, want *storeTelemetryError", err)
		}
		if se.code != "agent.telemetry_sample_failed" {
			t.Fatalf("code = %q, want agent.telemetry_sample_failed", se.code)
		}
	})

	t.Run("happy path fans out + reports presence", func(t *testing.T) {
		srv := NewTestServer()
		_, ch, unsub := srv.ServerPerf.subscribe("mock-host-qwen")
		defer unsub()
		req, raw := ingestReq(t, validIngestAgentBody)
		if err := srv.ingestTelemetrySample(ctx, "mock-host-qwen", req, raw); err != nil {
			t.Fatalf("ingest: %v", err)
		}
		select {
		case s := <-ch:
			if s.ServerID != "mock-host-qwen" || s.CPUUtilPct != 42 {
				t.Fatalf("fanned = %+v", s)
			}
		default:
			t.Fatal("no fanned-out sample")
		}
		if !srv.AgentPresence.Reporting("mock-host-qwen") {
			t.Fatal("agent presence not reported")
		}
	})
}

// gpuReportBody is a system report whose GPUs carry the given vendors, in order
// ("" omits the field, as an agent without gpu_vendor_identity does).
func gpuReportBody(vendors ...string) json.RawMessage {
	gpus := make([]string, 0, len(vendors))
	for i, v := range vendors {
		g := fmt.Sprintf(`{"index":%d,"name":"G%d","memory_total_bytes":1000`, i, i)
		if v != "" {
			g += fmt.Sprintf(`,"vendor":%q`, v)
		}
		gpus = append(gpus, g+"}")
	}
	return json.RawMessage(`{"agent_version":"0.27.0","gpus":[` + strings.Join(gpus, ",") + `]}`)
}

func specGPUVendors(t *testing.T, srv *Server, specID string) []string {
	t.Helper()
	gpus, err := srv.Routes.RuntimeSpecGPUs(context.Background(), specID)
	if err != nil {
		t.Fatalf("RuntimeSpecGPUs: %v", err)
	}
	out := make([]string, 0, len(gpus))
	for _, g := range gpus {
		out = append(out, g.GPUVendor)
	}
	return out
}

func TestHardwareReportBackfillsLegacyVendorOnce(t *testing.T) {
	ctx := context.Background()
	srv := NewTestServer()
	spy := &clusterNotifySpy{API: srv.Portal}
	srv.Portal = spy
	seedRuntimeIngestSpec(t, srv, "rspec_backfill")

	if err := srv.ingestSystemReport(ctx, "mock-host-qwen", gpuReportBody("amd", "nvidia")); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if got := specGPUVendors(t, srv, "rspec_backfill"); len(got) != 1 || got[0] != "nvidia" {
		t.Fatalf("spec GPU vendors = %v, want [nvidia] (the legacy vendor of an nvidia+amd host)", got)
	}
	server, err := srv.Routes.AIServerByID(ctx, "mock-host-qwen")
	if err != nil {
		t.Fatalf("AIServerByID: %v", err)
	}
	if server.GPULegacyVendor != "nvidia" || !server.GPUVendorNote {
		t.Fatalf("server legacy vendor %q note %v, want nvidia true", server.GPULegacyVendor, server.GPUVendorNote)
	}
	if got := spy.take(); !slices.Equal(got, []string{"mock-host-qwen"}) {
		t.Fatalf("cluster notifications after the backfill = %v, want one for the server", got)
	}

	if err := srv.ingestSystemReport(ctx, "mock-host-qwen", gpuReportBody("amd", "nvidia")); err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	if got := spy.take(); len(got) != 0 {
		t.Fatalf("the repeated report notified %v, want nothing", got)
	}
}

func TestHardwareReportFromOldAgentBackfillsNothing(t *testing.T) {
	ctx := context.Background()
	srv := NewTestServer()
	seedRuntimeIngestSpec(t, srv, "rspec_old")
	if err := srv.ingestSystemReport(ctx, "mock-host-qwen", gpuReportBody("", "")); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if got := specGPUVendors(t, srv, "rspec_old"); len(got) != 1 || got[0] != "" {
		t.Fatalf("spec GPU vendors = %v, want the legacy empty vendor kept", got)
	}
	server, _ := srv.Routes.AIServerByID(ctx, "mock-host-qwen")
	if server.GPULegacyVendor != "" || server.GPUVendorNote {
		t.Fatalf("server legacy vendor %q note %v, want untouched", server.GPULegacyVendor, server.GPUVendorNote)
	}
}

func TestSingleVendorReportSetsNoNote(t *testing.T) {
	ctx := context.Background()
	srv := NewTestServer()
	seedRuntimeIngestSpec(t, srv, "rspec_single")
	if err := srv.ingestSystemReport(ctx, "mock-host-qwen", gpuReportBody("nvidia", "nvidia")); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if got := specGPUVendors(t, srv, "rspec_single"); len(got) != 1 || got[0] != "nvidia" {
		t.Fatalf("spec GPU vendors = %v, want [nvidia]", got)
	}
	server, _ := srv.Routes.AIServerByID(ctx, "mock-host-qwen")
	if server.GPULegacyVendor != "nvidia" || server.GPUVendorNote {
		t.Fatalf("server legacy vendor %q note %v, want nvidia false", server.GPULegacyVendor, server.GPUVendorNote)
	}
}

func TestVendorlessReportKeepsStoredLegacyVendor(t *testing.T) {
	ctx := context.Background()
	srv := NewTestServer()
	seedRuntimeIngestSpec(t, srv, "rspec_keep")
	if err := srv.ingestSystemReport(ctx, "mock-host-qwen", gpuReportBody("nvidia")); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if err := srv.ingestSystemReport(ctx, "mock-host-qwen", gpuReportBody("")); err != nil {
		t.Fatalf("vendor-less ingest: %v", err)
	}
	server, _ := srv.Routes.AIServerByID(ctx, "mock-host-qwen")
	if server.GPULegacyVendor != "nvidia" {
		t.Fatalf("legacy vendor = %q after a vendor-less report, want nvidia kept", server.GPULegacyVendor)
	}
	if got := specGPUVendors(t, srv, "rspec_keep"); len(got) != 1 || got[0] != "nvidia" {
		t.Fatalf("spec GPU vendors = %v, want [nvidia] untouched", got)
	}
}
