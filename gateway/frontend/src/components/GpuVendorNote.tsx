// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { Alert, Button, type SxProps, type Theme } from '@mui/material';
import { useState } from 'react';
import { type GpuVendor, vendorLabel } from '../api';
import type { Translation } from './shared/types';

/**
 * The note that a server's GPU rows written before vendors existed (#434) were
 * assigned to one vendor, with the button that acknowledges it. The caller
 * renders it while `server.gpu_vendor_note` is true; `onDismiss` persists the
 * acknowledgement (dismissGpuVendorNote). The note hides itself as soon as the
 * dismissal succeeds, so it does not wait for the server to be read again; a
 * failed dismissal keeps it and is reported through `onError`.
 */
export function GpuVendorNote({
  t,
  vendor,
  onDismiss,
  onError,
  sx,
}: Readonly<{
  t: Translation;
  vendor: GpuVendor | undefined;
  onDismiss: () => Promise<void>;
  onError?: (err: unknown) => void;
  // Applied to the note itself, so a margin goes away with it on dismissal.
  sx?: SxProps<Theme>;
}>) {
  const [dismissed, setDismissed] = useState(false);
  const [busy, setBusy] = useState(false);
  const label = vendorLabel(vendor ?? '');
  if (dismissed || label === '') return null;

  async function dismiss() {
    setBusy(true);
    try {
      await onDismiss();
      setDismissed(true);
    } catch (err) {
      onError?.(err);
    } finally {
      setBusy(false);
    }
  }

  return (
    <Alert
      severity="info"
      sx={sx}
      action={
        <Button color="inherit" size="small" disabled={busy} onClick={() => void dismiss()}>
          {t.gpuLegacyMappedDismiss}
        </Button>
      }
    >
      {t.gpuLegacyMappedNote(label)}
    </Alert>
  );
}
