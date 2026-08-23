// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"context"
	"testing"
)

// stubPower is a PowerCollector returning fixed pointers.
type stubPower struct {
	name        string
	available   bool
	cpu, system *float64
}

func (s stubPower) Name() string    { return s.name }
func (s stubPower) Available() bool { return s.available }
func (s stubPower) Collect(context.Context) (*float64, *float64, error) {
	return s.cpu, s.system, nil
}

func fptr(v float64) *float64 { return &v }

func TestMultiPowerCollectorFirstNonNilPerMetric(t *testing.T) {
	// native provides CPU only; LHM provides CPU+system. Native wins for CPU
	// (first non-nil), LHM fills system (D9).
	native := stubPower{name: "native", available: true, cpu: fptr(30), system: nil}
	lhm := stubPower{name: "lhm", available: true, cpu: fptr(99), system: fptr(150)}
	m := &multiPowerCollector{subs: []PowerCollector{native, lhm}}
	cpu, system, err := m.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if cpu == nil || *cpu != 30 {
		t.Fatalf("CPU = %v, want 30 (native wins)", cpu)
	}
	if system == nil || *system != 150 {
		t.Fatalf("system = %v, want 150 (LHM fills the gap)", system)
	}
	if !m.Available() {
		t.Fatal("Available should be true when any sub is available")
	}
}

func TestMultiPowerCollectorNativeNilFallsToLHM(t *testing.T) {
	native := stubPower{name: "native", available: true, cpu: nil, system: nil}
	lhm := stubPower{name: "lhm", available: true, cpu: fptr(42), system: nil}
	m := &multiPowerCollector{subs: []PowerCollector{native, lhm}}
	cpu, _, _ := m.Collect(context.Background())
	if cpu == nil || *cpu != 42 {
		t.Fatalf("CPU = %v, want 42 (native nil -> LHM)", cpu)
	}
}

func TestMultiPowerCollectorSubErrorNeverFails(t *testing.T) {
	// A sub-collector error is swallowed; the composite still returns nil error.
	m := &multiPowerCollector{subs: []PowerCollector{errPower{}}}
	if _, _, err := m.Collect(context.Background()); err != nil {
		t.Fatalf("composite Collect should never error, got %v", err)
	}
}

// errPower always errors.
type errPower struct{}

func (errPower) Name() string    { return "err" }
func (errPower) Available() bool { return true }
func (errPower) Collect(context.Context) (*float64, *float64, error) {
	return nil, nil, context.DeadlineExceeded
}

func TestDetectPowerCollectorComposesLHMWhenConfigured(t *testing.T) {
	m := DetectPowerCollector("http://127.0.0.1:8085/data.json")
	if m == nil {
		t.Fatal("DetectPowerCollector returned nil")
	}
	if !m.Available() {
		t.Fatal("a composite with an LHM URL should be Available()")
	}
}

func TestDetectPowerCollectorNeverNil(t *testing.T) {
	// Even with no LHM URL the result is a non-nil, Collect-safe composite.
	m := DetectPowerCollector("")
	if m == nil {
		t.Fatal("DetectPowerCollector(\"\") returned nil")
	}
	if _, _, err := m.Collect(context.Background()); err != nil {
		t.Fatalf("empty composite Collect should not error, got %v", err)
	}
}
