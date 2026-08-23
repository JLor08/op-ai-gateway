// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// raplReading is a domain's last-seen monotonic energy counter + when it was read.
type raplReading struct {
	energyUJ uint64
	maxUJ    uint64
	at       time.Time
}

// raplCollector derives CPU package watts and total-system watts from the Linux
// powercap RAPL sysfs energy counters (Δenergy/Δt), with a hwmon power-sensor
// fallback for system watts. It is pure filesystem + time (no linux-only syscalls),
// so it lives in a build-tag-free file and is fully testable on any OS by pointing
// powercapRoot/hwmonRoot at a synthetic tree. Stateful: it holds the previous
// per-domain reading, so the FIRST Collect returns nil (no delta yet). energy_uj is
// root-only (0400) since CVE-2020-8694, so a non-root agent gets a read error on
// every counter -> nil (best-effort, no graph).
type raplCollector struct {
	powercapRoot string
	hwmonRoot    string
	now          func() time.Time

	mu   sync.Mutex
	prev map[string]raplReading // keyed by domain dir path
}

// newRAPLCollector builds a RAPL collector reading the given sysfs roots.
func newRAPLCollector(powercapRoot, hwmonRoot string) *raplCollector {
	return &raplCollector{
		powercapRoot: powercapRoot,
		hwmonRoot:    hwmonRoot,
		now:          time.Now,
		prev:         map[string]raplReading{},
	}
}

// Name identifies this collector.
func (c *raplCollector) Name() string { return "rapl" }

// Available reports whether any intel-rapl powercap domain OR a hwmon power sensor
// is present. It does NOT prove the energy files are readable; an unreadable counter
// simply yields a nil value from Collect.
func (c *raplCollector) Available() bool {
	if domains, _ := filepath.Glob(filepath.Join(c.powercapRoot, "intel-rapl:*")); len(domains) > 0 {
		return true
	}
	if inputs, _ := filepath.Glob(filepath.Join(c.hwmonRoot, "*", "power*_input")); len(inputs) > 0 {
		return true
	}
	return false
}

// Collect reads every intel-rapl domain, sums the package-* domains for CPU watts
// (multi-socket) and uses the psys domain (else a hwmon power sensor) for system
// watts. Watts are Δenergy_uj / Δt across consecutive Collects, wraparound-corrected
// via max_energy_range_uj. A metric with no readable counter or no prior sample
// yields nil (best-effort). Never returns an error.
func (c *raplCollector) Collect(_ context.Context) (*float64, *float64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	domains, _ := filepath.Glob(filepath.Join(c.powercapRoot, "intel-rapl:*"))

	var pkgWatts float64
	havePkg := false
	var psysWatts *float64

	for _, dir := range domains {
		name := strings.TrimSpace(readSysfsString(filepath.Join(dir, "name")))
		if name == "" {
			continue
		}
		isPkg := strings.HasPrefix(name, "package")
		isPsys := name == "psys"
		if !isPkg && !isPsys {
			continue // core/dram/uncore subdomains are subsets of the package
		}
		energy, ok := readSysfsUint(filepath.Join(dir, "energy_uj"))
		if !ok {
			continue // unreadable (EACCES when not root) or absent -> skip
		}
		maxUJ, _ := readSysfsUint(filepath.Join(dir, "max_energy_range_uj"))
		cur := raplReading{energyUJ: energy, maxUJ: maxUJ, at: now}
		prev, seen := c.prev[dir]
		c.prev[dir] = cur
		if !seen {
			continue // first sample for this domain: no delta yet
		}
		dt := now.Sub(prev.at).Seconds()
		if dt <= 0 {
			continue
		}
		watts := raplDeltaJoules(prev.energyUJ, cur.energyUJ, prev.maxUJ) / dt
		if watts < 0 {
			continue
		}
		if isPkg {
			pkgWatts += watts
			havePkg = true
		} else { // psys
			w := watts
			psysWatts = &w
		}
	}

	var cpu *float64
	if havePkg {
		w := pkgWatts
		cpu = &w
	}

	system := psysWatts
	if system == nil {
		system = c.hwmonSystemWatts()
	}
	return cpu, system, nil
}

// hwmonSystemWatts returns the first readable hwmon power*_input (microwatts,
// instantaneous) as watts, or nil when none is present/readable.
func (c *raplCollector) hwmonSystemWatts() *float64 {
	inputs, _ := filepath.Glob(filepath.Join(c.hwmonRoot, "*", "power*_input"))
	for _, p := range inputs {
		if uw, ok := readSysfsUint(p); ok {
			w := float64(uw) / 1e6 // microwatts -> watts
			return &w
		}
	}
	return nil
}

// raplDeltaJoules returns the energy delta in JOULES between two µJ counter reads,
// correcting a counter wraparound via maxUJ (the range ceiling). cur >= prev is a
// plain difference; cur < prev means the counter wrapped, so the delta is
// (max - prev) + cur. maxUJ==0 (unknown ceiling) treats a decrease as 0.
func raplDeltaJoules(prev, cur, maxUJ uint64) float64 {
	var deltaUJ uint64
	switch {
	case cur >= prev:
		deltaUJ = cur - prev
	case maxUJ > prev:
		deltaUJ = (maxUJ - prev) + cur
	default:
		return 0
	}
	return float64(deltaUJ) / 1e6 // microjoules -> joules
}

// readSysfsString reads a sysfs file, returning "" on any error.
func readSysfsString(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// readSysfsUint reads a sysfs file holding a base-10 unsigned integer, returning
// (0,false) on any read/parse error.
func readSysfsUint(path string) (uint64, bool) {
	s := strings.TrimSpace(readSysfsString(path))
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
