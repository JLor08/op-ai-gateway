// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { createContext, useContext } from 'react';

import type { ColorModePref } from './useColorMode';
import type { Brand } from './tokens';

export type ThemeControls = {
  activeThemeId: string;
  setActiveThemeId: (id: string) => void;
  reloadTheme: () => void; // re-fetch GET /api/system/theme and apply
  pref: ColorModePref;
  effective: 'light' | 'dark';
  setMode: (p: ColorModePref) => void;
  toggle: () => void;
  hasDark: boolean; // whether the active theme offers dark
  brand: Brand;
  productName: string; // full product name for the active theme (e.g. "Skynet AI Gateway")
};

export const ThemeControlsContext = createContext<ThemeControls | null>(null);

export function useThemeControls(): ThemeControls {
  const ctx = useContext(ThemeControlsContext);
  if (!ctx) throw new Error('useThemeControls must be used within ThemeRoot');
  return ctx;
}
