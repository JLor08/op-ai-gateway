// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { VoiceChatControl, type VoiceChatControlProps } from './VoiceChatControl';
import { messages, type Locale } from '../i18n';
import type { ModelOption } from '../api';
import { DEFAULT_VOICE_SYSTEM_PROMPT } from './chat/ChatStore';
import { VOICE_TURN_FALLBACK_MESSAGE } from './chat/voiceSession';
import type {
  VoiceChatApi,
  VoiceChatModels,
  VoiceChatState,
  VoiceChoice,
  VoiceModelChoice,
  VoicePromptChoice,
} from './chat/ChatStore';

afterEach(cleanup);

const t = messages.en;

// The always-mounted room of the playback controls (a data-testid: it has no role of its own, so
// that an empty one says nothing to a screen reader), and the height it keeps: that of a row of
// small buttons, which is what the controls are.
const PLAYBACK_ROOM = 'voice-playback-room';
const PLAYBACK_ROOM_MIN_HEIGHT = 'calc(1.75 * 0.8125rem + 8px)';

// The hook's output, with every control a spy: this component only draws the conversation and
// forwards the user's gestures (useVoiceChat.test.ts is about the conversation itself).
function makeVoice(over: Partial<VoiceChatApi> = {}): VoiceChatApi {
  return {
    state: 'off',
    liveTranscript: '',
    answerPreview: '',
    error: null,
    paused: false,
    micMuted: false,
    muted: false,
    audioPlaying: false,
    audioSection: null,
    start: vi.fn(),
    stop: vi.fn(),
    pushStart: vi.fn(),
    pushEnd: vi.fn(),
    cancel: vi.fn(),
    pause: vi.fn(),
    resume: vi.fn(),
    stopSpeaking: vi.fn(),
    toggleMicMute: vi.fn(),
    toggleMute: vi.fn(),
    ...over,
  };
}

// A catalogue entry of an audio model, named for display (the picker shows the display name).
const entry = (id: string, over: Partial<ModelOption> = {}): ModelOption => ({
  id,
  display_name: `${id} (display)`,
  flavors: ['openai_audio'],
  loading_on_count: 0,
  ...over,
});

function makeChoice(ids: string[], over: Partial<VoiceModelChoice> = {}): VoiceModelChoice {
  return {
    options: ids.map((id) => entry(id)),
    pick: '',
    used: ids[0] ?? '',
    set: vi.fn(),
    ...over,
  };
}

// The speech model's voice field: the voices it offers and the one that would be spoken in.
function makeVoiceChoice(voices: string[] = [], over: Partial<VoiceChoice> = {}): VoiceChoice {
  return { voices, value: '', set: vi.fn(), ...over };
}

// The thread's voice system prompt: the text the field shows (the built-in default, pre-filled,
// unless the thread has its own).
function makePromptChoice(
  value = 'The built-in prompt.',
  over: Partial<VoicePromptChoice> = {},
): VoicePromptChoice {
  return { value, set: vi.fn(), ...over };
}

// The store's pickers of the conversation's models, with every setter a spy: this component only
// draws them and forwards the user's choice (the store's picks are ChatStore.test.tsx's).
function makeModels(over: Partial<VoiceChatModels> = {}): VoiceChatModels {
  return {
    ready: true,
    llm: 'llm (display)',
    asr: makeChoice(['asr-a', 'asr-b']),
    tts: makeChoice(['tts-a', 'tts-clone']),
    voice: makeVoiceChoice(),
    systemPrompt: makePromptChoice(),
    ...over,
  };
}

function renderControl(voice: VoiceChatApi, props: Partial<VoiceChatControlProps> = {}) {
  const ui = (next: VoiceChatApi) => (
    <VoiceChatControl t={t} voice={next} capable gap={null} models={makeModels()} {...props} />
  );
  const view = render(ui(voice));
  return { ...view, update: (next: VoiceChatApi) => view.rerender(ui(next)) };
}

// The indicator is the one status region of the control that names the state of the microphone.
function indicator() {
  return indicatorOf(t);
}

function indicatorOf(translation: typeof t) {
  return screen.getByRole('status', { name: translation.chatVoiceIndicator });
}

describe('VoiceChatControl toggle', () => {
  it('toggle is disabled when the model triple is not satisfiable', () => {
    const voice = makeVoice();
    renderControl(voice, { capable: false, gap: 'tts' });

    const toggle = screen.getByRole('switch', { name: t.chatVoice });
    expect(toggle).toBeDisabled();
    // The reason is on screen (not only in a tooltip), and the click does nothing.
    expect(screen.getByText(t.chatVoiceUnavailableTts)).toBeInTheDocument();
    fireEvent.click(toggle);
    expect(voice.start).not.toHaveBeenCalled();
  });

  it.each([
    ['llm', 'chatVoiceUnavailableLlm'],
    ['asr', 'chatVoiceUnavailableAsr'],
    ['tts', 'chatVoiceUnavailableTts'],
    ['checking', 'chatVoiceUnavailableChecking'],
  ] as const)('names the missing leg: %s', (gap, key) => {
    renderControl(makeVoice(), { capable: false, gap });
    expect(screen.getByRole('switch', { name: t.chatVoice })).toBeDisabled();
    expect(screen.getByText(t[key])).toBeInTheDocument();
  });

  it('is disabled, and says why, while a chat run holds the thread', () => {
    renderControl(makeVoice(), { busy: true });
    expect(screen.getByRole('switch', { name: t.chatVoice })).toBeDisabled();
    expect(screen.getByText(t.chatBusy)).toBeInTheDocument();
  });

  it('enabling calls start', () => {
    const voice = makeVoice();
    renderControl(voice);

    const toggle = screen.getByRole('switch', { name: t.chatVoice });
    expect(toggle).toBeEnabled();
    expect(toggle).not.toBeChecked();
    fireEvent.click(toggle);

    expect(voice.start).toHaveBeenCalledTimes(1);
    expect(voice.stop).not.toHaveBeenCalled();
  });

  it('disabling a running conversation calls stop, even when the models are no longer offered', () => {
    const voice = makeVoice({ state: 'listening' });
    // The catalogue moved on under the running conversation: the way OUT must stay open.
    renderControl(voice, { capable: false, gap: 'asr', busy: true });

    const toggle = screen.getByRole('switch', { name: t.chatVoice });
    expect(toggle).toBeChecked();
    expect(toggle).toBeEnabled();
    fireEvent.click(toggle);

    expect(voice.stop).toHaveBeenCalledTimes(1);
    expect(voice.start).not.toHaveBeenCalled();
  });
});

describe('VoiceChatControl lifetime', () => {
  it('ends a running conversation when it goes away (another page, another thread)', () => {
    const voice = makeVoice({ state: 'listening' });
    const { unmount } = renderControl(voice);
    expect(voice.stop).not.toHaveBeenCalled();
    // The control is only mounted while the thread is one Voice Chat is offered on, on the chat
    // page: when it goes, the microphone must not stay open with nothing on screen to end it.
    unmount();
    expect(voice.stop).toHaveBeenCalledTimes(1);
  });

  it('does not end anything when it goes away with the conversation off', () => {
    const voice = makeVoice({ state: 'off' });
    const { unmount } = renderControl(voice);
    unmount();
    expect(voice.stop).not.toHaveBeenCalled();
  });

  it('ends the conversation that is running when it goes, not the one it started with', () => {
    const first = makeVoice({ state: 'off' });
    const { update, unmount } = renderControl(first);
    const second = makeVoice({ state: 'speaking' });
    update(second);
    unmount();
    expect(second.stop).toHaveBeenCalledTimes(1);
    expect(first.stop).not.toHaveBeenCalled();
  });
});

describe('VoiceChatControl listening indicator', () => {
  it('the indicator shows a listening state when state==="listening" and a not-listening state during speaking/thinking/idle', () => {
    const { update } = renderControl(makeVoice({ state: 'listening' }));

    // LISTENING: an active mic and its label.
    expect(within(indicator()).getByText(t.chatVoiceStateListening)).toBeInTheDocument();
    expect(within(indicator()).getByTestId('MicIcon')).toBeInTheDocument();
    expect(within(indicator()).queryByTestId('MicOffIcon')).not.toBeInTheDocument();

    // NOT LISTENING: a muted mic and the matching status, never the listening label.
    const notListening: [VoiceChatState, string][] = [
      ['thinking', t.chatVoiceStateThinking],
      ['speaking', t.chatVoiceStateSpeaking],
      ['draining', t.chatVoiceStateDraining],
      ['idle', t.chatVoiceStateIdle],
      ['connecting', t.chatVoiceStateConnecting],
      ['off', t.chatVoiceStateOff],
    ];
    for (const [state, label] of notListening) {
      update(makeVoice({ state }));
      expect(within(indicator()).getByText(label), state).toBeInTheDocument();
      expect(within(indicator()).getByTestId('MicOffIcon'), state).toBeInTheDocument();
      expect(within(indicator()).queryByTestId('MicIcon'), state).not.toBeInTheDocument();
      expect(screen.queryByText(t.chatVoiceStateListening), state).not.toBeInTheDocument();
    }
  });

  // The indicator is what tells the user whether they are heard, so it must not scroll away
  // under a long thread or a long answer (#318): it sticks to the viewport's edge, on the page's
  // own background so that what scrolls under it does not show through. (jsdom has no layout, so
  // this pins the style, not the scrolling.)
  it('is sticky, on the page background, in every state', () => {
    const { update } = renderControl(makeVoice({ state: 'listening' }));
    for (const state of ['listening', 'thinking', 'idle', 'off'] as const) {
      update(makeVoice({ state }));
      const style = getComputedStyle(indicator());
      expect(style.position, state).toBe('sticky');
      expect(style.bottom, state).toBe('0px');
      expect(style.backgroundColor, state).toBe('var(--page)');
    }
  });

  it('says that the microphone waits for the answer while it is spoken', () => {
    const { update } = renderControl(makeVoice({ state: 'speaking' }));
    expect(within(indicator()).getByText(t.chatVoiceMicPaused)).toBeInTheDocument();

    update(makeVoice({ state: 'draining' }));
    expect(within(indicator()).getByText(t.chatVoiceMicPaused)).toBeInTheDocument();

    // ... and not otherwise: the claim is only true while the answer plays.
    update(makeVoice({ state: 'listening' }));
    expect(screen.queryByText(t.chatVoiceMicPaused)).not.toBeInTheDocument();
  });

  it('the live transcript shows while listening', () => {
    const { update } = renderControl(makeVoice({ state: 'listening', liveTranscript: '' }));
    // Nothing said yet: no empty box.
    expect(screen.queryByRole('region', { name: t.chatVoiceTranscript })).not.toBeInTheDocument();

    update(makeVoice({ state: 'listening', liveTranscript: 'what is the wea' }));
    expect(screen.getByRole('region', { name: t.chatVoiceTranscript })).toHaveTextContent(
      'what is the wea',
    );

    update(makeVoice({ state: 'listening', liveTranscript: 'what is the weather today' }));
    expect(screen.getByRole('region', { name: t.chatVoiceTranscript })).toHaveTextContent(
      'what is the weather today',
    );
  });

  it('keeps what was said on screen while the answer is written and spoken, with the answer', () => {
    // The thread gets the user's turn only when the gateway commits the turn, so until then
    // this is the one place the question is shown, beside the answer it is being given.
    const { update } = renderControl(
      makeVoice({
        state: 'thinking',
        liveTranscript: 'what is the weather today',
        answerPreview: 'It is sunny',
      }),
    );
    for (const state of ['thinking', 'speaking', 'draining'] as const) {
      update(
        makeVoice({
          state,
          liveTranscript: 'what is the weather today',
          answerPreview: 'It is sunny',
        }),
      );
      expect(screen.getByRole('region', { name: t.chatVoiceTranscript }), state).toHaveTextContent(
        'what is the weather today',
      );
      expect(screen.getByRole('region', { name: t.chatVoiceAnswer }), state).toHaveTextContent(
        'It is sunny',
      );
    }
  });

  it('shows no turn when the conversation is off', () => {
    renderControl(makeVoice({ state: 'off', liveTranscript: 'left over', answerPreview: 'x' }));
    expect(screen.queryByRole('region', { name: t.chatVoiceTranscript })).not.toBeInTheDocument();
    expect(screen.queryByRole('region', { name: t.chatVoiceAnswer })).not.toBeInTheDocument();
  });
});

describe('VoiceChatControl push-to-talk', () => {
  it('push-to-talk button calls pushStart/pushEnd', () => {
    const voice = makeVoice({ state: 'listening' });
    renderControl(voice);

    const talk = screen.getByRole('button', { name: t.chatVoicePushToTalk });
    fireEvent.pointerDown(talk);
    expect(voice.pushStart).toHaveBeenCalledTimes(1);
    expect(voice.pushEnd).not.toHaveBeenCalled();

    fireEvent.pointerUp(talk);
    expect(voice.pushEnd).toHaveBeenCalledTimes(1);
    expect(voice.pushStart).toHaveBeenCalledTimes(1);
  });

  it('ends the turn when the press is cancelled, once, and only for a press that began', () => {
    const voice = makeVoice({ state: 'listening' });
    renderControl(voice);
    const talk = screen.getByRole('button', { name: t.chatVoicePushToTalk });

    // A release with no press before it (the pointer went down elsewhere) is nothing.
    fireEvent.pointerUp(talk);
    expect(voice.pushEnd).not.toHaveBeenCalled();

    fireEvent.pointerDown(talk);
    fireEvent.pointerCancel(talk);
    fireEvent.pointerUp(talk);
    expect(voice.pushEnd).toHaveBeenCalledTimes(1);
  });

  it('works from the keyboard: Space held talks, released ends', () => {
    const voice = makeVoice({ state: 'listening' });
    renderControl(voice);
    const talk = screen.getByRole('button', { name: t.chatVoicePushToTalk });

    fireEvent.keyDown(talk, { key: ' ' });
    // The OS key repeat is not a new press.
    fireEvent.keyDown(talk, { key: ' ', repeat: true });
    expect(voice.pushStart).toHaveBeenCalledTimes(1);
    fireEvent.keyUp(talk, { key: ' ' });
    expect(voice.pushEnd).toHaveBeenCalledTimes(1);
  });

  it('re-engages from idle (after a failed turn)', () => {
    const voice = makeVoice({ state: 'idle' });
    renderControl(voice);
    const talk = screen.getByRole('button', { name: t.chatVoicePushToTalk });
    expect(talk).toBeEnabled();
    fireEvent.pointerDown(talk);
    expect(voice.pushStart).toHaveBeenCalledTimes(1);
  });

  it.each(['off', 'connecting', 'thinking', 'speaking', 'draining'] as const)(
    'is closed while the state is %s',
    (state) => {
      const voice = makeVoice({ state });
      renderControl(voice);
      const talk = screen.queryByRole('button', { name: t.chatVoicePushToTalk });
      // Off has no talk button at all; the others have it, closed (the gateway ignores it).
      if (talk) {
        expect(talk).toBeDisabled();
        fireEvent.pointerDown(talk);
      }
      expect(voice.pushStart).not.toHaveBeenCalled();
    },
  );

  it('offers to stop the answer while it is being prepared or spoken, and calls cancel', () => {
    const voice = makeVoice({ state: 'speaking' });
    const { update } = renderControl(voice);
    fireEvent.click(screen.getByRole('button', { name: t.chatVoiceCancel }));
    expect(voice.cancel).toHaveBeenCalledTimes(1);

    update(makeVoice({ state: 'listening' }));
    expect(screen.queryByRole('button', { name: t.chatVoiceCancel })).not.toBeInTheDocument();
  });
});

describe('VoiceChatControl errors', () => {
  it('shows a refused ticket by its gateway code, and holds the toggle until it is dismissed', () => {
    const voice = makeVoice({
      state: 'off',
      error: {
        kind: 'refused',
        code: 'portal.voice_chat_coload_failed',
        message: 'portal.voice_chat_coload_failed: the models cannot be held together',
      },
    });
    renderControl(voice);

    expect(screen.getByRole('alert')).toHaveTextContent(t.chatVoiceErrorCoLoad);
    // The same request would be refused again: the toggle says so instead of inviting a retry.
    expect(screen.getByRole('switch', { name: t.chatVoice })).toBeDisabled();

    // Dismissing clears the refusal (stop does that), which opens the toggle again.
    fireEvent.click(screen.getByRole('button', { name: t.chatVoiceDismiss }));
    expect(voice.stop).toHaveBeenCalledTimes(1);
  });

  it.each([
    ['portal.voice_chat_asr_unavailable', 'chatVoiceErrorAsrUnavailable'],
    ['portal.voice_chat_llm_unavailable', 'chatVoiceErrorLlmUnavailable'],
    ['portal.voice_chat_tts_unavailable', 'chatVoiceErrorTtsUnavailable'],
    ['portal.voice_chat_coload_failed', 'chatVoiceErrorCoLoad'],
    ['portal.voice_chat_audio_settings_invalid', 'chatVoiceErrorAudioSettings'],
    ['portal.voice_chat_ticket_failed', 'chatVoiceErrorTicket'],
    ['portal.chat_not_found', 'chatVoiceErrorRefused'],
  ] as const)('maps the refusal %s to its sentence', (code, key) => {
    renderControl(makeVoice({ error: { kind: 'refused', code, message: 'raw' } }));
    expect(screen.getByRole('alert')).toHaveTextContent(t[key]);
    // The raw English of the gateway is diagnostic, never the user's text.
    expect(screen.queryByText('raw')).not.toBeInTheDocument();
  });

  // The gateway's own 500 (portal.voice_chat_ticket_failed) reaches the control as a failure a
  // retry can get past ("dropped") that carries its code: it has a sentence of its own, and the
  // switch stays open. A "dropped" with no code (a lost connection) keeps the generic sentence.
  it('says "could not be started" for a dropped ticket that carries the gateway code, with the toggle open', () => {
    renderControl(
      makeVoice({
        error: { kind: 'dropped', code: 'portal.voice_chat_ticket_failed', message: 'raw' },
      }),
    );

    expect(screen.getByRole('alert')).toHaveTextContent(t.chatVoiceErrorTicket);
    expect(screen.getByRole('alert')).not.toHaveTextContent(t.chatVoiceErrorDropped);
    expect(screen.getByRole('switch', { name: t.chatVoice })).toBeEnabled();
  });

  it('keeps the generic sentence for a dropped connection, and for a dropped code it has no sentence for', () => {
    const { update } = renderControl(makeVoice({ error: { kind: 'dropped', message: 'raw' } }));
    expect(screen.getByRole('alert')).toHaveTextContent(t.chatVoiceErrorDropped);

    update(makeVoice({ error: { kind: 'dropped', code: 'proxy.bad_gateway', message: 'raw' } }));
    expect(screen.getByRole('alert')).toHaveTextContent(t.chatVoiceErrorDropped);
  });

  it('keeps the toggle open after a failure a retry can get past', () => {
    for (const kind of ['dropped', 'denied', 'unsupported', 'capture'] as const) {
      const voice = makeVoice({ error: { kind, message: 'raw' } });
      const { unmount } = renderControl(voice);
      expect(screen.getByRole('alert'), kind).toBeInTheDocument();
      expect(screen.getByRole('switch', { name: t.chatVoice }), kind).toBeEnabled();
      unmount();
    }
  });

  it('says what a failed turn means, with the conversation still open', () => {
    renderControl(
      makeVoice({
        state: 'idle',
        error: { kind: 'turn', code: 'speech.upstream_error', message: 'raw' },
      }),
    );
    expect(screen.getByRole('alert')).toHaveTextContent(t.chatVoiceErrorTurn);
    // The conversation is still on, so the toggle is on, and is the way out.
    expect(screen.getByRole('switch', { name: t.chatVoice })).toBeChecked();
  });

  // The failure's real reason (#329): the headline is the sentence alone, and the gateway's code and
  // message are behind a "Details" disclosure that is closed until it is asked for.
  describe('the failure details', () => {
    const turnError = {
      kind: 'turn',
      code: 'speech.upstream_error',
      message: 'the speech backend answered 502',
    } as const;
    const detailsToggle = () => screen.getByRole('button', { name: t.chatVoiceErrorDetails });
    // The diagnostic is two lines (the code, then the message): matched by its code, which is
    // enough to find it, and the whole text is asserted on the element found.
    const diagnostic = () => screen.getByText(/speech\.upstream_error/);

    it('keeps the code out of the headline and behind a closed Details toggle', () => {
      renderControl(makeVoice({ state: 'idle', error: turnError }));

      // The headline is the message div's own text: the sentence, with no "(code)" after it.
      expect(screen.getByText(t.chatVoiceErrorTurn)).toBeInTheDocument();
      const toggle = detailsToggle();
      expect(toggle).toHaveAttribute('aria-expanded', 'false');
      expect(diagnostic()).not.toBeVisible();
    });

    it('reveals the code and the message on click, and hides them again on a second', () => {
      renderControl(makeVoice({ state: 'idle', error: turnError }));
      const toggle = detailsToggle();

      fireEvent.click(toggle);
      expect(toggle).toHaveAttribute('aria-expanded', 'true');
      expect(diagnostic()).toBeVisible();
      expect(diagnostic()).toHaveTextContent(
        'speech.upstream_error the speech backend answered 502',
      );
      // The toggle names the region it opens.
      expect(toggle).toHaveAttribute('aria-controls', diagnostic().id);

      fireEvent.click(toggle);
      expect(toggle).toHaveAttribute('aria-expanded', 'false');
    });

    it('shows it for a refused ticket and a dropped connection as well, without the inline code', () => {
      const { update } = renderControl(
        makeVoice({
          error: { kind: 'refused', code: 'portal.chat_not_found', message: 'no such chat' },
        }),
      );
      // A refusal with no sentence of ours is the generic one, with no "(code)" after it.
      expect(screen.getByText(t.chatVoiceErrorRefused)).toBeInTheDocument();
      expect(detailsToggle()).toHaveAttribute('aria-expanded', 'false');

      update(makeVoice({ error: { kind: 'dropped', message: 'connection closed (1006)' } }));
      expect(screen.getByText(t.chatVoiceErrorDropped)).toBeInTheDocument();
      expect(detailsToggle()).toHaveAttribute('aria-expanded', 'false');
    });

    it('starts closed for each new error, not as the last one was left', () => {
      const { update } = renderControl(makeVoice({ state: 'idle', error: turnError }));
      fireEvent.click(detailsToggle());
      expect(detailsToggle()).toHaveAttribute('aria-expanded', 'true');

      update(
        makeVoice({
          state: 'idle',
          error: { kind: 'turn', code: 'speech.other_error', message: 'something else' },
        }),
      );
      expect(detailsToggle()).toHaveAttribute('aria-expanded', 'false');
    });

    it("renders no toggle for a failure that is the browser's own", () => {
      for (const kind of ['denied', 'unsupported', 'capture'] as const) {
        const { unmount } = renderControl(makeVoice({ error: { kind, message: 'raw' } }));
        expect(screen.getByRole('alert'), kind).toBeInTheDocument();
        expect(
          screen.queryByRole('button', { name: t.chatVoiceErrorDetails }),
          kind,
        ).not.toBeInTheDocument();
        unmount();
      }
    });

    it('renders no toggle for a failed turn that has only the generic fallback and no code', () => {
      renderControl(
        makeVoice({
          state: 'idle',
          error: { kind: 'turn', message: VOICE_TURN_FALLBACK_MESSAGE },
        }),
      );
      expect(screen.getByText(t.chatVoiceErrorTurn)).toBeInTheDocument();
      expect(
        screen.queryByRole('button', { name: t.chatVoiceErrorDetails }),
      ).not.toBeInTheDocument();
    });
  });

  it('shows no alert without an error', () => {
    renderControl(makeVoice({ state: 'listening' }));
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });
});

// The pickers of the conversation's models (#317): the transcription model and the speech model
// are the user's to pick (the chat model is the thread's own, only named). Each lists the models
// that are on offer plus "automatic", which is what a thread that has picked nothing is on.
describe('VoiceChatControl model pickers', () => {
  const asrPicker = () => screen.getByRole('combobox', { name: t.chatVoiceAsrModel });
  const ttsPicker = () => screen.getByRole('combobox', { name: t.chatVoiceTtsModel });
  const open = (picker: HTMLElement) => fireEvent.mouseDown(picker);
  const offered = () => screen.getAllByRole('option').map((option) => option.textContent);

  it('has one picker for the transcription model and one for the speech model', () => {
    renderControl(makeVoice());

    expect(asrPicker()).toBeInTheDocument();
    expect(ttsPicker()).toBeInTheDocument();
  });

  it('lists the automatic option and only the models of its own leg that are on offer', () => {
    renderControl(makeVoice());

    open(asrPicker());
    expect(offered()).toEqual([t.chatVoiceModelAuto, 'asr-a (display)', 'asr-b (display)']);
    fireEvent.keyDown(asrPicker(), { key: 'Escape' });

    open(ttsPicker());
    expect(offered()).toEqual([t.chatVoiceModelAuto, 'tts-a (display)', 'tts-clone (display)']);
  });

  it('shows automatic, and the model it would use, while nothing is picked', () => {
    renderControl(makeVoice(), {
      models: makeModels({ asr: makeChoice(['asr-a', 'asr-b'], { used: 'asr-b' }) }),
    });

    expect(asrPicker()).toHaveValue(t.chatVoiceModelAuto);
    // The line under it names the model that is actually used (by its display name).
    expect(screen.getByText(t.chatVoiceModelUsing('asr-b (display)'))).toBeInTheDocument();
  });

  it('shows the pick, and no "uses" line for a model the user chose', () => {
    renderControl(makeVoice(), {
      models: makeModels({ tts: makeChoice(['tts-a', 'tts-clone'], { pick: 'tts-clone' }) }),
    });

    expect(ttsPicker()).toHaveValue('tts-clone (display)');
    expect(
      screen.queryByText(t.chatVoiceModelUsing('tts-clone (display)')),
    ).not.toBeInTheDocument();
  });

  it('hands the model id of a chosen option to the store, for the leg it belongs to', () => {
    const models = makeModels();
    renderControl(makeVoice(), { models });

    open(asrPicker());
    fireEvent.click(screen.getByRole('option', { name: 'asr-b (display)' }));
    expect(models.asr.set).toHaveBeenCalledWith('asr-b');
    expect(models.tts.set).not.toHaveBeenCalled();

    open(ttsPicker());
    fireEvent.click(screen.getByRole('option', { name: 'tts-clone (display)' }));
    expect(models.tts.set).toHaveBeenCalledWith('tts-clone');
  });

  it('hands over an empty pick when automatic is chosen again', () => {
    const models = makeModels({ asr: makeChoice(['asr-a', 'asr-b'], { pick: 'asr-b' }) });
    renderControl(makeVoice(), { models });

    open(asrPicker());
    fireEvent.click(screen.getByRole('option', { name: t.chatVoiceModelAuto }));

    expect(models.asr.set).toHaveBeenCalledWith('');
  });

  it('is closed until the gateway has said which models stream', () => {
    renderControl(makeVoice(), { models: makeModels({ ready: false }) });

    expect(asrPicker()).toBeDisabled();
    expect(ttsPicker()).toBeDisabled();
  });

  it('is closed while a conversation is on: its ticket holds the models it started with', () => {
    const { update } = renderControl(makeVoice({ state: 'listening' }));
    expect(asrPicker()).toBeDisabled();
    expect(ttsPicker()).toBeDisabled();

    update(makeVoice({ state: 'off' }));
    expect(asrPicker()).toBeEnabled();
    expect(ttsPicker()).toBeEnabled();
  });

  it('is open on a switch that is closed for another reason, so the user can pick ahead', () => {
    renderControl(makeVoice(), { capable: false, gap: 'asr', busy: true });

    expect(asrPicker()).toBeEnabled();
    expect(ttsPicker()).toBeEnabled();
  });

  it("names the thread's own model as the chat model, without a picker for it", () => {
    renderControl(makeVoice());

    expect(screen.getByText(t.chatVoiceLlmNote('llm (display)'))).toBeInTheDocument();
    expect(screen.getAllByRole('combobox')).toHaveLength(2);
  });

  it('says nothing about a chat model when the thread has none', () => {
    renderControl(makeVoice(), { models: makeModels({ llm: '' }) });

    expect(screen.queryByText(/llm/)).not.toBeInTheDocument();
    expect(screen.queryByText(t.chatVoiceLlmNote(''))).not.toBeInTheDocument();
  });

  // The same green dot as the chat's own model picker: a model that is loaded on a server answers
  // at once, one that is not has to be loaded first, which is worth knowing before the pick.
  describe('loaded-model dot', () => {
    // The dot is the span with the title the option carries; the option's own text stays the bare
    // display name.
    const dotOf = (option: HTMLElement) => option.querySelector('span[aria-hidden][title]');
    const loadedAsr = makeChoice([], {
      options: [
        entry('asr-a'),
        entry('asr-b', { loaded: true, loaded_on: ['gpu-1', 'gpu-2'] }),
        entry('asr-c', { loaded: false, loaded_on: [] }),
      ],
      used: 'asr-b',
    });

    it('marks the loaded models of the transcription picker, and only those', () => {
      renderControl(makeVoice(), { models: makeModels({ asr: loadedAsr }) });

      open(asrPicker());
      const option = (name: string) => screen.getByRole('option', { name });
      expect(dotOf(option('asr-b (display)'))).toHaveAttribute(
        'title',
        `${t.chatModelLoadedOn}: gpu-1, gpu-2`,
      );
      expect(dotOf(option('asr-a (display)'))).toBeNull();
      expect(dotOf(option('asr-c (display)'))).toBeNull();
      expect(dotOf(option(t.chatVoiceModelAuto))).toBeNull();
    });

    it('marks the loaded models of the speech picker, with the plain title when no server is named', () => {
      const tts = makeChoice([], {
        options: [entry('tts-a'), entry('tts-clone', { loaded: true })],
      });
      renderControl(makeVoice(), { models: makeModels({ tts }) });

      open(ttsPicker());
      expect(dotOf(screen.getByRole('option', { name: 'tts-clone (display)' }))).toHaveAttribute(
        'title',
        t.chatModelLoaded,
      );
      expect(dotOf(screen.getByRole('option', { name: 'tts-a (display)' }))).toBeNull();
    });

    it('shows the dot in the field once a loaded model is picked, and in that picker only', () => {
      renderControl(makeVoice(), { models: makeModels({ asr: { ...loadedAsr, pick: 'asr-b' } }) });

      // The speech picker has no loaded model, so the one dot there is is the transcription one's.
      const dot = screen.getByTestId('searchable-select-loaded-dot');
      expect(asrPicker().closest('.MuiFormControl-root')).toContainElement(dot);
      expect(dot).toHaveAttribute('title', `${t.chatModelLoadedOn}: gpu-1, gpu-2`);
    });

    it('shows no dot in the field for a pick that is not loaded', () => {
      renderControl(makeVoice(), { models: makeModels({ asr: { ...loadedAsr, pick: 'asr-c' } }) });

      expect(screen.queryByTestId('searchable-select-loaded-dot')).not.toBeInTheDocument();
    });

    it('shows no dot at all while no model is loaded', () => {
      renderControl(makeVoice(), {
        models: makeModels({ asr: makeChoice(['asr-a', 'asr-b'], { pick: 'asr-a' }) }),
      });

      open(asrPicker());
      expect(screen.queryByTestId('searchable-select-loaded-dot')).not.toBeInTheDocument();
      expect(screen.getAllByRole('option').every((option) => dotOf(option) === null)).toBe(true);
    });
  });
});

// The voice of the speech model (#318): a combobox under the speech model's picker, over the voices
// the gateway has cached for it -- the composer's own field (SpeechVoiceField), shown only when
// the model has voices to offer.
describe('VoiceChatControl voice field', () => {
  const voiceField = () => screen.getByRole('combobox', { name: t.chatSpeechVoice });
  const queryVoiceField = () => screen.queryByRole('combobox', { name: t.chatSpeechVoice });
  const voices = ['alba', 'marius', 'zeta'];

  it('is not shown when the speech model has no voices: its own default voice speaks', () => {
    renderControl(makeVoice());

    expect(queryVoiceField()).not.toBeInTheDocument();
    expect(screen.queryByText(t.chatSpeechVoiceHint)).not.toBeInTheDocument();
  });

  it('lists the voices of the speech model, under its picker', () => {
    renderControl(makeVoice(), { models: makeModels({ voice: makeVoiceChoice(voices) }) });

    // After the speech model's picker, and so after the transcription one that precedes it.
    expect(screen.getAllByRole('combobox').map((box) => box.getAttribute('id'))).toEqual([
      'chat-voice-asr-model',
      'chat-voice-tts-model',
      'chat-voice-tts-voice',
    ]);

    fireEvent.mouseDown(voiceField());
    expect(screen.getAllByRole('option').map((option) => option.textContent)).toEqual(voices);
  });

  it('shows the voice that would be spoken in (the pre-selected preset of a clone, or the pick)', () => {
    renderControl(makeVoice(), {
      models: makeModels({ voice: makeVoiceChoice(voices, { value: 'alba' }) }),
    });

    expect(voiceField()).toHaveValue('alba');
  });

  it('hands a listed voice, and a typed one, to the store', () => {
    const voice = makeVoiceChoice(voices);
    renderControl(makeVoice(), { models: makeModels({ voice }) });

    fireEvent.mouseDown(voiceField());
    fireEvent.click(screen.getByRole('option', { name: 'marius' }));
    expect(voice.set).toHaveBeenLastCalledWith('marius');

    fireEvent.change(voiceField(), { target: { value: 'my_voice' } });
    expect(voice.set).toHaveBeenLastCalledWith('my_voice');
  });

  it('is closed while a conversation is on, and until the gateway has said which models stream', () => {
    const models = makeModels({ voice: makeVoiceChoice(voices, { value: 'alba' }) });
    const { update } = renderControl(makeVoice({ state: 'listening' }), { models });
    expect(voiceField()).toBeDisabled();

    update(makeVoice({ state: 'off' }));
    expect(voiceField()).toBeEnabled();

    cleanup();
    renderControl(makeVoice(), { models: { ...models, ready: false } });
    expect(voiceField()).toBeDisabled();
  });
});

// The voice system prompt (#314): a multi-line field beside the pickers, the thread's own, shown
// pre-filled with the text a run would carry. Unlike the pickers it does not wait for the gateway
// to say which models stream (the prompt has nothing to do with them), but like them it is closed
// while a conversation is on.
describe('VoiceChatControl system prompt field', () => {
  const promptField = () => screen.getByRole('textbox', { name: t.chatVoiceSystemPrompt });
  const queryPromptField = () => screen.queryByRole('textbox', { name: t.chatVoiceSystemPrompt });
  // The field sits in a section that is collapsed until its heading is clicked (like the thread's
  // settings); the tests that are about the field open it first.
  const toggle = () => screen.getByRole('button', { name: t.chatVoiceSystemPrompt });
  const openPrompt = () => fireEvent.click(toggle());
  const renderOpen = (...args: Parameters<typeof renderControl>) => {
    const view = renderControl(...args);
    openPrompt();
    return view;
  };

  describe('collapsed section', () => {
    it('is collapsed by default: neither the field nor its reset are shown', () => {
      renderControl(makeVoice());

      expect(toggle()).toHaveAttribute('aria-expanded', 'false');
      expect(queryPromptField()).not.toBeInTheDocument();
      expect(
        screen.queryByRole('button', { name: t.chatVoiceSystemPromptReset }),
      ).not.toBeInTheDocument();
      expect(screen.queryByText(t.chatVoiceSystemPromptHint)).not.toBeInTheDocument();
    });

    it('expands on a click, and collapses again on the next', () => {
      renderControl(makeVoice(), { models: makeModels({ systemPrompt: makePromptChoice('Hi.') }) });

      openPrompt();
      expect(toggle()).toHaveAttribute('aria-expanded', 'true');
      expect(promptField()).toHaveValue('Hi.');
      expect(
        screen.getByRole('button', { name: t.chatVoiceSystemPromptReset }),
      ).toBeInTheDocument();

      openPrompt();
      expect(toggle()).toHaveAttribute('aria-expanded', 'false');
      expect(queryPromptField()).not.toBeInTheDocument();
    });

    it('can be opened on a switch that is closed, and before the gateway has said anything', () => {
      renderControl(makeVoice(), {
        models: makeModels({ ready: false }),
        capable: false,
        gap: 'asr',
        busy: true,
      });

      expect(toggle()).toBeEnabled();
    });
  });

  it('shows the prompt field pre-filled and calls set on edit', () => {
    const systemPrompt = makePromptChoice('Speak briefly.');
    renderOpen(makeVoice(), { models: makeModels({ systemPrompt }) });

    expect(promptField()).toHaveValue('Speak briefly.');
    expect(promptField().tagName).toBe('TEXTAREA');

    fireEvent.change(promptField(), { target: { value: 'Be terse.\nNo lists.' } });
    expect(systemPrompt.set).toHaveBeenCalledWith('Be terse.\nNo lists.');
    expect(systemPrompt.set).toHaveBeenCalledTimes(1);
  });

  it('says what the prompt is for and what an empty one means', () => {
    renderOpen(makeVoice());

    expect(screen.getByText(t.chatVoiceSystemPromptHint)).toBeInTheDocument();
  });

  it('disables the prompt field while the conversation is live (locked)', () => {
    const { update } = renderOpen(makeVoice({ state: 'listening' }));
    expect(promptField()).toBeDisabled();

    update(makeVoice({ state: 'off' }));
    expect(promptField()).toBeEnabled();
  });

  it('is open before the gateway has said which models stream, and on a switch closed for another reason', () => {
    renderOpen(makeVoice(), {
      models: makeModels({ ready: false }),
      capable: false,
      gap: 'asr',
      busy: true,
    });

    expect(promptField()).toBeEnabled();
  });

  // The prompt is the one free-text field of the control: a paste of a whole document would be
  // carried into every run of the conversation, so the field is bounded.
  it('bounds the length of the prompt', () => {
    renderOpen(makeVoice());

    expect(promptField()).toHaveAttribute('maxlength', '4000');
  });

  describe('reset to default', () => {
    const resetButton = () => screen.getByRole('button', { name: t.chatVoiceSystemPromptReset });

    it('puts the built-in prompt back into the field', () => {
      const systemPrompt = makePromptChoice('Be terse.');
      renderOpen(makeVoice(), { models: makeModels({ systemPrompt }) });

      fireEvent.click(resetButton());

      expect(systemPrompt.set).toHaveBeenCalledTimes(1);
      expect(systemPrompt.set).toHaveBeenCalledWith(DEFAULT_VOICE_SYSTEM_PROMPT);
    });

    it('is open for an emptied field too: the default is what the empty field means, spelled out', () => {
      const systemPrompt = makePromptChoice('');
      renderOpen(makeVoice(), { models: makeModels({ systemPrompt }) });

      expect(resetButton()).toBeEnabled();
      fireEvent.click(resetButton());

      expect(systemPrompt.set).toHaveBeenCalledWith(DEFAULT_VOICE_SYSTEM_PROMPT);
    });

    it('is closed while the field already holds the built-in prompt: there is nothing to reset', () => {
      renderOpen(makeVoice(), {
        models: makeModels({ systemPrompt: makePromptChoice(DEFAULT_VOICE_SYSTEM_PROMPT) }),
      });

      expect(resetButton()).toBeDisabled();
    });

    it('is closed while the conversation is live, like the field it resets', () => {
      const systemPrompt = makePromptChoice('Be terse.');
      const models = makeModels({ systemPrompt });
      const { update } = renderOpen(makeVoice({ state: 'listening' }), { models });
      expect(resetButton()).toBeDisabled();

      fireEvent.click(resetButton());
      expect(systemPrompt.set).not.toHaveBeenCalled();

      update(makeVoice({ state: 'off' }));
      expect(resetButton()).toBeEnabled();
    });

    it('is open before the gateway has said which models stream, like the field', () => {
      renderOpen(makeVoice(), {
        models: makeModels({ ready: false, systemPrompt: makePromptChoice('Be terse.') }),
      });

      expect(resetButton()).toBeEnabled();
    });
  });
});

// The system prompt section is collapsed by default, which would hide that the prompt is not the
// built-in one any more (#326): its toggle carries a small "edited" marker while the section is shut
// and the prompt is the thread's own. "Its own" is decided as the store decides what to persist
// (voiceChatSetting): the trimmed prompt, and a blank one is the built-in prompt again.
describe.each(['en', 'de'] as const satisfies readonly Locale[])(
  'VoiceChatControl customised prompt marker (%s)',
  (locale) => {
    const lt = messages[locale];
    const toggle = () => screen.getByRole('button', { name: lt.chatVoiceSystemPrompt });
    const marker = () => screen.queryByText(lt.chatVoiceSystemPromptEdited);
    const controlWith = (value: string) => (
      <VoiceChatControl
        t={lt}
        voice={makeVoice()}
        capable
        gap={null}
        models={makeModels({ systemPrompt: makePromptChoice(value) })}
      />
    );
    const renderPrompt = (value: string) => render(controlWith(value));

    it('says the prompt is edited on the collapsed toggle, without expanding it', () => {
      renderPrompt('Be terse.');

      expect(toggle()).toHaveAttribute('aria-expanded', 'false');
      expect(marker()).toBeInTheDocument();
      // The marker is the toggle's own: it is inside the button, and the button is described by
      // it, so a screen reader says it too -- without it becoming part of the button's name.
      expect(toggle()).toContainElement(marker());
      expect(toggle()).toHaveAccessibleDescription(lt.chatVoiceSystemPromptEdited);
      expect(toggle()).toHaveAccessibleName(lt.chatVoiceSystemPrompt);
    });

    it('shows no marker for the built-in prompt', () => {
      renderPrompt(DEFAULT_VOICE_SYSTEM_PROMPT);

      expect(marker()).not.toBeInTheDocument();
      expect(toggle()).not.toHaveAccessibleDescription();
    });

    it('shows no marker for the built-in prompt that differs only by surrounding whitespace', () => {
      renderPrompt(`\n  ${DEFAULT_VOICE_SYSTEM_PROMPT} \n`);

      expect(marker()).not.toBeInTheDocument();
    });

    // The store persists no prompt for a blank field and the backend runs the built-in one, as the
    // field's own hint says ("leave empty to use the default"): nothing is customised.
    it.each(['', '   \n\t '])('shows no marker for a blank prompt (%j)', (blank) => {
      renderPrompt(blank);

      expect(marker()).not.toBeInTheDocument();
    });

    it('follows the prompt: it appears on an edit and goes when the built-in prompt is back', () => {
      const view = renderPrompt(DEFAULT_VOICE_SYSTEM_PROMPT);
      expect(marker()).not.toBeInTheDocument();

      view.rerender(controlWith('Be terse.'));
      expect(marker()).toBeInTheDocument();

      view.rerender(controlWith(DEFAULT_VOICE_SYSTEM_PROMPT));
      expect(marker()).not.toBeInTheDocument();
    });

    // Expanded, the field itself is on screen: the marker is only for the shut section, and it
    // must not appear and disappear under the user's cursor while they type.
    it('is only for the collapsed section: it goes while the section is open', () => {
      renderPrompt('Be terse.');
      expect(marker()).toBeInTheDocument();

      fireEvent.click(toggle());
      expect(toggle()).toHaveAttribute('aria-expanded', 'true');
      expect(marker()).not.toBeInTheDocument();
      expect(toggle()).not.toHaveAccessibleDescription();

      fireEvent.click(toggle());
      expect(marker()).toBeInTheDocument();
    });

    it('marks an edited prompt while a conversation is on, too: the toggle stays readable', () => {
      renderControl(makeVoice({ state: 'listening' }), {
        t: lt,
        models: makeModels({ systemPrompt: makePromptChoice('Be terse.') }),
      });

      expect(marker()).toBeInTheDocument();
      expect(toggle()).toBeEnabled();
    });
  },
);

// The controls of the answer being spoken, and the two mutes: the user's own microphone (#315) and
// the answer's speakers (#326, always on, beside it). Every label is read from the locale under
// test, so a control missing its words in either one fails.
describe.each(['en', 'de'] as const satisfies readonly Locale[])(
  'VoiceChatControl playback controls and the two mutes (%s)',
  (locale) => {
    const lt = messages[locale];
    const renderLocale = (voice: VoiceChatApi) => renderControl(voice, { t: lt });
    const playbackGroup = () => screen.queryByRole('group', { name: lt.chatVoicePlayback });
    const playing = (over: Partial<VoiceChatApi> = {}) =>
      makeVoice({ state: 'speaking', audioPlaying: true, ...over });

    describe('the playback controls', () => {
      it('appear while the answer is playing: pause and stop, and not the mute, which is always on', () => {
        renderLocale(playing());

        const group = within(playbackGroup()!);
        expect(group.getByRole('button', { name: lt.chatVoicePause })).toBeEnabled();
        expect(group.getByRole('button', { name: lt.chatVoiceStopPlayback })).toBeEnabled();
        // Not the resume of a pause that is not there.
        expect(group.queryByRole('button', { name: lt.chatVoiceResume })).not.toBeInTheDocument();
        // The mute of the answer is on screen, but it is not one of these: it is there when they
        // are not (the output mute's own tests).
        expect(group.queryByRole('button', { name: lt.chatVoiceMute })).toBeNull();
        expect(group.queryByRole('button', { name: lt.chatVoiceUnmute })).toBeNull();
        expect(screen.getByRole('button', { name: lt.chatVoiceMute })).toBeInTheDocument();
      });

      it('appear in the tail of the answer that is still playing out (draining), too', () => {
        renderLocale(playing({ state: 'draining' }));

        expect(
          within(playbackGroup()!).getByRole('button', { name: lt.chatVoicePause }),
        ).toBeInTheDocument();
      });

      it('pause calls voice.pause, and only that', () => {
        const voice = playing();
        renderLocale(voice);

        fireEvent.click(screen.getByRole('button', { name: lt.chatVoicePause }));

        expect(voice.pause).toHaveBeenCalledTimes(1);
        expect(voice.resume).not.toHaveBeenCalled();
        expect(voice.stopSpeaking).not.toHaveBeenCalled();
      });

      it('a paused answer offers resume instead of pause, and resume calls voice.resume', () => {
        const voice = playing({ paused: true });
        renderLocale(voice);

        expect(screen.queryByRole('button', { name: lt.chatVoicePause })).not.toBeInTheDocument();
        fireEvent.click(screen.getByRole('button', { name: lt.chatVoiceResume }));

        expect(voice.resume).toHaveBeenCalledTimes(1);
        expect(voice.pause).not.toHaveBeenCalled();
      });

      it('says that the answer is paused in the indicator, and keeps the controls', () => {
        const { update } = renderLocale(playing());
        expect(within(indicatorOf(lt)).getByText(lt.chatVoiceStateSpeaking)).toBeInTheDocument();

        update(playing({ paused: true }));

        expect(within(indicatorOf(lt)).getByText(lt.chatVoiceStatePaused)).toBeInTheDocument();
        expect(screen.queryByText(lt.chatVoiceStateSpeaking)).not.toBeInTheDocument();
        expect(playbackGroup()).toBeInTheDocument();
      });

      it('stop cuts the spoken answer short (voice.stopSpeaking): it does not end the conversation, nor cancel the turn', () => {
        const voice = playing();
        renderLocale(voice);

        fireEvent.click(screen.getByRole('button', { name: lt.chatVoiceStopPlayback }));

        expect(voice.stopSpeaking).toHaveBeenCalledTimes(1);
        // voice.stop is the whole conversation's switch, and cancel is the other control.
        expect(voice.stop).not.toHaveBeenCalled();
        expect(voice.cancel).not.toHaveBeenCalled();
      });

      it('keep the control that cancels the turn, which is another thing than stopping the playback', () => {
        const voice = playing();
        renderLocale(voice);

        fireEvent.click(screen.getByRole('button', { name: lt.chatVoiceCancel }));

        expect(voice.cancel).toHaveBeenCalledTimes(1);
        expect(voice.stopSpeaking).not.toHaveBeenCalled();
      });

      it.each(['off', 'connecting', 'listening', 'thinking', 'idle'] as const)(
        'are not there while the state is %s, whatever the audio says',
        (state) => {
          renderLocale(makeVoice({ state, audioPlaying: true }));

          expect(playbackGroup()).not.toBeInTheDocument();
          expect(screen.queryByRole('button', { name: lt.chatVoicePause })).not.toBeInTheDocument();
          expect(
            screen.queryByRole('button', { name: lt.chatVoiceStopPlayback }),
          ).not.toBeInTheDocument();
        },
      );

      it.each(['speaking', 'draining'] as const)(
        'are not there while %s without audio being heard: before the first chunk, or after it played out',
        (state) => {
          renderLocale(makeVoice({ state, audioPlaying: false }));

          expect(playbackGroup()).not.toBeInTheDocument();
          expect(screen.queryByRole('button', { name: lt.chatVoicePause })).not.toBeInTheDocument();
          expect(
            screen.queryByRole('button', { name: lt.chatVoiceStopPlayback }),
          ).not.toBeInTheDocument();
          // The mute of the answer does not wait for audio: it can be set ahead of it.
          expect(screen.getByRole('button', { name: lt.chatVoiceMute })).toBeInTheDocument();
        },
      );

      it('go away when the answer has played out', () => {
        const { update } = renderLocale(playing({ state: 'draining' }));
        expect(playbackGroup()).toBeInTheDocument();

        update(makeVoice({ state: 'draining', audioPlaying: false }));

        expect(playbackGroup()).not.toBeInTheDocument();
      });
    });

    // Every state of a conversation that is on, with and without audio being heard: the mute of the
    // answer and the room of the playback controls must not depend on either.
    const onStates = [
      'connecting',
      'listening',
      'thinking',
      'speaking',
      'draining',
      'idle',
    ] as const;
    const onStatesByAudio = onStates.flatMap((state) => [
      [state, false],
      [state, true],
    ]) as ReadonlyArray<readonly [(typeof onStates)[number], boolean]>;

    describe('the output mute', () => {
      const outputMute = () => screen.getByRole('button', { name: lt.chatVoiceMute });

      it.each(onStatesByAudio)(
        'is there whenever the conversation is on (%s, audio heard: %s), and calls voice.toggleMute',
        (state, audioPlaying) => {
          const voice = makeVoice({ state, audioPlaying });
          renderLocale(voice);

          expect(outputMute()).toBeEnabled();
          // One signal of its state, the label: no aria-pressed beside it.
          expect(outputMute()).not.toHaveAttribute('aria-pressed');
          fireEvent.click(outputMute());

          expect(voice.toggleMute).toHaveBeenCalledTimes(1);
          // The mute of the answer is not the mute of the microphone, nor any other control.
          expect(voice.toggleMicMute).not.toHaveBeenCalled();
          expect(voice.stop).not.toHaveBeenCalled();
          expect(voice.stopSpeaking).not.toHaveBeenCalled();
          expect(voice.pause).not.toHaveBeenCalled();
        },
      );

      it('is not there while the conversation is off', () => {
        renderLocale(makeVoice({ state: 'off', audioPlaying: true }));

        expect(screen.queryByRole('button', { name: lt.chatVoiceMute })).not.toBeInTheDocument();
        expect(screen.queryByRole('button', { name: lt.chatVoiceUnmute })).not.toBeInTheDocument();
      });

      it.each(onStates)(
        'reflects the mute in %s: it reads unmute, with the silent speaker, and calls toggleMute again',
        (state) => {
          const voice = makeVoice({ state, muted: true });
          renderLocale(voice);

          expect(screen.queryByRole('button', { name: lt.chatVoiceMute })).not.toBeInTheDocument();
          const unmute = screen.getByRole('button', { name: lt.chatVoiceUnmute });
          expect(unmute).not.toHaveAttribute('aria-pressed');
          // The state is on screen between answers, not only in the words: the icon says it too.
          expect(within(unmute).getByTestId('VolumeOffIcon')).toBeInTheDocument();
          fireEvent.click(unmute);

          expect(voice.toggleMute).toHaveBeenCalledTimes(1);
          expect(voice.toggleMicMute).not.toHaveBeenCalled();
        },
      );

      it('shows the live speaker while the answer is not muted', () => {
        renderLocale(makeVoice({ state: 'idle' }));

        expect(within(outputMute()).getByTestId('VolumeUpIcon')).toBeInTheDocument();
        expect(within(outputMute()).queryByTestId('VolumeOffIcon')).not.toBeInTheDocument();
      });

      it('can be set ahead of the first answer: it reads unmute before any audio has been heard', () => {
        const { update } = renderLocale(makeVoice({ state: 'listening' }));
        expect(playbackGroup()).not.toBeInTheDocument();
        fireEvent.click(outputMute());

        const muted = makeVoice({ state: 'listening', muted: true });
        update(muted);

        expect(screen.getByRole('button', { name: lt.chatVoiceUnmute })).toBeInTheDocument();
        // Its mute holds when the first answer comes: still reads unmute, now beside the playback.
        update(makeVoice({ state: 'speaking', audioPlaying: true, muted: true }));
        expect(screen.getByRole('button', { name: lt.chatVoiceUnmute })).toBeInTheDocument();
        expect(playbackGroup()).toBeInTheDocument();
      });

      it('sits beside the microphone mute, in the row that is there whenever the conversation is on', () => {
        renderLocale(makeVoice({ state: 'idle' }));

        const micMute = screen.getByRole('button', { name: lt.chatVoiceMicMute });
        expect(outputMute().parentElement).toBe(micMute.parentElement);
        // The same row as push-to-talk: it is not a control of the playback group or of its room.
        expect(outputMute().parentElement).toBe(
          screen.getByRole('button', { name: lt.chatVoicePushToTalk }).parentElement,
        );
      });

      it('is not one of the playback controls: it is outside their group and their room', () => {
        renderLocale(makeVoice({ state: 'speaking', audioPlaying: true }));

        expect(
          within(playbackGroup()!).queryByRole('button', { name: lt.chatVoiceMute }),
        ).toBeNull();
        expect(screen.getByTestId(PLAYBACK_ROOM).contains(outputMute())).toBe(false);
      });
    });

    // The playback controls come and go with the audio, and the room they occupy does not: the
    // surrounding layout (what is under them, the error, the indicator above) must not jump. The
    // room only keeps that LAYOUT still. Pause and stop themselves still unmount when the audio
    // ends, and a focus on one of them goes with it; what stays put are the controls of the row
    // above the room, the output mute among them, which the playback coming and going never remounts
    // or moves, so their focus (and a press under the finger) is not lost.
    describe('the room the playback controls keep', () => {
      const room = () => screen.getByTestId(PLAYBACK_ROOM);

      it.each(onStates)(
        'is reserved while the conversation is on (%s), with no playback controls in it',
        (state) => {
          renderLocale(makeVoice({ state, audioPlaying: false }));

          expect(room()).toBeInTheDocument();
          expect(room()).toBeEmptyDOMElement();
          expect(playbackGroup()).not.toBeInTheDocument();
        },
      );

      it('is not reserved while the conversation is off: there is no row of controls to keep', () => {
        renderLocale(makeVoice({ state: 'off' }));

        expect(screen.queryByTestId(PLAYBACK_ROOM)).not.toBeInTheDocument();
      });

      it('holds the playback controls while the answer is heard, and is still the same element', () => {
        const { update } = renderLocale(makeVoice({ state: 'thinking' }));
        const before = room();

        update(playing());
        expect(room()).toBe(before);
        expect(room()).toContainElement(playbackGroup());

        update(makeVoice({ state: 'draining', audioPlaying: false }));
        expect(room()).toBe(before);
        expect(room()).toBeEmptyDOMElement();
      });

      it('has the height of a row of the controls whether or not they are drawn, so nothing moves', () => {
        const { update } = renderLocale(makeVoice({ state: 'thinking' }));
        expect(room()).toHaveStyle({ minHeight: PLAYBACK_ROOM_MIN_HEIGHT });

        update(playing());
        expect(room()).toHaveStyle({ minHeight: PLAYBACK_ROOM_MIN_HEIGHT });
      });

      it('leaves the row above it and the controls in it where they are while the playback comes and goes', () => {
        const { update } = renderLocale(makeVoice({ state: 'thinking' }));
        const talk = screen.getByRole('button', { name: lt.chatVoicePushToTalk });
        const micMute = screen.getByRole('button', { name: lt.chatVoiceMicMute });
        const outputMute = screen.getByRole('button', { name: lt.chatVoiceMute });
        const indicator = indicatorOf(lt);
        const above = room().previousElementSibling;

        update(playing());
        update(makeVoice({ state: 'draining', audioPlaying: false }));

        // The very same nodes, in the same order: React did not remount or move any of them.
        expect(screen.getByRole('button', { name: lt.chatVoicePushToTalk })).toBe(talk);
        expect(screen.getByRole('button', { name: lt.chatVoiceMicMute })).toBe(micMute);
        expect(screen.getByRole('button', { name: lt.chatVoiceMute })).toBe(outputMute);
        expect(indicatorOf(lt)).toBe(indicator);
        expect(room().previousElementSibling).toBe(above);
      });

      it('does not take the focus from the output mute when the answer starts and ends', () => {
        const { update } = renderLocale(makeVoice({ state: 'thinking' }));
        screen.getByRole('button', { name: lt.chatVoiceMute }).focus();
        expect(screen.getByRole('button', { name: lt.chatVoiceMute })).toHaveFocus();

        update(playing());
        expect(screen.getByRole('button', { name: lt.chatVoiceMute })).toHaveFocus();

        update(makeVoice({ state: 'draining', audioPlaying: false }));
        expect(screen.getByRole('button', { name: lt.chatVoiceMute })).toHaveFocus();
      });
    });

    describe('the microphone mute', () => {
      const micMute = () => screen.getByRole('button', { name: lt.chatVoiceMicMute });

      it.each(['connecting', 'listening', 'thinking', 'speaking', 'draining', 'idle'] as const)(
        'is there whenever the conversation is on (%s), and calls voice.toggleMicMute',
        (state) => {
          const voice = makeVoice({ state });
          renderLocale(voice);

          expect(micMute()).toBeEnabled();
          // One signal of its state, the label: no aria-pressed beside it.
          expect(micMute()).not.toHaveAttribute('aria-pressed');
          fireEvent.click(micMute());

          expect(voice.toggleMicMute).toHaveBeenCalledTimes(1);
          expect(voice.toggleMute).not.toHaveBeenCalled();
          expect(voice.stop).not.toHaveBeenCalled();
        },
      );

      it('is not there while the conversation is off', () => {
        renderLocale(makeVoice({ state: 'off' }));

        expect(screen.queryByRole('button', { name: lt.chatVoiceMicMute })).not.toBeInTheDocument();
        expect(
          screen.queryByRole('button', { name: lt.chatVoiceMicUnmute }),
        ).not.toBeInTheDocument();
      });

      it('reflects the mute: it reads unmute, and calls toggleMicMute again', () => {
        const voice = makeVoice({ state: 'listening', micMuted: true });
        renderLocale(voice);

        expect(screen.queryByRole('button', { name: lt.chatVoiceMicMute })).not.toBeInTheDocument();
        const unmute = screen.getByRole('button', { name: lt.chatVoiceMicUnmute });
        expect(unmute).not.toHaveAttribute('aria-pressed');
        fireEvent.click(unmute);

        expect(voice.toggleMicMute).toHaveBeenCalledTimes(1);
      });

      it('stays while the answer is spoken, next to the playback controls', () => {
        renderLocale(playing());

        expect(micMute()).toBeInTheDocument();
        expect(playbackGroup()).toBeInTheDocument();
        // It is not one of the playback controls: it is there when they are not.
        expect(
          within(playbackGroup()!).queryByRole('button', { name: lt.chatVoiceMicMute }),
        ).toBeNull();
      });

      it('closes push-to-talk while the microphone is muted: nothing would be heard, so nothing is started', () => {
        const voice = makeVoice({ state: 'listening', micMuted: true });
        renderLocale(voice);

        const talk = screen.getByRole('button', { name: lt.chatVoicePushToTalk });
        expect(talk).toBeDisabled();
        fireEvent.pointerDown(talk);
        fireEvent.keyDown(talk, { key: ' ' });
        expect(voice.pushStart).not.toHaveBeenCalled();
      });

      it('closes the re-engaging push-to-talk of an idle conversation too, until it is unmuted', () => {
        const { update } = renderLocale(makeVoice({ state: 'idle', micMuted: true }));
        expect(screen.getByRole('button', { name: lt.chatVoicePushToTalk })).toBeDisabled();

        const voice = makeVoice({ state: 'idle', micMuted: false });
        update(voice);
        const talk = screen.getByRole('button', { name: lt.chatVoicePushToTalk });
        expect(talk).toBeEnabled();
        fireEvent.pointerDown(talk);
        expect(voice.pushStart).toHaveBeenCalledTimes(1);
      });

      it('says in the indicator that the user is not heard: no hot microphone, and why', () => {
        const { update } = renderLocale(makeVoice({ state: 'listening' }));
        expect(within(indicatorOf(lt)).getByText(lt.chatVoiceStateListening)).toBeInTheDocument();
        expect(within(indicatorOf(lt)).getByTestId('MicIcon')).toBeInTheDocument();

        update(makeVoice({ state: 'listening', micMuted: true }));

        const status = within(indicatorOf(lt));
        // A muted microphone is never labelled "listening", whatever the gateway is doing.
        expect(status.getByText(lt.chatVoiceStateMicMuted)).toBeInTheDocument();
        expect(status.getByText(lt.chatVoiceMicMutedDetail)).toBeInTheDocument();
        expect(status.getByTestId('MicOffIcon')).toBeInTheDocument();
        expect(status.queryByTestId('MicIcon')).not.toBeInTheDocument();
        expect(screen.queryByText(lt.chatVoiceStateListening)).not.toBeInTheDocument();
      });

      it('keeps the state of the conversation as the label where it is not listening, and adds the mute as the detail', () => {
        renderLocale(makeVoice({ state: 'thinking', micMuted: true }));

        const status = within(indicatorOf(lt));
        expect(status.getByText(lt.chatVoiceStateThinking)).toBeInTheDocument();
        expect(status.getByText(lt.chatVoiceMicMutedDetail)).toBeInTheDocument();
      });

      it('leaves the "microphone is paused" detail while the answer is spoken: it is true either way', () => {
        renderLocale(playing({ micMuted: true }));

        expect(within(indicatorOf(lt)).getByText(lt.chatVoiceMicPaused)).toBeInTheDocument();
        expect(screen.queryByText(lt.chatVoiceMicMutedDetail)).not.toBeInTheDocument();
      });

      it('says nothing of a mute that is not there', () => {
        renderLocale(makeVoice({ state: 'listening' }));

        expect(screen.queryByText(lt.chatVoiceMicMutedDetail)).not.toBeInTheDocument();
        expect(screen.queryByText(lt.chatVoiceStateMicMuted)).not.toBeInTheDocument();
      });
    });
  },
);

describe('the words of the playback controls and the microphone mute', () => {
  const keys = [
    'chatVoicePlayback',
    'chatVoicePause',
    'chatVoiceResume',
    'chatVoiceMute',
    'chatVoiceUnmute',
    'chatVoiceStopPlayback',
    'chatVoiceMicMute',
    'chatVoiceMicUnmute',
    'chatVoiceStatePaused',
    'chatVoiceStateMicMuted',
    'chatVoiceMicMutedDetail',
  ] as const;

  it.each(keys)('%s is worded in both languages, and in each its own', (key) => {
    expect(messages.de[key].length).toBeGreaterThan(0);
    expect(messages.en[key].length).toBeGreaterThan(0);
    expect(messages.de[key]).not.toBe(messages.en[key]);
  });

  // Cancel (the whole answer) and Stop (its speech) are on screen together while the answer is
  // spoken, and one abandons what the other only silences: the words must not read as synonyms.
  it('words cancelling the whole answer and stopping its speech apart: no word in common', () => {
    for (const locale of ['en', 'de'] as const) {
      const words = (text: string) => new Set(text.toLowerCase().split(/[\s-]+/));
      const cancel = words(messages[locale].chatVoiceCancel);
      const stop = words(messages[locale].chatVoiceStopPlayback);
      expect(
        [...cancel].filter((word) => stop.has(word)),
        locale,
      ).toEqual([]);
    }
  });

  it('gives every control of one row a name of its own, so a screen reader tells them apart', () => {
    for (const locale of ['en', 'de'] as const) {
      const names = [
        messages[locale].chatVoicePause,
        messages[locale].chatVoiceResume,
        messages[locale].chatVoiceMute,
        messages[locale].chatVoiceUnmute,
        messages[locale].chatVoiceStopPlayback,
        messages[locale].chatVoiceMicMute,
        messages[locale].chatVoiceMicUnmute,
        messages[locale].chatVoiceCancel,
        messages[locale].chatVoicePushToTalk,
      ];
      expect(new Set(names).size, locale).toBe(names.length);
    }
  });
});
