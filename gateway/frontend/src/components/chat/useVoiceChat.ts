// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// The Voice Chat conversation of the chat store (issue #312), behind a narrow interface like
// useChatRuns and useLiveTranscription: the store hands it the chat to talk in and the way to
// reload that chat's thread, and gets the conversation's state, what is being said and answered,
// and its controls back.
//
// WHAT IT OWNS is the lifetime of the conversation: ONE session (createVoiceSession, in
// voiceSession.ts -- the microphone, the connection, the speakers) for MANY turns. The gateway
// decides when a turn starts and ends, and the session reports it, so this does not start a
// session per turn; it starts one when the user turns Voice Chat on and lets go of it when they
// turn it off, when another chat is opened, when it fails for good, or when the provider goes
// away. Every callback of a session that has been let go of finds the generation moved on, and is
// ignored.
//
// WHAT IT DOES NOT DO is persist anything. The gateway commits a turn -- the user's words and the
// answer, text and stored audio, together -- once, when the turn is really over (a pause that
// the user does not continue is not the end: they may be drawing breath). A commit written from
// here as well would be saved twice, so there is no `commit` among its deps, and what it shows
// of a turn in flight is PROVISIONAL, a preview that the stored thread then replaces:
//
//   - `liveTranscript` is what the user has said so far. An `asr_partial` is the provisional
//     text of the CURRENT CHUNK only, and replaces the last partial; across a pause the gateway
//     ends the chunk (`asr_final`) and the next one starts from nothing. So the finals are
//     accumulated here, and what is shown is those plus the current partial. (The partial does
//     not replace the whole transcript: that would drop everything said before the pause.)
//   - `answerPreview` is the answer as it is written (`answer_delta` appends). `answer_done`
//     is provisional too -- it fires before the commit, and a later `answer_reset` can void it
//     -- and the preview already holds all of the text, so it changes nothing. `answer_reset`
//     (the user went on talking) drops the preview, not the transcript.
//   - `audio_done` is the turn committed. The thread is reloaded from the gateway
//     (`refreshThread`), which is how the stored user and assistant turns reach the chat (no
//     frame carries a message id to fetch them by), and the preview is dropped for the next turn.
//     A turn error reloads it as well: a turn can degrade after its commit (#322), and the error
//     frame is then the only signal that the stored turn is there to be shown.
//
// The answer being spoken has controls of its own, which are NOT the conversation's: pause and
// resume hold and continue it, and stopSpeaking cuts it short (the gateway goes back to listening
// and the conversation goes on), where `stop` ends the whole conversation. Its speakers have a
// mute (`muted`), and the microphone has one of its own (`micMuted`): both the user's alone -- the
// gateway is told nothing, the one plays nothing and the other hears nothing -- and each lasts as
// long as the conversation does (the speakers' also over every answer in it). `paused` and
// `audioPlaying` are the session's to say, not this hook's: a pause that the answer ending clears
// must not stay on screen into the next one, and the controls are shown only for audio that is
// being heard.
//
// Errors are the session's: a TURN that failed (a model errored, an admission was refused) leaves
// the gateway at `idle` with the conversation still open, so it is surfaced, the preview is
// dropped, and the user re-engages with pushStart; every other kind ends the conversation, and
// it is torn down with the error kept for the component to explain. The hook has no user-facing
// text: it reports the kind and the gateway's code, and the component says what to do about it.

import {
  useCallback,
  useEffect,
  useMemo,
  useReducer,
  useRef,
  useState,
  type RefObject,
} from 'react';
import { joinTranscript } from './liveSession';
import { speechPlayback, type SpeechPlayback } from './speechPlayback';
import {
  createVoiceSession,
  type VoiceAudioSection,
  type VoiceErrorDetail,
  type VoiceServerState,
  type VoiceSession,
  type VoiceTicketApi,
} from './voiceSession';

/**
 * Where the conversation stands: `off` until it is started and after it has ended, `connecting`
 * until the gateway reports its own state (the microphone is asked for, the models are loaded
 * and the connection is opened), and then the gateway's state machine -- only in `listening` is
 * the user heard.
 */
export type VoiceChatState = 'off' | 'connecting' | VoiceServerState;

/** What a conversation is started for, read when the user starts it. */
export type VoiceChatStart = {
  chatId: string;
  /** The gateway models of the loop: the live transcription, the chat and the speech model. */
  asrModel: string;
  llmModel: string;
  ttsModel: string;
  /** The voice the speech model answers in; '' is the model's own default voice. */
  voice: string;
};

/**
 * A failure: `kind: 'turn'` is one turn that failed and the conversation is still open (the state
 * is `idle`); every other kind has ended it (the state is `off`). `code` is the gateway's stable
 * error code, when there is one; `message` is diagnostic text, not for the user.
 */
export type VoiceChatError = VoiceErrorDetail & { message: string };

export type VoiceChatDeps = {
  apiRef: RefObject<VoiceTicketApi>;
  /** The open chat: a conversation belongs to the chat it started in, and ends if another opens. */
  activeChatId: string | null;
  /** The chat's answer to start: what to talk for, or null when it cannot (nothing is said). */
  begin: () => VoiceChatStart | null;
  /**
   * Reloads the chat's thread from the gateway, which has committed the turn that has just ended:
   * the stored user and assistant turns replace what was shown of it. It reports its own failure
   * (the hook ignores it), and it is called for every turn -- at its end (`audio_done`) and when
   * it fails and the conversation goes on, which it may do after the commit -- so it may be
   * called while the last reload is still on its way, and when nothing was committed.
   */
  refreshThread: (chatId: string) => void | Promise<void>;
  /** The page's speech playback, readied inside the user's gesture (a stand-in in tests). */
  playback?: Pick<SpeechPlayback, 'prime'>;
};

export type VoiceChatApi = {
  state: VoiceChatState;
  /** What the user has said so far this turn (provisional): finished chunks and the current one. */
  liveTranscript: string;
  /** The answer so far (provisional): empty until the answer starts, and after it is void. */
  answerPreview: string;
  /** The failure that last ended, or interrupted, the conversation. */
  error: VoiceChatError | null;
  /** The spoken answer is paused (by `pause`, until `resume`, a stop, or the answer ending). */
  paused: boolean;
  /** The user's own microphone is muted (by `toggleMicMute`): the gateway hears nothing, so no turn. */
  micMuted: boolean;
  /** The spoken answer is muted (by `toggleMute`): it plays on, silently, and so does each next one. */
  muted: boolean;
  /**
   * The answer's audio is being heard: it has started playing and has not played out or been
   * ended. A pause is still being heard (the held audio is yet to be). The controls of the
   * playback are for this, and for nothing else -- not before the first chunk, nor in the
   * gateway's tail after the audio is over, where a pause would do nothing.
   */
  audioPlaying: boolean;
  /**
   * The section of the spoken answer whose audio is arriving, or null when there is none (see
   * VoiceAudioSection: its `ordinal` is its block's place in the stored turn, whatever gaps the
   * gateway's own `section` indices have). It is the section being received, which can run ahead
   * of what is audible.
   */
  audioSection: VoiceAudioSection | null;
  /** Turn Voice Chat on. Call it from the user's gesture: the microphone is asked for inside it. */
  start: () => void;
  /** Turn it off: the conversation ends, and an error that was shown is dismissed. */
  stop: () => void;
  /** The user starts talking (push-to-talk), which also re-engages after a failed turn. */
  pushStart: () => void;
  /** The user is done talking: the turn ends now, without waiting for a pause. */
  pushEnd: () => void;
  /** Abandon the turn in progress: its answer is silenced and listening resumes. */
  cancel: () => void;
  /** Pause the spoken answer: a no-op unless it is being heard (`paused` says whether it took). */
  pause: () => void;
  /** Continue a paused answer from where it stopped. */
  resume: () => void;
  /**
   * Cut the spoken answer short and let the user talk: the gateway drops the rest of it and goes
   * back to listening, and the conversation goes on. (`stop` is the end of the conversation.)
   */
  stopSpeaking: () => void;
  /** Mute the user's microphone, or unmute it: the conversation stays on, but no turn is heard. */
  toggleMicMute: () => void;
  /** Mute the spoken answer, or unmute it: it keeps playing, silently, and so do the next ones. */
  toggleMute: () => void;
};

// The turn in flight, as shown: the finished chunks, the chunk being said, the answer, and the
// section of it whose audio is arriving.
type VoiceTurn = {
  pending: string;
  partial: string;
  answer: string;
  audioSection: VoiceAudioSection | null;
};

const NO_TURN: VoiceTurn = { pending: '', partial: '', answer: '', audioSection: null };

type TurnAction =
  | { type: 'partial'; text: string }
  | { type: 'final'; text: string }
  | { type: 'delta'; text: string }
  | { type: 'answerReset' }
  | { type: 'audioSection'; attribution: VoiceAudioSection }
  | { type: 'clear' };

function turnReducer(turn: VoiceTurn, action: TurnAction): VoiceTurn {
  switch (action.type) {
    case 'partial':
      return action.text === turn.partial ? turn : { ...turn, partial: action.text };
    case 'final':
      // The chunk is finished: it joins what was said before it, and its partial is spent.
      return {
        ...turn,
        pending: joinTranscript(turn.pending, action.text),
        partial: '',
      };
    case 'delta':
      return action.text === '' ? turn : { ...turn, answer: turn.answer + action.text };
    case 'answerReset':
      return turn.answer === '' ? turn : { ...turn, answer: '' };
    case 'audioSection': {
      const { section, ordinal } = action.attribution;
      return turn.audioSection?.section === section && turn.audioSection.ordinal === ordinal
        ? turn
        : { ...turn, audioSection: action.attribution };
    }
    case 'clear':
      return turn.pending === '' &&
        turn.partial === '' &&
        turn.answer === '' &&
        turn.audioSection === null
        ? turn
        : NO_TURN;
  }
}

export function useVoiceChat(deps: VoiceChatDeps): VoiceChatApi {
  // The latest deps, read through a ref so that the callbacks below -- which the session holds on
  // to for as long as the conversation lasts -- stay stable and never see stale ones.
  const depsRef = useRef(deps);
  depsRef.current = deps;

  const [state, setState] = useState<VoiceChatState>('off');
  const [turn, dispatch] = useReducer(turnReducer, NO_TURN);
  const [error, setError] = useState<VoiceChatError | null>(null);
  const [paused, setPaused] = useState(false);
  const [micMuted, setMicMuted] = useState(false);
  const [muted, setMuted] = useState(false);
  const [audioPlaying, setAudioPlaying] = useState(false);
  // What the render last saw of them, for the controls below (which are stable, and are called
  // between renders): the gateway's state, and the mutes that the next toggles reverse.
  const stateRef = useRef<VoiceChatState>('off');
  stateRef.current = state;
  const micMutedRef = useRef(false);
  const mutedRef = useRef(false);

  // The conversation's session (null when there is none), and which session that is
  // (`generation`: every callback of a session that was let go of finds it moved on, and is
  // ignored). `chatRef` is the chat it was started for.
  const sessionRef = useRef<VoiceSession | null>(null);
  const generationRef = useRef(0);
  const chatRef = useRef<string | null>(null);

  // Lets go of the session, with everything it holds (the microphone, the connection, the
  // speakers). Idempotent, and safe on a session that has already ended itself.
  const release = useCallback(() => {
    generationRef.current += 1;
    const session = sessionRef.current;
    sessionRef.current = null;
    session?.stop();
  }, []);

  // The controls' own state starts over with each conversation: a pause belongs to the answer it
  // paused, and the microphone and the speakers of a new conversation are live (a mute is not
  // carried into a conversation the user starts afresh).
  const resetControls = useCallback(() => {
    micMutedRef.current = false;
    mutedRef.current = false;
    setPaused(false);
    setMicMuted(false);
    setMuted(false);
    setAudioPlaying(false);
  }, []);

  // The conversation is over: nothing of it is shown any more, `failure` is why (null for the
  // user's own end).
  const end = useCallback(
    (failure: VoiceChatError | null) => {
      release();
      dispatch({ type: 'clear' });
      resetControls();
      setState('off');
      setError(failure);
    },
    [release, resetControls],
  );

  // The gateway has committed the turn: reload the chat's thread, so that the stored turns are
  // what the chat shows. A refresh that fails reports itself.
  const refreshThread = useCallback((chatId: string) => {
    let reloading: void | Promise<void>;
    try {
      reloading = depsRef.current.refreshThread(chatId);
    } catch {
      return;
    }
    void Promise.resolve(reloading).catch(() => undefined);
  }, []);

  const start = useCallback(() => {
    if (sessionRef.current) return;
    const target = depsRef.current.begin();
    if (!target) return;
    // The page's audio is readied from inside the user's gesture, like every other entry point
    // of speech (ChatStore's primeSpeechPlayback).
    (depsRef.current.playback ?? speechPlayback).prime();
    generationRef.current += 1;
    const generation = generationRef.current;
    const current = () => generationRef.current === generation;
    chatRef.current = target.chatId;
    dispatch({ type: 'clear' });
    resetControls();
    setError(null);
    setState('connecting');

    const session = createVoiceSession({
      api: depsRef.current.apiRef.current,
      chatId: target.chatId,
      asrModel: target.asrModel,
      llmModel: target.llmModel,
      ttsModel: target.ttsModel,
      voice: target.voice,
      onState: (next) => {
        if (!current()) return;
        // A session that ended without saying why (a failure says it, through onError, first and
        // is then no longer current): there is nobody left to hear.
        if (next === 'closed' || next === 'error') end(null);
        else setState(next);
      },
      onTranscript: ({ text, final }) => {
        if (current()) dispatch({ type: final ? 'final' : 'partial', text });
      },
      onAnswer: ({ text, done }) => {
        // `done` is provisional and the preview already holds the whole answer: nothing to do.
        if (current() && !done) dispatch({ type: 'delta', text });
      },
      onAnswerReset: () => {
        if (current()) dispatch({ type: 'answerReset' });
      },
      onAudioDone: () => {
        if (!current()) return;
        dispatch({ type: 'clear' });
        refreshThread(target.chatId);
      },
      onPausedChange: (next) => {
        if (current()) setPaused(next);
      },
      onAudioSection: (attribution) => {
        if (current()) dispatch({ type: 'audioSection', attribution });
      },
      onPlayingChange: (next) => {
        if (current()) setAudioPlaying(next);
      },
      onError: (message, detail) => {
        if (!current()) return;
        const failure: VoiceChatError = { ...detail, message };
        if (detail.kind === 'turn') {
          // The gateway has put the conversation back to idle, and it goes on.
          dispatch({ type: 'clear' });
          setError(failure);
          setState('idle');
          // The turn may have failed AFTER the gateway committed it (the speech degraded once the
          // answer was stored), and then this frame, not an audio_done, is all this tab is told:
          // reload the thread as the end of a turn does, or the committed turn shows only when
          // the chat is next opened. Harmless when nothing was committed. It does not touch the
          // error just set (the reload reports its own failure as a toast of the store's).
          refreshThread(target.chatId);
          return;
        }
        // No reload here: this ends the conversation, and the store reloads the thread itself
        // when a conversation ends in the middle of a turn (a second one would double the report
        // of a failed reload).
        end(failure);
      },
    });
    if (current()) sessionRef.current = session;
  }, [end, refreshThread, resetControls]);

  const stop = useCallback(() => end(null), [end]);

  const pushStart = useCallback(() => {
    if (!sessionRef.current) return;
    // Re-engaging after a failed turn: the failure has been seen.
    setError(null);
    sessionRef.current.pushStart();
  }, []);

  const pushEnd = useCallback(() => {
    sessionRef.current?.pushEnd();
  }, []);

  const cancel = useCallback(() => {
    if (!sessionRef.current) return;
    // The gateway drops the turn's pending text and run without a frame of its own to say so.
    dispatch({ type: 'clear' });
    sessionRef.current.cancel();
  }, []);

  // `paused` is not set here: the session says whether the pause took (it drops one when nothing
  // is being heard), and clears it when the answer ends.
  const pause = useCallback(() => {
    sessionRef.current?.pause();
  }, []);

  const resume = useCallback(() => {
    sessionRef.current?.resume();
  }, []);

  const stopSpeaking = useCallback(() => {
    const session = sessionRef.current;
    if (!session) return;
    const cutShort = stateRef.current === 'speaking';
    session.stopSpeaking();
    // An answer cut short before it ended has no `audio_done` to say what became of its turn: the
    // gateway either cancels it (nothing is stored) or lets the commit it already won finish, and
    // sends nothing either way. What was shown of it is void, and the thread is reloaded as it
    // is for a turn that failed, so that a stored turn shows. (Once the turn is committed -- the
    // answer is only draining -- audio_done has done both, and a user who is already talking
    // keeps their transcript.)
    if (!cutShort) return;
    dispatch({ type: 'clear' });
    const chatId = chatRef.current;
    if (chatId !== null) refreshThread(chatId);
  }, [refreshThread]);

  const toggleMicMute = useCallback(() => {
    const session = sessionRef.current;
    if (!session) return;
    const next = !micMutedRef.current;
    micMutedRef.current = next;
    setMicMuted(next);
    session.setMicMuted(next);
  }, []);

  const toggleMute = useCallback(() => {
    const session = sessionRef.current;
    if (!session) return;
    const next = !mutedRef.current;
    mutedRef.current = next;
    setMuted(next);
    session.setMuted(next);
  }, []);

  // A conversation belongs to the chat it started in: it does not follow the user to another
  // chat (with the microphone still open), and has nowhere to go when no chat is open.
  const { activeChatId } = deps;
  useEffect(() => {
    if (sessionRef.current && chatRef.current !== activeChatId) stop();
  }, [activeChatId, stop]);

  // The provider going away (logout) releases the microphone and the connection.
  useEffect(() => release, [release]);

  return useMemo(
    () => ({
      state,
      liveTranscript: joinTranscript(turn.pending, turn.partial),
      answerPreview: turn.answer,
      error,
      paused,
      micMuted,
      muted,
      audioPlaying,
      audioSection: turn.audioSection,
      start,
      stop,
      pushStart,
      pushEnd,
      cancel,
      pause,
      resume,
      stopSpeaking,
      toggleMicMute,
      toggleMute,
    }),
    [
      state,
      turn,
      error,
      paused,
      micMuted,
      muted,
      audioPlaying,
      start,
      stop,
      pushStart,
      pushEnd,
      cancel,
      pause,
      resume,
      stopSpeaking,
      toggleMicMute,
      toggleMute,
    ],
  );
}
