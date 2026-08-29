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
import { PortalApiError } from '../api/transport';
import type { PortalApi } from './shared/types';
import { messages } from '../i18n';
import type {
  BenchmarkStatus,
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
  RuntimeLogBatch,
  RuntimeLogState,
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
    set_visible_devices: false,
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
// An idle benchmark status. The model-mapping tab's edit mask polls the
// server's active runs to gate its context-size probe button.
const idleBenchmark: BenchmarkStatus = {
  running: false,
  server_id: 'srv_1',
  scope: 'application',
  total: 0,
  done: 0,
};

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
    // The owning application, for the few tests that need a field of it other
    // than the module default -- e.g. a non-empty `context_probe_path`, which
    // is what enables the shared mask's context-probe button.
    application?: PortalApplication;
    // The three calls `MappingForm`'s context probe makes, overridable exactly
    // as MappingSection's own tests override them.
    activeBenchmarks?: PortalApi['activeBenchmarks'];
    benchmarkStatus?: PortalApi['benchmarkStatus'];
    probeMappingContext?: PortalApi['probeMappingContext'];
    warnings?: string[];
    coresidencyPairs?: [string, string][];
    // Never resolves -- simulates the GET still being in flight, for the
    // "must not write before the fetch settles" regression tests.
    coresidencyPending?: boolean;
    gpuBudgets?: GPUBudget[];
    gpuBudgetsPending?: boolean;
    // Task 22b / C1: the two OTHER failure shapes, for the three resources
    // that only ever had "pending". `*Failing` rejects the FIRST call (a hard
    // failure with nothing in hand, `error`); `*FailsOnCall` rejects the
    // given 1-based call so an earlier one can succeed first, which is the
    // failed-RELOAD-over-an-existing-payload state (`stale-error`). Both are
    // the same options the report resource already had.
    mappingsFailing?: boolean;
    // Fix round 1, C4: the mappings resource was the one of the three that
    // gained no `*FailsOnCall`, so its `stale-error` state -- the only one that
    // still renders rows and override actions -- had no test at all.
    mappingsFailsOnCall?: number;
    // Fix round 1, M8: the fourth resource. Same two shapes as the others.
    warningsFailing?: boolean;
    warningsFailsOnCall?: number;
    coresidencyFailing?: boolean;
    coresidencyFailsOnCall?: number;
    gpuBudgetsFailing?: boolean;
    gpuBudgetsFailsOnCall?: number;
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
    // still holds an earlier payload (`useResource` never clears it). Reached
    // in place by the LANGUAGE switch (see `rerenderWithLocale`); `server.id`
    // is a loader dep too, but a real server switch remounts, so that is the
    // defensive case. 1-based call index.
    reportFailsOnCall?: number;
    // Pushed synchronously from inside the subscribe call, i.e. exactly like
    // the stream's `snapshot` frame arriving on connect.
    statusRows?: RuntimeStatus[];
  } = {},
) {
  const mappings = opts.mappings ?? [];
  const specsByMappingId = opts.specsByMappingId ?? {};
  // One value for the initial render AND both rerender helpers: a helper that
  // silently swapped the application back to the module default would make a
  // mid-flow rerender change a fact the test did not mean to change.
  const applicationForTest = opts.application ?? application;
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
  const logSubscriptions: string[] = [];
  let logUnsubscribeCount = 0;
  let reportCalls = 0;
  let mappingsCalls = 0;
  let coresidencyCalls = 0;
  let gpuBudgetsCalls = 0;
  let warningsCalls = 0;
  const subscribedServerIds: string[] = [];

  const fakeApi = {
    mappings: vi.fn(() => {
      mappingsCalls += 1;
      if (
        (opts.mappingsFailing && mappingsCalls === 1) ||
        opts.mappingsFailsOnCall === mappingsCalls
      ) {
        return Promise.reject(new Error('mappings unavailable'));
      }
      return Promise.resolve({ data: mappings });
    }),
    createMapping: vi.fn(async (_aid: string, body: CreateMappingRequest) => {
      created.push(body);
      return makeMapping({ id: 'map_created', ...(body as Partial<PortalModelMapping>) });
    }),
    updateMapping: vi.fn(async (id: string, body: UpdateMappingRequest) => {
      updatedMappings.push({ id, body });
      // Models the real PATCH: every field is a pointer, so an ABSENT key
      // leaves the stored value alone. Merging onto makeMapping()'s DEFAULTS
      // instead made an omitted `status` come back as 'active' -- the fake
      // itself performing the silent re-enable this screen exists to prevent,
      // and hiding it from the test that asks about it.
      const current = mappings.find((m) => m.id === id) ?? makeMapping({ id });
      return { ...current, id, ...(body as Partial<PortalModelMapping>) };
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
    runtimeCoresidency: vi.fn(() => {
      coresidencyCalls += 1;
      if (opts.coresidencyPending) return new Promise<{ pairs: [string, string][] }>(() => {});
      if (
        (opts.coresidencyFailing && coresidencyCalls === 1) ||
        opts.coresidencyFailsOnCall === coresidencyCalls
      ) {
        return Promise.reject(new Error('coresidency unavailable'));
      }
      return Promise.resolve({ pairs: opts.coresidencyPairs ?? [] });
    }),
    putRuntimeCoresidency: vi.fn(async (_appId: string, body: { pairs: [string, string][] }) => {
      putCoresidencyBodies.push(body.pairs);
      return { pairs: body.pairs };
    }),
    runtimeWarnings: vi.fn(() => {
      warningsCalls += 1;
      if (
        (opts.warningsFailing && warningsCalls === 1) ||
        opts.warningsFailsOnCall === warningsCalls
      ) {
        return Promise.reject(new Error('warnings unavailable'));
      }
      return Promise.resolve({ warnings: opts.warnings ?? [] });
    }),
    gpuBudgets: vi.fn(() => {
      gpuBudgetsCalls += 1;
      if (opts.gpuBudgetsPending) return new Promise<{ budgets: GPUBudget[] }>(() => {});
      if (
        (opts.gpuBudgetsFailing && gpuBudgetsCalls === 1) ||
        opts.gpuBudgetsFailsOnCall === gpuBudgetsCalls
      ) {
        return Promise.reject(new Error('budgets unavailable'));
      }
      // A FRESH array per call, like a real JSON response: `setData` with the
      // identical object would make React bail out of the render and the
      // re-seed effect would never re-run, which is not what a re-GET does.
      return Promise.resolve({ budgets: (opts.gpuBudgets ?? []).map((b) => ({ ...b })) });
    }),
    putGpuBudgets: vi.fn(async (_serverId: string, body: { budgets: GPUBudget[] }) => {
      putBudgets.push(body.budgets);
      // The authoritative post-save list is NOT the request body echoed back:
      // expected_uuid/expected_name are a drift detector the BACKEND snapshots
      // from telemetry the first time a budget row is created, so a row saved
      // without them comes back carrying them. Echoing the body verbatim made
      // "kept the draft" and "re-seeded from the server's answer" produce
      // byte-identical DOM -- which is why the M9 re-seed test could not fail
      // when the dirty-flag reset its own comment guards was deleted.
      return {
        budgets: body.budgets.map((b) => ({
          ...b,
          expected_uuid: b.expected_uuid || `GPU-${b.index}-UUID`,
          expected_name: b.expected_name || `card-at-${b.index}`,
        })),
      };
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
    // T3's per-row log view. Recorded rather than inert: subscribing is what
    // makes the agent stream, so a test that wants to prove a view was (or was
    // not) opened has to be able to see it.
    subscribeRuntimeLogs: vi.fn(
      (
        _serverId: string,
        specId: string,
        _onBatch: (batch: RuntimeLogBatch) => void,
        _onState: (state: RuntimeLogState) => void,
      ) => {
        logSubscriptions.push(specId);
        return () => {
          logUnsubscribeCount++;
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
    // The model-mapping tab reuses `MappingForm`, whose context-size probe polls
    // the server's running benchmarks to gate its button. `activeBenchmarks`
    // must RESOLVE (to an empty list) rather than be absent: a rejection lands
    // in the poll's own catch and leaves the button in the wrong state.
    activeBenchmarks: opts.activeBenchmarks ?? vi.fn(async () => []),
    benchmarkStatus: opts.benchmarkStatus ?? vi.fn(async () => idleBenchmark),
    probeMappingContext: opts.probeMappingContext ?? vi.fn(async () => idleBenchmark),
  };

  const view = render(
    <ToastProvider>
      <RuntimeAdminSection
        t={t}
        api={fakeApi}
        server={serverForTest}
        application={applicationForTest}
        // Drives the probe's status poll immediately, like MappingSection's own
        // tests. Without it forwarded, the shared mask's one async loop is
        // testable on the ordinary screen and not on this tab.
        pollIntervalMs={0}
      />
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
    // T3: which specs a log view was opened for, and how many were closed
    // again. Closing is what tells the agent to stop streaming, so both halves
    // are observable.
    logs: {
      subscribedSpecIds: logSubscriptions,
      unsubscribes: () => logUnsubscribeCount,
    },
    /**
     * How many times the mapping list was GET. The model-mapping tab and the
     * three tabs beside it read ONE copy of these rows, so a write on the tab
     * must be visible everywhere WITHOUT a refetch -- that is the property
     * this counter pins.
     */
    mappingsCallCount: () => mappingsCalls,
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
            application={applicationForTest}
          />
        </ToastProvider>,
      ),
    /**
     * Re-renders with another locale's translation table, i.e. the language
     * menu being used (App.tsx holds `locale` in state and reads
     * `messages[locale]`, so the tree re-renders rather than remounting).
     *
     * That is the deps change that reaches `stale-error` on this screen while
     * the component STAYS MOUNTED: `t` is a dep of every `useResource` here,
     * so a switch re-runs all of them, and `useResource` leaves the previous
     * payload in `data` if the re-run fails. `server.id` is a dep too, but a
     * real server switch remounts (`ApplicationSection` is rendered with
     * `key={`app-${server.id}`}` in ServerList.tsx), so the language switch is
     * the reachable one.
     */
    rerenderWithLocale: (next: typeof t) =>
      view.rerender(
        <ToastProvider>
          <RuntimeAdminSection
            t={next}
            api={fakeApi}
            server={serverForTest}
            application={applicationForTest}
          />
        </ToastProvider>,
      ),
  };
}

/**
 * Waits for the button called `name` to be present AND ENABLED — the
 * synchronisation point that an accessible name alone is not.
 *
 * Two controls on this screen are gated on a mapping's LAZY per-row spec GET
 * (`useEffect` over `mappings` → `api.runtimeSpec`), and both look ready
 * before it lands:
 *
 *  - The row's single **Delete**. `rowActions` labels it
 *    `meaning === 'mapping' ? t.mappingDelete : t.runtimeSpecDelete`, so the
 *    pre-GET `'unknown'` state carries the SAME name as the settled `'spec'`
 *    state — and is `disabled` (`rowBusy || meaning === 'unknown'`). A query by
 *    name therefore MATCHES the disabled control (Testing Library's role
 *    queries do not filter on `disabled`), and React drops `onClick` on a
 *    disabled `<button>`: the click is swallowed and no confirm dialog opens.
 *  - The live-status row's **override actions** (Force stop / Force start /
 *    Clear override / Restart). `statusActions` looks the row up in
 *    `specByRuntimeId`, which is built from `specsById`, and returns only the
 *    log view when there is no entry — so here the control is ABSENT rather
 *    than disabled, and a synchronous query fails with "Unable to find". They
 *    can additionally be disabled by `overridesLocked`.
 *
 * `toBeEnabled()` covers both shapes: `getByRole` inside `waitFor` retries
 * through the absent phase, and the matcher rejects the disabled phase.
 *
 * Returns nothing on purpose, so the caller cannot keep what it waited on:
 * `IconAction` wraps only the disabled variant in a `<span>` (to hang the
 * tooltip on a non-interactive control), so the button's DOM node is REPLACED
 * when the read lands. Every caller re-queries.
 *
 * Not a substitute for `findByRole('dialog')`: an enabled click yields its
 * dialog synchronously inside `fireEvent`'s `act()` (measured), so the
 * synchronous dialog queries in this file are correct as they stand and
 * awaiting them would only trade an instant error for a timeout.
 *
 * Cannot be used inside `vi.useFakeTimers()` — see the C1 test below for why.
 */
async function waitForEnabledButton(name: string): Promise<void> {
  await waitFor(() => expect(screen.getByRole('button', { name })).toBeEnabled());
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
    // Scoped to the tab role like `runtimeSpecs` above, because the specs
    // table's live-state column now carries this label too and a bare text
    // query would match twice. NOT coverage for that relabel: this reads the
    // TAB's label, which the relabel did not touch, and it passes whichever
    // label the column carries. The relabel is pinned by "labels the two tabs'
    // status columns with two different words" below.
    expect(screen.getByRole('tab', { name: t.runtimeLiveStatus })).toBeInTheDocument();

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
    fireEvent.click(screen.getByRole('tab', { name: t.runtimeLiveStatus }));
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

  // The binary-path/OS mismatch advisory: a spec's path is absolute for the
  // other platform than the one this server's agent reports. It rides the same
  // opaque-code channel as the timeout warning, so all that is pinned here is
  // that the code maps to a label instead of falling through to its raw wire
  // string -- and that two codes render as two banners.
  it('shows the binary-path/OS mismatch warning banner, alongside the timeout one', async () => {
    renderSection({ warnings: ['binary_path_os_mismatch'] });
    expect(await screen.findByText(t.runtimeBinaryPathOsMismatchWarning)).toBeInTheDocument();
    expect(screen.queryByText('binary_path_os_mismatch')).not.toBeInTheDocument();

    cleanup();
    renderSection({ warnings: ['timeout_ms_below_startup_timeout', 'binary_path_os_mismatch'] });
    expect(await screen.findByText(t.runtimeTimeoutWarning)).toBeInTheDocument();
    expect(screen.getByText(t.runtimeBinaryPathOsMismatchWarning)).toBeInTheDocument();
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

  // The agent reserves six base-environment names, not two, and folds case
  // before comparing -- Windows resolves environment names case-insensitively,
  // so `Path` and `SystemRoot` (the only spellings a Windows operator types)
  // are the same variables. A mirror that knew only exact `PATH`/`HOME` let
  // those save and fail later at process start as `not_permitted`.
  it.each(['Path', 'SystemRoot', 'userprofile', 'LOCALAPPDATA', 'windir', 'hOmE'])(
    'rejects the reserved env key %s in the spelling an operator would type',
    async (key) => {
      const { created, putSpecs } = renderSection();
      fireEvent.click(await screen.findByRole('button', { name: t.runtimeSpecCreate }));
      fireEvent.change(screen.getByLabelText(t.mappingGatewayName), { target: { value: 'gw' } });
      fireEvent.change(screen.getByLabelText(t.mappingAppName), { target: { value: 'app' } });
      fireEvent.change(screen.getByLabelText(t.runtimeSpecBinary), {
        target: { value: '/usr/bin/llama-server' },
      });
      fireEvent.change(screen.getByLabelText(t.runtimeSpecEnv), {
        target: { value: `${key}=C:\\attacker\\bin` },
      });

      fireEvent.click(screen.getByRole('button', { name: t.runtimeSpecCreate }));

      expect(await screen.findByText(t.runtimeSpecEnvReserved)).toBeInTheDocument();
      expect(created).toHaveLength(0);
      expect(putSpecs).toHaveLength(0);
    },
  );

  // The other half of the same rule: the variables deliberately left OUT of
  // the agent's base stay settable, because they are the operator's only lever
  // for redirecting a child's home or cache once HOME is reserved. ${MODEL}
  // rides along here since it must reach the agent untouched.
  it('accepts the cache/home redirection keys the agent deliberately does not reserve', async () => {
    const { putSpecs } = renderSection();
    fireEvent.click(await screen.findByRole('button', { name: t.runtimeSpecCreate }));
    fireEvent.change(screen.getByLabelText(t.mappingGatewayName), { target: { value: 'gw' } });
    fireEvent.change(screen.getByLabelText(t.mappingAppName), { target: { value: 'app' } });
    fireEvent.change(screen.getByLabelText(t.runtimeSpecBinary), {
      target: { value: '/usr/bin/llama-server' },
    });
    fireEvent.change(screen.getByLabelText(t.runtimeSpecArgs), {
      target: { value: '--alias\n${MODEL}' },
    });
    fireEvent.change(screen.getByLabelText(t.runtimeSpecEnv), {
      target: { value: 'HF_HOME=D:\\models\\hf\nTEMP=D:\\models\\tmp\nMODEL_TAG=${MODEL}' },
    });

    fireEvent.click(screen.getByRole('button', { name: t.runtimeSpecCreate }));

    await waitFor(() => expect(putSpecs).toHaveLength(1));
    expect(putSpecs[0].body.args).toEqual(['--alias', '${MODEL}']);
    expect(putSpecs[0].body.env).toEqual({
      HF_HOME: 'D:\\models\\hf',
      TEMP: 'D:\\models\\tmp',
      MODEL_TAG: '${MODEL}',
    });
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
      subscribeRuntimeLogs: vi.fn(() => () => {}),
      updateServer: vi.fn(async (id: string, body: UpdateServerRequest) => ({
        ...server,
        id,
        runtime_max_processes: body.runtime_max_processes,
      })),
      serverHardware: vi.fn(async () => ({ available: false }) as HardwareResponse),
      activeBenchmarks: vi.fn(async () => []),
      benchmarkStatus: vi.fn(async () => idleBenchmark),
      probeMappingContext: vi.fn(async () => idleBenchmark),
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
    // `findByText` answers for the mappings list, not for this row's lazy spec
    // GET, and awaiting the Delete by NAME does not answer for it either -- the
    // shared label matches the disabled 'unknown' state too, and the click is
    // then swallowed (see `waitForEnabledButton`). Seen in CI as an instant
    // `role "dialog"` failure.
    await waitForEnabledButton(t.runtimeSpecDelete);
    fireEvent.click(screen.getByRole('button', { name: t.runtimeSpecDelete }));
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

// The arguments field's contract -- ONE argument per line, a flag and its
// value on two lines -- used to be stated nowhere at all: its label read
// "Arguments" and nothing else. An operator pasted a whole llama-server
// command line onto one line, the parser (correctly) made that one argv
// element, and the first explanation they got was llama-server's own
// `error: invalid argument: --port 50395 --mmproj C:\... -m C:\... --temp 1.0`
// -- a foreign program's rejection that cannot possibly explain OUR rule.
//
// These tests pin the three signals that replace that experience, and, just as
// importantly, pin what they must NOT fire on: the detection separates "looks
// like a whole command line" from "contains a space", and only the first is
// safe to act on, because a legitimate argument value contains spaces all the
// time (a Windows path, a chat template, a prompt fragment).
describe('RuntimeAdminSection arguments field contract (hint + warnings)', () => {
  async function openCreateForm() {
    fireEvent.click(await screen.findByRole('button', { name: t.runtimeSpecCreate }));
  }

  function setArgs(value: string) {
    fireEvent.change(screen.getByLabelText(t.runtimeSpecArgs), { target: { value } });
  }

  // The example is multi-line by nature, and testing-library's default
  // normalizer collapses whitespace -- which would erase exactly the property
  // under test. Compare the text node verbatim instead.
  const verbatim = { normalizer: (s: string) => s };

  it('states the one-token-per-line rule under the field and SHOWS it as an example', async () => {
    renderSection();
    await openCreateForm();

    expect(screen.getByText(t.runtimeSpecArgsHint)).toBeInTheDocument();
    // The example is the load-bearing half: a flag and its value on separate
    // lines, ${PORT} rather than a number, and a path whose internal spaces do
    // NOT split it.
    const example = screen.getByText(t.runtimeSpecArgsExample, verbatim);
    expect(example).toBeInTheDocument();
    expect(example.textContent).toBe(
      '--port\n${PORT}\n-m\nC:\\Program Files\\models\\Qwen3-27B-Q4.gguf',
    );
  });

  it('warns when a whole command line is pasted onto one line, naming the line', async () => {
    renderSection();
    await openCreateForm();
    setArgs(
      '--port 50395 --mmproj C:\\models\\mmproj-BF16.gguf -m C:\\models\\Qwen3.8-27B-UD-Q4-K-XL.gguf --temp 1.0',
    );

    const warning = await screen.findByText(t.runtimeSpecArgsCommandLine, { exact: false });
    expect(warning.textContent).toContain('--port 50395 --mmproj');
  });

  it('warns rather than refuses: the pasted line still saves if the operator insists', async () => {
    const { created, putSpecs } = renderSection();
    await openCreateForm();
    fireEvent.change(screen.getByLabelText(t.mappingGatewayName), { target: { value: 'gw' } });
    fireEvent.change(screen.getByLabelText(t.mappingAppName), { target: { value: 'app' } });
    fireEvent.change(screen.getByLabelText(t.runtimeSpecBinary), {
      target: { value: '/usr/bin/llama-server' },
    });
    setArgs('--port 50395 --mmproj C:\\models\\mmproj-BF16.gguf');
    expect(
      await screen.findByText(t.runtimeSpecArgsCommandLine, { exact: false }),
    ).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: t.runtimeSpecCreate }));

    await waitFor(() => expect(putSpecs).toHaveLength(1));
    expect(created).toHaveLength(1);
    expect(putSpecs[0].body.args).toEqual(['--port 50395 --mmproj C:\\models\\mmproj-BF16.gguf']);
  });

  it('does NOT warn about a Windows model path whose spaces are part of the value', async () => {
    renderSection();
    await openCreateForm();
    setArgs('-m\nC:\\Program Files\\models\\Qwen3 27B UD-Q4-K-XL.gguf');

    expect(screen.queryByText(t.runtimeSpecArgsCommandLine, { exact: false })).toBeNull();
  });

  it('does NOT warn about a chat template full of dash-prefixed tokens', async () => {
    renderSection();
    await openCreateForm();
    // Jinja whitespace-control markers: `-%}` twice over, plus a leading `{%-`.
    // A naive "more than one token starting with a dash" rule fires on this;
    // requiring a LETTER after the dashes is what keeps it off correct input.
    setArgs('--chat-template\n{%- for m in messages -%}{{ m.content }}{%- endfor -%}');

    expect(screen.queryByText(t.runtimeSpecArgsCommandLine, { exact: false })).toBeNull();
  });

  it('warns about a hard-coded --port while the agent is the one assigning it', async () => {
    renderSection();
    await openCreateForm();
    setArgs('--port\n50395');

    const warning = await screen.findByText(t.runtimeSpecArgsHardcodedPort, { exact: false });
    expect(warning.textContent).toContain('50395');
  });

  it('does NOT warn when the port argument is ${PORT}', async () => {
    renderSection();
    await openCreateForm();
    setArgs('--port\n${PORT}');

    expect(screen.queryByText(t.runtimeSpecArgsHardcodedPort, { exact: false })).toBeNull();
  });

  it('does NOT warn about literal numbers in unrelated arguments', async () => {
    renderSection();
    await openCreateForm();
    // Every one of these is a bare in-range number: the NUMBER carries no
    // signal, only a port-naming flag in front of it does.
    setArgs('--ctx-size\n32768\n-ngl\n99\n--threads\n8080\n--port\n${PORT}');

    expect(screen.queryByText(t.runtimeSpecArgsHardcodedPort, { exact: false })).toBeNull();
  });

  it('does NOT warn about a literal --port once the listen port is pinned to it', async () => {
    renderSection();
    await openCreateForm();
    setArgs('--port\n50395');
    expect(
      await screen.findByText(t.runtimeSpecArgsHardcodedPort, { exact: false }),
    ).toBeInTheDocument();

    fireEvent.change(screen.getByLabelText(t.runtimeSpecListenPort), {
      target: { value: '50395' },
    });

    expect(screen.queryByText(t.runtimeSpecArgsHardcodedPort, { exact: false })).toBeNull();
  });

  // Round 2, same operator: with the arguments split correctly, the next
  // failure was `error while handling argument "--chat-template-file": error:
  // failed to open file 'C:\llama-swap\chat-template.jinja'` -- a trailing
  // space left behind by the paste, invisible in the field, indistinguishable
  // from a missing file.
  it('makes an invisible trailing space visible, naming the line and marking the space', async () => {
    renderSection();
    await openCreateForm();
    setArgs('--chat-template-file\nC:\\llama-swap\\chat-template.jinja ');

    const warning = await screen.findByText(t.runtimeSpecArgsEdgeWhitespace, { exact: false });
    const detail = screen.getByText('2: C:\\llama-swap\\chat-template.jinja·', verbatim);
    expect(warning).toBeInTheDocument();
    expect(detail).toBeInTheDocument();
  });

  it('flags leading whitespace too, on the line it is actually on', async () => {
    renderSection();
    await openCreateForm();
    setArgs('--alias\n  qwen3-27b');

    expect(
      await screen.findByText(t.runtimeSpecArgsEdgeWhitespace, { exact: false }),
    ).toBeInTheDocument();
    expect(screen.getByText('2: ··qwen3-27b', verbatim)).toBeInTheDocument();
  });

  it('reports a whitespace-only line as its own case, not as an edge-space one', async () => {
    renderSection();
    await openCreateForm();
    setArgs('--alias\n \nqwen3-27b');

    expect(
      await screen.findByText(t.runtimeSpecArgsBlankLine, { exact: false }),
    ).toBeInTheDocument();
    expect(screen.getByText('2: ·', verbatim)).toBeInTheDocument();
    expect(screen.queryByText(t.runtimeSpecArgsEdgeWhitespace, { exact: false })).toBeNull();
  });

  it('flags meaningful trailing whitespace but never rewrites or blocks it', async () => {
    const { putSpecs } = renderSection();
    await openCreateForm();
    fireEvent.change(screen.getByLabelText(t.mappingGatewayName), { target: { value: 'gw' } });
    fireEvent.change(screen.getByLabelText(t.mappingAppName), { target: { value: 'app' } });
    fireEvent.change(screen.getByLabelText(t.runtimeSpecBinary), {
      target: { value: '/usr/bin/llama-server' },
    });
    // A reverse-prompt whose trailing space is the whole point of the value.
    setArgs('--reverse-prompt\nUser: ');
    expect(
      await screen.findByText(t.runtimeSpecArgsEdgeWhitespace, { exact: false }),
    ).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: t.runtimeSpecCreate }));

    await waitFor(() => expect(putSpecs).toHaveLength(1));
    expect(putSpecs[0].body.args).toEqual(['--reverse-prompt', 'User: ']);
  });

  it('says nothing at all about a correctly written argument list', async () => {
    renderSection();
    await openCreateForm();
    setArgs('--port\n${PORT}\n-m\nC:\\Program Files\\models\\Qwen3 27B.gguf\n--ctx-size\n32768');

    expect(screen.queryByText(t.runtimeSpecArgsCommandLine, { exact: false })).toBeNull();
    expect(screen.queryByText(t.runtimeSpecArgsHardcodedPort, { exact: false })).toBeNull();
    expect(screen.queryByText(t.runtimeSpecArgsEdgeWhitespace, { exact: false })).toBeNull();
    expect(screen.queryByText(t.runtimeSpecArgsBlankLine, { exact: false })).toBeNull();
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
      await screen.findByRole('checkbox', { name: `${t.runtimeMatrixCell}: Charlie + Alpha` }),
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
      await screen.findByRole('checkbox', { name: `${t.runtimeMatrixCell}: Bravo + Alpha` }),
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
      screen.queryByLabelText(`${t.runtimeMatrixCell}: Charlie + Alpha`),
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
    const firstCell = await screen.findByRole('checkbox', {
      name: `${t.runtimeMatrixCell}: Bravo + Alpha`,
    });
    fireEvent.click(firstCell);

    const secondCell = await screen.findByRole('checkbox', {
      name: `${t.runtimeMatrixCell}: Charlie + Bravo`,
    });
    await waitFor(() => expect(secondCell).toBeDisabled());
    fireEvent.click(secondCell); // disabled -- must be a no-op
    expect(fakeApi.putRuntimeCoresidency).toHaveBeenCalledTimes(1);
    // The busy window is short, but an unexplained dead grid is the same
    // defect as an unexplained read-only one -- and it must NOT reuse the
    // file-mode sentence, which would tell the operator the matrix is
    // permanently read-only when it is about to accept clicks again.
    fireEvent.mouseOver(secondCell);
    const busyTip = await screen.findByRole('tooltip');
    expect(busyTip).toHaveTextContent(t.runtimeMatrixDisabledSaving);
    expect(busyTip).not.toHaveTextContent(t.runtimeMatrixDisabledFileMode);
    fireEvent.mouseOut(secondCell);

    resolveFirst({ pairs: [['map_1', 'map_2']] });
    await waitFor(() => expect(secondCell).not.toBeDisabled());
    // ...and the reason goes away with it: a live cell must not claim it is
    // saving.
    fireEvent.mouseOver(secondCell);
    expect(await screen.findByRole('tooltip')).not.toHaveTextContent(t.runtimeMatrixDisabledSaving);
    fireEvent.mouseOut(secondCell);

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
    // NOT one of the visibility variables set_visible_devices owns: this
    // fixture also sets that option below, and the two together are exactly
    // the combination the form refuses (validateVisibleDevices) -- a fixture
    // that cannot be saved would make every save-path test below untestable
    // for the wrong reason.
    env: { NCCL_P2P_DISABLE: '1', HF_HOME: '/data/hf' },
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
    // Non-default on purpose: the override actions build their PUT body by
    // spreading the LOADED spec, so a `false` here could not tell a preserved
    // value from a defaulted one.
    set_visible_devices: true,
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
    set_visible_devices: spec.set_visible_devices,
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

  // T3, the log view's row action. Two properties, and the second is the one
  // that matters: the action exists on rows that get NO other action at all
  // (file mode, an unresolvable spec, a pre-settled report GET), because
  // reading what a process printed is not a write -- and those are exactly the
  // states an operator can be stuck in with no other way to find out what is
  // happening.
  it('offers the log view on every status row and opens a stream for that row only', async () => {
    const { logs, stream } = renderSection({
      statusRows: [
        makeStatus({ spec_id: 'spec_1', model: 'app-model', state: 'crashed' }),
        makeStatus({ spec_id: 'spec_2', model: 'other-model', state: 'running' }),
      ],
    });
    stream.setStatus('open');
    await openStatusTab();
    await screen.findByText('app-model');

    // Nothing is streamed before an operator asks: the subscription IS the
    // request, so merely rendering the tab must not start one.
    expect(logs.subscribedSpecIds).toEqual([]);

    fireEvent.click(inRowWith('app-model').getByRole('button', { name: t.runtimeLogs }));
    expect(logs.subscribedSpecIds).toEqual(['spec_1']);
    expect(await screen.findByText(t.runtimeLogsIntro)).toBeInTheDocument();

    // Closing it is what tells the agent to stop.
    fireEvent.click(screen.getByRole('button', { name: t.captureClose }));
    await waitFor(() => expect(logs.unsubscribes()).toBe(1));
  });

  it('offers the log view in file mode, where no override action exists at all', async () => {
    const { logs, stream } = renderSection({
      report: fileModeReport({ specs: [] }),
      statusRows: [makeStatus({ spec_id: 'spec_1', model: 'app-model' })],
    });
    stream.setStatus('open');
    await openStatusTab();
    await screen.findByText('app-model');

    expect(screen.queryByRole('button', { name: t.runtimeForceStart })).not.toBeInTheDocument();
    fireEvent.click(inRowWith('app-model').getByRole('button', { name: t.runtimeLogs }));
    expect(logs.subscribedSpecIds).toEqual(['spec_1']);
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
    const cell = await screen.findByRole('checkbox', {
      name: `${t.runtimeMatrixCell}: File-Bravo + File-Alpha`,
    });
    expect(cell).toBeDisabled();
    // ...and it says WHY it is disabled. "Read-only in file mode" and "pair
    // not allowed yet" are different facts; a greyed cell with no explanation
    // leaves the operator to guess which one they are looking at.
    fireEvent.mouseOver(cell);
    expect(await screen.findByRole('tooltip')).toHaveTextContent(t.runtimeMatrixDisabledFileMode);
    fireEvent.mouseOut(cell);

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
    const cell = await screen.findByRole('checkbox', {
      name: `${t.runtimeMatrixCell}: File-Bravo + File-Alpha`,
    });
    // The reported coresident pair renders as set.
    expect(cell).toBeChecked();

    fireEvent.click(screen.getByRole('tab', { name: t.runtimeLimits }));
    expect(await screen.findByText('3')).toBeInTheDocument();
    expect(screen.getByText('46000', { exact: false })).toBeInTheDocument();
  });

  // Correction 5: parse_error is the agent saying it could not read its own
  // config file. In that state `config` is unusable, so it must not be shown.
  //
  // C2 fix round: the field carries a CODE from a closed set
  // (`json_syntax`, `duplicate_spec_id`, `file_missing`, `read_failed`, and
  // the agent's `unclassified` floor), never free text -- so the operator must
  // be shown a sentence, not the identifier. The gating stays truthiness-based,
  // which is why the unknown-code case below still suppresses the config
  // exactly as a known one does.
  it('surfaces parse_error as a sentence, not a code, and stops rendering config', async () => {
    renderFileMode(fileConfig, { parse_error: 'json_syntax' });
    expect(
      await screen.findByText(`${t.runtimeParseError} ${t.runtimeParseErrorJsonSyntax}`),
    ).toBeInTheDocument();
    // The raw code never reaches the operator.
    expect(screen.queryByText('json_syntax', { exact: false })).not.toBeInTheDocument();
    expect(screen.queryByText('File-Alpha')).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole('tab', { name: t.runtimeMatrix }));
    await waitFor(() =>
      expect(
        screen.queryByLabelText(`${t.runtimeMatrixCell}: File-Bravo + File-Alpha`),
      ).not.toBeInTheDocument(),
    );
  });

  it('maps the duplicate_spec_id code to its own sentence', async () => {
    renderFileMode(fileConfig, { parse_error: 'duplicate_spec_id' });
    expect(
      await screen.findByText(`${t.runtimeParseError} ${t.runtimeParseErrorDuplicateSpecId}`),
    ).toBeInTheDocument();
    expect(screen.queryByText('duplicate_spec_id', { exact: false })).not.toBeInTheDocument();
  });

  // A1: the two ACCESS codes. They are the reason this whole mechanism was
  // reachable-but-unreachable for the most ordinary file-mode failure there
  // is -- the agent swallowed a missing or unreadable runtime.json entirely,
  // so the operator got the "nothing configured" screen with no hint that a
  // file was supposed to be there. Each must render its OWN sentence: the
  // shared fallback would put "the file is gone" and "the file's permissions
  // are wrong" behind the same words, which is the distinction the operator
  // is here for.
  it.each([
    ['file_missing', 'runtimeParseErrorFileMissing'],
    ['read_failed', 'runtimeParseErrorReadFailed'],
  ] as const)('maps the %s access code to its own sentence', async (code, key) => {
    renderFileMode(fileConfig, { parse_error: code });
    expect(await screen.findByText(`${t.runtimeParseError} ${t[key]}`)).toBeInTheDocument();
    // Neither the raw code nor the generic fallback reaches the operator.
    expect(screen.queryByText(code, { exact: false })).not.toBeInTheDocument();
    expect(
      screen.queryByText(t.runtimeParseErrorUnknown, { exact: false }),
    ).not.toBeInTheDocument();
    // The config is unusable in this state for the same reason a parse
    // failure makes it unusable, so the same suppression applies.
    expect(screen.queryByText('File-Alpha')).not.toBeInTheDocument();
  });

  // A code this build does not know: the agent's `unclassified` floor, a code
  // added on the agent side ahead of the portal, or the gateway's own generic
  // constant for a value outside its allow-list. All three must degrade to
  // one honest sentence rather than printing an identifier -- and must still
  // suppress the unusable config, which is why the gate is truthiness and not
  // a code lookup.
  it('falls back to the unknown-reason sentence for a code it does not recognise', async () => {
    renderFileMode(fileConfig, { parse_error: 'config parse error' });
    expect(
      await screen.findByText(`${t.runtimeParseError} ${t.runtimeParseErrorUnknown}`),
    ).toBeInTheDocument();
    expect(screen.queryByText('File-Alpha')).not.toBeInTheDocument();
  });

  // The HEALTHY file-mode path, which four rounds of review never asked
  // about: `parse_error` is `omitempty`, so a healthy agent sends no field at
  // all. The gateway used to fabricate one here (its redaction had no
  // empty-input case), which meant every healthy file-mode server showed the
  // parse-error banner with its config view suppressed, permanently. Fixed on
  // the backend; this pins the frontend half of the same contract.
  it('shows no parse-error banner at all when the agent reports none', async () => {
    renderFileMode();
    expect(await screen.findByText(t.runtimeManagedLocally)).toBeInTheDocument();
    expect(screen.queryByText(t.runtimeParseError, { exact: false })).not.toBeInTheDocument();
    expect(screen.queryByText(t.runtimeConfigUnavailable)).not.toBeInTheDocument();
    // ... and the config it could not have parsed is on screen.
    expect(screen.getByText('File-Alpha')).toBeInTheDocument();
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
  // `ready`. The stale payload must not decide whether this screen is writable.
  //
  // Driven here by a `server.id` change WITHOUT a remount, which is the
  // defensive case: task 22b established that a real server switch remounts
  // this component (`ServerList.tsx` keys `ApplicationSection` by server id),
  // so the trigger that reaches this state in place is the language switch --
  // exercised by the co-residency and GPU-budget siblings below. The name and
  // this comment used to assert the server switch as the reachable trigger,
  // which the correction had left standing (fix round 1, M10).
  it('treats a report GET that fails on a re-fetch as a failure, not as the last known mode', async () => {
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
  // APPLICATION-scoped, so a row can arrive that this application's mappings
  // cannot resolve: a spec deleted here while the agent still reports it until
  // its next config sync, or a snapshot landing before every per-mapping spec
  // GET has settled. (A second `server_agent` application on one server is no
  // longer among the causes -- the portal refuses it on create and on retype,
  // and migration 68 indexes it; see the fallback's own comment in
  // RuntimeAdminSection.tsx.) An empty actions cell with no explanation would
  // make the override feature look broken.
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

// ===========================================================================
// Task 22b, Batch C -- the deferred items.
// ===========================================================================

// C1: the co-residency resource had only "pending". A permanently failed pairs
// GET left the tab saying "Laden…" for as long as the page stayed open, with
// no retry (the resource's `reload()` was not even destructured) and no way to
// tell it from a slow one.
describe('RuntimeAdminSection failing co-residency GET (task 22b, C1)', () => {
  function threeMappings() {
    return [
      makeMapping({ id: 'map_1', gateway_model_name: 'Alpha' }),
      makeMapping({ id: 'map_2', gateway_model_name: 'Bravo' }),
      makeMapping({ id: 'map_3', gateway_model_name: 'Charlie' }),
    ];
  }

  it('names the failure, writes nothing, and offers a retry that recovers', async () => {
    const { fakeApi } = renderSection({
      mappings: threeMappings(),
      coresidencyFailing: true,
    });

    fireEvent.click(await screen.findByText(t.runtimeMatrix));
    expect(await screen.findByText(t.runtimeCoresidencyUnavailable)).toBeInTheDocument();
    // Not "still loading" -- that was the whole defect.
    expect(screen.queryByText(t.loading)).not.toBeInTheDocument();
    // And the write gating is untouched: nothing clickable, so nothing can PUT
    // a full-replace list built from pairs we never loaded.
    expect(
      screen.queryByLabelText(`${t.runtimeMatrixCell}: Charlie + Alpha`),
    ).not.toBeInTheDocument();
    expect(fakeApi.putRuntimeCoresidency).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole('button', { name: t.resourceRetry }));
    await waitFor(() => expect(fakeApi.runtimeCoresidency).toHaveBeenCalledTimes(2));
    expect(
      await screen.findByRole('checkbox', { name: `${t.runtimeMatrixCell}: Charlie + Alpha` }),
    ).toBeInTheDocument();
    expect(screen.queryByText(t.runtimeCoresidencyUnavailable)).not.toBeInTheDocument();
  });

  // The OTHER failure shape, and the one that is a write hazard rather than
  // only a rendering one: `!loading && data !== null` reports a failed RELOAD
  // as ready, so the matrix stayed rendered and CLICKABLE off pairs we had
  // just failed to refresh -- and a toggle PUTs the complete replacement list,
  // silently deleting whatever another admin added meanwhile. Reachable with
  // the component mounted through the language switch, since `t` is a dep of
  // every loader here.
  it('stops accepting toggles when a RELOAD fails over an existing pair list', async () => {
    const en = messages.en;
    const { fakeApi, rerenderWithLocale } = renderSection({
      mappings: threeMappings(),
      coresidencyPairs: [['map_1', 'map_2']],
      coresidencyFailsOnCall: 2,
    });

    fireEvent.click(await screen.findByText(t.runtimeMatrix));
    // Loaded and live to begin with.
    expect(
      await screen.findByRole('checkbox', { name: `${t.runtimeMatrixCell}: Charlie + Alpha` }),
    ).toBeInTheDocument();

    rerenderWithLocale(en);
    await waitFor(() => expect(fakeApi.runtimeCoresidency).toHaveBeenCalledTimes(2));

    // The STALE text, not the hard-failure one (fix round 1, M7): the pairs
    // were loaded and are still in hand, so "could not be loaded" is false --
    // what failed is the refresh, and that is what hides the matrix.
    expect(await screen.findByText(en.runtimeCoresidencyStale)).toBeInTheDocument();
    expect(screen.queryByText(en.runtimeCoresidencyUnavailable)).not.toBeInTheDocument();
    expect(
      screen.queryByLabelText(`${en.runtimeMatrixCell}: Charlie + Alpha`),
    ).not.toBeInTheDocument();
    expect(fakeApi.putRuntimeCoresidency).not.toHaveBeenCalled();
  });
});

// C1: the GPU-budgets resource, same two shapes.
describe('RuntimeAdminSection failing GPU-budgets GET (task 22b, C1)', () => {
  it('names the failure, offers no Save, and offers a retry that recovers', async () => {
    const { fakeApi } = renderSection({ gpuBudgetsFailing: true });

    fireEvent.click(await screen.findByText(t.runtimeLimits));
    expect(await screen.findByText(t.runtimeBudgetsUnavailable)).toBeInTheDocument();
    expect(screen.queryByText(t.loading)).not.toBeInTheDocument();
    // Task 21's Critical stays closed: no form at all, so no write.
    expect(screen.queryByRole('button', { name: t.save })).not.toBeInTheDocument();
    expect(screen.queryByLabelText(t.runtimeMaxProcesses)).not.toBeInTheDocument();
    expect(fakeApi.putGpuBudgets).not.toHaveBeenCalled();
    expect(fakeApi.updateServer).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole('button', { name: t.resourceRetry }));
    await waitFor(() => expect(fakeApi.gpuBudgets).toHaveBeenCalledTimes(2));
    expect(await screen.findByLabelText(t.runtimeMaxProcesses)).toBeInTheDocument();
    expect(screen.queryByText(t.runtimeBudgetsUnavailable)).not.toBeInTheDocument();
  });

  // The write hazard, one degree worse than the co-residency one: on a failed
  // RELOAD `budgetRows` still holds the values from BEFORE it (the re-seed
  // effect returns early while `data` is unchanged), so the looser gate left a
  // fully populated form on screen whose Save would have PUT those values as
  // the COMPLETE replacement set, with the failure invisible.
  it('hides the Save form when a RELOAD fails over an existing budget list', async () => {
    const en = messages.en;
    const { fakeApi, rerenderWithLocale } = renderSection({
      gpuBudgets: [{ index: 0, budget_mb: 40000, expected_uuid: '', expected_name: '' }],
      gpuBudgetsFailsOnCall: 2,
      runtimeMaxProcesses: 2,
    });

    fireEvent.click(await screen.findByText(t.runtimeLimits));
    const budgetField = await screen.findByLabelText(t.runtimeGpuBudget);
    expect((budgetField as HTMLInputElement).value).toBe('40000');

    rerenderWithLocale(en);
    await waitFor(() => expect(fakeApi.gpuBudgets).toHaveBeenCalledTimes(2));

    // Same correction as the co-residency sibling (fix round 1, M7).
    expect(await screen.findByText(en.runtimeBudgetsStale)).toBeInTheDocument();
    expect(screen.queryByText(en.runtimeBudgetsUnavailable)).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: en.save })).not.toBeInTheDocument();
    expect(screen.queryByLabelText(en.runtimeGpuBudget)).not.toBeInTheDocument();
    expect(fakeApi.putGpuBudgets).not.toHaveBeenCalled();
  });
});

// C1 / task 22's N3: `specsSettled` required `mappingsData !== null`, so a
// failed mappings GET left it false forever -- every live-status row
// unresolvable, its actions cell blank AND its `Unmatched` chip suppressed,
// which is the silent blank the chip exists to prevent, reached through a
// different failed GET. The mappings feed all four tabs, so the state is named
// screen-wide.
describe('RuntimeAdminSection failing mappings GET (task 22b, C1 / N3)', () => {
  it('names the failure above the tab strip instead of blanking the status rows', async () => {
    const { fakeApi, stream } = renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      specsByMappingId: { map_1: fullSpec() },
      statusRows: [makeStatus({ spec_id: 'spec_1', state: 'running' })],
      mappingsFailing: true,
    });
    stream.setStatus('open');

    expect(await screen.findByText(t.runtimeMappingsUnavailable)).toBeInTheDocument();
    // The specs list no longer claims to be loading either.
    expect(screen.queryByText(t.loading)).not.toBeInTheDocument();

    // Still named on the status tab -- the tab where the consequence lands.
    await openStatusTab();
    expect(screen.getByText(t.runtimeMappingsUnavailable)).toBeInTheDocument();

    // The retry recovers the whole join: the row resolves to its gateway model
    // name and its overrides come back.
    fireEvent.click(screen.getByRole('button', { name: t.resourceRetry }));
    await waitFor(() => expect(fakeApi.mappings).toHaveBeenCalledTimes(2));
    expect(await screen.findByText('gw-model')).toBeInTheDocument();
    expect(screen.queryByText(t.runtimeMappingsUnavailable)).not.toBeInTheDocument();
    expect(await screen.findByRole('button', { name: t.runtimeForceStop })).toBeInTheDocument();
  });
});

// C7: the restart notices rendered inside the status tab only. The sequence is
// bounded at 120 s and every notice tells the operator to go and clear an
// override on ANOTHER tab, so switching tabs mid-sequence -- likely, not
// exotic -- made the one message you must not miss disappear.
describe('RuntimeAdminSection restart notices are screen-wide (task 22b, C7)', () => {
  it('keeps a restart timeout notice visible after switching tabs', async () => {
    vi.useFakeTimers();
    try {
      const { stream } = renderSection({
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
      // The awaited `stopped` never arrives.
      await act(async () => {
        vi.advanceTimersByTime(RESTART_STOP_TIMEOUT_MS + 1000);
        await Promise.resolve();
      });
      expect(screen.getByText(t.runtimeRestartTimeout)).toBeInTheDocument();

      // The notice says to check the "Runtime specs" tab -- so it has to still
      // be there once the operator does exactly that.
      fireEvent.click(screen.getByRole('tab', { name: t.runtimeSpecs }));
      expect(screen.getByText(t.runtimeRestartTimeout)).toBeInTheDocument();
      fireEvent.click(screen.getByRole('tab', { name: t.runtimeLimits }));
      expect(screen.getByText(t.runtimeRestartTimeout)).toBeInTheDocument();
    } finally {
      vi.useRealTimers();
    }
  });
});

// C6, first item: hoisting `setSpecsById` above the run token (fix round 2, N1)
// was right -- the bug it fixed was reachable on EVERY timeout -- but it left
// ARRIVAL ORDER deciding the cache when two writes to the same mapping
// overlap, because `RuntimeSpec` carries no version/etag.
describe('RuntimeAdminSection overlapping writes to one mapping (task 22b, C6)', () => {
  it('ignores an abandoned write that resolves after a later write to the same mapping', async () => {
    vi.useFakeTimers();
    try {
      let landFirst: (updated: RuntimeSpec) => void = () => {};
      const first = new Promise<RuntimeSpec>((resolve) => {
        landFirst = resolve;
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

      // Write A: force_stopped, and it hangs past its watchdog.
      fakeApi.putRuntimeSpec.mockImplementationOnce(() => first);
      fireEvent.click(screen.getByRole('button', { name: t.runtimeForceStop }));
      await act(async () => {
        await Promise.resolve();
      });
      await act(async () => {
        vi.advanceTimersByTime(OVERRIDE_WRITE_TIMEOUT_MS + 1000);
        await Promise.resolve();
      });
      expect(screen.getByText(t.runtimeWriteTimeout)).toBeInTheDocument();

      // Write B on the SAME mapping, issued after A was given up on, and it
      // resolves normally. The server now holds force_running.
      fireEvent.click(screen.getByRole('button', { name: t.runtimeForceStart }));
      await act(async () => {
        await Promise.resolve();
        await Promise.resolve();
      });
      expect(screen.queryByRole('button', { name: t.runtimeForceStart })).not.toBeInTheDocument();

      // ...and only THEN does A land. Its payload is older than B's and must
      // not become the cache: the row would offer Force start (a write that
      // undoes B) and hide Force stop (the write that was already abandoned).
      await act(async () => {
        landFirst(fullSpec({ admin_state: 'force_stopped' }));
        await Promise.resolve();
      });

      expect(screen.getByRole('button', { name: t.runtimeForceStop })).toBeInTheDocument();
      expect(screen.queryByRole('button', { name: t.runtimeForceStart })).not.toBeInTheDocument();
    } finally {
      vi.useRealTimers();
    }
  });
});

// C6, third item: `finishRestart` got the same N1 hoist as its two siblings but
// no dedicated red/green pair -- it was justified by verified code-shape
// symmetry alone. This is that pair.
describe('RuntimeAdminSection abandoned clear write refreshes the cache (task 22b, C6)', () => {
  it('refreshes the row after an abandoned restart-clear write later succeeds', async () => {
    vi.useFakeTimers();
    try {
      let landClear: (updated: RuntimeSpec) => void = () => {};
      const clearWrite = new Promise<RuntimeSpec>((resolve) => {
        landClear = resolve;
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
      fireEvent.click(screen.getByRole('button', { name: t.runtimeRestart }));
      await act(async () => {
        await Promise.resolve();
      });
      // The CLEAR half is the one that hangs.
      fakeApi.putRuntimeSpec.mockImplementationOnce(() => clearWrite);
      stream.push([makeStatus({ spec_id: 'spec_1', state: 'stopped' })]);
      await act(async () => {
        await Promise.resolve();
      });
      await act(async () => {
        vi.advanceTimersByTime(OVERRIDE_WRITE_TIMEOUT_MS + 1000);
        await Promise.resolve();
      });
      expect(screen.getByText(t.runtimeRestartClearTimeout)).toBeInTheDocument();

      // ...and then it lands: the server holds admin_state '' again.
      await act(async () => {
        landClear(fullSpec({ admin_state: '' }));
        await Promise.resolve();
      });

      // The cache has to follow, or the row keeps offering a Clear override
      // that is already cleared and keeps hiding the Restart that is available
      // again -- while the notice tells the operator to go and clear it by hand.
      expect(
        screen.queryByRole('button', { name: t.runtimeClearOverride }),
      ).not.toBeInTheDocument();
      expect(screen.getByRole('button', { name: t.runtimeRestart })).toBeInTheDocument();
      // The user-visible half stays abandoned.
      expect(screen.queryByText(t.systemSaved)).not.toBeInTheDocument();
    } finally {
      vi.useRealTimers();
    }
  });
});

// C5 (PRE-EXISTING): `addGpuRow`/`addBudgetRow` keep the auto-PROPOSED index
// clear of collisions, but an operator can retype a row's index onto another
// row's. Both backend writes refuse a duplicate outright (checked in
// portal/service_runtime.go -- neither dedupes, so no filled-in row is ever
// silently discarded), but with a message that names neither the field nor the
// reason, after a round trip.
describe('RuntimeAdminSection duplicate GPU index (task 22b, C5)', () => {
  it('refuses a spec whose two GPU rows share an index, naming the index, before any write', async () => {
    const { putSpecs, created } = renderSection();

    fireEvent.click(await screen.findByRole('button', { name: t.runtimeSpecCreate }));
    fireEvent.change(screen.getByLabelText(t.mappingGatewayName), { target: { value: 'gw' } });
    fireEvent.change(screen.getByLabelText(t.mappingAppName), { target: { value: 'app' } });
    fireEvent.change(screen.getByLabelText(t.runtimeSpecBinary), {
      target: { value: '/usr/bin/llama-server' },
    });
    fireEvent.click(screen.getByRole('button', { name: t.runtimeSpecGpuAdd }));
    fireEvent.click(screen.getByRole('button', { name: t.runtimeSpecGpuAdd }));
    // addGpuRow proposed 0 and 1; retype the second onto the first.
    fireEvent.change(
      screen.getByLabelText(`${t.runtimeSpecGpuIndex}`, { selector: '#runtime-spec-gpu-index-1' }),
      {
        target: { value: '0' },
      },
    );
    fireEvent.click(screen.getByRole('button', { name: t.runtimeSpecCreate }));

    expect(await screen.findByText(`${t.runtimeGpuIndexDuplicate}: GPU 0`)).toBeInTheDocument();
    expect(created).toHaveLength(0);
    expect(putSpecs).toHaveLength(0);
  });

  it('refuses a GPU-budget save whose two rows share an index, before any write', async () => {
    const { fakeApi, putBudgets } = renderSection({
      gpuBudgets: [
        { index: 0, budget_mb: 10000, expected_uuid: '', expected_name: '' },
        { index: 1, budget_mb: 20000, expected_uuid: '', expected_name: '' },
      ],
    });

    fireEvent.click(await screen.findByText(t.runtimeLimits));
    await screen.findByLabelText(`${t.runtimeSpecGpuIndex}`, {
      selector: '#runtime-budget-index-0',
    });
    fireEvent.change(
      screen.getByLabelText(`${t.runtimeSpecGpuIndex}`, {
        selector: '#runtime-budget-index-1',
      }),
      { target: { value: '0' } },
    );
    fireEvent.click(screen.getByRole('button', { name: t.save }));

    expect(await screen.findByText(`${t.runtimeGpuIndexDuplicate}: GPU 0`)).toBeInTheDocument();
    expect(putBudgets).toHaveLength(0);
    expect(fakeApi.putGpuBudgets).not.toHaveBeenCalled();
    expect(fakeApi.updateServer).not.toHaveBeenCalled();
  });
});

// C4: `parseArgsText` pops ONE trailing blank line so an accidental Enter is
// not saved as a new empty argument -- a trade-off a task-20 finding asked
// for, and the right one. The cost is that a genuinely empty TRAILING argument
// collapses on a no-op re-save, and that branch had no test, which made a
// deliberate trade-off look like a bug waiting to be "fixed".
describe('RuntimeAdminSection trailing empty argument (task 22b, C4)', () => {
  it('drops a trailing empty argument on a no-op re-save, while keeping an internal one', async () => {
    const spec = makeSpec({
      configured: true,
      mapping_id: 'map_1',
      binary: '/usr/bin/llama-server',
      // One empty arg in the MIDDLE (preserved) and one at the END (collapsed).
      args: ['--foo', '', '--bar', ''],
    });
    const { putSpecs } = renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      specsByMappingId: { map_1: spec },
    });

    await screen.findByText('gw-model');
    fireEvent.click(await screen.findByRole('button', { name: t.runtimeSpecEditAction }));
    await screen.findByLabelText(t.runtimeSpecBinary);
    fireEvent.click(screen.getByRole('button', { name: t.save }));

    await waitFor(() => expect(putSpecs).toHaveLength(1));
    // The internal blank survives; the trailing one is indistinguishable from
    // the textarea artifact the pop exists to swallow, so it does not.
    expect(putSpecs[0].body.args).toEqual(['--foo', '', '--bar']);
  });
});

// ===========================================================================
// Task 22b, batch C -- fix round 1
// ===========================================================================

// C1: the per-mapping write ticket compared the resolving write against the
// last ISSUED ticket, so ANY later write burned the comparison even when it
// wrote nothing. A later write that FAILS therefore discarded an earlier
// abandoned-but-successful one permanently -- which is exactly the N1 defect
// fix round 2 closed, reached through the fix for C6. Four of the five review
// lenses found this independently.
describe('RuntimeAdminSection ticket compares against the last COMMITTED write (fix round 1, C1)', () => {
  it('still lands an abandoned override write when the LATER write to the same mapping failed', async () => {
    let landFirst: (updated: RuntimeSpec) => void = () => {};
    const first = new Promise<RuntimeSpec>((resolve) => {
      landFirst = resolve;
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
    // Wait for the row's override actions to exist (see `waitForEnabledButton`)
    // BEFORE the fake clock goes in, because that wait cannot happen after it:
    // `@testing-library/react`'s `asyncWrapper` ends every async query with
    // `await new Promise(r => setTimeout(r, 0))` and only pumps the clock when
    // `jestFakeTimersAreEnabled()` -- which tests for a `jest` global Vitest
    // does not define. Under `vi.useFakeTimers()` that `setTimeout` is faked
    // and never fires, so `waitFor`/`findBy*` hang for the whole 5 s test
    // timeout even when their condition ALREADY holds (measured: 5.01 s at
    // every slowness step, including 0). Hence real timers here, fake ones
    // below.
    await waitForEnabledButton(t.runtimeForceStop);

    // From here the fake clock, for the 30 s override watchdog. Late enough to
    // be free: the only two timers this component arms are that watchdog and
    // the restart deadline, and both are armed by a click, never at mount.
    vi.useFakeTimers();
    try {
      // Write A: force_stopped, hangs past its 30 s watchdog. The row unlocks
      // and the operator is told the outcome is unknown.
      fakeApi.putRuntimeSpec.mockImplementationOnce(() => first);
      fireEvent.click(screen.getByRole('button', { name: t.runtimeForceStop }));
      await act(async () => {
        await Promise.resolve();
      });
      await act(async () => {
        vi.advanceTimersByTime(OVERRIDE_WRITE_TIMEOUT_MS + 1000);
        await Promise.resolve();
      });
      expect(screen.getByText(t.runtimeWriteTimeout)).toBeInTheDocument();

      // Write B: the retry, on the same mapping -- and it REJECTS, so it never
      // writes the cache. It must not burn A's right to commit.
      //
      // This second click needs no gate of its own, and could not have one
      // (fake timers, see above). The spec is cached from here on, so the
      // action cannot go absent again, and the only thing that could still
      // disable it is `overrideBusy` -- which the watchdog clears in the SAME
      // commit that shows the notice the line above just asserted.
      fakeApi.putRuntimeSpec.mockImplementationOnce(() =>
        Promise.reject(new Error('write B failed')),
      );
      fireEvent.click(screen.getByRole('button', { name: t.runtimeForceStop }));
      await act(async () => {
        await Promise.resolve();
        await Promise.resolve();
        await Promise.resolve();
      });

      // ...and only now does A land, having actually stored force_stopped.
      await act(async () => {
        landFirst(fullSpec({ admin_state: 'force_stopped' }));
        await Promise.resolve();
        await Promise.resolve();
      });

      // The row has to follow the server, or it offers Force stop / Force start
      // / Restart and NO Clear override while the model is admission-blocked --
      // with the write-timeout notice on screen telling the operator to clear
      // by hand an override the row denies exists.
      expect(screen.getByRole('button', { name: t.runtimeClearOverride })).toBeInTheDocument();
      expect(screen.queryByRole('button', { name: t.runtimeForceStop })).not.toBeInTheDocument();
      // The user-visible half stays abandoned: no success toast for a write the
      // operator was already told had been given up on.
      expect(screen.queryByText(t.systemSaved)).not.toBeInTheDocument();
    } finally {
      vi.useRealTimers();
    }
  });

  // A door that needs no watchdog at all, because `overrideBusy` does not
  // disable the specs tab's row actions (`rowActions` gates only on
  // `loadingEditFor`): a spec DELETE that fails burns a ticket without
  // committing, and here the override's run token was never bumped either --
  // so the operator sees a "saved" toast for a write the row then denies
  // happened.
  it('still lands an in-flight override write when a later spec DELETE failed', async () => {
    let landOverride: (updated: RuntimeSpec) => void = () => {};
    const override = new Promise<RuntimeSpec>((resolve) => {
      landOverride = resolve;
    });
    const { fakeApi, stream } = renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      specsByMappingId: { map_1: fullSpec() },
      statusRows: [makeStatus({ spec_id: 'spec_1', state: 'running' })],
    });
    stream.setStatus('open');
    await screen.findByText('gw-model');
    fireEvent.click(screen.getByRole('tab', { name: t.runtimeLiveStatus }));

    fakeApi.putRuntimeSpec.mockImplementationOnce(() => override);
    // `findByText('gw-model')` above answers for the MAPPINGS list, not for
    // this row's per-row spec GET -- and the override actions are rendered
    // only once that GET has landed. Measured without this gate: 4/5 runs
    // failed with `Unable to find ... "Stopp erzwingen"` at slowness step 5.
    await waitForEnabledButton(t.runtimeForceStop);
    fireEvent.click(screen.getByRole('button', { name: t.runtimeForceStop }));
    await act(async () => {
      await Promise.resolve();
    });

    // The specs tab stays reachable while that PUT is outstanding. Delete the
    // spec -- and the delete fails, so it writes nothing.
    fireEvent.click(screen.getByRole('tab', { name: t.runtimeSpecs }));
    fakeApi.deleteRuntimeSpec.mockImplementationOnce(() =>
      Promise.reject(new Error('delete failed')),
    );
    // Same shared-label hazard as the spec-delete test above: wait for the
    // settled, enabled control rather than for the name alone.
    await waitForEnabledButton(t.runtimeSpecDelete);
    fireEvent.click(screen.getByRole('button', { name: t.runtimeSpecDelete }));
    fireEvent.click(
      within(screen.getByRole('dialog')).getByRole('button', { name: t.runtimeSpecDelete }),
    );
    await act(async () => {
      await Promise.resolve();
      await Promise.resolve();
    });
    // A failed delete leaves the confirm dialog open (`setConfirmingDeleteId('')`
    // is only reached on success), and an open modal aria-hides the tab strip.
    fireEvent.click(
      within(screen.getByRole('dialog')).getByRole('button', { name: t.mappingCancel }),
    );
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());

    // Now the override write lands. Its run token was never bumped, so it
    // toasts success -- and the row must agree with that toast.
    await act(async () => {
      landOverride(fullSpec({ admin_state: 'force_stopped' }));
      await Promise.resolve();
      await Promise.resolve();
    });

    fireEvent.click(screen.getByRole('tab', { name: t.runtimeLiveStatus }));
    expect(screen.getByRole('button', { name: t.runtimeClearOverride })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: t.runtimeForceStop })).not.toBeInTheDocument();
  });
});

// C2: `openEdit` wrote `specsById` straight from its GET, with no ticket. That
// GET is issued from the specs tab while nothing locks the status tab, so a
// slow one lands AFTER a successful override write and overwrites the entry
// with its pre-PUT snapshot -- and nothing re-fetches on the way back
// (`loadedIdsRef` blocks the lazy loader), so the poison is permanent.
describe('RuntimeAdminSection openEdit GET takes a ticket (fix round 1, C2)', () => {
  it('does not let a slow Edit GET overwrite a newer override write', async () => {
    let landGet: (spec: RuntimeSpec) => void = () => {};
    const slowGet = new Promise<RuntimeSpec>((resolve) => {
      landGet = resolve;
    });
    const { fakeApi, stream } = renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      specsByMappingId: { map_1: fullSpec() },
      statusRows: [makeStatus({ spec_id: 'spec_1', state: 'running' })],
    });
    stream.setStatus('open');
    await screen.findByText('gw-model');
    await act(async () => {
      await Promise.resolve();
    });

    // The lazy per-mapping GET has settled; THIS one -- the Edit GET -- is slow.
    fakeApi.runtimeSpec.mockImplementationOnce(() => slowGet);
    fireEvent.click(await screen.findByRole('button', { name: t.runtimeSpecEditAction }));
    await act(async () => {
      await Promise.resolve();
    });
    // Still on the list: `specMode` only flips once the GET resolves, so
    // nothing on this screen is locked.
    expect(screen.queryByLabelText(t.runtimeSpecBinary)).not.toBeInTheDocument();

    // The operator switches to the live-status tab and forces the model down.
    fireEvent.click(screen.getByRole('tab', { name: t.runtimeLiveStatus }));
    // Same gate as the two tests above. This one is only INCIDENTALLY safe --
    // the lazy GET is meant to have settled by here (it is the Edit GET that
    // is slow in this test), and it usually has; measured without the gate,
    // 1/5 runs still failed with `Unable to find ... "Stopp erzwingen"` at
    // slowness step 5. "Usually" is what this whole change is about.
    await waitForEnabledButton(t.runtimeForceStop);
    fireEvent.click(screen.getByRole('button', { name: t.runtimeForceStop }));
    await act(async () => {
      await Promise.resolve();
      await Promise.resolve();
    });
    expect(screen.getByRole('button', { name: t.runtimeClearOverride })).toBeInTheDocument();

    // ...and only NOW the Edit GET lands, carrying its pre-PUT snapshot.
    await act(async () => {
      landGet(fullSpec({ admin_state: '' }));
      await Promise.resolve();
    });

    // The form opened and hydrated from that snapshot -- which is right, it is
    // the document this form will PUT back. Back out of it.
    await screen.findByLabelText(t.runtimeSpecBinary);
    fireEvent.click(screen.getByRole('button', { name: t.cancel }));
    fireEvent.click(screen.getByRole('tab', { name: t.runtimeLiveStatus }));

    // The CACHE, though, must still hold the override the server actually has.
    expect(screen.getByRole('button', { name: t.runtimeClearOverride })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: t.runtimeForceStop })).not.toBeInTheDocument();
  });
});

// F1: C2 gave the READ a snapshot, but of the ISSUE counter -- which only sees
// writes STARTED after the read started. So it caught exactly one of the two
// orderings this GET has with an override write, and the OTHER one is reached
// by the same two ordinary clicks, no watchdog and no fake timers needed: the
// write goes first, so `issued` has ALREADY moved when the read snapshots it,
// the write commits inside the read's window, and the returning snapshot still
// compares equal and goes in over it. Snapshotting the COMMIT counter refuses
// that -- and, being advanced only by writes that actually wrote, it also stops
// a write that committed NOTHING from discarding a read.
describe('RuntimeAdminSection spec reads snapshot the COMMIT order (fix round 2, F1)', () => {
  it('refuses an Edit GET issued while an override write was already in flight', async () => {
    let landPut: (updated: RuntimeSpec) => void = () => {};
    const put = new Promise<RuntimeSpec>((resolve) => {
      landPut = resolve;
    });
    let landGet: (spec: RuntimeSpec) => void = () => {};
    const slowGet = new Promise<RuntimeSpec>((resolve) => {
      landGet = resolve;
    });
    const { fakeApi, stream } = renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      specsByMappingId: { map_1: fullSpec() },
      statusRows: [makeStatus({ spec_id: 'spec_1', state: 'running' })],
    });
    stream.setStatus('open');
    await screen.findByText('gw-model');
    await act(async () => {
      await Promise.resolve();
    });

    // 1. Force stop. Its ticket is ISSUED and the PUT is outstanding, so
    //    `overrideBusy` locks the status table -- and only the status table.
    fireEvent.click(screen.getByRole('tab', { name: t.runtimeLiveStatus }));
    fakeApi.putRuntimeSpec.mockImplementationOnce(() => put);
    fireEvent.click(screen.getByRole('button', { name: t.runtimeForceStop }));
    await act(async () => {
      await Promise.resolve();
    });

    // 2. The tab strip is never disabled, so the operator goes back to the
    //    specs tab and hits Edit (`rowActions` gates only on `loadingEditFor`).
    //    This read starts AFTER the write's ticket was issued -- the ordering
    //    an issue-order snapshot cannot see.
    fireEvent.click(screen.getByRole('tab', { name: t.runtimeSpecs }));
    fakeApi.runtimeSpec.mockImplementationOnce(() => slowGet);
    fireEvent.click(await screen.findByRole('button', { name: t.runtimeSpecEditAction }));
    await act(async () => {
      await Promise.resolve();
    });

    // 3. The PUT resolves FIRST: the server has stored force_stopped and the
    //    cache now says so.
    await act(async () => {
      landPut(fullSpec({ admin_state: 'force_stopped' }));
      await Promise.resolve();
      await Promise.resolve();
    });

    // 4. ...and only NOW the GET lands, carrying its pre-PUT snapshot.
    await act(async () => {
      landGet(fullSpec({ admin_state: '' }));
      await Promise.resolve();
    });

    // The FORM still hydrates from that snapshot -- it is the document this
    // form would PUT back, which C2's own comment insists on. Back out of it;
    // nothing re-fetches on the way (`loadedIdsRef` blocks the lazy loader), so
    // whatever the cache holds now is what it holds for the rest of the mount.
    await screen.findByLabelText(t.runtimeSpecBinary);
    fireEvent.click(screen.getByRole('button', { name: t.cancel }));
    fireEvent.click(screen.getByRole('tab', { name: t.runtimeLiveStatus }));

    // The CACHE must still hold what the server acknowledged. Otherwise the row
    // offers Force stop on an already-force-stopped model and denies the Clear
    // override that is the only way out of it.
    expect(screen.getByRole('button', { name: t.runtimeClearOverride })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: t.runtimeForceStop })).not.toBeInTheDocument();
  });

  // The other half of the same one-word change, and the mirror of C1 for
  // READS: a write that FAILED wrote nothing, so it must not cost this mapping
  // the read that overlapped it. `commitSpecRead` compares the COMMIT counter
  // at landing time; compared against the ISSUE counter it would see the failed
  // write's ticket and throw the payload away.
  //
  // This used to be driven through a mapping delete on a row whose lazy spec
  // GET was still in flight -- the route task 22b's delete gate deliberately
  // CLOSED, because a Delete on a row whose spec state is unknown is exactly
  // what must not reach an endpoint (see the delete-gate suite below). The
  // failing write is therefore an override PUT, which is still reachable on a
  // row that HAS a cached spec, and the read is the Edit GET: it starts before
  // the write's ticket is issued, so a correct `seen` still matches at landing.
  it('keeps an Edit read that a FAILED override write overlapped', async () => {
    let landGet: (spec: RuntimeSpec) => void = () => {};
    const slowGet = new Promise<RuntimeSpec>((resolve) => {
      landGet = resolve;
    });
    const { fakeApi, stream } = renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      specsByMappingId: { map_1: fullSpec() },
      statusRows: [makeStatus({ spec_id: 'spec_1', state: 'running' })],
    });
    stream.setStatus('open');
    await screen.findByText('gw-model');
    await act(async () => {
      await Promise.resolve();
    });

    // The lazy per-mapping GET has settled; THIS one -- the Edit GET -- hangs.
    // It snapshots the counters while nothing has been written at all.
    fakeApi.runtimeSpec.mockImplementationOnce(() => slowGet);
    fireEvent.click(await screen.findByRole('button', { name: t.runtimeSpecEditAction }));
    await act(async () => {
      await Promise.resolve();
    });

    // Now a write takes a ticket for the same mapping and FAILS, so it commits
    // nothing -- the issue counter has moved, the commit counter has not.
    fireEvent.click(screen.getByRole('tab', { name: t.runtimeLiveStatus }));
    fakeApi.putRuntimeSpec.mockImplementationOnce(() =>
      Promise.reject(new Error('override failed')),
    );
    fireEvent.click(screen.getByRole('button', { name: t.runtimeForceStop }));
    await act(async () => {
      await Promise.resolve();
      await Promise.resolve();
    });
    // Nothing was stored, so nothing about the row may have changed.
    expect(screen.queryByRole('button', { name: t.runtimeClearOverride })).not.toBeInTheDocument();

    // ...and only NOW the Edit GET lands, carrying an override a SECOND admin
    // put in place. It is the freshest thing anyone has said about this spec.
    await act(async () => {
      landGet(fullSpec({ admin_state: 'force_running' }));
      await Promise.resolve();
    });
    await screen.findByLabelText(t.runtimeSpecBinary);
    fireEvent.click(screen.getByRole('button', { name: t.cancel }));

    // It has to be kept. Discarded, it is discarded for good -- `loadedIdsRef`
    // already holds this id and never retries -- and the row would then deny
    // the Clear override that is the only way out of a force_running.
    fireEvent.click(screen.getByRole('tab', { name: t.runtimeLiveStatus }));
    expect(screen.getByRole('button', { name: t.runtimeClearOverride })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: t.runtimeForceStart })).not.toBeInTheDocument();
  });
});

// C3: `submitCreate` pre-seeds `loadedIdsRef` so the new mapping is not
// re-fetched, but nothing ever added it to `specLoadSettled` -- whose only
// writer is the lazy loader's `finally`, which the pre-seed skips. So
// `specsSettled` was false for the rest of the mount and the `Unmatched` chip
// vanished from every genuinely unresolved row: the reported name with a blank
// actions cell and no marker, which is the two-states-look-identical case the
// chip exists to remove.
describe('RuntimeAdminSection specsSettled survives a create (fix round 1, C3)', () => {
  it('keeps the Unmatched chip on an unresolved row after a mapping is created', async () => {
    const { created } = renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      specsByMappingId: { map_1: fullSpec() },
      statusRows: [makeStatus({ spec_id: 'spec_orphan', model: 'orphan-model' })],
    });
    await openStatusTab();
    // Baseline: once every per-mapping spec GET has settled, the orphan row is
    // named as unmatched.
    expect(await screen.findByText(t.runtimeStatusUnresolvedShort)).toBeInTheDocument();

    fireEvent.click(screen.getByRole('tab', { name: t.runtimeSpecs }));
    fireEvent.click(await screen.findByRole('button', { name: t.runtimeSpecCreate }));
    fireEvent.change(screen.getByLabelText(t.mappingGatewayName), {
      target: { value: 'gw-new' },
    });
    fireEvent.change(screen.getByLabelText(t.mappingAppName), { target: { value: 'app-new' } });
    fireEvent.change(screen.getByLabelText(t.runtimeSpecBinary), {
      target: { value: '/usr/bin/llama-server' },
    });
    fireEvent.click(screen.getByRole('button', { name: t.runtimeSpecCreate }));
    await waitFor(() => expect(created).toHaveLength(1));
    await act(async () => {
      await Promise.resolve();
      await Promise.resolve();
    });

    await openStatusTab();
    expect(screen.getByText(t.runtimeStatusUnresolvedShort)).toBeInTheDocument();
  });
});

// C4: the mappings banner passed no `staleErrorLabel`, so a failed RELOAD
// rendered the hard-error text -- which claims no process can be matched and no
// override actions are available. Both clauses are false in that state:
// `mappings` and `specsById` still hold the previous payload, the rows resolve,
// and the overrides work. And `specsSettled` went false with it, so the chip
// disappeared from the rows that ARE genuinely unmatched -- the silent blank
// task 22's N3 removed, reached through the fix for N3.
describe('RuntimeAdminSection failing mappings RELOAD (fix round 1, C4)', () => {
  it('names the stale fact truthfully and keeps the rows, their actions and the chip', async () => {
    const en = messages.en;
    const { fakeApi, stream, rerenderWithLocale } = renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      specsByMappingId: { map_1: fullSpec() },
      statusRows: [
        makeStatus({ spec_id: 'spec_1', state: 'running' }),
        makeStatus({ spec_id: 'spec_orphan', model: 'orphan-model' }),
      ],
      mappingsFailsOnCall: 2,
    });
    stream.setStatus('open');
    await openStatusTab();
    // Resolved and unresolved row side by side, both named.
    expect(await screen.findByText('gw-model')).toBeInTheDocument();
    expect(screen.getByText(t.runtimeStatusUnresolvedShort)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: t.runtimeForceStop })).toBeInTheDocument();

    // The language switch: `t` is a dep of every loader here, so this re-runs
    // the mappings GET with the component mounted -- and it fails, leaving the
    // previous list in `data`.
    rerenderWithLocale(en);
    await waitFor(() => expect(fakeApi.mappings).toHaveBeenCalledTimes(2));

    // The stale fact, stated as what it is.
    expect(await screen.findByText(en.runtimeMappingsStale)).toBeInTheDocument();
    // NOT the hard-error text, whose two claims are both false here.
    expect(screen.queryByText(en.runtimeMappingsUnavailable)).not.toBeInTheDocument();
    // The row still resolves and still offers its overrides, so a banner
    // claiming otherwise would be the misleading half.
    expect(screen.getByText('gw-model')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: en.runtimeForceStop })).toBeInTheDocument();
    // ...and the genuinely unmatched row keeps its marker rather than going
    // quietly blank.
    expect(screen.getByText(en.runtimeStatusUnresolvedShort)).toBeInTheDocument();
  });
});

// M5: the mapping-delete branch removed `specsById[id]` directly and recorded
// no ticket, so a spec write still in flight for that mapping resurrected the
// deleted mapping's spec. `confirmDelete`'s own comment states this rule for
// the sibling (spec-delete) branch: "a delete is a write to the same cache
// entry, so it takes a ticket too".
//
// The reachable route in: the create form's spec PUT has no watchdog and
// `busy` disables only Submit, so Cancel gets the operator back to a list that
// already holds the new mapping (createMapping resolved first) with NO cache
// entry for it -- which is the mapping-delete branch. Deleting it there, then
// having the PUT land, invents a configured spec for a mapping that is gone.
describe('RuntimeAdminSection mapping delete takes a ticket (fix round 1, M5)', () => {
  it('does not let an in-flight spec write resurrect a deleted mapping', async () => {
    let landSpecPut: (spec: RuntimeSpec) => void = () => {};
    const specPut = new Promise<RuntimeSpec>((resolve) => {
      landSpecPut = resolve;
    });
    const { fakeApi, created, deletedMappingIds } = renderSection({
      mappings: [],
      // A process the agent reports under the spec id the pending PUT will
      // return, so the resurrection is visible where it does harm.
      statusRows: [makeStatus({ spec_id: 'spec_new', model: 'orphan-model' })],
    });

    fireEvent.click(await screen.findByRole('button', { name: t.runtimeSpecCreate }));
    fireEvent.change(screen.getByLabelText(t.mappingGatewayName), { target: { value: 'gw-new' } });
    fireEvent.change(screen.getByLabelText(t.mappingAppName), { target: { value: 'app-new' } });
    fireEvent.change(screen.getByLabelText(t.runtimeSpecBinary), {
      target: { value: '/usr/bin/llama-server' },
    });
    // The mapping POST resolves; the spec PUT hangs.
    fakeApi.putRuntimeSpec.mockImplementationOnce(() => specPut);
    fireEvent.click(screen.getByRole('button', { name: t.runtimeSpecCreate }));
    await waitFor(() => expect(created).toHaveLength(1));
    await act(async () => {
      await Promise.resolve();
    });

    // Cancel is not disabled by `busy`, so the operator is back on the list
    // while the PUT is still outstanding -- with the new mapping already in it
    // and no spec cached for it, i.e. the mapping-delete branch.
    fireEvent.click(screen.getByRole('button', { name: t.cancel }));
    fireEvent.click(await screen.findByRole('button', { name: t.mappingDelete }));
    fireEvent.click(
      within(screen.getByRole('dialog')).getByRole('button', { name: t.mappingDelete }),
    );
    await waitFor(() => expect(deletedMappingIds).toEqual(['map_created']));
    // The dialog's exit transition keeps the tab strip aria-hidden until it is
    // unmounted.
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());

    // ...and only now does the spec PUT land.
    await act(async () => {
      landSpecPut(fullSpec({ id: 'spec_new', mapping_id: 'map_created' }));
      await Promise.resolve();
      await Promise.resolve();
    });

    // The cache must stay empty. A resurrected entry gives the status row four
    // override actions that all PUT to a mapping id that no longer exists, on
    // a row `mappingForStatus` can no longer even name.
    fireEvent.click(screen.getByRole('tab', { name: t.runtimeLiveStatus }));
    expect(screen.getByText('orphan-model')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: t.runtimeForceStop })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: t.runtimeRestart })).not.toBeInTheDocument();
  });
});

// M6: C7 lifted the restart notices to screen level, and every one of them
// tells the operator to go to the specs tab and clear an override by hand --
// but nothing on the specs tab cleared them (only the three status-tab flows
// did), and the form sub-view early-returns without the banner stack. So the
// operator did exactly what the notice said and landed back on the specs tab
// with that same notice as the first thing they saw.
describe('RuntimeAdminSection a spec save clears the restart notice (fix round 1, M6)', () => {
  it('clears a restart timeout notice when the form save lands no override', async () => {
    vi.useFakeTimers();
    try {
      const { stream } = renderSection({
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
      // The awaited `stopped` never arrives: force_stopped is left in place and
      // the notice says to clear it by hand on the specs tab.
      await act(async () => {
        vi.advanceTimersByTime(RESTART_STOP_TIMEOUT_MS + 1000);
        await Promise.resolve();
      });
      expect(screen.getByText(t.runtimeRestartTimeout)).toBeInTheDocument();

      // Do exactly that: open the spec form (whose admin_state field hydrates
      // to "automatic" from the GET) and save.
      fireEvent.click(screen.getByRole('tab', { name: t.runtimeSpecs }));
      fireEvent.click(screen.getByRole('button', { name: t.runtimeSpecEditAction }));
      await act(async () => {
        await Promise.resolve();
        await Promise.resolve();
      });
      fireEvent.click(screen.getByRole('button', { name: t.save }));
      await act(async () => {
        await Promise.resolve();
        await Promise.resolve();
        await Promise.resolve();
      });

      // Back on the specs tab, and the notice must not be the first thing the
      // operator sees after resolving it.
      expect(screen.queryByText(t.runtimeRestartTimeout)).not.toBeInTheDocument();
    } finally {
      vi.useRealTimers();
    }
  });

  it('keeps the notice when the save leaves an override in place', async () => {
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
      await act(async () => {
        vi.advanceTimersByTime(RESTART_STOP_TIMEOUT_MS + 1000);
        await Promise.resolve();
      });
      expect(screen.getByText(t.runtimeRestartTimeout)).toBeInTheDocument();

      // A save that keeps force_stopped is not the remediation the notice asks
      // for, so the notice is still news. The form hydrates from its own GET,
      // so that is where the override comes from here.
      fireEvent.click(screen.getByRole('tab', { name: t.runtimeSpecs }));
      fakeApi.runtimeSpec.mockImplementationOnce(async () =>
        fullSpec({ admin_state: 'force_stopped' }),
      );
      fireEvent.click(screen.getByRole('button', { name: t.runtimeSpecEditAction }));
      await act(async () => {
        await Promise.resolve();
        await Promise.resolve();
      });
      fireEvent.click(screen.getByRole('button', { name: t.save }));
      await act(async () => {
        await Promise.resolve();
        await Promise.resolve();
        await Promise.resolve();
      });

      expect(screen.getByText(t.runtimeRestartTimeout)).toBeInTheDocument();
    } finally {
      vi.useRealTimers();
    }
  });
});

// M8: the warnings resource was the one left on the two-state shape although
// C1's own title was "every remaining resource". It gates nothing and its
// failure raises a toast that does not auto-dismiss, so this is the mildest
// instance -- but `warningsData ?? []` still makes "this application has no
// advisory warnings" and "we failed to find out" render identically.
describe('RuntimeAdminSection failing warnings GET (fix round 1, M8)', () => {
  it('says the advisory list is unknown rather than rendering it as empty, and recovers', async () => {
    const { fakeApi } = renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      specsByMappingId: { map_1: fullSpec() },
      warnings: ['timeout_ms_below_startup_timeout'],
      warningsFailing: true,
    });

    expect(await screen.findByText(t.runtimeWarningsUnavailable)).toBeInTheDocument();
    // Not the screen-wide read-only treatment: this resource gates nothing.
    expect(
      await screen.findByRole('button', { name: t.runtimeSpecEditAction }),
    ).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: t.resourceRetry }));
    await waitFor(() => expect(fakeApi.runtimeWarnings).toHaveBeenCalledTimes(2));
    expect(await screen.findByText(t.runtimeTimeoutWarning)).toBeInTheDocument();
    expect(screen.queryByText(t.runtimeWarningsUnavailable)).not.toBeInTheDocument();
  });

  it('distinguishes a failed REFRESH from a failed first load', async () => {
    const en = messages.en;
    const { fakeApi, rerenderWithLocale } = renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      specsByMappingId: { map_1: fullSpec() },
      warnings: ['timeout_ms_below_startup_timeout'],
      warningsFailsOnCall: 2,
    });

    expect(await screen.findByText(t.runtimeTimeoutWarning)).toBeInTheDocument();
    rerenderWithLocale(en);
    await waitFor(() => expect(fakeApi.runtimeWarnings).toHaveBeenCalledTimes(2));

    expect(await screen.findByText(en.runtimeWarningsStale)).toBeInTheDocument();
    expect(screen.queryByText(en.runtimeWarningsUnavailable)).not.toBeInTheDocument();
    // The last known list stays on screen -- that is what `stale-error` means.
    expect(screen.getByText(en.runtimeTimeoutWarning)).toBeInTheDocument();
  });
});

// M9: the re-seed comment claimed the budgets are "never refreshed by a
// background poll, so this can't clobber an in-progress edit". Two triggers
// make that false: `t` is a loader dep, so a language switch re-GETs them with
// the component mounted, and C1 added a Retry button. An unsaved edit snapped
// back to the server's values with no toast and no indication.
describe('RuntimeAdminSection budget draft survives a re-seed (fix round 1, M9)', () => {
  it('keeps an unsaved budget edit across a language switch', async () => {
    const en = messages.en;
    const { fakeApi, putBudgets, rerenderWithLocale } = renderSection({
      gpuBudgets: [{ index: 0, budget_mb: 40000, expected_uuid: '', expected_name: '' }],
    });

    fireEvent.click(await screen.findByText(t.runtimeLimits));
    const budgetField = await screen.findByLabelText(t.runtimeGpuBudget);
    expect((budgetField as HTMLInputElement).value).toBe('40000');
    fireEvent.change(budgetField, { target: { value: '32000' } });

    rerenderWithLocale(en);
    await waitFor(() => expect(fakeApi.gpuBudgets).toHaveBeenCalledTimes(2));
    // `gpuBudgets` having been CALLED twice only means the re-GET was ISSUED.
    // Its payload -- and the re-seed effect that is what would clobber the
    // draft -- lands microtasks later, so without this flush the assertion
    // below can read the field BEFORE the clobber and pass for the wrong
    // reason. It made this test a 3-in-5 barrier against removing the guard.
    await act(async () => {
      await Promise.resolve();
      await Promise.resolve();
      await Promise.resolve();
    });

    // The draft is still the operator's, and Save still writes the complete
    // list they can actually see.
    const after = await screen.findByLabelText(en.runtimeGpuBudget);
    expect((after as HTMLInputElement).value).toBe('32000');
    fireEvent.click(screen.getByRole('button', { name: en.save }));
    await waitFor(() => expect(putBudgets).toHaveLength(1));
    expect(putBudgets[0]).toEqual([
      { index: 0, budget_mb: 32000, expected_uuid: '', expected_name: '' },
    ]);
  });

  it('still re-seeds from the authoritative list a successful save feeds back', async () => {
    const { putBudgets } = renderSection({
      gpuBudgets: [{ index: 0, budget_mb: 40000, expected_uuid: '', expected_name: '' }],
    });

    fireEvent.click(await screen.findByText(t.runtimeLimits));
    const budgetField = await screen.findByLabelText(t.runtimeGpuBudget);
    fireEvent.change(budgetField, { target: { value: '32000' } });
    fireEvent.click(screen.getByRole('button', { name: t.save }));
    await waitFor(() => expect(putBudgets).toHaveLength(1));
    // Saved without a drift snapshot -- the loaded list had none.
    expect(putBudgets[0]).toEqual([
      { index: 0, budget_mb: 32000, expected_uuid: '', expected_name: '' },
    ]);

    // The dirty flag must not survive the save, or the post-save list (which
    // carries the expected_* snapshots the backend takes) would never land.
    // Asserting only the visible budget cannot see that: the draft and the
    // server's answer agree about 32000 and differ ONLY in the expected_*
    // fields, which this form does not render -- so the next Save's BODY is
    // where "we re-seeded" and "we kept the draft" finally part company.
    await waitFor(() =>
      expect((screen.getByLabelText(t.runtimeGpuBudget) as HTMLInputElement).value).toBe('32000'),
    );
    fireEvent.change(screen.getByLabelText(t.runtimeGpuBudget), { target: { value: '16000' } });
    fireEvent.click(screen.getByRole('button', { name: t.save }));
    await waitFor(() => expect(putBudgets).toHaveLength(2));
    expect(putBudgets[1]).toEqual([
      {
        index: 0,
        budget_mb: 16000,
        expected_uuid: 'GPU-0-UUID',
        expected_name: 'card-at-0',
      },
    ]);
  });
});

// Task 22b: a row action that performs a strictly LARGER destructive operation
// than the operator asked for.
//
// The row's one Delete means "delete the spec" or "delete the MAPPING", and it
// was decided by `specsById[id]?.configured` -- a cache filled by a sequential
// per-row fan-out that `loadedIdsRef` never retries. So "no entry" was read as
// "no spec" while it still meant "we have not found out": for the length of the
// fan-out on every row, and for the whole mount on a row whose one spec GET
// failed. `confirmDelete` then honoured that reading and called
// `api.deleteMapping`, destroying the model route the operator only meant to
// unconfigure. `disabled: rowBusy` never covered that window, and the
// destructive branch was the DEFAULT when knowledge was missing.
describe('RuntimeAdminSection delete gate (task 22b)', () => {
  /** The row's single Delete, whichever of the two things it currently says. */
  function rowDelete(): HTMLElement {
    return (
      screen.queryByRole('button', { name: t.mappingDelete }) ??
      screen.getByRole('button', { name: t.runtimeSpecDelete })
    );
  }

  /** Drains the two await points a confirmed delete needs to reach its call. */
  async function settleDelete() {
    await act(async () => {
      await Promise.resolve();
      await Promise.resolve();
    });
  }

  /**
   * Clicks the row's Delete on a row where it is OFFERED, and confirms it. That
   * second click is the one that reaches an endpoint.
   *
   * The confirmation is required, not merely handled if present. It used to be
   * optional -- `queryByRole('dialog')`, skipped when null -- which made this
   * helper silently do nothing whenever the Delete was still disabled and ate
   * the click (see `waitForEnabledButton`). The test then failed several lines
   * later on `expected [] to deeply equal ['map_1']`, which names neither the
   * swallowed click nor the row that swallowed it. Callers that expect the
   * gate to REFUSE the click use `clickLockedDelete` instead, so nothing here
   * has to guess which of the two it is looking at.
   *
   * `getByRole` and not `findByRole`, deliberately: an enabled click yields its
   * dialog synchronously inside `fireEvent`'s `act()` (measured), so awaiting
   * would only turn an instant, informative error into a 1 s timeout.
   */
  async function clickThroughDelete() {
    fireEvent.click(rowDelete());
    const dialog = screen.getByRole('dialog');
    fireEvent.click(
      within(dialog).queryByRole('button', { name: t.mappingDelete }) ??
        within(dialog).getByRole('button', { name: t.runtimeSpecDelete }),
    );
    await settleDelete();
  }

  /**
   * Clicks the row's Delete on a row where the gate must REFUSE it, and asserts
   * that no confirmation opened -- the observable half of "locked", and what
   * lets `clickThroughDelete` insist on the dialog everywhere else.
   */
  async function clickLockedDelete() {
    fireEvent.click(rowDelete());
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    await settleDelete();
  }

  it('never deletes the mapping while this row’s spec state is unknown', async () => {
    let landLazyGet: (spec: RuntimeSpec) => void = () => {};
    const lazyGet = new Promise<RuntimeSpec>((resolve) => {
      landLazyGet = resolve;
    });
    const { fakeApi, deletedMappingIds, deletedSpecIds } = renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      // The server HAS a configured spec for this mapping. Nothing on the
      // screen knows that yet, which is the entire point.
      specsByMappingId: { map_1: fullSpec() },
    });
    // Held open BEFORE the fan-out can issue it: `mappings` is `mappingsData ??
    // []`, so the lazy loader's effect finds nothing to load on the first
    // render and only fires once the mappings GET resolves -- which is a
    // microtask, i.e. strictly after this synchronous line.
    fakeApi.runtimeSpec.mockImplementationOnce(() => lazyGet);
    await screen.findByText('gw-model');
    await act(async () => {
      await Promise.resolve();
    });

    const del = rowDelete();
    await clickLockedDelete();

    // THE assertion, and deliberately about the OUTCOME rather than the label:
    // a label that reads wrongly is a nuisance, a model route that no longer
    // exists is data loss. Whatever this control says it does, it must not have
    // deleted the mapping.
    expect(fakeApi.deleteMapping).not.toHaveBeenCalled();
    expect(deletedMappingIds).toEqual([]);
    // Nor may it have quietly done the other one instead.
    expect(fakeApi.deleteRuntimeSpec).not.toHaveBeenCalled();
    expect(deletedSpecIds).toEqual([]);
    expect(screen.getByText('gw-model')).toBeInTheDocument();

    // How that is achieved: the action is still offered, but locked -- and it
    // says why, because a greyed-out control with no account of itself is its
    // own defect (RowAction.title, forwarded on the inline path).
    expect(del).toBeDisabled();
    fireEvent.mouseOver(del.parentElement as HTMLElement);
    expect(await screen.findByRole('tooltip')).toHaveTextContent(t.runtimeSpecDeleteStateLoading);

    // And it resolves itself the moment the read answers.
    await act(async () => {
      landLazyGet(fullSpec());
      await Promise.resolve();
      await Promise.resolve();
    });
    expect(await screen.findByRole('button', { name: t.runtimeSpecDelete })).toBeEnabled();
  });

  it('offers the spec delete on that same row once the read answers', async () => {
    const { fakeApi, deletedSpecIds, deletedMappingIds } = renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      specsByMappingId: { map_1: fullSpec() },
    });
    await screen.findByText('gw-model');
    await act(async () => {
      await Promise.resolve();
    });

    // Settled and configured: the smaller operation, offered and working.
    //
    // The single flush above is not what settles this row -- awaiting the
    // ENABLED control is (see `waitForEnabledButton`). Measured without it,
    // 5/5 runs failed here on `Received element is not enabled: <button
    // aria-label="Spezifikation löschen" disabled="">` at slowness step 5.
    // Waiting first also moves the mapping-delete check into the settled
    // state, where it is the assertion that matters: before the read lands the
    // label is `t.runtimeSpecDelete` whatever the row turns out to be, so an
    // absent `t.mappingDelete` there says nothing at all.
    await waitForEnabledButton(t.runtimeSpecDelete);
    expect(screen.queryByRole('button', { name: t.mappingDelete })).not.toBeInTheDocument();
    await clickThroughDelete();
    expect(deletedSpecIds).toEqual(['map_1']);
    expect(fakeApi.deleteMapping).not.toHaveBeenCalled();
    expect(deletedMappingIds).toEqual([]);
  });

  // M5, first half: "no cache entry" legitimately means "no spec" for a mapping
  // created through the form whose spec PUT then failed -- `submitCreate` seeds
  // `loadedIdsRef` AND `specLoadSettled` for it, so the state is known, and the
  // partial failure is precisely the case where the operator wants the mapping
  // gone again. The gate must not take that away.
  it('keeps the mapping delete on a created mapping whose spec PUT failed', async () => {
    const { fakeApi, created, deletedMappingIds, deletedSpecIds } = renderSection({
      mappings: [],
    });

    fireEvent.click(await screen.findByRole('button', { name: t.runtimeSpecCreate }));
    fireEvent.change(screen.getByLabelText(t.mappingGatewayName), { target: { value: 'gw-new' } });
    fireEvent.change(screen.getByLabelText(t.mappingAppName), { target: { value: 'app-new' } });
    fireEvent.change(screen.getByLabelText(t.runtimeSpecBinary), {
      target: { value: '/usr/bin/llama-server' },
    });
    // The mapping POST succeeds, the spec PUT does not: a partial failure, so
    // there is genuinely no spec and nothing in the cache to say so.
    fakeApi.putRuntimeSpec.mockImplementationOnce(() => Promise.reject(new Error('spec failed')));
    fireEvent.click(screen.getByRole('button', { name: t.runtimeSpecCreate }));
    await waitFor(() => expect(created).toHaveLength(1));
    await act(async () => {
      await Promise.resolve();
      await Promise.resolve();
    });

    const del = await screen.findByRole('button', { name: t.mappingDelete });
    expect(del).toBeEnabled();
    await clickThroughDelete();
    expect(deletedMappingIds).toEqual(['map_created']);
    expect(fakeApi.deleteRuntimeSpec).not.toHaveBeenCalled();
    expect(deletedSpecIds).toEqual([]);
  });

  // M5, second half: a spec that was just DELETED. `commitSpecCache` writes an
  // `emptySpec` for it, so the entry exists and says "not configured" -- the
  // one case where the cache answers the question negatively by itself. The
  // next Delete on that row is the mapping delete, and must still work.
  it('keeps the mapping delete on a row whose spec was just deleted', async () => {
    const { fakeApi, deletedSpecIds, deletedMappingIds } = renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      specsByMappingId: { map_1: fullSpec() },
    });
    await screen.findByText('gw-model');
    await act(async () => {
      await Promise.resolve();
    });

    // Same gate as the test above, and the reason `clickThroughDelete` now
    // insists on its dialog: without the wait, 5/5 runs at slowness step 5 hit
    // the disabled Delete, the click was eaten, the old helper skipped the
    // confirmation it could not find, and the test failed two lines down on
    // `expected [] to deeply equal ['map_1']`.
    await waitForEnabledButton(t.runtimeSpecDelete);
    await clickThroughDelete();
    expect(deletedSpecIds).toEqual(['map_1']);
    // The dialog's exit transition keeps the rest of the screen aria-hidden
    // until it is unmounted.
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());

    const del = await screen.findByRole('button', { name: t.mappingDelete });
    expect(del).toBeEnabled();
    await clickThroughDelete();
    expect(deletedMappingIds).toEqual(['map_1']);
    expect(fakeApi.deleteRuntimeSpec).toHaveBeenCalledTimes(1);
  });

  // The permanent case, and the reason the gate cannot key on `specLoadSettled`
  // alone: the loader's `finally` records a FAILED GET as settled too (it has
  // to -- `specsSettled` gates the `Unmatched` marker and must not wedge). A
  // rejected GET answers nothing, so it must not license the mapping delete
  // either -- and since `loadedIdsRef` never retries, it would license it for
  // the rest of the mount. Edit is the retry: it is non-destructive, left
  // ungated on purpose, and does its own GET.
  it('locks the delete on a row whose spec read FAILED, and Edit re-answers it', async () => {
    const { fakeApi, deletedMappingIds, deletedSpecIds } = renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      specsByMappingId: { map_1: makeSpec({ configured: false, mapping_id: 'map_1' }) },
    });
    fakeApi.runtimeSpec.mockImplementationOnce(() => Promise.reject(new Error('spec GET failed')));
    await screen.findByText('gw-model');
    await act(async () => {
      await Promise.resolve();
      await Promise.resolve();
    });

    const del = rowDelete();
    await clickLockedDelete();
    expect(fakeApi.deleteMapping).not.toHaveBeenCalled();
    expect(deletedMappingIds).toEqual([]);
    expect(deletedSpecIds).toEqual([]);
    expect(del).toBeDisabled();
    // A different sentence from the still-loading one: this state does not
    // resolve itself, so the tooltip names the way out instead of asking for
    // patience.
    fireEvent.mouseOver(del.parentElement as HTMLElement);
    expect(await screen.findByRole('tooltip')).toHaveTextContent(t.runtimeSpecDeleteStateUnknown);

    // Take that way out. The Edit GET answers the question, so the control is
    // no longer dead for the rest of the mount.
    fireEvent.click(screen.getByRole('button', { name: t.runtimeSpecEditAction }));
    await act(async () => {
      await Promise.resolve();
      await Promise.resolve();
    });
    fireEvent.click(await screen.findByRole('button', { name: t.cancel }));

    const retried = await screen.findByRole('button', { name: t.mappingDelete });
    expect(retried).toBeEnabled();
    await clickThroughDelete();
    expect(deletedMappingIds).toEqual(['map_1']);
  });
});

// Sonar typescript:S6479 (task 24e §4.2): both editable row lists were keyed by
// their ARRAY INDEX while supporting mid-list removal (`rows.filter((_, i) => i
// !== idx)`). An index is not an identity there: deleting row n shifts every
// later row down, so React reconciles row n+1's data onto row n's element and
// DESTROYS the last element instead of the deleted one. Everything the DOM node
// owns rather than the props -- focus, caret, selection -- therefore lands on
// the wrong row, or nowhere, exactly when the operator is mid-edit.
//
// Both tests below assert the surviving rows' VALUES (which a controlled input
// renders correctly either way) AND the node identity (which is what the index
// key actually broke): the node the operator was typing into must still be in
// the document, still hold its own row, and still have focus.
describe('RuntimeAdminSection row identity across a mid-list delete (Sonar S6479)', () => {
  it('keeps every surviving GPU row on its own values and its own DOM node', async () => {
    renderSection();

    fireEvent.click(await screen.findByRole('button', { name: t.runtimeSpecCreate }));
    for (let i = 0; i < 3; i += 1) {
      fireEvent.click(screen.getByRole('button', { name: t.runtimeSpecGpuAdd }));
    }
    // addGpuRow proposes indices 0/1/2; give each row a distinct VRAM so a
    // misbinding is visible in the values and not just in the row count.
    ['1000', '2000', '3000'].forEach((mb, i) => {
      fireEvent.change(
        screen.getByLabelText(t.runtimeSpecVram, { selector: `#runtime-spec-gpu-vram-${i}` }),
        { target: { value: mb } },
      );
    });

    // The operator is editing the LAST row when they remove the middle one.
    const editing = screen.getByLabelText(t.runtimeSpecVram, {
      selector: '#runtime-spec-gpu-vram-2',
    }) as HTMLInputElement;
    editing.focus();
    expect(document.activeElement).toBe(editing);

    fireEvent.click(screen.getAllByRole('button', { name: t.runtimeSpecGpuRemove })[1]);

    expect(
      (screen.getAllByLabelText(t.runtimeSpecGpuIndex) as HTMLInputElement[]).map((el) => el.value),
    ).toEqual(['0', '2']);
    expect(
      (screen.getAllByLabelText(t.runtimeSpecVram) as HTMLInputElement[]).map((el) => el.value),
    ).toEqual(['1000', '3000']);
    // The node the operator was in survived the delete, still carries its own
    // row, and kept the caret. With an index key React removes THIS node and
    // rebinds row 2's values onto row 1's node instead.
    expect(editing.isConnected).toBe(true);
    expect(editing.value).toBe('3000');
    expect(document.activeElement).toBe(editing);
  });

  it('keeps every surviving GPU-budget row on its own values and its own DOM node', async () => {
    renderSection({
      gpuBudgets: [
        { index: 0, budget_mb: 10000, expected_uuid: '', expected_name: '' },
        { index: 1, budget_mb: 20000, expected_uuid: '', expected_name: '' },
        { index: 2, budget_mb: 30000, expected_uuid: '', expected_name: '' },
      ],
    });

    fireEvent.click(await screen.findByText(t.runtimeLimits));
    const editing = (await screen.findByLabelText(t.runtimeGpuBudget, {
      selector: '#runtime-budget-mb-2',
    })) as HTMLInputElement;
    editing.focus();
    expect(document.activeElement).toBe(editing);

    fireEvent.click(screen.getAllByRole('button', { name: t.runtimeSpecGpuRemove })[1]);

    expect(
      (screen.getAllByLabelText(t.runtimeSpecGpuIndex) as HTMLInputElement[]).map((el) => el.value),
    ).toEqual(['0', '2']);
    expect(
      (screen.getAllByLabelText(t.runtimeGpuBudget) as HTMLInputElement[]).map((el) => el.value),
    ).toEqual(['10000', '30000']);
    expect(editing.isConnected).toBe(true);
    expect(editing.value).toBe('30000');
    expect(document.activeElement).toBe(editing);
  });
});

// The spec form's GPU rows: pick a reported card by name, get its index.
//
// The case this is designed around is 4x or 8x of the SAME card, which is the
// normal AI-server build rather than an edge case -- a picker labelled by name
// alone would read as eight copies of one string and be useless exactly where
// it is most needed.
describe('RuntimeAdminSection spec GPU picker', () => {
  // Eight identical cards, distinguishable only by index, PCI bus id and UUID
  // -- i.e. the real thing.
  function eightIdenticalCards(): HardwareGPU[] {
    return Array.from({ length: 8 }, (_, i) => ({
      index: i,
      name: 'NVIDIA GeForce RTX 4090',
      uuid: `GPU-${String(i).repeat(8)}-dead-beef`,
      // Real bus ids are hex and non-contiguous, e.g. 0x21, 0x25, 0x29 ...
      pci_bus_id: `00000000:${(0x21 + i * 4).toString(16).padStart(2, '0')}:00.0`,
      memory_total_bytes: 24 * 1024 * 1024 * 1024,
    }));
  }

  async function openSpecFormWithOneGpuRow() {
    fireEvent.click(await screen.findByRole('button', { name: t.runtimeSpecCreate }));
    fireEvent.click(await screen.findByRole('button', { name: t.runtimeSpecGpuAdd }));
  }

  it('selects a card by name and writes its index into the GPU index field', async () => {
    renderSection({
      hardware: makeHardware([
        { index: 0, name: 'RTX 4090', uuid: 'GPU-aaa', memory_total_bytes: 0 },
        { index: 3, name: 'RTX A6000', uuid: 'GPU-bbb', memory_total_bytes: 0 },
      ]),
    });
    await openSpecFormWithOneGpuRow();

    const indexField = (await screen.findByLabelText(t.runtimeSpecGpuIndex)) as HTMLInputElement;
    expect(indexField.value).toBe('0');

    fireEvent.mouseDown(screen.getByRole('combobox', { name: t.runtimeSpecGpuPick }));
    fireEvent.click(await screen.findByRole('option', { name: /RTX A6000/ }));

    expect(indexField.value).toBe('3');
  });

  it('makes eight identically named cards individually distinguishable and picks the right one', async () => {
    renderSection({ hardware: makeHardware(eightIdenticalCards()) });
    await openSpecFormWithOneGpuRow();

    fireEvent.mouseDown(screen.getByRole('combobox', { name: t.runtimeSpecGpuPick }));
    const options = await screen.findAllByRole('option');
    // 8 cards + the placeholder.
    expect(options).toHaveLength(9);

    // THE REQUIREMENT: no two options read the same. A list of eight identical
    // strings fails an operator even though the values behind it differ.
    const labels = options.map((o) => o.textContent ?? '');
    expect(new Set(labels).size).toBe(labels.length);

    // And each one still carries what a human recognises the card by, plus the
    // handle that maps to a physical slot.
    expect(labels).toContain('GPU 5 · NVIDIA GeForce RTX 4090 · 00000000:35:00.0');

    fireEvent.click(screen.getByRole('option', { name: /00000000:35:00\.0/ }));
    expect((screen.getByLabelText(t.runtimeSpecGpuIndex) as HTMLInputElement).value).toBe('5');
  });

  it('falls back to a shortened UUID when the agent reports no PCI bus id', async () => {
    renderSection({
      hardware: makeHardware([
        {
          index: 0,
          name: 'RTX 4090',
          uuid: 'GPU-a1b2c3d4-5e6f-7788-99aa-bbccddeeff00',
          memory_total_bytes: 0,
        },
        {
          index: 1,
          name: 'RTX 4090',
          uuid: 'GPU-99887766-5e6f-7788-99aa-bbccddeeff00',
          memory_total_bytes: 0,
        },
      ]),
    });
    await openSpecFormWithOneGpuRow();

    fireEvent.mouseDown(screen.getByRole('combobox', { name: t.runtimeSpecGpuPick }));
    const labels = (await screen.findAllByRole('option')).map((o) => o.textContent ?? '');
    expect(labels).toContain('GPU 0 · RTX 4090 · a1b2c3d4…');
    expect(labels).toContain('GPU 1 · RTX 4090 · 99887766…');
  });

  it('still labels every option distinctly when a host reports neither bus id nor UUID (AMD/Apple)', async () => {
    renderSection({
      hardware: makeHardware([
        { index: 0, name: 'Radeon Pro W7900', memory_total_bytes: 0 },
        { index: 1, name: 'Radeon Pro W7900', memory_total_bytes: 0 },
      ]),
    });
    await openSpecFormWithOneGpuRow();

    fireEvent.mouseDown(screen.getByRole('combobox', { name: t.runtimeSpecGpuPick }));
    const labels = (await screen.findAllByRole('option')).map((o) => o.textContent ?? '');
    expect(labels).toContain('GPU 0 · Radeon Pro W7900');
    expect(labels).toContain('GPU 1 · Radeon Pro W7900');
  });

  // A machine that has never reported, or a CPU-only host: no picker at all
  // (an empty dropdown reads as broken), a sentence saying why, and the index
  // field still fully usable -- this branch has twice shipped a control that
  // failed closed when a resource had not resolved.
  it('omits the picker and explains itself when the server has reported no GPUs, while manual entry still works', async () => {
    const { putSpecs, created } = renderSection(); // default hardware: { available: false }
    await openSpecFormWithOneGpuRow();

    expect(await screen.findByText(t.runtimeSpecGpuNoTelemetry)).toBeInTheDocument();
    expect(screen.queryByRole('combobox', { name: t.runtimeSpecGpuPick })).not.toBeInTheDocument();

    const indexField = screen.getByLabelText(t.runtimeSpecGpuIndex);
    expect(indexField).not.toBeDisabled();
    fireEvent.change(indexField, { target: { value: '2' } });
    fireEvent.change(screen.getByLabelText(t.runtimeSpecVram), { target: { value: '18000' } });

    fireEvent.change(screen.getByLabelText(t.mappingGatewayName), { target: { value: 'gw' } });
    fireEvent.change(screen.getByLabelText(t.mappingAppName), { target: { value: 'app' } });
    fireEvent.change(screen.getByLabelText(t.runtimeSpecBinary), {
      target: { value: '/usr/bin/llama-server' },
    });
    fireEvent.click(screen.getByRole('button', { name: t.runtimeSpecCreate }));

    await waitFor(() => expect(created).toHaveLength(1));
    await waitFor(() => expect(putSpecs).toHaveLength(1));
    expect(putSpecs[0].body.gpus).toEqual([
      { index: 2, vram_estimate_mb: 18000, vram_measured_mb: 0 },
    ]);
  });

  // duplicateGpuIndex refuses a collision at submit and the backend refuses it
  // again, so offering an index only to fail validation afterwards is a worse
  // experience than not offering it.
  it("does not offer an index a sibling row already uses, but keeps offering the row's own", async () => {
    renderSection({
      hardware: makeHardware([
        { index: 0, name: 'Card A', memory_total_bytes: 0 },
        { index: 1, name: 'Card B', memory_total_bytes: 0 },
        { index: 2, name: 'Card C', memory_total_bytes: 0 },
      ]),
    });
    fireEvent.click(await screen.findByRole('button', { name: t.runtimeSpecCreate }));
    const addGpu = await screen.findByRole('button', { name: t.runtimeSpecGpuAdd });
    fireEvent.click(addGpu); // row 0 -> index 0
    fireEvent.click(addGpu); // row 1 -> index 1

    const pickers = screen.getAllByRole('combobox', { name: t.runtimeSpecGpuPick });
    expect(pickers).toHaveLength(2);

    fireEvent.mouseDown(pickers[0]);
    const labels = (await screen.findAllByRole('option')).map((o) => o.textContent ?? '');
    expect(labels).toContain('GPU 0 · Card A'); // its own -- still shown, or the select reads as unset
    expect(labels).toContain('GPU 2 · Card C');
    expect(labels.some((l) => l.includes('Card B'))).toBe(false); // held by row 1
  });

  // Telemetry can be behind reality. A hand-typed index for a card the server
  // has not reported must not make the picker claim something else is selected.
  it('leaves the picker on its placeholder for a hand-typed index telemetry does not know', async () => {
    renderSection({
      hardware: makeHardware([{ index: 0, name: 'Card A', memory_total_bytes: 0 }]),
    });
    await openSpecFormWithOneGpuRow();

    fireEvent.change(screen.getByLabelText(t.runtimeSpecGpuIndex), { target: { value: '7' } });

    expect((screen.getByLabelText(t.runtimeSpecGpuIndex) as HTMLInputElement).value).toBe('7');
    expect(screen.getByRole('combobox', { name: t.runtimeSpecGpuPick }).textContent).toBe(
      t.runtimeSpecGpuPickPlaceholder,
    );
  });
});

// ---------------------------------------------------------------------------
// set_visible_devices: the checkbox that turns the GPU rows from a declaration
// into an enforcement.
// ---------------------------------------------------------------------------

describe('RuntimeAdminSection set_visible_devices', () => {
  // Fills the create form's mandatory fields and turns the checkbox on.
  // Returns nothing: every assertion below is about what the form did with it.
  async function openCreateWithVisibleDevices() {
    fireEvent.click(await screen.findByRole('button', { name: t.runtimeSpecCreate }));
    fireEvent.change(screen.getByLabelText(t.mappingGatewayName), { target: { value: 'gw' } });
    fireEvent.change(screen.getByLabelText(t.mappingAppName), { target: { value: 'app' } });
    fireEvent.change(screen.getByLabelText(t.runtimeSpecBinary), {
      target: { value: '/usr/bin/llama-server' },
    });
    fireEvent.click(screen.getByLabelText(t.runtimeSpecSetVisibleDevices));
  }

  it('sends the flag with the GPU rows it applies to', async () => {
    const { putSpecs } = renderSection();
    await openCreateWithVisibleDevices();
    fireEvent.click(screen.getByRole('button', { name: t.runtimeSpecGpuAdd }));
    fireEvent.change(screen.getByLabelText(t.runtimeSpecGpuIndex), { target: { value: '3' } });

    fireEvent.click(screen.getByRole('button', { name: t.runtimeSpecCreate }));

    await waitFor(() => expect(putSpecs).toHaveLength(1));
    expect(putSpecs[0].body.set_visible_devices).toBe(true);
    expect(putSpecs[0].body.gpus).toEqual([{ index: 3, vram_estimate_mb: 0, vram_measured_mb: 0 }]);
  });

  // Trap 2 lives here and nowhere else: nothing can ENFORCE that an argument
  // naming a device number is written in the child's numbering, because the
  // agent cannot parse an arbitrary model server's argv. Saying it at the
  // checkbox is the whole of the mitigation, so its absence is a regression.
  it('states the child-side renumbering where the option is turned on', async () => {
    renderSection();
    fireEvent.click(await screen.findByRole('button', { name: t.runtimeSpecCreate }));
    const hint = await screen.findByText(t.runtimeSpecSetVisibleDevicesHint);
    expect(hint).toBeInTheDocument();
    // Not merely present -- it has to actually carry the consequence.
    expect(t.runtimeSpecSetVisibleDevicesHint).toMatch(/0/);
    expect(t.runtimeSpecSetVisibleDevicesHint).toContain('--main-gpu');
  });

  // Trap 1: an EMPTY visibility value does not mean "no restriction", it means
  // nothing is visible. Caught before the round trip, like the reserved-env-key
  // and duplicate-GPU-index checks.
  it('refuses the option with no GPU rows before ever calling the API', async () => {
    const { created, putSpecs } = renderSection();
    await openCreateWithVisibleDevices();

    fireEvent.click(screen.getByRole('button', { name: t.runtimeSpecCreate }));

    expect(await screen.findByText(t.errorRuntimeSpecVisibleDevicesNoGpus)).toBeInTheDocument();
    expect(created).toHaveLength(0);
    expect(putSpecs).toHaveLength(0);
  });

  // Trap 3, in every spelling and for all three owned variables. HIP is in the
  // list although the agent never sets it: it selects from what ROCR already
  // left visible, so a hand-set HIP list on top of agent-managed ROCR
  // filtering leaves the child with no usable device.
  it.each([
    'CUDA_VISIBLE_DEVICES',
    'cuda_visible_devices',
    'ROCR_VISIBLE_DEVICES',
    'Hip_Visible_Devices',
  ])('refuses the option alongside a hand-set %s', async (key) => {
    const { created, putSpecs } = renderSection();
    await openCreateWithVisibleDevices();
    fireEvent.click(screen.getByRole('button', { name: t.runtimeSpecGpuAdd }));
    fireEvent.change(screen.getByLabelText(t.runtimeSpecEnv), {
      target: { value: `${key}=0,1` },
    });

    fireEvent.click(screen.getByRole('button', { name: t.runtimeSpecCreate }));

    expect(
      await screen.findByText(new RegExp(t.errorRuntimeSpecVisibleDevicesConflict.slice(0, 24))),
    ).toBeInTheDocument();
    expect(created).toHaveLength(0);
    expect(putSpecs).toHaveLength(0);
  });

  // The same variable is fine when the option is OFF: the refusal is about two
  // sources for one value, never about the variable itself. An operator who
  // wants to pin devices by hand still can.
  it('accepts a hand-set visibility variable when the option is off', async () => {
    const { putSpecs } = renderSection();
    fireEvent.click(await screen.findByRole('button', { name: t.runtimeSpecCreate }));
    fireEvent.change(screen.getByLabelText(t.mappingGatewayName), { target: { value: 'gw' } });
    fireEvent.change(screen.getByLabelText(t.mappingAppName), { target: { value: 'app' } });
    fireEvent.change(screen.getByLabelText(t.runtimeSpecBinary), {
      target: { value: '/usr/bin/llama-server' },
    });
    fireEvent.change(screen.getByLabelText(t.runtimeSpecEnv), {
      target: { value: 'CUDA_VISIBLE_DEVICES=0,1' },
    });

    fireEvent.click(screen.getByRole('button', { name: t.runtimeSpecCreate }));

    await waitFor(() => expect(putSpecs).toHaveLength(1));
    expect(putSpecs[0].body.set_visible_devices).toBe(false);
    expect(putSpecs[0].body.env).toEqual({ CUDA_VISIBLE_DEVICES: '0,1' });
  });

  // ONEAPI_DEVICE_SELECTOR is deliberately NOT owned: composing it with the
  // option is the documented escape hatch for a runtime the agent has no
  // vendor mapping for, so refusing it would break the very thing
  // ${HOST_GPU_IDS} exists for.
  it('lets ONEAPI_DEVICE_SELECTOR compose with the option', async () => {
    const { putSpecs } = renderSection();
    await openCreateWithVisibleDevices();
    fireEvent.click(screen.getByRole('button', { name: t.runtimeSpecGpuAdd }));
    fireEvent.change(screen.getByLabelText(t.runtimeSpecEnv), {
      target: { value: 'ONEAPI_DEVICE_SELECTOR=level_zero:${HOST_GPU_IDS}' },
    });

    fireEvent.click(screen.getByRole('button', { name: t.runtimeSpecCreate }));

    await waitFor(() => expect(putSpecs).toHaveLength(1));
    expect(putSpecs[0].body.set_visible_devices).toBe(true);
    expect(putSpecs[0].body.env).toEqual({
      ONEAPI_DEVICE_SELECTOR: 'level_zero:${HOST_GPU_IDS}',
    });
  });

  // Hydration: the edit form must show the STORED value, or a save would
  // silently turn the option off on a spec that had it on.
  it('hydrates the checkbox from the stored spec', async () => {
    renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      specsByMappingId: { map_1: fullSpec() },
    });
    fireEvent.click(await screen.findByRole('button', { name: t.runtimeSpecEditAction }));
    await act(async () => {
      await Promise.resolve();
      await Promise.resolve();
    });
    const checkbox = screen.getByLabelText(t.runtimeSpecSetVisibleDevices) as HTMLInputElement;
    expect(checkbox.checked).toBe(true);
  });
});

/**
 * The model-mapping tab: the SAME table and the SAME edit mask an ordinary
 * application gets (`MappingSection`), minus the actions that would mint or
 * destroy rows the launch specs depend on -- and with the ownership boundary
 * enforced in the one direction the spec form does not enforce it.
 */
describe('RuntimeAdminSection model-mapping tab', () => {
  it('places the model-mapping tab to the LEFT of the runtime specs', async () => {
    renderSection({ mappings: [makeMapping({ id: 'map_1' })] });
    await screen.findByText('gw-model');
    // STRUCTURAL: pins the tab's existence AND its position, which is what was
    // asked for ("links vom RUNTIME-SPEZIFIKATIONEN").
    expect(screen.getAllByRole('tab').map((el) => el.textContent)).toEqual([
      t.runtimeMappingTab,
      t.runtimeSpecs,
      t.runtimeMatrix,
      t.runtimeLimits,
      t.runtimeLiveStatus,
    ]);
  });

  it("labels the two tabs' status columns with two different words", async () => {
    renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      specsByMappingId: { map_1: makeSpec({ configured: true, mapping_id: 'map_1' }) },
    });
    await screen.findByText('gw-model');

    // The specs tab is open on mount. Its status column means the PROCESS's
    // running/stopped/unknown, so it must NOT be the bare `Status` the tab one
    // to the left uses for the MAPPING's active/disabled -- two adjacent tabs
    // must not label two different facts with one word. Both directions are
    // asserted on purpose: only the negative fails if the column is relabelled
    // back to `tableStatus`, and only the positive fails if it drifts to some
    // third string.
    expect(
      await screen.findByRole('columnheader', { name: t.runtimeLiveStatus }),
    ).toBeInTheDocument();
    expect(screen.queryByRole('columnheader', { name: t.tableStatus })).toBeNull();

    fireEvent.click(screen.getByRole('tab', { name: t.runtimeMappingTab }));
    expect(await screen.findByRole('columnheader', { name: t.tableStatus })).toBeInTheDocument();
    expect(screen.queryByRole('columnheader', { name: t.runtimeLiveStatus })).toBeNull();
  });

  it('shows the mapping table without the actions that would break a launch spec', async () => {
    renderSection({
      mappings: [makeMapping({ id: 'map_1', gateway_model_name: 'gw-model' })],
      specsByMappingId: { map_1: makeSpec({ configured: true, mapping_id: 'map_1' }) },
    });
    await screen.findByText('gw-model');
    fireEvent.click(screen.getByRole('tab', { name: t.runtimeMappingTab }));

    // The mapping table's own columns, and NOT the specs table's.
    expect(await screen.findByRole('columnheader', { name: t.mappingAppName })).toBeInTheDocument();
    expect(screen.getByText(t.statusActive)).toBeInTheDocument();
    expect(screen.queryByRole('columnheader', { name: t.runtimeSpecBinary })).toBeNull();

    // BEHAVIOURAL: edit and the status toggle are offered...
    expect(screen.getByRole('button', { name: t.mappingEdit })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: t.tokenActionDisable })).toBeInTheDocument();
    // ...and nothing that creates or destroys a row. Delete is what the
    // operator asked to drop; create/sync/benchmark are the same argument
    // (a mapping created here would have no spec, a sync disables every
    // mapping whose app model name the agent does not list).
    expect(screen.queryByRole('button', { name: t.mappingDelete })).toBeNull();
    expect(screen.queryByRole('button', { name: t.mappingCreate })).toBeNull();
    expect(screen.queryByRole('button', { name: t.syncModels })).toBeNull();
    expect(screen.queryByRole('button', { name: t.runBenchmark })).toBeNull();
    expect(screen.getByText(t.runtimeMappingCreateHint)).toBeInTheDocument();
  });

  it('edits the two fields the mapping owns and never sends the one the spec owns', async () => {
    const { updatedMappings } = renderSection({
      mappings: [
        makeMapping({ id: 'map_1', gateway_model_name: 'gw-model', app_model_name: 'app-old' }),
      ],
    });
    await screen.findByText('gw-model');
    fireEvent.click(screen.getByRole('tab', { name: t.runtimeMappingTab }));
    fireEvent.click(await screen.findByRole('button', { name: t.mappingEdit }));
    await screen.findByLabelText(t.mappingGatewayName);

    // STRUCTURAL: the ownership boundary, read off the DOM attribute rather
    // than by typing -- jsdom accepts fireEvent.change on a readOnly input.
    expect(document.querySelector('#mapping-app-name')).toHaveAttribute('readonly');
    expect(document.querySelector('#mapping-gateway-name')).not.toHaveAttribute('readonly');
    expect(screen.getByText(t.mappingAppNameReadOnly)).toBeInTheDocument();

    fireEvent.change(screen.getByLabelText(t.mappingGatewayName), {
      target: { value: 'gw-renamed' },
    });
    // A non-native MUI Select: open it, then click the option. `fireEvent.change`
    // has no value setter to drive here.
    fireEvent.mouseDown(screen.getByRole('combobox', { name: t.tableStatus }));
    fireEvent.click(await screen.findByRole('option', { name: t.statusDisabled }));
    fireEvent.click(screen.getByRole('button', { name: t.mappingSave }));

    await waitFor(() => expect(updatedMappings).toHaveLength(1));
    // BEHAVIOURAL: the PATCH is pointer-gated per field, so an ABSENT key
    // carries no value for that field -- which is what makes the two screens
    // non-overlapping WRITERS. This pins the BODY, and nothing more: the
    // backend re-writes the whole row it loaded, so two PATCHes in flight at
    // once still lose an update no matter which keys each one names
    // (11-risks-and-technical-debt.md §11.1). Do not read a green here as
    // "the race is closed".
    expect(updatedMappings[0].body.gateway_model_name).toBe('gw-renamed');
    expect(updatedMappings[0].body.status).toBe('disabled');
    expect(updatedMappings[0].body).not.toHaveProperty('app_model_name');
  });

  it("runs the shared mask's context probe on this tab and fills the field", async () => {
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

    const { updatedMappings } = renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      // The probe button is gated on the APPLICATION's probe path, and the
      // module default has none.
      application: { ...application, context_probe_path: '/props' },
      probeMappingContext,
      benchmarkStatus,
    });
    await screen.findByText('gw-model');
    fireEvent.click(screen.getByRole('tab', { name: t.runtimeMappingTab }));
    fireEvent.click(await screen.findByRole('button', { name: t.mappingEdit }));

    // "Der selbe Edit" includes the probe, and the probe is the one part of the
    // shared mask with an async loop -- which is why `pollIntervalMs` is
    // forwarded to `MappingForm` from here as well as from `MappingSection`.
    // Without that forward this test cannot exist: the poll falls back to the
    // shared helper's ~2 s cadence and the assertion below times out.
    const probeBtn = await screen.findByRole('button', { name: t.mappingProbeContext });
    await waitFor(() => expect(probeBtn).toBeEnabled());
    fireEvent.click(probeBtn);

    await waitFor(() => expect(probeMappingContext).toHaveBeenCalledWith('map_1'));
    await waitFor(() =>
      expect((screen.getByLabelText(t.mappingContextSize) as HTMLInputElement).value).toBe('8192'),
    );
    // Fill only -- the operator still saves. Identical to the ordinary screen.
    expect(updatedMappings).toHaveLength(0);
  });

  it('toggles a mapping status with a status-only PATCH', async () => {
    const { updatedMappings } = renderSection({
      mappings: [makeMapping({ id: 'map_1', status: 'active' })],
    });
    await screen.findByText('gw-model');
    fireEvent.click(screen.getByRole('tab', { name: t.runtimeMappingTab }));
    fireEvent.click(await screen.findByRole('button', { name: t.tokenActionDisable }));

    await waitFor(() => expect(updatedMappings).toHaveLength(1));
    expect(updatedMappings[0].body).toEqual({ status: 'disabled' });
    expect(await screen.findByText(t.statusDisabled)).toBeInTheDocument();
  });

  it('shares ONE copy of the rows with the specs tab -- no refetch, no stale name', async () => {
    const { updatedMappings, mappingsCallCount } = renderSection({
      mappings: [makeMapping({ id: 'map_1', gateway_model_name: 'gw-model' })],
      specsByMappingId: { map_1: makeSpec({ configured: true, mapping_id: 'map_1' }) },
    });
    await screen.findByText('gw-model');
    fireEvent.click(screen.getByRole('tab', { name: t.runtimeMappingTab }));
    fireEvent.click(await screen.findByRole('button', { name: t.mappingEdit }));
    await screen.findByLabelText(t.mappingGatewayName);
    fireEvent.change(screen.getByLabelText(t.mappingGatewayName), {
      target: { value: 'gw-renamed' },
    });
    fireEvent.click(screen.getByRole('button', { name: t.mappingSave }));
    await waitFor(() => expect(updatedMappings).toHaveLength(1));

    // BEHAVIOURAL: the specs table beside it joins against the same rows, so
    // the rename is there immediately -- and no second GET was needed to get
    // it there. Both halves fail if the tab keeps its own copy of the list.
    fireEvent.click(screen.getByRole('tab', { name: t.runtimeSpecs }));
    expect(await screen.findByText('gw-renamed')).toBeInTheDocument();
    expect(mappingsCallCount()).toBe(1);
  });

  it('reports a gateway-name conflict and keeps the form open', async () => {
    const { fakeApi } = renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
    });
    await screen.findByText('gw-model');
    fakeApi.updateMapping.mockRejectedValueOnce(
      new PortalApiError(409, 'mapping.gateway_name_conflict', 'taken'),
    );
    fireEvent.click(screen.getByRole('tab', { name: t.runtimeMappingTab }));
    fireEvent.click(await screen.findByRole('button', { name: t.mappingEdit }));
    await screen.findByLabelText(t.mappingGatewayName);
    fireEvent.change(screen.getByLabelText(t.mappingGatewayName), { target: { value: 'taken' } });
    fireEvent.click(screen.getByRole('button', { name: t.mappingSave }));

    // This tab is now the ONLY place an existing mapping's gateway name can be
    // renamed, so its 409 has to be readable here.
    expect(
      await screen.findByText(t.errorMappingGatewayNameConflict, { exact: false }),
    ).toBeInTheDocument();
    expect(screen.getByLabelText(t.mappingGatewayName)).toBeInTheDocument();
  });

  it('stays writable in file mode, where every other tab is read-only', async () => {
    renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      report: fileModeReport({ specs: [] }),
    });
    await screen.findByText(t.runtimeManagedLocally);
    // Existing behaviour: no spec create button in file mode.
    expect(screen.queryByRole('button', { name: t.runtimeSpecCreate })).toBeNull();

    fireEvent.click(screen.getByRole('tab', { name: t.runtimeMappingTab }));
    // DELIBERATE non-gating: a mapping is a gateway ROUTE and exists whether or
    // not the agent manages its processes from a local file, so taking a model
    // out of service must stay possible here. ADR-029's "no write control
    // before its own GET" governs full-document PUTs; the mapping PATCH merges.
    expect(await screen.findByRole('button', { name: t.mappingEdit })).toBeEnabled();
    expect(screen.getByRole('button', { name: t.tokenActionDisable })).toBeEnabled();
  });
});

/**
 * The other half of the same boundary: the launch-spec form stops pretending to
 * own the two mapping fields it never wrote through its own endpoint.
 */
describe('RuntimeAdminSection spec form ownership', () => {
  it('locks the gateway model name on EDIT and drops the status select entirely', async () => {
    renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      specsByMappingId: { map_1: makeSpec({ configured: true, mapping_id: 'map_1' }) },
    });
    fireEvent.click(await screen.findByRole('button', { name: t.runtimeSpecEditAction }));
    // Await something that proves the form rendered before asserting a NEGATIVE.
    await screen.findByLabelText(t.runtimeSpecBinary);

    expect(document.querySelector('#runtime-spec-gateway-name')).toHaveAttribute('readonly');
    expect(screen.getByText(t.runtimeSpecGatewayNameReadOnly)).toBeInTheDocument();
    // The application model name is the spec's `upstream_model`; this form owns it.
    expect(document.querySelector('#runtime-spec-app-name')).not.toHaveAttribute('readonly');
    // By ID, not by the `t.tableStatus` label: that string also labels the
    // specs table's live-state column and the mapping tab's status column.
    expect(document.querySelector('#runtime-spec-status')).toBeNull();
  });

  it('sends ONLY the application model name to the mapping on a spec edit', async () => {
    const { updatedMappings, putSpecs } = renderSection({
      mappings: [
        makeMapping({ id: 'map_1', gateway_model_name: 'gw-model', app_model_name: 'app-old' }),
      ],
      specsByMappingId: {
        map_1: makeSpec({ configured: true, mapping_id: 'map_1', binary: '/usr/bin/old' }),
      },
    });
    fireEvent.click(await screen.findByRole('button', { name: t.runtimeSpecEditAction }));
    await screen.findByLabelText(t.runtimeSpecBinary);
    fireEvent.change(screen.getByLabelText(t.runtimeSpecBinary), {
      target: { value: '/usr/bin/new' },
    });
    fireEvent.click(screen.getByRole('button', { name: t.save }));

    await waitFor(() => expect(putSpecs).toHaveLength(1));
    // BEHAVIOURAL: one key. The gateway name is read-only here and the status
    // is not shown at all -- a form does not send a field it does not let you
    // edit, or it silently reverts whatever the other screen just did.
    expect(updatedMappings[0].body).toEqual({ app_model_name: 'app-old' });
  });

  it('does NOT re-enable a disabled mapping when only the launch config changes', async () => {
    const { updatedMappings } = renderSection({
      mappings: [makeMapping({ id: 'map_1', status: 'disabled' })],
      specsByMappingId: {
        map_1: makeSpec({ configured: true, mapping_id: 'map_1', binary: '/usr/bin/old' }),
      },
    });
    fireEvent.click(await screen.findByRole('button', { name: t.runtimeSpecEditAction }));
    await screen.findByLabelText(t.runtimeSpecBinary);
    fireEvent.change(screen.getByLabelText(t.runtimeSpecBinary), {
      target: { value: '/usr/bin/new' },
    });
    fireEvent.click(screen.getByRole('button', { name: t.save }));

    await waitFor(() => expect(updatedMappings).toHaveLength(1));
    // THE regression guard. `status` defaults to 'active' in this component,
    // so leaving the key in the body while removing the control would PATCH a
    // deliberately disabled model back into service on the next spec save --
    // no error, no diff, and no column on the specs tab that contradicts it.
    // Survives the future cleanup that deletes the hydration line as dead code.
    expect(Object.prototype.hasOwnProperty.call(updatedMappings[0].body, 'status')).toBe(false);
    // ...and it is still disabled on the tab that owns that field.
    fireEvent.click(screen.getByRole('tab', { name: t.runtimeMappingTab }));
    expect(await screen.findByText(t.statusDisabled)).toBeInTheDocument();
  });

  it('keeps the gateway model name editable on CREATE and sends no status', async () => {
    const { created } = renderSection();
    fireEvent.click(await screen.findByRole('button', { name: t.runtimeSpecCreate }));

    // CREATE is the exception, and not for cosmetic reasons: this form creates
    // the MAPPING first and keys the spec PUT by the id it returns, the backend
    // refuses an empty gateway name, and this is the ONLY mapping-create path a
    // server_agent application has.
    const gateway = document.querySelector('#runtime-spec-gateway-name');
    expect(gateway).not.toHaveAttribute('readonly');
    expect(gateway).toHaveAttribute('required');
    expect(document.querySelector('#runtime-spec-status')).toBeNull();

    fireEvent.change(screen.getByLabelText(t.mappingGatewayName), { target: { value: 'gw-new' } });
    fireEvent.change(screen.getByLabelText(t.mappingAppName), { target: { value: 'app-new' } });
    fireEvent.change(screen.getByLabelText(t.runtimeSpecBinary), {
      target: { value: '/usr/bin/llama-server' },
    });
    fireEvent.click(screen.getByRole('button', { name: t.runtimeSpecCreate }));

    await waitFor(() => expect(created).toHaveLength(1));
    // No status: CreateMapping normalises an absent status to active, which is
    // byte-for-byte what the removed hard-coded 'active' produced.
    expect(created[0]).toEqual({ gateway_model_name: 'gw-new', app_model_name: 'app-new' });
  });
});
