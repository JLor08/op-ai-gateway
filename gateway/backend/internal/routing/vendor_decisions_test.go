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

// vendorDecisionModel is the one OpenAI Decisions model (ADR-100).
const vendorDecisionModel = "gpt-6-luna"

// seedDecisionVendorAccount creates an active vendor account of the given vendor
// and auth type, served under prefix, with one model row per slug (the stored
// shape: GatewayModel = prefix + slug). A preset vendor gets a root URL, as
// create-time validation guarantees.
func seedDecisionVendorAccount(t *testing.T, store *MemoryStore, now time.Time, id, vendor, authType, prefix string, slugs ...string) {
	t.Helper()
	ctx := context.Background()
	acc := VendorAccount{
		ID: id, OwnerUserID: vendorOwner, Vendor: vendor, AuthType: authType,
		Name: id, Status: VendorAccountStatusActive, ModelPrefix: prefix,
		CreatedAt: now, UpdatedAt: now,
	}
	if authType == VendorAuthSubscription {
		acc.OAuthTokens = "enc:tokens"
	} else {
		acc.APIKey = vendorKey
	}
	if IsPresetVendor(vendor) {
		acc.BaseURL = "https://compat.example.test"
	}
	must(t, store.CreateVendorAccount(ctx, acc))
	rows := make([]VendorAccountModel, 0, len(slugs))
	for _, slug := range slugs {
		rows = append(rows, VendorAccountModel{AccountID: id, GatewayModel: prefix + slug, UpstreamModel: slug, APIFlavor: APIFlavorOpenAI})
	}
	must(t, store.SetVendorAccountModels(ctx, id, rows))
}

// decisionRequest is the request each decision endpoint hands Resolve: its
// flavor (openai_decisions for /v1/decisions, the coarse decisions for
// /v1/systemone) and the derived [any_decision] requirement.
func decisionRequest(model, flavor string) inference.Request {
	return inference.Request{Model: model, APIFlavor: flavor, RequiredCapabilities: []string{CapabilityAnyDecision}}
}

// decisionFlavors are the request flavors of the two decision endpoints.
var decisionFlavors = []string{APIFlavorOpenAIDecisions, APIFlavorDecisions}

func TestOpenAIDecisionModelsIsTheGAAllowlist(t *testing.T) {
	if got := OpenAIDecisionModelIDs(); !reflect.DeepEqual(got, []string{vendorDecisionModel}) {
		t.Fatalf("OpenAIDecisionModelIDs = %v, want exactly [%s]", got, vendorDecisionModel)
	}
	for id, want := range map[string]bool{vendorDecisionModel: true, "GPT-6-LUNA": false, "oa/" + vendorDecisionModel: false, "gpt-5": false, "": false} {
		if got := IsOpenAIDecisionModel(id); got != want {
			t.Errorf("IsOpenAIDecisionModel(%q) = %v, want %v", id, got, want)
		}
	}
	// The ids are a fresh slice: changing it changes nothing.
	ids := OpenAIDecisionModelIDs()
	ids[0] = "gpt-5"
	if IsOpenAIDecisionModel("gpt-5") || !IsOpenAIDecisionModel(vendorDecisionModel) {
		t.Fatal("modifying OpenAIDecisionModelIDs' result changed the allowlist")
	}
}

func TestIsVendorDecisionModel(t *testing.T) {
	model := func(gateway, upstream string) VendorAccountModel {
		return VendorAccountModel{GatewayModel: gateway, UpstreamModel: upstream, APIFlavor: APIFlavorOpenAI}
	}
	acc := func(vendor, authType string) VendorAccount {
		return VendorAccount{ID: "acc", Vendor: vendor, AuthType: authType, Status: VendorAccountStatusActive}
	}
	cases := []struct {
		name string
		acc  VendorAccount
		m    VendorAccountModel
		want bool
	}{
		{"openai api key, the decision model", acc(VendorOpenAI, VendorAuthAPIKey), model(vendorDecisionModel, vendorDecisionModel), true},
		{"openai api key, prefixed gateway name", acc(VendorOpenAI, VendorAuthAPIKey), model("oa/"+vendorDecisionModel, vendorDecisionModel), true},
		{"openai api key, a chat model", acc(VendorOpenAI, VendorAuthAPIKey), model("gpt-5", "gpt-5"), false},
		{"openai api key, the name only as gateway name", acc(VendorOpenAI, VendorAuthAPIKey), model(vendorDecisionModel, "gpt-5"), false},
		{"openai api key, a prefixed upstream name", acc(VendorOpenAI, VendorAuthAPIKey), model("oa/"+vendorDecisionModel, "oa/"+vendorDecisionModel), false},
		{"openai api key, case differs", acc(VendorOpenAI, VendorAuthAPIKey), model("GPT-6-LUNA", "GPT-6-LUNA"), false},
		{"openai subscription", acc(VendorOpenAI, VendorAuthSubscription), model(vendorDecisionModel, vendorDecisionModel), false},
		{"anthropic api key", acc(VendorAnthropic, VendorAuthAPIKey), model(vendorDecisionModel, vendorDecisionModel), false},
		{"anthropic subscription", acc(VendorAnthropic, VendorAuthSubscription), model(vendorDecisionModel, vendorDecisionModel), false},
		{"xai preset", acc(VendorXAI, VendorAuthAPIKey), model(vendorDecisionModel, vendorDecisionModel), false},
		{"openrouter preset", acc(VendorOpenRouter, VendorAuthAPIKey), model(vendorDecisionModel, vendorDecisionModel), false},
		{"google shim preset", acc(VendorGoogle, VendorAuthAPIKey), model(vendorDecisionModel, vendorDecisionModel), false},
		{"native gemini", acc(VendorGoogleNative, VendorAuthAPIKey), model(vendorDecisionModel, vendorDecisionModel), false},
		{"custom openai-compatible", acc(VendorOpenAICompatible, VendorAuthAPIKey), model(vendorDecisionModel, vendorDecisionModel), false},
		{"unknown auth type", acc(VendorOpenAI, ""), model(vendorDecisionModel, vendorDecisionModel), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsVendorDecisionModel(tc.acc, tc.m); got != tc.want {
				t.Fatalf("IsVendorDecisionModel = %v, want %v", got, tc.want)
			}
		})
	}
}

// A decision request (either endpoint's flavor, [any_decision]) for the decision
// model of an OpenAI api-key account the token may use resolves to the vendor
// target: the decisions flavor, /v1/decisions passed through, /v1/systemone
// translated, and the model marked as a vendor decision model.
func TestVendorDecisionRequestResolvesToTheOpenAIAPIKeyAccount(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	seedDecisionVendorAccount(t, store, now, "acc_openai", VendorOpenAI, VendorAuthAPIKey, "", "gpt-5", vendorDecisionModel)
	for _, mode := range []string{vendorRoutingModeVendorFirst, vendorRoutingModeFallbackOnly} {
		resolver := vendorResolver(store, now, true, mode)
		for _, flavor := range decisionFlavors {
			t.Run(mode+"/"+flavor, func(t *testing.T) {
				target, err := resolver.Resolve(ctx, ownerToken(), decisionRequest(vendorDecisionModel, flavor))
				if err != nil {
					t.Fatalf("Resolve = %v, want the vendor decision target", err)
				}
				if target.Provider != ProviderVendorOpenAI || target.Endpoint != "https://api.openai.com" || target.VendorAccountID != "acc_openai" || target.Subscription {
					t.Fatalf("target = %+v, want the api.openai.com api-key target of acc_openai", target)
				}
				if target.ProviderModel != vendorDecisionModel || target.APIToken != vendorKey {
					t.Fatalf("ProviderModel/APIToken = %q/%q, want %q and the sealed key", target.ProviderModel, target.APIToken, vendorDecisionModel)
				}
				if !slices.Contains(target.APIFlavors, APIFlavorDecisions) {
					t.Fatalf("APIFlavors = %v, want the decisions flavor", target.APIFlavors)
				}
				if target.DecisionsMode != EndpointModePassthrough || target.SystemOneMode != EndpointModeTranslate {
					t.Fatalf("DecisionsMode/SystemOneMode = %q/%q, want passthrough/translate", target.DecisionsMode, target.SystemOneMode)
				}
				if !target.VendorDecisionModel {
					t.Fatal("VendorDecisionModel = false, want true")
				}
				if target.EffectiveKind() != ProviderVendorOpenAI || !DecisionsPassthroughCapableKind(target.EffectiveKind()) {
					t.Fatalf("EffectiveKind = %q, want the passthrough-capable vendor_openai", target.EffectiveKind())
				}
			})
		}
	}
}

// A decision request for a model the account serves that is not a decision
// model never reaches the vendor.
func TestVendorDecisionRequestForANonDecisionModelIsNotRouted(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	seedDecisionVendorAccount(t, store, now, "acc_openai", VendorOpenAI, VendorAuthAPIKey, "", "gpt-5", vendorDecisionModel)
	resolver := vendorResolver(store, now, true, vendorRoutingModeVendorFirst)
	for _, flavor := range decisionFlavors {
		if _, err := resolver.Resolve(ctx, ownerToken(), decisionRequest("gpt-5", flavor)); !errors.Is(err, ErrNoModelRoute) {
			t.Fatalf("Resolve(%s, gpt-5) = %v, want ErrNoModelRoute", flavor, err)
		}
	}
}

// Only an OpenAI api-key account serves a decision request: an Anthropic,
// subscription, preset or OpenAI-compatible account with a gpt-6-luna row is
// never chosen, alone or ahead of the OpenAI api-key account.
func TestVendorDecisionRequestNeverPicksANonOpenAIAPIKeyAccount(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	others := []struct{ id, vendor, authType string }{
		{"acc_a_anthropic", VendorAnthropic, VendorAuthAPIKey},
		{"acc_b_anthropic_sub", VendorAnthropic, VendorAuthSubscription},
		{"acc_c_openai_sub", VendorOpenAI, VendorAuthSubscription},
		{"acc_d_xai", VendorXAI, VendorAuthAPIKey},
		{"acc_e_openrouter", VendorOpenRouter, VendorAuthAPIKey},
		{"acc_f_google", VendorGoogle, VendorAuthAPIKey},
		{"acc_g_gemini", VendorGoogleNative, VendorAuthAPIKey},
		{"acc_h_compat", VendorOpenAICompatible, VendorAuthAPIKey},
	}
	for _, o := range others {
		t.Run(o.id+" alone", func(t *testing.T) {
			store := NewMemoryStore()
			seedDecisionVendorAccount(t, store, now, o.id, o.vendor, o.authType, "", vendorDecisionModel)
			resolver := vendorResolver(store, now, true, vendorRoutingModeVendorFirst)
			// The control: the same row is vendor-routable for chat.
			if control, err := resolver.Resolve(ctx, ownerToken(), inference.Request{Model: vendorDecisionModel, APIFlavor: "openai_chat_completions"}); err != nil || control.VendorAccountID != o.id {
				t.Fatalf("control chat Resolve = %+v, %v; want the %s target", control, err, o.id)
			}
			for _, flavor := range decisionFlavors {
				if target, err := resolver.Resolve(ctx, ownerToken(), decisionRequest(vendorDecisionModel, flavor)); !errors.Is(err, ErrNoModelRoute) {
					t.Fatalf("Resolve(%s) = %+v, %v; want ErrNoModelRoute (never a %s account)", flavor, target, err, o.vendor)
				}
			}
		})
	}
	t.Run("ahead of the openai api-key account", func(t *testing.T) {
		store := NewMemoryStore()
		for _, o := range others {
			seedDecisionVendorAccount(t, store, now, o.id, o.vendor, o.authType, "", vendorDecisionModel)
		}
		seedDecisionVendorAccount(t, store, now, "acc_z_openai", VendorOpenAI, VendorAuthAPIKey, "", vendorDecisionModel)
		resolver := vendorResolver(store, now, true, vendorRoutingModeVendorFirst)
		for _, flavor := range decisionFlavors {
			target, err := resolver.Resolve(ctx, ownerToken(), decisionRequest(vendorDecisionModel, flavor))
			if err != nil || target.VendorAccountID != "acc_z_openai" {
				t.Fatalf("Resolve(%s) = %+v, %v; want the acc_z_openai target", flavor, target, err)
			}
		}
	})
}

// §11 access applies unchanged: a token without vendor access resolves nothing,
// and a per-token prefix override is the name the decision model is served under.
func TestVendorDecisionRequestHonoursTheTokenVendorAccess(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	seedDecisionVendorAccount(t, store, now, "acc_openai", VendorOpenAI, VendorAuthAPIKey, "oa/", vendorDecisionModel)
	resolver := vendorResolver(store, now, true, vendorRoutingModeVendorFirst)

	t.Run("no vendor access", func(t *testing.T) {
		tok := ownerToken()
		tok.VendorAccess = auth.VendorAccess{}
		for _, name := range []string{"oa/" + vendorDecisionModel, vendorDecisionModel} {
			if _, err := resolver.Resolve(ctx, tok, decisionRequest(name, APIFlavorOpenAIDecisions)); !errors.Is(err, ErrNoModelRoute) {
				t.Fatalf("Resolve(%s) = %v, want ErrNoModelRoute (strict opt-in)", name, err)
			}
		}
	})
	t.Run("native prefix", func(t *testing.T) {
		target, err := resolver.Resolve(ctx, ownerToken(), decisionRequest("oa/"+vendorDecisionModel, APIFlavorDecisions))
		if err != nil || target.VendorAccountID != "acc_openai" || target.ProviderModel != vendorDecisionModel {
			t.Fatalf("Resolve = %+v, %v; want acc_openai serving %s", target, err, vendorDecisionModel)
		}
		if _, err := resolver.Resolve(ctx, ownerToken(), decisionRequest(vendorDecisionModel, APIFlavorDecisions)); !errors.Is(err, ErrNoModelRoute) {
			t.Fatalf("Resolve(bare name) = %v, want ErrNoModelRoute under the native prefix", err)
		}
	})
	t.Run("prefix override", func(t *testing.T) {
		tok := ownerToken()
		tok.VendorAccess = auth.VendorAccess{Accounts: []auth.VendorAccessEntry{{AccountID: "acc_openai", OverrideEnabled: true, OverridePrefix: "dx/"}}}
		target, err := resolver.Resolve(ctx, tok, decisionRequest("dx/"+vendorDecisionModel, APIFlavorOpenAIDecisions))
		if err != nil || target.VendorAccountID != "acc_openai" || target.ProviderModel != vendorDecisionModel || target.Model != "dx/"+vendorDecisionModel {
			t.Fatalf("Resolve = %+v, %v; want acc_openai under the override name", target, err)
		}
		if _, err := resolver.Resolve(ctx, tok, decisionRequest("oa/"+vendorDecisionModel, APIFlavorOpenAIDecisions)); !errors.Is(err, ErrNoModelRoute) {
			t.Fatalf("Resolve(native name) = %v, want ErrNoModelRoute under the override", err)
		}
	})
	t.Run("flag off", func(t *testing.T) {
		off := vendorResolver(store, now, false, vendorRoutingModeVendorFirst)
		if _, err := off.Resolve(ctx, ownerToken(), decisionRequest("oa/"+vendorDecisionModel, APIFlavorOpenAIDecisions)); !errors.Is(err, ErrNoModelRoute) {
			t.Fatalf("Resolve = %v, want ErrNoModelRoute with vendor accounts off", err)
		}
	})
}

// A chat request for the decision model keeps today's target exactly: the
// decision opt-in changes nothing outside a decision request.
func TestVendorChatRequestForTheDecisionModelIsUnchanged(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	seedDecisionVendorAccount(t, store, now, "acc_openai", VendorOpenAI, VendorAuthAPIKey, "", vendorDecisionModel)
	resolver := vendorResolver(store, now, true, vendorRoutingModeVendorFirst)
	acc, err := store.VendorAccountByID(ctx, "acc_openai")
	must(t, err)
	m := VendorAccountModel{AccountID: "acc_openai", GatewayModel: vendorDecisionModel, UpstreamModel: vendorDecisionModel, APIFlavor: APIFlavorOpenAI}
	for _, fine := range []string{"openai_chat_completions", "openai_responses", "anthropic_messages"} {
		target, err := resolver.Resolve(ctx, ownerToken(), inference.Request{Model: vendorDecisionModel, APIFlavor: fine})
		if err != nil {
			t.Fatalf("Resolve(%s) = %v, want the chat vendor target", fine, err)
		}
		want := vendorAccountTarget(acc, m, vendorDecisionModel, NormalizeAPIFlavor(fine), fine)
		if !reflect.DeepEqual(target, want) {
			t.Fatalf("Resolve(%s) = %+v\nwant today's target %+v", fine, target, want)
		}
		if target.VendorDecisionModel || target.DecisionsMode != "" || target.SystemOneMode != "" || slices.Contains(target.APIFlavors, APIFlavorDecisions) {
			t.Fatalf("Resolve(%s) = %+v, want no decision route on a chat target", fine, target)
		}
	}
}

// [any_decision] is the ONE capability list that opts into the vendor path, and
// only on a decision endpoint's flavor. Every other list stays self-hosted,
// even for the decision model of an OpenAI api-key account.
func TestVendorPathRefusesEveryOtherCapabilityList(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	seedDecisionVendorAccount(t, store, now, "acc_openai", VendorOpenAI, VendorAuthAPIKey, "", vendorDecisionModel)
	resolver := vendorResolver(store, now, true, vendorRoutingModeVendorFirst)
	cases := []struct {
		name   string
		flavor string
		caps   []string
	}{
		{"speech", "openai_audio_speech", []string{CapabilitySpeech}},
		{"transcription", "openai_audio_transcriptions", []string{CapabilityTranscription}},
		{"image", "openai_images", []string{CapabilityImage}},
		{"image edit", "openai_images_edits", []string{CapabilityImageEdit}},
		{"vision on chat", "openai_chat_completions", []string{CapabilityVision}},
		{"decision", APIFlavorDecisions, []string{CapabilityDecision}},
		{"decision_openai", APIFlavorOpenAIDecisions, []string{CapabilityDecisionOpenAI}},
		{"any_decision plus vision", APIFlavorOpenAIDecisions, []string{CapabilityAnyDecision, CapabilityVision}},
		{"any_decision twice", APIFlavorOpenAIDecisions, []string{CapabilityAnyDecision, CapabilityAnyDecision}},
		{"any_decision on chat", "openai_chat_completions", []string{CapabilityAnyDecision}},
		{"any_decision on images", "openai_images", []string{CapabilityAnyDecision}},
		{"decisions flavor without the requirement", APIFlavorDecisions, nil},
		{"openai_decisions flavor without the requirement", APIFlavorOpenAIDecisions, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := inference.Request{Model: vendorDecisionModel, APIFlavor: tc.flavor, RequiredCapabilities: tc.caps}
			target, err := resolver.Resolve(ctx, ownerToken(), req)
			if err == nil || target.VendorAccountID != "" {
				t.Fatalf("Resolve = %+v, %v; want no vendor target", target, err)
			}
		})
	}
}

// A server-override decision request never lands on a vendor account.
func TestVendorDecisionRequestUnderAServerOverrideIsNotVendor(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	seedDecisionVendorAccount(t, store, now, "acc_openai", VendorOpenAI, VendorAuthAPIKey, "", vendorDecisionModel)
	resolver := vendorResolver(store, now, true, vendorRoutingModeVendorFirst)
	req := decisionRequest(vendorDecisionModel, APIFlavorOpenAIDecisions)
	req.ServerOverrideID = "srv_missing"
	if target, err := resolver.Resolve(ctx, ownerToken(), req); err == nil || target.VendorAccountID != "" {
		t.Fatalf("Resolve = %+v, %v; want an override refusal, never a vendor target", target, err)
	}
}

// A shared account serves a decision request only to a token whose policy names
// it: all:true covers a principal's OWN accounts and never borrows one, for a
// user and for a service token alike, and a service token routes only under its
// own named policy.
func TestVendorDecisionRequestOnASharedAccountNeedsThePerAccountGrant(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	seedDecisionVendorAccount(t, store, now, "acc_shared", VendorOpenAI, VendorAuthAPIKey, "", vendorDecisionModel) // owned by vendorOwner
	acc, err := store.VendorAccountByID(ctx, "acc_shared")
	must(t, err)
	resolver := vendorResolver(store, now, true, vendorRoutingModeVendorFirst)
	resolver.SetVendorAccountEnumerator(&fakeVendorEnumerator{accounts: []VendorAccount{acc}})

	allTrue := auth.VendorAccess{All: true}
	refused := map[string]auth.Token{
		"borrower with all:true":       {ID: "tok_b", UserID: sharingBorrower, Active: true, VendorAccess: allTrue},
		"borrower without access":      {ID: "tok_b", UserID: sharingBorrower, Active: true},
		"borrower naming another":      borrowerToken("acc_other"),
		"service token with all:true":  {ID: "tok_s", Kind: "service", ServiceID: sharingServiceA, Active: true, VendorAccess: allTrue},
		"service token without access": {ID: "tok_s", Kind: "service", ServiceID: sharingServiceA, Active: true},
	}
	for name, token := range refused {
		t.Run(name, func(t *testing.T) {
			for _, flavor := range decisionFlavors {
				if target, err := resolver.Resolve(ctx, token, decisionRequest(vendorDecisionModel, flavor)); !errors.Is(err, ErrNoModelRoute) {
					t.Fatalf("Resolve(%s) = %+v, %v; want ErrNoModelRoute without a per-account grant", flavor, target, err)
				}
			}
		})
	}
	granted := map[string]auth.Token{
		"borrower naming it":      borrowerToken("acc_shared"),
		"service token naming it": serviceToken(sharingServiceA, "acc_shared"),
	}
	for name, token := range granted {
		t.Run(name, func(t *testing.T) {
			for _, flavor := range decisionFlavors {
				target, err := resolver.Resolve(ctx, token, decisionRequest(vendorDecisionModel, flavor))
				if err != nil || target.VendorAccountID != "acc_shared" || !target.VendorDecisionModel {
					t.Fatalf("Resolve(%s) = %+v, %v; want the shared account's decision target", flavor, target, err)
				}
			}
		})
	}
}
