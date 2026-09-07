// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"op-ai-gateway/internal/routing"
	"strings"
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
