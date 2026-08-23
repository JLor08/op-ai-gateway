// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { Chat } from './Chat';
import { ChatStoreProvider } from './chat/ChatStore';
import { ToastProvider } from './shared/ToastProvider';
import { messages } from '../i18n';
import type {
  ActiveChatRun,
  ModelOption,
  PortalServer,
  PortalToken,
  ServerModelOption,
  StartChatRunBody,
} from '../api';

const t = messages.de;

// vision: true so the pre-existing image-retention/attach tests below (which
// predate the vision-capability gate) keep exercising attach + send unchanged;
// the "Chat: vision gating" describe block below covers the non-capable path.
const models: ModelOption[] = [
  { id: 'gpt-oss-20b', display_name: 'gpt-oss-20b', flavors: ['openai'], vision: true },
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
    chats: vi.fn(async () => ({ data: rows.map(({ content: _content, ...rest }) => rest) })),
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
    saveChat: vi.fn(async (id: string, body: { title: string; content: unknown }) => {
      seq += 1;
      const index = rows.findIndex((row) => row.id === id);
      if (index >= 0)
        rows[index] = {
          ...rows[index],
          title: body.title,
          content: body.content,
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

// Wait for the provider's initial list-load to finish and open the fresh chat:
// the sidebar renders its (untitled) row only after chatsLoading flips false and
// the chat is activated, so this doubles as a readiness gate before interacting.
async function waitForChatReady() {
  await screen.findByText(t.chatUntitled);
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
    set src(_value: string) {
      queueMicrotask(() => {
        if (imageDecodeFails) {
          this.onerror?.();
          return;
        }
        this.naturalWidth = 32;
        this.naturalHeight = 24;
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
  const rerenderWith = (nextTokens: PortalToken[], nextServers: PortalServer[] = serversOverride) =>
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

beforeEach(() => {
  imageDecodeFails = false;
  FakeEventSource.instances = [];
  serverModelsByServer = {};
  installStubs();
  chatApi = makeChatApi();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('Chat run-as token selector', () => {
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

describe('Chat image retention', () => {
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
    fireEvent.change(screen.getByLabelText(t.messageLabel), { target: { value: 'look at this' } });
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

describe('Chat: remembers an unavailable model', () => {
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

describe('Chat: loaded-state of the selected model', () => {
  it('shows a green loaded dot BEFORE the name once a loaded model is selected', async () => {
    const loaded: ModelOption[] = [
      {
        id: 'gpt-oss-20b',
        display_name: 'gpt-oss-20b',
        flavors: ['openai'],
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

describe('Chat: image attachment gated on model vision capability', () => {
  const nonVisionModels: ModelOption[] = [
    { id: 'text-only', display_name: 'text-only', flavors: ['openai'], vision: false },
  ];
  const mixedModels: ModelOption[] = [
    { id: 'vision-model', display_name: 'vision-model', flavors: ['openai'], vision: true },
    { id: 'text-model', display_name: 'text-model', flavors: ['openai'], vision: false },
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

    fireEvent.change(screen.getByLabelText(t.messageLabel), { target: { value: 'look at this' } });
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

    await waitFor(() => expect(screen.queryByAltText(t.chatAttachedImage)).not.toBeInTheDocument());
    const banner = await screen.findByRole('alert');
    expect(banner).toHaveTextContent(t.chatImageModelUnsupported);
  });
});

describe('Chat: server override picker (Task 6)', () => {
  it('hides the whole control when the caller manages zero servers', async () => {
    renderChat(tokens, models, []);
    await waitForChatReady();
    openSettings();

    expect(screen.queryByRole('combobox', { name: t.serverOverrideLabel })).not.toBeInTheDocument();
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
    { id: 'gpt-oss-20b', display_name: 'gpt-oss-20b', flavors: ['openai'] },
    { id: 'server-only-model', display_name: 'server-only-model', flavors: ['openai'] },
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

describe('Chat: run-as token locks the server-override control (T2)', () => {
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
    expect(screen.queryByRole('option', { name: models[0].display_name })).not.toBeInTheDocument();
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
