// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { type Fetcher, request } from './transport';
import type { AdminGroupCandidate, UserRef } from './groups';

// Resource Groups (Phase 1, spec: docs/superpowers/specs/2026-08-11-resource-groups-phase-1-design.md).
// A resource group is an admin-linked containment object over a set of
// AI-servers -- managed like a server/service (admin-group linkage via the
// can_manage_resources co-manager flag; no owner/delegate model of its own).
// Field names mirror portal.ResourceGroupDTO exactly, field-for-field
// (internal/portal/service_resource_groups.go).
export type ResourceGroupStatus = 'active' | 'disabled';

// A minimal, non-secret AI-server reference used by ResourceGroup.servers +
// resourceGroupServerCandidates() -- mirrors portal.ResourceGroupServerRefDTO
// (id + display name only).
export type ResourceGroupServerRef = { id: string; name: string };

export type ResourceGroup = {
  id: string;
  name: string;
  status: ResourceGroupStatus;
  // system_group is the containment root -- the system-tier group every
  // linked admin group must be a child of (zero value {id:"",name:""} for an
  // ungrouped resource group -- unreachable in practice since CreateResourceGroup
  // always requires >=1 admin group, which always derives a non-empty root).
  system_group: { id: string; name: string };
  // admin_groups is the resource group's linked admin-group set (id+name),
  // the authorization basis for every management action. Always a non-nil
  // slice ([] never happens in practice -- see system_group).
  admin_groups: { id: string; name: string }[];
  // servers is the resource group's member AI-servers (id+name). Always a
  // non-nil slice ([] = no members yet).
  servers: ResourceGroupServerRef[];
};

// POST /api/portal/resource-groups body. AdminGroupIDs is mandatory for
// EVERY caller, including system scope -- the backend rejects an empty set
// with resource_group.admin_group_required. Every chosen group must share
// one parent (system-tier) group, which becomes the resource group's
// system_group -- mirrors CreateServerRequest.admin_group_ids/
// CreateServiceRequest.admin_group_ids.
export type CreateResourceGroupRequest = {
  name: string;
  status?: string;
  admin_group_ids: string[];
  // SystemGroupID: an optional system-admin convenience cross-check -- when
  // set, every chosen admin_group_ids entry's parent must equal it, or the
  // create is rejected (resource_group.admin_group_parent_mismatch).
  system_group_id?: string;
};

// PATCH /api/portal/resource-groups/{id} body -- pointer-semantics on the
// backend (nil/omitted = keep the current value). Admin-group linkage is
// edited separately, via setResourceGroupAdminGroups; server membership via
// setResourceGroupServers.
export type UpdateResourceGroupRequest = {
  name?: string;
  status?: string;
};

// Resource Groups Phase 2 -- provisioning (spec:
// docs/superpowers/specs/2026-08-12-resource-groups-phase-2-provisioning). A
// resource group's "provisioned for" set: which principals (users/user
// groups/admin groups/services) may use its member servers. Mirrors
// portal.ResourceGroupProvisionDTO exactly (kind + target id/name).
export type ResourceGroupProvisionKind = 'user' | 'user_group' | 'admin_group' | 'service';

export type ResourceGroupProvision = {
  kind: ResourceGroupProvisionKind;
  target_id: string;
  target_name: string;
};

// PUT /api/portal/resource-groups/{id}/provisions body entry -- no
// target_name (the server resolves display names on read).
export type ResourceGroupProvisionInput = {
  kind: ResourceGroupProvisionKind;
  target_id: string;
};

// GET /api/portal/resource-groups/{id}/provision-candidates -- every target
// the caller may provision for THIS resource group, already filtered to the
// caller's own visible landscape (so the picker never offers a target the
// write path would reject). Mirrors portal.ResourceGroupProvisionCandidatesDTO;
// Services is typed []GroupRefDTO server-side but rides the same {id,name}
// wire shape as user_groups/admin_groups.
export type ResourceGroupProvisionCandidates = {
  users: UserRef[];
  user_groups: { id: string; name: string }[];
  admin_groups: { id: string; name: string }[];
  services: { id: string; name: string }[];
};

export function resourceGroupsApi(fetcher: Fetcher) {
  return {
    // Resource Groups (Phase 1, spec: 2026-08-11-resource-groups-phase-1-design.md).
    // A resource group is managed like a server/service (admin-group linkage
    // via the can_manage_resources co-manager flag; no owner/delegate model
    // of its own). Mirrors the server/service admin-group-linkage endpoints'
    // shape exactly (gateway.handlePortalResourceGroups*, resource_group_endpoints.go).
    resourceGroups: () =>
      request<{ data: ResourceGroup[] }>(fetcher, '/api/portal/resource-groups'),
    createResourceGroup: (body: CreateResourceGroupRequest) =>
      request<ResourceGroup>(fetcher, '/api/portal/resource-groups', { method: 'POST', body }),
    updateResourceGroup: (id: string, body: UpdateResourceGroupRequest) =>
      request<ResourceGroup>(fetcher, `/api/portal/resource-groups/${encodeURIComponent(id)}`, {
        method: 'PATCH',
        body,
      }),
    deleteResourceGroup: (id: string) =>
      request<{ ok: boolean }>(fetcher, `/api/portal/resource-groups/${encodeURIComponent(id)}`, {
        method: 'DELETE',
      }),
    // The admin-tier groups the caller may create/link a resource group
    // into: system scope -> every admin-tier group; anyone else -> the
    // groups they own or co-manage with can_manage_resources. Drives the
    // create-resource-group / linkage-editor picker. Mirrors
    // serverAdminGroupCandidates/serviceAdminGroupCandidates exactly.
    resourceGroupAdminGroupCandidates: () =>
      request<{ data: AdminGroupCandidate[] }>(
        fetcher,
        '/api/portal/resource-group-admin-group-candidates',
      ).then((r) => r.data),
    // Replaces a resource group's linked admin-group set. >=1 group
    // required; every chosen group must share one parent (system-tier)
    // group; the containment root is immutable once set.
    setResourceGroupAdminGroups: (id: string, groupIds: string[]) =>
      request<ResourceGroup>(
        fetcher,
        `/api/portal/resource-groups/${encodeURIComponent(id)}/admin-groups`,
        {
          method: 'PUT',
          body: { admin_group_ids: groupIds },
        },
      ),
    // The AI-servers the caller may enter into resource group id -- filtered
    // to the resource group's own system group (drives the membership-editor
    // picker, never offers a server the write path would reject).
    resourceGroupServerCandidates: (id: string) =>
      request<{ data: ResourceGroupServerRef[] }>(
        fetcher,
        `/api/portal/resource-groups/${encodeURIComponent(id)}/server-candidates`,
      ).then((r) => r.data),
    // Replaces a resource group's member-server set (add desired-not-current,
    // remove current-not-desired). Adding a server requires the caller to
    // ALSO manage that server (authorizeServer) AND for it to sit under the
    // resource group's own system group; removal needs only
    // authorizeResourceGroup.
    setResourceGroupServers: (id: string, serverIds: string[]) =>
      request<ResourceGroup>(
        fetcher,
        `/api/portal/resource-groups/${encodeURIComponent(id)}/servers`,
        {
          method: 'PUT',
          body: { server_ids: serverIds },
        },
      ),
    // Provisioning (Phase 2, spec 2026-08-12-resource-groups-phase-2-provisioning):
    // which users/user-groups/admin-groups/services are granted use of the
    // resource group's member servers. Mirrors the admin-groups/servers
    // linkage endpoints' shape -- GET -> {data:[...]}, PUT -> a full-replace
    // body, both authorizeResourceGroup-gated.
    resourceGroupProvisions: (id: string) =>
      request<{ data: ResourceGroupProvision[] }>(
        fetcher,
        `/api/portal/resource-groups/${encodeURIComponent(id)}/provisions`,
      ).then((r) => r.data),
    setResourceGroupProvisions: (id: string, provisions: ResourceGroupProvisionInput[]) =>
      request<{ ok: boolean }>(
        fetcher,
        `/api/portal/resource-groups/${encodeURIComponent(id)}/provisions`,
        {
          method: 'PUT',
          body: { provisions },
        },
      ),
    // The caller's own visible landscape for THIS resource group, split by
    // target kind -- feeds the provisioning editor's per-kind picker; never
    // offers a target setResourceGroupProvisions would reject.
    resourceGroupProvisionCandidates: (id: string) =>
      request<ResourceGroupProvisionCandidates>(
        fetcher,
        `/api/portal/resource-groups/${encodeURIComponent(id)}/provision-candidates`,
      ),
  };
}
