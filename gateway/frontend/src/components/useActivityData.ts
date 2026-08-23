// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type Dispatch,
  type SetStateAction,
} from 'react';
import {
  PortalApiError,
  type ActiveRequest,
  type ActivityQuery,
  type TimeSeries,
  type UsagePage,
  type UsageStats,
} from '../api';
import type { PortalApi, Translation } from './shared/types';
import { formatPortalError } from './shared/format';
import type { TsBucket, TsWindow } from './activityColumns';

const SSE_THROTTLE_MS = 1000;

export type UseActivityDataArgs = {
  api: Pick<
    PortalApi,
    'activeRequests' | 'activity' | 'activityStats' | 'subscribeActivity' | 'usageTimeSeries'
  >;
  // The fully-assembled query (filters/sort/paging/scope/range). Refetches on
  // every identity change, exactly like the container's own effect did before
  // the extraction.
  query: ActivityQuery;
  // Whether the current view is page 1, sorted by created_at desc -- only that
  // view's list refetches on an SSE signal (see runSseRefetch below).
  newest: boolean;
  tsWindow: TsWindow;
  tsBucket: TsBucket;
  t: Translation;
  onUnauthorized: () => void;
};

export type UseActivityDataResult = {
  pageData: UsagePage | null;
  stats: UsageStats | null;
  // Best-effort like the stats/active fetches: an error keeps the previous
  // series and never fails the page.
  timeSeries: TimeSeries | null;
  // In-flight (running) requests: best-effort, never fails the page (see loadActive).
  active: ActiveRequest[];
  loading: boolean;
  error: string;
  newCount: number;
  // Exposed so a caller can optimistically zero the "N new requests" pill the
  // instant it is acknowledged, without waiting for the refetch to resolve
  // (mirrors the original inline onClick's synchronous setNewCount(0)).
  setNewCount: Dispatch<SetStateAction<number>>;
  // Re-runs load() against the CURRENT `query`. Used for the manual refresh
  // action and after a capture mutation (delete / secret toggle).
  refresh: (opts?: { silent?: boolean }) => Promise<void>;
};

// Owns the whole Activity data-orchestration layer: the list/stats/active/
// time-series fetches, their monotonic request-id guards, the SSE-driven
// refetch (with its asymmetric guard bump-and-release), and the leading-edge
// SSE throttle. Moved verbatim out of Activity.tsx -- the logic is untouched,
// only the closure over props/inputs changed (they arrive as hook arguments
// instead of component-scope variables).
export function useActivityData({
  api,
  query,
  newest,
  tsWindow,
  tsBucket,
  t,
  onUnauthorized,
}: UseActivityDataArgs): UseActivityDataResult {
  const [pageData, setPageData] = useState<UsagePage | null>(null);
  const [stats, setStats] = useState<UsageStats | null>(null);
  const [timeSeries, setTimeSeries] = useState<TimeSeries | null>(null);
  const [active, setActive] = useState<ActiveRequest[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [newCount, setNewCount] = useState(0);

  const onUnauthorizedRef = useRef(onUnauthorized);
  onUnauthorizedRef.current = onUnauthorized;
  const tRef = useRef(t);
  tRef.current = t;

  // TWO monotonic guards, because the LIST and the STATS have different lifetimes:
  //  - reqIdRef guards the list commit (setPageData) AND the loading spinner.
  //  - statsReqIdRef guards the stats commit (setStats).
  // Each fetch captures the next id up front and only commits if it is still the
  // latest when it resolves. This drops out-of-order responses from un-debounced
  // filter typing and stops a refetch from clobbering a newer load (or vice versa).
  //
  // Splitting the guards is what lets a NOT-newest SSE refetch update stats WITHOUT
  // dropping an in-flight load()'s list commit: it bumps only statsReqIdRef, so the
  // load's list still commits against its own (still-latest) reqIdRef.
  const reqIdRef = useRef(0);
  const statsReqIdRef = useRef(0);
  // Own monotonic guard for the best-effort active-requests fetch (analogous to
  // statsReqIdRef): only the latest fetch commits, so an out-of-order response
  // never clobbers a newer one.
  const activeReqIdRef = useRef(0);
  // Own monotonic guard for the best-effort time-series fetch (like activeReqIdRef):
  // only the latest fetch commits, so an out-of-order response never clobbers a newer one.
  const tsReqIdRef = useRef(0);
  // Current window/bucket read from refs so load()/runSseRefetch() can fetch the
  // latest series without adding these to their useCallback deps (which would
  // otherwise rebuild load() and retrigger the list/stats load on every toggle).
  const tsWindowRef = useRef(tsWindow);
  tsWindowRef.current = tsWindow;
  const tsBucketRef = useRef(tsBucket);
  tsBucketRef.current = tsBucket;

  // Best-effort fetch of the activity time-series (charts). Errors keep the
  // previous series and never surface an alert — the charts must not fail the page.
  const loadTimeSeries = useCallback(
    async (
      window: TsWindow,
      bucket: TsBucket,
      scope: 'own' | 'all' | 'user',
      userId?: string,
      tokenId?: string,
    ) => {
      const myId = ++tsReqIdRef.current;
      try {
        const resp = await api.usageTimeSeries({
          window,
          bucket,
          scope,
          user_id: userId,
          token_id: tokenId,
        });
        if (tsReqIdRef.current !== myId) return;
        setTimeSeries(resp);
      } catch {
        /* best-effort: keep the previous series, never fail the page */
      }
    },
    [api],
  );

  // Best-effort fetch of the running-connections list. On error we keep the
  // previous list and never surface an alert — the panel must not fail the page
  // (mirrors the has_capture enrichment policy).
  const loadActive = useCallback(
    async (activeScope: 'own' | 'all' | 'user', userId?: string, tokenId?: string) => {
      const myId = ++activeReqIdRef.current;
      try {
        const resp = await api.activeRequests(activeScope, { user_id: userId, token_id: tokenId });
        if (activeReqIdRef.current !== myId) return;
        setActive(resp.data);
      } catch {
        /* best-effort: keep the previous list, never fail the page */
      }
    },
    [api],
  );

  const load = useCallback(
    async (activeQuery: ActivityQuery, opts?: { silent?: boolean }) => {
      const myListId = ++reqIdRef.current;
      const myStatsId = ++statsReqIdRef.current;
      if (!opts?.silent) setLoading(true);
      setError('');
      // Parallel best-effort: never blocks or fails the list/stats load.
      void loadActive(activeQuery.scope ?? 'own', activeQuery.user_id, activeQuery.token_id);
      void loadTimeSeries(
        tsWindowRef.current,
        tsBucketRef.current,
        activeQuery.scope ?? 'own',
        activeQuery.user_id,
        activeQuery.token_id,
      );
      try {
        const [listResp, statsResp] = await Promise.all([
          api.activity(activeQuery),
          api.activityStats(activeQuery),
        ]);
        // Commit each half against its own guard; a later refetch may have
        // superseded one but not the other.
        if (statsReqIdRef.current === myStatsId) setStats(statsResp);
        if (reqIdRef.current !== myListId) return; // list superseded by a newer load/refetch
        setPageData(listResp);
        setNewCount(0);
      } catch (err) {
        if (reqIdRef.current !== myListId) return; // stale error; a newer load owns the view
        if (err instanceof PortalApiError && err.status === 401) {
          onUnauthorizedRef.current();
          return;
        }
        setError(
          err instanceof PortalApiError ? err.message : formatPortalError(err, tRef.current),
        );
      } finally {
        if (reqIdRef.current === myListId) setLoading(false);
      }
    },
    [api, loadActive, loadTimeSeries],
  );

  // (Re)load on mount and on every param change.
  useEffect(() => {
    void load(query);
  }, [load, query]);

  const newestRef = useRef(newest);
  newestRef.current = newest;
  const queryRef = useRef(query);
  queryRef.current = query;

  // SSE-driven refetch: stats always (window-wide, page-independent); list only
  // when the newest view is showing. Failures are transient -> no toast.
  //
  // Guard bumping is asymmetric by design (see reqIdRef/statsReqIdRef above):
  //  - Stats always refetch, so ALWAYS bump statsReqIdRef and guard setStats on it
  //    (the latest stats fetch wins over any in-flight load's stats).
  //  - Only the NEWEST view refetches the list, so ONLY the newest path bumps
  //    reqIdRef (superseding an in-flight load's list) and owns the spinner. Off the
  //    newest view we touch NEITHER reqIdRef NOR loading: an in-flight load()'s list
  //    commit and spinner-clear survive, while stats still update and the pill counts.
  const runSseRefetch = useCallback(async () => {
    const activeQuery = queryRef.current;
    const isNewest = newestRef.current;
    // Always refresh the running connections (start AND end pokes fire this).
    void loadActive(activeQuery.scope ?? 'own', activeQuery.user_id, activeQuery.token_id);
    // Always refresh the time-series charts (they are a live, window-wide overview).
    void loadTimeSeries(
      tsWindowRef.current,
      tsBucketRef.current,
      activeQuery.scope ?? 'own',
      activeQuery.user_id,
      activeQuery.token_id,
    );
    const myStatsId = ++statsReqIdRef.current;
    const myListId = isNewest ? ++reqIdRef.current : reqIdRef.current;
    // Track whether each half actually committed. On the failure/no-commit path we
    // must RELEASE any guard we advanced but never committed against; otherwise an
    // earlier load() still in flight can NEVER commit that half (its guard id stays
    // behind ours forever) -> stale tiles (not-newest stats reject) or stale rows
    // (newest list reject) with loading cleared and no error shown.
    let statsCommitted = false;
    let listCommitted = false;
    try {
      const statsResp = await api.activityStats(activeQuery);
      if (statsReqIdRef.current === myStatsId) {
        setStats(statsResp);
        statsCommitted = true;
      }
      if (isNewest) {
        const listResp = await api.activity(activeQuery);
        if (reqIdRef.current !== myListId) return; // superseded by a newer load/refetch
        setPageData(listResp);
        setNewCount(0);
        listCommitted = true;
      }
    } catch {
      /* transient SSE refetch error; nav stays put */
    } finally {
      // Release a guard we advanced but did NOT commit against, so a still-in-flight
      // earlier load() can commit its half. Pre-increment is sequential+synchronous at
      // dispatch, so `myId - 1` is exactly the immediately-preceding claimant (that
      // load). The `=== myId` check means we only release while STILL the latest
      // claimant; if a newer op already bumped past us we leave it (newest wins). We
      // never roll back a half we committed — that would let an older load overwrite
      // our fresher data.
      if (!statsCommitted && statsReqIdRef.current === myStatsId) {
        statsReqIdRef.current = myStatsId - 1;
      }
      if (isNewest && !listCommitted && reqIdRef.current === myListId) {
        reqIdRef.current = myListId - 1;
      }
      // Only the newest path owns the spinner (it bumped reqIdRef): it clears a
      // spinner that a now-superseded in-flight load() can no longer clear itself
      // (that load's finally is a no-op once its reqIdRef id is stale). The
      // not-newest path never touches loading — it belongs to the live load(). On a
      // rejected newest refetch we just rolled reqIdRef back to the load's id, so this
      // is a no-op and that load's own finally clears loading (never stranded, never
      // double-cleared).
      if (isNewest && reqIdRef.current === myListId) setLoading(false);
    }
  }, [api, loadActive, loadTimeSeries]);
  const runSseRefetchRef = useRef(runSseRefetch);
  runSseRefetchRef.current = runSseRefetch;

  // Leading-edge throttle (~1/s): the first signal fires immediately, further
  // signals within the window coalesce into one trailing refetch. The counter
  // is incremented per signal (before the throttle) per the spec.
  const throttle = useRef<{ last: number; timer: number | null }>({ last: 0, timer: null });
  const onSignal = useCallback(() => {
    setNewCount((n) => n + 1);
    const now = Date.now();
    const since = now - throttle.current.last;
    if (since >= SSE_THROTTLE_MS) {
      throttle.current.last = now;
      void runSseRefetchRef.current();
    } else {
      throttle.current.timer ??= window.setTimeout(() => {
        throttle.current.timer = null;
        throttle.current.last = Date.now();
        void runSseRefetchRef.current();
      }, SSE_THROTTLE_MS - since);
    }
  }, []);

  // On reconnect after a dropped SSE stream, resync: run the same refetch the tick
  // uses (stats always + list only on the newest view) and reset the pill to 0 — a
  // gap may have dropped signals, so the counter is cleared, never incremented here.
  const onReconnect = useCallback(() => {
    setNewCount(0);
    void runSseRefetchRef.current();
  }, []);

  useEffect(() => {
    const unsubscribe = api.subscribeActivity(
      () => onSignal(),
      () => onReconnect(),
    );
    return () => {
      unsubscribe();
      if (throttle.current.timer !== null) {
        window.clearTimeout(throttle.current.timer);
        throttle.current.timer = null;
      }
    };
  }, [api, onSignal, onReconnect]);

  // Refetch the time-series whenever the shared window/resolution or the effective
  // scope/user/token filter changes (best-effort; the monotonic guard drops stale
  // responses). load()/runSseRefetch() also refresh it on their own triggers.
  useEffect(() => {
    void loadTimeSeries(tsWindow, tsBucket, query.scope ?? 'own', query.user_id, query.token_id);
  }, [loadTimeSeries, tsWindow, tsBucket, query.scope, query.user_id, query.token_id]);

  const refresh = useCallback((opts?: { silent?: boolean }) => load(query, opts), [load, query]);

  return { pageData, stats, timeSeries, active, loading, error, newCount, setNewCount, refresh };
}
