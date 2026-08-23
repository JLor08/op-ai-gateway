// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { Activity } from './Activity';
import { ToastProvider } from './shared/ToastProvider';
import { messages } from '../i18n';
import type { UsageEvent, UsagePage, UsageStats } from '../api';

const t = messages.de;

function makeRow(overrides: Partial<UsageEvent> = {}): UsageEvent {
  return {
    id: 'req_1',
    user_id: 'usr_1',
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

// total > limit so the table shows enough rows to page/sort; content irrelevant.
function makePage(): UsagePage {
  return { data: [makeRow()], page: 1, limit: 25, total: 60, total_pages: 3 };
}

function setup() {
  let signal: () => void = () => {};
  let reconnect: () => void = () => {};
  const unsubscribe = vi.fn();
  const api = {
    activity: vi.fn(async () => makePage()),
    activityStats: vi.fn(async () => makeStats()),
    activeRequests: vi.fn(async () => ({ data: [] })),
    usageTimeSeries: vi.fn(async () => ({ points: [], bucket_seconds: 5, from: '', to: '' })),
    subscribeActivity: vi.fn((cb: () => void, onReconnect?: () => void) => {
      signal = cb;
      if (onReconnect) reconnect = onReconnect;
      return unsubscribe;
    }),
    tokens: vi.fn(async () => ({ data: [] })),
    adminUsers: vi.fn(async () => ({ data: [] })),
    userTokens: vi.fn(async () => ({ data: [] })),
    // Not exercised by this suite (SSE reconnect/backoff only).
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
  render(
    <ToastProvider>
      <Activity t={t} api={api} role="user" onUnauthorized={vi.fn()} />
    </ToastProvider>,
  );
  const activityCalls = () =>
    (api.activity as unknown as { mock: { calls: unknown[][] } }).mock.calls.length;
  const statsCalls = () =>
    (api.activityStats as unknown as { mock: { calls: unknown[][] } }).mock.calls.length;
  // fireReconnect delivers the wrapper's onReconnect callback, i.e. what the
  // EventSource wrapper invokes on a re-open after an onerror -> onopen (a
  // dropped stream that comes back), the reconnect-resync seam.
  return {
    api,
    fireSignal: () => act(() => signal()),
    fireReconnect: () => act(() => reconnect()),
    activityCalls,
    statsCalls,
  };
}

afterEach(cleanup);

describe('Activity SSE behavior', () => {
  it('on the newest view a signal refetches both stats and the list, with no pill', async () => {
    const { fireSignal, activityCalls, statsCalls } = setup();
    await screen.findByRole('cell', { name: 'qwen-coder' });
    const listBefore = activityCalls();
    const statsBefore = statsCalls();

    fireSignal();

    await waitFor(() => expect(statsCalls()).toBe(statsBefore + 1));
    await waitFor(() => expect(activityCalls()).toBe(listBefore + 1));
    expect(
      screen.queryByRole('button', { name: t.activityNewRequests(1) }),
    ).not.toBeInTheDocument();
  });

  it("off the newest view a signal refetches stats only and shows the 'N neue' pill", async () => {
    const { fireSignal, activityCalls, statsCalls } = setup();
    await screen.findByRole('cell', { name: 'qwen-coder' });

    // Leave the newest view: sort by a different column (created_at desc -> latency asc).
    fireEvent.click(screen.getByRole('button', { name: t.activityColTime }));
    await waitFor(() =>
      expect(
        screen.getByRole('columnheader', { name: new RegExp(t.activityColTime) }),
      ).toHaveAttribute('aria-sort', 'ascending'),
    );
    const listBefore = activityCalls();
    const statsBefore = statsCalls();

    fireSignal();
    fireSignal();

    // stats stayed live (leading-edge fired once); list untouched.
    await waitFor(() => expect(statsCalls()).toBe(statsBefore + 1));
    expect(activityCalls()).toBe(listBefore);
    expect(screen.getByRole('button', { name: t.activityNewRequests(2) })).toBeInTheDocument();
  });

  it('clicking the pill refetches the list and clears the counter (resync)', async () => {
    const { fireSignal, activityCalls } = setup();
    await screen.findByRole('cell', { name: 'qwen-coder' });
    fireEvent.click(screen.getByRole('button', { name: t.activityColTime }));
    await waitFor(() =>
      expect(
        screen.getByRole('columnheader', { name: new RegExp(t.activityColTime) }),
      ).toHaveAttribute('aria-sort', 'ascending'),
    );
    fireSignal();
    const pill = await screen.findByRole('button', { name: t.activityNewRequests(1) });
    const before = activityCalls();

    fireEvent.click(pill);

    await waitFor(() => expect(activityCalls()).toBe(before + 1));
    await waitFor(() =>
      expect(
        screen.queryByRole('button', { name: t.activityNewRequests(1) }),
      ).not.toBeInTheDocument(),
    );
  });

  it('resyncs stats and resets the pill on reconnect (onerror -> onopen)', async () => {
    const { fireSignal, fireReconnect, activityCalls, statsCalls } = setup();
    await screen.findByRole('cell', { name: 'qwen-coder' });

    // Leave the newest view so signals build the pill instead of refetching the list.
    fireEvent.click(screen.getByRole('button', { name: t.activityColTime }));
    await waitFor(() =>
      expect(
        screen.getByRole('columnheader', { name: new RegExp(t.activityColTime) }),
      ).toHaveAttribute('aria-sort', 'ascending'),
    );
    fireSignal();
    fireSignal();
    expect(
      await screen.findByRole('button', { name: t.activityNewRequests(2) }),
    ).toBeInTheDocument();
    const listBefore = activityCalls();
    const statsBefore = statsCalls();

    // The stream drops and re-opens: the wrapper delivers onReconnect.
    fireReconnect();

    // Stats refetch fires (window-wide) and the pill counter is reset to 0; off the
    // newest view the list is left untouched (no increment, no list refetch).
    await waitFor(() => expect(statsCalls()).toBe(statsBefore + 1));
    expect(activityCalls()).toBe(listBefore);
    await waitFor(() =>
      expect(
        screen.queryByRole('button', { name: t.activityNewRequests(2) }),
      ).not.toBeInTheDocument(),
    );
  });
});
