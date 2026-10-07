// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"op-ai-gateway/internal/routing"
	"testing"
)

// TestVendorCatalogShape pins the invariants the resolver relies on, not the
// exact ids (those are reverse-engineered and get refreshed as vendors change):
// a small non-empty set per vendor, every row in that vendor's own wire flavor,
// no empty or duplicate gateway ids, and no AccountID baked into the template.
func TestVendorCatalogShape(t *testing.T) {
	wantFlavor := map[string]string{
		routing.VendorOpenAI:    routing.APIFlavorOpenAI,
		routing.VendorAnthropic: routing.APIFlavorAnthropic,
	}
	for vendor, flavor := range wantFlavor {
		catalog := VendorCatalog(vendor)
		if n := len(catalog); n < 2 || n > 4 {
			t.Errorf("%s catalog has %d entries, want a small curated set of 2-4", vendor, n)
		}
		seen := map[string]bool{}
		for _, m := range catalog {
			if m.GatewayModel == "" || m.UpstreamModel == "" {
				t.Errorf("%s catalog entry %+v has an empty model id", vendor, m)
			}
			if m.APIFlavor != flavor {
				t.Errorf("%s catalog entry %q flavor = %q, want %q", vendor, m.GatewayModel, m.APIFlavor, flavor)
			}
			if m.AccountID != "" {
				t.Errorf("%s catalog entry %q carries AccountID %q, want none (set per account on seed)", vendor, m.GatewayModel, m.AccountID)
			}
			if seen[m.GatewayModel] {
				t.Errorf("%s catalog repeats gateway model %q", vendor, m.GatewayModel)
			}
			seen[m.GatewayModel] = true
		}
	}
}

func TestVendorCatalogUnknownVendorIsEmpty(t *testing.T) {
	for _, vendor := range []string{"", "mistral", "OpenAI"} {
		if got := VendorCatalog(vendor); len(got) != 0 {
			t.Errorf("VendorCatalog(%q) = %+v, want empty", vendor, got)
		}
	}
}

// The catalog is a package-level template; a caller that edits the returned
// slice must not change what the next account is seeded with.
func TestVendorCatalogReturnsAFreshSlice(t *testing.T) {
	first := VendorCatalog(routing.VendorOpenAI)
	want := first[0]
	first[0].GatewayModel = "scribbled"
	if got := VendorCatalog(routing.VendorOpenAI)[0]; got != want {
		t.Fatalf("catalog mutated through a returned slice: got %+v, want %+v", got, want)
	}
}
