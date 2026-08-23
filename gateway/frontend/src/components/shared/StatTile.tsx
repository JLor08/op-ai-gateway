// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { ReactNode } from 'react';
import { Paper } from '@mui/material';

/**
 * Outlined metric card: big value, bold label, optional muted detail line.
 * Extracted verbatim from the Dashboard metric markup so both the Dashboard
 * and the Activity 5-tile row share one card style.
 */
export function StatTile({
  value,
  label,
  detail,
}: Readonly<{
  value: ReactNode;
  label: ReactNode;
  detail?: ReactNode;
}>) {
  return (
    <Paper
      component="article"
      variant="outlined"
      sx={{
        minHeight: 132,
        p: 2.25,
        borderTop: '4px solid var(--brand-primary)',
        '& strong': { display: 'block', mb: 1.25, fontSize: 30, lineHeight: 1 },
        '& span': { display: 'block', fontWeight: 700 },
        '& small': { display: 'block', mt: 1.5, color: 'var(--muted)' },
      }}
    >
      <strong>{value}</strong>
      <span>{label}</span>
      {detail !== undefined && <small>{detail}</small>}
    </Paper>
  );
}
