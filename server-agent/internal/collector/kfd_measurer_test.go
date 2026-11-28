// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// The fake trees below mirror a real 2x RX 6900 XT host: node 0 is the CPU
// (gpu_id 0), nodes 1 and 2 are the GPUs, and each pid directory holds a
// vram_<gpu_id> file per GPU next to unrelated KFD files.
const (
	kfdTestGPU0 = 59699
	kfdTestGPU1 = 19306

	kfdTestPID4B   = 12834
	kfdTestPID12B  = 13286
	kfdTestBytes4B = 3949375488
	kfdTestBytes12 = 9405710336
)

func writeKFDFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeKFDNode(t *testing.T, root, node, gpuID string) {
	t.Helper()
	writeKFDFile(t, filepath.Join(root, "topology", "nodes", node, "gpu_id"), gpuID+"\n")
}

func writeKFDVRAM(t *testing.T, root string, pid int, gpuID uint64, bytes int64) {
	t.Helper()
	name := "vram_" + strconv.FormatUint(gpuID, 10)
	writeKFDFile(t, filepath.Join(root, "proc", strconv.Itoa(pid), name), strconv.FormatInt(bytes, 10)+"\n")
}

// fakeKFDTree builds the two-GPU host with the 4B model on the first GPU and
// the 12B model on the second.
func fakeKFDTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeKFDNode(t, root, "0", "0")
	writeKFDNode(t, root, "1", strconv.Itoa(kfdTestGPU0))
	writeKFDNode(t, root, "2", strconv.Itoa(kfdTestGPU1))
	writeKFDVRAM(t, root, kfdTestPID4B, kfdTestGPU0, kfdTestBytes4B)
	writeKFDVRAM(t, root, kfdTestPID4B, kfdTestGPU1, 0)
	writeKFDVRAM(t, root, kfdTestPID12B, kfdTestGPU1, kfdTestBytes12)
	writeKFDFile(t, filepath.Join(root, "proc", strconv.Itoa(kfdTestPID12B), "sdma_19306"), "123\n")
	writeKFDFile(t, filepath.Join(root, "proc", strconv.Itoa(kfdTestPID12B), "pasid"), "32769\n")
	if err := os.MkdirAll(filepath.Join(root, "proc", strconv.Itoa(kfdTestPID12B), "queues"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func identityGroup(pid int) (int, bool) { return pid, true }

func TestKFDGPUOrdinalsRankGPUNodesNumerically(t *testing.T) {
	root := t.TempDir()
	writeKFDNode(t, root, "0", "0")
	writeKFDNode(t, root, "1", strconv.Itoa(kfdTestGPU0))
	writeKFDNode(t, root, "2", strconv.Itoa(kfdTestGPU1))
	writeKFDNode(t, root, "10", "777")
	writeKFDNode(t, root, "3", "garbage")
	writeKFDFile(t, filepath.Join(root, "topology", "nodes", "notanode", "gpu_id"), "4242\n")
	if err := os.MkdirAll(filepath.Join(root, "topology", "nodes", "4"), 0o755); err != nil {
		t.Fatal(err) // node without a gpu_id file: unreadable, skipped
	}

	got, err := kfdGPUOrdinals(root)
	if err != nil {
		t.Fatalf("kfdGPUOrdinals: %v", err)
	}
	want := map[uint64]int{kfdTestGPU0: 0, kfdTestGPU1: 1, 777: 2}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("kfdGPUOrdinals = %v, want %v (node 10 ranks after node 2: numeric, not lexical)", got, want)
	}
}

func TestKFDGPUOrdinalsNoGPUIsEmptyNotError(t *testing.T) {
	root := t.TempDir()
	writeKFDNode(t, root, "0", "0")
	got, err := kfdGPUOrdinals(root)
	if err != nil {
		t.Fatalf("kfdGPUOrdinals: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("kfdGPUOrdinals = %v, want empty", got)
	}
}

func TestKFDGPUOrdinalsMissingNodesDirIsError(t *testing.T) {
	if _, err := kfdGPUOrdinals(t.TempDir()); err == nil {
		t.Fatal("kfdGPUOrdinals on a tree without topology/nodes: err = nil, want an error")
	}
}

func TestKFDProcessVRAMReadsVRAMFilesOnly(t *testing.T) {
	root := fakeKFDTree(t)
	writeKFDFile(t, filepath.Join(root, "proc", "self-ish", "vram_59699"), "1048576\n")
	writeKFDFile(t, filepath.Join(root, "proc", strconv.Itoa(kfdTestPID4B), "vram_notanumber"), "1048576\n")

	got, err := kfdProcessVRAM(root)
	if err != nil {
		t.Fatalf("kfdProcessVRAM: %v", err)
	}
	want := map[int]map[uint64]int64{
		kfdTestPID4B:  {kfdTestGPU0: kfdTestBytes4B, kfdTestGPU1: 0},
		kfdTestPID12B: {kfdTestGPU1: kfdTestBytes12},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("kfdProcessVRAM = %v, want %v", got, want)
	}
}

func TestKFDProcessVRAMMissingProcDirIsError(t *testing.T) {
	if _, err := kfdProcessVRAM(t.TempDir()); err == nil {
		t.Fatal("kfdProcessVRAM on a tree without proc: err = nil, want an error")
	}
}

func TestAttributeKFD(t *testing.T) {
	ordinals := map[uint64]int{kfdTestGPU0: 0, kfdTestGPU1: 1}
	mib4B := int(int64(kfdTestBytes4B) >> 20)
	mib12B := int(int64(kfdTestBytes12) >> 20)
	if mib4B != 3766 || mib12B != 8969 {
		t.Fatalf("fixture arithmetic drifted: %d, %d", mib4B, mib12B)
	}
	const worker = 20001
	const workerBytes = 512 << 20

	tests := []struct {
		name    string
		perPID  map[int]map[uint64]int64
		pids    []int
		groupOf func(int) (int, bool)
		want    map[int]map[int]int
	}{
		{
			name: "identity groups map to ordinals",
			perPID: map[int]map[uint64]int64{
				kfdTestPID4B:  {kfdTestGPU0: kfdTestBytes4B, kfdTestGPU1: 0},
				kfdTestPID12B: {kfdTestGPU1: kfdTestBytes12},
			},
			pids:    []int{kfdTestPID4B, kfdTestPID12B},
			groupOf: identityGroup,
			want:    map[int]map[int]int{kfdTestPID4B: {0: mib4B}, kfdTestPID12B: {1: mib12B}},
		},
		{
			name: "unwanted pid excluded",
			perPID: map[int]map[uint64]int64{
				kfdTestPID4B:  {kfdTestGPU0: kfdTestBytes4B},
				kfdTestPID12B: {kfdTestGPU1: kfdTestBytes12},
			},
			pids:    []int{kfdTestPID12B},
			groupOf: identityGroup,
			want:    map[int]map[int]int{kfdTestPID12B: {1: mib12B}},
		},
		{
			name: "worker summed into its group owner before MiB conversion",
			perPID: map[int]map[uint64]int64{
				kfdTestPID12B: {kfdTestGPU1: kfdTestBytes12},
				worker:        {kfdTestGPU1: workerBytes},
			},
			pids: []int{kfdTestPID12B},
			groupOf: func(pid int) (int, bool) {
				if pid == worker {
					return kfdTestPID12B, true
				}
				return pid, true
			},
			want: map[int]map[int]int{kfdTestPID12B: {1: int((int64(kfdTestBytes12) + workerBytes) >> 20)}},
		},
		{
			name: "unknown group skipped",
			perPID: map[int]map[uint64]int64{
				kfdTestPID4B:  {kfdTestGPU0: kfdTestBytes4B},
				kfdTestPID12B: {kfdTestGPU1: kfdTestBytes12},
			},
			pids: []int{kfdTestPID4B, kfdTestPID12B},
			groupOf: func(pid int) (int, bool) {
				return pid, pid != kfdTestPID4B
			},
			want: map[int]map[int]int{kfdTestPID12B: {1: mib12B}},
		},
		{
			name: "unknown gpu_id skipped",
			perPID: map[int]map[uint64]int64{
				kfdTestPID4B: {kfdTestGPU0: kfdTestBytes4B, 777: kfdTestBytes12},
			},
			pids:    []int{kfdTestPID4B},
			groupOf: identityGroup,
			want:    map[int]map[int]int{kfdTestPID4B: {0: mib4B}},
		},
		{
			name: "all zero is nil",
			perPID: map[int]map[uint64]int64{
				kfdTestPID4B: {kfdTestGPU0: 0, kfdTestGPU1: 0},
			},
			pids:    []int{kfdTestPID4B},
			groupOf: identityGroup,
			want:    nil,
		},
		{
			name: "sub-MiB dropped",
			perPID: map[int]map[uint64]int64{
				kfdTestPID4B:  {kfdTestGPU0: (1 << 20) - 1},
				kfdTestPID12B: {kfdTestGPU1: kfdTestBytes12},
			},
			pids:    []int{kfdTestPID4B, kfdTestPID12B},
			groupOf: identityGroup,
			want:    map[int]map[int]int{kfdTestPID12B: {1: mib12B}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := attributeKFD(tt.perPID, ordinals, tt.pids, tt.groupOf)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("attributeKFD = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestKFDMeasurerEndToEnd(t *testing.T) {
	m := kfdMeasurer{root: fakeKFDTree(t), groupOf: identityGroup}
	got := m.measure([]int{kfdTestPID4B, kfdTestPID12B})
	want := map[int]map[int]int{
		kfdTestPID4B:  {0: int(int64(kfdTestBytes4B) >> 20)},
		kfdTestPID12B: {1: int(int64(kfdTestBytes12) >> 20)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("measure = %v, want %v", got, want)
	}
}

func TestKFDMeasurerEmptyPIDsIsNil(t *testing.T) {
	m := kfdMeasurer{root: fakeKFDTree(t), groupOf: identityGroup}
	if got := m.measure(nil); got != nil {
		t.Fatalf("measure(nil) = %v, want nil", got)
	}
}

func TestKFDMeasurerBrokenRootIsNil(t *testing.T) {
	m := kfdMeasurer{root: filepath.Join(t.TempDir(), "missing"), groupOf: identityGroup}
	if got := m.measure([]int{kfdTestPID4B}); got != nil {
		t.Fatalf("measure on a missing root = %v, want nil", got)
	}

	noProc := t.TempDir()
	writeKFDNode(t, noProc, "1", strconv.Itoa(kfdTestGPU0))
	m = kfdMeasurer{root: noProc, groupOf: identityGroup}
	if got := m.measure([]int{kfdTestPID4B}); got != nil {
		t.Fatalf("measure on a tree without proc = %v, want nil", got)
	}
}

func readTestdata(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// writeKFDTopology materialises a "== <node>" block file (bare gpu_id line,
// then the properties lines) under root/topology/nodes/<n>/{gpu_id,properties}.
func writeKFDTopology(t *testing.T, text string) string {
	t.Helper()
	root := t.TempDir()
	for _, block := range strings.Split(text, "== ")[1:] {
		lines := strings.Split(strings.TrimRight(block, "\n"), "\n")
		node := strings.TrimSpace(lines[0])
		writeKFDFile(t, filepath.Join(root, "topology", "nodes", node, "gpu_id"), strings.TrimSpace(lines[1])+"\n")
		writeKFDFile(t, filepath.Join(root, "topology", "nodes", node, "properties"), strings.Join(lines[2:], "\n")+"\n")
	}
	return root
}
