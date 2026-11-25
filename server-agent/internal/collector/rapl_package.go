// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"path/filepath"
	"strings"
	"time"
)

// PackageEnergyReader returns the CPU package energy consumed (in joules) between
// consecutive DeltaJoules calls, summed over every intel-rapl:* domain whose name
// starts with "package" (multi-socket). It exports the same reading logic the
// raplCollector uses (shared helpers, wraparound-corrected via max_energy_range_uj)
// so the hostpower mode can consume the host's package energy per tick without
// duplicating it. Pure filesystem + time, so it is testable on any OS by pointing
// powercapRoot at a synthetic tree.
//
// Stateful: it holds the previous per-domain counter. It has a single caller and
// is therefore NOT goroutine-safe (no mutex).
type PackageEnergyReader struct {
	powercapRoot string
	prev         map[string]raplReading // keyed by domain dir path
}

// NewPackageEnergyReader builds a reader over the given powercap sysfs root
// (normally /sys/class/powercap).
func NewPackageEnergyReader(powercapRoot string) *PackageEnergyReader {
	return &PackageEnergyReader{
		powercapRoot: powercapRoot,
		prev:         map[string]raplReading{},
	}
}

// DeltaJoules returns the summed package energy delta since the previous call.
// ok is false on the first call (no previous reading) and whenever no package
// domain yielded a delta (none present, or energy_uj unreadable, e.g. EACCES when
// not root). Domains without a previous reading (newly readable) are skipped for
// that call and contribute from the next one.
func (r *PackageEnergyReader) DeltaJoules() (joules float64, ok bool) {
	now := time.Now()
	domains, _ := filepath.Glob(filepath.Join(r.powercapRoot, "intel-rapl:*"))
	for _, dir := range domains {
		name := strings.TrimSpace(readSysfsString(filepath.Join(dir, "name")))
		if !strings.HasPrefix(name, "package") {
			continue // core/dram/uncore subdomains are subsets of the package
		}
		energy, readable := readSysfsUint(filepath.Join(dir, "energy_uj"))
		if !readable {
			continue
		}
		maxUJ, _ := readSysfsUint(filepath.Join(dir, "max_energy_range_uj"))
		prev, seen := r.prev[dir]
		r.prev[dir] = raplReading{energyUJ: energy, maxUJ: maxUJ, at: now}
		if !seen {
			continue
		}
		joules += raplDeltaJoules(prev.energyUJ, energy, prev.maxUJ)
		ok = true
	}
	return joules, ok
}
