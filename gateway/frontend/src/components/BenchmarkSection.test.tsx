// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { BenchmarkSection, type BenchmarkScope } from './BenchmarkSection';
import { messages } from '../i18n';
import {
  PortalApiError,
  type BenchmarkRunDTO,
  type BenchmarkStatus,
  type PortalApplication,
  type PortalModelMapping,
  type PortalServer,
} from '../api';
import type { PortalApi } from './shared/types';

const t = messages.de;

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
    last_seen_at: '2026-07-24T12:00:10Z',
    created_at: '2026-07-24T10:00:00Z',
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

function makeApp(over: Partial<PortalApplication> = {}): PortalApplication {
  return {
    id: 'app_1',
    server_id: 'srv_1',
    type: 'vllm',
    port: 8000,
    scheme: 'https',
    endpoint: 'https://s1.example.test:8000',
    api_flavors: ['openai'],
    priority: 0,
    weight: 0,
    timeout_ms: 30000,
    affinity_ttl_seconds: 1800,
    admission_queue_timeout_seconds: 0,
    status: 'active',
    always_reachable: false,
    health_check_path: '/v1/health',
    health_check_mode: 'health_path',
    health_check_interval_seconds: 0,
    native_responses: false,
    native_messages: false,
    loaded_models_path: '',
    loaded_models_format: '',
    context_probe_path: '',
    app_path_suffix: '',
    api_token_set: false,
    api_token_header: '',
    benchmark_schedule_enabled: false,
    benchmark_schedule_interval_seconds: 0,
    opportunistic_metrics_enabled: false,
    proxy_listen_port: 0,
    reachable: true,
    last_checked_at: '2026-07-24T12:00:00Z',
    created_at: '2026-07-24T12:00:00Z',
    ...over,
  };
}

function makeMapping(over: Partial<PortalModelMapping> = {}): PortalModelMapping {
  return {
    id: 'map_1',
    application_id: 'app_1',
    gateway_model_name: 'gw-model',
    app_model_name: 'app-model',
    status: 'active',
    created_at: '2026-07-24T12:00:00Z',
    gen_tokens_per_second: 0,
    prompt_tokens_per_second: 0,
    load_time_ms: 0,
    context_size: 0,
    is_mtp: false,
    vision_capable: false,
    energy_wh_per_token: 0,
    metrics_locked: false,
    metrics_source: '',
    metrics_updated_at: null,
    max_concurrency: 0,
    recommended_concurrency: 0,
    gen_tokens_per_second_at_capacity: 0,
    ...over,
  };
}

const idle: BenchmarkStatus = {
  running: false,
  server_id: 'srv_1',
  scope: 'server',
  total: 0,
  done: 0,
};

type Overrides = {
  apps?: PortalApplication[];
  mappings?: PortalModelMapping[];
  runs?: BenchmarkRunDTO[];
  benchmarkServer?: PortalApi['benchmarkServer'];
  benchmarkApplication?: PortalApi['benchmarkApplication'];
  benchmarkMapping?: PortalApi['benchmarkMapping'];
  benchmarkStatus?: PortalApi['benchmarkStatus'];
  subscribeBenchmark?: PortalApi['subscribeBenchmark'];
};

// A fake api providing every method BenchmarkSection touches. `subscribe` captures
// the onStatus callback so a test can drive live frames; `applications`/`mappings`
// always resolve (the mount effects call them unconditionally).
function makeApi(over: Overrides = {}) {
  const captured: { onStatus?: (s: BenchmarkStatus) => void } = {};
  const unsubscribe = vi.fn();
  const subscribeBenchmark =
    over.subscribeBenchmark ??
    (vi.fn((_id: string, onStatus: (s: BenchmarkStatus) => void) => {
      captured.onStatus = onStatus;
      return unsubscribe;
    }) as unknown as PortalApi['subscribeBenchmark']);

  const api = {
    applications: vi.fn(async () => ({ data: over.apps ?? [] })),
    mappings: vi.fn(async () => ({ data: over.mappings ?? [] })),
    mappingBenchmarks: vi.fn(async () => over.runs ?? []),
    benchmarkServer:
      over.benchmarkServer ?? (vi.fn(async () => idle) as unknown as PortalApi['benchmarkServer']),
    benchmarkApplication:
      over.benchmarkApplication ??
      (vi.fn(async () => idle) as unknown as PortalApi['benchmarkApplication']),
    benchmarkMapping:
      over.benchmarkMapping ??
      (vi.fn(async () => idle) as unknown as PortalApi['benchmarkMapping']),
    // Never resolves by default so a running frame keeps the live panel visible
    // (completion tests override it).
    benchmarkStatus:
      over.benchmarkStatus ??
      (vi.fn(
        () => new Promise<BenchmarkStatus>(() => {}),
      ) as unknown as PortalApi['benchmarkStatus']),
    subscribeBenchmark,
  };
  return { api, captured, unsubscribe };
}

// Open a non-native MUI Select (by its combobox label) and click one option.
async function pickOption(comboLabel: string, optionText: string) {
  fireEvent.mouseDown(screen.getByRole('combobox', { name: comboLabel }));
  fireEvent.click(await screen.findByRole('option', { name: optionText }));
}

function renderSection(
  scope: BenchmarkScope,
  over: Overrides = {},
  extra: { onModelsChanged?: () => void; pollIntervalMs?: number } = {},
) {
  const { api, captured, unsubscribe } = makeApi(over);
  render(
    <BenchmarkSection
      t={t}
      api={api}
      server={makeServer()}
      initialScope={scope}
      onModelsChanged={extra.onModelsChanged}
      pollIntervalMs={extra.pollIntervalMs}
    />,
  );
  return { api, captured, unsubscribe };
}

afterEach(cleanup);

describe('BenchmarkSection', () => {
  it('shows the start form (scope + type + Start) when no run is active', async () => {
    renderSection({ kind: 'server' });
    expect(await screen.findByRole('combobox', { name: t.benchmarkScope })).toBeInTheDocument();
    expect(screen.getByRole('combobox', { name: t.benchmarkType })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: t.benchmarkStart })).toBeInTheDocument();
  });

  it('includes a vision option in the type selector', async () => {
    renderSection({ kind: 'server' });
    await screen.findByRole('combobox', { name: t.benchmarkType });
    fireEvent.mouseDown(screen.getByRole('combobox', { name: t.benchmarkType }));
    expect(await screen.findByRole('option', { name: t.benchmarkTypeVision })).toBeInTheDocument();
  });

  it("starts a server+speed run via benchmarkServer(id, 'speed')", async () => {
    const benchmarkServer = vi.fn(async () => idle) as unknown as PortalApi['benchmarkServer'];
    renderSection({ kind: 'server' }, { benchmarkServer });
    await screen.findByRole('button', { name: t.benchmarkStart });
    fireEvent.click(screen.getByRole('button', { name: t.benchmarkStart }));
    await waitFor(() => expect(benchmarkServer).toHaveBeenCalledWith('srv_1', 'speed'));
  });

  it("starts an application+capacity run via benchmarkApplication(appId, 'capacity')", async () => {
    const benchmarkApplication = vi.fn(
      async () => idle,
    ) as unknown as PortalApi['benchmarkApplication'];
    renderSection(
      { kind: 'server' },
      { apps: [makeApp({ id: 'app_1', endpoint: 'https://one.test:8000' })], benchmarkApplication },
    );
    await screen.findByRole('combobox', { name: t.benchmarkScope });
    await pickOption(t.benchmarkScope, t.benchmarkScopeApplication);
    // Choose the app explicitly (also proves the apps loaded), then the type.
    await pickOption(t.benchmarkScopeApplication, 'https://one.test:8000');
    await pickOption(t.benchmarkType, t.benchmarkTypeCapacity);
    fireEvent.click(screen.getByRole('button', { name: t.benchmarkStart }));
    await waitFor(() => expect(benchmarkApplication).toHaveBeenCalledWith('app_1', 'capacity'));
  });

  it("starts a mapping+both run via benchmarkMapping(id, 'both')", async () => {
    const benchmarkMapping = vi.fn(async () => idle) as unknown as PortalApi['benchmarkMapping'];
    renderSection(
      { kind: 'server' },
      {
        apps: [makeApp({ id: 'app_1' })],
        mappings: [makeMapping({ id: 'map_1', gateway_model_name: 'gw-model' })],
        benchmarkMapping,
      },
    );
    await screen.findByRole('combobox', { name: t.benchmarkScope });
    await pickOption(t.benchmarkScope, t.benchmarkScopeMapping);
    // Pick the mapping explicitly (also waits for the mappings to load).
    await pickOption(t.benchmarkScopeMapping, 'gw-model');
    await pickOption(t.benchmarkType, t.benchmarkTypeBoth);
    fireEvent.click(screen.getByRole('button', { name: t.benchmarkStart }));
    await waitFor(() => expect(benchmarkMapping).toHaveBeenCalledWith('map_1', 'both'));
  });

  it('renders the live panel and hides the Start form while a run is active', async () => {
    const subscribeBenchmark = vi.fn((_id: string, onStatus: (s: BenchmarkStatus) => void) => {
      onStatus({
        running: true,
        server_id: 'srv_1',
        scope: 'application',
        total: 3,
        done: 1,
        results: [
          {
            mapping_id: 'm1',
            gateway_model_name: 'gw',
            gen_tokens_per_second: 42,
            prompt_tokens_per_second: 0,
            load_time_ms: 1000,
          },
        ],
      });
      return () => {};
    }) as unknown as PortalApi['subscribeBenchmark'];

    renderSection({ kind: 'server' }, { subscribeBenchmark });

    // Live panel shows done/total + the streamed per-model result row.
    await screen.findByText(/1\/3/);
    await screen.findByText('gw: 42 tok/s, 1000 ms');
    // The Start form is not rendered while running.
    expect(screen.queryByRole('button', { name: t.benchmarkStart })).not.toBeInTheDocument();
  });

  it('shows ONLY the vision verdict for a vision result row — no meaningless speed metrics', async () => {
    const subscribeBenchmark = vi.fn((_id: string, onStatus: (s: BenchmarkStatus) => void) => {
      onStatus({
        running: true,
        server_id: 'srv_1',
        scope: 'application',
        total: 1,
        done: 1,
        results: [
          {
            mapping_id: 'm1',
            gateway_model_name: 'gw',
            gen_tokens_per_second: 0,
            prompt_tokens_per_second: 0,
            load_time_ms: 0,
            vision_capable: true,
          },
        ],
      });
      return () => {};
    }) as unknown as PortalApi['subscribeBenchmark'];

    renderSection({ kind: 'server' }, { subscribeBenchmark });

    expect(await screen.findByText(`gw: ${t.benchmarkVision}: ✓`)).toBeInTheDocument();
    expect(screen.queryByText(/tok\/s/)).not.toBeInTheDocument();
  });

  it('resumes an in-progress run delivered by the subscription on mount', async () => {
    // subscribeBenchmark immediately delivers a running snapshot → the live panel
    // renders without any Start click (proves re-entry reflects an active run).
    const subscribeBenchmark = vi.fn((_id: string, onStatus: (s: BenchmarkStatus) => void) => {
      onStatus({ running: true, server_id: 'srv_1', scope: 'server', total: 2, done: 0 });
      return () => {};
    }) as unknown as PortalApi['subscribeBenchmark'];

    renderSection({ kind: 'server' }, { subscribeBenchmark });
    expect(await screen.findByLabelText(t.benchmarkLive)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: t.benchmarkStart })).not.toBeInTheDocument();
  });

  it('shows an inline notice when a start is rejected with 409', async () => {
    const benchmarkServer = vi.fn(async () => {
      throw new PortalApiError(409, 'benchmark.server_in_use', 'server busy');
    }) as unknown as PortalApi['benchmarkServer'];
    renderSection({ kind: 'server' }, { benchmarkServer });
    await screen.findByRole('button', { name: t.benchmarkStart });
    fireEvent.click(screen.getByRole('button', { name: t.benchmarkStart }));
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent(t.errorBenchmarkServerInUse);
    // No crash: the Start button stays available.
    expect(screen.getByRole('button', { name: t.benchmarkStart })).toBeInTheDocument();
  });

  it('renders the history (speed table + capacity curve) and the last-completed line', async () => {
    const speedRun: BenchmarkRunDTO = {
      id: 'run_speed',
      mapping_id: 'map_1',
      server_id: 'srv_1',
      created_at: '2026-07-24T12:00:00Z',
      gen_tokens_per_second: 40,
      prompt_tokens_per_second: 200,
      load_time_ms: 1000,
      context_size: 8192,
      error: '',
      kind: 'speed',
    };
    const capacityRun: BenchmarkRunDTO = {
      id: 'run_cap',
      mapping_id: 'map_1',
      server_id: 'srv_1',
      created_at: '2026-07-24T11:00:00Z',
      gen_tokens_per_second: 0,
      prompt_tokens_per_second: 0,
      load_time_ms: 0,
      context_size: 0,
      error: '',
      kind: 'capacity',
      capacity: {
        max_concurrency: 2,
        recommended_concurrency: 1,
        gen_tokens_per_second_at_capacity: 120.5,
        memory_observed: true,
        levels: [
          {
            concurrency: 1,
            aggregate_tokens_per_second: 100.5,
            per_request_tokens_per_second: 100.5,
            mean_latency_ms: 44,
            successes: 1,
            errors: 0,
            stop_reason: '',
          },
          {
            concurrency: 2,
            aggregate_tokens_per_second: 175.5,
            per_request_tokens_per_second: 87.75,
            mean_latency_ms: 90,
            successes: 2,
            errors: 0,
            stop_reason: 'latency-collapse',
          },
        ],
      },
    };

    renderSection(
      { kind: 'server' },
      {
        apps: [makeApp({ id: 'app_1' })],
        mappings: [makeMapping({ id: 'map_1' })],
        runs: [speedRun, capacityRun],
      },
    );

    // Speed table cell (gen tok/s) + capacity section header + per-level stop.
    expect(await screen.findByText('40')).toBeInTheDocument();
    await screen.findByText(t.benchmarkCapacityRuns);
    expect(screen.getByText('latency-collapse')).toBeInTheDocument();
    // The newest run's "last completed" line renders (newest-first → speedRun).
    expect(screen.getByText(new RegExp(t.benchmarkLastCompleted))).toBeInTheDocument();
  });

  it('renders the vision history section (verdict row + error row), separate from the speed table', async () => {
    const visionCapableRun: BenchmarkRunDTO = {
      id: 'run_vision_ok',
      mapping_id: 'map_1',
      server_id: 'srv_1',
      created_at: '2026-08-05T12:00:00Z',
      gen_tokens_per_second: 0,
      prompt_tokens_per_second: 0,
      load_time_ms: 0,
      context_size: 0,
      error: '',
      kind: 'vision',
      vision_capable: true,
    };
    const visionInconclusiveRun: BenchmarkRunDTO = {
      id: 'run_vision_err',
      mapping_id: 'map_1',
      server_id: 'srv_1',
      created_at: '2026-08-05T11:00:00Z',
      gen_tokens_per_second: 0,
      prompt_tokens_per_second: 0,
      load_time_ms: 0,
      context_size: 0,
      error: 'upstream down',
      kind: 'vision',
      vision_capable: false,
    };

    renderSection(
      { kind: 'server' },
      {
        apps: [makeApp({ id: 'app_1' })],
        mappings: [makeMapping({ id: 'map_1' })],
        runs: [visionCapableRun, visionInconclusiveRun],
      },
    );

    await screen.findByText(t.benchmarkVisionRuns);
    expect(screen.getByText(`${t.benchmarkVision}: ✓`)).toBeInTheDocument();
    expect(screen.getByText('upstream down')).toBeInTheDocument();
    // A vision-only history must NOT render the speed table (no speed/capacity rows).
    expect(screen.queryByText(t.benchmarkRunAt)).not.toBeInTheDocument();
    expect(screen.queryByText(t.benchmarkCapacityRuns)).not.toBeInTheDocument();
  });

  it('calls onModelsChanged when the poll resolves the run to completion', async () => {
    // subscribeBenchmark delivers a running frame → the poll starts. benchmarkStatus
    // returns running once, then not-running, so the poll resolves and completion
    // fires onModelsChanged.
    const subscribeBenchmark = vi.fn((_id: string, onStatus: (s: BenchmarkStatus) => void) => {
      onStatus({ running: true, server_id: 'srv_1', scope: 'server', total: 1, done: 0 });
      return () => {};
    }) as unknown as PortalApi['subscribeBenchmark'];
    let call = 0;
    const benchmarkStatus = vi.fn(async () =>
      call++ === 0
        ? { running: true, server_id: 'srv_1', scope: 'server', total: 1, done: 0 }
        : { running: false, server_id: 'srv_1', scope: 'server', total: 1, done: 1 },
    ) as unknown as PortalApi['benchmarkStatus'];
    const onModelsChanged = vi.fn();

    renderSection(
      { kind: 'server' },
      { subscribeBenchmark, benchmarkStatus },
      { onModelsChanged, pollIntervalMs: 1 },
    );

    await waitFor(() => expect(onModelsChanged).toHaveBeenCalled());
  });

  it("recovers when the completion poll gives up (re-reads status, doesn't get stuck 'running')", async () => {
    // A running frame starts the poll. benchmarkStatus errors 5× in a row → the poll
    // REJECTS (MAX_CONSECUTIVE_ERRORS). The 6th call (the catch's ground-truth re-read)
    // reports not-running. The pre-fix swallow-and-die would leave the live panel stuck
    // and never fire onModelsChanged; the re-arm must recover it.
    const subscribeBenchmark = vi.fn((_id: string, onStatus: (s: BenchmarkStatus) => void) => {
      onStatus({ running: true, server_id: 'srv_1', scope: 'server', total: 1, done: 0 });
      return () => {};
    }) as unknown as PortalApi['subscribeBenchmark'];
    let call = 0;
    const benchmarkStatus = vi.fn(async () => {
      call += 1;
      if (call <= 5) throw new Error('blip'); // 5 consecutive errors → pollBenchmarkStatus rejects
      return { running: false, server_id: 'srv_1', scope: 'server', total: 1, done: 1 };
    }) as unknown as PortalApi['benchmarkStatus'];
    const onModelsChanged = vi.fn();

    renderSection(
      { kind: 'server' },
      { subscribeBenchmark, benchmarkStatus },
      { onModelsChanged, pollIntervalMs: 1 },
    );

    // Recovered: completion fired AND the area is unstuck (Start form back).
    await waitFor(() => expect(onModelsChanged).toHaveBeenCalled());
    await screen.findByRole('button', { name: t.benchmarkStart });
  });
});
