// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { act, renderHook } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { useColorMode } from './useColorMode';

const KEY = 'op.colorMode';

/**
 * Install an in-memory `localStorage` stub. This jsdom build does not expose
 * `window.localStorage`, so the hook's guarded writes would otherwise be silent
 * no-ops and the persistence assertions could not observe anything.
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
 * Install a `window.matchMedia` stub whose media-query-list reports the given
 * `dark` state for `(prefers-color-scheme: dark)`. The stub carries the
 * add/removeEventListener pair the hook's effect subscribes to, so mounting
 * never throws.
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

beforeEach(() => {
  installLocalStorage();
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe('useColorMode', () => {
  it('resolves to light when no pref is stored and the OS is light', () => {
    installMatchMedia(false);

    const { result } = renderHook(() => useColorMode(true));

    expect(result.current.pref).toBe('system');
    expect(result.current.effective).toBe('light');
  });

  it('resolves to dark when no pref is stored and the OS is dark', () => {
    installMatchMedia(true);

    const { result } = renderHook(() => useColorMode(true));

    expect(result.current.pref).toBe('system');
    expect(result.current.effective).toBe('dark');
  });

  it("honours a stored 'dark' pref even when the OS is light", () => {
    installMatchMedia(false);
    localStorage.setItem(KEY, 'dark');

    const { result } = renderHook(() => useColorMode(true));

    expect(result.current.pref).toBe('dark');
    expect(result.current.effective).toBe('dark');
  });

  it("honours a stored 'light' pref even when the OS is dark", () => {
    installMatchMedia(true);
    localStorage.setItem(KEY, 'light');

    const { result } = renderHook(() => useColorMode(true));

    expect(result.current.pref).toBe('light');
    expect(result.current.effective).toBe('light');
  });

  it('persists and applies the pref set via setMode', () => {
    installMatchMedia(false);

    const { result } = renderHook(() => useColorMode(true));

    act(() => {
      result.current.setMode('dark');
    });

    expect(localStorage.getItem(KEY)).toBe('dark');
    expect(result.current.pref).toBe('dark');
    expect(result.current.effective).toBe('dark');
  });

  it('forces light when the theme has no dark variant, regardless of stored pref', () => {
    installMatchMedia(true);
    localStorage.setItem(KEY, 'dark');

    const { result } = renderHook(() => useColorMode(false));

    expect(result.current.pref).toBe('dark');
    expect(result.current.effective).toBe('light');
  });

  it('forces light when the theme has no dark variant, even when the OS is dark', () => {
    installMatchMedia(true);

    const { result } = renderHook(() => useColorMode(false));

    expect(result.current.effective).toBe('light');
  });

  it('toggles light -> dark and persists the pref', () => {
    installMatchMedia(false);

    const { result } = renderHook(() => useColorMode(true));

    expect(result.current.effective).toBe('light');

    act(() => {
      result.current.toggle();
    });

    expect(result.current.effective).toBe('dark');
    expect(result.current.pref).toBe('dark');
    expect(localStorage.getItem(KEY)).toBe('dark');
  });

  it('toggles dark -> light and persists the pref', () => {
    installMatchMedia(true);

    const { result } = renderHook(() => useColorMode(true));

    expect(result.current.effective).toBe('dark');

    act(() => {
      result.current.toggle();
    });

    expect(result.current.effective).toBe('light');
    expect(result.current.pref).toBe('light');
    expect(localStorage.getItem(KEY)).toBe('light');
  });

  it('subscribes and unsubscribes to OS color-scheme changes', () => {
    const { mql } = installMatchMedia(false);

    const { unmount } = renderHook(() => useColorMode(true));

    expect(mql.addEventListener).toHaveBeenCalledWith('change', expect.any(Function));

    unmount();

    expect(mql.removeEventListener).toHaveBeenCalledWith('change', expect.any(Function));
  });
});
