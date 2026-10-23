// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// The microphone capture of live transcription (#266): an AudioWorkletProcessor, which runs in
// the audio rendering thread of the page's AudioContext, apart from the page. The page connects
// the microphone to it through an AudioWorkletNode of one input channel (so the browser has
// already mixed the microphone down to mono); here each 128-sample block is resampled from the
// context's rate (usually 48 kHz) to the rate the gateway takes, written as 16-bit
// little-endian PCM, and posted to the page in ~100 ms batches, each ArrayBuffer transferred,
// not copied. The page sends every batch on as one binary WebSocket frame.
//
// All the audio logic is in livePcm.ts, where it is tested; this is the shell that the audio
// thread calls. It is a module of its own: liveTranscription.ts imports it with `?worker&url`,
// which makes Vite bundle it separately (with livePcm.ts and what that imports inlined, as a
// worklet has no module graph of the app) and give the bundle's URL, with the portal's base
// path, to AudioWorklet.addModule. A plain `new URL('./captureWorklet.ts', import.meta.url)`
// would serve the TypeScript source as it is, which no browser can run.

import {
  LIVE_CAPTURE_PROCESSOR_NAME,
  LIVE_FLUSHED_MESSAGE,
  LIVE_FLUSH_MESSAGE,
  LinearResampler,
  Pcm16Batcher,
  liveBatchSamples,
} from './livePcm';

// What the AudioWorkletGlobalScope provides, which the DOM lib that this project compiles
// against does not declare (it has AudioWorkletNode, the page's half). Only what is used.
declare const sampleRate: number;
declare class AudioWorkletProcessor {
  readonly port: MessagePort;
  constructor(options?: unknown);
}
declare function registerProcessor(name: string, processor: new (options?: never) => unknown): void;

type CaptureOptions = { processorOptions?: { outputRate?: unknown } };

class LiveCaptureProcessor extends AudioWorkletProcessor {
  private readonly resampler: LinearResampler;
  private readonly batcher: Pcm16Batcher;

  constructor(options?: CaptureOptions) {
    super(options);
    // The rate to produce is the page's to say (it is the rate the ticket asks the gateway for),
    // so that the two cannot be set apart; the rate to read is the context's, here.
    const outputRate = options?.processorOptions?.outputRate;
    if (typeof outputRate !== 'number') throw new Error('the capture worklet needs an outputRate');
    this.resampler = new LinearResampler(sampleRate, outputRate);
    this.batcher = new Pcm16Batcher(liveBatchSamples(outputRate));
    this.port.onmessage = (event: MessageEvent) => {
      if (event.data !== LIVE_FLUSH_MESSAGE) return;
      this.batcher.flush(this.post);
      this.port.postMessage(LIVE_FLUSHED_MESSAGE);
    };
  }

  // Transfers the batch to the page: nothing is copied, and nothing here keeps a reference.
  private readonly post = (pcm: ArrayBuffer): void => {
    this.port.postMessage(pcm, [pcm]);
  };

  process(inputs: Float32Array[][]): boolean {
    // No channel: the microphone is not connected (or has been, and is gone). Stay alive.
    const channel = inputs[0]?.[0];
    if (channel) this.batcher.push(this.resampler.process(channel), this.post);
    return true;
  }
}

registerProcessor(LIVE_CAPTURE_PROCESSOR_NAME, LiveCaptureProcessor);
