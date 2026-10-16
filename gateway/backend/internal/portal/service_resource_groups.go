// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"sort"
	"strings"
)

// CodeResourceGroupNotFound is ErrResourceGroupNotFound's API error code,
// exported so internal/gateway/resource_group_endpoints.go can share the
// exact value instead of re-hardcoding it.
const CodeResourceGroupNotFound = "resource_group.not_found"

// ErrResourceGroupNotFound is returned by authorizeResourceGroup (and any
// caller that surfaces it) for BOTH an unknown resource-group id and a
// principal with no reach into an existing one -- the 404-no-leak contract
// shared by authorizeServer/authorizeServiceRead.
var ErrResourceGroupNotFound = errors.New(CodeResourceGroupNotFound)

// Resource-group WRITE path sentinels (Resource Groups Phase 1, Task 4, spec
// 2026-08-11) -- mirror the service/server admin-group-linkage sentinels
// (ErrServiceAdminGroup*/ErrServerAdminGroup*) exactly, keyed on
// "resource_group" instead of "service"/"server".
// ErrResourceGroupForbidden: CreateResourceGroup's authorization gate --
// a non-system principal with NO can_manage_resources reach into any admin
// group.
// ErrResourceGroupValidation: a request-shape problem (blank name, an
// unrecognized status value).
// ErrResourceGroupAdminGroupRequired: the (post-dedup) admin_group_ids set is
// empty -- every resource group, regardless of the creating/editing
// principal's scope, must be linked to at least one admin-tier group.
// ErrResourceGroupAdminGroupInvalid: an id does not resolve to an existing
// ADMIN-tier group, or (for a non-system principal) is not one the principal
// may manage resource groups through (resourceManageGroupIDs).
// ErrResourceGroupAdminGroupParentMismatch: the chosen groups do not all
// share ONE parent (system-tier) group, or (system-scope only) contradict an
// explicitly-supplied SystemGroupID cross-check, or -- for an
// ALREADY-grouped resource group -- would relocate its containment root
// (checked independently of the caller's scope; see SetResourceGroupAdminGroups).
var (
	ErrResourceGroupForbidden                = errors.New("resource_group.forbidden")
	ErrResourceGroupValidation               = errors.New("resource_group.validation_failed")
	ErrResourceGroupAdminGroupRequired       = errors.New("resource_group.admin_group_required")
	ErrResourceGroupAdminGroupInvalid        = errors.New("resource_group.admin_group_invalid")
	ErrResourceGroupAdminGroupParentMismatch = errors.New("resource_group.admin_group_parent_mismatch")
)

// Resource-group SERVER-MEMBERSHIP sentinels (Resource Groups Phase 1, Task
// 5, spec 2026-08-11) -- the one genuinely new authorization rule in this
// feature: to ENTER a server into a resource group the caller must manage
// BOTH the resource group (authorizeResourceGroup) AND the server
// (authorizeServer, service.go's existing Phase B choke-point), and the
// server must sit under the SAME system group as the resource group.
// ErrResourceGroupServerForbidden: a candidate server the caller may not
// authorizeServer-reach (a 404-no-leak on the SERVER, surfaced as this
// distinct 404-mapped sentinel so the resource-group's own not-found stays
// unambiguous) -- the caller never learns whether the server exists at all
// vs. exists-but-forbidden, since authorizeServer itself returns the same
// ErrServerNotFound in both cases.
// ErrResourceGroupServerSystemGroupMismatch: a candidate server whose
// SystemGroupID differs from (or is empty against) the resource group's --
// containment, not an existence/authorization question, hence a distinct
// 400-mapped sentinel.
var (
	ErrResourceGroupServerForbidden           = errors.New("resource_group.server_forbidden")
	ErrResourceGroupServerSystemGroupMismatch = errors.New("resource_group.server_system_group_mismatch")
)

// ErrResourceGroupVendorAccountSystemGroupMismatch is the vendor-account counterpart of
// ErrResourceGroupServerSystemGroupMismatch (#183, spec 2026-10-15 sections 5 and 9): a
// shared account whose containment root (system_group_id) is not the resource group's,
// or is empty -- containment, not an existence or authorization question, hence a 400.
// Returned by both link paths on ADD (the owner's self-service and the manager side).
var ErrResourceGroupVendorAccountSystemGroupMismatch = errors.New("resource_group.vendor_account_system_group_mismatch")

// authorizeResourceGroup loads the resource group and returns
// ErrResourceGroupNotFound unless the principal is system-scoped or a
// can_manage_resources owner/co-manager of one of the resource group's
// linked admin groups (resourceManageGroupIDs) -- the READ authorization
// choke-point for this task (Resource Groups Phase 1, spec 2026-08-11),
// mirroring authorizeServer (Phase B) and authorizeServiceRead (Phase C).
// Unlike a server/service, a resource group has NO owner-list fallback --
// admin-group linkage is the only path in, besides the system scope. A
// stranger and an unknown id get the IDENTICAL error (no existence leak).
func (s *Service) authorizeResourceGroup(ctx context.Context, principal auth.Token, id string) (routing.ResourceGroup, error) {
	rg, err := s.routes.ResourceGroupByID(ctx, id)
	if err != nil {
		return routing.ResourceGroup{}, ErrResourceGroupNotFound
	}
	if isSystem(principal) {
		return rg, nil
	}
	groupIDs, err := s.routes.ResourceGroupAdminGroups(ctx, id)
	if err != nil {
		return routing.ResourceGroup{}, err
	}
	if len(groupIDs) > 0 {
		manageGroups, err := s.resourceManageGroupIDs(ctx, principal)
		if err != nil {
			return routing.ResourceGroup{}, err
		}
		for _, gid := range groupIDs {
			if manageGroups[gid] {
				return rg, nil
			}
		}
	}
	return routing.ResourceGroup{}, ErrResourceGroupNotFound
}

// ResourceGroupDTO is the read/list shape of a resource group (Resource
// Groups Phase 1, spec 2026-08-11). AdminGroups and Servers are always
// non-nil ([] when unlinked/empty); SystemGroup is the zero value when
// ungrouped (SystemGroupID == "").
type ResourceGroupDTO struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	// SystemGroup is the containment root -- the system-tier group every
	// linked admin group must be a child of (zero value for an ungrouped
	// resource group). A best-effort lookup: a vanished group yields a zero
	// value rather than failing the whole DTO.
	SystemGroup GroupRefDTO `json:"system_group"`
	// AdminGroups is the resource group's linked admin-group set (id+name),
	// the authorization basis authorizeResourceGroup/ListResourceGroups
	// consume. A linked group that has since vanished is skipped
	// (best-effort name resolution), never failing the whole DTO.
	AdminGroups []GroupRefDTO `json:"admin_groups"`
	// Servers is the resource group's member AI-servers (id+name). A member
	// server that has since vanished is skipped (best-effort name
	// resolution), never failing the whole DTO.
	Servers []ResourceGroupServerRefDTO `json:"servers"`
	// VendorAccounts is the resource group's linked vendor accounts (id+name,
	// credential-free), always non-nil. Every stored link is listed, the one a
	// manager may not operate and one whose account is no longer shared included:
	// what a group offers is its manager's call, and a manager can drop any link
	// (SetResourceGroupVendorAccounts). An account that has since vanished is
	// skipped (best-effort name resolution), never failing the whole DTO. Empty
	// while the vendor-accounts module flag is off.
	VendorAccounts []ResourceGroupVendorAccountRefDTO `json:"vendor_accounts"`
}

// ResourceGroupVendorAccountRefDTO is a minimal, credential-free vendor-account
// reference used by ResourceGroupDTO.VendorAccounts and the manager-side candidates
// (id + display name only; never the owner, the level or any credential flag).
type ResourceGroupVendorAccountRefDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ResourceGroupServerRefDTO is a minimal, non-secret AI-server reference used
// by ResourceGroupDTO.Servers (id + display name only).
type ResourceGroupServerRefDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ListResourceGroups returns every resource group the principal may manage:
// system-scope sees ALL (unconditional bypass, s.routes.ResourceGroups);
// anyone else sees ResourceGroupsByAdminGroups(resourceManageGroupIDs), which
// is already deduped by resource-group id and stably ordered by id (see
// SQLiteStore/MemoryStore ResourceGroupsByAdminGroups) -- mirroring
// ListServers/ListServices' union pattern, minus the owner/delegate branch a
// resource group has none of.
func (s *Service) ListResourceGroups(ctx context.Context, principal auth.Token) ([]ResourceGroupDTO, error) {
	var groups []routing.ResourceGroup
	if isSystem(principal) {
		all, err := s.routes.ResourceGroups(ctx)
		if err != nil {
			return nil, err
		}
		groups = all
	} else {
		manageGroups, err := s.resourceManageGroupIDs(ctx, principal)
		if err != nil {
			return nil, err
		}
		groupIDs := make([]string, 0, len(manageGroups))
		for gid := range manageGroups {
			groupIDs = append(groupIDs, gid)
		}
		byGroup, err := s.routes.ResourceGroupsByAdminGroups(ctx, groupIDs)
		if err != nil {
			return nil, err
		}
		groups = byGroup
	}
	out := make([]ResourceGroupDTO, 0, len(groups))
	for _, rg := range groups {
		dto, err := s.resourceGroupDTO(ctx, rg)
		if err != nil {
			return nil, err
		}
		out = append(out, dto)
	}
	return out, nil
}

// resourceGroupDTO resolves rg's linked admin groups, containment-root
// system group, and member servers into a ResourceGroupDTO -- mirroring
// serverDTO/serviceDTO's own admin-group/system-group resolution exactly
// (id+name via s.groups.UserGroupByID, best-effort/skip-on-error, nil-safe on
// s.groups), plus a member-server resolution via s.routes.AIServerByID
// (best-effort/skip-on-error; s.routes is never nil) and the linked vendor
// accounts (resourceGroupVendorAccountRefs).
func (s *Service) resourceGroupDTO(ctx context.Context, rg routing.ResourceGroup) (ResourceGroupDTO, error) {
	groupIDs, err := s.routes.ResourceGroupAdminGroups(ctx, rg.ID)
	if err != nil {
		return ResourceGroupDTO{}, err
	}
	adminGroups := make([]GroupRefDTO, 0, len(groupIDs))
	for _, gid := range groupIDs {
		if s.groups == nil {
			break
		}
		g, err := s.groups.UserGroupByID(ctx, gid)
		if err != nil {
			// linked group vanished; skip rather than fail the whole DTO
			continue
		}
		adminGroups = append(adminGroups, GroupRefDTO{ID: g.ID, Name: g.Name})
	}
	systemGroup := GroupRefDTO{}
	if rg.SystemGroupID != "" && s.groups != nil {
		if g, err := s.groups.UserGroupByID(ctx, rg.SystemGroupID); err == nil {
			systemGroup = GroupRefDTO{ID: g.ID, Name: g.Name}
		}
	}
	servers, err := s.resourceGroupServerRefs(ctx, rg.ID)
	if err != nil {
		return ResourceGroupDTO{}, err
	}
	vendorAccounts, err := s.resourceGroupVendorAccountRefs(ctx, rg.ID)
	if err != nil {
		return ResourceGroupDTO{}, err
	}
	return ResourceGroupDTO{
		ID:             rg.ID,
		Name:           rg.Name,
		Status:         rg.Status,
		SystemGroup:    systemGroup,
		AdminGroups:    adminGroups,
		Servers:        servers,
		VendorAccounts: vendorAccounts,
	}, nil
}

// resourceGroupServerRefs resolves the member servers of rgID to id+name
// references, in link order (best-effort/skip-on-error: a member server that
// vanished is skipped rather than failing the whole DTO). Never nil.
func (s *Service) resourceGroupServerRefs(ctx context.Context, rgID string) ([]ResourceGroupServerRefDTO, error) {
	serverIDs, err := s.routes.ResourceGroupServers(ctx, rgID)
	if err != nil {
		return nil, err
	}
	servers := make([]ResourceGroupServerRefDTO, 0, len(serverIDs))
	for _, sid := range serverIDs {
		srv, err := s.routes.AIServerByID(ctx, sid)
		if err != nil {
			// member server vanished; skip rather than fail the whole DTO
			continue
		}
		servers = append(servers, ResourceGroupServerRefDTO{ID: srv.ID, Name: srv.Name})
	}
	return servers, nil
}

// resourceGroupVendorAccountRefs resolves the vendor accounts linked into rgID to
// id+name references, in link order. An account that vanished since the link is
// skipped (the FK cascade normally removes the link with it) rather than failing the
// whole DTO, like a vanished member server. Never nil.
//
// While the vendor-accounts module flag is off (its default) the answer is the empty
// list and the store is not read: the whole area is switched off then (every
// vendor-account method is a 409), so a group does not advertise accounts of it, and a
// deployment that never enables the module pays no extra query per group.
func (s *Service) resourceGroupVendorAccountRefs(ctx context.Context, rgID string) ([]ResourceGroupVendorAccountRefDTO, error) {
	if !s.VendorAccountsEnabled(ctx) {
		return []ResourceGroupVendorAccountRefDTO{}, nil
	}
	ids, err := s.routes.ResourceGroupVendorAccounts(ctx, rgID)
	if err != nil {
		return nil, err
	}
	out := make([]ResourceGroupVendorAccountRefDTO, 0, len(ids))
	for _, aid := range ids {
		acc, err := s.routes.VendorAccountByID(ctx, aid)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, ResourceGroupVendorAccountRefDTO{ID: acc.ID, Name: acc.Name})
	}
	return out, nil
}

// GetResourceGroup is the *Read* object-gate over a single resource group
// (mirrors GetServer/GetService).
func (s *Service) GetResourceGroup(ctx context.Context, principal auth.Token, id string) (ResourceGroupDTO, error) {
	rg, err := s.authorizeResourceGroup(ctx, principal, id)
	if err != nil {
		return ResourceGroupDTO{}, err
	}
	return s.resourceGroupDTO(ctx, rg)
}

// CreateResourceGroupRequest is CreateResourceGroup's body. Status ""
// defaults to active (normalizeResourceGroupStatus). AdminGroupIDs is
// mandatory for EVERY caller, including system-scope -- see
// validateResourceGroupAdminGroupIDs.
type CreateResourceGroupRequest struct {
	Name   string `json:"name"`
	Status string `json:"status,omitempty"`
	// AdminGroupIDs is the set of ADMIN-tier groups the new resource group is
	// linked to (resource_group_admin_groups) -- mandatory for EVERY caller,
	// including system-scope (mirrors CreateServerRequest.AdminGroupIDs /
	// CreateServiceRequest.AdminGroupIDs). Every chosen group must share one
	// parent (system-tier) group, which becomes the resource group's
	// SystemGroupID containment root; see validateResourceGroupAdminGroupIDs.
	AdminGroupIDs []string `json:"admin_group_ids"`
	// SystemGroupID is an optional system-admin convenience cross-check: when
	// set (system-scope only), every chosen AdminGroupIDs entry's parent must
	// equal it, or the create is rejected as a parent mismatch.
	SystemGroupID string `json:"system_group_id"`
}

// UpdateResourceGroupRequest is UpdateResourceGroup's body -- pointer-based
// partial PATCH, mirroring UpdateServerRequest/UpdateServiceRequest: nil =
// keep the current value. Admin-group linkage is edited separately, via
// SetResourceGroupAdminGroups.
type UpdateResourceGroupRequest struct {
	Name   *string `json:"name,omitempty"`
	Status *string `json:"status,omitempty"`
}

// normalizeResourceGroupStatus mirrors normalizeServiceStatus: only the
// two-value active/disabled vocabulary is accepted (a resource group has no
// "maintenance" state).
func normalizeResourceGroupStatus(raw string) (string, error) {
	status := strings.TrimSpace(raw)
	if status == "" {
		return routing.ServerStatusActive, nil
	}
	switch status {
	case routing.ServerStatusActive, routing.ServerStatusDisabled:
		return status, nil
	default:
		return "", ErrResourceGroupValidation
	}
}

// validateResourceGroupAdminGroupIDs validates the admin-group set a resource
// group is (or is being re-)linked into -- shared by CreateResourceGroup and
// SetResourceGroupAdminGroups (mirrors validateServiceAdminGroupIDs/
// validateAdminGroupIDs exactly, keyed on "resource group" instead of
// "service"/"server"). rawIDs is trimmed + deduped first; an empty result is
// ErrResourceGroupAdminGroupRequired (every resource group needs >=1 admin
// group, regardless of the caller's scope). Each remaining id must resolve to
// an EXISTING ADMIN-tier group (else ErrResourceGroupAdminGroupInvalid); for
// a non-system principal, each must ALSO be one they may manage resource
// groups through (resourceManageGroupIDs -- a system-scope principal skips
// this check and may link into ANY admin-tier group). Every chosen group
// must share exactly ONE ParentGroupID -- the resource group's containment
// root -- or the call is rejected as ErrResourceGroupAdminGroupParentMismatch;
// when the caller is system-scope and supplied a non-empty systemGroupHint (a
// convenience cross-check, CreateResourceGroupRequest.SystemGroupID), that
// resolved root must equal it too. Returns the deduped ids (order preserved)
// and the resolved systemGroupID.
func (s *Service) validateResourceGroupAdminGroupIDs(ctx context.Context, principal auth.Token, rawIDs []string, systemGroupHint string) ([]string, string, error) {
	return s.validateAdminGroupScope(ctx, principal, rawIDs, systemGroupHint, s.resourceManageGroupIDs, adminGroupSentinels{
		Required:       ErrResourceGroupAdminGroupRequired,
		Invalid:        ErrResourceGroupAdminGroupInvalid,
		ParentMismatch: ErrResourceGroupAdminGroupParentMismatch,
	})
}

// CreateResourceGroup creates a new resource group. Authorization (mirrors
// CreateServer's/CreateService's Phase B/C rewrite): allowed for a
// system-scope principal OR one who may manage resource groups through at
// least one admin group (resourceManageGroupIDs); a principal with neither
// gets ErrResourceGroupForbidden. Every create -- regardless of scope --
// must additionally link the resource group to >=1 existing admin-tier group
// (req.AdminGroupIDs, validated by validateResourceGroupAdminGroupIDs); a
// rejection there happens BEFORE the resource-group row is created, so a
// rejected create never leaves an orphan.
func (s *Service) CreateResourceGroup(ctx context.Context, principal auth.Token, req CreateResourceGroupRequest) (ResourceGroupDTO, error) {
	if !isSystem(principal) {
		manageGroups, err := s.resourceManageGroupIDs(ctx, principal)
		if err != nil {
			return ResourceGroupDTO{}, err
		}
		if len(manageGroups) == 0 {
			return ResourceGroupDTO{}, ErrResourceGroupForbidden
		}
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return ResourceGroupDTO{}, ErrResourceGroupValidation
	}
	status, err := normalizeResourceGroupStatus(req.Status)
	if err != nil {
		return ResourceGroupDTO{}, err
	}
	// Admin-group linkage: validated LAST among the request-shape checks,
	// still strictly BEFORE the resource-group row is created -- see the
	// function doc (orphan-safe).
	adminGroupIDs, systemGroupID, err := s.validateResourceGroupAdminGroupIDs(ctx, principal, req.AdminGroupIDs, req.SystemGroupID)
	if err != nil {
		return ResourceGroupDTO{}, err
	}
	now := s.clock().UTC()
	rg := routing.ResourceGroup{
		ID:        "rgrp_" + compactRandomHex(16),
		Name:      name,
		Status:    status,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.routes.CreateResourceGroup(ctx, rg); err != nil {
		return ResourceGroupDTO{}, err
	}
	// Admin-group linkage persist: AFTER the resource-group row exists
	// (resource_group_admin_groups.resource_group_id is an FK), BEFORE
	// returning -- best-effort ordering mirrors CreateService/CreateServer
	// (validation-before-create makes this recoverable, not orphan-unsafe).
	if err := s.routes.UpdateResourceGroupSystemGroup(ctx, rg.ID, systemGroupID); err != nil {
		return ResourceGroupDTO{}, err
	}
	rg.SystemGroupID = systemGroupID
	for _, gid := range adminGroupIDs {
		if err := s.routes.SetResourceGroupAdminGroup(ctx, rg.ID, gid); err != nil {
			return ResourceGroupDTO{}, err
		}
	}
	return s.resourceGroupDTO(ctx, rg)
}

// UpdateResourceGroup updates name/status -- the *Settings*-equivalent
// object-gate (authorizeResourceGroup; a resource group has no separate
// Tokens/Settings split, unlike Service). Everything that can fail is
// validated BEFORE anything is persisted.
func (s *Service) UpdateResourceGroup(ctx context.Context, principal auth.Token, id string, req UpdateResourceGroupRequest) (ResourceGroupDTO, error) {
	rg, err := s.authorizeResourceGroup(ctx, principal, id)
	if err != nil {
		return ResourceGroupDTO{}, err
	}
	name := rg.Name
	if req.Name != nil {
		name = strings.TrimSpace(*req.Name)
		if name == "" {
			return ResourceGroupDTO{}, ErrResourceGroupValidation
		}
	}
	status := rg.Status
	if req.Status != nil {
		status, err = normalizeResourceGroupStatus(*req.Status)
		if err != nil {
			return ResourceGroupDTO{}, err
		}
	}
	rg.Name = name
	rg.Status = status
	rg.UpdatedAt = s.clock().UTC()
	if err := s.routes.UpdateResourceGroup(ctx, rg); err != nil {
		return ResourceGroupDTO{}, err
	}
	return s.resourceGroupDTO(ctx, rg)
}

// SetResourceGroupAdminGroups replaces a resource group's linked admin-group
// set -- the linkage editor's write path, mirrors SetServerAdminGroups/
// SetServiceAdminGroups exactly. authorizeResourceGroup gates FIRST
// (404-no-leak: only a current owner/can_manage_resources co-manager/system
// principal may see or edit the linkage at all), THEN the new set is
// validated by the SAME rules CreateResourceGroup uses
// (validateResourceGroupAdminGroupIDs: each id existing ADMIN-tier +, for a
// non-system caller, in resourceManageGroupIDs; every chosen group sharing
// one parent; >=1 required). The delta vs the resource group's CURRENT
// admin groups is applied (SetResourceGroupAdminGroup for additions,
// RemoveResourceGroupAdminGroup for removals).
//
// Containment root is IMMUTABLE once set (mirrors the server/service-side
// guard): for an ALREADY-grouped resource group (rg.SystemGroupID != "") the
// new set's derived common parent must equal the resource group's CURRENT
// root, or the call is rejected as ErrResourceGroupAdminGroupParentMismatch --
// checked EXPLICITLY below, independent of the caller's scope (NOT via
// validateResourceGroupAdminGroupIDs's systemGroupHint parameter, which only
// applies its cross-check under system-scope -- a plain admin who happens to
// own/co-manage admin groups in two different tenants would otherwise be
// able to swap a grouped resource group's linked groups for ones under a
// DIFFERENT system group and thereby relocate its containment root; that is
// exactly the scenario this guard closes, for EVERY principal, including
// system). UpdateResourceGroupSystemGroup therefore fires ONLY the very
// first time an UNGROUPED resource group (SystemGroupID=="") gets its first
// link -- once set, the guard above holds it fixed on every later call.
func (s *Service) SetResourceGroupAdminGroups(ctx context.Context, principal auth.Token, id string, groupIDs []string) (ResourceGroupDTO, error) {
	rg, err := s.authorizeResourceGroup(ctx, principal, id)
	if err != nil {
		return ResourceGroupDTO{}, err
	}
	ids, systemGroupID, err := s.validateResourceGroupAdminGroupIDs(ctx, principal, groupIDs, "")
	if err != nil {
		return ResourceGroupDTO{}, err
	}
	if rg.SystemGroupID != "" && systemGroupID != rg.SystemGroupID {
		return ResourceGroupDTO{}, ErrResourceGroupAdminGroupParentMismatch
	}
	current, err := s.routes.ResourceGroupAdminGroups(ctx, id)
	if err != nil {
		return ResourceGroupDTO{}, err
	}
	currentSet := make(map[string]bool, len(current))
	for _, gid := range current {
		currentSet[gid] = true
	}
	wantSet := make(map[string]bool, len(ids))
	for _, gid := range ids {
		wantSet[gid] = true
	}
	for _, gid := range ids {
		if !currentSet[gid] {
			if err := s.routes.SetResourceGroupAdminGroup(ctx, id, gid); err != nil {
				return ResourceGroupDTO{}, err
			}
		}
	}
	for _, gid := range current {
		if !wantSet[gid] {
			if err := s.routes.RemoveResourceGroupAdminGroup(ctx, id, gid); err != nil {
				return ResourceGroupDTO{}, err
			}
		}
	}
	// Reached ONLY on the first grouping of a previously-ungrouped resource
	// group (rg.SystemGroupID=="" here): the immutability guard above already
	// rejected any attempt to move an ALREADY-grouped resource group's root,
	// so this branch can no longer fire on a later call for the same group.
	if systemGroupID != rg.SystemGroupID {
		if err := s.routes.UpdateResourceGroupSystemGroup(ctx, id, systemGroupID); err != nil {
			return ResourceGroupDTO{}, err
		}
		rg.SystemGroupID = systemGroupID
	}
	return s.resourceGroupDTO(ctx, rg)
}

// ResourceGroupAdminGroupCandidates lists the admin-tier groups the caller
// may create/link a resource group into (drives the create-resource-group /
// linkage-editor picker's auto-select-one / mandatory-choose-many /
// no-groups-hint logic; mirrors ServerAdminGroupCandidates/
// ServiceAdminGroupCandidates exactly). A system-scope principal gets EVERY
// admin-tier group (may link into any of them, per
// validateResourceGroupAdminGroupIDs); anyone else gets exactly the groups
// resourceManageGroupIDs returns (owner or can_manage_resources co-manager).
func (s *Service) ResourceGroupAdminGroupCandidates(ctx context.Context, principal auth.Token) ([]AdminGroupCandidateDTO, error) {
	var groups []store.UserGroup
	if isSystem(principal) {
		all, err := s.groups.ListUserGroupsByTier(ctx, store.GroupTierAdmin)
		if err != nil {
			return nil, err
		}
		groups = all
	} else {
		manageGroups, err := s.resourceManageGroupIDs(ctx, principal)
		if err != nil {
			return nil, err
		}
		for gid := range manageGroups {
			g, err := s.groups.UserGroupByID(ctx, gid)
			if err != nil {
				// linked group vanished between the enumeration and this
				// lookup; skip rather than fail the whole candidate list.
				continue
			}
			groups = append(groups, g)
		}
	}
	out := make([]AdminGroupCandidateDTO, 0, len(groups))
	for _, g := range groups {
		parentName := ""
		if g.ParentGroupID != "" {
			if parent, err := s.groups.UserGroupByID(ctx, g.ParentGroupID); err == nil {
				parentName = parent.Name
			}
		}
		out = append(out, AdminGroupCandidateDTO{
			ID: g.ID, Name: g.Name, ParentGroupID: g.ParentGroupID, ParentGroupName: parentName,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// DeleteResourceGroup removes a resource group -- authorizeResourceGroup
// gates it (a stranger/unknown id gets ErrResourceGroupNotFound). The
// underlying store cascades resource_group_admin_groups/resource_group_servers
// (a DB FK on SQLite/Postgres; the memory driver mirrors the cascade
// in-process -- see MemoryStore.DeleteResourceGroup).
func (s *Service) DeleteResourceGroup(ctx context.Context, principal auth.Token, id string) error {
	if _, err := s.authorizeResourceGroup(ctx, principal, id); err != nil {
		return err
	}
	return s.routes.DeleteResourceGroup(ctx, id)
}

// SetResourceGroupServers replaces a resource group's member-server set
// (Resource Groups Phase 1, Task 5, spec 2026-08-11) -- the delta model
// mirrors SetResourceGroupAdminGroups/SetServiceAdminGroups exactly (add
// desired-not-current, remove current-not-desired), but ADDING a server is
// gated by the dual-manage rule that has no admin-group-linkage analogue:
//
//  1. authorizeResourceGroup(id) gates the WHOLE call first (404-no-leak on
//     the resource group itself -- a stranger and an unknown id are
//     indistinguishable).
//  2. For every id in the DESIRED set that is not already a current member,
//     authorizeServer(id) must ALSO pass -- a server the caller may not
//     otherwise manage is not addable via this route, full stop; that
//     failure aborts the ENTIRE call with ErrResourceGroupServerForbidden
//     (no partial application -- see the loop below, which validates every
//     new id BEFORE performing any store write).
//  3. The (now-authorized) server's SystemGroupID must equal rg.SystemGroupID
//     -- a mismatched OR ungrouped ("") server is ErrResourceGroupServerSystemGroupMismatch.
//
// REMOVAL needs none of the above: dropping a current member from the
// desired set only re-touches resource_group_servers, which is already
// gated by step 1 (authorizeResourceGroup) -- a server the caller can no
// longer authorizeServer-reach (ownership changed, group unlinked, ...)
// can still be dropped by whoever manages the resource group. A server may
// belong to MULTIPLE resource groups simultaneously; this method only ever
// touches id's own membership row set, never another resource group's.
func (s *Service) SetResourceGroupServers(ctx context.Context, principal auth.Token, id string, serverIDs []string) (ResourceGroupDTO, error) {
	rg, err := s.authorizeResourceGroup(ctx, principal, id)
	if err != nil {
		return ResourceGroupDTO{}, err
	}
	desired := make([]string, 0, len(serverIDs))
	seen := map[string]struct{}{}
	for _, raw := range serverIDs {
		sid := strings.TrimSpace(raw)
		if sid == "" {
			continue
		}
		if _, dup := seen[sid]; dup {
			continue
		}
		seen[sid] = struct{}{}
		desired = append(desired, sid)
	}
	current, err := s.routes.ResourceGroupServers(ctx, id)
	if err != nil {
		return ResourceGroupDTO{}, err
	}
	currentSet := make(map[string]bool, len(current))
	for _, sid := range current {
		currentSet[sid] = true
	}
	desiredSet := make(map[string]bool, len(desired))
	for _, sid := range desired {
		desiredSet[sid] = true
	}
	// Validate EVERY new addition BEFORE writing anything, so a rejected call
	// never partially applies (the same all-or-nothing shape as
	// SetResourceGroupAdminGroups' own pre-validated set).
	for _, sid := range desired {
		if currentSet[sid] {
			continue
		}
		srv, err := s.authorizeServer(ctx, principal, sid)
		if err != nil {
			return ResourceGroupDTO{}, ErrResourceGroupServerForbidden
		}
		if srv.SystemGroupID != rg.SystemGroupID {
			return ResourceGroupDTO{}, ErrResourceGroupServerSystemGroupMismatch
		}
	}
	for _, sid := range desired {
		if !currentSet[sid] {
			if err := s.routes.SetResourceGroupServer(ctx, id, sid); err != nil {
				return ResourceGroupDTO{}, err
			}
		}
	}
	for _, sid := range current {
		if !desiredSet[sid] {
			if err := s.routes.RemoveResourceGroupServer(ctx, id, sid); err != nil {
				return ResourceGroupDTO{}, err
			}
		}
	}
	return s.resourceGroupDTO(ctx, rg)
}

// ResourceGroupServerCandidates lists the AI-servers the caller may enter
// into resource group id (drives the membership-editor picker): the
// resource group's own visibility gate (authorizeResourceGroup) runs first
// (404-no-leak), then the caller's MANAGEABLE servers are enumerated --
// system-scope -> every server (s.routes.AIServers); anyone else -> the
// SAME owner-union-admin-group union ListServers uses (ServersByAdminGroups
// over serverManageGroupIDs, deduped against ServersByOwner) -- and finally
// FILTERED down to those whose SystemGroupID equals rg.SystemGroupID (the
// same containment rule SetResourceGroupServers enforces on add, surfaced
// here so the picker never offers a server the write path would reject).
func (s *Service) ResourceGroupServerCandidates(ctx context.Context, principal auth.Token, id string) ([]ResourceGroupServerRefDTO, error) {
	rg, err := s.authorizeResourceGroup(ctx, principal, id)
	if err != nil {
		return nil, err
	}
	var servers []routing.AIServer
	if isSystem(principal) {
		all, err := s.routes.AIServers(ctx)
		if err != nil {
			return nil, err
		}
		servers = all
	} else {
		manageGroups, err := s.serverManageGroupIDs(ctx, principal)
		if err != nil {
			return nil, err
		}
		groupIDs := make([]string, 0, len(manageGroups))
		for gid := range manageGroups {
			groupIDs = append(groupIDs, gid)
		}
		var byGroup []routing.AIServer
		if len(groupIDs) > 0 {
			byGroup, err = s.routes.ServersByAdminGroups(ctx, groupIDs)
			if err != nil {
				return nil, err
			}
		}
		byOwner, err := s.routes.ServersByOwner(ctx, principal.UserID)
		if err != nil {
			return nil, err
		}
		seen := make(map[string]bool, len(byGroup)+len(byOwner))
		servers = make([]routing.AIServer, 0, len(byGroup)+len(byOwner))
		for _, list := range [][]routing.AIServer{byGroup, byOwner} {
			for _, srv := range list {
				if seen[srv.ID] {
					continue
				}
				seen[srv.ID] = true
				servers = append(servers, srv)
			}
		}
	}
	out := make([]ResourceGroupServerRefDTO, 0, len(servers))
	for _, srv := range servers {
		if srv.SystemGroupID != rg.SystemGroupID {
			continue
		}
		out = append(out, ResourceGroupServerRefDTO{ID: srv.ID, Name: srv.Name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// SetResourceGroupVendorAccounts replaces a resource group's vendor-account set (#183,
// spec 2026-10-15 sections 6, 7 and 9) -- the vendor-account counterpart of
// SetResourceGroupServers, with the same delta model (add desired-not-current, remove
// current-not-desired) and the same all-or-nothing validation, but a stricter gate on
// ADDING an account, because a link here lets the group's members spend someone else's
// credential. In this order:
//
//  1. requireVendorAccountSharingEnabled (module flag, then the sharing master flag, both
//     a 409), before the group is loaded, so the answer is global and reveals nothing;
//  2. authorizeResourceGroup(id) gates the WHOLE call (ErrResourceGroupNotFound, the
//     404-no-leak on the group itself: a stranger and an unknown id are
//     indistinguishable), so a caller who does not manage the group learns nothing of
//     any account;
//  3. for every id in the DESIRED set that is not already linked, ALL of
//     authorizeResourceGroupVendorAccountAdd: the caller OPERATES the account
//     (authorizeVendorAccountOperate, so system scope gets no bypass) -- a stranger and
//     an unknown id are the same ErrVendorAccountNotFound -- the account is `full`
//     (ErrVendorAccountForbidden at `shared`, where only the owner's self-service links
//     it; ErrVendorAccountNotShared at `none`), and its root is the group's
//     (ErrResourceGroupVendorAccountSystemGroupMismatch). Any failure aborts the ENTIRE
//     call before a single store write.
//
// REMOVAL needs none of step 3: dropping a current link only re-touches
// resource_group_vendor_accounts, already gated by step 2, so whoever manages the group
// can drop any account out of it, one they cannot operate or one that is no longer shared
// included (what a group offers is its manager's call, like a member server). An account
// already linked is not re-validated when the caller sends it again. An account may be
// linked into MULTIPLE groups; this method only ever touches id's own link set.
//
// Returns the group, with its vendor accounts listed. This only links accounts: whether a
// member can use one is the routing grant's decision (live share level, per-account
// opt-in).
func (s *Service) SetResourceGroupVendorAccounts(ctx context.Context, principal auth.Token, id string, accountIDs []string) (ResourceGroupDTO, error) {
	if err := s.requireVendorAccountSharingEnabled(ctx); err != nil {
		return ResourceGroupDTO{}, err
	}
	rg, err := s.authorizeResourceGroup(ctx, principal, id)
	if err != nil {
		return ResourceGroupDTO{}, err
	}
	current, err := s.routes.ResourceGroupVendorAccounts(ctx, id)
	if err != nil {
		return ResourceGroupDTO{}, err
	}
	add, remove := idSetDelta(current, trimmedUniqueIDs(accountIDs))
	// Validate EVERY new addition BEFORE writing anything, so a rejected call never
	// partially applies (the same all-or-nothing shape as SetResourceGroupServers).
	for _, aid := range add {
		if err := s.authorizeResourceGroupVendorAccountAdd(ctx, principal, rg, aid); err != nil {
			return ResourceGroupDTO{}, err
		}
	}
	if err := s.writeResourceGroupVendorAccountLinks(ctx, id, add, remove); err != nil {
		return ResourceGroupDTO{}, err
	}
	return s.resourceGroupDTO(ctx, rg)
}

// authorizeResourceGroupVendorAccountAdd is the per-account gate of ADDING an account to
// rg from the resource-group manager side (see SetResourceGroupVendorAccounts, step 3).
// The order matters for what each answer reveals: an account the caller cannot operate is
// ErrVendorAccountNotFound whatever its level (so nothing about a stranger's account
// leaks), and only then is the level asked: `shared` is ErrVendorAccountForbidden (the
// caller can see and operate the account, so "not found" would be a lie; the owner links
// a `shared` account through their own self-service), anything below `full` is
// ErrVendorAccountNotShared, and last comes the root.
func (s *Service) authorizeResourceGroupVendorAccountAdd(ctx context.Context, principal auth.Token, rg routing.ResourceGroup, accountID string) error {
	acc, err := s.loadVendorAccount(ctx, accountID)
	if err != nil {
		return err
	}
	if err := s.authorizeVendorAccountOperate(ctx, principal, acc); err != nil {
		return err
	}
	level := s.effectiveVendorAccountShareLevel(ctx, acc)
	if level == routing.VendorShareLevelShared {
		return ErrVendorAccountForbidden
	}
	if level != routing.VendorShareLevelFull {
		return ErrVendorAccountNotShared
	}
	if !vendorAccountRootMatches(acc, rg) {
		return ErrResourceGroupVendorAccountSystemGroupMismatch
	}
	return nil
}

// writeResourceGroupVendorAccountLinks applies a validated delta to rgID: the missing
// links are written first, then the surplus ones removed. A store failure stops at the
// first error, leaving the earlier writes in place (each is a single idempotent row, so a
// retry converges). An account that vanished between validation and write is
// ErrVendorAccountNotFound.
func (s *Service) writeResourceGroupVendorAccountLinks(ctx context.Context, rgID string, add, remove []string) error {
	for _, aid := range add {
		if err := s.routes.SetResourceGroupVendorAccount(ctx, rgID, aid); err != nil {
			return notFoundAsVendorAccountNotFound(err)
		}
	}
	for _, aid := range remove {
		if err := s.routes.RemoveResourceGroupVendorAccount(ctx, rgID, aid); err != nil {
			return err
		}
	}
	return nil
}

// ResourceGroupVendorAccountCandidates lists the vendor accounts the caller may add to
// resource group id (drives the membership-editor picker; mirrors
// ResourceGroupServerCandidates): the group's own gate (authorizeResourceGroup) runs first
// (404-no-leak), then the accounts the caller OPERATES -- their own and the shared ones of
// a management group they are a CM of (operableVendorAccounts; system scope widens
// nothing, it never bypasses an account write) -- FILTERED down to the `full` ones whose
// root is the group's: exactly what SetResourceGroupVendorAccounts would accept, so the
// picker never offers an account the write path would refuse. Accounts already in the
// group are included (so the picker shows the selection). Sorted by name, then id; never
// nil. Like every method behind the link endpoints it is a 409 while sharing is off.
func (s *Service) ResourceGroupVendorAccountCandidates(ctx context.Context, principal auth.Token, id string) ([]ResourceGroupVendorAccountRefDTO, error) {
	if err := s.requireVendorAccountSharingEnabled(ctx); err != nil {
		return nil, err
	}
	rg, err := s.authorizeResourceGroup(ctx, principal, id)
	if err != nil {
		return nil, err
	}
	accounts, err := s.operableVendorAccounts(ctx, principal)
	if err != nil {
		return nil, err
	}
	out := make([]ResourceGroupVendorAccountRefDTO, 0, len(accounts))
	for _, acc := range accounts {
		if s.effectiveVendorAccountShareLevel(ctx, acc) == routing.VendorShareLevelFull && vendorAccountRootMatches(acc, rg) {
			out = append(out, ResourceGroupVendorAccountRefDTO{ID: acc.ID, Name: acc.Name})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// operableVendorAccounts returns the accounts principal passes
// authorizeVendorAccountOperate for: their own (every level) plus the accounts they
// manage as a CM (managedVendorAccounts: shared or full by the effective level, deduped
// against their own). A principal with no user identity operates nothing. The two arms
// are read the way the management listing reads them, so a candidate list built from them
// never offers an account the operate gate would refuse.
func (s *Service) operableVendorAccounts(ctx context.Context, principal auth.Token) ([]routing.VendorAccount, error) {
	if principal.UserID == "" {
		return nil, nil
	}
	own, err := s.routes.VendorAccountsByOwner(ctx, principal.UserID)
	if err != nil {
		return nil, err
	}
	managed, err := s.managedVendorAccounts(ctx, principal, own)
	if err != nil {
		return nil, err
	}
	return append(own, managed...), nil
}

// trimmedUniqueIDs trims each id and drops the blank ones and the repeats, keeping the
// first occurrence's order. Never nil.
func trimmedUniqueIDs(raw []string) []string {
	out := make([]string, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for _, r := range raw {
		id := strings.TrimSpace(r)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// idSetDelta returns what turns the current id set into the desired one: add = the
// desired ids not in current (desired order), remove = the current ids not in desired
// (current order).
func idSetDelta(current, desired []string) (add, remove []string) {
	currentSet := make(map[string]bool, len(current))
	for _, id := range current {
		currentSet[id] = true
	}
	desiredSet := make(map[string]bool, len(desired))
	for _, id := range desired {
		desiredSet[id] = true
		if !currentSet[id] {
			add = append(add, id)
		}
	}
	for _, id := range current {
		if !desiredSet[id] {
			remove = append(remove, id)
		}
	}
	return add, remove
}
