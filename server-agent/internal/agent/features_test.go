// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package agent

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// semverLTE reports whether a <= b for two "x.y.z" SemVer strings (no
// pre-release/build metadata -- neither Version nor any Feature.Since ever
// carries one). Test-only: production code negotiates by NAME EQUALITY, never
// by comparing versions, so this helper exists solely to guard the registry
// itself (TestFeatureRegistry below), not to gate any runtime behavior.
func semverLTE(a, b string) bool {
	pa, oka := parseSemver(a)
	pb, okb := parseSemver(b)
	if !oka || !okb {
		return false
	}
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return true
}

func parseSemver(v string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

func TestSemverLTEHelper(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"0.1.0", "0.2.0", true},
		{"0.2.0", "0.2.0", true},
		{"0.3.0", "0.2.0", false},
		{"1.0.0", "0.9.9", false},
		{"0.9.9", "1.0.0", true},
	}
	for _, c := range cases {
		if got := semverLTE(c.a, c.b); got != c.want {
			t.Errorf("semverLTE(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
	if semverLTE("not-a-version", "0.2.0") {
		t.Error("semverLTE should reject a malformed version")
	}
}

// TestFeatureRegistry is the version guard: it fails the build the moment a
// feature entry's Since outruns agent.Version, which is exactly what
// happens when a new Feature is appended without the accompanying MINOR
// bump. It also enforces the registry's two other invariants: every name is
// snake_case, and no name repeats.
func TestFeatureRegistry(t *testing.T) {
	nameRe := regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	seen := map[string]bool{}
	for _, f := range Features {
		if !nameRe.MatchString(f.Name) {
			t.Errorf("feature name %q is not snake_case", f.Name)
		}
		if seen[f.Name] {
			t.Errorf("duplicate feature %q", f.Name)
		}
		seen[f.Name] = true
		if !semverLTE(f.Since, Version) {
			t.Errorf("feature %q Since %s > Version %s — bump agent.Version", f.Name, f.Since, Version)
		}
	}
}

func TestFeatureNamesMatchesRegistry(t *testing.T) {
	names := FeatureNames()
	if len(names) != len(Features) {
		t.Fatalf("FeatureNames() len = %d, want %d", len(names), len(Features))
	}
	for i, f := range Features {
		if names[i] != f.Name {
			t.Errorf("FeatureNames()[%d] = %q, want %q", i, names[i], f.Name)
		}
	}
}

// TestCapabilitiesJSONShape pins the wire shape: exactly one key,
// "features", carrying the registry names. agent_version must NOT appear --
// it is reported once, as the sample's own top-level field, so a version
// bump has a single place to touch.
func TestCapabilitiesJSONShape(t *testing.T) {
	raw := capabilitiesJSON()
	if raw == nil {
		t.Fatal("capabilitiesJSON() returned nil, want a valid JSON object")
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatalf("capabilitiesJSON() is not a JSON object: %v (raw=%s)", err, raw)
	}
	if _, dup := keys["agent_version"]; dup {
		t.Errorf("capabilitiesJSON() carries agent_version (%s); the version belongs on the sample's top-level agent_version field only", raw)
	}
	if len(keys) != 1 {
		t.Errorf("capabilitiesJSON() has %d keys (%s), want exactly one (features)", len(keys), raw)
	}
	var features []string
	if err := json.Unmarshal(keys["features"], &features); err != nil {
		t.Fatalf("capabilitiesJSON().features is not a string array: %v (raw=%s)", err, raw)
	}
	if len(features) != len(Features) {
		t.Fatalf("capabilitiesJSON().features = %v, want the %d registry names", features, len(Features))
	}
	for i, f := range Features {
		if features[i] != f.Name {
			t.Errorf("capabilitiesJSON().features[%d] = %q, want %q", i, features[i], f.Name)
		}
	}
}

// TestCapabilitiesJSONReturnsAFreshSliceEachCall is the aliasing guard:
// capabilitiesJSON hands out a copy of a package-level template, so a
// caller that ever wrote through the json.RawMessage it received must not
// be able to corrupt every later sample's capabilities -- the same class of
// bug sample.EmptyCapabilities exists as a function to prevent.
func TestCapabilitiesJSONReturnsAFreshSliceEachCall(t *testing.T) {
	first := capabilitiesJSON()
	want := string(first)
	for i := range first {
		first[i] = 'X'
	}
	if got := string(capabilitiesJSON()); got != want {
		t.Fatalf("after overwriting a returned capabilities slice, the next call returned %q, want %q -- capabilitiesJSON must copy, never alias the package-level template", got, want)
	}
}

// TestRuntimeConfigAckFeatureIsDeclared pins the acknowledgement feature's
// exact wire NAME and the version it ships in. The name is the whole
// negotiation: the gateway checks this literal string before it will wait
// for an acknowledgement instead of falling back to its fixed isolation
// delay, and nothing in this module gates on it -- so a rename here breaks
// the gateway silently, with no compiler and no other test to catch it.
// This is the same two-sides-of-one-contract reasoning that makes
// internal/runtime carry its own copy of the runtime_manager literal.
func TestRuntimeConfigAckFeatureIsDeclared(t *testing.T) {
	const name = "runtime_config_ack"
	for _, f := range Features {
		if f.Name != name {
			continue
		}
		if f.Since != "0.3.0" {
			t.Fatalf("feature %q Since = %q, want 0.3.0 (the branch's single bump)", name, f.Since)
		}
		return
	}
	t.Fatalf("Features does not declare %q; the gateway cannot know an acknowledgement will ever arrive: %+v", name, Features)
}
