// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"context"
	"errors"
	"op-ai-gateway/internal/inference"
	"reflect"
	"testing"
	"time"
)

// seedVendorSubscriptionAccount creates a SUBSCRIPTION vendor account (OAuth
// tokens, no api key) owned by vendorOwner with one model row, at the given
// status. Mirrors how the portal connect flow leaves an account: AuthType
// subscription, a sealed OAuthTokens blob, APIKey empty.
func seedVendorSubscriptionAccount(t *testing.T, store *MemoryStore, now time.Time, id, vendor, sealedTokens, status, gatewayModel, upstreamModel, flavor string) {
	t.Helper()
	ctx := context.Background()
	must(t, store.CreateVendorAccount(ctx, VendorAccount{
		ID: id, OwnerUserID: vendorOwner, Vendor: vendor, AuthType: VendorAuthSubscription,
		Name: id, Status: status, APIKey: "", OAuthTokens: sealedTokens,
		CreatedAt: now, UpdatedAt: now,
	}))
	must(t, store.SetVendorAccountModels(ctx, id, []VendorAccountModel{
		{AccountID: id, GatewayModel: gatewayModel, UpstreamModel: upstreamModel, APIFlavor: flavor},
	}))
}

// TestVendorSubscriptionAnthropicResolvesToDispatchTarget is the core M5a proof:
// an ACTIVE Anthropic subscription account resolves to a target that carries the
// account id (so dispatch resolves the bearer), the Claude-Code masquerade, the
// two OAuth headers, and NO APIToken.
func TestVendorSubscriptionAnthropicResolvesToDispatchTarget(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	seedVendorSubscriptionAccount(t, store, now, "acc_sub", VendorAnthropic, "enc:sealed-tokens", VendorAccountStatusActive, "claude-sonnet", "claude-3-7-sonnet", APIFlavorAnthropic)
	resolver := vendorResolver(store, now, true, vendorRoutingModeVendorFirst)

	// Reach it over both inbound dialects (translate both).
	for _, flavor := range []string{"anthropic_messages", "openai_chat"} {
		target, err := resolver.Resolve(ctx, ownerToken(), inference.Request{Model: "claude-sonnet", APIFlavor: flavor})
		if err != nil {
			t.Fatalf("Resolve(flavor=%q) = %v, want a subscription target", flavor, err)
		}
		if target.Provider != ProviderVendorAnthropic {
			t.Errorf("flavor=%q: Provider = %q, want %q", flavor, target.Provider, ProviderVendorAnthropic)
		}
		if target.Endpoint != "https://api.anthropic.com" {
			t.Errorf("flavor=%q: Endpoint = %q", flavor, target.Endpoint)
		}
		if target.VendorAccountID != "acc_sub" {
			t.Errorf("flavor=%q: VendorAccountID = %q, want acc_sub", flavor, target.VendorAccountID)
		}
		if target.Masquerade != MasqueradeClaudeCode {
			t.Errorf("flavor=%q: Masquerade = %q, want %q", flavor, target.Masquerade, MasqueradeClaudeCode)
		}
		if target.APIToken != "" {
			t.Errorf("flavor=%q: APIToken = %q, want empty (bearer resolved at dispatch)", flavor, target.APIToken)
		}
		if target.APITokenHeader != "" {
			t.Errorf("flavor=%q: APITokenHeader = %q, want empty", flavor, target.APITokenHeader)
		}
		if got := target.ExtraHeaders["anthropic-version"]; got != "2023-06-01" {
			t.Errorf("flavor=%q: anthropic-version = %q, want 2023-06-01", flavor, got)
		}
		if got := target.ExtraHeaders["anthropic-beta"]; got != "oauth-2025-04-20" {
			t.Errorf("flavor=%q: anthropic-beta = %q, want oauth-2025-04-20", flavor, got)
		}
		if target.ProviderModel != "claude-3-7-sonnet" {
			t.Errorf("flavor=%q: ProviderModel = %q, want claude-3-7-sonnet", flavor, target.ProviderModel)
		}
		if target.RouteID != "vendor:acc_sub:claude-sonnet" {
			t.Errorf("flavor=%q: RouteID = %q", flavor, target.RouteID)
		}
	}
}

// TestVendorSubscriptionNeedsReconnectDoesNotMatch proves a needs_reconnect
// (dead refresh token) subscription account is NOT served: it falls through to
// the standard path, which has no self-hosted route => ErrNoModelRoute.
func TestVendorSubscriptionNeedsReconnectDoesNotMatch(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	seedVendorSubscriptionAccount(t, store, now, "acc_sub", VendorAnthropic, "enc:sealed-tokens", VendorAccountStatusNeedsReconnect, "claude-sonnet", "claude-3-7-sonnet", APIFlavorAnthropic)
	resolver := vendorResolver(store, now, true, vendorRoutingModeVendorFirst)

	if _, err := resolver.Resolve(ctx, ownerToken(), inference.Request{Model: "claude-sonnet", APIFlavor: "anthropic_messages"}); !errors.Is(err, ErrNoModelRoute) {
		t.Fatalf("Resolve(needs_reconnect) = %v, want ErrNoModelRoute (account must not serve)", err)
	}
}

// TestVendorSubscriptionOpenAIDoesNotMatch proves an OpenAI subscription account
// is deliberately NOT served in M5a (OpenAI subscription serving is Milestone
// 5b): it falls through to ErrNoModelRoute rather than producing a target.
func TestVendorSubscriptionOpenAIDoesNotMatch(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	seedVendorSubscriptionAccount(t, store, now, "acc_sub_oai", VendorOpenAI, "enc:sealed-tokens", VendorAccountStatusActive, "gpt-5-codex", "gpt-5-codex", APIFlavorOpenAI)
	resolver := vendorResolver(store, now, true, vendorRoutingModeVendorFirst)

	if _, err := resolver.Resolve(ctx, ownerToken(), inference.Request{Model: "gpt-5-codex", APIFlavor: "openai_chat"}); !errors.Is(err, ErrNoModelRoute) {
		t.Fatalf("Resolve(openai subscription) = %v, want ErrNoModelRoute (OpenAI subscription is Milestone 5b)", err)
	}
}

// vendorSubscriptionTargetMayBeZero names the fields the subscription Anthropic
// target legitimately leaves zero (the subscription analogue of
// vendorTargetMayBeZero). Crucially ExtraHeaders/Masquerade/VendorAccountID are
// NOT listed — a subscription target that forgot any of them is a bug.
var vendorSubscriptionTargetMayBeZero = map[string]bool{
	"ServerID":                    true, // no on-prem server
	"APIToken":                    true, // bearer resolved at dispatch, not carried here
	"APITokenHeader":              true, // Authorization is set by the dispatch layer, not a static header
	"ResponsesMode":               true, // zero == translate
	"MessagesMode":                true, // zero == translate
	"OpportunisticMetrics":        true, // no per-app toggle for a vendor
	"ResponsesLiveTimingsEnabled": true, // llama.cpp-only; N/A
	"LiveProgressSupport":         true, // mapping-persisted; a vendor carries none
	"LiveProgressSpecType":        true, // server_agent-only; N/A
}

// TestVendorSubscriptionTargetCompleteness reflects over the built subscription
// target and requires every field not named above to be non-zero, so a new
// Target field a subscription target should carry cannot be silently forgotten.
func TestVendorSubscriptionTargetCompleteness(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	acc := VendorAccount{ID: "acc_sub", OwnerUserID: vendorOwner, Vendor: VendorAnthropic, AuthType: VendorAuthSubscription, Status: VendorAccountStatusActive, OAuthTokens: "enc:t", CreatedAt: now, UpdatedAt: now}
	m := VendorAccountModel{AccountID: "acc_sub", GatewayModel: "claude-sonnet", UpstreamModel: "claude-3-7-sonnet", APIFlavor: APIFlavorAnthropic}
	target := vendorSubscriptionAnthropicTarget(acc, m, "claude-sonnet", APIFlavorAnthropic)

	tv := reflect.ValueOf(target)
	tt := tv.Type()
	for i := 0; i < tt.NumField(); i++ {
		name := tt.Field(i).Name
		if vendorSubscriptionTargetMayBeZero[name] {
			continue
		}
		if tv.Field(i).IsZero() {
			t.Fatalf("vendorSubscriptionAnthropicTarget left Target.%s at its zero value. If a subscription target should carry it, wire it in vendorSubscriptionAnthropicTarget (internal/routing/resolver.go); if legitimately zero, add %q to vendorSubscriptionTargetMayBeZero with a reason.", name, name)
		}
	}
}
