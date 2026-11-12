// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import "testing"

// The JSON body cap of POST /v1/systemone is the server's setting: ServerDeps
// carries it, a value <= 0 falls back to the 32 MiB default.
func TestDecisionsBodyCapIsASetting(t *testing.T) {
	cases := []struct {
		name string
		deps int
		want int64
	}{
		{"unset uses the default", 0, defaultDecisionsBodyBytes},
		{"negative uses the default", -5, defaultDecisionsBodyBytes},
		{"configured value is honoured", 4096, 4096},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := New(ServerDeps{MaxDecisionsBodyBytes: tc.deps})
			if got := srv.decisionsBodyLimit(); got != tc.want {
				t.Fatalf("decisionsBodyLimit() = %d, want %d", got, tc.want)
			}
		})
	}
}

// A Server never built through New (a bare &Server{}) reads as the default.
func TestDecisionsBodyLimitDefaultsOnABareServer(t *testing.T) {
	if got := (&Server{}).decisionsBodyLimit(); got != 32<<20 {
		t.Fatalf("bare Server decisionsBodyLimit() = %d, want %d", got, 32<<20)
	}
}
