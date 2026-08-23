// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// lhmAMDDataJSON mirrors the real LibreHardwareMonitor /data.json shape on an AMD
// Threadripper host: the CPU package power leaf is named just "Package" (NOT "CPU
// Package", which is the Intel spelling), under an /amdcpu SensorId; there are also
// per-core "Core #N (SMU)" power leaves and, on the GPU node, a "GPU Package" power
// leaf. The collector must pick the AMD CPU "Package" and neither a core nor the GPU.
const lhmAMDDataJSON = `{
  "Text": "Sensor",
  "Children": [
    {
      "Text": "AMD Ryzen Threadripper PRO 9965WX 24-Cores",
      "Children": [
        {
          "Text": "Powers",
          "Children": [
            { "Text": "Package", "Type": "Power", "Value": "61,7 W", "SensorId": "/amdcpu/0/power/0" },
            { "Text": "Core #1 (SMU)", "Type": "Power", "Value": "0,4 W", "SensorId": "/amdcpu/0/power/1" }
          ]
        }
      ]
    },
    {
      "Text": "NVIDIA GeForce RTX 4090",
      "Children": [
        {
          "Text": "Powers",
          "Children": [
            { "Text": "GPU Package", "Type": "Power", "Value": "7,7 W", "SensorId": "/gpu-nvidia/0/power/0" }
          ]
        }
      ]
    }
  ]
}`

// TestLHMCollectMatchesAMDPackageCPUPower: the AMD "Package" (comma-decimal) is read
// as CPU watts; the GPU "GPU Package" and per-core "Core (SMU)" leaves are NOT.
func TestLHMCollectMatchesAMDPackageCPUPower(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(lhmAMDDataJSON))
	}))
	defer srv.Close()

	cpu, system, err := newLHMPowerCollector(srv.URL, srv.Client()).Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect err: %v", err)
	}
	if cpu == nil {
		t.Fatal("cpu watts = nil; want the AMD 'Package' power (61.7 W)")
	}
	if *cpu < 61.6 || *cpu > 61.8 {
		t.Fatalf("cpu watts = %v; want ~61.7 (the AMD 'Package', not a core or the GPU package)", *cpu)
	}
	// This host's LHM tree has no whole-system/board power rail -> system stays nil.
	if system != nil {
		t.Fatalf("system watts = %v; want nil (no board/PSU total power sensor present)", *system)
	}
}
