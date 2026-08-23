// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { ExternalThemeData } from '../api';
import { DEFAULT_THEME, MATRIX_THEME, SKYNET_THEME } from './tokens';
import type { Brand, PortalTheme, ThemeMode, ThemeTokens } from './tokens';

const themes: Record<string, PortalTheme> = {
  default: DEFAULT_THEME,
  matrix: MATRIX_THEME,
  skynet: SKYNET_THEME,
};

/** The compiled-in theme ids — everything NOT loaded from an external theme.json. */
export const BUILTIN_IDS: readonly string[] = Object.keys(themes);

/** All theme ids that are currently registered. */
export function availableThemeIds(): string[] {
  return Object.keys(themes);
}

/** Whether the given theme object provides a dark variant. Pure helper. */
export function themeHasDarkOf(theme: PortalTheme): boolean {
  return !!theme.dark;
}

/** Pick the tokens for a mode, falling back to light when no dark exists. Pure helper. */
export function pickTokens(theme: PortalTheme, mode: ThemeMode): ThemeTokens {
  if (mode === 'dark' && theme.dark) return theme.dark;
  return theme.light;
}

/** Whether the registered theme (or the default fallback) provides a dark variant. */
export function themeHasDark(id: string): boolean {
  return themeHasDarkOf(themes[id] ?? themes.default);
}

/** Resolve the tokens for a registered theme id and mode, falling back to the default theme. */
export function resolveTheme(id: string, mode: ThemeMode): ThemeTokens {
  return pickTokens(themes[id] ?? themes.default, mode);
}

/** Resolve the brand descriptor for a registered theme id, falling back to the default. */
export function resolveBrand(id: string): Brand {
  return (themes[id] ?? themes.default).brand;
}

/** Resolve the app-wide font family for a theme id, or undefined to keep the MUI default. */
export function resolveFont(id: string): string | undefined {
  return themes[id]?.font;
}

/** Resolve the full product name for a theme id, falling back to the default. */
export function resolveProductName(id: string): string {
  return (themes[id] ?? themes.default).productName;
}

/**
 * Resolve the browser-tab favicon URL for a theme id (a REAL per-theme PNG file
 * under the app base, e.g. `/portal/favicon-default.png`), falling back to the
 * default. A real file — not a `data:` URI — so Safari macOS renders it.
 */
export function resolveFavicon(id: string): string {
  return `${import.meta.env.BASE_URL}${(themes[id] ?? themes.default).favicon}`;
}

// --- External theme data (operator-deployed theme.json, see task-6 brief) ---
//
// An external theme is pure data served by the backend as `getPublicTheme()`'s
// `data` field (see `../api`'s `ExternalThemeData`); it is never compiled into
// this bundle. The helpers below turn that wire payload into the same shapes
// the compiled registry above produces, so `ThemeRoot` can treat a `source:
// "external"` response uniformly with a built-in one.
//
// `light`/`dark` on the wire payload are PARTIAL token maps (the backend only
// ever sends the keys an operator's theme.json actually set); merging them
// OVER the Default theme's tokens means an external theme that only overrides
// a handful of colors still gets a fully-populated, visually coherent token
// set for everything it didn't mention.

/** Merge an external theme's light-mode token overrides over the Default light tokens. */
function externalLightTokens(data: ExternalThemeData): ThemeTokens {
  return { ...DEFAULT_THEME.light, ...data.light };
}

/**
 * Merge an external theme's dark-mode token overrides over the Default dark
 * tokens, or undefined if the theme didn't supply a dark block (mirrors a
 * built-in dark-less theme: dark mode falls back to light).
 */
function externalDarkTokens(data: ExternalThemeData): ThemeTokens | undefined {
  if (!data.dark || Object.keys(data.dark).length === 0) return undefined;
  return { ...(DEFAULT_THEME.dark as ThemeTokens), ...data.dark };
}

/** Whether an external theme supplies a dark variant at all. */
export function externalHasDark(data: ExternalThemeData): boolean {
  return externalDarkTokens(data) !== undefined;
}

/** Resolve the tokens for an external theme and mode, falling back to light. */
export function externalToTokens(data: ExternalThemeData, mode: ThemeMode): ThemeTokens {
  const dark = mode === 'dark' ? externalDarkTokens(data) : undefined;
  return dark ?? externalLightTokens(data);
}

/**
 * Resolve the brand descriptor for an external theme. A `"image"` brand
 * renders the operator-supplied logo served by the backend at an ABSOLUTE
 * `/api/...` URL (never under the SPA base — the asset endpoint isn't part
 * of the SPA) — but ONLY when the theme actually shipped a logo file
 * (`data.hasLogo`). A theme.json can declare `brand.type:"image"` without
 * ever including `logo.svg`/`logo.png`; without this check that would render
 * a permanently broken `<img>` in the header for every visitor, with no
 * recovery short of an admin editing the theme package on disk. So an
 * image brand with no logo file falls back to the text wordmark instead
 * (`data.brand.text`, or the theme's display `name` if that's empty).
 */
export function externalBrand(id: string, data: ExternalThemeData): Brand {
  const mark =
    data.brand.type === 'image' && data.hasLogo
      ? { type: 'image' as const, url: `/api/system/themes/${id}/logo` }
      : { type: 'text' as const, text: data.brand.text || data.name };
  return { mark, title: data.brand.title };
}

/** Resolve the app-wide font family for an external theme, or undefined to keep the MUI default. */
export function externalFont(data: ExternalThemeData): string | undefined {
  return data.font || undefined;
}

/** Resolve the full product name for an external theme. */
export function externalProductName(data: ExternalThemeData): string {
  return data.productName;
}

/**
 * Resolve the browser-tab favicon URL for an external theme: the backend's
 * absolute asset endpoint when the theme shipped a favicon, else the
 * compiled-in Default theme's favicon.
 */
export function externalFavicon(id: string, data: ExternalThemeData): string {
  return data.hasFavicon ? `/api/system/themes/${id}/favicon` : resolveFavicon('default');
}
