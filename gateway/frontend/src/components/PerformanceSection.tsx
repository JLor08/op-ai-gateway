// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useEffect, useMemo, useRef, useState } from 'react';
import { Alert, Box, Button } from '@mui/material';
import type { PerfGPU, PerfPoint, PortalServer, TimeSeriesPoint } from '../api';
import type { PortalApi, Translation } from './shared/types';
import { Panel } from './shared/Panel';
import { SelectField } from './shared/SelectField';
import { usePolledFetch } from './shared/usePolledFetch';
import { LineChart, type LineSeries } from './LineChart';
import { chartColor } from './chartPalette';
import { formatTsSeconds } from './activityColumns';

// The Performance view offers three short live windows. These tokens double as
// the `usageTimeSeries` window param (they are members of the shared TsWindow
// whitelist) and drive the rolling-trim span for the SSE-appended samples.
export type PerfWindow = '5m' | '15m' | '1h';
const WINDOWS: readonly PerfWindow[] = ['5m', '15m', '1h'];
const WINDOW_SECONDS: Record<PerfWindow, number> = { '5m': 300, '15m': 900, '1h': 3600 };
// Throughput resolution per window (all valid TsBucket values); finer for short
// windows, coarser for the hour so the chart stays readable.
const WINDOW_BUCKET: Record<PerfWindow, number> = { '5m': 5, '15m': 10, '1h': 60 };

// Token throughput comes from usage/timeseries (not the perf SSE), so it is
// refetched on a short interval while live to keep the chart moving alongside the
// SSE-driven host/GPU charts. Paused freezes it. Exported for the test.
export const THROUGHPUT_POLL_MS = 10000;
// Stable by-reference fallback for a not-yet-loaded throughput series, so the
// derived `tsPoints` doesn't hand the useMemo below a fresh [] every render
// (which would defeat its memoization).
const EMPTY_TS_POINTS: TimeSeriesPoint[] = [];

// A last_seen older than ~3x the expected agent publish cadence flags the server
// as stale (non-blocking banner). The agent's default cadence is a few seconds.
const EXPECTED_CADENCE_SECONDS = 10;
const STALE_AFTER_SECONDS = EXPECTED_CADENCE_SECONDS * 3;

// Per-series line color: the large theme-independent palette (chartPalette),
// shared by per-GPU and per-CPU-core lines so a many-core CPU or a multi-GPU box
// still gets distinct, stable colors. Kept as `gpuColorAt` for its existing call
// sites/tests; delegates to chartColor.
export const gpuColorAt = chartColor;

export type GpuMetric = 'util_pct' | 'mem_pct' | 'mem_mib' | 'temp_c' | 'power_w';

// Percentage with a divide-by-zero guard (0 when total is non-positive).
export function pct(used: number, total: number): number {
  return total > 0 ? (used / total) * 100 : 0;
}

function gpuLabel(index: number, name: string): string {
  return name ? `${name} #${index}` : `GPU ${index}`;
}

function gpuMetricValue(g: PerfGPU, metric: GpuMetric): number {
  switch (metric) {
    case 'util_pct':
      return g.util_pct;
    case 'mem_pct':
      return pct(g.mem_used_bytes, g.mem_total_bytes);
    case 'mem_mib':
      return g.mem_used_bytes / (1024 * 1024);
    case 'temp_c':
      return g.temp_c;
    case 'power_w':
      return g.power_w;
  }
}

/**
 * One `LineSeries` per GPU index present in the LATEST point (the current GPU
 * set), each series pulling `metric` across every point (missing index → 0).
 * Pure; exported for unit testing. `mem_pct` is computed used/total*100 with a
 * divide-by-zero guard.
 */
export function gpuSeries(
  points: PerfPoint[],
  metric: GpuMetric,
  colorAt: (i: number) => string,
): LineSeries[] {
  const latest = points.at(-1);
  if (!latest || latest.gpus.length === 0) return [];
  return latest.gpus.map((g, index) => ({
    label: gpuLabel(index, g.name),
    color: colorAt(index),
    values: points.map((p) => {
      const gpu = p.gpus[index];
      return gpu ? gpuMetricValue(gpu, metric) : 0;
    }),
  }));
}

/**
 * One line per CPU core (util %), sized to the latest point's core count and
 * colored from the large theme-independent palette. A point with fewer cores than
 * the latest contributes 0 for the missing indices. Pure; exported for testing.
 */
export function cpuCoreSeries(points: PerfPoint[]): LineSeries[] {
  const latest = points.at(-1);
  const cores = latest?.cpu_cores?.length ?? 0;
  if (cores === 0) return [];
  return Array.from({ length: cores }, (_, index) => ({
    label: `Core ${index}`,
    color: chartColor(index),
    values: points.map((p) => p.cpu_cores?.[index] ?? 0),
  }));
}

// Sum a monotonic net counter across all interfaces at a point.
function sumNet(p: PerfPoint, key: 'rx_bytes' | 'tx_bytes'): number {
  return p.net.reduce((acc, n) => acc + (n[key] ?? 0), 0);
}

/**
 * Per-point byte rate (B/s) for a net counter: delta bytes / delta seconds
 * between consecutive points. The first point is 0; a non-positive time delta
 * or a non-monotonic drop (counter reset) yields 0. Pure; exported for testing.
 */
export function netRateSeries(points: PerfPoint[], key: 'rx_bytes' | 'tx_bytes'): number[] {
  return points.map((p, i) => {
    if (i === 0) return 0;
    const prev = points[i - 1];
    const dt = (new Date(p.t).getTime() - new Date(prev.t).getTime()) / 1000;
    if (dt <= 0) return 0;
    const delta = sumNet(p, key) - sumNet(prev, key);
    return delta > 0 ? delta / dt : 0;
  });
}

// Short wall-clock label (HH:MM:SS) for a perf sample timestamp.
export function perfTimeLabel(iso: string): string {
  return new Date(iso).toLocaleTimeString();
}

/**
 * Live per-server performance sub-view: fetches a decimated history window,
 * subscribes to the per-server SSE (snapshot replaces, sample appends within a
 * rolling window), and renders reused `LineChart`s for GPU, host, and token
 * throughput. AgentToken-style leaf: `{ t, api, server }`.
 */
export function PerformanceSection({
  t,
  api,
  server,
}: Readonly<{
  t: Translation;
  api: Pick<PortalApi, 'serverPerfHistory' | 'subscribeServerPerf' | 'usageTimeSeries'>;
  server: PortalServer;
}>) {
  const [points, setPoints] = useState<PerfPoint[]>([]);
  const [win, setWin] = useState<PerfWindow>('15m');
  const [live, setLive] = useState(true);
  const [status, setStatus] = useState<'open' | 'error' | 'loading'>('loading');

  // The SSE effect must not resubscribe when `live`/`win` change; the sample
  // callback reads the latest values through refs instead.
  const liveRef = useRef(live);
  const winRef = useRef(win);
  useEffect(() => {
    liveRef.current = live;
  }, [live]);
  useEffect(() => {
    winRef.current = win;
  }, [win]);

  // History (perf points); refetched on window change. Token throughput is
  // handled by its own poll effect below.
  useEffect(() => {
    let cancelled = false;
    setStatus('loading');
    api
      .serverPerfHistory(server.id, win)
      .then((h) => {
        if (!cancelled) setPoints(h.points ?? []);
      })
      .catch(() => {
        if (!cancelled) setStatus('error');
      });
    return () => {
      cancelled = true;
    };
  }, [api, server.id, win]);

  // Live token throughput: the perf SSE carries only host/GPU samples, not usage,
  // so throughput is fetched from usage/timeseries here — immediately when the
  // deps change (mount, window change) or on RESUME from pause, AND then every
  // THROUGHPUT_POLL_MS while live. Pause freezes the chart at its last value
  // (usePolledFetch keeps the previous data on a transient fetch failure too —
  // "non-blocking: keep the last series").
  const throughputFetch = usePolledFetch(
    () =>
      api.usageTimeSeries({
        window: win,
        bucket: WINDOW_BUCKET[win],
        scope: 'all',
        server_exact: server.name,
      }),
    [api, server.name, win],
    { intervalMs: THROUGHPUT_POLL_MS, live },
  );
  const throughput = throughputFetch.data;

  // Live SSE: `snapshot` replaces the series; each `sample` appends. Both trim to
  // the selected window span. The backend snapshot carries the FULL per-server
  // ring (up to ~1h), so it MUST be window-trimmed here or a "5m" selection would
  // render the whole ring on connect and on every reconnect. A snapshot is also
  // ignored while paused so a reconnect can't clobber a frozen view.
  useEffect(() => {
    const stop = api.subscribeServerPerf(
      server.id,
      (snap) => {
        if (!liveRef.current) return;
        const anchor = snap.length ? new Date(snap.at(-1)!.t).getTime() : Date.now();
        const cutoff = anchor - WINDOW_SECONDS[winRef.current] * 1000;
        setPoints(snap.filter((pt) => new Date(pt.t).getTime() >= cutoff));
      },
      (p) => {
        if (!liveRef.current) return;
        setPoints((prev) => {
          const cutoff = new Date(p.t).getTime() - WINDOW_SECONDS[winRef.current] * 1000;
          return [...prev, p].filter((pt) => new Date(pt.t).getTime() >= cutoff);
        });
      },
      setStatus,
    );
    return stop;
  }, [api, server.id]);

  const times = useMemo(() => points.map((p) => perfTimeLabel(p.t)), [points]);
  const latest = points.at(-1);
  const hasGpu = !!latest && latest.gpus.length > 0;
  const coreSeries = useMemo(() => cpuCoreSeries(points), [points]);
  const hasCores = coreSeries.length > 0;
  // Power charts appear only where the latest point actually carries a value
  // (mirrors hasGpu). null = not measured -> no chart.
  const hasCpuPower = !!latest && latest.cpu_power_w != null;
  const hasSystemPower = !!latest && latest.system_power_w != null;
  const hasCpuTemp = !!latest && latest.cpu_temp_c != null;

  const tsPoints = throughput?.points ?? EMPTY_TS_POINTS;
  const tsTimes = useMemo(() => tsPoints.map((p) => perfTimeLabel(p.t)), [tsPoints]);

  const windowControl = (
    <Box sx={{ display: 'flex', flexWrap: 'wrap', gap: 1.5, alignItems: 'center' }}>
      <Box sx={{ minWidth: 130 }}>
        <SelectField
          id="perf-window"
          label={t.serverPerfWindowLabel}
          value={win}
          onChange={(e) => setWin(e.target.value as PerfWindow)}
        >
          {WINDOWS.map((w) => (
            <option key={w} value={w}>
              {formatTsSeconds(WINDOW_SECONDS[w], t)}
            </option>
          ))}
        </SelectField>
      </Box>
      <Button
        type="button"
        variant="outlined"
        size="small"
        aria-label={t.serverPerfPauseToggle}
        onClick={() => setLive((v) => !v)}
      >
        {live ? t.serverPerfLive : t.serverPerfPaused}
      </Button>
    </Box>
  );

  // Empty state: no data AND the server has never reported.
  if (points.length === 0 && server.last_seen_at == null) {
    return (
      <Panel titleId="perf-heading" title={server.name} actions={windowControl}>
        <Box
          component="span"
          data-testid="perf-point-count"
          data-perf-status={status}
          sx={{ display: 'none' }}
        >
          {points.length}
        </Box>
        <Alert severity="info">{t.serverPerfNoAgent}</Alert>
      </Panel>
    );
  }

  // Stale banner: no live data and the last report is older than the threshold.
  const staleSeconds = server.last_seen_at
    ? Math.floor((Date.now() - new Date(server.last_seen_at).getTime()) / 1000)
    : null;
  const stale = points.length === 0 && staleSeconds != null && staleSeconds > STALE_AFTER_SECONDS;

  return (
    <Panel titleId="perf-heading" title={server.name} actions={windowControl}>
      <Box
        component="span"
        data-testid="perf-point-count"
        data-perf-status={status}
        sx={{ display: 'none' }}
      >
        {points.length}
      </Box>
      {stale && staleSeconds != null && (
        <Alert severity="warning" sx={{ mb: 2 }}>
          {t.serverPerfStale(staleSeconds)}
        </Alert>
      )}
      <Box
        sx={{
          display: 'grid',
          gridTemplateColumns: { xs: '1fr', md: 'repeat(2, minmax(0, 1fr))' },
          gap: 2,
        }}
      >
        {hasGpu && (
          <>
            <LineChart
              t={t}
              title={t.serverPerfGpuUtil}
              unit={t.serverPerfUnitPct}
              times={times}
              series={gpuSeries(points, 'util_pct', gpuColorAt)}
            />
            <LineChart
              t={t}
              title={t.serverPerfGpuVram}
              unit={t.serverPerfUnitPct}
              times={times}
              series={gpuSeries(points, 'mem_pct', gpuColorAt)}
            />
            <LineChart
              t={t}
              title={t.serverPerfGpuVramMb}
              unit={t.serverPerfUnitMb}
              times={times}
              series={gpuSeries(points, 'mem_mib', gpuColorAt)}
            />
            <LineChart
              t={t}
              title={t.serverPerfGpuTemp}
              unit={t.serverPerfUnitCelsius}
              times={times}
              series={gpuSeries(points, 'temp_c', gpuColorAt)}
            />
            <LineChart
              t={t}
              title={t.serverPerfGpuPower}
              unit={t.serverPerfUnitWatt}
              times={times}
              series={gpuSeries(points, 'power_w', gpuColorAt)}
            />
          </>
        )}
        <LineChart
          t={t}
          title={t.serverPerfCpu}
          unit={t.serverPerfUnitPct}
          times={times}
          series={[
            {
              label: t.serverPerfCpu,
              color: chartColor(0),
              values: points.map((p) => p.cpu_util_pct),
            },
          ]}
        />
        {hasCores && (
          <LineChart
            t={t}
            title={t.serverPerfCpuCores}
            unit={t.serverPerfUnitPct}
            times={times}
            series={coreSeries}
          />
        )}
        {hasCpuPower && (
          <LineChart
            t={t}
            title={t.serverPerfCpuPower}
            unit={t.serverPerfUnitWatt}
            times={times}
            series={[
              {
                label: t.serverPerfCpuPower,
                color: chartColor(0),
                values: points.map((p) => p.cpu_power_w ?? 0),
              },
            ]}
          />
        )}
        {hasSystemPower && (
          <LineChart
            t={t}
            title={t.serverPerfSystemPower}
            unit={t.serverPerfUnitWatt}
            times={times}
            series={[
              {
                label: t.serverPerfSystemPower,
                color: chartColor(0),
                values: points.map((p) => p.system_power_w ?? 0),
              },
            ]}
          />
        )}
        {hasCpuTemp && (
          <LineChart
            t={t}
            title={t.serverPerfCpuTemp}
            unit={t.serverPerfUnitCelsius}
            times={times}
            series={[
              {
                label: t.serverPerfCpuTemp,
                color: chartColor(0),
                values: points.map((p) => p.cpu_temp_c ?? 0),
              },
            ]}
          />
        )}
        <LineChart
          t={t}
          title={t.serverPerfMem}
          unit={t.serverPerfUnitPct}
          times={times}
          series={[
            {
              label: t.serverPerfMemUsed,
              color: 'var(--brand-primary)',
              values: points.map((p) => pct(p.mem_used_bytes, p.mem_total_bytes)),
            },
            {
              label: t.serverPerfSwapUsed,
              color: 'var(--chart-series-2)',
              values: points.map((p) => pct(p.swap_used_bytes, p.swap_total_bytes)),
            },
          ]}
        />
        <LineChart
          t={t}
          title={t.serverPerfLoad}
          unit=""
          times={times}
          series={[
            {
              label: t.serverPerfLoad1,
              color: 'var(--brand-primary)',
              values: points.map((p) => p.load1),
            },
            {
              label: t.serverPerfLoad5,
              color: 'var(--chart-series-2)',
              values: points.map((p) => p.load5),
            },
            {
              label: t.serverPerfLoad15,
              color: gpuColorAt(2),
              values: points.map((p) => p.load15),
            },
          ]}
        />
        <LineChart
          t={t}
          title={t.serverPerfNet}
          unit={t.serverPerfUnitBytesPerSec}
          times={times}
          series={[
            {
              label: t.serverPerfNetRx,
              color: 'var(--brand-primary)',
              values: netRateSeries(points, 'rx_bytes'),
            },
            {
              label: t.serverPerfNetTx,
              color: 'var(--chart-series-2)',
              values: netRateSeries(points, 'tx_bytes'),
            },
          ]}
        />
        <LineChart
          t={t}
          title={t.serverPerfTokThroughput}
          unit={t.activityTsUnitTokPerSec}
          times={tsTimes}
          series={[
            {
              label: t.activityTsPromptThroughput,
              color: 'var(--brand-primary)',
              values: tsPoints.map((p) => p.prompt_tokens_per_second),
            },
            {
              label: t.activityTsCompletionThroughput,
              color: 'var(--chart-series-2)',
              values: tsPoints.map((p) => p.completion_tokens_per_second),
            },
          ]}
        />
      </Box>
    </Panel>
  );
}
