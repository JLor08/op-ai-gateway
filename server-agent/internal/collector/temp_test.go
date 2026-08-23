// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"context"
	"errors"
	"testing"
)

// stubTemp is a TempCollector returning a fixed pointer or error.
type stubTemp struct {
	name      string
	available bool
	val       *float64
	err       error
}

func (s stubTemp) Name() string    { return s.name }
func (s stubTemp) Available() bool { return s.available }
func (s stubTemp) Collect(context.Context) (*float64, error) {
	return s.val, s.err
}

func TestMultiTempCollectorFirstNonNilWins(t *testing.T) {
	a := stubTemp{name: "a", available: true, val: nil}
	b := stubTemp{name: "b", available: true, val: fptr(61.2)}
	m := &multiTempCollector{subs: []TempCollector{a, b}}
	got, err := m.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got == nil || *got != 61.2 {
		t.Fatalf("got %v, want 61.2 (first non-nil sub wins)", deref(got))
	}
	if !m.Available() {
		t.Fatal("Available should be true when any sub is available")
	}
	if m.Name() != "temp" {
		t.Fatalf("Name() = %q, want \"temp\"", m.Name())
	}
}

func TestMultiTempCollectorSkipsUnavailable(t *testing.T) {
	a := stubTemp{name: "a", available: false, val: fptr(10)}
	b := stubTemp{name: "b", available: true, val: fptr(20)}
	m := &multiTempCollector{subs: []TempCollector{a, b}}
	got, err := m.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got == nil || *got != 20 {
		t.Fatalf("got %v, want 20 (unavailable sub skipped)", deref(got))
	}
}

func TestMultiTempCollectorSubErrorTolerated(t *testing.T) {
	// A sub-collector error is swallowed when a later sub yields a value.
	a := stubTemp{name: "a", available: true, err: errors.New("boom")}
	b := stubTemp{name: "b", available: true, val: fptr(30)}
	m := &multiTempCollector{subs: []TempCollector{a, b}}
	got, err := m.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect should not surface a swallowed error, got %v", err)
	}
	if got == nil || *got != 30 {
		t.Fatalf("got %v, want 30", deref(got))
	}
}

func TestMultiTempCollectorAllErrorReturnsFirstErr(t *testing.T) {
	// When no sub yields a value, the composite surfaces the first error (for
	// diagnostics) rather than swallowing it entirely.
	boom := errors.New("boom")
	m := &multiTempCollector{subs: []TempCollector{stubTemp{name: "a", available: true, err: boom}}}
	got, err := m.Collect(context.Background())
	if err == nil {
		t.Fatal("expected the sub's error to surface when no sub yields a value")
	}
	if got != nil {
		t.Fatalf("got %v, want nil", deref(got))
	}
}

func TestMultiTempCollectorAvailableFalseWhenEmpty(t *testing.T) {
	m := &multiTempCollector{}
	if m.Available() {
		t.Fatal("an empty composite should not be Available()")
	}
}

func TestTempSourcesListsComposedSubs(t *testing.T) {
	m := &multiTempCollector{subs: []TempCollector{stubTemp{name: "gopsutil"}, stubTemp{name: "lhm"}}}
	got := TempSources(m)
	if !containsString(got, "gopsutil") || !containsString(got, "lhm") {
		t.Fatalf("TempSources = %v, want gopsutil+lhm", got)
	}
}

func TestTempSourcesNonCompositeReturnsNil(t *testing.T) {
	if got := TempSources(stubTemp{name: "solo"}); got != nil {
		t.Fatalf("TempSources(non-composite) = %v, want nil", got)
	}
}

func TestDetectTempCollectorNeverNil(t *testing.T) {
	// Even with no LHM URL (and regardless of whether the native OS sub applies
	// on this platform), the result is a non-nil, Collect-safe composite.
	m := DetectTempCollector("")
	if m == nil {
		t.Fatal("DetectTempCollector(\"\") returned nil")
	}
	if _, err := m.Collect(context.Background()); err != nil {
		t.Fatalf("empty composite Collect should not error, got %v", err)
	}
}

// TestDetectTempCollectorComposesLHMWhenConfigured proves the Windows CPU-temp
// source (LHM) is actually wired into DetectTempCollector — an empty URL must
// NOT compose an LHM sub (mirrors DetectPowerCollector's contract).
func TestDetectTempCollectorComposesLHMWhenConfigured(t *testing.T) {
	m := DetectTempCollector("http://127.0.0.1:8085/data.json")
	if m == nil {
		t.Fatal("DetectTempCollector returned nil")
	}
	if !m.Available() {
		t.Fatal("a composite with an LHM URL should be Available()")
	}
	if !containsString(TempSources(m), "lhm") {
		t.Fatalf("TempSources = %v, want \"lhm\" among them when a URL is configured", TempSources(m))
	}
}

// TestDetectTempCollectorNoLHMWithoutURL proves an empty LHM URL composes to
// no temp sub beyond whatever the native OS collector contributes.
func TestDetectTempCollectorNoLHMWithoutURL(t *testing.T) {
	m := DetectTempCollector("")
	if containsString(TempSources(m), "lhm") {
		t.Fatalf("TempSources = %v, did not expect \"lhm\" with no URL configured", TempSources(m))
	}
}
