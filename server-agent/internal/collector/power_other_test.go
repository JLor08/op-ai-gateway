// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build !linux && !darwin

package collector

import "testing"

func TestNativePowerCollectorWindowsWiring(t *testing.T) {
	if c := newNativePowerCollector(""); c != nil {
		t.Fatalf("newNativePowerCollector(\"\") = %T, want nil interface", c)
	}
	c, ok := newNativePowerCollector(`Z:\`).(*raplCollector)
	if !ok {
		t.Fatal(`newNativePowerCollector(Z:\) is not a *raplCollector`)
	}
	if c.domainGlob != "intel-rapl_*" {
		t.Fatalf("domainGlob = %q, want intel-rapl_*", c.domainGlob)
	}
	if c.hwmonRoot != "" {
		t.Fatalf("hwmonRoot = %q, want empty", c.hwmonRoot)
	}
}
