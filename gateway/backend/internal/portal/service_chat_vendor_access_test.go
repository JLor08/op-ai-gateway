// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/store"
	"reflect"
	"testing"
)

func TestEncodeChatSessionVendorAccess(t *testing.T) {
	fx := newTokenVendorAccessFixture(t)
	ctx := context.Background()

	t.Run("own account encodes", func(t *testing.T) {
		got, err := fx.svc.EncodeChatSessionVendorAccess(ctx, "usr_1", &VendorAccessDTO{Accounts: []VendorAccessEntryDTO{{AccountID: " acc_a "}}})
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		want := store.EncodeVendorAccess(store.DecodeVendorAccess(got))
		if got == "" || got != want {
			t.Fatalf("encoded = %q, want a non-empty canonical policy (%q)", got, want)
		}
	})
	t.Run("explicit empty policy stores all:false, never the unset marker", func(t *testing.T) {
		for _, d := range []*VendorAccessDTO{nil, {}, {Accounts: []VendorAccessEntryDTO{}}} {
			got, err := fx.svc.EncodeChatSessionVendorAccess(ctx, "usr_1", d)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if got != `{"all":false}` {
				t.Fatalf("encoded = %q, want {\"all\":false}", got)
			}
		}
	})
	t.Run("foreign account is invalid", func(t *testing.T) {
		_, err := fx.svc.EncodeChatSessionVendorAccess(ctx, "usr_1", &VendorAccessDTO{Accounts: []VendorAccessEntryDTO{{AccountID: "acc_other"}}})
		if !errors.Is(err, ErrTokenVendorAccessInvalid) {
			t.Fatalf("err = %v, want ErrTokenVendorAccessInvalid", err)
		}
	})
	t.Run("name conflict", func(t *testing.T) {
		_, err := fx.svc.EncodeChatSessionVendorAccess(ctx, "usr_1", &VendorAccessDTO{Accounts: []VendorAccessEntryDTO{
			{AccountID: "acc_a", PrefixOverride: &PrefixOverrideDTO{Enabled: true, Value: ""}},
			{AccountID: "acc_b", PrefixOverride: &PrefixOverrideDTO{Enabled: true, Value: ""}},
		}})
		if !errors.Is(err, ErrTokenVendorAccessConflict) {
			t.Fatalf("err = %v, want ErrTokenVendorAccessConflict", err)
		}
	})
}

func chatRow(t *testing.T, svc *Service, userID string) TokenDTO {
	t.Helper()
	list, err := svc.tokensForUser(context.Background(), userID)
	if err != nil {
		t.Fatalf("tokensForUser: %v", err)
	}
	for _, d := range list.Data {
		if d.IsChatSession {
			return d
		}
	}
	t.Fatal("no chat session row")
	return TokenDTO{}
}

func TestChatSessionRowVendorAccess(t *testing.T) {
	fx := newTokenVendorAccessFixture(t)
	ctx := context.Background()
	setStored := func(v string) {
		t.Helper()
		u, err := fx.dir.UserByID(ctx, "usr_1")
		if err != nil {
			t.Fatal(err)
		}
		u.ChatVendorAccess = v
		if err := fx.dir.UpdateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("unset shows all and groups", func(t *testing.T) {
		got := chatRow(t, fx.svc, "usr_1").VendorAccess
		want := &VendorAccessDTO{All: true, IncludeGroups: true, Accounts: []VendorAccessEntryDTO{}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("vendor_access = %#v, want %#v", got, want)
		}
	})
	t.Run("explicit zero policy is present with empty accounts", func(t *testing.T) {
		setStored(`{"all":false}`)
		got := chatRow(t, fx.svc, "usr_1").VendorAccess
		want := &VendorAccessDTO{All: false, Accounts: []VendorAccessEntryDTO{}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("vendor_access = %#v, want %#v", got, want)
		}
	})
	t.Run("stored policy is decoded", func(t *testing.T) {
		setStored(`{"all":false,"accounts":[{"account_id":"acc_a"}]}`)
		got := chatRow(t, fx.svc, "usr_1").VendorAccess
		if got == nil || len(got.Accounts) != 1 || got.Accounts[0].AccountID != "acc_a" {
			t.Fatalf("vendor_access = %#v", got)
		}
	})
	t.Run("failed user lookup omits the field", func(t *testing.T) {
		if got := chatRow(t, fx.svc, "usr_missing").VendorAccess; got != nil {
			t.Fatalf("vendor_access = %#v, want omitted", got)
		}
	})
}
