// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"op-ai-gateway/internal/routing"
	"strings"
	"sync"
)

// groupEntry is one active model group's resolved members (priority-ordered) and its
// selection policy, held in the GroupRegistry snapshot.
type groupEntry struct {
	members []routing.GroupMember
	policy  routing.GroupPolicy
}

// GroupRegistry is the hot-path, in-memory view of the model-group config the resolver
// reads; it satisfies routing.GroupResolver. Unlike LoadedModelRegistry (PUSH-fed), it
// is PULL-fed: RefreshGroups reloads a fresh snapshot from the store (every ACTIVE group
// with its priority-ordered members, plus the per-model "locked" visibility set) and
// swaps it in atomically. Built once per process and shared as BOTH the portal GroupCache
// (refreshed after each group / model-setting write) AND the resolver's GroupResolver, so
// a portal CRUD refresh updates the very snapshot the resolver reads. All methods are
// nil-safe: a nil registry means no groups exist and nothing is locked, preserving the
// pre-feature single-model behavior (the No-Op invariant).
type GroupRegistry struct {
	store  routing.Store
	mu     sync.RWMutex
	groups map[string]groupEntry // keyed by lowercased gateway_model_name
	locked map[string]struct{}   // lowercased names whose ModelSetting.Visibility == "locked"
}

// NewGroupRegistry returns an empty registry backed by store. The empty snapshot is a
// valid "no groups" value handled by every method; RefreshGroups populates it.
func NewGroupRegistry(store routing.Store) *GroupRegistry {
	return &GroupRegistry{
		store:  store,
		groups: make(map[string]groupEntry),
		locked: make(map[string]struct{}),
	}
}

// RefreshGroups reloads the whole snapshot from the store: every ACTIVE group with its
// priority-ordered members, plus the set of models whose visibility is "locked" (a
// group-only model a direct request must be refused). It builds the new maps first and
// swaps them under the lock, so a concurrent Group/DirectAllowed read never observes a
// half-built snapshot. On a store error it returns the error WITHOUT mutating the current
// snapshot (a transient blip keeps the last good view). Nil-safe.
func (r *GroupRegistry) RefreshGroups(ctx context.Context) error {
	if r == nil {
		return nil
	}
	groups, err := r.store.ModelGroups(ctx)
	if err != nil {
		return err
	}
	// Build the flattening graph from ALL groups (active status preserved per-group,
	// not filtered here) so a subgroup member name is recognizable as a group in
	// routing.FlattenGroup and an inactive subgroup correctly contributes nothing when
	// expanded from an active parent.
	graph := make(map[string]routing.FlatGroup, len(groups))
	for _, g := range groups {
		members, err := r.store.GroupMembersByGroup(ctx, g.ID)
		if err != nil {
			return err
		}
		graph[strings.ToLower(strings.TrimSpace(g.GatewayModelName))] = routing.FlatGroup{
			Traversal: g.Traversal,
			Members:   members,
			Active:    g.Status == routing.ServerStatusActive,
		}
	}
	// Only ACTIVE groups are offered; each is flattened via ITS OWN traversal (and
	// every nested subgroup's own traversal, recursively) into an ordered,
	// de-duplicated leaf-model list — the resolver's failover then walks a plain
	// model list unchanged, with no group-aware logic of its own.
	nextGroups := make(map[string]groupEntry, len(groups))
	for _, g := range groups {
		if g.Status != routing.ServerStatusActive {
			continue
		}
		flat := routing.FlattenGroup(g.GatewayModelName, graph)
		members := make([]routing.GroupMember, len(flat))
		for i, name := range flat {
			members[i] = routing.GroupMember{MemberGatewayName: name, Priority: i}
		}
		// MemberOrder/MinSpeedFallback are normalised here (empty OR unrecognized ->
		// their default), matching how the resolver already fails an unknown
		// FailoverMode open to "sticky" behavior via a plain equality check.
		// ClimbSpeedMarginPercent is NOT defaulted: 0 is a legitimate "no margin
		// required" policy, not an unset sentinel.
		memberOrder := g.MemberOrder
		if memberOrder != routing.MemberOrderPriority && memberOrder != routing.MemberOrderSpeed {
			memberOrder = routing.MemberOrderPriority
		}
		minSpeedFallback := g.MinSpeedFallback
		if minSpeedFallback != routing.MinSpeedFallbackError && minSpeedFallback != routing.MinSpeedFallbackIgnore {
			minSpeedFallback = routing.MinSpeedFallbackError
		}
		nextGroups[strings.ToLower(g.GatewayModelName)] = groupEntry{
			members: members,
			policy: routing.GroupPolicy{
				FailoverMode:            g.FailoverMode,
				MemberOrder:             memberOrder,
				LoadedOnly:              g.LoadedOnly,
				ClimbSpeedMarginPercent: g.ClimbSpeedMarginPercent,
				MinTokensPerSecond:      g.MinTokensPerSecond,
				MinSpeedFallback:        minSpeedFallback,
			},
		}
	}
	settings, err := r.store.ModelSettings(ctx)
	if err != nil {
		return err
	}
	nextLocked := make(map[string]struct{})
	for _, s := range settings {
		if s.Visibility == "locked" {
			nextLocked[strings.ToLower(s.GatewayModelName)] = struct{}{}
		}
	}
	r.mu.Lock()
	r.groups = nextGroups
	r.locked = nextLocked
	r.mu.Unlock()
	return nil
}

// Group returns a COPY of a group's priority-ordered members and its selection policy
// when name is an active group (case-insensitive); ok=false otherwise. Nil-safe.
func (r *GroupRegistry) Group(name string) ([]routing.GroupMember, routing.GroupPolicy, bool) {
	if r == nil {
		return nil, routing.GroupPolicy{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.groups[strings.ToLower(name)]
	if !ok {
		return nil, routing.GroupPolicy{}, false
	}
	return append([]routing.GroupMember(nil), e.members...), e.policy, true
}

// DirectAllowed reports whether a direct (non-group) request for a model is permitted. It
// is refused ONLY when the model's visibility is "locked" (a group-only model, reachable
// solely via a group). Model-level, not membership. Nil-safe: a nil registry locks
// nothing, so a direct request is always allowed (the No-Op invariant).
func (r *GroupRegistry) DirectAllowed(name string) bool {
	if r == nil {
		return true
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, locked := r.locked[strings.ToLower(name)]
	return !locked
}
