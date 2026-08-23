// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { createContext, useContext, useEffect, useState, type ReactNode } from 'react';
import { Backdrop, Box, CircularProgress, Paper, Typography } from '@mui/material';
import type { PortalApi, Translation } from './types';

export type ConnectionStatus = 'online' | 'offline';

const ConnectionContext = createContext<ConnectionStatus>('online');

/** Current backend-connection status ("online" | "offline"). */
export function useConnectionStatus(): ConnectionStatus {
  return useContext(ConnectionContext);
}

// Base liveness cadence: probe /healthz on this interval. This is the RELIABLE
// signal — an already-open SSE can go half-open (backend gone but the proxy keeps
// the stream open), and EventSource has no heartbeat *timeout*, so `onerror`
// alone can miss a drop. Active polling catches it regardless; the SSE error is
// only a fast accelerator on top.
const POLL_MS = 4000;

/**
 * Holds a single always-on SSE connection (the same `/api/portal/usage/events`
 * stream the Activity view uses) purely to observe backend reachability, and
 * renders a full-screen blocking overlay whenever the backend is unreachable.
 *
 * Flow: the SSE errors → after a short debounce we confirm with a public
 * `/healthz` probe (so an auth/transient blip does not lock the UI) → if the
 * probe fails we lock and keep re-probing until it succeeds; the SSE's own
 * exponential-backoff reconnect (or a successful probe) clears the lock. Mounted
 * only inside the authenticated shell, so the stream opens only when signed in.
 */
export function ConnectionProvider({
  api,
  t,
  children,
}: Readonly<{
  api: Pick<PortalApi, 'checkHealth' | 'subscribeActivity'>;
  t: Translation;
  children: ReactNode;
}>) {
  const [status, setStatus] = useState<ConnectionStatus>('online');

  useEffect(() => {
    let cancelled = false;
    // Consecutive /healthz failures. A single failed poll may be a transient blip,
    // so a *soft* probe locks only after two in a row; a *hard* probe (triggered by
    // the SSE actually erroring — a corroborating signal) locks on the first fail.
    let fails = 0;

    const online = () => {
      if (cancelled) return;
      fails = 0;
      setStatus('online');
    };

    const probe = (hard: boolean) => {
      void api.checkHealth().then((ok) => {
        if (cancelled) return;
        if (ok) {
          online();
        } else {
          fails += 1;
          if (hard || fails >= 2) setStatus('offline');
        }
      });
    };

    // Reliable base signal: poll on an interval regardless of SSE state.
    const poll = setInterval(() => probe(false), POLL_MS);

    // SSE accelerator: a re-open (or reconnect) clears the lock instantly; an error
    // triggers an immediate confirming probe that locks on a single failure.
    const unsubscribe = api.subscribeActivity(
      () => {},
      () => online(),
      (sseStatus) => {
        if (sseStatus === 'open') online();
        else probe(true);
      },
    );

    return () => {
      cancelled = true;
      clearInterval(poll);
      unsubscribe();
    };
  }, [api]);

  return (
    <ConnectionContext.Provider value={status}>
      {children}
      <Backdrop
        open={status === 'offline'}
        data-testid="connection-lost"
        sx={{
          zIndex: (theme) => theme.zIndex.modal + 10,
          bgcolor: 'rgba(0, 0, 0, 0.6)',
          backdropFilter: 'blur(2px)',
        }}
      >
        <Paper
          role="alertdialog"
          aria-modal="true"
          aria-labelledby="connection-lost-title"
          elevation={6}
          sx={{
            maxWidth: 420,
            mx: 2,
            p: 3,
            display: 'flex',
            alignItems: 'center',
            gap: 2,
            bgcolor: 'var(--surface)',
            color: 'var(--text)',
            border: '1px solid var(--line)',
            borderRadius: '12px',
          }}
        >
          <CircularProgress size={28} sx={{ color: 'var(--brand-primary)', flexShrink: 0 }} />
          <Box>
            <Typography
              id="connection-lost-title"
              component="h2"
              sx={{ fontWeight: 700, fontSize: 16 }}
            >
              {t.connectionLostTitle}
            </Typography>
            <Typography sx={{ color: 'var(--muted)', fontSize: 14, mt: 0.5 }}>
              {t.connectionLostBody}
            </Typography>
          </Box>
        </Paper>
      </Backdrop>
    </ConnectionContext.Provider>
  );
}
