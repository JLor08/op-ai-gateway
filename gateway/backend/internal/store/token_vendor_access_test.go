// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"op-ai-gateway/internal/auth"
	"reflect"
	"testing"
)

func TestVendorAccessRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   auth.VendorAccess
		want string // canonical encoded form
	}{
		{"strict default", auth.VendorAccess{}, ""},
		{"all", auth.VendorAccess{All: true}, `{"all":true}`},
		{"explicit native", auth.VendorAccess{Accounts: []auth.VendorAccessEntry{{AccountID: "acc_1"}}}, `{"accounts":[{"account_id":"acc_1"}]}`},
		{"explicit override value", auth.VendorAccess{Accounts: []auth.VendorAccessEntry{{AccountID: "acc_1", OverrideEnabled: true, OverridePrefix: "foo/"}}}, `{"accounts":[{"account_id":"acc_1","prefix_override":{"enabled":true,"value":"foo/"}}]}`},
		{"explicit override empty", auth.VendorAccess{Accounts: []auth.VendorAccessEntry{{AccountID: "acc_1", OverrideEnabled: true, OverridePrefix: ""}}}, `{"accounts":[{"account_id":"acc_1","prefix_override":{"enabled":true,"value":""}}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := EncodeVendorAccess(tc.in)
			if got != tc.want {
				t.Fatalf("Encode = %q, want %q", got, tc.want)
			}
			back := DecodeVendorAccess(got)
			if !reflect.DeepEqual(back, tc.in) {
				t.Fatalf("Decode(Encode) = %#v, want %#v", back, tc.in)
			}
		})
	}
}

func TestDecodeVendorAccessTolerant(t *testing.T) {
	for _, s := range []string{"", "   ", "not json", "{", `{"accounts":[{"account_id":""}]}`} {
		got := DecodeVendorAccess(s)
		if got.All || len(got.Accounts) != 0 {
			t.Fatalf("DecodeVendorAccess(%q) = %#v, want strict default", s, got)
		}
	}
}
