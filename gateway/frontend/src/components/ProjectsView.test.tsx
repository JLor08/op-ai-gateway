// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ProjectsView } from './ProjectsView';
import { ToastProvider } from './shared/ToastProvider';
import { messages } from '../i18n';
import type {
  GroupLandscape,
  Project,
  ProjectGroupRef,
  ProjectMembers,
  ProjectToken,
  ProjectTokenUsageTotal,
  UserRef,
} from '../api';
import type { PortalApi } from './shared/types';

const t = messages.de;

function makeProject(overrides: Partial<Project> = {}): Project {
  return {
    id: 'proj_1',
    name: 'Test Project',
    description: '',
    owner_user_id: '',
    my_role: 'member',
    can_manage: false,
    member_count: 0,
    group_count: 0,
    ...overrides,
  };
}

function makeProjectToken(overrides: Partial<ProjectToken> = {}): ProjectToken {
  return {
    id: 'tok_1',
    name: 'Test Token',
    secret_prefix: 'sk_abc',
    owner_user_id: 'usr_2',
    owner_name: 'User B',
    status: 'active',
    created_at: '2026-08-01T00:00:00Z',
    request_count: 0,
    input_tokens: 0,
    output_tokens: 0,
    total_tokens: 0,
    ...overrides,
  };
}

function makeUsageTotal(overrides: Partial<ProjectTokenUsageTotal> = {}): ProjectTokenUsageTotal {
  return { request_count: 0, input_tokens: 0, output_tokens: 0, total_tokens: 0, ...overrides };
}

function makeAdminGroup(
  overrides: Partial<import('../api').UserGroup> = {},
): import('../api').UserGroup {
  return {
    id: 'grp_admin_1',
    tier: 'admin',
    name: 'Admin Group',
    parent_group_id: 'grp_system_1',
    owner_user_id: 'usr_1',
    owner_name: '',
    my_role: 'member',
    can_manage: false,
    can_manage_users: false,
    can_manage_servers: false,
    can_manage_services: false,
    can_manage_resources: false,
    member_count: 1,
    manager_count: 0,
    coupled_projects: [],
    ...overrides,
  };
}

function renderProjectsView(
  opts: {
    projects?: Project[];
    userId?: string;
    candidates?: { users: UserRef[]; groups: ProjectGroupRef[] };
    members?: ProjectMembers;
    tokens?: ProjectToken[];
    tokensTotal?: ProjectTokenUsageTotal;
    overrides?: Partial<PortalApi>;
  } = {},
) {
  const projects = opts.projects ?? [];
  const emptyLandscape: GroupLandscape = { system: [], admin: [], user: [] };
  const fakeApi = {
    projects: vi.fn(async () => projects),
    myProjects: vi.fn(async () => []),
    groups: vi.fn(async () => emptyLandscape),
    projectCandidates: vi.fn(async () => opts.candidates ?? { users: [], groups: [] }),
    projectMembers: vi.fn(
      async () => opts.members ?? { users: [], groups: [], transfer_candidates: [] },
    ),
    projectTokens: vi.fn(async () => ({
      tokens: opts.tokens ?? [],
      total: opts.tokensTotal ?? makeUsageTotal(),
    })),
    detachProjectToken: vi.fn(async () => ({ ok: true })),
    createProject: vi.fn(async (body: { name: string; description: string }) =>
      makeProject({ name: body.name, description: body.description }),
    ),
    renameProject: vi.fn(async (id: string, name: string, description: string) =>
      makeProject({ id, name, description }),
    ),
    transferProject: vi.fn(async () => ({ ok: true })),
    deleteProject: vi.fn(async () => ({ ok: true })),
    addProjectMembers: vi.fn(async () => ({ ok: true })),
    removeProjectMember: vi.fn(async () => ({ ok: true })),
    addProjectGroups: vi.fn(async () => ({ ok: true })),
    removeProjectGroup: vi.fn(async () => ({ ok: true })),
  };
  Object.assign(fakeApi, opts.overrides ?? {});

  render(
    <ToastProvider>
      <ProjectsView t={t} api={fakeApi} role="user" userId={opts.userId ?? 'usr_1'} />
    </ToastProvider>,
  );
  return { fakeApi };
}

afterEach(cleanup);

// SearchableSelect renders a non-native MUI Select (role="combobox"): open the
// named combobox, then click the option (options render in a portal). Mirrors
// GroupsView.test.tsx/TokenList.test.tsx's identical helper.
async function selectOption(comboName: string | RegExp, optionText: string) {
  fireEvent.mouseDown(screen.getByRole('combobox', { name: comboName }));
  fireEvent.click(await screen.findByRole('option', { name: optionText }));
}

// Scopes a query to the <tr> containing displayName -- mirrors
// GroupsView.test.tsx's identical helper, needed here because several rows
// can share the SAME row-action label (e.g. "Entfernen" on every non-coupled
// member/group row).
function rowFor(displayName: string): HTMLElement {
  return screen.getByText(displayName).closest('tr')!;
}

describe('ProjectsView list', () => {
  it('a member sees the projects they own or belong to', async () => {
    const projects = [
      makeProject({
        id: 'proj_owned',
        name: 'Owned Project',
        owner_user_id: 'usr_1',
        my_role: 'owner',
        can_manage: true,
      }),
      makeProject({
        id: 'proj_member',
        name: 'Member Project',
        owner_user_id: 'usr_2',
        my_role: 'member',
      }),
    ];
    renderProjectsView({ projects, userId: 'usr_1' });

    expect(await screen.findByText('Owned Project')).toBeInTheDocument();
    expect(screen.getByText('Member Project')).toBeInTheDocument();
  });

  it("renders a project's total_tokens in the list, defaulting to 0 when absent", async () => {
    const projects = [
      makeProject({
        id: 'proj_a',
        name: 'Alpha',
        owner_user_id: 'usr_1',
        my_role: 'owner',
        can_manage: true,
        total_tokens: 875,
      }),
      // Beta has no total_tokens at all (the field is optional -- e.g. a DTO
      // from a non-ListProjects call) and distinct nonzero member/group
      // counts, so its row-scoped "0" can only be the total_tokens cell.
      makeProject({
        id: 'proj_b',
        name: 'Beta',
        owner_user_id: 'usr_1',
        my_role: 'owner',
        can_manage: true,
        member_count: 2,
        group_count: 3,
      }),
    ];
    renderProjectsView({ projects, userId: 'usr_1' });

    const alphaRow = (await screen.findByText('Alpha')).closest('tr')!;
    const betaRow = screen.getByText('Beta').closest('tr')!;
    expect(within(alphaRow).getByText((875).toLocaleString())).toBeInTheDocument();
    expect(within(betaRow).getByText('0')).toBeInTheDocument();
  });

  it('shows a coupled indicator chip for a project coupled to a group', async () => {
    const coupled = makeProject({
      id: 'proj_c',
      name: 'Coupled Project',
      owner_user_id: 'usr_1',
      my_role: 'owner',
      can_manage: true,
      coupled_group_id: 'grp_1',
      coupled_group_name: 'Team',
    });
    renderProjectsView({ projects: [coupled], userId: 'usr_1' });

    expect(await screen.findByText('Coupled Project')).toBeInTheDocument();
    expect(screen.getByText(t.projectsCoupledChip('Team'))).toBeInTheDocument();
  });
});

describe('ProjectsView create project', () => {
  it('creates a project by calling api.createProject with the name and description', async () => {
    const { fakeApi } = renderProjectsView({ projects: [] });

    fireEvent.click(await screen.findByRole('button', { name: t.projectsCreateTitle }));
    fireEvent.change(await screen.findByLabelText(t.projectName), {
      target: { value: 'My New Project' },
    });
    fireEvent.change(screen.getByLabelText(t.projectDescription), {
      target: { value: 'Some description' },
    });
    fireEvent.click(screen.getByRole('button', { name: t.save }));

    await screen.findByRole('button', { name: t.projectsCreateTitle });
    expect(fakeApi.createProject).toHaveBeenCalledWith({
      name: 'My New Project',
      description: 'Some description',
    });
  });
});

describe('ProjectsView coupled projects', () => {
  it('creates a coupled project by selecting an owned user-group (sends coupled_group_id)', async () => {
    const { fakeApi } = renderProjectsView({
      projects: [],
      userId: 'usr_1',
      // the create form's group picker is fed from ListGroups-style data; provide
      // the caller's owned user-group via the appropriate mock (groups()/myOwnedUserGroups()).
      overrides: {
        groups: vi.fn(async () => ({
          system: [],
          admin: [],
          user: [
            {
              id: 'grp_1',
              tier: 'user' as const,
              name: 'Team',
              parent_group_id: 'grp_admin',
              owner_user_id: 'usr_1',
              owner_name: '',
              my_role: 'owner' as const,
              can_manage: true,
              can_manage_users: true,
              can_manage_servers: false,
              can_manage_services: false,
              can_manage_resources: false,
              member_count: 1,
              manager_count: 0,
              coupled_projects: [],
            },
          ],
        })),
      },
    });
    fireEvent.click(await screen.findByRole('button', { name: t.projectsCreateTitle }));
    fireEvent.change(await screen.findByLabelText(t.projectName), {
      target: { value: 'Coupled P' },
    });
    fireEvent.click(screen.getByLabelText(t.projectsCoupleToggle)); // enable coupling
    await selectOption(t.projectsCoupleSelectLabel, 'Team'); // pick the group
    fireEvent.click(screen.getByRole('button', { name: t.save }));
    await screen.findByRole('button', { name: t.projectsCreateTitle });
    expect(fakeApi.createProject).toHaveBeenCalledWith(
      expect.objectContaining({ name: 'Coupled P', coupled_group_id: 'grp_1' }),
    );
  });

  it("a coupled project's members sub-view is read-only (no add/transfer, no row actions on either table)", async () => {
    const coupled = makeProject({
      id: 'proj_c',
      name: 'C',
      owner_user_id: 'usr_1',
      my_role: 'owner',
      can_manage: true,
      coupled_group_id: 'grp_1',
      coupled_group_name: 'Team',
    });
    renderProjectsView({
      projects: [coupled],
      userId: 'usr_1',
      members: {
        users: [],
        groups: [{ id: 'grp_1', name: 'Team' }],
        transfer_candidates: [
          { id: 'usr_1', email: 'a@example.com', display_name: 'Owner Person' },
        ],
      },
    });
    fireEvent.click(await screen.findByRole('button', { name: t.projectsActionMembers }));
    await screen.findByRole('heading', { name: t.projectsMembersTitle('C') });
    // No add-member button, no transfer heading -- coupling drives read-only.
    expect(screen.queryByRole('button', { name: t.projectsActionAdd })).not.toBeInTheDocument();
    expect(screen.queryByText(t.projectsTransferLabel)).not.toBeInTheDocument();
    // The coupled group's resolved member and the coupled group itself are
    // shown, but NEITHER table renders any row action (a coupled roster is
    // fully read-only -- even the owner-of-record gets no action).
    expect(await screen.findByText('Team')).toBeInTheDocument();
    expect(await screen.findByText('Owner Person')).toBeInTheDocument();
    expect(
      screen.queryByRole('button', { name: t.projectsActionTransfer }),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByRole('button', { name: t.projectsActionRemoveMember }),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByRole('button', { name: t.projectsActionRemoveGroup }),
    ).not.toBeInTheDocument();
  });

  it('create-a-new-group mode with 0 admin groups disables Save and shows the parent-unavailable warning', async () => {
    renderProjectsView({
      projects: [],
      userId: 'usr_1',
      overrides: { groups: vi.fn(async () => ({ system: [], admin: [], user: [] })) },
    });
    fireEvent.click(await screen.findByRole('button', { name: t.projectsCreateTitle }));
    fireEvent.change(await screen.findByLabelText(t.projectName), {
      target: { value: 'Coupled P' },
    });
    fireEvent.click(screen.getByLabelText(t.projectsCoupleToggle)); // enable coupling
    fireEvent.click(await screen.findByRole('radio', { name: t.projectsCoupleModeCreate })); // switch to "create new group"
    fireEvent.change(await screen.findByLabelText(t.projectsCoupleNewName), {
      target: { value: 'New Group' },
    });

    expect(await screen.findByText(t.groupsNoAdminGroupHint)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: t.save })).toBeDisabled();
  });

  it('create-a-new-group mode with >1 admin groups disables Save until a parent is picked, then submits parent_group_id', async () => {
    const { fakeApi } = renderProjectsView({
      projects: [],
      userId: 'usr_1',
      overrides: {
        groups: vi.fn(async () => ({
          system: [],
          admin: [
            makeAdminGroup({ id: 'grp_a', name: 'Admin A' }),
            makeAdminGroup({ id: 'grp_b', name: 'Admin B' }),
          ],
          user: [],
        })),
      },
    });
    fireEvent.click(await screen.findByRole('button', { name: t.projectsCreateTitle }));
    fireEvent.change(await screen.findByLabelText(t.projectName), {
      target: { value: 'Coupled P' },
    });
    fireEvent.click(screen.getByLabelText(t.projectsCoupleToggle)); // enable coupling
    fireEvent.click(await screen.findByRole('radio', { name: t.projectsCoupleModeCreate })); // switch to "create new group"
    fireEvent.change(await screen.findByLabelText(t.projectsCoupleNewName), {
      target: { value: 'New Group' },
    });

    // Two admin groups, none picked yet -> the parent picker shows, Save disabled.
    await screen.findByRole('combobox', { name: t.groupsParentLabel });
    expect(screen.getByRole('button', { name: t.save })).toBeDisabled();

    await selectOption(t.groupsParentLabel, 'Admin B');
    expect(screen.getByRole('button', { name: t.save })).not.toBeDisabled();

    fireEvent.click(screen.getByRole('button', { name: t.save }));
    await screen.findByRole('button', { name: t.projectsCreateTitle });
    expect(fakeApi.createProject).toHaveBeenCalledWith(
      expect.objectContaining({
        name: 'Coupled P',
        create_coupled_group: { name: 'New Group', parent_group_id: 'grp_b' },
      }),
    );
  });
});

describe('ProjectsView non-owner row actions', () => {
  it('a plain member (can_manage false) sees no rename/members/tokens/delete row actions', async () => {
    const projects = [
      makeProject({
        id: 'proj_member',
        name: 'Member Project',
        owner_user_id: 'usr_2',
        my_role: 'member',
        can_manage: false,
      }),
    ];
    renderProjectsView({ projects, userId: 'usr_1' });

    expect(await screen.findByText('Member Project')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: t.projectsActionRename })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: t.projectsActionMembers })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: t.projectsActionTokens })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: t.projectsActionDelete })).not.toBeInTheDocument();
  });

  it("an admin managing another user's project (can_manage true, my_role member) sees manage actions (incl. members + tokens) but the transfer control never appears at the list level", async () => {
    const projects = [
      makeProject({
        id: 'proj_other',
        name: 'Other Owner Project',
        owner_user_id: 'usr_2',
        my_role: 'member',
        can_manage: true,
      }),
    ];
    renderProjectsView({ projects, userId: 'usr_1' });

    expect(
      await screen.findByRole('button', { name: t.projectsActionMembers }),
    ).toBeInTheDocument();
    expect(screen.getByRole('button', { name: t.projectsActionTokens })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: t.projectsActionDelete })).toBeInTheDocument();
    // Transfer only ever shows inside the members sub-view, and only for the
    // actual owner (my_role === "owner") -- never as a list-level row action.
    expect(
      screen.queryByRole('button', { name: t.projectsActionTransfer }),
    ).not.toBeInTheDocument();
  });
});

describe('ProjectsView members sub-view row actions', () => {
  it('an owner sees Eigentuemer wechseln + Entfernen on a non-owner member row', async () => {
    const owned = makeProject({
      id: 'proj_owned',
      name: 'Owned Project',
      owner_user_id: 'usr_1',
      my_role: 'owner',
      can_manage: true,
    });
    renderProjectsView({
      projects: [owned],
      userId: 'usr_1',
      members: {
        users: [{ id: 'usr_2', email: 'b@example.com', display_name: 'User B' }],
        groups: [],
        transfer_candidates: [{ id: 'usr_2', email: 'b@example.com', display_name: 'User B' }],
      },
    });

    fireEvent.click(await screen.findByRole('button', { name: t.projectsActionMembers }));
    await screen.findByRole('heading', { name: t.projectsMembersTitle('Owned Project') });
    await screen.findByText('User B');
    const memberRow = rowFor('User B');
    expect(
      within(memberRow).getByRole('button', { name: t.projectsActionTransfer }),
    ).toBeInTheDocument();
    expect(
      within(memberRow).getByRole('button', { name: t.projectsActionRemoveMember }),
    ).toBeInTheDocument();
  });

  // The members table renders the EFFECTIVE member set (transfer_candidates
  // = direct-users UNION group-resolved members), for BOTH coupled and
  // non-coupled projects, so a group-resolved (non-direct) member DOES get a
  // row and CAN be offered as an "Eigentuemer wechseln" target -- restoring
  // the 2026-08-09 "transfer_candidates" fix (TransferProject accepts any
  // isProjectMember, including a group-only member) that a naive
  // roster.users-only table would have regressed. Entfernen, in contrast, is
  // scoped to DIRECT members only (a group-resolved member has no
  // project_members row to remove -- you'd remove the group instead).
  it("a group-resolved (non-direct) member's row offers Eigentuemer wechseln but not Entfernen; a direct member offers both", async () => {
    const owned = makeProject({
      id: 'proj_owned',
      name: 'Owned Project',
      owner_user_id: 'usr_1',
      my_role: 'owner',
      can_manage: true,
    });
    renderProjectsView({
      projects: [owned],
      userId: 'usr_1',
      members: {
        users: [{ id: 'usr_direct', email: 'd@example.com', display_name: 'Direct Person' }],
        groups: [{ id: 'grp_1', name: 'Team A' }],
        transfer_candidates: [
          { id: 'usr_direct', email: 'd@example.com', display_name: 'Direct Person' },
          { id: 'usr_g', email: 'g@example.com', display_name: 'Group Person' },
        ],
      },
    });

    fireEvent.click(await screen.findByRole('button', { name: t.projectsActionMembers }));
    await screen.findByRole('heading', { name: t.projectsMembersTitle('Owned Project') });
    await screen.findByText('Group Person');

    const directRow = rowFor('Direct Person');
    expect(
      within(directRow).getByRole('button', { name: t.projectsActionTransfer }),
    ).toBeInTheDocument();
    expect(
      within(directRow).getByRole('button', { name: t.projectsActionRemoveMember }),
    ).toBeInTheDocument();

    const groupRow = rowFor('Group Person');
    expect(
      within(groupRow).getByRole('button', { name: t.projectsActionTransfer }),
    ).toBeInTheDocument();
    expect(
      within(groupRow).queryByRole('button', { name: t.projectsActionRemoveMember }),
    ).not.toBeInTheDocument();
  });

  it("transfers ownership to a group-resolved (non-direct) member via its row action, calling api.transferProject with that member's id", async () => {
    const owned = makeProject({
      id: 'proj_owned',
      name: 'Owned Project',
      owner_user_id: 'usr_1',
      my_role: 'owner',
      can_manage: true,
    });
    const { fakeApi } = renderProjectsView({
      projects: [owned],
      userId: 'usr_1',
      // No DIRECT members, one assigned group; the group-resolved member is a
      // valid TransferProject target and must be reachable via its row's
      // "Eigentuemer wechseln" action even though it has no direct-member row.
      members: {
        users: [],
        groups: [{ id: 'grp_1', name: 'Team A' }],
        transfer_candidates: [
          { id: 'usr_g', email: 'g@example.com', display_name: 'Group Person' },
        ],
      },
    });

    fireEvent.click(await screen.findByRole('button', { name: t.projectsActionMembers }));
    await screen.findByRole('heading', { name: t.projectsMembersTitle('Owned Project') });
    await screen.findByText('Group Person');
    const groupRow = rowFor('Group Person');
    fireEvent.click(within(groupRow).getByRole('button', { name: t.projectsActionTransfer }));
    const confirmDialog = within(await screen.findByRole('dialog'));
    fireEvent.click(confirmDialog.getByRole('button', { name: t.projectsActionTransfer }));
    await waitFor(() =>
      expect(fakeApi.transferProject).toHaveBeenCalledWith('proj_owned', 'usr_g'),
    );
  });

  it('a can_manage admin who is not the owner sees Entfernen but never Eigentuemer wechseln, anywhere in the table', async () => {
    const other = makeProject({
      id: 'proj_other',
      name: 'Other Owner Project',
      owner_user_id: 'usr_2',
      my_role: 'member',
      can_manage: true,
    });
    renderProjectsView({
      projects: [other],
      userId: 'usr_1',
      members: {
        users: [{ id: 'usr_3', email: 'c@example.com', display_name: 'User C' }],
        groups: [],
        transfer_candidates: [
          { id: 'usr_2', email: 'b@example.com', display_name: 'User B' },
          { id: 'usr_3', email: 'c@example.com', display_name: 'User C' },
        ],
      },
    });

    fireEvent.click(await screen.findByRole('button', { name: t.projectsActionMembers }));
    await screen.findByRole('heading', { name: t.projectsMembersTitle('Other Owner Project') });
    await screen.findByText('User C');
    const memberRow = rowFor('User C');
    expect(
      within(memberRow).getByRole('button', { name: t.projectsActionRemoveMember }),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole('button', { name: t.projectsActionTransfer }),
    ).not.toBeInTheDocument();
    // User B is group-resolved-only (in transfer_candidates, not in the
    // direct `users` list) -- its row shows, but with NO remove action
    // either (not a direct member; a non-owner viewer has no transfer
    // regardless).
    const groupOnlyRow = rowFor('User B');
    expect(
      within(groupOnlyRow).queryByRole('button', { name: t.projectsActionRemoveMember }),
    ).not.toBeInTheDocument();
  });

  it("never offers Eigentuemer wechseln on the owner's own row when the owner is listed as a direct member", async () => {
    const owned = makeProject({
      id: 'proj_owned',
      name: 'Owned Project',
      owner_user_id: 'usr_1',
      my_role: 'owner',
      can_manage: true,
    });
    renderProjectsView({
      projects: [owned],
      userId: 'usr_1',
      members: {
        users: [{ id: 'usr_1', email: 'a@example.com', display_name: 'Owner Person' }],
        groups: [],
        transfer_candidates: [
          { id: 'usr_1', email: 'a@example.com', display_name: 'Owner Person' },
        ],
      },
    });

    fireEvent.click(await screen.findByRole('button', { name: t.projectsActionMembers }));
    await screen.findByRole('heading', { name: t.projectsMembersTitle('Owned Project') });
    await screen.findByText('Owner Person');
    const ownerRow = rowFor('Owner Person');
    expect(
      within(ownerRow).queryByRole('button', { name: t.projectsActionTransfer }),
    ).not.toBeInTheDocument();
    // Remove is still offered on every non-coupled row -- projects have no
    // manager tier, so unlike GroupsView's roster this is unconditional.
    expect(
      within(ownerRow).getByRole('button', { name: t.projectsActionRemoveMember }),
    ).toBeInTheDocument();
  });

  it("removes a group via its row's remove action, calling api.removeProjectGroup with that row's id", async () => {
    const owned = makeProject({
      id: 'proj_owned',
      name: 'Owned Project',
      owner_user_id: 'usr_1',
      my_role: 'owner',
      can_manage: true,
    });
    const { fakeApi } = renderProjectsView({
      projects: [owned],
      userId: 'usr_1',
      members: { users: [], groups: [{ id: 'grp_1', name: 'Team A' }], transfer_candidates: [] },
    });

    fireEvent.click(await screen.findByRole('button', { name: t.projectsActionMembers }));
    await screen.findByRole('heading', { name: t.projectsMembersTitle('Owned Project') });
    await screen.findByText('Team A');
    const groupRow = rowFor('Team A');
    fireEvent.click(within(groupRow).getByRole('button', { name: t.projectsActionRemoveGroup }));
    await waitFor(() =>
      expect(fakeApi.removeProjectGroup).toHaveBeenCalledWith('proj_owned', 'grp_1'),
    );
  });

  it("removes a member via its row's remove action, calling api.removeProjectMember with that row's id", async () => {
    const owned = makeProject({
      id: 'proj_owned',
      name: 'Owned Project',
      owner_user_id: 'usr_1',
      my_role: 'owner',
      can_manage: true,
    });
    const { fakeApi } = renderProjectsView({
      projects: [owned],
      userId: 'usr_1',
      members: {
        users: [{ id: 'usr_2', email: 'b@example.com', display_name: 'User B' }],
        groups: [],
        transfer_candidates: [{ id: 'usr_2', email: 'b@example.com', display_name: 'User B' }],
      },
    });

    fireEvent.click(await screen.findByRole('button', { name: t.projectsActionMembers }));
    await screen.findByRole('heading', { name: t.projectsMembersTitle('Owned Project') });
    await screen.findByText('User B');
    const memberRow = rowFor('User B');
    fireEvent.click(within(memberRow).getByRole('button', { name: t.projectsActionRemoveMember }));
    await waitFor(() =>
      expect(fakeApi.removeProjectMember).toHaveBeenCalledWith('proj_owned', 'usr_2'),
    );
  });

  it("transfer confirm targets the clicked row's member id, going through the confirm dialog", async () => {
    const owned = makeProject({
      id: 'proj_owned',
      name: 'Owned Project',
      owner_user_id: 'usr_1',
      my_role: 'owner',
      can_manage: true,
    });
    const { fakeApi } = renderProjectsView({
      projects: [owned],
      userId: 'usr_1',
      members: {
        users: [
          { id: 'usr_2', email: 'b@example.com', display_name: 'User B' },
          { id: 'usr_3', email: 'c@example.com', display_name: 'User C' },
        ],
        groups: [],
        transfer_candidates: [
          { id: 'usr_2', email: 'b@example.com', display_name: 'User B' },
          { id: 'usr_3', email: 'c@example.com', display_name: 'User C' },
        ],
      },
    });

    fireEvent.click(await screen.findByRole('button', { name: t.projectsActionMembers }));
    await screen.findByRole('heading', { name: t.projectsMembersTitle('Owned Project') });
    await screen.findByText('User B');
    const rowB = rowFor('User B');
    fireEvent.click(within(rowB).getByRole('button', { name: t.projectsActionTransfer }));
    const confirmDialog = within(await screen.findByRole('dialog'));
    fireEvent.click(confirmDialog.getByRole('button', { name: t.projectsActionTransfer }));
    await waitFor(() =>
      expect(fakeApi.transferProject).toHaveBeenCalledWith('proj_owned', 'usr_2'),
    );
  });
});

describe('ProjectsView members vs. tokens sub-view split', () => {
  it('opening the members action shows no tokens content and does not call api.projectTokens', async () => {
    const owned = makeProject({
      id: 'proj_owned',
      name: 'Owned Project',
      owner_user_id: 'usr_1',
      my_role: 'owner',
      can_manage: true,
    });
    const { fakeApi } = renderProjectsView({
      projects: [owned],
      userId: 'usr_1',
      tokens: [makeProjectToken({ id: 'tok_1', name: 'Agent Token', owner_name: 'User B' })],
    });

    fireEvent.click(await screen.findByRole('button', { name: t.projectsActionMembers }));
    await screen.findByRole('heading', { name: t.projectsMembersTitle('Owned Project') });
    // The members sub-view no longer renders the tokens section at all.
    expect(screen.queryByText(t.projectsTokensLabel)).not.toBeInTheDocument();
    expect(screen.queryByText('Agent Token')).not.toBeInTheDocument();
    expect(fakeApi.projectTokens).not.toHaveBeenCalled();
  });

  it('opening the tokens action shows no member/group management content and does not call api.projectMembers', async () => {
    const owned = makeProject({
      id: 'proj_owned',
      name: 'Owned Project',
      owner_user_id: 'usr_1',
      my_role: 'owner',
      can_manage: true,
    });
    const { fakeApi } = renderProjectsView({
      projects: [owned],
      userId: 'usr_1',
      tokens: [makeProjectToken({ id: 'tok_1', name: 'Agent Token', owner_name: 'User B' })],
    });

    fireEvent.click(await screen.findByRole('button', { name: t.projectsActionTokens }));
    await screen.findByRole('heading', { name: t.projectsTokensTitle('Owned Project') });
    // The tokens sub-view no longer renders member/group management or transfer.
    expect(screen.queryByText(t.projectsAddMembersLabel)).not.toBeInTheDocument();
    expect(screen.queryByText(t.projectsAddGroupsLabel)).not.toBeInTheDocument();
    expect(screen.queryByText(t.projectsTransferLabel)).not.toBeInTheDocument();
    expect(fakeApi.projectMembers).not.toHaveBeenCalled();
    // The token content is present here instead.
    expect(await screen.findByText('Agent Token')).toBeInTheDocument();
  });
});

describe('ProjectsView tokens sub-view assigned tokens', () => {
  it("lists a project's assigned tokens (name + owner) via api.projectTokens", async () => {
    const owned = makeProject({
      id: 'proj_owned',
      name: 'Owned Project',
      owner_user_id: 'usr_1',
      my_role: 'owner',
      can_manage: true,
    });
    const { fakeApi } = renderProjectsView({
      projects: [owned],
      userId: 'usr_1',
      tokens: [makeProjectToken({ id: 'tok_1', name: 'Agent Token', owner_name: 'User B' })],
    });

    fireEvent.click(await screen.findByRole('button', { name: t.projectsActionTokens }));
    await screen.findByRole('heading', { name: t.projectsTokensTitle('Owned Project') });
    expect(await screen.findByText('Agent Token')).toBeInTheDocument();
    expect(screen.getByText('User B')).toBeInTheDocument();
    expect(fakeApi.projectTokens).toHaveBeenCalledWith('proj_owned');
  });

  it('falls back to owner_user_id when owner_name is empty', async () => {
    const owned = makeProject({
      id: 'proj_owned',
      name: 'Owned Project',
      owner_user_id: 'usr_1',
      my_role: 'owner',
      can_manage: true,
    });
    renderProjectsView({
      projects: [owned],
      userId: 'usr_1',
      tokens: [
        makeProjectToken({
          id: 'tok_1',
          name: 'Agent Token',
          owner_name: '',
          owner_user_id: 'usr_ghost',
        }),
      ],
    });

    fireEvent.click(await screen.findByRole('button', { name: t.projectsActionTokens }));
    await screen.findByRole('heading', { name: t.projectsTokensTitle('Owned Project') });
    expect(await screen.findByText('usr_ghost')).toBeInTheDocument();
  });

  it("renders each token's own usage numbers (requests, prompt, generated, total)", async () => {
    const owned = makeProject({
      id: 'proj_owned',
      name: 'Owned Project',
      owner_user_id: 'usr_1',
      my_role: 'owner',
      can_manage: true,
    });
    renderProjectsView({
      projects: [owned],
      userId: 'usr_1',
      tokens: [
        makeProjectToken({
          id: 'tok_1',
          name: 'Agent Token',
          request_count: 12,
          input_tokens: 340,
          output_tokens: 210,
          total_tokens: 550,
        }),
      ],
      tokensTotal: makeUsageTotal({
        request_count: 12,
        input_tokens: 340,
        output_tokens: 210,
        total_tokens: 550,
      }),
    });

    fireEvent.click(await screen.findByRole('button', { name: t.projectsActionTokens }));
    await screen.findByRole('heading', { name: t.projectsTokensTitle('Owned Project') });
    await screen.findByText('Agent Token');

    // The per-token row's own usage numbers render as plain table cells.
    expect(screen.getByText('12')).toBeInTheDocument();
    expect(screen.getByText('340')).toBeInTheDocument();
    expect(screen.getByText('210')).toBeInTheDocument();
    expect(screen.getByText('550')).toBeInTheDocument();
  });

  it('shows the project TOTAL line, distinct from (and able to exceed) the sum of the visible rows', async () => {
    const owned = makeProject({
      id: 'proj_owned',
      name: 'Owned Project',
      owner_user_id: 'usr_1',
      my_role: 'owner',
      can_manage: true,
    });
    renderProjectsView({
      projects: [owned],
      userId: 'usr_1',
      // The single visible row sums to 550 tokens / 12 requests, but the
      // project's TRUE total is far higher -- usage from a token that has
      // since been detached is still counted in `total`, never in the rows.
      tokens: [
        makeProjectToken({
          id: 'tok_1',
          name: 'Agent Token',
          request_count: 12,
          input_tokens: 340,
          output_tokens: 210,
          total_tokens: 550,
        }),
      ],
      tokensTotal: makeUsageTotal({
        request_count: 999,
        input_tokens: 5000,
        output_tokens: 4000,
        total_tokens: 9000,
      }),
    });

    fireEvent.click(await screen.findByRole('button', { name: t.projectsActionTokens }));
    await screen.findByRole('heading', { name: t.projectsTokensTitle('Owned Project') });
    await screen.findByText('Agent Token');

    expect(await screen.findByText(t.projectsTokensTotalLabel)).toBeInTheDocument();
    expect(screen.getByText(`${t.projectsTokensColRequests}: 999`)).toBeInTheDocument();
    expect(screen.getByText(`${t.projectsTokensColPrompt}: 5,000`)).toBeInTheDocument();
    expect(screen.getByText(`${t.projectsTokensColGenerated}: 4,000`)).toBeInTheDocument();
    expect(screen.getByText(`${t.projectsTokensColTotal}: 9,000`)).toBeInTheDocument();
  });

  it('shows the empty-state label when no tokens are assigned', async () => {
    const owned = makeProject({
      id: 'proj_owned',
      name: 'Owned Project',
      owner_user_id: 'usr_1',
      my_role: 'owner',
      can_manage: true,
    });
    renderProjectsView({ projects: [owned], userId: 'usr_1', tokens: [] });

    fireEvent.click(await screen.findByRole('button', { name: t.projectsActionTokens }));
    await screen.findByRole('heading', { name: t.projectsTokensTitle('Owned Project') });
    expect(await screen.findByText(t.projectsTokensEmpty)).toBeInTheDocument();
  });

  it('detaches a token after confirm, calling api.detachProjectToken and refreshing the list', async () => {
    const owned = makeProject({
      id: 'proj_owned',
      name: 'Owned Project',
      owner_user_id: 'usr_1',
      my_role: 'owner',
      can_manage: true,
    });
    const detachedToken = makeProjectToken({ id: 'tok_1', name: 'Agent Token' });
    let call = 0;
    const { fakeApi } = renderProjectsView({
      projects: [owned],
      userId: 'usr_1',
      overrides: {
        projectTokens: vi.fn(async () => {
          call += 1;
          return { tokens: call === 1 ? [detachedToken] : [], total: makeUsageTotal() };
        }) as unknown as PortalApi['projectTokens'],
      },
    });

    fireEvent.click(await screen.findByRole('button', { name: t.projectsActionTokens }));
    await screen.findByRole('heading', { name: t.projectsTokensTitle('Owned Project') });
    expect(await screen.findByText('Agent Token')).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: t.projectsTokenDetach }));
    const confirmDialog = within(await screen.findByRole('dialog'));
    expect(confirmDialog.getByText(t.projectsTokenDetachConfirmTitle)).toBeInTheDocument();
    fireEvent.click(confirmDialog.getByRole('button', { name: t.projectsTokenDetach }));

    await waitFor(() =>
      expect(fakeApi.detachProjectToken).toHaveBeenCalledWith('proj_owned', 'tok_1'),
    );
    expect(await screen.findByText(t.projectsTokensEmpty)).toBeInTheDocument();
    expect(fakeApi.projectTokens).toHaveBeenCalledTimes(2);
  });
});
