// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import { PcmRingBuffer, playbackBufferCapacity } from './playbackPcm';

// The sample buffer the player hands the worklet, as a plain array for readable assertions.
const f32 = (values: number[]): Float32Array => Float32Array.from(values);

describe('PcmRingBuffer', () => {
  it('reads back exactly what was written, in order', () => {
    const ring = new PcmRingBuffer(8);
    ring.write(f32([1, 2, 3]));
    expect(ring.available).toBe(3);
    const out = new Float32Array(3);
    expect(ring.read(out)).toBe(3);
    expect(Array.from(out)).toEqual([1, 2, 3]);
    expect(ring.available).toBe(0);
  });

  it('zero-fills the tail and reports the real count on an underrun', () => {
    const ring = new PcmRingBuffer(8);
    ring.write(f32([1, 2]));
    const out = new Float32Array(4);
    // Only two real samples are available: the worklet needs to know that (the drain edge).
    expect(ring.read(out)).toBe(2);
    expect(Array.from(out)).toEqual([1, 2, 0, 0]);
    expect(ring.available).toBe(0);
  });

  it('reads an empty buffer as silence with a real count of zero', () => {
    const ring = new PcmRingBuffer(4);
    const out = new Float32Array(3).fill(9);
    expect(ring.read(out)).toBe(0);
    expect(Array.from(out)).toEqual([0, 0, 0]);
  });

  it('serves a run of writes across many small reads, in order and without loss', () => {
    const ring = new PcmRingBuffer(16);
    ring.write(f32([1, 2, 3, 4]));
    const a = new Float32Array(3);
    expect(ring.read(a)).toBe(3);
    expect(Array.from(a)).toEqual([1, 2, 3]);
    ring.write(f32([5, 6]));
    const b = new Float32Array(3);
    expect(ring.read(b)).toBe(3);
    // The read spans the 4 left before the new write and into it: 4, then 5, 6.
    expect(Array.from(b)).toEqual([4, 5, 6]);
    expect(ring.available).toBe(0);
  });

  it('wraps a write around the end of the backing buffer without reordering', () => {
    const ring = new PcmRingBuffer(4);
    ring.write(f32([1, 2, 3]));
    const first = new Float32Array(2);
    ring.read(first); // consume 1, 2 -> read head now at index 2
    expect(Array.from(first)).toEqual([1, 2]);
    // 1 sample left (3) + 3 new (4, 5, 6) exactly fills capacity 4, wrapping past the end.
    ring.write(f32([4, 5, 6]));
    expect(ring.available).toBe(4);
    const out = new Float32Array(4);
    expect(ring.read(out)).toBe(4);
    expect(Array.from(out)).toEqual([3, 4, 5, 6]);
  });

  it('grows to hold a write larger than the remaining capacity, preserving order', () => {
    const ring = new PcmRingBuffer(2);
    ring.write(f32([1, 2]));
    ring.write(f32([3, 4, 5])); // 2 + 3 = 5 > capacity 2: must grow, never drop
    expect(ring.capacity).toBeGreaterThanOrEqual(5);
    expect(ring.available).toBe(5);
    const out = new Float32Array(5);
    expect(ring.read(out)).toBe(5);
    expect(Array.from(out)).toEqual([1, 2, 3, 4, 5]);
  });

  it('grows while wrapped, keeping the live samples in order', () => {
    const ring = new PcmRingBuffer(4);
    ring.write(f32([1, 2, 3, 4]));
    const drop = new Float32Array(3);
    ring.read(drop); // head moves to index 3; one live sample (4) remains, wrapped region free
    ring.write(f32([5, 6, 7, 8, 9])); // 1 + 5 = 6 > capacity 4: grow from a wrapped state
    expect(ring.available).toBe(6);
    const out = new Float32Array(6);
    expect(ring.read(out)).toBe(6);
    expect(Array.from(out)).toEqual([4, 5, 6, 7, 8, 9]);
  });

  it('ignores an empty write', () => {
    const ring = new PcmRingBuffer(4);
    ring.write(f32([]));
    expect(ring.available).toBe(0);
  });

  it('rejects a capacity that is not a positive integer', () => {
    expect(() => new PcmRingBuffer(0)).toThrow();
    expect(() => new PcmRingBuffer(-1)).toThrow();
    expect(() => new PcmRingBuffer(1.5)).toThrow();
  });
});

describe('playbackBufferCapacity', () => {
  it('is the whole number of samples in the given seconds at the given rate', () => {
    expect(playbackBufferCapacity(48000, 8)).toBe(384000);
    expect(playbackBufferCapacity(44100, 0.5)).toBe(22050);
  });

  it('is at least one sample', () => {
    expect(playbackBufferCapacity(48000, 0)).toBeGreaterThanOrEqual(1);
  });
});
