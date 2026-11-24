// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/vendorauth"
	"sync"
	"testing"
	"time"
)

var _ routing.VendorUsagePuller = (*Service)(nil)

// anthropicPulled is the snapshot the Anthropic usage fake answers.
func anthropicPulled() vendorauth.OpenAISubscriptionUsage {
	return vendorauth.OpenAISubscriptionUsage{FiveHourPct: 99.5, FiveHourResetAt: usageAt(2), WeeklyPct: 40, WeeklyResetAt: usageAt(100), SpendUsedPct: -1}
}

// requestPull asks for a pull at now (the fake clock) and waits for the detached
// pull goroutine, if one was started, to finish.
func requestPull(svc *Service, id string, now time.Time) {
	setUsageClock(svc, now)
	svc.RequestVendorUsagePull(id)
	svc.waitVendorGroupPulls()
}

func TestVendorGroupPullAtMostOncePerInterval(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	fake.okAnthropicUsage(anthropicPulled())
	acc := connectedSubscription(t, svc, routeStore, routing.VendorAnthropic, "")
	t0 := discoveryTestNow

	requestPull(svc, acc.ID, t0)
	requestPull(svc, acc.ID, t0.Add(time.Minute))
	requestPull(svc, acc.ID, t0.Add(vendorGroupUsagePullInterval-time.Second))
	if got := len(fake.anthropicUsageRecorded()); got != 1 {
		t.Fatalf("fetches inside the interval = %d, want 1", got)
	}

	requestPull(svc, acc.ID, t0.Add(vendorGroupUsagePullInterval))
	if got := len(fake.anthropicUsageRecorded()); got != 2 {
		t.Fatalf("fetches after the interval = %d, want 2", got)
	}
	call := fake.anthropicUsageRecorded()[0]
	if call.accessToken != discoveryTestAccess || !call.hasClient {
		t.Fatalf("fetch call = %+v, want the opened access token and the bounded client", call)
	}
	stored, found := storedUsage(t, routeStore, acc.ID)
	if !found || stored.FiveHourPct != 99.5 || stored.WeeklyPct != 40 {
		t.Fatalf("stored usage = %+v (found %v), want the pulled snapshot", stored, found)
	}
}

func TestVendorGroupPullBacksOffAfterRateLimit(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	fake.failAnthropicUsage(vendorauth.ErrUsageRateLimited)
	acc := connectedSubscription(t, svc, routeStore, routing.VendorAnthropic, "")

	now := discoveryTestNow
	requestPull(svc, acc.ID, now)
	wantCalls := 1
	// Each rate-limited fetch doubles the wait: 30 min, 60 min, 120 min, then the
	// 4 h cap holds.
	for _, wait := range []time.Duration{30 * time.Minute, time.Hour, 2 * time.Hour, vendorGroupUsagePullMaxBackoff, vendorGroupUsagePullMaxBackoff} {
		requestPull(svc, acc.ID, now.Add(wait-time.Second))
		if got := len(fake.anthropicUsageRecorded()); got != wantCalls {
			t.Fatalf("a request %v - 1s after a rate-limited fetch pulled (calls = %d, want %d)", wait, got, wantCalls)
		}
		now = now.Add(wait)
		requestPull(svc, acc.ID, now)
		wantCalls++
		if got := len(fake.anthropicUsageRecorded()); got != wantCalls {
			t.Fatalf("a request %v after a rate-limited fetch did not pull (calls = %d, want %d)", wait, got, wantCalls)
		}
	}

	// A success resets the backoff to the plain interval.
	fake.okAnthropicUsage(anthropicPulled())
	now = now.Add(vendorGroupUsagePullMaxBackoff)
	requestPull(svc, acc.ID, now)
	wantCalls++
	requestPull(svc, acc.ID, now.Add(vendorGroupUsagePullInterval))
	wantCalls++
	if got := len(fake.anthropicUsageRecorded()); got != wantCalls {
		t.Fatalf("calls after success = %d, want %d (back to the 15 min interval)", got, wantCalls)
	}
}

func TestVendorGroupPullSkipsAccountsWithoutAnActivePull(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	fake.okAnthropicUsage(anthropicPulled())
	fake.okUsage(usagePulled())
	openaiKey := apiKeyAccount(t, svc, routing.VendorOpenAI, "")
	anthropicKey := apiKeyAccount(t, svc, routing.VendorAnthropic, "")
	// Creating the accounts may have run a create-time discovery.
	fake.reset()

	requestPull(svc, openaiKey.ID, discoveryTestNow)
	requestPull(svc, anthropicKey.ID, discoveryTestNow)
	requestPull(svc, "va_missing", discoveryTestNow)
	fake.requireNoUsageCalls(t, "for an OpenAI api-key account")
	fake.requireNoCompatUsageCalls(t, "for a native api-key account")
	if got := fake.anthropicUsageRecorded(); len(got) != 0 {
		t.Fatalf("anthropic usage calls = %+v, want none for api-key or unknown accounts", got)
	}
	if _, found := storedUsage(t, routeStore, openaiKey.ID); found {
		t.Fatal("a usage snapshot was stored for an api-key account")
	}
}

func TestVendorGroupPullPullsAnOpenAISubscriptionToo(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	fake.okUsage(usagePulled())
	acc := connectedSubscription(t, svc, routeStore, routing.VendorOpenAI, "")

	requestPull(svc, acc.ID, discoveryTestNow)
	fake.onlyUsageCall(t)
}

func TestVendorGroupPullNeverBlocksTheCaller(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	fake.okAnthropicUsage(anthropicPulled())
	gate := make(chan struct{})
	fake.anthropicUsageGate = gate
	acc := connectedSubscription(t, svc, routeStore, routing.VendorAnthropic, "")
	setUsageClock(svc, discoveryTestNow)

	returned := make(chan struct{})
	go func() {
		svc.RequestVendorUsagePull(acc.ID)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("RequestVendorUsagePull blocked on the vendor fetch")
	}
	close(gate)
	svc.waitVendorGroupPulls()
	if got := len(fake.anthropicUsageRecorded()); got != 1 {
		t.Fatalf("fetches = %d, want 1", got)
	}
}

// Many resolve-path callers hitting the throttle at once start exactly one pull.
func TestVendorGroupPullConcurrentRequestsStartOnePull(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	fake.okAnthropicUsage(anthropicPulled())
	acc := connectedSubscription(t, svc, routeStore, routing.VendorAnthropic, "")
	setUsageClock(svc, discoveryTestNow)

	var wg sync.WaitGroup
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			svc.RequestVendorUsagePull(acc.ID)
		}()
	}
	wg.Wait()
	svc.waitVendorGroupPulls()
	if got := len(fake.anthropicUsageRecorded()); got != 1 {
		t.Fatalf("fetches = %d, want 1", got)
	}
}

// The dedicated refresh pulls an Anthropic subscription through its own fetcher and
// merges the answer over what the passive scrape stored (a window the pull does not
// know keeps the scraped value).
func TestRefreshVendorAccountUsagePullsAnAnthropicSubscription(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	acc := connectedSubscription(t, svc, routeStore, routing.VendorAnthropic, "")
	seedUsage(t, routeStore, passiveUsage(acc.ID))
	weeklyOnly := vendorauth.OpenAISubscriptionUsage{FiveHourPct: -1, WeeklyPct: 77, WeeklyResetAt: usageAt(50), SpendUsedPct: -1}
	fake.okAnthropicUsage(weeklyOnly)

	_, res, err := svc.RefreshVendorAccountUsage(context.Background(), ownerToken(), acc.ID, 0)
	if err != nil || res.Status != VendorUsageRefreshOK {
		t.Fatalf("result = %+v, err = %v, want ok", res, err)
	}
	fake.requireNoUsageCalls(t, "for an Anthropic subscription")
	stored, _ := storedUsage(t, routeStore, acc.ID)
	before := passiveUsage(acc.ID)
	if stored.WeeklyPct != 77 || stored.FiveHourPct != before.FiveHourPct {
		t.Fatalf("stored = %+v, want weekly 77 from the pull and the scraped five-hour %v kept", stored, before.FiveHourPct)
	}
}

// A 429 from the usage endpoint during the dedicated refresh also backs the group
// pull of that account off.
func TestRefreshVendorAccountUsageRateLimitBacksTheGroupPullOff(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	acc := connectedSubscription(t, svc, routeStore, routing.VendorAnthropic, "")
	fake.failAnthropicUsage(vendorauth.ErrUsageRateLimited)
	setUsageClock(svc, discoveryTestNow)

	_, res, err := svc.RefreshVendorAccountUsage(context.Background(), ownerToken(), acc.ID, 0)
	if err != nil || res.Status != VendorUsageRefreshUnverifiable {
		t.Fatalf("result = %+v, err = %v, want unverifiable", res, err)
	}
	requestPull(svc, acc.ID, discoveryTestNow.Add(vendorGroupUsagePullInterval))
	if got := len(fake.anthropicUsageRecorded()); got != 1 {
		t.Fatalf("fetches = %d, want only the manual one (30 min backoff)", got)
	}
	requestPull(svc, acc.ID, discoveryTestNow.Add(2*vendorGroupUsagePullInterval))
	if got := len(fake.anthropicUsageRecorded()); got != 2 {
		t.Fatalf("fetches = %d, want 2 after the backoff", got)
	}
}
