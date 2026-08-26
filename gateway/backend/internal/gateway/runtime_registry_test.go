// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import "testing"

func TestAgentFeaturesRegistrySetAndHas(t *testing.T) {
	r := newAgentFeaturesRegistry()
	if r.Has("srv-a", "runtime_manager") {
		t.Fatal("a server that never reported must not have any feature")
	}
	r.Set("srv-a", []string{"runtime_manager", "foo"})
	if !r.Has("srv-a", "runtime_manager") {
		t.Fatal("Has must report a just-set feature")
	}
	if r.Has("srv-a", "bar") {
		t.Fatal("Has must not report an undeclared feature")
	}
	if r.Has("srv-b", "runtime_manager") {
		t.Fatal("a DIFFERENT server's report must not leak")
	}
}

// A fresh Set call REPLACES the prior set (a telemetry sample is a full
// snapshot, never a delta) -- an agent that stops declaring a feature (e.g. a
// downgrade or an older binary reconnecting) must see Has flip back to false.
func TestAgentFeaturesRegistrySetReplacesNotMerges(t *testing.T) {
	r := newAgentFeaturesRegistry()
	r.Set("srv-a", []string{"runtime_manager"})
	r.Set("srv-a", []string{})
	if r.Has("srv-a", "runtime_manager") {
		t.Fatal("a later empty Set must clear the previously-declared feature")
	}
}

// nil-safe on every method (the package-wide convention): a bare *Server
// built directly in a test, bypassing New, must keep working.
func TestAgentFeaturesRegistryNilSafe(t *testing.T) {
	var r *agentFeaturesRegistry
	r.Set("srv-a", []string{"runtime_manager"}) // must not panic
	if r.Has("srv-a", "runtime_manager") {
		t.Fatal("a nil registry must report false, never panic or true")
	}
}

func TestRuntimeStatusRegistrySetAndIsFileMode(t *testing.T) {
	r := newRuntimeStatusRegistry()
	if r.IsFileMode("srv-a") {
		t.Fatal("a server that never reported must default to false (not file mode)")
	}
	r.SetFileMode("srv-a", true)
	if !r.IsFileMode("srv-a") {
		t.Fatal("IsFileMode must report a just-set flag")
	}
	r.SetFileMode("srv-a", false)
	if r.IsFileMode("srv-a") {
		t.Fatal("SetFileMode(false) must clear the flag")
	}
	if r.IsFileMode("srv-b") {
		t.Fatal("a DIFFERENT server's flag must not leak")
	}
}

func TestRuntimeStatusRegistryNilSafe(t *testing.T) {
	var r *runtimeStatusRegistry
	r.SetFileMode("srv-a", true) // must not panic
	if r.IsFileMode("srv-a") {
		t.Fatal("a nil registry must report false, never panic or true")
	}
}
