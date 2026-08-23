// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { UserMenu } from './UserMenu';
import { messages } from '../../i18n';

const t = messages.de;
afterEach(cleanup);

describe('UserMenu', () => {
  it('fires profile and logout from the menu', async () => {
    const onProfile = vi.fn();
    const onLogout = vi.fn();
    render(<UserMenu displayName="Dev User" onProfile={onProfile} onLogout={onLogout} t={t} />);

    fireEvent.click(screen.getByRole('button', { name: 'Dev User' }));
    fireEvent.click(await screen.findByRole('menuitem', { name: t.profile }));
    expect(onProfile).toHaveBeenCalledTimes(1);

    fireEvent.click(screen.getByRole('button', { name: 'Dev User' }));
    fireEvent.click(await screen.findByRole('menuitem', { name: t.logout }));
    expect(onLogout).toHaveBeenCalledTimes(1);
  });

  it('renders the systemAdminSlot above profile and passes a working closeMenu', async () => {
    render(
      <UserMenu
        displayName="Dev User"
        onProfile={vi.fn()}
        onLogout={vi.fn()}
        t={t}
        systemAdminSlot={(closeMenu) => (
          <button type="button" onClick={closeMenu}>
            step-up
          </button>
        )}
      />,
    );

    fireEvent.click(screen.getByRole('button', { name: 'Dev User' }));
    const slot = await screen.findByRole('button', { name: 'step-up' });
    // The slot precedes the profile item in the menu DOM order.
    const profile = screen.getByRole('menuitem', { name: t.profile });
    expect(slot.compareDocumentPosition(profile) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();

    // The provided closeMenu closes the dropdown.
    fireEvent.click(slot);
    await waitFor(() =>
      expect(screen.queryByRole('menuitem', { name: t.profile })).not.toBeInTheDocument(),
    );
  });
});
