// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useState, useEffect, useRef, type SubmitEvent } from 'react';
import { Alert, Box, Button, Typography } from '@mui/material';
import AddIcon from '@mui/icons-material/Add';
import AppsIcon from '@mui/icons-material/Apps';
import EditIcon from '@mui/icons-material/Edit';
import DeleteIcon from '@mui/icons-material/Delete';
import type {
  ApplicationHealthMode,
  ApplicationScheme,
  ApplicationStatus,
  ApplicationType,
  CreateApplicationRequest,
  PortalApplication,
  PortalServer,
  UpdateApplicationRequest,
} from '../api';
import type { BadgeStatus, Translation, PortalApi } from './shared/types';
import { formatPortalError, formatDate } from './shared/format';
import { useResource } from './shared/useResource';
import { StatusChip } from './shared/StatusChip';
import { Panel } from './shared/Panel';
import { Field } from './shared/Field';
import { SelectField } from './shared/SelectField';
import { CheckboxGroup } from './shared/CheckboxGroup';
import { ConfirmDialog } from './shared/ConfirmDialog';
import { Breadcrumbs, type BreadcrumbItem } from './shared/Breadcrumbs';
import { ListTable, listTableLabels, type ListColumn } from './shared/ListTable';
import type { RowAction } from './shared/RowActionsMenu';
import { useToast } from './shared/ToastProvider';
import { applicationStatusOptions, applicationStatusLabelByKey } from './shared/application';
import { applicationTypeDefaults, migrateTypeFields } from './shared/applicationTypeDefaults';
import { MappingSection } from './MappingSection';
import { RuntimeAdminSection } from './RuntimeAdminSection';

const applicationTypeOptions: ApplicationType[] = [
  'ollama',
  'vllm',
  'llama_cpp',
  'llama_swap',
  'litellm',
  'server_agent',
];
const applicationSchemeOptions: ApplicationScheme[] = ['http', 'https'];
const applicationFlavorOptions = ['openai', 'anthropic'];

// Three states, not two. The previous binary derivation rendered four
// materially different situations as one amber "Not proxied" -- waiting for a
// port, waiting for the switch, running its own TLS, and on a server that runs
// no proxy at all -- and the operator's opt-out would have made it five.
//
// 'proxied' still mirrors routing.ApplicationEndpoint's own branch exactly
// (scheme=="https" && proxy_listen_port!=0), which is a statement about the
// LISTENER. 'excluded' is the separate, operator-owned fact, so it is read from
// the flag and tested first.
//
// Known residual, deliberately out of scope: 'pending' still merges "waiting
// for a port" with "this server runs no proxy".
type ApplicationProxyState = 'excluded' | 'proxied' | 'pending';

function applicationProxyState(app: PortalApplication): ApplicationProxyState {
  if (app.proxy_excluded) return 'excluded';
  return app.scheme === 'https' && app.proxy_listen_port !== 0 ? 'proxied' : 'pending';
}

const applicationProxyStateLabelByKey: Record<
  ApplicationProxyState,
  'applicationProxyChipExcluded' | 'applicationProxied' | 'applicationNotProxied'
> = {
  excluded: 'applicationProxyChipExcluded',
  proxied: 'applicationProxied',
  // Reused on purpose so no existing string is orphaned.
  pending: 'applicationNotProxied',
};

const applicationProxyStateChipStatus: Record<ApplicationProxyState, BadgeStatus> = {
  // Grey, not amber: a deliberate operator choice is not a warning.
  excluded: 'standby',
  proxied: 'success',
  pending: 'watch',
};

const defaultApplicationAffinityTtlSeconds = 1800;
const defaultApplicationAdmissionQueueTimeoutSeconds = 0;
const defaultApplicationHealthPath = '/v1/health';
const defaultApplicationHealthMode: ApplicationHealthMode = 'health_path';

const applicationHealthModeOptions: ApplicationHealthMode[] = [
  'health_path',
  'always_reachable',
  'model_sync',
];
const healthModeLabelByKey: Record<
  ApplicationHealthMode,
  'applicationHealthModePath' | 'applicationAlwaysReachable' | 'applicationHealthModeModelSync'
> = {
  health_path: 'applicationHealthModePath',
  always_reachable: 'applicationAlwaysReachable',
  model_sync: 'applicationHealthModeModelSync',
};

// Seeded into the "Custom" interval input; the actual custom value replaces it
// on edit. 0 is never a valid custom value (0 means "follow the system setting").
const defaultCustomHealthIntervalSeconds = 30;

// Seeded into the scheduled-benchmark interval input when the toggle is enabled
// but no interval was set yet (default: once per day).
const defaultBenchmarkIntervalSeconds = 86400;

type Mode =
  | 'list'
  | 'create'
  | { kind: 'edit'; app: PortalApplication }
  | { kind: 'mappings'; app: PortalApplication };

function toggleFlavor(list: string[], flavor: string): string[] {
  return list.includes(flavor) ? list.filter((item) => item !== flavor) : [...list, flavor];
}

export function ApplicationSection({
  t,
  api,
  server,
  onModelsChanged,
  trail = [],
}: Readonly<{
  t: Translation;
  api: Pick<
    PortalApi,
    | 'activeBenchmarks'
    | 'applications'
    | 'benchmarkApplication'
    | 'benchmarkMapping'
    | 'benchmarkServer'
    | 'benchmarkStatus'
    | 'createApplication'
    | 'createMapping'
    | 'deleteApplication'
    | 'deleteMapping'
    | 'healthCheckInterval'
    | 'mappingBenchmarks'
    | 'mappings'
    | 'probeMappingContext'
    | 'subscribeBenchmark'
    | 'syncApplicationModels'
    | 'updateApplication'
    | 'updateMapping'
    // Agent-runtime-manager (Task 20): forwarded to RuntimeAdminSection when a
    // server_agent application's "manage models" drill-down opens it instead
    // of MappingSection.
    | 'runtimeSpec'
    | 'putRuntimeSpec'
    | 'deleteRuntimeSpec'
    | 'runtimeCoresidency'
    | 'putRuntimeCoresidency'
    | 'runtimeWarnings'
    | 'gpuBudgets'
    | 'putGpuBudgets'
    | 'runtimeReport'
    | 'subscribeRuntimeStatus'
    | 'subscribeRuntimeLogs'
    // Task 21 (matrix + server limits): forwarded the same way.
    | 'updateServer'
    | 'serverHardware'
  >;
  server: PortalServer;
  onModelsChanged?: () => void;
  trail?: BreadcrumbItem[];
}>) {
  const {
    data: applicationsData,
    setData: setApplications,
    error,
    loading,
  } = useResource(() => api.applications(server.id).then((r) => r.data), [api, server.id, t], t);
  const { showError } = useToast();
  useEffect(() => {
    if (error) showError(error);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [error]);
  const applications = applicationsData ?? [];
  const managedRuntimeOnly = Boolean(server.managed_runtime_only);
  const serverAgentApps = applications.filter((a) => a.type === 'server_agent');

  const [mode, setMode] = useState<Mode>('list');

  // managed_runtime_only mirrors the backend's own gate (Task 6): a server
  // restricted this way only ever needs ONE server_agent application (its
  // model mappings, each with a launch spec, live underneath it) -- so once
  // that one application exists there is nothing else to click through to on
  // the list, and the operator lands straight in its runtime admin. Guarded
  // to fire once, on the first successful load, not on every poll refresh.
  const autoDrilledRef = useRef(false);
  useEffect(() => {
    if (autoDrilledRef.current || !managedRuntimeOnly || applicationsData === null) return;
    autoDrilledRef.current = true;
    if (serverAgentApps.length === 1) {
      setMode({ kind: 'mappings', app: serverAgentApps[0] });
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [managedRuntimeOnly, applicationsData]);
  const [busy, setBusy] = useState(false);
  const [confirmingDeleteId, setConfirmingDeleteId] = useState('');

  const [type, setType] = useState<ApplicationType>('ollama');
  const [port, setPort] = useState(11434);
  const [scheme, setScheme] = useState<ApplicationScheme>('http');
  const [flavors, setFlavors] = useState<string[]>([...applicationFlavorOptions]);
  const [status, setStatus] = useState<ApplicationStatus>('active');
  const [priority, setPriority] = useState(0);
  const [weight, setWeight] = useState(0);
  const [timeoutMs, setTimeoutMs] = useState(applicationTypeDefaults.ollama.timeoutMs);
  const [affinityTtl, setAffinityTtl] = useState(defaultApplicationAffinityTtlSeconds);
  const [admissionQueueTimeout, setAdmissionQueueTimeout] = useState(
    defaultApplicationAdmissionQueueTimeoutSeconds,
  );
  const [healthMode, setHealthMode] = useState<ApplicationHealthMode>(defaultApplicationHealthMode);
  const [healthPath, setHealthPath] = useState(defaultApplicationHealthPath);
  // "default" = follow the system-wide interval (stores 0); "custom" = fixed value.
  const [healthIntervalMode, setHealthIntervalMode] = useState<'default' | 'custom'>('default');
  const [healthIntervalSeconds, setHealthIntervalSeconds] = useState(
    defaultCustomHealthIntervalSeconds,
  );
  const [nativeResponses, setNativeResponses] = useState(false);
  const [nativeMessages, setNativeMessages] = useState(false);
  const [loadedModelsPath, setLoadedModelsPath] = useState('');
  const [loadedModelsFormat, setLoadedModelsFormat] = useState('auto');
  const [contextProbePath, setContextProbePath] = useState('');
  const [appPathSuffix, setAppPathSuffix] = useState('');
  const [apiTokenHeader, setApiTokenHeader] = useState('');
  // Write-only upstream API token: empty input = keep the stored token,
  // a typed value = replace, cleared flag = send "" (clear).
  const [tokenInput, setTokenInput] = useState('');
  const [tokenCleared, setTokenCleared] = useState(false);
  // The operator's opt-out, plus the value the form OPENED with. The seed is
  // what submitEdit diffs against so an unrelated save never restates it -- see
  // submitEdit below, and ServerList's managed_runtime_only for the same
  // reasoning at length.
  const [proxyExcluded, setProxyExcluded] = useState(false);
  const [proxyExcludedSeed, setProxyExcludedSeed] = useState(false);
  const [benchmarkScheduleEnabled, setBenchmarkScheduleEnabled] = useState(false);
  const [opportunisticMetricsEnabled, setOpportunisticMetricsEnabled] = useState(false);
  // What the gateway's TLS proxy is doing on THIS server. The `?? 'unknown'`
  // default is load-bearing: an older backend, or a DTO that lost the field,
  // must SHOW the control, never hide it.
  const serverProxyState = server.tls_proxy_state ?? 'unknown';
  // Only 'out_of_scope' hides the control, because only 'out_of_scope' is
  // derived from DURABLE state (a stored server column plus a stored setting)
  // and so cannot appear out of nowhere after a gateway restart. 'agent_off'
  // and 'unknown' change the SENTENCE shown, never the visibility -- the
  // agent's cert report is in-RAM and is absent both after every restart and on
  // a proxy-mode agent before its first certificate.
  //
  // OVERRIDE, non-negotiable: an application that IS excluded renders the
  // control in every state, out_of_scope included. An operator must always be
  // able to see and undo their own setting.
  const showProxyControls = serverProxyState !== 'out_of_scope' || proxyExcluded;
  const [benchmarkIntervalSeconds, setBenchmarkIntervalSeconds] = useState(
    defaultBenchmarkIntervalSeconds,
  );
  const [systemHealthIntervalSeconds, setSystemHealthIntervalSeconds] = useState<number | null>(
    null,
  );

  useEffect(() => {
    let cancelled = false;
    api
      .healthCheckInterval()
      .then((r) => {
        if (!cancelled) setSystemHealthIntervalSeconds(r.health_check_interval_seconds);
      })
      .catch(() => {
        /* non-system admins / transient errors: keep the static note */
      });
    return () => {
      cancelled = true;
    };
  }, [api]);

  function openCreate() {
    // managed_runtime_only servers only ever accept a server_agent create
    // (the backend rejects anything else with application.managed_runtime_only)
    // -- seed the form with that type instead of ollama's so the operator
    // does not have to know to switch it themselves.
    const defaultType: ApplicationType = managedRuntimeOnly ? 'server_agent' : 'ollama';
    const d = applicationTypeDefaults[defaultType];
    setType(defaultType);
    setPort(d.port);
    setScheme(d.scheme);
    setFlavors([...applicationFlavorOptions]);
    setStatus('active');
    setPriority(0);
    setWeight(0);
    setTimeoutMs(d.timeoutMs);
    setAffinityTtl(defaultApplicationAffinityTtlSeconds);
    setAdmissionQueueTimeout(defaultApplicationAdmissionQueueTimeoutSeconds);
    setHealthMode(defaultApplicationHealthMode);
    setHealthPath(defaultApplicationHealthPath);
    setHealthIntervalMode('default');
    setHealthIntervalSeconds(defaultCustomHealthIntervalSeconds);
    setNativeResponses(d.nativeResponses);
    setNativeMessages(d.nativeMessages);
    setLoadedModelsPath(d.loadedModelsPath);
    setLoadedModelsFormat(d.loadedModelsFormat);
    setContextProbePath(d.contextProbePath);
    setAppPathSuffix('');
    setApiTokenHeader('');
    setTokenInput('');
    setTokenCleared(false);
    setProxyExcluded(false);
    setProxyExcludedSeed(false);
    setBenchmarkScheduleEnabled(false);
    setOpportunisticMetricsEnabled(false);
    setBenchmarkIntervalSeconds(defaultBenchmarkIntervalSeconds);
    setMode('create');
  }

  function handleTypeChange(newType: ApplicationType) {
    const patch = migrateTypeFields(type, newType, {
      port,
      scheme,
      nativeResponses,
      nativeMessages,
      loadedModelsPath,
      loadedModelsFormat,
      contextProbePath,
      timeoutMs,
    });
    if (patch.port !== undefined) setPort(patch.port);
    if (patch.scheme !== undefined) setScheme(patch.scheme);
    if (patch.nativeResponses !== undefined) setNativeResponses(patch.nativeResponses);
    if (patch.nativeMessages !== undefined) setNativeMessages(patch.nativeMessages);
    if (patch.loadedModelsPath !== undefined) setLoadedModelsPath(patch.loadedModelsPath);
    if (patch.loadedModelsFormat !== undefined) setLoadedModelsFormat(patch.loadedModelsFormat);
    if (patch.contextProbePath !== undefined) setContextProbePath(patch.contextProbePath);
    if (patch.timeoutMs !== undefined) setTimeoutMs(patch.timeoutMs);
    setType(newType);
  }

  function openEdit(app: PortalApplication) {
    setType(app.type);
    setPort(app.port);
    setScheme(app.scheme);
    setFlavors([...app.api_flavors]);
    setStatus(app.status);
    setPriority(app.priority);
    setWeight(app.weight);
    setTimeoutMs(app.timeout_ms);
    setAffinityTtl(app.affinity_ttl_seconds);
    setAdmissionQueueTimeout(app.admission_queue_timeout_seconds);
    setHealthMode(app.health_check_mode);
    setHealthPath(app.health_check_path);
    setHealthIntervalMode(app.health_check_interval_seconds > 0 ? 'custom' : 'default');
    setHealthIntervalSeconds(
      app.health_check_interval_seconds > 0
        ? app.health_check_interval_seconds
        : defaultCustomHealthIntervalSeconds,
    );
    setNativeResponses(app.native_responses);
    setNativeMessages(app.native_messages);
    setLoadedModelsPath(app.loaded_models_path ?? '');
    setLoadedModelsFormat(app.loaded_models_format || 'auto');
    setContextProbePath(app.context_probe_path ?? '');
    setAppPathSuffix(app.app_path_suffix ?? '');
    setApiTokenHeader(app.api_token_header ?? '');
    setTokenInput('');
    setTokenCleared(false);
    setProxyExcluded(app.proxy_excluded);
    setProxyExcludedSeed(app.proxy_excluded);
    setBenchmarkScheduleEnabled(app.benchmark_schedule_enabled);
    setOpportunisticMetricsEnabled(app.opportunistic_metrics_enabled);
    setBenchmarkIntervalSeconds(
      app.benchmark_schedule_interval_seconds > 0
        ? app.benchmark_schedule_interval_seconds
        : defaultBenchmarkIntervalSeconds,
    );
    setMode({ kind: 'edit', app });
  }

  function buildBody(): CreateApplicationRequest {
    // Token sentinel: cleared flag → "" (clear); a typed value → replace;
    // otherwise omit the field entirely (create = no token, update = keep the stored one).
    let apiToken: string | undefined;
    if (tokenCleared) {
      apiToken = '';
    } else if (tokenInput !== '') {
      apiToken = tokenInput;
    }
    return {
      type,
      port,
      scheme,
      api_flavors: flavors,
      status,
      priority,
      weight,
      timeout_ms: timeoutMs,
      affinity_ttl_seconds: affinityTtl,
      admission_queue_timeout_seconds: admissionQueueTimeout,
      health_check_mode: healthMode,
      health_check_path: healthPath,
      // "default" stores 0 so the app keeps following the system setting live.
      health_check_interval_seconds: healthIntervalMode === 'custom' ? healthIntervalSeconds : 0,
      native_responses: nativeResponses,
      native_messages: nativeMessages,
      loaded_models_path: loadedModelsPath.trim(),
      loaded_models_format: loadedModelsFormat,
      context_probe_path: contextProbePath.trim(),
      app_path_suffix: appPathSuffix.trim(),
      api_token_header: apiTokenHeader.trim(),
      ...(apiToken !== undefined ? { api_token: apiToken } : {}),
      // The showProxyControls gate is load-bearing on CREATE: on an
      // out-of-scope server the checkbox is not rendered, and an https create
      // must then send NO key at all so the backend normalizes it into the
      // excluded state -- an explicit `false` there is a request to PARTICIPATE
      // and is refused (application.proxy_entry_scheme).
      ...(showProxyControls ? { proxy_excluded: proxyExcluded } : {}),
      benchmark_schedule_enabled: benchmarkScheduleEnabled,
      opportunistic_metrics_enabled: opportunisticMetricsEnabled,
      // "off" stores 0 so an unscheduled app keeps a clean interval.
      benchmark_schedule_interval_seconds: benchmarkScheduleEnabled ? benchmarkIntervalSeconds : 0,
    };
  }

  async function submitCreate(event: SubmitEvent<HTMLFormElement>) {
    event.preventDefault();
    setBusy(true);
    try {
      const created = await api.createApplication(server.id, buildBody());
      setApplications((current) => [created, ...(current ?? [])]);
      setMode('list');
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setBusy(false);
    }
  }

  async function submitEdit(event: SubmitEvent<HTMLFormElement>) {
    event.preventDefault();
    if (typeof mode === 'string' || mode.kind !== 'edit') return;
    setBusy(true);
    try {
      const body: UpdateApplicationRequest = buildBody();
      // buildBody returns ONE literal reused verbatim for update, so a field
      // added there is restated on every save. proxy_excluded must not be:
      // sending it unconditionally would compile, pass a "the switch works"
      // test, and still be a defect -- every save made for an unrelated reason
      // would restate the operator's opt-out from whatever this form last read,
      // and on a hidden control would CLEAR it, returning an ordinary 200 with
      // nothing to notice. Compared against the seed captured when the form
      // OPENED, not against mode.app, which a refresh can replace mid-edit.
      if (proxyExcluded === proxyExcludedSeed) delete body.proxy_excluded;
      const updated = await api.updateApplication(mode.app.id, body);
      setApplications((current) =>
        (current ?? []).map((row) => (row.id === updated.id ? updated : row)),
      );
      setMode('list');
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setBusy(false);
    }
  }

  async function removeApplication(id: string) {
    try {
      await api.deleteApplication(id);
      setApplications((current) => (current ?? []).filter((row) => row.id !== id));
      setConfirmingDeleteId('');
      setMode('list');
    } catch (err) {
      showError(formatPortalError(err, t));
    }
  }

  const columns: ListColumn<PortalApplication>[] = [
    {
      id: 'type',
      label: t.applicationType,
      value: (a) => a.type,
      filter: 'enum',
      searchable: false,
    },
    { id: 'endpoint', label: t.tableEndpoint, value: (a) => a.endpoint, filter: 'text' },
    {
      id: 'flavors',
      label: t.applicationFlavors,
      value: (a) => a.api_flavors.join(', '),
      filter: 'text',
      render: (a) => a.api_flavors.join(', ') || '-',
    },
    {
      id: 'status',
      label: t.tableStatus,
      value: (a) => a.status,
      filter: 'enum',
      searchable: false,
      enumLabel: (v) => t[applicationStatusLabelByKey[v as ApplicationStatus] ?? 'statusActive'],
      render: (a) => (
        <StatusChip status={a.status} label={t[applicationStatusLabelByKey[a.status]]} />
      ),
    },
    {
      id: 'reachable',
      label: t.applicationReachable,
      value: (a) => (a.reachable ? 'reachable' : 'unreachable'),
      filter: 'enum',
      searchable: false,
      enumLabel: (v) => (v === 'reachable' ? t.applicationReachable : t.applicationUnreachable),
      render: (a) => (
        <Box
          component="span"
          title={formatDate(a.last_checked_at, t.applicationReachableNever)}
          sx={{ display: 'inline-flex' }}
        >
          <StatusChip
            status={a.reachable ? 'success' : 'error'}
            label={a.reachable ? t.applicationReachable : t.applicationUnreachable}
          />
        </Box>
      ),
    },
    // Read-only https-auto-switch status, derived CLIENT-SIDE from fields
    // already on the DTO. The value/enumLabel take the same three keys as the
    // chip so the enum filter offers all three states rather than collapsing
    // two of them.
    {
      id: 'proxy_status',
      label: t.applicationProxyStatus,
      value: (a) => applicationProxyState(a),
      filter: 'enum',
      searchable: false,
      enumLabel: (v) =>
        t[applicationProxyStateLabelByKey[v as ApplicationProxyState] ?? 'applicationNotProxied'],
      render: (a) => (
        <StatusChip
          status={applicationProxyStateChipStatus[applicationProxyState(a)]}
          label={t[applicationProxyStateLabelByKey[applicationProxyState(a)]]}
        />
      ),
    },
  ];

  const rowActions = (app: PortalApplication): RowAction[] => [
    {
      key: 'mappings',
      label: t.mappingManage,
      icon: <AppsIcon fontSize="small" />,
      onClick: () => setMode({ kind: 'mappings', app }),
    },
    {
      key: 'edit',
      label: t.applicationEdit,
      icon: <EditIcon fontSize="small" />,
      onClick: () => openEdit(app),
    },
    {
      key: 'delete',
      label: t.applicationDelete,
      color: 'error',
      icon: <DeleteIcon fontSize="small" />,
      onClick: () => setConfirmingDeleteId(app.id),
    },
  ];

  // Mappings drill-down. MappingSection renders its own breadcrumb from this
  // trail (this server's applications become a clickable ancestor).
  if (typeof mode !== 'string' && mode.kind === 'mappings') {
    const app = applications.find((a) => a.id === mode.app.id) ?? mode.app;
    // A server_agent application's model view IS the agent-managed runtime
    // admin (spec decision 5) -- the row action's label stays "manage
    // models"; only the destination differs.
    if (app.type === 'server_agent') {
      return (
        <RuntimeAdminSection
          key={app.id}
          t={t}
          api={api}
          server={server}
          application={app}
          trail={[...trail, { label: server.name, onClick: () => setMode('list') }]}
        />
      );
    }
    return (
      <MappingSection
        key={app.id}
        t={t}
        api={api}
        server={server}
        application={app}
        onModelsChanged={onModelsChanged}
        trail={[...trail, { label: server.name, onClick: () => setMode('list') }]}
      />
    );
  }

  // Create / edit sub-view (input mask).
  if (mode !== 'list') {
    const editing = mode !== 'create';
    // The app being edited (holds api_token_set); undefined on create.
    const editApp = typeof mode !== 'string' && mode.kind === 'edit' ? mode.app : undefined;
    // One server_agent application per AI server, because only one agent runs
    // per server. The rule is already enforced three levels down -- migration
    // 68's partial unique index, the portal service's pre-read, and the 409
    // application.server_agent_exists -- so this is purely the AFFORDANCE that
    // says so BEFORE a whole form has been filled in, instead of only after
    // the submit.
    //
    // It is not the enforcement and cannot be: `applications` is this
    // component's local list, fetched once and never polled, and it reads as
    // [] both while the first fetch is in flight and after one failed (the
    // `?? []` above). Whenever it is wrong this gate silently opens; the 409
    // is what actually holds the line, and submitCreate/submitEdit still
    // render it.
    //
    // The `a.id !== editApp?.id` exclusion mirrors the backend's own
    // excludeAppID exactly (create passes "", update passes app.ID), so the
    // server's own server_agent application never collides with itself: its
    // edit form keeps the type selected, selectable, and re-saveable, and can
    // switch away and back before saving. Retyping any OTHER application to
    // server_agent is blocked, which is the second write path the backend
    // guards with the same sentinel.
    const serverAgentTaken = serverAgentApps.some((a) => a.id !== editApp?.id);
    // The SECOND gate on this control, and it is CREATE-ONLY. The backend reads
    // Server.ManagedRuntimeOnly in exactly one place -- inside CreateApplication,
    // against the RAW requested type -- and UpdateApplication never looks at it.
    // So `&& !editing` is not a nicety: without it the portal would refuse, on
    // an edit, writes the backend accepts, and refuse them SILENTLY (a disabled
    // option gives no error to look up). That is a worse failure than the one
    // this closes. The gate follows the backend's own scope or it is a bug.
    //
    // Unlike serverAgentTaken this reads the SERVER dto, not the applications
    // list, so no fetch window opens it -- but the server dto is itself fetched
    // by the parent list and never refreshed here, so a PATCH that sets the flag
    // afterwards leaves this form offering all six types. The 409 is still the
    // enforcement; a test fires that path so the mapping is not dropped.
    const managedRuntimeOnlyCreate = managedRuntimeOnly && !editing;
    // One helperText slot, two reasons. They are co-reachable -- but only
    // through the first-fetch window: on a settled managed server that already
    // holds an agent application the create button is not rendered at all
    // (see the list view below), so the create form cannot be opened; while
    // that first fetch is in flight `applications` reads [] and the button is
    // not loading-gated, so it can. Composed narrowest-first: "only server_agent
    // is creatable here", then "and that one is taken" -- which together say
    // the intersection is empty, exactly what the disabled options then show.
    const typeReasons = [
      managedRuntimeOnlyCreate ? t.applicationTypeManagedRuntimeOnly : undefined,
      serverAgentTaken ? t.applicationTypeServerAgentTaken : undefined,
    ].filter((reason): reason is string => reason !== undefined);
    // The scheme is the GATEWAY's field only while the application actually
    // takes part in the proxy on a server that runs one. In every other case --
    // excluded, or a server out of scope -- it is the operator's, and the
    // select is enabled.
    //
    // Disabled, its VALUE stays the STORED scheme, so an unrelated save on an
    // already-switched https application re-sends `https` and is a no-op. It
    // must never re-send `http`: that would open a downgrade window on every
    // save made for an unrelated reason.
    //
    // The reason rides in helperText rather than a tooltip, for the same
    // accessibility reason the type select already records: a disabled MUI
    // control is out of the tab order and sets pointer-events:none, so anything
    // anchored on hover is unreachable by keyboard and screen reader.
    const schemeManaged = showProxyControls && !proxyExcluded;
    // Three distinct sentences, never one reused. On an out-of-scope server the
    // control is only visible through the already-excluded OVERRIDE, and the
    // out-of-scope explanation is then the honest thing to say next to it.
    let proxyModeNote = t.applicationProxyModeUnknownNote;
    if (serverProxyState === 'proxy') proxyModeNote = t.applicationProxyModeActiveNote;
    else if (serverProxyState === 'agent_off') proxyModeNote = t.applicationProxyModeOffNote;
    else if (serverProxyState === 'out_of_scope') proxyModeNote = t.applicationProxyOutOfScopeNote;
    // The assigned proxy port, as STATIC TEXT and never a form control: the
    // gateway owns it, and buildBody must go on never sending
    // proxy_listen_port. 0 must never render as "0", as a blank or as a dash --
    // the not-yet state is spelled out as a wait with a bound.
    let proxyPortNote = t.applicationProxyPortUnassigned;
    if (proxyExcluded) proxyPortNote = t.applicationProxyPortExcluded;
    else if (editApp && editApp.proxy_listen_port > 0) {
      proxyPortNote = t.applicationProxyPort(editApp.proxy_listen_port);
    }
    return (
      <>
        <Breadcrumbs
          ariaLabel={t.breadcrumb}
          backLabel={t.back}
          items={[
            ...trail,
            { label: server.name, onClick: () => setMode('list') },
            { label: editing ? t.applicationEdit : t.applicationCreate },
          ]}
        />
        <Panel
          titleId="application-form-heading"
          title={editing ? t.applicationEdit : t.applicationCreate}
        >
          <Box
            component="form"
            onSubmit={editing ? submitEdit : submitCreate}
            sx={{ display: 'grid', gridTemplateColumns: 'minmax(260px, 520px)', gap: 2.25 }}
          >
            <SelectField
              id="application-type"
              label={t.applicationType}
              value={type}
              onChange={(e) => handleTypeChange(e.target.value as ApplicationType)}
              // A disabled control must say why -- but the reason rides on the
              // FIELD, not on the option. MUI leaves `disabledItemsFocusable`
              // false, so arrow-key navigation skips a disabled option
              // entirely and anything anchored there (a Tooltip, a title) is
              // unreachable by keyboard and screen reader -- issue #26's
              // defect, made worse. `helperText` is wired to the combobox via
              // aria-describedby, so it is announced on focus, before the menu
              // is ever opened, and it is legible without a hover. It states
              // the rule, the reason for it and the remedy; the 409 toast
              // stays terse because it arrives after the fact and is already
              // prefixed with its raw error code.
              helperText={typeReasons.length > 0 ? typeReasons.join(' ') : undefined}
            >
              {applicationTypeOptions.map((option) => (
                <option
                  value={option}
                  key={option}
                  // Disabled, never filtered out of the list: openEdit seeds
                  // `type` from the row, and a value with no matching MenuItem
                  // renders a BLANK combobox -- the operator cannot see what
                  // the application IS while editing it.
                  //
                  // It is NOT a data-integrity problem, and an earlier version
                  // of this comment said it was ("buildBody would submit a
                  // silent retype"). Probed: under a filtering variant the save
                  // path still sends `type: 'server_agent'`, because `type`
                  // holds the seeded value whether or not a MenuItem matches
                  // it. Overstating the reason is what invites the "fix" that
                  // reintroduces the blank field.
                  //
                  // The managed_runtime_only half disables the complementary
                  // five, and only while creating -- see managedRuntimeOnlyCreate
                  // above. Same reasoning against filtering: openEdit seeds the
                  // type from the row, and an edit is precisely where this gate
                  // must not apply at all.
                  disabled={
                    (option === 'server_agent' && serverAgentTaken) ||
                    (managedRuntimeOnlyCreate && option !== 'server_agent')
                  }
                >
                  {option}
                </option>
              ))}
            </SelectField>
            <Field
              id="application-port"
              label={t.applicationPort}
              type="number"
              value={String(port)}
              onChange={(e) => setPort(e.target.value === '' ? 0 : Number(e.target.value))}
              required
              inputProps={{ min: 1, max: 65535 }}
            />
            <SelectField
              id="application-scheme"
              label={t.applicationScheme}
              value={scheme}
              onChange={(e) => setScheme(e.target.value as ApplicationScheme)}
              disabled={schemeManaged}
              helperText={schemeManaged ? t.applicationSchemeManagedNote : undefined}
            >
              {applicationSchemeOptions.map((option) => (
                <option value={option} key={option}>
                  {option}
                </option>
              ))}
            </SelectField>
            {showProxyControls ? (
              <>
                <CheckboxGroup
                  legend={t.applicationProxyLegend}
                  options={[{ value: 'excluded', label: t.applicationProxyExcluded }]}
                  selected={proxyExcluded ? ['excluded'] : []}
                  onToggle={() => {
                    const next = !proxyExcluded;
                    setProxyExcluded(next);
                    if (!next) {
                      // Unticking. http is the ONLY re-entry: the candidate
                      // predicate's https arm needs a proxy port, only the
                      // gateway assigns one, and only to candidates. It is also
                      // what the backend requires, so forcing it here turns a
                      // 409 into a save that works.
                      setScheme('http');
                    } else if (editApp?.scheme === 'https' && editApp.proxy_listen_port !== 0) {
                      // Ticking on an already-proxied application. That `https`
                      // described the AGENT'S listener, not the upstream --
                      // AgentProxyRoutes publishes http://127.0.0.1:<Port>, so
                      // the application itself speaks plaintext on its own port.
                      // Pre-setting http is what keeps it reachable the instant
                      // the PATCH lands. The operator may still choose https in
                      // the same save; that is the whole point of the feature.
                      setScheme('http');
                    }
                  }}
                />
                <Typography variant="caption" sx={{ color: 'text.secondary' }}>
                  {t.applicationProxyExcludedNote}
                </Typography>
                <Typography variant="caption" sx={{ color: 'text.secondary' }}>
                  {proxyModeNote}
                </Typography>
                {proxyExcluded && scheme === 'https' ? (
                  <Alert severity="warning">{t.applicationProxyOwnTLSWarning(server.domain)}</Alert>
                ) : null}
                <Typography variant="caption" sx={{ color: 'text.secondary' }}>
                  {proxyPortNote}
                </Typography>
              </>
            ) : (
              // Never a blank: the slot the control would have occupied still
              // explains why there is nothing to set here.
              <Typography variant="caption" sx={{ color: 'text.secondary' }}>
                {t.applicationProxyOutOfScopeNote}
              </Typography>
            )}
            <CheckboxGroup
              legend={t.applicationFlavors}
              options={applicationFlavorOptions.map((f) => ({ value: f, label: f }))}
              selected={flavors}
              onToggle={(v) => setFlavors((current) => toggleFlavor(current, v))}
            />
            <CheckboxGroup
              legend={t.applicationNativeLegend}
              options={[
                { value: 'responses', label: t.applicationNativeResponses },
                { value: 'messages', label: t.applicationNativeMessages },
              ]}
              selected={[
                ...(nativeResponses ? ['responses'] : []),
                ...(nativeMessages ? ['messages'] : []),
              ]}
              onToggle={(v) => {
                if (v === 'responses') setNativeResponses((c) => !c);
                else if (v === 'messages') setNativeMessages((c) => !c);
              }}
            />
            <Typography variant="caption" sx={{ color: 'text.secondary' }}>
              {t.applicationNativeNote}
            </Typography>
            <Box
              component="fieldset"
              sx={{
                border: 0,
                m: 0,
                p: 0,
                display: 'flex',
                flexWrap: 'wrap',
                alignItems: 'flex-start',
                gap: 1.5,
              }}
            >
              <legend>{t.applicationLoadedModelsLegend}</legend>
              <Field
                id="application-loaded-models-path"
                label={t.applicationLoadedModelsPath}
                value={loadedModelsPath}
                onChange={(e) => setLoadedModelsPath(e.target.value)}
                placeholder="/running"
              />
              <SelectField
                id="application-loaded-models-format"
                label={t.applicationLoadedModelsFormat}
                value={loadedModelsFormat}
                onChange={(e) => setLoadedModelsFormat(e.target.value)}
                disabled={loadedModelsPath.trim() === ''}
              >
                <option value="auto">{t.applicationLoadedFormatAuto}</option>
                <option value="openai">{t.applicationLoadedFormatOpenai}</option>
                <option value="llama_swap">{t.applicationLoadedFormatLlamaSwap}</option>
                <option value="llama_cpp">{t.applicationLoadedFormatLlamaCpp}</option>
                <option value="litellm">{t.applicationLoadedFormatLitellm}</option>
              </SelectField>
            </Box>
            <Typography variant="caption" sx={{ color: 'text.secondary' }}>
              {t.applicationLoadedModelsNote}
            </Typography>
            <Field
              id="application-context-probe-path"
              label={t.applicationContextProbePath}
              value={contextProbePath}
              onChange={(e) => setContextProbePath(e.target.value)}
              placeholder="/props"
              helperText={t.applicationContextProbePathHelp}
            />
            <Typography variant="caption" sx={{ color: 'text.secondary' }}>
              {t.applicationContextProbeNote}
            </Typography>
            <Field
              id="application-path-suffix"
              label={t.applicationPathSuffixLabel}
              value={appPathSuffix}
              onChange={(e) => setAppPathSuffix(e.target.value)}
            />
            <Box>
              <Field
                id="application-api-token"
                type="password"
                label={t.applicationApiTokenLabel}
                value={tokenCleared ? '' : tokenInput}
                onChange={(e) => {
                  setTokenInput(e.target.value);
                  setTokenCleared(false);
                }}
                autoComplete="new-password"
                placeholder={
                  editing && editApp?.api_token_set && !tokenCleared
                    ? t.applicationApiTokenSetPlaceholder
                    : undefined
                }
                helperText={t.applicationApiTokenNote}
              />
              {editing && editApp?.api_token_set && !tokenCleared && (
                <Button
                  type="button"
                  size="small"
                  variant="text"
                  color="secondary"
                  onClick={() => {
                    setTokenCleared(true);
                    setTokenInput('');
                  }}
                >
                  {t.applicationApiTokenClear}
                </Button>
              )}
            </Box>
            <Field
              id="application-api-token-header"
              label={t.applicationApiTokenHeaderLabel}
              value={apiTokenHeader}
              onChange={(e) => setApiTokenHeader(e.target.value)}
              helperText={t.applicationApiTokenHeaderHelp}
            />
            <CheckboxGroup
              legend={t.applicationMetricsLegend}
              options={[
                { value: 'scheduled', label: t.applicationScheduledBenchmark },
                { value: 'opportunistic', label: t.applicationOpportunisticMetrics },
              ]}
              selected={[
                ...(benchmarkScheduleEnabled ? ['scheduled'] : []),
                ...(opportunisticMetricsEnabled ? ['opportunistic'] : []),
              ]}
              onToggle={(v) => {
                if (v === 'scheduled') setBenchmarkScheduleEnabled((c) => !c);
                else if (v === 'opportunistic') setOpportunisticMetricsEnabled((c) => !c);
              }}
            />
            {benchmarkScheduleEnabled && (
              <Field
                id="application-benchmark-interval-seconds"
                label={t.applicationScheduledBenchmarkIntervalLabel}
                type="number"
                value={String(benchmarkIntervalSeconds)}
                onChange={(e) =>
                  setBenchmarkIntervalSeconds(e.target.value === '' ? 0 : Number(e.target.value))
                }
              />
            )}
            <Typography variant="caption" sx={{ color: 'text.secondary' }}>
              {t.applicationMetricsNote}
            </Typography>
            <SelectField
              id="application-status"
              label={t.tableStatus}
              value={status}
              onChange={(e) => setStatus(e.target.value as ApplicationStatus)}
            >
              {applicationStatusOptions.map((s) => (
                <option value={s} key={s}>
                  {t[applicationStatusLabelByKey[s]]}
                </option>
              ))}
            </SelectField>
            <SelectField
              id="application-health-mode"
              label={t.applicationHealthMode}
              value={healthMode}
              onChange={(e) => setHealthMode(e.target.value as ApplicationHealthMode)}
            >
              {applicationHealthModeOptions.map((m) => (
                <option value={m} key={m}>
                  {t[healthModeLabelByKey[m]]}
                </option>
              ))}
            </SelectField>
            {healthMode === 'health_path' && (
              <Field
                id="application-health-path"
                label={t.applicationHealthPath}
                value={healthPath}
                onChange={(e) => setHealthPath(e.target.value)}
              />
            )}
            {healthMode === 'model_sync' && (
              <Typography variant="caption" sx={{ color: 'text.secondary' }}>
                {t.applicationHealthModeNote}
              </Typography>
            )}
            {healthMode !== 'always_reachable' && (
              <>
                <SelectField
                  id="application-health-interval-mode"
                  label={t.applicationHealthIntervalLabel}
                  value={healthIntervalMode}
                  onChange={(e) => setHealthIntervalMode(e.target.value as 'default' | 'custom')}
                >
                  <option value="default">{t.applicationHealthIntervalDefault}</option>
                  <option value="custom">{t.applicationHealthIntervalCustom}</option>
                </SelectField>
                {healthIntervalMode === 'custom' ? (
                  <Field
                    id="application-health-interval-seconds"
                    label={t.applicationHealthIntervalSecondsLabel}
                    type="number"
                    value={String(healthIntervalSeconds)}
                    onChange={(e) =>
                      setHealthIntervalSeconds(e.target.value === '' ? 0 : Number(e.target.value))
                    }
                  />
                ) : (
                  <Typography variant="caption" sx={{ color: 'text.secondary' }}>
                    {t.applicationHealthIntervalNote}
                    {systemHealthIntervalSeconds !== null &&
                      ` (${t.applicationHealthIntervalCurrent}: ${systemHealthIntervalSeconds} s)`}
                  </Typography>
                )}
              </>
            )}
            <Box
              component="fieldset"
              sx={{
                border: 0,
                m: 0,
                p: 0,
                display: 'flex',
                flexWrap: 'wrap',
                alignItems: 'center',
                gap: 1.5,
              }}
            >
              <legend>{t.applicationAdvanced}</legend>
              <Field
                id="application-priority"
                label={t.applicationPriority}
                type="number"
                value={String(priority)}
                onChange={(e) => setPriority(e.target.value === '' ? 0 : Number(e.target.value))}
              />
              <Field
                id="application-weight"
                label={t.applicationWeight}
                type="number"
                value={String(weight)}
                onChange={(e) => setWeight(e.target.value === '' ? 0 : Number(e.target.value))}
              />
              <Field
                id="application-timeout"
                label={t.applicationTimeout}
                type="number"
                value={String(timeoutMs)}
                onChange={(e) => setTimeoutMs(e.target.value === '' ? 0 : Number(e.target.value))}
              />
              <Field
                id="application-affinity-ttl"
                label={t.applicationAffinityTtl}
                type="number"
                value={String(affinityTtl)}
                onChange={(e) => setAffinityTtl(e.target.value === '' ? 0 : Number(e.target.value))}
              />
              <Field
                id="application-admission-queue-timeout"
                label={t.applicationAdmissionQueueTimeout}
                type="number"
                value={String(admissionQueueTimeout)}
                onChange={(e) =>
                  setAdmissionQueueTimeout(e.target.value === '' ? 0 : Number(e.target.value))
                }
                helperText={t.applicationAdmissionQueueTimeoutHelp}
              />
            </Box>
            <Box sx={{ display: 'flex', gap: 1.5 }}>
              <Button type="submit" variant="contained" disabled={busy}>
                {editing ? t.applicationSave : t.applicationCreate}
              </Button>
              <Button
                type="button"
                variant="text"
                color="secondary"
                onClick={() => setMode('list')}
              >
                {t.cancel}
              </Button>
            </Box>
          </Box>
        </Panel>
      </>
    );
  }

  // On a managed_runtime_only server the two backend gates intersect to the
  // empty set once the one server_agent application exists: managed_runtime_only
  // permits only that type, and the one-agent-per-server rule refuses a second
  // of it. No create of any type can succeed, so the button is not offered.
  //
  // HIDDEN, not disabled -- and deliberately the opposite call to the one made
  // for the type option, because the alternatives are not the same. Removing an
  // option from a select blanks the combobox, since the form's `type` is seeded
  // from the row whether or not a menu item matches it; removing a button costs
  // nothing structurally. And the reason has to be readable without a hover:
  // helperText gave the field one (aria-describedby, announced on focus), but a
  // disabled MUI Button is out of the tab order and sets pointer-events: none,
  // so the only reason-carrier left for it is a Tooltip on a wrapper span --
  // unreachable by keyboard and screen reader, which is issue #26 exactly.
  // Plain text in the reading order, right where the button was, beats both.
  const canCreateApplication = !managedRuntimeOnly || serverAgentApps.length === 0;
  return (
    <>
      <Breadcrumbs
        ariaLabel={t.breadcrumb}
        backLabel={t.back}
        items={[...trail, { label: server.name }]}
      />
      <Panel
        titleId="application-heading"
        title={t.applications}
        subtitle={t.applicationsIntro}
        actions={
          canCreateApplication && (
            <Button variant="contained" startIcon={<AddIcon />} onClick={openCreate}>
              {t.applicationCreate}
            </Button>
          )
        }
      >
        {managedRuntimeOnly && (
          <Alert severity="info" sx={{ mb: 2 }}>
            {/* Two sentences, two elements. The banner keeps its own text node
                so it stays matchable exactly, and the second sentence is bound
                to the same condition as the button rather than restated, so the
                two can never drift into claiming a restriction that is not in
                force. */}
            <Box component="span" sx={{ display: 'block' }}>
              {t.runtimeManagedOnlyBanner}
            </Box>
            {!canCreateApplication && (
              <Box component="span" sx={{ display: 'block', mt: 0.5 }}>
                {t.runtimeManagedOnlyCreateBlocked}
              </Box>
            )}
          </Alert>
        )}
        <ListTable
          rows={applications}
          columns={columns}
          rowKey={(a) => a.id}
          actions={rowActions}
          storageKey="op.applications"
          minWidth={820}
          labels={listTableLabels(t)}
          loading={loading || applicationsData === null}
        />
      </Panel>

      <ConfirmDialog
        open={confirmingDeleteId !== ''}
        title={t.applicationDeleteConfirm}
        confirmLabel={t.applicationDelete}
        cancelLabel={t.applicationCancel}
        onConfirm={() => removeApplication(confirmingDeleteId)}
        onCancel={() => setConfirmingDeleteId('')}
      />
    </>
  );
}
