// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"reflect"
	"slices"
	"testing"
	"time"
)

// seedEmulationSubscriptionAccount creates an active OpenAI subscription account
// serving slugs (no prefix) and sets its decision-emulation opt-in.
func seedEmulationSubscriptionAccount(t *testing.T, store *MemoryStore, now time.Time, id string, emulation bool, slugs ...string) VendorAccount {
	t.Helper()
	ctx := context.Background()
	seedDecisionVendorAccount(t, store, now, id, VendorOpenAI, VendorAuthSubscription, "", slugs...)
	must(t, store.UpdateVendorAccountDecisionEmulation(ctx, id, emulation))
	acc, err := store.VendorAccountByID(ctx, id)
	must(t, err)
	return acc
}

// emulationToken is ownerToken with the token's decision-emulation opt-in set.
func emulationToken(on bool) auth.Token {
	tok := ownerToken()
	tok.DecisionsEmulation = on
	return tok
}

// assertEmulatedDecisionTarget checks the emulated shape: the ordinary OpenAI
// subscription target of acc (ChatGPT backend, OAuth at dispatch) with the
// decisions flavor, both decision modes emulate and the emulation mark, never
// the ADR-100 decision-model mark.
func assertEmulatedDecisionTarget(t *testing.T, target Target, accID string) {
	t.Helper()
	if target.Provider != ProviderVendorOpenAISubscription || target.Endpoint != "https://chatgpt.com/backend-api/codex" || target.VendorAccountID != accID || !target.Subscription {
		t.Fatalf("target = %+v, want the ChatGPT-backend subscription target of %s", target, accID)
	}
	if target.ProviderModel != vendorDecisionModel || target.APIToken != "" {
		t.Fatalf("ProviderModel/APIToken = %q/%q, want %q and no carried bearer", target.ProviderModel, target.APIToken, vendorDecisionModel)
	}
	if !slices.Contains(target.APIFlavors, APIFlavorDecisions) || !slices.Contains(target.APIFlavors, APIFlavorOpenAI) {
		t.Fatalf("APIFlavors = %v, want openai and decisions", target.APIFlavors)
	}
	if target.DecisionsMode != EndpointModeEmulate || target.SystemOneMode != EndpointModeEmulate {
		t.Fatalf("DecisionsMode/SystemOneMode = %q/%q, want emulate/emulate", target.DecisionsMode, target.SystemOneMode)
	}
	if !target.DecisionEmulated {
		t.Fatal("DecisionEmulated = false, want true")
	}
	if target.VendorDecisionModel {
		t.Fatal("VendorDecisionModel = true, want false (no native decision verdict on an emulated target)")
	}
}

// A decision request for gpt-6-luna on an OpenAI subscription account is served
// as an emulated target only when the account AND the token opt in; with either
// off nothing routes (the existing no-route error).
func TestDecisionEmulationNeedsBothOptIns(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name         string
		account, tok bool
		served       bool
	}{
		{"account on, token on", true, true, true},
		{"account on, token off", true, false, false},
		{"account off, token on", false, true, false},
		{"account off, token off", false, false, false},
	} {
		store := NewMemoryStore()
		seedEmulationSubscriptionAccount(t, store, now, "acc_sub", tc.account, "gpt-5", vendorDecisionModel)
		for _, mode := range []string{vendorRoutingModeVendorFirst, vendorRoutingModeFallbackOnly} {
			resolver := vendorResolver(store, now, true, mode)
			for _, flavor := range decisionFlavors {
				t.Run(tc.name+"/"+mode+"/"+flavor, func(t *testing.T) {
					target, err := resolver.Resolve(ctx, emulationToken(tc.tok), decisionRequest(vendorDecisionModel, flavor))
					if !tc.served {
						if !errors.Is(err, ErrNoModelRoute) {
							t.Fatalf("Resolve = %+v, %v; want ErrNoModelRoute", target, err)
						}
						return
					}
					if err != nil {
						t.Fatalf("Resolve = %v, want the emulated target", err)
					}
					assertEmulatedDecisionTarget(t, target, "acc_sub")
				})
			}
		}
	}
}

// Only the decision model is emulated: a decision request for another row of
// the opted-in account is not served.
func TestDecisionEmulationOnlyForTheDecisionModel(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	seedEmulationSubscriptionAccount(t, store, now, "acc_sub", true, "gpt-5", vendorDecisionModel)
	resolver := vendorResolver(store, now, true, vendorRoutingModeVendorFirst)
	for _, flavor := range decisionFlavors {
		if target, err := resolver.Resolve(ctx, emulationToken(true), decisionRequest("gpt-5", flavor)); !errors.Is(err, ErrNoModelRoute) {
			t.Fatalf("Resolve(%s, gpt-5) = %+v, %v; want ErrNoModelRoute", flavor, target, err)
		}
	}
}

// The vendor_subscriptions_enabled switch still applies: with it off, an
// opted-in subscription account serves no decision request either.
func TestDecisionEmulationHonoursTheSubscriptionSwitch(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	seedEmulationSubscriptionAccount(t, store, now, "acc_sub", true, vendorDecisionModel)
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetVendorAccountAccessors(func() bool { return true }, func() string { return vendorRoutingModeVendorFirst }, func() bool { return false })
	for _, flavor := range decisionFlavors {
		if target, err := resolver.Resolve(ctx, emulationToken(true), decisionRequest(vendorDecisionModel, flavor)); !errors.Is(err, ErrNoModelRoute) {
			t.Fatalf("Resolve(%s) = %+v, %v; want ErrNoModelRoute with subscriptions off", flavor, target, err)
		}
	}
}

// A chat request for gpt-6-luna on the opted-in account builds today's chat
// target exactly, with both opt-ins on.
func TestDecisionEmulationLeavesTheChatTargetUnchanged(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	acc := seedEmulationSubscriptionAccount(t, store, now, "acc_sub", true, vendorDecisionModel)
	resolver := vendorResolver(store, now, true, vendorRoutingModeVendorFirst)
	m := VendorAccountModel{AccountID: "acc_sub", GatewayModel: vendorDecisionModel, UpstreamModel: vendorDecisionModel, APIFlavor: APIFlavorOpenAI}
	for _, fine := range []string{"openai_chat_completions", "openai_responses"} {
		target, err := resolver.Resolve(ctx, emulationToken(true), inference.Request{Model: vendorDecisionModel, APIFlavor: fine})
		if err != nil {
			t.Fatalf("Resolve(%s) = %v, want the chat subscription target", fine, err)
		}
		want := vendorSubscriptionOpenAITarget(acc, m, vendorDecisionModel, NormalizeAPIFlavor(fine), fine)
		if !reflect.DeepEqual(target, want) {
			t.Fatalf("Resolve(%s) = %+v\nwant today's target %+v", fine, target, want)
		}
		if target.DecisionEmulated || target.DecisionsMode != "" || target.SystemOneMode != "" {
			t.Fatalf("Resolve(%s) = %+v, want no decision route on a chat target", fine, target)
		}
	}
}

// The ADR-100 api-key path is untouched by the opt-ins: an OpenAI api-key
// account (even with the account flag set) answers with the native decision
// target, never an emulated one.
func TestDecisionEmulationLeavesTheAPIKeyPathUnchanged(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	seedDecisionVendorAccount(t, store, now, "acc_openai", VendorOpenAI, VendorAuthAPIKey, "", vendorDecisionModel)
	must(t, store.UpdateVendorAccountDecisionEmulation(ctx, "acc_openai", true))
	resolver := vendorResolver(store, now, true, vendorRoutingModeVendorFirst)
	for _, on := range []bool{false, true} {
		for _, flavor := range decisionFlavors {
			target, err := resolver.Resolve(ctx, emulationToken(on), decisionRequest(vendorDecisionModel, flavor))
			if err != nil {
				t.Fatalf("Resolve(%s, token %v) = %v", flavor, on, err)
			}
			if target.DecisionEmulated || !target.VendorDecisionModel || target.DecisionsMode != EndpointModePassthrough || target.SystemOneMode != EndpointModeTranslate || target.Subscription {
				t.Fatalf("Resolve(%s, token %v) = %+v, want the ADR-100 api-key decision target", flavor, on, target)
			}
		}
	}
}

// isVendorDecisionEmulationAccount: an OpenAI subscription account with the
// account opt-in, asked by a token with the token opt-in. Nothing else.
func TestIsVendorDecisionEmulationAccount(t *testing.T) {
	acc := func(vendor, authType string, on bool) VendorAccount {
		return VendorAccount{ID: "acc", Vendor: vendor, AuthType: authType, Status: VendorAccountStatusActive, DecisionEmulation: on}
	}
	for _, tc := range []struct {
		name string
		acc  VendorAccount
		tok  bool
		want bool
	}{
		{"openai subscription, both on", acc(VendorOpenAI, VendorAuthSubscription, true), true, true},
		{"openai subscription, token off", acc(VendorOpenAI, VendorAuthSubscription, true), false, false},
		{"openai subscription, account off", acc(VendorOpenAI, VendorAuthSubscription, false), true, false},
		{"openai api key, both on", acc(VendorOpenAI, VendorAuthAPIKey, true), true, false},
		{"anthropic subscription, both on", acc(VendorAnthropic, VendorAuthSubscription, true), true, false},
		{"xai preset, both on", acc(VendorXAI, VendorAuthAPIKey, true), true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isVendorDecisionEmulationAccount(tc.acc, emulationToken(tc.tok)); got != tc.want {
				t.Fatalf("isVendorDecisionEmulationAccount = %v, want %v", got, tc.want)
			}
		})
	}
}

// With either opt-in off, the per-account gate turns a subscription account
// away for a decision request before any model row is read.
func TestDecisionEmulationGateRunsBeforeTheModelRead(t *testing.T) {
	r := vendorResolver(NewMemoryStore(), time.Now(), true, vendorRoutingModeVendorFirst)
	for _, tc := range []struct {
		account, tok, want bool
	}{{true, true, true}, {true, false, false}, {false, true, false}, {false, false, false}} {
		acc := VendorAccount{ID: "acc_sub", Vendor: VendorOpenAI, AuthType: VendorAuthSubscription, Status: VendorAccountStatusActive, DecisionEmulation: tc.account}
		if got := r.vendorAccountUsable(acc, emulationToken(tc.tok), APIFlavorDecisions, true); got != tc.want {
			t.Fatalf("vendorAccountUsable(account %v, token %v) = %v, want %v", tc.account, tc.tok, got, tc.want)
		}
	}
	// A non-decision request on the decisions flavor still never reaches a
	// subscription account (the flavor guard).
	acc := VendorAccount{ID: "acc_sub", Vendor: VendorOpenAI, AuthType: VendorAuthSubscription, Status: VendorAccountStatusActive, DecisionEmulation: true}
	if r.vendorAccountUsable(acc, emulationToken(true), APIFlavorDecisions, false) {
		t.Fatal("vendorAccountUsable admitted a non-decision decisions-flavor request on a subscription account")
	}
}

// The group path honours the same rule: an OpenAI group whose member is the
// opted-in subscription account serves the emulated target only with both
// opt-ins.
func TestDecisionEmulationThroughAVendorGroup(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name         string
		account, tok bool
		served       bool
	}{
		{"both on", true, true, true},
		{"token off", true, false, false},
		{"account off", false, true, false},
		{"both off", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMemoryStore()
			seedEmulationSubscriptionAccount(t, store, now, "acc_sub", tc.account, vendorDecisionModel)
			must(t, store.CreateVendorAccountGroup(ctx, VendorAccountGroup{
				ID: "vag_oai", OwnerUserID: vendorOwner, Vendor: VendorOpenAI, Name: "vag_oai", ModelPrefix: "grp/",
				Status:               VendorAccountGroupStatusActive,
				ThresholdFiveHourPct: 99, ThresholdWeeklyPct: 99, ThresholdCreditsPct: 99,
				CreatedAt: now, UpdatedAt: now,
			}))
			must(t, store.SetVendorAccountGroupMembers(ctx, "vag_oai", []string{"acc_sub"}))
			resolver := vendorResolver(store, now, true, vendorRoutingModeVendorFirst)
			tok := emulationToken(tc.tok)
			tok.VendorAccess = auth.VendorAccess{All: true, IncludeGroups: true}
			for _, flavor := range decisionFlavors {
				target, err := resolver.Resolve(ctx, tok, decisionRequest("grp/"+vendorDecisionModel, flavor))
				if !tc.served {
					if !errors.Is(err, ErrNoModelRoute) {
						t.Fatalf("Resolve(%s) = %+v, %v; want ErrNoModelRoute", flavor, target, err)
					}
					continue
				}
				if err != nil {
					t.Fatalf("Resolve(%s) = %v, want the emulated group target", flavor, err)
				}
				assertEmulatedDecisionTarget(t, target, "acc_sub")
				if target.VendorGroupID != "vag_oai" {
					t.Fatalf("VendorGroupID = %q, want vag_oai", target.VendorGroupID)
				}
			}
		})
	}
}

// vendorDecisionEmulationTargetMayBeZero is the emulated decision target's
// shape: the OpenAI subscription set minus the decision modes (both emulate,
// the whole point), plus ResponsesMode (a decision request is no Responses
// request).
var vendorDecisionEmulationTargetMayBeZero = map[string]bool{
	"VendorGroupID":               true, // set only by the group resolver after it picks a member
	"VendorDecisionModel":         true, // no native decision verdict on an emulated target
	"VendorShareUsageWithMembers": true, // the owner opt-in; this fixture leaves it off
	"ServerID":                    true, // no on-prem server
	"APIToken":                    true, // bearer resolved at dispatch, not carried here
	"APITokenHeader":              true, // Authorization is set by the dispatch layer
	"ResponsesMode":               true, // zero == translate; a decision request is no Responses request
	"MessagesMode":                true, // no messages path on an OpenAI subscription
	"Masquerade":                  true, // no disguise on the ChatGPT backend
	"OpportunisticMetrics":        true, // no per-app toggle for a vendor
	"ResponsesLiveTimingsEnabled": true, // llama.cpp-only; N/A
	"LiveProgressSupport":         true, // mapping-persisted; a vendor carries none
	"LiveProgressSpecType":        true, // server_agent-only; N/A
	"InstanceKey":                 true, // a vendor target addresses no instance
	"InstanceCapable":             true, // a vendor target runs no instances
	"InstancePool":                true, // a vendor target is not selected from a pool
	"OpenAIPathPrefix":            true, // the ChatGPT backend is reached at a bare /responses
}

func TestDecisionEmulationTargetCompleteness(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	acc := VendorAccount{ID: "acc_sub", OwnerUserID: vendorOwner, Vendor: VendorOpenAI, AuthType: VendorAuthSubscription, Status: VendorAccountStatusActive, OAuthTokens: "enc:t", DecisionEmulation: true, CreatedAt: now, UpdatedAt: now}
	m := VendorAccountModel{AccountID: "acc_sub", GatewayModel: vendorDecisionModel, UpstreamModel: vendorDecisionModel, APIFlavor: APIFlavorOpenAI}
	target, ok := vendorAccountModelTarget(acc, m, emulationToken(true), decisionRequest(vendorDecisionModel, APIFlavorOpenAIDecisions), APIFlavorDecisions)
	if !ok {
		t.Fatal("vendorAccountModelTarget built no emulated decision target")
	}
	tv := reflect.ValueOf(target)
	tt := tv.Type()
	for i := 0; i < tt.NumField(); i++ {
		name := tt.Field(i).Name
		if !vendorDecisionEmulationTargetMayBeZero[name] && tv.Field(i).IsZero() {
			t.Fatalf("the emulated decision target left Target.%s zero; wire it or add it to vendorDecisionEmulationTargetMayBeZero with a reason", name)
		}
	}
	// The builder fails closed without the token opt-in, even past the gate.
	if _, ok := vendorAccountModelTarget(acc, m, emulationToken(false), decisionRequest(vendorDecisionModel, APIFlavorOpenAIDecisions), APIFlavorDecisions); ok {
		t.Fatal("vendorAccountModelTarget built an emulated target for a token without the opt-in")
	}
}

// MemoryStore.CreateVendorAccount drops a caller's DecisionEmulation, as the SQL
// INSERT (which omits the column) does: only
// UpdateVendorAccountDecisionEmulation turns the opt-in on.
func TestMemoryCreateVendorAccountIgnoresDecisionEmulation(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	must(t, store.CreateVendorAccount(ctx, VendorAccount{
		ID: "acc_sub", OwnerUserID: vendorOwner, Vendor: VendorOpenAI, AuthType: VendorAuthSubscription,
		Status: VendorAccountStatusActive, DecisionEmulation: true, CreatedAt: now, UpdatedAt: now,
	}))
	acc, err := store.VendorAccountByID(ctx, "acc_sub")
	must(t, err)
	if acc.DecisionEmulation {
		t.Fatal("CreateVendorAccount kept DecisionEmulation; a new account must start off")
	}
}
