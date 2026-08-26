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

func TestActiveFeaturesIsIntersection(t *testing.T) {
	got := ActiveFeatures([]string{"runtime_manager", "unknown_future"})
	if len(got) != 1 || got[0] != "runtime_manager" {
		t.Fatalf("got %v", got)
	}
	if len(ActiveFeatures(nil)) != 0 {
		t.Fatal("empty gateway set must disable everything")
	}
	if len(ActiveFeatures([]string{"unknown_future"})) != 0 {
		t.Fatal("a gateway set with no overlap must disable everything")
	}
}

func TestCapabilitiesJSONShape(t *testing.T) {
	raw := capabilitiesJSON()
	if raw == nil {
		t.Fatal("capabilitiesJSON() returned nil, want a valid JSON object")
	}
	var v struct {
		Features     []string `json:"features"`
		AgentVersion string   `json:"agent_version"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if v.AgentVersion != Version || len(v.Features) == 0 {
		t.Fatalf("%+v", v)
	}
}
