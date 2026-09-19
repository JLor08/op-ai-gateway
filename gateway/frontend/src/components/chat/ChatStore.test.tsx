// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { memo } from 'react';
import {
  ChatStoreProvider,
  ChatStoreContext,
  ChatStreamingContext,
  useChatStore,
  useChatStreaming,
  type ChatStore,
} from './ChatStore';
import { ToastProvider } from '../shared/ToastProvider';
import { messages, type Locale } from '../../i18n';
import type { ActiveChatRun, ModelOption, PortalToken, ServerModelOption } from '../../api';

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
// maxContentBytes is the cap the real listing serves (portal.MaxChatContentBytes
// on ChatListResponse); a test shrinks it to drive the composer's capacity
// refusal without building a multi-megabyte fixture.
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
    // The server-override picker's filtered-model fetch: no test in this file
    // exercises it, but the store now holds a real reference to this method so
    // the mock must provide one.
    serverModels: vi.fn(async () => [] as ServerModelOption[]),
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
              .map((p) =>
                p.type === 'image_url' ? `image_url:${p.image_url.url}` : `text:${p.text}`,
              )
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
      <span data-testid="run-kind">{c.runKind ?? ''}</span>
      <span data-testid="run-elapsed-ms">{c.runElapsedMs ?? ''}</span>
      <span data-testid="model-image-capable">{String(c.modelImageCapable)}</span>
      <span data-testid="image-capacity">
        {c.imageCapacityLeft === null ? 'unknown' : String(c.imageCapacityLeft)}
      </span>
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
      {/* Selects a caller-supplied model id (a test's "pick an available model"
          action) without needing a full SearchableSelect in this probe. */}
      <button type="button" onClick={() => c.setModel(altModelId)}>
        set-model-alt
      </button>
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
          <button type="button" onClick={() => c.handlersFor(m.id).onEdit('edited text')}>
            edit-{m.id}
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
          >
            <Probe altModelId={opts.altModelId ?? nextModels[0]?.id ?? ''} />
          </ChatStoreProvider>
        </ToastProvider>,
      );
    return { onRefresh, rerenderWithModels, unmount: view.unmount };
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
      await waitFor(() =>
        expect(chatApi.spies.saveChat).toHaveBeenCalledWith(
          'c_seed',
          expect.objectContaining({ title: 'Renamed' }),
        ),
      );
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

  describe(`ChatStoreProvider image-thread kind and capacity [${locale}]`, () => {
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
    // A generated image big enough that a shrunken cap provably cannot hold a
    // second one (the capacity estimate is the largest image the thread has
    // actually produced -- see shared/chatCapacity.ts).
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
      expect(screen.getByTestId('image-capacity').textContent).toBe('unknown');

      fireEvent.change(screen.getByLabelText('probe-input'), { target: { value: 'more' } });
      fireEvent.click(screen.getByRole('button', { name: 'send' }));

      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalled());
      expect(
        (chatApi.spies.startChatRun.mock.calls[0][1] as { settings: { kind?: string } }).settings
          .kind,
      ).toBe('');
    });

    it('refuses to send when the transcript has no room for another image', async () => {
      // A thread already within one image of the served cap: the remaining
      // capacity is exact and known NOW, so spending minutes of upstream CPU
      // on an artifact we can already prove we cannot store is pure waste.
      chatApi = makeChatApi(imageThread(bigImage), 300_000);
      renderProvider([], { models: imageModels });
      await waitForReady();
      expect(screen.getByTestId('chat-kind').textContent).toBe('image');
      await waitFor(() => expect(screen.getByTestId('image-capacity').textContent).toBe('0'));

      fireEvent.change(screen.getByLabelText('probe-input'), { target: { value: 'a dog' } });
      fireEvent.click(screen.getByRole('button', { name: 'send' }));

      expect(await screen.findByText(t.chatCapacityExhausted)).toBeTruthy();
      expect(chatApi.spies.startChatRun).not.toHaveBeenCalled();
      // The composer is left untouched -- nothing was sent, nothing dropped.
      expect(screen.getByTestId('count').textContent).toBe('2');
    });

    it('sends normally while the transcript still has room for another image', async () => {
      // Negative control for the refusal above: same thread, the real cap.
      chatApi = makeChatApi(imageThread(bigImage));
      renderProvider([], { models: imageModels });
      await waitForReady();
      await waitFor(() =>
        expect(screen.getByTestId('image-capacity').textContent).not.toBe('unknown'),
      );
      expect(Number(screen.getByTestId('image-capacity').textContent)).toBeGreaterThan(0);

      fireEvent.change(screen.getByLabelText('probe-input'), { target: { value: 'a dog' } });
      fireEvent.click(screen.getByRole('button', { name: 'send' }));

      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalled());
      expect(screen.queryByText(t.chatCapacityExhausted)).toBeNull();
    });

    it('reports an unknown capacity (and refuses nothing) when the server serves no cap', async () => {
      chatApi = makeChatApi(imageThread(bigImage), 0);
      renderProvider([], { models: imageModels });
      await waitForReady();

      expect(screen.getByTestId('image-capacity').textContent).toBe('unknown');
      fireEvent.change(screen.getByLabelText('probe-input'), { target: { value: 'a dog' } });
      fireEvent.click(screen.getByRole('button', { name: 'send' }));

      await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalled());
    });

    it('leaves a text thread without a capacity number at all', async () => {
      chatApi = makeChatApi([
        {
          id: 'c1',
          title: 'C1',
          created_at: T,
          updated_at: T,
          content: { settings: { model: models[0].id }, messages: [] },
        },
      ]);
      renderProvider();
      await waitForReady();

      expect(screen.getByTestId('chat-kind').textContent).toBe('');
      expect(screen.getByTestId('image-capacity').textContent).toBe('unknown');
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
