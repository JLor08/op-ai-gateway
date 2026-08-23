// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';

import type { ExternalThemeData } from '../api';
import { DEFAULT_THEME, MONOSPACE_STACK } from './tokens';
import type { PortalTheme, ThemeTokens } from './tokens';
import {
  availableThemeIds,
  BUILTIN_IDS,
  externalBrand,
  externalFavicon,
  externalFont,
  externalHasDark,
  externalProductName,
  externalToTokens,
  pickTokens,
  resolveFavicon,
  resolveFont,
  resolveProductName,
  resolveTheme,
  themeHasDark,
  themeHasDarkOf,
} from './registry';

/** A minimal-but-valid external theme payload, as the wire response would shape it. */
function externalFixture(overrides: Partial<ExternalThemeData> = {}): ExternalThemeData {
  return {
    id: 'acme',
    name: 'Acme',
    productName: 'Acme AI Gateway',
    font: '',
    brand: { type: 'text', text: 'Acme', title: 'AI Gateway' },
    hasFavicon: false,
    hasLogo: false,
    ...overrides,
  };
}

const darklessFixture: PortalTheme = {
  id: 'x',
  name: 'X',
  brand: { mark: { type: 'text', text: 'X' }, title: 'Gateway' },
  productName: 'X Gateway',
  favicon: 'favicon-x.png',
  light: DEFAULT_THEME.light,
};

describe('resolveTheme', () => {
  it('returns the default light tokens', () => {
    const tokens = resolveTheme('default', 'light');

    expect(tokens.mode).toBe('light');
    expect(tokens.surface).toBe('#ffffff');
    expect(tokens.brandAccent).toBe('#0d9488');
  });

  it('returns the default dark tokens', () => {
    const tokens = resolveTheme('default', 'dark');

    expect(tokens.mode).toBe('dark');
    expect(tokens.surface).toBe('#292524');
    expect(tokens.brandAccent).toBe('#2dd4bf');
  });

  it('falls back to the default theme for an unknown id', () => {
    const tokens = resolveTheme('does-not-exist', 'light');

    expect(tokens).toEqual(DEFAULT_THEME.light);
  });

  it('returns the matrix tokens in dark mode', () => {
    const tokens = resolveTheme('matrix', 'light');
    expect(tokens.mode).toBe('dark');
    expect(tokens.brandPrimary).toBe('#00ff66');
    expect(tokens.page).toBe('#000000');
  });

  it('returns the skynet tokens in dark mode (red-on-black)', () => {
    const tokens = resolveTheme('skynet', 'light');
    expect(tokens.mode).toBe('dark');
    expect(tokens.brandPrimary).toBe('#ff2a2a');
    expect(tokens.page).toBe('#000000');
  });

  // A multi-line chart colours its second series with chartSeries2; it must be
  // distinct from brandPrimary in every theme/mode, since brandAccent equals
  // brandPrimary in the Default and Matrix palettes (the two lines would coincide).
  it.each([
    ['default', 'light'],
    ['default', 'dark'],
    ['matrix', 'light'],
    ['skynet', 'light'],
  ] as const)(
    'gives %s (%s) a chart second-series colour distinct from the primary',
    (id, mode) => {
      const tokens = resolveTheme(id, mode);
      expect(tokens.chartSeries2).toBeTruthy();
      expect(tokens.chartSeries2).not.toBe(tokens.brandPrimary);
    },
  );
});

describe('pickTokens', () => {
  it('returns light tokens when a theme has no dark variant', () => {
    const tokens: ThemeTokens = pickTokens(darklessFixture, 'dark');

    expect(tokens).toEqual(DEFAULT_THEME.light);
    expect(tokens.mode).toBe('light');
  });
});

describe('themeHasDark', () => {
  it('is true for the default theme', () => {
    expect(themeHasDark('default')).toBe(true);
  });

  it('falls back to the default theme (which has dark) for an unknown id', () => {
    expect(themeHasDark('does-not-exist')).toBe(true);
  });

  it('is false for the matrix theme (single dark-styled slot)', () => {
    expect(themeHasDark('matrix')).toBe(false);
  });
});

describe('themeHasDarkOf', () => {
  it('is true for a theme with a dark variant', () => {
    expect(themeHasDarkOf(DEFAULT_THEME)).toBe(true);
  });

  it('is false for a dark-less theme', () => {
    expect(themeHasDarkOf(darklessFixture)).toBe(false);
  });
});

describe('availableThemeIds', () => {
  it('includes the default theme id', () => {
    expect(availableThemeIds()).toContain('default');
  });

  it('includes the matrix theme id', () => {
    expect(availableThemeIds()).toContain('matrix');
  });

  it('includes the skynet theme id', () => {
    expect(availableThemeIds()).toContain('skynet');
  });

  it('no longer includes cgi (dropped in favor of external themes)', () => {
    expect(availableThemeIds()).not.toContain('cgi');
  });
});

describe('BUILTIN_IDS', () => {
  it('is exactly the compiled theme ids, no cgi', () => {
    expect(new Set(BUILTIN_IDS)).toEqual(new Set(['default', 'matrix', 'skynet']));
  });

  it('matches availableThemeIds', () => {
    expect(new Set(BUILTIN_IDS)).toEqual(new Set(availableThemeIds()));
  });
});

describe('resolveProductName', () => {
  it('returns the default product name', () => {
    expect(resolveProductName('default')).toBe('On-Prem AI Gateway');
  });

  it('renames the product for the skynet theme', () => {
    expect(resolveProductName('skynet')).toBe('Skynet AI Gateway');
  });

  it('falls back to the default product name for an unknown id', () => {
    expect(resolveProductName('does-not-exist')).toBe('On-Prem AI Gateway');
  });
});

describe('resolveFavicon', () => {
  it('returns a real per-theme PNG file URL for the default theme', () => {
    const favicon = resolveFavicon('default');
    expect(favicon).toMatch(/favicon-default\.png$/);
    expect(favicon).not.toMatch(/^data:/);
  });

  it('falls back to the default favicon for an unknown id', () => {
    expect(resolveFavicon('does-not-exist')).toBe(resolveFavicon('default'));
  });

  it.each(['default', 'matrix', 'skynet'] as const)(
    'gives %s a distinct favicon from the other themes',
    (id) => {
      const favicon = resolveFavicon(id);
      const others = (['default', 'matrix', 'skynet'] as const).filter((other) => other !== id);
      for (const other of others) {
        expect(favicon).not.toBe(resolveFavicon(other));
      }
    },
  );
});

describe('resolveFont', () => {
  it('returns the monospace stack for matrix', () => {
    expect(resolveFont('matrix')).toBe(MONOSPACE_STACK);
  });
  it('returns the monospace stack for skynet', () => {
    expect(resolveFont('skynet')).toBe(MONOSPACE_STACK);
  });
  it('returns undefined for themes without a font override', () => {
    expect(resolveFont('default')).toBeUndefined();
  });
});

// --- External theme helpers (operator-deployed theme.json data) ---

describe('externalToTokens', () => {
  it('merges partial light overrides over the Default light tokens', () => {
    const data = externalFixture({ light: { brandPrimary: '#123456' } });

    const tokens = externalToTokens(data, 'light');

    expect(tokens.mode).toBe('light');
    expect(tokens.brandPrimary).toBe('#123456');
    // Every key the theme didn't set is inherited from Default light.
    expect(tokens.surface).toBe(DEFAULT_THEME.light.surface);
    expect(tokens.page).toBe(DEFAULT_THEME.light.page);
  });

  it('merges partial dark overrides over the Default dark tokens', () => {
    const data = externalFixture({ dark: { brandPrimary: '#abcdef' } });

    const tokens = externalToTokens(data, 'dark');

    expect(tokens.mode).toBe('dark');
    expect(tokens.brandPrimary).toBe('#abcdef');
    expect(tokens.surface).toBe(DEFAULT_THEME.dark!.surface);
  });

  it('falls back to light when dark mode is requested but the theme has no dark block', () => {
    const data = externalFixture({ light: { brandPrimary: '#123456' } });

    const tokens = externalToTokens(data, 'dark');

    expect(tokens.mode).toBe('light');
    expect(tokens.brandPrimary).toBe('#123456');
  });

  it('returns the plain Default tokens when the theme sets no overrides at all', () => {
    const data = externalFixture();

    expect(externalToTokens(data, 'light')).toEqual(DEFAULT_THEME.light);
  });
});

describe('externalHasDark', () => {
  it('is false when the theme supplies no dark block', () => {
    expect(externalHasDark(externalFixture())).toBe(false);
  });

  it('is true when the theme supplies a non-empty dark block', () => {
    expect(externalHasDark(externalFixture({ dark: { brandPrimary: '#abcdef' } }))).toBe(true);
  });
});

describe('externalBrand', () => {
  it("resolves a text brand mark from the theme's brand text/title", () => {
    const data = externalFixture({ brand: { type: 'text', text: 'Acme', title: 'Cloud' } });

    expect(externalBrand('acme', data)).toEqual({
      mark: { type: 'text', text: 'Acme' },
      title: 'Cloud',
    });
  });

  it('resolves an image brand mark to the absolute backend logo endpoint when a logo file was shipped', () => {
    const data = externalFixture({
      brand: { type: 'image', text: '', title: 'Cloud' },
      hasLogo: true,
    });

    expect(externalBrand('acme', data)).toEqual({
      mark: { type: 'image', url: '/api/system/themes/acme/logo' },
      title: 'Cloud',
    });
  });

  // A theme.json can declare brand.type:"image" without ever shipping a
  // logo.svg/logo.png. Rendering the image mark anyway would produce a
  // permanently broken <img> for every visitor with no recovery short of an
  // admin editing the theme package on disk — so this MUST fall back to text.
  it('falls back to the text mark when an image brand has no logo file (hasLogo: false)', () => {
    const data = externalFixture({
      brand: { type: 'image', text: 'Acme', title: 'Cloud' },
      hasLogo: false,
    });

    expect(externalBrand('acme', data)).toEqual({
      mark: { type: 'text', text: 'Acme' },
      title: 'Cloud',
    });
  });

  it("falls back to the theme's display name when the image brand's text is also empty", () => {
    const data = externalFixture({
      name: 'Acme Corp',
      brand: { type: 'image', text: '', title: 'Cloud' },
      hasLogo: false,
    });

    expect(externalBrand('acme', data)).toEqual({
      mark: { type: 'text', text: 'Acme Corp' },
      title: 'Cloud',
    });
  });

  it("falls back to the display name when a text brand's own text is empty", () => {
    const data = externalFixture({
      name: 'Acme Corp',
      brand: { type: 'text', text: '', title: 'Cloud' },
    });

    expect(externalBrand('acme', data)).toEqual({
      mark: { type: 'text', text: 'Acme Corp' },
      title: 'Cloud',
    });
  });
});

describe('externalFont', () => {
  it('returns undefined for an empty font string', () => {
    expect(externalFont(externalFixture({ font: '' }))).toBeUndefined();
  });

  it("returns the theme's font when set", () => {
    expect(externalFont(externalFixture({ font: MONOSPACE_STACK }))).toBe(MONOSPACE_STACK);
  });
});

describe('externalProductName', () => {
  it("returns the theme's product name", () => {
    expect(externalProductName(externalFixture({ productName: 'Acme AI Gateway' }))).toBe(
      'Acme AI Gateway',
    );
  });
});

describe('externalFavicon', () => {
  it('resolves to the absolute backend favicon endpoint when the theme has one', () => {
    expect(externalFavicon('acme', externalFixture({ hasFavicon: true }))).toBe(
      '/api/system/themes/acme/favicon',
    );
  });

  it('falls back to the compiled Default favicon when the theme has none', () => {
    expect(externalFavicon('acme', externalFixture({ hasFavicon: false }))).toBe(
      resolveFavicon('default'),
    );
  });
});
