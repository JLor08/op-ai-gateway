// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"op-ai-server-agent/internal/sample"
	"strconv"
	"strings"
)

// parseDmidecodeMemory parses `dmidecode -t memory` output, extracting each
// populated "Memory Device" (DMI type 17) block into a MemoryModule. Empty slots
// ("Size: No Module Installed") are skipped. Pure text parsing — used only on
// Linux (root-gated in the collector), but testable on any OS.
func parseDmidecodeMemory(data []byte) []sample.MemoryModule {
	var mods []sample.MemoryModule
	var cur *sample.MemoryModule
	flush := func() {
		if cur != nil && cur.SizeBytes > 0 {
			mods = append(mods, *cur)
		}
		cur = nil
	}
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "Memory Device" {
			flush()
			cur = &sample.MemoryModule{}
			continue
		}
		if cur == nil {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		switch key {
		case "Size":
			cur.SizeBytes = parseDmidecodeSize(val)
		case "Locator":
			cur.Locator = val
		case "Type":
			if val != "Unknown" {
				cur.Type = val
			}
		case "Speed", "Configured Memory Speed", "Configured Clock Speed":
			if cur.SpeedMHz == 0 {
				cur.SpeedMHz = parseLeadingInt(val)
			}
		}
	}
	flush()
	return mods
}

// parseDmidecodeSize parses a dmidecode size like "16384 MB" / "16 GB" to bytes.
// "No Module Installed" / unparseable -> 0.
func parseDmidecodeSize(s string) int64 {
	fields := strings.Fields(s)
	if len(fields) < 2 {
		return 0
	}
	n, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return 0
	}
	switch strings.ToUpper(fields[1]) {
	case "TB":
		return n * 1024 * 1024 * 1024 * 1024
	case "GB":
		return n * 1024 * 1024 * 1024
	case "MB":
		return n * 1024 * 1024
	case "KB":
		return n * 1024
	default:
		return 0
	}
}
