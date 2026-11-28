// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestSanitizeSystemReportKeepsGPUVendorFields(t *testing.T) {
	var r agentSystemReport
	raw := `{"vulkan_device_count":-2,"gpus":[
		{"index":0,"name":"a","vendor":" AMD ","vram_measurable":["vulkan","rocm","bogus","rocm"],"vulkan_ordinal":1},
		{"index":1,"name":"b","vendor":"intel","vulkan_ordinal":-3},
		{"index":2,"name":"c","vendor":"nvidia","vram_measurable":["bogus"]}]}`
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatal(err)
	}
	canonical, _ := sanitizeSystemReport(&r, time.Now())
	var got agentSystemReport
	if err := json.Unmarshal(canonical, &got); err != nil {
		t.Fatal(err)
	}
	if got.VulkanDeviceCount != 0 {
		t.Errorf("vulkan_device_count = %d, want 0", got.VulkanDeviceCount)
	}
	g := got.GPUs
	if g[0].Vendor != "amd" || !reflect.DeepEqual(g[0].VRAMMeasurable, []string{"rocm", "vulkan"}) ||
		g[0].VulkanOrdinal == nil || *g[0].VulkanOrdinal != 1 {
		t.Errorf("gpu0 = %+v", g[0])
	}
	if g[1].Vendor != "" || g[1].VulkanOrdinal != nil {
		t.Errorf("gpu1 = %+v, want empty vendor and no ordinal", g[1])
	}
	if g[2].Vendor != "nvidia" || len(g[2].VRAMMeasurable) != 0 {
		t.Errorf("gpu2 = %+v", g[2])
	}
}

func TestSanitizeSystemReportLegacyVendor(t *testing.T) {
	for raw, want := range map[string]string{` NVIDIA `: "nvidia", `amd`: "amd", `intel`: ""} {
		var r agentSystemReport
		if err := json.Unmarshal([]byte(`{"gpu_legacy_vendor":"`+raw+`"}`), &r); err != nil {
			t.Fatal(err)
		}
		sanitizeSystemReport(&r, time.Now())
		if r.GPULegacyVendor != want {
			t.Errorf("gpu_legacy_vendor %q -> %q, want %q", raw, r.GPULegacyVendor, want)
		}
	}
}

func TestTelemetryGPUSampleCarriesVendorAndPCI(t *testing.T) {
	req, _ := ingestReq(t, `{"host":{"cpu_util_pct":1},"gpus":[{"index":0,"name":"x","vendor":"AMD","pci_bus_id":"0000:01:00.0"},{"index":1,"vendor":"intel"}]}`)
	s, err := telemetrySampleFromRequest(req, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if s.GPUs[0].Vendor != "amd" || s.GPUs[0].PCIBusID != "0000:01:00.0" {
		t.Errorf("gpu0 = %+v", s.GPUs[0])
	}
	if s.GPUs[1].Vendor != "" {
		t.Errorf("gpu1 vendor = %q, want empty", s.GPUs[1].Vendor)
	}
	p := perfPointFromSample(s)
	if p.GPUs[0].Vendor != "amd" {
		t.Errorf("perf vendor = %q", p.GPUs[0].Vendor)
	}
}

func TestRuntimeStatusGPUCarriesVendor(t *testing.T) {
	var samples []agentRuntimeSample
	raw := `[{"spec_id":"s","model":"m","gpus":[{"index":0,"vendor":"nvidia","vram_measured_mb":100},{"index":1,"vendor":"intel","vram_measured_mb":50}]}]`
	if err := json.Unmarshal([]byte(raw), &samples); err != nil {
		t.Fatal(err)
	}
	rows := runtimeStatusDTOsFromSamples(samples, time.Now())
	if len(rows) != 1 || len(rows[0].GPUs) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[0].GPUs[0].Vendor != "nvidia" || rows[0].GPUs[1].Vendor != "" {
		t.Errorf("gpus = %+v", rows[0].GPUs)
	}
}
