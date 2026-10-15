// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"sort"
)

// ServerResourceGroupDTO is the read shape of one resource group a server
// OWNER may self-service enter/leave their server into/from: id + name + whether
// the server is currently a member. No other server of the resource group is
// ever exposed here (server-owner self-service, spec
// 2026-08-11-resource-groups-server-owner-self-service).
type ServerResourceGroupDTO struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Member bool   `json:"member"`
}

// authorizeServerOwner is the STRICT server-owner gate for the self-service
// resource-group path: system-scope OR principal is in the server's ServerOwners.
// Unlike authorizeServer it does NOT grant a can_manage_servers admin-group
// co-manager (faithful to "Server-Eigentuemer"). A server the caller does not own
// yields ErrServerNotFound (404-no-leak).
func (s *Service) authorizeServerOwner(ctx context.Context, principal auth.Token, serverID string) (routing.AIServer, error) {
	server, err := s.routes.AIServerByID(ctx, serverID)
	if err != nil {
		return routing.AIServer{}, ErrServerNotFound
	}
	if isSystem(principal) {
		return server, nil
	}
	owners, err := s.routes.ServerOwners(ctx, serverID)
	if err != nil {
		return routing.AIServer{}, err
	}
	for _, ownerID := range owners {
		if ownerID == principal.UserID {
			return server, nil
		}
	}
	return routing.AIServer{}, ErrServerNotFound
}

// memberAdminGroupIDs returns the set of ADMIN-tier group ids the principal is a
// MEMBER of (member state; this is BROADER than resourceManageGroupIDs, which
// further filters to owner/can_manage_resources). Nil-safe on s.groups.
func (s *Service) memberAdminGroupIDs(ctx context.Context, principal auth.Token) (map[string]bool, error) {
	out := map[string]bool{}
	if s.groups == nil {
		return out, nil
	}
	groups, err := s.groups.UserGroupsForUser(ctx, principal.UserID, store.GroupTierAdmin, store.GroupStateMember)
	if err != nil {
		return nil, err
	}
	for _, g := range groups {
		out[g.ID] = true
	}
	return out, nil
}

// resourceGroupForOwner is the eligibility gate for the self-service path: the
// resource group exists AND (system-scope OR is linked to >=1 admin group the
// principal is a member of). A miss yields ErrResourceGroupNotFound (404-no-leak).
// memberGroups is precomputed by the caller (memberAdminGroupIDs).
func (s *Service) resourceGroupForOwner(ctx context.Context, principal auth.Token, rgID string, memberGroups map[string]bool) (routing.ResourceGroup, error) {
	rg, err := s.routes.ResourceGroupByID(ctx, rgID)
	if err != nil {
		return routing.ResourceGroup{}, ErrResourceGroupNotFound
	}
	if isSystem(principal) {
		return rg, nil
	}
	linked, err := s.routes.ResourceGroupAdminGroups(ctx, rgID)
	if err != nil {
		return routing.ResourceGroup{}, err
	}
	for _, gid := range linked {
		if memberGroups[gid] {
			return rg, nil
		}
	}
	return routing.ResourceGroup{}, ErrResourceGroupNotFound
}

// reachableResourceGroups lists the resource groups a self-service caller may
// enter/leave something into/from: system scope -> every resource group;
// anyone else -> the groups linked to >=1 admin group in memberGroups
// (precomputed by memberAdminGroupIDs). It is the list-side counterpart of
// resourceGroupForOwner (the same reach rule, applied to one id), shared by the
// server-owner and the service-manager self-service paths.
func (s *Service) reachableResourceGroups(ctx context.Context, principal auth.Token, memberGroups map[string]bool) ([]routing.ResourceGroup, error) {
	if isSystem(principal) {
		return s.routes.ResourceGroups(ctx)
	}
	ids := make([]string, 0, len(memberGroups))
	for gid := range memberGroups {
		ids = append(ids, gid)
	}
	return s.routes.ResourceGroupsByAdminGroups(ctx, ids)
}

// ServerOwnerResourceGroups lists the resource groups a server OWNER may enter
// their server into: strict-owner gate on serverID, then the resource groups
// linked to an admin group the owner is a MEMBER of AND sharing the server's
// system group, each with Member = the server is currently in it. Sorted by name.
func (s *Service) ServerOwnerResourceGroups(ctx context.Context, principal auth.Token, serverID string) ([]ServerResourceGroupDTO, error) {
	server, err := s.authorizeServerOwner(ctx, principal, serverID)
	if err != nil {
		return nil, err
	}
	memberGroups, err := s.memberAdminGroupIDs(ctx, principal)
	if err != nil {
		return nil, err
	}
	groups, err := s.reachableResourceGroups(ctx, principal, memberGroups)
	if err != nil {
		return nil, err
	}
	out := make([]ServerResourceGroupDTO, 0, len(groups))
	for _, rg := range groups {
		if rg.SystemGroupID != server.SystemGroupID {
			continue
		}
		members, err := s.routes.ResourceGroupServers(ctx, rg.ID)
		if err != nil {
			return nil, err
		}
		isMember := false
		for _, sid := range members {
			if sid == serverID {
				isMember = true
				break
			}
		}
		out = append(out, ServerResourceGroupDTO{ID: rg.ID, Name: rg.Name, Member: isMember})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// AddServerToResourceGroup enters a server the caller OWNS into a resource group
// linked to an admin group the caller is a MEMBER of, iff they share a system
// group. Idempotent. 404-no-leak on a non-owned server or an ineligible resource
// group; 400 on a system-group mismatch.
func (s *Service) AddServerToResourceGroup(ctx context.Context, principal auth.Token, serverID, rgID string) error {
	server, err := s.authorizeServerOwner(ctx, principal, serverID)
	if err != nil {
		return err
	}
	memberGroups, err := s.memberAdminGroupIDs(ctx, principal)
	if err != nil {
		return err
	}
	rg, err := s.resourceGroupForOwner(ctx, principal, rgID, memberGroups)
	if err != nil {
		return err
	}
	if server.SystemGroupID != rg.SystemGroupID {
		return ErrResourceGroupServerSystemGroupMismatch
	}
	return s.routes.SetResourceGroupServer(ctx, rgID, serverID)
}

// RemoveServerFromResourceGroup removes a server the caller OWNS from a resource
// group linked to an admin group the caller is a MEMBER of. NO same-system-group
// check (mirrors the manager removal, which validates containment only on adds).
// Idempotent. 404-no-leak on a non-owned server or an ineligible resource group.
func (s *Service) RemoveServerFromResourceGroup(ctx context.Context, principal auth.Token, serverID, rgID string) error {
	if _, err := s.authorizeServerOwner(ctx, principal, serverID); err != nil {
		return err
	}
	memberGroups, err := s.memberAdminGroupIDs(ctx, principal)
	if err != nil {
		return err
	}
	if _, err := s.resourceGroupForOwner(ctx, principal, rgID, memberGroups); err != nil {
		return err
	}
	return s.routes.RemoveResourceGroupServer(ctx, rgID, serverID)
}
