// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { render, screen } from '@testing-library/react';
import { expect, test } from 'vitest';

// These two tests pin the unmount-after-every-test invariant that
// vitest.setup.ts establishes, and they only mean something as a PAIR, in this
// order: the first leaves a tree mounted, the second checks it is gone.
//
// The invariant is worth pinning because nothing about it is visible at the
// call site. Testing Library's auto-cleanup does not register under this
// config (it needs a global `afterEach`, and `globals` is off), so the cleanup
// comes from our own setup file. If that call is removed, most of the suite
// keeps passing and the only symptom is an occasional CI job that fails with
// every test green — see the long comment in vitest.setup.ts. This pair turns
// that into an immediate, named failure instead.

test('leaves a tree mounted on purpose (first half of the cleanup pin)', () => {
  render(<div data-testid="cleanup-pin">mounted</div>);
  expect(screen.getAllByTestId('cleanup-pin')).toHaveLength(1);
});

test('every tree from the previous test has been unmounted', () => {
  expect(document.querySelectorAll('[data-testid="cleanup-pin"]')).toHaveLength(0);
});
