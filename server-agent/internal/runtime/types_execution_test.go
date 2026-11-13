// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"fmt"
	"strings"
	"testing"
)

func execDoc(specExtra string) []byte {
	return []byte(fmt.Sprintf(`{"etag":"e","specs":[{"id":"S","binary":"/x","upstream_model":"m"%s}]}`, specExtra))
}

func TestParseConfigKeepsValidWSLSpec(t *testing.T) {
	cfg, err := ParseConfig(execDoc(`,"execution":{"mode":"wsl","wsl":{"distro":"Ubuntu-24.04"}}`))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if len(cfg.Specs) != 1 {
		t.Fatalf("specs = %d, want 1", len(cfg.Specs))
	}
	if !cfg.Specs[0].IsWSL() {
		t.Fatal("IsWSL() = false, want true")
	}
	if got := cfg.Specs[0].WSLDistro(); got != "Ubuntu-24.04" {
		t.Fatalf("WSLDistro() = %q, want Ubuntu-24.04", got)
	}
}

func TestParseConfigDropsInvalidExecution(t *testing.T) {
	rpc := `"kind":"rpc_server","rpc":{"head_spec_id":"h","transport":"direct","bind_ip":"127.0.0.1","port":47001}`
	cases := map[string]string{
		"docker mode":       `,"execution":{"mode":"docker"}`,
		"wsl without block": `,"execution":{"mode":"wsl"}`,
		"empty distro":      `,"execution":{"mode":"wsl","wsl":{"distro":""}}`,
		"distro with space": `,"execution":{"mode":"wsl","wsl":{"distro":"Ubuntu 24.04"}}`,
		"distro too long":   `,"execution":{"mode":"wsl","wsl":{"distro":"` + strings.Repeat("a", 65) + `"}}`,
		// wsl.exe would read a leading '-' as an option of its own.
		"distro like an option":    `,"execution":{"mode":"wsl","wsl":{"distro":"-d"}}`,
		"distro with a dash first": `,"execution":{"mode":"wsl","wsl":{"distro":"-Ubuntu"}}`,
		"rpc member":               `,` + rpc + `,"execution":{"mode":"wsl","wsl":{"distro":"Ubuntu"}}`,
		"cluster head":             `,"cluster":{"endpoints":[{"address":"a:1","device_count":1}],"transport":"direct","network":"lan"},"execution":{"mode":"wsl","wsl":{"distro":"Ubuntu"}}`,
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			cfg, err := ParseConfig(execDoc(extra))
			if err != nil {
				t.Fatalf("ParseConfig: %v, want the spec dropped without an error", err)
			}
			if len(cfg.Specs) != 0 {
				t.Fatalf("specs = %+v, want the invalid spec dropped", cfg.Specs)
			}
		})
	}
}

func TestParseConfigNativeExecutionIsNative(t *testing.T) {
	for name, extra := range map[string]string{
		"explicit native": `,"execution":{"mode":"native"}`,
		"absent":          ``,
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := ParseConfig(execDoc(extra))
			if err != nil {
				t.Fatalf("ParseConfig: %v", err)
			}
			if len(cfg.Specs) != 1 {
				t.Fatalf("specs = %d, want 1", len(cfg.Specs))
			}
			if cfg.Specs[0].IsWSL() || cfg.Specs[0].WSLDistro() != "" {
				t.Fatal("a native or absent execution block must not read as WSL")
			}
		})
	}
}

func TestSameLaunchShapeTreatsExecutionAsLaunchShape(t *testing.T) {
	wsl := func(distro string) Spec {
		return Spec{ID: "S", Execution: &Execution{Mode: ExecutionModeWSL, WSL: &WSLExecution{Distro: distro}}}
	}
	if sameLaunchShape(wsl("Ubuntu"), wsl("Debian")) {
		t.Fatal("specs differing only in the WSL distro share a launch shape, want a relaunch")
	}
	if !sameLaunchShape(Spec{ID: "S"}, Spec{ID: "S", Execution: &Execution{Mode: ExecutionModeNative}}) {
		t.Fatal("nil execution and native execution differ in launch shape, want the same")
	}
	// An empty mode reads as native (invalidExecutionReason), so it is the same
	// launch as well.
	if !sameLaunchShape(Spec{ID: "S"}, Spec{ID: "S", Execution: &Execution{}}) {
		t.Fatal("nil execution and an empty-mode execution differ in launch shape, want the same")
	}
	if !sameLaunchShape(Spec{ID: "S", Execution: &Execution{}}, Spec{ID: "S", Execution: &Execution{Mode: ExecutionModeNative}}) {
		t.Fatal("empty-mode and native execution differ in launch shape, want the same")
	}
	if sameLaunchShape(Spec{ID: "S"}, wsl("Ubuntu")) {
		t.Fatal("native and WSL share a launch shape, want a relaunch")
	}
}
