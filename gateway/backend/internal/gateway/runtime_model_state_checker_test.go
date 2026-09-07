// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import "testing"

// No runtime status has ever been published for the server: ok must be false, not a
// zero-value match, so the resolver falls back to per-server telemetry.
func TestRuntimeModelStateCheckerUnknownServerReportsNotOK(t *testing.T) {
	c := newRuntimeModelStateChecker(newRuntimeStatusRegistry())
	if _, _, _, ok := c.RuntimeModelState("srv-a", "qwen2.5"); ok {
		t.Fatal("a server with no published runtime status must report ok=false")
	}
}

// A published snapshot with no spec reporting the requested upstream model name (Model)
// must also report ok=false: an unrelated spec on the same server must not leak its
// state/metrics onto a different model.
func TestRuntimeModelStateCheckerNoMatchingModelReportsNotOK(t *testing.T) {
	reg := newRuntimeStatusRegistry()
	reg.publish("srv-a", []RuntimeStatusDTO{{SpecID: "spec-1", Model: "other-model", State: "running", ActiveRequests: 3, QueueDepth: 1}})
	c := newRuntimeModelStateChecker(reg)
	if _, _, _, ok := c.RuntimeModelState("srv-a", "qwen2.5"); ok {
		t.Fatal("a snapshot with no matching Model must report ok=false")
	}
}

// The common case: exactly one spec reports the requested model. Its state and live
// active/queue counts are returned verbatim.
func TestRuntimeModelStateCheckerSingleMatch(t *testing.T) {
	reg := newRuntimeStatusRegistry()
	reg.publish("srv-a", []RuntimeStatusDTO{{SpecID: "spec-1", Model: "qwen2.5", State: "starting", ActiveRequests: 0, QueueDepth: 0}})
	c := newRuntimeModelStateChecker(reg)
	state, active, queue, ok := c.RuntimeModelState("srv-a", "qwen2.5")
	if !ok || state != "starting" || active != 0 || queue != 0 {
		t.Fatalf("RuntimeModelState = (%q, %d, %d, %v), want (starting, 0, 0, true)", state, active, queue, ok)
	}
}

// Two specs report the same upstream model name (e.g. a duplicate mapping); the RUNNING
// one must win over a non-running sibling, regardless of publish order, so a live
// serving instance is never shadowed by a stale/starting duplicate.
func TestRuntimeModelStateCheckerPrefersRunningMatch(t *testing.T) {
	reg := newRuntimeStatusRegistry()
	reg.publish("srv-a", []RuntimeStatusDTO{
		{SpecID: "spec-starting", Model: "qwen2.5", State: "starting", ActiveRequests: 0, QueueDepth: 0},
		{SpecID: "spec-running", Model: "qwen2.5", State: "running", ActiveRequests: 4, QueueDepth: 2},
	})
	c := newRuntimeModelStateChecker(reg)
	state, active, queue, ok := c.RuntimeModelState("srv-a", "qwen2.5")
	if !ok || state != "running" || active != 4 || queue != 2 {
		t.Fatalf("RuntimeModelState = (%q, %d, %d, %v), want (running, 4, 2, true) -- running must win", state, active, queue, ok)
	}
}

// A server id must not leak another server's runtime status.
func TestRuntimeModelStateCheckerServerScoped(t *testing.T) {
	reg := newRuntimeStatusRegistry()
	reg.publish("srv-a", []RuntimeStatusDTO{{SpecID: "spec-1", Model: "qwen2.5", State: "running", ActiveRequests: 5, QueueDepth: 5}})
	c := newRuntimeModelStateChecker(reg)
	if _, _, _, ok := c.RuntimeModelState("srv-b", "qwen2.5"); ok {
		t.Fatal("a different server id must not see srv-a's runtime status")
	}
}

// Nil-safe on both the checker and the registry it wraps, mirroring every other
// per-server registry adapter in this package.
func TestRuntimeModelStateCheckerNilSafe(t *testing.T) {
	var nilChecker *runtimeModelStateChecker
	if _, _, _, ok := nilChecker.RuntimeModelState("srv-a", "qwen2.5"); ok {
		t.Fatal("a nil checker must report ok=false, never panic")
	}
	c := newRuntimeModelStateChecker(nil)
	if _, _, _, ok := c.RuntimeModelState("srv-a", "qwen2.5"); ok {
		t.Fatal("a checker wrapping a nil registry must report ok=false, never panic")
	}
}
