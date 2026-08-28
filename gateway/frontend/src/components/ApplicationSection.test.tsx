// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ApplicationSection } from './ApplicationSection';
import { ToastProvider } from './shared/ToastProvider';
import { formatDate } from './shared/format';
import { messages } from '../i18n';
import type {
  BenchmarkStatus,
  CreateApplicationRequest,
  CreateMappingRequest,
  PortalApplication,
  PortalModelMapping,
  PortalServer,
  RuntimeSpec,
  UpdateApplicationRequest,
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
  health_status: 'unknown',
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

function makeApp(overrides: Partial<PortalApplication> = {}): PortalApplication {
  return {
    id: 'app_1',
    server_id: 'srv_1',
    type: 'vllm',
    port: 8000,
    scheme: 'https',
    endpoint: 'https://s1.example.test:8000',
    api_flavors: ['openai', 'anthropic'],
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
    last_checked_at: '2026-07-16T12:00:00Z',
    created_at: '2026-07-16T12:00:00Z',
    ...overrides,
  };
}

// Never invoked by these tests (the mappings drill-down is out of scope here),
// but ApplicationSection forwards `api` to MappingSection, so the mock must
// structurally satisfy MappingSection's Pick too.
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

// A benign "not configured" runtime spec (RuntimeSpec's own zero-value
// convention), used only by the server_agent drill-down tests below.
function makeRuntimeSpec(overrides: Partial<RuntimeSpec> = {}): RuntimeSpec {
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
    set_visible_devices: false,
    gpus: [],
    ...overrides,
  };
}

const idleBenchmark: BenchmarkStatus = {
  running: false,
  server_id: 'srv_1',
  scope: 'application',
  total: 0,
  done: 0,
};

function renderSection(
  opts: {
    apps?: PortalApplication[];
    healthInterval?: number | 'error';
    server?: PortalServer;
  } = {},
) {
  const apps = opts.apps ?? [];
  const activeServer = opts.server ?? server;
  const created: CreateApplicationRequest[] = [];
  const updated: { id: string; body: UpdateApplicationRequest }[] = [];
  const fakeApi = {
    applications: vi.fn(async () => ({ data: apps })),
    createApplication: vi.fn(async (_serverId: string, body: CreateApplicationRequest) => {
      created.push(body);
      return makeApp({ id: 'app_created', ...(body as Partial<PortalApplication>) });
    }),
    updateApplication: vi.fn(async (id: string, body: UpdateApplicationRequest) => {
      updated.push({ id, body });
      return makeApp({ id, ...(body as Partial<PortalApplication>) });
    }),
    deleteApplication: vi.fn(async () => ({ ok: true })),
    healthCheckInterval: vi.fn(async () => {
      if (opts.healthInterval === 'error') throw new Error('nope');
      return { health_check_interval_seconds: opts.healthInterval ?? 30 };
    }),
    // Mappings drill-down (unreached by these tests; see makeMapping above).
    mappings: vi.fn(async () => ({ data: [] })),
    createMapping: vi.fn(async (_aid: string, body: CreateMappingRequest) =>
      makeMapping({ id: 'map_created', ...(body as Partial<PortalModelMapping>) }),
    ),
    updateMapping: vi.fn(async (id: string, body: UpdateMappingRequest) =>
      makeMapping({ id, ...(body as Partial<PortalModelMapping>) }),
    ),
    deleteMapping: vi.fn(async () => ({ ok: true })),
    syncApplicationModels: vi.fn(async () => ({
      added: 0,
      disabled: 0,
      unchanged: 0,
      conflicted: 0,
    })),
    benchmarkServer: vi.fn(async () => idleBenchmark),
    benchmarkApplication: vi.fn(async () => idleBenchmark),
    benchmarkMapping: vi.fn(async () => idleBenchmark),
    benchmarkStatus: vi.fn(async () => idleBenchmark),
    subscribeBenchmark: vi.fn(() => () => {}),
    mappingBenchmarks: vi.fn(async () => []),
    activeBenchmarks: vi.fn(async () => []),
    probeMappingContext: vi.fn(async () => idleBenchmark),
    // RuntimeAdminSection's Pick (reached only by the server_agent drill-down
    // tests below; unused defaults for every other test).
    runtimeSpec: vi.fn(async () => makeRuntimeSpec()),
    putRuntimeSpec: vi.fn(async () => makeRuntimeSpec()),
    deleteRuntimeSpec: vi.fn(async () => ({ ok: true })),
    runtimeCoresidency: vi.fn(async () => ({ pairs: [] })),
    putRuntimeCoresidency: vi.fn(async () => ({ pairs: [] })),
    runtimeWarnings: vi.fn(async () => ({ warnings: [] })),
    gpuBudgets: vi.fn(async () => ({ budgets: [] })),
    putGpuBudgets: vi.fn(async () => ({ budgets: [] })),
    runtimeReport: vi.fn(async () => ({ available: false, agent_version: '', agent_features: [] })),
    subscribeRuntimeStatus: vi.fn(() => () => {}),
    subscribeRuntimeLogs: vi.fn(() => () => {}),
    // Task 21 (matrix + server limits): unused defaults for every test here
    // (reached only by RuntimeAdminSection's own tests), mirroring the
    // runtime* defaults above.
    updateServer: vi.fn(async (id: string) => ({ ...activeServer, id })),
    serverHardware: vi.fn(async () => ({ available: false })),
  };

  render(
    <ToastProvider>
      <ApplicationSection t={t} api={fakeApi} server={activeServer} />
    </ToastProvider>,
  );
  return { fakeApi, created, updated };
}

// The create/edit form is a sub-view mask reached from the list. Open it by
// clicking the "create" action; the mask's submit button reuses the same label.
function openCreate() {
  fireEvent.click(screen.getByRole('button', { name: t.applicationCreate }));
}

// The health-mode field is a non-native MUI Select (combobox), not a <select>.
// Open it and click the option; options render in a portal on document.body.
async function selectHealthMode(optionText: string) {
  fireEvent.mouseDown(screen.getByRole('combobox', { name: t.applicationHealthMode }));
  fireEvent.click(await screen.findByRole('option', { name: optionText }));
}

// The application-type field is the same non-native MUI Select pattern.
async function selectType(optionText: string) {
  fireEvent.mouseDown(screen.getByRole('combobox', { name: t.applicationType }));
  fireEvent.click(await screen.findByRole('option', { name: optionText }));
}

afterEach(cleanup);

describe('ApplicationSection health config fields', () => {
  it('defaults to health_path mode with the default path and sends both on create', async () => {
    const { created } = renderSection();
    openCreate();
    const modeField = screen.getByRole('combobox', { name: t.applicationHealthMode });
    // The selected value shows as the combobox's rendered option text.
    expect(modeField).toHaveTextContent(t.applicationHealthModePath);
    const pathField = screen.getByLabelText(t.applicationHealthPath) as HTMLInputElement;
    expect(pathField.value).toBe('/v1/health');

    // In the mask, the submit button carries the same "create" label.
    fireEvent.click(screen.getByRole('button', { name: t.applicationCreate }));
    await waitFor(() => expect(created).toHaveLength(1));
    expect(created[0].health_check_mode).toBe('health_path');
    expect(created[0].health_check_path).toBe('/v1/health');
  });

  it('selecting model_sync hides the health path field and sends model_sync on create', async () => {
    const { created } = renderSection();
    openCreate();
    await selectHealthMode(t.applicationHealthModeModelSync);
    // The health-path field is only relevant in health_path mode.
    expect(screen.queryByLabelText(t.applicationHealthPath)).toBeNull();
    expect(screen.getByText(t.applicationHealthModeNote)).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: t.applicationCreate }));
    await waitFor(() => expect(created).toHaveLength(1));
    expect(created[0].health_check_mode).toBe('model_sync');
  });

  it('selecting always_reachable hides the path field and sends always_reachable on create', async () => {
    const { created } = renderSection();
    openCreate();
    await selectHealthMode(t.applicationAlwaysReachable);
    expect(screen.queryByLabelText(t.applicationHealthPath)).toBeNull();

    fireEvent.click(screen.getByRole('button', { name: t.applicationCreate }));
    await waitFor(() => expect(created).toHaveLength(1));
    expect(created[0].health_check_mode).toBe('always_reachable');
  });

  it('populates the health mode on edit and saves a switch to model_sync', async () => {
    const { updated } = renderSection({
      apps: [
        makeApp({ id: 'app_1', health_check_mode: 'health_path', health_check_path: '/ready' }),
      ],
    });
    await screen.findByText('https://s1.example.test:8000');

    // Open the edit sub-view from the row's inline edit action.
    fireEvent.click(screen.getByRole('button', { name: t.applicationEdit }));

    const editPath = screen.getByLabelText(t.applicationHealthPath) as HTMLInputElement;
    expect(editPath.value).toBe('/ready');
    const editMode = screen.getByRole('combobox', { name: t.applicationHealthMode });
    expect(editMode).toHaveTextContent(t.applicationHealthModePath);

    await selectHealthMode(t.applicationHealthModeModelSync);
    // Path field disappears in model_sync mode.
    expect(screen.queryByLabelText(t.applicationHealthPath)).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: t.applicationSave }));

    await waitFor(() => expect(updated).toHaveLength(1));
    expect(updated[0].id).toBe('app_1');
    expect(updated[0].body.health_check_mode).toBe('model_sync');
  });

  it('defaults the health-check interval to Default and sends 0 on create', async () => {
    const { created } = renderSection();
    openCreate();
    expect(
      screen.getByRole('combobox', { name: t.applicationHealthIntervalLabel }),
    ).toHaveTextContent(t.applicationHealthIntervalDefault);
    // No seconds input while on Default.
    expect(
      screen.queryByLabelText(t.applicationHealthIntervalSecondsLabel),
    ).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: t.applicationCreate }));
    await waitFor(() => expect(created).toHaveLength(1));
    expect(created[0].health_check_interval_seconds).toBe(0);
  });

  it('Custom interval sends the entered seconds on create', async () => {
    const { created } = renderSection();
    openCreate();
    fireEvent.mouseDown(screen.getByRole('combobox', { name: t.applicationHealthIntervalLabel }));
    fireEvent.click(await screen.findByRole('option', { name: t.applicationHealthIntervalCustom }));
    fireEvent.change(screen.getByLabelText(t.applicationHealthIntervalSecondsLabel), {
      target: { value: '45' },
    });
    fireEvent.click(screen.getByRole('button', { name: t.applicationCreate }));
    await waitFor(() => expect(created).toHaveLength(1));
    expect(created[0].health_check_interval_seconds).toBe(45);
  });

  it('hides the interval control for always_reachable mode', async () => {
    renderSection();
    openCreate();
    await selectHealthMode(t.applicationAlwaysReachable);
    expect(
      screen.queryByRole('combobox', { name: t.applicationHealthIntervalLabel }),
    ).not.toBeInTheDocument();
  });

  it('populates Custom + the stored seconds on edit', async () => {
    renderSection({
      apps: [
        makeApp({
          id: 'app_1',
          health_check_mode: 'health_path',
          health_check_interval_seconds: 45,
        }),
      ],
    });
    await screen.findByText('https://s1.example.test:8000');
    fireEvent.click(screen.getByRole('button', { name: t.applicationEdit }));
    expect(
      screen.getByRole('combobox', { name: t.applicationHealthIntervalLabel }),
    ).toHaveTextContent(t.applicationHealthIntervalCustom);
    expect(
      (screen.getByLabelText(t.applicationHealthIntervalSecondsLabel) as HTMLInputElement).value,
    ).toBe('45');
  });
});

describe('ApplicationSection litellm options', () => {
  it('offers litellm in the application-type dropdown', async () => {
    renderSection();
    openCreate();
    fireEvent.mouseDown(screen.getByRole('combobox', { name: t.applicationType }));
    expect(await screen.findByRole('option', { name: 'litellm' })).toBeInTheDocument();
  });

  it('offers the litellm option in the loaded-models format dropdown', async () => {
    renderSection();
    openCreate();
    // The format select is disabled until a loaded-models path is set.
    fireEvent.change(screen.getByLabelText(t.applicationLoadedModelsPath), {
      target: { value: '/health' },
    });
    fireEvent.mouseDown(screen.getByRole('combobox', { name: t.applicationLoadedModelsFormat }));
    expect(
      await screen.findByRole('option', { name: t.applicationLoadedFormatLitellm }),
    ).toBeInTheDocument();
  });
});

describe('ApplicationSection type prefill', () => {
  it('seeds ollama defaults on the fresh create form', () => {
    renderSection();
    openCreate();
    const pathField = screen.getByLabelText(t.applicationLoadedModelsPath) as HTMLInputElement;
    expect(pathField.value).toBe('/api/ps');
  });

  it('prefills type-specific fields when the type changes to llama_swap', async () => {
    renderSection();
    openCreate();
    await selectType('llama_swap');
    expect((screen.getByLabelText(t.applicationLoadedModelsPath) as HTMLInputElement).value).toBe(
      '/running',
    );
    expect((screen.getByLabelText(t.applicationContextProbePath) as HTMLInputElement).value).toBe(
      '/upstream/{model}/props',
    );
  });

  it('keeps a customized loaded-models path across a type change', async () => {
    renderSection();
    openCreate();
    const pathField = screen.getByLabelText(t.applicationLoadedModelsPath) as HTMLInputElement;
    fireEvent.change(pathField, { target: { value: '/custom' } });
    await selectType('llama_swap');
    expect((screen.getByLabelText(t.applicationLoadedModelsPath) as HTMLInputElement).value).toBe(
      '/custom',
    );
    // an untouched field still migrates:
    expect((screen.getByLabelText(t.applicationContextProbePath) as HTMLInputElement).value).toBe(
      '/upstream/{model}/props',
    );
  });
});

describe('ApplicationSection server_agent type', () => {
  it('offers server_agent in the application-type dropdown', async () => {
    renderSection();
    openCreate();
    fireEvent.mouseDown(screen.getByRole('combobox', { name: t.applicationType }));
    expect(await screen.findByRole('option', { name: 'server_agent' })).toBeInTheDocument();
  });

  // The backend defaults a server_agent application's timeout_ms to 600000
  // (10 minutes, a TOTAL request deadline covering a cold model load) instead
  // of the usual 30000 -- migrateTypeFields must carry that default the same
  // way it carries every other type-specific field.
  it('migrates the timeout to 600000 when switching to server_agent with an untouched timeout', async () => {
    renderSection();
    openCreate();
    await selectType('server_agent');
    expect((screen.getByLabelText(t.applicationTimeout) as HTMLInputElement).value).toBe('600000');
  });

  it('preserves a customized timeout across a switch to server_agent', async () => {
    renderSection();
    openCreate();
    const timeoutField = screen.getByLabelText(t.applicationTimeout) as HTMLInputElement;
    fireEvent.change(timeoutField, { target: { value: '45000' } });
    await selectType('server_agent');
    expect((screen.getByLabelText(t.applicationTimeout) as HTMLInputElement).value).toBe('45000');
  });
});

describe('ApplicationSection reachability indicator', () => {
  it('shows a reachable chip with the last-checked timestamp in a tooltip', async () => {
    renderSection({
      apps: [makeApp({ id: 'app_1', reachable: true, last_checked_at: '2026-07-16T12:00:00Z' })],
    });
    await screen.findByText('https://s1.example.test:8000');

    // The reachable column header also reads "Erreichbar", so scope the chip
    // assertion to the tooltip wrapper (which carries the last-checked title).
    const expectedTitle = formatDate('2026-07-16T12:00:00Z', t.applicationReachableNever);
    const tooltip = screen.getByTitle(expectedTitle);
    expect(within(tooltip).getByText(t.applicationReachable)).toBeInTheDocument();
  });

  it("shows an unreachable chip and a 'not checked yet' tooltip when last_checked_at is null", async () => {
    renderSection({ apps: [makeApp({ id: 'app_1', reachable: false, last_checked_at: null })] });
    await screen.findByText('https://s1.example.test:8000');

    expect(screen.getByText(t.applicationUnreachable)).toBeInTheDocument();
    expect(screen.getByTitle(t.applicationReachableNever)).toBeInTheDocument();
  });
});

// P4 Task 11: read-only proxy status, derived CLIENT-SIDE (no new backend
// field) as scheme==="https" && proxy_listen_port!==0 -- see routing.
// ApplicationEndpoint's identical derivation in the backend.
describe('ApplicationSection proxy status indicator (P4 Task 11)', () => {
  it('shows a proxied chip when scheme is https and a proxy_listen_port is assigned', async () => {
    renderSection({ apps: [makeApp({ id: 'app_1', scheme: 'https', proxy_listen_port: 8601 })] });
    await screen.findByText('https://s1.example.test:8000');
    expect(screen.getByText(t.applicationProxied)).toBeInTheDocument();
  });

  it('shows a not-proxied chip when scheme is https but no proxy_listen_port is assigned yet', async () => {
    renderSection({ apps: [makeApp({ id: 'app_1', scheme: 'https', proxy_listen_port: 0 })] });
    await screen.findByText('https://s1.example.test:8000');
    expect(screen.getByText(t.applicationNotProxied)).toBeInTheDocument();
  });

  it('shows a not-proxied chip for a plain http application even with a stale proxy_listen_port', async () => {
    renderSection({ apps: [makeApp({ id: 'app_1', scheme: 'http', proxy_listen_port: 8601 })] });
    await screen.findByText('https://s1.example.test:8000');
    expect(screen.getByText(t.applicationNotProxied)).toBeInTheDocument();
  });
});

describe('ApplicationSection native passthrough toggles', () => {
  it('defaults the native toggles per the ollama type (responses off, messages on) and sends the toggled state on create', async () => {
    const { created } = renderSection();
    openCreate();
    const responses = screen.getByRole('checkbox', {
      name: t.applicationNativeResponses,
    }) as HTMLInputElement;
    const messages = screen.getByRole('checkbox', {
      name: t.applicationNativeMessages,
    }) as HTMLInputElement;
    // ollama's type defaults: no /v1/responses, but /v1/messages since v0.14.0.
    expect(responses.checked).toBe(false);
    expect(messages.checked).toBe(true);

    fireEvent.click(responses); // also enable Codex passthrough
    fireEvent.click(screen.getByRole('button', { name: t.applicationCreate }));

    await waitFor(() => expect(created).toHaveLength(1));
    expect(created[0].native_responses).toBe(true);
    expect(created[0].native_messages).toBe(true);
  });

  it('populates the native toggles on edit and saves a change', async () => {
    const { updated } = renderSection({
      apps: [makeApp({ id: 'app_1', native_responses: true, native_messages: false })],
    });
    await screen.findByText('https://s1.example.test:8000');
    fireEvent.click(screen.getByRole('button', { name: t.applicationEdit }));

    const responses = screen.getByRole('checkbox', {
      name: t.applicationNativeResponses,
    }) as HTMLInputElement;
    const messages = screen.getByRole('checkbox', {
      name: t.applicationNativeMessages,
    }) as HTMLInputElement;
    expect(responses.checked).toBe(true);
    expect(messages.checked).toBe(false);

    fireEvent.click(messages); // also enable Claude Code passthrough
    fireEvent.click(screen.getByRole('button', { name: t.applicationSave }));

    await waitFor(() => expect(updated).toHaveLength(1));
    expect(updated[0].body.native_responses).toBe(true);
    expect(updated[0].body.native_messages).toBe(true);
  });
});

describe('ApplicationSection metrics auto-update toggles', () => {
  it('defaults both metrics toggles off, hides the interval field, and sends the defaults on create', async () => {
    const { created } = renderSection();
    openCreate();
    const scheduled = screen.getByRole('checkbox', {
      name: t.applicationScheduledBenchmark,
    }) as HTMLInputElement;
    const opportunistic = screen.getByRole('checkbox', {
      name: t.applicationOpportunisticMetrics,
    }) as HTMLInputElement;
    expect(scheduled.checked).toBe(false);
    expect(opportunistic.checked).toBe(false);
    // The interval field only appears once the scheduled toggle is on.
    expect(
      screen.queryByLabelText(t.applicationScheduledBenchmarkIntervalLabel),
    ).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: t.applicationCreate }));
    await waitFor(() => expect(created).toHaveLength(1));
    expect(created[0].benchmark_schedule_enabled).toBe(false);
    expect(created[0].opportunistic_metrics_enabled).toBe(false);
    // Interval is forced to 0 while the schedule is off.
    expect(created[0].benchmark_schedule_interval_seconds).toBe(0);
  });

  it('reveals the interval field when scheduled is enabled and sends all three fields on create', async () => {
    const { created } = renderSection();
    openCreate();
    fireEvent.click(screen.getByRole('checkbox', { name: t.applicationScheduledBenchmark }));
    fireEvent.click(screen.getByRole('checkbox', { name: t.applicationOpportunisticMetrics }));
    const interval = screen.getByLabelText(
      t.applicationScheduledBenchmarkIntervalLabel,
    ) as HTMLInputElement;
    expect(interval).toBeInTheDocument();
    fireEvent.change(interval, { target: { value: '3600' } });

    fireEvent.click(screen.getByRole('button', { name: t.applicationCreate }));
    await waitFor(() => expect(created).toHaveLength(1));
    expect(created[0].benchmark_schedule_enabled).toBe(true);
    expect(created[0].opportunistic_metrics_enabled).toBe(true);
    expect(created[0].benchmark_schedule_interval_seconds).toBe(3600);
  });

  it('hydrates the toggles + interval from the DTO on edit', async () => {
    renderSection({
      apps: [
        makeApp({
          id: 'app_1',
          benchmark_schedule_enabled: true,
          opportunistic_metrics_enabled: true,
          benchmark_schedule_interval_seconds: 7200,
        }),
      ],
    });
    await screen.findByText('https://s1.example.test:8000');
    fireEvent.click(screen.getByRole('button', { name: t.applicationEdit }));

    const scheduled = screen.getByRole('checkbox', {
      name: t.applicationScheduledBenchmark,
    }) as HTMLInputElement;
    const opportunistic = screen.getByRole('checkbox', {
      name: t.applicationOpportunisticMetrics,
    }) as HTMLInputElement;
    expect(scheduled.checked).toBe(true);
    expect(opportunistic.checked).toBe(true);
    expect(
      (screen.getByLabelText(t.applicationScheduledBenchmarkIntervalLabel) as HTMLInputElement)
        .value,
    ).toBe('7200');
  });
});

describe('ApplicationSection path suffix + upstream token', () => {
  it('submits app_path_suffix (trimmed) and omits api_token when the token field is untouched on create', async () => {
    const { created } = renderSection();
    openCreate();
    fireEvent.change(screen.getByLabelText(t.applicationPathSuffixLabel), {
      target: { value: '  /v1beta  ' },
    });

    fireEvent.click(screen.getByRole('button', { name: t.applicationCreate }));
    await waitFor(() => expect(created).toHaveLength(1));
    expect(created[0].app_path_suffix).toBe('/v1beta');
    // An empty token field means the field is omitted entirely (no token on create).
    expect('api_token' in created[0]).toBe(false);
  });

  it('sends the typed api_token and the token header (trimmed) on create', async () => {
    const { created } = renderSection();
    openCreate();
    fireEvent.change(screen.getByLabelText(t.applicationApiTokenLabel), {
      target: { value: 'sk-secret' },
    });
    fireEvent.change(screen.getByLabelText(t.applicationApiTokenHeaderLabel), {
      target: { value: ' x-api-key ' },
    });

    fireEvent.click(screen.getByRole('button', { name: t.applicationCreate }));
    await waitFor(() => expect(created).toHaveLength(1));
    expect(created[0].api_token).toBe('sk-secret');
    expect(created[0].api_token_header).toBe('x-api-key');
  });

  it("shows the 'set' placeholder + clear button on edit and keeps the stored token when untouched", async () => {
    const { updated } = renderSection({
      apps: [
        makeApp({
          id: 'app_1',
          api_token_set: true,
          api_token_header: 'x-api-key',
          app_path_suffix: '/models',
        }),
      ],
    });
    await screen.findByText('https://s1.example.test:8000');
    fireEvent.click(screen.getByRole('button', { name: t.applicationEdit }));

    const tokenField = screen.getByLabelText(t.applicationApiTokenLabel) as HTMLInputElement;
    // The secret is never hydrated into the field (write-only).
    expect(tokenField.value).toBe('');
    expect(tokenField.placeholder).toBe(t.applicationApiTokenSetPlaceholder);
    // The header + path suffix hydrate from the DTO.
    expect(
      (screen.getByLabelText(t.applicationApiTokenHeaderLabel) as HTMLInputElement).value,
    ).toBe('x-api-key');
    expect((screen.getByLabelText(t.applicationPathSuffixLabel) as HTMLInputElement).value).toBe(
      '/models',
    );
    // A clear button is offered while a token is stored.
    expect(screen.getByRole('button', { name: t.applicationApiTokenClear })).toBeInTheDocument();

    // Saving without touching the token omits api_token (keep the stored one).
    fireEvent.click(screen.getByRole('button', { name: t.applicationSave }));
    await waitFor(() => expect(updated).toHaveLength(1));
    expect('api_token' in updated[0].body).toBe(false);
    expect(updated[0].body.api_token_header).toBe('x-api-key');
  });

  it("clears the stored token (api_token: '') when the clear button is used", async () => {
    const { updated } = renderSection({ apps: [makeApp({ id: 'app_1', api_token_set: true })] });
    await screen.findByText('https://s1.example.test:8000');
    fireEvent.click(screen.getByRole('button', { name: t.applicationEdit }));

    fireEvent.click(screen.getByRole('button', { name: t.applicationApiTokenClear }));
    // Once cleared the clear button + set placeholder disappear.
    expect(
      screen.queryByRole('button', { name: t.applicationApiTokenClear }),
    ).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: t.applicationSave }));
    await waitFor(() => expect(updated).toHaveLength(1));
    expect(updated[0].body.api_token).toBe('');
  });
});

describe('ApplicationSection health-interval default display', () => {
  it('shows the live system default value in the Standard branch', async () => {
    renderSection({ healthInterval: 45 });
    openCreate();
    // MuiTypography variant="caption" renders as a <span> (not a <p>) by
    // default, so match on the element's own text content rather than the tag.
    expect(
      await screen.findByText(
        (_content, el) =>
          el?.tagName.toLowerCase() === 'span' &&
          (el.textContent ?? '').includes(`${t.applicationHealthIntervalCurrent}: 45`),
      ),
    ).toBeTruthy();
  });

  it('falls back to the static note when the interval fetch fails', async () => {
    renderSection({ healthInterval: 'error' });
    openCreate();
    expect(screen.getByText(t.applicationHealthIntervalNote, { exact: false })).toBeInTheDocument();
    await waitFor(() => {
      expect(screen.queryByText(new RegExp(`${t.applicationHealthIntervalCurrent}:`))).toBeNull();
    });
  });
});

// Agent-runtime-manager (Task 20): the "manage models" drill-down opens
// RuntimeAdminSection instead of MappingSection for a server_agent
// application, and the managed_runtime_only server restriction (mirroring
// the backend's own CreateApplication gate, Task 6) drives the create
// button + auto-drill in the list view.
describe('ApplicationSection server_agent drill-down + managed_runtime_only', () => {
  it('opens RuntimeAdminSection instead of MappingSection for server_agent apps', async () => {
    renderSection({ apps: [makeApp({ id: 'app_1', type: 'server_agent' })] });
    await screen.findByText('https://s1.example.test:8000');

    fireEvent.click(screen.getByRole('button', { name: t.mappingManage }));

    // RuntimeAdminSection's tab strip, not MappingSection's list heading.
    expect(await screen.findByRole('tab', { name: t.runtimeSpecs })).toBeInTheDocument();
    expect(screen.getByText(t.runtimeMatrix)).toBeInTheDocument();
    expect(screen.getByText(t.runtimeLimits)).toBeInTheDocument();
    expect(screen.getByText(t.runtimeLiveStatus)).toBeInTheDocument();
    expect(screen.queryByText(t.modelMappings)).not.toBeInTheDocument();
  });

  it('auto-drills into the one server_agent app on a managed_runtime_only server', async () => {
    renderSection({
      server: { ...server, managed_runtime_only: true },
      apps: [makeApp({ id: 'app_1', type: 'server_agent' })],
    });

    // Auto-drilled straight past the one-item list into the runtime admin.
    expect(await screen.findByRole('tab', { name: t.runtimeSpecs })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: t.applicationCreate })).not.toBeInTheDocument();
  });

  it('hides create (without auto-drilling) and shows the banner with more than one server_agent app', async () => {
    renderSection({
      server: { ...server, managed_runtime_only: true },
      apps: [
        makeApp({ id: 'app_1', type: 'server_agent' }),
        makeApp({ id: 'app_2', type: 'server_agent' }),
      ],
    });

    expect(await screen.findByText(t.runtimeManagedOnlyBanner)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: t.applicationCreate })).not.toBeInTheDocument();
    // Stayed on the list (two candidates -> no unambiguous auto-drill target).
    expect(screen.getAllByRole('button', { name: t.mappingManage })).toHaveLength(2);
  });

  it('shows create (defaulted to server_agent) and the banner when managed_runtime_only has no app yet', async () => {
    renderSection({ server: { ...server, managed_runtime_only: true }, apps: [] });

    await screen.findByText(t.runtimeManagedOnlyBanner);
    fireEvent.click(screen.getByRole('button', { name: t.applicationCreate }));
    expect(screen.getByRole('combobox', { name: t.applicationType })).toHaveTextContent(
      'server_agent',
    );
  });
});
