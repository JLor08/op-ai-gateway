// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { Activity } from './Activity';
import { formatEnergyWh } from './StatTiles';
import { formatCost } from '../currency';
import { ToastProvider } from './shared/ToastProvider';
import { PreferencesProvider } from './shared/preferences';
import { messages } from '../i18n';
import {
  PortalApiError,
  type UsageEvent,
  type UsageGroupRow,
  type UsagePage,
  type UsageStats,
} from '../api';
import type { PortalApi } from './shared/types';

type Deferred<T> = {
  promise: Promise<T>;
  resolve: (value: T) => void;
  reject: (reason?: unknown) => void;
};
function deferred<T>(): Deferred<T> {
  let resolve!: (value: T) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

function totalsWith(total_requests: number) {
  return {
    total_requests,
    error_count: 0,
    cached_tokens: 0,
    cache_write_tokens: 0,
    input_tokens: 2,
    output_tokens: 6,
  };
}

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
    cache_write_tokens: 0,
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

function makePage(rows: UsageEvent[], overrides: Partial<UsagePage> = {}): UsagePage {
  return { data: rows, page: 1, limit: 25, total: rows.length, total_pages: 1, ...overrides };
}

const emptyHistogram = { bins: [], min: 0, max: 0, bin_size: 0, p50: 0, p95: 0, p99: 0 };

function makeStats(overrides: Partial<UsageStats> = {}): UsageStats {
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
    ...overrides,
  };
}

type ApiOverrides = {
  activity?: PortalApi['activity'];
  activityStats?: PortalApi['activityStats'];
  subscribeActivity?: PortalApi['subscribeActivity'];
  usageTimeSeries?: PortalApi['usageTimeSeries'];
  getCurrency?: PortalApi['getCurrency'];
  preferences?: PortalApi['preferences'];
  usageGroups?: PortalApi['usageGroups'];
};

function makeApi(over: ApiOverrides = {}) {
  const unsubscribe = vi.fn();
  const api = {
    activity: over.activity ?? vi.fn(async () => makePage([makeRow()])),
    activityStats: over.activityStats ?? vi.fn(async () => makeStats()),
    subscribeActivity: over.subscribeActivity ?? vi.fn(() => unsubscribe),
    activeRequests: vi.fn(async () => ({ data: [] })),
    usageTimeSeries:
      over.usageTimeSeries ??
      vi.fn(async () => ({ points: [], bucket_seconds: 5, from: '', to: '' })),
    // Default resolves empty -- exercised only by tests that switch the view into
    // the grouped (chain.length > 0) mode; every other test never calls it.
    usageGroups: over.usageGroups ?? vi.fn(async () => ({ data: [], group_by: 'server' })),
    tokens: vi.fn(async () => ({ data: [] })),
    adminUsers: vi.fn(async () => ({ data: [] })),
    userTokens: vi.fn(async () => ({ data: [] })),
    // Defaults to USD-unavailable (factor 0) -- matches the pre-T5 behavior
    // (Euro-cent-only) for every test that doesn't care about the selector.
    getCurrency: over.getCurrency ?? vi.fn(async () => ({ usd_per_eur: 0 })),
    preferences: over.preferences ?? vi.fn(async () => ({})),
    setPreference: vi.fn(async () => ({ ok: true })),
    // Capture drill-down (never opened by these tests; see Activity.capture.test.tsx).
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

function makeGroupRow(overrides: Partial<UsageGroupRow> = {}): UsageGroupRow {
  return {
    key: 'srv-a',
    key_label: 'GPU A',
    count: 3,
    error_count: 0,
    input_tokens: 10,
    output_tokens: 20,
    total_tokens: 30,
    cached_tokens: 0,
    cache_write_tokens: 0,
    energy_wh: 1,
    cost_eur: 0.1,
    first_at: '2026-07-10T12:00:00Z',
    last_at: '2026-07-10T12:30:00Z',
    ...overrides,
  };
}

function renderActivity(
  over: ApiOverrides = {},
  props: { role?: string; onUnauthorized?: () => void } = {},
) {
  const { api, unsubscribe } = makeApi(over);
  const onUnauthorized = props.onUnauthorized ?? vi.fn();
  render(
    <ToastProvider>
      <Activity t={t} api={api} role={props.role ?? 'user'} onUnauthorized={onUnauthorized} />
    </ToastProvider>,
  );
  return { api, unsubscribe, onUnauthorized };
}

afterEach(cleanup);

describe('Activity self-loading', () => {
  it('fetches list and stats on mount and renders a row', async () => {
    const { api } = renderActivity();
    expect(await screen.findByRole('cell', { name: 'qwen-coder' })).toBeInTheDocument();
    expect(api.activity).toHaveBeenCalledTimes(1);
    expect(api.activityStats).toHaveBeenCalledTimes(1);
  });

  it('shows the loading label before data arrives, then the empty label for no rows', async () => {
    const activity = vi.fn(async () => makePage([]));
    renderActivity({ activity });
    // initial paint: loading is distinct from empty
    expect(screen.getByText(t.loading)).toBeInTheDocument();
    expect(await screen.findByText(t.activityEmpty)).toBeInTheDocument();
    expect(screen.queryByText(t.loading)).not.toBeInTheDocument();
  });

  it('refetches both endpoints when a filter changes', async () => {
    const { api } = renderActivity();
    await screen.findByRole('cell', { name: 'qwen-coder' });
    fireEvent.change(screen.getByLabelText(t.activitySearchLabel), { target: { value: 'req_9' } });
    await waitFor(() => {
      const last = (api.activity as unknown as { mock: { calls: unknown[][] } }).mock.calls.at(-1)!;
      expect((last[0] as { q: string }).q).toBe('req_9');
    });
    expect(
      (api.activityStats as unknown as { mock: { calls: unknown[][] } }).mock.calls.at(-1)![0],
    ).toMatchObject({
      q: 'req_9',
    });
  });

  it('flips the order when the active sort header is clicked', async () => {
    const { api } = renderActivity();
    await screen.findByRole('cell', { name: 'qwen-coder' });
    fireEvent.click(screen.getByRole('button', { name: t.activityColTime })); // active column created_at
    await waitFor(() => {
      const last = (api.activity as unknown as { mock: { calls: unknown[][] } }).mock.calls.at(-1)!;
      expect(last[0]).toMatchObject({ sort: 'created_at', order: 'asc' });
    });
  });

  it('routes a 401 to onUnauthorized without an inline alert', async () => {
    const activity = vi.fn(async () => {
      throw new PortalApiError(401, 'auth.session_invalid', 'expired');
    });
    const { onUnauthorized } = renderActivity({ activity });
    await waitFor(() => expect(onUnauthorized).toHaveBeenCalled());
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('shows an inline alert for a non-401 load error', async () => {
    const activity = vi.fn(async () => {
      throw new PortalApiError(500, 'request.failed', 'server exploded');
    });
    const { onUnauthorized } = renderActivity({ activity });
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('server exploded');
    expect(onUnauthorized).not.toHaveBeenCalled();
  });

  it('subscribes on mount and unsubscribes on unmount', async () => {
    const { unsubscribe } = renderActivity();
    await screen.findByRole('cell', { name: 'qwen-coder' });
    cleanup();
    expect(unsubscribe).toHaveBeenCalled();
  });
});

describe('Activity request ordering (reqIdRef guard)', () => {
  // Test A: a stale load whose response lands AFTER a newer load's must not
  // clobber the view, and the spinner must end cleared.
  it("keeps the newest query's data when an older load resolves last", async () => {
    const activityDeferreds: Deferred<UsagePage>[] = [];
    const statsDeferreds: Deferred<UsageStats>[] = [];
    const activity: PortalApi['activity'] = vi.fn(() => {
      const d = deferred<UsagePage>();
      activityDeferreds.push(d);
      return d.promise;
    });
    const activityStats: PortalApi['activityStats'] = vi.fn(() => {
      const d = deferred<UsageStats>();
      statsDeferreds.push(d);
      return d.promise;
    });
    renderActivity({ activity, activityStats });

    // Settle the mount load first so we start from a known, spinner-cleared state.
    await act(async () => {
      activityDeferreds[0].resolve(makePage([makeRow({ model: 'mount-model' })]));
      statsDeferreds[0].resolve(makeStats());
    });
    expect(await screen.findByRole('cell', { name: 'mount-model' })).toBeInTheDocument();

    // Two successive filter changes fire load A (index 1) then the newer load B (index 2).
    const search = screen.getByLabelText(t.activitySearchLabel);
    fireEvent.change(search, { target: { value: 'A' } });
    fireEvent.change(search, { target: { value: 'B' } });
    await waitFor(() => expect(activityDeferreds).toHaveLength(3));

    // Newer load B resolves FIRST: empty page + distinctive stats (total_requests 42).
    await act(async () => {
      activityDeferreds[2].resolve(makePage([]));
      statsDeferreds[2].resolve(makeStats({ totals: totalsWith(42) }));
    });
    expect(await screen.findByText(t.activityEmpty)).toBeInTheDocument();
    expect(screen.queryByText(t.loading)).not.toBeInTheDocument();
    expect(screen.getByText('42')).toBeInTheDocument();

    // Older load A resolves LAST with a row + stats (total_requests 7); it must be ignored.
    await act(async () => {
      activityDeferreds[1].resolve(makePage([makeRow({ model: 'model-A' })]));
      statsDeferreds[1].resolve(makeStats({ totals: totalsWith(7) }));
    });
    expect(screen.queryByRole('cell', { name: 'model-A' })).not.toBeInTheDocument();
    expect(screen.getByText(t.activityEmpty)).toBeInTheDocument();
    expect(screen.queryByText(t.loading)).not.toBeInTheDocument();
    expect(screen.getByText('42')).toBeInTheDocument();
    expect(screen.queryByText('7')).not.toBeInTheDocument();
  });

  // Test B: an SSE-driven refetch that supersedes an in-flight load and settles
  // last must both win the data AND clear the spinner. Pre-fix, runSseRefetch()
  // committed data but never touched setLoading, stranding loading=true here.
  it('clears loading when an SSE refetch supersedes an in-flight load', async () => {
    const activityDeferreds: Deferred<UsagePage>[] = [];
    const statsDeferreds: Deferred<UsageStats>[] = [];
    const activity: PortalApi['activity'] = vi.fn(() => {
      const d = deferred<UsagePage>();
      activityDeferreds.push(d);
      return d.promise;
    });
    const activityStats: PortalApi['activityStats'] = vi.fn(() => {
      const d = deferred<UsageStats>();
      statsDeferreds.push(d);
      return d.promise;
    });
    let sseOnActivity: (() => void) | null = null;
    const subscribeActivity: PortalApi['subscribeActivity'] = vi.fn((onActivity: () => void) => {
      sseOnActivity = onActivity;
      return vi.fn();
    });
    renderActivity({ activity, activityStats, subscribeActivity });

    // Mount load is in flight (loading=true); its list/stats deferreds exist, unresolved.
    await waitFor(() => expect(activityDeferreds).toHaveLength(1));
    expect(screen.getByText(t.loading)).toBeInTheDocument();

    // Fire an SSE signal mid-flight -> runSseRefetch() bumps reqIdRef past the load.
    act(() => sseOnActivity!());

    // The superseded mount load resolves FIRST: its guard drops the commit and its
    // finally is a no-op, so loading is NOT cleared by it (row must not appear).
    await act(async () => {
      activityDeferreds[0].resolve(makePage([makeRow({ model: 'mount-model' })]));
      statsDeferreds[0].resolve(makeStats({ totals: totalsWith(1) }));
    });
    expect(screen.queryByRole('cell', { name: 'mount-model' })).not.toBeInTheDocument();

    // The SSE refetch settles LAST: stats (total_requests 55) then an empty list.
    await waitFor(() => expect(statsDeferreds).toHaveLength(2));
    await act(async () => {
      statsDeferreds[1].resolve(makeStats({ totals: totalsWith(55) }));
    });
    await waitFor(() => expect(activityDeferreds).toHaveLength(2));
    await act(async () => {
      activityDeferreds[1].resolve(makePage([]));
    });

    // SSE refetch data won AND the spinner is cleared: empty label, not the loader.
    expect(await screen.findByText(t.activityEmpty)).toBeInTheDocument();
    expect(screen.queryByText(t.loading)).not.toBeInTheDocument();
    expect(screen.getByText('55')).toBeInTheDocument();
    expect(screen.queryByRole('cell', { name: 'mount-model' })).not.toBeInTheDocument();
  });

  // Test C (Defect 2 regression): off the newest view, a STATS-ONLY SSE refetch
  // must NOT drop an in-flight list load(). Pre-fix, runSseRefetch() bumped the
  // shared reqIdRef even when it fetched stats only, so a load triggered by a
  // filter/sort change had its list commit dropped by the reqIdRef guard — the
  // table kept STALE rows while the tiles updated. The guards are now split
  // (reqIdRef for the list, statsReqIdRef for stats).
  it('keeps an in-flight list load when a not-newest SSE refetch fires (stats-only)', async () => {
    const activityDeferreds: Deferred<UsagePage>[] = [];
    const statsDeferreds: Deferred<UsageStats>[] = [];
    const activity: PortalApi['activity'] = vi.fn(() => {
      const d = deferred<UsagePage>();
      activityDeferreds.push(d);
      return d.promise;
    });
    const activityStats: PortalApi['activityStats'] = vi.fn(() => {
      const d = deferred<UsageStats>();
      statsDeferreds.push(d);
      return d.promise;
    });
    let sseOnActivity: (() => void) | null = null;
    const subscribeActivity: PortalApi['subscribeActivity'] = vi.fn((onActivity: () => void) => {
      sseOnActivity = onActivity;
      return vi.fn();
    });
    const activityCalls = () =>
      (activity as unknown as { mock: { calls: unknown[][] } }).mock.calls.length;
    renderActivity({ activity, activityStats, subscribeActivity });

    // 1) Settle the mount load (newest view) to start from a clean state.
    await waitFor(() => expect(activityDeferreds).toHaveLength(1));
    await act(async () => {
      activityDeferreds[0].resolve(makePage([makeRow({ model: 'mount-model' })]));
      statsDeferreds[0].resolve(makeStats({ totals: totalsWith(1) }));
    });
    expect(await screen.findByRole('cell', { name: 'mount-model' })).toBeInTheDocument();

    // 2) Leave the newest view: sort by latency (created_at desc -> latency asc).
    fireEvent.click(screen.getByRole('button', { name: t.activityColDuration }));
    await waitFor(() => expect(activityDeferreds).toHaveLength(2));
    await act(async () => {
      activityDeferreds[1].resolve(
        makePage([makeRow({ id: 'req_sorted', model: 'sorted-model' })]),
      );
      statsDeferreds[1].resolve(makeStats({ totals: totalsWith(5) }));
    });
    expect(await screen.findByRole('cell', { name: 'sorted-model' })).toBeInTheDocument();

    // 3) Change a filter -> a new list load starts and stays IN FLIGHT.
    fireEvent.change(screen.getByLabelText(t.activitySearchLabel), {
      target: { value: 'req_filtered' },
    });
    await waitFor(() => expect(activityDeferreds).toHaveLength(3));
    const listCallsBeforeSignal = activityCalls();

    // 4) Fire ONE SSE signal WHILE the load is in flight. Off the newest view this
    // refetches STATS ONLY and increments the pill; it must NOT fetch the list nor
    // bump the list guard.
    act(() => sseOnActivity!());
    // The signal incremented the pill (the load has not committed yet to reset it).
    expect(
      await screen.findByRole('button', { name: t.activityNewRequests(1) }),
    ).toBeInTheDocument();
    // A stats fetch was issued; NO extra list fetch from the SSE refetch.
    await waitFor(() => expect(statsDeferreds).toHaveLength(4));
    expect(activityCalls()).toBe(listCallsBeforeSignal);

    // 5) The SSE stats settle first (distinctive total 99), superseding the load's stats.
    await act(async () => {
      statsDeferreds[3].resolve(makeStats({ totals: totalsWith(99) }));
    });
    expect(await screen.findByText('99')).toBeInTheDocument();

    // 6) The in-flight filter load settles LAST: its LIST commit must still apply
    //    (the bug dropped it), while its stats are ignored (SSE already superseded).
    await act(async () => {
      activityDeferreds[2].resolve(
        makePage([makeRow({ id: 'req_filtered', model: 'filtered-model' })]),
      );
      statsDeferreds[2].resolve(makeStats({ totals: totalsWith(7) }));
    });

    // List load applied (fresh rows), stale rows gone, stats reflect the SSE refetch,
    // spinner cleared, and the pill reset once the fresh list committed.
    expect(await screen.findByRole('cell', { name: 'filtered-model' })).toBeInTheDocument();
    expect(screen.queryByRole('cell', { name: 'sorted-model' })).not.toBeInTheDocument();
    expect(screen.getByText('99')).toBeInTheDocument();
    expect(screen.queryByText('7')).not.toBeInTheDocument();
    expect(screen.queryByText(t.loading)).not.toBeInTheDocument();
    expect(
      screen.queryByRole('button', { name: t.activityNewRequests(1) }),
    ).not.toBeInTheDocument();
  });

  // Test D (rollback regression, not-newest STATS half): a not-newest, stats-only
  // SSE refetch whose api.activityStats REJECTS must RELEASE the statsReqIdRef it
  // claimed at dispatch, so an in-flight load()'s stats can still commit. Pre-fix
  // the rejected refetch swallowed the error but left statsReqIdRef advanced,
  // permanently dropping the in-flight load's stats commit -> STALE tiles with the
  // spinner cleared and no error shown. Post-fix the guard is rolled back to the
  // load's id on the no-commit path, so the load's fresh stats commit.
  it("commits an in-flight load's stats after a not-newest SSE stats refetch rejects", async () => {
    const activityDeferreds: Deferred<UsagePage>[] = [];
    const statsDeferreds: Deferred<UsageStats>[] = [];
    const activity: PortalApi['activity'] = vi.fn(() => {
      const d = deferred<UsagePage>();
      activityDeferreds.push(d);
      return d.promise;
    });
    const activityStats: PortalApi['activityStats'] = vi.fn(() => {
      const d = deferred<UsageStats>();
      statsDeferreds.push(d);
      return d.promise;
    });
    let sseOnActivity: (() => void) | null = null;
    const subscribeActivity: PortalApi['subscribeActivity'] = vi.fn((onActivity: () => void) => {
      sseOnActivity = onActivity;
      return vi.fn();
    });
    const activityCalls = () =>
      (activity as unknown as { mock: { calls: unknown[][] } }).mock.calls.length;
    renderActivity({ activity, activityStats, subscribeActivity });

    // 1) Settle the mount load (newest view) for a clean start.
    await waitFor(() => expect(activityDeferreds).toHaveLength(1));
    await act(async () => {
      activityDeferreds[0].resolve(makePage([makeRow({ model: 'mount-model' })]));
      statsDeferreds[0].resolve(makeStats({ totals: totalsWith(1) }));
    });
    expect(await screen.findByRole('cell', { name: 'mount-model' })).toBeInTheDocument();

    // 2) Leave the newest view (sort by latency) so the SSE refetch is stats-only.
    //    This load commits distinctive baseline stats (total 51).
    fireEvent.click(screen.getByRole('button', { name: t.activityColDuration }));
    await waitFor(() => expect(activityDeferreds).toHaveLength(2));
    await act(async () => {
      activityDeferreds[1].resolve(
        makePage([makeRow({ id: 'req_sorted', model: 'sorted-model' })]),
      );
      statsDeferreds[1].resolve(makeStats({ totals: totalsWith(51) }));
    });
    expect(await screen.findByRole('cell', { name: 'sorted-model' })).toBeInTheDocument();
    expect(screen.getByText('51')).toBeInTheDocument();

    // 3) Change a filter -> a new list+stats load starts and stays IN FLIGHT.
    fireEvent.change(screen.getByLabelText(t.activitySearchLabel), {
      target: { value: 'req_filtered' },
    });
    await waitFor(() => expect(activityDeferreds).toHaveLength(3));
    const listCallsBeforeSignal = activityCalls();

    // 4) Fire ONE SSE signal while the load is in flight -> stats-only refetch that
    //    claims statsReqIdRef (no list fetch, no list-guard bump).
    act(() => sseOnActivity!());
    await waitFor(() => expect(statsDeferreds).toHaveLength(4));
    expect(activityCalls()).toBe(listCallsBeforeSignal);

    // 5) The SSE stats refetch REJECTS. Pre-fix this stranded statsReqIdRef advanced.
    await act(async () => {
      statsDeferreds[3].reject(new PortalApiError(500, 'request.failed', 'sse stats blew up'));
    });
    expect(screen.queryByRole('alert')).not.toBeInTheDocument(); // transient SSE error swallowed

    // 6) The in-flight filter load settles LAST: BOTH its list AND its stats must
    //    commit. Pre-fix the stats commit was dropped (statsReqIdRef stuck ahead),
    //    leaving the stale total 51; post-fix total 73 shows.
    await act(async () => {
      activityDeferreds[2].resolve(
        makePage([makeRow({ id: 'req_filtered', model: 'filtered-model' })]),
      );
      statsDeferreds[2].resolve(makeStats({ totals: totalsWith(73) }));
    });

    expect(await screen.findByRole('cell', { name: 'filtered-model' })).toBeInTheDocument();
    expect(screen.queryByRole('cell', { name: 'sorted-model' })).not.toBeInTheDocument();
    expect(screen.getByText('73')).toBeInTheDocument(); // load's fresh stats committed
    expect(screen.queryByText('51')).not.toBeInTheDocument(); // stale stats released
    expect(screen.queryByText(t.loading)).not.toBeInTheDocument();
  });

  // Test E (rollback regression, newest LIST half): a newest SSE refetch whose
  // api.activity (list) REJECTS must RELEASE the reqIdRef it claimed at dispatch,
  // so an in-flight load()'s list can still commit. Pre-fix the rejected refetch
  // swallowed the error but left reqIdRef advanced, permanently dropping the
  // in-flight load's list commit -> STALE rows. Post-fix the guard is rolled back
  // to the load's id on the no-commit path, so the load's fresh rows commit.
  it("commits an in-flight load's list after a newest SSE list refetch rejects", async () => {
    const activityDeferreds: Deferred<UsagePage>[] = [];
    const statsDeferreds: Deferred<UsageStats>[] = [];
    const activity: PortalApi['activity'] = vi.fn(() => {
      const d = deferred<UsagePage>();
      activityDeferreds.push(d);
      return d.promise;
    });
    const activityStats: PortalApi['activityStats'] = vi.fn(() => {
      const d = deferred<UsageStats>();
      statsDeferreds.push(d);
      return d.promise;
    });
    let sseOnActivity: (() => void) | null = null;
    const subscribeActivity: PortalApi['subscribeActivity'] = vi.fn((onActivity: () => void) => {
      sseOnActivity = onActivity;
      return vi.fn();
    });
    renderActivity({ activity, activityStats, subscribeActivity });

    // 1) Settle the mount load (newest view: page 1, created_at desc).
    await waitFor(() => expect(activityDeferreds).toHaveLength(1));
    await act(async () => {
      activityDeferreds[0].resolve(makePage([makeRow({ model: 'mount-model' })]));
      statsDeferreds[0].resolve(makeStats({ totals: totalsWith(1) }));
    });
    expect(await screen.findByRole('cell', { name: 'mount-model' })).toBeInTheDocument();

    // 2) Change a filter (STILL the newest view) -> a new list+stats load starts and
    //    stays IN FLIGHT (loading=true).
    fireEvent.change(screen.getByLabelText(t.activitySearchLabel), {
      target: { value: 'req_filtered' },
    });
    await waitFor(() => expect(activityDeferreds).toHaveLength(2));

    // 3) Fire ONE SSE signal while the load is in flight. On the newest view this
    //    refetches stats AND the list, claiming BOTH guards past the load.
    act(() => sseOnActivity!());
    await waitFor(() => expect(statsDeferreds).toHaveLength(3));

    // 4) The SSE stats resolve (they commit, total 88), then its LIST fetch REJECTS.
    //    Pre-fix this left reqIdRef advanced, dropping the load's list commit.
    await act(async () => {
      statsDeferreds[2].resolve(makeStats({ totals: totalsWith(88) }));
    });
    await waitFor(() => expect(activityDeferreds).toHaveLength(3));
    await act(async () => {
      activityDeferreds[2].reject(new PortalApiError(500, 'request.failed', 'sse list blew up'));
    });
    expect(screen.queryByRole('alert')).not.toBeInTheDocument(); // transient SSE error swallowed

    // 5) The in-flight filter load settles LAST: its LIST commit must still apply
    //    (pre-fix it was dropped, leaving stale mount-model rows), and loading clears.
    await act(async () => {
      activityDeferreds[1].resolve(
        makePage([makeRow({ id: 'req_filtered', model: 'filtered-model' })]),
      );
      statsDeferreds[1].resolve(makeStats({ totals: totalsWith(7) }));
    });

    expect(await screen.findByRole('cell', { name: 'filtered-model' })).toBeInTheDocument();
    expect(screen.queryByRole('cell', { name: 'mount-model' })).not.toBeInTheDocument();
    expect(screen.queryByText(t.loading)).not.toBeInTheDocument();
  });
});

describe('Activity energy chart + stat tiles (P3 T2)', () => {
  it('renders the energy chart alongside the throughput/concurrency charts', async () => {
    renderActivity();
    // The LineChart title renders even before any time-series data resolves (it
    // falls through to the no-data placeholder), so this proves the chart is wired
    // into the page rather than proving live data flowed through it.
    expect(await screen.findByText(t.activityEnergyChart)).toBeInTheDocument();
  });

  it('feeds the energy chart from the time-series energy_wh field, independently of the other charts', async () => {
    // Every OTHER time-series field (connections/concurrency/prompt/completion) is
    // zero, so the connections chart AND the token-throughput chart both fall back
    // to the "no data" placeholder (the two speed histograms are independently
    // empty by default too, for a baseline of 4); only energy_wh is non-zero, so
    // the energy chart alone must render real data. This proves the chart reads
    // `energy_wh` specifically rather than reusing one of the other fields.
    const usageTimeSeries = vi.fn(async () => ({
      points: [
        {
          t: '2026-07-10T12:00:00Z',
          connections: 0,
          concurrency: 0,
          prompt_tokens_per_second: 0,
          completion_tokens_per_second: 0,
          energy_wh: 3,
        },
        {
          t: '2026-07-10T12:00:05Z',
          connections: 0,
          concurrency: 0,
          prompt_tokens_per_second: 0,
          completion_tokens_per_second: 0,
          energy_wh: 7,
        },
      ],
      bucket_seconds: 5,
      from: '2026-07-10T12:00:00Z',
      to: '2026-07-10T12:00:10Z',
    }));
    renderActivity({ usageTimeSeries });
    await screen.findByRole('cell', { name: 'qwen-coder' });
    await waitFor(() => expect(screen.getAllByText(t.activityNoData)).toHaveLength(4));
    // The energy chart itself never falls back to the placeholder (it renders the
    // real <svg>, not the "no data" Box).
    const energyChart = screen.getByRole('img', { name: t.activityEnergyChart });
    expect(energyChart.tagName.toLowerCase()).toBe('svg');
  });

  it('shows an energy tile and a cost tile fed from the stats totals', async () => {
    const activityStats = vi.fn(async () =>
      makeStats({ totals: { ...totalsWith(1), total_energy_wh: 2500, total_cost_eur: 1.2345 } }),
    );
    renderActivity({ activityStats });
    expect(await screen.findByText(t.activityEnergyTile)).toBeInTheDocument();
    expect(screen.getByText(t.activityCostTile)).toBeInTheDocument();
    expect(screen.getByText(formatEnergyWh(2500))).toBeInTheDocument();
    // Default costUnit is "eur_cent" and getCurrency defaults to usd_per_eur: 0
    // (USD unavailable) in this suite's makeApi(), so the effective unit is
    // eur_cent regardless.
    expect(screen.getByText(formatCost(1.2345, 'eur_cent', 0))).toBeInTheDocument();
  });
});

describe('Activity cost-unit selector (currency-unit T5)', () => {
  it('renders the cost-unit selector, offering only Euro units when USD is unavailable', async () => {
    renderActivity({ getCurrency: vi.fn(async () => ({ usd_per_eur: 0 })) });
    // Wait for a full render cycle so the getCurrency() mount-effect has resolved
    // before inspecting the option set.
    await screen.findByRole('cell', { name: 'qwen-coder' });
    const combo = screen.getByRole('combobox', { name: t.activityCostUnit });
    fireEvent.mouseDown(combo);
    expect(await screen.findByRole('option', { name: t.currencyUnitEur })).toBeInTheDocument();
    expect(screen.getByRole('option', { name: t.currencyUnitEurCent })).toBeInTheDocument();
    expect(screen.queryByRole('option', { name: t.currencyUnitUsd })).not.toBeInTheDocument();
    expect(screen.queryByRole('option', { name: t.currencyUnitUsdCent })).not.toBeInTheDocument();
  });

  it('offers USD units too once the USD-per-EUR factor is available', async () => {
    renderActivity({ getCurrency: vi.fn(async () => ({ usd_per_eur: 1.1 })) });
    // Same wait-for-a-full-render-cycle rationale as above.
    await screen.findByRole('cell', { name: 'qwen-coder' });
    const combo = screen.getByRole('combobox', { name: t.activityCostUnit });
    fireEvent.mouseDown(combo);
    expect(await screen.findByRole('option', { name: t.currencyUnitUsd })).toBeInTheDocument();
    expect(screen.getByRole('option', { name: t.currencyUnitUsdCent })).toBeInTheDocument();
  });

  it('switching the unit reformats both the Usage-list cost column and the aggregate cost tile', async () => {
    const activityStats = vi.fn(async () =>
      makeStats({ totals: { ...totalsWith(1), total_cost_eur: 0.5 } }),
    );
    const activity = vi.fn(async () => makePage([makeRow({ cost_eur: 0.5 })]));
    renderActivity({
      activity,
      activityStats,
      getCurrency: vi.fn(async () => ({ usd_per_eur: 1.1 })),
    });

    await screen.findByRole('cell', { name: 'qwen-coder' });
    // The cost_eur column is hidden by default; enable it via the column menu so
    // both the table cell AND the stat tile are on screen simultaneously. Two
    // "columns" buttons exist (the running-connections ListTable has its own);
    // the ActivityTable's is the last one in DOM order.
    const columnsButtons = screen.getAllByRole('button', { name: t.listColumns });
    fireEvent.click(columnsButtons[columnsButtons.length - 1]);
    const costColumnCheckbox = await screen.findByRole('checkbox', { name: t.activityColCostEur });
    fireEvent.click(costColumnCheckbox);
    // Close the column menu (Escape must be dispatched on a node inside the
    // Menu's portal so it bubbles up to the Modal's own keydown handler; the
    // rest of the page is aria-hidden while the Modal is open, so a later
    // getByRole("combobox", ...) on the cost-unit selector would otherwise fail).
    fireEvent.keyDown(costColumnCheckbox, { key: 'Escape' });

    // Default costUnit ("eur_cent"): 0.5 EUR = 50.0000 ct, shown in BOTH the
    // Usage-list cost cell AND the aggregate cost tile.
    await waitFor(() => {
      expect(screen.getAllByText(formatCost(0.5, 'eur_cent', 1.1)).length).toBeGreaterThanOrEqual(
        2,
      );
    });

    const combo = screen.getByRole('combobox', { name: t.activityCostUnit });
    fireEvent.mouseDown(combo);
    fireEvent.click(await screen.findByRole('option', { name: t.currencyUnitUsd }));

    await waitFor(() => {
      expect(screen.getAllByText(formatCost(0.5, 'usd', 1.1)).length).toBeGreaterThanOrEqual(2);
    });
    expect(screen.queryByText(formatCost(0.5, 'eur_cent', 1.1))).not.toBeInTheDocument();
  });
});

describe('Activity collapsible time-series charts', () => {
  it('shows the charts (incl. window + resolution controls) by default and hides them after toggling', async () => {
    // No PreferencesProvider here: usePreference falls back to its default (false =
    // expanded), which is the intended fresh-user state.
    renderActivity();
    // Heading appears once stats have loaded (the whole {stats && …} block renders).
    await screen.findByText(t.activityChartsTitle);

    // Expanded by default: the window + resolution dropdowns are inside the section.
    expect(screen.queryAllByText(t.activityTsWindowLabel).length).toBeGreaterThan(0);
    expect(screen.queryAllByText(t.activityTsBucketLabel).length).toBeGreaterThan(0);
    const toggle = screen.getByLabelText(t.activityChartsToggle);
    expect(toggle).toHaveAttribute('aria-expanded', 'true');

    fireEvent.click(toggle);

    // Collapsed (unmountOnExit): the window + resolution controls are removed too,
    // not just the graphs. The header stays.
    await waitFor(() => {
      expect(screen.queryAllByText(t.activityTsWindowLabel)).toHaveLength(0);
    });
    expect(screen.queryAllByText(t.activityTsBucketLabel)).toHaveLength(0);
    expect(screen.getByLabelText(t.activityChartsToggle)).toHaveAttribute('aria-expanded', 'false');
    expect(screen.getByText(t.activityChartsTitle)).toBeInTheDocument();
  });

  it('starts collapsed when the persisted preference is collapsed', async () => {
    // Seed the profile preference via a PreferencesProvider whose GET returns the
    // collapsed flag; the provider reconciles it in, so the section renders folded.
    const { api } = makeApi({
      preferences: vi.fn(async () => ({ 'activity.chartsCollapsed': true })),
    });
    render(
      <PreferencesProvider api={api}>
        <ToastProvider>
          <Activity t={t} api={api} role="user" onUnauthorized={vi.fn()} />
        </ToastProvider>
      </PreferencesProvider>,
    );
    await screen.findByText(t.activityChartsTitle);

    await waitFor(() => {
      expect(screen.getByLabelText(t.activityChartsToggle)).toHaveAttribute(
        'aria-expanded',
        'false',
      );
    });
    expect(screen.queryAllByText(t.activityTsWindowLabel)).toHaveLength(0);
  });
});

describe('Activity group-by chain: legacy activity.groupBy seed vs. an explicit empty chain', () => {
  // Regression for a final-review finding: effectiveChain must gate the legacy
  // seed on the chain preference being UNSET (usePreference's by-reference
  // EMPTY_CHAIN default), not merely `.length === 0` -- else removing the last
  // chip (which persists activity.groupByChain = []) would fall back to the
  // legacy activity.groupBy value and resurrect grouping, making the flat table
  // permanently unreachable for any user with a legacy value stored.

  it('seeds the grouped view from the legacy activity.groupBy when activity.groupByChain is UNSET', async () => {
    const usageGroups = vi.fn(async () => ({ data: [makeGroupRow()], group_by: 'server' }));
    const { api } = makeApi({
      preferences: vi.fn(async () => ({ 'activity.groupBy': 'server' })),
      usageGroups,
    });
    render(
      <PreferencesProvider api={api}>
        <ToastProvider>
          <Activity t={t} api={api} role="user" onUnauthorized={vi.fn()} />
        </ToastProvider>
      </PreferencesProvider>,
    );

    // The grouped table (ActivityGroups) renders -- its group-only column header
    // ("Anzahl" via activityGroupCount, distinct from ActivityTable's columns).
    expect(
      await screen.findByRole('columnheader', { name: t.activityGroupCount }),
    ).toBeInTheDocument();
    await waitFor(() => expect(usageGroups).toHaveBeenCalled());
    // The flat per-request table (ActivityTable) never mounts alongside it.
    expect(screen.queryByRole('cell', { name: 'qwen-coder' })).not.toBeInTheDocument();
  });

  it('keeps grouping OFF when activity.groupByChain is explicitly stored empty, even with a legacy activity.groupBy set', async () => {
    const usageGroups = vi.fn(async () => ({ data: [makeGroupRow()], group_by: 'server' }));
    const { api } = makeApi({
      preferences: vi.fn(async () => ({
        'activity.groupBy': 'server',
        'activity.groupByChain': [],
      })),
      usageGroups,
    });
    render(
      <PreferencesProvider api={api}>
        <ToastProvider>
          <Activity t={t} api={api} role="user" onUnauthorized={vi.fn()} />
        </ToastProvider>
      </PreferencesProvider>,
    );

    // The flat per-request table (ActivityTable) renders...
    expect(await screen.findByRole('cell', { name: 'qwen-coder' })).toBeInTheDocument();
    // ...and the grouped table never mounts -- usageGroups is never called, and
    // its column header is absent.
    expect(usageGroups).not.toHaveBeenCalled();
    expect(
      screen.queryByRole('columnheader', { name: t.activityGroupCount }),
    ).not.toBeInTheDocument();
  });
});
