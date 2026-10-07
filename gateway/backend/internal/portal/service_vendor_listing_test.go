// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"slices"
	"testing"
	"time"
)

// modelDTONamed returns the Models() row for name, or a zero DTO and false.
func modelDTONamed(data []ModelDTO, name string) (ModelDTO, bool) {
	for _, row := range data {
		if row.ID == name {
			return row, true
		}
	}
	return ModelDTO{}, false
}

// TestVendorModelsAppearInOwnerListings proves an owner's own active vendor
// account surfaces its catalog models in every owner-facing listing — the chat
// picker (Models()), /v1/models (ModelsForFlavor openai) and the Anthropic list
// (ModelsForFlavor anthropic) — under BOTH coarse flavors, matching what
// resolveVendorAccount dispatches (translate serves either inbound dialect).
func TestVendorModelsAppearInOwnerListings(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	svc, _ := newVendorAccountTestService(t, now)
	ctx := context.Background()
	owner := ownerToken()
	createTestVendorAccount(t, svc, owner, apiKeyAccountRequest("My OpenAI"))

	// A catalog model (openAIModels[0]) is listed in the picker under both flavors.
	const vendorModel = "gpt-5"
	row, ok := modelDTONamed(svc.Models(ctx, owner).Data, vendorModel)
	if !ok {
		t.Fatalf("Models() has no row for the vendor model %q", vendorModel)
	}
	if row.Visibility != "shown" {
		t.Errorf("vendor model Visibility = %q, want shown", row.Visibility)
	}
	for _, f := range []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic} {
		if !slices.Contains(row.Flavors, f) {
			t.Errorf("vendor model Flavors = %q, want it to include %q (dispatch serves both via translate)", row.Flavors, f)
		}
	}

	if got := svc.ModelsForFlavor(ctx, owner, routing.APIFlavorOpenAI); !slices.Contains(got, vendorModel) {
		t.Errorf("/v1/models (openai) = %q, want it to include %q", got, vendorModel)
	}
	if got := svc.ModelsForFlavor(ctx, owner, routing.APIFlavorAnthropic); !slices.Contains(got, vendorModel) {
		t.Errorf("/anthropic/v1/models = %q, want it to include %q (served via translate)", got, vendorModel)
	}
}

// TestVendorModelsHiddenFromNonOwner proves the overlay is owner-scoped: a
// different user never sees another user's vendor models, and neither does the
// admin management surface (ManageModels), which shows the system's real models.
func TestVendorModelsHiddenFromNonOwner(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	svc, _ := newVendorAccountTestService(t, now)
	ctx := context.Background()
	owner := ownerToken()
	createTestVendorAccount(t, svc, owner, apiKeyAccountRequest("My OpenAI"))

	const vendorModel = "gpt-5"
	other := auth.Token{UserID: "usr_other", Scopes: []string{"gateway:use"}}
	if _, ok := modelDTONamed(svc.Models(ctx, other).Data, vendorModel); ok {
		t.Errorf("Models() for a non-owner lists %q; vendor models must be owner-scoped", vendorModel)
	}
	if got := svc.ModelsForFlavor(ctx, other, routing.APIFlavorOpenAI); slices.Contains(got, vendorModel) {
		t.Errorf("/v1/models for a non-owner = %q, must not include %q", got, vendorModel)
	}
	// ManageModels (admin surface) must not carry the OWNER's personal vendor
	// models either.
	if _, ok := modelDTONamed(svc.ManageModels(ctx, owner).Data, vendorModel); ok {
		t.Errorf("ManageModels() lists the vendor model %q; the admin surface must stay the system's real models", vendorModel)
	}
}

// TestVendorModelsHiddenWhenFlagOff proves the master flag gates the overlay: with
// vendor_accounts_enabled off, the owner's own vendor models vanish from every
// listing (the no-op invariant that keeps served_flavors parity green by default).
func TestVendorModelsHiddenWhenFlagOff(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	svc, _ := newVendorAccountTestService(t, now)
	ctx := context.Background()
	owner := ownerToken()
	createTestVendorAccount(t, svc, owner, apiKeyAccountRequest("My OpenAI"))

	setVendorAccountsEnabled(t, svc, false)

	const vendorModel = "gpt-5"
	if _, ok := modelDTONamed(svc.Models(ctx, owner).Data, vendorModel); ok {
		t.Errorf("Models() lists %q with the master flag off; the overlay must be gated", vendorModel)
	}
	if got := svc.ModelsForFlavor(ctx, owner, routing.APIFlavorOpenAI); slices.Contains(got, vendorModel) {
		t.Errorf("/v1/models = %q with the flag off, must not include %q", got, vendorModel)
	}
}

// TestVendorModelsAnthropicAccountListings proves a native-Anthropic account's
// catalog models surface too (under both flavors, since dispatch translates).
func TestVendorModelsAnthropicAccountListings(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	svc, _ := newVendorAccountTestService(t, now)
	ctx := context.Background()
	owner := ownerToken()
	createTestVendorAccount(t, svc, owner, CreateVendorAccountRequest{
		Vendor:   routing.VendorAnthropic,
		AuthType: routing.VendorAuthAPIKey,
		Name:     "My Anthropic",
		APIKey:   vendorAccountTestKey,
	})

	const vendorModel = "claude-sonnet-5-5"
	if got := svc.ModelsForFlavor(ctx, owner, routing.APIFlavorAnthropic); !slices.Contains(got, vendorModel) {
		t.Errorf("/anthropic/v1/models = %q, want it to include %q", got, vendorModel)
	}
	if got := svc.ModelsForFlavor(ctx, owner, routing.APIFlavorOpenAI); !slices.Contains(got, vendorModel) {
		t.Errorf("/v1/models (openai) = %q, want it to include %q (served via translate)", got, vendorModel)
	}
}
