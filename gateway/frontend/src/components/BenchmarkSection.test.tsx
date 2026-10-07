// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { BenchmarkSection, type BenchmarkScope } from './BenchmarkSection';
import { messages, type Locale } from '../i18n';
import {
  PortalApiError,
  type BenchmarkResult,
  type BenchmarkRunDTO,
  type BenchmarkStatus,
  type PortalApplication,
  type PortalModelMapping,
  type PortalServer,
  type VRAMReportDTO,
} from '../api';
import type { PortalApi } from './shared/types';

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
    responses_mode: 'passthrough',
    messages_mode: 'passthrough',
    responses_live_timings_enabled: false,
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
    proxy_excluded: false,
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
    capabilities: [],
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
  mappingsByApp?: Record<string, PortalModelMapping[]>;
  runs?: BenchmarkRunDTO[];
  benchmarkServer?: PortalApi['benchmarkServer'];
  benchmarkApplication?: PortalApi['benchmarkApplication'];
  benchmarkMapping?: PortalApi['benchmarkMapping'];
  benchmarkStatus?: PortalApi['benchmarkStatus'];
  probeMappingVram?: PortalApi['probeMappingVram'];
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
    mappings: vi.fn(async (appId: string) => ({
      data: over.mappingsByApp?.[appId] ?? over.mappings ?? [],
    })),
    mappingBenchmarks: vi.fn(async () => over.runs ?? []),
    benchmarkServer:
      over.benchmarkServer ?? (vi.fn(async () => idle) as unknown as PortalApi['benchmarkServer']),
    benchmarkApplication:
      over.benchmarkApplication ??
      (vi.fn(async () => idle) as unknown as PortalApi['benchmarkApplication']),
    benchmarkMapping:
      over.benchmarkMapping ??
      (vi.fn(async () => idle) as unknown as PortalApi['benchmarkMapping']),
    probeMappingVram:
      over.probeMappingVram ??
      (vi.fn(async () => idle) as unknown as PortalApi['probeMappingVram']),
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

afterEach(cleanup);

for (const locale of ['de', 'en'] as readonly Locale[]) {
  const t = messages[locale];

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

  describe(`BenchmarkSection [${locale}]`, () => {
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
      expect(
        await screen.findByRole('option', { name: t.benchmarkTypeVision }),
      ).toBeInTheDocument();
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
        {
          apps: [makeApp({ id: 'app_1', endpoint: 'https://one.test:8000' })],
          benchmarkApplication,
        },
      );
      await screen.findByRole('combobox', { name: t.benchmarkScope });
      await pickOption(t.benchmarkScope, t.benchmarkScopeApplication);
      // Choose the app explicitly (also proves the apps loaded), then the type.
      await pickOption(t.benchmarkScopeApplication, 'https://one.test:8000');
      await pickOption(t.benchmarkType, t.benchmarkTypeCapacity);
      fireEvent.click(screen.getByRole('button', { name: t.benchmarkStart }));
      await waitFor(() => expect(benchmarkApplication).toHaveBeenCalledWith('app_1', 'capacity'));
    });

    // The application scope names its application, which is not necessarily
    // the server's first.
    it("starts the application scope it was opened with, not the server's first", async () => {
      const benchmarkApplication = vi.fn(
        async () => idle,
      ) as unknown as PortalApi['benchmarkApplication'];
      renderSection(
        { kind: 'application', id: 'app_2', name: 'two' },
        {
          apps: [
            makeApp({ id: 'app_1' }),
            makeApp({ id: 'app_2', endpoint: 'https://two.test:8000' }),
          ],
          benchmarkApplication,
        },
      );
      await waitFor(() =>
        expect(
          screen.getByRole('combobox', { name: t.benchmarkScopeApplication }),
        ).toHaveTextContent('https://two.test:8000'),
      );
      fireEvent.click(screen.getByRole('button', { name: t.benchmarkStart }));
      await waitFor(() => expect(benchmarkApplication).toHaveBeenCalledWith('app_2', 'speed'));
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

    // A load_time_ms of 0 means the run measured no load time, not a load of
    // 0 ms, so the line names it as not measured.
    it('says the load time was not measured when a result carries load_time_ms 0', async () => {
      const subscribeBenchmark = vi.fn((_id: string, onStatus: (s: BenchmarkStatus) => void) => {
        onStatus({
          running: true,
          server_id: 'srv_1',
          scope: 'application',
          total: 2,
          done: 1,
          results: [
            {
              mapping_id: 'm1',
              gateway_model_name: 'gw',
              gen_tokens_per_second: 42,
              prompt_tokens_per_second: 0,
              load_time_ms: 0,
            },
          ],
        });
        return () => {};
      }) as unknown as PortalApi['subscribeBenchmark'];

      renderSection({ kind: 'server' }, { subscribeBenchmark });

      const line =
        locale === 'de'
          ? 'gw: 42 tok/s, Ladezeit nicht gemessen'
          : 'gw: 42 tok/s, load time not measured';
      expect(await screen.findByText(line)).toBeInTheDocument();
      expect(screen.queryByText(/tok\/s, 0 ms/)).not.toBeInTheDocument();
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

  // The VRAM measurement is the fourth run kind, and the only one that is not a
  // `?mode=` value: it drains the whole server once and then loads exactly ONE
  // model, so it exists only on the MODEL scope and goes through its own
  // endpoint. These tests pin the four things D4/D5 say the surface must not do:
  // offer it where it cannot run, start it through a mode, render a "no result"
  // as a zero, and hide the fleet it force-stopped.
  describe(`BenchmarkSection VRAM run [${locale}]`, () => {
    const vramReport = (over: Partial<VRAMReportDTO> = {}): VRAMReportDTO => ({
      isolated: true,
      gpus: [
        {
          index: 0,
          baseline_used_mb: 1024,
          delta_mb: 22528,
          measured_mb: 21000,
          fingerprint_kind: 'uuid',
          attributable: true,
        },
      ],
      ...over,
    });

    // A terminal SSE frame for a finished VRAM run: `running: false` with the
    // result attached, exactly as the runner publishes it in its terminal defer.
    function finishedVramRun(result: Partial<BenchmarkResult>): Overrides {
      return {
        subscribeBenchmark: vi.fn((_id: string, onStatus: (s: BenchmarkStatus) => void) => {
          onStatus({
            running: false,
            server_id: 'srv_1',
            scope: 'vram-probe',
            mode: 'vram',
            total: 1,
            done: 1,
            results: [
              {
                mapping_id: 'map_1',
                gateway_model_name: 'gw-model',
                gen_tokens_per_second: 0,
                prompt_tokens_per_second: 0,
                load_time_ms: 0,
                ...result,
              },
            ],
          });
          return () => {};
        }) as unknown as PortalApi['subscribeBenchmark'],
      };
    }

    const withMapping: Overrides = {
      apps: [makeApp({ id: 'app_1' })],
      mappings: [makeMapping({ id: 'map_1', gateway_model_name: 'gw-model' })],
    };

    it('offers the VRAM type ONLY on the model scope, and says where it lives', async () => {
      renderSection({ kind: 'server' }, withMapping);
      await screen.findByRole('combobox', { name: t.benchmarkType });
      // The hint is unconditional: it is the only place the scope restriction and
      // the drain are stated, so it must be readable before the option appears.
      expect(screen.getByText(t.benchmarkTypeVramHint)).toBeInTheDocument();
      fireEvent.mouseDown(screen.getByRole('combobox', { name: t.benchmarkType }));
      expect(await screen.findByRole('option', { name: t.benchmarkTypeSpeed })).toBeInTheDocument();
      expect(screen.queryByRole('option', { name: t.benchmarkTypeVram })).not.toBeInTheDocument();
      fireEvent.keyDown(screen.getByRole('listbox'), { key: 'Escape' });

      await pickOption(t.benchmarkScope, t.benchmarkScopeMapping);
      fireEvent.mouseDown(screen.getByRole('combobox', { name: t.benchmarkType }));
      expect(await screen.findByRole('option', { name: t.benchmarkTypeVram })).toBeInTheDocument();
    });

    it('starts it through probeMappingVram, never through a benchmark mode', async () => {
      const probeMappingVram = vi.fn(async () => idle) as unknown as PortalApi['probeMappingVram'];
      const { api } = renderSection(
        { kind: 'mapping', id: 'map_1', name: 'gw-model' },
        {
          ...withMapping,
          probeMappingVram,
        },
      );
      await screen.findByRole('combobox', { name: t.benchmarkType });
      await pickOption(t.benchmarkType, t.benchmarkTypeVram);
      fireEvent.click(screen.getByRole('button', { name: t.benchmarkStart }));
      await waitFor(() => expect(probeMappingVram).toHaveBeenCalledWith('map_1'));
      // Not a mode: the three ?mode= starters stay untouched.
      expect(api.benchmarkMapping).not.toHaveBeenCalled();
      expect(api.benchmarkServer).not.toHaveBeenCalled();
      expect(api.benchmarkApplication).not.toHaveBeenCalled();
    });

    it('drops the VRAM type when the scope leaves the model scope', async () => {
      // Otherwise the type select keeps a value this scope cannot start, and
      // Start would silently probe some mapping the operator never chose.
      const probeMappingVram = vi.fn(async () => idle) as unknown as PortalApi['probeMappingVram'];
      const { api } = renderSection(
        { kind: 'mapping', id: 'map_1', name: 'gw-model' },
        {
          ...withMapping,
          probeMappingVram,
        },
      );
      await screen.findByRole('combobox', { name: t.benchmarkType });
      await pickOption(t.benchmarkType, t.benchmarkTypeVram);
      await pickOption(t.benchmarkScope, t.benchmarkScopeServer);
      fireEvent.click(screen.getByRole('button', { name: t.benchmarkStart }));
      await waitFor(() => expect(api.benchmarkServer).toHaveBeenCalledWith('srv_1', 'speed'));
      expect(probeMappingVram).not.toHaveBeenCalled();
    });

    it('localizes a precondition refusal instead of showing the raw code alone', async () => {
      const probeMappingVram = vi.fn(async () => {
        throw new PortalApiError(409, 'benchmark.vram_isolation_unavailable', 'file mode');
      }) as unknown as PortalApi['probeMappingVram'];
      renderSection(
        { kind: 'mapping', id: 'map_1', name: 'gw-model' },
        {
          ...withMapping,
          probeMappingVram,
        },
      );
      await screen.findByRole('combobox', { name: t.benchmarkType });
      await pickOption(t.benchmarkType, t.benchmarkTypeVram);
      fireEvent.click(screen.getByRole('button', { name: t.benchmarkStart }));
      expect(await screen.findByRole('alert')).toHaveTextContent(
        t.errorBenchmarkVramIsolationUnavailable,
      );
    });

    it('warns, while the run is live, that the whole server is force-stopped', async () => {
      const subscribeBenchmark = vi.fn((_id: string, onStatus: (s: BenchmarkStatus) => void) => {
        onStatus({
          running: true,
          server_id: 'srv_1',
          scope: 'vram-probe',
          mode: 'vram',
          total: 1,
          done: 0,
        });
        return () => {};
      }) as unknown as PortalApi['subscribeBenchmark'];
      renderSection(
        { kind: 'mapping', id: 'map_1', name: 'gw-model' },
        {
          ...withMapping,
          subscribeBenchmark,
        },
      );
      expect(await screen.findByText(t.benchmarkVramRunningNote)).toBeInTheDocument();
    });

    it('reports the numbers, the isolation and the drained fleet of a finished run', async () => {
      renderSection(
        { kind: 'mapping', id: 'map_1', name: 'gw-model' },
        {
          ...withMapping,
          ...finishedVramRun({
            vram: vramReport({
              drained_spec_ids: ['spec_a', 'spec_b'],
              isolation_evidence: { spec_a: 'stopped_after_write', spec_b: 'no_process_at_write' },
            }),
          }),
        },
      );
      await screen.findByText(t.benchmarkVramResultTitle, { exact: false });
      expect(screen.getByText(t.benchmarkVramIsolationConfirmed)).toBeInTheDocument();
      // Both numbers, side by side and never averaged.
      expect(screen.getByText('22528')).toBeInTheDocument();
      expect(screen.getByText('21000')).toBeInTheDocument();
      expect(screen.getByText(t.benchmarkVramFingerprintUuid)).toBeInTheDocument();
      // The named risk: what the run force-stopped is on screen, so an operator
      // whose gateway died mid-run knows which specs stay stopped until it
      // starts again.
      expect(screen.getByText(/spec_a/)).toHaveTextContent('spec_b');
      expect(screen.getByText(t.benchmarkVramDrainedNote)).toBeInTheDocument();
    });

    // The run records its drain in the benchmark override lease, which the
    // gateway settles when it starts again. So the drained note names that
    // remedy instead of sending the operator to clear the overrides by hand,
    // while a restore that FAILED may still need a hand and says that the
    // gateway retries it as well.
    it('says a drain left behind is cleared when the gateway starts again, and a failed restore by hand', async () => {
      renderSection(
        { kind: 'mapping', id: 'map_1', name: 'gw-model' },
        {
          ...withMapping,
          ...finishedVramRun({
            vram: vramReport({
              drained_spec_ids: ['spec_a', 'spec_b'],
              restore_failed: ['spec_b'],
            }),
          }),
        },
      );
      const note = await screen.findByText(t.benchmarkVramDrainedNote);
      expect(note).not.toHaveTextContent(/von Hand|by hand/);
      expect(note).toHaveTextContent(locale === 'de' ? /wieder startet/ : /starts again/);
      const failed = screen.getByText(t.benchmarkVramRestoreFailed, { exact: false });
      expect(failed).toHaveTextContent('spec_b');
      expect(failed).toHaveTextContent(locale === 'de' ? /von Hand/ : /by hand/);
      expect(failed).toHaveTextContent(locale === 'de' ? /nächsten Start/ : /next start/);
    });

    it('says WHICH proof the isolation rested on', async () => {
      // Two questions, two lines: whether the evidence was complete
      // (`isolated`), and how strong it was allowed to be (`isolation_proof`).
      // An operator weighing a number needs the second, because "the agent
      // confirmed it applied this document" and "we waited a minute and saw no
      // process" are not the same claim.
      renderSection(
        { kind: 'mapping', id: 'map_1', name: 'gw-model' },
        {
          ...withMapping,
          ...finishedVramRun({ vram: vramReport({ isolation_proof: 'config_acknowledged' }) }),
        },
      );
      expect(
        await screen.findByText(t.benchmarkVramIsolationProofAcknowledged),
      ).toBeInTheDocument();
      expect(screen.queryByText(t.benchmarkVramIsolationProofBindDelay)).not.toBeInTheDocument();
    });

    it('names no proof at all for a run that did not record one', async () => {
      // A report from a gateway that predates the acknowledgement records
      // NEITHER standard, and rendering `bind_delay` for it would be a guess
      // about behaviour that is not in the payload.
      renderSection(
        { kind: 'mapping', id: 'map_1', name: 'gw-model' },
        { ...withMapping, ...finishedVramRun({ vram: vramReport({}) }) },
      );
      await screen.findByText(t.benchmarkVramResultTitle, { exact: false });
      expect(screen.queryByText(t.benchmarkVramIsolationProofAcknowledged)).not.toBeInTheDocument();
      expect(screen.queryByText(t.benchmarkVramIsolationProofBindDelay)).not.toBeInTheDocument();
    });

    it('names the specs it could NOT restore as something to clear by hand', async () => {
      renderSection(
        { kind: 'mapping', id: 'map_1', name: 'gw-model' },
        {
          ...withMapping,
          ...finishedVramRun({
            vram: vramReport({ drained_spec_ids: ['spec_a'], restore_failed: ['spec_a'] }),
          }),
        },
      );
      const alert = await screen.findByText(t.benchmarkVramRestoreFailed, { exact: false });
      expect(alert).toHaveTextContent('spec_a');
      // And it does NOT reach for the takeover sentence, which says the opposite.
      expect(screen.queryByText(t.benchmarkVramRestoreTakenOver, { exact: false })).toBeNull();
    });

    // A spec the restore found TAKEN OVER (it was not force_stopped at the
    // restore) is not a spec that was left force_stopped, and it must not read
    // like one: "clear these by hand" would name an override that is not
    // there, or is not the run's.
    it('separates a taken-over override from one it could not restore', async () => {
      renderSection(
        { kind: 'mapping', id: 'map_1', name: 'gw-model' },
        {
          ...withMapping,
          ...finishedVramRun({
            vram: vramReport({
              drained_spec_ids: ['spec_a'],
              restore_taken_over: ['spec_a'],
            }),
          }),
        },
      );
      const alert = await screen.findByText(t.benchmarkVramRestoreTakenOver, { exact: false });
      expect(alert).toHaveTextContent('spec_a');
      expect(screen.queryByText(t.benchmarkVramRestoreFailed, { exact: false })).toBeNull();
    });

    it('renders an inconclusive outcome as its REASON, never as a 0', async () => {
      renderSection(
        { kind: 'mapping', id: 'map_1', name: 'gw-model' },
        {
          ...withMapping,
          ...finishedVramRun({
            vram: vramReport({ inconclusive: 'already_resident', gpus: [] }),
          }),
        },
      );
      await screen.findByText(t.benchmarkVramInconclusiveAlreadyResident, { exact: false });
      // Not an error, and not a measurement of nothing: no per-GPU table at all,
      // so there is no cell that could read 0 MB.
      expect(screen.queryByText(t.benchmarkVramColDelta)).not.toBeInTheDocument();
      expect(screen.queryByText('0')).not.toBeInTheDocument();
    });

    it('distinguishes "no result" from "never reached the measurement phase"', async () => {
      renderSection(
        { kind: 'mapping', id: 'map_1', name: 'gw-model' },
        { ...withMapping, ...finishedVramRun({ error: 'benchmark.vram_isolation_blocked' }) },
      );
      // An ABSENT report is the other outcome: nothing was stopped and nothing
      // measured, so it must not read like a run that measured and failed.
      await screen.findByText(t.benchmarkVramNoReport);
      expect(screen.getByText('benchmark.vram_isolation_blocked')).toBeInTheDocument();
      expect(screen.queryByText(t.benchmarkVramInconclusiveTitle)).not.toBeInTheDocument();
      expect(screen.queryByText(t.benchmarkVramIsolationConfirmed)).not.toBeInTheDocument();
    });

    it('renders the vram history rows as evidence, outside the speed table', async () => {
      const vramRun: BenchmarkRunDTO = {
        id: 'run_vram',
        mapping_id: 'map_1',
        server_id: 'srv_1',
        created_at: '2026-09-01T10:00:00Z',
        gen_tokens_per_second: 0,
        prompt_tokens_per_second: 0,
        load_time_ms: 0,
        context_size: 0,
        error: '',
        kind: 'vram',
        vram: vramReport({
          gpus: [
            {
              index: 1,
              baseline_used_mb: 2048,
              delta_mb: 0,
              measured_mb: 19000,
              fingerprint_kind: 'name_total',
              unified_memory: true,
              attributable: false,
            },
          ],
        }),
      };
      renderSection({ kind: 'server' }, { ...withMapping, runs: [vramRun] });
      await screen.findByText(t.benchmarkVramRuns);
      expect(screen.getByText('19000')).toBeInTheDocument();
      // An unknown number is a dash. 0 means UNKNOWN throughout this feature, so
      // rendering the missing delta as "0" would invent a measurement.
      expect(screen.getByText('—')).toBeInTheDocument();
      expect(screen.getByText(t.benchmarkVramFingerprintNameTotal)).toBeInTheDocument();
      expect(screen.getByText(t.benchmarkVramUnifiedMemory)).toBeInTheDocument();
      expect(screen.getByText(t.benchmarkVramNotAttributable)).toBeInTheDocument();
      // A vram row is NOT a speed row: the speed table would render it as four
      // dashes and a green tick.
      expect(screen.queryByText(t.benchmarkRunAt)).not.toBeInTheDocument();
    });

    it('renders a vram history row that reached no number as its reason', async () => {
      const vramRun: BenchmarkRunDTO = {
        id: 'run_vram_inconclusive',
        mapping_id: 'map_1',
        server_id: 'srv_1',
        created_at: '2026-09-01T10:00:00Z',
        gen_tokens_per_second: 0,
        prompt_tokens_per_second: 0,
        load_time_ms: 0,
        context_size: 0,
        error: '',
        kind: 'vram',
        vram: vramReport({ inconclusive: 'baseline_unstable', gpus: [] }),
      };
      renderSection({ kind: 'server' }, { ...withMapping, runs: [vramRun] });
      await screen.findByText(t.benchmarkVramRuns);
      expect(
        screen.getByText(t.benchmarkVramInconclusiveBaselineUnstable, { exact: false }),
      ).toBeInTheDocument();
      expect(screen.queryByText(t.benchmarkVramColDelta)).not.toBeInTheDocument();
    });
  });

  // An images-only mapping (its effective API flavors name openai_images and
  // no text flavor) cannot answer the chat prompt that speed, capacity, both
  // and vision send. The gateway refuses those runs on the mapping scope and
  // skips the mapping on the application and server scopes; these tests pin
  // what the portal shows for both.
  describe(`BenchmarkSection images-only mappings [${locale}]`, { timeout: 15_000 }, () => {
    function result(over: Partial<BenchmarkResult>): BenchmarkResult {
      return {
        mapping_id: 'map_1',
        gateway_model_name: 'gw-model',
        gen_tokens_per_second: 0,
        prompt_tokens_per_second: 0,
        load_time_ms: 0,
        ...over,
      };
    }

    function delivering(status: Partial<BenchmarkStatus>): Overrides {
      return {
        subscribeBenchmark: vi.fn((_id: string, onStatus: (s: BenchmarkStatus) => void) => {
          onStatus({ ...idle, scope: 'application', mode: 'speed', ...status });
          return () => {};
        }) as unknown as PortalApi['subscribeBenchmark'],
      };
    }

    const imagesOnlyMapping: Overrides = {
      apps: [makeApp({ id: 'app_1' })],
      mappings: [makeMapping({ id: 'map_img', gateway_model_name: 'sd-model', images_only: true })],
    };

    it('renders a skipped mapping as its skip, never as "0 tok/s, 0 ms"', async () => {
      renderSection(
        { kind: 'server' },
        delivering({
          running: true,
          total: 2,
          done: 1,
          results: [result({ gateway_model_name: 'sd-model', skipped: 'images_only' })],
        }),
      );
      expect(
        await screen.findByText(`sd-model: ${t.benchmarkResultSkippedImagesOnly}`),
      ).toBeInTheDocument();
      expect(screen.queryByText(/tok\/s/)).not.toBeInTheDocument();
    });

    // Every run kind that sends a chat prompt gets the notice.
    it.each(['speed', 'capacity', 'both', 'vision'])(
      'names the skipped and the unmeasured mappings once a %s run has finished',
      async (mode) => {
        const unreadable =
          'runtime spec unreadable; not benchmarked (no chat prompt sent): store unavailable';
        renderSection(
          { kind: 'server' },
          delivering({
            running: false,
            mode,
            total: 3,
            done: 3,
            results: [
              result({
                mapping_id: 'map_ok',
                gateway_model_name: 'text-model',
                gen_tokens_per_second: 42,
              }),
              result({
                mapping_id: 'map_img',
                gateway_model_name: 'sd-model',
                skipped: 'images_only',
              }),
              result({
                mapping_id: 'map_bad',
                gateway_model_name: 'other-model',
                error: unreadable,
              }),
            ],
          }),
        );
        const notice = await screen.findByLabelText(t.benchmarkNotMeasured);
        expect(
          within(notice).getByText(`sd-model: ${t.benchmarkResultSkippedImagesOnly}`),
        ).toBeInTheDocument();
        expect(within(notice).getByText(`other-model: ${unreadable}`)).toBeInTheDocument();
        // A measured mapping is not "not measured": its number is in the history.
        expect(within(notice).queryByText(/text-model/)).not.toBeInTheDocument();
      },
    );

    it('shows no notice after a finished run that measured every mapping', async () => {
      renderSection(
        { kind: 'server' },
        delivering({
          running: false,
          total: 1,
          done: 1,
          results: [result({ gen_tokens_per_second: 42 })],
        }),
      );
      await screen.findByRole('button', { name: t.benchmarkStart });
      expect(screen.queryByLabelText(t.benchmarkNotMeasured)).not.toBeInTheDocument();
    });

    // A VRAM run renders its own outcome, and a load or a context probe is not
    // a measurement this form started: none of them gets the notice.
    it.each(['vram', 'load', 'context'])(
      'shows no notice after a finished %s run, even with an error',
      async (mode) => {
        renderSection(
          { kind: 'server' },
          delivering({
            running: false,
            mode,
            total: 1,
            done: 1,
            results: [result({ error: 'provider.unavailable: upstream status 503' })],
          }),
        );
        await screen.findByRole('button', { name: t.benchmarkStart });
        expect(screen.queryByLabelText(t.benchmarkNotMeasured)).not.toBeInTheDocument();
      },
    );

    it('disables the chat-prompt run types for an images-only mapping and says why', async () => {
      const probeMappingVram = vi.fn(async () => idle) as unknown as PortalApi['probeMappingVram'];
      renderSection(
        { kind: 'mapping', id: 'map_img', name: 'sd-model' },
        { ...imagesOnlyMapping, probeMappingVram },
      );
      const start = await screen.findByRole('button', { name: t.benchmarkStart });
      await waitFor(() => expect(start).toBeDisabled());
      expect(start).toHaveAccessibleDescription(t.benchmarkImagesOnlyHint);
      expect(screen.getByText(t.benchmarkImagesOnlyHint)).toBeInTheDocument();

      fireEvent.mouseDown(screen.getByRole('combobox', { name: t.benchmarkType }));
      for (const label of [
        t.benchmarkTypeSpeed,
        t.benchmarkTypeCapacity,
        t.benchmarkTypeBoth,
        t.benchmarkTypeVision,
      ]) {
        expect(await screen.findByRole('option', { name: label })).toHaveAttribute(
          'aria-disabled',
          'true',
        );
      }
      // The VRAM measurement loads the model without a chat prompt, so it stays.
      const vram = screen.getByRole('option', { name: t.benchmarkTypeVram });
      expect(vram).not.toHaveAttribute('aria-disabled', 'true');
      fireEvent.click(vram);

      await waitFor(() => expect(start).toBeEnabled());
      // The hint stays, but it no longer describes a refusal of Start.
      expect(start).not.toHaveAccessibleDescription();
      fireEvent.click(start);
      await waitFor(() => expect(probeMappingVram).toHaveBeenCalledWith('map_img'));
    });

    // The type is not switched when the Model picker moves to an images-only
    // mapping, so a chat-prompt type chosen on a text mapping stays selected:
    // Start is refused for each of them, not only for the default speed.
    it.each([
      ['capacity', 'benchmarkTypeCapacity'],
      ['both', 'benchmarkTypeBoth'],
      ['vision', 'benchmarkTypeVision'],
    ] as const)(
      'refuses Start when %s stays selected as the Model picker moves to an images-only mapping',
      async (_mode, key) => {
        renderSection(
          { kind: 'mapping', id: 'map_txt', name: 'txt', applicationId: 'app_1' },
          {
            apps: [makeApp({ id: 'app_1' })],
            mappings: [
              makeMapping({ id: 'map_txt', gateway_model_name: 'txt' }),
              makeMapping({ id: 'map_img', gateway_model_name: 'sd-model', images_only: true }),
            ],
          },
        );
        await screen.findByRole('combobox', { name: t.benchmarkHistory });
        await pickOption(t.benchmarkType, t[key]);
        const start = screen.getByRole('button', { name: t.benchmarkStart });
        expect(start).toBeEnabled();
        await pickOption(t.benchmarkScopeMapping, 'sd-model');
        await waitFor(() => expect(start).toBeDisabled());
        expect(start).toHaveAccessibleDescription(t.benchmarkImagesOnlyHint);
      },
    );

    // A model scope picked on the form rather than opened from a row names no
    // mapping: it runs the application's first one, so the marker is read off
    // that one.
    it('reads the marker off the first mapping when the model scope is picked on the form', async () => {
      renderSection({ kind: 'server' }, imagesOnlyMapping);
      await screen.findByRole('combobox', { name: t.benchmarkHistory });
      const start = screen.getByRole('button', { name: t.benchmarkStart });
      expect(start).toBeEnabled();
      await pickOption(t.benchmarkScope, t.benchmarkScopeMapping);
      await waitFor(() => expect(start).toBeDisabled());
    });

    it('keeps every run type for a text mapping, with no hint', async () => {
      renderSection(
        { kind: 'mapping', id: 'map_1', name: 'gw-model' },
        {
          apps: [makeApp({ id: 'app_1' })],
          mappings: [makeMapping({ id: 'map_1', images_only: false })],
        },
      );
      await screen.findByRole('combobox', { name: t.benchmarkHistory });
      expect(screen.getByRole('button', { name: t.benchmarkStart })).toBeEnabled();
      expect(screen.queryByText(t.benchmarkImagesOnlyHint)).not.toBeInTheDocument();
    });

    // The application and server scopes skip an images-only mapping and run
    // the rest, so the marker blocks nothing there.
    it('leaves the application scope startable although its first mapping is images-only', async () => {
      const benchmarkApplication = vi.fn(
        async () => idle,
      ) as unknown as PortalApi['benchmarkApplication'];
      renderSection(
        { kind: 'application', id: 'app_1', name: 'app' },
        { ...imagesOnlyMapping, benchmarkApplication },
      );
      // The history picker renders once the mappings have loaded.
      await screen.findByRole('combobox', { name: t.benchmarkHistory });
      const start = screen.getByRole('button', { name: t.benchmarkStart });
      expect(start).toBeEnabled();
      expect(screen.queryByText(t.benchmarkImagesOnlyHint)).not.toBeInTheDocument();
      fireEvent.click(start);
      await waitFor(() => expect(benchmarkApplication).toHaveBeenCalledWith('app_1', 'speed'));
    });

    it("reads the marker from the mapping's own application, not the server's first", async () => {
      renderSection(
        { kind: 'mapping', id: 'map_img', name: 'sd-model', applicationId: 'app_2' },
        {
          apps: [makeApp({ id: 'app_1' }), makeApp({ id: 'app_2' })],
          mappingsByApp: {
            app_1: [makeMapping({ id: 'map_txt', application_id: 'app_1' })],
            app_2: [
              makeMapping({
                id: 'map_img',
                application_id: 'app_2',
                gateway_model_name: 'sd-model',
                images_only: true,
              }),
            ],
          },
        },
      );
      const start = await screen.findByRole('button', { name: t.benchmarkStart });
      await waitFor(() => expect(start).toBeDisabled());
      expect(start).toHaveAccessibleDescription(t.benchmarkImagesOnlyHint);
    });

    // The scope names its application and its mapping before either list has
    // loaded. Until then the pickers show nothing rather than an id none of
    // their options carries yet, which MUI warns about on every such render.
    it('warns of no out-of-range picker value while the lists load, then shows the loaded mapping', async () => {
      const warn = vi.spyOn(console, 'warn');
      try {
        renderSection(
          { kind: 'mapping', id: 'map_img', name: 'sd-model', applicationId: 'app_1' },
          imagesOnlyMapping,
        );
        await screen.findByRole('combobox', { name: t.benchmarkHistory });
        expect(screen.getByRole('combobox', { name: t.benchmarkScopeMapping })).toHaveTextContent(
          'sd-model',
        );
        expect(
          warn.mock.calls.map((args) => args.join(' ')).filter((m) => m.includes('out-of-range')),
        ).toEqual([]);
      } finally {
        warn.mockRestore();
      }
    });
  });

  // A manual speed or both run unpins the server's pinned specs for its
  // duration and stops every running one before each measurement. Neither is
  // silent: the live panel names both sets, and once the run has finished the
  // area names the specs pinned again, the ones that could not be pinned
  // again (a warning: they may still be unpinned until an operator pins
  // them), and the stopped ones, whether or not any mapping went unmeasured.
  describe(`BenchmarkSection unpin and stop notices [${locale}]`, { timeout: 15_000 }, () => {
    const measured: BenchmarkResult = {
      mapping_id: 'map_1',
      gateway_model_name: 'gw-model',
      gen_tokens_per_second: 42,
      prompt_tokens_per_second: 0,
      load_time_ms: 1000,
    };

    function delivering(status: Partial<BenchmarkStatus>): Overrides {
      return {
        subscribeBenchmark: vi.fn((_id: string, onStatus: (s: BenchmarkStatus) => void) => {
          onStatus({ ...idle, scope: 'application', mode: 'speed', ...status });
          return () => {};
        }) as unknown as PortalApi['subscribeBenchmark'],
      };
    }

    // The alert whose first line is `text`, and the spec ids it names, one per
    // line.
    function alertWith(text: string, container: HTMLElement = document.body): HTMLElement {
      const alert = within(container).getByText(text).closest<HTMLElement>('[role="alert"]');
      if (alert === null) throw new Error(`no alert carries: ${text}`);
      return alert;
    }
    function namedIds(alert: HTMLElement): string[] {
      return within(alert)
        .getAllByText(/^rs_/)
        .map((node) => node.textContent ?? '');
    }

    it('names the unpinned and the stopped specs at the top of the live panel', async () => {
      renderSection(
        { kind: 'server' },
        delivering({
          running: true,
          total: 2,
          done: 1,
          results: [measured],
          unpinned_spec_ids: ['rs_a', 'rs_b'],
          stopped_spec_ids: ['rs_a', 'rs_c'],
        }),
      );
      const panel = await screen.findByLabelText(t.benchmarkLive);

      const unpinned = alertWith(t.benchmarkUnpinnedDuringRun, panel);
      expect(unpinned).toHaveClass('MuiAlert-colorInfo');
      expect(namedIds(unpinned)).toEqual(['rs_a', 'rs_b']);
      const stopped = alertWith(t.benchmarkStoppedForMeasurement, panel);
      expect(stopped).toHaveClass('MuiAlert-colorInfo');
      expect(namedIds(stopped)).toEqual(['rs_a', 'rs_c']);
      // At the top of the panel, and only there while the run is live.
      expect(panel.firstElementChild).toBe(unpinned);
      expect(screen.getAllByText(t.benchmarkUnpinnedDuringRun)).toHaveLength(1);
      expect(screen.queryByText(t.benchmarkUnpinnedAfterRun)).not.toBeInTheDocument();
      expect(screen.queryByText(t.benchmarkRepinFailed)).not.toBeInTheDocument();
    });

    it('names the pinned-again, the not-pinned-again and the stopped specs after the run', async () => {
      renderSection(
        { kind: 'server' },
        delivering({
          running: false,
          total: 1,
          done: 1,
          results: [measured],
          unpinned_spec_ids: ['rs_a', 'rs_b'],
          repin_failed: ['rs_b'],
          stopped_spec_ids: ['rs_c'],
        }),
      );
      await screen.findByRole('button', { name: t.benchmarkStart });
      expect(screen.queryByLabelText(t.benchmarkLive)).not.toBeInTheDocument();
      // Every mapping was measured, so there is no unmeasured notice to sit in.
      expect(screen.queryByLabelText(t.benchmarkNotMeasured)).not.toBeInTheDocument();

      const pinnedAgain = alertWith(t.benchmarkUnpinnedAfterRun);
      expect(pinnedAgain).toHaveClass('MuiAlert-colorInfo');
      expect(namedIds(pinnedAgain)).toEqual(['rs_a']);
      const repinFailed = alertWith(t.benchmarkRepinFailed);
      expect(repinFailed).toHaveClass('MuiAlert-colorWarning');
      expect(namedIds(repinFailed)).toEqual(['rs_b']);
      const stopped = alertWith(t.benchmarkStoppedForMeasurement);
      expect(stopped).toHaveClass('MuiAlert-colorInfo');
      expect(namedIds(stopped)).toEqual(['rs_c']);
      expect(screen.queryByText(t.benchmarkUnpinnedDuringRun)).not.toBeInTheDocument();
    });

    // Each of the three lists shows the finished notice on its own.
    const singleLists: {
      field: string;
      lists: Partial<BenchmarkStatus>;
      key: 'benchmarkUnpinnedAfterRun' | 'benchmarkRepinFailed' | 'benchmarkStoppedForMeasurement';
    }[] = [
      {
        field: 'unpinned_spec_ids',
        lists: { unpinned_spec_ids: ['rs_a'] },
        key: 'benchmarkUnpinnedAfterRun',
      },
      { field: 'repin_failed', lists: { repin_failed: ['rs_a'] }, key: 'benchmarkRepinFailed' },
      {
        field: 'stopped_spec_ids',
        lists: { stopped_spec_ids: ['rs_a'] },
        key: 'benchmarkStoppedForMeasurement',
      },
    ];
    it.each(singleLists)('shows the finished notice for $field alone', async ({ lists, key }) => {
      renderSection(
        { kind: 'server' },
        delivering({ running: false, total: 1, done: 1, results: [measured], ...lists }),
      );
      await screen.findByRole('button', { name: t.benchmarkStart });
      expect(namedIds(alertWith(t[key]))).toEqual(['rs_a']);
      expect(screen.getAllByRole('alert')).toHaveLength(1);
    });

    it('shows no notice for a run that unpinned and stopped nothing', async () => {
      renderSection(
        { kind: 'server' },
        delivering({
          running: false,
          total: 1,
          done: 1,
          results: [measured],
          unpinned_spec_ids: [],
          repin_failed: [],
          stopped_spec_ids: [],
        }),
      );
      await screen.findByRole('button', { name: t.benchmarkStart });
      expect(screen.queryAllByRole('alert')).toHaveLength(0);
    });

    it('shows no notice in the live panel of a run that unpinned and stopped nothing', async () => {
      renderSection(
        { kind: 'server' },
        delivering({ running: true, total: 2, done: 1, results: [measured], stopped_spec_ids: [] }),
      );
      await screen.findByLabelText(t.benchmarkLive);
      expect(screen.queryAllByRole('alert')).toHaveLength(0);
    });
  });
}
