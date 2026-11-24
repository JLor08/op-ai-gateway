// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/store"
	"reflect"
	"testing"
)

// TestSessionPrincipalChatVendorAccess pins that the token-less chat session's
// vendor access follows the user's stored policy: unset (”) keeps the
// historical all-own-accounts-incl-groups behaviour, an explicit policy is
// decoded verbatim (a stale account/group id is carried, not an error -- it
// simply matches nothing downstream), and a corrupt row fails closed.
func TestSessionPrincipalChatVendorAccess(t *testing.T) {
	cases := []struct {
		name   string
		stored string
		want   auth.VendorAccess
	}{
		{"unset keeps all own accounts and groups", "", auth.VendorAccess{All: true, IncludeGroups: true}},
		{"explicit no providers", `{"all":false}`, auth.VendorAccess{}},
		{"explicit all without groups", `{"all":true}`, auth.VendorAccess{All: true}},
		{"accounts and groups incl. stale ids", `{"all":false,"accounts":[{"account_id":"va_1","prefix_override":{"enabled":true,"value":"x-"}},{"account_id":"va_gone"}],"groups":[{"group_id":"vag_1"},{"group_id":"vag_gone"}]}`, auth.VendorAccess{
			Accounts: []auth.VendorAccessEntry{
				{AccountID: "va_1", OverrideEnabled: true, OverridePrefix: "x-"},
				{AccountID: "va_gone"},
			},
			Groups: []auth.VendorAccessGroupEntry{{GroupID: "vag_1"}, {GroupID: "vag_gone"}},
		}},
		{"malformed JSON fails closed", `{not json`, auth.VendorAccess{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			user := store.User{ID: "usr_1", DisplayName: "Ann", Role: "user", ChatVendorAccess: tc.stored}

			if got := sessionPrincipal(user, false).VendorAccess; !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("sessionPrincipal VendorAccess = %#v, want %#v", got, tc.want)
			}

			// The trusted-loopback chat acts for the same user and must carry
			// the same policy.
			s := newInternalAuthServer("s3cret", fakeUserLookup{"usr_1": user})
			r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			r.Header.Set(internalAuthHeaderName, "s3cret")
			r.Header.Set(internalUserHeaderName, "usr_1")
			tok, ok := s.loopbackPrincipal(r)
			if !ok {
				t.Fatal("loopback pair must authenticate")
			}
			if !reflect.DeepEqual(tok.VendorAccess, tc.want) {
				t.Fatalf("loopbackPrincipal VendorAccess = %#v, want %#v", tok.VendorAccess, tc.want)
			}
		})
	}
}
