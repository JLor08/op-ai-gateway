// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import {
  BILLING_UNIT_HELP_KEYS,
  ENERGY_SOURCE_HELP_KEYS,
  isTokenMetered,
  tokenAggregate,
} from './billingUnit';

describe('isTokenMetered', () => {
  it('treats an empty or absent unit as token-metered', () => {
    expect(isTokenMetered({ billing_unit: '' })).toBe(true);
    expect(isTokenMetered({})).toBe(true);
  });

  it('treats any non-empty unit as not token-metered', () => {
    expect(isTokenMetered({ billing_unit: 'image' })).toBe(false);
    expect(isTokenMetered({ billing_unit: 'audio_second' })).toBe(false);
    // Forward compatibility: an unknown unit from a newer backend is still not
    // token-metered. Falling back to "token-metered" would print a zero as if it
    // were a measurement.
    expect(isTokenMetered({ billing_unit: 'video_second' })).toBe(false);
  });
});

describe('tokenAggregate', () => {
  it('renders the number when the whole population is token-metered', () => {
    expect(tokenAggregate(300, 0, 10)).toEqual({ text: '300', mixed: false, applicable: true });
  });

  it('renders an em dash when none of the population is token-metered', () => {
    expect(tokenAggregate(0, 10, 10)).toEqual({ text: '—', mixed: false, applicable: false });
  });

  it('renders the number and flags mixed when the population is mixed', () => {
    // The sum is arithmetically correct for the token-metered subset; the flag is
    // what lets the caller say so instead of implying it covers everything.
    expect(tokenAggregate(300, 4, 10)).toEqual({ text: '300', mixed: true, applicable: true });
  });

  it('renders the number for an empty population rather than an em dash', () => {
    expect(tokenAggregate(0, 0, 0)).toEqual({ text: '0', mixed: false, applicable: true });
  });
});

describe('help key tables', () => {
  it('keys on the FULL wire enum value for every energy source', () => {
    expect(Object.keys(ENERGY_SOURCE_HELP_KEYS).sort()).toEqual([
      '',
      'estimated',
      'measured',
      'modeled',
      'unpriceable',
    ]);
  });

  it('has no entry for an unknown value, so no tooltip is invented for it', () => {
    expect(ENERGY_SOURCE_HELP_KEYS['made_up']).toBeUndefined();
    expect(BILLING_UNIT_HELP_KEYS['made_up']).toBeUndefined();
  });

  it('keys billing units on the full wire value', () => {
    expect(Object.keys(BILLING_UNIT_HELP_KEYS).sort()).toEqual(['audio_second', 'image']);
  });
});
