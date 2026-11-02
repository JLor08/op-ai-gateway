// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"math"
	"os"
	"runtime"
	"testing"
)

// The per-process RAM measurer is one exported name with one implementation
// per platform (ram_measurer_other.go / ram_measurer_windows.go), so these
// tests carry no build tag: they assert the contract both implementations
// share, and the Windows one is at least compiled and vetted wherever
// `GOOS=windows go vet ./...` runs.

// newTestRAMMeasurer returns the platform's measurer, failing rather than
// skipping when it is absent off Windows: on every other platform the measurer
// is a plain gopsutil read of the process table, so a nil there is a defect,
// not an unsupported host. Windows keeps the skip because it can legitimately
// be nil (a psapi export missing on a stripped-down image).
func newTestRAMMeasurer(t *testing.T) func(pids []int) map[int]int {
	t.Helper()
	m := NewRAMMeasurer()
	if m == nil {
		if runtime.GOOS == "windows" {
			t.Skip("no process RAM measurer on this Windows host")
		}
		t.Fatal("NewRAMMeasurer() = nil, want a measurer: reading a process's own RSS must work on every non-Windows platform")
	}
	return m
}

// TestRAMMeasurerReportsSelf is the whole happy path against the one process
// every test can be sure exists. A Go test binary holds well over a megabyte
// of resident memory, so a result of 0 or a missing key would mean the
// measurer read nothing rather than that the process is small.
func TestRAMMeasurerReportsSelf(t *testing.T) {
	m := newTestRAMMeasurer(t)
	pid := os.Getpid()
	got := m([]int{pid})
	if got[pid] <= 1 {
		t.Fatalf("measurer([self %d]) = %v, want a resident size of more than 1 MB for this test process", pid, got)
	}
}

// TestRAMMeasurerSkipsAPidItCannotRead pins the "say nothing" rule: a pid that
// is gone (the exit race the manager's own generation guard exists for) is
// ABSENT from the result, never present with 0. The manager reads a present
// key as a measurement, and a measured 0 means "unknown", not "needs no RAM".
func TestRAMMeasurerSkipsAPidItCannotRead(t *testing.T) {
	m := newTestRAMMeasurer(t)
	self := os.Getpid()
	const gone = math.MaxInt32 - 1 // far above any pid_max, and a valid int32
	got := m([]int{gone, self})
	if _, ok := got[gone]; ok {
		t.Fatalf("measurer reported pid %d, which is not a live process: %v", gone, got)
	}
	if got[self] <= 1 {
		t.Fatalf("one unreadable pid cost the readable one its measurement: %v", got)
	}
}

// TestRAMMeasurerIgnoresNonPositivePIDs: pid 0 is the System Idle Process on
// Windows and the kernel on Unix, and a negative value is never a pid. The
// measurer must not even ask the OS about them -- on some platforms asking
// about pid 0 succeeds and reports the kernel's own memory, which would be
// attributed to whatever spec the caller meant.
func TestRAMMeasurerIgnoresNonPositivePIDs(t *testing.T) {
	m := newTestRAMMeasurer(t)
	if got := m([]int{0, -1}); len(got) != 0 {
		t.Fatalf("measurer([0, -1]) = %v, want nothing", got)
	}
}

// TestRAMMeasurerNothingToMeasure: an empty or nil pid list is "nothing to
// measure", answered without touching the OS.
func TestRAMMeasurerNothingToMeasure(t *testing.T) {
	m := newTestRAMMeasurer(t)
	if got := m(nil); len(got) != 0 {
		t.Fatalf("measurer(nil) = %v, want nothing", got)
	}
	if got := m([]int{}); len(got) != 0 {
		t.Fatalf("measurer([]) = %v, want nothing", got)
	}
}
