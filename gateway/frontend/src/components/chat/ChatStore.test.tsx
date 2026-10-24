// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi, type Mock } from 'vitest';

import { memo } from 'react';
import {
  ChatStoreProvider,
  ChatStoreContext,
  ChatStreamingContext,
  useChatStore,
  useChatStreaming,
  type ChatStore,
  type ChatUiMessage,
} from './ChatStore';
import { ToastProvider } from '../shared/ToastProvider';
import { formatPortalError } from '../shared/format';
import { AudioDecodeError, audioBlobToWav } from '../shared/audioToWav';
import { createLiveSession, type LiveSessionArgs } from './liveTranscription';
import {
  NO_SPEECH_LANGUAGE,
  NO_SPEECH_STREAMING,
  type SpeechLanguageGate,
  type SpeechStreamingGate,
} from '../../api/models';
import { PortalApiError } from '../../api/transport';
import { messages, type Locale } from '../../i18n';
import type {
  ActiveChatRun,
  ImageModelOptions,
  ModelOption,
  PortalToken,
  ServerModelOption,
} from '../../api';

// jsdom has no WebAudio, so the browser-side conversion of a clip to WAV (audioToWav.test.ts's
// subject) is a stand-in here: it "converts" by prefixing the bytes, so a part's payload says
// whether the CONVERTED clip (not the upload, merely relabelled) is what the turn carries.
vi.mock('../shared/audioToWav', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../shared/audioToWav')>()),
  audioBlobToWav: vi.fn(async (blob: Blob) => new Blob(['WAV:', blob], { type: 'audio/wav' })),
}));

// jsdom has no Web Audio either, so the progressive speech playback (speechPlayback.test.ts's
// subject) is a recording stand-in here: these tests are about WHEN the store hands audio to
// it, primes it and silences it, not about sound.
const playback = vi.hoisted(() => ({
  prime: vi.fn(),
  push: vi.fn((..._args: unknown[]) => true),
  end: vi.fn(),
  stopAll: vi.fn(),
  isLive: vi.fn(() => false),
  setMuted: vi.fn(),
}));
vi.mock('./speechPlayback', async (importOriginal) => ({
  ...(await importOriginal<typeof import('./speechPlayback')>()),
  speechPlayback: playback,
}));

// jsdom has no microphone or WebSocket, so a live transcription session (liveTranscription.ts,
// whose glue liveTranscription.session.test.ts runs against a fake browser) is a recording
// stand-in here: these tests are about what the STORE does with a session -- see
// useLiveTranscription.test.tsx for the state machine, and the live describe below for how it is
// wired into the chat (the thread it runs in, the turn it commits).
vi.mock('./liveTranscription', async (importOriginal) => ({
  ...(await importOriginal<typeof import('./liveTranscription')>()),
  createLiveSession: vi.fn(),
}));

const models: ModelOption[] = [
  { id: 'gpt-oss-20b', display_name: 'gpt-oss-20b', flavors: ['openai'], loading_on_count: 0 },
];

// Fixed timestamps for seeded chat rows (T2 > T1 so the T2 chat is "newest").
const T = '2026-07-17T12:00:00Z';
const T1 = '2026-07-17T11:00:00Z';
const T2 = '2026-07-17T13:00:00Z';

// A run is subscribed via EventSource; the store opens `new EventSource(url)`
// and wires named `snapshot`/`delta`/`done` listeners. This fake captures every
// instance and lets a test emit those named events with JSON-encoded data.
class FakeEventSource {
  static instances: FakeEventSource[] = [];
  url: string;
  listeners: Record<string, ((e: MessageEvent) => void)[]> = {};
  onerror: (() => void) | null = null;
  closed = false;
  constructor(url: string) {
    this.url = url;
    FakeEventSource.instances.push(this);
  }
  addEventListener(type: string, fn: (e: MessageEvent) => void) {
    (this.listeners[type] ??= []).push(fn);
  }
  emit(type: string, data: unknown) {
    for (const fn of this.listeners[type] ?? []) fn({ data: JSON.stringify(data) } as MessageEvent);
  }
  close() {
    this.closed = true;
  }
}

// This jsdom build does not expose window.localStorage; install an in-memory
// stub so the store's guarded reads/writes (and the migration path) are exercised.
function installLocalStorage(seed: Record<string, string> = {}) {
  const store = new Map<string, string>(Object.entries(seed));
  const storage = {
    getItem: (k: string) => (store.has(k) ? store.get(k)! : null),
    setItem: (k: string, v: string) => {
      store.set(k, String(v));
    },
    removeItem: (k: string) => {
      store.delete(k);
    },
    clear: () => store.clear(),
    key: (i: number) => Array.from(store.keys())[i] ?? null,
    get length() {
      return store.size;
    },
  } satisfies Storage;
  vi.stubGlobal('localStorage', storage);
}

// Stateful in-memory chat backend (mirrors the real client surface the provider
// uses: chats / createChat / chat / saveChat / deleteChat).
type ChatRow = {
  id: string;
  title: string;
  created_at: string;
  updated_at: string;
  content: unknown;
};
// maxContentBytes is the cap the real listing still serves
// (portal.MaxChatContentBytes on ChatListResponse). The store ignores it -- images
// live out of the transcript -- and a test shrinks it to prove no send is refused
// on its account.
function makeChatApi(seed: ChatRow[] = [], maxContentBytes = 4 * 1024 * 1024) {
  let seq = 0;
  const rows: ChatRow[] = [...seed];
  const stamp = () => new Date(Date.UTC(2026, 6, 17, 12, seq)).toISOString();
  const spies = {
    chats: vi.fn(async () => ({
      data: rows.map(({ content: _content, ...rest }) => rest),
      max_content_bytes: maxContentBytes,
    })),
    createChat: vi.fn(async (body: { title?: string; content?: unknown }) => {
      seq += 1;
      const row: ChatRow = {
        id: `chat_${seq}`,
        title: body.title ?? '',
        created_at: stamp(),
        updated_at: stamp(),
        content: body.content ?? { settings: {}, messages: [] },
      };
      rows.unshift(row);
      return row;
    }),
    chat: vi.fn(async (id: string) => {
      const found = rows.find((row) => row.id === id);
      if (!found) throw new Error('chat not found');
      return found;
    }),
    // Mirrors the backend's PATCH-style PUT: a body with no `content` key
    // (a title-only rename) keeps the row's stored content untouched.
    saveChat: vi.fn(async (id: string, body: { title: string; content?: unknown }) => {
      seq += 1;
      const index = rows.findIndex((row) => row.id === id);
      if (index >= 0)
        rows[index] = {
          ...rows[index],
          title: body.title,
          ...('content' in body ? { content: body.content } : {}),
          updated_at: stamp(),
        };
      return rows[index];
    }),
    deleteChat: vi.fn(async (id: string) => {
      const index = rows.findIndex((row) => row.id === id);
      if (index >= 0) rows.splice(index, 1);
      return { ok: true };
    }),
    // Mock completeness for the pagehide keepalive flush (PortalApi.saveChatKeepalive):
    // no test in this file dispatches `pagehide`, but the store now holds a real
    // reference to this method so the mock must provide one.
    saveChatKeepalive: vi.fn(),
    startChatRun: vi.fn(async (chatId: string, _body: unknown) => ({
      run_id: 'run_1',
      chat_id: chatId,
      status: 'running' as const,
    })),
    cancelChatRun: vi.fn(async () => ({ ok: true })),
    activeChatRuns: vi.fn(async () => ({ data: [] as ActiveChatRun[] })),
    // The two mutations of the audio answer lifecycle (#281): the gateway answers {ok:true}
    // and rewrites the stored document itself; the lifecycle tests below override them to
    // hold or refuse the call. Like every spy here they change no row: what the store does
    // with its LOCAL transcript afterwards is what is under test.
    deleteMessageAudio: vi.fn(async (_chatId: string, _messageId: string) => ({ ok: true })),
    pruneChatAudio: vi.fn(async (_chatId: string, _keepLast: number) => ({ ok: true })),
    // The server-override picker's filtered-model fetch: no test in this file
    // exercises it, but the store now holds a real reference to this method so
    // the mock must provide one.
    serverModels: vi.fn(async () => [] as ServerModelOption[]),
    // The per-model sd-server image options fetch, issued whenever the picked
    // model GENERATES images. Defaults to "nothing known" (every field omitted);
    // the image-options tests below override it.
    modelImageOptions: vi.fn(async (_name: string) => ({}) as ImageModelOptions),
    // The cached voice names of a speech model, fetched whenever the picked model
    // SYNTHESIZES speech. Defaults to "none known"; the voice-picker tests below
    // override it.
    modelVoices: vi.fn(async (_name: string) => [] as string[]),
    // The speech composer's streaming gate, fetched whenever the picked model
    // SYNTHESIZES speech. Defaults to "no streaming" (the buffered WAV run); the
    // speech-streaming tests below override it.
    modelSpeechStreaming: vi.fn(
      async (_name: string): Promise<SpeechStreamingGate> => NO_SPEECH_STREAMING,
    ),
    // The speech composer's language gate, fetched whenever the picked model SYNTHESIZES
    // speech. Closed by default (no language control, nothing pre-filled); the speech-language
    // tests below override it.
    modelSpeechLanguage: vi.fn(
      async (_name: string): Promise<SpeechLanguageGate> => NO_SPEECH_LANGUAGE,
    ),
    // The transcription composer's live gate, fetched whenever the picked model TRANSCRIBES
    // speech. Closed by default (no live mode); the transcription-streaming tests below open it.
    modelTranscriptionStreaming: vi.fn(async (_name: string): Promise<boolean> => false),
    // The live session's ticket POST. The session itself is a stand-in (see the vi.mock of
    // ./liveTranscription), so nothing here calls it: the store only has to hand it over.
    createLiveTranscriptionTicket: vi.fn(async (_body: { model: string; language?: string }) => ({
      ticket: 'tkt',
    })),
  };
  return { api: spies, rows, spies };
}

let chatApi: ReturnType<typeof makeChatApi>;

function Probe({ altModelId = '' }: { altModelId?: string } = {}) {
  const c = useChatStore();
  const last = c.messages[c.messages.length - 1];
  const lastText = last && typeof last.content === 'string' ? last.content : '';
  return (
    <div>
      <span data-testid="count">{c.messages.length}</span>
      <span data-testid="streaming">{String(c.streaming)}</span>
      <span data-testid="last">{lastText}</span>
      {/* The last turn's STRUCTURED content, as "type:detail" per part (empty
          for a plain string). `last` above can only ever show text, so without
          this a test about an image turn can assert that a bubble EXISTS but
          not that it holds the generated image. */}
      <span data-testid="last-parts">
        {Array.isArray(last?.content)
          ? last.content
              .map((p) => {
                if (p.type === 'image_url') return `image_url:${p.image_url.url}`;
                if (p.type === 'audio_url') return `audio_url:${p.audio_url.url}`;
                if (p.type === 'audio_removed') return 'audio_removed';
                return `text:${p.text}`;
              })
              .join('|')
          : ''}
      </span>
      <span data-testid="last-status">{last?.status ?? ''}</span>
      <span data-testid="loading">{String(c.chatsLoading)}</span>
      <span data-testid="active">{c.activeChatId ?? ''}</span>
      <span data-testid="chats">{c.chats.map((ch) => `${ch.id}:${ch.title}`).join('|')}</span>
      <span data-testid="model">{c.model}</span>
      <span data-testid="model-available">{String(c.modelAvailable)}</span>
      <span data-testid="chat-kind">{c.chatKind}</span>
      {/* The whole transcript, as the store holds it, for the audio lifecycle tests. */}
      <span data-testid="messages-json">{JSON.stringify(c.messages)}</span>
      {/* The retention change held for an explicit confirm ("null" while there is none). */}
      <span data-testid="pending-retention">{JSON.stringify(c.pendingAudioRetention)}</span>
      <span data-testid="run-kind">{c.runKind ?? ''}</span>
      <span data-testid="run-elapsed-ms">{c.runElapsedMs ?? ''}</span>
      <span data-testid="model-image-capable">{String(c.modelImageCapable)}</span>
      <span data-testid="model-image-edit">{String(c.modelImageEdit)}</span>
      <span data-testid="model-speech-capable">{String(c.modelSpeechCapable)}</span>
      <span data-testid="model-transcription-capable">{String(c.modelTranscriptionCapable)}</span>
      {/* The speech thread's concrete audio settings (voice / speed / passthrough text). */}
      <span data-testid="audio-settings">{JSON.stringify(c.audioSettings)}</span>
      {/* The transcription thread's concrete settings (language / prompt / temperature /
          passthrough text). */}
      <span data-testid="transcription-settings">{JSON.stringify(c.transcriptionSettings)}</span>
      <span data-testid="attach-mode">{c.attachMode}</span>
      {/* The composer's attachment strip: the source image(s) and the mask, as the
          urls the next send would carry. */}
      <span data-testid="composer-images">{c.images.join('|')}</span>
      <span data-testid="composer-mask">{c.mask ?? ''}</span>
      {/* The transcription composer's clip: the data url the next send would carry, and the
          name it is shown under ('' for a recording). */}
      <span data-testid="composer-audio">{c.audio?.url ?? ''}</span>
      <span data-testid="composer-audio-name">{c.audio?.name ?? ''}</span>
      {/* Whether a clip is being converted to WAV (the attach that has not landed yet). */}
      <span data-testid="audio-converting">{String(c.audioConverting)}</span>
      <input
        type="file"
        aria-label="probe-audio-file"
        onChange={(e) => void c.handleFiles(e.target.files)}
      />
      {/* A browser recording, as the recorder hands it over: the recorder's own type, with the
          codec parameter. */}
      <button
        type="button"
        onClick={() =>
          void c.attachRecording(new Blob(['recorded'], { type: 'audio/webm;codecs=opus' }))
        }
      >
        attach-recording
      </button>
      <button type="button" onClick={() => c.removeAudio()}>
        remove-audio
      </button>
      {/* The fetched per-model image options ("null" until/unless one arrives). */}
      <span data-testid="model-image-options">{JSON.stringify(c.modelImageOptions)}</span>
      {/* The fetched voice names of the picked speech model ("[]" until/unless some arrive). */}
      <span data-testid="model-voices">{JSON.stringify(c.modelVoices)}</span>
      {/* Whether the speech composer may offer streaming (the gate is open), and whether the
          active chat's run is playing its audio live. */}
      <span data-testid="speech-streaming">{String(c.speechStreaming)}</span>
      {/* Whether the speech composer may offer the language control (the gate says the model
          takes a language). */}
      <span data-testid="speech-language">{String(c.speechLanguage)}</span>
      {/* Whether the transcription composer may offer a live recording (the gate is open). */}
      <span data-testid="transcription-streaming">{String(c.transcriptionStreaming)}</span>
      {/* The live transcription session: its state, the transcript so far and its error. */}
      <span data-testid="live-state">{c.liveSession.state}</span>
      <span data-testid="live-transcript">{c.liveSession.transcript}</span>
      <span data-testid="live-error">{c.liveSession.error ?? ''}</span>
      <button type="button" onClick={() => c.startLiveTranscription()}>
        live-start
      </button>
      <button type="button" onClick={() => c.stopLiveTranscription()}>
        live-stop
      </button>
      <button type="button" onClick={() => c.abortLiveTranscription()}>
        live-abort
      </button>
      <button type="button" onClick={() => c.keepLiveTranscript()}>
        live-keep
      </button>
      <span data-testid="run-audio-live">{String(c.runAudioLive)}</span>
      <span data-testid="run-audio-muted">{String(c.runAudioMuted)}</span>
      <button type="button" onClick={() => c.muteAudio('c1', true)}>
        mute-audio
      </button>
      <button type="button" onClick={() => c.muteAudio('c1', false)}>
        unmute-audio
      </button>
      <button type="button" onClick={() => c.setAudioSettings((s) => ({ ...s, stream: false }))}>
        set-audio-stream-off
      </button>
      <span data-testid="model-options">{c.modelOptions.map((o) => o.id).join(',')}</span>
      <span data-testid="override-model">{c.overrideModel}</span>
      <span data-testid="override-locks">{String(c.overrideLocksModel)}</span>
      <input
        aria-label="probe-input"
        value={c.input}
        onChange={(e) => c.setInput(e.target.value)}
      />
      <button type="button" onClick={() => c.send()}>
        send
      </button>
      <button type="button" onClick={() => c.stop()}>
        stop
      </button>
      <button type="button" onClick={() => c.newChat()}>
        newchat
      </button>
      <button type="button" onClick={() => c.setSystemPrompt('hi')}>
        set-system
      </button>
      {/* The speech composer's settings, moved off their defaults, and then the
          passthrough text broken (not a JSON object) without touching the rest. */}
      <button
        type="button"
        onClick={() =>
          c.setAudioSettings({
            voice: 'af_heart',
            speed: '1.25',
            language: 'de',
            passthrough: '{"seed":3}',
            stream: true,
            keepAudio: true,
            keepLast: '3',
          })
        }
      >
        set-audio
      </button>
      <button
        type="button"
        onClick={() => c.setAudioSettings((s) => ({ ...s, passthrough: '{"seed":' }))}
      >
        break-audio-passthrough
      </button>
      {/* The retention control's edits (#281): keep-last-N with the given count text (blank,
          a number, or text the wire drops), do-not-store, and back on, each a patch over what
          the composer shows, as the control sends them. */}
      {['', '1', '2', '5', '10', 'abc', '1.5', '0'].map((text) => (
        <button
          key={`keep-last-${text}`}
          type="button"
          onClick={() => c.setAudioSettings((s) => ({ ...s, keepAudio: true, keepLast: text }))}
        >
          keep-last-{text === '' ? 'blank' : text}
        </button>
      ))}
      <button type="button" onClick={() => c.setAudioSettings((s) => ({ ...s, keepAudio: false }))}>
        keep-audio-off
      </button>
      <button type="button" onClick={() => c.setAudioSettings((s) => ({ ...s, keepAudio: true }))}>
        keep-audio-on
      </button>
      <button type="button" onClick={() => c.setAudioSettings((s) => ({ ...s, voice: 'bf_emma' }))}>
        set-voice
      </button>
      {/* The language control's two edits: a typed code, and the field cleared again. */}
      <button type="button" onClick={() => c.setAudioSettings((s) => ({ ...s, language: 'fr' }))}>
        set-language
      </button>
      <button type="button" onClick={() => c.setAudioSettings((s) => ({ ...s, language: '' }))}>
        clear-language
      </button>
      <button type="button" onClick={() => c.confirmAudioRetention()}>
        confirm-retention
      </button>
      <button type="button" onClick={() => c.cancelAudioRetention()}>
        cancel-retention
      </button>
      {/* ...or valid JSON naming a field the run owns (case-insensitively). */}
      <button
        type="button"
        onClick={() =>
          c.setAudioSettings((s) => ({
            ...s,
            passthrough: '{"seed": 1, "Response_Format": "mp3"}',
          }))
        }
      >
        reserve-audio-passthrough
      </button>
      {/* The transcription composer's settings, moved off their defaults (an explicit
          temperature of 0 among them), then the passthrough text broken, and then naming a
          field the run owns (case-insensitively), each without touching the rest. */}
      <button
        type="button"
        onClick={() =>
          c.setTranscriptionSettings({
            language: 'de',
            prompt: 'Namen: Zoe',
            temperature: '0',
            passthrough: '{"beam":5}',
          })
        }
      >
        set-transcription
      </button>
      <button
        type="button"
        onClick={() => c.setTranscriptionSettings((s) => ({ ...s, passthrough: '{"beam":' }))}
      >
        break-transcription-passthrough
      </button>
      <button
        type="button"
        onClick={() =>
          c.setTranscriptionSettings((s) => ({
            ...s,
            passthrough: '{"beam": 1, "File": "x"}',
          }))
        }
      >
        reserve-transcription-passthrough
      </button>
      {/* Selects a caller-supplied model id (a test's "pick an available model"
          action) without needing a full SearchableSelect in this probe. */}
      <button type="button" onClick={() => c.setModel(altModelId)}>
        set-model-alt
      </button>
      {/* One "pick this model" button per reachable model, for tests that switch
          between several (set-model-alt only ever targets one id). */}
      {c.chatModels.map((m) => (
        <button key={`pick-${m.id}`} type="button" onClick={() => c.setModel(m.id)}>
          pick-{m.id}
        </button>
      ))}
      {c.usableTokens.map((tk) => (
        <button key={tk.id} type="button" onClick={() => c.setSelectedTokenId(tk.id)}>
          select-token-{tk.id}
        </button>
      ))}
      {/* Per-message edit/regenerate triggers (via the store's handlersFor), so a
          test can exercise the startRunWithHistory entry points directly. */}
      {c.messages.map((m) => (
        <span key={`h-${m.id}`}>
          <button type="button" onClick={() => c.handlersFor(m.id).onRegenerate()}>
            regenerate-{m.id}
          </button>
          <button type="button" onClick={() => c.handlersFor(m.id).onDeleteAudio()}>
            delete-audio-{m.id}
          </button>
          <button type="button" onClick={() => c.handlersFor(m.id).onEdit('edited text')}>
            edit-{m.id}
          </button>
          <button type="button" onClick={() => c.handlersFor(m.id).onEdit('  ')}>
            edit-blank-{m.id}
          </button>
          {/* Refine: the first image part of the message (what ImageTurn hands the
              handler for it), and a url that is none of the message's images. */}
          <button
            type="button"
            onClick={() => {
              const part = Array.isArray(m.content)
                ? m.content.find((p) => p.type === 'image_url')
                : undefined;
              c.handlersFor(m.id).onRefine(part?.type === 'image_url' ? part.image_url.url : '');
            }}
          >
            refine-{m.id}
          </button>
          <button
            type="button"
            onClick={() => c.handlersFor(m.id).onRefine('/api/portal/chat-blobs/blob_foreign')}
          >
            refine-foreign-{m.id}
          </button>
        </span>
      ))}
      {c.chats.map((ch) => (
        <span key={ch.id}>
          <button type="button" onClick={() => c.selectChat(ch.id)}>
            select-{ch.id}
          </button>
          <button type="button" onClick={() => c.deleteChat(ch.id)}>
            delete-{ch.id}
          </button>
          <button type="button" onClick={() => c.renameChat(ch.id, 'Renamed')}>
            rename-{ch.id}
          </button>
        </span>
      ))}
    </div>
  );
}

for (const locale of ['de', 'en'] as readonly Locale[]) {
  const t = messages[locale];

  function renderProvider(
    tokens: PortalToken[] = [],
    opts: { models?: ModelOption[]; refreshModels?: () => void; altModelId?: string } = {},
  ) {
    const onRefresh = vi.fn(async () => {});
    const modelsToUse = opts.models ?? models;
    const view = render(
      <ToastProvider>
        <ChatStoreProvider
          api={chatApi.api}
          models={modelsToUse}
          tokens={tokens}
          onRefresh={onRefresh}
          refreshModels={opts.refreshModels}
          t={t}
          locale={locale}
        >
          <Probe altModelId={opts.altModelId ?? modelsToUse[0]?.id ?? ''} />
        </ChatStoreProvider>
      </ToastProvider>,
    );
    const rerenderWithModels = (nextModels: ModelOption[]) =>
      view.rerender(
        <ToastProvider>
          <ChatStoreProvider
            api={chatApi.api}
            models={nextModels}
            tokens={tokens}
            onRefresh={onRefresh}
            refreshModels={opts.refreshModels}
            t={t}
            locale={locale}
          >
            <Probe altModelId={opts.altModelId ?? nextModels[0]?.id ?? ''} />
          </ChatStoreProvider>
        </ToastProvider>,
      );
    // The same provider re-rendered under the OTHER portal language (no remount), for the
    // tests that pin what a UI-language switch must not do to the thread's own settings.
    const rerenderWithLocale = (nextLocale: Locale) =>
      view.rerender(
        <ToastProvider>
          <ChatStoreProvider
            api={chatApi.api}
            models={modelsToUse}
            tokens={tokens}
            onRefresh={onRefresh}
            refreshModels={opts.refreshModels}
            t={messages[nextLocale]}
            locale={nextLocale}
          >
            <Probe altModelId={opts.altModelId ?? modelsToUse[0]?.id ?? ''} />
          </ChatStoreProvider>
        </ToastProvider>,
      );
    return { onRefresh, rerenderWithModels, rerenderWithLocale, unmount: view.unmount };
  }

  // The provider opens/creates a chat asynchronously on mount; wait for that to
  // settle before interacting so a late activation cannot clobber test state.
  async function waitForReady() {
    await waitFor(() => expect(screen.getByTestId('loading').textContent).toBe('false'));
  }

  beforeEach(() => {
    installLocalStorage();
    chatApi = makeChatApi();
    // Re-install the EventSource stub every test: afterEach's unstubAllGlobals
    // removes it (alongside the localStorage stub), so a module-level stub would
    // vanish after the first test.
    FakeEventSource.instances = [];
    vi.stubGlobal('EventSource', FakeEventSource as unknown as typeof EventSource);
  });

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
  });

  describe(`useChatStore / useChatStreaming guards [${locale}]`, () => {
    it('useChatStore throws outside a provider', () => {
      function Bad() {
        useChatStore();
        return null;
      }
      const spy = vi.spyOn(console, 'error').mockImplementation(() => {});
      expect(() => render(<Bad />)).toThrow(/ChatStoreProvider/);
      spy.mockRestore();
    });

    it('useChatStreaming returns false outside a provider (no throw)', () => {
      function Peek() {
        return <span data-testid="s">{String(useChatStreaming())}</span>;
      }
      render(<Peek />);
      expect(screen.getByTestId('s').textContent).toBe('false');
    });
  });

  describe(`ChatStoreProvider bootstrap [${locale}]`, () => {
    it('creates a fresh chat when the list is empty and there is nothing to migrate', async () => {
      renderProvider();
      await waitForReady();
      expect(chatApi.spies.createChat).toHaveBeenCalledTimes(1);
      expect(screen.getByTestId('active').textContent).not.toBe('');
      expect(screen.getByTestId('count').textContent).toBe('0');
    });

    it('loads the list and opens the newest chat', async () => {
      chatApi = makeChatApi([
        {
          id: 'c_new',
          title: 'Newer',
          created_at: '2026-07-17T13:00:00Z',
          updated_at: '2026-07-17T13:00:00Z',
          content: { settings: {}, messages: [{ id: 'm1', role: 'user', content: 'in newer' }] },
        },
        {
          id: 'c_old',
          title: 'Older',
          created_at: '2026-07-17T11:00:00Z',
          updated_at: '2026-07-17T11:00:00Z',
          content: { settings: {}, messages: [{ id: 'm2', role: 'user', content: 'in older' }] },
        },
      ]);
      renderProvider();
      await waitForReady();
      expect(chatApi.spies.createChat).not.toHaveBeenCalled();
      expect(screen.getByTestId('active').textContent).toBe('c_new');
      expect(screen.getByTestId('last').textContent).toBe('in newer');
    });

    it('migrates an existing localStorage conversation into the first server chat and clears the keys', async () => {
      installLocalStorage({
        'op.chat.model': 'gpt-oss-20b',
        'op.chat.systemPrompt': 'you are helpful',
        'op.chat.messages': JSON.stringify([{ id: 'm1', role: 'user', content: 'restored hi' }]),
      });
      renderProvider();
      await waitForReady();

      expect(chatApi.spies.createChat).toHaveBeenCalledTimes(1);
      const body = chatApi.spies.createChat.mock.calls[0][0] as {
        title?: string;
        content?: { settings: { system_prompt: string }; messages: { content: string }[] };
      };
      expect(body.title).toBe('restored hi');
      expect(body.content?.messages[0].content).toBe('restored hi');
      expect(body.content?.settings.system_prompt).toBe('you are helpful');
      // The migrated transcript is now the active chat.
      expect(screen.getByTestId('last').textContent).toBe('restored hi');
      // Legacy keys are removed after migration.
      expect(window.localStorage.getItem('op.chat.messages')).toBeNull();
      expect(window.localStorage.getItem('op.chat.model')).toBeNull();
    });
  });

  describe(`ChatStoreProvider chat actions [${locale}]`, () => {
    it('newChat creates + activates a fresh empty chat', async () => {
      chatApi = makeChatApi([
        {
          id: 'c_seed',
          title: 'Seed',
          created_at: '2026-07-17T10:00:00Z',
          updated_at: '2026-07-17T10:00:00Z',
          content: { settings: {}, messages: [{ id: 'm1', role: 'user', content: 'seeded' }] },
        },
      ]);
      renderProvider();
      await waitForReady();
      expect(screen.getByTestId('last').textContent).toBe('seeded');
      const activeBefore = screen.getByTestId('active').textContent;

      fireEvent.click(screen.getByRole('button', { name: 'newchat' }));

      await waitFor(() => expect(chatApi.spies.createChat).toHaveBeenCalled());
      await waitFor(() => expect(screen.getByTestId('count').textContent).toBe('0'));
      await waitFor(() => expect(screen.getByTestId('active').textContent).not.toBe(activeBefore));
    });

    it("selectChat loads the target chat's content", async () => {
      chatApi = makeChatApi([
        {
          id: 'c_new',
          title: 'Newer',
          created_at: '2026-07-17T13:00:00Z',
          updated_at: '2026-07-17T13:00:00Z',
          content: { settings: {}, messages: [{ id: 'm1', role: 'user', content: 'in newer' }] },
        },
        {
          id: 'c_old',
          title: 'Older',
          created_at: '2026-07-17T11:00:00Z',
          updated_at: '2026-07-17T11:00:00Z',
          content: { settings: {}, messages: [{ id: 'm2', role: 'user', content: 'in older' }] },
        },
      ]);
      renderProvider();
      await waitForReady();
      expect(screen.getByTestId('last').textContent).toBe('in newer');

      fireEvent.click(screen.getByRole('button', { name: 'select-c_old' }));

      await waitFor(() => expect(screen.getByTestId('active').textContent).toBe('c_old'));
      expect(screen.getByTestId('last').textContent).toBe('in older');
    });

    it('deleteChat removes the chat and falls back to the next newest when the active one is deleted', async () => {
      chatApi = makeChatApi([
        {
          id: 'c_new',
          title: 'Newer',
          created_at: '2026-07-17T13:00:00Z',
          updated_at: '2026-07-17T13:00:00Z',
          content: { settings: {}, messages: [{ id: 'm1', role: 'user', content: 'in newer' }] },
        },
        {
          id: 'c_old',
          title: 'Older',
          created_at: '2026-07-17T11:00:00Z',
          updated_at: '2026-07-17T11:00:00Z',
          content: { settings: {}, messages: [{ id: 'm2', role: 'user', content: 'in older' }] },
        },
      ]);
      renderProvider();
      await waitForReady();
      expect(screen.getByTestId('active').textContent).toBe('c_new');

      fireEvent.click(screen.getByRole('button', { name: 'delete-c_new' }));

      await waitFor(() => expect(chatApi.spies.deleteChat).toHaveBeenCalledWith('c_new'));
      await waitFor(() => expect(screen.getByTestId('active').textContent).toBe('c_old'));
      expect(screen.getByTestId('last').textContent).toBe('in older');
      // The deleted chat is gone from the sidebar list.
      expect(screen.getByTestId('chats').textContent).not.toContain('c_new');
    });

    it('renameChat updates the title locally and persists it', async () => {
      chatApi = makeChatApi([
        {
          id: 'c_seed',
          title: 'Seed',
          created_at: '2026-07-17T10:00:00Z',
          updated_at: '2026-07-17T10:00:00Z',
          content: { settings: {}, messages: [] },
        },
      ]);
      renderProvider();
      await waitForReady();

      fireEvent.click(screen.getByRole('button', { name: 'rename-c_seed' }));

      await waitFor(() =>
        expect(screen.getByTestId('chats').textContent).toContain('c_seed:Renamed'),
      );
      // Title-only: no `content` key, so the backend's PATCH-style PUT keeps
      // the stored document untouched.
      await waitFor(() =>
        expect(chatApi.spies.saveChat).toHaveBeenCalledWith('c_seed', { title: 'Renamed' }),
      );
    });

    // renameChat writes the new title OPTIMISTICALLY and is the one writer of
    // the four with no client-side run gate, so this branch's own 409 on
    // PUT /chats/{id} while a run is active lands squarely on it: without the
    // rollback the toast says the rename failed while the sidebar goes on
    // showing the new name until the next reload silently reverts it.
    //
    // The run is a REAL one (send -> FakeEventSource), not a stubbed flag:
    // it is also what keeps the debounced autosave from firing (the save
    // effect skips a chat with a live run), so the only saveChat in this test
    // is the rename's own.
    it('rolls the optimistic title back when a rename is refused during a run', async () => {
      chatApi = makeChatApi([
        {
          id: 'c_seed',
          title: 'Seed',
          created_at: '2026-07-17T10:00:00Z',
          updated_at: '2026-07-17T10:00:00Z',
          content: { settings: { model: 'gpt-oss-20b' }, messages: [] },
        },
      ]);
      renderProvider();
      await waitForReady();

      fireEvent.change(screen.getByLabelText('probe-input'), { target: { value: 'hello' } });
      fireEvent.click(screen.getByRole('button', { name: 'send' }));
      await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
      chatApi.spies.saveChat.mockClear();
      chatApi.spies.saveChat.mockRejectedValue(
        new PortalApiError(409, 'portal.chat_run_active', 'a run is active for this chat'),
      );

      fireEvent.click(screen.getByRole('button', { name: 'rename-c_seed' }));

      // The refusal is reported...
      await screen.findByText(`portal.chat_run_active: ${t.errorChatRunActive}`);
      await waitFor(() => expect(chatApi.spies.saveChat).toHaveBeenCalledTimes(1));
      // ...and the sidebar shows the title the server still holds, not the
      // one it rejected.
      expect(screen.getByTestId('chats').textContent).toContain('c_seed:Seed');
      expect(screen.getByTestId('chats').textContent).not.toContain('c_seed:Renamed');
    });

    // The rollback's SECOND half is not the chat's title -- `activeTitleRef`
    // is the title the debounced autosave PUTs for whatever chat is active
    // right now, and "right now" is after an await the user can spend
    // switching chats. Restoring it unconditionally therefore writes the
    // renamed chat's old title into a DIFFERENT chat's ref, and the next
    // autosave persists a rename nobody asked for, silently and with no
    // toast for the chat it lands on.
    //
    // Reproduced exactly as a user would: rename Alpha, click over to Beta,
    // and only then let Alpha's PUT fail with the 409 this rollback exists
    // for. The PUT is held open with a manual deferred so the chat switch is
    // deterministically inside the window rather than raced into it.
    it("does not write the renamed chat's old title into the ref of a chat the user switched to", async () => {
      chatApi = makeChatApi([
        {
          id: 'c_alpha',
          title: 'Alpha',
          created_at: T2,
          updated_at: T2,
          content: { settings: { model: 'gpt-oss-20b' }, messages: [] },
        },
        {
          id: 'c_beta',
          title: 'Beta',
          created_at: T1,
          updated_at: T1,
          content: { settings: { model: 'gpt-oss-20b' }, messages: [] },
        },
      ]);
      renderProvider();
      await waitForReady();
      await waitFor(() => expect(screen.getByTestId('active').textContent).toBe('c_alpha'));

      const workingSave = chatApi.spies.saveChat.getMockImplementation()!;
      let failRename: (err: unknown) => void = () => {};
      chatApi.spies.saveChat.mockImplementation(
        () =>
          new Promise((_resolve, reject) => {
            failRename = reject;
          }),
      );

      fireEvent.click(screen.getByRole('button', { name: 'rename-c_alpha' }));
      await waitFor(() => expect(chatApi.spies.saveChat).toHaveBeenCalledTimes(1));

      // The user moves on while that PUT is still open.
      fireEvent.click(screen.getByRole('button', { name: 'select-c_beta' }));
      await waitFor(() => expect(screen.getByTestId('active').textContent).toBe('c_beta'));

      // Restore a working save BEFORE the failure, so the autosave below is
      // observable; the already-pending promise is rejected through its own
      // captured reject.
      chatApi.spies.saveChat.mockImplementation(workingSave);
      failRename(new PortalApiError(409, 'portal.chat_run_active', 'a run is active'));

      await screen.findByText(`portal.chat_run_active: ${t.errorChatRunActive}`);
      // Alpha's row is rolled back, exactly as when no switch happened.
      expect(screen.getByTestId('chats').textContent).toContain('c_alpha:Alpha');
      expect(screen.getByTestId('chats').textContent).not.toContain('c_alpha:Renamed');

      // The real assertion: the next autosave must still be Beta's own
      // title. `activeTitleRef` is only observable through what flushSave
      // PUTs, which is also the exact mechanism that made the defect a
      // SILENT, persisted one.
      chatApi.spies.saveChat.mockClear();
      fireEvent.click(screen.getByRole('button', { name: 'set-system' }));
      await waitFor(() => expect(chatApi.spies.saveChat).toHaveBeenCalled(), { timeout: 2500 });
      const [savedId, body] = chatApi.spies.saveChat.mock.calls.at(-1)!;
      expect(savedId).toBe('c_beta');
      expect(body.title).toBe('Beta');
    });

    // The mirror image of the case above, and the reason the guard needs BOTH
    // of its conditions rather than only "is the renamed chat active now".
    // Here the rename targets a chat that is NOT active, so the ref was never
    // written -- and then the user opens that very chat, which makes it
    // active before the failure lands. A guard that only asked "is this chat
    // active now?" would answer yes and restore a title belonging to the chat
    // the user LEFT.
    it("does not write a departed chat's title into the ref when the user opens the renamed chat", async () => {
      chatApi = makeChatApi([
        {
          id: 'c_alpha',
          title: 'Alpha',
          created_at: T2,
          updated_at: T2,
          content: { settings: { model: 'gpt-oss-20b' }, messages: [] },
        },
        {
          id: 'c_beta',
          title: 'Beta',
          created_at: T1,
          updated_at: T1,
          content: { settings: { model: 'gpt-oss-20b' }, messages: [] },
        },
      ]);
      renderProvider();
      await waitForReady();
      await waitFor(() => expect(screen.getByTestId('active').textContent).toBe('c_alpha'));

      const workingSave = chatApi.spies.saveChat.getMockImplementation()!;
      let failRename: (err: unknown) => void = () => {};
      chatApi.spies.saveChat.mockImplementation(
        () =>
          new Promise((_resolve, reject) => {
            failRename = reject;
          }),
      );

      // Beta is NOT the active chat, so this rename never touches the ref.
      fireEvent.click(screen.getByRole('button', { name: 'rename-c_beta' }));
      await waitFor(() => expect(chatApi.spies.saveChat).toHaveBeenCalledTimes(1));

      fireEvent.click(screen.getByRole('button', { name: 'select-c_beta' }));
      await waitFor(() => expect(screen.getByTestId('active').textContent).toBe('c_beta'));

      chatApi.spies.saveChat.mockImplementation(workingSave);
      failRename(new PortalApiError(409, 'portal.chat_run_active', 'a run is active'));

      await screen.findByText(`portal.chat_run_active: ${t.errorChatRunActive}`);
      expect(screen.getByTestId('chats').textContent).toContain('c_beta:Beta');

      chatApi.spies.saveChat.mockClear();
      fireEvent.click(screen.getByRole('button', { name: 'set-system' }));
      await waitFor(() => expect(chatApi.spies.saveChat).toHaveBeenCalled(), { timeout: 2500 });
      const [savedId, body] = chatApi.spies.saveChat.mock.calls.at(-1)!;
      expect(savedId).toBe('c_beta');
      expect(body.title).toBe('Beta');
    });

    // The rollback is not conditioned on the status code. A 404 (the chat was
    // deleted in another tab) leaves exactly the same wrong title on screen,
    // and a rule that reverted only 409s would be a distinction nothing
    // downstream could act on.
    it('rolls the optimistic title back on a NON-409 failure too', async () => {
      chatApi = makeChatApi([
        {
          id: 'c_seed',
          title: 'Seed',
          created_at: '2026-07-17T10:00:00Z',
          updated_at: '2026-07-17T10:00:00Z',
          content: { settings: { model: 'gpt-oss-20b' }, messages: [] },
        },
      ]);
      renderProvider();
      await waitForReady();
      chatApi.spies.saveChat.mockRejectedValue(
        new PortalApiError(404, 'portal.chat_not_found', 'chat not found'),
      );

      fireEvent.click(screen.getByRole('button', { name: 'rename-c_seed' }));

      await screen.findByText(`portal.chat_not_found: ${t.errorChatNotFound}`);
      await waitFor(() => expect(screen.getByTestId('chats').textContent).toContain('c_seed:Seed'));
      expect(screen.getByTestId('chats').textContent).not.toContain('c_seed:Renamed');
    });

    it('debounced-saves the active chat after a settings change', async () => {
      renderProvider();
      await waitForReady();
      const before = chatApi.spies.saveChat.mock.calls.length;

      fireEvent.click(screen.getByRole('button', { name: 'set-system' }));

      await waitFor(
        () => expect(chatApi.spies.saveChat.mock.calls.length).toBeGreaterThan(before),
        {
          timeout: 2500,
        },
      );
      const lastCall = chatApi.spies.saveChat.mock.calls.at(-1)!;
      const content = lastCall[1].content as { settings: { system_prompt: string } };
      expect(content.settings.system_prompt).toBe('hi');
    });

    // Regression: deleting the active chat while a debounced save is pending must
    // cancel that save, or the 800ms timer fires a PUT against the just-deleted
    // chat (backend 404 -> spurious "chat not found" toast). We keep the post-delete
    // replacement load PENDING so that activateChat (which would also clear the
    // timer) cannot be what saves us — only deleteChat's synchronous cancel can.
    it('deleting the active chat cancels its pending debounced save (no save against the deleted chat)', async () => {
      chatApi = makeChatApi([
        {
          id: 'c_new',
          title: 'Newer',
          created_at: '2026-07-17T13:00:00Z',
          updated_at: '2026-07-17T13:00:00Z',
          content: { settings: {}, messages: [{ id: 'm1', role: 'user', content: 'in newer' }] },
        },
        {
          id: 'c_old',
          title: 'Older',
          created_at: '2026-07-17T11:00:00Z',
          updated_at: '2026-07-17T11:00:00Z',
          content: { settings: {}, messages: [{ id: 'm2', role: 'user', content: 'in older' }] },
        },
      ]);
      renderProvider();
      await waitForReady();
      expect(screen.getByTestId('active').textContent).toBe('c_new');

      // Hold the replacement (c_old) load open for the whole assertion window.
      let releaseReplacement: () => void = () => {};
      chatApi.spies.chat.mockImplementation(
        (id: string) =>
          new Promise<ChatRow>((resolve) => {
            releaseReplacement = () => resolve(chatApi.rows.find((row) => row.id === id)!);
          }),
      );

      // Change a setting (dirties + schedules the 800ms save), then delete the
      // active chat within the debounce window.
      fireEvent.click(screen.getByRole('button', { name: 'set-system' }));
      fireEvent.click(screen.getByRole('button', { name: 'delete-c_new' }));
      await waitFor(() => expect(chatApi.spies.deleteChat).toHaveBeenCalledWith('c_new'));

      // Let the 800ms debounce window fully elapse (with margin). The replacement
      // load is still pending, so a surviving timer WOULD have fired by now.
      await new Promise((resolve) => setTimeout(resolve, 1100));

      // The pending save must never have targeted the deleted chat.
      expect(chatApi.spies.saveChat.mock.calls.some((call) => call[0] === 'c_new')).toBe(false);

      // Release the replacement so the store settles on c_old (clean teardown).
      await act(async () => {
        releaseReplacement();
      });
      await waitFor(() => expect(screen.getByTestId('active').textContent).toBe('c_old'));
    });

    // Regression: rapid B-then-C clicks both pass the entry guard (active id is not
    // updated until a load resolves), so without a latest-request token an earlier
    // click whose fetch resolves LATER would clobber the last click. The last click
    // must always win regardless of fetch completion order.
    it('selectChat: a stale earlier click cannot override the last click (latest-request wins)', async () => {
      chatApi = makeChatApi([
        {
          id: 'c_new',
          title: 'Newer',
          created_at: '2026-07-17T13:00:00Z',
          updated_at: '2026-07-17T13:00:00Z',
          content: { settings: {}, messages: [{ id: 'm1', role: 'user', content: 'in newer' }] },
        },
        {
          id: 'c_mid',
          title: 'Middle',
          created_at: '2026-07-17T12:00:00Z',
          updated_at: '2026-07-17T12:00:00Z',
          content: { settings: {}, messages: [{ id: 'm2', role: 'user', content: 'in middle' }] },
        },
        {
          id: 'c_old',
          title: 'Older',
          created_at: '2026-07-17T11:00:00Z',
          updated_at: '2026-07-17T11:00:00Z',
          content: { settings: {}, messages: [{ id: 'm3', role: 'user', content: 'in older' }] },
        },
      ]);
      renderProvider();
      await waitForReady();
      expect(screen.getByTestId('active').textContent).toBe('c_new');

      // Make chat() loads resolvable out of click order.
      const pending = new Map<string, () => void>();
      chatApi.spies.chat.mockImplementation(
        (id: string) =>
          new Promise<ChatRow>((resolve) => {
            pending.set(id, () => resolve(chatApi.rows.find((row) => row.id === id)!));
          }),
      );

      // Click c_old first, then c_mid — c_mid is the LAST click and must win.
      fireEvent.click(screen.getByRole('button', { name: 'select-c_old' }));
      fireEvent.click(screen.getByRole('button', { name: 'select-c_mid' }));
      await waitFor(() => {
        expect(pending.has('c_old')).toBe(true);
        expect(pending.has('c_mid')).toBe(true);
      });

      // Resolve the later click (c_mid) first...
      await act(async () => {
        pending.get('c_mid')!();
      });
      await waitFor(() => expect(screen.getByTestId('active').textContent).toBe('c_mid'));

      // ...then the earlier click (c_old) resolves late — it must be dropped as stale.
      await act(async () => {
        pending.get('c_old')!();
      });

      expect(screen.getByTestId('active').textContent).toBe('c_mid');
      expect(screen.getByTestId('last').textContent).toBe('in middle');
    });
  });

  describe(`ChatStoreProvider streaming [${locale}]`, () => {
    it('send starts a server run and applies streamed deltas to the active chat', async () => {
      chatApi = makeChatApi([
        {
          id: 'c1',
          title: 'C1',
          created_at: T,
          updated_at: T,
          content: { settings: { model: 'gpt-oss-20b' }, messages: [] },
        },
      ]);
      renderProvider();
      await waitForReady();

      fireEvent.change(screen.getByLabelText('probe-input'), { target: { value: 'hello' } });
      fireEvent.click(screen.getByRole('button', { name: 'send' }));

      // send posts a run, then subscribes over EventSource.
      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalled());
      expect(chatApi.spies.startChatRun.mock.calls[0][0]).toBe('c1');
      await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
      const es = FakeEventSource.instances.at(-1)!;

      await act(async () => {
        es.emit('snapshot', { reasoning: '', content: '', status: 'running' });
        es.emit('delta', { content: 'Hi ' });
        es.emit('delta', { content: 'there' });
      });
      // user bubble + the streamed assistant bubble.
      await waitFor(() => expect(screen.getByTestId('count').textContent).toBe('2'));
      await waitFor(() => expect(screen.getByTestId('last').textContent).toBe('Hi there'));
      expect(screen.getByTestId('streaming').textContent).toBe('true');

      await act(async () => {
        es.emit('done', { content: 'Hi there', status: 'completed' });
      });
      await waitFor(() => expect(screen.getByTestId('streaming').textContent).toBe('false'));
      expect(screen.getByTestId('last').textContent).toBe('Hi there');
      // The subscription is closed once the run reaches a terminal state.
      expect(es.closed).toBe(true);
    });

    // Task 8: a run's terminal `error` field is a WIRE CODE
    // (errorLabelByCode), not prose -- the toast must show the localized
    // label, never the raw code a future backend and this frontend now both
    // know about (formatChatRunErrorCode, format.ts).
    it('a terminal run error is localized before it reaches the toast, not shown as a raw code', async () => {
      chatApi = makeChatApi([
        {
          id: 'c1',
          title: 'C1',
          created_at: T,
          updated_at: T,
          content: { settings: { model: 'gpt-oss-20b' }, messages: [] },
        },
      ]);
      renderProvider();
      await waitForReady();

      fireEvent.change(screen.getByLabelText('probe-input'), { target: { value: 'hello' } });
      fireEvent.click(screen.getByRole('button', { name: 'send' }));
      await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
      const es = FakeEventSource.instances.at(-1)!;

      await act(async () => {
        es.emit('done', { content: '', status: 'error', error: 'gateway.chat_run_timeout' });
      });

      expect(await screen.findByText(t.errorChatRunTimeout)).toBeTruthy();
      expect(screen.queryByText('gateway.chat_run_timeout')).toBeNull();
    });

    it("useChatStreaming reflects the store's streaming flag", async () => {
      chatApi = makeChatApi([
        {
          id: 'c1',
          title: 'C1',
          created_at: T,
          updated_at: T,
          content: { settings: { model: 'gpt-oss-20b' }, messages: [] },
        },
      ]);
      function HookPeek() {
        return <span data-testid="hook">{String(useChatStreaming())}</span>;
      }
      const onRefresh = vi.fn(async () => {});
      render(
        <ToastProvider>
          <ChatStoreProvider
            api={chatApi.api}
            models={models}
            tokens={[]}
            onRefresh={onRefresh}
            t={t}
            locale={locale}
          >
            <Probe />
            <HookPeek />
          </ChatStoreProvider>
        </ToastProvider>,
      );
      await waitForReady();

      expect(screen.getByTestId('hook').textContent).toBe('false');
      fireEvent.change(screen.getByLabelText('probe-input'), { target: { value: 'go' } });
      fireEvent.click(screen.getByRole('button', { name: 'send' }));

      await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
      const es = FakeEventSource.instances.at(-1)!;
      await act(async () => {
        es.emit('delta', { content: 'streaming now' });
      });
      await waitFor(() => expect(screen.getByTestId('hook').textContent).toBe('true'));

      await act(async () => {
        es.emit('done', { content: 'streaming now', status: 'completed' });
      });
      await waitFor(() => expect(screen.getByTestId('hook').textContent).toBe('false'));
    });

    it('two concurrent runs write to their own chats', async () => {
      chatApi = makeChatApi([
        {
          id: 'c1',
          title: 'C1',
          created_at: T2,
          updated_at: T2,
          content: { settings: { model: 'gpt-oss-20b' }, messages: [] },
        },
        {
          id: 'c2',
          title: 'C2',
          created_at: T1,
          updated_at: T1,
          content: { settings: { model: 'gpt-oss-20b' }, messages: [] },
        },
      ]);
      chatApi.spies.startChatRun = vi.fn(async (chatId: string) => ({
        run_id: `run_${chatId}`,
        chat_id: chatId,
        status: 'running' as const,
      }));
      renderProvider();
      await waitForReady(); // active = c1 (newest)
      expect(screen.getByTestId('active').textContent).toBe('c1');

      // Start a run in c1.
      fireEvent.change(screen.getByLabelText('probe-input'), { target: { value: 'in one' } });
      fireEvent.click(screen.getByRole('button', { name: 'send' }));
      await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
      const es1 = FakeEventSource.instances[0];

      // Switch to c2 (allowed while c1 runs) and start a run there too.
      fireEvent.click(screen.getByRole('button', { name: 'select-c2' }));
      await waitFor(() => expect(screen.getByTestId('active').textContent).toBe('c2'));
      fireEvent.change(screen.getByLabelText('probe-input'), { target: { value: 'in two' } });
      fireEvent.click(screen.getByRole('button', { name: 'send' }));
      await waitFor(() => expect(FakeEventSource.instances).toHaveLength(2));
      const es2 = FakeEventSource.instances[1];

      await act(async () => {
        es1.emit('delta', { content: 'answer one' });
        es2.emit('delta', { content: 'answer two' });
      });
      // The active chat (c2) shows its own answer.
      await waitFor(() => expect(screen.getByTestId('last').textContent).toBe('answer two'));

      // Switching back to c1 shows the background run's answer (kept in its buffer).
      fireEvent.click(screen.getByRole('button', { name: 'select-c1' }));
      await waitFor(() => expect(screen.getByTestId('active').textContent).toBe('c1'));
      await waitFor(() => expect(screen.getByTestId('last').textContent).toBe('answer one'));
    });
  });

  describe(`ChatStoreProvider reopen + interrupted (Task 3.4) [${locale}]`, () => {
    it('on load, subscribes to active runs and replays the snapshot', async () => {
      chatApi = makeChatApi([
        {
          id: 'c1',
          title: 'C1',
          created_at: T,
          updated_at: T,
          content: {
            settings: {},
            messages: [
              { id: 'u', role: 'user', content: 'hi' },
              { id: 'a', role: 'assistant', content: 'part', status: 'pending' },
            ],
          },
        },
      ]);
      chatApi.spies.activeChatRuns = vi.fn(async () => ({
        data: [{ chat_id: 'c1', run_id: 'run_x', status: 'running' as const }],
      }));
      renderProvider();
      await waitForReady();

      // The reopen subscribes to the still-running server run.
      await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
      const es = FakeEventSource.instances[0];
      expect(es.url).toContain('run_x');

      // The pending tail is NOT marked interrupted — the live run owns it.
      expect(screen.getByTestId('last-status').textContent).not.toBe('interrupted');

      // The replayed snapshot replaces the persisted partial with the live one.
      await act(async () => {
        es.emit('snapshot', { reasoning: '', content: 'part more', status: 'running' });
      });
      await waitFor(() => expect(screen.getByTestId('last').textContent).toBe('part more'));
    });

    // Regression for the "reopen fabricates a chat buffer" bug: a BACKGROUND
    // running chat (not the newest, so not activated on load) must have its buffer
    // seeded from the server doc BEFORE its run snapshot is applied. Otherwise the
    // snapshot fabricates a lone-assistant buffer on an empty base and the prior
    // user message vanishes when the user switches to that chat.
    it('reopening a background running chat shows its full transcript with the run snapshot on the tail', async () => {
      chatApi = makeChatApi([
        {
          id: 'c_new',
          title: 'Newer',
          created_at: T2,
          updated_at: T2,
          content: { settings: {}, messages: [{ id: 'n1', role: 'user', content: 'in newer' }] },
        },
        {
          id: 'c_old',
          title: 'Older',
          created_at: T1,
          updated_at: T1,
          content: {
            settings: {},
            messages: [
              { id: 'o1', role: 'user', content: 'older question' },
              { id: 'o2', role: 'assistant', content: 'partial', status: 'pending' },
            ],
          },
        },
      ]);
      chatApi.spies.activeChatRuns = vi.fn(async () => ({
        data: [{ chat_id: 'c_old', run_id: 'run_old', status: 'running' as const }],
      }));
      renderProvider();
      await waitForReady();
      // The newest chat is active; its transcript is intact, NOT clobbered by the
      // background run's snapshot.
      expect(screen.getByTestId('active').textContent).toBe('c_new');
      expect(screen.getByTestId('last').textContent).toBe('in newer');

      // The background chat's run is subscribed only AFTER its buffer is seeded
      // from the server doc, so its EventSource appears once that fetch resolves.
      await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
      const es = FakeEventSource.instances[0];
      expect(es.url).toContain('run_old');

      // The live run streams onto the seeded (full) transcript, not an empty base.
      await act(async () => {
        es.emit('snapshot', { reasoning: '', content: 'live answer', status: 'running' });
      });

      // Switch to the background chat: it shows the FULL transcript (the prior
      // user message survives) with the snapshot content on the trailing assistant.
      fireEvent.click(screen.getByRole('button', { name: 'select-c_old' }));
      await waitFor(() => expect(screen.getByTestId('active').textContent).toBe('c_old'));
      // 2 messages = user + assistant (a fabricated lone-assistant buffer -> 1).
      expect(screen.getByTestId('count').textContent).toBe('2');
      expect(screen.getByTestId('last').textContent).toBe('live answer');
    });

    // Regression (review Important 3): a late subscriber (reopen/reconnect just
    // after a run finished) receives the terminal state as a `snapshot`
    // (status completed/error/canceled) and then the stream closes with NO `done`
    // to follow. The snapshot handler must finalize like `done`, or the chat stays
    // marked running forever (spinner + Stop, inputs disabled) until a full reload.
    it('finalizes when the only event is a terminal snapshot (no done follows)', async () => {
      chatApi = makeChatApi([
        {
          id: 'c1',
          title: 'C1',
          created_at: T,
          updated_at: T,
          content: {
            settings: {},
            messages: [
              { id: 'u', role: 'user', content: 'q' },
              { id: 'a', role: 'assistant', content: 'partial', status: 'pending' },
            ],
          },
        },
      ]);
      chatApi.spies.activeChatRuns = vi.fn(async () => ({
        data: [{ chat_id: 'c1', run_id: 'run_term', status: 'running' as const }],
      }));
      renderProvider();
      await waitForReady();

      // The reopen subscribes to the (believed-running) server run.
      await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
      const es = FakeEventSource.instances[0];
      expect(es.url).toContain('run_term');
      await waitFor(() => expect(screen.getByTestId('streaming').textContent).toBe('true'));

      // The backend committed the canonical transcript before the (late) subscriber
      // connected; the run finished between subscribe and the first frame, so the
      // FIRST and only event is a terminal snapshot — no `done` will follow.
      const row = chatApi.rows.find((r) => r.id === 'c1')!;
      row.content = {
        settings: {},
        messages: [
          { id: 'u', role: 'user', content: 'q' },
          { id: 'a', role: 'assistant', content: 'final answer', status: 'complete' },
        ],
      };
      await act(async () => {
        es.emit('snapshot', { reasoning: '', content: 'final answer', status: 'completed' });
      });

      // The chat must NOT stay running (pre-fix it did — snapshot ignored status),
      // the content is shown, and the subscription is closed.
      await waitFor(() => expect(screen.getByTestId('streaming').textContent).toBe('false'));
      expect(screen.getByTestId('last').textContent).toBe('final answer');
      expect(screen.getByTestId('last-status').textContent).toBe('complete');
      expect(es.closed).toBe(true);
    });

    // A run that finished in a prior session is NOT in the (now running-only)
    // active list, so the reopen must not resubscribe to it — the transcript is
    // read straight from the server doc, intact.
    it('does not resubscribe to a chat whose run already finished', async () => {
      chatApi = makeChatApi([
        {
          id: 'c1',
          title: 'C1',
          created_at: T,
          updated_at: T,
          content: {
            settings: {},
            messages: [
              { id: 'u', role: 'user', content: 'prior question' },
              { id: 'a', role: 'assistant', content: 'prior answer', status: 'complete' },
            ],
          },
        },
      ]);
      chatApi.spies.activeChatRuns = vi.fn(async () => ({ data: [] as ActiveChatRun[] }));
      renderProvider();
      await waitForReady();

      // No EventSource opened — nothing running to resubscribe to.
      expect(FakeEventSource.instances).toHaveLength(0);
      // The transcript is loaded straight from the server doc, intact.
      expect(screen.getByTestId('count').textContent).toBe('2');
      expect(screen.getByTestId('last').textContent).toBe('prior answer');
      expect(screen.getByTestId('last-status').textContent).toBe('complete');
    });

    it('a pending transcript with no active run shows interrupted (no subscription)', async () => {
      chatApi = makeChatApi([
        {
          id: 'c1',
          title: 'C1',
          created_at: T,
          updated_at: T,
          content: {
            settings: {},
            messages: [{ id: 'a', role: 'assistant', content: 'half', status: 'pending' }],
          },
        },
      ]);
      chatApi.spies.activeChatRuns = vi.fn(async () => ({ data: [] as ActiveChatRun[] }));
      renderProvider();
      await waitForReady();

      // No run to subscribe to.
      expect(FakeEventSource.instances).toHaveLength(0);
      // Partial output is kept, but flagged interrupted (gateway restart lost the run).
      expect(screen.getByTestId('last').textContent).toBe('half');
      await waitFor(() =>
        expect(screen.getByTestId('last-status').textContent).toBe('interrupted'),
      );
    });
  });

  describe(`ChatStoreProvider refetch-on-done (review follow-up #A) [${locale}]`, () => {
    it('refetches and adopts the server-canonical transcript on done', async () => {
      chatApi = makeChatApi([
        {
          id: 'c1',
          title: 'C1',
          created_at: T,
          updated_at: T,
          content: { settings: { model: 'gpt-oss-20b' }, messages: [] },
        },
      ]);
      renderProvider();
      await waitForReady();

      fireEvent.change(screen.getByLabelText('probe-input'), { target: { value: 'hello' } });
      fireEvent.click(screen.getByRole('button', { name: 'send' }));
      await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
      const es = FakeEventSource.instances[0];

      await act(async () => {
        es.emit('delta', { content: 'streamed-fe' });
      });
      await waitFor(() => expect(screen.getByTestId('last').textContent).toBe('streamed-fe'));

      const chatCallsBeforeDone = chatApi.spies.chat.mock.calls.length;
      // Simulate the backend committing the canonical transcript (server ids +
      // status) BEFORE `done`. The FE must adopt THIS on refetch, not its own buffer.
      const row = chatApi.rows.find((r) => r.id === 'c1')!;
      row.content = {
        settings: {},
        messages: [
          { id: 'msg_srv_user', role: 'user', content: 'hello' },
          {
            id: 'msg_srv_asst',
            role: 'assistant',
            content: 'canonical answer',
            status: 'complete',
          },
        ],
      };

      await act(async () => {
        es.emit('done', { content: 'streamed-fe', status: 'completed' });
      });

      // The GET refetch fires after done...
      await waitFor(() =>
        expect(chatApi.spies.chat.mock.calls.length).toBeGreaterThan(chatCallsBeforeDone),
      );
      // ...and the shown transcript is the server's canonical doc (not the FE buffer).
      await waitFor(() => expect(screen.getByTestId('last').textContent).toBe('canonical answer'));
      expect(screen.getByTestId('last-status').textContent).toBe('complete');
    });
  });

  describe(`ChatStoreProvider run kind + elapsed anchor (Task 14) [${locale}]`, () => {
    // Review round 2, Finding 1: the client's "is this thread's first send"
    // guess (chatKind, derived from messagesRef.current.length === 0) can
    // disagree with the server's authoritative one (PrepareChatRun, keyed on
    // len(doc.Messages) > 0) whenever the client's transcript view is stale
    // -- a second tab still holding messages = [] after another tab's first
    // send would guess from the picked model and could pin the WRONG kind
    // locally. runKind must come from the run's own report (here simulated
    // via the snapshot's `kind`), never from chatKind, so the pending
    // render always matches what the executor is actually running. This
    // forces the disagreement directly: chatKind stays '' (a text thread,
    // by the store's own reckoning) while the run's own snapshot reports
    // 'image' -- runKind must follow the run, not the thread setting.
    it("reads the run's own reported kind, not the thread's chatKind, so a stale client view cannot mislabel the pending render", async () => {
      chatApi = makeChatApi([
        {
          id: 'c1',
          title: 'C1',
          created_at: T,
          updated_at: T,
          content: { settings: { model: 'gpt-oss-20b' }, messages: [] },
        },
      ]);
      renderProvider();
      await waitForReady();
      expect(screen.getByTestId('chat-kind').textContent).toBe('');

      fireEvent.change(screen.getByLabelText('probe-input'), { target: { value: 'hello' } });
      fireEvent.click(screen.getByRole('button', { name: 'send' }));
      await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
      const es = FakeEventSource.instances[0];

      await act(async () => {
        es.emit('snapshot', { status: 'running', kind: 'image', elapsed_ms: 0 });
      });

      expect(screen.getByTestId('run-kind').textContent).toBe('image');
      // chatKind is a DIFFERENT fact (the thread's own settings-level pin)
      // and must stay whatever it already was -- it is deliberately not
      // re-derived from the run.
      expect(screen.getByTestId('chat-kind').textContent).toBe('');
    });

    // /v1/images/generations refuses `stream`, so an image run's SSE stream
    // carries exactly ONE snapshot near the start and then nothing until
    // `done` -- tens of seconds to minutes later. The composer's pending
    // clock must still tick through that whole gap, so this pins the anchor
    // math end to end: seed from a snapshot's elapsed_ms, then advance real
    // (fake) time with NO further server event, and the exposed
    // `runElapsedMs` must have grown by the same amount. A client that
    // recomputed elapsed time from the moment of Send (ignoring elapsed_ms)
    // would still pass the "grows by ~3000" half of this test, so the FIRST
    // assertion (the value right after the snapshot is ~5000, not ~0) is
    // what pins the anchor itself, not just the ticking.
    it("anchors on the run's own snapshot elapsed_ms and interpolates locally between snapshots", async () => {
      chatApi = makeChatApi([
        {
          id: 'c1',
          title: 'C1',
          created_at: T,
          updated_at: T,
          content: { settings: { model: 'gpt-oss-20b' }, messages: [] },
        },
      ]);
      // shouldAdvanceTime keeps wall-clock flowing so RTL's waitFor (a real
      // setTimeout poll under the hood) still resolves -- same reasoning as
      // the refreshModels-poll test above.
      vi.useFakeTimers({ shouldAdvanceTime: true });
      try {
        renderProvider();
        await waitForReady();

        fireEvent.change(screen.getByLabelText('probe-input'), { target: { value: 'hello' } });
        fireEvent.click(screen.getByRole('button', { name: 'send' }));
        await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
        const es = FakeEventSource.instances[0];

        // The server says 5s had already elapsed (e.g. admission queueing)
        // before this snapshot was taken.
        await act(async () => {
          es.emit('snapshot', { status: 'running', elapsed_ms: 5000 });
        });
        const afterSnapshot = Number(screen.getByTestId('run-elapsed-ms').textContent);
        expect(afterSnapshot).toBeGreaterThanOrEqual(5000);
        expect(afterSnapshot).toBeLessThan(5050);

        // No further snapshot arrives (the whole point of a buffered image
        // run) -- the clock must still advance from LOCAL interpolation
        // alone. runElapsedMs is read at render time (ImagePendingTurn, not
        // ChatStore, owns the actual once-a-second tick a user sees -- see
        // ChatMessage.test.tsx), so force a fresh render via an unrelated
        // store action, the same way the freeze test below does.
        await act(async () => {
          await vi.advanceTimersByTimeAsync(3000);
        });
        fireEvent.click(screen.getByRole('button', { name: 'set-system' }));
        const afterTick = Number(screen.getByTestId('run-elapsed-ms').textContent);
        expect(afterTick - afterSnapshot).toBeGreaterThanOrEqual(2900);
        expect(afterTick).toBeLessThan(8100);
      } finally {
        vi.useRealTimers();
      }
    });

    // A finished run lingers in the registry for the eviction grace period,
    // and elapsedMsLocked's own backend contract (chat_runs.go) FREEZES at
    // the run's real duration once terminal -- a late subscriber must be
    // told how long the run TOOK, not how long ago it happened to be read.
    // Without the `run.status !== 'running'` guard in elapsedMsOf, this value
    // would silently keep growing on every unrelated re-render for as long as
    // the chat stays open after completion, even though nothing is currently
    // rendering it.
    it("freezes the elapsed value at the run's final duration once terminal, instead of continuing to grow", async () => {
      chatApi = makeChatApi([
        {
          id: 'c1',
          title: 'C1',
          created_at: T,
          updated_at: T,
          content: { settings: { model: 'gpt-oss-20b' }, messages: [] },
        },
      ]);
      vi.useFakeTimers({ shouldAdvanceTime: true });
      try {
        renderProvider();
        await waitForReady();

        fireEvent.change(screen.getByLabelText('probe-input'), { target: { value: 'hello' } });
        fireEvent.click(screen.getByRole('button', { name: 'send' }));
        await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
        const es = FakeEventSource.instances[0];

        await act(async () => {
          es.emit('done', { status: 'completed', content: 'answer', elapsed_ms: 8000 });
        });
        const afterDone = Number(screen.getByTestId('run-elapsed-ms').textContent);
        expect(afterDone).toBeGreaterThanOrEqual(8000);
        expect(afterDone).toBeLessThan(8050);

        // Let a lot of (fake) wall-clock time pass, and force a fresh render
        // via an unrelated store action -- a naive "always add
        // performance.now() - anchor" implementation would show ~28000 here.
        await act(async () => {
          await vi.advanceTimersByTimeAsync(20000);
        });
        fireEvent.click(screen.getByRole('button', { name: 'set-system' }));
        const afterWait = Number(screen.getByTestId('run-elapsed-ms').textContent);
        expect(afterWait).toBe(afterDone);
      } finally {
        vi.useRealTimers();
      }
    });
  });

  describe(`ChatStoreProvider finishRun keeps structured content (Task 10) [${locale}]`, () => {
    // finishRun's terminal prune exists to drop a bubble a run never wrote
    // anything into. Its predicate used to be
    //   (typeof last.content === 'string' ? last.content.length === 0 : true)
    // -- so ANY non-string content counted as empty. An image turn's content
    // is an ARRAY of parts, so a one-image turn was called empty and its
    // bubble was deleted the instant `done` arrived, even though generation
    // and persistence both succeeded.
    //
    // THE PAYLOAD BELOW IS THE REAL WIRE SHAPE, and that is the whole point.
    // This test used to emit `content: [{type:'image_url',...}]`, which the
    // backend cannot produce: `content` is the run's streamed TEXT buffer and
    // is a Go string. The real terminal event of an image run carries its
    // structured content under `content_parts` (runEvent.ContentParts,
    // chat_runs.go) -- and before that field existed it carried NO content at
    // all, so the prune fix this test guards was never on a path the product
    // reached. Emitting the shape the backend actually sends is what makes it
    // load-bearing.
    //
    // Both tests below force the post-`done` canonical refetch
    // (adoptCanonicalTranscript) to fail. That refetch unconditionally
    // replaces the whole buffer with whatever the server doc holds, so a
    // succeeding refetch here would silently overwrite finishRun's own edit
    // and mask either direction of a broken predicate: it could put a wrongly
    // deleted bubble back (masking test 1 against a broken fix) or reinstate
    // an already-pruned bubble (masking test 2 against a fix that stopped
    // pruning). Rejecting it pins each assertion to finishRun's own buffer
    // edit, which is the thing under test.
    it('keeps an assistant bubble whose content is a structured image array', async () => {
      chatApi = makeChatApi([
        {
          id: 'c1',
          title: 'C1',
          created_at: T,
          updated_at: T,
          content: { settings: { model: 'gpt-oss-20b' }, messages: [] },
        },
      ]);
      renderProvider();
      await waitForReady();
      chatApi.spies.chat.mockRejectedValue(new Error('refetch unavailable'));

      fireEvent.change(screen.getByLabelText('probe-input'), { target: { value: 'draw a cat' } });
      fireEvent.click(screen.getByRole('button', { name: 'send' }));
      await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
      const es = FakeEventSource.instances[0];

      await act(async () => {
        // Byte-for-byte what the backend emits for a successful image run: the
        // terminal status, the per-run facts, NO `content` (the text buffer is
        // empty and omitempty drops it), and the committed parts under
        // `content_parts`.
        es.emit('done', {
          status: 'completed',
          kind: 'image',
          elapsed_ms: 42_000,
          content_parts: [{ type: 'image_url', image_url: { url: 'data:image/png;base64,AA==' } }],
        });
      });

      await waitFor(() => expect(screen.getByTestId('streaming').textContent).toBe('false'));
      // user turn + the assistant bubble holding the generated image.
      expect(screen.getByTestId('count').textContent).toBe('2');
      // ...and the bubble holds the IMAGE, not an empty string that merely
      // survived the prune. Without the content_parts read in useChatRuns the
      // count above can still be 2 for a bubble that renders nothing.
      expect(screen.getByTestId('last-parts').textContent).toBe(
        'image_url:data:image/png;base64,AA==',
      );
    });

    // The regression guard: the prune must keep doing its actual job on the
    // shape it was written for. A fix that simply stops pruning (e.g. always
    // keeping the tail) would pass the test above and break every run that
    // errors/cancels before writing anything.
    it('still prunes an assistant bubble with an empty string and no reasoning', async () => {
      chatApi = makeChatApi([
        {
          id: 'c1',
          title: 'C1',
          created_at: T,
          updated_at: T,
          content: { settings: { model: 'gpt-oss-20b' }, messages: [] },
        },
      ]);
      renderProvider();
      await waitForReady();
      chatApi.spies.chat.mockRejectedValue(new Error('refetch unavailable'));

      fireEvent.change(screen.getByLabelText('probe-input'), { target: { value: 'hello' } });
      fireEvent.click(screen.getByRole('button', { name: 'send' }));
      await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
      const es = FakeEventSource.instances[0];

      await act(async () => {
        es.emit('done', { content: '', status: 'completed' });
      });

      await waitFor(() => expect(screen.getByTestId('streaming').textContent).toBe('false'));
      // Only the user turn remains -- the empty assistant bubble was pruned.
      expect(screen.getByTestId('count').textContent).toBe('1');
    });
  });

  describe(`ChatStoreProvider never saves a transcript it could not reconcile [${locale}]`, () => {
    // The second half of the whole-branch review's finding 1/2. The
    // post-`done` canonical refetch is best-effort by design, but its failure
    // used to be swallowed AND followed, ~800 ms later, by a debounced PUT of
    // the local buffer -- and a PUT full-replaces the stored content blob with
    // no merge. So a refetch that failed (it downloads the whole document,
    // which for an image thread is megabytes) made the portal overwrite the
    // server's committed turn with a transcript that did not contain it.
    //
    // The refusal is armed BEFORE the await, not in the catch: a refetch can
    // easily outlast the 800 ms debounce, and by the time its rejection lands
    // there would be nothing left to cancel. So the test advances well past
    // the debounce with the rejection still pending, and only then rejects.
    it('saves nothing after a failed canonical refetch, and says so', async () => {
      chatApi = makeChatApi([
        {
          id: 'c1',
          title: 'C1',
          created_at: T,
          updated_at: T,
          content: { settings: { model: 'gpt-oss-20b' }, messages: [] },
        },
      ]);
      vi.useFakeTimers({ shouldAdvanceTime: true });
      try {
        renderProvider();
        await waitForReady();
        chatApi.spies.saveChat.mockClear();
        let failRefetch: (() => void) | undefined;
        chatApi.spies.chat.mockImplementation(
          () =>
            new Promise((_resolve, reject) => {
              failRefetch = () => reject(new Error('refetch unavailable'));
            }),
        );

        fireEvent.change(screen.getByLabelText('probe-input'), { target: { value: 'draw a cat' } });
        fireEvent.click(screen.getByRole('button', { name: 'send' }));
        await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
        const es = FakeEventSource.instances[0];

        await act(async () => {
          es.emit('done', {
            status: 'completed',
            kind: 'image',
            content_parts: [
              { type: 'image_url', image_url: { url: 'data:image/png;base64,AA==' } },
            ],
          });
        });
        await waitFor(() => expect(screen.getByTestId('streaming').textContent).toBe('false'));

        // Well past SAVE_DEBOUNCE_MS while the refetch is still in flight.
        await act(async () => {
          await vi.advanceTimersByTimeAsync(5000);
        });
        expect(chatApi.spies.saveChat).not.toHaveBeenCalled();

        // ...and still nothing once it actually fails.
        await act(async () => {
          failRefetch?.();
          await vi.advanceTimersByTimeAsync(5000);
        });
        expect(chatApi.spies.saveChat).not.toHaveBeenCalled();
        // The image is still on screen (the terminal event carried it), so the
        // thread looks healthy -- which is exactly why the user has to be told
        // that this tab has stopped persisting it.
        expect(screen.getByTestId('last-parts').textContent).toBe(
          'image_url:data:image/png;base64,AA==',
        );
        expect(await screen.findByText(t.errorChatTranscriptStale)).toBeInTheDocument();
      } finally {
        vi.useRealTimers();
      }
    });

    // flushSave is not the only writer: the pagehide keepalive and the
    // unmount flush each PUT directly, bypassing it. Three doors on the same
    // room, so the refusal has to be on all three -- and the two quiet ones
    // are the worse ones to get wrong, because a keepalive PUT's outcome is
    // invisible on the client side by construction.
    it('writes nothing on pagehide or unmount either, after a failed canonical refetch', async () => {
      chatApi = makeChatApi([
        {
          id: 'c1',
          title: 'C1',
          created_at: T,
          updated_at: T,
          content: { settings: { model: 'gpt-oss-20b' }, messages: [] },
        },
      ]);
      const { unmount } = renderProvider();
      await waitForReady();
      chatApi.spies.saveChat.mockClear();
      chatApi.spies.saveChatKeepalive.mockClear();
      chatApi.spies.chat.mockRejectedValue(new Error('refetch unavailable'));

      fireEvent.change(screen.getByLabelText('probe-input'), { target: { value: 'hello' } });
      fireEvent.click(screen.getByRole('button', { name: 'send' }));
      await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
      const es = FakeEventSource.instances[0];

      await act(async () => {
        es.emit('done', { status: 'completed', content: 'an answer' });
      });
      await waitFor(() => expect(screen.getByTestId('streaming').textContent).toBe('false'));
      await screen.findByText(t.errorChatTranscriptStale);

      await act(async () => {
        window.dispatchEvent(new Event('pagehide'));
      });
      expect(chatApi.spies.saveChatKeepalive).not.toHaveBeenCalled();

      await act(async () => {
        unmount();
      });
      expect(chatApi.spies.saveChat).not.toHaveBeenCalled();
    });

    // A rename carries no transcript, so it is not one of the writers the
    // tests above hold to refusing a stale chat. The backend's PUT keeps the
    // stored content when the body carries no `content` key, so a rename
    // sends only the title: it never reads the chat's document at all,
    // active, background, or stale alike, and a chat's transcript staleness
    // has nothing to do with whether its title can be changed.
    it('renames a non-active chat by sending only the title, with no document fetch', async () => {
      chatApi = makeChatApi([
        {
          id: 'c_alpha',
          title: 'Alpha',
          created_at: T2,
          updated_at: T2,
          content: { settings: { model: 'gpt-oss-20b' }, messages: [] },
        },
        {
          id: 'c_beta',
          title: 'Beta',
          created_at: T1,
          updated_at: T1,
          content: { settings: { model: 'gpt-oss-20b' }, messages: [] },
        },
      ]);
      renderProvider();
      await waitForReady();
      // c_alpha (newest) is the active chat; c_beta is the one renamed below.
      await waitFor(() => expect(screen.getByTestId('active').textContent).toBe('c_alpha'));
      chatApi.spies.saveChat.mockClear();
      chatApi.spies.chat.mockClear();

      fireEvent.click(screen.getByRole('button', { name: 'rename-c_beta' }));

      await waitFor(() => expect(chatApi.spies.saveChat).toHaveBeenCalledTimes(1));
      expect(chatApi.spies.saveChat).toHaveBeenCalledWith('c_beta', { title: 'Renamed' });
      expect(chatApi.spies.chat).not.toHaveBeenCalledWith('c_beta');
    });

    // The other side of the same switch: an adopt that SUCCEEDS must leave
    // persistence working. Without this a fix that simply stopped saving
    // after every run would pass the test above and silently break autosave
    // for every chat.
    it('saves again once the canonical refetch succeeds', async () => {
      chatApi = makeChatApi([
        {
          id: 'c1',
          title: 'C1',
          created_at: T,
          updated_at: T,
          content: { settings: { model: 'gpt-oss-20b' }, messages: [] },
        },
      ]);
      vi.useFakeTimers({ shouldAdvanceTime: true });
      try {
        renderProvider();
        await waitForReady();
        chatApi.spies.saveChat.mockClear();

        fireEvent.change(screen.getByLabelText('probe-input'), { target: { value: 'hello' } });
        fireEvent.click(screen.getByRole('button', { name: 'send' }));
        await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
        const es = FakeEventSource.instances[0];

        await act(async () => {
          es.emit('done', { status: 'completed', content: 'an answer' });
        });
        await waitFor(() => expect(screen.getByTestId('streaming').textContent).toBe('false'));

        fireEvent.click(screen.getByRole('button', { name: 'set-system' }));
        await act(async () => {
          await vi.advanceTimersByTimeAsync(2000);
        });
        await waitFor(() => expect(chatApi.spies.saveChat).toHaveBeenCalled());
      } finally {
        vi.useRealTimers();
      }
    });
  });

  describe(`ChatStoreProvider stop/cancel + delete (Task 3.5) [${locale}]`, () => {
    it("stop cancels the active chat's run", async () => {
      chatApi = makeChatApi([
        {
          id: 'c1',
          title: 'C1',
          created_at: T,
          updated_at: T,
          content: { settings: { model: 'gpt-oss-20b' }, messages: [] },
        },
      ]);
      renderProvider();
      await waitForReady();

      fireEvent.change(screen.getByLabelText('probe-input'), { target: { value: 'hi' } });
      fireEvent.click(screen.getByRole('button', { name: 'send' }));
      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalled());
      await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));

      fireEvent.click(screen.getByRole('button', { name: 'stop' }));
      await waitFor(() => expect(chatApi.spies.cancelChatRun).toHaveBeenCalledWith('c1', 'run_1'));
    });

    it('deleting a chat closes its EventSource', async () => {
      chatApi = makeChatApi([
        {
          id: 'c1',
          title: 'C1',
          created_at: T2,
          updated_at: T2,
          content: { settings: { model: 'gpt-oss-20b' }, messages: [] },
        },
        {
          id: 'c2',
          title: 'C2',
          created_at: T1,
          updated_at: T1,
          content: { settings: { model: 'gpt-oss-20b' }, messages: [] },
        },
      ]);
      renderProvider();
      await waitForReady();
      expect(screen.getByTestId('active').textContent).toBe('c1');

      fireEvent.change(screen.getByLabelText('probe-input'), { target: { value: 'hi' } });
      fireEvent.click(screen.getByRole('button', { name: 'send' }));
      await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
      const es = FakeEventSource.instances[0];

      fireEvent.click(screen.getByRole('button', { name: 'delete-c1' }));
      await waitFor(() => expect(es.closed).toBe(true));
    });
  });

  describe(`ChatStoreProvider busy guard (review follow-up #B) [${locale}]`, () => {
    it('sending to a chat with a live run shows the chatBusy toast (no second run)', async () => {
      chatApi = makeChatApi([
        {
          id: 'c1',
          title: 'C1',
          created_at: T,
          updated_at: T,
          content: { settings: { model: 'gpt-oss-20b' }, messages: [] },
        },
      ]);
      renderProvider();
      await waitForReady();

      // Start a run and leave it running.
      fireEvent.change(screen.getByLabelText('probe-input'), { target: { value: 'first' } });
      fireEvent.click(screen.getByRole('button', { name: 'send' }));
      await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));

      // A second send to the same busy chat is refused with a toast.
      fireEvent.change(screen.getByLabelText('probe-input'), { target: { value: 'second' } });
      fireEvent.click(screen.getByRole('button', { name: 'send' }));

      expect(await screen.findByText(t.chatBusy)).toBeTruthy();
      // The guard held: only the first run was started.
      expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1);
    });
  });

  describe(`ChatStoreProvider offered models [${locale}]`, () => {
    // The chat sends a text request (chat completions) or an image request,
    // so it offers a model whose offered flavors include openai or
    // openai_images, and openai_images counts only behind an image verdict.
    it('offers exactly the models the chat can send a request the gateway serves', async () => {
      const shape = (id: string, flavors: string[], image?: boolean): ModelOption => ({
        id,
        display_name: id,
        flavors,
        loading_on_count: 0,
        image,
      });
      renderProvider([], {
        models: [
          shape('text-model', ['openai', 'anthropic']),
          shape('anthropic-only', ['anthropic']),
          shape('no-flavor', []),
          shape('sd-verdict', ['openai_images'], true),
          shape('sd-no-verdict', ['openai_images'], false),
          // A text model on a mixed application without an image verdict:
          // offered for its openai flavor.
          shape('mixed-text', ['openai', 'openai_images'], false),
          shape('anthropic-images-no-verdict', ['anthropic', 'openai_images'], false),
          shape('anthropic-images-verdict', ['anthropic', 'openai_images'], true),
        ],
      });
      await waitForReady();
      // A fresh chat selects no model, so modelOptions is chatModels itself.
      expect(screen.getByTestId('model').textContent).toBe('');
      expect(screen.getByTestId('model-options').textContent).toBe(
        'text-model,sd-verdict,mixed-text,anthropic-images-verdict',
      );
    });
  });

  describe(`ChatStoreProvider remembers an unavailable model [${locale}]`, () => {
    it('keeps a saved model not in chatModels selected, unavailable, and never re-persists it', async () => {
      chatApi = makeChatApi([
        {
          id: 'c1',
          title: 'C1',
          created_at: T,
          updated_at: T,
          content: { settings: { model: 'unavailable-model' }, messages: [] },
        },
      ]);
      renderProvider();
      await waitForReady();

      expect(screen.getByTestId('model').textContent).toBe('unavailable-model');
      expect(screen.getByTestId('model-available').textContent).toBe('false');
      // The saved model is injected into modelOptions alongside the reachable ones.
      expect(screen.getByTestId('model-options').textContent).toContain('unavailable-model');

      // Give the (removed) coercion effect + the debounced save every chance to
      // fire; the fix means neither ever runs for an unavailable saved model.
      await new Promise((resolve) => setTimeout(resolve, 1100));
      expect(screen.getByTestId('model').textContent).toBe('unavailable-model');
      expect(chatApi.spies.saveChat).not.toHaveBeenCalled();
    });

    it('keeps the saved model visible even when chatModels is empty (all unreachable)', async () => {
      chatApi = makeChatApi([
        {
          id: 'c1',
          title: 'C1',
          created_at: T,
          updated_at: T,
          content: { settings: { model: 'unavailable-model' }, messages: [] },
        },
      ]);
      renderProvider([], { models: [] });
      await waitForReady();

      expect(screen.getByTestId('model').textContent).toBe('unavailable-model');
      expect(screen.getByTestId('model-available').textContent).toBe('false');
      expect(screen.getByTestId('model-options').textContent).toBe('unavailable-model');
    });

    it('does NOT auto-select a model for a fresh chat: it stays empty (clearable/searchable)', async () => {
      renderProvider();
      await waitForReady();

      // No coercion to chatModels[0]: an empty selection persists so the field can be
      // cleared and searched. modelAvailable is false until the user picks one.
      expect(screen.getByTestId('model').textContent).toBe('');
      expect(screen.getByTestId('model-available').textContent).toBe('false');
      // Picking a model then works normally.
      fireEvent.click(screen.getByRole('button', { name: 'set-model-alt' }));
      expect(screen.getByTestId('model').textContent).toBe(models[0].id);
      expect(screen.getByTestId('model-available').textContent).toBe('true');
    });

    it('selecting an available model flips modelAvailable back to true', async () => {
      chatApi = makeChatApi([
        {
          id: 'c1',
          title: 'C1',
          created_at: T,
          updated_at: T,
          content: { settings: { model: 'unavailable-model' }, messages: [] },
        },
      ]);
      renderProvider();
      await waitForReady();
      expect(screen.getByTestId('model-available').textContent).toBe('false');

      fireEvent.click(screen.getByRole('button', { name: 'set-model-alt' }));

      expect(screen.getByTestId('model').textContent).toBe(models[0].id);
      expect(screen.getByTestId('model-available').textContent).toBe('true');
    });

    it('send() is blocked with a toast while the model is unavailable, and allowed once it is', async () => {
      chatApi = makeChatApi([
        {
          id: 'c1',
          title: 'C1',
          created_at: T,
          updated_at: T,
          content: { settings: { model: 'unavailable-model' }, messages: [] },
        },
      ]);
      renderProvider();
      await waitForReady();

      fireEvent.change(screen.getByLabelText('probe-input'), { target: { value: 'hello' } });
      fireEvent.click(screen.getByRole('button', { name: 'send' }));

      expect(await screen.findByText(t.chatModelUnavailable)).toBeTruthy();
      expect(chatApi.spies.startChatRun).not.toHaveBeenCalled();

      // Switching to an available model unblocks sending.
      fireEvent.click(screen.getByRole('button', { name: 'set-model-alt' }));
      fireEvent.click(screen.getByRole('button', { name: 'send' }));

      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalled());
    });

    it("resolves a run-as token's per-model override map by the requested model (no catch-all)", async () => {
      const twoModels: ModelOption[] = [
        { id: 'model-a', display_name: 'A', flavors: ['openai'], loading_on_count: 0 },
        { id: 'model-b', display_name: 'B', flavors: ['openai'], loading_on_count: 0 },
      ];
      const mapToken: PortalToken = {
        id: 'tok_map',
        name: 'Map Token',
        secret_prefix: 'dev-secr',
        status: 'active',
        scopes: ['gateway:use'],
        expires_at: null,
        last_used_at: null,
        created_at: T,
        model_override: '', // no catch-all
        model_override_map: { 'model-b': { to: 'model-a', offer: false, hide_target: false } }, // requesting model-b -> model-a
        log_communication: false,
        secret: false,
        is_chat_session: false,
        deletable: true,
      };
      chatApi = makeChatApi([
        {
          id: 'c1',
          title: 'C1',
          created_at: T,
          updated_at: T,
          content: { settings: { model: 'model-a' }, messages: [] },
        },
      ]);
      renderProvider([mapToken], { models: twoModels, altModelId: 'model-b' });
      await waitForReady();

      fireEvent.click(screen.getByRole('button', { name: `select-token-${mapToken.id}` }));
      // Current model is model-a: NOT a map key and no catch-all -> no override, and
      // the picker is NOT locked (there are per-model entries).
      expect(screen.getByTestId('override-model').textContent).toBe('');
      expect(screen.getByTestId('override-locks').textContent).toBe('false');

      // Switch the requested model to model-b -> the map entry fires -> model-a.
      fireEvent.click(screen.getByRole('button', { name: 'set-model-alt' }));
      expect(screen.getByTestId('override-model').textContent).toBe('model-a');
    });

    it('locks the model picker only for a pure catch-all override (no map entries)', async () => {
      const catchAllToken: PortalToken = {
        id: 'tok_catch',
        name: 'Catch Token',
        secret_prefix: 'dev-secr',
        status: 'active',
        scopes: ['gateway:use'],
        expires_at: null,
        last_used_at: null,
        created_at: T,
        model_override: 'gpt-oss-20b', // catch-all, no map
        log_communication: false,
        secret: false,
        is_chat_session: false,
        deletable: true,
      };
      chatApi = makeChatApi([
        {
          id: 'c1',
          title: 'C1',
          created_at: T,
          updated_at: T,
          content: { settings: { model: 'gpt-oss-20b' }, messages: [] },
        },
      ]);
      renderProvider([catchAllToken]);
      await waitForReady();

      fireEvent.click(screen.getByRole('button', { name: `select-token-${catchAllToken.id}` }));
      // Pure catch-all forces every model -> picker locked, override shows the target.
      expect(screen.getByTestId('override-locks').textContent).toBe('true');
      expect(screen.getByTestId('override-model').textContent).toBe('gpt-oss-20b');
    });

    it("blocks send when a run-as token's model_override is unavailable, even though the chat's own model is fine", async () => {
      const overrideToken: PortalToken = {
        id: 'tok_override',
        name: 'Override Token',
        secret_prefix: 'dev-secr',
        status: 'active',
        scopes: ['gateway:use'],
        expires_at: null,
        last_used_at: null,
        created_at: T,
        model_override: 'unavailable-override',
        log_communication: false,
        secret: false,
        is_chat_session: false,
        deletable: true,
      };
      chatApi = makeChatApi([
        {
          id: 'c1',
          title: 'C1',
          created_at: T,
          updated_at: T,
          content: { settings: { model: models[0].id }, messages: [] },
        },
      ]);
      renderProvider([overrideToken]);
      await waitForReady();
      // The chat's own model is available before the override is selected.
      expect(screen.getByTestId('model-available').textContent).toBe('true');

      fireEvent.click(screen.getByRole('button', { name: `select-token-${overrideToken.id}` }));
      expect(screen.getByTestId('model-available').textContent).toBe('false');

      fireEvent.change(screen.getByLabelText('probe-input'), { target: { value: 'hello' } });
      fireEvent.click(screen.getByRole('button', { name: 'send' }));

      expect(await screen.findByText(t.chatModelUnavailable)).toBeTruthy();
      expect(chatApi.spies.startChatRun).not.toHaveBeenCalled();
    });

    it('polls refreshModels while unavailable and stops once the model becomes available', async () => {
      chatApi = makeChatApi([
        {
          id: 'c1',
          title: 'C1',
          created_at: T,
          updated_at: T,
          content: { settings: { model: 'unavailable-model' }, messages: [] },
        },
      ]);
      const refreshModels = vi.fn();
      // Fake timers must be installed BEFORE the provider mounts: the poll effect
      // calls the real setInterval as soon as it renders unavailable, and faking
      // timers afterwards would not retroactively intercept that call.
      // shouldAdvanceTime keeps wall-clock time flowing too, so RTL's waitFor
      // (which polls via a real setTimeout under the hood) still resolves.
      vi.useFakeTimers({ shouldAdvanceTime: true });
      try {
        renderProvider([], { refreshModels });
        await waitForReady();
        expect(screen.getByTestId('model-available').textContent).toBe('false');

        await act(async () => {
          await vi.advanceTimersByTimeAsync(15000);
        });
        expect(refreshModels).toHaveBeenCalledTimes(1);

        await act(async () => {
          await vi.advanceTimersByTimeAsync(15000);
        });
        expect(refreshModels).toHaveBeenCalledTimes(2);

        // Once available, the poll stops — no further calls, even much later.
        fireEvent.click(screen.getByRole('button', { name: 'set-model-alt' }));
        expect(screen.getByTestId('model-available').textContent).toBe('true');

        await act(async () => {
          await vi.advanceTimersByTimeAsync(60000);
        });
        expect(refreshModels).toHaveBeenCalledTimes(2);
      } finally {
        vi.useRealTimers();
      }
    });

    it('regenerate is blocked (no run, no transcript truncation, toast) while the model is unavailable, and works once available', async () => {
      chatApi = makeChatApi([
        {
          id: 'c1',
          title: 'C1',
          created_at: T,
          updated_at: T,
          content: {
            settings: { model: 'unavailable-model' },
            messages: [
              { id: 'u1', role: 'user', content: 'question' },
              { id: 'a1', role: 'assistant', content: 'answer', status: 'complete' },
            ],
          },
        },
      ]);
      renderProvider();
      await waitForReady();
      expect(screen.getByTestId('model-available').textContent).toBe('false');
      expect(screen.getByTestId('count').textContent).toBe('2');

      // Regenerate the assistant turn: blocked. Crucially, the transcript must NOT
      // be truncated (pre-fix startRunWithHistory did setMessages(history) before
      // any guard, dropping the assistant turn and then debounce-saving it away).
      fireEvent.click(screen.getByRole('button', { name: 'regenerate-a1' }));

      expect(await screen.findByText(t.chatModelUnavailable)).toBeTruthy();
      expect(chatApi.spies.startChatRun).not.toHaveBeenCalled();
      expect(screen.getByTestId('count').textContent).toBe('2');
      expect(screen.getByTestId('last').textContent).toBe('answer');

      // The truncation must never have been persisted either.
      await new Promise((resolve) => setTimeout(resolve, 1100));
      expect(chatApi.spies.saveChat).not.toHaveBeenCalled();
      expect(screen.getByTestId('count').textContent).toBe('2');

      // Once an available model is selected, regenerate works again.
      fireEvent.click(screen.getByRole('button', { name: 'set-model-alt' }));
      expect(screen.getByTestId('model-available').textContent).toBe('true');
      fireEvent.click(screen.getByRole('button', { name: 'regenerate-a1' }));
      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalled());
    });

    it('editUserMessage is blocked (no run, no transcript change, toast) while the model is unavailable, and works once available', async () => {
      chatApi = makeChatApi([
        {
          id: 'c1',
          title: 'C1',
          created_at: T,
          updated_at: T,
          content: {
            settings: { model: 'unavailable-model' },
            messages: [
              { id: 'u1', role: 'user', content: 'question' },
              { id: 'a1', role: 'assistant', content: 'answer', status: 'complete' },
            ],
          },
        },
      ]);
      renderProvider();
      await waitForReady();
      expect(screen.getByTestId('model-available').textContent).toBe('false');
      expect(screen.getByTestId('count').textContent).toBe('2');

      fireEvent.click(screen.getByRole('button', { name: 'edit-u1' }));

      expect(await screen.findByText(t.chatModelUnavailable)).toBeTruthy();
      expect(chatApi.spies.startChatRun).not.toHaveBeenCalled();
      // Transcript unchanged (an edit would truncate to just the edited user turn).
      expect(screen.getByTestId('count').textContent).toBe('2');
      expect(screen.getByTestId('last').textContent).toBe('answer');

      await new Promise((resolve) => setTimeout(resolve, 1100));
      expect(chatApi.spies.saveChat).not.toHaveBeenCalled();

      // Once available, editing starts a run on the edited history.
      fireEvent.click(screen.getByRole('button', { name: 'set-model-alt' }));
      expect(screen.getByTestId('model-available').textContent).toBe('true');
      fireEvent.click(screen.getByRole('button', { name: 'edit-u1' }));
      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalled());
    });
  });

  describe(`ChatStoreProvider startRunWithHistory vision guard uses the REPLAYED history, not the full transcript [${locale}]`, () => {
    // The default fixture model ("gpt-oss-20b") carries no `vision` field, so
    // modelVisionCapable derives to false — exercising the non-capable guard
    // without needing a dedicated non-vision model fixture.
    const imageContent = [
      { type: 'text' as const, text: 'look at this' },
      { type: 'image_url' as const, image_url: { url: 'data:image/jpeg;base64,xx' } },
    ];

    it('regenerating an EARLIER turn succeeds even though a LATER (truncated-away) turn carries an image', async () => {
      chatApi = makeChatApi([
        {
          id: 'c1',
          title: 'C1',
          created_at: T,
          updated_at: T,
          content: {
            settings: { model: models[0].id },
            messages: [
              { id: 'u1', role: 'user', content: 'first question' },
              { id: 'a1', role: 'assistant', content: 'first answer', status: 'complete' },
              { id: 'u2', role: 'user', content: imageContent },
              { id: 'a2', role: 'assistant', content: 'second answer', status: 'complete' },
            ],
          },
        },
      ]);
      renderProvider();
      await waitForReady();
      expect(screen.getByTestId('model-available').textContent).toBe('true');

      // Regenerating a1: the replayed history is only [u1] (everything from u2
      // onward, incl. the image, is dropped by truncation) — must NOT be blocked.
      fireEvent.click(screen.getByRole('button', { name: 'regenerate-a1' }));

      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalled());
      expect(screen.queryByText(t.chatImageModelUnsupported)).not.toBeInTheDocument();
      const body = chatApi.spies.startChatRun.mock.calls[0][1] as { edited_history: unknown[] };
      expect(body.edited_history).toHaveLength(1);
    });

    it('regenerating a turn whose replayed history DOES contain an image is blocked with a toast', async () => {
      chatApi = makeChatApi([
        {
          id: 'c1',
          title: 'C1',
          created_at: T,
          updated_at: T,
          content: {
            settings: { model: models[0].id },
            messages: [
              { id: 'u1', role: 'user', content: 'first question' },
              { id: 'a1', role: 'assistant', content: 'first answer', status: 'complete' },
              { id: 'u2', role: 'user', content: imageContent },
              { id: 'a2', role: 'assistant', content: 'second answer', status: 'complete' },
            ],
          },
        },
      ]);
      renderProvider();
      await waitForReady();
      expect(screen.getByTestId('model-available').textContent).toBe('true');

      // Regenerating a2: the replayed history is [u1, a1, u2] — u2 carries the
      // image, so this must be blocked (no run started, transcript untouched).
      fireEvent.click(screen.getByRole('button', { name: 'regenerate-a2' }));

      expect(await screen.findByText(t.chatImageModelUnsupported)).toBeTruthy();
      expect(chatApi.spies.startChatRun).not.toHaveBeenCalled();
      expect(screen.getByTestId('count').textContent).toBe('4');
    });

    // An image thread's request body carries no history at all, so the vision
    // guard has nothing to protect and must not fire. Before this change it
    // did, and it told the user that a model whose only purpose is images
    // "does not support images" -- from the SECOND image turn onward, because
    // that is when the replayed history first contains one.
    it('regenerates in an image thread whose history contains generated images', async () => {
      chatApi = makeChatApi([
        {
          id: 'c1',
          title: 'C1',
          created_at: T,
          updated_at: T,
          content: {
            settings: { model: models[0].id, kind: 'image' },
            messages: [
              { id: 'u1', role: 'user', content: 'a cat' },
              { id: 'a1', role: 'assistant', content: imageContent, status: 'complete' },
              { id: 'u2', role: 'user', content: 'a dog' },
              { id: 'a2', role: 'assistant', content: imageContent, status: 'complete' },
            ],
          },
        },
      ]);
      renderProvider();
      await waitForReady();

      fireEvent.click(screen.getByRole('button', { name: 'regenerate-a2' }));

      // The run starts and no refusal toast appears.
      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalled());
      expect(screen.queryByText(t.chatImageModelUnsupported)).toBeNull();
    });
  });

  // ADR-043 R1. ONE predicate of the THREAD's kind and the picked model decides
  // what an attached image means -- an image the model reads (vision), a source
  // the model edits (edit), or nothing -- and the attach control, the clear on a
  // model switch and send() all read it. The kind decides first: a thread's
  // attachments follow the thread, not whichever model is picked after it.
  describe(`ChatStoreProvider attachMode [${locale}]`, () => {
    const textVision: ModelOption = {
      id: 'text-vision',
      display_name: 'text-vision',
      flavors: ['openai'],
      loading_on_count: 0,
      vision: true,
    };
    const textOnly: ModelOption = {
      ...textVision,
      id: 'text-only',
      display_name: 'text-only',
      vision: false,
    };
    const sdEdit: ModelOption = {
      id: 'sd-edit',
      display_name: 'sd-edit',
      flavors: ['openai'],
      loading_on_count: 0,
      image: true,
      image_edit: true,
    };
    const sdNoEdit: ModelOption = {
      ...sdEdit,
      id: 'sd-noedit',
      display_name: 'sd-noedit',
      image_edit: false,
    };
    const userTurn = { id: 'u1', role: 'user', content: 'hello' };
    const asstTurn = { id: 'a1', role: 'assistant', content: 'hi', status: 'complete' };

    it.each([
      // An unsent thread's kind follows the picked model (the next send pins it).
      ['an empty thread, a vision text model', textVision, undefined, 'vision'],
      ['an empty thread, a text model without vision', textOnly, undefined, 'none'],
      ['an empty thread, an edit-capable image model', sdEdit, undefined, 'edit'],
      [
        'an empty thread, an edit-capable image model that is also vision',
        { ...sdEdit, vision: true },
        undefined,
        'edit',
      ],
      ['an empty thread, an image model without image_edit', sdNoEdit, undefined, 'none'],
      [
        'an empty thread, an image model that is vision but cannot edit',
        { ...sdNoEdit, vision: true },
        undefined,
        'none',
      ],
      // A pinned thread keeps its kind whatever model is picked.
      ['an image thread, a vision text model', textVision, 'image', 'none'],
      ['an image thread, an edit-capable image model', sdEdit, 'image', 'edit'],
      ['an image thread, an image model without image_edit', sdNoEdit, 'image', 'none'],
      ['a text thread, an edit-capable image model', sdEdit, '', 'none'],
      [
        'a text thread, an image model that is vision and edit-capable',
        { ...sdEdit, vision: true },
        '',
        'none',
      ],
      ['a text thread, a vision text model', textVision, '', 'vision'],
    ] as const)('is derived for %s', async (_name, model, pinnedKind, want) => {
      const seeded = pinnedKind !== undefined;
      chatApi = makeChatApi(
        seeded
          ? [
              {
                id: 'c1',
                title: 'C1',
                created_at: T,
                updated_at: T,
                content: {
                  settings: { model: model.id, ...(pinnedKind ? { kind: pinnedKind } : {}) },
                  messages: [userTurn, asstTurn],
                },
              },
            ]
          : [],
      );
      renderProvider([], { models: [model], altModelId: model.id });
      await waitForReady();
      if (!seeded) fireEvent.click(screen.getByRole('button', { name: 'set-model-alt' }));

      expect(screen.getByTestId('attach-mode').textContent).toBe(want);
      expect(screen.getByTestId('model-image-edit').textContent).toBe(
        String(model.image_edit ?? false),
      );
    });
  });

  // The replay guard (startRunWithHistory) used to exempt an image thread
  // outright, on the premise that an image run's request carries no history.
  // That is false for an EDIT turn: its last user message carries the source.
  // The exemption is now exactly that: older images (earlier sources, generated
  // results) are never sent, so they never block; the replayed history's LAST
  // user message is the turn's source, and an edit needs an edit-capable model.
  describe(`ChatStoreProvider startRunWithHistory guard for an image thread's edit turn [${locale}]`, () => {
    const editModel: ModelOption = {
      id: 'sd-edit',
      display_name: 'sd-edit',
      flavors: ['openai'],
      loading_on_count: 0,
      image: true,
      image_edit: true,
    };
    const noEditModel: ModelOption = {
      ...editModel,
      id: 'sd-noedit',
      display_name: 'sd-noedit',
      image_edit: false,
    };
    const textModel: ModelOption = {
      id: 'text-model',
      display_name: 'text-model',
      flavors: ['openai'],
      loading_on_count: 0,
    };
    const sourceRef = '/api/portal/chat-blobs/blob_source';
    const resultRef = '/api/portal/chat-blobs/blob_result';
    const editTurn = (id: string) => ({
      id,
      role: 'user',
      content: [
        { type: 'text', text: 'make it blue' },
        { type: 'image_url', image_url: { url: sourceRef } },
      ],
    });
    const resultTurn = (id: string) => ({
      id,
      role: 'assistant',
      content: [{ type: 'image_url', image_url: { url: resultRef } }],
      status: 'complete',
    });
    const seed = (model: ModelOption, messages: unknown[]): ChatRow[] => [
      {
        id: 'c1',
        title: 'C1',
        created_at: T,
        updated_at: T,
        content: { settings: { model: model.id, kind: 'image' }, messages },
      },
    ];

    it('regenerates an edit turn with an edit-capable model, replaying the source part untouched', async () => {
      chatApi = makeChatApi(seed(editModel, [editTurn('u1'), resultTurn('a1')]));
      renderProvider([], { models: [editModel] });
      await waitForReady();

      fireEvent.click(screen.getByRole('button', { name: 'regenerate-a1' }));

      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalled());
      const body = chatApi.spies.startChatRun.mock.calls[0][1] as {
        edited_history: { id: string; content: unknown }[];
        settings: { kind: string };
      };
      expect(body.settings.kind).toBe('image');
      expect(body.edited_history).toHaveLength(1);
      // The source goes back as the same stored reference: no copy, no re-embed.
      expect(body.edited_history[0].content).toEqual(editTurn('u1').content);
    });

    it('refuses to regenerate an edit turn once the model cannot edit, before touching the transcript', async () => {
      chatApi = makeChatApi(seed(noEditModel, [editTurn('u1'), resultTurn('a1')]));
      renderProvider([], { models: [noEditModel] });
      await waitForReady();
      expect(screen.getByTestId('count').textContent).toBe('2');

      fireEvent.click(screen.getByRole('button', { name: 'regenerate-a1' }));

      // The generator's own sentence: it generates images and takes none as input.
      expect(await screen.findByText(t.chatImageGeneratorNoInput)).toBeTruthy();
      expect(chatApi.spies.startChatRun).not.toHaveBeenCalled();
      // The truncation (which a debounced save would then PUT) did not happen.
      expect(screen.getByTestId('count').textContent).toBe('2');
    });

    it('refuses an edit turn on a model that is not an image model at all, with the input sentence', async () => {
      chatApi = makeChatApi(seed(textModel, [editTurn('u1'), resultTurn('a1')]));
      renderProvider([], { models: [textModel] });
      await waitForReady();

      fireEvent.click(screen.getByRole('button', { name: 'regenerate-a1' }));

      expect(await screen.findByText(t.chatImageModelUnsupported)).toBeTruthy();
      expect(chatApi.spies.startChatRun).not.toHaveBeenCalled();
    });

    it('still lets a generation turn regenerate on a model that cannot edit, older sources and results included', async () => {
      // The exemption that remains: nothing OLDER than the last user message is
      // ever sent, so an earlier edit's source and every generated result are
      // history the guard has nothing to protect.
      chatApi = makeChatApi(
        seed(noEditModel, [
          editTurn('u1'),
          resultTurn('a1'),
          { id: 'u2', role: 'user', content: 'a dog' },
          resultTurn('a2'),
        ]),
      );
      renderProvider([], { models: [noEditModel] });
      await waitForReady();

      fireEvent.click(screen.getByRole('button', { name: 'regenerate-a2' }));

      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalled());
      expect(screen.queryByText(t.chatImageGeneratorNoInput)).toBeNull();
      expect(screen.queryByText(t.chatImageModelUnsupported)).toBeNull();
    });

    it('checks a regenerate against the replayed history, not the whole transcript', async () => {
      chatApi = makeChatApi(
        seed(noEditModel, [
          { id: 'u1', role: 'user', content: 'a cat' },
          resultTurn('a1'),
          editTurn('u2'),
          resultTurn('a2'),
        ]),
      );
      renderProvider([], { models: [noEditModel] });
      await waitForReady();

      // Truncation drops the later turn's source: this replay is a plain generation.
      fireEvent.click(screen.getByRole('button', { name: 'regenerate-a1' }));
      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalled());
    });

    it('editing an edit turn keeps its source image and replaces only the prompt text', async () => {
      chatApi = makeChatApi(seed(editModel, [editTurn('u1'), resultTurn('a1')]));
      renderProvider([], { models: [editModel] });
      await waitForReady();

      fireEvent.click(screen.getByRole('button', { name: 'edit-u1' }));

      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalled());
      const body = chatApi.spies.startChatRun.mock.calls[0][1] as {
        edited_history: { id: string; content: unknown }[];
      };
      expect(body.edited_history).toHaveLength(1);
      expect(body.edited_history[0].id).toBe('u1');
      expect(body.edited_history[0].content).toEqual([
        { type: 'text', text: 'edited text' },
        { type: 'image_url', image_url: { url: sourceRef } },
      ]);
    });

    it('refuses an edit of an edit turn on a model that cannot edit, leaving the transcript alone', async () => {
      chatApi = makeChatApi(seed(noEditModel, [editTurn('u1'), resultTurn('a1')]));
      renderProvider([], { models: [noEditModel] });
      await waitForReady();

      fireEvent.click(screen.getByRole('button', { name: 'edit-u1' }));

      expect(await screen.findByText(t.chatImageGeneratorNoInput)).toBeTruthy();
      expect(chatApi.spies.startChatRun).not.toHaveBeenCalled();
      expect(screen.getByTestId('count').textContent).toBe('2');
    });

    // A masked edit (inpainting): the mask is a tagged image part of the same user
    // message and replays exactly as the source does -- an edit of the prompt keeps
    // it, a regenerate sends it untouched, the guard treats the pair as the edit it
    // is -- while a message holding a mask and NO source is no edit at all.
    describe('a masked edit', () => {
      const maskRef = '/api/portal/chat-blobs/blob_mask';
      const maskedTurn = (id: string) => ({
        id,
        role: 'user',
        content: [
          { type: 'text', text: 'fill the sky' },
          { type: 'image_url', image_url: { url: sourceRef } },
          { type: 'image_url', image_url: { url: maskRef }, purpose: 'mask' },
        ],
      });

      it('regenerates with the mask part replayed untouched, tag and reference included', async () => {
        chatApi = makeChatApi(seed(editModel, [maskedTurn('u1'), resultTurn('a1')]));
        renderProvider([], { models: [editModel] });
        await waitForReady();

        fireEvent.click(screen.getByRole('button', { name: 'regenerate-a1' }));

        await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalled());
        const body = chatApi.spies.startChatRun.mock.calls[0][1] as {
          edited_history: { id: string; content: unknown }[];
        };
        expect(body.edited_history).toHaveLength(1);
        expect(body.edited_history[0].content).toStrictEqual(maskedTurn('u1').content);
      });

      it('keeps the source and the mask when only the prompt text of the turn is edited', async () => {
        chatApi = makeChatApi(seed(editModel, [maskedTurn('u1'), resultTurn('a1')]));
        renderProvider([], { models: [editModel] });
        await waitForReady();

        fireEvent.click(screen.getByRole('button', { name: 'edit-u1' }));

        await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalled());
        const body = chatApi.spies.startChatRun.mock.calls[0][1] as {
          edited_history: { id: string; content: unknown }[];
        };
        expect(body.edited_history[0].content).toStrictEqual([
          { type: 'text', text: 'edited text' },
          { type: 'image_url', image_url: { url: sourceRef } },
          { type: 'image_url', image_url: { url: maskRef }, purpose: 'mask' },
        ]);
      });

      it('is refused as the edit it is on a model that cannot edit, before the transcript is touched', async () => {
        chatApi = makeChatApi(seed(noEditModel, [maskedTurn('u1'), resultTurn('a1')]));
        renderProvider([], { models: [noEditModel] });
        await waitForReady();

        fireEvent.click(screen.getByRole('button', { name: 'regenerate-a1' }));

        expect(await screen.findByText(t.chatImageGeneratorNoInput)).toBeTruthy();
        expect(chatApi.spies.startChatRun).not.toHaveBeenCalled();
        expect(screen.getByTestId('count').textContent).toBe('2');
      });

      it('is no edit without its source: a message holding only a mask replays as a plain generation', async () => {
        const maskOnly = {
          id: 'u1',
          role: 'user',
          content: [
            { type: 'text', text: 'a cat' },
            { type: 'image_url', image_url: { url: maskRef }, purpose: 'mask' },
          ],
        };
        chatApi = makeChatApi(seed(noEditModel, [maskOnly, resultTurn('a1')]));
        renderProvider([], { models: [noEditModel] });
        await waitForReady();

        fireEvent.click(screen.getByRole('button', { name: 'regenerate-a1' }));

        // Not refused as an edit (a generator that cannot be edited may still
        // generate): the executor ignores a mask that has no source.
        await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalled());
        expect(screen.queryByText(t.chatImageGeneratorNoInput)).toBeNull();
      });
    });

    // The prompt rule of the composer (an edit needs one) holds on the replay
    // paths too: an edit that blanks the prompt of an edit turn, or a regenerate of
    // an edit turn that was stored without one (the old composer allowed that),
    // would be truncated, started and then answered images.prompt_required.
    it('refuses to blank the prompt of an edit turn, before touching the transcript', async () => {
      chatApi = makeChatApi(seed(editModel, [editTurn('u1'), resultTurn('a1')]));
      renderProvider([], { models: [editModel] });
      await waitForReady();

      fireEvent.click(screen.getByRole('button', { name: 'edit-blank-u1' }));

      expect(await screen.findByText(t.chatImageEditPromptRequired)).toBeTruthy();
      expect(chatApi.spies.startChatRun).not.toHaveBeenCalled();
      expect(screen.getByTestId('count').textContent).toBe('2');
    });

    it('refuses to regenerate an edit turn that was stored without a prompt', async () => {
      const promptless = {
        id: 'u1',
        role: 'user',
        content: [
          { type: 'text', text: '' },
          { type: 'image_url', image_url: { url: sourceRef } },
        ],
      };
      chatApi = makeChatApi(seed(editModel, [promptless, resultTurn('a1')]));
      renderProvider([], { models: [editModel] });
      await waitForReady();

      fireEvent.click(screen.getByRole('button', { name: 'regenerate-a1' }));

      expect(await screen.findByText(t.chatImageEditPromptRequired)).toBeTruthy();
      expect(chatApi.spies.startChatRun).not.toHaveBeenCalled();
      expect(screen.getByTestId('count').textContent).toBe('2');
    });
  });

  // Iterative refine (ADR-043 R1): a generated image of an image thread can be the
  // source of the next edit. The action seeds the composer with the image's EXISTING
  // /chat-blobs/{id} reference -- no copy, no re-embed, so the transcript never grows
  // by the image's bytes -- and the user types the instruction and sends; the turn is
  // an ordinary edit turn from there (the composer gate, the prompt rule, the replay
  // handling all apply as they do to an uploaded source).
  describe(`ChatStoreProvider refine a generated image in place [${locale}]`, () => {
    const editModel: ModelOption = {
      id: 'sd-edit',
      display_name: 'sd-edit',
      flavors: ['openai'],
      loading_on_count: 0,
      image: true,
      image_edit: true,
    };
    const noEditModel: ModelOption = {
      ...editModel,
      id: 'sd-noedit',
      display_name: 'sd-noedit',
      image_edit: false,
    };
    const textVisionModel: ModelOption = {
      id: 'text-vision',
      display_name: 'text-vision',
      flavors: ['openai'],
      loading_on_count: 0,
      vision: true,
    };
    const resultRef = '/api/portal/chat-blobs/blob_result';
    const resultTwoRef = '/api/portal/chat-blobs/blob_result_two';
    const uploadedRef = '/api/portal/chat-blobs/blob_upload';
    const generated = (id: string, url: string) => ({
      id,
      role: 'assistant',
      content: [{ type: 'image_url', image_url: { url } }],
      status: 'complete',
    });
    // A refine turn as the composer sends it and the server stores it: the prompt, and
    // the source -- the SAME reference the assistant turn before it holds.
    const refineTurn = (id: string, url: string, text = 'make the sky purple') => ({
      id,
      role: 'user',
      content: [
        { type: 'text', text },
        { type: 'image_url', image_url: { url } },
      ],
    });
    const thread = (model: ModelOption, kind: string, msgs: unknown[]): ChatRow[] => [
      {
        id: 'c1',
        title: 'C1',
        created_at: T,
        updated_at: T,
        content: { settings: { model: model.id, ...(kind ? { kind } : {}) }, messages: msgs },
      },
    ];
    const generation = () => [
      { id: 'u1', role: 'user', content: 'a lighthouse' },
      generated('a1', resultRef),
    ];
    const startBody = () =>
      chatApi.spies.startChatRun.mock.calls[0][1] as {
        user_message?: unknown;
        edited_history?: { id: string; content: unknown }[];
        settings: { kind: string };
      };

    describe('the action', () => {
      it("seeds the composer with the image's own stored reference and starts no run", async () => {
        chatApi = makeChatApi(thread(editModel, 'image', generation()));
        renderProvider([], { models: [editModel] });
        await waitForReady();
        expect(screen.getByTestId('composer-images').textContent).toBe('');

        fireEvent.click(screen.getByRole('button', { name: 'refine-a1' }));

        // The reference itself, string for string -- not fetched, not re-encoded.
        expect(screen.getByTestId('composer-images').textContent).toBe(resultRef);
        // Nothing was sent or stored yet: the user still has to say what to change.
        expect(chatApi.spies.startChatRun).not.toHaveBeenCalled();
        expect(screen.getByTestId('count').textContent).toBe('2');
        // No mask is attached for the user: that is theirs to add.
        expect(screen.getByTestId('composer-mask').textContent).toBe('');
      });

      it('sends the refine turn as an edit whose source is that same reference, embedding nothing', async () => {
        chatApi = makeChatApi(thread(editModel, 'image', generation()));
        renderProvider([], { models: [editModel] });
        await waitForReady();

        fireEvent.click(screen.getByRole('button', { name: 'refine-a1' }));
        fireEvent.change(screen.getByLabelText('probe-input'), {
          target: { value: 'make the sky purple' },
        });
        fireEvent.click(screen.getByRole('button', { name: 'send' }));

        await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
        const body = startBody();
        expect(body.settings.kind).toBe('image');
        // The source part is the assistant image's own url: the reference is reused.
        expect(body.user_message).toStrictEqual([
          { type: 'text', text: 'make the sky purple' },
          { type: 'image_url', image_url: { url: resultRef } },
        ]);
        // ...and no image bytes ride the request: the transcript does not grow by the
        // image (the point of storing images out of it).
        expect(JSON.stringify(body)).not.toContain('data:');
        // The optimistic turn shows the same reference, after the generation turn.
        expect(screen.getByTestId('count').textContent).toBe('3');
        expect(screen.getByTestId('last-parts').textContent).toBe(
          `text:make the sky purple|image_url:${resultRef}`,
        );
        // The strip is spent, like any send's.
        expect(screen.getByTestId('composer-images').textContent).toBe('');
      });

      it("seeds an old chat's inline result as it is: it has no reference to reuse, so it goes out as any upload does", async () => {
        // Before images were stored out of the transcript a result was an inline data
        // url (and such a thread migrates on its next run). It is the only handle that
        // image has, so it is taken verbatim; the commit stores it like an upload.
        const inline = 'data:image/png;base64,AAAA';
        chatApi = makeChatApi(
          thread(editModel, 'image', [
            { id: 'u1', role: 'user', content: 'a lighthouse' },
            generated('a1', inline),
          ]),
        );
        renderProvider([], { models: [editModel] });
        await waitForReady();

        fireEvent.click(screen.getByRole('button', { name: 'refine-a1' }));

        expect(screen.getByTestId('composer-images').textContent).toBe(inline);
      });

      it('keeps the prompt rule of an edit: a refine without an instruction is not sent', async () => {
        chatApi = makeChatApi(thread(editModel, 'image', generation()));
        renderProvider([], { models: [editModel] });
        await waitForReady();

        fireEvent.click(screen.getByRole('button', { name: 'refine-a1' }));
        fireEvent.click(screen.getByRole('button', { name: 'send' }));

        expect(await screen.findByText(t.chatImageEditPromptRequired)).toBeTruthy();
        expect(chatApi.spies.startChatRun).not.toHaveBeenCalled();
        // The image stays attached: the user types the instruction and sends again.
        expect(screen.getByTestId('composer-images').textContent).toBe(resultRef);
        expect(screen.getByTestId('count').textContent).toBe('2');
      });

      it('replaces a source seeded earlier: an edit has one source, so a second refine swaps it', async () => {
        chatApi = makeChatApi(
          thread(editModel, 'image', [
            ...generation(),
            refineTurn('u2', resultRef),
            generated('a2', resultTwoRef),
          ]),
        );
        renderProvider([], { models: [editModel] });
        await waitForReady();

        fireEvent.click(screen.getByRole('button', { name: 'refine-a1' }));
        expect(screen.getByTestId('composer-images').textContent).toBe(resultRef);
        fireEvent.click(screen.getByRole('button', { name: 'refine-a2' }));

        // The newer result replaced the older one: one source, never two.
        expect(screen.getByTestId('composer-images').textContent).toBe(resultTwoRef);
        fireEvent.click(screen.getByRole('button', { name: 'refine-a2' }));
        expect(screen.getByTestId('composer-images').textContent).toBe(resultTwoRef);
      });
    });

    describe('where it is allowed', () => {
      it('refuses on a thread whose model cannot be edited, seeding nothing', async () => {
        chatApi = makeChatApi(thread(noEditModel, 'image', generation()));
        renderProvider([], { models: [noEditModel] });
        await waitForReady();

        fireEvent.click(screen.getByRole('button', { name: 'refine-a1' }));

        expect(await screen.findByText(t.chatImageGeneratorNoInput)).toBeTruthy();
        expect(screen.getByTestId('composer-images').textContent).toBe('');
        expect(chatApi.spies.startChatRun).not.toHaveBeenCalled();
      });

      it('refuses when the model of the thread is not an image model at all', async () => {
        chatApi = makeChatApi(thread(textVisionModel, 'image', generation()));
        renderProvider([], { models: [textVisionModel] });
        await waitForReady();

        fireEvent.click(screen.getByRole('button', { name: 'refine-a1' }));

        expect(await screen.findByText(t.chatImageModelUnsupported)).toBeTruthy();
        expect(screen.getByTestId('composer-images').textContent).toBe('');
      });

      it('never seeds a text thread, even with an edit-capable model picked', async () => {
        // A text thread stays a text thread whichever model is picked afterwards
        // (attachMode follows the thread's kind): there is nothing to refine in one.
        chatApi = makeChatApi(thread(editModel, '', generation()));
        renderProvider([], { models: [editModel] });
        await waitForReady();
        expect(screen.getByTestId('attach-mode').textContent).toBe('none');

        fireEvent.click(screen.getByRole('button', { name: 'refine-a1' }));

        expect(screen.getByTestId('composer-images').textContent).toBe('');
      });

      it('is for a generated result only: an image of a user turn and a url the message does not hold seed nothing', async () => {
        chatApi = makeChatApi(
          thread(editModel, 'image', [
            { id: 'u0', role: 'user', content: 'a cat' },
            generated('a0', resultRef),
            refineTurn('u1', uploadedRef),
            generated('a1', resultTwoRef),
          ]),
        );
        renderProvider([], { models: [editModel] });
        await waitForReady();

        // The user's own image (the source of an earlier edit): out of scope.
        fireEvent.click(screen.getByRole('button', { name: 'refine-u1' }));
        expect(screen.getByTestId('composer-images').textContent).toBe('');
        // A url that is not one of THAT message's images (a stale or foreign one).
        fireEvent.click(screen.getByRole('button', { name: 'refine-foreign-a1' }));
        expect(screen.getByTestId('composer-images').textContent).toBe('');
      });

      it('refuses while this chat has a live run, like edit and regenerate do', async () => {
        chatApi = makeChatApi(thread(editModel, 'image', generation()));
        renderProvider([], { models: [editModel] });
        await waitForReady();
        fireEvent.change(screen.getByLabelText('probe-input'), { target: { value: 'a dog' } });
        fireEvent.click(screen.getByRole('button', { name: 'send' }));
        await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));

        fireEvent.click(screen.getByRole('button', { name: 'refine-a1' }));

        expect(await screen.findByText(t.chatBusy)).toBeTruthy();
        expect(screen.getByTestId('composer-images').textContent).toBe('');
      });

      it('does not carry the reference into another chat: it names a blob of THIS one', async () => {
        const rows = thread(editModel, 'image', generation());
        rows.push({
          id: 'c2',
          title: 'C2',
          created_at: T1,
          updated_at: T1,
          content: { settings: { model: editModel.id, kind: 'image' }, messages: [] },
        });
        chatApi = makeChatApi(rows);
        renderProvider([], { models: [editModel] });
        await waitForReady();
        fireEvent.click(screen.getByRole('button', { name: 'refine-a1' }));
        expect(screen.getByTestId('composer-images').textContent).toBe(resultRef);

        fireEvent.click(screen.getByRole('button', { name: 'select-c2' }));
        await waitFor(() => expect(screen.getByTestId('active').textContent).toBe('c2'));

        // Sent there it would be refused (images.blob_not_owned): the chat-bound
        // reference is dropped with the chat it belongs to.
        expect(screen.getByTestId('composer-images').textContent).toBe('');
      });
    });

    // A refine turn, once sent, is an ordinary edit turn: the replay handling of the
    // edit turn (replay guard, source part kept on an edit of the prompt) covers it.
    // What is special about it is only that its source is the very reference an earlier
    // assistant turn holds -- which the replay must send back untouched, never inline.
    describe('replaying a refine turn', () => {
      const refined = () => [
        ...generation(),
        refineTurn('u2', resultRef),
        generated('a2', resultTwoRef),
      ];

      it('regenerates it with the source part sent back as the same reference', async () => {
        chatApi = makeChatApi(thread(editModel, 'image', refined()));
        renderProvider([], { models: [editModel] });
        await waitForReady();

        fireEvent.click(screen.getByRole('button', { name: 'regenerate-a2' }));

        await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
        const body = startBody();
        expect(body.settings.kind).toBe('image');
        expect(body.edited_history?.map((m) => m.id)).toEqual(['u1', 'a1', 'u2']);
        expect(body.edited_history?.[2].content).toStrictEqual(refineTurn('u2', resultRef).content);
        expect(JSON.stringify(body)).not.toContain('data:');
      });

      it('keeps the source when only the instruction of the turn is edited', async () => {
        chatApi = makeChatApi(thread(editModel, 'image', refined()));
        renderProvider([], { models: [editModel] });
        await waitForReady();

        fireEvent.click(screen.getByRole('button', { name: 'edit-u2' }));

        await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
        const body = startBody();
        expect(body.edited_history?.map((m) => m.id)).toEqual(['u1', 'a1', 'u2']);
        expect(body.edited_history?.[2].content).toStrictEqual([
          { type: 'text', text: 'edited text' },
          { type: 'image_url', image_url: { url: resultRef } },
        ]);
      });

      it('refuses the replay of it once the model cannot edit, before the transcript is touched', async () => {
        chatApi = makeChatApi(thread(noEditModel, 'image', refined()));
        renderProvider([], { models: [noEditModel] });
        await waitForReady();

        fireEvent.click(screen.getByRole('button', { name: 'regenerate-a2' }));

        expect(await screen.findByText(t.chatImageGeneratorNoInput)).toBeTruthy();
        expect(chatApi.spies.startChatRun).not.toHaveBeenCalled();
        expect(screen.getByTestId('count').textContent).toBe('4');
      });

      it('regenerates the generation BEFORE it as a plain generation, dropping the refine turn', async () => {
        chatApi = makeChatApi(thread(noEditModel, 'image', refined()));
        renderProvider([], { models: [noEditModel] });
        await waitForReady();

        fireEvent.click(screen.getByRole('button', { name: 'regenerate-a1' }));

        await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
        expect(startBody().edited_history?.map((m) => m.id)).toEqual(['u1']);
        expect(screen.queryByText(t.chatImageGeneratorNoInput)).toBeNull();
      });
    });
  });

  describe(`ChatStoreProvider image-thread kind [${locale}]`, () => {
    // An image GENERATOR that does not also read images. `vision` is
    // deliberately absent so modelVisionCapable derives false -- that is the
    // combination these tests need (it is what makes the regenerate guard's
    // history check reachable), NOT a claim that the two go together:
    // generating and accepting images are orthogonal capabilities the backend
    // aggregates separately, and a model can carry both. Chat.test.tsx covers
    // that case explicitly, because it is the one where the attach gate's
    // image clause is load-bearing.
    const imageModels: ModelOption[] = [
      {
        id: 'sd-turbo',
        display_name: 'sd-turbo',
        flavors: ['openai'],
        loading_on_count: 0,
        image: true,
      },
    ];
    const generatedImage = [
      { type: 'image_url' as const, image_url: { url: 'data:image/png;base64,AAAA' } },
    ];
    // A generated image far larger than the deliberately shrunken cap the
    // no-refusal test serves: were the store still estimating image capacity
    // from the document size, this thread would provably have no room left.
    const bigImage = [
      {
        type: 'image_url' as const,
        image_url: { url: `data:image/png;base64,${'A'.repeat(200_000)}` },
      },
    ];
    const imageThread = (content: unknown[]): ChatRow[] => [
      {
        id: 'c1',
        title: 'C1',
        created_at: T,
        updated_at: T,
        content: {
          settings: { model: imageModels[0].id, kind: 'image' },
          messages: [
            { id: 'u1', role: 'user', content: 'a cat' },
            { id: 'a1', role: 'assistant', content, status: 'complete' },
          ],
        },
      },
    ];

    // INHERITED DEFECT 1 (traced in Task 12's review): buildDoc() rebuilds
    // `settings` from React state and carried no `kind`, and SaveChat
    // FULL-REPLACES the opaque content blob with no merge -- so the first
    // autosave after the backend pinned the thread silently erased the pin,
    // and the next send's PrepareChatRun read an empty stored kind and re-pinned
    // the thread to text, permanently. The server-side pin is defeated in
    // practice until the kind round-trips through save AND reload.
    it('persists the pinned image kind across a save and a reload', async () => {
      chatApi = makeChatApi(imageThread(generatedImage));
      const { unmount } = renderProvider([], { models: imageModels });
      await waitForReady();
      expect(screen.getByTestId('chat-kind').textContent).toBe('image');

      // Any settings edit schedules the debounced save; the document it PUTs
      // must still carry the pin.
      fireEvent.click(screen.getByRole('button', { name: 'set-system' }));
      await waitFor(() => expect(chatApi.spies.saveChat).toHaveBeenCalled(), { timeout: 3000 });
      const saved = chatApi.spies.saveChat.mock.calls.at(-1)![1] as {
        content: { settings: { kind?: string } };
      };
      expect(saved.content.settings.kind).toBe('image');

      // ...and a reload reads it back out of the saved document. Without this
      // half the pin could round-trip through the PUT and still be lost on the
      // next load (normalizeDoc drops what it does not know about).
      unmount();
      renderProvider([], { models: imageModels });
      await waitForReady();
      expect(screen.getByTestId('chat-kind').textContent).toBe('image');
    });

    // The mirror image of the round trip above, and of the backend's own
    // TestPortalChat...TextStaysByteIdentical: `kind` is omitempty on BOTH
    // sides. A text thread's stored settings must not gain the key just
    // because the portal now knows about kinds -- every existing text chat in
    // the system would otherwise have its blob reshaped on its next save, for
    // a field that means nothing to it.
    it('writes no kind key at all when saving a text thread', async () => {
      chatApi = makeChatApi([
        {
          id: 'c1',
          title: 'C1',
          created_at: T,
          updated_at: T,
          content: {
            settings: { model: models[0].id },
            messages: [
              { id: 'u1', role: 'user', content: 'hello' },
              { id: 'a1', role: 'assistant', content: 'hi', status: 'complete' },
            ],
          },
        },
      ]);
      renderProvider();
      await waitForReady();
      expect(screen.getByTestId('chat-kind').textContent).toBe('');

      fireEvent.click(screen.getByRole('button', { name: 'set-system' }));
      await waitFor(() => expect(chatApi.spies.saveChat).toHaveBeenCalled(), { timeout: 3000 });
      const saved = chatApi.spies.saveChat.mock.calls.at(-1)![1] as {
        content: { settings: Record<string, unknown> };
      };
      expect(Object.keys(saved.content.settings)).not.toContain('kind');
    });

    // The THIRD of the three lockstep edits a new persisted setting needs in
    // useChatPersistence: the debounced effect's EXPLICIT dep array, which
    // sits under an eslint-disable for exhaustive-deps, so a missing dep is
    // silent -- the setting simply never schedules a save. Every other route
    // to a kind change also changes `model` or `messages` and would therefore
    // pass without the dep; this isolates it, using the one real-world trigger
    // that does not: the models catalogue is reloaded (onRefresh does that
    // after every run) and now reports the SAME picked model as image-capable.
    it('schedules a save when the thread kind is the only thing that changed', async () => {
      const flipModels = (image: boolean): ModelOption[] => [
        {
          id: 'flip-model',
          display_name: 'flip-model',
          flavors: ['openai'],
          loading_on_count: 0,
          image,
        },
      ];
      const { rerenderWithModels } = renderProvider([], { models: flipModels(false) });
      await waitForReady();
      fireEvent.click(screen.getByRole('button', { name: 'set-model-alt' }));
      await waitFor(() => expect(chatApi.spies.saveChat).toHaveBeenCalled(), { timeout: 3000 });
      expect(screen.getByTestId('chat-kind').textContent).toBe('');
      chatApi.spies.saveChat.mockClear();

      rerenderWithModels(flipModels(true));
      expect(screen.getByTestId('chat-kind').textContent).toBe('image');

      await waitFor(() => expect(chatApi.spies.saveChat).toHaveBeenCalled(), { timeout: 3000 });
      const saved = chatApi.spies.saveChat.mock.calls.at(-1)![1] as {
        content: { settings: { kind?: string } };
      };
      expect(saved.content.settings.kind).toBe('image');
    });

    // INHERITED DEFECT 2 (traced in Task 12's review): the kind was read once,
    // in activateChat, so in a LIVE session -- new chat, pick an image model,
    // send, send again, regenerate, no reload -- it was still "" and Task 12's
    // kind-aware guard did nothing. Only a reloaded thread benefited.
    it('tracks the thread kind through a whole live session with no reload', async () => {
      renderProvider([], { models: imageModels });
      await waitForReady();

      // The real backend COMMITS the canonical transcript before it emits
      // `done`, and the store refetches it right after (adoptCanonicalTranscript).
      // Mirror that here, or the refetch would replace the live transcript
      // with this fake row's older copy.
      const commitTurns = (turns: unknown[]) => {
        chatApi.rows[0].content = {
          settings: { model: imageModels[0].id, kind: 'image' },
          messages: turns,
        };
      };

      // 1. Pick the image model on a brand-new chat. Until the thread is
      //    pinned, the composer's kind follows the picked model.
      fireEvent.click(screen.getByRole('button', { name: 'set-model-alt' }));
      expect(screen.getByTestId('model-image-capable').textContent).toBe('true');
      expect(screen.getByTestId('chat-kind').textContent).toBe('image');

      // 2. First send establishes the pin (server-side, and here).
      fireEvent.change(screen.getByLabelText('probe-input'), { target: { value: 'a cat' } });
      fireEvent.click(screen.getByRole('button', { name: 'send' }));
      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
      expect(
        (chatApi.spies.startChatRun.mock.calls[0][1] as { settings: { kind?: string } }).settings
          .kind,
      ).toBe('image');
      commitTurns([
        { id: 'u1', role: 'user', content: 'a cat' },
        { id: 'a1', role: 'assistant', content: generatedImage, status: 'complete' },
      ]);
      await act(async () => {
        FakeEventSource.instances.at(-1)!.emit('done', {
          content: generatedImage,
          status: 'completed',
        });
      });

      // 3. Second send on the SAME thread, still no reload: the transcript is
      //    no longer empty, so the kind must now come from the thread's pin
      //    rather than from the model -- and it must still be "image".
      expect(screen.getByTestId('chat-kind').textContent).toBe('image');
      fireEvent.change(screen.getByLabelText('probe-input'), { target: { value: 'a dog' } });
      fireEvent.click(screen.getByRole('button', { name: 'send' }));
      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(2));
      expect(
        (chatApi.spies.startChatRun.mock.calls[1][1] as { settings: { kind?: string } }).settings
          .kind,
      ).toBe('image');
      commitTurns([
        { id: 'u1', role: 'user', content: 'a cat' },
        { id: 'a1', role: 'assistant', content: generatedImage, status: 'complete' },
        { id: 'u2', role: 'user', content: 'a dog' },
        { id: 'a2', role: 'assistant', content: generatedImage, status: 'complete' },
      ]);
      await act(async () => {
        FakeEventSource.instances.at(-1)!.emit('done', {
          content: generatedImage,
          status: 'completed',
        });
      });

      // 4. Regenerate the last turn. Its REPLAYED history is [u1, a1, u2] and
      //    a1 is a generated image, so the role-blind vision guard fires
      //    unless the thread is known to be an image thread -- which, in a
      //    live session, it only is if the kind is real state.
      await waitFor(() => expect(screen.getByTestId('count').textContent).toBe('4'));
      fireEvent.click(screen.getByRole('button', { name: 'regenerate-a2' }));

      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(3));
      expect(screen.queryByText(t.chatImageModelUnsupported)).toBeNull();
    });

    // The pagehide keepalive is capped near the browser's own keepalive body
    // ceiling, and a single inline base64 image is far past it -- so for every
    // thread this feature creates, the keepalive is the skipped branch. The cap
    // stays (raising it would just move the failure to the browser dropping the
    // PUT); what must hold is that the skip is not a LOSS: the chat stays dirty
    // and the unlimited debounced save still persists the change.
    it('leaves an oversized image document to the debounced save when the keepalive skips it', async () => {
      chatApi = makeChatApi(imageThread(bigImage));
      renderProvider([], { models: imageModels });
      await waitForReady();

      // Dirty the chat, then navigate away before the debounce fires.
      fireEvent.click(screen.getByRole('button', { name: 'set-system' }));
      act(() => {
        window.dispatchEvent(new Event('pagehide'));
      });
      expect(chatApi.spies.saveChatKeepalive).not.toHaveBeenCalled();

      // The chat was left dirty, so the debounced save still lands the change.
      // (Clearing the dirty flag on the skip would silently drop it instead.)
      await waitFor(() => expect(chatApi.spies.saveChat).toHaveBeenCalled(), { timeout: 3000 });
      const saved = chatApi.spies.saveChat.mock.calls.at(-1)![1] as {
        content: { settings: { system_prompt?: string } };
      };
      expect(saved.content.settings.system_prompt).toBe('hi');
    });

    it('still fires the pagehide keepalive for a document inside the cap', async () => {
      // Negative control: the skip above is about SIZE, not about image
      // threads as such.
      chatApi = makeChatApi(imageThread(generatedImage));
      renderProvider([], { models: imageModels });
      await waitForReady();

      fireEvent.click(screen.getByRole('button', { name: 'set-system' }));
      act(() => {
        window.dispatchEvent(new Event('pagehide'));
      });

      expect(chatApi.spies.saveChatKeepalive).toHaveBeenCalled();
    });

    // The thread's kind follows the THREAD, not the picked model. A text
    // thread that already has history has been pinned to text server-side
    // (PrepareChatRun forces the stored kind on every send after the first),
    // so picking an image model in it must not turn its composer -- or the
    // kind it submits -- into an image one.
    it('keeps an existing text thread text even when an image model is picked', async () => {
      chatApi = makeChatApi([
        {
          id: 'c1',
          title: 'C1',
          created_at: T,
          updated_at: T,
          content: {
            settings: { model: imageModels[0].id },
            messages: [
              { id: 'u1', role: 'user', content: 'hello' },
              { id: 'a1', role: 'assistant', content: 'hi', status: 'complete' },
            ],
          },
        },
      ]);
      renderProvider([], { models: imageModels });
      await waitForReady();

      expect(screen.getByTestId('model-image-capable').textContent).toBe('true');
      expect(screen.getByTestId('chat-kind').textContent).toBe('');

      fireEvent.change(screen.getByLabelText('probe-input'), { target: { value: 'more' } });
      fireEvent.click(screen.getByRole('button', { name: 'send' }));

      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalled());
      expect(
        (chatApi.spies.startChatRun.mock.calls[0][1] as { settings: { kind?: string } }).settings
          .kind,
      ).toBe('');
    });

    it('does not refuse an image send on the size of the transcript', async () => {
      // Generated images are stored out of the transcript (as
      // /api/portal/chat-blobs/{id} urls), so the document size says nothing
      // about how many more images fit and the old image-count estimate (and
      // its send() refusal) is gone. The listing still serves a cap this thread
      // already exceeds; the send must go through anyway. Text overflow stays
      // the server's 400 portal.chat_too_large.
      chatApi = makeChatApi(imageThread(bigImage), 300_000);
      renderProvider([], { models: imageModels });
      await waitForReady();
      expect(screen.getByTestId('chat-kind').textContent).toBe('image');

      fireEvent.change(screen.getByLabelText('probe-input'), { target: { value: 'a dog' } });
      fireEvent.click(screen.getByRole('button', { name: 'send' }));

      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalled());
    });

    // The backend's run-start refusals for the chat binary payload store. Each
    // 400 reaches the toast through formatPortalError, which must show the
    // localized label (after the code), never the raw English the server sent.
    it.each([
      ['images.blob_type_unsupported', 'errorImagesBlobTypeUnsupported'],
      ['images.blob_too_large', 'errorImagesBlobTooLarge'],
      ['images.blob_not_owned', 'errorImagesBlobNotOwned'],
      ['portal.chat_run_history_too_large', 'errorChatRunHistoryTooLarge'],
    ] as const)(
      'localizes the %s run-start refusal instead of showing the raw server text',
      async (code, labelKey) => {
        chatApi = makeChatApi(imageThread(generatedImage));
        chatApi.spies.startChatRun.mockRejectedValue(
          new PortalApiError(400, code, 'raw english from the server'),
        );
        renderProvider([], { models: imageModels });
        await waitForReady();

        fireEvent.change(screen.getByLabelText('probe-input'), { target: { value: 'a dog' } });
        fireEvent.click(screen.getByRole('button', { name: 'send' }));

        expect(await screen.findByText(`${code}: ${t[labelKey]}`)).toBeTruthy();
        expect(screen.queryByText(/raw english from the server/)).toBeNull();
      },
    );
  });

  // The per-model sd-server options (samplers / size limits / ...) the composer's
  // dropdowns narrow to. Fetched on demand when the picked model GENERATES images
  // -- the serverOverrideModels pattern: latest-wins, a failure degrades to null
  // (so the dropdowns fall back to the static Phase 1b-i superset).
  describe(`ChatStoreProvider per-model image options [${locale}]`, () => {
    const imageOptionModels: ModelOption[] = [
      {
        id: 'sd-turbo',
        display_name: 'sd-turbo',
        flavors: ['openai'],
        loading_on_count: 0,
        image: true,
      },
      {
        id: 'sd-xl',
        display_name: 'sd-xl',
        flavors: ['openai'],
        loading_on_count: 0,
        image: true,
      },
      { id: 'gpt-oss-20b', display_name: 'gpt-oss-20b', flavors: ['openai'], loading_on_count: 0 },
    ];
    const turboOptions: ImageModelOptions = {
      samplers: ['euler_a', 'lcm'],
      max_batch_count: 4,
      size_limits: { min_width: 64, max_width: 1024, min_height: 64, max_height: 1024 },
    };
    const xlOptions: ImageModelOptions = { schedulers: ['karras'] };

    const optionsShown = () => screen.getByTestId('model-image-options').textContent;

    it('fetches nothing while the picked model is not an image generator', async () => {
      renderProvider([], { models: imageOptionModels });
      await waitForReady();
      fireEvent.click(screen.getByRole('button', { name: 'pick-gpt-oss-20b' }));

      expect(screen.getByTestId('model-image-capable').textContent).toBe('false');
      expect(chatApi.spies.modelImageOptions).not.toHaveBeenCalled();
      expect(optionsShown()).toBe('null');
    });

    it('fetches and exposes the options of a picked image model', async () => {
      chatApi.spies.modelImageOptions.mockResolvedValue(turboOptions);
      renderProvider([], { models: imageOptionModels });
      await waitForReady();
      expect(optionsShown()).toBe('null');

      fireEvent.click(screen.getByRole('button', { name: 'pick-sd-turbo' }));

      await waitFor(() => expect(optionsShown()).toBe(JSON.stringify(turboOptions)));
      expect(chatApi.spies.modelImageOptions).toHaveBeenCalledTimes(1);
      expect(chatApi.spies.modelImageOptions).toHaveBeenCalledWith('sd-turbo');
    });

    it('refetches for another image model and clears when a text model is picked', async () => {
      chatApi.spies.modelImageOptions.mockImplementation(async (name: string) =>
        name === 'sd-turbo' ? turboOptions : xlOptions,
      );
      renderProvider([], { models: imageOptionModels });
      await waitForReady();

      fireEvent.click(screen.getByRole('button', { name: 'pick-sd-turbo' }));
      await waitFor(() => expect(optionsShown()).toBe(JSON.stringify(turboOptions)));

      fireEvent.click(screen.getByRole('button', { name: 'pick-sd-xl' }));
      await waitFor(() => expect(optionsShown()).toBe(JSON.stringify(xlOptions)));
      expect(chatApi.spies.modelImageOptions).toHaveBeenLastCalledWith('sd-xl');

      fireEvent.click(screen.getByRole('button', { name: 'pick-gpt-oss-20b' }));
      await waitFor(() => expect(optionsShown()).toBe('null'));
      // Going text must clear without another fetch.
      expect(chatApi.spies.modelImageOptions).toHaveBeenCalledTimes(2);
    });

    it('leaves the options null when the fetch fails', async () => {
      chatApi.spies.modelImageOptions.mockRejectedValue(new Error('boom'));
      renderProvider([], { models: imageOptionModels });
      await waitForReady();

      fireEvent.click(screen.getByRole('button', { name: 'pick-sd-turbo' }));

      await waitFor(() => expect(chatApi.spies.modelImageOptions).toHaveBeenCalledWith('sd-turbo'));
      expect(optionsShown()).toBe('null');
    });

    it('drops options a previous model fetched when the next fetch fails', async () => {
      chatApi.spies.modelImageOptions.mockImplementation(async (name: string) => {
        if (name === 'sd-turbo') return turboOptions;
        throw new Error('boom');
      });
      renderProvider([], { models: imageOptionModels });
      await waitForReady();

      fireEvent.click(screen.getByRole('button', { name: 'pick-sd-turbo' }));
      await waitFor(() => expect(optionsShown()).toBe(JSON.stringify(turboOptions)));

      fireEvent.click(screen.getByRole('button', { name: 'pick-sd-xl' }));
      await waitFor(() => expect(optionsShown()).toBe('null'));
    });

    it("does not keep the previous model's options while the next fetch is in flight", async () => {
      let releaseXl: (v: ImageModelOptions) => void = () => {};
      chatApi.spies.modelImageOptions.mockImplementation((name: string) =>
        name === 'sd-turbo'
          ? Promise.resolve(turboOptions)
          : new Promise<ImageModelOptions>((resolve) => {
              releaseXl = resolve;
            }),
      );
      renderProvider([], { models: imageOptionModels });
      await waitForReady();

      fireEvent.click(screen.getByRole('button', { name: 'pick-sd-turbo' }));
      await waitFor(() => expect(optionsShown()).toBe(JSON.stringify(turboOptions)));

      // sd-xl's fetch is pending: sd-turbo's limits must not linger as if they
      // were sd-xl's.
      fireEvent.click(screen.getByRole('button', { name: 'pick-sd-xl' }));
      await waitFor(() => expect(chatApi.spies.modelImageOptions).toHaveBeenCalledWith('sd-xl'));
      expect(optionsShown()).toBe('null');

      await act(async () => {
        releaseXl(xlOptions);
      });
      expect(optionsShown()).toBe(JSON.stringify(xlOptions));
    });

    it('ignores a stale response for a previously picked model (latest wins)', async () => {
      // sd-turbo's response is held back until after sd-xl's has landed.
      let releaseTurbo: (v: ImageModelOptions) => void = () => {};
      chatApi.spies.modelImageOptions.mockImplementation((name: string) =>
        name === 'sd-turbo'
          ? new Promise<ImageModelOptions>((resolve) => {
              releaseTurbo = resolve;
            })
          : Promise.resolve(xlOptions),
      );
      renderProvider([], { models: imageOptionModels });
      await waitForReady();

      fireEvent.click(screen.getByRole('button', { name: 'pick-sd-turbo' }));
      await waitFor(() => expect(chatApi.spies.modelImageOptions).toHaveBeenCalledWith('sd-turbo'));
      fireEvent.click(screen.getByRole('button', { name: 'pick-sd-xl' }));
      await waitFor(() => expect(optionsShown()).toBe(JSON.stringify(xlOptions)));

      await act(async () => {
        releaseTurbo(turboOptions);
      });
      expect(optionsShown()).toBe(JSON.stringify(xlOptions));
    });
  });

  // The voice names the speech composer's picker offers. Fetched on demand when the
  // picked model SYNTHESIZES speech -- the image-options pattern: clear first, latest
  // wins, and a failure degrades to [] (the picker is then plain free text, the field
  // as it was before voices were discoverable).
  describe(`ChatStoreProvider per-model voices [${locale}]`, () => {
    const voiceModels: ModelOption[] = [
      {
        id: 'kokoro',
        display_name: 'kokoro',
        flavors: ['openai_audio'],
        loading_on_count: 0,
        speech: true,
      },
      {
        id: 'piper',
        display_name: 'piper',
        flavors: ['openai_audio'],
        loading_on_count: 0,
        speech: true,
      },
      { id: 'gpt-oss-20b', display_name: 'gpt-oss-20b', flavors: ['openai'], loading_on_count: 0 },
    ];
    const kokoroVoices = ['af_heart', 'bf_emma'];
    const piperVoices = ['amy'];

    const voicesShown = () => screen.getByTestId('model-voices').textContent;

    it('fetches nothing while the picked model does not synthesize speech', async () => {
      renderProvider([], { models: voiceModels });
      await waitForReady();

      fireEvent.click(screen.getByRole('button', { name: 'pick-gpt-oss-20b' }));

      expect(screen.getByTestId('model-speech-capable').textContent).toBe('false');
      expect(chatApi.spies.modelVoices).not.toHaveBeenCalled();
      expect(voicesShown()).toBe('[]');
    });

    it('fetches and exposes the voices of a picked speech model', async () => {
      chatApi.spies.modelVoices.mockResolvedValue(kokoroVoices);
      renderProvider([], { models: voiceModels });
      await waitForReady();
      expect(voicesShown()).toBe('[]');

      fireEvent.click(screen.getByRole('button', { name: 'pick-kokoro' }));

      await waitFor(() => expect(voicesShown()).toBe(JSON.stringify(kokoroVoices)));
      expect(chatApi.spies.modelVoices).toHaveBeenCalledTimes(1);
      expect(chatApi.spies.modelVoices).toHaveBeenCalledWith('kokoro');
    });

    it('refetches for another speech model and clears when a text model is picked', async () => {
      chatApi.spies.modelVoices.mockImplementation(async (name: string) =>
        name === 'kokoro' ? kokoroVoices : piperVoices,
      );
      renderProvider([], { models: voiceModels });
      await waitForReady();

      fireEvent.click(screen.getByRole('button', { name: 'pick-kokoro' }));
      await waitFor(() => expect(voicesShown()).toBe(JSON.stringify(kokoroVoices)));

      fireEvent.click(screen.getByRole('button', { name: 'pick-piper' }));
      await waitFor(() => expect(voicesShown()).toBe(JSON.stringify(piperVoices)));
      expect(chatApi.spies.modelVoices).toHaveBeenLastCalledWith('piper');

      fireEvent.click(screen.getByRole('button', { name: 'pick-gpt-oss-20b' }));
      await waitFor(() => expect(voicesShown()).toBe('[]'));
      // Going text must clear without another fetch.
      expect(chatApi.spies.modelVoices).toHaveBeenCalledTimes(2);
    });

    it('stays empty when the fetch fails (fail-open: free text)', async () => {
      chatApi.spies.modelVoices.mockRejectedValue(new Error('boom'));
      renderProvider([], { models: voiceModels });
      await waitForReady();

      fireEvent.click(screen.getByRole('button', { name: 'pick-kokoro' }));

      await waitFor(() => expect(chatApi.spies.modelVoices).toHaveBeenCalledWith('kokoro'));
      expect(voicesShown()).toBe('[]');
    });

    it('stays empty when the model has no cached voices', async () => {
      chatApi.spies.modelVoices.mockResolvedValue([]);
      renderProvider([], { models: voiceModels });
      await waitForReady();

      fireEvent.click(screen.getByRole('button', { name: 'pick-kokoro' }));

      await waitFor(() => expect(chatApi.spies.modelVoices).toHaveBeenCalledWith('kokoro'));
      expect(voicesShown()).toBe('[]');
    });

    it("drops the previous model's voices when the next fetch fails", async () => {
      chatApi.spies.modelVoices.mockImplementation(async (name: string) => {
        if (name === 'kokoro') return kokoroVoices;
        throw new Error('boom');
      });
      renderProvider([], { models: voiceModels });
      await waitForReady();

      fireEvent.click(screen.getByRole('button', { name: 'pick-kokoro' }));
      await waitFor(() => expect(voicesShown()).toBe(JSON.stringify(kokoroVoices)));

      fireEvent.click(screen.getByRole('button', { name: 'pick-piper' }));
      await waitFor(() => expect(chatApi.spies.modelVoices).toHaveBeenCalledWith('piper'));
      await waitFor(() => expect(voicesShown()).toBe('[]'));
    });

    it("does not keep the previous model's voices while the next fetch is in flight", async () => {
      let releasePiper: (v: string[]) => void = () => {};
      chatApi.spies.modelVoices.mockImplementation((name: string) =>
        name === 'kokoro'
          ? Promise.resolve(kokoroVoices)
          : new Promise<string[]>((resolve) => {
              releasePiper = resolve;
            }),
      );
      renderProvider([], { models: voiceModels });
      await waitForReady();

      fireEvent.click(screen.getByRole('button', { name: 'pick-kokoro' }));
      await waitFor(() => expect(voicesShown()).toBe(JSON.stringify(kokoroVoices)));

      // piper's fetch is pending: kokoro's voices must not linger as if they were
      // piper's (a voice piper does not have would be offered, and 500 on send).
      fireEvent.click(screen.getByRole('button', { name: 'pick-piper' }));
      await waitFor(() => expect(chatApi.spies.modelVoices).toHaveBeenCalledWith('piper'));
      expect(voicesShown()).toBe('[]');

      await act(async () => {
        releasePiper(piperVoices);
      });
      expect(voicesShown()).toBe(JSON.stringify(piperVoices));
    });

    it('ignores a stale response for a previously picked model (latest wins)', async () => {
      // kokoro's response is held back until after piper's has landed.
      let releaseKokoro: (v: string[]) => void = () => {};
      chatApi.spies.modelVoices.mockImplementation((name: string) =>
        name === 'kokoro'
          ? new Promise<string[]>((resolve) => {
              releaseKokoro = resolve;
            })
          : Promise.resolve(piperVoices),
      );
      renderProvider([], { models: voiceModels });
      await waitForReady();

      fireEvent.click(screen.getByRole('button', { name: 'pick-kokoro' }));
      await waitFor(() => expect(chatApi.spies.modelVoices).toHaveBeenCalledWith('kokoro'));
      fireEvent.click(screen.getByRole('button', { name: 'pick-piper' }));
      await waitFor(() => expect(voicesShown()).toBe(JSON.stringify(piperVoices)));

      await act(async () => {
        releaseKokoro(kokoroVoices);
      });
      expect(voicesShown()).toBe(JSON.stringify(piperVoices));
    });
  });

  // A text-to-speech model is picked like an image generator: the chat offers it, the
  // empty thread's composer follows it (prospectiveKind), and the first send pins the
  // thread as "speech". The audio settings ride the run only for that kind.
  // The portal speech-streaming run (ADR-068): the composer offers streaming only where the
  // gateway says the model streams AND its rate is known, primes the browser's audio inside
  // the send gesture, hands the run's `audio` events to the playback, and silences it when
  // the run ends any way but completing.
  describe(`ChatStoreProvider speech streaming [${locale}]`, () => {
    const speechModel = (id: string): ModelOption => ({
      id,
      display_name: id,
      flavors: ['openai_audio'],
      loading_on_count: 0,
      speech: true,
    });
    const kokoro = speechModel('kokoro');
    const piper = speechModel('piper');
    const open: SpeechStreamingGate = { streaming: true, sampleRate: 44100 };
    const speechThread = (audio?: unknown): ChatRow[] => [
      {
        id: 'c1',
        title: 'C1',
        created_at: T,
        updated_at: T,
        content: {
          settings: { model: kokoro.id, kind: 'speech', ...(audio ? { audio } : {}) },
          messages: [],
        },
      },
    ];
    const sendText = (text: string) => {
      fireEvent.change(screen.getByLabelText('probe-input'), { target: { value: text } });
      fireEvent.click(screen.getByRole('button', { name: 'send' }));
    };
    const chunk = (seq: number) => ({ audio: { seq, rate: 44100, data: 'AAAA' } });

    beforeEach(() => {
      playback.prime.mockClear();
      playback.end.mockClear();
      playback.stopAll.mockClear();
      playback.setMuted.mockClear();
      playback.push.mockReset().mockReturnValue(true);
    });

    it('asks whether the picked speech model may stream, and offers streaming only on a yes', async () => {
      chatApi = makeChatApi(speechThread());
      chatApi.spies.modelSpeechStreaming.mockResolvedValue(open);
      renderProvider([], { models: [kokoro] });
      await waitForReady();
      await waitFor(() => expect(screen.getByTestId('speech-streaming').textContent).toBe('true'));
      expect(chatApi.spies.modelSpeechStreaming).toHaveBeenCalledWith('kokoro');
    });

    it('offers no streaming while the gate is closed, and not before the answer arrives', async () => {
      chatApi = makeChatApi(speechThread());
      let answer: (g: SpeechStreamingGate) => void = () => {};
      chatApi.spies.modelSpeechStreaming.mockImplementation(
        () => new Promise<SpeechStreamingGate>((resolve) => (answer = resolve)),
      );
      renderProvider([], { models: [kokoro] });
      await waitForReady();
      await waitFor(() => expect(chatApi.spies.modelSpeechStreaming).toHaveBeenCalled());
      expect(screen.getByTestId('speech-streaming').textContent).toBe('false');
      await act(async () => answer(NO_SPEECH_STREAMING));
      expect(screen.getByTestId('speech-streaming').textContent).toBe('false');
    });

    it('fails closed when the lookup fails', async () => {
      chatApi = makeChatApi(speechThread());
      chatApi.spies.modelSpeechStreaming.mockRejectedValue(new Error('boom'));
      renderProvider([], { models: [kokoro] });
      await waitForReady();
      await waitFor(() => expect(chatApi.spies.modelSpeechStreaming).toHaveBeenCalled());
      expect(screen.getByTestId('speech-streaming').textContent).toBe('false');
    });

    it('does not ask for a model that is not a speech model', async () => {
      renderProvider([], { models });
      await waitForReady();
      expect(chatApi.spies.modelSpeechStreaming).not.toHaveBeenCalled();
      expect(screen.getByTestId('speech-streaming').textContent).toBe('false');
    });

    it("drops the previous model's answer at once when another model is picked", async () => {
      chatApi = makeChatApi(speechThread());
      chatApi.spies.modelSpeechStreaming.mockImplementation(async (name: string) =>
        name === 'kokoro' ? open : NO_SPEECH_STREAMING,
      );
      renderProvider([], { models: [kokoro, piper] });
      await waitForReady();
      await waitFor(() => expect(screen.getByTestId('speech-streaming').textContent).toBe('true'));
      fireEvent.click(screen.getByRole('button', { name: 'pick-piper' }));
      await waitFor(() =>
        expect(chatApi.spies.modelSpeechStreaming).toHaveBeenLastCalledWith('piper'),
      );
      expect(screen.getByTestId('speech-streaming').textContent).toBe('false');
    });

    it('primes the audio inside the send, before the run is requested, when streaming is on and open', async () => {
      chatApi = makeChatApi(speechThread());
      chatApi.spies.modelSpeechStreaming.mockResolvedValue(open);
      renderProvider([], { models: [kokoro] });
      await waitForReady();
      await waitFor(() => expect(screen.getByTestId('speech-streaming').textContent).toBe('true'));

      sendText('hello');
      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
      expect(playback.prime).toHaveBeenCalledTimes(1);
      expect(playback.prime.mock.invocationCallOrder[0]).toBeLessThan(
        chatApi.spies.startChatRun.mock.invocationCallOrder[0],
      );
    });

    it.each([
      ['the gate is closed', undefined, NO_SPEECH_STREAMING],
      ['the thread turned streaming off', { stream: false }, open],
    ])('does not prime the audio when %s', async (_name, audio, gate) => {
      chatApi = makeChatApi(speechThread(audio));
      chatApi.spies.modelSpeechStreaming.mockResolvedValue(gate);
      renderProvider([], { models: [kokoro] });
      await waitForReady();
      await waitFor(() => expect(chatApi.spies.modelSpeechStreaming).toHaveBeenCalled());

      sendText('hello');
      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
      expect(playback.prime).not.toHaveBeenCalled();
    });

    it("seeds the thread's streaming choice from the document and sends it with the run", async () => {
      chatApi = makeChatApi(speechThread({ voice: 'af_heart', stream: false }));
      renderProvider([], { models: [kokoro] });
      await waitForReady();
      expect(JSON.parse(screen.getByTestId('audio-settings').textContent!)).toMatchObject({
        stream: false,
      });
      sendText('hello');
      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
      const settings = (
        chatApi.spies.startChatRun.mock.calls[0][1] as {
          settings: { audio?: { stream?: boolean } };
        }
      ).settings;
      expect(settings.audio?.stream).toBe(false);
    });

    it('turning the choice back on is the default again, and is not written', async () => {
      chatApi = makeChatApi(speechThread({ stream: false }));
      renderProvider([], { models: [kokoro] });
      await waitForReady();
      fireEvent.click(screen.getByRole('button', { name: 'set-audio' })); // stream: true
      sendText('hello');
      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
      const settings = (
        chatApi.spies.startChatRun.mock.calls[0][1] as {
          settings: { audio?: Record<string, unknown> };
        }
      ).settings;
      expect(settings.audio).not.toHaveProperty('stream');
    });

    // Starts a speech run and returns its event source, the run subscribed.
    async function startRun() {
      chatApi = makeChatApi(speechThread());
      chatApi.spies.modelSpeechStreaming.mockResolvedValue(open);
      const view = renderProvider([], { models: [kokoro] });
      await waitForReady();
      await waitFor(() => expect(screen.getByTestId('speech-streaming').textContent).toBe('true'));
      sendText('hello');
      await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
      return { es: FakeEventSource.instances[0], ...view };
    }

    it('hands the run its audio chunks as they arrive and shows it as playing live', async () => {
      const { es } = await startRun();
      expect(screen.getByTestId('run-audio-live').textContent).toBe('false');

      act(() => {
        es.emit('snapshot', { reasoning: '', content: '', status: 'running', kind: 'speech' });
        es.emit('audio', chunk(0));
        es.emit('audio', chunk(1));
      });

      expect(playback.push).toHaveBeenCalledTimes(2);
      expect(playback.push).toHaveBeenNthCalledWith(1, 'c1', 'run_1', chunk(0).audio);
      expect(playback.push).toHaveBeenNthCalledWith(2, 'c1', 'run_1', chunk(1).audio);
      expect(screen.getByTestId('run-audio-live').textContent).toBe('true');
      // The audio never becomes text in the transcript.
      expect(screen.getByTestId('last').textContent).toBe('');
    });

    it('does not show the run as live when the playback refuses the stream (a gap)', async () => {
      playback.push.mockReturnValue(false);
      const { es } = await startRun();
      act(() => es.emit('audio', chunk(0)));
      expect(playback.push).toHaveBeenCalledTimes(1);
      expect(screen.getByTestId('run-audio-live').textContent).toBe('false');
    });

    it('ignores an audio event without a chunk', async () => {
      const { es } = await startRun();
      act(() => es.emit('audio', {}));
      expect(playback.push).not.toHaveBeenCalled();
    });

    it('lets a completed run play out, and shows the finished turn', async () => {
      const { es } = await startRun();
      act(() => es.emit('audio', chunk(0)));
      expect(screen.getByTestId('run-audio-live').textContent).toBe('true');

      act(() =>
        es.emit('done', {
          content: '',
          status: 'completed',
          kind: 'speech',
          content_parts: [{ type: 'audio_url', audio_url: { url: 'data:audio/wav;base64,UklG' } }],
        }),
      );
      expect(playback.end).toHaveBeenCalledWith('c1', true);
      expect(screen.getByTestId('run-audio-live').textContent).toBe('false');
      await waitFor(() =>
        expect(screen.getByTestId('last-parts').textContent).toBe(
          'audio_url:data:audio/wav;base64,UklG',
        ),
      );
    });

    it.each([
      ['a Stop', { status: 'canceled', error: '' }],
      ['an error', { status: 'error', error: 'gateway.chat_run_timeout' }],
    ])('silences a run that ends in %s', async (_name, terminal) => {
      const { es } = await startRun();
      act(() => es.emit('audio', chunk(0)));
      act(() => es.emit('done', { content: '', kind: 'speech', ...terminal }));
      expect(playback.end).toHaveBeenCalledWith('c1', false);
      expect(screen.getByTestId('run-audio-live').textContent).toBe('false');
    });

    it('ends the playback of a run it only learns is over from a terminal snapshot', async () => {
      const { es } = await startRun();
      act(() => es.emit('audio', chunk(0)));
      act(() => es.emit('snapshot', { content: '', status: 'completed', kind: 'speech' }));
      expect(playback.end).toHaveBeenCalledWith('c1', true);
    });

    it("silences a chat's audio when the chat is deleted", async () => {
      const { es } = await startRun();
      act(() => es.emit('audio', chunk(0)));
      fireEvent.click(screen.getByRole('button', { name: 'delete-c1' }));
      await waitFor(() => expect(playback.end).toHaveBeenCalledWith('c1', false));
    });

    // The live mute (#281): local playback only -- the run is neither stopped nor changed.
    describe('muting the live run', () => {
      it('hands the mute to the playback, mirrors it, and unmutes again', async () => {
        const { es } = await startRun();
        act(() => es.emit('audio', chunk(0)));
        expect(screen.getByTestId('run-audio-muted').textContent).toBe('false');

        fireEvent.click(screen.getByRole('button', { name: 'mute-audio' }));
        expect(playback.setMuted).toHaveBeenLastCalledWith('c1', true);
        expect(screen.getByTestId('run-audio-muted').textContent).toBe('true');

        fireEvent.click(screen.getByRole('button', { name: 'unmute-audio' }));
        expect(playback.setMuted).toHaveBeenLastCalledWith('c1', false);
        expect(screen.getByTestId('run-audio-muted').textContent).toBe('false');
      });

      it('does not cancel, end or otherwise touch the run', async () => {
        const { es } = await startRun();
        act(() => es.emit('audio', chunk(0)));
        fireEvent.click(screen.getByRole('button', { name: 'mute-audio' }));
        expect(chatApi.spies.cancelChatRun).not.toHaveBeenCalled();
        expect(playback.end).not.toHaveBeenCalled();
        expect(screen.getByTestId('run-audio-live').textContent).toBe('true');
      });

      it.each([
        ['completes', { status: 'completed', error: '' }],
        ['is canceled', { status: 'canceled', error: '' }],
      ])('does not outlive the run when it %s', async (_name, terminal) => {
        const { es } = await startRun();
        act(() => es.emit('audio', chunk(0)));
        fireEvent.click(screen.getByRole('button', { name: 'mute-audio' }));
        expect(screen.getByTestId('run-audio-muted').textContent).toBe('true');
        act(() => es.emit('done', { content: '', kind: 'speech', ...terminal }));
        expect(screen.getByTestId('run-audio-muted').textContent).toBe('false');
      });
    });

    it('silences every chat when the store goes away', async () => {
      const { unmount } = await startRun();
      unmount();
      expect(playback.stopAll).toHaveBeenCalled();
    });
  });

  // The speech composer's language control (settings.audio.language). It exists only where the
  // gateway says the model takes a `language` (api.modelSpeechLanguage): the speech streaming
  // gate's twin, so it is fetched for a speech model only, closed until the gateway answers,
  // latest-wins, and FAIL CLOSED. The gate also carries the model's declared default, which
  // drives the pre-fill: an EMPTY speech thread starts on the portal's UI language unless the
  // model says it detects the language itself ("auto") -- so German text on a model that
  // defaults to Chinese just works -- and anything the thread already has is left alone.
  describe(`ChatStoreProvider speech language [${locale}]`, () => {
    const speechModel = (id: string): ModelOption => ({
      id,
      display_name: id,
      flavors: ['openai_audio'],
      loading_on_count: 0,
      speech: true,
    });
    const kokoro = speechModel('kokoro');
    const piper = speechModel('piper');
    const takes = (def?: string): SpeechLanguageGate => ({ accepts: true, default: def ?? '' });
    // A speech thread on kokoro, optionally with saved audio settings and turns already spoken.
    const speechThread = (audio?: unknown, withTurn = false): ChatRow[] => [
      {
        id: 'c1',
        title: 'C1',
        created_at: T,
        updated_at: T,
        content: {
          settings: { model: kokoro.id, kind: 'speech', ...(audio ? { audio } : {}) },
          messages: withTurn ? [{ id: 'm1', role: 'user', content: 'hello' }] : [],
        },
      },
    ];
    const offered = () => screen.getByTestId('speech-language').textContent;
    const language = () =>
      (JSON.parse(screen.getByTestId('audio-settings').textContent!) as { language: string })
        .language;

    it('asks whether the picked speech model takes a language, and offers the control only on a yes', async () => {
      chatApi = makeChatApi(speechThread());
      chatApi.spies.modelSpeechLanguage.mockResolvedValue(takes('auto'));
      renderProvider([], { models: [kokoro] });
      await waitForReady();
      await waitFor(() => expect(offered()).toBe('true'));
      expect(chatApi.spies.modelSpeechLanguage).toHaveBeenCalledWith('kokoro');
    });

    it('offers nothing while the gate is closed, and not before the answer arrives', async () => {
      chatApi = makeChatApi(speechThread());
      let answer: (g: SpeechLanguageGate) => void = () => {};
      chatApi.spies.modelSpeechLanguage.mockImplementation(
        () => new Promise<SpeechLanguageGate>((resolve) => (answer = resolve)),
      );
      renderProvider([], { models: [kokoro] });
      await waitForReady();
      await waitFor(() => expect(chatApi.spies.modelSpeechLanguage).toHaveBeenCalled());
      expect(offered()).toBe('false');
      await act(async () => answer(NO_SPEECH_LANGUAGE));
      expect(offered()).toBe('false');
    });

    it('fails closed when the lookup fails', async () => {
      chatApi = makeChatApi(speechThread());
      chatApi.spies.modelSpeechLanguage.mockRejectedValue(new Error('boom'));
      renderProvider([], { models: [kokoro] });
      await waitForReady();
      await waitFor(() => expect(chatApi.spies.modelSpeechLanguage).toHaveBeenCalled());
      expect(offered()).toBe('false');
      expect(language()).toBe('');
    });

    it('does not ask for a model that is not a speech model', async () => {
      renderProvider([], { models });
      await waitForReady();
      expect(chatApi.spies.modelSpeechLanguage).not.toHaveBeenCalled();
      expect(offered()).toBe('false');
    });

    it("drops the previous model's answer at once when another model is picked", async () => {
      chatApi = makeChatApi(speechThread());
      chatApi.spies.modelSpeechLanguage.mockImplementation(async (name: string) =>
        name === 'kokoro' ? takes('auto') : NO_SPEECH_LANGUAGE,
      );
      renderProvider([], { models: [kokoro, piper] });
      await waitForReady();
      await waitFor(() => expect(offered()).toBe('true'));
      fireEvent.click(screen.getByRole('button', { name: 'pick-piper' }));
      await waitFor(() =>
        expect(chatApi.spies.modelSpeechLanguage).toHaveBeenLastCalledWith('piper'),
      );
      expect(offered()).toBe('false');
    });

    it('pre-fills the portal UI language when the model declares a default other than auto', async () => {
      chatApi = makeChatApi(speechThread());
      chatApi.spies.modelSpeechLanguage.mockResolvedValue(takes('zh'));
      renderProvider([], { models: [kokoro] });
      await waitForReady();

      await waitFor(() => expect(language()).toBe(locale));
    });

    it('leaves the language blank when the model declares no default (voice/auto decides)', async () => {
      // A model that takes a language but declares NO default (e.g. audio.cpp's
      // kokoro, whose default is "the selected voice prefix") must be treated like
      // an auto-detecting one: the control is offered, but nothing is forced in --
      // an empty default is not a wrong-language default to correct, and the model
      // may not even support the UI language (kokoro rejects "de").
      chatApi = makeChatApi(speechThread());
      chatApi.spies.modelSpeechLanguage.mockResolvedValue(takes());
      renderProvider([], { models: [kokoro] });
      await waitForReady();
      await waitFor(() => expect(offered()).toBe('true'));

      expect(language()).toBe('');
    });

    it.each(['auto', 'AUTO'])(
      'leaves the language blank when the model detects it itself (default "%s")',
      async (def) => {
        chatApi = makeChatApi(speechThread());
        chatApi.spies.modelSpeechLanguage.mockResolvedValue(takes(def));
        renderProvider([], { models: [kokoro] });
        await waitForReady();
        await waitFor(() => expect(offered()).toBe('true'));

        expect(language()).toBe('');
      },
    );

    it('pre-fills nothing for a model that does not take a language', async () => {
      chatApi = makeChatApi(speechThread());
      renderProvider([], { models: [kokoro] });
      await waitForReady();
      await waitFor(() => expect(chatApi.spies.modelSpeechLanguage).toHaveBeenCalled());

      expect(language()).toBe('');
    });

    it('never overwrites a language the thread already has', async () => {
      chatApi = makeChatApi(speechThread({ language: 'fr' }));
      chatApi.spies.modelSpeechLanguage.mockResolvedValue(takes('zh'));
      renderProvider([], { models: [kokoro] });
      await waitForReady();
      await waitFor(() => expect(offered()).toBe('true'));

      expect(language()).toBe('fr');
    });

    it('keeps a typed language when the gate answers again for another model', async () => {
      chatApi = makeChatApi(speechThread());
      chatApi.spies.modelSpeechLanguage.mockResolvedValue(takes('auto'));
      renderProvider([], { models: [kokoro, piper] });
      await waitForReady();
      await waitFor(() => expect(offered()).toBe('true'));
      fireEvent.click(screen.getByRole('button', { name: 'set-language' }));
      expect(language()).toBe('fr');

      chatApi.spies.modelSpeechLanguage.mockResolvedValue(takes('zh'));
      fireEvent.click(screen.getByRole('button', { name: 'pick-piper' }));
      await waitFor(() =>
        expect(chatApi.spies.modelSpeechLanguage).toHaveBeenLastCalledWith('piper'),
      );
      // Let piper's answer land before judging what it did to the field.
      await act(async () => {});
      expect(offered()).toBe('true');

      expect(language()).toBe('fr');
    });

    it('does not refill a language the user cleared', async () => {
      chatApi = makeChatApi(speechThread());
      chatApi.spies.modelSpeechLanguage.mockResolvedValue(takes('zh'));
      renderProvider([], { models: [kokoro] });
      await waitForReady();
      await waitFor(() => expect(language()).toBe(locale));

      fireEvent.click(screen.getByRole('button', { name: 'clear-language' }));
      await act(async () => {});

      expect(language()).toBe('');
    });

    it('does not refill a cleared language when the portal language changes', async () => {
      chatApi = makeChatApi(speechThread());
      chatApi.spies.modelSpeechLanguage.mockResolvedValue(takes('zh'));
      const { rerenderWithLocale } = renderProvider([], { models: [kokoro] });
      await waitForReady();
      await waitFor(() => expect(language()).toBe(locale));
      fireEvent.click(screen.getByRole('button', { name: 'clear-language' }));
      await act(async () => {});
      expect(language()).toBe('');

      // The pre-fill reads the UI language through a ref, so switching it re-runs nothing.
      rerenderWithLocale(locale === 'de' ? 'en' : 'de');
      await act(async () => {});

      expect(language()).toBe('');
    });

    it('does not rewrite a thread that has already spoken', async () => {
      chatApi = makeChatApi(speechThread(undefined, true));
      chatApi.spies.modelSpeechLanguage.mockResolvedValue(takes('zh'));
      renderProvider([], { models: [kokoro] });
      await waitForReady();
      await waitFor(() => expect(offered()).toBe('true'));
      await act(async () => {});

      expect(language()).toBe('');
    });

    it('does not pre-fill a text thread that has a speech model picked', async () => {
      chatApi = makeChatApi([
        {
          id: 'c1',
          title: 'C1',
          created_at: T,
          updated_at: T,
          content: {
            settings: { model: kokoro.id },
            messages: [{ id: 'm1', role: 'user', content: 'hello' }],
          },
        },
      ]);
      chatApi.spies.modelSpeechLanguage.mockResolvedValue(takes('zh'));
      renderProvider([], { models: [kokoro] });
      await waitForReady();
      await waitFor(() => expect(offered()).toBe('true'));
      await act(async () => {});

      expect(language()).toBe('');
    });
  });

  describe(`ChatStoreProvider speech-thread kind [${locale}]`, () => {
    // A TTS model: only the openai_audio flavor, with the backend's speech verdict.
    const kokoro: ModelOption = {
      id: 'kokoro',
      display_name: 'kokoro',
      flavors: ['openai_audio'],
      loading_on_count: 0,
      speech: true,
    };
    const speechModels: ModelOption[] = [kokoro];
    const wav = [{ type: 'audio_url' as const, audio_url: { url: 'data:audio/wav;base64,UklG' } }];
    const speechThread = (audio?: unknown, withTurn = true): ChatRow[] => [
      {
        id: 'c1',
        title: 'C1',
        created_at: T,
        updated_at: T,
        content: {
          settings: { model: kokoro.id, kind: 'speech', ...(audio ? { audio } : {}) },
          messages: withTurn
            ? [
                { id: 'u1', role: 'user', content: 'say hi' },
                { id: 'a1', role: 'assistant', content: wav, status: 'complete' },
              ]
            : [],
        },
      },
    ];
    const startSettings = (call = 0) =>
      (chatApi.spies.startChatRun.mock.calls[call][1] as { settings: Record<string, unknown> })
        .settings;
    const sendText = (text: string) => {
      fireEvent.change(screen.getByLabelText('probe-input'), { target: { value: text } });
      fireEvent.click(screen.getByRole('button', { name: 'send' }));
    };

    it('offers a speech-capable audio model, and only that one of the audio models', async () => {
      const shape = (id: string, flavors: string[], speech?: boolean): ModelOption => ({
        id,
        display_name: id,
        flavors,
        loading_on_count: 0,
        speech,
      });
      renderProvider([], {
        models: [
          shape('text-model', ['openai', 'anthropic']),
          // Transcription / unprobed audio: openai_audio without a speech verdict.
          shape('whisper-no-verdict', ['openai_audio']),
          shape('whisper-no', ['openai_audio'], false),
          shape('tts-yes', ['openai_audio'], true),
          // A verdict without the flavor the speech endpoint is served under.
          shape('tts-wrong-flavor', ['anthropic'], true),
          shape('audio-and-text', ['openai', 'openai_audio'], false),
        ],
      });
      await waitForReady();
      expect(screen.getByTestId('model-options').textContent).toBe(
        'text-model,tts-yes,audio-and-text',
      );
    });

    // speech_clone marks a voice-CLONE model: it keeps speech:true (the direct
    // endpoint serves it) but a text-only chat composer cannot supply the
    // reference voice it needs, so the picker drops it (#252) -- UNLESS the model
    // has configured preset voices (speech_clone_voices, #301), which make a clone
    // model usable without a reference voice. A speech model with no speech_clone
    // flag is fail-open (offered), and a clone model that ALSO transcribes is
    // still offered via the transcription half.
    const cloneAudio = (
      id: string,
      speech?: boolean,
      speech_clone?: boolean,
      transcription?: boolean,
      speech_clone_voices?: boolean,
    ): ModelOption => ({
      id,
      display_name: id,
      flavors: ['openai_audio'],
      loading_on_count: 0,
      speech,
      speech_clone,
      speech_clone_voices,
      transcription,
    });

    it('excludes a clone-task speech model from the picker, keeping plain tts (#252)', async () => {
      renderProvider([], {
        models: [
          cloneAudio('tts-plain', true, false),
          cloneAudio('tts-unclassified', true), // no speech_clone row -> fail-open, offered
          cloneAudio('voice-clone', true, true), // clone -> excluded from the speech picker
          cloneAudio('clone-and-asr', true, true, true), // still offered via transcription
        ],
      });
      await waitForReady();
      expect(screen.getByTestId('model-options').textContent).toBe(
        'tts-plain,tts-unclassified,clone-and-asr',
      );
    });

    it('offers a clone speech model that has configured preset voices (#301)', async () => {
      renderProvider([], {
        models: [
          cloneAudio('tts-plain', true, false),
          cloneAudio('clone-with-voices', true, true, false, true), // presets -> offered
          cloneAudio('clone-voices-false', true, true, false, false), // explicit false -> excluded
          cloneAudio('clone-voices-unset', true, true), // reference-only clone -> excluded
          cloneAudio('clone-voices-and-asr', true, true, true, true), // offered either way
          cloneAudio('plain-with-voices-flag', true, false, false, true), // not a clone: unchanged
        ],
      });
      await waitForReady();
      expect(screen.getByTestId('model-options').textContent).toBe(
        'tts-plain,clone-with-voices,clone-voices-and-asr,plain-with-voices-flag',
      );
    });

    it('opens the speech composer for a preset-voice clone model only (#301)', async () => {
      renderProvider([], {
        models: [
          cloneAudio('tts-plain', true, false),
          cloneAudio('clone-with-voices', true, true, false, true),
          // Offered only through its transcription verdict: it must NOT open the
          // speech composer (the kind precedence would otherwise pick speech).
          cloneAudio('clone-and-asr', true, true, true, false),
        ],
      });
      await waitForReady();

      fireEvent.click(screen.getByRole('button', { name: 'pick-clone-with-voices' }));
      expect(screen.getByTestId('model-speech-capable').textContent).toBe('true');
      expect(screen.getByTestId('chat-kind').textContent).toBe('speech');

      fireEvent.click(screen.getByRole('button', { name: 'pick-clone-and-asr' }));
      expect(screen.getByTestId('model-speech-capable').textContent).toBe('false');
      expect(screen.getByTestId('model-transcription-capable').textContent).toBe('true');
      expect(screen.getByTestId('chat-kind').textContent).toBe('transcription');

      // Plain tts is unchanged.
      fireEvent.click(screen.getByRole('button', { name: 'pick-tts-plain' }));
      expect(screen.getByTestId('model-speech-capable').textContent).toBe('true');
      expect(screen.getByTestId('chat-kind').textContent).toBe('speech');
    });

    it('leaves the offered text and image models exactly as they were', async () => {
      // The existing offered-models rule, restated with a speech model alongside:
      // adding the speech admission changed nothing for the others.
      const shape = (id: string, flavors: string[], image?: boolean): ModelOption => ({
        id,
        display_name: id,
        flavors,
        loading_on_count: 0,
        image,
      });
      renderProvider([], {
        models: [
          shape('text-model', ['openai']),
          shape('sd-verdict', ['openai_images'], true),
          shape('sd-no-verdict', ['openai_images'], false),
          kokoro,
        ],
      });
      await waitForReady();
      expect(screen.getByTestId('model-options').textContent).toBe('text-model,sd-verdict,kokoro');
    });

    it('switches an empty thread to the speech kind when a speech model is picked', async () => {
      renderProvider([], { models: [...models, kokoro] });
      await waitForReady();
      expect(screen.getByTestId('chat-kind').textContent).toBe('');

      fireEvent.click(screen.getByRole('button', { name: 'pick-kokoro' }));
      expect(screen.getByTestId('model-speech-capable').textContent).toBe('true');
      expect(screen.getByTestId('model-image-capable').textContent).toBe('false');
      expect(screen.getByTestId('chat-kind').textContent).toBe('speech');
      // Text-to-speech takes no image input, so nothing can be attached.
      expect(screen.getByTestId('attach-mode').textContent).toBe('none');

      // And back to a text model: the empty thread follows the pick again.
      fireEvent.click(screen.getByRole('button', { name: 'pick-gpt-oss-20b' }));
      expect(screen.getByTestId('model-speech-capable').textContent).toBe('false');
      expect(screen.getByTestId('chat-kind').textContent).toBe('');
    });

    it('lets the image verdict win for a model that is both image and speech capable', async () => {
      renderProvider([], {
        models: [{ ...kokoro, id: 'both', display_name: 'both', image: true }],
      });
      await waitForReady();
      fireEvent.click(screen.getByRole('button', { name: 'pick-both' }));
      expect(screen.getByTestId('model-image-capable').textContent).toBe('true');
      expect(screen.getByTestId('model-speech-capable').textContent).toBe('true');
      expect(screen.getByTestId('chat-kind').textContent).toBe('image');
    });

    it('starts the first send as a speech run carrying the audio settings, and pins the thread', async () => {
      renderProvider([], { models: [...models, kokoro] });
      await waitForReady();
      fireEvent.click(screen.getByRole('button', { name: 'pick-kokoro' }));
      fireEvent.click(screen.getByRole('button', { name: 'set-audio' }));
      sendText('hello there');

      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
      const settings = startSettings();
      expect(settings.model).toBe('kokoro');
      expect(settings.kind).toBe('speech');
      expect(settings.audio).toEqual({
        voice: 'af_heart',
        speed: 1.25,
        language: 'de',
        passthrough: { seed: 3 },
        keep_last_audio: 3,
      });
      // A speech run carries no image knobs.
      expect('image' in settings).toBe(false);

      // The pin wins from now on: a text model picked afterwards does not turn the
      // thread back into a text thread.
      fireEvent.click(screen.getByRole('button', { name: 'pick-gpt-oss-20b' }));
      expect(screen.getByTestId('chat-kind').textContent).toBe('speech');
    });

    it('sends only the audio fields the user moved off the default', async () => {
      renderProvider([], { models: speechModels });
      await waitForReady();
      fireEvent.click(screen.getByRole('button', { name: 'pick-kokoro' }));
      sendText('hello');

      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
      expect(startSettings().kind).toBe('speech');
      expect(startSettings().audio).toEqual({});
    });

    // Review focus: a text or image thread must be byte-identical. Neither carries an
    // audio key on the run body, and a text thread still carries no image one.
    it.each([
      ['a text thread', [models[0]], 'gpt-oss-20b', ''],
      [
        'an image thread',
        [
          {
            id: 'sd-turbo',
            display_name: 'sd-turbo',
            flavors: ['openai'],
            loading_on_count: 0,
            image: true,
          },
        ],
        'sd-turbo',
        'image',
      ],
    ] as [string, ModelOption[], string, string][])(
      'sends no audio key with the run of %s',
      async (_label, pool, pick, kind) => {
        renderProvider([], { models: pool });
        await waitForReady();
        fireEvent.click(screen.getByRole('button', { name: `pick-${pick}` }));
        // Audio state that is not defaults must still never reach another kind.
        fireEvent.click(screen.getByRole('button', { name: 'set-audio' }));
        sendText('hello');

        await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
        const settings = startSettings();
        expect(settings.kind).toBe(kind);
        expect('audio' in settings).toBe(false);
        expect('image' in settings).toBe(kind === 'image');
      },
    );

    it('refuses to send a speech turn whose advanced passthrough is not a JSON object', async () => {
      renderProvider([], { models: speechModels });
      await waitForReady();
      fireEvent.click(screen.getByRole('button', { name: 'pick-kokoro' }));
      fireEvent.click(screen.getByRole('button', { name: 'set-audio' }));
      fireEvent.click(screen.getByRole('button', { name: 'break-audio-passthrough' }));
      sendText('hello');

      // Said, not silently dropped: the knob the user typed would otherwise vanish.
      expect(await screen.findByText(t.chatAudioPassthroughInvalid)).toBeTruthy();
      expect(chatApi.spies.startChatRun).not.toHaveBeenCalled();
      // Nothing was stored or cleared: the composer keeps the text, the thread no turn.
      expect((screen.getByLabelText('probe-input') as HTMLInputElement).value).toBe('hello');
      expect(screen.getByTestId('count').textContent).toBe('0');

      // Fixing the text lets the same send through.
      fireEvent.click(screen.getByRole('button', { name: 'set-audio' }));
      fireEvent.click(screen.getByRole('button', { name: 'send' }));
      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
    });

    it('refuses a regenerate whose advanced passthrough is not a JSON object, transcript untouched', async () => {
      chatApi = makeChatApi(speechThread());
      renderProvider([], { models: speechModels });
      await waitForReady();
      fireEvent.click(screen.getByRole('button', { name: 'break-audio-passthrough' }));
      fireEvent.click(screen.getByRole('button', { name: 'regenerate-a1' }));

      expect(await screen.findByText(t.chatAudioPassthroughInvalid)).toBeTruthy();
      expect(chatApi.spies.startChatRun).not.toHaveBeenCalled();
      expect(screen.getByTestId('count').textContent).toBe('2');
    });

    // The backend ends such a run gateway.chat_run_speech_reserved_field AFTER the turn is
    // committed; the store mirrors the list and refuses the send first, naming the field.
    it('refuses to send a speech turn whose passthrough names a reserved field, and says which', async () => {
      renderProvider([], { models: speechModels });
      await waitForReady();
      fireEvent.click(screen.getByRole('button', { name: 'pick-kokoro' }));
      fireEvent.click(screen.getByRole('button', { name: 'reserve-audio-passthrough' }));
      sendText('hello');

      expect(
        await screen.findByText(t.chatSpeechPassthroughReserved('Response_Format')),
      ).toBeTruthy();
      expect(screen.queryByText(t.chatAudioPassthroughInvalid)).toBeNull();
      expect(chatApi.spies.startChatRun).not.toHaveBeenCalled();
      // Nothing was stored or cleared.
      expect((screen.getByLabelText('probe-input') as HTMLInputElement).value).toBe('hello');
      expect(screen.getByTestId('count').textContent).toBe('0');

      // Fixing the text lets the same send through.
      fireEvent.click(screen.getByRole('button', { name: 'set-audio' }));
      fireEvent.click(screen.getByRole('button', { name: 'send' }));
      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
    });

    it('refuses a regenerate whose passthrough names a reserved field, transcript untouched', async () => {
      chatApi = makeChatApi(speechThread());
      renderProvider([], { models: speechModels });
      await waitForReady();
      fireEvent.click(screen.getByRole('button', { name: 'reserve-audio-passthrough' }));
      fireEvent.click(screen.getByRole('button', { name: 'regenerate-a1' }));

      expect(
        await screen.findByText(t.chatSpeechPassthroughReserved('Response_Format')),
      ).toBeTruthy();
      expect(chatApi.spies.startChatRun).not.toHaveBeenCalled();
      expect(screen.getByTestId('count').textContent).toBe('2');
    });

    it('regenerates a speech turn without the vision guard: an audio part is not an image', async () => {
      chatApi = makeChatApi(speechThread());
      renderProvider([], { models: speechModels });
      await waitForReady();
      expect(screen.getByTestId('chat-kind').textContent).toBe('speech');

      fireEvent.click(screen.getByRole('button', { name: 'regenerate-a1' }));

      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
      expect(screen.queryByText(t.chatImageModelUnsupported)).toBeNull();
      expect(startSettings().kind).toBe('speech');
    });

    it('shows a stored audio answer part as it arrived', async () => {
      chatApi = makeChatApi(speechThread());
      renderProvider([], { models: speechModels });
      await waitForReady();
      expect(screen.getByTestId('last-parts').textContent).toBe(
        'audio_url:data:audio/wav;base64,UklG',
      );
    });

    it('persists the audio settings with the speech thread and seeds them back on reload', async () => {
      chatApi = makeChatApi(speechThread({ voice: 'bf_emma', speed: 0.8 }));
      const { unmount } = renderProvider([], { models: speechModels });
      await waitForReady();
      expect(screen.getByTestId('chat-kind').textContent).toBe('speech');
      // Seeded from the loaded document, speed shown as the blank-able string.
      expect(JSON.parse(screen.getByTestId('audio-settings').textContent!)).toEqual({
        voice: 'bf_emma',
        speed: '0.8',
        language: '',
        passthrough: '',
        stream: true,
        keepAudio: true,
        keepLast: '',
      });

      // The audio settings are the ONLY thing that changes here, so this also holds
      // the debounced save's dependency list: without audioSettings in it, no save
      // is ever scheduled for a settings-only edit.
      fireEvent.click(screen.getByRole('button', { name: 'set-audio' }));
      await waitFor(() => expect(chatApi.spies.saveChat).toHaveBeenCalled(), { timeout: 3000 });
      const saved = chatApi.spies.saveChat.mock.calls.at(-1)![1] as {
        content: { settings: { kind?: string; audio?: unknown } };
      };
      expect(saved.content.settings.kind).toBe('speech');
      expect(saved.content.settings.audio).toEqual({
        voice: 'af_heart',
        speed: 1.25,
        language: 'de',
        passthrough: { seed: 3 },
        keep_last_audio: 3,
      });

      // ...and the reload reads them back out of the saved document (the PUT
      // full-replaces the blob, so a setting not seeded here is erased next save).
      unmount();
      renderProvider([], { models: speechModels });
      await waitForReady();
      expect(JSON.parse(screen.getByTestId('audio-settings').textContent!)).toMatchObject({
        voice: 'af_heart',
        speed: '1.25',
        language: 'de',
        keepAudio: true,
        keepLast: '3',
      });
    });

    it('keeps the stored audio settings when an unrelated setting is saved', async () => {
      chatApi = makeChatApi(
        speechThread({ voice: 'bf_emma', speed: 0.8, passthrough: { a: 1 }, keep_last_audio: 0 }),
      );
      renderProvider([], { models: speechModels });
      await waitForReady();

      fireEvent.click(screen.getByRole('button', { name: 'set-system' }));
      await waitFor(() => expect(chatApi.spies.saveChat).toHaveBeenCalled(), { timeout: 3000 });
      const saved = chatApi.spies.saveChat.mock.calls.at(-1)![1] as {
        content: { settings: { audio?: unknown } };
      };
      expect(saved.content.settings.audio).toEqual({
        voice: 'bf_emma',
        speed: 0.8,
        passthrough: { a: 1 },
        keep_last_audio: 0,
      });
    });

    it.each([
      ['text', undefined],
      ['image', 'image'],
    ] as const)(
      'writes no audio key when saving a %s thread, whatever the audio state holds',
      async (_label, kind) => {
        chatApi = makeChatApi([
          {
            id: 'c1',
            title: 'C1',
            created_at: T,
            updated_at: T,
            content: {
              settings: { model: models[0].id, ...(kind ? { kind } : {}) },
              messages: [
                { id: 'u1', role: 'user', content: 'hello' },
                { id: 'a1', role: 'assistant', content: 'hi', status: 'complete' },
              ],
            },
          },
        ]);
        renderProvider();
        await waitForReady();

        // Audio state moved on a thread that is not a speech one: it must not persist.
        fireEvent.click(screen.getByRole('button', { name: 'set-audio' }));
        fireEvent.click(screen.getByRole('button', { name: 'set-system' }));
        await waitFor(() => expect(chatApi.spies.saveChat).toHaveBeenCalled(), { timeout: 3000 });
        const saved = chatApi.spies.saveChat.mock.calls.at(-1)![1] as {
          content: { settings: Record<string, unknown> };
        };
        expect(Object.keys(saved.content.settings)).not.toContain('audio');
        if (!kind) expect(Object.keys(saved.content.settings)).not.toContain('kind');
      },
    );
  });

  // The audio answer lifecycle (#281), store side: deleting one answer's audio and the
  // retroactive prune that tightening the retention setting asks for. The gateway does the
  // deleting; the store's job is to ask only when it was told to (a click, a CONFIRMED
  // tightening), to bring its own transcript in line afterwards so a later save cannot write
  // an audio_url the gateway has deleted, and never to delete in bulk unconfirmed.
  describe(`ChatStoreProvider audio answer lifecycle [${locale}]`, () => {
    const kokoro: ModelOption = {
      id: 'kokoro',
      display_name: 'kokoro',
      flavors: ['openai_audio'],
      loading_on_count: 0,
      speech: true,
    };
    const speechModels: ModelOption[] = [kokoro];
    const blob = (n: number) => `/api/portal/chat-blobs/blob_${n}`;
    const audioPart = (n: number) => ({ type: 'audio_url' as const, audio_url: { url: blob(n) } });
    const marker = { type: 'audio_removed' };
    const settingsFor = (audio?: Record<string, unknown>) => ({
      model: kokoro.id,
      kind: 'speech',
      ...(audio ? { audio } : {}),
    });
    const threeAnswers = [
      { id: 'u1', role: 'user', content: 'one' },
      { id: 'a1', role: 'assistant', content: [audioPart(1)], status: 'complete' },
      { id: 'u2', role: 'user', content: 'two' },
      { id: 'a2', role: 'assistant', content: [audioPart(2)], status: 'complete' },
      { id: 'u3', role: 'user', content: 'three' },
      { id: 'a3', role: 'assistant', content: [audioPart(3)], status: 'complete' },
    ];
    // A saved speech thread: three spoken answers unless told otherwise, and a retention
    // setting only when given.
    const spokenChat = (
      audio?: Record<string, unknown>,
      messageRows: unknown[] = threeAnswers,
    ): ChatRow[] => [
      {
        id: 'c1',
        title: 'C1',
        created_at: T,
        updated_at: T,
        content: { settings: settingsFor(audio), messages: messageRows },
      },
    ];
    const openSpokenChat = async (audio?: Record<string, unknown>, messageRows?: unknown[]) => {
      chatApi = makeChatApi(spokenChat(audio, messageRows));
      renderProvider([], { models: speechModels });
      await waitForReady();
    };

    const click = (name: string) => fireEvent.click(screen.getByRole('button', { name }));
    const transcript = () =>
      JSON.parse(screen.getByTestId('messages-json').textContent!) as ChatUiMessage[];
    const partsOf = (id: string) => transcript().find((m) => m.id === id)!.content;
    const pending = () =>
      JSON.parse(screen.getByTestId('pending-retention').textContent!) as {
        keepLast: number;
        excess: number;
      } | null;
    const shown = () => JSON.parse(screen.getByTestId('audio-settings').textContent!);
    type SavedBody = {
      content: {
        settings: { audio?: Record<string, unknown> };
        messages: { id: string; content: unknown }[];
      };
    };
    const lastSaved = () => chatApi.spies.saveChat.mock.calls.at(-1)![1] as SavedBody;
    const isSavedWithVoice = (call: unknown[]) =>
      (call[1] as SavedBody).content.settings.audio?.voice === 'bf_emma';
    // The save is debounced (800 ms): waiting it out is how "nothing was saved" is shown.
    const settle = () => new Promise((resolve) => setTimeout(resolve, 1000));
    const savedWithin = (check: () => boolean) =>
      waitFor(() => expect(check()).toBe(true), { timeout: 3000 });
    const deferred = <T,>() => {
      let resolve!: (value: T) => void;
      let reject!: (reason: unknown) => void;
      const promise = new Promise<T>((res, rej) => {
        resolve = res;
        reject = rej;
      });
      return { promise, resolve, reject };
    };
    const refused = (status: number, code: string) => new PortalApiError(status, code, 'refused');
    const originalAudio = [audioPart(1)];

    describe('deleting one answer', () => {
      it('asks the gateway, then shows that answer as not stored and no other', async () => {
        await openSpokenChat();

        click('delete-audio-a2');

        await waitFor(() =>
          expect(chatApi.spies.deleteMessageAudio).toHaveBeenCalledWith('c1', 'a2'),
        );
        await waitFor(() => expect(partsOf('a2')).toEqual([marker]));
        expect(partsOf('a1')).toEqual(originalAudio);
        expect(partsOf('a3')).toEqual([audioPart(3)]);
        expect(chatApi.spies.deleteMessageAudio).toHaveBeenCalledTimes(1);
        expect(chatApi.spies.pruneChatAudio).not.toHaveBeenCalled();
      });

      it('keeps the audio on screen until the gateway has actually deleted it', async () => {
        await openSpokenChat();
        const held = deferred<{ ok: boolean }>();
        chatApi.spies.deleteMessageAudio.mockReturnValueOnce(held.promise);

        click('delete-audio-a1');
        await waitFor(() => expect(chatApi.spies.deleteMessageAudio).toHaveBeenCalled());
        expect(partsOf('a1')).toEqual(originalAudio);

        held.resolve({ ok: true });
        await waitFor(() => expect(partsOf('a1')).toEqual([marker]));
      });

      it('keeps the text of a mixed answer and replaces only its audio', async () => {
        await openSpokenChat(undefined, [
          { id: 'u1', role: 'user', content: 'one' },
          {
            id: 'a1',
            role: 'assistant',
            content: [{ type: 'text', text: 'spoken words' }, audioPart(1), audioPart(2)],
            status: 'complete',
          },
        ]);

        click('delete-audio-a1');

        await waitFor(() =>
          expect(partsOf('a1')).toEqual([{ type: 'text', text: 'spoken words' }, marker]),
        );
      });

      it('leaves the transcript as it was and says why when the gateway refuses', async () => {
        await openSpokenChat();
        const err = refused(409, 'portal.chat_run_active');
        chatApi.spies.deleteMessageAudio.mockRejectedValueOnce(err);

        click('delete-audio-a1');

        expect(await screen.findByText(formatPortalError(err, t))).toBeTruthy();
        expect(partsOf('a1')).toEqual(originalAudio);
        expect(partsOf('a2')).toEqual([audioPart(2)]);
      });

      it('does not ask while the chat has a live run', async () => {
        await openSpokenChat();
        fireEvent.change(screen.getByLabelText('probe-input'), { target: { value: 'again' } });
        click('send');
        await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));

        click('delete-audio-a1');

        expect(await screen.findByText(t.chatBusy)).toBeTruthy();
        expect(chatApi.spies.deleteMessageAudio).not.toHaveBeenCalled();
        expect(partsOf('a1')).toEqual(originalAudio);
      });

      // Only an assistant answer that holds audio has any to delete: a user turn (a
      // transcription clip is the user's input), a text answer and an already-removed one are
      // no-ops, so no request is sent for them.
      it('sends nothing for a message that holds no answer audio', async () => {
        await openSpokenChat(undefined, [
          { id: 'u1', role: 'user', content: [audioPart(9)] },
          { id: 'a1', role: 'assistant', content: 'plain text', status: 'complete' },
          { id: 'a2', role: 'assistant', content: [marker], status: 'complete' },
        ]);

        click('delete-audio-u1');
        click('delete-audio-a1');
        click('delete-audio-a2');
        await settle();

        expect(chatApi.spies.deleteMessageAudio).not.toHaveBeenCalled();
        expect(partsOf('u1')).toEqual([audioPart(9)]);
      });

      // THE LOCAL COPY IS REFRESHED: the document a later save writes (a PUT replaces the whole
      // stored document) carries the marker. Had the store kept the audio_url, that save would
      // put back a part pointing at a blob the gateway has deleted, which serves a 404.
      it('saves the marker, never the deleted audio, with every later save', async () => {
        await openSpokenChat();
        click('delete-audio-a2');
        await waitFor(() => expect(partsOf('a2')).toEqual([marker]));

        // An unrelated edit makes a save of its own, after the delete.
        click('set-voice');
        await savedWithin(() => chatApi.spies.saveChat.mock.calls.some(isSavedWithVoice));

        const saved = chatApi.spies.saveChat.mock.calls.map((call) => call[1] as SavedBody);
        const afterDelete = saved.filter(
          (body) => body.content.settings.audio?.voice === 'bf_emma',
        );
        expect(afterDelete.length).toBeGreaterThan(0);
        for (const body of afterDelete) {
          expect(JSON.stringify(body.content.messages)).not.toContain(blob(2));
          expect(body.content.messages.find((m) => m.id === 'a2')!.content).toEqual([marker]);
          expect(JSON.stringify(body.content.messages)).toContain(blob(1));
        }
      });
    });

    describe('tightening the retention of a chat that holds more audio', () => {
      it('holds the stricter value for a confirm instead of saving or deleting anything', async () => {
        await openSpokenChat();

        click('keep-last-2');

        expect(pending()).toEqual({ keepLast: 2, excess: 1 });
        // The control shows what was asked for, so typing a count can carry on.
        expect(shown()).toMatchObject({ keepAudio: true, keepLast: '2' });
        await settle();
        expect(chatApi.spies.saveChat).not.toHaveBeenCalled();
        expect(chatApi.spies.pruneChatAudio).not.toHaveBeenCalled();
        expect(partsOf('a1')).toEqual(originalAudio);
      });

      it('runs a send with the stored retention while the stricter one is held', async () => {
        await openSpokenChat();
        click('keep-last-2');
        fireEvent.change(screen.getByLabelText('probe-input'), { target: { value: 'again' } });
        click('send');

        await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
        const body = chatApi.spies.startChatRun.mock.calls[0][1] as {
          settings: { audio?: Record<string, unknown> };
        };
        expect(body.settings.audio).toEqual({});
      });

      it('on confirm prunes to the new count, shows the pruned answers, and saves the setting', async () => {
        await openSpokenChat();
        click('keep-last-2');
        const held = deferred<{ ok: boolean }>();
        chatApi.spies.pruneChatAudio.mockReturnValueOnce(held.promise);

        click('confirm-retention');
        await waitFor(() => expect(chatApi.spies.pruneChatAudio).toHaveBeenCalledWith('c1', 2));
        // Nothing is shown as deleted, and no setting is committed, until the gateway says so.
        expect(partsOf('a1')).toEqual(originalAudio);
        expect(pending()).toEqual({ keepLast: 2, excess: 1 });

        held.resolve({ ok: true });
        await waitFor(() => expect(partsOf('a1')).toEqual([marker]));
        expect(partsOf('a2')).toEqual([audioPart(2)]);
        expect(partsOf('a3')).toEqual([audioPart(3)]);
        expect(pending()).toBeNull();
        expect(shown()).toMatchObject({ keepAudio: true, keepLast: '2' });
        expect(chatApi.spies.pruneChatAudio).toHaveBeenCalledTimes(1);

        // The new setting is persisted, with the pruned transcript beside it.
        await savedWithin(
          () =>
            chatApi.spies.saveChat.mock.calls.length > 0 &&
            lastSaved().content.settings.audio?.keep_last_audio === 2,
        );
        expect(lastSaved().content.settings.audio).toEqual({ keep_last_audio: 2 });
        expect(lastSaved().content.messages.find((m) => m.id === 'a1')!.content).toEqual([marker]);
        expect(JSON.stringify(lastSaved().content.messages)).not.toContain(blob(1));
      });

      it('carries the confirmed retention into the next run', async () => {
        await openSpokenChat();
        click('keep-last-2');
        click('confirm-retention');
        await waitFor(() => expect(pending()).toBeNull());
        await waitFor(() => expect(partsOf('a1')).toEqual([marker]));

        fireEvent.change(screen.getByLabelText('probe-input'), { target: { value: 'again' } });
        click('send');

        await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
        const body = chatApi.spies.startChatRun.mock.calls[0][1] as {
          settings: { audio?: Record<string, unknown> };
        };
        expect(body.settings.audio).toEqual({ keep_last_audio: 2 });
      });

      it('on decline reverts the control and persists and deletes nothing', async () => {
        await openSpokenChat();
        click('keep-last-2');
        expect(pending()).not.toBeNull();

        click('cancel-retention');

        expect(pending()).toBeNull();
        expect(shown()).toMatchObject({ keepAudio: true, keepLast: '' });
        await settle();
        expect(chatApi.spies.saveChat).not.toHaveBeenCalled();
        expect(chatApi.spies.pruneChatAudio).not.toHaveBeenCalled();
        expect(partsOf('a1')).toEqual(originalAudio);
      });

      it('treats turning "keep audio" off as keeping none: confirming removes every answer audio', async () => {
        await openSpokenChat();

        click('keep-audio-off');

        expect(pending()).toEqual({ keepLast: 0, excess: 3 });
        expect(shown()).toMatchObject({ keepAudio: false });
        expect(chatApi.spies.pruneChatAudio).not.toHaveBeenCalled();

        click('confirm-retention');
        await waitFor(() => expect(chatApi.spies.pruneChatAudio).toHaveBeenCalledWith('c1', 0));
        await waitFor(() => expect(partsOf('a3')).toEqual([marker]));
        expect(partsOf('a1')).toEqual([marker]);
        expect(partsOf('a2')).toEqual([marker]);
        // The user turns, their text, are untouched.
        expect(partsOf('u1')).toBe('one');
        await savedWithin(
          () =>
            chatApi.spies.saveChat.mock.calls.length > 0 &&
            lastSaved().content.settings.audio?.keep_last_audio === 0,
        );
      });

      it('tightens from a stored count the same way', async () => {
        await openSpokenChat({ keep_last_audio: 3 });

        click('keep-last-1');

        expect(pending()).toEqual({ keepLast: 1, excess: 2 });
        click('confirm-retention');
        await waitFor(() => expect(chatApi.spies.pruneChatAudio).toHaveBeenCalledWith('c1', 1));
        await waitFor(() => expect(partsOf('a2')).toEqual([marker]));
        expect(partsOf('a1')).toEqual([marker]);
        expect(partsOf('a3')).toEqual([audioPart(3)]);
      });

      // Typing a count goes through its first digit: from keep-all, "1" then "0" is a "1"
      // (stricter, held) and then a "10" that the chat's audio does not exceed. The count that
      // ends up typed is what counts, and the held value never deletes on the way.
      it('lets a count be typed through a stricter first digit without deleting', async () => {
        await openSpokenChat();

        click('keep-last-1');
        expect(pending()).toEqual({ keepLast: 1, excess: 2 });
        click('keep-last-2');
        expect(pending()).toEqual({ keepLast: 2, excess: 1 });
        click('keep-last-10');

        expect(pending()).toBeNull();
        expect(shown()).toMatchObject({ keepAudio: true, keepLast: '10' });
        expect(chatApi.spies.pruneChatAudio).not.toHaveBeenCalled();
        await savedWithin(
          () =>
            chatApi.spies.saveChat.mock.calls.length > 0 &&
            lastSaved().content.settings.audio?.keep_last_audio === 10,
        );
        expect(partsOf('a1')).toEqual(originalAudio);
      });

      it('applies an edit of another setting at once and keeps the held retention apart', async () => {
        await openSpokenChat();
        click('keep-last-2');

        click('set-voice');

        expect(shown()).toMatchObject({ voice: 'bf_emma', keepLast: '2' });
        expect(pending()).toEqual({ keepLast: 2, excess: 1 });
        // The voice is saved; the held retention is not.
        await savedWithin(() => chatApi.spies.saveChat.mock.calls.length > 0);
        expect(lastSaved().content.settings.audio).toEqual({ voice: 'bf_emma' });

        click('cancel-retention');
        expect(shown()).toMatchObject({ voice: 'bf_emma', keepAudio: true, keepLast: '' });
      });

      it('applies a tightening that leaves no stored audio beyond the limit without asking', async () => {
        await openSpokenChat();

        click('keep-last-5'); // three answers hold audio

        expect(pending()).toBeNull();
        expect(shown()).toMatchObject({ keepLast: '5' });
        await savedWithin(
          () =>
            chatApi.spies.saveChat.mock.calls.length > 0 &&
            lastSaved().content.settings.audio?.keep_last_audio === 5,
        );
        expect(chatApi.spies.pruneChatAudio).not.toHaveBeenCalled();
      });

      it('applies "do not store" at once on a chat that holds no audio', async () => {
        await openSpokenChat(undefined, [
          { id: 'u1', role: 'user', content: 'one' },
          { id: 'a1', role: 'assistant', content: 'a text answer', status: 'complete' },
        ]);

        click('keep-audio-off');

        expect(pending()).toBeNull();
        expect(shown()).toMatchObject({ keepAudio: false });
        await savedWithin(
          () =>
            chatApi.spies.saveChat.mock.calls.length > 0 &&
            lastSaved().content.settings.audio?.keep_last_audio === 0,
        );
        expect(chatApi.spies.pruneChatAudio).not.toHaveBeenCalled();
      });

      it('counts only answers that hold audio: a not-stored answer takes no slot', async () => {
        await openSpokenChat(undefined, [
          { id: 'a1', role: 'assistant', content: [audioPart(1)], status: 'complete' },
          { id: 'a2', role: 'assistant', content: [marker], status: 'complete' },
          { id: 'a3', role: 'assistant', content: [audioPart(3)], status: 'complete' },
        ]);

        click('keep-last-1');

        expect(pending()).toEqual({ keepLast: 1, excess: 1 });
        click('confirm-retention');
        await waitFor(() => expect(partsOf('a1')).toEqual([marker]));
        expect(partsOf('a3')).toEqual([audioPart(3)]);
      });

      it('keeps the hold, says why, and deletes nothing when the gateway refuses the prune', async () => {
        await openSpokenChat();
        click('keep-last-2');
        const err = refused(409, 'portal.chat_run_active');
        chatApi.spies.pruneChatAudio.mockRejectedValueOnce(err);

        click('confirm-retention');

        expect(await screen.findByText(formatPortalError(err, t))).toBeTruthy();
        expect(partsOf('a1')).toEqual(originalAudio);
        expect(pending()).toEqual({ keepLast: 2, excess: 1 });
        expect(shown()).toMatchObject({ keepLast: '2' });
        await settle();
        expect(chatApi.spies.saveChat).not.toHaveBeenCalled();

        // It can be confirmed again.
        click('confirm-retention');
        await waitFor(() => expect(partsOf('a1')).toEqual([marker]));
        expect(chatApi.spies.pruneChatAudio).toHaveBeenCalledTimes(2);
      });

      it('does not prune while the chat has a live run', async () => {
        await openSpokenChat();
        click('keep-last-2');
        fireEvent.change(screen.getByLabelText('probe-input'), { target: { value: 'again' } });
        click('send');
        await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));

        click('confirm-retention');

        expect(await screen.findByText(t.chatBusy)).toBeTruthy();
        expect(chatApi.spies.pruneChatAudio).not.toHaveBeenCalled();
        expect(pending()).toEqual({ keepLast: 2, excess: 1 });
      });

      it('has nothing to confirm or decline when nothing is held', async () => {
        await openSpokenChat();

        click('confirm-retention');
        click('cancel-retention');
        await settle();

        expect(chatApi.spies.pruneChatAudio).not.toHaveBeenCalled();
        expect(chatApi.spies.saveChat).not.toHaveBeenCalled();
        expect(shown()).toMatchObject({ keepAudio: true, keepLast: '' });
      });

      it('drops a held retention when another chat is opened', async () => {
        chatApi = makeChatApi([
          ...spokenChat(),
          {
            id: 'c2',
            title: 'C2',
            created_at: T1,
            updated_at: T1,
            content: { settings: settingsFor(), messages: [] },
          },
        ]);
        renderProvider([], { models: speechModels });
        await waitForReady();
        click('keep-last-2');
        expect(pending()).not.toBeNull();

        click('select-c2');
        await waitFor(() => expect(screen.getByTestId('active').textContent).toBe('c2'));
        expect(pending()).toBeNull();
        expect(shown()).toMatchObject({ keepAudio: true, keepLast: '' });

        click('select-c1');
        await waitFor(() => expect(screen.getByTestId('active').textContent).toBe('c1'));
        expect(pending()).toBeNull();
        expect(shown()).toMatchObject({ keepAudio: true, keepLast: '' });
        click('confirm-retention');
        await settle();
        expect(chatApi.spies.pruneChatAudio).not.toHaveBeenCalled();
      });
    });

    // Raising the retention, keeping everything and switching storing back on never delete
    // anything, so they take effect without a question.
    describe('loosening the retention', () => {
      it.each([
        ['a larger count', { keep_last_audio: 2 }, 'keep-last-5', { keep_last_audio: 5 }],
        ['keep all', { keep_last_audio: 2 }, 'keep-last-blank', {}],
        ['storing audio again', { keep_last_audio: 0 }, 'keep-audio-on', {}],
        [
          'a count after do-not-store',
          { keep_last_audio: 0 },
          'keep-last-1',
          { keep_last_audio: 1 },
        ],
      ] as const)(
        'applies %s at once, with no question and no prune',
        async (_label, stored, button, wire) => {
          await openSpokenChat(stored);

          click(button);

          expect(pending()).toBeNull();
          await savedWithin(
            () =>
              chatApi.spies.saveChat.mock.calls.length > 0 &&
              JSON.stringify(lastSaved().content.settings.audio) === JSON.stringify(wire),
          );
          expect(chatApi.spies.pruneChatAudio).not.toHaveBeenCalled();
          expect(chatApi.spies.deleteMessageAudio).not.toHaveBeenCalled();
          expect(partsOf('a1')).toEqual(originalAudio);
        },
      );
    });

    // Judged on what the wire would carry, not on the raw text: a count the wire drops is
    // keep-all, which never tightens anything.
    describe('a blank or invalid count', () => {
      it.each([['blank'], ['abc'], ['1.5'], ['0']])(
        'is not read as a stricter change (%s)',
        async (text) => {
          await openSpokenChat();

          click(`keep-last-${text}`);

          expect(pending()).toBeNull();
          expect(shown()).toMatchObject({
            keepAudio: true,
            keepLast: text === 'blank' ? '' : text,
          });
          expect(chatApi.spies.pruneChatAudio).not.toHaveBeenCalled();
          expect(partsOf('a1')).toEqual(originalAudio);
        },
      );

      it('does not hold a count that is dropped on the way from a stored one either', async () => {
        await openSpokenChat({ keep_last_audio: 2 });

        click('keep-last-abc');

        expect(pending()).toBeNull();
        expect(chatApi.spies.pruneChatAudio).not.toHaveBeenCalled();
        expect(partsOf('a1')).toEqual(originalAudio);
      });
    });
  });

  // A speech-to-text model is picked like a text-to-speech one: the chat offers it, the
  // empty thread's composer follows it (prospectiveKind), and the first send pins the thread as
  // "transcription". The transcription settings ride the run only for that kind.
  describe(`ChatStoreProvider transcription-thread kind [${locale}]`, () => {
    // An ASR model: only the openai_audio flavor, with the backend's transcription verdict.
    const whisper: ModelOption = {
      id: 'whisper',
      display_name: 'whisper',
      flavors: ['openai_audio'],
      loading_on_count: 0,
      transcription: true,
    };
    const kokoro: ModelOption = {
      id: 'kokoro',
      display_name: 'kokoro',
      flavors: ['openai_audio'],
      loading_on_count: 0,
      speech: true,
    };
    const transcriptionModels: ModelOption[] = [whisper];
    const clip = [
      { type: 'audio_url' as const, audio_url: { url: 'data:audio/webm;base64,AAAA' } },
    ];
    const transcriptionThread = (
      transcription?: unknown,
      opts: { withTurn?: boolean; title?: string } = {},
    ): ChatRow[] => [
      {
        id: 'c1',
        title: opts.title ?? 'C1',
        created_at: T,
        updated_at: T,
        content: {
          settings: {
            model: whisper.id,
            kind: 'transcription',
            ...(transcription ? { transcription } : {}),
          },
          messages:
            (opts.withTurn ?? true)
              ? [
                  { id: 'u1', role: 'user', content: clip },
                  { id: 'a1', role: 'assistant', content: 'hello there', status: 'complete' },
                ]
              : [],
        },
      },
    ];
    const startSettings = (call = 0) =>
      (chatApi.spies.startChatRun.mock.calls[call][1] as { settings: Record<string, unknown> })
        .settings;
    // A transcription turn is audio only: the clip is attached through handleFiles (a wav
    // file here) and Send sends it. sendText stands in as the turn of the OTHER kinds.
    const sendText = (text: string) => {
      fireEvent.change(screen.getByLabelText('probe-input'), { target: { value: text } });
      fireEvent.click(screen.getByRole('button', { name: 'send' }));
    };
    const attachClip = async (file = new File(['RIFFclip'], 'clip.wav', { type: 'audio/wav' })) => {
      fireEvent.change(screen.getByLabelText('probe-audio-file'), { target: { files: [file] } });
      await waitFor(() => expect(screen.getByTestId('composer-audio').textContent).not.toBe(''));
    };
    const sendClip = async () => {
      await attachClip();
      fireEvent.click(screen.getByRole('button', { name: 'send' }));
    };
    // The data url of a clip the converter stand-in (see the vi.mock above) made of an upload
    // that held `text`: always audio/wav, the converted bytes.
    const wavUrl = (text: string) => `data:audio/wav;base64,${btoa(`WAV:${text}`)}`;
    const clipUrl = wavUrl('RIFFclip');
    const toWav = vi.mocked(audioBlobToWav);
    beforeEach(() => toWav.mockClear());

    it('offers a transcription-capable audio model, and only that one of the audio models', async () => {
      const shape = (
        id: string,
        flavors: string[],
        flags: { speech?: boolean; transcription?: boolean } = {},
      ): ModelOption => ({ id, display_name: id, flavors, loading_on_count: 0, ...flags });
      renderProvider([], {
        models: [
          shape('text-model', ['openai', 'anthropic']),
          // Unprobed audio: openai_audio without any verdict.
          shape('audio-no-verdict', ['openai_audio']),
          shape('asr-no', ['openai_audio'], { transcription: false }),
          shape('asr-yes', ['openai_audio'], { transcription: true }),
          // A verdict without the flavor the transcription endpoint is served under.
          shape('asr-wrong-flavor', ['anthropic'], { transcription: true }),
          shape('audio-and-text', ['openai', 'openai_audio'], { transcription: false }),
          // The speech admission is untouched next to it.
          shape('tts-yes', ['openai_audio'], { speech: true }),
        ],
      });
      await waitForReady();
      expect(screen.getByTestId('model-options').textContent).toBe(
        'text-model,asr-yes,audio-and-text,tts-yes',
      );
    });

    it('leaves the offered text, image and speech models exactly as they were', async () => {
      const shape = (id: string, flavors: string[], image?: boolean): ModelOption => ({
        id,
        display_name: id,
        flavors,
        loading_on_count: 0,
        image,
      });
      renderProvider([], {
        models: [
          shape('text-model', ['openai']),
          shape('sd-verdict', ['openai_images'], true),
          shape('sd-no-verdict', ['openai_images'], false),
          kokoro,
          whisper,
        ],
      });
      await waitForReady();
      expect(screen.getByTestId('model-options').textContent).toBe(
        'text-model,sd-verdict,kokoro,whisper',
      );
    });

    it('switches an empty thread to the transcription kind when a transcription model is picked', async () => {
      renderProvider([], { models: [...models, whisper] });
      await waitForReady();
      expect(screen.getByTestId('chat-kind').textContent).toBe('');
      expect(screen.getByTestId('model-transcription-capable').textContent).toBe('false');

      fireEvent.click(screen.getByRole('button', { name: 'pick-whisper' }));
      expect(screen.getByTestId('model-transcription-capable').textContent).toBe('true');
      expect(screen.getByTestId('model-speech-capable').textContent).toBe('false');
      expect(screen.getByTestId('model-image-capable').textContent).toBe('false');
      expect(screen.getByTestId('chat-kind').textContent).toBe('transcription');
      // A transcription run reads only its audio, so no image can be attached.
      expect(screen.getByTestId('attach-mode').textContent).toBe('none');

      // And back to a text model: the empty thread follows the pick again.
      fireEvent.click(screen.getByRole('button', { name: 'pick-gpt-oss-20b' }));
      expect(screen.getByTestId('model-transcription-capable').textContent).toBe('false');
      expect(screen.getByTestId('chat-kind').textContent).toBe('');
    });

    // The precedence chain is image -> speech -> transcription. A model carrying two of the
    // verdicts only arises from manual configuration (the probe never produces it), and the
    // earlier kind keeps what it always did.
    it.each([
      ['image and transcription', { image: true, transcription: true }, 'image'],
      ['speech and transcription', { speech: true, transcription: true }, 'speech'],
      [
        'image, speech and transcription',
        { image: true, speech: true, transcription: true },
        'image',
      ],
      ['transcription alone', { transcription: true }, 'transcription'],
    ] as [string, Partial<ModelOption>, string][])(
      'picks the %s model’s kind by that precedence',
      async (_label, flags, kind) => {
        renderProvider([], {
          models: [{ ...whisper, id: 'multi', display_name: 'multi', ...flags }],
        });
        await waitForReady();
        fireEvent.click(screen.getByRole('button', { name: 'pick-multi' }));
        expect(screen.getByTestId('model-transcription-capable').textContent).toBe('true');
        expect(screen.getByTestId('chat-kind').textContent).toBe(kind);
      },
    );

    it('starts the first send as a transcription run carrying the settings, and pins the thread', async () => {
      renderProvider([], { models: [...models, whisper] });
      await waitForReady();
      fireEvent.click(screen.getByRole('button', { name: 'pick-whisper' }));
      fireEvent.click(screen.getByRole('button', { name: 'set-transcription' }));
      await sendClip();

      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
      const settings = startSettings();
      expect(settings.model).toBe('whisper');
      expect(settings.kind).toBe('transcription');
      // temperature 0 is an explicit, deterministic setting and is sent as such.
      expect(settings.transcription).toEqual({
        language: 'de',
        prompt: 'Namen: Zoe',
        temperature: 0,
        passthrough: { beam: 5 },
      });
      // A transcription run carries no image or speech knobs.
      expect('image' in settings).toBe(false);
      expect('audio' in settings).toBe(false);

      // The pin wins from now on: a text model picked afterwards does not turn the thread
      // back into a text thread.
      fireEvent.click(screen.getByRole('button', { name: 'pick-gpt-oss-20b' }));
      expect(screen.getByTestId('chat-kind').textContent).toBe('transcription');
    });

    it('sends only the transcription fields the user set', async () => {
      renderProvider([], { models: transcriptionModels });
      await waitForReady();
      fireEvent.click(screen.getByRole('button', { name: 'pick-whisper' }));
      await sendClip();

      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
      expect(startSettings().kind).toBe('transcription');
      expect(startSettings().transcription).toEqual({});
    });

    // Review focus 2: a text, image or speech thread must be byte-identical. None carries a
    // transcription key on the run body, whatever the transcription state holds.
    it.each([
      ['a text thread', [models[0]], 'gpt-oss-20b', ''],
      [
        'an image thread',
        [
          {
            id: 'sd-turbo',
            display_name: 'sd-turbo',
            flavors: ['openai'],
            loading_on_count: 0,
            image: true,
          },
        ],
        'sd-turbo',
        'image',
      ],
      ['a speech thread', [kokoro], 'kokoro', 'speech'],
    ] as [string, ModelOption[], string, string][])(
      'sends no transcription key with the run of %s',
      async (_label, pool, pick, kind) => {
        renderProvider([], { models: pool });
        await waitForReady();
        fireEvent.click(screen.getByRole('button', { name: `pick-${pick}` }));
        // Transcription state that is not defaults must still never reach another kind.
        fireEvent.click(screen.getByRole('button', { name: 'set-transcription' }));
        sendText('hello');

        await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
        const settings = startSettings();
        expect(settings.kind).toBe(kind);
        expect('transcription' in settings).toBe(false);
        expect('image' in settings).toBe(kind === 'image');
        expect('audio' in settings).toBe(kind === 'speech');
      },
    );

    it('refuses to send a transcription turn whose advanced passthrough is not a JSON object', async () => {
      renderProvider([], { models: transcriptionModels });
      await waitForReady();
      fireEvent.click(screen.getByRole('button', { name: 'pick-whisper' }));
      fireEvent.click(screen.getByRole('button', { name: 'set-transcription' }));
      fireEvent.click(screen.getByRole('button', { name: 'break-transcription-passthrough' }));
      await sendClip();

      // Said, not silently dropped: the knob the user typed would otherwise vanish.
      expect(await screen.findByText(t.chatTranscriptionPassthroughInvalid)).toBeTruthy();
      expect(chatApi.spies.startChatRun).not.toHaveBeenCalled();
      // Nothing was stored or cleared: the composer keeps the clip, the thread no turn.
      expect(screen.getByTestId('composer-audio').textContent).toBe(clipUrl);
      expect(screen.getByTestId('count').textContent).toBe('0');

      // Fixing the text lets the same send through.
      fireEvent.click(screen.getByRole('button', { name: 'set-transcription' }));
      fireEvent.click(screen.getByRole('button', { name: 'send' }));
      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
    });

    it('refuses a regenerate whose advanced passthrough is not a JSON object, transcript untouched', async () => {
      chatApi = makeChatApi(transcriptionThread());
      renderProvider([], { models: transcriptionModels });
      await waitForReady();
      fireEvent.click(screen.getByRole('button', { name: 'break-transcription-passthrough' }));
      fireEvent.click(screen.getByRole('button', { name: 'regenerate-a1' }));

      expect(await screen.findByText(t.chatTranscriptionPassthroughInvalid)).toBeTruthy();
      expect(chatApi.spies.startChatRun).not.toHaveBeenCalled();
      expect(screen.getByTestId('count').textContent).toBe('2');
    });

    // The backend ends such a run gateway.chat_run_transcription_reserved_field AFTER the
    // turn is committed; the store mirrors the list and refuses the send first, naming the
    // field exactly as the user wrote it.
    it('refuses to send a transcription turn whose passthrough names a reserved field, and says which', async () => {
      renderProvider([], { models: transcriptionModels });
      await waitForReady();
      fireEvent.click(screen.getByRole('button', { name: 'pick-whisper' }));
      fireEvent.click(screen.getByRole('button', { name: 'reserve-transcription-passthrough' }));
      await sendClip();

      expect(await screen.findByText(t.chatTranscriptionPassthroughReserved('File'))).toBeTruthy();
      expect(screen.queryByText(t.chatTranscriptionPassthroughInvalid)).toBeNull();
      expect(chatApi.spies.startChatRun).not.toHaveBeenCalled();
      // Nothing was stored or cleared.
      expect(screen.getByTestId('composer-audio').textContent).toBe(clipUrl);
      expect(screen.getByTestId('count').textContent).toBe('0');

      // Fixing the text lets the same send through.
      fireEvent.click(screen.getByRole('button', { name: 'set-transcription' }));
      fireEvent.click(screen.getByRole('button', { name: 'send' }));
      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
    });

    it('refuses a regenerate whose passthrough names a reserved field, transcript untouched', async () => {
      chatApi = makeChatApi(transcriptionThread());
      renderProvider([], { models: transcriptionModels });
      await waitForReady();
      fireEvent.click(screen.getByRole('button', { name: 'reserve-transcription-passthrough' }));
      fireEvent.click(screen.getByRole('button', { name: 'regenerate-a1' }));

      expect(await screen.findByText(t.chatTranscriptionPassthroughReserved('File'))).toBeTruthy();
      expect(chatApi.spies.startChatRun).not.toHaveBeenCalled();
      expect(screen.getByTestId('count').textContent).toBe('2');
    });

    it('does not apply the transcription guard to a speech thread, nor the speech one to a transcription thread', async () => {
      // A speech thread whose TRANSCRIPTION state is broken still sends...
      renderProvider([], { models: [kokoro] });
      await waitForReady();
      fireEvent.click(screen.getByRole('button', { name: 'pick-kokoro' }));
      fireEvent.click(screen.getByRole('button', { name: 'break-transcription-passthrough' }));
      sendText('hello');
      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
      expect(startSettings().kind).toBe('speech');
      cleanup();

      // ...and a transcription thread whose AUDIO (speech) state is broken still sends.
      chatApi = makeChatApi();
      FakeEventSource.instances = [];
      renderProvider([], { models: transcriptionModels });
      await waitForReady();
      fireEvent.click(screen.getByRole('button', { name: 'pick-whisper' }));
      fireEvent.click(screen.getByRole('button', { name: 'break-audio-passthrough' }));
      await sendClip();
      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
      expect(startSettings().kind).toBe('transcription');
    });

    describe('the audio turn', () => {
      const bodyOf = (call = 0) =>
        chatApi.spies.startChatRun.mock.calls[call][1] as {
          user_message?: unknown;
          edited_history?: { role: string; content: unknown }[];
        };

      it('sends the clip as an audio-only user turn, shows it in the transcript and clears the composer', async () => {
        renderProvider([], { models: transcriptionModels });
        await waitForReady();
        fireEvent.click(screen.getByRole('button', { name: 'pick-whisper' }));
        await sendClip();

        await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
        // The turn IS the clip: one audio_url part, as audio/wav, no text part.
        expect(bodyOf().user_message).toEqual([{ type: 'audio_url', audio_url: { url: clipUrl } }]);
        expect(startSettings().kind).toBe('transcription');
        expect(screen.getByTestId('count').textContent).toBe('1');
        expect(screen.getByTestId('last-parts').textContent).toBe(`audio_url:${clipUrl}`);
        // Sent: the clip is no longer waiting in the composer.
        expect(screen.getByTestId('composer-audio').textContent).toBe('');
        expect(screen.getByTestId('chat-kind').textContent).toBe('transcription');
      });

      it('takes the clip of a recording through the same path, converted to audio/wav', async () => {
        renderProvider([], { models: transcriptionModels });
        await waitForReady();
        fireEvent.click(screen.getByRole('button', { name: 'pick-whisper' }));
        fireEvent.click(screen.getByRole('button', { name: 'attach-recording' }));
        await waitFor(() => expect(screen.getByTestId('composer-audio').textContent).not.toBe(''));

        // A recording has no file name; the composer labels it itself. The recorder's own
        // audio/webm;codecs=opus is what the converter was given, and not what is attached.
        expect(screen.getByTestId('composer-audio-name').textContent).toBe('');
        expect(toWav.mock.calls[0][0].type).toBe('audio/webm;codecs=opus');
        expect(screen.getByTestId('composer-audio').textContent).toBe(wavUrl('recorded'));
        fireEvent.click(screen.getByRole('button', { name: 'send' }));
        await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
        expect(bodyOf().user_message).toEqual([
          { type: 'audio_url', audio_url: { url: wavUrl('recorded') } },
        ]);
      });

      it('refuses a recording the browser cannot convert, attaching nothing', async () => {
        toWav.mockRejectedValueOnce(new AudioDecodeError('unsupported'));
        renderProvider([], { models: transcriptionModels });
        await waitForReady();
        fireEvent.click(screen.getByRole('button', { name: 'pick-whisper' }));
        fireEvent.click(screen.getByRole('button', { name: 'attach-recording' }));

        expect(await screen.findByText(t.chatAudioErrorDecode)).toBeTruthy();
        expect(screen.getByTestId('composer-audio').textContent).toBe('');
      });

      it('names a picked file by its own name, and attaches it as the audio/wav it was converted to', async () => {
        renderProvider([], { models: transcriptionModels });
        await waitForReady();
        fireEvent.click(screen.getByRole('button', { name: 'pick-whisper' }));
        await attachClip(new File(['x'], 'standup.m4a', { type: 'audio/x-m4a' }));

        expect(screen.getByTestId('composer-audio-name').textContent).toBe('standup.m4a');
        expect(screen.getByTestId('composer-audio').textContent).toBe(wavUrl('x'));
      });

      // The payload is whatever follows the LAST comma of the reader's data url: base64 has none,
      // but a media-type parameter may. (The converted blob is audio/wav, so a stand-in that
      // reports a parameterised type is what reaches the reader here.)
      it('reads the payload of a converted clip whose reported type contains a comma', async () => {
        toWav.mockResolvedValueOnce(new Blob(['pcm'], { type: 'audio/wav;note=a,b' }));
        renderProvider([], { models: transcriptionModels });
        await waitForReady();
        fireEvent.click(screen.getByRole('button', { name: 'pick-whisper' }));
        await attachClip(new File(['x'], 'clip.wav', { type: 'audio/wav' }));

        expect(screen.getByTestId('composer-audio').textContent).toBe(
          `data:audio/wav;base64,${btoa('pcm')}`,
        );
      });

      // Not audio the decoder can read: said, and nothing attached.
      it('refuses a file the browser cannot decode, attaching nothing', async () => {
        toWav.mockRejectedValueOnce(new AudioDecodeError('corrupt'));
        renderProvider([], { models: transcriptionModels });
        await waitForReady();
        fireEvent.click(screen.getByRole('button', { name: 'pick-whisper' }));
        fireEvent.change(screen.getByLabelText('probe-audio-file'), {
          target: { files: [new File(['x'], 'broken.mp3', { type: 'audio/mpeg' })] },
        });

        expect(await screen.findByText(t.chatAudioErrorDecode)).toBeTruthy();
        expect(screen.getByTestId('composer-audio').textContent).toBe('');
      });

      it('keeps the clip it had when the next one cannot be decoded', async () => {
        renderProvider([], { models: transcriptionModels });
        await waitForReady();
        fireEvent.click(screen.getByRole('button', { name: 'pick-whisper' }));
        await attachClip();
        toWav.mockRejectedValueOnce(new AudioDecodeError('corrupt'));
        fireEvent.change(screen.getByLabelText('probe-audio-file'), {
          target: { files: [new File(['x'], 'broken.mp3', { type: 'audio/mpeg' })] },
        });

        expect(await screen.findByText(t.chatAudioErrorDecode)).toBeTruthy();
        expect(screen.getByTestId('composer-audio').textContent).toBe(clipUrl);
      });

      // The conversion of a long clip takes a moment (audioToWav), and the composer says so while
      // it runs (audioConverting). The converter stand-in answers at once, so these tests hold a
      // conversion open by hand: `deferConversion` makes the NEXT call to the converter a promise
      // the test settles itself, in the order it chooses.
      describe('converting a clip', () => {
        const deferConversion = () => {
          let resolve!: (wav: Blob) => void;
          let reject!: (err: unknown) => void;
          const pending = new Promise<Blob>((res, rej) => {
            resolve = res;
            reject = rej;
          });
          toWav.mockImplementationOnce(() => pending);
          return {
            finish: (text: string) => resolve(new Blob(['WAV:', text], { type: 'audio/wav' })),
            fail: (err: unknown) => reject(err),
          };
        };
        const converting = () => screen.getByTestId('audio-converting').textContent;
        const pickFile = (text: string, name: string) =>
          fireEvent.change(screen.getByLabelText('probe-audio-file'), {
            target: { files: [new File([text], name, { type: 'audio/wav' })] },
          });
        // Lets everything a settled conversion still has to do (the data url is read from the
        // WAV by a FileReader, a macrotask) run, so a result that WOULD land has landed.
        const settle = () =>
          act(async () => {
            await new Promise((resolve) => setTimeout(resolve, 50));
          });
        const start = async () => {
          renderProvider([], { models: transcriptionModels });
          await waitForReady();
          fireEvent.click(screen.getByRole('button', { name: 'pick-whisper' }));
        };

        it('is not converting until a clip is attached', async () => {
          await start();
          expect(converting()).toBe('false');
        });

        it('is converting from the moment a file is picked until its clip lands', async () => {
          const conversion = deferConversion();
          await start();
          pickFile('long', 'long.wav');

          expect(converting()).toBe('true');
          expect(screen.getByTestId('composer-audio').textContent).toBe('');

          conversion.finish('long');
          await waitFor(() =>
            expect(screen.getByTestId('composer-audio').textContent).not.toBe(''),
          );
          // The clip and the end of the conversion arrive together: never a clip that is still
          // reported as converting, nor a converted flag with no clip yet.
          expect(converting()).toBe('false');
          expect(screen.getByTestId('composer-audio').textContent).toBe(wavUrl('long'));
        });

        it('is converting while a recording is converted, by the same path', async () => {
          const conversion = deferConversion();
          await start();
          fireEvent.click(screen.getByRole('button', { name: 'attach-recording' }));

          expect(converting()).toBe('true');
          conversion.finish('recorded');
          await waitFor(() => expect(converting()).toBe('false'));
          expect(screen.getByTestId('composer-audio').textContent).toBe(wavUrl('recorded'));
        });

        it('stops converting when the conversion fails, and says why', async () => {
          const conversion = deferConversion();
          await start();
          pickFile('x', 'broken.mp3');
          expect(converting()).toBe('true');

          conversion.fail(new AudioDecodeError('corrupt'));

          expect(await screen.findByText(t.chatAudioErrorDecode)).toBeTruthy();
          expect(converting()).toBe('false');
          expect(screen.getByTestId('composer-audio').textContent).toBe('');
        });

        it('stops converting when the clip is refused before it is converted', async () => {
          await start();
          fireEvent.change(screen.getByLabelText('probe-audio-file'), {
            target: { files: [new File(['x'], 'cat.png', { type: 'image/png' })] },
          });

          expect(await screen.findByText(t.chatAudioErrorType)).toBeTruthy();
          expect(converting()).toBe('false');
        });

        // Attach A (long) and then B (short): B lands first, and A -- which resolves last -- must
        // not overwrite it. The clip is the one attached LAST, whatever order they finish in.
        it('discards a superseded conversion that finishes after the newer one', async () => {
          const first = deferConversion();
          await start();
          pickFile('A', 'a.wav');
          pickFile('B', 'b.wav');

          // B (the default stand-in, immediate) lands; A is still being converted.
          await waitFor(() =>
            expect(screen.getByTestId('composer-audio').textContent).not.toBe(''),
          );
          expect(screen.getByTestId('composer-audio-name').textContent).toBe('b.wav');
          expect(converting()).toBe('false');

          first.finish('A');
          await settle();

          expect(screen.getByTestId('composer-audio-name').textContent).toBe('b.wav');
          expect(screen.getByTestId('composer-audio').textContent).toBe(wavUrl('B'));
          expect(converting()).toBe('false');
        });

        it('keeps converting until the newest attach lands, whichever finishes first', async () => {
          const first = deferConversion();
          const second = deferConversion();
          await start();
          pickFile('A', 'a.wav');
          pickFile('B', 'b.wav');

          // A, the older, finishes first: it is superseded, so it neither lands nor ends the
          // conversion of B, which is still in flight.
          first.finish('A');
          await settle();
          expect(screen.getByTestId('composer-audio').textContent).toBe('');
          expect(converting()).toBe('true');

          second.finish('B');
          await waitFor(() =>
            expect(screen.getByTestId('composer-audio').textContent).toBe(wavUrl('B')),
          );
          expect(converting()).toBe('false');
        });

        it('says nothing of a superseded conversion that fails', async () => {
          const first = deferConversion();
          await start();
          pickFile('A', 'a.wav');
          pickFile('B', 'b.wav');
          await waitFor(() =>
            expect(screen.getByTestId('composer-audio').textContent).not.toBe(''),
          );

          first.fail(new AudioDecodeError('corrupt'));
          await settle();

          expect(screen.queryByText(t.chatAudioErrorDecode)).toBeNull();
          expect(screen.getByTestId('composer-audio-name').textContent).toBe('b.wav');
          expect(converting()).toBe('false');
        });

        it('does not reattach a clip that was removed while it was being converted', async () => {
          const conversion = deferConversion();
          await start();
          pickFile('A', 'a.wav');
          expect(converting()).toBe('true');

          fireEvent.click(screen.getByRole('button', { name: 'remove-audio' }));
          // Removing the clip abandons the attach in flight, so nothing is converting any more.
          expect(converting()).toBe('false');

          conversion.finish('A');
          await settle();

          expect(screen.getByTestId('composer-audio').textContent).toBe('');
          expect(converting()).toBe('false');
        });

        it('does not reattach a clip once the turn it was replacing has been sent', async () => {
          await start();
          await attachClip();
          const conversion = deferConversion();
          pickFile('late', 'late.wav');
          expect(converting()).toBe('true');

          // The clip that was in the composer goes out with a send made mid-conversion.
          fireEvent.click(screen.getByRole('button', { name: 'send' }));
          await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
          expect(bodyOf().user_message).toEqual([
            { type: 'audio_url', audio_url: { url: clipUrl } },
          ]);
          expect(screen.getByTestId('composer-audio').textContent).toBe('');
          expect(converting()).toBe('false');

          conversion.finish('late');
          await settle();

          // The stale result must not turn up in the composer after the send emptied it.
          expect(screen.getByTestId('composer-audio').textContent).toBe('');
          expect(converting()).toBe('false');
        });

        it('lands nowhere when the thread stopped being a transcription one, and stops converting', async () => {
          const conversion = deferConversion();
          renderProvider([], { models: [models[0], whisper] });
          await waitForReady();
          fireEvent.click(screen.getByRole('button', { name: 'pick-whisper' }));
          pickFile('A', 'a.wav');
          expect(converting()).toBe('true');

          fireEvent.click(screen.getByRole('button', { name: `pick-${models[0].id}` }));
          conversion.finish('A');
          await settle();

          expect(screen.getByTestId('composer-audio').textContent).toBe('');
          expect(converting()).toBe('false');
        });
      });

      it('removes the clip, and sending then has nothing to send', async () => {
        renderProvider([], { models: transcriptionModels });
        await waitForReady();
        fireEvent.click(screen.getByRole('button', { name: 'pick-whisper' }));
        await attachClip();

        fireEvent.click(screen.getByRole('button', { name: 'remove-audio' }));
        expect(screen.getByTestId('composer-audio').textContent).toBe('');

        fireEvent.click(screen.getByRole('button', { name: 'send' }));
        expect(await screen.findByText(t.chatTranscriptionNeedsAudio)).toBeTruthy();
        expect(chatApi.spies.startChatRun).not.toHaveBeenCalled();
      });

      // A transcription turn has no text to send: typed text cannot stand in for the clip, and
      // is not lost when the clip goes.
      it('refuses a send with no clip, whatever text was typed', async () => {
        renderProvider([], { models: transcriptionModels });
        await waitForReady();
        fireEvent.click(screen.getByRole('button', { name: 'pick-whisper' }));
        sendText('please transcribe this');

        expect(await screen.findByText(t.chatTranscriptionNeedsAudio)).toBeTruthy();
        expect(chatApi.spies.startChatRun).not.toHaveBeenCalled();
        expect(screen.getByTestId('count').textContent).toBe('0');
      });

      it('sends the clip alone: text typed before the model was picked is neither in the turn nor its title', async () => {
        renderProvider([], { models: [...models, whisper] });
        await waitForReady();
        fireEvent.change(screen.getByLabelText('probe-input'), {
          target: { value: 'typed first' },
        });
        fireEvent.click(screen.getByRole('button', { name: 'pick-whisper' }));
        await sendClip();

        await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
        expect(bodyOf().user_message).toEqual([{ type: 'audio_url', audio_url: { url: clipUrl } }]);
        // The text would have named the chat (maybeAutoTitle), had the turn been its text.
        expect(screen.getByTestId('chats').textContent).not.toContain('typed first');
        expect(screen.getByTestId('last-parts').textContent).not.toContain('typed first');
      });

      it('rolls the user turn back when the run cannot be started', async () => {
        chatApi.spies.startChatRun.mockRejectedValueOnce(new Error('boom'));
        renderProvider([], { models: transcriptionModels });
        await waitForReady();
        fireEvent.click(screen.getByRole('button', { name: 'pick-whisper' }));
        await sendClip();

        await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
        await waitFor(() => expect(screen.getByTestId('count').textContent).toBe('0'));
      });

      // An image is not what a transcription thread takes, and an audio file is not what a
      // text thread's attach button takes: the thread's kind decides what handleFiles does.
      it('refuses an image on a transcription thread instead of attaching it', async () => {
        renderProvider([], { models: transcriptionModels });
        await waitForReady();
        fireEvent.click(screen.getByRole('button', { name: 'pick-whisper' }));
        fireEvent.change(screen.getByLabelText('probe-audio-file'), {
          target: { files: [new File(['x'], 'cat.png', { type: 'image/png' })] },
        });

        expect(await screen.findByText(t.chatAudioErrorType)).toBeTruthy();
        expect(screen.getByTestId('composer-images').textContent).toBe('');
        expect(screen.getByTestId('composer-audio').textContent).toBe('');
      });

      it('keeps an audio file out of a text thread', async () => {
        renderProvider([], { models: [models[0], whisper] });
        await waitForReady();
        fireEvent.click(screen.getByRole('button', { name: `pick-${models[0].id}` }));
        fireEvent.change(screen.getByLabelText('probe-audio-file'), {
          target: { files: [new File(['x'], 'clip.wav', { type: 'audio/wav' })] },
        });

        // Not an image, so the image path says so as it always did; no clip appears.
        expect(await screen.findByText(t.chatImageErrorType)).toBeTruthy();
        expect(screen.getByTestId('composer-audio').textContent).toBe('');
      });

      it('drops a clip when the thread stops being a transcription one', async () => {
        renderProvider([], { models: [models[0], whisper] });
        await waitForReady();
        fireEvent.click(screen.getByRole('button', { name: 'pick-whisper' }));
        await attachClip();

        fireEvent.click(screen.getByRole('button', { name: `pick-${models[0].id}` }));

        await waitFor(() => expect(screen.getByTestId('composer-audio').textContent).toBe(''));
      });

      it('keeps a clip across a switch to another transcription model', async () => {
        const other: ModelOption = { ...whisper, id: 'whisper-2', display_name: 'whisper-2' };
        renderProvider([], { models: [whisper, other] });
        await waitForReady();
        fireEvent.click(screen.getByRole('button', { name: 'pick-whisper' }));
        await attachClip();

        fireEvent.click(screen.getByRole('button', { name: 'pick-whisper-2' }));

        expect(screen.getByTestId('chat-kind').textContent).toBe('transcription');
        expect(screen.getByTestId('composer-audio').textContent).toBe(clipUrl);
      });
    });

    // A replay sends the thread's last user turn again, and what it transcribes is that turn's
    // clip: a transcription turn without one has nothing to transcribe, and the endpoint would
    // answer only after the transcript was truncated and the turn stored.
    describe('replaying a turn that has no audio', () => {
      const withTurns = (messages: unknown[]): ChatRow[] => [
        {
          id: 'c1',
          title: 'C1',
          created_at: T,
          updated_at: T,
          content: {
            settings: { model: whisper.id, kind: 'transcription' },
            messages,
          },
        },
      ];
      const audioless = [
        { id: 'u1', role: 'user', content: 'a stray text turn' },
        { id: 'a1', role: 'assistant', content: 'hello there', status: 'complete' },
      ];

      it('blocks a regenerate, says why and leaves the transcript alone', async () => {
        chatApi = makeChatApi(withTurns(audioless));
        renderProvider([], { models: transcriptionModels });
        await waitForReady();

        fireEvent.click(screen.getByRole('button', { name: 'regenerate-a1' }));

        expect(await screen.findByText(t.chatTranscriptionReplayNoAudio)).toBeTruthy();
        expect(chatApi.spies.startChatRun).not.toHaveBeenCalled();
        expect(screen.getByTestId('count').textContent).toBe('2');
      });

      it('blocks an edit of such a turn too: its text is not what is transcribed', async () => {
        chatApi = makeChatApi(withTurns(audioless));
        renderProvider([], { models: transcriptionModels });
        await waitForReady();

        fireEvent.click(screen.getByRole('button', { name: 'edit-u1' }));

        expect(await screen.findByText(t.chatTranscriptionReplayNoAudio)).toBeTruthy();
        expect(chatApi.spies.startChatRun).not.toHaveBeenCalled();
        expect(screen.getByTestId('count').textContent).toBe('2');
      });

      it('judges the history that is replayed, not the whole transcript', async () => {
        chatApi = makeChatApi(
          withTurns([
            ...audioless,
            { id: 'u2', role: 'user', content: clip },
            { id: 'a2', role: 'assistant', content: 'second take', status: 'complete' },
          ]),
        );
        renderProvider([], { models: transcriptionModels });
        await waitForReady();

        // The later turn has its clip, but replaying the FIRST turn drops it: blocked.
        fireEvent.click(screen.getByRole('button', { name: 'regenerate-a1' }));
        expect(await screen.findByText(t.chatTranscriptionReplayNoAudio)).toBeTruthy();
        expect(chatApi.spies.startChatRun).not.toHaveBeenCalled();
        expect(screen.getByTestId('count').textContent).toBe('4');

        // Replaying the second turn keeps its clip in the history: allowed.
        fireEvent.click(screen.getByRole('button', { name: 'regenerate-a2' }));
        await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
        const body = chatApi.spies.startChatRun.mock.calls[0][1] as {
          edited_history: { id: string }[];
        };
        expect(body.edited_history.map((m) => m.id)).toEqual(['u1', 'a1', 'u2']);
      });

      it('lets a turn with a stored clip through: a committed turn holds a blob reference', async () => {
        const stored = [
          {
            type: 'audio_url',
            audio_url: { url: '/api/portal/chat-blobs/blob_0123456789abcdef0123456789abcdef' },
          },
        ];
        chatApi = makeChatApi(
          withTurns([
            { id: 'u1', role: 'user', content: stored },
            { id: 'a1', role: 'assistant', content: 'hello there', status: 'complete' },
          ]),
        );
        renderProvider([], { models: transcriptionModels });
        await waitForReady();

        fireEvent.click(screen.getByRole('button', { name: 'regenerate-a1' }));

        await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
        expect(screen.queryByText(t.chatTranscriptionReplayNoAudio)).toBeNull();
      });

      it('is a transcription rule: a text thread replays its text turn as ever', async () => {
        chatApi = makeChatApi([
          {
            id: 'c1',
            title: 'C1',
            created_at: T,
            updated_at: T,
            content: { settings: { model: 'gpt-oss-20b' }, messages: audioless },
          },
        ]);
        renderProvider([], { models });
        await waitForReady();

        fireEvent.click(screen.getByRole('button', { name: 'regenerate-a1' }));

        await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
        expect(screen.queryByText(t.chatTranscriptionReplayNoAudio)).toBeNull();
      });
    });

    it('regenerates a transcription turn without the vision guard: an audio input is not an image', async () => {
      chatApi = makeChatApi(transcriptionThread());
      renderProvider([], { models: transcriptionModels });
      await waitForReady();
      expect(screen.getByTestId('chat-kind').textContent).toBe('transcription');

      fireEvent.click(screen.getByRole('button', { name: 'regenerate-a1' }));

      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
      expect(screen.queryByText(t.chatImageModelUnsupported)).toBeNull();
      expect(startSettings().kind).toBe('transcription');
      // The replayed history still carries the clip: it is what the run transcribes.
      const body = chatApi.spies.startChatRun.mock.calls[0][1] as {
        edited_history: { role: string; content: unknown }[];
      };
      expect(body.edited_history).toEqual([
        expect.objectContaining({ role: 'user', content: clip }),
      ]);
    });

    it('keeps the clip when the text of a transcription turn is edited', async () => {
      chatApi = makeChatApi(transcriptionThread());
      renderProvider([], { models: transcriptionModels });
      await waitForReady();

      fireEvent.click(screen.getByRole('button', { name: 'edit-u1' }));

      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
      const body = chatApi.spies.startChatRun.mock.calls[0][1] as {
        edited_history: { role: string; content: unknown }[];
      };
      // The edited turn still holds the audio the run reads; losing it would leave the run
      // with nothing to transcribe.
      expect(body.edited_history).toHaveLength(1);
      expect(body.edited_history[0].content).toEqual([
        { type: 'text', text: 'edited text' },
        ...clip,
      ]);
    });

    it('persists the transcription settings with the thread and seeds them back on reload', async () => {
      chatApi = makeChatApi(transcriptionThread({ language: 'en', temperature: 0.8 }));
      const { unmount } = renderProvider([], { models: transcriptionModels });
      await waitForReady();
      expect(screen.getByTestId('chat-kind').textContent).toBe('transcription');
      // Seeded from the loaded document, temperature shown as the blank-able string.
      expect(JSON.parse(screen.getByTestId('transcription-settings').textContent!)).toEqual({
        language: 'en',
        prompt: '',
        temperature: '0.8',
        passthrough: '',
      });

      // The transcription settings are the ONLY thing that changes here, so this also holds
      // the debounced save's dependency list: without transcriptionSettings in it, no save
      // is ever scheduled for a settings-only edit.
      fireEvent.click(screen.getByRole('button', { name: 'set-transcription' }));
      await waitFor(() => expect(chatApi.spies.saveChat).toHaveBeenCalled(), { timeout: 3000 });
      const saved = chatApi.spies.saveChat.mock.calls.at(-1)![1] as {
        content: { settings: { kind?: string; transcription?: unknown } };
      };
      expect(saved.content.settings.kind).toBe('transcription');
      expect(saved.content.settings.transcription).toEqual({
        language: 'de',
        prompt: 'Namen: Zoe',
        temperature: 0,
        passthrough: { beam: 5 },
      });

      // ...and the reload reads them back out of the saved document (the PUT full-replaces
      // the blob, so a setting not seeded here is erased next save).
      unmount();
      renderProvider([], { models: transcriptionModels });
      await waitForReady();
      expect(JSON.parse(screen.getByTestId('transcription-settings').textContent!)).toMatchObject({
        language: 'de',
        prompt: 'Namen: Zoe',
        temperature: '0',
      });
    });

    it('keeps the stored transcription settings when an unrelated setting is saved', async () => {
      chatApi = makeChatApi(
        transcriptionThread({ language: 'en', temperature: 0, passthrough: { a: 1 } }),
      );
      renderProvider([], { models: transcriptionModels });
      await waitForReady();

      fireEvent.click(screen.getByRole('button', { name: 'set-system' }));
      await waitFor(() => expect(chatApi.spies.saveChat).toHaveBeenCalled(), { timeout: 3000 });
      const saved = chatApi.spies.saveChat.mock.calls.at(-1)![1] as {
        content: { settings: { transcription?: unknown } };
      };
      expect(saved.content.settings.transcription).toEqual({
        language: 'en',
        temperature: 0,
        passthrough: { a: 1 },
      });
    });

    it.each([
      ['text', undefined],
      ['image', 'image'],
      ['speech', 'speech'],
    ] as const)(
      'writes no transcription key when saving a %s thread, whatever the transcription state holds',
      async (_label, kind) => {
        chatApi = makeChatApi([
          {
            id: 'c1',
            title: 'C1',
            created_at: T,
            updated_at: T,
            content: {
              settings: { model: models[0].id, ...(kind ? { kind } : {}) },
              messages: [
                { id: 'u1', role: 'user', content: 'hello' },
                { id: 'a1', role: 'assistant', content: 'hi', status: 'complete' },
              ],
            },
          },
        ]);
        renderProvider();
        await waitForReady();

        // Transcription state moved on a thread that is not a transcription one: it must not
        // persist.
        fireEvent.click(screen.getByRole('button', { name: 'set-transcription' }));
        fireEvent.click(screen.getByRole('button', { name: 'set-system' }));
        await waitFor(() => expect(chatApi.spies.saveChat).toHaveBeenCalled(), { timeout: 3000 });
        const saved = chatApi.spies.saveChat.mock.calls.at(-1)![1] as {
          content: { settings: Record<string, unknown> };
        };
        expect(Object.keys(saved.content.settings)).not.toContain('transcription');
        if (!kind) expect(Object.keys(saved.content.settings)).not.toContain('kind');
      },
    );

    // Fork 4: the first user turn is audio only, so the usual title (the first user message's
    // text) is empty. A completed transcription run names an untitled chat after its transcript.
    //
    // The runs below are REPLAYS (regenerate) of a loaded audio-only turn: it is the one way to
    // start a run from an audio-only turn before the composer can attach a clip (a typed text
    // would itself title the chat as it is sent), and a replay is also a real way for a chat to
    // reach its first transcript -- the first run failed, the user tries again.
    describe('titling from the transcript', () => {
      const untitled = (title = ''): ChatRow[] => transcriptionThread(undefined, { title });
      const regenerateAndFinish = async (done: Record<string, unknown>) => {
        fireEvent.click(screen.getByRole('button', { name: 'regenerate-a1' }));
        await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
        await act(async () => {
          FakeEventSource.instances[0].emit('done', done);
        });
        await waitFor(() => expect(screen.getByTestId('streaming').textContent).toBe('false'));
      };
      const transcriptDone = (text: string, extra: Record<string, unknown> = {}) => ({
        status: 'completed',
        kind: 'transcription',
        content_parts: [{ type: 'text', text }],
        ...extra,
      });
      // The saves that carry a title and nothing else (no `content` key): the title-only PUT
      // a rename makes, as against the autosave of the whole document.
      const titleOnlySaves = () =>
        chatApi.spies.saveChat.mock.calls.filter(([, body]) => !('content' in body));

      it('titles an untitled chat from the transcript, shows it and persists it', async () => {
        chatApi = makeChatApi(untitled());
        renderProvider([], { models: transcriptionModels });
        await waitForReady();
        expect(screen.getByTestId('chats').textContent).toBe('c1:');

        await regenerateAndFinish(
          transcriptDone('Good morning, this is a note about the quarterly planning meeting'),
        );

        // deriveTitle's 40-character cut, as it falls.
        const title = 'Good morning, this is a note about the q';
        await waitFor(() => expect(screen.getByTestId('chats').textContent).toBe(`c1:${title}`));
        // Persisted by a title-only save, so it holds with no later autosave and for a chat that
        // is not the active one.
        await waitFor(() => expect(chatApi.spies.saveChat).toHaveBeenCalledWith('c1', { title }));
      });

      it('leaves a chat that already has a title alone', async () => {
        chatApi = makeChatApi(untitled('My memo'));
        renderProvider([], { models: transcriptionModels });
        await waitForReady();

        await regenerateAndFinish(transcriptDone('Something else entirely'));

        expect(screen.getByTestId('chats').textContent).toBe('c1:My memo');
        expect(titleOnlySaves()).toEqual([]);
      });

      it('treats the new-chat placeholder as no title yet', async () => {
        chatApi = makeChatApi(untitled(t.chatNewChat));
        renderProvider([], { models: transcriptionModels });
        await waitForReady();
        expect(screen.getByTestId('chats').textContent).toBe(`c1:${t.chatNewChat}`);

        await regenerateAndFinish(transcriptDone('Hello world'));

        await waitFor(() => expect(screen.getByTestId('chats').textContent).toBe('c1:Hello world'));
      });

      it('leaves the chat untitled when the transcript is empty', async () => {
        chatApi = makeChatApi(untitled());
        renderProvider([], { models: transcriptionModels });
        await waitForReady();

        await regenerateAndFinish(transcriptDone('   \n '));

        expect(screen.getByTestId('chats').textContent).toBe('c1:');
        expect(titleOnlySaves()).toEqual([]);
      });

      it.each([
        ['error', { status: 'error', error: 'gateway.chat_run_timeout' }],
        ['canceled', { status: 'canceled' }],
      ] as const)('does not title from a run that ended %s', async (_status, outcome) => {
        chatApi = makeChatApi(untitled());
        renderProvider([], { models: transcriptionModels });
        await waitForReady();

        await regenerateAndFinish({
          kind: 'transcription',
          content_parts: [{ type: 'text', text: 'partial words' }],
          ...outcome,
        });

        expect(screen.getByTestId('chats').textContent).toBe('c1:');
        expect(titleOnlySaves()).toEqual([]);
      });

      // The title comes from the transcript only for a transcription run: a text or speech
      // reply is not a transcript, and those chats are named by the first message as before.
      it.each([
        ['a text run', '', 'Hello from the model'],
        ['a speech run', 'speech', 'Say this please'],
      ] as const)('does not title %s from its reply', async (_label, kind, reply) => {
        chatApi = makeChatApi([
          {
            id: 'c1',
            title: '',
            created_at: T,
            updated_at: T,
            content: {
              settings: { model: kokoro.id, ...(kind ? { kind } : {}) },
              messages: [
                { id: 'u1', role: 'user', content: 'say hi' },
                { id: 'a1', role: 'assistant', content: 'hi', status: 'complete' },
              ],
            },
          },
        ]);
        renderProvider([], { models: [kokoro] });
        await waitForReady();

        await regenerateAndFinish({
          status: 'completed',
          kind,
          content_parts: [{ type: 'text', text: reply }],
        });

        expect(screen.getByTestId('chats').textContent).toBe('c1:');
        expect(titleOnlySaves()).toEqual([]);
      });
    });
  });

  // The transcription composer's live gate (#266): a live recording is offered only where the
  // gateway says the picked transcription model's mapping streams a transcription. The speech
  // gate's twin, minus the rate: closed until an answer says yes, closed again at once when the
  // model changes, fail closed on a failed lookup, and never asked of a model that does not
  // transcribe.
  describe(`ChatStoreProvider transcription streaming gate [${locale}]`, () => {
    const asrModel = (id: string): ModelOption => ({
      id,
      display_name: id,
      flavors: ['openai_audio'],
      loading_on_count: 0,
      transcription: true,
    });
    const whisper = asrModel('whisper');
    const parakeet = asrModel('parakeet');
    const kokoro: ModelOption = {
      id: 'kokoro',
      display_name: 'kokoro',
      flavors: ['openai_audio'],
      loading_on_count: 0,
      speech: true,
    };
    const flag = () => screen.getByTestId('transcription-streaming').textContent;
    const transcriptionThread = (): ChatRow[] => [
      {
        id: 'c1',
        title: 'C1',
        created_at: T,
        updated_at: T,
        content: { settings: { model: whisper.id, kind: 'transcription' }, messages: [] },
      },
    ];

    it('asks whether the picked transcription model may go live, and offers it only on a yes', async () => {
      chatApi = makeChatApi(transcriptionThread());
      chatApi.spies.modelTranscriptionStreaming.mockResolvedValue(true);
      renderProvider([], { models: [whisper] });
      await waitForReady();
      await waitFor(() => expect(flag()).toBe('true'));
      expect(chatApi.spies.modelTranscriptionStreaming).toHaveBeenCalledWith('whisper');
    });

    it('offers no live mode while the gate is closed, and not before the answer arrives', async () => {
      chatApi = makeChatApi(transcriptionThread());
      let answer: (streams: boolean) => void = () => {};
      chatApi.spies.modelTranscriptionStreaming.mockImplementation(
        () => new Promise<boolean>((resolve) => (answer = resolve)),
      );
      renderProvider([], { models: [whisper] });
      await waitForReady();
      await waitFor(() => expect(chatApi.spies.modelTranscriptionStreaming).toHaveBeenCalled());
      expect(flag()).toBe('false');
      await act(async () => answer(false));
      expect(flag()).toBe('false');
    });

    it('fails closed when the lookup fails', async () => {
      chatApi = makeChatApi(transcriptionThread());
      chatApi.spies.modelTranscriptionStreaming.mockRejectedValue(new Error('boom'));
      renderProvider([], { models: [whisper] });
      await waitForReady();
      await waitFor(() => expect(chatApi.spies.modelTranscriptionStreaming).toHaveBeenCalled());
      expect(flag()).toBe('false');
    });

    it('follows the picked model: a yes for one is dropped at once when another is picked', async () => {
      chatApi = makeChatApi(transcriptionThread());
      chatApi.spies.modelTranscriptionStreaming.mockImplementation(
        async (name: string) => name === 'whisper',
      );
      renderProvider([], { models: [whisper, parakeet] });
      await waitForReady();
      await waitFor(() => expect(flag()).toBe('true'));

      fireEvent.click(screen.getByRole('button', { name: 'pick-parakeet' }));
      await waitFor(() =>
        expect(chatApi.spies.modelTranscriptionStreaming).toHaveBeenLastCalledWith('parakeet'),
      );
      expect(flag()).toBe('false');

      // And back: the first model's gate is asked again and reopens.
      fireEvent.click(screen.getByRole('button', { name: 'pick-whisper' }));
      await waitFor(() => expect(flag()).toBe('true'));
      expect(chatApi.spies.modelTranscriptionStreaming).toHaveBeenLastCalledWith('whisper');
    });

    it('lets the latest pick win over a slow answer for the previous model', async () => {
      chatApi = makeChatApi(transcriptionThread());
      const answers = new Map<string, (streams: boolean) => void>();
      chatApi.spies.modelTranscriptionStreaming.mockImplementation(
        (name: string) => new Promise<boolean>((resolve) => answers.set(name, resolve)),
      );
      renderProvider([], { models: [whisper, parakeet] });
      await waitForReady();
      await waitFor(() => expect(answers.has('whisper')).toBe(true));

      fireEvent.click(screen.getByRole('button', { name: 'pick-parakeet' }));
      await waitFor(() => expect(answers.has('parakeet')).toBe(true));
      // The first model's slow yes lands after the switch: it belongs to a model no longer
      // picked and must not open the new model's gate.
      await act(async () => answers.get('whisper')?.(true));
      expect(flag()).toBe('false');
      await act(async () => answers.get('parakeet')?.(false));
      expect(flag()).toBe('false');
    });

    it('does not ask for a model that does not transcribe, and keeps the gate closed', async () => {
      renderProvider([], { models: [...models, kokoro] });
      await waitForReady();
      expect(chatApi.spies.modelTranscriptionStreaming).not.toHaveBeenCalled();
      expect(flag()).toBe('false');

      fireEvent.click(screen.getByRole('button', { name: 'pick-kokoro' }));
      expect(chatApi.spies.modelTranscriptionStreaming).not.toHaveBeenCalled();
      expect(flag()).toBe('false');
    });

    it('asks nothing of the speech gate for a transcription model', async () => {
      chatApi = makeChatApi(transcriptionThread());
      chatApi.spies.modelTranscriptionStreaming.mockResolvedValue(true);
      renderProvider([], { models: [whisper] });
      await waitForReady();
      await waitFor(() => expect(flag()).toBe('true'));
      expect(chatApi.spies.modelSpeechStreaming).not.toHaveBeenCalled();
      expect(screen.getByTestId('speech-streaming').textContent).toBe('false');
    });
  });

  // The live recording of a transcription thread (#266), as the store wires it into the chat:
  // the session is made for the thread's model and language, through the provider's api; the
  // finished transcript is committed as one completed turn WITHOUT a chat run (the gateway's live
  // endpoint did, and billed, the work); and the recording does not outlive the thread it started
  // in. How a session's states and its reconnects are handled is useLiveTranscription.test.tsx's.
  describe(`ChatStoreProvider live transcription [${locale}]`, () => {
    const whisper: ModelOption = {
      id: 'whisper',
      display_name: 'whisper',
      flavors: ['openai_audio'],
      loading_on_count: 0,
      transcription: true,
    };
    const liveThread = (
      overrides: Partial<ChatRow> = {},
      settings: Record<string, unknown> = {},
    ): ChatRow => ({
      id: 'c1',
      title: '',
      created_at: T,
      updated_at: T,
      content: { settings: { model: 'whisper', kind: 'transcription', ...settings }, messages: [] },
      ...overrides,
    });

    type FakeLive = { args: LiveSessionArgs; stop: Mock<() => void>; abort: Mock<() => void> };
    let sessions: FakeLive[];

    beforeEach(() => {
      sessions = [];
      vi.mocked(createLiveSession).mockImplementation((args) => {
        const session: FakeLive = {
          args,
          stop: vi.fn(() => args.onState('finalizing')),
          abort: vi.fn(),
        };
        sessions.push(session);
        args.onState('connecting');
        return session;
      });
    });
    afterEach(() => vi.mocked(createLiveSession).mockReset());

    const state = () => screen.getByTestId('live-state').textContent;
    const transcript = () => screen.getByTestId('live-transcript').textContent;
    const click = (name: string) => fireEvent.click(screen.getByRole('button', { name }));
    const storedMessages = () =>
      JSON.parse(screen.getByTestId('messages-json').textContent ?? '[]') as ChatUiMessage[];

    // A transcription thread whose model may go live, open and ready.
    async function renderLive(rows: ChatRow[] = [liveThread()]) {
      chatApi = makeChatApi(rows);
      chatApi.spies.modelTranscriptionStreaming.mockResolvedValue(true);
      const view = renderProvider([], { models: [whisper] });
      await waitForReady();
      await waitFor(() =>
        expect(screen.getByTestId('transcription-streaming').textContent).toBe('true'),
      );
      return view;
    }

    // One short recording: started, listening, a few words said, then stopped and delivered.
    async function record(words: string, final = words) {
      click('live-start');
      act(() => sessions.at(-1)!.args.onState('listening'));
      act(() => sessions.at(-1)!.args.onDelta(words));
      click('live-stop');
      await act(async () => {
        sessions.at(-1)!.args.onDone(final);
        sessions.at(-1)!.args.onState('closed');
      });
    }

    describe('starting', () => {
      it('makes a session for the thread: its model, its language (trimmed), the provider api', async () => {
        await renderLive([liveThread({}, { transcription: { language: ' de ' } })]);

        click('live-start');

        expect(sessions).toHaveLength(1);
        expect(sessions[0].args.model).toBe('whisper');
        expect(sessions[0].args.language).toBe('de');
        expect(sessions[0].args.api).toBe(chatApi.api);
        expect(state()).toBe('connecting');
      });

      it('starts without a language when the thread has none (auto-detect)', async () => {
        await renderLive();
        click('live-start');
        expect(sessions[0].args.language).toBeUndefined();
      });

      it('does not start where live is not on offer (the gate is closed)', async () => {
        chatApi = makeChatApi([liveThread()]);
        renderProvider([], { models: [whisper] });
        await waitForReady();
        await waitFor(() => expect(chatApi.spies.modelTranscriptionStreaming).toHaveBeenCalled());

        click('live-start');

        expect(sessions).toHaveLength(0);
        expect(state()).toBe('idle');
      });

      it('does not start on a thread that is not a transcription one', async () => {
        chatApi = makeChatApi();
        renderProvider([], { models });
        await waitForReady();

        click('live-start');

        expect(sessions).toHaveLength(0);
        expect(state()).toBe('idle');
      });

      it('does not start while the chat has a run in flight, and says so', async () => {
        chatApi = makeChatApi([liveThread()]);
        chatApi.spies.modelTranscriptionStreaming.mockResolvedValue(true);
        chatApi.spies.activeChatRuns.mockResolvedValue({
          data: [
            {
              chat_id: 'c1',
              run_id: 'r1',
              status: 'running',
              kind: 'transcription',
              elapsed_ms: 0,
            },
          ],
        });
        renderProvider([], { models: [whisper] });
        await waitForReady();
        await waitFor(() =>
          expect(screen.getByTestId('transcription-streaming').textContent).toBe('true'),
        );

        click('live-start');

        expect(sessions).toHaveLength(0);
        expect(await screen.findByText(t.chatBusy)).toBeInTheDocument();
      });
    });

    describe('a finished recording', () => {
      it('is committed as one completed turn: a marker for the recording, and the transcript', async () => {
        await renderLive();
        expect(screen.getByTestId('count').textContent).toBe('0');

        await record('Hello wor', 'Hello world.');

        const turns = storedMessages();
        expect(turns).toHaveLength(2);
        expect(turns[0]).toMatchObject({ role: 'user', content: t.chatTranscriptionLiveTurn });
        expect(turns[1]).toMatchObject({
          role: 'assistant',
          content: 'Hello world.',
          status: 'complete',
        });
        expect(turns[0].id).not.toBe(turns[1].id);
        expect(state()).toBe('idle');
        expect(transcript()).toBe('');
        // The answer is the thread's last text, which is what its title and replays read.
        expect(screen.getByTestId('last').textContent).toBe('Hello world.');
      });

      it('starts NO chat run: the work was done, and billed, by the live endpoint', async () => {
        await renderLive();
        await record('Hello');
        expect(chatApi.spies.startChatRun).not.toHaveBeenCalled();
        expect(FakeEventSource.instances).toHaveLength(0);
        expect(screen.getByTestId('streaming').textContent).toBe('false');
      });

      it('keeps the thread a transcription thread, pinned by the turn', async () => {
        // A thread with no pin yet: it follows the model, until the first turn pins it.
        await renderLive([liveThread({}, { kind: undefined })]);
        expect(screen.getByTestId('chat-kind').textContent).toBe('transcription');

        await record('Hello');

        expect(screen.getByTestId('chat-kind').textContent).toBe('transcription');
        await waitFor(
          () => {
            const saved = chatApi.spies.saveChat.mock.calls.at(-1)?.[1].content as {
              settings: { kind?: string };
              messages: unknown[];
            };
            expect(saved.settings.kind).toBe('transcription');
            expect(saved.messages).toHaveLength(2);
          },
          { timeout: 2500 },
        );
      });

      it('is saved with the chat, like any other turn', async () => {
        await renderLive();
        await record('Save me please');

        await waitFor(
          () => {
            const saved = chatApi.spies.saveChat.mock.calls.at(-1)?.[1].content as {
              messages: { role: string; content: string }[];
            };
            expect(saved.messages.map((m) => m.content)).toEqual([
              t.chatTranscriptionLiveTurn,
              'Save me please',
            ]);
          },
          { timeout: 2500 },
        );
      });

      it('names an untitled chat after what was said', async () => {
        await renderLive();
        await record('Remember to buy oat milk');
        expect(screen.getByTestId('chats').textContent).toBe('c1:Remember to buy oat milk');
      });

      it('leaves a chat that already has a title alone', async () => {
        await renderLive([liveThread({ title: 'Meeting notes' })]);
        await record('Something else entirely');
        expect(screen.getByTestId('chats').textContent).toBe('c1:Meeting notes');
      });

      it('appends to a thread that already has turns', async () => {
        await renderLive([
          liveThread({
            title: 'Meeting notes',
            content: {
              settings: { model: 'whisper', kind: 'transcription' },
              messages: [
                { id: 'u1', role: 'user', content: t.chatTranscriptionLiveTurn },
                { id: 'a1', role: 'assistant', content: 'First recording.', status: 'complete' },
              ],
            },
          }),
        ]);

        await record('Second one');

        expect(storedMessages().map((m) => m.content)).toEqual([
          t.chatTranscriptionLiveTurn,
          'First recording.',
          t.chatTranscriptionLiveTurn,
          'Second one',
        ]);
      });

      it('with no speech commits nothing, and says so', async () => {
        await renderLive();
        click('live-start');
        act(() => sessions[0].args.onState('listening'));
        click('live-stop');
        await act(async () => {
          sessions[0].args.onDone('');
          sessions[0].args.onState('closed');
        });

        expect(screen.getByTestId('count').textContent).toBe('0');
        expect(await screen.findByText(t.errorChatRunNoTranscript)).toBeInTheDocument();
        expect(state()).toBe('idle');
      });
    });

    describe('failures', () => {
      it('an error shows its message, keeps the transcript, and "use" commits it anyway', async () => {
        await renderLive();
        click('live-start');
        act(() => sessions[0].args.onState('listening'));
        act(() => sessions[0].args.onDelta('Said before it broke'));
        act(() => {
          sessions[0].args.onError('upstream error', { kind: 'refused' });
          sessions[0].args.onState('error');
        });

        expect(state()).toBe('error');
        expect(screen.getByTestId('live-error').textContent).toBe(t.chatTranscriptionLiveError);
        expect(transcript()).toBe('Said before it broke');
        expect(screen.getByTestId('count').textContent).toBe('0');

        click('live-keep');

        expect(state()).toBe('idle');
        expect(storedMessages().map((m) => m.content)).toEqual([
          t.chatTranscriptionLiveTurn,
          'Said before it broke',
        ]);
      });

      it('a dropped connection reconnects with a new session for the same thread', async () => {
        await renderLive();
        click('live-start');
        act(() => sessions[0].args.onState('listening'));
        act(() => sessions[0].args.onDelta('First'));
        // Fake timers only from here: the provider's own loading above waits on real ones.
        vi.useFakeTimers();
        try {
          act(() => {
            sessions[0].args.onError('closed (1006)', { kind: 'dropped' });
            sessions[0].args.onState('error');
          });
          expect(state()).toBe('reconnecting');

          act(() => void vi.advanceTimersByTime(1000));

          expect(sessions).toHaveLength(2);
          expect(sessions[1].args.model).toBe('whisper');
          expect(transcript()).toBe('First');
        } finally {
          vi.useRealTimers();
        }
      });
    });

    describe('stopping and aborting', () => {
      it('Stop finishes the session; the recording is not over until its transcript is in', async () => {
        await renderLive();
        click('live-start');
        act(() => sessions[0].args.onState('listening'));

        click('live-stop');

        expect(sessions[0].stop).toHaveBeenCalledTimes(1);
        expect(state()).toBe('finalizing');
      });

      it('Abort cancels it and commits nothing', async () => {
        await renderLive();
        click('live-start');
        act(() => sessions[0].args.onState('listening'));
        act(() => sessions[0].args.onDelta('dropped on the floor'));

        click('live-abort');

        expect(sessions[0].abort).toHaveBeenCalledTimes(1);
        expect(state()).toBe('idle');
        expect(screen.getByTestId('count').textContent).toBe('0');
      });

      it('the recording ends with the provider', async () => {
        const view = await renderLive();
        click('live-start');
        act(() => sessions[0].args.onState('listening'));

        view.unmount();

        expect(sessions[0].abort).toHaveBeenCalledTimes(1);
      });
    });

    describe('the thread it belongs to', () => {
      const secondChat = (): ChatRow =>
        liveThread({ id: 'c2', title: 'Other', updated_at: T2 }, { model: 'whisper' });

      it('another chat opened mid-recording ends the recording, and the words are not committed there', async () => {
        await renderLive([liveThread({ title: 'First', updated_at: T1 }), secondChat()]);
        // c2 is the newest, so it is the one open.
        expect(screen.getByTestId('active').textContent).toBe('c2');
        click('live-start');
        act(() => sessions[0].args.onState('listening'));
        act(() => sessions[0].args.onDelta('for chat two'));

        click('select-c1');
        await waitFor(() => expect(screen.getByTestId('active').textContent).toBe('c1'));

        await waitFor(() => expect(state()).toBe('idle'));
        expect(sessions[0].abort).toHaveBeenCalledTimes(1);
        expect(screen.getByTestId('count').textContent).toBe('0');
      });

      it('a new chat opened mid-recording ends the recording', async () => {
        await renderLive();
        click('live-start');
        act(() => sessions[0].args.onState('listening'));

        click('newchat');
        await waitFor(() => expect(screen.getByTestId('active').textContent).not.toBe('c1'));

        await waitFor(() => expect(state()).toBe('idle'));
        expect(sessions[0].abort).toHaveBeenCalledTimes(1);
      });

      it('another model picked mid-recording ends the recording', async () => {
        const parakeet: ModelOption = { ...whisper, id: 'parakeet', display_name: 'parakeet' };
        chatApi = makeChatApi([liveThread()]);
        chatApi.spies.modelTranscriptionStreaming.mockResolvedValue(true);
        renderProvider([], { models: [whisper, parakeet] });
        await waitForReady();
        await waitFor(() =>
          expect(screen.getByTestId('transcription-streaming').textContent).toBe('true'),
        );
        click('live-start');
        act(() => sessions[0].args.onState('listening'));

        click('pick-parakeet');

        await waitFor(() => expect(sessions[0].abort).toHaveBeenCalledTimes(1));
        await waitFor(() => expect(state()).toBe('idle'));
      });
    });

    describe('the rest of the composer while recording', () => {
      it('refuses a send of a clip: the thread is busy with the recording', async () => {
        await renderLive();
        click('attach-recording');
        await waitFor(() => expect(screen.getByTestId('composer-audio').textContent).not.toBe(''));
        click('live-start');
        act(() => sessions[0].args.onState('listening'));

        click('send');

        expect(chatApi.spies.startChatRun).not.toHaveBeenCalled();
        expect(await screen.findByText(t.chatBusy)).toBeInTheDocument();
      });

      // A thread with one transcribed turn, which Regenerate and Edit would replay (the clip is
      // what a replay transcribes). While a recording runs they are refused like a send: a
      // replay started now would own the transcript the recording's turn is to land in.
      const replayableThread = () =>
        liveThread({
          title: 'Clip',
          content: {
            settings: { model: 'whisper', kind: 'transcription' },
            messages: [
              {
                id: 'u1',
                role: 'user',
                content: [
                  { type: 'text', text: '' },
                  { type: 'audio_url', audio_url: { url: 'data:audio/wav;base64,UklG' } },
                ],
              },
              { id: 'a1', role: 'assistant', content: 'The clip', status: 'complete' },
            ],
          },
        });

      it.each([
        ['regenerate', 'regenerate-a1'],
        ['edit', 'edit-u1'],
      ])(
        'refuses %s: the thread is busy with the recording, and nothing is touched',
        async (_n, button) => {
          await renderLive([replayableThread()]);
          click('live-start');
          act(() => sessions[0].args.onState('listening'));

          click(button);

          expect(await screen.findByText(t.chatBusy)).toBeInTheDocument();
          expect(chatApi.spies.startChatRun).not.toHaveBeenCalled();
          // The transcript is as it was (a replay truncates it before it starts the run), and
          // the recording is still going.
          expect(screen.getByTestId('count').textContent).toBe('2');
          expect(screen.getByTestId('last').textContent).toBe('The clip');
          expect(state()).toBe('listening');
        },
      );

      it.each([
        ['regenerate', 'regenerate-a1'],
        ['edit', 'edit-u1'],
      ])('lets %s through again once the recording has ended', async (_n, button) => {
        await renderLive([replayableThread()]);
        click('live-start');
        act(() => sessions[0].args.onState('listening'));
        click('live-abort');
        expect(state()).toBe('idle');

        click(button);

        await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
        expect(screen.queryByText(t.chatBusy)).not.toBeInTheDocument();
      });
    });
  });

  describe(`useChatStreaming isolation [${locale}]`, () => {
    it('does not re-render on store-value churn while streaming is unchanged', () => {
      let renders = 0;
      const Peek = memo(function Peek() {
        useChatStreaming();
        renders += 1;
        return null;
      });
      // Two distinct store objects (mirrors the per-token fresh `value`), both idle.
      const storeA = { streaming: false } as unknown as ChatStore;
      const storeB = { streaming: false } as unknown as ChatStore;
      const tree = (streaming: boolean, store: ChatStore) => (
        <ChatStreamingContext.Provider value={streaming}>
          <ChatStoreContext.Provider value={store}>
            <Peek />
          </ChatStoreContext.Provider>
        </ChatStreamingContext.Provider>
      );
      const { rerender } = render(tree(false, storeA));
      const afterFirst = renders;
      // Store value changes (new object) but the streaming boolean does not:
      // a useChatStreaming consumer must NOT re-render.
      rerender(tree(false, storeB));
      expect(renders).toBe(afterFirst);
      // Flipping the streaming boolean DOES re-render it.
      rerender(tree(true, storeB));
      expect(renders).toBe(afterFirst + 1);
    });
  });
}
