// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useEffect, useMemo, useRef, useState } from 'react';
import { Alert, Box, Button, Chip, FormControlLabel, FormHelperText, Switch } from '@mui/material';
import type { LogRecord } from '../api';
import type { PortalApi, Translation } from './shared/types';
import { Panel } from './shared/Panel';
import { SelectField } from './shared/SelectField';

// Mirrors the backend ring cap so the live view keeps the same recent window and
// never grows unbounded from the SSE `record` stream.
const MAX_RECORDS = 2000;

// The runtime-selectable levels (lower-case wire values sent to setLogLevel).
// "trace" is the most verbose and leads the list; method-level spans (from the
// Tracing toggle below) only appear in this view when the level is "trace".
const LEVELS = ['trace', 'debug', 'info', 'warn', 'error'] as const;

// Chip color per slog level (`level` is upper-case "TRACE"|"DEBUG"|"INFO"|"WARN"|"ERROR").
function levelColor(level: string): 'default' | 'info' | 'warning' | 'error' {
  switch (level.toUpperCase()) {
    case 'ERROR':
      return 'error';
    case 'WARN':
      return 'warning';
    case 'TRACE':
    case 'DEBUG':
      return 'default';
    default:
      return 'info'; // INFO (and anything unexpected)
  }
}

// Short wall-clock label (HH:MM:SS) for a log record timestamp; the raw string
// falls through unchanged if it is not a parseable date.
function formatTime(iso: string): string {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleTimeString();
}

// Flatten slog attrs to a compact `key=value` string. The bearer/agent token is
// never present in `attrs` (guaranteed by the backend), so this is safe to show.
function formatAttrs(attrs: Record<string, unknown>): string {
  return Object.entries(attrs)
    .map(([k, v]) => `${k}=${typeof v === 'string' ? v : JSON.stringify(v)}`)
    .join(' ');
}

/**
 * System-admin live log view: loads the ring snapshot + current level, subscribes
 * to the log SSE (snapshot replaces, record appends — trimmed to MAX_RECORDS,
 * both gated on Live), offers a runtime level dropdown (calls api.setLogLevel), a
 * Live/Pause toggle (the button label reflects the current state), and a Clear
 * button. Renders a monospace scrolling list of time · level chip · msg · attrs.
 */
export function LogsView({
  t,
  api,
}: Readonly<{
  t: Translation;
  api: Pick<PortalApi, 'getTracing' | 'logs' | 'setLogLevel' | 'setTracing' | 'subscribeLogs'>;
}>) {
  const [records, setRecords] = useState<LogRecord[]>([]);
  const [level, setLevel] = useState<string>('info');
  const [live, setLive] = useState(true);
  const [tracingEnabled, setTracingEnabled] = useState(false);

  // The SSE effect must not resubscribe when `live` toggles; the callbacks read
  // the latest value through a ref instead (mirrors PerformanceSection).
  const liveRef = useRef(live);
  useEffect(() => {
    liveRef.current = live;
  }, [live]);

  // Auto-scroll the list to the newest line as records arrive while live.
  const listRef = useRef<HTMLDivElement | null>(null);

  // Initial snapshot (ring + current level).
  useEffect(() => {
    let cancelled = false;
    api
      .logs()
      .then((res) => {
        if (cancelled) return;
        setRecords(res.records ?? []);
        setLevel(res.level ?? 'info');
      })
      .catch(() => {
        /* non-blocking: the SSE snapshot will seed the view */
      });
    return () => {
      cancelled = true;
    };
  }, [api]);

  // Initial tracing state (independent of the log level — the two are separate
  // gates: spans are recorded only when Tracing is ON AND the level is trace).
  useEffect(() => {
    let cancelled = false;
    api
      .getTracing()
      .then((res) => {
        if (cancelled) return;
        setTracingEnabled(res.enabled);
      })
      .catch(() => {
        /* non-blocking: leave the last-known (default off) state */
      });
    return () => {
      cancelled = true;
    };
  }, [api]);

  // Live SSE: `snapshot` replaces the list + level; each `record` appends. Both
  // are ignored while paused so a reconnect/push can't clobber a frozen view.
  useEffect(() => {
    const stop = api.subscribeLogs(
      (snap, lvl) => {
        if (!liveRef.current) return;
        setRecords(snap.slice(-MAX_RECORDS));
        setLevel(lvl);
      },
      (r) => {
        if (!liveRef.current) return;
        setRecords((prev) => [...prev, r].slice(-MAX_RECORDS));
      },
    );
    return stop;
  }, [api]);

  useEffect(() => {
    if (!live) return;
    const el = listRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [records, live]);

  const levelLabels = useMemo<Record<string, string>>(
    () => ({
      trace: t.logsLevelTrace,
      debug: t.logsLevelDebug,
      info: t.logsLevelInfo,
      warn: t.logsLevelWarn,
      error: t.logsLevelError,
    }),
    [t],
  );

  const onLevelChange = (next: string) => {
    setLevel(next); // optimistic; the server confirm below re-affirms it
    api
      .setLogLevel(next)
      .then((res) => setLevel(res.level))
      .catch(() => {
        /* keep the optimistic value; the next snapshot reconciles */
      });
  };

  const onTracingChange = (next: boolean) => {
    setTracingEnabled(next); // optimistic; the server confirm below re-affirms it
    api
      .setTracing(next)
      .then((res) => setTracingEnabled(res.enabled))
      .catch(() => {
        /* keep the optimistic value; a later reload reconciles */
      });
  };

  const controls = (
    <Box sx={{ display: 'flex', flexWrap: 'wrap', gap: 1.5, alignItems: 'center' }}>
      <Box sx={{ minWidth: 150 }}>
        <SelectField
          id="logs-level"
          label={t.logsLevelLabel}
          value={level}
          onChange={(e) => onLevelChange(e.target.value)}
        >
          {LEVELS.map((lv) => (
            <option key={lv} value={lv}>
              {levelLabels[lv]}
            </option>
          ))}
        </SelectField>
      </Box>
      <Button type="button" variant="outlined" size="small" onClick={() => setLive((v) => !v)}>
        {live ? t.logsLive : t.logsPause}
      </Button>
      <Button type="button" variant="outlined" size="small" onClick={() => setRecords([])}>
        {t.logsClear}
      </Button>
      <Box>
        <FormControlLabel
          control={
            <Switch
              size="small"
              checked={tracingEnabled}
              onChange={(e) => onTracingChange(e.target.checked)}
            />
          }
          label={t.logsTracingLabel}
        />
        <FormHelperText sx={{ ml: 0, mt: -0.5 }}>{t.logsTracingHelp}</FormHelperText>
      </Box>
    </Box>
  );

  return (
    <Panel titleId="logs-heading" title={t.logsTitle} actions={controls}>
      {records.length === 0 ? (
        <Alert severity="info">{t.logsEmpty}</Alert>
      ) : (
        <Box
          ref={listRef}
          role="log"
          aria-label={t.logsTitle}
          sx={{
            fontFamily: 'var(--font-mono, monospace)',
            fontSize: 13,
            lineHeight: 1.6,
            maxHeight: '70vh',
            overflowY: 'auto',
            border: '1px solid var(--line)',
            borderRadius: 1,
            bgcolor: 'var(--surface)',
            p: 1.5,
          }}
        >
          {records.map((r, i) => (
            <Box
              key={`${r.t}-${i}`}
              sx={{
                display: 'flex',
                alignItems: 'baseline',
                gap: 1,
                py: 0.25,
                borderBottom: '1px solid transparent',
                whiteSpace: 'pre-wrap',
                wordBreak: 'break-word',
              }}
            >
              <Box component="span" sx={{ color: 'text.secondary', flex: '0 0 auto' }}>
                {formatTime(r.t)}
              </Box>
              <Chip
                size="small"
                color={levelColor(r.level)}
                label={r.level}
                sx={{ flex: '0 0 auto', fontFamily: 'inherit', height: 20 }}
              />
              <Box component="span" sx={{ flex: '1 1 auto' }}>
                {r.msg}
                {r.attrs && Object.keys(r.attrs).length > 0 && (
                  <Box component="span" sx={{ color: 'text.secondary', ml: 1 }}>
                    {formatAttrs(r.attrs)}
                  </Box>
                )}
              </Box>
            </Box>
          ))}
        </Box>
      )}
    </Panel>
  );
}
