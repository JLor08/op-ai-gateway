// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import { chartColor, chartPaletteSize } from './chartPalette';

describe('chartColor', () => {
  it('returns stable base-palette colors for the first indices', () => {
    const a = chartColor(0);
    expect(a).toMatch(/^#[0-9a-f]{6}$/i);
    expect(chartColor(0)).toBe(a); // stable
    expect(chartColor(1)).not.toBe(chartColor(0)); // distinct
  });

  it('falls back to golden-angle HSL beyond the base palette (never runs out)', () => {
    const c = chartColor(chartPaletteSize);
    expect(c).toMatch(/^hsl\(/);
    // A 128-core CPU still gets a color, and adjacent hues differ.
    expect(chartColor(127)).toMatch(/^hsl\(/);
    expect(chartColor(chartPaletteSize)).not.toBe(chartColor(chartPaletteSize + 1));
  });

  it('guards negative / non-finite indices', () => {
    expect(chartColor(-5)).toBe(chartColor(0));
    expect(chartColor(NaN)).toBe(chartColor(0));
  });
});
