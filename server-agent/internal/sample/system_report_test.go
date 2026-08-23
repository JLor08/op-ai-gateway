// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package sample

import (
	"encoding/json"
	"testing"
)

func TestSystemReportNormalizeAndWireTags(t *testing.T) {
	r := SystemReport{AgentVersion: "1.2.3", OS: "linux"}
	r.Normalize()
	if r.GPUs == nil {
		t.Fatal("Normalize must force a non-nil GPUs slice")
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// The wire contract: snake_case tags the gateway agentSystemReport mirrors.
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"agent_version", "os", "arch", "cpu", "memory", "mainboard", "bios", "gpus", "collected_at"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("wire missing key %q in %s", k, b)
		}
	}
}

func TestGPUDriverVersionTag(t *testing.T) {
	b, _ := json.Marshal(GPU{Name: "X", DriverVersion: "550.1"})
	if got := string(b); !contains(got, `"driver_version":"550.1"`) {
		t.Fatalf("GPU driver_version tag missing: %s", got)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	}()
}
