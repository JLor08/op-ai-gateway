// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { afterEach } from 'vitest';

// jsdom only implements HTMLCanvasElement.getContext when the optional native
// `canvas` package is installed; without it, every call logs a loud
// "Error: Not implemented: HTMLCanvasElement.prototype.getContext" to the
// test output (e.g. whenever a test mounts the Matrix theme's <MatrixRain/>)
// and then returns null anyway. Stubbing it to return null directly keeps the
// exact same runtime behavior — components take their documented
// no-2D-context path (see theme/MatrixRain.tsx) — minus the console noise.
HTMLCanvasElement.prototype.getContext = (() =>
  null) as typeof HTMLCanvasElement.prototype.getContext;

// Depending on the Node/jsdom combination, `window.localStorage` may be
// missing entirely (observed: Node 26 + jsdom 25 → undefined, while CI's
// Node 22 has a working one). Code under test treats localStorage as
// best-effort, so tests pass either way — but they must behave the SAME in
// both environments, so polyfill a minimal in-memory implementation when it
// is absent.
if (!window.localStorage) {
  const store = new Map<string, string>();
  const polyfill: Storage = {
    get length() {
      return store.size;
    },
    key: (i: number) => [...store.keys()][i] ?? null,
    getItem: (k: string) => (store.has(k) ? (store.get(k) as string) : null),
    setItem: (k: string, v: string) => {
      store.set(k, String(v));
    },
    removeItem: (k: string) => {
      store.delete(k);
    },
    clear: () => store.clear(),
  };
  Object.defineProperty(window, 'localStorage', { value: polyfill, configurable: true });
}

// Tests within one file share the same jsdom, so anything persisted to
// localStorage leaks into the next test. The PreferencesProvider mirrors every
// server preference there (op.pref.*) and seeds from it synchronously on
// mount, so a leaked mirror changes the FIRST render of the next test (seen
// in CI: Activity's legacy activity.groupBy mirror from one test resurrected
// grouping in the next). Isolate every test.
afterEach(() => {
  window.localStorage.clear();
});
