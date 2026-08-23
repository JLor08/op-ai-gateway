// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseRocmJSON(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "rocm-smi.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	gpus, err := parseRocmJSON(data)
	if err != nil {
		t.Fatalf("parseRocmJSON: %v", err)
	}
	if len(gpus) != 1 {
		t.Fatalf("want 1 GPU (system key skipped), got %d", len(gpus))
	}

	g := gpus[0]
	if g.Index != 0 {
		t.Errorf("Index = %d, want 0", g.Index)
	}
	if g.Name != "AMD Instinct MI210" {
		t.Errorf("Name = %q, want %q", g.Name, "AMD Instinct MI210")
	}
	if g.UtilPct != 42 {
		t.Errorf("UtilPct = %v, want 42", g.UtilPct)
	}
	if g.MemUsedBytes != 12884901888 {
		t.Errorf("MemUsedBytes = %d, want 12884901888", g.MemUsedBytes)
	}
	if g.MemTotalBytes != 68702699520 {
		t.Errorf("MemTotalBytes = %d, want 68702699520", g.MemTotalBytes)
	}
	if g.TempC != 58 {
		t.Errorf("TempC = %d, want 58", g.TempC)
	}
	if g.PowerW != 170 {
		t.Errorf("PowerW = %v, want 170", g.PowerW)
	}
}

func TestParseRocmJSONMissingKey(t *testing.T) {
	// A card lacking the power field must default PowerW to 0, not error.
	data := []byte(`{"card0":{"Card series":"AMD Radeon","GPU use (%)":"5","VRAM Total Memory (B)":"1000","VRAM Total Used Memory (B)":"500","Temperature (Sensor edge) (C)":"30.0"}}`)

	gpus, err := parseRocmJSON(data)
	if err != nil {
		t.Fatalf("parseRocmJSON: %v", err)
	}
	if len(gpus) != 1 {
		t.Fatalf("want 1 GPU, got %d", len(gpus))
	}
	if gpus[0].PowerW != 0 {
		t.Errorf("PowerW = %v, want 0 (missing key)", gpus[0].PowerW)
	}
	if gpus[0].Name != "AMD Radeon" {
		t.Errorf("Name = %q, want %q", gpus[0].Name, "AMD Radeon")
	}
}

func TestParseRocmJSONEmpty(t *testing.T) {
	gpus, err := parseRocmJSON([]byte(`{}`))
	if err != nil {
		t.Fatalf("empty object: unexpected error %v", err)
	}
	if len(gpus) != 0 {
		t.Fatalf("empty object: want 0 GPUs, got %d", len(gpus))
	}
}

func TestParseRocmJSONCapturesDriverVersion(t *testing.T) {
	data := []byte(`{"card0":{"Card series":"MI300","VRAM Total Memory (B)":"68719476736"},"system":{"Driver version":"6.2.4"}}`)
	gpus, err := parseRocmJSON(data)
	if err != nil || len(gpus) != 1 {
		t.Fatalf("parse = %v err=%v", gpus, err)
	}
	if gpus[0].DriverVersion != "6.2.4" {
		t.Fatalf("driver_version = %q, want 6.2.4", gpus[0].DriverVersion)
	}
}
