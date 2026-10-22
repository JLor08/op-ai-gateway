// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Stitching the stored parts of one long speech answer back into ONE WAV in the browser.
//
// A long answer is saved as several ordered `audio_url` parts (one chat blob each), every part a
// standalone 16-bit PCM WAV with its own header. Played one after the other they would gap at
// every seam and offer N downloads, so the player joins them: it reads each part's `data` chunk,
// writes the PCM end to end behind ONE canonical header and hands the native <audio> element
// that single clip. It is the mirror of what the gateway does when it writes the parts (strip a
// header, keep the PCM, wrap once).
//
// Joining is only valid for parts of one format -- they come from one model and one synthesis,
// so they share it -- and a mismatch throws instead of producing samples played at the wrong
// rate or channel count.

export const WAV_MEDIA_TYPE = 'audio/wav';

// The canonical RIFF/WAVE header of an uncompressed PCM file: the RIFF chunk's header (12), the
// 16-byte `fmt ` chunk (24 with its own header) and the `data` chunk's header (8).
const HEADER_BYTES = 44;
// The part of a RIFF chunk before its body: a 4-byte id and a 4-byte little-endian size.
const CHUNK_HEADER_BYTES = 8;
// The size of the `fmt ` chunk's fields up to bits-per-sample (the PCM form).
const FMT_PCM_BYTES = 16;
// Integer PCM, the only sample format the gateway writes and the header below can describe.
const FORMAT_PCM = 1;
// RIFF sizes are 32-bit: the largest `data` chunk whose RIFF size still fits.
const MAX_DATA_BYTES = 0xffffffff - (HEADER_BYTES - CHUNK_HEADER_BYTES);

// What a part's `fmt ` chunk says about its samples, and where its PCM is.
interface WavPart {
  channels: number;
  sampleRate: number;
  bitsPerSample: number;
  // The PCM itself, a view into the part's blob: whole frames only. A Blob slice is a reference
  // rather than a copy, so a long answer is not held in memory twice while it is stitched.
  pcm: Blob;
}

// The bytes of one frame (one sample of every channel); a sample takes whole bytes.
function blockAlignOf(format: Omit<WavPart, 'pcm'>): number {
  return format.channels * Math.ceil(format.bitsPerSample / 8);
}

// Reads `length` bytes of the blob from `start`; fewer when the blob ends first.
async function readBytes(blob: Blob, start: number, length: number): Promise<DataView> {
  return new DataView(await blob.slice(start, start + length).arrayBuffer());
}

function fourCC(view: DataView, offset: number): string {
  return String.fromCharCode(
    view.getUint8(offset),
    view.getUint8(offset + 1),
    view.getUint8(offset + 2),
    view.getUint8(offset + 3),
  );
}

// Walks the chunks of one WAV (RIFF/WAVE, then `id size body` triples, bodies padded to an even
// length) to its `fmt ` and `data` chunks. Chunks around them (LIST and the like) are stepped
// over, and whatever follows `data` is not read. Anything that is not an integer-PCM WAV throws.
async function parseWav(blob: Blob, index: number): Promise<WavPart> {
  const label = `WAV part ${index + 1}`;
  const top = await readBytes(blob, 0, 12);
  if (top.byteLength < 12 || fourCC(top, 0) !== 'RIFF' || fourCC(top, 8) !== 'WAVE') {
    throw new Error(`${label} is not a RIFF/WAVE file`);
  }

  let format: Omit<WavPart, 'pcm'> | undefined;
  let offset = 12;
  while (offset + CHUNK_HEADER_BYTES <= blob.size) {
    const header = await readBytes(blob, offset, CHUNK_HEADER_BYTES);
    const id = fourCC(header, 0);
    const size = header.getUint32(4, true);
    const body = offset + CHUNK_HEADER_BYTES;

    if (id === 'fmt ') {
      if (size < FMT_PCM_BYTES) throw new Error(`${label} has a fmt chunk too small for PCM`);
      const fmt = await readBytes(blob, body, FMT_PCM_BYTES);
      if (fmt.byteLength < FMT_PCM_BYTES) throw new Error(`${label} has a truncated fmt chunk`);
      if (fmt.getUint16(0, true) !== FORMAT_PCM) {
        throw new Error(`${label} is not integer PCM`);
      }
      format = {
        channels: fmt.getUint16(2, true),
        sampleRate: fmt.getUint32(4, true),
        bitsPerSample: fmt.getUint16(14, true),
      };
      if (format.channels === 0 || format.bitsPerSample === 0) {
        throw new Error(`${label} has an empty sample format`);
      }
    } else if (id === 'data') {
      if (!format) throw new Error(`${label} has no fmt chunk before its data`);
      // A header may declare more than the file holds (a streaming writer leaves 0xFFFFFFFF, a
      // truncated file simply ends early): the data is what is really there. A stray partial
      // frame at the end is dropped so the next part starts on a frame boundary.
      const available = Math.min(size, blob.size - body);
      const frameBytes = blockAlignOf(format);
      const whole = available - (available % frameBytes);
      return { ...format, pcm: blob.slice(body, body + whole) };
    }
    offset = body + size + (size % 2);
  }
  throw new Error(format ? `${label} has no data chunk` : `${label} has no fmt chunk`);
}

function sameFormat(a: Omit<WavPart, 'pcm'>, b: Omit<WavPart, 'pcm'>): boolean {
  return (
    a.channels === b.channels &&
    a.sampleRate === b.sampleRate &&
    a.bitsPerSample === b.bitsPerSample
  );
}

function writeAscii(view: DataView, offset: number, text: string) {
  for (let i = 0; i < text.length; i += 1) view.setUint8(offset + i, text.codePointAt(i) ?? 0);
}

// The 44-byte header of a PCM WAV holding `dataBytes` of the given format.
function canonicalHeader(format: Omit<WavPart, 'pcm'>, dataBytes: number): Uint8Array<ArrayBuffer> {
  const out = new Uint8Array(HEADER_BYTES);
  const view = new DataView(out.buffer);
  const blockAlign = blockAlignOf(format);
  writeAscii(view, 0, 'RIFF');
  view.setUint32(4, HEADER_BYTES - CHUNK_HEADER_BYTES + dataBytes, true); // all after this field
  writeAscii(view, 8, 'WAVE');
  writeAscii(view, 12, 'fmt ');
  view.setUint32(16, FMT_PCM_BYTES, true);
  view.setUint16(20, FORMAT_PCM, true);
  view.setUint16(22, format.channels, true);
  view.setUint32(24, format.sampleRate, true);
  view.setUint32(28, format.sampleRate * blockAlign, true); // bytes per second
  view.setUint16(32, blockAlign, true);
  view.setUint16(34, format.bitsPerSample, true);
  writeAscii(view, 36, 'data');
  view.setUint32(40, dataBytes, true);
  return out;
}

// Joins WAV parts, in order, into one audio/wav blob: the header is written once from the first
// part's format, and the data is every part's PCM back to back. A single blob is returned as it
// is -- the same object, not parsed or copied -- so a one-part answer (every clip saved before
// answers were split, every short one) plays exactly the bytes the server served. Rejects for
// an empty list, a part that is not an integer-PCM WAV, or parts whose rate, channel count or
// sample width differ.
export async function concatWavBlobs(blobs: Blob[]): Promise<Blob> {
  if (blobs.length === 0) throw new Error('no WAV parts to concatenate');
  if (blobs.length === 1) return blobs[0];

  const parts = await Promise.all(blobs.map((blob, i) => parseWav(blob, i)));
  const first = parts[0];
  const mismatch = parts.findIndex((part) => !sameFormat(first, part));
  if (mismatch !== -1) {
    throw new Error(`WAV part ${mismatch + 1} has a different format than part 1`);
  }
  const dataBytes = parts.reduce((total, part) => total + part.pcm.size, 0);
  if (dataBytes > MAX_DATA_BYTES) throw new Error('the joined WAV is too large for one file');

  return new Blob([canonicalHeader(first, dataBytes), ...parts.map((part) => part.pcm)], {
    type: WAV_MEDIA_TYPE,
  });
}
