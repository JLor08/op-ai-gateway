// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"errors"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/storeerr"
	"slices"
	"testing"
	"time"
)

var vagNow = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

// seedVAGUsers creates the owner users the SQL legs need for the owner FK; the
// memory store has no user concept.
func seedVAGUsers(t *testing.T, s *SQLStore) {
	t.Helper()
	for _, id := range []string{"u_vag", "u_vag_other"} {
		if err := s.CreateUser(context.Background(), newTestUser(id, id+"@example.test", vagNow)); err != nil {
			t.Fatalf("seed user %s: %v", id, err)
		}
	}
}

func vagGroup(id string) routing.VendorAccountGroup {
	return routing.VendorAccountGroup{
		ID: id, OwnerUserID: "u_vag", Vendor: routing.VendorAnthropic, Name: "Team " + id, ModelPrefix: "grp/",
		Status:               routing.VendorAccountGroupStatusActive,
		ThresholdFiveHourPct: 90, ThresholdWeeklyPct: 80, ThresholdCreditsPct: 70,
		CreatedAt: vagNow, UpdatedAt: vagNow,
	}
}

func seedVAGAccounts(t *testing.T, s routing.Store, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if err := s.CreateVendorAccount(context.Background(), routing.VendorAccount{
			ID: id, OwnerUserID: "u_vag", Vendor: routing.VendorAnthropic, AuthType: routing.VendorAuthSubscription,
			Name: id, Status: routing.VendorAccountStatusActive, CreatedAt: vagNow, UpdatedAt: vagNow,
		}); err != nil {
			t.Fatalf("seed account %s: %v", id, err)
		}
	}
}

func vagMemberIDs(t *testing.T, s routing.Store, groupID string) map[string]int {
	t.Helper()
	members, err := s.VendorAccountGroupMembers(context.Background(), groupID)
	if err != nil {
		t.Fatalf("members: %v", err)
	}
	if members == nil {
		t.Fatal("members is nil, want non-nil")
	}
	out := map[string]int{}
	for _, m := range members {
		if m.GroupID != groupID {
			t.Errorf("member group = %q, want %q", m.GroupID, groupID)
		}
		out[m.AccountID] = m.Position
	}
	return out
}

func TestVendorAccountGroupCRUD(t *testing.T) {
	forEachRoutingStoreSeeded(t, seedVAGUsers, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		g := vagGroup("vag_b")
		if err := s.CreateVendorAccountGroup(ctx, g); err != nil {
			t.Fatalf("create: %v", err)
		}
		if err := s.CreateVendorAccountGroup(ctx, vagGroup("vag_a")); err != nil {
			t.Fatalf("create a: %v", err)
		}
		got, err := s.VendorAccountGroupByID(ctx, "vag_b")
		if err != nil {
			t.Fatalf("by id: %v", err)
		}
		if !vagEqual(got, g) {
			t.Fatalf("round-trip mismatch:\n got  %+v\n want %+v", got, g)
		}

		upd := g
		upd.Vendor, upd.OwnerUserID = routing.VendorOpenAI, "u_vag_other"
		upd.Name, upd.ModelPrefix, upd.Status = "Renamed", "new/", routing.VendorAccountGroupStatusDisabled
		upd.ThresholdFiveHourPct, upd.ThresholdWeeklyPct, upd.ThresholdCreditsPct = 11, 22, 33
		upd.UpdatedAt = vagNow.Add(time.Hour)
		if err := s.UpdateVendorAccountGroup(ctx, upd); err != nil {
			t.Fatalf("update: %v", err)
		}
		want := upd
		want.Vendor, want.OwnerUserID, want.CreatedAt = g.Vendor, g.OwnerUserID, g.CreatedAt
		got, _ = s.VendorAccountGroupByID(ctx, "vag_b")
		if !vagEqual(got, want) {
			t.Fatalf("after update:\n got  %+v\n want %+v", got, want)
		}
		if err := s.UpdateVendorAccountGroup(ctx, vagGroup("vag_nope")); !errors.Is(err, storeerr.ErrNotFound) {
			t.Fatalf("update unknown err = %v, want ErrNotFound", err)
		}

		list, err := s.VendorAccountGroupsByOwner(ctx, "u_vag")
		if err != nil || len(list) != 2 || list[0].ID != "vag_a" || list[1].ID != "vag_b" {
			t.Fatalf("by owner = %+v, %v; want vag_a, vag_b", list, err)
		}
		stranger, err := s.VendorAccountGroupsByOwner(ctx, "u_stranger")
		if err != nil || stranger == nil || len(stranger) != 0 {
			t.Fatalf("stranger = %#v, %v; want empty non-nil", stranger, err)
		}

		if err := s.DeleteVendorAccountGroup(ctx, "vag_b"); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, err := s.VendorAccountGroupByID(ctx, "vag_b"); !errors.Is(err, storeerr.ErrNotFound) {
			t.Fatalf("read after delete err = %v, want ErrNotFound", err)
		}
		if err := s.DeleteVendorAccountGroup(ctx, "vag_b"); !errors.Is(err, storeerr.ErrNotFound) {
			t.Fatalf("second delete err = %v, want ErrNotFound", err)
		}
	})
}

func vagEqual(a, b routing.VendorAccountGroup) bool {
	a.CreatedAt, a.UpdatedAt = a.CreatedAt.UTC(), a.UpdatedAt.UTC()
	b.CreatedAt, b.UpdatedAt = b.CreatedAt.UTC(), b.UpdatedAt.UTC()
	return a == b
}

func TestVendorAccountGroupMembersReplaceAndOrder(t *testing.T) {
	forEachRoutingStoreSeeded(t, seedVAGUsers, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		seedVAGAccounts(t, s, "va_a", "va_b")
		if err := s.CreateVendorAccountGroup(ctx, vagGroup("vag_m")); err != nil {
			t.Fatalf("create: %v", err)
		}

		if err := s.SetVendorAccountGroupMembers(ctx, "vag_m", []string{"va_b", "va_a"}); err != nil {
			t.Fatalf("set [b,a]: %v", err)
		}
		members, _ := s.VendorAccountGroupMembers(ctx, "vag_m")
		if len(members) != 2 || members[0].AccountID != "va_b" || members[0].Position != 1 ||
			members[1].AccountID != "va_a" || members[1].Position != 2 {
			t.Fatalf("members = %+v, want b=1 then a=2", members)
		}

		if err := s.SetVendorAccountGroupMembers(ctx, "vag_m", []string{"va_a"}); err != nil {
			t.Fatalf("set [a]: %v", err)
		}
		if got := vagMemberIDs(t, s, "vag_m"); len(got) != 1 || got["va_a"] != 1 {
			t.Fatalf("members after replace = %v, want only a=1", got)
		}

		if err := s.SetVendorAccountGroupMembers(ctx, "vag_m", []string{"va_a", "va_a"}); !errors.Is(err, storeerr.ErrConflict) {
			t.Fatalf("duplicate err = %v, want ErrConflict", err)
		}
		if got := vagMemberIDs(t, s, "vag_m"); len(got) != 1 || got["va_a"] != 1 {
			t.Fatalf("members after rejected set = %v, want old set intact", got)
		}
		if err := s.SetVendorAccountGroupMembers(ctx, "vag_m", []string{"va_a", "va_ghost"}); !errors.Is(err, storeerr.ErrNotFound) {
			t.Fatalf("unknown account err = %v, want ErrNotFound", err)
		}
		if got := vagMemberIDs(t, s, "vag_m"); len(got) != 1 || got["va_a"] != 1 {
			t.Fatalf("members after unknown account = %v, want old set intact", got)
		}
		if err := s.SetVendorAccountGroupMembers(ctx, "vag_ghost", []string{"va_a"}); !errors.Is(err, storeerr.ErrNotFound) {
			t.Fatalf("unknown group err = %v, want ErrNotFound", err)
		}
		if err := s.SetVendorAccountGroupMembers(ctx, "vag_m", nil); err != nil {
			t.Fatalf("clear: %v", err)
		}
		if got := vagMemberIDs(t, s, "vag_m"); len(got) != 0 {
			t.Fatalf("members after clear = %v, want none", got)
		}
	})
}

func TestVendorAccountGroupCascades(t *testing.T) {
	forEachRoutingStoreSeeded(t, seedVAGUsers, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		seedVAGAccounts(t, s, "va_a", "va_b")
		for _, id := range []string{"vag_1", "vag_2"} {
			if err := s.CreateVendorAccountGroup(ctx, vagGroup(id)); err != nil {
				t.Fatalf("create %s: %v", id, err)
			}
			if err := s.SetVendorAccountGroupMembers(ctx, id, []string{"va_a", "va_b"}); err != nil {
				t.Fatalf("members %s: %v", id, err)
			}
		}

		// Deleting a member account removes its member rows, renumbering nothing.
		if err := s.DeleteVendorAccount(ctx, "va_a"); err != nil {
			t.Fatalf("delete account: %v", err)
		}
		for _, id := range []string{"vag_1", "vag_2"} {
			ids := vagMemberIDs(t, s, id)
			if _, ok := ids["va_a"]; ok || len(ids) != 1 {
				t.Fatalf("group %s members after account delete = %v, want only va_b", id, ids)
			}
		}

		// Deleting the group removes all of its member rows (and no other group's).
		if err := s.DeleteVendorAccountGroup(ctx, "vag_1"); err != nil {
			t.Fatalf("delete group: %v", err)
		}
		if got := vagMemberIDs(t, s, "vag_1"); len(got) != 0 {
			t.Fatalf("members of a deleted group = %v, want none", got)
		}
		if got := vagMemberIDs(t, s, "vag_2"); !slices.Equal(mapKeys(got), []string{"va_b"}) {
			t.Fatalf("other group's members = %v, want va_b", got)
		}

		// Deleting the owner user removes the group (SQL only; the memory store has no users).
		sqlStore, ok := s.(*SQLStore)
		if !ok {
			t.Log("memory store has no user concept; owner cascade is SQL-only")
			return
		}
		if err := deleteUserForTest(ctx, sqlStore, "u_vag"); err != nil {
			t.Fatalf("delete user: %v", err)
		}
		if _, err := s.VendorAccountGroupByID(ctx, "vag_2"); !errors.Is(err, storeerr.ErrNotFound) {
			t.Fatalf("group after owner delete err = %v, want ErrNotFound", err)
		}
	})
}

func mapKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
