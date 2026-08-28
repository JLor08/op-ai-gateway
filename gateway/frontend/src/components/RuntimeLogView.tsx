// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useEffect, useRef, useState } from 'react';
import {
  Alert,
  Box,
  Button,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  Typography,
} from '@mui/material';
import type { RuntimeLogBatch, RuntimeLogEntry, RuntimeLogState } from '../api';
import type { PortalApi, Translation } from './shared/types';

/**
 * The live view of ONE managed model process's stdout+stderr, opened from a
 * row of the live-status tab. It is where an operator ends up when a row says
 * `crashed`, or when a model will not finish loading, and the question is
 * always the same: what is the process actually printing?
 *
 * Three properties shape everything here, and each of them is a rule about not
 * lying to the reader:
 *
 *  1. **Opening this view is what makes the agent stream.** The subscription
 *     is the request; the unsubscribe is the stop. So the effect's cleanup is
 *     not housekeeping -- it is what keeps an unwatched fleet quiet -- and the
 *     subscription is deliberately torn down when the dialog CLOSES, not merely
 *     when the screen unmounts.
 *  2. **An empty window always says why.** Three different silences reach here
 *     (see RuntimeLogState) plus a fourth -- a connected agent whose retained
 *     buffer is genuinely empty, which is what an agent restart leaves behind.
 *     All four are rendered as sentences. An unexplained empty window is
 *     indistinguishable from "this model prints nothing", which is exactly the
 *     question the operator opened this to answer.
 *  3. **Every gap is visible.** `dropped_bytes` is rendered wherever it
 *     appears, and this component's own display cap produces the same kind of
 *     marker when it trims. A gap shown as silence would be a lie about what
 *     the process printed.
 */

/**
 * How many entries the browser keeps. The agent's own buffer is the history
 * (megabytes, operator-sized); this is only what the DOM holds, so it is sized
 * for rendering cost rather than for retention. Trimming past it is reported,
 * never silent -- see trimmedBytes.
 */
const maxRenderedEntries = 4000;

/** One rendered line: process output, a boundary marker, or a gap notice. */
function LogLine({ entry, t }: Readonly<{ entry: RuntimeLogEntry; t: Translation }>) {
  const gap =
    entry.dropped_bytes && entry.dropped_bytes > 0 ? (
      <Box component="span" sx={{ color: 'warning.main', fontStyle: 'italic' }}>
        {`\n${t.runtimeLogsDropped(entry.dropped_bytes)}\n`}
      </Box>
    ) : null;

  // A boundary between two runs of the same spec. The wording is OURS: the
  // backend allow-lists the event kind to a closed set precisely so that what
  // an operator reads as a portal statement cannot be text an agent chose.
  if (entry.event === 'started' || entry.event === 'exited') {
    const label =
      entry.event === 'started'
        ? t.runtimeLogsProcessStarted(entry.pid ?? 0)
        : t.runtimeLogsProcessExited(entry.exit_code ?? 0);
    return (
      <>
        {gap}
        <Box
          component="span"
          sx={{ color: 'text.secondary', fontStyle: 'italic', display: 'block', my: 0.5 }}
        >
          {`── ${label} ──`}
        </Box>
      </>
    );
  }
  return (
    <>
      {gap}
      {entry.text}
    </>
  );
}

export function RuntimeLogView({
  open,
  onClose,
  api,
  t,
  serverId,
  specId,
  title,
}: Readonly<{
  open: boolean;
  onClose: () => void;
  // Narrowed to the one call this view makes, matching the app's
  // Pick<PortalApi, …> prop convention: a component that can only subscribe
  // cannot accidentally grow a write.
  api: Pick<PortalApi, 'subscribeRuntimeLogs'>;
  t: Translation;
  serverId: string;
  specId: string;
  title: string;
}>) {
  const [entries, setEntries] = useState<RuntimeLogEntry[]>([]);
  const [trimmedBytes, setTrimmedBytes] = useState(0);
  const [state, setState] = useState<RuntimeLogState | null>(null);
  // `scrollbackSeen` is what separates "the agent's buffer is empty" from
  // "nothing has arrived yet". The agent always sends a scrollback batch on
  // subscribe -- an EMPTY one when it has nothing retained -- so its arrival is
  // the moment those two stop being the same state.
  const [scrollbackSeen, setScrollbackSeen] = useState(false);
  const [connectionError, setConnectionError] = useState(false);
  // Follow the tail unless the operator has scrolled away from it. Reading old
  // output while new output arrives is a normal thing to do here, and yanking
  // the viewport away mid-read would make the view unusable for its own
  // purpose.
  const followRef = useRef(true);
  const boxRef = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    if (!open) return undefined;
    setEntries([]);
    setTrimmedBytes(0);
    setState(null);
    setScrollbackSeen(false);
    setConnectionError(false);
    followRef.current = true;

    const onBatch = (batch: RuntimeLogBatch) => {
      if (batch.scrollback) {
        // REPLACE, never append: a reconnect delivers a fresh scrollback and
        // appending it to what is on screen would duplicate the history.
        setScrollbackSeen(true);
        setTrimmedBytes(0);
        setEntries(batch.entries.slice(-maxRenderedEntries));
        return;
      }
      setEntries((prev) => {
        const next = [...prev, ...batch.entries];
        if (next.length <= maxRenderedEntries) return next;
        const cut = next.slice(0, next.length - maxRenderedEntries);
        const lost = cut.reduce((sum, e) => sum + (e.text?.length ?? 0), 0);
        if (lost > 0) setTrimmedBytes((n) => n + lost);
        return next.slice(-maxRenderedEntries);
      });
    };

    return api.subscribeRuntimeLogs(serverId, specId, onBatch, setState, (status) =>
      setConnectionError(status === 'error'),
    );
  }, [api, open, serverId, specId]);

  useEffect(() => {
    if (!followRef.current) return;
    const el = boxRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [entries]);

  const hasOutput = entries.length > 0;
  const notice = (() => {
    if (connectionError) return { severity: 'warning' as const, text: t.runtimeLogsDisconnected };
    if (state === 'offline') return { severity: 'warning' as const, text: t.runtimeLogsOffline };
    if (state === 'unsupported')
      return { severity: 'warning' as const, text: t.runtimeLogsUnsupported };
    // A connected, capable agent that delivered an EMPTY history: say so, or
    // the blank area reads as "the process printed nothing".
    if (state === 'streaming' && scrollbackSeen && !hasOutput)
      return { severity: 'info' as const, text: t.runtimeLogsEmptyBuffer };
    if (!scrollbackSeen && !hasOutput)
      return { severity: 'info' as const, text: t.runtimeLogsWaiting };
    return null;
  })();

  return (
    <Dialog open={open} onClose={onClose} fullWidth maxWidth="lg">
      <DialogTitle>{`${t.runtimeLogsTitle} — ${title}`}</DialogTitle>
      <DialogContent>
        <Typography variant="body2" color="text.secondary" sx={{ mb: 1 }}>
          {t.runtimeLogsIntro}
        </Typography>
        {notice && (
          <Alert severity={notice.severity} sx={{ mb: 1 }}>
            {notice.text}
          </Alert>
        )}
        {trimmedBytes > 0 && (
          <Alert severity="warning" sx={{ mb: 1 }}>
            {t.runtimeLogsTrimmed(trimmedBytes)}
          </Alert>
        )}
        <Box
          ref={boxRef}
          role="log"
          aria-label={t.runtimeLogsTitle}
          aria-live="polite"
          onScroll={(e) => {
            const el = e.currentTarget;
            followRef.current = el.scrollHeight - el.scrollTop - el.clientHeight < 24;
          }}
          sx={{
            fontFamily: 'monospace',
            fontSize: '0.8rem',
            whiteSpace: 'pre-wrap',
            // The one place the page may scroll sideways is inside this box:
            // a model server's output is full of long unbroken lines.
            overflowWrap: 'anywhere',
            overflowY: 'auto',
            overflowX: 'auto',
            maxHeight: '60vh',
            minHeight: '20vh',
            p: 1,
            border: 1,
            borderColor: 'divider',
            borderRadius: 1,
            bgcolor: 'action.hover',
          }}
        >
          {entries.map((entry, i) => (
            // The index is a legitimate key here: entries are append-only
            // and never reordered, and a scrollback REPLACES the whole list.
            <LogLine key={i} entry={entry} t={t} />
          ))}
        </Box>
      </DialogContent>
      <DialogActions>
        <Button onClick={onClose}>{t.captureClose}</Button>
      </DialogActions>
    </Dialog>
  );
}
