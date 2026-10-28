// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { VoiceChatControl, type VoiceChatControlProps } from './VoiceChatControl';
import { messages } from '../i18n';
import type { ModelOption } from '../api';
import type {
  VoiceChatApi,
  VoiceChatModels,
  VoiceChatState,
  VoiceChoice,
  VoiceModelChoice,
} from './chat/ChatStore';

afterEach(cleanup);

const t = messages.en;

// The hook's output, with every control a spy: this component only draws the conversation and
// forwards the user's gestures (useVoiceChat.test.ts is about the conversation itself).
function makeVoice(over: Partial<VoiceChatApi> = {}): VoiceChatApi {
  return {
    state: 'off',
    liveTranscript: '',
    answerPreview: '',
    error: null,
    start: vi.fn(),
    stop: vi.fn(),
    pushStart: vi.fn(),
    pushEnd: vi.fn(),
    cancel: vi.fn(),
    ...over,
  };
}

// A catalogue entry of an audio model, named for display (the picker shows the display name).
const entry = (id: string): ModelOption => ({
  id,
  display_name: `${id} (display)`,
  flavors: ['openai_audio'],
  loading_on_count: 0,
});

function makeChoice(ids: string[], over: Partial<VoiceModelChoice> = {}): VoiceModelChoice {
  return { options: ids.map(entry), pick: '', used: ids[0] ?? '', set: vi.fn(), ...over };
}

// The speech model's voice field: the voices it offers and the one that would be spoken in.
function makeVoiceChoice(voices: string[] = [], over: Partial<VoiceChoice> = {}): VoiceChoice {
  return { voices, value: '', set: vi.fn(), ...over };
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
  return screen.getByRole('status', { name: t.chatVoiceIndicator });
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
    expect(screen.getByRole('alert')).toHaveTextContent('speech.upstream_error');
    // The conversation is still on, so the toggle is on, and is the way out.
    expect(screen.getByRole('switch', { name: t.chatVoice })).toBeChecked();
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
