// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build linux

package collector

import (
	"slices"
	"testing"
)

func TestNativePowerCollectorUsesConfiguredRoot(t *testing.T) {
	custom := "/run/op-hostpower"
	c, ok := newNativePowerCollector(custom).(*raplCollector)
	if !ok {
		t.Fatalf("newNativePowerCollector(custom) is %T, want *raplCollector", newNativePowerCollector(custom))
	}
	if c.powercapRoot != custom {
		t.Fatalf("powercapRoot = %q, want %q", c.powercapRoot, custom)
	}

	d, ok := newNativePowerCollector("").(*raplCollector)
	if !ok {
		t.Fatal("newNativePowerCollector(\"\") is not a *raplCollector")
	}
	if d.powercapRoot != defaultPowercapRoot {
		t.Fatalf("default powercapRoot = %q, want %q", d.powercapRoot, defaultPowercapRoot)
	}
}

func TestLinuxCompositeOrder(t *testing.T) {
	origGOOS, origEuid := helperGOOS, helperGeteuid
	t.Cleanup(func() { helperGOOS, helperGeteuid = origGOOS, origEuid })
	cfg := PowerConfig{HelperSocket: "/run/x/agent.sock"}

	helperGOOS, helperGeteuid = "linux", func() int { return 1000 }
	if got, want := PowerSources(DetectPowerCollector(cfg)), []string{"rapl", "helper", "hwmon"}; !slices.Equal(got, want) {
		t.Fatalf("non-root with socket: sources = %v, want %v", got, want)
	}

	want := []string{"rapl", "hwmon"}
	helperGeteuid = func() int { return 0 }
	if got := PowerSources(DetectPowerCollector(cfg)); !slices.Equal(got, want) {
		t.Fatalf("root: sources = %v, want %v", got, want)
	}
	helperGeteuid = func() int { return 1000 }
	if got := PowerSources(DetectPowerCollector(PowerConfig{})); !slices.Equal(got, want) {
		t.Fatalf("no socket: sources = %v, want %v", got, want)
	}
}
