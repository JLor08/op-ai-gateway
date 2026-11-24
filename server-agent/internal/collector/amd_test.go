// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"os"
	"path/filepath"
	"slices"
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
	if g.UUID != "" || g.PCIBusID != "" {
		t.Errorf("UUID/PCIBusID = %q/%q, want empty (3.x emits neither)", g.UUID, g.PCIBusID)
	}
}

func TestParseRocmJSON4x(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "rocm-smi-4.0.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	gpus, err := parseRocmJSON(data)
	if err != nil {
		t.Fatalf("parseRocmJSON: %v", err)
	}
	if len(gpus) != 2 {
		t.Fatalf("want 2 GPUs, got %d", len(gpus))
	}
	want := []struct {
		uuid, pci   string
		used        int64
		util, power float64
		temp        int
	}{
		{"0x12b1004de0189bb2", "0000:01:00.0", 4002840576, 3, 24, 61},
		{"0xf427f0e640d6686b", "0000:02:00.0", 17174528, 0, 8, 42},
	}
	for i, w := range want {
		g := gpus[i]
		if g.Index != i || g.Name != "AMD Radeon RX 6900 XT" {
			t.Errorf("gpu %d: index/name = %d/%q", i, g.Index, g.Name)
		}
		if g.UUID != w.uuid || g.PCIBusID != w.pci {
			t.Errorf("gpu %d: uuid/pci = %q/%q, want %q/%q", i, g.UUID, g.PCIBusID, w.uuid, w.pci)
		}
		if g.MemTotalBytes != 17163091968 || g.MemUsedBytes != w.used {
			t.Errorf("gpu %d: mem = %d/%d", i, g.MemUsedBytes, g.MemTotalBytes)
		}
		if g.UtilPct != w.util || g.TempC != w.temp || g.PowerW != w.power {
			t.Errorf("gpu %d: util/temp/power = %v/%d/%v", i, g.UtilPct, g.TempC, g.PowerW)
		}
		if g.DriverVersion != "6.8.0-147-generic" {
			t.Errorf("gpu %d: driver = %q", i, g.DriverVersion)
		}
	}
}

func TestParseRocmJSONSentinelsAreEmpty(t *testing.T) {
	data := []byte(`{"card0":{"Device Name":"N/A","Card Series":"AMD X","Unique ID":"N/A","PCI Bus":"n/a"}}`)
	gpus, err := parseRocmJSON(data)
	if err != nil || len(gpus) != 1 {
		t.Fatalf("parse = %v err=%v", gpus, err)
	}
	g := gpus[0]
	if g.Name != "AMD X" || g.UUID != "" || g.PCIBusID != "" {
		t.Errorf("name/uuid/pci = %q/%q/%q", g.Name, g.UUID, g.PCIBusID)
	}
}

func TestRocmArgs(t *testing.T) {
	if !slices.Contains(rocmArgs, "--showbus") || !slices.Contains(rocmArgs, "--showuniqueid") {
		t.Errorf("rocmArgs missing --showbus/--showuniqueid: %v", rocmArgs)
	}
	if slices.Contains(rocmArgs, "--showmemuse") {
		t.Errorf("rocmArgs must not contain --showmemuse: %v", rocmArgs)
	}
	i := slices.Index(rocmArgs, "--showmeminfo")
	if i < 0 || i+1 >= len(rocmArgs) || rocmArgs[i+1] != "vram" {
		t.Errorf("rocmArgs needs --showmeminfo vram adjacent: %v", rocmArgs)
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
