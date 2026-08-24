// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
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
