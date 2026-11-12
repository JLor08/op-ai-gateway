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

const vendorDecisionSlug = "gpt-6-luna"

// seedVendorDecisionAccount seeds an active account of the given vendor and auth
// type owned by ownerToken(), served under prefix, with one row per slug.
func seedVendorDecisionAccount(t *testing.T, routeStore *routing.MemoryStore, now time.Time, id, vendor, authType, prefix string, slugs ...string) {
	t.Helper()
	ctx := context.Background()
	acc := routing.VendorAccount{
		ID: id, OwnerUserID: ownerToken().UserID, Vendor: vendor, AuthType: authType,
		Name: id, Status: routing.VendorAccountStatusActive, ModelPrefix: prefix, CreatedAt: now, UpdatedAt: now,
	}
	if authType == routing.VendorAuthSubscription {
		acc.OAuthTokens = "enc:sealed"
	} else {
		acc.APIKey = "enc:key"
	}
	if routing.IsPresetVendor(vendor) {
		acc.BaseURL = "https://compat.example.test"
	}
	if err := routeStore.CreateVendorAccount(ctx, acc); err != nil {
		t.Fatalf("CreateVendorAccount: %v", err)
	}
	rows := make([]routing.VendorAccountModel, 0, len(slugs))
	for _, slug := range slugs {
		rows = append(rows, routing.VendorAccountModel{AccountID: id, GatewayModel: prefix + slug, UpstreamModel: slug, APIFlavor: routing.APIFlavorOpenAI})
	}
	if err := routeStore.SetVendorAccountModels(ctx, id, rows); err != nil {
		t.Fatalf("SetVendorAccountModels: %v", err)
	}
}

// The decision model of an OpenAI api-key account is listed with its chat
// flavors plus the decisions flavor and both decision flags (passthrough to
// /v1/decisions, translate for /v1/systemone); the account's chat models keep
// their chat flavors only.
func TestVendorDecisionModelIsListedWithTheDecisionsFlavor(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newVendorAccountTestService(t, now)
	seedVendorDecisionAccount(t, routeStore, now, "acc_oai", routing.VendorOpenAI, routing.VendorAuthAPIKey, "oa/", vendorDecisionSlug, "gpt-5")
	ctx := context.Background()
	owner := withAllVendorAccess(ownerToken())

	data := svc.Models(ctx, owner).Data
	row, ok := modelDTONamed(data, "oa/"+vendorDecisionSlug)
	if !ok {
		t.Fatalf("Models() has no row for oa/%s", vendorDecisionSlug)
	}
	if !slices.Equal(row.Flavors, []string{routing.APIFlavorAnthropic, routing.APIFlavorDecisions, routing.APIFlavorOpenAI}) || !row.Decision || !row.DecisionOpenAI {
		t.Fatalf("row = %+v, want flavors [anthropic decisions openai] with decision and decision_openai", row)
	}
	chat, ok := modelDTONamed(data, "oa/gpt-5")
	if !ok || !slices.Equal(chat.Flavors, []string{routing.APIFlavorAnthropic, routing.APIFlavorOpenAI}) || chat.Decision || chat.DecisionOpenAI {
		t.Fatalf("chat row = %+v (found %v), want [anthropic openai] and no decision flags", chat, ok)
	}
	if got := svc.ModelsForFlavor(ctx, owner, routing.APIFlavorDecisions); !slices.Contains(got, "oa/"+vendorDecisionSlug) || slices.Contains(got, "oa/gpt-5") {
		t.Fatalf("ModelsForFlavor(decisions) = %v, want the decision model only", got)
	}
	for _, flavor := range []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic} {
		if got := svc.ModelsForFlavor(ctx, owner, flavor); !slices.Contains(got, "oa/"+vendorDecisionSlug) || !slices.Contains(got, "oa/gpt-5") {
			t.Fatalf("ModelsForFlavor(%s) = %v, want the decision model listed for chat as before", flavor, got)
		}
	}
}

// §11 visibility applies unchanged: no access, no listing; a prefix override
// renames the listed decision model.
func TestVendorDecisionModelListingFollowsTheTokenVendorAccess(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newVendorAccountTestService(t, now)
	seedVendorDecisionAccount(t, routeStore, now, "acc_oai", routing.VendorOpenAI, routing.VendorAuthAPIKey, "oa/", vendorDecisionSlug)
	ctx := context.Background()

	if _, ok := modelDTONamed(svc.Models(ctx, ownerToken()).Data, "oa/"+vendorDecisionSlug); ok {
		t.Fatal("a token without vendor access lists the decision model")
	}
	override := ownerToken()
	override.VendorAccess = auth.VendorAccess{Accounts: []auth.VendorAccessEntry{{AccountID: "acc_oai", OverrideEnabled: true, OverridePrefix: "dx/"}}}
	data := svc.Models(ctx, override).Data
	if row, ok := modelDTONamed(data, "dx/"+vendorDecisionSlug); !ok || !row.Decision || !row.DecisionOpenAI {
		t.Fatalf("override row = %+v (found %v), want dx/%s with both decision flags", row, ok, vendorDecisionSlug)
	}
	if _, ok := modelDTONamed(data, "oa/"+vendorDecisionSlug); ok {
		t.Fatal("the native name is listed under a prefix override")
	}
}

// Only an OpenAI api-key account makes a decision model: the same row on an
// Anthropic, subscription or preset account is listed as it always was, without
// the decisions flavor or a decision flag.
func TestVendorDecisionFlavorIsOnlyForTheOpenAIAPIKeyAccount(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct{ vendor, authType string }{
		{routing.VendorAnthropic, routing.VendorAuthAPIKey},
		{routing.VendorAnthropic, routing.VendorAuthSubscription},
		{routing.VendorOpenAI, routing.VendorAuthSubscription},
		{routing.VendorXAI, routing.VendorAuthAPIKey},
		{routing.VendorOpenAICompatible, routing.VendorAuthAPIKey},
		{routing.VendorGoogleNative, routing.VendorAuthAPIKey},
	} {
		t.Run(tc.vendor+"/"+tc.authType, func(t *testing.T) {
			svc, routeStore := newVendorAccountTestService(t, now)
			seedVendorDecisionAccount(t, routeStore, now, "acc_x", tc.vendor, tc.authType, "", vendorDecisionSlug)
			row, ok := modelDTONamed(svc.Models(context.Background(), withAllVendorAccess(ownerToken())).Data, vendorDecisionSlug)
			if !ok {
				t.Fatalf("Models() has no row for %s", vendorDecisionSlug)
			}
			if slices.Contains(row.Flavors, routing.APIFlavorDecisions) || row.Decision || row.DecisionOpenAI {
				t.Fatalf("row = %+v, want no decisions flavor and no decision flag", row)
			}
		})
	}
}

// The offering sees the decision model as callable (and a capable candidate)
// for both decision endpoints, and keeps it callable and existing for chat as
// before; it is a capable candidate only for a decision request.
func TestVendorDecisionModelIsInTheOfferingForBothDecisionEndpoints(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newVendorAccountTestService(t, now)
	seedVendorDecisionAccount(t, routeStore, now, "acc_oai", routing.VendorOpenAI, routing.VendorAuthAPIKey, "oa/", vendorDecisionSlug, "gpt-5")
	ctx := context.Background()
	owner := withAllVendorAccess(ownerToken())
	has := func(set map[string]struct{}, name string) bool { _, ok := set[name]; return ok }
	anyDecision := []string{routing.CapabilityAnyDecision}

	for _, flavor := range []string{routing.APIFlavorOpenAIDecisions, routing.APIFlavorDecisions} {
		off := svc.ModelOfferingFor(ctx, owner, flavor, anyDecision)
		name := "oa/" + vendorDecisionSlug
		if !has(off.Callable, name) || !has(off.Existing, name) || !has(off.Capable, name) {
			t.Fatalf("%s offering = %+v, want %s callable, existing and capable", flavor, off, name)
		}
		if has(off.Callable, "oa/gpt-5") || has(off.Capable, "oa/gpt-5") {
			t.Fatalf("%s offering = %+v, want the chat model out", flavor, off)
		}
	}
	for _, flavor := range []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic} {
		chat := svc.ModelOfferingFor(ctx, owner, flavor, nil)
		if !has(chat.Callable, "oa/"+vendorDecisionSlug) || !has(chat.Existing, "oa/"+vendorDecisionSlug) || !has(chat.Callable, "oa/gpt-5") {
			t.Fatalf("%s offering = %+v, want both models callable and existing, as before", flavor, chat)
		}
	}
	// [any_decision] on a chat flavor is no decision request: the resolver refuses
	// it on the vendor path, so the offering names no vendor candidate for it.
	chatAny := svc.ModelOfferingFor(ctx, owner, routing.APIFlavorOpenAI, anyDecision)
	if has(chatAny.Capable, "oa/"+vendorDecisionSlug) {
		t.Fatalf("openai + [any_decision] offering = %+v, want no vendor decision candidate", chatAny)
	}
	none := svc.ModelOfferingFor(ctx, ownerToken(), routing.APIFlavorOpenAIDecisions, anyDecision)
	if has(none.Callable, "oa/"+vendorDecisionSlug) || has(none.Capable, "oa/"+vendorDecisionSlug) {
		t.Fatalf("offering without vendor access = %+v, want the decision model out", none)
	}
}

// The OpenAI api-key seed carries the decision model.
func TestVendorCatalogSeedsTheOpenAIDecisionModel(t *testing.T) {
	ids := catalogGatewayModels(VendorCatalog(routing.VendorOpenAI, routing.VendorAuthAPIKey))
	if !slices.Contains(ids, vendorDecisionSlug) {
		t.Fatalf("OpenAI api-key catalog = %v, want %s", ids, vendorDecisionSlug)
	}
	for _, authType := range vendorAuthTypes {
		if slices.Contains(catalogGatewayModels(VendorCatalog(routing.VendorAnthropic, authType)), vendorDecisionSlug) {
			t.Fatalf("Anthropic %s catalog carries %s", authType, vendorDecisionSlug)
		}
	}
	if slices.Contains(catalogGatewayModels(VendorCatalog(routing.VendorOpenAI, routing.VendorAuthSubscription)), vendorDecisionSlug) {
		t.Fatalf("OpenAI subscription catalog carries %s (the ChatGPT backend has no Decisions endpoint)", vendorDecisionSlug)
	}
}

// A vendor decision model that shares its name with a self-hosted decision model
// whose System One endpoint is disabled (so the self-hosted fold lists the name
// under decisions with Decision false) is still listed and offered as a decision
// model on both endpoints: under vendor_first the vendor serves it, and the
// listing's flags and the offering's Capable both OR the vendor in.
func TestVendorDecisionModelOrsInOverASelfHostedNameCollision(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newVendorAccountTestService(t, now)
	name := "oa/" + vendorDecisionSlug
	offerDecisionModel(t, routeStore, routing.ProviderVLLM, modesSystemOneOff, nil, name)
	seedVendorDecisionAccount(t, routeStore, now, "acc_oai", routing.VendorOpenAI, routing.VendorAuthAPIKey, "oa/", vendorDecisionSlug)
	ctx := context.Background()
	owner := withAllVendorAccess(ownerToken())

	row, ok := modelDTONamed(svc.Models(ctx, owner).Data, name)
	if !ok || row.OfferedOnCount != 1 {
		t.Fatalf("row = %+v (found %v), want the shared name with its self-hosted mapping visible", row, ok)
	}
	if !row.Decision || !row.DecisionOpenAI {
		t.Fatalf("row = %+v, want both decision flags ORed in from the vendor", row)
	}
	for _, flavor := range []string{routing.APIFlavorOpenAIDecisions, routing.APIFlavorDecisions} {
		off := svc.ModelOfferingFor(ctx, owner, flavor, []string{routing.CapabilityAnyDecision})
		if _, ok := off.Capable[name]; !ok {
			t.Fatalf("%s offering = %+v, want %s capable, as the listing says", flavor, off, name)
		}
	}
}
