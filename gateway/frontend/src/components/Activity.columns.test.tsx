// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { Activity } from './Activity';
import { ToastProvider } from './shared/ToastProvider';
import { messages } from '../i18n';
import type { UsageEvent, UsagePage, UsageStats } from '../api';

const t = messages.de;

function installStorage() {
  const store = new Map<string, string>();
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
  return store;
}

function makeRow(overrides: Partial<UsageEvent> = {}): UsageEvent {
  return {
    id: 'req_1',
    user_id: 'usr_42',
    token_id: 'tok_1',
    api_flavor: 'portal_chat',
    model: 'qwen-coder',
    provider: 'mock',
    host: 'mock-host',
    input_tokens: 2,
    output_tokens: 6,
    total_tokens: 8,
    latency_ms: 14,
    status: 'success',
    created_at: '2026-07-10T12:01:00Z',
    cached_tokens: 0,
    prompt_per_second: 12.5,
    tokens_per_second: 40,
    http_status: 200,
    content_type: 'application/json',
    req_path: '/v1/chat/completions',
    provider_model: 'qwen2.5',
    stream: true,
    token_name: 'Dev Token',
    server_name: 'GPU 1',
    user_name: 'Alice Admin',
    ...overrides,
  } as UsageEvent;
}

const emptyHistogram = { bins: [], min: 0, max: 0, bin_size: 0, p50: 0, p95: 0, p99: 0 };
function makeStats(): UsageStats {
  return {
    totals: {
      total_requests: 1,
      error_count: 0,
      cached_tokens: 0,
      cache_write_tokens: 0,
      input_tokens: 2,
      output_tokens: 6,
    },
    prompt_per_second: emptyHistogram,
    tokens_per_second: emptyHistogram,
  };
}
function makePage(): UsagePage {
  return { data: [makeRow()], page: 1, limit: 25, total: 1, total_pages: 1 };
}

function makeApi() {
  return {
    activity: vi.fn(async () => makePage()),
    activityStats: vi.fn(async () => makeStats()),
    activeRequests: vi.fn(async () => ({ data: [] })),
    usageTimeSeries: vi.fn(async () => ({ points: [], bucket_seconds: 5, from: '', to: '' })),
    subscribeActivity: vi.fn(() => vi.fn()),
    tokens: vi.fn(async () => ({ data: [] })),
    adminUsers: vi.fn(async () => ({ data: [] })),
    userTokens: vi.fn(async () => ({ data: [] })),
    // Not exercised by this suite (column-visibility/order only).
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
}

// The scope switch is now a non-native MUI Select (SelectField): open the
// combobox then click the option, rather than firing a native change event.
async function selectScope(optionName: string) {
  fireEvent.mouseDown(screen.getByRole('combobox', { name: t.activityScopeLabel }));
  fireEvent.click(await screen.findByRole('option', { name: optionName }));
}

function renderActivity(role: string) {
  const api = makeApi();
  render(
    <ToastProvider>
      <Activity t={t} api={api} role={role} onUnauthorized={vi.fn()} />
    </ToastProvider>,
  );
  return api;
}

beforeEach(() => installStorage());
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('Activity admin scope + columns', () => {
  it('hides the scope switch and owner column for a plain user', async () => {
    renderActivity('user');
    await screen.findByRole('cell', { name: 'qwen-coder' });
    expect(screen.queryByLabelText(t.activityScopeLabel)).not.toBeInTheDocument();
    expect(
      screen.queryByRole('columnheader', { name: t.activityColOwner }),
    ).not.toBeInTheDocument();
  });

  it('renders the scope switch at the top of the view, above the stat tiles', async () => {
    renderActivity('admin');
    await screen.findByRole('cell', { name: 'qwen-coder' });
    const scope = screen.getByLabelText(t.activityScopeLabel);
    const stats = screen.getByLabelText(t.activityStatsLabel);
    // The single toggle governs the whole view, so it must PRECEDE the stat tiles.
    expect(stats.compareDocumentPosition(scope) & Node.DOCUMENT_POSITION_PRECEDING).toBeTruthy();
  });

  it('reveals the owner column for a plain admin who switches to all-scope', async () => {
    const api = renderActivity('admin');
    // Scope to the COMPLETED table (the running panel also owns an all-scope owner
    // column now, so an unscoped columnheader query would match two).
    const completed = (await screen.findByRole('cell', { name: 'qwen-coder' })).closest('table')!;
    expect(
      within(completed).queryByRole('columnheader', { name: t.activityColOwner }),
    ).not.toBeInTheDocument();

    await selectScope(t.activityScopeAll);

    await waitFor(() =>
      expect(
        within(completed).getByRole('columnheader', { name: t.activityColOwner }),
      ).toBeInTheDocument(),
    );
    expect(within(completed).getByRole('cell', { name: 'Alice Admin' })).toBeInTheDocument();
    await waitFor(() => {
      const last = (api.activity as unknown as { mock: { calls: unknown[][] } }).mock.calls.at(-1)!;
      expect(last[0]).toMatchObject({ scope: 'all' });
    });
  });

  it('reveals the owner column for a system-admin who switches to all-scope', async () => {
    const api = renderActivity('system_admin');
    const completed = (await screen.findByRole('cell', { name: 'qwen-coder' })).closest('table')!;
    expect(
      within(completed).queryByRole('columnheader', { name: t.activityColOwner }),
    ).not.toBeInTheDocument();

    await selectScope(t.activityScopeAll);

    await waitFor(() =>
      expect(
        within(completed).getByRole('columnheader', { name: t.activityColOwner }),
      ).toBeInTheDocument(),
    );
    expect(within(completed).getByRole('cell', { name: 'Alice Admin' })).toBeInTheDocument();
    await waitFor(() => {
      const last = (api.activity as unknown as { mock: { calls: unknown[][] } }).mock.calls.at(-1)!;
      expect(last[0]).toMatchObject({ scope: 'all' });
    });
  });

  it('persists a hidden column to the user profile and drops it from the table', async () => {
    const store = installStorage();
    renderActivity('user');
    // Scope queries to the usage table region: the running-connections panel is
    // its own table with its own "Spalten" button and its own "Modell" column,
    // so unscoped queries would match both.
    const usage = screen.getByRole('region', { name: t.usageTableTitle });
    await within(usage).findByRole('cell', { name: 'qwen-coder' });

    // Open the column menu (its button now lives inside the table toolbar) and
    // hide the Model column.
    fireEvent.click(within(usage).getByRole('button', { name: t.listColumns }));
    fireEvent.click(await screen.findByRole('checkbox', { name: t.tableModel }));

    await waitFor(() =>
      expect(
        within(usage).queryByRole('columnheader', { name: t.tableModel }),
      ).not.toBeInTheDocument(),
    );
    // Column visibility now persists at the user profile via usePreference; with
    // no PreferencesProvider mounted it falls back to the localStorage mirror
    // (key "op.pref." + "table.activity.hidden"), a JSON array of hidden ids.
    await waitFor(() => {
      const raw = store.get('op.pref.table.activity.hidden');
      expect(raw).toBeTruthy();
      expect(JSON.parse(raw!)).toContain('model');
    });
  });

  it('shows the service column when enabled via the column menu (Phase 1 service accounts)', async () => {
    const api = makeApi();
    (api.activity as unknown as { mockResolvedValue: (v: UsagePage) => void }).mockResolvedValue({
      data: [makeRow({ service_name: 'Nightly Batch' })],
      page: 1,
      limit: 25,
      total: 1,
      total_pages: 1,
    });
    render(
      <ToastProvider>
        <Activity t={t} api={api} role="user" onUnauthorized={vi.fn()} />
      </ToastProvider>,
    );
    const usage = screen.getByRole('region', { name: t.usageTableTitle });
    await within(usage).findByRole('cell', { name: 'qwen-coder' });
    // Hidden by default: no "Dienst" column/cell yet.
    expect(
      within(usage).queryByRole('columnheader', { name: new RegExp(t.activityColService) }),
    ).not.toBeInTheDocument();

    fireEvent.click(within(usage).getByRole('button', { name: t.listColumns }));
    const serviceCheckbox = await screen.findByRole('checkbox', { name: t.activityColService });
    fireEvent.click(serviceCheckbox);
    // Close the column menu (Escape dispatched on a node inside the Menu's
    // portal so it bubbles to the Modal's keydown handler; the rest of the
    // page is aria-hidden while the Modal is open, so a later getByRole on
    // "usage" would otherwise find nothing — mirrors Activity.test.tsx's
    // cost-column-toggle test).
    fireEvent.keyDown(serviceCheckbox, { key: 'Escape' });

    await waitFor(() =>
      expect(
        within(usage).getByRole('columnheader', { name: new RegExp(t.activityColService) }),
      ).toBeInTheDocument(),
    );
    expect(within(usage).getByRole('cell', { name: 'Nightly Batch' })).toBeInTheDocument();
  });

  it('persists the owner display choice to the user profile', async () => {
    const store = installStorage();
    const api = makeApi();
    render(
      <ToastProvider>
        <Activity t={t} api={api} role="system_admin" onUnauthorized={vi.fn()} />
      </ToastProvider>,
    );
    const usage = screen.getByRole('region', { name: t.usageTableTitle });
    const completed = (await within(usage).findByRole('cell', { name: 'qwen-coder' })).closest(
      'table',
    )!;
    await selectScope(t.activityScopeAll);
    await waitFor(() =>
      expect(
        within(completed).getByRole('columnheader', { name: t.activityColOwner }),
      ).toBeInTheDocument(),
    );

    fireEvent.click(within(usage).getByRole('button', { name: t.listColumns }));
    fireEvent.click(await screen.findByRole('radio', { name: t.activityOwnerId }));

    await waitFor(() =>
      expect(within(completed).getByRole('cell', { name: 'usr_42' })).toBeInTheDocument(),
    );
    // ownerDisplay persists via usePreference; the localStorage mirror stores the
    // JSON-encoded value under "op.pref." + "table.activity.ownerDisplay".
    await waitFor(() =>
      expect(JSON.parse(store.get('op.pref.table.activity.ownerDisplay')!)).toBe('id'),
    );
  });
});
