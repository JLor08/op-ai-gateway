// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import "op-ai-server-agent/internal/sample"

// These structs mirror the WMI class columns the Windows hwinfo collector queries.
// Kept in a build-tag-free file so the pure mappers below are unit-testable on any
// OS (only the wmi.Query wiring in hwinfo_windows.go is Windows-only). No serial or
// UUID column is ever selected (privacy D4).
type win32BaseBoard struct {
	Manufacturer string
	Product      string
	Version      string
}

type win32BIOS struct {
	Manufacturer      string
	SMBIOSBIOSVersion string
}

type win32PhysicalMemory struct {
	Capacity         uint64
	Speed            uint32
	SMBIOSMemoryType uint16
	DeviceLocator    string
}

func mapWin32Board(bb win32BaseBoard, bios win32BIOS) (sample.Mainboard, sample.BIOS) {
	return sample.Mainboard{Vendor: bb.Manufacturer, Product: bb.Product, Version: bb.Version},
		sample.BIOS{Vendor: bios.Manufacturer, Version: bios.SMBIOSBIOSVersion}
}

func mapWin32Memory(mods []win32PhysicalMemory) []sample.MemoryModule {
	out := make([]sample.MemoryModule, 0, len(mods))
	for _, m := range mods {
		out = append(out, sample.MemoryModule{
			Locator:   m.DeviceLocator,
			SizeBytes: int64(m.Capacity),
			Type:      smbiosMemoryType(m.SMBIOSMemoryType),
			SpeedMHz:  int(m.Speed),
		})
	}
	return out
}

// smbiosMemoryType maps the SMBIOS memory-type code (Win32_PhysicalMemory.
// SMBIOSMemoryType) to a human label. Unknown -> "".
func smbiosMemoryType(code uint16) string {
	switch code {
	case 0x13:
		return "DDR"
	case 0x14:
		return "DDR2"
	case 0x18:
		return "DDR3"
	case 0x1A:
		return "DDR4"
	case 0x22:
		return "DDR5"
	case 0x1D:
		return "LPDDR3"
	case 0x1E:
		return "LPDDR4"
	case 0x23:
		return "LPDDR5"
	default:
		return ""
	}
}
