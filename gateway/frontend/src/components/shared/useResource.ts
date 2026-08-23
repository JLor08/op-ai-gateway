// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useRef, useState } from 'react';
import { formatPortalError } from './format';
import type { Translation } from './types';
import { useLatestFetch, type FetchStatus } from './useLatestFetch';

// Stable by-reference default so a call site that omits `options` doesn't
// hand the hook a fresh object every render.
const DEFAULT_OPTIONS: { trackLoading?: boolean } = {};

/**
 * useResource layers i18n error formatting and an independently-settable
 * `data`/`error` pair on top of useLatestFetch, which now supplies the
 * latest-wins + unmount-safe guard for all 16 of this hook's call sites for
 * free. `data`/`setData` and `error`/`setError` stay plain state (several call
 * sites mutate `data` directly for an optimistic local update decoupled from
 * the next fetch -- see GroupsView.tsx) that this hook re-seeds whenever a
 * fetch settles.
 */
export function useResource<T>(
  loader: () => Promise<T>,
  deps: unknown[],
  t: Translation,
  options: { trackLoading?: boolean } = DEFAULT_OPTIONS,
) {
  const trackLoading = options.trackLoading ?? true;
  const fetch = useLatestFetch(loader, deps);

  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState('');

  // Mirror fetch's settled result into `data`/`error` the SAME render it
  // changes -- the React-endorsed "adjust state while rendering" pattern
  // (a guarded setState call during render, not inside a useEffect) -- so a
  // caller relying on the fetch's result being visible as soon as it settles
  // (many call sites read `data` right after their first render commits)
  // sees it without an extra effect-flush round-trip. useEffect would have
  // worked too, but adds one more render pass than the previous
  // implementation had, and several tests assert on that pre-existing
  // timing.
  const seenRef = useRef<{ status: FetchStatus; data: T | null; error: unknown }>({
    status: 'idle',
    data: null,
    error: null,
  });
  if (
    seenRef.current.status !== fetch.status ||
    seenRef.current.data !== fetch.data ||
    seenRef.current.error !== fetch.error
  ) {
    seenRef.current = { status: fetch.status, data: fetch.data, error: fetch.error };
    if (fetch.status === 'loading') {
      setError('');
    } else if (fetch.status === 'ok') {
      setData(fetch.data);
    } else if (fetch.status === 'error') {
      setError(formatPortalError(fetch.error, t));
    }
  }

  const loading = trackLoading && fetch.status === 'loading';

  return { data, setData, loading, error, setError, reload: fetch.reload };
}
