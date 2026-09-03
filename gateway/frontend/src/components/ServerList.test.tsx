// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ServerList } from './ServerList';
import { ToastProvider } from './shared/ToastProvider';
import { messages } from '../i18n';
import { PortalApiError } from '../api';
import type {
  AdminGroupCandidate,
  CreateServerRequest,
  CreateServerResponse,
  PortalApplication,
  PortalModelMapping,
  PortalServer,
  RuntimeSpec,
  ServerHealthStatus,
  SystemSettings as SystemSettingsDTO,
  UpdateServerRequest,
} from '../api';
import type { PortalApi } from './shared/types';
import type { CurrencyUnit } from '../currency';

// A single admin-group candidate under a single system group (Phase B, spec
// 2026-08-10) -- the common case where the create form's picker auto-selects
// with no extra step, so every pre-existing create-flow test (which doesn't
// care about admin-group linkage) keeps the "Server erstellen" action
// enabled unchanged.
const defaultAdminGroupCandidates: AdminGroupCandidate[] = [
  {
    id: 'ag_default',
    name: 'Default Admin Group',
    parent_group_id: 'sg_default',
    parent_group_name: 'Default System Group',
  },
];

// A minimal-but-complete SystemSettingsDTO for the getSystemSettings() fetch the
// per-server policy-override control uses to learn the EFFECTIVE scope.
function makeSystemSettings(overrides: Partial<SystemSettingsDTO> = {}): SystemSettingsDTO {
  return {
    theme: 'default',
    available_themes: [{ id: 'default', name: 'Default' }],
    language: 'de',
    available_languages: ['de'],
    capture_retention_days: 30,
    capture_enabled: true,
    capture_override: false,
    health_check_interval_seconds: 30,
    agent_presence_timeout_seconds: 15,
    smtp_enabled: false,
    smtp_host: '',
    smtp_port: 587,
    smtp_username: '',
    smtp_password_set: false,
    smtp_from: '',
    smtp_from_name: '',
    smtp_tls_mode: 'starttls',
    totp_mode: 'off',
    route_affinity_session_mode: 'client_session',
    vision_probe_mode: 'accept',
    energy_default_price_per_kwh: 0,
    energy_default_pue: 0,
    energy_default_wh_per_token: 0,
    currency_usd_per_eur: 0,
    energy_default_price_unit: 'eur_cent',
    netbird_enabled: true,
    netbird_url: 'https://nb.io',
    netbird_groups: [],
    netbird_token_set: true,
    netbird_only: false,
    netbird_gateway_peer_id: '',
    netbird_gateway_peer_name: '',
    netbird_manage_policies: true,
    netbird_policy_scope: 'auto',
    netbird_effective_policy_scope: 'selected',
    netbird_deny_by_default: false,
    netbird_deny_by_default_enforce: false,
    netbird_peer_sync_interval_seconds: 30,
    netbird_reconcile_interval_seconds: 60,
    netbird_allow_ping_gateway: false,
    netbird_allow_ping_all_servers: false,
    netbird_token_rotate_before_days: 14,
    netbird_agent_download_only: false,
    system_admin_mode_require_password: true,
    resource_provisioning_enforce: false,
    cert_enabled: false,
    cert_issuer_mode: 'acme',
    cert_self_signed_validity_days: 365,
    cert_ca_renew_before_days: 365,
    acme_email: '',
    acme_directory_url: 'https://acme-v02.api.letsencrypt.org/directory',
    cert_base_domain: '',
    cert_gateway_domain: '',
    cert_server_scope: 'selected',
    cert_manage_public_domain: false,
    cert_public_domains: [],
    cert_renew_before_days: 30,
    ...overrides,
  };
}

const t = messages.de;

function makeServer(id: string, health: ServerHealthStatus): PortalServer {
  return {
    id,
    name: `Server ${id}`,
    domain: `${id}.example.test`,
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
    health_status: health,
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
}

// ServerList forwards `api` to seven child sections (ApplicationSection,
// AgentTokenSection, PerformanceSection, AvailabilitySection, HardwareSection,
// ServerResourceGroupsSection, BenchmarkSection), so its narrowed Pick unions
// every one of their method sets -- far more than any single test here
// exercises. Deriving the type from the component itself (rather than
// duplicating its ~49-method Pick literal) keeps this file in sync with
// ServerList.tsx automatically.
type ServerListApi = Parameters<typeof ServerList>[0]['api'];

const idleBenchmarkStatus = {
  running: false,
  server_id: 'srv_1',
  scope: 'server',
  total: 0,
  done: 0,
};

function defaultApplication(overrides: Partial<PortalApplication> = {}): PortalApplication {
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

function defaultMapping(overrides: Partial<PortalModelMapping> = {}): PortalModelMapping {
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

// Agent-runtime-manager (Task 20): a benign "not configured" runtime spec,
// mirroring RuntimeSpec's own zero-value convention (configured: false).
function defaultRuntimeSpec(overrides: Partial<RuntimeSpec> = {}): RuntimeSpec {
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
    api_flavors: [],
    responses_mode: 'passthrough',
    messages_mode: 'passthrough',
    ...overrides,
  };
}

// Complete stub for ServerList's whole narrowed Pick, with benign defaults for
// every method. Most tests below only exercise a handful of these (the list
// view + at most one drill-down section); every other method is never called
// but must still be PRESENT for the mock to structurally satisfy the type, so
// individual tests spread this base and override just the methods they care
// about, rather than re-declaring the full set each time.
function baseServerListApi(): ServerListApi {
  return {
    activeBenchmarks: vi.fn(async () => []),
    adminUsers: vi.fn(async () => ({ data: [] })),
    agentBinaries: vi.fn(async () => ({
      agent_version: '',
      go_version: '',
      built_at: '',
      binaries: [],
      netbird_agent_download_only: false,
      agent_download_base: '',
    })),
    agentPresenceTimeout: vi.fn(async () => ({ seconds: 0 })),
    agentTokenStatus: vi.fn(async () => ({
      exists: false,
      config: { gateway_url: '', ca_file: '', ca_cache_file: '', ca_pem: '' },
      agent_download_base: '',
    })),
    applications: vi.fn(async () => ({ data: [] })),
    benchmarkApplication: vi.fn(async () => idleBenchmarkStatus),
    benchmarkMapping: vi.fn(async () => idleBenchmarkStatus),
    benchmarkServer: vi.fn(async () => idleBenchmarkStatus),
    benchmarkStatus: vi.fn(async () => idleBenchmarkStatus),
    createApplication: vi.fn(async () => defaultApplication()),
    createMapping: vi.fn(async () => defaultMapping()),
    createServer: vi.fn(async () => makeServer('srv_created', 'healthy')),
    deleteApplication: vi.fn(async () => ({ ok: true })),
    deleteMapping: vi.fn(async () => ({ ok: true })),
    deleteRuntimeSpec: vi.fn(async () => ({ ok: true })),
    deleteServer: vi.fn(async () => ({ ok: true })),
    downloadAgentBinary: vi.fn(async () => new Blob()),
    generateAgentToken: vi.fn(async () => ({
      secret: '',
      token: {
        exists: false,
        config: { gateway_url: '', ca_file: '', ca_cache_file: '', ca_pem: '' },
        agent_download_base: '',
      },
    })),
    getCurrency: vi.fn(async () => ({ usd_per_eur: 0 })),
    getSystemSettings: vi.fn(async () => makeSystemSettings()),
    gpuBudgets: vi.fn(async () => ({ budgets: [] })),
    healthCheckInterval: vi.fn(async () => ({ health_check_interval_seconds: 30 })),
    joinResourceGroup: vi.fn(async () => ({ ok: true })),
    leaveResourceGroup: vi.fn(async () => ({ ok: true })),
    mappingBenchmarks: vi.fn(async () => []),
    mappings: vi.fn(async () => ({ data: [] })),
    netbirdEnabled: vi.fn(async () => ({
      enabled: false,
      module_enabled: false,
      netbird_only: false,
      manage_policies: false,
      effective_policy_scope: '',
      deny_by_default: false,
    })),
    netbirdGroups: vi.fn(async () => ({ data: [] })),
    netbirdPeers: vi.fn(async () => ({ data: [] })),
    probeMappingContext: vi.fn(async () => idleBenchmarkStatus),
    probeMappingVram: vi.fn(async () => idleBenchmarkStatus),
    putGpuBudgets: vi.fn(async () => ({ budgets: [] })),
    putRuntimeCoresidency: vi.fn(async () => ({ pairs: [] })),
    putRuntimeSpec: vi.fn(async () => defaultRuntimeSpec()),
    regenerateNetbirdKey: vi.fn(async () => ({ setup_key: '' })),
    revokeAgentToken: vi.fn(async () => ({ ok: true })),
    runtimeCoresidency: vi.fn(async () => ({ pairs: [] })),
    runtimeReport: vi.fn(async () => ({ available: false, agent_version: '', agent_features: [] })),
    runtimeSpec: vi.fn(async () => defaultRuntimeSpec()),
    runtimeWarnings: vi.fn(async () => ({ warnings: [] })),
    serverAdminGroupCandidates: vi.fn(async () => []),
    serverAvailability: vi.fn(async () => ({ points: [], from: '', to: '' })),
    serverHardware: vi.fn(async () => ({ available: false })),
    serverPerfHistory: vi.fn(async () => ({ points: [], from: '', to: '' })),
    serverResourceGroups: vi.fn(async () => []),
    servers: vi.fn(async () => ({ data: [] })),
    setServerAdminGroups: vi.fn(async () => makeServer('srv_1', 'healthy')),
    setServerCertificateOverride: vi.fn(async () => makeServer('srv_1', 'healthy')),
    setServerEnergy: vi.fn(async () => makeServer('srv_1', 'healthy')),
    setServerHTTPSSwitchOverride: vi.fn(async () => makeServer('srv_1', 'healthy')),
    setServerNetbird: vi.fn(async () => makeServer('srv_1', 'healthy')),
    subscribeBenchmark: vi.fn(() => () => {}),
    subscribeRuntimeStatus: vi.fn(() => () => {}),
    subscribeRuntimeLogs: vi.fn(() => () => {}),
    subscribeServerPerf: vi.fn(() => () => {}),
    syncApplicationModels: vi.fn(async () => ({
      added: 0,
      disabled: 0,
      unchanged: 0,
      conflicted: 0,
    })),
    updateApplication: vi.fn(async () => defaultApplication()),
    updateMapping: vi.fn(async () => defaultMapping()),
    updateServer: vi.fn(async () => makeServer('srv_1', 'healthy')),
    usageTimeSeries: vi.fn(async () => ({ points: [], bucket_seconds: 5, from: '', to: '' })),
  };
}

function renderList(servers: PortalServer[]) {
  const fakeApi = {
    ...baseServerListApi(),
    adminUsers: vi.fn(async () => ({ data: [] })),
    // The list view polls the running benchmarks for the live indicator chip.
    activeBenchmarks: vi.fn(async () => []),
  };

  render(
    <ToastProvider>
      <ServerList t={t} api={fakeApi} servers={servers} setServers={vi.fn()} role="user" />
    </ToastProvider>,
  );
}

afterEach(cleanup);

describe('ServerList health chip', () => {
  it("renders the three health states, labelling unhealthy as 'Nicht verfügbar'", () => {
    renderList([
      makeServer('healthy', 'healthy'),
      makeServer('degraded', 'degraded'),
      makeServer('unhealthy', 'unhealthy'),
    ]);

    expect(screen.getByText(t.healthHealthy)).toBeInTheDocument();
    expect(screen.getByText(t.healthDegraded)).toBeInTheDocument();
    // The Unavailable state maps to the `unhealthy` enum; its label is relabelled.
    expect(t.healthUnhealthy).toBe('Nicht verfügbar');
    expect(messages.en.healthUnhealthy).toBe('Unavailable');
    expect(screen.getByText('Nicht verfügbar')).toBeInTheDocument();
  });
});

describe('ServerList Agent column', () => {
  function renderWithAgent(servers: PortalServer[], moduleEnabled = false) {
    const fakeApi = {
      ...baseServerListApi(),
      adminUsers: vi.fn(async () => ({ data: [] })),
      activeBenchmarks: vi.fn(async () => []),
      netbirdEnabled: vi.fn(async () => ({
        enabled: moduleEnabled,
        module_enabled: moduleEnabled,
        netbird_only: false,
        manage_policies: false,
        effective_policy_scope: 'selected',
        deny_by_default: false,
      })),
    };

    render(
      <ToastProvider>
        <ServerList t={t} api={fakeApi} servers={servers} setServers={vi.fn()} role="user" />
      </ToastProvider>,
    );
  }

  it('renders the three agent states (active/inactive/unconfigured)', () => {
    // status "disabled" on every row so the Status column's "Aktiv" label
    // never collides with the Agent column's "Aktiv" label.
    renderWithAgent([
      { ...makeServer('a1', 'healthy'), status: 'disabled', agent_status: 'active' },
      { ...makeServer('a2', 'healthy'), status: 'disabled', agent_status: 'inactive' },
      { ...makeServer('a3', 'healthy'), status: 'disabled', agent_status: 'unconfigured' },
    ]);

    expect(screen.getByText(t.agentStatusActive)).toBeInTheDocument();
    expect(screen.getByText(t.agentStatusInactive)).toBeInTheDocument();
    expect(screen.getByText(t.agentStatusUnconfigured)).toBeInTheDocument();
  });

  it('places the Agent column between Health and NetBird', async () => {
    renderWithAgent(
      [
        {
          ...makeServer('a1', 'healthy'),
          status: 'disabled',
          agent_status: 'active',
          netbird_enabled: true,
        },
      ],
      true,
    );

    // Wait for the NetBird module-enabled fetch to resolve so the netbird
    // column renders too.
    const netbirdCell = await screen.findByText(t.serverNetbirdNotRegistered);
    const healthCell = screen.getByText(t.healthHealthy);
    const agentCell = screen.getByText(t.agentStatusActive);

    expect(
      healthCell.compareDocumentPosition(agentCell) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
    expect(
      agentCell.compareDocumentPosition(netbirdCell) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
  });
});

describe('ServerList Server-ID column', () => {
  // The column-visibility toggle persists to localStorage (usePreference), so clear
  // it between cases to keep each test starting from the hidden-by-default state.
  beforeEach(() => {
    try {
      window.localStorage.clear();
    } catch {
      /* jsdom/private-mode guard */
    }
  });

  it('is optional (hidden by default) and renders the raw server id once enabled', () => {
    renderList([makeServer('srv-xyz', 'healthy')]);
    // Hidden by default: no cell shows the raw id (name shows "Server srv-xyz").
    expect(screen.queryByText('srv-xyz')).not.toBeInTheDocument();

    // Enable it via the column-visibility menu (target the checkbox unambiguously).
    fireEvent.click(screen.getByRole('button', { name: t.listColumns }));
    fireEvent.click(screen.getByRole('checkbox', { name: t.tableServerId }));

    // Now the raw id renders as its own cell.
    expect(screen.getByText('srv-xyz')).toBeInTheDocument();
  });

  it('places the Server-ID column first (leads the row) when shown', () => {
    renderList([makeServer('srv-abc', 'healthy')]);
    fireEvent.click(screen.getByRole('button', { name: t.listColumns }));
    fireEvent.click(screen.getByRole('checkbox', { name: t.tableServerId }));

    // In the data row the id cell ("srv-abc") precedes the name cell ("Server
    // srv-abc") in document order — i.e. Server-ID is the first column. (Exact-text
    // queries: the id/name values are distinct and never appear in the menu.)
    const idCell = screen.getByText('srv-abc');
    const nameCell = screen.getByText('Server srv-abc');
    expect(
      idCell.compareDocumentPosition(nameCell) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
  });
});

describe('ServerList path suffix', () => {
  it('renders the server path suffix field and submits it (trimmed) on create', async () => {
    const createServer = vi.fn(
      async (body: CreateServerRequest) =>
        ({ ...makeServer('new', 'healthy'), ...body }) as CreateServerResponse,
    );
    const fakeApi = {
      ...baseServerListApi(),
      adminUsers: vi.fn(async () => ({ data: [] })),
      activeBenchmarks: vi.fn(async () => []),
      serverAdminGroupCandidates: vi.fn(async () => defaultAdminGroupCandidates),
      createServer,
    };

    render(
      <ToastProvider>
        <ServerList t={t} api={fakeApi} servers={[]} setServers={vi.fn()} role="admin" />
      </ToastProvider>,
    );

    // Enter the create sub-view (the page-level create action).
    fireEvent.click(screen.getByRole('button', { name: t.serverCreate }));
    fireEvent.change(screen.getByLabelText(t.serverNameLabel), { target: { value: 'srv' } });
    fireEvent.change(screen.getByLabelText(t.serverDomainLabel), {
      target: { value: 'd.example.test' },
    });
    // The path-suffix field renders after the domain field.
    fireEvent.change(screen.getByLabelText(t.serverPathSuffixLabel), {
      target: { value: '  /api  ' },
    });
    // Let the admin-group candidates fetch resolve (single candidate ->
    // auto-selected, no picker) before submitting.
    await screen.findByText(t.serverAdminGroupAuto('Default Admin Group'));

    // The mask's submit button reuses the create label; only one exists here.
    fireEvent.click(screen.getByRole('button', { name: t.serverCreate }));
    await waitFor(() => expect(createServer).toHaveBeenCalledTimes(1));
    // Submitted trimmed in the request body.
    expect(createServer.mock.calls[0][0].server_path_suffix).toBe('/api');
  });
});

describe('ServerList energy config', () => {
  // price_per_kwh is stored canonically in EUR; the price field displays/accepts
  // it converted into the current price_unit (default "eur_cent"), so entering
  // "32" (32 cents) must submit price_per_kwh: 0.32 — never the raw typed number.
  it('renders the four energy-config fields and submits them on create (price normalized to EUR + its unit)', async () => {
    const createServer = vi.fn(
      async (body: CreateServerRequest) =>
        ({ ...makeServer('new', 'healthy'), ...body }) as CreateServerResponse,
    );
    const fakeApi = {
      ...baseServerListApi(),
      adminUsers: vi.fn(async () => ({ data: [] })),
      activeBenchmarks: vi.fn(async () => []),
      getCurrency: vi.fn(async () => ({ usd_per_eur: 0 })),
      serverAdminGroupCandidates: vi.fn(async () => defaultAdminGroupCandidates),
      createServer,
    };

    render(
      <ToastProvider>
        <ServerList t={t} api={fakeApi} servers={[]} setServers={vi.fn()} role="admin" />
      </ToastProvider>,
    );

    fireEvent.click(screen.getByRole('button', { name: t.serverCreate }));
    fireEvent.change(screen.getByLabelText(t.serverNameLabel), { target: { value: 'srv' } });
    fireEvent.change(screen.getByLabelText(t.serverDomainLabel), {
      target: { value: 'd.example.test' },
    });
    fireEvent.change(screen.getByLabelText(t.serverEstimatedWatts), { target: { value: '350' } });
    fireEvent.change(screen.getByLabelText(t.serverIdleWatts), { target: { value: '40' } });
    fireEvent.change(screen.getByLabelText(t.serverPricePerKwh), { target: { value: '32' } });
    fireEvent.change(screen.getByLabelText(t.serverPue), { target: { value: '1.4' } });
    // Let the admin-group candidates fetch resolve before submitting.
    await screen.findByText(t.serverAdminGroupAuto('Default Admin Group'));

    fireEvent.click(screen.getByRole('button', { name: t.serverCreate }));
    await waitFor(() => expect(createServer).toHaveBeenCalledTimes(1));
    const body = createServer.mock.calls[0][0];
    expect(body.estimated_watts).toBe(350);
    expect(body.idle_watts).toBe(40);
    expect(body.price_per_kwh).toBe(0.32);
    expect(body.price_unit).toBe('eur_cent');
    expect(body.pue).toBe(1.4);
  });

  it('blank energy-config fields submit as 0 on create', async () => {
    const createServer = vi.fn(
      async (body: CreateServerRequest) =>
        ({ ...makeServer('new', 'healthy'), ...body }) as CreateServerResponse,
    );
    const fakeApi = {
      ...baseServerListApi(),
      adminUsers: vi.fn(async () => ({ data: [] })),
      activeBenchmarks: vi.fn(async () => []),
      getCurrency: vi.fn(async () => ({ usd_per_eur: 0 })),
      serverAdminGroupCandidates: vi.fn(async () => defaultAdminGroupCandidates),
      createServer,
    };

    render(
      <ToastProvider>
        <ServerList t={t} api={fakeApi} servers={[]} setServers={vi.fn()} role="admin" />
      </ToastProvider>,
    );

    fireEvent.click(screen.getByRole('button', { name: t.serverCreate }));
    fireEvent.change(screen.getByLabelText(t.serverNameLabel), { target: { value: 'srv' } });
    fireEvent.change(screen.getByLabelText(t.serverDomainLabel), {
      target: { value: 'd.example.test' },
    });
    // Let the admin-group candidates fetch resolve before submitting.
    await screen.findByText(t.serverAdminGroupAuto('Default Admin Group'));

    fireEvent.click(screen.getByRole('button', { name: t.serverCreate }));
    await waitFor(() => expect(createServer).toHaveBeenCalledTimes(1));
    const body = createServer.mock.calls[0][0];
    expect(body.estimated_watts).toBe(0);
    expect(body.idle_watts).toBe(0);
    expect(body.price_per_kwh).toBe(0);
    expect(body.pue).toBe(0);
  });

  // 0.3 EUR/kWh, stored with price_unit "eur_cent", must pre-fill as "30" (its
  // display unit), not "0.3" (the raw EUR number).
  it('pre-fills the energy-config fields from the server row on edit (price shown in its unit)', async () => {
    const server = {
      ...makeServer('srv-e', 'healthy'),
      estimated_watts: 500,
      idle_watts: 60,
      price_per_kwh: 0.3,
      pue: 1.6,
      price_unit: 'eur_cent' as const,
    };
    const fakeApi = {
      ...baseServerListApi(),
      adminUsers: vi.fn(async () => ({ data: [] })),
      activeBenchmarks: vi.fn(async () => []),
      getCurrency: vi.fn(async () => ({ usd_per_eur: 0 })),
    };

    render(
      <ToastProvider>
        <ServerList t={t} api={fakeApi} servers={[server]} setServers={vi.fn()} role="admin" />
      </ToastProvider>,
    );

    fireEvent.click(screen.getByRole('button', { name: t.serverActionEdit }));
    expect(screen.getByLabelText(t.serverEstimatedWatts)).toHaveValue(500);
    expect(screen.getByLabelText(t.serverIdleWatts)).toHaveValue(60);
    expect(screen.getByLabelText(t.serverPricePerKwh)).toHaveValue(30);
    expect(screen.getByLabelText(t.serverPue)).toHaveValue(1.6);
  });

  it('saves energy-config via its own button (dedicated endpoint, not the main form; price normalized to EUR + its unit)', async () => {
    const server = {
      ...makeServer('srv-e', 'healthy'),
      estimated_watts: 500,
      idle_watts: 60,
      price_per_kwh: 0.45,
      pue: 1.6,
      price_unit: 'eur_cent' as const,
    };
    const setServerEnergy = vi.fn(
      async (
        id: string,
        est: number,
        idle: number,
        price: number,
        pue: number,
        priceUnit: CurrencyUnit,
      ) => ({
        ...server,
        estimated_watts: est,
        idle_watts: idle,
        price_per_kwh: price,
        pue: pue,
        price_unit: priceUnit,
      }),
    );
    const updateServer = vi.fn();
    const fakeApi = {
      ...baseServerListApi(),
      adminUsers: vi.fn(async () => ({ data: [] })),
      activeBenchmarks: vi.fn(async () => []),
      getCurrency: vi.fn(async () => ({ usd_per_eur: 1.1 })),
      setServerEnergy,
      updateServer,
    };

    render(
      <ToastProvider>
        <ServerList t={t} api={fakeApi} servers={[server]} setServers={vi.fn()} role="admin" />
      </ToastProvider>,
    );

    fireEvent.click(screen.getByRole('button', { name: t.serverActionEdit }));
    fireEvent.change(screen.getByLabelText(t.serverEstimatedWatts), { target: { value: '250' } });
    // 30 eur_cent == 0.3 EUR — must be normalized before hitting setServerEnergy.
    fireEvent.change(screen.getByLabelText(t.serverPricePerKwh), { target: { value: '30' } });
    fireEvent.click(screen.getByRole('button', { name: t.serverEnergySave }));

    await waitFor(() => expect(setServerEnergy).toHaveBeenCalledTimes(1));
    expect(setServerEnergy).toHaveBeenCalledWith('srv-e', 250, 60, 0.3, 1.6, 'eur_cent');
    // The energy save is independent of the main server PATCH.
    expect(updateServer).not.toHaveBeenCalled();
  });

  // The price-field invariant: switching the unit re-displays the SAME underlying
  // price in the new unit — it must never reinterpret the raw typed number.
  it('re-displays the same underlying price after switching units, without reinterpreting the typed number', async () => {
    const server = {
      ...makeServer('srv-e', 'healthy'),
      price_per_kwh: 0,
      price_unit: 'eur_cent' as const,
    };
    const fakeApi = {
      ...baseServerListApi(),
      adminUsers: vi.fn(async () => ({ data: [] })),
      activeBenchmarks: vi.fn(async () => []),
      getCurrency: vi.fn(async () => ({ usd_per_eur: 0 })),
    };

    render(
      <ToastProvider>
        <ServerList t={t} api={fakeApi} servers={[server]} setServers={vi.fn()} role="admin" />
      </ToastProvider>,
    );

    fireEvent.click(screen.getByRole('button', { name: t.serverActionEdit }));
    const priceField = screen.getByLabelText(t.serverPricePerKwh) as HTMLInputElement;
    fireEvent.change(priceField, { target: { value: '30' } });

    const unitField = screen.getByLabelText(t.priceUnitLabel);
    fireEvent.mouseDown(unitField);
    fireEvent.click(await screen.findByRole('option', { name: t.currencyUnitEur }));

    // 30 eur_cent == 0.3 EUR -> switching to "eur" must re-display "0.3", not "30".
    expect(priceField.value).toBe('0.3');
  });

  // Review finding (MEDIUM, data-loss): a stored USD price unit with the
  // conversion factor at 0 (USD unavailable) must degrade to eur_cent for
  // display/save — never show/save 0 (fromEur(x,"usd",0)===0) and never leave
  // the unit Select out of its own option set.
  it('degrades a stored USD price unit to eur_cent when the conversion factor is 0 (no data loss)', async () => {
    const server = {
      ...makeServer('srv-usd', 'healthy'),
      estimated_watts: 500,
      idle_watts: 60,
      price_per_kwh: 0.3,
      pue: 1.6,
      price_unit: 'usd' as const,
    };
    const setServerEnergy = vi.fn(
      async (
        id: string,
        est: number,
        idle: number,
        price: number,
        pue: number,
        priceUnit: CurrencyUnit,
      ) => ({
        ...server,
        estimated_watts: est,
        idle_watts: idle,
        price_per_kwh: price,
        pue: pue,
        price_unit: priceUnit,
      }),
    );
    const updateServer = vi.fn();
    const fakeApi = {
      ...baseServerListApi(),
      adminUsers: vi.fn(async () => ({ data: [] })),
      activeBenchmarks: vi.fn(async () => []),
      getCurrency: vi.fn(async () => ({ usd_per_eur: 0 })),
      setServerEnergy,
      updateServer,
    };

    render(
      <ToastProvider>
        <ServerList t={t} api={fakeApi} servers={[server]} setServers={vi.fn()} role="admin" />
      </ToastProvider>,
    );

    fireEvent.click(screen.getByRole('button', { name: t.serverActionEdit }));

    // The stored unit is "usd", but the factor is 0 -> the field must show the
    // eur_cent-converted price (30), never 0 (which fromEur(0.3,"usd",0) would give).
    const priceField = (await screen.findByLabelText(t.serverPricePerKwh)) as HTMLInputElement;
    expect(priceField.value).toBe('30');

    // The unit Select must show the degraded eur_cent option, never a blank/
    // out-of-range "usd" (availableUnits(0) excludes USD entirely).
    const unitField = screen.getByRole('combobox', { name: t.priceUnitLabel });
    expect(unitField).toHaveTextContent(t.currencyUnitEurCent);

    fireEvent.click(screen.getByRole('button', { name: t.serverEnergySave }));

    await waitFor(() => expect(setServerEnergy).toHaveBeenCalledTimes(1));
    // Must round-trip the original 0.3 EUR (via eur_cent "30"), never 0 and
    // never the raw stored "usd" unit.
    expect(setServerEnergy).toHaveBeenCalledWith('srv-usd', 500, 60, 0.3, 1.6, 'eur_cent');
  });
});

describe('ServerList admin-group picker (Phase B, spec 2026-08-10)', () => {
  function renderCreate(
    candidates: AdminGroupCandidate[],
    createServer?: PortalApi['createServer'],
  ) {
    const create =
      createServer ??
      vi.fn(
        async (body: CreateServerRequest) =>
          ({ ...makeServer('new', 'healthy'), ...body }) as CreateServerResponse,
      );
    const fakeApi = {
      ...baseServerListApi(),
      adminUsers: vi.fn(async () => ({ data: [] })),
      activeBenchmarks: vi.fn(async () => []),
      serverAdminGroupCandidates: vi.fn(async () => candidates),
      createServer: create,
    };
    render(
      <ToastProvider>
        <ServerList t={t} api={fakeApi} servers={[]} setServers={vi.fn()} role="admin" />
      </ToastProvider>,
    );
    return { createServer: create as ReturnType<typeof vi.fn> };
  }

  it('auto-selects the single admin-group candidate (no field) and submits its id', async () => {
    const candidates: AdminGroupCandidate[] = [
      { id: 'ag_1', name: 'Ops Admins', parent_group_id: 'sg_1', parent_group_name: 'Ops System' },
    ];
    const { createServer } = renderCreate(candidates);

    fireEvent.click(screen.getByRole('button', { name: t.serverCreate }));
    // The auto note names the single candidate + derives the system group;
    // no picker of any kind renders.
    await screen.findByText(t.serverAdminGroupAuto('Ops Admins'));
    expect(screen.getByText(t.serverAdminGroupSystemGroupAuto('Ops System'))).toBeInTheDocument();
    expect(screen.queryByLabelText(t.serverAdminGroupLabel)).not.toBeInTheDocument();
    expect(screen.queryByLabelText(t.serverAdminGroupSystemGroupLabel)).not.toBeInTheDocument();

    fireEvent.change(screen.getByLabelText(t.serverNameLabel), { target: { value: 'srv' } });
    fireEvent.change(screen.getByLabelText(t.serverDomainLabel), {
      target: { value: 'd.example.test' },
    });
    fireEvent.click(screen.getByRole('button', { name: t.serverCreate }));
    await waitFor(() => expect(createServer).toHaveBeenCalledTimes(1));
    expect(createServer.mock.calls[0][0].admin_group_ids).toEqual(['ag_1']);
  });

  it('shows a required multi-select when there are several candidates under ONE system group, and submits the chosen ids', async () => {
    const candidates: AdminGroupCandidate[] = [
      { id: 'ag_a', name: 'Group A', parent_group_id: 'sg_1', parent_group_name: 'Ops System' },
      { id: 'ag_b', name: 'Group B', parent_group_id: 'sg_1', parent_group_name: 'Ops System' },
    ];
    const { createServer } = renderCreate(candidates);

    fireEvent.click(screen.getByRole('button', { name: t.serverCreate }));
    // A single shared parent -> no system-group step, just the derived note.
    await screen.findByText(t.serverAdminGroupSystemGroupAuto('Ops System'));
    expect(screen.queryByLabelText(t.serverAdminGroupSystemGroupLabel)).not.toBeInTheDocument();
    fireEvent.change(screen.getByLabelText(t.serverNameLabel), { target: { value: 'srv' } });
    fireEvent.change(screen.getByLabelText(t.serverDomainLabel), {
      target: { value: 'd.example.test' },
    });

    // Nothing picked yet -> submit stays disabled.
    expect(screen.getByRole('button', { name: t.serverCreate })).toBeDisabled();

    fireEvent.mouseDown(screen.getByLabelText(t.serverAdminGroupLabel));
    fireEvent.click(await screen.findByRole('option', { name: 'Group B' }));

    fireEvent.click(screen.getByRole('button', { name: t.serverCreate }));
    await waitFor(() => expect(createServer).toHaveBeenCalledTimes(1));
    expect(createServer.mock.calls[0][0].admin_group_ids).toEqual(['ag_b']);
  });

  it('requires a system-group choice first when candidates span MORE THAN ONE parent, then narrows the admin-group picker to its children', async () => {
    const candidates: AdminGroupCandidate[] = [
      { id: 'ag_a', name: 'Group A', parent_group_id: 'sg_1', parent_group_name: 'System One' },
      { id: 'ag_b', name: 'Group B', parent_group_id: 'sg_1', parent_group_name: 'System One' },
      { id: 'ag_c', name: 'Group C', parent_group_id: 'sg_2', parent_group_name: 'System Two' },
    ];
    const { createServer } = renderCreate(candidates);

    fireEvent.click(screen.getByRole('button', { name: t.serverCreate }));
    await screen.findByLabelText(t.serverAdminGroupSystemGroupLabel);
    // No admin-group picker of any kind before a system group is chosen.
    expect(screen.queryByLabelText(t.serverAdminGroupLabel)).not.toBeInTheDocument();
    expect(screen.queryByText(t.serverAdminGroupAuto('Group C'))).not.toBeInTheDocument();

    fireEvent.mouseDown(screen.getByLabelText(t.serverAdminGroupSystemGroupLabel));
    fireEvent.click(await screen.findByRole('option', { name: 'System Two' }));

    // Narrowed to System Two's single child -> auto-selected.
    await screen.findByText(t.serverAdminGroupAuto('Group C'));
    expect(screen.queryByLabelText(t.serverAdminGroupLabel)).not.toBeInTheDocument();
    expect(screen.queryByText('Group A')).not.toBeInTheDocument();

    fireEvent.change(screen.getByLabelText(t.serverNameLabel), { target: { value: 'srv' } });
    fireEvent.change(screen.getByLabelText(t.serverDomainLabel), {
      target: { value: 'd.example.test' },
    });
    fireEvent.click(screen.getByRole('button', { name: t.serverCreate }));
    await waitFor(() => expect(createServer).toHaveBeenCalledTimes(1));
    expect(createServer.mock.calls[0][0].admin_group_ids).toEqual(['ag_c']);
  });

  it('shows a hint and keeps the submit action disabled when the caller has no admin-group candidate', async () => {
    const createServer = vi.fn(
      async (body: CreateServerRequest) =>
        ({ ...makeServer('new', 'healthy'), ...body }) as CreateServerResponse,
    );
    renderCreate([], createServer as unknown as PortalApi['createServer']);

    fireEvent.click(screen.getByRole('button', { name: t.serverCreate }));
    await screen.findByText(t.serverNoAdminGroupHint);
    fireEvent.change(screen.getByLabelText(t.serverNameLabel), { target: { value: 'srv' } });
    fireEvent.change(screen.getByLabelText(t.serverDomainLabel), {
      target: { value: 'd.example.test' },
    });

    expect(screen.getByRole('button', { name: t.serverCreate })).toBeDisabled();
    fireEvent.click(screen.getByRole('button', { name: t.serverCreate }));
    expect(createServer).not.toHaveBeenCalled();
  });
});

describe('ServerList edit-form admin-groups editor (Phase B, spec 2026-08-10)', () => {
  it("pre-fills the server's linked groups and saves the edited set via its own button", async () => {
    const server = {
      ...makeServer('srv-ag', 'healthy'),
      admin_groups: [{ id: 'ag_a', name: 'Group A' }],
      system_group_id: 'sg_1',
      system_group_name: 'Ops System',
    };
    const candidates: AdminGroupCandidate[] = [
      { id: 'ag_a', name: 'Group A', parent_group_id: 'sg_1', parent_group_name: 'Ops System' },
      { id: 'ag_b', name: 'Group B', parent_group_id: 'sg_1', parent_group_name: 'Ops System' },
      // A candidate under a DIFFERENT system group must NOT be offered here.
      {
        id: 'ag_other',
        name: "Other System's Group",
        parent_group_id: 'sg_2',
        parent_group_name: 'Other System',
      },
    ];
    const setServerAdminGroups = vi.fn(async () => ({
      ...server,
      admin_groups: [{ id: 'ag_b', name: 'Group B' }],
    }));
    const fakeApi = {
      ...baseServerListApi(),
      adminUsers: vi.fn(async () => ({ data: [] })),
      activeBenchmarks: vi.fn(async () => []),
      serverAdminGroupCandidates: vi.fn(async () => candidates),
      setServerAdminGroups,
    };
    render(
      <ToastProvider>
        <ServerList t={t} api={fakeApi} servers={[server]} setServers={vi.fn()} role="admin" />
      </ToastProvider>,
    );

    fireEvent.click(screen.getByRole('button', { name: t.serverActionEdit }));
    await screen.findByText(t.serverAdminGroupsSectionTitle);
    // Pre-filled with the server's own linked group.
    expect(screen.getByText('Group A')).toBeInTheDocument();
    expect(screen.queryByText("Other System's Group")).not.toBeInTheDocument();

    fireEvent.mouseDown(screen.getByLabelText(t.serverAdminGroupLabel));
    fireEvent.click(await screen.findByRole('option', { name: 'Group B' }));
    fireEvent.click(screen.getByRole('button', { name: t.serverAdminGroupsSave }));

    await waitFor(() =>
      expect(setServerAdminGroups).toHaveBeenCalledWith('srv-ag', ['ag_a', 'ag_b']),
    );
    expect(await screen.findByText(t.serverAdminGroupsSaved)).toBeInTheDocument();
  });

  it('disables the save action once the last linked group would be removed', async () => {
    const server = {
      ...makeServer('srv-ag2', 'healthy'),
      admin_groups: [{ id: 'ag_a', name: 'Group A' }],
      system_group_id: 'sg_1',
      system_group_name: 'Ops System',
    };
    const candidates: AdminGroupCandidate[] = [
      { id: 'ag_a', name: 'Group A', parent_group_id: 'sg_1', parent_group_name: 'Ops System' },
    ];
    const fakeApi = {
      ...baseServerListApi(),
      adminUsers: vi.fn(async () => ({ data: [] })),
      activeBenchmarks: vi.fn(async () => []),
      serverAdminGroupCandidates: vi.fn(async () => candidates),
    };
    render(
      <ToastProvider>
        <ServerList t={t} api={fakeApi} servers={[server]} setServers={vi.fn()} role="admin" />
      </ToastProvider>,
    );

    fireEvent.click(screen.getByRole('button', { name: t.serverActionEdit }));
    await screen.findByText(t.serverAdminGroupsSectionTitle);
    expect(screen.getByRole('button', { name: t.serverAdminGroupsSave })).toBeEnabled();

    // Remove the only chip (its own delete affordance) -> Save disables.
    const chipDelete = screen.getByText('Group A').parentElement!.querySelector('svg')!;
    fireEvent.click(chipDelete);
    expect(screen.getByRole('button', { name: t.serverAdminGroupsSave })).toBeDisabled();
  });

  // Migration recovery (2026-08-10 follow-up): a server created before Phase B
  // has no containment root (system_group_id==""). The edit editor must offer
  // the create-style choose-a-system-group flow so a system_admin can SET the
  // root — the pre-fix code filtered options to the (empty) root's children and
  // offered nothing, so the root could never be assigned.
  it('offers the create-style flow for an ungrouped (migrated) server and sets its root on save', async () => {
    const server = {
      ...makeServer('srv-ungrouped', 'healthy'),
      admin_groups: [],
      system_group_id: '',
      system_group_name: '',
    };
    const candidates: AdminGroupCandidate[] = [
      { id: 'ag_a', name: 'Group A', parent_group_id: 'sg_1', parent_group_name: 'System One' },
      { id: 'ag_b', name: 'Group B', parent_group_id: 'sg_1', parent_group_name: 'System One' },
      { id: 'ag_c', name: 'Group C', parent_group_id: 'sg_2', parent_group_name: 'System Two' },
    ];
    const setServerAdminGroups = vi.fn(async () => ({
      ...server,
      admin_groups: [{ id: 'ag_c', name: 'Group C' }],
      system_group_id: 'sg_2',
      system_group_name: 'System Two',
    }));
    const fakeApi = {
      ...baseServerListApi(),
      adminUsers: vi.fn(async () => ({ data: [] })),
      activeBenchmarks: vi.fn(async () => []),
      serverAdminGroupCandidates: vi.fn(async () => candidates),
      setServerAdminGroups,
    };
    render(
      <ToastProvider>
        <ServerList t={t} api={fakeApi} servers={[server]} setServers={vi.fn()} role="admin" />
      </ToastProvider>,
    );

    fireEvent.click(screen.getByRole('button', { name: t.serverActionEdit }));
    await screen.findByText(t.serverAdminGroupsSectionTitle);
    // Candidates span two system groups -> the system-group step appears first,
    // and no admin-group picker until one is chosen. Save stays disabled.
    await screen.findByLabelText(t.serverAdminGroupSystemGroupLabel);
    expect(screen.queryByLabelText(t.serverAdminGroupLabel)).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: t.serverAdminGroupsSave })).toBeDisabled();

    fireEvent.mouseDown(screen.getByLabelText(t.serverAdminGroupSystemGroupLabel));
    fireEvent.click(await screen.findByRole('option', { name: 'System Two' }));

    // Narrowed to System Two's single child -> auto-selected, Save enabled.
    await screen.findByText(t.serverAdminGroupAuto('Group C'));
    expect(screen.getByRole('button', { name: t.serverAdminGroupsSave })).toBeEnabled();

    fireEvent.click(screen.getByRole('button', { name: t.serverAdminGroupsSave }));
    await waitFor(() =>
      expect(setServerAdminGroups).toHaveBeenCalledWith('srv-ungrouped', ['ag_c']),
    );
    expect(await screen.findByText(t.serverAdminGroupsSaved)).toBeInTheDocument();
  });

  it('auto-selects the lone candidate for an ungrouped server under a single system group', async () => {
    const server = {
      ...makeServer('srv-ungrouped2', 'healthy'),
      admin_groups: [],
      system_group_id: '',
      system_group_name: '',
    };
    const candidates: AdminGroupCandidate[] = [
      { id: 'ag_1', name: 'Ops Admins', parent_group_id: 'sg_1', parent_group_name: 'Ops System' },
    ];
    const setServerAdminGroups = vi.fn(async () => ({
      ...server,
      admin_groups: [{ id: 'ag_1', name: 'Ops Admins' }],
      system_group_id: 'sg_1',
      system_group_name: 'Ops System',
    }));
    const fakeApi = {
      ...baseServerListApi(),
      adminUsers: vi.fn(async () => ({ data: [] })),
      activeBenchmarks: vi.fn(async () => []),
      serverAdminGroupCandidates: vi.fn(async () => candidates),
      setServerAdminGroups,
    };
    render(
      <ToastProvider>
        <ServerList t={t} api={fakeApi} servers={[server]} setServers={vi.fn()} role="admin" />
      </ToastProvider>,
    );

    fireEvent.click(screen.getByRole('button', { name: t.serverActionEdit }));
    await screen.findByText(t.serverAdminGroupsSectionTitle);
    // One system group, one admin group -> both derived as text, no picker,
    // Save enabled off the auto-selected id.
    await screen.findByText(t.serverAdminGroupAuto('Ops Admins'));
    expect(screen.getByText(t.serverAdminGroupSystemGroupAuto('Ops System'))).toBeInTheDocument();
    expect(screen.queryByLabelText(t.serverAdminGroupLabel)).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: t.serverAdminGroupsSave })).toBeEnabled();

    fireEvent.click(screen.getByRole('button', { name: t.serverAdminGroupsSave }));
    await waitFor(() =>
      expect(setServerAdminGroups).toHaveBeenCalledWith('srv-ungrouped2', ['ag_1']),
    );
  });
});

describe('ServerList performance sub-view', () => {
  it("opens the Performance sub-view from the 'Leistung' row action", async () => {
    const stop = vi.fn();
    const subscribeServerPerf = vi.fn(() => stop);
    const serverPerfHistory = vi.fn(async () => ({ points: [], from: '', to: '' }));
    const usageTimeSeries = vi.fn(async () => ({
      points: [],
      bucket_seconds: 0,
      from: '',
      to: '',
    }));
    const fakeApi = {
      ...baseServerListApi(),
      adminUsers: vi.fn(async () => ({ data: [] })),
      activeBenchmarks: vi.fn(async () => []),
      serverPerfHistory,
      usageTimeSeries,
      subscribeServerPerf,
    };

    render(
      <ToastProvider>
        <ServerList
          t={t}
          api={fakeApi}
          servers={[makeServer('perf', 'healthy')]}
          setServers={vi.fn()}
          role="user"
        />
      </ToastProvider>,
    );

    // The server row renders its actions as inline icon buttons (maxInline 5);
    // the "Leistung" action is a directly-labelled button.
    fireEvent.click(screen.getByRole('button', { name: t.serverPerformance }));

    // The sub-view mounted: the breadcrumb carries the server name, the empty
    // state renders (no history + the server never reported), the section fetched
    // history for the default 15m window, and it subscribed to the per-server SSE.
    const crumbs = await screen.findByRole('navigation', { name: t.breadcrumb });
    expect(within(crumbs).getByText('Server perf')).toBeInTheDocument();
    expect(await screen.findByText(t.serverPerfNoAgent)).toBeInTheDocument();
    expect(serverPerfHistory).toHaveBeenCalledWith('perf', '15m');
    expect(subscribeServerPerf).toHaveBeenCalled();
  });
});

describe('ServerList benchmark', () => {
  // The column-visibility toggle persists to localStorage; clear it so the
  // (default-visible) benchmark-running column starts shown each case.
  beforeEach(() => {
    try {
      window.localStorage.clear();
    } catch {
      /* jsdom/private-mode guard */
    }
  });

  it('opens the benchmark area from the row action instead of starting a run', async () => {
    const benchmarkServer = vi.fn(async () => ({
      running: false,
      server_id: 'bench',
      scope: 'server',
      total: 0,
      done: 0,
    }));
    const fakeApi = {
      ...baseServerListApi(),
      adminUsers: vi.fn(async () => ({ data: [] })),
      activeBenchmarks: vi.fn(async () => []),
      benchmarkServer,
      // BenchmarkSection mounts on navigation.
      subscribeBenchmark: vi.fn(() => () => {}),
      applications: vi.fn(async () => ({ data: [] })),
      mappings: vi.fn(async () => ({ data: [] })),
      mappingBenchmarks: vi.fn(async () => []),
    };

    render(
      <ToastProvider>
        <ServerList
          t={t}
          api={fakeApi}
          servers={[makeServer('bench', 'healthy')]}
          setServers={vi.fn()}
          role="user"
        />
      </ToastProvider>,
    );

    // The removed capacity action must be gone; only one "Benchmark" action remains.
    expect(screen.queryByRole('button', { name: t.runCapacityBenchmark })).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: t.runBenchmark }));

    // It navigates to the consolidated area (never starts a run): the section
    // heading + the benchmarkArea breadcrumb crumb render, and no run was POSTed.
    expect(
      await screen.findByRole('heading', { level: 2, name: new RegExp(t.benchmarkArea) }),
    ).toBeInTheDocument();
    const crumbs = await screen.findByRole('navigation', { name: t.breadcrumb });
    expect(within(crumbs).getByText(t.benchmarkArea)).toBeInTheDocument();
    expect(benchmarkServer).not.toHaveBeenCalled();
  });

  it('renders the live running chip for a server with an active benchmark', async () => {
    const activeBenchmarks = vi.fn(async () => [
      { running: true, server_id: 'srv-run', scope: 'server', total: 3, done: 1 },
    ]);
    const fakeApi = {
      ...baseServerListApi(),
      adminUsers: vi.fn(async () => ({ data: [] })),
      activeBenchmarks,
    };

    render(
      <ToastProvider>
        <ServerList
          t={t}
          api={fakeApi}
          servers={[makeServer('srv-run', 'healthy'), makeServer('srv-idle', 'healthy')]}
          setServers={vi.fn()}
          role="user"
        />
      </ToastProvider>,
    );

    // The 3s poll runs immediately on mount; the chip shows the running server's
    // done/total. The idle server has no chip (its cell stays empty).
    expect(await screen.findByText(`${t.benchmarkRunning} (1/3)`)).toBeInTheDocument();
    expect(activeBenchmarks).toHaveBeenCalled();
  });
});

describe('ServerList NetBird', () => {
  // The netbird column + row action are default-visible; clear the persisted
  // column visibility so each case starts clean.
  beforeEach(() => {
    try {
      window.localStorage.clear();
    } catch {
      /* jsdom/private-mode guard */
    }
  });

  function nbServer(id: string, over: Partial<PortalServer>): PortalServer {
    return { ...makeServer(id, 'healthy'), netbird_enabled: true, ...over };
  }

  function renderNb(opts: {
    moduleEnabled: boolean;
    netbirdOnly?: boolean;
    managePolicies?: boolean;
    effectivePolicyScope?: string;
    denyByDefault?: boolean;
    servers?: PortalServer[];
    isSystemAdmin?: boolean;
    createServer?: PortalApi['createServer'];
    regenerateNetbirdKey?: PortalApi['regenerateNetbirdKey'];
    setServerNetbird?: PortalApi['setServerNetbird'];
    netbirdPeers?: PortalApi['netbirdPeers'];
    netbirdGroups?: PortalApi['netbirdGroups'];
    deleteServer?: PortalApi['deleteServer'];
    getSystemSettings?: PortalApi['getSystemSettings'];
    serverAdminGroupCandidates?: PortalApi['serverAdminGroupCandidates'];
  }) {
    // The module gate is now the portal-scoped boolean flag (works for a normal
    // admin), not the system-scoped settings fetch. It also carries the
    // policy-management state (manage/scope/deny) so the create-form override
    // control is role/scope/deny aware for a normal admin too.
    const netbirdEnabled = vi.fn(async () => ({
      enabled: opts.moduleEnabled,
      module_enabled: opts.moduleEnabled,
      netbird_only: opts.netbirdOnly ?? false,
      manage_policies: opts.managePolicies ?? false,
      effective_policy_scope: opts.effectivePolicyScope ?? 'selected',
      deny_by_default: opts.denyByDefault ?? false,
    }));
    const createServer =
      opts.createServer ??
      vi.fn(
        async (body: CreateServerRequest) =>
          ({ ...makeServer('new', 'healthy'), ...body }) as CreateServerResponse,
      );
    const regenerateNetbirdKey =
      opts.regenerateNetbirdKey ?? vi.fn(async () => ({ setup_key: '' }));
    const setServerNetbird =
      opts.setServerNetbird ?? vi.fn(async () => makeServer('new', 'healthy'));
    const netbirdPeers = opts.netbirdPeers ?? vi.fn(async () => ({ data: [] }));
    const netbirdGroups = opts.netbirdGroups ?? vi.fn(async () => ({ data: [] }));
    const deleteServer = opts.deleteServer ?? vi.fn(async () => ({ ok: true }));
    // Backs the per-server policy-override control's effective-scope fetch
    // (system-admin only). Defaults to "selected" scope; a test can override to
    // "all" or make it fail (hiding the control).
    const getSystemSettings = opts.getSystemSettings ?? vi.fn(async () => makeSystemSettings());
    // Phase B, spec 2026-08-10: a single candidate under a single system group
    // (the common case) so the create form's admin-group picker auto-selects
    // (no extra step) and every pre-existing create-flow test below keeps
    // submitting unimpeded; a test can override to exercise the multi-
    // candidate / no-candidate states.
    const serverAdminGroupCandidates =
      opts.serverAdminGroupCandidates ?? vi.fn(async () => defaultAdminGroupCandidates);
    const setServers = vi.fn();
    const fakeApi = {
      ...baseServerListApi(),
      adminUsers: vi.fn(async () => ({ data: [] })),
      activeBenchmarks: vi.fn(async () => []),
      netbirdEnabled,
      createServer,
      regenerateNetbirdKey,
      setServerNetbird,
      netbirdPeers,
      getSystemSettings,
      netbirdGroups,
      deleteServer,
      serverAdminGroupCandidates,
      // Enroll refreshes the rows afterwards (best-effort).
      servers: vi.fn(async () => ({ data: opts.servers ?? [] })),
    };
    render(
      <ToastProvider>
        <ServerList
          t={t}
          api={fakeApi}
          servers={opts.servers ?? []}
          setServers={setServers}
          role="admin"
          isSystemAdmin={opts.isSystemAdmin ?? false}
        />
      </ToastProvider>,
    );
    return {
      fakeApi,
      createServer,
      regenerateNetbirdKey,
      setServerNetbird,
      netbirdPeers,
      deleteServer,
      setServers,
      getSystemSettings,
    };
  }

  // Opens the create form only once the netbirdEnabled() fetch's state has been
  // COMMITTED -- which "it has been called" does not imply, and which every
  // assertion below about the create form's INITIAL state depends on.
  //
  // openCreate SNAPSHOTS netbird_only, manage_policies, the effective scope and
  // deny-by-default into the form's own state (setNetbirdChecked,
  // setCreatePolicyOverride) at click time. A click dispatched before that
  // fetch's setStates are committed therefore runs the PRE-fetch render's
  // closure, where netbird_only is still false: the box opens unchecked and the
  // override falls to ''. No later commit repairs either -- openCreate never
  // runs again -- so a `waitFor` placed AFTER the click cannot rescue it; it can
  // only burn its timeout and fail.
  //
  // `waitFor(() => expect(fakeApi.netbirdEnabled).toHaveBeenCalled())` was not a
  // synchronisation point for any of that: the call happens at mount, one
  // microtask before the fake resolves and one macrotask before React's
  // Scheduler commits the update, while waitFor returns the moment its callback
  // passes. The two race. Measured on a reproduction that changes nothing but
  // the clock React's scheduler reads -- +6 ms per reading, so every unit of
  // work blows the 5 ms frame budget and the concurrent render yields and
  // re-schedules, which is what CPU load does to it -- six tests in this file
  // fail, the SYSTEM-admin case at `expect(checkbox.checked).toBe(true)`.
  //
  // The list's NetBird column renders on netbirdModuleEnabled, which lands in
  // the SAME batched commit as the other four values, so its header is a
  // committed-state proxy for all of them and strictly stronger than the call
  // count (it also proves the response said enabled). It exists only under
  // moduleEnabled: true; the module-disabled cases have no positive marker to
  // wait for and keep the call-count wait.
  async function openCreateOnceNetbirdFlagsCommitted() {
    await screen.findByRole('columnheader', { name: t.settingsNetbirdTitle });
    fireEvent.click(screen.getByRole('button', { name: t.serverCreate }));
  }

  it('hides the create checkbox when the module is disabled', async () => {
    const { fakeApi } = renderNb({ moduleEnabled: false });
    await waitFor(() => expect(fakeApi.netbirdEnabled).toHaveBeenCalled());
    fireEvent.click(screen.getByRole('button', { name: t.serverCreate }));
    // The create form renders (name field present) but no NetBird checkbox.
    expect(screen.getByLabelText(t.serverNameLabel)).toBeInTheDocument();
    expect(screen.queryByLabelText(t.serverNetbirdEnable)).not.toBeInTheDocument();
  });

  it('shows the checkbox when enabled and locks the domain when checked', async () => {
    renderNb({ moduleEnabled: true });
    fireEvent.click(screen.getByRole('button', { name: t.serverCreate }));
    const checkbox = await screen.findByLabelText(t.serverNetbirdEnable);
    const domain = screen.getByLabelText(t.serverDomainLabel) as HTMLInputElement;
    expect(domain).not.toBeDisabled();
    fireEvent.click(checkbox);
    expect(domain).toBeDisabled();
    expect(screen.getByText(t.serverNetbirdDomainAuto)).toBeInTheDocument();
  });

  it('reveals the display-once setup key returned by create', async () => {
    const createServer = vi.fn(async (body: CreateServerRequest) => ({
      ...makeServer('new', 'healthy'),
      ...body,
      netbird_setup_key: 'SK-secret-123',
    }));
    renderNb({
      moduleEnabled: true,
      createServer: createServer as unknown as PortalApi['createServer'],
    });
    fireEvent.click(screen.getByRole('button', { name: t.serverCreate }));
    fireEvent.change(screen.getByLabelText(t.serverNameLabel), { target: { value: 'srv' } });
    fireEvent.click(await screen.findByLabelText(t.serverNetbirdEnable));
    // Submit (only the mask's create button exists now).
    fireEvent.click(screen.getByRole('button', { name: t.serverCreate }));
    await waitFor(() => expect(createServer).toHaveBeenCalledTimes(1));
    expect(createServer.mock.calls[0][0].netbird_enabled).toBe(true);
    expect(await screen.findByText('SK-secret-123')).toBeInTheDocument();
  });

  it('forces + locks the create checkbox for a NORMAL admin when netbird_only is on', async () => {
    const createServer = vi.fn(
      async (body: CreateServerRequest) =>
        ({ ...makeServer('new', 'healthy'), ...body }) as CreateServerResponse,
    );
    renderNb({
      moduleEnabled: true,
      netbirdOnly: true,
      isSystemAdmin: false,
      createServer: createServer as unknown as PortalApi['createServer'],
    });
    await openCreateOnceNetbirdFlagsCommitted();
    const checkbox = (await screen.findByLabelText(t.serverNetbirdEnable)) as HTMLInputElement;
    expect(checkbox.checked).toBe(true);
    expect(checkbox).toBeDisabled();
    expect(screen.getByText(t.serverNetbirdOnlyForcedNote)).toBeInTheDocument();
    // The precheck warning is system-admin-only, so it must NOT show here.
    expect(screen.queryByText(t.serverNetbirdOnlyPrecheckWarning)).not.toBeInTheDocument();
    fireEvent.change(screen.getByLabelText(t.serverNameLabel), { target: { value: 'srv' } });
    fireEvent.click(screen.getByRole('button', { name: t.serverCreate }));
    await waitFor(() => expect(createServer).toHaveBeenCalledTimes(1));
    expect(createServer.mock.calls[0][0]).toMatchObject({ netbird_enabled: true });
  });

  it('pre-selects but keeps the create checkbox editable for a SYSTEM admin when netbird_only is on', async () => {
    const createServer = vi.fn(
      async (body: CreateServerRequest) =>
        ({ ...makeServer('new', 'healthy'), ...body }) as CreateServerResponse,
    );
    renderNb({
      moduleEnabled: true,
      netbirdOnly: true,
      isSystemAdmin: true,
      createServer: createServer as unknown as PortalApi['createServer'],
    });
    await openCreateOnceNetbirdFlagsCommitted();
    const checkbox = (await screen.findByLabelText(t.serverNetbirdEnable)) as HTMLInputElement;
    expect(checkbox.checked).toBe(true);
    expect(checkbox).not.toBeDisabled();
    expect(screen.getByText(t.serverNetbirdOnlyPrecheckWarning)).toBeInTheDocument();
    // Uncheck it → the server is created OUTSIDE the NetBird network (needs a domain).
    fireEvent.click(checkbox);
    fireEvent.change(screen.getByLabelText(t.serverNameLabel), { target: { value: 'srv' } });
    fireEvent.change(screen.getByLabelText(t.serverDomainLabel), {
      target: { value: 'srv.example' },
    });
    fireEvent.click(screen.getByRole('button', { name: t.serverCreate }));
    await waitFor(() => expect(createServer).toHaveBeenCalledTimes(1));
    expect(createServer.mock.calls[0][0].netbird_enabled).toBeUndefined();
  });

  it('leaves the create checkbox unchecked + editable + note-free when netbird_only is off', async () => {
    renderNb({ moduleEnabled: true, netbirdOnly: false });
    await openCreateOnceNetbirdFlagsCommitted();
    const checkbox = (await screen.findByLabelText(t.serverNetbirdEnable)) as HTMLInputElement;
    expect(checkbox.checked).toBe(false);
    expect(checkbox).not.toBeDisabled();
    expect(screen.queryByText(t.serverNetbirdOnlyForcedNote)).not.toBeInTheDocument();
    expect(screen.queryByText(t.serverNetbirdOnlyPrecheckWarning)).not.toBeInTheDocument();
  });

  it("renders the tri-state connection indicator on each server's OWN row", async () => {
    renderNb({
      moduleEnabled: true,
      servers: [
        nbServer('nb-notreg', { netbird_peer_id: '', netbird_connected: false }),
        nbServer('nb-conn', { netbird_peer_id: 'peer-1', netbird_connected: true }),
        nbServer('nb-disc', { netbird_peer_id: 'peer-2', netbird_connected: false }),
      ],
    });
    // Wait for the netbird column to appear (the module setting resolves async).
    await screen.findByText(t.serverNetbirdConnected);

    // Scope each assertion to the row of a specific server (by its unique name), so
    // an inverted connected↔disconnected mapping would put the label on the wrong
    // row and fail here — an unbound getByText would pass either way.
    const rowOf = (name: string) => screen.getByText(name).closest('tr') as HTMLElement;
    expect(
      within(rowOf('Server nb-notreg')).getByText(t.serverNetbirdNotRegistered),
    ).toBeInTheDocument();
    expect(within(rowOf('Server nb-conn')).getByText(t.serverNetbirdConnected)).toBeInTheDocument();
    expect(
      within(rowOf('Server nb-disc')).getByText(t.serverNetbirdDisconnected),
    ).toBeInTheDocument();
  });

  it('regenerates a setup key from the row action (confirm → reveal)', async () => {
    const regenerateNetbirdKey = vi.fn(async () => ({ setup_key: 'SK-regen-9' }));
    renderNb({
      moduleEnabled: true,
      // A gateway-managed peer, so the regenerate action is enabled (see gate test).
      servers: [
        nbServer('nb-1', {
          netbird_peer_id: 'peer-1',
          netbird_connected: true,
          netbird_peer_managed: true,
        }),
      ],
      regenerateNetbirdKey,
    });
    // With 8 actions on a NetBird server the row collapses into a kebab menu.
    fireEvent.click(await screen.findByRole('button', { name: t.listRowMenu }));
    fireEvent.click(await screen.findByRole('menuitem', { name: t.serverNetbirdRegenerate }));
    // Confirm the destructive action, then the display-once reveal shows the key.
    fireEvent.click(await screen.findByRole('button', { name: t.serverNetbirdRegenerate }));
    await waitFor(() => expect(regenerateNetbirdKey).toHaveBeenCalledWith('nb-1'));
    expect(await screen.findByText('SK-regen-9')).toBeInTheDocument();
  });

  it('shows the ENROLL action for a NON-netbird server when the module is enabled and reveals the key', async () => {
    const regenerateNetbirdKey = vi.fn(async () => ({ setup_key: 'SK-enroll-1' }));
    // A plain (non-netbird) server; the module gate resolves async → 8 actions → kebab.
    renderNb({
      moduleEnabled: true,
      servers: [makeServer('plain', 'healthy')],
      regenerateNetbirdKey,
    });
    // The enroll action is labelled by state (non-netbird → "NetBird-Enrollment").
    fireEvent.click(await screen.findByRole('button', { name: t.listRowMenu }));
    fireEvent.click(await screen.findByRole('menuitem', { name: t.serverNetbirdEnroll }));
    // Confirm dialog title/button reflect enroll, then the reveal shows the key.
    fireEvent.click(await screen.findByRole('button', { name: t.serverNetbirdEnroll }));
    await waitFor(() => expect(regenerateNetbirdKey).toHaveBeenCalledWith('plain'));
    expect(await screen.findByText('SK-enroll-1')).toBeInTheDocument();
  });

  it('renders the system-admin linkage editor in the edit form and saves the edited peer id', async () => {
    const server = nbServer('nb-link', {
      netbird_peer_id: 'old-peer',
      netbird_group_id: 'grp-1',
      netbird_setup_key_id: 'key-1',
      netbird_connected: true,
    });
    const setServerNetbird = vi.fn(async () => ({ ...server, netbird_peer_id: 'new-peer' }));
    renderNb({ moduleEnabled: true, isSystemAdmin: true, servers: [server], setServerNetbird });
    // Open the edit form (kebab → edit).
    fireEvent.click(await screen.findByRole('button', { name: t.listRowMenu }));
    fireEvent.click(await screen.findByRole('menuitem', { name: t.serverActionEdit }));
    // The linkage section renders with the peer id prefilled from the server.
    const peer = (await screen.findByLabelText(t.serverNetbirdPeerId)) as HTMLInputElement;
    expect(peer.value).toBe('old-peer');
    fireEvent.change(peer, { target: { value: 'new-peer' } });
    fireEvent.click(screen.getByRole('button', { name: t.serverNetbirdLinkSave }));
    // The group multiselect (pre-filled from netbird_group_ids = []) sends [] ids.
    await waitFor(() =>
      expect(setServerNetbird).toHaveBeenCalledWith(
        'nb-link',
        true,
        'new-peer',
        [],
        false,
        '',
        false,
        false,
      ),
    );
  });

  it('renders the per-server ping-allow checkbox and sends its value on save', async () => {
    const server = nbServer('nb-ping', { netbird_peer_id: 'peer-1', netbird_allow_ping: false });
    const setServerNetbird = vi.fn(async () => ({ ...server, netbird_allow_ping: true }));
    renderNb({ moduleEnabled: true, isSystemAdmin: true, servers: [server], setServerNetbird });
    fireEvent.click(await screen.findByRole('button', { name: t.listRowMenu }));
    fireEvent.click(await screen.findByRole('menuitem', { name: t.serverActionEdit }));
    // The checkbox renders (pre-filled false) and toggling it sends true on save.
    const allowPing = (await screen.findByLabelText(t.serverNetbirdAllowPing)) as HTMLInputElement;
    expect(allowPing).not.toBeChecked();
    fireEvent.click(allowPing);
    expect(allowPing).toBeChecked();
    fireEvent.click(screen.getByRole('button', { name: t.serverNetbirdLinkSave }));
    await waitFor(() =>
      expect(setServerNetbird).toHaveBeenCalledWith(
        'nb-ping',
        true,
        'peer-1',
        [],
        false,
        '',
        true,
        false,
      ),
    );
  });

  it("shows the RED ping opt-out when 'Alle Server pingbar' is on and threads pingExclude=true (clearing allow) on save", async () => {
    // Pre-set netbird_allow_ping true to prove checking the opt-out clears it (mutual exclusivity).
    const server = nbServer('nb-ping-excl', {
      netbird_peer_id: 'peer-1',
      netbird_allow_ping: true,
    });
    const getSystemSettings = vi.fn(async () =>
      makeSystemSettings({ netbird_allow_ping_all_servers: true }),
    );
    const setServerNetbird = vi.fn(async () => ({ ...server, netbird_ping_exclude: true }));
    renderNb({
      moduleEnabled: true,
      isSystemAdmin: true,
      servers: [server],
      setServerNetbird,
      getSystemSettings,
    });
    fireEvent.click(await screen.findByRole('button', { name: t.listRowMenu }));
    fireEvent.click(await screen.findByRole('menuitem', { name: t.serverActionEdit }));
    // The RED opt-out renders (awaits the pingAllServers flip); its Checkbox root carries the error class.
    const box = (await screen.findByRole('checkbox', {
      name: t.serverNetbirdPingExclude,
    })) as HTMLInputElement;
    const root = box.closest('.MuiCheckbox-root');
    expect(root).not.toBeNull();
    expect(root?.classList.contains('MuiCheckbox-colorError')).toBe(true);
    // The opt-in ("Ping erlauben") text is NOT shown in opt-out mode.
    expect(screen.queryByText(t.serverNetbirdAllowPing)).not.toBeInTheDocument();
    // Checking the opt-out flips pingExclude=true AND clears allow-ping (mutual exclusivity)
    // → the trailing two save args are allowPing=false, pingExclude=true.
    fireEvent.click(box);
    fireEvent.click(screen.getByRole('button', { name: t.serverNetbirdLinkSave }));
    await waitFor(() =>
      expect(setServerNetbird).toHaveBeenCalledWith(
        'nb-ping-excl',
        true,
        'peer-1',
        [],
        false,
        '',
        false,
        true,
      ),
    );
  });

  it("shows the opt-IN ping control when 'Alle Server pingbar' is off and threads pingExclude=false on save", async () => {
    const server = nbServer('nb-ping-in', { netbird_peer_id: 'peer-1', netbird_allow_ping: false });
    const getSystemSettings = vi.fn(async () =>
      makeSystemSettings({ netbird_allow_ping_all_servers: false }),
    );
    const setServerNetbird = vi.fn(async () => ({ ...server, netbird_allow_ping: true }));
    renderNb({
      moduleEnabled: true,
      isSystemAdmin: true,
      servers: [server],
      setServerNetbird,
      getSystemSettings,
    });
    fireEvent.click(await screen.findByRole('button', { name: t.listRowMenu }));
    fireEvent.click(await screen.findByRole('menuitem', { name: t.serverActionEdit }));
    // The opt-in renders (no red); the RED opt-out text never appears.
    const allowPing = (await screen.findByLabelText(t.serverNetbirdAllowPing)) as HTMLInputElement;
    expect(screen.queryByText(t.serverNetbirdPingExclude)).not.toBeInTheDocument();
    // Checking the opt-in flips allowPing=true AND clears pingExclude (mutual exclusivity)
    // → the trailing two save args are allowPing=true, pingExclude=false.
    fireEvent.click(allowPing);
    fireEvent.click(screen.getByRole('button', { name: t.serverNetbirdLinkSave }));
    await waitFor(() =>
      expect(setServerNetbird).toHaveBeenCalledWith(
        'nb-ping-in',
        true,
        'peer-1',
        [],
        false,
        '',
        true,
        false,
      ),
    );
  });

  it('pre-fills the group multiselect from netbird_group_ids, disables it when the peer is unenrolled, and sends the ids on save', async () => {
    // A server WITH a policy-group mirror + an enrolled peer → the group chip is
    // pre-filled and the multiselect is enabled; saving forwards the group ids.
    const enrolled = nbServer('nb-groups', {
      netbird_peer_id: 'peer-e',
      netbird_group_ids: [{ id: 'g-A', name: 'alpha' }],
    });
    const setServerNetbird = vi.fn(async () => enrolled);
    const netbirdGroups = vi.fn(async () => ({
      data: [
        { id: 'g-A', name: 'alpha' },
        { id: 'g-B', name: 'beta' },
      ],
    }));
    const { setServerNetbird: sn1 } = renderNb({
      moduleEnabled: true,
      isSystemAdmin: true,
      servers: [enrolled],
      setServerNetbird,
      netbirdGroups,
    });
    fireEvent.click(await screen.findByRole('button', { name: t.listRowMenu }));
    fireEvent.click(await screen.findByRole('menuitem', { name: t.serverActionEdit }));
    // The pre-filled group name is shown as a selected chip.
    expect(await screen.findByText('alpha')).toBeInTheDocument();
    // Enabled (not the unenrolled note).
    expect(screen.queryByText(t.serverNetbirdGroupsUnenrolled)).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: t.serverNetbirdLinkSave }));
    await waitFor(() =>
      expect(sn1).toHaveBeenCalledWith(
        'nb-groups',
        true,
        'peer-e',
        ['g-A'],
        false,
        '',
        false,
        false,
      ),
    );
    cleanup();

    // A server WITHOUT an enrolled peer → the multiselect is disabled + the note shows.
    const unenrolled = nbServer('nb-unenrolled', { netbird_peer_id: '' });
    renderNb({ moduleEnabled: true, isSystemAdmin: true, servers: [unenrolled] });
    fireEvent.click(await screen.findByRole('button', { name: t.listRowMenu }));
    fireEvent.click(await screen.findByRole('menuitem', { name: t.serverActionEdit }));
    expect(await screen.findByText(t.serverNetbirdGroupsUnenrolled)).toBeInTheDocument();
    const groupInput = screen.getByLabelText(t.serverNetbirdGroups) as HTMLInputElement;
    expect(groupInput.disabled).toBe(true);
  });

  it('hides the linkage editor for a non-system-admin', async () => {
    const server = nbServer('nb-nolink', {
      netbird_peer_id: 'p',
      netbird_group_id: 'g',
      netbird_setup_key_id: 'k',
    });
    renderNb({ moduleEnabled: true, isSystemAdmin: false, servers: [server] });
    fireEvent.click(await screen.findByRole('button', { name: t.listRowMenu }));
    fireEvent.click(await screen.findByRole('menuitem', { name: t.serverActionEdit }));
    // The main edit form renders (name field) but the linkage section does not.
    expect(await screen.findByLabelText(t.serverNameLabel)).toBeInTheDocument();
    expect(screen.queryByText(t.serverNetbirdLinkTitle)).not.toBeInTheDocument();
    expect(screen.queryByLabelText(t.serverNetbirdPeerId)).not.toBeInTheDocument();
  });

  async function openLinkageEditor(rowName?: string) {
    // Scope the kebab to a specific row when the list has more than one server
    // (an unscoped findByRole would match every row's menu button).
    const scope = rowName ? within(screen.getByText(rowName).closest('tr') as HTMLElement) : screen;
    fireEvent.click(await scope.findByRole('button', { name: t.listRowMenu }));
    fireEvent.click(await screen.findByRole('menuitem', { name: t.serverActionEdit }));
    // The peer picker renders after the async netbirdPeers() load resolves.
    return (await screen.findByLabelText(t.serverNetbirdPeerPick)) as HTMLInputElement;
  }

  it('loads the peer picker and fills the peer-id field (+ domain hint) when a peer is selected', async () => {
    const server = nbServer('nb-link', { netbird_peer_id: '' });
    const netbirdPeers = vi.fn(async () => ({
      data: [
        { id: 'peer-alpha', name: 'Alpha', dns_label: 'alpha.netbird.io', connected: true },
        { id: 'peer-beta', name: 'Beta', dns_label: 'beta.netbird.io', connected: false },
      ],
    }));
    renderNb({ moduleEnabled: true, isSystemAdmin: true, servers: [server], netbirdPeers });

    const pick = await openLinkageEditor();
    await waitFor(() => expect(netbirdPeers).toHaveBeenCalled());
    // Type to open + filter, then pick the Alpha peer.
    fireEvent.change(pick, { target: { value: 'Alpha' } });
    fireEvent.click(await screen.findByRole('option', { name: /Alpha/ }));

    // Selecting fills the editable peer-id field (the source of truth for save).
    const peer = screen.getByLabelText(t.serverNetbirdPeerId) as HTMLInputElement;
    await waitFor(() => expect(peer.value).toBe('peer-alpha'));
    // The dns_label surfaces as the "Domain wird: …" hint.
    expect(screen.getByText(t.serverNetbirdPeerDomainHint('alpha.netbird.io'))).toBeInTheDocument();
  });

  it('marks a peer already linked to ANOTHER server as disabled/annotated', async () => {
    const edited = nbServer('nb-link', { netbird_peer_id: '' });
    // A second server already owns peer-beta → it must be disabled in the picker.
    const other = nbServer('nb-other', { netbird_peer_id: 'peer-beta' });
    const netbirdPeers = vi.fn(async () => ({
      data: [
        { id: 'peer-alpha', name: 'Alpha', dns_label: 'alpha.netbird.io', connected: true },
        { id: 'peer-beta', name: 'Beta', dns_label: 'beta.netbird.io', connected: false },
      ],
    }));
    renderNb({ moduleEnabled: true, isSystemAdmin: true, servers: [edited, other], netbirdPeers });

    const pick = await openLinkageEditor('Server nb-link');
    fireEvent.change(pick, { target: { value: 'Beta' } });
    const betaOption = await screen.findByRole('option', { name: /Beta/ });
    expect(betaOption).toHaveAttribute('aria-disabled', 'true');
    expect(betaOption.textContent).toContain(t.serverNetbirdPeerLinked);
  });

  it("does NOT mark the EDITED server's OWN peer as already-linked (current-server exclusion)", async () => {
    // The edited server already owns "p-self"; a DIFFERENT server owns "p-other".
    // Both peers are also offered by netbirdPeers(). `linkedElsewhere` excludes the
    // edited server (`s.id !== editServer.id`), so p-self must stay selectable while
    // p-other (linked elsewhere) is disabled/annotated. Removing that exclusion from
    // production disables/annotates p-self too and fails this test.
    const edited = nbServer('nb-link', { netbird_peer_id: 'p-self' });
    const other = nbServer('nb-other', { netbird_peer_id: 'p-other' });
    const netbirdPeers = vi.fn(async () => ({
      data: [
        { id: 'p-self', name: 'SelfPeer', dns_label: 'self.netbird.io', connected: true },
        { id: 'p-other', name: 'OtherPeer', dns_label: 'other.netbird.io', connected: false },
      ],
    }));
    renderNb({ moduleEnabled: true, isSystemAdmin: true, servers: [edited, other], netbirdPeers });

    const pick = await openLinkageEditor('Server nb-link');
    // The picker is pre-selected to p-self (the edited server's own peer), so open
    // the popup without changing the input — MUI then lists ALL options (typing while
    // a value is set would filter to the selected option only).
    fireEvent.mouseDown(pick);
    fireEvent.keyDown(pick, { key: 'ArrowDown' });
    await screen.findByRole('option', { name: /SelfPeer/ });
    const options = screen.getAllByRole('option');
    const selfOption = options.find((o) => /SelfPeer/.test(o.textContent ?? '')) as HTMLElement;
    const otherOption = options.find((o) => /OtherPeer/.test(o.textContent ?? '')) as HTMLElement;
    expect(selfOption).toBeTruthy();
    expect(otherOption).toBeTruthy();

    // p-self belongs to the CURRENT server → NOT disabled and NOT "already linked".
    expect(selfOption).not.toHaveAttribute('aria-disabled', 'true');
    expect(selfOption.textContent).not.toContain(t.serverNetbirdPeerLinked);

    // Contrast: p-other belongs to ANOTHER server → disabled + annotated.
    expect(otherOption).toHaveAttribute('aria-disabled', 'true');
    expect(otherOption.textContent).toContain(t.serverNetbirdPeerLinked);
  });

  it('hides the peer picker but keeps the manual peer-id field usable when netbirdPeers() rejects', async () => {
    const server = nbServer('nb-link', { netbird_peer_id: '' });
    const netbirdPeers = vi.fn(async () => {
      throw new Error('peers load failed');
    });
    const setServerNetbird = vi.fn(async () => ({ ...server, netbird_peer_id: 'manual-peer' }));
    renderNb({
      moduleEnabled: true,
      isSystemAdmin: true,
      servers: [server],
      netbirdPeers: netbirdPeers as unknown as PortalApi['netbirdPeers'],
      setServerNetbird,
    });

    // Open the edit form (kebab → edit); don't use openLinkageEditor — the picker
    // never appears on a load error, so its label wait would hang.
    fireEvent.click(await screen.findByRole('button', { name: t.listRowMenu }));
    fireEvent.click(await screen.findByRole('menuitem', { name: t.serverActionEdit }));

    // The linkage section still renders (no crash), and once the peers load rejects
    // the dropdown is hidden. Forcing the dropdown to render on error fails this.
    expect(await screen.findByText(t.serverNetbirdLinkTitle)).toBeInTheDocument();
    await waitFor(() => expect(netbirdPeers).toHaveBeenCalled());
    await waitFor(() =>
      expect(screen.queryByLabelText(t.serverNetbirdPeerPick)).not.toBeInTheDocument(),
    );

    // The manual peer-id field stays present and drives a successful save.
    const peer = screen.getByLabelText(t.serverNetbirdPeerId) as HTMLInputElement;
    expect(peer).toBeInTheDocument();
    fireEvent.change(peer, { target: { value: 'manual-peer' } });
    fireEvent.click(screen.getByRole('button', { name: t.serverNetbirdLinkSave }));
    await waitFor(() =>
      expect(setServerNetbird).toHaveBeenCalledWith(
        'nb-link',
        true,
        'manual-peer',
        [],
        false,
        '',
        false,
        false,
      ),
    );
  });

  it('shows a specific toast and keeps the editor open on a peer-in-use (409) save', async () => {
    const server = nbServer('nb-link', { netbird_peer_id: '' });
    const setServerNetbird = vi.fn(async () => {
      throw new PortalApiError(409, 'netbird.peer_in_use', 'peer in use');
    });
    renderNb({ moduleEnabled: true, isSystemAdmin: true, servers: [server], setServerNetbird });

    await openLinkageEditor();
    // Type a peer id into the manual field, then attempt to save.
    const peer = screen.getByLabelText(t.serverNetbirdPeerId) as HTMLInputElement;
    fireEvent.change(peer, { target: { value: 'peer-dup' } });
    fireEvent.click(screen.getByRole('button', { name: t.serverNetbirdLinkSave }));

    await waitFor(() =>
      expect(setServerNetbird).toHaveBeenCalledWith(
        'nb-link',
        true,
        'peer-dup',
        [],
        false,
        '',
        false,
        false,
      ),
    );
    // The specific in-use toast shows, and the editor stays open (save button present).
    expect(await screen.findByText(t.serverNetbirdPeerInUse)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: t.serverNetbirdLinkSave })).toBeInTheDocument();
  });

  // --- Part D: one-peer "gateway-managed" flag + gated regenerate action ---

  it('pre-fills the managed checkbox (checked) and threads its toggled-off value to setServerNetbird', async () => {
    const managed = nbServer('nb-managed', {
      netbird_peer_id: 'peer-m',
      netbird_peer_managed: true,
    });
    const setServerNetbird = vi.fn(async () => managed);
    renderNb({ moduleEnabled: true, isSystemAdmin: true, servers: [managed], setServerNetbird });
    fireEvent.click(await screen.findByRole('button', { name: t.listRowMenu }));
    fireEvent.click(await screen.findByRole('menuitem', { name: t.serverActionEdit }));
    const box = (await screen.findByRole('checkbox', {
      name: t.serverNetbirdPeerManagedLabel,
    })) as HTMLInputElement;
    expect(box.checked).toBe(true);
    fireEvent.click(box); // uncheck → managed=false on save
    fireEvent.click(screen.getByRole('button', { name: t.serverNetbirdLinkSave }));
    await waitFor(() =>
      expect(setServerNetbird).toHaveBeenCalledWith(
        'nb-managed',
        true,
        'peer-m',
        [],
        false,
        '',
        false,
        false,
      ),
    );
  });

  it('pre-fills the managed checkbox (unchecked) and sends managed=true when toggled on', async () => {
    const unmanaged = nbServer('nb-unmanaged', {
      netbird_peer_id: 'peer-u',
      netbird_peer_managed: false,
    });
    const setServerNetbird = vi.fn(async () => unmanaged);
    renderNb({ moduleEnabled: true, isSystemAdmin: true, servers: [unmanaged], setServerNetbird });
    fireEvent.click(await screen.findByRole('button', { name: t.listRowMenu }));
    fireEvent.click(await screen.findByRole('menuitem', { name: t.serverActionEdit }));
    const box = (await screen.findByRole('checkbox', {
      name: t.serverNetbirdPeerManagedLabel,
    })) as HTMLInputElement;
    expect(box.checked).toBe(false);
    fireEvent.click(box); // check → managed=true on save
    fireEvent.click(screen.getByRole('button', { name: t.serverNetbirdLinkSave }));
    await waitFor(() =>
      expect(setServerNetbird).toHaveBeenCalledWith(
        'nb-unmanaged',
        true,
        'peer-u',
        [],
        true,
        '',
        false,
        false,
      ),
    );
  });

  it('DISABLES the regenerate action for a non-managed server that already has a peer', async () => {
    // peer set + managed=false → generating a key could delete a foreign peer, so it
    // is gated. Dropping the WHOLE gate would enable it here and fail this test.
    renderNb({
      moduleEnabled: true,
      servers: [nbServer('nb-foreign', { netbird_peer_id: 'peer-x', netbird_peer_managed: false })],
    });
    fireEvent.click(await screen.findByRole('button', { name: t.listRowMenu }));
    const item = await screen.findByRole('menuitem', { name: t.serverNetbirdRegenerate });
    expect(item).toHaveAttribute('aria-disabled', 'true');
  });

  it('ENABLES the regenerate action for a gateway-managed server with a peer', async () => {
    renderNb({
      moduleEnabled: true,
      servers: [nbServer('nb-managed2', { netbird_peer_id: 'peer-y', netbird_peer_managed: true })],
    });
    fireEvent.click(await screen.findByRole('button', { name: t.listRowMenu }));
    const item = await screen.findByRole('menuitem', { name: t.serverNetbirdRegenerate });
    expect(item).not.toHaveAttribute('aria-disabled', 'true');
  });

  it('KEEPS the enroll action enabled for a fresh server with no peer (regardless of managed)', async () => {
    // peer_id==="" → always allowed (nothing to delete). Dropping `|| peer_id===""`
    // from the gate would disable this fresh server and fail here.
    renderNb({ moduleEnabled: true, servers: [makeServer('fresh', 'healthy')] });
    fireEvent.click(await screen.findByRole('button', { name: t.listRowMenu }));
    const item = await screen.findByRole('menuitem', { name: t.serverNetbirdEnroll });
    expect(item).not.toHaveAttribute('aria-disabled', 'true');
  });

  it('shows the peer-not-managed toast on a 409 from the regenerate call', async () => {
    // The UI gates the action, but the backend is authoritative (409 backstop). Drive
    // the reject via a managed server (so the action is triggerable) and assert the toast.
    const regenerateNetbirdKey = vi.fn(async () => {
      throw new PortalApiError(409, 'netbird.peer_not_managed', 'not managed');
    });
    renderNb({
      moduleEnabled: true,
      servers: [nbServer('nb-1', { netbird_peer_id: 'peer-1', netbird_peer_managed: true })],
      regenerateNetbirdKey: regenerateNetbirdKey as unknown as PortalApi['regenerateNetbirdKey'],
    });
    fireEvent.click(await screen.findByRole('button', { name: t.listRowMenu }));
    fireEvent.click(await screen.findByRole('menuitem', { name: t.serverNetbirdRegenerate }));
    fireEvent.click(await screen.findByRole('button', { name: t.serverNetbirdRegenerate }));
    await waitFor(() => expect(regenerateNetbirdKey).toHaveBeenCalledWith('nb-1'));
    expect(await screen.findByText(t.serverNetbirdPeerNotManaged)).toBeInTheDocument();
  });

  // --- Part B: opt-in "also delete NetBird peer + setup key" on server delete ---

  // Module-on NetBird/non-NetBird servers carry 7 row actions → kebab menu. Open it
  // then pick Delete to reach the confirm dialog.
  async function openDeleteDialog(rowName?: string) {
    const scope = rowName ? within(screen.getByText(rowName).closest('tr') as HTMLElement) : screen;
    fireEvent.click(await scope.findByRole('button', { name: t.listRowMenu }));
    fireEvent.click(await screen.findByRole('menuitem', { name: t.serverActionDelete }));
    // The confirm dialog is now open.
    await screen.findByText(t.serverDeleteConfirm);
  }

  it('offers the delete-peer checkbox for a NetBird server WITH a peer (module on)', async () => {
    renderNb({ moduleEnabled: true, servers: [nbServer('nb-del', { netbird_peer_id: 'peer-1' })] });
    await openDeleteDialog();
    expect(screen.getByRole('checkbox', { name: t.serverNetbirdDeletePeer })).toBeInTheDocument();
  });

  it('offers the delete-peer checkbox for a NetBird server with ONLY a dangling setup key (no peer)', async () => {
    renderNb({
      moduleEnabled: true,
      servers: [nbServer('nb-del', { netbird_peer_id: '', netbird_setup_key_id: 'key-1' })],
    });
    await openDeleteDialog();
    expect(screen.getByRole('checkbox', { name: t.serverNetbirdDeletePeer })).toBeInTheDocument();
  });

  it('hides the delete-peer checkbox for a NetBird server with no peer AND no setup key', async () => {
    renderNb({
      moduleEnabled: true,
      servers: [nbServer('nb-del', { netbird_peer_id: '', netbird_setup_key_id: '' })],
    });
    await openDeleteDialog();
    expect(
      screen.queryByRole('checkbox', { name: t.serverNetbirdDeletePeer }),
    ).not.toBeInTheDocument();
  });

  it('hides the delete-peer checkbox for a non-NetBird server even with a leftover key (module on)', async () => {
    // netbird_enabled=false but a stray setup-key id is present → the netbird_enabled
    // gate is the only thing hiding the checkbox (dropping it from production shows it).
    renderNb({
      moduleEnabled: true,
      servers: [{ ...makeServer('plain', 'healthy'), netbird_setup_key_id: 'leftover-key' }],
    });
    await openDeleteDialog();
    expect(
      screen.queryByRole('checkbox', { name: t.serverNetbirdDeletePeer }),
    ).not.toBeInTheDocument();
  });

  it('hides the delete-peer checkbox when the module is disabled (even for a NetBird server)', async () => {
    // Module off → 6 row actions → the delete action renders as a direct button.
    const { fakeApi } = renderNb({
      moduleEnabled: false,
      servers: [nbServer('nb-del', { netbird_peer_id: 'peer-1' })],
    });
    await waitFor(() => expect(fakeApi.netbirdEnabled).toHaveBeenCalled());
    fireEvent.click(screen.getByRole('button', { name: t.serverActionDelete }));
    expect(screen.getByText(t.serverDeleteConfirm)).toBeInTheDocument();
    expect(
      screen.queryByRole('checkbox', { name: t.serverNetbirdDeletePeer }),
    ).not.toBeInTheDocument();
  });

  it('pre-checks the box when netbird_peer_managed is true', async () => {
    renderNb({
      moduleEnabled: true,
      servers: [nbServer('nb-del', { netbird_peer_id: 'peer-1', netbird_peer_managed: true })],
    });
    await openDeleteDialog();
    expect(
      (screen.getByRole('checkbox', { name: t.serverNetbirdDeletePeer }) as HTMLInputElement)
        .checked,
    ).toBe(true);
  });

  it('leaves the box unchecked when netbird_peer_managed is false', async () => {
    renderNb({
      moduleEnabled: true,
      servers: [nbServer('nb-del', { netbird_peer_id: 'peer-1', netbird_peer_managed: false })],
    });
    await openDeleteDialog();
    expect(
      (screen.getByRole('checkbox', { name: t.serverNetbirdDeletePeer }) as HTMLInputElement)
        .checked,
    ).toBe(false);
  });

  it('deletes with delete_peer=true when the (pre-checked) box is confirmed', async () => {
    const deleteServer = vi.fn(async () => ({ ok: true }));
    renderNb({
      moduleEnabled: true,
      servers: [nbServer('nb-del', { netbird_peer_id: 'peer-1', netbird_peer_managed: true })],
      deleteServer,
    });
    await openDeleteDialog();
    fireEvent.click(screen.getByRole('button', { name: t.serverActionDelete }));
    await waitFor(() => expect(deleteServer).toHaveBeenCalledWith('nb-del', true));
  });

  it('deletes with delete_peer=false when the box is left unchecked', async () => {
    const deleteServer = vi.fn(async () => ({ ok: true }));
    renderNb({
      moduleEnabled: true,
      servers: [nbServer('nb-del', { netbird_peer_id: 'peer-1', netbird_peer_managed: false })],
      deleteServer,
    });
    await openDeleteDialog();
    fireEvent.click(screen.getByRole('button', { name: t.serverActionDelete }));
    await waitFor(() => expect(deleteServer).toHaveBeenCalledWith('nb-del', false));
  });

  it('threads the toggled checkbox value to deleteServer (unchecked → checked → true)', async () => {
    const deleteServer = vi.fn(async () => ({ ok: true }));
    renderNb({
      moduleEnabled: true,
      servers: [nbServer('nb-del', { netbird_peer_id: 'peer-1', netbird_peer_managed: false })],
      deleteServer,
    });
    await openDeleteDialog();
    fireEvent.click(screen.getByRole('checkbox', { name: t.serverNetbirdDeletePeer }));
    fireEvent.click(screen.getByRole('button', { name: t.serverActionDelete }));
    await waitFor(() => expect(deleteServer).toHaveBeenCalledWith('nb-del', true));
  });

  it('shows a warning toast but still removes the row when the NetBird cleanup failed', async () => {
    const deleteServer = vi.fn(async () => ({ ok: true, netbird_peer_delete_failed: true }));
    const { setServers } = renderNb({
      moduleEnabled: true,
      servers: [nbServer('nb-del', { netbird_peer_id: 'peer-1', netbird_peer_managed: true })],
      deleteServer,
    });
    await openDeleteDialog();
    fireEvent.click(screen.getByRole('button', { name: t.serverActionDelete }));
    await waitFor(() => expect(deleteServer).toHaveBeenCalledWith('nb-del', true));
    // Row still filtered out (setServers invoked) AND the non-fatal warning shows.
    await waitFor(() => expect(setServers).toHaveBeenCalled());
    expect(await screen.findByText(t.serverNetbirdPeerDeleteWarning)).toBeInTheDocument();
  });

  // --- Part C: display-once `netbird up …` console command in the reveal modal ---

  it('reveals the setup command (with a copy button) returned by create', async () => {
    const createServer = vi.fn(async (body: CreateServerRequest) => ({
      ...makeServer('new', 'healthy'),
      ...body,
      netbird_setup_key: 'SK-cmd-1',
      netbird_setup_command: 'netbird up --management-url https://nb.test --setup-key SK-cmd-1',
    }));
    renderNb({
      moduleEnabled: true,
      createServer: createServer as unknown as PortalApi['createServer'],
    });
    fireEvent.click(screen.getByRole('button', { name: t.serverCreate }));
    fireEvent.change(screen.getByLabelText(t.serverNameLabel), { target: { value: 'srv' } });
    fireEvent.click(await screen.findByLabelText(t.serverNetbirdEnable));
    fireEvent.click(screen.getByRole('button', { name: t.serverCreate }));
    await waitFor(() => expect(createServer).toHaveBeenCalledTimes(1));
    expect(
      await screen.findByText('netbird up --management-url https://nb.test --setup-key SK-cmd-1'),
    ).toBeInTheDocument();
    // The command line carries its own copy button (aria-label = the command label).
    expect(screen.getByRole('button', { name: t.serverNetbirdSetupCommand })).toBeInTheDocument();
  });

  it('reveals the setup command returned by regenerate', async () => {
    const regenerateNetbirdKey = vi.fn(async () => ({
      setup_key: 'SK-regen-cmd',
      netbird_setup_command: 'netbird up --management-url https://nb.test --setup-key SK-regen-cmd',
    }));
    renderNb({
      moduleEnabled: true,
      // A gateway-managed peer, so the regenerate action is enabled (see gate test).
      servers: [
        nbServer('nb-1', {
          netbird_peer_id: 'peer-1',
          netbird_connected: true,
          netbird_peer_managed: true,
        }),
      ],
      regenerateNetbirdKey,
    });
    fireEvent.click(await screen.findByRole('button', { name: t.listRowMenu }));
    fireEvent.click(await screen.findByRole('menuitem', { name: t.serverNetbirdRegenerate }));
    fireEvent.click(await screen.findByRole('button', { name: t.serverNetbirdRegenerate }));
    await waitFor(() => expect(regenerateNetbirdKey).toHaveBeenCalledWith('nb-1'));
    expect(
      await screen.findByText(
        'netbird up --management-url https://nb.test --setup-key SK-regen-cmd',
      ),
    ).toBeInTheDocument();
    expect(screen.getByRole('button', { name: t.serverNetbirdSetupCommand })).toBeInTheDocument();
  });

  // --- Per-server policy-management override control (mode-aware, system-admin-only) ---

  it('hides the policy-override control for a non-system-admin (the whole linkage panel is system-admin-only)', async () => {
    const getSystemSettings = vi.fn(async () =>
      makeSystemSettings({ netbird_effective_policy_scope: 'all' }),
    );
    renderNb({
      moduleEnabled: true,
      isSystemAdmin: false,
      servers: [nbServer('nb-pol', { netbird_peer_id: 'peer-1' })],
      getSystemSettings,
    });
    fireEvent.click(await screen.findByRole('button', { name: t.listRowMenu }));
    fireEvent.click(await screen.findByRole('menuitem', { name: t.serverActionEdit }));
    await screen.findByLabelText(t.serverNameLabel);
    // Never fetched (not system-admin) and neither override label ever renders.
    expect(getSystemSettings).not.toHaveBeenCalled();
    expect(screen.queryByText(t.serverNetbirdPolicyExclude)).not.toBeInTheDocument();
    expect(screen.queryByText(t.serverNetbirdPolicyInclude)).not.toBeInTheDocument();
  });

  it("shows the OPT-OUT checkbox when the effective scope is 'all'", async () => {
    const getSystemSettings = vi.fn(async () =>
      makeSystemSettings({ netbird_effective_policy_scope: 'all' }),
    );
    renderNb({
      moduleEnabled: true,
      isSystemAdmin: true,
      servers: [nbServer('nb-pol', { netbird_peer_id: 'peer-1' })],
      getSystemSettings,
    });
    fireEvent.click(await screen.findByRole('button', { name: t.listRowMenu }));
    fireEvent.click(await screen.findByRole('menuitem', { name: t.serverActionEdit }));
    expect(
      await screen.findByRole('checkbox', { name: t.serverNetbirdPolicyExclude }),
    ).toBeInTheDocument();
    // The opt-IN control (meaningful only in "selected" scope) never renders here.
    expect(
      screen.queryByRole('checkbox', { name: t.serverNetbirdPolicyInclude }),
    ).not.toBeInTheDocument();
  });

  it("renders the NetBird-only warning and a RED exclude control when netbird_only is on ('all' scope)", async () => {
    const getSystemSettings = vi.fn(async () =>
      makeSystemSettings({ netbird_effective_policy_scope: 'all', netbird_only: true }),
    );
    renderNb({
      moduleEnabled: true,
      isSystemAdmin: true,
      servers: [nbServer('nb-pol', { netbird_peer_id: 'peer-1' })],
      getSystemSettings,
    });
    fireEvent.click(await screen.findByRole('button', { name: t.listRowMenu }));
    fireEvent.click(await screen.findByRole('menuitem', { name: t.serverActionEdit }));
    // The warning Alert is rendered.
    expect(await screen.findByText(t.serverNetbirdOnlyPolicyWarning)).toBeInTheDocument();
    // The exclude control is red: its MUI Checkbox root carries the error-color class.
    const box = (await screen.findByRole('checkbox', {
      name: t.serverNetbirdPolicyExclude,
    })) as HTMLInputElement;
    const root = box.closest('.MuiCheckbox-root');
    expect(root).not.toBeNull();
    expect(root?.classList.contains('MuiCheckbox-colorError')).toBe(true);
  });

  it("does NOT render the NetBird-only warning when netbird_only is off ('all' scope)", async () => {
    const getSystemSettings = vi.fn(async () =>
      makeSystemSettings({ netbird_effective_policy_scope: 'all', netbird_only: false }),
    );
    renderNb({
      moduleEnabled: true,
      isSystemAdmin: true,
      servers: [nbServer('nb-pol', { netbird_peer_id: 'peer-1' })],
      getSystemSettings,
    });
    fireEvent.click(await screen.findByRole('button', { name: t.listRowMenu }));
    fireEvent.click(await screen.findByRole('menuitem', { name: t.serverActionEdit }));
    // The exclude control renders (so the netbird section loaded) but the warning does not.
    await screen.findByRole('checkbox', { name: t.serverNetbirdPolicyExclude });
    expect(screen.queryByText(t.serverNetbirdOnlyPolicyWarning)).not.toBeInTheDocument();
  });

  it("shows the OPT-IN checkbox when the effective scope is 'selected'", async () => {
    const getSystemSettings = vi.fn(async () =>
      makeSystemSettings({ netbird_effective_policy_scope: 'selected' }),
    );
    renderNb({
      moduleEnabled: true,
      isSystemAdmin: true,
      servers: [nbServer('nb-pol', { netbird_peer_id: 'peer-1' })],
      getSystemSettings,
    });
    fireEvent.click(await screen.findByRole('button', { name: t.listRowMenu }));
    fireEvent.click(await screen.findByRole('menuitem', { name: t.serverActionEdit }));
    expect(
      await screen.findByRole('checkbox', { name: t.serverNetbirdPolicyInclude }),
    ).toBeInTheDocument();
    // The opt-OUT control (meaningful only in "all" scope) never renders here.
    expect(
      screen.queryByRole('checkbox', { name: t.serverNetbirdPolicyExclude }),
    ).not.toBeInTheDocument();
  });

  it('hides both override controls when the effective-scope fetch fails (graceful degrade)', async () => {
    const getSystemSettings = vi.fn(async () => {
      throw new Error('nope');
    });
    renderNb({
      moduleEnabled: true,
      isSystemAdmin: true,
      servers: [nbServer('nb-pol', { netbird_peer_id: 'peer-1' })],
      getSystemSettings,
    });
    fireEvent.click(await screen.findByRole('button', { name: t.listRowMenu }));
    fireEvent.click(await screen.findByRole('menuitem', { name: t.serverActionEdit }));
    // Same cause as the create-form sites above, other fetch: getSystemSettings
    // having been CALLED says nothing about its rejection having been committed,
    // and under a slow commit this test failed with the include control still on
    // screen. It renders from the netbirdEnabled scope ('selected', renderNb's
    // default) the moment the edit form opens and the failure can only clear it a
    // microtask later, so asserting it PRESENT first is also what stops the
    // absence assertions from passing vacuously -- before the scope lands the
    // control is absent for the wrong reason.
    expect(
      screen.getByRole('checkbox', { name: t.serverNetbirdPolicyInclude }),
    ).toBeInTheDocument();
    await waitFor(() => expect(getSystemSettings).toHaveBeenCalled());
    // The rest of the linkage editor still renders (never blocked by the failure).
    expect(screen.getByRole('button', { name: t.serverNetbirdLinkSave })).toBeInTheDocument();
    await waitFor(() =>
      expect(
        screen.queryByRole('checkbox', { name: t.serverNetbirdPolicyInclude }),
      ).not.toBeInTheDocument(),
    );
    expect(
      screen.queryByRole('checkbox', { name: t.serverNetbirdPolicyExclude }),
    ).not.toBeInTheDocument();
  });

  it('toggles the opt-out checkbox and threads "exclude" to setServerNetbird on save (\'all\' scope)', async () => {
    const server = nbServer('nb-pol-all', { netbird_peer_id: 'peer-1' });
    const getSystemSettings = vi.fn(async () =>
      makeSystemSettings({ netbird_effective_policy_scope: 'all' }),
    );
    const setServerNetbird = vi.fn(async () => ({ ...server, netbird_policy_override: 'exclude' }));
    renderNb({
      moduleEnabled: true,
      isSystemAdmin: true,
      servers: [server],
      setServerNetbird,
      getSystemSettings,
    });
    fireEvent.click(await screen.findByRole('button', { name: t.listRowMenu }));
    fireEvent.click(await screen.findByRole('menuitem', { name: t.serverActionEdit }));
    const box = (await screen.findByRole('checkbox', {
      name: t.serverNetbirdPolicyExclude,
    })) as HTMLInputElement;
    expect(box.checked).toBe(false); // server.netbird_policy_override defaults to ""
    fireEvent.click(box);
    fireEvent.click(screen.getByRole('button', { name: t.serverNetbirdLinkSave }));
    await waitFor(() =>
      expect(setServerNetbird).toHaveBeenCalledWith(
        'nb-pol-all',
        true,
        'peer-1',
        [],
        false,
        'exclude',
        false,
        false,
      ),
    );
  });

  it('toggles the opt-in checkbox and threads "include" to setServerNetbird on save (\'selected\' scope)', async () => {
    const server = nbServer('nb-pol-sel', { netbird_peer_id: 'peer-1' });
    const getSystemSettings = vi.fn(async () =>
      makeSystemSettings({ netbird_effective_policy_scope: 'selected' }),
    );
    const setServerNetbird = vi.fn(async () => ({ ...server, netbird_policy_override: 'include' }));
    renderNb({
      moduleEnabled: true,
      isSystemAdmin: true,
      servers: [server],
      setServerNetbird,
      getSystemSettings,
    });
    fireEvent.click(await screen.findByRole('button', { name: t.listRowMenu }));
    fireEvent.click(await screen.findByRole('menuitem', { name: t.serverActionEdit }));
    const box = (await screen.findByRole('checkbox', {
      name: t.serverNetbirdPolicyInclude,
    })) as HTMLInputElement;
    expect(box.checked).toBe(false);
    fireEvent.click(box);
    fireEvent.click(screen.getByRole('button', { name: t.serverNetbirdLinkSave }));
    await waitFor(() =>
      expect(setServerNetbird).toHaveBeenCalledWith(
        'nb-pol-sel',
        true,
        'peer-1',
        [],
        false,
        'include',
        false,
        false,
      ),
    );
  });

  it("pre-fills the opt-out checkbox as checked when the server already has an 'exclude' override", async () => {
    const server = nbServer('nb-pol-excl', {
      netbird_peer_id: 'peer-1',
      netbird_policy_override: 'exclude',
    });
    const getSystemSettings = vi.fn(async () =>
      makeSystemSettings({ netbird_effective_policy_scope: 'all' }),
    );
    renderNb({ moduleEnabled: true, isSystemAdmin: true, servers: [server], getSystemSettings });
    fireEvent.click(await screen.findByRole('button', { name: t.listRowMenu }));
    fireEvent.click(await screen.findByRole('menuitem', { name: t.serverActionEdit }));
    const box = (await screen.findByRole('checkbox', {
      name: t.serverNetbirdPolicyExclude,
    })) as HTMLInputElement;
    expect(box.checked).toBe(true);
  });

  // --- Create-form policy-override control (role / scope / deny aware) ---

  it('forces + locks the opt-in (include) control for a NORMAL admin under selected scope + deny-by-default on create', async () => {
    const createServer = vi.fn(
      async (body: CreateServerRequest) =>
        ({ ...makeServer('new', 'healthy'), ...body }) as CreateServerResponse,
    );
    renderNb({
      moduleEnabled: true,
      netbirdOnly: true,
      managePolicies: true,
      effectivePolicyScope: 'selected',
      denyByDefault: true,
      isSystemAdmin: false,
      createServer: createServer as unknown as PortalApi['createServer'],
    });
    await openCreateOnceNetbirdFlagsCommitted();
    // Synchronous now that the form is opened only once manage/scope/deny are
    // committed: the pre-selection is decided at openCreate and cannot arrive later.
    const nb = (await screen.findByLabelText(t.serverNetbirdEnable)) as HTMLInputElement;
    expect(nb.checked).toBe(true);
    const box = (await screen.findByLabelText(t.serverNetbirdPolicyInclude)) as HTMLInputElement;
    expect(box.checked).toBe(true);
    expect(box).toBeDisabled();
    expect(screen.getByText(t.serverNetbirdPolicyForcedNote)).toBeInTheDocument();
    fireEvent.change(screen.getByLabelText(t.serverNameLabel), { target: { value: 'srv' } });
    fireEvent.click(screen.getByRole('button', { name: t.serverCreate }));
    await waitFor(() => expect(createServer).toHaveBeenCalledTimes(1));
    expect(createServer.mock.calls[0][0].netbird_policy_override).toBe('include');
  });

  it('pre-selects but keeps the opt-in (include) control editable for a SYSTEM admin under selected scope + deny-by-default on create', async () => {
    const createServer = vi.fn(
      async (body: CreateServerRequest) =>
        ({ ...makeServer('new', 'healthy'), ...body }) as CreateServerResponse,
    );
    renderNb({
      moduleEnabled: true,
      netbirdOnly: true,
      managePolicies: true,
      effectivePolicyScope: 'selected',
      denyByDefault: true,
      isSystemAdmin: true,
      createServer: createServer as unknown as PortalApi['createServer'],
    });
    await openCreateOnceNetbirdFlagsCommitted();
    // Synchronous now that the form is opened only once the flags are committed:
    // the pre-selection is decided at openCreate and cannot arrive later.
    const nb = (await screen.findByLabelText(t.serverNetbirdEnable)) as HTMLInputElement;
    expect(nb.checked).toBe(true);
    const box = (await screen.findByLabelText(t.serverNetbirdPolicyInclude)) as HTMLInputElement;
    expect(box.checked).toBe(true);
    expect(box).not.toBeDisabled();
    expect(screen.getByText(t.serverNetbirdPolicyPrecheckNote)).toBeInTheDocument();
    // Uncheck it → the override is cleared (a system-admin may opt out).
    fireEvent.click(box);
    fireEvent.change(screen.getByLabelText(t.serverNameLabel), { target: { value: 'srv' } });
    fireEvent.click(screen.getByRole('button', { name: t.serverCreate }));
    await waitFor(() => expect(createServer).toHaveBeenCalledTimes(1));
    expect(createServer.mock.calls[0][0].netbird_policy_override).toBe('');
  });

  it('shows an editable, default-off opt-in control with no note under selected scope + deny off (normal admin) on create', async () => {
    const createServer = vi.fn(
      async (body: CreateServerRequest) =>
        ({ ...makeServer('new', 'healthy'), ...body }) as CreateServerResponse,
    );
    renderNb({
      moduleEnabled: true,
      netbirdOnly: true,
      managePolicies: true,
      effectivePolicyScope: 'selected',
      denyByDefault: false,
      isSystemAdmin: false,
      createServer: createServer as unknown as PortalApi['createServer'],
    });
    await openCreateOnceNetbirdFlagsCommitted();
    // Synchronous now that the form is opened only once the flags are committed:
    // the pre-selection is decided at openCreate and cannot arrive later.
    const nb = (await screen.findByLabelText(t.serverNetbirdEnable)) as HTMLInputElement;
    expect(nb.checked).toBe(true);
    const box = (await screen.findByLabelText(t.serverNetbirdPolicyInclude)) as HTMLInputElement;
    expect(box.checked).toBe(false);
    expect(box).not.toBeDisabled();
    expect(screen.queryByText(t.serverNetbirdPolicyForcedNote)).not.toBeInTheDocument();
    expect(screen.queryByText(t.serverNetbirdPolicyPrecheckNote)).not.toBeInTheDocument();
    fireEvent.click(box);
    fireEvent.change(screen.getByLabelText(t.serverNameLabel), { target: { value: 'srv' } });
    fireEvent.click(screen.getByRole('button', { name: t.serverCreate }));
    await waitFor(() => expect(createServer).toHaveBeenCalledTimes(1));
    expect(createServer.mock.calls[0][0].netbird_policy_override).toBe('include');
  });

  it('renders the RED opt-out (exclude) control + a deny note under all scope for a SYSTEM admin on create', async () => {
    const createServer = vi.fn(
      async (body: CreateServerRequest) =>
        ({ ...makeServer('new', 'healthy'), ...body }) as CreateServerResponse,
    );
    renderNb({
      moduleEnabled: true,
      netbirdOnly: true,
      managePolicies: true,
      effectivePolicyScope: 'all',
      denyByDefault: true,
      isSystemAdmin: true,
      createServer: createServer as unknown as PortalApi['createServer'],
    });
    await openCreateOnceNetbirdFlagsCommitted();
    // Synchronous now that the form is opened only once the flags are committed:
    // the pre-selection is decided at openCreate and cannot arrive later.
    const nb = (await screen.findByLabelText(t.serverNetbirdEnable)) as HTMLInputElement;
    expect(nb.checked).toBe(true);
    const box = (await screen.findByLabelText(t.serverNetbirdPolicyExclude)) as HTMLInputElement;
    expect(box.checked).toBe(false);
    expect(screen.getByText(t.serverNetbirdPolicyOptOutDenyNote)).toBeInTheDocument();
    fireEvent.click(box);
    fireEvent.change(screen.getByLabelText(t.serverNameLabel), { target: { value: 'srv' } });
    fireEvent.click(screen.getByRole('button', { name: t.serverCreate }));
    await waitFor(() => expect(createServer).toHaveBeenCalledTimes(1));
    expect(createServer.mock.calls[0][0].netbird_policy_override).toBe('exclude');
  });

  it('shows NO policy control for a NORMAL admin under all scope on create (netbird still forced on)', async () => {
    renderNb({
      moduleEnabled: true,
      netbirdOnly: true,
      managePolicies: true,
      effectivePolicyScope: 'all',
      denyByDefault: true,
      isSystemAdmin: false,
    });
    await openCreateOnceNetbirdFlagsCommitted();
    // The netbird checkbox is forced on for a normal admin under netbird_only, but the
    // all-scope opt-out is system-admin-only → neither policy control renders. Synchronous
    // now that the form is opened only once the flags are committed.
    const nb = (await screen.findByLabelText(t.serverNetbirdEnable)) as HTMLInputElement;
    expect(nb.checked).toBe(true);
    expect(screen.queryByLabelText(t.serverNetbirdPolicyExclude)).toBeNull();
    expect(screen.queryByLabelText(t.serverNetbirdPolicyInclude)).toBeNull();
  });

  it('shows NO policy control when manage_policies is off on create', async () => {
    renderNb({
      moduleEnabled: true,
      netbirdOnly: true,
      managePolicies: false,
      effectivePolicyScope: 'selected',
      denyByDefault: true,
      isSystemAdmin: true,
    });
    await openCreateOnceNetbirdFlagsCommitted();
    // Synchronous now that the form is opened only once the flags are committed:
    // the pre-selection is decided at openCreate and cannot arrive later.
    const nb = (await screen.findByLabelText(t.serverNetbirdEnable)) as HTMLInputElement;
    expect(nb.checked).toBe(true);
    expect(screen.queryByLabelText(t.serverNetbirdPolicyInclude)).toBeNull();
  });

  it("does NOT submit a stale 'include' override when manage_policies is off (selected + deny) on create", async () => {
    // Degenerate config: no policy control renders (manage off), so a netbird create
    // must NOT silently thread the pre-selected "include" — the override falls to "".
    const createServer = vi.fn(
      async (body: CreateServerRequest) =>
        ({ ...makeServer('new', 'healthy'), ...body }) as CreateServerResponse,
    );
    renderNb({
      moduleEnabled: true,
      netbirdOnly: true,
      managePolicies: false,
      effectivePolicyScope: 'selected',
      denyByDefault: true,
      isSystemAdmin: true,
      createServer: createServer as unknown as PortalApi['createServer'],
    });
    await openCreateOnceNetbirdFlagsCommitted();
    // Synchronous now that the form is opened only once the flags are committed:
    // the pre-selection is decided at openCreate and cannot arrive later.
    const nb = (await screen.findByLabelText(t.serverNetbirdEnable)) as HTMLInputElement;
    expect(nb.checked).toBe(true);
    // No policy control (manage off).
    expect(screen.queryByLabelText(t.serverNetbirdPolicyInclude)).toBeNull();
    fireEvent.change(screen.getByLabelText(t.serverNameLabel), { target: { value: 'srv' } });
    fireEvent.click(screen.getByRole('button', { name: t.serverCreate }));
    await waitFor(() => expect(createServer).toHaveBeenCalledTimes(1));
    // netbird create submits, but the override is empty (no stale "include").
    expect(createServer.mock.calls[0][0].netbird_enabled).toBe(true);
    expect(createServer.mock.calls[0][0].netbird_policy_override).toBe('');
  });
});

// Note: this repo does NOT have @testing-library/user-event installed
// (verified absent from package.json/node_modules) -- these tests use the
// existing fireEvent/waitFor idiom the rest of this file uses instead.
describe('ServerList certificate override', () => {
  beforeEach(() => {
    try {
      window.localStorage.clear();
    } catch {
      /* jsdom/private-mode guard */
    }
  });

  function certServer(id: string, over: Partial<PortalServer> = {}): PortalServer {
    return { ...makeServer(id, 'healthy'), netbird_enabled: true, ...over };
  }

  function renderCert(opts: {
    certEnabled?: boolean;
    certServerScope?: 'all' | 'selected';
    servers?: PortalServer[];
    setServerCertificateOverride?: PortalApi['setServerCertificateOverride'];
    getSystemSettings?: PortalApi['getSystemSettings'];
  }) {
    const getSystemSettings =
      opts.getSystemSettings ??
      vi.fn(async () =>
        makeSystemSettings({
          cert_enabled: opts.certEnabled ?? true,
          cert_server_scope: opts.certServerScope ?? 'selected',
        }),
      );
    const setServerCertificateOverride =
      opts.setServerCertificateOverride ??
      vi.fn(async () => (opts.servers ?? [])[0] ?? certServer('x'));
    const netbirdEnabled = vi.fn(async () => ({
      enabled: true,
      module_enabled: true,
      netbird_only: false,
      manage_policies: false,
      effective_policy_scope: 'selected',
      deny_by_default: false,
    }));
    const fakeApi = {
      ...baseServerListApi(),
      adminUsers: vi.fn(async () => ({ data: [] })),
      activeBenchmarks: vi.fn(async () => []),
      netbirdEnabled,
      getSystemSettings,
      setServerCertificateOverride,
      serverAdminGroupCandidates: vi.fn(async () => defaultAdminGroupCandidates),
      servers: vi.fn(async () => ({ data: opts.servers ?? [] })),
    };
    render(
      <ToastProvider>
        <ServerList
          t={t}
          api={fakeApi}
          servers={opts.servers ?? []}
          setServers={vi.fn()}
          role="admin"
          isSystemAdmin
        />
      </ToastProvider>,
    );
    return { getSystemSettings, setServerCertificateOverride };
  }

  it("shows a red opt-out checkbox in 'all' scope and sends exclude", async () => {
    const server = certServer('cert-1', { certificate_override: '' });
    const { setServerCertificateOverride } = renderCert({
      certServerScope: 'all',
      servers: [server],
    });
    fireEvent.click(await screen.findByRole('button', { name: t.listRowMenu }));
    fireEvent.click(await screen.findByRole('menuitem', { name: t.serverActionEdit }));
    const box = (await screen.findByRole('checkbox', {
      name: t.serverCertificateExclude,
    })) as HTMLInputElement;
    expect(box.checked).toBe(false);
    fireEvent.click(box);
    await waitFor(() =>
      expect(setServerCertificateOverride).toHaveBeenCalledWith('cert-1', 'exclude'),
    );
  });

  it("shows an opt-in checkbox in 'selected' scope and sends include", async () => {
    const server = certServer('cert-2', { certificate_override: '' });
    const { setServerCertificateOverride } = renderCert({
      certServerScope: 'selected',
      servers: [server],
    });
    fireEvent.click(await screen.findByRole('button', { name: t.listRowMenu }));
    fireEvent.click(await screen.findByRole('menuitem', { name: t.serverActionEdit }));
    expect(
      screen.queryByRole('checkbox', { name: t.serverCertificateExclude }),
    ).not.toBeInTheDocument();
    const box = (await screen.findByRole('checkbox', {
      name: t.serverCertificateInclude,
    })) as HTMLInputElement;
    fireEvent.click(box);
    await waitFor(() =>
      expect(setServerCertificateOverride).toHaveBeenCalledWith('cert-2', 'include'),
    );
  });

  it('hides the control when the certificate module is disabled', async () => {
    const server = certServer('cert-3', { certificate_override: '' });
    renderCert({ certEnabled: false, certServerScope: 'all', servers: [server] });
    fireEvent.click(await screen.findByRole('button', { name: t.listRowMenu }));
    fireEvent.click(await screen.findByRole('menuitem', { name: t.serverActionEdit }));
    await screen.findByLabelText(t.serverNameLabel);
    expect(screen.queryByLabelText(t.serverCertificateExclude)).toBeNull();
    expect(screen.queryByLabelText(t.serverCertificateInclude)).toBeNull();
  });
});

describe('ServerList https-switch override (P4 Task 11)', () => {
  beforeEach(() => {
    try {
      window.localStorage.clear();
    } catch {
      /* jsdom/private-mode guard */
    }
  });

  function switchServer(id: string, over: Partial<PortalServer> = {}): PortalServer {
    return { ...makeServer(id, 'healthy'), netbird_enabled: true, ...over };
  }

  function renderSwitch(opts: {
    httpsSwitchMode?: 'manual' | 'auto' | 'selected';
    servers?: PortalServer[];
    setServerHTTPSSwitchOverride?: PortalApi['setServerHTTPSSwitchOverride'];
    getSystemSettings?: PortalApi['getSystemSettings'];
  }) {
    const getSystemSettings =
      opts.getSystemSettings ??
      vi.fn(async () =>
        makeSystemSettings({ cert_https_switch_mode: opts.httpsSwitchMode ?? 'auto' }),
      );
    const setServerHTTPSSwitchOverride =
      opts.setServerHTTPSSwitchOverride ??
      vi.fn(async () => (opts.servers ?? [])[0] ?? switchServer('x'));
    const netbirdEnabled = vi.fn(async () => ({
      enabled: true,
      module_enabled: true,
      netbird_only: false,
      manage_policies: false,
      effective_policy_scope: 'selected',
      deny_by_default: false,
    }));
    const fakeApi = {
      ...baseServerListApi(),
      adminUsers: vi.fn(async () => ({ data: [] })),
      activeBenchmarks: vi.fn(async () => []),
      netbirdEnabled,
      getSystemSettings,
      setServerHTTPSSwitchOverride,
      serverAdminGroupCandidates: vi.fn(async () => defaultAdminGroupCandidates),
      servers: vi.fn(async () => ({ data: opts.servers ?? [] })),
    };
    render(
      <ToastProvider>
        <ServerList
          t={t}
          api={fakeApi}
          servers={opts.servers ?? []}
          setServers={vi.fn()}
          role="admin"
          isSystemAdmin
        />
      </ToastProvider>,
    );
    return { getSystemSettings, setServerHTTPSSwitchOverride };
  }

  it("shows an opt-out checkbox in 'auto' mode and sends exclude", async () => {
    const server = switchServer('switch-1', { https_switch_override: '' });
    const { setServerHTTPSSwitchOverride } = renderSwitch({
      httpsSwitchMode: 'auto',
      servers: [server],
    });
    fireEvent.click(await screen.findByRole('button', { name: t.listRowMenu }));
    fireEvent.click(await screen.findByRole('menuitem', { name: t.serverActionEdit }));
    expect(
      screen.queryByRole('checkbox', { name: t.serverHTTPSSwitchInclude }),
    ).not.toBeInTheDocument();
    const box = (await screen.findByRole('checkbox', {
      name: t.serverHTTPSSwitchExclude,
    })) as HTMLInputElement;
    expect(box.checked).toBe(false);
    fireEvent.click(box);
    await waitFor(() =>
      expect(setServerHTTPSSwitchOverride).toHaveBeenCalledWith('switch-1', 'exclude'),
    );
  });

  it("shows an opt-in checkbox in 'selected' mode and sends include", async () => {
    const server = switchServer('switch-2', { https_switch_override: '' });
    const { setServerHTTPSSwitchOverride } = renderSwitch({
      httpsSwitchMode: 'selected',
      servers: [server],
    });
    fireEvent.click(await screen.findByRole('button', { name: t.listRowMenu }));
    fireEvent.click(await screen.findByRole('menuitem', { name: t.serverActionEdit }));
    expect(
      screen.queryByRole('checkbox', { name: t.serverHTTPSSwitchExclude }),
    ).not.toBeInTheDocument();
    const box = (await screen.findByRole('checkbox', {
      name: t.serverHTTPSSwitchInclude,
    })) as HTMLInputElement;
    fireEvent.click(box);
    await waitFor(() =>
      expect(setServerHTTPSSwitchOverride).toHaveBeenCalledWith('switch-2', 'include'),
    );
  });

  it("hides the control entirely in 'manual' mode", async () => {
    const server = switchServer('switch-3', { https_switch_override: '' });
    renderSwitch({ httpsSwitchMode: 'manual', servers: [server] });
    fireEvent.click(await screen.findByRole('button', { name: t.listRowMenu }));
    fireEvent.click(await screen.findByRole('menuitem', { name: t.serverActionEdit }));
    await screen.findByLabelText(t.serverNameLabel);
    expect(screen.queryByLabelText(t.serverHTTPSSwitchExclude)).toBeNull();
    expect(screen.queryByLabelText(t.serverHTTPSSwitchInclude)).toBeNull();
  });

  // Mutual-exclusion guard (the hard requirement in the brief): unchecking an
  // already-"exclude"-overridden server under 'auto' mode must send "" -- never
  // leave the stale opposite-sense value that a later mode flip could resurrect.
  it("unchecking the opt-out box in 'auto' mode sends '' (never a stale value)", async () => {
    const server = switchServer('switch-4', { https_switch_override: 'exclude' });
    const { setServerHTTPSSwitchOverride } = renderSwitch({
      httpsSwitchMode: 'auto',
      servers: [server],
    });
    fireEvent.click(await screen.findByRole('button', { name: t.listRowMenu }));
    fireEvent.click(await screen.findByRole('menuitem', { name: t.serverActionEdit }));
    const box = (await screen.findByRole('checkbox', {
      name: t.serverHTTPSSwitchExclude,
    })) as HTMLInputElement;
    expect(box.checked).toBe(true);
    fireEvent.click(box);
    await waitFor(() => expect(setServerHTTPSSwitchOverride).toHaveBeenCalledWith('switch-4', ''));
  });

  // The override is gated on the switch mode ONLY, never on NetBird (unlike the
  // certificate_override sibling): a server whose own netbird_enabled is false
  // still shows the opt-out control and submits it under 'auto'.
  it('renders the opt-out control for a server with netbird_enabled=false', async () => {
    const server = switchServer('switch-nb', { https_switch_override: '', netbird_enabled: false });
    const { setServerHTTPSSwitchOverride } = renderSwitch({
      httpsSwitchMode: 'auto',
      servers: [server],
    });
    fireEvent.click(await screen.findByRole('button', { name: t.listRowMenu }));
    fireEvent.click(await screen.findByRole('menuitem', { name: t.serverActionEdit }));
    const box = (await screen.findByRole('checkbox', {
      name: t.serverHTTPSSwitchExclude,
    })) as HTMLInputElement;
    expect(box.checked).toBe(false);
    fireEvent.click(box);
    await waitFor(() =>
      expect(setServerHTTPSSwitchOverride).toHaveBeenCalledWith('switch-nb', 'exclude'),
    );
  });
});

describe('ServerList live poll', () => {
  // The list view refreshes the servers prop on a ~5s timer (mirroring the benchmark
  // chip poll) so status / health / netbird-connection stay live. Fake timers drive it.
  function fullFakeApi(over: Partial<ServerListApi> = {}): ServerListApi {
    return {
      ...baseServerListApi(),
      netbirdEnabled: vi.fn(async () => ({
        enabled: false,
        module_enabled: false,
        netbird_only: false,
        manage_policies: false,
        effective_policy_scope: '',
        deny_by_default: false,
      })),
      ...over,
    };
  }

  it('polls api.servers() on the list view and pushes results to setServers', async () => {
    vi.useFakeTimers();
    try {
      const setServers = vi.fn();
      const fresh = [makeServer('s1', 'degraded')];
      const fakeApi = fullFakeApi({ servers: vi.fn(async () => ({ data: fresh })) });
      render(
        <ToastProvider>
          <ServerList
            t={t}
            api={fakeApi}
            servers={[makeServer('s1', 'healthy')]}
            setServers={setServers}
            role="user"
          />
        </ToastProvider>,
      );

      // No immediate tick: App already re-fetches on navigation into Servers.
      expect(fakeApi.servers).not.toHaveBeenCalled();
      await vi.advanceTimersByTimeAsync(5000);
      expect(fakeApi.servers).toHaveBeenCalledTimes(1);
      expect(setServers).toHaveBeenCalledWith(fresh);
      await vi.advanceTimersByTimeAsync(5000);
      expect(fakeApi.servers).toHaveBeenCalledTimes(2);
    } finally {
      vi.useRealTimers();
    }
  });

  it('stops polling servers after unmount', async () => {
    vi.useFakeTimers();
    try {
      const fakeApi = fullFakeApi({ servers: vi.fn(async () => ({ data: [] })) });
      const { unmount } = render(
        <ToastProvider>
          <ServerList t={t} api={fakeApi} servers={[]} setServers={vi.fn()} role="user" />
        </ToastProvider>,
      );
      unmount();
      await vi.advanceTimersByTimeAsync(15000);
      expect(fakeApi.servers).not.toHaveBeenCalled();
    } finally {
      vi.useRealTimers();
    }
  });
});

// managed_runtime_only (issue #25): the server form's own control for the flag
// whose downstream effects ApplicationSection already implements. It lives HERE
// and not on RuntimeAdminSection's "server limits" tab -- that tab is reached
// only through an existing server_agent application's manage-models action, so a
// control there could not be set before provisioning the very application the
// flag governs.
describe('ServerList managed_runtime_only control', () => {
  function managedApi(over: Partial<ServerListApi> = {}): ServerListApi {
    return {
      ...baseServerListApi(),
      adminUsers: vi.fn(async () => ({ data: [] })),
      activeBenchmarks: vi.fn(async () => []),
      serverAdminGroupCandidates: vi.fn(async () => defaultAdminGroupCandidates),
      ...over,
    };
  }

  function renderForm(api: ServerListApi, servers: PortalServer[], role = 'admin') {
    render(
      <ToastProvider>
        <ServerList t={t} api={api} servers={servers} setServers={vi.fn()} role={role} />
      </ToastProvider>,
    );
  }

  // NetBird stays off in every test here, so the row has 9 actions and
  // ListTable renders them inline -- the edit action is a plain button, with no
  // menu to open (and hence no aria-hidden overlay over the form behind it).
  function openEditForm() {
    fireEvent.click(screen.getByRole('button', { name: t.serverActionEdit }));
  }

  // A PATCH stub whose response is a valid PortalServer. Deliberately NOT
  // `{...server, ...body}`: UpdateServerRequest types `status` as a plain
  // string, so spreading it over the DTO widens PortalServer's ServerStatus
  // union. Nothing here reads the response back, so echoing the two fields
  // these tests vary is enough -- the assertions are all on the request.
  function echoUpdate(server: PortalServer) {
    return vi.fn(async (_id: string, body: UpdateServerRequest): Promise<PortalServer> => ({
      ...server,
      name: body.name ?? server.name,
      managed_runtime_only: body.managed_runtime_only ?? server.managed_runtime_only,
    }));
  }

  it('renders the checkbox unchecked on create and carries its reason as an accessible description', async () => {
    renderForm(managedApi(), []);
    fireEvent.click(screen.getByRole('button', { name: t.serverCreate }));
    const box = screen.getByRole('checkbox', { name: t.serverManagedRuntimeOnlyLabel });
    expect(box).not.toBeChecked();
    // The reason must be reachable without a hover: it rides on the input via
    // aria-describedby, announced on focus, exactly as ApplicationSection's
    // type-field helperText does (and as issue #26 asks for). A title/Tooltip
    // would be unreachable by keyboard and screen reader.
    expect(box).toHaveAccessibleDescription(t.serverManagedRuntimeOnlyHelp);
  });

  it('reflects the edited server current value in both directions', async () => {
    const on = { ...makeServer('srv-on', 'healthy'), managed_runtime_only: true };
    renderForm(managedApi(), [on]);
    openEditForm();
    expect(screen.getByRole('checkbox', { name: t.serverManagedRuntimeOnlyLabel })).toBeChecked();
    cleanup();

    // A server DTO that omits the field entirely (the optional-for-fixtures
    // case) must read as "off", never as undefined-ish checked.
    renderForm(managedApi(), [makeServer('srv-off', 'healthy')]);
    openEditForm();
    expect(
      screen.getByRole('checkbox', { name: t.serverManagedRuntimeOnlyLabel }),
    ).not.toBeChecked();
  });

  it('sends managed_runtime_only on create', async () => {
    const createServer = vi.fn(
      async (body: CreateServerRequest) =>
        ({ ...makeServer('new', 'healthy'), ...body }) as CreateServerResponse,
    );
    renderForm(managedApi({ createServer }), []);
    fireEvent.click(screen.getByRole('button', { name: t.serverCreate }));
    fireEvent.change(screen.getByLabelText(t.serverNameLabel), { target: { value: 'srv' } });
    fireEvent.change(screen.getByLabelText(t.serverDomainLabel), {
      target: { value: 'd.example.test' },
    });
    fireEvent.click(screen.getByRole('checkbox', { name: t.serverManagedRuntimeOnlyLabel }));
    await screen.findByText(t.serverAdminGroupAuto('Default Admin Group'));
    fireEvent.click(screen.getByRole('button', { name: t.serverCreate }));
    await waitFor(() => expect(createServer).toHaveBeenCalledTimes(1));
    // portal.CreateServerRequest carries the field, so provisioning a
    // managed-only server takes one call -- the point of offering it on create
    // as well as on edit.
    expect(createServer.mock.calls[0][0].managed_runtime_only).toBe(true);
  });

  it('sends managed_runtime_only: false on a create where the box was left alone', async () => {
    const createServer = vi.fn(
      async (body: CreateServerRequest) =>
        ({ ...makeServer('new', 'healthy'), ...body }) as CreateServerResponse,
    );
    renderForm(managedApi({ createServer }), []);
    fireEvent.click(screen.getByRole('button', { name: t.serverCreate }));
    fireEvent.change(screen.getByLabelText(t.serverNameLabel), { target: { value: 'srv' } });
    fireEvent.change(screen.getByLabelText(t.serverDomainLabel), {
      target: { value: 'd.example.test' },
    });
    await screen.findByText(t.serverAdminGroupAuto('Default Admin Group'));
    fireEvent.click(screen.getByRole('button', { name: t.serverCreate }));
    await waitFor(() => expect(createServer).toHaveBeenCalledTimes(1));
    // Create has no "leave unchanged" case -- a new row's column defaults to
    // false either way -- so the create body states the value outright.
    expect(createServer.mock.calls[0][0].managed_runtime_only).toBe(false);
  });

  it('turning it ON in the edit form sends managed_runtime_only: true', async () => {
    const server = makeServer('srv-e', 'healthy');
    const updateServer = echoUpdate(server);
    renderForm(managedApi({ updateServer }), [server]);
    openEditForm();
    fireEvent.click(screen.getByRole('checkbox', { name: t.serverManagedRuntimeOnlyLabel }));
    fireEvent.click(screen.getByRole('button', { name: t.serverActionSave }));
    await waitFor(() => expect(updateServer).toHaveBeenCalledTimes(1));
    expect(updateServer.mock.calls[0][1].managed_runtime_only).toBe(true);
  });

  it('turning it OFF sends an explicit managed_runtime_only: false, not an omission', async () => {
    const server = { ...makeServer('srv-e', 'healthy'), managed_runtime_only: true };
    const updateServer = echoUpdate(server);
    renderForm(managedApi({ updateServer }), [server]);
    openEditForm();
    fireEvent.click(screen.getByRole('checkbox', { name: t.serverManagedRuntimeOnlyLabel }));
    fireEvent.click(screen.getByRole('button', { name: t.serverActionSave }));
    await waitFor(() => expect(updateServer).toHaveBeenCalledTimes(1));
    // The other half of the Go *bool: omission means "leave unchanged", so
    // turning the policy OFF has to put `false` on the wire. An implementation
    // that only ever sends the field when it is true cannot clear the flag at
    // all, and the operator's uncheck would silently do nothing.
    const body = updateServer.mock.calls[0][1];
    expect(body).toHaveProperty('managed_runtime_only');
    expect(body.managed_runtime_only).toBe(false);
  });

  // THE test. portal.UpdateServerRequest.ManagedRuntimeOnly is a *bool:
  // undefined = "leave unchanged", false = "turn off". A save that touches only
  // the name must not decide the policy question at all -- if it puts `false`
  // on the wire because the form state defaulted there (an unseeded checkbox,
  // or one hidden behind a role gate), it silently destroys an operator's
  // configuration and the response looks like a perfectly ordinary 200.
  it('a save that changes only the name omits managed_runtime_only entirely', async () => {
    const server = { ...makeServer('srv-e', 'healthy'), managed_runtime_only: true };
    const updateServer = echoUpdate(server);
    renderForm(managedApi({ updateServer }), [server]);
    openEditForm();
    fireEvent.change(screen.getByLabelText(t.serverNameLabel), { target: { value: 'renamed' } });
    fireEvent.click(screen.getByRole('button', { name: t.serverActionSave }));
    await waitFor(() => expect(updateServer).toHaveBeenCalledTimes(1));
    const body = updateServer.mock.calls[0][1];
    expect(body.name).toBe('renamed');
    expect(body).not.toHaveProperty('managed_runtime_only');
  });

  it('toggling the box and toggling it back also omits the field', async () => {
    const server = makeServer('srv-e', 'healthy');
    const updateServer = echoUpdate(server);
    renderForm(managedApi({ updateServer }), [server]);
    openEditForm();
    const box = screen.getByRole('checkbox', { name: t.serverManagedRuntimeOnlyLabel });
    fireEvent.click(box);
    fireEvent.click(box);
    fireEvent.click(screen.getByRole('button', { name: t.serverActionSave }));
    await waitFor(() => expect(updateServer).toHaveBeenCalledTimes(1));
    expect(updateServer.mock.calls[0][1]).not.toHaveProperty('managed_runtime_only');
  });

  // Authorization, pinned both ways. UpdateServer runs authorizeServer (system
  // scope OR a server owner OR an admin-group manager) and then adds exactly
  // ONE field-level gate: `req.OwnerIDs != nil && !isAdmin(principal)` ->
  // ErrServerForbidden. ManagedRuntimeOnly has no such gate, and the HTTP layer
  // requires only scopeGatewayUse -- so a server OWNER may flip it, and gating
  // this control on isAdmin the way the owners field is gated would hide a
  // control the backend accepts.
  it('offers the control to a non-admin server owner, who gets no owners field', async () => {
    const server = { ...makeServer('srv-own', 'healthy'), managed_runtime_only: true };
    const updateServer = echoUpdate(server);
    renderForm(managedApi({ updateServer }), [server], 'user');
    openEditForm();
    // Present and correctly seeded for role="user"...
    const box = screen.getByRole('checkbox', { name: t.serverManagedRuntimeOnlyLabel });
    expect(box).toBeChecked();
    // ...while the owners field -- the one that DOES have a backend isAdmin
    // gate -- is absent. The two are deliberately not gated alike.
    expect(screen.queryByLabelText(t.serverOwnersLabel)).not.toBeInTheDocument();
    fireEvent.click(box);
    fireEvent.click(screen.getByRole('button', { name: t.serverActionSave }));
    await waitFor(() => expect(updateServer).toHaveBeenCalledTimes(1));
    const body = updateServer.mock.calls[0][1];
    expect(body.managed_runtime_only).toBe(false);
    expect(body).not.toHaveProperty('owner_ids');
  });

  it('offers the control to an admin as well', async () => {
    renderForm(managedApi(), [makeServer('srv-adm', 'healthy')], 'admin');
    openEditForm();
    expect(
      screen.getByRole('checkbox', { name: t.serverManagedRuntimeOnlyLabel }),
    ).toBeInTheDocument();
  });
});
