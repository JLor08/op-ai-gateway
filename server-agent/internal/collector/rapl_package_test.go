// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func writeRAPLDomain(t *testing.T, root, dir, name string, energyUJ uint64) {
	t.Helper()
	d := filepath.Join(root, dir)
	writeSysfs(t, d, "name", name+"\n")
	writeSysfs(t, d, "energy_uj", strconv.FormatUint(energyUJ, 10)+"\n")
	writeSysfs(t, d, "max_energy_range_uj", "262143328850\n")
}

func setRAPLEnergy(t *testing.T, root, dir string, energyUJ uint64) {
	t.Helper()
	writeSysfs(t, filepath.Join(root, dir), "energy_uj", strconv.FormatUint(energyUJ, 10)+"\n")
}

func TestPackageEnergyReaderFirstCallNotOK(t *testing.T) {
	root := t.TempDir()
	writeRAPLDomain(t, root, "intel-rapl:0", "package-0", 1_000_000)
	r := NewPackageEnergyReader(root)
	if _, ok := r.DeltaJoules(); ok {
		t.Fatal("first call must not be ok (no previous reading)")
	}
}

func TestPackageEnergyReaderSumsPackagesSkipsSubdomains(t *testing.T) {
	root := t.TempDir()
	writeRAPLDomain(t, root, "intel-rapl:0", "package-0", 1_000_000)
	writeRAPLDomain(t, root, "intel-rapl:1", "package-1", 500_000)
	writeRAPLDomain(t, root, "intel-rapl:0:0", "core", 0)
	r := NewPackageEnergyReader(root)
	r.DeltaJoules()
	setRAPLEnergy(t, root, "intel-rapl:0", 4_000_000)
	setRAPLEnergy(t, root, "intel-rapl:1", 2_500_000)
	setRAPLEnergy(t, root, "intel-rapl:0:0", 9_000_000)
	j, ok := r.DeltaJoules()
	if !ok || math.Abs(j-5.0) > 1e-9 {
		t.Fatalf("got (%v,%v), want (5.0,true)", j, ok)
	}
}

func TestPackageEnergyReaderWraparound(t *testing.T) {
	root := t.TempDir()
	writeRAPLDomain(t, root, "intel-rapl:0", "package-0", 262143328000)
	r := NewPackageEnergyReader(root)
	r.DeltaJoules()
	setRAPLEnergy(t, root, "intel-rapl:0", 1150)
	j, ok := r.DeltaJoules()
	if !ok || math.Abs(j-0.002) > 1e-9 {
		t.Fatalf("got (%v,%v), want (0.002,true)", j, ok)
	}
}

func TestPackageEnergyReaderNoDomainsNotOK(t *testing.T) {
	r := NewPackageEnergyReader(t.TempDir())
	for i := 0; i < 2; i++ {
		if _, ok := r.DeltaJoules(); ok {
			t.Fatalf("call %d: ok with no domains", i+1)
		}
	}
}

func TestPackageEnergyReaderThirdCallDeltasAgainstSecond(t *testing.T) {
	root := t.TempDir()
	writeRAPLDomain(t, root, "intel-rapl:0", "package-0", 1_000_000)
	r := NewPackageEnergyReader(root)
	r.DeltaJoules()
	setRAPLEnergy(t, root, "intel-rapl:0", 4_000_000)
	if j, ok := r.DeltaJoules(); !ok || math.Abs(j-3.0) > 1e-9 {
		t.Fatalf("second call (%v,%v), want (3.0,true)", j, ok)
	}
	setRAPLEnergy(t, root, "intel-rapl:0", 10_000_000)
	if j, ok := r.DeltaJoules(); !ok || math.Abs(j-6.0) > 1e-9 {
		t.Fatalf("third call (%v,%v), want (6.0,true) against the second reading", j, ok)
	}
}

func TestPackageEnergyReaderUnreadableDomainReBaselines(t *testing.T) {
	root := t.TempDir()
	writeRAPLDomain(t, root, "intel-rapl:0", "package-0", 1_000_000)
	r := NewPackageEnergyReader(root)
	r.DeltaJoules()

	// Unreadable for one call.
	if err := os.Remove(filepath.Join(root, "intel-rapl:0", "energy_uj")); err != nil {
		t.Fatal(err)
	}
	if j, ok := r.DeltaJoules(); ok || j != 0 {
		t.Fatalf("unreadable call (%v,%v), want (0,false)", j, ok)
	}

	// Readable again far ahead: no gap-spanning delta, it re-baselines.
	setRAPLEnergy(t, root, "intel-rapl:0", 100_000_000)
	if j, ok := r.DeltaJoules(); ok || j != 0 {
		t.Fatalf("return call (%v,%v), want (0,false): delta spans the gap", j, ok)
	}
	setRAPLEnergy(t, root, "intel-rapl:0", 103_000_000)
	if j, ok := r.DeltaJoules(); !ok || math.Abs(j-3.0) > 1e-9 {
		t.Fatalf("next call (%v,%v), want (3.0,true) against the re-baseline", j, ok)
	}
}

func TestPackageEnergyReaderDropsVanishedDomain(t *testing.T) {
	root := t.TempDir()
	writeRAPLDomain(t, root, "intel-rapl:0", "package-0", 1_000_000)
	writeRAPLDomain(t, root, "intel-rapl:1", "package-1", 1_000_000)
	r := NewPackageEnergyReader(root)
	r.DeltaJoules()
	if err := os.RemoveAll(filepath.Join(root, "intel-rapl:1")); err != nil {
		t.Fatal(err)
	}
	r.DeltaJoules()
	if _, kept := r.prev[filepath.Join(root, "intel-rapl:1")]; kept {
		t.Fatal("prev still holds a domain no longer in the glob")
	}
	writeRAPLDomain(t, root, "intel-rapl:1", "package-1", 50_000_000)
	setRAPLEnergy(t, root, "intel-rapl:0", 2_000_000)
	if j, ok := r.DeltaJoules(); !ok || math.Abs(j-1.0) > 1e-9 {
		t.Fatalf("got (%v,%v), want (1.0,true): returned domain must re-baseline", j, ok)
	}
}
