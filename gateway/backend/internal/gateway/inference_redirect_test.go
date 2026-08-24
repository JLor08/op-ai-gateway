// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"testing"
)

// offering builds a portal.ModelOffering from two plain name lists, so each
// test below reads as "these names are offered, these exist".
func offering(offered, existing []string) portal.ModelOffering {
	out := portal.ModelOffering{Offered: map[string]struct{}{}, Existing: map[string]struct{}{}}
	for _, n := range offered {
		out.Offered[n] = struct{}{}
	}
	for _, n := range existing {
		out.Existing[n] = struct{}{}
	}
	return out
}

func TestRedirectOffWithoutTheSwitch(t *testing.T) {
	tok := auth.Token{LastUsedModel: "a"}
	if got := redirectUnknownModel(tok, "nope", offering([]string{"a"}, []string{"a"})); got != "" {
		t.Fatalf("redirected to %q although the switch is off", got)
	}
}

func TestRedirectUsesLastUsedWhenStillOffered(t *testing.T) {
	tok := auth.Token{UnknownModelRedirect: true, LastUsedModel: "a", UnknownModelFallback: "f"}
	if got := redirectUnknownModel(tok, "nope", offering([]string{"a", "f"}, []string{"a", "f"})); got != "a" {
		t.Fatalf("redirect = %q, want a", got)
	}
}

func TestRedirectFallsBackWhenLastUsedGone(t *testing.T) {
	// The marker survives the model disappearing; the fallback is exactly for it.
	tok := auth.Token{UnknownModelRedirect: true, LastUsedModel: "gone", UnknownModelFallback: "f"}
	if got := redirectUnknownModel(tok, "nope", offering([]string{"f"}, []string{"f"})); got != "f" {
		t.Fatalf("redirect = %q, want f", got)
	}
}

func TestRedirectFallsBackWhenNoLastUsedYet(t *testing.T) {
	tok := auth.Token{UnknownModelRedirect: true, UnknownModelFallback: "f"}
	if got := redirectUnknownModel(tok, "nope", offering([]string{"f"}, []string{"f"})); got != "f" {
		t.Fatalf("redirect = %q, want f", got)
	}
}

func TestRedirectGivesUpWhenFallbackNotOffered(t *testing.T) {
	// Nothing usable left: the request must fail exactly as it does today.
	tok := auth.Token{UnknownModelRedirect: true, LastUsedModel: "gone", UnknownModelFallback: "alsogone"}
	if got := redirectUnknownModel(tok, "nope", offering([]string{"other"}, []string{"other"})); got != "" {
		t.Fatalf("redirect = %q, want no redirect", got)
	}
}

func TestRedirectLeavesBlockedModelsAloneByDefault(t *testing.T) {
	// The model exists but this token does not see it. With the narrow setting a
	// refusal stays a refusal, so a misconfigured allowlist keeps being visible.
	tok := auth.Token{UnknownModelRedirect: true, LastUsedModel: "a"}
	if got := redirectUnknownModel(tok, "secret", offering([]string{"a"}, []string{"a", "secret"})); got != "" {
		t.Fatalf("redirect = %q, want no redirect for a blocked model", got)
	}
}

func TestRedirectCoversBlockedModelsWhenWidened(t *testing.T) {
	tok := auth.Token{UnknownModelRedirect: true, UnknownModelRedirectBlocked: true, LastUsedModel: "a"}
	if got := redirectUnknownModel(tok, "secret", offering([]string{"a"}, []string{"a", "secret"})); got != "a" {
		t.Fatalf("redirect = %q, want a", got)
	}
}

func TestRedirectLeavesKnownModelsAlone(t *testing.T) {
	tok := auth.Token{UnknownModelRedirect: true, LastUsedModel: "a"}
	if got := redirectUnknownModel(tok, "b", offering([]string{"a", "b"}, []string{"a", "b"})); got != "" {
		t.Fatalf("redirect = %q, want no redirect for an offered model", got)
	}
}

// TestRedirectLeavesAnOfferedAliasAlone covers the one case where Offered and
// Existing genuinely disagree: an override alias is a name this token is
// offered that exists nowhere else (see applyOverrideAliases). Asking about
// Existing first would call such an alias unknown and redirect away from a name
// the token was deliberately offered — so "is it offered" has to be asked, and
// answered, before "does it exist at all".
func TestRedirectLeavesAnOfferedAliasAlone(t *testing.T) {
	tok := auth.Token{UnknownModelRedirect: true, LastUsedModel: "a"}
	if got := redirectUnknownModel(tok, "alias", offering([]string{"a", "alias"}, []string{"a"})); got != "" {
		t.Fatalf("redirect = %q, want no redirect for an offered alias", got)
	}
}

// TestRedirectWidenedStillLeavesOfferedModelsAlone proves the widening switch
// only ever adds blocked names to the redirect's reach. A model this token IS
// offered stays untouched under UnknownModelRedirectBlocked too — the switch
// widens what counts as "does not apply", it does not turn the redirect on for
// working requests.
func TestRedirectWidenedStillLeavesOfferedModelsAlone(t *testing.T) {
	tok := auth.Token{UnknownModelRedirect: true, UnknownModelRedirectBlocked: true, LastUsedModel: "a"}
	if got := redirectUnknownModel(tok, "b", offering([]string{"a", "b"}, []string{"a", "b"})); got != "" {
		t.Fatalf("redirect = %q, want no redirect for an offered model", got)
	}
}

// TestRedirectDeclinesOnAnEmptyOffering pins the store-failure direction.
// ModelOfferingFor returns two WHOLLY EMPTY sets on any store error (that is
// deliberate — see its doc comment: the only safe partial result is none).
// Empty sets mean every candidate looks unoffered, so the chain must run out
// and decline: a store hiccup makes the client see today's ordinary error, it
// never silently reroutes traffic to some arbitrary model.
func TestRedirectDeclinesOnAnEmptyOffering(t *testing.T) {
	tok := auth.Token{UnknownModelRedirect: true, UnknownModelRedirectBlocked: true, LastUsedModel: "a", UnknownModelFallback: "f"}
	if got := redirectUnknownModel(tok, "nope", offering(nil, nil)); got != "" {
		t.Fatalf("redirect = %q, want no redirect when the offering lookup failed", got)
	}
}
