// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  OVERRIDE_WRITE_TIMEOUT_MS,
  RESTART_STOP_TIMEOUT_MS,
  RESTART_VANISH_GRACE_MS,
  RuntimeAdminSection,
} from './RuntimeAdminSection';
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
  RuntimeReport,
  RuntimeReportContent,
  RuntimeSpec,
  RuntimeStatus,
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

function makeStatus(overrides: Partial<RuntimeStatus> = {}): RuntimeStatus {
  return {
    spec_id: 'spec_1',
    model: 'app-model',
    state: 'running',
    since: '2026-07-16T12:00:00Z',
    in_flight: 0,
    restarts: 0,
    ...overrides,
  };
}

// The default (gateway-source) runtime report: nothing ever reported, so the
// screen stays writable. agent_version/agent_features are always present on
// this DTO (they come from the latest telemetry row, not the report).
function makeReport(overrides: Partial<RuntimeReport> = {}): RuntimeReport {
  return { available: false, agent_version: '', agent_features: [], ...overrides };
}

// A file-mode report: `source: 'file'` on the NESTED report object (the
// RuntimeReport itself has no `source` field), which is the whole read-only
// trigger for this screen.
function fileModeReport(
  config: unknown,
  extra: Partial<RuntimeReportContent> = {},
  reportOverrides: Partial<RuntimeReport> = {},
): RuntimeReport {
  return {
    available: true,
    collected_at: '2026-07-16T12:00:00Z',
    updated_at: '2026-07-16T12:00:00Z',
    agent_version: '0.2.0',
    agent_features: ['runtime_manager'],
    report: { source: 'file', collected_at: '2026-07-16T12:00:00Z', config, ...extra },
    ...reportOverrides,
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
    // Task 22: the file-mode/feature-negotiation report, and the live SSE.
    report?: RuntimeReport;
    reportPending?: boolean;
    // Fix round 1, I3: `useResource` on error sets `error` and leaves
    // `data === null`, so a FAILED report GET is a THIRD fact, not the same
    // one as a pending GET. The FIRST call rejects and every later one
    // resolves, so a single test can drive both the failure render and the
    // retry that recovers from it.
    reportFailing?: boolean;
    // Fix round 2: the OTHER failure shape -- a GET that fails while `data`
    // still holds an earlier payload (`useResource` never clears it), which on
    // this screen is reachable by switching servers, since `server.id` is a
    // loader dep. 1-based call index.
    reportFailsOnCall?: number;
    // Pushed synchronously from inside the subscribe call, i.e. exactly like
    // the stream's `snapshot` frame arriving on connect.
    statusRows?: RuntimeStatus[];
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
  // Captured from the subscribe call so a test can drive the live stream
  // frame by frame (both `snapshot` and `update` are full replacements).
  let onDataCb: ((rows: RuntimeStatus[]) => void) | null = null;
  let onStatusCb: ((status: 'open' | 'error') => void) | null = null;
  let unsubscribeCount = 0;
  let reportCalls = 0;
  const subscribedServerIds: string[] = [];

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
      // `id` (the SPEC id, the join key against the live status stream) is
      // preserved exactly as the backend does -- a PUT never re-keys the row.
      return makeSpec({
        configured: true,
        mapping_id: mappingId,
        id: specsByMappingId[mappingId]?.id,
        ...body,
      });
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
    runtimeReport: vi.fn(() => {
      reportCalls += 1;
      if (opts.reportPending) return new Promise<RuntimeReport>(() => {});
      if (opts.reportFailing && reportCalls === 1) {
        return Promise.reject(new Error('report unavailable'));
      }
      if (opts.reportFailsOnCall === reportCalls) {
        return Promise.reject(new Error('report unavailable'));
      }
      return Promise.resolve(opts.report ?? makeReport());
    }),
    subscribeRuntimeStatus: vi.fn(
      (
        _serverId: string,
        onData: (rows: RuntimeStatus[]) => void,
        onStatus?: (status: 'open' | 'error') => void,
      ) => {
        onDataCb = onData;
        onStatusCb = onStatus ?? null;
        subscribedServerIds.push(_serverId);
        if (opts.statusRows) onData(opts.statusRows);
        return () => {
          unsubscribeCount++;
        };
      },
    ),
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

  const view = render(
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
    unmount: view.unmount,
    stream: {
      /** One full-replacement frame (`snapshot` or `update` -- same shape). */
      push: (rows: RuntimeStatus[]) => act(() => onDataCb?.(rows)),
      /**
       * A frame pushed WITHOUT act(), i.e. handed to the component but not
       * yet flushed to the DOM. That is what makes the render-vs-click race
       * reproducible: the button in the document still carries the previous
       * render's `disabled` and its onClick still closes over the previous
       * render's row, exactly as in the browser during the ~1 s between two
       * frames.
       */
      pushUnflushed: (rows: RuntimeStatus[]) => onDataCb?.(rows),
      setStatus: (status: 'open' | 'error') => act(() => onStatusCb?.(status)),
      unsubscribes: () => unsubscribeCount,
      subscribedServerIds,
    },
    /**
     * Unmounts ONLY RuntimeAdminSection, leaving the ToastProvider mounted
     * above it. `view.unmount()` tears the provider down too, so a toast
     * pushed after unmount has nowhere to render and the assertion passes
     * for the wrong reason; this one observes the component's own
     * mounted-guard directly.
     */
    detachSection: () => view.rerender(<ToastProvider>{null}</ToastProvider>),
    /** Re-renders with a different server, to exercise a mid-flow switch. */
    rerenderWithServer: (id: string) =>
      view.rerender(
        <ToastProvider>
          <RuntimeAdminSection
            t={t}
            api={fakeApi}
            server={{ ...serverForTest, id }}
            application={application}
          />
        </ToastProvider>,
      ),
  };
}

afterEach(cleanup);

describe('RuntimeAdminSection tab strip', () => {
  it('renders the specs/matrix/limits/status tabs, all four wired to real data', async () => {
    const { stream } = renderSection();
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

    // Status: the live table renders its own empty state (the stream is open
    // and reports nothing), no longer the generic area placeholder.
    stream.setStatus('open');
    fireEvent.click(screen.getByText(t.runtimeLiveStatus));
    expect(await screen.findByText(t.runtimeStatusEmpty)).toBeInTheDocument();
    expect(screen.queryByText(t.runtimeAreaPlaceholder)).not.toBeInTheDocument();
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

  // Review round 1, Important: two clicks in quick succession must not let a
  // slow first response overwrite state a second toggle already advanced --
  // that would silently drop the second pair on the next write. The chosen
  // fix mirrors Area 3's `limitsBusy`: disable the whole matrix while a PUT
  // is outstanding, so a second click physically cannot fire until the
  // first response has been reconciled.
  it('IMPORTANT: disables the matrix while a toggle PUT is in flight, so a second click cannot race a still-pending response', async () => {
    let resolveFirst: (v: { pairs: [string, string][] }) => void = () => {};
    const firstPut = new Promise<{ pairs: [string, string][] }>((resolve) => {
      resolveFirst = resolve;
    });
    const { fakeApi, putCoresidencyBodies } = renderSection({
      mappings: threeMappings(),
      coresidencyPairs: [],
    });
    fakeApi.putRuntimeCoresidency.mockImplementationOnce(async () => firstPut);

    fireEvent.click(await screen.findByText(t.runtimeMatrix));
    const firstCell = await screen.findByRole('button', {
      name: `${t.runtimeMatrixCell}: Bravo + Alpha`,
    });
    fireEvent.click(firstCell);

    const secondCell = await screen.findByRole('button', {
      name: `${t.runtimeMatrixCell}: Charlie + Bravo`,
    });
    await waitFor(() => expect(secondCell).toBeDisabled());
    fireEvent.click(secondCell); // disabled -- must be a no-op
    expect(fakeApi.putRuntimeCoresidency).toHaveBeenCalledTimes(1);

    resolveFirst({ pairs: [['map_1', 'map_2']] });
    await waitFor(() => expect(secondCell).not.toBeDisabled());

    fireEvent.click(secondCell);
    await waitFor(() => expect(fakeApi.putRuntimeCoresidency).toHaveBeenCalledTimes(2));
    // Only the second call reaches the default (pushing) mock implementation
    // -- the first was overridden above -- so exactly one entry is expected,
    // and it must contain BOTH the reconciled first pair and the new one.
    expect(putCoresidencyBodies).toHaveLength(1);
    expect(putCoresidencyBodies[0]).toHaveLength(2);
    expect(putCoresidencyBodies[0]).toEqual(
      expect.arrayContaining([
        ['map_1', 'map_2'],
        ['map_2', 'map_3'],
      ]),
    );
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
      name: `${t.runtimeGpuDriftIconLabel}: GPU 0`,
    });
    expect(warningIcon).not.toBeDisabled();
    expect(screen.getByLabelText(t.runtimeSpecGpuIndex)).not.toBeDisabled();
    expect(screen.getByLabelText(t.runtimeGpuBudget)).not.toBeDisabled();
  });

  it('shows no drift warning when the GPU reports no UUID, even if expected_name differs (AMD/Apple -- no drift detection available, not "drift detected")', async () => {
    renderSection({
      // Live name deliberately differs from the stored expected_name -- the
      // drift check compares ONLY the UUID (per the brief); a name mismatch
      // alone must never trigger a warning when no UUID is available on
      // either side.
      hardware: makeHardware([{ index: 0, name: 'Apple M3 Max GPU', memory_total_bytes: 0 }]),
      gpuBudgets: [
        { index: 0, budget_mb: 10000, expected_uuid: '', expected_name: 'Apple M2 Max GPU' },
      ],
    });

    fireEvent.click(await screen.findByText(t.runtimeLimits));
    await screen.findByLabelText(t.runtimeGpuBudget);
    expect(
      screen.queryByRole('button', { name: `${t.runtimeGpuDriftIconLabel}: GPU 0` }),
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

// ---------------------------------------------------------------------------
// Task 22: live status, admin overrides, the restart sequence, file mode and
// the feature-mismatch banner.
// ---------------------------------------------------------------------------

// A fully populated, deliberately NON-default spec: every field carries a
// value that a synthesized/defaulted PUT body would get wrong. Used by the
// override tests, which compare the PUT body field by field.
function fullSpec(overrides: Partial<RuntimeSpec> = {}): RuntimeSpec {
  return makeSpec({
    configured: true,
    id: 'spec_1',
    mapping_id: 'map_1',
    enabled: true,
    binary: '/usr/local/bin/llama-server',
    args: ['--model', '/models/a model.gguf', '--port', '${PORT}'],
    env: { CUDA_VISIBLE_DEVICES: '0', HF_HOME: '/data/hf' },
    work_dir: '/opt/models',
    listen_port: 8099,
    health_path: '/healthz',
    health_timeout_seconds: 7,
    startup_timeout_seconds: 300,
    idle_timeout_seconds: 600,
    admission_wait_timeout_seconds: 45,
    pinned: true,
    admin_state: '',
    vram_locked: true,
    gpus: [{ index: 1, vram_estimate_mb: 22000, vram_measured_mb: 21500 }],
    ...overrides,
  });
}

// The PUT body the override actions MUST send: the loaded spec verbatim,
// minus the three server-owned fields, with only admin_state replaced.
function expectedBody(spec: RuntimeSpec, adminState: string): PutRuntimeSpecRequest {
  return {
    enabled: spec.enabled,
    binary: spec.binary,
    args: spec.args,
    env: spec.env,
    work_dir: spec.work_dir,
    listen_port: spec.listen_port,
    health_path: spec.health_path,
    health_timeout_seconds: spec.health_timeout_seconds,
    startup_timeout_seconds: spec.startup_timeout_seconds,
    idle_timeout_seconds: spec.idle_timeout_seconds,
    admission_wait_timeout_seconds: spec.admission_wait_timeout_seconds,
    pinned: spec.pinned,
    admin_state: adminState,
    vram_locked: spec.vram_locked,
    gpus: spec.gpus,
  };
}

async function openStatusTab() {
  fireEvent.click(await screen.findByRole('tab', { name: t.runtimeLiveStatus }));
}

/**
 * Scopes queries to the table row carrying `text`. Needed as soon as a test
 * has more than one status row: every row renders the same action labels, so
 * an unscoped `getByRole('button', { name: t.runtimeRestart })` would throw
 * on the multiple match rather than tell us anything about a specific row.
 */
function inRowWith(text: string) {
  const rows = screen.getAllByRole('row').filter((r) => r.textContent?.includes(text));
  if (rows.length !== 1) {
    throw new Error(`expected exactly one table row containing ${text}, found ${rows.length}`);
  }
  return within(rows[0]);
}

describe('RuntimeAdminSection live status list', () => {
  it('renders one row per reported process with its state, since, pid, port, in-flight and restarts', async () => {
    const { stream } = renderSection({
      statusRows: [
        makeStatus({
          spec_id: 'spec_1',
          model: 'app-model',
          state: 'running',
          pid: 4242,
          port: 8099,
          in_flight: 3,
          restarts: 2,
        }),
      ],
    });
    stream.setStatus('open');
    await openStatusTab();

    expect(await screen.findByText('app-model')).toBeInTheDocument();
    expect(screen.getByText(t.runtimeStateRunning)).toHaveAttribute('data-status', 'active');
    expect(screen.getByText('4242')).toBeInTheDocument();
    expect(screen.getByText('8099')).toBeInTheDocument();
    expect(screen.getByText('3')).toBeInTheDocument();
    expect(screen.getByText('2')).toBeInTheDocument();
  });

  // Correction 3: the portal has exactly THREE status colours (success/watch/
  // standby; theme/ThemeRoot.tsx) and statusClassByKey collapses
  // error/disabled/expired onto standby, so a badge map naming 'error' or
  // 'disabled' renders the same grey chip as 'standby'. The colour therefore
  // cannot carry the state; the LABEL has to. This pins both halves: the
  // three colours that DO differ, and nine distinct labels.
  it('uses only the three real colours and lets the label carry every state distinction', async () => {
    const states = [
      'running',
      'starting',
      'pending_vram_unknown',
      'stopped',
      'draining',
      'backoff',
      'crashed',
      'start_failed',
      'not_permitted',
    ];
    const { stream } = renderSection({
      statusRows: states.map((state, i) =>
        makeStatus({ spec_id: `spec_${i}`, model: `m-${state}`, state }),
      ),
    });
    stream.setStatus('open');
    await openStatusTab();

    await screen.findByText('m-running');
    expect(screen.getByText(t.runtimeStateRunning)).toHaveAttribute('data-status', 'active');
    expect(screen.getByText(t.runtimeStateStarting)).toHaveAttribute('data-status', 'watch');
    expect(screen.getByText(t.runtimeStatePendingVram)).toHaveAttribute('data-status', 'watch');
    expect(screen.getByText(t.runtimeStateStopped)).toHaveAttribute('data-status', 'standby');
    expect(screen.getByText(t.runtimeStateDraining)).toHaveAttribute('data-status', 'standby');
    expect(screen.getByText(t.runtimeStateBackoff)).toHaveAttribute('data-status', 'standby');
    expect(screen.getByText(t.runtimeStateCrashed)).toHaveAttribute('data-status', 'standby');
    expect(screen.getByText(t.runtimeStateStartFailed)).toHaveAttribute('data-status', 'standby');
    expect(screen.getByText(t.runtimeStateNotPermitted)).toHaveAttribute('data-status', 'standby');
  });

  it('falls back to the raw wire value for a state this portal build does not know', async () => {
    const { stream } = renderSection({
      statusRows: [makeStatus({ state: 'quantum_superposition' })],
    });
    stream.setStatus('open');
    await openStatusTab();
    expect(await screen.findByText('quantum_superposition')).toBeInTheDocument();
  });

  // Two facts, one view: "nothing is running" and "we cannot see what is
  // running" must not look alike. An implementation that only ever renders
  // the generic list-empty text fails this.
  it('tells "no process reported" apart from "the stream is down"', async () => {
    const { stream } = renderSection();
    stream.setStatus('error');
    await openStatusTab();

    expect(await screen.findByText(t.runtimeStreamError)).toBeInTheDocument();
    expect(screen.queryByText(t.runtimeStatusEmpty)).not.toBeInTheDocument();

    stream.setStatus('open');
    expect(await screen.findByText(t.runtimeStatusEmpty)).toBeInTheDocument();
    expect(screen.queryByText(t.runtimeStreamError)).not.toBeInTheDocument();
  });

  // THE crux of this task (brief, "The failure signal"): last_error is
  // cleared only by the next SUCCESSFUL start, never by a state change, so a
  // spec can be `stopped` and still carry "last attempt failed". "Last load
  // failed" is therefore not a state, and no state chip can convey it -- it
  // needs its own always-visible affordance. An implementation that shows
  // last_error only for crashed/start_failed rows fails this.
  it('shows the last error on a STOPPED row, because last_error survives state changes', async () => {
    const { stream } = renderSection({
      statusRows: [
        makeStatus({
          state: 'stopped',
          last_error: {
            message: 'CUDA error: out of memory',
            at: '2026-07-15T14:32:00Z',
            exit_code: 1,
            failures: 3,
            stderr_tail: 'ggml_cuda_host_malloc: failed to allocate',
          },
        }),
      ],
    });
    stream.setStatus('open');
    await openStatusTab();

    expect(await screen.findByText(t.runtimeStateStopped)).toHaveAttribute(
      'data-status',
      'standby',
    );
    // Visible in the row itself, not hidden behind a hover-only tooltip.
    expect(screen.getByText('CUDA error: out of memory')).toBeInTheDocument();
  });

  it('replaces the row set on every frame instead of appending', async () => {
    const { stream } = renderSection();
    stream.setStatus('open');
    await openStatusTab();

    stream.push([
      makeStatus({ spec_id: 'spec_1', model: 'model-a' }),
      makeStatus({ spec_id: 'spec_2', model: 'model-b' }),
    ]);
    expect(await screen.findByText('model-a')).toBeInTheDocument();
    expect(screen.getByText('model-b')).toBeInTheDocument();

    stream.push([makeStatus({ spec_id: 'spec_1', model: 'model-a' })]);
    await waitFor(() => expect(screen.queryByText('model-b')).not.toBeInTheDocument());
    expect(screen.getByText('model-a')).toBeInTheDocument();
  });

  it('unsubscribes from the stream on unmount', async () => {
    const { stream, unmount } = renderSection();
    await openStatusTab();
    expect(stream.unsubscribes()).toBe(0);
    unmount();
    expect(stream.unsubscribes()).toBe(1);
  });

  it('joins the live state into the launch-specs list by spec_id', async () => {
    const { stream } = renderSection({
      mappings: [
        makeMapping({ id: 'map_1', gateway_model_name: 'Alpha' }),
        makeMapping({ id: 'map_2', gateway_model_name: 'Bravo' }),
      ],
      specsByMappingId: {
        map_1: makeSpec({ configured: true, id: 'spec_1', mapping_id: 'map_1' }),
        map_2: makeSpec({ configured: true, id: 'spec_2', mapping_id: 'map_2' }),
      },
      statusRows: [makeStatus({ spec_id: 'spec_1', state: 'starting' })],
    });
    stream.setStatus('open');

    // Specs tab is the default one.
    await screen.findByText('Alpha');
    // Alpha's spec is reported starting; Bravo's spec has no live status.
    expect(await screen.findByText(t.runtimeStateStarting)).toBeInTheDocument();
    expect(screen.getByText(t.runtimeStatusUnknown)).toBeInTheDocument();
  });
});

describe('RuntimeAdminSection admin overrides', () => {
  // The plan's third full-replace hazard: putRuntimeSpec's body is the ENTIRE
  // spec, so a synthesized or defaulted body silently overwrites the
  // operator's configured command line. Comparing the whole body field by
  // field is the only assertion that can see that; checking admin_state alone
  // cannot.
  it('force-stop PUTs the loaded spec verbatim with ONLY admin_state changed', async () => {
    const spec = fullSpec();
    const { putSpecs, stream } = renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      specsByMappingId: { map_1: spec },
      statusRows: [makeStatus({ spec_id: 'spec_1' })],
    });
    stream.setStatus('open');
    await openStatusTab();

    fireEvent.click(await screen.findByRole('button', { name: t.runtimeForceStop }));

    await waitFor(() => expect(putSpecs).toHaveLength(1));
    expect(putSpecs[0].mappingId).toBe('map_1');
    expect(putSpecs[0].body).toEqual(expectedBody(spec, 'force_stopped'));
  });

  it('clear-override PUTs the loaded spec verbatim with admin_state emptied', async () => {
    const spec = fullSpec({ admin_state: 'force_stopped' });
    const { putSpecs, stream } = renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      specsByMappingId: { map_1: spec },
      statusRows: [makeStatus({ spec_id: 'spec_1', state: 'stopped' })],
    });
    stream.setStatus('open');
    await openStatusTab();

    fireEvent.click(await screen.findByRole('button', { name: t.runtimeClearOverride }));

    await waitFor(() => expect(putSpecs).toHaveLength(1));
    expect(putSpecs[0].body).toEqual(expectedBody(spec, ''));
  });

  it('shows the override actions the current admin_state allows, and only those', async () => {
    const { stream } = renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      specsByMappingId: { map_1: fullSpec({ admin_state: 'force_stopped' }) },
      statusRows: [makeStatus({ spec_id: 'spec_1', state: 'stopped' })],
    });
    stream.setStatus('open');
    await openStatusTab();

    expect(await screen.findByRole('button', { name: t.runtimeForceStart })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: t.runtimeClearOverride })).toBeInTheDocument();
    // Already force_stopped -- forcing it again is a no-op, and a restart
    // would silently end with a DIFFERENT override than it started with.
    expect(screen.queryByRole('button', { name: t.runtimeForceStop })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: t.runtimeRestart })).not.toBeInTheDocument();
  });

  // Brief: "If no loaded spec matches the row's spec_id ... render NO
  // override buttons for that row rather than falling back to a synthesized
  // body." A synthesized body would wipe the operator's command line.
  it('renders no override actions for a row whose spec is not loaded', async () => {
    const { fakeApi, stream } = renderSection({
      mappings: [],
      statusRows: [makeStatus({ spec_id: 'spec_from_nowhere', model: 'orphan' })],
    });
    stream.setStatus('open');
    await openStatusTab();

    expect(await screen.findByText('orphan')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: t.runtimeForceStop })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: t.runtimeForceStart })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: t.runtimeRestart })).not.toBeInTheDocument();
    expect(fakeApi.putRuntimeSpec).not.toHaveBeenCalled();
  });

  // Same gating discipline as areas 2/3: until the runtime-report GET has
  // settled we do not know whether this screen is about to become read-only,
  // so nothing writable may be presented.
  it('presents no override actions while the runtime-report GET is still pending', async () => {
    const { fakeApi, stream } = renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      specsByMappingId: { map_1: fullSpec() },
      statusRows: [makeStatus({ spec_id: 'spec_1' })],
      reportPending: true,
    });
    stream.setStatus('open');
    await openStatusTab();

    // Fix round 1: this row IS resolvable to a mapping, so the agent-reported
    // name now renders as the subordinate line ("Reported by the agent:
    // app-model") beneath the gateway name -- hence the substring match. The
    // assertion's point is unchanged: the row renders, its actions do not.
    // The row renders -- only its ACTIONS are withheld. Waiting on the
    // GATEWAY name specifically: the per-mapping spec GETs settle after the
    // first paint, so asserting on the agent-reported name alone can match
    // the pre-resolution node that the resolved re-render then replaces.
    expect(await screen.findByText('gw-model')).toBeInTheDocument();
    expect(screen.getByText('app-model', { exact: false })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: t.runtimeForceStop })).not.toBeInTheDocument();
    expect(fakeApi.putRuntimeSpec).not.toHaveBeenCalled();
  });
});

describe('RuntimeAdminSection restart sequence', () => {
  function setupRestart(rowState = 'running') {
    const spec = fullSpec();
    const handles = renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      specsByMappingId: { map_1: spec },
      statusRows: [makeStatus({ spec_id: 'spec_1', state: rowState })],
    });
    handles.stream.setStatus('open');
    return { spec, ...handles };
  }

  it('runs force_stopped -> await stopped -> clear override, preserving every other spec field', async () => {
    const { spec, putSpecs, stream } = setupRestart();
    await openStatusTab();

    fireEvent.click(await screen.findByRole('button', { name: t.runtimeRestart }));
    await waitFor(() => expect(putSpecs).toHaveLength(1));
    expect(putSpecs[0].body).toEqual(expectedBody(spec, 'force_stopped'));
    // Visible progress while the sequence waits for the stream.
    expect(await screen.findByText(t.runtimeRestartStopping)).toBeInTheDocument();

    // Still running -> nothing further happens.
    stream.push([makeStatus({ spec_id: 'spec_1', state: 'draining' })]);
    expect(putSpecs).toHaveLength(1);

    // The `stopped` frame is the only signal that force_stopped took effect.
    stream.push([makeStatus({ spec_id: 'spec_1', state: 'stopped' })]);
    await waitFor(() => expect(putSpecs).toHaveLength(2));
    expect(putSpecs[1].body).toEqual(expectedBody(spec, ''));
    await waitFor(() =>
      expect(screen.queryByText(t.runtimeRestartStopping)).not.toBeInTheDocument(),
    );
  });

  // The likeliest case in practice, and the one a happy-path-only test leaves
  // unproven: the child never reports `stopped`. The wait MUST be bounded and
  // the clear PUT must NOT be sent (the override is still in effect, and the
  // operator has to decide what to do).
  it('bounds the wait: a wedged child surfaces a timeout and never sends the clear', async () => {
    vi.useFakeTimers();
    try {
      const { putSpecs, stream } = setupRestart();
      await act(async () => {
        await Promise.resolve();
      });
      fireEvent.click(screen.getByRole('tab', { name: t.runtimeLiveStatus }));
      fireEvent.click(screen.getByRole('button', { name: t.runtimeRestart }));
      await act(async () => {
        await Promise.resolve();
      });
      expect(putSpecs).toHaveLength(1);

      // Frames keep arriving; the child just never stops.
      for (let i = 0; i < 5; i++) {
        stream.push([makeStatus({ spec_id: 'spec_1', state: 'draining' })]);
        await act(async () => {
          vi.advanceTimersByTime(1000);
          await Promise.resolve();
        });
      }
      expect(putSpecs).toHaveLength(1);
      expect(screen.queryByText(t.runtimeRestartTimeout)).not.toBeInTheDocument();

      await act(async () => {
        vi.advanceTimersByTime(RESTART_STOP_TIMEOUT_MS);
        await Promise.resolve();
      });

      expect(screen.getByText(t.runtimeRestartTimeout)).toBeInTheDocument();
      // The override was deliberately NOT cleared behind the operator's back.
      expect(putSpecs).toHaveLength(1);
      expect(screen.queryByText(t.runtimeRestartStopping)).not.toBeInTheDocument();
    } finally {
      vi.useRealTimers();
    }
  });

  it('abandons the sequence on unmount without a further write', async () => {
    const { putSpecs, stream, unmount } = setupRestart();
    await openStatusTab();
    fireEvent.click(await screen.findByRole('button', { name: t.runtimeRestart }));
    await waitFor(() => expect(putSpecs).toHaveLength(1));
    await screen.findByText(t.runtimeRestartStopping);

    unmount();
    expect(stream.unsubscribes()).toBe(1);
    // The `stopped` frame arrives after the component is gone: no clear PUT,
    // and no state update on an unmounted tree.
    stream.push([makeStatus({ spec_id: 'spec_1', state: 'stopped' })]);
    await act(async () => {
      await Promise.resolve();
    });
    expect(putSpecs).toHaveLength(1);
  });

  it('abandons the sequence when the server changes mid-flight', async () => {
    const { putSpecs, stream, rerenderWithServer } = setupRestart();
    await openStatusTab();
    fireEvent.click(await screen.findByRole('button', { name: t.runtimeRestart }));
    await waitFor(() => expect(putSpecs).toHaveLength(1));
    await screen.findByText(t.runtimeRestartStopping);

    rerenderWithServer('srv_2');
    await waitFor(() =>
      expect(screen.queryByText(t.runtimeRestartStopping)).not.toBeInTheDocument(),
    );
    expect(stream.subscribedServerIds).toEqual(['srv_1', 'srv_2']);

    stream.push([makeStatus({ spec_id: 'spec_1', state: 'stopped' })]);
    await act(async () => {
      await Promise.resolve();
    });
    // The remaining step only means anything while we watch THAT server.
    expect(putSpecs).toHaveLength(1);
  });

  it('aborts (without clearing the override) when the row disappears for good', async () => {
    vi.useFakeTimers();
    try {
      const { putSpecs, stream } = setupRestart();
      await act(async () => {
        await Promise.resolve();
      });
      fireEvent.click(screen.getByRole('tab', { name: t.runtimeLiveStatus }));
      fireEvent.click(screen.getByRole('button', { name: t.runtimeRestart }));
      await act(async () => {
        await Promise.resolve();
      });
      expect(putSpecs).toHaveLength(1);

      // Row gone. Not yet terminal: the gateway keeps status in volatile RAM,
      // so a restart there empties the list for well under a second.
      stream.push([]);
      expect(screen.queryByText(t.runtimeRestartVanished)).not.toBeInTheDocument();

      await act(async () => {
        vi.advanceTimersByTime(RESTART_VANISH_GRACE_MS + 1000);
        await Promise.resolve();
      });
      stream.push([]);

      expect(screen.getByText(t.runtimeRestartVanished)).toBeInTheDocument();
      // PUTting a spec whose mapping may be gone would either 404 or
      // resurrect a spec the operator just deleted.
      expect(putSpecs).toHaveLength(1);
    } finally {
      vi.useRealTimers();
    }
  });

  it('does not abort on a transient empty frame: the row coming back resumes the sequence', async () => {
    const { spec, putSpecs, stream } = setupRestart();
    await openStatusTab();
    fireEvent.click(await screen.findByRole('button', { name: t.runtimeRestart }));
    await waitFor(() => expect(putSpecs).toHaveLength(1));

    // A gateway restart: one empty frame, then the next sample refills it.
    stream.push([]);
    stream.push([makeStatus({ spec_id: 'spec_1', state: 'draining' })]);
    expect(screen.queryByText(t.runtimeRestartVanished)).not.toBeInTheDocument();
    expect(await screen.findByText(t.runtimeRestartStopping)).toBeInTheDocument();

    stream.push([makeStatus({ spec_id: 'spec_1', state: 'stopped' })]);
    await waitFor(() => expect(putSpecs).toHaveLength(2));
    expect(putSpecs[1].body).toEqual(expectedBody(spec, ''));
  });

  it('refuses a second restart while one is in flight', async () => {
    const { putSpecs, stream } = setupRestart();
    await openStatusTab();
    const restartButton = await screen.findByRole('button', { name: t.runtimeRestart });
    fireEvent.click(restartButton);
    await waitFor(() => expect(putSpecs).toHaveLength(1));

    const busyButton = screen.getByRole('button', { name: t.runtimeRestart });
    expect(busyButton).toBeDisabled();
    fireEvent.click(busyButton);
    // Every other override write is locked too: any admin_state change
    // during the sequence would fight it.
    expect(screen.getByRole('button', { name: t.runtimeForceStart })).toBeDisabled();
    expect(putSpecs).toHaveLength(1);

    stream.push([makeStatus({ spec_id: 'spec_1', state: 'stopped' })]);
    await waitFor(() => expect(putSpecs).toHaveLength(2));
  });
});

describe('RuntimeAdminSection file mode (spec §10.2)', () => {
  const fileConfig = {
    router_listen: 9000,
    max_processes: 3,
    gpu_budgets: [{ index: 0, budget_mb: 46000 }],
    specs: [
      {
        id: 'rs1',
        model: 'File-Alpha',
        upstream_model: 'file-alpha',
        binary: '/opt/llama/llama-server',
        args: ['--model', '/models/alpha.gguf'],
        env: { HF_TOKEN: '***' },
        gpus: [{ index: 0, vram_mb: 20000 }],
        listen_port: 0,
        idle_timeout_seconds: 900,
        pinned: true,
        admin_state: '',
      },
      {
        id: 'rs2',
        model: 'File-Bravo',
        upstream_model: 'file-bravo',
        binary: '/opt/vllm/vllm',
        args: [],
        env: {},
        gpus: [{ index: 0, vram_mb: 21000 }],
        listen_port: 0,
        idle_timeout_seconds: 0,
        pinned: false,
        admin_state: '',
      },
    ],
    coresident: [['rs1', 'rs2']],
    etag: 'abc123',
  };

  function renderFileMode(config: unknown = fileConfig, extra: Partial<RuntimeReportContent> = {}) {
    return renderSection({
      mappings: [makeMapping({ id: 'map_1', gateway_model_name: 'Gateway-Alpha' })],
      specsByMappingId: {
        map_1: makeSpec({ configured: true, id: 'spec_1', mapping_id: 'map_1' }),
      },
      statusRows: [makeStatus({ spec_id: 'rs1', model: 'file-alpha' })],
      report: fileModeReport(config, extra),
    });
  }

  it('announces file mode and marks the gateway-side specs as ineffective', async () => {
    renderFileMode();
    expect(await screen.findByText(t.runtimeManagedLocally)).toBeInTheDocument();
    expect(screen.getByText(t.runtimeIneffectiveSpecs)).toBeInTheDocument();
  });

  it('turns every edit affordance off: no spec form, a disabled matrix, no budget form, no overrides', async () => {
    const { stream } = renderFileMode();
    await screen.findByText(t.runtimeManagedLocally);

    // Specs: no create, no row actions.
    expect(screen.queryByRole('button', { name: t.runtimeSpecCreate })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: t.runtimeSpecEditAction })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: t.runtimeSpecDelete })).not.toBeInTheDocument();

    // Matrix: rendered (from the report) but every cell disabled -- this is
    // exactly the `disabled` prop Task 21 built and deliberately left unwired.
    fireEvent.click(screen.getByRole('tab', { name: t.runtimeMatrix }));
    const cell = await screen.findByRole('button', {
      name: `${t.runtimeMatrixCell}: File-Bravo + File-Alpha`,
    });
    expect(cell).toBeDisabled();

    // Limits: read-only, no Save, no fields.
    fireEvent.click(screen.getByRole('tab', { name: t.runtimeLimits }));
    await screen.findByText(t.runtimeMaxProcesses, { exact: false });
    expect(screen.queryByRole('button', { name: t.save })).not.toBeInTheDocument();
    expect(screen.queryByLabelText(t.runtimeMaxProcesses)).not.toBeInTheDocument();
    expect(screen.queryByLabelText(t.runtimeGpuBudget)).not.toBeInTheDocument();

    // Status: live rows still work (they ride the sample), overrides do not
    // (the admin override lives in the gateway document file mode ignores).
    stream.setStatus('open');
    fireEvent.click(screen.getByRole('tab', { name: t.runtimeLiveStatus }));
    expect(await screen.findByText('file-alpha')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: t.runtimeForceStop })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: t.runtimeRestart })).not.toBeInTheDocument();
  });

  it('renders specs and the matrix from the report config, not from the gateway CRUD data', async () => {
    renderFileMode();
    await screen.findByText(t.runtimeManagedLocally);

    expect(await screen.findByText('File-Alpha')).toBeInTheDocument();
    expect(screen.getByText('File-Bravo')).toBeInTheDocument();
    // The gateway-side mapping is NOT what file mode shows.
    expect(screen.queryByText('Gateway-Alpha')).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole('tab', { name: t.runtimeMatrix }));
    const cell = await screen.findByRole('button', {
      name: `${t.runtimeMatrixCell}: File-Bravo + File-Alpha`,
    });
    // The reported coresident pair renders as set.
    expect(cell).toHaveAttribute('aria-pressed', 'true');

    fireEvent.click(screen.getByRole('tab', { name: t.runtimeLimits }));
    expect(await screen.findByText('3')).toBeInTheDocument();
    expect(screen.getByText('46000', { exact: false })).toBeInTheDocument();
  });

  // Correction 5: parse_error is the agent saying it could not read its own
  // config file. In that state `config` is unusable, so it must not be shown.
  it('surfaces parse_error prominently and stops rendering config', async () => {
    renderFileMode(fileConfig, { parse_error: 'yaml' });
    expect(await screen.findByText(t.runtimeParseError, { exact: false })).toBeInTheDocument();
    expect(screen.getByText('yaml', { exact: false })).toBeInTheDocument();
    expect(screen.queryByText('File-Alpha')).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole('tab', { name: t.runtimeMatrix }));
    await waitFor(() =>
      expect(
        screen.queryByRole('button', { name: `${t.runtimeMatrixCell}: File-Bravo + File-Alpha` }),
      ).not.toBeInTheDocument(),
    );
  });

  // Correction 6: `config` is typed `unknown` on purpose -- it is whatever
  // the agent's file contained. A malformed agent-supplied config must never
  // blank or crash the admin screen.
  it('survives a structurally malformed config: renders what parses, flags what does not', async () => {
    renderFileMode({
      max_processes: 'three',
      gpu_budgets: 'nope',
      specs: [
        {
          id: 'rs1',
          model: 'File-Alpha',
          binary: '/opt/llama/llama-server',
          gpus: [{ index: 0, vram_mb: 20000 }],
        },
        'garbage',
        { id: 42 },
      ],
      coresident: [['rs1', 'rs2'], 'not-a-pair'],
    });

    expect(await screen.findByText(t.runtimeManagedLocally)).toBeInTheDocument();
    expect(await screen.findByText('File-Alpha')).toBeInTheDocument();
    expect(screen.getByText(t.runtimeConfigUnrecognised)).toBeInTheDocument();
  });

  it('survives a config that is not an object at all', async () => {
    renderFileMode('not-an-object');
    expect(await screen.findByText(t.runtimeManagedLocally)).toBeInTheDocument();
    expect(screen.getByText(t.runtimeConfigUnrecognised)).toBeInTheDocument();
  });

  it('presents no writable UI while the report GET is still pending', async () => {
    const { fakeApi } = renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      specsByMappingId: { map_1: fullSpec() },
      reportPending: true,
      gpuBudgets: [{ index: 0, budget_mb: 40000, expected_uuid: '', expected_name: '' }],
      coresidencyPairs: [],
    });

    await screen.findByText('gw-model');
    expect(screen.queryByRole('button', { name: t.runtimeSpecCreate })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: t.runtimeSpecEditAction })).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole('tab', { name: t.runtimeLimits }));
    await screen.findByText(t.loading);
    expect(screen.queryByRole('button', { name: t.save })).not.toBeInTheDocument();
    expect(fakeApi.putGpuBudgets).not.toHaveBeenCalled();
    expect(fakeApi.putRuntimeSpec).not.toHaveBeenCalled();
  });

  it('stays writable for a gateway-source report', async () => {
    renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      specsByMappingId: { map_1: fullSpec() },
      report: makeReport({
        available: true,
        agent_version: '0.2.0',
        agent_features: ['runtime_manager'],
        report: { source: 'gateway', collected_at: '2026-07-16T12:00:00Z', config: {} },
      }),
    });

    expect(await screen.findByRole('button', { name: t.runtimeSpecCreate })).toBeInTheDocument();
    expect(screen.queryByText(t.runtimeManagedLocally)).not.toBeInTheDocument();
  });
});

describe('RuntimeAdminSection feature-mismatch banner (spec §9)', () => {
  function renderWithFeatures(features: string[], statusRows: RuntimeStatus[] = []) {
    return renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      specsByMappingId: {
        map_1: makeSpec({ configured: true, id: 'spec_1', mapping_id: 'map_1' }),
      },
      statusRows,
      report: makeReport({ agent_version: '0.1.4', agent_features: features }),
    });
  }

  it('explains the silence when specs exist, nothing is reported, and the agent lacks runtime_manager', async () => {
    renderWithFeatures(['proxy_status']);
    expect(await screen.findByText(t.runtimeFeatureMismatch)).toBeInTheDocument();
    // Naming the reported version is the point: it says WHAT to upgrade.
    expect(screen.getByText('0.1.4', { exact: false })).toBeInTheDocument();
  });

  it('stays quiet when the agent does declare runtime_manager', async () => {
    renderWithFeatures(['runtime_manager']);
    await screen.findByText('gw-model');
    expect(screen.queryByText(t.runtimeFeatureMismatch)).not.toBeInTheDocument();
  });

  it('stays quiet when the stream is reporting processes anyway', async () => {
    const { stream } = renderWithFeatures([], [makeStatus({ spec_id: 'spec_1' })]);
    stream.setStatus('open');
    await screen.findByText('gw-model');
    expect(screen.queryByText(t.runtimeFeatureMismatch)).not.toBeInTheDocument();
  });

  it('stays quiet when no spec is configured at all', async () => {
    renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      specsByMappingId: { map_1: makeSpec({ configured: false, mapping_id: 'map_1' }) },
      report: makeReport({ agent_version: '0.1.4', agent_features: [] }),
    });
    await screen.findByText('gw-model');
    expect(screen.queryByText(t.runtimeFeatureMismatch)).not.toBeInTheDocument();
  });
});

// ---------------------------------------------------------------------------
// Task 22 fix round 1. Three production-reachable paths that had no test at
// all -- and they are exactly the ones that were broken -- plus the bounded
// writes, the stronger unmount observation, and the gateway/upstream name
// pair.
// ---------------------------------------------------------------------------

describe('RuntimeAdminSection restart-state gate (fix round 1, I1)', () => {
  // A restart is force_stopped -> await `stopped` -> clear. On the agent side
  // that middle step can only ever arrive for a spec that has something to
  // stop or a resting state applyConfig resets:
  //   - force_stopped with no live process is a no-op
  //     (server-agent/internal/runtime/manager.go:724-728), and
  //   - applyConfig's changed-spec reset deliberately EXCLUDES
  //     StateNotPermitted and StatePendingVRAMUnknown (:676-698, the "I6"
  //     comment) while it does reset start_failed/crashed/backoff.
  // So on `stopped`, `pending_vram_unknown` and `not_permitted` the wait can
  // never be satisfied: the UI would spin for the full bound, report a
  // timeout, and leave force_stopped in place -- which makes the model
  // admission-blocked (manager.go ErrAdmissionBlocked) until a human clears
  // it by hand. A UI action would have made a model unavailable and reported
  // it as a timeout.
  function fourStates() {
    const rows: { specId: string; mappingId: string; gw: string; model: string; state: string }[] =
      [
        {
          specId: 'spec_run',
          mappingId: 'map_run',
          gw: 'Gw-Run',
          model: 'up-run',
          state: 'running',
        },
        {
          specId: 'spec_stop',
          mappingId: 'map_stop',
          gw: 'Gw-Stop',
          model: 'up-stop',
          state: 'stopped',
        },
        {
          specId: 'spec_vram',
          mappingId: 'map_vram',
          gw: 'Gw-Vram',
          model: 'up-vram',
          state: 'pending_vram_unknown',
        },
        {
          specId: 'spec_perm',
          mappingId: 'map_perm',
          gw: 'Gw-Perm',
          model: 'up-perm',
          state: 'not_permitted',
        },
      ];
    return renderSection({
      mappings: rows.map((r) => makeMapping({ id: r.mappingId, gateway_model_name: r.gw })),
      specsByMappingId: Object.fromEntries(
        rows.map((r) => [r.mappingId, fullSpec({ id: r.specId, mapping_id: r.mappingId })]),
      ),
      statusRows: rows.map((r) =>
        makeStatus({ spec_id: r.specId, model: r.model, state: r.state }),
      ),
    });
  }

  it('offers Restart only where a `stopped` frame can actually follow the force_stopped write', async () => {
    const { stream } = fourStates();
    stream.setStatus('open');
    await openStatusTab();
    // Wait for the per-mapping spec GETs to settle: only a resolved row has
    // any actions at all, so this is the barrier that matters here.
    expect(await screen.findAllByRole('button', { name: t.runtimeRestart })).toHaveLength(4);

    // There IS a process to stop here.
    expect(inRowWith('up-run').getByRole('button', { name: t.runtimeRestart })).toBeEnabled();

    // These three cannot report the transition the sequence waits for. The
    // action stays VISIBLE (hiding it would make an operator wonder whether
    // the portal forgot it, and the row's own state is the explanation) but
    // disabled, so the sequence can never be started from here.
    for (const model of ['up-stop', 'up-vram', 'up-perm']) {
      const row = inRowWith(model);
      expect(row.getByRole('button', { name: t.runtimeRestart })).toBeDisabled();
      // "Force start" is the action that does something on all three.
      expect(row.getByRole('button', { name: t.runtimeForceStart })).toBeEnabled();
    }
  });

  it('sends nothing when the disabled Restart on a pending_vram_unknown row is clicked', async () => {
    const { fakeApi, stream } = fourStates();
    stream.setStatus('open');
    await openStatusTab();
    expect(await screen.findAllByRole('button', { name: t.runtimeRestart })).toHaveLength(4);

    fireEvent.click(inRowWith('up-vram').getByRole('button', { name: t.runtimeRestart }));
    await act(async () => {
      await Promise.resolve();
    });
    expect(fakeApi.putRuntimeSpec).not.toHaveBeenCalled();
    expect(screen.queryByText(t.runtimeRestartStopping)).not.toBeInTheDocument();
  });
});

describe('RuntimeAdminSection restart stream watermark (fix round 1, I2)', () => {
  // The sequence confirmed a STATE, never a TRANSITION: the moment the phase
  // flipped to `waiting` it tested whatever the last frame happened to say --
  // possibly a frame received before its own force_stopped PUT even landed.
  // Then it "completed", cleared the override, and reported success while
  // nothing had been restarted.
  it('does not complete off a `stopped` frame that predates its own force_stopped write', async () => {
    const spec = fullSpec();
    let landWrite: (updated: RuntimeSpec) => void = () => {};
    const write = new Promise<RuntimeSpec>((resolve) => {
      landWrite = resolve;
    });
    const { fakeApi, putSpecs, stream } = renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      specsByMappingId: { map_1: spec },
      statusRows: [makeStatus({ spec_id: 'spec_1', state: 'running' })],
    });
    stream.setStatus('open');
    await openStatusTab();
    // Only the FIRST put is held open; the clear PUT (if it ever comes) goes
    // through the recording default implementation.
    fakeApi.putRuntimeSpec.mockImplementationOnce(() => write);

    fireEvent.click(await screen.findByRole('button', { name: t.runtimeRestart }));
    await waitFor(() => expect(fakeApi.putRuntimeSpec).toHaveBeenCalledTimes(1));

    // A `stopped` frame arriving while the write is still in flight cannot be
    // a consequence of it -- the process is resting for some other reason
    // (a drain that was already running, an idle timeout).
    stream.push([makeStatus({ spec_id: 'spec_1', state: 'stopped' })]);

    await act(async () => {
      landWrite(fullSpec({ admin_state: 'force_stopped' }));
      // Two macrotask turns: enough for the awaited write's continuation, the
      // render it triggers, and the stream effect that runs after it. If the
      // sequence completes off the stale frame it has done so by now.
      await new Promise((resolve) => setTimeout(resolve, 0));
      await new Promise((resolve) => setTimeout(resolve, 0));
    });
    expect(fakeApi.putRuntimeSpec).toHaveBeenCalledTimes(1);
    expect(putSpecs).toHaveLength(0);
    // ...and the sequence is still alive, still waiting.
    expect(screen.getByText(t.runtimeRestartStopping)).toBeInTheDocument();

    // Only a frame from strictly AFTER the write completes the sequence.
    stream.push([makeStatus({ spec_id: 'spec_1', state: 'stopped' })]);
    await waitFor(() => expect(fakeApi.putRuntimeSpec).toHaveBeenCalledTimes(2));
    expect(putSpecs).toHaveLength(1);
    expect(putSpecs[0].body).toEqual(expectedBody(spec, ''));
  });
});

describe('RuntimeAdminSection failing runtime report (fix round 1, I3)', () => {
  // `reportReady = !reportLoading && reportData !== null` collapsed two
  // different facts: useResource on error sets `error` and leaves
  // `data === null`, so a failed GET looked exactly like a pending one --
  // forever. Every write affordance on the WHOLE screen disappeared and the
  // matrix/limits tabs claimed to be loading indefinitely, with the only
  // signal a toast that scrolls away.
  it('names the third state, keeps writes off, and offers a retry that recovers', async () => {
    const { fakeApi } = renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      specsByMappingId: { map_1: fullSpec() },
      reportFailing: true,
    });

    expect(await screen.findByText(t.runtimeModeUnknown)).toBeInTheDocument();
    // Writes stay disabled -- that half was always right; it just has to say why.
    expect(screen.queryByRole('button', { name: t.runtimeSpecCreate })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: t.runtimeSpecEditAction })).not.toBeInTheDocument();

    // And no tab pretends to still be loading.
    fireEvent.click(screen.getByRole('tab', { name: t.runtimeMatrix }));
    expect(await screen.findByText(t.runtimeModeUnknownShort)).toBeInTheDocument();
    expect(screen.queryByText(t.loading)).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole('tab', { name: t.runtimeLimits }));
    expect(await screen.findByText(t.runtimeModeUnknownShort)).toBeInTheDocument();
    expect(screen.queryByText(t.loading)).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: t.save })).not.toBeInTheDocument();

    // The retry re-runs the GET, and the screen comes back.
    fireEvent.click(screen.getByRole('button', { name: t.resourceRetry }));
    await waitFor(() => expect(fakeApi.runtimeReport).toHaveBeenCalledTimes(2));
    fireEvent.click(screen.getByRole('tab', { name: t.runtimeSpecs }));
    expect(await screen.findByRole('button', { name: t.runtimeSpecCreate })).toBeInTheDocument();
    expect(screen.queryByText(t.runtimeModeUnknown)).not.toBeInTheDocument();
  });

  it('offers no override actions on the status tab while the mode is unknown', async () => {
    const { fakeApi, stream } = renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      specsByMappingId: { map_1: fullSpec() },
      statusRows: [makeStatus({ spec_id: 'spec_1' })],
      reportFailing: true,
    });
    stream.setStatus('open');
    await openStatusTab();

    expect(await screen.findByText(t.runtimeModeUnknown)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: t.runtimeForceStop })).not.toBeInTheDocument();
    expect(fakeApi.putRuntimeSpec).not.toHaveBeenCalled();
  });

  // Fix round 2, the forward-looking half of I3: the OTHER failure shape.
  // `useResource` never clears `data` on a failed fetch, so a report GET that
  // fails AFTER one succeeded leaves the previous payload in hand -- and
  // `data !== null` used to be tested before the error, reporting that as
  // `ready`. On this screen it is reachable by switching servers (`server.id`
  // is a loader dep), and the stale payload is the WRONG server's runtime
  // mode, so it must not decide whether this screen is writable.
  it('treats a report GET that fails after a server switch as a failure, not as the old server’s mode', async () => {
    const { stream, rerenderWithServer } = renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      specsByMappingId: { map_1: fullSpec() },
      statusRows: [makeStatus({ spec_id: 'spec_1', state: 'running' })],
      reportFailsOnCall: 2,
    });
    stream.setStatus('open');
    await openStatusTab();
    // The first server resolved: writable, no banner.
    expect(await screen.findByRole('button', { name: t.runtimeForceStop })).toBeInTheDocument();
    expect(screen.queryByText(t.runtimeModeUnknown)).not.toBeInTheDocument();

    rerenderWithServer('srv_other');
    await act(async () => {
      await Promise.resolve();
    });
    stream.push([makeStatus({ spec_id: 'spec_1', state: 'running' })]);

    expect(await screen.findByText(t.runtimeModeUnknown)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: t.runtimeForceStop })).not.toBeInTheDocument();
    // And it must not read as "still loading" either.
    expect(screen.queryByText(t.loading)).not.toBeInTheDocument();
  });
});

describe('RuntimeAdminSection bounded override writes (fix round 1, M4)', () => {
  // api/transport.ts has no AbortController, so a PUT that never settles used
  // to leave `overrideBusy` / the restart's `clearing` phase set forever --
  // every action on every row disabled behind a "clearing…" chip, with no
  // escape but a page reload.
  it('releases the lock when an override PUT never settles', async () => {
    vi.useFakeTimers();
    try {
      const { fakeApi, stream } = renderSection({
        mappings: [makeMapping({ id: 'map_1' })],
        specsByMappingId: { map_1: fullSpec() },
        statusRows: [makeStatus({ spec_id: 'spec_1', state: 'running' })],
      });
      stream.setStatus('open');
      await act(async () => {
        await Promise.resolve();
      });
      fireEvent.click(screen.getByRole('tab', { name: t.runtimeLiveStatus }));
      fakeApi.putRuntimeSpec.mockImplementationOnce(() => new Promise<RuntimeSpec>(() => {}));
      fireEvent.click(screen.getByRole('button', { name: t.runtimeForceStop }));
      await act(async () => {
        await Promise.resolve();
      });
      expect(screen.getByRole('button', { name: t.runtimeForceStart })).toBeDisabled();

      await act(async () => {
        vi.advanceTimersByTime(OVERRIDE_WRITE_TIMEOUT_MS + 1000);
        await Promise.resolve();
      });
      expect(screen.getByText(t.runtimeWriteTimeout)).toBeInTheDocument();
      expect(screen.getByRole('button', { name: t.runtimeForceStart })).toBeEnabled();
    } finally {
      vi.useRealTimers();
    }
  });

  it('releases the lock when the restart sequence’s clear PUT never settles', async () => {
    vi.useFakeTimers();
    try {
      const { fakeApi, stream } = renderSection({
        mappings: [makeMapping({ id: 'map_1' })],
        specsByMappingId: { map_1: fullSpec() },
        statusRows: [makeStatus({ spec_id: 'spec_1', state: 'running' })],
      });
      stream.setStatus('open');
      await act(async () => {
        await Promise.resolve();
      });
      fireEvent.click(screen.getByRole('tab', { name: t.runtimeLiveStatus }));
      fireEvent.click(screen.getByRole('button', { name: t.runtimeRestart }));
      await act(async () => {
        await Promise.resolve();
      });
      // The clear PUT is the one that hangs.
      fakeApi.putRuntimeSpec.mockImplementationOnce(() => new Promise<RuntimeSpec>(() => {}));
      stream.push([makeStatus({ spec_id: 'spec_1', state: 'stopped' })]);
      await act(async () => {
        await Promise.resolve();
      });
      expect(screen.getByText(t.runtimeRestartClearing)).toBeInTheDocument();

      await act(async () => {
        vi.advanceTimersByTime(OVERRIDE_WRITE_TIMEOUT_MS + 1000);
        await Promise.resolve();
      });
      expect(screen.getByText(t.runtimeRestartClearTimeout)).toBeInTheDocument();
      expect(screen.queryByText(t.runtimeRestartClearing)).not.toBeInTheDocument();
      expect(screen.getByRole('button', { name: t.runtimeForceStart })).toBeEnabled();
    } finally {
      vi.useRealTimers();
    }
  });
});

describe('RuntimeAdminSection abandoned writes still refresh the cache (fix round 2, N1)', () => {
  // The run token (fix round 1, M4) correctly keeps the success toast and the
  // lock release behind it -- but it also gated `setSpecsById`, which is a
  // CACHE OF SERVER TRUTH. So a write abandoned by its watchdog that then
  // resolved left the row claiming the opposite of the facts: `admin_state:
  // ''`, i.e. Force stop and Restart offered and NO Clear override, while the
  // server actually held force_stopped -- with the timeout notice at the same
  // time telling the operator that an override is in effect and must be
  // cleared by hand. The row pointed away from the only action that fixes it.
  it('refreshes the row after an abandoned override write later succeeds', async () => {
    vi.useFakeTimers();
    try {
      let landWrite: (updated: RuntimeSpec) => void = () => {};
      const write = new Promise<RuntimeSpec>((resolve) => {
        landWrite = resolve;
      });
      const { fakeApi, stream } = renderSection({
        mappings: [makeMapping({ id: 'map_1' })],
        specsByMappingId: { map_1: fullSpec() },
        statusRows: [makeStatus({ spec_id: 'spec_1', state: 'running' })],
      });
      stream.setStatus('open');
      await act(async () => {
        await Promise.resolve();
      });
      fireEvent.click(screen.getByRole('tab', { name: t.runtimeLiveStatus }));
      fakeApi.putRuntimeSpec.mockImplementationOnce(() => write);
      fireEvent.click(screen.getByRole('button', { name: t.runtimeForceStop }));
      await act(async () => {
        await Promise.resolve();
      });

      // The watchdog gives the write up and says the outcome is unknown.
      await act(async () => {
        vi.advanceTimersByTime(OVERRIDE_WRITE_TIMEOUT_MS + 1000);
        await Promise.resolve();
      });
      expect(screen.getByText(t.runtimeWriteTimeout)).toBeInTheDocument();

      // ...and THEN the PUT lands successfully. The server now holds
      // force_stopped.
      await act(async () => {
        landWrite(fullSpec({ admin_state: 'force_stopped' }));
        await Promise.resolve();
      });

      // Clear override is the only action that undoes it, and the notice just
      // told the operator to use it -- so the row has to offer it.
      expect(screen.getByRole('button', { name: t.runtimeClearOverride })).toBeInTheDocument();
      // And must no longer offer the write that already happened.
      expect(screen.queryByRole('button', { name: t.runtimeForceStop })).not.toBeInTheDocument();
      // The user-visible halves stay abandoned: no success toast for a flow
      // the operator has already been told is over.
      expect(screen.queryByText(t.systemSaved)).not.toBeInTheDocument();
    } finally {
      vi.useRealTimers();
    }
  });
});

describe('RuntimeAdminSection restart state gate is re-asserted on click (fix round 2, N2)', () => {
  // The gate lived ONLY in the `disabled` prop, and the onClick closure
  // carries the row from the render that set it. Frames arrive ~1/s, so a
  // state change landing between that render's commit and the click's
  // dispatch let a `stopped` row start the sequence -- a force_stopped
  // override no `stopped` frame can ever clear, i.e. exactly the
  // admission-blocked model the gate exists to prevent.
  it('refuses a restart whose row went non-restartable between the render and the click', async () => {
    const { fakeApi, stream } = renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      specsByMappingId: { map_1: fullSpec() },
      statusRows: [makeStatus({ spec_id: 'spec_1', state: 'running' })],
    });
    stream.setStatus('open');
    await openStatusTab();
    const restartButton = await screen.findByRole('button', { name: t.runtimeRestart });
    expect(restartButton).toBeEnabled();

    // The race: the frame is in, the DOM is not yet.
    stream.pushUnflushed([makeStatus({ spec_id: 'spec_1', state: 'stopped' })]);
    restartButton.dispatchEvent(new MouseEvent('click', { bubbles: true }));
    await act(async () => {
      await Promise.resolve();
    });

    // Nothing was written and no sequence is running.
    expect(fakeApi.putRuntimeSpec).not.toHaveBeenCalled();
    expect(screen.queryByText(t.runtimeRestartStopping)).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: t.runtimeRestart })).toBeDisabled();
  });

  // A greyed-out Restart with no explanation is the RESTING state of a healthy
  // row: a successful restart ends `stopped`, where Restart is disabled. So
  // every operator meets it, and it has to say why.
  it('explains on hover why Restart is disabled on a stopped row', async () => {
    const { stream } = renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      specsByMappingId: { map_1: fullSpec() },
      statusRows: [makeStatus({ spec_id: 'spec_1', state: 'stopped' })],
    });
    stream.setStatus('open');
    await openStatusTab();
    const restartButton = await screen.findByRole('button', { name: t.runtimeRestart });
    expect(restartButton).toBeDisabled();
    // The wrapper span, not the button: a disabled MUI button is pointer-inert
    // and a Tooltip anchored on it never fires.
    fireEvent.mouseOver(restartButton.parentElement as HTMLElement);
    expect(await screen.findByRole('tooltip')).toHaveTextContent(t.runtimeRestartUnavailable);
  });
});

describe('RuntimeAdminSection unmount guard (fix round 1)', () => {
  // The existing unmount test tears the ToastProvider down along with the
  // section, so "no success toast" would pass even with the guard removed.
  // Hoisting the provider ABOVE the unmounted subtree observes the component's
  // own mounted-guard directly.
  it('pushes no success toast when the override PUT resolves after the section alone unmounts', async () => {
    let landWrite: (updated: RuntimeSpec) => void = () => {};
    const write = new Promise<RuntimeSpec>((resolve) => {
      landWrite = resolve;
    });
    const { fakeApi, stream, detachSection } = renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      specsByMappingId: { map_1: fullSpec() },
      statusRows: [makeStatus({ spec_id: 'spec_1', state: 'running' })],
    });
    stream.setStatus('open');
    await openStatusTab();
    fakeApi.putRuntimeSpec.mockImplementationOnce(() => write);
    fireEvent.click(await screen.findByRole('button', { name: t.runtimeForceStop }));
    await waitFor(() => expect(fakeApi.putRuntimeSpec).toHaveBeenCalledTimes(1));

    detachSection();
    await act(async () => {
      landWrite(fullSpec({ admin_state: 'force_stopped' }));
      await Promise.resolve();
    });
    // The ToastProvider is still mounted and would happily render this.
    expect(screen.queryByText(t.systemSaved)).not.toBeInTheDocument();
  });
});

describe('RuntimeAdminSection status row identity (fix round 1, accepted design change)', () => {
  // The gateway model name is the only name that appears anywhere else in the
  // portal -- and area 1 of this same screen joins to this table by spec_id
  // while labelling its rows with it. So the gateway name is the primary
  // label here too; the agent-reported upstream name stays visible beneath it
  // rather than being replaced, because a disagreement between the two is a
  // genuine fact worth seeing.
  it('shows the gateway model name first, with the agent-reported name beneath it', async () => {
    const { stream } = renderSection({
      mappings: [
        makeMapping({
          id: 'map_1',
          gateway_model_name: 'Gateway-Alpha',
          app_model_name: 'up-alpha',
        }),
      ],
      specsByMappingId: { map_1: fullSpec() },
      statusRows: [makeStatus({ spec_id: 'spec_1', model: 'up-alpha' })],
    });
    stream.setStatus('open');
    await openStatusTab();

    expect(await screen.findByText('Gateway-Alpha')).toBeInTheDocument();
    expect(screen.getByText('up-alpha', { exact: false })).toBeInTheDocument();
    // Same name on both sides -> nothing to warn about.
    expect(screen.queryByTitle(t.runtimeStatusNameMismatch)).not.toBeInTheDocument();
  });

  it('marks a row where the agent reports a different model name than the mapping', async () => {
    const { stream } = renderSection({
      mappings: [
        makeMapping({
          id: 'map_1',
          gateway_model_name: 'Gateway-Alpha',
          app_model_name: 'up-alpha',
        }),
      ],
      specsByMappingId: { map_1: fullSpec() },
      statusRows: [makeStatus({ spec_id: 'spec_1', model: 'something-else' })],
    });
    stream.setStatus('open');
    await openStatusTab();

    expect(await screen.findByText('Gateway-Alpha')).toBeInTheDocument();
    expect(screen.getByTitle(t.runtimeStatusNameMismatch)).toBeInTheDocument();
  });

  // The status stream is SERVER-scoped while the spec/mapping join is
  // APPLICATION-scoped, and nothing in the backend stops a server from
  // carrying a second `server_agent` application (only `unique(server_id,
  // port)` exists). On such a server most rows hit this fallback, and an
  // empty actions cell with no explanation would make the override feature
  // look broken.
  it('says so when a row cannot be resolved to a mapping of this application', async () => {
    const { stream } = renderSection({
      mappings: [makeMapping({ id: 'map_1', gateway_model_name: 'Gateway-Alpha' })],
      specsByMappingId: { map_1: fullSpec() },
      statusRows: [makeStatus({ spec_id: 'spec_from_another_app', model: 'orphan' })],
    });
    stream.setStatus('open');
    await openStatusTab();

    expect(await screen.findByText(t.runtimeStatusUnresolvedShort)).toBeInTheDocument();
    expect(screen.getByText('orphan')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: t.runtimeForceStop })).not.toBeInTheDocument();
  });
});

describe('RuntimeAdminSection feature-mismatch vs. never-reported (fix round 1, M8)', () => {
  // The backend yields `[]` features when there is no telemetry row at all,
  // so a server whose agent has never connected was told to UPDATE its agent,
  // with "Agent version: —". `server.agent_status` separates the two facts and
  // was already in props.
  it('tells a server whose agent never reported to install/connect it, not to update it', async () => {
    renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      specsByMappingId: {
        map_1: makeSpec({ configured: true, id: 'spec_1', mapping_id: 'map_1' }),
      },
      report: makeReport({ agent_version: '', agent_features: [] }),
    });

    expect(await screen.findByText(t.runtimeAgentNeverReported)).toBeInTheDocument();
    expect(screen.queryByText(t.runtimeFeatureMismatch)).not.toBeInTheDocument();
  });
});
