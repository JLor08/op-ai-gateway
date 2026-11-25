// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build linux

package collector

import "testing"

func TestNativePowerCollectorUsesConfiguredRoot(t *testing.T) {
	custom := "/run/op-hostpower"
	c, ok := newNativePowerCollector(custom).(*raplCollector)
	if !ok {
		t.Fatalf("newNativePowerCollector(custom) is %T, want *raplCollector", newNativePowerCollector(custom))
	}
	if c.powercapRoot != custom {
		t.Fatalf("powercapRoot = %q, want %q", c.powercapRoot, custom)
	}
	if c.hwmonRoot != defaultHwmonRoot {
		t.Fatalf("hwmonRoot = %q, want default %q", c.hwmonRoot, defaultHwmonRoot)
	}

	d, ok := newNativePowerCollector("").(*raplCollector)
	if !ok {
		t.Fatal("newNativePowerCollector(\"\") is not a *raplCollector")
	}
	if d.powercapRoot != defaultPowercapRoot {
		t.Fatalf("default powercapRoot = %q, want %q", d.powercapRoot, defaultPowercapRoot)
	}
}
