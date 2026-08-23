// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// lhmTempIntelJSON mirrors a LibreHardwareMonitor /data.json tree on an Intel
// host: a genuine "CPU Package" Temperature sensor, its misleading "CPU
// Package Distance to TjMax" sibling (a margin/countdown value, not an
// absolute temperature — must be excluded), a GPU package temp, and a
// motherboard temp (both must be ignored).
const lhmTempIntelJSON = `{
  "Text": "Sensor",
  "Children": [
    {
      "Text": "MACHINE",
      "Children": [
        {
          "Text": "Intel Core i9-13900K",
          "Children": [
            {
              "Text": "Temperatures",
              "Children": [
                { "Text": "CPU Package", "Type": "Temperature", "Value": "45.0 °C" },
                { "Text": "CPU Package Distance to TjMax", "Type": "Temperature", "Value": "55.0 °C" }
              ]
            }
          ]
        },
        {
          "Text": "NVIDIA GeForce RTX 4090",
          "Children": [
            {
              "Text": "Temperatures",
              "Children": [
                { "Text": "GPU Package", "Type": "Temperature", "Value": "62.0 °C" }
              ]
            }
          ]
        },
        {
          "Text": "Mainboard",
          "Children": [
            {
              "Text": "Temperatures",
              "Children": [
                { "Text": "Motherboard", "Type": "Temperature", "Value": "38.0 °C" }
              ]
            }
          ]
        }
      ]
    }
  ]
}`

func TestLHMTempCollectParsesIntelCPUPackage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Errorf("request must carry no Authorization header (LHM has no token), got %q", auth)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(lhmTempIntelJSON))
	}))
	defer srv.Close()

	c := newLHMTempCollector(srv.URL, srv.Client())
	if !c.Available() {
		t.Fatal("a configured URL must be available")
	}
	cpu, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if cpu == nil || *cpu != 45.0 {
		t.Fatalf("cpu temp = %v, want 45.0 ('CPU Package', not the Distance-to-TjMax margin, the GPU, or the board)", cpu)
	}
}

func TestLHMTempCollectUnreachableYieldsNil(t *testing.T) {
	c := newLHMTempCollector("http://127.0.0.1:0/data.json", &http.Client{})
	cpu, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect should never error (best-effort), got %v", err)
	}
	if cpu != nil {
		t.Fatalf("want nil on an unreachable endpoint, got %v", *cpu)
	}
}

func TestLHMTempEmptyURLUnavailable(t *testing.T) {
	if newLHMTempCollector("", nil).Available() {
		t.Fatal("an empty URL must be unavailable")
	}
}

// lhmTempAMDJSON mirrors an AMD host: the CPU package control temperature is
// named just "Tctl" (not "CPU Package", the Intel spelling), under an
// /amdcpu SensorId (comma-decimal locale); a GPU core temp must be ignored.
const lhmTempAMDJSON = `{
  "Text": "Sensor",
  "Children": [
    {
      "Text": "AMD Ryzen Threadripper PRO 9965WX 24-Cores",
      "Children": [
        {
          "Text": "Temperatures",
          "Children": [
            { "Text": "Tctl", "Type": "Temperature", "Value": "61,7 °C", "SensorId": "/amdcpu/0/temperature/2" }
          ]
        }
      ]
    },
    {
      "Text": "NVIDIA GeForce RTX 4090",
      "Children": [
        {
          "Text": "Temperatures",
          "Children": [
            { "Text": "GPU Core", "Type": "Temperature", "Value": "70.0 °C", "SensorId": "/gpu-nvidia/0/temperature/0" }
          ]
        }
      ]
    }
  ]
}`

func TestLHMTempCollectMatchesAMDTctl(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(lhmTempAMDJSON))
	}))
	defer srv.Close()

	cpu, err := newLHMTempCollector(srv.URL, srv.Client()).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect err: %v", err)
	}
	if cpu == nil {
		t.Fatal("cpu temp = nil; want the AMD 'Tctl' temperature (~61.7 °C)")
	}
	if *cpu < 61.6 || *cpu > 61.8 {
		t.Fatalf("cpu temp = %v; want ~61.7 (the AMD 'Tctl', not the GPU)", *cpu)
	}
}

// TestFindLHMTempIntelPackage exercises findLHMTemp directly (no HTTP): the
// CPU-package temperature is picked, the Distance-to-TjMax margin and the GPU
// package temp are ignored, and every "Temperature" sensor name is reported.
func TestFindLHMTempIntelPackage(t *testing.T) {
	root := &lhmNode{Children: []lhmNode{
		{Text: "CPU", Children: []lhmNode{
			{Text: "CPU Package", Type: "Temperature", Value: "45.0 °C"},
			{Text: "CPU Package Distance to TjMax", Type: "Temperature", Value: "55.0 °C"},
		}},
		{Text: "GPU", Children: []lhmNode{
			{Text: "GPU Package", Type: "Temperature", Value: "62.0 °C"},
		}},
	}}
	cpu, names := findLHMTemp(root)
	if cpu == nil || *cpu != 45.0 {
		t.Fatalf("cpu = %v, want 45.0", cpu)
	}
	if len(names) != 3 {
		t.Fatalf("temp sensor names = %v, want 3 entries", names)
	}
}

// TestFindLHMTempAMDTctl proves the AMD "Tctl" leaf (disambiguated via
// sensorIDIsCPU, mirroring findLHMPower's AMD "Package" handling) is picked
// over a GPU temperature sensor.
func TestFindLHMTempAMDTctl(t *testing.T) {
	root := &lhmNode{Children: []lhmNode{
		{Text: "CPU", Children: []lhmNode{
			{Text: "Tctl", Type: "Temperature", Value: "61,7 °C", SensorID: "/amdcpu/0/temperature/2"},
		}},
		{Text: "GPU", Children: []lhmNode{
			{Text: "GPU Core", Type: "Temperature", Value: "70.0 °C", SensorID: "/gpu-amd/0/temperature/0"},
		}},
	}}
	cpu, _ := findLHMTemp(root)
	if cpu == nil || *cpu < 61.6 || *cpu > 61.8 {
		t.Fatalf("cpu = %v, want ~61.7 (AMD Tctl)", cpu)
	}
}

// TestFindLHMTempNoMatchYieldsNil: board/board-only Temperature sensors and a
// non-Temperature Load sensor never match.
func TestFindLHMTempNoMatchYieldsNil(t *testing.T) {
	root := &lhmNode{Children: []lhmNode{
		{Text: "Motherboard", Type: "Temperature", Value: "38.0 °C"},
		{Text: "Load", Children: []lhmNode{
			{Text: "CPU Total", Type: "Load", Value: "12.0 %"},
		}},
	}}
	cpu, names := findLHMTemp(root)
	if cpu != nil {
		t.Fatalf("cpu = %v, want nil (no CPU-package temperature sensor)", *cpu)
	}
	if len(names) != 1 || names[0] != "Motherboard" {
		t.Fatalf("temp sensor names = %v, want [Motherboard]", names)
	}
}

// TestFindLHMTempEmptyYieldsNil: an empty tree yields nil + no sensor names.
func TestFindLHMTempEmptyYieldsNil(t *testing.T) {
	cpu, names := findLHMTemp(&lhmNode{})
	if cpu != nil {
		t.Fatalf("cpu = %v, want nil", *cpu)
	}
	if len(names) != 0 {
		t.Fatalf("temp sensor names = %v, want none", names)
	}
}

// TestFindLHMTempGpuPackageOnlyYieldsNil: a GPU "…Package" Temperature sensor with
// a GPU SensorId (and NO CPU sibling) must NOT match — the AMD "package" fallback is
// gated by sensorIDIsCPU. Guards the CPU-id gate against a sensor-ordering where the
// GPU node is the only "package" match present.
func TestFindLHMTempGpuPackageOnlyYieldsNil(t *testing.T) {
	root := &lhmNode{Children: []lhmNode{
		{Text: "GPU", Children: []lhmNode{
			{Text: "GPU Package", Type: "Temperature", Value: "62.0 °C", SensorID: "/gpu-nvidia/0/temperature/0"},
		}},
	}}
	cpu, _ := findLHMTemp(root)
	if cpu != nil {
		t.Fatalf("cpu = %v, want nil (a GPU 'package' temp must not match the CPU fallback)", *cpu)
	}
}

// TestFindLHMTempDistanceBeforeRealNotReturned: when Intel's "CPU Package Distance to
// TjMax" MARGIN sensor is visited BEFORE the real "CPU Package" (LHM can order
// sensors either way), the !distance guard must skip the margin so the absolute
// reading is returned — not the margin value.
func TestFindLHMTempDistanceBeforeRealNotReturned(t *testing.T) {
	root := &lhmNode{Children: []lhmNode{
		{Text: "CPU", Children: []lhmNode{
			{Text: "CPU Package Distance to TjMax", Type: "Temperature", Value: "55.0 °C"},
			{Text: "CPU Package", Type: "Temperature", Value: "45.0 °C"},
		}},
	}}
	cpu, _ := findLHMTemp(root)
	if cpu == nil || *cpu != 45.0 {
		t.Fatalf("cpu = %v, want 45.0 (the distance-to-TjMax margin must be skipped even when it comes first)", cpu)
	}
}

func TestParseLHMCelsius(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	cases := []struct {
		name string
		in   string
		want *float64
	}{
		{"space+degree", "45.0 °C", f(45.0)},
		{"comma decimal", "45,0 °C", f(45.0)},
		{"no space", "45.0°C", f(45.0)},
		{"bare C", "45 C", f(45.0)},
		{"empty", "", nil},
		{"unparseable", "n/a", nil},
	}
	for _, c := range cases {
		got := parseLHMCelsius(c.in)
		if (got == nil) != (c.want == nil) {
			t.Fatalf("%s: got %v want %v", c.name, got, c.want)
		}
		if got != nil && *got != *c.want {
			t.Fatalf("%s: got %v want %v", c.name, *got, *c.want)
		}
	}
}

// TestLHMTempPowerSourcesUnaffected proves the temp collector's Available()
// gate mirrors the power one for symmetry (same "" -> unavailable contract).
func TestLHMTempAvailableMirrorsPower(t *testing.T) {
	if newLHMTempCollector("  ", nil).Available() {
		t.Fatal("a whitespace-only URL must be unavailable (TrimSpace)")
	}
}
