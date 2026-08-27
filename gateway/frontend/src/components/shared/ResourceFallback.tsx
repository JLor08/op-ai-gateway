// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { Alert, Box, Button, Typography } from '@mui/material';

/**
 * A fetched resource has THREE states, not two. `useResource` (and the
 * `useLatestFetch` underneath it) sets `error` and leaves `data` at `null` on
 * failure, so the widespread `!loading && data !== null` "ready" test collapses
 * "still loading" and "the request failed" into one indistinguishable value.
 * Every call site that gates a writable UI on that test therefore turns
 * PERMANENTLY read-only on a failed GET while still claiming to be loading --
 * with no retry and, once the toast has scrolled away, no explanation.
 *
 * `resourceState` names the third state and `ResourceFallback` renders it:
 * keep the writes disabled (that part is right -- we genuinely do not know
 * what the server allows), but say why and offer a way out.
 */
export type ResourceState = 'loading' | 'error' | 'ready';

export function resourceState(resource: {
  loading: boolean;
  error: string;
  data: unknown;
}): ResourceState {
  if (resource.loading) return 'loading';
  if (resource.data !== null) return 'ready';
  // `data === null` and no error yet is the pre-first-fetch ('idle') window,
  // which is a loading state, not a failure.
  return resource.error !== '' ? 'error' : 'loading';
}

/**
 * Renders the not-ready half of a resource: the plain "loading…" line every
 * call site already had, or -- new -- an explicit failure with an optional
 * retry action. Renders nothing when the resource is ready.
 *
 * `errorDetail` is meant for the resource's own formatted error message; the
 * `errorLabel` carries what the failure MEANS for this screen, which the raw
 * message never does.
 */
export function ResourceFallback({
  state,
  loadingLabel,
  errorLabel,
  errorDetail,
  retryLabel,
  onRetry,
}: Readonly<{
  state: ResourceState;
  loadingLabel: string;
  errorLabel: string;
  errorDetail?: string;
  retryLabel?: string;
  onRetry?: () => void;
}>) {
  if (state === 'ready') return null;
  if (state === 'loading') {
    return <Typography color="text.secondary">{loadingLabel}</Typography>;
  }
  return (
    <Alert
      severity="warning"
      action={
        onRetry && retryLabel ? (
          <Button color="inherit" size="small" onClick={onRetry}>
            {retryLabel}
          </Button>
        ) : undefined
      }
    >
      <Box sx={{ display: 'grid', gap: 0.25 }}>
        <span>{errorLabel}</span>
        {errorDetail ? <Typography variant="caption">{errorDetail}</Typography> : null}
      </Box>
    </Alert>
  );
}
