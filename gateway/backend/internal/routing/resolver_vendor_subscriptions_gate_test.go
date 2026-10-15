// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"context"
	"errors"
	"op-ai-gateway/internal/inference"
	"testing"
	"time"
)

// subscriptionsGateResolver is a resolver with the vendor branch ON
// (vendor_first) and the vendor_subscriptions_enabled accessor set to
// subscriptionsAllowed (nil = never wired, which must mean "allowed").
func subscriptionsGateResolver(store resolverStore, now time.Time, subscriptionsAllowed func() bool) *Resolver {
	r := NewResolver(store, func() time.Time { return now }, nil)
	r.SetVendorAccountAccessors(func() bool { return true }, func() string { return vendorRoutingModeVendorFirst }, subscriptionsAllowed)
	return r
}

// subscriptionsGateStore holds, for the one owner, an api_key OpenAI account plus
// an Anthropic and an OpenAI SUBSCRIPTION account, each serving one distinct model.
func subscriptionsGateStore(t *testing.T, now time.Time) *MemoryStore {
	t.Helper()
	store := NewMemoryStore()
	seedVendorAccount(t, store, now, "acc_key", vendorOwner, VendorOpenAI, vendorKey, "", "gpt-4o", APIFlavorOpenAI)
	seedVendorSubscriptionAccount(t, store, now, "acc_sub_ant", VendorAnthropic, "enc:tokens", VendorAccountStatusActive, "", "claude-sonnet", APIFlavorAnthropic)
	seedVendorSubscriptionAccount(t, store, now, "acc_sub_oai", VendorOpenAI, "enc:tokens", VendorAccountStatusActive, "", "gpt-5-codex", APIFlavorOpenAI)
	return store
}

// TestVendorSubscriptionsGateSkipsOnlySubscriptionAccounts is the core proof of the
// vendor_subscriptions_enabled gate: with it OFF a subscription account (either
// vendor) no longer serves its models (ErrNoModelRoute, the same fall-through a
// needs_reconnect account gets), while an api_key account of the same owner is
// untouched; with it ON everything resolves. The accessor is read on EVERY resolve,
// so flipping it live takes effect on the next request.
func TestVendorSubscriptionsGateSkipsOnlySubscriptionAccounts(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	allowed := true
	resolver := subscriptionsGateResolver(subscriptionsGateStore(t, now), now, func() bool { return allowed })

	requests := []struct {
		name          string
		model, flavor string
		subscription  bool
	}{
		{"api_key openai", "gpt-4o", "openai_chat", false},
		{"anthropic subscription (messages)", "claude-sonnet", "anthropic_messages", true},
		{"anthropic subscription (chat)", "claude-sonnet", "openai_chat", true},
		{"openai subscription (chat)", "gpt-5-codex", "openai_chat", true},
		{"openai subscription (responses)", "gpt-5-codex", "openai_responses", true},
	}
	for _, state := range []struct {
		label   string
		allowed bool
	}{{"on", true}, {"off", false}, {"on again", true}} {
		allowed = state.allowed
		for _, rq := range requests {
			_, err := resolver.Resolve(ctx, ownerToken(), inference.Request{Model: rq.model, APIFlavor: rq.flavor})
			wantRoute := state.allowed || !rq.subscription
			switch {
			case wantRoute && err != nil:
				t.Errorf("subscriptions %s, %s: Resolve = %v, want it to resolve", state.label, rq.name, err)
			case !wantRoute && !errors.Is(err, ErrNoModelRoute):
				t.Errorf("subscriptions %s, %s: Resolve = %v, want ErrNoModelRoute (subscription accounts must stop routing)", state.label, rq.name, err)
			}
		}
	}
}

// With the accessor never wired (nil) subscriptions are ALLOWED: the setting is
// default-on, so a resolver built without the accessor must not silently drop an
// operator's subscription accounts.
func TestVendorSubscriptionsGateNilAccessorDefaultsOn(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	resolver := subscriptionsGateResolver(subscriptionsGateStore(t, now), now, nil)

	for _, model := range []string{"claude-sonnet", "gpt-5-codex"} {
		flavor := "openai_chat"
		if _, err := resolver.Resolve(ctx, ownerToken(), inference.Request{Model: model, APIFlavor: flavor}); err != nil {
			t.Errorf("Resolve(%q) with no subscriptions accessor = %v, want a subscription target (default on)", model, err)
		}
	}
}

// The gate does not widen anything: a subscription account stays unreachable when
// the master flag is OFF even if the subscriptions accessor says "allowed".
func TestVendorSubscriptionsGateIsNarrowerThanTheMasterFlag(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	resolver := NewResolver(subscriptionsGateStore(t, now), func() time.Time { return now }, nil)
	resolver.SetVendorAccountAccessors(func() bool { return false }, nil, func() bool { return true })

	for _, model := range []string{"gpt-4o", "claude-sonnet", "gpt-5-codex"} {
		if _, err := resolver.Resolve(ctx, ownerToken(), inference.Request{Model: model, APIFlavor: "openai_chat"}); !errors.Is(err, ErrNoModelRoute) {
			t.Errorf("Resolve(%q) with the master flag off = %v, want ErrNoModelRoute", model, err)
		}
	}
}

// A skipped subscription account falls through to the standard path like any other
// skipped account: with a same-named self-hosted mapping the request lands on the
// self-hosted server (vendor_first would otherwise have preferred the vendor).
func TestVendorSubscriptionsGateFallsThroughToSelfHosted(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	must(t, store.CreateAIServer(ctx, AIServer{ID: "srv_fast", Name: "srv_fast", Domain: "srv_fast.test", Status: ServerStatusActive, HealthStatus: HealthHealthy, CreatedAt: now, UpdatedAt: now}))
	must(t, store.CreateApplication(ctx, Application{ID: "app_fast", ServerID: "srv_fast", Type: ProviderMock, Port: 8000, Scheme: "http", APIFlavors: []string{APIFlavorOpenAI}, Priority: 10, Weight: 50, TimeoutMS: 30000, AffinityTTLSeconds: 1800, Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	must(t, store.CreateMapping(ctx, ModelMapping{ID: "map_fast", ApplicationID: "app_fast", GatewayModelName: "shared-model", AppModelName: "self-hosted-7b", Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	must(t, store.UpsertTelemetry(ctx, ServerTelemetry{ServerID: "srv_fast", ReportedAt: now, LatencyMS: 100, ProviderHealth: "{}", Capabilities: "{}", RawSummary: "{}", UpdatedAt: now}))
	seedVendorSubscriptionAccount(t, store, now, "acc_sub_oai", VendorOpenAI, "enc:tokens", VendorAccountStatusActive, "", "shared-model", APIFlavorOpenAI)

	on, err := subscriptionsGateResolver(store, now, func() bool { return true }).Resolve(ctx, ownerToken(), inference.Request{Model: "shared-model", APIFlavor: "openai_chat"})
	if err != nil || on.Provider != ProviderVendorOpenAISubscription {
		t.Fatalf("subscriptions on: target = %#v, err = %v, want the subscription vendor target", on, err)
	}
	off, err := subscriptionsGateResolver(store, now, func() bool { return false }).Resolve(ctx, ownerToken(), inference.Request{Model: "shared-model", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("subscriptions off: Resolve = %v, want the self-hosted fall-through", err)
	}
	if off.ServerID != "srv_fast" || off.RouteID != "map_fast" {
		t.Fatalf("subscriptions off: target = %#v, want the self-hosted srv_fast/map_fast target", off)
	}
}
