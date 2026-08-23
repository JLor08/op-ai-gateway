// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"os"
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
