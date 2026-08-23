// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"encoding/json"
	"op-ai-server-agent/internal/sample"
	"strconv"
	"strings"
)

// parseSPHardware maps `system_profiler -json SPHardwareDataType` output to the
// mainboard + BIOS sections. Apple has no motherboard-vendor concept, so vendor is
// filled as "Apple Inc." and product is the machine model. It never reads a serial
// (the serial_number field is deliberately not in the decode struct).
func parseSPHardware(data []byte) (sample.Mainboard, sample.BIOS) {
	var root struct {
		Items []struct {
			MachineModel   string `json:"machine_model"`
			MachineName    string `json:"machine_name"`
			BootRomVersion string `json:"boot_rom_version"`
		} `json:"SPHardwareDataType"`
	}
	if err := json.Unmarshal(data, &root); err != nil || len(root.Items) == 0 {
		return sample.Mainboard{}, sample.BIOS{}
	}
	it := root.Items[0]
	board := sample.Mainboard{Vendor: "Apple Inc.", Product: firstNonEmpty(it.MachineModel, it.MachineName)}
	bios := sample.BIOS{Vendor: "Apple Inc.", Version: strings.TrimSpace(it.BootRomVersion)}
	return board, bios
}

// parseSPMemory maps `system_profiler -json SPMemoryDataType` output to per-DIMM
// modules. Intel Macs expose an `_items` array of DIMMs; Apple Silicon reports
// unified memory with no `_items`, yielding no modules (total-only, per D2).
func parseSPMemory(data []byte) []sample.MemoryModule {
	var root struct {
		Items []struct {
			DIMMs []struct {
				Name  string `json:"_name"`
				Size  string `json:"dimm_size"`
				Type  string `json:"dimm_type"`
				Speed string `json:"dimm_speed"`
			} `json:"_items"`
		} `json:"SPMemoryDataType"`
	}
	if err := json.Unmarshal(data, &root); err != nil {
		return nil
	}
	var mods []sample.MemoryModule
	for _, item := range root.Items {
		for _, d := range item.DIMMs {
			mods = append(mods, sample.MemoryModule{
				Locator:   strings.TrimSpace(d.Name),
				SizeBytes: parseSizeToBytes(d.Size),
				Type:      strings.TrimSpace(d.Type),
				SpeedMHz:  parseLeadingInt(d.Speed),
			})
		}
	}
	return mods
}

// parseSizeToBytes parses a human size like "16 GB" / "8192 MB" to bytes (binary
// units, matching Apple's reporting). Unparseable -> 0.
func parseSizeToBytes(s string) int64 {
	fields := strings.Fields(strings.TrimSpace(s))
	if len(fields) < 1 {
		return 0
	}
	n, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return 0
	}
	unit := ""
	if len(fields) >= 2 {
		unit = strings.ToUpper(fields[1])
	}
	switch unit {
	case "TB":
		return n * 1024 * 1024 * 1024 * 1024
	case "GB":
		return n * 1024 * 1024 * 1024
	case "MB":
		return n * 1024 * 1024
	case "KB":
		return n * 1024
	default:
		return n
	}
}

// parseLeadingInt parses the leading integer of a string like "2667 MHz". -> 0 on
// failure.
func parseLeadingInt(s string) int {
	fields := strings.Fields(strings.TrimSpace(s))
	if len(fields) < 1 {
		return 0
	}
	n, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0
	}
	return n
}
