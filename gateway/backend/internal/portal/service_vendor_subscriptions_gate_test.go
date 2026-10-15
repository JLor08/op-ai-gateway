// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/routing"
	"slices"
	"strconv"
	"testing"
	"time"
)

// setVendorSubscriptionsEnabled writes the vendor_subscriptions_enabled setting
// into svc's existing settings store (the vendor-account test service holds one
// with the master flag on), leaving every other key alone.
func setVendorSubscriptionsEnabled(t *testing.T, svc *Service, enabled bool) {
	t.Helper()
	if err := svc.settings.SetSystemSetting(context.Background(), vendorSubscriptionsEnabledKey, strconv.FormatBool(enabled), time.Time{}); err != nil {
		t.Fatalf("SetSystemSetting: %v", err)
	}
}

// vendor_subscriptions_enabled is default ON (opt-out): an absent, blank or
// unparseable value must read as true, so only an explicit false switches the
// operator's subscription accounts off.
func TestVendorSubscriptionsEnabledHelper(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]string
		want bool
	}{
		{"nil -> on", nil, true},
		{"absent -> on", map[string]string{}, true},
		{"blank -> on", map[string]string{vendorSubscriptionsEnabledKey: ""}, true},
		{"unparseable -> on", map[string]string{vendorSubscriptionsEnabledKey: "maybe"}, true},
		{"explicit true", map[string]string{vendorSubscriptionsEnabledKey: "true"}, true},
		{"padded true", map[string]string{vendorSubscriptionsEnabledKey: " true "}, true},
		{"explicit false", map[string]string{vendorSubscriptionsEnabledKey: "false"}, false},
		{"padded false", map[string]string{vendorSubscriptionsEnabledKey: " false "}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := VendorSubscriptionsEnabled(tc.in); got != tc.want {
				t.Fatalf("VendorSubscriptionsEnabled(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// The service accessor, the settings view and a missing/unreadable store all agree
// on ON: a transient settings read failure must never silently disable the
// operator's subscription accounts.
func TestVendorSubscriptionsEnabledDefaultsToOn(t *testing.T) {
	ctx := context.Background()
	stores := map[string]*Service{
		"empty store":  NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Clock: fixedClock()}),
		"nil store":    NewService(ServiceDeps{Clock: fixedClock()}),
		"read failure": NewService(ServiceDeps{SystemSettings: failingSettingsReadStore{NewMemorySystemSettings()}, Clock: fixedClock()}),
	}
	for name, svc := range stores {
		t.Run(name, func(t *testing.T) {
			if !svc.VendorSubscriptionsEnabled(ctx) {
				t.Fatal("VendorSubscriptionsEnabled = false, want true (default on, fails to on)")
			}
			if !svc.SystemSettingsView(ctx).VendorSubscriptionsEnabled {
				t.Fatal("view.VendorSubscriptionsEnabled = false, want true (default on)")
			}
		})
	}
}

func TestUpdateSystemSettingsPersistsVendorSubscriptionsEnabled(t *testing.T) {
	ctx := context.Background()
	settings := NewMemorySystemSettings()
	svc := NewService(ServiceDeps{SystemSettings: settings, Clock: fixedClock()})

	off := false
	got, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{VendorSubscriptionsEnabled: &off})
	if err != nil {
		t.Fatalf("UpdateSystemSettings(off): %v", err)
	}
	if got.VendorSubscriptionsEnabled {
		t.Fatal("returned DTO VendorSubscriptionsEnabled = true after turning it off")
	}
	values, err := settings.SystemSettings(ctx)
	if err != nil {
		t.Fatalf("SystemSettings: %v", err)
	}
	if values[vendorSubscriptionsEnabledKey] != "false" {
		t.Fatalf("stored vendor_subscriptions_enabled = %q, want %q", values[vendorSubscriptionsEnabledKey], "false")
	}
	if svc.VendorSubscriptionsEnabled(ctx) || svc.SystemSettingsView(ctx).VendorSubscriptionsEnabled {
		t.Fatal("accessor/view report on after turning it off")
	}

	// A request that omits the field keeps the stored value (nil = keep).
	enabled := true
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{VendorAccountsEnabled: &enabled}); err != nil {
		t.Fatalf("UpdateSystemSettings(other field): %v", err)
	}
	if svc.VendorSubscriptionsEnabled(ctx) {
		t.Fatal("an update that omitted vendor_subscriptions_enabled changed it")
	}

	on := true
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{VendorSubscriptionsEnabled: &on}); err != nil {
		t.Fatalf("UpdateSystemSettings(on): %v", err)
	}
	if values, _ = settings.SystemSettings(ctx); values[vendorSubscriptionsEnabledKey] != "true" {
		t.Fatalf("stored vendor_subscriptions_enabled = %q, want %q", values[vendorSubscriptionsEnabledKey], "true")
	}
	if !svc.VendorSubscriptionsEnabled(ctx) {
		t.Fatal("accessor reports off after turning it back on")
	}
}

func TestUpdateSystemSettingsVendorSubscriptionsEnabledRequiresSystemScope(t *testing.T) {
	svc := NewService(ServiceDeps{SystemSettings: NewMemorySystemSettings(), Clock: fixedClock()})
	off := false
	if _, err := svc.UpdateSystemSettings(context.Background(), adminToken(), UpdateSystemSettingsRequest{VendorSubscriptionsEnabled: &off}); !errors.Is(err, ErrPrincipalForbidden) {
		t.Fatalf("plain admin err = %v, want ErrPrincipalForbidden", err)
	}
	if !svc.VendorSubscriptionsEnabled(context.Background()) {
		t.Fatal("a forbidden update switched subscriptions off")
	}
}

// subscriptionGateListingFixture is an owner with an api_key OpenAI account (catalog
// model gpt-5) and an OpenAI and an Anthropic SUBSCRIPTION account, each serving
// one distinct model, plus a resolver wired to the same service the way the gateway
// wires it (master flag on, subscriptions accessor read from svc).
type subscriptionGateListingFixture struct {
	svc      *Service
	owner    auth.Token
	resolver *routing.Resolver
}

const (
	gateAPIKeyModel  = "gpt-5"
	gateOpenAISubMdl = "gpt-5-codex"
	gateAnthropicSub = "claude-sub-model"
)

func newSubscriptionGateListingFixture(t *testing.T) subscriptionGateListingFixture {
	t.Helper()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newVendorAccountTestService(t, now)
	owner := withAllVendorAccess(ownerToken())
	createTestVendorAccount(t, svc, owner, apiKeyAccountRequest("My OpenAI"))
	seedActiveSubscriptionAccount(t, routeStore, now, owner, "acc_sub_oai", routing.VendorOpenAI, gateOpenAISubMdl, routing.APIFlavorOpenAI)
	seedActiveSubscriptionAccount(t, routeStore, now, owner, "acc_sub_ant", routing.VendorAnthropic, gateAnthropicSub, routing.APIFlavorAnthropic)

	resolver := routing.NewResolver(routeStore, func() time.Time { return now }, nil)
	resolver.SetVendorAccountAccessors(
		func() bool { return svc.VendorAccountsEnabled(context.Background()) },
		nil,
		func() bool { return svc.VendorSubscriptionsEnabled(context.Background()) },
	)
	return subscriptionGateListingFixture{svc: svc, owner: owner, resolver: resolver}
}

// advertised reports whether name is in the owner's /v1/models, the Anthropic list,
// the chat picker or the dashboard route table (the four listing surfaces).
func (f subscriptionGateListingFixture) advertised(name string) (openAI, anthropic, picker, dashboard bool) {
	ctx := context.Background()
	_, picker = modelDTONamed(f.svc.Models(ctx, f.owner).Data, name)
	return slices.Contains(f.svc.ModelsForFlavor(ctx, f.owner, routing.APIFlavorOpenAI), name),
		slices.Contains(f.svc.ModelsForFlavor(ctx, f.owner, routing.APIFlavorAnthropic), name),
		picker,
		len(routesForModel(f.svc.vendorDashboardRoutes(ctx, f.owner), name)) > 0
}

// routable reports whether the resolver serves name to the owner over openai chat.
func (f subscriptionGateListingFixture) routable(t *testing.T, name string) bool {
	t.Helper()
	_, err := f.resolver.Resolve(context.Background(), f.owner, inference.Request{Model: name, APIFlavor: "openai_chat"})
	if err != nil && !errors.Is(err, routing.ErrNoModelRoute) {
		t.Fatalf("Resolve(%q): unexpected error %v", name, err)
	}
	return err == nil
}

// TestVendorSubscriptionsOffHidesSubscriptionModelsFromEveryListing proves the
// LISTING half of the gate: with vendor_subscriptions_enabled OFF the owner's
// subscription models vanish from /v1/models, the Anthropic list, the chat picker
// and the dashboard route table, while the api_key account's model stays; ON
// brings them back. The accounts themselves are never deleted.
func TestVendorSubscriptionsOffHidesSubscriptionModelsFromEveryListing(t *testing.T) {
	f := newSubscriptionGateListingFixture(t)

	// ON (the default): everything is advertised on its served dialects.
	for _, name := range []string{gateAPIKeyModel, gateOpenAISubMdl, gateAnthropicSub} {
		openAI, _, picker, dashboard := f.advertised(name)
		if !openAI || !picker || !dashboard {
			t.Fatalf("subscriptions on: %q advertised openai/picker/dashboard = %v/%v/%v, want all true", name, openAI, picker, dashboard)
		}
	}

	setVendorSubscriptionsEnabled(t, f.svc, false)

	openAI, anthropic, picker, dashboard := f.advertised(gateAPIKeyModel)
	if !openAI || !anthropic || !picker || !dashboard {
		t.Errorf("subscriptions off: the api_key model %q is advertised %v/%v/%v/%v, want it untouched", gateAPIKeyModel, openAI, anthropic, picker, dashboard)
	}
	for _, name := range []string{gateOpenAISubMdl, gateAnthropicSub} {
		openAI, anthropic, picker, dashboard := f.advertised(name)
		if openAI || anthropic || picker || dashboard {
			t.Errorf("subscriptions off: subscription model %q still advertised openai/anthropic/picker/dashboard = %v/%v/%v/%v", name, openAI, anthropic, picker, dashboard)
		}
	}
	// vendorModelFlavorSets (the alias derivation's input) agrees, and stays complete.
	sets, loaded := f.svc.vendorModelFlavorSets(context.Background(), f.owner)
	if !loaded {
		t.Error("subscriptions off: vendorModelFlavorSets reported an incomplete set")
	}
	if _, ok := sets[gateAPIKeyModel]; !ok {
		t.Errorf("subscriptions off: vendorModelFlavorSets lost the api_key model %q", gateAPIKeyModel)
	}
	for _, name := range []string{gateOpenAISubMdl, gateAnthropicSub} {
		if _, ok := sets[name]; ok {
			t.Errorf("subscriptions off: vendorModelFlavorSets still holds subscription model %q", name)
		}
	}

	setVendorSubscriptionsEnabled(t, f.svc, true)
	for _, name := range []string{gateAPIKeyModel, gateOpenAISubMdl, gateAnthropicSub} {
		if openAI, _, _, _ := f.advertised(name); !openAI {
			t.Errorf("subscriptions back on: %q is not advertised again (the accounts must survive the toggle)", name)
		}
	}
}

// TestVendorSubscriptionsGateKeepsListingAndResolverInLockStep is the served-flavors
// parity invariant for the new gate: in BOTH states, a vendor model is advertised
// exactly when the resolver serves it (never advertised-but-unroutable, never
// routable-but-unadvertised), and the subscription models flip with the setting
// while the api_key model never does.
func TestVendorSubscriptionsGateKeepsListingAndResolverInLockStep(t *testing.T) {
	f := newSubscriptionGateListingFixture(t)

	for _, state := range []struct {
		label        string
		on           bool
		wantServed   []string
		wantUnserved []string
	}{
		{"on", true, []string{gateAPIKeyModel, gateOpenAISubMdl, gateAnthropicSub}, nil},
		{"off", false, []string{gateAPIKeyModel}, []string{gateOpenAISubMdl, gateAnthropicSub}},
		{"on again", true, []string{gateAPIKeyModel, gateOpenAISubMdl, gateAnthropicSub}, nil},
	} {
		setVendorSubscriptionsEnabled(t, f.svc, state.on)
		for _, name := range append(slices.Clone(state.wantServed), state.wantUnserved...) {
			advertisedOpenAI, _, _, _ := f.advertised(name)
			routable := f.routable(t, name)
			if advertisedOpenAI != routable {
				t.Errorf("subscriptions %s: %q advertised = %v but routable = %v; the listing and the resolver must move together", state.label, name, advertisedOpenAI, routable)
			}
			if want := slices.Contains(state.wantServed, name); routable != want {
				t.Errorf("subscriptions %s: %q routable = %v, want %v", state.label, name, routable, want)
			}
		}
	}
}

// Creating a SUBSCRIPTION account is refused while the gate is off (an api_key one
// is not), nothing is persisted by the refusal, and it works again when switched on.
func TestCreateVendorAccountSubscriptionRefusedWhenSubscriptionsOff(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newVendorAccountTestService(t, now)
	ctx := context.Background()
	setVendorSubscriptionsEnabled(t, svc, false)

	for _, vendor := range []string{routing.VendorOpenAI, routing.VendorAnthropic} {
		_, err := svc.CreateVendorAccount(ctx, ownerToken(), CreateVendorAccountRequest{Vendor: vendor, AuthType: routing.VendorAuthSubscription, Name: "Sub " + vendor})
		if !errors.Is(err, ErrVendorSubscriptionsDisabled) {
			t.Fatalf("create %s subscription while off: err = %v, want ErrVendorSubscriptionsDisabled", vendor, err)
		}
	}
	if rows, err := routeStore.VendorAccounts(ctx); err != nil || len(rows) != 0 {
		t.Fatalf("rows after refused creates = %#v, %v, want none persisted", rows, err)
	}

	// An api_key account (either vendor) is untouched by the gate.
	createTestVendorAccount(t, svc, ownerToken(), apiKeyAccountRequest("Key account"))

	setVendorSubscriptionsEnabled(t, svc, true)
	dto := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorAnthropic, "Claude Max")
	if dto.AuthType != routing.VendorAuthSubscription {
		t.Fatalf("created auth_type = %q, want subscription once the gate is back on", dto.AuthType)
	}
}

// The master flag keeps its precedence: with it off the refusal is still
// ErrVendorAccountsDisabled, whatever the subscriptions setting says.
func TestCreateVendorAccountMasterFlagOffStillWinsOverSubscriptionsGate(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	svc, _ := newVendorAccountTestService(t, now)
	setVendorSubscriptionsEnabled(t, svc, false)
	if err := svc.settings.SetSystemSetting(context.Background(), vendorAccountsEnabledKey, "false", time.Time{}); err != nil {
		t.Fatalf("SetSystemSetting: %v", err)
	}
	_, err := svc.CreateVendorAccount(context.Background(), ownerToken(), CreateVendorAccountRequest{Vendor: routing.VendorOpenAI, AuthType: routing.VendorAuthSubscription, Name: "Sub"})
	if !errors.Is(err, ErrVendorAccountsDisabled) {
		t.Fatalf("err = %v, want ErrVendorAccountsDisabled", err)
	}
}

// Every connect method (token import, code-paste begin/complete, device begin/poll)
// is refused with ErrVendorSubscriptionsDisabled while the gate is off, leaves the
// account untouched, and works again once it is switched on. The checks keep their
// order: an unknown id or a stranger is still a not-found, and an api_key account
// is still "not a subscription account" (the gate does not mask either).
func TestConnectVendorAccountRefusedWhenSubscriptionsOff(t *testing.T) {
	svc, routeStore, _ := newVendorConnectTestService(t)
	ctx := context.Background()
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorOpenAI, "ChatGPT Plus")
	keyAcc := createTestVendorAccount(t, svc, ownerToken(), apiKeyAccountRequest("Key account"))
	setVendorSubscriptionsEnabled(t, svc, false)

	calls := map[string]func() error{
		"import": func() error {
			_, err := svc.ConnectVendorAccountImport(ctx, ownerToken(), acc.ID, ConnectVendorAccountImportRequest{AccessToken: connectTestAccess})
			return err
		},
		"begin": func() error {
			_, err := svc.BeginVendorAccountConnect(ctx, ownerToken(), acc.ID)
			return err
		},
		"complete": func() error {
			_, err := svc.CompleteVendorAccountConnect(ctx, ownerToken(), acc.ID, "a-code")
			return err
		},
		"device begin": func() error {
			_, _, err := svc.BeginVendorAccountDeviceConnect(ctx, ownerToken(), acc.ID)
			return err
		},
		"device poll": func() error {
			_, err := svc.PollVendorAccountDeviceConnect(ctx, ownerToken(), acc.ID)
			return err
		},
	}
	for label, call := range calls {
		if err := call(); !errors.Is(err, ErrVendorSubscriptionsDisabled) {
			t.Errorf("%s while subscriptions are off: err = %v, want ErrVendorSubscriptionsDisabled", label, err)
		}
	}
	if row, err := routeStore.VendorAccountByID(ctx, acc.ID); err != nil || row.OAuthTokens != "" {
		t.Fatalf("account after refused connects = %#v, %v, want it untouched (no tokens stored)", row, err)
	}
	if pendingCount(svc) != 0 {
		t.Fatal("a refused begin stored a pending connect entry")
	}

	if _, err := svc.BeginVendorAccountConnect(ctx, otherToken(), acc.ID); !errors.Is(err, ErrVendorAccountNotFound) {
		t.Errorf("stranger begin: err = %v, want ErrVendorAccountNotFound (authorization comes first)", err)
	}
	if _, err := svc.BeginVendorAccountConnect(ctx, ownerToken(), "va_missing"); !errors.Is(err, ErrVendorAccountNotFound) {
		t.Errorf("unknown id begin: err = %v, want ErrVendorAccountNotFound", err)
	}
	if _, err := svc.BeginVendorAccountConnect(ctx, ownerToken(), keyAcc.ID); !errors.Is(err, ErrVendorAccountNotSubscription) {
		t.Errorf("api_key account begin: err = %v, want ErrVendorAccountNotSubscription", err)
	}

	setVendorSubscriptionsEnabled(t, svc, true)
	if _, err := svc.ConnectVendorAccountImport(ctx, ownerToken(), acc.ID, ConnectVendorAccountImportRequest{AccessToken: connectTestAccess}); err != nil {
		t.Fatalf("import after switching subscriptions back on: %v", err)
	}
}
