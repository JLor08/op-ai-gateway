// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// jsdom only implements HTMLCanvasElement.getContext when the optional native
// `canvas` package is installed; without it, every call logs a loud
// "Error: Not implemented: HTMLCanvasElement.prototype.getContext" to the
// test output (e.g. whenever a test mounts the Matrix theme's <MatrixRain/>)
// and then returns null anyway. Stubbing it to return null directly keeps the
// exact same runtime behavior — components take their documented
// no-2D-context path (see theme/MatrixRain.tsx) — minus the console noise.
HTMLCanvasElement.prototype.getContext = (() =>
  null) as typeof HTMLCanvasElement.prototype.getContext;

export {};
