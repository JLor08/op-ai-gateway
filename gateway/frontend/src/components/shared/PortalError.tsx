// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { Alert } from '@mui/material';
import type { Translation } from './types';

export function PortalError({ t, error }: Readonly<{ t: Translation; error: string }>) {
  if (!error) {
    return null;
  }
  return (
    <Alert severity="error" role="alert">
      {t.portalError}: {error}
    </Alert>
  );
}
