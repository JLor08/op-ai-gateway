// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { type Fetcher, request } from './transport';

// User groups (spec: docs/superpowers/specs/2026-08-08-user-groups-design.md).
// Three tiers form a strict hierarchy: system (system_admin only) -> admin
// (owned by an admin/system_admin, member of a system group) -> user (owned by
// any user, member of an admin group). Field names mirror portal.UserGroupDTO /
// -GroupLandscapeDTO / -InvitationDTO / -UserRefDTO exactly, field-for-field
// (internal/portal/service_user_groups.go).
export type UserGroupTier = 'system' | 'admin' | 'user';

// The caller's role in a given group. "" is the backend's zero value when the
// caller has no role in the group at all (e.g. a system-tier group and the
// caller isn't a system_admin co-managing it, or a group the caller is
// otherwise not owner/manager/member of) — see portal.groupDTO's switch,
// which never sets a "none" string; the field is simply left unset.
export type GroupRole = 'owner' | 'manager' | 'member' | '';

export type UserGroup = {
  id: string;
  tier: UserGroupTier;
  name: string;
  parent_group_id: string;
  owner_user_id: string;
  // Owner display name, resolved best-effort by the backend (empty for a
  // system-tier / owner-less group or if the lookup failed).
  owner_name: string;
  my_role: GroupRole;
  can_manage: boolean;
  // can_manage_users (spec 2026-08-10, per-Admin-Group co-manager permissions):
  // may the caller manage this group's USERS (invite/add/remove members) --
  // owner | a co-manager whose stored CanManageUsers flag is set | a
  // system_admin. Independent of can_manage, which now means "may manage
  // the group STRUCTURE" (rename/promote/demote/transfer/delete) -- owner |
  // a co-manager whose stored CanManageGroup flag is set | a system_admin.
  // Mirrors portal.UserGroupDTO.CanManageUsers exactly.
  can_manage_users: boolean;
  // can_manage_servers (Phase B, spec 2026-08-10): may the caller manage
  // AI-servers linked to this admin-tier group -- owner | a co-manager whose
  // stored CanManageServers flag is set | a system_admin. Follows ONLY its
  // own stored flag, independent of can_manage/can_manage_users. Mirrors
  // portal.UserGroupDTO.CanManageServers exactly.
  can_manage_servers: boolean;
  // can_manage_services (Phase C, spec 2026-08-10): may the caller manage
  // Services (service accounts) linked to this admin-tier group -- owner |
  // a co-manager whose stored CanManageServices flag is set | a
  // system_admin. Follows ONLY its own stored flag, independent of
  // can_manage/can_manage_users/can_manage_servers. Mirrors
  // portal.UserGroupDTO.CanManageServices exactly.
  can_manage_services: boolean;
  // can_manage_resources (Resource Groups Phase 1, spec 2026-08-11): may the
  // caller manage Resource Groups linked to this admin-tier group -- owner |
  // a co-manager whose stored CanManageResources flag is set | a
  // system_admin. Follows ONLY its own stored flag, independent of
  // can_manage/can_manage_users/can_manage_servers/can_manage_services.
  // Mirrors portal.UserGroupDTO.CanManageResources exactly.
  can_manage_resources: boolean;
  member_count: number;
  manager_count: number;
  // Projects coupled to this group (spec 2026-08-09, coupled projects) — only
  // ever non-empty on a user-tier group; always [] rather than omitted/null
  // (mirrors portal.UserGroupDTO.CoupledProjects, initialized non-nil).
  coupled_projects?: { id: string; name: string }[];
};

// The full per-principal group tree returned by GET /api/portal/groups
// (Service.ListGroups): system groups (system_admin only), admin groups
// (owned/managed/member-of), user groups (owned/managed/member-of).
export type GroupLandscape = {
  system: UserGroup[];
  admin: UserGroup[];
  user: UserGroup[];
};

// The caller's own pending user-tier group invitations
// (GET /api/portal/groups/invitations).
export type GroupInvitation = {
  group_id: string;
  group_name: string;
  invited_by: string;
  parent_group_id: string;
};

// A minimal, non-secret user reference used by member/candidate lists (never
// exposes role/status/tokens) — mirrors portal.UserRefDTO.
export type UserRef = {
  id: string;
  email: string;
  display_name: string;
};

// A single row in a group's CURRENT roster (GET .../members) — unlike
// UserRef/groupCandidates (ADD-able users only, i.e. NOT current members),
// this also carries the row's state and role flags so the manage UI can
// render a real member/manager/owner picker instead of a raw user-id field.
// Mirrors portal.UserGroupMemberDTO exactly.
export type GroupMember = {
  user_id: string;
  email: string;
  display_name: string;
  // "member" | "invited" (store.GroupStateMember/Invited).
  state: 'member' | 'invited';
  is_manager: boolean;
  is_owner: boolean;
  // can_manage_users/can_manage_group/can_manage_servers (spec 2026-08-10 +
  // Phase B): this row's stored co-manager per-permission flags -- all false
  // for a non-manager row (is_manager false). Mirrors
  // portal.UserGroupMemberDTO.CanManageUsers/CanManageGroup/CanManageServers
  // exactly.
  can_manage_users: boolean;
  can_manage_group: boolean;
  can_manage_servers: boolean;
  // can_manage_services (Phase C, spec 2026-08-10): this row's stored
  // co-manager Services-management flag -- false for a non-manager row
  // (is_manager false). Mirrors portal.UserGroupMemberDTO.CanManageServices
  // exactly.
  can_manage_services: boolean;
  // can_manage_resources (Resource Groups Phase 1, spec 2026-08-11): this
  // row's stored co-manager Resource-Groups-management flag -- false for a
  // non-manager row (is_manager false). Mirrors
  // portal.UserGroupMemberDTO.CanManageResources exactly.
  can_manage_resources: boolean;
};

// POST /api/portal/groups body.
export type CreateGroupRequest = {
  tier: UserGroupTier;
  name: string;
  parent_group_id?: string;
  owner_user_id?: string;
};

// A system-admin-only candidate for "create an admin group FOR another admin"
// (GET /api/portal/admin-owner-candidates) — mirrors portal.AdminOwnerCandidateDTO.
export type AdminOwnerCandidate = {
  user_id: string;
  display_name: string;
  email: string;
  system_groups: { id: string; name: string }[];
};

// A no-leak reference to an admin-tier group the caller may create/link a
// server into, plus its containment root's id/name (the server's future
// system_group_id) -- Phase B, spec 2026-08-10. Mirrors
// portal.AdminGroupCandidateDTO exactly (GET
// /api/portal/server-admin-group-candidates). Shared across the
// server/service/resource-group admin-group-linkage endpoints.
export type AdminGroupCandidate = {
  id: string;
  name: string;
  parent_group_id: string;
  parent_group_name: string;
};

type ManagerPerms = {
  canManageUsers: boolean;
  canManageGroup: boolean;
  canManageServers: boolean;
  canManageServices: boolean;
  canManageResources: boolean;
};

// Default for promoteManager's `perms` param -- see its comment below.
const DEFAULT_MANAGER_PERMS: ManagerPerms = {
  canManageUsers: true,
  canManageGroup: true,
  canManageServers: true,
  canManageServices: true,
  canManageResources: true,
};

export function groupsApi(fetcher: Fetcher) {
  return {
    // User groups (spec: 2026-08-08-user-groups-design.md). All routes are
    // gateway:use and mirror gateway.handlePortalGroups* (group_endpoints.go)
    // exactly — same paths, methods, and request/response shapes.
    groups: () => request<GroupLandscape>(fetcher, '/api/portal/groups'),
    groupInvitations: () =>
      request<{ data: GroupInvitation[] }>(fetcher, '/api/portal/groups/invitations').then(
        (r) => r.data,
      ),
    groupCandidates: (id: string) =>
      request<{ data: UserRef[] }>(
        fetcher,
        `/api/portal/groups/${encodeURIComponent(id)}/candidates`,
      ).then((r) => r.data),
    // The group's CURRENT roster (member + invited rows), for the manage
    // sub-view's real member/manager/owner pickers (as opposed to
    // groupCandidates, which lists only ADD-able users).
    groupMembers: (id: string) =>
      request<{ data: GroupMember[] }>(
        fetcher,
        `/api/portal/groups/${encodeURIComponent(id)}/members`,
      ).then((r) => r.data),
    createGroup: (body: CreateGroupRequest) =>
      request<UserGroup>(fetcher, '/api/portal/groups', { method: 'POST', body }),
    // System-admin-only: eligible admin owners for "create an admin group FOR
    // another admin" (spec: 2026-08-10-system-admin-create-admin-group-for-owner).
    adminOwnerCandidates: () =>
      request<{ data: AdminOwnerCandidate[] }>(fetcher, '/api/portal/admin-owner-candidates').then(
        (r) => r.data,
      ),
    // PATCH {name} renames; PATCH {owner_user_id} transfers ownership (mutually
    // exclusive on the backend — never send both in one call).
    renameGroup: (id: string, name: string) =>
      request<UserGroup>(fetcher, `/api/portal/groups/${encodeURIComponent(id)}`, {
        method: 'PATCH',
        body: { name },
      }),
    transferGroup: (id: string, ownerUserId: string) =>
      request<{ ok: boolean }>(fetcher, `/api/portal/groups/${encodeURIComponent(id)}`, {
        method: 'PATCH',
        body: { owner_user_id: ownerUserId },
      }),
    deleteGroup: (id: string) =>
      request<{ ok: boolean }>(fetcher, `/api/portal/groups/${encodeURIComponent(id)}`, {
        method: 'DELETE',
      }),
    addGroupMembers: (id: string, userIds: string[]) =>
      request<{ ok: boolean }>(fetcher, `/api/portal/groups/${encodeURIComponent(id)}/members`, {
        method: 'POST',
        body: { user_ids: userIds },
      }),
    removeGroupMember: (id: string, userId: string) =>
      request<{ ok: boolean }>(
        fetcher,
        `/api/portal/groups/${encodeURIComponent(id)}/members/${encodeURIComponent(userId)}`,
        { method: 'DELETE' },
      ),
    // Promotes userId to co-manager of the group, with the given initial
    // per-permission flags (spec 2026-08-10 + Phase B can_manage_servers +
    // Phase C can_manage_services + Resource Groups Phase 1
    // can_manage_resources). Defaulting ALL FIVE to true reproduces the
    // pre-feature "a co-manager can do everything" behavior byte-for-byte --
    // every pre-existing caller that doesn't pass a `perms` arg keeps
    // working unchanged.
    promoteManager: (id: string, userId: string, perms: ManagerPerms = DEFAULT_MANAGER_PERMS) =>
      request<{ ok: boolean }>(fetcher, `/api/portal/groups/${encodeURIComponent(id)}/managers`, {
        method: 'POST',
        body: {
          user_id: userId,
          can_manage_users: perms.canManageUsers,
          can_manage_group: perms.canManageGroup,
          can_manage_servers: perms.canManageServers,
          can_manage_services: perms.canManageServices,
          can_manage_resources: perms.canManageResources,
        },
      }),
    demoteManager: (id: string, userId: string) =>
      request<{ ok: boolean }>(
        fetcher,
        `/api/portal/groups/${encodeURIComponent(id)}/managers/${encodeURIComponent(userId)}`,
        { method: 'DELETE' },
      ),
    // Narrows/widens an EXISTING co-manager's per-permission flags (owner/
    // system_admin only, spec 2026-08-10 + Phase B can_manage_servers +
    // Phase C can_manage_services + Resource Groups Phase 1
    // can_manage_resources) -- never promotes; the userId must already hold
    // a co-manager row on the group, else the backend rejects with
    // group.candidate_invalid (400).
    setManagerPermissions: (
      id: string,
      userId: string,
      perms: {
        canManageUsers: boolean;
        canManageGroup: boolean;
        canManageServers: boolean;
        canManageServices: boolean;
        canManageResources: boolean;
      },
    ) =>
      request<{ ok: boolean }>(
        fetcher,
        `/api/portal/groups/${encodeURIComponent(id)}/managers/${encodeURIComponent(userId)}`,
        {
          method: 'PATCH',
          body: {
            can_manage_users: perms.canManageUsers,
            can_manage_group: perms.canManageGroup,
            can_manage_servers: perms.canManageServers,
            can_manage_services: perms.canManageServices,
            can_manage_resources: perms.canManageResources,
          },
        },
      ),
    // The invitee accepting/declining their OWN pending invitation.
    acceptInvitation: (id: string) =>
      request<{ ok: boolean }>(fetcher, `/api/portal/groups/${encodeURIComponent(id)}/accept`, {
        method: 'POST',
      }),
    declineInvitation: (id: string) =>
      request<{ ok: boolean }>(fetcher, `/api/portal/groups/${encodeURIComponent(id)}/decline`, {
        method: 'POST',
      }),
  };
}
