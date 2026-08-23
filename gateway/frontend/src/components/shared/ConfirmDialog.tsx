// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { ReactNode } from 'react';
import {
  Button,
  Dialog,
  DialogActions,
  DialogContent,
  DialogContentText,
  DialogTitle,
} from '@mui/material';

/**
 * Ersetzt den Inline-Zwei-Schritt-Confirm und window.confirm. Das jeweilige
 * Domänen-Löschlabel (z. B. t.tokenActionDelete === 'Loeschen') als confirmLabel
 * übergeben, damit der Bestätigen-Button diesen Accessible Name behält. Der
 * optionale `body` rendert einen erläuternden Absatz; der optionale `extra`-Slot
 * rendert beliebige Steuerelemente (z. B. eine Checkbox) NACH dem Body und vor den
 * Aktionen. Rendert nichts, solange geschlossen.
 */
export function ConfirmDialog({
  open,
  title,
  body,
  extra,
  confirmLabel,
  cancelLabel,
  onConfirm,
  onCancel,
}: Readonly<{
  open: boolean;
  title: string;
  body?: string;
  extra?: ReactNode;
  confirmLabel: string;
  cancelLabel: string;
  onConfirm: () => void;
  onCancel: () => void;
}>) {
  return (
    <Dialog open={open} onClose={onCancel}>
      <DialogTitle>{title}</DialogTitle>
      {(body || extra) && (
        <DialogContent>
          {body && <DialogContentText>{body}</DialogContentText>}
          {extra}
        </DialogContent>
      )}
      <DialogActions>
        <Button onClick={onCancel} color="secondary">
          {cancelLabel}
        </Button>
        <Button onClick={onConfirm} color="error" variant="contained">
          {confirmLabel}
        </Button>
      </DialogActions>
    </Dialog>
  );
}
