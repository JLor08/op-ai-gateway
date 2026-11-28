// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"bytes"
	"reflect"
	"testing"
)

var amdIdx = map[pciAddr]int{{0, 1, 0, 0}: 0, {0, 2, 0, 0}: 1}

// splitFdinfoBlocks splits a fixture on "== <fd target>" lines.
func splitFdinfoBlocks(data []byte) [][]byte {
	var blocks [][]byte
	var cur []byte
	started := false
	for _, line := range bytes.SplitAfter(data, []byte("\n")) {
		if bytes.HasPrefix(line, []byte("== ")) {
			if started {
				blocks = append(blocks, cur)
			}
			cur, started = nil, true
			continue
		}
		cur = append(cur, line...)
	}
	if started {
		blocks = append(blocks, cur)
	}
	return blocks
}

func clientsOf(t *testing.T, name string) []drmClient {
	t.Helper()
	var out []drmClient
	for _, b := range splitFdinfoBlocks([]byte(readTestdata(t, name))) {
		if c, ok := parseDRMFdinfo(b); ok {
			out = append(out, c)
		}
	}
	return out
}

func TestFdinfoHIPRunMeasuresTheModelCard(t *testing.T) {
	sums, anyKey := sumDRMClients(clientsOf(t, "fdinfo-amdgpu-hip.txt"))
	if !anyKey {
		t.Fatal("vram key not seen")
	}
	if got := drmVRAMToMiB(sums, amdIdx); !reflect.DeepEqual(got, map[int]int{1: 3461}) {
		t.Fatal(got)
	}
}

func TestFdinfoVulkanRunCountsDuplicatedClientOnce(t *testing.T) {
	sums, _ := sumDRMClients(clientsOf(t, "fdinfo-amdgpu-vulkan.txt"))
	if sums[pciAddr{0, 2, 0, 0}] != 3402924 || sums[pciAddr{0, 1, 0, 0}] != 28 {
		t.Fatalf("one client, two fds per node: %v", sums)
	}
	if got := drmVRAMToMiB(sums, amdIdx); !reflect.DeepEqual(got, map[int]int{1: 3323}) {
		t.Fatalf("got %v, want 3323 (not 6646)", got)
	}
}

func TestSumDRMClientsDropsStrayCardBelowFloor(t *testing.T) {
	sums, _ := sumDRMClients(clientsOf(t, "fdinfo-amdgpu-vulkan.txt"))
	if _, ok := drmVRAMToMiB(sums, amdIdx)[0]; ok {
		t.Fatal("28 KiB on card 0 became a measurement")
	}
}

func TestDRMVRAMDropsUnmappedPDev(t *testing.T) {
	if got := drmVRAMToMiB(map[pciAddr]uint64{{0, 9, 0, 0}: 4 << 20}, amdIdx); got != nil {
		t.Fatal(got)
	}
}

func TestParseDRMFdinfoWithoutVRAMKey(t *testing.T) {
	c, ok := parseDRMFdinfo(splitFdinfoBlocks([]byte(readTestdata(t, "fdinfo-amdgpu-novram.txt")))[0])
	if !ok || c.HasVRAM {
		t.Fatal(c, ok)
	}
}

func TestParseDRMFdinfoIgnoresOtherDrivers(t *testing.T) {
	if _, ok := parseDRMFdinfo([]byte("drm-driver:\ti915\ndrm-client-id:\t3\ndrm-pdev:\t0000:00:02.0\n")); ok {
		t.Fatal("i915 accepted")
	}
}

func TestParseDRMFdinfoUnits(t *testing.T) {
	c, _ := parseDRMFdinfo([]byte("drm-driver:\tamdgpu\ndrm-client-id:\t1\ndrm-pdev:\t0000:02:00.0\ndrm-memory-vram:\t3 MiB\n"))
	if c.VRAMKiB != 3072 {
		t.Fatal(c)
	}
}

func TestParseDRMFdinfoMalformedVRAMIsNotZero(t *testing.T) {
	c, ok := parseDRMFdinfo([]byte("drm-driver:\tamdgpu\ndrm-client-id:\t1\ndrm-pdev:\t0000:02:00.0\ndrm-memory-vram:\tlots KiB\n"))
	if !ok || c.HasVRAM {
		t.Fatal(c, ok)
	}
}
