// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build windows

package collector

import (
	"context"
	"op-ai-server-agent/internal/sample"

	"github.com/yusufpapurcu/wmi"
)

// platformHardware queries WMI (no admin) for mainboard/BIOS/DIMMs. Each query is
// best-effort: an error leaves that section zero-valued. No serial/UUID column is
// selected (privacy D4).
func platformHardware(_ context.Context) (sample.Mainboard, sample.BIOS, []sample.MemoryModule) {
	var bb []win32BaseBoard
	_ = wmi.Query("SELECT Manufacturer, Product, Version FROM Win32_BaseBoard", &bb)
	var bi []win32BIOS
	_ = wmi.Query("SELECT Manufacturer, SMBIOSBIOSVersion FROM Win32_BIOS", &bi)
	var pm []win32PhysicalMemory
	_ = wmi.Query("SELECT Capacity, Speed, SMBIOSMemoryType, DeviceLocator FROM Win32_PhysicalMemory", &pm)

	var b0 win32BaseBoard
	if len(bb) > 0 {
		b0 = bb[0]
	}
	var i0 win32BIOS
	if len(bi) > 0 {
		i0 = bi[0]
	}
	board, bios := mapWin32Board(b0, i0)
	return board, bios, mapWin32Memory(pm)
}
