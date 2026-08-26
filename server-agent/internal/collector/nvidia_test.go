// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestParseNvidiaCSV(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "nvidia-smi.csv"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	gpus, err := parseNvidiaCSV(data)
	if err != nil {
		t.Fatalf("parseNvidiaCSV: %v", err)
	}
	if len(gpus) != 2 {
		t.Fatalf("want 2 GPUs, got %d", len(gpus))
	}

	g0 := gpus[0]
	if g0.Index != 0 {
		t.Errorf("gpu[0].Index = %d, want 0", g0.Index)
	}
	if g0.Name != "NVIDIA GeForce RTX 4090" {
		t.Errorf("gpu[0].Name = %q, want %q", g0.Name, "NVIDIA GeForce RTX 4090")
	}
	if g0.UUID != "GPU-aaaa-0" {
		t.Errorf("gpu[0].UUID = %q, want %q", g0.UUID, "GPU-aaaa-0")
	}
	if g0.UtilPct != 88 {
		t.Errorf("gpu[0].UtilPct = %v, want 88", g0.UtilPct)
	}
	if g0.MemUsedBytes != 12000*1024*1024 {
		t.Errorf("gpu[0].MemUsedBytes = %d, want %d", g0.MemUsedBytes, 12000*1024*1024)
	}
	if g0.MemTotalBytes != 24564*1024*1024 {
		t.Errorf("gpu[0].MemTotalBytes = %d, want %d", g0.MemTotalBytes, 24564*1024*1024)
	}
	if g0.TempC != 71 {
		t.Errorf("gpu[0].TempC = %d, want 71", g0.TempC)
	}
	if g0.PowerW != 320.5 {
		t.Errorf("gpu[0].PowerW = %v, want 320.5", g0.PowerW)
	}
	if g0.FanPct != 60 {
		t.Errorf("gpu[0].FanPct = %v, want 60", g0.FanPct)
	}
	if g0.VRAMTempC != 0 {
		t.Errorf("gpu[0].VRAMTempC = %d, want 0", g0.VRAMTempC)
	}

	if gpus[1].FanPct != 0 {
		t.Errorf("gpu[1].FanPct = %v, want 0 ([N/A] → 0)", gpus[1].FanPct)
	}
}

func TestParseNvidiaCSVEmpty(t *testing.T) {
	gpus, err := parseNvidiaCSV(nil)
	if err != nil {
		t.Fatalf("empty input: unexpected error %v", err)
	}
	if len(gpus) != 0 {
		t.Fatalf("empty input: want 0 GPUs, got %d", len(gpus))
	}

	// A malformed short row must be skipped, not panic.
	short := []byte("0, only, three, fields\n")
	gpus, err = parseNvidiaCSV(short)
	if err != nil {
		t.Fatalf("short row: unexpected error %v", err)
	}
	if len(gpus) != 0 {
		t.Fatalf("short row: want 0 GPUs, got %d", len(gpus))
	}
}

func TestParseNvidiaCSVCapturesDriverVersion(t *testing.T) {
	// 10 fields: the 9 metric columns + driver_version appended.
	data := []byte("0, RTX 4090, GPU-uuid-0, 55, 8000, 24000, 61, 300, 45, 550.54.15\n")
	gpus, err := parseNvidiaCSV(data)
	if err != nil || len(gpus) != 1 {
		t.Fatalf("parse = %v err=%v", gpus, err)
	}
	if gpus[0].DriverVersion != "550.54.15" {
		t.Fatalf("driver_version = %q, want 550.54.15", gpus[0].DriverVersion)
	}
	if gpus[0].Name != "RTX 4090" {
		t.Fatalf("name = %q", gpus[0].Name)
	}
}

func TestParseNvidiaCSVBackCompatNineFields(t *testing.T) {
	// A 9-field row (no driver_version) must still parse (driver stays empty).
	data := []byte("0, RTX 4090, GPU-uuid-0, 55, 8000, 24000, 61, 300, 45\n")
	gpus, err := parseNvidiaCSV(data)
	if err != nil || len(gpus) != 1 || gpus[0].DriverVersion != "" {
		t.Fatalf("parse = %v err=%v", gpus, err)
	}
}

// canned nvidia-smi output fixtures for the compute-apps measurer (design
// doc §5): two GPUs, three compute-app rows (two processes on GPU 0, one on
// GPU 1), including sentinel/whitespace noise parseNvidiaCSV already
// tolerates for the telemetry query.
const (
	canned2GPUIndexCSV   = "0, GPU-aaaa-0\n1, GPU-bbbb-1\n"
	cannedComputeAppsCSV = "12345, GPU-aaaa-0, 21234\n" +
		"12345, GPU-bbbb-1, 8000\n" +
		"99999, GPU-aaaa-0, 5000\n"
)

func TestParseNvidiaGPUIndexCSV(t *testing.T) {
	got := parseNvidiaGPUIndexCSV([]byte(canned2GPUIndexCSV))
	want := map[string]int{"GPU-aaaa-0": 0, "GPU-bbbb-1": 1}
	if len(got) != len(want) {
		t.Fatalf("got = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("got[%q] = %d, want %d", k, got[k], v)
		}
	}
}

func TestParseNvidiaGPUIndexCSVSkipsShortRows(t *testing.T) {
	got := parseNvidiaGPUIndexCSV([]byte("just-one-field\n\n0, GPU-aaaa-0\n"))
	if len(got) != 1 || got["GPU-aaaa-0"] != 0 {
		t.Fatalf("got = %v, want {GPU-aaaa-0:0}", got)
	}
}

func TestParseNvidiaComputeAppsCSV(t *testing.T) {
	rows := parseNvidiaComputeAppsCSV([]byte(cannedComputeAppsCSV))
	if len(rows) != 3 {
		t.Fatalf("len(rows) = %d, want 3", len(rows))
	}
	if rows[0] != (nvidiaComputeAppRow{PID: 12345, GPUUUID: "GPU-aaaa-0", UsedMemoryMB: 21234}) {
		t.Errorf("rows[0] = %+v", rows[0])
	}
	if rows[1] != (nvidiaComputeAppRow{PID: 12345, GPUUUID: "GPU-bbbb-1", UsedMemoryMB: 8000}) {
		t.Errorf("rows[1] = %+v", rows[1])
	}
	if rows[2] != (nvidiaComputeAppRow{PID: 99999, GPUUUID: "GPU-aaaa-0", UsedMemoryMB: 5000}) {
		t.Errorf("rows[2] = %+v", rows[2])
	}
}

func TestParseNvidiaComputeAppsCSVEmptyAndShortRows(t *testing.T) {
	rows := parseNvidiaComputeAppsCSV(nil)
	if len(rows) != 0 {
		t.Fatalf("nil input: want 0 rows, got %d", len(rows))
	}
	rows = parseNvidiaComputeAppsCSV([]byte("12345, GPU-aaaa-0\n"))
	if len(rows) != 0 {
		t.Fatalf("short row: want 0 rows, got %d", len(rows))
	}
}

// TestMeasureNvidiaComputeAppsAttributesByPIDAndMapsUUIDToIndex proves the
// end-to-end shape SetMeasurer needs: only the requested PIDs are reported,
// and each row's GPU UUID resolves to the matching index -- exercised via
// the pure parse functions rather than a real nvidia-smi (which the test
// host does not have), matching how the manager's buildSnapshot actually
// consumes this measurer's return value.
// TestAttributeComputeAppsFiltersByPIDAndMapsUUIDToIndex calls the actual
// production function (fix round 1, I4) instead of re-implementing its
// filter/map loop inline: the ORIGINAL version of this test
// (TestMeasureNvidiaComputeAppsAttributesByPIDAndMapsUUIDToIndex) built its
// own copy of the loop and never called attributeComputeApps or measure at
// all, so a bug in the real implementation -- wrong filter direction, a
// dropped GPU index, wrong map nesting -- would have left it green. This is
// the seventh instance of that class caught on this branch; the fix here is
// structural (call the extracted pure function), not a patched assertion.
func TestAttributeComputeAppsFiltersByPIDAndMapsUUIDToIndex(t *testing.T) {
	uuidToIndex := parseNvidiaGPUIndexCSV([]byte(canned2GPUIndexCSV))
	rows := parseNvidiaComputeAppsCSV([]byte(cannedComputeAppsCSV))

	// The manager only cares about ITS OWN child (12345), not pid 99999.
	out := attributeComputeApps(rows, uuidToIndex, []int{12345})

	if len(out) != 1 {
		t.Fatalf("out = %v, want exactly pid 12345", out)
	}
	if out[12345][0] != 21234 || out[12345][1] != 8000 {
		t.Errorf("out[12345] = %v, want {0:21234 1:8000}", out[12345])
	}
	if _, ok := out[99999]; ok {
		t.Errorf("out contains pid 99999, which was not in the requested pid set")
	}
}

// TestAttributeComputeAppsUnknownUUIDSkippedNotGuessed proves a row whose
// GPU UUID has no entry in uuidToIndex is skipped rather than attributed to
// a wrong/zero index.
func TestAttributeComputeAppsUnknownUUIDSkippedNotGuessed(t *testing.T) {
	rows := parseNvidiaComputeAppsCSV([]byte(cannedComputeAppsCSV))
	// Only GPU-bbbb-1 is known here; GPU-aaaa-0 (pid 12345's other row) is
	// deliberately absent from uuidToIndex.
	out := attributeComputeApps(rows, map[string]int{"GPU-bbbb-1": 1}, []int{12345, 99999})

	if len(out) != 1 {
		t.Fatalf("out = %v, want exactly pid 12345", out)
	}
	if _, ok := out[12345][0]; ok {
		t.Errorf("out[12345] = %v, want no entry for index 0 (its GPU UUID was not in uuidToIndex)", out[12345])
	}
	if out[12345][1] != 8000 {
		t.Errorf("out[12345][1] = %v, want 8000", out[12345][1])
	}
}

// TestAttributeComputeAppsNilOnNoMatches proves an empty/nil result (not an
// empty-but-non-nil map) when nothing in rows matches pids -- buildSnapshot
// (manager.go) treats a nil measurement map as "nothing measured", falling
// back to static estimates.
func TestAttributeComputeAppsNilOnNoMatches(t *testing.T) {
	rows := parseNvidiaComputeAppsCSV([]byte(cannedComputeAppsCSV))
	if out := attributeComputeApps(rows, map[string]int{"GPU-aaaa-0": 0}, []int{424242}); out != nil {
		t.Fatalf("out = %v, want nil", out)
	}
}

// TestHasUnresolvedUUID pins M1's cache-invalidation trigger: a miss on a
// WANTED pid's GPU UUID reports true (refetch the mapping); an unwanted
// pid's unknown UUID must NOT trigger a refetch; a fully-resolved wanted
// set reports false.
func TestHasUnresolvedUUID(t *testing.T) {
	rows := parseNvidiaComputeAppsCSV([]byte(cannedComputeAppsCSV))

	full := map[string]int{"GPU-aaaa-0": 0, "GPU-bbbb-1": 1}
	if hasUnresolvedUUID(rows, map[int]bool{12345: true}, full) {
		t.Error("hasUnresolvedUUID with a fully-resolved wanted set = true, want false")
	}

	partial := map[string]int{"GPU-bbbb-1": 1} // GPU-aaaa-0 missing
	if !hasUnresolvedUUID(rows, map[int]bool{12345: true}, partial) {
		t.Error("hasUnresolvedUUID with a wanted pid's UUID missing = false, want true")
	}

	// pid 99999 uses GPU-aaaa-0, which IS missing from `partial` -- but
	// 99999 is not in the wanted set, so this must NOT report a miss.
	if hasUnresolvedUUID(rows, map[int]bool{12345: false, 99999: false}, partial) {
		t.Error("hasUnresolvedUUID must ignore an unresolved UUID belonging to a PID that is not wanted")
	}
}

func TestNewNvidiaComputeAppsNilWithoutBinary(t *testing.T) {
	// This test host is not guaranteed to have nvidia-smi installed (in
	// fact almost certainly does not, in CI); NewNvidiaComputeApps must
	// degrade to nil rather than returning a measurer that will fail every
	// call -- the design doc's own "measurement is a hardware capability,
	// not a negotiated feature" contract (§5).
	if _, err := exec.LookPath("nvidia-smi"); err == nil {
		t.Skip("nvidia-smi is present on this host; nothing to prove here")
	}
	if f := NewNvidiaComputeApps(); f != nil {
		t.Fatal("NewNvidiaComputeApps() = non-nil without nvidia-smi on PATH, want nil")
	}
}
