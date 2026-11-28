// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
	"op-ai-gateway/internal/routing"
	"strings"
	"testing"
	"time"
)

// setMixedSpecGPUs replaces specID's GPU rows with rows for the given keys
// (positions in order, estimate 1000 each) and stores measured[i] > 0 as the
// row's measured value.
func setMixedSpecGPUs(t *testing.T, srv *Server, specID string, keys []routing.GPUKey, measured []int) {
	t.Helper()
	ctx := context.Background()
	rows := make([]routing.RuntimeSpecGPU, 0, len(keys))
	for i, k := range keys {
		rows = append(rows, routing.RuntimeSpecGPU{SpecID: specID, GPUVendor: k.Vendor, GPUIndex: k.Index, Position: i, VRAMEstimateMB: 1000})
	}
	if err := srv.Routes.SetRuntimeSpecGPUs(ctx, specID, rows); err != nil {
		t.Fatalf("set spec gpus: %v", err)
	}
	for i, k := range keys {
		if i < len(measured) && measured[i] > 0 {
			if err := srv.Routes.UpdateRuntimeSpecGPUMeasured(ctx, specID, k, measured[i]); err != nil {
				t.Fatalf("seed measured %v: %v", k, err)
			}
		}
	}
}

// measuredByKey reads specID's stored measured VRAM per GPU key.
func measuredByKey(t *testing.T, srv *Server, specID string) map[routing.GPUKey]int {
	t.Helper()
	gpus, err := srv.Routes.RuntimeSpecGPUs(context.Background(), specID)
	if err != nil {
		t.Fatalf("RuntimeSpecGPUs: %v", err)
	}
	out := make(map[routing.GPUKey]int, len(gpus))
	for _, g := range gpus {
		out[g.Key()] = g.VRAMMeasuredMB
	}
	return out
}

func ingestRuntimeGPUs(t *testing.T, srv *Server, specID, gpus string) {
	t.Helper()
	body := `{"host":{"cpu_util_pct":1},"runtimes":[{"spec_id":"` + specID + `","state":"running","gpus":[` + gpus + `]}]}`
	req, raw := ingestReq(t, body)
	if err := srv.ingestTelemetrySample(context.Background(), "mock-host-qwen", req, raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}
}

var (
	keyNVIDIA0 = routing.GPUKey{Vendor: routing.GPUVendorNVIDIA, Index: 0}
	keyAMD0    = routing.GPUKey{Vendor: routing.GPUVendorAMD, Index: 0}
	keyLegacy0 = routing.GPUKey{Vendor: "", Index: 0}
)

// A mixed host reports index 0 once per vendor: each spec row gets its own
// measurement (keyed by the index alone, the second overwrote the first).
func TestWriteBackRuntimeVRAMPerVendor(t *testing.T) {
	srv := NewTestServer()
	seedRuntimeIngestSpec(t, srv, "rspec_mixed")
	setMixedSpecGPUs(t, srv, "rspec_mixed", []routing.GPUKey{keyNVIDIA0, keyAMD0}, nil)

	ingestRuntimeGPUs(t, srv, "rspec_mixed",
		`{"index":0,"vendor":"nvidia","vram_measured_mb":7874},{"index":0,"vendor":"amd","vram_measured_mb":3323}`)

	got := measuredByKey(t, srv, "rspec_mixed")
	if got[keyNVIDIA0] != 7874 || got[keyAMD0] != 3323 {
		t.Fatalf("measured = %v, want NVIDIA 0: 7874, AMD 0: 3323", got)
	}
}

// A downgraded agent (no gpu_vendor_identity) reports no vendor: its frame
// speaks for the server's stored legacy vendor, the vendor the backfill gave
// the rows. An exact-key write of ("", 0) would find no row at all.
func TestWriteBackVendorlessFrameUsesLegacyVendor(t *testing.T) {
	srv := NewTestServer()
	ctx := context.Background()
	seedRuntimeIngestSpec(t, srv, "rspec_downgraded")
	if _, err := srv.Routes.BackfillServerGPUVendor(ctx, "mock-host-qwen", routing.GPUVendorNVIDIA, true); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	setMixedSpecGPUs(t, srv, "rspec_downgraded", []routing.GPUKey{keyNVIDIA0, keyAMD0}, []int{0, 1111})

	ingestRuntimeGPUs(t, srv, "rspec_downgraded", `{"index":0,"vram_measured_mb":7874}`)

	got := measuredByKey(t, srv, "rspec_downgraded")
	if got[keyNVIDIA0] != 7874 || got[keyAMD0] != 1111 {
		t.Fatalf("measured = %v, want NVIDIA 0: 7874 and AMD 0 kept at 1111", got)
	}
}

// Without a stored legacy vendor (the server was never backfilled) a
// vendor-less frame keeps the key ("", index), the key its rows still carry.
func TestWriteBackVendorlessFrameWithoutLegacyVendor(t *testing.T) {
	srv := NewTestServer()
	seedRuntimeIngestSpec(t, srv, "rspec_legacy")

	ingestRuntimeGPUs(t, srv, "rspec_legacy", `{"index":0,"vram_measured_mb":4321}`)

	if got := measuredByKey(t, srv, "rspec_legacy"); got[keyLegacy0] != 4321 {
		t.Fatalf("measured = %v, want GPU 0: 4321", got)
	}
}

// An empty-vendor row saved after the backfill (an unflagged agent's portal
// save) stands for the legacy vendor too: a frame naming the legacy vendor --
// a flagged agent resolves the row's empty vendor before it runs it -- or no
// vendor at all lands on it.
func TestWriteBackLegacyVendorFrameReachesEmptyVendorRow(t *testing.T) {
	srv := NewTestServer()
	ctx := context.Background()
	seedRuntimeIngestSpec(t, srv, "rspec_saved_later")
	if _, err := srv.Routes.BackfillServerGPUVendor(ctx, "mock-host-qwen", routing.GPUVendorNVIDIA, false); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	setMixedSpecGPUs(t, srv, "rspec_saved_later", []routing.GPUKey{keyLegacy0, keyAMD0}, nil)

	ingestRuntimeGPUs(t, srv, "rspec_saved_later",
		`{"index":0,"vendor":"nvidia","vram_measured_mb":5000},{"index":0,"vendor":"amd","vram_measured_mb":2000}`)
	if got := measuredByKey(t, srv, "rspec_saved_later"); got[keyLegacy0] != 5000 || got[keyAMD0] != 2000 {
		t.Fatalf("after a vendor frame: measured = %v, want GPU 0: 5000, AMD 0: 2000", got)
	}

	ingestRuntimeGPUs(t, srv, "rspec_saved_later", `{"index":0,"vram_measured_mb":6000}`)
	if got := measuredByKey(t, srv, "rspec_saved_later"); got[keyLegacy0] != 6000 || got[keyAMD0] != 2000 {
		t.Fatalf("after a vendor-less frame: measured = %v, want GPU 0: 6000, AMD 0: 2000", got)
	}
}

// Two instances of one replicable spec fold per (vendor, index): the largest
// per key, never across vendors.
func TestFoldInstanceRowsKeysByVendor(t *testing.T) {
	rows := []RuntimeStatusDTO{
		{SpecID: "S", Instance: 0, State: "running", GPUs: []RuntimeGPUStatusDTO{{Index: 0, Vendor: "nvidia", VRAMMeasuredMB: 100}, {Index: 0, Vendor: "amd", VRAMMeasuredMB: 900}}},
		{SpecID: "S", Instance: 1, State: "running", GPUs: []RuntimeGPUStatusDTO{{Index: 0, VRAMMeasuredMB: 300}}},
	}
	got := foldInstanceRows(rows, routing.GPUVendorNVIDIA)["S"].MaxGPU
	if len(got) != 2 || got[keyNVIDIA0] != 300 || got[keyAMD0] != 900 {
		t.Fatalf("MaxGPU = %v, want NVIDIA 0: 300 (the vendor-less row resolved to the legacy vendor), AMD 0: 900", got)
	}
}

func TestVRAMGPUByKeyKeepsCollidingIndices(t *testing.T) {
	sample := routing.TelemetrySample{GPUs: []routing.GPUSample{
		{Index: 0, Vendor: "nvidia", MemUsedBytes: 1},
		{Index: 0, Vendor: "amd", MemUsedBytes: 2},
		{Index: 0, Vendor: "amd", MemUsedBytes: 3}, // a duplicate key keeps the first row
	}}
	got := vramGPUByKey(sample)
	if len(got) != 2 || got[keyNVIDIA0].MemUsedBytes != 1 || got[keyAMD0].MemUsedBytes != 2 {
		t.Fatalf("vramGPUByKey = %+v, want NVIDIA 0 and AMD 0 apart", got)
	}
}

func TestVRAMReportItemsCarryVendor(t *testing.T) {
	amd1 := routing.GPUKey{Vendor: routing.GPUVendorAMD, Index: 1}
	before := routing.TelemetrySample{GPUs: []routing.GPUSample{
		{Index: 1, Vendor: "nvidia", MemUsedBytes: 0},
		{Index: 1, Vendor: "amd", MemUsedBytes: 1 * vramBytesPerMB},
	}}
	after := routing.TelemetrySample{GPUs: []routing.GPUSample{
		{Index: 1, Vendor: "nvidia", MemUsedBytes: 9000 * vramBytesPerMB},
		{Index: 1, Vendor: "amd", MemUsedBytes: 2001 * vramBytesPerMB},
	}}
	items := vramReportItems([]routing.GPUKey{amd1}, true, vramRunPlanned{}, before, after, map[routing.GPUKey]int{amd1: 1990})
	if len(items) != 1 {
		t.Fatalf("items = %+v", items)
	}
	if it := items[0]; it.Vendor != "amd" || it.Index != 1 || it.DeltaMB != 2000 || it.MeasuredMB != 1990 || it.BaselineUsedMB != 1 {
		t.Fatalf("item = %+v, want AMD 1 with its own delta 2000 and measured 1990", it)
	}
}

// The spec's rows and the samples speak of one card the same way: an empty
// vendor on either side stands for the legacy vendor.
func TestVRAMWatchedKeysResolveLegacyVendor(t *testing.T) {
	sample := vramResolveSampleVendors(routing.TelemetrySample{GPUs: []routing.GPUSample{
		{Index: 0}, {Index: 0, Vendor: "amd"},
	}}, routing.GPUVendorNVIDIA)
	rows := vramResolveSpecGPUVendors([]routing.RuntimeSpecGPU{{GPUIndex: 0}, {GPUVendor: "amd", GPUIndex: 0}}, routing.GPUVendorNVIDIA)
	keys, attributable := vramWatchedKeys(rows, sample)
	if !attributable || len(keys) != 2 || keys[0] != keyAMD0 || keys[1] != keyNVIDIA0 {
		t.Fatalf("vramWatchedKeys = %v, %v, want [AMD 0, NVIDIA 0] attributable", keys, attributable)
	}
	if byKey := vramGPUByKey(sample); len(byKey) != 2 {
		t.Fatalf("resolved sample = %+v, want NVIDIA 0 and AMD 0", byKey)
	}
}

func TestVRAMMeasuredFromFrameKeysByVendor(t *testing.T) {
	frame := []RuntimeStatusDTO{{SpecID: "S", MeasuredAt: time.Now(), GPUs: []RuntimeGPUStatusDTO{
		{Index: 0, VRAMMeasuredMB: 700}, {Index: 0, Vendor: "amd", VRAMMeasuredMB: 300},
	}}}
	got := vramMeasuredFromFrame(frame, "S", routing.GPUVendorNVIDIA)
	if len(got) != 2 || got[keyNVIDIA0] != 700 || got[keyAMD0] != 300 {
		t.Fatalf("vramMeasuredFromFrame = %v, want NVIDIA 0: 700, AMD 0: 300", got)
	}
}

// A cluster member's GPU rows on a mixed host are raised per (vendor, index);
// a vendor-less frame and a vendor-less member row both stand for the legacy
// vendor.
func TestClusterUsageRaiseMeasuredPerVendor(t *testing.T) {
	cur := routing.ClusterMember{}
	gpus := []routing.ClusterMemberGPU{
		{GPUVendor: "nvidia", GPUIndex: 0},
		{GPUVendor: "amd", GPUIndex: 0},
		{GPUVendor: "", GPUIndex: 1},
	}
	row := RuntimeStatusDTO{GPUs: []RuntimeGPUStatusDTO{
		{Index: 0, Vendor: "nvidia", VRAMMeasuredMB: 7000},
		{Index: 0, Vendor: "amd", VRAMMeasuredMB: 3000},
		{Index: 1, VRAMMeasuredMB: 500},
	}}
	vram := measuredVRAM(row, routing.GPUVendorNVIDIA)
	peak, stored := raiseMeasured(&cur, gpus, routing.GPUVendorNVIDIA, 0, vram)
	if !peak || !stored {
		t.Fatalf("raiseMeasured = %v, %v, want both raised", peak, stored)
	}
	want := []int{7000, 3000, 500}
	for i, g := range gpus {
		if g.VRAMMeasuredMB != want[i] || g.RunPeakVRAMMB != want[i] {
			t.Errorf("gpu %d (%s %d) = measured %d, peak %d, want %d", i, g.GPUVendor, g.GPUIndex, g.VRAMMeasuredMB, g.RunPeakVRAMMB, want[i])
		}
	}
}

// legacyKeys is the GPU keys of a host whose GPUs carry no vendor.
func legacyKeys(indexes ...int) []routing.GPUKey {
	if indexes == nil {
		return nil
	}
	out := make([]routing.GPUKey, 0, len(indexes))
	for _, i := range indexes {
		out = append(out, routing.GPUKey{Index: i})
	}
	return out
}

// The declared-GPU precondition reads a vendor-less spec row as the legacy
// vendor's card, and names a missing card by its key.
func TestVRAMDeclaredGPUMissingResolvesLegacyVendor(t *testing.T) {
	srv := NewTestServer()
	ctx := context.Background()
	seedRuntimeIngestSpec(t, srv, "rspec_declared") // one row ("", 0)
	nvidiaHost := routing.TelemetrySample{GPUs: []routing.GPUSample{{Index: 0, Vendor: "nvidia"}}}
	if err := srv.vramDeclaredGPUMissing(ctx, "rspec_declared", routing.GPUVendorNVIDIA, nvidiaHost); err != nil {
		t.Fatalf("a vendor-less row on an NVIDIA-legacy host with NVIDIA 0 present: %v", err)
	}
	setMixedSpecGPUs(t, srv, "rspec_declared", []routing.GPUKey{keyLegacy0, {Vendor: routing.GPUVendorAMD, Index: 1}}, nil)
	err := srv.vramDeclaredGPUMissing(ctx, "rspec_declared", routing.GPUVendorNVIDIA, nvidiaHost)
	var refusal *vramRefusal
	if !errors.As(err, &refusal) || refusal.code != codeBenchmarkVRAMDeclaredGPUMissing || !strings.Contains(refusal.msg, "AMD 1") {
		t.Fatalf("err = %v, want the declared-GPU refusal naming AMD 1", err)
	}
}

// Resolving a sample's vendors copies its GPUs: the sample is shared with
// every other reader of the live ring.
func TestVRAMResolveSampleVendorsDoesNotModifyTheSample(t *testing.T) {
	sample := routing.TelemetrySample{GPUs: []routing.GPUSample{{Index: 0}}}
	resolved := vramResolveSampleVendors(sample, routing.GPUVendorAMD)
	if sample.GPUs[0].Vendor != "" || resolved.GPUs[0].Vendor != routing.GPUVendorAMD {
		t.Fatalf("sample %+v, resolved %+v", sample.GPUs, resolved.GPUs)
	}
}
