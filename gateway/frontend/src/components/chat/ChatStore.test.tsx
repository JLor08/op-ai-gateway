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
import { messages } from '../../i18n';
import type { ActiveChatRun, ModelOption, PortalToken, ServerModelOption } from '../../api';

const t = messages.de;
const models: ModelOption[] = [
  { id: 'gpt-oss-20b', display_name: 'gpt-oss-20b', flavors: ['openai'] },
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
function makeChatApi(seed: ChatRow[] = []) {
  let seq = 0;
  const rows: ChatRow[] = [...seed];
  const stamp = () => new Date(Date.UTC(2026, 6, 17, 12, seq)).toISOString();
  const spies = {
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
      <span data-testid="last-status">{last?.status ?? ''}</span>
      <span data-testid="loading">{String(c.chatsLoading)}</span>
      <span data-testid="active">{c.activeChatId ?? ''}</span>
      <span data-testid="chats">{c.chats.map((ch) => `${ch.id}:${ch.title}`).join('|')}</span>
      <span data-testid="model">{c.model}</span>
      <span data-testid="model-available">{String(c.modelAvailable)}</span>
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
  return { onRefresh, rerenderWithModels };
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

describe('useChatStore / useChatStreaming guards', () => {
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

describe('ChatStoreProvider bootstrap', () => {
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

describe('ChatStoreProvider chat actions', () => {
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

    await waitFor(() => expect(chatApi.spies.saveChat.mock.calls.length).toBeGreaterThan(before), {
      timeout: 2500,
    });
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

describe('ChatStoreProvider streaming', () => {
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

describe('ChatStoreProvider reopen + interrupted (Task 3.4)', () => {
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
    await waitFor(() => expect(screen.getByTestId('last-status').textContent).toBe('interrupted'));
  });
});

describe('ChatStoreProvider refetch-on-done (review follow-up #A)', () => {
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
        { id: 'msg_srv_asst', role: 'assistant', content: 'canonical answer', status: 'complete' },
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

describe('ChatStoreProvider stop/cancel + delete (Task 3.5)', () => {
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

describe('ChatStoreProvider busy guard (review follow-up #B)', () => {
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

    expect(await screen.findByText(messages.de.chatBusy)).toBeTruthy();
    // The guard held: only the first run was started.
    expect(chatApi.spies.startChatRun).toHaveBeenCalledTimes(1);
  });
});

describe('ChatStoreProvider remembers an unavailable model', () => {
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

    expect(await screen.findByText(messages.de.chatModelUnavailable)).toBeTruthy();
    expect(chatApi.spies.startChatRun).not.toHaveBeenCalled();

    // Switching to an available model unblocks sending.
    fireEvent.click(screen.getByRole('button', { name: 'set-model-alt' }));
    fireEvent.click(screen.getByRole('button', { name: 'send' }));

    await waitFor(() => expect(chatApi.spies.startChatRun).toHaveBeenCalled());
  });

  it("resolves a run-as token's per-model override map by the requested model (no catch-all)", async () => {
    const twoModels: ModelOption[] = [
      { id: 'model-a', display_name: 'A', flavors: ['openai'] },
      { id: 'model-b', display_name: 'B', flavors: ['openai'] },
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
      model_override_map: { 'model-b': 'model-a' }, // requesting model-b -> model-a
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

    expect(await screen.findByText(messages.de.chatModelUnavailable)).toBeTruthy();
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

    expect(await screen.findByText(messages.de.chatModelUnavailable)).toBeTruthy();
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

    expect(await screen.findByText(messages.de.chatModelUnavailable)).toBeTruthy();
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

describe('ChatStoreProvider startRunWithHistory vision guard uses the REPLAYED history, not the full transcript', () => {
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
    expect(screen.queryByText(messages.de.chatImageModelUnsupported)).not.toBeInTheDocument();
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

    expect(await screen.findByText(messages.de.chatImageModelUnsupported)).toBeTruthy();
    expect(chatApi.spies.startChatRun).not.toHaveBeenCalled();
    expect(screen.getByTestId('count').textContent).toBe('4');
  });
});

describe('useChatStreaming isolation', () => {
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
