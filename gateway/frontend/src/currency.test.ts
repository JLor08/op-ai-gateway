// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import { CURRENCY_UNITS, availableUnits, formatCost, fromEur, toEur } from './currency';

describe('currency', () => {
  describe('fromEur', () => {
    const factor = 1.1;

    it('eur passes the value through unchanged', () => {
      expect(fromEur(2, 'eur', factor)).toBe(2);
    });

    it('eur_cent multiplies by 100', () => {
      expect(fromEur(2, 'eur_cent', factor)).toBe(200);
    });

    it('usd multiplies by the factor', () => {
      expect(fromEur(2, 'usd', factor)).toBeCloseTo(2.2, 10);
    });

    it('usd_cent multiplies by the factor and 100', () => {
      expect(fromEur(2, 'usd_cent', factor)).toBeCloseTo(220, 10);
    });
  });

  describe('toEur', () => {
    const factor = 1.1;

    it('eur passes the value through unchanged', () => {
      expect(toEur(2, 'eur', factor)).toBe(2);
    });

    it('eur_cent divides by 100', () => {
      expect(toEur(200, 'eur_cent', factor)).toBeCloseTo(2, 10);
    });

    it('usd divides by the factor', () => {
      expect(toEur(2.2, 'usd', factor)).toBeCloseTo(2, 10);
    });

    it('usd_cent divides by 100 and the factor', () => {
      expect(toEur(220, 'usd_cent', factor)).toBeCloseTo(2, 10);
    });

    it('usd with factor<=0 returns 0, never NaN', () => {
      const result = toEur(5, 'usd', 0);
      expect(result).toBe(0);
      expect(Number.isNaN(result)).toBe(false);
    });

    it('usd_cent with factor<=0 returns 0, never NaN', () => {
      const result = toEur(5, 'usd_cent', 0);
      expect(result).toBe(0);
      expect(Number.isNaN(result)).toBe(false);
    });
  });

  describe('formatCost', () => {
    it('formats eur_cent with 4 decimals and the ct suffix', () => {
      expect(formatCost(0.0003, 'eur_cent', 1)).toBe('0.0300 ct');
    });

    it('formats eur with 4 decimals and the € prefix', () => {
      expect(formatCost(0.0003, 'eur', 1)).toBe('€ 0.0003');
    });

    it('formats usd with 4 decimals and the $ prefix', () => {
      expect(formatCost(0.0003, 'usd', 2)).toBe('$ 0.0006');
    });

    it('formats usd_cent with 4 decimals and the US-ct suffix', () => {
      expect(formatCost(0.0003, 'usd_cent', 2)).toBe('0.0600 US-ct');
    });

    it('returns an em dash for zero', () => {
      expect(formatCost(0, 'eur', 1)).toBe('—');
    });

    it('returns an em dash for undefined', () => {
      expect(formatCost(undefined, 'eur', 1)).toBe('—');
    });

    it('returns an em dash for a non-finite value', () => {
      expect(formatCost(Number.NaN, 'eur', 1)).toBe('—');
      expect(formatCost(Number.POSITIVE_INFINITY, 'eur', 1)).toBe('—');
    });
  });

  describe('availableUnits', () => {
    it('excludes USD units when the factor is 0', () => {
      expect(availableUnits(0)).toEqual(['eur', 'eur_cent']);
    });

    it('excludes USD units when the factor is negative', () => {
      expect(availableUnits(-1)).toEqual(['eur', 'eur_cent']);
    });

    it('includes all 4 units when the factor is positive', () => {
      expect(availableUnits(1.1)).toEqual(CURRENCY_UNITS);
      expect(availableUnits(1.1)).toEqual(['eur', 'eur_cent', 'usd', 'usd_cent']);
    });
  });
});
