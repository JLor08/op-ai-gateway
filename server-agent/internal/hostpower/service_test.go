// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package hostpower

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

type fixture struct {
	t    *testing.T
	root string
}

func newFixture(t *testing.T, vms ...string) *fixture {
	t.Helper()
	f := &fixture{t: t, root: t.TempDir()}
	dom := filepath.Join(f.root, "powercap", "intel-rapl:0")
	f.mkdir(dom)
	f.write(filepath.Join(dom, "name"), "package-0\n")
	f.write(filepath.Join(dom, "max_energy_range_uj"), "262143328850\n")
	f.setEnergyUJ(1_000_000_000)
	f.setBusyUsec(0)
	f.mkdir(filepath.Join(f.root, "out"))
	for _, vm := range vms {
		f.setVMUsec(vm, 0)
	}
	return f
}

func (f *fixture) mkdir(p string) {
	f.t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) write(p, content string) {
	f.t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) energyPath() string {
	return filepath.Join(f.root, "powercap", "intel-rapl:0", "energy_uj")
}

func (f *fixture) setEnergyUJ(v uint64) { f.write(f.energyPath(), strconv.FormatUint(v, 10)+"\n") }

// setBusyUsec writes a /proc/stat whose busy time is v microseconds (user jiffies).
func (f *fixture) setBusyUsec(v uint64) {
	f.write(filepath.Join(f.root, "proc_stat"),
		fmt.Sprintf("cpu  %d 0 0 5000 0 0 0 0 0 0\ncpu0 1 2 3\n", v/usecPerJiffy))
}

func (f *fixture) vmDir(vm string) string {
	return filepath.Join(f.root, "cgroup", "qemu.slice", vm+".scope")
}

func (f *fixture) setVMUsec(vm string, v uint64) {
	f.mkdir(f.vmDir(vm))
	f.write(filepath.Join(f.vmDir(vm), "cpu.stat"), fmt.Sprintf("usage_usec %d\nuser_usec 1\n", v))
}

func (f *fixture) service() *Service {
	return New(Options{
		PowercapRoot: filepath.Join(f.root, "powercap"),
		ProcStatPath: filepath.Join(f.root, "proc_stat"),
		CgroupRoot:   filepath.Join(f.root, "cgroup"),
		OutDir:       filepath.Join(f.root, "out"),
	})
}

func (f *fixture) outFile(vm string) string {
	b, err := os.ReadFile(filepath.Join(f.root, "out", vm, DomainDir, "energy_uj"))
	if err != nil {
		f.t.Fatal(err)
	}
	return string(b)
}

func mustTick(t *testing.T, s *Service) {
	t.Helper()
	if err := s.Tick(); err != nil {
		t.Fatalf("Tick: %v", err)
	}
}

func TestTickFirstTickBaselinesAndCreatesDirs(t *testing.T) {
	f := newFixture(t, "100", "101")
	s := f.service()
	mustTick(t, s)
	for _, vm := range []string{"100", "101"} {
		if got := f.outFile(vm); got != "0\n" {
			t.Errorf("vm %s energy_uj = %q, want 0", vm, got)
		}
		if c := s.Counters()[vm]; c != 0 {
			t.Errorf("vm %s counter = %d", vm, c)
		}
	}
}

func advanceTwoVMs(f *fixture) {
	f.setEnergyUJ(1_010_000_000) // +10 J
	f.setBusyUsec(1_000_000)
	f.setVMUsec("100", 250_000)
	f.setVMUsec("101", 500_000)
}

func TestTickAttributesByActiveShare(t *testing.T) {
	f := newFixture(t, "100", "101")
	s := f.service()
	mustTick(t, s)
	advanceTwoVMs(f)
	mustTick(t, s)
	c := s.Counters()
	if c["100"] != 2_500_000 || c["101"] != 5_000_000 {
		t.Errorf("counters = %v", c)
	}
	if got := f.outFile("100"); got != "2500000\n" {
		t.Errorf("file 100 = %q", got)
	}
	if got := f.outFile("101"); got != "5000000\n" {
		t.Errorf("file 101 = %q", got)
	}
}

func TestTickPersistsAcrossRestart(t *testing.T) {
	f := newFixture(t, "100", "101")
	s := f.service()
	mustTick(t, s)
	advanceTwoVMs(f)
	mustTick(t, s)
	s2 := f.service()
	mustTick(t, s2)
	if got := s2.Counters()["100"]; got != 2_500_000 {
		t.Errorf("restored counter = %d, want 2500000", got)
	}
}

func TestTickSkipsVMWhoseCPUStatVanished(t *testing.T) {
	f := newFixture(t, "100", "101")
	s := f.service()
	mustTick(t, s)
	if err := os.Remove(filepath.Join(f.vmDir("101"), "cpu.stat")); err != nil {
		t.Fatal(err)
	}
	f.setEnergyUJ(1_010_000_000)
	f.setBusyUsec(1_000_000)
	f.setVMUsec("100", 250_000)
	mustTick(t, s)
	c := s.Counters()
	if c["100"] != 2_500_000 || c["101"] != 0 {
		t.Errorf("counters = %v", c)
	}
	// 101 returns with a large usage: it re-baselines, no huge catch-up delta.
	f.setEnergyUJ(1_020_000_000)
	f.setBusyUsec(2_000_000)
	f.setVMUsec("100", 500_000)
	f.setVMUsec("101", 9_000_000)
	mustTick(t, s)
	if c := s.Counters(); c["101"] != 0 || c["100"] != 5_000_000 {
		t.Errorf("after return counters = %v", c)
	}
}

func TestTickZeroHostDeltaLeavesCountersUnchanged(t *testing.T) {
	f := newFixture(t, "100")
	s := f.service()
	mustTick(t, s)
	f.setEnergyUJ(1_010_000_000)
	f.setVMUsec("100", 250_000) // busy stays 0
	mustTick(t, s)
	if c := s.Counters()["100"]; c != 0 {
		t.Errorf("counter = %d, want 0", c)
	}
}

func TestTickNoEnergyDeltaStillAdvancesBaselines(t *testing.T) {
	f := newFixture(t, "100")
	s := f.service()
	mustTick(t, s) // baselines: energy 1000 J, busy 0, vm 0

	// Gap tick: energy unreadable while busy and VM usage advance.
	if err := os.Remove(f.energyPath()); err != nil {
		t.Fatal(err)
	}
	f.setBusyUsec(1_000_000)
	f.setVMUsec("100", 750_000)
	mustTick(t, s)
	if c := s.Counters()["100"]; c != 0 {
		t.Fatalf("gap tick attributed %d", c)
	}

	// Final tick: energy back (+10 J vs. before the gap), busy +1 s, vm +0.25 s.
	f.setEnergyUJ(1_010_000_000)
	f.setBusyUsec(2_000_000)
	f.setVMUsec("100", 1_000_000)
	mustTick(t, s)
	// Final interval only: 0.25 * 10 J. Unadvanced baselines would give
	// share 1_000_000/2_000_000 = 0.5 -> 5_000_000.
	if c := s.Counters()["100"]; c != 2_500_000 {
		t.Errorf("counter = %d, want 2500000", c)
	}
}

func TestOptionsWithDefaults(t *testing.T) {
	o := Options{}.withDefaults()
	if o.PowercapRoot != DefaultPowercapRoot || o.ProcStatPath != DefaultProcStatPath ||
		o.CgroupRoot != DefaultCgroupRoot || o.OutDir != DefaultOutDir {
		t.Errorf("defaults = %+v", o)
	}
	if got := (Options{OutDir: "/x"}).withDefaults().OutDir; got != "/x" || strings.Contains(got, "op-hostpower") {
		t.Errorf("explicit OutDir overridden: %q", got)
	}
}
