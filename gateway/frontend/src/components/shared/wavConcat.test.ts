// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import { concatWavBlobs } from './wavConcat';

// A little-endian RIFF chunk: 4-byte id, 4-byte size, the body, and the pad byte an odd body
// is followed by.
function chunk(id: string, body: Uint8Array, declaredSize = body.length): Uint8Array<ArrayBuffer> {
  const out = new Uint8Array(8 + body.length + (body.length % 2));
  const view = new DataView(out.buffer);
  for (let i = 0; i < 4; i += 1) view.setUint8(i, id.charCodeAt(i));
  view.setUint32(4, declaredSize, true);
  out.set(body, 8);
  return out;
}

function fmtBody(rate: number, channels: number, bits: number, tag = 1): Uint8Array {
  const out = new Uint8Array(16);
  const view = new DataView(out.buffer);
  const blockAlign = (channels * bits) / 8;
  view.setUint16(0, tag, true);
  view.setUint16(2, channels, true);
  view.setUint32(4, rate, true);
  view.setUint32(8, rate * blockAlign, true);
  view.setUint16(12, blockAlign, true);
  view.setUint16(14, bits, true);
  return out;
}

function riff(...chunks: Uint8Array[]): Uint8Array<ArrayBuffer> {
  const size = chunks.reduce((n, c) => n + c.length, 4);
  const out = new Uint8Array(8 + size);
  const view = new DataView(out.buffer);
  'RIFF'.split('').forEach((c, i) => view.setUint8(i, c.charCodeAt(0)));
  view.setUint32(4, size, true);
  'WAVE'.split('').forEach((c, i) => view.setUint8(8 + i, c.charCodeAt(0)));
  let offset = 12;
  for (const c of chunks) {
    out.set(c, offset);
    offset += c.length;
  }
  return out;
}

// The tiny WAV builder: `pcm` is the raw data chunk, `before` / `after` are extra chunks (LIST
// and the like) around it, `dataSize` overrides what the data chunk's header declares.
interface WavOptions {
  rate?: number;
  channels?: number;
  bits?: number;
  tag?: number;
  before?: Uint8Array[];
  after?: Uint8Array[];
  dataSize?: number;
}

function wav(pcm: number[], o: WavOptions = {}): Blob {
  const { rate = 16000, channels = 1, bits = 16, tag = 1, before = [], after = [] } = o;
  const bytes = riff(
    chunk('fmt ', fmtBody(rate, channels, bits, tag)),
    ...before,
    chunk('data', Uint8Array.from(pcm), o.dataSize),
    ...after,
  );
  return new Blob([bytes], { type: 'audio/wav' });
}

async function bytesOf(blob: Blob): Promise<Uint8Array> {
  return new Uint8Array(await blob.arrayBuffer());
}

const ascii = (bytes: Uint8Array, from: number, to: number) =>
  String.fromCharCode(...bytes.slice(from, to));
const viewOf = (bytes: Uint8Array) =>
  new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);

// A parsed canonical output: the 44-byte header's fields and what follows it.
function readCanonical(bytes: Uint8Array) {
  const view = viewOf(bytes);
  return {
    riff: ascii(bytes, 0, 4),
    riffSize: view.getUint32(4, true),
    wave: ascii(bytes, 8, 12),
    fmtId: ascii(bytes, 12, 16),
    fmtSize: view.getUint32(16, true),
    tag: view.getUint16(20, true),
    channels: view.getUint16(22, true),
    rate: view.getUint32(24, true),
    byteRate: view.getUint32(28, true),
    blockAlign: view.getUint16(32, true),
    bits: view.getUint16(34, true),
    dataId: ascii(bytes, 36, 40),
    dataSize: view.getUint32(40, true),
    data: Array.from(bytes.slice(44)),
  };
}

describe('concatWavBlobs', () => {
  it('joins three mono 16-bit WAVs into one WAV whose data is the three data chunks in order', async () => {
    const out = await concatWavBlobs([wav([1, 0, 2, 0]), wav([3, 0, 4, 0, 5, 0]), wav([6, 0])]);

    expect(out.type).toBe('audio/wav');
    const parsed = readCanonical(await bytesOf(out));
    expect(parsed.data).toEqual([1, 0, 2, 0, 3, 0, 4, 0, 5, 0, 6, 0]);
    expect(parsed).toMatchObject({
      riff: 'RIFF',
      wave: 'WAVE',
      fmtId: 'fmt ',
      fmtSize: 16,
      tag: 1,
      channels: 1,
      rate: 16000,
      byteRate: 32000,
      blockAlign: 2,
      bits: 16,
      dataId: 'data',
      dataSize: 12,
      // everything after the RIFF size field: the 36 bytes of header past it plus the data
      riffSize: 36 + 12,
    });
  });

  it('keeps the first WAV format in the header (rate, channels, bits)', async () => {
    const out = await concatWavBlobs([
      wav([1, 0, 2, 0, 3, 0, 4, 0], { rate: 24000, channels: 2 }),
      wav([5, 0, 6, 0, 7, 0, 8, 0], { rate: 24000, channels: 2 }),
    ]);

    const parsed = readCanonical(await bytesOf(out));
    expect(parsed).toMatchObject({
      channels: 2,
      rate: 24000,
      bits: 16,
      blockAlign: 4,
      byteRate: 96000,
      dataSize: 16,
    });
  });

  it('returns a single WAV unchanged: the very same blob, the same bytes', async () => {
    const only = wav([1, 0, 2, 0]);
    const before = await bytesOf(only);

    const out = await concatWavBlobs([only]);

    expect(out).toBe(only);
    expect(await bytesOf(out)).toEqual(before);
  });

  it('returns a single blob unchanged even when it is not a WAV at all', async () => {
    const opaque = new Blob([new Uint8Array([1, 2, 3, 4])], { type: 'audio/wav' });

    expect(await concatWavBlobs([opaque])).toBe(opaque);
  });

  it('rejects an empty list', async () => {
    await expect(concatWavBlobs([])).rejects.toThrow(/no WAV/);
  });

  describe('a format mismatch is an error, never garbage', () => {
    it.each([
      ['sample rate', { rate: 24000 }],
      ['channel count', { channels: 2 }],
      ['bits per sample', { bits: 8 }],
    ] as const)('rejects parts that differ in %s', async (_name, other) => {
      await expect(concatWavBlobs([wav([1, 0, 2, 0]), wav([1, 0, 2, 0], other)])).rejects.toThrow(
        /format/,
      );
    });

    it('rejects a mismatch in a later part, not only the second', async () => {
      await expect(
        concatWavBlobs([wav([1, 0]), wav([2, 0]), wav([3, 0], { rate: 8000 })]),
      ).rejects.toThrow(/format/);
    });
  });

  describe('chunks around the data chunk', () => {
    const list = chunk('LIST', Uint8Array.from([9, 9, 9, 9, 9, 9, 9, 9]));
    // An odd-sized chunk is followed by a pad byte the parser must step over.
    const odd = chunk('junk', Uint8Array.from([7, 7, 7]));

    it('finds the data after an extra chunk before it and leaves the extra chunk out', async () => {
      const out = await concatWavBlobs([
        wav([1, 0, 2, 0], { before: [list, odd] }),
        wav([3, 0], { before: [odd] }),
      ]);

      const parsed = readCanonical(await bytesOf(out));
      expect(parsed.data).toEqual([1, 0, 2, 0, 3, 0]);
      expect(parsed.dataSize).toBe(6);
    });

    it('stops at the data chunk and ignores a chunk after it', async () => {
      const out = await concatWavBlobs([
        wav([1, 0, 2, 0], { after: [list] }),
        wav([3, 0, 4, 0], { after: [odd, list] }),
      ]);

      const parsed = readCanonical(await bytesOf(out));
      expect(parsed.data).toEqual([1, 0, 2, 0, 3, 0, 4, 0]);
    });
  });

  describe('a data chunk that does not match the file', () => {
    it('reads to the end of the file when the declared size runs past it', async () => {
      // 0xFFFFFFFF is what a streaming writer leaves in the header; a truncated file is the
      // other way to get here. Either way the data is what the file really holds.
      const out = await concatWavBlobs([
        wav([1, 0, 2, 0], { dataSize: 0xffffffff }),
        wav([3, 0, 4, 0], { dataSize: 1000 }),
      ]);

      expect(readCanonical(await bytesOf(out)).data).toEqual([1, 0, 2, 0, 3, 0, 4, 0]);
    });

    it('drops a trailing partial frame so the next part stays on a frame boundary', async () => {
      const out = await concatWavBlobs([
        // three bytes: one whole 16-bit frame and one stray byte
        wav([1, 0, 99], { dataSize: 3 }),
        wav([2, 0, 3, 0]),
      ]);

      const parsed = readCanonical(await bytesOf(out));
      expect(parsed.data).toEqual([1, 0, 2, 0, 3, 0]);
      expect(parsed.dataSize).toBe(6);
    });
  });

  describe('input that is not a WAV this can stitch', () => {
    const good = () => wav([1, 0, 2, 0]);

    it('rejects a file that is not RIFF/WAVE', async () => {
      const junk = new Blob([new Uint8Array(64).fill(7)], { type: 'audio/wav' });

      await expect(concatWavBlobs([good(), junk])).rejects.toThrow(/RIFF\/WAVE/);
    });

    it('rejects a file too short to hold a header', async () => {
      await expect(concatWavBlobs([good(), new Blob([new Uint8Array(5)])])).rejects.toThrow(
        /RIFF\/WAVE/,
      );
    });

    it('rejects a file with no data chunk', async () => {
      const noData = new Blob([riff(chunk('fmt ', fmtBody(16000, 1, 16)))]);

      await expect(concatWavBlobs([good(), noData])).rejects.toThrow(/data/);
    });

    it('rejects a file with no fmt chunk before its data', async () => {
      const noFmt = new Blob([riff(chunk('data', Uint8Array.from([1, 0])))]);

      await expect(concatWavBlobs([good(), noFmt])).rejects.toThrow(/fmt/);
    });

    it('rejects a fmt chunk too small to be PCM', async () => {
      const tiny = new Blob([
        riff(chunk('fmt ', new Uint8Array(8)), chunk('data', new Uint8Array(2))),
      ]);

      await expect(concatWavBlobs([good(), tiny])).rejects.toThrow(/fmt/);
    });

    it('rejects a sample format other than integer PCM (re-wrapping would mislabel it)', async () => {
      await expect(
        concatWavBlobs([
          wav([0, 0, 128, 63], { bits: 32, tag: 3 }),
          wav([0, 0, 128, 63], { bits: 32, tag: 3 }),
        ]),
      ).rejects.toThrow(/PCM/);
    });
  });
});
