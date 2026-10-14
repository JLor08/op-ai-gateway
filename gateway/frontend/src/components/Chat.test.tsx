// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { heicTo } from 'heic-to';

import { Chat } from './Chat';
import {
  ChatStoreProvider,
  IMAGE_OUTPUT_FORMATS,
  IMAGE_SAMPLERS,
  IMAGE_SCHEDULERS,
  IMAGE_SIZE_OPTIONS,
} from './chat/ChatStore';
import { ToastProvider } from './shared/ToastProvider';
import { messages, type Locale } from '../i18n';
import type {
  ActiveChatRun,
  ImageModelOptions,
  ModelOption,
  PortalServer,
  PortalToken,
  ServerModelOption,
  StartChatRunBody,
} from '../api';

// heic-to loads a libheif WASM decoder at runtime, which jsdom cannot run: mock it
// (as imageAttach.test.ts does) to a JPEG blob, so a HEIC attachment can be followed
// through the real upload pipeline (prepareImageDataUrl) into the sent message.
vi.mock('heic-to', () => ({
  heicTo: vi.fn(async () => new Blob(['jpeg-bytes'], { type: 'image/jpeg' })),
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
    // The server-override picker's filtered-model fetch (Task 6, step 3).
    serverModels: vi.fn(async (serverId: string) => serverModelsByServer[serverId] ?? []),
    // The per-model sd-server image options the store fetches whenever the picked
    // model GENERATES images: this file's fixtures know none, so every field is
    // omitted (the dropdowns then use the static Phase 1b-i superset).
    modelImageOptions: vi.fn(async (_name: string) => ({}) as ImageModelOptions),
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
