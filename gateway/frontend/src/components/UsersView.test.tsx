// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { UsersView } from './UsersView';
import { ToastProvider } from './shared/ToastProvider';
import { messages } from '../i18n';
import { EMPTY_LIMIT_CONFIG } from '../api';
import type {
  AdminUser,
  CreateUserRequest,
  GroupLandscape,
  InviteResponse,
  LimitConfig,
  UserGroup,
} from '../api';
import type { PortalApi } from './shared/types';

const t = messages.de;

afterEach(cleanup);

function renderUsersView(
  overrides?: Partial<
    Pick<
      PortalApi,
      | 'adminResetTotp'
      | 'adminUsers'
      | 'createUser'
      | 'groups'
      | 'reinviteUser'
      | 'setUserLimits'
      | 'updateUser'
      | 'userLimits'
    >
  >,
  canAssignSystemAdmin = false,
) {
  const fakeApi = {
    adminUsers: vi.fn(async () => ({ data: [] })),
    createUser: vi.fn(
      async () => ({ invite_url: 'https://gw.example/invite/abc123' }) as InviteResponse,
    ),
    // Default: exactly one manageable admin group -> auto-selected, no
    // control shown, submit never blocked. The admin-group picker is
    // mandatory for every actor (spec:
    // 2026-08-09-group-visibility-admin-group-invite-design.md), so every
    // pre-existing test that doesn't care about the picker needs this
    // default to keep its invite flow unblocked; tests that specifically
    // exercise the picker pass their own `groups` override explicitly.
    groups: vi.fn(async (): Promise<GroupLandscape> => ({
      system: [],
      admin: [
        makeGroup({
          id: 'grp-default-admin',
          tier: 'admin',
          name: 'Default Admin Group',
          owner_user_id: 'usr-owner',
          can_manage: true,
          // can_manage_users (spec 2026-08-10): the invite-picker's actual
          // filter source, distinct from can_manage (group STRUCTURE).
          can_manage_users: true,
          member_count: 1,
        }),
      ],
      user: [],
    })),
    updateUser: vi.fn(async () => ({}) as AdminUser),
    reinviteUser: vi.fn(
      async () => ({ invite_url: 'https://gw.example/invite/reinvite' }) as InviteResponse,
    ),
    adminResetTotp: vi.fn(async () => ({ ok: true })),
    userLimits: vi.fn(async () => ({
      limits: EMPTY_LIMIT_CONFIG,
      usage: { requests_this_period: 0, tokens_this_period: 0, cost_this_period: 0 },
    })),
    setUserLimits: vi.fn(async () => ({
      limits: EMPTY_LIMIT_CONFIG,
      usage: { requests_this_period: 0, tokens_this_period: 0, cost_this_period: 0 },
    })),
    ...overrides,
  };
  render(
    <ToastProvider>
      <UsersView t={t} api={fakeApi} canAssignSystemAdmin={canAssignSystemAdmin} />
    </ToastProvider>,
  );
  return { fakeApi };
}

describe('UsersView invite dialog', () => {
  it('widens the invite modal (md) and shows a copy button wired to the invite url', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.assign(navigator, { clipboard: { writeText } });
    renderUsersView();

    // Open the create sub-view, fill the email, submit → invite modal opens.
    // The default single manageable admin group auto-selects once the async
    // api.groups() load resolves, so the submit button starts disabled and
    // must be awaited enabled before the final click.
    fireEvent.click(screen.getByRole('button', { name: t.userCreate }));
    fireEvent.change(screen.getByLabelText(t.tableEmail), { target: { value: 'new@example.com' } });
    await waitFor(() => expect(screen.getByRole('button', { name: t.userCreate })).toBeEnabled());
    fireEvent.click(screen.getByRole('button', { name: t.userCreate }));

    const dialog = await screen.findByRole('dialog');
    expect(dialog).toHaveClass('MuiDialog-paperWidthMd');
    expect(within(dialog).getByText('https://gw.example/invite/abc123')).toBeInTheDocument();

    fireEvent.click(within(dialog).getByRole('button', { name: t.userInviteCopy }));
    expect(writeText).toHaveBeenCalledWith('https://gw.example/invite/abc123');
  });

  it('shows an email-sent status line when the backend emailed the link', async () => {
    renderUsersView({
      createUser: vi.fn(async () => ({
        user: { email: 'new@example.com' },
        invite_url: 'https://gw.example/invite/abc123',
        email_sent: true,
        email_error: '',
      })) as unknown as PortalApi['createUser'],
    });
    fireEvent.click(screen.getByRole('button', { name: t.userCreate }));
    fireEvent.change(screen.getByLabelText(t.tableEmail), { target: { value: 'new@example.com' } });
    await waitFor(() => expect(screen.getByRole('button', { name: t.userCreate })).toBeEnabled());
    fireEvent.click(screen.getByRole('button', { name: t.userCreate }));
    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByText(t.userInviteEmailSent('new@example.com'))).toBeInTheDocument();
  });

  it('shows the email error as a warning while still showing the link', async () => {
    renderUsersView({
      createUser: vi.fn(async () => ({
        user: { email: 'new@example.com' },
        invite_url: 'https://gw.example/invite/abc123',
        email_sent: false,
        email_error: 'dial tcp: timeout',
      })) as unknown as PortalApi['createUser'],
    });
    fireEvent.click(screen.getByRole('button', { name: t.userCreate }));
    fireEvent.change(screen.getByLabelText(t.tableEmail), { target: { value: 'new@example.com' } });
    await waitFor(() => expect(screen.getByRole('button', { name: t.userCreate })).toBeEnabled());
    fireEvent.click(screen.getByRole('button', { name: t.userCreate }));
    const dialog = await screen.findByRole('dialog');
    expect(
      within(dialog).getByText(t.userInviteEmailFailed('dial tcp: timeout')),
    ).toBeInTheDocument();
    expect(within(dialog).getByText('https://gw.example/invite/abc123')).toBeInTheDocument();
  });
});

describe('UsersView admin TOTP reset', () => {
  const enrolledUser = {
    id: 'user-1',
    email: 'enrolled@example.com',
    display_name: 'Enrolled User',
    role: 'user',
    status: 'active',
    preferred_language: 'de',
    created_at: '2026-01-01T00:00:00Z',
    totp_enabled: true,
  };

  const unenrolledUser = {
    id: 'user-2',
    email: 'plain@example.com',
    display_name: 'Plain User',
    role: 'user',
    status: 'active',
    preferred_language: 'de',
    created_at: '2026-01-01T00:00:00Z',
    totp_enabled: false,
  };

  it('resets TOTP after confirmation and only for enrolled users', async () => {
    const adminResetTotp = vi.fn(async () => ({ ok: true }));
    renderUsersView({
      adminUsers: vi.fn(async () => ({ data: [enrolledUser] })),
      adminResetTotp: adminResetTotp as unknown as PortalApi['adminResetTotp'],
    });

    // The Users row actions render inline icon buttons (maxInlineActions=5,
    // ListTable's RowActionsCell) rather than a kebab menu, so the reset
    // action is a plain labelled button — not behind t.listRowMenu/menuitem.
    fireEvent.click(await screen.findByRole('button', { name: t.userActionResetTotp }));
    fireEvent.click(await screen.findByRole('button', { name: t.userResetTotpConfirm }));

    await waitFor(() => expect(adminResetTotp).toHaveBeenCalledWith(enrolledUser.id));
  });

  it('does not offer the reset action for a user without TOTP enrolled', async () => {
    renderUsersView({
      adminUsers: vi.fn(async () => ({ data: [unenrolledUser] })),
    });

    expect(await screen.findByText(unenrolledUser.email)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: t.userActionResetTotp })).toBeNull();
  });
});

describe('UsersView system_admin row actions', () => {
  const sysAdmin = {
    id: 'sa-1',
    email: 'sys@example.com',
    display_name: 'Sys Admin',
    role: 'system_admin',
    status: 'active',
    preferred_language: 'de',
    created_at: '2026-01-01T00:00:00Z',
    totp_enabled: true,
  };

  it('disables Edit + Deactivate on a system_admin for a non-elevated caller, keeps support ops enabled', async () => {
    renderUsersView({ adminUsers: vi.fn(async () => ({ data: [sysAdmin] })) }, false);
    await screen.findByText(sysAdmin.email);
    // Edit + Deactivate (both route through UpdateUser -> ErrForbiddenRole) are not clickable.
    expect(screen.getByRole('button', { name: t.userActionEdit })).toBeDisabled();
    expect(screen.getByRole('button', { name: t.userActionDisable })).toBeDisabled();
    // The allowed support ops stay clickable.
    expect(screen.getByRole('button', { name: t.userActionLimits })).toBeEnabled();
    expect(screen.getByRole('button', { name: t.userActionReinvite })).toBeEnabled();
    expect(screen.getByRole('button', { name: t.userActionResetTotp })).toBeEnabled();
  });

  it('enables Edit + Deactivate on a system_admin for an elevated (system-admin-mode) caller', async () => {
    renderUsersView({ adminUsers: vi.fn(async () => ({ data: [sysAdmin] })) }, true);
    await screen.findByText(sysAdmin.email);
    expect(screen.getByRole('button', { name: t.userActionEdit })).toBeEnabled();
    expect(screen.getByRole('button', { name: t.userActionDisable })).toBeEnabled();
  });

  it('does not lock Edit/Deactivate on a non-system_admin user for a non-elevated caller', async () => {
    const plainAdmin = { ...sysAdmin, id: 'a-9', email: 'admin9@example.com', role: 'admin' };
    renderUsersView({ adminUsers: vi.fn(async () => ({ data: [plainAdmin] })) }, false);
    await screen.findByText(plainAdmin.email);
    expect(screen.getByRole('button', { name: t.userActionEdit })).toBeEnabled();
    expect(screen.getByRole('button', { name: t.userActionDisable })).toBeEnabled();
  });
});

describe('UsersView admin limits editor', () => {
  const plainUser = {
    id: 'user-3',
    email: 'limits@example.com',
    display_name: 'Limits User',
    role: 'user',
    status: 'active',
    preferred_language: 'de',
    created_at: '2026-01-01T00:00:00Z',
    totp_enabled: false,
  };

  it("loads a user's stored limits + usage into the dialog on open", async () => {
    const userLimits = vi.fn(async () => ({
      limits: {
        ...EMPTY_LIMIT_CONFIG,
        request_quota: 10000,
        request_quota_period: 'day',
      } as LimitConfig,
      usage: { requests_this_period: 8000, tokens_this_period: 0, cost_this_period: 0 },
    }));
    renderUsersView({
      adminUsers: vi.fn(async () => ({ data: [plainUser] })),
      userLimits: userLimits as unknown as PortalApi['userLimits'],
    });

    fireEvent.click(await screen.findByRole('button', { name: t.userActionLimits }));
    await waitFor(() => expect(userLimits).toHaveBeenCalledWith('user-3'));
    expect(
      (await screen.findByLabelText(t.limitRequestQuotaLabel)) as HTMLInputElement,
    ).toHaveValue(10000);
    expect(screen.getByText(t.limitUsageRequestsLine(8000, 10000))).toBeInTheDocument();
  });

  it('saves an edited limit via setUserLimits and closes the dialog', async () => {
    const userLimits = vi.fn(async () => ({
      limits: EMPTY_LIMIT_CONFIG,
      usage: { requests_this_period: 0, tokens_this_period: 0, cost_this_period: 0 },
    }));
    const setUserLimits = vi.fn(async (_id: string, cfg: LimitConfig) => ({
      limits: cfg,
      usage: { requests_this_period: 0, tokens_this_period: 0, cost_this_period: 0 },
    }));
    renderUsersView({
      adminUsers: vi.fn(async () => ({ data: [plainUser] })),
      userLimits: userLimits as unknown as PortalApi['userLimits'],
      setUserLimits: setUserLimits as unknown as PortalApi['setUserLimits'],
    });

    fireEvent.click(await screen.findByRole('button', { name: t.userActionLimits }));
    fireEvent.change(await screen.findByLabelText(t.limitRateRequestsLabel), {
      target: { value: '20' },
    });
    fireEvent.change(screen.getByLabelText(t.limitRateWindowLabel), { target: { value: '60' } });
    fireEvent.click(screen.getByRole('button', { name: t.save }));

    await waitFor(() =>
      expect(setUserLimits).toHaveBeenCalledWith('user-3', {
        ...EMPTY_LIMIT_CONFIG,
        rate_requests: 20,
        rate_window_seconds: 60,
      }),
    );
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
  });
});

// --- Invite-form admin-group picker (spec:
// 2026-08-09-group-visibility-admin-group-invite-design.md) ---

function makeGroup(overrides: Partial<UserGroup> = {}): UserGroup {
  return {
    id: 'grp-1',
    tier: 'system',
    name: 'Group One',
    parent_group_id: '',
    owner_user_id: 'usr-owner',
    owner_name: '',
    my_role: 'member',
    can_manage: false,
    can_manage_users: false,
    can_manage_servers: false,
    can_manage_services: false,
    can_manage_resources: false,
    member_count: 1,
    manager_count: 0,
    ...overrides,
  };
}

function landscapeWithAdmin(admin: UserGroup[]): GroupLandscape {
  return { system: [], admin, user: [] };
}

function openCreateForm() {
  fireEvent.click(screen.getByRole('button', { name: t.userCreate }));
}

describe('UsersView invite-form admin-group picker', () => {
  it('shows a required select for 2+ manageable admin groups and sends the chosen id', async () => {
    const groupA = makeGroup({
      id: 'ag-a',
      name: 'Admin A',
      tier: 'admin',
      can_manage: true,
      can_manage_users: true,
    });
    const groupB = makeGroup({
      id: 'ag-b',
      name: 'Admin B',
      tier: 'admin',
      can_manage: true,
      can_manage_users: true,
    });
    const createUser = vi.fn(async (_b: CreateUserRequest) => ({
      invite_url: 'https://gw.example/invite/xyz',
    }));
    renderUsersView({
      groups: vi.fn(async () =>
        landscapeWithAdmin([groupA, groupB]),
      ) as unknown as PortalApi['groups'],
      createUser: createUser as unknown as PortalApi['createUser'],
    });
    openCreateForm();
    fireEvent.change(await screen.findByLabelText(t.tableEmail), {
      target: { value: 'new@example.com' },
    });
    const combo = await screen.findByRole('combobox', { name: t.userInviteAdminGroupLabel });
    expect(screen.getByRole('button', { name: t.userCreate })).toBeDisabled();
    fireEvent.mouseDown(combo);
    fireEvent.click(await screen.findByRole('option', { name: 'Admin A' }));
    expect(screen.getByRole('button', { name: t.userCreate })).toBeEnabled();
    fireEvent.click(screen.getByRole('button', { name: t.userCreate }));
    await waitFor(() => expect(createUser).toHaveBeenCalled());
    expect(createUser.mock.calls[0][0].admin_group_id).toBe('ag-a');
  });

  it('auto-sends the single manageable admin group id (no control) when exactly one exists', async () => {
    const solo = makeGroup({
      id: 'ag-solo',
      name: 'Solo',
      tier: 'admin',
      can_manage: true,
      can_manage_users: true,
    });
    const groupsSpy = vi.fn(async () => landscapeWithAdmin([solo]));
    const createUser = vi.fn(async (_b: CreateUserRequest) => ({
      invite_url: 'https://gw.example/invite/xyz',
    }));
    renderUsersView({
      groups: groupsSpy as unknown as PortalApi['groups'],
      createUser: createUser as unknown as PortalApi['createUser'],
    });
    openCreateForm();
    fireEvent.change(await screen.findByLabelText(t.tableEmail), {
      target: { value: 'new@example.com' },
    });
    await waitFor(() => expect(groupsSpy).toHaveBeenCalled());
    expect(screen.queryByRole('combobox', { name: t.userInviteAdminGroupLabel })).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: t.userCreate }));
    await waitFor(() => expect(createUser).toHaveBeenCalled());
    expect(createUser.mock.calls[0][0].admin_group_id).toBe('ag-solo');
  });

  it('shows the no-admin-group hint and blocks submit when none are manageable', async () => {
    const createUser = vi.fn(async (_b: CreateUserRequest) => ({ invite_url: 'x' }));
    renderUsersView({
      groups: vi.fn(async () => landscapeWithAdmin([])) as unknown as PortalApi['groups'],
      createUser: createUser as unknown as PortalApi['createUser'],
    });
    openCreateForm();
    fireEvent.change(await screen.findByLabelText(t.tableEmail), {
      target: { value: 'new@example.com' },
    });
    expect(await screen.findByText(t.userInviteNoAdminGroupHint)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: t.userCreate })).toBeDisabled();
  });

  // Per-Admin-Group co-manager permissions (spec 2026-08-10) narrowed the
  // picker's filter source from can_manage (group STRUCTURE) to
  // can_manage_users (USER assignment) -- a co-manager restricted to
  // structure-only must not see a group as an invite target, even though
  // can_manage is still true for them.
  it("excludes a group where can_manage is true but can_manage_users is false (a structure-only co-manager's group)", async () => {
    const eligible = makeGroup({
      id: 'ag-eligible',
      name: 'Eligible',
      tier: 'admin',
      can_manage: true,
      can_manage_users: true,
    });
    const structureOnly = makeGroup({
      id: 'ag-structure-only',
      name: 'StructureOnly',
      tier: 'admin',
      can_manage: true,
      can_manage_users: false,
    });
    const createUser = vi.fn(async (_b: CreateUserRequest) => ({
      invite_url: 'https://gw.example/invite/xyz',
    }));
    renderUsersView({
      groups: vi.fn(async () =>
        landscapeWithAdmin([eligible, structureOnly]),
      ) as unknown as PortalApi['groups'],
      createUser: createUser as unknown as PortalApi['createUser'],
    });
    openCreateForm();
    fireEvent.change(await screen.findByLabelText(t.tableEmail), {
      target: { value: 'new@example.com' },
    });
    // Exactly ONE group is actually eligible (can_manage_users) -> it
    // auto-selects with NO picker control shown. If the filter regressed
    // back to can_manage, BOTH groups would count as manageable and the
    // mandatory dropdown would appear instead.
    await waitFor(() =>
      expect(screen.queryByRole('combobox', { name: t.userInviteAdminGroupLabel })).toBeNull(),
    );
    fireEvent.click(screen.getByRole('button', { name: t.userCreate }));
    await waitFor(() => expect(createUser).toHaveBeenCalled());
    expect(createUser.mock.calls[0][0].admin_group_id).toBe('ag-eligible');
  });
});
