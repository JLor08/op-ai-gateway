// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"fmt"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"testing"
)

// offeringMapping describes one gateway model to seed: its name and the API
// flavors of the application that offers it. (Named offeringMapping rather
// than plain "mapping" because that word is already a local variable name all
// over this package.)
type offeringMapping struct {
	name    string
	flavors []string
}

// newOfferingTestService builds a Service over the memory routing store with
// one active server + application + mapping per given model, following the
// pattern of the existing offering tests (offerModel/offerSvc).
func newOfferingTestService(t *testing.T, mappings ...offeringMapping) (*Service, *routing.MemoryStore) {
	t.Helper()
	rs := routing.NewMemoryStore()
	for i, m := range mappings {
		srvID := fmt.Sprintf("srv_off_%d", i)
		appID := fmt.Sprintf("app_off_%d", i)
		offerModel(t, rs, srvID, fmt.Sprintf("OfferBox%d", i), appID, m.flavors, m.name, m.name+"-up", routing.ServerStatusActive)
	}
	return offerSvc(rs, nil), rs
}

// tokenWithRules returns a token carrying the given model-override rules. The
// rules are written in the store shape (the shape the token editor persists)
// and converted through the one bridge that exists for it.
func tokenWithRules(rules map[string]store.ModelOverrideRule) auth.Token {
	return auth.Token{UserID: "usr_off", ModelOverrideRules: store.AuthModelOverrideRules(rules)}
}

func TestOfferedAliasAppearsWithTargetFlavors(t *testing.T) {
	// An alias inherits its target's flavors: an Anthropic-only target must not
	// surface the alias in the OpenAI listing.
	ctx := context.Background()
	svc, _ := newOfferingTestService(t, offeringMapping{name: "qwen3-32b", flavors: []string{routing.APIFlavorAnthropic}})
	token := tokenWithRules(map[string]store.ModelOverrideRule{
		"claude-x": {To: "qwen3-32b", Offer: true},
	})
	if _, ok := svc.ModelOfferingFor(ctx, token, routing.APIFlavorAnthropic).Offered["claude-x"]; !ok {
		t.Fatal("alias missing from the anthropic listing")
	}
	if _, ok := svc.ModelOfferingFor(ctx, token, routing.APIFlavorOpenAI).Offered["claude-x"]; ok {
		t.Fatal("alias leaked into the openai listing")
	}
}

func TestUnofferedAliasStaysOut(t *testing.T) {
	ctx := context.Background()
	svc, _ := newOfferingTestService(t, offeringMapping{name: "qwen3-32b", flavors: []string{routing.APIFlavorOpenAI}})
	token := tokenWithRules(map[string]store.ModelOverrideRule{"gpt-4o": {To: "qwen3-32b"}})
	if _, ok := svc.ModelOfferingFor(ctx, token, routing.APIFlavorOpenAI).Offered["gpt-4o"]; ok {
		t.Fatal("alias offered although Offer is false")
	}
}

func TestHideTargetDropsTargetName(t *testing.T) {
	ctx := context.Background()
	svc, _ := newOfferingTestService(t, offeringMapping{name: "qwen3-32b", flavors: []string{routing.APIFlavorOpenAI}})
	token := tokenWithRules(map[string]store.ModelOverrideRule{
		"gpt-4o": {To: "qwen3-32b", Offer: true, HideTarget: true},
	})
	off := svc.ModelOfferingFor(ctx, token, routing.APIFlavorOpenAI)
	if _, ok := off.Offered["qwen3-32b"]; ok {
		t.Fatal("hidden target still listed")
	}
	// Hiding is a listing concern only: the target still EXISTS, so the redirect
	// in Task 5 must not treat it as unknown.
	if _, ok := off.Existing["qwen3-32b"]; !ok {
		t.Fatal("hidden target dropped from Existing")
	}
	// The alias itself is still offered.
	if _, ok := off.Offered["gpt-4o"]; !ok {
		t.Fatal("alias missing although Offer is set")
	}
}

func TestHideTargetWinsOverASecondRow(t *testing.T) {
	// Two rows onto one target, only one hiding it: a set switch is an
	// instruction, an unset one only its absence — so the target is hidden.
	ctx := context.Background()
	svc, _ := newOfferingTestService(t, offeringMapping{name: "qwen3-32b", flavors: []string{routing.APIFlavorOpenAI}})
	token := tokenWithRules(map[string]store.ModelOverrideRule{
		"a": {To: "qwen3-32b", Offer: true, HideTarget: true},
		"b": {To: "qwen3-32b", Offer: true},
	})
	if _, ok := svc.ModelOfferingFor(ctx, token, routing.APIFlavorOpenAI).Offered["qwen3-32b"]; ok {
		t.Fatal("target visible although one row hides it")
	}
}

// TestAliasToUnknownTargetIsNotOffered: a rule pointing at a model that does
// not exist would advertise a dead name — it is skipped entirely, and its
// HideTarget cannot remove anything either.
func TestAliasToUnknownTargetIsNotOffered(t *testing.T) {
	ctx := context.Background()
	svc, _ := newOfferingTestService(t, offeringMapping{name: "qwen3-32b", flavors: []string{routing.APIFlavorOpenAI}})
	token := tokenWithRules(map[string]store.ModelOverrideRule{
		"ghost": {To: "does-not-exist", Offer: true, HideTarget: true},
	})
	off := svc.ModelOfferingFor(ctx, token, routing.APIFlavorOpenAI)
	if _, ok := off.Offered["ghost"]; ok {
		t.Fatal("alias onto a non-existent target was offered")
	}
	if _, ok := off.Offered["qwen3-32b"]; !ok {
		t.Fatal("the real model disappeared from the listing")
	}
}

// TestOfferedAliasOfHiddenTargetIsListed: the alias is a DIFFERENT name that
// does not reveal the hidden one, so an explicitly offered alias survives its
// target's model_settings suppression — and inherits the target's flavors from
// the PRE-suppression set.
func TestOfferedAliasOfHiddenTargetIsListed(t *testing.T) {
	ctx := context.Background()
	svc, rs := newOfferingTestService(t, offeringMapping{name: "qwen3-32b", flavors: []string{routing.APIFlavorOpenAI}})
	offerVisibility(t, rs, "qwen3-32b", "hidden")
	token := tokenWithRules(map[string]store.ModelOverrideRule{
		"gpt-4o": {To: "qwen3-32b", Offer: true},
	})
	off := svc.ModelOfferingFor(ctx, token, routing.APIFlavorOpenAI)
	if _, ok := off.Offered["gpt-4o"]; !ok {
		t.Fatal("alias of a hidden target missing from the listing")
	}
	if _, ok := off.Offered["qwen3-32b"]; ok {
		t.Fatal("hidden target listed under its own name")
	}
	// Existing ignores visibility: the hidden model still exists.
	if _, ok := off.Existing["qwen3-32b"]; !ok {
		t.Fatal("hidden model missing from Existing")
	}
}

// TestOfferingExistingIncludesGroups: a model GROUP shares the model namespace
// and is therefore a name that exists. Guards against building Existing from a
// group overlay computed over an empty model map (which yields no groups at
// all, since a group without an offerable member is never offered).
func TestOfferingExistingIncludesGroups(t *testing.T) {
	ctx := context.Background()
	svc, rs := newOfferingTestService(t,
		offeringMapping{name: "m1", flavors: []string{routing.APIFlavorOpenAI}},
		offeringMapping{name: "m2", flavors: []string{routing.APIFlavorAnthropic}},
	)
	offerGroup(t, rs, "grp_off", "coder", "m1", "m2")

	openai := svc.ModelOfferingFor(ctx, auth.Token{UserID: "usr_off"}, routing.APIFlavorOpenAI)
	if _, ok := openai.Existing["coder"]; !ok {
		t.Fatalf("group missing from Existing(openai): %#v", openai.Existing)
	}
	anthropic := svc.ModelOfferingFor(ctx, auth.Token{UserID: "usr_off"}, routing.APIFlavorAnthropic)
	if _, ok := anthropic.Existing["coder"]; !ok {
		t.Fatalf("group missing from Existing(anthropic): %#v", anthropic.Existing)
	}
	// A hidden group is still a name that exists.
	offerVisibility(t, rs, "coder", "hidden")
	off := svc.ModelOfferingFor(ctx, auth.Token{UserID: "usr_off"}, routing.APIFlavorOpenAI)
	if _, ok := off.Existing["coder"]; !ok {
		t.Fatal("hidden group dropped from Existing")
	}
	if _, ok := off.Offered["coder"]; ok {
		t.Fatal("hidden group still offered")
	}
}

// TestOfferedAliasOntoAGroup: a group name is a valid override target, so an
// alias onto it inherits the group's flavor union.
func TestOfferedAliasOntoAGroup(t *testing.T) {
	ctx := context.Background()
	svc, rs := newOfferingTestService(t,
		offeringMapping{name: "m1", flavors: []string{routing.APIFlavorOpenAI}},
		offeringMapping{name: "m2", flavors: []string{routing.APIFlavorAnthropic}},
	)
	offerGroup(t, rs, "grp_off", "coder", "m1", "m2")
	token := tokenWithRules(map[string]store.ModelOverrideRule{
		"gpt-4o": {To: "coder", Offer: true, HideTarget: true},
	})
	for _, flavor := range []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic} {
		off := svc.ModelOfferingFor(ctx, token, flavor)
		if _, ok := off.Offered["gpt-4o"]; !ok {
			t.Fatalf("alias onto a group missing from the %s listing", flavor)
		}
		if _, ok := off.Offered["coder"]; ok {
			t.Fatalf("hidden group target still listed in the %s listing", flavor)
		}
	}
}

// TestOfferingWithoutRulesIsUnchanged: a token WITHOUT override rules gets
// exactly the listing it got before this change — ModelsForFlavor and
// ModelOfferingFor.Offered agree, and neither gained nor lost a name.
func TestOfferingWithoutRulesIsUnchanged(t *testing.T) {
	ctx := context.Background()
	svc, rs := newOfferingTestService(t,
		offeringMapping{name: "m1", flavors: []string{routing.APIFlavorOpenAI}},
		offeringMapping{name: "m2", flavors: []string{routing.APIFlavorAnthropic}},
		offeringMapping{name: "m3", flavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic}},
		offeringMapping{name: "m4", flavors: []string{routing.APIFlavorOpenAI}},
	)
	offerGroup(t, rs, "grp_off", "coder", "m1", "m2")
	offerVisibility(t, rs, "m4", "hidden")

	token := auth.Token{UserID: "usr_off"}
	want := map[string][]string{
		routing.APIFlavorOpenAI:    {"coder", "m1", "m3"},
		routing.APIFlavorAnthropic: {"coder", "m2", "m3"},
	}
	for flavor, expected := range want {
		got := svc.ModelsForFlavor(ctx, token, flavor)
		if len(got) != len(expected) {
			t.Fatalf("ModelsForFlavor(%s) = %#v, want %#v", flavor, got, expected)
		}
		for i, name := range expected {
			if got[i] != name {
				t.Fatalf("ModelsForFlavor(%s) = %#v, want %#v", flavor, got, expected)
			}
		}
		offered := svc.ModelOfferingFor(ctx, token, flavor).Offered
		if len(offered) != len(expected) {
			t.Fatalf("Offered(%s) = %#v, want %#v", flavor, offered, expected)
		}
		for _, name := range expected {
			if _, ok := offered[name]; !ok {
				t.Fatalf("Offered(%s) = %#v, want %#v", flavor, offered, expected)
			}
		}
	}
}

// TestOverrideAliasReachesModelsForFlavor: the overlay lives in
// modelFlavorSets, so the discovery listings (/v1/models, the Anthropic
// listing) pick it up without a change of their own.
func TestOverrideAliasReachesModelsForFlavor(t *testing.T) {
	ctx := context.Background()
	svc, _ := newOfferingTestService(t, offeringMapping{name: "qwen3-32b", flavors: []string{routing.APIFlavorOpenAI}})
	token := tokenWithRules(map[string]store.ModelOverrideRule{
		"gpt-4o": {To: "qwen3-32b", Offer: true, HideTarget: true},
	})
	got := svc.ModelsForFlavor(ctx, token, routing.APIFlavorOpenAI)
	if len(got) != 1 || got[0] != "gpt-4o" {
		t.Fatalf("ModelsForFlavor = %#v, want [gpt-4o]", got)
	}
}

// TestOfferingIsWhollyEmptyOnMappingStoreError: a store failure yields BOTH
// sets empty, never a half-built answer. A populated Offered next to an empty
// Existing is indistinguishable, to the caller, from "these names exist but
// none of them is real" — which is exactly the state that makes the Task-5
// redirect fire on every offered name.
func TestOfferingIsWhollyEmptyOnMappingStoreError(t *testing.T) {
	ctx := context.Background()
	rs := routing.NewMemoryStore()
	offerModel(t, rs, "srv_e", "BoxE", "app_e", []string{routing.APIFlavorOpenAI}, "qwen3-32b", "qwen-up", routing.ServerStatusActive)

	svc := offerSvc(gatewayAIServersErrStore{rs}, nil)
	off := svc.ModelOfferingFor(ctx, auth.Token{UserID: "usr_off"}, routing.APIFlavorOpenAI)
	if len(off.Offered) != 0 || len(off.Existing) != 0 {
		t.Fatalf("store error must yield two empty sets, got Offered=%#v Existing=%#v", off.Offered, off.Existing)
	}
}

// TestOfferingIsWhollyEmptyOnGroupOverlayError: the group overlay is where the
// group NAMES come from, so failing open there (as the LISTING does — see
// TestModelsOverlayFailOpen) would hand the redirect an Existing set missing
// every group, making a perfectly valid group name look unknown. This one
// caller therefore treats the overlay error as fatal and returns nothing.
func TestOfferingIsWhollyEmptyOnGroupOverlayError(t *testing.T) {
	ctx := context.Background()
	rs := routing.NewMemoryStore()
	offerModel(t, rs, "srv_e", "BoxE", "app_e", []string{routing.APIFlavorOpenAI}, "m1", "m1-up", routing.ServerStatusActive)
	offerGroup(t, rs, "grp_e", "coder", "m1")

	svc := offerSvc(groupErrStore{rs}, nil)
	off := svc.ModelOfferingFor(ctx, auth.Token{UserID: "usr_off"}, routing.APIFlavorOpenAI)
	if len(off.Offered) != 0 || len(off.Existing) != 0 {
		t.Fatalf("overlay error must yield two empty sets, got Offered=%#v Existing=%#v", off.Offered, off.Existing)
	}
	// The LISTING keeps its fail-open behaviour — this is a divergence of the
	// offering lookup only, not a change to what /v1/models serves.
	if !containsString(svc.ModelsForFlavor(ctx, auth.Token{UserID: "usr_off"}, routing.APIFlavorOpenAI), "m1") {
		t.Fatal("the listing must still fail open on an overlay error")
	}
}

// lateFailAIServersStore serves the first AIServers call and fails every one
// after it — a store that goes away mid-call, which is the only way a function
// doing TWO mapping traversals can produce a half-built answer.
type lateFailAIServersStore struct {
	*routing.MemoryStore
	calls int
}

func (l *lateFailAIServersStore) AIServers(ctx context.Context) ([]routing.AIServer, error) {
	l.calls++
	if l.calls > 1 {
		return nil, errors.New("store: AI-server list went away mid-call")
	}
	return l.MemoryStore.AIServers(ctx)
}

// TestOfferingNeverReturnsAPartialResult: with the store failing from the
// second traversal onwards, the answer must still be self-consistent — either
// wholly empty, or an Existing that accounts for every Offered name. A second
// traversal reintroduced into ModelOfferingFor fails here, because Offered
// would survive it and Existing would not.
func TestOfferingNeverReturnsAPartialResult(t *testing.T) {
	ctx := context.Background()
	rs := routing.NewMemoryStore()
	offerModel(t, rs, "srv_l", "BoxL", "app_l", []string{routing.APIFlavorOpenAI}, "m1", "m1-up", routing.ServerStatusActive)
	offerModel(t, rs, "srv_l2", "BoxL2", "app_l2", []string{routing.APIFlavorOpenAI}, "m2", "m2-up", routing.ServerStatusActive)

	svc := offerSvc(&lateFailAIServersStore{MemoryStore: rs}, nil)
	off := svc.ModelOfferingFor(ctx, auth.Token{UserID: "usr_off"}, routing.APIFlavorOpenAI)
	if len(off.Offered) == 0 {
		if len(off.Existing) != 0 {
			t.Fatalf("empty Offered beside a populated Existing: %#v", off.Existing)
		}
		return // wholly empty is the other acceptable answer
	}
	// This token has no override rules, so every offered name is a real model
	// and MUST be accounted for by Existing.
	for name := range off.Offered {
		if _, ok := off.Existing[name]; !ok {
			t.Fatalf("offered %q is missing from Existing (partial result): Offered=%#v Existing=%#v", name, off.Offered, off.Existing)
		}
	}
}

// countingRoutingStore counts the two store traversals ModelOfferingFor is at
// risk of doing twice: the server/application/mapping walk behind
// activeMappingViews, and the group overlay's own reads (ModelGroups plus one
// GroupMembersByGroup per group).
type countingRoutingStore struct {
	*routing.MemoryStore
	aiServers    int
	modelGroups  int
	groupMembers int
}

func (c *countingRoutingStore) AIServers(ctx context.Context) ([]routing.AIServer, error) {
	c.aiServers++
	return c.MemoryStore.AIServers(ctx)
}

func (c *countingRoutingStore) ModelGroups(ctx context.Context) ([]routing.ModelGroup, error) {
	c.modelGroups++
	return c.MemoryStore.ModelGroups(ctx)
}

func (c *countingRoutingStore) GroupMembersByGroup(ctx context.Context, id string) ([]routing.GroupMember, error) {
	c.groupMembers++
	return c.MemoryStore.GroupMembersByGroup(ctx, id)
}

// --- Task 4b: the same override-alias overlay applied to Models() / modelsResponse ---
//
// ModelOfferingFor (above) drives the per-flavor discovery listing
// (/v1/models, the Anthropic listing) via modelFlavorSets. Models() -- the
// LM Studio / chat-picker listing -- goes through modelsResponse instead,
// which builds its own flavor map from activeMappingViews rather than
// reusing modelFlavorSets, so it never picked up the override-alias overlay
// Task 4 built. These tests close that gap; see also
// TestManageModelsNeverShowsAliases below, which guards the one place the
// overlay must NOT apply.

// TestOfferedAliasAppearsInModelsListing: a token's Offer rule surfaces the
// alias name in Models(), the usage-facing listing that also feeds the
// portal's chat model picker.
func TestOfferedAliasAppearsInModelsListing(t *testing.T) {
	ctx := context.Background()
	svc, _ := newOfferingTestService(t, offeringMapping{name: "qwen3-32b", flavors: []string{routing.APIFlavorOpenAI}})
	token := tokenWithRules(map[string]store.ModelOverrideRule{
		"claude-x": {To: "qwen3-32b", Offer: true},
	})
	byID := modelsByID(svc.Models(ctx, token))
	if _, ok := byID["claude-x"]; !ok {
		t.Fatalf("offered alias missing from Models(): %#v", byID)
	}
}

// TestOfferedAliasCarriesTargetContextSize: the alias entry is not a bare
// name -- it carries its target's DTO data (context size here), same as the
// target's own row would, just filed under the alias name. This is correct,
// not a leak: the alias really does route to this target, and the listing is
// a display, never an access control.
func TestOfferedAliasCarriesTargetContextSize(t *testing.T) {
	ctx := context.Background()
	rs := routing.NewMemoryStore()
	if err := rs.CreateAIServer(ctx, routing.AIServer{ID: "srv_alias_ctx", Name: "AliasCtxBox", Domain: "srv_alias_ctx.test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: offeringTime, UpdatedAt: offeringTime}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := rs.CreateApplication(ctx, routing.Application{ID: "app_alias_ctx", ServerID: "srv_alias_ctx", Type: routing.ProviderVLLM, Port: 8000, Scheme: "https", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: offeringTime, UpdatedAt: offeringTime}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := rs.CreateMapping(ctx, routing.ModelMapping{ID: "map_alias_ctx", ApplicationID: "app_alias_ctx", GatewayModelName: "qwen3-32b", AppModelName: "qwen3-32b-up", Status: routing.ServerStatusActive, ContextSize: 32768, CreatedAt: offeringTime, UpdatedAt: offeringTime}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	svc := offerSvc(rs, nil)
	token := tokenWithRules(map[string]store.ModelOverrideRule{
		"claude-x": {To: "qwen3-32b", Offer: true},
	})
	byID := modelsByID(svc.Models(ctx, token))
	if byID["claude-x"].ContextSize != 32768 {
		t.Fatalf("claude-x context_size = %d, want 32768 (the target's)", byID["claude-x"].ContextSize)
	}
}

// TestHideTargetDropsFromModelsListing: HideTarget removes the target's own
// entry from Models(), mirroring the per-flavor listing's behaviour.
func TestHideTargetDropsFromModelsListing(t *testing.T) {
	ctx := context.Background()
	svc, _ := newOfferingTestService(t, offeringMapping{name: "qwen3-32b", flavors: []string{routing.APIFlavorOpenAI}})
	token := tokenWithRules(map[string]store.ModelOverrideRule{
		"claude-x": {To: "qwen3-32b", Offer: true, HideTarget: true},
	})
	byID := modelsByID(svc.Models(ctx, token))
	if _, ok := byID["qwen3-32b"]; ok {
		t.Fatal("hidden target still listed in Models()")
	}
	if _, ok := byID["claude-x"]; !ok {
		t.Fatal("alias missing from Models() although Offer is set")
	}
}

// TestManageModelsNeverShowsAliases: ManageModels() is the admin management
// surface (modelsResponse(suppress=false)) and must show the system's real
// models only -- filling it with one token's aliases would be wrong, since it
// is not scoped to any one token's perspective.
func TestManageModelsNeverShowsAliases(t *testing.T) {
	ctx := context.Background()
	svc, _ := newOfferingTestService(t, offeringMapping{name: "qwen3-32b", flavors: []string{routing.APIFlavorOpenAI}})
	token := tokenWithRules(map[string]store.ModelOverrideRule{
		"claude-x": {To: "qwen3-32b", Offer: true},
	})
	byID := modelsByID(svc.ManageModels(ctx, token))
	if _, ok := byID["claude-x"]; ok {
		t.Fatal("ManageModels() must never show a token's override aliases")
	}
	if _, ok := byID["qwen3-32b"]; !ok {
		t.Fatal("real model missing from ManageModels()")
	}
}

// TestOfferingReadsEachStoreTraversalOnce: Offered and Existing come from the
// same two reads with different filters applied, so one call must walk the
// mapping store once and load the group overlay once — Task 5 puts this on the
// per-inference-request path and Service caches nothing.
func TestOfferingReadsEachStoreTraversalOnce(t *testing.T) {
	ctx := context.Background()
	rs := routing.NewMemoryStore()
	offerModel(t, rs, "srv_c", "BoxC", "app_c", []string{routing.APIFlavorOpenAI}, "m1", "m1-up", routing.ServerStatusActive)
	offerGroup(t, rs, "grp_c", "coder", "m1")

	counted := &countingRoutingStore{MemoryStore: rs}
	svc := offerSvc(counted, nil)
	off := svc.ModelOfferingFor(ctx, auth.Token{UserID: "usr_off"}, routing.APIFlavorOpenAI)
	if _, ok := off.Existing["coder"]; !ok {
		t.Fatalf("precondition: group missing from Existing: %#v", off.Existing)
	}
	if counted.aiServers != 1 {
		t.Fatalf("AIServers called %d times, want 1 (one mapping traversal)", counted.aiServers)
	}
	if counted.modelGroups != 1 {
		t.Fatalf("ModelGroups called %d times, want 1 (one overlay load)", counted.modelGroups)
	}
	if counted.groupMembers != 1 {
		t.Fatalf("GroupMembersByGroup called %d times, want 1 per group (one overlay load)", counted.groupMembers)
	}
}
