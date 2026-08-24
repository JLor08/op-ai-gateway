// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, renderHook, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

// Hoisted so the module mock (which Vitest lifts above the imports) can close
// over the same spy we drive from each test.
const { getPublicTheme } = vi.hoisted(() => ({ getPublicTheme: vi.fn() }));

vi.mock('../api', () => ({
  createPortalApi: () => ({ getPublicTheme }),
}));

import type { ExternalThemeData } from '../api';
import { ThemeRoot } from './ThemeRoot';
import { useThemeControls } from './useThemeControls';

/** A minimal child that exposes setActiveThemeId so tests can drive a theme switch. */
function ThemeSwitcher({ to }: { to: string }) {
  const { setActiveThemeId } = useThemeControls();
  return <button onClick={() => setActiveThemeId(to)}>switch to {to}</button>;
}

/** A minimal child that surfaces the resolved brand mark so tests can assert it. */
function BrandProbe() {
  const { brand } = useThemeControls();
  const mark = brand.mark;
  const detail = mark.type === 'image' ? mark.url : mark.type === 'text' ? mark.text : mark.id;
  return <span data-testid="brand-mark">{`${mark.type}:${detail}`}</span>;
}

function faviconHref(): string | null {
  return document.head.querySelector<HTMLLinkElement>('link[rel="icon"]')?.href ?? null;
}

const KEY = 'op.colorMode';

/**
 * Install an in-memory `localStorage` stub. This jsdom build does not expose
 * `window.localStorage`, so the color-mode hook's guarded reads/writes would
 * otherwise be silent no-ops. (Copied from useColorMode.test.tsx.)
 */
function installLocalStorage() {
  const store = new Map<string, string>();
  const storage = {
    getItem: (key: string) => (store.has(key) ? store.get(key)! : null),
    setItem: (key: string, value: string) => {
      store.set(key, String(value));
    },
    removeItem: (key: string) => {
      store.delete(key);
    },
    clear: () => {
      store.clear();
    },
    key: (index: number) => Array.from(store.keys())[index] ?? null,
    get length() {
      return store.size;
    },
  } satisfies Storage;
  vi.stubGlobal('localStorage', storage);
  return storage;
}

/**
 * Install a `window.matchMedia` stub reporting the given `dark` state for
 * `(prefers-color-scheme: dark)`. (Copied from useColorMode.test.tsx.)
 */
function installMatchMedia(dark: boolean) {
  const mql = {
    matches: dark,
    media: '(prefers-color-scheme: dark)',
    addEventListener: vi.fn(),
    removeEventListener: vi.fn(),
  };
  const matchMedia = vi.fn().mockReturnValue(mql);
  vi.stubGlobal('matchMedia', matchMedia);
  return { mql, matchMedia };
}

/** A minimal-but-valid external theme payload, as the wire response would shape it. */
function externalFixture(overrides: Partial<ExternalThemeData> = {}): ExternalThemeData {
  return {
    id: 'acme',
    name: 'Acme',
    productName: 'Acme AI Gateway',
    font: '',
    brand: { type: 'text', text: 'Acme', title: 'Cloud' },
    hasFavicon: false,
    hasLogo: false,
    ...overrides,
  };
}

beforeEach(() => {
  installLocalStorage();
  getPublicTheme.mockReset();
  getPublicTheme.mockResolvedValue({ theme: 'default', source: 'builtin', data: null });
});

afterEach(() => {
  // This project runs Vitest without `globals`, so Testing Library's auto
  // cleanup (which hooks the global afterEach) is not registered — unmount
  // rendered trees explicitly so they don't accumulate across tests.
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  // documentElement is shared across tests — clear the bridge's inline writes.
  document.documentElement.removeAttribute('style');
  delete document.documentElement.dataset.mode;
  // document.head is shared across tests too — drop any favicon <link> the
  // component find-or-created so the next test starts from a clean head.
  document.head.querySelector('link[rel="icon"]')?.remove();
});

describe('ThemeRoot', () => {
  it('fetches the public theme on mount (fetch->apply wiring fires)', async () => {
    installMatchMedia(false);

    render(
      <ThemeRoot>
        <div data-testid="child" />
      </ThemeRoot>,
    );

    // Proves the pre-auth GET /api/system/theme actually runs on mount; the
    // resolved id then drives the CSS-var bridge below.
    await waitFor(() => expect(getPublicTheme).toHaveBeenCalledTimes(1));
    await waitFor(() =>
      expect(document.documentElement.style.getPropertyValue('--brand-accent')).toBe('#0d9488'),
    );
  });

  it('applies the default light theme + mode on mount and renders children', async () => {
    installMatchMedia(false);

    render(
      <ThemeRoot>
        <div data-testid="child" />
      </ThemeRoot>,
    );

    await waitFor(() => {
      expect(document.documentElement.style.getPropertyValue('--brand-accent')).toBe('#0d9488');
    });
    expect(document.documentElement.dataset.mode).toBe('light');
    expect(screen.getByTestId('child')).toBeInTheDocument();
  });

  it('applies the dark variant when the color mode resolves to dark', async () => {
    installMatchMedia(true);
    localStorage.setItem(KEY, 'dark');

    render(
      <ThemeRoot>
        <div data-testid="child" />
      </ThemeRoot>,
    );

    await waitFor(() => {
      expect(document.documentElement.dataset.mode).toBe('dark');
    });
    expect(document.documentElement.style.getPropertyValue('--brand-accent')).toBe('#2dd4bf');
  });

  it('keeps the default theme when the pre-auth fetch rejects', async () => {
    installMatchMedia(false);
    getPublicTheme.mockReset();
    getPublicTheme.mockRejectedValue(new Error('network down'));

    render(
      <ThemeRoot>
        <div data-testid="child" />
      </ThemeRoot>,
    );

    await waitFor(() => {
      expect(screen.getByTestId('child')).toBeInTheDocument();
    });
    expect(document.documentElement.style.getPropertyValue('--brand-accent')).toBe('#0d9488');
    expect(document.documentElement.dataset.mode).toBe('light');
  });

  it('sets the default-theme favicon (real PNG file) on mount (find-or-create the <link>)', async () => {
    installMatchMedia(false);

    render(
      <ThemeRoot>
        <div data-testid="child" />
      </ThemeRoot>,
    );

    await waitFor(() => {
      expect(faviconHref()).toContain('favicon-default.png');
    });
    const link = document.head.querySelector<HTMLLinkElement>('link[rel="icon"]');
    expect(link?.type).toBe('image/png');
    // A real file, never a data: URI (Safari macOS does not render data: favicons).
    expect(faviconHref()).not.toContain('data:');
  });

  it('swaps the favicon file per theme when the active theme changes', async () => {
    installMatchMedia(false);

    render(
      <ThemeRoot>
        <ThemeSwitcher to="skynet" />
      </ThemeRoot>,
    );

    await waitFor(() => {
      expect(faviconHref()).toContain('favicon-default.png');
    });

    fireEvent.click(screen.getByRole('button', { name: 'switch to skynet' }));

    await waitFor(() => {
      expect(faviconHref()).toContain('favicon-skynet.png');
    });
    expect(faviconHref()).not.toContain('favicon-default.png');
  });

  it('throws when useThemeControls is used outside ThemeRoot', () => {
    const spy = vi.spyOn(console, 'error').mockImplementation(() => {});

    expect(() => renderHook(() => useThemeControls())).toThrow(
      'useThemeControls must be used within ThemeRoot',
    );

    spy.mockRestore();
  });
});

describe('ThemeRoot (external theme data)', () => {
  it("applies an external theme's light token overrides and product name", async () => {
    installMatchMedia(false);
    getPublicTheme.mockResolvedValue({
      theme: 'acme',
      source: 'external',
      data: externalFixture({ light: { brandPrimary: '#ff00aa' } }),
    });

    render(
      <ThemeRoot>
        <div data-testid="child" />
      </ThemeRoot>,
    );

    await waitFor(() => {
      expect(document.documentElement.style.getPropertyValue('--brand-primary')).toBe('#ff00aa');
    });
    expect(document.title).toBe('Acme AI Gateway');
    // Tokens the external theme didn't override still inherit from Default.
    expect(document.documentElement.style.getPropertyValue('--surface')).toBe('#ffffff');
  });

  it("renders an image brand mark's <img> src from the backend logo endpoint", async () => {
    installMatchMedia(false);
    getPublicTheme.mockResolvedValue({
      theme: 'acme',
      source: 'external',
      data: externalFixture({ brand: { type: 'image', text: '', title: 'Cloud' }, hasLogo: true }),
    });

    render(
      <ThemeRoot>
        <BrandProbe />
      </ThemeRoot>,
    );

    // ThemeRoot must resolve the external image brand to the absolute backend
    // logo endpoint (not just apply the product name).
    //
    // The title is awaited rather than asserted straight after the brand mark:
    // the mark is rendered output and is therefore already in the DOM at commit
    // time, while the title is written by an effect, which React flushes after
    // that commit. Asserting it synchronously here read the still-default title
    // whenever the poll landed in that window (seen in CI). The sibling title
    // assertions in this file wait on values that are themselves effect-written,
    // so they are synchronised with the flush already.
    await waitFor(() => {
      expect(screen.getByTestId('brand-mark').textContent).toBe(
        'image:/api/system/themes/acme/logo',
      );
      expect(document.title).toBe('Acme AI Gateway');
    });
  });

  it("resolves the external theme's own favicon endpoint when it has one", async () => {
    installMatchMedia(false);
    getPublicTheme.mockResolvedValue({
      theme: 'acme',
      source: 'external',
      data: externalFixture({ hasFavicon: true }),
    });

    render(
      <ThemeRoot>
        <div data-testid="child" />
      </ThemeRoot>,
    );

    await waitFor(() => {
      expect(faviconHref()).toContain('/api/system/themes/acme/favicon');
    });
  });

  it('leaves the built-in path unchanged when the fetch resolves a builtin theme', async () => {
    installMatchMedia(false);
    getPublicTheme.mockResolvedValue({ theme: 'skynet', source: 'builtin', data: null });

    render(
      <ThemeRoot>
        <div data-testid="child" />
      </ThemeRoot>,
    );

    await waitFor(() => {
      expect(document.title).toBe('Skynet AI Gateway');
    });
    expect(document.documentElement.style.getPropertyValue('--brand-primary')).toBe('#ff2a2a');
  });
});
