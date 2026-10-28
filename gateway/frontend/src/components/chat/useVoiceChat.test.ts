// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { act, cleanup, renderHook } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi, type Mock } from 'vitest';
import type {
  VoiceAudioSection,
  VoiceErrorDetail,
  VoiceSessionArgs,
  VoiceSessionState,
  VoiceTicketApi,
} from './voiceSession';
import { createVoiceSession } from './voiceSession';
import { useVoiceChat, type VoiceChatStart } from './useVoiceChat';

// The real session is the browser's microphone, a WebSocket and the speakers (voiceSession.test.ts
// has a fake browser for those). Here it is a recording stand-in, so that these tests are about
// what the HOOK does with a session: the provisional display it builds from the frames, what it
// does at the turn's commit, how it classifies a failure, and how it lets go.
vi.mock('./voiceSession', () => ({ createVoiceSession: vi.fn() }));

type FakeSession = {
  args: VoiceSessionArgs;
  pushStart: Mock<() => void>;
  pushEnd: Mock<() => void>;
  cancel: Mock<() => void>;
  pause: Mock<() => void>;
  resume: Mock<() => void>;
  stopSpeaking: Mock<() => void>;
  setMicMuted: Mock<(muted: boolean) => void>;
  setMuted: Mock<(muted: boolean) => void>;
  stop: Mock<() => void>;
};

let sessions: FakeSession[];

// What the real session does at each point, as far as the hook can see it. A terminal failure is
// onError followed by the final `error` state, which is the order createVoiceSession delivers.
const emit = {
  state: (s: FakeSession, state: VoiceSessionState) => act(() => s.args.onState(state)),
  partial: (s: FakeSession, text: string) => act(() => s.args.onTranscript({ text, final: false })),
  final: (s: FakeSession, text: string) => act(() => s.args.onTranscript({ text, final: true })),
  delta: (s: FakeSession, text: string) => act(() => s.args.onAnswer({ text, done: false })),
  answerDone: (s: FakeSession) => act(() => s.args.onAnswer({ text: '', done: true })),
  answerReset: (s: FakeSession) => act(() => s.args.onAnswerReset()),
  audioDone: (s: FakeSession) => act(() => s.args.onAudioDone()),
  paused: (s: FakeSession, paused: boolean) => act(() => s.args.onPausedChange?.(paused)),
  audioSection: (s: FakeSession, attribution: VoiceAudioSection) =>
    act(() => s.args.onAudioSection?.(attribution)),
  playing: (s: FakeSession, playing: boolean) => act(() => s.args.onPlayingChange?.(playing)),
  turnError: (s: FakeSession, code?: string, message = 'the turn failed') =>
    act(() => s.args.onError(message, { kind: 'turn', ...(code === undefined ? {} : { code }) })),
  failed: (s: FakeSession, detail: VoiceErrorDetail, message = 'the session failed') =>
    act(() => {
      s.args.onError(message, detail);
      s.args.onState('error');
    }),
};

const START: VoiceChatStart = {
  chatId: 'c1',
  asrModel: 'asr',
  llmModel: 'llm',
  ttsModel: 'tts',
  voice: 'alba',
};

beforeEach(() => {
  sessions = [];
  vi.mocked(createVoiceSession).mockImplementation((args) => {
    const session: FakeSession = {
      args,
      pushStart: vi.fn(),
      pushEnd: vi.fn(),
      cancel: vi.fn(),
      pause: vi.fn(),
      resume: vi.fn(),
      stopSpeaking: vi.fn(),
      setMicMuted: vi.fn(),
      setMuted: vi.fn(),
      stop: vi.fn(),
    };
    sessions.push(session);
    // `connecting` is the one state reported before createVoiceSession returns.
    args.onState('connecting');
    return session;
  });
});

afterEach(() => {
  cleanup();
  vi.mocked(createVoiceSession).mockReset();
});

function setup(
  options: {
    begin?: () => VoiceChatStart | null;
    refreshThread?: (chatId: string) => void | Promise<void>;
    activeChatId?: string | null;
  } = {},
) {
  const begin = vi.fn(options.begin ?? (() => START));
  const refreshThread = vi.fn(options.refreshThread ?? (() => undefined));
  const prime = vi.fn();
  const api: VoiceTicketApi = { createVoiceChatTicket: vi.fn() };
  const view = renderHook(
    ({ activeChatId }: { activeChatId: string | null }) =>
      useVoiceChat({
        apiRef: { current: api },
        activeChatId,
        begin,
        refreshThread,
        playback: { prime },
      }),
    {
      initialProps: {
        activeChatId: options.activeChatId === undefined ? 'c1' : options.activeChatId,
      },
    },
  );
  return {
    ...view,
    begin,
    refreshThread,
    prime,
    api,
    voice: () => view.result.current,
    start: () => act(() => view.result.current.start()),
    stop: () => act(() => view.result.current.stop()),
    pushStart: () => act(() => view.result.current.pushStart()),
    pushEnd: () => act(() => view.result.current.pushEnd()),
    cancel: () => act(() => view.result.current.cancel()),
    pause: () => act(() => view.result.current.pause()),
    resume: () => act(() => view.result.current.resume()),
    stopSpeaking: () => act(() => view.result.current.stopSpeaking()),
    toggleMicMute: () => act(() => view.result.current.toggleMicMute()),
    toggleMute: () => act(() => view.result.current.toggleMute()),
  };
}

// A conversation that is up and listening: the usual starting point of a turn.
function listening(h: ReturnType<typeof setup>): FakeSession {
  h.start();
  const session = sessions[0];
  emit.state(session, 'listening');
  return session;
}

describe('useVoiceChat', () => {
  describe('starting', () => {
    it('start opens a session for the chat, its three models and the voice, and reflects its state', () => {
      const h = setup();
      expect(h.voice().state).toBe('off');

      h.start();

      // Synchronously: this is what keeps the microphone request inside the user's gesture.
      expect(sessions).toHaveLength(1);
      expect(sessions[0].args).toMatchObject({
        chatId: 'c1',
        asrModel: 'asr',
        llmModel: 'llm',
        ttsModel: 'tts',
        voice: 'alba',
        api: h.api,
      });
      expect(h.voice().state).toBe('connecting');

      // The gateway's own state machine, one state at a time, drives the indicator.
      for (const state of ['listening', 'thinking', 'speaking', 'draining', 'idle'] as const) {
        emit.state(sessions[0], state);
        expect(h.voice().state).toBe(state);
      }
      expect(h.voice().error).toBeNull();
    });

    it('primes the speech playback inside the call, before the session is made', () => {
      const h = setup();
      h.start();

      expect(h.prime).toHaveBeenCalledTimes(1);
      expect(h.prime.mock.invocationCallOrder[0]).toBeLessThan(
        vi.mocked(createVoiceSession).mock.invocationCallOrder[0],
      );
    });

    it('does not start when the chat cannot (begin says no), and primes nothing', () => {
      const h = setup({ begin: () => null });
      h.start();

      expect(sessions).toHaveLength(0);
      expect(h.prime).not.toHaveBeenCalled();
      expect(h.voice().state).toBe('off');
    });

    it('does not start a second session while one is running', () => {
      const h = setup();
      h.start();
      h.start();

      expect(sessions).toHaveLength(1);
      expect(h.begin).toHaveBeenCalledTimes(1);
      expect(h.prime).toHaveBeenCalledTimes(1);
    });

    it('starts from a clean slate', () => {
      const h = setup();
      const first = listening(h);
      emit.partial(first, 'left over');
      emit.failed(first, { kind: 'dropped' });

      h.start();

      expect(sessions).toHaveLength(2);
      expect(h.voice()).toMatchObject({
        state: 'connecting',
        liveTranscript: '',
        answerPreview: '',
        error: null,
        paused: false,
        micMuted: false,
        muted: false,
        audioPlaying: false,
        audioSection: null,
      });
    });
  });

  describe('the provisional display: the backend persists, the hook only shows', () => {
    it('asr_partial and asr_final update liveTranscript WITHOUT committing a chat message', () => {
      const h = setup();
      const session = listening(h);

      emit.partial(session, 'hello wor');
      expect(h.voice().liveTranscript).toBe('hello wor');
      emit.partial(session, 'hello world');
      expect(h.voice().liveTranscript).toBe('hello world');
      emit.final(session, 'Hello world.');
      expect(h.voice().liveTranscript).toBe('Hello world.');

      // Nothing is written to the thread: the gateway commits the turn, and the hook has no way
      // to commit one (a local commit would be persisted twice).
      expect(h.refreshThread).not.toHaveBeenCalled();
    });

    it('pendingText accumulates across chunks: finals are kept, a partial is only the current chunk', () => {
      const h = setup();
      const session = listening(h);

      // Chunk 1: the user pauses, and the chunk ends.
      emit.partial(session, 'What is');
      emit.partial(session, 'What is the capital');
      emit.final(session, 'What is the capital of France?');
      expect(h.voice().liveTranscript).toBe('What is the capital of France?');

      // Chunk 2 (the user went on talking): its partial is shown after chunk 1, not instead of it.
      emit.partial(session, 'and');
      expect(h.voice().liveTranscript).toBe('What is the capital of France? and');
      emit.partial(session, 'and its population');
      expect(h.voice().liveTranscript).toBe('What is the capital of France? and its population');
      emit.final(session, 'and its population?');
      expect(h.voice().liveTranscript).toBe('What is the capital of France? and its population?');

      // Chunk 3.
      emit.partial(session, 'thanks');
      expect(h.voice().liveTranscript).toBe(
        'What is the capital of France? and its population? thanks',
      );
    });

    it('joins chunks with one space, unless one of them brings its own', () => {
      const h = setup();
      const session = listening(h);

      emit.final(session, 'First chunk.');
      emit.final(session, ' Second chunk.');
      expect(h.voice().liveTranscript).toBe('First chunk. Second chunk.');
      emit.partial(session, 'third');
      expect(h.voice().liveTranscript).toBe('First chunk. Second chunk. third');
    });

    it('an empty chunk adds nothing, and clears the partial it replaces', () => {
      const h = setup();
      const session = listening(h);
      emit.final(session, 'Kept.');
      emit.partial(session, 'uh');

      emit.final(session, '');

      expect(h.voice().liveTranscript).toBe('Kept.');
    });

    it('answer_delta appends to answerPreview, and answer_done keeps it (it is provisional)', () => {
      const h = setup();
      const session = listening(h);
      emit.state(session, 'thinking');

      emit.delta(session, 'Paris ');
      emit.delta(session, 'is the capital.');
      expect(h.voice().answerPreview).toBe('Paris is the capital.');

      // answer_done fires before the commit and a later answer_reset can still void it: the
      // preview is left as it is, neither cleared nor treated as persisted.
      emit.answerDone(session);
      expect(h.voice().answerPreview).toBe('Paris is the capital.');
      expect(h.refreshThread).not.toHaveBeenCalled();
    });

    it('answer_reset clears answerPreview, keeps the transcript, and a new answer starts from nothing', () => {
      const h = setup();
      const session = listening(h);
      emit.final(session, 'What is the capital of France?');
      emit.state(session, 'thinking');
      emit.delta(session, 'Paris is the capital.');
      emit.answerDone(session);

      // The user went on talking: the speculative answer is void, the words so far are not.
      emit.answerReset(session);

      expect(h.voice().answerPreview).toBe('');
      expect(h.voice().liveTranscript).toBe('What is the capital of France?');
      emit.delta(session, 'France');
      expect(h.voice().answerPreview).toBe('France');
    });
  });

  describe('the turn is committed (audio_done)', () => {
    it('audio_done triggers a chat-thread refresh, so the persisted turns appear', () => {
      const h = setup();
      const session = listening(h);
      emit.final(session, 'Hello.');
      emit.delta(session, 'Hi there.');
      expect(h.refreshThread).not.toHaveBeenCalled();

      emit.audioDone(session);

      expect(h.refreshThread).toHaveBeenCalledTimes(1);
      expect(h.refreshThread).toHaveBeenCalledWith('c1');
    });

    it('audio_done clears the provisional display for the next turn', () => {
      const h = setup();
      const session = listening(h);
      emit.final(session, 'Hello.');
      emit.partial(session, 'and');
      emit.delta(session, 'Hi there.');

      emit.audioDone(session);

      expect(h.voice()).toMatchObject({ liveTranscript: '', answerPreview: '' });
      // The next turn builds on nothing of the last.
      emit.partial(session, 'Next question');
      expect(h.voice().liveTranscript).toBe('Next question');
    });

    it('keeps the session: the conversation goes on with the next turn', () => {
      const h = setup();
      const session = listening(h);
      emit.audioDone(session);
      emit.state(session, 'draining');
      emit.state(session, 'listening');

      emit.audioDone(session);

      expect(session.stop).not.toHaveBeenCalled();
      expect(sessions).toHaveLength(1);
      expect(h.voice().state).toBe('listening');
      expect(h.refreshThread).toHaveBeenCalledTimes(2);
    });

    it('refreshes the chat the conversation started in', () => {
      const h = setup({ begin: () => ({ ...START, chatId: 'c-voice' }), activeChatId: 'c-voice' });
      const session = listening(h);

      emit.audioDone(session);

      expect(h.refreshThread).toHaveBeenCalledWith('c-voice');
    });

    it.each([
      ['rejects', () => Promise.reject(new Error('offline'))],
      [
        'throws',
        () => {
          throw new Error('broken');
        },
      ],
    ])('a refresh that %s does not break the conversation', async (_name, refreshThread) => {
      const h = setup({ refreshThread });
      const session = listening(h);

      emit.audioDone(session);
      await act(async () => undefined);

      expect(h.voice().state).toBe('listening');
      emit.partial(session, 'still here');
      expect(h.voice().liveTranscript).toBe('still here');
    });
  });

  describe('the controls', () => {
    it('pushStart, pushEnd and cancel go to the session', () => {
      const h = setup();
      const session = listening(h);

      h.pushStart();
      h.pushEnd();
      h.cancel();

      expect(session.pushStart).toHaveBeenCalledTimes(1);
      expect(session.pushEnd).toHaveBeenCalledTimes(1);
      expect(session.cancel).toHaveBeenCalledTimes(1);
    });

    it('cancel abandons the turn: its provisional transcript and answer are dropped too', () => {
      const h = setup();
      const session = listening(h);
      emit.final(session, 'Never mind.');
      emit.delta(session, 'Well,');

      h.cancel();

      // The gateway drops the pending text and the run without a frame of its own to say so.
      expect(h.voice()).toMatchObject({ liveTranscript: '', answerPreview: '' });
    });

    it('do nothing without a session', () => {
      const h = setup();

      h.pushStart();
      h.pushEnd();
      h.cancel();

      expect(sessions).toHaveLength(0);
      expect(h.voice().state).toBe('off');
    });
  });

  describe('playback controls: pause, resume and stopping the spoken answer', () => {
    it('start with nothing paused, muted or playing', () => {
      const h = setup();
      expect(h.voice()).toMatchObject({
        paused: false,
        micMuted: false,
        muted: false,
        audioPlaying: false,
        audioSection: null,
      });
    });

    it('pause and resume go to the session', () => {
      const h = setup();
      const session = listening(h);
      emit.state(session, 'speaking');

      h.pause();
      h.resume();

      expect(session.pause).toHaveBeenCalledTimes(1);
      expect(session.resume).toHaveBeenCalledTimes(1);
    });

    it('paused is what the session reports: set by a pause, cleared by whatever ends it', () => {
      const h = setup();
      const session = listening(h);
      emit.state(session, 'speaking');

      emit.paused(session, true);
      expect(h.voice().paused).toBe(true);

      emit.paused(session, false);
      expect(h.voice().paused).toBe(false);
    });

    it('does not claim a pause the session has not confirmed (the session drops a pause when nothing plays)', () => {
      const h = setup();
      const session = listening(h);

      h.pause();

      expect(session.pause).toHaveBeenCalledTimes(1);
      expect(h.voice().paused).toBe(false);
    });

    it('stopSpeaking tells the session to cut the answer short, and the conversation goes on', () => {
      const h = setup();
      const session = listening(h);
      emit.state(session, 'speaking');

      h.stopSpeaking();

      expect(session.stopSpeaking).toHaveBeenCalledTimes(1);
      // Not the hook's own stop: the conversation is not torn down.
      expect(session.stop).not.toHaveBeenCalled();
      expect(h.voice().state).toBe('speaking'); // the gateway says when it is listening again
    });

    it('stopSpeaking drops what was shown of the turn that was cut short, and reloads the thread', () => {
      const h = setup();
      const session = listening(h);
      emit.final(session, 'Tell me a story.');
      emit.state(session, 'speaking');
      emit.delta(session, 'Once upon');
      emit.audioSection(session, { section: 0, ordinal: 0 });

      h.stopSpeaking();

      // The gateway cancels the turn or lets its commit finish, and says nothing either way:
      // the preview of it goes, and the thread is reloaded as it is for a turn that failed.
      expect(h.voice()).toMatchObject({
        liveTranscript: '',
        answerPreview: '',
        audioSection: null,
      });
      expect(h.refreshThread).toHaveBeenCalledTimes(1);
      expect(h.refreshThread).toHaveBeenCalledWith('c1');
    });

    it('stopSpeaking while listening leaves what the user is saying alone', () => {
      const h = setup();
      const session = listening(h);
      emit.partial(session, 'still talking');

      h.stopSpeaking();

      // The session is told (it is safe at any time, and the gateway ignores it), but there is
      // no answer being spoken whose preview could be dropped.
      expect(session.stopSpeaking).toHaveBeenCalledTimes(1);
      expect(h.voice().liveTranscript).toBe('still talking');
      expect(h.refreshThread).not.toHaveBeenCalled();
    });

    it('stopSpeaking after the turn is committed (draining) reloads nothing: audio_done already did', () => {
      const h = setup();
      const session = listening(h);
      emit.state(session, 'speaking');
      emit.audioDone(session);
      emit.state(session, 'draining');
      h.refreshThread.mockClear();

      h.stopSpeaking();

      expect(session.stopSpeaking).toHaveBeenCalledTimes(1);
      expect(h.refreshThread).not.toHaveBeenCalled();
    });

    it('do nothing without a session', () => {
      const h = setup();

      h.pause();
      h.resume();
      h.stopSpeaking();
      h.toggleMicMute();
      h.toggleMute();

      expect(sessions).toHaveLength(0);
      expect(h.voice()).toMatchObject({
        state: 'off',
        paused: false,
        micMuted: false,
        muted: false,
      });
      expect(h.refreshThread).not.toHaveBeenCalled();
    });

    it('a pause does not outlive the conversation, nor start the next one paused', () => {
      const h = setup();
      const first = listening(h);
      emit.paused(first, true);
      expect(h.voice().paused).toBe(true);

      h.stop();
      expect(h.voice().paused).toBe(false);
      // The session that was let go of is deaf, and cannot pause the display again.
      emit.paused(first, true);
      expect(h.voice().paused).toBe(false);

      h.start();
      expect(h.voice().paused).toBe(false);
    });

    it('a pause is cleared when the conversation fails', () => {
      const h = setup();
      const session = listening(h);
      emit.paused(session, true);

      emit.failed(session, { kind: 'dropped' });

      expect(h.voice().paused).toBe(false);
    });

    it('stop is still the end of the conversation, not a control of the answer', () => {
      const h = setup();
      const session = listening(h);
      emit.state(session, 'speaking');

      h.stop();

      expect(session.stop).toHaveBeenCalledTimes(1);
      expect(session.stopSpeaking).not.toHaveBeenCalled();
      expect(h.voice().state).toBe('off');
    });
  });

  describe('the microphone mute', () => {
    it('toggleMicMute mutes the session`s microphone, and unmutes it the next time', () => {
      const h = setup();
      const session = listening(h);

      h.toggleMicMute();
      expect(session.setMicMuted).toHaveBeenLastCalledWith(true);
      expect(h.voice().micMuted).toBe(true);

      h.toggleMicMute();
      expect(session.setMicMuted).toHaveBeenLastCalledWith(false);
      expect(h.voice().micMuted).toBe(false);
      expect(session.setMicMuted).toHaveBeenCalledTimes(2);
    });

    it('is available whenever the conversation is on, whatever the gateway is doing', () => {
      const h = setup();
      const session = listening(h);

      for (const state of ['thinking', 'speaking', 'draining', 'idle'] as const) {
        emit.state(session, state);
        h.toggleMicMute();
      }

      expect(session.setMicMuted.mock.calls).toEqual([[true], [false], [true], [false]]);
    });

    it('is a mute of the microphone only: the turn on screen and the answer are untouched', () => {
      const h = setup();
      const session = listening(h);
      emit.partial(session, 'half a sentence');

      h.toggleMicMute();

      expect(h.voice()).toMatchObject({ liveTranscript: 'half a sentence', state: 'listening' });
      expect(session.cancel).not.toHaveBeenCalled();
      expect(session.pause).not.toHaveBeenCalled();
    });

    it('is cleared by the end of the conversation: the next one starts with a live microphone', () => {
      const h = setup();
      const first = listening(h);
      h.toggleMicMute();
      expect(h.voice().micMuted).toBe(true);

      h.stop();
      expect(h.voice().micMuted).toBe(false);

      h.start();
      expect(h.voice().micMuted).toBe(false);
      // And the toggle is a mute again, not an unmute of a mute that is gone.
      const second = sessions[1];
      emit.state(second, 'listening');
      h.toggleMicMute();
      expect(second.setMicMuted).toHaveBeenCalledWith(true);
      expect(first.setMicMuted).toHaveBeenCalledTimes(1);
    });

    it('is cleared when the conversation fails', () => {
      const h = setup();
      const session = listening(h);
      h.toggleMicMute();

      emit.failed(session, { kind: 'capture' });

      expect(h.voice().micMuted).toBe(false);
    });

    it('survives a failed turn: the conversation is still on, and so is the mute', () => {
      const h = setup();
      const session = listening(h);
      h.toggleMicMute();

      emit.turnError(session, 'speech.upstream_error');

      expect(h.voice().micMuted).toBe(true);
    });
  });

  describe('the answer mute (the speakers)', () => {
    it('toggleMute mutes the session`s speakers, and unmutes them the next time', () => {
      const h = setup();
      const session = listening(h);

      h.toggleMute();
      expect(session.setMuted).toHaveBeenLastCalledWith(true);
      expect(h.voice().muted).toBe(true);

      h.toggleMute();
      expect(session.setMuted).toHaveBeenLastCalledWith(false);
      expect(h.voice().muted).toBe(false);
      expect(session.setMuted).toHaveBeenCalledTimes(2);
    });

    it('is available whenever the conversation is on: the mute is the session`s, and holds for the answers to come', () => {
      const h = setup();
      const session = listening(h);

      for (const state of ['thinking', 'speaking', 'draining', 'idle'] as const) {
        emit.state(session, state);
        h.toggleMute();
      }

      expect(session.setMuted.mock.calls).toEqual([[true], [false], [true], [false]]);
    });

    it('is not the microphone mute, and does not touch the answer', () => {
      const h = setup();
      const session = listening(h);
      emit.state(session, 'speaking');

      h.toggleMute();

      expect(h.voice().micMuted).toBe(false);
      expect(session.setMicMuted).not.toHaveBeenCalled();
      expect(session.pause).not.toHaveBeenCalled();
      expect(session.stopSpeaking).not.toHaveBeenCalled();
    });

    it('the microphone mute is not this one', () => {
      const h = setup();
      const session = listening(h);

      h.toggleMicMute();

      expect(h.voice().muted).toBe(false);
      expect(session.setMuted).not.toHaveBeenCalled();
    });

    it('is cleared by the end of the conversation: the next one is audible', () => {
      const h = setup();
      const first = listening(h);
      h.toggleMute();
      expect(h.voice().muted).toBe(true);

      h.stop();
      expect(h.voice().muted).toBe(false);

      h.start();
      expect(h.voice().muted).toBe(false);
      // And the toggle is a mute again, not an unmute of a mute that is gone.
      const second = sessions[1];
      emit.state(second, 'listening');
      h.toggleMute();
      expect(second.setMuted).toHaveBeenCalledWith(true);
      expect(first.setMuted).toHaveBeenCalledTimes(1);
    });

    it('is cleared when the conversation fails', () => {
      const h = setup();
      const session = listening(h);
      h.toggleMute();

      emit.failed(session, { kind: 'capture' });

      expect(h.voice().muted).toBe(false);
    });

    it('survives a failed turn: the conversation is still on, and so is the mute', () => {
      const h = setup();
      const session = listening(h);
      h.toggleMute();

      emit.turnError(session, 'speech.upstream_error');

      expect(h.voice().muted).toBe(true);
    });
  });

  describe('whether the answer is being heard', () => {
    it('follows the session', () => {
      const h = setup();
      const session = listening(h);
      emit.state(session, 'speaking');

      emit.playing(session, true);
      expect(h.voice().audioPlaying).toBe(true);

      emit.playing(session, false);
      expect(h.voice().audioPlaying).toBe(false);
    });

    it('does not outlive the conversation, nor start the next one playing', () => {
      const h = setup();
      const first = listening(h);
      emit.playing(first, true);

      h.stop();
      expect(h.voice().audioPlaying).toBe(false);
      // The session that was let go of is deaf, and cannot start the display again.
      emit.playing(first, true);
      expect(h.voice().audioPlaying).toBe(false);

      h.start();
      expect(h.voice().audioPlaying).toBe(false);
    });

    it('is cleared when the conversation fails', () => {
      const h = setup();
      const session = listening(h);
      emit.playing(session, true);

      emit.failed(session, { kind: 'dropped' });

      expect(h.voice().audioPlaying).toBe(false);
    });
  });

  describe('the section the live audio belongs to', () => {
    it('follows the session, including across a gap in the gateway`s section indices', () => {
      const h = setup();
      const session = listening(h);
      emit.state(session, 'speaking');

      emit.audioSection(session, { section: 0, ordinal: 0 });
      expect(h.voice().audioSection).toEqual({ section: 0, ordinal: 0 });

      // Section 1's speech failed, so the next one the gateway tags is 2: the second block.
      emit.audioSection(session, { section: 2, ordinal: 1 });
      expect(h.voice().audioSection).toEqual({ section: 2, ordinal: 1 });
    });

    it('is dropped with the turn: at its commit, at a cancel, and at a failed turn', () => {
      const h = setup();
      const session = listening(h);

      emit.audioSection(session, { section: 0, ordinal: 0 });
      emit.audioDone(session);
      expect(h.voice().audioSection).toBeNull();

      emit.audioSection(session, { section: 0, ordinal: 0 });
      h.cancel();
      expect(h.voice().audioSection).toBeNull();

      emit.audioSection(session, { section: 0, ordinal: 0 });
      emit.turnError(session);
      expect(h.voice().audioSection).toBeNull();
    });

    it('ignores a session that was let go of', () => {
      const h = setup();
      const first = listening(h);
      h.stop();

      emit.audioSection(first, { section: 0, ordinal: 0 });

      expect(h.voice().audioSection).toBeNull();
    });

    it('keeps the same object when the section did not change, so nothing re-renders for it', () => {
      const h = setup();
      const session = listening(h);
      emit.audioSection(session, { section: 1, ordinal: 0 });
      const before = h.voice();

      emit.audioSection(session, { section: 1, ordinal: 0 });

      expect(h.voice()).toBe(before);
    });
  });

  describe('stopping', () => {
    it('stop tears the session down and forgets the display', () => {
      const h = setup();
      const session = listening(h);
      emit.final(session, 'Half a turn');
      emit.delta(session, 'Half an answer');

      h.stop();

      expect(session.stop).toHaveBeenCalledTimes(1);
      expect(h.voice()).toMatchObject({
        state: 'off',
        liveTranscript: '',
        answerPreview: '',
        error: null,
      });
    });

    it('ignores whatever the stopped session reports afterwards', () => {
      const h = setup();
      const session = listening(h);
      h.stop();

      emit.state(session, 'closed');
      emit.state(session, 'speaking');
      emit.partial(session, 'late');
      emit.delta(session, 'late');
      emit.audioDone(session);
      emit.turnError(session);
      emit.failed(session, { kind: 'dropped' });

      expect(h.voice()).toMatchObject({
        state: 'off',
        liveTranscript: '',
        answerPreview: '',
        error: null,
      });
      expect(h.refreshThread).not.toHaveBeenCalled();
    });

    it('is safe to repeat, and without a session', () => {
      const h = setup();
      h.stop();
      const session = listening(h);

      h.stop();
      h.stop();

      expect(session.stop).toHaveBeenCalledTimes(1);
      expect(h.voice().state).toBe('off');
    });

    it('a new conversation after a stop is a new session, deaf to the old one', () => {
      const h = setup();
      const first = listening(h);
      h.stop();

      h.start();
      emit.state(sessions[1], 'listening');
      emit.partial(sessions[1], 'second conversation');
      emit.partial(first, 'ghost');
      emit.state(first, 'thinking');

      expect(sessions).toHaveLength(2);
      expect(h.voice()).toMatchObject({
        state: 'listening',
        liveTranscript: 'second conversation',
      });
    });

    it('lets go of the session when the provider goes away', () => {
      const h = setup();
      const session = listening(h);

      h.unmount();

      expect(session.stop).toHaveBeenCalledTimes(1);
    });

    it('does not touch a session that was never made, when the provider goes away', () => {
      const h = setup();
      h.unmount();
      expect(sessions).toHaveLength(0);
    });
  });

  describe('the chat the conversation belongs to', () => {
    it('ends when another chat is opened', () => {
      const h = setup();
      const session = listening(h);
      emit.partial(session, 'for chat one');

      h.rerender({ activeChatId: 'c2' });

      expect(session.stop).toHaveBeenCalledTimes(1);
      expect(h.voice()).toMatchObject({ state: 'off', liveTranscript: '' });
    });

    it('ends when the chat is closed', () => {
      const h = setup();
      const session = listening(h);

      h.rerender({ activeChatId: null });

      expect(session.stop).toHaveBeenCalledTimes(1);
      expect(h.voice().state).toBe('off');
    });

    it('does nothing while the chat is unchanged', () => {
      const h = setup();
      const session = listening(h);

      h.rerender({ activeChatId: 'c1' });

      expect(session.stop).not.toHaveBeenCalled();
      expect(h.voice().state).toBe('listening');
    });

    it('does nothing without a conversation', () => {
      const h = setup();
      h.rerender({ activeChatId: 'c2' });
      expect(h.voice().state).toBe('off');
    });
  });

  describe('errors', () => {
    it('an error frame surfaces and returns to idle; the session goes on', () => {
      const h = setup();
      const session = listening(h);
      emit.final(session, 'What is the capital of France?');
      emit.state(session, 'thinking');
      emit.delta(session, 'Pa');

      emit.turnError(session, 'speech.upstream_error', 'the speech model failed');

      expect(h.voice().error).toEqual({
        kind: 'turn',
        code: 'speech.upstream_error',
        message: 'the speech model failed',
      });
      expect(h.voice().state).toBe('idle');
      // The turn failed, so nothing of it is left on the display.
      expect(h.voice()).toMatchObject({ liveTranscript: '', answerPreview: '' });
      // The session is NOT over: it is the same one, and the gateway's frames still reach the hook.
      expect(session.stop).not.toHaveBeenCalled();
      emit.state(session, 'listening');
      emit.partial(session, 'try again');
      expect(h.voice()).toMatchObject({ state: 'listening', liveTranscript: 'try again' });
    });

    // A turn can fail AFTER the gateway has committed it (the speech degraded once the answer was
    // stored): its `error` frame, not an audio_done, is then the only signal this tab gets, so it
    // reloads the thread like the end of a turn does or the committed turn shows only on the
    // next open of the chat.
    it('a turn error reloads the thread, so a turn that degraded after its commit shows', () => {
      const h = setup();
      const session = listening(h);
      emit.final(session, 'What is the capital of France?');
      emit.state(session, 'thinking');
      emit.delta(session, 'Paris.');
      expect(h.refreshThread).not.toHaveBeenCalled();

      emit.turnError(session, 'speech.upstream_error', 'the speech model failed');

      expect(h.refreshThread).toHaveBeenCalledTimes(1);
      expect(h.refreshThread).toHaveBeenCalledWith('c1');
      // The reload does not touch what the error says, nor the conversation that goes on.
      expect(h.voice().error).toEqual({
        kind: 'turn',
        code: 'speech.upstream_error',
        message: 'the speech model failed',
      });
      expect(h.voice().state).toBe('idle');
      expect(session.stop).not.toHaveBeenCalled();
    });

    it('reloads the chat the conversation started in, and once per failed turn', () => {
      const h = setup({ begin: () => ({ ...START, chatId: 'c-voice' }), activeChatId: 'c-voice' });
      const session = listening(h);

      emit.turnError(session, 'speech.upstream_error');
      emit.state(session, 'listening');
      emit.turnError(session);

      expect(h.refreshThread.mock.calls).toEqual([['c-voice'], ['c-voice']]);
    });

    it.each([
      ['rejects', () => Promise.reject(new Error('offline'))],
      [
        'throws',
        () => {
          throw new Error('broken');
        },
      ],
    ])(
      'a reload after a turn error that %s leaves the error and the conversation',
      async (_n, refreshThread) => {
        const h = setup({ refreshThread });
        const session = listening(h);

        emit.turnError(session, 'speech.upstream_error');
        await act(async () => undefined);

        expect(h.voice()).toMatchObject({ state: 'idle', error: { kind: 'turn' } });
        emit.state(session, 'listening');
        emit.partial(session, 'still here');
        expect(h.voice().liveTranscript).toBe('still here');
      },
    );

    it('ignores the turn error of a session that was let go of: no reload either', () => {
      const h = setup();
      const session = listening(h);
      h.stop();

      emit.turnError(session, 'speech.upstream_error');

      expect(h.refreshThread).not.toHaveBeenCalled();
      expect(h.voice().error).toBeNull();
    });

    it('a turn error without a code carries none', () => {
      const h = setup();
      const session = listening(h);

      emit.turnError(session);

      expect(h.voice().error).toEqual({ kind: 'turn', message: 'the turn failed' });
    });

    it('the user re-engaging (pushStart) clears a turn error, and so does a new conversation', () => {
      const h = setup();
      const session = listening(h);
      emit.turnError(session, 'speech.upstream_error');
      expect(h.voice().error).not.toBeNull();

      h.pushStart();

      expect(h.voice().error).toBeNull();
      expect(session.pushStart).toHaveBeenCalledTimes(1);
    });

    const terminal: [string, VoiceErrorDetail][] = [
      ['the connection dropped', { kind: 'dropped' }],
      ['the gateway refused the ticket', { kind: 'refused', code: 'portal.voice_chat_busy' }],
      ['the microphone is refused', { kind: 'denied' }],
      ['the browser cannot capture', { kind: 'unsupported' }],
      ['the audio capture failed', { kind: 'capture' }],
    ];

    it.each(terminal)(
      '%s: the error is surfaced and the conversation is torn down',
      (_n, detail) => {
        const h = setup();
        const session = listening(h);
        emit.final(session, 'said so far');
        emit.delta(session, 'answer so far');

        emit.failed(session, detail, 'it failed');

        // No reload from the hook: an end of the conversation in the middle of a turn is the
        // store's own reload (a second one here would double a failed reload's report).
        expect(h.refreshThread).not.toHaveBeenCalled();
        expect(h.voice().error).toEqual({ ...detail, message: 'it failed' });
        expect(h.voice()).toMatchObject({ state: 'off', liveTranscript: '', answerPreview: '' });
        // Nothing is retried on its own: a new conversation is the user's call.
        expect(sessions).toHaveLength(1);
        // Whatever the dead session says afterwards is nobody's.
        emit.state(session, 'listening');
        emit.partial(session, 'ghost');
        expect(h.voice().state).toBe('off');
      },
    );

    it('a terminal error is cleared by the next start, and by stop (the user has seen it)', () => {
      const h = setup();
      emit.failed(listening(h), { kind: 'denied' });
      expect(h.voice().error).not.toBeNull();

      h.stop();
      expect(h.voice().error).toBeNull();

      emit.failed((h.start(), sessions[1]), { kind: 'dropped' });
      expect(h.voice().error).not.toBeNull();
      h.start();
      expect(sessions).toHaveLength(3);
      expect(h.voice().error).toBeNull();
    });

    it.each(['closed', 'error'] as const)(
      'a session that reports %s without saying why is over: the hook does not sit on a dead one',
      (final) => {
        const h = setup();
        const session = listening(h);
        emit.partial(session, 'said');

        emit.state(session, final);

        expect(h.voice()).toMatchObject({ state: 'off', liveTranscript: '' });
        // And a new conversation can be started.
        h.start();
        expect(sessions).toHaveLength(2);
      },
    );
  });
});
