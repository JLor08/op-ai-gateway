// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"sort"
)

// ServiceResourceGroupDTO is the read shape of one resource group a service
// MANAGER may self-service enter/leave their service into/from: id + name +
// whether the service is currently provisioned into it. Nothing else about the
// resource group (its servers, its other provisioned targets) is exposed here
// (service-side resource-group self-service, #183 Phase 0, the analogue of
// ServerResourceGroupDTO).
type ServiceResourceGroupDTO struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Member bool   `json:"member"`
}

// authorizeServiceManager is the STRICT service-manager gate for the
// resource-group self-service path: system scope OR the principal is a
// can_manage_services owner/co-manager (serviceManageGroupIDs) of >=1 admin
// group the service is linked to. Unlike authorizeServiceRead/Settings it does
// NOT admit a service delegate (Token- or Full-): a delegate manages the
// service's tokens/settings, not which resource groups -- and so, later, which
// shared resources -- its tokens may reach. A service the caller does not
// manage yields ErrServiceNotFound (404-no-leak): the same error as an unknown
// id, whether the caller is a stranger, a plain member of the linked group, or
// a co-manager without can_manage_services.
func (s *Service) authorizeServiceManager(ctx context.Context, principal auth.Token, serviceID string) (routing.Service, error) {
	svc, err := s.routes.ServiceByID(ctx, serviceID)
	if err != nil {
		return routing.Service{}, ErrServiceNotFound
	}
	if isSystem(principal) {
		return svc, nil
	}
	linked, err := s.routes.ServiceAdminGroups(ctx, serviceID)
	if err != nil {
		return routing.Service{}, err
	}
	if len(linked) == 0 {
		return routing.Service{}, ErrServiceNotFound
	}
	manageGroups, err := s.serviceManageGroupIDs(ctx, principal)
	if err != nil {
		return routing.Service{}, err
	}
	for _, gid := range linked {
		if manageGroups[gid] {
			return svc, nil
		}
	}
	return routing.Service{}, ErrServiceNotFound
}

// authorizeServiceRGSelfService is the combined gate for ADDING/REMOVING a
// service to/from a resource group through the self-service path: the caller
// must manage the service (authorizeServiceManager, ErrServiceNotFound) AND be
// able to reach the resource group, i.e. be a MEMBER of an admin group it is
// linked to (resourceGroupForOwner, ErrResourceGroupNotFound). The service gate
// runs first, so a caller who cannot manage the service learns nothing about
// the resource group; a miss on the resource group is indistinguishable from a
// non-existent id.
func (s *Service) authorizeServiceRGSelfService(ctx context.Context, principal auth.Token, serviceID, rgID string) error {
	if _, err := s.authorizeServiceManager(ctx, principal, serviceID); err != nil {
		return err
	}
	memberGroups, err := s.memberAdminGroupIDs(ctx, principal)
	if err != nil {
		return err
	}
	_, err = s.resourceGroupForOwner(ctx, principal, rgID, memberGroups)
	return err
}

// ServiceResourceGroups lists the resource groups a service MANAGER may enter
// their service into: the service-manager gate on serviceID, then every
// resource group linked to an admin group the caller is a MEMBER of (system
// scope: every group), each with Member = the service is currently provisioned
// into it (a resource_group_provisions row of kind service). A group the
// service is provisioned into but the caller cannot reach is NOT listed.
// Sorted by name.
func (s *Service) ServiceResourceGroups(ctx context.Context, principal auth.Token, serviceID string) ([]ServiceResourceGroupDTO, error) {
	if _, err := s.authorizeServiceManager(ctx, principal, serviceID); err != nil {
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
	provisioned, err := s.routes.ResourceGroupIDsByProvisionTargets(ctx, routing.ProvisionKindService, []string{serviceID})
	if err != nil {
		return nil, err
	}
	isMember := make(map[string]bool, len(provisioned))
	for _, rgID := range provisioned {
		isMember[rgID] = true
	}
	out := make([]ServiceResourceGroupDTO, 0, len(groups))
	for _, rg := range groups {
		out = append(out, ServiceResourceGroupDTO{ID: rg.ID, Name: rg.Name, Member: isMember[rg.ID]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// AddServiceToResourceGroup provisions a service the caller MANAGES into a
// resource group linked to an admin group the caller is a MEMBER of, so the
// service's tokens may use what the group offers. It writes ONE
// resource_group_provisions row (kind service) -- the same row the RG-side
// provisions editor writes, but never that editor's whole-set replace, so the
// group's other provisioned targets are untouched. Idempotent. 404-no-leak on a
// service the caller does not manage (ErrServiceNotFound) or an unreachable
// resource group (ErrResourceGroupNotFound).
func (s *Service) AddServiceToResourceGroup(ctx context.Context, principal auth.Token, serviceID, rgID string) error {
	if err := s.authorizeServiceRGSelfService(ctx, principal, serviceID, rgID); err != nil {
		return err
	}
	return s.routes.SetResourceGroupProvision(ctx, rgID, routing.ProvisionKindService, serviceID)
}

// RemoveServiceFromResourceGroup removes a service the caller MANAGES from a
// resource group linked to an admin group the caller is a MEMBER of: the
// single-row inverse of AddServiceToResourceGroup (same gates, same 404-no-leak
// errors). Idempotent (a service that is not provisioned there is a no-op).
func (s *Service) RemoveServiceFromResourceGroup(ctx context.Context, principal auth.Token, serviceID, rgID string) error {
	if err := s.authorizeServiceRGSelfService(ctx, principal, serviceID, rgID); err != nil {
		return err
	}
	return s.routes.RemoveResourceGroupProvision(ctx, rgID, routing.ProvisionKindService, serviceID)
}
