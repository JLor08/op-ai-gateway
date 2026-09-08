// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { act, renderHook } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { useActivityData, type UseActivityDataArgs } from './useActivityData';
import { messages } from '../i18n';
import type { ActiveRequest, UsagePage, UsageStats } from '../api';

const t = messages.de;

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

function makeActive(overrides: Partial<ActiveRequest> = {}): ActiveRequest {
  return {
    id: 'act_1',
    user_id: 'usr_1',
    user_name: 'Live User',
    token_id: 'tok_1',
    token_name: 'Live Token',
    model: 'live-model',
    requested_model: 'live-model',
    server_name: 'live-server',
    api_flavor: 'openai',
    req_path: '/v1/chat/completions',
    provider_path: '/v1/chat/completions',
    provider_model: 'upstream-live-model',
    stream: true,
    started_at: '2026-07-16T12:00:00.000Z',
    output_tokens: 0,
    tokens_per_second: 0,
    tokens_per_second_source: '',
    ttft_ms: 0,
    ...overrides,
  };
}

// Only the subset of PortalApi the hook consumes (see UseActivityDataArgs).
function makeApi(activeRequests: UseActivityDataArgs['api']['activeRequests']) {
  const api: UseActivityDataArgs['api'] = {
    activity: vi.fn(async () => makeEmptyPage()),
    activityStats: vi.fn(async () => makeStats()),
    subscribeActivity: vi.fn(() => vi.fn()),
    activeRequests,
    usageTimeSeries: vi.fn(async () => ({ points: [], bucket_seconds: 5, from: '', to: '' })),
  };
  return api;
}

afterEach(() => {
  vi.useRealTimers();
});

describe('useActivityData active-requests poll', () => {
  it('polls the active list while requests are running and stops when the list empties', async () => {
    vi.useFakeTimers();
    // Call 1 is the mount load (one running row). Call 2 is the first 2s poll
    // tick, whose response empties the list -- the interval must not survive
    // that and must not fire again afterwards.
    let call = 0;
    const activeRequests = vi.fn(async () => {
      call += 1;
      return { data: call === 1 ? [makeActive()] : [] };
    });
    const api = makeApi(activeRequests);
    // Hoisted so its reference is stable across re-renders: the hook's mount
    // effect depends on `query` by identity (see useActivityData.ts:207-209),
    // so a fresh `{}` literal recreated on every render would retrigger that
    // effect every time and never settle.
    const query: UseActivityDataArgs['query'] = {};
    const onUnauthorized = vi.fn();

    const { result } = renderHook(() =>
      useActivityData({
        api,
        query,
        newest: true,
        tsWindow: '5m',
        tsBucket: 5,
        t,
        onUnauthorized,
      }),
    );

    // Flush the mount load (promises resolve as microtasks under fake timers).
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(activeRequests).toHaveBeenCalledTimes(1);
    expect(result.current.active).toHaveLength(1);

    // A row is running, so the poll is armed. Advancing 2s fires it.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2000);
    });
    expect(activeRequests).toHaveBeenCalledTimes(2);
    expect(result.current.active).toHaveLength(0);

    // The list is now empty -> the effect tore the interval down and the
    // `active.length === 0` guard stops it rearming one. No further fetches
    // happen no matter how long we wait.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(10000);
    });
    expect(activeRequests).toHaveBeenCalledTimes(2);
  });
});
