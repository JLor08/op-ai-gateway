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

/**
 * The Activity table's energy cell. A llama.cpp RPC cluster request carries
 * the energy of each host that served it; its total is a focusable, named
 * button that opens a tooltip listing every host with its watt-hours, its
 * source (the same chip and help as the energy-source column) and its cost.
 * Any other row is the plain figure.
 */
export function EnergyPartsCell({
  row,
  t,
  costUnit,
  currencyFactor,
}: Readonly<{
  row: UsageEvent;
  t: Translation;
  costUnit: CurrencyUnit;
  currencyFactor: number;
}>) {
  const total = row.energy_wh ? whText(row.energy_wh) : '—';
  const parts = row.energy_parts ?? [];
  if (parts.length === 0) return total;
  const title = (
    <Box sx={{ display: 'grid', gap: 0.5 }}>
      <Typography variant="caption" sx={{ fontWeight: 600 }}>
        {t.activityEnergyPartsTitle}
      </Typography>
      {parts.map((part, i) => {
        const cost =
          part.cost === undefined ? '' : ` · ${formatCost(part.cost, costUnit, currencyFactor)}`;
        return (
          <Box
            key={`${part.role}-${part.member ?? 0}-${i}`}
            sx={{ display: 'flex', gap: 0.5, alignItems: 'center' }}
          >
            <Typography variant="caption">{`${partLabel(part, t)}: ${whText(part.energy_wh)} Wh${cost}`}</Typography>
            {part.energy_source && chipWithHelp(part.energy_source, ENERGY_SOURCE_HELP_KEYS, t)}
          </Box>
        );
      })}
    </Box>
  );
  return (
    <Tooltip title={title} describeChild>
      <Typography
        component="span"
        variant="body2"
        tabIndex={0}
        role="button"
        aria-label={t.activityEnergyPartsLabel(total)}
        data-testid={`energy-parts-${row.id}`}
        sx={{ textDecoration: 'underline dotted', cursor: 'help' }}
      >
        {total}
      </Typography>
    </Tooltip>
  );
}
