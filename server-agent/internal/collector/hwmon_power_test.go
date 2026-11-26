// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"context"
	"path/filepath"
	"testing"
)

func TestHwmonPowerCollector(t *testing.T) {
	root := t.TempDir()
	writeSysfs(t, filepath.Join(root, "hwmon0"), "power1_input", "7000000\n") // 7 W
	c := newHwmonPowerCollector(root)
	eq(t, "hwmon", c.Name())
	if !c.Available() {
		t.Fatal("Available() = false, want true")
	}
	cpu, sys, err := c.Collect(context.Background())
	noErr(t, err)
	if cpu != nil {
		t.Fatalf("cpu = %v, want nil", *cpu)
	}
	if sys == nil || *sys < 6.999 || *sys > 7.001 {
		t.Fatalf("system = %v, want 7", sys)
	}
}

func TestHwmonPowerCollectorNone(t *testing.T) {
	c := newHwmonPowerCollector(t.TempDir())
	if c.Available() {
		t.Fatal("Available() = true for an empty tree")
	}
	cpu, sys, err := c.Collect(context.Background())
	noErr(t, err)
	if cpu != nil || sys != nil {
		t.Fatalf("got (%v, %v), want nils", cpu, sys)
	}
}

func TestHwmonPowerEmptyRootNeverGlobsCwd(t *testing.T) {
	cwd := t.TempDir()
	writeSysfs(t, filepath.Join(cwd, "x"), "power1_input", "5000000")
	t.Chdir(cwd)
	if got := hwmonPowerInputs(""); got != nil {
		t.Fatalf("hwmonPowerInputs(\"\") = %v, want nil", got)
	}
	if newHwmonPowerCollector("").Available() {
		t.Fatal("Available() = true for an empty root")
	}
}

type fakePower struct {
	name     string
	cpu, sys *float64
}

func (f fakePower) Name() string    { return f.name }
func (f fakePower) Available() bool { return true }
func (f fakePower) Collect(context.Context) (*float64, *float64, error) {
	return f.cpu, f.sys, nil
}

func fp(v float64) *float64 { return &v }

func compositeSystem(t *testing.T, subs ...PowerCollector) float64 {
	t.Helper()
	_, sys, err := (&multiPowerCollector{subs: subs}).Collect(context.Background())
	noErr(t, err)
	if sys == nil {
		t.Fatal("system = nil")
	}
	return *sys
}

func TestRootRAPLPsysBeatsHwmon(t *testing.T) {
	hw := fakePower{name: "hwmon", sys: fp(55)}
	eq(t, 30.0, compositeSystem(t, fakePower{name: "rapl", sys: fp(30)}, fakePower{name: "helper"}, hw))
}

func TestHelperPsysBeatsHwmon(t *testing.T) {
	hw := fakePower{name: "hwmon", sys: fp(55)}
	eq(t, 40.0, compositeSystem(t, fakePower{name: "rapl"}, fakePower{name: "helper", sys: fp(40)}, hw))
}
