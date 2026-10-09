// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"op-ai-gateway/internal/auth"
	"testing"
)

func TestTokenVendorPrefix(t *testing.T) {
	acc := VendorAccount{ID: "acc_1", ModelPrefix: "native/"}
	cases := []struct {
		name        string
		access      auth.VendorAccess
		wantPrefix  string
		wantAllowed bool
	}{
		{"all uses native", auth.VendorAccess{All: true}, "native/", true},
		{"not listed -> denied", auth.VendorAccess{}, "", false},
		{"listed, no override -> native", auth.VendorAccess{Accounts: []auth.VendorAccessEntry{{AccountID: "acc_1"}}}, "native/", true},
		{"listed, override value", auth.VendorAccess{Accounts: []auth.VendorAccessEntry{{AccountID: "acc_1", OverrideEnabled: true, OverridePrefix: "x/"}}}, "x/", true},
		{"listed, override empty -> no prefix", auth.VendorAccess{Accounts: []auth.VendorAccessEntry{{AccountID: "acc_1", OverrideEnabled: true, OverridePrefix: ""}}}, "", true},
		{"other account listed -> denied", auth.VendorAccess{Accounts: []auth.VendorAccessEntry{{AccountID: "acc_2"}}}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, ok := TokenVendorPrefix(tc.access, acc)
			if p != tc.wantPrefix || ok != tc.wantAllowed {
				t.Fatalf("= (%q,%v), want (%q,%v)", p, ok, tc.wantPrefix, tc.wantAllowed)
			}
		})
	}
}
