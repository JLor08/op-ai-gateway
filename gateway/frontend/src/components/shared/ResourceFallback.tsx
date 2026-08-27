// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { Alert, Box, Button, Typography } from '@mui/material';

/**
 * A fetched resource has FOUR states, not two. `useResource` (and the
 * `useLatestFetch` underneath it) sets `error` and never touches `data` on
 * failure, so the widespread `!loading && data !== null` "ready" test collapses
 * distinct facts into one value:
 *
 *  - `loading` — in flight, or not fetched yet at all;
 *  - `error` — the fetch failed and there is NOTHING on screen. Every call site
 *    that gates a writable UI on the two-state test turns PERMANENTLY read-only
 *    here while still claiming to be loading, with no retry and, once the toast
 *    has scrolled away, no explanation;
 *  - `stale-error` — a reload failed but the PREVIOUS payload is still held.
 *    The two-state test reports this as `ready` and the error is invisible, so
 *    the screen silently shows values that may no longer be true. Call sites
 *    with a `reload()` on a resource that already holds data (and any call site
 *    whose `useResource` deps change — a server switch re-runs the loader while
 *    `data` keeps the OLD server's payload) must handle this one: keep
 *    rendering the data, say it is the last known state, and offer the retry;
 *  - `ready`.
 *
 * `resourceState` names them and `ResourceFallback` renders the three
 * not-ready ones. For a hard `error` the right move is to keep the writes
 * disabled (we genuinely do not know what the server allows) but say why and
 * offer a way out.
 *
 * ## `data` must never be a legitimate `null`
 *
 * `data === null` is this function's only signal for "no payload", so a loader
 * that legitimately RESOLVES `null` never reaches `ready` — it looks like the
 * pre-first-fetch window forever. TypeScript cannot catch that here: at the
 * `data: T | null` position `T = Foo | null` collapses to `Foo | null`, i.e. it
 * is indistinguishable from the correct `T = Foo` (verified with tsc; a
 * `T extends NonNullable<unknown>` bound rejects only `undefined`, so it would
 * read as a guarantee it does not give). A call site whose payload is genuinely
 * nullable must therefore wrap it — resolve `{ value: T | null }` — rather than
 * hand it in raw.
 */
export type ResourceState = 'loading' | 'error' | 'stale-error' | 'ready';

export function resourceState(resource: {
  loading: boolean;
  error: string;
  data: unknown;
}): ResourceState {
  if (resource.loading) return 'loading';
  // The error is tested BEFORE the payload, deliberately. The other order
  // reports "loaded once, then a refresh failed" as `ready` and drops the
  // error on the floor.
  if (resource.error !== '') return resource.data !== null ? 'stale-error' : 'error';
  // `data === null` and no error is the pre-first-fetch ('idle') window, which
  // is a loading state, not a failure.
  return resource.data !== null ? 'ready' : 'loading';
}

/**
 * Renders the not-ready half of a resource: the plain "loading…" line every
 * call site already had, or an explicit failure with an optional retry action.
 * Renders nothing when the resource is ready.
 *
 * `errorDetail` is meant for the resource's own formatted error message; the
 * `errorLabel` carries what the failure MEANS for this screen, which the raw
 * message never does.
 *
 * On `stale-error` this renders `staleErrorLabel` (falling back to
 * `errorLabel`) and NOTHING ELSE changes — the data is still the call site's to
 * render, above or below this banner. That is the whole point of the state:
 * the last known values stay on screen, labelled as such.
 *
 * `loadingLabel` is required even at a call site that statically knows `state`
 * is never `'loading'`. Deliberate: making it optional would trade one unused
 * prop for a silently blank loading state the day that call site starts passing
 * a computed `state`, which is the direction every call site here moves in.
 */
export function ResourceFallback({
  state,
  loadingLabel,
  errorLabel,
  staleErrorLabel,
  errorDetail,
  retry,
  severity = 'warning',
}: Readonly<{
  state: ResourceState;
  loadingLabel: string;
  errorLabel: string;
  staleErrorLabel?: string;
  errorDetail?: string;
  /**
   * The retry action. A single object rather than a `retryLabel`/`onRetry`
   * pair: half of a pair made the button vanish silently, with nothing to
   * catch it.
   */
  retry?: { label: string; onRetry: () => void };
  /**
   * How loud the failure is. `warning` by default; a call site whose failure
   * gates whether a whole screen may be written to and one whose failure only
   * staled a read-only chart are not the same event.
   */
  severity?: 'info' | 'warning' | 'error';
}>) {
  if (state === 'ready') return null;
  if (state === 'loading') {
    return <Typography color="text.secondary">{loadingLabel}</Typography>;
  }
  return (
    <Alert
      severity={severity}
      action={
        retry ? (
          <Button color="inherit" size="small" onClick={retry.onRetry}>
            {retry.label}
          </Button>
        ) : undefined
      }
    >
      <Box sx={{ display: 'grid', gap: 0.25 }}>
        <span>{state === 'stale-error' ? (staleErrorLabel ?? errorLabel) : errorLabel}</span>
        {errorDetail ? <Typography variant="caption">{errorDetail}</Typography> : null}
      </Box>
    </Alert>
  );
}
