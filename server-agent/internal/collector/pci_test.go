// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"op-ai-server-agent/internal/sample"
	"reflect"
	"testing"
)

func TestParsePCIAddr(t *testing.T) {
	for in, want := range map[string]pciAddr{
		"0000:02:00.0": {0, 2, 0, 0}, "00000000:65:00.0": {0, 0x65, 0, 0}, "0001:C1:1f.7": {1, 0xc1, 0x1f, 7},
	} {
		if got, ok := parsePCIAddr(in); !ok || got != want {
			t.Errorf("%s: %+v %v", in, got, ok)
		}
	}
	for _, bad := range []string{"", "[N/A]", "02:00.0", "0000:02:00"} {
		if _, ok := parsePCIAddr(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestPCIAddrString(t *testing.T) {
	if got := (pciAddr{0, 2, 0, 0}).String(); got != "0000:02:00.0" {
		t.Fatal(got)
	}
}

func TestDecodeKFDLocation(t *testing.T) {
	if decodeKFDLocation(0, 256) != (pciAddr{0, 1, 0, 0}) || decodeKFDLocation(0, 512) != (pciAddr{0, 2, 0, 0}) || decodeKFDLocation(0, 0x0a0b) != (pciAddr{0, 0x0a, 1, 3}) {
		t.Fatal()
	}
}

func TestKFDPCIRanksFromTopology(t *testing.T) {
	ranks, err := kfdPCIRanks(writeKFDTopology(t, readTestdata(t, "kfd-topology-2gpu.txt")))
	if err != nil || ranks[pciAddr{0, 1, 0, 0}] != 0 || ranks[pciAddr{0, 2, 0, 0}] != 1 || len(ranks) != 2 {
		t.Fatal(ranks, err)
	}
}

func TestKFDPCIRanksSkippedNodeKeepsRank(t *testing.T) {
	root := t.TempDir()
	writeKFDNode(t, root, "1", "111") // no properties: no PCI address, still rank 0
	writeKFDNode(t, root, "2", "222")
	writeKFDFile(t, root+"/topology/nodes/2/properties", "location_id 512\ndomain 0\n")
	ranks, err := kfdPCIRanks(root)
	if err != nil || len(ranks) != 1 || ranks[pciAddr{0, 2, 0, 0}] != 1 {
		t.Fatal(ranks, err)
	}
}

func TestApplyKFDRanksReordersByPCI(t *testing.T) {
	// rocm-smi listed bus 2 as card0 and bus 1 as card1 (an enumeration that disagrees with KFD)
	in := []sample.GPU{{Index: 0, PCIBusID: "0000:02:00.0"}, {Index: 1, PCIBusID: "0000:01:00.0"}}
	out, ok := applyKFDRanks(in, map[pciAddr]int{{0, 1, 0, 0}: 0, {0, 2, 0, 0}: 1})
	if !ok || out[0].Index != 1 || out[1].Index != 0 {
		t.Fatal(out, ok)
	}
}

func TestApplyKFDRanksAllOrNothing(t *testing.T) {
	in := []sample.GPU{{Index: 0, PCIBusID: "0000:02:00.0"}, {Index: 1, PCIBusID: ""}}
	if out, ok := applyKFDRanks(in, map[pciAddr]int{{0, 2, 0, 0}: 1}); ok || out[0].Index != 0 || out[1].Index != 1 {
		t.Fatal(out, ok)
	}
}

func TestAMDIndexPublishedOnlyOnCompleteJoin(t *testing.T) {
	ranks := map[pciAddr]int{{0, 1, 0, 0}: 0, {0, 2, 0, 0}: 1}
	joinAMDIndex([]sample.GPU{{Index: 0, PCIBusID: "0000:01:00.0"}, {Index: 1, PCIBusID: "0000:02:00.0"}}, ranks)
	if !reflect.DeepEqual(currentAMDIndex(), ranks) {
		t.Fatal(currentAMDIndex())
	}
	joinAMDIndex([]sample.GPU{{Index: 0, PCIBusID: ""}}, ranks) // incomplete join: the map is unpublished
	if currentAMDIndex() != nil {
		t.Fatal("an incomplete join must unpublish the map")
	}
	joinAMDIndex([]sample.GPU{{Index: 0, PCIBusID: "0000:01:00.0"}}, ranks)
	joinAMDIndex([]sample.GPU{{Index: 0, PCIBusID: "0000:01:00.0"}}, nil) // topology unreadable
	if currentAMDIndex() != nil {
		t.Fatal("an unreadable topology must unpublish the map")
	}
}
