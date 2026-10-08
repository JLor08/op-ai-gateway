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

// seedPrefixedVendorAccount creates an ACTIVE vendor account (api_key or
// subscription) with ModelPrefix prefix and ONE model row exactly as discovery
// seeds it: GatewayModel = prefix + slug (what a caller asks the gateway for),
// UpstreamModel = slug (what the vendor is sent). The resolver reads only the two
// model columns, but the account carries the prefix too so the fixture is the
// real stored shape.
func seedPrefixedVendorAccount(t *testing.T, store *MemoryStore, now time.Time, id, vendor, authType, prefix, slug, flavor string) {
	t.Helper()
	ctx := context.Background()
	acc := VendorAccount{
		ID: id, OwnerUserID: vendorOwner, Vendor: vendor, AuthType: authType,
		Name: id, Status: VendorAccountStatusActive, ModelPrefix: prefix,
		CreatedAt: now, UpdatedAt: now,
	}
	if authType == VendorAuthSubscription {
		acc.OAuthTokens = "enc:sealed-tokens"
	} else {
		acc.APIKey = vendorKey
	}
	must(t, store.CreateVendorAccount(ctx, acc))
	must(t, store.SetVendorAccountModels(ctx, id, []VendorAccountModel{
		{AccountID: id, GatewayModel: prefix + slug, UpstreamModel: slug, APIFlavor: flavor, DisplayName: slug},
	}))
}

// TestVendorAccountPrefixedModelResolvesAndDispatchesRawSlug is the Task 6 proof:
// a vendor account served under a model PREFIX is matched by the PREFIXED name
// (Target.Model is what the caller asked for, so usage and the RouteID name the
// public id) while the vendor is sent the RAW slug (Target.ProviderModel). The
// bare slug is NOT a name the gateway serves any more. Every vendor target kind
// builds its Target the same way, so each is covered, plus the unprefixed account
// (GatewayModel == UpstreamModel) as the regression guard.
func TestVendorAccountPrefixedModelResolvesAndDispatchesRawSlug(t *testing.T) {
	const slug = "gpt-6-luna"
	cases := []struct {
		name         string
		vendor       string
		authType     string
		prefix       string
		modelFlavor  string // the model row's coarse flavor
		requestFlavs []string
		wantProvider string
	}{
		{"openai api_key", VendorOpenAI, VendorAuthAPIKey, "work/", APIFlavorOpenAI, []string{"openai_chat", "openai_responses"}, ProviderVendorOpenAI},
		{"anthropic api_key", VendorAnthropic, VendorAuthAPIKey, "work/", APIFlavorAnthropic, []string{"anthropic_messages", "openai_chat"}, ProviderVendorAnthropic},
		{"openai subscription", VendorOpenAI, VendorAuthSubscription, "chatgpt/", APIFlavorOpenAI, []string{"openai_chat", "openai_responses"}, ProviderVendorOpenAISubscription},
		{"anthropic subscription", VendorAnthropic, VendorAuthSubscription, "claude/", APIFlavorAnthropic, []string{"anthropic_messages", "openai_chat"}, ProviderVendorAnthropic},
		{"openai api_key, no prefix", VendorOpenAI, VendorAuthAPIKey, "", APIFlavorOpenAI, []string{"openai_chat"}, ProviderVendorOpenAI},
		{"openai subscription, no prefix", VendorOpenAI, VendorAuthSubscription, "", APIFlavorOpenAI, []string{"openai_responses"}, ProviderVendorOpenAISubscription},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
			store := NewMemoryStore()
			seedPrefixedVendorAccount(t, store, now, "acc_prefixed", tc.vendor, tc.authType, tc.prefix, slug, tc.modelFlavor)
			resolver := vendorResolver(store, now, true, vendorRoutingModeVendorFirst)

			for _, flavor := range tc.requestFlavs {
				target, err := resolver.Resolve(ctx, ownerToken(), inference.Request{Model: tc.prefix + slug, APIFlavor: flavor})
				if err != nil {
					t.Fatalf("flavor=%q: Resolve(%q) = %v, want a vendor target", flavor, tc.prefix+slug, err)
				}
				if target.Provider != tc.wantProvider {
					t.Errorf("flavor=%q: Provider = %q, want %q", flavor, target.Provider, tc.wantProvider)
				}
				if target.Model != tc.prefix+slug {
					t.Errorf("flavor=%q: Model = %q, want %q (the public, prefixed id)", flavor, target.Model, tc.prefix+slug)
				}
				if target.ProviderModel != slug {
					t.Errorf("flavor=%q: ProviderModel = %q, want the raw vendor slug %q (what is sent upstream)", flavor, target.ProviderModel, slug)
				}
				if target.VendorAccountID != "acc_prefixed" {
					t.Errorf("flavor=%q: VendorAccountID = %q, want acc_prefixed", flavor, target.VendorAccountID)
				}
				if wantSub := tc.authType == VendorAuthSubscription; target.Subscription != wantSub {
					t.Errorf("flavor=%q: Subscription = %v, want %v", flavor, target.Subscription, wantSub)
				}
			}

			if tc.prefix == "" {
				return // the bare slug IS the public id; nothing to refuse
			}
			// The bare slug is no longer a name this account serves: it is
			// chatgpt/gpt-6-luna now. It falls through to the standard path, which has
			// no self-hosted route, so ErrNoModelRoute (not a vendor target).
			for _, flavor := range tc.requestFlavs {
				if got, err := resolver.Resolve(ctx, ownerToken(), inference.Request{Model: slug, APIFlavor: flavor}); !errors.Is(err, ErrNoModelRoute) {
					t.Errorf("flavor=%q: Resolve(raw %q) = %+v, %v; want ErrNoModelRoute (only %q is served)", flavor, slug, got, err, tc.prefix+slug)
				}
			}
		})
	}
}

// TestVendorAccountPrefixDisambiguatesSameSlugAcrossAccounts is the reason the
// prefix exists: two of the owner's accounts serve the SAME vendor slug, and each
// public name routes to its own account (and its own credential), while the bare
// slug is served by neither.
func TestVendorAccountPrefixDisambiguatesSameSlugAcrossAccounts(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	const slug = "gpt-6-luna"
	seedPrefixedVendorAccount(t, store, now, "acc_a", VendorOpenAI, VendorAuthAPIKey, "team-a/", slug, APIFlavorOpenAI)
	seedPrefixedVendorAccount(t, store, now, "acc_b", VendorOpenAI, VendorAuthSubscription, "chatgpt/", slug, APIFlavorOpenAI)
	resolver := vendorResolver(store, now, true, vendorRoutingModeVendorFirst)

	for _, tc := range []struct{ model, wantAccount, wantProvider string }{
		{"team-a/" + slug, "acc_a", ProviderVendorOpenAI},
		{"chatgpt/" + slug, "acc_b", ProviderVendorOpenAISubscription},
	} {
		target, err := resolver.Resolve(ctx, ownerToken(), inference.Request{Model: tc.model, APIFlavor: "openai_chat"})
		if err != nil {
			t.Fatalf("Resolve(%q) = %v, want a vendor target", tc.model, err)
		}
		if target.VendorAccountID != tc.wantAccount || target.Provider != tc.wantProvider {
			t.Errorf("Resolve(%q) -> account %q provider %q, want %q / %q", tc.model, target.VendorAccountID, target.Provider, tc.wantAccount, tc.wantProvider)
		}
		if target.ProviderModel != slug {
			t.Errorf("Resolve(%q): ProviderModel = %q, want the raw slug %q", tc.model, target.ProviderModel, slug)
		}
	}
	if _, err := resolver.Resolve(ctx, ownerToken(), inference.Request{Model: slug, APIFlavor: "openai_chat"}); !errors.Is(err, ErrNoModelRoute) {
		t.Fatalf("Resolve(raw %q) = %v, want ErrNoModelRoute (both accounts serve it only under their prefix)", slug, err)
	}
}
