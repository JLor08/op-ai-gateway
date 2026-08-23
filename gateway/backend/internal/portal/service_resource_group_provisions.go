// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"strings"
)

// AllowedServerIDs returns, for the given candidate server ids, which the
// principal may USE under resource-group provisioning + the enforcement mode
// (Resource Groups Phase 2, spec 2026-08-12-resource-groups-phase-2-provisioning).
// The returned map has an entry (true) for every id in serverIDs the principal
// is allowed to use; an id absent (or false) is disallowed. This is the
// concrete implementation of the routing.ProvisioningGate seam (Task 2) — the
// gateway adapter (internal/gateway) delegates here via the portal.API
// interface.
//
// Phase 2: NO role bypass — system-scope + server/RG owner go through the
// exact same logic as any other principal (a later phase could add one; see
// the two marked lines below, the single intended extension point).
//
// Enforcement mode (ResourceProvisioningEnforce, system setting, default
// false):
//   - opt-in (enforce=false): a candidate is allowed if EITHER it is not a
//     member of ANY provisioned resource group (unrestricted — today's
//     behavior is preserved for servers nobody has opted into provisioning
//     for), OR it is a member of a resource group the principal is
//     provisioned into.
//   - deny (enforce=true): deny-by-default — a candidate is allowed ONLY if
//     it is a member of a resource group the principal is provisioned into.
//
// Fail direction follows the mode: a store error while resolving the
// principal's provisioned resource groups (or, in opt-in mode, while
// resolving the global provisioned-RG set) leans ALLOW under opt-in (keeps
// today's behavior — a settings/store glitch must not silently start
// rejecting traffic that was never gated before) and leans DENY under
// enforce (a store glitch must never leak access under deny-by-default).
func (s *Service) AllowedServerIDs(ctx context.Context, principal auth.Token, serverIDs []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(serverIDs) == 0 {
		return out, nil
	}
	candidate := map[string]bool{}
	for _, id := range serverIDs {
		candidate[id] = true
	}
	enforce := s.ResourceProvisioningEnforce(ctx)

	rgIDs, err := s.provisionedRGsForPrincipal(ctx, principal)
	if err != nil {
		// Fail direction follows the mode: opt-in leans ALLOW, deny leans DENY.
		if enforce {
			return out, nil
		}
		for id := range candidate {
			out[id] = true
		}
		return out, nil
	}
	allowed := map[string]bool{}
	for rgID := range rgIDs {
		servers, err := s.routes.ResourceGroupServers(ctx, rgID)
		if err != nil {
			if enforce {
				return out, nil
			}
			for id := range candidate {
				out[id] = true
			}
			return out, nil
		}
		for _, sid := range servers {
			if candidate[sid] {
				allowed[sid] = true
			}
		}
	}
	if enforce {
		// deny-by-default: only explicitly allowed candidates. (bypass would go here)
		for id := range candidate {
			if allowed[id] {
				out[id] = true
			}
		}
		return out, nil
	}
	// opt-in: allow a candidate that is allowed OR not in ANY provisioned RG.
	restricted := map[string]bool{}
	provRGs, err := s.routes.ProvisionedResourceGroupIDs(ctx)
	if err != nil {
		for id := range candidate {
			out[id] = true
		}
		return out, nil // fail-open in opt-in
	}
	for rgID := range provRGs {
		servers, err := s.routes.ResourceGroupServers(ctx, rgID)
		if err != nil {
			for id := range candidate {
				out[id] = true
			}
			return out, nil
		}
		for _, sid := range servers {
			if candidate[sid] {
				restricted[sid] = true
			}
		}
	}
	for id := range candidate {
		if allowed[id] || !restricted[id] {
			out[id] = true // (bypass would go here too)
		}
	}
	return out, nil
}

// provisionedRGsForPrincipal returns the resource-group ids the principal is
// provisioned into. A SERVICE principal matches ONLY a direct `service`
// provision target (never via any group — a service is not a member of a
// user/admin group). A USER principal matches: a direct `user` provision
// target, PLUS every user-tier group it is a MEMBER of (routing.ProvisionKindUserGroup),
// PLUS every admin-tier group it is a MEMBER of (routing.ProvisionKindAdminGroup).
// Only store.GroupStateMember counts — an invited-but-not-accepted membership
// never grants provisioning.
func (s *Service) provisionedRGsForPrincipal(ctx context.Context, principal auth.Token) (map[string]bool, error) {
	out := map[string]bool{}
	add := func(kind string, ids []string) error {
		if len(ids) == 0 {
			return nil
		}
		rgs, err := s.routes.ResourceGroupIDsByProvisionTargets(ctx, kind, ids)
		if err != nil {
			return err
		}
		for _, rg := range rgs {
			out[rg] = true
		}
		return nil
	}
	if principal.IsService() {
		if principal.ServiceID == "" {
			return out, nil
		}
		return out, add(routing.ProvisionKindService, []string{principal.ServiceID})
	}
	if principal.UserID == "" {
		return out, nil
	}
	if err := add(routing.ProvisionKindUser, []string{principal.UserID}); err != nil {
		return nil, err
	}
	if s.groups != nil {
		userGroups, err := s.groups.UserGroupsForUser(ctx, principal.UserID, store.GroupTierUser, store.GroupStateMember)
		if err != nil {
			return nil, err
		}
		if err := add(routing.ProvisionKindUserGroup, groupIDs(userGroups)); err != nil {
			return nil, err
		}
		adminGroups, err := s.groups.UserGroupsForUser(ctx, principal.UserID, store.GroupTierAdmin, store.GroupStateMember)
		if err != nil {
			return nil, err
		}
		if err := add(routing.ProvisionKindAdminGroup, groupIDs(adminGroups)); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// groupIDs extracts the ids from a []store.UserGroup slice.
func groupIDs(groups []store.UserGroup) []string {
	ids := make([]string, 0, len(groups))
	for _, g := range groups {
		ids = append(ids, g.ID)
	}
	return ids
}

// --- Resource Groups Phase 2 -- provisioning MANAGEMENT endpoints (spec
// 2026-08-12-resource-groups-phase-2-provisioning, Task 5): read/replace a
// resource group's "provisioned-for" target set (resource_group_provisions,
// Task 1), plus a combined candidate list for the editor. All three methods
// below reuse authorizeResourceGroup (the Phase-1 RG object-gate,
// UNCHANGED) as their FIRST gate -- a stranger/unknown resource-group id
// gets the identical ErrResourceGroupNotFound 404-no-leak the
// admin-group/server-linkage endpoints already use. This is orthogonal to
// AllowedServerIDs/provisionedRGsForPrincipal above (Task 3): those read the
// provisioning links at REQUEST time to gate server usability; these
// methods are the admin-side CRUD over the same links. ------------------

// ErrResourceGroupProvisionTargetInvalid is returned by
// SetResourceGroupProvisions when a supplied (kind, target_id) pair is
// either an unrecognized kind, a blank target id, or a kind/target pair the
// CALLER cannot see. "Cannot see" is deliberately narrower than "does not
// exist" -- resourceGroupProvisionVisibleTargets checks the CALLER'S OWN
// visible landscape (VisibleUserIDs / the caller's own group-tier landscape
// / ListServices), never a raw existence check, so a caller can never probe
// whether an out-of-reach id exists by trying to provision for it. The
// check is all-or-nothing: the FIRST invalid pair aborts the WHOLE call
// before s.routes.SetResourceGroupProvisions is ever invoked, so a rejected
// PUT leaves the stored set byte-unchanged.
var ErrResourceGroupProvisionTargetInvalid = errors.New("resource_group.provision_target_invalid")

// ResourceGroupProvisionDTO is one "provisioned-for" target of a resource
// group, as returned by ResourceGroupProvisionsView. TargetName is resolved
// for display, BEST-EFFORT (a vanished/unresolvable target, or an s.groups
// dependency that is nil, reads back "" -- never fails the whole list; see
// resourceGroupProvisionTargetName).
type ResourceGroupProvisionDTO struct {
	Kind       string `json:"kind"`
	TargetID   string `json:"target_id"`
	TargetName string `json:"target_name"`
}

// ResourceGroupProvisionsView lists resource group id's provisioned-for
// targets (Kind + TargetID + resolved display name) -- the *Read*
// object-gate (authorizeResourceGroup, 404-no-leak) mirrors
// GetResourceGroup/ResourceGroupServerCandidates exactly. Always returns a
// non-nil (possibly empty) slice.
func (s *Service) ResourceGroupProvisionsView(ctx context.Context, principal auth.Token, id string) ([]ResourceGroupProvisionDTO, error) {
	if _, err := s.authorizeResourceGroup(ctx, principal, id); err != nil {
		return nil, err
	}
	provisions, err := s.routes.ResourceGroupProvisions(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]ResourceGroupProvisionDTO, 0, len(provisions))
	for _, p := range provisions {
		out = append(out, ResourceGroupProvisionDTO{
			Kind:       p.Kind,
			TargetID:   p.TargetID,
			TargetName: s.resourceGroupProvisionTargetName(ctx, p),
		})
	}
	return out, nil
}

// resourceGroupProvisionTargetName BEST-EFFORT resolves a provision
// target's display name for ResourceGroupProvisionsView -- an unknown kind,
// a vanished target, or a nil s.groups dependency all read back "" rather
// than failing the whole list.
func (s *Service) resourceGroupProvisionTargetName(ctx context.Context, p routing.ResourceGroupProvision) string {
	switch p.Kind {
	case routing.ProvisionKindUser:
		u, err := s.users.UserByID(ctx, p.TargetID)
		if err != nil {
			return ""
		}
		if u.DisplayName != "" {
			return u.DisplayName
		}
		return u.Email
	case routing.ProvisionKindUserGroup, routing.ProvisionKindAdminGroup:
		if s.groups == nil {
			return ""
		}
		g, err := s.groups.UserGroupByID(ctx, p.TargetID)
		if err != nil {
			return ""
		}
		return g.Name
	case routing.ProvisionKindService:
		svc, err := s.routes.ServiceByID(ctx, p.TargetID)
		if err != nil {
			return ""
		}
		return svc.Name
	default:
		return ""
	}
}

// resourceGroupProvisionVisibleTargets computes the CALLER's own visible
// target set for each of the four provision kinds -- the shared basis for
// BOTH SetResourceGroupProvisions' validation and
// ResourceGroupProvisionCandidates' picker, so the picker never offers a
// target the write path would reject:
//
//   - user -> VisibleUserIDs(principal) (self + the members principal
//     already sees, per the user-groups visibility model -- system sees
//     everyone).
//   - user_group / admin_group -> principal's OWN group landscape
//     (ListGroups), restricted to that tier -- covers every group the
//     principal owns, co-manages, or is a member of, PLUS (admin scope, user
//     tier only) every user group parented by one of the principal's own
//     admin groups (ListGroups' read-only parent-linked expansion).
//   - service -> ListServices(principal) (delegate-reachable UNION
//     admin-group-manageable services; system sees every service).
func (s *Service) resourceGroupProvisionVisibleTargets(ctx context.Context, principal auth.Token) (users, userGroups, adminGroups, services map[string]bool, err error) {
	users, err = s.VisibleUserIDs(ctx, principal)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	landscape, err := s.ListGroups(ctx, principal)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	userGroups = make(map[string]bool, len(landscape.User))
	for _, g := range landscape.User {
		userGroups[g.ID] = true
	}
	adminGroups = make(map[string]bool, len(landscape.Admin))
	for _, g := range landscape.Admin {
		adminGroups[g.ID] = true
	}
	svcList, err := s.ListServices(ctx, principal)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	services = make(map[string]bool, len(svcList))
	for _, svc := range svcList {
		services[svc.ID] = true
	}
	return users, userGroups, adminGroups, services, nil
}

// SetResourceGroupProvisions atomically REPLACES resource group id's whole
// provisioned-for target set -- the *Write* object-gate
// (authorizeResourceGroup) runs FIRST (404-no-leak), THEN every supplied
// (kind, target_id) pair is validated against the caller's OWN visible
// landscape (resourceGroupProvisionVisibleTargets) BEFORE anything is
// written: a blank target id, an unrecognized kind, or a target the caller
// cannot see all abort the WHOLE call with ErrResourceGroupProvisionTargetInvalid
// -- s.routes.SetResourceGroupProvisions is never reached on a rejected
// call, so the stored set stays byte-unchanged (all-or-nothing, mirroring
// SetResourceGroupServers/SetResourceGroupAdminGroups' own
// validate-everything-before-any-write ordering). A blank/nil provisions
// slice clears the whole set (s.routes.SetResourceGroupProvisions's own
// documented behavior).
func (s *Service) SetResourceGroupProvisions(ctx context.Context, principal auth.Token, id string, provisions []routing.ResourceGroupProvision) error {
	if _, err := s.authorizeResourceGroup(ctx, principal, id); err != nil {
		return err
	}
	visibleUsers, visibleUserGroups, visibleAdminGroups, visibleServices, err := s.resourceGroupProvisionVisibleTargets(ctx, principal)
	if err != nil {
		return err
	}
	normalized := make([]routing.ResourceGroupProvision, 0, len(provisions))
	for _, p := range provisions {
		kind := strings.TrimSpace(p.Kind)
		targetID := strings.TrimSpace(p.TargetID)
		if targetID == "" {
			return ErrResourceGroupProvisionTargetInvalid
		}
		var ok bool
		switch kind {
		case routing.ProvisionKindUser:
			ok = visibleUsers[targetID]
		case routing.ProvisionKindUserGroup:
			ok = visibleUserGroups[targetID]
		case routing.ProvisionKindAdminGroup:
			ok = visibleAdminGroups[targetID]
		case routing.ProvisionKindService:
			ok = visibleServices[targetID]
		}
		if !ok {
			return ErrResourceGroupProvisionTargetInvalid
		}
		normalized = append(normalized, routing.ResourceGroupProvision{Kind: kind, TargetID: targetID})
	}
	return s.routes.SetResourceGroupProvisions(ctx, id, normalized)
}

// ResourceGroupProvisionCandidatesDTO carries the caller-visible candidate
// set for EACH of the four provisioning target kinds, scoped to resource
// group id's editor picker (ResourceGroupProvisionCandidates) -- a single
// COMBINED endpoint (Task 5, Step 4: the frontend editor needs all four
// target types at once, so a combined round-trip is preferred over four
// separate per-type candidate calls). Services reuse the generic
// {id,name} GroupRefDTO shape (no distinct ServiceRefDTO exists in this
// codebase; the JSON field is still named "services"). Every field is
// always non-nil ([] when empty).
type ResourceGroupProvisionCandidatesDTO struct {
	Users       []UserRefDTO  `json:"users"`
	UserGroups  []GroupRefDTO `json:"user_groups"`
	AdminGroups []GroupRefDTO `json:"admin_groups"`
	Services    []GroupRefDTO `json:"services"`
}

// ResourceGroupProvisionCandidates lists the users/groups/services the
// caller may provision resource group id FOR: the resource group's own
// visibility gate (authorizeResourceGroup) runs first (404-no-leak -- a
// stranger never learns even that the candidate endpoint exists), then each
// kind is populated from the EXACT SAME visible-target sets
// SetResourceGroupProvisions validates against
// (resourceGroupProvisionVisibleTargets), so the picker never offers a
// target the write path would reject.
func (s *Service) ResourceGroupProvisionCandidates(ctx context.Context, principal auth.Token, id string) (ResourceGroupProvisionCandidatesDTO, error) {
	if _, err := s.authorizeResourceGroup(ctx, principal, id); err != nil {
		return ResourceGroupProvisionCandidatesDTO{}, err
	}
	visibleUsers, visibleUserGroups, visibleAdminGroups, visibleServices, err := s.resourceGroupProvisionVisibleTargets(ctx, principal)
	if err != nil {
		return ResourceGroupProvisionCandidatesDTO{}, err
	}
	out := ResourceGroupProvisionCandidatesDTO{
		Users:       make([]UserRefDTO, 0, len(visibleUsers)),
		UserGroups:  make([]GroupRefDTO, 0, len(visibleUserGroups)),
		AdminGroups: make([]GroupRefDTO, 0, len(visibleAdminGroups)),
		Services:    make([]GroupRefDTO, 0, len(visibleServices)),
	}
	for uid := range visibleUsers {
		u, err := s.users.UserByID(ctx, uid)
		if err != nil {
			// principal vanished between VisibleUserIDs and this lookup; skip
			// rather than fail the whole candidate list.
			continue
		}
		out.Users = append(out.Users, UserRefDTO{ID: u.ID, Email: u.Email, DisplayName: u.DisplayName})
	}
	sortUserRefs(out.Users)
	if s.groups != nil {
		for gid := range visibleUserGroups {
			g, err := s.groups.UserGroupByID(ctx, gid)
			if err != nil {
				continue
			}
			out.UserGroups = append(out.UserGroups, GroupRefDTO{ID: g.ID, Name: g.Name})
		}
		for gid := range visibleAdminGroups {
			g, err := s.groups.UserGroupByID(ctx, gid)
			if err != nil {
				continue
			}
			out.AdminGroups = append(out.AdminGroups, GroupRefDTO{ID: g.ID, Name: g.Name})
		}
	}
	sortGroupRefs(out.UserGroups)
	sortGroupRefs(out.AdminGroups)
	for sid := range visibleServices {
		svc, err := s.routes.ServiceByID(ctx, sid)
		if err != nil {
			continue
		}
		out.Services = append(out.Services, GroupRefDTO{ID: svc.ID, Name: svc.Name})
	}
	sortGroupRefs(out.Services)
	return out, nil
}
