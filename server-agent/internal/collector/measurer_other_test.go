// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build !windows && !linux

package collector

import "testing"

// TestNewVRAMMeasurerMatchesComputeAppsOffWindows is the runtime half of the
// selection test for every host that is neither Windows nor Linux: there the
// selector must resolve to EXACTLY the compute-apps measurer. Linux is
// excluded because it adds an AMD measurer, so on an AMD Linux host the
// selector is non-nil while compute-apps is nil; the linuxVRAMMeasurer tests
// pin the Linux assembly with fakes instead. Nil-ness is the whole observable
// difference (function values are not comparable), and it is the one that
// matters: nil means "estimates only", non-nil means "measure".
//
// The assertion holds either way round -- without nvidia-smi both are nil,
// with it both are non-nil -- so it never needs to skip. It only has teeth
// where nvidia-smi exists, though: without it, both sides are nil for the same
// trivial reason.
// TestNonWindowsMeasurerArmKeepsComputeApps is the one that bites everywhere.
func TestNewVRAMMeasurerMatchesComputeAppsOffWindows(t *testing.T) {
	if (NewVRAMMeasurer().Measure == nil) != (NewNvidiaComputeApps() == nil) {
		t.Fatalf("NewVRAMMeasurer().Measure nil = %v, NewNvidiaComputeApps() nil = %v -- off Windows and Linux the selector must resolve to compute-apps unchanged",
			NewVRAMMeasurer().Measure == nil, NewNvidiaComputeApps() == nil)
	}
}
