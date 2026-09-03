// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import "testing"

func TestEndpointModeValid(t *testing.T) {
	for _, m := range []EndpointMode{EndpointModeDisabled, EndpointModeTranslate, EndpointModePassthrough} {
		if !m.Valid() {
			t.Errorf("%q should be valid", m)
		}
	}
	for _, m := range []EndpointMode{"", "PASSTHROUGH", "proxy", "native"} {
		if m.Valid() {
			t.Errorf("%q should be invalid", m)
		}
	}
}

func TestParseEndpointMode(t *testing.T) {
	cases := []struct {
		in      string
		want    EndpointMode
		wantErr bool
	}{
		{"disabled", EndpointModeDisabled, false},
		{"translate", EndpointModeTranslate, false},
		{"passthrough", EndpointModePassthrough, false},
		{"  Passthrough ", EndpointModePassthrough, false},
		{"", DefaultEndpointMode, false},
		{"native", "", true},
	}
	for _, c := range cases {
		got, err := ParseEndpointMode(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseEndpointMode(%q): want error, got %q", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseEndpointMode(%q): unexpected error %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("ParseEndpointMode(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestEndpointModeOrDefault(t *testing.T) {
	if got := EndpointMode("").OrDefault(); got != EndpointModePassthrough {
		t.Errorf(`("").OrDefault() = %q, want passthrough`, got)
	}
	if got := EndpointModeTranslate.OrDefault(); got != EndpointModeTranslate {
		t.Errorf("translate.OrDefault() = %q, want translate", got)
	}
}
