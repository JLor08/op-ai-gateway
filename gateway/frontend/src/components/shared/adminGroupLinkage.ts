// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { AdminGroupCandidate } from '../../api';

// Admin-group linkage (Phase B/C, spec 2026-08-10; Resource Groups Phase 1,
// spec 2026-08-11). Shared by ServerList.tsx (PortalServer), ServicesView.tsx
// (PortalService) and ResourceGroupsView.tsx (ResourceGroup) -- the three
// entity kinds a caller can create/link admin-tier groups into. Extracted
// from three byte-identical (modulo entity type) copies (FV-1); only
// `editAdminGroupOptions` needed a small accessor param, since ResourceGroup
// nests its containment root as `system_group.{id,name}` rather than the
// flat `system_group_id`/`system_group_name` fields PortalServer/
// PortalService use.

export type DistinctSystemGroup = { id: string; name: string };

// A caller's create/link candidates may span more than one system (parent)
// group -- e.g. a system_admin, who sees every admin-tier group
// account-wide. Dedups the candidates' (parent_group_id, parent_group_name)
// pairs so the create form can offer a "which system group?" step ONLY when
// that step is actually ambiguous (more than one distinct parent); the
// common case (every candidate shares one parent) collapses to zero extra
// steps.
export function distinctParentGroups(candidates: AdminGroupCandidate[]): DistinctSystemGroup[] {
  const seen = new Map<string, string>();
  for (const c of candidates) {
    if (c.parent_group_id && !seen.has(c.parent_group_id))
      seen.set(c.parent_group_id, c.parent_group_name);
  }
  return Array.from(seen.entries()).map(([id, name]) => ({ id, name }));
}

// Narrows `candidates` to the ones under the EFFECTIVE system group: when the
// candidates share a single parent (the common case), every one qualifies
// (`distinct.length<=1`); when they span several, only `systemGroupId`'s
// children do -- an unset systemGroupId (nothing chosen yet) narrows to none,
// so the multi-select simply doesn't render until the operator picks one.
export function candidatesUnderSystemGroup(
  candidates: AdminGroupCandidate[],
  distinct: DistinctSystemGroup[],
  systemGroupId: string,
): AdminGroupCandidate[] {
  if (distinct.length <= 1) return candidates;
  if (!systemGroupId) return [];
  return candidates.filter((c) => c.parent_group_id === systemGroupId);
}

// The edit-form admin-groups editor's option set: the caller's OWN
// candidates under the entity's containment root, UNION the entity's
// currently-linked groups (so a group the caller doesn't personally manage
// -- e.g. linked by a different co-manager, or visible only via system scope
// elsewhere -- still shows as a removable chip rather than silently
// vanishing). `systemGroup` extracts the entity's containment root --
// PortalServer/PortalService expose it as flat system_group_id/_name fields,
// ResourceGroup nests it as system_group.{id,name}; the accessor lets one
// generic helper cover both shapes instead of forking it.
export function editAdminGroupOptions<T extends { admin_groups: { id: string; name: string }[] }>(
  candidates: AdminGroupCandidate[],
  entity: T,
  systemGroup: (entity: T) => { id: string; name: string },
): AdminGroupCandidate[] {
  const root = systemGroup(entity);
  const byId = new Map<string, AdminGroupCandidate>();
  for (const c of candidates) {
    if (c.parent_group_id === root.id) byId.set(c.id, c);
  }
  for (const g of entity.admin_groups) {
    if (!byId.has(g.id)) {
      byId.set(g.id, {
        id: g.id,
        name: g.name,
        parent_group_id: root.id,
        parent_group_name: root.name,
      });
    }
  }
  return Array.from(byId.values());
}

// Display strings for AdminGroupPicker/AdminGroupsEditor, threaded through so
// each view keeps its own translation keys (t.serverAdminGroup*,
// t.serviceAdminGroup*, t.resourceGroupAdminGroup*) rather than the shared
// components reaching into `Translation` themselves.
export type AdminGroupLinkageLabels = {
  noCandidatesHint: string;
  systemGroupLabel: string;
  systemGroupAuto: (name: string) => string;
  adminGroupLabel: string;
  adminGroupAuto: (name: string) => string;
};
