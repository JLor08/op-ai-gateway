// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// The audio half of live transcription (#266), as plain functions and classes: what the capture
// worklet (captureWorklet.ts) does to each block of microphone samples, and what the session
// (liveTranscription.ts) and the worklet agree on. Kept apart from both so that it can be
// tested without a browser: the worklet is a thin shell around these (it runs in the audio
// thread, where nothing can be reached from a test), and this file imports nothing that needs
// a window, so it can be bundled into the worklet.
//
// The stream the gateway takes is fixed: mono, signed 16-bit little-endian PCM, at
// LIVE_TRANSCRIPTION_SAMPLE_RATE. A microphone gives floats at the audio context's own rate
// (usually 48 kHz), so each block goes through
//
//   LinearResampler (context rate -> 16 kHz)  ->  Pcm16Batcher (s16le, ~100 ms per frame)
//
// and each full batch is posted to the page, which sends it as one binary WebSocket frame.

import { BYTES_PER_SAMPLE, CHANNELS, toInt16 } from '../shared/audioToWav';

// The name the capture processor registers under, for AudioWorkletNode to find it by.
export const LIVE_CAPTURE_PROCESSOR_NAME = 'live-pcm-capture';

// The two messages the page and the worklet exchange besides PCM (which is an ArrayBuffer). The
// page asks for the batch still being filled -- the last fraction of a second of speech, which
// would otherwise never leave the worklet -- and the worklet answers once that batch (if any)
// has been posted, so that "flushed" follows every byte of PCM on the same, ordered, port.
export const LIVE_FLUSH_MESSAGE = 'flush';
export const LIVE_FLUSHED_MESSAGE = 'flushed';

// How much audio goes into one binary frame, in seconds. Short, so that the transcript follows
// the speech closely and a frame is a small send; long enough that the per-frame overhead is
// nothing. 100 ms at 16 kHz is 1600 samples, 3200 bytes.
export const LIVE_BATCH_SECONDS = 0.1;

// The gateway's cap on one inbound binary frame (maxLiveFrameBytes): a frame of this size or
// more closes the connection with 1009. A batch is two orders below it; this constant exists so
// that the relation is written down and tested, not because a batch could come near it.
export const MAX_LIVE_FRAME_BYTES = 1 << 20;

/** The number of samples in one ~100 ms batch at `rate` Hz (a whole number, at least 1). */
export function liveBatchSamples(rate: number): number {
  return Math.max(1, Math.round(rate * LIVE_BATCH_SECONDS));
}

/**
 * Resamples a mono float stream from one rate to another by linear interpolation, block by
 * block. It is meant for streaming: the position of the next output sample is carried from one
 * block to the next (as a fraction of an input sample, which at 44.1 kHz is not a whole
 * number), as is the last sample of the previous block, so that cutting the stream into blocks
 * anywhere changes nothing -- no gap, no repeated sample, no click at the seams.
 *
 * Linear interpolation is not a band-limited resampler: going down it does not filter out what
 * the lower rate cannot hold, so energy above 8 kHz folds back into the speech band at low
 * level. Speech recognition does not hear that, and it keeps this a few lines long.
 *
 * An output sample at position p (in input samples, p = 0 the first input sample ever) needs
 * the input sample on each side of p, so the stream is one input sample ahead of what has been
 * emitted: the last sample of a block is only used once the first of the next has arrived.
 */
export class LinearResampler {
  // The input samples that make one output sample: above 1 going down, below 1 going up.
  private readonly step: number;
  // Where the next output sample sits, in input samples from the start of the next block. Never
  // below -1, which is the last sample of the previous block (`previous`).
  private position = 0;
  private previous = 0;

  constructor(inputRate: number, outputRate: number) {
    if (!(inputRate > 0 && Number.isFinite(inputRate))) {
      throw new Error(`invalid input sample rate: ${inputRate}`);
    }
    if (!(outputRate > 0 && Number.isFinite(outputRate))) {
      throw new Error(`invalid output sample rate: ${outputRate}`);
    }
    this.step = inputRate / outputRate;
  }

  /** The output samples this block completes (possibly none). A fresh array every call. */
  process(input: Float32Array): Float32Array {
    const length = input.length;
    if (length === 0) return new Float32Array(0);
    const out = new Float32Array(Math.ceil((length + 1) / this.step) + 1);
    let count = 0;
    let position = this.position;
    // A sample at `position` lies between input[i] and input[i + 1]; i is -1 while it lies
    // between the last sample of the previous block and the first of this one.
    while (position < length - 1) {
      const i = Math.floor(position);
      const fraction = position - i;
      const a = i < 0 ? this.previous : input[i];
      out[count++] = a + (input[i + 1] - a) * fraction;
      position += this.step;
    }
    this.position = position - length;
    this.previous = input[length - 1];
    return out.subarray(0, count);
  }
}

/**
 * Collects float samples into fixed-size batches of 16-bit little-endian PCM. `push` hands every
 * batch it completes to `emit` as a fresh ArrayBuffer (which the worklet transfers to the page,
 * so the batcher never touches one again); `flush` hands over the batch being filled, at its
 * true length, or nothing if it is empty. The float to int16 conversion is the one the WAV
 * encoder of the recorder uses.
 */
export class Pcm16Batcher {
  private buffer!: ArrayBuffer;
  private view!: DataView;
  private filled = 0;

  constructor(private readonly batchSamples: number) {
    if (!Number.isInteger(batchSamples) || batchSamples < 1) {
      throw new Error(`invalid batch size: ${batchSamples}`);
    }
    this.start();
  }

  push(samples: Float32Array, emit: (pcm: ArrayBuffer) => void): void {
    for (const sample of samples) {
      // Explicitly little-endian: the wire format is s16le whatever the platform's byte order.
      this.view.setInt16(this.filled * BYTES_PER_SAMPLE, toInt16(sample), true);
      if (++this.filled === this.batchSamples) {
        const full = this.buffer;
        this.start();
        emit(full);
      }
    }
  }

  flush(emit: (pcm: ArrayBuffer) => void): void {
    if (this.filled === 0) return;
    const partial = this.buffer.slice(0, this.filled * BYTES_PER_SAMPLE);
    this.start();
    emit(partial);
  }

  private start(): void {
    this.buffer = new ArrayBuffer(this.batchSamples * CHANNELS * BYTES_PER_SAMPLE);
    this.view = new DataView(this.buffer);
    this.filled = 0;
  }
}
