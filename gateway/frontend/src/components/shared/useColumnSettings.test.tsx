// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { reconcileHiddenIds, useColumnSettings } from './useColumnSettings';

const CATALOGUE = ['a', 'b', 'c'] as const;
type Id = (typeof CATALOGUE)[number];
const DEFAULT_HIDDEN: readonly Id[] = ['c'];

describe('reconcileHiddenIds (crash-safety guard, shared across all four sites)', () => {
  it('falls back to the given default-hidden set for a corrupt (non-array) value', () => {
    for (const corrupt of [{ foo: 1 }, 'bogus', 42, null, undefined] as unknown[]) {
      expect(reconcileHiddenIds(corrupt, CATALOGUE, DEFAULT_HIDDEN)).toEqual(DEFAULT_HIDDEN);
    }
  });

  it('drops unknown/stale ids but keeps known ones, preserving array order', () => {
    expect(reconcileHiddenIds(['a', 'bogus', 'b', 7], CATALOGUE, DEFAULT_HIDDEN)).toEqual([
      'a',
      'b',
    ]);
  });

  it("accepts an empty array as 'nothing hidden' (not corruption)", () => {
    expect(reconcileHiddenIds([], CATALOGUE, DEFAULT_HIDDEN)).toEqual([]);
  });

  it('treats a known id absent from a stored set as visible (new-id default)', () => {
    expect(reconcileHiddenIds(['a'], CATALOGUE, DEFAULT_HIDDEN)).not.toContain('b');
  });
});

// Harness component (mirrors the pattern in preferences.test.tsx — this repo has
// no react-hooks testing helper) exercising the hook with no PreferencesProvider
// mounted, so usePreference falls back to its localStorage-mirror mode.
function Harness({ baseKey = 'test.cols' }: { baseKey?: string }) {
  const { order, hidden, visibleIds, toggle, reorder, reset } = useColumnSettings<Id>(
    baseKey,
    CATALOGUE,
    DEFAULT_HIDDEN,
  );
  return (
    <div>
      <span data-testid="order">{JSON.stringify(order)}</span>
      <span data-testid="hidden">{JSON.stringify(hidden)}</span>
      <span data-testid="visible">{JSON.stringify(visibleIds)}</span>
      <button onClick={() => toggle('b')}>toggle-b</button>
      <button onClick={() => reorder('a', 'c', 'after')}>reorder</button>
      <button onClick={() => reset()}>reset</button>
    </div>
  );
}

function installStorage() {
  const store = new Map<string, string>();
  const mock = {
    getItem: (k: string) => (store.has(k) ? (store.get(k) as string) : null),
    setItem: (k: string, v: string) => void store.set(k, String(v)),
    removeItem: (k: string) => void store.delete(k),
    clear: () => store.clear(),
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
  return store;
}

beforeEach(() => installStorage());
afterEach(cleanup);

describe('useColumnSettings', () => {
  it('defaults order to the catalogue and hidden to defaultHidden', () => {
    render(<Harness />);
    expect(screen.getByTestId('order').textContent).toBe(JSON.stringify(CATALOGUE));
    expect(screen.getByTestId('hidden').textContent).toBe(JSON.stringify(DEFAULT_HIDDEN));
    expect(screen.getByTestId('visible').textContent).toBe(JSON.stringify(['a', 'b']));
  });

  it('toggle flips hidden membership', () => {
    render(<Harness />);
    fireEvent.click(screen.getByText('toggle-b'));
    expect(screen.getByTestId('hidden').textContent).toBe(JSON.stringify(['c', 'b']));
    expect(screen.getByTestId('visible').textContent).toBe(JSON.stringify(['a']));
    fireEvent.click(screen.getByText('toggle-b'));
    expect(screen.getByTestId('hidden').textContent).toBe(JSON.stringify(['c']));
  });

  it('reorder moves a column via moveColumn/reconcileOrder', () => {
    render(<Harness />);
    fireEvent.click(screen.getByText('reorder'));
    expect(screen.getByTestId('order').textContent).toBe(JSON.stringify(['b', 'c', 'a']));
  });

  it('reset restores both hidden and order to their defaults', () => {
    render(<Harness />);
    fireEvent.click(screen.getByText('toggle-b'));
    fireEvent.click(screen.getByText('reorder'));
    fireEvent.click(screen.getByText('reset'));
    expect(screen.getByTestId('order').textContent).toBe(JSON.stringify(CATALOGUE));
    expect(screen.getByTestId('hidden').textContent).toBe(JSON.stringify(DEFAULT_HIDDEN));
  });

  it('persists hidden/order to the localStorage mirror under baseKey.hidden/.order', () => {
    const store = installStorage();
    render(<Harness baseKey="test.persist" />);
    fireEvent.click(screen.getByText('toggle-b'));
    expect(JSON.parse(store.get('op.pref.test.persist.hidden')!)).toEqual(['c', 'b']);
  });
});
