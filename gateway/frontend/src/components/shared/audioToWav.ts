// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Converting an audio clip to WAV in the browser. The transcription endpoint of the audio server
// reads WAV only (a webm, ogg, m4a or mp3 upload is refused with a 400), while the browser's
// recorder makes webm/opus (Chromium), ogg (Firefox) or mp4/aac (Safari) and a user may attach
// anything. So the composer decodes whatever the browser can decode (WebAudio's decodeAudioData
// reads the bytes, not the type), downmixes it to mono, resamples it to 16 kHz -- the rate speech
// models take, which also keeps a clip small -- and writes it as 16-bit PCM. No library: WebAudio
// does the decoding, the mixing and the resampling, and the encoding is a 44-byte header.

// The media type of what audioBlobToWav returns, and so of every clip the composer attaches.
export const WAV_MEDIA_TYPE = 'audio/wav';

// What the converted clip is: mono, 16 kHz, 16-bit samples. The channel count and the sample width
// are exported for the live capture (chat/livePcm.ts), which writes the same PCM format.
const SAMPLE_RATE = 16000;
export const CHANNELS = 1;
export const BYTES_PER_SAMPLE = 2;

// The canonical RIFF/WAVE header of an uncompressed PCM file is 44 bytes: the RIFF chunk's
// header (12), the `fmt ` chunk (24) and the `data` chunk's header (8).
const HEADER_BYTES = 44;

// Audio the browser could not turn into a WAV: bytes it cannot decode (not audio, corrupt, a
// codec this browser lacks), a browser with no WebAudio, or a clip with nothing in it. The
// composer tells the user so, as opposed to any other failure.
export class AudioDecodeError extends Error {
  // `Error(message, { cause })` and the global `ErrorOptions` type only exist under lib ES2022+;
  // this project targets ES2020, so forward the cause onto a declared field manually (as
  // ImageAttachError does) to keep what the browser said for debugging.
  readonly cause?: unknown;
  constructor(message = 'audio could not be decoded', options?: { cause?: unknown }) {
    super(message);
    this.name = 'AudioDecodeError';
    if (options && 'cause' in options) this.cause = options.cause;
  }
}

// The WebAudio constructors of this browser: the standard ones, or Safari's prefixed ones before
// 14.1. Looked up on every call, not once at load, so a page that lacks them still loads.
function webAudio(): {
  Context: typeof AudioContext;
  Offline: typeof OfflineAudioContext;
} {
  const g = globalThis as unknown as {
    AudioContext?: typeof AudioContext;
    webkitAudioContext?: typeof AudioContext;
    OfflineAudioContext?: typeof OfflineAudioContext;
    webkitOfflineAudioContext?: typeof OfflineAudioContext;
  };
  const Context = g.AudioContext ?? g.webkitAudioContext;
  const Offline = g.OfflineAudioContext ?? g.webkitOfflineAudioContext;
  if (!Context || !Offline) throw new AudioDecodeError('this browser has no WebAudio');
  return { Context, Offline };
}

// Decodes the bytes of a clip with whatever codecs the browser has. The buffer comes back at the
// context's own sample rate and with the clip's own channels. Nothing in it (no channel, no
// frame) is as undecodable as bytes the browser refused: a render of zero frames is an error.
async function decode(context: AudioContext, bytes: ArrayBuffer): Promise<AudioBuffer> {
  let decoded: AudioBuffer | undefined;
  try {
    decoded = await context.decodeAudioData(bytes);
  } catch (cause) {
    throw new AudioDecodeError('the browser could not decode the audio', { cause });
  }
  // `undefined` is what a Safari older than 14.1, whose decodeAudioData takes callbacks only,
  // gives back.
  if (!decoded || decoded.numberOfChannels === 0 || decoded.length === 0) {
    throw new AudioDecodeError('the audio has no samples');
  }
  return decoded;
}

// Renders the decoded clip into one channel at 16 kHz, which is how the browser downmixes (the
// speakers rule: stereo to mono is the mean of the two, a surround layout folds down by the
// standard's matrix) and resamples (an offline context renders at its own rate): the samples of
// that one channel. Rounded up to a whole frame, so the tail of the clip is not cut off.
async function renderMono16k(
  Offline: typeof OfflineAudioContext,
  decoded: AudioBuffer,
): Promise<Float32Array> {
  const frames = Math.ceil((decoded.length * SAMPLE_RATE) / decoded.sampleRate);
  try {
    const offline = new Offline(CHANNELS, frames, SAMPLE_RATE);
    const source = offline.createBufferSource();
    source.buffer = decoded;
    source.connect(offline.destination);
    source.start(0);
    const rendered = await offline.startRendering();
    return rendered.getChannelData(0);
  } catch (cause) {
    throw new AudioDecodeError('the browser could not resample the audio', { cause });
  }
}

function writeAscii(view: DataView, offset: number, text: string) {
  for (let i = 0; i < text.length; i++) view.setUint8(offset + i, text.codePointAt(i) ?? 0);
}

// A float sample as 16-bit PCM: clamped to [-1, 1] (a decoder may overshoot), then scaled so that
// -1 is -32768 and +1 is +32767, the two ends of an int16. Shared with the live capture.
export function toInt16(sample: number): number {
  const clamped = Math.max(-1, Math.min(1, sample));
  return Math.round(clamped < 0 ? clamped * 0x8000 : clamped * 0x7fff);
}

// The samples as a canonical 16-bit PCM WAV file (little-endian throughout).
function encodeWav(samples: Float32Array): Blob {
  const dataBytes = samples.length * BYTES_PER_SAMPLE;
  const buffer = new ArrayBuffer(HEADER_BYTES + dataBytes);
  const view = new DataView(buffer);
  const blockAlign = CHANNELS * BYTES_PER_SAMPLE;

  writeAscii(view, 0, 'RIFF');
  view.setUint32(4, HEADER_BYTES - 8 + dataBytes, true); // everything after this field
  writeAscii(view, 8, 'WAVE');
  writeAscii(view, 12, 'fmt ');
  view.setUint32(16, 16, true); // the fmt chunk's size: the PCM form has no extension
  view.setUint16(20, 1, true); // format 1: PCM
  view.setUint16(22, CHANNELS, true);
  view.setUint32(24, SAMPLE_RATE, true);
  view.setUint32(28, SAMPLE_RATE * blockAlign, true); // bytes per second
  view.setUint16(32, blockAlign, true);
  view.setUint16(34, BYTES_PER_SAMPLE * 8, true); // bits per sample
  writeAscii(view, 36, 'data');
  view.setUint32(40, dataBytes, true);
  for (let i = 0; i < samples.length; i++) {
    view.setInt16(HEADER_BYTES + i * BYTES_PER_SAMPLE, toInt16(samples[i]), true);
  }
  return new Blob([buffer], { type: WAV_MEDIA_TYPE });
}

// Converts an audio clip -- any format the browser can decode, whatever its type says -- to a mono
// 16 kHz 16-bit PCM WAV blob of type audio/wav. Throws an AudioDecodeError for audio the browser
// cannot turn into one (or has no WebAudio for). A blob that cannot be read at all is no decode
// failure: whatever Blob#arrayBuffer() rejects with comes through as it is.
// The audio context that decoded the clip is closed whatever happens: a page may hold only a few
// of them, and each pins audio resources until it is.
export async function audioBlobToWav(input: Blob): Promise<Blob> {
  const { Context, Offline } = webAudio();
  const bytes = await input.arrayBuffer();
  const context = new Context();
  try {
    const decoded = await decode(context, bytes);
    return encodeWav(await renderMono16k(Offline, decoded));
  } finally {
    try {
      await context.close();
    } catch {
      // Already closed: nothing left to release.
    }
  }
}
