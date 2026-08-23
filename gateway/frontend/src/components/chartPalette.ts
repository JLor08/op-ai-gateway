// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// A large, THEME-INDEPENDENT categorical palette for multi-series charts (per-GPU
// and per-CPU-core lines). Fixed hex colors so lines stay distinct and stable
// regardless of the active theme. Beyond the base list, colors are generated via
// golden-angle HSL rotation so any number of series (e.g. a 128-core CPU) still
// gets distinguishable hues without ever running out.

// 32 hand-picked high-contrast colors (Tableau/D3/ColorBrewer categoricals).
const BASE = [
  '#4e79a7',
  '#f28e2b',
  '#e15759',
  '#76b7b2',
  '#59a14f',
  '#edc948',
  '#b07aa1',
  '#ff9da7',
  '#9c755f',
  '#bab0ac',
  '#1f77b4',
  '#ff7f0e',
  '#2ca02c',
  '#d62728',
  '#9467bd',
  '#8c564b',
  '#e377c2',
  '#7f7f7f',
  '#bcbd22',
  '#17becf',
  '#a6cee3',
  '#fb9a99',
  '#fdbf6f',
  '#cab2d6',
  '#b15928',
  '#6a3d9a',
  '#33a02c',
  '#e31a1c',
  '#ff7f00',
  '#00b3b3',
  '#c71585',
  '#1b9e77',
] as const;

export const chartPaletteSize = BASE.length;

// chartColor returns a stable, distinct color for series index i. i < 32 uses the
// curated base palette; beyond that, golden-angle (137.508°) hue rotation keeps
// successive hues maximally separated for arbitrarily many series.
export function chartColor(i: number): string {
  if (!Number.isFinite(i) || i < 0) i = 0;
  i = Math.floor(i);
  if (i < BASE.length) return BASE[i];
  const hue = (i * 137.508) % 360;
  return `hsl(${hue.toFixed(1)}, 68%, 52%)`;
}
