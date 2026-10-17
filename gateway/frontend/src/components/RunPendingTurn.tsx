// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useEffect, useReducer, useRef } from 'react';
import { Box, Typography } from '@mui/material';
import { formatElapsed } from './shared/elapsed';

// The wait state shared by every run kind that answers ONCE, with no incremental
// events: an image run (ImagePendingTurn) and a speech run (AudioPendingTurn). The
// per-kind rationale (why a progress bar or the text-pending counter would be
// dishonest) lives in those two files; this is the mechanics they have in common, kept
// in one place so the clock cannot drift apart between them.
//
// It renders exactly three things, each backed by a value that exists: liveness (the
// caller only mounts it while the run's own server-reported status is "running" -- see
// ChatMessage's guard), an elapsed clock anchored on the server's own measurement, and
// one static sentence stating that no intermediate news is coming.
export function RunPendingTurn({
  // The run's server-measured age in ms, AS OF THE LAST SNAPSHOT the caller
  // received (useChatRuns' elapsedMsOf) -- an anchor, not a live value. Every
  // time this prop changes we re-anchor on a fresh performance.now() and tick
  // the DISPLAY locally in between (see the anchorRef effect below), so a
  // reopened tab and the sending tab -- which each learn the same server
  // value at a different real-world instant -- end up rendering the same
  // true elapsed time. A client clock computed from the local moment of Send
  // would be wrong in any tab that never witnessed that moment.
  elapsedMs,
  roleLabel,
  waitLabel,
  noNewsLabel,
}: Readonly<{
  elapsedMs: number;
  roleLabel: string;
  // What the run is waiting for ("Waiting for image" / "Waiting for audio").
  waitLabel: string;
  // The static sentence stating that no intermediate news is coming.
  noNewsLabel: string;
}>) {
  // Anchor once per distinct server value: (server ms, the performance.now()
  // instant it was read). Re-anchored only when `elapsedMs` itself changes
  // (a fresh snapshot), never on every render -- otherwise the interpolation
  // below would forever compute a near-zero gap and the clock would never
  // advance between snapshots.
  const anchorRef = useRef({ serverMs: elapsedMs, at: performance.now() });
  const lastElapsedMsRef = useRef(elapsedMs);
  if (lastElapsedMsRef.current !== elapsedMs) {
    lastElapsedMsRef.current = elapsedMs;
    anchorRef.current = { serverMs: elapsedMs, at: performance.now() };
  }

  // Force a re-render once a second so the clock ticks even when NOTHING else
  // about this turn's props changes between snapshots -- which, for a run that
  // answers once, is the common case (no delta to re-render on). Mirrors
  // ActiveRequestsPanel's own 1s ticker.
  const [, tick] = useReducer((n: number) => n + 1, 0);
  useEffect(() => {
    const id = window.setInterval(tick, 1000);
    return () => window.clearInterval(id);
  }, []);

  const displayMs = anchorRef.current.serverMs + (performance.now() - anchorRef.current.at);

  return (
    <Box
      component="article"
      data-role="assistant"
      sx={{
        width: 'fit-content',
        minWidth: 'min(760px, 100%)',
        maxWidth: '90%',
        mb: 2,
        ml: 'auto',
        p: '14px 16px',
        bgcolor: 'var(--surface)',
        borderRight: '4px solid var(--brand-primary)',
        borderRadius: '10px',
      }}
    >
      <Typography
        component="div"
        sx={{ fontSize: 13, textTransform: 'uppercase', fontWeight: 700 }}
      >
        {roleLabel}
      </Typography>
      <Typography component="div" sx={{ mt: 1.25, color: 'var(--text)' }}>
        {waitLabel}
      </Typography>
      {/* aria-hidden: the transcript is role="log" aria-live="polite"
          aria-relevant="additions text" (Chat.tsx), so an unhidden
          once-per-second tick would be announced every second and make the
          thread unusable with a screen reader. The label above and the
          sentence below ARE announced -- only this number is not. */}
      <Box component="span" aria-hidden="true" sx={{ color: 'var(--muted)', fontSize: 13 }}>
        {formatElapsed(displayMs)}
      </Box>
      <Typography component="div" sx={{ mt: 0.5, color: 'var(--muted)', fontSize: 13 }}>
        {noNewsLabel}
      </Typography>
    </Box>
  );
}
