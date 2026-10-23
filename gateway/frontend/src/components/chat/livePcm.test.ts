// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import { BYTES_PER_SAMPLE, CHANNELS, toInt16 } from '../shared/audioToWav';
import { LinearResampler, MAX_LIVE_FRAME_BYTES, Pcm16Batcher, liveBatchSamples } from './livePcm';

function ramp(length: number, from = 0): Float32Array {
  return Float32Array.from({ length }, (_, i) => (from + i) / 1000);
}

// Feed `input` through a resampler in blocks of the given sizes (cycled), joining what comes out.
function resampleInBlocks(
  resampler: LinearResampler,
  input: Float32Array,
  blocks: number[],
): number[] {
  const out: number[] = [];
  let at = 0;
  for (let b = 0; at < input.length; b++) {
    const size = Math.min(blocks[b % blocks.length], input.length - at);
    out.push(...resampler.process(input.subarray(at, at + size)));
    at += size;
  }
  return out;
}

describe('LinearResampler', () => {
  it('keeps every third sample of 48 kHz audio going to 16 kHz', () => {
    const out = new LinearResampler(48000, 16000).process(ramp(12));
    expect(Array.from(out)).toEqual([0, 3, 6, 9].map((i) => Math.fround(i / 1000)));
  });

  it('turns a second of 48 kHz audio, fed in render-quantum blocks, into a second of 16 kHz', () => {
    const resampler = new LinearResampler(48000, 16000);
    let total = 0;
    for (let i = 0; i < 375; i++) total += resampler.process(new Float32Array(128)).length;
    expect(total).toBe(16000);
  });

  it.each([
    [48000, 16000],
    [24000, 16000],
    [44100, 16000],
    [8000, 16000],
    [16000, 16000],
  ])('is the same however %i Hz input is cut into blocks (to %i Hz)', (inRate, outRate) => {
    const input = Float32Array.from({ length: 4000 }, (_, i) => Math.sin(i / 7) * 0.8);
    const whole = Array.from(new LinearResampler(inRate, outRate).process(input));
    for (const blocks of [[1], [128], [3, 5, 7], [1000, 1]]) {
      const pieces = resampleInBlocks(new LinearResampler(inRate, outRate), input, blocks);
      // The fractional position is carried across blocks, so a cut changes nothing (not even
      // by a sample, which a position reset to 0 on every block would).
      expect(pieces.length).toBe(whole.length);
      for (let i = 0; i < whole.length; i++) expect(pieces[i]).toBeCloseTo(whole[i], 5);
    }
  });

  it('interpolates linearly between samples when the rate goes up', () => {
    const out = new LinearResampler(8000, 16000).process(Float32Array.of(0, 1, 0, -1, 0));
    expect(Array.from(out)).toEqual([0, 0.5, 1, 0.5, 0, -0.5, -1, -0.5]);
  });

  it('passes a constant signal through unchanged across blocks (no clicks at the seams)', () => {
    const out = resampleInBlocks(
      new LinearResampler(44100, 16000),
      new Float32Array(2000).fill(0.25),
      [128],
    );
    expect(out.length).toBeGreaterThan(700);
    for (const sample of out) expect(sample).toBeCloseTo(0.25, 6);
  });

  it('is not disturbed by an empty block or a block of one sample', () => {
    const input = ramp(11);
    const resampler = new LinearResampler(48000, 16000);
    const out = [
      ...resampler.process(new Float32Array(0)),
      ...resampler.process(input.subarray(0, 1)),
      ...resampler.process(new Float32Array(0)),
      ...resampler.process(input.subarray(1)),
    ];
    expect(out).toEqual(Array.from(new LinearResampler(48000, 16000).process(input)));
    expect(out).toHaveLength(4);
  });

  it('refuses a rate that is not a positive finite number', () => {
    expect(() => new LinearResampler(0, 16000)).toThrow();
    expect(() => new LinearResampler(48000, -1)).toThrow();
    expect(() => new LinearResampler(Number.NaN, 16000)).toThrow();
    expect(() => new LinearResampler(48000, Number.POSITIVE_INFINITY)).toThrow();
  });
});

describe('Pcm16Batcher', () => {
  const collect = () => {
    const batches: ArrayBuffer[] = [];
    return { batches, emit: (pcm: ArrayBuffer) => batches.push(pcm) };
  };

  it('writes little-endian signed 16-bit samples with the shared float conversion', () => {
    const { batches, emit } = collect();
    new Pcm16Batcher(4).push(Float32Array.of(0, 1, -1, 0.5), emit);
    expect(batches).toHaveLength(1);
    expect(batches[0].byteLength).toBe(4 * BYTES_PER_SAMPLE);
    const view = new DataView(batches[0]);
    expect([0, 1, 2, 3].map((i) => view.getInt16(i * 2, true))).toEqual([
      0,
      32767,
      -32768,
      toInt16(0.5),
    ]);
    // Little-endian on the wire whatever the platform: 32767 is 0xFF 0x7F.
    expect(Array.from(new Uint8Array(batches[0]).subarray(2, 4))).toEqual([0xff, 0x7f]);
  });

  it('clamps what overshoots [-1, 1]', () => {
    const { batches, emit } = collect();
    new Pcm16Batcher(2).push(Float32Array.of(3, -3), emit);
    const view = new DataView(batches[0]);
    expect([view.getInt16(0, true), view.getInt16(2, true)]).toEqual([32767, -32768]);
  });

  it('holds samples back until a batch is full, whatever the sizes pushed', () => {
    const { batches, emit } = collect();
    const batcher = new Pcm16Batcher(5);
    batcher.push(ramp(3), emit);
    expect(batches).toHaveLength(0);
    batcher.push(ramp(3, 3), emit); // 6 so far: one batch of 5, one left over
    expect(batches).toHaveLength(1);
    batcher.push(ramp(12, 6), emit); // 1 + 12 = 13: two more, three left over
    expect(batches.map((b) => b.byteLength)).toEqual([10, 10, 10]);
    // The batches are the stream in order, with nothing lost or repeated at the joins.
    const all = batches.flatMap((b) => {
      const view = new DataView(b);
      return Array.from({ length: 5 }, (_, i) => view.getInt16(i * 2, true));
    });
    expect(all).toEqual(Array.from({ length: 15 }, (_, i) => toInt16(i / 1000)));
  });

  it('flushes the partial batch at its real length, and nothing when it is empty', () => {
    const { batches, emit } = collect();
    const batcher = new Pcm16Batcher(10);
    batcher.flush(emit);
    expect(batches).toHaveLength(0);
    batcher.push(ramp(3), emit);
    batcher.flush(emit);
    expect(batches.map((b) => b.byteLength)).toEqual([6]);
    // Flushed means gone: a second flush has nothing, and the next batch starts clean.
    batcher.flush(emit);
    expect(batches).toHaveLength(1);
    batcher.push(ramp(10, 100), emit);
    expect(new DataView(batches[1]).getInt16(0, true)).toBe(toInt16(0.1));
  });

  it('emits buffers that can be transferred (each batch is a fresh ArrayBuffer)', () => {
    const { batches, emit } = collect();
    new Pcm16Batcher(2).push(ramp(6), emit);
    expect(batches).toHaveLength(3);
    expect(new Set(batches).size).toBe(3);
    for (const b of batches) expect(b).toBeInstanceOf(ArrayBuffer);
  });

  it('refuses a batch size that is not a positive whole number', () => {
    expect(() => new Pcm16Batcher(0)).toThrow();
    expect(() => new Pcm16Batcher(1.5)).toThrow();
  });
});

describe('liveBatchSamples', () => {
  it('is 100 ms of audio, a frame far below the gateway cap of one binary frame', () => {
    const samples = liveBatchSamples(16000);
    expect(samples).toBe(1600);
    const bytes = samples * CHANNELS * BYTES_PER_SAMPLE;
    expect(bytes).toBe(3200);
    // A frame of MAX_LIVE_FRAME_BYTES or more closes the socket (1009); stay orders below it.
    expect(MAX_LIVE_FRAME_BYTES).toBe(1 << 20);
    expect(bytes).toBeLessThan(MAX_LIVE_FRAME_BYTES / 100);
  });

  it('is a whole number of samples at any rate', () => {
    expect(Number.isInteger(liveBatchSamples(44100))).toBe(true);
    expect(liveBatchSamples(8000)).toBe(800);
  });
});
