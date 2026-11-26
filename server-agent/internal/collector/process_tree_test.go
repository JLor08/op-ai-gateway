// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"reflect"
	"slices"
	"sync"
	"testing"
)

// The process-tree registry (#424) is global state. Each test registers its
// own roots and unregisters them in a cleanup, and no two tests share a PID,
// so they stay independent.

const mib int64 = 1024 * 1024

func registerTestTree(t *testing.T, root int, members ...int) func() {
	t.Helper()
	unregister := RegisterProcessTree(root, func() []int { return slices.Clone(members) })
	t.Cleanup(unregister)
	return unregister
}

func TestProcessTreeRegistryRootAndMembers(t *testing.T) {
	// The members func of a real job lists the root too; this one leaves it
	// out to pin that treeMembers adds it.
	unregister := registerTestTree(t, 100, 101, 102)

	for _, pid := range []int{100, 101, 102} {
		if root, ok := treeRootOf(pid); !ok || root != 100 {
			t.Fatalf("treeRootOf(%d) = %d, %v; want 100, true", pid, root, ok)
		}
	}
	if _, ok := treeRootOf(200); ok {
		t.Fatal("treeRootOf(200) reported a tree for an unregistered pid")
	}
	got := treeMembers(100)
	slices.Sort(got)
	if want := []int{100, 101, 102}; !reflect.DeepEqual(got, want) {
		t.Fatalf("treeMembers(100) = %v, want %v", got, want)
	}
	if got := treeMembers(200); !reflect.DeepEqual(got, []int{200}) {
		t.Fatalf("treeMembers(200) = %v, want [200] (an unregistered pid is its own tree)", got)
	}

	unregister()
	unregister() // idempotent
	if _, ok := treeRootOf(101); ok {
		t.Fatal("treeRootOf(101) still reports a tree after unregister")
	}
	if got := treeMembers(100); !reflect.DeepEqual(got, []int{100}) {
		t.Fatalf("treeMembers(100) after unregister = %v, want [100]", got)
	}
}

func TestProcessTreeUnregisterOnlyRemovesItsOwnRegistration(t *testing.T) {
	// A root PID reused by a later generation registers again; the earlier
	// generation's late unregister must not remove the later one.
	first := RegisterProcessTree(110, func() []int { return []int{110, 111} })
	second := registerTestTree(t, 110, 110, 112)
	first()
	if root, ok := treeRootOf(112); !ok || root != 110 {
		t.Fatalf("treeRootOf(112) = %d, %v after the first registration's unregister; want 110, true", root, ok)
	}
	if _, ok := treeRootOf(111); ok {
		t.Fatal("treeRootOf(111) still reports the unregistered generation's member")
	}
	second()
}

func TestProcessTreeRAMSumsMembers(t *testing.T) {
	unregister := registerTestTree(t, 100, 100, 101, 102)
	sizes := map[int]uint64{
		100: uint64(10 * mib),
		101: uint64(2000 * mib),
		102: uint64(300 * mib),
		200: uint64(512 * mib),
	}
	read := func(pid int) (uint64, bool) {
		b, ok := sizes[pid]
		return b, ok
	}

	got := measureTreeRAM([]int{100, 200}, snapshotProcessTrees(), read)
	if want := map[int]int{100: 2310, 200: 512}; !reflect.DeepEqual(got, want) {
		t.Fatalf("measureTreeRAM = %v, want %v", got, want)
	}

	unregister()
	got = measureTreeRAM([]int{100, 200}, snapshotProcessTrees(), read)
	if want := map[int]int{100: 10, 200: 512}; !reflect.DeepEqual(got, want) {
		t.Fatalf("measureTreeRAM after unregister = %v, want %v", got, want)
	}
}

func TestProcessTreeRAMKeepsTheMeasurerContract(t *testing.T) {
	registerTestTree(t, 120, 120, 121, 122)
	// The root has exited (unreadable), one member reads, one does not.
	read := func(pid int) (uint64, bool) {
		if pid == 121 {
			return uint64(700 * mib), true
		}
		return 0, false
	}
	got := measureTreeRAM([]int{120, 130}, snapshotProcessTrees(), read)
	if want := map[int]int{120: 700}; !reflect.DeepEqual(got, want) {
		t.Fatalf("measureTreeRAM = %v, want %v", got, want)
	}
	// Nothing readable: nil, never a present 0.
	if got := measureTreeRAM([]int{120, 130}, snapshotProcessTrees(), func(int) (uint64, bool) { return 0, false }); got != nil {
		t.Fatalf("measureTreeRAM with nothing readable = %v, want nil", got)
	}
	// Under 1 MB in total rounds to 0 and is dropped.
	if got := measureTreeRAM([]int{120}, snapshotProcessTrees(), func(int) (uint64, bool) { return 1000, true }); got != nil {
		t.Fatalf("measureTreeRAM with a sub-MB tree = %v, want nil", got)
	}
}

func TestProcessTreeRAMCountsADuplicatedMemberOnce(t *testing.T) {
	registerTestTree(t, 140, 140, 141, 141)
	read := func(int) (uint64, bool) { return uint64(100 * mib), true }
	got := measureTreeRAM([]int{140}, snapshotProcessTrees(), read)
	if want := map[int]int{140: 200}; !reflect.DeepEqual(got, want) {
		t.Fatalf("measureTreeRAM = %v, want %v", got, want)
	}
}

func TestProcessTreePDHRowsAttributeToRoot(t *testing.T) {
	unregister := registerTestTree(t, 100, 100, 101, 102)
	gpu0 := pdhLUID{HighPart: 0, LowPart: 0x1000}
	gpu1 := pdhLUID{HighPart: 0, LowPart: 0x1001}
	gpu2 := pdhLUID{HighPart: 0, LowPart: 0x1002}
	luidToIndex := map[pdhLUID]int{gpu0: 0, gpu1: 1, gpu2: 2}
	instances := []pdhProcessMemory{
		{PID: 101, LUID: gpu0, DedicatedBytes: 8 * 1024 * mib},
		{PID: 101, LUID: gpu2, DedicatedBytes: 20 * 1024 * mib},
		{PID: 102, LUID: gpu2, DedicatedBytes: 1 * 1024 * mib},
		{PID: 300, LUID: gpu1, DedicatedBytes: 4 * 1024 * mib},
	}

	rows := snapshotProcessTrees().pdhRowsToRoots(instances)
	got := attributePDHDedicated(rows, luidToIndex, []int{100, 300})
	want := map[int]map[int]int{
		100: {0: 8 * 1024, 2: 21 * 1024},
		300: {1: 4 * 1024},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("attributed = %v, want %v", got, want)
	}
	if instances[0].PID != 101 {
		t.Fatal("pdhRowsToRoots rewrote its input slice")
	}

	unregister()
	rows = snapshotProcessTrees().pdhRowsToRoots(instances)
	got = attributePDHDedicated(rows, luidToIndex, []int{100, 300})
	if want := map[int]map[int]int{300: {1: 4 * 1024}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("attributed after unregister = %v, want %v", got, want)
	}
}

func TestProcessTreeComputeAppsAttributeToRoot(t *testing.T) {
	unregister := registerTestTree(t, 100, 100, 101, 102)
	uuidToIndex := map[string]int{"GPU-0": 0, "GPU-1": 1, "GPU-2": 2}
	rows := []nvidiaComputeAppRow{
		{PID: 101, GPUUUID: "GPU-0", UsedMemoryMB: 8 * 1024},
		{PID: 101, GPUUUID: "GPU-2", UsedMemoryMB: 20 * 1024},
		{PID: 102, GPUUUID: "GPU-2", UsedMemoryMB: 1 * 1024},
		{PID: 300, GPUUUID: "GPU-1", UsedMemoryMB: 4 * 1024},
	}

	got := attributeComputeApps(snapshotProcessTrees().computeAppsToRoots(slices.Clone(rows)), uuidToIndex, []int{100, 300})
	want := map[int]map[int]int{
		100: {0: 8 * 1024, 2: 21 * 1024},
		300: {1: 4 * 1024},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("attributed = %v, want %v", got, want)
	}

	unregister()
	got = attributeComputeApps(snapshotProcessTrees().computeAppsToRoots(slices.Clone(rows)), uuidToIndex, []int{100, 300})
	if want := map[int]map[int]int{300: {1: 4 * 1024}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("attributed after unregister = %v, want %v", got, want)
	}
}

func TestProcessTreeEmptySnapshotIsANoOp(t *testing.T) {
	var snap processTreeSnapshot
	instances := []pdhProcessMemory{{PID: 7, DedicatedBytes: 1}}
	if got := snap.pdhRowsToRoots(instances); !reflect.DeepEqual(got, instances) {
		t.Fatalf("pdhRowsToRoots on an empty snapshot = %v, want the input", got)
	}
	if root, ok := snap.rootOf(7); ok {
		t.Fatalf("rootOf on an empty snapshot = %d, true", root)
	}
}

// A members func is a live job query that may run while its tree is being
// unregistered; the registry must neither race nor hold its lock across the
// call. Run with -race.
func TestProcessTreeRegistryIsSafeForConcurrentUse(t *testing.T) {
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for j := range 200 {
				root := 10000 + i*1000 + j
				unregister := RegisterProcessTree(root, func() []int {
					// A members func that takes the registry lock
					// itself would deadlock if the snapshot held the
					// lock across the call.
					RegisterProcessTree(root+700, func() []int { return nil })()
					return []int{root, root + 500}
				})
				_ = snapshotProcessTrees()
				unregister()
			}
		})
	}
	wg.Wait()
}
