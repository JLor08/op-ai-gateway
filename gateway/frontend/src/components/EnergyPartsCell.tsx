// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { Box, Tooltip, Typography } from '@mui/material';
import type { UsageEnergyPart, UsageEvent } from '../api';
import { formatCost, type CurrencyUnit } from '../currency';
import type { Translation } from './shared/types';
import { chipWithHelp } from './chipWithHelp';
import { ENERGY_SOURCE_HELP_KEYS } from './billingUnit';

function whText(wh: number): string {
  return wh.toFixed(3);
}

// A part's label: the server's name when the viewer may see it, else "Head"
// for the head and the member's number for a hidden member.
function partLabel(part: UsageEnergyPart, t: Translation): string {
  if (part.server_name) return part.server_name;
  return part.role === 'head'
    ? t.activityEnergyPartHead
    : t.activityEnergyPartMember(part.member ?? 0);
}

// Which energy figure a cell shows: the whole draw (energy_wh) or the draw
// above each host's idle draw (energy_marginal_wh). Cost is priced on the
// whole draw, so only the total figure lists it.
export type EnergyFigure = 'total' | 'marginal';

type FigureView = {
  row: (row: UsageEvent) => number | undefined;
  part: (part: UsageEnergyPart) => number;
  withCost: boolean;
  title: (t: Translation) => string;
  label: (t: Translation, wh: string) => string;
  testId: string;
};

const FIGURES: Record<EnergyFigure, FigureView> = {
  total: {
    row: (row) => row.energy_wh,
    part: (part) => part.energy_wh,
    withCost: true,
    title: (t) => t.activityEnergyPartsTitle,
    label: (t, wh) => t.activityEnergyPartsLabel(wh),
    testId: 'energy-parts',
  },
  marginal: {
    row: (row) => row.energy_marginal_wh,
    part: (part) => part.marginal_wh,
    withCost: false,
    title: (t) => t.activityEnergyMarginalPartsTitle,
    label: (t, wh) => t.activityEnergyMarginalPartsLabel(wh),
    testId: 'energy-marginal-parts',
  },
};

/**
 * An Activity table energy cell (figure: the energy or the marginal energy
 * column). A llama.cpp RPC cluster request carries the energy of each host
 * that served it; its figure is a focusable, named button that opens a
 * tooltip listing every host with that figure, its source (the same chip and
 * help as the energy-source column) and, for the total, its cost. Any other
 * row is the plain figure.
 */
export function EnergyPartsCell({
  row,
  figure,
  t,
  costUnit,
  currencyFactor,
}: Readonly<{
  row: UsageEvent;
  figure: EnergyFigure;
  t: Translation;
  costUnit: CurrencyUnit;
  currencyFactor: number;
}>) {
  const view = FIGURES[figure];
  const value = view.row(row);
  const total = value ? whText(value) : '—';
  const parts = row.energy_parts ?? [];
  if (parts.length === 0) return total;
  const costText = (part: UsageEnergyPart): string =>
    view.withCost && part.cost !== undefined
      ? ` · ${formatCost(part.cost, costUnit, currencyFactor)}`
      : '';
  const title = (
    <Box sx={{ display: 'grid', gap: 0.5 }}>
      <Typography variant="caption" sx={{ fontWeight: 600 }}>
        {view.title(t)}
      </Typography>
      {parts.map((part, i) => (
        <Box
          key={`${part.role}-${part.member ?? 0}-${i}`}
          sx={{ display: 'flex', gap: 0.5, alignItems: 'center' }}
        >
          <Typography variant="caption">{`${partLabel(part, t)}: ${whText(view.part(part))} Wh${costText(part)}`}</Typography>
          {part.energy_source && chipWithHelp(part.energy_source, ENERGY_SOURCE_HELP_KEYS, t)}
        </Box>
      ))}
    </Box>
  );
  return (
    <Tooltip title={title} describeChild>
      <Typography
        component="span"
        variant="body2"
        tabIndex={0}
        role="button"
        aria-label={view.label(t, total)}
        data-testid={`${view.testId}-${row.id}`}
        sx={{ textDecoration: 'underline dotted', cursor: 'help' }}
      >
        {total}
      </Typography>
    </Tooltip>
  );
}
