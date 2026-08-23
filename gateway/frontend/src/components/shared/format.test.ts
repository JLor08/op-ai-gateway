// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import { formatDate } from './format';
describe('formatDate', () => {
  it('returns the fallback for null/empty/invalid input', () => {
    expect(formatDate(null, 'never')).toBe('never');
    expect(formatDate(undefined, 'never')).toBe('never');
    expect(formatDate('', 'never')).toBe('never');
    expect(formatDate('not-a-date', 'never')).toBe('never');
  });
  it('formats a valid ISO timestamp to a non-raw, non-fallback string', () => {
    const out = formatDate('2026-07-12T07:22:01Z', 'never');
    expect(out).not.toBe('never');
    expect(out).not.toBe('2026-07-12T07:22:01Z');
    expect(out).toContain('2026');
  });
});
