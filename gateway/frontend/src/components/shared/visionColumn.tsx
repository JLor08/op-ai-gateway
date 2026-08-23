// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { Chip } from '@mui/material';
import type { ReactNode } from 'react';
import type { ListColumn } from './ListTable';
import type { Translation } from './types';

/** Unified "Vision" cell: a neutral outlined chip when capable, else an en-dash. */
export function renderVisionCell(capable: boolean, label: string): ReactNode {
  return capable ? <Chip size="small" variant="outlined" label={label} /> : '–';
}

/** A consistent "Vision" ListColumn for any row type. `getCapable` reads the row's
 *  vision flag; `defaultHidden` preserves each table's current visibility. */
export function makeVisionColumn<Row>(
  t: Translation,
  getCapable: (row: Row) => boolean,
  opts?: { defaultHidden?: boolean },
): ListColumn<Row> {
  return {
    id: 'vision',
    label: t.tableModelVision,
    value: (row) => (getCapable(row) ? 'yes' : 'no'),
    render: (row) => renderVisionCell(getCapable(row), t.tableModelVision),
    filter: 'enum',
    searchable: false,
    enumLabel: (v) => (v === 'yes' ? t.tableModelVision : '–'),
    ...(opts?.defaultHidden ? { defaultHidden: true } : {}),
  };
}
