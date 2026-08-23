// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { Box, Chip } from '@mui/material';
import type { BadgeStatus } from './types';
import { statusChipSx, statusClassByKey } from './status';

/**
 * MUI-Chip, dessen Label ein <span data-status="<key>"> mit dem übersetzten Label
 * als einzigem Text-Node ist. Testing-Librarys getByText(label) löst auf diesen
 * inneren span auf (Elemente mit Kind-Elementen werden übersprungen), daher bleibt
 * getByText(label).toHaveAttribute('data-status', key) grün. IMMER das übersetzte
 * Label übergeben — nie den rohen Status.
 */
export function StatusChip({ status, label }: Readonly<{ status: BadgeStatus; label: string }>) {
  const key = statusClassByKey[status] as keyof typeof statusChipSx;
  const sx = statusChipSx[key];
  return (
    <Chip
      size="small"
      sx={{ bgcolor: sx.bg, color: sx.color }}
      label={
        <Box component="span" data-status={key}>
          {label}
        </Box>
      }
    />
  );
}
