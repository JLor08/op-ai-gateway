// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, it, expect, vi } from 'vitest';
import { AvailabilitySection, buildSegments, bucketUptime } from './AvailabilitySection';
import { messages } from '../i18n';
import type { AvailabilityPoint } from '../api';
import type { TimelineState } from './UptimeTimeline';

const t = messages.de;

// This file's config doesn't enable vitest `globals`, so @testing-library/react's
// auto-cleanup (which checks for a global `afterEach`) never registers; without an
// explicit unmount, each test's DOM lingers into the next. Mirrors the pattern
// already used by Activity.capture.test.tsx / Activity.active.test.tsx.
afterEach(cleanup);

// Health state mapper mirroring the component's own healthState (kept local so
// the pure helpers are tested independently of the component internals).
const healthOf = (p: AvailabilityPoint): TimelineState =>
  p.health === 'unhealthy'
    ? 'unhealthy'
    : p.health === 'degraded'
      ? 'degraded'
      : p.health === 'unknown' || p.health === ''
        ? 'unknown'
        : 'healthy';

describe('availability derivations', () => {
  const from = '2026-01-01T00:00:00Z';
  // The last sample (02:00) is within the 10-min trailing threshold of `to` so the
  // final segment holds its observed state rather than becoming an "unknown" tail.
  const to = '2026-01-01T02:05:00Z';
  const pts: AvailabilityPoint[] = [
    {
      t: '2026-01-01T00:00:00Z',
      health: 'healthy',
      reachable_count: 1,
      active_count: 1,
      agent_reporting: true,
      netbird_connected: false,
      gap_before: false,
    },
    {
      t: '2026-01-01T02:00:00Z',
      health: 'unhealthy',
      reachable_count: 0,
      active_count: 1,
      agent_reporting: true,
      netbird_connected: false,
      gap_before: false,
    },
  ];

  it('builds health segments spanning the window with the first/last states', () => {
    const segs = buildSegments(pts, from, to, healthOf);
    expect(segs.length).toBeGreaterThan(0);
    expect(segs[0].state).toBe('healthy');
    expect(segs[segs.length - 1].state).toBe('unhealthy');
    // The first segment starts at the window start (back-fill / first sample).
    expect(new Date(segs[0].start).getTime()).toBe(new Date(from).getTime());
    // The last segment ends at the window end.
    expect(new Date(segs[segs.length - 1].end).getTime()).toBe(new Date(to).getTime());
    // Segments are contiguous and forward in time.
    for (let i = 1; i < segs.length; i++) {
      expect(new Date(segs[i].start).getTime()).toBe(new Date(segs[i - 1].end).getTime());
      expect(new Date(segs[i].end).getTime()).toBeGreaterThan(new Date(segs[i].start).getTime());
    }
  });

  it("back-fills [from, first point) with the first sample's state", () => {
    const late: AvailabilityPoint[] = [
      {
        t: '2026-01-01T01:00:00Z',
        health: 'healthy',
        reachable_count: 1,
        active_count: 1,
        agent_reporting: true,
        netbird_connected: false,
        gap_before: false,
      },
      {
        t: '2026-01-01T01:30:00Z',
        health: 'healthy',
        reachable_count: 1,
        active_count: 1,
        agent_reporting: true,
        netbird_connected: false,
        gap_before: false,
      },
    ];
    const segs = buildSegments(late, from, to, healthOf);
    expect(new Date(segs[0].start).getTime()).toBe(new Date(from).getTime());
    expect(segs[0].state).toBe('healthy');
  });

  it('returns a single unknown segment when there are no points', () => {
    const segs = buildSegments([], from, to, healthOf);
    expect(segs).toHaveLength(1);
    expect(segs[0].state).toBe('unknown');
    expect(new Date(segs[0].start).getTime()).toBe(new Date(from).getTime());
    expect(new Date(segs[0].end).getTime()).toBe(new Date(to).getTime());
  });

  it('computes uptime percent per bucket over known time', () => {
    // up = not unhealthy; healthy [00:00,02:00) then unhealthy to `to` (02:05). Two
    // 2h buckets -> first all-up (~100), last all-down (~0), all known (no gap).
    const pct = bucketUptime(pts, from, to, 7200, (p) => p.health !== 'unhealthy');
    expect(pct).toHaveLength(2);
    expect(pct[0]).toBeGreaterThan(90);
    expect(pct[pct.length - 1]).toBeLessThan(10);
  });

  it('returns all-zero buckets with no points', () => {
    const pct = bucketUptime([], from, to, 7200, () => true);
    expect(pct).toHaveLength(2);
    expect(pct.every((v) => v === 0)).toBe(true);
  });

  // Observer-gap coverage: an interior span the backend flags gap_before (the gateway
  // was not sampling) must become an "unknown" gap, never a held-last-state run. The
  // backend sets gap_before on the post-gap sample (05:00, whose raw predecessor 00:10
  // is > the 10-min gap floor away); the trailing edge (05:05 -> gapTo 10:00) is stale
  // by > the 10-min threshold, so it is also unknown.
  const gapFrom = '2026-01-01T00:00:00Z';
  const gapTo = '2026-01-01T10:00:00Z';
  const gapPts: AvailabilityPoint[] = [
    {
      t: '2026-01-01T00:00:00Z',
      health: 'healthy',
      reachable_count: 1,
      active_count: 1,
      agent_reporting: true,
      netbird_connected: false,
      gap_before: false,
    },
    {
      t: '2026-01-01T00:05:00Z',
      health: 'healthy',
      reachable_count: 1,
      active_count: 1,
      agent_reporting: true,
      netbird_connected: false,
      gap_before: false,
    },
    {
      t: '2026-01-01T00:10:00Z',
      health: 'healthy',
      reachable_count: 1,
      active_count: 1,
      agent_reporting: true,
      netbird_connected: false,
      gap_before: false,
    },
    {
      t: '2026-01-01T05:00:00Z',
      health: 'healthy',
      reachable_count: 1,
      active_count: 1,
      agent_reporting: true,
      netbird_connected: false,
      gap_before: true,
    },
    {
      t: '2026-01-01T05:05:00Z',
      health: 'healthy',
      reachable_count: 1,
      active_count: 1,
      agent_reporting: true,
      netbird_connected: false,
      gap_before: false,
    },
  ];

  it('inserts an unknown gap segment for an interior observer gap and a trailing gap to `to`', () => {
    const segs = buildSegments(gapPts, gapFrom, gapTo, healthOf);
    // Interior observer gap between the 00:10 sample and the 05:00 sample.
    const interior = segs.find(
      (s) =>
        s.state === 'unknown' &&
        new Date(s.start).getTime() === new Date('2026-01-01T00:10:00Z').getTime(),
    );
    expect(interior).toBeDefined();
    expect(new Date(interior!.end).getTime()).toBe(new Date('2026-01-01T05:00:00Z').getTime());
    // Trailing observer gap: the last sample (05:05) is stale relative to `to`.
    const last = segs[segs.length - 1];
    expect(last.state).toBe('unknown');
    expect(new Date(last.end).getTime()).toBe(new Date(gapTo).getTime());
    // Every segment is contiguous, positive width, and forward in time.
    for (let i = 1; i < segs.length; i++) {
      expect(new Date(segs[i].start).getTime()).toBe(new Date(segs[i - 1].end).getTime());
      expect(new Date(segs[i].end).getTime()).toBeGreaterThan(new Date(segs[i].start).getTime());
    }
  });

  it('excludes observer-gap time from the uptime denominator (no hold-last-state inflation)', () => {
    // Dense up samples (00:00→00:10), then a 100-min observer gap the backend flags on
    // the post-gap down sample (01:50, gap_before=true), which runs to `to` (02:00).
    // One 2h bucket. Known time = 10 min up ([00:00,00:10)) + 10 min down ([01:50,02:00),
    // 10 min from `to` so not stale) = 20 min → 50%. A naive hold-last-state would count
    // the gap as up → (10+100)/120 ≈ 91.7%. The gap_before interval is excluded from BOTH
    // numerator and denominator, so the bucket reads ~50%, well under the hold-last value.
    const denomFrom = '2026-01-01T00:00:00Z';
    const denomTo = '2026-01-01T02:00:00Z';
    const denomPts: AvailabilityPoint[] = [
      {
        t: '2026-01-01T00:00:00Z',
        health: 'healthy',
        reachable_count: 1,
        active_count: 1,
        agent_reporting: true,
        netbird_connected: false,
        gap_before: false,
      },
      {
        t: '2026-01-01T00:05:00Z',
        health: 'healthy',
        reachable_count: 1,
        active_count: 1,
        agent_reporting: true,
        netbird_connected: false,
        gap_before: false,
      },
      {
        t: '2026-01-01T00:10:00Z',
        health: 'healthy',
        reachable_count: 1,
        active_count: 1,
        agent_reporting: true,
        netbird_connected: false,
        gap_before: false,
      },
      {
        t: '2026-01-01T01:50:00Z',
        health: 'unhealthy',
        reachable_count: 0,
        active_count: 1,
        agent_reporting: true,
        netbird_connected: false,
        gap_before: true,
      },
    ];
    const pct = bucketUptime(denomPts, denomFrom, denomTo, 7200, (p) => p.health !== 'unhealthy');
    expect(pct).toHaveLength(1);
    expect(pct[0]).toBeGreaterThan(40);
    expect(pct[0]).toBeLessThan(60);
  });

  // REGRESSION (the fixed MAJOR): a collapsed multi-hour CONTINUOUS same-state run is
  // same-state points hours apart WITHOUT gap_before (the gateway WAS sampling, the
  // reduction just dropped the redundant heartbeats). The old median-spacing heuristic
  // (adaptiveGap = max(10min, 3×median)) saw two 5-min blips drop the median to ~5min →
  // threshold ~15min → every collapsed hours-apart run exceeded it and was painted grey
  // ("unknown") → a mostly-healthy day read ~0% uptime. With the authoritative gap_before
  // flag (all false here — no observer gap), those runs stay HEALTHY. This fixture is the
  // reduced backend output for a ~24h day sampled every 5 min with a 5-min unhealthy blip
  // at 08:00 and at 16:00. It MUST FAIL against the old median code and PASS with the fix.
  const dayFrom = '2026-01-01T00:00:00Z';
  const dayTo = '2026-01-02T00:00:00Z';
  const dayPts: AvailabilityPoint[] = [
    {
      t: '2026-01-01T00:00:00Z',
      health: 'healthy',
      reachable_count: 1,
      active_count: 1,
      agent_reporting: true,
      netbird_connected: false,
      gap_before: false,
    }, // day start
    {
      t: '2026-01-01T07:55:00Z',
      health: 'healthy',
      reachable_count: 1,
      active_count: 1,
      agent_reporting: true,
      netbird_connected: false,
      gap_before: false,
    }, // run end (pre-blip1)
    {
      t: '2026-01-01T08:00:00Z',
      health: 'unhealthy',
      reachable_count: 0,
      active_count: 1,
      agent_reporting: true,
      netbird_connected: false,
      gap_before: false,
    }, // blip1
    {
      t: '2026-01-01T08:05:00Z',
      health: 'healthy',
      reachable_count: 1,
      active_count: 1,
      agent_reporting: true,
      netbird_connected: false,
      gap_before: false,
    }, // recovery
    {
      t: '2026-01-01T15:55:00Z',
      health: 'healthy',
      reachable_count: 1,
      active_count: 1,
      agent_reporting: true,
      netbird_connected: false,
      gap_before: false,
    }, // run end (pre-blip2)
    {
      t: '2026-01-01T16:00:00Z',
      health: 'unhealthy',
      reachable_count: 0,
      active_count: 1,
      agent_reporting: true,
      netbird_connected: false,
      gap_before: false,
    }, // blip2
    {
      t: '2026-01-01T16:05:00Z',
      health: 'healthy',
      reachable_count: 1,
      active_count: 1,
      agent_reporting: true,
      netbird_connected: false,
      gap_before: false,
    }, // recovery
    {
      t: '2026-01-01T23:55:00Z',
      health: 'healthy',
      reachable_count: 1,
      active_count: 1,
      agent_reporting: true,
      netbird_connected: false,
      gap_before: false,
    }, // newest heartbeat (5 min before `to`)
  ];

  it('does not paint collapsed continuous same-state runs as gaps (false-gap regression)', () => {
    const segs = buildSegments(dayPts, dayFrom, dayTo, healthOf);
    // No collapsed healthy run is misread as an observer gap.
    expect(segs.some((s) => s.state === 'unknown')).toBe(false);
    // Exactly the two 5-minute unhealthy blips remain as unhealthy segments; the rest is healthy.
    expect(segs.filter((s) => s.state === 'unhealthy')).toHaveLength(2);
    expect(segs.every((s) => s.state === 'healthy' || s.state === 'unhealthy')).toBe(true);
  });

  it('reads ~100% uptime for the non-blip hours of a mostly-healthy day (not ~0%)', () => {
    const pct = bucketUptime(dayPts, dayFrom, dayTo, 3600, (p) => p.health !== 'unhealthy');
    expect(pct).toHaveLength(24);
    // Hours entirely within a collapsed healthy run must read full uptime, NOT 0% (the
    // old code excluded them as "unknown gap" → knownMs=0 → 0%).
    for (const hour of [3, 12, 20]) {
      expect(pct[hour]).toBeGreaterThan(95);
    }
    // The two blip hours are still measured as degraded (a 5-min down in a 60-min bucket),
    // proving the blips are not lost — high but below full.
    expect(pct[8]).toBeGreaterThan(85);
    expect(pct[8]).toBeLessThan(99);
    expect(pct[16]).toBeGreaterThan(85);
    expect(pct[16]).toBeLessThan(99);
  });
});

describe('AvailabilitySection', () => {
  it('renders timelines + charts from fetched data', async () => {
    const api = {
      serverAvailability: vi.fn().mockResolvedValue({
        points: [
          {
            t: '2026-01-01T00:00:00Z',
            health: 'healthy',
            reachable_count: 1,
            active_count: 1,
            agent_reporting: true,
          },
          {
            t: '2026-01-01T00:30:00Z',
            health: 'unhealthy',
            reachable_count: 0,
            active_count: 1,
            agent_reporting: false,
          },
        ],
        from: '2026-01-01T00:00:00Z',
        to: '2026-01-01T01:00:00Z',
      }),
    } as any;
    const server = { id: 's1', name: 'srv', last_seen_at: '2026-01-01T00:59:00Z' } as any;
    render(<AvailabilitySection t={t} api={api} server={server} />);
    await waitFor(() =>
      expect(api.serverAvailability).toHaveBeenCalledWith('s1', expect.any(String)),
    );
    expect(await screen.findByText(t.availabilityHealthTimeline)).toBeInTheDocument();
    expect(screen.getByText(t.availabilityAgentTimeline)).toBeInTheDocument();
    expect(screen.getByText(t.availabilityUptimeChart)).toBeInTheDocument();
    expect(screen.getByText(t.availabilityAgentChart)).toBeInTheDocument();
  });

  it('shows the empty state when no points are returned', async () => {
    const api = {
      serverAvailability: vi.fn().mockResolvedValue({
        points: [],
        from: '2026-01-01T00:00:00Z',
        to: '2026-01-01T01:00:00Z',
      }),
    } as any;
    const server = { id: 's2', name: 'srv2', last_seen_at: null } as any;
    render(<AvailabilitySection t={t} api={api} server={server} />);
    expect(await screen.findByText(t.availabilityNoData)).toBeInTheDocument();
  });

  // TestAvailabilitySectionDegradedUnhealthyAbsentSegments (naming mirrors the
  // Go convention used elsewhere in this repo's test suites): proves the
  // degraded/unhealthy/absent color+label mappings actually reach the
  // rendered timeline, not just the healthy/unknown paths the two tests
  // above exercise. The window here is deliberately tight (from == the
  // first point, to == the last point, no point more than GAP_THRESHOLD_MS
  // stale) so every interior segment holds its point's real state instead
  // of collapsing to "unknown" — the earlier "renders timelines..." test's
  // sole unhealthy point falls OUTSIDE that threshold and so is painted
  // "unknown" for its trailing segment, never actually reaching colorHealth/
  // labelHealth('unhealthy').
  it('colors and labels degraded/unhealthy health and absent agent segments', async () => {
    const history = {
      points: [
        {
          t: '2026-02-01T00:00:00Z',
          health: 'healthy',
          reachable_count: 1,
          active_count: 1,
          agent_reporting: true,
          netbird_connected: false,
          gap_before: false,
        },
        {
          t: '2026-02-01T00:10:00Z',
          health: 'degraded',
          reachable_count: 1,
          active_count: 1,
          agent_reporting: false,
          netbird_connected: false,
          gap_before: false,
        },
        {
          t: '2026-02-01T00:20:00Z',
          health: 'unhealthy',
          reachable_count: 0,
          active_count: 1,
          agent_reporting: true,
          netbird_connected: false,
          gap_before: false,
        },
        {
          t: '2026-02-01T00:30:00Z',
          health: 'healthy',
          reachable_count: 1,
          active_count: 1,
          agent_reporting: true,
          netbird_connected: false,
          gap_before: false,
        },
      ],
      from: '2026-02-01T00:00:00Z',
      to: '2026-02-01T00:30:00Z',
    };
    const api = { serverAvailability: vi.fn().mockResolvedValue(history) } as any;
    const server = { id: 's3', name: 'srv3', netbird_peer_id: '', last_seen_at: null } as any;
    render(<AvailabilitySection t={t} api={api} server={server} />);

    // Each UptimeTimeline instance's title Typography and its <svg> are
    // direct siblings inside the SAME wrapper Box, so locating the svg via
    // the title text (rather than a brittle document-wide positional index,
    // which an unrelated icon <svg> elsewhere in the panel could shift) is
    // robust regardless of what else the panel renders.
    const svgNear = (title: string): SVGElement => {
      const el = screen.getByText(title).parentElement?.querySelector('svg');
      if (!el) throw new Error(`no <svg> found next to title ${title}`);
      return el;
    };
    const healthSvg = await waitFor(() => svgNear(t.availabilityHealthTimeline));
    const agentSvg = svgNear(t.availabilityAgentTimeline);

    const healthFills = Array.from(healthSvg.querySelectorAll('[data-segment]')).map((el) =>
      el.getAttribute('fill'),
    );
    expect(healthFills).toContain('var(--watch-bg, #ed6c02)'); // degraded
    expect(healthFills).toContain('#d32f2f'); // unhealthy

    const agentFills = Array.from(agentSvg.querySelectorAll('[data-segment]')).map((el) =>
      el.getAttribute('fill'),
    );
    expect(agentFills).toContain('#d32f2f'); // absent

    // Hovering each colored segment surfaces its label text in the tooltip —
    // proving colorForState and labelForState agree on which segment is which.
    const degradedRect = healthSvg.querySelector('[fill="var(--watch-bg, #ed6c02)"]');
    expect(degradedRect).not.toBeNull();
    fireEvent.mouseEnter(degradedRect as Element);
    expect(await screen.findByText(new RegExp(t.availabilityStateDegraded))).toBeInTheDocument();
    fireEvent.mouseLeave(degradedRect as Element);

    const unhealthyRect = healthSvg.querySelector('[fill="#d32f2f"]');
    expect(unhealthyRect).not.toBeNull();
    fireEvent.mouseEnter(unhealthyRect as Element);
    expect(await screen.findByText(new RegExp(t.availabilityStateUnhealthy))).toBeInTheDocument();
    fireEvent.mouseLeave(unhealthyRect as Element);

    const absentRect = agentSvg.querySelector('[fill="#d32f2f"]');
    expect(absentRect).not.toBeNull();
    fireEvent.mouseEnter(absentRect as Element);
    expect(await screen.findByText(new RegExp(t.availabilityStateAbsent))).toBeInTheDocument();
    fireEvent.mouseLeave(absentRect as Element);
  });
});

describe('AvailabilitySection NetBird gate', () => {
  const history = {
    points: [
      {
        t: '2026-01-01T00:00:00Z',
        health: 'healthy',
        reachable_count: 1,
        active_count: 1,
        agent_reporting: true,
        netbird_connected: true,
        gap_before: false,
      },
      {
        t: '2026-01-01T00:30:00Z',
        health: 'healthy',
        reachable_count: 1,
        active_count: 1,
        agent_reporting: true,
        netbird_connected: false,
        gap_before: false,
      },
    ],
    from: '2026-01-01T00:00:00Z',
    to: '2026-01-01T01:00:00Z',
  };

  it('shows the NetBird timeline + chart when the server has a linked peer', async () => {
    const api = { serverAvailability: vi.fn().mockResolvedValue(history) } as any;
    const server = { id: 's1', name: 'srv', netbird_peer_id: 'peer-1', last_seen_at: null } as any;
    render(<AvailabilitySection t={t} api={api} server={server} />);
    expect(await screen.findByText(t.availabilityNetbirdTimeline)).toBeInTheDocument();
    expect(screen.getAllByText(t.availabilityNetbirdChart).length).toBeGreaterThan(0);
  });

  it('hides the NetBird graphs when the server has no linked peer', async () => {
    const api = { serverAvailability: vi.fn().mockResolvedValue(history) } as any;
    const server = { id: 's2', name: 'srv2', netbird_peer_id: '', last_seen_at: null } as any;
    render(<AvailabilitySection t={t} api={api} server={server} />);
    // Wait for data to load (health timeline always renders) before asserting absence.
    expect(await screen.findByText(t.availabilityHealthTimeline)).toBeInTheDocument();
    expect(screen.queryByText(t.availabilityNetbirdTimeline)).toBeNull();
    expect(screen.queryAllByText(t.availabilityNetbirdChart)).toHaveLength(0);
  });
});
