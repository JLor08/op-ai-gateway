// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { Activity } from './Activity';
import { ToastProvider } from './shared/ToastProvider';
import { messages } from '../i18n';
import type { PortalToken, UsagePage, UsageStats } from '../api';

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
function makeToken(over: Partial<PortalToken> = {}): PortalToken {
  return {
    id: 'tok_own',
    name: 'My Token',
    secret_prefix: 'opaigw_',
    status: 'active',
    scopes: ['gateway:use'],
    expires_at: null,
    last_used_at: null,
    created_at: '2026-07-11T00:00:00Z',
    model_override: '',
    log_communication: false,
    secret: false,
    is_chat_session: false,
    deletable: true,
    ...over,
  };
}

function makeApi() {
  const unsubscribe = vi.fn();
  const api = {
    activity: vi.fn(async () => makeEmptyPage()),
    activityStats: vi.fn(async () => makeStats()),
    activeRequests: vi.fn(async () => ({ data: [] })),
    usageTimeSeries: vi.fn(async () => ({ points: [], bucket_seconds: 5, from: '', to: '' })),
    subscribeActivity: vi.fn(() => unsubscribe),
    tokens: vi.fn(async () => ({ data: [makeToken()] })),
    // Not exercised by this suite (custom date-range picker only).
    adminUsers: vi.fn(async () => ({ data: [] })),
    userTokens: vi.fn(async () => ({ data: [] })),
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
  return { api };
}

function renderActivity(role = 'user') {
  const { api } = makeApi();
  render(
    <ToastProvider>
      <Activity t={t} api={api} role={role} onUnauthorized={vi.fn()} />
    </ToastProvider>,
  );
  return { api };
}

async function pick(comboLabel: string, optionText: string) {
  fireEvent.mouseDown(screen.getByRole('combobox', { name: comboLabel }));
  fireEvent.click(await screen.findByRole('option', { name: optionText }));
}
function lastArg(fn: unknown) {
  return (fn as { mock: { calls: unknown[][] } }).mock.calls.at(-1)![0] as Record<string, unknown>;
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('Activity custom absolute time range', () => {
  it('sends range=all + the absolute from/to bounds (not range=custom) when custom is chosen', async () => {
    installStorage();
    const { api } = renderActivity('user');
    await screen.findByRole('combobox', { name: t.activityRangeLabel });

    await pick(t.activityRangeLabel, t.activityRangeCustom);
    // The two datetime inputs appear only in custom mode.
    fireEvent.change(screen.getByLabelText(t.activityRangeFrom), {
      target: { value: '2026-01-01T00:00' },
    });
    fireEvent.change(screen.getByLabelText(t.activityRangeTo), {
      target: { value: '2026-02-01T00:00' },
    });

    await waitFor(() => expect(lastArg(api.activity).time_from).toBe('2026-01-01T00:00'));
    const q = lastArg(api.activity);
    expect(q.time_to).toBe('2026-02-01T00:00');
    // range is coerced to "all" so the preset lower bound doesn't also clip; the
    // client never sends the "custom" pseudo-value to the backend.
    expect(q.range).toBe('all');
  });

  it('drops the custom bounds when switching back to a preset (range=7d, no custom from/to)', async () => {
    installStorage();
    const { api } = renderActivity('user');
    await screen.findByRole('combobox', { name: t.activityRangeLabel });

    await pick(t.activityRangeLabel, t.activityRangeCustom);
    fireEvent.change(screen.getByLabelText(t.activityRangeFrom), {
      target: { value: '2026-01-01T00:00' },
    });
    await waitFor(() => expect(lastArg(api.activity).time_from).toBe('2026-01-01T00:00'));

    // Back to a preset: the query must carry range=7d and no longer the custom bound.
    await pick(t.activityRangeLabel, t.activityRange7d);
    await waitFor(() => expect(lastArg(api.activity).range).toBe('7d'));
    expect(lastArg(api.activity).time_from).not.toBe('2026-01-01T00:00');
    // The datetime inputs are gone (custom-only).
    expect(screen.queryByLabelText(t.activityRangeFrom)).not.toBeInTheDocument();
  });
});
