// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// The audio half of streaming speech PLAYBACK (#347), as a plain class and a few constants:
// the ring buffer the playback worklet (playbackWorklet.ts) drains, and the names the page and
// that worklet agree on. Kept apart from both so it can be tested without a browser -- the
// worklet is a thin shell around this (it runs in the audio thread, where nothing can be reached
// from a test), and this file imports nothing that needs a window, so it can be bundled into the
// worklet.
//
// Why a ring buffer at all: TTS audio used to be played by scheduling one AudioBufferSourceNode
// per chunk from the main JS thread. On a phone, with the microphone capture worklet also
// running, the main thread jitters and the next chunk's start time slips -> a gap at the chunk
// seam -> an audible dropout (#347). Instead, the page now fills this ring buffer and an
// AudioWorkletProcessor on the real-time audio thread drains one render quantum from it each
// callback: one continuous stream, so main-thread/capture jitter no longer opens gaps.
//
// The page resamples each chunk to the context's own rate before writing (see speechPlayback.ts),
// so this buffer is a plain mono float stream at the output rate -- no rates, no formats here.

// The name the playback processor registers under, for AudioWorkletNode to find it by.
export const STREAMING_PCM_PLAYBACK_PROCESSOR_NAME = 'streaming-pcm-playback';

// Control messages the page sends the worklet (besides PCM, which is a Float32Array). A pause
// holds the playout where it is -- the worklet outputs silence and drains nothing, so the ring
// keeps the unplayed tail -- and a resume continues from there. A stop ends the processor.
export const PLAYBACK_PAUSE_MESSAGE = 'pause';
export const PLAYBACK_RESUME_MESSAGE = 'resume';
export const PLAYBACK_STOP_MESSAGE = 'stop';

// The one message the worklet sends back: the ring has drained to empty after playing audio
// (a gap between chunks, or the true end of the stream). `played` is the cumulative number of
// real samples the worklet has output so far, which the page uses to know how much is still
// pending without racing an in-flight write (see speechPlayback.ts).
export const PLAYBACK_DRAINED_MESSAGE = 'drained';
export type PlaybackDrainedMessage = { type: typeof PLAYBACK_DRAINED_MESSAGE; played: number };

// How much audio the ring buffer holds before it has to grow, in seconds. The gateway relays a
// spoken answer faster than it plays, so the ring runs ahead of the speakers by up to this much;
// generous enough that growth (below) is the rare safety valve for a very long answer, not the
// common path.
export const DEFAULT_PLAYBACK_BUFFER_SECONDS = 8;

/** The whole number of samples in `seconds` of audio at `rate` Hz (at least 1). */
export function playbackBufferCapacity(rate: number, seconds: number): number {
  return Math.max(1, Math.round(rate * seconds));
}

/**
 * A growable circular buffer of mono float samples: the page writes decoded, resampled PCM into
 * it and the worklet reads a render quantum out of it at a time. `read` always fills the whole
 * output block -- zero-filling the tail when the buffer runs dry -- and returns how many of those
 * samples were real, which is how the worklet detects the drain (an underrun) to report it.
 *
 * It never drops audio: a write larger than the free space grows the backing array (doubling
 * until it fits) and keeps every live sample in order. Growth copies the live samples once and is
 * the rare case -- the buffer is sized (DEFAULT_PLAYBACK_BUFFER_SECONDS) to hold the gateway's
 * usual lead without it -- so the audio thread's steady state is allocation-free.
 */
export class PcmRingBuffer {
  // The backing store; its length is the current capacity. `head` is the read position and
  // `count` the number of samples available to read, so the write position is (head + count).
  private data: Float32Array;
  private head = 0;
  private count = 0;

  constructor(capacity: number) {
    if (!Number.isInteger(capacity) || capacity < 1) {
      throw new Error(`invalid ring buffer capacity: ${capacity}`);
    }
    this.data = new Float32Array(capacity);
  }

  /** The number of samples the backing array can hold before it must grow. */
  get capacity(): number {
    return this.data.length;
  }

  /** The number of samples available to read. */
  get available(): number {
    return this.count;
  }

  /** Append `samples`, growing the backing array if they do not fit. A no-op for an empty block. */
  write(samples: Float32Array): void {
    const length = samples.length;
    if (length === 0) return;
    if (this.count + length > this.data.length) this.grow(this.count + length);
    const capacity = this.data.length;
    const writeAt = (this.head + this.count) % capacity;
    const firstPart = Math.min(length, capacity - writeAt);
    this.data.set(samples.subarray(0, firstPart), writeAt);
    if (firstPart < length) this.data.set(samples.subarray(firstPart), 0);
    this.count += length;
  }

  /**
   * Fill `out` with the next samples, in order: up to `available` real ones, then silence for the
   * rest of the block. Returns the count of real samples read (so the worklet knows when it has
   * run dry). Advances past the samples returned.
   */
  read(out: Float32Array): number {
    const read = Math.min(this.count, out.length);
    const capacity = this.data.length;
    const firstPart = Math.min(read, capacity - this.head);
    out.set(this.data.subarray(this.head, this.head + firstPart), 0);
    if (firstPart < read) out.set(this.data.subarray(0, read - firstPart), firstPart);
    if (read < out.length) out.fill(0, read);
    this.head = (this.head + read) % capacity;
    this.count -= read;
    return read;
  }

  // Grow the backing array to at least `minCapacity` samples (doubling), moving the live samples
  // to the front so the read position is 0 again.
  private grow(minCapacity: number): void {
    let capacity = this.data.length;
    while (capacity < minCapacity) capacity *= 2;
    const next = new Float32Array(capacity);
    const current = this.data;
    const currentCapacity = current.length;
    for (let i = 0; i < this.count; i++) next[i] = current[(this.head + i) % currentCapacity];
    this.data = next;
    this.head = 0;
  }
}
