// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
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
// that is not Windows must keep compute-apps as its first choice, so the
// non-Windows arm delegates to the per-platform fallback and both fallbacks
// call NewNvidiaComputeApps. Only the Linux fallback may add the AMD measurers
// (DRM fdinfo and KFD are Linux kernel interfaces). The PDH measurer cannot
// even build there (pdh.dll, gdi32.dll), so naming it would be a compile error
// -- but a compile error only on the platforms that are not the one being
// edited.
func TestNonWindowsMeasurerArmKeepsComputeApps(t *testing.T) {
	if !referencedIdents(t, "measurer_other.go")["fallbackVRAMMeasurer"] {
		t.Error("measurer_other.go does not call fallbackVRAMMeasurer -- the non-Windows arm must go through the per-platform fallback")
	}
	for _, file := range []string{"measurer_other.go", "measurer_fallback_linux.go", "measurer_fallback_other.go"} {
		if referencedIdents(t, file)["newNvidiaPDHMeasurer"] {
			t.Errorf("%s references newNvidiaPDHMeasurer, which exists only on Windows", file)
		}
	}
	for _, file := range []string{"measurer_fallback_linux.go", "measurer_fallback_other.go"} {
		if !referencedIdents(t, file)["NewNvidiaComputeApps"] {
			t.Errorf("%s does not call NewNvidiaComputeApps -- every non-Windows platform must keep the measurer it has today", file)
		}
	}
	linux := referencedIdents(t, "measurer_fallback_linux.go")
	if !linux["NewKFDMeasurer"] {
		t.Error("measurer_fallback_linux.go does not call NewKFDMeasurer -- AMD Linux hosts on a kernel without fdinfo VRAM would measure nothing")
	}
	// The AMD wiring: the fdinfo self-probe picks the source, and the AMD
	// index comes from the collector's published PCI join and nothing else.
	for _, id := range []string{"probeDRMFdinfo", "fdinfoMeasurer", "currentAMDIndex", "linuxVRAMMeasurer"} {
		if !linux[id] {
			t.Errorf("measurer_fallback_linux.go does not reference %s -- the Linux AMD measurer is wired around the fdinfo probe and the collector's join", id)
		}
	}
	if referencedIdents(t, "measurer_fallback_other.go")["NewKFDMeasurer"] {
		t.Error("measurer_fallback_other.go references NewKFDMeasurer, which exists only on Linux")
	}
}

// TestNewVRAMMeasurerNilWithoutNvidiaSMI holds on every platform and for both
// arms wherever no GPU source exists: without nvidia-smi (and, on Linux,
// without KFD or an amdgpu render node) there is no way to attribute a
// measurement to a GPU index at all, so the constructor must return a nil
// Measure rather than a measurer that fails every call. Measurement is a hardware capability, not a negotiated feature
// (design doc §5) -- SetMeasurer(nil) is NewManager's own default and leaves
// the operator's estimates standing.
func TestNewVRAMMeasurerNilWithoutNvidiaSMI(t *testing.T) {
	if _, err := exec.LookPath(nvidiaSMI); err == nil {
		t.Skip("nvidia-smi is present on this host; nothing to prove here")
	}
	if _, err := os.Stat(filepath.Join(kfdRoot, "proc")); err == nil {
		t.Skip("KFD sysfs is present on this host; the Linux fallback may measure from it")
	}
	if _, err := os.Stat("/dev/dri"); err == nil {
		t.Skip("DRM devices are present on this host; the Linux fallback may measure from amdgpu fdinfo")
	}
	if f := NewVRAMMeasurer(); f.Measure != nil {
		t.Fatal("NewVRAMMeasurer().Measure = non-nil without nvidia-smi on PATH or KFD sysfs, want nil")
	}
}
