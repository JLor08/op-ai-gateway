// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useEffect, useRef } from 'react';
import { useLatestFetch } from './useLatestFetch';

/**
 * usePolledFetch layers periodic polling on useLatestFetch: the deps-keyed
 * load stays latest-wins + unmount-safe, and on top of it a `live` flag drives
 * an interval poll. Mirrors PerformanceSection's token-throughput poll and
 * AvailabilitySection's history poll: while `live`, `loader` re-runs every
 * `intervalMs`, AND immediately the instant `live` flips from paused to
 * resumed (so a resumed chart never sits on a stale value for up to
 * `intervalMs`). Pausing (`live: false`) clears the interval and freezes the
 * last committed data -- no fetch, no interval, until resumed.
 */
export function usePolledFetch<T>(
  loader: () => Promise<T>,
  deps: readonly unknown[],
  options: { intervalMs: number; live: boolean },
) {
  const { intervalMs, live } = options;
  const fetch = useLatestFetch(loader, deps);
  const { reload } = fetch;

  const liveRef = useRef(live);
  liveRef.current = live;
  const wasLiveRef = useRef(live);

  // Resume: an explicit paused -> live transition refetches immediately
  // (the initial mount does NOT double-fetch: useLatestFetch's own mount
  // effect already covers it, and wasLiveRef starts equal to `live`).
  useEffect(() => {
    if (live && !wasLiveRef.current) {
      void reload();
    }
    wasLiveRef.current = live;
  }, [live, reload]);

  // Poll on an interval while live; cleared on pause or unmount.
  useEffect(() => {
    if (!live) return;
    const id = window.setInterval(() => {
      if (liveRef.current) void reload();
    }, intervalMs);
    return () => window.clearInterval(id);
  }, [live, intervalMs, reload]);

  return fetch;
}
