// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"op-ai-gateway/internal/logbuffer"
	"op-ai-gateway/internal/routing"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
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
	seedRuntimeIngestSpecForServer(t, srv, "mock-host-qwen", specID, vramLocked)
}

// seedRuntimeIngestSpecForServer is seedRuntimeIngestSpec parameterized by
// owning server id, so a test can seed a spec belonging to a DIFFERENT
// server than the one it later ingests a sample as (the cross-server
// authorization tests below).
func seedRuntimeIngestSpecForServer(t *testing.T, srv *Server, serverID, specID string, vramLocked bool) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	appID, mappingID := "app_"+specID, "map_"+specID
	if err := srv.Routes.CreateApplication(ctx, routing.Application{
		ID: appID, ServerID: serverID, Type: routing.ProviderServerAgent, Port: 8081, Scheme: "http",
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
	body := `{"host":{"cpu_util_pct":1},"runtimes":[{"spec_id":"rspec_status","model":"qwen-coder","state":"running","since":"2026-08-20T10:00:00Z","pid":4242,"port":9001,"in_flight":2,"restarts":1,"context_size":8192,"active_requests":3,"queue_depth":5}]}`
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
			got.PID != 4242 || got.Port != 9001 || got.InFlight != 2 || got.Restarts != 1 || !got.Since.Equal(since) ||
			got.ContextSize != 8192 || got.ActiveRequests != 3 || got.QueueDepth != 5 {
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

// --- Task 9 review fix: cross-server authorization on the VRAM write-back --

// TestIngestTelemetrySampleRuntimeVRAMWriteBackRejectsCrossServerSpec is the
// regression guard for the write-back authorization gap: spec_id is an
// agent-supplied body field, and the ONLY thing binding a telemetry sample
// to a server is the token-derived serverID passed into
// ingestTelemetrySample -- nothing previously checked that the spec named
// by spec_id actually belongs to THAT server. An agent authenticated for
// "mock-host-qwen" naming a spec_id that belongs to a DIFFERENT server must
// leave that other server's spec untouched. This test FAILS against the
// pre-fix writeBackRuntimeVRAM, which resolved only RuntimeSpecByID +
// VRAMLocked and wrote unconditionally.
func TestIngestTelemetrySampleRuntimeVRAMWriteBackRejectsCrossServerSpec(t *testing.T) {
	srv := NewTestServer()
	ctx := context.Background()
	now := time.Now().UTC()
	const otherServerID = "mock-host-other-tenant"
	if err := srv.Routes.CreateAIServer(ctx, routing.AIServer{
		ID: otherServerID, Name: otherServerID, Domain: otherServerID + ".example.test",
		Provider: routing.ProviderMock, Endpoint: "mock://" + otherServerID,
		Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create other server: %v", err)
	}
	// rspec_cross_tenant belongs to otherServerID, NOT mock-host-qwen.
	seedRuntimeIngestSpecForServer(t, srv, otherServerID, "rspec_cross_tenant", false)

	// Authenticated as "mock-host-qwen" (the token-derived serverID), the
	// sample names otherServerID's spec_id with a forged measured VRAM.
	body := `{"host":{"cpu_util_pct":1},"runtimes":[{"spec_id":"rspec_cross_tenant","state":"running","gpus":[{"index":0,"vram_measured_mb":99999}]}]}`
	req, raw := ingestReq(t, body)
	if err := srv.ingestTelemetrySample(ctx, "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest must succeed (best-effort write-back) even when the sample names another server's spec_id: %v", err)
	}

	gpus, err := srv.Routes.RuntimeSpecGPUs(ctx, "rspec_cross_tenant")
	if err != nil || len(gpus) != 1 {
		t.Fatalf("RuntimeSpecGPUs: gpus=%#v err=%v", gpus, err)
	}
	if gpus[0].VRAMMeasuredMB != 0 {
		t.Fatalf("VRAMMeasuredMB = %d, want untouched 0 -- an agent for one server must not overwrite another server's spec VRAM via spec_id", gpus[0].VRAMMeasuredMB)
	}
}

// TestIngestTelemetrySampleRuntimeVRAMWriteBackAcceptsOwnServerSpec is the
// companion positive case: the SAME spec shape, but owned by the reporting
// server, must still write back normally -- the authorization check must
// not become a blanket rejection.
func TestIngestTelemetrySampleRuntimeVRAMWriteBackAcceptsOwnServerSpec(t *testing.T) {
	srv := NewTestServer()
	seedRuntimeIngestSpec(t, srv, "rspec_own_tenant", false)

	body := `{"host":{"cpu_util_pct":1},"runtimes":[{"spec_id":"rspec_own_tenant","state":"running","gpus":[{"index":0,"vram_measured_mb":22222}]}]}`
	req, raw := ingestReq(t, body)
	if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	gpus, err := srv.Routes.RuntimeSpecGPUs(context.Background(), "rspec_own_tenant")
	if err != nil || len(gpus) != 1 {
		t.Fatalf("RuntimeSpecGPUs: gpus=%#v err=%v", gpus, err)
	}
	if gpus[0].VRAMMeasuredMB != 22222 {
		t.Fatalf("VRAMMeasuredMB = %d, want 22222 (the reporting server owns this spec)", gpus[0].VRAMMeasuredMB)
	}
}

// --- Task 9 review fix: bounding + memoizing the VRAM write-back loop ------

// countingRuntimeSpecStore wraps a real *routing.MemoryStore and counts calls
// to RuntimeSpecByID, so a test can assert on resolution CALL COUNT rather
// than only on the resulting store state -- the write-back's memoization and
// length-cap are otherwise invisible from the outside (both a memoized and
// an un-memoized loop reach the same final GPU-row state).
type countingRuntimeSpecStore struct {
	*routing.MemoryStore
	runtimeSpecByIDCalls atomic.Int32
}

func (c *countingRuntimeSpecStore) RuntimeSpecByID(ctx context.Context, id string) (routing.RuntimeSpec, bool, error) {
	c.runtimeSpecByIDCalls.Add(1)
	return c.MemoryStore.RuntimeSpecByID(ctx, id)
}

// manyRuntimeSamples builds n agentRuntimeSample entries, each carrying one
// measured-VRAM GPU row (so none is skipped by the empty-GPUs guard). When
// specID is "", each entry gets its OWN distinct (unknown) spec_id; when
// specID is non-empty, every entry shares it.
func manyRuntimeSamples(n int, specID string) []agentRuntimeSample {
	out := make([]agentRuntimeSample, n)
	for i := range out {
		id := specID
		if id == "" {
			id = fmt.Sprintf("rspec_ghost_%d", i)
		}
		out[i] = agentRuntimeSample{
			SpecID: id,
			State:  "running",
			GPUs:   []agentRuntimeGPUSample{{Index: 0, VRAMMeasuredMB: 1000}},
		}
	}
	return out
}

// TestIngestTelemetrySampleRuntimeVRAMWriteBackMemoizesRepeatedUnknownSpecID
// proves an unknown spec_id repeated many times in ONE sample resolves
// exactly once -- the miss itself is memoized, not just a successful
// resolution (the pre-fix code re-read on every occurrence of a miss).
func TestIngestTelemetrySampleRuntimeVRAMWriteBackMemoizesRepeatedUnknownSpecID(t *testing.T) {
	srv := NewTestServer()
	counting := &countingRuntimeSpecStore{MemoryStore: srv.Routes.(*routing.MemoryStore)}
	srv.Routes = counting

	req := agentTelemetryRequest{Host: &agentHostReport{CPUUtilPct: 1}, Runtimes: manyRuntimeSamples(10, "rspec_repeat_unknown")}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if got := counting.runtimeSpecByIDCalls.Load(); got != 1 {
		t.Fatalf("RuntimeSpecByID calls = %d, want exactly 1 (an unknown spec_id repeated 10x must resolve once and memoize the miss)", got)
	}
}

// TestIngestTelemetrySampleRuntimeVRAMWriteBackBoundsSampleCount proves the
// runtimes array is length-capped at maxRuntimeSamplesPerSample BEFORE any
// resolution is attempted: 300 entries with 300 DISTINCT unknown spec_ids
// (so memoization cannot mask the count) must drive at most
// maxRuntimeSamplesPerSample RuntimeSpecByID calls, not 300.
func TestIngestTelemetrySampleRuntimeVRAMWriteBackBoundsSampleCount(t *testing.T) {
	srv := NewTestServer()
	counting := &countingRuntimeSpecStore{MemoryStore: srv.Routes.(*routing.MemoryStore)}
	srv.Routes = counting

	const total = maxRuntimeSamplesPerSample + 44
	req := agentTelemetryRequest{Host: &agentHostReport{CPUUtilPct: 1}, Runtimes: manyRuntimeSamples(total, "")}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if got := counting.runtimeSpecByIDCalls.Load(); got != maxRuntimeSamplesPerSample {
		t.Fatalf("RuntimeSpecByID calls = %d, want exactly %d (the runtimes array must be truncated before processing, not just deduplicated)", got, maxRuntimeSamplesPerSample)
	}
}

// --- Task 9 review round 2: audit trail must not depend on lock status ----

// TestIngestTelemetrySampleRuntimeVRAMWriteBackWarnsOnCrossServerEvenWhenLocked
// is the audit-trail regression guard: resolveRuntimeSpecWritable used to
// check VRAMLocked BEFORE the cross-server ownership check, so a naming
// attempt against a spec that happened to be locked returned false with NO
// Warn logged -- the write was still correctly blocked, but the audit trail
// for exactly the attack the previous round fixed was silently incomplete.
// Ownership is now checked UNCONDITIONALLY, so the Warn fires whether or
// not the targeted spec is locked.
func TestIngestTelemetrySampleRuntimeVRAMWriteBackWarnsOnCrossServerEvenWhenLocked(t *testing.T) {
	srv := NewTestServer()
	ctx := context.Background()
	now := time.Now().UTC()
	const otherServerID = "mock-host-other-tenant-locked"
	if err := srv.Routes.CreateAIServer(ctx, routing.AIServer{
		ID: otherServerID, Name: otherServerID, Domain: otherServerID + ".example.test",
		Provider: routing.ProviderMock, Endpoint: "mock://" + otherServerID,
		Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create other server: %v", err)
	}
	// rspec_cross_locked is BOTH owned by a different server AND VRAMLocked.
	seedRuntimeIngestSpecForServer(t, srv, otherServerID, "rspec_cross_locked", true)

	buf, restore := withCapturedSlog(t)
	defer restore()

	body := `{"host":{"cpu_util_pct":1},"runtimes":[{"spec_id":"rspec_cross_locked","state":"running","gpus":[{"index":0,"vram_measured_mb":99999}]}]}`
	req, raw := ingestReq(t, body)
	if err := srv.ingestTelemetrySample(ctx, "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	// The write is still correctly blocked (cross-server wins regardless of
	// lock status).
	gpus, err := srv.Routes.RuntimeSpecGPUs(ctx, "rspec_cross_locked")
	if err != nil || len(gpus) != 1 || gpus[0].VRAMMeasuredMB != 0 {
		t.Fatalf("gpus = %#v, err = %v, want untouched", gpus, err)
	}

	recs := buf.Snapshot()
	if !findLogRecord(recs, "WARN", "spec belongs to a different server") {
		t.Fatal("a cross-server naming attempt against a LOCKED spec must still log a Warn -- the audit trail must not depend on lock status")
	}
}

// countingMeasuredWriteStore counts the agent-owned VRAM write-back's own
// UPDATE, so a test can assert that an UNCHANGED measurement costs no write
// at all.
type countingMeasuredWriteStore struct {
	*routing.MemoryStore
	updateCalls atomic.Int32
}

func (c *countingMeasuredWriteStore) UpdateRuntimeSpecGPUMeasured(ctx context.Context, specID string, gpuIndex, measuredMB int) error {
	c.updateCalls.Add(1)
	return c.MemoryStore.UpdateRuntimeSpecGPUMeasured(ctx, specID, gpuIndex, measuredMB)
}

// TestIngestTelemetrySampleRuntimeVRAMWriteBackSkipsUnchangedValue is F2's
// second half. Telemetry arrives once per second and each sample is a full
// snapshot, so a spec whose measurement is simply STABLE -- the normal case
// for a loaded model serving nothing -- drove one unconditional UPDATE per
// second per (spec, gpu), forever. Neither side had change detection and the
// SQL is an unconditional write, so an idle overnight server with a handful
// of measured specs across two cards generated on the order of a million
// identical UPDATEs a day: WAL growth on SQLite and dead-tuple churn on
// Postgres, for a table with a dozen rows.
//
// Detection belongs HERE, on the side that owns the stored row, not on the
// agent: suppressing the report at the source would make the two sides
// diverge permanently the moment the stored value changed out from under a
// long-running agent (an operator deleting and re-adding a GPU row resets it
// to 0), and the agent would never resend. Comparing against what is actually
// stored converges no matter what happened to the row.
func TestIngestTelemetrySampleRuntimeVRAMWriteBackSkipsUnchangedValue(t *testing.T) {
	srv := NewTestServer()
	seedRuntimeIngestSpec(t, srv, "rspec_unchanged", false)
	counting := &countingMeasuredWriteStore{MemoryStore: srv.Routes.(*routing.MemoryStore)}
	srv.Routes = counting

	body := `{"host":{"cpu_util_pct":1},"runtimes":[{"spec_id":"rspec_unchanged","state":"running","gpus":[{"index":0,"vram_measured_mb":21234}]}]}`
	for i := 0; i < 3; i++ {
		req, raw := ingestReq(t, body)
		if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
			t.Fatalf("ingest %d: %v", i, err)
		}
	}
	if got := counting.updateCalls.Load(); got != 1 {
		t.Fatalf("UpdateRuntimeSpecGPUMeasured calls = %d across three samples carrying the SAME measurement, want exactly 1 -- BUG F2: an unchanged value is rewritten on every telemetry sample, once per second per (spec, gpu), forever", got)
	}

	// A value that genuinely moved must still be written: change detection
	// must not turn into "write once and never again".
	changed := `{"host":{"cpu_util_pct":1},"runtimes":[{"spec_id":"rspec_unchanged","state":"running","gpus":[{"index":0,"vram_measured_mb":22500}]}]}`
	req, raw := ingestReq(t, changed)
	if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest (changed): %v", err)
	}
	if got := counting.updateCalls.Load(); got != 2 {
		t.Fatalf("UpdateRuntimeSpecGPUMeasured calls = %d after a CHANGED measurement, want 2", got)
	}
	gpus, err := srv.Routes.RuntimeSpecGPUs(context.Background(), "rspec_unchanged")
	if err != nil || len(gpus) != 1 {
		t.Fatalf("RuntimeSpecGPUs: gpus=%#v err=%v", gpus, err)
	}
	if gpus[0].VRAMMeasuredMB != 22500 {
		t.Fatalf("VRAMMeasuredMB = %d, want the changed 22500", gpus[0].VRAMMeasuredMB)
	}
}

// TestIngestTelemetrySampleRuntimeStatusCarriesMeasuredVRAMWatermark proves
// the live status stream carries this sample's per-GPU measured VRAM together
// with the GATEWAY's own arrival time for the frame that carried it. The
// stored row cannot supply that watermark -- routing.RuntimeSpecGPU has no
// timestamp and the write-back deliberately skips an unchanged value -- so a
// consumer polling the store cannot tell this run's measurement from one
// taken last week. The stamp is the gateway's clock, never the agent's
// self-reported reported_at.
func TestIngestTelemetrySampleRuntimeStatusCarriesMeasuredVRAMWatermark(t *testing.T) {
	srv := NewTestServer()
	seedRuntimeIngestSpec(t, srv, "rspec_watermark", false)

	// A deliberately stale agent-reported timestamp: the watermark must NOT
	// come from it.
	staleReport := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	body := `{"reported_at":"2020-01-01T00:00:00Z","host":{"cpu_util_pct":1},"runtimes":[` +
		`{"spec_id":"rspec_watermark","state":"running","gpus":[{"index":0,"vram_measured_mb":21000},{"index":3,"vram_measured_mb":1500}]}]}`
	req, raw := ingestReq(t, body)

	before := time.Now().UTC()
	if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	after := time.Now().UTC()

	snap, _, unsub := srv.RuntimeStatus.subscribe("mock-host-qwen")
	defer unsub()
	if len(snap) != 1 {
		t.Fatalf("snapshot = %#v, want one entry", snap)
	}
	got := snap[0]
	if len(got.GPUs) != 2 {
		t.Fatalf("status GPUs = %#v, want both measured indices", got.GPUs)
	}
	if got.GPUs[0].Index != 0 || got.GPUs[0].VRAMMeasuredMB != 21000 ||
		got.GPUs[1].Index != 3 || got.GPUs[1].VRAMMeasuredMB != 1500 {
		t.Fatalf("status GPUs = %#v", got.GPUs)
	}
	if got.MeasuredAt.Before(before) || got.MeasuredAt.After(after) {
		t.Fatalf("MeasuredAt = %v, want the gateway's own arrival time within [%v, %v]", got.MeasuredAt, before, after)
	}
	if got.MeasuredAt.Equal(staleReport) {
		t.Fatal("MeasuredAt took the agent's self-reported reported_at, which is not a gateway observation")
	}
}

// TestIngestTelemetrySampleRuntimeStatusOmitsWatermarkWithoutAMeasurement
// proves the stream never claims a freshness it has no measurement for: a
// frame that measured nothing (no measurer on the host, or a spec that is not
// running) carries no GPU rows and no timestamp, and a measured value of 0 --
// which means UNKNOWN everywhere else in this feature, and which the store
// write-back drops on the same rule -- does not become one.
func TestIngestTelemetrySampleRuntimeStatusOmitsWatermarkWithoutAMeasurement(t *testing.T) {
	cases := []struct {
		name string
		gpus string
	}{
		{name: "no gpus key at all", gpus: ""},
		{name: "an empty gpus array", gpus: `,"gpus":[]`},
		{name: "a measured 0 means unknown", gpus: `,"gpus":[{"index":0,"vram_measured_mb":0}]`},
		{name: "a negative measurement is not a measurement", gpus: `,"gpus":[{"index":0,"vram_measured_mb":-5}]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := NewTestServer()
			seedRuntimeIngestSpec(t, srv, "rspec_nomeasure", false)
			body := `{"host":{"cpu_util_pct":1},"runtimes":[{"spec_id":"rspec_nomeasure","state":"running"` + tc.gpus + `}]}`
			req, raw := ingestReq(t, body)
			if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
				t.Fatalf("ingest: %v", err)
			}
			snap, _, unsub := srv.RuntimeStatus.subscribe("mock-host-qwen")
			defer unsub()
			if len(snap) != 1 {
				t.Fatalf("snapshot = %#v, want one entry", snap)
			}
			if len(snap[0].GPUs) != 0 {
				t.Fatalf("status GPUs = %#v, want none", snap[0].GPUs)
			}
			if !snap[0].MeasuredAt.IsZero() {
				t.Fatalf("MeasuredAt = %v, want the zero time (no measurement to be fresh about)", snap[0].MeasuredAt)
			}
			// The wire must omit both, not send `"gpus":null,"measured_at":"0001-01-01T00:00:00Z"`.
			payload, err := json.Marshal(snap[0])
			if err != nil {
				t.Fatalf("marshal status: %v", err)
			}
			if strings.Contains(string(payload), "gpus") || strings.Contains(string(payload), "measured_at") {
				t.Fatalf("status JSON = %s, want no gpus/measured_at keys", payload)
			}
		})
	}
}

// --- Task 12: per-mapping context probe write-back ------------------------

// TestIngestTelemetrySampleRuntimeContextWriteBack proves a sample's
// per-runtime context_size is persisted onto the spec's owning mapping
// (routing.ModelMapping.ContextSize) when the reporting agent declares the
// runtime_model_probe capability THIS sample -- Task 12, Option B: persist
// only the stable context size onto the mapping, no per-mapping active/queue
// storage, no migration, no new store method (UpdateMappingContextProbe
// already exists).
func TestIngestTelemetrySampleRuntimeContextWriteBack(t *testing.T) {
	srv := NewTestServer()
	seedRuntimeIngestSpec(t, srv, "rspec_ctx_on", false)

	body := `{"host":{"cpu_util_pct":1},"capabilities":{"features":["runtime_model_probe"]},` +
		`"runtimes":[{"spec_id":"rspec_ctx_on","state":"running","context_size":8192}]}`
	req, raw := ingestReq(t, body)
	if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	mapping, err := srv.Routes.MappingByID(context.Background(), "map_rspec_ctx_on")
	if err != nil {
		t.Fatalf("MappingByID: %v", err)
	}
	if mapping.ContextSize != 8192 {
		t.Fatalf("ContextSize = %d, want 8192", mapping.ContextSize)
	}
	if mapping.MetricsSource != "probe" {
		t.Fatalf("MetricsSource = %q, want %q", mapping.MetricsSource, "probe")
	}
}

// TestIngestTelemetrySampleRuntimeContextWriteBackGuardedByCapability proves
// the context write-back never fires for a sample that does not declare
// runtime_model_probe, even though the runtime entry itself carries a
// context_size -- the capability gate is evaluated per sample, not latched
// once true.
func TestIngestTelemetrySampleRuntimeContextWriteBackGuardedByCapability(t *testing.T) {
	srv := NewTestServer()
	seedRuntimeIngestSpec(t, srv, "rspec_ctx_off", false)

	body := `{"host":{"cpu_util_pct":1},"runtimes":[{"spec_id":"rspec_ctx_off","state":"running","context_size":8192}]}`
	req, raw := ingestReq(t, body)
	if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	mapping, err := srv.Routes.MappingByID(context.Background(), "map_rspec_ctx_off")
	if err != nil {
		t.Fatalf("MappingByID: %v", err)
	}
	if mapping.ContextSize != 0 {
		t.Fatalf("ContextSize = %d, want untouched 0 (agent did not declare runtime_model_probe)", mapping.ContextSize)
	}
}

// TestIngestTelemetrySampleRuntimeContextWriteBackRejectsCrossServerSpec
// mirrors TestIngestTelemetrySampleRuntimeVRAMWriteBackRejectsCrossServerSpec
// for the context write-back's sibling resolver (resolveRuntimeSpecMapping):
// an agent authenticated for one server must not be able to overwrite
// another server's mapping context_size by naming its spec_id.
func TestIngestTelemetrySampleRuntimeContextWriteBackRejectsCrossServerSpec(t *testing.T) {
	srv := NewTestServer()
	ctx := context.Background()
	now := time.Now().UTC()
	const otherServerID = "mock-host-other-tenant-ctx"
	if err := srv.Routes.CreateAIServer(ctx, routing.AIServer{
		ID: otherServerID, Name: otherServerID, Domain: otherServerID + ".example.test",
		Provider: routing.ProviderMock, Endpoint: "mock://" + otherServerID,
		Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create other server: %v", err)
	}
	// rspec_cross_ctx belongs to otherServerID, NOT mock-host-qwen.
	seedRuntimeIngestSpecForServer(t, srv, otherServerID, "rspec_cross_ctx", false)

	body := `{"host":{"cpu_util_pct":1},"capabilities":{"features":["runtime_model_probe"]},` +
		`"runtimes":[{"spec_id":"rspec_cross_ctx","state":"running","context_size":8192}]}`
	req, raw := ingestReq(t, body)
	if err := srv.ingestTelemetrySample(ctx, "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest must succeed (best-effort write-back) even when the sample names another server's spec_id: %v", err)
	}

	mapping, err := srv.Routes.MappingByID(ctx, "map_rspec_cross_ctx")
	if err != nil {
		t.Fatalf("MappingByID: %v", err)
	}
	if mapping.ContextSize != 0 {
		t.Fatalf("ContextSize = %d, want untouched 0 -- an agent for one server must not overwrite another server's mapping context via spec_id", mapping.ContextSize)
	}
}

// TestIngestTelemetrySampleRuntimeContextWriteBackSkipsLockedMapping proves an
// operator-pinned (metrics_locked) mapping's manually-set context_size
// survives an agent probe reporting a different value. UpdateMappingContextProbe
// itself already no-ops on a locked mapping (SQL's metrics_locked = 0 guard);
// this proves the ingest path reaches that call at all and that the manual
// value + provenance are genuinely left alone end to end.
func TestIngestTelemetrySampleRuntimeContextWriteBackSkipsLockedMapping(t *testing.T) {
	srv := NewTestServer()
	seedRuntimeIngestSpec(t, srv, "rspec_ctx_locked", false)
	ctx := context.Background()

	mapping, err := srv.Routes.MappingByID(ctx, "map_rspec_ctx_locked")
	if err != nil {
		t.Fatalf("MappingByID: %v", err)
	}
	mapping.MetricsLocked = true
	mapping.ContextSize = 4096
	mapping.MetricsSource = "manual"
	if err := srv.Routes.UpdateMapping(ctx, mapping); err != nil {
		t.Fatalf("UpdateMapping: %v", err)
	}

	body := `{"host":{"cpu_util_pct":1},"capabilities":{"features":["runtime_model_probe"]},` +
		`"runtimes":[{"spec_id":"rspec_ctx_locked","state":"running","context_size":8192}]}`
	req, raw := ingestReq(t, body)
	if err := srv.ingestTelemetrySample(ctx, "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	got, err := srv.Routes.MappingByID(ctx, "map_rspec_ctx_locked")
	if err != nil {
		t.Fatalf("MappingByID: %v", err)
	}
	if got.ContextSize != 4096 || got.MetricsSource != "manual" {
		t.Fatalf("mapping = %#v, want the manually pinned context_size=4096/source=manual left untouched", got)
	}
}

// countingContextWriteStore counts the context write-back's own UPDATE
// (UpdateMappingContextProbe), so a test can assert that an UNCHANGED probed
// context_size costs no write at all -- writeBackRuntimeContext's sibling
// spy to countingMeasuredWriteStore above, same mechanism, different call.
type countingContextWriteStore struct {
	*routing.MemoryStore
	updateCalls atomic.Int32
}

func (c *countingContextWriteStore) UpdateMappingContextProbe(ctx context.Context, id string, contextSize int, at time.Time) error {
	c.updateCalls.Add(1)
	return c.MemoryStore.UpdateMappingContextProbe(ctx, id, contextSize, at)
}

// TestIngestTelemetrySampleRuntimeContextWriteBackSkipsUnchangedValue is
// writeBackRuntimeContext's change-detection half, mirroring
// TestIngestTelemetrySampleRuntimeVRAMWriteBackSkipsUnchangedValue for its
// VRAM sibling: telemetry arrives roughly once a second and each sample is a
// full snapshot, so a runtime whose probed context window is simply STABLE
// (the normal case once a model is loaded) must not drive one unconditional
// UPDATE per second per mapping, forever. Detection compares against the
// mapping's CURRENTLY STORED context_size (resolveRuntimeSpecMapping's
// storedContext), not against what this same spec_id reported last sample.
func TestIngestTelemetrySampleRuntimeContextWriteBackSkipsUnchangedValue(t *testing.T) {
	srv := NewTestServer()
	seedRuntimeIngestSpec(t, srv, "rspec_ctx_unchanged", false)
	counting := &countingContextWriteStore{MemoryStore: srv.Routes.(*routing.MemoryStore)}
	srv.Routes = counting

	body := `{"host":{"cpu_util_pct":1},"capabilities":{"features":["runtime_model_probe"]},` +
		`"runtimes":[{"spec_id":"rspec_ctx_unchanged","state":"running","context_size":8192}]}`

	// First ingest: the mapping starts at ContextSize=0, so 8192 is a genuine
	// change and must be written.
	req, raw := ingestReq(t, body)
	if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest 1: %v", err)
	}
	if got := counting.updateCalls.Load(); got != 1 {
		t.Fatalf("UpdateMappingContextProbe calls after first ingest = %d, want 1 (initial write)", got)
	}

	// Second ingest, SAME context_size: must be skipped entirely.
	req, raw = ingestReq(t, body)
	if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest 2 (unchanged): %v", err)
	}
	if got := counting.updateCalls.Load(); got != 1 {
		t.Fatalf("UpdateMappingContextProbe calls = %d after a SECOND sample carrying the SAME context_size, want still 1 -- an unchanged value must not be rewritten", got)
	}
	mapping, err := srv.Routes.MappingByID(context.Background(), "map_rspec_ctx_unchanged")
	if err != nil {
		t.Fatalf("MappingByID: %v", err)
	}
	if mapping.ContextSize != 8192 {
		t.Fatalf("ContextSize = %d, want 8192 (unchanged from the first write)", mapping.ContextSize)
	}

	// A value that genuinely moved must still be written: change detection
	// must not turn into "write once and never again".
	changed := `{"host":{"cpu_util_pct":1},"capabilities":{"features":["runtime_model_probe"]},` +
		`"runtimes":[{"spec_id":"rspec_ctx_unchanged","state":"running","context_size":16384}]}`
	req, raw = ingestReq(t, changed)
	if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest 3 (changed): %v", err)
	}
	if got := counting.updateCalls.Load(); got != 2 {
		t.Fatalf("UpdateMappingContextProbe calls = %d after a CHANGED context_size, want 2", got)
	}
	mapping, err = srv.Routes.MappingByID(context.Background(), "map_rspec_ctx_unchanged")
	if err != nil {
		t.Fatalf("MappingByID: %v", err)
	}
	if mapping.ContextSize != 16384 {
		t.Fatalf("ContextSize = %d, want the changed 16384", mapping.ContextSize)
	}
}

// --- Task 12: per-server telemetry aggregate = sum across runtimes --------

// TestIngestTelemetrySamplePerServerAggregateSumsRuntimes proves that when a
// multi-model server-agent declares runtime_model_probe and reports
// per-runtime active_requests/queue_depth, the persisted per-server
// ServerTelemetry summary (the routing scorer's input) is the SUM across
// runtimes -- replacing, not adding to, the legacy top-level
// active_requests/queue_depth fields (which an overlapping agent-wide scrape
// could otherwise double-count against).
func TestIngestTelemetrySamplePerServerAggregateSumsRuntimes(t *testing.T) {
	srv := NewTestServer()
	body := `{"host":{"cpu_util_pct":1},"active_requests":99,"queue_depth":99,` +
		`"capabilities":{"features":["runtime_model_probe"]},` +
		`"runtimes":[{"spec_id":"rt_a","state":"running","active_requests":2,"queue_depth":1},` +
		`{"spec_id":"rt_b","state":"running","active_requests":3,"queue_depth":0}]}`
	req, raw := ingestReq(t, body)
	if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	telemetry, ok, err := srv.Routes.TelemetryByServer(context.Background(), "mock-host-qwen")
	if err != nil || !ok {
		t.Fatalf("TelemetryByServer: ok=%v err=%v", ok, err)
	}
	if telemetry.ActiveRequests != 5 {
		t.Fatalf("ActiveRequests = %d, want 5 (sum across runtimes, not the top-level 99)", telemetry.ActiveRequests)
	}
	if telemetry.QueueDepth != 1 {
		t.Fatalf("QueueDepth = %d, want 1 (sum across runtimes, not the top-level 99)", telemetry.QueueDepth)
	}
}

// TestIngestTelemetrySamplePerServerAggregateUsesTopLevelWithoutCapability is
// the flag-off companion: an agent that does not declare runtime_model_probe
// must leave the legacy top-level active_requests/queue_depth in effect even
// though the sample also happens to carry a runtimes array.
func TestIngestTelemetrySamplePerServerAggregateUsesTopLevelWithoutCapability(t *testing.T) {
	srv := NewTestServer()
	body := `{"host":{"cpu_util_pct":1},"active_requests":7,"queue_depth":4,` +
		`"runtimes":[{"spec_id":"rt_a","state":"running","active_requests":2,"queue_depth":1},` +
		`{"spec_id":"rt_b","state":"running","active_requests":3,"queue_depth":0}]}`
	req, raw := ingestReq(t, body)
	if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	telemetry, ok, err := srv.Routes.TelemetryByServer(context.Background(), "mock-host-qwen")
	if err != nil || !ok {
		t.Fatalf("TelemetryByServer: ok=%v err=%v", ok, err)
	}
	if telemetry.ActiveRequests != 7 {
		t.Fatalf("ActiveRequests = %d, want the top-level 7 (no runtime_model_probe capability declared)", telemetry.ActiveRequests)
	}
	if telemetry.QueueDepth != 4 {
		t.Fatalf("QueueDepth = %d, want the top-level 4 (no runtime_model_probe capability declared)", telemetry.QueueDepth)
	}
}

// TestIngestTelemetrySamplePerServerAggregateClampsNegativeRuntimeValue proves
// sumRuntimeActiveQueue clamps a per-runtime negative active_requests to 0
// before summing, rather than letting it flow into the persisted per-server
// ServerTelemetry aggregate. telemetryFromRequest already rejects a negative
// TOP-LEVEL active_requests/queue_depth (agent_ingest.go ~1410), but a
// negative value nested inside one runtimes[] entry bypassed that guard: left
// unclamped, it would persist a negative aggregate that the routing scorer's
// validTelemetry treats as invalid, silently excluding this server from ALL
// routing for every model until a later clean sample. The runtimes[] array
// is best-effort enrichment throughout this file (see writeBackRuntimeVRAM's
// "a report is evidence, not a transaction" discipline), so the fix clamps
// rather than rejects: ingest must still succeed.
func TestIngestTelemetrySamplePerServerAggregateClampsNegativeRuntimeValue(t *testing.T) {
	srv := NewTestServer()
	body := `{"host":{"cpu_util_pct":1},"active_requests":99,"queue_depth":99,` +
		`"capabilities":{"features":["runtime_model_probe"]},` +
		`"runtimes":[{"spec_id":"rt_a","state":"running","active_requests":-999,"queue_depth":0},` +
		`{"spec_id":"rt_b","state":"running","active_requests":3,"queue_depth":0}]}`
	req, raw := ingestReq(t, body)
	if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest must succeed (best-effort runtimes[] enrichment, not a transaction) even with a negative per-runtime active_requests: %v", err)
	}

	telemetry, ok, err := srv.Routes.TelemetryByServer(context.Background(), "mock-host-qwen")
	if err != nil || !ok {
		t.Fatalf("TelemetryByServer: ok=%v err=%v", ok, err)
	}
	if telemetry.ActiveRequests != 3 {
		t.Fatalf("ActiveRequests = %d, want 3 (the negative rt_a entry clamped to 0, not summed as -999)", telemetry.ActiveRequests)
	}
}

// --- Task 3: Probe reachability status fields ---

// TestIngestTelemetrySampleRuntimeProbeReachability proves that a runtime
// sample carrying metrics_probe and context_probe reachability status fields
// correctly populates the RuntimeStatusDTO with those exact values.
func TestIngestTelemetrySampleRuntimeProbeReachability(t *testing.T) {
	srv := NewTestServer()
	seedRuntimeIngestSpec(t, srv, "rspec_probes", false)

	body := `{"host":{"cpu_util_pct":1},"runtimes":[{"spec_id":"rspec_probes","model":"qwen-coder","state":"running","since":"2026-08-20T10:00:00Z","pid":4242,"port":9001,"in_flight":2,"restarts":1,"context_size":8192,"active_requests":3,"queue_depth":5,"metrics_probe":"ok","context_probe":"unreachable"}]}`
	req, raw := ingestReq(t, body)
	if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	snap, _, unsub := srv.RuntimeStatus.subscribe("mock-host-qwen")
	defer unsub()
	if len(snap) != 1 {
		t.Fatalf("snapshot = %#v, want one entry", snap)
	}
	got := snap[0]
	if got.MetricsProbe != "ok" {
		t.Fatalf("MetricsProbe = %q, want %q", got.MetricsProbe, "ok")
	}
	if got.ContextProbe != "unreachable" {
		t.Fatalf("ContextProbe = %q, want %q", got.ContextProbe, "unreachable")
	}
}

// --- #49-3: the probe write path writes capability ROWS ------------------
//
// One writer now covers everything a per-runtime probe determines about a
// child's build -- the open capability verdict list AND the live-progress
// verdict, which lost its own writer when it became just another row. So the
// tests below share one spy, and the live-progress tests assert the
// routing.CapabilityLiveProgress row where they used to assert a column.

// countingCapabilityRowWriteStore counts the write-back's own
// UpsertMappingCapabilities calls and records the rows of each, so a test can
// assert exactly how many writes fired and with what -- the row-shaped
// successor to countingLiveProgressWriteStore/countingCapabilitiesWriteStore,
// and a sibling of countingContextWriteStore/countingMeasuredWriteStore
// above: same mechanism, different call.
//
// The CALL COUNT is what the no-write-amplification and precedence
// assertions turn on: a probe that correctly declines to write and one that
// writes an identical value reach the same stored state, so only the count
// distinguishes them.
type countingCapabilityRowWriteStore struct {
	*routing.MemoryStore
	upsertCalls atomic.Int32

	mu   sync.Mutex
	sent [][]routing.CapabilityRow
}

func (c *countingCapabilityRowWriteStore) UpsertMappingCapabilities(ctx context.Context, mappingID string, rows []routing.CapabilityRow) error {
	c.upsertCalls.Add(1)
	c.mu.Lock()
	c.sent = append(c.sent, append([]routing.CapabilityRow(nil), rows...))
	c.mu.Unlock()
	return c.MemoryStore.UpsertMappingCapabilities(ctx, mappingID, rows)
}

// lastSent returns the rows handed to the most recent
// UpsertMappingCapabilities call (nil when there was none), so a test can
// assert what ONE write carried rather than only the resulting state.
func (c *countingCapabilityRowWriteStore) lastSent() []routing.CapabilityRow {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.sent) == 0 {
		return nil
	}
	return c.sent[len(c.sent)-1]
}

// countingRowStore wraps srv.Routes in the spy above and returns it, the
// three-line preamble every test below shares.
func countingRowStore(srv *Server) *countingCapabilityRowWriteStore {
	counting := &countingCapabilityRowWriteStore{MemoryStore: srv.Routes.(*routing.MemoryStore)}
	srv.Routes = counting
	return counting
}

// seedCapabilityRow writes one capability row directly through the store API
// (never through the code path under test), so a test can establish "what is
// already on file" -- an operator's manual verdict, the vision benchmark's
// measurement, a legacy row migration 78 inherited, or an earlier probe's own
// answer.
func seedCapabilityRow(t *testing.T, srv *Server, mappingID, capability, verdict, source string) {
	t.Helper()
	if err := srv.Routes.UpsertMappingCapabilities(context.Background(), mappingID, []routing.CapabilityRow{{
		Capability: capability, Verdict: verdict, Source: source,
		CheckedAt: time.Now().UTC().Add(-time.Hour),
	}}); err != nil {
		t.Fatalf("seed %s=%s (%s) on %s: %v", capability, verdict, source, mappingID, err)
	}
}

// capabilityRow reads one stored capability row back, keyed by name. ok is
// false when the capability has NO row -- which is how "unknown" is expressed,
// so a test distinguishing "never determined" from a definitive verdict has
// to look at the bool, not at an empty verdict string.
func capabilityRow(t *testing.T, srv *Server, mappingID, capability string) (routing.CapabilityRow, bool) {
	t.Helper()
	rows, err := srv.Routes.MappingCapabilities(context.Background(), mappingID)
	if err != nil {
		t.Fatalf("MappingCapabilities(%s): %v", mappingID, err)
	}
	row, ok := routing.CapabilityRowsByName(rows)[capability]
	return row, ok
}

// assertCapabilityRow fails unless mappingID's capability row holds exactly
// this verdict and source. The SOURCE half is load-bearing, not decoration:
// it is what the precedence rule reads, so a test that only checked the
// verdict could not tell a probe's write from an operator's surviving one.
func assertCapabilityRow(t *testing.T, srv *Server, mappingID, capability, wantVerdict, wantSource string) {
	t.Helper()
	row, ok := capabilityRow(t, srv, mappingID, capability)
	if !ok {
		t.Fatalf("%s has NO %q row, want %s/%s", mappingID, capability, wantVerdict, wantSource)
	}
	if row.Verdict != wantVerdict || row.Source != wantSource {
		t.Fatalf("%s %q row = %s/%s, want %s/%s", mappingID, capability, row.Verdict, row.Source, wantVerdict, wantSource)
	}
}

// liveProgressBody builds a minimal runtime_model_probe-declaring telemetry
// body naming specID with the given live_progress_support verdict (omitted
// entirely from the JSON when support is ""). It carries no gpus, no
// context_size and no per-runtime capabilities object: that isolates the
// live-progress verdict's own path through the shared capability write-back
// from writeBackRuntimeVRAM's and writeBackRuntimeContext's, both of which
// `continue` before ever resolving a spec when their own preconditions (GPUs
// present / context_size > 0) are absent -- letting a memoization test count
// RuntimeSpecByID calls attributable to this path alone.
func liveProgressBody(specID, support string) string {
	field := ""
	if support != "" {
		field = `,"live_progress_support":"` + support + `"`
	}
	return `{"host":{"cpu_util_pct":1},"capabilities":{"features":["runtime_model_probe"]},` +
		`"runtimes":[{"spec_id":"` + specID + `","state":"running"` + field + `}]}`
}

// TestIngestLiveProgressLandsAsARow proves the live-progress verdict from a
// telemetry sample becomes a routing.CapabilityLiveProgress ROW -- "supported"
// mapping onto the row model's "yes" -- written by the SHARED capability
// write-back rather than by a writer of its own, and that an IDENTICAL second
// sample costs no further write.
//
// The shared-path half is asserted by sending a sample that carries BOTH a
// live-progress verdict and an ordinary capability verdict and requiring
// exactly ONE upsert holding BOTH rows: they come off one /props document in
// one probe pass, so a second write would mean the two facts had been split
// apart again.
//
// The no-rewrite half is writeBackRuntimeContext's F2 fix applied to a
// capability rather than a metric -- and it matters MORE here: a build
// capability is stable by nature, so the SAME child build reports the SAME
// verdict every single second for its whole life. Without change detection
// this would drive one write per second per mapping, forever, for a value
// that can never change short of an operator swapping the upstream binary.
func TestIngestLiveProgressLandsAsARow(t *testing.T) {
	srv := NewTestServer()
	seedRuntimeIngestSpec(t, srv, "rspec_lp_row", false)
	counting := countingRowStore(srv)

	body := `{"host":{"cpu_util_pct":1},"capabilities":{"features":["runtime_model_probe"]},` +
		`"runtimes":[{"spec_id":"rspec_lp_row","state":"running","live_progress_support":"supported",` +
		`"capabilities":{"verdicts":[{"name":"vision","verdict":"yes"}]}}]}`
	for i := 0; i < 2; i++ {
		req, raw := ingestReq(t, body)
		if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
			t.Fatalf("ingest %d: %v", i, err)
		}
	}
	if got := counting.upsertCalls.Load(); got != 1 {
		t.Fatalf("UpsertMappingCapabilities calls = %d across two samples carrying the SAME verdicts, want exactly 1", got)
	}
	if got := len(counting.lastSent()); got != 2 {
		t.Fatalf("the single write carried %d rows (%+v), want 2 -- the live-progress verdict must ride the SAME write as the capability verdicts, not a second one", got, counting.lastSent())
	}
	assertCapabilityRow(t, srv, "map_rspec_lp_row", routing.CapabilityLiveProgress, routing.CapabilityYes, routing.CapabilitySourceLlamaCppProps)
	assertCapabilityRow(t, srv, "map_rspec_lp_row", routing.CapabilityVision, routing.CapabilityYes, routing.CapabilitySourceLlamaCppProps)
}

// TestIngestDedicatedLiveProgressFieldBeatsTheOpenVerdictList proves the
// resolution of the one collision the OPEN capability vocabulary makes
// possible on this path: an agent that both reports a "live_progress" verdict
// in its open Verdicts list AND fills the dedicated live_progress_support
// field. The dedicated field wins.
//
// The mechanism is a pair, and neither half works alone:
// runtimeSampleCapabilityRows emits the dedicated row FIRST, and rule 0 of
// routing.WritableCapabilityRows keeps the first row for a capability name
// and drops every later one. Before rule 0 the same outcome came out of the
// upsert loop applying both rows in order and the last one winning -- which
// is why the projection appended the dedicated row LAST. Keeping rule 0 and
// that old order together would silently hand the open list the last word,
// so the order is now load-bearing in the opposite direction.
//
// The write carries ONE row, not two: a duplicated name must never reach the
// store twice, whatever the store would then do with it.
func TestIngestDedicatedLiveProgressFieldBeatsTheOpenVerdictList(t *testing.T) {
	srv := NewTestServer()
	seedRuntimeIngestSpec(t, srv, "rspec_lp_dup", false)
	counting := countingRowStore(srv)

	// The dedicated field says unsupported; the open list claims the same
	// capability is supported. The verdicts are opposite on purpose, so the
	// assertion cannot pass because both happen to agree.
	body := `{"host":{"cpu_util_pct":1},"capabilities":{"features":["runtime_model_probe"]},` +
		`"runtimes":[{"spec_id":"rspec_lp_dup","state":"running","live_progress_support":"unsupported",` +
		`"capabilities":{"verdicts":[{"name":"live_progress","verdict":"yes"}]}}]}`
	req, raw := ingestReq(t, body)
	if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if sent := counting.lastSent(); len(sent) != 1 {
		t.Fatalf("the write carried %d rows (%+v), want exactly 1 -- one capability name, one row", len(sent), sent)
	}
	assertCapabilityRow(t, srv, "map_rspec_lp_dup", routing.CapabilityLiveProgress,
		routing.CapabilityNo, routing.CapabilitySourceLlamaCppProps)
}

// TestIngestTelemetrySampleLiveProgressWriteBackPersistsOnce proves an
// "unsupported" sample lands as a live_progress row carrying the row model's
// "no", and that an IDENTICAL second sample costs no further write. The
// negative direction is the interesting one: it is the verdict a bool column
// could not tell apart from "never probed", and the row model can.
func TestIngestTelemetrySampleLiveProgressWriteBackPersistsOnce(t *testing.T) {
	srv := NewTestServer()
	seedRuntimeIngestSpec(t, srv, "rspec_lp_once", false)
	counting := countingRowStore(srv)

	body := liveProgressBody("rspec_lp_once", "unsupported")
	for i := 0; i < 2; i++ {
		req, raw := ingestReq(t, body)
		if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
			t.Fatalf("ingest %d: %v", i, err)
		}
	}
	if got := counting.upsertCalls.Load(); got != 1 {
		t.Fatalf("UpsertMappingCapabilities calls = %d across two samples carrying the SAME verdict, want exactly 1", got)
	}
	assertCapabilityRow(t, srv, "map_rspec_lp_once", routing.CapabilityLiveProgress, routing.CapabilityNo, routing.CapabilitySourceLlamaCppProps)
}

// TestIngestTelemetrySampleLiveProgressWriteBackSkipsEmptySample proves a
// sample whose live_progress_support is "" -- an older agent that predates
// the field, or a child whose build this agent has not yet reached a stable
// verdict for -- never writes: unknown must never overwrite a stored
// verdict. The mapping is seeded to ALREADY hold "yes" (through the store API
// directly, not the code path under test), so this assertion cannot pass
// merely because both sides happen to be empty.
func TestIngestTelemetrySampleLiveProgressWriteBackSkipsEmptySample(t *testing.T) {
	srv := NewTestServer()
	seedRuntimeIngestSpec(t, srv, "rspec_lp_empty", false)
	ctx := context.Background()
	seedCapabilityRow(t, srv, "map_rspec_lp_empty", routing.CapabilityLiveProgress, routing.CapabilityYes, routing.CapabilitySourceLlamaCppProps)
	counting := countingRowStore(srv)

	body := liveProgressBody("rspec_lp_empty", "")
	req, raw := ingestReq(t, body)
	if err := srv.ingestTelemetrySample(ctx, "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if got := counting.upsertCalls.Load(); got != 0 {
		t.Fatalf(`UpsertMappingCapabilities calls = %d for an empty ("") sample, want 0 -- unknown must never overwrite a stored verdict`, got)
	}
	assertCapabilityRow(t, srv, "map_rspec_lp_empty", routing.CapabilityLiveProgress, routing.CapabilityYes, routing.CapabilitySourceLlamaCppProps)
}

// TestIngestTelemetrySampleLiveProgressWriteBackUpdatesLockedMapping proves
// the deliberate difference from writeBackRuntimeContext: UNLIKE the
// context-size write-back, a metrics_locked mapping's capabilities ARE still
// written. A capability is a property of the upstream build, not a metric an
// operator pins numbers against -- see
// routing.MappingStore.UpsertMappingCapabilities for the full rationale, and
// note that model_mapping_capabilities carries no lock guard for an in-Go
// pre-check here to mirror. The mapping's manually pinned
// context_size/metrics_source must remain untouched: only the orthogonal
// capability write is unlocked.
func TestIngestTelemetrySampleLiveProgressWriteBackUpdatesLockedMapping(t *testing.T) {
	srv := NewTestServer()
	seedRuntimeIngestSpec(t, srv, "rspec_lp_locked", false)
	ctx := context.Background()

	mapping, err := srv.Routes.MappingByID(ctx, "map_rspec_lp_locked")
	if err != nil {
		t.Fatalf("MappingByID: %v", err)
	}
	mapping.MetricsLocked = true
	mapping.ContextSize = 4096
	mapping.MetricsSource = "manual"
	if err := srv.Routes.UpdateMapping(ctx, mapping); err != nil {
		t.Fatalf("UpdateMapping: %v", err)
	}

	counting := countingRowStore(srv)

	body := liveProgressBody("rspec_lp_locked", "supported")
	req, raw := ingestReq(t, body)
	if err := srv.ingestTelemetrySample(ctx, "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if got := counting.upsertCalls.Load(); got != 1 {
		t.Fatalf("UpsertMappingCapabilities calls = %d, want exactly 1 -- a locked mapping's capability must still be written", got)
	}
	assertCapabilityRow(t, srv, "map_rspec_lp_locked", routing.CapabilityLiveProgress, routing.CapabilityYes, routing.CapabilitySourceLlamaCppProps)
	got, err := srv.Routes.MappingByID(ctx, "map_rspec_lp_locked")
	if err != nil {
		t.Fatalf("MappingByID: %v", err)
	}
	if got.ContextSize != 4096 || got.MetricsSource != "manual" {
		t.Fatalf("mapping = %#v, want the manually pinned context_size/metrics_source left untouched", got)
	}
}

// TestIngestTelemetrySampleLiveProgressWriteBackWritesOnChange proves change
// detection is not "write once and never again": a verdict that genuinely
// moves (e.g. an operator swaps the upstream binary for one with a different
// build) must still be written.
func TestIngestTelemetrySampleLiveProgressWriteBackWritesOnChange(t *testing.T) {
	srv := NewTestServer()
	seedRuntimeIngestSpec(t, srv, "rspec_lp_change", false)
	counting := countingRowStore(srv)
	ctx := context.Background()

	req, raw := ingestReq(t, liveProgressBody("rspec_lp_change", "unsupported"))
	if err := srv.ingestTelemetrySample(ctx, "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest 1: %v", err)
	}
	if got := counting.upsertCalls.Load(); got != 1 {
		t.Fatalf("UpsertMappingCapabilities calls after first ingest = %d, want 1", got)
	}

	req, raw = ingestReq(t, liveProgressBody("rspec_lp_change", "supported"))
	if err := srv.ingestTelemetrySample(ctx, "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest 2 (changed): %v", err)
	}
	if got := counting.upsertCalls.Load(); got != 2 {
		t.Fatalf("UpsertMappingCapabilities calls = %d after a CHANGED verdict (unsupported -> supported), want 2", got)
	}
	assertCapabilityRow(t, srv, "map_rspec_lp_change", routing.CapabilityLiveProgress, routing.CapabilityYes, routing.CapabilitySourceLlamaCppProps)
}

// manyLiveProgressSamples builds n agentRuntimeSample entries all naming
// specID with the given live_progress_support verdict and no gpus/
// context_size -- isolating the capability write-back's own resolution from
// writeBackRuntimeVRAM's and writeBackRuntimeContext's, both of which
// `continue` before ever resolving a spec when their own preconditions (GPUs
// present / context_size > 0) are absent.
func manyLiveProgressSamples(n int, specID, support string) []agentRuntimeSample {
	out := make([]agentRuntimeSample, n)
	for i := range out {
		out[i] = agentRuntimeSample{SpecID: specID, State: "running", LiveProgressSupport: support}
	}
	return out
}

// TestIngestTelemetrySampleLiveProgressWriteBackMemoizesRepeatedSpecID proves
// a snapshot repeating one spec_id many times resolves it exactly once:
// telemetry arrives roughly once per second as a FULL SNAPSHOT, so a sample
// naming the same spec_id many times over must not re-run the ownership
// resolution chain (RuntimeSpecByID + MappingByID + ApplicationByID) once per
// occurrence. A call-count spy on RuntimeSpecByID is the honest way to prove
// this: both a memoized and an un-memoized loop reach the same final stored
// value, so only the call count distinguishes them.
func TestIngestTelemetrySampleLiveProgressWriteBackMemoizesRepeatedSpecID(t *testing.T) {
	srv := NewTestServer()
	seedRuntimeIngestSpec(t, srv, "rspec_lp_memo", false)
	counting := &countingRuntimeSpecStore{MemoryStore: srv.Routes.(*routing.MemoryStore)}
	srv.Routes = counting

	req := agentTelemetryRequest{
		Host:         &agentHostReport{CPUUtilPct: 1},
		Capabilities: json.RawMessage(`{"features":["runtime_model_probe"]}`),
		Runtimes:     manyLiveProgressSamples(10, "rspec_lp_memo", "supported"),
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if got := counting.runtimeSpecByIDCalls.Load(); got != 1 {
		t.Fatalf("RuntimeSpecByID calls = %d, want exactly 1 (one spec_id repeated 10x in one snapshot must resolve once)", got)
	}
}

// TestIngestTelemetrySampleLiveProgressWriteBackRejectsCrossServerSpec is the
// capability write-back's own cross-tenant guard, the third sibling of
// TestIngestTelemetrySampleRuntimeVRAMWriteBackRejectsCrossServerSpec and
// TestIngestTelemetrySampleRuntimeContextWriteBackRejectsCrossServerSpec:
// spec_id is an agent-supplied body field with no other verification anywhere
// on this path, and the only thing binding a sample to a server is the
// token-derived serverID. An agent authenticated for one server must not be
// able to overwrite another server's mapping verdict by naming its spec_id.
// Until this test, deleting the ownership check passed the whole suite.
//
// The sample carries TWO runtimes: the foreign spec plus a control spec the
// reporting server does own. That makes the assertions unfalsifiable by
// accident -- the write count must be exactly 1 (the control's), so removing
// the ownership check turns it into 2, and the foreign mapping is seeded with
// a DISTINGUISHABLE stored verdict ("no") that the forged sample would flip
// to "yes". The Warn is asserted too, matching the VRAM sibling's audit-trail
// discipline: an agent naming another server's resources is a signal worth
// keeping.
func TestIngestTelemetrySampleLiveProgressWriteBackRejectsCrossServerSpec(t *testing.T) {
	srv := NewTestServer()
	ctx := context.Background()
	now := time.Now().UTC()
	const otherServerID = "mock-host-other-tenant-lp"
	if err := srv.Routes.CreateAIServer(ctx, routing.AIServer{
		ID: otherServerID, Name: otherServerID, Domain: otherServerID + ".example.test",
		Provider: routing.ProviderMock, Endpoint: "mock://" + otherServerID,
		Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create other server: %v", err)
	}
	// rspec_cross_lp belongs to otherServerID; rspec_own_lp to the reporting one.
	seedRuntimeIngestSpecForServer(t, srv, otherServerID, "rspec_cross_lp", false)
	seedRuntimeIngestSpec(t, srv, "rspec_own_lp", false)
	seedCapabilityRow(t, srv, "map_rspec_cross_lp", routing.CapabilityLiveProgress, routing.CapabilityNo, routing.CapabilitySourceLlamaCppProps)

	counting := countingRowStore(srv)

	buf, restore := withCapturedSlog(t)
	defer restore()

	body := `{"host":{"cpu_util_pct":1},"capabilities":{"features":["runtime_model_probe"]},"runtimes":[` +
		`{"spec_id":"rspec_cross_lp","state":"running","live_progress_support":"supported"},` +
		`{"spec_id":"rspec_own_lp","state":"running","live_progress_support":"supported"}]}`
	req, raw := ingestReq(t, body)
	if err := srv.ingestTelemetrySample(ctx, "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest must succeed (best-effort write-back) even when the sample names another server's spec_id: %v", err)
	}

	if got := counting.upsertCalls.Load(); got != 1 {
		t.Fatalf("UpsertMappingCapabilities calls = %d, want exactly 1 (only the control spec this server owns)", got)
	}
	assertCapabilityRow(t, srv, "map_rspec_cross_lp", routing.CapabilityLiveProgress, routing.CapabilityNo, routing.CapabilitySourceLlamaCppProps)
	assertCapabilityRow(t, srv, "map_rspec_own_lp", routing.CapabilityLiveProgress, routing.CapabilityYes, routing.CapabilitySourceLlamaCppProps)
	if !findLogRecord(buf.Snapshot(), "WARN", "spec belongs to a different server") {
		t.Fatal("a cross-server naming attempt must log a Warn, not a Debug -- an agent naming another server's resources is an audit signal, not a merely stale id")
	}
}

// TestIngestTelemetrySampleLiveProgressWriteBackGuardedByCapability pins the
// runtime_model_probe gate on this write-back, mirroring
// TestIngestTelemetrySampleRuntimeContextWriteBackGuardedByCapability for its
// context sibling. The verdict rides on the exact same per-runtime probe pass
// (server-agent's probeRuntimeChild) that produces context_size, so it shares
// that pass's trust boundary: an agent that has never declared
// runtime_model_probe must never have a mapping's stored capability touched
// from this path. That ruling had nothing protecting it -- the gate could be
// deleted with the suite still green.
//
// The mapping is seeded with a stored "no" that the ungated write would flip
// to "yes", and the writer's call count is asserted at zero, so neither
// assertion can pass off an empty-equals-empty coincidence.
func TestIngestTelemetrySampleLiveProgressWriteBackGuardedByCapability(t *testing.T) {
	srv := NewTestServer()
	seedRuntimeIngestSpec(t, srv, "rspec_lp_nocap", false)
	ctx := context.Background()
	seedCapabilityRow(t, srv, "map_rspec_lp_nocap", routing.CapabilityLiveProgress, routing.CapabilityNo, routing.CapabilitySourceLlamaCppProps)
	counting := countingRowStore(srv)

	// No "capabilities" object at all -- an agent that never declared
	// runtime_model_probe, exactly like an older agent build.
	body := `{"host":{"cpu_util_pct":1},"runtimes":[{"spec_id":"rspec_lp_nocap","state":"running","live_progress_support":"supported"}]}`
	req, raw := ingestReq(t, body)
	if err := srv.ingestTelemetrySample(ctx, "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	if got := counting.upsertCalls.Load(); got != 0 {
		t.Fatalf("UpsertMappingCapabilities calls = %d, want 0 (the agent did not declare runtime_model_probe)", got)
	}
	assertCapabilityRow(t, srv, "map_rspec_lp_nocap", routing.CapabilityLiveProgress, routing.CapabilityNo, routing.CapabilitySourceLlamaCppProps)
}

// TestIngestTelemetrySampleLiveProgressCarriedOnStatusDTO proves a runtime
// sample's live_progress_support reaches the volatile RuntimeStatusDTO the
// portal's live SSE stream serves, mirroring
// TestIngestTelemetrySampleRuntimeProbeReachability for MetricsProbe/
// ContextProbe. Unlike the store write-back, DTO publishing is unconditional
// -- it is not gated on the runtime_model_probe capability.
func TestIngestTelemetrySampleLiveProgressCarriedOnStatusDTO(t *testing.T) {
	srv := NewTestServer()
	seedRuntimeIngestSpec(t, srv, "rspec_lp_dto", false)

	body := `{"host":{"cpu_util_pct":1},"runtimes":[{"spec_id":"rspec_lp_dto","state":"running","live_progress_support":"unsupported"}]}`
	req, raw := ingestReq(t, body)
	if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	snap, _, unsub := srv.RuntimeStatus.subscribe("mock-host-qwen")
	defer unsub()
	if len(snap) != 1 {
		t.Fatalf("snapshot = %#v, want one entry", snap)
	}
	if snap[0].LiveProgressSupport != "unsupported" {
		t.Fatalf("LiveProgressSupport = %q, want %q", snap[0].LiveProgressSupport, "unsupported")
	}
}

// --- #49-3: capability verdicts and the operator's precedence rule -------

// capabilitiesBody builds a minimal runtime_model_probe-declaring telemetry
// body naming specID, with the given JSON object embedded verbatim as the
// runtime entry's own "capabilities" field (e.g.
// `{"verdicts":[{"name":"vision","verdict":"yes"}]}` or `{}`). Pass "" to
// omit the key entirely -- an older agent that predates capability
// detection, decoding to a nil *agentRuntimeCapabilitiesSample rather than a
// zero-valued one.
func capabilitiesBody(specID, capsJSON string) string {
	field := ""
	if capsJSON != "" {
		field = `,"capabilities":` + capsJSON
	}
	return `{"host":{"cpu_util_pct":1},"capabilities":{"features":["runtime_model_probe"]},` +
		`"runtimes":[{"spec_id":"` + specID + `","state":"running"` + field + `}]}`
}

// TestIngestWritesBackCapabilities proves a sample carrying definitive
// capability verdicts becomes one row per capability -- each stamped with the
// llama_cpp_props SOURCE, which is what makes the precedence rule
// expressible at all -- and that an IDENTICAL second sample costs no further
// write: the capability write-back's own instance of the compare-to-stored
// discipline. Without it, every ~1s telemetry sample from every managed
// process would drive one write per mapping, forever, for verdicts that can
// only change if an operator swaps the upstream binary.
func TestIngestWritesBackCapabilities(t *testing.T) {
	srv := NewTestServer()
	seedRuntimeIngestSpec(t, srv, "rspec_caps_once", false)
	counting := countingRowStore(srv)

	body := capabilitiesBody("rspec_caps_once", `{"verdicts":[{"name":"vision","verdict":"yes"},{"name":"tools","verdict":"no"}]}`)
	for i := 0; i < 2; i++ {
		req, raw := ingestReq(t, body)
		if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
			t.Fatalf("ingest %d: %v", i, err)
		}
	}
	if got := counting.upsertCalls.Load(); got != 1 {
		t.Fatalf("UpsertMappingCapabilities calls = %d across two samples carrying the SAME verdicts, want exactly 1", got)
	}
	assertCapabilityRow(t, srv, "map_rspec_caps_once", routing.CapabilityVision, routing.CapabilityYes, routing.CapabilitySourceLlamaCppProps)
	assertCapabilityRow(t, srv, "map_rspec_caps_once", routing.CapabilityTools, routing.CapabilityNo, routing.CapabilitySourceLlamaCppProps)
	if _, ok := capabilityRow(t, srv, "map_rspec_caps_once", routing.CapabilityAudio); ok {
		t.Fatal("an unreported capability must have NO row -- absence is how unknown is expressed, not an empty verdict")
	}
}

// TestIngestProbeWritesANoVerdictOnAnUnknownCapabilityName is the trap the
// pre-row shim left behind, pinned. The vocabulary of capability NAMES is
// open: an upstream may report one this binary has never heard of. The old
// projection could only fold such a name into a mapping-wide "extra" list
// that had no verdict of its own, so an unknown name's "yes" survived as an
// implicit positive while its "no" was silently DROPPED -- there was nowhere
// to put it.
//
// With one row per capability there is. Both directions must land, and the
// asymmetry must not survive the rewrite: this test sends a known name and
// two unknown ones, one "yes" and one "no", and requires all three rows.
func TestIngestProbeWritesANoVerdictOnAnUnknownCapabilityName(t *testing.T) {
	srv := NewTestServer()
	seedRuntimeIngestSpec(t, srv, "rspec_caps_unknown", false)
	counting := countingRowStore(srv)

	body := capabilitiesBody("rspec_caps_unknown", `{"verdicts":[`+
		`{"name":"vision","verdict":"yes"},`+
		`{"name":"future_modality","verdict":"no"},`+
		`{"name":"thinking","verdict":"yes"}]}`)
	req, raw := ingestReq(t, body)
	if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if got := counting.upsertCalls.Load(); got != 1 {
		t.Fatalf("UpsertMappingCapabilities calls = %d, want exactly 1", got)
	}
	assertCapabilityRow(t, srv, "map_rspec_caps_unknown", routing.CapabilityVision, routing.CapabilityYes, routing.CapabilitySourceLlamaCppProps)
	assertCapabilityRow(t, srv, "map_rspec_caps_unknown", "thinking", routing.CapabilityYes, routing.CapabilitySourceLlamaCppProps)
	row, ok := capabilityRow(t, srv, "map_rspec_caps_unknown", "future_modality")
	if !ok {
		t.Fatal(`no "future_modality" row -- an unknown capability name with a "no" verdict is a perfectly good row and must be written, not dropped the way the pre-row projection had to`)
	}
	if row.Verdict != routing.CapabilityNo {
		t.Fatalf(`"future_modality" verdict = %q, want %q -- the negative direction is exactly what the old shim lost`, row.Verdict, routing.CapabilityNo)
	}
}

// TestIngestProbeDoesNotOverwriteAManualVerdict is the operator's rule, and
// the reason this whole sub-project exists: a verdict a HUMAN established
// must survive a probe reporting the opposite, forever, without the operator
// having to lock anything. Provenance replaces the lock -- see
// routing.MappingStore.UpsertMappingCapabilities.
//
// The manual row says vision=no; the sample says vision=yes. The write count
// must be zero FOR THAT CAPABILITY, which the test makes unfalsifiable by
// sending a second, unmanaged verdict (tools) in the same document: exactly
// one write must fire, and it must carry only the tools row. A test that
// merely counted writes at zero could be satisfied by a write path that had
// stopped working altogether.
func TestIngestProbeDoesNotOverwriteAManualVerdict(t *testing.T) {
	srv := NewTestServer()
	seedRuntimeIngestSpec(t, srv, "rspec_caps_manual", false)
	seedCapabilityRow(t, srv, "map_rspec_caps_manual", routing.CapabilityVision, routing.CapabilityNo, routing.CapabilitySourceManual)
	counting := countingRowStore(srv)

	body := capabilitiesBody("rspec_caps_manual", `{"verdicts":[{"name":"vision","verdict":"yes"},{"name":"tools","verdict":"yes"}]}`)
	req, raw := ingestReq(t, body)
	if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	if got := counting.upsertCalls.Load(); got != 1 {
		t.Fatalf("UpsertMappingCapabilities calls = %d, want exactly 1 (the unmanaged tools verdict)", got)
	}
	sent := counting.lastSent()
	if len(sent) != 1 || sent[0].Capability != routing.CapabilityTools {
		t.Fatalf("the write carried %+v, want exactly the tools row -- a probe must never send a capability a human established", sent)
	}
	assertCapabilityRow(t, srv, "map_rspec_caps_manual", routing.CapabilityVision, routing.CapabilityNo, routing.CapabilitySourceManual)
	assertCapabilityRow(t, srv, "map_rspec_caps_manual", routing.CapabilityTools, routing.CapabilityYes, routing.CapabilitySourceLlamaCppProps)
}

// TestIngestProbeDoesNotOverwriteABenchmarkVerdict is the same rule for the
// other authoritative source: the vision benchmark MEASURED this -- it sent
// a real image to the real upstream and read the real answer -- so a probe
// re-reading a /props document must not talk over it either. This is also
// the "and a subsequent probe leaves it alone" half of
// TestVisionBenchmarkWritesAnAuthoritativeRow, which writes such a row
// through the benchmark runner itself.
//
// Unfalsifiable the same way as the manual sibling: a second, unmanaged
// verdict must still get through, so exactly one write fires and carries
// only that row.
func TestIngestProbeDoesNotOverwriteABenchmarkVerdict(t *testing.T) {
	srv := NewTestServer()
	seedRuntimeIngestSpec(t, srv, "rspec_caps_bench", false)
	seedCapabilityRow(t, srv, "map_rspec_caps_bench", routing.CapabilityVision, routing.CapabilityNo, routing.CapabilitySourceVisionBenchmark)
	counting := countingRowStore(srv)

	body := capabilitiesBody("rspec_caps_bench", `{"verdicts":[{"name":"vision","verdict":"yes"},{"name":"tools","verdict":"yes"}]}`)
	req, raw := ingestReq(t, body)
	if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	if got := counting.upsertCalls.Load(); got != 1 {
		t.Fatalf("UpsertMappingCapabilities calls = %d, want exactly 1 (the unmanaged tools verdict)", got)
	}
	sent := counting.lastSent()
	if len(sent) != 1 || sent[0].Capability != routing.CapabilityTools {
		t.Fatalf("the write carried %+v, want exactly the tools row -- a probe must never send a capability a real measurement established", sent)
	}
	assertCapabilityRow(t, srv, "map_rspec_caps_bench", routing.CapabilityVision, routing.CapabilityNo, routing.CapabilitySourceVisionBenchmark)
}

// TestIngestProbeOverwritesItsOwnAndLegacyVerdicts is precedence's other
// half, and it is what keeps the rule from degenerating into "never write
// anything": a row whose source is a PROBE source is overwritable, which is
// how a verdict re-establishes itself after an operator swaps the upstream
// binary. legacy (migration 78's inheritance from the pre-78 columns) counts
// as a probe source deliberately -- its real origin is unknowable, and
// treating a guess as authoritative would freeze it in forever.
//
// Three sub-cases, each on its own seeded mapping: a differing
// llama_cpp_props row is replaced, a differing legacy row is replaced (and
// re-sourced), and an AGREEING probe row is not rewritten at all.
func TestIngestProbeOverwritesItsOwnAndLegacyVerdicts(t *testing.T) {
	ctx := context.Background()

	t.Run("a differing llama_cpp_props row is replaced", func(t *testing.T) {
		srv := NewTestServer()
		seedRuntimeIngestSpec(t, srv, "rspec_caps_own", false)
		seedCapabilityRow(t, srv, "map_rspec_caps_own", routing.CapabilityVision, routing.CapabilityNo, routing.CapabilitySourceLlamaCppProps)
		counting := countingRowStore(srv)

		req, raw := ingestReq(t, capabilitiesBody("rspec_caps_own", `{"verdicts":[{"name":"vision","verdict":"yes"}]}`))
		if err := srv.ingestTelemetrySample(ctx, "mock-host-qwen", req, raw); err != nil {
			t.Fatalf("ingest: %v", err)
		}
		if got := counting.upsertCalls.Load(); got != 1 {
			t.Fatalf("UpsertMappingCapabilities calls = %d, want exactly 1 -- a probe's own earlier verdict is overwritable", got)
		}
		assertCapabilityRow(t, srv, "map_rspec_caps_own", routing.CapabilityVision, routing.CapabilityYes, routing.CapabilitySourceLlamaCppProps)
	})

	t.Run("a differing legacy row is replaced", func(t *testing.T) {
		srv := NewTestServer()
		seedRuntimeIngestSpec(t, srv, "rspec_caps_legacy", false)
		seedCapabilityRow(t, srv, "map_rspec_caps_legacy", routing.CapabilityVision, routing.CapabilityNo, routing.CapabilitySourceLegacy)
		counting := countingRowStore(srv)

		req, raw := ingestReq(t, capabilitiesBody("rspec_caps_legacy", `{"verdicts":[{"name":"vision","verdict":"yes"}]}`))
		if err := srv.ingestTelemetrySample(ctx, "mock-host-qwen", req, raw); err != nil {
			t.Fatalf("ingest: %v", err)
		}
		if got := counting.upsertCalls.Load(); got != 1 {
			t.Fatalf("UpsertMappingCapabilities calls = %d, want exactly 1 -- a legacy verdict's real origin is unknowable, so it must stay probe-overwritable", got)
		}
		assertCapabilityRow(t, srv, "map_rspec_caps_legacy", routing.CapabilityVision, routing.CapabilityYes, routing.CapabilitySourceLlamaCppProps)
	})

	t.Run("an agreeing probe row is not rewritten", func(t *testing.T) {
		srv := NewTestServer()
		seedRuntimeIngestSpec(t, srv, "rspec_caps_agree", false)
		seedCapabilityRow(t, srv, "map_rspec_caps_agree", routing.CapabilityVision, routing.CapabilityYes, routing.CapabilitySourceLlamaCppProps)
		counting := countingRowStore(srv)

		req, raw := ingestReq(t, capabilitiesBody("rspec_caps_agree", `{"verdicts":[{"name":"vision","verdict":"yes"}]}`))
		if err := srv.ingestTelemetrySample(ctx, "mock-host-qwen", req, raw); err != nil {
			t.Fatalf("ingest: %v", err)
		}
		if got := counting.upsertCalls.Load(); got != 0 {
			t.Fatalf("UpsertMappingCapabilities calls = %d, want 0 -- the stored verdict already agrees, and this sample arrives once a second forever", got)
		}
		assertCapabilityRow(t, srv, "map_rspec_caps_agree", routing.CapabilityVision, routing.CapabilityYes, routing.CapabilitySourceLlamaCppProps)
	})
}

// TestIngestStampsTheSourceTheAgentReported is #54's fix round on the
// gateway side: the PRODUCER names the provenance and this ingest uses what
// it reported, because the wire carries no runtime type and an inferred
// provenance is exactly what this column exists to prevent. Before the
// source rode the wire, an Ollama-declared verdict reached the operator's
// capability tooltip stamped "llama_cpp_props" -- a false attribution on the
// one column whose whole job is to say who said this.
//
// The three accepted inputs and their rows, each on its own mapping so no
// subtest can be satisfied by another's write:
//
//   - the reported ollama_api_show lands rows sourced ollama_api_show;
//   - the reported llama_cpp_props lands rows sourced llama_cpp_props;
//   - NO source key at all keeps today's default, llama_cpp_props -- safe
//     rather than convenient, because the only agent that omits it probes
//     /props, which an Ollama child answers with a 404 (no verdicts, hence
//     no rows). See rowSource.
func TestIngestStampsTheSourceTheAgentReported(t *testing.T) {
	ctx := context.Background()

	t.Run("an ollama-probed sample lands ollama_api_show rows", func(t *testing.T) {
		srv := NewTestServer()
		seedRuntimeIngestSpec(t, srv, "rspec_src_ollama", false)
		counting := countingRowStore(srv)

		req, raw := ingestReq(t, capabilitiesBody("rspec_src_ollama",
			`{"verdicts":[{"name":"vision","verdict":"yes"},{"name":"thinking","verdict":"yes"}],"source":"ollama_api_show"}`))
		if err := srv.ingestTelemetrySample(ctx, "mock-host-qwen", req, raw); err != nil {
			t.Fatalf("ingest: %v", err)
		}
		if got := counting.upsertCalls.Load(); got != 1 {
			t.Fatalf("UpsertMappingCapabilities calls = %d, want exactly 1", got)
		}
		assertCapabilityRow(t, srv, "map_rspec_src_ollama", routing.CapabilityVision, routing.CapabilityYes, routing.CapabilitySourceOllamaAPIShow)
		assertCapabilityRow(t, srv, "map_rspec_src_ollama", "thinking", routing.CapabilityYes, routing.CapabilitySourceOllamaAPIShow)
	})

	t.Run("a llama_cpp-probed sample lands llama_cpp_props rows", func(t *testing.T) {
		srv := NewTestServer()
		seedRuntimeIngestSpec(t, srv, "rspec_src_props", false)
		counting := countingRowStore(srv)

		req, raw := ingestReq(t, capabilitiesBody("rspec_src_props",
			`{"verdicts":[{"name":"vision","verdict":"no"}],"source":"llama_cpp_props"}`))
		if err := srv.ingestTelemetrySample(ctx, "mock-host-qwen", req, raw); err != nil {
			t.Fatalf("ingest: %v", err)
		}
		if got := counting.upsertCalls.Load(); got != 1 {
			t.Fatalf("UpsertMappingCapabilities calls = %d, want exactly 1", got)
		}
		assertCapabilityRow(t, srv, "map_rspec_src_props", routing.CapabilityVision, routing.CapabilityNo, routing.CapabilitySourceLlamaCppProps)
	})

	t.Run("no source key keeps the historical default", func(t *testing.T) {
		srv := NewTestServer()
		seedRuntimeIngestSpec(t, srv, "rspec_src_absent", false)
		counting := countingRowStore(srv)

		req, raw := ingestReq(t, capabilitiesBody("rspec_src_absent",
			`{"verdicts":[{"name":"vision","verdict":"yes"}]}`))
		if err := srv.ingestTelemetrySample(ctx, "mock-host-qwen", req, raw); err != nil {
			t.Fatalf("ingest: %v", err)
		}
		if got := counting.upsertCalls.Load(); got != 1 {
			t.Fatalf("UpsertMappingCapabilities calls = %d, want exactly 1 -- an agent predating the source field must keep working unchanged", got)
		}
		assertCapabilityRow(t, srv, "map_rspec_src_absent", routing.CapabilityVision, routing.CapabilityYes, routing.CapabilitySourceLlamaCppProps)
	})

	t.Run("no capabilities object at all still stamps the live-progress row", func(t *testing.T) {
		// The nil-receiver half of the default: an agent old enough to
		// predate capability DETECTION reports live_progress_support and no
		// "capabilities" key whatsoever, and its one row must still be
		// attributed exactly as before. Without this case the nil branch of
		// rowSource was pinned only incidentally, by a routing e2e test.
		srv := NewTestServer()
		seedRuntimeIngestSpec(t, srv, "rspec_src_nilobj", false)
		counting := countingRowStore(srv)

		body := `{"host":{"cpu_util_pct":1},"capabilities":{"features":["runtime_model_probe"]},` +
			`"runtimes":[{"spec_id":"rspec_src_nilobj","state":"running","live_progress_support":"unsupported"}]}`
		req, raw := ingestReq(t, body)
		if err := srv.ingestTelemetrySample(ctx, "mock-host-qwen", req, raw); err != nil {
			t.Fatalf("ingest: %v", err)
		}
		if got := counting.upsertCalls.Load(); got != 1 {
			t.Fatalf("UpsertMappingCapabilities calls = %d, want exactly 1 -- a sample with no capabilities object still carries a live-progress verdict", got)
		}
		assertCapabilityRow(t, srv, "map_rspec_src_nilobj", routing.CapabilityLiveProgress, routing.CapabilityNo, routing.CapabilitySourceLlamaCppProps)
	})
}

// TestIngestRejectsAnUnclaimableCapabilitySource is the ruling on a source
// this gateway does not recognise: the rows are DROPPED, never clamped onto
// the default. Clamping would print a provenance nobody reported on the
// operator-visible column -- a fabricated attribution, the very defect the
// column exists to prevent -- while dropping leaves the capability UNKNOWN,
// which this model expresses natively as the absence of a row.
//
// Three inputs, and the last two are why the allowlist is exactly the two
// PROBE sources rather than a typo filter: an agent has no standing to claim
// "manual" (rank 3, "an operator said so") or "vision_benchmark" (rank 2, a
// real image actually sent to the real upstream). A blind write would put
// either lie in front of an operator AND, through
// capabilitySourceRank's default branch ranking an unknown source at 1, let
// an unknown-provenance verdict overwrite a real probe's row at equal rank.
//
// Each case is unfalsifiable rather than merely quiet: the mapping is seeded
// through the store API with a DIFFERING stored verdict, so a write that got
// through would flip it and be visible, and the sample also carries a second
// capability with no stored row at all, so "nothing was written" cannot be
// satisfied by a write path that had simply stopped working.
func TestIngestRejectsAnUnclaimableCapabilitySource(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, spec, source string
	}{
		{"an unrecognised source", "rspec_src_bogus", "totally_made_up_probe"},
		{"an agent claiming manual", "rspec_src_manual", routing.CapabilitySourceManual},
		{"an agent claiming vision_benchmark", "rspec_src_bench", routing.CapabilitySourceVisionBenchmark},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := NewTestServer()
			seedRuntimeIngestSpec(t, srv, tc.spec, false)
			mappingID := "map_" + tc.spec
			seedCapabilityRow(t, srv, mappingID, routing.CapabilityVision, routing.CapabilityNo, routing.CapabilitySourceLlamaCppProps)
			counting := countingRowStore(srv)

			req, raw := ingestReq(t, capabilitiesBody(tc.spec,
				`{"verdicts":[{"name":"vision","verdict":"yes"},{"name":"tools","verdict":"yes"}],"source":"`+tc.source+`"}`))
			if err := srv.ingestTelemetrySample(ctx, "mock-host-qwen", req, raw); err != nil {
				t.Fatalf("ingest: %v", err)
			}
			if got := counting.upsertCalls.Load(); got != 0 {
				t.Fatalf("UpsertMappingCapabilities calls = %d for source %q, want 0 -- a source this gateway cannot attribute must write nothing, not borrow another probe's name", got, tc.source)
			}
			assertCapabilityRow(t, srv, mappingID, routing.CapabilityVision, routing.CapabilityNo, routing.CapabilitySourceLlamaCppProps)
			if row, ok := capabilityRow(t, srv, mappingID, routing.CapabilityTools); ok {
				t.Fatalf("a tools row exists (%+v), want none -- the whole pass is voided by an unattributable source", row)
			}
		})
	}
}

// withCapturedSlogAtTheDefaultLevel swaps the process-global slog default
// for a buffer at the level a real gateway runs at: INFO
// (OP_AI_GATEWAY_LOG_LEVEL's own default, config.Load). That is the whole
// point of not reusing withCapturedSlog beside it, which captures at Debug:
// a record emitted at Debug never reaches this buffer, exactly as it never
// reaches a default deployment's log, so a Debug-level capture would pass
// whatever level the code under test chose.
func withCapturedSlogAtTheDefaultLevel(t *testing.T) *logbuffer.Buffer {
	t.Helper()
	buf := logbuffer.NewBuffer(200, slog.LevelInfo)
	prev := slog.Default()
	slog.SetDefault(slog.New(buf.Handler(io.Discard)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// TestIngestWarnsWhenACapabilitySourceCannotBeAttributed pins that the drop
// its sibling above proves is also VISIBLE. The two assertions are one
// finding: an unattributable source voids every row of the pass, and the
// rows' absence is how this model spells "unknown" -- indistinguishable
// from a probe that never ran -- so a silent drop would leave a newer agent
// losing its whole capability report with nothing anywhere to point at.
//
// The capture runs at INFO, the level a real gateway runs at, which is what
// makes this a test rather than a restatement of the source: at Debug the
// line is filtered out before it reaches any log a default deployment
// keeps, so "it is logged" is only true at a level nobody runs.
func TestIngestWarnsWhenACapabilitySourceCannotBeAttributed(t *testing.T) {
	ctx := context.Background()
	buf := withCapturedSlogAtTheDefaultLevel(t)

	srv := NewTestServer()
	seedRuntimeIngestSpec(t, srv, "rspec_src_warn", false)
	counting := countingRowStore(srv)

	req, raw := ingestReq(t, capabilitiesBody("rspec_src_warn",
		`{"verdicts":[{"name":"vision","verdict":"yes"}],"source":"a_third_probe_this_gateway_never_heard_of"}`))
	if err := srv.ingestTelemetrySample(ctx, "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if got := counting.upsertCalls.Load(); got != 0 {
		t.Fatalf("UpsertMappingCapabilities calls = %d, want 0", got)
	}

	recs := buf.Snapshot()
	if !findLogRecord(recs, "WARN", "unrecognised source") {
		t.Fatalf("no WARN record naming an unrecognised source at the gateway's own default level (info); records = %+v -- a drop nobody can see is a fleet-wide capability blackout with no diagnostic", recs)
	}
	// The rejected source itself must be IN the record: "some sample was
	// dropped" does not tell an operator which agent build to look at.
	for _, r := range recs {
		if r.Level != "WARN" || !strings.Contains(r.Msg, "unrecognised source") {
			continue
		}
		if got, _ := r.Attrs["source"].(string); got != "a_third_probe_this_gateway_never_heard_of" {
			t.Fatalf("WARN record source attr = %q, want the source the agent actually reported", got)
		}
		if got, _ := r.Attrs["spec_id"].(string); got != "rspec_src_warn" {
			t.Fatalf("WARN record spec_id attr = %q, want rspec_src_warn", got)
		}
		return
	}
}

// TestIngestOllamaSourcedVerdictRespectsThePrecedenceRank proves the rank
// rule did not move when the second probe source arrived: ollama_api_show is
// a PROBE (rank 1 through capabilitySourceRank's default branch, with no
// case of its own), so it can never overwrite a verdict a human established
// (manual, 3) or one a real measurement established (vision_benchmark, 2).
//
// Unfalsifiable the same way as its llama_cpp_props siblings above: the
// sample carries a second, unmanaged verdict, so exactly one write must fire
// and must carry only that row. A test that merely counted zero writes could
// be satisfied by a write path that had stopped working -- and this one has
// a brand-new source string in it, which is exactly the kind of value a
// silent typo would strand.
func TestIngestOllamaSourcedVerdictRespectsThePrecedenceRank(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, spec, storedSource string
	}{
		{"manual is not overwritten", "rspec_ollama_vs_manual", routing.CapabilitySourceManual},
		{"vision_benchmark is not overwritten", "rspec_ollama_vs_bench", routing.CapabilitySourceVisionBenchmark},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := NewTestServer()
			seedRuntimeIngestSpec(t, srv, tc.spec, false)
			mappingID := "map_" + tc.spec
			seedCapabilityRow(t, srv, mappingID, routing.CapabilityVision, routing.CapabilityNo, tc.storedSource)
			counting := countingRowStore(srv)

			req, raw := ingestReq(t, capabilitiesBody(tc.spec,
				`{"verdicts":[{"name":"vision","verdict":"yes"},{"name":"tools","verdict":"yes"}],"source":"ollama_api_show"}`))
			if err := srv.ingestTelemetrySample(ctx, "mock-host-qwen", req, raw); err != nil {
				t.Fatalf("ingest: %v", err)
			}
			if got := counting.upsertCalls.Load(); got != 1 {
				t.Fatalf("UpsertMappingCapabilities calls = %d, want exactly 1 (the unmanaged tools verdict)", got)
			}
			sent := counting.lastSent()
			if len(sent) != 1 || sent[0].Capability != routing.CapabilityTools {
				t.Fatalf("the write carried %+v, want exactly the tools row -- a probe must never send a capability %s established", sent, tc.storedSource)
			}
			assertCapabilityRow(t, srv, mappingID, routing.CapabilityVision, routing.CapabilityNo, tc.storedSource)
			assertCapabilityRow(t, srv, mappingID, routing.CapabilityTools, routing.CapabilityYes, routing.CapabilitySourceOllamaAPIShow)
		})
	}
}

// TestIngestLiveProgressRowCarriesTheReportedSource pins that the reported
// source governs EVERY row one probe pass produces, the live-progress row
// included. The agent fills LiveProgressSupport and Capabilities from a
// SINGLE fetch of a SINGLE document (probeRuntimeChildProps), so one source
// describes both; hard-coding llama.cpp's name on the live-progress row
// while reading the reported one for its siblings would attribute two halves
// of one document to two different probes.
//
// The sample here is one today's agent cannot produce -- Ollama exposes no
// timings_per_token surface, so ProbeOllamaVerdicts always leaves
// LiveProgress "" -- and that is the point: if the wire ever does carry that
// combination, the row must say where it came from rather than borrowing a
// name the document never had.
func TestIngestLiveProgressRowCarriesTheReportedSource(t *testing.T) {
	srv := NewTestServer()
	seedRuntimeIngestSpec(t, srv, "rspec_lp_src", false)
	counting := countingRowStore(srv)

	body := `{"host":{"cpu_util_pct":1},"capabilities":{"features":["runtime_model_probe"]},` +
		`"runtimes":[{"spec_id":"rspec_lp_src","state":"running","live_progress_support":"supported",` +
		`"capabilities":{"verdicts":[{"name":"vision","verdict":"yes"}],"source":"ollama_api_show"}}]}`
	req, raw := ingestReq(t, body)
	if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if got := counting.upsertCalls.Load(); got != 1 {
		t.Fatalf("UpsertMappingCapabilities calls = %d, want exactly 1 (both rows ride one write)", got)
	}
	assertCapabilityRow(t, srv, "map_rspec_lp_src", routing.CapabilityLiveProgress, routing.CapabilityYes, routing.CapabilitySourceOllamaAPIShow)
	assertCapabilityRow(t, srv, "map_rspec_lp_src", routing.CapabilityVision, routing.CapabilityYes, routing.CapabilitySourceOllamaAPIShow)
}

// TestIngestCapabilitiesEmptyNeverClears proves an all-empty capabilities
// object -- detection ran and determined nothing -- leaves stored rows
// untouched and calls the writer zero times: unknown must never overwrite a
// stored verdict. The mapping is seeded to ALREADY hold "yes" through the
// store API directly (not through the code path under test), so this
// assertion cannot pass merely because both sides happen to be empty.
func TestIngestCapabilitiesEmptyNeverClears(t *testing.T) {
	srv := NewTestServer()
	seedRuntimeIngestSpec(t, srv, "rspec_caps_empty", false)
	ctx := context.Background()
	seedCapabilityRow(t, srv, "map_rspec_caps_empty", routing.CapabilityVision, routing.CapabilityYes, routing.CapabilitySourceLlamaCppProps)
	counting := countingRowStore(srv)

	body := capabilitiesBody("rspec_caps_empty", `{}`)
	req, raw := ingestReq(t, body)
	if err := srv.ingestTelemetrySample(ctx, "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if got := counting.upsertCalls.Load(); got != 0 {
		t.Fatalf("UpsertMappingCapabilities calls = %d for an all-empty capabilities object, want 0 -- unknown must never overwrite a stored verdict", got)
	}
	assertCapabilityRow(t, srv, "map_rspec_caps_empty", routing.CapabilityVision, routing.CapabilityYes, routing.CapabilitySourceLlamaCppProps)
}

// TestIngestCapabilitiesNilIsNotAClear proves a sample with NO "capabilities"
// key at all inside a runtime entry -- an agent build that predates
// capability detection -- is also zero writes, stored rows untouched.
// Nil and all-empty are DIFFERENT facts (see agentRuntimeSample.Capabilities'
// doc: an older agent vs. a detection pass that determined nothing), but
// both are "no write" -- this test and TestIngestCapabilitiesEmptyNeverClears
// together pin that BOTH reasons reach the same (correct) outcome, not that
// they are indistinguishable internally.
func TestIngestCapabilitiesNilIsNotAClear(t *testing.T) {
	srv := NewTestServer()
	seedRuntimeIngestSpec(t, srv, "rspec_caps_nil", false)
	ctx := context.Background()
	seedCapabilityRow(t, srv, "map_rspec_caps_nil", routing.CapabilityVision, routing.CapabilityYes, routing.CapabilitySourceLlamaCppProps)
	counting := countingRowStore(srv)

	// capsJSON == "" omits the "capabilities" key entirely from the runtime
	// object -- rt.Capabilities decodes to a nil pointer, not a zero struct.
	body := capabilitiesBody("rspec_caps_nil", "")
	req, raw := ingestReq(t, body)
	if err := srv.ingestTelemetrySample(ctx, "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if got := counting.upsertCalls.Load(); got != 0 {
		t.Fatalf("UpsertMappingCapabilities calls = %d for a nil capabilities (no key at all), want 0", got)
	}
	assertCapabilityRow(t, srv, "map_rspec_caps_nil", routing.CapabilityVision, routing.CapabilityYes, routing.CapabilitySourceLlamaCppProps)
}

// TestIngestCapabilitiesWithoutFeatureNeverWrites pins the runtime_model_probe
// gate on this write-back, mirroring
// TestIngestTelemetrySampleLiveProgressWriteBackGuardedByCapability: the
// verdicts ride the same per-runtime probe pass that produces context_size,
// so they share that pass's trust boundary. An agent that has never declared
// runtime_model_probe must never have a mapping's stored capabilities touched
// from this path -- even though the per-runtime "capabilities" object itself
// is present and definitive.
func TestIngestCapabilitiesWithoutFeatureNeverWrites(t *testing.T) {
	srv := NewTestServer()
	seedRuntimeIngestSpec(t, srv, "rspec_caps_nocap", false)
	ctx := context.Background()
	counting := countingRowStore(srv)

	// No top-level "capabilities" object at all -- an agent that never
	// declared runtime_model_probe, exactly like an older agent build.
	body := `{"host":{"cpu_util_pct":1},"runtimes":[{"spec_id":"rspec_caps_nocap","state":"running","capabilities":{"verdicts":[{"name":"vision","verdict":"yes"},{"name":"tools","verdict":"no"}]}}]}`
	req, raw := ingestReq(t, body)
	if err := srv.ingestTelemetrySample(ctx, "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if got := counting.upsertCalls.Load(); got != 0 {
		t.Fatalf("UpsertMappingCapabilities calls = %d, want 0 (the agent did not declare runtime_model_probe)", got)
	}
	if _, ok := capabilityRow(t, srv, "map_rspec_caps_nocap", routing.CapabilityVision); ok {
		t.Fatal("a vision row exists, want none -- an agent that never declared runtime_model_probe must not reach this write path")
	}
}

// TestIngestCapabilitiesCrossServerRejected is the capability write-back's
// own cross-tenant guard, mirroring
// TestIngestTelemetrySampleLiveProgressWriteBackRejectsCrossServerSpec:
// spec_id is an agent-supplied body field with no other verification
// anywhere on this path, and the only thing binding a sample to a server is
// the token-derived serverID. The sample carries TWO runtimes -- the foreign
// spec plus a control spec the reporting server does own -- so the write
// count must be exactly 1 (the control's); removing the ownership check
// would turn it into 2, and the foreign mapping is seeded with a
// DISTINGUISHABLE stored verdict ("no") that the forged sample would flip to
// "yes".
func TestIngestCapabilitiesCrossServerRejected(t *testing.T) {
	srv := NewTestServer()
	ctx := context.Background()
	now := time.Now().UTC()
	const otherServerID = "mock-host-other-tenant-caps"
	if err := srv.Routes.CreateAIServer(ctx, routing.AIServer{
		ID: otherServerID, Name: otherServerID, Domain: otherServerID + ".example.test",
		Provider: routing.ProviderMock, Endpoint: "mock://" + otherServerID,
		Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create other server: %v", err)
	}
	// rspec_cross_caps belongs to otherServerID; rspec_own_caps to the
	// reporting one.
	seedRuntimeIngestSpecForServer(t, srv, otherServerID, "rspec_cross_caps", false)
	seedRuntimeIngestSpec(t, srv, "rspec_own_caps", false)
	seedCapabilityRow(t, srv, "map_rspec_cross_caps", routing.CapabilityVision, routing.CapabilityNo, routing.CapabilitySourceLlamaCppProps)

	counting := countingRowStore(srv)

	buf, restore := withCapturedSlog(t)
	defer restore()

	body := `{"host":{"cpu_util_pct":1},"capabilities":{"features":["runtime_model_probe"]},"runtimes":[` +
		`{"spec_id":"rspec_cross_caps","state":"running","capabilities":{"verdicts":[{"name":"vision","verdict":"yes"}]}},` +
		`{"spec_id":"rspec_own_caps","state":"running","capabilities":{"verdicts":[{"name":"vision","verdict":"yes"}]}}]}`
	req, raw := ingestReq(t, body)
	if err := srv.ingestTelemetrySample(ctx, "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest must succeed (best-effort write-back) even when the sample names another server's spec_id: %v", err)
	}

	if got := counting.upsertCalls.Load(); got != 1 {
		t.Fatalf("UpsertMappingCapabilities calls = %d, want exactly 1 (only the control spec this server owns)", got)
	}
	assertCapabilityRow(t, srv, "map_rspec_cross_caps", routing.CapabilityVision, routing.CapabilityNo, routing.CapabilitySourceLlamaCppProps)
	assertCapabilityRow(t, srv, "map_rspec_own_caps", routing.CapabilityVision, routing.CapabilityYes, routing.CapabilitySourceLlamaCppProps)
	if !findLogRecord(buf.Snapshot(), "WARN", "spec belongs to a different server") {
		t.Fatal("a cross-server naming attempt must log a Warn, not a Debug -- an agent naming another server's resources is an audit signal, not a merely stale id")
	}
}

// TestAgentRuntimeCapabilitiesSampleDecode is the gateway-side decode half of
// #49 sub-project 2, task 2: the wire shape is a keyed Verdicts list, and the
// vocabulary is OPEN -- a capability name this binary has never heard of
// must decode and be CARRIED, not dropped, the same forward-compatibility
// rule parseAgentCapabilities already documents for the declared-feature
// list. Also pins the nil-vs-non-nil-empty distinction at the decode layer:
// no "capabilities" key at all decodes to a nil pointer (an agent that
// predates capability detection), while an explicit empty verdicts list
// decodes to a non-nil pointer with an empty (non-nil) Verdicts (detection
// ran, determined nothing).
func TestAgentRuntimeCapabilitiesSampleDecode(t *testing.T) {
	t.Run("known and unknown verdicts both decode and are carried", func(t *testing.T) {
		body := `{"host":{"cpu_util_pct":1},"runtimes":[{"spec_id":"rspec_decode","state":"running",` +
			`"capabilities":{"verdicts":[{"name":"vision","verdict":"yes"},{"name":"future_modality","verdict":"no"}]}}]}`
		req, _ := ingestReq(t, body)
		if len(req.Runtimes) != 1 {
			t.Fatalf("runtimes = %+v, want 1", req.Runtimes)
		}
		caps := req.Runtimes[0].Capabilities
		if caps == nil {
			t.Fatal("Capabilities = nil, want a non-nil pointer")
		}
		want := []agentRuntimeCapabilityVerdict{
			{Name: "vision", Verdict: "yes"},
			{Name: "future_modality", Verdict: "no"},
		}
		if !reflect.DeepEqual(caps.Verdicts, want) {
			t.Errorf("Verdicts = %+v, want %+v -- an unknown capability name must be CARRIED, not dropped", caps.Verdicts, want)
		}
	})

	t.Run("no capabilities key decodes to nil", func(t *testing.T) {
		body := `{"host":{"cpu_util_pct":1},"runtimes":[{"spec_id":"rspec_decode_nil","state":"running"}]}`
		req, _ := ingestReq(t, body)
		if len(req.Runtimes) != 1 {
			t.Fatalf("runtimes = %+v, want 1", req.Runtimes)
		}
		if got := req.Runtimes[0].Capabilities; got != nil {
			t.Errorf("Capabilities = %+v, want nil (no key at all -- an agent that predates capability detection)", got)
		}
	})

	t.Run("empty verdicts list decodes to non-nil-empty", func(t *testing.T) {
		body := `{"host":{"cpu_util_pct":1},"runtimes":[{"spec_id":"rspec_decode_empty","state":"running","capabilities":{"verdicts":[]}}]}`
		req, _ := ingestReq(t, body)
		if len(req.Runtimes) != 1 {
			t.Fatalf("runtimes = %+v, want 1", req.Runtimes)
		}
		caps := req.Runtimes[0].Capabilities
		if caps == nil {
			t.Fatal("Capabilities = nil, want a non-nil pointer -- detection ran and determined nothing, distinct from the no-key case above")
		}
		if len(caps.Verdicts) != 0 {
			t.Errorf("Verdicts = %+v, want empty", caps.Verdicts)
		}
	})
}
