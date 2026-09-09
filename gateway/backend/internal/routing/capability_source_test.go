// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import "testing"

// TestCapabilitySourceIsAuthoritative pins the operator-facing safety
// property the whole capability design rests on: a probe must never
// overwrite a verdict a human or a real measurement established. This
// function is the single place that rule is spelled out (every write path
// asks it rather than repeating the comparison), so a change here silently
// changes what "authoritative" means everywhere at once -- it had no test
// before this one.
func TestCapabilitySourceIsAuthoritative(t *testing.T) {
	cases := []struct {
		name   string
		source string
		want   bool
	}{
		{"manual is authoritative", CapabilitySourceManual, true},
		{"vision_benchmark is authoritative", CapabilitySourceVisionBenchmark, true},
		{"llama_cpp_props (a probe) is not authoritative", CapabilitySourceLlamaCppProps, false},
		{"legacy (a migrated guess) is not authoritative", CapabilitySourceLegacy, false},
		{"empty source is not authoritative", "", false},
		{"an unknown source is not authoritative", "some_unknown_future_source", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CapabilitySourceIsAuthoritative(tc.source); got != tc.want {
				t.Fatalf("CapabilitySourceIsAuthoritative(%q) = %v, want %v", tc.source, got, tc.want)
			}
		})
	}
}
