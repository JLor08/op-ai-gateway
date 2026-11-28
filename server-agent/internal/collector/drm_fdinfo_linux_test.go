// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build linux

package collector

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
)

// fakeProc is a /proc-shaped tree in a temp dir: <pid>/fd/<n> symlinks and
// <pid>/fdinfo/<n> files, the two things readDRMClients reads.
type fakeProc struct {
	t    *testing.T
	root string
}

func newFakeProc(t *testing.T) fakeProc {
	t.Helper()
	root := t.TempDir()
	// Non-numeric /proc entries exist on every host and must be skipped.
	for _, d := range []string{"self", "sys"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return fakeProc{t: t, root: root}
}

// fd adds fd n of pid pointing at target; info, when non-nil, is its fdinfo.
func (p fakeProc) fd(pid, n int, target string, info []byte) {
	p.t.Helper()
	dir := filepath.Join(p.root, strconv.Itoa(pid))
	for _, sub := range []string{"fd", "fdinfo"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			p.t.Fatal(err)
		}
	}
	if err := os.Symlink(target, filepath.Join(dir, "fd", strconv.Itoa(n))); err != nil {
		p.t.Fatal(err)
	}
	if info != nil {
		if err := os.WriteFile(filepath.Join(dir, "fdinfo", strconv.Itoa(n)), info, 0o644); err != nil {
			p.t.Fatal(err)
		}
	}
}

// vulkanBlocks returns the fixture's fdinfo blocks: [0] and [1] are client
// 28698 on 0000:02:00.0 (3402924 KiB, two fds sharing one client), [2] is
// client 28699 on 0000:01:00.0 (28 KiB, a card the process merely opened).
func vulkanBlocks(t *testing.T) [][]byte {
	t.Helper()
	b := splitFdinfoBlocks([]byte(readTestdata(t, "fdinfo-amdgpu-vulkan.txt")))
	if len(b) < 3 {
		t.Fatalf("fixture has %d blocks", len(b))
	}
	return b
}

// bigClient is an amdgpu client distinct from the fixture's, on the model
// card, large enough that counting it by mistake is unmissable.
var bigClient = []byte("drm-driver:\tamdgpu\ndrm-client-id:\t99999\ndrm-pdev:\t0000:02:00.0\ndrm-memory-vram:\t5000000 KiB\n")

var fakeGroups = map[int]int{100: 100, 101: 100, 300: 300}

func fakeGroupOf(pid int) (int, bool) {
	g, ok := fakeGroups[pid]
	return g, ok
}

func fakeIndex() map[pciAddr]int { return amdIdx }

func TestFdinfoMeasurerSumsAcrossGroupPidsOnce(t *testing.T) {
	b := vulkanBlocks(t)
	p := newFakeProc(t)
	p.fd(100, 3, "/dev/dri/renderD129", b[0])
	p.fd(100, 4, "/dev/dri/renderD128", b[2])      // 28 KiB: below the noise floor
	p.fd(101, 5, "/dev/dri/renderD129", b[1])      // the worker holds the same client
	p.fd(300, 6, "/dev/dri/renderD129", bigClient) // another group, not asked about
	m := fdinfoMeasurer{procRoot: p.root, index: fakeIndex, groupOf: fakeGroupOf}
	if got, want := m.measure([]int{100}), (map[int]map[int]int{100: {1: 3323}}); !reflect.DeepEqual(got, want) {
		t.Fatalf("measure = %v, want %v", got, want)
	}
}

func TestFdinfoMeasurerUnreadablePidIsNoMeasurement(t *testing.T) {
	b := vulkanBlocks(t)
	p := newFakeProc(t)
	p.fd(100, 3, "/dev/dri/renderD129", nil) // fdinfo gone (the process exited mid-scan, or no access)
	m := fdinfoMeasurer{procRoot: p.root, index: fakeIndex, groupOf: fakeGroupOf}
	if got := m.measure([]int{100}); got != nil {
		t.Fatalf("measure = %v, want nil (no measurement, never a measured 0)", got)
	}

	if os.Geteuid() == 0 {
		t.Log("running as root: mode 000 does not deny root, the missing-fdinfo case above stands for it")
		return
	}
	p.fd(101, 5, "/dev/dri/renderD129", b[0])
	if err := os.Chmod(filepath.Join(p.root, "101", "fdinfo", "5"), 0); err != nil {
		t.Fatal(err)
	}
	if got := m.measure([]int{100}); got != nil {
		t.Fatalf("measure with an unreadable fdinfo = %v, want nil", got)
	}
}

func TestFdinfoMeasurerSkipsNonDRMFds(t *testing.T) {
	b := vulkanBlocks(t)
	p := newFakeProc(t)
	p.fd(100, 3, "/dev/dri/renderD129", b[0])
	p.fd(100, 7, "socket:[123]", bigClient)             // never read: not a DRM fd
	p.fd(100, 8, "/home/user/models/x.gguf", bigClient) // nor this
	p.fd(100, 9, "/dev/kfd", bigClient)                 // nor KFD, which is never added to fdinfo
	m := fdinfoMeasurer{procRoot: p.root, index: fakeIndex, groupOf: fakeGroupOf}
	if got, want := m.measure([]int{100}), (map[int]map[int]int{100: {1: 3323}}); !reflect.DeepEqual(got, want) {
		t.Fatalf("measure = %v, want %v", got, want)
	}
}

func TestFdinfoMeasurerWithoutJoinMeasuresNothing(t *testing.T) {
	b := vulkanBlocks(t)
	p := newFakeProc(t)
	p.fd(100, 3, "/dev/dri/renderD129", b[0])
	m := fdinfoMeasurer{procRoot: p.root, index: func() map[pciAddr]int { return nil }, groupOf: fakeGroupOf}
	if got := m.measure([]int{100}); got != nil {
		t.Fatalf("measure without a join = %v, want nil", got)
	}
	// A join that does not cover the card drops it rather than guessing.
	m.index = func() map[pciAddr]int { return map[pciAddr]int{{0, 1, 0, 0}: 0} }
	if got := m.measure([]int{100}); got != nil {
		t.Fatalf("measure with the card unmapped = %v, want nil", got)
	}
	if got := (fdinfoMeasurer{procRoot: p.root, index: fakeIndex, groupOf: fakeGroupOf}).measure(nil); got != nil {
		t.Fatalf("measure(no pids) = %v, want nil", got)
	}
}

func TestScanGroupMembers(t *testing.T) {
	p := newFakeProc(t)
	for _, pid := range []int{100, 101, 300, 400} { // 400 has no group (exited)
		p.fd(pid, 3, "/dev/null", nil)
	}
	got := scanGroupMembers(p.root, map[int]bool{100: true}, fakeGroupOf)
	if want := (map[int][]int{100: {100, 101}}); !reflect.DeepEqual(got, want) {
		t.Fatalf("scanGroupMembers = %v, want %v", got, want)
	}
}

// TestAMDGPURenderNodes pins the self-probe's node choice: only render nodes
// whose driver is amdgpu, in node order.
func TestAMDGPURenderNodes(t *testing.T) {
	dev, sys := t.TempDir(), t.TempDir()
	for node, driver := range map[string]string{"renderD128": "nvidia", "renderD129": "amdgpu", "renderD130": "amdgpu", "renderD131": ""} {
		if err := os.WriteFile(filepath.Join(dev, node), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if driver == "" {
			continue // no driver link: skipped
		}
		dir := filepath.Join(sys, node, "device")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../../../bus/pci/drivers/"+driver, filepath.Join(dir, "driver")); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dev, "card0"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got := amdgpuRenderNodes(dev, sys)
	if want := []string{filepath.Join(dev, "renderD129"), filepath.Join(dev, "renderD130")}; !reflect.DeepEqual(got, want) {
		t.Fatalf("amdgpuRenderNodes = %v, want %v", got, want)
	}
}

// TestProbeRenderNodeOnANonDRMFile: a file whose own fdinfo carries no amdgpu
// keys is opened (and closed) but does not count as fdinfo support.
func TestProbeRenderNodeOnANonDRMFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "renderD129")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, opened := probeRenderNode(f); ok || !opened {
		t.Fatalf("probeRenderNode(regular file) = %v, %v; want false, true", ok, opened)
	}
	if ok, opened := probeRenderNode(filepath.Join(t.TempDir(), "missing")); ok || opened {
		t.Fatalf("probeRenderNode(missing) = %v, %v; want false, false", ok, opened)
	}
}
