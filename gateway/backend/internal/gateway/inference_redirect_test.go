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

// offeringWithLocked builds the model_settings "locked" shape: a GROUP-ONLY
// name that exists but that a direct request cannot route to at all
// (GroupRegistry.DirectAllowed → routing.ErrNoModelRoute). Unlike "hidden" this
// is a real access boundary, so the name is absent from BOTH Offered and
// Callable while staying in Existing — which is exactly the "exists but you
// cannot call it" shape UnknownModelRedirectBlocked is for.
func offeringWithLocked(callable, locked []string) portal.ModelOffering {
	return portal.ModelOffering{
		Offered:  nameSet(callable),
		Callable: nameSet(callable),
		Existing: nameSet(callable, locked),
	}
}

// offeringWithSuppressed builds the case where the LISTING and the ACCESS set
// disagree: `suppressed` names are dropped from the listing (model_settings
// "hidden", or a rule's HideTarget) yet stay fully callable under their own
// name — so they are absent from Offered but present in Callable and Existing.
// This is the shape the redirect has to get right; asking Offered here would
// reroute a request the token was entitled to serve.
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

// TestRedirectWidenedRedirectsALockedModel: "locked" means group-only, so a
// direct request for that name is refused with ErrNoModelRoute. That is
// literally the "the model exists but you cannot call it" case
// UnknownModelRedirectBlocked was added for, so widened mode must redirect it —
// which only happens while Callable excludes locked names.
func TestRedirectWidenedRedirectsALockedModel(t *testing.T) {
	tok := auth.Token{UnknownModelRedirect: true, UnknownModelRedirectBlocked: true, LastUsedModel: "a"}
	if got := redirectUnknownModel(tok, "locked-model", offeringWithLocked([]string{"a"}, []string{"locked-model"})); got != "a" {
		t.Fatalf("redirect = %q, want a (a locked model cannot be called directly)", got)
	}
}

// TestRedirectRejectsALockedFallback: the other consequence. A locked name is
// not a usable TARGET either — redirecting onto it would swap the client's
// model for one that then fails to route, under a name the client never sent.
// Configured as the fallback, it must simply not be taken.
func TestRedirectRejectsALockedFallback(t *testing.T) {
	tok := auth.Token{UnknownModelRedirect: true, LastUsedModel: "gone", UnknownModelFallback: "locked-model"}
	if got := redirectUnknownModel(tok, "nope", offeringWithLocked([]string{"a"}, []string{"locked-model"})); got != "" {
		t.Fatalf("redirect = %q, want no redirect onto a locked model", got)
	}
}

// TestRedirectSkipsALockedLastUsedModel: same for the marker. A model can be
// locked AFTER a token last used it successfully, so the last-used candidate is
// exactly where a stale locked name shows up — the chain must step over it and
// take the fallback rather than route to a dead end.
func TestRedirectSkipsALockedLastUsedModel(t *testing.T) {
	tok := auth.Token{UnknownModelRedirect: true, LastUsedModel: "locked-model", UnknownModelFallback: "a"}
	if got := redirectUnknownModel(tok, "nope", offeringWithLocked([]string{"a"}, []string{"locked-model"})); got != "a" {
		t.Fatalf("redirect = %q, want the fallback a", got)
	}
}

// serviceToken builds a service-account token with a model allowlist — the ONE
// per-token access boundary portal.ModelOffering cannot know about, since it is
// enforced by modelAllowed on this side of the portal boundary. A name blocked
// only by the allowlist is therefore in Callable and still 403s, which is
// exactly the "exists but you may not use it" case widened mode is for.
func serviceToken(allowed ...string) auth.Token {
	return auth.Token{Kind: "service", UnknownModelRedirect: true, AllowedModels: allowed}
}

// TestRedirectWidenedCoversAnAllowlistBlockedModel is the service allowlist as
// the canonical widened-mode case: the model exists, this token is provisioned
// for it, and only the allowlist refuses it. Judging by Callable alone leaves
// it looking callable, the redirect declines, and the client gets the same 403
// the switch was turned on to avoid.
func TestRedirectWidenedCoversAnAllowlistBlockedModel(t *testing.T) {
	tok := serviceToken("a")
	tok.UnknownModelRedirectBlocked = true
	tok.LastUsedModel = "a"
	if got := redirectUnknownModel(tok, "b", offering([]string{"a", "b"}, []string{"a", "b"})); got != "a" {
		t.Fatalf("redirect = %q, want a (b is blocked by the service allowlist)", got)
	}
}

// TestRedirectNarrowLeavesAnAllowlistBlockedModelAlone: the switch is what makes
// the difference, so the default must still refuse. "b" exists, so narrow mode
// short-circuits on Existing and the 403 stays visible.
func TestRedirectNarrowLeavesAnAllowlistBlockedModelAlone(t *testing.T) {
	tok := serviceToken("a")
	tok.LastUsedModel = "a"
	if got := redirectUnknownModel(tok, "b", offering([]string{"a", "b"}, []string{"a", "b"})); got != "" {
		t.Fatalf("redirect = %q, want no redirect in narrow mode", got)
	}
}

// TestRedirectRejectsAnAllowlistBlockedFallback: the candidate half. Redirecting
// onto a name the allowlist refuses would replace a legible "unknown model"
// error with a 403 naming a model the client never sent.
func TestRedirectRejectsAnAllowlistBlockedFallback(t *testing.T) {
	tok := serviceToken("a")
	tok.LastUsedModel = "gone"
	tok.UnknownModelFallback = "f"
	if got := redirectUnknownModel(tok, "nope", offering([]string{"a", "f"}, []string{"a", "f"})); got != "" {
		t.Fatalf("redirect = %q, want no redirect onto an allowlist-blocked fallback", got)
	}
}

// TestRedirectSkipsAnAllowlistBlockedLastUsedModel: a model can drop out of the
// allowlist AFTER a token last used it, so the marker is exactly where a stale
// blocked name shows up. The chain steps over it and takes the fallback.
func TestRedirectSkipsAnAllowlistBlockedLastUsedModel(t *testing.T) {
	tok := serviceToken("a")
	tok.LastUsedModel = "dropped"
	tok.UnknownModelFallback = "a"
	if got := redirectUnknownModel(tok, "nope", offering([]string{"a", "dropped"}, []string{"a", "dropped"})); got != "a" {
		t.Fatalf("redirect = %q, want the fallback a", got)
	}
}

// TestRedirectEmptyAllowlistMeansEverything pins the allowlist's own default: on
// a service token an EMPTY allowlist allows every model (it is opt-in, not
// deny-by-default). Reading it as "nothing allowed" would make every name
// uncallable, so widened mode would redirect every request and then find no
// candidate to redirect it to.
func TestRedirectEmptyAllowlistMeansEverything(t *testing.T) {
	tok := serviceToken()
	tok.UnknownModelRedirectBlocked = true
	tok.LastUsedModel = "a"
	if got := redirectUnknownModel(tok, "b", offering([]string{"a", "b"}, []string{"a", "b"})); got != "" {
		t.Fatalf("redirect = %q, want no redirect: an empty allowlist allows everything", got)
	}
	if got := redirectUnknownModel(tok, "nope", offering([]string{"a"}, []string{"a"})); got != "a" {
		t.Fatalf("redirect = %q, want a: an empty allowlist must not block the candidate either", got)
	}
}

// TestRedirectAllowlistIgnoredForAUserToken: AllowedModels only ever means
// anything on a service token (modelAllowed's own rule), and a user token never
// carries one. A stray value must not narrow anything.
func TestRedirectAllowlistIgnoredForAUserToken(t *testing.T) {
	tok := auth.Token{UnknownModelRedirect: true, UnknownModelRedirectBlocked: true, LastUsedModel: "a", AllowedModels: []string{"a"}}
	if got := redirectUnknownModel(tok, "b", offering([]string{"a", "b"}, []string{"a", "b"})); got != "" {
		t.Fatalf("redirect = %q, want no redirect: a user token has no allowlist", got)
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
