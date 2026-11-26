// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build linux

package collector

import "testing"

// TestPickLinuxVRAMMeasurer pins the Linux priority with fakes, so it holds on
// any runner: NVIDIA first (every NVIDIA host keeps exactly the measurer it
// had, mixed hosts included), KFD only when nvidia-smi is absent, and nil --
// operator estimates stand -- when neither source exists. Function values are
// not comparable, so each fake is identified by what it returns.
func TestPickLinuxVRAMMeasurer(t *testing.T) {
	nvidia := func([]int) map[int]map[int]int { return map[int]map[int]int{1: {0: 1}} }
	kfd := func([]int) map[int]map[int]int { return map[int]map[int]int{2: {0: 2}} }

	if got := pickLinuxVRAMMeasurer(nvidia, kfd); got == nil || got(nil)[1] == nil {
		t.Error("both present: want the nvidia measurer")
	}
	if got := pickLinuxVRAMMeasurer(nil, kfd); got == nil || got(nil)[2] == nil {
		t.Error("nvidia absent: want the KFD measurer")
	}
	if got := pickLinuxVRAMMeasurer(nil, nil); got != nil {
		t.Error("both absent: want nil")
	}
}
