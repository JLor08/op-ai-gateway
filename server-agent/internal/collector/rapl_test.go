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

	c := newRAPLCollector(root, filepath.Join(root, "nohwmon"))
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

	c := newRAPLCollector(root, filepath.Join(root, "nohwmon"))
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
	c := newRAPLCollector(root, filepath.Join(root, "nohwmon"))
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
	c := newRAPLCollector(root, filepath.Join(root, "nohwmon"))
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
	c2 := newRAPLCollector(filepath.Join(root2, "powercap"), filepath.Join(root2, "hwmon"))
	_, sys2, _ := c2.Collect(context.Background())
	if sys2 == nil || *sys2 < 6.999 || *sys2 > 7.001 {
		t.Fatalf("hwmon system watts = %v, want 7", sys2)
	}

	// Neither psys nor hwmon -> nil.
	c3 := newRAPLCollector(t.TempDir(), t.TempDir())
	_, sys3, _ := c3.Collect(context.Background())
	if sys3 != nil {
		t.Fatalf("system = %v, want nil (no psys, no hwmon)", *sys3)
	}
}
