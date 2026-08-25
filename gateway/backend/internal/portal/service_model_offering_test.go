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

// listed reports whether the per-flavor discovery listing (/v1/models et al.)
// shows `name` to this token. The listing is what the removed
// ModelOffering.Offered field used to mirror; asking ModelsForFlavor asks the
// real thing instead of a copy of it, which is the whole reason the copy went.
func listed(t *testing.T, svc *Service, token auth.Token, flavor, name string) bool {
	t.Helper()
	return containsString(svc.ModelsForFlavor(context.Background(), token, flavor), name)
}

// tokenWithRules returns a token carrying the given model-override rules. The
// rules are written in the store shape (the shape the token editor persists)
// and converted through the one bridge that exists for it.
func tokenWithRules(rules map[string]store.ModelOverrideRule) auth.Token {
	return auth.Token{UserID: "usr_off", ModelOverrideRules: store.AuthModelOverrideRules(rules)}
}

// aliasRules is the auth-side rule map applyOverrideAliases takes, written in
// the store shape the token editor persists and converted through the one
// bridge that exists for it.
func aliasRules(rules map[string]store.ModelOverrideRule) map[string]auth.ModelOverrideRule {
	return store.AuthModelOverrideRules(rules)
}

// flavorSet is one entry of the per-name flavor map the overlay works on.
func flavorSet(flavors ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(flavors))
	for _, f := range flavors {
		out[f] = struct{}{}
	}
	return out
}

// TestOverrideAliasFlavorsAreCopiedNotShared: an offered alias must get its OWN
// flavor set, never a reference to its target's. Sharing one map object between
// an alias, its target, and every other alias onto that target means a single
// later per-name flavor edit silently reaches names it was never applied to —
// the kind of aliasing bug that only surfaces once someone adds the first
// mutator, far from here.
func TestOverrideAliasFlavorsAreCopiedNotShared(t *testing.T) {
	preSuppress := map[string]map[string]struct{}{"target": flavorSet(routing.APIFlavorOpenAI)}
	sets := map[string]map[string]struct{}{"target": preSuppress["target"]}
	applyOverrideAliases(sets, preSuppress, aliasRules(map[string]store.ModelOverrideRule{
		"alias-a": {To: "target", Offer: true},
		"alias-b": {To: "target", Offer: true},
	}))
	// Mutating one alias's flavors must reach nothing else.
	sets["alias-a"][routing.APIFlavorAnthropic] = struct{}{}
	for _, name := range []string{"alias-b", "target"} {
		if _, leaked := sets[name][routing.APIFlavorAnthropic]; leaked {
			t.Fatalf("%q shares its flavor map with alias-a: %#v", name, sets[name])
		}
	}
	if _, leaked := preSuppress["target"][routing.APIFlavorAnthropic]; leaked {
		t.Fatalf("the pre-suppression map was mutated through an alias: %#v", preSuppress["target"])
	}
}

// TestHideTargetRemovesAnAliasOfTheSameName documents the one surprising
// consequence of applying every HideTarget AFTER all the adding: hiding is by
// NAME, and an alias name shares the model namespace. A rule that offers the
// alias "shadow" and another rule that names "shadow" as its hide target both
// hit the same key, so the alias is added and then removed — the token sees
// neither the real model nor the alias.
//
// That is the intended reading of "a set switch is an instruction", and it is
// DETERMINISTIC: the deferred hide pass is exactly what stops Go's randomized
// map iteration from deciding the outcome. Repeated so a version that
// interleaved adding and hiding would fail here rather than flake in
// production.
func TestHideTargetRemovesAnAliasOfTheSameName(t *testing.T) {
	for i := 0; i < 64; i++ {
		preSuppress := map[string]map[string]struct{}{
			"shadow": flavorSet(routing.APIFlavorOpenAI),
			"real":   flavorSet(routing.APIFlavorOpenAI),
		}
		sets := map[string]map[string]struct{}{
			"shadow": preSuppress["shadow"],
			"real":   preSuppress["real"],
		}
		applyOverrideAliases(sets, preSuppress, aliasRules(map[string]store.ModelOverrideRule{
			"shadow": {To: "real", Offer: true},        // offers an alias named "shadow"
			"other":  {To: "shadow", HideTarget: true}, // hides the name "shadow"
		}))
		if _, ok := sets["shadow"]; ok {
			t.Fatalf("run %d: %q survived a rule that hides that name: %#v", i, "shadow", sets)
		}
		if _, ok := sets["real"]; !ok {
			t.Fatalf("run %d: the hide pass removed an unrelated name: %#v", i, sets)
		}
	}
}

func TestOfferedAliasAppearsWithTargetFlavors(t *testing.T) {
	// An alias inherits its target's flavors: an Anthropic-only target must not
	// surface the alias in the OpenAI listing.
	ctx := context.Background()
	svc, _ := newOfferingTestService(t, offeringMapping{name: "qwen3-32b", flavors: []string{routing.APIFlavorAnthropic}})
	token := tokenWithRules(map[string]store.ModelOverrideRule{
		"claude-x": {To: "qwen3-32b", Offer: true},
	})
	if !listed(t, svc, token, routing.APIFlavorAnthropic, "claude-x") {
		t.Fatal("alias missing from the anthropic listing")
	}
	if listed(t, svc, token, routing.APIFlavorOpenAI, "claude-x") {
		t.Fatal("alias leaked into the openai listing")
	}
	_ = ctx
}

func TestUnofferedAliasStaysOut(t *testing.T) {
	ctx := context.Background()
	svc, _ := newOfferingTestService(t, offeringMapping{name: "qwen3-32b", flavors: []string{routing.APIFlavorOpenAI}})
	token := tokenWithRules(map[string]store.ModelOverrideRule{"gpt-4o": {To: "qwen3-32b"}})
	if listed(t, svc, token, routing.APIFlavorOpenAI, "gpt-4o") {
		t.Fatal("alias offered although Offer is false")
	}
	_ = ctx
}

func TestHideTargetDropsTargetName(t *testing.T) {
	ctx := context.Background()
	svc, _ := newOfferingTestService(t, offeringMapping{name: "qwen3-32b", flavors: []string{routing.APIFlavorOpenAI}})
	token := tokenWithRules(map[string]store.ModelOverrideRule{
		"gpt-4o": {To: "qwen3-32b", Offer: true, HideTarget: true},
	})
	if listed(t, svc, token, routing.APIFlavorOpenAI, "qwen3-32b") {
		t.Fatal("hidden target still listed")
	}
	// Hiding is a listing concern only: the target still EXISTS, so the redirect
	// in Task 5 must not treat it as unknown.
	off := svc.ModelOfferingFor(ctx, token, routing.APIFlavorOpenAI)
	if _, ok := off.Existing["qwen3-32b"]; !ok {
		t.Fatal("hidden target dropped from Existing")
	}
	// The alias itself is still offered.
	if !listed(t, svc, token, routing.APIFlavorOpenAI, "gpt-4o") {
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
	if listed(t, svc, token, routing.APIFlavorOpenAI, "qwen3-32b") {
		t.Fatal("target visible although one row hides it")
	}
	_ = ctx
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
	if listed(t, svc, token, routing.APIFlavorOpenAI, "ghost") {
		t.Fatal("alias onto a non-existent target was offered")
	}
	if !listed(t, svc, token, routing.APIFlavorOpenAI, "qwen3-32b") {
		t.Fatal("the real model disappeared from the listing")
	}
	_ = ctx
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
	if !listed(t, svc, token, routing.APIFlavorOpenAI, "gpt-4o") {
		t.Fatal("alias of a hidden target missing from the listing")
	}
	if listed(t, svc, token, routing.APIFlavorOpenAI, "qwen3-32b") {
		t.Fatal("hidden target listed under its own name")
	}
	// Existing ignores visibility: the hidden model still exists.
	off := svc.ModelOfferingFor(ctx, token, routing.APIFlavorOpenAI)
	if _, ok := off.Existing["qwen3-32b"]; !ok {
		t.Fatal("hidden model missing from Existing")
	}
}

// TestCallableSplitsHiddenFromLocked is the whole point of Callable. Both
// values suppress a model from every usage-facing LISTING, and there the two
// are interchangeable — but only one of them is about access:
//
//   - "hidden" is display-only. The model routes fine under its own name, so it
//     stays callable and the redirect must leave a request for it alone.
//   - "locked" is group-only. GroupRegistry.DirectAllowed refuses it and
//     routing.Resolver turns that into ErrNoModelRoute, so a direct request
//     cannot succeed — it is not callable, and the redirect both may and must
//     act on it.
//
// Either way the name still EXISTS: narrow mode keeps refusing rather than
// redirecting.
func TestCallableSplitsHiddenFromLocked(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		visibility   string
		wantCallable bool
	}{
		{"hidden", true},
		{"locked", false},
	}
	for _, tc := range cases {
		t.Run(tc.visibility, func(t *testing.T) {
			svc, rs := newOfferingTestService(t, offeringMapping{name: "qwen3-32b", flavors: []string{routing.APIFlavorOpenAI}})
			offerVisibility(t, rs, "qwen3-32b", tc.visibility)
			token := auth.Token{UserID: "usr_off"}
			off := svc.ModelOfferingFor(ctx, token, routing.APIFlavorOpenAI)
			if listed(t, svc, token, routing.APIFlavorOpenAI, "qwen3-32b") {
				t.Fatalf("%s model still listed", tc.visibility)
			}
			if _, ok := off.Callable["qwen3-32b"]; ok != tc.wantCallable {
				t.Fatalf("Callable[%s model] = %v, want %v", tc.visibility, ok, tc.wantCallable)
			}
			if _, ok := off.Existing["qwen3-32b"]; !ok {
				t.Fatalf("%s model dropped from Existing", tc.visibility)
			}
		})
	}
}

// TestCallableDropsALockedGroupName: "locked" is refused for a GROUP name too —
// the resolver's group branch checks DirectAllowed before dispatching ("a
// locked group: not directly requestable"), so the locked filter must apply to
// group names, not only model names.
func TestCallableDropsALockedGroupName(t *testing.T) {
	ctx := context.Background()
	svc, rs := newOfferingTestService(t,
		offeringMapping{name: "m1", flavors: []string{routing.APIFlavorOpenAI}},
		offeringMapping{name: "m2", flavors: []string{routing.APIFlavorOpenAI}},
	)
	offerGroup(t, rs, "grp_off", "coder", "m1", "m2")
	offerVisibility(t, rs, "coder", "locked")
	off := svc.ModelOfferingFor(ctx, auth.Token{UserID: "usr_off"}, routing.APIFlavorOpenAI)
	if _, ok := off.Callable["coder"]; ok {
		t.Fatal("locked group name is callable although a direct request for it is refused")
	}
	if _, ok := off.Existing["coder"]; !ok {
		t.Fatal("locked group dropped from Existing")
	}
	// Its members keep their own, unlocked names.
	if _, ok := off.Callable["m1"]; !ok {
		t.Fatal("member of a locked group lost its own callable name")
	}
}

// TestCallableDropsALockedMemberButKeepsItsGroup is the group-path question: a
// locked model is reachable VIA a group, and the group name must stay callable
// while the member's OWN name must not ride along on it. It does not: the group
// overlay contributes only the group's name (carrying its members' FLAVOR
// union, never their names), so the member has exactly one entry of its own and
// the locked filter removes it.
func TestCallableDropsALockedMemberButKeepsItsGroup(t *testing.T) {
	ctx := context.Background()
	svc, rs := newOfferingTestService(t,
		offeringMapping{name: "m1", flavors: []string{routing.APIFlavorOpenAI}},
		offeringMapping{name: "m2", flavors: []string{routing.APIFlavorOpenAI}},
	)
	offerGroup(t, rs, "grp_off", "coder", "m1", "m2")
	offerVisibility(t, rs, "m1", "locked")
	off := svc.ModelOfferingFor(ctx, auth.Token{UserID: "usr_off"}, routing.APIFlavorOpenAI)
	if _, ok := off.Callable["m1"]; ok {
		t.Fatal("locked member rode into Callable under its own name")
	}
	if _, ok := off.Callable["coder"]; !ok {
		t.Fatal("the group itself must stay callable — that is how a locked member is reached")
	}
	if _, ok := off.Existing["m1"]; !ok {
		t.Fatal("locked member dropped from Existing")
	}
}

// TestCallableKeepsAHideTargetTarget: the same distinction from the per-token
// side. A rule's HideTarget is explicitly documented as a listing overlay and
// never an access control — the target stays callable under its real name.
func TestCallableKeepsAHideTargetTarget(t *testing.T) {
	ctx := context.Background()
	svc, _ := newOfferingTestService(t, offeringMapping{name: "qwen3-32b", flavors: []string{routing.APIFlavorOpenAI}})
	token := tokenWithRules(map[string]store.ModelOverrideRule{
		"gpt-4o": {To: "qwen3-32b", Offer: true, HideTarget: true},
	})
	if listed(t, svc, token, routing.APIFlavorOpenAI, "qwen3-32b") {
		t.Fatal("hidden target still listed")
	}
	off := svc.ModelOfferingFor(ctx, token, routing.APIFlavorOpenAI)
	if _, ok := off.Callable["qwen3-32b"]; !ok {
		t.Fatal("hidden target dropped from Callable although it still routes")
	}
}

// TestCallableIncludesAHiddenGroup: a group name is callable like any model
// name, and its own display visibility must not remove it from the access set
// either.
func TestCallableIncludesAHiddenGroup(t *testing.T) {
	ctx := context.Background()
	svc, rs := newOfferingTestService(t,
		offeringMapping{name: "m1", flavors: []string{routing.APIFlavorOpenAI}},
		offeringMapping{name: "m2", flavors: []string{routing.APIFlavorAnthropic}},
	)
	offerGroup(t, rs, "grp_off", "coder", "m1", "m2")
	offerVisibility(t, rs, "coder", "hidden")
	token := auth.Token{UserID: "usr_off"}
	off := svc.ModelOfferingFor(ctx, token, routing.APIFlavorOpenAI)
	if listed(t, svc, token, routing.APIFlavorOpenAI, "coder") {
		t.Fatal("hidden group still listed")
	}
	if _, ok := off.Callable["coder"]; !ok {
		t.Fatalf("hidden group dropped from Callable: %#v", off.Callable)
	}
}

// TestCallableExcludesAnAliasName: an alias is a listing name, not a routable
// one — resolveModelOverride rewrites it to its target before anything else
// sees it, so the target, and only the target, is what is callable.
func TestCallableExcludesAnAliasName(t *testing.T) {
	ctx := context.Background()
	svc, _ := newOfferingTestService(t, offeringMapping{name: "qwen3-32b", flavors: []string{routing.APIFlavorOpenAI}})
	token := tokenWithRules(map[string]store.ModelOverrideRule{
		"claude-x": {To: "qwen3-32b", Offer: true},
	})
	if !listed(t, svc, token, routing.APIFlavorOpenAI, "claude-x") {
		t.Fatal("alias missing from the listing")
	}
	off := svc.ModelOfferingFor(ctx, token, routing.APIFlavorOpenAI)
	if _, ok := off.Callable["claude-x"]; ok {
		t.Fatal("alias leaked into Callable")
	}
	if _, ok := off.Callable["qwen3-32b"]; !ok {
		t.Fatal("alias target missing from Callable")
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
	if listed(t, svc, auth.Token{UserID: "usr_off"}, routing.APIFlavorOpenAI, "coder") {
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
		if !listed(t, svc, token, flavor, "gpt-4o") {
			t.Fatalf("alias onto a group missing from the %s listing", flavor)
		}
		if listed(t, svc, token, flavor, "coder") {
			t.Fatalf("hidden group target still listed in the %s listing", flavor)
		}
	}
	_ = ctx
}

// TestOfferingWithoutRulesIsUnchanged: a token WITHOUT override rules gets
// exactly the listing it got before this change — neither gained nor lost a
// name. It also pins listing ⊆ Callable for such a token: the listing may drop
// a merely-hidden name (m4 here), but it can never show one the token cannot
// route to, since without rules there are no aliases to add.
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
		callable := svc.ModelOfferingFor(ctx, token, flavor).Callable
		for _, name := range expected {
			if _, ok := callable[name]; !ok {
				t.Fatalf("listed %q is not callable for %s: Callable = %#v", name, flavor, callable)
			}
		}
		// The hidden model is the other half of the same point: dropped from the
		// listing, still callable. Only the openai flavor serves it.
		if _, ok := callable["m4"]; ok != (flavor == routing.APIFlavorOpenAI) {
			t.Fatalf("Callable(%s)[m4] = %v: a hidden model must stay callable where it is served", flavor, ok)
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
// sets empty, never a half-built answer. A populated Callable next to an empty
// Existing is indistinguishable, to the caller, from "these names exist but
// none of them is real" — which is exactly the state that makes the redirect
// fire on every request and then find a candidate to send it to.
func TestOfferingIsWhollyEmptyOnMappingStoreError(t *testing.T) {
	ctx := context.Background()
	rs := routing.NewMemoryStore()
	offerModel(t, rs, "srv_e", "BoxE", "app_e", []string{routing.APIFlavorOpenAI}, "qwen3-32b", "qwen-up", routing.ServerStatusActive)

	svc := offerSvc(gatewayAIServersErrStore{rs}, nil)
	off := svc.ModelOfferingFor(ctx, auth.Token{UserID: "usr_off"}, routing.APIFlavorOpenAI)
	assertOfferingWhollyEmpty(t, off, "store error")
}

// assertOfferingWhollyEmpty checks the all-or-nothing contract across BOTH
// sets. Callable matters most: a populated Callable beside an empty Existing
// would make the redirect treat every request as unknown and then find a
// perfectly good candidate to send it to — the exact silent rerouting the
// empty-on-error rule exists to prevent.
func assertOfferingWhollyEmpty(t *testing.T, off ModelOffering, what string) {
	t.Helper()
	if len(off.Callable) != 0 || len(off.Existing) != 0 {
		t.Fatalf("%s must yield empty sets, got Callable=%#v Existing=%#v", what, off.Callable, off.Existing)
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
	assertOfferingWhollyEmpty(t, off, "overlay error")
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
// wholly empty, or an Existing that accounts for every Callable name. A second
// traversal reintroduced into ModelOfferingFor fails here, because Callable
// would survive it and Existing would not.
func TestOfferingNeverReturnsAPartialResult(t *testing.T) {
	ctx := context.Background()
	rs := routing.NewMemoryStore()
	offerModel(t, rs, "srv_l", "BoxL", "app_l", []string{routing.APIFlavorOpenAI}, "m1", "m1-up", routing.ServerStatusActive)
	offerModel(t, rs, "srv_l2", "BoxL2", "app_l2", []string{routing.APIFlavorOpenAI}, "m2", "m2-up", routing.ServerStatusActive)

	svc := offerSvc(&lateFailAIServersStore{MemoryStore: rs}, nil)
	off := svc.ModelOfferingFor(ctx, auth.Token{UserID: "usr_off"}, routing.APIFlavorOpenAI)
	if len(off.Callable) == 0 {
		if len(off.Existing) != 0 {
			t.Fatalf("empty Callable beside a populated Existing: %#v", off.Existing)
		}
		return // wholly empty is the other acceptable answer
	}
	// Callable ⊆ Existing unconditionally: a name this token can route to
	// exists by definition. A Callable name outside Existing is the partial
	// result that makes the redirect judge the request unknown and then hand it
	// that very name as a target.
	for name := range off.Callable {
		if _, ok := off.Existing[name]; !ok {
			t.Fatalf("callable %q is missing from Existing (partial result): Callable=%#v Existing=%#v", name, off.Callable, off.Existing)
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

// TestOfferedAliasOntoAGroupReportsIsGroup: models and groups share one
// namespace (TestOfferedAliasOntoAGroup already covers this as a legitimate
// override target), so an alias whose rule.To names a GROUP really does fail
// over across that group's members. The alias entry must report IsGroup:
// true, or it understates what routing through it actually does -- it is not
// an ordinary single-model alias.
func TestOfferedAliasOntoAGroupReportsIsGroup(t *testing.T) {
	ctx := context.Background()
	svc, rs := newOfferingTestService(t,
		offeringMapping{name: "m1", flavors: []string{routing.APIFlavorOpenAI}},
		offeringMapping{name: "m2", flavors: []string{routing.APIFlavorOpenAI}},
	)
	offerGroup(t, rs, "grp_off", "coder", "m1", "m2")
	token := tokenWithRules(map[string]store.ModelOverrideRule{
		"claude-x": {To: "coder", Offer: true},
	})
	byID := modelsByID(svc.Models(ctx, token))
	dto, ok := byID["claude-x"]
	if !ok {
		t.Fatalf("alias onto a group missing from Models(): %#v", byID)
	}
	if !dto.IsGroup {
		t.Fatal("alias onto a group must report is_group=true, understates that it fails over across members")
	}
}

// TestOfferingReadsEachStoreTraversalOnce: Callable and Existing come from the
// same two reads with different filters applied, so one call must walk the
// mapping store once and load the group overlay once — this sits on the
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
