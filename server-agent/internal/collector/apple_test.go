// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseIoregGPU(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "ioreg-ioaccelerator.txt"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	gpus, err := parseIoregGPU(data)
	if err != nil {
		t.Fatalf("parseIoregGPU: %v", err)
	}
	if len(gpus) != 1 {
		t.Fatalf("want 1 GPU, got %d", len(gpus))
	}

	g := gpus[0]
	if g.Index != 0 {
		t.Errorf("Index = %d, want 0", g.Index)
	}
	if g.Name == "" {
		t.Errorf("Name is empty, want non-empty (fixture has %q)", "Apple M4 Max")
	}
	if g.Name != "Apple M4 Max" {
		t.Errorf("Name = %q, want %q", g.Name, "Apple M4 Max")
	}
	// The captured fixture reports "Device Utilization %"=0.
	if g.UtilPct != 0 {
		t.Errorf("UtilPct = %v, want 0 (fixture value)", g.UtilPct)
	}
	// Memory: the PerformanceStatistics dict contains a DECOY key
	// "In use system memory (driver)"=0 BEFORE the real "In use system memory"
	// =28360704. Asserting the real value pins the parser's decoy-exclusion — a
	// looser regex that matched the "(driver)" variant would yield 0 here.
	if g.MemUsedBytes != 28360704 {
		t.Errorf("MemUsedBytes = %d, want 28360704 (parser must skip the \"(driver)\" decoy key)", g.MemUsedBytes)
	}
	if g.MemTotalBytes != 3171975168 {
		t.Errorf("MemTotalBytes = %d, want 3171975168 (\"Alloc system memory\")", g.MemTotalBytes)
	}
}

func TestParseIoregGPUTolerant(t *testing.T) {
	// Raw, key-less text must never panic and yields a single GPU with a
	// zero utilization when the key is absent.
	gpus, err := parseIoregGPU([]byte("some unrelated ioreg text with no accelerator keys\n"))
	if err != nil {
		t.Fatalf("tolerant parse: unexpected error %v", err)
	}
	if len(gpus) != 1 {
		t.Fatalf("want 1 GPU even without keys, got %d", len(gpus))
	}
	if gpus[0].UtilPct != 0 {
		t.Errorf("UtilPct = %v, want 0 (key absent)", gpus[0].UtilPct)
	}
}
