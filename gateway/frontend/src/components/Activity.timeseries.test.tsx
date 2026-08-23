// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { Activity } from './Activity';
import { ToastProvider } from './shared/ToastProvider';
import { messages } from '../i18n';
import type { TimeSeries, UsagePage, UsageStats } from '../api';
import type { PortalApi } from './shared/types';

const t = messages.de;

// Fresh, inspectable localStorage per test so the persisted window/bucket prefs
// are deterministic and assertable (mirrors the other Activity test suites).
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

function makeEmptyPage(): UsagePage {
  return { data: [], page: 1, limit: 25, total: 0, total_pages: 0 };
}

function makeTs(): TimeSeries {
  return {
    points: [
      {
        t: '2026-07-16T12:00:00Z',
        connections: 1,
        concurrency: 1,
        prompt_tokens_per_second: 4,
        completion_tokens_per_second: 7,
      },
      {
        t: '2026-07-16T12:00:05Z',
        connections: 2,
        concurrency: 1,
        prompt_tokens_per_second: 5,
        completion_tokens_per_second: 8,
      },
      {
        t: '2026-07-16T12:00:10Z',
        connections: 3,
        concurrency: 2,
        prompt_tokens_per_second: 6,
        completion_tokens_per_second: 9,
      },
    ],
    bucket_seconds: 5,
    from: '2026-07-16T11:55:10Z',
    to: '2026-07-16T12:00:10Z',
  };
}

type Over = {
  usageTimeSeries?: PortalApi['usageTimeSeries'];
  subscribeActivity?: PortalApi['subscribeActivity'];
};

function makeApi(over: Over = {}) {
  const unsubscribe = vi.fn();
  const api = {
    activity: vi.fn(async () => makeEmptyPage()),
    activityStats: vi.fn(async () => makeStats()),
    activeRequests: vi.fn(async () => ({ data: [] })),
    usageTimeSeries: over.usageTimeSeries ?? vi.fn(async () => makeTs()),
    subscribeActivity: over.subscribeActivity ?? vi.fn(() => unsubscribe),
    tokens: vi.fn(async () => ({ data: [] })),
    adminUsers: vi.fn(async () => ({ data: [] })),
    userTokens: vi.fn(async () => ({ data: [] })),
    // Not exercised by this suite (time-series bucket/window controls only).
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

function renderActivity(over: Over = {}, role = 'user') {
  const { api } = makeApi(over);
  render(
    <ToastProvider>
      <Activity t={t} api={api} role={role} onUnauthorized={vi.fn()} />
    </ToastProvider>,
  );
  return { api };
}

function tsCalls(api: Pick<PortalApi, 'usageTimeSeries'>) {
  return (api.usageTimeSeries as unknown as { mock: { calls: unknown[][] } }).mock.calls;
}
function lastTsArg(api: Pick<PortalApi, 'usageTimeSeries'>) {
  return tsCalls(api).at(-1)![0] as { window: string; bucket: number; scope: string };
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('Activity time-series charts', () => {
  it('renders the three line charts from api.usageTimeSeries with the persisted defaults', async () => {
    installStorage();
    const { api } = renderActivity();

    expect(await screen.findByText(t.activityTsConnections)).toBeInTheDocument();
    expect(screen.getByText(t.activityTsPromptThroughput)).toBeInTheDocument();
    expect(screen.getByText(t.activityTsCompletionThroughput)).toBeInTheDocument();
    // Chart 1 is a two-series chart: its legend carries both series labels.
    expect(screen.getByText(t.activityTsConnectionsThroughput)).toBeInTheDocument();
    expect(screen.getByText(t.activityTsConcurrency)).toBeInTheDocument();

    await waitFor(() => expect(api.usageTimeSeries).toHaveBeenCalled());
    expect(lastTsArg(api)).toMatchObject({ window: '5m', bucket: 5, scope: 'own' });
  });

  it('switches + persists the window and refetches', async () => {
    const store = installStorage();
    const { api } = renderActivity();
    await screen.findByText(t.activityTsConnections);

    // Window is a MUI Select now: open it, then pick the "15 Min" option (label
    // formatted from the 900s window duration).
    fireEvent.mouseDown(screen.getByRole('combobox', { name: t.activityTsWindowLabel }));
    fireEvent.click(await screen.findByRole('option', { name: `15 ${t.tsUnitMin}` }));

    await waitFor(() => expect(lastTsArg(api).window).toBe('15m'));
    expect(store.get('op.activity.tsWindow')).toBe('15m');
  });

  it('switches + persists the resolution (bucket) and refetches', async () => {
    const store = installStorage();
    const { api } = renderActivity();
    await screen.findByText(t.activityTsConnections);

    // Resolution is a MUI Select now: open it, then pick the "10s" option.
    fireEvent.mouseDown(screen.getByRole('combobox', { name: t.activityTsBucketLabel }));
    fireEvent.click(await screen.findByRole('option', { name: '10s' }));

    await waitFor(() => expect(lastTsArg(api).bucket).toBe(10));
    expect(store.get('op.activity.tsBucket')).toBe('10');
  });

  it('is scope-aware: an admin switching to all refetches the series with scope=all', async () => {
    installStorage();
    const { api } = renderActivity({}, 'admin');
    await screen.findByText(t.activityTsConnections);
    expect(lastTsArg(api).scope).toBe('own');

    fireEvent.mouseDown(screen.getByRole('combobox', { name: t.activityScopeLabel }));
    fireEvent.click(await screen.findByRole('option', { name: t.activityScopeAll }));

    await waitFor(() => expect(lastTsArg(api).scope).toBe('all'));
  });

  it('refetches the time-series on an SSE signal', async () => {
    installStorage();
    let signal: () => void = () => {};
    const subscribeActivity = vi.fn((cb: () => void) => {
      signal = cb;
      return vi.fn();
    }) as unknown as PortalApi['subscribeActivity'];
    const { api } = renderActivity({ subscribeActivity });

    await screen.findByText(t.activityTsConnections);
    const before = tsCalls(api).length;

    act(() => signal());

    await waitFor(() => expect(tsCalls(api).length).toBeGreaterThan(before));
  });
});
