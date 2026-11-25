// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ReadTopLevelRAPLDomains lists the top-level powercap RAPL zones under
// powercapRoot (directories named intel-rapl:N, N decimal; sub-zones such as
// intel-rapl:0:0 are excluded). A zone is included only with a non-empty name
// and a readable energy_uj; max_energy_range_uj is 0 when unreadable. The result
// is sorted by zone. A missing root yields an empty result.
func ReadTopLevelRAPLDomains(powercapRoot string) []HelperRAPLDomain {
	entries, err := os.ReadDir(powercapRoot)
	if err != nil {
		return nil
	}
	var out []HelperRAPLDomain
	for _, e := range entries {
		zone := e.Name()
		rest, ok := strings.CutPrefix(zone, "intel-rapl:")
		if !ok || !isDecimal(rest) {
			continue
		}
		dir := filepath.Join(powercapRoot, zone)
		name := strings.TrimSpace(readSysfsString(filepath.Join(dir, "name")))
		if name == "" {
			continue
		}
		energy, ok := readSysfsUint(filepath.Join(dir, "energy_uj"))
		if !ok {
			continue
		}
		maxRange, _ := readSysfsUint(filepath.Join(dir, "max_energy_range_uj"))
		out = append(out, HelperRAPLDomain{Zone: zone, Name: name, EnergyUJ: energy, MaxEnergyRangeUJ: maxRange})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Zone < out[j].Zone })
	return out
}

func isDecimal(s string) bool {
	_, err := strconv.ParseUint(s, 10, 64)
	return err == nil
}
