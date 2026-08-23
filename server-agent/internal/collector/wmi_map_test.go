// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import "testing"

func TestMapWin32BoardAndBIOS(t *testing.T) {
	board, bios := mapWin32Board(
		win32BaseBoard{Manufacturer: "MSI", Product: "PRO Z790", Version: "1.0"},
		win32BIOS{Manufacturer: "American Megatrends", SMBIOSBIOSVersion: "A.60"},
	)
	if board.Vendor != "MSI" || board.Product != "PRO Z790" || board.Version != "1.0" {
		t.Fatalf("board = %#v", board)
	}
	if bios.Vendor != "American Megatrends" || bios.Version != "A.60" {
		t.Fatalf("bios = %#v", bios)
	}
}

func TestMapWin32Memory(t *testing.T) {
	mods := mapWin32Memory([]win32PhysicalMemory{
		{Capacity: 17179869184, Speed: 3200, SMBIOSMemoryType: 0x1A, DeviceLocator: "DIMM0"},
		{Capacity: 17179869184, Speed: 5600, SMBIOSMemoryType: 0x22, DeviceLocator: "DIMM1"},
	})
	if len(mods) != 2 {
		t.Fatalf("modules = %d", len(mods))
	}
	if mods[0].SizeBytes != 17179869184 || mods[0].SpeedMHz != 3200 || mods[0].Type != "DDR4" || mods[0].Locator != "DIMM0" {
		t.Fatalf("mods[0] = %#v", mods[0])
	}
	if mods[1].Type != "DDR5" {
		t.Fatalf("mods[1].Type = %q, want DDR5", mods[1].Type)
	}
}

func TestSMBIOSMemoryType(t *testing.T) {
	if smbiosMemoryType(0x1A) != "DDR4" || smbiosMemoryType(0x22) != "DDR5" || smbiosMemoryType(0x99) != "" {
		t.Fatal("smbios type mapping wrong")
	}
}
