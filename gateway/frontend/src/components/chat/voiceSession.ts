// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// The portal's side of a Voice Chat session (issue #312): the microphone captured as 16 kHz mono
// s16le PCM (captureWorklet.ts) and streamed up a WebSocket, and everything the gateway's voice
// orchestrator sends back down it -- its state, the live transcript of what the user is saying,
// the answer as it is written, and the answer as audio -- routed to the caller's callbacks and,
// for the audio, to a StreamingPcmPlayer. It is the twin of the live transcription client
// (liveTranscription.ts), and is built the same way; what differs is that the session is a whole
// conversation, not one recording: it spans many turns, and the gateway, not the client, decides
// when a turn starts and ends.
//
// The wire contract, which the gateway's /api/portal/voice-chat implements:
//
//   - A ticket is minted first, by a POST the portal's CSRF protection covers (a WebSocket cannot
//     carry the X-OP-CSRF header). It names the chat and the three models of the loop, and the
//     gateway holds them in memory together before it answers, so this POST can take minutes on
//     a cold start. The ticket rides the upgrade as the second subprotocol: new WebSocket(url,
//     [name, ticket]). It is single-use and good for 30 seconds, so it is used at once.
//   - UP: binary frames of raw s16le PCM, ~100 ms each (a frame of 1 MiB or more closes the
//     connection with 1009), and text control frames {"type": "push_start" | "push_end" |
//     "cancel" | "bye"}. The microphone is sent up for as long as the session lasts: the gateway
//     discards it while the answer is being spoken (it is half-duplex, enforced there, so that
//     the answer can never be heard as speech).
//   - DOWN: text frames, parsed by parseVoiceDownFrame below. `state` is the gateway's state
//     machine; `asr_partial` is the provisional transcript of the whole of what is being said
//     (it REPLACES the last one), `asr_final` the finished user turn; `answer_delta` is a piece
//     of the answer's text (it is APPENDED), `answer_done` its end; `answer_reset` says the
//     answer so far is void, because the user went on talking; `audio_delta` is a chunk of the
//     spoken answer and `audio_done` its end; `error` is a turn that failed.
//
// Display is provisional: the gateway persists the turns, so nothing here commits one. What is
// shown from `asr_partial` and `answer_delta` is a preview, and what the chat shows once the
// turn is committed (`audio_done`) is the stored thread.
//
// A session is made with createVoiceSession and ended with stop(). This file is the glue between
// that and the browser's audio and WebSocket, which cannot be run in a test; what can be tested
// is pulled out of it: parseVoiceDownFrame and voiceChatWsUrl here, the audio in livePcm.ts and
// speechPlayback.ts.

import { LIVE_TRANSCRIPTION_SAMPLE_RATE, type PortalApi } from '../../api';
import workletUrl from './captureWorklet.ts?worker&url';
import {
  CONTEXT_RUN_WAIT_MS,
  FLUSH_WAIT_MS,
  LiveFailure,
  MAX_PENDING_BATCHES,
  audioContextConstructor,
  describeError,
  isPermissionDenied,
  settleWithin,
  ticketFailure,
  type LiveErrorKind,
} from './liveTranscription';
import { LIVE_CAPTURE_PROCESSOR_NAME, LIVE_FLUSHED_MESSAGE, LIVE_FLUSH_MESSAGE } from './livePcm';
import {
  StreamingPcmPlayer,
  acceptChunk,
  decodeBase64,
  type LiveState,
  type PlaybackContext,
} from './speechPlayback';

// The subprotocol the gateway selects; the ticket is the other one offered.
export const VOICE_WS_SUBPROTOCOL = 'op-voice-chat';

// The API path is absolute: the portal's SPA lives under /portal/, its API does not.
export const VOICE_WS_PATH = '/api/portal/voice-chat';

/** The states the gateway's voice state machine reports in a `state` frame. */
export type VoiceServerState = 'idle' | 'listening' | 'thinking' | 'speaking' | 'draining';

const SERVER_STATES: readonly string[] = [
  'idle',
  'listening',
  'thinking',
  'speaking',
  'draining',
] satisfies VoiceServerState[];

/**
 * Where a voice session stands: `connecting` until the gateway says otherwise, then the gateway's
 * own state (only in `listening` is the user heard), and finally `closed` or `error`, which are
 * final.
 */
export type VoiceSessionState = 'connecting' | VoiceServerState | 'closed' | 'error';

/** A frame of the voice stream, parsed. */
export type VoiceDownFrame =
  | { type: 'state'; state: VoiceServerState }
  | { type: 'asr_partial'; text: string }
  | { type: 'asr_final'; text: string; messageId?: string }
  | { type: 'answer_delta'; text: string }
  | { type: 'answer_done'; messageId?: string }
  | { type: 'answer_reset' }
  | { type: 'audio_delta'; seq: number; rate: number; audio: string }
  | { type: 'audio_done' }
  | { type: 'error'; message: string; code?: string };

/**
 * Reads one text frame the gateway sent down, or null for anything that is not one: text that is
 * not JSON, JSON that is not an object, an unknown `type` (or `state`), a field of the wrong
 * type. Never throws, and a frame the client does not understand is for the caller to ignore, so
 * that a newer gateway adding a frame type cannot break an older portal.
 *
 * The gateway leaves an empty field out of its frames, so a missing `text`, `message` or `audio`
 * is the empty string. A `message_id` or an error's `code` is an optional hint: it is left off
 * the parsed frame when it is missing, empty or not a string, and a frame must stay the frame it
 * is whatever it says.
 */
export function parseVoiceDownFrame(data: string): VoiceDownFrame | null {
  let parsed: unknown;
  try {
    parsed = JSON.parse(data);
  } catch {
    return null;
  }
  if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) return null;
  const frame = parsed as Record<string, unknown>;
  const hint = (value: unknown): string | undefined =>
    typeof value === 'string' && value !== '' ? value : undefined;
  // A text field: absent is empty, but present it must be text.
  const text = (value: unknown): string | null => {
    if (value === undefined) return '';
    return typeof value === 'string' ? value : null;
  };
  switch (frame.type) {
    case 'state':
      return typeof frame.state === 'string' && SERVER_STATES.includes(frame.state)
        ? { type: 'state', state: frame.state as VoiceServerState }
        : null;
    case 'asr_partial':
    case 'answer_delta': {
      const body = text(frame.text);
      return body === null ? null : { type: frame.type, text: body };
    }
    case 'asr_final': {
      const body = text(frame.text);
      if (body === null) return null;
      const messageId = hint(frame.message_id);
      return messageId === undefined
        ? { type: 'asr_final', text: body }
        : { type: 'asr_final', text: body, messageId };
    }
    case 'answer_done': {
      const messageId = hint(frame.message_id);
      return messageId === undefined ? { type: 'answer_done' } : { type: 'answer_done', messageId };
    }
    case 'answer_reset':
    case 'audio_done':
      return { type: frame.type };
    case 'audio_delta': {
      const audio = text(frame.audio);
      if (typeof frame.seq !== 'number' || typeof frame.rate !== 'number' || audio === null) {
        return null;
      }
      return { type: 'audio_delta', seq: frame.seq, rate: frame.rate, audio };
    }
    case 'error': {
      const message = text(frame.message);
      if (message === null) return null;
      const code = hint(frame.code);
      return code === undefined ? { type: 'error', message } : { type: 'error', message, code };
    }
    default:
      return null;
  }
}

/** The WebSocket URL of the voice endpoint on the page's own origin (the session cookie rides it). */
export function voiceChatWsUrl(location: { protocol: string; host: string }): string {
  return `${location.protocol === 'https:' ? 'wss:' : 'ws:'}//${location.host}${VOICE_WS_PATH}`;
}

/** What createVoiceSession needs of the portal's API client: the ticket POST. */
export type VoiceTicketApi = Pick<PortalApi, 'createVoiceChatTicket'>;

/**
 * Why a session failed, or one turn of it.
 *
 *  - `turn`: one turn failed (a model errored, an admission was refused) and the gateway has put
 *    the session back to `idle`: it is NOT over, and the user re-engages with pushStart. `code`
 *    is the stable code of the leg that failed.
 *  - Every other kind ends the session, with the same meaning as for a live transcription
 *    (LiveErrorKind): `dropped` -- the connection ended without the gateway saying why, or the
 *    ticket request did not get through (a retry may work); `refused` -- the gateway said no, a
 *    ticket it refused, with its `code` (the same request is refused again); `denied` -- the
 *    microphone was refused; `unsupported` -- this browser cannot capture audio this way;
 *    `capture` -- the microphone or the audio pipeline failed.
 */
export type VoiceErrorKind = LiveErrorKind | 'turn';

export type VoiceErrorDetail = {
  kind: VoiceErrorKind;
  /** The gateway's error code, when there is one. */
  code?: string;
};

/** The user's words: the gateway's live transcript of the turn, or the finished turn. */
export type VoiceTranscript = {
  /** The whole turn so far (it replaces the transcript shown), or all of it when `final`. */
  text: string;
  /** The turn is finished, and the gateway has persisted it as a user message (`messageId`). */
  final: boolean;
  messageId?: string;
};

/** The answer as the model writes it. */
export type VoiceAnswer = {
  /** A piece of the answer (it is appended to what is shown); empty when `done`. */
  text: string;
  /** The answer is complete, and persisted as an assistant message (`messageId`). */
  done: boolean;
  messageId?: string;
};

/**
 * What the session needs of an answer's player: the slice of StreamingPcmPlayer it uses.
 * `onEmpty` is the #304 drain hook -- the player calls it when its queue has played out, which
 * is how the session tells the gateway the answer finished playing (`playback_done`), so the
 * gateway can end its half-duplex drain at true playout-done rather than relay-done (spec 6.2).
 */
export type VoicePlayer = Pick<StreamingPcmPlayer, 'push' | 'stop' | 'onEmpty'>;

export type VoiceSessionArgs = {
  api: VoiceTicketApi;
  /** The chat the spoken turns belong to. */
  chatId: string;
  /** The gateway models of the loop: the live transcription, the chat and the speech model. */
  asrModel: string;
  llmModel: string;
  ttsModel: string;
  /** The turn as it is being said (a partial replaces the last) and as it was finished. */
  onTranscript(transcript: VoiceTranscript): void;
  /** The answer's text as it streams, and its end. */
  onAnswer(answer: VoiceAnswer): void;
  /** The answer so far is void (the user went on talking): drop the preview of it. */
  onAnswerReset(): void;
  /** The spoken answer is all sent (the turn is committed): the stored thread has the turn. */
  onAudioDone(): void;
  /** Every state the session is in, `connecting` (made before createVoiceSession returns) first. */
  onState(state: VoiceSessionState): void;
  /** A turn failed (`kind: 'turn'`, the session goes on) or the session failed (it is over). */
  onError(message: string, detail: VoiceErrorDetail): void;
  /**
   * Makes the player of one answer's audio on the session's AudioContext. The default is a
   * StreamingPcmPlayer; a test passes a stand-in, since sound cannot be heard in jsdom.
   */
  createPlayer?: (ctx: PlaybackContext) => VoicePlayer;
};

export type VoiceSession = {
  /** The user starts talking (push-to-talk): a hint to the gateway, which also listens without it. */
  pushStart(): void;
  /** The user is done talking: the turn is finished now, without waiting for a pause. */
  pushEnd(): void;
  /** Abandon the turn in progress: its answer is silenced here and the gateway returns to listening. */
  cancel(): void;
  /** End the conversation: say goodbye, stop the microphone and the audio, and tear everything down. */
  stop(): void;
};

const defaultCreatePlayer = (ctx: PlaybackContext): VoicePlayer => new StreamingPcmPlayer(ctx);

/**
 * Start a Voice Chat session: the microphone, the connection, and the conversation.
 *
 * Call it from inside the user's gesture (the click that turns Voice Chat on): the AudioContext
 * and the microphone request are made before the first await, which is what lets a browser that
 * restricts autoplay (Safari) run the context -- and so play the answers -- at all.
 *
 * The order is: the microphone is asked for (the user may take a while to answer), then a ticket
 * is minted and the WebSocket opened, while the audio already being captured is held for it.
 * Apart from the initial `connecting`, no callback is made from inside this call, only after it
 * has returned, and none after the session is over (`closed` or `error`), however it ended.
 * stop() is safe at any point, in any number; the other controls are dropped until the socket is
 * open (there is nobody to tell), and after the end.
 */
export function createVoiceSession(args: VoiceSessionArgs): VoiceSession {
  const createPlayer = args.createPlayer ?? defaultCreatePlayer;
  let state: VoiceSessionState = 'connecting';
  // The session is over: nothing is delivered, and everything is released.
  let over = false;
  // The socket is open, and every batch of audio goes straight to it.
  let open = false;
  // pushEnd() asked the worklet for the last batch of audio, which push_end must follow.
  let pushEndPending = false;

  let context: AudioContext | null = null;
  let stream: MediaStream | null = null;
  let source: MediaStreamAudioSourceNode | null = null;
  let node: AudioWorkletNode | null = null;
  let socket: WebSocket | null = null;
  let pushEndTimer: ReturnType<typeof setTimeout> | undefined;
  const pending: ArrayBuffer[] = [];

  // The answer being spoken. Each answer's audio is its own stream, numbered from 0, so it has a
  // player of its own and a playback state that starts again at `waiting`.
  let player: VoicePlayer | null = null;
  let playState: LiveState = { phase: 'waiting' };

  const setState = (next: VoiceSessionState) => {
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

  // Silence what is playing and forget the stream it was part of.
  const stopPlayback = () => {
    player?.stop();
    player = null;
    playState = { phase: 'waiting' };
  };

  // Ends the session for good: everything is released first, then `notify` (the onError that says
  // why) and the final state are delivered. The first call wins.
  const finish = (final: 'closed' | 'error', notify?: () => void) => {
    if (over) return;
    over = true;
    clearTimeout(pushEndTimer);
    pushEndPending = false;
    pending.length = 0;
    if (socket) {
      // An end on our own terms (Stop, a microphone that failed) says goodbye first, so that the
      // gateway ends the conversation cleanly. When the socket is already gone (`open` is
      // cleared by its close) there is nobody to say it to.
      if (open) {
        try {
          socket.send(JSON.stringify({ type: 'bye' }));
        } catch {
          // Already closing.
        }
      }
      socket.onopen = socket.onmessage = socket.onerror = socket.onclose = null;
      try {
        socket.close(1000);
      } catch {
        // Already closed.
      }
    }
    open = false;
    stopPlayback();
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

  const fail = (message: string, detail: VoiceErrorDetail) =>
    finish('error', () => args.onError(message, detail));

  // One text control frame, when there is a socket to send it on.
  const sendControl = (type: 'push_start' | 'push_end' | 'cancel' | 'playback_done') => {
    if (over || !open || !socket) return;
    socket.send(JSON.stringify({ type }));
  };

  // push_end, once everything captured has been sent: the last batch from the worklet (its
  // `flushed` answer follows it on the same, ordered, port), or the wait for it having run out.
  const sendPushEnd = () => {
    if (!pushEndPending) return;
    pushEndPending = false;
    clearTimeout(pushEndTimer);
    sendControl('push_end');
  };

  const deliverAudio = (pcm: ArrayBuffer) => {
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
      sendPushEnd();
    }
  };

  // One chunk of the spoken answer to the player of its stream. acceptChunk is the rule of
  // progressive playback: a chunk is played only while the stream is whole (from chunk 0, in
  // order, at one rate), and the first irregularity abandons the rest of this answer's audio --
  // the stored turn has all of it, and a hole in it would be heard.
  const playChunk = (frame: Extract<VoiceDownFrame, { type: 'audio_delta' }>) => {
    if (!context) return;
    // Every answer's audio starts at chunk 0, and a turn that was cancelled sends no end: so a
    // chunk 0 is a new answer, which replaces what is left of the last.
    if (frame.seq === 0) stopPlayback();
    const verdict = acceptChunk(playState, {
      seq: frame.seq,
      rate: frame.rate,
      data: frame.audio,
    });
    playState = verdict.state;
    if (!verdict.play) return;
    const bytes = decodeBase64(frame.audio);
    if (!player) {
      player = createPlayer(context as unknown as PlaybackContext);
      // The #304 drain hook: when this answer's player has played its queue out, tell the
      // gateway playout is done so it can end its half-duplex drain at true playout-done, not
      // relay-done (spec 6.2). A fresh player is made per answer, so it is wired per player
      // here. The gateway ignores the signal unless it is draining and applies its own
      // fallback, so an early or repeated call cannot move a session that is not waiting on it.
      player.onEmpty = () => sendControl('playback_done');
    }
    if (bytes === null || !player.push(bytes, frame.rate)) {
      playState = { phase: 'abandoned', reason: 'format' };
    }
  };

  const onSocketMessage = (event: MessageEvent) => {
    if (over || typeof event.data !== 'string') return;
    const frame = parseVoiceDownFrame(event.data);
    if (!frame) return;
    switch (frame.type) {
      case 'state':
        setState(frame.state);
        break;
      case 'asr_partial':
        args.onTranscript({ text: frame.text, final: false });
        break;
      case 'asr_final':
        args.onTranscript({
          text: frame.text,
          final: true,
          ...(frame.messageId === undefined ? {} : { messageId: frame.messageId }),
        });
        break;
      case 'answer_delta':
        args.onAnswer({ text: frame.text, done: false });
        break;
      case 'answer_done':
        args.onAnswer({
          text: '',
          done: true,
          ...(frame.messageId === undefined ? {} : { messageId: frame.messageId }),
        });
        break;
      case 'answer_reset':
        args.onAnswerReset();
        break;
      case 'audio_delta':
        playChunk(frame);
        break;
      case 'audio_done':
        args.onAudioDone();
        break;
      case 'error':
        // One turn failed; the gateway is back at idle and the session goes on.
        args.onError(frame.message || frame.code || 'the voice turn could not be completed', {
          kind: 'turn',
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
    // A processor that throws goes silent: audio would stop flowing with nothing to say so, and
    // the user would talk to a session that cannot hear them. Fail at once instead.
    node.onprocessorerror = () => fail('the audio capture stopped working', { kind: 'capture' });
    source.connect(node);
    // The node writes no output (silence), but is connected on to the destination so that every
    // browser pulls it: one that only renders what reaches the destination would never run it.
    node.connect(ctx.destination);
    // A microphone that goes away (unplugged, permission revoked) leaves a conversation that
    // cannot hear: end it, and say why.
    for (const track of media.getAudioTracks()) {
      track.onended = () => fail('the microphone stopped', { kind: 'capture' });
    }
  };

  const openSocket = (ticket: string) => {
    const ws = new WebSocket(voiceChatWsUrl(window.location), [VOICE_WS_SUBPROTOCOL, ticket]);
    socket = ws;
    ws.onopen = () => {
      open = true;
      for (const pcm of pending.splice(0)) ws.send(pcm);
    };
    ws.onmessage = onSocketMessage;
    // An error event carries nothing; the close that follows it has the code and the reason. The
    // gateway ends a session only when told to (bye) or when it fails, so a close that this side
    // did not ask for is a drop.
    ws.onclose = (event) => {
      open = false;
      const why = event.reason ? `: ${event.reason}` : '';
      fail(`voice chat connection closed (${event.code}${why})`, { kind: 'dropped' });
    };
  };

  // After the microphone and the worklet settle, validate them and bring the capture graph up.
  // Returns false when the session was stopped while the user was answering the permission
  // prompt (the caller stops before minting a ticket); throws LiveFailure on a denied or failed
  // microphone, a failed worklet load, or a context the browser will not run.
  const bringUpCapture = async (
    media: PromiseSettledResult<MediaStream>,
    worklet: PromiseSettledResult<void>,
    ctx: AudioContext,
    resumed: Promise<void | undefined>,
  ): Promise<boolean> => {
    if (media.status === 'fulfilled') {
      // Held from here, and released with the rest -- or at once if the session is already over
      // (stopped while the user was answering the permission prompt), which would leave the
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
    // A context the browser did not let run (it was resumed in the gesture, and has had all the
    // time the permission prompt took) captures nothing -- and plays nothing: the worklet is
    // never called, and the session would sit "connected" to silence.
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
      // Echo cancellation and noise suppression are a belt-and-suspenders second line under the
      // server-authoritative half-duplex gate (spec 6.2): the gate already stops TTS-era audio
      // from reaching the ASR, and these further damp any room echo the microphone picks up.
      mediaDevices.getUserMedia({ audio: { echoCancellation: true, noiseSuppression: true } }),
      ctx.audioWorklet.addModule(workletUrl),
    ]);
    if (!(await bringUpCapture(media, worklet, ctx, resumed))) return;
    let ticket: string;
    try {
      ({ ticket } = await args.api.createVoiceChatTicket({
        chatId: args.chatId,
        asrModel: args.asrModel,
        llmModel: args.llmModel,
        ttsModel: args.ttsModel,
      }));
    } catch (error) {
      throw ticketFailure(error);
    }
    if (over) return;
    // The audio held so far is from the wait for the ticket, which can last minutes (the gateway
    // loads the models before it answers): speech that was not for anyone, and nothing the user
    // should see transcribed once the conversation opens. What is held from here on is the
    // handshake's, which is what the buffer is for.
    pending.length = 0;
    openSocket(ticket);
  };

  const session: VoiceSession = {
    pushStart() {
      sendControl('push_start');
    },
    pushEnd() {
      if (over || !open || !node || pushEndPending) return;
      pushEndPending = true;
      // The worklet is asked for the batch it was still filling -- the end of the last word --
      // and push_end follows that batch, or the wait for it (a worklet that died does not answer).
      pushEndTimer = setTimeout(sendPushEnd, FLUSH_WAIT_MS);
      node.port.postMessage(LIVE_FLUSH_MESSAGE);
    },
    cancel() {
      if (over) return;
      // A push_end still waiting for its batch is the turn being abandoned, too.
      pushEndPending = false;
      clearTimeout(pushEndTimer);
      // The answer already sent down and queued to play would otherwise be heard to the end.
      stopPlayback();
      sendControl('cancel');
    },
    stop() {
      finish('closed');
    },
  };

  args.onState(state);
  start().catch((error: unknown) =>
    fail(describeError(error), error instanceof LiveFailure ? error.detail : { kind: 'capture' }),
  );
  return session;
}
