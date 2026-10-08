// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"op-ai-gateway/internal/routing"
	"slices"
	"testing"
)

// vendorAuthTypes are the account auth types a catalog is asked for.
var vendorAuthTypes = []string{routing.VendorAuthSubscription, routing.VendorAuthAPIKey}

// TestVendorCatalogShape pins the invariants the resolver relies on, not the
// exact ids (those are reverse-engineered and get refreshed as vendors change):
// a small non-empty set per vendor and auth type, every row in that vendor's own
// wire flavor, no empty or duplicate gateway ids, and no AccountID baked into
// the template.
func TestVendorCatalogShape(t *testing.T) {
	wantFlavor := map[string]string{
		routing.VendorOpenAI:    routing.APIFlavorOpenAI,
		routing.VendorAnthropic: routing.APIFlavorAnthropic,
	}
	for vendor, flavor := range wantFlavor {
		for _, authType := range vendorAuthTypes {
			catalog := VendorCatalog(vendor, authType)
			if n := len(catalog); n < 2 || n > 4 {
				t.Errorf("%s/%s catalog has %d entries, want a small curated set of 2-4", vendor, authType, n)
			}
			seen := map[string]bool{}
			for _, m := range catalog {
				if m.GatewayModel == "" || m.UpstreamModel == "" {
					t.Errorf("%s/%s catalog entry %+v has an empty model id", vendor, authType, m)
				}
				if m.GatewayModel != m.UpstreamModel {
					t.Errorf("%s/%s catalog entry %+v diverges gateway from upstream id, want a transparent pass-through", vendor, authType, m)
				}
				if m.APIFlavor != flavor {
					t.Errorf("%s/%s catalog entry %q flavor = %q, want %q", vendor, authType, m.GatewayModel, m.APIFlavor, flavor)
				}
				if m.AccountID != "" {
					t.Errorf("%s/%s catalog entry %q carries AccountID %q, want none (set per account on seed)", vendor, authType, m.GatewayModel, m.AccountID)
				}
				if seen[m.GatewayModel] {
					t.Errorf("%s/%s catalog repeats gateway model %q", vendor, authType, m.GatewayModel)
				}
				seen[m.GatewayModel] = true
			}
		}
	}
}

func catalogGatewayModels(catalog []routing.VendorAccountModel) []string {
	ids := make([]string, 0, len(catalog))
	for _, m := range catalog {
		ids = append(ids, m.GatewayModel)
	}
	return ids
}

// The Codex ChatGPT backend that serves an OpenAI SUBSCRIPTION account does not
// serve gpt-4.1 or o3: seeding them would guarantee a model error on an account
// whose credential is fine, which is exactly the auth-vs-model confusion the
// credential validation exists to remove. api.openai.com (the api_key path)
// serves the full set. This pins both sets by id, deliberately: the split IS the
// behaviour.
func TestVendorCatalogOpenAISplitsByAuthType(t *testing.T) {
	for _, tc := range []struct {
		authType string
		want     []string
	}{
		{routing.VendorAuthSubscription, []string{"gpt-5", "gpt-5-mini"}},
		{routing.VendorAuthAPIKey, []string{"gpt-5", "gpt-5-mini", "gpt-4.1", "o3"}},
	} {
		t.Run(tc.authType, func(t *testing.T) {
			got := catalogGatewayModels(VendorCatalog(routing.VendorOpenAI, tc.authType))
			if !slices.Equal(got, tc.want) {
				t.Fatalf("OpenAI %s catalog = %q, want %q", tc.authType, got, tc.want)
			}
		})
	}
}

// Anthropic's OAuth Messages path and its api key serve the same model ids, so
// the auth type does not narrow its catalog.
func TestVendorCatalogAnthropicIsTheSameForBothAuthTypes(t *testing.T) {
	want := []string{"claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-4-5"}
	for _, authType := range vendorAuthTypes {
		got := catalogGatewayModels(VendorCatalog(routing.VendorAnthropic, authType))
		if !slices.Equal(got, want) {
			t.Errorf("Anthropic %s catalog = %q, want %q", authType, got, want)
		}
	}
}

func TestVendorCatalogUnknownVendorIsEmpty(t *testing.T) {
	for _, vendor := range []string{"", "mistral", "OpenAI"} {
		for _, authType := range vendorAuthTypes {
			if got := VendorCatalog(vendor, authType); len(got) != 0 {
				t.Errorf("VendorCatalog(%q, %q) = %+v, want empty", vendor, authType, got)
			}
		}
	}
}

// An auth type the gateway does not know seeds nothing, for every vendor: there
// is no honest model set to guess for it.
func TestVendorCatalogUnknownAuthTypeIsEmpty(t *testing.T) {
	for _, vendor := range []string{routing.VendorOpenAI, routing.VendorAnthropic} {
		for _, authType := range []string{"", "oauth", "API_KEY"} {
			if got := VendorCatalog(vendor, authType); len(got) != 0 {
				t.Errorf("VendorCatalog(%q, %q) = %+v, want empty", vendor, authType, got)
			}
		}
	}
}

// The catalog is a package-level template; a caller that edits the returned
// slice must not change what the next account is seeded with.
func TestVendorCatalogReturnsAFreshSlice(t *testing.T) {
	for _, authType := range vendorAuthTypes {
		first := VendorCatalog(routing.VendorOpenAI, authType)
		want := first[0]
		first[0].GatewayModel = "scribbled"
		if got := VendorCatalog(routing.VendorOpenAI, authType)[0]; got != want {
			t.Fatalf("%s catalog mutated through a returned slice: got %+v, want %+v", authType, got, want)
		}
	}
}
