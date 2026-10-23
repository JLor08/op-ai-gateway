// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// The store's model of a live transcription session (#266): where it stands and what has been
// transcribed so far, as a pure reducer. The socket, the microphone and the reconnects are
// useLiveTranscription's; this is only the state they drive, kept apart so that the transcript
// rules can be tested without a browser.
//
// The transcript is two texts, because a session can be replaced. `base` is what the sessions
// before the current one said; `current` is what the current one has said so far. A delta
// appends to `current`, and a partial REPLACES `current` (the model revising its hypothesis
// about what it has heard in this session) -- never `base`: after a reconnect the new session
// knows nothing of the earlier audio, so its first partial must not wipe out what the earlier
// session already delivered.

/** Where the session stands, as the composer shows it. */
export type LiveTranscriptionPhase =
  'idle' | 'connecting' | 'listening' | 'reconnecting' | 'finalizing' | 'error';

/** The session as the store exposes it to the composer. */
export type LiveTranscription = {
  state: LiveTranscriptionPhase;
  /** Everything transcribed so far, across reconnects. */
  transcript: string;
  /** The user-facing sentence for the failure, in the `error` state. */
  error?: string;
};

/** What the reducer holds: the exposed state, with the transcript still in its two halves. */
export type LiveSessionModel = {
  state: LiveTranscriptionPhase;
  base: string;
  current: string;
  error?: string;
};

export const IDLE_LIVE_SESSION: LiveSessionModel = { state: 'idle', base: '', current: '' };

export type LiveAction =
  | { type: 'start' }
  | { type: 'listening' }
  | { type: 'finalizing' }
  | { type: 'delta'; text: string }
  | { type: 'partial'; text: string }
  | { type: 'done'; text: string }
  | { type: 'reconnecting' }
  | { type: 'fail'; error: string }
  | { type: 'reset' };

// The phases in which a session is alive: it has a microphone or a socket, or is about to.
const RUNNING: readonly LiveTranscriptionPhase[] = [
  'connecting',
  'listening',
  'reconnecting',
  'finalizing',
];

/** Whether a session is in flight (anything but idle and error). */
export function liveSessionRunning(state: LiveTranscriptionPhase): boolean {
  return RUNNING.includes(state);
}

/**
 * Two stretches of transcript as one: a single space between them unless either side already
 * brings its own whitespace (a delta often starts with the space that separates it from the
 * word before).
 */
export function joinTranscript(first: string, second: string): string {
  if (first === '') return second;
  if (second === '') return first;
  return /\s$/.test(first) || /^\s/.test(second) ? first + second : `${first} ${second}`;
}

/** The whole transcript so far, across reconnects. */
export function liveTranscriptOf(model: LiveSessionModel): string {
  return joinTranscript(model.base, model.current);
}

export function liveSessionReducer(model: LiveSessionModel, action: LiveAction): LiveSessionModel {
  switch (action.type) {
    case 'start':
      return { state: 'connecting', base: '', current: '' };
    case 'listening':
      // Only the end of a connect: not a session that is finalizing, failed or gone.
      return model.state === 'connecting' || model.state === 'reconnecting'
        ? { ...model, state: 'listening' }
        : model;
    case 'finalizing':
      return liveSessionRunning(model.state) && model.state !== 'finalizing'
        ? { ...model, state: 'finalizing' }
        : model;
    case 'delta':
      return liveSessionRunning(model.state)
        ? { ...model, current: model.current + action.text }
        : model;
    case 'partial':
      return liveSessionRunning(model.state) ? { ...model, current: action.text } : model;
    case 'done':
      // The session's final transcript replaces what its deltas and partials gave; one with no
      // text of its own (the gateway's "keep what you have") leaves it as it was.
      return liveSessionRunning(model.state) && action.text !== ''
        ? { ...model, current: action.text }
        : model;
    case 'reconnecting':
      // The dropped session's text is settled into the base: the next session starts its own.
      return liveSessionRunning(model.state)
        ? { state: 'reconnecting', base: liveTranscriptOf(model), current: '' }
        : model;
    case 'fail':
      return { ...model, state: 'error', error: action.error };
    case 'reset':
      return IDLE_LIVE_SESSION;
  }
}
