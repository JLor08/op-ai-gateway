// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { ReactNode } from 'react';
import { Stack } from '@mui/material';

/** Horizontaler Zeilen-Aktions-Container. Kinder sind MUI <Button size="small"/>. */
export function RowActions({ children }: Readonly<{ children: ReactNode }>) {
  return (
    <Stack direction="row" sx={{ flexWrap: 'wrap', gap: 1 }}>
      {children}
    </Stack>
  );
}
