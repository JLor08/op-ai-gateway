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

// The wire "enabled" flag is honored: an override object with enabled=false is
// NOT an active override, so presence AND enabled <=> OverrideEnabled. Encode only
// ever emits enabled=true, so a decoded policy re-encodes without the dead object.
func TestDecodeVendorAccessHonorsOverrideEnabledFlag(t *testing.T) {
	got := DecodeVendorAccess(`{"accounts":[` +
		`{"account_id":"off","prefix_override":{"enabled":false,"value":"x"}},` +
		`{"account_id":"on","prefix_override":{"enabled":true,"value":"y"}},` +
		`{"account_id":"none"}]}`)
	want := auth.VendorAccess{Accounts: []auth.VendorAccessEntry{
		{AccountID: "off"},
		{AccountID: "on", OverrideEnabled: true, OverridePrefix: "y"},
		{AccountID: "none"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Decode = %#v, want %#v", got, want)
	}
	if enc := EncodeVendorAccess(got); enc != `{"accounts":[{"account_id":"off"},{"account_id":"on","prefix_override":{"enabled":true,"value":"y"}},{"account_id":"none"}]}` {
		t.Fatalf("Encode(Decode(..)) = %q", enc)
	}
}

func TestVendorAccessGroupsRoundTrip(t *testing.T) {
	in := auth.VendorAccess{
		All:           true,
		IncludeGroups: true,
		Groups: []auth.VendorAccessGroupEntry{
			{GroupID: "vag_1"},
			{GroupID: "vag_2", OverrideEnabled: true, OverridePrefix: ""},
		},
	}
	enc := EncodeVendorAccess(in)
	if !reflect.DeepEqual(DecodeVendorAccess(enc), in) {
		t.Fatalf("Decode(Encode(%q)) = %#v, want %#v", enc, DecodeVendorAccess(enc), in)
	}
	groupsOnly := auth.VendorAccess{Groups: []auth.VendorAccessGroupEntry{{GroupID: "vag_1", OverrideEnabled: true, OverridePrefix: "p/"}}}
	if got := DecodeVendorAccess(EncodeVendorAccess(groupsOnly)); !reflect.DeepEqual(got, groupsOnly) {
		t.Fatalf("groups-only round trip = %#v, want %#v", got, groupsOnly)
	}
	if got := EncodeVendorAccess(auth.VendorAccess{}); got != "" {
		t.Fatalf("zero policy encodes to %q, want empty", got)
	}
}

func TestDecodeVendorAccessLegacyAndGroupTolerance(t *testing.T) {
	old := DecodeVendorAccess(`{"all":true}`)
	if !old.All || old.IncludeGroups || old.Groups != nil {
		t.Fatalf("legacy row = %#v", old)
	}
	got := DecodeVendorAccess(`{"groups":[{"prefix_override":{"enabled":true}}]}`)
	if got.Groups != nil || len(got.Groups) != 0 {
		t.Fatalf("id-less group entry kept: %#v", got.Groups)
	}
	if got := EncodeVendorAccess(got); got != "" {
		t.Fatalf("re-encode = %q, want empty", got)
	}
}
