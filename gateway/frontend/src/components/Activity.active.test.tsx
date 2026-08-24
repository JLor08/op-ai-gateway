// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { Activity } from './Activity';
import { ToastProvider } from './shared/ToastProvider';
import { messages } from '../i18n';
import type { ActiveRequest, UsagePage, UsageStats } from '../api';
import type { PortalApi } from './shared/types';

const t = messages.de;

// Fresh localStorage per test so the persisted scope (op.activity.scope) is
// deterministic. The running panel shows `user_name || user_id` directly (no
// owner-display preference), so the optional seed is a no-op kept for symmetry.
function installStorage(ownerDisplay?: 'name' | 'id') {
  const store = new Map<string, string>();
  if (ownerDisplay) store.set('op.pref.table.activity.ownerDisplay', JSON.stringify(ownerDisplay));
  const storage = {
    getItem: (k: string) => (store.has(k) ? store.get(k)! : null),
    setItem: (k: string, v: string) => void store.set(k, String(v)),
    removeItem: (k: string) => void store.delete(k),
    clear: () => store.clear(),
    key: (i: number) => Array.from(store.keys())[i] ?? null,
    get length() {
      return store.size;
    },
  } satisfies Storage;
  vi.stubGlobal('localStorage', storage);
}

const emptyHistogram = { bins: [], min: 0, max: 0, bin_size: 0, p50: 0, p95: 0, p99: 0 };

function makeStats(): UsageStats {
  return {
    totals: {
      total_requests: 0,
      error_count: 0,
      cached_tokens: 0,
      cache_write_tokens: 0,
      input_tokens: 0,
      output_tokens: 0,
    },
    prompt_per_second: emptyHistogram,
    tokens_per_second: emptyHistogram,
  };
}

// Completed-list is empty in these tests so the only rows on screen come from the
// running-connections panel (no model-name collisions between the two tables).
function makeEmptyPage(): UsagePage {
  return { data: [], page: 1, limit: 25, total: 0, total_pages: 0 };
}

function makeActive(overrides: Partial<ActiveRequest> = {}): ActiveRequest {
  return {
    id: 'act_1',
    user_id: 'usr_1',
    user_name: 'Live User',
    token_id: 'tok_1',
    token_name: 'Live Token',
    model: 'live-model',
    // Deliberately different from `model` (as if a token override were in play)
    // so the two model columns stay individually addressable in these tests.
    requested_model: 'client-model',
    server_name: 'live-server',
    api_flavor: 'openai',
    req_path: '/v1/chat/completions',
    provider_path: '/v1/chat/completions',
    provider_model: 'upstream-live-model',
    stream: true,
    started_at: '2026-07-16T12:00:00.000Z',
    ...overrides,
  };
}

type ApiOverrides = {
  activeRequests?: PortalApi['activeRequests'];
  subscribeActivity?: PortalApi['subscribeActivity'];
};

function makeApi(over: ApiOverrides = {}) {
  const unsubscribe = vi.fn();
  const api = {
    activity: vi.fn(async () => makeEmptyPage()),
    activityStats: vi.fn(async () => makeStats()),
    subscribeActivity: over.subscribeActivity ?? vi.fn(() => unsubscribe),
    activeRequests: over.activeRequests ?? vi.fn(async () => ({ data: [makeActive()] })),
    usageTimeSeries: vi.fn(async () => ({ points: [], bucket_seconds: 5, from: '', to: '' })),
    tokens: vi.fn(async () => ({ data: [] })),
    adminUsers: vi.fn(async () => ({ data: [] })),
    userTokens: vi.fn(async () => ({ data: [] })),
    // Not exercised by this suite (running-connections panel only); see
    // Activity.capture.test.tsx / Activity.test.tsx for capture/currency/group coverage.
    getCurrency: vi.fn(async () => ({ usd_per_eur: 0 })),
    usageGroups: vi.fn(async () => ({ data: [], group_by: 'server' })),
    captureDetail: vi.fn(async () => ({
      id: '',
      api_flavor: '',
      http_status: 0,
      created_at: '',
      req_headers: {},
      req_body: '',
      resp_headers: {},
      resp_body: '',
      truncated: false,
      secret: false,
      can_toggle_secret: false,
    })),
    deleteCapture: vi.fn(async () => ({ ok: true })),
    setCaptureSecret: vi.fn(async () => ({ ok: true })),
  };
  return { api, unsubscribe };
}

function renderActivity(over: ApiOverrides = {}, role = 'user') {
  const { api } = makeApi(over);
  render(
    <ToastProvider>
      <Activity t={t} api={api} role={role} onUnauthorized={vi.fn()} />
    </ToastProvider>,
  );
  return { api };
}

// The scope control is now a non-native MUI Select (SelectField): open it, then
// click the option. Options render in a portal on document.body.
async function selectScope(optionText: string) {
  fireEvent.mouseDown(screen.getByRole('combobox', { name: t.activityScopeLabel }));
  fireEvent.click(await screen.findByRole('option', { name: optionText }));
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('Activity running-connections panel', () => {
  it('renders a row from api.activeRequests with model, token, path, server and elapsed', async () => {
    renderActivity({ activeRequests: vi.fn(async () => ({ data: [makeActive()] })) });

    const modelCell = await screen.findByRole('cell', { name: 'live-model' });
    const row = modelCell.closest('tr')!;
    expect(within(row).getByText('Live Token')).toBeInTheDocument();
    // The path is now its own column (the api_flavor suffix was dropped in the
    // list-view refactor) and the server name is a separate column.
    expect(within(row).getByText('/v1/chat/completions')).toBeInTheDocument();
    expect(within(row).getByText('live-server')).toBeInTheDocument();
    // The last cell is the live elapsed value ("Xs" or "m:ss").
    const cells = within(row).getAllByRole('cell');
    expect(cells[cells.length - 1].textContent).toMatch(/^\d+s$|^\d+:\d{2}$/);
    // The panel carries its own heading (the same label also leads the stat tiles).
    expect(
      screen.getByRole('heading', { level: 2, name: t.activityActiveTitle }),
    ).toBeInTheDocument();
  });

  it('shows the pre-override requested model of a running request by default', async () => {
    // A token override is in play: the request routes to "live-model" while the
    // client asked for "client-model". Seeing that second name without waiting
    // for the request to finish is the reason this column exists, so it must be
    // visible without touching the column menu.
    renderActivity({ activeRequests: vi.fn(async () => ({ data: [makeActive()] })) });

    const modelCell = await screen.findByRole('cell', { name: 'live-model' });
    const row = modelCell.closest('tr')!;
    expect(within(row).getByRole('cell', { name: 'client-model' })).toBeInTheDocument();
    // Scoped to THIS table: the completed-requests table on the same page also
    // carries a requested-model header.
    const table = modelCell.closest('table')!;
    expect(
      within(table).getByRole('columnheader', { name: new RegExp(t.tableRequestedModel) }),
    ).toBeInTheDocument();
  });

  it('renders a dash when a running request carries no requested model', async () => {
    renderActivity({
      activeRequests: vi.fn(async () => ({ data: [makeActive({ requested_model: '' })] })),
    });

    const row = (await screen.findByRole('cell', { name: 'live-model' })).closest('tr')!;
    expect(within(row).getByRole('cell', { name: '-' })).toBeInTheDocument();
  });

  it('shows the session label when a running request has no token', async () => {
    // Token-less session chat: token_id is empty (it still carries the user's
    // display name as token_name) -> the panel shows the session label.
    renderActivity({
      activeRequests: vi.fn(async () => ({
        data: [makeActive({ token_id: '', token_name: 'Dev User' })],
      })),
    });
    expect(await screen.findByText(t.activityActiveSession)).toBeInTheDocument();
  });

  it('shows the empty label when there are no running connections', async () => {
    renderActivity({ activeRequests: vi.fn(async () => ({ data: [] })) });
    expect(await screen.findByText(t.activityActiveEmpty)).toBeInTheDocument();
  });

  it('keeps the page alive (no alert) when api.activeRequests rejects', async () => {
    renderActivity({ activeRequests: vi.fn(async () => Promise.reject(new Error('boom'))) });
    // The completed-list empty state still renders; the active fetch failure is swallowed.
    expect(await screen.findByText(t.activityEmpty)).toBeInTheDocument();
    expect(screen.getByText(t.activityActiveEmpty)).toBeInTheDocument();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('refetches the running connections on an SSE signal', async () => {
    let signal: () => void = () => {};
    const subscribeActivity = vi.fn((cb: () => void) => {
      signal = cb;
      return vi.fn();
    }) as unknown as PortalApi['subscribeActivity'];
    const activeRequests = vi.fn(async () => ({ data: [makeActive()] }));
    renderActivity({ subscribeActivity, activeRequests });

    await screen.findByRole('cell', { name: 'live-model' });
    const before = (activeRequests as unknown as { mock: { calls: unknown[][] } }).mock.calls
      .length;

    act(() => signal());

    await waitFor(() =>
      expect(
        (activeRequests as unknown as { mock: { calls: unknown[][] } }).mock.calls,
      ).toHaveLength(before + 1),
    );
  });

  it('requests scope=all when an admin switches to the all-users scope', async () => {
    const activeRequests = vi.fn(async () => ({ data: [makeActive()] }));
    renderActivity({ activeRequests }, 'admin');

    await screen.findByRole('cell', { name: 'live-model' });
    expect(activeRequests).toHaveBeenCalledWith('own', { user_id: undefined, token_id: undefined });

    await selectScope(t.activityScopeAll);

    await waitFor(() =>
      expect(activeRequests).toHaveBeenCalledWith('all', {
        user_id: undefined,
        token_id: undefined,
      }),
    );
  });

  it('persists the scope to localStorage on change and restores it on a fresh remount', async () => {
    installStorage(); // fresh storage, default scope = own
    const first = renderActivity(
      { activeRequests: vi.fn(async () => ({ data: [makeActive()] })) },
      'admin',
    );

    await screen.findByRole('cell', { name: 'live-model' });
    expect(first.api.activeRequests).toHaveBeenCalledWith('own', {
      user_id: undefined,
      token_id: undefined,
    });

    await selectScope(t.activityScopeAll);
    await waitFor(() =>
      expect(first.api.activeRequests).toHaveBeenCalledWith('all', {
        user_id: undefined,
        token_id: undefined,
      }),
    );
    // The change effect persisted the scope (writeScope).
    expect(localStorage.getItem('op.activity.scope')).toBe('all');

    // A fresh mount (navigate away and back) reads scope=all from localStorage.
    cleanup();
    const second = renderActivity(
      { activeRequests: vi.fn(async () => ({ data: [makeActive()] })) },
      'admin',
    );

    await waitFor(() =>
      expect(second.api.activeRequests).toHaveBeenCalledWith('all', {
        user_id: undefined,
        token_id: undefined,
      }),
    );
    // The non-native Select shows the selected option's text (not an input .value).
    expect(screen.getByRole('combobox', { name: t.activityScopeLabel }).textContent).toBe(
      t.activityScopeAll,
    );
  });

  it('adds a Benutzer column showing the display name in the all-scope view', async () => {
    installStorage();
    const activeRequests = vi.fn(async () => ({
      data: [makeActive({ user_id: 'usr_99', user_name: 'Alice Active' })],
    }));
    renderActivity({ activeRequests }, 'admin');

    await screen.findByRole('cell', { name: 'live-model' });
    await selectScope(t.activityScopeAll);

    // The running panel gains its own owner/Benutzer column header (scoped to the
    // running table so it never matches the completed table's owner column).
    const activeTable = (await screen.findByRole('cell', { name: 'live-model' })).closest('table')!;
    await waitFor(() =>
      expect(
        within(activeTable).getByRole('columnheader', { name: t.activityColOwner }),
      ).toBeInTheDocument(),
    );
    // The row shows the display name (preferred over the id).
    const row = within(activeTable).getByRole('cell', { name: 'live-model' }).closest('tr')!;
    expect(within(row).getByText('Alice Active')).toBeInTheDocument();
    expect(within(row).queryByText('usr_99')).not.toBeInTheDocument();
  });

  it('falls back to the user id in the Benutzer column when the name is empty', async () => {
    installStorage();
    const activeRequests = vi.fn(async () => ({
      data: [makeActive({ user_id: 'usr_99', user_name: '' })],
    }));
    renderActivity({ activeRequests }, 'admin');

    await screen.findByRole('cell', { name: 'live-model' });
    await selectScope(t.activityScopeAll);

    const activeTable = (await screen.findByRole('cell', { name: 'live-model' })).closest('table')!;
    const row = within(activeTable).getByRole('cell', { name: 'live-model' }).closest('tr')!;
    await waitFor(() => expect(within(row).getByText('usr_99')).toBeInTheDocument());
  });

  it('has no Benutzer column in the own-scope view', async () => {
    installStorage();
    const activeRequests = vi.fn(async () => ({
      data: [makeActive({ user_id: 'usr_99', user_name: 'Alice Active' })],
    }));
    renderActivity({ activeRequests }, 'admin'); // default scope = own

    const modelCell = await screen.findByRole('cell', { name: 'live-model' });
    const activeTable = modelCell.closest('table')!;
    expect(
      within(activeTable).queryByRole('columnheader', { name: t.activityColOwner }),
    ).not.toBeInTheDocument();
    // No owner cell -> neither the name nor the id shows in the running row.
    const row = modelCell.closest('tr')!;
    expect(within(row).queryByText('Alice Active')).not.toBeInTheDocument();
    expect(within(row).queryByText('usr_99')).not.toBeInTheDocument();
  });

  it('ticks the elapsed label every second while requests are running', async () => {
    vi.useFakeTimers();
    try {
      const base = new Date('2026-07-16T12:00:00.000Z');
      vi.setSystemTime(base);
      const activeRequests = vi.fn(async () => ({
        data: [makeActive({ started_at: base.toISOString() })],
      }));
      render(
        <ToastProvider>
          <Activity
            t={t}
            api={makeApi({ activeRequests }).api}
            role="user"
            onUnauthorized={vi.fn()}
          />
        </ToastProvider>,
      );

      // Flush the mount load (promises resolve as microtasks under fake timers).
      await act(async () => {
        await vi.advanceTimersByTimeAsync(0);
      });
      expect(screen.getByText('0s')).toBeInTheDocument();

      // The 1s ticker advances the elapsed label.
      await act(async () => {
        await vi.advanceTimersByTimeAsync(3000);
      });
      expect(screen.getByText('3s')).toBeInTheDocument();
      expect(screen.queryByText('0s')).not.toBeInTheDocument();
    } finally {
      vi.useRealTimers();
    }
  });
});
