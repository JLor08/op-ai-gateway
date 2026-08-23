// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { act, cleanup, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  PerformanceSection,
  gpuSeries,
  cpuCoreSeries,
  netRateSeries,
  pct,
  gpuColorAt,
  THROUGHPUT_POLL_MS,
} from './PerformanceSection';
import { chartColor } from './chartPalette';
import { messages } from '../i18n';
import type { PerfGPU, PerfHistory, PerfPoint, PortalServer, TimeSeries } from '../api';

const t = messages.de;

function makeGpu(over: Partial<PerfGPU> = {}): PerfGPU {
  return {
    index: 0,
    name: 'E2E-PERF-GPU',
    uuid: 'GPU-e2e-0',
    util_pct: 63,
    mem_used_bytes: 8000,
    mem_total_bytes: 16000,
    temp_c: 58,
    vram_temp_c: 62,
    power_w: 90,
    fan_pct: 35,
    ...over,
  };
}

function makePoint(iso: string, over: Partial<PerfPoint> = {}): PerfPoint {
  return {
    t: iso,
    cpu_util_pct: 20,
    cpu_cores: [10, 30],
    mem_used_bytes: 4_000_000,
    mem_total_bytes: 16_000_000,
    swap_used_bytes: 0,
    swap_total_bytes: 2_000_000,
    load1: 1.2,
    load5: 1.1,
    load15: 1.0,
    active_requests: 1,
    queue_depth: 0,
    gpus: [makeGpu()],
    net: [{ name: 'eth0', rx_bytes: 1000, tx_bytes: 500 }],
    cpu_power_w: null,
    system_power_w: null,
    cpu_temp_c: null,
    ...over,
  };
}

function makeHistory(points: PerfPoint[]): PerfHistory {
  return { points, from: points[0]?.t ?? '', to: points.at(-1)?.t ?? '' };
}

function makeTs(): TimeSeries {
  return {
    points: [
      {
        t: '2026-07-19T12:00:00Z',
        connections: 1,
        concurrency: 1,
        prompt_tokens_per_second: 4,
        completion_tokens_per_second: 7,
      },
      {
        t: '2026-07-19T12:00:05Z',
        connections: 2,
        concurrency: 1,
        prompt_tokens_per_second: 5,
        completion_tokens_per_second: 8,
      },
    ],
    bucket_seconds: 5,
    from: '2026-07-19T11:55:00Z',
    to: '2026-07-19T12:00:05Z',
  };
}

function makeServer(over: Partial<PortalServer> = {}): PortalServer {
  return {
    id: 'srv_1',
    name: 'Mock Server',
    domain: 'mock.test',
    server_path_suffix: '',
    netbird_enabled: false,
    netbird_setup_key_id: '',
    netbird_group_id: '',
    netbird_peer_id: '',
    netbird_connected: false,
    netbird_group_ids: [],
    netbird_peer_managed: false,
    netbird_policy_override: '',
    netbird_allow_ping: false,
    netbird_ping_exclude: false,
    status: 'active',
    health_status: 'healthy',
    owners: [],
    last_seen_at: '2026-07-19T12:00:10Z',
    created_at: '2026-07-19T10:00:00Z',
    agent_status: 'unconfigured',
    agent_presence_timeout_seconds: 0,
    estimated_watts: 0,
    idle_watts: 0,
    price_per_kwh: 0,
    pue: 0,
    price_unit: 'eur_cent',
    admin_groups: [],
    system_group_id: '',
    system_group_name: '',
    ...over,
  };
}

type Captured = {
  onSnapshot: (points: PerfPoint[]) => void;
  onSample: (point: PerfPoint) => void;
  onStatus?: (s: 'open' | 'error') => void;
};

function makeApi(
  over: {
    history?: PerfHistory;
    ts?: TimeSeries;
  } = {},
) {
  const captured: Partial<Captured> = {};
  const unsubscribe = vi.fn();
  const subscribeServerPerf = vi.fn(
    (
      _id: string,
      onSnapshot: (points: PerfPoint[]) => void,
      onSample: (point: PerfPoint) => void,
      onStatus?: (s: 'open' | 'error') => void,
    ) => {
      captured.onSnapshot = onSnapshot;
      captured.onSample = onSample;
      captured.onStatus = onStatus;
      return unsubscribe;
    },
  );
  const api = {
    serverPerfHistory: vi.fn(
      async () =>
        over.history ??
        makeHistory([
          makePoint('2026-07-19T12:00:00Z'),
          makePoint('2026-07-19T12:00:05Z'),
          makePoint('2026-07-19T12:00:10Z'),
        ]),
    ),
    usageTimeSeries: vi.fn(async () => over.ts ?? makeTs()),
    subscribeServerPerf,
  };
  return { api, captured, unsubscribe };
}

afterEach(() => cleanup());

describe('PerformanceSection helpers', () => {
  it('gpuSeries computes mem_pct with a divide-by-zero guard', () => {
    const points: PerfPoint[] = [
      makePoint('2026-07-19T12:00:00Z', {
        gpus: [makeGpu({ mem_used_bytes: 4000, mem_total_bytes: 16000 })],
      }),
      makePoint('2026-07-19T12:00:05Z', {
        gpus: [makeGpu({ mem_used_bytes: 8000, mem_total_bytes: 16000 })],
      }),
      makePoint('2026-07-19T12:00:10Z', {
        gpus: [makeGpu({ mem_used_bytes: 500, mem_total_bytes: 0 })],
      }),
    ];
    const series = gpuSeries(points, 'mem_pct', gpuColorAt);
    expect(series).toHaveLength(1);
    expect(series[0].values).toEqual([25, 50, 0]);
    expect(series[0].color).toBe(gpuColorAt(0));
  });

  it('cpuCoreSeries builds one palette-colored line per core, sized to the latest point', () => {
    const points: PerfPoint[] = [
      makePoint('2026-07-19T12:00:00Z', { cpu_cores: [5, 15, 25, 35] }),
      makePoint('2026-07-19T12:00:05Z', { cpu_cores: [6, 16, 26, 36] }),
    ];
    const series = cpuCoreSeries(points);
    expect(series).toHaveLength(4);
    expect(series[0].label).toBe('Core 0');
    expect(series[3].label).toBe('Core 3');
    expect(series[0].color).toBe(chartColor(0));
    expect(series[2].color).toBe(chartColor(2));
    expect(series[1].values).toEqual([15, 16]);
    // A point missing a core index contributes 0 for it.
    const ragged = cpuCoreSeries([
      makePoint('2026-07-19T12:00:00Z', { cpu_cores: [1] }),
      makePoint('2026-07-19T12:00:05Z', { cpu_cores: [2, 9] }),
    ]);
    expect(ragged).toHaveLength(2); // sized to the latest (2 cores)
    expect(ragged[1].values).toEqual([0, 9]); // first point had no core 1
  });

  it('cpuCoreSeries returns [] when there are no cores', () => {
    expect(cpuCoreSeries([makePoint('2026-07-19T12:00:00Z', { cpu_cores: [] })])).toEqual([]);
    expect(cpuCoreSeries([])).toEqual([]);
  });

  it('gpuSeries computes mem_mib from mem_used_bytes', () => {
    const points: PerfPoint[] = [
      makePoint('2026-07-19T12:00:00Z', { gpus: [makeGpu({ mem_used_bytes: 1024 * 1024 })] }),
      makePoint('2026-07-19T12:00:05Z', {
        gpus: [makeGpu({ mem_used_bytes: 8 * 1024 * 1024 + 512 * 1024 })],
      }),
    ];
    const series = gpuSeries(points, 'mem_mib', gpuColorAt);
    expect(series).toHaveLength(1);
    expect(series[0].values).toEqual([1, 8.5]);
  });

  it('gpuSeries returns [] when the latest point has no GPUs', () => {
    const points = [makePoint('2026-07-19T12:00:00Z', { gpus: [] })];
    expect(gpuSeries(points, 'util_pct', gpuColorAt)).toEqual([]);
  });

  it('netRateSeries computes B/s deltas, zeroes the first point and counter resets', () => {
    const points: PerfPoint[] = [
      makePoint('2026-07-19T12:00:00Z', { net: [{ name: 'eth0', rx_bytes: 1000, tx_bytes: 0 }] }),
      makePoint('2026-07-19T12:00:05Z', { net: [{ name: 'eth0', rx_bytes: 6000, tx_bytes: 0 }] }),
      makePoint('2026-07-19T12:00:10Z', { net: [{ name: 'eth0', rx_bytes: 3000, tx_bytes: 0 }] }),
    ];
    // first -> 0; (6000-1000)/5s = 1000 B/s; reset (3000 < 6000) -> 0.
    expect(netRateSeries(points, 'rx_bytes')).toEqual([0, 1000, 0]);
  });

  it('pct guards divide-by-zero', () => {
    expect(pct(5, 0)).toBe(0);
    expect(pct(5, 10)).toBe(50);
  });
});

describe('PerformanceSection', () => {
  it('renders the GPU utilization chart from the fetched history', async () => {
    const { api } = makeApi();
    render(<PerformanceSection t={t} api={api} server={makeServer()} />);
    expect(await screen.findByRole('img', { name: t.serverPerfGpuUtil })).toBeInTheDocument();
  });

  it('renders the VRAM-MB chart alongside the VRAM-% chart', async () => {
    const { api } = makeApi();
    render(<PerformanceSection t={t} api={api} server={makeServer()} />);
    expect(await screen.findByRole('img', { name: t.serverPerfGpuVram })).toBeInTheDocument();
    expect(screen.getByRole('img', { name: t.serverPerfGpuVramMb })).toBeInTheDocument();
  });

  it('renders the per-core CPU chart when the samples carry cpu_cores', async () => {
    const { api } = makeApi({
      history: makeHistory([
        makePoint('2026-07-19T12:00:00Z', { cpu_cores: [10, 20, 30, 40] }),
        makePoint('2026-07-19T12:00:05Z', { cpu_cores: [11, 21, 31, 41] }),
      ]),
    });
    render(<PerformanceSection t={t} api={api} server={makeServer()} />);
    expect(await screen.findByRole('img', { name: t.serverPerfCpuCores })).toBeInTheDocument();
  });

  it('appends a live sample from the SSE callback', async () => {
    const { api, captured } = makeApi();
    render(<PerformanceSection t={t} api={api} server={makeServer()} />);
    // history loads 3 points.
    await screen.findByRole('img', { name: t.serverPerfGpuUtil });
    expect(screen.getByTestId('perf-point-count')).toHaveTextContent('3');

    act(() => captured.onSample!(makePoint('2026-07-19T12:00:15Z')));
    expect(screen.getByTestId('perf-point-count')).toHaveTextContent('4');
  });

  it('shows the no-agent empty state when there is no history and the server never reported', async () => {
    const { api } = makeApi({ history: makeHistory([]) });
    render(<PerformanceSection t={t} api={api} server={makeServer({ last_seen_at: null })} />);
    expect(await screen.findByText(t.serverPerfNoAgent)).toBeInTheDocument();
  });

  it('unsubscribes from the SSE on unmount', async () => {
    const { api, unsubscribe } = makeApi();
    const { unmount } = render(<PerformanceSection t={t} api={api} server={makeServer()} />);
    await screen.findByRole('img', { name: t.serverPerfGpuUtil });
    unmount();
    expect(unsubscribe).toHaveBeenCalled();
  });

  it('polls usageTimeSeries on an interval while live and clears it on unmount', async () => {
    vi.useFakeTimers();
    try {
      const { api } = makeApi();
      const { unmount } = render(<PerformanceSection t={t} api={api} server={makeServer()} />);
      const calls = () =>
        (api.usageTimeSeries as unknown as ReturnType<typeof vi.fn>).mock.calls.length;
      // Flush the mount fetches (history + the initial throughput fetch).
      await act(async () => {
        await Promise.resolve();
        await Promise.resolve();
      });
      const initial = calls();
      expect(initial).toBeGreaterThanOrEqual(1);

      // One poll interval -> one more usageTimeSeries call.
      await act(async () => {
        vi.advanceTimersByTime(THROUGHPUT_POLL_MS);
        await Promise.resolve();
      });
      expect(calls()).toBe(initial + 1);

      // Unmount clears the interval: no further polls.
      unmount();
      const afterUnmount = calls();
      await act(async () => {
        vi.advanceTimersByTime(THROUGHPUT_POLL_MS * 3);
        await Promise.resolve();
      });
      expect(calls()).toBe(afterUnmount);
    } finally {
      vi.useRealTimers();
    }
  });

  it('renders the CPU-power and System-power charts when the latest point carries them', async () => {
    const { api } = makeApi({
      history: makeHistory([
        makePoint('2026-07-19T12:00:00Z', { cpu_power_w: 60, system_power_w: 150 }),
        makePoint('2026-07-19T12:00:05Z', { cpu_power_w: 62, system_power_w: 155 }),
      ]),
    });
    render(<PerformanceSection t={t} api={api} server={makeServer()} />);
    expect(await screen.findByRole('img', { name: t.serverPerfCpuPower })).toBeInTheDocument();
    expect(screen.getByRole('img', { name: t.serverPerfSystemPower })).toBeInTheDocument();
  });

  it("omits the power charts when the latest point's power is null", async () => {
    const { api } = makeApi({
      history: makeHistory([
        makePoint('2026-07-19T12:00:00Z', { cpu_power_w: null, system_power_w: null }),
      ]),
    });
    render(<PerformanceSection t={t} api={api} server={makeServer()} />);
    await screen.findByRole('img', { name: t.serverPerfGpuUtil });
    expect(screen.queryByRole('img', { name: t.serverPerfCpuPower })).not.toBeInTheDocument();
    expect(screen.queryByRole('img', { name: t.serverPerfSystemPower })).not.toBeInTheDocument();
  });

  it('renders the CPU-temperature chart when the latest point carries cpu_temp_c', async () => {
    const { api } = makeApi({
      history: makeHistory([
        makePoint('2026-07-19T12:00:00Z', { cpu_temp_c: 55 }),
        makePoint('2026-07-19T12:00:05Z', { cpu_temp_c: 58.5 }),
      ]),
    });
    render(<PerformanceSection t={t} api={api} server={makeServer()} />);
    expect(await screen.findByRole('img', { name: t.serverPerfCpuTemp })).toBeInTheDocument();
  });

  it("omits the CPU-temperature chart when the latest point's cpu_temp_c is null or absent", async () => {
    const { api } = makeApi({
      history: makeHistory([makePoint('2026-07-19T12:00:00Z', { cpu_temp_c: null })]),
    });
    render(<PerformanceSection t={t} api={api} server={makeServer()} />);
    await screen.findByRole('img', { name: t.serverPerfGpuUtil });
    expect(screen.queryByRole('img', { name: t.serverPerfCpuTemp })).not.toBeInTheDocument();
  });
});
