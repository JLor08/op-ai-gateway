// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { RuntimeAdminSection } from './RuntimeAdminSection';
import { ToastProvider } from './shared/ToastProvider';
import { messages } from '../i18n';
import type {
  CreateMappingRequest,
  PortalApplication,
  PortalModelMapping,
  PortalServer,
  PutRuntimeSpecRequest,
  RuntimeSpec,
  UpdateMappingRequest,
} from '../api';

const t = messages.de;

const server: PortalServer = {
  id: 'srv_1',
  name: 'S1',
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
  type: 'server_agent',
  port: 8081,
  scheme: 'http',
  endpoint: 'http://s1.example.test:8081',
  api_flavors: [],
  priority: 0,
  weight: 0,
  timeout_ms: 600000,
  affinity_ttl_seconds: 1800,
  admission_queue_timeout_seconds: 0,
  status: 'active',
  always_reachable: false,
  health_check_path: '/v1/health',
  health_check_mode: 'health_path',
  health_check_interval_seconds: 0,
  native_responses: false,
  native_messages: false,
  loaded_models_path: '/running',
  loaded_models_format: 'llama_swap',
  context_probe_path: '',
  app_path_suffix: '',
  api_token_set: false,
  api_token_header: '',
  benchmark_schedule_enabled: false,
  benchmark_schedule_interval_seconds: 0,
  opportunistic_metrics_enabled: false,
  proxy_listen_port: 0,
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

function makeSpec(overrides: Partial<RuntimeSpec> = {}): RuntimeSpec {
  return {
    configured: false,
    mapping_id: 'map_1',
    enabled: false,
    binary: '',
    args: [],
    env: {},
    work_dir: '',
    listen_port: 0,
    health_path: '',
    health_timeout_seconds: 0,
    startup_timeout_seconds: 0,
    idle_timeout_seconds: 0,
    admission_wait_timeout_seconds: 0,
    pinned: false,
    admin_state: '',
    vram_locked: false,
    gpus: [],
    ...overrides,
  };
}

function renderSection(
  opts: {
    mappings?: PortalModelMapping[];
    specsByMappingId?: Record<string, RuntimeSpec>;
    warnings?: string[];
  } = {},
) {
  const mappings = opts.mappings ?? [];
  const specsByMappingId = opts.specsByMappingId ?? {};
  const created: CreateMappingRequest[] = [];
  const updatedMappings: { id: string; body: UpdateMappingRequest }[] = [];
  const putSpecs: { mappingId: string; body: PutRuntimeSpecRequest }[] = [];
  const deletedSpecIds: string[] = [];
  const deletedMappingIds: string[] = [];

  const fakeApi = {
    mappings: vi.fn(async () => ({ data: mappings })),
    createMapping: vi.fn(async (_aid: string, body: CreateMappingRequest) => {
      created.push(body);
      return makeMapping({ id: 'map_created', ...(body as Partial<PortalModelMapping>) });
    }),
    updateMapping: vi.fn(async (id: string, body: UpdateMappingRequest) => {
      updatedMappings.push({ id, body });
      return makeMapping({ id, ...(body as Partial<PortalModelMapping>) });
    }),
    deleteMapping: vi.fn(async (id: string) => {
      deletedMappingIds.push(id);
      return { ok: true };
    }),
    runtimeSpec: vi.fn(
      async (mappingId: string) =>
        specsByMappingId[mappingId] ?? makeSpec({ mapping_id: mappingId }),
    ),
    putRuntimeSpec: vi.fn(async (mappingId: string, body: PutRuntimeSpecRequest) => {
      putSpecs.push({ mappingId, body });
      return makeSpec({ configured: true, mapping_id: mappingId, ...body });
    }),
    deleteRuntimeSpec: vi.fn(async (id: string) => {
      deletedSpecIds.push(id);
      return { ok: true };
    }),
    runtimeCoresidency: vi.fn(async () => ({ pairs: [] })),
    putRuntimeCoresidency: vi.fn(async () => ({ pairs: [] })),
    runtimeWarnings: vi.fn(async () => ({ warnings: opts.warnings ?? [] })),
    gpuBudgets: vi.fn(async () => ({ budgets: [] })),
    putGpuBudgets: vi.fn(async () => ({ budgets: [] })),
    runtimeReport: vi.fn(async () => ({ available: false, agent_version: '', agent_features: [] })),
    subscribeRuntimeStatus: vi.fn(() => () => {}),
  };

  render(
    <ToastProvider>
      <RuntimeAdminSection t={t} api={fakeApi} server={server} application={application} />
    </ToastProvider>,
  );
  return { fakeApi, created, updatedMappings, putSpecs, deletedSpecIds, deletedMappingIds };
}

afterEach(cleanup);

describe('RuntimeAdminSection tab strip', () => {
  it('renders the specs/matrix/limits/status tabs, with area-1 stubs for the other three', async () => {
    renderSection();
    // "specs" is the active tab, so its Tab label AND its Panel title render
    // at once (both say "Runtime-Spezifikationen") -- scope to the tab role.
    expect(await screen.findByRole('tab', { name: t.runtimeSpecs })).toBeInTheDocument();
    expect(screen.getByText(t.runtimeMatrix)).toBeInTheDocument();
    expect(screen.getByText(t.runtimeLimits)).toBeInTheDocument();
    expect(screen.getByText(t.runtimeLiveStatus)).toBeInTheDocument();

    fireEvent.click(screen.getByText(t.runtimeMatrix));
    expect(screen.getByText(t.runtimeAreaPlaceholder)).toBeInTheDocument();
  });
});

describe('RuntimeAdminSection launch specs list', () => {
  it('renders the specs list with spec data', async () => {
    renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      specsByMappingId: {
        map_1: makeSpec({
          configured: true,
          mapping_id: 'map_1',
          enabled: true,
          binary: '/usr/local/bin/llama-server',
          pinned: true,
          idle_timeout_seconds: 300,
          gpus: [{ index: 0, vram_estimate_mb: 22000, vram_measured_mb: 0 }],
        }),
      },
    });

    await screen.findByText('gw-model');
    // binary basename, not the full path
    expect(await screen.findByText('llama-server')).toBeInTheDocument();
    expect(screen.getByText('0: 22000 MB')).toBeInTheDocument();
    // The pinned column header AND the pinned-chip cell both carry this text.
    expect(screen.getAllByText(t.runtimeSpecPinned).length).toBeGreaterThanOrEqual(2);
  });

  it('shows the timeout warning banner when the backend reports one', async () => {
    renderSection({ warnings: ['timeout_ms_below_startup_timeout'] });
    expect(await screen.findByText(t.runtimeTimeoutWarning)).toBeInTheDocument();
  });
});

describe('RuntimeAdminSection create (mapping + spec)', () => {
  it('creates a mapping+spec through the form', async () => {
    const { created, putSpecs } = renderSection();
    fireEvent.click(await screen.findByRole('button', { name: t.runtimeSpecCreate }));

    fireEvent.change(screen.getByLabelText(t.mappingGatewayName), {
      target: { value: 'gw-new' },
    });
    fireEvent.change(screen.getByLabelText(t.mappingAppName), { target: { value: 'app-new' } });
    fireEvent.change(screen.getByLabelText(t.runtimeSpecBinary), {
      target: { value: '/usr/bin/llama-server' },
    });
    fireEvent.change(screen.getByLabelText(t.runtimeSpecArgs), {
      target: { value: '--model\n/models/a model.gguf\n--port\n${PORT}' },
    });
    fireEvent.change(screen.getByLabelText(t.runtimeSpecEnv), {
      target: { value: 'CUDA_VISIBLE_DEVICES=0\nTOKEN=${AGENT_ENV:MY_TOKEN}' },
    });

    fireEvent.click(screen.getByRole('button', { name: t.runtimeSpecCreate }));

    await waitFor(() => expect(created).toHaveLength(1));
    expect(created[0].gateway_model_name).toBe('gw-new');
    expect(created[0].app_model_name).toBe('app-new');

    await waitFor(() => expect(putSpecs).toHaveLength(1));
    expect(putSpecs[0].mappingId).toBe('map_created');
    // one argument per line, including the one that legitimately contains a space
    expect(putSpecs[0].body.args).toEqual(['--model', '/models/a model.gguf', '--port', '${PORT}']);
    expect(putSpecs[0].body.env).toEqual({
      CUDA_VISIBLE_DEVICES: '0',
      TOKEN: '${AGENT_ENV:MY_TOKEN}',
    });
    expect(putSpecs[0].body.binary).toBe('/usr/bin/llama-server');

    // Back on the specs list (the create action button only shows there).
    expect(await screen.findByRole('button', { name: t.runtimeSpecCreate })).toBeInTheDocument();
  });

  it('rejects a reserved env key before ever calling the API', async () => {
    const { created, putSpecs } = renderSection();
    fireEvent.click(await screen.findByRole('button', { name: t.runtimeSpecCreate }));
    fireEvent.change(screen.getByLabelText(t.mappingGatewayName), { target: { value: 'gw' } });
    fireEvent.change(screen.getByLabelText(t.mappingAppName), { target: { value: 'app' } });
    fireEvent.change(screen.getByLabelText(t.runtimeSpecBinary), {
      target: { value: '/usr/bin/llama-server' },
    });
    fireEvent.change(screen.getByLabelText(t.runtimeSpecEnv), {
      target: { value: 'PATH=/custom' },
    });

    fireEvent.click(screen.getByRole('button', { name: t.runtimeSpecCreate }));

    expect(await screen.findByText(t.runtimeSpecEnvReserved)).toBeInTheDocument();
    expect(created).toHaveLength(0);
    expect(putSpecs).toHaveLength(0);
  });

  it('reports a partial failure honestly when the spec write fails after the mapping was created', async () => {
    const mappings = [makeMapping({ id: 'map_1' })];
    const fakeApi = {
      mappings: vi.fn(async () => ({ data: mappings })),
      createMapping: vi.fn(async () => makeMapping({ id: 'map_created' })),
      updateMapping: vi.fn(async (id: string, body: UpdateMappingRequest) =>
        makeMapping({ id, ...(body as Partial<PortalModelMapping>) }),
      ),
      deleteMapping: vi.fn(async () => ({ ok: true })),
      runtimeSpec: vi.fn(async (mappingId: string) => makeSpec({ mapping_id: mappingId })),
      putRuntimeSpec: vi.fn(async () => {
        throw new Error('boom');
      }),
      deleteRuntimeSpec: vi.fn(async () => ({ ok: true })),
      runtimeCoresidency: vi.fn(async () => ({ pairs: [] })),
      putRuntimeCoresidency: vi.fn(async () => ({ pairs: [] })),
      runtimeWarnings: vi.fn(async () => ({ warnings: [] })),
      gpuBudgets: vi.fn(async () => ({ budgets: [] })),
      putGpuBudgets: vi.fn(async () => ({ budgets: [] })),
      runtimeReport: vi.fn(async () => ({
        available: false,
        agent_version: '',
        agent_features: [],
      })),
      subscribeRuntimeStatus: vi.fn(() => () => {}),
    };

    render(
      <ToastProvider>
        <RuntimeAdminSection t={t} api={fakeApi} server={server} application={application} />
      </ToastProvider>,
    );

    fireEvent.click(await screen.findByRole('button', { name: t.runtimeSpecCreate }));
    fireEvent.change(screen.getByLabelText(t.mappingGatewayName), { target: { value: 'gw' } });
    fireEvent.change(screen.getByLabelText(t.mappingAppName), { target: { value: 'app' } });
    fireEvent.change(screen.getByLabelText(t.runtimeSpecBinary), {
      target: { value: '/usr/bin/llama-server' },
    });
    fireEvent.click(screen.getByRole('button', { name: t.runtimeSpecCreate }));

    // The mapping WAS created; the toast says so rather than leaving the
    // operator guessing which half landed.
    expect(await screen.findByText(new RegExp(t.runtimeSpecPartialFailure))).toBeInTheDocument();
    expect(fakeApi.createMapping).toHaveBeenCalledTimes(1);
  });
});

describe('RuntimeAdminSection edit + delete', () => {
  it('edits an existing spec', async () => {
    const spec = makeSpec({
      configured: true,
      mapping_id: 'map_1',
      enabled: true,
      binary: '/usr/bin/llama-server',
      args: ['--foo'],
    });
    const { putSpecs, updatedMappings } = renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      specsByMappingId: { map_1: spec },
    });

    await screen.findByText('gw-model');
    fireEvent.click(await screen.findByRole('button', { name: t.runtimeSpecEditAction }));

    const binaryField = await screen.findByLabelText(t.runtimeSpecBinary);
    expect((binaryField as HTMLInputElement).value).toBe('/usr/bin/llama-server');
    fireEvent.change(binaryField, { target: { value: '/usr/bin/llama-server-v2' } });
    fireEvent.click(screen.getByRole('button', { name: t.save }));

    await waitFor(() => expect(putSpecs).toHaveLength(1));
    expect(putSpecs[0].mappingId).toBe('map_1');
    expect(putSpecs[0].body.binary).toBe('/usr/bin/llama-server-v2');
    await waitFor(() => expect(updatedMappings).toHaveLength(1));
    expect(updatedMappings[0].id).toBe('map_1');
  });

  it('deletes a configured spec (not the mapping)', async () => {
    const { deletedSpecIds, deletedMappingIds } = renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      specsByMappingId: { map_1: makeSpec({ configured: true, mapping_id: 'map_1' }) },
    });

    await screen.findByText('gw-model');
    fireEvent.click(await screen.findByRole('button', { name: t.runtimeSpecDelete }));
    fireEvent.click(
      within(screen.getByRole('dialog')).getByRole('button', { name: t.runtimeSpecDelete }),
    );

    await waitFor(() => expect(deletedSpecIds).toEqual(['map_1']));
    expect(deletedMappingIds).toHaveLength(0);
  });

  it('deletes the mapping itself when no spec is configured yet', async () => {
    const { deletedSpecIds, deletedMappingIds } = renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      specsByMappingId: { map_1: makeSpec({ configured: false, mapping_id: 'map_1' }) },
    });

    await screen.findByText('gw-model');
    fireEvent.click(await screen.findByRole('button', { name: t.mappingDelete }));
    fireEvent.click(
      within(screen.getByRole('dialog')).getByRole('button', { name: t.mappingDelete }),
    );

    await waitFor(() => expect(deletedMappingIds).toEqual(['map_1']));
    expect(deletedSpecIds).toHaveLength(0);
  });
});
