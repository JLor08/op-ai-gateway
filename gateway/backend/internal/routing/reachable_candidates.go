// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"context"
	"fmt"
	"op-ai-gateway/internal/auth"
)

// ReachableCandidates returns the active mapping candidates a standard dispatch
// of a request for modelName, made by principal on apiFlavor, could be routed
// to: the pool resolveStandard (and resolveGroup, member by member) selects from
// BEFORE any health, capacity, capability or scoring decision. It is for a caller
// that wants "what could this token reach" without making a request, the voices
// endpoint being the first: it reads a cache that must not offer what a dispatch
// would refuse.
//
// The rules are exactly dispatch's, in dispatch's order, and share its code:
//
//   - a GROUP name (GroupResolver.Group) is replaced by its flattened members, and
//     a name that is both a group and a model is the group, as in resolveStandard;
//   - a locked name is not reachable DIRECTLY (GroupResolver.DirectAllowed), for a
//     group exactly as for a plain model. Only the name asked about is checked:
//     a locked MEMBER stays reachable through the group that lists it, which is
//     what "group-only" means;
//   - each name's ActiveMappingsForModel pool (active mappings on active
//     applications serving apiFlavor, which is normalised as dispatch does) is cut
//     by filterProvisioned, the same method with the same gate, so a server the
//     principal may not use contributes nothing and a gate failure is returned
//     rather than read as "allowed".
//
// A mapping reachable through several members is returned once. The result is
// empty, never an error, for an unknown name, a locked one or a fully
// unprovisioned one: callers that must not tell those apart (no-leak) get one
// answer. Order is the members' order, then the store's per-name order; callers
// that need a stable order sort.
//
// Deliberately NOT applied, because each is a property of one particular request
// rather than of what the principal may reach: affinity, capability filtering,
// the speed floor and loaded-only filters, health and reachability, capacity and
// the vendor-account path. A caller wanting a capability applies the verdicts
// itself, from the rows it reads.
//
// Nil-safe: a nil resolver or one without a store reaches nothing.
func (r *Resolver) ReachableCandidates(ctx context.Context, principal auth.Token, modelName, apiFlavor string) ([]MappingCandidate, error) {
	if r == nil || r.store == nil {
		return nil, nil
	}
	names := []string{modelName}
	if r.groups != nil {
		members, _, isGroup := r.groups.Group(modelName)
		if !r.groups.DirectAllowed(modelName) {
			return nil, nil
		}
		if isGroup {
			names = names[:0]
			for _, m := range members {
				names = append(names, m.MemberGatewayName)
			}
		}
	}
	flavor := NormalizeAPIFlavor(apiFlavor)
	var out []MappingCandidate
	seen := map[string]bool{}
	for _, name := range names {
		cands, err := r.store.ActiveMappingsForModel(ctx, name, flavor)
		if err != nil {
			return nil, fmt.Errorf("resolve mappings: %w", err)
		}
		cands, err = r.filterProvisioned(ctx, principal, cands)
		if err != nil {
			return nil, err
		}
		for _, c := range cands {
			if !seen[c.Mapping.ID] {
				seen[c.Mapping.ID] = true
				out = append(out, c)
			}
		}
	}
	return out, nil
}
