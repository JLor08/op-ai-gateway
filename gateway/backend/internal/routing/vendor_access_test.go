// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"op-ai-gateway/internal/auth"
	"testing"
)

func TestTokenVendorPrefix(t *testing.T) {
	// The same account id seen two ways: owned by the token's user, and owned by
	// somebody else (so, for this token, a SHARED account). All==true covers the
	// first only; the second is used only through an explicit per-account entry.
	own := VendorAccount{ID: "acc_1", OwnerUserID: "usr_me", ModelPrefix: "native/"}
	shared := VendorAccount{ID: "acc_1", OwnerUserID: "usr_other", ModelPrefix: "native/"}
	entry := func(e ...auth.VendorAccessEntry) auth.VendorAccess { return auth.VendorAccess{Accounts: e} }
	listed := auth.VendorAccessEntry{AccountID: "acc_1"}
	override := auth.VendorAccessEntry{AccountID: "acc_1", OverrideEnabled: true, OverridePrefix: "x/"}
	emptyOverride := auth.VendorAccessEntry{AccountID: "acc_1", OverrideEnabled: true, OverridePrefix: ""}
	otherListed := auth.VendorAccessEntry{AccountID: "acc_2"}

	user := func(a auth.VendorAccess) auth.Token { return auth.Token{UserID: "usr_me", VendorAccess: a} }
	service := func(a auth.VendorAccess) auth.Token {
		return auth.Token{Kind: "service", ServiceID: "svc_1", VendorAccess: a}
	}
	cases := []struct {
		name        string
		token       auth.Token
		acc         VendorAccount
		wantPrefix  string
		wantAllowed bool
	}{
		// An OWN account: the behaviour that existed before sharing, unchanged.
		{"own: all uses native", user(auth.VendorAccess{All: true}), own, "native/", true},
		{"own: all wins over a listed override", user(auth.VendorAccess{All: true, Accounts: []auth.VendorAccessEntry{override}}), own, "native/", true},
		{"own: not listed -> denied", user(auth.VendorAccess{}), own, "", false},
		{"own: listed, no override -> native", user(entry(listed)), own, "native/", true},
		{"own: listed, override value", user(entry(override)), own, "x/", true},
		{"own: listed, override empty -> no prefix", user(entry(emptyOverride)), own, "", true},
		{"own: other account listed -> denied", user(entry(otherListed)), own, "", false},

		// A SHARED account: reached only by naming it. all:true never covers it.
		{"shared: all:true does NOT cover it", user(auth.VendorAccess{All: true}), shared, "", false},
		{"shared: all:true with another account listed does not cover it", user(auth.VendorAccess{All: true, Accounts: []auth.VendorAccessEntry{otherListed}}), shared, "", false},
		{"shared: not listed -> denied", user(auth.VendorAccess{}), shared, "", false},
		{"shared: other account listed -> denied", user(entry(otherListed)), shared, "", false},
		{"shared: listed, no override -> its native prefix", user(entry(listed)), shared, "native/", true},
		{"shared: listed, override value", user(entry(override)), shared, "x/", true},
		{"shared: listed, override empty -> no prefix", user(entry(emptyOverride)), shared, "", true},
		{"shared: all:true plus a listed entry -> the entry applies", user(auth.VendorAccess{All: true, Accounts: []auth.VendorAccessEntry{override}}), shared, "x/", true},

		// A SERVICE token owns nothing: every account it can reach is shared.
		{"service: all:true covers nothing", service(auth.VendorAccess{All: true}), shared, "", false},
		{"service: not listed -> denied", service(auth.VendorAccess{}), shared, "", false},
		{"service: listed -> allowed", service(entry(listed)), shared, "native/", true},
		{"service: listed with override", service(entry(override)), shared, "x/", true},

		// No identity owns nothing, even an account whose owner id is blank.
		{"no identity: all:true does not cover an ownerless account", auth.Token{VendorAccess: auth.VendorAccess{All: true}}, VendorAccount{ID: "acc_1", ModelPrefix: "native/"}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, ok := TokenVendorPrefix(tc.token, tc.acc)
			if p != tc.wantPrefix || ok != tc.wantAllowed {
				t.Fatalf("= (%q,%v), want (%q,%v)", p, ok, tc.wantPrefix, tc.wantAllowed)
			}
		})
	}
}

// TestTokenOwnsVendorAccount: ownership is the account's owner matching the token's
// USER; a service token, or a token with no user, owns nothing, and a blank owner
// matches nobody.
func TestTokenOwnsVendorAccount(t *testing.T) {
	cases := []struct {
		name  string
		token auth.Token
		acc   VendorAccount
		want  bool
	}{
		{"owner", auth.Token{UserID: "usr_me"}, VendorAccount{OwnerUserID: "usr_me"}, true},
		{"another user", auth.Token{UserID: "usr_me"}, VendorAccount{OwnerUserID: "usr_other"}, false},
		{"service token", auth.Token{Kind: "service", ServiceID: "svc_1"}, VendorAccount{OwnerUserID: "usr_me"}, false},
		{"no identity, blank owner", auth.Token{}, VendorAccount{}, false},
		{"user, blank owner", auth.Token{UserID: "usr_me"}, VendorAccount{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := TokenOwnsVendorAccount(tc.token, tc.acc); got != tc.want {
				t.Fatalf("TokenOwnsVendorAccount = %v, want %v", got, tc.want)
			}
		})
	}
}
