// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package hostpower

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const qemuSlice = "qemu.slice"

// DiscoverVMs lists the numeric VM IDs of the QEMU scopes under
// cgroupRoot/qemu.slice, sorted ascending. It returns an empty slice when the
// slice is absent.
func DiscoverVMs(cgroupRoot string) []string {
	entries, err := os.ReadDir(filepath.Join(cgroupRoot, qemuSlice))
	if err != nil {
		return []string{}
	}
	type vm struct {
		id  string
		num uint64
	}
	var vms []vm
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id, ok := strings.CutSuffix(e.Name(), ".scope")
		if !ok {
			continue
		}
		n, err := strconv.ParseUint(id, 10, 64)
		if err != nil {
			continue
		}
		vms = append(vms, vm{id, n})
	}
	sort.Slice(vms, func(i, j int) bool { return vms[i].num < vms[j].num })
	ids := make([]string, 0, len(vms))
	for _, v := range vms {
		ids = append(ids, v.id)
	}
	return ids
}

// ReadVMUsageUsec returns the cumulative CPU time of a VM's cgroup scope in
// microseconds; false when cpu.stat is unreadable or has no usage_usec.
func ReadVMUsageUsec(cgroupRoot, vmid string) (uint64, bool) {
	b, err := os.ReadFile(filepath.Join(cgroupRoot, qemuSlice, vmid+".scope", "cpu.stat"))
	if err != nil {
		return 0, false
	}
	return ParseCgroupUsageUsec(string(b))
}
