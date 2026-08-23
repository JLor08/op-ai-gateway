// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { GroupsView } from './GroupsView';
import { ToastProvider } from './shared/ToastProvider';
import { messages } from '../i18n';
import type {
  AdminOwnerCandidate,
  CreateGroupRequest,
  GroupInvitation,
  GroupLandscape,
  GroupMember,
  UserGroup,
} from '../api';
import type { PortalApi } from './shared/types';

const t = messages.de;

function makeGroup(overrides: Partial<UserGroup> = {}): UserGroup {
  return {
    id: 'ugrp_1',
    tier: 'user',
    name: 'Test Group',
    parent_group_id: '',
    owner_user_id: '',
    owner_name: '',
    my_role: '',
    can_manage: false,
    can_manage_users: false,
    can_manage_servers: false,
    can_manage_services: false,
    can_manage_resources: false,
    member_count: 0,
    manager_count: 0,
    ...overrides,
  };
}

const EMPTY_LANDSCAPE: GroupLandscape = { system: [], admin: [], user: [] };

function renderGroupsView(
  opts: {
    landscape?: GroupLandscape;
    invitations?: GroupInvitation[];
    role?: string;
    userId?: string;
    systemAdminMode?: boolean;
    overrides?: Partial<PortalApi>;
  } = {},
) {
  const landscape = opts.landscape ?? EMPTY_LANDSCAPE;
  // Mutable so accept/decline can remove the responded-to invitation before the
  // component's post-action refetch — otherwise the refetch would just hand
  // back the original (still-pending) list and the UI could never settle.
  let currentInvitations = [...(opts.invitations ?? [])];
  const fakeApi = {
    groups: vi.fn(async () => landscape),
    groupInvitations: vi.fn(async () => currentInvitations),
    groupCandidates: vi.fn(async () => []),
    adminOwnerCandidates: vi.fn(async () => [] as AdminOwnerCandidate[]),
    createGroup: vi.fn(async (body: CreateGroupRequest) =>
      makeGroup({ tier: body.tier, name: body.name, parent_group_id: body.parent_group_id ?? '' }),
    ),
    renameGroup: vi.fn(async (id: string, name: string) => makeGroup({ id, name })),
    transferGroup: vi.fn(async () => ({ ok: true })),
    deleteGroup: vi.fn(async () => ({ ok: true })),
    addGroupMembers: vi.fn(async () => ({ ok: true })),
    groupMembers: vi.fn(async () => []),
    removeGroupMember: vi.fn(async () => ({ ok: true })),
    promoteManager: vi.fn(async () => ({ ok: true })),
    demoteManager: vi.fn(async () => ({ ok: true })),
    setManagerPermissions: vi.fn(async () => ({ ok: true })),
    acceptInvitation: vi.fn(async (groupId: string) => {
      currentInvitations = currentInvitations.filter((inv) => inv.group_id !== groupId);
      return { ok: true };
    }),
    declineInvitation: vi.fn(async (groupId: string) => {
      currentInvitations = currentInvitations.filter((inv) => inv.group_id !== groupId);
      return { ok: true };
    }),
  };
  Object.assign(fakeApi, opts.overrides ?? {});

  render(
    <ToastProvider>
      <GroupsView
        t={t}
        api={fakeApi}
        role={opts.role ?? 'user'}
        userId={opts.userId ?? 'usr_1'}
        systemAdminMode={opts.systemAdminMode ?? false}
      />
    </ToastProvider>,
  );
  return { fakeApi };
}

afterEach(cleanup);

describe('GroupsView role-gated sections', () => {
  it("a plain user sees 'Meine Gruppen' + 'Einladungen' but not the admin/system sections", async () => {
    renderGroupsView({ role: 'user' });
    expect(await screen.findByText(t.groupsUserTitle)).toBeInTheDocument();
    expect(screen.getByText(t.groupsInvitationsTitle)).toBeInTheDocument();
    expect(screen.queryByText(t.groupsSystemTitle)).not.toBeInTheDocument();
    expect(screen.queryByText(t.groupsAdminTitle)).not.toBeInTheDocument();
  });

  it('an ELEVATED system_admin sees all three group sections plus invitations', async () => {
    renderGroupsView({ role: 'system_admin', systemAdminMode: true });
    expect(await screen.findByText(t.groupsSystemTitle)).toBeInTheDocument();
    expect(screen.getByText(t.groupsAdminTitle)).toBeInTheDocument();
    expect(screen.getByText(t.groupsUserTitle)).toBeInTheDocument();
    expect(screen.getByText(t.groupsInvitationsTitle)).toBeInTheDocument();
  });

  it("shows the owner's display name (not the raw user id) in the owner column", async () => {
    const owned = makeGroup({
      id: 'ugrp_owned',
      tier: 'admin',
      name: 'Owned Admin Group',
      owner_user_id: 'usr_other',
      owner_name: 'Alice Admin',
      my_role: 'member',
    });
    renderGroupsView({
      role: 'system_admin',
      systemAdminMode: true,
      userId: 'usr_me', // not the owner -> the non-self branch
      landscape: { system: [], admin: [owned], user: [] },
    });
    expect(await screen.findByText('Alice Admin')).toBeInTheDocument();
    // The opaque owner id must NOT be shown when a name is available.
    expect(screen.queryByText(/usr_other/)).toBeNull();
  });

  it('a NON-elevated system_admin does not see the system section (acts as an admin)', async () => {
    renderGroupsView({ role: 'system_admin', systemAdminMode: false });
    // Admin section renders first (role-gated); assert it to prove the
    // landscape loaded before checking the system section's ABSENCE.
    expect(await screen.findByText(t.groupsAdminTitle)).toBeInTheDocument();
    expect(screen.queryByText(t.groupsSystemTitle)).toBeNull();
  });

  it('an admin sees the admin section but not the system section', async () => {
    renderGroupsView({ role: 'admin' });
    await screen.findByText(t.groupsAdminTitle);
    expect(screen.queryByText(t.groupsSystemTitle)).not.toBeInTheDocument();
  });
});

describe('GroupsView create user-group', () => {
  it('creates a user-group with the auto-selected single admin-group parent', async () => {
    const landscape: GroupLandscape = {
      system: [],
      admin: [
        makeGroup({
          id: 'ugrp_admin1',
          tier: 'admin',
          name: 'Admin Group A',
          my_role: 'member',
          member_count: 1,
        }),
      ],
      user: [],
    };
    const { fakeApi } = renderGroupsView({ role: 'user', landscape });

    fireEvent.click(await screen.findByRole('button', { name: t.groupsCreateUserTitle }));
    fireEvent.change(await screen.findByLabelText(t.tableName), {
      target: { value: 'My New Group' },
    });
    fireEvent.click(screen.getByRole('button', { name: t.save }));

    await screen.findByText(t.groupsUserTitle);
    expect(fakeApi.createGroup).toHaveBeenCalledWith({
      tier: 'user',
      name: 'My New Group',
      parent_group_id: 'ugrp_admin1',
    });
  });
});

describe('GroupsView create admin-group for owner (system-admin only)', () => {
  it('shows the owner picker for an elevated system_admin creating an admin group and derives the parent from the owner', async () => {
    const candidates = [
      {
        user_id: 'usr_a',
        display_name: 'Admin A',
        email: 'a@example.test',
        system_groups: [{ id: 'sg1', name: 'SG One' }],
      },
    ];
    const createGroup = vi.fn(async (_body: CreateGroupRequest) =>
      makeGroup({ tier: 'admin', name: 'AG' }),
    );
    renderGroupsView({
      role: 'system_admin',
      systemAdminMode: true,
      landscape: {
        system: [
          { ...makeGroup({ id: 'sg1', tier: 'system', name: 'SG One' }) },
          { ...makeGroup({ id: 'sg2', tier: 'system', name: 'SG Two' }) },
        ],
        admin: [],
        user: [],
      },
      overrides: {
        adminOwnerCandidates: vi.fn(
          async () => candidates,
        ) as unknown as PortalApi['adminOwnerCandidates'],
        createGroup: createGroup as unknown as PortalApi['createGroup'],
      },
    });
    fireEvent.click(await screen.findByRole('button', { name: t.groupsCreateAdminTitle }));
    fireEvent.change(await screen.findByLabelText(t.tableName), { target: { value: 'AG' } });
    // Pick the owner; parent auto-resolves to the owner's single system group.
    const ownerCombo = await screen.findByRole('combobox', { name: t.groupsOwnerLabel });
    fireEvent.mouseDown(ownerCombo);
    fireEvent.click(await screen.findByRole('option', { name: 'Admin A (a@example.test)' }));
    fireEvent.click(screen.getByRole('button', { name: t.save }));
    await waitFor(() => expect(createGroup).toHaveBeenCalled());
    expect(createGroup.mock.calls[0][0]).toMatchObject({
      tier: 'admin',
      name: 'AG',
      owner_user_id: 'usr_a',
      parent_group_id: 'sg1',
    });
  });

  it('does not show the owner picker for a regular admin', async () => {
    renderGroupsView({
      role: 'admin',
      systemAdminMode: false,
      landscape: {
        system: [makeGroup({ id: 'sg1', tier: 'system', name: 'SG One' })],
        admin: [],
        user: [],
      },
    });
    fireEvent.click(await screen.findByRole('button', { name: t.groupsCreateAdminTitle }));
    await screen.findByLabelText(t.tableName);
    expect(screen.queryByRole('combobox', { name: t.groupsOwnerLabel })).toBeNull();
  });
});

describe('GroupsView delete confirmation', () => {
  it('warns with the coupled project names when deleting a group that has coupled projects', async () => {
    const landscape: GroupLandscape = {
      system: [],
      admin: [],
      user: [
        makeGroup({
          id: 'grp_1',
          tier: 'user',
          name: 'Team',
          owner_user_id: 'usr_1',
          my_role: 'owner',
          can_manage: true,
          member_count: 1,
          manager_count: 0,
          coupled_projects: [
            { id: 'p1', name: 'Alpha' },
            { id: 'p2', name: 'Beta' },
          ],
        }),
      ],
    };
    renderGroupsView({ role: 'user', landscape, userId: 'usr_1' });

    fireEvent.click(await screen.findByRole('button', { name: t.groupsActionDelete }));

    expect(await screen.findByText(/Alpha/)).toBeInTheDocument();
    expect(screen.getByText(/Beta/)).toBeInTheDocument();
  });

  it('shows no coupled-project hint when the group has none', async () => {
    renderGroupsView({ role: 'user', landscape: makeOwnedGroupLandscape() });

    fireEvent.click(await screen.findByRole('button', { name: t.groupsActionDelete }));

    await screen.findByText(t.groupsDeleteConfirmTitle);
    expect(screen.queryByText(/gekoppelt/i)).not.toBeInTheDocument();
  });
});

describe('GroupsView invitations', () => {
  it('accepting an invitation calls api.acceptInvitation with the group id', async () => {
    const invitations: GroupInvitation[] = [
      {
        group_id: 'ugrp_invite_1',
        group_name: 'Invited Group',
        invited_by: 'usr_owner',
        parent_group_id: 'ugrp_admin1',
      },
    ];
    const { fakeApi } = renderGroupsView({ role: 'user', invitations });

    expect(await screen.findByText('Invited Group')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: t.groupsActionAccept }));

    await screen.findByText(t.groupsInvitationsEmpty);
    expect(fakeApi.acceptInvitation).toHaveBeenCalledWith('ugrp_invite_1');
  });

  it('declining an invitation calls api.declineInvitation with the group id', async () => {
    const invitations: GroupInvitation[] = [
      {
        group_id: 'ugrp_invite_2',
        group_name: 'Another Group',
        invited_by: 'usr_owner',
        parent_group_id: 'ugrp_admin1',
      },
    ];
    const { fakeApi } = renderGroupsView({ role: 'user', invitations });

    expect(await screen.findByText('Another Group')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: t.groupsActionDecline }));

    await screen.findByText(t.groupsInvitationsEmpty);
    expect(fakeApi.declineInvitation).toHaveBeenCalledWith('ugrp_invite_2');
  });
});

function makeMember(overrides: Partial<GroupMember> = {}): GroupMember {
  return {
    user_id: 'usr_x',
    email: 'x@example.test',
    display_name: 'X Person',
    state: 'member',
    is_manager: false,
    is_owner: false,
    can_manage_users: false,
    can_manage_group: false,
    can_manage_servers: false,
    can_manage_services: false,
    can_manage_resources: false,
    ...overrides,
  };
}

// A user-tier group the viewing principal owns (my_role/can_manage set so
// the "Mitglieder verwalten" row action is shown, and, inside that sub-view,
// the roster's owner-only promote/demote/transfer row actions).
function makeOwnedGroupLandscape(): GroupLandscape {
  const group = makeGroup({
    id: 'ugrp_1',
    tier: 'user',
    name: 'Roster Group',
    owner_user_id: 'usr_owner',
    my_role: 'owner',
    can_manage: true,
    member_count: 3,
    manager_count: 1,
  });
  return { system: [], admin: [], user: [group] };
}

async function openMembers() {
  fireEvent.click(await screen.findByRole('button', { name: t.groupsActionMembers }));
}

// Scopes a query to the <tr> containing displayName -- mirrors
// GroupServersSection.test.tsx / ModelServersSection.test.tsx's identical
// rowFor helper, needed here because several rows can share the SAME action
// label (e.g. "Entfernen" on every non-owner row).
function rowFor(displayName: string): HTMLElement {
  return screen.getByText(displayName).closest('tr')!;
}

describe('GroupsView members roster', () => {
  it("shows the group's real current roster as a table with a role column, and offers no action on the owner row", async () => {
    const roster: GroupMember[] = [
      makeMember({
        user_id: 'usr_owner',
        display_name: 'Owner Person',
        email: 'owner@example.test',
        is_owner: true,
      }),
      makeMember({
        user_id: 'usr_manager',
        display_name: 'Manager Person',
        email: 'manager@example.test',
        is_manager: true,
      }),
      makeMember({
        user_id: 'usr_plain',
        display_name: 'Plain Person',
        email: 'plain@example.test',
      }),
      makeMember({
        user_id: 'usr_invited',
        display_name: 'Invited Person',
        email: 'invited@example.test',
        state: 'invited',
      }),
    ];
    renderGroupsView({
      role: 'user',
      landscape: makeOwnedGroupLandscape(),
      overrides: { groupMembers: vi.fn(async () => roster) },
    });

    await openMembers();

    expect(await screen.findByText('Owner Person')).toBeInTheDocument();
    expect(screen.getByText('Manager Person')).toBeInTheDocument();
    expect(screen.getByText('Plain Person')).toBeInTheDocument();
    expect(screen.getByText('Invited Person')).toBeInTheDocument();
    expect(screen.getByText(t.groupsRoleOwner)).toBeInTheDocument();
    // "Mitverwalter" is ALSO the plain count label just above the roster
    // (t.groupsColManagers === t.groupsRoleManager, same word) -- the role
    // cell adds a second match rather than being the only one.
    expect(screen.getAllByText(t.groupsRoleManager).length).toBeGreaterThanOrEqual(1);
    expect(screen.getByText(t.groupsMemberStateInvited)).toBeInTheDocument();
    expect(screen.getByText(t.groupsRoleMember)).toBeInTheDocument();

    // Every row except the owner's gets a remove action.
    expect(screen.getAllByRole('button', { name: t.groupsActionRemoveMember })).toHaveLength(3);
    // The owner row gets NO action at all -- never even a remove/demote.
    const ownerRow = rowFor('Owner Person');
    expect(
      within(ownerRow).queryByRole('button', { name: t.groupsActionRemoveMember }),
    ).not.toBeInTheDocument();
    expect(
      within(ownerRow).queryByRole('button', { name: t.groupsActionDemote }),
    ).not.toBeInTheDocument();
  });

  it("removes a member via its row's remove action, calling api.removeGroupMember with that row's user id", async () => {
    let currentRoster: GroupMember[] = [
      makeMember({ user_id: 'usr_owner', display_name: 'Owner Person', is_owner: true }),
      makeMember({ user_id: 'usr_plain', display_name: 'Plain Person' }),
    ];
    const removeGroupMember = vi.fn(async (_id: string, uid: string) => {
      currentRoster = currentRoster.filter((m) => m.user_id !== uid);
      return { ok: true };
    });
    const { fakeApi } = renderGroupsView({
      role: 'user',
      landscape: makeOwnedGroupLandscape(),
      overrides: {
        groupMembers: vi.fn(async () => currentRoster),
        removeGroupMember,
      },
    });

    await openMembers();
    await screen.findByText('Plain Person');

    // Only "Plain Person" is removable (the owner row has no button).
    fireEvent.click(screen.getByRole('button', { name: t.groupsActionRemoveMember }));

    await waitFor(() => expect(screen.queryByText('Plain Person')).not.toBeInTheDocument());
    expect(fakeApi.removeGroupMember).toHaveBeenCalledWith('ugrp_1', 'usr_plain');
  });

  it("owner: promotes a plain member, demotes a manager, then transfers ownership to a manager -- each via that row's own action", async () => {
    const roster: GroupMember[] = [
      makeMember({ user_id: 'usr_owner', display_name: 'Owner Person', is_owner: true }),
      makeMember({ user_id: 'usr_manager', display_name: 'Manager Person', is_manager: true }),
      makeMember({ user_id: 'usr_plain', display_name: 'Plain Person' }),
    ];
    const { fakeApi } = renderGroupsView({
      role: 'user',
      landscape: makeOwnedGroupLandscape(),
      overrides: { groupMembers: vi.fn(async () => roster) },
    });

    await openMembers();
    await screen.findByText('Plain Person');
    const plainRow = rowFor('Plain Person');
    const managerRow = rowFor('Manager Person');

    // Promote: only the plain member's row offers it (not the owner's, not
    // the already-manager's).
    expect(
      within(managerRow).queryByRole('button', { name: t.groupsActionPromote }),
    ).not.toBeInTheDocument();
    fireEvent.click(within(plainRow).getByRole('button', { name: t.groupsActionPromote }));
    await waitFor(() => expect(fakeApi.promoteManager).toHaveBeenCalledWith('ugrp_1', 'usr_plain'));

    // Demote: only the current manager's row offers it (not the plain
    // member's).
    expect(
      within(plainRow).queryByRole('button', { name: t.groupsActionDemote }),
    ).not.toBeInTheDocument();
    fireEvent.click(within(managerRow).getByRole('button', { name: t.groupsActionDemote }));
    await waitFor(() =>
      expect(fakeApi.demoteManager).toHaveBeenCalledWith('ugrp_1', 'usr_manager'),
    );

    // Transfer: also only the manager's row offers it (the backend requires
    // the new owner to already be a manager); goes through the confirm
    // dialog.
    expect(
      within(plainRow).queryByRole('button', { name: t.groupsActionTransfer }),
    ).not.toBeInTheDocument();
    fireEvent.click(within(managerRow).getByRole('button', { name: t.groupsActionTransfer }));
    const confirmDialog = within(await screen.findByRole('dialog'));
    fireEvent.click(confirmDialog.getByRole('button', { name: t.groupsActionTransfer }));
    await waitFor(() =>
      expect(fakeApi.transferGroup).toHaveBeenCalledWith('ugrp_1', 'usr_manager'),
    );
  });

  it('a co-manager (not the owner) sees only Entfernen -- never Befoerdern/Degradieren/Eigentuemer wechseln', async () => {
    const roster: GroupMember[] = [
      makeMember({ user_id: 'usr_owner', display_name: 'Owner Person', is_owner: true }),
      makeMember({ user_id: 'usr_manager2', display_name: 'Other Manager', is_manager: true }),
      makeMember({ user_id: 'usr_plain', display_name: 'Plain Person' }),
    ];
    const managerLandscape: GroupLandscape = {
      system: [],
      admin: [],
      user: [
        makeGroup({
          id: 'ugrp_1',
          tier: 'user',
          name: 'Roster Group',
          owner_user_id: 'usr_owner',
          my_role: 'manager',
          can_manage: true,
          member_count: 3,
          manager_count: 2,
        }),
      ],
    };
    renderGroupsView({
      role: 'user',
      userId: 'usr_manager2',
      landscape: managerLandscape,
      overrides: { groupMembers: vi.fn(async () => roster) },
    });

    await openMembers();
    await screen.findByText('Plain Person');

    // The plain member's row offers ONLY remove -- promote is owner-only.
    const plainRow = rowFor('Plain Person');
    expect(
      within(plainRow).getByRole('button', { name: t.groupsActionRemoveMember }),
    ).toBeInTheDocument();
    expect(
      within(plainRow).queryByRole('button', { name: t.groupsActionPromote }),
    ).not.toBeInTheDocument();

    // No row offers demote/transfer either -- both are owner-only.
    expect(screen.queryByRole('button', { name: t.groupsActionDemote })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: t.groupsActionTransfer })).not.toBeInTheDocument();

    // The owner row still gets no destructive action.
    const ownerRow = rowFor('Owner Person');
    expect(
      within(ownerRow).queryByRole('button', { name: t.groupsActionRemoveMember }),
    ).not.toBeInTheDocument();
  });
});

// Per-Admin-Group co-manager permissions (spec 2026-08-10 + Phase B + Phase
// C + Resource Groups Phase 1, spec 2026-08-11): the FIVE roster-table
// permission checkboxes (Benutzer-Verwaltung / Gruppen-Änderung /
// Server-Verwaltung / Dienst-Verwaltung / Ressourcen-Verwaltung).
function permRoster(): GroupMember[] {
  return [
    makeMember({ user_id: 'usr_owner', display_name: 'Owner Person', is_owner: true }),
    makeMember({
      user_id: 'usr_manager',
      display_name: 'Manager Person',
      is_manager: true,
      can_manage_users: true,
      can_manage_group: false,
      can_manage_servers: true,
      can_manage_services: true,
      can_manage_resources: true,
    }),
    makeMember({ user_id: 'usr_plain', display_name: 'Plain Person' }),
  ];
}

describe('GroupsView per-manager permission controls', () => {
  it("owner: sees the co-manager row's five permission checkboxes as editable, and toggling one PATCHes all five flags (carrying over the untouched four)", async () => {
    const { fakeApi } = renderGroupsView({
      role: 'user',
      landscape: makeOwnedGroupLandscape(),
      overrides: { groupMembers: vi.fn(async () => permRoster()) },
    });

    await openMembers();
    await screen.findByText('Manager Person');

    const usersBox = screen.getByRole('checkbox', {
      name: `${t.groupsPermUsers} – Manager Person`,
    });
    const groupBox = screen.getByRole('checkbox', {
      name: `${t.groupsPermGroup} – Manager Person`,
    });
    const serversBox = screen.getByRole('checkbox', {
      name: `${t.groupsPermServers} – Manager Person`,
    });
    const servicesBox = screen.getByRole('checkbox', {
      name: `${t.groupsPermServices} – Manager Person`,
    });
    const resourcesBox = screen.getByRole('checkbox', {
      name: `${t.groupsPermResources} – Manager Person`,
    });
    expect(usersBox).toBeEnabled();
    expect(usersBox).toBeChecked();
    expect(groupBox).toBeEnabled();
    expect(groupBox).not.toBeChecked();
    expect(serversBox).toBeEnabled();
    expect(serversBox).toBeChecked();
    expect(servicesBox).toBeEnabled();
    expect(servicesBox).toBeChecked();
    expect(resourcesBox).toBeEnabled();
    expect(resourcesBox).toBeChecked();

    // Toggling ONLY the group-changes checkbox must still send the CURRENT
    // (unchanged) can_manage_users/can_manage_servers/can_manage_services/
    // can_manage_resources values -- the PATCH has no partial update.
    fireEvent.click(groupBox);
    await waitFor(() =>
      expect(fakeApi.setManagerPermissions).toHaveBeenCalledWith('ugrp_1', 'usr_manager', {
        canManageUsers: true,
        canManageGroup: true,
        canManageServers: true,
        canManageServices: true,
        canManageResources: true,
      }),
    );

    // The owner row (implicit full permission, not a stored flag) and a
    // plain member row (no manage relationship at all) get no real
    // checkbox -- just a dash.
    expect(within(rowFor('Owner Person')).queryAllByRole('checkbox')).toHaveLength(0);
    expect(within(rowFor('Plain Person')).queryAllByRole('checkbox')).toHaveLength(0);
  });

  // Toggling the THIRD (servers) checkbox specifically -- proves the Phase
  // B column reaches setManagerPermissions with all five flags, carrying
  // over the four untouched ones (users=true, group=false, services=true,
  // resources=true).
  it('owner: toggling the servers checkbox PATCHes all five flags, carrying over the untouched four', async () => {
    const { fakeApi } = renderGroupsView({
      role: 'user',
      landscape: makeOwnedGroupLandscape(),
      overrides: { groupMembers: vi.fn(async () => permRoster()) },
    });

    await openMembers();
    await screen.findByText('Manager Person');

    const serversBox = screen.getByRole('checkbox', {
      name: `${t.groupsPermServers} – Manager Person`,
    });
    expect(serversBox).toBeChecked();
    fireEvent.click(serversBox);
    await waitFor(() =>
      expect(fakeApi.setManagerPermissions).toHaveBeenCalledWith('ugrp_1', 'usr_manager', {
        canManageUsers: true,
        canManageGroup: false,
        canManageServers: false,
        canManageServices: true,
        canManageResources: true,
      }),
    );
  });

  // Toggling the FOURTH (services) checkbox specifically -- proves the
  // Phase C column reaches setManagerPermissions with all five flags,
  // carrying over the four untouched ones (users=true, group=false,
  // servers=true, resources=true).
  it('owner: toggling the services checkbox PATCHes all five flags, carrying over the untouched four', async () => {
    const { fakeApi } = renderGroupsView({
      role: 'user',
      landscape: makeOwnedGroupLandscape(),
      overrides: { groupMembers: vi.fn(async () => permRoster()) },
    });

    await openMembers();
    await screen.findByText('Manager Person');

    const servicesBox = screen.getByRole('checkbox', {
      name: `${t.groupsPermServices} – Manager Person`,
    });
    expect(servicesBox).toBeChecked();
    fireEvent.click(servicesBox);
    await waitFor(() =>
      expect(fakeApi.setManagerPermissions).toHaveBeenCalledWith('ugrp_1', 'usr_manager', {
        canManageUsers: true,
        canManageGroup: false,
        canManageServers: true,
        canManageServices: false,
        canManageResources: true,
      }),
    );
  });

  // Toggling the FIFTH (resources) checkbox specifically -- proves the new
  // Resource Groups Phase 1 column reaches setManagerPermissions with all
  // five flags, carrying over the four untouched ones (users=true,
  // group=false, servers=true, services=true).
  it('owner: toggling the resources checkbox PATCHes all five flags, carrying over the untouched four', async () => {
    const { fakeApi } = renderGroupsView({
      role: 'user',
      landscape: makeOwnedGroupLandscape(),
      overrides: { groupMembers: vi.fn(async () => permRoster()) },
    });

    await openMembers();
    await screen.findByText('Manager Person');

    const resourcesBox = screen.getByRole('checkbox', {
      name: `${t.groupsPermResources} – Manager Person`,
    });
    expect(resourcesBox).toBeChecked();
    fireEvent.click(resourcesBox);
    await waitFor(() =>
      expect(fakeApi.setManagerPermissions).toHaveBeenCalledWith('ugrp_1', 'usr_manager', {
        canManageUsers: true,
        canManageGroup: false,
        canManageServers: true,
        canManageServices: true,
        canManageResources: false,
      }),
    );
  });

  it('a co-manager (not the owner, not System-Admin mode) sees the same checkboxes but cannot toggle them', async () => {
    const managerLandscape: GroupLandscape = {
      system: [],
      admin: [],
      user: [
        makeGroup({
          id: 'ugrp_1',
          tier: 'user',
          name: 'Roster Group',
          owner_user_id: 'usr_owner',
          my_role: 'manager',
          can_manage: true,
          member_count: 3,
          manager_count: 2,
        }),
      ],
    };
    const { fakeApi } = renderGroupsView({
      role: 'user',
      userId: 'usr_manager2',
      landscape: managerLandscape,
      overrides: { groupMembers: vi.fn(async () => permRoster()) },
    });

    await openMembers();
    await screen.findByText('Manager Person');

    const usersBox = screen.getByRole('checkbox', {
      name: `${t.groupsPermUsers} – Manager Person`,
    });
    const groupBox = screen.getByRole('checkbox', {
      name: `${t.groupsPermGroup} – Manager Person`,
    });
    const serversBox = screen.getByRole('checkbox', {
      name: `${t.groupsPermServers} – Manager Person`,
    });
    const servicesBox = screen.getByRole('checkbox', {
      name: `${t.groupsPermServices} – Manager Person`,
    });
    const resourcesBox = screen.getByRole('checkbox', {
      name: `${t.groupsPermResources} – Manager Person`,
    });
    expect(usersBox).toBeDisabled();
    expect(usersBox).toBeChecked();
    expect(groupBox).toBeDisabled();
    expect(groupBox).not.toBeChecked();
    expect(serversBox).toBeDisabled();
    expect(serversBox).toBeChecked();
    expect(servicesBox).toBeDisabled();
    expect(servicesBox).toBeChecked();
    expect(resourcesBox).toBeDisabled();
    expect(resourcesBox).toBeChecked();

    fireEvent.click(usersBox);
    fireEvent.click(serversBox);
    fireEvent.click(servicesBox);
    fireEvent.click(resourcesBox);
    expect(fakeApi.setManagerPermissions).not.toHaveBeenCalled();
  });

  it("an ELEVATED system_admin (not the owner) may still edit a co-manager's permission checkboxes", async () => {
    const systemAdminLandscape: GroupLandscape = {
      system: [],
      admin: [
        makeGroup({
          id: 'ugrp_ag',
          tier: 'admin',
          name: 'Admin Group',
          owner_user_id: 'usr_owner',
          my_role: '',
          can_manage: true,
          member_count: 3,
          manager_count: 2,
        }),
      ],
      user: [],
    };
    const { fakeApi } = renderGroupsView({
      role: 'system_admin',
      systemAdminMode: true,
      userId: 'usr_sysadmin',
      landscape: systemAdminLandscape,
      overrides: { groupMembers: vi.fn(async () => permRoster()) },
    });

    await openMembers();
    await screen.findByText('Manager Person');

    const usersBox = screen.getByRole('checkbox', {
      name: `${t.groupsPermUsers} – Manager Person`,
    });
    expect(usersBox).toBeEnabled();
    fireEvent.click(usersBox);
    await waitFor(() =>
      expect(fakeApi.setManagerPermissions).toHaveBeenCalledWith('ugrp_ag', 'usr_manager', {
        canManageUsers: false,
        canManageGroup: false,
        canManageServers: true,
        canManageServices: true,
        canManageResources: true,
      }),
    );
  });
});
