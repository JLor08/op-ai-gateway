// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import "testing"

const spHardwareFixture = `{"SPHardwareDataType":[{"machine_model":"Mac15,9","machine_name":"MacBook Pro","boot_rom_version":"11881.1.1"}]}`

const spMemoryIntelFixture = `{"SPMemoryDataType":[{"_items":[{"_name":"BANK 0/ChannelA-DIMM0","dimm_size":"16 GB","dimm_type":"DDR4","dimm_speed":"2667 MHz"},{"_name":"BANK 1/ChannelB-DIMM0","dimm_size":"16 GB","dimm_type":"DDR4","dimm_speed":"2667 MHz"}]}]}`

const spMemoryAppleFixture = `{"SPMemoryDataType":[{"dimm_type":"unified","SPMemoryDataType":"36 GB"}]}`

func TestParseSPHardware(t *testing.T) {
	board, bios := parseSPHardware([]byte(spHardwareFixture))
	if board.Vendor != "Apple Inc." || board.Product != "Mac15,9" {
		t.Fatalf("board = %#v", board)
	}
	if bios.Vendor != "Apple Inc." || bios.Version != "11881.1.1" {
		t.Fatalf("bios = %#v", bios)
	}
}

func TestParseSPMemoryIntel(t *testing.T) {
	mods := parseSPMemory([]byte(spMemoryIntelFixture))
	if len(mods) != 2 {
		t.Fatalf("modules = %d, want 2", len(mods))
	}
	if mods[0].Locator != "BANK 0/ChannelA-DIMM0" || mods[0].Type != "DDR4" || mods[0].SpeedMHz != 2667 {
		t.Fatalf("mods[0] = %#v", mods[0])
	}
	if mods[0].SizeBytes != 16*1024*1024*1024 {
		t.Fatalf("size = %d", mods[0].SizeBytes)
	}
}

func TestParseSPMemoryAppleSiliconYieldsNoModules(t *testing.T) {
	if mods := parseSPMemory([]byte(spMemoryAppleFixture)); len(mods) != 0 {
		t.Fatalf("apple silicon should have no per-DIMM modules, got %d", len(mods))
	}
}
