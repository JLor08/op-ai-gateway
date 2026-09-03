// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { MappingSection } from './MappingSection';
import { ToastProvider } from './shared/ToastProvider';
import { messages } from '../i18n';
import {
  type BenchmarkStatus,
  type CreateMappingRequest,
  type PortalApplication,
  type PortalModelMapping,
  type PortalServer,
  type UpdateMappingRequest,
} from '../api';
import type { PortalApi } from './shared/types';

const t = messages.de;

const server: PortalServer = {
  id: 'srv_1',
  name: 'Server 1',
  domain: 's1.example.test',
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
  last_seen_at: null,
  created_at: '2026-07-16T12:00:00Z',
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
};

const application: PortalApplication = {
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
  last_checked_at: '2026-07-16T12:00:00Z',
  created_at: '2026-07-16T12:00:00Z',
};

function makeMapping(overrides: Partial<PortalModelMapping> = {}): PortalModelMapping {
  return {
    id: 'map_1',
    application_id: 'app_1',
    gateway_model_name: 'gw-model',
    app_model_name: 'app-model',
    status: 'active',
    created_at: '2026-07-16T12:00:00Z',
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
    ...overrides,
  };
}

function renderSection(
  opts: {
    mappings?: PortalModelMapping[];
    application?: PortalApplication;
    benchmarkApplication?: PortalApi['benchmarkApplication'];
    benchmarkMapping?: PortalApi['benchmarkMapping'];
    benchmarkStatus?: PortalApi['benchmarkStatus'];
    subscribeBenchmark?: PortalApi['subscribeBenchmark'];
    mappingBenchmarks?: PortalApi['mappingBenchmarks'];
    activeBenchmarks?: PortalApi['activeBenchmarks'];
    probeMappingContext?: PortalApi['probeMappingContext'];
  } = {},
) {
  const mappings = opts.mappings ?? [];
  const app = opts.application ?? application;
  const created: { serverId: string; body: CreateMappingRequest }[] = [];
  const updated: { id: string; body: UpdateMappingRequest }[] = [];
  const idle: BenchmarkStatus = {
    running: false,
    server_id: 's1',
    scope: 'application',
    total: 0,
    done: 0,
  };
  const fakeApi = {
    mappings: vi.fn(async () => ({ data: mappings })),
    // The benchmark sub-view (BenchmarkSection) loads the server's apps on mount.
    applications: vi.fn(async () => ({ data: [app] })),
    createMapping: vi.fn(async (applicationId: string, body: CreateMappingRequest) => {
      created.push({ serverId: applicationId, body });
      return makeMapping({ id: 'map_created', ...(body as Partial<PortalModelMapping>) });
    }),
    updateMapping: vi.fn(async (id: string, body: UpdateMappingRequest) => {
      updated.push({ id, body });
      return makeMapping({ id, ...(body as Partial<PortalModelMapping>) });
    }),
    deleteMapping: vi.fn(async () => ({ ok: true })),
    syncApplicationModels: vi.fn(async () => ({
      added: 0,
      disabled: 0,
      unchanged: 0,
      conflicted: 0,
    })),
    benchmarkServer: vi.fn(async () => idle),
    benchmarkApplication: opts.benchmarkApplication ?? vi.fn(async () => idle),
    benchmarkMapping: opts.benchmarkMapping ?? vi.fn(async () => idle),
    benchmarkStatus: opts.benchmarkStatus ?? vi.fn(async () => idle),
    // Default no-op SSE subscription (returns an unsub) so the sub-view doesn't
    // call an undefined method; tests exercising a live frame override it.
    subscribeBenchmark: opts.subscribeBenchmark ?? vi.fn(() => () => {}),
    mappingBenchmarks: opts.mappingBenchmarks ?? vi.fn(async () => []),
    // The edit form polls the running benchmarks to gate the context-probe button.
    activeBenchmarks: opts.activeBenchmarks ?? vi.fn(async () => []),
    probeMappingContext: opts.probeMappingContext ?? vi.fn(async () => idle),
    probeMappingVram: vi.fn(async () => idle),
  };

  render(
    <ToastProvider>
      {/* pollIntervalMs=0 drives the status poll immediately (no 2s wait). */}
      <MappingSection t={t} api={fakeApi} server={server} application={app} pollIntervalMs={0} />
    </ToastProvider>,
  );
  return { fakeApi, created, updated };
}

afterEach(cleanup);

describe('MappingSection performance metrics', () => {
  it('sends all nine metric fields from the create form with distinct values', async () => {
    const { created } = renderSection();
    // Open the create sub-view from the list action.
    fireEvent.click(screen.getByRole('button', { name: t.mappingCreate }));

    fireEvent.change(screen.getByLabelText(t.mappingGatewayName), {
      target: { value: 'gw-model' },
    });
    fireEvent.change(screen.getByLabelText(t.mappingAppName), { target: { value: 'app-model' } });
    // Distinct values per field so a crossed wiring (e.g. promptTps → load_time_ms)
    // is caught rather than passing silently.
    fireEvent.change(screen.getByLabelText(t.mappingContextSize), { target: { value: '131072' } });
    fireEvent.change(screen.getByLabelText(t.mappingGenTokensPerSecond), {
      target: { value: '40.5' },
    });
    fireEvent.change(screen.getByLabelText(t.mappingPromptTokensPerSecond), {
      target: { value: '200.25' },
    });
    fireEvent.change(screen.getByLabelText(t.mappingLoadTimeMs), { target: { value: '1500' } });
    fireEvent.change(screen.getByLabelText(t.mappingMaxConcurrency), { target: { value: '16' } });
    fireEvent.change(screen.getByLabelText(t.mappingRecommendedConcurrency), {
      target: { value: '8' },
    });
    fireEvent.change(screen.getByLabelText(t.mappingGenTpsAtCapacity), {
      target: { value: '640.5' },
    });
    fireEvent.click(screen.getByRole('checkbox', { name: t.mappingIsMtp }));
    fireEvent.click(screen.getByRole('checkbox', { name: t.mappingVisionCapable }));
    fireEvent.click(screen.getByRole('checkbox', { name: t.mappingMetricsLocked }));

    // In the mask, the submit button carries the same "create" label.
    fireEvent.click(screen.getByRole('button', { name: t.mappingCreate }));

    await waitFor(() => expect(created).toHaveLength(1));
    expect(created[0].body).toMatchObject({
      gen_tokens_per_second: 40.5,
      prompt_tokens_per_second: 200.25,
      load_time_ms: 1500,
      context_size: 131072,
      is_mtp: true,
      vision_capable: true,
      metrics_locked: true,
      max_concurrency: 16,
      recommended_concurrency: 8,
      gen_tokens_per_second_at_capacity: 640.5,
    });
  });

  it('populates every metric field on edit and resubmits them unchanged', async () => {
    const { updated } = renderSection({
      mappings: [
        makeMapping({
          id: 'map_1',
          context_size: 8192,
          gen_tokens_per_second: 40.5,
          prompt_tokens_per_second: 200.25,
          load_time_ms: 1500,
          is_mtp: true,
          vision_capable: true,
          metrics_locked: true,
          max_concurrency: 16,
          recommended_concurrency: 8,
          gen_tokens_per_second_at_capacity: 640.5,
        }),
      ],
    });
    await screen.findByText('gw-model');
    fireEvent.click(screen.getByRole('button', { name: t.mappingEdit }));

    // Each input must reflect its OWN row value (catches an openEdit transposition).
    expect((screen.getByLabelText(t.mappingContextSize) as HTMLInputElement).value).toBe('8192');
    expect((screen.getByLabelText(t.mappingGenTokensPerSecond) as HTMLInputElement).value).toBe(
      '40.5',
    );
    expect((screen.getByLabelText(t.mappingPromptTokensPerSecond) as HTMLInputElement).value).toBe(
      '200.25',
    );
    expect((screen.getByLabelText(t.mappingLoadTimeMs) as HTMLInputElement).value).toBe('1500');
    expect((screen.getByLabelText(t.mappingMaxConcurrency) as HTMLInputElement).value).toBe('16');
    expect((screen.getByLabelText(t.mappingRecommendedConcurrency) as HTMLInputElement).value).toBe(
      '8',
    );
    expect((screen.getByLabelText(t.mappingGenTpsAtCapacity) as HTMLInputElement).value).toBe(
      '640.5',
    );
    expect(
      (screen.getByRole('checkbox', { name: t.mappingIsMtp }) as HTMLInputElement).checked,
    ).toBe(true);
    expect(
      (screen.getByRole('checkbox', { name: t.mappingVisionCapable }) as HTMLInputElement).checked,
    ).toBe(true);
    expect(
      (screen.getByRole('checkbox', { name: t.mappingMetricsLocked }) as HTMLInputElement).checked,
    ).toBe(true);

    fireEvent.click(screen.getByRole('button', { name: t.mappingSave }));

    await waitFor(() => expect(updated).toHaveLength(1));
    expect(updated[0].id).toBe('map_1');
    expect(updated[0].body).toMatchObject({
      context_size: 8192,
      gen_tokens_per_second: 40.5,
      prompt_tokens_per_second: 200.25,
      load_time_ms: 1500,
      is_mtp: true,
      vision_capable: true,
      metrics_locked: true,
      max_concurrency: 16,
      recommended_concurrency: 8,
      gen_tokens_per_second_at_capacity: 640.5,
    });
  });
});

describe('MappingSection status field', () => {
  it('keeps the status control on the ordinary form and sends the chosen value', async () => {
    const { updated } = renderSection({
      mappings: [makeMapping({ id: 'map_1', status: 'active' })],
    });
    await screen.findByText('gw-model');
    fireEvent.click(screen.getByRole('button', { name: t.mappingEdit }));

    // The pin the agent-runtime tab's own tests cannot stand in for. `MappingForm`
    // is shared, and the launch-spec form beside that tab deliberately dropped its
    // status control (the mapping owns the field; the tab one to the left edits
    // it) -- which gives a later "nothing needs status on a form any more"
    // cleanup a plausible wrong target. Gate the select behind the tab's own flag
    // and every test on that tab still passes, while an ORDINARY application --
    // which has no runtime spec and no second tab -- silently loses the only way
    // to take one of its models out of service from the portal.
    const select = screen.getByRole('combobox', { name: t.tableStatus });
    // A non-native MUI Select (shared/SelectField): open it, then click the
    // option. `fireEvent.change` has no value setter to drive here.
    fireEvent.mouseDown(select);
    fireEvent.click(await screen.findByRole('option', { name: t.statusDisabled }));
    fireEvent.click(screen.getByRole('button', { name: t.mappingSave }));

    await waitFor(() => expect(updated).toHaveLength(1));
    expect(updated[0].id).toBe('map_1');
    expect(updated[0].body.status).toBe('disabled');
  });
});

describe('MappingSection vision column', () => {
  it('renders the Vision column value per row (visible by default)', async () => {
    renderSection({
      mappings: [
        makeMapping({ id: 'map_1', gateway_model_name: 'gw-vision', vision_capable: true }),
        makeMapping({ id: 'map_2', gateway_model_name: 'gw-text', vision_capable: false }),
      ],
    });
    await screen.findByText('gw-vision');
    await screen.findByText('gw-text');

    // Not defaultHidden, so it shows without opening the column menu. "Vision"
    // is both the column header label and the true-row cell's chip label, so at
    // least two matches (header + chip) are expected. The false row shows an
    // en-dash (U+2013) — the unified false glyph, not the old em-dash "—".
    expect(screen.getAllByText('Vision').length).toBeGreaterThanOrEqual(2);
    expect(screen.getAllByText('–').length).toBeGreaterThan(0);
  });
});

describe('MappingSection benchmark navigation', () => {
  it('opens the benchmark area scoped to the mapping from the row action (no immediate run)', async () => {
    const { fakeApi } = renderSection({
      mappings: [makeMapping({ id: 'map_1', gateway_model_name: 'gw-model' })],
    });
    await screen.findByText('gw-model');

    // Two "Benchmark" buttons share the label: the panel header (visible text →
    // application scope) and the inline row icon button (no text → mapping scope).
    const rowButton = screen
      .getAllByRole('button', { name: t.runBenchmark })
      .find((b) => b.textContent === '');
    expect(rowButton).toBeTruthy();
    fireEvent.click(rowButton!);

    // It navigates to the consolidated area (never starts a run) — the section
    // heading + breadcrumb crumb render, and no trigger endpoint was called.
    expect(
      await screen.findByRole('heading', { level: 2, name: new RegExp(t.benchmarkArea) }),
    ).toBeInTheDocument();
    expect(fakeApi.benchmarkMapping).not.toHaveBeenCalled();
    expect(fakeApi.benchmarkApplication).not.toHaveBeenCalled();
    expect(fakeApi.benchmarkServer).not.toHaveBeenCalled();
  });

  it('opens the benchmark area scoped to the application from the panel action (no immediate run)', async () => {
    const { fakeApi } = renderSection({
      mappings: [makeMapping({ id: 'map_1', gateway_model_name: 'gw-model' })],
    });
    await screen.findByText('gw-model');

    // The panel header "Benchmark" button carries visible text (the icon-only row
    // action does not), so getByText targets the header (application-scoped) entry.
    fireEvent.click(screen.getByText(t.runBenchmark));

    expect(
      await screen.findByRole('heading', { level: 2, name: new RegExp(t.benchmarkArea) }),
    ).toBeInTheDocument();
    expect(fakeApi.benchmarkApplication).not.toHaveBeenCalled();
    expect(fakeApi.benchmarkMapping).not.toHaveBeenCalled();
    expect(fakeApi.benchmarkServer).not.toHaveBeenCalled();
  });
});

describe('MappingSection manual context-size probe', () => {
  const appWithProbe: PortalApplication = { ...application, context_probe_path: '/props' };

  it('probes the context size and fills the field (without auto-saving)', async () => {
    const probeMappingContext = vi.fn(async () => ({
      running: true,
      server_id: 'srv_1',
      scope: 'context-probe',
      total: 1,
      done: 0,
    })) as unknown as PortalApi['probeMappingContext'];
    const benchmarkStatus = vi.fn(async () => ({
      running: false,
      server_id: 'srv_1',
      scope: 'context-probe',
      total: 1,
      done: 1,
      results: [
        {
          mapping_id: 'map_1',
          gateway_model_name: 'gw-model',
          gen_tokens_per_second: 0,
          prompt_tokens_per_second: 0,
          load_time_ms: 0,
          context_size: 8192,
        },
      ],
    })) as unknown as PortalApi['benchmarkStatus'];

    const { updated } = renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      application: appWithProbe,
      probeMappingContext,
      benchmarkStatus,
    });
    await screen.findByText('gw-model');
    fireEvent.click(screen.getByRole('button', { name: t.mappingEdit }));

    const probeBtn = await screen.findByRole('button', { name: t.mappingProbeContext });
    expect(probeBtn).not.toBeDisabled();
    fireEvent.click(probeBtn);

    await waitFor(() => expect(probeMappingContext).toHaveBeenCalledWith('map_1'));
    // The status poll's reported size fills the context field — fill only.
    await waitFor(() =>
      expect((screen.getByLabelText(t.mappingContextSize) as HTMLInputElement).value).toBe('8192'),
    );
    // Filling the field must NOT trigger a save; the user still saves manually.
    expect(updated).toHaveLength(0);
  });

  it('disables the probe button when the app has no context_probe_path', async () => {
    renderSection({ mappings: [makeMapping({ id: 'map_1' })] }); // module app: context_probe_path === ""
    await screen.findByText('gw-model');
    fireEvent.click(screen.getByRole('button', { name: t.mappingEdit }));

    const probeBtn = await screen.findByRole('button', { name: t.mappingProbeContext });
    expect(probeBtn).toBeDisabled();
  });

  it('disables the probe button while this server has an active run', async () => {
    const activeBenchmarks = vi.fn(async () => [
      { running: true, server_id: 'srv_1', scope: 'server', total: 2, done: 1 },
    ]) as unknown as PortalApi['activeBenchmarks'];
    renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      application: appWithProbe,
      activeBenchmarks,
    });
    await screen.findByText('gw-model');
    fireEvent.click(screen.getByRole('button', { name: t.mappingEdit }));

    const probeBtn = await screen.findByRole('button', { name: t.mappingProbeContext });
    await waitFor(() => expect(probeBtn).toBeDisabled());
  });
});

describe('MappingSection optional metric columns', () => {
  it('exposes the Ladezeit and Prompt-throughput columns (hidden by default) via the column menu', async () => {
    renderSection({
      mappings: [makeMapping({ load_time_ms: 1500, prompt_tokens_per_second: 40.5 })],
    });
    await screen.findByText('gw-model'); // list loaded
    // Both metric columns are hidden by default.
    expect(screen.queryByText('1500')).toBeNull();
    expect(screen.queryByText('40.5')).toBeNull();

    // Open the column-settings menu once and enable both optional metric columns.
    // (The menu is a modal that stays open across checkbox toggles; getByText below
    // ignores the aria-hidden background, so the values are found while it's open.)
    fireEvent.click(screen.getByRole('button', { name: t.listColumns }));
    fireEvent.click(screen.getByRole('checkbox', { name: t.mappingLoadTimeMs }));
    fireEvent.click(screen.getByRole('checkbox', { name: t.mappingPromptTokensPerSecond }));
    expect(screen.getByText('1500')).toBeInTheDocument();
    // Throughput is shown to two decimals (see the formatting test below).
    expect(screen.getByText('40.50')).toBeInTheDocument();
  });

  it('shows throughput to two decimals and energy to ten', async () => {
    renderSection({
      mappings: [
        makeMapping({
          gen_tokens_per_second: 12.3456,
          gen_tokens_per_second_at_capacity: 8.019,
          energy_wh_per_token: 0.0000001234,
        }),
      ],
    });
    await screen.findByText('gw-model');

    fireEvent.click(screen.getByRole('button', { name: t.listColumns }));
    fireEvent.click(screen.getByRole('checkbox', { name: t.mappingGenTokensPerSecond }));
    fireEvent.click(screen.getByRole('checkbox', { name: t.mappingGenTpsAtCapacity }));
    fireEvent.click(screen.getByRole('checkbox', { name: t.mappingEnergyWhPerToken }));

    expect(screen.getByText('12.35')).toBeInTheDocument();
    expect(screen.getByText('8.02')).toBeInTheDocument();
    // Energy per token is a very small number; ten decimals keep it readable.
    expect(screen.getByText('0.0000001234')).toBeInTheDocument();
  });

  it('sorts the numeric metric columns by magnitude, not lexically', async () => {
    // 9 vs 100 is the pair that exposes text sorting: '100' precedes '9'.
    renderSection({
      mappings: [
        makeMapping({ id: 'm1', gateway_model_name: 'gw-hundred', gen_tokens_per_second: 100 }),
        makeMapping({ id: 'm2', gateway_model_name: 'gw-nine', gen_tokens_per_second: 9 }),
        makeMapping({ id: 'm3', gateway_model_name: 'gw-twenty', gen_tokens_per_second: 20 }),
      ],
    });
    await screen.findByText('gw-hundred');

    fireEvent.click(screen.getByRole('button', { name: t.listColumns }));
    fireEvent.click(screen.getByRole('checkbox', { name: t.mappingGenTokensPerSecond }));
    fireEvent.keyDown(screen.getByRole('checkbox', { name: t.mappingGenTokensPerSecond }), {
      key: 'Escape',
    });

    fireEvent.click(await screen.findByRole('button', { name: t.mappingGenTokensPerSecond }));

    await waitFor(() => {
      const shown = screen
        .getAllByRole('row')
        .slice(1)
        .map((row) => row.querySelector('td')?.textContent ?? '');
      expect(shown).toEqual(['gw-nine', 'gw-twenty', 'gw-hundred']);
    });
  });
});
