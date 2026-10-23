// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// The portal's side of a live transcription session (#266): the microphone captured as 16 kHz
// mono s16le PCM (captureWorklet.ts), streamed up a WebSocket to the gateway, and the
// transcript coming back down it as the speaker talks.
//
// The wire contract, which the gateway's /api/portal/transcription-live implements:
//
//   - A ticket is minted first, by a POST the portal's CSRF protection covers (a WebSocket
//     cannot carry the X-OP-CSRF header, so the upgrade cannot be protected that way). The
//     ticket rides the upgrade as the second subprotocol: new WebSocket(url, [name, ticket]).
//     It is single-use and good for 30 seconds, so it is minted only once the microphone is
//     granted (the permission prompt can take as long as the user likes) and used at once.
//   - UP: binary frames of raw s16le PCM, ~100 ms each (a frame of 1 MiB or more closes the
//     connection with 1009), and one text frame {"type":"end"} when the speaker is done. A
//     close WITHOUT it is a truncated session, which the gateway does not finish or bill.
//   - DOWN: text frames {"type": "delta" | "partial" | "done" | "error", text? | message? | code?}.
//     A delta is appended to the transcript, a partial replaces all of it (a model that revises
//     its hypothesis), done is the final transcript (an empty one means "keep what you have")
//     and error is the end of the session, with a message and, when the gateway refused the
//     session with one of its stable codes (a busy model, an unsupported one), that code.
//
// A session is made with createLiveSession and ended with stop() (finish: the audio so far is
// transcribed and delivered as `done`) or abort() (cancel: nothing more is sent or wanted).
// This file is the glue between those and the browser's audio and WebSocket, which cannot be
// run in a test; what can be tested is pulled out of it: parseLiveDownFrame and
// liveTranscriptionWsUrl here, the audio in livePcm.ts.

import { LIVE_TRANSCRIPTION_SAMPLE_RATE, type PortalApi } from '../../api';
import workletUrl from './captureWorklet.ts?worker&url';
import { LIVE_CAPTURE_PROCESSOR_NAME, LIVE_FLUSHED_MESSAGE, LIVE_FLUSH_MESSAGE } from './livePcm';

// The subprotocol the gateway selects; the ticket is the other one offered.
export const LIVE_WS_SUBPROTOCOL = 'op-live-transcription';

// The API path is absolute: the portal's SPA lives under /portal/, its API does not.
const LIVE_WS_PATH = '/api/portal/transcription-live';

// How long stop() waits for the final transcript before giving up on it, in ms. The model
// finishes the audio it still holds after the end of input, which for a long utterance on a
// slow model takes seconds, not milliseconds.
export const LIVE_FINALIZE_GRACE_MS = 10_000;

// How long stop() waits for the worklet to hand over the last batch of audio before sending the
// end of input without it, in ms: the worklet answers within a render quantum (a few ms) unless
// it has died.
const FLUSH_WAIT_MS = 500;

// The audio captured while the connection is still being made (the ticket, the handshake) is
// kept, so that the first words are not lost to it. Each entry is ~100 ms; a connection that
// takes longer than the cap drops the oldest.
const MAX_PENDING_BATCHES = 100;

// How long the session waits for a context that was resumed in the user's gesture to be running
// before giving up on it, in ms: a browser answers within a few ms, so a context that is still
// suspended after this has been refused (or has no gesture to run under).
const CONTEXT_RUN_WAIT_MS = 1_000;

// The WebSocket close code of a server that is busy ("try again later"), and the gateway's own
// code for the same refusal on its other transports.
const WS_TRY_AGAIN_LATER = 1013;
export const LIVE_BUSY_CODE = 'transcription.live_busy';

/** Where a live session stands. It starts `connecting`; `closed` and `error` are final. */
export type LiveSessionState = 'connecting' | 'listening' | 'finalizing' | 'closed' | 'error';

/** A frame of the transcript stream, parsed. */
export type LiveDownFrame =
  | { type: 'delta'; text: string }
  | { type: 'partial'; text: string }
  | { type: 'done'; text: string }
  | { type: 'error'; message: string; code?: string };

/**
 * Reads one text frame the gateway sent down, or null for anything that is not one: text that is
 * not JSON, JSON that is not an object, an unknown `type`, a field of the wrong type. Never
 * throws, and a frame the client does not understand is for the caller to ignore, so that a
 * newer gateway adding a frame type cannot break an older portal.
 *
 * The gateway leaves an empty field out of its frames, so a missing `text` or `message` is the
 * empty string (a `done` with nothing to add reads as `{type: 'done', text: ''}`). An error
 * frame's `code` is left off the parsed frame when it is missing, empty or not a string: it is
 * an optional hint, and an error frame must stay an error frame whatever it says.
 */
export function parseLiveDownFrame(data: string): LiveDownFrame | null {
  let parsed: unknown;
  try {
    parsed = JSON.parse(data);
  } catch {
    return null;
  }
  if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) return null;
  const frame = parsed as { type?: unknown; text?: unknown; message?: unknown; code?: unknown };
  switch (frame.type) {
    case 'delta':
    case 'partial':
    case 'done': {
      const text = frame.text === undefined ? '' : frame.text;
      return typeof text === 'string' ? { type: frame.type, text } : null;
    }
    case 'error': {
      const message = frame.message === undefined ? '' : frame.message;
      if (typeof message !== 'string') return null;
      return typeof frame.code === 'string' && frame.code !== ''
        ? { type: 'error', message, code: frame.code }
        : { type: 'error', message };
    }
    default:
      return null;
  }
}

/** The WebSocket URL of the live endpoint on the page's own origin (the session cookie rides it). */
export function liveTranscriptionWsUrl(location: { protocol: string; host: string }): string {
  return `${location.protocol === 'https:' ? 'wss:' : 'ws:'}//${location.host}${LIVE_WS_PATH}`;
}

/** What createLiveSession needs of the portal's API client: the ticket POST. */
export type LiveTicketApi = Pick<PortalApi, 'createLiveTranscriptionTicket'>;

/**
 * Why a session failed, which is what tells the caller whether trying again can help.
 *
 *  - `dropped`: the connection ended without the gateway saying why, or the ticket request did
 *    not get through (a network failure, a 5xx). The same request may well work next time.
 *  - `refused`: the gateway said no -- an error frame, a ticket it refused (a 4xx, with its
 *    `code`), a busy close. The same request would be refused again.
 *  - `denied`: the user, or the browser's policy, refused the microphone.
 *  - `unsupported`: this browser cannot capture audio this way (no microphone API, no
 *    AudioWorklet).
 *  - `capture`: the microphone or the audio pipeline failed (no device, a worklet that died, a
 *    context the browser would not run).
 */
export type LiveErrorKind = 'dropped' | 'refused' | 'denied' | 'unsupported' | 'capture';

export type LiveErrorDetail = {
  kind: LiveErrorKind;
  /** The gateway's error code, when there is one (a refused ticket, a busy close). */
  code?: string;
};

export type LiveSessionArgs = {
  api: LiveTicketApi;
  model: string;
  /** The language to transcribe in; blank or absent is auto-detect. */
  language?: string;
  /** Text to append to the transcript. */
  onDelta(text: string): void;
  /** The provisional transcript, which replaces all of it so far. */
  onPartial(text: string): void;
  /** The final transcript (empty: keep what the deltas and partials gave). The session is over. */
  onDone(text: string): void;
  /** The session failed, with a message for the log and the kind of failure. The session is over. */
  onError(message: string, detail: LiveErrorDetail): void;
  /** Every state the session is in, `connecting` (made before createLiveSession returns) first. */
  onState(state: LiveSessionState): void;
};

export type LiveSession = {
  /** Finish: stop the microphone, have what was said transcribed, and deliver it as `done`. */
  stop(): void;
  /** Cancel: tear everything down without finishing (the gateway sees a truncated session). */
  abort(): void;
};

// The page's AudioContext constructor: the standard one, or Safari's prefixed one before 14.1.
function audioContextConstructor(): typeof AudioContext | undefined {
  if (typeof window === 'undefined') return undefined;
  return (
    window.AudioContext ??
    (window as unknown as { webkitAudioContext?: typeof AudioContext }).webkitAudioContext
  );
}

function describeError(error: unknown): string {
  if (error instanceof Error) return error.message || error.name;
  return String(error);
}

// A getUserMedia refusal by the user or the browser's policy (NotAllowedError is the standard
// name, PermissionDeniedError the old one, SecurityError a page the browser will not give the
// microphone to). Read by `name`, never `instanceof`: the rejection is a DOMException, which is
// not an Error of this realm in every environment.
function isPermissionDenied(error: unknown): boolean {
  const name =
    typeof error === 'object' && error !== null && 'name' in error ? String(error.name) : '';
  return name === 'NotAllowedError' || name === 'PermissionDeniedError' || name === 'SecurityError';
}

// A failure with its kind, thrown inside start() so that the one catch there can report it.
class LiveFailure extends Error {
  readonly detail: LiveErrorDetail;
  constructor(message: string, detail: LiveErrorDetail) {
    super(message);
    this.detail = detail;
  }
}

// What a failed ticket request means: a 4xx is the gateway refusing this session (the same
// request is refused again); anything else -- a network failure, a 5xx -- did not get an answer.
function ticketFailure(error: unknown): LiveFailure {
  const status = (error as { status?: unknown } | null)?.status;
  const code = (error as { code?: unknown } | null)?.code;
  const refused = typeof status === 'number' && status >= 400 && status < 500;
  return new LiveFailure(describeError(error), {
    kind: refused ? 'refused' : 'dropped',
    ...(refused && typeof code === 'string' && code !== '' ? { code } : {}),
  });
}

// Waits for `promise`, but not longer than `ms`; the timer does not outlive the wait.
async function settleWithin(promise: Promise<unknown>, ms: number): Promise<void> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  const timeout = new Promise<void>((resolve) => {
    timer = setTimeout(resolve, ms);
  });
  try {
    await Promise.race([promise, timeout]);
  } finally {
    clearTimeout(timer);
  }
}

/**
 * Start a live transcription session: the microphone, the connection, and the transcript.
 *
 * Call it from inside the user's gesture (the click that starts the recording): the
 * AudioContext and the microphone request are made before the first await, which is what lets a
 * browser that restricts autoplay (Safari) run the context at all.
 *
 * The order is: the microphone is asked for (the user may take a while to answer), then a ticket
 * is minted and the WebSocket opened, while the audio already being captured is held for it.
 * Apart from the initial `connecting`, no callback is made from inside this call, only after it
 * has returned, and none after the session is over (`closed` or `error`), however it ended.
 * stop() and abort() are safe at any point, in any number.
 */
export function createLiveSession(args: LiveSessionArgs): LiveSession {
  let state: LiveSessionState = 'connecting';
  // The session is over: nothing is delivered, and everything is released.
  let over = false;
  // stop() was called: the audio is no longer captured and the end of input is on its way.
  let stopping = false;
  // The socket is open, and every batch of audio goes straight to it.
  let open = false;
  // The worklet has handed over its last batch (after stop()).
  let flushed = false;
  let endSent = false;

  let context: AudioContext | null = null;
  let stream: MediaStream | null = null;
  let source: MediaStreamAudioSourceNode | null = null;
  let node: AudioWorkletNode | null = null;
  let socket: WebSocket | null = null;
  let graceTimer: ReturnType<typeof setTimeout> | undefined;
  let flushTimer: ReturnType<typeof setTimeout> | undefined;
  const pending: ArrayBuffer[] = [];

  const setState = (next: LiveSessionState) => {
    if (state === next) return;
    state = next;
    args.onState(next);
  };

  const stopTracks = (media: MediaStream | null) => {
    for (const track of media?.getTracks() ?? []) {
      track.onended = null;
      track.stop();
    }
  };

  // Ends the session for good: everything is released first, then `notify` (the onDone or
  // onError that says why) and the final state are delivered. The first call wins.
  const finish = (final: 'closed' | 'error', notify?: () => void) => {
    if (over) return;
    over = true;
    clearTimeout(graceTimer);
    clearTimeout(flushTimer);
    pending.length = 0;
    if (socket) {
      socket.onopen = socket.onmessage = socket.onerror = socket.onclose = null;
      try {
        socket.close(1000);
      } catch {
        // Already closed.
      }
    }
    if (node) node.port.onmessage = null;
    for (const audioNode of [node, source]) {
      try {
        audioNode?.disconnect();
      } catch {
        // Not connected.
      }
    }
    stopTracks(stream);
    // A page may hold only a few AudioContexts, and each pins audio resources until closed.
    void context?.close().catch(() => undefined);
    socket = node = source = stream = context = null;
    try {
      notify?.();
    } finally {
      setState(final);
    }
  };

  const fail = (message: string, detail: LiveErrorDetail) =>
    finish('error', () => args.onError(message, detail));

  // The end of input, once everything captured has been sent: the last batch from the worklet
  // (flushed), and the connection to send it on (open).
  const sendEndIfReady = () => {
    if (!stopping || !flushed || !open || endSent || !socket) return;
    endSent = true;
    socket.send(JSON.stringify({ type: 'end' }));
  };

  const deliverAudio = (pcm: ArrayBuffer) => {
    // After the last batch (stop() is waiting for the transcript) whatever the worklet still
    // produces is the silence of a microphone that is no longer connected: not for the gateway,
    // which has been, or is about to be, told that the input is over.
    if (flushed) return;
    if (open && socket) {
      socket.send(pcm);
      return;
    }
    if (pending.length >= MAX_PENDING_BATCHES) pending.shift();
    pending.push(pcm);
  };

  const onWorkletMessage = (event: MessageEvent) => {
    if (over) return;
    if (event.data instanceof ArrayBuffer) {
      deliverAudio(event.data);
    } else if (event.data === LIVE_FLUSHED_MESSAGE) {
      flushed = true;
      sendEndIfReady();
    }
  };

  const onSocketMessage = (event: MessageEvent) => {
    if (typeof event.data !== 'string') return;
    const frame = parseLiveDownFrame(event.data);
    if (!frame) return;
    switch (frame.type) {
      case 'delta':
        args.onDelta(frame.text);
        break;
      case 'partial':
        args.onPartial(frame.text);
        break;
      case 'done':
        finish('closed', () => args.onDone(frame.text));
        break;
      case 'error':
        fail(frame.message || 'live transcription failed', {
          kind: 'refused',
          ...(frame.code === undefined ? {} : { code: frame.code }),
        });
        break;
    }
  };

  // The microphone into the worklet. The audio starts flowing, into `pending`, from here.
  const connectCapture = (ctx: AudioContext, media: MediaStream) => {
    source = ctx.createMediaStreamSource(media);
    // One input channel in explicit mode: the browser itself mixes the microphone down to mono.
    node = new AudioWorkletNode(ctx, LIVE_CAPTURE_PROCESSOR_NAME, {
      numberOfInputs: 1,
      numberOfOutputs: 1,
      outputChannelCount: [1],
      channelCount: 1,
      channelCountMode: 'explicit',
      processorOptions: { outputRate: LIVE_TRANSCRIPTION_SAMPLE_RATE },
    });
    node.port.onmessage = onWorkletMessage;
    // A processor that throws goes silent: audio would stop flowing with nothing to say so,
    // and only the end of the recording would notice. Fail at once instead.
    node.onprocessorerror = () => fail('the audio capture stopped working', { kind: 'capture' });
    source.connect(node);
    // The node writes no output (silence), but is connected on to the destination so that every
    // browser pulls it: one that only renders what reaches the destination would never run it.
    node.connect(ctx.destination);
    // A microphone that goes away (unplugged, permission revoked) ends the recording like Stop.
    for (const track of media.getAudioTracks()) track.onended = () => session.stop();
  };

  const openSocket = (ticket: string) => {
    const ws = new WebSocket(liveTranscriptionWsUrl(window.location), [
      LIVE_WS_SUBPROTOCOL,
      ticket,
    ]);
    socket = ws;
    ws.onopen = () => {
      open = true;
      if (!stopping) setState('listening');
      for (const pcm of pending.splice(0)) ws.send(pcm);
      sendEndIfReady();
    };
    ws.onmessage = onSocketMessage;
    // An error event carries nothing; the close that follows it has the code and the reason.
    ws.onclose = (event) => {
      const why = event.reason ? `: ${event.reason}` : '';
      const message = `live transcription connection closed (${event.code}${why})`;
      // A close the gateway chose ("busy", try again later) is a no; any other end of the
      // connection without a final transcript is a drop.
      fail(
        message,
        event.code === WS_TRY_AGAIN_LATER
          ? { kind: 'refused', code: LIVE_BUSY_CODE }
          : { kind: 'dropped' },
      );
    };
  };

  // After the microphone and the worklet settle, validate them and bring the capture graph
  // up. Returns false when the session was aborted while the user was answering the
  // permission prompt (the caller stops before minting a ticket); throws LiveFailure on a
  // denied or failed microphone, a failed worklet load, or a context the browser will not run.
  const bringUpCapture = async (
    media: PromiseSettledResult<MediaStream>,
    worklet: PromiseSettledResult<void>,
    ctx: AudioContext,
    resumed: Promise<void | undefined>,
  ): Promise<boolean> => {
    if (media.status === 'fulfilled') {
      // Held from here, and released with the rest -- or at once if the session is already over
      // (aborted while the user was answering the permission prompt), which would leave the
      // microphone on with nothing to stop it.
      if (over) stopTracks(media.value);
      else stream = media.value;
    }
    if (over) return false;
    if (media.status === 'rejected') {
      throw new LiveFailure(describeError(media.reason), {
        kind: isPermissionDenied(media.reason) ? 'denied' : 'capture',
      });
    }
    if (worklet.status === 'rejected') {
      throw new LiveFailure(describeError(worklet.reason), { kind: 'capture' });
    }
    // A context the browser did not let run (it was resumed in the gesture above, and has had
    // all the time the permission prompt took) captures nothing: the worklet is never called,
    // and the session would sit "listening" to silence until its grace period ran out.
    if (ctx.state !== 'running') await settleWithin(resumed, CONTEXT_RUN_WAIT_MS);
    if (over) return false;
    if (ctx.state !== 'running') {
      throw new LiveFailure('the browser would not start the audio capture', { kind: 'capture' });
    }
    connectCapture(ctx, media.value);
    return true;
  };

  const start = async () => {
    // Everything up to the first await is synchronous, so it is inside the user's gesture.
    const Context = audioContextConstructor();
    const mediaDevices = typeof navigator === 'undefined' ? undefined : navigator.mediaDevices;
    if (!Context || !mediaDevices?.getUserMedia) {
      throw new LiveFailure('this browser cannot capture the microphone', { kind: 'unsupported' });
    }
    const ctx = new Context();
    context = ctx;
    if (!ctx.audioWorklet) {
      throw new LiveFailure('this browser has no AudioWorklet', { kind: 'unsupported' });
    }
    const resumed = ctx.resume().catch(() => undefined);
    const [media, worklet] = await Promise.allSettled([
      mediaDevices.getUserMedia({ audio: true }),
      ctx.audioWorklet.addModule(workletUrl),
    ]);
    if (!(await bringUpCapture(media, worklet, ctx, resumed))) return;
    let ticket: string;
    try {
      ({ ticket } = await args.api.createLiveTranscriptionTicket({
        model: args.model,
        language: args.language,
      }));
    } catch (error) {
      throw ticketFailure(error);
    }
    if (over) return;
    openSocket(ticket);
  };

  const session: LiveSession = {
    stop() {
      if (over || stopping) return;
      // Nothing captured yet (the microphone has not been granted): nothing to finish.
      if (!node) {
        finish('closed');
        return;
      }
      stopping = true;
      setState('finalizing');
      // The microphone is off now (its indicator with it); the worklet is asked for the batch it
      // was still filling, and the end of input follows that batch.
      try {
        source?.disconnect();
      } catch {
        // Not connected.
      }
      stopTracks(stream);
      graceTimer = setTimeout(
        () => fail('live transcription did not deliver its final transcript', { kind: 'dropped' }),
        LIVE_FINALIZE_GRACE_MS,
      );
      flushTimer = setTimeout(() => {
        flushed = true;
        sendEndIfReady();
      }, FLUSH_WAIT_MS);
      node.port.postMessage(LIVE_FLUSH_MESSAGE);
    },
    abort() {
      finish('closed');
    },
  };

  args.onState(state);
  start().catch((error: unknown) =>
    fail(describeError(error), error instanceof LiveFailure ? error.detail : { kind: 'capture' }),
  );
  return session;
}
