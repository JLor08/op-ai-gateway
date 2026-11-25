// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTopRAPLDomain(t *testing.T, root, dir, name, energy, maxRange string) string {
	t.Helper()
	d := filepath.Join(root, dir)
	noErr(t, os.MkdirAll(d, 0o755))
	if name != "" {
		noErr(t, os.WriteFile(filepath.Join(d, "name"), []byte(name+"\n"), 0o644))
	}
	if energy != "" {
		noErr(t, os.WriteFile(filepath.Join(d, "energy_uj"), []byte(energy+"\n"), 0o644))
	}
	if maxRange != "" {
		noErr(t, os.WriteFile(filepath.Join(d, "max_energy_range_uj"), []byte(maxRange+"\n"), 0o644))
	}
	return d
}

func TestReadTopLevelRAPLDomains(t *testing.T) {
	root := t.TempDir()
	writeTopRAPLDomain(t, root, "intel-rapl:1", "psys", "200", "")
	writeTopRAPLDomain(t, root, "intel-rapl:0", "package-0", "100", "1000")
	writeTopRAPLDomain(t, root, "intel-rapl:0:0", "core", "50", "500")
	writeTopRAPLDomain(t, root, "intel-rapl:x", "bogus", "1", "1")
	writeTopRAPLDomain(t, root, "intel-rapl:3", "", "7", "7") // no name

	want := []HelperRAPLDomain{
		{Zone: "intel-rapl:0", Name: "package-0", EnergyUJ: 100, MaxEnergyRangeUJ: 1000},
		{Zone: "intel-rapl:1", Name: "psys", EnergyUJ: 200, MaxEnergyRangeUJ: 0},
	}
	eq(t, want, ReadTopLevelRAPLDomains(root))

	t.Run("unreadable energy_uj is excluded", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads mode-000 files")
		}
		d := writeTopRAPLDomain(t, root, "intel-rapl:2", "dram", "300", "300")
		noErr(t, os.Chmod(filepath.Join(d, "energy_uj"), 0))
		eq(t, want, ReadTopLevelRAPLDomains(root))
	})
}

func TestReadTopLevelRAPLDomainsMissingRoot(t *testing.T) {
	empty(t, ReadTopLevelRAPLDomains(filepath.Join(t.TempDir(), "nope")))
}
