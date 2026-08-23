// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useState, type MouseEvent, type ReactNode } from 'react';
import { Button, Menu, MenuItem } from '@mui/material';
import AccountCircleIcon from '@mui/icons-material/AccountCircle';
import KeyboardArrowDownIcon from '@mui/icons-material/KeyboardArrowDown';
import type { Translation } from './types';

export function UserMenu({
  displayName,
  onProfile,
  onLogout,
  t,
  systemAdminSlot,
}: Readonly<{
  displayName: string;
  onProfile: () => void;
  onLogout: () => void;
  t: Translation;
  /**
   * Optional content rendered ABOVE the profile item (e.g. the system-admin
   * step-up control). Receives a `closeMenu` callback so its items can close
   * the dropdown. Given as a render prop; the slot owns its own trailing
   * divider so nothing renders when it collapses to null (non-system-admins).
   * When a slot is present the Menu is `keepMounted` so a dialog the slot opens
   * survives the menu closing.
   */
  systemAdminSlot?: (closeMenu: () => void) => ReactNode;
}>) {
  const [anchorEl, setAnchorEl] = useState<null | HTMLElement>(null);
  const open = Boolean(anchorEl);
  const closeMenu = () => setAnchorEl(null);
  return (
    <>
      <Button
        color="inherit"
        aria-haspopup="menu"
        aria-expanded={open ? 'true' : undefined}
        onClick={(e: MouseEvent<HTMLElement>) => setAnchorEl(e.currentTarget)}
        startIcon={<AccountCircleIcon aria-hidden="true" />}
        endIcon={<KeyboardArrowDownIcon aria-hidden="true" />}
        sx={{ fontWeight: 700, textTransform: 'none', whiteSpace: 'nowrap' }}
      >
        {displayName}
      </Button>
      <Menu
        anchorEl={anchorEl}
        open={open}
        onClose={closeMenu}
        keepMounted={Boolean(systemAdminSlot)}
      >
        {systemAdminSlot ? systemAdminSlot(closeMenu) : null}
        <MenuItem
          onClick={() => {
            onProfile();
            closeMenu();
          }}
        >
          {t.profile}
        </MenuItem>
        <MenuItem
          onClick={() => {
            onLogout();
            closeMenu();
          }}
        >
          {t.logout}
        </MenuItem>
      </Menu>
    </>
  );
}
