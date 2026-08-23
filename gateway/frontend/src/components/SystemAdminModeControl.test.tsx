// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { MenuList } from '@mui/material';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { SystemAdminModeControl } from './SystemAdminModeControl';
import { messages } from '../i18n';
import { PortalApiError, type CurrentUser } from '../api';
import type { PortalApi } from './shared/types';

const t = messages.de;
afterEach(cleanup);

function makeUser(over: Partial<CurrentUser> = {}): CurrentUser {
  return {
    id: 'u',
    email: 'e@x.io',
    display_name: 'E',
    role: 'system_admin',
    preferred_language: 'de',
    totp_enabled: false,
    totp_mode: 'off',
    system_admin_mode: false,
    system_admin_mode_expires_at: '',
    system_admin_mode_require_password: true,
    ...over,
  };
}

function renderControl(
  user: CurrentUser | null,
  apiOverrides: Partial<Pick<PortalApi, 'enterSystemAdminMode' | 'exitSystemAdminMode'>> = {},
  {
    onChanged = vi.fn(),
    onAction = vi.fn(),
  }: { onChanged?: () => void; onAction?: () => void } = {},
) {
  const api: Pick<PortalApi, 'enterSystemAdminMode' | 'exitSystemAdminMode'> = {
    enterSystemAdminMode: vi.fn(),
    exitSystemAdminMode: vi.fn(),
    ...apiOverrides,
  };
  // Rendered inside a MenuList to mirror the user dropdown it lives in (the
  // trigger renders as MenuItems); the confirm dialog is a portal.
  render(
    <MenuList>
      <SystemAdminModeControl
        t={t}
        api={api}
        currentUser={user}
        onChanged={onChanged}
        onAction={onAction}
      />
    </MenuList>,
  );
  return { api, onChanged, onAction };
}

describe('SystemAdminModeControl', () => {
  it('renders nothing for a non-system_admin role', () => {
    renderControl(makeUser({ role: 'admin' }));
    expect(
      screen.queryByRole('menuitem', { name: t.systemAdminModeEnter }),
    ).not.toBeInTheDocument();
  });

  it('renders nothing when there is no current user', () => {
    renderControl(null);
    expect(
      screen.queryByRole('menuitem', { name: t.systemAdminModeEnter }),
    ).not.toBeInTheDocument();
  });

  it('require-password true: clicking Enter opens a dialog; submitting calls enterSystemAdminMode(password)', async () => {
    const enterSystemAdminMode = vi.fn(async () => makeUser({ system_admin_mode: true }));
    const { onChanged, onAction } = renderControl(
      makeUser({ system_admin_mode_require_password: true }),
      { enterSystemAdminMode },
    );

    fireEvent.click(screen.getByRole('menuitem', { name: t.systemAdminModeEnter }));
    expect(onAction).toHaveBeenCalledTimes(1); // closes the dropdown
    expect(await screen.findByText(t.systemAdminModeDialogBody)).toBeInTheDocument();
    expect(enterSystemAdminMode).not.toHaveBeenCalled();

    const dialog = screen.getByRole('dialog');
    fireEvent.change(within(dialog).getByLabelText(t.systemAdminModePasswordLabel), {
      target: { value: 'sekret' },
    });
    fireEvent.click(within(dialog).getByRole('button', { name: t.systemAdminModeEnter }));

    await waitFor(() => expect(enterSystemAdminMode).toHaveBeenCalledWith('sekret'));
    await waitFor(() =>
      expect(onChanged).toHaveBeenCalledWith(expect.objectContaining({ system_admin_mode: true })),
    );
    // The dialog closes on success.
    await waitFor(() =>
      expect(screen.queryByText(t.systemAdminModeDialogBody)).not.toBeInTheDocument(),
    );
  });

  it('require-password false: clicking Enter calls enterSystemAdminMode() directly, no dialog', async () => {
    const enterSystemAdminMode = vi.fn(async () =>
      makeUser({ system_admin_mode: true, system_admin_mode_require_password: false }),
    );
    const { onChanged, onAction } = renderControl(
      makeUser({ system_admin_mode_require_password: false }),
      { enterSystemAdminMode },
    );

    fireEvent.click(screen.getByRole('menuitem', { name: t.systemAdminModeEnter }));
    expect(onAction).toHaveBeenCalledTimes(1);

    await waitFor(() => expect(enterSystemAdminMode).toHaveBeenCalledWith(undefined));
    await waitFor(() =>
      expect(onChanged).toHaveBeenCalledWith(expect.objectContaining({ system_admin_mode: true })),
    );
    expect(screen.queryByText(t.systemAdminModeDialogBody)).not.toBeInTheDocument();
  });

  it('elevated: shows the active status + a leave item that calls exitSystemAdminMode', async () => {
    const exitSystemAdminMode = vi.fn(async () => makeUser({ system_admin_mode: false }));
    const { onChanged, onAction } = renderControl(makeUser({ system_admin_mode: true }), {
      exitSystemAdminMode,
    });

    expect(screen.getByText(t.systemAdminModeActive)).toBeInTheDocument();
    expect(
      screen.queryByRole('menuitem', { name: t.systemAdminModeEnter }),
    ).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole('menuitem', { name: t.systemAdminModeLeave }));
    expect(onAction).toHaveBeenCalledTimes(1);

    await waitFor(() => expect(exitSystemAdminMode).toHaveBeenCalled());
    await waitFor(() =>
      expect(onChanged).toHaveBeenCalledWith(expect.objectContaining({ system_admin_mode: false })),
    );
  });

  it('shows an inline error and keeps the dialog open on a wrong-password error', async () => {
    const enterSystemAdminMode = vi.fn(async () => {
      throw new PortalApiError(401, 'auth.invalid_credentials', 'invalid credentials');
    });
    renderControl(makeUser({ system_admin_mode_require_password: true }), { enterSystemAdminMode });

    fireEvent.click(screen.getByRole('menuitem', { name: t.systemAdminModeEnter }));
    await screen.findByText(t.systemAdminModeDialogBody);

    const dialog = screen.getByRole('dialog');
    fireEvent.change(within(dialog).getByLabelText(t.systemAdminModePasswordLabel), {
      target: { value: 'wrong' },
    });
    fireEvent.click(within(dialog).getByRole('button', { name: t.systemAdminModeEnter }));

    await waitFor(() => expect(enterSystemAdminMode).toHaveBeenCalled());
    expect(await screen.findByRole('alert')).toHaveTextContent(t.errorAuthInvalidCredentials);
    // Still open — the password field is still reachable.
    expect(screen.getByLabelText(t.systemAdminModePasswordLabel)).toBeInTheDocument();
  });
});
