// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build !windows

package collector

import "testing"

// TestNewVRAMMeasurerMatchesComputeAppsOffWindows is the runtime half of the
// selection test, on the arm CI can actually execute: introducing the selector
// must leave Linux, macOS and every other non-Windows host with EXACTLY the
// measurer they had before it existed. Nil-ness is the whole observable
// difference (function values are not comparable), and it is the one that
// matters: nil means "estimates only", non-nil means "measure".
//
// The assertion holds either way round -- on a CI runner without nvidia-smi
// both are nil, on a real NVIDIA Linux host both are non-nil -- so it never
// needs to skip. It only has teeth where nvidia-smi exists, though: on a
// runner without it, both sides are nil for the same trivial reason.
// TestNonWindowsMeasurerArmKeepsComputeApps is the one that bites everywhere.
func TestNewVRAMMeasurerMatchesComputeAppsOffWindows(t *testing.T) {
	if (NewVRAMMeasurer() == nil) != (NewNvidiaComputeApps() == nil) {
		t.Fatalf("NewVRAMMeasurer() nil = %v, NewNvidiaComputeApps() nil = %v -- off Windows the selector must resolve to compute-apps unchanged",
			NewVRAMMeasurer() == nil, NewNvidiaComputeApps() == nil)
	}
}
