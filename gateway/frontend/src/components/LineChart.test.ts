// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, it, expect } from 'vitest';
import { downsample, monotonePath } from './LineChart';

// Parse a path "M x y C c1x c1y, c2x c2y, x y C ..." into per-segment control+anchor
// y-values: [{y0, c1y, c2y, y1}, ...].
function segments(d: string): { y0: number; c1y: number; c2y: number; y1: number }[] {
  const nums = d.match(/-?\d+(?:\.\d+)?/g)?.map(Number) ?? [];
  // First two numbers are the M anchor; then each C segment is 6 numbers.
  const out: { y0: number; c1y: number; c2y: number; y1: number }[] = [];
  let y0 = nums[1];
  for (let i = 2; i + 5 < nums.length; i += 6) {
    const c1y = nums[i + 1];
    const c2y = nums[i + 3];
    const y1 = nums[i + 5];
    out.push({ y0, c1y, c2y, y1 });
    y0 = y1;
  }
  return out;
}

describe('monotonePath (no overshoot)', () => {
  it('returns a straight segment for two points and a move for one', () => {
    expect(monotonePath([{ x: 0, y: 5 }])).toBe('M 0 5');
    expect(
      monotonePath([
        { x: 0, y: 1 },
        { x: 10, y: 3 },
      ]),
    ).toBe('M 0 1 L 10 3');
  });

  it('never lets a control point overshoot/undershoot a spike (0,0,peak,0,0)', () => {
    // A spike is the classic case where Catmull-Rom dips below 0 (undershoot).
    const pts = [0, 0, 4, 0, 0].map((y, i) => ({ x: i * 10, y }));
    const segs = segments(monotonePath(pts));
    for (const s of segs) {
      const lo = Math.min(s.y0, s.y1);
      const hi = Math.max(s.y0, s.y1);
      // Each cubic Bézier is bounded by its control points, so keeping the control
      // y's within the segment's endpoint range guarantees no over/undershoot.
      expect(s.c1y).toBeGreaterThanOrEqual(lo - 1e-9);
      expect(s.c1y).toBeLessThanOrEqual(hi + 1e-9);
      expect(s.c2y).toBeGreaterThanOrEqual(lo - 1e-9);
      expect(s.c2y).toBeLessThanOrEqual(hi + 1e-9);
    }
  });

  it('keeps every control point within the global data range for arbitrary values', () => {
    const values = [3, 1, 4, 1, 5, 9, 2, 6, 5, 3, 5];
    const pts = values.map((y, i) => ({ x: i * 7, y }));
    const lo = Math.min(...values);
    const hi = Math.max(...values);
    const segs = segments(monotonePath(pts));
    for (const s of segs) {
      for (const y of [s.c1y, s.c2y]) {
        expect(y).toBeGreaterThanOrEqual(lo - 1e-9);
        expect(y).toBeLessThanOrEqual(hi + 1e-9);
      }
    }
  });

  it('flattens the tangent at a local extremum (peak control points do not exceed the peak)', () => {
    const pts = [0, 2, 1].map((y, i) => ({ x: i, y })); // up then down — 2 is a local max
    const segs = segments(monotonePath(pts));
    for (const s of segs) {
      expect(Math.max(s.c1y, s.c2y)).toBeLessThanOrEqual(2 + 1e-9);
    }
  });
});

describe('downsample', () => {
  it('returns the input unchanged when within the cap', () => {
    const times = ['a', 'b', 'c'];
    const series = [{ label: 'x', color: 'red', values: [1, 2, 3] }];
    const out = downsample(times, series, 5);
    expect(out.times).toBe(times);
    expect(out.series).toBe(series);
  });

  it('reduces to at most maxPoints groups, averaging each series within a group', () => {
    const times = ['a', 'b', 'c', 'd'];
    const series = [{ label: 'x', color: 'red', values: [0, 10, 20, 30] }];
    const out = downsample(times, series, 2);
    // g0 = mean(0,10)=5 @ "a"; g1 = mean(20,30)=25 @ "c".
    expect(out.times).toEqual(['a', 'c']);
    expect(out.series[0].values).toEqual([5, 25]);
    // label/colour are preserved.
    expect(out.series[0].label).toBe('x');
    expect(out.series[0].color).toBe('red');
  });

  it('downsamples every series in lockstep to the same length', () => {
    const n = 1000;
    const times = Array.from({ length: n }, (_, i) => String(i));
    const series = [
      { label: 'a', color: '1', values: Array.from({ length: n }, (_, i) => i) },
      { label: 'b', color: '2', values: Array.from({ length: n }, () => 1) },
    ];
    const out = downsample(times, series, 200);
    expect(out.times).toHaveLength(200);
    expect(out.series[0].values).toHaveLength(200);
    expect(out.series[1].values).toHaveLength(200);
    // A constant series stays constant through averaging.
    expect(out.series[1].values.every((v) => v === 1)).toBe(true);
  });
});
