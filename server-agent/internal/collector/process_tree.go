// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import "sync"

// This file is the process-tree registry (#424): the Windows counterpart of
// the process-group boundary the Linux measurers use (groupNvidiaComputeApps
// in nvidia_groups_linux.go). A runtime spec with process_tree "job" runs in a
// Windows job object, and a model server such as Strata does its real work in
// a GRANDCHILD -- the process holding nearly all of the model's VRAM and RAM
// is not the PID the manager knows. The runtime registers each job-mode root
// with a func listing the job's live processes; the measurers then charge
// every member's RAM and per-GPU VRAM to that root, the PID the manager asks
// about. A PID in no registered tree keeps its own, per-PID attribution.
//
// Like nvidia_pdh.go, it carries NO build tag: the registry and every
// attribution decision run, and are tested, on every platform. Only the
// Windows runtime registers trees, so elsewhere the registry stays empty and
// every helper below is a no-op.
//
// A tree's members func is a live job query (QueryInformationJobObject), so a
// measurement takes ONE snapshot -- each members func called once -- and
// resolves every PID against it, rather than querying per PID. The registry's
// lock is never held across a members call: the query is a syscall, and the
// runtime may be unregistering (and closing the job) at the same moment. The
// members func itself must be safe after its job is closed; the runtime's
// jobControl.pids returns nil then, under the mutex close takes, so a
// snapshot racing an unregister sees an empty tree, never a recycled handle.

type processTreeEntry struct {
	root    int
	members func() []int
}

var processTrees struct {
	mu      sync.Mutex
	nextID  uint64
	entries map[uint64]processTreeEntry
}

// RegisterProcessTree records that every process members lists belongs to the
// managed process root, until the returned unregister is called. unregister is
// idempotent and removes this registration only, so a later registration of
// the same root PID (a new generation that reused it) survives an earlier
// generation's late unregister. members may list root itself or not; root is
// always a member of its own tree.
func RegisterProcessTree(root int, members func() []int) (unregister func()) {
	processTrees.mu.Lock()
	defer processTrees.mu.Unlock()
	if processTrees.entries == nil {
		processTrees.entries = make(map[uint64]processTreeEntry)
	}
	processTrees.nextID++
	id := processTrees.nextID
	processTrees.entries[id] = processTreeEntry{root: root, members: members}
	return func() {
		processTrees.mu.Lock()
		defer processTrees.mu.Unlock()
		delete(processTrees.entries, id)
	}
}

// processTreeSnapshot is one reading of every registered tree. The zero value
// is the empty snapshot.
type processTreeSnapshot struct {
	// root maps every member PID, roots included, to its tree's root.
	root map[int]int
	// members maps each root to its tree's distinct PIDs, root first.
	members map[int][]int
}

// snapshotProcessTrees reads every registered tree once.
//
// A PID two trees claim goes to the smaller root, so the answer does not
// depend on map order; a root is always its own tree's, whatever another
// tree's members func lists. Neither happens with the runtime's jobs (each
// managed process is in exactly one job of the agent's), so this is a
// determinism rule, not a policy.
func snapshotProcessTrees() processTreeSnapshot {
	processTrees.mu.Lock()
	entries := make([]processTreeEntry, 0, len(processTrees.entries))
	for _, e := range processTrees.entries {
		entries = append(entries, e)
	}
	processTrees.mu.Unlock()
	if len(entries) == 0 {
		return processTreeSnapshot{}
	}

	listed := make(map[int][]int, len(entries))
	for _, e := range entries {
		listed[e.root] = append(listed[e.root], e.members()...)
	}

	snap := processTreeSnapshot{root: make(map[int]int), members: make(map[int][]int, len(listed))}
	for root := range listed {
		snap.root[root] = root
	}
	for root, pids := range listed {
		snap.claim(root, pids)
	}
	for pid, root := range snap.root {
		if pid != root {
			snap.members[root] = append(snap.members[root], pid)
		}
	}
	for root := range listed {
		snap.members[root] = append([]int{root}, snap.members[root]...)
	}
	return snap
}

// claim assigns pids to root unless another tree holds them: a root keeps
// itself, and a contested PID goes to the smaller root.
func (s processTreeSnapshot) claim(root int, pids []int) {
	for _, pid := range pids {
		if pid <= 0 {
			continue
		}
		if cur, ok := s.root[pid]; ok && (cur == pid || cur <= root) {
			continue
		}
		s.root[pid] = root
	}
}

// rootOf reports the registered tree pid belongs to.
func (s processTreeSnapshot) rootOf(pid int) (int, bool) {
	root, ok := s.root[pid]
	return root, ok
}

// membersOf lists root's tree, root included. A PID that roots no registered
// tree is its own one-process tree.
func (s processTreeSnapshot) membersOf(root int) []int {
	if m, ok := s.members[root]; ok {
		return append([]int(nil), m...)
	}
	return []int{root}
}

// treeRootOf reports the registered tree pid belongs to, from a fresh
// snapshot. A measurement resolving many PIDs takes one snapshot instead.
func treeRootOf(pid int) (root int, ok bool) {
	return snapshotProcessTrees().rootOf(pid)
}

// treeMembers lists root's tree, root included, from a fresh snapshot.
func treeMembers(root int) []int {
	return snapshotProcessTrees().membersOf(root)
}

// measureTreeRAM is the RAM measurer's contract (pid -> MB; an unreadable or
// sub-MB pid absent; nothing measurable nil) over trees: each requested pid's
// figure is the sum of readBytes over its tree's members, summed in bytes and
// rounded once. A member that cannot be read (exited between the snapshot and
// the read, or access denied) contributes nothing; the rest still count, so a
// root that has already exited while its tree runs on is still charged for
// the tree.
func measureTreeRAM(pids []int, snap processTreeSnapshot, readBytes func(pid int) (uint64, bool)) map[int]int {
	var out map[int]int
	for _, pid := range pids {
		var total uint64
		for _, member := range snap.membersOf(pid) {
			if b, ok := readBytes(member); ok {
				total += b
			}
		}
		mb := int(int64(total) / bytesPerMB)
		if mb <= 0 {
			continue
		}
		if out == nil {
			out = make(map[int]int, len(pids))
		}
		out[pid] = mb
	}
	return out
}

// pdhRowsToRoots returns instances with each member PID replaced by its tree's
// root, so attributePDHDedicated sums a tree's per-GPU VRAM under the PID the
// manager asks about. The input is not modified; an empty snapshot returns it
// as is.
func (s processTreeSnapshot) pdhRowsToRoots(instances []pdhProcessMemory) []pdhProcessMemory {
	if len(s.root) == 0 {
		return instances
	}
	out := make([]pdhProcessMemory, len(instances))
	for i, in := range instances {
		if root, ok := s.root[in.PID]; ok {
			in.PID = root
		}
		out[i] = in
	}
	return out
}

// computeAppsToRoots is pdhRowsToRoots for nvidia-smi compute-apps rows. It
// rewrites rows in place, as groupNvidiaComputeApps' contract allows.
func (s processTreeSnapshot) computeAppsToRoots(rows []nvidiaComputeAppRow) []nvidiaComputeAppRow {
	for i := range rows {
		if root, ok := s.root[rows[i].PID]; ok {
			rows[i].PID = root
		}
	}
	return rows
}
