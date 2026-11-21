// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"errors"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// TestSetVendorAccountShareUsage: the owner-only opt-in (#407) to show an account's
// usage figures to everyone allowed to use it. The owner's rule is the raise gate's:
// a CM or a member can see the account and get a 403, a stranger, an unknown id and
// system scope (a stranger for every write) get the no-leak 404.
func TestSetVendorAccountShareUsage(t *testing.T) {
	e := newVendorMemberEnv(t)
	acc := e.accounts[routing.VendorShareLevelShared]
	e.link(acc.ID)
	owner := token(vendorAuthzOwnerID)

	stored := func() bool {
		t.Helper()
		got, err := e.routes.VendorAccountByID(e.ctx, acc.ID)
		if err != nil {
			t.Fatalf("VendorAccountByID: %v", err)
		}
		return got.ShareUsageWithMembers
	}

	dto, err := e.svc.SetVendorAccountShareUsage(e.ctx, owner, acc.ID, SetVendorAccountShareUsageRequest{Enabled: true})
	if err != nil {
		t.Fatalf("owner on: %v", err)
	}
	if !dto.ShareUsageWithMembers || !stored() || !dto.IsOwner {
		t.Errorf("owner on: dto %+v, stored %v, want the management DTO with the opt-in set", dto, stored())
	}
	dto, err = e.svc.SetVendorAccountShareUsage(e.ctx, owner, acc.ID, SetVendorAccountShareUsageRequest{Enabled: false})
	if err != nil {
		t.Fatalf("owner off: %v", err)
	}
	if dto.ShareUsageWithMembers || stored() {
		t.Errorf("owner off: dto %+v, stored %v, want the opt-in cleared", dto, stored())
	}

	denied := []struct {
		name string
		tok  func() error
		want error
	}{
		{"cm", func() error {
			_, err := e.svc.SetVendorAccountShareUsage(e.ctx, token("usr_cm", "admin"), acc.ID, SetVendorAccountShareUsageRequest{Enabled: true})
			return err
		}, ErrVendorAccountForbidden},
		{"member", func() error {
			_, err := e.svc.SetVendorAccountShareUsage(e.ctx, e.member, acc.ID, SetVendorAccountShareUsageRequest{Enabled: true})
			return err
		}, ErrVendorAccountForbidden},
		{"stranger", func() error {
			_, err := e.svc.SetVendorAccountShareUsage(e.ctx, token("usr_stranger"), acc.ID, SetVendorAccountShareUsageRequest{Enabled: true})
			return err
		}, ErrVendorAccountNotFound},
		{"system scope", func() error {
			_, err := e.svc.SetVendorAccountShareUsage(e.ctx, token("usr_s", "system", "admin"), acc.ID, SetVendorAccountShareUsageRequest{Enabled: true})
			return err
		}, ErrVendorAccountNotFound},
		{"unknown id", func() error {
			_, err := e.svc.SetVendorAccountShareUsage(e.ctx, owner, "vacc_does_not_exist", SetVendorAccountShareUsageRequest{Enabled: true})
			return err
		}, ErrVendorAccountNotFound},
	}
	for _, tc := range denied {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.tok(); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
			if stored() {
				t.Error("a refused call wrote the opt-in")
			}
		})
	}
}

// TestSetVendorAccountShareUsageAtAnyLevel: the opt-in is not gated on the share
// level. The owner of a private (`none`) account can set it; it is stored and changes
// nothing for anyone else while the account is not shared.
func TestSetVendorAccountShareUsageAtAnyLevel(t *testing.T) {
	e := newVendorMemberEnv(t)
	acc := e.accounts[routing.VendorShareLevelNone]
	e.link(acc.ID) // a leftover link grants nothing at `none`
	if err := e.routes.UpsertVendorAccountUsage(e.ctx, routing.VendorAccountUsage{
		AccountID: acc.ID, FiveHourPct: 42, UpdatedAt: e.now,
	}); err != nil {
		t.Fatalf("UpsertVendorAccountUsage: %v", err)
	}
	dto, err := e.svc.SetVendorAccountShareUsage(e.ctx, token(vendorAuthzOwnerID), acc.ID, SetVendorAccountShareUsageRequest{Enabled: true})
	if err != nil {
		t.Fatalf("owner on a private account: %v", err)
	}
	got, err := e.routes.VendorAccountByID(e.ctx, acc.ID)
	if err != nil {
		t.Fatalf("VendorAccountByID: %v", err)
	}
	if !dto.ShareUsageWithMembers || !got.ShareUsageWithMembers || got.ShareLevel != routing.VendorShareLevelNone {
		t.Errorf("dto %+v, stored opt-in %v, level %q: want the opt-in stored and the level still none", dto, got.ShareUsageWithMembers, got.ShareLevel)
	}
	// Nobody else gains anything: the member still cannot read the private account,
	// and the list does not carry it.
	if _, err := e.svc.GetVendorAccount(e.ctx, e.member, acc.ID); !errors.Is(err, ErrVendorAccountNotFound) {
		t.Errorf("member read of a private opted-in account = %v, want ErrVendorAccountNotFound", err)
	}
	list, err := e.svc.ListVendorAccounts(e.ctx, e.member)
	if err != nil {
		t.Fatalf("member list: %v", err)
	}
	for _, row := range list.Data {
		if row.ID == acc.ID {
			t.Errorf("the member's list carries the private account: %+v", row)
		}
	}
}

// TestSetVendorAccountShareUsageIsRefusedWhileVendorAccountsAreOff: the module flag
// answers first, as for every vendor-account write.
func TestSetVendorAccountShareUsageIsRefusedWhileVendorAccountsAreOff(t *testing.T) {
	e := newVendorMemberEnv(t)
	acc := e.accounts[routing.VendorShareLevelShared]
	setVendorAccountsEnabled(t, e.svc, false)
	_, err := e.svc.SetVendorAccountShareUsage(e.ctx, token(vendorAuthzOwnerID), acc.ID, SetVendorAccountShareUsageRequest{Enabled: true})
	if !errors.Is(err, ErrVendorAccountsDisabled) {
		t.Errorf("err = %v, want ErrVendorAccountsDisabled", err)
	}
}

// TestMemberVendorAccountDTOUsageFollowsOptIn: a member's single read and the list's
// member row (one builder, memberVendorAccountDTO) carry the usage snapshot exactly
// while the owner's opt-in is on, and the member view never reports the flag itself.
func TestMemberVendorAccountDTOUsageFollowsOptIn(t *testing.T) {
	e := newVendorMemberEnv(t)
	acc := e.accounts[routing.VendorShareLevelShared]
	e.link(acc.ID)
	reset := e.now.Add(time.Hour)
	if err := e.routes.UpsertVendorAccountUsage(e.ctx, routing.VendorAccountUsage{
		AccountID: acc.ID, FiveHourPct: 42, FiveHourResetAt: &reset, WeeklyPct: 7, UpdatedAt: e.now,
	}); err != nil {
		t.Fatalf("UpsertVendorAccountUsage: %v", err)
	}

	for _, on := range []bool{true, false} {
		if err := e.routes.UpdateVendorAccountShareUsage(e.ctx, acc.ID, on); err != nil {
			t.Fatalf("UpdateVendorAccountShareUsage(%v): %v", on, err)
		}
		single, err := e.svc.GetVendorAccount(e.ctx, e.member, acc.ID)
		if err != nil {
			t.Fatalf("member read: %v", err)
		}
		list, err := e.svc.ListVendorAccounts(e.ctx, e.member)
		if err != nil {
			t.Fatalf("member list: %v", err)
		}
		var row *VendorAccountDTO
		for i := range list.Data {
			if list.Data[i].ID == acc.ID {
				row = &list.Data[i]
			}
		}
		if row == nil {
			t.Fatalf("the member's list lacks %s", acc.ID)
		}
		for label, dto := range map[string]VendorAccountDTO{"read": single, "list row": *row} {
			if got := dto.Usage != nil; got != on {
				t.Errorf("opt-in %v, %s: usage present = %v, want %v", on, label, got, on)
			}
			if on && (dto.Usage == nil || dto.Usage.FiveHourPct != 42 || dto.Usage.WeeklyPct != 7) {
				t.Errorf("opt-in on, %s: usage = %+v, want the stored snapshot", label, dto.Usage)
			}
			if dto.ShareUsageWithMembers != on {
				t.Errorf("opt-in %v, %s: share_usage_with_members = %v, want the account's value", on, label, dto.ShareUsageWithMembers)
			}
		}
	}
}
