// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { AudioDecodeError, audioBlobToWav } from './audioToWav';

// jsdom has no WebAudio: no AudioContext, no decodeAudioData, no OfflineAudioContext. These fakes
// stand in for the parts the converter uses, with the two results a test cares about under its
// control -- what the browser "decodes" the upload to, and what the offline render "returns" (a
// mono 16 kHz buffer, which is what the real one makes of the decoded one). The real downmix and
// resample are the browser's own and cannot be run here; what is tested is that the converter asks
// for them (the offline context's shape, the wiring) and encodes what comes back.

// An AudioBuffer as far as the converter reads one.
class FakeAudioBuffer {
  readonly numberOfChannels: number;
  readonly length: number;
  constructor(
    readonly sampleRate: number,
    private readonly channels: Float32Array[],
  ) {
    this.numberOfChannels = channels.length;
    this.length = channels[0]?.length ?? 0;
  }
  get duration() {
    return this.length / this.sampleRate;
  }
  getChannelData(channel: number) {
    return this.channels[channel];
  }
}

// What the fakes are told to do, replaced for every test.
type WebAudio = {
  decode: (data: ArrayBuffer) => Promise<FakeAudioBuffer>;
  render: () => Promise<FakeAudioBuffer>;
  closeFails: boolean;
  contexts: FakeAudioContext[];
  offline: FakeOfflineAudioContext[];
};
let webAudio: WebAudio;

class FakeAudioContext {
  closed = false;
  readonly decodeAudioData = vi.fn((data: ArrayBuffer) => webAudio.decode(data));
  readonly close = vi.fn(async () => {
    this.closed = true;
    if (webAudio.closeFails) throw new DOMException('already closed', 'InvalidStateError');
  });
  constructor() {
    webAudio.contexts.push(this);
  }
}

class FakeOfflineAudioContext {
  readonly destination = { kind: 'destination' };
  readonly source = { buffer: null as unknown, connect: vi.fn(), start: vi.fn() };
  readonly startRendering = vi.fn(() => webAudio.render());
  constructor(
    readonly numberOfChannels: number,
    readonly length: number,
    readonly sampleRate: number,
  ) {
    webAudio.offline.push(this);
  }
  createBufferSource() {
    return this.source;
  }
}

// A buffer of one channel per array, at `sampleRate`.
const bufferOf = (sampleRate: number, ...channels: number[][]) =>
  new FakeAudioBuffer(
    sampleRate,
    channels.map((samples) => Float32Array.from(samples)),
  );

// The bytes of a blob (a FileReader: this jsdom's Blob has no arrayBuffer()).
function bytesOf(blob: Blob): Promise<Uint8Array> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve(new Uint8Array(reader.result as ArrayBuffer));
    reader.onerror = () => reject(new Error('read failed'));
    reader.readAsArrayBuffer(blob);
  });
}

const ascii = (bytes: Uint8Array, from: number, to: number) =>
  String.fromCharCode(...bytes.slice(from, to));
const viewOf = (bytes: Uint8Array) =>
  new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);

const upload = () => new Blob(['webm-bytes'], { type: 'audio/webm;codecs=opus' });

beforeEach(() => {
  webAudio = {
    // 24 stereo frames at 48 kHz are 8 frames at 16 kHz.
    decode: async () => bufferOf(48000, new Array(24).fill(0), new Array(24).fill(0)),
    render: async () => bufferOf(16000, [0, 0.5, -0.5, 1, -1, 1.5, -1.5, 0.25]),
    closeFails: false,
    contexts: [],
    offline: [],
  };
  vi.stubGlobal('AudioContext', FakeAudioContext);
  vi.stubGlobal('OfflineAudioContext', FakeOfflineAudioContext);
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('audioBlobToWav', () => {
  it('returns an audio/wav blob', async () => {
    const wav = await audioBlobToWav(upload());

    expect(wav).toBeInstanceOf(Blob);
    expect(wav.type).toBe('audio/wav');
  });

  it('writes a canonical 44-byte PCM header: mono, 16 kHz, 16 bit', async () => {
    const bytes = await bytesOf(await audioBlobToWav(upload()));
    const view = viewOf(bytes);
    const dataBytes = 8 * 2;

    expect(ascii(bytes, 0, 4)).toBe('RIFF');
    // The RIFF size counts everything after its own two fields: the rest of the header, the data.
    expect(view.getUint32(4, true)).toBe(36 + dataBytes);
    expect(ascii(bytes, 8, 12)).toBe('WAVE');
    expect(ascii(bytes, 12, 16)).toBe('fmt ');
    expect(view.getUint32(16, true)).toBe(16); // the fmt chunk's size
    expect(view.getUint16(20, true)).toBe(1); // PCM
    expect(view.getUint16(22, true)).toBe(1); // channels
    expect(view.getUint32(24, true)).toBe(16000); // sample rate
    expect(view.getUint32(28, true)).toBe(16000 * 2); // byte rate
    expect(view.getUint16(32, true)).toBe(2); // block align
    expect(view.getUint16(34, true)).toBe(16); // bits per sample
    expect(ascii(bytes, 36, 40)).toBe('data');
    expect(view.getUint32(40, true)).toBe(dataBytes);
    expect(bytes.length).toBe(44 + dataBytes);
  });

  it('encodes the rendered samples as little-endian int16, clamped to [-1, 1]', async () => {
    const bytes = await bytesOf(await audioBlobToWav(upload()));
    const view = viewOf(bytes);
    const samples = Array.from({ length: 8 }, (_, i) => view.getInt16(44 + i * 2, true));

    // 0, +/-0.5, full scale either way, +/-1.5 clamped to full scale, 0.25.
    expect(samples).toEqual([0, 16384, -16384, 32767, -32768, 32767, -32768, 8192]);
  });

  it('decodes the bytes of the upload', async () => {
    let received: Uint8Array | undefined;
    webAudio.decode = async (data) => {
      received = new Uint8Array(data);
      return bufferOf(48000, new Array(24).fill(0));
    };

    await audioBlobToWav(upload());

    expect(received && String.fromCharCode(...received)).toBe('webm-bytes');
  });

  it('asks for a mono 16 kHz render of the decoded buffer, sized to the resampled length', async () => {
    const decoded = bufferOf(48000, new Array(24).fill(0), new Array(24).fill(0));
    webAudio.decode = async () => decoded;

    await audioBlobToWav(upload());

    expect(webAudio.offline).toHaveLength(1);
    const offline = webAudio.offline[0];
    expect([offline.numberOfChannels, offline.length, offline.sampleRate]).toEqual([1, 8, 16000]);
    // The decoded buffer is what the render plays, from the start, into the context's output.
    expect(offline.source.buffer).toBe(decoded);
    expect(offline.source.connect).toHaveBeenCalledWith(offline.destination);
    expect(offline.source.start).toHaveBeenCalledWith(0);
    expect(offline.startRendering).toHaveBeenCalledTimes(1);
  });

  it('rounds the rendered length up, so the tail of the clip is not cut off', async () => {
    // 100 frames at 44.1 kHz are 36.28 frames at 16 kHz.
    webAudio.decode = async () => bufferOf(44100, new Array(100).fill(0));

    await audioBlobToWav(upload());

    expect(webAudio.offline[0].length).toBe(37);
  });

  it('closes the decoding context, which holds audio resources', async () => {
    await audioBlobToWav(upload());

    expect(webAudio.contexts).toHaveLength(1);
    expect(webAudio.contexts[0].close).toHaveBeenCalledTimes(1);
  });

  it('does not let a context that cannot be closed (already closed) fail the conversion', async () => {
    webAudio.closeFails = true;

    const wav = await audioBlobToWav(upload());

    expect(wav.type).toBe('audio/wav');
    expect(webAudio.contexts[0].close).toHaveBeenCalledTimes(1);
  });

  it('falls back to the prefixed contexts of an older Safari', async () => {
    vi.stubGlobal('AudioContext', undefined);
    vi.stubGlobal('OfflineAudioContext', undefined);
    vi.stubGlobal('webkitAudioContext', FakeAudioContext);
    vi.stubGlobal('webkitOfflineAudioContext', FakeOfflineAudioContext);

    const wav = await audioBlobToWav(upload());

    expect(wav.type).toBe('audio/wav');
    expect(webAudio.contexts).toHaveLength(1);
    expect(webAudio.offline).toHaveLength(1);
  });

  describe('refusing what cannot be converted, as an AudioDecodeError', () => {
    it('refuses audio the browser cannot decode', async () => {
      const failure = new DOMException('Unable to decode audio data', 'EncodingError');
      webAudio.decode = () => Promise.reject(failure);

      const result = audioBlobToWav(upload());

      await expect(result).rejects.toBeInstanceOf(AudioDecodeError);
      // What the browser said is kept for debugging.
      await expect(result).rejects.toMatchObject({ cause: failure });
    });

    it('still closes the context when the decode failed', async () => {
      webAudio.decode = () => Promise.reject(new DOMException('bad', 'EncodingError'));

      await expect(audioBlobToWav(upload())).rejects.toBeInstanceOf(AudioDecodeError);

      expect(webAudio.contexts[0].close).toHaveBeenCalledTimes(1);
    });

    it('refuses a clip whose render fails', async () => {
      webAudio.render = () => Promise.reject(new Error('render failed'));

      await expect(audioBlobToWav(upload())).rejects.toBeInstanceOf(AudioDecodeError);
      expect(webAudio.contexts[0].close).toHaveBeenCalledTimes(1);
    });

    // A zero-length offline context is itself an error in a browser, so a clip with no frames is
    // refused before a render is even built.
    it.each([
      ['no channels', () => bufferOf(48000)],
      ['no frames', () => bufferOf(48000, [])],
    ])('refuses audio that decodes to %s', async (_name, decoded) => {
      webAudio.decode = async () => decoded();

      await expect(audioBlobToWav(upload())).rejects.toBeInstanceOf(AudioDecodeError);
      expect(webAudio.offline).toHaveLength(0);
      expect(webAudio.contexts[0].close).toHaveBeenCalledTimes(1);
    });

    // A browser that lacks either half cannot convert: refused up front, before anything is read
    // or opened.
    it.each([
      ['an AudioContext', 'AudioContext'],
      ['an OfflineAudioContext', 'OfflineAudioContext'],
    ])('refuses a browser without %s', async (_name, constructor) => {
      vi.stubGlobal(constructor, undefined);

      await expect(audioBlobToWav(upload())).rejects.toBeInstanceOf(AudioDecodeError);
      expect(webAudio.contexts).toHaveLength(0);
      expect(webAudio.offline).toHaveLength(0);
    });

    it('names itself, so a caller can tell it from any other failure', async () => {
      webAudio.decode = () => Promise.reject(new Error('bad'));

      await expect(audioBlobToWav(upload())).rejects.toMatchObject({ name: 'AudioDecodeError' });
    });
  });
});
