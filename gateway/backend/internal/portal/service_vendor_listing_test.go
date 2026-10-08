// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/usage"
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

	// A catalog model (the first OpenAI one, gpt-5) is listed in the picker under both flavors.
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

// TestVendorModelsOpenAISubscriptionAdvertisedUnderOpenAIOnly proves an ACTIVE
// OpenAI SUBSCRIPTION account's models ARE advertised to their owner as of M5c — in
// the chat picker and /v1/models (openai) — but NOT in /anthropic/v1/models, because
// the ChatGPT backend speaks Responses only and the account serves the openai
// dialect only (openai_responses passthrough + chat translate). Listing it under
// anthropic would break served_flavors parity (an anthropic call would 404).
func TestVendorModelsOpenAISubscriptionAdvertisedUnderOpenAIOnly(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newVendorAccountTestService(t, now)
	ctx := context.Background()
	owner := ownerToken()

	// An api_key OpenAI account (its catalog models stay visible under both dialects).
	createTestVendorAccount(t, svc, owner, apiKeyAccountRequest("My OpenAI"))
	// An ACTIVE OpenAI subscription account serving a distinct model.
	const subModel = "gpt-5-codex"
	seedActiveSubscriptionAccount(t, routeStore, now, owner, "acc_sub_oai", routing.VendorOpenAI, subModel, routing.APIFlavorOpenAI)

	// The subscription model is advertised under the openai dialect.
	row, ok := modelDTONamed(svc.Models(ctx, owner).Data, subModel)
	if !ok {
		t.Fatalf("Models() has no row for the OpenAI subscription model %q; it must be advertised in M5c", subModel)
	}
	if !slices.Contains(row.Flavors, routing.APIFlavorOpenAI) {
		t.Errorf("subscription model Flavors = %q, want it to include openai", row.Flavors)
	}
	if slices.Contains(row.Flavors, routing.APIFlavorAnthropic) {
		t.Errorf("subscription model Flavors = %q, must NOT include anthropic (the backend speaks Responses only)", row.Flavors)
	}
	if got := svc.ModelsForFlavor(ctx, owner, routing.APIFlavorOpenAI); !slices.Contains(got, subModel) {
		t.Errorf("/v1/models (openai) = %q, want it to include the subscription model %q", got, subModel)
	}
	if got := svc.ModelsForFlavor(ctx, owner, routing.APIFlavorAnthropic); slices.Contains(got, subModel) {
		t.Errorf("/anthropic/v1/models = %q, must not include the subscription model %q", got, subModel)
	}

	// The api_key account's catalog model is still advertised under both dialects.
	if _, ok := modelDTONamed(svc.Models(ctx, owner).Data, "gpt-5"); !ok {
		t.Error("Models() dropped the api_key OpenAI model gpt-5")
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

// TestVendorModelsAreAdvertisedUnderTheirPrefixedName proves the listings carry a
// vendor account's model under its PREFIXED gateway name (the public id a caller
// requests), never under the bare vendor slug, and that every advertised name is
// really requestable: the resolver maps it to a target that sends the RAW slug
// upstream. Covers both account shapes: an api_key account created through the
// service with a prefix (its catalog seed is re-labelled) and a subscription
// account holding a discovery-shaped row (GatewayModel = prefix + slug,
// UpstreamModel = slug), which is advertised under the openai dialect only.
func TestVendorModelsAreAdvertisedUnderTheirPrefixedName(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newVendorAccountTestService(t, now)
	ctx := context.Background()
	owner := ownerToken()

	// api_key account created WITH a prefix: the seeded catalog rows are prefixed.
	req := apiKeyAccountRequest("Work OpenAI")
	req.ModelPrefix = "work/"
	createTestVendorAccount(t, svc, owner, req)

	// Subscription account whose row is exactly what discovery writes.
	const (
		subSlug   = "gpt-6-luna"
		subPrefix = "chatgpt/"
	)
	if err := routeStore.CreateVendorAccount(ctx, routing.VendorAccount{
		ID: "acc_sub_oai", OwnerUserID: owner.UserID, Vendor: routing.VendorOpenAI, AuthType: routing.VendorAuthSubscription,
		Name: "ChatGPT", Status: routing.VendorAccountStatusActive, OAuthTokens: "enc:sealed", ModelPrefix: subPrefix,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateVendorAccount: %v", err)
	}
	if err := routeStore.SetVendorAccountModels(ctx, "acc_sub_oai", []routing.VendorAccountModel{
		{AccountID: "acc_sub_oai", GatewayModel: subPrefix + subSlug, UpstreamModel: subSlug, APIFlavor: routing.APIFlavorOpenAI, DisplayName: "GPT 6 Luna"},
	}); err != nil {
		t.Fatalf("SetVendorAccountModels: %v", err)
	}

	picker := svc.Models(ctx, owner).Data
	openAIList := svc.ModelsForFlavor(ctx, owner, routing.APIFlavorOpenAI)
	anthropicList := svc.ModelsForFlavor(ctx, owner, routing.APIFlavorAnthropic)

	// The prefixed names are advertised ...
	for _, name := range []string{"work/gpt-5", subPrefix + subSlug} {
		if _, ok := modelDTONamed(picker, name); !ok {
			t.Errorf("Models() has no row for %q; the prefixed gateway name must be advertised", name)
		}
		if !slices.Contains(openAIList, name) {
			t.Errorf("/v1/models = %q, want it to include %q", openAIList, name)
		}
	}
	// ... and the bare slugs are not (they are no longer names the gateway serves).
	for _, bare := range []string{"gpt-5", subSlug} {
		if _, ok := modelDTONamed(picker, bare); ok {
			t.Errorf("Models() lists the bare slug %q; only the prefixed name is served", bare)
		}
		if slices.Contains(openAIList, bare) || slices.Contains(anthropicList, bare) {
			t.Errorf("a listing includes the bare slug %q; only the prefixed name is served", bare)
		}
	}
	// Dialects follow the account kind: api_key serves both, the OpenAI
	// subscription openai only.
	if !slices.Contains(anthropicList, "work/gpt-5") {
		t.Errorf("/anthropic/v1/models = %q, want it to include work/gpt-5 (api_key accounts serve both dialects)", anthropicList)
	}
	if slices.Contains(anthropicList, subPrefix+subSlug) {
		t.Errorf("/anthropic/v1/models = %q, must not include %q (OpenAI subscription is openai-only)", anthropicList, subPrefix+subSlug)
	}

	// Advertised == requestable: the resolver serves each prefixed name and sends
	// the raw slug upstream.
	resolver := routing.NewResolver(routeStore, func() time.Time { return now }, nil)
	resolver.SetVendorAccountAccessors(func() bool { return true }, nil)
	for name, wantUpstream := range map[string]string{"work/gpt-5": "gpt-5", subPrefix + subSlug: subSlug} {
		target, err := resolver.Resolve(ctx, owner, inference.Request{Model: name, APIFlavor: "openai_chat"})
		if err != nil {
			t.Errorf("Resolve(%q) = %v, want the advertised name to be requestable", name, err)
			continue
		}
		if target.Model != name || target.ProviderModel != wantUpstream {
			t.Errorf("Resolve(%q): Model=%q ProviderModel=%q, want %q / %q", name, target.Model, target.ProviderModel, name, wantUpstream)
		}
	}
}

// newVendorDashboardTestService is newVendorAccountTestService plus the usage
// recorder Dashboard reads its metrics from (the shared constructor wires none).
func newVendorDashboardTestService(t *testing.T, now time.Time) (*Service, *routing.MemoryStore) {
	t.Helper()
	svc, routeStore := newVendorAccountTestService(t, now)
	svc.usage = usage.NewRecorder()
	return svc, routeStore
}

// seedPrefixedVendorAccount seeds an account of the given status holding one
// discovery-shaped model row (GatewayModel = prefix + slug, UpstreamModel = slug)
// directly into the route store, so a dashboard test can assert which accounts'
// models surface.
func seedPrefixedVendorAccount(t *testing.T, routeStore *routing.MemoryStore, now time.Time, owner auth.Token, id, name, status, prefix, slug string) {
	t.Helper()
	ctx := context.Background()
	if err := routeStore.CreateVendorAccount(ctx, routing.VendorAccount{
		ID: id, OwnerUserID: owner.UserID, Vendor: routing.VendorOpenAI, AuthType: routing.VendorAuthSubscription,
		Name: name, Status: status, OAuthTokens: "enc:sealed", ModelPrefix: prefix, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateVendorAccount %s: %v", id, err)
	}
	if err := routeStore.SetVendorAccountModels(ctx, id, []routing.VendorAccountModel{
		{AccountID: id, GatewayModel: prefix + slug, UpstreamModel: slug, APIFlavor: routing.APIFlavorOpenAI, DisplayName: slug},
	}); err != nil {
		t.Fatalf("SetVendorAccountModels %s: %v", id, err)
	}
}

// routesForModel returns the dashboard rows whose Model is name.
func routesForModel(routes []RouteDTO, name string) []RouteDTO {
	var out []RouteDTO
	for _, r := range routes {
		if r.Model == name {
			out = append(out, r)
		}
	}
	return out
}

// TestVendorModelsAppearInTheOwnersDashboardRoutes proves the dashboard's "Live
// Model Routes" table carries the principal's own active vendor-account models,
// under their PREFIXED gateway name, next to the self-hosted routes: provider is
// the vendor, host is the account's name (a vendor model has no server), status
// is active. It is the dashboard half of the model-listing owner overlay.
func TestVendorModelsAppearInTheOwnersDashboardRoutes(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newVendorDashboardTestService(t, now)
	ctx := context.Background()
	owner := ownerToken()

	// A self-hosted route, so the table proves the two kinds sit side by side.
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "srv_1", Name: "Server One", Domain: "s1.test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: "app_1", ServerID: "srv_1", Type: routing.ProviderVLLM, Port: 8000, Scheme: "https", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: "map_1", ApplicationID: "app_1", GatewayModelName: "qwen-coder", AppModelName: "qwen2.5", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	seedPrefixedVendorAccount(t, routeStore, now, owner, "acc_sub_oai", "My ChatGPT", routing.VendorAccountStatusActive, "chatgpt/", "gpt-6-luna")

	routes := svc.Dashboard(ctx, owner).Routes

	rows := routesForModel(routes, "chatgpt/gpt-6-luna")
	if len(rows) != 1 {
		t.Fatalf("dashboard routes = %#v, want exactly one row for the prefixed vendor model", routes)
	}
	if row := rows[0]; row.Provider != routing.VendorOpenAI || row.Host != "My ChatGPT" || row.Status != "active" {
		t.Errorf("vendor route row = %#v, want provider %q, host the account name, status active", row, routing.VendorOpenAI)
	}
	if got := routesForModel(routes, "gpt-6-luna"); len(got) != 0 {
		t.Errorf("dashboard routes list the bare slug: %#v; only the prefixed name is served", got)
	}
	if got := routesForModel(routes, "qwen-coder"); len(got) != 1 {
		t.Errorf("self-hosted route lost: %#v", routes)
	}
}

// TestVendorDashboardRoutesAreOwnerScopedAndGated proves the dashboard overlay
// follows the same rules as the other listings: another user, a service token
// (no UserID, owns no accounts) and a master flag that is off all see none of it,
// and an account that is not active (disabled, needs_reconnect) contributes
// nothing, because dispatch would not serve it either.
func TestVendorDashboardRoutesAreOwnerScopedAndGated(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newVendorDashboardTestService(t, now)
	ctx := context.Background()
	owner := ownerToken()
	seedPrefixedVendorAccount(t, routeStore, now, owner, "acc_active", "Active", routing.VendorAccountStatusActive, "a/", "m1")
	seedPrefixedVendorAccount(t, routeStore, now, owner, "acc_disabled", "Disabled", routing.VendorAccountStatusDisabled, "d/", "m2")
	seedPrefixedVendorAccount(t, routeStore, now, owner, "acc_reconnect", "Reconnect", routing.VendorAccountStatusNeedsReconnect, "r/", "m3")

	ownerRoutes := svc.Dashboard(ctx, owner).Routes
	if len(routesForModel(ownerRoutes, "a/m1")) != 1 {
		t.Fatalf("owner routes = %#v, want the active account's model", ownerRoutes)
	}
	for _, inactive := range []string{"d/m2", "r/m3"} {
		if got := routesForModel(ownerRoutes, inactive); len(got) != 0 {
			t.Errorf("owner routes list %q of a non-active account: %#v", inactive, got)
		}
	}

	for name, token := range map[string]auth.Token{
		"another user":  otherToken(),
		"service token": {Scopes: []string{"gateway:use"}},
	} {
		if got := routesForModel(svc.Dashboard(ctx, token).Routes, "a/m1"); len(got) != 0 {
			t.Errorf("%s sees the owner's vendor model on the dashboard: %#v", name, got)
		}
	}

	setVendorAccountsEnabled(t, svc, false)
	if got := routesForModel(svc.Dashboard(ctx, owner).Routes, "a/m1"); len(got) != 0 {
		t.Errorf("dashboard lists the vendor model with the master flag off: %#v", got)
	}
}

// TestVendorDashboardRoutesKeepADeterministicOrder proves two accounts that serve
// the same model name each get a row with its own id (so two accounts that even
// share a NAME stay distinct for the table's row key) and the table order does
// not depend on map iteration or sort instability: model, then row id.
func TestVendorDashboardRoutesKeepADeterministicOrder(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newVendorDashboardTestService(t, now)
	ctx := context.Background()
	owner := ownerToken()
	seedPrefixedVendorAccount(t, routeStore, now, owner, "acc_b", "Beta", routing.VendorAccountStatusActive, "", "shared")
	seedPrefixedVendorAccount(t, routeStore, now, owner, "acc_a", "Alpha", routing.VendorAccountStatusActive, "", "shared")
	seedPrefixedVendorAccount(t, routeStore, now, owner, "acc_c", "Alpha", routing.VendorAccountStatusActive, "", "shared")
	seedPrefixedVendorAccount(t, routeStore, now, owner, "acc_z", "Zeta", routing.VendorAccountStatusActive, "", "aaa")

	want := []string{"acc_z:aaa", "acc_a:shared", "acc_b:shared", "acc_c:shared"}
	for i := 0; i < 20; i++ {
		var order []string
		for _, r := range svc.Dashboard(ctx, owner).Routes {
			order = append(order, r.ID)
		}
		if !slices.Equal(order, want) {
			t.Fatalf("dashboard row ids = %v, want %v", order, want)
		}
	}
}
