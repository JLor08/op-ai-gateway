// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"reflect"
	"testing"
	"time"
)

// seedVendorAccount creates an active vendor account owned by ownerUserID and
// gives it one model row (gatewayModel → upstreamModel) in the given coarse
// flavor. Mirrors how portal.CreateVendorAccount + the catalog seed populate the
// store, but kept local so the resolver tests do not depend on the portal layer.
func seedVendorAccount(t *testing.T, store *MemoryStore, now time.Time, id, ownerUserID, vendor, sealedKey, gatewayModel, upstreamModel, flavor string) {
	t.Helper()
	ctx := context.Background()
	must(t, store.CreateVendorAccount(ctx, VendorAccount{
		ID: id, OwnerUserID: ownerUserID, Vendor: vendor, AuthType: VendorAuthAPIKey,
		Name: id, Status: VendorAccountStatusActive, APIKey: sealedKey,
		CreatedAt: now, UpdatedAt: now,
	}))
	must(t, store.SetVendorAccountModels(ctx, id, []VendorAccountModel{
		{AccountID: id, GatewayModel: gatewayModel, UpstreamModel: upstreamModel, APIFlavor: flavor},
	}))
}

// vendorResolver builds a Resolver over store with the vendor accessors wired to
// the given enabled flag and routing mode, so a test states precisely which gate
// state it exercises.
func vendorResolver(store resolverStore, now time.Time, enabled bool, mode string) *Resolver {
	r := NewResolver(store, func() time.Time { return now }, nil)
	r.SetVendorAccountAccessors(func() bool { return enabled }, func() string { return mode })
	return r
}

const (
	vendorOwner   = "usr_owner"
	vendorTokenID = "tok_owner"
	vendorKey     = "enc:owner-openai-key"
)

func ownerToken() auth.Token {
	return auth.Token{ID: vendorTokenID, UserID: vendorOwner, Active: true}
}

// TestVendorAccountOwnerResolvesToVendorTarget is the core proof: the OWNER's
// request for a model only their vendor account serves resolves to a vendor
// Target with the OpenAI provider/endpoint/auth, the sealed key carried verbatim,
// and the translate-both-flavors shape.
func TestVendorAccountOwnerResolvesToVendorTarget(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	seedVendorAccount(t, store, now, "acc_openai", vendorOwner, VendorOpenAI, vendorKey, "gpt-4o", "gpt-4o-2024", APIFlavorOpenAI)
	resolver := vendorResolver(store, now, true, vendorRoutingModeVendorFirst)

	target, err := resolver.Resolve(ctx, ownerToken(), inference.Request{Model: "gpt-4o", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve returned %v, want a vendor target", err)
	}
	if target.Provider != ProviderVendorOpenAI {
		t.Errorf("Provider = %q, want %q", target.Provider, ProviderVendorOpenAI)
	}
	if target.Endpoint != "https://api.openai.com" {
		t.Errorf("Endpoint = %q, want https://api.openai.com", target.Endpoint)
	}
	if target.ProviderModel != "gpt-4o-2024" {
		t.Errorf("ProviderModel = %q, want gpt-4o-2024 (the UpstreamModel)", target.ProviderModel)
	}
	if target.APIToken != vendorKey {
		t.Errorf("APIToken = %q, want the sealed account key %q", target.APIToken, vendorKey)
	}
	if target.APITokenHeader != "" {
		t.Errorf("APITokenHeader = %q, want \"\" (Authorization: Bearer default) for an OpenAI vendor", target.APITokenHeader)
	}
	if target.RouteID != "vendor:acc_openai:gpt-4o" {
		t.Errorf("RouteID = %q, want vendor:acc_openai:gpt-4o", target.RouteID)
	}
	// M6a: every vendor target carries its account id for usage attribution, but an
	// api-key target is NOT a subscription target -- its bearer rides in APIToken.
	if target.VendorAccountID != "acc_openai" {
		t.Errorf("VendorAccountID = %q, want acc_openai (usage attribution on every vendor target)", target.VendorAccountID)
	}
	if target.Subscription {
		t.Error("Subscription = true, want false for an api-key vendor target (no dispatch-time OAuth bearer)")
	}
}

// TestVendorAccountAnthropicTargetShape proves the native-Anthropic vendor kind
// gets its own provider, endpoint and x-api-key auth header.
func TestVendorAccountAnthropicTargetShape(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	seedVendorAccount(t, store, now, "acc_anthropic", vendorOwner, VendorAnthropic, "enc:owner-anthropic-key", "claude-sonnet", "claude-3-7-sonnet", APIFlavorAnthropic)
	resolver := vendorResolver(store, now, true, vendorRoutingModeVendorFirst)

	// Reach it over BOTH inbound dialects: the Target serves either via translate.
	for _, flavor := range []string{"anthropic_messages", "openai_chat"} {
		target, err := resolver.Resolve(ctx, ownerToken(), inference.Request{Model: "claude-sonnet", APIFlavor: flavor})
		if err != nil {
			t.Fatalf("Resolve(flavor=%q) returned %v, want a vendor target", flavor, err)
		}
		if target.Provider != ProviderVendorAnthropic {
			t.Errorf("flavor=%q: Provider = %q, want %q", flavor, target.Provider, ProviderVendorAnthropic)
		}
		if target.Endpoint != "https://api.anthropic.com" {
			t.Errorf("flavor=%q: Endpoint = %q, want https://api.anthropic.com", flavor, target.Endpoint)
		}
		if target.APITokenHeader != "x-api-key" {
			t.Errorf("flavor=%q: APITokenHeader = %q, want x-api-key", flavor, target.APITokenHeader)
		}
		if target.APIFlavor != NormalizeAPIFlavor(flavor) {
			t.Errorf("flavor=%q: Target.APIFlavor = %q, want the request's coarse flavor %q", flavor, target.APIFlavor, NormalizeAPIFlavor(flavor))
		}
	}
}

// TestVendorAccountDoesNotLeakAcrossPrincipals covers the two non-owner cases: a
// DIFFERENT user, and a SERVICE token (UserID==""). Neither may borrow the
// owner's account, so both get ErrNoModelRoute (no self-hosted route exists).
func TestVendorAccountDoesNotLeakAcrossPrincipals(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	seedVendorAccount(t, store, now, "acc_openai", vendorOwner, VendorOpenAI, vendorKey, "gpt-4o", "gpt-4o-2024", APIFlavorOpenAI)
	resolver := vendorResolver(store, now, true, vendorRoutingModeVendorFirst)

	cases := []struct {
		name  string
		token auth.Token
	}{
		{"other user", auth.Token{ID: "tok_other", UserID: "usr_other", Active: true}},
		{"service token (no UserID)", auth.Token{ID: "svc_tok", UserID: "", Active: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := resolver.Resolve(ctx, tc.token, inference.Request{Model: "gpt-4o", APIFlavor: "openai_chat"}); !errors.Is(err, ErrNoModelRoute) {
				t.Fatalf("Resolve for %s = %v, want ErrNoModelRoute (the owner's account must not be borrowed)", tc.name, err)
			}
		})
	}
}

// TestVendorAccountDisabledFlagSkipsBranch proves the master flag gates the whole
// branch: with vendorEnabled()==false the owner's own request falls through to
// ErrNoModelRoute, exactly as a resolver that never had the accessors wired.
func TestVendorAccountDisabledFlagSkipsBranch(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	seedVendorAccount(t, store, now, "acc_openai", vendorOwner, VendorOpenAI, vendorKey, "gpt-4o", "gpt-4o-2024", APIFlavorOpenAI)

	// Flag off via the accessor, and also the nil-accessor path (never wired).
	for _, name := range []string{"flag off", "accessors never wired"} {
		t.Run(name, func(t *testing.T) {
			resolver := NewResolver(store, func() time.Time { return now }, nil)
			if name == "flag off" {
				resolver.SetVendorAccountAccessors(func() bool { return false }, func() string { return vendorRoutingModeVendorFirst })
			}
			if _, err := resolver.Resolve(ctx, ownerToken(), inference.Request{Model: "gpt-4o", APIFlavor: "openai_chat"}); !errors.Is(err, ErrNoModelRoute) {
				t.Fatalf("Resolve = %v, want ErrNoModelRoute (vendor branch must be off)", err)
			}
		})
	}
}

// TestVendorAccountPrecedenceAgainstSelfHostedMapping pins the vendor_first vs
// fallback_only semantics against a SAME-NAMED self-hosted mapping, and the
// fallback case where no self-hosted route exists at all.
func TestVendorAccountPrecedenceAgainstSelfHostedMapping(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

	// A store with a self-hosted mapping for "shared-model" on srv_fast AND the
	// owner's vendor account serving the same name.
	withSelfHosted := func() *MemoryStore {
		store := NewMemoryStore()
		must(t, store.CreateAIServer(ctx, AIServer{ID: "srv_fast", Name: "srv_fast", Domain: "srv_fast.test", Status: ServerStatusActive, HealthStatus: HealthHealthy, CreatedAt: now, UpdatedAt: now}))
		must(t, store.CreateApplication(ctx, Application{ID: "app_fast", ServerID: "srv_fast", Type: ProviderMock, Port: 8000, Scheme: "http", APIFlavors: []string{APIFlavorOpenAI}, Priority: 10, Weight: 50, TimeoutMS: 30000, AffinityTTLSeconds: 1800, Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
		must(t, store.CreateMapping(ctx, ModelMapping{ID: "map_fast", ApplicationID: "app_fast", GatewayModelName: "shared-model", AppModelName: "self-hosted-7b", Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
		must(t, store.UpsertTelemetry(ctx, ServerTelemetry{ServerID: "srv_fast", ReportedAt: now, LatencyMS: 100, ProviderHealth: "{}", Capabilities: "{}", RawSummary: "{}", UpdatedAt: now}))
		seedVendorAccount(t, store, now, "acc_openai", vendorOwner, VendorOpenAI, vendorKey, "shared-model", "gpt-4o-2024", APIFlavorOpenAI)
		return store
	}

	t.Run("vendor_first wins over self-hosted", func(t *testing.T) {
		resolver := vendorResolver(withSelfHosted(), now, true, vendorRoutingModeVendorFirst)
		target, err := resolver.Resolve(ctx, ownerToken(), inference.Request{Model: "shared-model", APIFlavor: "openai_chat"})
		if err != nil {
			t.Fatalf("Resolve = %v", err)
		}
		if target.Provider != ProviderVendorOpenAI || target.ServerID != "" {
			t.Fatalf("target = %#v, want the vendor Target (vendor_first wins)", target)
		}
	})

	t.Run("fallback_only prefers self-hosted", func(t *testing.T) {
		resolver := vendorResolver(withSelfHosted(), now, true, vendorRoutingModeFallbackOnly)
		target, err := resolver.Resolve(ctx, ownerToken(), inference.Request{Model: "shared-model", APIFlavor: "openai_chat"})
		if err != nil {
			t.Fatalf("Resolve = %v", err)
		}
		if target.ServerID != "srv_fast" || target.RouteID != "map_fast" {
			t.Fatalf("target = %#v, want the self-hosted srv_fast/map_fast target (fallback_only prefers self-hosted)", target)
		}
	})

	t.Run("fallback_only uses vendor when no self-hosted route", func(t *testing.T) {
		store := NewMemoryStore()
		seedVendorAccount(t, store, now, "acc_openai", vendorOwner, VendorOpenAI, vendorKey, "shared-model", "gpt-4o-2024", APIFlavorOpenAI)
		resolver := vendorResolver(store, now, true, vendorRoutingModeFallbackOnly)
		target, err := resolver.Resolve(ctx, ownerToken(), inference.Request{Model: "shared-model", APIFlavor: "openai_chat"})
		if err != nil {
			t.Fatalf("Resolve = %v, want the vendor fallback target", err)
		}
		if target.Provider != ProviderVendorOpenAI {
			t.Fatalf("target = %#v, want the vendor Target (fallback when no self-hosted route)", target)
		}
	})
}

// TestVendorAccountSkippedForImagesCapabilityAndOverride proves the three request
// shapes the branch deliberately declines: an images request, a capability-gated
// request, and a server-override request.
func TestVendorAccountSkippedForImagesCapabilityAndOverride(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	seedVendorAccount(t, store, now, "acc_openai", vendorOwner, VendorOpenAI, vendorKey, "gpt-4o", "gpt-4o-2024", APIFlavorOpenAI)
	resolver := vendorResolver(store, now, true, vendorRoutingModeVendorFirst)

	t.Run("images request skips vendor", func(t *testing.T) {
		if _, err := resolver.Resolve(ctx, ownerToken(), inference.Request{Model: "gpt-4o", APIFlavor: "openai_images"}); !errors.Is(err, ErrNoModelRoute) {
			t.Fatalf("Resolve(images) = %v, want ErrNoModelRoute (vendor branch declines images)", err)
		}
	})

	t.Run("capability request skips vendor", func(t *testing.T) {
		if _, err := resolver.Resolve(ctx, ownerToken(), inference.Request{Model: "gpt-4o", APIFlavor: "openai_chat", RequiredCapabilities: []string{"vision"}}); !errors.Is(err, ErrNoModelRoute) {
			t.Fatalf("Resolve(capability) = %v, want ErrNoModelRoute (vendor branch declines capability-gated requests)", err)
		}
	})

	t.Run("server-override request never lands on vendor", func(t *testing.T) {
		// No self-hosted server offers the model, so the override path refuses with
		// its own sentinel — crucially NOT a vendor Target.
		_, err := resolver.Resolve(ctx, ownerToken(), inference.Request{Model: "gpt-4o", APIFlavor: "openai_chat", ServerOverrideID: "srv_missing"})
		if err == nil {
			t.Fatal("Resolve(server-override) returned a target, want an override refusal (never a vendor Target)")
		}
		if errors.Is(err, ErrNoModelRoute) {
			return // acceptable: no override server/model at all
		}
		if !errors.Is(err, ErrServerOverrideModelUnavailable) && !errors.Is(err, ErrServerOverrideServerUnavailable) {
			t.Fatalf("Resolve(server-override) = %v, want an override refusal", err)
		}
	})
}

// TestVendorAccountOpenAIAPIKeyResponsesResolvesToPassthroughTarget is the core
// proof of the api-key Responses passthrough: an OpenAI API-KEY account reached
// over the FINE openai_responses flavor resolves to a NATIVE-PASSTHROUGH target at
// api.openai.com (ResponsesMode passthrough, so the dispatch layer relays the
// inbound Responses body/SSE verbatim to /v1/responses instead of translating it
// to /v1/chat/completions). It is still an api-key target: the sealed key rides in
// APIToken, Subscription stays false, and the bare slug is the upstream model.
func TestVendorAccountOpenAIAPIKeyResponsesResolvesToPassthroughTarget(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	seedVendorAccount(t, store, now, "acc_openai", vendorOwner, VendorOpenAI, vendorKey, "gpt-4o", "gpt-4o-2024", APIFlavorOpenAI)
	resolver := vendorResolver(store, now, true, vendorRoutingModeVendorFirst)

	target, err := resolver.Resolve(ctx, ownerToken(), inference.Request{Model: "gpt-4o", APIFlavor: "openai_responses"})
	if err != nil {
		t.Fatalf("Resolve(openai_responses) = %v, want a passthrough target", err)
	}
	if target.Provider != ProviderVendorOpenAI {
		t.Errorf("Provider = %q, want %q", target.Provider, ProviderVendorOpenAI)
	}
	if target.Endpoint != "https://api.openai.com" {
		t.Errorf("Endpoint = %q, want https://api.openai.com", target.Endpoint)
	}
	if target.ResponsesMode != EndpointModePassthrough {
		t.Errorf("ResponsesMode = %q, want %q (lossless native passthrough to /v1/responses)", target.ResponsesMode, EndpointModePassthrough)
	}
	if target.Subscription {
		t.Error("Subscription = true, want false for an api-key target (the bearer rides in APIToken)")
	}
	if target.APIToken != vendorKey {
		t.Errorf("APIToken = %q, want the sealed account key %q", target.APIToken, vendorKey)
	}
	if target.ProviderModel != "gpt-4o-2024" {
		t.Errorf("ProviderModel = %q, want gpt-4o-2024 (the bare upstream slug)", target.ProviderModel)
	}
	if target.VendorAccountID != "acc_openai" {
		t.Errorf("VendorAccountID = %q, want acc_openai", target.VendorAccountID)
	}
	if target.MessagesMode != "" {
		t.Errorf("MessagesMode = %q, want zero (the api-key target has no messages passthrough)", target.MessagesMode)
	}
	if len(target.APIFlavors) != 2 || target.APIFlavors[0] != APIFlavorOpenAI || target.APIFlavors[1] != APIFlavorAnthropic {
		t.Errorf("APIFlavors = %v, want [openai anthropic] unchanged", target.APIFlavors)
	}
}

// TestVendorAccountAPIKeyTranslateFlavorsLeaveResponsesModeZero pins the scope of
// the passthrough: it is OpenAI api-key + openai_responses ONLY. Chat and
// anthropic_messages to the same OpenAI account stay TRANSLATE (ResponsesMode
// zero), and an ANTHROPIC api-key account stays translate for EVERY inbound
// dialect, openai_responses included (its upstream is /v1/messages, which has no
// Responses surface to pass through to).
func TestVendorAccountAPIKeyTranslateFlavorsLeaveResponsesModeZero(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name         string
		vendor       string
		model        string
		modelFlavor  string
		reqFlavor    string
		wantProvider string
	}{
		{"openai account, openai_chat", VendorOpenAI, "gpt-4o", APIFlavorOpenAI, "openai_chat", ProviderVendorOpenAI},
		{"openai account, openai_chat_completions", VendorOpenAI, "gpt-4o", APIFlavorOpenAI, "openai_chat_completions", ProviderVendorOpenAI},
		{"openai account, anthropic_messages", VendorOpenAI, "gpt-4o", APIFlavorOpenAI, "anthropic_messages", ProviderVendorOpenAI},
		{"anthropic account, openai_responses", VendorAnthropic, "claude-sonnet", APIFlavorAnthropic, "openai_responses", ProviderVendorAnthropic},
		{"anthropic account, openai_chat", VendorAnthropic, "claude-sonnet", APIFlavorAnthropic, "openai_chat", ProviderVendorAnthropic},
		{"anthropic account, anthropic_messages", VendorAnthropic, "claude-sonnet", APIFlavorAnthropic, "anthropic_messages", ProviderVendorAnthropic},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewMemoryStore()
			seedVendorAccount(t, store, now, "acc_key", vendorOwner, tc.vendor, vendorKey, tc.model, tc.model+"-upstream", tc.modelFlavor)
			resolver := vendorResolver(store, now, true, vendorRoutingModeVendorFirst)

			target, err := resolver.Resolve(ctx, ownerToken(), inference.Request{Model: tc.model, APIFlavor: tc.reqFlavor})
			if err != nil {
				t.Fatalf("Resolve(%s) = %v, want a vendor target", tc.reqFlavor, err)
			}
			if target.Provider != tc.wantProvider {
				t.Errorf("Provider = %q, want %q", target.Provider, tc.wantProvider)
			}
			if target.ResponsesMode != "" {
				t.Errorf("ResponsesMode = %q, want zero (translate) for %s", target.ResponsesMode, tc.name)
			}
			if target.Subscription {
				t.Error("Subscription = true, want false for an api-key target")
			}
		})
	}
}

// vendorTargetMayBeZero names the Target fields the hand-built vendor Target
// legitimately leaves at their zero value, each with a reason — the vendor
// analogue of targetFromMayLeaveZero. The assertion below fails on any OTHER
// zero field, so a field added to Target that a vendor Target should carry
// cannot be silently forgotten here. It describes the openai_responses
// (passthrough) shape, so ResponsesMode is deliberately absent.
var vendorTargetMayBeZero = map[string]bool{
	"ServerID":                    true, // a vendor target has no on-prem server
	"MessagesMode":                true, // zero == translate
	"OpportunisticMetrics":        true, // no per-app opportunistic-metrics toggle for a vendor
	"ResponsesLiveTimingsEnabled": true, // llama.cpp-only timings injection; N/A for a vendor
	"LiveProgressSupport":         true, // mapping-persisted verdict; a vendor carries none
	"LiveProgressSpecType":        true, // server_agent-only; N/A for a vendor
	"APITokenHeader":              true, // "" for an OpenAI vendor (Bearer default); set for Anthropic
	"ExtraHeaders":                true, // an API-KEY vendor target needs no static extra headers (subscription-only)
	"Masquerade":                  true, // no Claude-Code disguise on the API-KEY path (subscription-only)
	"Subscription":                true, // false on the API-KEY path -- its bearer rides in APIToken, not resolved at dispatch
}

// TestVendorAccountTargetCompleteness is the vendor-Target analogue of
// TestTargetFromPopulatesEveryField: it reflects over the built Target and
// requires every field not named in vendorTargetMayBeZero to be non-zero, so a
// new Target field that a vendor target should populate is caught here too.
func TestVendorAccountTargetCompleteness(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	acc := VendorAccount{ID: "acc_openai", OwnerUserID: vendorOwner, Vendor: VendorOpenAI, AuthType: VendorAuthAPIKey, Status: VendorAccountStatusActive, APIKey: vendorKey, CreatedAt: now, UpdatedAt: now}
	m := VendorAccountModel{AccountID: "acc_openai", GatewayModel: "gpt-4o", UpstreamModel: "gpt-4o-2024", APIFlavor: APIFlavorOpenAI}
	// The openai_responses (passthrough) shape: the one api-key OpenAI target whose
	// ResponsesMode is non-zero (lossless native passthrough), so ResponsesMode is
	// NOT in vendorTargetMayBeZero. The translate shapes leave it zero by design and
	// are pinned in TestVendorAccountAPIKeyTranslateFlavorsLeaveResponsesModeZero.
	target := vendorAccountTarget(acc, m, "gpt-4o", APIFlavorOpenAI, "openai_responses")

	tv := reflect.ValueOf(target)
	tt := tv.Type()
	for i := 0; i < tt.NumField(); i++ {
		name := tt.Field(i).Name
		if vendorTargetMayBeZero[name] {
			continue
		}
		if tv.Field(i).IsZero() {
			t.Fatalf("vendorAccountTarget left Target.%s at its zero value. If a vendor Target should carry it, wire it in vendorAccountTarget (internal/routing/resolver.go); if it is legitimately zero, add %q to vendorTargetMayBeZero with a reason.", name, name)
		}
	}
}
