// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { RuntimeAdminSection } from './RuntimeAdminSection';
import { ToastProvider } from './shared/ToastProvider';
import { messages } from '../i18n';
import type {
  CreateMappingRequest,
  GPUBudget,
  HardwareGPU,
  HardwareResponse,
  PortalApplication,
  PortalModelMapping,
  PortalServer,
  PutRuntimeSpecRequest,
  RuntimeSpec,
  UpdateMappingRequest,
  UpdateServerRequest,
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

// Minimal-but-valid HardwareResponse for the "server limits" telemetry-prefill
// and drift-warning tests: only the GPU list varies per test, everything else
// is boilerplate HardwareReport shape the type requires.
function makeHardware(gpus: HardwareGPU[]): HardwareResponse {
  return {
    available: true,
    report: {
      collected_at: '2026-07-16T12:00:00Z',
      agent_version: '1.0.0',
      os: 'linux',
      arch: 'amd64',
      cpu: { model: '', vendor: '', physical_cores: 0, logical_threads: 0, base_mhz: 0 },
      memory: { total_bytes: 0 },
      mainboard: { vendor: '', product: '', version: '' },
      bios: { vendor: '', version: '' },
      gpus,
    },
  };
}

function renderSection(
  opts: {
    mappings?: PortalModelMapping[];
    specsByMappingId?: Record<string, RuntimeSpec>;
    warnings?: string[];
    coresidencyPairs?: [string, string][];
    // Never resolves -- simulates the GET still being in flight, for the
    // "must not write before the fetch settles" regression tests.
    coresidencyPending?: boolean;
    gpuBudgets?: GPUBudget[];
    gpuBudgetsPending?: boolean;
    hardware?: HardwareResponse;
    runtimeMaxProcesses?: number;
  } = {},
) {
  const mappings = opts.mappings ?? [];
  const specsByMappingId = opts.specsByMappingId ?? {};
  const created: CreateMappingRequest[] = [];
  const updatedMappings: { id: string; body: UpdateMappingRequest }[] = [];
  const putSpecs: { mappingId: string; body: PutRuntimeSpecRequest }[] = [];
  const deletedSpecIds: string[] = [];
  const deletedMappingIds: string[] = [];
  const putCoresidencyBodies: [string, string][][] = [];
  const putBudgets: GPUBudget[][] = [];
  const updatedServers: { id: string; body: UpdateServerRequest }[] = [];
  const serverForTest: PortalServer = {
    ...server,
    runtime_max_processes: opts.runtimeMaxProcesses,
  };

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
    runtimeCoresidency: vi.fn(() =>
      opts.coresidencyPending
        ? new Promise<{ pairs: [string, string][] }>(() => {})
        : Promise.resolve({ pairs: opts.coresidencyPairs ?? [] }),
    ),
    putRuntimeCoresidency: vi.fn(async (_appId: string, body: { pairs: [string, string][] }) => {
      putCoresidencyBodies.push(body.pairs);
      return { pairs: body.pairs };
    }),
    runtimeWarnings: vi.fn(async () => ({ warnings: opts.warnings ?? [] })),
    gpuBudgets: vi.fn(() =>
      opts.gpuBudgetsPending
        ? new Promise<{ budgets: GPUBudget[] }>(() => {})
        : Promise.resolve({ budgets: opts.gpuBudgets ?? [] }),
    ),
    putGpuBudgets: vi.fn(async (_serverId: string, body: { budgets: GPUBudget[] }) => {
      putBudgets.push(body.budgets);
      return { budgets: body.budgets };
    }),
    runtimeReport: vi.fn(async () => ({ available: false, agent_version: '', agent_features: [] })),
    subscribeRuntimeStatus: vi.fn(() => () => {}),
    updateServer: vi.fn(async (id: string, body: UpdateServerRequest) => {
      updatedServers.push({ id, body });
      // Only runtime_max_processes matters to Area 3's own round trip; the
      // other UpdateServerRequest fields are typed looser (e.g. `status` as
      // a bare `string`) than PortalServer's own field types, so they are
      // deliberately not blindly spread onto the returned server here.
      return { ...serverForTest, id, runtime_max_processes: body.runtime_max_processes };
    }),
    serverHardware: vi.fn(async () => opts.hardware ?? ({ available: false } as HardwareResponse)),
  };

  render(
    <ToastProvider>
      <RuntimeAdminSection t={t} api={fakeApi} server={serverForTest} application={application} />
    </ToastProvider>,
  );
  return {
    fakeApi,
    created,
    updatedMappings,
    putSpecs,
    deletedSpecIds,
    deletedMappingIds,
    putCoresidencyBodies,
    putBudgets,
    updatedServers,
  };
}

afterEach(cleanup);

describe('RuntimeAdminSection tab strip', () => {
  it('renders the specs/matrix/limits/status tabs; matrix + limits are now real, status stays a Task-22 stub', async () => {
    renderSection();
    // "specs" is the active tab, so its Tab label AND its Panel title render
    // at once (both say "Runtime-Spezifikationen") -- scope to the tab role.
    expect(await screen.findByRole('tab', { name: t.runtimeSpecs })).toBeInTheDocument();
    expect(screen.getByText(t.runtimeMatrix)).toBeInTheDocument();
    expect(screen.getByText(t.runtimeLimits)).toBeInTheDocument();
    expect(screen.getByText(t.runtimeLiveStatus)).toBeInTheDocument();

    // Matrix: with zero launch specs (default renderSection), the "need two"
    // hint renders instead of a table -- proves this tab is wired to real
    // data, not the old placeholder.
    fireEvent.click(screen.getByText(t.runtimeMatrix));
    expect(await screen.findByText(t.runtimeMatrixNeedTwo)).toBeInTheDocument();

    // Limits: the GPU-budget/process-limit form renders (also no longer a
    // placeholder).
    fireEvent.click(screen.getByText(t.runtimeLimits));
    expect(await screen.findByLabelText(t.runtimeMaxProcesses)).toBeInTheDocument();

    // Status is Task 22's remaining stub.
    fireEvent.click(screen.getByText(t.runtimeLiveStatus));
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
      updateServer: vi.fn(async (id: string, body: UpdateServerRequest) => ({
        ...server,
        id,
        runtime_max_processes: body.runtime_max_processes,
      })),
      serverHardware: vi.fn(async () => ({ available: false }) as HardwareResponse),
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
    expect(
      await screen.findByText(t.runtimeSpecPartialFailure, { exact: false }),
    ).toBeInTheDocument();
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

// Review round 2, Important 1/2/3: the agent's ExpandPlaceholders classifies
// ${...} occurrences in BOTH args and env values with the exact same rule
// (server-agent/internal/runtime/policy_local.go); the portal form has to
// match it precisely -- neither stricter (would block a legitimate model-
// server templating token) nor looser (would let a spec save that can never
// start). These tests pin both directions, in both fields.
describe('RuntimeAdminSection placeholder validation (mirrors the agent policy)', () => {
  async function openCreateAndFillBase() {
    fireEvent.click(await screen.findByRole('button', { name: t.runtimeSpecCreate }));
    fireEvent.change(screen.getByLabelText(t.mappingGatewayName), { target: { value: 'gw' } });
    fireEvent.change(screen.getByLabelText(t.mappingAppName), { target: { value: 'app' } });
    fireEvent.change(screen.getByLabelText(t.runtimeSpecBinary), {
      target: { value: '/usr/bin/llama-server' },
    });
  }

  it('accepts ${PORT} and ${AGENT_ENV:NAME} exactly, in args and env alike', async () => {
    const { putSpecs } = renderSection();
    await openCreateAndFillBase();
    fireEvent.change(screen.getByLabelText(t.runtimeSpecArgs), {
      target: { value: '--port\n${PORT}' },
    });
    fireEvent.change(screen.getByLabelText(t.runtimeSpecEnv), {
      target: { value: 'TOKEN=${AGENT_ENV:MY_TOKEN}' },
    });
    fireEvent.click(screen.getByRole('button', { name: t.runtimeSpecCreate }));

    await waitFor(() => expect(putSpecs).toHaveLength(1));
    expect(putSpecs[0].body.args).toEqual(['--port', '${PORT}']);
    expect(putSpecs[0].body.env).toEqual({ TOKEN: '${AGENT_ENV:MY_TOKEN}' });
  });

  it('accepts ${TRANSPORT} -- a substring/Contains check would wrongly refuse this', async () => {
    const { created, putSpecs } = renderSection();
    await openCreateAndFillBase();
    fireEvent.change(screen.getByLabelText(t.runtimeSpecArgs), {
      target: { value: '--endpoint\n${TRANSPORT}' },
    });
    fireEvent.click(screen.getByRole('button', { name: t.runtimeSpecCreate }));

    await waitFor(() => expect(created).toHaveLength(1));
    await waitFor(() => expect(putSpecs).toHaveLength(1));
    expect(putSpecs[0].body.args).toEqual(['--endpoint', '${TRANSPORT}']);
  });

  it('accepts ${EXPORT_DIR} and ${MY_AGENT_ENVIRONMENT} in env values', async () => {
    const { putSpecs } = renderSection();
    await openCreateAndFillBase();
    fireEvent.change(screen.getByLabelText(t.runtimeSpecEnv), {
      target: { value: 'A=${EXPORT_DIR}\nB=${MY_AGENT_ENVIRONMENT}' },
    });
    fireEvent.click(screen.getByRole('button', { name: t.runtimeSpecCreate }));

    await waitFor(() => expect(putSpecs).toHaveLength(1));
    expect(putSpecs[0].body.env).toEqual({ A: '${EXPORT_DIR}', B: '${MY_AGENT_ENVIRONMENT}' });
  });

  it('rejects ${PORTX} in an argument as a malformed near-miss', async () => {
    const { created, putSpecs } = renderSection();
    await openCreateAndFillBase();
    fireEvent.change(screen.getByLabelText(t.runtimeSpecArgs), {
      target: { value: '--port\n${PORTX}' },
    });
    fireEvent.click(screen.getByRole('button', { name: t.runtimeSpecCreate }));

    expect(
      await screen.findByText(t.runtimeSpecPlaceholderInvalid, { exact: false }),
    ).toBeInTheDocument();
    expect(created).toHaveLength(0);
    expect(putSpecs).toHaveLength(0);
  });

  it('rejects the lowercase ${port} near-miss in an argument', async () => {
    const { created } = renderSection();
    await openCreateAndFillBase();
    fireEvent.change(screen.getByLabelText(t.runtimeSpecArgs), { target: { value: '${port}' } });
    fireEvent.click(screen.getByRole('button', { name: t.runtimeSpecCreate }));

    expect(
      await screen.findByText(t.runtimeSpecPlaceholderInvalid, { exact: false }),
    ).toBeInTheDocument();
    expect(created).toHaveLength(0);
  });

  it('rejects the empty-name ${AGENT_ENV:} near-miss in an env value', async () => {
    const { created } = renderSection();
    await openCreateAndFillBase();
    fireEvent.change(screen.getByLabelText(t.runtimeSpecEnv), {
      target: { value: 'A=${AGENT_ENV:}' },
    });
    fireEvent.click(screen.getByRole('button', { name: t.runtimeSpecCreate }));

    expect(
      await screen.findByText(t.runtimeSpecPlaceholderInvalid, { exact: false }),
    ).toBeInTheDocument();
    expect(created).toHaveLength(0);
  });

  it('rejects ${AGENT_ENV:OP_AGENT_*} used in an ARGUMENT, not only in env', async () => {
    const { created, putSpecs } = renderSection();
    await openCreateAndFillBase();
    fireEvent.change(screen.getByLabelText(t.runtimeSpecArgs), {
      target: { value: '--token=${AGENT_ENV:OP_AGENT_TOKEN}' },
    });
    fireEvent.click(screen.getByRole('button', { name: t.runtimeSpecCreate }));

    expect(await screen.findByText(t.runtimeSpecEnvReserved)).toBeInTheDocument();
    expect(created).toHaveLength(0);
    expect(putSpecs).toHaveLength(0);
  });
});

// Review round 2, Important 2/3: a load -> open edit -> save WITHOUT any
// change must be a no-op on the wire. Filtering "blank-looking" arg lines or
// trimming env values would silently rewrite the spec on every untouched
// save -- this pins the fix and would fail against the prior filtering
// behaviour.
describe('RuntimeAdminSection edit-without-changes round trip', () => {
  it('preserves a blank arg line, an indented arg, and trailing whitespace in an env value unchanged', async () => {
    const originalArgs = ['--foo', '', '--bar', '  --indented'];
    const originalEnv = { SOME_VAR: 'value with trailing space   ', OTHER: 'plain' };
    const spec = makeSpec({
      configured: true,
      mapping_id: 'map_1',
      binary: '/usr/bin/llama-server',
      args: originalArgs,
      env: originalEnv,
    });
    const { putSpecs } = renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      specsByMappingId: { map_1: spec },
    });

    await screen.findByText('gw-model');
    fireEvent.click(await screen.findByRole('button', { name: t.runtimeSpecEditAction }));
    await screen.findByLabelText(t.runtimeSpecBinary);
    // No edits at all -- just re-save exactly what was loaded.
    fireEvent.click(screen.getByRole('button', { name: t.save }));

    await waitFor(() => expect(putSpecs).toHaveLength(1));
    expect(putSpecs[0].body.args).toEqual(originalArgs);
    expect(putSpecs[0].body.env).toEqual(originalEnv);
  });
});

// Task 21: the matrix tab is wired to real co-residency data (see
// RuntimeMatrix.test.tsx for the component's own rendering/canonicalisation/
// tooltip unit tests) -- these tests pin the SECTION's data flow: computing
// the full replaced pair list and PUTting it as a full replace, never a
// delta.
describe('RuntimeAdminSection co-residency matrix wiring', () => {
  function threeMappings() {
    return [
      makeMapping({ id: 'map_1', gateway_model_name: 'Alpha' }),
      makeMapping({ id: 'map_2', gateway_model_name: 'Bravo' }),
      makeMapping({ id: 'map_3', gateway_model_name: 'Charlie' }),
    ];
  }

  it('toggling a new cell persists the FULL replaced pair list, not just the toggled pair', async () => {
    const { putCoresidencyBodies } = renderSection({
      mappings: threeMappings(),
      coresidencyPairs: [['map_1', 'map_2']],
    });

    fireEvent.click(await screen.findByText(t.runtimeMatrix));
    fireEvent.click(
      await screen.findByRole('button', { name: `${t.runtimeMatrixCell}: Charlie + Alpha` }),
    );

    await waitFor(() => expect(putCoresidencyBodies).toHaveLength(1));
    // The pre-existing pair survives AND the newly toggled one is added -- a
    // delta-only (buggy) implementation would send just [["map_1","map_3"]].
    expect(putCoresidencyBodies[0]).toHaveLength(2);
    expect(putCoresidencyBodies[0]).toEqual(
      expect.arrayContaining([
        ['map_1', 'map_2'],
        ['map_1', 'map_3'],
      ]),
    );
  });

  it('un-toggling an already-allowed pair removes only that pair from the persisted list', async () => {
    const { putCoresidencyBodies } = renderSection({
      mappings: threeMappings(),
      coresidencyPairs: [
        ['map_1', 'map_2'],
        ['map_1', 'map_3'],
      ],
    });

    fireEvent.click(await screen.findByText(t.runtimeMatrix));
    fireEvent.click(
      await screen.findByRole('button', { name: `${t.runtimeMatrixCell}: Bravo + Alpha` }),
    );

    await waitFor(() => expect(putCoresidencyBodies).toHaveLength(1));
    expect(putCoresidencyBodies[0]).toEqual([['map_1', 'map_3']]);
  });

  // Review round 1, CRITICAL: `coresidencyData ?? []` collapses "GET still
  // loading" and "GET resolved to genuinely empty" into the same value. If
  // the matrix rendered (and accepted clicks) from that collapsed `[]`
  // before the real GET settled, a single click would PUT a full-replace
  // list containing ONLY the just-toggled pair -- silently erasing every
  // pair a previous admin had already saved. The fix is to render nothing
  // clickable until the fetch has actually resolved.
  it('CRITICAL: shows a loading state and performs no write while the co-residency GET is still pending', async () => {
    const { fakeApi } = renderSection({
      mappings: threeMappings(),
      coresidencyPending: true,
    });

    fireEvent.click(await screen.findByText(t.runtimeMatrix));

    // The matrix must not exist yet -- there is nothing to click, so there
    // is nothing that could PUT an empty replacement list.
    await screen.findByText(t.loading);
    expect(
      screen.queryByRole('button', { name: `${t.runtimeMatrixCell}: Charlie + Alpha` }),
    ).not.toBeInTheDocument();
    expect(fakeApi.putRuntimeCoresidency).not.toHaveBeenCalled();
  });
});

// Task 21: "server limits" -- GPU budget rows prefilled from live telemetry,
// the never-blocking UUID-drift warning, and saving budgets + the process
// limit together.
describe('RuntimeAdminSection server limits wiring', () => {
  // Review round 1, CRITICAL: `budgetRows` starts `[]` and is only ever
  // re-seeded once the GET resolves. If Save were reachable before that,
  // `budgetRows` (still `[]`) would PUT as the full replacement -- erasing
  // every previously configured per-GPU budget on a single premature click.
  // The fix is to render nothing (no Save button, no fields) until the
  // fetch has actually resolved.
  it('CRITICAL: shows a loading state and performs no write while the GPU-budgets GET is still pending', async () => {
    const { fakeApi } = renderSection({ gpuBudgetsPending: true });

    fireEvent.click(await screen.findByText(t.runtimeLimits));

    await screen.findByText(t.loading);
    // Neither the Save button nor the process-limit field exist yet -- there
    // is no way to trigger a write.
    expect(screen.queryByRole('button', { name: t.save })).not.toBeInTheDocument();
    expect(screen.queryByLabelText(t.runtimeMaxProcesses)).not.toBeInTheDocument();
    expect(fakeApi.putGpuBudgets).not.toHaveBeenCalled();
    expect(fakeApi.updateServer).not.toHaveBeenCalled();
  });

  it("prefills a new budget row's index and VRAM (in MB) from live telemetry", async () => {
    renderSection({
      hardware: makeHardware([
        {
          index: 0,
          name: 'RTX 4090',
          uuid: 'GPU-aaa',
          memory_total_bytes: 24 * 1024 * 1024 * 1024,
        },
      ]),
    });

    fireEvent.click(await screen.findByText(t.runtimeLimits));
    fireEvent.click(await screen.findByRole('button', { name: t.runtimeSpecGpuAdd }));

    const indexField = await screen.findByLabelText(t.runtimeSpecGpuIndex);
    expect((indexField as HTMLInputElement).value).toBe('0');
    const budgetField = screen.getByLabelText(t.runtimeGpuBudget);
    expect((budgetField as HTMLInputElement).value).toBe('24576'); // 24 GiB, in MB
  });

  it('renders a UUID-drift warning without disabling any control on that row', async () => {
    renderSection({
      hardware: makeHardware([
        { index: 0, name: 'RTX 5090', uuid: 'GPU-live', memory_total_bytes: 0 },
      ]),
      gpuBudgets: [
        { index: 0, budget_mb: 24000, expected_uuid: 'GPU-old', expected_name: 'RTX 4090' },
      ],
    });

    fireEvent.click(await screen.findByText(t.runtimeLimits));
    const warningIcon = await screen.findByRole('button', {
      name: `${t.runtimeGpuDriftWarning}: GPU 0`,
    });
    expect(warningIcon).not.toBeDisabled();
    expect(screen.getByLabelText(t.runtimeSpecGpuIndex)).not.toBeDisabled();
    expect(screen.getByLabelText(t.runtimeGpuBudget)).not.toBeDisabled();
  });

  it('shows no drift warning when the GPU reports no UUID (AMD/Apple -- no drift detection available, not "drift detected")', async () => {
    renderSection({
      hardware: makeHardware([{ index: 0, name: 'Apple GPU', memory_total_bytes: 0 }]),
      gpuBudgets: [{ index: 0, budget_mb: 10000, expected_uuid: '', expected_name: 'Apple GPU' }],
    });

    fireEvent.click(await screen.findByText(t.runtimeLimits));
    await screen.findByLabelText(t.runtimeGpuBudget);
    expect(
      screen.queryByRole('button', { name: `${t.runtimeGpuDriftWarning}: GPU 0` }),
    ).not.toBeInTheDocument();
  });

  it('saves the GPU budgets and the process limit together', async () => {
    const { putBudgets, updatedServers } = renderSection({
      gpuBudgets: [{ index: 0, budget_mb: 40000, expected_uuid: '', expected_name: '' }],
      runtimeMaxProcesses: 2,
    });

    fireEvent.click(await screen.findByText(t.runtimeLimits));
    const maxProcField = await screen.findByLabelText(t.runtimeMaxProcesses);
    expect((maxProcField as HTMLInputElement).value).toBe('2');
    fireEvent.change(maxProcField, { target: { value: '5' } });
    fireEvent.click(screen.getByRole('button', { name: t.save }));

    await waitFor(() => expect(putBudgets).toHaveLength(1));
    expect(putBudgets[0]).toEqual([
      { index: 0, budget_mb: 40000, expected_uuid: '', expected_name: '' },
    ]);
    await waitFor(() => expect(updatedServers).toHaveLength(1));
    expect(updatedServers[0].body.runtime_max_processes).toBe(5);
  });
});
