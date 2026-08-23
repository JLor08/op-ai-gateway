// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useEffect, useMemo, useState } from 'react';
import { Box, Button, Typography } from '@mui/material';
import type { AvailabilityPoint, PortalServer } from '../api';
import type { PortalApi, Translation } from './shared/types';
import { Panel } from './shared/Panel';
import { SelectField } from './shared/SelectField';
import { usePolledFetch } from './shared/usePolledFetch';
import { LineChart } from './LineChart';
import { UptimeTimeline, type TimelineState, type UptimeSegment } from './UptimeTimeline';
import { chartColor } from './chartPalette';
import {
  TS_WINDOWS,
  TS_BUCKETS,
  TS_WINDOW_SECONDS,
  formatTsSeconds,
  type TsWindow,
  type TsBucket,
} from './activityColumns';

const AVAIL_WINDOW_KEY = 'op.availability.tsWindow';
const AVAIL_BUCKET_KEY = 'op.availability.tsBucket';
const DEFAULT_WIN: TsWindow = '1d';
const DEFAULT_BUCKET: TsBucket = 900;
// Trailing-edge threshold: if the newest reduced point is more than this away from
// `to`, the tail [lastPoint, to) is an observer gap ("unknown"). Interior gaps are
// NOT inferred from spacing — they come from the backend `gap_before` flag, which is
// authoritative (a collapsed multi-hour same-state run is same-state points hours
// apart WITHOUT gap_before, and must NOT be mistaken for a gap). 10 min = 2×heartbeat.
const GAP_THRESHOLD_MS = 10 * 60 * 1000;
// Poll cadence while live (mirrors PerformanceSection's live refresh idea).
const AVAIL_POLL_MS = 30000;
// Stable by-reference fallback for a not-yet-loaded/failed fetch, so the
// derived `points` doesn't hand the useMemos below a fresh [] every render
// (which would defeat their memoization -- see PerformanceSection's tsPoints).
const EMPTY_POINTS: AvailabilityPoint[] = [];

function readWin(): TsWindow {
  try {
    const v = window.localStorage?.getItem(AVAIL_WINDOW_KEY) as TsWindow | null;
    return v && TS_WINDOWS.includes(v) ? v : DEFAULT_WIN;
  } catch {
    return DEFAULT_WIN;
  }
}
function readBucket(): TsBucket {
  try {
    const v = Number(window.localStorage?.getItem(AVAIL_BUCKET_KEY));
    return TS_BUCKETS.includes(v as TsBucket) ? (v as TsBucket) : DEFAULT_BUCKET;
  } catch {
    return DEFAULT_BUCKET;
  }
}

function healthState(p: AvailabilityPoint): TimelineState {
  if (p.health === 'unhealthy') return 'unhealthy';
  if (p.health === 'degraded') return 'degraded';
  if (p.health === 'unknown' || p.health === '') return 'unknown';
  return 'healthy';
}
function agentState(p: AvailabilityPoint): TimelineState {
  return p.agent_reporting ? 'present' : 'absent';
}
function netbirdState(p: AvailabilityPoint): TimelineState {
  return p.netbird_connected ? 'connected' : 'disconnected';
}

// Up = a genuinely available server (not unhealthy, and a known health state).
function healthIsUp(p: AvailabilityPoint): boolean {
  return p.health !== 'unhealthy' && p.health !== '' && p.health !== 'unknown';
}

// Up = the server's linked NetBird peer was connected.
function netbirdIsUp(p: AvailabilityPoint): boolean {
  return p.netbird_connected;
}

/**
 * buildSegments turns availability points into contiguous timeline segments,
 * back-filling [from, points[0]) with the first sample's state. The interval
 * [points[i], points[i+1]) is an "unknown" observer gap when points[i+1].gap_before
 * is set (the backend's authoritative flag: the gateway was not sampling), otherwise
 * it holds stateOf(points[i]). The trailing edge [lastPoint, to) is unknown only when
 * `to` is more than GAP_THRESHOLD_MS beyond the last point (the newest sample is
 * stale). Interior gaps are NOT inferred from spacing, so a collapsed multi-hour
 * same-state run (same-state points hours apart, gap_before=false) stays painted with
 * its state rather than misread as a gap. All segments are clipped to [from, to] and
 * returned strictly forward in time. Pure; exported for tests. Empty input yields a
 * single "unknown" segment spanning the window.
 */
export function buildSegments(
  points: AvailabilityPoint[],
  from: string,
  to: string,
  stateOf: (p: AvailabilityPoint) => TimelineState,
): UptimeSegment[] {
  const f = new Date(from).getTime();
  const tt = new Date(to).getTime();
  if (points.length === 0) return [{ start: from, end: to, state: 'unknown' }];
  const segs: UptimeSegment[] = [];
  const firstT = new Date(points[0].t).getTime();
  // Back-fill the head of the window with the first observed state (points[0] never
  // carries gap_before — it is the first in-window sample, with no predecessor).
  if (firstT > f)
    segs.push({ start: from, end: new Date(firstT).toISOString(), state: stateOf(points[0]) });
  for (let i = 0; i < points.length; i++) {
    const startMs = Math.max(f, new Date(points[i].t).getTime());
    let state: TimelineState;
    let endMs: number;
    if (i + 1 < points.length) {
      endMs = Math.min(new Date(points[i + 1].t).getTime(), tt);
      // Interior gap: authoritative flag on the NEXT point (its raw predecessor was
      // > the backend gap floor away). Otherwise this point's state held.
      state = points[i + 1].gap_before ? 'unknown' : stateOf(points[i]);
    } else {
      endMs = tt;
      // Trailing edge: unknown only when the newest sample is stale relative to `to`.
      state =
        tt - new Date(points[i].t).getTime() > GAP_THRESHOLD_MS ? 'unknown' : stateOf(points[i]);
    }
    if (endMs <= startMs) continue;
    segs.push({
      start: new Date(startMs).toISOString(),
      end: new Date(endMs).toISOString(),
      state,
    });
  }
  return segs.filter((s) => new Date(s.end).getTime() > new Date(s.start).getTime());
}

/**
 * bucketUptime returns, per resolution bucket in [from,to], the percent (0–100)
 * of KNOWN (non-gap) time that satisfied isUp. A bucket with no known time -> 0.
 * The interval [points[i], points[i+1]) is excluded (unknown) when points[i+1] is
 * flagged gap_before (an observer gap contributes to neither known nor up time), and
 * the trailing interval [lastPoint, to) is excluded only when the newest sample is
 * stale (> GAP_THRESHOLD_MS from `to`). Interior gaps are driven by the authoritative
 * flag, NOT by spacing, so a collapsed multi-hour same-state run counts fully as known
 * time rather than being dropped. Pure; exported for tests.
 */
export function bucketUptime(
  points: AvailabilityPoint[],
  from: string,
  to: string,
  bucketSeconds: number,
  isUp: (p: AvailabilityPoint) => boolean,
): number[] {
  const f = new Date(from).getTime();
  const tt = new Date(to).getTime();
  const bucketMs = bucketSeconds * 1000;
  const nBuckets = Math.max(1, Math.ceil((tt - f) / bucketMs));
  const upMs = new Array<number>(nBuckets).fill(0);
  const knownMs = new Array<number>(nBuckets).fill(0);
  if (points.length === 0) return new Array<number>(nBuckets).fill(0);
  for (let i = 0; i < points.length; i++) {
    const pointMs = new Date(points[i].t).getTime();
    const startMs = Math.max(f, pointMs);
    let endMs: number;
    let isGap: boolean;
    if (i + 1 < points.length) {
      endMs = new Date(points[i + 1].t).getTime();
      isGap = points[i + 1].gap_before === true;
    } else {
      endMs = tt;
      isGap = tt - pointMs > GAP_THRESHOLD_MS;
    }
    endMs = Math.min(endMs, tt);
    if (endMs <= startMs) continue;
    if (isGap) continue; // observer gap: contributes to neither known nor up time
    const up = isUp(points[i]);
    // Distribute [startMs, endMs) across the buckets it overlaps.
    let cur = startMs;
    while (cur < endMs) {
      const bi = Math.min(nBuckets - 1, Math.floor((cur - f) / bucketMs));
      const bucketEnd = f + (bi + 1) * bucketMs;
      const segEnd = Math.min(endMs, bucketEnd);
      const dur = segEnd - cur;
      knownMs[bi] += dur;
      if (up) upMs[bi] += dur;
      cur = segEnd;
    }
  }
  return upMs.map((u, i) => (knownMs[i] > 0 ? (u / knownMs[i]) * 100 : 0));
}

// Percent formatted for display. The codebase formats rates/percentages with a
// fixed one-decimal precision (ActivityTable, SpeedHistogram); we match that
// rather than thread a locale, and pass a pre-formatted string to the i18n keys.
function fmtPct(pct: number): string {
  return `${pct.toFixed(1)} %`;
}

/**
 * Per-server availability sub-view: fetches the reduced availability history for
 * the selected window, renders two uptime timelines (server health + ServerAgent
 * presence) and two uptime-% line charts, and (while live) polls every 30s.
 * Leaf: `{ t, api, server }`, mirroring PerformanceSection.
 */
export function AvailabilitySection({
  t,
  api,
  server,
}: Readonly<{
  t: Translation;
  api: Pick<PortalApi, 'serverAvailability'>;
  server: PortalServer;
}>) {
  const [win, setWin] = useState<TsWindow>(() => readWin());
  const [bucket, setBucket] = useState<TsBucket>(() => readBucket());
  const [live, setLive] = useState(true);

  useEffect(() => {
    try {
      window.localStorage?.setItem(AVAIL_WINDOW_KEY, win);
    } catch {
      /* best-effort */
    }
  }, [win]);
  useEffect(() => {
    try {
      window.localStorage?.setItem(AVAIL_BUCKET_KEY, String(bucket));
    } catch {
      /* best-effort */
    }
  }, [bucket]);

  // Fetches on mount/server/window change AND on a 30s poll while live
  // (immediately on resume too) — latest-wins + unmount-safe via
  // useLatestFetch underneath, so a slow response for an older window can
  // never overwrite a newer one, and a post-unmount resolution is a no-op.
  const availability = usePolledFetch(
    () => api.serverAvailability(server.id, win),
    [server.id, win],
    {
      intervalMs: AVAIL_POLL_MS,
      live,
    },
  );
  const pending = availability.status === 'idle' || availability.status === 'loading';
  const points: AvailabilityPoint[] = availability.data?.points ?? EMPTY_POINTS;
  const from = availability.data?.from ?? '';
  const to = availability.data?.to ?? '';

  const colorHealth = (s: TimelineState) => {
    switch (s) {
      case 'healthy':
        return 'var(--success-bg, #2e7d32)';
      case 'degraded':
        return 'var(--watch-bg, #ed6c02)';
      case 'unhealthy':
        return '#d32f2f';
      default:
        return 'var(--standby-bg, #9e9e9e)';
    }
  };
  const colorAgent = (s: TimelineState) => {
    switch (s) {
      case 'present':
        return 'var(--success-bg, #2e7d32)';
      case 'absent':
        return '#d32f2f';
      default:
        return 'var(--standby-bg, #9e9e9e)';
    }
  };
  const labelHealth = (s: TimelineState) => {
    switch (s) {
      case 'healthy':
        return t.availabilityStateHealthy;
      case 'degraded':
        return t.availabilityStateDegraded;
      case 'unhealthy':
        return t.availabilityStateUnhealthy;
      default:
        return t.availabilityStateUnknown;
    }
  };
  const labelAgent = (s: TimelineState) => {
    switch (s) {
      case 'present':
        return t.availabilityStatePresent;
      case 'absent':
        return t.availabilityStateAbsent;
      default:
        return t.availabilityStateUnknown;
    }
  };
  const colorNetbird = (s: TimelineState) => {
    switch (s) {
      case 'connected':
        return 'var(--success-bg, #2e7d32)';
      case 'disconnected':
        return '#d32f2f';
      default:
        return 'var(--standby-bg, #9e9e9e)';
    }
  };
  const labelNetbird = (s: TimelineState) => {
    switch (s) {
      case 'connected':
        return t.availabilityStateConnected;
      case 'disconnected':
        return t.availabilityStateDisconnected;
      default:
        return t.availabilityStateUnknown;
    }
  };

  const healthSegs = useMemo(
    () => (from && to ? buildSegments(points, from, to, healthState) : []),
    [points, from, to],
  );
  const agentSegs = useMemo(
    () => (from && to ? buildSegments(points, from, to, agentState) : []),
    [points, from, to],
  );

  const uptimePct = useMemo(
    () => (from && to ? bucketUptime(points, from, to, bucket, healthIsUp) : []),
    [points, from, to, bucket],
  );
  const agentPct = useMemo(
    () => (from && to ? bucketUptime(points, from, to, bucket, (p) => p.agent_reporting) : []),
    [points, from, to, bucket],
  );
  const netbirdSegs = useMemo(
    () => (from && to ? buildSegments(points, from, to, netbirdState) : []),
    [points, from, to],
  );
  const netbirdPct = useMemo(
    () => (from && to ? bucketUptime(points, from, to, bucket, netbirdIsUp) : []),
    [points, from, to, bucket],
  );

  const times = useMemo(() => {
    if (!from || !to) return [];
    const bucketMs = bucket * 1000;
    const f = new Date(from).getTime();
    return uptimePct.map((_, i) => new Date(f + i * bucketMs).toLocaleString());
  }, [from, to, bucket, uptimePct]);

  const avg = (arr: number[]) => (arr.length ? arr.reduce((a, b) => a + b, 0) / arr.length : 0);

  const controls = (
    <Box sx={{ display: 'flex', flexWrap: 'wrap', gap: 1.5, alignItems: 'center' }}>
      <Box sx={{ minWidth: 140 }}>
        <SelectField
          id="avail-window"
          label={t.activityTsWindowLabel}
          value={win}
          onChange={(e) => setWin(e.target.value as TsWindow)}
        >
          {TS_WINDOWS.map((w) => (
            <option key={w} value={w}>
              {formatTsSeconds(TS_WINDOW_SECONDS[w], t)}
            </option>
          ))}
        </SelectField>
      </Box>
      <Box sx={{ minWidth: 140 }}>
        <SelectField
          id="avail-bucket"
          label={t.activityTsBucketLabel}
          value={String(bucket)}
          onChange={(e) => setBucket(Number(e.target.value) as TsBucket)}
        >
          {TS_BUCKETS.map((b) => (
            <option key={b} value={String(b)}>
              {formatTsSeconds(b, t)}
            </option>
          ))}
        </SelectField>
      </Box>
      <Button type="button" variant="outlined" size="small" onClick={() => setLive((v) => !v)}>
        {live ? t.serverPerfLive : t.serverPerfPaused}
      </Button>
    </Box>
  );

  const empty = !pending && points.length === 0;
  const showNetbird = server.netbird_peer_id !== '';

  return (
    <Panel titleId="availability-heading" title={server.name} actions={controls}>
      {empty ? (
        <Typography color="text.secondary">{t.availabilityNoData}</Typography>
      ) : (
        <Box sx={{ display: 'flex', flexDirection: 'column', gap: 2 }}>
          <UptimeTimeline
            t={t}
            title={t.availabilityHealthTimeline}
            segments={healthSegs}
            windowFrom={from}
            windowTo={to}
            colorForState={colorHealth}
            labelForState={labelHealth}
          />
          <UptimeTimeline
            t={t}
            title={t.availabilityAgentTimeline}
            segments={agentSegs}
            windowFrom={from}
            windowTo={to}
            colorForState={colorAgent}
            labelForState={labelAgent}
          />
          {showNetbird && (
            <UptimeTimeline
              t={t}
              title={t.availabilityNetbirdTimeline}
              segments={netbirdSegs}
              windowFrom={from}
              windowTo={to}
              colorForState={colorNetbird}
              labelForState={labelNetbird}
            />
          )}
          <Typography variant="body2" color="text.secondary">
            {t.availabilityUptimeSummary(fmtPct(avg(uptimePct)))} ·{' '}
            {t.availabilityAgentSummary(fmtPct(avg(agentPct)))}
            {showNetbird ? ` · ${t.availabilityNetbirdSummary(fmtPct(avg(netbirdPct)))}` : ''}
          </Typography>
          <Box
            sx={{
              display: 'grid',
              gridTemplateColumns: { xs: '1fr', md: 'repeat(2, minmax(0, 1fr))' },
              gap: 2,
            }}
          >
            <LineChart
              t={t}
              title={t.availabilityUptimeChart}
              unit={t.serverPerfUnitPct}
              times={times}
              series={[
                { label: t.availabilityUptimeChart, color: chartColor(0), values: uptimePct },
              ]}
            />
            <LineChart
              t={t}
              title={t.availabilityAgentChart}
              unit={t.serverPerfUnitPct}
              times={times}
              series={[{ label: t.availabilityAgentChart, color: chartColor(1), values: agentPct }]}
            />
            {showNetbird && (
              <LineChart
                t={t}
                title={t.availabilityNetbirdChart}
                unit={t.serverPerfUnitPct}
                times={times}
                series={[
                  { label: t.availabilityNetbirdChart, color: chartColor(2), values: netbirdPct },
                ]}
              />
            )}
          </Box>
        </Box>
      )}
    </Panel>
  );
}
