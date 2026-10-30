// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Progressive playback of a streaming speech run (ADR-068, the TTS composer).
//
// A speech run whose model streams relays its audio to the portal as `audio` run events:
// the headerless 16-bit little-endian MONO PCM of one chunk, base64-encoded, numbered, with
// the sample rate to play it at (the stream itself carries no format). This module plays
// those chunks as they arrive, with the Web Audio API, and is the only thing in the portal
// that does; the finished clip is committed server-side as an ordinary WAV and rendered by
// AudioTurn exactly like a buffered reply, so playing live is an enhancement that can be
// abandoned at any point without losing anything.
//
// It is built so that the parts that cannot be heard in a test are small:
//
//   - acceptChunk is the reducer that decides, per chunk, whether it may be played. A chunk
//     is played only while the stream is whole -- starting at chunk 0, in order, at one
//     sample rate -- because the portal's run events are lossy by design (a subscriber whose
//     buffer is full is dropped events), which for audio is a gap you can hear. The first
//     irregularity abandons live playback for the run; the committed WAV is complete.
//   - decodeBase64 / pcm16ToFloat32 are the pure conversion, including the odd byte a chunk
//     boundary can leave inside a sample.
//   - StreamingPcmPlayer resamples each decoded chunk to the AudioContext's own rate and streams
//     it into an AudioWorklet ring buffer (playbackWorklet.ts / playbackPcm.ts), through one gain
//     node that is the run's mute.
//   - createSpeechPlayback holds one session per chat on top of those, which useChatRuns drives
//     from the run's events.
//
// An AudioWorklet ring buffer is used rather than scheduling one AudioBufferSourceNode per chunk
// from the main thread (#347): on a phone, with the microphone capture worklet also running, the
// main thread jitters and the next chunk's scheduled start slips, opening an audible gap at the
// chunk seam. A worklet drains the ring continuously on the real-time audio thread, so the main
// thread only has to FILL the buffer -- one continuous stream, gapless under main-thread load.

import { LinearResampler } from './livePcm';
import {
  PLAYBACK_DRAINED_MESSAGE,
  PLAYBACK_PAUSE_MESSAGE,
  PLAYBACK_RESUME_MESSAGE,
  PLAYBACK_STOP_MESSAGE,
  STREAMING_PCM_PLAYBACK_PROCESSOR_NAME,
  type PlaybackDrainedMessage,
} from './playbackPcm';
import playbackWorkletUrl from './playbackWorklet.ts?worker&url';

/** One chunk of a streaming speech run's audio, as the `audio` run event carries it. */
export type AudioChunk = {
  // The chunk's number within its run, from 0. A hole in the numbers is a chunk the
  // connection dropped.
  seq: number;
  // The sample rate to play it at, in Hz.
  rate: number;
  // The chunk's PCM (16-bit little-endian mono), standard base64.
  data: string;
};

// The sample rates the portal will play a stream at, in Hz: the gateway's own bounds
// (speechStreamMin/MaxSampleRate), so a rate it would never send is one something broke.
export const MIN_STREAM_SAMPLE_RATE = 8000;
export const MAX_STREAM_SAMPLE_RATE = 192000;

/** Where live playback of one run stands. */
export type LiveState =
  // Nothing has arrived yet.
  | { phase: 'waiting' }
  // Playing: the rate it plays at and the number the next chunk must have.
  | { phase: 'live'; rate: number; nextSeq: number }
  // Given up for this run, for good: the stream has a hole in it (or was joined late), its
  // chunks are not audio this player can play, or the browser's audio is not running (a run
  // adopted by a tab that was never primed). What is already queued is left to finish --
  // except on a context that is not running, where it is silenced (see `push`); the run's
  // committed WAV is the complete audio.
  | { phase: 'abandoned'; reason: 'gap' | 'format' | 'suspended' };

/**
 * Decide whether `chunk` may be played, given where playback stands. The reducer of
 * progressive playback: pure, so the rule is testable without any audio. It plays a chunk
 * only if it is the one it expects at the rate it has been playing at, and abandons the run
 * at the first that is not (and then every one after it).
 */
export function acceptChunk(
  state: LiveState,
  chunk: AudioChunk,
): { state: LiveState; play: boolean } {
  if (state.phase === 'abandoned') return { state, play: false };
  const abandon = (reason: 'gap' | 'format'): { state: LiveState; play: boolean } => ({
    state: { phase: 'abandoned', reason },
    play: false,
  });
  const rateOk =
    Number.isInteger(chunk.rate) &&
    chunk.rate >= MIN_STREAM_SAMPLE_RATE &&
    chunk.rate <= MAX_STREAM_SAMPLE_RATE;
  if (!rateOk || !Number.isInteger(chunk.seq) || typeof chunk.data !== 'string') {
    return abandon('format');
  }
  if (state.phase === 'waiting') {
    // A run is joined at its start or not at all: a first chunk that is not chunk 0 means the
    // beginning was missed (a reconnect mid-run), and playing from the middle of a sentence
    // would only be noise.
    return chunk.seq === 0
      ? { state: { phase: 'live', rate: chunk.rate, nextSeq: 1 }, play: true }
      : abandon('gap');
  }
  if (chunk.rate !== state.rate) return abandon('format');
  if (chunk.seq !== state.nextSeq) return abandon('gap');
  return { state: { phase: 'live', rate: state.rate, nextSeq: state.nextSeq + 1 }, play: true };
}

/** Standard base64 to bytes, or null for text that is not base64 (never a throw). */
export function decodeBase64(data: string): Uint8Array | null {
  try {
    const binary = atob(data);
    const bytes = new Uint8Array(binary.length);
    // codePointAt, not charCodeAt: they differ only on a surrogate PAIR, and atob's output is
    // a binary string by definition (every code unit 0..255), so no surrogate can occur here
    // and the two are exactly equivalent. The `?? 0` is for the type only -- `i` is in range.
    for (let i = 0; i < binary.length; i++) bytes[i] = binary.codePointAt(i) ?? 0;
    return bytes;
  } catch {
    return null;
  }
}

/**
 * 16-bit little-endian PCM to floats in [-1, 1). A chunk may end in the middle of a sample
 * (nothing in the stream's contract aligns a chunk to a whole sample), so the byte left
 * over is handed back as `carry` and must be passed in with the next chunk, which puts it
 * back in front of that chunk's first byte.
 */
export function pcm16ToFloat32(
  bytes: Uint8Array,
  carry: number | null,
): { samples: Float32Array<ArrayBuffer>; carry: number | null } {
  let data = bytes;
  if (carry !== null) {
    data = new Uint8Array(bytes.length + 1);
    data[0] = carry;
    data.set(bytes, 1);
  }
  const whole = data.length - (data.length % 2);
  const view = new DataView(data.buffer, data.byteOffset, data.byteLength);
  const samples = new Float32Array(whole / 2);
  for (let i = 0; i < samples.length; i++) samples[i] = view.getInt16(i * 2, true) / 32768;
  return { samples, carry: whole < data.length ? data[whole] : null };
}

// The slice of the Web Audio API this module needs to know the context by. The player needs only
// the output rate (to resample to) and reads the clock state through isRunning below; the real
// browser AudioContext also exposes audioWorklet/createGain/destination, which the worklet sink
// reaches after narrowing -- they are kept off this structural type so a test can stand in a plain
// object and inject its own sink.
export type PlaybackContext = {
  readonly sampleRate: number;
  readonly state?: string;
  resume?(): Promise<void>;
};

// The margin, in ms, added to a draining tail's own remaining duration before the fallback
// timer declares it drained (see createSpeechPlayback's `end`). The primary drain signal is the
// worklet's drained message; this fallback only fires when that never arrives (a context frozen
// mid-tail, a stand-in that does not report), and the margin keeps it from racing the real end of
// audio by a few ms of scheduling slack.
const DRAIN_FALLBACK_MARGIN_MS = 250;

/**
 * Where the player hands its resampled audio to be heard: an AudioWorklet ring buffer on the
 * real-time audio thread (createWorkletSink), or a stand-in in a test. `write` enqueues samples
 * already at the context's rate; `pause`/`resume` hold and continue the playout (the ring keeps
 * the unplayed tail); `setMuted` silences the output without stopping its clock; `onDrained` is
 * called with the cumulative count of samples played out each time the ring drains to empty.
 */
export type PlaybackSink = {
  write(samples: Float32Array): void;
  setMuted(muted: boolean): void;
  pause(): void;
  resume(): void;
  stop(): void;
  onDrained: ((played: number) => void) | null;
};

// The playback worklet module, added to each AudioContext once: addModule is idempotent, but
// awaiting one shared promise per context avoids a second fetch and a race to make the node.
const playbackModuleAdded = new WeakMap<AudioContext, Promise<void>>();

function ensurePlaybackModule(ctx: AudioContext): Promise<void> {
  let added = playbackModuleAdded.get(ctx);
  if (!added) {
    added = ctx.audioWorklet.addModule(playbackWorkletUrl);
    playbackModuleAdded.set(ctx, added);
    // A failed load must not be cached for the page's whole life (the context is the long-lived
    // shared one): drop the entry so a later player -- the next answer -- can try again. The
    // caller of this attempt still sees the rejection and degrades for now.
    added.catch(() => {
      if (playbackModuleAdded.get(ctx) === added) playbackModuleAdded.delete(ctx);
    });
  }
  return added;
}

/**
 * The real sink: an AudioWorkletNode running playbackWorklet.ts (the ring buffer), through one
 * gain node that is the stream's mute, to the destination. Setup is async (the module must be
 * added first), so samples written before the node exists are held on the main thread and flushed
 * once it does. If the browser has no AudioWorklet, or the module will not load, live playout is
 * simply skipped -- it is an enhancement, and the committed WAV is the complete audio.
 */
function createWorkletSink(ctx: PlaybackContext): PlaybackSink {
  // At runtime the context is always a real AudioContext (sharedAudioContext / the voice
  // session's own); a test injects its own sink instead of ever reaching here.
  const audioCtx = ctx as unknown as AudioContext;
  let node: AudioWorkletNode | null = null;
  let gain: GainNode | null = null;
  // Samples written before the node exists, flushed in order once it does.
  const queued: Float32Array[] = [];
  let paused = false;
  let muted = false;
  let stopped = false;
  // Set when the worklet could not be loaded: playout is then silent (the committed WAV is the
  // audio), but drains are still reported so turn-taking is not stuck -- see reportSilentDrain.
  let failed = false;
  // The cumulative samples written, so a silent drain can report the same count a real one would.
  let writtenCount = 0;
  let drainScheduled = false;

  const sink: PlaybackSink = {
    onDrained: null,
    write(samples) {
      if (stopped) return;
      writtenCount += samples.length;
      if (node) node.port.postMessage(samples, [samples.buffer as ArrayBuffer]);
      else if (failed) reportSilentDrain();
      else queued.push(samples);
    },
    setMuted(next) {
      muted = next;
      if (gain) gain.gain.value = next ? 0 : 1;
    },
    pause() {
      paused = true;
      node?.port.postMessage(PLAYBACK_PAUSE_MESSAGE);
    },
    resume() {
      paused = false;
      node?.port.postMessage(PLAYBACK_RESUME_MESSAGE);
    },
    stop() {
      stopped = true;
      sink.onDrained = null;
      queued.length = 0;
      if (node) {
        try {
          node.port.postMessage(PLAYBACK_STOP_MESSAGE);
        } catch {
          // The port may already be gone.
        }
        node.port.onmessage = null;
        try {
          node.disconnect();
        } catch {
          // Not connected.
        }
        node = null;
      }
      if (gain) {
        try {
          gain.disconnect();
        } catch {
          // Not connected.
        }
        gain = null;
      }
    },
  };

  // If the worklet never loads, playout is silent but must still report a drain, so the session can
  // tell the gateway the answer finished and reopen the microphone (the saved WAV is the audio).
  // Deferred to a microtask so it never re-enters the caller's push, and debounced so a burst of
  // writes reports one drain, as a real ring draining to empty would.
  function reportSilentDrain(): void {
    if (drainScheduled) return;
    drainScheduled = true;
    queueMicrotask(() => {
      drainScheduled = false;
      if (!stopped) sink.onDrained?.(writtenCount);
    });
  }

  void setup();

  async function setup(): Promise<void> {
    try {
      await ensurePlaybackModule(audioCtx);
      if (stopped) return;
      const workletNode = new AudioWorkletNode(audioCtx, STREAMING_PCM_PLAYBACK_PROCESSOR_NAME, {
        numberOfInputs: 0,
        numberOfOutputs: 1,
        outputChannelCount: [1],
      });
      workletNode.port.onmessage = (event: MessageEvent) => {
        if (stopped) return;
        const data = event.data as PlaybackDrainedMessage | undefined;
        if (data?.type === PLAYBACK_DRAINED_MESSAGE) sink.onDrained?.(data.played);
      };
      const gainNode = audioCtx.createGain();
      gainNode.gain.value = muted ? 0 : 1;
      workletNode.connect(gainNode).connect(audioCtx.destination);
      node = workletNode;
      gain = gainNode;
      // Apply a pause asked for before the node existed, then flush the audio that arrived first.
      if (paused) workletNode.port.postMessage(PLAYBACK_PAUSE_MESSAGE);
      for (const samples of queued.splice(0)) {
        workletNode.port.postMessage(samples, [samples.buffer as ArrayBuffer]);
      }
    } catch {
      // No AudioWorklet, or the module failed to load: stay silent, but report what was written as
      // drained so turn-taking is not stuck. The saved WAV is the complete audio, so nothing is lost.
      failed = true;
      queued.length = 0;
      reportSilentDrain();
    }
  }

  return sink;
}

/**
 * Plays the decoded chunks of ONE stream by resampling each to the AudioContext's own rate
 * (continuously, carrying the resampler state across chunks so there is no click at the seams)
 * and streaming the samples into an AudioWorklet ring buffer. The worklet drains the ring on the
 * audio thread, so however unevenly the chunks arrive, and however busy the main thread is, the
 * output is one continuous stream with no gaps -- for as long as they arrive faster than they
 * play, which the gateway ensures by relaying ahead of real time.
 *
 * It can be paused and resumed (see `pause`). A pause holds THIS player's playout -- the worklet
 * outputs silence and the ring keeps the unplayed tail -- without suspending the AudioContext,
 * which is the page's one, shared by every chat and player.
 */
export class StreamingPcmPlayer {
  private carry: number | null = null;
  // The resampler from the stream's rate to the context's, made on the first chunk (the rate is
  // not known before then) and kept for the whole stream, so cutting the audio into chunks changes
  // nothing at the seams.
  private resampler: LinearResampler | null = null;
  private stopped = false;
  private paused = false;
  private readonly sink: PlaybackSink;
  // The context's output rate: what the audio is resampled to, and the unit of remainingSeconds.
  private readonly outputRate: number;
  // Samples (at the output rate) handed to the sink, and the cumulative count it has reported
  // played out. Their difference is what is still to play (`pending`): 0 exactly when drained.
  private written = 0;
  private played = 0;
  // Called whenever the sink reports its ring drained -- EVERY drain, including a gap between
  // chunks when TTS lags real time. The session (createSpeechPlayback) wires this to its drain
  // signal ONLY after the run has ended, so a queue that empties mid-stream never fires it there.
  // Never fired while paused (a pause is not a drain) or after stop.
  onEmpty: (() => void) | null = null;

  constructor(
    ctx: PlaybackContext,
    createSink: (ctx: PlaybackContext) => PlaybackSink = createWorkletSink,
  ) {
    this.outputRate = ctx.sampleRate;
    this.sink = createSink(ctx);
    this.sink.onDrained = (played) => {
      this.played = played;
      if (this.stopped || this.paused) return;
      this.onEmpty?.();
    };
  }

  /** Queue one chunk's PCM. False when it could not be (the player is stopped, or the rate is one
   * the resampler will not accept): the caller then stops pushing. */
  push(bytes: Uint8Array, rate: number): boolean {
    if (this.stopped) return false;
    if (!this.resampler) {
      try {
        this.resampler = new LinearResampler(rate, this.outputRate);
      } catch {
        // A rate the resampler will not accept (0, NaN, ...): nothing was queued.
        return false;
      }
    }
    const { samples, carry } = pcm16ToFloat32(bytes, this.carry);
    this.carry = carry;
    if (samples.length === 0) return true;
    const out = this.resampler.process(samples);
    // The resampler is one input sample ahead of what it emits, so a tiny chunk can complete no
    // output sample yet: nothing to write, but the chunk was accepted.
    if (out.length === 0) return true;
    this.written += out.length;
    this.sink.write(out);
    return true;
  }

  /**
   * Mute or unmute the stream: the gain of the whole output. Reversible, and local to this
   * player -- nothing is stopped, dropped or re-queued, so the stream's clock keeps running and
   * unmuting rejoins it where it is.
   */
  setMuted(muted: boolean): void {
    this.sink.setMuted(muted);
  }

  /**
   * Halt the playout where it is: the worklet holds the ring and outputs silence, so the unplayed
   * tail (and everything pushed meanwhile) waits for `resume`. Only this player's output is held;
   * the shared AudioContext keeps running for every other chat, and `onEmpty` does not fire (a
   * pause is not a drain). A pause on a player that is already paused, or stopped, does nothing.
   */
  pause(): void {
    if (this.stopped || this.paused) return;
    this.paused = true;
    this.sink.pause();
  }

  /** Continue after `pause`: the worklet drains the ring again from where it stopped. A player that
   * is not paused, or stopped, does nothing. */
  resume(): void {
    if (this.stopped || !this.paused) return;
    this.paused = false;
    this.sink.resume();
  }

  /** Silence everything queued and ignore every later chunk. */
  stop(): void {
    this.stopped = true;
    this.onEmpty = null;
    this.sink.stop();
  }

  /** How much audio has been queued but not yet played out, in samples at the output rate: it is
   * 0 exactly when the sink has drained. */
  get pending(): number {
    return this.written - this.played;
  }

  /** The seconds of audio still to play, used to bound the drain fallback timer. */
  remainingSeconds(): number {
    return this.pending / this.outputRate;
  }
}

/** Live playback of the speech runs of the portal's chats: what useChatRuns drives. */
export type SpeechPlayback = {
  /** Get the browser's audio ready, from inside a user gesture (the click that sends a
   * speech turn), so that the first chunk can be heard where autoplay is restricted. */
  prime(): void;
  /** Take one chunk of the chat's run. True when the run is playing live after it. */
  push(chatId: string, runId: string, chunk: AudioChunk): boolean;
  /** The run ended. A completed one on a running clock KEEPS its session alive and lets what is
   * queued play OUT (the draining tail) -- so Mute and `abort` can still reach it, and `onDrained`
   * fires when the last source ends. Anything else -- a Stop, an error, or a context that is not
   * producing sound -- goes silent at once and drains immediately. */
  end(chatId: string, completed: boolean): void;
  /** Stop a chat's audio now, draining tail and all, and drop its session: the tail-stop of the
   * live controls once the run itself is already done (it touches neither the run nor what is
   * stored of it). A chat with no session is a no-op. */
  abort(chatId: string): void;
  /** Silence every chat and forget them all. */
  stopAll(): void;
  /** Mute or unmute the chat's run: local playback only (the run itself, and what is saved of
   * it, is untouched), reversible, and reachable through the draining tail of a completed run. A
   * chat with no session is a no-op. */
  setMuted(chatId: string, muted: boolean): void;
  /** Whether the chat's run is playing live right now (its chunks arriving and scheduled). False
   * the moment the run ends, even while its tail is still draining: a completed run's draining tail
   * is tracked by its render mirror (runAudioLive, kept set until onDrained fires), not here. */
  isLive(chatId: string): boolean;
  /** Register the one callback fired with a chatId when that chat's audio has fully drained (a
   * completed run's tail finished, or a run was silenced): the signal that `runAudioLive` may be
   * cleared. Last registration wins. */
  onDrained(cb: (chatId: string) => void): void;
};

type Session = {
  runId: string;
  state: LiveState;
  player: StreamingPcmPlayer;
  ctx: PlaybackContext;
  // Whether the run has ended and the player is draining its tail (or has been silenced). Set by
  // `end`; a fresh run's session starts false.
  ended: boolean;
  // The fallback timer that declares the tail drained if the worklet's drained message never fires
  // (a context frozen mid-tail). Cleared when the real drain lands, or the session is discarded.
  drainTimer: ReturnType<typeof setTimeout> | null;
};

// Whether the context is producing sound: it says it is running, or (a stand-in that keeps
// no state) says nothing. A suspended or interrupted context has a frozen clock, on which
// anything queued waits to be played -- stale -- whenever something later resumes it.
function isRunning(ctx: PlaybackContext): boolean {
  return ctx.state === undefined || ctx.state === 'running';
}

/**
 * One playback session per chat on top of the pieces above. `getContext` is asked lazily, so
 * an environment with no Web Audio (a test, an old browser) is a controller that never plays
 * and never throws: live playback is an enhancement and its absence is the buffered
 * behavior, with the finished clip rendered by AudioTurn as ever. `makePlayer` is the seam a
 * test uses to inject a player with a recording sink (the default makes the real worklet player).
 */
export function createSpeechPlayback(
  getContext: () => PlaybackContext | null,
  makePlayer: (ctx: PlaybackContext) => StreamingPcmPlayer = (ctx) => new StreamingPcmPlayer(ctx),
): SpeechPlayback {
  const sessions = new Map<string, Session>();
  let onDrainedCb: ((chatId: string) => void) | null = null;

  const resume = (ctx: PlaybackContext) => {
    if (ctx.state === 'suspended') void ctx.resume?.()?.catch(() => undefined);
  };

  // Silence a session's player and tear down its drain plumbing, WITHOUT signalling the drain or
  // removing it from the map. The one place that actually stops a player, so a pending fallback
  // timer and the onEmpty hook never outlive it.
  const silence = (session: Session) => {
    if (session.drainTimer !== null) {
      clearTimeout(session.drainTimer);
      session.drainTimer = null;
    }
    session.player.stop(); // also nulls onEmpty
  };

  // A chat's audio has fully drained: silence what is left, drop the session, and signal the
  // drain so its render mirror (runAudioLive) can clear. Guarded against a session a newer run
  // has already replaced -- a stale fallback timer must never delete the current session.
  const drain = (chatId: string, session: Session) => {
    if (sessions.get(chatId) !== session) return;
    silence(session);
    sessions.delete(chatId);
    onDrainedCb?.(chatId);
  };

  return {
    prime() {
      const ctx = getContext();
      if (ctx) resume(ctx);
    },

    push(chatId, runId, chunk) {
      let session = sessions.get(chatId);
      if (session?.runId !== runId) {
        // A new run of this chat replaces the previous one (still live, or draining a completed
        // run's tail): drain the old session -- stop its audio so it cannot overlap this answer
        // or outlive the chat, drop it, AND signal its drain, so the old run's live/mute marks
        // clear. Without that signal a mute left on the draining tail would carry over and
        // mislabel this new run (it starts unmuted). THIS run's own push, below, then sets the
        // chat live again.
        if (session) drain(chatId, session);
        const ctx = getContext();
        if (!ctx) return false;
        resume(ctx);
        session = {
          runId,
          state: { phase: 'waiting' },
          player: makePlayer(ctx),
          ctx,
          ended: false,
          drainTimer: null,
        };
        sessions.set(chatId, session);
      }
      // Live playback needs a context that is running NOW. A tab that re-adopts a speech run
      // before its first chunk (a reload, a second tab, during a model's cold start) was never
      // primed by a gesture, so its context is suspended -- and `resume` above only asks: it
      // answers later, if at all. Audio queued on the frozen clock would play, stale and
      // possibly over a newer run, whenever something finally resumed it, so such a run is not
      // played live (and neither is one whose context is suspended mid-run, whose queue is
      // silenced for the same reason); its committed WAV is the reply. The resume request is
      // kept: it is what unlocks the page for the NEXT run.
      if (session.state.phase !== 'abandoned' && !isRunning(session.ctx)) {
        session.player.stop();
        session.state = { phase: 'abandoned', reason: 'suspended' };
        return false;
      }
      const verdict = acceptChunk(session.state, chunk);
      session.state = verdict.state;
      if (!verdict.play) return false;
      const bytes = decodeBase64(chunk.data);
      if (bytes === null || !session.player.push(bytes, chunk.rate)) {
        session.state = { phase: 'abandoned', reason: 'format' };
        return false;
      }
      return true;
    },

    end(chatId, completed) {
      const session = sessions.get(chatId);
      if (!session) return;
      // A second end for a session already draining a completed run's tail: a clean one is
      // redundant (the drain is already wired), but a Stop, an error, or a chat-delete (forget's
      // `end(false)`) must still cut the tail that is playing out NOW -- otherwise it stays
      // audible with no controls, and outlives a deleted chat.
      if (session.ended) {
        if (!completed) drain(chatId, session);
        return;
      }
      session.ended = true;
      // Anything but a clean end on a running clock goes silent at once: a Stop, an error, or a
      // context whose frozen queue would otherwise play stale when something resumed it. The
      // chat's audio has drained immediately.
      if (!completed || !isRunning(session.ctx)) {
        drain(chatId, session);
        return;
      }
      // A clean end with the clock running: let the queued tail play OUT. The session is KEPT so
      // Mute and `abort` can still reach it while its saved player appears beside it; it is
      // dropped, and the drain signalled, when the tail finishes.
      if (session.player.pending === 0) {
        drain(chatId, session);
        return;
      }
      // Primary signal: the worklet's drained message (onEmpty). Fallback: a timer bounded by the
      // tail's own remaining length, in case that never arrives (a context suspended mid-tail), so
      // the live controls can never hang on screen forever.
      session.player.onEmpty = () => drain(chatId, session);
      session.drainTimer = setTimeout(
        () => drain(chatId, session),
        session.player.remainingSeconds() * 1000 + DRAIN_FALLBACK_MARGIN_MS,
      );
    },

    abort(chatId) {
      const session = sessions.get(chatId);
      if (!session) return;
      drain(chatId, session);
    },

    stopAll() {
      for (const [chatId, session] of sessions) {
        silence(session);
        onDrainedCb?.(chatId);
      }
      sessions.clear();
    },

    setMuted(chatId, muted) {
      sessions.get(chatId)?.player.setMuted(muted);
    },

    isLive(chatId) {
      const session = sessions.get(chatId);
      return session !== undefined && !session.ended && session.state.phase === 'live';
    },

    onDrained(cb) {
      onDrainedCb = cb;
    },
  };
}

// The page's one AudioContext. Browsers cap how many may exist, and a context created inside
// the user's gesture (prime) is the one that is allowed to make sound, so it is made once and
// reused for every run.
let sharedContext: PlaybackContext | null | undefined;

function sharedAudioContext(): PlaybackContext | null {
  if (sharedContext !== undefined) return sharedContext;
  try {
    const Ctor: typeof AudioContext | undefined =
      typeof window === 'undefined'
        ? undefined
        : (window.AudioContext ??
          (window as unknown as { webkitAudioContext?: typeof AudioContext }).webkitAudioContext);
    sharedContext = Ctor ? (new Ctor() as unknown as PlaybackContext) : null;
  } catch {
    sharedContext = null;
  }
  return sharedContext;
}

/** The portal's speech playback, on the page's own AudioContext. */
export const speechPlayback: SpeechPlayback = createSpeechPlayback(sharedAudioContext);
