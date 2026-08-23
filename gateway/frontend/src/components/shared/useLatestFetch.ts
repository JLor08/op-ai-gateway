// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useCallback, useEffect, useRef, useState } from 'react';

// 'idle' is the pre-mount value only -- the hook always kicks off its first
// load from a mount effect, so callers mostly see 'loading' -> 'ok'|'error'.
export type FetchStatus = 'idle' | 'loading' | 'ok' | 'error';

// Stable by-reference default: a call site that doesn't need extra deps (a
// fetch keyed on nothing) shouldn't hand useEffect a fresh array every render.
const EMPTY_DEPS: readonly unknown[] = [];

/**
 * useLatestFetch centralizes the fetch-lifecycle guard hand-rolled across ~18
 * leaf components in four divergent dialects (cancelled-flag / monotonic
 * reqId+unmount-bump / reqId+live-flag+poll-interval / refs-mirroring-state
 * for SSE). Modeled on HardwareSection's / ServerResourceGroupsSection's reqId
 * pattern, it gives every caller two guarantees for free:
 *
 *  - latest-wins: a monotonic request-id token means a slow OLDER response
 *    can never overwrite state already committed by a newer one -- even
 *    across a `deps` change or an explicit `reload()` racing the in-flight
 *    load it superseded.
 *  - unmount invalidation: a dedicated cleanup-only effect bumps the same
 *    token on unmount, so a response resolving after unmount is a no-op
 *    (no setState-after-unmount).
 *
 * `loader` runs once whenever `deps` changes (compared like a useEffect
 * dependency list) and once per explicit `reload()` call; `reload` always
 * invokes the CURRENT `loader` closure, even called from a stale handler.
 */
export function useLatestFetch<T>(loader: () => Promise<T>, deps: readonly unknown[] = EMPTY_DEPS) {
  const [data, setData] = useState<T | null>(null);
  const [status, setStatus] = useState<FetchStatus>('idle');
  const [error, setError] = useState<unknown>(null);
  const reqIdRef = useRef(0);

  const reload = useCallback(() => {
    const reqId = ++reqIdRef.current;
    setStatus('loading');
    setError(null);
    return loader()
      .then((result) => {
        if (reqId !== reqIdRef.current) return; // superseded or unmounted
        setData(result);
        setStatus('ok');
      })
      .catch((err: unknown) => {
        if (reqId !== reqIdRef.current) return; // superseded or unmounted
        setError(err);
        setStatus('error');
      });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps);

  useEffect(() => {
    void reload();
  }, [reload]);

  // Invalidate any in-flight request on unmount. Kept as its OWN `[]`-deps
  // effect (rather than a cleanup returned from the effect above) so it reads
  // reqIdRef.current only in a cleanup whose effect has no other dependencies
  // -- the shape that does NOT trip react-hooks/exhaustive-deps' "ref value
  // will likely have changed by the time this cleanup runs" warning.
  useEffect(
    () => () => {
      ++reqIdRef.current;
    },
    [],
  );

  return { data, status, error, reload };
}
