// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"testing"
)

func nameSet(lists ...[]string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, list := range lists {
		for _, n := range list {
			out[n] = struct{}{}
		}
	}
	return out
}

// offering builds the ORDINARY portal.ModelOffering, where nothing is
// suppressed and everything listed is therefore also callable — so each test
// below reads as "these names are offered, these exist".
func offering(offered, existing []string) portal.ModelOffering {
	return portal.ModelOffering{Offered: nameSet(offered), Callable: nameSet(offered), Existing: nameSet(existing)}
}

// offeringWithSuppressed builds the case where the LISTING and the ACCESS set
// disagree: `suppressed` names are dropped from the listing (model_settings
// hidden/locked, or a rule's HideTarget) yet stay fully callable under their
// own name — so they are absent from Offered but present in Callable and
// Existing. This is the shape the redirect has to get right; asking Offered
// here would reroute a request the token was entitled to serve.
func offeringWithSuppressed(offered, suppressed []string) portal.ModelOffering {
	return portal.ModelOffering{
		Offered:  nameSet(offered),
		Callable: nameSet(offered, suppressed),
		Existing: nameSet(offered, suppressed),
	}
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

// TestRedirectWidenedLeavesASuppressedButCallableModelAlone is the case that
// separates the listing set from the access set. A model suppressed by
// model_settings (hidden/locked) — or by a rule's HideTarget — is dropped from
// Offered but stays fully callable under its own name. It is therefore ∈Existing
// and ∉Offered, exactly the shape widened mode acts on, so a redirect that asked
// Offered would fire here and reroute a request this token was entitled to
// serve. Asking Callable, it declines.
func TestRedirectWidenedLeavesASuppressedButCallableModelAlone(t *testing.T) {
	tok := auth.Token{UnknownModelRedirect: true, UnknownModelRedirectBlocked: true, LastUsedModel: "a"}
	if got := redirectUnknownModel(tok, "hidden-but-callable", offeringWithSuppressed([]string{"a"}, []string{"hidden-but-callable"})); got != "" {
		t.Fatalf("redirect = %q, want no redirect for a suppressed but callable model", got)
	}
}

// TestRedirectNarrowModeIsUnchangedBySuppression pins that none of the above
// moved the default. In narrow mode a suppressed-but-callable model was, and
// remains, left alone — there it is caught by the Existing short-circuit rather
// than the Callable one, so it must hold whichever set the first check reads.
func TestRedirectNarrowModeIsUnchangedBySuppression(t *testing.T) {
	tok := auth.Token{UnknownModelRedirect: true, LastUsedModel: "a"}
	if got := redirectUnknownModel(tok, "hidden-but-callable", offeringWithSuppressed([]string{"a"}, []string{"hidden-but-callable"})); got != "" {
		t.Fatalf("redirect = %q, want no redirect in narrow mode", got)
	}
}

// TestRedirectAcceptsASuppressedButCallableCandidate is the same distinction
// from the other side: the token's last used model may since have been hidden
// from the listing while staying perfectly routable. Judging candidates by the
// listing would skip it and fall through to the fallback (or decline outright)
// even though it still works.
func TestRedirectAcceptsASuppressedButCallableCandidate(t *testing.T) {
	tok := auth.Token{UnknownModelRedirect: true, LastUsedModel: "hidden-but-callable", UnknownModelFallback: "f"}
	if got := redirectUnknownModel(tok, "nope", offeringWithSuppressed([]string{"f"}, []string{"hidden-but-callable"})); got != "hidden-but-callable" {
		t.Fatalf("redirect = %q, want hidden-but-callable", got)
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
