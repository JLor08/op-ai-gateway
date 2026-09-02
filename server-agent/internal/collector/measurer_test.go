// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"testing"
)

// referencedIdents returns every identifier the given source file MENTIONS IN
// CODE. Parsed with mode 0, so comments are not attached and prose that names a
// function does not count -- which matters here, because both selector files
// discuss NewNvidiaComputeApps in their doc comments on purpose.
func referencedIdents(t *testing.T, file string) map[string]bool {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	out := make(map[string]bool)
	ast.Inspect(f, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			out[id.Name] = true
		}
		return true
	})
	return out
}

// TestWindowsMeasurerArmNeverSelectsComputeApps is the only machine-checkable
// statement of the rule on a Linux CI runner, and the rule is the reason this
// selector exists.
//
// On Windows the compute-apps measurer is not merely useless, it is ACTIVELY
// HARMFUL: `nvidia-smi --query-compute-apps=...,used_memory` reports `[N/A]`
// under WDDM, naInt maps that to 0, and attributeComputeApps therefore returns
// a NON-NIL map of zeros. buildSnapshot treats a present key as authoritative,
// so every managed model is charged 0 MB, the GPU budget looks entirely free,
// and co-residency admission loses the OOM protection it exists for. Nothing
// -- no measurer at all -- is strictly safer there, because nothing falls back
// to the operator's estimate.
//
// CI (ubuntu-latest) never compiles measurer_windows.go, so this reads the
// source instead: an identifier check, in the spirit of internal/archtest's
// import-graph rules.
func TestWindowsMeasurerArmNeverSelectsComputeApps(t *testing.T) {
	ids := referencedIdents(t, "measurer_windows.go")
	for _, banned := range []string{"NewNvidiaComputeApps", "nvidiaComputeAppsMeasurer", "attributeComputeApps"} {
		if ids[banned] {
			t.Errorf("measurer_windows.go references %s -- on Windows the compute-apps measurer yields measured ZEROS that override every operator VRAM estimate; the Windows arm must select the PDH measurer or nil", banned)
		}
	}
	if !ids["newNvidiaPDHMeasurer"] {
		t.Error("measurer_windows.go does not call newNvidiaPDHMeasurer -- the Windows arm must select the PDH measurer (which returns nil by itself when the host cannot support it)")
	}
}

// TestNonWindowsMeasurerArmKeepsComputeApps is the other half: every platform
// that is not Windows must keep today's behaviour exactly, so the non-Windows
// arm selects compute-apps and nothing else. The PDH measurer cannot even
// build there (pdh.dll, gdi32.dll), so naming it would be a compile error --
// but a compile error only on the platforms that are not the one being edited.
func TestNonWindowsMeasurerArmKeepsComputeApps(t *testing.T) {
	ids := referencedIdents(t, "measurer_other.go")
	if !ids["NewNvidiaComputeApps"] {
		t.Error("measurer_other.go does not call NewNvidiaComputeApps -- every non-Windows platform must keep the measurer it has today")
	}
	if ids["newNvidiaPDHMeasurer"] {
		t.Error("measurer_other.go references newNvidiaPDHMeasurer, which exists only on Windows")
	}
}

// TestNewVRAMMeasurerNilWithoutNvidiaSMI holds on every platform and for both
// arms: without nvidia-smi there is no way to attribute a measurement to a GPU
// index at all, so the constructor must return nil rather than a measurer that
// fails every call. Measurement is a hardware capability, not a negotiated
// feature (design doc §5) -- SetMeasurer(nil) is NewManager's own default and
// leaves the operator's estimates standing.
func TestNewVRAMMeasurerNilWithoutNvidiaSMI(t *testing.T) {
	if _, err := exec.LookPath(nvidiaSMI); err == nil {
		t.Skip("nvidia-smi is present on this host; nothing to prove here")
	}
	if f := NewVRAMMeasurer(); f != nil {
		t.Fatal("NewVRAMMeasurer() = non-nil without nvidia-smi on PATH, want nil")
	}
}
