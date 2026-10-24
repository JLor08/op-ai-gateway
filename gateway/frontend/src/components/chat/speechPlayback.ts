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
//   - StreamingPcmPlayer schedules decoded chunks back to back on an AudioContext's clock,
//     through one gain node that is the run's mute.
//   - createSpeechPlayback holds one session per chat on top of those, which useChatRuns
//     drives from the run's events.
//
// Scheduled AudioBufferSourceNodes are used rather than an AudioWorklet: the chunks arrive
// from the network in whole pieces, and a node per chunk placed on the audio clock is
// gapless without a second thread, a separately served worklet module, or a message channel
// whose underruns would need their own protocol.

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

// The slice of the Web Audio API the player uses. The browser's AudioContext satisfies it;
// a test passes a recording stand-in, since sound cannot be heard in jsdom.
export type PlaybackBuffer = {
  readonly duration: number;
  copyToChannel(source: Float32Array<ArrayBuffer>, channelNumber: number): void;
};
export type PlaybackSource = {
  buffer: PlaybackBuffer | null;
  onended: ((event: Event) => void) | null;
  connect(destination: unknown): unknown;
  start(when?: number): void;
  stop(when?: number): void;
};
// The one gain stage of a stream: every source reaches the destination through it, and its
// value is the mute (0 silent, 1 as decoded).
export type PlaybackGain = {
  readonly gain: { value: number };
  connect(destination: unknown): unknown;
};
export type PlaybackContext = {
  readonly currentTime: number;
  readonly destination: unknown;
  readonly state?: string;
  createBuffer(numberOfChannels: number, length: number, sampleRate: number): PlaybackBuffer;
  createBufferSource(): PlaybackSource;
  createGain(): PlaybackGain;
  resume?(): Promise<void>;
};

// How far ahead of "now" the first chunk of a stream (and the first after a stall) is
// scheduled, in seconds: a cushion against the jitter between the chunks that follow, paid
// once, as latency, at the start.
const DEFAULT_LEAD_SECONDS = 0.12;

// The margin, in ms, added to a draining tail's own remaining duration before the fallback
// timer declares it drained (see createSpeechPlayback's `end`). The primary drain signal is the
// last source's `onended`; this fallback only fires when that never arrives (a context frozen
// mid-tail, a stand-in that does not call onended), and the margin keeps it from racing the real
// end of audio by a few ms of scheduling slack.
const DRAIN_FALLBACK_MARGIN_MS = 250;

/**
 * Plays the decoded chunks of ONE stream back to back on an AudioContext's own clock: each
 * chunk becomes a buffer source started exactly where the previous one ends, so there is no
 * gap however unevenly the chunks arrive, for as long as they arrive faster than they play.
 * When the queue runs dry (the stream stalled, or it is simply over) the next chunk is
 * re-anchored a lead ahead of now instead of being scheduled in the past, where it would be
 * dropped or played in a burst.
 */
export class StreamingPcmPlayer {
  private nextTime = 0;
  private carry: number | null = null;
  private stopped = false;
  // The stream's mute, kept here so it holds before the gain node exists (the node is made
  // with the first chunk) and for every chunk after.
  private muted = false;
  // The persistent gain stage between the sources and the destination: made once, with the
  // first chunk, and shared by every chunk after it.
  private gain: PlaybackGain | null = null;
  private readonly sources = new Set<PlaybackSource>();
  // Called whenever the last scheduled source ends and the queue becomes empty. The session
  // (createSpeechPlayback) wires this to its drain signal ONLY after the run has ended, so a
  // queue that empties mid-stream (between chunks of a run still going) never fires it.
  onEmpty: (() => void) | null = null;

  constructor(
    private readonly ctx: PlaybackContext,
    private readonly lead: number = DEFAULT_LEAD_SECONDS,
  ) {}

  /** Queue one chunk's PCM. False when it could not be (the player is stopped, or the browser
   * refused the buffer): the caller then stops pushing. */
  push(bytes: Uint8Array, rate: number): boolean {
    if (this.stopped) return false;
    const { samples, carry } = pcm16ToFloat32(bytes, this.carry);
    this.carry = carry;
    if (samples.length === 0) return true;
    let source: PlaybackSource;
    let duration: number;
    try {
      const buffer = this.ctx.createBuffer(1, samples.length, rate);
      buffer.copyToChannel(samples, 0);
      source = this.ctx.createBufferSource();
      source.buffer = buffer;
      source.connect(this.gainStage());
      duration = buffer.duration;
    } catch {
      // A rate the browser cannot make a buffer of: nothing was scheduled.
      return false;
    }
    const now = this.ctx.currentTime;
    const startAt = this.nextTime > now ? this.nextTime : now + this.lead;
    source.onended = () => {
      this.sources.delete(source);
      // The last scheduled source has ended: tell whoever is draining this player (nothing,
      // while the run is still live).
      if (this.sources.size === 0) this.onEmpty?.();
    };
    this.sources.add(source);
    source.start(startAt);
    this.nextTime = startAt + duration;
    return true;
  }

  /**
   * Mute or unmute the stream: the gain of what is queued AND of every chunk still to come.
   * Reversible, and local to this player's output -- nothing is stopped, dropped or
   * rescheduled, so the stream's clock keeps running and unmuting rejoins it where it is.
   */
  setMuted(muted: boolean): void {
    this.muted = muted;
    if (this.gain) this.gain.gain.value = muted ? 0 : 1;
  }

  // The gain stage, made on first use at the current mute and wired to the destination once.
  private gainStage(): PlaybackGain {
    if (!this.gain) {
      const gain = this.ctx.createGain();
      gain.gain.value = this.muted ? 0 : 1;
      gain.connect(this.ctx.destination);
      this.gain = gain;
    }
    return this.gain;
  }

  /** Silence everything scheduled and ignore every later chunk. */
  stop(): void {
    this.stopped = true;
    this.onEmpty = null;
    for (const source of this.sources) {
      try {
        source.stop();
      } catch {
        // A source that has not started, or already ended, has nothing to stop.
      }
    }
    this.sources.clear();
  }

  /** How many scheduled sources have not yet ended: the length of the audio still to play out. */
  get pending(): number {
    return this.sources.size;
  }

  /** The seconds of audio still scheduled past "now" -- the queued tail's own remaining length,
   * used to bound the drain fallback timer. Zero once the queue has caught up to the clock. */
  remainingSeconds(): number {
    return Math.max(0, this.nextTime - this.ctx.currentTime);
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
   * the moment the run ends, even while its tail is still draining -- use `isPlayingOut` for that. */
  isLive(chatId: string): boolean;
  /** Whether the chat is still producing sound: playing live, OR draining a completed run's tail
   * that has not finished yet. True from the first chunk until the tail has fully played out. */
  isPlayingOut(chatId: string): boolean;
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
  // The fallback timer that declares the tail drained if the last source's `onended` never fires
  // (a context frozen mid-tail). Cleared when the real drain lands, or the session is discarded.
  drainTimer: ReturnType<typeof setTimeout> | null;
};

// Whether the context is producing sound: it says it is running, or (a stand-in that keeps
// no state) says nothing. A suspended or interrupted context has a frozen clock, on which
// anything scheduled waits to be played -- stale -- whenever something later resumes it.
function isRunning(ctx: PlaybackContext): boolean {
  return ctx.state === undefined || ctx.state === 'running';
}

/**
 * One playback session per chat on top of the pieces above. `getContext` is asked lazily, so
 * an environment with no Web Audio (a test, an old browser) is a controller that never plays
 * and never throws: live playback is an enhancement and its absence is the buffered
 * behavior, with the finished clip rendered by AudioTurn as ever.
 */
export function createSpeechPlayback(getContext: () => PlaybackContext | null): SpeechPlayback {
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
        // A new run of this chat: stop the previous one's tail (whether it was still live or
        // already draining a completed run) so it cannot overlap this answer or outlive the
        // chat. No drain signal here -- THIS run's own push, below, sets the chat's live state.
        if (session) silence(session);
        const ctx = getContext();
        if (!ctx) return false;
        resume(ctx);
        session = {
          runId,
          state: { phase: 'waiting' },
          player: new StreamingPcmPlayer(ctx),
          ctx,
          ended: false,
          drainTimer: null,
        };
        sessions.set(chatId, session);
      }
      // Live playback needs a context that is running NOW. A tab that re-adopts a speech run
      // before its first chunk (a reload, a second tab, during a model's cold start) was never
      // primed by a gesture, so its context is suspended -- and `resume` above only asks: it
      // answers later, if at all. Audio scheduled on the frozen clock would play, stale and
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
      if (!session || session.ended) return;
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
      // Primary signal: the last scheduled source's `onended`. Fallback: a timer bounded by the
      // tail's own remaining length, in case `onended` never arrives (a context suspended
      // mid-tail), so the live controls can never hang on screen forever.
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

    isPlayingOut(chatId) {
      const session = sessions.get(chatId);
      if (!session) return false;
      // While the run is live, "playing out" is just "live". Once it has ended, the tail is
      // playing out for as long as the player still holds scheduled sources.
      if (!session.ended) return session.state.phase === 'live';
      return session.player.pending > 0;
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
