// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"strings"
)

// ModelOffering answers the questions the unknown-model redirect asks about a
// requested model name, for ONE API flavor and the capabilities the request
// requires. The sets are deliberately distinct, and confusing them produces a
// wrong redirect.
//
// Callable is the ACCESS set: what this token can actually route to under its
// real name, i.e. exactly the names a direct request can succeed with. It
// applies the same per-token reachability the LISTING does (the server
// allowlist and resource-group provisioning of visibleMappingViews), and then
// splits the two model_settings suppression values apart, because only one of
// them is about access at all:
//
//   - "hidden" (and a rule's HideTarget) is DISPLAY ONLY — the name drops out
//     of the listing and still routes perfectly. It stays in Callable, and an
//     active group stays regardless of the group name's own display visibility.
//     Asking the listing instead would fire the redirect on a request the token
//     was entitled to serve and reroute it away from a working model.
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
// So Callable ⊆ Existing.
//
// THE LISTING SET IS NOT HERE, on purpose. What a token sees advertised —
// ModelsForFlavor / Models(), which drop the suppressed names and add the
// token's own offered override aliases — is neither a subset nor a superset of
// Callable, and it answers no question the redirect asks: an alias is rewritten
// before routing, so it is not a routable name, and a suppressed name routes
// fine. This type carried an `Offered` field mirroring that listing until it
// was found to have no production reader at all, only per-request cost. The
// listing has one composition (flavorSetsFromViews) and one set of consumers
// (the discovery endpoints), and asking THOSE is how you ask about the listing;
// see the visibility matrix on Service.Models in service.go.
//
// Capable is Callable narrowed to the names that carry EVERY capability the
// request requires, and it answers only the redirect's CANDIDATE question. A
// request that requires a capability (today only /v1/images/generations, which
// requires "image") is refused by the capability gate for a model without it,
// so a LastUsedModel or fallback outside Capable would turn a legible "unknown
// model" into a 404 model_not_capable about a model the client never named.
// The flavor cannot answer this: Callable for openai_images holds every model
// of an application that declares the flavor, verdict or not. Capable is judged
// by the listing's own image fold (imageFlagsByName, groupCapabilityFlags), the
// rule behind ModelDTO.Image, so a name is a candidate exactly when the portal
// lists it as generating images. With no required capability Capable is
// Callable itself. A required capability that has no fold makes Capable empty:
// nothing is known to carry it, so nothing is a candidate. The requested-name
// question never asks Capable: a callable name without the capability is the
// client's own model, and its model_not_capable answer names it.
//
// One caller outside the redirect uses Callable: callableModelNames, the
// configuration-time guard for every model-valued token setting — same
// question ("can this name be routed to directly"), same answer.
type ModelOffering struct {
	Callable map[string]struct{} // names this token can actually route to
	Capable  map[string]struct{} // the Callable names carrying every required capability
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
// CONSEQUENCE of that deferred hide pass, worth naming because it looks like a
// bug until you see it: hiding is by NAME, and an alias name lives in the same
// namespace as a model name. So if one rule OFFERS the alias "A" and another
// rule names "A" as its target with HideTarget set, the second rule wins — "A"
// is added by the first pass and removed by the second, and the token sees
// neither. This is deterministic (map iteration order cannot affect it, which
// is exactly why the hide pass is deferred), it matches "a set switch is an
// instruction", and it is the same outcome an operator would get by hiding a
// real model of that name. It is not silently reordered or half-applied.
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
			// COPIED, not aliased. Assigning preSuppress[rule.To] by reference
			// would make the alias entry and its target the SAME map object —
			// and two aliases onto one target the same object as each other —
			// so any later per-name flavor edit would silently reach names it
			// was never applied to. Nothing mutates these today; a copy costs a
			// few entries per offered alias and removes the hazard instead of
			// leaving a note about it.
			copied := make(map[string]struct{}, len(flavors))
			for flavor := range flavors {
				copied[flavor] = struct{}{}
			}
			sets[name] = copied
		}
		if rule.HideTarget {
			hidden[rule.To] = struct{}{}
		}
	}
	for name := range hidden {
		delete(sets, name)
	}
}

// ModelOfferingFor answers the questions the unknown-model redirect asks: can
// this token route to a name, does the name exist at all, and does it carry
// every capability in required (see ModelOffering.Capable). Existing
// deliberately ignores per-token visibility and the listing switches — only
// then can the redirect tell "no such model" from "not yours".
//
// ALL OR NOTHING, for the mapping/visibility/group-overlay/capability-row
// reads this function makes directly: on a store error from any of those,
// EVERY set comes back empty, the function never returns a half-built
// answer. A populated Callable beside an empty Existing would tell the
// redirect that every name this token can use is simultaneously unknown, and
// it would redirect all of them — then hand each one a perfectly good
// candidate to go to. Because the caller cannot distinguish a partial result
// from a real one, the only safe partial result is none.
//
// ONE EXCEPTION: the per-application runtime-spec read the capability fold
// makes for a server_agent application (RuntimeSpecsByApplication, inside
// capableNames -> runtimeSpecFlavorsForViews) does NOT push this function to
// the all-or-nothing empty result on failure. It degrades PER APPLICATION
// instead — it logs and drops only that application's mappings from Capable
// (fail-closed, mirroring the listing's own image fold), leaving Callable
// and Existing untouched and every other application's Capable entries
// intact. Measured with a failing RuntimeSpecsByApplication for one
// server_agent application among several mappings:
// Callable={agent-image, plain-image}, Capable={plain-image}.
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
// both shared between the sets — this sits on the per-request path in the
// redirect and Service caches nothing. A required capability adds the image
// fold's own reads, the same the listing makes: one batch capability read and
// one runtime-spec read per server_agent application.
func (s *Service) ModelOfferingFor(ctx context.Context, token auth.Token, flavor string, required []string) ModelOffering {
	if s.routes == nil {
		return seedModelOffering(flavor, required)
	}
	// One traversal feeds both sets: Existing needs the unfiltered views,
	// Callable the resource-group-filtered ones, and the filter is a pure
	// post-step over the same slice (the identical one visibleMappingViews
	// applies — see filterVisibleMappingViews).
	views, err := s.activeMappingViews(ctx)
	if err != nil {
		return emptyModelOffering()
	}
	visible, err := s.filterVisibleMappingViews(ctx, token, views)
	if err != nil {
		return emptyModelOffering()
	}
	// One overlay load feeds both sides too: its store reads do not depend on
	// the flavor map it is built against, so it is loaded once and built twice.
	// Unlike the listing, an overlay failure is FATAL here — the group NAMES
	// come from it, and an Existing set missing every group would make a
	// perfectly valid group name look unknown to the redirect.
	overlay, err := s.loadGroupOverlayInputs(ctx)
	if err != nil {
		return emptyModelOffering()
	}
	// Callable comes out of the LISTING's own composition (same function), so the
	// two can never disagree about what this token reaches. It is that
	// composition's OTHER half — the pre-suppression map it already builds for
	// the alias overlay, which is precisely "token-filtered, but before the
	// model_settings hidden/locked names were dropped, and with every active
	// group regardless of its display visibility" — minus the locked names,
	// re-dropped by callableNamesForFlavor. No extra store read: it comes out of
	// the one call below that was already being made, and the visibility map the
	// locked filter needs is the overlay's own, already loaded.
	//
	// The finished LISTING that same call also produces is discarded here (`_`):
	// no question this type answers is about the listing, and composing it is
	// how the pre-suppression map comes to exist at all — it is a by-product of
	// one call, not a second computation. Reusing the listing's own composition
	// is what keeps Callable from drifting away from what the token really
	// reaches.
	_, preSuppress := flavorSetsFromViews(visible, &overlay, token)
	callable := callableNamesForFlavor(preSuppress, flavor, overlay.visByLower)
	capable, err := s.capableNames(ctx, callable, visible, overlay, required)
	if err != nil {
		return emptyModelOffering()
	}
	return ModelOffering{
		Callable: callable,
		Capable:  capable,
		Existing: existingNamesForFlavor(views, overlay, flavor),
	}
}

// capableNames narrows callable to the names that carry every capability in
// required (ModelOffering.Capable). It folds over the same token-filtered views
// the listing folds over for this token (Models() reads visibleMappingViews),
// with the listing's own rules: imageFlagsByName for a model name, and
// groupCapabilityFlags over the group overlay for a group name. "image" is the
// only capability a request requires today and the only one with a fold here;
// any other makes the result empty (fail-closed). An error is a failed store
// read, which the caller turns into the all-or-nothing empty offering.
func (s *Service) capableNames(ctx context.Context, callable map[string]struct{}, views []mappingView, overlay groupOverlayInputs, required []string) (map[string]struct{}, error) {
	if len(required) == 0 {
		return callable, nil
	}
	for _, capability := range required {
		if capability != routing.CapabilityImage {
			return map[string]struct{}{}, nil
		}
	}
	mappingIDs := make([]string, len(views))
	for i, view := range views {
		mappingIDs[i] = view.mapping.ID
	}
	capsByMapping, err := s.routes.MappingCapabilitiesForMappings(ctx, mappingIDs)
	if err != nil {
		return nil, err
	}
	specFlavors, specFailed := s.runtimeSpecFlavorsForViews(ctx, views)
	flags := imageFlagsByName(views, capabilityRowsFor(capsByMapping, routing.CapabilityImage), specFlavors, specFailed)
	entries, _ := buildGroupOverlay(overlay, perNameFlavors(views))
	for name, flag := range groupCapabilityFlags(entries, flags) {
		flags[name] = flag
	}
	out := make(map[string]struct{})
	for name := range callable {
		if flags[name] {
			out[name] = struct{}{}
		}
	}
	return out, nil
}

// emptyModelOffering is the ALL-OR-NOTHING store-error result: every set empty,
// never a half-built answer. See ModelOfferingFor for why the failure direction
// is the opposite of the listing's fail-open.
func emptyModelOffering() ModelOffering {
	return ModelOffering{Callable: map[string]struct{}{}, Capable: map[string]struct{}{}, Existing: map[string]struct{}{}}
}

// seedModelOffering is the unconfigured-routing-store fallback, the one place
// where the sets agree with the listing by construction: it mirrors
// ModelsForFlavor's seed models so the answers match what /v1/models actually
// served. The seeds expose the text flavors (seedAPIFlavors), so on those each
// is both callable and existing; any other flavor, openai_images included,
// offers nothing at all. The seeds carry no capability, so with a required
// capability none of them is Capable.
func seedModelOffering(flavor string, required []string) ModelOffering {
	out := emptyModelOffering()
	if !isSeedAPIFlavor(flavor) {
		return out
	}
	for _, name := range seedModelNames {
		out.Callable[name] = struct{}{}
		out.Existing[name] = struct{}{}
	}
	if len(required) == 0 {
		out.Capable = out.Callable
	}
	return out
}

// callableNamesForFlavor narrows the listing composition's pre-suppression map
// — "token-filtered, but before the model_settings hidden/locked names were
// dropped, and with every active group regardless of its display visibility" —
// to the ACCESS set for one flavor. See ModelOffering.Callable.
func callableNamesForFlavor(preSuppress map[string]map[string]struct{}, flavor string, visByLower map[string]string) map[string]struct{} {
	out := make(map[string]struct{})
	for name, flavors := range preSuppress {
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
		if isLocked(visByLower[strings.ToLower(strings.TrimSpace(name))]) {
			continue
		}
		out[name] = struct{}{}
	}
	return out
}

// existingNamesForFlavor is every name that exists at all for one flavor: the
// models plus the group names.
//
// It is built WITHOUT the token filter and without the visibility overlay: a
// model the token cannot see still exists, and conflating the two would make
// every invisible model look unknown. Hence the caller's UNFILTERED views and
// the group overlay's suppress set discarded.
func existingNamesForFlavor(views []mappingView, overlay groupOverlayInputs, flavor string) map[string]struct{} {
	out := make(map[string]struct{})
	all := perNameFlavors(views)
	for name, flavors := range all {
		if _, ok := flavors[flavor]; ok {
			out[name] = struct{}{}
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
			out[e.Name] = struct{}{}
		}
	}
	return out
}
