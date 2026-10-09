// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
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

// Only an OpenAI SUBSCRIPTION has an active usage pull. Any other account (an api
// key of either vendor, an Anthropic subscription) answers "unsupported": no token
// is opened or renewed (a sealed credential that cannot be read is no error here),
// no fetcher runs, and the stored snapshot, if any, is still answered.
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
	if err != nil || res.Status != VendorUsageRefreshFresh || res.Detail == "" {
		t.Fatalf("second lazy call = %+v, err = %v, want fresh with a detail", res, err)
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
