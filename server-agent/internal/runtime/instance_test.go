// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import "testing"

// TestInstanceAddressFormat: instance 0 is the bare base name -- the address a
// spec has always had -- and every further instance is base#k.
func TestInstanceAddressFormat(t *testing.T) {
	for _, tc := range []struct {
		base string
		k    int
		want string
	}{
		{"m", 0, "m"},
		{"m", 1, "m#1"},
		{"m", 63, "m#63"},
		{"a#b", 3, "a#b#3"},
		{"rspec_x", 0, "rspec_x"},
	} {
		if got := InstanceAddress(tc.base, tc.k); got != tc.want {
			t.Errorf("InstanceAddress(%q, %d) = %q, want %q", tc.base, tc.k, got, tc.want)
		}
	}
}

// TestInstanceKeyMatchesAddressScheme: the agent's instance key for a spec is
// the spec id with the same #k suffix scheme as the model address.
func TestInstanceKeyMatchesAddressScheme(t *testing.T) {
	for _, tc := range []struct {
		specID string
		k      int
		want   string
	}{
		{"rspec_a", 0, "rspec_a"},
		{"rspec_a", 1, "rspec_a#1"},
		{"rspec_a", 63, "rspec_a#63"},
	} {
		if got := InstanceKey(tc.specID, tc.k); got != tc.want {
			t.Errorf("InstanceKey(%q, %d) = %q, want %q", tc.specID, tc.k, got, tc.want)
		}
	}
}

// TestInstanceParseAddress pins which strings are instance addresses. Only a
// decimal suffix of 1..63 after the LAST '#' (no sign, no leading zero) is one;
// everything else is a literal model name at instance 0, so a real model whose
// name happens to contain '#' keeps routing as itself.
func TestInstanceParseAddress(t *testing.T) {
	for _, tc := range []struct {
		in       string
		wantBase string
		wantK    int
	}{
		{"m", "m", 0},
		{"m#1", "m", 1},
		{"m#2", "m", 2},
		{"m#9", "m", 9},
		{"m#10", "m", 10},
		{"m#63", "m", 63},
		{"m#64", "m#64", 0},   // above the index cap
		{"m#100", "m#100", 0}, // above the index cap
		{"m#0", "m#0", 0},     // 0 is the bare name, never spelled #0
		{"m#02", "m#02", 0},   // leading zero
		{"m#00", "m#00", 0},   // leading zero
		{"m#-1", "m#-1", 0},   // sign
		{"m#+2", "m#+2", 0},   // sign
		{"m#", "m#", 0},       // empty suffix
		{"m#a", "m#a", 0},     // not a number
		{"m#2 ", "m#2 ", 0},   // stray space
		{"m# 2", "m# 2", 0},   // stray space
		{"m#1_0", "m#1_0", 0}, // Atoi-style underscore grouping is not decimal
		{"m#٣", "m#٣", 0},     // a non-ASCII digit is not a decimal digit here
		{"a#b#3", "a#b", 3},   // split on the LAST '#'
		{"a#2#3", "a#2", 3},   // the inner #2 stays part of the base
		{"a#3#b", "a#3#b", 0}, // last '#' has a non-numeric suffix
		{"foo#2#64", "foo#2#64", 0},
	} {
		base, k := ParseInstanceAddress(tc.in)
		if base != tc.wantBase || k != tc.wantK {
			t.Errorf("ParseInstanceAddress(%q) = (%q, %d), want (%q, %d)", tc.in, base, k, tc.wantBase, tc.wantK)
		}
	}
}

// TestInstanceAddressRoundTrip: every address the agent can format parses back
// to the same base and index, for every k the cap allows.
func TestInstanceAddressRoundTrip(t *testing.T) {
	for _, base := range []string{"m", "qwen-coder", "a#b"} {
		for k := 0; k <= MaxInstanceIndex; k++ {
			addr := InstanceAddress(base, k)
			gotBase, gotK := ParseInstanceAddress(addr)
			if gotBase != base || gotK != k {
				t.Fatalf("ParseInstanceAddress(%q) = (%q, %d), want (%q, %d)", addr, gotBase, gotK, base, k)
			}
		}
	}
	if MaxInstanceIndex != 63 {
		t.Fatalf("MaxInstanceIndex = %d, want 63 (the wire cap shared with the gateway)", MaxInstanceIndex)
	}
}

// TestInstanceBase: the base name of an address, given its index -- the index,
// not the shape of the name, decides whether there is a suffix to drop, so a
// literal name with '#' in it stays whole at instance 0 and keeps its own '#'
// as the base of a further instance.
func TestInstanceBase(t *testing.T) {
	for _, tc := range []struct {
		address string
		k       int
		want    string
	}{
		{"m", 0, "m"},
		{"m#2", 2, "m"},
		{"foo#2", 0, "foo#2"},
		{"foo#2#1", 1, "foo#2"},
		{"org/model#63", 63, "org/model"},
	} {
		if got := InstanceBase(tc.address, tc.k); got != tc.want {
			t.Errorf("InstanceBase(%q, %d) = %q, want %q", tc.address, tc.k, got, tc.want)
		}
	}
	for k := 0; k <= MaxInstanceIndex; k++ {
		if got := InstanceBase(InstanceAddress("a#1", k), k); got != "a#1" {
			t.Fatalf("InstanceBase(InstanceAddress(a#1, %d), %d) = %q, want a#1", k, k, got)
		}
	}
}

func TestInstanceAllowed(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec Spec
		k    int
		want bool
	}{
		{"instance 0 of a plain spec", Spec{}, 0, true},
		{"non-replicable refuses k=1", Spec{Replicable: false}, 1, false},
		{"non-replicable ignores max_instances", Spec{MaxInstances: 4}, 1, false},
		{"instance 0 of a replicable spec", Spec{Replicable: true, MaxInstances: 1}, 0, true},
		{"uncapped replicable allows k=1", Spec{Replicable: true}, 1, true},
		{"uncapped replicable allows k=63", Spec{Replicable: true}, 63, true},
		{"uncapped replicable stops at the index cap", Spec{Replicable: true}, 64, false},
		{"max=4 allows k=3", Spec{Replicable: true, MaxInstances: 4}, 3, true},
		{"max=4 refuses k=4", Spec{Replicable: true, MaxInstances: 4}, 4, false},
		{"max=1 means instance 0 only", Spec{Replicable: true, MaxInstances: 1}, 1, false},
		{"max above the index cap is still capped", Spec{Replicable: true, MaxInstances: 200}, 64, false},
		{"a negative index is never an instance", Spec{Replicable: true}, -1, false},
	} {
		if got := InstanceAllowed(tc.spec, tc.k); got != tc.want {
			t.Errorf("%s: InstanceAllowed(k=%d) = %v, want %v", tc.name, tc.k, got, tc.want)
		}
	}
}

func TestInstanceProtectedCount(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec Spec
		want int
	}{
		{"plain spec protects nothing", Spec{}, 0},
		{"replicable min=2", Spec{Replicable: true, MinInstances: 2}, 2},
		{"pinned min=0 keeps instance 0", Spec{Replicable: true, Pinned: true}, 1},
		{"force_running min=0 keeps instance 0", Spec{Replicable: true, AdminState: "force_running"}, 1},
		{"force_stopped protects nothing", Spec{Replicable: true, AdminState: "force_stopped"}, 0},
		{"the floor wins over the pin", Spec{Replicable: true, Pinned: true, MinInstances: 3}, 3},
		{"the pin wins over a zero floor", Spec{Replicable: true, Pinned: true, MinInstances: 0}, 1},
		{"non-replicable ignores min_instances", Spec{MinInstances: 3}, 0},
		{"non-replicable pinned is still instance 0", Spec{MinInstances: 3, Pinned: true}, 1},
		{"replicable with no floor and no pin", Spec{Replicable: true}, 0},
	} {
		if got := ProtectedCount(tc.spec); got != tc.want {
			t.Errorf("%s: ProtectedCount = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestInstanceIsBurst: instance 0 is never burst capacity (it is the spec's own
// process, even with no floor), and an instance is burst exactly when its index
// is at or above the protected count.
func TestInstanceIsBurst(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec Spec
		k    int
		want bool
	}{
		{"no floor: k=0 is not burst", Spec{Replicable: true}, 0, false},
		{"no floor: k=1 is burst", Spec{Replicable: true}, 1, true},
		{"min=3: k=2 is protected", Spec{Replicable: true, MinInstances: 3}, 2, false},
		{"min=3: k=3 is burst", Spec{Replicable: true, MinInstances: 3}, 3, true},
		{"min=3: k=0 is protected", Spec{Replicable: true, MinInstances: 3}, 0, false},
		{"pinned: k=0 is protected", Spec{Replicable: true, Pinned: true}, 0, false},
		{"pinned: k=1 is burst", Spec{Replicable: true, Pinned: true}, 1, true},
		{"non-replicable: k=0 is not burst", Spec{MinInstances: 3}, 0, false},
	} {
		if got := IsBurst(tc.spec, tc.k); got != tc.want {
			t.Errorf("%s: IsBurst(k=%d) = %v, want %v", tc.name, tc.k, got, tc.want)
		}
	}
}
