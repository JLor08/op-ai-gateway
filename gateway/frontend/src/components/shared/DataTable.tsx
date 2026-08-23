// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { ReactNode } from 'react';
import {
  Paper,
  Table,
  TableBody,
  TableCell,
  TableContainer,
  TableHead,
  TableRow,
} from '@mui/material';

/**
 * Drop-in für TableWrap: identische Props, MUI-Body. Kopfzellen rendern als <th>
 * (Rolle columnheader); die aufrufende View MUSS ihre Zeilen von <tr>/<td> auf
 * MUI <TableRow>/<TableCell> umstellen (beide emittieren weiter <tr>/<td>, also
 * bleiben Rollen row/cell und `.closest('tr')` erhalten).
 */
export function DataTable({
  columns,
  isEmpty,
  emptyLabel,
  children,
  minWidth = 680,
}: Readonly<{
  columns: string[];
  isEmpty: boolean;
  emptyLabel: string;
  children: ReactNode;
  minWidth?: number;
}>) {
  return (
    <TableContainer
      component={Paper}
      variant="outlined"
      sx={{
        overflowX: 'auto',
        borderTop: '4px solid transparent',
        borderImage: 'linear-gradient(90deg, var(--brand-accent), var(--brand-primary)) 1',
      }}
    >
      <Table size="small" sx={{ minWidth }}>
        <TableHead>
          <TableRow>
            {columns.map((col) => (
              <TableCell key={col}>{col}</TableCell>
            ))}
          </TableRow>
        </TableHead>
        <TableBody>
          {isEmpty && (
            <TableRow>
              <TableCell colSpan={columns.length}>{emptyLabel}</TableCell>
            </TableRow>
          )}
          {children}
        </TableBody>
      </Table>
    </TableContainer>
  );
}
