// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { act, cleanup, renderHook } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi, type Mock } from 'vitest';
import { messages, type Locale } from '../../i18n';
import {
  createLiveSession,
  type LiveErrorDetail,
  type LiveSessionArgs,
  type LiveSessionState,
} from './liveTranscription';
import {
  LIVE_RECONNECT_BASE_DELAY_MS,
  LIVE_RECONNECT_MAX_ATTEMPTS,
  useLiveTranscription,
  type LiveThread,
} from './useLiveTranscription';

// The real session is the browser's microphone and a WebSocket (liveTranscription.session.test.ts
// has a fake browser for those). Here it is a recording stand-in, so that these tests are about
// what the STORE does with a session: the transcript it keeps, the states it shows, when it
// reconnects and when it gives up, and what it commits.
vi.mock('./liveTranscription', async (importOriginal) => ({
  ...(await importOriginal<typeof import('./liveTranscription')>()),
  createLiveSession: vi.fn(),
}));

type FakeSession = {
  args: LiveSessionArgs;
  stop: Mock<() => void>;
  abort: Mock<() => void>;
};

let sessions: FakeSession[];

// What the real session does at each point, as far as the store can see it: a state change is
// reported through onState, and the callback that explains a final state (onDone, onError)
// comes just before it.
const emit = {
  state: (s: FakeSession, state: LiveSessionState) => act(() => s.args.onState(state)),
  listening: (s: FakeSession) => emit.state(s, 'listening'),
  delta: (s: FakeSession, text: string) => act(() => s.args.onDelta(text)),
  partial: (s: FakeSession, text: string) => act(() => s.args.onPartial(text)),
  done: (s: FakeSession, text = '') =>
    act(() => {
      s.args.onDone(text);
      s.args.onState('closed');
    }),
  error: (s: FakeSession, detail: LiveErrorDetail, message = 'it failed') =>
    act(() => {
      s.args.onError(message, detail);
      s.args.onState('error');
    }),
};

const dropped: LiveErrorDetail = { kind: 'dropped' };

const THREAD: LiveThread = { chatId: 'c1', model: 'whisper', available: true };

beforeEach(() => {
  vi.useFakeTimers();
  sessions = [];
  vi.mocked(createLiveSession).mockImplementation((args) => {
    const session: FakeSession = {
      args,
      // The real session reports `finalizing` from inside stop().
      stop: vi.fn(() => args.onState('finalizing')),
      abort: vi.fn(),
    };
    sessions.push(session);
    // `connecting` is the one state reported before createLiveSession returns.
    args.onState('connecting');
    return session;
  });
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.mocked(createLiveSession).mockReset();
});

for (const locale of ['de', 'en'] as readonly Locale[]) {
  const t = messages[locale];

  describe(`useLiveTranscription [${locale}]`, () => {
    function setup(
      options: {
        begin?: () => { chatId: string; model: string; language: string } | null;
        commit?: (chatId: string, transcript: string) => string | null;
      } = {},
    ) {
      const begin = vi.fn(
        options.begin ?? (() => ({ chatId: 'c1', model: 'whisper', language: '' })),
      );
      const commit = vi.fn(options.commit ?? (() => null));
      const showError = vi.fn();
      const api = { createLiveTranscriptionTicket: vi.fn() };
      const view = renderHook(
        ({ thread }: { thread: LiveThread }) =>
          useLiveTranscription({
            apiRef: { current: api },
            tRef: { current: t },
            showErrorRef: { current: showError },
            thread,
            begin,
            commit,
          }),
        { initialProps: { thread: THREAD } },
      );
      return {
        ...view,
        begin,
        commit,
        showError,
        api,
        live: () => view.result.current,
        start: () => act(() => view.result.current.startLiveTranscription()),
        stop: () => act(() => view.result.current.stopLiveTranscription()),
        abort: () => act(() => view.result.current.abortLiveTranscription()),
        keep: () => act(() => view.result.current.keepLiveTranscript()),
        advance: (ms: number) => act(() => void vi.advanceTimersByTime(ms)),
      };
    }

    describe('starting', () => {
      it('creates the session inside the call, with the thread, its model and the language', () => {
        const h = setup({ begin: () => ({ chatId: 'c1', model: 'whisper', language: 'de' }) });
        expect(h.live().liveSession).toEqual({ state: 'idle', transcript: '' });

        h.start();

        // Synchronously: this is what keeps the microphone request inside the user's gesture.
        expect(sessions).toHaveLength(1);
        expect(sessions[0].args.model).toBe('whisper');
        expect(sessions[0].args.language).toBe('de');
        expect(sessions[0].args.api).toBe(h.api);
        expect(h.live().liveSession.state).toBe('connecting');
      });

      it('passes no language when there is none (auto-detect)', () => {
        const h = setup();
        h.start();
        expect(sessions[0].args.language).toBeUndefined();
      });

      it('follows the session to listening', () => {
        const h = setup();
        h.start();
        emit.listening(sessions[0]);
        expect(h.live().liveSession.state).toBe('listening');
      });

      it('does not start when the thread cannot (begin says no)', () => {
        const h = setup({ begin: () => null });
        h.start();
        expect(sessions).toHaveLength(0);
        expect(h.live().liveSession.state).toBe('idle');
      });

      it('does not start a second session while one is running', () => {
        const h = setup();
        h.start();
        h.start();
        expect(sessions).toHaveLength(1);
        expect(h.begin).toHaveBeenCalledTimes(1);
      });
    });

    describe('the transcript', () => {
      it('appends a delta and replaces on a partial', () => {
        const h = setup();
        h.start();
        emit.listening(sessions[0]);

        emit.delta(sessions[0], 'Hello wor');
        expect(h.live().liveSession.transcript).toBe('Hello wor');
        emit.delta(sessions[0], 'ld');
        expect(h.live().liveSession.transcript).toBe('Hello world');
        emit.partial(sessions[0], 'Hello, world');
        expect(h.live().liveSession.transcript).toBe('Hello, world');
        emit.delta(sessions[0], '!');
        expect(h.live().liveSession.transcript).toBe('Hello, world!');
      });
    });

    describe('finishing', () => {
      it('done commits the final transcript as a turn, and the session is idle again', () => {
        const h = setup();
        h.start();
        emit.listening(sessions[0]);
        emit.delta(sessions[0], 'Hello wor');
        h.stop();
        expect(sessions[0].stop).toHaveBeenCalledTimes(1);
        expect(h.live().liveSession.state).toBe('finalizing');

        emit.done(sessions[0], 'Hello world.');

        expect(h.commit).toHaveBeenCalledTimes(1);
        expect(h.commit).toHaveBeenCalledWith('c1', 'Hello world.');
        expect(h.live().liveSession).toEqual({ state: 'idle', transcript: '' });
        expect(h.showError).not.toHaveBeenCalled();
      });

      it('a done with no text of its own keeps what the deltas gave', () => {
        const h = setup();
        h.start();
        emit.listening(sessions[0]);
        emit.delta(sessions[0], 'Kept as spoken');
        h.stop();

        emit.done(sessions[0], '');

        expect(h.commit).toHaveBeenCalledWith('c1', 'Kept as spoken');
      });

      it('a done with nothing at all commits nothing, and says so', () => {
        const h = setup();
        h.start();
        emit.listening(sessions[0]);
        h.stop();

        emit.done(sessions[0], '');

        expect(h.commit).not.toHaveBeenCalled();
        expect(h.showError).toHaveBeenCalledWith(t.errorChatRunNoTranscript);
        expect(h.live().liveSession.state).toBe('idle');
      });

      it('a commit the thread refuses is an error with the transcript kept, and "use" tries again', () => {
        const commit = vi.fn<(chatId: string, transcript: string) => string | null>();
        commit.mockReturnValueOnce('the chat is busy');
        const h = setup({ commit });
        h.start();
        emit.listening(sessions[0]);
        emit.delta(sessions[0], 'Worth keeping');
        h.stop();
        emit.done(sessions[0], 'Worth keeping.');

        expect(h.live().liveSession).toEqual({
          state: 'error',
          transcript: 'Worth keeping.',
          error: 'the chat is busy',
        });

        commit.mockReturnValueOnce(null);
        h.keep();
        expect(commit).toHaveBeenLastCalledWith('c1', 'Worth keeping.');
        expect(h.live().liveSession).toEqual({ state: 'idle', transcript: '' });
      });

      it('ignores whatever the finished session reports afterwards', () => {
        const h = setup();
        h.start();
        emit.listening(sessions[0]);
        emit.delta(sessions[0], 'Text');
        h.stop();
        emit.done(sessions[0], 'Text.');

        emit.delta(sessions[0], 'late');
        emit.error(sessions[0], dropped);

        expect(h.commit).toHaveBeenCalledTimes(1);
        expect(h.live().liveSession.state).toBe('idle');
        expect(sessions).toHaveLength(1);
      });
    });

    describe('errors the user must act on: no retry, a message that says what to do', () => {
      const cases: [string, LiveErrorDetail, (typeof t)[keyof typeof t]][] = [
        ['the microphone is refused', { kind: 'denied' }, t.chatRecordDenied],
        ['the browser cannot capture', { kind: 'unsupported' }, t.chatTranscriptionLiveUnsupported],
        [
          'the model is not streaming',
          { kind: 'refused', code: 'transcription.live_not_streaming' },
          t.chatTranscriptionLiveUnsupported,
        ],
        [
          'the agent cannot stream',
          { kind: 'refused', code: 'transcription.live_agent_unsupported' },
          t.chatTranscriptionLiveUnsupported,
        ],
        [
          'the parameters are invalid',
          { kind: 'refused', code: 'transcription.live_params_invalid' },
          t.chatTranscriptionLiveUnsupported,
        ],
        [
          'the model is busy',
          { kind: 'refused', code: 'transcription.live_busy' },
          t.chatTranscriptionLiveBusy,
        ],
        [
          'the gateway answers with an error frame',
          { kind: 'refused' },
          t.chatTranscriptionLiveError,
        ],
        ['the audio capture fails', { kind: 'capture' }, t.chatTranscriptionLiveError],
      ];

      it.each(cases)('%s', (_name, detail, expected) => {
        const h = setup();
        h.start();
        emit.listening(sessions[0]);
        emit.delta(sessions[0], 'said so far');

        emit.error(sessions[0], detail);

        expect(h.live().liveSession).toEqual({
          state: 'error',
          transcript: 'said so far',
          error: expected,
        });
        // No retry, however long it waits.
        h.advance(60_000);
        expect(sessions).toHaveLength(1);
      });

      it('an error frame that carries the gateway code of a busy model reads as busy', () => {
        const h = setup();
        h.start();
        emit.error(sessions[0], { kind: 'refused' }, 'transcription.live_busy: model is busy');
        expect(h.live().liveSession.error).toBe(t.chatTranscriptionLiveBusy);
      });

      it('"use" offers what was transcribed before the failure', () => {
        const h = setup();
        h.start();
        emit.listening(sessions[0]);
        emit.delta(sessions[0], 'Before the failure');
        emit.error(sessions[0], { kind: 'refused' });

        h.keep();

        expect(h.commit).toHaveBeenCalledWith('c1', 'Before the failure');
        expect(h.live().liveSession.state).toBe('idle');
      });

      it('"use" with nothing transcribed just clears the error', () => {
        const h = setup();
        h.start();
        emit.error(sessions[0], { kind: 'denied' });

        h.keep();

        expect(h.commit).not.toHaveBeenCalled();
        expect(h.live().liveSession.state).toBe('idle');
      });

      it('can be started again from the error, with a clean slate', () => {
        const h = setup();
        h.start();
        emit.listening(sessions[0]);
        emit.delta(sessions[0], 'old');
        emit.error(sessions[0], { kind: 'capture' });

        h.start();

        expect(sessions).toHaveLength(2);
        expect(h.live().liveSession).toEqual({ state: 'connecting', transcript: '' });
      });
    });

    describe('reconnecting after an unexpected drop', () => {
      it('a drop while recording shows reconnecting, keeps the text, and starts a NEW session after a backoff', () => {
        const h = setup();
        h.start();
        emit.listening(sessions[0]);
        emit.delta(sessions[0], 'First part');

        emit.error(sessions[0], dropped);

        expect(h.live().liveSession).toEqual({
          state: 'reconnecting',
          transcript: 'First part',
        });
        // Not at once: the backoff comes first.
        expect(sessions).toHaveLength(1);
        h.advance(LIVE_RECONNECT_BASE_DELAY_MS - 1);
        expect(sessions).toHaveLength(1);
        h.advance(1);

        expect(sessions).toHaveLength(2);
        expect(sessions[1].args.model).toBe('whisper');
        // Still "reconnecting" while the new session connects...
        expect(h.live().liveSession.state).toBe('reconnecting');
        // ...and listening once it is up, with the earlier text still there.
        emit.listening(sessions[1]);
        expect(h.live().liveSession).toEqual({ state: 'listening', transcript: 'First part' });
      });

      it('continues the transcript after what was said, and a partial replaces only the new session', () => {
        const h = setup();
        h.start();
        emit.listening(sessions[0]);
        emit.delta(sessions[0], 'First part');
        emit.error(sessions[0], dropped);
        h.advance(LIVE_RECONNECT_BASE_DELAY_MS);
        emit.listening(sessions[1]);

        emit.delta(sessions[1], 'second');
        emit.partial(sessions[1], 'Second part');
        expect(h.live().liveSession.transcript).toBe('First part Second part');

        h.stop();
        emit.done(sessions[1], 'Second part.');

        // One turn, with both sessions' text; the dropped one is not sent again.
        expect(h.commit).toHaveBeenCalledTimes(1);
        expect(h.commit).toHaveBeenCalledWith('c1', 'First part Second part.');
      });

      it('the new session is created from the same thread, language and api, with a fresh ticket of its own', () => {
        const h = setup({ begin: () => ({ chatId: 'c1', model: 'whisper', language: 'de' }) });
        h.start();
        emit.error(sessions[0], dropped);
        h.advance(LIVE_RECONNECT_BASE_DELAY_MS);

        expect(sessions[1].args.language).toBe('de');
        expect(sessions[1].args.api).toBe(h.api);
        // Each session mints its own ticket (createLiveSession does, on its own).
        expect(sessions[1].args).not.toBe(sessions[0].args);
        expect(h.begin).toHaveBeenCalledTimes(1);
      });

      it('backs off longer each time, and gives up after the bound with an error', () => {
        const h = setup();
        h.start();
        emit.listening(sessions[0]);
        emit.delta(sessions[0], 'said');
        // The first failure has progress (the text) behind it; the rest have none.
        emit.error(sessions[0], dropped);

        for (let attempt = 1; attempt <= LIVE_RECONNECT_MAX_ATTEMPTS; attempt += 1) {
          const delay = LIVE_RECONNECT_BASE_DELAY_MS * 2 ** (attempt - 1);
          h.advance(delay - 1);
          expect(sessions).toHaveLength(attempt);
          h.advance(1);
          expect(sessions).toHaveLength(attempt + 1);
          expect(h.live().liveSession.state).toBe('reconnecting');
          emit.error(sessions[attempt], dropped);
        }

        // The last attempt dropped as well: no more.
        expect(h.live().liveSession).toEqual({
          state: 'error',
          transcript: 'said',
          error: t.chatTranscriptionLiveError,
        });
        h.advance(600_000);
        expect(sessions).toHaveLength(LIVE_RECONNECT_MAX_ATTEMPTS + 1);
      });

      it('a session that got somewhere (it transcribed) gives the retries back', () => {
        const h = setup();
        h.start();
        // Many more drops than the bound, each followed by a reconnect that produced text.
        for (let round = 0; round < LIVE_RECONNECT_MAX_ATTEMPTS * 2; round += 1) {
          const session = sessions.at(-1)!;
          emit.listening(session);
          emit.delta(session, `part${round}`);
          emit.error(session, dropped);
          expect(h.live().liveSession.state).toBe('reconnecting');
          h.advance(LIVE_RECONNECT_BASE_DELAY_MS);
        }
        expect(sessions).toHaveLength(LIVE_RECONNECT_MAX_ATTEMPTS * 2 + 1);
        expect(h.live().liveSession.state).toBe('reconnecting');
      });

      it('a refusal while reconnecting ends it: an error, no further attempt', () => {
        const h = setup();
        h.start();
        emit.listening(sessions[0]);
        emit.delta(sessions[0], 'said');
        emit.error(sessions[0], dropped);
        h.advance(LIVE_RECONNECT_BASE_DELAY_MS);

        emit.error(sessions[1], { kind: 'refused', code: 'transcription.live_busy' });

        expect(h.live().liveSession).toEqual({
          state: 'error',
          transcript: 'said',
          error: t.chatTranscriptionLiveBusy,
        });
        h.advance(600_000);
        expect(sessions).toHaveLength(2);
      });

      it('a drop after Stop is not reconnected: the user has already said they are done', () => {
        const h = setup();
        h.start();
        emit.listening(sessions[0]);
        emit.delta(sessions[0], 'said');
        h.stop();

        emit.error(sessions[0], dropped, 'did not deliver its final transcript');

        expect(h.live().liveSession).toEqual({
          state: 'error',
          transcript: 'said',
          error: t.chatTranscriptionLiveError,
        });
        h.advance(600_000);
        expect(sessions).toHaveLength(1);
      });

      it('Stop while waiting to reconnect commits what was said and starts nothing', () => {
        const h = setup();
        h.start();
        emit.listening(sessions[0]);
        emit.delta(sessions[0], 'Said before the drop');
        emit.error(sessions[0], dropped);
        expect(h.live().liveSession.state).toBe('reconnecting');

        h.stop();

        expect(h.commit).toHaveBeenCalledWith('c1', 'Said before the drop');
        expect(h.live().liveSession.state).toBe('idle');
        h.advance(600_000);
        expect(sessions).toHaveLength(1);
      });

      it('Stop while waiting to reconnect, with nothing said, just ends it quietly', () => {
        const h = setup();
        h.start();
        emit.error(sessions[0], dropped);

        h.stop();

        expect(h.commit).not.toHaveBeenCalled();
        expect(h.showError).not.toHaveBeenCalled();
        expect(h.live().liveSession.state).toBe('idle');
      });

      it('ignores a late report from the session that was replaced', () => {
        const h = setup();
        h.start();
        emit.listening(sessions[0]);
        emit.delta(sessions[0], 'First');
        emit.error(sessions[0], dropped);
        h.advance(LIVE_RECONNECT_BASE_DELAY_MS);
        emit.listening(sessions[1]);

        // The first session is over; whatever it might still say is nobody's.
        emit.delta(sessions[0], 'ghost');
        emit.error(sessions[0], dropped);

        expect(h.live().liveSession).toEqual({ state: 'listening', transcript: 'First' });
        expect(sessions).toHaveLength(2);
      });
    });

    describe('stopping', () => {
      it('Stop before the microphone is ready closes the session without a final transcript: nothing said, nothing committed', () => {
        const h = setup();
        h.start();

        h.stop();
        // The real session finishes `closed` straight away, with no done.
        emit.state(sessions[0], 'closed');

        expect(h.commit).not.toHaveBeenCalled();
        expect(h.live().liveSession.state).toBe('idle');
      });

      it('Stop before the microphone is ready, after an earlier session said something, keeps that', () => {
        const h = setup();
        h.start();
        emit.listening(sessions[0]);
        emit.delta(sessions[0], 'Earlier');
        emit.error(sessions[0], dropped);
        h.advance(LIVE_RECONNECT_BASE_DELAY_MS);

        h.stop();
        emit.state(sessions[1], 'closed');

        expect(h.commit).toHaveBeenCalledWith('c1', 'Earlier');
        expect(h.live().liveSession.state).toBe('idle');
      });

      it('does nothing without a session', () => {
        const h = setup();
        h.stop();
        expect(h.live().liveSession.state).toBe('idle');
      });

      it('shows finalizing until the final transcript arrives', () => {
        const h = setup();
        h.start();
        emit.listening(sessions[0]);
        h.stop();
        h.stop();
        expect(sessions[0].stop).toHaveBeenCalledTimes(1);
        expect(h.live().liveSession.state).toBe('finalizing');
        // Text still arriving while the model finishes its audio is kept.
        emit.delta(sessions[0], 'tail');
        expect(h.live().liveSession.transcript).toBe('tail');
      });
    });

    describe('aborting', () => {
      it('aborts the session, forgets everything and ignores what it says afterwards', () => {
        const h = setup();
        h.start();
        emit.listening(sessions[0]);
        emit.delta(sessions[0], 'thrown away');

        h.abort();

        expect(sessions[0].abort).toHaveBeenCalledTimes(1);
        expect(h.live().liveSession).toEqual({ state: 'idle', transcript: '' });
        emit.done(sessions[0], 'late text');
        expect(h.commit).not.toHaveBeenCalled();
        expect(h.live().liveSession.state).toBe('idle');
      });

      it('cancels a reconnect that is waiting', () => {
        const h = setup();
        h.start();
        emit.error(sessions[0], dropped);

        h.abort();
        h.advance(600_000);

        expect(sessions).toHaveLength(1);
        expect(h.live().liveSession.state).toBe('idle');
      });

      it('clears an error too', () => {
        const h = setup();
        h.start();
        emit.error(sessions[0], { kind: 'refused' });
        h.abort();
        expect(h.live().liveSession).toEqual({ state: 'idle', transcript: '' });
      });

      it('lets go of the session when the provider goes away', () => {
        const h = setup();
        h.start();
        emit.listening(sessions[0]);

        h.unmount();

        expect(sessions[0].abort).toHaveBeenCalledTimes(1);
      });

      it('and of a waiting reconnect', () => {
        const h = setup();
        h.start();
        emit.error(sessions[0], dropped);

        h.unmount();
        h.advance(600_000);

        expect(sessions).toHaveLength(1);
      });
    });

    describe('the thread the session belongs to', () => {
      it('aborts when another chat is opened', () => {
        const h = setup();
        h.start();
        emit.listening(sessions[0]);
        emit.delta(sessions[0], 'for chat one');

        h.rerender({ thread: { ...THREAD, chatId: 'c2' } });

        expect(sessions[0].abort).toHaveBeenCalledTimes(1);
        expect(h.live().liveSession).toEqual({ state: 'idle', transcript: '' });
        expect(h.commit).not.toHaveBeenCalled();
      });

      it('aborts when the model changes, or live stops being on offer', () => {
        const h = setup();
        h.start();
        emit.listening(sessions[0]);
        h.rerender({ thread: { ...THREAD, model: 'parakeet' } });
        expect(sessions[0].abort).toHaveBeenCalledTimes(1);
        expect(h.live().liveSession.state).toBe('idle');

        h.rerender({ thread: THREAD });
        h.start();
        emit.listening(sessions[1]);
        h.rerender({ thread: { ...THREAD, available: false } });
        expect(sessions[1].abort).toHaveBeenCalledTimes(1);
        expect(h.live().liveSession.state).toBe('idle');
      });

      it('drops a failed transcript that no longer belongs to the open chat', () => {
        const h = setup();
        h.start();
        emit.listening(sessions[0]);
        emit.delta(sessions[0], 'left over');
        emit.error(sessions[0], { kind: 'refused' });
        expect(h.live().liveSession.state).toBe('error');

        h.rerender({ thread: { ...THREAD, chatId: 'c2' } });

        expect(h.live().liveSession).toEqual({ state: 'idle', transcript: '' });
      });

      it('does nothing while the thread is unchanged', () => {
        const h = setup();
        h.start();
        emit.listening(sessions[0]);
        h.rerender({ thread: { ...THREAD } });
        expect(sessions[0].abort).not.toHaveBeenCalled();
        expect(h.live().liveSession.state).toBe('listening');
      });
    });
  });
}
