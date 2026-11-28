// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build linux

package collector

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Per-process VRAM on AMD Linux comes primarily from the kernel's DRM fdinfo:
// every open amdgpu render node is a DRM client whose /proc/<pid>/fdinfo/<fd>
// reports drm-memory-vram, for ROCm/HIP and Vulkan processes alike (KFD sees
// only the former). The pure parsing and summing live in drm_fdinfo.go; this
// file walks /proc and probes the kernel.

const (
	linuxProcRoot = "/proc"
	devDRIDir     = "/dev/dri"
	sysClassDRM   = "/sys/class/drm"
	drmDevLink    = "/dev/dri/"
)

// fdinfoMeasurer charges each managed process group's amdgpu VRAM to the
// group's leader, the boundary groupNvidiaComputeApps and KFD attribution use.
// index is the AMD collector's published PCI→index join (currentAMDIndex in
// production); without it nothing is measured, because an index derived any
// other way could disagree with the GPU the collector reports.
type fdinfoMeasurer struct {
	procRoot string
	index    func() map[pciAddr]int
	groupOf  func(pid int) (int, bool)
}

// measure de-duplicates clients across ALL fds and pids of a group before
// summing: a Vulkan process holds several fds on one render node that share a
// client id, and a forked worker inherits its leader's fds. A pid whose fds
// cannot be read contributes no clients, so an unreadable group is "not
// measured" (the estimate stays), never a measured 0; cards below the noise
// floor and cards the join does not cover are dropped.
func (m fdinfoMeasurer) measure(pids []int) map[int]map[int]int {
	if len(pids) == 0 {
		return nil
	}
	idx := m.index()
	if len(idx) == 0 {
		return nil
	}
	wanted := make(map[int]bool, len(pids))
	for _, p := range pids {
		wanted[p] = true
	}
	var out map[int]map[int]int
	for owner, members := range scanGroupMembers(m.procRoot, wanted, m.groupOf) {
		var clients []drmClient
		for _, pid := range members {
			clients = append(clients, readDRMClients(m.procRoot, pid)...)
		}
		sums, _ := sumDRMClients(clients)
		if byIdx := drmVRAMToMiB(sums, idx); byIdx != nil {
			if out == nil {
				out = make(map[int]map[int]int)
			}
			out[owner] = byIdx
		}
	}
	return out
}

// scanGroupMembers maps each wanted group owner to every pid under procRoot
// whose group it leads (the owner included, when it is alive). Non-numeric
// entries and pids without a group (exited mid-scan) are skipped.
func scanGroupMembers(procRoot string, wanted map[int]bool, groupOf func(int) (int, bool)) map[int][]int {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil
	}
	out := make(map[int][]int)
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		owner, ok := groupOf(pid)
		if !ok || !wanted[owner] {
			continue
		}
		out[owner] = append(out[owner], pid)
	}
	return out
}

// readDRMClients returns pid's amdgpu DRM clients: only fds whose link target
// is under /dev/dri/ are read, so sockets, model files and /dev/kfd never are.
// A permission error or a vanished pid yields no clients.
func readDRMClients(procRoot string, pid int) []drmClient {
	dir := filepath.Join(procRoot, strconv.Itoa(pid))
	fds, err := os.ReadDir(filepath.Join(dir, "fd"))
	if err != nil {
		return nil
	}
	var out []drmClient
	for _, fd := range fds {
		target, err := os.Readlink(filepath.Join(dir, "fd", fd.Name()))
		if err != nil || !strings.HasPrefix(target, drmDevLink) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, "fdinfo", fd.Name()))
		if err != nil {
			continue
		}
		if c, ok := parseDRMFdinfo(data); ok {
			out = append(out, c)
		}
	}
	return out
}

// probeDRMFdinfo reports whether this kernel's amdgpu fdinfo carries
// drm-memory-vram: it opens the first amdgpu render node the agent may open,
// reads its own fdinfo for that fd and closes it again. False -- no amdgpu
// render node, none openable, or no parseable key -- selects the KFD fallback.
func probeDRMFdinfo() bool {
	for _, node := range amdgpuRenderNodes(devDRIDir, sysClassDRM) {
		if ok, opened := probeRenderNode(node); opened {
			return ok
		}
	}
	return false
}

// amdgpuRenderNodes lists devDir's render nodes whose
// <sysDRM>/<node>/device/driver link names amdgpu, in node order.
func amdgpuRenderNodes(devDir, sysDRM string) []string {
	nodes, err := filepath.Glob(filepath.Join(devDir, "renderD*"))
	if err != nil {
		return nil
	}
	var out []string
	for _, node := range nodes { // Glob sorts; render minors are all three digits
		driver, err := os.Readlink(filepath.Join(sysDRM, filepath.Base(node), "device", "driver"))
		if err == nil && filepath.Base(driver) == "amdgpu" {
			out = append(out, node)
		}
	}
	return out
}

// probeRenderNode opens path and reads /proc/self/fdinfo for the new fd.
// opened is false when the node could not be opened (try the next one); ok is
// true when the fdinfo parses as an amdgpu client with drm-memory-vram -- the
// same parse the measurer relies on.
func probeRenderNode(path string) (ok, opened bool) {
	f, err := os.Open(path)
	if err != nil {
		return false, false
	}
	defer f.Close()
	data, err := os.ReadFile(filepath.Join(linuxProcRoot, "self", "fdinfo", strconv.FormatUint(uint64(f.Fd()), 10)))
	if err != nil {
		return false, true
	}
	c, parsed := parseDRMFdinfo(data)
	return parsed && c.HasVRAM, true
}
