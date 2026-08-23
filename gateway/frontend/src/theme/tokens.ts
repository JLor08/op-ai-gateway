// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

export type ThemeMode = 'light' | 'dark';

export type BrandMark =
  { type: 'text'; text: string } | { type: 'logo'; id: 'matrix' } | { type: 'image'; url: string };
export type Brand = { mark: BrandMark; title: string };

export const MONOSPACE_STACK =
  'ui-monospace, SFMono-Regular, Menlo, Consolas, "Liberation Mono", monospace';

export type ThemeTokens = {
  mode: ThemeMode;
  surface: string;
  page: string;
  text: string;
  muted: string;
  line: string;
  brandAccent: string;
  brandPrimary: string;
  // Second categorical series color for multi-line charts. Distinct from
  // brandPrimary in EVERY theme (brandAccent equals brandPrimary in the
  // Default/Matrix palettes, so a chart's two lines would otherwise coincide).
  chartSeries2: string;
  sidebar: string;
  sidebarActive: string;
  successBg: string;
  successText: string;
  watchBg: string;
  watchText: string;
  standbyBg: string;
  standbyText: string;
  header: string;
  headerText: string;
  navText: string;
  navActiveText: string;
  accentText: string;
  accentSoft: string;
};

export type PortalTheme = {
  id: string;
  name: string;
  brand: Brand;
  /** Full product name, e.g. "On-Prem AI Gateway". Drives the brand aria-label
      and the Dashboard welcome line, so a theme can rename the product (Skynet). */
  productName: string;
  /** Browser-tab icon: the filename of a REAL per-theme PNG file shipped in
      `public/` (served at `<base>/<favicon>`), so switching themes swaps the
      favicon. A real file — not a `data:` URI — because Safari macOS does not
      render `data:` URI favicons. Rebuild the PNGs via the harness in
      `docs/` if the glyphs/colors change. */
  favicon: string;
  font?: string;
  light: ThemeTokens;
  dark?: ThemeTokens;
};

const defaultLight: ThemeTokens = {
  mode: 'light',
  surface: '#ffffff',
  page: '#fafaf9',
  text: '#1c1917',
  muted: '#78716c',
  line: '#e7e5e4',
  brandAccent: '#0d9488',
  brandPrimary: '#0d9488',
  chartSeries2: '#d97706', // amber — complements the teal primary on light surfaces
  sidebar: '#f5f5f4',
  sidebarActive: '#f0fdfa',
  successBg: '#d8f3e8',
  successText: '#116149',
  watchBg: '#f0fdfa',
  watchText: '#0d9488',
  standbyBg: '#eef0f4',
  standbyText: '#4a515c',
  header: '#292524',
  headerText: '#e7e5e4',
  navText: '#78716c',
  navActiveText: '#0d9488',
  accentText: '#ffffff',
  accentSoft: '#f0fdfa',
};

const defaultDark: ThemeTokens = {
  mode: 'dark',
  surface: '#292524',
  page: '#1c1917',
  text: '#f5f5f4',
  muted: '#a8a29e',
  line: '#44403c',
  brandAccent: '#2dd4bf',
  brandPrimary: '#2dd4bf',
  chartSeries2: '#fbbf24', // amber (brighter for dark surfaces)
  sidebar: '#292524',
  sidebarActive: '#134e4a',
  successBg: '#0f3d31',
  successText: '#5eead4',
  watchBg: '#134e4a',
  watchText: '#5eead4',
  standbyBg: '#3a3633',
  standbyText: '#d6d3d1',
  header: '#0c0a09',
  headerText: '#e7e5e4',
  navText: '#a8a29e',
  navActiveText: '#5eead4',
  accentText: '#0c2a26',
  accentSoft: '#134e4a',
};

export const DEFAULT_THEME: PortalTheme = {
  id: 'default',
  name: 'Default',
  brand: { mark: { type: 'text', text: 'On-Prem' }, title: 'AI Gateway' },
  productName: 'On-Prem AI Gateway',
  favicon: 'favicon-default.png',
  light: defaultLight,
  dark: defaultDark,
};

const matrixTokens: ThemeTokens = {
  mode: 'dark',
  surface: 'rgba(10, 22, 12, 0.85)',
  page: '#000000',
  text: '#cfffd8',
  muted: '#5fae74',
  line: '#14351f',
  brandAccent: '#00ff66',
  brandPrimary: '#00ff66',
  chartSeries2: '#38bdf8', // cyan — reads clearly against the green-on-black matrix
  sidebar: 'rgba(6, 14, 8, 0.9)',
  sidebarActive: 'rgba(0, 60, 20, 0.5)',
  successBg: 'rgba(0, 60, 20, 0.5)',
  successText: '#7bffa6',
  watchBg: 'rgba(0, 60, 20, 0.35)',
  watchText: '#00ff66',
  standbyBg: 'rgba(20, 40, 26, 0.6)',
  standbyText: '#9fd7ac',
  header: 'rgba(0, 0, 0, 0.82)',
  headerText: '#cfffd8',
  navText: '#7bd69a',
  navActiveText: '#00ff66',
  accentText: '#001a08',
  accentSoft: 'rgba(0, 60, 20, 0.35)',
};

export const MATRIX_THEME: PortalTheme = {
  id: 'matrix',
  name: 'Matrix',
  brand: { mark: { type: 'logo', id: 'matrix' }, title: 'AI Gateway' },
  productName: 'On-Prem AI Gateway',
  favicon: 'favicon-matrix.png',
  font: MONOSPACE_STACK,
  light: matrixTokens,
};

// Terminator-inspired "Skynet" theme: a red-on-black HUD, monospace, dark-only.
// Like Matrix, the single styled slot lives under `light` with mode:"dark" and
// no `dark` variant. It renames the product to "Skynet AI Gateway" via both the
// text brand mark ("Skynet" + "AI Gateway") and `productName`.
const skynetTokens: ThemeTokens = {
  mode: 'dark',
  surface: '#160b0b',
  page: '#000000',
  text: '#ff6b6b',
  muted: '#a85252',
  line: '#3a1414',
  brandAccent: '#ff2a2a',
  brandPrimary: '#ff2a2a',
  chartSeries2: '#ff9d2a', // HUD amber — distinct from the red primary on black
  sidebar: '#0a0505',
  sidebarActive: 'rgba(255, 42, 42, 0.16)',
  successBg: 'rgba(46, 160, 87, 0.22)',
  successText: '#5eff9b', // "target acquired" green reads clearly against the red HUD
  watchBg: 'rgba(255, 157, 42, 0.18)',
  watchText: '#ffb14d',
  standbyBg: 'rgba(120, 28, 28, 0.5)',
  standbyText: '#ff9a9a',
  header: '#000000',
  headerText: '#ff6b6b',
  navText: '#a85252',
  navActiveText: '#ff2a2a',
  accentText: '#0a0000',
  accentSoft: 'rgba(255, 42, 42, 0.16)',
};

export const SKYNET_THEME: PortalTheme = {
  id: 'skynet',
  name: 'Skynet',
  brand: { mark: { type: 'text', text: 'Skynet' }, title: 'AI Gateway' },
  productName: 'Skynet AI Gateway',
  favicon: 'favicon-skynet.png',
  font: MONOSPACE_STACK,
  light: skynetTokens,
};
