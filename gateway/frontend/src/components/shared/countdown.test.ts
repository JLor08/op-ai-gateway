// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import { formatCountdown } from './countdown';

const MIN = 60_000;
const HOUR = 60 * MIN;
const DAY = 24 * HOUR;

describe('formatCountdown', () => {
  it('reads under a minute -- and a non-positive span -- as "< 1 min"', () => {
    expect(formatCountdown(0)).toBe('< 1 min');
    expect(formatCountdown(59_999)).toBe('< 1 min');
    expect(formatCountdown(-5 * MIN)).toBe('< 1 min');
  });

  it('shows whole minutes below an hour', () => {
    expect(formatCountdown(MIN)).toBe('1 min');
    expect(formatCountdown(42 * MIN + 59_000)).toBe('42 min');
    expect(formatCountdown(59 * MIN)).toBe('59 min');
  });

  it('shows hours and minutes below a day, dropping a zero minute part', () => {
    expect(formatCountdown(2 * HOUR + 14 * MIN)).toBe('2 h 14 min');
    expect(formatCountdown(HOUR)).toBe('1 h');
    expect(formatCountdown(23 * HOUR + 59 * MIN)).toBe('23 h 59 min');
  });

  it('shows days and hours from a day up, dropping a zero hour part', () => {
    expect(formatCountdown(3 * DAY + 4 * HOUR + 30 * MIN)).toBe('3 d 4 h');
    expect(formatCountdown(DAY)).toBe('1 d');
    expect(formatCountdown(6 * DAY + 23 * HOUR)).toBe('6 d 23 h');
  });
});
