// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Per-process VRAM on AMD Linux comes from the kernel's KFD (the amdgpu
// compute driver) sysfs: /sys/class/kfd/kfd/proc/<pid>/vram_<gpu_id> holds the
// bytes of VRAM that process has allocated on that GPU, mode 0444, so the
// agent needs no privileges. It is the same source `rocm-smi --showpids`
// reads, without spawning a Python tool on the manager's owner goroutine.
//
// gpu_id is KFD's opaque per-GPU identifier, not an index. The ordinal is the
// GPU's rank among the KFD topology nodes that are GPUs (gpu_id 0 is a CPU
// node), ordered by node number. That rank is the ROCr device ordinal that
// ROCR_VISIBLE_DEVICES addresses, which is how the agent assigns AMD GPUs, and
// it equals rocm-smi's cardN index, so the measured ordinal and the GPU the
// collector reports line up.
//
// Attribution uses the same process-group boundary as groupNvidiaComputeApps:
// each managed root leads its group, and a worker's VRAM is charged to its
// group's leader. Only processes that opened /dev/kfd (ROCm/HIP) appear here;
// a Vulkan or OpenGL client allocates through the graphics path and is
// invisible, which leaves its spec on the operator's estimate rather than a
// wrong figure. A KFD entry that outlives its process is harmless: it is only
// charged when its group owner is a pid the manager asked about.

// kfdRoot is the KFD sysfs directory.
const kfdRoot = "/sys/class/kfd/kfd"

const kfdVRAMPrefix = "vram_"

// kfdGPUNode is one GPU entry of the KFD topology.
type kfdGPUNode struct {
	node  int
	gpuID uint64
}

// kfdGPUOrdinals maps each GPU's KFD gpu_id to its ordinal under root. Nodes
// whose gpu_id cannot be read or parsed are skipped rather than failing the
// whole map; an error means the topology itself is unreadable.
func kfdGPUOrdinals(root string) (map[uint64]int, error) {
	nodesDir := filepath.Join(root, "topology", "nodes")
	entries, err := os.ReadDir(nodesDir)
	if err != nil {
		return nil, err
	}
	var gpus []kfdGPUNode
	for _, e := range entries {
		if g, ok := readKFDGPUNode(nodesDir, e.Name()); ok {
			gpus = append(gpus, g)
		}
	}
	// Directory order is lexical, so node 10 would sort before node 2.
	sort.Slice(gpus, func(i, j int) bool { return gpus[i].node < gpus[j].node })
	out := make(map[uint64]int, len(gpus))
	for rank, g := range gpus {
		out[g.gpuID] = rank
	}
	return out, nil
}

// readKFDGPUNode reads one topology node directory. It reports false for a
// non-numeric name, an unreadable or unparseable gpu_id, and gpu_id 0 (a CPU
// node).
func readKFDGPUNode(nodesDir, name string) (kfdGPUNode, bool) {
	node, err := strconv.Atoi(name)
	if err != nil || node < 0 {
		return kfdGPUNode{}, false
	}
	raw, err := os.ReadFile(filepath.Join(nodesDir, name, "gpu_id"))
	if err != nil {
		return kfdGPUNode{}, false
	}
	gpuID, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil || gpuID == 0 {
		return kfdGPUNode{}, false
	}
	return kfdGPUNode{node: node, gpuID: gpuID}, true
}

// kfdProcessVRAM reads every KFD process's per-GPU VRAM bytes, keyed by pid
// then gpu_id. A pid directory or file that vanishes mid-scan belongs to a
// process that just exited and is skipped; an error means proc itself is
// unreadable.
func kfdProcessVRAM(root string) (map[int]map[uint64]int64, error) {
	procDir := filepath.Join(root, "proc")
	entries, err := os.ReadDir(procDir)
	if err != nil {
		return nil, err
	}
	out := make(map[int]map[uint64]int64)
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if byGPU := readKFDPidVRAM(filepath.Join(procDir, e.Name())); len(byGPU) > 0 {
			out[pid] = byGPU
		}
	}
	return out, nil
}

// readKFDPidVRAM reads one pid directory's vram_<gpu_id> files. An unreadable
// directory or file is skipped, so the result may be empty.
func readKFDPidVRAM(pidDir string) map[uint64]int64 {
	byGPU := make(map[uint64]int64)
	files, err := os.ReadDir(pidDir)
	if err != nil {
		return byGPU
	}
	for _, f := range files {
		gpuID, ok := parseKFDVRAMName(f.Name())
		if !ok {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(pidDir, f.Name()))
		if err != nil {
			continue
		}
		bytes, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
		if err != nil {
			continue
		}
		byGPU[gpuID] = bytes
	}
	return byGPU
}

// parseKFDVRAMName extracts the gpu_id from a vram_<gpu_id> file name.
func parseKFDVRAMName(name string) (uint64, bool) {
	idText, ok := strings.CutPrefix(name, kfdVRAMPrefix)
	if !ok {
		return 0, false
	}
	gpuID, err := strconv.ParseUint(idText, 10, 64)
	return gpuID, err == nil
}

// attributeKFD charges each KFD process's VRAM to its group owner when that
// owner is one of pids, keyed by GPU ordinal in MiB. Bytes are summed before
// the MiB conversion so several sub-MiB workers are not each rounded away.
// Zero entries are dropped and an empty result is nil: a present key would be
// read as a measured 0 that overrides the operator's estimate.
func attributeKFD(perPID map[int]map[uint64]int64, ordinals map[uint64]int, pids []int, groupOf func(pid int) (int, bool)) map[int]map[int]int {
	wanted := make(map[int]bool, len(pids))
	for _, p := range pids {
		wanted[p] = true
	}
	sums := make(map[int]map[int]int64)
	for pid, byGPU := range perPID {
		owner, ok := groupOf(pid)
		if !ok || !wanted[owner] {
			continue
		}
		sumKFDBytes(sums, owner, byGPU, ordinals)
	}
	return kfdSumsToMiB(sums)
}

// sumKFDBytes adds one process's per-GPU bytes to its owner's per-ordinal
// sums, skipping GPUs that have no ordinal.
func sumKFDBytes(sums map[int]map[int]int64, owner int, byGPU map[uint64]int64, ordinals map[uint64]int) {
	for gpuID, bytes := range byGPU {
		ordinal, ok := ordinals[gpuID]
		if !ok {
			continue
		}
		if sums[owner] == nil {
			sums[owner] = make(map[int]int64)
		}
		sums[owner][ordinal] += bytes
	}
}

// kfdSumsToMiB converts summed bytes to MiB, dropping entries that round to
// zero or below. The result is nil when nothing remains.
func kfdSumsToMiB(sums map[int]map[int]int64) map[int]map[int]int {
	var out map[int]map[int]int
	for owner, byOrdinal := range sums {
		for ordinal, bytes := range byOrdinal {
			mib := int(bytes >> 20)
			if mib <= 0 {
				continue
			}
			if out == nil {
				out = make(map[int]map[int]int)
			}
			if out[owner] == nil {
				out[owner] = make(map[int]int)
			}
			out[owner][ordinal] = mib
		}
	}
	return out
}

// kfdMeasurer reads the topology on every call instead of caching it: the
// reads are a few small sysfs files per GPU and per process, and a fresh read
// can never attribute to a GPU that was reset or removed.
type kfdMeasurer struct {
	root    string
	groupOf func(pid int) (int, bool)
}

func (m kfdMeasurer) measure(pids []int) map[int]map[int]int {
	if len(pids) == 0 {
		return nil
	}
	ordinals, err := kfdGPUOrdinals(m.root)
	if err != nil {
		return nil
	}
	perPID, err := kfdProcessVRAM(m.root)
	if err != nil {
		return nil
	}
	return attributeKFD(perPID, ordinals, pids, m.groupOf)
}
