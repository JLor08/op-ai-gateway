// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { useState } from 'react';
import { afterEach, beforeEach, describe, expect, it, vi, type Mock } from 'vitest';
import { heicTo } from 'heic-to';

import { Chat } from './Chat';
import { createLiveSession, type LiveSessionArgs } from './chat/liveTranscription';
import {
  ChatStoreProvider,
  useChatStore,
  IMAGE_OUTPUT_FORMATS,
  IMAGE_SAMPLERS,
  IMAGE_SCHEDULERS,
  IMAGE_SIZE_OPTIONS,
} from './chat/ChatStore';
import { ToastProvider } from './shared/ToastProvider';
import { AudioDecodeError, audioBlobToWav } from './shared/audioToWav';
import { messages, type Locale } from '../i18n';
import {
  NO_SPEECH_LANGUAGE,
  NO_SPEECH_STREAMING,
  type ActiveChatRun,
  type ImageModelOptions,
  type ModelOption,
  type PortalServer,
  type PortalToken,
  type ServerModelOption,
  type SpeechLanguageGate,
  type SpeechStreamingGate,
  type StartChatRunBody,
} from '../api';

// heic-to loads a libheif WASM decoder at runtime, which jsdom cannot run: mock it
// (as imageAttach.test.ts does) to a JPEG blob, so a HEIC attachment can be followed
// through the real upload pipeline (prepareImageDataUrl) into the sent message.
vi.mock('heic-to', () => ({
  heicTo: vi.fn(async () => new Blob(['jpeg-bytes'], { type: 'image/jpeg' })),
}));

// jsdom has no WebAudio, so the browser-side conversion of a clip to WAV (audioToWav.test.ts's
// subject) is a stand-in here: it "converts" by prefixing the bytes, so a part's payload says
// whether the CONVERTED clip (not the upload, merely relabelled) is what the turn carries.
vi.mock('./shared/audioToWav', async (importOriginal) => ({
  ...(await importOriginal<typeof import('./shared/audioToWav')>()),
  audioBlobToWav: vi.fn(async (blob: Blob) => new Blob(['WAV:', blob], { type: 'audio/wav' })),
}));

// jsdom has no Web Audio, so the progressive speech playback is a recording stand-in: inert
// (it plays nothing and reports no run live) unless a test makes its `push` say the run is
// playing, as the live speech bubble's controls (mute, abort) do.
const playback = vi.hoisted(() => ({
  prime: vi.fn(),
  push: vi.fn((..._args: unknown[]) => false),
  end: vi.fn(),
  stopAll: vi.fn(),
  isLive: vi.fn(() => false),
  setMuted: vi.fn(),
}));
vi.mock('./chat/speechPlayback', async (importOriginal) => ({
  ...(await importOriginal<typeof import('./chat/speechPlayback')>()),
  speechPlayback: playback,
}));

// jsdom has no microphone or WebSocket either: a live transcription session is a recording
// stand-in, driven by hand from the tests of the composer's live mode (the session itself has
// its own tests: liveTranscription.session.test.ts).
vi.mock('./chat/liveTranscription', async (importOriginal) => ({
  ...(await importOriginal<typeof import('./chat/liveTranscription')>()),
  createLiveSession: vi.fn(),
}));

// Captured before any spy is installed, so a mocked createElement can build a
// real element without recursing into itself.
const nativeCreateElement = document.createElement.bind(document);

// vision: true so the pre-existing image-retention/attach tests below (which
// predate the vision-capability gate) keep exercising attach + send unchanged;
// the "Chat: vision gating" describe block below covers the non-capable path.
const models: ModelOption[] = [
  {
    id: 'gpt-oss-20b',
    display_name: 'gpt-oss-20b',
    flavors: ['openai'],
    loading_on_count: 0,
    vision: true,
  },
];

// The store now subscribes to a server run over EventSource; this fake records
// every instance so a test can emit the run's snapshot/delta/done events.
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

// Stateful in-memory chat backend so the provider's list-load / create / save /
// reload round-trips exercise real persistence (the transcript now lives
// server-side, not in localStorage). The same instance is reused across an
// unmount + remount within a test, so a saved transcript survives the reload.
type ChatRow = {
  id: string;
  title: string;
  created_at: string;
  updated_at: string;
  content: unknown;
};
function makeChatApi() {
  let seq = 0;
  const rows: ChatRow[] = [];
  const stamp = () => new Date(Date.UTC(2026, 6, 17, 12, seq)).toISOString();
  const api = {
    // The real listing still serves max_content_bytes (portal.MaxChatContentBytes);
    // the composer must IGNORE it now that images live out of the transcript, so
    // the mock keeps serving it to prove the capacity line stays gone.
    chats: vi.fn(async () => ({
      data: rows.map(({ content: _content, ...rest }) => rest),
      max_content_bytes: 4 * 1024 * 1024,
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
    startChatRun: vi.fn(async (chatId: string, _body: StartChatRunBody) => ({
      run_id: 'run_1',
      chat_id: chatId,
      status: 'running' as const,
    })),
    cancelChatRun: vi.fn(async () => ({ ok: true })),
    activeChatRuns: vi.fn(async () => ({ data: [] as ActiveChatRun[] })),
    // The audio answer lifecycle's two mutations (#281): the gateway answers {ok:true}.
    deleteMessageAudio: vi.fn(async (_chatId: string, _messageId: string) => ({ ok: true })),
    pruneChatAudio: vi.fn(async (_chatId: string, _keepLast: number) => ({ ok: true })),
    // The server-override picker's filtered-model fetch (Task 6, step 3).
    serverModels: vi.fn(async (serverId: string) => serverModelsByServer[serverId] ?? []),
    // The per-model sd-server image options the store fetches whenever the picked
    // model GENERATES images: this file's fixtures know none, so every field is
    // omitted (the dropdowns then use the static Phase 1b-i superset).
    modelImageOptions: vi.fn(async (_name: string) => ({}) as ImageModelOptions),
    // The cached voice names the store fetches whenever the picked model SYNTHESIZES
    // speech: none by default (the voice field is then plain free text); the voice
    // picker tests below seed their own.
    modelVoices: vi.fn(async (_name: string) => [] as string[]),
    // The speech composer's streaming gate the store fetches for the same models: closed by
    // default (no stream toggle, the buffered run); the streaming-toggle tests below open it.
    modelSpeechStreaming: vi.fn(
      async (_name: string): Promise<SpeechStreamingGate> => NO_SPEECH_STREAMING,
    ),
    // The speech composer's language gate the store fetches for the same models: closed by
    // default (no language control, nothing pre-filled); the language-control tests below open it.
    modelSpeechLanguage: vi.fn(
      async (_name: string): Promise<SpeechLanguageGate> => NO_SPEECH_LANGUAGE,
    ),
    // The transcription composer's live gate the store fetches for transcription models:
    // closed by default (no live mode, the buffered recording).
    modelTranscriptionStreaming: vi.fn(async (_name: string): Promise<boolean> => false),
    // The live recording's ticket POST: no test in this file starts a live session, but the store
    // now holds a real reference to this method so the mock must provide one.
    createLiveTranscriptionTicket: vi.fn(async (_body: { model: string; language?: string }) => ({
      ticket: 'tkt',
    })),
    // The pagehide keepalive flush: no test in this file dispatches `pagehide`,
    // but the store now holds a real reference to this method so the mock must
    // provide one.
    saveChatKeepalive: vi.fn(),
  };
  return { api, rows, spies: api };
}

// Seeded by tests that exercise the server-override picker's filtered model
// dropdown, keyed by server id; read by the makeChatApi() fake above. Reset in
// beforeEach.
let serverModelsByServer: Record<string, ServerModelOption[]> = {};

function makeServer(overrides: Partial<PortalServer> = {}): PortalServer {
  return {
    id: 'srv_a',
    name: 'Server A',
    domain: 'srv-a.example.test',
    server_path_suffix: '',
    status: 'active',
    health_status: 'healthy',
    owners: [],
    last_seen_at: null,
    created_at: '2026-08-12T12:00:00Z',
    netbird_enabled: false,
    netbird_setup_key_id: '',
    netbird_group_id: '',
    netbird_peer_id: '',
    netbird_connected: false,
    netbird_group_ids: [],
    netbird_peer_managed: false,
    netbird_policy_override: '',
    netbird_allow_ping: false,
    netbird_ping_exclude: false,
    agent_status: 'unconfigured',
    agent_presence_timeout_seconds: 0,
    estimated_watts: 0,
    idle_watts: 0,
    price_per_kwh: 0,
    pue: 0,
    price_unit: 'eur_cent',
    admin_groups: [],
    system_group_id: '',
    system_group_name: '',
    ...overrides,
  };
}

let chatApi: ReturnType<typeof makeChatApi>;

// Seeds a chat whose saved model is not among `models` (simulates an upstream
// that has since become unreachable) so the bootstrap load activates it as-is
// — no coercion. Empty title so the sidebar's "Ohne Titel" placeholder still
// gates waitForChatReady.
function seedUnavailableModelChat() {
  const stamp = '2026-07-17T12:00:00Z';
  chatApi.rows.push({
    id: 'c_seed',
    title: '',
    created_at: stamp,
    updated_at: stamp,
    content: { settings: { model: 'unavailable-model' }, messages: [] },
  });
}

// Seeds a persisted chat whose assistant turn already carries a generated
// image, preceded by the user turn that asked for it -- exercising promptText
// through the real component tree (Chat.tsx computing it from the previous
// message, ChatStore loading it, ChatMessage/ImageTurn rendering it) rather
// than as a literal prop handed directly to ChatMessage in isolation.
function seedGeneratedImageChat() {
  const stamp = '2026-07-19T12:00:00Z';
  chatApi.rows.push({
    id: 'c_image_seed',
    title: '',
    created_at: stamp,
    updated_at: stamp,
    content: {
      settings: {},
      messages: [
        { id: 'm_prompt', role: 'user', content: 'a cat on a bicycle' },
        {
          id: 'm_image',
          role: 'assistant',
          content: [{ type: 'image_url', image_url: { url: 'data:image/png;base64,AAAA' } }],
        },
      ],
    },
  });
}

function makeToken(overrides: Partial<PortalToken> = {}): PortalToken {
  return {
    id: 'tok_plain',
    name: 'Plain Token',
    secret_prefix: 'dev-secr',
    status: 'active',
    scopes: ['gateway:use'],
    expires_at: null,
    last_used_at: null,
    created_at: '2026-07-10T12:00:00Z',
    model_override: '',
    log_communication: false,
    secret: false,
    is_chat_session: false,
    deletable: true,
    ...overrides,
  };
}

const plainToken = makeToken({ id: 'tok_plain', name: 'Plain Token', model_override: '' });
const overrideToken = makeToken({
  id: 'tok_override',
  name: 'Override Token',
  model_override: 'qwen-coder',
});
const tokens: PortalToken[] = [plainToken, overrideToken];

// This jsdom build exposes no window.localStorage; install an in-memory stub so
// the lifted store's persistence (and the image-retention across remount test)
// is exercised. The downscaler decodes via `new Image()`, which never fires
// onload for a data URL under jsdom, so stub a decoder that reports small dims
// (<= the 1568px cap -> the original data URL is kept, no canvas re-encode).
// Flip on to make the stubbed decoder fail (fires onerror) so prepareImageDataUrl
// rejects with ImageAttachError reason "decode". Reset per test in beforeEach.
let imageDecodeFails = false;

// What the stubbed decoder reports as an image's pixel size: 32x24 unless a test says
// otherwise (<= the 1568 cap, so nothing is ever re-encoded). A test that needs a
// bigger image either registers it here by its exact src (a chat-blob reference, which
// holds no bytes to read) or makes it out of bytes that SAY their size: fakeImageFile
// builds such a file, and the stubbed canvas (stubCanvas) encodes the size of what it
// draws the same way, so a data url read back through the decoder gives the size the
// pipeline really produced. Reset per test in beforeEach.
let imageSizes = new Map<string, [number, number]>();
const fakeImageBytes = (width: number, height: number) => `fake-image ${width}x${height}`;
function fakeImageFile(width: number, height: number, name = 'image.png') {
  return new File([fakeImageBytes(width, height)], name, { type: 'image/png' });
}
function decodedSize(src: string): [number, number] {
  const registered = imageSizes.get(src);
  if (registered) return registered;
  const payload = /^data:[^;,]+;base64,(.*)$/.exec(src)?.[1];
  let said: RegExpExecArray | null = null;
  try {
    said = payload ? /^fake-image (\d+)x(\d+)$/.exec(atob(payload)) : null;
  } catch {
    // not base64 at all: a seeded placeholder, which is the default size
  }
  return said ? [Number(said[1]), Number(said[2])] : [32, 24];
}

function installStubs() {
  const store = new Map<string, string>();
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
  class MockImage {
    onload: (() => void) | null = null;
    onerror: (() => void) | null = null;
    naturalWidth = 0;
    naturalHeight = 0;
    set src(value: string) {
      queueMicrotask(() => {
        if (imageDecodeFails) {
          this.onerror?.();
          return;
        }
        [this.naturalWidth, this.naturalHeight] = decodedSize(value);
        this.onload?.();
      });
    }
  }
  vi.stubGlobal('Image', MockImage);
  vi.stubGlobal('EventSource', FakeEventSource as unknown as typeof EventSource);
}

// The run-as-token and model dropdowns are now searchable MUI Autocompletes
// (shared/SearchableSelect), not native <select>s. Pick an option by opening the
// combobox (scoped by its label) and clicking the option, which renders in a
// portal on document.body.
async function pickOption(comboLabel: string, optionText: string) {
  fireEvent.mouseDown(screen.getByRole('combobox', { name: comboLabel }));
  fireEvent.click(await screen.findByRole('option', { name: optionText }));
}

// Lets the promise continuations a click sets off (the recorder's awaited getUserMedia and its
// recorder callbacks) run and their state updates land. act() wraps exactly that -- code that is
// not a Testing Library call -- and not the click itself, which fireEvent already wraps.
async function flushMicrotasks() {
  await act(async () => {});
}

beforeEach(() => {
  imageDecodeFails = false;
  imageSizes = new Map();
  FakeEventSource.instances = [];
  serverModelsByServer = {};
  installStubs();
  chatApi = makeChatApi();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

for (const locale of ['de', 'en'] as readonly Locale[]) {
  const t = messages[locale];

  // Wait for the provider's initial list-load to finish and open the fresh chat:
  // the sidebar renders its (untitled) row only after chatsLoading flips false and
  // the chat is activated, so this doubles as a readiness gate before interacting.
  async function waitForChatReady() {
    await screen.findByText(t.chatUntitled);
  }

  function renderChat(
    initialTokens: PortalToken[] = tokens,
    modelsOverride: ModelOption[] = models,
    serversOverride: PortalServer[] = [],
  ) {
    const onRefresh = vi.fn(async () => {});
    const view = render(
      <ToastProvider>
        <ChatStoreProvider
          api={chatApi.api}
          models={modelsOverride}
          tokens={initialTokens}
          servers={serversOverride}
          onRefresh={onRefresh}
          t={t}
          locale={locale}
        >
          <Chat t={t} />
        </ChatStoreProvider>
      </ToastProvider>,
    );
    const rerenderWith = (
      nextTokens: PortalToken[],
      nextServers: PortalServer[] = serversOverride,
    ) =>
      view.rerender(
        <ToastProvider>
          <ChatStoreProvider
            api={chatApi.api}
            models={modelsOverride}
            tokens={nextTokens}
            servers={nextServers}
            onRefresh={onRefresh}
            t={t}
            locale={locale}
          >
            <Chat t={t} />
          </ChatStoreProvider>
        </ToastProvider>,
      );
    return { onRefresh, rerenderWith, unmount: view.unmount };
  }

  function openSettings() {
    fireEvent.click(screen.getByRole('button', { name: t.chatSettings }));
  }

  describe(`Chat run-as token selector [${locale}]`, () => {
    it("sends the run-as token id in the started run's settings for a plain (no-override) token", async () => {
      renderChat();
      await waitForChatReady();

      await pickOption(t.chatModel, models[0].display_name);
      await pickOption(t.chatRunAsTokenLabel, plainToken.name);
      fireEvent.change(screen.getByLabelText(t.messageLabel), { target: { value: 'hello there' } });
      fireEvent.click(screen.getByRole('button', { name: t.send }));

      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalled());
      const body = chatApi.spies.startChatRun.mock.calls[0][1] as StartChatRunBody;
      expect(body.settings.run_as_token_id).toBe(plainToken.id);
    });

    it('disables the model select and shows the override hint when a model-override token is selected', async () => {
      renderChat();
      await waitForChatReady();

      const modelSelect = screen.getByLabelText(t.chatModel) as HTMLInputElement;
      expect(modelSelect).not.toBeDisabled();

      await pickOption(t.chatRunAsTokenLabel, overrideToken.name);

      expect(modelSelect).toBeDisabled();
      expect(screen.getByText(`${t.chatModelFromToken}: qwen-coder`)).toBeInTheDocument();
    });

    it('defaults to no token and starts a run with an empty run_as_token_id', async () => {
      renderChat();
      await waitForChatReady();

      await pickOption(t.chatModel, models[0].display_name);
      fireEvent.change(screen.getByLabelText(t.messageLabel), { target: { value: 'hi' } });
      fireEvent.click(screen.getByRole('button', { name: t.send }));

      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalled());
      const body = chatApi.spies.startChatRun.mock.calls[0][1] as StartChatRunBody;
      expect(body.settings.run_as_token_id ?? '').toBe('');
    });

    it('resets a stale selection when the chosen token is no longer usable', async () => {
      const { rerenderWith } = renderChat();
      await waitForChatReady();
      await pickOption(t.chatModel, models[0].display_name);

      // The Autocomplete input reflects the selected option's LABEL (not its id).
      const tokenSelect = screen.getByLabelText(t.chatRunAsTokenLabel) as HTMLInputElement;
      await pickOption(t.chatRunAsTokenLabel, plainToken.name);
      expect(tokenSelect.value).toBe(plainToken.name);

      // Simulate onRefresh reloading tokens without the previously-selected one
      // (deleted / disabled / expired / lost gateway:use).
      rerenderWith([overrideToken]);

      // Selection resets to the empty value, whose option label is chatRunAsNone.
      await waitFor(() => expect(tokenSelect.value).toBe(t.chatRunAsNone));

      // A subsequent send must carry no run-as target.
      fireEvent.change(screen.getByLabelText(t.messageLabel), { target: { value: 'hi' } });
      fireEvent.click(screen.getByRole('button', { name: t.send }));

      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalled());
      const body = chatApi.spies.startChatRun.mock.calls[0][1] as StartChatRunBody;
      expect(body.settings.run_as_token_id ?? '').toBe('');
    });
  });

  describe(`Chat image retention [${locale}]`, () => {
    it('persists a sent image to the server and restores it on reload', async () => {
      const { unmount } = renderChat();
      await waitForChatReady();
      await pickOption(t.chatModel, models[0].display_name);

      // Attach an image; the compose strip shows the preview.
      const fileInput = screen.getByLabelText(t.chatAttachImage);
      const goodFile = new File(['x'], 'a.jpg', { type: 'image/jpeg' });
      fireEvent.change(fileInput, { target: { files: [goodFile] } });
      await screen.findByAltText(t.chatAttachedImage);

      // Send it with some text; start a server run, then finish it with an empty
      // reply so the run-end flush round-trips the transcript to the server.
      fireEvent.change(screen.getByLabelText(t.messageLabel), {
        target: { value: 'look at this' },
      });
      fireEvent.click(screen.getByRole('button', { name: t.send }));

      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalled());
      // The sent user message carries the image (compose strip cleared on send).
      await waitFor(() => expect(screen.getAllByAltText(t.chatAttachedImage)).toHaveLength(1));
      // "look at this" also becomes the auto-title in the sidebar, so scope the
      // transcript assertion to the message log.
      expect(within(screen.getByRole('log')).getByText('look at this')).toBeInTheDocument();

      // Finish the run (empty reply): the dangling empty assistant is pruned and
      // the run-end flush persists the user turn (with the image) to the server.
      await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
      await act(async () => {
        FakeEventSource.instances[0].emit('done', { content: '', status: 'completed' });
      });

      // The run-end flush must round-trip the transcript (with the image) to the
      // server before we reload.
      await waitFor(() => {
        const stored = chatApi.rows[0]?.content as { messages?: unknown[] } | undefined;
        expect(stored?.messages?.length ?? 0).toBeGreaterThan(0);
      });

      // Remounting a FRESH provider (same backend) reloads the newest chat and
      // restores the image + text from server content, not localStorage.
      unmount();
      renderChat();
      await waitFor(() => expect(screen.getAllByAltText(t.chatAttachedImage)).toHaveLength(1));
      expect(within(screen.getByRole('log')).getByText('look at this')).toBeInTheDocument();
    });

    it('maps an image decode failure to the generic image error', async () => {
      imageDecodeFails = true;
      renderChat();
      await waitForChatReady();

      const fileInput = screen.getByLabelText(t.chatAttachImage);
      const goodFile = new File(['x'], 'a.jpg', { type: 'image/jpeg' });
      fireEvent.change(fileInput, { target: { files: [goodFile] } });

      const banner = await screen.findByRole('alert');
      expect(banner).toHaveTextContent(t.chatImageError);
      // not the type/size branch text, and no image attached
      expect(banner).not.toHaveTextContent(t.chatImageErrorSize);
      expect(screen.queryByAltText(t.chatAttachedImage)).not.toBeInTheDocument();
    });
  });

  describe(`Chat: remembers an unavailable model [${locale}]`, () => {
    it("keeps the saved-but-unavailable model as the dropdown's selected value and shows the red indicator", async () => {
      seedUnavailableModelChat();
      renderChat();
      await waitForChatReady();

      const modelSelect = screen.getByLabelText(t.chatModel) as HTMLInputElement;
      expect(modelSelect.value).toBe('unavailable-model');
      expect(screen.getByTestId('searchable-select-unavailable')).toBeInTheDocument();
    });

    it('disables Send while the model is unavailable', async () => {
      seedUnavailableModelChat();
      renderChat();
      await waitForChatReady();

      expect(screen.getByRole('button', { name: t.send })).toBeDisabled();
    });

    it('shows no red indicator once an available model is picked and enables Send', async () => {
      renderChat();
      await waitForChatReady();
      await pickOption(t.chatModel, models[0].display_name);

      expect(screen.queryByTestId('searchable-select-unavailable')).not.toBeInTheDocument();
      expect(screen.getByRole('button', { name: t.send })).not.toBeDisabled();
    });

    it('does not auto-select a model: a fresh chat starts empty with Send disabled and no red indicator', async () => {
      renderChat();
      await waitForChatReady();

      const modelSelect = screen.getByLabelText(t.chatModel) as HTMLInputElement;
      expect(modelSelect.value).toBe('');
      expect(screen.queryByTestId('searchable-select-unavailable')).not.toBeInTheDocument();
      expect(screen.getByRole('button', { name: t.send })).toBeDisabled();
    });
  });

  describe(`Chat: generated-image alt text comes from the prompt [${locale}]`, () => {
    // ChatMessage.test.tsx pins this at the literal-prop level (promptText
    // handed straight to ChatMessage); nothing there proves Chat.tsx actually
    // computes and threads that prop through the real component tree. This
    // renders a persisted thread through Chat + ChatStoreProvider end to end.
    it("renders a generated assistant image whose alt text is the preceding user turn's prompt", async () => {
      seedGeneratedImageChat();
      renderChat();
      await waitForChatReady();

      const image = await screen.findByAltText('a cat on a bicycle');
      expect(image).toHaveAttribute('src', 'data:image/png;base64,AAAA');
    });

    // The same argument one prop over: ChatMessage.test.tsx pins that a turn
    // id produces a turn-unique download name, but nothing there proves
    // Chat.tsx actually supplies one. Without it every turn falls back to the
    // bare positional name and two image turns collide again.
    it("names the download from the TURN's own id, threaded from the transcript", async () => {
      seedGeneratedImageChat();
      let anchor: HTMLAnchorElement | undefined;
      // Restored in the finally below: this suite's afterEach unstubs globals
      // but does not restore spies, and a surviving createElement spy
      // recurses into itself on the next test that installs one.
      const createElement = vi
        .spyOn(document, 'createElement')
        .mockImplementation((tag: string, options?: unknown) => {
          const el = nativeCreateElement(tag, options as ElementCreationOptions);
          if (tag === 'a') {
            anchor = el as HTMLAnchorElement;
            // jsdom cannot navigate to the blob: URL downloadBinary builds.
            anchor.click = () => {};
          }
          return el;
        });
      try {
        vi.stubGlobal('URL', {
          ...URL,
          createObjectURL: vi.fn(() => 'blob:stub'),
          revokeObjectURL: vi.fn(),
        });
        renderChat();
        await waitForChatReady();
        await screen.findByAltText('a cat on a bicycle');

        fireEvent.click(screen.getByRole('button', { name: t.chatDownloadImage }));

        expect(anchor?.download).toBe('generated-image-m_image-1.png');
      } finally {
        createElement.mockRestore();
      }
    });
  });

  describe(`Chat: loaded-state of the selected model [${locale}]`, () => {
    it('shows a green loaded dot BEFORE the name once a loaded model is selected', async () => {
      const loaded: ModelOption[] = [
        {
          id: 'gpt-oss-20b',
          display_name: 'gpt-oss-20b',
          flavors: ['openai'],
          loading_on_count: 0,
          loaded: true,
          loaded_on: ['GPU-1'],
        },
      ];
      renderChat(tokens, loaded);
      await waitForChatReady();
      await pickOption(t.chatModel, 'gpt-oss-20b');

      expect(screen.getByTestId('searchable-select-loaded-dot')).toBeInTheDocument();
    });

    it('shows no loaded dot when the selected model is not loaded', async () => {
      renderChat(); // default model fixture is not loaded
      await waitForChatReady();
      await pickOption(t.chatModel, models[0].display_name);

      expect(screen.queryByTestId('searchable-select-loaded-dot')).not.toBeInTheDocument();
    });
  });

  describe(`Chat: image attachment gated on model vision capability [${locale}]`, () => {
    const nonVisionModels: ModelOption[] = [
      {
        id: 'text-only',
        display_name: 'text-only',
        flavors: ['openai'],
        loading_on_count: 0,
        vision: false,
      },
    ];
    const mixedModels: ModelOption[] = [
      {
        id: 'vision-model',
        display_name: 'vision-model',
        flavors: ['openai'],
        loading_on_count: 0,
        vision: true,
      },
      {
        id: 'text-model',
        display_name: 'text-model',
        flavors: ['openai'],
        loading_on_count: 0,
        vision: false,
      },
    ];

    it('disables the attach button when the selected model is not vision-capable', async () => {
      renderChat(tokens, nonVisionModels);
      await waitForChatReady();
      await pickOption(t.chatModel, 'text-only');

      const attachButton = screen.getByRole('button', { name: t.chatAttachImage });
      expect(attachButton).toHaveAttribute('aria-disabled', 'true');
    });

    it('keeps the attach button enabled for a vision-capable model', async () => {
      renderChat(); // default fixture is vision: true
      await waitForChatReady();
      await pickOption(t.chatModel, models[0].display_name);

      const attachButton = screen.getByRole('button', { name: t.chatAttachImage });
      expect(attachButton).not.toHaveAttribute('aria-disabled', 'true');
    });

    it('blocks send() with an attached image on a non-vision-capable model and starts no run', async () => {
      renderChat(tokens, nonVisionModels);
      await waitForChatReady();
      await pickOption(t.chatModel, 'text-only');

      // The attach button is disabled but the underlying <input> is not (jsdom
      // does not block a programmatic change event on it), matching how the
      // other attach tests in this file drive the composer.
      const fileInput = screen.getByLabelText(t.chatAttachImage);
      const goodFile = new File(['x'], 'a.jpg', { type: 'image/jpeg' });
      fireEvent.change(fileInput, { target: { files: [goodFile] } });
      await screen.findByAltText(t.chatAttachedImage);

      fireEvent.change(screen.getByLabelText(t.messageLabel), {
        target: { value: 'look at this' },
      });
      fireEvent.click(screen.getByRole('button', { name: t.send }));

      const banner = await screen.findByRole('alert');
      expect(banner).toHaveTextContent(t.chatImageModelUnsupported);
      expect(chatApi.spies.startChatRun).not.toHaveBeenCalled();
      // the composer is left untouched (not sent, not silently dropped)
      expect(screen.getByAltText(t.chatAttachedImage)).toBeInTheDocument();
    });

    it('clears an attached image and warns when switching from a vision-capable to a non-capable model', async () => {
      renderChat(tokens, mixedModels);
      await waitForChatReady();
      await pickOption(t.chatModel, 'vision-model');

      const fileInput = screen.getByLabelText(t.chatAttachImage);
      const goodFile = new File(['x'], 'a.jpg', { type: 'image/jpeg' });
      fireEvent.change(fileInput, { target: { files: [goodFile] } });
      await screen.findByAltText(t.chatAttachedImage);

      await pickOption(t.chatModel, 'text-model');

      await waitFor(() =>
        expect(screen.queryByAltText(t.chatAttachedImage)).not.toBeInTheDocument(),
      );
      const banner = await screen.findByRole('alert');
      expect(banner).toHaveTextContent(t.chatImageModelUnsupported);
    });
  });

  describe(`Chat: the composer of an image thread [${locale}]`, () => {
    // The same shape as nonVisionModels above, with `image: true` and NO
    // `vision` key: an image GENERATOR accepts no image INPUT, and the two are
    // different capabilities (the backend AND-aggregates each separately).
    const imageModels: ModelOption[] = [
      {
        id: 'sd-turbo',
        display_name: 'sd-turbo',
        flavors: ['openai'],
        loading_on_count: 0,
        image: true,
      },
    ];

    it('labels the prompt field as an image prompt for an image-capable model', async () => {
      renderChat(tokens, imageModels);
      await waitForChatReady();
      await pickOption(t.chatModel, imageModels[0].display_name);

      expect(screen.getByLabelText(t.chatImagePromptLabel)).toBeInTheDocument();
      // ...and the text-thread label is gone, not merely shadowed.
      expect(screen.queryByLabelText(t.messageLabel)).not.toBeInTheDocument();
      // The field's hint says what the thread will accept, not just what the
      // field is called.
      expect(screen.getByText(t.chatImageOnlyHint)).toBeInTheDocument();
    });

    // An image model on an sd-server application carries ONLY the
    // openai_images flavor -- its upstream serves no chat endpoint. It must
    // still be selectable, and selecting it must make an image thread.
    const imagesOnlyModels: ModelOption[] = [
      {
        id: 'flux1-dev',
        display_name: 'flux1-dev',
        flavors: ['openai_images'],
        loading_on_count: 0,
        image: true,
      },
    ];

    it('offers an images-only model and makes it an image thread', async () => {
      renderChat(tokens, imagesOnlyModels);
      await waitForChatReady();
      await pickOption(t.chatModel, 'flux1-dev');
      expect(screen.getByLabelText(t.chatImagePromptLabel)).toBeInTheDocument();
    });

    // openai_images without an image:yes verdict is refused by the images
    // gate and cannot be chatted with either (no openai flavor), so offering
    // it produces a request that can only fail.
    it('does not offer an images-flavored model the gate would refuse', async () => {
      renderChat(tokens, [{ ...imagesOnlyModels[0], image: false }]);
      await waitForChatReady();
      fireEvent.mouseDown(screen.getByRole('combobox', { name: t.chatModel }));
      expect(screen.queryByRole('option', { name: 'flux1-dev' })).not.toBeInTheDocument();
    });

    // Every model on a MIXED application ([openai, openai_images]) carries
    // both flavors. One without an image verdict is still a text model: the
    // openai flavor offers it, and it must make an ordinary TEXT thread -- the
    // images flavor alone never takes precedence over openai.
    it('offers a mixed-flavor model without an image verdict as a text model', async () => {
      renderChat(tokens, [
        {
          id: 'mixed-text',
          display_name: 'mixed-text',
          flavors: ['openai', 'openai_images'],
          loading_on_count: 0,
          image: false,
        },
      ]);
      await waitForChatReady();
      await pickOption(t.chatModel, 'mixed-text');
      expect(screen.getByLabelText(t.messageLabel)).toBeInTheDocument();
      expect(screen.queryByLabelText(t.chatImagePromptLabel)).not.toBeInTheDocument();
    });

    it('keeps the ordinary message label for a model that does not generate images', async () => {
      renderChat();
      await waitForChatReady();
      await pickOption(t.chatModel, models[0].display_name);

      expect(screen.getByLabelText(t.messageLabel)).toBeInTheDocument();
      expect(screen.queryByLabelText(t.chatImagePromptLabel)).not.toBeInTheDocument();
      expect(screen.queryByText(t.chatImageOnlyHint)).not.toBeInTheDocument();
    });

    it('sends the image kind in the started run settings', async () => {
      renderChat(tokens, imageModels);
      await waitForChatReady();
      await pickOption(t.chatModel, imageModels[0].display_name);
      fireEvent.change(screen.getByLabelText(t.chatImagePromptLabel), {
        target: { value: 'a cat on a bicycle' },
      });
      fireEvent.click(screen.getByRole('button', { name: t.send }));

      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalled());
      const body = chatApi.spies.startChatRun.mock.calls[0][1] as StartChatRunBody;
      expect(body.settings.kind).toBe('image');
    });

    it('shows the image-generation controls (and hides the text controls) for an image thread', async () => {
      renderChat(tokens, imageModels);
      await waitForChatReady();
      await pickOption(t.chatModel, imageModels[0].display_name);
      fireEvent.click(screen.getByRole('button', { name: t.chatSettings }));

      expect(screen.getByRole('combobox', { name: t.chatImageSize })).toBeInTheDocument();
      expect(screen.getByLabelText(t.chatImageCount)).toBeInTheDocument();
      expect(screen.getByRole('combobox', { name: t.chatImageOutputFormat })).toBeInTheDocument();
      // The text-model controls are gone, not merely shadowed: the images
      // endpoint uses none of them.
      expect(screen.queryByLabelText(t.chatSystemPrompt)).not.toBeInTheDocument();
      expect(screen.queryByLabelText(t.chatMaxTokens)).not.toBeInTheDocument();
    });

    it('shows the text controls (and no image controls) for a text thread', async () => {
      renderChat();
      await waitForChatReady();
      await pickOption(t.chatModel, models[0].display_name);
      fireEvent.click(screen.getByRole('button', { name: t.chatSettings }));

      expect(screen.getByLabelText(t.chatSystemPrompt)).toBeInTheDocument();
      expect(screen.getByLabelText(t.chatMaxTokens)).toBeInTheDocument();
      expect(screen.queryByRole('combobox', { name: t.chatImageSize })).not.toBeInTheDocument();
    });

    it('carries a changed image count into the started run settings', async () => {
      renderChat(tokens, imageModels);
      await waitForChatReady();
      await pickOption(t.chatModel, imageModels[0].display_name);
      fireEvent.click(screen.getByRole('button', { name: t.chatSettings }));
      fireEvent.change(screen.getByLabelText(t.chatImageCount), { target: { value: '4' } });
      fireEvent.change(screen.getByLabelText(t.chatImagePromptLabel), {
        target: { value: 'a cat' },
      });
      fireEvent.click(screen.getByRole('button', { name: t.send }));

      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalled());
      const body = chatApi.spies.startChatRun.mock.calls[0][1] as StartChatRunBody;
      expect(body.settings.image?.n).toBe(4);
    });

    it('reveals the advanced knobs only on demand and sends them (numeric parsed) in the run', async () => {
      renderChat(tokens, imageModels);
      await waitForChatReady();
      await pickOption(t.chatModel, imageModels[0].display_name);
      fireEvent.click(screen.getByRole('button', { name: t.chatSettings }));

      // The advanced section is collapsed until its toggle is clicked.
      expect(screen.queryByLabelText(t.chatImageSteps)).not.toBeInTheDocument();
      fireEvent.click(screen.getByRole('button', { name: t.chatImageAdvanced }));
      expect(screen.getByLabelText(t.chatImageSteps)).toBeInTheDocument();

      fireEvent.change(screen.getByLabelText(t.chatImageSteps), { target: { value: '28' } });
      fireEvent.change(screen.getByLabelText(t.chatImageNegativePrompt), {
        target: { value: 'blurry' },
      });
      fireEvent.change(screen.getByLabelText(t.chatImagePromptLabel), {
        target: { value: 'a cat' },
      });
      fireEvent.click(screen.getByRole('button', { name: t.send }));

      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalled());
      const body = chatApi.spies.startChatRun.mock.calls[0][1] as StartChatRunBody;
      // The blank-able text field is parsed to a wire number; the text knob rides as-is.
      expect(body.settings.image).toMatchObject({ steps: 28, negative_prompt: 'blurry' });
    });

    // The edit knob lives in the advanced section: blank by default (sd-server's
    // own 0.75), explained by a tooltip that also says reference-edit models may
    // want more, and sent as a number when set.
    it('offers the edit strength in the advanced section, blank by default, and sends it parsed', async () => {
      renderChat(tokens, imageModels);
      await waitForChatReady();
      await pickOption(t.chatModel, imageModels[0].display_name);
      fireEvent.click(screen.getByRole('button', { name: t.chatSettings }));
      expect(screen.queryByLabelText(t.chatImageStrength)).not.toBeInTheDocument();
      fireEvent.click(screen.getByRole('button', { name: t.chatImageAdvanced }));

      const strength = screen.getByLabelText(t.chatImageStrength);
      expect(strength).toHaveValue(null); // a blank number field: sd-server keeps its default
      fireEvent.mouseOver(strength);
      expect(await screen.findByRole('tooltip')).toHaveTextContent(t.chatImageStrengthHint);

      fireEvent.change(strength, { target: { value: '0.9' } });
      fireEvent.change(screen.getByLabelText(t.chatImagePromptLabel), {
        target: { value: 'a cat' },
      });
      fireEvent.click(screen.getByRole('button', { name: t.send }));

      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalled());
      const body = chatApi.spies.startChatRun.mock.calls[0][1] as StartChatRunBody;
      expect(body.settings.image?.strength).toBe(0.9);
    });

    it('sends no strength when the field is left blank', async () => {
      renderChat(tokens, imageModels);
      await waitForChatReady();
      await pickOption(t.chatModel, imageModels[0].display_name);
      fireEvent.click(screen.getByRole('button', { name: t.chatSettings }));
      fireEvent.change(screen.getByLabelText(t.chatImagePromptLabel), {
        target: { value: 'a cat' },
      });
      fireEvent.click(screen.getByRole('button', { name: t.send }));

      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalled());
      const body = chatApi.spies.startChatRun.mock.calls[0][1] as StartChatRunBody;
      expect(body.settings.image).not.toHaveProperty('strength');
    });

    it('disables the attach button for an image model that is not vision-capable', async () => {
      renderChat(tokens, imageModels);
      await waitForChatReady();
      await pickOption(t.chatModel, imageModels[0].display_name);

      // An image GENERATOR has no reason to accept an image INPUT, and the two
      // are different capabilities. The tooltip must say which one it means.
      expect(screen.getByRole('button', { name: t.chatAttachImage })).toHaveAttribute(
        'aria-disabled',
        'true',
      );
    });

    it('disables the attach button for an image model that IS vision-capable too', async () => {
      // The load-bearing case: a non-vision image model already had attach
      // disabled by the vision gate alone, so only a model that is BOTH
      // proves the image gate exists. An image thread's request body carries
      // no history and no attachment, so offering the attach button on it
      // would offer something the run silently drops.
      renderChat(tokens, [{ ...imageModels[0], vision: true }]);
      await waitForChatReady();
      await pickOption(t.chatModel, imageModels[0].display_name);

      expect(screen.getByRole('button', { name: t.chatAttachImage })).toHaveAttribute(
        'aria-disabled',
        'true',
      );
    });

    it('explains the disabled attach button with the image-GENERATOR string, not the input one', async () => {
      renderChat(tokens, imageModels);
      await waitForChatReady();
      await pickOption(t.chatModel, imageModels[0].display_name);

      fireEvent.mouseOver(screen.getByRole('button', { name: t.chatAttachImage }));

      const tip = await screen.findByRole('tooltip');
      expect(tip).toHaveTextContent(t.chatImageGeneratorNoInput);
      expect(tip).not.toHaveTextContent(t.chatImageModelUnsupported);
    });

    it('clears an attachment when switching to an image-generating model', async () => {
      // The generator here is ALSO vision-capable, so the pre-existing
      // vision-based clear would not fire: without the image-generator clause
      // the attachment would survive into an image run, whose request body
      // carries no attachment at all — a silent drop after the send.
      renderChat(tokens, [
        {
          id: 'vision-model',
          display_name: 'vision-model',
          flavors: ['openai'],
          loading_on_count: 0,
          vision: true,
        },
        { ...imageModels[0], vision: true },
      ]);
      await waitForChatReady();
      await pickOption(t.chatModel, 'vision-model');

      const fileInput = screen.getByLabelText(t.chatAttachImage);
      fireEvent.change(fileInput, {
        target: { files: [new File(['x'], 'a.jpg', { type: 'image/jpeg' })] },
      });
      await screen.findByAltText(t.chatAttachedImage);

      await pickOption(t.chatModel, imageModels[0].display_name);

      await waitFor(() =>
        expect(screen.queryByAltText(t.chatAttachedImage)).not.toBeInTheDocument(),
      );
      const banner = await screen.findByRole('alert');
      expect(banner).toHaveTextContent(t.chatImageGeneratorNoInput);
    });

    it('shows no image-count capacity line in an image thread', async () => {
      // Generated images are stored out of the transcript (as
      // /api/portal/chat-blobs/{id} urls), so "room for N more images" is no
      // longer derivable from the document size: the composer states no number
      // and refuses no send on one. The listing still serves max_content_bytes,
      // so this fails if the portal keeps reading it for an image estimate.
      renderChat(tokens, imageModels);
      await waitForChatReady();
      await pickOption(t.chatModel, imageModels[0].display_name);

      expect(screen.getByLabelText(t.chatImagePromptLabel)).toBeInTheDocument();
      expect(screen.queryByTestId('chat-capacity')).not.toBeInTheDocument();
    });

    it('shows no capacity line for a text thread', async () => {
      renderChat();
      await waitForChatReady();
      await pickOption(t.chatModel, models[0].display_name);

      expect(screen.queryByTestId('chat-capacity')).not.toBeInTheDocument();
    });
  });

  // The live controls of a streaming speech run (#281), through the real store: Mute is the
  // reversible silence of local playback (the run goes on), Abort is the composer's own Stop
  // -- the run's cancel, nothing else.
  describe(`Chat: the live speech bubble controls [${locale}]`, () => {
    const kokoro: ModelOption = {
      id: 'kokoro',
      display_name: 'kokoro',
      flavors: ['openai_audio'],
      loading_on_count: 0,
      speech: true,
    };

    beforeEach(() => {
      playback.push.mockReset().mockReturnValue(true);
      playback.setMuted.mockClear();
    });

    async function startLiveRun() {
      renderChat(tokens, [kokoro]);
      await waitForChatReady();
      await pickOption(t.chatModel, 'kokoro');
      fireEvent.change(screen.getByLabelText(t.chatSpeechPromptLabel), {
        target: { value: 'hello there' },
      });
      fireEvent.click(screen.getByRole('button', { name: t.send }));
      await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
      const es = FakeEventSource.instances[0];
      act(() => {
        es.emit('snapshot', { reasoning: '', content: '', status: 'running', kind: 'speech' });
        es.emit('audio', { audio: { seq: 0, rate: 44100, data: 'AAAA' } });
      });
      return es;
    }

    it('offers Mute and Abort once the run plays live', async () => {
      await startLiveRun();
      expect(await screen.findByRole('button', { name: t.chatAudioMute })).toBeInTheDocument();
      expect(screen.getByRole('button', { name: t.chatAudioAbort })).toBeInTheDocument();
    });

    it('Mute silences the playback and flips to Unmute; Unmute restores it', async () => {
      await startLiveRun();
      fireEvent.click(await screen.findByRole('button', { name: t.chatAudioMute }));
      expect(playback.setMuted).toHaveBeenLastCalledWith('chat_1', true);
      fireEvent.click(await screen.findByRole('button', { name: t.chatAudioUnmute }));
      expect(playback.setMuted).toHaveBeenLastCalledWith('chat_1', false);
      expect(await screen.findByRole('button', { name: t.chatAudioMute })).toBeInTheDocument();
      // Muting is playback only: the run was never cancelled.
      expect(chatApi.spies.cancelChatRun).not.toHaveBeenCalled();
    });

    it('Abort cancels the run through the existing cancel', async () => {
      await startLiveRun();
      fireEvent.click(await screen.findByRole('button', { name: t.chatAudioAbort }));
      await waitFor(() => expect(chatApi.spies.cancelChatRun).toHaveBeenCalledTimes(1));
      expect(chatApi.spies.cancelChatRun).toHaveBeenCalledWith('chat_1', 'run_1');
    });

    it('drops the controls when the run ends', async () => {
      const es = await startLiveRun();
      await screen.findByRole('button', { name: t.chatAudioAbort });
      act(() => es.emit('done', { content: '', status: 'canceled', error: '', kind: 'speech' }));
      await waitFor(() =>
        expect(screen.queryByRole('button', { name: t.chatAudioAbort })).toBeNull(),
      );
      expect(screen.queryByRole('button', { name: t.chatAudioMute })).toBeNull();
      expect(screen.queryByRole('button', { name: t.chatAudioUnmute })).toBeNull();
    });
  });

  // A text-to-speech model is picked like an image generator: the chat offers it and an
  // empty thread's composer follows it (the store's prospective kind). The input stays
  // text; the settings panel swaps the text-chat controls for the speech ones (a
  // voice combobox -- the model's cached voices to pick from, any name to type -- a
  // speed, and a collapsed Advanced JSON passthrough). The output is WAV only, so no
  // format control exists.
  describe(`Chat: the composer of a speech thread [${locale}]`, () => {
    const speechModels: ModelOption[] = [
      {
        id: 'kokoro',
        display_name: 'kokoro',
        flavors: ['openai_audio'],
        loading_on_count: 0,
        speech: true,
      },
    ];

    async function renderSpeechThread(models: ModelOption[] = speechModels) {
      renderChat(tokens, models);
      await waitForChatReady();
      await pickOption(t.chatModel, 'kokoro');
    }

    // Opens the settings panel and the Advanced section; returns the passthrough field.
    function openPassthrough() {
      openSettings();
      fireEvent.click(screen.getByRole('button', { name: t.chatSpeechAdvanced }));
      return screen.getByLabelText(t.chatSpeechPassthrough);
    }

    async function sendSpeech(text = 'hello there') {
      fireEvent.change(screen.getByLabelText(t.chatSpeechPromptLabel), { target: { value: text } });
      fireEvent.click(screen.getByRole('button', { name: t.send }));
    }

    function startedSettings() {
      return (chatApi.spies.startChatRun.mock.calls[0][1] as StartChatRunBody).settings;
    }

    it('labels the prompt field as the text to speak, still a text input', async () => {
      await renderSpeechThread();

      const prompt = screen.getByLabelText(t.chatSpeechPromptLabel);
      expect(prompt.tagName).toBe('TEXTAREA');
      expect(screen.getByText(t.chatSpeechOnlyHint)).toBeInTheDocument();
      // The text and image labels are gone, not merely shadowed.
      expect(screen.queryByLabelText(t.messageLabel)).not.toBeInTheDocument();
      expect(screen.queryByLabelText(t.chatImagePromptLabel)).not.toBeInTheDocument();
    });

    it('shows the speech controls and hides the text and image controls', async () => {
      await renderSpeechThread();
      openSettings();

      expect(screen.getByRole('combobox', { name: t.chatSpeechVoice })).toBeInTheDocument();
      expect(screen.getByRole('spinbutton', { name: t.chatSpeechSpeed })).toBeInTheDocument();
      expect(screen.getByRole('button', { name: t.chatSpeechAdvanced })).toBeInTheDocument();
      // A system prompt, temperature and a token cap mean nothing to /v1/audio/speech...
      expect(screen.queryByLabelText(t.chatSystemPrompt)).not.toBeInTheDocument();
      expect(screen.queryByLabelText(t.chatMaxTokens)).not.toBeInTheDocument();
      // ...and neither do the image knobs.
      expect(screen.queryByRole('combobox', { name: t.chatImageSize })).not.toBeInTheDocument();
      expect(screen.queryByLabelText(t.chatImageCount)).not.toBeInTheDocument();
      expect(screen.queryByRole('button', { name: t.chatImageAdvanced })).not.toBeInTheDocument();
    });

    // The speech controls belong to a speech THREAD only.
    function expectNoSpeechControls() {
      expect(screen.queryByLabelText(t.chatSpeechVoice)).not.toBeInTheDocument();
      expect(screen.queryByLabelText(t.chatSpeechSpeed)).not.toBeInTheDocument();
      expect(screen.queryByRole('button', { name: t.chatSpeechAdvanced })).not.toBeInTheDocument();
      expect(screen.queryByLabelText(t.chatSpeechPromptLabel)).not.toBeInTheDocument();
    }

    it('hides the speech controls on a text thread', async () => {
      renderChat();
      await waitForChatReady();
      await pickOption(t.chatModel, models[0].display_name);
      openSettings();

      expect(screen.getByLabelText(t.chatSystemPrompt)).toBeInTheDocument();
      expectNoSpeechControls();
    });

    it('hides the speech controls on an image thread', async () => {
      renderChat(tokens, [
        {
          id: 'sd-turbo',
          display_name: 'sd-turbo',
          flavors: ['openai'],
          loading_on_count: 0,
          image: true,
        },
      ]);
      await waitForChatReady();
      await pickOption(t.chatModel, 'sd-turbo');
      openSettings();

      expect(screen.getByRole('combobox', { name: t.chatImageSize })).toBeInTheDocument();
      expectNoSpeechControls();
    });

    it('offers the voice as a combobox and the speed as a number, both blank by default', async () => {
      await renderSpeechThread();
      openSettings();

      // A combobox that also takes free text (a model's voices are only what the
      // gateway has cached, so any name may still be typed).
      const voice = screen.getByRole('combobox', { name: t.chatSpeechVoice });
      expect(voice).toHaveValue('');
      // The placeholder says "model default" -- it is a hint, never written.
      expect(voice).toHaveAttribute('placeholder', t.chatSpeechVoiceDefault);

      const speed = screen.getByRole('spinbutton', { name: t.chatSpeechSpeed });
      expect(speed).toHaveValue(null); // a blank number field: the model keeps its own rate
      expect(speed).toHaveAttribute('placeholder', '1.0');
      // A negative rate is inert, so the field refuses it; otherwise unclamped (no max):
      // the model's own range applies.
      expect(speed).toHaveAttribute('min', '0');
      expect(speed).not.toHaveAttribute('max');
    });

    it('does not offer a response format: the answer is always WAV', async () => {
      await renderSpeechThread();
      openSettings();
      fireEvent.click(screen.getByRole('button', { name: t.chatSpeechAdvanced }));

      // No control of any kind for it, and the panel says why.
      expect(screen.queryByLabelText(/format/i)).not.toBeInTheDocument();
      expect(screen.queryByRole('combobox', { name: /format/i })).not.toBeInTheDocument();
      expect(screen.getByText(t.chatSpeechFormatHint)).toBeInTheDocument();

      fireEvent.change(screen.getByLabelText(t.chatSpeechPromptLabel), { target: { value: 'x' } });
      fireEvent.click(screen.getByRole('button', { name: t.send }));
      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalled());
      expect(startedSettings().audio).not.toHaveProperty('response_format');
    });

    it('keeps the advanced passthrough collapsed until asked for', async () => {
      await renderSpeechThread();
      openSettings();

      const toggle = screen.getByRole('button', { name: t.chatSpeechAdvanced });
      expect(toggle).toHaveAttribute('aria-expanded', 'false');
      expect(screen.queryByLabelText(t.chatSpeechPassthrough)).not.toBeInTheDocument();

      fireEvent.click(toggle);
      expect(toggle).toHaveAttribute('aria-expanded', 'true');
      const field = screen.getByLabelText(t.chatSpeechPassthrough);
      expect(field.tagName).toBe('TEXTAREA');
      expect(field).toHaveValue('');
      expect(field).toHaveAccessibleDescription(t.chatSpeechPassthroughHint);

      fireEvent.click(toggle);
      expect(screen.queryByLabelText(t.chatSpeechPassthrough)).not.toBeInTheDocument();
    });

    it('sends the voice, the speed and the parsed passthrough in the started run', async () => {
      await renderSpeechThread();
      openSettings();
      fireEvent.change(screen.getByRole('combobox', { name: t.chatSpeechVoice }), {
        target: { value: 'bf_emma' },
      });
      fireEvent.change(screen.getByRole('spinbutton', { name: t.chatSpeechSpeed }), {
        target: { value: '0.8' },
      });
      fireEvent.click(screen.getByRole('button', { name: t.chatSpeechAdvanced }));
      fireEvent.change(screen.getByLabelText(t.chatSpeechPassthrough), {
        target: { value: '{"instructions": "Speak slowly.", "seed": 1}' },
      });
      await sendSpeech();

      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalled());
      const settings = startedSettings();
      expect(settings.kind).toBe('speech');
      // The numeric field is parsed to a wire number; the passthrough keeps each value's
      // JSON type.
      expect(settings.audio).toEqual({
        voice: 'bf_emma',
        speed: 0.8,
        passthrough: { instructions: 'Speak slowly.', seed: 1 },
      });
      expect(settings).not.toHaveProperty('image');
    });

    it('sends no audio knob that was left blank', async () => {
      await renderSpeechThread();
      await sendSpeech();

      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalled());
      expect(startedSettings().audio).toEqual({});
    });

    it('seeds the fields from a saved speech thread', async () => {
      chatApi.rows.push({
        id: 'c_speech_seed',
        title: '',
        created_at: '2026-07-22T12:00:00Z',
        updated_at: '2026-07-22T12:00:00Z',
        content: {
          settings: {
            model: 'kokoro',
            kind: 'speech',
            audio: { voice: 'bf_emma', speed: 0.8, passthrough: { seed: 1 } },
          },
          messages: [],
        },
      });
      renderChat(tokens, speechModels);
      await waitForChatReady();
      openSettings();

      expect(screen.getByRole('combobox', { name: t.chatSpeechVoice })).toHaveValue('bf_emma');
      expect(screen.getByRole('spinbutton', { name: t.chatSpeechSpeed })).toHaveValue(0.8);
      fireEvent.click(screen.getByRole('button', { name: t.chatSpeechAdvanced }));
      expect(screen.getByLabelText(t.chatSpeechPassthrough)).toHaveValue(
        JSON.stringify({ seed: 1 }, null, 2),
      );
    });

    // The voice field is a combobox over the voices the gateway has cached for the picked
    // model (api.modelVoices), and it stays FREE TEXT: the cache is only what the health
    // loop has seen, so a voice it never saw may still be typed. No voices (none cached, or
    // the lookup failed) is not an error -- the field is then just the free-text field it
    // was before voices were discoverable.
    describe('the voice picker', () => {
      const voices = ['af_heart', 'bf_emma', 'bm_george'];

      // The speech thread with the model's voices fetched (or the fetch refused) and the
      // settings open; returns the voice combobox. A non-empty list is awaited through the
      // hint that only a known list shows, so the test never races the fetch.
      async function renderPicker(fetched: string[] | Error = voices) {
        if (fetched instanceof Error) chatApi.spies.modelVoices.mockRejectedValue(fetched);
        else chatApi.spies.modelVoices.mockResolvedValue(fetched);
        await renderSpeechThread();
        await waitFor(() => expect(chatApi.spies.modelVoices).toHaveBeenCalledWith('kokoro'));
        openSettings();
        if (Array.isArray(fetched) && fetched.length > 0) {
          await screen.findByText(t.chatSpeechVoiceHint);
        } else {
          await flushMicrotasks();
        }
        return screen.getByRole('combobox', { name: t.chatSpeechVoice });
      }

      const openList = (voice: HTMLElement) => fireEvent.mouseDown(voice);
      const offered = () => screen.queryAllByRole('option').map((option) => option.textContent);

      async function expectSentVoice(expected: Record<string, unknown>) {
        await sendSpeech();
        await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalled());
        expect(startedSettings().audio).toEqual(expected);
      }

      it('asks for the voices of the picked speech model', async () => {
        await renderPicker();

        expect(chatApi.spies.modelVoices).toHaveBeenCalledTimes(1);
        expect(chatApi.spies.modelVoices).toHaveBeenCalledWith('kokoro');
      });

      it('lists the fetched voices and tells the user a name may be typed too', async () => {
        const voice = await renderPicker();

        expect(screen.getByText(t.chatSpeechVoiceHint)).toBeInTheDocument();
        openList(voice);
        await screen.findByRole('option', { name: 'af_heart' });
        expect(offered()).toEqual(voices);
      });

      it('sends a picked voice in the started run', async () => {
        const voice = await renderPicker();
        openList(voice);
        fireEvent.click(await screen.findByRole('option', { name: 'bf_emma' }));

        expect(voice).toHaveValue('bf_emma');
        await expectSentVoice({ voice: 'bf_emma' });
      });

      it('narrows the list to what is typed', async () => {
        const voice = await renderPicker();
        openList(voice);
        await screen.findByRole('option', { name: 'af_heart' });

        fireEvent.change(voice, { target: { value: 'b' } });

        expect(offered()).toEqual(['bf_emma', 'bm_george']);
      });

      it('accepts a typed voice the list does not hold, and sends it unchanged', async () => {
        const voice = await renderPicker();

        fireEvent.change(voice, { target: { value: 'my_custom_voice' } });
        expect(voice).toHaveValue('my_custom_voice');
        // Nothing matches, and the field offers nothing instead of refusing the name...
        expect(offered()).toEqual([]);
        expect(screen.queryByText(/no options/i)).not.toBeInTheDocument();
        // ...nor does leaving the field drop it.
        fireEvent.blur(voice);
        expect(voice).toHaveValue('my_custom_voice');

        await expectSentVoice({ voice: 'my_custom_voice' });
      });

      it('keeps the typed text on Enter instead of choosing a match', async () => {
        const voice = await renderPicker();
        openList(voice);
        await screen.findByRole('option', { name: 'bf_emma' });

        // "b" matches two voices, yet Enter commits the name the user typed: it is the
        // name that is kept, and sent.
        fireEvent.change(voice, { target: { value: 'b' } });
        fireEvent.keyDown(voice, { key: 'Enter' });

        expect(voice).toHaveValue('b');
        await expectSentVoice({ voice: 'b' });
      });

      it('keeps what was typed while the voices were still loading', async () => {
        let release: (v: string[]) => void = () => {};
        chatApi.spies.modelVoices.mockImplementation(
          () =>
            new Promise<string[]>((resolve) => {
              release = resolve;
            }),
        );
        await renderSpeechThread();
        await waitFor(() => expect(chatApi.spies.modelVoices).toHaveBeenCalledWith('kokoro'));
        openSettings();
        const voice = screen.getByRole('combobox', { name: t.chatSpeechVoice });

        // The list is not here yet: the field is free text, and the user types.
        fireEvent.change(voice, { target: { value: 'my_voice' } });
        await act(async () => {
          release(voices);
        });
        await screen.findByText(t.chatSpeechVoiceHint);

        // The late list neither overwrites the typed name nor drops it.
        expect(voice).toHaveValue('my_voice');
        await expectSentVoice({ voice: 'my_voice' });
      });

      it('shows the dropdown arrow only while there is a list to open', async () => {
        // Scoped to the voice field: the model picker has an arrow of its own.
        const arrowOf = (voice: HTMLElement) =>
          within(voice.closest('.MuiAutocomplete-root') as HTMLElement).queryByRole('button', {
            name: 'Open',
          });

        expect(arrowOf(await renderPicker())).toBeInTheDocument();

        cleanup();
        chatApi = makeChatApi();
        expect(arrowOf(await renderPicker([]))).not.toBeInTheDocument();
      });

      it('sends no voice once the field is cleared again', async () => {
        const voice = await renderPicker();
        openList(voice);
        fireEvent.click(await screen.findByRole('option', { name: 'bf_emma' }));

        fireEvent.change(voice, { target: { value: '' } });

        expect(voice).toHaveValue('');
        await expectSentVoice({});
      });

      it('keeps a saved voice the fetched list does not hold, and still sends it', async () => {
        chatApi.rows.push({
          id: 'c_speech_saved_voice',
          title: '',
          created_at: '2026-07-22T12:00:00Z',
          updated_at: '2026-07-22T12:00:00Z',
          content: {
            settings: { model: 'kokoro', kind: 'speech', audio: { voice: 'retired_voice' } },
            messages: [],
          },
        });
        chatApi.spies.modelVoices.mockResolvedValue(voices);
        renderChat(tokens, speechModels);
        await waitForChatReady();
        openSettings();
        await screen.findByText(t.chatSpeechVoiceHint);

        // The pick the list omits stays in the field -- not blanked, not replaced by the
        // first voice -- while the list still offers everything the model has.
        const voice = screen.getByRole('combobox', { name: t.chatSpeechVoice });
        expect(voice).toHaveValue('retired_voice');
        openList(voice);
        await screen.findByRole('option', { name: 'af_heart' });
        expect(offered()).toEqual(voices);
        expect(voice).toHaveValue('retired_voice');

        fireEvent.keyDown(voice, { key: 'Escape' });
        await expectSentVoice({ voice: 'retired_voice' });
      });

      it.each([
        ['has no cached voices', [] as string[]],
        ['cannot be asked for them', new Error('voices unavailable')],
      ])('is the plain free-text field when the model %s', async (_case, fetched) => {
        const voice = await renderPicker(fetched);

        // Nothing to pick from, and no claim that there is.
        expect(screen.queryByText(t.chatSpeechVoiceHint)).not.toBeInTheDocument();
        openList(voice);
        expect(offered()).toEqual([]);
        expect(screen.queryByText(/no options/i)).not.toBeInTheDocument();

        fireEvent.change(voice, { target: { value: 'bf_emma' } });
        expect(voice).toHaveValue('bf_emma');
        await expectSentVoice({ voice: 'bf_emma' });
      });
    });

    // Streaming playback (ADR-068): the toggle exists only where the gateway says the picked
    // model streams AND its rate is known (api.modelSpeechStreaming), is on by default, and
    // writes only its opt-out into the run's settings.
    describe('the streaming toggle', () => {
      const open: SpeechStreamingGate = { streaming: true, sampleRate: 44100 };
      const toggle = () => screen.queryByRole('checkbox', { name: t.chatSpeechStream });

      async function renderStreamingThread(gate: SpeechStreamingGate) {
        chatApi.spies.modelSpeechStreaming.mockResolvedValue(gate);
        await renderSpeechThread();
        await waitFor(() =>
          expect(chatApi.spies.modelSpeechStreaming).toHaveBeenCalledWith('kokoro'),
        );
        openSettings();
        await flushMicrotasks();
      }

      it('is offered, on, for a model the gateway says streams', async () => {
        await renderStreamingThread(open);

        const box = await screen.findByRole('checkbox', { name: t.chatSpeechStream });
        expect(box).toBeChecked();
        expect(box).toHaveAccessibleDescription(t.chatSpeechStreamHint);
      });

      it('is not offered for a model that cannot be streamed (or not yet known to)', async () => {
        await renderStreamingThread(NO_SPEECH_STREAMING);

        expect(toggle()).not.toBeInTheDocument();
        expect(screen.queryByText(t.chatSpeechStreamHint)).not.toBeInTheDocument();
      });

      it('is not offered on a text thread', async () => {
        chatApi.spies.modelSpeechStreaming.mockResolvedValue(open);
        renderChat();
        await waitForChatReady();
        await pickOption(t.chatModel, models[0].display_name);
        openSettings();
        await flushMicrotasks();

        expect(toggle()).not.toBeInTheDocument();
        expect(chatApi.spies.modelSpeechStreaming).not.toHaveBeenCalled();
      });

      it('writes nothing for streaming left on (the default)', async () => {
        await renderStreamingThread(open);
        await screen.findByRole('checkbox', { name: t.chatSpeechStream });

        await sendSpeech();
        await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
        expect(startedSettings().audio).not.toHaveProperty('stream');
      });

      it('sends the opt-out once it is switched off', async () => {
        await renderStreamingThread(open);
        fireEvent.click(await screen.findByRole('checkbox', { name: t.chatSpeechStream }));
        expect(toggle()).not.toBeChecked();

        await sendSpeech();
        await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
        expect(startedSettings().audio).toEqual({ stream: false });
      });

      it('is locked while a run is in flight, like the other settings', async () => {
        await renderStreamingThread(open);
        await screen.findByRole('checkbox', { name: t.chatSpeechStream });

        await sendSpeech();
        await waitFor(() => expect(toggle()).toBeDisabled());
      });
    });

    // The language control (settings.audio.language): a free-text field (any code may be typed;
    // the gateway does not say which a model accepts) that exists only where the gateway says
    // the picked model takes a `language` (api.modelSpeechLanguage). An empty thread starts on
    // the portal's UI language unless the model declares it detects the language itself
    // ("auto"); the user can change or clear it, and a blank one is never written.
    describe('the language control', () => {
      const field = () => screen.queryByRole('textbox', { name: t.chatSpeechLanguage });
      const takes = (def: string): SpeechLanguageGate => ({ accepts: true, default: def });

      async function renderLanguageThread(gate: SpeechLanguageGate | Error) {
        if (gate instanceof Error) chatApi.spies.modelSpeechLanguage.mockRejectedValue(gate);
        else chatApi.spies.modelSpeechLanguage.mockResolvedValue(gate);
        await renderSpeechThread();
        await waitFor(() =>
          expect(chatApi.spies.modelSpeechLanguage).toHaveBeenCalledWith('kokoro'),
        );
        openSettings();
        await flushMicrotasks();
      }

      async function expectSentAudio(expected: Record<string, unknown>) {
        await sendSpeech();
        await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
        expect(startedSettings().audio).toEqual(expected);
      }

      it('is offered, blank, for a model the gateway says takes a language', async () => {
        await renderLanguageThread(takes('auto'));

        const language = await screen.findByRole('textbox', { name: t.chatSpeechLanguage });
        expect(language).toHaveValue('');
        // The placeholder says "model default" -- a hint, never written.
        expect(language).toHaveAttribute('placeholder', t.chatSpeechLanguageDefault);
        expect(language).toHaveAccessibleDescription(t.chatSpeechLanguageHint);
      });

      it('is absent for a model the gateway says takes none', async () => {
        await renderLanguageThread(NO_SPEECH_LANGUAGE);

        expect(field()).not.toBeInTheDocument();
        // The rest of the speech settings are untouched.
        expect(screen.getByRole('combobox', { name: t.chatSpeechVoice })).toBeInTheDocument();
      });

      it('is absent when the gateway cannot be asked', async () => {
        await renderLanguageThread(new Error('language unavailable'));

        expect(field()).not.toBeInTheDocument();
      });

      it('is not offered on a text thread', async () => {
        chatApi.spies.modelSpeechLanguage.mockResolvedValue(takes('zh'));
        renderChat();
        await waitForChatReady();
        await pickOption(t.chatModel, models[0].display_name);
        openSettings();
        await flushMicrotasks();

        expect(field()).not.toBeInTheDocument();
        expect(chatApi.spies.modelSpeechLanguage).not.toHaveBeenCalled();
      });

      it('sets settings.audio.language in the started run when a code is typed', async () => {
        await renderLanguageThread(takes('auto'));
        const language = await screen.findByRole('textbox', { name: t.chatSpeechLanguage });

        fireEvent.change(language, { target: { value: 'fr' } });

        expect(language).toHaveValue('fr');
        await expectSentAudio({ language: 'fr' });
      });

      it('pre-fills the portal UI language when the model declares a default other than auto', async () => {
        await renderLanguageThread(takes('zh'));

        const language = await screen.findByRole('textbox', { name: t.chatSpeechLanguage });
        await waitFor(() => expect(language).toHaveValue(locale));
        await expectSentAudio({ language: locale });
      });

      it('starts blank, and sends no language, when the model declares auto', async () => {
        await renderLanguageThread(takes('auto'));

        const language = await screen.findByRole('textbox', { name: t.chatSpeechLanguage });
        expect(language).toHaveValue('');
        await expectSentAudio({});
      });

      it('sends no language once the pre-filled field is cleared', async () => {
        await renderLanguageThread(takes('zh'));
        const language = await screen.findByRole('textbox', { name: t.chatSpeechLanguage });
        await waitFor(() => expect(language).toHaveValue(locale));

        fireEvent.change(language, { target: { value: '' } });

        expect(language).toHaveValue('');
        await expectSentAudio({});
        // Cleared means absent, not an empty string on the wire.
        expect(startedSettings().audio).not.toHaveProperty('language');
      });

      it('keeps a saved language the gateway would pre-fill over', async () => {
        chatApi.rows.push({
          id: 'c_speech_saved_language',
          title: '',
          created_at: '2026-07-22T12:00:00Z',
          updated_at: '2026-07-22T12:00:00Z',
          content: {
            settings: { model: 'kokoro', kind: 'speech', audio: { language: 'fr' } },
            messages: [],
          },
        });
        chatApi.spies.modelSpeechLanguage.mockResolvedValue(takes('zh'));
        renderChat(tokens, speechModels);
        await waitForChatReady();
        openSettings();

        const language = await screen.findByRole('textbox', { name: t.chatSpeechLanguage });
        await flushMicrotasks();
        expect(language).toHaveValue('fr');
        await expectSentAudio({ language: 'fr' });
      });

      it('is locked while a run is in flight, like the other settings', async () => {
        await renderLanguageThread(takes('auto'));
        await screen.findByRole('textbox', { name: t.chatSpeechLanguage });

        await sendSpeech();
        await waitFor(() => expect(field()).toBeDisabled());
      });
    });

    // Answer-audio retention (#281): a checkbox ("keep audio answers", on by default) over a
    // keep-all / keep-last-N radio whose N field exists only for "keep last N". Unchecking is
    // "do not store" and is only selectable while the thread streams (the same gate as the
    // streaming toggle): a buffered answer is only ever heard from storage. The control
    // shows what is stored and never writes on its own.
    describe('the audio retention control', () => {
      const open: SpeechStreamingGate = { streaming: true, sampleRate: 44100 };
      const keepBox = () => screen.getByRole('checkbox', { name: t.chatAudioKeep });
      const keepAllRadio = () => screen.getByRole('radio', { name: t.chatAudioKeepAll });
      const keepLastRadio = () => screen.getByRole('radio', { name: t.chatAudioKeepLast });
      const countField = () =>
        screen.queryByRole('spinbutton', { name: t.chatAudioKeepLastCount }) as HTMLInputElement;
      const typeCount = (value: string) => fireEvent.change(countField(), { target: { value } });

      // The speech thread with its settings panel open. With an open gate it waits for the
      // streaming toggle, which only exists once the gate's answer has landed.
      async function renderRetentionThread(gate: SpeechStreamingGate = NO_SPEECH_STREAMING) {
        chatApi.spies.modelSpeechStreaming.mockResolvedValue(gate);
        await renderSpeechThread();
        await waitFor(() =>
          expect(chatApi.spies.modelSpeechStreaming).toHaveBeenCalledWith('kokoro'),
        );
        openSettings();
        await flushMicrotasks();
        if (gate.streaming) await screen.findByRole('checkbox', { name: t.chatSpeechStream });
      }

      // A saved speech thread, opened with its settings panel.
      async function renderSavedThread(audio: Record<string, unknown>, gate = open) {
        chatApi.spies.modelSpeechStreaming.mockResolvedValue(gate);
        chatApi.rows.push({
          id: 'c_speech_keep',
          title: '',
          created_at: '2026-07-22T12:00:00Z',
          updated_at: '2026-07-22T12:00:00Z',
          content: { settings: { model: 'kokoro', kind: 'speech', audio }, messages: [] },
        });
        renderChat(tokens, speechModels);
        await waitForChatReady();
        await waitFor(() =>
          expect(chatApi.spies.modelSpeechStreaming).toHaveBeenCalledWith('kokoro'),
        );
        openSettings();
        await flushMicrotasks();
      }

      it('offers the checkbox on, with keep-all chosen and no count field', async () => {
        await renderRetentionThread();

        expect(keepBox()).toBeChecked();
        expect(keepAllRadio()).toBeChecked();
        expect(keepLastRadio()).not.toBeChecked();
        expect(countField()).not.toBeInTheDocument();
      });

      it('is not offered on a text thread', async () => {
        renderChat();
        await waitForChatReady();
        await pickOption(t.chatModel, models[0].display_name);
        openSettings();

        expect(screen.queryByRole('checkbox', { name: t.chatAudioKeep })).not.toBeInTheDocument();
        expect(screen.queryByRole('radio', { name: t.chatAudioKeepAll })).not.toBeInTheDocument();
      });

      it('reveals a whole-number count of at least 1 for keep-last-N only', async () => {
        await renderRetentionThread();
        fireEvent.click(keepLastRadio());

        expect(keepLastRadio()).toBeChecked();
        expect(keepAllRadio()).not.toBeChecked();
        expect(countField()).toHaveAttribute('type', 'number');
        expect(countField()).toHaveAttribute('min', '1');
        expect(countField()).toHaveAttribute('step', '1');
        expect(countField()).toHaveValue(null); // blank until the user types one

        fireEvent.click(keepAllRadio());
        expect(countField()).not.toBeInTheDocument();
      });

      it('sends the typed count as keep_last_audio', async () => {
        await renderRetentionThread();
        fireEvent.click(keepLastRadio());
        typeCount('3');
        expect(countField()).toHaveValue(3);

        await sendSpeech();
        await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
        expect(startedSettings().audio).toEqual({ keep_last_audio: 3 });
      });

      it('sends nothing for keep-all, and choosing it clears a typed count', async () => {
        await renderRetentionThread();
        fireEvent.click(keepLastRadio());
        typeCount('3');
        fireEvent.click(keepAllRadio());
        expect(keepAllRadio()).toBeChecked();

        // Back to keep-last-N: the count is gone, not remembered.
        fireEvent.click(keepLastRadio());
        expect(countField()).toHaveValue(null);
        fireEvent.click(keepAllRadio());

        await sendSpeech();
        await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
        expect(startedSettings().audio).not.toHaveProperty('keep_last_audio');
      });

      it('keeps keep-last-N chosen while the count is being retyped from empty', async () => {
        await renderRetentionThread();
        fireEvent.click(keepLastRadio());
        typeCount('3');
        typeCount(''); // the last digit deleted: mid-edit, not a change of mind

        expect(keepLastRadio()).toBeChecked();
        expect(countField()).toBeInTheDocument();
        typeCount('7');
        expect(countField()).toHaveValue(7);
      });

      // An unusable count otherwise falls through to keep-all in silence, so the field says
      // so, inline, and says what happens meanwhile. A blank field is not an error (it is
      // simply not filled in yet), and a fixed one clears the message.
      it('flags a count that is not a whole number of at least 1, and clears once fixed', async () => {
        await renderSpeechThread();
        openSettings();
        fireEvent.click(keepLastRadio());

        for (const bad of ['0', '-2', '1.5']) {
          typeCount(bad);
          expect(countField(), bad).toHaveAttribute('aria-invalid', 'true');
          expect(countField(), bad).toHaveAccessibleDescription(t.chatAudioKeepLastInvalid);
        }

        typeCount('4');
        expect(countField()).not.toHaveAttribute('aria-invalid', 'true');
        expect(screen.queryByText(t.chatAudioKeepLastInvalid)).not.toBeInTheDocument();

        typeCount('0');
        expect(screen.getByText(t.chatAudioKeepLastInvalid)).toBeInTheDocument();
        typeCount('');
        expect(countField()).not.toHaveAttribute('aria-invalid', 'true');
        expect(screen.queryByText(t.chatAudioKeepLastInvalid)).not.toBeInTheDocument();
      });

      it('sends no count while the typed one is unusable', async () => {
        await renderRetentionThread();
        fireEvent.click(keepLastRadio());
        typeCount('0');

        await sendSpeech();
        await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
        expect(startedSettings().audio).not.toHaveProperty('keep_last_audio');
      });

      it('cannot be switched off on a thread that does not stream, and says why', async () => {
        await renderRetentionThread(NO_SPEECH_STREAMING);

        expect(keepBox()).toBeChecked();
        expect(keepBox()).toBeDisabled();
        expect(keepBox()).toHaveAccessibleDescription(t.chatAudioKeepHint);
        // The rest of the control still works: only "do not store" needs the stream.
        fireEvent.click(keepLastRadio());
        expect(countField()).toBeInTheDocument();
      });

      it('can be switched off on a streaming thread, which hides keep-all/keep-last-N', async () => {
        await renderRetentionThread(open);
        expect(keepBox()).toBeEnabled();
        expect(keepBox()).toHaveAccessibleDescription(t.chatAudioKeepHint);

        fireEvent.click(keepBox());
        expect(keepBox()).not.toBeChecked();
        expect(screen.queryByRole('radio', { name: t.chatAudioKeepAll })).not.toBeInTheDocument();
        expect(countField()).not.toBeInTheDocument();

        await sendSpeech();
        await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
        expect(startedSettings().audio).toEqual({ keep_last_audio: 0 });
      });

      it('switches back on to the choice it had', async () => {
        await renderRetentionThread(open);
        fireEvent.click(keepLastRadio());
        typeCount('5');
        fireEvent.click(keepBox());
        fireEvent.click(keepBox());

        expect(keepBox()).toBeChecked();
        expect(keepLastRadio()).toBeChecked();
        expect(countField()).toHaveValue(5);
      });

      it('is locked while a run is in flight, like the other settings', async () => {
        await renderRetentionThread(open);
        fireEvent.click(keepLastRadio());
        typeCount('2');

        await sendSpeech();
        await waitFor(() => expect(keepBox()).toBeDisabled());
        expect(keepAllRadio()).toBeDisabled();
        expect(keepLastRadio()).toBeDisabled();
        expect(countField()).toBeDisabled();
      });

      it('shows a stored keep-last-N, and writes nothing on load', async () => {
        await renderSavedThread({ keep_last_audio: 3 });

        expect(keepBox()).toBeChecked();
        expect(keepLastRadio()).toBeChecked();
        expect(countField()).toHaveValue(3);

        // The save is debounced (800 ms): wait it out, and nothing was written.
        await new Promise((resolve) => setTimeout(resolve, 1000));
        expect(chatApi.spies.saveChat).not.toHaveBeenCalled();
      });

      it('shows a stored do-not-store, and writes nothing on load', async () => {
        await renderSavedThread({ keep_last_audio: 0 });

        expect(keepBox()).not.toBeChecked();
        expect(screen.queryByRole('radio', { name: t.chatAudioKeepAll })).not.toBeInTheDocument();

        await new Promise((resolve) => setTimeout(resolve, 1000));
        expect(chatApi.spies.saveChat).not.toHaveBeenCalled();
      });

      // The model may have stopped streaming since: the stored "do not store" shows as it is
      // (the run stores a buffered answer anyway), and only SELECTING it is withheld, so the
      // checkbox stays free to be switched back on but never off again.
      it('shows a stored do-not-store as is on a thread that no longer streams', async () => {
        await renderSavedThread({ keep_last_audio: 0 }, NO_SPEECH_STREAMING);

        expect(keepBox()).not.toBeChecked();
        expect(keepBox()).toBeEnabled();

        fireEvent.click(keepBox());
        expect(keepBox()).toBeChecked();
        expect(keepBox()).toBeDisabled();
      });

      // The positive control of the two "writes nothing" tests: an edit IS saved, so their
      // silence means the control stayed quiet and not that the save never runs here.
      it('saves an edit', async () => {
        await renderSavedThread({ keep_last_audio: 3 });
        typeCount('5');

        await waitFor(() => expect(chatApi.spies.saveChat).toHaveBeenCalled(), { timeout: 3000 });
        const saved = chatApi.spies.saveChat.mock.calls[0][1] as {
          content: { settings: { audio: unknown } };
        };
        expect(saved.content.settings.audio).toEqual({ keep_last_audio: 5 });
      });
    });

    // Tightening the retention of a chat that already holds more audio deletes that audio for
    // good, so the composer asks first (#281): the stricter value waits beside the control,
    // shown but neither saved nor sent, until it is applied (which deletes the excess) or
    // discarded (which puts the control back and touches nothing). The value is held at once;
    // only the notice that asks about it waits for the value to stand still (500 ms), so an
    // interim digit on the way to a larger count is never announced as a deletion.
    describe('the confirm of a retention that deletes stored audio', () => {
      const open: SpeechStreamingGate = { streaming: true, sampleRate: 44100 };
      const wav = { type: 'audio_url', audio_url: { url: 'data:audio/wav;base64,UklG' } };
      const answers = [
        { id: 'u1', role: 'user', content: 'one' },
        { id: 'a1', role: 'assistant', content: [wav], status: 'complete' },
        { id: 'u2', role: 'user', content: 'two' },
        { id: 'a2', role: 'assistant', content: [wav], status: 'complete' },
        { id: 'u3', role: 'user', content: 'three' },
        { id: 'a3', role: 'assistant', content: [wav], status: 'complete' },
      ];
      const keepBox = () => screen.getByRole('checkbox', { name: t.chatAudioKeep });
      const keepLastRadio = () => screen.getByRole('radio', { name: t.chatAudioKeepLast });
      const countField = () =>
        screen.getByRole('spinbutton', { name: t.chatAudioKeepLastCount }) as HTMLInputElement;
      const typeCount = (value: string) => fireEvent.change(countField(), { target: { value } });
      const applyButton = () => screen.getByRole('button', { name: t.chatAudioPruneConfirm });
      const discardButton = () => screen.getByRole('button', { name: t.chatAudioPruneDiscard });
      const notice = () => screen.queryByRole('alert');
      // The notice shows once the held value has stood still for its settle time.
      const noticeAfterSettle = () => screen.findByRole('alert', {}, { timeout: 3000 });
      // Waits out the settle time, for the tests that assert the notice does NOT come.
      const outwaitSettle = () =>
        act(async () => {
          await new Promise((resolve) => setTimeout(resolve, 800));
        });
      // Records the text of every alert that is inserted from now on, which is what a screen
      // reader would read out, so "never shown" holds for a notice that came and went too.
      function recordAnnouncements(): { texts: string[]; stop: () => void } {
        const texts: string[] = [];
        const observer = new MutationObserver((records) => {
          for (const added of records.flatMap((r) => [...r.addedNodes])) {
            if (!(added instanceof HTMLElement)) continue;
            const alerts = added.matches('[role="alert"]')
              ? [added]
              : [...added.querySelectorAll('[role="alert"]')];
            texts.push(...alerts.map((a) => a.textContent ?? ''));
          }
        });
        observer.observe(document.body, { childList: true, subtree: true });
        return { texts, stop: () => observer.disconnect() };
      }

      // A saved speech thread of three spoken answers, opened with its settings panel.
      async function renderThread(audio: Record<string, unknown> = {}) {
        chatApi.spies.modelSpeechStreaming.mockResolvedValue(open);
        chatApi.rows.push({
          id: 'c_prune',
          title: '',
          created_at: '2026-07-22T12:00:00Z',
          updated_at: '2026-07-22T12:00:00Z',
          content: { settings: { model: 'kokoro', kind: 'speech', audio }, messages: answers },
        });
        renderChat(tokens, speechModels);
        await waitForChatReady();
        await waitFor(() =>
          expect(chatApi.spies.modelSpeechStreaming).toHaveBeenCalledWith('kokoro'),
        );
        openSettings();
        await flushMicrotasks();
      }

      it('asks, naming how many answers lose their audio, before keeping fewer', async () => {
        await renderThread();
        expect(notice()).not.toBeInTheDocument();

        fireEvent.click(keepLastRadio());
        typeCount('2');
        // The value is held at once, in its field, though the question is not asked yet.
        expect(countField()).toHaveValue(2);
        expect(notice()).not.toBeInTheDocument();

        expect(await noticeAfterSettle()).toHaveTextContent(t.chatAudioPrunePending(2, 1));
        expect(applyButton()).toBeEnabled();
        expect(discardButton()).toBeEnabled();
        // What was asked for stays in its field, so a count can be typed on.
        expect(countField()).toHaveValue(2);
        expect(chatApi.spies.pruneChatAudio).not.toHaveBeenCalled();
        // Nothing was saved either (the save is debounced: wait it out).
        await new Promise((resolve) => setTimeout(resolve, 1000));
        expect(chatApi.spies.saveChat).not.toHaveBeenCalled();
        expect(screen.queryByText(t.chatAudioNotStored)).not.toBeInTheDocument();
      });

      it('deletes the excess and keeps the new count once it is applied', async () => {
        await renderThread();
        fireEvent.click(keepLastRadio());
        typeCount('2');
        await noticeAfterSettle();
        // The held value waited for this click: nothing has been deleted before it.
        expect(chatApi.spies.pruneChatAudio).not.toHaveBeenCalled();

        fireEvent.click(applyButton());

        await waitFor(() =>
          expect(chatApi.spies.pruneChatAudio).toHaveBeenCalledWith('c_prune', 2),
        );
        // The oldest answer is now the not-stored placeholder; the two newest keep a player.
        expect(await screen.findByText(t.chatAudioNotStored)).toBeInTheDocument();
        expect(screen.getAllByText(t.chatAudioNotStored)).toHaveLength(1);
        await waitFor(() => expect(notice()).not.toBeInTheDocument());
        expect(countField()).toHaveValue(2);
        await waitFor(() => expect(chatApi.spies.saveChat).toHaveBeenCalled(), { timeout: 3000 });
        const saved = chatApi.spies.saveChat.mock.calls.at(-1)![1] as {
          content: { settings: { audio: unknown } };
        };
        expect(saved.content.settings.audio).toEqual({ keep_last_audio: 2 });
      });

      it('puts the control back and deletes and saves nothing when it is discarded', async () => {
        await renderThread({ keep_last_audio: 3 });
        expect(countField()).toHaveValue(3);

        typeCount('1');
        expect(await noticeAfterSettle()).toHaveTextContent(t.chatAudioPrunePending(1, 2));
        fireEvent.click(discardButton());

        expect(notice()).not.toBeInTheDocument();
        expect(countField()).toHaveValue(3);
        await new Promise((resolve) => setTimeout(resolve, 1000));
        expect(chatApi.spies.pruneChatAudio).not.toHaveBeenCalled();
        expect(chatApi.spies.saveChat).not.toHaveBeenCalled();
        expect(screen.queryByText(t.chatAudioNotStored)).not.toBeInTheDocument();
      });

      it('asks before "do not store" removes every answer audio, and switches it off when applied', async () => {
        await renderThread();

        fireEvent.click(keepBox());

        expect(keepBox()).not.toBeChecked();
        expect(await noticeAfterSettle()).toHaveTextContent(t.chatAudioPrunePending(0, 3));
        expect(chatApi.spies.pruneChatAudio).not.toHaveBeenCalled();

        fireEvent.click(applyButton());
        await waitFor(() =>
          expect(chatApi.spies.pruneChatAudio).toHaveBeenCalledWith('c_prune', 0),
        );
        await waitFor(() => expect(screen.getAllByText(t.chatAudioNotStored)).toHaveLength(3));
        expect(keepBox()).not.toBeChecked();
      });

      it('switches the box back on when "do not store" is discarded', async () => {
        await renderThread();
        fireEvent.click(keepBox());
        await noticeAfterSettle();

        fireEvent.click(discardButton());

        expect(keepBox()).toBeChecked();
        expect(notice()).not.toBeInTheDocument();
        expect(chatApi.spies.pruneChatAudio).not.toHaveBeenCalled();
      });

      it('does not ask for a loosening, or for a count the stored audio does not exceed', async () => {
        await renderThread({ keep_last_audio: 2 });

        typeCount('5');
        await outwaitSettle();

        expect(notice()).not.toBeInTheDocument();
        expect(countField()).toHaveValue(5);
        typeCount('3');
        await outwaitSettle();
        expect(notice()).not.toBeInTheDocument(); // three answers, a count of three
        expect(chatApi.spies.pruneChatAudio).not.toHaveBeenCalled();
      });

      // Each of the four values is outwaited for real (outwaitSettle), so the body spends
      // about 3.2 s on timers alone, which is too close to vitest's 5 s default under CI or
      // coverage load; the generous limit is only headroom, every value is still waited out.
      it('does not ask while the count is blank or not a whole number', async () => {
        await renderThread();
        fireEvent.click(keepLastRadio());

        for (const text of ['abc', '1.5', '0', '']) {
          typeCount(text);
          await outwaitSettle();
          expect(notice(), JSON.stringify(text)).not.toBeInTheDocument();
        }
        expect(chatApi.spies.pruneChatAudio).not.toHaveBeenCalled();
      }, 15000);

      it('never raises the notice for a stricter first digit that is made looser at once', async () => {
        await renderThread();
        fireEvent.click(keepLastRadio());
        const announced = recordAnnouncements();

        typeCount('1');
        // Held at once: the field shows it, but the question is not asked for a passing digit.
        expect(countField()).toHaveValue(1);
        expect(notice()).not.toBeInTheDocument();
        typeCount('10');
        await outwaitSettle();
        announced.stop();

        expect(announced.texts).toEqual([]);
        expect(notice()).not.toBeInTheDocument();
        expect(countField()).toHaveValue(10);
        expect(chatApi.spies.pruneChatAudio).not.toHaveBeenCalled();
        // The looser 10 is an ordinary edit and is saved; the passing 1 never was.
        await waitFor(() => expect(chatApi.spies.saveChat).toHaveBeenCalled(), { timeout: 3000 });
        for (const call of chatApi.spies.saveChat.mock.calls) {
          const saved = call[1] as { content: { settings: { audio: unknown } } };
          expect(saved.content.settings.audio).toEqual({ keep_last_audio: 10 });
        }
      });

      it('asks again, after the wait, when a count that went looser comes back stricter', async () => {
        await renderThread();
        fireEvent.click(keepLastRadio());

        typeCount('1');
        expect(await noticeAfterSettle()).toHaveTextContent(t.chatAudioPrunePending(1, 2));
        typeCount('10');
        expect(notice()).not.toBeInTheDocument();
        typeCount('1');
        // The same value as before, but it went away and has to stand still again.
        expect(notice()).not.toBeInTheDocument();

        expect(await noticeAfterSettle()).toHaveTextContent(t.chatAudioPrunePending(1, 2));
        expect(chatApi.spies.pruneChatAudio).not.toHaveBeenCalled();
      });
    });

    describe('an advanced passthrough that is not a JSON object', () => {
      it('is flagged on the field itself, and clears once fixed', async () => {
        await renderSpeechThread();
        const field = openPassthrough();

        fireEvent.change(field, { target: { value: '[1, 2]' } }); // valid JSON, but not an object
        expect(field).toBeInvalid();
        expect(field).toHaveAccessibleDescription(t.chatAudioPassthroughInvalid);

        fireEvent.change(field, { target: { value: '{"seed": 1}' } });
        expect(field).toBeValid();
        expect(field).toHaveAccessibleDescription(t.chatSpeechPassthroughHint);
      });

      // The backend ends such a run only after the turn is committed; the composer mirrors
      // the reserved list and says so inline, naming the field.
      it('flags a field the run owns, naming it, and opens the Advanced section for it', async () => {
        await renderSpeechThread();
        const field = openPassthrough();

        fireEvent.change(field, { target: { value: '{"seed": 1, "Model": "x"}' } });
        expect(field).toBeInvalid();
        expect(field).toHaveAccessibleDescription(t.chatSpeechPassthroughReserved('Model'));

        fireEvent.change(field, { target: { value: '{"seed": 1}' } });
        expect(field).toBeValid();
        expect(field).toHaveAccessibleDescription(t.chatSpeechPassthroughHint);
      });

      it('refuses a send for a field the run owns, with the field and panel held open', async () => {
        await renderSpeechThread();
        const field = openPassthrough();
        fireEvent.change(field, { target: { value: '{"stream": true}' } });

        // The user closes the whole panel, then sends.
        openSettings();
        await sendSpeech();

        expect(await screen.findByRole('alert')).toHaveTextContent(
          t.chatSpeechPassthroughReserved('stream'),
        );
        expect(chatApi.spies.startChatRun).not.toHaveBeenCalled();
        // Both toggles stay held open, so the toast points at a field that is on screen.
        expect(screen.getByLabelText(t.chatSpeechPassthrough)).toHaveValue('{"stream": true}');
        expect(screen.getByLabelText(t.chatSpeechPassthrough)).toBeInvalid();
      });

      it('does not flag a blank field or an empty object', async () => {
        await renderSpeechThread();
        const field = openPassthrough();

        expect(field).toBeValid();
        fireEvent.change(field, { target: { value: '{}' } });
        expect(field).toBeValid();
      });

      it('keeps the Advanced section open however it is toggled, so the refusal names a visible field', async () => {
        await renderSpeechThread();
        const field = openPassthrough();
        fireEvent.change(field, { target: { value: '{"seed": ' } }); // malformed

        // A collapse is refused while the text is invalid...
        fireEvent.click(screen.getByRole('button', { name: t.chatSpeechAdvanced }));
        expect(screen.getByLabelText(t.chatSpeechPassthrough)).toBeInTheDocument();

        // ...and the click left nothing behind: fixing the text (its last keystroke makes
        // it valid) does not collapse the field out from under the user.
        fireEvent.change(screen.getByLabelText(t.chatSpeechPassthrough), {
          target: { value: '{"seed": 1}' },
        });
        expect(screen.getByLabelText(t.chatSpeechPassthrough)).toBeInTheDocument();

        // From then on the toggle collapses it as usual.
        fireEvent.click(screen.getByRole('button', { name: t.chatSpeechAdvanced }));
        expect(screen.queryByLabelText(t.chatSpeechPassthrough)).not.toBeInTheDocument();
      });

      it('keeps the settings panel open too: the send refusal leaves the field in view', async () => {
        await renderSpeechThread();
        const field = openPassthrough();
        fireEvent.change(field, { target: { value: '{oops' } });

        // The user closes the whole panel, then sends.
        openSettings();
        await sendSpeech();

        // Refused, said so in the toast...
        expect(await screen.findByRole('alert')).toHaveTextContent(t.chatAudioPassthroughInvalid);
        expect(chatApi.spies.startChatRun).not.toHaveBeenCalled();
        // ...and the field that must be fixed is on screen, not hidden behind either toggle.
        expect(screen.getByLabelText(t.chatSpeechPassthrough)).toHaveValue('{oops');
        expect(screen.getByLabelText(t.chatSpeechPassthrough)).toBeInvalid();
      });

      // The store lives above the view switch, so the text outlives the Chat view while
      // the view's own open/closed state does not: coming back to the chat finds the
      // text still invalid with every section collapsed.
      it('is shown again when the chat view is left and re-entered with the text still invalid', async () => {
        function Harness() {
          const [shown, setShown] = useState(true);
          return (
            <>
              <button onClick={() => setShown((s) => !s)}>toggle-chat-view</button>
              {shown && <Chat t={t} />}
            </>
          );
        }
        render(
          <ToastProvider>
            <ChatStoreProvider
              api={chatApi.api}
              models={speechModels}
              tokens={tokens}
              servers={[]}
              onRefresh={vi.fn(async () => {})}
              t={t}
              locale={locale}
            >
              <Harness />
            </ChatStoreProvider>
          </ToastProvider>,
        );
        await waitForChatReady();
        await pickOption(t.chatModel, 'kokoro');
        fireEvent.change(openPassthrough(), { target: { value: '{oops' } });

        fireEvent.click(screen.getByRole('button', { name: 'toggle-chat-view' }));
        expect(screen.queryByLabelText(t.chatSpeechPassthrough)).not.toBeInTheDocument();
        fireEvent.click(screen.getByRole('button', { name: 'toggle-chat-view' }));

        // A fresh view (settings closed, Advanced collapsed) still shows the bad field.
        expect(screen.getByLabelText(t.chatSpeechPassthrough)).toHaveValue('{oops');
        expect(screen.getByLabelText(t.chatSpeechPassthrough)).toBeInvalid();
      });
    });

    // The thread's kind decides the attach control: a speech run sends only the text,
    // so an image could never be used -- not even for a vision-capable model, whose
    // button would otherwise read as merely "attach an image" while being disabled.
    it('disables the attach button and says why, even for a vision-capable model', async () => {
      await renderSpeechThread([{ ...speechModels[0], vision: true }]);

      const attach = screen.getByRole('button', { name: t.chatAttachImage });
      expect(attach).toHaveAttribute('aria-disabled', 'true');
      fireEvent.mouseOver(attach);
      expect(await screen.findByRole('tooltip')).toHaveTextContent(t.chatSpeechNoImageInput);
    });
  });

  // The not-stored bubble (#281): an answer whose audio is gone carries the `audio_removed`
  // marker and renders as a placeholder with a regenerate button. The marker answer is a real
  // transcript message with its own id, so that button replays THAT turn -- the history up to
  // its own prompt -- through the same store handler as the action stack's.
  describe(`Chat: regenerating a speech answer whose audio is not stored [${locale}]`, () => {
    const speechModel: ModelOption = {
      id: 'kokoro',
      display_name: 'kokoro',
      flavors: ['openai_audio'],
      loading_on_count: 0,
      speech: true,
    };
    const marker = { type: 'audio_removed' };

    function seedAnswers() {
      chatApi.rows.push({
        id: 'c_not_stored',
        title: 'Greetings',
        created_at: '2026-07-22T12:00:00Z',
        updated_at: '2026-07-22T12:00:00Z',
        content: {
          settings: { model: 'kokoro', kind: 'speech' },
          messages: [
            { id: 'u1', role: 'user', content: 'say hello' },
            { id: 'a1', role: 'assistant', content: [marker], status: 'complete' },
            { id: 'u2', role: 'user', content: 'say goodbye' },
            {
              id: 'a2',
              role: 'assistant',
              content: [{ type: 'text', text: 'Goodbye.' }, marker],
              status: 'complete',
            },
          ],
        },
      });
    }

    async function renderAnswers() {
      seedAnswers();
      renderChat(tokens, [speechModel]);
      await screen.findByText('Greetings');
      // Both answers are placeholders: no player, no download, one regenerate each.
      expect(await screen.findAllByText(t.chatAudioNotStored)).toHaveLength(2);
      expect(document.querySelector('audio')).toBeNull();
      expect(screen.queryByRole('button', { name: t.chatDownloadAudio })).toBeNull();
      return screen.getAllByRole('button', { name: t.chatRegenerate });
    }

    function startedHistory() {
      return (chatApi.spies.startChatRun.mock.calls[0][1] as StartChatRunBody).edited_history;
    }

    it('replays the history up to the answer the button belongs to', async () => {
      const [, second] = await renderAnswers();

      fireEvent.click(second);

      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
      // a2's own prompt is the last turn replayed: [u1, a1, u2] -- a2 is dropped.
      expect(startedHistory()).toEqual([
        expect.objectContaining({ id: 'u1', role: 'user' }),
        expect.objectContaining({ id: 'a1', role: 'assistant' }),
        expect.objectContaining({ id: 'u2', role: 'user' }),
      ]);
    });

    it('replays only up to the first prompt from the first answer, not the last one', async () => {
      const [first] = await renderAnswers();

      fireEvent.click(first);

      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
      expect(startedHistory()).toEqual([expect.objectContaining({ id: 'u1', role: 'user' })]);
    });
  });

  // The per-answer delete of a stored spoken answer's audio (#281). The answer's own id is bound
  // by the store's handlersFor, so the request names THAT answer; the control asks inline first,
  // and an answer whose audio is already gone offers none.
  describe(`Chat: deleting the audio of one spoken answer [${locale}]`, () => {
    const speechModel: ModelOption = {
      id: 'kokoro',
      display_name: 'kokoro',
      flavors: ['openai_audio'],
      loading_on_count: 0,
      speech: true,
    };
    const wav = { type: 'audio_url', audio_url: { url: 'data:audio/wav;base64,UklG' } };
    const marker = { type: 'audio_removed' };

    // u1/a1 and u2/a2 hold audio, u3/a3 is already not stored.
    async function renderAnswers() {
      chatApi.rows.push({
        id: 'c_delete_audio',
        title: 'Greetings',
        created_at: '2026-07-22T12:00:00Z',
        updated_at: '2026-07-22T12:00:00Z',
        content: {
          settings: { model: 'kokoro', kind: 'speech' },
          messages: [
            { id: 'u1', role: 'user', content: 'say one' },
            { id: 'a1', role: 'assistant', content: [wav], status: 'complete' },
            { id: 'u2', role: 'user', content: 'say two' },
            { id: 'a2', role: 'assistant', content: [wav], status: 'complete' },
            { id: 'u3', role: 'user', content: 'say three' },
            { id: 'a3', role: 'assistant', content: [marker], status: 'complete' },
          ],
        },
      });
      renderChat(tokens, [speechModel]);
      await screen.findByText('Greetings');
      expect(await screen.findAllByText(t.chatAudioNotStored)).toHaveLength(1);
    }

    const trashes = () => screen.queryAllByRole('button', { name: t.chatAudioDelete });

    it('offers the control on each answer that holds audio, and on none that does not', async () => {
      await renderAnswers();

      // a1 and a2 have a player and a trash icon; a3 is the placeholder and has none.
      expect(trashes()).toHaveLength(2);
      expect(screen.getAllByLabelText(t.chatGeneratedAudio)).toHaveLength(2);
    });

    it('deletes the audio of the answer it belongs to, once the inline confirm is accepted', async () => {
      await renderAnswers();

      fireEvent.click(trashes()[1]);
      expect(screen.getByText(t.chatAudioDeleteConfirm)).toBeInTheDocument();
      expect(screen.queryByRole('dialog')).toBeNull();
      expect(chatApi.spies.deleteMessageAudio).not.toHaveBeenCalled();
      fireEvent.click(screen.getByRole('button', { name: t.chatAudioDeleteAction }));

      await waitFor(() =>
        expect(chatApi.spies.deleteMessageAudio).toHaveBeenCalledWith('c_delete_audio', 'a2'),
      );
      expect(chatApi.spies.deleteMessageAudio).toHaveBeenCalledTimes(1);
      // a2 is now the not-stored bubble (with a3, two placeholders); a1 still plays and still
      // offers its own delete, and the placeholders offer none.
      await waitFor(() => expect(screen.getAllByText(t.chatAudioNotStored)).toHaveLength(2));
      expect(screen.getAllByLabelText(t.chatGeneratedAudio)).toHaveLength(1);
      expect(trashes()).toHaveLength(1);
    });

    it('deletes the other answer when it is the first one that is asked', async () => {
      await renderAnswers();

      fireEvent.click(trashes()[0]);
      fireEvent.click(screen.getByRole('button', { name: t.chatAudioDeleteAction }));

      await waitFor(() =>
        expect(chatApi.spies.deleteMessageAudio).toHaveBeenCalledWith('c_delete_audio', 'a1'),
      );
    });

    it('sends nothing, and keeps the audio, when the confirm is cancelled', async () => {
      await renderAnswers();

      fireEvent.click(trashes()[1]);
      fireEvent.click(screen.getByRole('button', { name: t.chatCancel }));

      expect(trashes()).toHaveLength(2);
      expect(screen.queryByText(t.chatAudioDeleteConfirm)).toBeNull();
      expect(chatApi.spies.deleteMessageAudio).not.toHaveBeenCalled();
      expect(screen.getAllByText(t.chatAudioNotStored)).toHaveLength(1);
    });
  });

  // A transcription (speech-to-text) thread: the composer is AUDIO first -- there is no text
  // prompt, the turn is a clip, attached from a file or recorded in the browser -- and the
  // settings are the transcription knobs. The reply is text, so there is no format control.
  describe(`Chat: the composer of a transcription thread [${locale}]`, () => {
    const asrModels: ModelOption[] = [
      {
        id: 'whisper',
        display_name: 'whisper',
        flavors: ['openai_audio'],
        loading_on_count: 0,
        transcription: true,
      },
    ];

    // The browser's recording stack, small enough to drive from here: a microphone whose track
    // says when it is released, and a recorder that reports what a Chromium one does.
    class FakeRecorder {
      state: 'inactive' | 'recording' = 'inactive';
      mimeType = 'audio/webm;codecs=opus';
      ondataavailable: ((event: { data: Blob }) => void) | null = null;
      onstop: (() => void) | null = null;
      onerror: (() => void) | null = null;
      start() {
        this.state = 'recording';
      }
      stop() {
        this.state = 'inactive';
        this.ondataavailable?.({
          data: new Blob(['voice-bytes'], { type: 'audio/webm;codecs=opus' }),
        });
        this.onstop?.();
      }
    }
    let getUserMedia: ReturnType<typeof vi.fn>;
    let micTrackStop: ReturnType<typeof vi.fn>;

    beforeEach(() => {
      micTrackStop = vi.fn();
      getUserMedia = vi.fn(async () => ({ getTracks: () => [{ stop: micTrackStop }] }));
      Object.defineProperty(navigator, 'mediaDevices', {
        value: { getUserMedia },
        configurable: true,
      });
      vi.stubGlobal('MediaRecorder', FakeRecorder);
      Object.defineProperty(URL, 'createObjectURL', {
        value: vi.fn(() => 'blob:recording'),
        configurable: true,
      });
      Object.defineProperty(URL, 'revokeObjectURL', { value: vi.fn(), configurable: true });
    });

    afterEach(() => {
      Reflect.deleteProperty(navigator, 'mediaDevices');
      Reflect.deleteProperty(URL, 'createObjectURL');
      Reflect.deleteProperty(URL, 'revokeObjectURL');
    });

    async function renderTranscriptionThread(models: ModelOption[] = asrModels) {
      renderChat(tokens, models);
      await waitForChatReady();
      await pickOption(t.chatModel, 'whisper');
    }

    const sendButton = () => screen.getByRole('button', { name: t.send });

    function attachAudio(file: File) {
      fireEvent.change(screen.getByLabelText(t.chatAttachAudio), { target: { files: [file] } });
    }

    const wavFile = (type = 'audio/x-wav', name = 'memo.wav') =>
      new File(['RIFFwave-bytes'], name, { type });

    // The data url of a clip the converter stand-in (see the vi.mock above) made of an upload
    // that held `text`: always audio/wav, the converted bytes.
    const wavUrl = (text: string) => `data:audio/wav;base64,${btoa(`WAV:${text}`)}`;
    const toWav = vi.mocked(audioBlobToWav);
    beforeEach(() => toWav.mockClear());

    // Records a clip through the real recorder and attaches it to the turn.
    async function recordClip() {
      fireEvent.click(screen.getByRole('button', { name: t.chatRecordStart }));
      await flushMicrotasks();
      fireEvent.click(screen.getByRole('button', { name: t.chatRecordStop }));
      await flushMicrotasks();
      fireEvent.click(screen.getByRole('button', { name: t.chatRecordUse }));
    }

    function startedBody() {
      return chatApi.spies.startChatRun.mock.calls[0][1] as StartChatRunBody;
    }

    // Opens the settings panel and the Advanced section; returns the passthrough field.
    function openPassthrough() {
      openSettings();
      fireEvent.click(screen.getByRole('button', { name: t.chatTranscriptionAdvanced }));
      return screen.getByLabelText(t.chatTranscriptionPassthrough);
    }

    describe('the audio input', () => {
      it('replaces the text prompt: a clip, not a message, is the turn', async () => {
        await renderTranscriptionThread();

        expect(
          screen.getByRole('group', { name: t.chatTranscriptionInputLabel }),
        ).toBeInTheDocument();
        expect(screen.getByText(t.chatTranscriptionOnlyHint)).toBeInTheDocument();
        // There is nothing to type into: none of the text, image or speech prompts.
        expect(screen.queryByLabelText(t.messageLabel)).not.toBeInTheDocument();
        expect(screen.queryByLabelText(t.chatImagePromptLabel)).not.toBeInTheDocument();
        expect(screen.queryByLabelText(t.chatSpeechPromptLabel)).not.toBeInTheDocument();
        expect(document.getElementById('chat-message')).toBeNull();
      });

      it('offers an attach button for any audio and a record button', async () => {
        await renderTranscriptionThread();

        const input = screen.getByLabelText(t.chatAttachAudio);
        expect(input).toHaveAttribute('type', 'file');
        // Any audio the browser can decode: it is converted to WAV before it is attached.
        const accepted = (input.getAttribute('accept') ?? '').split(',');
        expect(accepted).toContain('audio/*');
        // One clip is the turn.
        expect(input).not.toHaveAttribute('multiple');
        expect(screen.getByRole('button', { name: t.chatRecordStart })).toBeEnabled();
      });

      it('hides the image and speech controls', async () => {
        await renderTranscriptionThread();
        openSettings();

        // No image attach, mask or knobs...
        expect(screen.queryByLabelText(t.chatAttachImage)).not.toBeInTheDocument();
        expect(screen.queryByLabelText(t.chatAttachImageEdit)).not.toBeInTheDocument();
        expect(screen.queryByRole('combobox', { name: t.chatImageSize })).not.toBeInTheDocument();
        expect(screen.queryByRole('button', { name: t.chatImageAdvanced })).not.toBeInTheDocument();
        // ...no speech knobs...
        expect(screen.queryByLabelText(t.chatSpeechVoice)).not.toBeInTheDocument();
        expect(screen.queryByLabelText(t.chatSpeechSpeed)).not.toBeInTheDocument();
        // ...and none of the text chat's (a system prompt and a token cap mean nothing here).
        expect(screen.queryByLabelText(t.chatSystemPrompt)).not.toBeInTheDocument();
        expect(screen.queryByLabelText(t.chatMaxTokens)).not.toBeInTheDocument();
      });

      it('belongs to a transcription thread only', async () => {
        renderChat();
        await waitForChatReady();
        await pickOption(t.chatModel, models[0].display_name);

        expect(screen.queryByLabelText(t.chatAttachAudio)).not.toBeInTheDocument();
        expect(screen.queryByRole('button', { name: t.chatRecordStart })).not.toBeInTheDocument();
        expect(screen.getByLabelText(t.messageLabel)).toBeInTheDocument();
        expect(sendButton()).toBeEnabled();
      });

      it('keeps Send disabled until a clip is attached, and says what is missing', async () => {
        await renderTranscriptionThread();

        expect(sendButton()).toBeDisabled();
        fireEvent.mouseOver(sendButton().parentElement as HTMLElement);
        expect(await screen.findByRole('tooltip')).toHaveTextContent(t.chatTranscriptionNeedsAudio);

        attachAudio(wavFile());
        await waitFor(() => expect(sendButton()).toBeEnabled());
      });

      it('shows the attached clip with its name and a player, and removes it again', async () => {
        await renderTranscriptionThread();
        attachAudio(wavFile('audio/wav', 'standup.wav'));

        const player = await screen.findByLabelText(t.chatAttachedAudio);
        expect(player.tagName).toBe('AUDIO');
        expect(player).toHaveAttribute('controls');
        expect(player.getAttribute('src')).toMatch(/^data:audio\/wav;base64,/);
        expect(screen.getByText('standup.wav')).toBeInTheDocument();

        fireEvent.click(screen.getByRole('button', { name: t.chatRemoveAudio }));
        expect(screen.queryByLabelText(t.chatAttachedAudio)).not.toBeInTheDocument();
        expect(screen.queryByText('standup.wav')).not.toBeInTheDocument();
        expect(sendButton()).toBeDisabled();
      });

      it('replaces the clip when another is attached: a turn has one', async () => {
        await renderTranscriptionThread();
        attachAudio(wavFile('audio/wav', 'first.wav'));
        await screen.findByText('first.wav');

        attachAudio(wavFile('audio/wav', 'second.wav'));
        await screen.findByText('second.wav');

        expect(screen.queryByText('first.wav')).not.toBeInTheDocument();
        expect(screen.getAllByLabelText(t.chatAttachedAudio)).toHaveLength(1);
      });

      it('disables the audio input while a run is in flight', async () => {
        await renderTranscriptionThread();
        attachAudio(wavFile());
        await waitFor(() => expect(sendButton()).toBeEnabled());
        fireEvent.click(sendButton());
        await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalled());

        // The run is live (no `done` yet): nothing may be attached or recorded under it.
        await waitFor(() =>
          expect(screen.getByRole('button', { name: t.chatAttachAudio })).toHaveAttribute(
            'aria-disabled',
            'true',
          ),
        );
        expect(screen.getByRole('button', { name: t.chatRecordStart })).toBeDisabled();
        expect(sendButton()).toBeDisabled();
      });

      it('drops a clip that is left behind when the thread turns out not to be a transcription one', async () => {
        renderChat(tokens, [...asrModels, ...models]);
        await waitForChatReady();
        await pickOption(t.chatModel, 'whisper');
        attachAudio(wavFile('audio/wav', 'gone.wav'));
        await screen.findByText('gone.wav');

        await pickOption(t.chatModel, models[0].display_name);

        expect(screen.queryByText('gone.wav')).not.toBeInTheDocument();
        expect(screen.getByLabelText(t.messageLabel)).toBeInTheDocument();
        // And a text turn sends as it always did, with no leftover clip in it.
        fireEvent.change(screen.getByLabelText(t.messageLabel), { target: { value: 'hi' } });
        fireEvent.click(sendButton());
        await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalled());
        expect(startedBody().user_message).toBe('hi');
      });
    });

    describe('attaching a file', () => {
      it('sends a wav file as an audio/wav part', async () => {
        await renderTranscriptionThread();
        attachAudio(wavFile('audio/x-wav'));
        await waitFor(() => expect(sendButton()).toBeEnabled());
        fireEvent.click(sendButton());

        await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalled());
        // The whole turn: the clip alone, as the converter made it (a wav is converted too, to
        // the 16 kHz mono the server reads), under audio/wav.
        expect(startedBody().user_message).toEqual([
          { type: 'audio_url', audio_url: { url: wavUrl('RIFFwave-bytes') } },
        ]);
        expect(startedBody().settings.kind).toBe('transcription');
      });

      // The server reads WAV only, so whatever the browser can decode is converted to one
      // (audioToWav) BEFORE it is attached: the player, the part and the stored turn all carry the
      // wav, never the upload's own format.
      it.each([
        ['audio/mp3', 'take.mp3'],
        ['audio/x-m4a', 'take.m4a'],
        ['audio/ogg;codecs=opus', 'take.ogg'],
        ['video/webm', 'take.webm'],
        ['audio/flac', 'take.flac'],
        // No type at all (a file dialog that does not know the format): the decoder decides.
        ['', 'take.MP3'],
        ['application/octet-stream', 'take.m4a'],
      ])('converts a %s file called %s to an audio/wav part', async (type, name) => {
        await renderTranscriptionThread();
        const upload = new File(['bytes'], name, { type });
        attachAudio(upload);
        await waitFor(() => expect(sendButton()).toBeEnabled());
        expect(toWav).toHaveBeenCalledWith(upload);
        // The preview plays the wav that will be sent.
        expect(screen.getByLabelText(t.chatAttachedAudio).getAttribute('src')).toBe(
          wavUrl('bytes'),
        );
        fireEvent.click(sendButton());

        await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalled());
        expect(startedBody().user_message).toEqual([
          { type: 'audio_url', audio_url: { url: wavUrl('bytes') } },
        ]);
      });

      it.each([
        ['an image', new File(['x'], 'cat.png', { type: 'image/png' })],
        ['a text file', new File(['x'], 'notes.txt', { type: 'text/plain' })],
        ['a pdf', new File(['x'], 'paper.pdf', { type: 'application/pdf' })],
      ])('refuses %s with the unsupported-format message', async (_name, file) => {
        await renderTranscriptionThread();
        attachAudio(file);

        expect(await screen.findByRole('alert')).toHaveTextContent(t.chatAudioErrorType);
        expect(screen.queryByLabelText(t.chatAttachedAudio)).not.toBeInTheDocument();
        expect(sendButton()).toBeDisabled();
        expect(toWav).not.toHaveBeenCalled();
      });

      it('says so when the browser cannot decode the audio, and attaches nothing', async () => {
        toWav.mockRejectedValueOnce(new AudioDecodeError('corrupt'));
        await renderTranscriptionThread();
        attachAudio(new File(['x'], 'broken.mp3', { type: 'audio/mpeg' }));

        expect(await screen.findByRole('alert')).toHaveTextContent(t.chatAudioErrorDecode);
        expect(screen.queryByLabelText(t.chatAttachedAudio)).not.toBeInTheDocument();
        expect(sendButton()).toBeDisabled();
      });

      it('says so, and keeps the clip it had, when the next one cannot be decoded', async () => {
        await renderTranscriptionThread();
        attachAudio(wavFile('audio/wav', 'keep.wav'));
        await screen.findByText('keep.wav');

        toWav.mockRejectedValueOnce(new AudioDecodeError('corrupt'));
        attachAudio(new File(['x'], 'broken.mp3', { type: 'audio/mpeg' }));

        expect(await screen.findByRole('alert')).toHaveTextContent(t.chatAudioErrorDecode);
        expect(screen.getByText('keep.wav')).toBeInTheDocument();
        expect(sendButton()).toBeEnabled();
      });

      it('refuses audio whose WAV would be over the 20 MB the backend stores', async () => {
        // A small compressed upload that decodes to a long clip: the converted size counts.
        const long = new Blob(['x'], { type: 'audio/wav' });
        Object.defineProperty(long, 'size', { value: 20 * 1024 * 1024 + 1 });
        toWav.mockResolvedValueOnce(long);
        await renderTranscriptionThread();
        attachAudio(new File(['small'], 'long.mp3', { type: 'audio/mpeg' }));

        expect(await screen.findByRole('alert')).toHaveTextContent(t.chatAudioErrorSize);
        expect(screen.queryByLabelText(t.chatAttachedAudio)).not.toBeInTheDocument();
        expect(sendButton()).toBeDisabled();
      });

      it('refuses a clip over the 20 MB the backend stores', async () => {
        await renderTranscriptionThread();
        const big = wavFile('audio/wav');
        Object.defineProperty(big, 'size', { value: 20 * 1024 * 1024 + 1 });
        attachAudio(big);

        expect(await screen.findByRole('alert')).toHaveTextContent(t.chatAudioErrorSize);
        expect(sendButton()).toBeDisabled();
      });

      it('refuses an empty file', async () => {
        await renderTranscriptionThread();
        attachAudio(new File([], 'silence.wav', { type: 'audio/wav' }));

        expect(await screen.findByRole('alert')).toHaveTextContent(t.chatAudioErrorEmpty);
        expect(sendButton()).toBeDisabled();
      });

      it('keeps the clip it had when the next one is refused', async () => {
        await renderTranscriptionThread();
        attachAudio(wavFile('audio/wav', 'keep.wav'));
        await screen.findByText('keep.wav');

        attachAudio(new File(['x'], 'cat.png', { type: 'image/png' }));

        expect(await screen.findByRole('alert')).toHaveTextContent(t.chatAudioErrorType);
        expect(screen.getByText('keep.wav')).toBeInTheDocument();
        expect(sendButton()).toBeEnabled();
      });

      it('clears the clip and the composer when the turn is sent', async () => {
        await renderTranscriptionThread();
        attachAudio(wavFile('audio/wav', 'sent.wav'));
        await waitFor(() => expect(sendButton()).toBeEnabled());
        fireEvent.click(sendButton());
        await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalled());

        expect(screen.queryByText('sent.wav')).not.toBeInTheDocument();
        // The composer's preview is gone; the clip now lives in the sent user bubble of the
        // transcript, a player of the same name (the attached-audio label is shared with the
        // composer's, as the attached-image alt text is), so the two are told apart by where
        // they are.
        const composer = screen.getByRole('group', { name: t.chatTranscriptionInputLabel });
        expect(within(composer).queryByLabelText(t.chatAttachedAudio)).not.toBeInTheDocument();
        expect(
          within(screen.getByRole('log')).getByLabelText(t.chatAttachedAudio),
        ).toBeInTheDocument();
      });
    });

    describe('recording', () => {
      it('converts a recorded audio/webm;codecs=opus clip to an audio/wav part', async () => {
        await renderTranscriptionThread();
        await recordClip();

        // The take is attached, labelled as a recording, and Send is open.
        expect(await screen.findByText(t.chatAudioRecordingLabel)).toBeInTheDocument();
        await waitFor(() => expect(sendButton()).toBeEnabled());
        fireEvent.click(sendButton());

        await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalled());
        // The recorder's own format never leaves the browser: the take was converted to WAV when
        // it was used (the bytes are the converter's, not the recorder's).
        expect(toWav).toHaveBeenCalledTimes(1);
        expect(startedBody().user_message).toEqual([
          { type: 'audio_url', audio_url: { url: wavUrl('voice-bytes') } },
        ]);
        expect(JSON.stringify(startedBody().user_message)).not.toContain('webm');
        expect(startedBody().settings.kind).toBe('transcription');
      });

      it('says so when the take cannot be converted, and attaches nothing', async () => {
        toWav.mockRejectedValueOnce(new AudioDecodeError('unsupported'));
        await renderTranscriptionThread();
        await recordClip();

        expect(await screen.findByRole('alert')).toHaveTextContent(t.chatAudioErrorDecode);
        expect(screen.queryByLabelText(t.chatAttachedAudio)).not.toBeInTheDocument();
        expect(screen.queryByText(t.chatAudioRecordingLabel)).not.toBeInTheDocument();
        expect(sendButton()).toBeDisabled();
      });

      it('releases the microphone once the take is stopped', async () => {
        await renderTranscriptionThread();
        await recordClip();

        expect(getUserMedia).toHaveBeenCalledWith({ audio: true });
        expect(micTrackStop).toHaveBeenCalled();
      });

      it('says so when the microphone is denied, and the composer still works', async () => {
        getUserMedia.mockRejectedValueOnce(new DOMException('denied', 'NotAllowedError'));
        await renderTranscriptionThread();
        fireEvent.click(screen.getByRole('button', { name: t.chatRecordStart }));
        await flushMicrotasks();

        expect(screen.getByRole('alert')).toHaveTextContent(t.chatRecordDenied);
        expect(sendButton()).toBeDisabled();
        // A file can still be attached, and the error is about the microphone only.
        attachAudio(wavFile());
        await waitFor(() => expect(sendButton()).toBeEnabled());
      });

      it('says so when the browser cannot record at all', async () => {
        Reflect.deleteProperty(navigator, 'mediaDevices');
        await renderTranscriptionThread();
        fireEvent.click(screen.getByRole('button', { name: t.chatRecordStart }));
        await flushMicrotasks();

        expect(screen.getByRole('alert')).toHaveTextContent(t.chatRecordUnsupported);
      });

      it('discards a take without attaching anything', async () => {
        await renderTranscriptionThread();
        fireEvent.click(screen.getByRole('button', { name: t.chatRecordStart }));
        await flushMicrotasks();
        fireEvent.click(screen.getByRole('button', { name: t.chatRecordStop }));
        await flushMicrotasks();
        expect(screen.getByLabelText(t.chatRecordPreview)).toBeInTheDocument();

        fireEvent.click(screen.getByRole('button', { name: t.chatRecordDiscard }));

        expect(screen.queryByLabelText(t.chatRecordPreview)).not.toBeInTheDocument();
        expect(screen.queryByLabelText(t.chatAttachedAudio)).not.toBeInTheDocument();
        expect(sendButton()).toBeDisabled();
      });

      it('lets go of a take in progress when the thread leaves the transcription composer', async () => {
        renderChat(tokens, [...asrModels, ...models]);
        await waitForChatReady();
        await pickOption(t.chatModel, 'whisper');
        fireEvent.click(screen.getByRole('button', { name: t.chatRecordStart }));
        await flushMicrotasks();
        expect(screen.getByRole('button', { name: t.chatRecordStop })).toBeInTheDocument();

        await pickOption(t.chatModel, models[0].display_name);

        // The recorder went with the composer, and the microphone with it.
        expect(screen.queryByRole('button', { name: t.chatRecordStop })).not.toBeInTheDocument();
        expect(micTrackStop).toHaveBeenCalled();
      });
    });

    // A clip is converted to WAV in the browser before it is attached, which takes a moment for a
    // long one: the composer says so while it runs, and holds everything that could stack another
    // attach on it or send before it lands. The converter stand-in answers at once, so these
    // tests hold a conversion open by hand and finish it themselves.
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
      const composer = () => screen.getByRole('group', { name: t.chatTranscriptionInputLabel });
      const indicator = () => within(composer()).queryByRole('status');
      const attachButton = () => screen.getByRole('button', { name: t.chatAttachAudio });
      const recordButton = () => screen.getByRole('button', { name: t.chatRecordStart });

      it('shows no indicator while nothing is being converted', async () => {
        await renderTranscriptionThread();

        expect(indicator()).toBeNull();
        expect(screen.queryByText(t.chatTranscriptionConverting)).not.toBeInTheDocument();
      });

      it('shows an indicator, and holds attach, record and Send, while a file is converted', async () => {
        const conversion = deferConversion();
        await renderTranscriptionThread();
        attachAudio(wavFile('audio/wav', 'long.wav'));

        // A spinner with its label, announced as a status.
        expect(await within(composer()).findByRole('status')).toHaveTextContent(
          t.chatTranscriptionConverting,
        );
        expect(within(composer()).getByRole('progressbar', { hidden: true })).toBeInTheDocument();
        expect(attachButton()).toHaveAttribute('aria-disabled', 'true');
        expect(recordButton()).toBeDisabled();
        expect(sendButton()).toBeDisabled();
        expect(screen.queryByLabelText(t.chatAttachedAudio)).not.toBeInTheDocument();

        conversion.finish('long');

        // Back to the normal state, with the clip attached and Send open.
        await waitFor(() => expect(indicator()).toBeNull());
        expect(screen.queryByText(t.chatTranscriptionConverting)).not.toBeInTheDocument();
        expect(screen.getByText('long.wav')).toBeInTheDocument();
        expect(attachButton()).not.toHaveAttribute('aria-disabled', 'true');
        expect(recordButton()).toBeEnabled();
        expect(sendButton()).toBeEnabled();
      });

      it('holds Send while a clip is converted even though one is already attached', async () => {
        await renderTranscriptionThread();
        attachAudio(wavFile('audio/wav', 'first.wav'));
        await screen.findByText('first.wav');
        expect(sendButton()).toBeEnabled();

        const conversion = deferConversion();
        attachAudio(wavFile('audio/wav', 'second.wav'));

        // The clip it had stays in view, but nothing may be sent (or stacked) until the new one
        // lands: Send would otherwise transcribe the clip that is being replaced.
        expect(await within(composer()).findByRole('status')).toBeInTheDocument();
        expect(screen.getByText('first.wav')).toBeInTheDocument();
        expect(sendButton()).toBeDisabled();
        expect(attachButton()).toHaveAttribute('aria-disabled', 'true');

        conversion.finish('second');

        await screen.findByText('second.wav');
        expect(screen.queryByText('first.wav')).not.toBeInTheDocument();
        expect(indicator()).toBeNull();
        expect(sendButton()).toBeEnabled();
      });

      it('shows the indicator while a recording is converted, and holds the record controls', async () => {
        const conversion = deferConversion();
        await renderTranscriptionThread();
        await recordClip();

        expect(await within(composer()).findByRole('status')).toHaveTextContent(
          t.chatTranscriptionConverting,
        );
        expect(recordButton()).toBeDisabled();
        expect(attachButton()).toHaveAttribute('aria-disabled', 'true');
        expect(sendButton()).toBeDisabled();

        conversion.finish('voice-bytes');

        expect(await screen.findByText(t.chatAudioRecordingLabel)).toBeInTheDocument();
        expect(indicator()).toBeNull();
        expect(recordButton()).toBeEnabled();
        expect(sendButton()).toBeEnabled();
      });

      it('holds "Use recording" of a take that is waiting while a file is converted', async () => {
        await renderTranscriptionThread();
        fireEvent.click(recordButton());
        await flushMicrotasks();
        fireEvent.click(screen.getByRole('button', { name: t.chatRecordStop }));
        await flushMicrotasks();
        expect(screen.getByRole('button', { name: t.chatRecordUse })).toBeEnabled();

        const conversion = deferConversion();
        attachAudio(wavFile('audio/wav', 'file.wav'));

        // The take cannot be used (it would stack a second attach), but it can still be thrown away.
        expect(await within(composer()).findByRole('status')).toBeInTheDocument();
        expect(screen.getByRole('button', { name: t.chatRecordUse })).toBeDisabled();
        expect(screen.getByRole('button', { name: t.chatRecordDiscard })).toBeEnabled();

        conversion.finish('file');

        await screen.findByText('file.wav');
        expect(screen.getByRole('button', { name: t.chatRecordUse })).toBeEnabled();
      });

      it('goes back to the normal state, with the error shown, when the conversion fails', async () => {
        const conversion = deferConversion();
        await renderTranscriptionThread();
        attachAudio(new File(['x'], 'broken.mp3', { type: 'audio/mpeg' }));
        expect(await within(composer()).findByRole('status')).toBeInTheDocument();

        conversion.fail(new AudioDecodeError('corrupt'));

        expect(await screen.findByRole('alert')).toHaveTextContent(t.chatAudioErrorDecode);
        expect(indicator()).toBeNull();
        expect(attachButton()).not.toHaveAttribute('aria-disabled', 'true');
        expect(recordButton()).toBeEnabled();
        expect(screen.queryByLabelText(t.chatAttachedAudio)).not.toBeInTheDocument();
        // No clip, so Send stays closed -- for that reason, not for the conversion.
        expect(sendButton()).toBeDisabled();
      });

      it('does not say that a clip is missing while the one just attached is converted', async () => {
        const conversion = deferConversion();
        await renderTranscriptionThread();
        attachAudio(wavFile());
        expect(await within(composer()).findByRole('status')).toBeInTheDocument();

        // The indicator says what is going on; the "attach audio first" hint would contradict it.
        fireEvent.mouseOver(sendButton().parentElement as HTMLElement);
        await act(async () => {
          await new Promise((resolve) => setTimeout(resolve, 200));
        });
        expect(screen.queryByRole('tooltip')).toBeNull();

        conversion.finish('RIFFwave-bytes');
        await waitFor(() => expect(sendButton()).toBeEnabled());
      });

      it('does not offer the indicator on a thread that takes text', async () => {
        renderChat();
        await waitForChatReady();

        expect(screen.queryByText(t.chatTranscriptionConverting)).not.toBeInTheDocument();
      });
    });

    // The LIVE mode (#266): for a model the gateway says can stream a transcription, the composer
    // offers a third way to give the turn -- speaking into the microphone, with the transcript
    // appearing as the user talks -- beside the file and the buffered recording. A model that
    // cannot stream gets none of it: no switch, the two inputs as they always were.
    describe('the live mode', () => {
      type FakeLive = {
        args: LiveSessionArgs;
        stop: Mock<() => void>;
        abort: Mock<() => void>;
      };
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

      const modeButton = (name: string) => screen.queryByRole('button', { name });
      const startButton = () =>
        screen.queryByRole('button', { name: t.chatTranscriptionLiveStart });
      const stopButton = () => screen.queryByRole('button', { name: t.chatTranscriptionLiveStop });
      const transcriptRegion = () =>
        screen.queryByRole('region', { name: t.chatTranscriptionLiveTranscript });

      async function renderStreamingThread() {
        chatApi.spies.modelTranscriptionStreaming.mockResolvedValue(true);
        await renderTranscriptionThread();
        // The gate answers a moment after the model is picked: the switch is the proof.
        await screen.findByRole('button', { name: t.chatTranscriptionModeLive });
      }

      const chooseLive = () =>
        fireEvent.click(screen.getByRole('button', { name: t.chatTranscriptionModeLive }));

      // A live recording, started from the panel and listening.
      async function startRecording() {
        chooseLive();
        fireEvent.click(startButton()!);
        await act(async () => sessions[0].args.onState('listening'));
      }

      describe('what is offered', () => {
        it('is not offered for a model that cannot stream: the file and the recorder, as ever', async () => {
          await renderTranscriptionThread();
          await waitFor(() =>
            expect(chatApi.spies.modelTranscriptionStreaming).toHaveBeenCalledWith('whisper'),
          );

          expect(modeButton(t.chatTranscriptionModeLive)).not.toBeInTheDocument();
          expect(modeButton(t.chatTranscriptionModeFile)).not.toBeInTheDocument();
          expect(modeButton(t.chatTranscriptionModeRecord)).not.toBeInTheDocument();
          expect(screen.getByLabelText(t.chatAttachAudio)).toBeInTheDocument();
          expect(screen.getByRole('button', { name: t.chatRecordStart })).toBeInTheDocument();
          expect(startButton()).not.toBeInTheDocument();
        });

        it('is offered as the third of file / record / live, once the gateway says the model streams', async () => {
          await renderStreamingThread();

          for (const name of [
            t.chatTranscriptionModeFile,
            t.chatTranscriptionModeRecord,
            t.chatTranscriptionModeLive,
          ]) {
            expect(modeButton(name)).toBeInTheDocument();
          }
          // The switch is named, as a group of choices.
          expect(screen.getByRole('group', { name: t.chatTranscriptionMode })).toBeInTheDocument();
        });

        it('shows the file input first, as the one that always was', async () => {
          await renderStreamingThread();

          expect(modeButton(t.chatTranscriptionModeFile)).toHaveAttribute('aria-pressed', 'true');
          expect(screen.getByLabelText(t.chatAttachAudio)).toBeInTheDocument();
          expect(screen.queryByRole('button', { name: t.chatRecordStart })).not.toBeInTheDocument();
          expect(startButton()).not.toBeInTheDocument();
        });

        it('shows only the chosen input', async () => {
          await renderStreamingThread();

          fireEvent.click(screen.getByRole('button', { name: t.chatTranscriptionModeRecord }));
          expect(screen.getByRole('button', { name: t.chatRecordStart })).toBeInTheDocument();
          expect(screen.queryByLabelText(t.chatAttachAudio)).not.toBeInTheDocument();
          expect(startButton()).not.toBeInTheDocument();

          chooseLive();
          expect(modeButton(t.chatTranscriptionModeLive)).toHaveAttribute('aria-pressed', 'true');
          expect(startButton()).toBeEnabled();
          expect(screen.getByText(t.chatTranscriptionLiveHint)).toBeInTheDocument();
          expect(screen.queryByLabelText(t.chatAttachAudio)).not.toBeInTheDocument();
          expect(screen.queryByRole('button', { name: t.chatRecordStart })).not.toBeInTheDocument();

          fireEvent.click(screen.getByRole('button', { name: t.chatTranscriptionModeFile }));
          expect(screen.getByLabelText(t.chatAttachAudio)).toBeInTheDocument();
          expect(startButton()).not.toBeInTheDocument();
        });

        it('has no transcript area until a recording has begun', async () => {
          await renderStreamingThread();
          chooseLive();
          expect(transcriptRegion()).not.toBeInTheDocument();
        });

        it('keeps the file and the recorded clip working in their modes', async () => {
          await renderStreamingThread();

          attachAudio(wavFile());
          expect(await screen.findByLabelText(t.chatAttachedAudio)).toBeInTheDocument();
          expect(sendButton()).toBeEnabled();
          // The clip waiting to be sent is shown whichever input is showing.
          chooseLive();
          expect(screen.getByLabelText(t.chatAttachedAudio)).toBeInTheDocument();
          fireEvent.click(sendButton());
          await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalled());
          expect(sessions).toHaveLength(0);
        });
      });

      describe('recording', () => {
        it('starts a session from the click, for the thread, and says it is connecting', async () => {
          await renderStreamingThread();
          chooseLive();

          fireEvent.click(startButton()!);

          expect(sessions).toHaveLength(1);
          expect(sessions[0].args.model).toBe('whisper');
          expect(screen.getByRole('status')).toHaveTextContent(t.chatTranscriptionLiveConnecting);
          expect(startButton()).not.toBeInTheDocument();
          expect(stopButton()).toBeEnabled();
          expect(transcriptRegion()).toBeInTheDocument();
        });

        it('says it is listening, and shows the transcript as it comes: deltas add, a partial replaces', async () => {
          await renderStreamingThread();
          await startRecording();
          expect(screen.getByRole('status')).toHaveTextContent(t.chatTranscriptionLiveListening);

          await act(async () => sessions[0].args.onDelta('Hello wor'));
          expect(transcriptRegion()).toHaveTextContent('Hello wor');
          await act(async () => sessions[0].args.onDelta('ld'));
          expect(transcriptRegion()).toHaveTextContent('Hello world');
          await act(async () => sessions[0].args.onPartial('Hello, world'));
          expect(transcriptRegion()).toHaveTextContent('Hello, world');
          expect(transcriptRegion()).not.toHaveTextContent('Hello world');
        });

        it('holds everything that could start another turn while it runs', async () => {
          await renderStreamingThread();
          await startRecording();

          expect(sendButton()).toBeDisabled();
          // The mode cannot be switched from under it.
          expect(modeButton(t.chatTranscriptionModeFile)).toBeDisabled();
          expect(modeButton(t.chatTranscriptionModeRecord)).toBeDisabled();
        });

        it('Stop finishes it: the transcript is shown as final, then it lands in the chat as a turn', async () => {
          await renderStreamingThread();
          await startRecording();
          await act(async () => sessions[0].args.onDelta('Hello wor'));

          fireEvent.click(stopButton()!);

          expect(sessions[0].stop).toHaveBeenCalledTimes(1);
          expect(screen.getByRole('status')).toHaveTextContent(t.chatTranscriptionRunPending);
          expect(stopButton()).not.toBeInTheDocument();
          expect(transcriptRegion()).toHaveTextContent('Hello wor');

          await act(async () => {
            sessions[0].args.onDone('Hello world.');
            sessions[0].args.onState('closed');
          });

          // The turn is in the transcript (the user's marker, and the answer), and the panel is
          // ready for the next recording.
          const turns = screen.getAllByRole('article');
          expect(turns).toHaveLength(2);
          expect(turns[0]).toHaveTextContent(t.chatTranscriptionLiveTurn);
          expect(turns[1]).toHaveTextContent('Hello world.');
          expect(chatApi.spies.startChatRun).not.toHaveBeenCalled();
          expect(startButton()).toBeEnabled();
          expect(transcriptRegion()).not.toBeInTheDocument();
          // Still a transcription thread: the composer did not turn into the text one.
          expect(screen.getByRole('group', { name: t.chatTranscriptionInputLabel })).toBeVisible();
        });

        it('says it is reconnecting when the connection drops, and carries on', async () => {
          await renderStreamingThread();
          await startRecording();
          await act(async () => sessions[0].args.onDelta('First part'));

          vi.useFakeTimers();
          try {
            act(() => {
              sessions[0].args.onError('closed (1006)', { kind: 'dropped' });
              sessions[0].args.onState('error');
            });
            expect(screen.getByRole('status')).toHaveTextContent(
              t.chatTranscriptionLiveReconnecting,
            );
            // What was transcribed stays on screen, and the recording can still be ended.
            expect(transcriptRegion()).toHaveTextContent('First part');
            expect(stopButton()).toBeEnabled();

            act(() => void vi.advanceTimersByTime(1000));
            expect(sessions).toHaveLength(2);
          } finally {
            vi.useRealTimers();
          }
          await act(async () => sessions[1].args.onState('listening'));
          expect(screen.getByRole('status')).toHaveTextContent(t.chatTranscriptionLiveListening);
          await act(async () => sessions[1].args.onDelta('second'));
          expect(transcriptRegion()).toHaveTextContent('First part second');
        });
      });

      describe('when it fails', () => {
        async function failWith(detail: { kind: 'refused' | 'denied' | 'capture' }, said = '') {
          await startRecording();
          if (said) await act(async () => sessions[0].args.onDelta(said));
          await act(async () => {
            sessions[0].args.onError('it failed', detail);
            sessions[0].args.onState('error');
          });
        }

        it('shows the error as an alert, and can be started again', async () => {
          await renderStreamingThread();
          await failWith({ kind: 'refused' });

          expect(screen.getByRole('alert')).toHaveTextContent(t.chatTranscriptionLiveError);
          expect(screen.queryByRole('button', { name: t.chatTranscriptionLiveUse })).toBeNull();
          expect(startButton()).toBeEnabled();
          // Nothing is running, so the other inputs are reachable again.
          expect(modeButton(t.chatTranscriptionModeFile)).toBeEnabled();

          fireEvent.click(startButton()!);
          expect(sessions).toHaveLength(2);
          expect(screen.queryByRole('alert')).not.toBeInTheDocument();
        });

        it('says what to do about a refused microphone', async () => {
          await renderStreamingThread();
          await failWith({ kind: 'denied' });
          expect(screen.getByRole('alert')).toHaveTextContent(t.chatRecordDenied);
        });

        it('offers what was transcribed before the failure: use it, or throw it away', async () => {
          await renderStreamingThread();
          await failWith({ kind: 'refused' }, 'Worth keeping');
          expect(transcriptRegion()).toHaveTextContent('Worth keeping');

          fireEvent.click(screen.getByRole('button', { name: t.chatTranscriptionLiveUse }));

          const turns = screen.getAllByRole('article');
          expect(turns).toHaveLength(2);
          expect(turns[1]).toHaveTextContent('Worth keeping');
          expect(screen.queryByRole('alert')).not.toBeInTheDocument();
          expect(transcriptRegion()).not.toBeInTheDocument();
        });

        it('does not let Start discard a transcript that can still be used', async () => {
          await renderStreamingThread();
          await failWith({ kind: 'refused' }, 'Worth keeping');

          // Use and Discard are the ways out; Start (which begins from a clean slate) would drop
          // the text without the user having chosen to.
          expect(startButton()).toBeDisabled();
          fireEvent.click(startButton()!);
          expect(sessions).toHaveLength(1);
          expect(transcriptRegion()).toHaveTextContent('Worth keeping');

          fireEvent.click(screen.getByRole('button', { name: t.chatRecordDiscard }));
          expect(startButton()).toBeEnabled();
        });

        it('lets Start through again once the transcript has been used', async () => {
          await renderStreamingThread();
          await failWith({ kind: 'refused' }, 'Worth keeping');
          expect(startButton()).toBeDisabled();

          fireEvent.click(screen.getByRole('button', { name: t.chatTranscriptionLiveUse }));

          expect(startButton()).toBeEnabled();
        });

        it('lets the user throw the transcript away', async () => {
          await renderStreamingThread();
          await failWith({ kind: 'refused' }, 'Not wanted');

          fireEvent.click(screen.getByRole('button', { name: t.chatRecordDiscard }));

          expect(screen.queryAllByRole('article')).toHaveLength(0);
          expect(screen.queryByRole('alert')).not.toBeInTheDocument();
          expect(transcriptRegion()).not.toBeInTheDocument();
        });

        it('switching to another input clears the error', async () => {
          await renderStreamingThread();
          await failWith({ kind: 'refused' });

          fireEvent.click(screen.getByRole('button', { name: t.chatTranscriptionModeFile }));
          chooseLive();

          expect(screen.queryByRole('alert')).not.toBeInTheDocument();
        });
      });

      describe('leaving', () => {
        // The chat view going away (the user opens another page of the portal) while the store,
        // which is mounted app-wide, stays: only the composer's own cleanup can end a recording.
        function renderLeavableChat() {
          function Page() {
            const [shown, setShown] = useState(true);
            const c = useChatStore();
            return (
              <>
                <button type="button" onClick={() => setShown(false)}>
                  leave-chat
                </button>
                {/* What the store holds of the recording, whether or not the chat is shown. */}
                <span data-testid="live-state">{c.liveSession.state}</span>
                {shown && <Chat t={t} />}
              </>
            );
          }
          render(
            <ToastProvider>
              <ChatStoreProvider
                api={chatApi.api}
                models={asrModels}
                tokens={tokens}
                onRefresh={vi.fn(async () => {})}
                t={t}
                locale={locale}
              >
                <Page />
              </ChatStoreProvider>
            </ToastProvider>,
          );
        }
        const leave = () => fireEvent.click(screen.getByRole('button', { name: 'leave-chat' }));

        async function renderLeavableStreamingThread() {
          chatApi.spies.modelTranscriptionStreaming.mockResolvedValue(true);
          renderLeavableChat();
          await waitForChatReady();
          await pickOption(t.chatModel, 'whisper');
          await screen.findByRole('button', { name: t.chatTranscriptionModeLive });
        }

        it('lets go of the microphone when the composer goes away mid-recording', async () => {
          await renderLeavableStreamingThread();
          await startRecording();

          leave();

          expect(sessions[0].abort).toHaveBeenCalledTimes(1);
          expect(screen.getByTestId('live-state')).toHaveTextContent('idle');
        });

        it('lets go of an error and its transcript as well: nothing is left behind to reappear', async () => {
          await renderLeavableStreamingThread();
          await startRecording();
          await act(async () => sessions[0].args.onDelta('left behind'));
          await act(async () => {
            sessions[0].args.onError('it failed', { kind: 'refused' });
            sessions[0].args.onState('error');
          });

          leave();

          // Nothing to abort (the session was over), but the state goes with the composer.
          expect(screen.getByTestId('live-state')).toHaveTextContent('idle');
        });

        it('does not cut off a recording that is already delivering its final transcript', async () => {
          await renderLeavableStreamingThread();
          await startRecording();
          await act(async () => sessions[0].args.onDelta('Almost done'));
          fireEvent.click(stopButton()!);

          leave();
          expect(sessions[0].abort).not.toHaveBeenCalled();

          // The transcript still arrives, and is kept in the thread it was recorded in.
          await act(async () => {
            sessions[0].args.onDone('Almost done.');
            sessions[0].args.onState('closed');
          });
          await waitFor(
            () => {
              const saved = chatApi.spies.saveChat.mock.calls.at(-1)?.[1].content as {
                messages: { content: unknown }[];
              };
              expect(saved.messages.map((m) => m.content)).toEqual([
                t.chatTranscriptionLiveTurn,
                'Almost done.',
              ]);
            },
            { timeout: 2500 },
          );
        });
      });
    });

    describe('the settings panel', () => {
      it('shows language, prompt, temperature and an Advanced section', async () => {
        await renderTranscriptionThread();
        openSettings();

        const language = screen.getByRole('textbox', { name: t.chatTranscriptionLanguage });
        expect(language).toHaveValue('');
        expect(language).toHaveAttribute('placeholder', t.chatTranscriptionDefault);
        expect(language).toHaveAccessibleDescription(t.chatTranscriptionLanguageHint);

        const prompt = screen.getByRole('textbox', { name: t.chatTranscriptionPrompt });
        expect(prompt).toHaveValue('');
        expect(prompt).toHaveAccessibleDescription(t.chatTranscriptionPromptHint);

        // A number, blank by default (the model keeps its own), never negative.
        const temperature = screen.getByRole('spinbutton', {
          name: t.chatTranscriptionTemperature,
        });
        expect(temperature).toHaveValue(null);
        expect(temperature).toHaveAttribute('min', '0');
        expect(temperature).not.toHaveAttribute('max');

        expect(
          screen.getByRole('button', { name: t.chatTranscriptionAdvanced }),
        ).toBeInTheDocument();
      });

      it('has no response format control: the reply is always text, and the panel says so', async () => {
        await renderTranscriptionThread();
        openSettings();
        fireEvent.click(screen.getByRole('button', { name: t.chatTranscriptionAdvanced }));

        // No control of any kind for it...
        expect(screen.queryByLabelText(/format/i)).not.toBeInTheDocument();
        expect(screen.queryByRole('combobox', { name: /format/i })).not.toBeInTheDocument();
        expect(screen.queryByRole('textbox', { name: /format/i })).not.toBeInTheDocument();
        // ...and a static caption instead.
        expect(screen.getByText(t.chatTranscriptionFormatHint)).toBeInTheDocument();

        attachAudio(wavFile());
        await waitFor(() => expect(sendButton()).toBeEnabled());
        fireEvent.click(sendButton());
        await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalled());
        expect(startedBody().settings.transcription).not.toHaveProperty('response_format');
      });

      it('keeps the advanced passthrough collapsed until asked for', async () => {
        await renderTranscriptionThread();
        openSettings();

        const toggle = screen.getByRole('button', { name: t.chatTranscriptionAdvanced });
        expect(toggle).toHaveAttribute('aria-expanded', 'false');
        expect(screen.queryByLabelText(t.chatTranscriptionPassthrough)).not.toBeInTheDocument();

        fireEvent.click(toggle);
        expect(toggle).toHaveAttribute('aria-expanded', 'true');
        const field = screen.getByLabelText(t.chatTranscriptionPassthrough);
        expect(field.tagName).toBe('TEXTAREA');
        expect(field).toHaveValue('');
        expect(field).toHaveAccessibleDescription(t.chatTranscriptionPassthroughHint);

        fireEvent.click(toggle);
        expect(screen.queryByLabelText(t.chatTranscriptionPassthrough)).not.toBeInTheDocument();
      });

      it('sends language, prompt, temperature and the parsed passthrough with the clip', async () => {
        await renderTranscriptionThread();
        openSettings();
        fireEvent.change(screen.getByRole('textbox', { name: t.chatTranscriptionLanguage }), {
          target: { value: 'de' },
        });
        fireEvent.change(screen.getByRole('textbox', { name: t.chatTranscriptionPrompt }), {
          target: { value: 'Namen: Zoe, Jörg' },
        });
        fireEvent.change(screen.getByRole('spinbutton', { name: t.chatTranscriptionTemperature }), {
          target: { value: '0' },
        });
        fireEvent.click(screen.getByRole('button', { name: t.chatTranscriptionAdvanced }));
        fireEvent.change(screen.getByLabelText(t.chatTranscriptionPassthrough), {
          target: { value: '{"beam": 5, "word_timestamps": true}' },
        });
        attachAudio(wavFile());
        await waitFor(() => expect(sendButton()).toBeEnabled());
        fireEvent.click(sendButton());

        await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalled());
        const { settings } = startedBody();
        expect(settings.kind).toBe('transcription');
        // An explicit 0 is a value (deterministic decoding), not blank; the passthrough keeps
        // each value's JSON type.
        expect(settings.transcription).toEqual({
          language: 'de',
          prompt: 'Namen: Zoe, Jörg',
          temperature: 0,
          passthrough: { beam: 5, word_timestamps: true },
        });
        expect(settings).not.toHaveProperty('audio');
        expect(settings).not.toHaveProperty('image');
      });

      it('sends no knob that was left blank', async () => {
        await renderTranscriptionThread();
        attachAudio(wavFile());
        await waitFor(() => expect(sendButton()).toBeEnabled());
        fireEvent.click(sendButton());

        await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalled());
        expect(startedBody().settings.transcription).toEqual({});
      });

      it('seeds the fields from a saved transcription thread', async () => {
        chatApi.rows.push({
          id: 'c_asr_seed',
          title: '',
          created_at: '2026-07-22T12:00:00Z',
          updated_at: '2026-07-22T12:00:00Z',
          content: {
            settings: {
              model: 'whisper',
              kind: 'transcription',
              transcription: {
                language: 'de',
                prompt: 'Zoe',
                temperature: 0.4,
                passthrough: { beam: 5 },
              },
            },
            messages: [],
          },
        });
        renderChat(tokens, asrModels);
        await waitForChatReady();
        openSettings();

        expect(screen.getByRole('textbox', { name: t.chatTranscriptionLanguage })).toHaveValue(
          'de',
        );
        expect(screen.getByRole('textbox', { name: t.chatTranscriptionPrompt })).toHaveValue('Zoe');
        expect(
          screen.getByRole('spinbutton', { name: t.chatTranscriptionTemperature }),
        ).toHaveValue(0.4);
        fireEvent.click(screen.getByRole('button', { name: t.chatTranscriptionAdvanced }));
        expect(screen.getByLabelText(t.chatTranscriptionPassthrough)).toHaveValue(
          JSON.stringify({ beam: 5 }, null, 2),
        );
      });

      it('does not show the transcription knobs on a text or a speech thread', async () => {
        renderChat(tokens, [
          ...models,
          {
            id: 'kokoro',
            display_name: 'kokoro',
            flavors: ['openai_audio'],
            loading_on_count: 0,
            speech: true,
          },
        ]);
        await waitForChatReady();
        for (const model of [models[0].display_name, 'kokoro']) {
          await pickOption(t.chatModel, model);
          openSettings();
          expect(screen.queryByLabelText(t.chatTranscriptionLanguage)).not.toBeInTheDocument();
          expect(screen.queryByLabelText(t.chatTranscriptionPrompt)).not.toBeInTheDocument();
          expect(screen.queryByLabelText(t.chatTranscriptionTemperature)).not.toBeInTheDocument();
          expect(screen.queryByText(t.chatTranscriptionFormatHint)).not.toBeInTheDocument();
          openSettings();
        }
      });
    });

    describe('an advanced passthrough that cannot be sent', () => {
      it('is flagged on the field when it is not a JSON object, without a speech example', async () => {
        await renderTranscriptionThread();
        const field = openPassthrough();

        fireEvent.change(field, { target: { value: '[1, 2]' } });
        expect(field).toBeInvalid();
        expect(field).toHaveAccessibleDescription(t.chatTranscriptionPassthroughInvalid);

        fireEvent.change(field, { target: { value: '{"beam": 5}' } });
        expect(field).toBeValid();
        expect(field).toHaveAccessibleDescription(t.chatTranscriptionPassthroughHint);
      });

      it('flags a field the run owns, naming it', async () => {
        await renderTranscriptionThread();
        const field = openPassthrough();

        fireEvent.change(field, { target: { value: '{"beam": 1, "Response_Format": "srt"}' } });
        expect(field).toBeInvalid();
        expect(field).toHaveAccessibleDescription(
          t.chatTranscriptionPassthroughReserved('Response_Format'),
        );
      });

      it.each([
        ['a double quote', '{"a\\"b": 1}', '"a\\"b"'],
        ['a line break', '{"a\\nb": 1}', '"a\\nb"'],
        ['a backslash', '{"a\\\\b": 1}', '"a\\\\b"'],
        ['an empty name', '{"": 1}', '""'],
      ])('flags a field name with %s, naming it', async (_name, text, shown) => {
        await renderTranscriptionThread();
        const field = openPassthrough();

        fireEvent.change(field, { target: { value: text } });

        expect(field).toBeInvalid();
        expect(field).toHaveAccessibleDescription(t.chatTranscriptionPassthroughBadKey(shown));
      });

      it('refuses the send, with the field and the panel held open, and keeps the clip', async () => {
        await renderTranscriptionThread();
        attachAudio(wavFile('audio/wav', 'held.wav'));
        await screen.findByText('held.wav');
        const field = openPassthrough();
        fireEvent.change(field, { target: { value: '{"a\\"b": 1}' } });

        // The user closes the whole panel, then sends.
        openSettings();
        fireEvent.click(sendButton());

        expect(await screen.findByRole('alert')).toHaveTextContent(
          t.chatTranscriptionPassthroughBadKey('"a\\"b"'),
        );
        expect(chatApi.spies.startChatRun).not.toHaveBeenCalled();
        // Both toggles stay held open so the toast points at a field that is on screen, and
        // nothing the user composed is lost.
        expect(screen.getByLabelText(t.chatTranscriptionPassthrough)).toBeInvalid();
        expect(screen.getByText('held.wav')).toBeInTheDocument();
      });

      it('does not flag a field name a form can carry', async () => {
        await renderTranscriptionThread();
        const field = openPassthrough();

        fireEvent.change(field, {
          target: { value: '{"timestamp_granularities[]": "word", "with space": 1}' },
        });

        expect(field).toBeValid();
      });
    });
  });

  // ADR-043 R1: an image ATTACHED to a turn of an edit-capable image thread makes
  // that turn an image edit. There is no mode and no button: the attach control
  // follows ONE predicate (the store's attachMode) of the thread's kind and the
  // picked model's flags, and the server decides edit vs generation from whether
  // the last user message carries an image.
  describe(`Chat: attaching an image to an image thread edits it (ADR-043 R1) [${locale}]`, () => {
    // An image generator whose mapping may also be edited. `vision` is deliberately
    // absent: sd models are vision:false, and the attach control must not depend on it.
    const editModel: ModelOption = {
      id: 'sd-edit',
      display_name: 'sd-edit',
      flavors: ['openai'],
      loading_on_count: 0,
      image: true,
      image_edit: true,
    };
    // The same, with the operator's opt-out (image_edit: no): it still generates.
    const noEditModel: ModelOption = {
      ...editModel,
      id: 'sd-noedit',
      display_name: 'sd-noedit',
      image_edit: false,
    };
    const visionModel: ModelOption = {
      id: 'vision-model',
      display_name: 'vision-model',
      flavors: ['openai'],
      loading_on_count: 0,
      vision: true,
    };

    function attach(...names: string[]) {
      fireEvent.change(screen.getByLabelText(t.chatAttachImage), {
        target: { files: names.map((n) => new File(['x'], n, { type: 'image/png' })) },
      });
    }

    it('enables the attach control for an edit-capable image model, without needing vision', async () => {
      renderChat(tokens, [editModel]);
      await waitForChatReady();
      await pickOption(t.chatModel, editModel.display_name);

      expect(screen.getByRole('button', { name: t.chatAttachImage })).not.toHaveAttribute(
        'aria-disabled',
        'true',
      );
      fireEvent.mouseOver(screen.getByRole('button', { name: t.chatAttachImage }));
      const tip = await screen.findByRole('tooltip');
      expect(tip).toHaveTextContent(t.chatAttachImageEdit);
    });

    it('says the thread edits too, in the prompt hint, only for an edit-capable model', async () => {
      renderChat(tokens, [editModel, noEditModel]);
      await waitForChatReady();

      await pickOption(t.chatModel, editModel.display_name);
      expect(screen.getByText(t.chatImageEditHint)).toBeInTheDocument();
      expect(screen.queryByText(t.chatImageOnlyHint)).not.toBeInTheDocument();

      // The non-edit-capable generator keeps today's wording, and attach stays
      // refused with the generator sentence.
      await pickOption(t.chatModel, noEditModel.display_name);
      expect(screen.getByText(t.chatImageOnlyHint)).toBeInTheDocument();
      expect(screen.queryByText(t.chatImageEditHint)).not.toBeInTheDocument();
      expect(screen.getByRole('button', { name: t.chatAttachImage })).toHaveAttribute(
        'aria-disabled',
        'true',
      );
      fireEvent.mouseOver(screen.getByRole('button', { name: t.chatAttachImage }));
      expect(await screen.findByRole('tooltip')).toHaveTextContent(t.chatImageGeneratorNoInput);
    });

    it('offers a source image for ONE image only: the picker takes a single file', async () => {
      renderChat(tokens, [editModel, visionModel]);
      await waitForChatReady();

      await pickOption(t.chatModel, visionModel.display_name);
      expect(screen.getByLabelText(t.chatAttachImage)).toHaveAttribute('multiple');

      await pickOption(t.chatModel, editModel.display_name);
      expect(screen.getByLabelText(t.chatAttachImage)).not.toHaveAttribute('multiple');
    });

    it('refuses a SECOND source image with its own message', async () => {
      renderChat(tokens, [editModel]);
      await waitForChatReady();
      await pickOption(t.chatModel, editModel.display_name);

      attach('a.png');
      await screen.findByAltText(t.chatAttachedImage);
      attach('b.png');

      expect(await screen.findByRole('alert')).toHaveTextContent(t.chatImageEditSingleSource);
      expect(screen.getAllByAltText(t.chatAttachedImage)).toHaveLength(1);
    });

    it('sends an attached image as the turn: the image part rides the user message of an image run', async () => {
      renderChat(tokens, [editModel]);
      await waitForChatReady();
      await pickOption(t.chatModel, editModel.display_name);

      attach('source.png');
      await screen.findByAltText(t.chatAttachedImage);
      fireEvent.change(screen.getByLabelText(t.chatImagePromptLabel), {
        target: { value: 'make it blue' },
      });
      fireEvent.click(screen.getByRole('button', { name: t.send }));

      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalled());
      const body = chatApi.spies.startChatRun.mock.calls[0][1] as StartChatRunBody;
      // The kind stays "image": there is no edit kind, the attachment is the operand.
      expect(body.settings.kind).toBe('image');
      expect(body.user_message).toEqual([
        { type: 'text', text: 'make it blue' },
        {
          type: 'image_url',
          image_url: { url: expect.stringMatching(/^data:image\/png;base64,/) },
        },
      ]);
      // The composer's attachment strip is cleared by the send, like any turn's.
      await waitFor(() => expect(screen.getByLabelText(t.chatImagePromptLabel)).toHaveValue(''));
    });

    it('keeps the thread an image thread across an edit turn, and a later plain prompt is a plain turn', async () => {
      renderChat(tokens, [editModel]);
      await waitForChatReady();
      await pickOption(t.chatModel, editModel.display_name);

      attach('source.png');
      await screen.findByAltText(t.chatAttachedImage);
      fireEvent.change(screen.getByLabelText(t.chatImagePromptLabel), {
        target: { value: 'edit it' },
      });
      fireEvent.click(screen.getByRole('button', { name: t.send }));
      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
      await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
      // The real backend commits the canonical transcript -- the source and the
      // result, both as stored references -- before it emits `done`, and the
      // store refetches it right after.
      const generated = [
        { type: 'image_url', image_url: { url: '/api/portal/chat-blobs/blob_1' } },
      ];
      chatApi.rows[0].content = {
        settings: { model: editModel.id, kind: 'image' },
        messages: [
          {
            id: 'u1',
            role: 'user',
            content: [
              { type: 'text', text: 'edit it' },
              { type: 'image_url', image_url: { url: '/api/portal/chat-blobs/blob_src' } },
            ],
          },
          { id: 'a1', role: 'assistant', content: generated, status: 'complete' },
        ],
      };
      await act(async () => {
        FakeEventSource.instances[0].emit('done', {
          content: '',
          status: 'completed',
          content_parts: generated,
        });
      });
      // The canonical refetch (a new chat is created, not loaded, so this is the
      // first chat() call there is).
      await waitFor(() => expect(chatApi.spies.chat).toHaveBeenCalled());
      await waitFor(() => expect(screen.queryByRole('button', { name: t.chatStop })).toBeNull());

      // Still an image thread: the image prompt label, not the text one.
      expect(screen.getByLabelText(t.chatImagePromptLabel)).toBeInTheDocument();
      expect(screen.queryByLabelText(t.messageLabel)).not.toBeInTheDocument();

      // The next turn has no attachment: a string message, the same kind -- the
      // server's executor, not this client, reads that as a generation.
      fireEvent.change(screen.getByLabelText(t.chatImagePromptLabel), {
        target: { value: 'a dog' },
      });
      fireEvent.click(screen.getByRole('button', { name: t.send }));
      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(2));
      const second = chatApi.spies.startChatRun.mock.calls[1][1] as StartChatRunBody;
      expect(second.settings.kind).toBe('image');
      expect(second.user_message).toBe('a dog');
    });

    it('clears an attached source when the thread moves to a model that cannot edit', async () => {
      renderChat(tokens, [editModel, noEditModel]);
      await waitForChatReady();
      await pickOption(t.chatModel, editModel.display_name);
      attach('source.png');
      await screen.findByAltText(t.chatAttachedImage);

      await pickOption(t.chatModel, noEditModel.display_name);

      await waitFor(() =>
        expect(screen.queryByAltText(t.chatAttachedImage)).not.toBeInTheDocument(),
      );
      expect(await screen.findByRole('alert')).toHaveTextContent(t.chatImageGeneratorNoInput);
    });

    it('keeps an attachment when a vision model is swapped for an edit-capable one: it becomes the source', async () => {
      renderChat(tokens, [visionModel, editModel]);
      await waitForChatReady();
      await pickOption(t.chatModel, visionModel.display_name);
      attach('source.png');
      await screen.findByAltText(t.chatAttachedImage);

      await pickOption(t.chatModel, editModel.display_name);

      expect(screen.getByAltText(t.chatAttachedImage)).toBeInTheDocument();
      expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    });

    it('refuses to send several images as one edit, and starts no run', async () => {
      renderChat(tokens, [visionModel, editModel]);
      await waitForChatReady();
      await pickOption(t.chatModel, visionModel.display_name);
      attach('a.png', 'b.png');
      await waitFor(() => expect(screen.getAllByAltText(t.chatAttachedImage)).toHaveLength(2));

      await pickOption(t.chatModel, editModel.display_name);
      fireEvent.change(screen.getByLabelText(t.chatImagePromptLabel), {
        target: { value: 'edit' },
      });
      fireEvent.click(screen.getByRole('button', { name: t.send }));

      expect(await screen.findByRole('alert')).toHaveTextContent(t.chatImageEditSingleSource);
      expect(chatApi.spies.startChatRun).not.toHaveBeenCalled();
      // Nothing was lost: the user removes one and sends.
      expect(screen.getAllByAltText(t.chatAttachedImage)).toHaveLength(2);
    });

    it('does not offer attach on a text thread for an image-edit model that is also vision-capable', async () => {
      // chatKind "" (a text thread) + a model that is vision AND an image model:
      // the text thread's rule is vision && !image, so this stays refused.
      chatApi.rows.push({
        id: 'c_text',
        title: 'T',
        created_at: '2026-07-19T12:00:00Z',
        updated_at: '2026-07-19T12:00:00Z',
        content: {
          settings: { model: 'sd-vision-edit' },
          messages: [
            { id: 'u1', role: 'user', content: 'hello' },
            { id: 'a1', role: 'assistant', content: 'hi', status: 'complete' },
          ],
        },
      });
      renderChat(tokens, [
        { ...editModel, id: 'sd-vision-edit', display_name: 'sd-vision-edit', vision: true },
      ]);
      await screen.findByText('T');
      await waitFor(() =>
        expect(screen.getByRole('button', { name: t.chatAttachImage })).toHaveAttribute(
          'aria-disabled',
          'true',
        ),
      );
    });
  });

  // Two defects of the edit turn's send path that Task D left (its review's
  // minors): an image-only edit was stored and then failed mid-run with
  // images.prompt_required, and a webp/gif source passed the picker only to be
  // refused by the edit route of sd-server (png and jpeg) as a generic upstream
  // error. Both are now refused where the user can still act on them -- in the
  // composer, before anything is sent or stored.
  describe(`Chat: an edit turn needs a prompt and a png/jpeg source [${locale}]`, () => {
    const editModel: ModelOption = {
      id: 'sd-edit',
      display_name: 'sd-edit',
      flavors: ['openai'],
      loading_on_count: 0,
      image: true,
      image_edit: true,
    };
    const visionModel: ModelOption = {
      id: 'vision-model',
      display_name: 'vision-model',
      flavors: ['openai'],
      loading_on_count: 0,
      vision: true,
    };

    function attachFile(name: string, type: string) {
      fireEvent.change(screen.getByLabelText(t.chatAttachImage), {
        target: { files: [new File(['x'], name, { type })] },
      });
    }

    describe('the prompt', () => {
      it('is required for an edit: an image-only turn is refused before it is sent or stored', async () => {
        renderChat(tokens, [editModel]);
        await waitForChatReady();
        await pickOption(t.chatModel, editModel.display_name);
        attachFile('source.png', 'image/png');
        await screen.findByAltText(t.chatAttachedImage);

        fireEvent.click(screen.getByRole('button', { name: t.send }));

        expect(await screen.findByRole('alert')).toHaveTextContent(t.chatImageEditPromptRequired);
        expect(chatApi.spies.startChatRun).not.toHaveBeenCalled();
        // Nothing was stored, nothing was lost: no user bubble appeared (the only
        // thumbnail is still the composer's), and the attachment is still there to
        // be sent once the prompt is written.
        expect(screen.getAllByAltText(t.chatAttachedImage)).toHaveLength(1);
        expect(screen.queryByRole('article')).not.toBeInTheDocument();

        fireEvent.change(screen.getByLabelText(t.chatImagePromptLabel), {
          target: { value: 'make it blue' },
        });
        fireEvent.click(screen.getByRole('button', { name: t.send }));
        await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
      });

      it('must have a word in it: whitespace alone does not count', async () => {
        renderChat(tokens, [editModel]);
        await waitForChatReady();
        await pickOption(t.chatModel, editModel.display_name);
        attachFile('source.png', 'image/png');
        await screen.findByAltText(t.chatAttachedImage);
        fireEvent.change(screen.getByLabelText(t.chatImagePromptLabel), {
          target: { value: '   \n ' },
        });

        fireEvent.click(screen.getByRole('button', { name: t.send }));

        expect(await screen.findByRole('alert')).toHaveTextContent(t.chatImageEditPromptRequired);
        expect(chatApi.spies.startChatRun).not.toHaveBeenCalled();
      });

      it('stays optional for a text model reading an image: only an EDIT needs one', async () => {
        renderChat(tokens, [visionModel]);
        await waitForChatReady();
        await pickOption(t.chatModel, visionModel.display_name);
        attachFile('photo.png', 'image/png');
        await screen.findByAltText(t.chatAttachedImage);

        fireEvent.click(screen.getByRole('button', { name: t.send }));

        await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
        expect(screen.queryByText(t.chatImageEditPromptRequired)).not.toBeInTheDocument();
      });
    });

    describe('the source format', () => {
      it('is png, jpeg and heic/heif in the picker of an edit, and any image for a text model', async () => {
        renderChat(tokens, [editModel, visionModel]);
        await waitForChatReady();

        await pickOption(t.chatModel, visionModel.display_name);
        expect(screen.getByLabelText(t.chatAttachImage)).toHaveAttribute('accept', 'image/*');

        // HEIC/HEIF are listed because the upload pipeline converts them to JPEG
        // before they are attached (so sd-server never sees one); a browser that
        // reports no MIME type for them matches on the extension.
        await pickOption(t.chatModel, editModel.display_name);
        expect(screen.getByLabelText(t.chatAttachImage)).toHaveAttribute(
          'accept',
          'image/png,image/jpeg,image/heic,image/heif,.heic,.heif',
        );
      });

      it.each([
        ['webp', 'image/webp'],
        ['gif', 'image/gif'],
      ])('refuses a %s source for an edit, however it got past the picker', async (ext, type) => {
        vi.mocked(heicTo).mockClear();
        renderChat(tokens, [editModel]);
        await waitForChatReady();
        await pickOption(t.chatModel, editModel.display_name);

        attachFile(`source.${ext}`, type);

        expect(await screen.findByRole('alert')).toHaveTextContent(t.chatImageEditSourceType);
        expect(screen.queryByAltText(t.chatAttachedImage)).not.toBeInTheDocument();
        expect(heicTo).not.toHaveBeenCalled();
      });

      // A HEIC photo (an iPhone's default) was a working edit source before the
      // png/jpeg restriction, because the upload pipeline already converts it to
      // JPEG: the restriction is about what sd-server SEES, and it never sees HEIC.
      it.each([
        ['by its MIME type', 'photo.heic', 'image/heic'],
        ['by its MIME type, the heif spelling', 'photo.heif', 'image/heif'],
        ['by its extension when the browser reports no MIME type', 'IMG_0001.HEIC', ''],
      ])('takes a HEIC source for an edit %s and sends it as a JPEG', async (_how, name, type) => {
        vi.mocked(heicTo).mockClear();
        renderChat(tokens, [editModel]);
        await waitForChatReady();
        await pickOption(t.chatModel, editModel.display_name);

        attachFile(name, type);

        expect(await screen.findByAltText(t.chatAttachedImage)).toBeInTheDocument();
        expect(screen.queryByRole('alert')).not.toBeInTheDocument();
        expect(heicTo).toHaveBeenCalledWith(expect.objectContaining({ type: 'image/jpeg' }));
        fireEvent.change(screen.getByLabelText(t.chatImagePromptLabel), {
          target: { value: 'make it blue' },
        });
        fireEvent.click(screen.getByRole('button', { name: t.send }));
        await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
        const body = chatApi.spies.startChatRun.mock.calls[0][1] as StartChatRunBody;
        const parts = body.user_message as { type: string; image_url?: { url: string } }[];
        // What goes to the server -- and on to sd-server -- is JPEG, not HEIC.
        expect(parts[1].image_url?.url).toMatch(/^data:image\/jpeg;base64,/);
      });

      it.each([
        ['png', 'image/png'],
        ['jpg', 'image/jpeg'],
      ])('takes a %s source for an edit', async (ext, type) => {
        renderChat(tokens, [editModel]);
        await waitForChatReady();
        await pickOption(t.chatModel, editModel.display_name);

        attachFile(`source.${ext}`, type);

        expect(await screen.findByAltText(t.chatAttachedImage)).toBeInTheDocument();
        expect(screen.queryByRole('alert')).not.toBeInTheDocument();
      });

      it('still takes a webp image for a text model that reads images', async () => {
        renderChat(tokens, [visionModel]);
        await waitForChatReady();
        await pickOption(t.chatModel, visionModel.display_name);

        attachFile('photo.webp', 'image/webp');

        expect(await screen.findByAltText(t.chatAttachedImage)).toBeInTheDocument();
        expect(screen.queryByRole('alert')).not.toBeInTheDocument();
      });
    });
  });

  // Masked inpainting (ADR-043 R1): an edit may also carry a MASK -- a PNG, the size
  // of the source image, whose transparent area marks the region to change. It has
  // its own slot next to the attach button, takes exactly one PNG, needs the source
  // first, and rides the turn as an image part tagged purpose "mask" (chatDoc.ts), so
  // the server stores, resolves and replays it like the source and sends it as the
  // `mask` form field. (The drawing canvas is a later step: this is the upload.)
  describe(`Chat: masked inpainting upload (ADR-043 R1) [${locale}]`, () => {
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
    const visionModel: ModelOption = {
      id: 'vision-model',
      display_name: 'vision-model',
      flavors: ['openai'],
      loading_on_count: 0,
      vision: true,
    };

    function attachSource(name = 'source.png') {
      fireEvent.change(screen.getByLabelText(t.chatAttachImage), {
        target: { files: [new File(['s'], name, { type: 'image/png' })] },
      });
    }
    function attachMask(name = 'mask.png', type = 'image/png') {
      fireEvent.change(screen.getByLabelText(t.chatAttachMask), {
        target: { files: [new File(['m'], name, { type })] },
      });
    }
    const maskButton = () => screen.getByRole('button', { name: t.chatAttachMask });

    async function editThreadWithSource() {
      renderChat(tokens, [editModel, noEditModel, visionModel]);
      await waitForChatReady();
      await pickOption(t.chatModel, editModel.display_name);
      attachSource();
      await screen.findByAltText(t.chatAttachedImage);
    }

    describe('the slot', () => {
      it('is offered for an edit only, and not for a text model reading images', async () => {
        renderChat(tokens, [editModel, noEditModel, visionModel]);
        await waitForChatReady();

        await pickOption(t.chatModel, visionModel.display_name);
        expect(screen.queryByLabelText(t.chatAttachMask)).not.toBeInTheDocument();

        await pickOption(t.chatModel, noEditModel.display_name);
        expect(screen.queryByLabelText(t.chatAttachMask)).not.toBeInTheDocument();

        await pickOption(t.chatModel, editModel.display_name);
        expect(screen.getByLabelText(t.chatAttachMask)).toBeInTheDocument();
      });

      it('waits for the source image: disabled, and saying why, until one is attached', async () => {
        renderChat(tokens, [editModel]);
        await waitForChatReady();
        await pickOption(t.chatModel, editModel.display_name);

        expect(maskButton()).toHaveAttribute('aria-disabled', 'true');
        fireEvent.mouseOver(maskButton());
        expect(await screen.findByRole('tooltip')).toHaveTextContent(t.chatMaskNeedsSource);

        attachSource();
        await screen.findByAltText(t.chatAttachedImage);
        await waitFor(() => expect(maskButton()).not.toHaveAttribute('aria-disabled', 'true'));
        fireEvent.mouseOver(maskButton());
        await waitFor(() =>
          expect(screen.getByRole('tooltip')).toHaveTextContent(t.chatAttachMaskTooltip),
        );
      });

      it('takes exactly one PNG: the picker offers png only and no multiple selection', async () => {
        await editThreadWithSource();

        const input = screen.getByLabelText(t.chatAttachMask);
        expect(input).toHaveAttribute('accept', 'image/png');
        expect(input).not.toHaveAttribute('multiple');
      });
    });

    describe('attaching a mask', () => {
      it('shows it beside the source, under its own name, and removes it on request', async () => {
        await editThreadWithSource();

        attachMask();

        expect(await screen.findByAltText(t.chatAttachedMask)).toBeInTheDocument();
        // The source is still the one attached image: a mask is not a second one.
        expect(screen.getAllByAltText(t.chatAttachedImage)).toHaveLength(1);

        fireEvent.click(screen.getByRole('button', { name: t.chatRemoveMask }));

        expect(screen.queryByAltText(t.chatAttachedMask)).not.toBeInTheDocument();
        expect(screen.getAllByAltText(t.chatAttachedImage)).toHaveLength(1);
      });

      it.each([
        ['a jpeg', 'mask.jpg', 'image/jpeg'],
        ['a webp', 'mask.webp', 'image/webp'],
        ['a gif', 'mask.gif', 'image/gif'],
      ])('refuses %s: a mask is a PNG, its alpha is the selection', async (_what, name, type) => {
        await editThreadWithSource();

        attachMask(name, type);

        expect(await screen.findByRole('alert')).toHaveTextContent(t.chatMaskErrorType);
        expect(screen.queryByAltText(t.chatAttachedMask)).not.toBeInTheDocument();
      });

      it('refuses a mask while no source image is attached, however the picker was reached', async () => {
        renderChat(tokens, [editModel]);
        await waitForChatReady();
        await pickOption(t.chatModel, editModel.display_name);

        // The control is disabled, but a change event still reaches the input.
        attachMask();

        expect(await screen.findByRole('alert')).toHaveTextContent(t.chatMaskNeedsSource);
        expect(screen.queryByAltText(t.chatAttachedMask)).not.toBeInTheDocument();
      });
    });

    describe('sending', () => {
      it('sends the mask after the source as an image part tagged purpose "mask"', async () => {
        await editThreadWithSource();
        attachMask();
        await screen.findByAltText(t.chatAttachedMask);
        fireEvent.change(screen.getByLabelText(t.chatImagePromptLabel), {
          target: { value: 'fill the sky' },
        });

        fireEvent.click(screen.getByRole('button', { name: t.send }));

        await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
        const body = chatApi.spies.startChatRun.mock.calls[0][1] as StartChatRunBody;
        expect(body.settings.kind).toBe('image');
        expect(body.user_message).toEqual([
          { type: 'text', text: 'fill the sky' },
          {
            type: 'image_url',
            image_url: { url: expect.stringMatching(/^data:image\/png;base64,/) },
          },
          {
            type: 'image_url',
            image_url: { url: expect.stringMatching(/^data:image\/png;base64,/) },
            purpose: 'mask',
          },
        ]);
        // The source part carries no tag: only the mask is told apart.
        const parts = body.user_message as { purpose?: string }[];
        expect(parts[1]).not.toHaveProperty('purpose');
        // The turn shows both, apart; the composer's own strip is empty again.
        expect(screen.getAllByAltText(t.chatAttachedImage)).toHaveLength(1);
        expect(screen.getAllByAltText(t.chatAttachedMask)).toHaveLength(1);
      });

      it('sends an edit with no mask exactly as before: the source alone, untagged', async () => {
        await editThreadWithSource();
        fireEvent.change(screen.getByLabelText(t.chatImagePromptLabel), {
          target: { value: 'make it blue' },
        });

        fireEvent.click(screen.getByRole('button', { name: t.send }));

        await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
        const body = chatApi.spies.startChatRun.mock.calls[0][1] as StartChatRunBody;
        expect(body.user_message).toStrictEqual([
          { type: 'text', text: 'make it blue' },
          {
            type: 'image_url',
            image_url: { url: expect.stringMatching(/^data:image\/png;base64,/) },
          },
        ]);
      });
    });

    describe('a mask cannot outlive what it belongs to', () => {
      it('goes when its source image is removed', async () => {
        await editThreadWithSource();
        attachMask();
        await screen.findByAltText(t.chatAttachedMask);

        // The source's remove button is the first of the strip.
        fireEvent.click(screen.getAllByRole('button', { name: t.chatCancel })[0]);

        await waitFor(() =>
          expect(screen.queryByAltText(t.chatAttachedMask)).not.toBeInTheDocument(),
        );
        expect(screen.queryByAltText(t.chatAttachedImage)).not.toBeInTheDocument();
        expect(maskButton()).toHaveAttribute('aria-disabled', 'true');
      });

      it('goes with the source when the thread moves to a model that cannot edit', async () => {
        await editThreadWithSource();
        attachMask();
        await screen.findByAltText(t.chatAttachedMask);

        await pickOption(t.chatModel, noEditModel.display_name);

        await waitFor(() =>
          expect(screen.queryByAltText(t.chatAttachedMask)).not.toBeInTheDocument(),
        );
        expect(screen.queryByAltText(t.chatAttachedImage)).not.toBeInTheDocument();
        expect(screen.queryByLabelText(t.chatAttachMask)).not.toBeInTheDocument();
      });

      it('goes alone when an edit model is swapped for a text model that reads images: the image stays', async () => {
        await editThreadWithSource();
        attachMask();
        await screen.findByAltText(t.chatAttachedMask);

        await pickOption(t.chatModel, visionModel.display_name);

        await waitFor(() =>
          expect(screen.queryByAltText(t.chatAttachedMask)).not.toBeInTheDocument(),
        );
        expect(screen.getByAltText(t.chatAttachedImage)).toBeInTheDocument();
      });
    });
  });

  // Iterative refine: a per-image action on a GENERATED image seeds the composer
  // with that image as the source of the next edit, by its existing stored
  // reference. The user types the instruction and sends; from there it is an
  // ordinary edit turn (the prompt rule, the optional mask, the replay handling).
  describe(`Chat: refining a generated image in place (ADR-043 R1) [${locale}]`, () => {
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
    const visionModel: ModelOption = {
      id: 'vision-model',
      display_name: 'vision-model',
      flavors: ['openai'],
      loading_on_count: 0,
      vision: true,
    };
    const resultRef = '/api/portal/chat-blobs/blob_result';
    const uploadRef = '/api/portal/chat-blobs/blob_upload';

    // A chat holding one generation: the prompt and its result, as the server stores
    // them (the image is a gateway reference, not inline bytes).
    function seedGeneration(model: ModelOption, kind = 'image', extra: unknown[] = []) {
      chatApi.rows.push({
        id: 'c_refine',
        title: 'Lighthouse',
        created_at: '2026-07-19T12:00:00Z',
        updated_at: '2026-07-19T12:00:00Z',
        content: {
          settings: { model: model.id, ...(kind ? { kind } : {}) },
          messages: [
            { id: 'u1', role: 'user', content: 'a lighthouse' },
            {
              id: 'a1',
              role: 'assistant',
              content: [{ type: 'image_url', image_url: { url: resultRef } }],
              status: 'complete',
            },
            ...extra,
          ],
        },
      });
    }
    const refineControl = () => screen.queryByRole('button', { name: t.chatRefineImage });
    function attachSource(name = 'source.png') {
      fireEvent.change(screen.getByLabelText(t.chatAttachImage), {
        target: { files: [new File(['s'], name, { type: 'image/png' })] },
      });
    }
    function attachMask() {
      fireEvent.change(screen.getByLabelText(t.chatAttachMask), {
        target: { files: [new File(['m'], 'mask.png', { type: 'image/png' })] },
      });
    }

    it('offers the action on the generated image of an image thread with an edit-capable model', async () => {
      seedGeneration(editModel);
      renderChat(tokens, [editModel]);
      await screen.findByText('Lighthouse');

      expect(await screen.findByRole('button', { name: t.chatRefineImage })).toBeInTheDocument();
    });

    it('seeds the composer with the image by its stored reference, focuses the prompt, and sends that reference as the source', async () => {
      seedGeneration(editModel);
      renderChat(tokens, [editModel]);
      await screen.findByText('Lighthouse');

      fireEvent.click(await screen.findByRole('button', { name: t.chatRefineImage }));

      // The composer's thumbnail is the result's own url -- not a fetched copy.
      const thumbnail = await screen.findByAltText(t.chatAttachedImage);
      expect(thumbnail).toHaveAttribute('src', resultRef);
      // The user's next move is the instruction: the prompt field has the focus.
      const prompt = screen.getByLabelText(t.chatImagePromptLabel);
      await waitFor(() => expect(prompt).toHaveFocus());
      expect(chatApi.spies.startChatRun).not.toHaveBeenCalled();

      fireEvent.change(prompt, { target: { value: 'make the sky purple' } });
      fireEvent.click(screen.getByRole('button', { name: t.send }));

      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
      const body = chatApi.spies.startChatRun.mock.calls[0][1] as StartChatRunBody;
      expect(body.settings.kind).toBe('image');
      expect(body.user_message).toStrictEqual([
        { type: 'text', text: 'make the sky purple' },
        { type: 'image_url', image_url: { url: resultRef } },
      ]);
      // Nothing was copied into the request: no inline bytes anywhere.
      expect(JSON.stringify(body)).not.toContain('data:');
    });

    it('does not send a refine without an instruction: the image stays attached and the field says why', async () => {
      seedGeneration(editModel);
      renderChat(tokens, [editModel]);
      await screen.findByText('Lighthouse');
      fireEvent.click(await screen.findByRole('button', { name: t.chatRefineImage }));
      await screen.findByAltText(t.chatAttachedImage);

      fireEvent.click(screen.getByRole('button', { name: t.send }));

      expect(await screen.findByRole('alert')).toHaveTextContent(t.chatImageEditPromptRequired);
      expect(chatApi.spies.startChatRun).not.toHaveBeenCalled();
      expect(screen.getByAltText(t.chatAttachedImage)).toHaveAttribute('src', resultRef);
    });

    it('lets the user add a mask to the refine, which rides the same turn after the source', async () => {
      seedGeneration(editModel);
      renderChat(tokens, [editModel]);
      await screen.findByText('Lighthouse');
      fireEvent.click(await screen.findByRole('button', { name: t.chatRefineImage }));
      await screen.findByAltText(t.chatAttachedImage);

      // The mask slot is open (a source is attached) and nothing was attached for us.
      expect(screen.queryByAltText(t.chatAttachedMask)).not.toBeInTheDocument();
      attachMask();
      await screen.findByAltText(t.chatAttachedMask);
      fireEvent.change(screen.getByLabelText(t.chatImagePromptLabel), {
        target: { value: 'fill the sky' },
      });
      fireEvent.click(screen.getByRole('button', { name: t.send }));

      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
      const body = chatApi.spies.startChatRun.mock.calls[0][1] as StartChatRunBody;
      expect(body.user_message).toStrictEqual([
        { type: 'text', text: 'fill the sky' },
        { type: 'image_url', image_url: { url: resultRef } },
        {
          type: 'image_url',
          image_url: { url: expect.stringMatching(/^data:image\/png;base64,/) },
          purpose: 'mask',
        },
      ]);
    });

    it('swaps a source attached by hand for the refined image, and drops the mask that belonged to it', async () => {
      seedGeneration(editModel);
      renderChat(tokens, [editModel]);
      await screen.findByText('Lighthouse');
      attachSource();
      await screen.findByAltText(t.chatAttachedImage);
      attachMask();
      await screen.findByAltText(t.chatAttachedMask);

      fireEvent.click(await screen.findByRole('button', { name: t.chatRefineImage }));

      await waitFor(() =>
        expect(screen.getByAltText(t.chatAttachedImage)).toHaveAttribute('src', resultRef),
      );
      // One source, and the old source's mask is gone with it: it was drawn for
      // another image and would not fit this one.
      expect(screen.getAllByAltText(t.chatAttachedImage)).toHaveLength(1);
      expect(screen.queryByAltText(t.chatAttachedMask)).not.toBeInTheDocument();
    });

    // The composer's attachment follows the user across chats -- except a reference,
    // which names a blob of the chat it was refined in (sent in another chat it would
    // be refused as images.blob_not_owned). The two halves are one flow: only the kind
    // of the attachment differs, so a switch that dropped everything, or kept
    // everything, fails one of them.
    describe('switching chats', () => {
      async function twoImageChats() {
        seedGeneration(editModel);
        chatApi.rows.push({
          id: 'c_other',
          title: 'Other',
          created_at: '2026-07-18T12:00:00Z',
          updated_at: '2026-07-18T12:00:00Z',
          content: { settings: { model: editModel.id, kind: 'image' }, messages: [] },
        });
        renderChat(tokens, [editModel]);
        await screen.findByText('Lighthouse');
        await screen.findByText('Other');
      }
      const openOther = async () => {
        fireEvent.click(screen.getByText('Other'));
        // The generation of the first chat is gone: the second chat is on screen.
        await waitFor(() => expect(screen.queryByText('a lighthouse')).not.toBeInTheDocument());
      };

      it('keeps an uploaded source: it is only bytes, and goes where the composer goes', async () => {
        await twoImageChats();
        attachSource();
        await screen.findByAltText(t.chatAttachedImage);

        await openOther();

        expect(screen.getByAltText(t.chatAttachedImage)).toHaveAttribute(
          'src',
          expect.stringMatching(/^data:image\/png;base64,/),
        );
      });

      it('drops a refined reference: it names a blob of the chat it was refined in', async () => {
        await twoImageChats();
        fireEvent.click(await screen.findByRole('button', { name: t.chatRefineImage }));
        await screen.findByAltText(t.chatAttachedImage);

        await openOther();

        expect(screen.queryByAltText(t.chatAttachedImage)).not.toBeInTheDocument();
      });
    });

    it('is not offered on an image thread whose model cannot be edited', async () => {
      seedGeneration(noEditModel);
      renderChat(tokens, [noEditModel]);
      await screen.findByText('Lighthouse');
      // The generated image itself is there...
      expect(await screen.findByRole('button', { name: t.chatDownloadImage })).toBeInTheDocument();

      // ...but the action is not: it could not run.
      expect(refineControl()).toBeNull();
    });

    it('is not offered on a text thread', async () => {
      seedGeneration(visionModel, '');
      renderChat(tokens, [visionModel]);
      await screen.findByText('Lighthouse');
      expect(await screen.findByRole('button', { name: t.chatDownloadImage })).toBeInTheDocument();

      expect(refineControl()).toBeNull();
    });

    it('follows the picked model: gone on one that cannot edit, back on one that can', async () => {
      seedGeneration(editModel);
      renderChat(tokens, [editModel, noEditModel]);
      await screen.findByText('Lighthouse');
      expect(await screen.findByRole('button', { name: t.chatRefineImage })).toBeInTheDocument();

      await pickOption(t.chatModel, noEditModel.display_name);
      await waitFor(() => expect(refineControl()).toBeNull());

      await pickOption(t.chatModel, editModel.display_name);
      expect(await screen.findByRole('button', { name: t.chatRefineImage })).toBeInTheDocument();
    });

    it('is for generated results only: the source image of an earlier edit has no such control', async () => {
      seedGeneration(editModel, 'image', [
        {
          id: 'u2',
          role: 'user',
          content: [
            { type: 'text', text: 'make it blue' },
            { type: 'image_url', image_url: { url: uploadRef } },
          ],
        },
        {
          id: 'a2',
          role: 'assistant',
          content: [{ type: 'image_url', image_url: { url: '/api/portal/chat-blobs/blob_two' } }],
          status: 'complete',
        },
      ]);
      renderChat(tokens, [editModel]);
      await screen.findByText('Lighthouse');

      // Two generated results, two controls; the user turn's thumbnail adds none.
      await waitFor(() =>
        expect(screen.getAllByRole('button', { name: t.chatRefineImage })).toHaveLength(2),
      );
      expect(screen.getAllByRole('button', { name: t.chatDownloadImage })).toHaveLength(2);
    });

    it('refines the image the user clicked: a newer result seeds ITS reference', async () => {
      const newerRef = '/api/portal/chat-blobs/blob_newer';
      seedGeneration(editModel, 'image', [
        {
          id: 'u2',
          role: 'user',
          content: [
            { type: 'text', text: 'make it blue' },
            { type: 'image_url', image_url: { url: resultRef } },
          ],
        },
        {
          id: 'a2',
          role: 'assistant',
          content: [{ type: 'image_url', image_url: { url: newerRef } }],
          status: 'complete',
        },
      ]);
      renderChat(tokens, [editModel]);
      await screen.findByText('Lighthouse');
      await waitFor(() =>
        expect(screen.getAllByRole('button', { name: t.chatRefineImage })).toHaveLength(2),
      );

      fireEvent.click(screen.getAllByRole('button', { name: t.chatRefineImage })[1]);

      // The sent turn's thumbnail (the older result, as u2's source) wears the same
      // alt text as the composer's, so tell the composer's by what only it can hold:
      // the newer result's reference.
      await waitFor(() =>
        expect(
          screen.getAllByAltText(t.chatAttachedImage).map((img) => img.getAttribute('src')),
        ).toContain(newerRef),
      );
    });
  });

  // The gateway refuses a mask that is not EXACTLY the size of the edit's source
  // (images.mask_invalid), so the portal sizes the mask to the source it actually
  // sends. The two sources differ: an uploaded image is downscaled to a 1568 px
  // longest edge on the way in, but a refined one is the stored image BY REFERENCE, at
  // its natural size -- 1792x1024 when the composer's own size menu made it -- so a
  // mask clamped to the same 1568 px would never fit it, with no way out for the user.
  describe(`Chat: an edit's mask is sized to its source, not clamped to 1568 px [${locale}]`, () => {
    const editModel: ModelOption = {
      id: 'sd-edit',
      display_name: 'sd-edit',
      flavors: ['openai'],
      loading_on_count: 0,
      image: true,
      image_edit: true,
    };
    const resultRef = '/api/portal/chat-blobs/blob_result';
    let drawImage: ReturnType<typeof vi.fn>;
    let toDataURL: ReturnType<typeof vi.spyOn>;
    let getContext: ReturnType<typeof vi.spyOn>;

    // jsdom has no canvas raster: a stubbed one that records the draw and encodes the
    // size it was given (see fakeImageBytes), so the url that comes out reads back to
    // the pixel size of the canvas it was drawn on.
    beforeEach(() => {
      drawImage = vi.fn();
      getContext = vi
        .spyOn(HTMLCanvasElement.prototype, 'getContext')
        .mockReturnValue({ drawImage } as unknown as CanvasRenderingContext2D);
      toDataURL = vi.spyOn(HTMLCanvasElement.prototype, 'toDataURL').mockImplementation(function (
        this: HTMLCanvasElement,
      ) {
        return `data:image/png;base64,${btoa(fakeImageBytes(this.width, this.height))}`;
      } as unknown as HTMLCanvasElement['toDataURL']);
    });
    afterEach(() => {
      getContext.mockRestore();
      toDataURL.mockRestore();
    });

    function seedGeneration() {
      chatApi.rows.push({
        id: 'c_refine',
        title: 'Lighthouse',
        created_at: '2026-07-19T12:00:00Z',
        updated_at: '2026-07-19T12:00:00Z',
        content: {
          settings: { model: editModel.id, kind: 'image' },
          messages: [
            { id: 'u1', role: 'user', content: 'a lighthouse' },
            {
              id: 'a1',
              role: 'assistant',
              content: [{ type: 'image_url', image_url: { url: resultRef } }],
              status: 'complete',
            },
          ],
        },
      });
    }
    // The generation was made at 1792x1024 (an offered size) and is refined as stored.
    async function refineLargeImage() {
      imageSizes.set(resultRef, [1792, 1024]);
      seedGeneration();
      renderChat(tokens, [editModel]);
      await screen.findByText('Lighthouse');
      fireEvent.click(await screen.findByRole('button', { name: t.chatRefineImage }));
      await screen.findByAltText(t.chatAttachedImage);
    }
    async function uploadSource(width: number, height: number) {
      renderChat(tokens, [editModel]);
      await waitForChatReady();
      await pickOption(t.chatModel, editModel.display_name);
      fireEvent.change(screen.getByLabelText(t.chatAttachImage), {
        target: { files: [fakeImageFile(width, height, 'source.png')] },
      });
      await screen.findByAltText(t.chatAttachedImage);
    }
    async function attachMask(width: number, height: number) {
      fireEvent.change(screen.getByLabelText(t.chatAttachMask), {
        target: { files: [fakeImageFile(width, height, 'mask.png')] },
      });
      await screen.findByAltText(t.chatAttachedMask);
    }
    async function sendEdit(prompt = 'fill the sky') {
      fireEvent.change(screen.getByLabelText(t.chatImagePromptLabel), {
        target: { value: prompt },
      });
      fireEvent.click(screen.getByRole('button', { name: t.send }));
      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1));
      return (chatApi.spies.startChatRun.mock.calls[0][1] as StartChatRunBody).user_message as {
        image_url?: { url: string };
        purpose?: string;
      }[];
    }
    const sizeOf = (url: string | undefined) => decodedSize(url ?? '');

    describe('a refined image', () => {
      it('gets a mask drawn at its own 1792x1024, not the 1568x896 the source upload would be clamped to', async () => {
        await refineLargeImage();

        await attachMask(3584, 2048); // the same picture at twice the size
        const parts = await sendEdit();

        expect(parts).toHaveLength(3);
        expect(parts[1].image_url?.url).toBe(resultRef);
        expect(parts[2].purpose).toBe('mask');
        expect(sizeOf(parts[2].image_url?.url)).toEqual([1792, 1024]);
        expect(drawImage).toHaveBeenCalledWith(expect.anything(), 0, 0, 1792, 1024);
        expect(toDataURL).toHaveBeenCalledWith('image/png');
      });

      it('keeps a mask that is already its size exactly as it was: no clamp, no re-encode', async () => {
        await refineLargeImage();

        await attachMask(1792, 1024);
        const parts = await sendEdit();

        expect(sizeOf(parts[2].image_url?.url)).toEqual([1792, 1024]);
        expect(parts[2].image_url?.url).toBe(
          `data:image/png;base64,${btoa(fakeImageBytes(1792, 1024))}`,
        );
        expect(drawImage).not.toHaveBeenCalled();
      });

      it('is sent without a mask exactly as before: the stored reference alone, nothing drawn', async () => {
        await refineLargeImage();

        const parts = await sendEdit('make the sky purple');

        expect(parts).toStrictEqual([
          { type: 'text', text: 'make the sky purple' },
          { type: 'image_url', image_url: { url: resultRef } },
        ]);
        expect(drawImage).not.toHaveBeenCalled();
        expect(toDataURL).not.toHaveBeenCalled();
      });
    });

    describe('an uploaded image', () => {
      it('keeps its mask the size of the downscaled source it is sent with', async () => {
        await uploadSource(4000, 3000); // downscaled to the 1568 px cap: 1568x1176

        await attachMask(4000, 3000);
        const parts = await sendEdit();

        expect(sizeOf(parts[1].image_url?.url)).toEqual([1568, 1176]);
        expect(sizeOf(parts[2].image_url?.url)).toEqual([1568, 1176]);
        expect(parts[2].purpose).toBe('mask');
      });

      it('is sent without a mask exactly as before: the downscaled source alone', async () => {
        await uploadSource(4000, 3000);

        const parts = await sendEdit('make it blue');

        expect(parts).toHaveLength(2);
        expect(parts[1]).toStrictEqual({
          type: 'image_url',
          image_url: { url: `data:image/png;base64,${btoa(fakeImageBytes(1568, 1176))}` },
        });
      });
    });
  });

  describe(`Chat: the advanced image controls follow the picked model's options [${locale}]`, () => {
    const imageModels: ModelOption[] = [
      {
        id: 'sd-turbo',
        display_name: 'sd-turbo',
        flavors: ['openai'],
        loading_on_count: 0,
        image: true,
      },
    ];

    // Lets the store's fetch of the picked model's options land: the effect
    // fires on the model pick, the (already resolved) mock settles in a
    // microtask, and only then do the dropdowns re-derive their options.
    async function settleModelOptions() {
      await waitFor(() => expect(chatApi.spies.modelImageOptions).toHaveBeenCalledWith('sd-turbo'));
      await act(async () => {});
    }

    // Opens the settings + advanced panels of a NEW image thread whose picked
    // model reports `options`.
    async function openAdvancedFor(options: ImageModelOptions) {
      chatApi.spies.modelImageOptions.mockResolvedValue(options);
      renderChat(tokens, imageModels);
      await waitForChatReady();
      await pickOption(t.chatModel, 'sd-turbo');
      await settleModelOptions();
      fireEvent.click(screen.getByRole('button', { name: t.chatSettings }));
      fireEvent.click(screen.getByRole('button', { name: t.chatImageAdvanced }));
    }

    // The same, for a SAVED image thread whose persisted knobs are `image`.
    async function openAdvancedForSaved(
      options: ImageModelOptions,
      image: Record<string, unknown>,
    ) {
      chatApi.rows.push({
        id: 'c_image_opts',
        title: '',
        created_at: '2026-07-21T12:00:00Z',
        updated_at: '2026-07-21T12:00:00Z',
        content: { settings: { model: 'sd-turbo', kind: 'image', image }, messages: [] },
      });
      chatApi.spies.modelImageOptions.mockResolvedValue(options);
      renderChat(tokens, imageModels);
      await waitForChatReady();
      await settleModelOptions();
      fireEvent.click(screen.getByRole('button', { name: t.chatSettings }));
      fireEvent.click(screen.getByRole('button', { name: t.chatImageAdvanced }));
    }

    // The option labels a combobox offers, in order (opened and closed again so
    // the next one can be read).
    async function offeredOptions(comboLabel: string): Promise<string[]> {
      const combo = screen.getByRole('combobox', { name: comboLabel });
      fireEvent.mouseDown(combo);
      const listbox = await screen.findByRole('listbox');
      const names = within(listbox)
        .getAllByRole('option')
        .map((option) => option.textContent ?? '');
      fireEvent.keyDown(combo, { key: 'Escape' });
      await waitFor(() => expect(screen.queryByRole('listbox')).not.toBeInTheDocument());
      return names;
    }

    async function sendPrompt() {
      fireEvent.change(screen.getByLabelText(t.chatImagePromptLabel), {
        target: { value: 'a cat' },
      });
      fireEvent.click(screen.getByRole('button', { name: t.send }));
      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalled());
      return chatApi.spies.startChatRun.mock.calls[0][1] as StartChatRunBody;
    }

    const formatLabel = (format: string) =>
      format === '' ? t.chatImageFormatDefault : format.toUpperCase();
    const defaultLabel = t.chatImageAdvancedDefault;

    it("offers only the model's sampler (plus the default entry) when it reports one", async () => {
      await openAdvancedFor({ samplers: ['euler_a'] });

      expect(await offeredOptions(t.chatImageSampler)).toEqual([defaultLabel, 'euler_a']);
    });

    it("offers only the model's schedulers and output formats too", async () => {
      await openAdvancedFor({ schedulers: ['karras', 'simple'], output_formats: ['png', 'webp'] });

      expect(await offeredOptions(t.chatImageScheduler)).toEqual([
        defaultLabel,
        'karras',
        'simple',
      ]);
      expect(await offeredOptions(t.chatImageOutputFormat)).toEqual([
        t.chatImageFormatDefault,
        'PNG',
        'WEBP',
      ]);
    });

    it.each([
      ['reports no lists at all', {}],
      ['reports empty lists', { samplers: [], schedulers: [], output_formats: [] }],
    ] as [string, ImageModelOptions][])(
      'offers the full static superset when the model %s',
      async (_name, options) => {
        await openAdvancedFor(options);

        expect(await offeredOptions(t.chatImageSampler)).toEqual(
          IMAGE_SAMPLERS.map((value) => (value === '' ? defaultLabel : value)),
        );
        expect(await offeredOptions(t.chatImageScheduler)).toEqual(
          IMAGE_SCHEDULERS.map((value) => (value === '' ? defaultLabel : value)),
        );
        expect(await offeredOptions(t.chatImageOutputFormat)).toEqual(
          IMAGE_OUTPUT_FORMATS.map(formatLabel),
        );
        expect(await offeredOptions(t.chatImageSize)).toEqual(
          IMAGE_SIZE_OPTIONS.map((value) => (value === '' ? t.chatImageSizeDefault : value)),
        );
      },
    );

    it('keeps a saved sampler the narrowed list omits selected, as a trailing option', async () => {
      await openAdvancedForSaved({ samplers: ['euler_a', 'lcm'] }, { sampler: 'dpm++2m' });

      const combo = screen.getByRole('combobox', { name: t.chatImageSampler }) as HTMLInputElement;
      // Not blank and not snapped to the default entry: the saved pick survives.
      expect(combo.value).toBe('dpm++2m');
      expect(await offeredOptions(t.chatImageSampler)).toEqual([
        defaultLabel,
        'euler_a',
        'lcm',
        'dpm++2m',
      ]);
    });

    it('keeps a saved scheduler, output format and size the narrowed lists omit selected', async () => {
      await openAdvancedForSaved(
        {
          schedulers: ['karras'],
          output_formats: ['png'],
          size_limits: { min_width: 512, max_width: 512, min_height: 512, max_height: 512 },
        },
        { scheduler: 'beta', output_format: 'jpeg', size: '768x768' },
      );

      const valueOf = (label: string) =>
        (screen.getByRole('combobox', { name: label }) as HTMLInputElement).value;
      expect(valueOf(t.chatImageScheduler)).toBe('beta');
      expect(valueOf(t.chatImageOutputFormat)).toBe('JPEG');
      expect(valueOf(t.chatImageSize)).toBe('768x768');
      expect((await offeredOptions(t.chatImageScheduler)).slice(-1)).toEqual(['beta']);
      expect((await offeredOptions(t.chatImageOutputFormat)).slice(-1)).toEqual(['JPEG']);
      expect((await offeredOptions(t.chatImageSize)).slice(-1)).toEqual(['768x768']);
    });

    it("shows the model's reported defaults as hints and sends none of them untouched", async () => {
      await openAdvancedFor({
        samplers: ['euler_a', 'lcm'],
        schedulers: ['karras', 'simple'],
        output_formats: ['png', 'webp'],
        defaults: {
          sampler: 'euler_a',
          scheduler: 'karras',
          output_format: 'webp',
          negative_prompt: 'blurry, low quality',
          steps: 20,
          cfg: 7.5,
          seed: 42,
          clip_skip: 2,
          width: 1024,
          height: 768,
        },
      });

      const describedBy = (label: string) =>
        screen.getByRole('combobox', { name: label }) as HTMLInputElement;
      expect(describedBy(t.chatImageSampler)).toHaveAccessibleDescription(
        `${defaultLabel}: euler_a`,
      );
      expect(describedBy(t.chatImageScheduler)).toHaveAccessibleDescription(
        `${defaultLabel}: karras`,
      );
      expect(describedBy(t.chatImageOutputFormat)).toHaveAccessibleDescription(
        `${defaultLabel}: WEBP`,
      );
      expect(describedBy(t.chatImageSize)).toHaveAccessibleDescription(`${defaultLabel}: 1024x768`);
      const placeholderOf = (label: string) => screen.getByLabelText(label);
      expect(placeholderOf(t.chatImageSteps)).toHaveAttribute('placeholder', `${defaultLabel}: 20`);
      expect(placeholderOf(t.chatImageCFG)).toHaveAttribute('placeholder', `${defaultLabel}: 7.5`);
      expect(placeholderOf(t.chatImageSeed)).toHaveAttribute('placeholder', `${defaultLabel}: 42`);
      expect(placeholderOf(t.chatImageClipSkip)).toHaveAttribute(
        'placeholder',
        `${defaultLabel}: 2`,
      );
      expect(placeholderOf(t.chatImageNegativePrompt)).toHaveAttribute(
        'placeholder',
        `${defaultLabel}: blurry, low quality`,
      );

      // A hint only: every field still reads as unset ...
      expect(describedBy(t.chatImageSampler).value).toBe(defaultLabel);
      expect(describedBy(t.chatImageScheduler).value).toBe(defaultLabel);
      expect(describedBy(t.chatImageOutputFormat).value).toBe(t.chatImageFormatDefault);
      expect(describedBy(t.chatImageSize).value).toBe(t.chatImageSizeDefault);
      for (const label of [
        t.chatImageSteps,
        t.chatImageCFG,
        t.chatImageSeed,
        t.chatImageClipSkip,
        t.chatImageNegativePrompt,
      ]) {
        expect(placeholderOf(label)).toHaveValue(label === t.chatImageNegativePrompt ? '' : null);
      }

      // ... and so the run body carries none of them (blank = sd-server's own default).
      const body = await sendPrompt();
      expect(Object.keys(body.settings.image ?? {})).toEqual(['n']);
    });

    it('shows a reported default of -1 or 0 as a hint rather than treating it as unset', async () => {
      await openAdvancedFor({ defaults: { clip_skip: -1, seed: -1, steps: 0 } });

      const hint = (label: string) => screen.getByLabelText(label).getAttribute('placeholder');
      expect(hint(t.chatImageClipSkip)).toBe(`${defaultLabel}: -1`);
      expect(hint(t.chatImageSeed)).toBe(`${defaultLabel}: -1`);
      expect(hint(t.chatImageSteps)).toBe(`${defaultLabel}: 0`);
      // An unreported default keeps the plain placeholder.
      expect(hint(t.chatImageCFG)).toBe(defaultLabel);
      // Still only a hint: nothing is sent.
      const body = await sendPrompt();
      expect(Object.keys(body.settings.image ?? {})).toEqual(['n']);
    });

    it('states the size default when BOTH the width and the height are reported', async () => {
      await openAdvancedFor({ defaults: { width: 1024, height: 768 } });

      expect(screen.getByRole('combobox', { name: t.chatImageSize })).toHaveAccessibleDescription(
        `${defaultLabel}: 1024x768`,
      );
    });

    it('states no size default unless BOTH the width and the height are reported', async () => {
      await openAdvancedFor({ defaults: { width: 1024 } });

      expect(
        screen.getByRole('combobox', { name: t.chatImageSize }),
      ).not.toHaveAccessibleDescription(/1024/);
    });

    it("caps the image count at the model's max batch count", async () => {
      await openAdvancedFor({ max_batch_count: 4 });

      const count = screen.getByLabelText(t.chatImageCount);
      expect(count).toHaveAccessibleDescription('1–4');
      fireEvent.change(count, { target: { value: '9' } });
      expect(count).toHaveValue(4);
      const body = await sendPrompt();
      expect(body.settings.image?.n).toBe(4);
    });

    it.each([
      ['no max batch count', {}],
      ['a half-stated size envelope and no max batch count', { size_limits: { max_width: 1024 } }],
      ['a non-positive max batch count', { max_batch_count: 0 }],
    ] as [string, ImageModelOptions][])(
      'falls back to the static count ceiling with %s',
      async (_name, options) => {
        await openAdvancedFor(options);

        const count = screen.getByLabelText(t.chatImageCount);
        expect(count).toHaveAccessibleDescription('1–8');
        fireEvent.change(count, { target: { value: '20' } });
        expect(count).toHaveValue(8);
      },
    );

    it("flags a saved count above the picked model's max batch count, and only then", async () => {
      await openAdvancedForSaved({ max_batch_count: 4 }, { n: 8 });

      const count = screen.getByLabelText(t.chatImageCount);
      expect(count).toHaveAttribute('aria-invalid', 'true');
      // Back inside the ceiling the flag clears, so it is not simply always on.
      fireEvent.change(count, { target: { value: '3' } });
      expect(count).toHaveAttribute('aria-invalid', 'false');
    });

    it("never raises the count ceiling above the gateway's hard cap of 8", async () => {
      // The model may only NARROW the cap: a reported 16 is still 8 (the portal's
      // own per-request limit), in the helper, the clamp and the run body.
      await openAdvancedFor({ max_batch_count: 16 });

      const count = screen.getByLabelText(t.chatImageCount);
      expect(count).toHaveAccessibleDescription('1–8');
      fireEvent.change(count, { target: { value: '20' } });
      expect(count).toHaveValue(8);
      const body = await sendPrompt();
      expect(body.settings.image?.n).toBe(8);
    });

    it('keeps the count ceiling at 8 for a model whose max batch count is exactly 8', async () => {
      await openAdvancedFor({ max_batch_count: 8 });

      const count = screen.getByLabelText(t.chatImageCount);
      expect(count).toHaveAccessibleDescription('1–8');
      fireEvent.change(count, { target: { value: '8' } });
      expect(count).toHaveValue(8);
      fireEvent.change(count, { target: { value: '9' } });
      expect(count).toHaveValue(8);
    });

    it('flags a saved count above the hard cap even when the model reports a larger max', async () => {
      await openAdvancedForSaved({ max_batch_count: 16 }, { n: 12 });

      expect(screen.getByLabelText(t.chatImageCount)).toHaveAttribute('aria-invalid', 'true');
    });

    it('offers only the size presets inside the size envelope', async () => {
      await openAdvancedFor({
        size_limits: { min_width: 600, max_width: 1024, min_height: 600, max_height: 1024 },
      });

      expect(await offeredOptions(t.chatImageSize)).toEqual([
        t.chatImageSizeDefault,
        '768x768',
        '1024x1024',
      ]);
    });

    it('applies only the stated side of a half-stated size envelope', async () => {
      // Maxima without minima: the missing side is unbounded, not NaN (which
      // would make every comparison false and empty the list).
      await openAdvancedFor({ size_limits: { max_width: 1024, max_height: 1024 } });

      expect(await offeredOptions(t.chatImageSize)).toEqual([
        t.chatImageSizeDefault,
        '512x512',
        '768x768',
        '1024x1024',
      ]);
    });

    it('applies a lone minimum height and leaves every other bound open', async () => {
      await openAdvancedFor({ size_limits: { min_height: 1024 } });

      expect(await offeredOptions(t.chatImageSize)).toEqual([
        t.chatImageSizeDefault,
        '1024x1024',
        '1024x1792',
        '1792x1024',
      ]);
    });
  });

  describe(`Chat: the pending row follows the RUN's kind, not the thread's [${locale}]`, () => {
    // Whole-branch review, finding 4. Chat.tsx is the ONLY wiring site that
    // decides which kind ChatMessage renders the in-flight turn from, and
    // nothing pinned it: replacing `c.runKind` with `c.chatKind` there --
    // precisely the defect an earlier fix round closed -- left the whole
    // suite green. runKind is well covered inside the store; the prop that
    // carries it into the view was not covered at all.
    const imageModels: ModelOption[] = [
      {
        id: 'sd-turbo',
        display_name: 'sd-turbo',
        flavors: ['openai'],
        loading_on_count: 0,
        image: true,
      },
    ];

    async function sendImagePrompt() {
      await pickOption(t.chatModel, imageModels[0].display_name);
      fireEvent.change(screen.getByLabelText(t.chatImagePromptLabel), {
        target: { value: 'a cat on a bicycle' },
      });
      fireEvent.click(screen.getByRole('button', { name: t.send }));
      await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
      // A running snapshot is what materialises the in-flight assistant row
      // (ensureAssistant); until one arrives the transcript's last message is
      // the user turn and no row is `rowStreaming` at all. It carries no
      // `kind` on purpose, so the kind under test stays the one the 201
      // reported -- which is the value Chat.tsx is wired to.
      await act(async () => {
        FakeEventSource.instances[0].emit('snapshot', { status: 'running', elapsed_ms: 1000 });
      });
    }

    it('renders the image wait state while the run itself reports the image kind', async () => {
      // The positive control. Without it the negative test below could pass
      // for the trivial reason that this tree never renders the wait state.
      chatApi.spies.startChatRun.mockImplementation(async (chatId: string) => ({
        run_id: 'run_1',
        chat_id: chatId,
        status: 'running' as const,
        kind: 'image',
      }));
      renderChat(tokens, imageModels);
      await waitForChatReady();
      await sendImagePrompt();

      expect(await screen.findByText(t.chatImageRunPending)).toBeInTheDocument();
      expect(screen.getByText(t.chatImageNoIntermediateNews)).toBeInTheDocument();
    });

    it('renders no image wait state while the run itself reports a text kind', async () => {
      // The stale-pin case. This client pins `image` locally on its first
      // send (the picked model generates images), but the SERVER has already
      // pinned this thread to text -- another tab sent a text turn first --
      // and forces that kind onto every later run, so the 201 reports "".
      // With c.chatKind here the composer's guess wins and the user watches
      // an image wait state, with no character counter and no streamed text,
      // over an entire text generation.
      chatApi.spies.startChatRun.mockImplementation(async (chatId: string) => ({
        run_id: 'run_1',
        chat_id: chatId,
        status: 'running' as const,
        kind: '',
      }));
      renderChat(tokens, imageModels);
      await waitForChatReady();
      await sendImagePrompt();

      // The thread's own kind really is `image` -- the composer is showing
      // its image affordances -- so the assertion below is about the RUN, not
      // about the thread having failed to pin.
      expect(screen.getByLabelText(t.chatImagePromptLabel)).toBeInTheDocument();
      // ...and the row really is streaming, so "nothing rendered" is not the
      // reason the wait state is absent.
      expect(await screen.findByRole('button', { name: t.chatStop })).toBeInTheDocument();
      expect(screen.queryByText(t.chatImageRunPending)).not.toBeInTheDocument();
      expect(screen.queryByText(t.chatImageNoIntermediateNews)).not.toBeInTheDocument();
    });
  });

  describe(`Chat: server override picker (Task 6) [${locale}]`, () => {
    it('hides the whole control when the caller manages zero servers', async () => {
      renderChat(tokens, models, []);
      await waitForChatReady();
      openSettings();

      expect(
        screen.queryByRole('combobox', { name: t.serverOverrideLabel }),
      ).not.toBeInTheDocument();
    });

    it('shows the picker (and no force checkbox yet) when the caller manages at least one server', async () => {
      renderChat(tokens, models, [makeServer({ id: 'srv_a', name: 'Server A' })]);
      await waitForChatReady();
      openSettings();

      expect(screen.getByRole('combobox', { name: t.serverOverrideLabel })).toBeInTheDocument();
      expect(screen.queryByLabelText(t.serverOverrideForceLabel)).not.toBeInTheDocument();
    });

    // Both fixture models are part of the general (reachable) catalog, mirroring
    // production where api.models() already aggregates every server's offerings
    // -- so picking either keeps modelAvailable true; only the DROPDOWN's option
    // set narrows once a server override is picked.
    const twoModelCatalog: ModelOption[] = [
      { id: 'gpt-oss-20b', display_name: 'gpt-oss-20b', flavors: ['openai'], loading_on_count: 0 },
      {
        id: 'server-only-model',
        display_name: 'server-only-model',
        flavors: ['openai'],
        loading_on_count: 0,
      },
    ];

    it("filters the model dropdown to the picked server's offered models and starts a run with both fields set", async () => {
      serverModelsByServer.srv_a = [{ id: 'server-only-model', display_name: 'server-only-model' }];
      renderChat(tokens, twoModelCatalog, [makeServer({ id: 'srv_a', name: 'Server A' })]);
      await waitForChatReady();
      openSettings();

      await pickOption(t.serverOverrideLabel, 'Server A');
      await waitFor(() => expect(chatApi.spies.serverModels).toHaveBeenCalledWith('srv_a'));

      // The force checkbox now renders (an override is set).
      expect(screen.getByLabelText(t.serverOverrideForceLabel)).toBeInTheDocument();

      // The main model dropdown now offers ONLY the override server's model, not
      // the full two-model catalog.
      fireEvent.mouseDown(screen.getByRole('combobox', { name: t.chatModel }));
      expect(await screen.findByRole('option', { name: 'server-only-model' })).toBeInTheDocument();
      expect(screen.queryByRole('option', { name: 'gpt-oss-20b' })).not.toBeInTheDocument();
      fireEvent.click(screen.getByRole('option', { name: 'server-only-model' }));

      fireEvent.click(screen.getByLabelText(t.serverOverrideForceLabel));
      fireEvent.change(screen.getByLabelText(t.messageLabel), { target: { value: 'hi' } });
      fireEvent.click(screen.getByRole('button', { name: t.send }));

      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalled());
      const body = chatApi.spies.startChatRun.mock.calls[0][1] as StartChatRunBody;
      expect(body.settings.server_override).toBe('srv_a');
      expect(body.settings.server_override_force_unreachable).toBe(true);
    });

    it('resets a stale override when the picked server drops out of the manageable set', async () => {
      serverModelsByServer.srv_a = [{ id: 'server-only-model', display_name: 'server-only-model' }];
      const { rerenderWith } = renderChat(tokens, twoModelCatalog, [
        makeServer({ id: 'srv_a', name: 'Server A' }),
      ]);
      await waitForChatReady();
      openSettings();

      const serverSelect = screen.getByLabelText(t.serverOverrideLabel) as HTMLInputElement;
      await pickOption(t.serverOverrideLabel, 'Server A');
      expect(serverSelect.value).toBe('Server A');
      expect(screen.getByLabelText(t.serverOverrideForceLabel)).toBeInTheDocument();

      // Simulate `servers` reloading (App's onRefresh path) without the
      // previously-picked server (deleted / management revoked).
      rerenderWith(tokens, []);

      // Selection resets to the empty value; the whole control (including the
      // force checkbox) disappears since the caller now manages zero servers.
      await waitFor(() =>
        expect(
          screen.queryByRole('combobox', { name: t.serverOverrideLabel }),
        ).not.toBeInTheDocument(),
      );
      expect(screen.queryByLabelText(t.serverOverrideForceLabel)).not.toBeInTheDocument();

      // The model dropdown is back to the full catalog; pick one and send -- the
      // run must carry no server override.
      await pickOption(t.chatModel, 'gpt-oss-20b');
      fireEvent.change(screen.getByLabelText(t.messageLabel), { target: { value: 'hi' } });
      fireEvent.click(screen.getByRole('button', { name: t.send }));

      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalled());
      const body = chatApi.spies.startChatRun.mock.calls[0][1] as StartChatRunBody;
      expect(body.settings.server_override ?? '').toBe('');
      expect(body.settings.server_override_force_unreachable ?? false).toBe(false);
    });
  });

  describe(`Chat: run-as token locks the server-override control (T2) [${locale}]`, () => {
    it("keeps the chat's own server-override control editable for a run-as token without a server override", async () => {
      renderChat(tokens, models, [makeServer({ id: 'srv_a', name: 'Server A' })]);
      await waitForChatReady();
      openSettings();

      await pickOption(t.chatRunAsTokenLabel, plainToken.name);

      const serverSelect = screen.getByRole('combobox', {
        name: t.serverOverrideLabel,
      }) as HTMLInputElement;
      expect(serverSelect).not.toBeDisabled();
    });

    it("locks the server-override control to the run-as token's server + force, and filters the model dropdown to it", async () => {
      const lockedToken = makeToken({
        id: 'tok_server_override',
        name: 'Locked Token',
        server_override: 'srv_token',
        server_override_force_unreachable: true,
      });
      serverModelsByServer.srv_token = [
        { id: 'token-server-model', display_name: 'token-server-model' },
      ];
      renderChat([...tokens, lockedToken], models, [makeServer({ id: 'srv_a', name: 'Server A' })]);
      await waitForChatReady();
      openSettings();

      await pickOption(t.chatRunAsTokenLabel, lockedToken.name);

      // The model dropdown's option fetch is keyed on the EFFECTIVE (token-locked)
      // server, not the chat's own (empty) picked server.
      await waitFor(() => expect(chatApi.spies.serverModels).toHaveBeenCalledWith('srv_token'));

      const serverSelect = screen.getByRole('combobox', {
        name: t.serverOverrideLabel,
      }) as HTMLInputElement;
      expect(serverSelect).toBeDisabled();
      // srv_token is not among the caller's manageable servers -- the synthetic
      // fallback option keeps the id visible instead of rendering blank.
      expect(serverSelect.value).toBe('srv_token');
      expect(screen.getByText(t.serverOverrideLockedHint)).toBeInTheDocument();

      const forceCheckbox = screen.getByLabelText(t.serverOverrideForceLabel) as HTMLInputElement;
      expect(forceCheckbox).toBeDisabled();
      expect(forceCheckbox.checked).toBe(true);

      fireEvent.mouseDown(screen.getByRole('combobox', { name: t.chatModel }));
      expect(await screen.findByRole('option', { name: 'token-server-model' })).toBeInTheDocument();
      expect(
        screen.queryByRole('option', { name: models[0].display_name }),
      ).not.toBeInTheDocument();
    });

    it("unlocks the chat's own server-override control again when the run-as token is cleared", async () => {
      const lockedToken = makeToken({
        id: 'tok_server_override',
        name: 'Locked Token',
        server_override: 'srv_token',
        server_override_force_unreachable: true,
      });
      serverModelsByServer.srv_token = [
        { id: 'token-server-model', display_name: 'token-server-model' },
      ];
      renderChat([...tokens, lockedToken], models, [makeServer({ id: 'srv_a', name: 'Server A' })]);
      await waitForChatReady();
      openSettings();

      await pickOption(t.chatRunAsTokenLabel, lockedToken.name);
      const serverSelect = screen.getByRole('combobox', {
        name: t.serverOverrideLabel,
      }) as HTMLInputElement;
      expect(serverSelect).toBeDisabled();

      await pickOption(t.chatRunAsTokenLabel, t.chatRunAsNone);

      await waitFor(() => expect(serverSelect).not.toBeDisabled());
    });
  });
}
