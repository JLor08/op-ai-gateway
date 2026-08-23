// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { CssBaseline, GlobalStyles, ThemeProvider, createTheme } from '@mui/material';
import { useCallback, useEffect, useMemo, useState } from 'react';
import type { ReactNode } from 'react';

import { createPortalApi } from '../api';
import type { ExternalThemeData } from '../api';
import { ToastProvider } from '../components/shared/ToastProvider';
import { MatrixRain } from './MatrixRain';
import {
  externalBrand,
  externalFavicon,
  externalFont,
  externalHasDark,
  externalProductName,
  externalToTokens,
  resolveBrand,
  resolveFavicon,
  resolveFont,
  resolveProductName,
  resolveTheme,
  themeHasDark,
} from './registry';
import type { ThemeTokens } from './tokens';
import { useColorMode } from './useColorMode';
import { ThemeControlsContext } from './useThemeControls';

/**
 * The theme actually in effect: either a compiled built-in (looked up by id
 * in the registry) or an operator-deployed external theme whose full data
 * payload (see `ExternalThemeData`) rode along on the same pre-auth
 * `getPublicTheme()` response that named it.
 */
type ActiveTheme = { id: string; source: 'builtin' | 'external'; data: ExternalThemeData | null };

/**
 * Maps resolved theme tokens onto the CSS custom-property names that the
 * `sx` `var(--…)` bridge and the `GlobalStyles` fallbacks read. Kept as an
 * explicit table so the runtime bridge and those defaults stay in lockstep.
 */
function cssVariables(tokens: ThemeTokens): Record<string, string> {
  return {
    '--surface': tokens.surface,
    '--page': tokens.page,
    '--text': tokens.text,
    '--muted': tokens.muted,
    '--line': tokens.line,
    '--brand-accent': tokens.brandAccent,
    '--brand-primary': tokens.brandPrimary,
    '--chart-series-2': tokens.chartSeries2,
    '--sidebar': tokens.sidebar,
    '--sidebar-active': tokens.sidebarActive,
    '--success-bg': tokens.successBg,
    '--success-text': tokens.successText,
    '--watch-bg': tokens.watchBg,
    '--watch-text': tokens.watchText,
    '--standby-bg': tokens.standbyBg,
    '--standby-text': tokens.standbyText,
    '--header': tokens.header,
    '--header-text': tokens.headerText,
    '--nav-text': tokens.navText,
    '--nav-active-text': tokens.navActiveText,
    '--accent-text': tokens.accentText,
    '--accent-soft': tokens.accentSoft,
  };
}

/**
 * Applies the active portal theme app-wide: it resolves the theme id fetched
 * from the public endpoint (pre-auth), bridges the resolved tokens onto CSS
 * custom properties on `<html>`, builds the matching MUI theme, and exposes
 * theme/color-mode controls via context.
 */
export function ThemeRoot({ children }: Readonly<{ children: ReactNode }>) {
  const [active, setActive] = useState<ActiveTheme>({
    id: 'default',
    source: 'builtin',
    data: null,
  });
  const activeThemeId = active.id;
  // Non-null only for a genuinely external response; a builtin (or a manual
  // setActiveThemeId switch, which always targets a compiled theme) leaves
  // this null so every resolver below falls through to the compiled registry.
  const externalData = active.source === 'external' ? active.data : null;

  // setActiveThemeId is exposed via context for switching among COMPILED
  // themes only (e.g. tests driving a theme switch without a real fetch);
  // the real external-theme path only ever arrives via reloadTheme's fetch.
  const setActiveThemeId = useCallback((id: string) => {
    setActive({ id, source: 'builtin', data: null });
  }, []);

  const hasDark = externalData ? externalHasDark(externalData) : themeHasDark(activeThemeId);
  const { pref, effective, setMode, toggle } = useColorMode(hasDark);

  const reloadTheme = useCallback(() => {
    createPortalApi()
      .getPublicTheme()
      .then((r) => setActive({ id: r.theme, source: r.source, data: r.data }))
      .catch(() => {
        /* keep the current (default) theme when the fetch fails */
      });
  }, []);

  useEffect(() => {
    reloadTheme();
  }, [reloadTheme]);

  const tokens = externalData
    ? externalToTokens(externalData, effective)
    : resolveTheme(activeThemeId, effective);
  // Memoized: externalBrand() (unlike the built-in registry's resolveBrand())
  // allocates a fresh object every call, which would otherwise churn the
  // ThemeControlsContext value below on every render regardless of whether
  // the resolved brand actually changed.
  const brand = useMemo(
    () => (externalData ? externalBrand(activeThemeId, externalData) : resolveBrand(activeThemeId)),
    [externalData, activeThemeId],
  );
  const font = externalData ? externalFont(externalData) : resolveFont(activeThemeId);
  const productName = externalData
    ? externalProductName(externalData)
    : resolveProductName(activeThemeId);

  // Keep the browser tab title in sync with the active theme's product name
  // (index.html ships a static fallback for the first paint / no-JS case).
  useEffect(() => {
    document.title = productName;
  }, [productName]);

  // Keep the browser tab favicon in sync with the active theme (index.html
  // ships a static default-theme <link> for the first paint / no-JS case).
  // Find-or-create the <link rel="icon"> so this works even if the static
  // one is absent for some reason.
  useEffect(() => {
    let link = document.head.querySelector<HTMLLinkElement>('link[rel="icon"]');
    if (!link) {
      link = document.createElement('link');
      link.rel = 'icon';
      document.head.appendChild(link);
    }
    // Point the tab icon at the active theme's REAL PNG file: a built-in
    // theme ships it in public/ (served at `<base>/favicon-<theme>.png`); an
    // external theme's own favicon is served by the backend's asset endpoint
    // (`/api/system/themes/{id}/favicon`, an ABSOLUTE path — not under the
    // SPA base), falling back to the compiled Default favicon when the
    // operator didn't supply one. A real file — not a `data:` URI — because
    // Safari macOS does not render `data:` URI favicons; either way the file
    // still swaps per theme, so the icon follows the theme.
    link.type = 'image/png';
    link.href = externalData
      ? externalFavicon(activeThemeId, externalData)
      : resolveFavicon(activeThemeId);
  }, [activeThemeId, externalData]);

  useEffect(() => {
    const root = document.documentElement;
    const vars = cssVariables(tokens);
    for (const [name, value] of Object.entries(vars)) {
      root.style.setProperty(name, value);
    }
    root.dataset.mode = tokens.mode;
    root.style.colorScheme = tokens.mode;
  }, [tokens]);

  const muiTheme = useMemo(
    () =>
      createTheme({
        palette: {
          mode: tokens.mode,
          // primary = the brand's main interactive colour (focus rings, checkboxes,
          // default buttons). secondary = the brand accent, used for the "keep it
          // red" buttons (Zurück/Abbrechen). brandAccent equals brandPrimary in
          // every COMPILED theme, so secondary is only visually distinct when an
          // external theme.json sets the two apart.
          primary: { main: tokens.brandPrimary, contrastText: tokens.accentText },
          secondary: { main: tokens.brandAccent, contrastText: tokens.accentText },
          background: { default: tokens.page, paper: tokens.surface },
          text: { primary: tokens.text, secondary: tokens.muted },
          divider: tokens.line,
        },
        typography: font ? { fontFamily: font } : undefined,
      }),
    [tokens, font],
  );

  // Memoized so a ThemeRoot re-render (e.g. from the muiTheme/tokens effects
  // above) does not, by itself, force every ThemeControlsContext consumer to
  // re-render with a structurally-identical-but-new object.
  const themeControlsValue = useMemo(
    () => ({
      activeThemeId,
      setActiveThemeId,
      reloadTheme,
      pref,
      effective,
      setMode,
      toggle,
      hasDark,
      brand,
      productName,
    }),
    [
      activeThemeId,
      setActiveThemeId,
      reloadTheme,
      pref,
      effective,
      setMode,
      toggle,
      hasDark,
      brand,
      productName,
    ],
  );

  return (
    <ThemeProvider theme={muiTheme}>
      <CssBaseline enableColorScheme />
      {/* Built-in Matrix easter egg only: an external theme id can never
          collide with a compiled one (externalData is set precisely when it
          didn't), but gate on the source too for clarity. */}
      {!externalData && activeThemeId === 'matrix' && <MatrixRain />}
      <GlobalStyles
        styles={{
          ':root': {
            '--brand-accent': '#0d9488',
            '--brand-primary': '#0d9488',
            '--chart-series-2': '#d97706',
            '--text': '#1c1917',
            '--muted': '#78716c',
            '--line': '#e7e5e4',
            '--surface': '#ffffff',
            '--page': '#fafaf9',
            '--sidebar': '#f5f5f4',
            '--sidebar-active': '#f0fdfa',
            '--success-bg': '#d8f3e8',
            '--success-text': '#116149',
            '--watch-bg': '#f0fdfa',
            '--watch-text': '#0d9488',
            '--standby-bg': '#eef0f4',
            '--standby-text': '#4a515c',
            '--header': '#292524',
            '--header-text': '#e7e5e4',
            '--nav-text': '#78716c',
            '--nav-active-text': '#0d9488',
            '--accent-text': '#ffffff',
            '--accent-soft': '#f0fdfa',
          },
          // MUI form controls (TextField / Select / Autocomplete) already render
          // their own brand-coloured focus outline. A second global outline +
          // box-shadow on the inner <input>/<textarea>/<select> double-framed them
          // with an off-brand grey halo (the box-shadow colour was a hard-coded
          // teal, so it looked wrong in every theme). Keep a theme-aware ring only
          // for genuinely non-MUI focusables (e.g. rendered markdown links).
          'button:focus-visible, a:focus-visible': {
            outline: '2px solid var(--brand-primary)',
            outlineOffset: '2px',
          },
        }}
      />
      <ThemeControlsContext.Provider value={themeControlsValue}>
        <ToastProvider>{children}</ToastProvider>
      </ThemeControlsContext.Provider>
    </ThemeProvider>
  );
}
