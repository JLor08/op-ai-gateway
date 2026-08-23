// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { type Fetcher, request } from './transport';
import type { UserRef } from './groups';
import type { PortalToken } from './tokens';

// Projects (spec: docs/superpowers/specs/2026-08-08-projects-design.md). A
// project is a user-owned attribution object for API-token usage/cost
// group-by. Field names mirror portal.ProjectDTO/-RefDTO/-MembersDTO/
// -GroupRefDTO exactly, field-for-field (internal/portal/service_projects.go).
export type ProjectRole = 'owner' | 'member' | 'none';

export type Project = {
  id: string;
  name: string;
  description: string;
  owner_user_id: string;
  my_role: ProjectRole;
  can_manage: boolean;
  member_count: number;
  group_count: number;
  // Coupled-projects (spec 2026-08-09): non-empty iff this project is coupled
  // to a user-tier group -- owner_user_id/my_role/can_manage are then the
  // DERIVED (group-owner) values, not the project's own row. Mirrors
  // portal.ProjectDTO.CoupledGroupID/-Name exactly.
  coupled_group_id?: string;
  coupled_group_name?: string;
  // total_tokens (2026-08-09): the project's TRUE all-time token usage total
  // (input+output+cached+cache-write across every usage_events row with this
  // project_id, regardless of the attributing token's current attachment) --
  // mirrors portal.ProjectDTO.TotalTokens. Optional since only ListProjects
  // populates it (other project responses, e.g. create/rename, read 0/undefined).
  total_tokens?: number;
};

// Slim {id,name} reference used by the token-assign picker
// (GET /api/portal/projects/mine) — mirrors portal.ProjectRefDTO.
export type ProjectRef = {
  id: string;
  name: string;
};

// A minimal, non-secret user-group reference used by ProjectMembersView/
// ProjectCandidates — mirrors portal.GroupRefDTO.
export type ProjectGroupRef = {
  id: string;
  name: string;
};

// A project's current roster (GET /api/portal/projects/{id}/members) —
// mirrors portal.ProjectMembersDTO. transfer_candidates is every EFFECTIVE
// member (direct users ∪ members of the assigned groups) — the exact set the
// ownership-transfer picker offers (the direct-only `users` list would miss a
// group-only member that TransferProject accepts).
export type ProjectMembers = {
  users: UserRef[];
  groups: ProjectGroupRef[];
  transfer_candidates: UserRef[];
};

// One API token currently attached to a project (owner/admin view via
// GET /api/portal/projects/{id}/tokens) — mirrors portal.ProjectTokenDTO
// field-for-field. Never carries a secret/hash, only the same non-secret
// secret_prefix the owner's own token list shows. The 4 usage fields are this
// CURRENTLY-ATTACHED token's own usage attributed to the project (all-time) --
// NOT the project total (see ProjectTokenUsageTotal below, which can exceed
// the sum of these rows: it also counts usage from tokens since detached).
export type ProjectToken = {
  id: string;
  name: string;
  secret_prefix: string;
  owner_user_id: string;
  owner_name: string;
  status: PortalToken['status'];
  created_at: string;
  last_used_at?: string;
  request_count: number;
  input_tokens: number;
  output_tokens: number;
  total_tokens: number;
};

// The project's TRUE all-time usage total (GET /api/portal/projects/{id}/tokens
// `total`) -- includes usage from tokens no longer attached to the project, so
// it may exceed the sum of the ProjectToken rows returned alongside it.
export type ProjectTokenUsageTotal = {
  request_count: number;
  input_tokens: number;
  output_tokens: number;
  total_tokens: number;
};

export function projectsApi(fetcher: Fetcher) {
  return {
    // Projects (spec: 2026-08-08-projects-design.md). All routes are
    // gateway:use and mirror gateway.handlePortalProject* (project_endpoints.go)
    // exactly — same paths, methods, and request/response shapes.
    projects: () =>
      request<{ data: Project[] }>(fetcher, '/api/portal/projects').then((r) => r.data),
    // Slim {id,name} option list for the token-assign picker (Task 8).
    myProjects: () =>
      request<{ data: ProjectRef[] }>(fetcher, '/api/portal/projects/mine').then((r) => r.data),
    projectCandidates: (id: string) =>
      request<{ users: UserRef[]; groups: ProjectGroupRef[] }>(
        fetcher,
        `/api/portal/projects/${encodeURIComponent(id)}/candidates`,
      ),
    projectMembers: (id: string) =>
      request<ProjectMembers>(fetcher, `/api/portal/projects/${encodeURIComponent(id)}/members`),
    // Coupling (spec 2026-08-09) is mutually exclusive: coupled_group_id couples
    // to an existing user-tier group the caller owns; create_coupled_group
    // creates one (optionally under a given admin-group parent) then couples.
    // Both omitted -> a normal project (unchanged).
    createProject: (body: {
      name: string;
      description: string;
      coupled_group_id?: string;
      create_coupled_group?: { name: string; parent_group_id?: string };
    }) => request<Project>(fetcher, '/api/portal/projects', { method: 'POST', body }),
    // PATCH {name,description} renames/redescribes; PATCH {owner_user_id}
    // transfers ownership (mutually exclusive on the backend — never send
    // both in one call).
    renameProject: (id: string, name: string, description: string) =>
      request<Project>(fetcher, `/api/portal/projects/${encodeURIComponent(id)}`, {
        method: 'PATCH',
        body: { name, description },
      }),
    transferProject: (id: string, ownerUserId: string) =>
      request<{ ok: boolean }>(fetcher, `/api/portal/projects/${encodeURIComponent(id)}`, {
        method: 'PATCH',
        body: { owner_user_id: ownerUserId },
      }),
    deleteProject: (id: string) =>
      request<{ ok: boolean }>(fetcher, `/api/portal/projects/${encodeURIComponent(id)}`, {
        method: 'DELETE',
      }),
    addProjectMembers: (id: string, userIds: string[]) =>
      request<{ ok: boolean }>(fetcher, `/api/portal/projects/${encodeURIComponent(id)}/members`, {
        method: 'POST',
        body: { user_ids: userIds },
      }),
    removeProjectMember: (id: string, userId: string) =>
      request<{ ok: boolean }>(
        fetcher,
        `/api/portal/projects/${encodeURIComponent(id)}/members/${encodeURIComponent(userId)}`,
        { method: 'DELETE' },
      ),
    addProjectGroups: (id: string, groupIds: string[]) =>
      request<{ ok: boolean }>(fetcher, `/api/portal/projects/${encodeURIComponent(id)}/groups`, {
        method: 'POST',
        body: { group_ids: groupIds },
      }),
    removeProjectGroup: (id: string, groupId: string) =>
      request<{ ok: boolean }>(
        fetcher,
        `/api/portal/projects/${encodeURIComponent(id)}/groups/${encodeURIComponent(groupId)}`,
        { method: 'DELETE' },
      ),
    // Project-assigned tokens (owner/admin only; the members sub-view is
    // already gated the same way, so no extra frontend gate needed). The
    // response carries both the per-token rows (currently-attached tokens
    // only) AND the project's true all-time total (which can include usage
    // from tokens since detached) -- see ProjectToken/ProjectTokenUsageTotal.
    projectTokens: (id: string) =>
      request<{ tokens: ProjectToken[]; total: ProjectTokenUsageTotal }>(
        fetcher,
        `/api/portal/projects/${encodeURIComponent(id)}/tokens`,
      ),
    detachProjectToken: (id: string, tokenId: string) =>
      request<{ ok: boolean }>(
        fetcher,
        `/api/portal/projects/${encodeURIComponent(id)}/tokens/${encodeURIComponent(tokenId)}`,
        { method: 'DELETE' },
      ),
  };
}
