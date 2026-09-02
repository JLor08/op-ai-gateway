// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import "testing"

// The instance names below are the shape observed on the operator's 3-GPU
// Windows host (spec: `docs/superpowers/specs/windows-vram-measurer.md`), the
// only place this grammar is documented at all -- Microsoft does not publish
// it. `0x0_0x16026` is the real LUID that could not be resolved there.
const (
	cannedPDHInstance     = "pid_4242_luid_0x0_0x16b6b_phys_0"
	cannedPDHInstanceHigh = "pid_4242_luid_0x1_0xfeed_phys_0"
)

func TestParsePDHInstanceName(t *testing.T) {
	got, ok := parsePDHInstanceName(cannedPDHInstance)
	if !ok {
		t.Fatalf("parsePDHInstanceName(%q) not ok", cannedPDHInstance)
	}
	if got.PID != 4242 {
		t.Errorf("PID = %d, want 4242", got.PID)
	}
	// Field order is `0x<HighPart>_0x<LowPart>`, measured on hardware -- the
	// single fact this parser can get wrong without any visible error.
	if got.LUID != (pdhLUID{HighPart: 0, LowPart: 0x16b6b}) {
		t.Errorf("LUID = %#v, want {HighPart:0 LowPart:0x16b6b}", got.LUID)
	}
	if got.Phys != 0 {
		t.Errorf("Phys = %d, want 0", got.Phys)
	}
	if got.DedicatedBytes != 0 {
		t.Errorf("DedicatedBytes = %d, want 0 (the caller supplies the value)", got.DedicatedBytes)
	}
}

// TestParsePDHInstanceNameHighPartFirst pins the field ORDER with a name whose
// two hex fields differ in both value and magnitude, so swapping them fails.
func TestParsePDHInstanceNameHighPartFirst(t *testing.T) {
	got, ok := parsePDHInstanceName(cannedPDHInstanceHigh)
	if !ok {
		t.Fatalf("parsePDHInstanceName(%q) not ok", cannedPDHInstanceHigh)
	}
	if got.LUID.HighPart != 1 {
		t.Errorf("LUID.HighPart = %d, want 1 (the FIRST hex field)", got.LUID.HighPart)
	}
	if got.LUID.LowPart != 0xfeed {
		t.Errorf("LUID.LowPart = %#x, want 0xfeed (the SECOND hex field)", got.LUID.LowPart)
	}
}

func TestParsePDHInstanceNameCaseInsensitive(t *testing.T) {
	got, ok := parsePDHInstanceName("PID_7_LUID_0x0_0x1ABCD_PHYS_2")
	if !ok {
		t.Fatal("uppercase instance name not parsed")
	}
	if got.PID != 7 || got.LUID.LowPart != 0x1abcd || got.Phys != 2 {
		t.Errorf("got %#v, want pid 7, low 0x1abcd, phys 2", got)
	}
}

// TestParsePDHInstanceNameSkipsUnexpectedShapes proves the parser SKIPS rather
// than panics or guesses. Every `\GPU Process Memory` instance name on the
// probe run matched the grammar, but that is one Windows build on one host:
// anything else must degrade to "not measured", never to a wrong PID or a
// wrong LUID.
func TestParsePDHInstanceNameSkipsUnexpectedShapes(t *testing.T) {
	for _, name := range []string{
		"",
		"_Total",
		"pid_4242_luid_0x0_phys_0",      // one LUID half only
		"pid_4242_luid_0x0_0x16b6b",     // no phys segment
		"luid_0x0_0x16b6b_phys_0",       // no pid segment
		"pid_x_luid_0x0_0x1_phys_0",     // non-numeric pid
		"pid_4242_luid_0xzz_0x1_phys_0", // non-hex LUID half
		"pid_99999999999999999999_luid_0x0_0x1_phys_0", // pid overflows int64
		"pid_1_luid_0x1FFFFFFFFF_0x1_phys_0",           // LUID half overflows uint32
	} {
		if got, ok := parsePDHInstanceName(name); ok {
			t.Errorf("parsePDHInstanceName(%q) = %#v, ok -- want skipped", name, got)
		}
	}
}

func TestParseNvidiaBusID(t *testing.T) {
	// nvidia-smi writes domain:bus:device.function in HEX; D3DKMT reports the
	// same numbers as decimal ints, so the parsed struct is the comparable form.
	got, ok := parseNvidiaBusID("00000000:21:00.0")
	if !ok {
		t.Fatal("parseNvidiaBusID(00000000:21:00.0) not ok")
	}
	if got != (pciAddress{Bus: 0x21, Device: 0, Function: 0}) {
		t.Errorf("got %#v, want {Bus:33 Device:0 Function:0}", got)
	}
	// Hex digits in every field, and no domain component.
	got, ok = parseNvidiaBusID("af:1e.3")
	if !ok {
		t.Fatal("parseNvidiaBusID(af:1e.3) not ok")
	}
	if got != (pciAddress{Bus: 0xaf, Device: 0x1e, Function: 3}) {
		t.Errorf("got %#v, want {Bus:175 Device:30 Function:3}", got)
	}
}

func TestParseNvidiaBusIDSkipsUnparseable(t *testing.T) {
	for _, s := range []string{"", "[N/A]", "00000000:21:00", "21", "00000000:2z:00.0", "00000000:21:00.x"} {
		if got, ok := parseNvidiaBusID(s); ok {
			t.Errorf("parseNvidiaBusID(%q) = %#v, ok -- want skipped", s, got)
		}
	}
}

// canned3GPUBusIDCSV is `nvidia-smi --query-gpu=index,pci.bus_id
// --format=csv,noheader,nounits` for a 3-GPU host.
const canned3GPUBusIDCSV = "0, 00000000:21:00.0\n" +
	"1, 00000000:49:00.0\n" +
	"2, 00000000:4a:00.0\n"

func TestParseNvidiaPCIIndexCSV(t *testing.T) {
	got := parseNvidiaPCIIndexCSV([]byte(canned3GPUBusIDCSV))
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3: %#v", len(got), got)
	}
	if idx, ok := got[pciAddress{Bus: 0x49}]; !ok || idx != 1 {
		t.Errorf("bus 0x49 -> (%d, %v), want (1, true)", idx, ok)
	}
	if idx, ok := got[pciAddress{Bus: 0x4a}]; !ok || idx != 2 {
		t.Errorf("bus 0x4a -> (%d, %v), want (2, true)", idx, ok)
	}
}

func TestParseNvidiaPCIIndexCSVSkipsBadRows(t *testing.T) {
	got := parseNvidiaPCIIndexCSV([]byte("0\n1, [N/A]\n2, 00000000:4a:00.0\n\n"))
	if len(got) != 1 {
		t.Fatalf("got %#v, want only the one parseable row", got)
	}
	if idx := got[pciAddress{Bus: 0x4a}]; idx != 2 {
		t.Errorf("bus 0x4a -> %d, want 2", idx)
	}
}

// pdhTestLUIDs are the three resolvable adapters plus the one that is not.
var (
	luidGPU0             = pdhLUID{LowPart: 0x16b6b}
	luidGPU1             = pdhLUID{LowPart: 0x16b7c}
	luidGPU2             = pdhLUID{LowPart: 0x16b8d}
	luidUnresolved       = pdhLUID{LowPart: 0x16026} // the real one that failed on hardware
	pdhTestLUIDIdx       = map[pdhLUID]int{luidGPU0: 0, luidGPU1: 1, luidGPU2: 2}
	oneMiB         int64 = 1024 * 1024
)

// TestAttributePDHDedicatedSumsPhysAndFiltersPIDs is the headline shape:
// per-phys segment rows sum into one (pid, gpu) number, the operator's
// 3-GPU model appears on all three GPUs, and a PID the manager did not ask
// about is dropped.
func TestAttributePDHDedicatedSumsPhysAndFiltersPIDs(t *testing.T) {
	instances := []pdhProcessMemory{
		{PID: 4242, LUID: luidGPU0, Phys: 0, DedicatedBytes: 6000 * oneMiB},
		{PID: 4242, LUID: luidGPU0, Phys: 1, DedicatedBytes: 500 * oneMiB},
		{PID: 4242, LUID: luidGPU1, Phys: 0, DedicatedBytes: 7000 * oneMiB},
		{PID: 4242, LUID: luidGPU2, Phys: 0, DedicatedBytes: 7100 * oneMiB},
		{PID: 999, LUID: luidGPU0, Phys: 0, DedicatedBytes: 1234 * oneMiB},
	}

	out := attributePDHDedicated(instances, pdhTestLUIDIdx, []int{4242})

	if len(out) != 1 {
		t.Fatalf("out = %v, want exactly pid 4242", out)
	}
	// 6000 + 500 across phys_0/phys_1 of the same (pid, LUID).
	if out[4242][0] != 6500 {
		t.Errorf("out[4242][0] = %d, want 6500 (phys segments summed)", out[4242][0])
	}
	if out[4242][1] != 7000 || out[4242][2] != 7100 {
		t.Errorf("out[4242] = %v, want GPU 1 = 7000 and GPU 2 = 7100", out[4242])
	}
	if _, ok := out[999]; ok {
		t.Errorf("out contains pid 999, which the manager did not ask about")
	}
}

// TestAttributePDHDedicatedDropsZeroSums is the most important behaviour in
// this file. runtime/manager.go buildSnapshot reads
// `if v, ok := byGPU[g.Index]; ok` -- a PRESENT key is authoritative, so a
// measured 0 overrides the operator's VRAM estimate and the GPU budget looks
// entirely free. That is precisely the pre-existing Windows bug this measurer
// exists to fix (nvidia-smi's `[N/A]` -> naInt -> 0), so a 0 must never leave
// this function: no key at all means "not measured", which falls back to the
// estimate.
func TestAttributePDHDedicatedDropsZeroSums(t *testing.T) {
	instances := []pdhProcessMemory{
		{PID: 4242, LUID: luidGPU0, Phys: 0, DedicatedBytes: 0},
		{PID: 4242, LUID: luidGPU1, Phys: 0, DedicatedBytes: 7000 * oneMiB},
		// Under one MiB: truncates to 0 MB, so it is not a measurement
		// either -- and falling back to the (larger) estimate is the safe
		// direction for admission.
		{PID: 4242, LUID: luidGPU2, Phys: 0, DedicatedBytes: 500_000},
	}

	out := attributePDHDedicated(instances, pdhTestLUIDIdx, []int{4242})

	if _, ok := out[4242][0]; ok {
		t.Errorf("out[4242] = %v, want NO key for GPU 0 (its dedicated usage was 0)", out[4242])
	}
	if _, ok := out[4242][2]; ok {
		t.Errorf("out[4242] = %v, want NO key for GPU 2 (its dedicated usage rounds to 0 MB)", out[4242])
	}
	if out[4242][1] != 7000 {
		t.Errorf("out[4242][1] = %d, want 7000", out[4242][1])
	}
	if len(out[4242]) != 1 {
		t.Errorf("out[4242] = %v, want exactly one entry", out[4242])
	}
}

// TestAttributePDHDedicatedDropsPIDWhoseSumsTruncateToZero proves the drop reaches the
// OUTER map too: a pid measured at 0 everywhere must not appear at all, or
// buildSnapshot would charge every one of its GPUs 0 MB.
//
// The byte counts are POSITIVE but sub-MiB on purpose. Zero-byte instances
// would be consumed by the per-instance `DedicatedBytes <= 0` guard, `sums`
// would stay empty, and the outer aggregation loop -- the very thing this test
// names -- would never execute, so no mutation of it could make the test fail
// (it would only re-test what TestAttributePDHDedicatedIgnoresNegativeBytes
// already pins). Sub-MiB values pass that guard, reach the outer loop, and are
// dropped there by the `mb <= 0` truncation guard, which is the path that has
// to hold: an eagerly allocated `out[pid]` would escape as an empty non-nil
// inner map.
func TestAttributePDHDedicatedDropsPIDWhoseSumsTruncateToZero(t *testing.T) {
	instances := []pdhProcessMemory{
		{PID: 4242, LUID: luidGPU0, Phys: 0, DedicatedBytes: 500_000},
		{PID: 4242, LUID: luidGPU1, Phys: 0, DedicatedBytes: 400_000},
	}
	out := attributePDHDedicated(instances, pdhTestLUIDIdx, []int{4242})
	if out != nil {
		t.Fatalf("out = %v, want nil (nothing was measured)", out)
	}
}

// TestAttributePDHDedicatedDropsPIDWhoseInstancesAreAllZeroBytes keeps the
// all-zero-bytes input covered too, one guard earlier: the per-instance
// filter, which must also leave the outer map nil rather than empty.
func TestAttributePDHDedicatedDropsPIDWhoseInstancesAreAllZeroBytes(t *testing.T) {
	instances := []pdhProcessMemory{
		{PID: 4242, LUID: luidGPU0, Phys: 0, DedicatedBytes: 0},
		{PID: 4242, LUID: luidGPU1, Phys: 0, DedicatedBytes: 0},
	}
	out := attributePDHDedicated(instances, pdhTestLUIDIdx, []int{4242})
	if out != nil {
		t.Fatalf("out = %v, want nil (nothing was measured)", out)
	}
}

// TestAttributePDHDedicatedSkipsUnmappedLUID proves an unresolvable LUID (the
// hardware's `0x0_0x16026`, an adapter with no NVIDIA GPU behind it) is
// skipped rather than attributed to GPU 0.
func TestAttributePDHDedicatedSkipsUnmappedLUID(t *testing.T) {
	instances := []pdhProcessMemory{
		{PID: 4242, LUID: luidUnresolved, Phys: 0, DedicatedBytes: 4694 * oneMiB},
		{PID: 4242, LUID: luidGPU1, Phys: 0, DedicatedBytes: 7000 * oneMiB},
	}
	out := attributePDHDedicated(instances, pdhTestLUIDIdx, []int{4242})
	if len(out[4242]) != 1 || out[4242][1] != 7000 {
		t.Fatalf("out[4242] = %v, want only {1:7000}", out[4242])
	}
	if _, ok := out[4242][0]; ok {
		t.Errorf("out[4242] has a GPU 0 entry -- an unmapped LUID must not become index 0")
	}
}

func TestAttributePDHDedicatedNilWhenNothingMatches(t *testing.T) {
	instances := []pdhProcessMemory{
		{PID: 4242, LUID: luidGPU0, Phys: 0, DedicatedBytes: 6000 * oneMiB},
	}
	if out := attributePDHDedicated(instances, pdhTestLUIDIdx, []int{7777}); out != nil {
		t.Fatalf("out = %v, want nil", out)
	}
	if out := attributePDHDedicated(instances, nil, []int{4242}); out != nil {
		t.Fatalf("out with no LUID mapping = %v, want nil", out)
	}
	if out := attributePDHDedicated(nil, pdhTestLUIDIdx, []int{4242}); out != nil {
		t.Fatalf("out with no instances = %v, want nil", out)
	}
}

// TestAttributePDHDedicatedIgnoresNegativeBytes proves a nonsense counter
// value cannot REDUCE a real one through the phys sum.
func TestAttributePDHDedicatedIgnoresNegativeBytes(t *testing.T) {
	instances := []pdhProcessMemory{
		{PID: 4242, LUID: luidGPU0, Phys: 0, DedicatedBytes: 6000 * oneMiB},
		{PID: 4242, LUID: luidGPU0, Phys: 1, DedicatedBytes: -5000 * oneMiB},
	}
	out := attributePDHDedicated(instances, pdhTestLUIDIdx, []int{4242})
	if out[4242][0] != 6000 {
		t.Errorf("out[4242][0] = %d, want 6000 (the negative row ignored)", out[4242][0])
	}
}

// TestNvidiaPCIBusIDFieldsColumnOrder makes the query/parser contract
// machine-checked instead of a comment, exactly as
// TestNvidiaQueryFieldsColumnOrder does for the telemetry query. Both columns
// here are read positionally by parseNvidiaPCIIndexCSV, and swapping them
// produces no error at all: a bus_id parsed as an index becomes 0 (naInt's
// sentinel handling) and an index parsed as a bus_id fails, so the mapping
// silently comes back empty and every measurement is discarded.
func TestNvidiaPCIBusIDFieldsColumnOrder(t *testing.T) {
	if nvidiaPCIBusIDFields != "index,pci.bus_id" {
		t.Fatalf("nvidiaPCIBusIDFields = %q, want \"index,pci.bus_id\" -- parseNvidiaPCIIndexCSV reads row[0] as the index and row[1] as the bus id", nvidiaPCIBusIDFields)
	}
}
