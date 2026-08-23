// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { render, screen, fireEvent, waitFor, act, cleanup } from '@testing-library/react';
import { PreferencesProvider, usePreference } from './preferences';
import type { PortalApi } from './types';

function Harness({
  prefKey = 'table.x.order',
  def = ['a'] as string[],
}: {
  prefKey?: string;
  def?: string[];
}) {
  const [value, setValue] = usePreference<string[]>(prefKey, def);
  return (
    <div>
      <span data-testid="val">{JSON.stringify(value)}</span>
      <button onClick={() => setValue(['z'])}>set</button>
      <button onClick={() => setValue((prev) => [...prev, 'y'])}>append</button>
    </div>
  );
}

const val = () => screen.getByTestId('val').textContent;

// The test env does not expose window.localStorage; install a working in-memory
// mock so the localStorage-mirror behaviour is actually exercised.
beforeEach(() => {
  const store = new Map<string, string>();
  const mock = {
    getItem: (k: string) => (store.has(k) ? (store.get(k) as string) : null),
    setItem: (k: string, v: string) => {
      store.set(k, String(v));
    },
    removeItem: (k: string) => {
      store.delete(k);
    },
    clear: () => {
      store.clear();
    },
    key: (i: number) => Array.from(store.keys())[i] ?? null,
    get length() {
      return store.size;
    },
  };
  Object.defineProperty(window, 'localStorage', {
    value: mock,
    writable: true,
    configurable: true,
  });
  vi.restoreAllMocks();
});

afterEach(() => {
  cleanup();
});

describe('usePreference (no provider — localStorage fallback)', () => {
  it('returns the default when nothing is stored', () => {
    render(<Harness />);
    expect(val()).toBe(JSON.stringify(['a']));
  });

  it('persists a set value to the localStorage mirror and updates the value', () => {
    render(<Harness />);
    fireEvent.click(screen.getByText('set'));
    expect(val()).toBe(JSON.stringify(['z']));
    expect(JSON.parse(window.localStorage.getItem('op.pref.table.x.order') as string)).toEqual([
      'z',
    ]);
  });

  it('supports a functional updater over the current value', () => {
    render(<Harness />);
    fireEvent.click(screen.getByText('set')); // -> ["z"]
    fireEvent.click(screen.getByText('append')); // -> ["z","y"]
    expect(val()).toBe(JSON.stringify(['z', 'y']));
  });

  it('treats a persisted JSON null as the default (does not return null)', () => {
    window.localStorage.setItem('op.pref.table.x.order', 'null');
    render(<Harness />);
    expect(val()).toBe(JSON.stringify(['a']));
  });

  it('seeds the initial value from a valid mirror entry', () => {
    window.localStorage.setItem('op.pref.table.x.order', JSON.stringify(['m']));
    render(<Harness />);
    expect(val()).toBe(JSON.stringify(['m']));
  });
});

function makeApi(
  overrides: Partial<Pick<PortalApi, 'preferences' | 'setPreference'>>,
): Pick<PortalApi, 'preferences' | 'setPreference'> {
  return {
    preferences: async () => ({}),
    setPreference: async () => ({ ok: true }),
    ...overrides,
  };
}

describe('PreferencesProvider', () => {
  it('seeds synchronously from the mirror, then lets the server value win for non-dirty keys', async () => {
    window.localStorage.setItem('op.pref.table.x.order', JSON.stringify(['m']));
    const api = makeApi({ preferences: async () => ({ 'table.x.order': ['s'] }) });
    render(
      <PreferencesProvider api={api}>
        <Harness />
      </PreferencesProvider>,
    );
    // First paint comes from the mirror (no flash of defaults).
    expect(val()).toBe(JSON.stringify(['m']));
    // Then the server value wins.
    await waitFor(() => expect(val()).toBe(JSON.stringify(['s'])));
    // Mirror is refreshed to the server value.
    expect(JSON.parse(window.localStorage.getItem('op.pref.table.x.order') as string)).toEqual([
      's',
    ]);
  });

  it('does NOT clobber a key the user changed before the initial GET resolved', async () => {
    let resolveGet: (v: Record<string, unknown>) => void = () => {};
    const api = makeApi({
      preferences: () =>
        new Promise((resolve) => {
          resolveGet = resolve;
        }),
    });
    render(
      <PreferencesProvider api={api}>
        <Harness />
      </PreferencesProvider>,
    );
    // User edits before the GET resolves -> key becomes dirty.
    fireEvent.click(screen.getByText('set'));
    expect(val()).toBe(JSON.stringify(['z']));
    // The GET now resolves with a stale server value; it must not override the edit.
    await act(async () => {
      resolveGet({ 'table.x.order': ['s'] });
    });
    expect(val()).toBe(JSON.stringify(['z']));
  });

  it('PUTs a changed value to the server (debounced)', async () => {
    const setPreference = vi.fn(async () => ({ ok: true }));
    const api = makeApi({ setPreference });
    render(
      <PreferencesProvider api={api}>
        <Harness />
      </PreferencesProvider>,
    );
    fireEvent.click(screen.getByText('set'));
    await waitFor(() => expect(setPreference).toHaveBeenCalledWith('table.x.order', ['z']));
  });

  it('flushes a pending write to the server on provider unmount', async () => {
    const setPreference = vi.fn(async () => ({ ok: true }));
    const api = makeApi({ setPreference });
    const { unmount } = render(
      <PreferencesProvider api={api}>
        <Harness />
      </PreferencesProvider>,
    );
    fireEvent.click(screen.getByText('set'));
    // Unmount immediately (before the 400ms debounce would fire).
    unmount();
    expect(setPreference).toHaveBeenCalledWith('table.x.order', ['z']);
  });
});
