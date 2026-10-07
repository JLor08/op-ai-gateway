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

// seedActiveSubscriptionAccount seeds an ACTIVE subscription vendor account owned
// by the principal directly into the route store (bypassing the connect flow) with
// one model row, so a listing test can assert whether that model is advertised.
func seedActiveSubscriptionAccount(t *testing.T, routeStore *routing.MemoryStore, now time.Time, owner auth.Token, id, vendor, gatewayModel, flavor string) {
	t.Helper()
	ctx := context.Background()
	if err := routeStore.CreateVendorAccount(ctx, routing.VendorAccount{
		ID: id, OwnerUserID: owner.UserID, Vendor: vendor, AuthType: routing.VendorAuthSubscription,
		Name: id, Status: routing.VendorAccountStatusActive, OAuthTokens: "enc:sealed", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateVendorAccount: %v", err)
	}
	if err := routeStore.SetVendorAccountModels(ctx, id, []routing.VendorAccountModel{
		{AccountID: id, GatewayModel: gatewayModel, UpstreamModel: gatewayModel, APIFlavor: flavor},
	}); err != nil {
		t.Fatalf("SetVendorAccountModels: %v", err)
	}
}

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

// TestVendorModelsOpenAISubscriptionHiddenUntilM5c proves an ACTIVE OpenAI
// SUBSCRIPTION account's models are NOT advertised in any owner listing (it serves
// only /v1/responses via native passthrough in M5b; chat/completions is M5c, so
// advertising would break served_flavors parity). An active OpenAI API-KEY account
// in the same listing stays visible, proving the skip is specific to the
// subscription auth type and not to the OpenAI vendor.
func TestVendorModelsOpenAISubscriptionHiddenUntilM5c(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newVendorAccountTestService(t, now)
	ctx := context.Background()
	owner := ownerToken()

	// An api_key OpenAI account (its catalog models must stay visible).
	createTestVendorAccount(t, svc, owner, apiKeyAccountRequest("My OpenAI"))
	// An ACTIVE OpenAI subscription account serving a distinct model (must be hidden).
	const subModel = "gpt-5-codex"
	seedActiveSubscriptionAccount(t, routeStore, now, owner, "acc_sub_oai", routing.VendorOpenAI, subModel, routing.APIFlavorOpenAI)

	// The subscription model is absent from every owner listing.
	if _, ok := modelDTONamed(svc.Models(ctx, owner).Data, subModel); ok {
		t.Errorf("Models() lists the OpenAI subscription model %q; it must be hidden until M5c", subModel)
	}
	if got := svc.ModelsForFlavor(ctx, owner, routing.APIFlavorOpenAI); slices.Contains(got, subModel) {
		t.Errorf("/v1/models (openai) = %q, must not include the subscription model %q", got, subModel)
	}
	if got := svc.ModelsForFlavor(ctx, owner, routing.APIFlavorAnthropic); slices.Contains(got, subModel) {
		t.Errorf("/anthropic/v1/models = %q, must not include the subscription model %q", got, subModel)
	}

	// The api_key account's catalog model is still advertised (skip is specific).
	if _, ok := modelDTONamed(svc.Models(ctx, owner).Data, "gpt-5"); !ok {
		t.Error("Models() dropped the api_key OpenAI model gpt-5; only subscription accounts must be skipped")
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
