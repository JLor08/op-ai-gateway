// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ApplicationSection } from './ApplicationSection';
import { ToastProvider } from './shared/ToastProvider';
import { formatDate } from './shared/format';
import { messages } from '../i18n';
import { PortalApiError } from '../api';
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
    probeMappingVram: vi.fn(async () => idleBenchmark),
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

// Same, but for an option that may be DISABLED. A disabled MenuItem swallows
// the click, so the menu stays open -- and an open MUI menu aria-hides the page
// behind it, which makes the combobox itself unqueryable. Close the menu the
// way an operator would before asserting on the field. Harmless when the click
// landed and the menu closed on its own.
async function attemptSelectType(optionText: string) {
  fireEvent.mouseDown(screen.getByRole('combobox', { name: t.applicationType }));
  fireEvent.click(await screen.findByRole('option', { name: optionText }));
  const listbox = screen.queryByRole('listbox');
  if (listbox !== null) fireEvent.keyDown(listbox, { key: 'Escape' });
  await waitFor(() => expect(screen.queryByRole('listbox')).not.toBeInTheDocument());
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

// Read-only proxy status. 'proxied' is still derived CLIENT-SIDE as
// scheme==="https" && proxy_listen_port!==0 (routing.ApplicationEndpoint's
// identical derivation), but 'excluded' is the operator-owned flag and is
// tested first, so the chip carries THREE distinct states rather than folding
// a deliberate choice in with "waiting".
describe('ApplicationSection proxy status indicator', () => {
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

  it('shows a distinct excluded chip, not the amber not-proxied one', async () => {
    renderSection({
      apps: [makeApp({ id: 'app_1', scheme: 'http', proxy_listen_port: 0, proxy_excluded: true })],
    });
    await screen.findByText('https://s1.example.test:8000');
    expect(screen.getByText(t.applicationProxyChipExcluded)).toBeInTheDocument();
    expect(screen.queryByText(t.applicationNotProxied)).toBeNull();
  });

  it('renders all three labels at once, so the enum filter can offer three states', async () => {
    renderSection({
      apps: [
        makeApp({ id: 'app_1', scheme: 'https', proxy_listen_port: 8601 }),
        makeApp({ id: 'app_2', port: 8002, scheme: 'http', proxy_listen_port: 0 }),
        makeApp({ id: 'app_3', port: 8003, scheme: 'http', proxy_excluded: true }),
      ],
    });
    await screen.findByText(t.applicationProxied);
    expect(screen.getByText(t.applicationNotProxied)).toBeInTheDocument();
    expect(screen.getByText(t.applicationProxyChipExcluded)).toBeInTheDocument();
  });
});

// The per-application opt-out from the gateway's TLS proxy.
describe('ApplicationSection proxy opt-out control', () => {
  const proxyServer: PortalServer = { ...server, tls_proxy_state: 'proxy' };
  const proxyCheckbox = () =>
    screen.getByRole('checkbox', { name: t.applicationProxyExcluded }) as HTMLInputElement;

  // SUBMIT DISCIPLINE. buildBody returns ONE literal reused verbatim for
  // update, so a field added there is restated on EVERY save. Restating this
  // one would silently rewrite the operator's opt-out from whatever the form
  // last read — and return an ordinary 200 with nothing to notice.
  it('omits proxy_excluded entirely when the operator did not move the checkbox', async () => {
    const { updated } = renderSection({
      server: proxyServer,
      apps: [makeApp({ id: 'app_1', scheme: 'http', proxy_excluded: true })],
    });
    await screen.findByText('https://s1.example.test:8000');
    fireEvent.click(screen.getByRole('button', { name: t.applicationEdit }));
    expect(proxyCheckbox().checked).toBe(true);

    fireEvent.change(screen.getByLabelText(t.applicationWeight), { target: { value: '7' } });
    fireEvent.click(screen.getByRole('button', { name: t.applicationSave }));
    await waitFor(() => expect(updated).toHaveLength(1));
    expect('proxy_excluded' in updated[0].body).toBe(false);
    expect(updated[0].body.weight).toBe(7);
  });

  it('sends proxy_excluded with the moved value, and the scheme rides along in the same request', async () => {
    const { updated } = renderSection({
      server: proxyServer,
      apps: [makeApp({ id: 'app_1', scheme: 'https', proxy_listen_port: 8601 })],
    });
    await screen.findByText('https://s1.example.test:8000');
    fireEvent.click(screen.getByRole('button', { name: t.applicationEdit }));
    expect(proxyCheckbox().checked).toBe(false);

    fireEvent.click(proxyCheckbox());
    fireEvent.click(screen.getByRole('button', { name: t.applicationSave }));
    await waitFor(() => expect(updated).toHaveLength(1));
    expect(updated[0].body.proxy_excluded).toBe(true);
    // ONE PATCH. Scheme and participation must land together: an excluded
    // application on an in-scope server is in neither recovery arm, so a
    // half-applied state would be permanent, not transient.
    expect(updated[0].body.scheme).toBe('http');
  });

  // VISIBILITY. Only out_of_scope hides the control, and it renders an
  // explanation in the slot rather than a blank.
  it('renders the control with the proxy-mode sentence when the agent runs the proxy', async () => {
    renderSection({ server: proxyServer, apps: [makeApp({ id: 'app_1' })] });
    await screen.findByText('https://s1.example.test:8000');
    fireEvent.click(screen.getByRole('button', { name: t.applicationEdit }));
    expect(proxyCheckbox()).toBeInTheDocument();
    expect(screen.getByText(t.applicationProxyModeActiveNote)).toBeInTheDocument();
  });

  it('renders the control with the agent-off sentence when the agent reports off/files', async () => {
    renderSection({
      server: { ...server, tls_proxy_state: 'agent_off' },
      apps: [makeApp({ id: 'app_1' })],
    });
    await screen.findByText('https://s1.example.test:8000');
    fireEvent.click(screen.getByRole('button', { name: t.applicationEdit }));
    expect(proxyCheckbox()).toBeInTheDocument();
    expect(screen.getByText(t.applicationProxyModeOffNote)).toBeInTheDocument();
  });

  it('renders the control with the unknown sentence when nothing has been reported', async () => {
    renderSection({
      server: { ...server, tls_proxy_state: 'unknown' },
      apps: [makeApp({ id: 'app_1' })],
    });
    await screen.findByText('https://s1.example.test:8000');
    fireEvent.click(screen.getByRole('button', { name: t.applicationEdit }));
    expect(proxyCheckbox()).toBeInTheDocument();
    expect(screen.getByText(t.applicationProxyModeUnknownNote)).toBeInTheDocument();
    expect(screen.queryByText(t.applicationProxyModeOffNote)).toBeNull();
  });

  // An older backend, or a DTO that lost the field, must SHOW the control.
  it('defaults a missing tls_proxy_state to unknown rather than hiding the control', async () => {
    renderSection({ apps: [makeApp({ id: 'app_1' })] });
    await screen.findByText('https://s1.example.test:8000');
    fireEvent.click(screen.getByRole('button', { name: t.applicationEdit }));
    expect(proxyCheckbox()).toBeInTheDocument();
    expect(screen.getByText(t.applicationProxyModeUnknownNote)).toBeInTheDocument();
  });

  it('hides the control out of scope and explains why IN ITS PLACE, never a blank', async () => {
    renderSection({
      server: { ...server, tls_proxy_state: 'out_of_scope' },
      apps: [makeApp({ id: 'app_1' })],
    });
    await screen.findByText('https://s1.example.test:8000');
    fireEvent.click(screen.getByRole('button', { name: t.applicationEdit }));
    expect(screen.queryByRole('checkbox', { name: t.applicationProxyExcluded })).toBeNull();
    // Asserted on the EXPLANATION, not merely on absence: an absence-only
    // assertion passes for a blank slot too.
    expect(screen.getByText(t.applicationProxyOutOfScopeNote)).toBeInTheDocument();
  });

  // THE OVERRIDE, non-negotiable: an operator must always be able to see and
  // undo their own setting, even on a server that runs no proxy.
  it('still renders the control out of scope when the application IS excluded', async () => {
    renderSection({
      server: { ...server, tls_proxy_state: 'out_of_scope' },
      apps: [makeApp({ id: 'app_1', scheme: 'http', proxy_excluded: true })],
    });
    await screen.findByText('https://s1.example.test:8000');
    fireEvent.click(screen.getByRole('button', { name: t.applicationEdit }));
    expect(proxyCheckbox().checked).toBe(true);
  });

  // SCHEME COUPLING.
  it('disables the scheme select on a participating in-scope application and says why', async () => {
    renderSection({ server: proxyServer, apps: [makeApp({ id: 'app_1', scheme: 'http' })] });
    await screen.findByText('https://s1.example.test:8000');
    fireEvent.click(screen.getByRole('button', { name: t.applicationEdit }));
    const scheme = screen.getByRole('combobox', { name: t.applicationScheme });
    expect(scheme).toHaveAttribute('aria-disabled', 'true');
    expect(screen.getByText(t.applicationSchemeManagedNote)).toBeInTheDocument();
  });

  it('leaves the scheme select enabled on an out-of-scope server', async () => {
    renderSection({
      server: { ...server, tls_proxy_state: 'out_of_scope' },
      apps: [makeApp({ id: 'app_1', scheme: 'http' })],
    });
    await screen.findByText('https://s1.example.test:8000');
    fireEvent.click(screen.getByRole('button', { name: t.applicationEdit }));
    expect(screen.getByRole('combobox', { name: t.applicationScheme })).not.toHaveAttribute(
      'aria-disabled',
      'true',
    );
  });

  it('never re-sends http for an already-switched participating https application', async () => {
    const { updated } = renderSection({
      server: proxyServer,
      apps: [makeApp({ id: 'app_1', scheme: 'https', proxy_listen_port: 8601 })],
    });
    await screen.findByText('https://s1.example.test:8000');
    fireEvent.click(screen.getByRole('button', { name: t.applicationEdit }));
    // An unrelated save on a disabled select must re-send the STORED scheme,
    // so it is a no-op rather than a downgrade window.
    fireEvent.change(screen.getByLabelText(t.applicationWeight), { target: { value: '3' } });
    fireEvent.click(screen.getByRole('button', { name: t.applicationSave }));
    await waitFor(() => expect(updated).toHaveLength(1));
    expect(updated[0].body.scheme).toBe('https');
  });

  it('unticking forces http, the only re-entry into the proxy', async () => {
    const { updated } = renderSection({
      server: proxyServer,
      apps: [makeApp({ id: 'app_1', scheme: 'https', proxy_excluded: true })],
    });
    await screen.findByText('https://s1.example.test:8000');
    fireEvent.click(screen.getByRole('button', { name: t.applicationEdit }));
    fireEvent.click(proxyCheckbox());
    fireEvent.click(screen.getByRole('button', { name: t.applicationSave }));
    await waitFor(() => expect(updated).toHaveLength(1));
    expect(updated[0].body.proxy_excluded).toBe(false);
    expect(updated[0].body.scheme).toBe('http');
  });

  it('warns about the operator own-TLS obligation only while excluded AND https', async () => {
    renderSection({
      server: proxyServer,
      apps: [makeApp({ id: 'app_1', scheme: 'https', proxy_excluded: true })],
    });
    await screen.findByText('https://s1.example.test:8000');
    fireEvent.click(screen.getByRole('button', { name: t.applicationEdit }));
    expect(screen.getByText(t.applicationProxyOwnTLSWarning(server.domain))).toBeInTheDocument();
    // Re-entering the proxy forces http, which retires the obligation.
    fireEvent.click(proxyCheckbox());
    expect(screen.queryByText(t.applicationProxyOwnTLSWarning(server.domain))).toBeNull();
  });

  // THE READ-ONLY PORT: static text, never a form control, and 0 never renders
  // as "0", as a blank, or as a dash.
  it('renders the assigned proxy port as static text with no control of that name', async () => {
    renderSection({
      server: proxyServer,
      apps: [makeApp({ id: 'app_1', scheme: 'https', proxy_listen_port: 8601 })],
    });
    await screen.findByText('https://s1.example.test:8000');
    fireEvent.click(screen.getByRole('button', { name: t.applicationEdit }));
    expect(screen.getByText(t.applicationProxyPort(8601))).toBeInTheDocument();
    expect(screen.queryByLabelText(/proxy.?listen.?port/i)).toBeNull();
  });

  it('spells out the not-yet-assigned state for a participating application', async () => {
    renderSection({
      server: proxyServer,
      apps: [makeApp({ id: 'app_1', scheme: 'http', proxy_listen_port: 0 })],
    });
    await screen.findByText('https://s1.example.test:8000');
    fireEvent.click(screen.getByRole('button', { name: t.applicationEdit }));
    expect(screen.getByText(t.applicationProxyPortUnassigned)).toBeInTheDocument();
  });

  it('says the excluded application holds no port at all', async () => {
    renderSection({
      server: proxyServer,
      apps: [makeApp({ id: 'app_1', scheme: 'http', proxy_excluded: true })],
    });
    await screen.findByText('https://s1.example.test:8000');
    fireEvent.click(screen.getByRole('button', { name: t.applicationEdit }));
    expect(screen.getByText(t.applicationProxyPortExcluded)).toBeInTheDocument();
  });

  // CREATE. The showProxyControls gate is load-bearing here: an out-of-scope
  // https create must send NO key so the backend normalizes it, rather than an
  // explicit false it would refuse.
  it('sends proxy_excluded on create when the control is visible', async () => {
    const { created } = renderSection({ server: proxyServer });
    openCreate();
    fireEvent.click(proxyCheckbox());
    // In the form sub-view the submit button reuses the create label and the
    // list's own create action is no longer rendered, so this is unambiguous.
    fireEvent.click(screen.getByRole('button', { name: t.applicationCreate }));
    await waitFor(() => expect(created).toHaveLength(1));
    expect(created[0].proxy_excluded).toBe(true);
  });

  it('omits proxy_excluded on create when the control is hidden', async () => {
    const { created } = renderSection({ server: { ...server, tls_proxy_state: 'out_of_scope' } });
    openCreate();
    // In the form sub-view the submit button reuses the create label and the
    // list's own create action is no longer rendered, so this is unambiguous.
    fireEvent.click(screen.getByRole('button', { name: t.applicationCreate }));
    await waitFor(() => expect(created).toHaveLength(1));
    expect('proxy_excluded' in created[0]).toBe(false);
  });
});

// The three-state endpoint-mode dropdowns (design: API-variant endpoint modes)
// that replaced the two native-passthrough checkboxes. openai gates the Codex
// (Responses) dropdown; anthropic gates the Claude Code (Messages) dropdown.
describe('ApplicationSection API-variant endpoint modes', () => {
  const responsesField = () => screen.getByRole('combobox', { name: t.applicationResponsesMode });
  const messagesField = () => screen.getByRole('combobox', { name: t.applicationMessagesMode });

  async function selectMode(field: HTMLElement, optionLabel: string) {
    fireEvent.mouseDown(field);
    fireEvent.click(await screen.findByRole('option', { name: optionLabel }));
  }

  it('defaults both modes to Durchreichen and sends passthrough on create', async () => {
    const { created } = renderSection();
    openCreate();
    expect(responsesField()).toHaveTextContent(t.applicationModePassthrough);
    expect(messagesField()).toHaveTextContent(t.applicationModePassthrough);

    fireEvent.click(screen.getByRole('button', { name: t.applicationCreate }));
    await waitFor(() => expect(created).toHaveLength(1));
    expect(created[0].responses_mode).toBe('passthrough');
    expect(created[0].messages_mode).toBe('passthrough');
  });

  it('populates the modes on edit and saves a change', async () => {
    const { updated } = renderSection({
      apps: [makeApp({ id: 'app_1', responses_mode: 'translate', messages_mode: 'disabled' })],
    });
    await screen.findByText('https://s1.example.test:8000');
    fireEvent.click(screen.getByRole('button', { name: t.applicationEdit }));

    expect(responsesField()).toHaveTextContent(t.applicationModeTranslate);
    expect(messagesField()).toHaveTextContent(t.applicationModeDisabled);

    await selectMode(responsesField(), t.applicationModePassthrough);
    fireEvent.click(screen.getByRole('button', { name: t.applicationSave }));

    await waitFor(() => expect(updated).toHaveLength(1));
    expect(updated[0].body.responses_mode).toBe('passthrough');
    expect(updated[0].body.messages_mode).toBe('disabled');
  });

  it('disables the Codex dropdown when the openai flavor is unchecked, without clobbering the stored mode', async () => {
    const { created } = renderSection();
    openCreate();
    expect(responsesField()).not.toHaveAttribute('aria-disabled', 'true');

    fireEvent.click(screen.getByRole('checkbox', { name: 'openai' }));
    expect(responsesField()).toHaveAttribute('aria-disabled', 'true');
    expect(responsesField()).toHaveTextContent(t.applicationModeDisabled);

    fireEvent.click(screen.getByRole('button', { name: t.applicationCreate }));
    await waitFor(() => expect(created).toHaveLength(1));
    expect(created[0].api_flavors).toEqual(['anthropic']);
    // The stored mode rides along untouched; the backend's effective rule gates
    // on the unchecked flavor.
    expect(created[0].responses_mode).toBe('passthrough');
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
    expect(screen.getByRole('tab', { name: t.runtimeMappingTab })).toBeInTheDocument();
    expect(screen.getByText(t.runtimeMatrix)).toBeInTheDocument();
    expect(screen.getByText(t.runtimeLimits)).toBeInTheDocument();
    // Scoped to the tab role because the specs table's live-state column
    // carries this label too, so a bare text query would match twice. NOT
    // coverage for that relabel -- this reads the TAB's label, which the
    // relabel did not touch; RuntimeAdminSection.test.tsx pins the columns.
    expect(screen.getByRole('tab', { name: t.runtimeLiveStatus })).toBeInTheDocument();
    // MappingSection's own panel heading. RuntimeAdminSection has a panel by
    // that name too now, but only inside its model-mapping tab -- and 'specs'
    // is the tab this screen opens on.
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

// One server_agent application per AI server, because only one agent runs per
// server. The rule is enforced three levels down (migration 68's partial
// unique index, the portal service's pre-read, and the 409
// application.server_agent_exists); what is asserted here is the AFFORDANCE
// that says so before a whole form has been filled in -- plus, at the end, the
// 409 itself, which stays the enforcement and must keep rendering.
describe('ApplicationSection one server_agent application per server', () => {
  const agentApp = () =>
    makeApp({
      id: 'app_agent',
      type: 'server_agent',
      port: 9100,
      endpoint: 'https://s1.example.test:9100',
    });

  it('disables (rather than removes) the server_agent option once the server has one', async () => {
    renderSection({ apps: [agentApp()] });
    // Settling the fetch is load-bearing: the create button is not
    // loading-gated, so clicking it synchronously would read the still-empty
    // list and the test would pass for the wrong reason.
    await screen.findByText('https://s1.example.test:9100');
    openCreate();

    fireEvent.mouseDown(screen.getByRole('combobox', { name: t.applicationType }));
    const option = await screen.findByRole('option', { name: 'server_agent' });
    // Still listed, still named, but not choosable -- a vanished option would
    // teach nothing, and it is what blanks the edit form (see the edit tests).
    expect(option).toHaveAttribute('aria-disabled', 'true');
  });

  it('does not let the create form select a second server_agent', async () => {
    renderSection({ apps: [agentApp()] });
    await screen.findByText('https://s1.example.test:9100');
    openCreate();

    await attemptSelectType('server_agent');

    // 'ollama' is openCreate's seed on an ordinary server: the click was
    // swallowed by the disabled item, so the type never moved.
    expect(screen.getByRole('combobox', { name: t.applicationType })).toHaveTextContent('ollama');
  });

  it('carries the reason on the type field, where focus alone reaches it', async () => {
    renderSection({ apps: [agentApp()] });
    await screen.findByText('https://s1.example.test:9100');
    openCreate();

    // helperText -> aria-describedby on the combobox. A disabled MenuItem is
    // skipped by the listbox's arrow-key navigation, so a tooltip anchored to
    // the option would be unreachable by keyboard and screen reader.
    expect(screen.getByRole('combobox', { name: t.applicationType })).toHaveAccessibleDescription(
      t.applicationTypeServerAgentTaken,
    );
  });

  it('leaves the option live, and says nothing, on a server that has no agent application', async () => {
    renderSection();
    openCreate();

    // Asserted with the menu shut: an open MUI menu aria-hides the page behind
    // it, so the combobox is unreachable by role while the listbox is up.
    expect(
      screen.getByRole('combobox', { name: t.applicationType }),
    ).not.toHaveAccessibleDescription();
    fireEvent.mouseDown(screen.getByRole('combobox', { name: t.applicationType }));
    expect(await screen.findByRole('option', { name: 'server_agent' })).not.toHaveAttribute(
      'aria-disabled',
    );
  });

  it('keeps the existing server_agent application editable under its own type', async () => {
    renderSection({ apps: [agentApp()] });
    await screen.findByText('https://s1.example.test:9100');

    fireEvent.click(screen.getByRole('button', { name: t.applicationEdit }));

    const combo = screen.getByRole('combobox', { name: t.applicationType });
    // A gate written as "filter server_agent out of the options" leaves
    // openEdit's setType(app.type) pointing at a value with no matching
    // MenuItem, and MUI renders that as a BLANK combobox -- which buildBody
    // would then submit as a retype, since it always sends `type`.
    expect(combo).toHaveTextContent('server_agent');
    expect(combo).not.toHaveAccessibleDescription();
    fireEvent.mouseDown(combo);
    expect(await screen.findByRole('option', { name: 'server_agent' })).not.toHaveAttribute(
      'aria-disabled',
    );
  });

  it('lets that application switch its type away and back within one unsaved edit', async () => {
    renderSection({ apps: [agentApp()] });
    await screen.findByText('https://s1.example.test:9100');
    fireEvent.click(screen.getByRole('button', { name: t.applicationEdit }));

    await selectType('ollama');
    await selectType('server_agent');

    // The exclusion is keyed on the edited row's id, not on the form's current
    // type: keying it on the type would strand this operator on 'ollama'.
    expect(screen.getByRole('combobox', { name: t.applicationType })).toHaveTextContent(
      'server_agent',
    );
  });

  it('blocks retyping another application to server_agent while one exists', async () => {
    renderSection({ apps: [makeApp({ id: 'app_vllm', type: 'vllm' }), agentApp()] });
    await screen.findByText('https://s1.example.test:9100');

    const vllmRow = screen.getByText('https://s1.example.test:8000').closest('tr') as HTMLElement;
    fireEvent.click(within(vllmRow).getByRole('button', { name: t.applicationEdit }));
    expect(screen.getByRole('combobox', { name: t.applicationType })).toHaveTextContent('vllm');

    await attemptSelectType('server_agent');

    // The second write path to the same violation; the backend guards it with
    // the same sentinel, so the affordance must cover it too.
    expect(screen.getByRole('combobox', { name: t.applicationType })).toHaveTextContent('vllm');
    expect(screen.getByRole('combobox', { name: t.applicationType })).toHaveAccessibleDescription(
      t.applicationTypeServerAgentTaken,
    );
  });

  it('still shows the type when a pre-invariant server holds two agent applications', async () => {
    // Only reachable on a database that already held duplicates when migration
    // 68 ran (the index is skipped there). Both rows stay editable: the option
    // is disabled -- the OTHER duplicate really would collide -- but MUI
    // computes the closed combobox's text from the matching child regardless
    // of its disabled state, so nothing is blanked and nothing is lost.
    renderSection({ apps: [makeApp({ id: 'app_a', type: 'server_agent' }), agentApp()] });
    await screen.findByText('https://s1.example.test:9100');

    const firstRow = screen.getByText('https://s1.example.test:8000').closest('tr') as HTMLElement;
    fireEvent.click(within(firstRow).getByRole('button', { name: t.applicationEdit }));

    const combo = screen.getByRole('combobox', { name: t.applicationType });
    expect(combo).toHaveTextContent('server_agent');
    expect(combo).toHaveAccessibleDescription(t.applicationTypeServerAgentTaken);
  });

  it('still renders the 409 when the local list was stale, and keeps the form open', async () => {
    // The affordance is not the enforcement and cannot be: this list is fetched
    // once, never polled, and reads as [] both while the first fetch is in
    // flight and after one failed. Rendered here with no applications at all,
    // so the UI gate is open and only the backend stands.
    const { fakeApi } = renderSection();
    openCreate();
    await selectType('server_agent');
    fakeApi.createApplication.mockRejectedValueOnce(
      new PortalApiError(
        409,
        'application.server_agent_exists',
        'server already has a server_agent application',
      ),
    );

    fireEvent.click(screen.getByRole('button', { name: t.applicationCreate }));

    expect(
      await screen.findByText(
        `application.server_agent_exists: ${t.errorApplicationServerAgentExists}`,
      ),
    ).toBeInTheDocument();
    // Form still open, typed data intact -- submitCreate only leaves on success.
    expect(screen.getByRole('combobox', { name: t.applicationType })).toHaveTextContent(
      'server_agent',
    );
  });
});

// managed_runtime_only is the SECOND gate on this same control, and it is
// CREATE-ONLY: the backend reads Server.ManagedRuntimeOnly inside
// CreateApplication and nowhere else -- UpdateApplication never looks at it.
// So an edit on such a server must keep offering all six types; a portal that
// disabled them there would refuse writes the backend accepts, silently.
describe('ApplicationSection managed_runtime_only type gate', () => {
  const managedServer: PortalServer = { ...server, managed_runtime_only: true };
  const allTypes = ['ollama', 'vllm', 'llama_cpp', 'llama_swap', 'litellm', 'server_agent'];
  const refusedTypes = allTypes.filter((type) => type !== 'server_agent');

  // THE point of the change. A naive `managedRuntimeOnly` predicate (no
  // `&& !editing`) turns this red: the five options come back aria-disabled
  // and the field grows a description it must not have here.
  it('offers every type, and disables none, when EDITING on such a server', async () => {
    renderSection({ server: managedServer, apps: [makeApp({ id: 'app_vllm', type: 'vllm' })] });
    // No server_agent application, so no auto-drill: the list renders.
    await screen.findByText('https://s1.example.test:8000');
    fireEvent.click(screen.getByRole('button', { name: t.applicationEdit }));

    const combo = screen.getByRole('combobox', { name: t.applicationType });
    expect(combo).toHaveTextContent('vllm');
    // Asserted with the menu SHUT: an open MUI menu aria-hides the page behind
    // it, so the combobox is unreachable by role while the listbox is up.
    expect(combo).not.toHaveAccessibleDescription();

    fireEvent.mouseDown(combo);
    for (const option of allTypes) {
      expect(await screen.findByRole('option', { name: option })).not.toHaveAttribute(
        'aria-disabled',
      );
    }
  });

  // The same fact asserted behaviourally rather than through ARIA: the write
  // the backend accepts actually goes out.
  it('saves an edit that retypes an application on such a server', async () => {
    const { updated } = renderSection({
      server: managedServer,
      apps: [makeApp({ id: 'app_vllm', type: 'vllm' })],
    });
    await screen.findByText('https://s1.example.test:8000');
    fireEvent.click(screen.getByRole('button', { name: t.applicationEdit }));

    await selectType('ollama');
    fireEvent.click(screen.getByRole('button', { name: t.applicationSave }));

    await waitFor(() => expect(updated).toHaveLength(1));
    expect(updated[0].body.type).toBe('ollama');
  });

  it('disables the five types the backend refuses when CREATING on such a server', async () => {
    renderSection({ server: managedServer, apps: [] });
    await screen.findByText(t.runtimeManagedOnlyBanner);
    openCreate();

    const combo = screen.getByRole('combobox', { name: t.applicationType });
    expect(combo).toHaveTextContent('server_agent');
    // helperText -> aria-describedby on the combobox, so the reason is
    // announced on focus, before the menu is ever opened, and is legible
    // without a hover. A tooltip on the disabled options would not be:
    // MUI leaves disabledItemsFocusable false, so arrow-key navigation
    // skips them entirely.
    expect(combo).toHaveAccessibleDescription(t.applicationTypeManagedRuntimeOnly);

    fireEvent.mouseDown(combo);
    expect(await screen.findByRole('option', { name: 'server_agent' })).not.toHaveAttribute(
      'aria-disabled',
    );
    for (const option of refusedTypes) {
      expect(await screen.findByRole('option', { name: option })).toHaveAttribute(
        'aria-disabled',
        'true',
      );
    }
  });

  it('does not let the create form leave server_agent on such a server', async () => {
    renderSection({ server: managedServer, apps: [] });
    await screen.findByText(t.runtimeManagedOnlyBanner);
    openCreate();

    await attemptSelectType('ollama');

    // The click was swallowed by the disabled item, so the type never moved
    // off openCreate's managed_runtime_only seed.
    expect(screen.getByRole('combobox', { name: t.applicationType })).toHaveTextContent(
      'server_agent',
    );
  });

  it('says nothing and disables nothing on a server without the flag', async () => {
    renderSection({ apps: [] });
    openCreate();

    const combo = screen.getByRole('combobox', { name: t.applicationType });
    expect(combo).not.toHaveAccessibleDescription();
    fireEvent.mouseDown(combo);
    for (const option of allTypes) {
      expect(await screen.findByRole('option', { name: option })).not.toHaveAttribute(
        'aria-disabled',
      );
    }
  });

  // CO-REACHABILITY of the two reasons, which decides whether composing them
  // is behaviour or dead defence. In the SETTLED state they cannot co-occur:
  // on a managed server with an agent application the create button is not
  // rendered at all, so the create form -- the only place the managed reason
  // applies -- cannot be opened. The one window where both bite is the first
  // fetch: `applications` reads [] while it is in flight and the create button
  // is NOT loading-gated, so the operator can open the form before the list
  // that would have hidden the button arrives. Two agent applications rather
  // than one on purpose: with exactly one, the auto-drill effect fires on that
  // same settle and yanks the form away, so the composed state is real but
  // transient; with two there is no unambiguous drill target and it persists.
  it('states both reasons when the create form was opened during the first fetch', async () => {
    renderSection({
      server: managedServer,
      apps: [
        makeApp({ id: 'app_a', type: 'server_agent' }),
        makeApp({
          id: 'app_b',
          type: 'server_agent',
          port: 9100,
          endpoint: 'https://s1.example.test:9100',
        }),
      ],
    });
    // Synchronous, before the fetch settles -- the list still reads [], so the
    // create button is still on screen.
    openCreate();

    const combo = screen.getByRole('combobox', { name: t.applicationType });
    expect(combo).toHaveAccessibleDescription(t.applicationTypeManagedRuntimeOnly);

    await waitFor(() =>
      expect(combo).toHaveAccessibleDescription(
        `${t.applicationTypeManagedRuntimeOnly} ${t.applicationTypeServerAgentTaken}`,
      ),
    );

    // openCreate's seed is still shown even though its own option is now
    // disabled too -- a closed MUI select computes its text from the matching
    // item regardless of that item's disabled state, so the field is not blank.
    expect(combo).toHaveTextContent('server_agent');

    // The two gates intersect to the empty set: nothing here is choosable, and
    // the backend would refuse every one of the six.
    fireEvent.mouseDown(combo);
    for (const option of allTypes) {
      expect(await screen.findByRole('option', { name: option })).toHaveAttribute(
        'aria-disabled',
        'true',
      );
    }
  });

  it('still renders the 409 when the server DTO was stale, and keeps the form open', async () => {
    // This gate reads `managed_runtime_only` off the server DTO the PARENT
    // fetched; ApplicationSection never refetches the server. A PATCH that
    // sets the flag after that fetch leaves this form offering all six types
    // and the backend is what refuses the write -- rendered here with the flag
    // absent, so the portal gate is open and only the 409 stands.
    const { fakeApi } = renderSection();
    openCreate();
    await selectType('vllm');
    fakeApi.createApplication.mockRejectedValueOnce(
      new PortalApiError(
        409,
        'application.managed_runtime_only',
        'server only accepts agent-managed applications',
      ),
    );

    fireEvent.click(screen.getByRole('button', { name: t.applicationCreate }));

    expect(
      await screen.findByText(
        `application.managed_runtime_only: ${t.errorApplicationManagedRuntimeOnly}`,
      ),
    ).toBeInTheDocument();
    // Form still open, typed data intact -- submitCreate only leaves on success.
    expect(screen.getByRole('combobox', { name: t.applicationType })).toHaveTextContent('vllm');
  });
});

// Part 2: the create button does not merely vanish on such a server, it says
// why. Hidden rather than disabled-with-a-tooltip: a disabled MUI Button is
// removed from the tab order and sets pointer-events:none, so a tooltip on it
// needs a wrapper span and is unreachable by keyboard -- issue #26 again. The
// alert is plain text in the reading order and needs no hover.
describe('ApplicationSection managed_runtime_only create button reason', () => {
  const managedServer: PortalServer = { ...server, managed_runtime_only: true };
  const twoAgents = () => [
    makeApp({ id: 'app_a', type: 'server_agent' }),
    makeApp({
      id: 'app_b',
      type: 'server_agent',
      port: 9100,
      endpoint: 'https://s1.example.test:9100',
    }),
  ];

  it('explains the vanished create button once the server has its agent application', async () => {
    renderSection({ server: managedServer, apps: twoAgents() });

    expect(await screen.findByText(t.runtimeManagedOnlyBanner)).toBeInTheDocument();
    expect(screen.getByText(t.runtimeManagedOnlyCreateBlocked)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: t.applicationCreate })).not.toBeInTheDocument();
  });

  it('stays silent about it while the create button is still offered', async () => {
    renderSection({ server: managedServer, apps: [] });

    expect(await screen.findByText(t.runtimeManagedOnlyBanner)).toBeInTheDocument();
    // An unconditional second sentence would state a restriction that is not
    // in force: this server can still be given its one agent application.
    expect(screen.queryByText(t.runtimeManagedOnlyCreateBlocked)).toBeNull();
    expect(screen.getByRole('button', { name: t.applicationCreate })).toBeInTheDocument();
  });

  // The everyday path to that sentence, end to end: the operator's own create
  // is what makes the button disappear. submitCreate pushes the new row into
  // local state and returns to the list, and the auto-drill has already latched
  // from the first load, so it does not fire on this 0->1 transition -- the
  // operator lands back on a list whose create button is gone, and the alert
  // is the only thing that can tell them why.
  it('explains it the moment the operator has created the one agent application', async () => {
    const { created } = renderSection({ server: managedServer, apps: [] });
    await screen.findByText(t.runtimeManagedOnlyBanner);
    expect(screen.queryByText(t.runtimeManagedOnlyCreateBlocked)).toBeNull();

    openCreate();
    fireEvent.click(screen.getByRole('button', { name: t.applicationCreate }));
    await waitFor(() => expect(created).toHaveLength(1));
    // openCreate seeded the only type this server accepts.
    expect(created[0].type).toBe('server_agent');

    expect(await screen.findByText(t.runtimeManagedOnlyCreateBlocked)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: t.applicationCreate })).not.toBeInTheDocument();
    // Still on the list, not bounced into the runtime admin.
    expect(screen.getByText(t.runtimeManagedOnlyBanner)).toBeInTheDocument();
  });

  it('says neither thing on a server without the flag, agent application or not', async () => {
    renderSection({ apps: [makeApp({ id: 'app_agent', type: 'server_agent' })] });
    await screen.findByText('https://s1.example.test:8000');

    expect(screen.queryByText(t.runtimeManagedOnlyBanner)).toBeNull();
    expect(screen.queryByText(t.runtimeManagedOnlyCreateBlocked)).toBeNull();
    expect(screen.getByRole('button', { name: t.applicationCreate })).toBeInTheDocument();
  });

  // STRUCTURAL, deliberately: it pins the two sentences as separate sibling
  // elements rather than one concatenated string, which is what keeps the
  // pinned findByText(runtimeManagedOnlyBanner) above matching an exact text
  // node (getNodeText reads only an element's DIRECT text children).
  it('keeps the two sentences as separate text nodes inside one alert', async () => {
    renderSection({ server: managedServer, apps: twoAgents() });

    const banner = await screen.findByText(t.runtimeManagedOnlyBanner);
    const blocked = screen.getByText(t.runtimeManagedOnlyCreateBlocked);
    expect(banner).not.toContainElement(blocked);
    expect(banner.parentElement).toBe(blocked.parentElement);
  });
});
