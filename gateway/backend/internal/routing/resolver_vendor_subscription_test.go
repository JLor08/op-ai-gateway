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

// TestVendorSubscriptionOpenAIResolvesToPassthroughTarget is the core M5b proof:
// an ACTIVE OpenAI subscription account, reached over the FINE openai_responses
// flavor, resolves to a NATIVE-PASSTHROUGH target pointed at the ChatGPT backend
// (ResponsesMode passthrough, the account id for the dispatch bearer, the two
// static Codex headers, [openai] flavors only, and NO APIToken/Masquerade).
func TestVendorSubscriptionOpenAIResolvesToPassthroughTarget(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	seedVendorSubscriptionAccount(t, store, now, "acc_sub_oai", VendorOpenAI, "enc:sealed-tokens", VendorAccountStatusActive, "gpt-5-codex", "gpt-5-codex-upstream", APIFlavorOpenAI)
	resolver := vendorResolver(store, now, true, vendorRoutingModeVendorFirst)

	target, err := resolver.Resolve(ctx, ownerToken(), inference.Request{Model: "gpt-5-codex", APIFlavor: "openai_responses"})
	if err != nil {
		t.Fatalf("Resolve(openai_responses) = %v, want a passthrough target", err)
	}
	if target.Provider != ProviderVendorOpenAI {
		t.Errorf("Provider = %q, want %q", target.Provider, ProviderVendorOpenAI)
	}
	if target.Endpoint != "https://chatgpt.com/backend-api/codex" {
		t.Errorf("Endpoint = %q, want the ChatGPT backend", target.Endpoint)
	}
	if target.ResponsesMode != EndpointModePassthrough {
		t.Errorf("ResponsesMode = %q, want passthrough", target.ResponsesMode)
	}
	if target.VendorAccountID != "acc_sub_oai" {
		t.Errorf("VendorAccountID = %q, want acc_sub_oai", target.VendorAccountID)
	}
	if target.ProviderModel != "gpt-5-codex-upstream" {
		t.Errorf("ProviderModel = %q, want gpt-5-codex-upstream", target.ProviderModel)
	}
	if target.APIToken != "" || target.APITokenHeader != "" {
		t.Errorf("APIToken/APITokenHeader = %q/%q, want empty (bearer resolved at dispatch)", target.APIToken, target.APITokenHeader)
	}
	if target.Masquerade != "" {
		t.Errorf("Masquerade = %q, want empty (the ChatGPT backend wants the real Codex body)", target.Masquerade)
	}
	if got := target.ExtraHeaders["OpenAI-Beta"]; got != "responses=experimental" {
		t.Errorf("OpenAI-Beta = %q, want responses=experimental", got)
	}
	if got := target.ExtraHeaders["originator"]; got != "codex_cli_rs" {
		t.Errorf("originator = %q, want codex_cli_rs", got)
	}
	if _, present := target.ExtraHeaders["chatgpt-account-id"]; present {
		t.Errorf("chatgpt-account-id must NOT be a static header; it is resolved per-account at dispatch, got %q", target.ExtraHeaders["chatgpt-account-id"])
	}
	if len(target.APIFlavors) != 1 || target.APIFlavors[0] != APIFlavorOpenAI {
		t.Errorf("APIFlavors = %q, want [openai] only (chat/completions is M5c)", target.APIFlavors)
	}
}

// TestVendorSubscriptionOpenAIChatFlavorDoesNotMatch proves a chat-flavor request
// to an ACTIVE OpenAI subscription account does NOT match (portal chat/completions
// support is Milestone 5c): it falls through to ErrNoModelRoute rather than
// producing a target that chat/completions would then 404 on.
func TestVendorSubscriptionOpenAIChatFlavorDoesNotMatch(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	seedVendorSubscriptionAccount(t, store, now, "acc_sub_oai", VendorOpenAI, "enc:sealed-tokens", VendorAccountStatusActive, "gpt-5-codex", "gpt-5-codex", APIFlavorOpenAI)
	resolver := vendorResolver(store, now, true, vendorRoutingModeVendorFirst)

	if _, err := resolver.Resolve(ctx, ownerToken(), inference.Request{Model: "gpt-5-codex", APIFlavor: "openai_chat"}); !errors.Is(err, ErrNoModelRoute) {
		t.Fatalf("Resolve(openai chat) = %v, want ErrNoModelRoute (chat/completions is Milestone 5c)", err)
	}
}

// TestVendorSubscriptionOpenAINeedsReconnectDoesNotMatch proves a needs_reconnect
// OpenAI subscription account is not served even over openai_responses.
func TestVendorSubscriptionOpenAINeedsReconnectDoesNotMatch(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	seedVendorSubscriptionAccount(t, store, now, "acc_sub_oai", VendorOpenAI, "enc:sealed-tokens", VendorAccountStatusNeedsReconnect, "gpt-5-codex", "gpt-5-codex", APIFlavorOpenAI)
	resolver := vendorResolver(store, now, true, vendorRoutingModeVendorFirst)

	if _, err := resolver.Resolve(ctx, ownerToken(), inference.Request{Model: "gpt-5-codex", APIFlavor: "openai_responses"}); !errors.Is(err, ErrNoModelRoute) {
		t.Fatalf("Resolve(needs_reconnect) = %v, want ErrNoModelRoute (account must not serve)", err)
	}
}

// vendorSubscriptionTargetMayBeZero names the fields the subscription ANTHROPIC
// target legitimately leaves zero (the subscription analogue of
// vendorTargetMayBeZero). Crucially ExtraHeaders/Masquerade/VendorAccountID are
// NOT listed — an Anthropic subscription target that forgot any of them is a bug.
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

// vendorSubscriptionOpenAITargetMayBeZero is the OpenAI analogue. It differs from
// the Anthropic set in exactly the two places the two targets differ in shape:
// ResponsesMode is non-zero (passthrough — the whole point), so it is NOT listed;
// Masquerade IS zero-OK (the ChatGPT backend wants the real Codex body, no system
// block). ExtraHeaders/VendorAccountID stay required.
var vendorSubscriptionOpenAITargetMayBeZero = map[string]bool{
	"ServerID":                    true, // no on-prem server
	"APIToken":                    true, // bearer resolved at dispatch, not carried here
	"APITokenHeader":              true, // Authorization is set by the dispatch layer, not a static header
	"MessagesMode":                true, // openai_responses only; no messages path
	"Masquerade":                  true, // native passthrough of the real Codex body; no disguise
	"OpportunisticMetrics":        true, // no per-app toggle for a vendor
	"ResponsesLiveTimingsEnabled": true, // llama.cpp-only; N/A
	"LiveProgressSupport":         true, // mapping-persisted; a vendor carries none
	"LiveProgressSpecType":        true, // server_agent-only; N/A
}

// TestVendorSubscriptionTargetCompleteness reflects over each built subscription
// target and requires every field not named in its may-be-zero set to be
// non-zero, so a new Target field a subscription target should carry cannot be
// silently forgotten.
func TestVendorSubscriptionTargetCompleteness(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	anthropicAcc := VendorAccount{ID: "acc_sub", OwnerUserID: vendorOwner, Vendor: VendorAnthropic, AuthType: VendorAuthSubscription, Status: VendorAccountStatusActive, OAuthTokens: "enc:t", CreatedAt: now, UpdatedAt: now}
	anthropicModel := VendorAccountModel{AccountID: "acc_sub", GatewayModel: "claude-sonnet", UpstreamModel: "claude-3-7-sonnet", APIFlavor: APIFlavorAnthropic}
	openAIAcc := VendorAccount{ID: "acc_sub_oai", OwnerUserID: vendorOwner, Vendor: VendorOpenAI, AuthType: VendorAuthSubscription, Status: VendorAccountStatusActive, OAuthTokens: "enc:t", CreatedAt: now, UpdatedAt: now}
	openAIModel := VendorAccountModel{AccountID: "acc_sub_oai", GatewayModel: "gpt-5-codex", UpstreamModel: "gpt-5-codex-upstream", APIFlavor: APIFlavorOpenAI}

	cases := []struct {
		name      string
		fn        string
		target    Target
		mayBeZero map[string]bool
	}{
		{"anthropic", "vendorSubscriptionAnthropicTarget", vendorSubscriptionAnthropicTarget(anthropicAcc, anthropicModel, "claude-sonnet", APIFlavorAnthropic), vendorSubscriptionTargetMayBeZero},
		{"openai", "vendorSubscriptionOpenAITarget", vendorSubscriptionOpenAITarget(openAIAcc, openAIModel, "gpt-5-codex", APIFlavorOpenAI), vendorSubscriptionOpenAITargetMayBeZero},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tv := reflect.ValueOf(tc.target)
			tt := tv.Type()
			for i := 0; i < tt.NumField(); i++ {
				name := tt.Field(i).Name
				if tc.mayBeZero[name] {
					continue
				}
				if tv.Field(i).IsZero() {
					t.Fatalf("%s left Target.%s at its zero value. If a subscription target should carry it, wire it in %s (internal/routing/resolver.go); if legitimately zero, add %q to the %s may-be-zero set with a reason.", tc.fn, name, tc.fn, name, tc.name)
				}
			}
		})
	}
}
