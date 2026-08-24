// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import { formatDate, formatMetric } from './format';
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

describe('formatMetric', () => {
  it('renders a value with the requested number of decimals', () => {
    expect(formatMetric(12.3456, 2)).toBe('12.35');
    expect(formatMetric(8, 2)).toBe('8.00');
    // Energy per token is a very small number and needs the long tail.
    expect(formatMetric(0.0000001234, 10)).toBe('0.0000001234');
  });

  it('renders whole numbers without a decimal point at zero decimals', () => {
    expect(formatMetric(1500, 0)).toBe('1500');
  });

  it('renders the em-dash placeholder for missing or zero values', () => {
    // The backend reports "not measured" as 0, which this list has always shown
    // as the placeholder rather than a misleading 0.00.
    expect(formatMetric(0, 2)).toBe('—');
    expect(formatMetric(null, 2)).toBe('—');
    expect(formatMetric(undefined, 2)).toBe('—');
  });
});
