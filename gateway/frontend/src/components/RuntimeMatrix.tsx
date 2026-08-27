// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import {
  Box,
  IconButton,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableRow,
  Tooltip,
  Typography,
} from '@mui/material';
import CheckCircleIcon from '@mui/icons-material/CheckCircle';
import BlockIcon from '@mui/icons-material/Block';
import type { Translation } from './shared/types';

export type RuntimeMatrixSpec = {
  id: string;
  model: string;
  gpus: { index: number; vramMb: number }[];
};

// The store is a dumb pair table; canonicalisation (which id sorts first) is
// entirely the portal's job -- see the task-21 brief. Comparing BOTH orders
// here means a pair that ever reaches this component non-canonical (e.g. a
// future caller that forgot to sort) still renders as allowed, rather than
// silently disagreeing with what the backend actually holds.
function pairAllowed(pairs: [string, string][], x: string, y: string): boolean {
  return pairs.some(([p, q]) => (p === x && q === y) || (p === y && q === x));
}

function canonicalPair(x: string, y: string): [string, string] {
  return x < y ? [x, y] : [y, x];
}

// The advisory tooltip: the summed estimated VRAM per GPU the two specs BOTH
// touch, against that GPU's configured budget -- "GPU 0: 44000 / 46000 MB",
// flagged when over. Disjoint GPUs never compete for VRAM, so there is
// nothing to sum for them; say that plainly instead of an empty table. This
// is guidance only -- the agent's own per-GPU arithmetic at admission time is
// the real veto, so the note at the bottom says so explicitly (a
// matrix-allowed pair can still be refused at runtime, and vice versa).
function MatrixTooltip({
  t,
  a,
  b,
  budgets,
}: Readonly<{
  t: Translation;
  a: RuntimeMatrixSpec;
  b: RuntimeMatrixSpec;
  budgets: Record<number, number>;
}>) {
  const bByIndex = new Map(b.gpus.map((g) => [g.index, g.vramMb]));
  const shared = a.gpus.filter((g) => bByIndex.has(g.index));
  return (
    <Box sx={{ display: 'grid', gap: 0.25 }}>
      {shared.length === 0 ? (
        <Typography variant="caption">{t.runtimeMatrixNoSharedGpu}</Typography>
      ) : (
        shared.map((g) => {
          const sum = g.vramMb + (bByIndex.get(g.index) ?? 0);
          const budget = budgets[g.index];
          const over = budget !== undefined && sum > budget;
          const base =
            budget !== undefined
              ? `GPU ${g.index}: ${sum} / ${budget} MB`
              : `GPU ${g.index}: ${sum} MB`;
          return (
            <Typography
              key={g.index}
              variant="caption"
              sx={over ? { color: 'error.main' } : undefined}
            >
              {over ? `${base} (${t.runtimeMatrixOverBudget})` : base}
            </Typography>
          );
        })
      )}
      <Typography variant="caption" color="text.secondary" sx={{ fontStyle: 'italic' }}>
        {t.runtimeMatrixAdvisory}
      </Typography>
    </Box>
  );
}

/**
 * The co-residency triangle: rows 2..n against columns 1..n-1, strictly below
 * the diagonal only -- co-residency is symmetric ("A with B" === "B with A")
 * and irreflexive (a spec is never paired with itself), so a full n×n grid
 * would draw every fact twice plus a meaningless diagonal. Renders inside its
 * own horizontally-scrolling container so a wide matrix never widens the page
 * (house pattern). Every cell carries a full accessible name
 * (`${t.runtimeMatrixCell}: ${row.model} + ${col.model}`) so tests and
 * screen readers can address it by role instead of DOM position.
 *
 * `onToggle` always fires with the pair sorted (a < b), regardless of which
 * of the two specs is the visual row vs. column -- the backend stores/reports
 * pairs canonically and expects the same on write (see SetCoResidency in
 * service_runtime.go); getting this wrong makes the UI and the backend
 * silently disagree about which cell is set.
 */
export function RuntimeMatrix({
  t,
  specs,
  pairs,
  onToggle,
  budgets,
  disabled = false,
}: Readonly<{
  t: Translation;
  specs: RuntimeMatrixSpec[];
  pairs: [string, string][];
  onToggle: (a: string, b: string) => void;
  budgets: Record<number, number>;
  disabled?: boolean;
}>) {
  if (specs.length < 2) {
    return <Typography color="text.secondary">{t.runtimeMatrixNeedTwo}</Typography>;
  }

  const columnSpecs = specs.slice(0, -1);
  const rowSpecs = specs.slice(1);

  return (
    <Box sx={{ overflowX: 'auto' }}>
      <Table size="small">
        <TableHead>
          <TableRow>
            <TableCell />
            {columnSpecs.map((col) => (
              <TableCell key={col.id} scope="col">
                {col.model}
              </TableCell>
            ))}
          </TableRow>
        </TableHead>
        <TableBody>
          {rowSpecs.map((rowSpec, rowOffset) => {
            // rowOffset is this row's index within rowSpecs (specs[1..n-1]);
            // its position in the FULL specs array is rowOffset+1, which is
            // also exactly how many columns (specs[0..rowOffset]) it draws --
            // the strictly-lower-triangle rule.
            const visibleColumns = columnSpecs.slice(0, rowOffset + 1);
            return (
              <TableRow key={rowSpec.id}>
                <TableCell component="th" scope="row">
                  {rowSpec.model}
                </TableCell>
                {visibleColumns.map((colSpec) => {
                  const [a, b] = canonicalPair(rowSpec.id, colSpec.id);
                  const allowed = pairAllowed(pairs, rowSpec.id, colSpec.id);
                  return (
                    <TableCell key={colSpec.id} align="center">
                      <Tooltip
                        title={<MatrixTooltip t={t} a={rowSpec} b={colSpec} budgets={budgets} />}
                      >
                        <span>
                          <IconButton
                            size="small"
                            color={allowed ? 'success' : 'default'}
                            disabled={disabled}
                            aria-label={`${t.runtimeMatrixCell}: ${rowSpec.model} + ${colSpec.model}`}
                            aria-pressed={allowed}
                            onClick={() => onToggle(a, b)}
                          >
                            {allowed ? (
                              <CheckCircleIcon fontSize="small" />
                            ) : (
                              <BlockIcon fontSize="small" />
                            )}
                          </IconButton>
                        </span>
                      </Tooltip>
                    </TableCell>
                  );
                })}
              </TableRow>
            );
          })}
        </TableBody>
      </Table>
    </Box>
  );
}
