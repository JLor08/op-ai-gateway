// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// The streaming speech PLAYBACK of Voice Chat (#347): an AudioWorkletProcessor, which runs in the
// audio rendering thread of the page's AudioContext, apart from the page. The page connects it to
// the destination through an AudioWorkletNode of one output channel; here each 128-sample render
// quantum is drained from a ring buffer that the page keeps filled with decoded, resampled PCM.
// Because playout is pulled continuously on the audio thread -- not scheduled per chunk from the
// jittering main thread -- a busy main thread (React, the WebSocket, the microphone capture
// worklet) no longer opens gaps between chunks, which is what dropped out the TTS on iOS (#347).
//
// All the audio logic is in playbackPcm.ts, where it is tested; this is the shell that the audio
// thread calls. It is a module of its own: speechPlayback.ts imports it with `?worker&url`, which
// makes Vite bundle it separately (with playbackPcm.ts inlined, as a worklet has no module graph
// of the app) and give the bundle's URL, with the portal's base path, to AudioWorklet.addModule.
// A plain `new URL('./playbackWorklet.ts', import.meta.url)` would serve the TypeScript source as
// it is, which no browser can run. This mirrors captureWorklet.ts, the microphone's half.

import {
  DEFAULT_PLAYBACK_BUFFER_SECONDS,
  PcmRingBuffer,
  PLAYBACK_DRAINED_MESSAGE,
  PLAYBACK_PAUSE_MESSAGE,
  PLAYBACK_RESUME_MESSAGE,
  PLAYBACK_STOP_MESSAGE,
  STREAMING_PCM_PLAYBACK_PROCESSOR_NAME,
  playbackBufferCapacity,
  type PlaybackDrainedMessage,
} from './playbackPcm';

// What the AudioWorkletGlobalScope provides, which the DOM lib that this project compiles against
// does not declare (it has AudioWorkletNode, the page's half). Only what is used.
declare const sampleRate: number;
declare class AudioWorkletProcessor {
  readonly port: MessagePort;
  constructor(options?: unknown);
}
declare function registerProcessor(name: string, processor: new (options?: never) => unknown): void;

type PlaybackOptions = { processorOptions?: { bufferSeconds?: unknown } };

class StreamingPcmPlaybackProcessor extends AudioWorkletProcessor {
  private readonly ring: PcmRingBuffer;
  private paused = false;
  private stopped = false;
  // The cumulative number of real samples output, reported with each drain so the page can tell
  // how much of what it has written is still to play, without racing an in-flight write.
  private played = 0;
  // Whether audio has been output since the last drain was reported, so an empty ring reports a
  // drain exactly once per run of audio (a gap between chunks, or the end) -- not on every idle
  // quantum before the first chunk or after the last.
  private active = false;

  constructor(options?: PlaybackOptions) {
    super(options);
    const bufferSeconds =
      typeof options?.processorOptions?.bufferSeconds === 'number'
        ? options.processorOptions.bufferSeconds
        : DEFAULT_PLAYBACK_BUFFER_SECONDS;
    this.ring = new PcmRingBuffer(playbackBufferCapacity(sampleRate, bufferSeconds));
    this.port.onmessage = (event: MessageEvent) => {
      const data = event.data;
      // PCM to play: the page transfers a Float32Array of samples already at this context's rate.
      if (data instanceof Float32Array) {
        this.ring.write(data);
        this.active = true;
        return;
      }
      if (data === PLAYBACK_PAUSE_MESSAGE) this.paused = true;
      else if (data === PLAYBACK_RESUME_MESSAGE) this.paused = false;
      else if (data === PLAYBACK_STOP_MESSAGE) this.stopped = true;
    };
  }

  process(_inputs: Float32Array[][], outputs: Float32Array[][]): boolean {
    // The processor is done: returning false lets the browser reclaim it.
    if (this.stopped) return false;
    const channel = outputs[0]?.[0];
    // Not wired to an output yet (or between graph edits): produce nothing but stay alive.
    if (!channel) return true;
    if (this.paused) {
      // Hold where we are: output silence and drain nothing, so the ring keeps the unplayed tail
      // for a resume. A pause is not a drain, so nothing is reported.
      channel.fill(0);
      return true;
    }
    this.played += this.ring.read(channel);
    if (this.active && this.ring.available === 0) {
      // The ring has drained to empty after playing audio: tell the page once, with the count of
      // real samples output so far. More audio (a later section) re-arms this.
      this.active = false;
      const message: PlaybackDrainedMessage = {
        type: PLAYBACK_DRAINED_MESSAGE,
        played: this.played,
      };
      this.port.postMessage(message);
    }
    return true;
  }
}

registerProcessor(STREAMING_PCM_PLAYBACK_PROCESSOR_NAME, StreamingPcmPlaybackProcessor);
