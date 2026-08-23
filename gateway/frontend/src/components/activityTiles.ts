// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Catalogue of the Activity stat tiles. Mirrors activityColumns.ts (the table
// column catalogue): a fixed list of tile ids + their i18n label key + whether
// they are visible by default. The declaration order IS the default order.
// Tile VISIBILITY + ORDER are per-user profile preferences (see Activity.tsx),
// exactly like the table columns; Cache-Write is the only tile hidden by default.

export type TileId =
  | 'running'
  | 'total_requests'
  | 'error_count'
  | 'cached_tokens'
  | 'cache_write_tokens'
  | 'input_tokens'
  | 'output_tokens'
  | 'energy'
  | 'cost';

export type TileDef = { id: TileId; labelKey: string; defaultVisible: boolean };

export const ACTIVITY_TILES: TileDef[] = [
  { id: 'running', labelKey: 'activityActiveTitle', defaultVisible: true },
  { id: 'total_requests', labelKey: 'activityTotalRequests', defaultVisible: true },
  { id: 'error_count', labelKey: 'activityErrorCount', defaultVisible: true },
  { id: 'cached_tokens', labelKey: 'activityCachedTokens', defaultVisible: true },
  { id: 'cache_write_tokens', labelKey: 'activityCacheWriteTokens', defaultVisible: false },
  { id: 'input_tokens', labelKey: 'activityInputTokens', defaultVisible: true },
  { id: 'output_tokens', labelKey: 'activityOutputTokens', defaultVisible: true },
  { id: 'energy', labelKey: 'activityEnergyTile', defaultVisible: true },
  { id: 'cost', labelKey: 'activityCostTile', defaultVisible: true },
];

// Stable module-level defaults for useColumnSettings (an inline literal would
// churn identity every render and loop the memo/effect that reconciles the
// order). The unknown-id sanitizer that used to live here (reconcileHiddenTiles)
// is now the generic reconcileHiddenIds in shared/useColumnSettings.ts.
export const DEFAULT_TILE_ORDER: TileId[] = ACTIVITY_TILES.map((x) => x.id);
export const DEFAULT_HIDDEN_TILES: TileId[] = ACTIVITY_TILES.filter((x) => !x.defaultVisible).map(
  (x) => x.id,
);
