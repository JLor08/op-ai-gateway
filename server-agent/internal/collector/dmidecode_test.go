// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import "testing"

const dmidecodeFixture = `# dmidecode 3.5
Handle 0x0040, DMI type 17, 40 bytes
Memory Device
	Size: 16384 MB
	Locator: DIMM 0
	Type: DDR4
	Speed: 3200 MT/s
Handle 0x0041, DMI type 17, 40 bytes
Memory Device
	Size: No Module Installed
	Locator: DIMM 1
	Type: Unknown
`

func TestParseDmidecodeMemory(t *testing.T) {
	mods := parseDmidecodeMemory([]byte(dmidecodeFixture))
	if len(mods) != 1 {
		t.Fatalf("modules = %d, want 1 (empty slot skipped)", len(mods))
	}
	m := mods[0]
	if m.Locator != "DIMM 0" || m.Type != "DDR4" || m.SpeedMHz != 3200 || m.SizeBytes != 16384*1024*1024 {
		t.Fatalf("mods[0] = %#v", m)
	}
}
