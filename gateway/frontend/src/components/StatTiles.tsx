// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { Box } from '@mui/material';
import type { StatTotals } from '../api';
import { formatCost, type CurrencyUnit } from '../currency';
import type { Translation } from './shared/types';
import { StatTile } from './shared/StatTile';
import type { TileId } from './activityTiles';

// Watt-hours -> a compact display string: kWh above the 1000 Wh threshold (2
// decimals), else Wh (1 decimal). Pure, exported for unit testing.
export function formatEnergyWh(wh: number): string {
  if (wh >= 1000) return `${(wh / 1000).toFixed(2)} kWh`;
  return `${wh.toFixed(1)} Wh`;
}

export function StatTiles({
  t,
  totals,
  order,
  hidden,
  runningCount,
  costUnit,
  currencyFactor,
}: Readonly<{
  t: Translation;
  totals: StatTotals;
  // Tile order + hidden ids (profile-persisted; see Activity.tsx). Only tiles in
  // `order` and NOT in `hidden` render, in that order.
  order: TileId[];
  hidden: TileId[];
  /** When set, the running-connections tile can render (scope-aware); skipped otherwise. */
  runningCount?: number;
  // Cost-unit display, driven by the Activity toolbar selector (mirrors the
  // ActivityTable cost_eur column): which currency unit the cost tile renders
  // in, and the USD-per-EUR factor formatCost needs to convert.
  costUnit: CurrencyUnit;
  currencyFactor: number;
}>) {
  // The value + label for every possible tile; the render loop picks the visible
  // ones in order. Additive P3 T2 tiles: total energy (Wh, or kWh above 1000) and
  // the per-server-price-weighted EUR cost derived from it (both portal-computed;
  // absent/undefined on an older response reads as 0, per the optional API type).
  const byId: Record<TileId, { label: string; value: string }> = {
    running: { label: t.activityActiveTitle, value: String(runningCount ?? 0) },
    total_requests: { label: t.activityTotalRequests, value: String(totals.total_requests) },
    error_count: { label: t.activityErrorCount, value: String(totals.error_count) },
    cached_tokens: { label: t.activityCachedTokens, value: String(totals.cached_tokens) },
    cache_write_tokens: {
      label: t.activityCacheWriteTokens,
      value: String(totals.cache_write_tokens),
    },
    input_tokens: { label: t.activityInputTokens, value: String(totals.input_tokens) },
    output_tokens: { label: t.activityOutputTokens, value: String(totals.output_tokens) },
    energy: { label: t.activityEnergyTile, value: formatEnergyWh(totals.total_energy_wh ?? 0) },
    cost: {
      label: t.activityCostTile,
      value: formatCost(totals.total_cost_eur, costUnit, currencyFactor),
    },
  };
  const visible = order.filter(
    (id) => !hidden.includes(id) && (id !== 'running' || typeof runningCount === 'number'),
  );
  return (
    <Box
      component="section"
      aria-label={t.activityStatsLabel}
      sx={{
        display: 'grid',
        // Wrap across rows by width instead of forcing one fixed-count row, so
        // the (now configurable) tile set stays responsive.
        gridTemplateColumns: 'repeat(auto-fit, minmax(160px, 1fr))',
        gap: 2,
        mb: 3,
      }}
    >
      {visible.map((id) => (
        <StatTile key={id} label={byId[id].label} value={byId[id].value} />
      ))}
    </Box>
  );
}
