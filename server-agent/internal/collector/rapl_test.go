// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeSysfs writes a single-line sysfs-style file under dir (created if needed).
func writeSysfs(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s/%s: %v", dir, name, err)
	}
}

func TestRAPLDeltaWatts(t *testing.T) {
	root := t.TempDir()
	pkg := filepath.Join(root, "intel-rapl:0")
	writeSysfs(t, pkg, "name", "package-0\n")
	writeSysfs(t, pkg, "max_energy_range_uj", "262143328850\n")
	writeSysfs(t, pkg, "energy_uj", "1000000\n")

	c := newRAPLCollector(root, filepath.Join(root, "nohwmon"), raplDomainGlobLinux)
	base := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return base }

	// First Collect: no delta yet -> nil CPU.
	cpu, _, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("first Collect: %v", err)
	}
	if cpu != nil {
		t.Fatalf("first Collect CPU = %v, want nil (no delta yet)", *cpu)
	}

	// +15 J over 1 s = 15 W.
	writeSysfs(t, pkg, "energy_uj", "16000000\n")
	c.now = func() time.Time { return base.Add(time.Second) }
	cpu, _, err = c.Collect(context.Background())
	if err != nil {
		t.Fatalf("second Collect: %v", err)
	}
	if cpu == nil {
		t.Fatal("second Collect CPU = nil, want ~15 W")
	}
	if *cpu < 14.999 || *cpu > 15.001 {
		t.Fatalf("CPU watts = %v, want 15", *cpu)
	}
}

func TestRAPLWraparound(t *testing.T) {
	root := t.TempDir()
	pkg := filepath.Join(root, "intel-rapl:0")
	writeSysfs(t, pkg, "name", "package-0\n")
	writeSysfs(t, pkg, "max_energy_range_uj", "20000000\n")
	writeSysfs(t, pkg, "energy_uj", "19000000\n")

	c := newRAPLCollector(root, filepath.Join(root, "nohwmon"), raplDomainGlobLinux)
	base := time.Now()
	c.now = func() time.Time { return base }
	if _, _, err := c.Collect(context.Background()); err != nil {
		t.Fatalf("seed Collect: %v", err)
	}
	// Wrap: 19,000,000 -> 1,000,000 with max 20,000,000 => (20M-19M)+1M = 2 J / 1 s = 2 W.
	writeSysfs(t, pkg, "energy_uj", "1000000\n")
	c.now = func() time.Time { return base.Add(time.Second) }
	cpu, _, _ := c.Collect(context.Background())
	if cpu == nil || *cpu < 1.999 || *cpu > 2.001 {
		t.Fatalf("wraparound CPU watts = %v, want 2", cpu)
	}
}

func TestRAPLUnreadableEnergyYieldsNil(t *testing.T) {
	root := t.TempDir()
	pkg := filepath.Join(root, "intel-rapl:0")
	writeSysfs(t, pkg, "name", "package-0\n")
	// No energy_uj file at all -> the same skip path as a real EACCES (0400,
	// non-root) read failure -> no package domain readable -> CPU nil.
	c := newRAPLCollector(root, filepath.Join(root, "nohwmon"), raplDomainGlobLinux)
	cpu, sys, _ := c.Collect(context.Background())
	if cpu != nil {
		t.Fatalf("CPU = %v, want nil (energy unreadable)", *cpu)
	}
	if sys != nil {
		t.Fatalf("system = %v, want nil (no psys/hwmon)", *sys)
	}
}

func TestRAPLSystemPsysThenHwmonFallback(t *testing.T) {
	// psys present -> system from psys delta.
	root := t.TempDir()
	psys := filepath.Join(root, "intel-rapl:1")
	writeSysfs(t, psys, "name", "psys\n")
	writeSysfs(t, psys, "max_energy_range_uj", "1000000000\n")
	writeSysfs(t, psys, "energy_uj", "0\n")
	c := newRAPLCollector(root, filepath.Join(root, "nohwmon"), raplDomainGlobLinux)
	base := time.Now()
	c.now = func() time.Time { return base }
	_, _, _ = c.Collect(context.Background())
	writeSysfs(t, psys, "energy_uj", "5000000\n") // +5 J / 1 s = 5 W
	c.now = func() time.Time { return base.Add(time.Second) }
	_, sys, _ := c.Collect(context.Background())
	if sys == nil || *sys < 4.999 || *sys > 5.001 {
		t.Fatalf("psys system watts = %v, want 5", sys)
	}

	// No psys -> hwmon power1_input (microwatts, instantaneous) -> system watts.
	root2 := t.TempDir()
	hwmon := filepath.Join(root2, "hwmon", "hwmon0")
	writeSysfs(t, hwmon, "power1_input", "7000000\n") // 7,000,000 uW = 7 W
	c2 := newRAPLCollector(filepath.Join(root2, "powercap"), filepath.Join(root2, "hwmon"), raplDomainGlobLinux)
	_, sys2, _ := c2.Collect(context.Background())
	if sys2 == nil || *sys2 < 6.999 || *sys2 > 7.001 {
		t.Fatalf("hwmon system watts = %v, want 7", sys2)
	}

	// Neither psys nor hwmon -> nil.
	c3 := newRAPLCollector(t.TempDir(), t.TempDir(), raplDomainGlobLinux)
	_, sys3, _ := c3.Collect(context.Background())
	if sys3 != nil {
		t.Fatalf("system = %v, want nil (no psys, no hwmon)", *sys3)
	}
}

// TestRAPLCollectorCustomRootAppearsLater covers a host-provided powercap root
// (e.g. a virtiofs mount) that does not exist yet when the agent starts: the
// collector reports unavailable without error, then picks the tree up once it
// appears.
func TestRAPLCollectorCustomRootAppearsLater(t *testing.T) {
	root := filepath.Join(t.TempDir(), "notyet")
	c := newRAPLCollector(root, filepath.Join(t.TempDir(), "nohwmon"), raplDomainGlobLinux)
	base := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return base }

	if c.Available() {
		t.Fatal("Available() = true for a missing root, want false")
	}
	cpu, _, err := c.Collect(context.Background())
	if err != nil || cpu != nil {
		t.Fatalf("Collect on missing root = (%v, %v), want (nil, nil)", cpu, err)
	}

	pkg := filepath.Join(root, "intel-rapl:0")
	writeSysfs(t, pkg, "name", "package-0\n")
	writeSysfs(t, pkg, "max_energy_range_uj", "9223372036854775807\n")
	writeSysfs(t, pkg, "energy_uj", "1000000\n")
	if !c.Available() {
		t.Fatal("Available() = false after the tree appeared, want true")
	}
	if cpu, _, err = c.Collect(context.Background()); err != nil || cpu != nil {
		t.Fatalf("baseline Collect = (%v, %v), want (nil, nil)", cpu, err)
	}

	writeSysfs(t, pkg, "energy_uj", "16000000\n")
	c.now = func() time.Time { return base.Add(time.Second) }
	cpu, _, err = c.Collect(context.Background())
	if err != nil || cpu == nil {
		t.Fatalf("second Collect = (%v, %v), want ~15 W", cpu, err)
	}
	if *cpu < 14.999 || *cpu > 15.001 {
		t.Fatalf("CPU watts = %v, want 15", *cpu)
	}
}

// TestRAPLZeroRangeCounterDecreaseIsZeroWatts pins the guest-side behaviour the
// hostpower service relies on by publishing max_energy_range_uj = 0: a counter
// that goes backwards (a host-side regression) reads as 0 W, never as a
// near-full-range wraparound.
func TestRAPLZeroRangeCounterDecreaseIsZeroWatts(t *testing.T) {
	root := t.TempDir()
	pkg := filepath.Join(root, "intel-rapl:0")
	writeSysfs(t, pkg, "name", "package-0\n")
	writeSysfs(t, pkg, "max_energy_range_uj", "0\n")
	writeSysfs(t, pkg, "energy_uj", "5000000\n")

	c := newRAPLCollector(root, filepath.Join(root, "nohwmon"), raplDomainGlobLinux)
	base := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return base }
	if _, _, err := c.Collect(context.Background()); err != nil {
		t.Fatalf("seed Collect: %v", err)
	}
	writeSysfs(t, pkg, "energy_uj", "1000000\n")
	c.now = func() time.Time { return base.Add(time.Second) }
	cpu, _, _ := c.Collect(context.Background())
	if cpu == nil || *cpu != 0 {
		t.Fatalf("decreasing counter with range 0: CPU watts = %v, want 0", cpu)
	}
}

// writePackageDomain writes a package-0 domain with the given energy_uj content.
func writePackageDomain(t *testing.T, dir string, energyUJ string) {
	t.Helper()
	writeSysfs(t, dir, "name", "package-0\n")
	writeSysfs(t, dir, "max_energy_range_uj", "262143328850\n")
	writeSysfs(t, dir, "energy_uj", energyUJ)
}

// sampleTwice collects at t0 and t0+1s (after bump) and returns the second CPU value.
func sampleTwice(t *testing.T, c *raplCollector, bump func()) *float64 {
	t.Helper()
	base := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return base }
	if _, _, err := c.Collect(context.Background()); err != nil {
		t.Fatalf("first Collect: %v", err)
	}
	bump()
	c.now = func() time.Time { return base.Add(time.Second) }
	cpu, _, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("second Collect: %v", err)
	}
	return cpu
}

func TestRAPLWindowsGlobReadsUnderscoreDomain(t *testing.T) {
	root := t.TempDir()
	pkg := filepath.Join(root, "intel-rapl_0")
	writePackageDomain(t, pkg, "1000000\n")
	c := newRAPLCollector(root, "", raplDomainGlobWindows)
	if !c.Available() {
		t.Fatal("Available() = false, want true for an intel-rapl_0 domain")
	}
	cpu := sampleTwice(t, c, func() { writeSysfs(t, pkg, "energy_uj", "16000000\n") })
	if cpu == nil || *cpu < 14.999 || *cpu > 15.001 {
		t.Fatalf("CPU watts = %v, want 15", cpu)
	}
}

func TestRAPLEachPlatformCountsOnce(t *testing.T) {
	for _, glob := range []string{raplDomainGlobLinux, raplDomainGlobWindows} {
		t.Run(glob, func(t *testing.T) {
			root := t.TempDir()
			colon := filepath.Join(root, "intel-rapl:0")
			under := filepath.Join(root, "intel-rapl_0")
			writePackageDomain(t, colon, "1000000\n")
			writePackageDomain(t, under, "1000000\n")
			c := newRAPLCollector(root, "", glob)
			cpu := sampleTwice(t, c, func() {
				writeSysfs(t, colon, "energy_uj", "11000000\n")
				writeSysfs(t, under, "energy_uj", "11000000\n")
			})
			if cpu == nil || *cpu < 9.999 || *cpu > 10.001 {
				t.Fatalf("CPU watts = %v, want 10 (never 20)", cpu)
			}
		})
	}
}

func TestRAPLEmptyHwmonRootNeverGlobsCwd(t *testing.T) {
	cwd := t.TempDir()
	writeSysfs(t, filepath.Join(cwd, "x"), "power1_input", "5000000")
	t.Chdir(cwd)

	c := newRAPLCollector(t.TempDir(), "", raplDomainGlobWindows)
	if c.Available() {
		t.Fatal("Available() = true, want false (empty hwmon root must not glob the working directory)")
	}
	_, system, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if system != nil {
		t.Fatalf("system watts = %v, want nil", *system)
	}
}
