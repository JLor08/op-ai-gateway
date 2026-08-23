// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// lhmDataJSON is a realistic LibreHardwareMonitor Remote Web Server /data.json
// sensor tree: sensor leaves carry a Type ("Power") and a Value string ("65.0 W").
const lhmDataJSON = `{
  "Text": "Sensor",
  "Children": [
    {
      "Text": "MACHINE",
      "Children": [
        {
          "Text": "AMD Ryzen 9 5950X",
          "Children": [
            {
              "Text": "Powers",
              "Children": [
                { "Text": "CPU Package", "Type": "Power", "Value": "65.0 W" },
                { "Text": "CPU Cores", "Type": "Power", "Value": "50.0 W" }
              ]
            }
          ]
        },
        {
          "Text": "Mainboard",
          "Children": [
            {
              "Text": "Powers",
              "Children": [
                { "Text": "System Total", "Type": "Power", "Value": "180.0 W" }
              ]
            }
          ]
        }
      ]
    }
  ]
}`

func TestLHMCollectParsesCPUAndSystem(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(lhmDataJSON))
	}))
	defer srv.Close()

	c := newLHMPowerCollector(srv.URL, srv.Client())
	if !c.Available() {
		t.Fatal("a configured URL must be available")
	}
	cpu, system, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if cpu == nil || *cpu != 65.0 {
		t.Fatalf("CPU watts = %v, want 65.0 (first 'CPU Package' Power sensor)", cpu)
	}
	if system == nil || *system != 180.0 {
		t.Fatalf("system watts = %v, want 180.0 ('System Total')", system)
	}
}

func TestLHMCollectUnreachableYieldsNil(t *testing.T) {
	c := newLHMPowerCollector("http://127.0.0.1:0/data.json", &http.Client{})
	cpu, system, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect should never error (best-effort), got %v", err)
	}
	if cpu != nil || system != nil {
		t.Fatalf("want nil/nil on an unreachable endpoint, got %v/%v", cpu, system)
	}
}

func TestLHMEmptyURLUnavailable(t *testing.T) {
	if newLHMPowerCollector("", nil).Available() {
		t.Fatal("an empty URL must be unavailable")
	}
}

// lhmCPUPackageWrongTypeJSON has a "CPU Package" sensor whose Type is
// "Temperature", not "Power" — LHM also reports a CPU Package temperature
// sensor under the same name. It must NOT be read as watts: the Type=="Power"
// gate must hold regardless of the sensor name.
const lhmCPUPackageWrongTypeJSON = `{
  "Text": "Sensor",
  "Children": [
    {
      "Text": "MACHINE",
      "Children": [
        {
          "Text": "AMD Ryzen 9 5950X",
          "Children": [
            {
              "Text": "Temperatures",
              "Children": [
                { "Text": "CPU Package", "Type": "Temperature", "Value": "45.0 °C" }
              ]
            }
          ]
        }
      ]
    }
  ]
}`

func TestLHMCollectIgnoresCPUPackageWithWrongType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(lhmCPUPackageWrongTypeJSON))
	}))
	defer srv.Close()

	c := newLHMPowerCollector(srv.URL, srv.Client())
	cpu, system, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if cpu != nil {
		t.Fatalf("CPU watts = %v, want nil (the sensor's Type is Temperature, not Power)", *cpu)
	}
	if system != nil {
		t.Fatalf("system watts = %v, want nil (no Power sensor at all)", *system)
	}
}

// lhmNoPowerSensorJSON is a well-formed sensor tree with sensors present but
// none of Type "Power" — the realistic shape when only temperature/load/clock
// sensors are exposed (e.g. a stripped-down LHM config).
const lhmNoPowerSensorJSON = `{
  "Text": "Sensor",
  "Children": [
    {
      "Text": "MACHINE",
      "Children": [
        {
          "Text": "AMD Ryzen 9 5950X",
          "Children": [
            {
              "Text": "Temperatures",
              "Children": [
                { "Text": "CPU Package", "Type": "Temperature", "Value": "45.0 °C" }
              ]
            },
            {
              "Text": "Load",
              "Children": [
                { "Text": "CPU Total", "Type": "Load", "Value": "12.0 %" }
              ]
            }
          ]
        }
      ]
    }
  ]
}`

func TestLHMCollectNoPowerSensorYieldsNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(lhmNoPowerSensorJSON))
	}))
	defer srv.Close()

	c := newLHMPowerCollector(srv.URL, srv.Client())
	cpu, system, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if cpu != nil {
		t.Fatalf("CPU watts = %v, want nil (no matching Power sensor)", *cpu)
	}
	if system != nil {
		t.Fatalf("system watts = %v, want nil (no matching Power sensor), not 0", *system)
	}
}

// lhmSystemAgentOnlyJSON has a "System Agent" Power sensor — an Intel CPU
// package SUB-RAIL (uncore/IMC/PCIe domain), not a whole-board/total rail —
// and no genuine system-total rail. "System Agent" contains "system" as a
// substring, so a naive Contains(name, "system") match would wrongly read it
// as TOTAL system watts.
const lhmSystemAgentOnlyJSON = `{
  "Text": "Sensor",
  "Children": [
    {
      "Text": "MACHINE",
      "Children": [
        {
          "Text": "Intel Core i9-13900K",
          "Children": [
            {
              "Text": "Powers",
              "Children": [
                { "Text": "CPU Package", "Type": "Power", "Value": "125.0 W" },
                { "Text": "System Agent", "Type": "Power", "Value": "8.0 W" }
              ]
            }
          ]
        }
      ]
    }
  ]
}`

func TestLHMSystemAgentRailNotMislabeledAsSystemWatts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(lhmSystemAgentOnlyJSON))
	}))
	defer srv.Close()

	c := newLHMPowerCollector(srv.URL, srv.Client())
	cpu, system, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if cpu == nil || *cpu != 125.0 {
		t.Fatalf("CPU watts = %v, want 125.0 ('CPU Package')", cpu)
	}
	if system != nil {
		t.Fatalf("system watts = %v, want nil ('System Agent' is a CPU sub-rail, not a total-system rail)", *system)
	}
}

// lhmSystemAgentThenTotalJSON has the misleading "System Agent" CPU sub-rail
// BEFORE a genuine total-system rail in tree order, so a buggy first-match
// implementation would lock onto "System Agent" and never reach "System
// Total".
const lhmSystemAgentThenTotalJSON = `{
  "Text": "Sensor",
  "Children": [
    {
      "Text": "MACHINE",
      "Children": [
        {
          "Text": "Intel Core i9-13900K",
          "Children": [
            {
              "Text": "Powers",
              "Children": [
                { "Text": "CPU Package", "Type": "Power", "Value": "125.0 W" },
                { "Text": "System Agent", "Type": "Power", "Value": "8.0 W" }
              ]
            }
          ]
        },
        {
          "Text": "Mainboard",
          "Children": [
            {
              "Text": "Powers",
              "Children": [
                { "Text": "System Total", "Type": "Power", "Value": "210.0 W" }
              ]
            }
          ]
        }
      ]
    }
  ]
}`

func TestLHMSystemAgentDoesNotShadowGenuineSystemRail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(lhmSystemAgentThenTotalJSON))
	}))
	defer srv.Close()

	c := newLHMPowerCollector(srv.URL, srv.Client())
	cpu, system, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if cpu == nil || *cpu != 125.0 {
		t.Fatalf("CPU watts = %v, want 125.0 ('CPU Package')", cpu)
	}
	if system == nil || *system != 210.0 {
		t.Fatalf("system watts = %v, want 210.0 ('System Total'), not the 'System Agent' CPU sub-rail", system)
	}
}
