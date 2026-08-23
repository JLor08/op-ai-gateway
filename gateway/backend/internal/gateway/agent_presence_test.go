// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"testing"
	"time"
)

func TestAgentPresenceRegistryReporting(t *testing.T) {
	now := time.Unix(1000, 0)
	r := NewAgentPresenceRegistry(180 * time.Second)
	r.now = func() time.Time { return now }

	if r.Reporting("srv-1") {
		t.Fatal("never-reported server must not be reporting")
	}
	r.Report("srv-1")
	if !r.Reporting("srv-1") {
		t.Fatal("just-reported server must be reporting")
	}
	now = now.Add(179 * time.Second)
	if !r.Reporting("srv-1") {
		t.Fatal("within window must still be reporting")
	}
	now = now.Add(1 * time.Second) // exactly 180s: inclusive boundary
	if !r.Reporting("srv-1") {
		t.Fatal("at the inclusive window boundary must still be reporting")
	}
	now = now.Add(1 * time.Second) // 181s total
	if r.Reporting("srv-1") {
		t.Fatal("past window must not be reporting")
	}
}

func TestAgentPresenceRegistryReportingWithin(t *testing.T) {
	now := time.Unix(2000, 0)
	r := NewAgentPresenceRegistry(180 * time.Second)
	r.now = func() time.Time { return now }

	if r.ReportingWithin("srv-1", 10*time.Second) {
		t.Fatal("never-seen server must not be reporting within any window")
	}
	r.Report("srv-1")
	if !r.ReportingWithin("srv-1", 10*time.Second) {
		t.Fatal("just-reported server must be reporting within a 10s window")
	}
	// Exact-equality boundary: an age exactly == the window is inclusive (<=, not <).
	now = now.Add(5 * time.Second)
	if !r.ReportingWithin("srv-1", 5*time.Second) {
		t.Fatal("a report exactly window-old must be reporting (inclusive <=)")
	}
	now = now.Add(1 * time.Microsecond)
	if r.ReportingWithin("srv-1", 1*time.Nanosecond) {
		t.Fatal("a report 1us old checked against a 1ns window must not be reporting")
	}

	var nilReg *AgentPresenceRegistry
	if nilReg.ReportingWithin("srv-1", 10*time.Second) {
		t.Fatal("nil registry must report false")
	}
}

func TestAgentPresenceRegistryRetainAndNilSafe(t *testing.T) {
	r := NewAgentPresenceRegistry(0) // default window
	r.Report("a")
	r.Report("b")
	r.Retain(map[string]struct{}{"a": {}})
	if !r.Reporting("a") {
		t.Fatal("retained server dropped")
	}
	if r.Reporting("b") {
		t.Fatal("evicted server still present")
	}
	var nilReg *AgentPresenceRegistry
	nilReg.Report("x")         // must not panic
	if nilReg.Reporting("x") { // nil -> false
		t.Fatal("nil registry must report false")
	}
	nilReg.Retain(nil) // must not panic
	r.Report("")       // empty id ignored, must not panic
	if r.Reporting("") {
		t.Fatal("empty id must never be recorded")
	}
}

func TestAgentPresenceRegistryReportReactivated(t *testing.T) {
	now := time.Unix(3000, 0)
	r := NewAgentPresenceRegistry(180 * time.Second)
	r.now = func() time.Time { return now }

	// First-ever report (never seen) is a reactivation edge.
	if !r.ReportReactivated("srv-1", 10*time.Second) {
		t.Fatal("first-ever report must be a reactivation")
	}
	// A follow-up report within the window is NOT a reactivation.
	now = now.Add(5 * time.Second)
	if r.ReportReactivated("srv-1", 10*time.Second) {
		t.Fatal("report within window must not be a reactivation")
	}
	// Exactly == window is inclusive-fresh (> is strict) -> NOT a reactivation.
	now = now.Add(10 * time.Second)
	if r.ReportReactivated("srv-1", 10*time.Second) {
		t.Fatal("report exactly window-old must not be a reactivation (> is strict)")
	}
	// A gap strictly larger than the window IS a reactivation.
	now = now.Add(11 * time.Second)
	if !r.ReportReactivated("srv-1", 10*time.Second) {
		t.Fatal("report older than window must be a reactivation")
	}
	// It stamps: an immediate follow-up is fresh again.
	if r.ReportReactivated("srv-1", 10*time.Second) {
		t.Fatal("ReportReactivated must stamp so an immediate follow-up is fresh")
	}

	// nil registry and empty id are no-ops returning false.
	var nilReg *AgentPresenceRegistry
	if nilReg.ReportReactivated("srv-1", time.Second) {
		t.Fatal("nil registry must return false")
	}
	if r.ReportReactivated("", time.Second) {
		t.Fatal("empty id must return false")
	}
	if _, ok := r.seen[""]; ok {
		t.Fatal("empty id must never be recorded")
	}
}
