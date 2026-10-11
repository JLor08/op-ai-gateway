// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"net/http"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/vendorauth"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- RefreshVendorAccountUsage: the dedicated, model-free usage refresh -----------------
//
// The "Usage & Limits" panel pulls an OpenAI subscription's usage snapshot on its
// own (the manual button and the lazy on-view pull), without the model discovery the
// models refresh runs first. The lazy pull is rate-limited by an in-memory
// last-active-pull time per account (VendorUsageLazyTTL); a manual refresh passes a
// zero maxAge and always asks the vendor.

// setUsageClock moves the service clock to now (the TTL and the stamped snapshot
// both read it).
func setUsageClock(svc *Service, now time.Time) {
	svc.clock = func() time.Time { return now }
}

// usagePullRecorded reports whether the tracker holds a pull for id that is fresh
// at the service clock's now (the pull time is the clock's reading when it ran).
func usagePullRecorded(svc *Service, id string) bool {
	return svc.vendorUsagePulls.within(id, svc.clock(), VendorUsageLazyTTL)
}

// usagePulled is the snapshot the usage fake answers in these tests.
func usagePulled() vendorauth.OpenAISubscriptionUsage {
	return vendorauth.OpenAISubscriptionUsage{FiveHourPct: 23, FiveHourResetAt: usageAt(2), WeeklyPct: 61, WeeklyResetAt: usageAt(100), CreditBalance: "12.34", SpendUsedPct: -1}
}

// A usage-only refresh pulls the usage, stores it and answers it, with the opened
// access token, the ChatGPT account id and the bounded client -- and NEVER runs the
// model discovery (no model fetcher call, the model rows untouched).
func TestRefreshVendorAccountUsagePullsUsageAndNeverCallsTheModelFetcher(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	fake.okSlugs(kindOpenAISubscription, "gpt-6-luna") // would replace the seed if the models refresh ran
	fake.okUsage(usagePulled())
	acc := connectedSubscription(t, svc, routeStore, routing.VendorOpenAI, "chatgpt/")
	seed := storedModels(t, routeStore, acc.ID)

	usage, res, err := svc.RefreshVendorAccountUsage(context.Background(), ownerToken(), acc.ID, 0)
	if err != nil {
		t.Fatalf("RefreshVendorAccountUsage: %v", err)
	}
	if res.Status != VendorUsageRefreshOK || res.Detail == "" {
		t.Fatalf("result = %+v, want ok with a detail", res)
	}
	fake.requireNoCalls(t, "for a usage-only refresh (the model discovery must not run)")
	call := fake.onlyUsageCall(t)
	if call.accessToken != discoveryTestAccess || call.accountID != discoveryTestAccount || !call.hasClient || call.timeout != 10*time.Second {
		t.Fatalf("usage call = %+v, want the opened access token, the ChatGPT account id and the bounded 10s client", call)
	}
	if got := storedModels(t, routeStore, acc.ID); !reflect.DeepEqual(got, seed) {
		t.Fatalf("model rows = %+v, want the seed untouched %+v", got, seed)
	}
	want := routing.VendorAccountUsage{
		AccountID: acc.ID, FiveHourPct: 23, FiveHourResetAt: usageAt(2),
		WeeklyPct: 61, WeeklyResetAt: usageAt(100), CreditBalance: "12.34", SpendUsedPct: -1, UpdatedAt: discoveryTestNow,
	}
	if got, found := storedUsage(t, routeStore, acc.ID); !found || !reflect.DeepEqual(got, want) {
		t.Fatalf("stored usage = %+v (found %v), want %+v", got, found, want)
	}
	if usage == nil || usage.FiveHourPct != 23 || usage.WeeklyPct != 61 || usage.CreditBalance != "12.34" || !usage.UpdatedAt.Equal(discoveryTestNow) {
		t.Fatalf("answered usage = %+v, want the stored snapshot", usage)
	}
	requireNoToken(t, "usage", usage)
	requireNoToken(t, "result", res)
}

// The answered usage is the stored snapshot AFTER the merge: a field the pull does
// not know keeps the passive scrape's value (the same rule as the models refresh).
func TestRefreshVendorAccountUsageAnswersTheMergedSnapshot(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	fake.okUsage(vendorauth.OpenAISubscriptionUsage{FiveHourPct: 23, WeeklyPct: -1, CreditBalance: "12.34", SpendUsedPct: -1})
	acc := connectedSubscription(t, svc, routeStore, routing.VendorOpenAI, "")
	seedUsage(t, routeStore, passiveUsage(acc.ID))

	usage, res, err := svc.RefreshVendorAccountUsage(context.Background(), ownerToken(), acc.ID, 0)
	if err != nil || res.Status != VendorUsageRefreshOK {
		t.Fatalf("result = %+v, err = %v, want ok", res, err)
	}
	if usage == nil || usage.FiveHourPct != 23 || usage.WeeklyPct != 70 || usage.CreditBalance != "12.34" {
		t.Fatalf("usage = %+v, want the pull's five-hour 23 / credit 12.34 merged over the scrape's weekly 70", usage)
	}
}

// A usage refresh sends the owner's sealed credential to the vendor, so it is
// owner-only like the models refresh: a stranger, a system-scope non-owner, a
// principal without identity and an unknown id are all 404 and never fetch.
func TestRefreshVendorAccountUsageIsOwnerOnly(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	fake.okUsage(usagePulled())
	refresher := installTokenRefresher(t, svc, routeStore)
	acc := connectedSubscription(t, svc, routeStore, routing.VendorOpenAI, "")

	for label, principal := range map[string]auth.Token{
		"another user":           otherToken(),
		"no user identity":       {},
		"another user (admin)":   {UserID: "usr_admin", Scopes: []string{"gateway:use", "admin"}},
		"system scope non-owner": systemToken(),
	} {
		usage, _, err := svc.RefreshVendorAccountUsage(context.Background(), principal, acc.ID, 0)
		if !errors.Is(err, ErrVendorAccountNotFound) {
			t.Fatalf("%s: err = %v, want ErrVendorAccountNotFound", label, err)
		}
		if usage != nil {
			t.Fatalf("%s: usage = %+v, want none for a refused principal", label, usage)
		}
	}
	if _, _, err := svc.RefreshVendorAccountUsage(context.Background(), ownerToken(), "va_missing", 0); !errors.Is(err, ErrVendorAccountNotFound) {
		t.Fatalf("unknown id: err = %v, want ErrVendorAccountNotFound", err)
	}
	fake.requireNoUsageCalls(t, "for a refused principal")
	fake.requireNoCalls(t, "for a refused principal")
	if got := refresher.recorded(); len(got) != 0 {
		t.Fatalf("refresher calls = %v, want none for a refused principal", got)
	}
	if _, found := storedUsage(t, routeStore, acc.ID); found {
		t.Fatal("a refused refresh stored a usage snapshot")
	}
}

func TestRefreshVendorAccountUsageRefusesWhileTheMasterFlagIsOff(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	fake.okUsage(usagePulled())
	acc := connectedSubscription(t, svc, routeStore, routing.VendorOpenAI, "")
	setVendorAccountsEnabled(t, svc, false)

	if _, _, err := svc.RefreshVendorAccountUsage(context.Background(), ownerToken(), acc.ID, 0); !errors.Is(err, ErrVendorAccountsDisabled) {
		t.Fatalf("err = %v, want ErrVendorAccountsDisabled", err)
	}
	if _, _, err := svc.RefreshVendorAccountUsage(context.Background(), ownerToken(), "va_missing", 0); !errors.Is(err, ErrVendorAccountsDisabled) {
		t.Fatalf("unknown id: err = %v, want ErrVendorAccountsDisabled (a disabled area confirms nothing)", err)
	}
	fake.requireNoUsageCalls(t, "while the module is off")
}

// Only an OpenAI SUBSCRIPTION (and an OpenRouter/Kilo api key, below) has an active
// usage pull. The native api keys of either vendor and an Anthropic subscription
// answer "unsupported": no token is opened or renewed (a sealed credential that
// cannot be read is no error here), no fetcher runs, and the stored snapshot, if any,
// is still answered.
func TestRefreshVendorAccountUsageIsUnsupportedForEveryOtherAccountKind(t *testing.T) {
	for _, tc := range []struct {
		name string
		make func(t *testing.T, svc *Service, routeStore *routing.MemoryStore) VendorAccountDTO
	}{
		{"openai api key", func(t *testing.T, svc *Service, _ *routing.MemoryStore) VendorAccountDTO {
			return apiKeyAccount(t, svc, routing.VendorOpenAI, "")
		}},
		{"anthropic api key", func(t *testing.T, svc *Service, _ *routing.MemoryStore) VendorAccountDTO {
			return apiKeyAccount(t, svc, routing.VendorAnthropic, "")
		}},
		{"anthropic subscription, token expired", func(t *testing.T, svc *Service, routeStore *routing.MemoryStore) VendorAccountDTO {
			return expiredSubscription(t, svc, routeStore, routing.VendorAnthropic, "")
		}},
		{"anthropic subscription, credential unreadable", func(t *testing.T, svc *Service, routeStore *routing.MemoryStore) VendorAccountDTO {
			acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorAnthropic, "Broken")
			row, err := routeStore.VendorAccountByID(context.Background(), acc.ID)
			if err != nil {
				t.Fatalf("VendorAccountByID: %v", err)
			}
			row.OAuthTokens = "garbage-without-a-prefix"
			if err := routeStore.UpdateVendorAccount(context.Background(), row); err != nil {
				t.Fatalf("UpdateVendorAccount: %v", err)
			}
			return acc
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, routeStore, fake := newDiscoveryTestService(t)
			fake.okUsage(usagePulled()) // an answer that WOULD be stored if the fetcher were consulted
			refresher := installTokenRefresher(t, svc, routeStore)
			acc := tc.make(t, svc, routeStore)

			usage, res, err := svc.RefreshVendorAccountUsage(context.Background(), ownerToken(), acc.ID, 0)
			if err != nil {
				t.Fatalf("RefreshVendorAccountUsage: %v (an account with no active pull is no error)", err)
			}
			if res.Status != VendorUsageRefreshUnsupported || res.Detail == "" {
				t.Fatalf("result = %+v, want unsupported with a detail", res)
			}
			if usage != nil {
				t.Fatalf("usage = %+v, want none (nothing stored)", usage)
			}
			fake.requireNoUsageCalls(t, "for an account that is not an OpenAI subscription")
			fake.requireNoCompatUsageCalls(t, "for an account whose preset names no usage strategy")
			fake.requireNoCalls(t, "for a usage refresh")
			if got := refresher.recorded(); len(got) != 0 {
				t.Fatalf("refresher calls = %v, want no token opened or renewed", got)
			}
			if _, found := storedUsage(t, routeStore, acc.ID); found {
				t.Fatal("a usage snapshot was stored for an unsupported account")
			}
		})
	}

	t.Run("a snapshot the passive scrape stored is still answered", func(t *testing.T) {
		svc, routeStore, fake := newDiscoveryTestService(t)
		acc := connectedSubscription(t, svc, routeStore, routing.VendorAnthropic, "")
		before := passiveUsage(acc.ID)
		seedUsage(t, routeStore, before)

		usage, res, err := svc.RefreshVendorAccountUsage(context.Background(), ownerToken(), acc.ID, 0)
		if err != nil || res.Status != VendorUsageRefreshUnsupported {
			t.Fatalf("result = %+v, err = %v, want unsupported / nil", res, err)
		}
		if usage == nil || usage.FiveHourPct != before.FiveHourPct || usage.WeeklyPct != before.WeeklyPct {
			t.Fatalf("usage = %+v, want the stored snapshot %+v", usage, before)
		}
		fake.requireNoUsageCalls(t, "for an Anthropic subscription")
	})
}

// The lazy TTL: a pull younger than maxAge is not repeated. The vendor is not asked,
// the status is "fresh" and the stored snapshot is answered; an older (or no) pull
// asks the vendor, and the age is strictly "younger than maxAge".
func TestRefreshVendorAccountUsageSkipsTheVendorWhileTheLastPullIsFresh(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	fake.okUsage(usagePulled())
	acc := connectedSubscription(t, svc, routeStore, routing.VendorOpenAI, "")
	ctx := context.Background()

	// No pull yet: the lazy call pulls.
	if usage, res, err := svc.RefreshVendorAccountUsage(ctx, ownerToken(), acc.ID, VendorUsageLazyTTL); err != nil || res.Status != VendorUsageRefreshOK || usage == nil {
		t.Fatalf("first lazy call = %+v / %+v, err = %v, want ok with usage", usage, res, err)
	}
	fake.onlyUsageCall(t)

	// One minute later: fresh, the stored snapshot, no vendor call.
	setUsageClock(svc, discoveryTestNow.Add(time.Minute))
	usage, res, err := svc.RefreshVendorAccountUsage(ctx, ownerToken(), acc.ID, VendorUsageLazyTTL)
	if err != nil || res.Status != VendorUsageRefreshFresh || !strings.Contains(res.Detail, "not re-fetched") {
		t.Fatalf("second lazy call = %+v, err = %v, want fresh whose detail says the snapshot was not re-fetched", res, err)
	}
	if strings.Contains(res.Detail, "current") {
		t.Fatalf("fresh detail = %q, must not claim the snapshot is current (the last attempt may have failed)", res.Detail)
	}
	if usage == nil || usage.FiveHourPct != 23 || !usage.UpdatedAt.Equal(discoveryTestNow) {
		t.Fatalf("fresh usage = %+v, want the stored snapshot", usage)
	}
	fake.onlyUsageCall(t) // still exactly the first call

	// Just under the TTL: still fresh.
	setUsageClock(svc, discoveryTestNow.Add(VendorUsageLazyTTL-time.Second))
	if _, res, err := svc.RefreshVendorAccountUsage(ctx, ownerToken(), acc.ID, VendorUsageLazyTTL); err != nil || res.Status != VendorUsageRefreshFresh {
		t.Fatalf("just under the TTL = %+v, err = %v, want fresh", res, err)
	}
	fake.onlyUsageCall(t)

	// Exactly the TTL: no longer younger than maxAge, so it pulls again.
	setUsageClock(svc, discoveryTestNow.Add(VendorUsageLazyTTL))
	if _, res, err := svc.RefreshVendorAccountUsage(ctx, ownerToken(), acc.ID, VendorUsageLazyTTL); err != nil || res.Status != VendorUsageRefreshOK {
		t.Fatalf("at the TTL = %+v, err = %v, want ok (a stale pull is repeated)", res, err)
	}
	if n := len(fake.usageRecorded()); n != 2 {
		t.Fatalf("usage fetcher calls = %d, want 2 after the stale pull", n)
	}

	// The pull restarted the clock on the TTL.
	if _, res, err := svc.RefreshVendorAccountUsage(ctx, ownerToken(), acc.ID, VendorUsageLazyTTL); err != nil || res.Status != VendorUsageRefreshFresh {
		t.Fatalf("right after the repeated pull = %+v, err = %v, want fresh", res, err)
	}
	if n := len(fake.usageRecorded()); n != 2 {
		t.Fatalf("usage fetcher calls = %d, want still 2", n)
	}
}

// A manual refresh (maxAge 0) always asks the vendor, even right after a pull.
func TestRefreshVendorAccountUsageForceBypassesTheTTL(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	fake.okUsage(usagePulled())
	acc := connectedSubscription(t, svc, routeStore, routing.VendorOpenAI, "")
	ctx := context.Background()

	for i := 1; i <= 3; i++ {
		_, res, err := svc.RefreshVendorAccountUsage(ctx, ownerToken(), acc.ID, 0)
		if err != nil || res.Status != VendorUsageRefreshOK {
			t.Fatalf("forced call %d = %+v, err = %v, want ok", i, res, err)
		}
		if n := len(fake.usageRecorded()); n != i {
			t.Fatalf("usage fetcher calls = %d after forced call %d, want %d", n, i, i)
		}
	}
	// The lazy path sees the forced pulls as fresh, so the button also quiets the view.
	if _, res, err := svc.RefreshVendorAccountUsage(ctx, ownerToken(), acc.ID, VendorUsageLazyTTL); err != nil || res.Status != VendorUsageRefreshFresh {
		t.Fatalf("lazy call after forced pulls = %+v, err = %v, want fresh", res, err)
	}
	if n := len(fake.usageRecorded()); n != 3 {
		t.Fatalf("usage fetcher calls = %d, want still 3", n)
	}
}

// The TTL is per account: one account's pull does not make another's fresh.
func TestRefreshVendorAccountUsageTTLIsPerAccount(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	fake.okUsage(usagePulled())
	first := connectedSubscription(t, svc, routeStore, routing.VendorOpenAI, "a/")
	second := connectedSubscription(t, svc, routeStore, routing.VendorOpenAI, "b/")
	ctx := context.Background()

	if _, res, err := svc.RefreshVendorAccountUsage(ctx, ownerToken(), first.ID, VendorUsageLazyTTL); err != nil || res.Status != VendorUsageRefreshOK {
		t.Fatalf("first account = %+v, err = %v, want ok", res, err)
	}
	if _, res, err := svc.RefreshVendorAccountUsage(ctx, ownerToken(), second.ID, VendorUsageLazyTTL); err != nil || res.Status != VendorUsageRefreshOK {
		t.Fatalf("second account = %+v, err = %v, want ok (its own pull, not the first's)", res, err)
	}
	if n := len(fake.usageRecorded()); n != 2 {
		t.Fatalf("usage fetcher calls = %d, want one per account", n)
	}
}

// The usage pull the models refresh runs on the way is an active pull too: a lazy
// refresh right after it is fresh and does not ask the vendor a second time.
func TestRefreshVendorAccountUsageCountsTheModelsRefreshPull(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	fake.okSlugs(kindOpenAISubscription, "gpt-6-luna")
	fake.okUsage(usagePulled())
	acc := connectedSubscription(t, svc, routeStore, routing.VendorOpenAI, "")
	ctx := context.Background()

	if _, res, err := svc.RefreshVendorAccountModels(ctx, ownerToken(), acc.ID); err != nil || res.Status != VendorRefreshOK {
		t.Fatalf("models refresh = %+v, err = %v, want ok", res, err)
	}
	fake.onlyUsageCall(t)

	_, res, err := svc.RefreshVendorAccountUsage(ctx, ownerToken(), acc.ID, VendorUsageLazyTTL)
	if err != nil || res.Status != VendorUsageRefreshFresh {
		t.Fatalf("lazy usage refresh = %+v, err = %v, want fresh", res, err)
	}
	fake.onlyUsageCall(t)
}

// An unverifiable fetch (a 401, a timeout, an unusable body) is no error: status
// unverifiable, the stored snapshot kept and answered. The vendor WAS asked, so the
// attempt counts toward the TTL (a failing vendor is not hammered on every view)
// while a manual refresh still asks again.
func TestRefreshVendorAccountUsageUnverifiableKeepsTheStoredUsage(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	fake.failUsage(vendorauth.OpenAISubscriptionUsage{FiveHourPct: 99, WeeklyPct: 99, CreditBalance: "junk", SpendUsedPct: -1})
	acc := connectedSubscription(t, svc, routeStore, routing.VendorOpenAI, "")
	before := passiveUsage(acc.ID)
	seedUsage(t, routeStore, before)
	ctx := context.Background()

	usage, res, err := svc.RefreshVendorAccountUsage(ctx, ownerToken(), acc.ID, VendorUsageLazyTTL)
	if err != nil || res.Status != VendorUsageRefreshUnverifiable || res.Detail == "" {
		t.Fatalf("result = %+v, err = %v, want unverifiable with a detail", res, err)
	}
	if usage == nil || usage.FiveHourPct != before.FiveHourPct || usage.CreditBalance != before.CreditBalance {
		t.Fatalf("usage = %+v, want the stored snapshot %+v", usage, before)
	}
	if got, _ := storedUsage(t, routeStore, acc.ID); !reflect.DeepEqual(got, before) {
		t.Fatalf("stored usage = %+v, want it untouched %+v", got, before)
	}
	fake.onlyUsageCall(t)

	if _, res, _ := svc.RefreshVendorAccountUsage(ctx, ownerToken(), acc.ID, VendorUsageLazyTTL); res.Status != VendorUsageRefreshFresh {
		t.Fatalf("lazy call after a failed attempt = %+v, want fresh (the vendor is not hammered)", res)
	}
	fake.onlyUsageCall(t)
	if _, res, _ := svc.RefreshVendorAccountUsage(ctx, ownerToken(), acc.ID, 0); res.Status != VendorUsageRefreshUnverifiable {
		t.Fatalf("forced call = %+v, want unverifiable (the button asks again)", res)
	}
	if n := len(fake.usageRecorded()); n != 2 {
		t.Fatalf("usage fetcher calls = %d, want 2", n)
	}
	requireNoToken(t, "usage", usage)
}

// A fetch that cannot be stored is unverifiable too, the snapshot as it was. A store
// that cannot even answer the snapshot afterwards is the one real error.
func TestRefreshVendorAccountUsageStoreFailures(t *testing.T) {
	ctx := context.Background()

	t.Run("snapshot write fails", func(t *testing.T) {
		svc, routeStore, fake := newDiscoveryTestService(t)
		fake.okUsage(usagePulled())
		acc := connectedSubscription(t, svc, routeStore, routing.VendorOpenAI, "")
		before := passiveUsage(acc.ID)
		seedUsage(t, routeStore, before)
		svc.routes = failUsageStore{Store: routeStore, writeErr: errors.New("usage table unavailable")}

		usage, res, err := svc.RefreshVendorAccountUsage(ctx, ownerToken(), acc.ID, 0)
		if err != nil || res.Status != VendorUsageRefreshUnverifiable {
			t.Fatalf("result = %+v, err = %v, want unverifiable / nil", res, err)
		}
		if usage == nil || usage.FiveHourPct != before.FiveHourPct {
			t.Fatalf("usage = %+v, want the stored snapshot %+v", usage, before)
		}
		if got, _ := storedUsage(t, routeStore, acc.ID); !reflect.DeepEqual(got, before) {
			t.Fatalf("stored usage = %+v, want it untouched %+v", got, before)
		}
	})
	t.Run("snapshot read fails", func(t *testing.T) {
		svc, routeStore, fake := newDiscoveryTestService(t)
		fake.okUsage(usagePulled())
		acc := connectedSubscription(t, svc, routeStore, routing.VendorOpenAI, "")
		seedUsage(t, routeStore, passiveUsage(acc.ID))
		svc.routes = failUsageStore{Store: routeStore, readErr: errors.New("usage table unavailable")}

		if _, _, err := svc.RefreshVendorAccountUsage(ctx, ownerToken(), acc.ID, 0); err == nil || !strings.Contains(err.Error(), "usage table unavailable") {
			t.Fatalf("err = %v, want the store failure surfaced", err)
		}
	})
}

// fetchVendorUsage is the extracted pull: its status is "ok" only when a snapshot
// was stored, "unverifiable" for a fetch that is not OK or a store read/write
// failure, "unsupported" for anything but an OpenAI subscription with an access token.
func TestFetchVendorUsageStatus(t *testing.T) {
	ctx := context.Background()
	tokens := vendorauth.TokenSet{AccessToken: discoveryTestAccess, AccountID: discoveryTestAccount}
	openAI := func(t *testing.T, svc *Service, routeStore *routing.MemoryStore) routing.VendorAccount {
		return accountRow(t, svc, connectedSubscription(t, svc, routeStore, routing.VendorOpenAI, ""))
	}
	openAIAPIKey := func(t *testing.T, svc *Service, _ *routing.MemoryStore) routing.VendorAccount {
		return accountRow(t, svc, apiKeyAccount(t, svc, routing.VendorOpenAI, ""))
	}
	anthropicSubscription := func(t *testing.T, svc *Service, routeStore *routing.MemoryStore) routing.VendorAccount {
		return accountRow(t, svc, connectedSubscription(t, svc, routeStore, routing.VendorAnthropic, ""))
	}

	for _, tc := range []struct {
		name string
		acc  func(t *testing.T, svc *Service, routeStore *routing.MemoryStore) routing.VendorAccount
		// usageOK is whether the fake usage fetch answers a verified read.
		usageOK bool
		// store, when set, wraps the route store with a failing usage read/write.
		store func(routing.Store) routing.Store
		ts    vendorauth.TokenSet
		want  string
	}{
		{name: "ok", acc: openAI, usageOK: true, ts: tokens, want: VendorUsageRefreshOK},
		{name: "fetch unverifiable", acc: openAI, ts: tokens, want: VendorUsageRefreshUnverifiable},
		{
			name: "snapshot read fails", acc: openAI, usageOK: true, ts: tokens, want: VendorUsageRefreshUnverifiable,
			store: func(s routing.Store) routing.Store { return failUsageStore{Store: s, readErr: errors.New("down")} },
		},
		{
			name: "snapshot write fails", acc: openAI, usageOK: true, ts: tokens, want: VendorUsageRefreshUnverifiable,
			store: func(s routing.Store) routing.Store { return failUsageStore{Store: s, writeErr: errors.New("down")} },
		},
		{name: "openai without an access token", acc: openAI, usageOK: true, want: VendorUsageRefreshUnsupported},
		{name: "openai api key", acc: openAIAPIKey, usageOK: true, ts: tokens, want: VendorUsageRefreshUnsupported},
		{name: "anthropic subscription", acc: anthropicSubscription, usageOK: true, ts: tokens, want: VendorUsageRefreshUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, routeStore, fake := newDiscoveryTestService(t)
			acc := tc.acc(t, svc, routeStore)
			if tc.usageOK {
				fake.okUsage(usagePulled())
			}
			if tc.store != nil {
				svc.routes = tc.store(routeStore)
			}

			if got := svc.fetchVendorUsage(ctx, acc, tc.ts); got != tc.want {
				t.Fatalf("fetchVendorUsage = %q, want %q", got, tc.want)
			}
			if tc.want == VendorUsageRefreshUnsupported {
				fake.requireNoUsageCalls(t, "for an unsupported account")
			}
		})
	}
}

func accountRow(t *testing.T, svc *Service, dto VendorAccountDTO) routing.VendorAccount {
	t.Helper()
	row, err := svc.routes.VendorAccountByID(context.Background(), dto.ID)
	if err != nil {
		t.Fatalf("VendorAccountByID: %v", err)
	}
	return row
}

// A pull that never reached the vendor because the request's context was already
// dead (a connect's shared budget ran out during the model list, a user navigating
// away mid-pull) is NOT a recorded pull: it answers unverifiable, and the next lazy
// call asks the vendor instead of answering "fresh" with no snapshot for the TTL.
func TestRefreshVendorAccountUsageDeadContextIsNotARecordedPull(t *testing.T) {
	// cancelDuring: the context ends while the fetch is in flight (the real fetcher
	// answers Unverifiable then); cancelBefore: it was dead before the call.
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, svc *Service, fake *fakeVendorDiscoverers, id string) (VendorUsageRefreshResult, error)
	}{
		{"cancelled during the fetch", func(t *testing.T, svc *Service, _ *fakeVendorDiscoverers, id string) (VendorUsageRefreshResult, error) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			inner := svc.vendorDiscovery.discoverers.OpenAIUsage
			defer func() { svc.vendorDiscovery.discoverers.OpenAIUsage = inner }()
			svc.vendorDiscovery.discoverers.OpenAIUsage = func(c context.Context, client *http.Client, accessToken, accountID string) (vendorauth.OpenAISubscriptionUsage, vendorauth.DiscoveryStatus) {
				cancel() // the budget runs out while the vendor is being asked
				usage, _ := inner(c, client, accessToken, accountID)
				return usage, vendorauth.DiscoveryUnverifiable
			}
			_, res, err := svc.RefreshVendorAccountUsage(ctx, ownerToken(), id, VendorUsageLazyTTL)
			return res, err
		}},
		{"cancelled before the call", func(t *testing.T, svc *Service, fake *fakeVendorDiscoverers, id string) (VendorUsageRefreshResult, error) {
			fake.failUsage(unknownUsage()) // a dead context makes the real fetcher answer Unverifiable
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, res, err := svc.RefreshVendorAccountUsage(ctx, ownerToken(), id, VendorUsageLazyTTL)
			return res, err
		}},
		{"deadline exceeded", func(t *testing.T, svc *Service, fake *fakeVendorDiscoverers, id string) (VendorUsageRefreshResult, error) {
			fake.failUsage(unknownUsage())
			ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			defer cancel()
			_, res, err := svc.RefreshVendorAccountUsage(ctx, ownerToken(), id, VendorUsageLazyTTL)
			return res, err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, routeStore, fake := newDiscoveryTestService(t)
			fake.okUsage(usagePulled())
			acc := connectedSubscription(t, svc, routeStore, routing.VendorOpenAI, "")

			res, err := tc.run(t, svc, fake, acc.ID)
			if err != nil || res.Status != VendorUsageRefreshUnverifiable {
				t.Fatalf("result = %+v, err = %v, want unverifiable / nil", res, err)
			}
			if usagePullRecorded(svc, acc.ID) {
				t.Fatal("a pull that never reached the vendor was recorded")
			}
			if _, found := storedUsage(t, routeStore, acc.ID); found {
				t.Fatal("a usage snapshot was stored by a dead-context pull")
			}

			// The next lazy call (live context, vendor answering again) pulls: it is
			// neither "fresh" nor snapshot-less for the TTL.
			fake.okUsage(usagePulled())
			before := len(fake.usageRecorded())
			usage, res, err := svc.RefreshVendorAccountUsage(context.Background(), ownerToken(), acc.ID, VendorUsageLazyTTL)
			if err != nil || res.Status != VendorUsageRefreshOK || usage == nil {
				t.Fatalf("next lazy call = %+v / %+v, err = %v, want ok with usage (it must pull again)", usage, res, err)
			}
			if after := len(fake.usageRecorded()); after != before+1 {
				t.Fatalf("usage fetcher calls = %d -> %d, want the next lazy call to ask the vendor once", before, after)
			}
		})
	}

	t.Run("a live context still records an unverifiable attempt", func(t *testing.T) {
		svc, routeStore, fake := newDiscoveryTestService(t)
		fake.failUsage(unknownUsage())
		acc := connectedSubscription(t, svc, routeStore, routing.VendorOpenAI, "")
		if _, res, err := svc.RefreshVendorAccountUsage(context.Background(), ownerToken(), acc.ID, VendorUsageLazyTTL); err != nil || res.Status != VendorUsageRefreshUnverifiable {
			t.Fatalf("result = %+v, err = %v, want unverifiable / nil", res, err)
		}
		if !usagePullRecorded(svc, acc.ID) {
			t.Fatal("a real (vendor-answered) unverifiable attempt was not recorded")
		}
	})
}

// A wall clock that steps backward after a pull (an NTP correction) must not make
// every later lazy call "fresh" for as long as the clock stays behind: a negative
// age is stale, so the call pulls.
func TestRefreshVendorAccountUsageBackwardClockStepPullsAgain(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	fake.okUsage(usagePulled())
	acc := connectedSubscription(t, svc, routeStore, routing.VendorOpenAI, "")
	ctx := context.Background()

	if _, res, err := svc.RefreshVendorAccountUsage(ctx, ownerToken(), acc.ID, VendorUsageLazyTTL); err != nil || res.Status != VendorUsageRefreshOK {
		t.Fatalf("first call = %+v, err = %v, want ok", res, err)
	}
	setUsageClock(svc, discoveryTestNow.Add(-time.Hour))
	if _, res, err := svc.RefreshVendorAccountUsage(ctx, ownerToken(), acc.ID, VendorUsageLazyTTL); err != nil || res.Status != VendorUsageRefreshOK {
		t.Fatalf("lazy call after a backward clock step = %+v, err = %v, want ok (a negative age is stale)", res, err)
	}
	if n := len(fake.usageRecorded()); n != 2 {
		t.Fatalf("usage fetcher calls = %d, want 2", n)
	}
	// A forced refresh asks whatever the clock did.
	setUsageClock(svc, discoveryTestNow.Add(-2*time.Hour))
	if _, res, err := svc.RefreshVendorAccountUsage(ctx, ownerToken(), acc.ID, 0); err != nil || res.Status != VendorUsageRefreshOK {
		t.Fatalf("forced call after a backward clock step = %+v, err = %v, want ok", res, err)
	}
	if n := len(fake.usageRecorded()); n != 3 {
		t.Fatalf("usage fetcher calls = %d, want 3", n)
	}
}

// A token that cannot be had is the models refresh's fail-soft answer, not an
// error: unverifiable with the credential-free note as the detail, no vendor call
// and the stored snapshot answered. The vendor was never asked, so it is not
// counted toward the TTL (a later lazy call tries again).
func TestRefreshVendorAccountUsageUnverifiableWhenTheTokenCannotBeHad(t *testing.T) {
	ctx := context.Background()

	t.Run("expired token, no refresher wired", func(t *testing.T) {
		svc, routeStore, fake := newDiscoveryTestService(t)
		fake.okUsage(usagePulled())
		acc := expiredSubscription(t, svc, routeStore, routing.VendorOpenAI, "")
		before := passiveUsage(acc.ID)
		seedUsage(t, routeStore, before)

		usage, res, err := svc.RefreshVendorAccountUsage(ctx, ownerToken(), acc.ID, VendorUsageLazyTTL)
		if err != nil || res.Status != VendorUsageRefreshUnverifiable || !strings.Contains(res.Detail, noVendorTokenRefresherNote) {
			t.Fatalf("result = %+v, err = %v, want unverifiable carrying %q", res, err, noVendorTokenRefresherNote)
		}
		if usage == nil || usage.FiveHourPct != before.FiveHourPct {
			t.Fatalf("usage = %+v, want the stored snapshot %+v", usage, before)
		}
		fake.requireNoUsageCalls(t, "without a usable access token")
		for _, secret := range []string{refreshTestOldAccess, refreshTestRefresh} {
			requireNoSubstring(t, res, secret)
		}

		// Not counted: once a refresher is wired the next lazy call pulls.
		installTokenRefresher(t, svc, routeStore)
		if _, res, err := svc.RefreshVendorAccountUsage(ctx, ownerToken(), acc.ID, VendorUsageLazyTTL); err != nil || res.Status != VendorUsageRefreshOK {
			t.Fatalf("lazy call after the refresher is wired = %+v, err = %v, want ok", res, err)
		}
	})
	t.Run("expired token, failing refresher", func(t *testing.T) {
		svc, routeStore, fake := newDiscoveryTestService(t)
		fake.okUsage(usagePulled())
		installTokenRefresher(t, svc, routeStore).err = errors.New("vendor said no")
		acc := expiredSubscription(t, svc, routeStore, routing.VendorOpenAI, "")

		_, res, err := svc.RefreshVendorAccountUsage(ctx, ownerToken(), acc.ID, 0)
		if err != nil || res.Status != VendorUsageRefreshUnverifiable || !strings.Contains(res.Detail, expiredVendorTokenNote) {
			t.Fatalf("result = %+v, err = %v, want unverifiable carrying %q", res, err, expiredVendorTokenNote)
		}
		fake.requireNoUsageCalls(t, "without a usable access token")
	})
	t.Run("subscription not connected", func(t *testing.T) {
		svc, _, fake := newDiscoveryTestService(t)
		fake.okUsage(usagePulled())
		acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorOpenAI, "Not connected")

		usage, res, err := svc.RefreshVendorAccountUsage(ctx, ownerToken(), acc.ID, 0)
		if err != nil || res.Status != VendorUsageRefreshUnverifiable || res.Detail == "" {
			t.Fatalf("result = %+v, err = %v, want unverifiable with a detail", res, err)
		}
		if usage != nil {
			t.Fatalf("usage = %+v, want none", usage)
		}
		fake.requireNoUsageCalls(t, "without an access token")
	})
}

// An expired-but-refreshable token is renewed ONCE through the gateway's locked
// refresher and the usage is fetched with the FRESH token.
func TestRefreshVendorAccountUsageRenewsAnExpiredTokenOnce(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	fake.okUsage(usagePulled())
	refresher := installTokenRefresher(t, svc, routeStore)
	acc := expiredSubscription(t, svc, routeStore, routing.VendorOpenAI, "")

	_, res, err := svc.RefreshVendorAccountUsage(context.Background(), ownerToken(), acc.ID, 0)
	if err != nil || res.Status != VendorUsageRefreshOK {
		t.Fatalf("result = %+v, err = %v, want ok", res, err)
	}
	if got := refresher.recorded(); !reflect.DeepEqual(got, []string{acc.ID}) {
		t.Fatalf("refresher calls = %v, want exactly one", got)
	}
	if call := fake.onlyUsageCall(t); call.accessToken != refreshTestFreshAccess || call.accountID != discoveryTestAccount {
		t.Fatalf("usage call = %+v, want the refreshed access token and the account id", call)
	}
}

// A stored credential that cannot be OPENED is the error the models refresh and the
// connection test report; nothing is fetched and nothing leaks.
func TestRefreshVendorAccountUsageUnreadableCredentialIsAnError(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	fake.okUsage(usagePulled())
	acc := connectedSubscription(t, svc, routeStore, routing.VendorOpenAI, "")
	row := accountRow(t, svc, acc)
	row.OAuthTokens = "garbage-without-a-prefix"
	if err := routeStore.UpdateVendorAccount(context.Background(), row); err != nil {
		t.Fatalf("UpdateVendorAccount: %v", err)
	}

	usage, _, err := svc.RefreshVendorAccountUsage(context.Background(), ownerToken(), acc.ID, 0)
	if !errors.Is(err, ErrVendorAccountCredentialUnreadable) {
		t.Fatalf("err = %v, want ErrVendorAccountCredentialUnreadable", err)
	}
	if usage != nil {
		t.Fatalf("usage = %+v, want none on an error", usage)
	}
	fake.requireNoUsageCalls(t, "for an unreadable credential")
}

// The tracker survives concurrent refreshes (run under -race): the pulls of many
// goroutines for several accounts neither race nor lose the TTL.
func TestRefreshVendorAccountUsageIsSafeUnderConcurrency(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	fake.okUsage(usagePulled())
	ids := make([]string, 0, 3)
	for _, prefix := range []string{"a/", "b/", "c/"} {
		ids = append(ids, connectedSubscription(t, svc, routeStore, routing.VendorOpenAI, prefix).ID)
	}

	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			maxAge := VendorUsageLazyTTL
			if i%4 == 0 {
				maxAge = 0
			}
			if _, _, err := svc.RefreshVendorAccountUsage(context.Background(), ownerToken(), ids[i%len(ids)], maxAge); err != nil {
				t.Errorf("RefreshVendorAccountUsage: %v", err)
			}
		}(i)
	}
	wg.Wait()

	for _, id := range ids {
		if _, res, err := svc.RefreshVendorAccountUsage(context.Background(), ownerToken(), id, VendorUsageLazyTTL); err != nil || res.Status != VendorUsageRefreshFresh {
			t.Fatalf("after the concurrent pulls %s = %+v, err = %v, want fresh", id, res, err)
		}
	}
}

// The tracker itself: zero-value safe, strictly younger-than semantics, per-account,
// forget drops an entry, and a non-positive maxAge is never fresh.
func TestVendorUsagePullTracker(t *testing.T) {
	var tr vendorUsagePullTracker
	at := discoveryTestNow

	if tr.within("va_1", at, time.Minute) {
		t.Fatal("an account never pulled reads as fresh")
	}
	tr.record("va_1", at)
	for _, tc := range []struct {
		now    time.Time
		maxAge time.Duration
		want   bool
	}{
		{at, time.Minute, true},
		{at.Add(59 * time.Second), time.Minute, true},
		{at.Add(time.Minute), time.Minute, false},
		{at.Add(time.Hour), time.Minute, false},
		{at, 0, false},
		{at, -time.Minute, false},
	} {
		if got := tr.within("va_1", tc.now, tc.maxAge); got != tc.want {
			t.Errorf("within(now=%v, maxAge=%v) = %v, want %v", tc.now.Sub(at), tc.maxAge, got, tc.want)
		}
	}
	if tr.within("va_2", at, time.Minute) {
		t.Fatal("another account reads as fresh")
	}
	// A wall clock that stepped BACKWARD since the pull (an NTP correction) gives a
	// negative age: that is stale, never fresh, and a non-positive maxAge (the
	// manual refresh) bypasses the TTL whatever the age.
	for _, tc := range []struct {
		name   string
		now    time.Time
		maxAge time.Duration
	}{
		{"one second back", at.Add(-time.Second), time.Minute},
		{"an hour back", at.Add(-time.Hour), time.Minute},
		{"one second back, force", at.Add(-time.Second), 0},
		{"one second back, negative maxAge", at.Add(-time.Second), -time.Minute},
		{"same instant, force", at, 0},
		{"later, force", at.Add(time.Second), 0},
	} {
		if tr.within("va_1", tc.now, tc.maxAge) {
			t.Errorf("%s: within = true, want false (a negative age is stale; maxAge <= 0 always bypasses)", tc.name)
		}
	}
	tr.forget("va_1")
	if tr.within("va_1", at, time.Minute) {
		t.Fatal("a forgotten account still reads as fresh")
	}
	tr.forget("va_unknown") // must not panic
}

// A deleted account's tracker entry is dropped with it, so the map cannot grow with
// the accounts a gateway has ever had.
func TestDeleteVendorAccountForgetsItsUsagePull(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	fake.okUsage(usagePulled())
	acc := connectedSubscription(t, svc, routeStore, routing.VendorOpenAI, "")

	if _, _, err := svc.RefreshVendorAccountUsage(context.Background(), ownerToken(), acc.ID, 0); err != nil {
		t.Fatalf("RefreshVendorAccountUsage: %v", err)
	}
	if !svc.vendorUsagePulls.within(acc.ID, discoveryTestNow, VendorUsageLazyTTL) {
		t.Fatal("the pull was not recorded")
	}
	if _, err := svc.DeleteVendorAccount(context.Background(), ownerToken(), acc.ID); err != nil {
		t.Fatalf("DeleteVendorAccount: %v", err)
	}
	if svc.vendorUsagePulls.within(acc.ID, discoveryTestNow, VendorUsageLazyTTL) {
		t.Fatal("the deleted account's pull time is still tracked")
	}
}

// --- api-key accounts: OpenRouter / Kilo ----------------------------------------------------
//
// An api-key account of a provider whose preset names a usage strategy (OpenRouter's
// per-key limit, Kilo's balance) is pulled through the ONE compatible-usage seam: the
// portal composes the URL from the preset, opens the stored key and upserts what the
// vendor reported as it is (these accounts are the single usage writer, so there is
// nothing to merge with). The subscription path above is unchanged.

// openRouterCapped is a capped OpenRouter key: 40 of 100 USD spent.
func openRouterCapped() vendorauth.OpenAISubscriptionUsage {
	return vendorauth.OpenAISubscriptionUsage{
		FiveHourPct: -1, WeeklyPct: -1, SpendUnit: "USD", SpendLimit: "100", SpendUsed: "40",
		SpendRemaining: "60", SpendUsedPct: 40, CreditStatus: "has_credits",
	}
}

// openRouterUnlimited is the same key after its cap was removed: no limit, no spend.
func openRouterUnlimited() vendorauth.OpenAISubscriptionUsage {
	return vendorauth.OpenAISubscriptionUsage{FiveHourPct: -1, WeeklyPct: -1, SpendUsedPct: -1, CreditStatus: "unlimited"}
}

// usageAccount creates an api-key account of an OpenAI-compatible vendor on its
// preset's default root, from a clean slate. Creating the account runs the
// create-time discovery, whose best-effort usage pull (refreshVendorUsage) already
// asks the usage seam and records a pull; so the seam answers nothing usable while
// the account is created, and its calls and the recorded pull are forgotten
// afterwards. A test configures the seam's answer AFTER calling this.
func usageAccount(t *testing.T, svc *Service, fake *fakeVendorDiscoverers, vendor string) VendorAccountDTO {
	t.Helper()
	fake.failCompatUsage(unknownUsage())
	acc := compatDiscoveryAccount(t, svc, fake, vendor, "", "p-")
	svc.vendorUsagePulls.forget(acc.ID)
	return acc
}

func TestHasActiveVendorUsagePull(t *testing.T) {
	for _, tc := range []struct {
		name     string
		vendor   string
		authType string
		want     bool
	}{
		{"openrouter api key", routing.VendorOpenRouter, routing.VendorAuthAPIKey, true},
		{"kilo api key", routing.VendorKilo, routing.VendorAuthAPIKey, true},
		{"openai subscription", routing.VendorOpenAI, routing.VendorAuthSubscription, true},
		{"openai api key", routing.VendorOpenAI, routing.VendorAuthAPIKey, false},
		{"anthropic api key", routing.VendorAnthropic, routing.VendorAuthAPIKey, false},
		{"anthropic subscription", routing.VendorAnthropic, routing.VendorAuthSubscription, false},
		{"xai api key", routing.VendorXAI, routing.VendorAuthAPIKey, false},
		{"google api key", routing.VendorGoogle, routing.VendorAuthAPIKey, false},
		{"custom openai-compatible api key", routing.VendorOpenAICompatible, routing.VendorAuthAPIKey, false},
		{"openrouter subscription (not a real kind)", routing.VendorOpenRouter, routing.VendorAuthSubscription, false},
		{"unknown vendor api key", "no-such-vendor", routing.VendorAuthAPIKey, false},
		{"unknown auth type", routing.VendorOpenRouter, "no-such-auth", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasActiveVendorUsagePull(routing.VendorAccount{Vendor: tc.vendor, AuthType: tc.authType}); got != tc.want {
				t.Fatalf("hasActiveVendorUsagePull(%s, %s) = %v, want %v", tc.vendor, tc.authType, got, tc.want)
			}
		})
	}
}

// An OpenRouter / Kilo api-key account is pulled: the URL is composed from the
// preset's default root and usage path, the stored key is opened and handed over with
// the strategy and the bounded client, the snapshot is stored as fetched and
// answered -- and neither the model discovery nor the ChatGPT usage fetch runs.
func TestRefreshVendorAccountUsagePullsAPIKeyAccountsThroughThePresetStrategy(t *testing.T) {
	for _, tc := range []struct {
		name, vendor, wantVia, wantURL string
	}{
		{"openrouter", routing.VendorOpenRouter, "openrouter_key", "https://openrouter.ai/api/v1/key"},
		{"kilo", routing.VendorKilo, "kilo_balance", "https://api.kilo.ai/api/profile/balance"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, routeStore, fake := newDiscoveryTestService(t)
			acc := usageAccount(t, svc, fake, tc.vendor)
			fake.okCompatUsage(openRouterCapped())
			seed := storedModels(t, routeStore, acc.ID)
			ctx := context.Background()

			usage, res, err := svc.RefreshVendorAccountUsage(ctx, ownerToken(), acc.ID, VendorUsageLazyTTL)
			if err != nil {
				t.Fatalf("RefreshVendorAccountUsage: %v", err)
			}
			if res.Status != VendorUsageRefreshOK || res.Detail == "" {
				t.Fatalf("result = %+v, want ok with a detail", res)
			}
			call := fake.onlyCompatUsageCall(t)
			if call.usageVia != tc.wantVia || call.usageURL != tc.wantURL || call.apiKey != vendorAccountTestKey || !call.hasClient || call.timeout != 10*time.Second {
				t.Fatalf("compatible usage call = %+v, want via %q, URL %q, the opened key and the bounded 10s client", call, tc.wantVia, tc.wantURL)
			}
			fake.requireNoUsageCalls(t, "for an api-key account (the ChatGPT usage fetch is subscription-only)")
			fake.requireNoCalls(t, "for a usage refresh (the model discovery must not run)")
			if got := storedModels(t, routeStore, acc.ID); !reflect.DeepEqual(got, seed) {
				t.Fatalf("model rows = %+v, want them untouched %+v", got, seed)
			}
			want := routing.VendorAccountUsage{
				AccountID: acc.ID, FiveHourPct: -1, WeeklyPct: -1, SpendUnit: "USD", SpendLimit: "100",
				SpendUsed: "40", SpendRemaining: "60", SpendUsedPct: 40, CreditStatus: "has_credits", UpdatedAt: discoveryTestNow,
			}
			if got, found := storedUsage(t, routeStore, acc.ID); !found || !reflect.DeepEqual(got, want) {
				t.Fatalf("stored usage = %+v (found %v), want %+v", got, found, want)
			}
			if usage == nil || usage.SpendLimit != "100" || usage.SpendUsedPct != 40 || usage.CreditStatus != "has_credits" || !usage.UpdatedAt.Equal(discoveryTestNow) {
				t.Fatalf("answered usage = %+v, want the stored snapshot", usage)
			}
			requireNoToken(t, "usage", usage)
			requireNoToken(t, "result", res)
			if !usagePullRecorded(svc, acc.ID) {
				t.Fatal("the api-key pull was not recorded for the lazy TTL")
			}

			// A second lazy call inside the 5-minute TTL is fresh: the vendor is not asked.
			setUsageClock(svc, discoveryTestNow.Add(time.Minute))
			usage, res, err = svc.RefreshVendorAccountUsage(ctx, ownerToken(), acc.ID, VendorUsageLazyTTL)
			if err != nil || res.Status != VendorUsageRefreshFresh {
				t.Fatalf("second lazy call = %+v, err = %v, want fresh", res, err)
			}
			if usage == nil || usage.SpendLimit != "100" {
				t.Fatalf("fresh usage = %+v, want the stored snapshot", usage)
			}
			fake.onlyCompatUsageCall(t)

			// ?force (maxAge 0) bypasses the TTL.
			if _, res, err = svc.RefreshVendorAccountUsage(ctx, ownerToken(), acc.ID, 0); err != nil || res.Status != VendorUsageRefreshOK {
				t.Fatalf("forced call = %+v, err = %v, want ok", res, err)
			}
			if n := len(fake.compatUsageRecorded()); n != 2 {
				t.Fatalf("compatible usage fetcher calls = %d, want 2 after the forced call", n)
			}
		})
	}
}

// An api-key account with no stored key cannot be asked: unverifiable, saying why,
// and the fetcher is never called. (The strategy's own account kind: the check is
// the account's, not the vendor's.)
func TestRefreshVendorAccountUsageAPIKeyAccountWithoutAKeyIsUnverifiable(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	acc := usageAccount(t, svc, fake, routing.VendorOpenRouter)
	fake.okCompatUsage(openRouterCapped())
	row := accountRow(t, svc, acc)
	row.APIKey = ""
	if err := routeStore.UpdateVendorAccount(context.Background(), row); err != nil {
		t.Fatalf("UpdateVendorAccount: %v", err)
	}

	usage, res, err := svc.RefreshVendorAccountUsage(context.Background(), ownerToken(), acc.ID, VendorUsageLazyTTL)
	if err != nil {
		t.Fatalf("RefreshVendorAccountUsage: %v", err)
	}
	if res.Status != VendorUsageRefreshUnverifiable || !strings.Contains(res.Detail, "no API key is set") {
		t.Fatalf("result = %+v, want unverifiable saying no API key is set", res)
	}
	if usage != nil {
		t.Fatalf("usage = %+v, want none (nothing stored)", usage)
	}
	fake.requireNoCompatUsageCalls(t, "without a key")
	if usagePullRecorded(svc, acc.ID) {
		t.Fatal("a refresh that never reached the vendor was recorded as a pull")
	}
	if _, found := storedUsage(t, routeStore, acc.ID); found {
		t.Fatal("a usage snapshot was stored without a key")
	}
}

// A stored key that cannot be opened is the documented 409 error of this endpoint,
// exactly as for a subscription's token set; nothing is fetched.
func TestRefreshVendorAccountUsageAPIKeyUnreadableCredentialIsAnError(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	acc := usageAccount(t, svc, fake, routing.VendorKilo)
	fake.okCompatUsage(openRouterCapped())
	row := accountRow(t, svc, acc)
	row.APIKey = "garbage-without-a-prefix"
	if err := routeStore.UpdateVendorAccount(context.Background(), row); err != nil {
		t.Fatalf("UpdateVendorAccount: %v", err)
	}

	usage, _, err := svc.RefreshVendorAccountUsage(context.Background(), ownerToken(), acc.ID, 0)
	if !errors.Is(err, ErrVendorAccountCredentialUnreadable) {
		t.Fatalf("err = %v, want ErrVendorAccountCredentialUnreadable", err)
	}
	if usage != nil {
		t.Fatalf("usage = %+v, want none on an error", usage)
	}
	fake.requireNoCompatUsageCalls(t, "for an unreadable credential")
}

// Kilo's balance endpoint is undocumented: a miss (the fetcher answers Unverifiable)
// is unverifiable, never a write -- the last good snapshot is kept and answered. The
// vendor WAS asked, so the attempt counts toward the lazy TTL; a manual refresh asks again.
func TestRefreshVendorAccountUsageAPIKeyUnverifiableKeepsTheStoredUsage(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	acc := usageAccount(t, svc, fake, routing.VendorKilo)
	fake.failCompatUsage(vendorauth.OpenAISubscriptionUsage{FiveHourPct: 99, WeeklyPct: 99, CreditBalance: "junk", SpendUsedPct: -1})
	before := routing.VendorAccountUsage{
		AccountID: acc.ID, FiveHourPct: -1, WeeklyPct: -1, CreditBalance: "7.50", SpendUsedPct: -1,
		CreditStatus: "has_credits", UpdatedAt: discoveryTestNow.Add(-time.Hour),
	}
	seedUsage(t, routeStore, before)
	ctx := context.Background()

	usage, res, err := svc.RefreshVendorAccountUsage(ctx, ownerToken(), acc.ID, VendorUsageLazyTTL)
	if err != nil || res.Status != VendorUsageRefreshUnverifiable || res.Detail == "" {
		t.Fatalf("result = %+v, err = %v, want unverifiable with a detail", res, err)
	}
	if usage == nil || usage.CreditBalance != "7.50" {
		t.Fatalf("usage = %+v, want the last good snapshot answered", usage)
	}
	if got, _ := storedUsage(t, routeStore, acc.ID); !reflect.DeepEqual(got, before) {
		t.Fatalf("stored usage = %+v, want it untouched %+v", got, before)
	}
	fake.onlyCompatUsageCall(t)

	if _, res, _ := svc.RefreshVendorAccountUsage(ctx, ownerToken(), acc.ID, VendorUsageLazyTTL); res.Status != VendorUsageRefreshFresh {
		t.Fatalf("lazy call after a failed attempt = %+v, want fresh (the vendor is not hammered)", res)
	}
	fake.onlyCompatUsageCall(t)
	if _, res, _ := svc.RefreshVendorAccountUsage(ctx, ownerToken(), acc.ID, 0); res.Status != VendorUsageRefreshUnverifiable {
		t.Fatalf("forced call = %+v, want unverifiable (the button asks again)", res)
	}
	if n := len(fake.compatUsageRecorded()); n != 2 {
		t.Fatalf("compatible usage fetcher calls = %d, want 2", n)
	}

	// With no snapshot stored yet, a miss writes nothing at all.
	other := usageAccount(t, svc, fake, routing.VendorKilo)
	if _, res, err := svc.RefreshVendorAccountUsage(ctx, ownerToken(), other.ID, 0); err != nil || res.Status != VendorUsageRefreshUnverifiable {
		t.Fatalf("no-snapshot miss = %+v, err = %v, want unverifiable / nil", res, err)
	}
	if _, found := storedUsage(t, routeStore, other.ID); found {
		t.Fatal("a failed pull stored a snapshot")
	}
}

// The api-key path upserts AS FETCHED (no merge): these accounts are the single usage
// writer, and a merge cannot express "now absent" -- a cap that was removed would keep
// its stale limit and spend. The subscription path keeps its merge (see
// TestRefreshVendorAccountUsageAnswersTheMergedSnapshot).
func TestRefreshVendorAccountUsageAPIKeyUpsertsAsFetchedNotMerged(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	acc := usageAccount(t, svc, fake, routing.VendorOpenRouter)
	ctx := context.Background()

	fake.okCompatUsage(openRouterCapped())
	if _, res, err := svc.RefreshVendorAccountUsage(ctx, ownerToken(), acc.ID, 0); err != nil || res.Status != VendorUsageRefreshOK {
		t.Fatalf("first pull = %+v, err = %v, want ok", res, err)
	}
	if got, _ := storedUsage(t, routeStore, acc.ID); got.SpendLimit != "100" || got.SpendUsedPct != 40 {
		t.Fatalf("first snapshot = %+v, want the capped key", got)
	}

	// The owner removed the key's cap at the vendor: the next pull reports unlimited.
	fake.okCompatUsage(openRouterUnlimited())
	later := discoveryTestNow.Add(10 * time.Minute)
	setUsageClock(svc, later)
	usage, res, err := svc.RefreshVendorAccountUsage(ctx, ownerToken(), acc.ID, 0)
	if err != nil || res.Status != VendorUsageRefreshOK {
		t.Fatalf("second pull = %+v, err = %v, want ok", res, err)
	}
	want := routing.VendorAccountUsage{AccountID: acc.ID, FiveHourPct: -1, WeeklyPct: -1, SpendUsedPct: -1, CreditStatus: "unlimited", UpdatedAt: later}
	if got, found := storedUsage(t, routeStore, acc.ID); !found || !reflect.DeepEqual(got, want) {
		t.Fatalf("stored usage = %+v (found %v), want exactly the unlimited snapshot %+v (no stale limit or spend merged in)", got, found, want)
	}
	if usage == nil || usage.SpendLimit != "" || usage.SpendUsed != "" || usage.SpendRemaining != "" || usage.SpendUnit != "" || usage.SpendUsedPct != -1 || usage.CreditStatus != "unlimited" {
		t.Fatalf("answered usage = %+v, want unlimited with the spend fields cleared", usage)
	}
}

// A store that cannot take the snapshot makes the pull unverifiable, the stored row
// as it was; a store that cannot be READ for the answer is the one real error.
func TestRefreshVendorAccountUsageAPIKeyStoreFailures(t *testing.T) {
	ctx := context.Background()

	t.Run("snapshot write fails", func(t *testing.T) {
		svc, routeStore, fake := newDiscoveryTestService(t)
		acc := usageAccount(t, svc, fake, routing.VendorOpenRouter)
		fake.okCompatUsage(openRouterCapped())
		before := routing.VendorAccountUsage{AccountID: acc.ID, FiveHourPct: -1, WeeklyPct: -1, SpendUsedPct: -1, CreditStatus: "unlimited", UpdatedAt: discoveryTestNow.Add(-time.Hour)}
		seedUsage(t, routeStore, before)
		svc.routes = failUsageStore{Store: routeStore, writeErr: errors.New("usage table unavailable")}

		usage, res, err := svc.RefreshVendorAccountUsage(ctx, ownerToken(), acc.ID, 0)
		if err != nil || res.Status != VendorUsageRefreshUnverifiable {
			t.Fatalf("result = %+v, err = %v, want unverifiable / nil", res, err)
		}
		if usage == nil || usage.CreditStatus != "unlimited" {
			t.Fatalf("usage = %+v, want the stored snapshot", usage)
		}
		if got, _ := storedUsage(t, routeStore, acc.ID); !reflect.DeepEqual(got, before) {
			t.Fatalf("stored usage = %+v, want it untouched %+v", got, before)
		}
	})
	t.Run("snapshot read fails", func(t *testing.T) {
		svc, routeStore, fake := newDiscoveryTestService(t)
		acc := usageAccount(t, svc, fake, routing.VendorOpenRouter)
		fake.okCompatUsage(openRouterCapped())
		svc.routes = failUsageStore{Store: routeStore, readErr: errors.New("usage table unavailable")}

		if _, _, err := svc.RefreshVendorAccountUsage(ctx, ownerToken(), acc.ID, 0); err == nil || !strings.Contains(err.Error(), "usage table unavailable") {
			t.Fatalf("err = %v, want the store failure surfaced", err)
		}
	})
}

// A pull whose context ended before the vendor could answer is no recorded pull (the
// same rule as the subscription path): the next lazy call asks the vendor instead of
// answering "fresh" with no snapshot for the TTL.
func TestRefreshVendorAccountUsageAPIKeyDeadContextIsNotARecordedPull(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	acc := usageAccount(t, svc, fake, routing.VendorOpenRouter)
	fake.failCompatUsage(unknownUsage())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, res, err := svc.RefreshVendorAccountUsage(ctx, ownerToken(), acc.ID, VendorUsageLazyTTL); err != nil || res.Status != VendorUsageRefreshUnverifiable {
		t.Fatalf("result = %+v, err = %v, want unverifiable / nil", res, err)
	}
	if usagePullRecorded(svc, acc.ID) {
		t.Fatal("a pull that never reached the vendor was recorded")
	}

	fake.okCompatUsage(openRouterCapped())
	if usage, res, err := svc.RefreshVendorAccountUsage(context.Background(), ownerToken(), acc.ID, VendorUsageLazyTTL); err != nil || res.Status != VendorUsageRefreshOK || usage == nil {
		t.Fatalf("next lazy call = %+v / %+v, err = %v, want ok with usage (it must pull again)", usage, res, err)
	}
	if _, found := storedUsage(t, routeStore, acc.ID); !found {
		t.Fatal("the later live pull stored nothing")
	}
}

// The usage-capable gate is the registry's, not a vendor list: the OpenAI-compatible
// providers whose preset names no usage strategy (x.ai, Google, a custom endpoint) and
// the native openai/anthropic api keys stay "unsupported" and ask nothing.
func TestRefreshVendorAccountUsageAPIKeyWithoutAStrategyIsUnsupported(t *testing.T) {
	for _, tc := range []struct {
		name string
		make func(t *testing.T, svc *Service, fake *fakeVendorDiscoverers) VendorAccountDTO
	}{
		{"xai", func(t *testing.T, svc *Service, fake *fakeVendorDiscoverers) VendorAccountDTO {
			return usageAccount(t, svc, fake, routing.VendorXAI)
		}},
		{"google", func(t *testing.T, svc *Service, fake *fakeVendorDiscoverers) VendorAccountDTO {
			return usageAccount(t, svc, fake, routing.VendorGoogle)
		}},
		{"custom openai-compatible", func(t *testing.T, svc *Service, fake *fakeVendorDiscoverers) VendorAccountDTO {
			return compatDiscoveryAccount(t, svc, fake, routing.VendorOpenAICompatible, "https://gw.example.test/root", "p-")
		}},
		{"native openai api key", func(t *testing.T, svc *Service, _ *fakeVendorDiscoverers) VendorAccountDTO {
			return apiKeyAccount(t, svc, routing.VendorOpenAI, "")
		}},
		{"native anthropic api key", func(t *testing.T, svc *Service, _ *fakeVendorDiscoverers) VendorAccountDTO {
			return apiKeyAccount(t, svc, routing.VendorAnthropic, "")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, routeStore, fake := newDiscoveryTestService(t)
			acc := tc.make(t, svc, fake)
			fake.okCompatUsage(openRouterCapped()) // an answer that WOULD be stored if the seam were consulted
			fake.okUsage(usagePulled())

			usage, res, err := svc.RefreshVendorAccountUsage(context.Background(), ownerToken(), acc.ID, 0)
			if err != nil || res.Status != VendorUsageRefreshUnsupported || res.Detail == "" {
				t.Fatalf("result = %+v, err = %v, want unsupported with a detail", res, err)
			}
			if strings.Contains(res.Detail, "OpenAI subscription accounts") {
				t.Fatalf("detail = %q, must not claim only OpenAI subscriptions have a usage pull", res.Detail)
			}
			if usage != nil {
				t.Fatalf("usage = %+v, want none", usage)
			}
			fake.requireNoCompatUsageCalls(t, "for an account whose preset names no usage strategy")
			fake.requireNoUsageCalls(t, "for an api-key account")
			if _, found := storedUsage(t, routeStore, acc.ID); found {
				t.Fatal("a usage snapshot was stored for an unsupported account")
			}
		})
	}
}

// Creating an OpenRouter / Kilo account with its key runs the create-time discovery,
// whose best-effort usage pull populates the snapshot at once (as connecting an OpenAI
// subscription does) and counts as an active pull for the lazy TTL.
func TestCreateVendorAccountPullsAPIKeyUsageOnTheWay(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	fake.okSlugs(kindOpenAICompatibleAPIKey, "model-a")
	fake.okCompatUsage(openRouterCapped())

	acc := createTestVendorAccount(t, svc, ownerToken(), compatAccountRequest(routing.VendorOpenRouter, ""))

	call := fake.onlyCompatUsageCall(t)
	if call.usageVia != "openrouter_key" || call.usageURL != "https://openrouter.ai/api/v1/key" || call.apiKey != vendorAccountTestKey {
		t.Fatalf("compatible usage call = %+v, want the OpenRouter key URL and the opened key", call)
	}
	got, found := storedUsage(t, routeStore, acc.ID)
	if !found || got.SpendLimit != "100" || got.CreditStatus != "has_credits" {
		t.Fatalf("stored usage = %+v (found %v), want the snapshot pulled at create time", got, found)
	}
	if !usagePullRecorded(svc, acc.ID) {
		t.Fatal("the create-time pull was not recorded for the lazy TTL")
	}
}

// The usage pull the models refresh runs on the way also covers an OpenRouter / Kilo
// api-key account: its snapshot is populated after RefreshVendorAccountModels, the
// pull counts toward the lazy TTL, and a vendor that fails it changes nothing about
// the models result.
func TestRefreshVendorAccountModelsPullsAPIKeyUsageOnTheWay(t *testing.T) {
	t.Run("the snapshot is populated and the pull is recorded", func(t *testing.T) {
		svc, routeStore, fake := newDiscoveryTestService(t)
		fake.okSlugs(kindOpenAICompatibleAPIKey, "model-a")
		acc := usageAccount(t, svc, fake, routing.VendorOpenRouter)
		fake.okCompatUsage(openRouterCapped())
		ctx := context.Background()

		_, res, err := svc.RefreshVendorAccountModels(ctx, ownerToken(), acc.ID)
		if err != nil || res.Status != VendorRefreshOK {
			t.Fatalf("models refresh = %+v, err = %v, want ok", res, err)
		}
		call := fake.onlyCompatUsageCall(t)
		if call.usageVia != "openrouter_key" || call.usageURL != "https://openrouter.ai/api/v1/key" || call.apiKey != vendorAccountTestKey {
			t.Fatalf("compatible usage call = %+v, want the OpenRouter key URL and the opened key", call)
		}
		fake.requireNoUsageCalls(t, "for an api-key account")
		got, found := storedUsage(t, routeStore, acc.ID)
		if !found || got.SpendLimit != "100" || got.SpendUsedPct != 40 || !got.UpdatedAt.Equal(discoveryTestNow) {
			t.Fatalf("stored usage = %+v (found %v), want the pulled snapshot", got, found)
		}

		// The lazy refresh right after is fresh: the models refresh's pull was an active one.
		if _, res, err := svc.RefreshVendorAccountUsage(ctx, ownerToken(), acc.ID, VendorUsageLazyTTL); err != nil || res.Status != VendorUsageRefreshFresh {
			t.Fatalf("lazy usage refresh = %+v, err = %v, want fresh", res, err)
		}
		fake.onlyCompatUsageCall(t)
	})

	t.Run("a failing usage pull never changes the models result", func(t *testing.T) {
		svc, routeStore, fake := newDiscoveryTestService(t)
		fake.okSlugs(kindOpenAICompatibleAPIKey, "model-a", "model-b")
		acc := usageAccount(t, svc, fake, routing.VendorKilo)
		fake.failCompatUsage(unknownUsage())

		_, res, err := svc.RefreshVendorAccountModels(context.Background(), ownerToken(), acc.ID)
		if err != nil || res.Status != VendorRefreshOK || res.Discovered != 2 {
			t.Fatalf("models refresh = %+v, err = %v, want ok / 2 regardless of the usage pull", res, err)
		}
		fake.onlyCompatUsageCall(t)
		if _, found := storedUsage(t, routeStore, acc.ID); found {
			t.Fatal("a failed pull stored a snapshot")
		}
	})

	t.Run("an account with no key never reaches the usage seam", func(t *testing.T) {
		svc, routeStore, fake := newDiscoveryTestService(t)
		acc := usageAccount(t, svc, fake, routing.VendorOpenRouter)
		fake.okCompatUsage(openRouterCapped())
		row := accountRow(t, svc, acc)
		row.APIKey = ""
		if err := routeStore.UpdateVendorAccount(context.Background(), row); err != nil {
			t.Fatalf("UpdateVendorAccount: %v", err)
		}

		if _, res, err := svc.RefreshVendorAccountModels(context.Background(), ownerToken(), acc.ID); err != nil || res.Status != VendorRefreshUnverifiable {
			t.Fatalf("models refresh = %+v, err = %v, want unverifiable / nil", res, err)
		}
		fake.requireNoCompatUsageCalls(t, "without a key")
	})

	t.Run("providers without a usage strategy are not pulled", func(t *testing.T) {
		for _, vendor := range []string{routing.VendorXAI, routing.VendorGoogle} {
			svc, _, fake := newDiscoveryTestService(t)
			fake.okSlugs(kindOpenAICompatibleAPIKey, "model-a")
			acc := usageAccount(t, svc, fake, vendor)
			fake.okCompatUsage(openRouterCapped())
			if _, res, err := svc.RefreshVendorAccountModels(context.Background(), ownerToken(), acc.ID); err != nil || res.Status != VendorRefreshOK {
				t.Fatalf("%s: models refresh = %+v, err = %v, want ok", vendor, res, err)
			}
			fake.requireNoCompatUsageCalls(t, "for "+vendor+" (its preset names no usage strategy)")
		}
	})
}
