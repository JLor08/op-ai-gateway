// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/auth"
	"strings"
)

// ModelOffering answers the questions the unknown-model redirect asks about a
// requested model name, for ONE API flavor. The three sets are deliberately
// distinct, and confusing any two of them produces a wrong redirect.
//
// Offered is the LISTING set: what this token sees in per-flavor discovery
// (/v1/models et al.), including the token's own offered override aliases and
// minus both the model_settings hidden/locked names and the targets its rules
// hide.
//
// Callable is the ACCESS set: what this token can actually route to under its
// real name, i.e. exactly the names a direct request can succeed with. It
// applies the same per-token reachability as Offered (the server allowlist and
// resource-group provisioning of visibleMappingViews), and then splits the two
// model_settings suppression values apart, because only one of them is about
// access at all:
//
//   - "hidden" (and a rule's HideTarget) is DISPLAY ONLY — the name drops out
//     of the listing and still routes perfectly. It stays in Callable, and an
//     active group stays regardless of the group name's own display visibility.
//     Asking Offered instead would fire the redirect on a request the token was
//     entitled to serve and reroute it away from a working model.
//   - "locked" is a real ACCESS boundary — the name is group-only, and a direct
//     request for it is refused with routing.ErrNoModelRoute (see isLocked). It
//     is therefore NOT callable and comes out. It is still Existing, which is
//     what keeps it the "exists but you cannot call it" case that
//     UnknownModelRedirectBlocked exists to redirect.
//
// A locked model reachable via a group does not sneak back in through the group
// path: the group overlay contributes the GROUP's own name (with its members'
// flavor union), never the member names, so a locked member's own name has only
// the one entry — its own — and that entry is dropped here.
//
// Existing is every name that exists at all for that flavor, deliberately
// WITHOUT the per-token visibility filter and WITHOUT the listing switches —
// only that separation lets the redirect tell "no such model" from "not yours".
//
// So Callable ⊆ Existing, while Offered is neither a subset nor a superset of
// Callable: it loses the suppressed names and gains the override aliases.
type ModelOffering struct {
	Offered  map[string]struct{} // names this token sees listed for one flavor
	Callable map[string]struct{} // names this token can actually route to
	Existing map[string]struct{} // names that exist at all for that flavor
}

// applyOverrideAliases overlays a token's override rules onto a flavor set:
// every rule with Offer adds its requested name, inheriting its target's
// flavors; every rule with HideTarget drops the target's own name.
//
// Flavors come from the pre-suppression set on purpose. A target hidden by
// visibility is not listed under its own name but stays callable, and the alias
// is a DIFFERENT name that does not reveal it — so an explicitly offered alias
// is listed even then.
//
// With several rules onto one target, HideTarget from any of them hides it: a
// set switch is an instruction, an unset one merely its absence. Hiding is
// therefore applied after all the adding, never interleaved with it.
//
// This is a LISTING overlay, never an access control: a hidden target stays
// callable under its real name, exactly as before this feature.
func applyOverrideAliases(sets, preSuppress map[string]map[string]struct{}, rules map[string]auth.ModelOverrideRule) {
	if len(rules) == 0 {
		return
	}
	hidden := make(map[string]struct{})
	for name, rule := range rules {
		flavors, ok := preSuppress[rule.To]
		if !ok {
			continue // target does not exist (or is not visible here): an alias would be a dead name
		}
		if rule.Offer {
			sets[name] = flavors
		}
		if rule.HideTarget {
			hidden[rule.To] = struct{}{}
		}
	}
	for name := range hidden {
		delete(sets, name)
	}
}

// ModelOfferingFor answers the two questions the unknown-model redirect asks
// about a requested name: does this token see it, and does it exist at all.
// Existing deliberately ignores per-token visibility and the listing switches —
// only then can the redirect tell "no such model" from "not yours".
//
// ALL OR NOTHING. On any store error BOTH sets come back empty; the function
// never returns a half-built answer. A populated Offered beside an empty
// Existing would tell the redirect that every name this token can use is
// simultaneously unknown, and it would redirect all of them. Because the caller
// cannot distinguish a partial result from a real one, the only safe partial
// result is none.
//
// This is deliberately NOT the fail-open that the listing does. ModelsForFlavor
// falls back to seedModelNames on a store error and modelFlavorSets proceeds
// without groups or suppression when the overlay read fails, because a glitch
// must never blank the model list a user is looking at. Here the failure
// direction is reversed: empty sets make the redirect DECLINE and the client
// see today's ordinary error, instead of a request being sent somewhere
// unintended. Same store, opposite safe direction, on purpose.
//
// The one shared fallback is the unconfigured routing store, where it mirrors
// ModelsForFlavor's seed models so both answers agree with what /v1/models
// actually served.
//
// Cost: one mapping traversal (activeMappingViews) and one group-overlay load,
// both shared between the two sets — this sits on the per-request path in the
// redirect and Service caches nothing.
func (s *Service) ModelOfferingFor(ctx context.Context, token auth.Token, flavor string) ModelOffering {
	empty := ModelOffering{Offered: map[string]struct{}{}, Callable: map[string]struct{}{}, Existing: map[string]struct{}{}}
	if s.routes == nil {
		if !isKnownAPIFlavor(flavor) {
			return empty
		}
		out := ModelOffering{Offered: map[string]struct{}{}, Callable: map[string]struct{}{}, Existing: map[string]struct{}{}}
		for _, name := range seedModelNames {
			out.Offered[name] = struct{}{}
			out.Callable[name] = struct{}{}
			out.Existing[name] = struct{}{}
		}
		return out
	}
	// One traversal feeds all three sets: Existing needs the unfiltered views,
	// Offered and Callable the resource-group-filtered ones, and the filter is a
	// pure post-step over the same slice (the identical one visibleMappingViews
	// applies — see filterVisibleMappingViews).
	views, err := s.activeMappingViews(ctx)
	if err != nil {
		return empty
	}
	visible, err := s.filterVisibleMappingViews(ctx, token, views)
	if err != nil {
		return empty
	}
	// One overlay load feeds both sides too: its store reads do not depend on
	// the flavor map it is built against, so it is loaded once and built twice.
	// Unlike the listing, an overlay failure is FATAL here — the group NAMES
	// come from it, and an Existing set missing every group would make a
	// perfectly valid group name look unknown to the redirect.
	overlay, err := s.loadGroupOverlayInputs(ctx)
	if err != nil {
		return empty
	}
	// Offered is composed exactly as the listing composes it (same function),
	// so the two can never disagree about what this token sees. Callable is that
	// same composition's OTHER half — the pre-suppression map it already builds
	// for the alias overlay, which is precisely "token-filtered, but before the
	// model_settings hidden/locked names were dropped, and with every active
	// group regardless of its display visibility" — minus the locked names,
	// re-dropped below. No extra store read: both come out of the one call below
	// that was already being made, and the visibility map the locked filter
	// needs is the overlay's own, already loaded.
	sets, callable := flavorSetsFromViews(visible, &overlay, token)
	out := ModelOffering{Offered: map[string]struct{}{}, Callable: map[string]struct{}{}, Existing: map[string]struct{}{}}
	for name, flavors := range sets {
		if _, ok := flavors[flavor]; ok {
			out.Offered[name] = struct{}{}
		}
	}
	for name, flavors := range callable {
		if _, ok := flavors[flavor]; !ok {
			continue
		}
		// The one suppression value that must NOT survive into the access set.
		// "locked" is group-only: a direct request for it is refused outright
		// (DirectAllowed → ErrNoModelRoute), for a group name exactly as for a
		// model name. Keeping it here would both stop widened mode redirecting
		// the very "exists but you cannot call it" case it is for, AND make a
		// locked name an eligible redirect TARGET — rerouting the request onto a
		// name that then fails to route, under a model the client never sent.
		// "hidden" stays: it is display-only and routes fine.
		if isLocked(overlay.visByLower[strings.ToLower(strings.TrimSpace(name))]) {
			continue
		}
		out.Callable[name] = struct{}{}
	}
	// Existing is built WITHOUT the token filter and without the visibility
	// overlay: a model the token cannot see still exists, and conflating the two
	// would make every invisible model look unknown. Hence the unfiltered views
	// and the group overlay's suppress set discarded.
	all := perNameFlavors(views)
	for name, flavors := range all {
		if _, ok := flavors[flavor]; ok {
			out.Existing[name] = struct{}{}
		}
	}
	// Groups share the model namespace, so a group name exists too. The overlay
	// is built against the full per-name flavor map — a group is only offered
	// once it has an offerable member, so building it against anything less (an
	// empty map, say) would silently yield no groups at all. Visibility is
	// ignored on purpose: a hidden group is still a name that exists.
	entries, _ := buildGroupOverlay(overlay, all)
	for _, e := range entries {
		if _, ok := e.Flavors[flavor]; ok {
			out.Existing[e.Name] = struct{}{}
		}
	}
	return out
}
