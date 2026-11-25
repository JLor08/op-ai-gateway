// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"bytes"
	"encoding/binary"
	"io"
	"op-ai-server-agent/internal/sample"
	"os"
)

// DefaultDMITablePath is the raw SMBIOS structure table exported by the Linux
// kernel (readable by root only).
const DefaultDMITablePath = "/sys/firmware/dmi/tables/DMI"

// maxSMBIOSTableBytes bounds how much of the table is read; real tables are a
// few tens of KiB.
const maxSMBIOSTableBytes = 1 << 20

const (
	smbiosTypeMemoryDevice = 17
	smbiosTypeEnd          = 127
	smbiosHeaderLen        = 4
)

// smbiosMemoryTypes maps the type-17 Memory Type byte to the name dmidecode
// prints. Unlisted values yield "".
var smbiosMemoryTypes = map[byte]string{
	0x01: "Other", 0x02: "Unknown", 0x03: "DRAM", 0x04: "EDRAM", 0x05: "VRAM",
	0x06: "SRAM", 0x07: "RAM", 0x08: "ROM", 0x09: "Flash", 0x0A: "EEPROM",
	0x0B: "FEPROM", 0x0C: "EPROM", 0x0D: "CDRAM", 0x0E: "3DRAM", 0x0F: "SDRAM",
	0x10: "SGRAM", 0x11: "RDRAM", 0x12: "DDR", 0x13: "DDR2", 0x14: "DDR2 FB-DIMM",
	0x18: "DDR3", 0x19: "FBD2", 0x1A: "DDR4", 0x1B: "LPDDR", 0x1C: "LPDDR2",
	0x1D: "LPDDR3", 0x1E: "LPDDR4", 0x1F: "Logical non-volatile device",
	0x20: "HBM", 0x21: "HBM2", 0x22: "DDR5", 0x23: "LPDDR5", 0x24: "HBM3",
}

// ReadSMBIOSMemoryModules reads the raw SMBIOS table at path (at most 1 MiB)
// and returns its populated memory devices.
func ReadSMBIOSMemoryModules(path string) ([]sample.MemoryModule, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	table, err := io.ReadAll(io.LimitReader(f, maxSMBIOSTableBytes))
	if err != nil {
		return nil, err
	}
	return ParseSMBIOSMemory(table), nil
}

// ParseSMBIOSMemory walks a raw SMBIOS structure table (DSP0134) and returns
// each populated Memory Device (type 17). Empty slots and unknown sizes are
// skipped; only the locator, size, type and speed are extracted (never serial
// or part numbers). Malformed or truncated input ends the walk; modules decoded
// before the damage are returned. Never panics.
func ParseSMBIOSMemory(table []byte) []sample.MemoryModule {
	var mods []sample.MemoryModule
	for off := 0; off+smbiosHeaderLen <= len(table); {
		typ := table[off]
		length := int(table[off+1])
		if typ == smbiosTypeEnd || length < smbiosHeaderLen || off+length > len(table) {
			break
		}
		formatted := table[off : off+length]
		strs, next, ok := smbiosStringSet(table, off+length)
		if !ok {
			break
		}
		if typ == smbiosTypeMemoryDevice {
			if m, ok := decodeMemoryDevice(formatted, strs); ok {
				mods = append(mods, m)
			}
		}
		off = next
	}
	return mods
}

// smbiosStringSet reads the NUL-terminated strings starting at start, ended by
// an extra NUL (an empty set is two NULs). It returns the strings, the offset
// of the next structure, and false when the set is not terminated.
func smbiosStringSet(table []byte, start int) (strs []string, next int, ok bool) {
	if start > len(table) {
		return nil, 0, false
	}
	// Empty set: two NULs.
	if start+2 <= len(table) && table[start] == 0 && table[start+1] == 0 {
		return nil, start + 2, true
	}
	pos := start
	for pos < len(table) {
		if table[pos] == 0 {
			return strs, pos + 1, true
		}
		end := bytes.IndexByte(table[pos:], 0)
		if end < 0 {
			return nil, 0, false
		}
		strs = append(strs, string(table[pos:pos+end]))
		pos += end + 1
	}
	return nil, 0, false
}

func decodeMemoryDevice(b []byte, strs []string) (sample.MemoryModule, bool) {
	var m sample.MemoryModule
	size, ok := smbiosMemorySize(b)
	if !ok || size <= 0 {
		return m, false
	}
	m.SizeBytes = size
	if idx := smbiosByte(b, 0x10); idx > 0 && int(idx) <= len(strs) {
		m.Locator = strs[idx-1]
	}
	m.Type = smbiosMemoryTypes[smbiosByte(b, 0x12)]
	if m.Type == "Unknown" {
		m.Type = ""
	}
	// Rated speed first, configured speed as the fallback.
	m.SpeedMHz = smbiosSpeed(b, 0x15, 0x54)
	if m.SpeedMHz == 0 {
		m.SpeedMHz = smbiosSpeed(b, 0x20, 0x58)
	}
	return m, true
}

func smbiosMemorySize(b []byte) (int64, bool) {
	raw, ok := smbiosWord(b, 0x0C)
	if !ok {
		return 0, false
	}
	switch raw {
	case 0, 0xFFFF:
		return 0, false
	case 0x7FFF:
		ext, ok := smbiosDword(b, 0x1C)
		if !ok {
			return 0, false
		}
		return int64(ext&0x7FFFFFFF) << 20, true
	}
	if raw&0x8000 != 0 {
		return int64(raw&0x7FFF) << 10, true
	}
	return int64(raw) << 20, true
}

// smbiosSpeed reads a speed word at wordOff, falling back to the extended
// dword at dwordOff when the word is 0xFFFF. Unknown/absent → 0.
func smbiosSpeed(b []byte, wordOff, dwordOff int) int {
	v, ok := smbiosWord(b, wordOff)
	if !ok {
		return 0
	}
	if v == 0xFFFF {
		ext, ok := smbiosDword(b, dwordOff)
		if !ok || ext > 1<<30 {
			return 0
		}
		return int(ext)
	}
	return int(v)
}

func smbiosByte(b []byte, off int) byte {
	if off >= len(b) {
		return 0
	}
	return b[off]
}

func smbiosWord(b []byte, off int) (uint16, bool) {
	if off+2 > len(b) {
		return 0, false
	}
	return binary.LittleEndian.Uint16(b[off:]), true
}

func smbiosDword(b []byte, off int) (uint32, bool) {
	if off+4 > len(b) {
		return 0, false
	}
	return binary.LittleEndian.Uint32(b[off:]), true
}
