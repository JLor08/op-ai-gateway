// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/auth"
)

// ModelOffering answers the two questions the unknown-model redirect asks about
// a requested model name, for ONE API flavor.
//
// Offered is what this token sees and uses: the per-flavor discovery listing
// (/v1/models et al.), including the token's own offered override aliases and
// minus the targets it hides.
//
// Existing is every name that exists at all for that flavor, deliberately
// WITHOUT the per-token visibility filter and WITHOUT the listing switches —
// only that separation lets the redirect tell "no such model" from "not yours".
type ModelOffering struct {
	Offered  map[string]struct{} // names this token sees/uses for one flavor
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
// Fails open like the listing it shares its source with: a store error yields
// empty sets, which makes the redirect decline rather than send a request
// somewhere unintended. With no routing store configured it mirrors
// ModelsForFlavor's seed fallback, so both answers stay consistent with what
// /v1/models actually served.
func (s *Service) ModelOfferingFor(ctx context.Context, token auth.Token, flavor string) ModelOffering {
	out := ModelOffering{Offered: map[string]struct{}{}, Existing: map[string]struct{}{}}
	if s.routes == nil {
		if isKnownAPIFlavor(flavor) {
			for _, name := range seedModelNames {
				out.Offered[name] = struct{}{}
				out.Existing[name] = struct{}{}
			}
		}
		return out
	}
	sets, err := s.modelFlavorSets(ctx, token)
	if err != nil {
		return out
	}
	for name, flavors := range sets {
		if _, ok := flavors[flavor]; ok {
			out.Offered[name] = struct{}{}
		}
	}
	// Existing is built WITHOUT the token filter and without the visibility
	// overlay: a model the token cannot see still exists, and conflating the two
	// would make every invisible model look unknown. Hence activeMappingViews
	// (token-less) rather than visibleMappingViews, and the group overlay's
	// suppress set discarded.
	views, err := s.activeMappingViews(ctx)
	if err != nil {
		return out
	}
	all := make(map[string]map[string]struct{})
	for _, view := range views {
		name := view.mapping.GatewayModelName
		if _, ok := all[name]; !ok {
			all[name] = make(map[string]struct{})
		}
		for _, f := range view.app.APIFlavors {
			if isKnownAPIFlavor(f) {
				all[name][f] = struct{}{}
			}
		}
	}
	for name, flavors := range all {
		if _, ok := flavors[flavor]; ok {
			out.Existing[name] = struct{}{}
		}
	}
	// Groups share the model namespace, so a group name exists too. The overlay
	// needs the full per-name flavor map to work from — a group is only offered
	// once it has an offerable member, so passing anything less (an empty map,
	// say) would silently yield no groups at all. Visibility is ignored on
	// purpose here: a hidden group is still a name that exists.
	if entries, _, gErr := s.modelGroupOverlay(ctx, all); gErr == nil {
		for _, e := range entries {
			if _, ok := e.Flavors[flavor]; ok {
				out.Existing[e.Name] = struct{}{}
			}
		}
	}
	return out
}
