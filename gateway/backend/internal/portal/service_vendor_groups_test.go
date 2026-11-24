// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

const (
	groupOwn2    = "vacc_g_zz_own2" // a second account owned by grantDirect
	groupOwnAnth = "vacc_g_zz_anth" // an anthropic account owned by grantDirect
)

// vendorGroupEnv is the grant fixture with grantDirect as the group owner: it owns
// grantOwnAccount and groupOwn2 (groupOwnAnth, an anthropic account that clashes by
// name with everything, is added only by the tests that need a vendor mismatch), and reaches grantShared, grantFull and
// grantDup through the granted resource group (at share levels shared/full, usage not
// shared with members).
type vendorGroupEnv struct {
	*vendorGrantEnv
	owner     auth.Token
	cooldowns *routing.VendorLimitCooldowns
}

func newVendorGroupEnv(t *testing.T) *vendorGroupEnv {
	t.Helper()
	e := &vendorGroupEnv{vendorGrantEnv: newVendorListingEnv(t), owner: auth.Token{UserID: grantDirect}}
	e.account(groupOwn2, grantDirect, routing.VendorAuthAPIKey, routing.VendorShareLevelNone)
	e.cooldowns = routing.NewVendorLimitCooldowns()
	e.svc.SetVendorLimitCooldowns(e.cooldowns)
	return e
}

func (e *vendorGroupEnv) create(prefix string, ids ...string) VendorAccountGroupDTO {
	e.t.Helper()
	dto, err := e.svc.CreateVendorAccountGroup(e.ctx, e.owner, CreateVendorAccountGroupRequest{
		Name: "Pool", Vendor: routing.VendorOpenAI, ModelPrefix: prefix, AccountIDs: ids,
	})
	if err != nil {
		e.t.Fatalf("CreateVendorAccountGroup(%q, %v): %v", prefix, ids, err)
	}
	return dto
}

func (e *vendorGroupEnv) usage(id string, fiveHour float64, reset *time.Time) {
	e.t.Helper()
	if err := e.routes.UpsertVendorAccountUsage(e.ctx, routing.VendorAccountUsage{
		AccountID: id, FiveHourPct: fiveHour, FiveHourResetAt: reset, WeeklyPct: -1, SpendUsedPct: -1, UpdatedAt: e.now,
	}); err != nil {
		e.t.Fatalf("UpsertVendorAccountUsage: %v", err)
	}
}

func requireGroupErr(t *testing.T, label string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Errorf("%s: err = %v, want %v", label, err, want)
	}
}

func requireMember(t *testing.T, g VendorAccountGroupDTO, pos int, id, state, reason string) VendorAccountGroupMemberDTO {
	t.Helper()
	if pos > len(g.Members) {
		t.Fatalf("group has %d members, want a member at %d", len(g.Members), pos)
	}
	m := g.Members[pos-1]
	if m.AccountID != id || m.Position != pos || m.State != state || m.Reason != reason {
		t.Errorf("member %d = {%s pos %d %s/%s}, want {%s pos %d %s/%s}", pos, m.AccountID, m.Position, m.State, m.Reason, id, pos, state, reason)
	}
	return m
}

func TestVendorAccountGroupCreateDefaults(t *testing.T) {
	e := newVendorGroupEnv(t)
	g := e.create("pool/", grantOwnAccount, grantShared)
	if len(g.ID) < 5 || g.ID[:4] != "vag_" || g.Vendor != routing.VendorOpenAI || g.Name != "Pool" || g.ModelPrefix != "pool/" {
		t.Errorf("group = %+v", g)
	}
	if g.Status != routing.VendorAccountGroupStatusActive {
		t.Errorf("status = %q, want active", g.Status)
	}
	if g.Thresholds != (VendorGroupThresholdsDTO{FiveHourPct: 99, WeeklyPct: 99, CreditsPct: 99}) {
		t.Errorf("thresholds = %+v, want 99/99/99", g.Thresholds)
	}
	if g.ActiveMemberID != grantOwnAccount {
		t.Errorf("active member = %q, want %q", g.ActiveMemberID, grantOwnAccount)
	}
	own := requireMember(t, g, 1, grantOwnAccount, "active", "")
	if own.OwnerName != "" {
		t.Errorf("own member owner name = %q, want empty", own.OwnerName)
	}
	shared := requireMember(t, g, 2, grantShared, "available", "")
	if shared.OwnerName != grantOwner {
		t.Errorf("shared member owner name = %q, want %q", shared.OwnerName, grantOwner)
	}
	if g.CreatedAt.IsZero() || g.UpdatedAt.IsZero() {
		t.Errorf("timestamps not set: %+v", g)
	}
	list, err := e.svc.ListVendorAccountGroups(e.ctx, e.owner)
	if err != nil || len(list.Groups) != 1 || list.Groups[0].ID != g.ID {
		t.Fatalf("list = %+v, %v", list, err)
	}
	got, err := e.svc.GetVendorAccountGroup(e.ctx, e.owner, g.ID)
	if err != nil || got.ID != g.ID || len(got.Members) != 2 {
		t.Fatalf("get = %+v, %v", got, err)
	}
}

func TestVendorAccountGroupValidation(t *testing.T) {
	e := newVendorGroupEnv(t)
	base := CreateVendorAccountGroupRequest{Name: "P", Vendor: routing.VendorOpenAI, ModelPrefix: "p/"}
	bad := func(label string, mutate func(*CreateVendorAccountGroupRequest), want error) {
		t.Helper()
		req := base
		mutate(&req)
		_, err := e.svc.CreateVendorAccountGroup(e.ctx, e.owner, req)
		requireGroupErr(t, label, err, want)
	}
	zero := 0
	over := 101
	bad("blank name", func(r *CreateVendorAccountGroupRequest) { r.Name = " " }, ErrVendorGroupNameRequired)
	bad("vendor", func(r *CreateVendorAccountGroupRequest) { r.Vendor = "nope" }, ErrVendorGroupVendorInvalid)
	bad("prefix", func(r *CreateVendorAccountGroupRequest) { r.ModelPrefix = "a b" }, ErrVendorGroupModelPrefixInvalid)
	bad("status", func(r *CreateVendorAccountGroupRequest) { r.Status = "needs_reconnect" }, ErrVendorGroupStatusInvalid)
	bad("threshold 0", func(r *CreateVendorAccountGroupRequest) { r.ThresholdFiveHourPct = &zero }, ErrVendorGroupThresholdInvalid)
	bad("threshold 101", func(r *CreateVendorAccountGroupRequest) { r.ThresholdCreditsPct = &over }, ErrVendorGroupThresholdInvalid)
	e.account(groupOwnAnth, grantDirect, routing.VendorAuthSubscription, routing.VendorShareLevelNone)
	bad("vendor mismatch", func(r *CreateVendorAccountGroupRequest) { r.AccountIDs = []string{groupOwnAnth} }, ErrVendorGroupMemberVendorMismatch)
	bad("unreachable", func(r *CreateVendorAccountGroupRequest) { r.AccountIDs = []string{grantStale} }, ErrVendorGroupMemberUnreachable)
	bad("unknown account", func(r *CreateVendorAccountGroupRequest) { r.AccountIDs = []string{"vacc_nope"} }, ErrVendorGroupMemberUnreachable)
	bad("duplicate", func(r *CreateVendorAccountGroupRequest) { r.AccountIDs = []string{grantOwnAccount, grantOwnAccount} }, ErrVendorGroupMemberDuplicate)
	if list, _ := e.svc.ListVendorAccountGroups(e.ctx, e.owner); len(list.Groups) != 0 {
		t.Errorf("a refused create stored %d groups", len(list.Groups))
	}
	if _, err := e.svc.CreateVendorAccountGroup(e.ctx, auth.Token{}, base); !errors.Is(err, ErrVendorGroupForbidden) {
		t.Errorf("create without a user = %v, want forbidden", err)
	}
}

func TestVendorAccountGroupModuleFlagOff(t *testing.T) {
	e := newVendorGroupEnv(t)
	g := e.create("p/", grantOwnAccount)
	setVendorSetting(t, e.svc, vendorAccountsEnabledKey, false)
	_, err := e.svc.ListVendorAccountGroups(e.ctx, e.owner)
	requireGroupErr(t, "list", err, ErrVendorAccountsDisabled)
	_, err = e.svc.GetVendorAccountGroup(e.ctx, e.owner, g.ID)
	requireGroupErr(t, "get", err, ErrVendorAccountsDisabled)
	_, err = e.svc.CreateVendorAccountGroup(e.ctx, e.owner, CreateVendorAccountGroupRequest{Name: "x", Vendor: routing.VendorOpenAI})
	requireGroupErr(t, "create", err, ErrVendorAccountsDisabled)
	_, err = e.svc.UpdateVendorAccountGroup(e.ctx, e.owner, g.ID, UpdateVendorAccountGroupRequest{})
	requireGroupErr(t, "update", err, ErrVendorAccountsDisabled)
	requireGroupErr(t, "delete", e.svc.DeleteVendorAccountGroup(e.ctx, e.owner, g.ID), ErrVendorAccountsDisabled)
	_, err = e.svc.SetVendorAccountGroupMembers(e.ctx, e.owner, g.ID, nil)
	requireGroupErr(t, "members", err, ErrVendorAccountsDisabled)
}

func TestVendorAccountGroupStrangerSeesNothing(t *testing.T) {
	e := newVendorGroupEnv(t)
	g := e.create("p/", grantOwnAccount)
	stranger := auth.Token{UserID: grantStranger}
	for _, id := range []string{g.ID, "vag_missing"} {
		_, err := e.svc.GetVendorAccountGroup(e.ctx, stranger, id)
		requireGroupErr(t, "get "+id, err, ErrVendorGroupNotFound)
		name := "x"
		_, err = e.svc.UpdateVendorAccountGroup(e.ctx, stranger, id, UpdateVendorAccountGroupRequest{Name: &name})
		requireGroupErr(t, "update "+id, err, ErrVendorGroupNotFound)
		requireGroupErr(t, "delete "+id, e.svc.DeleteVendorAccountGroup(e.ctx, stranger, id), ErrVendorGroupNotFound)
		_, err = e.svc.SetVendorAccountGroupMembers(e.ctx, stranger, id, nil)
		requireGroupErr(t, "members "+id, err, ErrVendorGroupNotFound)
	}
	list, err := e.svc.ListVendorAccountGroups(e.ctx, stranger)
	if err != nil || len(list.Groups) != 0 {
		t.Errorf("stranger list = %+v, %v", list, err)
	}
	if got, _ := e.svc.GetVendorAccountGroup(e.ctx, e.owner, g.ID); got.Name != "Pool" {
		t.Errorf("a refused write changed the group: %+v", got)
	}
}

func TestVendorAccountGroupUpdateAndDelete(t *testing.T) {
	e := newVendorGroupEnv(t)
	g := e.create("p/", grantOwnAccount)
	name, status, prefix := "Renamed", routing.VendorAccountGroupStatusDisabled, "q/"
	five := 80
	got, err := e.svc.UpdateVendorAccountGroup(e.ctx, e.owner, g.ID, UpdateVendorAccountGroupRequest{
		Name: &name, Status: &status, ModelPrefix: &prefix, ThresholdFiveHourPct: &five,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if got.Name != name || got.Status != status || got.ModelPrefix != prefix || got.Thresholds.FiveHourPct != 80 || got.Thresholds.WeeklyPct != 99 || got.Vendor != routing.VendorOpenAI {
		t.Errorf("updated = %+v", got)
	}
	bad := "a b"
	_, err = e.svc.UpdateVendorAccountGroup(e.ctx, e.owner, g.ID, UpdateVendorAccountGroupRequest{ModelPrefix: &bad})
	requireGroupErr(t, "bad prefix", err, ErrVendorGroupModelPrefixInvalid)
	blank := " "
	_, err = e.svc.UpdateVendorAccountGroup(e.ctx, e.owner, g.ID, UpdateVendorAccountGroupRequest{Name: &blank})
	requireGroupErr(t, "blank name", err, ErrVendorGroupNameRequired)
	if err := e.svc.DeleteVendorAccountGroup(e.ctx, e.owner, g.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, err = e.svc.GetVendorAccountGroup(e.ctx, e.owner, g.ID)
	requireGroupErr(t, "get after delete", err, ErrVendorGroupNotFound)
}

func TestVendorAccountGroupPrefixConflict(t *testing.T) {
	e := newVendorGroupEnv(t)
	// Both of grantDirect's openai accounts serve dup-model under no prefix. A group
	// holding only one of them under "" would shadow the other: refused.
	_, err := e.svc.CreateVendorAccountGroup(e.ctx, e.owner, CreateVendorAccountGroupRequest{
		Name: "G", Vendor: routing.VendorOpenAI, AccountIDs: []string{grantOwnAccount},
	})
	requireGroupErr(t, "outside account", err, ErrVendorGroupPrefixConflict)
	// Colliding only with its own members is fine: the group shadows them.
	g := e.create("", grantOwnAccount, groupOwn2)
	// Another group's names are protected too.
	_, err = e.svc.CreateVendorAccountGroup(e.ctx, e.owner, CreateVendorAccountGroupRequest{
		Name: "H", Vendor: routing.VendorOpenAI, AccountIDs: []string{grantShared},
	})
	requireGroupErr(t, "other group", err, ErrVendorGroupPrefixConflict)
	// A distinct prefix has no conflict even over the same accounts.
	e.create("alt/", grantOwnAccount)
	// A prefix update to a free prefix works, and so does one back to "" over both
	// members; a member removal that leaves an own account outside the group under the
	// same name is refused.
	prefix := "beta/"
	if _, err := e.svc.UpdateVendorAccountGroup(e.ctx, e.owner, g.ID, UpdateVendorAccountGroupRequest{ModelPrefix: &prefix}); err != nil {
		t.Fatalf("update to a free prefix: %v", err)
	}
	empty := ""
	_, err = e.svc.UpdateVendorAccountGroup(e.ctx, e.owner, g.ID, UpdateVendorAccountGroupRequest{ModelPrefix: &empty})
	if err != nil {
		t.Fatalf("back to the empty prefix over both members: %v", err)
	}
	_, err = e.svc.SetVendorAccountGroupMembers(e.ctx, e.owner, g.ID, []string{grantOwnAccount})
	requireGroupErr(t, "member removal", err, ErrVendorGroupPrefixConflict)
}

func TestVendorAccountGroupSetMembersOrders(t *testing.T) {
	e := newVendorGroupEnv(t)
	g := e.create("p/", grantOwnAccount, groupOwn2)
	got, err := e.svc.SetVendorAccountGroupMembers(e.ctx, e.owner, g.ID, []string{groupOwn2, grantShared, grantOwnAccount})
	if err != nil {
		t.Fatalf("set members: %v", err)
	}
	requireMember(t, got, 1, groupOwn2, "active", "")
	requireMember(t, got, 2, grantShared, "available", "")
	requireMember(t, got, 3, grantOwnAccount, "available", "")
	_, err = e.svc.SetVendorAccountGroupMembers(e.ctx, e.owner, g.ID, []string{groupOwn2, groupOwn2})
	requireGroupErr(t, "duplicate", err, ErrVendorGroupMemberDuplicate)
	_, err = e.svc.SetVendorAccountGroupMembers(e.ctx, e.owner, g.ID, []string{grantStale})
	requireGroupErr(t, "unreachable", err, ErrVendorGroupMemberUnreachable)
	e.account(groupOwnAnth, grantDirect, routing.VendorAuthSubscription, routing.VendorShareLevelNone)
	_, err = e.svc.SetVendorAccountGroupMembers(e.ctx, e.owner, g.ID, []string{groupOwnAnth})
	requireGroupErr(t, "vendor mismatch", err, ErrVendorGroupMemberVendorMismatch)
	again, _ := e.svc.GetVendorAccountGroup(e.ctx, e.owner, g.ID)
	if len(again.Members) != 3 || again.Members[0].AccountID != groupOwn2 {
		t.Errorf("a refused set changed the members: %+v", again.Members)
	}
	cleared, err := e.svc.SetVendorAccountGroupMembers(e.ctx, e.owner, g.ID, []string{})
	if err != nil || len(cleared.Members) != 0 || cleared.ActiveMemberID != "" {
		t.Errorf("clear = %+v, %v", cleared, err)
	}
}

func TestVendorAccountGroupMemberStates(t *testing.T) {
	t.Run("over five-hour threshold with a reset", func(t *testing.T) {
		e := newVendorGroupEnv(t)
		reset := e.now.Add(time.Hour)
		e.usage(grantOwnAccount, 99.5, &reset)
		g := e.create("p/", grantOwnAccount, groupOwn2)
		if g.ActiveMemberID != groupOwn2 {
			t.Errorf("active = %q, want %q", g.ActiveMemberID, groupOwn2)
		}
		m := requireMember(t, g, 1, grantOwnAccount, "over_threshold", "five_hour")
		if m.Pct == nil || *m.Pct != 99.5 || m.FreeAt == nil || !m.FreeAt.Equal(reset) || m.Usage == nil {
			t.Errorf("over member = %+v", m)
		}
		requireMember(t, g, 2, groupOwn2, "active", "")
	})
	t.Run("stale snapshot past its reset counts as renewed", func(t *testing.T) {
		e := newVendorGroupEnv(t)
		past := e.now.Add(-time.Minute)
		e.usage(grantOwnAccount, 99.5, &past)
		g := e.create("p/", grantOwnAccount, groupOwn2)
		requireMember(t, g, 1, grantOwnAccount, "active", "")
		requireMember(t, g, 2, groupOwn2, "available", "")
	})
	t.Run("shared member without usage opt-in", func(t *testing.T) {
		e := newVendorGroupEnv(t)
		e.usage(grantShared, 100, nil)
		g := e.create("p/", grantShared)
		m := requireMember(t, g, 1, grantShared, "active", "")
		if m.Pct != nil || m.Usage != nil || m.FreeAt != nil {
			t.Errorf("a member the owner does not share usage of exposes figures: %+v", m)
		}
		if err := e.routes.UpdateVendorAccountShareUsage(e.ctx, grantShared, true); err != nil {
			t.Fatal(err)
		}
		g, _ = e.svc.GetVendorAccountGroup(e.ctx, e.owner, g.ID)
		// With the opt-in the figure counts and (the only member) the fallback keeps it active.
		m = requireMember(t, g, 1, grantShared, "active", "")
		if m.Usage == nil {
			t.Errorf("opted-in shared member lacks usage: %+v", m)
		}
	})
	t.Run("disabled and revoked members", func(t *testing.T) {
		e := newVendorGroupEnv(t)
		g := e.create("p/", groupOwn2, grantShared, grantOwnAccount)
		acc, _ := e.routes.VendorAccountByID(e.ctx, groupOwn2)
		acc.Status = routing.VendorAccountStatusDisabled
		if err := e.routes.UpdateVendorAccount(e.ctx, acc); err != nil {
			t.Fatal(err)
		}
		e.setLevel(grantShared, routing.VendorShareLevelNone)
		g, _ = e.svc.GetVendorAccountGroup(e.ctx, e.owner, g.ID)
		requireMember(t, g, 1, groupOwn2, "not_usable", "disabled")
		requireMember(t, g, 2, grantShared, "not_usable", "not_reachable")
		requireMember(t, g, 3, grantOwnAccount, "active", "")
		if g.ActiveMemberID != grantOwnAccount {
			t.Errorf("active = %q", g.ActiveMemberID)
		}
	})
	t.Run("needs reconnect and subscriptions off", func(t *testing.T) {
		e := newVendorGroupEnv(t)
		g := e.create("p/", groupOwn2, grantOwnAccount)
		if err := e.routes.SetVendorAccountStatus(e.ctx, groupOwn2, routing.VendorAccountStatusNeedsReconnect); err != nil {
			t.Fatal(err)
		}
		g, _ = e.svc.GetVendorAccountGroup(e.ctx, e.owner, g.ID)
		requireMember(t, g, 1, groupOwn2, "not_usable", "needs_reconnect")
	})
	t.Run("cooldown", func(t *testing.T) {
		e := newVendorGroupEnv(t)
		until := e.now.Add(30 * time.Second)
		e.cooldowns.Set(grantOwnAccount, "gpt-x", until)
		g := e.create("p/", grantOwnAccount, groupOwn2)
		m := requireMember(t, g, 1, grantOwnAccount, "cooldown", "cooldown")
		if m.FreeAt == nil || !m.FreeAt.Equal(until) || len(m.Cooldowns) != 1 || m.Cooldowns[0].Model != "gpt-x" || !m.Cooldowns[0].Until.Equal(until) {
			t.Errorf("cooldown member = %+v", m)
		}
		requireMember(t, g, 2, groupOwn2, "active", "")
	})
	t.Run("every member blocked falls back to the soonest", func(t *testing.T) {
		e := newVendorGroupEnv(t)
		r1, r2 := e.now.Add(2*time.Hour), e.now.Add(time.Hour)
		e.usage(grantOwnAccount, 100, &r1)
		e.usage(groupOwn2, 100, &r2)
		g := e.create("p/", grantOwnAccount, groupOwn2)
		if g.ActiveMemberID != groupOwn2 {
			t.Errorf("active = %q, want the one freeing first", g.ActiveMemberID)
		}
		requireMember(t, g, 1, grantOwnAccount, "over_threshold", "five_hour")
		requireMember(t, g, 2, groupOwn2, "active", "")
	})
}

func TestVendorAccountGroupPrefixUpdateConflict(t *testing.T) {
	e := newVendorGroupEnv(t)
	g := e.create("solo/", grantOwnAccount)
	empty := ""
	_, err := e.svc.UpdateVendorAccountGroup(e.ctx, e.owner, g.ID, UpdateVendorAccountGroupRequest{ModelPrefix: &empty})
	requireGroupErr(t, "prefix update onto an outside account's names", err, ErrVendorGroupPrefixConflict)
	if got, _ := e.svc.GetVendorAccountGroup(e.ctx, e.owner, g.ID); got.ModelPrefix != "solo/" {
		t.Errorf("a refused update changed the prefix to %q", got.ModelPrefix)
	}
}
