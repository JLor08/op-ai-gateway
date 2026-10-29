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
//     "cancel" | "bye" | "playback_done" | "pause" | "resume" | "stop"}. The microphone is sent
//     up for as long as the session lasts, unless the user has muted it (setMicMuted: then none
//     is sent, and the gateway hears nothing and detects no turn): the gateway discards it while
//     the answer is being spoken (it is half-duplex, enforced there, so that the answer can
//     never be heard as speech). `pause`/`resume` hold and release the gateway's playout drain
//     (so the microphone cannot reopen over audio the user has paused), and `stop` cuts the
//     spoken answer short and puts the gateway back to listening. The answer can also be muted
//     (setMuted): a local matter of the speakers, which the gateway is told nothing of.
//   - DOWN: text frames, parsed by parseVoiceDownFrame below. `state` is the gateway's state
//     machine; `asr_partial` is the provisional transcript of the CURRENT CHUNK only (it
//     REPLACES the last partial, not the turn's transcript), and `asr_final` the finished text
//     of that one chunk. A turn can span several chunks -- a pause the user goes on from ends a
//     chunk, not the turn -- so the caller accumulates the finals and shows them plus the
//     current partial. `answer_delta` is a piece of the answer's text (it is APPENDED),
//     `answer_done` its end, which is provisional: it comes before the turn is committed, and
//     `answer_reset` says the answer so far is void, because the user went on talking.
//     `audio_delta` is a chunk of the spoken answer and `audio_done` its end, which is when the
//     turn has been committed; `error` is a turn that failed. An answer may be several spoken
//     sections: their audio is ONE stream (one `seq`, numbered from 0 once per answer), and an
//     `audio_delta` also names the `section` it belongs to, which only says which part of the
//     answer is being heard (see VoiceAudioSection).
//
// Display is provisional: the gateway persists the turns, and does so once, when the turn is
// committed (the user's words and the answer, text and audio, together), so nothing here commits
// one. What is shown from `asr_partial` and `answer_delta` is a preview; at `audio_done` the
// caller reloads the chat's thread, and the stored turns replace it. No frame carries the id of
// a stored message, which is why the thread is reloaded rather than patched.
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
  | { type: 'audio_delta'; seq: number; rate: number; audio: string; section?: number }
  | { type: 'audio_done' }
  | { type: 'error'; message: string; code?: string };

/** The fields of a down frame, before its `type` has said which frame it is. */
type FrameFields = Record<string, unknown>;

/** An optional hint field: the string, or undefined when it is missing, empty or not a string. */
const hintField = (value: unknown): string | undefined =>
  typeof value === 'string' && value !== '' ? value : undefined;

/** A text field: absent is empty, but present it must be text. */
const textField = (value: unknown): string | null => {
  if (value === undefined) return '';
  return typeof value === 'string' ? value : null;
};

/** The object a text frame holds, or null for text that is not JSON or JSON that is not an object. */
function parseFrameObject(data: string): FrameFields | null {
  let parsed: unknown;
  try {
    parsed = JSON.parse(data);
  } catch {
    return null;
  }
  if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) return null;
  return parsed as FrameFields;
}

function parseStateFrame(frame: FrameFields): VoiceDownFrame | null {
  return typeof frame.state === 'string' && SERVER_STATES.includes(frame.state)
    ? { type: 'state', state: frame.state as VoiceServerState }
    : null;
}

function parseTextFrame(
  type: 'asr_partial' | 'answer_delta',
  frame: FrameFields,
): VoiceDownFrame | null {
  const body = textField(frame.text);
  return body === null ? null : { type, text: body };
}

function parseAsrFinalFrame(frame: FrameFields): VoiceDownFrame | null {
  const body = textField(frame.text);
  if (body === null) return null;
  const messageId = hintField(frame.message_id);
  return messageId === undefined
    ? { type: 'asr_final', text: body }
    : { type: 'asr_final', text: body, messageId };
}

function parseAnswerDoneFrame(frame: FrameFields): VoiceDownFrame {
  const messageId = hintField(frame.message_id);
  return messageId === undefined ? { type: 'answer_done' } : { type: 'answer_done', messageId };
}

/** An optional section index: a whole number from 0, or undefined when it is missing or is not one. */
const sectionField = (value: unknown): number | undefined =>
  typeof value === 'number' && Number.isInteger(value) && value >= 0 ? value : undefined;

function parseAudioDeltaFrame(frame: FrameFields): VoiceDownFrame | null {
  const audio = textField(frame.audio);
  if (typeof frame.seq !== 'number' || typeof frame.rate !== 'number' || audio === null) {
    return null;
  }
  // The section only attributes the audio, so a tag that is not a section index is left off and
  // the chunk stays the audio it is: dropping it would put a hole in what is played.
  const section = sectionField(frame.section);
  return section === undefined
    ? { type: 'audio_delta', seq: frame.seq, rate: frame.rate, audio }
    : { type: 'audio_delta', seq: frame.seq, rate: frame.rate, audio, section };
}

function parseErrorFrame(frame: FrameFields): VoiceDownFrame | null {
  const message = textField(frame.message);
  if (message === null) return null;
  const code = hintField(frame.code);
  return code === undefined ? { type: 'error', message } : { type: 'error', message, code };
}

/**
 * Reads one text frame the gateway sent down, or null for anything that is not one: text that is
 * not JSON, JSON that is not an object, an unknown `type` (or `state`), a field of the wrong
 * type. Never throws, and a frame the client does not understand is for the caller to ignore, so
 * that a newer gateway adding a frame type cannot break an older portal.
 *
 * The gateway leaves an empty field out of its frames, so a missing `text`, `message` or `audio`
 * is the empty string (and a missing audio `section`, which the gateway leaves out for the first
 * section, is left off the parsed frame too). A `message_id`, an error's `code` or an audio
 * `section` is an optional hint: it is left off the parsed frame when it is missing, empty or not
 * of its type, and a frame must stay the frame it is whatever it says. (The gateway sends no
 * `message_id` today -- nothing is persisted before the turn is committed -- so it is read for a
 * newer gateway that does, and nothing relies on it.)
 */
export function parseVoiceDownFrame(data: string): VoiceDownFrame | null {
  const frame = parseFrameObject(data);
  if (frame === null) return null;
  switch (frame.type) {
    case 'state':
      return parseStateFrame(frame);
    case 'asr_partial':
    case 'answer_delta':
      return parseTextFrame(frame.type, frame);
    case 'asr_final':
      return parseAsrFinalFrame(frame);
    case 'answer_done':
      return parseAnswerDoneFrame(frame);
    case 'answer_reset':
    case 'audio_done':
      return { type: frame.type };
    case 'audio_delta':
      return parseAudioDeltaFrame(frame);
    case 'error':
      return parseErrorFrame(frame);
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
 *    ticket request did not get through (a retry may work; a 5xx of the ticket endpoint carries
 *    its own `code`, portal.voice_chat_ticket_failed); `refused` -- the gateway said no, a
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

/**
 * The user's words, one chunk of them. A turn is one or more chunks (a pause that the user goes
 * on from ends a chunk, not the turn), so this is never the whole turn: the caller accumulates
 * the finals, and shows them plus the current partial. Provisional all the same -- the turn is
 * persisted by the gateway, only once it is committed (`onAudioDone`).
 */
export type VoiceTranscript = {
  /**
   * The current chunk so far, which replaces the last partial (not what the earlier chunks said),
   * or the finished text of the chunk when `final`.
   */
  text: string;
  /** The chunk is finished: its `text` is final, and the caller keeps it. The turn is not over. */
  final: boolean;
  /** An id the gateway may send for a stored message: it sends none today, and nothing uses it. */
  messageId?: string;
};

/** The answer as the model writes it. Provisional until the turn is committed (`onAudioDone`). */
export type VoiceAnswer = {
  /** A piece of the answer (it is appended to what is shown); empty when `done`. */
  text: string;
  /**
   * The answer's text is complete. That is provisional: it comes before the turn is committed,
   * and a later `onAnswerReset` can still void it (the user went on talking).
   */
  done: boolean;
  /** An id the gateway may send for a stored message: it sends none today, and nothing uses it. */
  messageId?: string;
};

/**
 * What the session needs of an answer's player: the slice of StreamingPcmPlayer it uses.
 * `onEmpty` is the #304 drain hook -- the player calls it when its queue has played out, which
 * is how the session tells the gateway the answer finished playing (`playback_done`), so the
 * gateway can end its half-duplex drain at true playout-done rather than relay-done (spec 6.2).
 * `pause`/`resume` hold and continue the playout without ending the stream, and `pending` is how
 * many chunks it has still to play out (zero after a resume means no `onEmpty` will follow it).
 * `setMuted` silences its speakers without stopping its clock.
 */
export type VoicePlayer = Pick<
  StreamingPcmPlayer,
  'push' | 'stop' | 'onEmpty' | 'pause' | 'resume' | 'pending' | 'setMuted'
>;

/**
 * Which section of the spoken answer the audio now arriving belongs to. An answer is spoken in
 * one or more sections, whose audio comes down as ONE ordered stream (a later section is held
 * until the earlier one is sent), so this changes when a new section's first chunk arrives.
 *
 *  - `section` is the gateway's own tag for it. It is the index the section was LAUNCHED at, so
 *    it can skip: a section whose speech failed leaves a hole (0, then 2), and nothing here
 *    assumes the indices are 0..n-1.
 *  - `ordinal` is its place among the sections that have audio, from 0, in the order they were
 *    heard. The stored turn keeps exactly those sections, in that order, so this is the index of
 *    the section's block there, whatever the gaps in `section`.
 *
 * It names the section being RECEIVED: the gateway sends audio faster than it plays, so it can
 * run ahead of what is audible.
 */
export type VoiceAudioSection = { section: number; ordinal: number };

export type VoiceSessionArgs = {
  api: VoiceTicketApi;
  /** The chat the spoken turns belong to. */
  chatId: string;
  /** The gateway models of the loop: the live transcription, the chat and the speech model. */
  asrModel: string;
  llmModel: string;
  ttsModel: string;
  /** The voice the speech model answers in; '' is the model's own default voice. */
  voice: string;
  /**
   * One chunk of the turn as it is being said (a partial replaces the last partial) and as the
   * chunk finished (the caller accumulates the finals; the turn may have more chunks).
   */
  onTranscript(transcript: VoiceTranscript): void;
  /** The answer's text as it streams, and its provisional end. */
  onAnswer(answer: VoiceAnswer): void;
  /** The answer so far is void (the user went on talking): drop the preview of it. */
  onAnswerReset(): void;
  /**
   * The spoken answer is all sent: the turn is committed, and the gateway's stored thread has the
   * user's turn and the answer. The caller reloads the thread (no frame names a stored message).
   */
  onAudioDone(): void;
  /** Every state the session is in, `connecting` (made before createVoiceSession returns) first. */
  onState(state: VoiceSessionState): void;
  /** A turn failed (`kind: 'turn'`, the session goes on) or the session failed (it is over). */
  onError(message: string, detail: VoiceErrorDetail): void;
  /**
   * The spoken answer is paused (`true`) or not (`false`). It changes by the user's pause() and
   * resume(), and by whatever ends the answer instead -- a stop, a cancel, a failed turn, the next
   * answer, the gateway leaving `speaking` -- which clears a pause that was held.
   */
  onPausedChange?(paused: boolean): void;
  /** The audio now arriving is for a new section of the answer (see VoiceAudioSection). */
  onAudioSection?(section: VoiceAudioSection): void;
  /**
   * Whether the answer's audio is being heard (`true`) or not (`false`): it is, from the first
   * chunk that is played until the answer has played out -- the queue running dry once the answer
   * has been sent in full (the gateway draining), whatever the gateway's own tail does after it,
   * or running dry while the gateway is still speaking and staying dry for PLAYING_GAP_MS (the
   * last spoken section is over and the gateway goes on writing; a later section brings it back)
   * -- and it is not once something else ends the answer (a stop, a cancel, a failed turn, the
   * next answer, the gateway going back to listening). A pause does not end it: held audio is still
   * to be heard. It is what the controls of the playback are shown for.
   */
  onPlayingChange?(playing: boolean): void;
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
  /**
   * Pause the spoken answer: the player holds its place and the gateway holds its drain, so the
   * microphone stays closed until resume() (or stopSpeaking()). Dropped unless an answer is
   * actually being heard (nothing is playing before its first chunk, or after it has ended).
   */
  pause(): void;
  /** Continue a paused answer from where it stopped. Dropped when nothing is paused. */
  resume(): void;
  /**
   * Cut the spoken answer short: it is silenced here, and the gateway drops the rest of it and
   * goes back to listening (the conversation goes on). Not the end of the session -- that is stop().
   */
  stopSpeaking(): void;
  /**
   * Mute or unmute the spoken answer, for the rest of the session or until it is unmuted: the
   * speakers only, so nothing is sent, the answer goes on playing (silently, on the same clock)
   * and the gateway drains it as it would have. It holds for the answers to come, too -- each has
   * a player of its own, made muted while this is on.
   */
  setMuted(muted: boolean): void;
  /**
   * Mute or unmute the user's own microphone for the rest of the session, until it is unmuted:
   * while muted nothing is sent up, so the gateway detects no turn. The gateway needs no frame
   * for it, and the answer and the controls above are not affected.
   */
  setMicMuted(muted: boolean): void;
  /** End the conversation: say goodbye, stop the microphone and the audio, and tear everything down. */
  stop(): void;
};

const defaultCreatePlayer = (ctx: PlaybackContext): VoicePlayer => new StreamingPcmPlayer(ctx);

/**
 * How long the answer's queue must stay dry, while the gateway is still `speaking`, before its
 * audio is taken as played out (see onPlayingChange). The gateway only says the answer is over
 * (`audio_done`, then `draining`) when the whole run has finished, and with spoken summaries the
 * model goes on writing the detail long after the last summary has been heard: nothing audible is
 * left then, yet the state is still `speaking`. A gap between two sections -- the synthesis of the
 * next one not yet caught up -- is shorter than this, so the next chunk arrives inside the window
 * and keeps the audio going; a real end has no next chunk.
 */
export const PLAYING_GAP_MS = 1200;

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
  // How many audio chunks the current answer has RECEIVED from the gateway (the #322 playout
  // counter). It rides every playback_done as `played_through`, so the gateway can tell the genuine
  // end-of-audio signal (it accounts for everything relayed) from a stale inter-chunk gap signal
  // that lost the relay/signal race, and so never ends its half-duplex drain while later audio is
  // still playing out here. It counts audio RECEIVED (not only played), which keeps the count
  // aligned with what the gateway relayed when a drain follows (an unplayable trailing chunk has no
  // drain signal after it, so there it can stay ahead of what was signalled), and resets at each
  // answer's chunk 0.
  let receivedChunks = 0;
  // The user has paused the answer being spoken: its player holds its place, and so does the
  // gateway's drain. Cleared by anything that ends the answer (see stopPlayback).
  let paused = false;
  // The sections the current answer's audio has been attributed to, in the order they were heard
  // (their gateway tags, which can skip), and which of them the latest chunk was in. Both start
  // over with the answer, at its chunk 0.
  let heardSections: number[] = [];
  let currentOrdinal = -1;
  // The user has muted their microphone: nothing it captures is sent up (see setMicMuted).
  let micMuted = false;
  // The user has muted the answer's speakers (see setMuted). It is the SESSION's, not a player's:
  // each answer has a player of its own, and is made with this applied.
  let muted = false;
  // The current answer's audio is being heard (see onPlayingChange): its player has played a
  // chunk and the answer has not played out or been ended.
  let playing = false;

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

  const setPaused = (next: boolean) => {
    if (paused === next) return;
    paused = next;
    if (!over) args.onPausedChange?.(next);
  };

  const setPlaying = (next: boolean) => {
    if (playing === next) return;
    playing = next;
    if (!over) args.onPlayingChange?.(next);
  };

  // The window of a queue that ran dry in `speaking` (PLAYING_GAP_MS): when it runs out with
  // nothing queued and nothing held by a pause, the audio is over.
  let playingGapTimer: ReturnType<typeof setTimeout> | undefined;
  const clearPlayingGap = () => {
    clearTimeout(playingGapTimer);
    playingGapTimer = undefined;
  };
  const armPlayingGap = () => {
    clearPlayingGap();
    playingGapTimer = setTimeout(() => {
      playingGapTimer = undefined;
      if (playedOut()) setPlaying(false);
    }, PLAYING_GAP_MS);
  };
  // The audio is over now: no window is left to end it again, or to end the next answer's.
  const endPlaying = () => {
    clearPlayingGap();
    setPlaying(false);
  };

  // Silence what is playing and forget the stream it was part of -- and with it a pause: the
  // answer it was a pause of is gone, and the next one starts playing.
  const stopPlayback = () => {
    player?.stop();
    player = null;
    playState = { phase: 'waiting' };
    heardSections = [];
    currentOrdinal = -1;
    setPaused(false);
    endPlaying();
  };

  // Nothing of the answer is left to hear: not held by a pause, and nothing queued. (No player at
  // all -- no audio came -- is nothing to hear as well.)
  function playedOut(): boolean {
    return !paused && (player?.pending ?? 0) === 0;
  }

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

  // One text control frame, when there is a socket to send it on. `extra` carries a control's
  // optional fields (playback_done's played_through counter); the fields stay additive on the wire.
  const sendControl = (
    type: 'push_start' | 'push_end' | 'cancel' | 'playback_done' | 'pause' | 'resume' | 'stop',
    extra?: Record<string, number>,
  ) => {
    if (over || !open || !socket) return;
    socket.send(JSON.stringify({ type, ...extra }));
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
    // A muted microphone sends nothing, and keeps nothing for later either: the batch is simply
    // not for anyone.
    if (micMuted) return;
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

  // The chunk now arriving is of section `section` of the answer. What is reported is the section's
  // place among the ones heard (its `ordinal`) as well as the gateway's tag: the tag is the
  // index the section was launched at and can skip, the ordinal cannot, and the stored turn's
  // blocks are numbered by it. A change is reported once, not on every chunk of a section.
  const attributeSection = (section: number) => {
    let ordinal = heardSections.indexOf(section);
    if (ordinal < 0) ordinal = heardSections.push(section) - 1;
    if (ordinal === currentOrdinal) return;
    currentOrdinal = ordinal;
    args.onAudioSection?.({ section, ordinal });
  };

  // One chunk of the spoken answer to the player of its stream. acceptChunk is the rule of
  // progressive playback: a chunk is played only while the stream is whole (from chunk 0, in
  // order, at one rate), and the first irregularity abandons the rest of this answer's audio --
  // the stored turn has all of it, and a hole in it would be heard.
  const playChunk = (frame: Extract<VoiceDownFrame, { type: 'audio_delta' }>) => {
    if (!context) return;
    // Every answer's audio starts at chunk 0, and a turn that was cancelled sends no end: so a
    // chunk 0 is a new answer, which replaces what is left of the last -- and restarts the playout
    // counter, which numbers from the gateway's own per-turn reset.
    if (frame.seq === 0) {
      stopPlayback();
      receivedChunks = 0;
    }
    // Count every chunk RECEIVED (before the play verdict): this keeps the count aligned with what
    // the gateway relayed when a drain follows, even if a trailing chunk turns out unplayable.
    receivedChunks += 1;
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
      // A mute holds for every answer, not only the one it was asked for during: the new
      // player is made audible, so it is silenced before its first chunk.
      if (muted) player.setMuted(true);
      // The #304 drain hook: when this answer's player has played its queue out, tell the
      // gateway playout is done so it can end its half-duplex drain at true playout-done, not
      // relay-done (spec 6.2). A fresh player is made per answer, so it is wired per player here.
      // onEmpty fires on EVERY queue drain -- including a gap between chunks when TTS lags real
      // time, and while the gateway is still speaking in the concurrent flow. Each signal carries
      // played_through, the count of audio received so far (#322), so the gateway honors only a
      // signal that accounts for everything it relayed as the genuine end of audio, and never ends
      // the drain on a stale gap signal (nor below its server-side floor).
      player.onEmpty = () => {
        sendControl('playback_done', { played_through: receivedChunks });
        // Once the answer has been sent in full (the gateway is draining) a queue that runs dry is
        // the answer played out. Before that it is a gap between chunks of one still coming, or
        // the last section played out while the gateway goes on writing: the next chunk, if
        // there is one, arrives within the window and keeps the audio going.
        if (state === 'draining') endPlaying();
        else if (state === 'speaking') armPlayingGap();
      };
    }
    if (bytes === null || !player.push(bytes, frame.rate)) {
      playState = { phase: 'abandoned', reason: 'format' };
    }
    // Only audio that is being played live belongs to a block: once the stream is abandoned
    // there is nothing live to attribute (the stored turn's audio is what is replayed).
    if (playState.phase !== 'abandoned') {
      attributeSection(frame.section ?? 0);
      // More audio: a window of a dry queue that is open is void.
      clearPlayingGap();
      setPlaying(true);
    }
  };

  const onSocketMessage = (event: MessageEvent) => {
    if (over || typeof event.data !== 'string') return;
    const frame = parseVoiceDownFrame(event.data);
    if (!frame) return;
    switch (frame.type) {
      case 'state':
        // The gateway holds a pause for as long as it lasts, so a state that is not playing the
        // answer out means the pause is void: the answer was ended under it (the microphone is
        // open again), and what the player held must never play into that.
        if (paused && frame.state !== 'speaking' && frame.state !== 'draining') stopPlayback();
        // The answer is only heard in `speaking` and `draining`. Outside those two states there
        // is nothing left to play; in `draining` it is sent in full, so once what played out
        // covers it there is nothing more to hear either -- both cases end the playing window.
        if (
          (frame.state !== 'speaking' && frame.state !== 'draining') ||
          (frame.state === 'draining' && playedOut())
        ) {
          endPlaying();
        }
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
        // One turn failed; the gateway is back at idle and the session goes on. What is still
        // queued to play (a turn that degraded after some of its audio was sent down) is silenced,
        // as a cancel does: a later push_start would otherwise reopen the microphone into it.
        stopPlayback();
        args.onError(frame.message || frame.code || 'the voice turn could not be completed', {
          kind: 'turn',
          ...(frame.code === undefined ? {} : { code: frame.code }),
        });
        break;
    }
  };

  // A disabled track captures silence, so a muted microphone has nothing in the worklet's batch
  // that is filling when the user unmutes (the batches themselves are dropped in deliverAudio).
  const applyMicMute = (media: MediaStream | null) => {
    for (const track of media?.getAudioTracks() ?? []) track.enabled = !micMuted;
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
    // A mute asked for while the microphone was still being set up holds from its first batch.
    applyMicMute(media);
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
        voice: args.voice,
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
    pause() {
      // Only audio that is being heard can be paused: the gateway ignores a pause in any other
      // state (nothing is playing out), and a player paused on this side alone would hold the
      // next answer's first chunk, with the gateway's own drain running.
      if (over || paused || !player || (state !== 'speaking' && state !== 'draining')) return;
      setPaused(true);
      sendControl('pause');
      player.pause();
    },
    resume() {
      if (over || !paused) return;
      setPaused(false);
      // The gateway first: it re-arms its drain from the resume, and the drain signal below only
      // counts after it.
      sendControl('resume');
      if (!player) return;
      player.resume();
      // A pause that lands as the last chunk finishes playing swallows its drain: the player
      // drops a chunk the clock has already carried past, and so has nothing left whose ending
      // would fire `onEmpty` -- which is what ends the gateway's drain at true playout-done. With
      // nothing queued after a resume no drain is coming, so it is reported here; the gateway
      // takes a signal at every drain (and ignores one that is early or does not account for all
      // it relayed), so this is only ever a signal that is true. Without it the mic would stay
      // shut until the gateway's ceiling.
      if (player.pending === 0) {
        sendControl('playback_done', { played_through: receivedChunks });
        // As for a queue that ran dry (onEmpty): in draining it is the end; still speaking, the
        // next section may follow, so the end is only taken once the window has run out.
        if (state === 'draining') endPlaying();
        else if (state === 'speaking') armPlayingGap();
      }
    },
    stopSpeaking() {
      if (over) return;
      stopPlayback();
      sendControl('stop');
    },
    setMuted(next) {
      if (over || muted === next) return;
      muted = next;
      player?.setMuted(next);
    },
    setMicMuted(next) {
      if (over || micMuted === next) return;
      micMuted = next;
      // What was captured for a socket that is not open yet was captured before the mute.
      if (next) pending.length = 0;
      applyMicMute(stream);
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
