// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup } from '@testing-library/react';
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
// Testing Library's own auto-cleanup NEVER registers in this project, and the
// reason is easy to miss: @testing-library/react's entry point installs it only
// `if (typeof afterEach === 'function')` (node_modules/@testing-library/react/
// dist/index.js:26) — a GLOBAL afterEach. Vitest's `globals` defaults to false
// and vite.config.ts does not enable it, which is exactly why this file has to
// import `afterEach` from 'vitest' above. So at the moment RTL is evaluated
// there is no global hook to attach to, it silently skips auto-cleanup, and
// every rendered tree stays mounted in the shared jsdom for the rest of the
// file.
//
// That is not merely untidy. React defers a mounted root's passive-effect
// flush to the scheduler, which under Node is `setImmediate` — a Node timer,
// NOT a jsdom one, so it is not cancelled when Vitest tears the environment
// down between files. The deferred callback's first statement reads
// `window.event` (react-dom-client.development.js:17920), so an immediate that
// survives teardown throws `ReferenceError: window is not defined` as an
// UNCAUGHT exception. That fails the whole job with every test green, and
// `retry` cannot rescue it because it is not a test that failed.
//
// Unmounting after every test removes the precondition: no root outlives its
// test, so no passive-effect flush is left pending at teardown. Tests need not
// call `cleanup()` themselves — many still do, which is harmless and
// idempotent.
afterEach(() => {
  // Before the localStorage clear, not after: unmount effects may write to the
  // mirror, and the next test must start with both an empty DOM and an empty
  // store.
  cleanup();
  window.localStorage.clear();
});
