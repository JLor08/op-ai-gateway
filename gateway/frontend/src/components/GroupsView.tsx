// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useEffect, useMemo, useRef, useState, type SubmitEvent, type ReactNode } from 'react';
import { Alert, Box, Button, Checkbox, Stack, Tooltip, Typography } from '@mui/material';
import AddIcon from '@mui/icons-material/Add';
import EditIcon from '@mui/icons-material/Edit';
import DeleteIcon from '@mui/icons-material/Delete';
import GroupIcon from '@mui/icons-material/Group';
import LogoutIcon from '@mui/icons-material/Logout';
import ArrowUpwardIcon from '@mui/icons-material/ArrowUpward';
import ArrowDownwardIcon from '@mui/icons-material/ArrowDownward';
import SwapHorizIcon from '@mui/icons-material/SwapHoriz';
import PersonRemoveIcon from '@mui/icons-material/PersonRemove';
import type {
  AdminOwnerCandidate,
  CreateGroupRequest,
  GroupInvitation,
  GroupLandscape,
  GroupMember,
  UserGroup,
  UserGroupTier,
  UserRef,
} from '../api';
import type { Translation, PortalApi } from './shared/types';
import { formatPortalError } from './shared/format';
import { useResource } from './shared/useResource';
import { PageTitle } from './shared/PageTitle';
import { Panel } from './shared/Panel';
import { Field } from './shared/Field';
import { MultiSelectField } from './shared/MultiSelectField';
import { SearchableSelect } from './shared/SearchableSelect';
import { Breadcrumbs } from './shared/Breadcrumbs';
import { ListTable, listTableLabels, type ListColumn } from './shared/ListTable';
import type { RowAction } from './shared/RowActionsMenu';
import { useToast } from './shared/ToastProvider';
import { ConfirmDialog } from './shared/ConfirmDialog';

const EMPTY_LANDSCAPE: GroupLandscape = { system: [], admin: [], user: [] };

// Mode is a discriminated union (mirrors ServicesView's `{ kind: "detail"; ... }`
// style) rather than UsersView's simpler 2-variant `{ edit }` shape, since this
// view has three distinct object sub-views.
type Mode =
  | 'list'
  | { kind: 'create'; tier: UserGroupTier }
  | { kind: 'edit'; group: UserGroup }
  | { kind: 'members'; group: UserGroup };

/** Truncates a raw id for display, keeping the full value in a `title` tooltip. */
function ShortId({ id }: Readonly<{ id: string }>) {
  if (!id) return <>{'–'}</>;
  return <span title={id}>{id.length > 12 ? `${id.slice(0, 10)}…` : id}</span>;
}

function findGroupById(landscape: GroupLandscape, id: string): UserGroup | undefined {
  return (
    landscape.system.find((g) => g.id === id) ??
    landscape.admin.find((g) => g.id === id) ??
    landscape.user.find((g) => g.id === id)
  );
}

/** The default (no-owner-override) parent-tier options: system for admin-tier
 * creates, admin for user-tier creates, none for system-tier (it's the root). */
function defaultParentOptionsForTier(tier: UserGroupTier, landscape: GroupLandscape): UserGroup[] {
  if (tier === 'admin') return landscape.system;
  if (tier === 'user') return landscape.admin;
  return [];
}

/** The create-form title / create-button label are the same tier -> text mapping. */
function groupTierTitle(tier: UserGroupTier, t: Translation): string {
  switch (tier) {
    case 'system':
      return t.groupsCreateSystemTitle;
    case 'admin':
      return t.groupsCreateAdminTitle;
    case 'user':
      return t.groupsCreateUserTitle;
  }
}

type ManagePermFlag =
  | 'can_manage_users'
  | 'can_manage_group'
  | 'can_manage_servers'
  | 'can_manage_services'
  | 'can_manage_resources';

type ManagePermPatch = Partial<{
  canManageUsers: boolean;
  canManageGroup: boolean;
  canManageServers: boolean;
  canManageServices: boolean;
  canManageResources: boolean;
}>;

function managePermChecked(member: GroupMember, flag: ManagePermFlag): boolean {
  switch (flag) {
    case 'can_manage_users':
      return member.can_manage_users;
    case 'can_manage_group':
      return member.can_manage_group;
    case 'can_manage_servers':
      return member.can_manage_servers;
    case 'can_manage_services':
      return member.can_manage_services;
    case 'can_manage_resources':
      return member.can_manage_resources;
  }
}

function managePermLabel(flag: ManagePermFlag, t: Translation): string {
  switch (flag) {
    case 'can_manage_users':
      return t.groupsPermUsers;
    case 'can_manage_group':
      return t.groupsPermGroup;
    case 'can_manage_servers':
      return t.groupsPermServers;
    case 'can_manage_services':
      return t.groupsPermServices;
    case 'can_manage_resources':
      return t.groupsPermResources;
  }
}

function managePermPatch(flag: ManagePermFlag, checked: boolean): ManagePermPatch {
  switch (flag) {
    case 'can_manage_users':
      return { canManageUsers: checked };
    case 'can_manage_group':
      return { canManageGroup: checked };
    case 'can_manage_servers':
      return { canManageServers: checked };
    case 'can_manage_services':
      return { canManageServices: checked };
    case 'can_manage_resources':
      return { canManageResources: checked };
  }
}

/**
 * Role-gated Benutzergruppen (user-groups) management: a strict 3-tier
 * hierarchy (system -> admin -> user) plus user-tier invitations. Every
 * authenticated principal manages their own user-tier groups + invitations;
 * admin+ additionally manage admin-tier groups; system_admin additionally
 * manages system-tier groups (spec §7, §12).
 *
 * The "Members" sub-view shows the group's real current roster (via
 * api.groupMembers) as a ListTable with per-row actions (as opposed to
 * api.groupCandidates, which lists only ADD-able users, i.e. NOT current
 * members): Befoerdern (promote) on a plain member who isn't already a
 * manager or the owner; Degradieren (demote) + Eigentuemer wechseln
 * (transfer -- the backend requires the new owner to already be a manager,
 * mirrored here) on a current manager; Entfernen (remove) on any non-owner
 * row. The owner row never gets a destructive action. Promote/demote/
 * transfer are owner-only (canManageRoles); remove is available to the
 * owner OR any co-manager (group.can_manage).
 */
export function GroupsView({
  t,
  api,
  role,
  userId,
  systemAdminMode = false,
}: Readonly<{
  t: Translation;
  api: Pick<
    PortalApi,
    | 'acceptInvitation'
    | 'addGroupMembers'
    | 'adminOwnerCandidates'
    | 'createGroup'
    | 'declineInvitation'
    | 'deleteGroup'
    | 'demoteManager'
    | 'groupCandidates'
    | 'groupInvitations'
    | 'groupMembers'
    | 'groups'
    | 'promoteManager'
    | 'removeGroupMember'
    | 'renameGroup'
    | 'setManagerPermissions'
    | 'transferGroup'
  >;
  /** Whether the caller is currently in System-Admin mode (elevated). The
      system-tier group section is gated on THIS, not the raw role, so a
      non-elevated system_admin (who acts as a plain admin, and whose backend
      ListGroups omits the system tier + refuses every system-group action)
      does not see an empty, non-functional system panel. */
  systemAdminMode?: boolean;
  /** "user" | "admin" | "system_admin" (mirrors ServicesView's plain-string
      role prop; CurrentUser.role is untyped string on the wire). */
  role: string;
  userId: string;
}>) {
  const { showError } = useToast();
  const {
    data: landscapeData,
    setData: setLandscapeData,
    error: landscapeError,
    loading: landscapeLoading,
  } = useResource(() => api.groups(), [api, t], t, { trackLoading: false });
  const {
    data: invitationsData,
    setData: setInvitationsData,
    error: invitationsError,
  } = useResource(() => api.groupInvitations(), [api, t], t, { trackLoading: false });
  useEffect(() => {
    if (landscapeError) showError(landscapeError);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [landscapeError]);
  useEffect(() => {
    if (invitationsError) showError(invitationsError);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [invitationsError]);
  const landscape = landscapeData ?? EMPTY_LANDSCAPE;
  const invitations = invitationsData ?? [];

  async function reloadLandscape(): Promise<GroupLandscape> {
    const next = await api.groups();
    setLandscapeData(next);
    return next;
  }

  async function reloadInvitations() {
    setInvitationsData(await api.groupInvitations());
  }

  const [mode, setMode] = useState<Mode>('list');
  const [busy, setBusy] = useState(false);
  const [name, setName] = useState('');
  const [parentId, setParentId] = useState('');
  // System-admin-only "create an admin group FOR another admin" owner picker
  // (spec: 2026-08-10-system-admin-create-admin-group-for-owner).
  const [ownerId, setOwnerId] = useState('');
  const [ownerCandidates, setOwnerCandidates] = useState<AdminOwnerCandidate[]>([]);
  const [deleteTarget, setDeleteTarget] = useState<UserGroup | null>(null);
  const [leaveTarget, setLeaveTarget] = useState<UserGroup | null>(null);

  // "Manage members" sub-view state.
  const [candidates, setCandidates] = useState<UserRef[] | null>(null);
  const [candidatesLoading, setCandidatesLoading] = useState(false);
  const [selectedCandidates, setSelectedCandidates] = useState<string[]>([]);
  const [memberBusy, setMemberBusy] = useState(false);
  const [transferTarget, setTransferTarget] = useState<UserGroup | null>(null);

  // The group's real current roster -- feeds the member ListTable (as
  // opposed to `candidates` above, which is the ADD-able list, i.e. NOT
  // current members).
  const [roster, setRoster] = useState<GroupMember[] | null>(null);
  const [rosterLoading, setRosterLoading] = useState(false);
  // Latest-wins token (mirrors BenchmarkSection's historyReqRef): a slow
  // response for a since-switched group can't clobber a newer one.
  const rosterReqRef = useRef(0);
  // Set by the row-level "Eigentuemer wechseln" action, consumed by
  // confirmTransfer() once the ConfirmDialog is accepted.
  const [transferUserId, setTransferUserId] = useState('');

  const systemById = useMemo(
    () => new Map(landscape.system.map((g) => [g.id, g.name])),
    [landscape.system],
  );
  const adminById = useMemo(
    () => new Map(landscape.admin.map((g) => [g.id, g.name])),
    [landscape.admin],
  );

  function openCreate(tier: UserGroupTier) {
    setName('');
    setOwnerId('');
    const options = defaultParentOptionsForTier(tier, landscape);
    setParentId(options.length === 1 ? options[0].id : '');
    setMode({ kind: 'create', tier });
    if (tier === 'admin' && systemAdminMode) {
      void api
        .adminOwnerCandidates()
        .then(setOwnerCandidates)
        .catch((err) => {
          setOwnerCandidates([]);
          showError(formatPortalError(err, t));
        });
    } else {
      setOwnerCandidates([]);
    }
  }

  // Re-derives the parent from the chosen owner's own system groups (an
  // owner-scoped admin group must live under a system group THAT OWNER
  // belongs to, not the caller's); blank ("myself") reverts to the caller's
  // own landscape.system options.
  function onOwnerChange(next: string) {
    setOwnerId(next);
    const cand = ownerCandidates.find((c) => c.user_id === next);
    const groups = next === '' ? landscape.system : (cand?.system_groups ?? []);
    setParentId(groups.length === 1 ? groups[0].id : '');
  }

  function openEdit(group: UserGroup) {
    setName(group.name);
    setMode({ kind: 'edit', group });
  }

  function loadCandidates(groupId: string) {
    setCandidatesLoading(true);
    api
      .groupCandidates(groupId)
      .then((list) => setCandidates(list))
      .catch((err) => {
        showError(formatPortalError(err, t));
        setCandidates([]);
      })
      .finally(() => setCandidatesLoading(false));
  }

  // Loads groupId's current roster (Task 14b), guarded by the latest-wins
  // token so a slow response for a since-switched group can't win.
  function loadRoster(groupId: string) {
    const token = ++rosterReqRef.current;
    setRosterLoading(true);
    api
      .groupMembers(groupId)
      .then((list) => {
        if (rosterReqRef.current === token) setRoster(list);
      })
      .catch((err) => {
        if (rosterReqRef.current === token) {
          showError(formatPortalError(err, t));
          setRoster([]);
        }
      })
      .finally(() => {
        if (rosterReqRef.current === token) setRosterLoading(false);
      });
  }

  function openMembers(group: UserGroup) {
    setSelectedCandidates([]);
    setTransferUserId('');
    setCandidates(null);
    setRoster(null);
    loadCandidates(group.id);
    loadRoster(group.id);
    setMode({ kind: 'members', group });
  }

  // Re-fetch the landscape after a membership/manager/ownership change; if the
  // sub-view is still open on that group, refresh its snapshot in place (and
  // its candidate list + roster, since who's addable/current just changed) —
  // falling back to the list if the group vanished (e.g. cascade-deleted when
  // it became empty+ownerless, spec §8.1).
  async function afterGroupChanged(groupId: string) {
    const next = await reloadLandscape();
    const updated = findGroupById(next, groupId);
    if (updated) {
      setMode((current) =>
        current !== 'list' && current.kind === 'members'
          ? { kind: 'members', group: updated }
          : current,
      );
      loadCandidates(groupId);
      loadRoster(groupId);
    } else {
      setMode('list');
    }
  }

  async function submitCreate(tier: UserGroupTier, event: SubmitEvent<HTMLFormElement>) {
    event.preventDefault();
    setBusy(true);
    try {
      const body: CreateGroupRequest =
        tier === 'system'
          ? { tier, name }
          : {
              tier,
              name,
              parent_group_id: parentId,
              ...(tier === 'admin' && ownerId ? { owner_user_id: ownerId } : {}),
            };
      await api.createGroup(body);
      setMode('list');
      await reloadLandscape();
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setBusy(false);
    }
  }

  async function submitRename(event: SubmitEvent<HTMLFormElement>) {
    event.preventDefault();
    if (mode === 'list' || mode.kind !== 'edit') return;
    setBusy(true);
    try {
      await api.renameGroup(mode.group.id, name);
      setMode('list');
      await reloadLandscape();
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setBusy(false);
    }
  }

  async function submitAddMembers(group: UserGroup, event: SubmitEvent<HTMLFormElement>) {
    event.preventDefault();
    if (selectedCandidates.length === 0) return;
    setMemberBusy(true);
    try {
      await api.addGroupMembers(group.id, selectedCandidates);
      setSelectedCandidates([]);
      await afterGroupChanged(group.id);
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setMemberBusy(false);
    }
  }

  async function removeMember(group: UserGroup, userId: string) {
    setMemberBusy(true);
    try {
      await api.removeGroupMember(group.id, userId);
      await afterGroupChanged(group.id);
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setMemberBusy(false);
    }
  }

  async function promote(group: UserGroup, userId: string) {
    setMemberBusy(true);
    try {
      await api.promoteManager(group.id, userId);
      await afterGroupChanged(group.id);
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setMemberBusy(false);
    }
  }

  async function demote(group: UserGroup, userId: string) {
    setMemberBusy(true);
    try {
      await api.demoteManager(group.id, userId);
      await afterGroupChanged(group.id);
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setMemberBusy(false);
    }
  }

  // Narrows/widens an EXISTING co-manager row's per-permission flags (spec
  // 2026-08-10 + Phase B can_manage_servers + Phase C can_manage_services +
  // Resource Groups Phase 1 can_manage_resources). `patch` carries only the
  // ONE flag the checkbox that fired toggled; the other four flags are
  // carried over from the member's current roster value, since the PATCH
  // body always sends ALL FIVE (the backend has no partial-flag update).
  async function setManagerPermission(
    group: UserGroup,
    member: GroupMember,
    patch: ManagePermPatch,
  ) {
    setMemberBusy(true);
    try {
      await api.setManagerPermissions(group.id, member.user_id, {
        canManageUsers: patch.canManageUsers ?? member.can_manage_users,
        canManageGroup: patch.canManageGroup ?? member.can_manage_group,
        canManageServers: patch.canManageServers ?? member.can_manage_servers,
        canManageServices: patch.canManageServices ?? member.can_manage_services,
        canManageResources: patch.canManageResources ?? member.can_manage_resources,
      });
      await afterGroupChanged(group.id);
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setMemberBusy(false);
    }
  }

  async function confirmTransfer() {
    if (!transferTarget) return;
    const uid = transferUserId;
    const group = transferTarget;
    setTransferTarget(null);
    if (!uid) return;
    setMemberBusy(true);
    try {
      await api.transferGroup(group.id, uid);
      setTransferUserId('');
      await afterGroupChanged(group.id);
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setMemberBusy(false);
    }
  }

  async function confirmDelete() {
    if (!deleteTarget) return;
    const group = deleteTarget;
    setDeleteTarget(null);
    try {
      await api.deleteGroup(group.id);
      await reloadLandscape();
    } catch (err) {
      showError(formatPortalError(err, t));
    }
  }

  async function confirmLeave() {
    if (!leaveTarget) return;
    const group = leaveTarget;
    setLeaveTarget(null);
    try {
      await api.removeGroupMember(group.id, userId);
      await reloadLandscape();
    } catch (err) {
      showError(formatPortalError(err, t));
    }
  }

  async function respondInvitation(invitation: GroupInvitation, accept: boolean) {
    try {
      if (accept) await api.acceptInvitation(invitation.group_id);
      else await api.declineInvitation(invitation.group_id);
      await Promise.all([reloadInvitations(), reloadLandscape()]);
    } catch (err) {
      showError(formatPortalError(err, t));
    }
  }

  function roleLabel(value: string): string {
    if (value === 'owner') return t.groupsRoleOwner;
    if (value === 'manager') return t.groupsRoleManager;
    if (value === 'member') return t.groupsRoleMember;
    return '–';
  }

  function ownerCell(group: UserGroup): ReactNode {
    if (!group.owner_user_id) return '–';
    if (group.owner_user_id === userId) return t.groupsOwnerSelf;
    // Prefer the resolved display name; fall back to the id when the backend
    // couldn't resolve it (e.g. a lookup failure).
    if (group.owner_name) return group.owner_name;
    return <ShortId id={group.owner_user_id} />;
  }

  function parentCell(group: UserGroup, byId: Map<string, string>): ReactNode {
    if (!group.parent_group_id) return '–';
    const name = byId.get(group.parent_group_id);
    return name ?? <ShortId id={group.parent_group_id} />;
  }

  function rowActions(group: UserGroup): RowAction[] {
    const actions: RowAction[] = [];
    if (group.can_manage) {
      actions.push(
        {
          key: 'rename',
          label: t.groupsActionRename,
          icon: <EditIcon fontSize="small" />,
          onClick: () => openEdit(group),
        },
        {
          key: 'members',
          label: t.groupsActionMembers,
          icon: <GroupIcon fontSize="small" />,
          onClick: () => openMembers(group),
        },
        {
          key: 'delete',
          label: t.groupsActionDelete,
          icon: <DeleteIcon fontSize="small" />,
          color: 'error',
          onClick: () => setDeleteTarget(group),
        },
      );
    }
    if (group.my_role === 'member') {
      actions.push({
        key: 'leave',
        label: t.groupsActionLeave,
        icon: <LogoutIcon fontSize="small" />,
        onClick: () => setLeaveTarget(group),
      });
    }
    return actions;
  }

  const systemColumns: ListColumn<UserGroup>[] = [
    { id: 'name', label: t.tableName, value: (g) => g.name, filter: 'text' },
    {
      id: 'members',
      label: t.groupsColMembers,
      value: (g) => String(g.member_count),
      numeric: true,
    },
  ];

  const adminColumns: ListColumn<UserGroup>[] = [
    { id: 'name', label: t.tableName, value: (g) => g.name, filter: 'text' },
    {
      id: 'parent',
      label: t.groupsColParent,
      value: (g) => systemById.get(g.parent_group_id) ?? g.parent_group_id,
      render: (g) => parentCell(g, systemById),
    },
    {
      id: 'owner',
      label: t.groupsColOwner,
      value: (g) => g.owner_name || g.owner_user_id,
      render: (g) => ownerCell(g),
    },
    {
      id: 'members',
      label: t.groupsColMembers,
      value: (g) => String(g.member_count),
      numeric: true,
    },
    {
      id: 'managers',
      label: t.groupsColManagers,
      value: (g) => String(g.manager_count),
      numeric: true,
    },
    {
      id: 'role',
      label: t.groupsColMyRole,
      value: (g) => g.my_role,
      filter: 'enum',
      searchable: false,
      enumLabel: (v) => roleLabel(v),
      render: (g) => roleLabel(g.my_role),
    },
  ];

  const userColumns: ListColumn<UserGroup>[] = [
    { id: 'name', label: t.tableName, value: (g) => g.name, filter: 'text' },
    {
      id: 'parent',
      label: t.groupsColParent,
      value: (g) => adminById.get(g.parent_group_id) ?? g.parent_group_id,
      render: (g) => parentCell(g, adminById),
    },
    {
      id: 'owner',
      label: t.groupsColOwner,
      value: (g) => g.owner_name || g.owner_user_id,
      render: (g) => ownerCell(g),
    },
    {
      id: 'members',
      label: t.groupsColMembers,
      value: (g) => String(g.member_count),
      numeric: true,
    },
    {
      id: 'managers',
      label: t.groupsColManagers,
      value: (g) => String(g.manager_count),
      numeric: true,
    },
    {
      id: 'role',
      label: t.groupsColMyRole,
      value: (g) => g.my_role,
      filter: 'enum',
      searchable: false,
      enumLabel: (v) => roleLabel(v),
      render: (g) => roleLabel(g.my_role),
    },
  ];

  const listLabels = listTableLabels(t);

  // Always returns the SAME element structure (Tooltip > span > Button)
  // regardless of `disabledReason` — an empty title never shows a Tooltip
  // popper, so this is a no-op when enabled, but it keeps the disabled<->
  // enabled transition (e.g. once the landscape finishes loading) a plain
  // attribute update rather than a structural swap that would unmount and
  // remount a fresh DOM node (which orphans in-flight event handlers/refs).
  function createButton(tier: UserGroupTier) {
    let disabledReason = '';
    if (tier === 'admin' && landscape.system.length === 0) {
      disabledReason = t.groupsNoSystemGroupHint;
    } else if (tier === 'user' && landscape.admin.length === 0) {
      disabledReason = t.groupsNoAdminGroupHint;
    }
    const label = groupTierTitle(tier, t);
    return (
      <Tooltip title={disabledReason}>
        <span>
          <Button
            variant="contained"
            startIcon={<AddIcon />}
            disabled={disabledReason !== ''}
            onClick={() => openCreate(tier)}
          >
            {label}
          </Button>
        </span>
      </Tooltip>
    );
  }

  // --- Create / edit / members sub-views (replace the list in place) -------

  if (mode !== 'list' && mode.kind === 'create') {
    const tier = mode.tier;
    const showOwnerPicker = tier === 'admin' && systemAdminMode;
    const selectedOwner = ownerCandidates.find((c) => c.user_id === ownerId);
    // When an owner is chosen, the parent MUST be one of the owner's own
    // system groups; otherwise the caller's own landscape.system options.
    const parentOptions =
      tier === 'admin' && showOwnerPicker && ownerId
        ? (selectedOwner?.system_groups ?? [])
        : defaultParentOptionsForTier(tier, landscape);
    const title = groupTierTitle(tier, t);
    const ownerChosen = showOwnerPicker && ownerId !== '';
    let noParentHint: string;
    if (ownerChosen) {
      noParentHint = t.groupsOwnerNoSystemGroupHint;
    } else if (tier === 'admin') {
      noParentHint = t.groupsNoSystemGroupHint;
    } else {
      noParentHint = t.groupsNoAdminGroupHint;
    }
    const needsParent = tier !== 'system';
    const parentUnresolved = needsParent && parentOptions.length > 1 && parentId === '';
    const parentUnavailable = needsParent && parentOptions.length === 0;
    let parentField: ReactNode;
    if (parentUnavailable) {
      parentField = <Alert severity="warning">{noParentHint}</Alert>;
    } else if (parentOptions.length === 1) {
      parentField = (
        <Typography variant="body2" color="text.secondary">
          {t.groupsParentAuto(parentOptions[0].name)}
        </Typography>
      );
    } else {
      parentField = (
        <SearchableSelect
          id="group-parent"
          label={t.groupsParentLabel}
          value={parentId}
          onChange={setParentId}
          options={parentOptions.map((g) => ({ value: g.id, label: g.name }))}
        />
      );
    }
    return (
      <>
        <Breadcrumbs
          ariaLabel={t.breadcrumb}
          backLabel={t.back}
          items={[{ label: t.groups, onClick: () => setMode('list') }, { label: title }]}
        />
        <Panel titleId="groups-create-heading" title={title}>
          <Box
            component="form"
            onSubmit={(event) => void submitCreate(tier, event)}
            sx={{ display: 'grid', gridTemplateColumns: 'minmax(220px, 360px)', gap: 2.25 }}
          >
            <Field
              id="group-name"
              label={t.tableName}
              value={name}
              onChange={(e) => setName(e.target.value)}
              required
            />
            {showOwnerPicker && (
              <SearchableSelect
                id="group-owner"
                label={t.groupsOwnerLabel}
                value={ownerId}
                onChange={onOwnerChange}
                options={[
                  { value: '', label: t.groupsOwnerSelf },
                  ...ownerCandidates.map((c) => ({
                    value: c.user_id,
                    label: `${c.display_name} (${c.email})`,
                  })),
                ]}
              />
            )}
            {needsParent && parentField}
            <Box sx={{ display: 'flex', gap: 1.5 }}>
              <Button
                type="submit"
                variant="contained"
                disabled={busy || parentUnavailable || parentUnresolved}
              >
                {t.save}
              </Button>
              <Button
                type="button"
                variant="text"
                color="secondary"
                onClick={() => setMode('list')}
              >
                {t.cancel}
              </Button>
            </Box>
          </Box>
        </Panel>
      </>
    );
  }

  if (mode !== 'list' && mode.kind === 'edit') {
    return (
      <>
        <Breadcrumbs
          ariaLabel={t.breadcrumb}
          backLabel={t.back}
          items={[
            { label: t.groups, onClick: () => setMode('list') },
            { label: t.groupsEditTitle },
          ]}
        />
        <Panel titleId="groups-edit-heading" title={t.groupsEditTitle}>
          <Box
            component="form"
            onSubmit={submitRename}
            sx={{ display: 'grid', gridTemplateColumns: 'minmax(220px, 360px)', gap: 2.25 }}
          >
            <Field
              id="group-edit-name"
              label={t.tableName}
              value={name}
              onChange={(e) => setName(e.target.value)}
              required
            />
            <Box sx={{ display: 'flex', gap: 1.5 }}>
              <Button type="submit" variant="contained" disabled={busy}>
                {t.save}
              </Button>
              <Button
                type="button"
                variant="text"
                color="secondary"
                onClick={() => setMode('list')}
              >
                {t.cancel}
              </Button>
            </Box>
          </Box>
        </Panel>
      </>
    );
  }

  if (mode !== 'list' && mode.kind === 'members') {
    const group = mode.group;
    const candidateOptions = (candidates ?? []).map((c) => ({
      value: c.id,
      label: c.display_name || c.email,
      sublabel: c.email,
    }));
    const addLabel = group.tier === 'user' ? t.groupsActionInvite : t.groupsActionAdd;
    const rosterRows = roster ?? [];
    // Promote/demote/transfer are owner-only (mirrors the backend's owner-only
    // authorizeOwnerAction gate on PromoteManager/DemoteManager/TransferOwnership);
    // Entfernen (remove) is available to the owner OR any co-manager, since
    // reaching this sub-view at all already required group.can_manage.
    const canManageRoles = group.tier !== 'system' && group.my_role === 'owner';
    // Per-Admin-Group co-manager permissions (spec 2026-08-10): the two
    // permission checkboxes below are editable ONLY by the group's owner or
    // a system_admin -- mirrors the backend's authorizeOwnerAction gate on
    // SetManagerPermissions exactly (owner OR the `system` scope, which the
    // caller only carries while in System-Admin mode). Anyone else with
    // reach into this sub-view (a co-manager, via group.can_manage) sees the
    // same two checkboxes but disabled -- state visible, no toggle.
    const canEditPermissions =
      group.tier !== 'system' && (group.my_role === 'owner' || systemAdminMode);

    function memberRoleKey(m: GroupMember): string {
      if (m.is_owner) return 'owner';
      if (m.is_manager) return 'manager';
      if (m.state === 'invited') return 'invited';
      return 'member';
    }
    function memberRoleLabelForKey(key: string): string {
      if (key === 'owner') return t.groupsRoleOwner;
      if (key === 'manager') return t.groupsRoleManager;
      if (key === 'invited') return t.groupsMemberStateInvited;
      return t.groupsRoleMember;
    }

    // Renders one of the five per-co-manager permission checkboxes. Only a
    // co-manager row (is_manager, never the owner -- the owner's implicit
    // full permission isn't a stored flag, and a plain member/invited row
    // has no permission relationship at all) shows a real checkbox; every
    // other row shows a dash. Always the SAME element kind (a Checkbox,
    // merely toggling `disabled`) for a non-editing viewer, so the
    // editable<->read-only transition (e.g. on entering System-Admin mode)
    // never unmounts/remounts the control.
    function memberPermCell(member: GroupMember, flag: ManagePermFlag): ReactNode {
      if (member.is_owner || !member.is_manager) return '–';
      const checked = managePermChecked(member, flag);
      const label = managePermLabel(flag, t);
      return (
        <Checkbox
          checked={checked}
          size="small"
          disabled={!canEditPermissions || memberBusy}
          slotProps={{
            input: { 'aria-label': `${label} – ${member.display_name || member.email}` },
          }}
          // Guard in the handler too, not just via the `disabled` prop --
          // belt-and-suspenders so a read-only viewer's checkbox can never
          // reach the API even if some input path bypasses the native
          // disabled attribute (the backend's authorizeOwnerAction is the
          // real authority regardless).
          onChange={(e) => {
            if (!canEditPermissions) return;
            void setManagerPermission(group, member, managePermPatch(flag, e.target.checked));
          }}
        />
      );
    }

    const memberColumns: ListColumn<GroupMember>[] = [
      {
        id: 'name',
        label: t.tableName,
        value: (m) => m.display_name || m.email,
        filter: 'text',
        render: (m) => (
          <Box sx={{ display: 'flex', flexDirection: 'column', minWidth: 0 }}>
            <Typography sx={{ fontWeight: 600 }}>{m.display_name || m.email}</Typography>
            {m.display_name && m.email && (
              <Typography variant="caption" color="text.secondary">
                {m.email}
              </Typography>
            )}
          </Box>
        ),
      },
      {
        id: 'role',
        label: t.tableRole,
        value: (m) => memberRoleKey(m),
        filter: 'enum',
        searchable: false,
        enumLabel: (v) => memberRoleLabelForKey(v),
        render: (m) => memberRoleLabelForKey(memberRoleKey(m)),
      },
      // The five per-co-manager permission columns don't apply to a
      // system-tier group (no owner/manager concept there -- every row's
      // is_manager is always false), so they're omitted entirely rather
      // than rendered as a column of dashes.
      ...(group.tier !== 'system'
        ? [
            {
              id: 'perm-users',
              label: t.groupsPermUsers,
              value: (m: GroupMember) => (m.is_manager ? String(m.can_manage_users) : ''),
              searchable: false,
              render: (m: GroupMember) => memberPermCell(m, 'can_manage_users'),
            },
            {
              id: 'perm-group',
              label: t.groupsPermGroup,
              value: (m: GroupMember) => (m.is_manager ? String(m.can_manage_group) : ''),
              searchable: false,
              render: (m: GroupMember) => memberPermCell(m, 'can_manage_group'),
            },
            {
              id: 'perm-servers',
              label: t.groupsPermServers,
              value: (m: GroupMember) => (m.is_manager ? String(m.can_manage_servers) : ''),
              searchable: false,
              render: (m: GroupMember) => memberPermCell(m, 'can_manage_servers'),
            },
            {
              id: 'perm-services',
              label: t.groupsPermServices,
              value: (m: GroupMember) => (m.is_manager ? String(m.can_manage_services) : ''),
              searchable: false,
              render: (m: GroupMember) => memberPermCell(m, 'can_manage_services'),
            },
            {
              id: 'perm-resources',
              label: t.groupsPermResources,
              value: (m: GroupMember) => (m.is_manager ? String(m.can_manage_resources) : ''),
              searchable: false,
              render: (m: GroupMember) => memberPermCell(m, 'can_manage_resources'),
            },
          ]
        : []),
    ];

    // Eligibility mirrors the backend exactly: Befoerdern targets an
    // accepted plain member (state=member, not already a manager, not the
    // owner); Degradieren + Eigentuemer wechseln both target a current
    // manager (TransferOwnership rejects a non-manager target with
    // ErrGroupCandidateInvalid, so the transfer action is only offered on a
    // manager row, exactly like the demote action); Entfernen targets any
    // non-owner row. The owner row itself never gets a destructive action.
    function memberActions(member: GroupMember): RowAction[] {
      if (member.is_owner) return [];
      const actions: RowAction[] = [];
      if (canManageRoles) {
        if (member.state === 'member' && !member.is_manager) {
          actions.push({
            key: 'promote',
            label: t.groupsActionPromote,
            icon: <ArrowUpwardIcon fontSize="small" />,
            disabled: memberBusy,
            onClick: () => void promote(group, member.user_id),
          });
        }
        if (member.is_manager) {
          actions.push(
            {
              key: 'demote',
              label: t.groupsActionDemote,
              icon: <ArrowDownwardIcon fontSize="small" />,
              disabled: memberBusy,
              onClick: () => void demote(group, member.user_id),
            },
            {
              key: 'transfer',
              label: t.groupsActionTransfer,
              icon: <SwapHorizIcon fontSize="small" />,
              disabled: memberBusy,
              onClick: () => {
                setTransferUserId(member.user_id);
                setTransferTarget(group);
              },
            },
          );
        }
      }
      if (group.can_manage) {
        actions.push({
          key: 'remove',
          label: t.groupsActionRemoveMember,
          icon: <PersonRemoveIcon fontSize="small" />,
          color: 'error',
          disabled: memberBusy,
          onClick: () => void removeMember(group, member.user_id),
        });
      }
      return actions;
    }

    const memberListLabels = { ...listLabels, empty: t.groupsRosterEmpty };

    return (
      <>
        <Breadcrumbs
          ariaLabel={t.breadcrumb}
          backLabel={t.back}
          items={[
            { label: t.groups, onClick: () => setMode('list') },
            { label: t.groupsMembersTitle(group.name) },
          ]}
        />
        <Panel titleId="groups-members-heading" title={t.groupsMembersTitle(group.name)}>
          <Stack spacing={3}>
            <Stack direction="row" spacing={4}>
              <Typography variant="body2">
                {t.groupsColMembers}: {group.member_count}
              </Typography>
              {group.tier !== 'system' && (
                <Typography variant="body2">
                  {t.groupsColManagers}: {group.manager_count}
                </Typography>
              )}
            </Stack>
            <Box>
              <Typography variant="subtitle2" gutterBottom>
                {t.groupsRosterLabel}
              </Typography>
              <ListTable
                rows={rosterRows}
                columns={memberColumns}
                rowKey={(m) => m.user_id}
                actions={memberActions}
                storageKey="op.group-members"
                labels={memberListLabels}
                loading={rosterLoading && roster === null}
              />
            </Box>
            <Box component="form" onSubmit={(event) => void submitAddMembers(group, event)}>
              <Stack spacing={1.25}>
                <MultiSelectField
                  id="group-candidates"
                  label={t.groupsAddMembersLabel}
                  options={candidateOptions}
                  selected={selectedCandidates}
                  onChange={setSelectedCandidates}
                  disabled={candidatesLoading}
                />
                <Typography variant="caption" color="text.secondary">
                  {t.groupsAddMembersHelp}
                </Typography>
                <Box>
                  <Button
                    type="submit"
                    variant="contained"
                    disabled={memberBusy || selectedCandidates.length === 0}
                  >
                    {addLabel}
                  </Button>
                </Box>
              </Stack>
            </Box>
          </Stack>
        </Panel>
        <ConfirmDialog
          open={transferTarget !== null}
          title={t.groupsTransferConfirmTitle}
          body={t.groupsTransferConfirmBody}
          confirmLabel={t.groupsActionTransfer}
          cancelLabel={t.cancel}
          onConfirm={() => void confirmTransfer()}
          onCancel={() => setTransferTarget(null)}
        />
      </>
    );
  }

  // --- List view -------------------------------------------------------------

  return (
    <>
      <PageTitle title={t.groups} subtitle={t.groupsIntro} />
      <Stack spacing={3}>
        <Panel titleId="groups-invitations-heading" title={t.groupsInvitationsTitle}>
          {invitations.length === 0 ? (
            <Typography color="text.secondary">{t.groupsInvitationsEmpty}</Typography>
          ) : (
            <Stack spacing={1.5}>
              {invitations.map((invitation) => (
                <Box
                  key={invitation.group_id}
                  sx={{
                    display: 'flex',
                    alignItems: 'center',
                    justifyContent: 'space-between',
                    gap: 2,
                    p: 1.5,
                    border: '1px solid var(--line)',
                  }}
                >
                  <Box>
                    <Typography sx={{ fontWeight: 600 }}>{invitation.group_name}</Typography>
                    <Typography variant="caption" color="text.secondary">
                      {t.groupsInvitedByLabel}: <ShortId id={invitation.invited_by} />
                    </Typography>
                  </Box>
                  <Box sx={{ display: 'flex', gap: 1, flexShrink: 0 }}>
                    <Button
                      size="small"
                      variant="contained"
                      onClick={() => void respondInvitation(invitation, true)}
                    >
                      {t.groupsActionAccept}
                    </Button>
                    <Button
                      size="small"
                      variant="text"
                      color="secondary"
                      onClick={() => void respondInvitation(invitation, false)}
                    >
                      {t.groupsActionDecline}
                    </Button>
                  </Box>
                </Box>
              ))}
            </Stack>
          )}
        </Panel>
        <Panel
          titleId="groups-user-heading"
          title={t.groupsUserTitle}
          actions={createButton('user')}
        >
          <ListTable
            rows={landscape.user}
            columns={userColumns}
            rowKey={(g) => g.id}
            actions={rowActions}
            storageKey="op.groups.user"
            labels={listLabels}
            loading={landscapeLoading}
          />
        </Panel>
        {(role === 'admin' || role === 'system_admin') && (
          <Panel
            titleId="groups-admin-heading"
            title={t.groupsAdminTitle}
            actions={createButton('admin')}
          >
            <ListTable
              rows={landscape.admin}
              columns={adminColumns}
              rowKey={(g) => g.id}
              actions={rowActions}
              storageKey="op.groups.admin"
              labels={listLabels}
              loading={landscapeLoading}
            />
          </Panel>
        )}
        {systemAdminMode && (
          <Panel
            titleId="groups-system-heading"
            title={t.groupsSystemTitle}
            actions={createButton('system')}
          >
            <ListTable
              rows={landscape.system}
              columns={systemColumns}
              rowKey={(g) => g.id}
              actions={rowActions}
              storageKey="op.groups.system"
              labels={listLabels}
              loading={landscapeLoading}
            />
          </Panel>
        )}
      </Stack>
      <ConfirmDialog
        open={deleteTarget !== null}
        title={t.groupsDeleteConfirmTitle}
        body={t.groupsDeleteConfirmBody}
        extra={
          deleteTarget && (deleteTarget.coupled_projects?.length ?? 0) > 0 ? (
            <Alert severity="warning" sx={{ mt: 1 }}>
              {t.groupsDeleteCoupledHint(
                deleteTarget.coupled_projects!.map((p) => p.name).join(', '),
              )}
            </Alert>
          ) : undefined
        }
        confirmLabel={t.groupsActionDelete}
        cancelLabel={t.cancel}
        onConfirm={() => void confirmDelete()}
        onCancel={() => setDeleteTarget(null)}
      />
      <ConfirmDialog
        open={leaveTarget !== null}
        title={t.groupsLeaveConfirmTitle}
        body={t.groupsLeaveConfirmBody}
        confirmLabel={t.groupsActionLeave}
        cancelLabel={t.cancel}
        onConfirm={() => void confirmLeave()}
        onCancel={() => setLeaveTarget(null)}
      />
    </>
  );
}
