// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// The live transcription session of the chat store (#266), behind a narrow interface like
// useChatRuns and useChatPersistence: the store hands it the thread it runs in and the way to
// commit a finished transcript, and gets the session's state and its four controls back.
//
// WHAT IT OWNS is the lifetime of a recording, which is longer than one connection. The
// microphone and the socket are createLiveSession's (liveTranscription.ts), and a session of
// that kind is over for good the moment it ends. A RECORDING is what the user started and has
// not stopped, and it can outlive several sessions: when a connection drops while the user is
// still speaking, this starts a NEW session (a new ticket, a new backend session) after a short
// backoff, and carries the transcript so far across (liveSession.ts keeps the earlier text apart
// from the new session's, so that a partial of the new one cannot wipe it out). Audio that
// was already sent is never sent again; what is said while the connection is down is lost, which
// the reconnecting state says.
//
// To tell a drop from an end, it tracks the user's INTENT: `recording` until Stop (or abort)
// is pressed. Only a drop while recording is retried, and only the kinds of failure that a
// repeat can cure (`dropped`): a refusal by the gateway (a busy model, an unsupported one), a
// denied microphone or a browser that cannot capture would only be repeated. The retries are
// bounded, and a session that got somewhere (it transcribed something) gives them back.
//
// WHAT IT COMMITS: a finished recording becomes ONE completed turn of the chat, written
// directly through `commit` (the store appends the messages) and NOT through a chat run: the
// work was done, and billed, by the gateway's live endpoint. When the thread will not take it
// (a run in flight), the transcript stays in the error state, and keepLiveTranscript offers it
// again.

import { useCallback, useEffect, useMemo, useRef, useState, type RefObject } from 'react';
import type { Translation } from '../shared/types';
import {
  LIVE_BUSY_CODE,
  createLiveSession,
  type LiveErrorDetail,
  type LiveSession,
  type LiveSessionState,
  type LiveTicketApi,
} from './liveTranscription';
import {
  IDLE_LIVE_SESSION,
  liveSessionReducer,
  liveSessionRunning,
  liveTranscriptOf,
  type LiveAction,
  type LiveSessionModel,
  type LiveTranscription,
} from './liveSession';

export type { LiveTranscription, LiveTranscriptionPhase } from './liveSession';

// How many times in a row a dropped connection is retried before the recording gives up, and
// the wait before the first retry (it doubles each time: 1 s, 2 s, 4 s).
export const LIVE_RECONNECT_MAX_ATTEMPTS = 3;
export const LIVE_RECONNECT_BASE_DELAY_MS = 1000;

// The gateway's own refusals that mean "live transcription is not available for this model",
// which is the user's cue to use a clip instead (transcription.live_not_streaming: the model is
// not known to stream; _agent_unsupported: its agent cannot; _params_invalid: the session's
// parameters were refused).
const UNSUPPORTED_CODES: ReadonlySet<string> = new Set([
  'transcription.live_not_streaming',
  'transcription.live_agent_unsupported',
  'transcription.live_params_invalid',
]);

/** What the session is allowed to run in: the open thread, as the store reads it now. */
export type LiveThread = {
  chatId: string | null;
  /** The effective model of the thread. */
  model: string;
  /** Whether a live recording may be offered on it at all (a streaming model of a transcription thread). */
  available: boolean;
};

/** What a recording is started for, read when the user starts it. */
export type LiveStart = { chatId: string; model: string; language: string };

export type LiveTranscriptionDeps = {
  apiRef: RefObject<LiveTicketApi>;
  tRef: RefObject<Translation>;
  showErrorRef: RefObject<(message: string) => void>;
  thread: LiveThread;
  /** The thread's answer to Start: what to record for, or null when it cannot (nothing is said). */
  begin: () => LiveStart | null;
  /** Appends the finished transcript to the chat: null once done, or the sentence of why not. */
  commit: (chatId: string, transcript: string) => string | null;
};

export type LiveTranscriptionApi = {
  liveSession: LiveTranscription;
  /** Start a recording. Call it from the user's gesture: the microphone is asked for inside it. */
  startLiveTranscription: () => void;
  /** End the recording the way the user means it: finish, and commit the transcript. */
  stopLiveTranscription: () => void;
  /** Cancel: nothing is committed, and the transcript so far is dropped. */
  abortLiveTranscription: () => void;
  /** After a failure that left a transcript: commit what there is, as a turn of the chat. */
  keepLiveTranscript: () => void;
};

type Intent = 'idle' | 'recording' | 'stopping';

// The user-facing sentence for a failed session: what the user can do about it.
function liveErrorText(message: string, detail: LiveErrorDetail, t: Translation): string {
  const code = detail.code ?? (message.includes(LIVE_BUSY_CODE) ? LIVE_BUSY_CODE : '');
  if (detail.kind === 'denied') return t.chatRecordDenied;
  if (detail.kind === 'unsupported') return t.chatTranscriptionLiveUnsupported;
  if (code === LIVE_BUSY_CODE) return t.chatTranscriptionLiveBusy;
  if (UNSUPPORTED_CODES.has(code)) return t.chatTranscriptionLiveUnsupported;
  return t.chatTranscriptionLiveError;
}

export function useLiveTranscription(deps: LiveTranscriptionDeps): LiveTranscriptionApi {
  // The latest deps, read through a ref so that the callbacks below -- which session callbacks
  // and timers hold on to for as long as a recording lasts -- stay stable and never see stale ones.
  const depsRef = useRef(deps);
  depsRef.current = deps;

  // The reducer's model lives in a ref as well as in state: the session's callbacks arrive
  // between renders, and the final transcript is computed from the model as it is NOW.
  const modelRef = useRef<LiveSessionModel>(IDLE_LIVE_SESSION);
  const [model, setModel] = useState<LiveSessionModel>(IDLE_LIVE_SESSION);
  const apply = useCallback((action: LiveAction) => {
    const next = liveSessionReducer(modelRef.current, action);
    if (next === modelRef.current) return;
    modelRef.current = next;
    setModel(next);
  }, []);

  // The recording: what it runs for, whether the user still means it to go on, the session that
  // is live (null between two sessions), and which session that is (`generation`: every
  // callback of a session that has been replaced, aborted or finished finds it moved on and is
  // ignored). `attempts` counts the retries since the recording last made progress.
  const startRef = useRef<LiveStart | null>(null);
  const intentRef = useRef<Intent>('idle');
  const sessionRef = useRef<LiveSession | null>(null);
  const generationRef = useRef(0);
  const attemptsRef = useRef(0);
  const retryTimerRef = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  // The retry timer starts the next session through this, as openSession cannot name itself.
  const openSessionRef = useRef<() => void>(() => undefined);

  // Lets go of the recording's session and timers; what the model shows is left to the caller.
  const release = useCallback(() => {
    generationRef.current += 1;
    clearTimeout(retryTimerRef.current);
    retryTimerRef.current = undefined;
    const session = sessionRef.current;
    sessionRef.current = null;
    intentRef.current = 'idle';
    session?.abort();
  }, []);

  // Appends the transcript to the chat, and tells the user when that cannot be done. An empty
  // transcript commits nothing; `emptyMessage` (the user's Stop produced no text) says so.
  const settle = useCallback(
    (transcript: string, emptyMessage?: string) => {
      const { commit, showErrorRef } = depsRef.current;
      const chatId = startRef.current?.chatId;
      if (transcript === '' || chatId === undefined) {
        apply({ type: 'reset' });
        if (emptyMessage) showErrorRef.current(emptyMessage);
        return;
      }
      const refusal = commit(chatId, transcript);
      if (refusal === null) apply({ type: 'reset' });
      else apply({ type: 'fail', error: refusal });
    },
    [apply],
  );

  // A session that ended with a final transcript.
  const finishWithDone = useCallback(
    (doneText: string) => {
      apply({ type: 'done', text: doneText });
      const transcript = liveTranscriptOf(modelRef.current).trim();
      release();
      settle(transcript, depsRef.current.tRef.current.errorChatRunNoTranscript);
    },
    [apply, release, settle],
  );

  // The recording ended without a session to finish it (Stop pressed between two sessions, or
  // before the first one had a microphone): what has been transcribed is all there is.
  const finishWithoutSession = useCallback(() => {
    const transcript = liveTranscriptOf(modelRef.current).trim();
    release();
    settle(transcript);
  }, [release, settle]);

  // Starts one session of the recording. It runs synchronously, so that on the user's first
  // Start it is inside their gesture (the microphone and the audio context need that).
  const openSession = useCallback(() => {
    const start = startRef.current;
    if (!start) return;
    generationRef.current += 1;
    const generation = generationRef.current;
    const current = () => generationRef.current === generation;

    // Text is what shows a session works: the retries are given back.
    const progress = (action: LiveAction) => {
      if (!current()) return;
      attemptsRef.current = 0;
      apply(action);
    };

    const onError = (message: string, detail: LiveErrorDetail) => {
      if (!current()) return;
      sessionRef.current = null;
      const retry =
        intentRef.current === 'recording' &&
        detail.kind === 'dropped' &&
        attemptsRef.current < LIVE_RECONNECT_MAX_ATTEMPTS;
      if (retry) {
        attemptsRef.current += 1;
        apply({ type: 'reconnecting' });
        retryTimerRef.current = setTimeout(
          () => {
            retryTimerRef.current = undefined;
            if (intentRef.current === 'recording') openSessionRef.current();
          },
          LIVE_RECONNECT_BASE_DELAY_MS * 2 ** (attemptsRef.current - 1),
        );
        return;
      }
      // Over for good. The transcript so far stays, for keepLiveTranscript.
      intentRef.current = 'idle';
      apply({ type: 'fail', error: liveErrorText(message, detail, depsRef.current.tRef.current) });
    };

    const onState = (state: LiveSessionState) => {
      if (!current()) return;
      switch (state) {
        case 'listening':
          apply({ type: 'listening' });
          break;
        case 'finalizing':
          apply({ type: 'finalizing' });
          break;
        case 'closed':
          // After a `done` (the session is no longer current by then) or an abort, nothing
          // reaches here. A session that closes without either is one Stop ended before it had
          // a microphone: there is no transcript coming from it.
          if (intentRef.current === 'stopping') finishWithoutSession();
          break;
        default:
          // `connecting` is what start (or the reconnect state) already shows, and `error` is
          // onError's, which comes first.
          break;
      }
    };

    const session = createLiveSession({
      api: depsRef.current.apiRef.current,
      model: start.model,
      language: start.language === '' ? undefined : start.language,
      onDelta: (text) => progress({ type: 'delta', text }),
      onPartial: (text) => progress({ type: 'partial', text }),
      onDone: (text) => {
        if (current()) finishWithDone(text);
      },
      onError,
      onState,
    });
    if (current()) sessionRef.current = session;
  }, [apply, finishWithDone, finishWithoutSession]);
  openSessionRef.current = openSession;

  const startLiveTranscription = useCallback(() => {
    if (liveSessionRunning(modelRef.current.state)) return;
    const start = depsRef.current.begin();
    if (!start) return;
    release();
    startRef.current = start;
    intentRef.current = 'recording';
    attemptsRef.current = 0;
    apply({ type: 'start' });
    openSession();
  }, [apply, openSession, release]);

  const stopLiveTranscription = useCallback(() => {
    if (intentRef.current !== 'recording') return;
    intentRef.current = 'stopping';
    clearTimeout(retryTimerRef.current);
    retryTimerRef.current = undefined;
    const session = sessionRef.current;
    if (session) session.stop();
    else finishWithoutSession();
  }, [finishWithoutSession]);

  const abortLiveTranscription = useCallback(() => {
    release();
    startRef.current = null;
    apply({ type: 'reset' });
  }, [apply, release]);

  const keepLiveTranscript = useCallback(() => {
    if (modelRef.current.state !== 'error') return;
    settle(liveTranscriptOf(modelRef.current).trim());
  }, [settle]);

  // A recording belongs to the thread it started in. It does not follow the user to another
  // chat or model, and it ends when live stops being on offer (a model that cannot stream):
  // there is no composer left to show it, and nowhere for its transcript to go.
  const { chatId, model: threadModel, available } = deps.thread;
  useEffect(() => {
    const start = startRef.current;
    if (!start || modelRef.current.state === 'idle') return;
    if (chatId !== start.chatId || threadModel !== start.model || !available) {
      abortLiveTranscription();
    }
  }, [chatId, threadModel, available, abortLiveTranscription]);

  // The provider going away (logout) releases the microphone and the connection.
  useEffect(() => release, [release]);

  const liveSession = useMemo<LiveTranscription>(
    () => ({
      state: model.state,
      transcript: liveTranscriptOf(model),
      ...(model.error === undefined ? {} : { error: model.error }),
    }),
    [model],
  );

  return {
    liveSession,
    startLiveTranscription,
    stopLiveTranscription,
    abortLiveTranscription,
    keepLiveTranscript,
  };
}
