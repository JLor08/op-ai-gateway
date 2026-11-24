// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"sync"
	"testing"
	"time"
)

const groupTestOwner = vendorOwner

type stubUsagePuller struct {
	mu  sync.Mutex
	ids []string
}

func (s *stubUsagePuller) RequestVendorUsagePull(accountID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ids = append(s.ids, accountID)
}

type groupFixture struct {
	store *MemoryStore
	r     *Resolver
	now   time.Time
}

func newGroupFixture(t *testing.T) *groupFixture {
	t.Helper()
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	return &groupFixture{store: store, r: vendorResolver(store, now, true, vendorRoutingModeVendorFirst), now: now}
}

// account seeds an anthropic account owned by owner serving the given slugs.
func (f *groupFixture) account(t *testing.T, id, owner, prefix string, slugs ...string) VendorAccount {
	t.Helper()
	ctx := context.Background()
	must(t, f.store.CreateVendorAccount(ctx, VendorAccount{
		ID: id, OwnerUserID: owner, Vendor: VendorAnthropic, AuthType: VendorAuthAPIKey,
		Name: id, Status: VendorAccountStatusActive, APIKey: "enc:" + id, ModelPrefix: prefix,
		CreatedAt: f.now, UpdatedAt: f.now,
	}))
	var rows []VendorAccountModel
	for _, s := range slugs {
		rows = append(rows, VendorAccountModel{AccountID: id, GatewayModel: prefix + s, UpstreamModel: s, APIFlavor: APIFlavorAnthropic})
	}
	must(t, f.store.SetVendorAccountModels(ctx, id, rows))
	acc, err := f.store.VendorAccountByID(ctx, id)
	must(t, err)
	return acc
}

func (f *groupFixture) group(t *testing.T, id, prefix string, members ...string) {
	t.Helper()
	ctx := context.Background()
	must(t, f.store.CreateVendorAccountGroup(ctx, VendorAccountGroup{
		ID: id, OwnerUserID: groupTestOwner, Vendor: VendorAnthropic, Name: id, ModelPrefix: prefix,
		Status:               VendorAccountGroupStatusActive,
		ThresholdFiveHourPct: 99, ThresholdWeeklyPct: 99, ThresholdCreditsPct: 99,
		CreatedAt: f.now, UpdatedAt: f.now,
	}))
	must(t, f.store.SetVendorAccountGroupMembers(ctx, id, members))
}

func (f *groupFixture) usage(t *testing.T, id string, fiveHour float64, fiveHourReset *time.Time, weekly float64, weeklyReset *time.Time) {
	t.Helper()
	must(t, f.store.UpsertVendorAccountUsage(context.Background(), VendorAccountUsage{
		AccountID: id, FiveHourPct: fiveHour, FiveHourResetAt: fiveHourReset,
		WeeklyPct: weekly, WeeklyResetAt: weeklyReset, SpendUsedPct: -1, UpdatedAt: f.now,
	}))
}

func groupToken() auth.Token {
	return auth.Token{
		ID: vendorTokenID, UserID: groupTestOwner, Active: true,
		VendorAccess: auth.VendorAccess{All: true, IncludeGroups: true},
	}
}

func groupRequest(model string) inference.Request {
	return inference.Request{Model: model, APIFlavor: "anthropic_messages"}
}

func at(t time.Time) *time.Time { return &t }

func TestResolveVendorGroupPicksFirstBelowThreshold(t *testing.T) {
	f := newGroupFixture(t)
	f.account(t, "subA", groupTestOwner, "sub/", "claude-opus-4-5")
	f.account(t, "keyB", groupTestOwner, "key/", "claude-opus-4-5")
	f.group(t, "vag_1", "claude/", "subA", "keyB")
	f.usage(t, "subA", 99, at(f.now.Add(time.Hour)), -1, nil)

	target, err := f.r.Resolve(context.Background(), groupToken(), groupRequest("claude/claude-opus-4-5"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.VendorAccountID != "keyB" || target.VendorGroupID != "vag_1" || target.ProviderModel != "claude-opus-4-5" {
		t.Errorf("target = account %q group %q model %q", target.VendorAccountID, target.VendorGroupID, target.ProviderModel)
	}
}

func TestResolveVendorGroupShadowsOwnMember(t *testing.T) {
	f := newGroupFixture(t)
	f.account(t, "subA", groupTestOwner, "", "claude-opus-4-5")
	f.account(t, "keyB", groupTestOwner, "key/", "claude-opus-4-5")
	f.group(t, "vag_1", "", "subA", "keyB")

	target, err := f.r.Resolve(context.Background(), groupToken(), groupRequest("claude-opus-4-5"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.VendorGroupID != "vag_1" || target.VendorAccountID != "subA" {
		t.Errorf("target = account %q group %q, want subA via vag_1", target.VendorAccountID, target.VendorGroupID)
	}
}

func TestResolveVendorGroupFallsBackToSoonestFree(t *testing.T) {
	f := newGroupFixture(t)
	f.account(t, "subA", groupTestOwner, "sub/", "claude-opus-4-5")
	f.account(t, "keyB", groupTestOwner, "key/", "claude-opus-4-5")
	f.group(t, "vag_1", "claude/", "subA", "keyB")
	f.usage(t, "subA", 100, at(f.now.Add(3*time.Hour)), -1, nil)
	f.usage(t, "keyB", 100, at(f.now.Add(time.Hour)), -1, nil)

	target, err := f.r.Resolve(context.Background(), groupToken(), groupRequest("claude/claude-opus-4-5"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.VendorAccountID != "keyB" || target.VendorGroupID != "vag_1" {
		t.Errorf("target = account %q group %q, want keyB via vag_1", target.VendorAccountID, target.VendorGroupID)
	}
}

func TestResolveVendorGroupExclusionDisablesFallback(t *testing.T) {
	f := newGroupFixture(t)
	f.account(t, "subA", groupTestOwner, "sub/", "claude-opus-4-5")
	f.account(t, "keyB", groupTestOwner, "key/", "claude-opus-4-5")
	f.group(t, "vag_1", "claude/", "subA", "keyB")
	f.usage(t, "keyB", 100, at(f.now.Add(time.Hour)), -1, nil)

	req := groupRequest("claude/claude-opus-4-5")
	req.ExcludeVendorAccountIDs = []string{"subA"}
	_, err := f.r.Resolve(context.Background(), groupToken(), req)
	if !errors.Is(err, ErrNoModelRoute) {
		t.Fatalf("Resolve err = %v, want ErrNoModelRoute", err)
	}
}

func TestResolveVendorGroupAllMembersUnreachable(t *testing.T) {
	f := newGroupFixture(t)
	f.account(t, "own", groupTestOwner, "own/", "claude-opus-4-5")
	shared := f.account(t, "shared", "usr_other", "sh/", "claude-opus-4-5")
	f.group(t, "vag_1", "claude/", "own", "shared")
	must(t, f.store.SetVendorAccountStatus(context.Background(), "own", VendorAccountStatusDisabled))
	// the enumerator no longer lists the shared member
	f.r.SetVendorAccountEnumerator(&fakeVendorEnumerator{accounts: nil})
	_ = shared

	_, err := f.r.Resolve(context.Background(), groupToken(), groupRequest("claude/claude-opus-4-5"))
	if !errors.Is(err, ErrNoModelRoute) {
		t.Fatalf("Resolve err = %v, want ErrNoModelRoute (no error, no group match)", err)
	}
}

func TestResolveVendorGroupSkipsCooldownForThatModelOnly(t *testing.T) {
	f := newGroupFixture(t)
	f.account(t, "subA", groupTestOwner, "sub/", "claude-opus-4-5", "claude-sonnet-4-5")
	f.account(t, "keyB", groupTestOwner, "key/", "claude-opus-4-5", "claude-sonnet-4-5")
	f.group(t, "vag_1", "claude/", "subA", "keyB")
	cd := NewVendorLimitCooldowns()
	cd.Set("subA", "claude-opus-4-5", f.now.Add(time.Minute), f.now.Add(time.Minute))
	f.r.SetVendorLimitCooldowns(cd)

	opus, err := f.r.Resolve(context.Background(), groupToken(), groupRequest("claude/claude-opus-4-5"))
	must(t, err)
	sonnet, err := f.r.Resolve(context.Background(), groupToken(), groupRequest("claude/claude-sonnet-4-5"))
	must(t, err)
	if opus.VendorAccountID != "keyB" || sonnet.VendorAccountID != "subA" {
		t.Errorf("opus -> %q (want keyB), sonnet -> %q (want subA)", opus.VendorAccountID, sonnet.VendorAccountID)
	}
}

func TestResolveVendorGroupSharedMemberWithoutUsageOptIn(t *testing.T) {
	f := newGroupFixture(t)
	shared := f.account(t, "shared", "usr_other", "sh/", "claude-opus-4-5")
	f.account(t, "keyB", groupTestOwner, "key/", "claude-opus-4-5")
	f.group(t, "vag_1", "claude/", "shared", "keyB")
	f.usage(t, "shared", 100, at(f.now.Add(time.Hour)), -1, nil)
	f.r.SetVendorAccountEnumerator(&fakeVendorEnumerator{accounts: []VendorAccount{shared, mustAccount(t, f.store, "keyB")}})

	target, err := f.r.Resolve(context.Background(), groupToken(), groupRequest("claude/claude-opus-4-5"))
	must(t, err)
	if target.VendorAccountID != "shared" {
		t.Errorf("account = %q, want shared (usage not shared, thresholds do not apply)", target.VendorAccountID)
	}
}

func mustAccount(t *testing.T, s *MemoryStore, id string) VendorAccount {
	t.Helper()
	acc, err := s.VendorAccountByID(context.Background(), id)
	must(t, err)
	return acc
}

func TestResolveVendorGroupIgnoresForeignAndStaleIDs(t *testing.T) {
	f := newGroupFixture(t)
	f.account(t, "subA", "usr_other", "sub/", "claude-opus-4-5")
	must(t, f.store.CreateVendorAccountGroup(context.Background(), VendorAccountGroup{
		ID: "vag_foreign", OwnerUserID: "usr_other", Vendor: VendorAnthropic, Name: "f", ModelPrefix: "claude/",
		Status: VendorAccountGroupStatusActive, ThresholdFiveHourPct: 99, ThresholdWeeklyPct: 99, ThresholdCreditsPct: 99,
	}))
	must(t, f.store.SetVendorAccountGroupMembers(context.Background(), "vag_foreign", []string{"subA"}))
	tok := auth.Token{ID: "tok", UserID: groupTestOwner, Active: true, VendorAccess: auth.VendorAccess{
		Groups: []auth.VendorAccessGroupEntry{{GroupID: "vag_deleted"}, {GroupID: "vag_foreign"}},
	}}

	_, err := f.r.Resolve(context.Background(), tok, groupRequest("claude/claude-opus-4-5"))
	if !errors.Is(err, ErrNoModelRoute) {
		t.Fatalf("Resolve err = %v, want ErrNoModelRoute", err)
	}
}

func TestResolveVendorGroupRequestsPullForUnknownReset(t *testing.T) {
	f := newGroupFixture(t)
	f.account(t, "subA", groupTestOwner, "sub/", "claude-opus-4-5")
	f.account(t, "keyB", groupTestOwner, "key/", "claude-opus-4-5")
	f.group(t, "vag_1", "claude/", "subA", "keyB")
	f.usage(t, "subA", -1, nil, 100, nil)
	puller := &stubUsagePuller{}
	f.r.SetVendorUsagePuller(puller)

	target, err := f.r.Resolve(context.Background(), groupToken(), groupRequest("claude/claude-opus-4-5"))
	must(t, err)
	if target.VendorAccountID != "keyB" {
		t.Errorf("account = %q, want keyB", target.VendorAccountID)
	}
	if len(puller.ids) != 1 || puller.ids[0] != "subA" {
		t.Errorf("pulls = %v, want [subA]", puller.ids)
	}
}

// failingGroupStore answers every group read with an error and counts the reads, so
// a test can tell a resolve that never needs groups from one that reads them.
type failingGroupStore struct {
	*MemoryStore
	groupReads int
}

func (s *failingGroupStore) VendorAccountGroupsByOwner(context.Context, string) ([]VendorAccountGroup, error) {
	s.groupReads++
	return nil, errors.New("groups table unavailable")
}

// A token that can use no group (neither all + include_groups nor a listed group)
// never reads the groups, so a failing group read cannot fail its resolve; a token
// that can still reads them.
func TestResolveVendorGroupSkippedForTokensThatUseNoGroup(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	newFixture := func(t *testing.T) (*groupFixture, *failingGroupStore) {
		t.Helper()
		store := &failingGroupStore{MemoryStore: NewMemoryStore()}
		f := &groupFixture{store: store.MemoryStore, r: vendorResolver(store, now, true, vendorRoutingModeVendorFirst), now: now}
		f.account(t, "keyB", groupTestOwner, "key/", "claude-opus-4-5")
		return f, store
	}
	listed := []auth.VendorAccessEntry{{AccountID: "keyB"}}
	for name, access := range map[string]auth.VendorAccess{
		"all without include_groups": {All: true},
		"include_groups without all": {IncludeGroups: true, Accounts: listed},
		"accounts only":              {Accounts: listed},
	} {
		t.Run(name, func(t *testing.T) {
			f, store := newFixture(t)
			token := groupToken()
			token.VendorAccess = access
			target, err := f.r.Resolve(ctx, token, groupRequest("key/claude-opus-4-5"))
			if err != nil || target.VendorAccountID != "keyB" || target.VendorGroupID != "" {
				t.Fatalf("Resolve = account %q group %q, err %v; want keyB outside any group", target.VendorAccountID, target.VendorGroupID, err)
			}
			if store.groupReads != 0 {
				t.Fatalf("group reads = %d, want none for a token that can use no group", store.groupReads)
			}
		})
	}
	for name, access := range map[string]auth.VendorAccess{
		"all + include_groups": {All: true, IncludeGroups: true},
		"a listed group":       {Accounts: listed, Groups: []auth.VendorAccessGroupEntry{{GroupID: "vag_1"}}},
	} {
		t.Run(name+" still reads the groups", func(t *testing.T) {
			f, store := newFixture(t)
			token := groupToken()
			token.VendorAccess = access
			if _, err := f.r.Resolve(ctx, token, groupRequest("key/claude-opus-4-5")); err == nil {
				t.Fatal("Resolve succeeded, want the group read's failure")
			}
			if store.groupReads != 1 {
				t.Fatalf("group reads = %d, want 1", store.groupReads)
			}
		})
	}
}
