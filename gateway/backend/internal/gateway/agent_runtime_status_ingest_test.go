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
