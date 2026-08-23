// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useState } from 'react';
import type { SubmitEvent } from 'react';
import {
  Box,
  Button,
  Dialog,
  DialogActions,
  DialogContent,
  DialogContentText,
  DialogTitle,
  Divider,
  MenuItem,
  Typography,
} from '@mui/material';
import AdminPanelSettingsIcon from '@mui/icons-material/AdminPanelSettings';
import type { CurrentUser } from '../api';
import type { PortalApi, Translation } from './shared/types';
import { Field } from './shared/Field';
import { formatPortalError } from './shared/format';

/**
 * System-admin step-up control, rendered as items INSIDE the user dropdown
 * menu (above "Profil"). Renders ONLY for a `system_admin` role (any other
 * role, incl. a plain admin, sees nothing) -- the elevation itself is a
 * per-session capability, not a role, so this is the ONE place that still
 * gates on `role` directly (every other capability gate in the app now reads
 * `systemAdminMode`, derived from `currentUser.system_admin_mode`).
 *
 * Not elevated: an "enter" menu item; when the account requires a password
 * re-entry to elevate (`system_admin_mode_require_password`), it opens a
 * confirm dialog first, else it elevates directly. Elevated: an active-status
 * line + a "leave" menu item. Both mutations return the fresh `CurrentUser`,
 * which the caller (App) adopts via `onChanged` so every gated view/nav item
 * updates immediately.
 *
 * `onAction` is called at the start of each trigger click to close the parent
 * dropdown; the parent `Menu` is `keepMounted`, so the confirm dialog this
 * control opens survives that close.
 */
export function SystemAdminModeControl({
  t,
  api,
  currentUser,
  onChanged,
  onAction,
}: Readonly<{
  t: Translation;
  api: Pick<PortalApi, 'enterSystemAdminMode' | 'exitSystemAdminMode'>;
  currentUser: CurrentUser | null;
  onChanged: (user: CurrentUser) => void;
  onAction?: () => void;
}>) {
  const [dialogOpen, setDialogOpen] = useState(false);
  const [password, setPassword] = useState('');
  const [dialogError, setDialogError] = useState('');
  const [busy, setBusy] = useState(false);

  if (currentUser?.role !== 'system_admin') {
    return null;
  }

  const elevated = currentUser.system_admin_mode;

  function openDialog() {
    setPassword('');
    setDialogError('');
    setDialogOpen(true);
  }

  function closeDialog() {
    if (busy) return;
    setDialogOpen(false);
    setPassword('');
    setDialogError('');
  }

  async function enter(withPassword?: string) {
    setBusy(true);
    try {
      const updated = await api.enterSystemAdminMode(withPassword);
      onChanged(updated);
      setDialogOpen(false);
      setPassword('');
      setDialogError('');
    } catch (err) {
      setDialogError(formatPortalError(err, t));
      setDialogOpen(true);
    } finally {
      setBusy(false);
    }
  }

  async function leave() {
    setBusy(true);
    try {
      const updated = await api.exitSystemAdminMode();
      onChanged(updated);
    } finally {
      setBusy(false);
    }
  }

  function handleEnterClick() {
    if (currentUser!.system_admin_mode_require_password) {
      openDialog();
    } else {
      void enter();
    }
  }

  function submitDialog(event: SubmitEvent<HTMLFormElement>) {
    event.preventDefault();
    void enter(password);
  }

  return (
    <>
      {elevated ? (
        <>
          <MenuItem
            disabled
            sx={{
              gap: 1,
              color: 'warning.main',
              fontWeight: 700,
              '&.Mui-disabled': { opacity: 1 },
            }}
          >
            <AdminPanelSettingsIcon fontSize="small" aria-hidden="true" />
            {t.systemAdminModeActive}
          </MenuItem>
          <MenuItem
            disabled={busy}
            onClick={() => {
              onAction?.();
              void leave();
            }}
          >
            {t.systemAdminModeLeave}
          </MenuItem>
        </>
      ) : (
        <MenuItem
          disabled={busy}
          sx={{ gap: 1 }}
          onClick={() => {
            onAction?.();
            handleEnterClick();
          }}
        >
          <AdminPanelSettingsIcon fontSize="small" aria-hidden="true" />
          {t.systemAdminModeEnter}
        </MenuItem>
      )}
      <Divider />
      <Dialog open={dialogOpen} onClose={closeDialog}>
        <Box component="form" onSubmit={submitDialog}>
          <DialogTitle>{t.systemAdminModeDialogTitle}</DialogTitle>
          <DialogContent>
            <DialogContentText>{t.systemAdminModeDialogBody}</DialogContentText>
            <Box sx={{ mt: 2 }}>
              <Field
                id="system-admin-mode-password"
                label={t.systemAdminModePasswordLabel}
                type="password"
                value={password}
                onChange={(event) => setPassword(event.target.value)}
                autoComplete="current-password"
                autoFocus
                required
              />
            </Box>
            {dialogError && (
              <Typography color="error" role="alert" sx={{ mt: 1.5 }}>
                {dialogError}
              </Typography>
            )}
          </DialogContent>
          <DialogActions>
            <Button onClick={closeDialog} color="secondary" disabled={busy}>
              {t.cancel}
            </Button>
            <Button type="submit" variant="contained" disabled={busy || password === ''}>
              {t.systemAdminModeEnter}
            </Button>
          </DialogActions>
        </Box>
      </Dialog>
    </>
  );
}
