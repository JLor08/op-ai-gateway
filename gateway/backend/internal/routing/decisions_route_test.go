// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import "testing"

func TestEffectiveDecisionsRoute(t *testing.T) {
	cases := []struct {
		name         string
		mode         EndpointMode
		decision, oa bool
		route        EndpointMode
		ok           bool
	}{
		{"disabled", EndpointModeDisabled, false, false, EndpointModeDisabled, true},
		{"disabled with verdicts", EndpointModeDisabled, true, true, EndpointModeDisabled, true},
		{"auto prefers passthrough", EndpointModeAuto, true, true, EndpointModePassthrough, true},
		{"auto passthrough only", EndpointModeAuto, false, true, EndpointModePassthrough, true},
		{"auto translate", EndpointModeAuto, true, false, EndpointModeTranslate, true},
		{"auto neither", EndpointModeAuto, false, false, "", false},
		{"blank behaves as auto (translate)", "", true, false, EndpointModeTranslate, true},
		{"blank behaves as auto (passthrough)", "", true, true, EndpointModePassthrough, true},
		{"blank neither", "", false, false, "", false},
		{"translate with decision", EndpointModeTranslate, true, false, EndpointModeTranslate, true},
		{"translate without decision", EndpointModeTranslate, false, true, EndpointModeTranslate, false},
		{"passthrough with verdict", EndpointModePassthrough, false, true, EndpointModePassthrough, true},
		{"passthrough without verdict", EndpointModePassthrough, true, false, EndpointModePassthrough, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			route, ok := EffectiveDecisionsRoute(tc.mode, tc.decision, tc.oa)
			if route != tc.route || ok != tc.ok {
				t.Fatalf("got (%q,%v), want (%q,%v)", route, ok, tc.route, tc.ok)
			}
		})
	}
}
