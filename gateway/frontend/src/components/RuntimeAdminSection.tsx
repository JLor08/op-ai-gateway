// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useEffect, useRef, useState, type SubmitEvent } from 'react';
import {
  Alert,
  Box,
  Button,
  Checkbox,
  Chip,
  FormControlLabel,
  Tab,
  Tabs,
  Typography,
} from '@mui/material';
import AddIcon from '@mui/icons-material/Add';
import EditIcon from '@mui/icons-material/Edit';
import DeleteIcon from '@mui/icons-material/Delete';
import type {
  ApplicationStatus,
  PortalApplication,
  PortalModelMapping,
  PortalServer,
  PutRuntimeSpecRequest,
  RuntimeSpec,
  RuntimeSpecGPU,
} from '../api';
import type { Translation, PortalApi, MessageKey } from './shared/types';
import { formatPortalError, formatMetric } from './shared/format';
import { useResource } from './shared/useResource';
import { StatusChip } from './shared/StatusChip';
import { Panel } from './shared/Panel';
import { Field } from './shared/Field';
import { SelectField } from './shared/SelectField';
import { ConfirmDialog } from './shared/ConfirmDialog';
import { Breadcrumbs, type BreadcrumbItem } from './shared/Breadcrumbs';
import { ListTable, listTableLabels, type ListColumn } from './shared/ListTable';
import type { RowAction } from './shared/RowActionsMenu';
import { useToast } from './shared/ToastProvider';
import { applicationStatusOptions, applicationStatusLabelByKey } from './shared/application';

// Area 1 (this task, Task 20) is "Launch specs"; matrix/limits/status are
// Tasks 21-22 stubs rendered from the same tab strip so the whole section's
// navigation is visible/testable now (see the task-20 brief).
type Tab = 'specs' | 'matrix' | 'limits' | 'status';

type SpecMode = 'list' | 'create' | { kind: 'edit'; mapping: PortalModelMapping };

type GpuRow = { index: number; vramEstimateMb: number; vramMeasuredMb: number };

// The sole warning code RuntimeWarnings emits today (see
// gateway/backend/internal/portal/service_runtime.go); an unmapped future
// code falls back to its raw wire value rather than a misleading label.
const runtimeWarningLabelByCode: Record<string, MessageKey> = {
  timeout_ms_below_startup_timeout: 'runtimeTimeoutWarning',
};

// admin_state's three valid wire values (service_runtime.go): "" (no
// override), "force_running", "force_stopped".
const adminStateOptions: { value: string; labelKey: MessageKey }[] = [
  { value: '', labelKey: 'runtimeClearOverride' },
  { value: 'force_running', labelKey: 'runtimeForceStart' },
  { value: 'force_stopped', labelKey: 'runtimeForceStop' },
];

// A brand-new/never-configured spec is `configured: false` with every other
// field at its zero value (RuntimeSpec's own doc comment) -- reproduced here
// so a delete or an unloaded row can be rendered/edited without waiting on a
// network round trip.
function emptySpec(mappingId: string): RuntimeSpec {
  return {
    configured: false,
    mapping_id: mappingId,
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
  };
}

function basename(path: string): string {
  const parts = path.split(/[\\/]/);
  return parts[parts.length - 1] || path;
}

function formatGpus(gpus: RuntimeSpecGPU[]): string {
  return gpus.length === 0
    ? '—'
    : gpus.map((g) => `${g.index}: ${g.vram_estimate_mb} MB`).join(', ');
}

function renderBoolChip(active: boolean, label: string) {
  return active ? <Chip size="small" variant="outlined" label={label} /> : '–';
}

// One argument/line -- splitting on spaces would corrupt any argument that
// legitimately contains one (model server command lines routinely have
// these), so a textarea line IS the argument, trimmed of stray whitespace.
function parseArgsText(text: string): string[] {
  return text
    .split('\n')
    .map((line) => line.trim())
    .filter((line) => line !== '');
}

function formatArgsText(args: string[]): string {
  return args.join('\n');
}

// KEY=value per line. Rejects the two failure modes the agent itself would
// otherwise only discover at process-start time (invisible to this task's
// UI, which has no live-status view yet -- that is Task 22): PATH/HOME
// (reserved by the launcher) and any ${AGENT_ENV:OP_AGENT_*} reference (the
// agent's own credential namespace). A malformed (no "=") line is reported
// via the existing runtime_spec.env_invalid label so it reads the same way
// a backend rejection would.
function parseEnvText(
  text: string,
  t: Translation,
): { env: Record<string, string>; error?: string } {
  const env: Record<string, string> = {};
  for (const raw of text.split('\n')) {
    const line = raw.trim();
    if (line === '') continue;
    const eq = line.indexOf('=');
    if (eq <= 0) {
      return { env: {}, error: t.errorRuntimeSpecEnvInvalid };
    }
    const key = line.slice(0, eq).trim();
    const value = line.slice(eq + 1);
    if (key === 'PATH' || key === 'HOME' || value.includes('${AGENT_ENV:OP_AGENT_')) {
      return { env: {}, error: t.runtimeSpecEnvReserved };
    }
    env[key] = value;
  }
  return { env };
}

function formatEnvText(env: Record<string, string>): string {
  return Object.entries(env)
    .map(([k, v]) => `${k}=${v}`)
    .join('\n');
}

export function RuntimeAdminSection({
  t,
  api,
  // Not read by area 1 (server context already folds into `trail` before this
  // component ever mounts, same as MappingSection); the co-residency/limits/
  // status tabs (Tasks 21-22) are server-scoped (gpuBudgets/runtimeReport/
  // subscribeRuntimeStatus all take a server id) and will need it then.
  server: _server,
  application,
  trail = [],
}: Readonly<{
  t: Translation;
  api: Pick<
    PortalApi,
    | 'mappings'
    | 'createMapping'
    | 'updateMapping'
    | 'deleteMapping'
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
  >;
  server: PortalServer;
  application: PortalApplication;
  trail?: BreadcrumbItem[];
}>) {
  const { showError } = useToast();
  const [tab, setTab] = useState<Tab>('specs');
  const [specMode, setSpecMode] = useState<SpecMode>('list');
  const [busy, setBusy] = useState(false);
  const [confirmingDeleteId, setConfirmingDeleteId] = useState('');
  const [loadingEditFor, setLoadingEditFor] = useState('');

  const {
    data: mappingsData,
    setData: setMappings,
    error: mappingsError,
    loading: mappingsLoading,
  } = useResource(
    () => api.mappings(application.id).then((r) => r.data),
    [api, application.id, t],
    t,
  );
  const mappings = mappingsData ?? [];
  useEffect(() => {
    if (mappingsError) showError(mappingsError);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [mappingsError]);

  const {
    data: warningsData,
    error: warningsError,
    reload: reloadWarnings,
  } = useResource(
    () => api.runtimeWarnings(application.id).then((r) => r.warnings),
    [api, application.id, t],
    t,
  );
  const warnings = warningsData ?? [];
  useEffect(() => {
    if (warningsError) showError(warningsError);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [warningsError]);

  // Per-mapping runtime spec, loaded lazily and once per mapping id into a
  // map (the brief's "lazy... loaded once" recipe) -- a bulk endpoint does
  // not exist, so the list view fans out one GET per row.
  const [specsById, setSpecsById] = useState<Record<string, RuntimeSpec>>({});
  const loadedIdsRef = useRef<Set<string>>(new Set());
  useEffect(() => {
    const toLoad = mappings.filter((m) => !loadedIdsRef.current.has(m.id));
    if (toLoad.length === 0) return undefined;
    let cancelled = false;
    (async () => {
      for (const m of toLoad) {
        loadedIdsRef.current.add(m.id);
        try {
          const spec = await api.runtimeSpec(m.id);
          if (!cancelled) setSpecsById((cur) => ({ ...cur, [m.id]: spec }));
        } catch (err) {
          if (!cancelled) showError(formatPortalError(err, t));
        }
      }
    })();
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [mappings, api]);

  // ---- create/edit form state -----------------------------------------
  const [gatewayName, setGatewayName] = useState('');
  const [appName, setAppName] = useState('');
  const [status, setStatus] = useState<ApplicationStatus>('active');
  const [enabled, setEnabled] = useState(true);
  const [binary, setBinary] = useState('');
  const [argsText, setArgsText] = useState('');
  const [envText, setEnvText] = useState('');
  const [workDir, setWorkDir] = useState('');
  const [listenPort, setListenPort] = useState(0);
  const [healthPath, setHealthPath] = useState('/health');
  const [healthTimeoutSeconds, setHealthTimeoutSeconds] = useState(5);
  const [startupTimeoutSeconds, setStartupTimeoutSeconds] = useState(180);
  const [idleTimeoutSeconds, setIdleTimeoutSeconds] = useState(0);
  const [admissionWaitTimeoutSeconds, setAdmissionWaitTimeoutSeconds] = useState(0);
  const [pinned, setPinned] = useState(false);
  const [adminState, setAdminState] = useState('');
  const [vramLocked, setVramLocked] = useState(false);
  const [gpuRows, setGpuRows] = useState<GpuRow[]>([]);

  function resetSpecFields() {
    setEnabled(true);
    setBinary('');
    setArgsText('');
    setEnvText('');
    setWorkDir('');
    setListenPort(0);
    setHealthPath('/health');
    setHealthTimeoutSeconds(5);
    setStartupTimeoutSeconds(180);
    setIdleTimeoutSeconds(0);
    setAdmissionWaitTimeoutSeconds(0);
    setPinned(false);
    setAdminState('');
    setVramLocked(false);
    setGpuRows([]);
  }

  function hydrateSpecFields(spec: RuntimeSpec) {
    setEnabled(spec.enabled);
    setBinary(spec.binary);
    setArgsText(formatArgsText(spec.args));
    setEnvText(formatEnvText(spec.env));
    setWorkDir(spec.work_dir);
    setListenPort(spec.listen_port);
    setHealthPath(spec.health_path || '/health');
    setHealthTimeoutSeconds(spec.health_timeout_seconds || 5);
    setStartupTimeoutSeconds(spec.startup_timeout_seconds || 180);
    setIdleTimeoutSeconds(spec.idle_timeout_seconds);
    setAdmissionWaitTimeoutSeconds(spec.admission_wait_timeout_seconds);
    setPinned(spec.pinned);
    setAdminState(spec.admin_state);
    setVramLocked(spec.vram_locked);
    setGpuRows(
      spec.gpus.map((g) => ({
        index: g.index,
        vramEstimateMb: g.vram_estimate_mb,
        vramMeasuredMb: g.vram_measured_mb,
      })),
    );
  }

  function openCreate() {
    setGatewayName('');
    setAppName('');
    setStatus('active');
    resetSpecFields();
    setSpecMode('create');
  }

  // Re-reads the spec fresh (rather than trusting the bulk-loaded map, which
  // may not have settled yet for a just-rendered row) so the full-document
  // PUT this form issues on save never clobbers fields it never saw.
  async function openEdit(mapping: PortalModelMapping) {
    setGatewayName(mapping.gateway_model_name);
    setAppName(mapping.app_model_name);
    setStatus(mapping.status);
    setLoadingEditFor(mapping.id);
    try {
      const spec = await api.runtimeSpec(mapping.id);
      setSpecsById((cur) => ({ ...cur, [mapping.id]: spec }));
      loadedIdsRef.current.add(mapping.id);
      hydrateSpecFields(spec);
      setSpecMode({ kind: 'edit', mapping });
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setLoadingEditFor('');
    }
  }

  function addGpuRow() {
    setGpuRows((rows) => [...rows, { index: rows.length, vramEstimateMb: 0, vramMeasuredMb: 0 }]);
  }
  function removeGpuRow(idx: number) {
    setGpuRows((rows) => rows.filter((_, i) => i !== idx));
  }
  function updateGpuRow(idx: number, patch: Partial<Pick<GpuRow, 'index' | 'vramEstimateMb'>>) {
    setGpuRows((rows) => rows.map((r, i) => (i === idx ? { ...r, ...patch } : r)));
  }

  function buildSpecBody(env: Record<string, string>): PutRuntimeSpecRequest {
    return {
      enabled,
      binary: binary.trim(),
      args: parseArgsText(argsText),
      env,
      work_dir: workDir.trim(),
      listen_port: listenPort,
      health_path: healthPath.trim(),
      health_timeout_seconds: healthTimeoutSeconds,
      startup_timeout_seconds: startupTimeoutSeconds,
      idle_timeout_seconds: idleTimeoutSeconds,
      admission_wait_timeout_seconds: admissionWaitTimeoutSeconds,
      pinned,
      admin_state: adminState,
      vram_locked: vramLocked,
      gpus: gpuRows.map((r) => ({
        index: r.index,
        vram_estimate_mb: r.vramEstimateMb,
        vram_measured_mb: r.vramMeasuredMb,
      })),
    };
  }

  async function submitCreate(event: SubmitEvent<HTMLFormElement>) {
    event.preventDefault();
    const parsed = parseEnvText(envText, t);
    if (parsed.error) {
      showError(parsed.error);
      return;
    }
    setBusy(true);
    let mapping: PortalModelMapping;
    try {
      mapping = await api.createMapping(application.id, {
        gateway_model_name: gatewayName,
        app_model_name: appName,
        status,
      });
      setMappings((current) => [mapping, ...(current ?? [])]);
      loadedIdsRef.current.add(mapping.id);
    } catch (err) {
      showError(formatPortalError(err, t));
      setBusy(false);
      return;
    }
    // The mapping now exists regardless of what happens next -- a failure
    // from here on is reported as a partial failure, never silently, so the
    // operator knows the spec (not the mapping) needs another attempt.
    try {
      const spec = await api.putRuntimeSpec(mapping.id, buildSpecBody(parsed.env));
      setSpecsById((cur) => ({ ...cur, [mapping.id]: spec }));
      void reloadWarnings();
      setSpecMode('list');
    } catch (err) {
      showError(`${t.runtimeSpecPartialFailure}: ${formatPortalError(err, t)}`);
      setSpecMode('list');
    } finally {
      setBusy(false);
    }
  }

  async function submitEdit(event: SubmitEvent<HTMLFormElement>) {
    event.preventDefault();
    if (typeof specMode === 'string' || specMode.kind !== 'edit') return;
    const id = specMode.mapping.id;
    const parsed = parseEnvText(envText, t);
    if (parsed.error) {
      showError(parsed.error);
      return;
    }
    setBusy(true);
    try {
      const updated = await api.updateMapping(id, {
        gateway_model_name: gatewayName,
        app_model_name: appName,
        status,
      });
      setMappings((current) => (current ?? []).map((m) => (m.id === id ? updated : m)));
    } catch (err) {
      showError(formatPortalError(err, t));
      setBusy(false);
      return;
    }
    try {
      const spec = await api.putRuntimeSpec(id, buildSpecBody(parsed.env));
      setSpecsById((cur) => ({ ...cur, [id]: spec }));
      void reloadWarnings();
      setSpecMode('list');
    } catch (err) {
      showError(`${t.runtimeSpecPartialFailure}: ${formatPortalError(err, t)}`);
      setSpecMode('list');
    } finally {
      setBusy(false);
    }
  }

  const confirmTargetSpec = confirmingDeleteId ? specsById[confirmingDeleteId] : undefined;
  const confirmIsSpecDelete = Boolean(confirmTargetSpec?.configured);

  async function confirmDelete() {
    const id = confirmingDeleteId;
    if (!id) return;
    try {
      if (confirmIsSpecDelete) {
        await api.deleteRuntimeSpec(id);
        setSpecsById((cur) => ({ ...cur, [id]: emptySpec(id) }));
        void reloadWarnings();
      } else {
        await api.deleteMapping(id);
        setMappings((current) => (current ?? []).filter((m) => m.id !== id));
        setSpecsById((cur) => {
          const next = { ...cur };
          delete next[id];
          return next;
        });
        loadedIdsRef.current.delete(id);
      }
      setConfirmingDeleteId('');
    } catch (err) {
      showError(formatPortalError(err, t));
    }
  }

  const columns: ListColumn<PortalModelMapping>[] = [
    {
      id: 'gateway',
      label: t.mappingGatewayName,
      value: (m) => m.gateway_model_name,
      filter: 'text',
    },
    { id: 'app', label: t.mappingAppName, value: (m) => m.app_model_name, filter: 'text' },
    {
      id: 'enabled',
      label: t.runtimeSpecEnabled,
      value: (m) => (specsById[m.id]?.enabled ? 'yes' : 'no'),
      filter: 'enum',
      searchable: false,
      enumLabel: (v) => (v === 'yes' ? t.runtimeSpecEnabled : '–'),
      render: (m) => renderBoolChip(Boolean(specsById[m.id]?.enabled), t.runtimeSpecEnabled),
    },
    {
      id: 'binary',
      label: t.runtimeSpecBinary,
      value: (m) => basename(specsById[m.id]?.binary ?? ''),
      filter: 'text',
    },
    {
      id: 'gpus',
      label: t.runtimeSpecGpus,
      value: (m) => formatGpus(specsById[m.id]?.gpus ?? []),
      filter: 'text',
      searchable: false,
    },
    {
      id: 'pinned',
      label: t.runtimeSpecPinned,
      value: (m) => (specsById[m.id]?.pinned ? 'yes' : 'no'),
      filter: 'enum',
      searchable: false,
      enumLabel: (v) => (v === 'yes' ? t.runtimeSpecPinned : '–'),
      render: (m) => renderBoolChip(Boolean(specsById[m.id]?.pinned), t.runtimeSpecPinned),
    },
    {
      id: 'idle_timeout',
      label: t.runtimeSpecIdleTimeout,
      value: (m) => formatMetric(specsById[m.id]?.idle_timeout_seconds, 0),
      filter: 'text',
      numeric: true,
    },
    {
      // Filled by Task 22's live-status map; today this column is a
      // placeholder so the section's final shape is visible/testable.
      id: 'live_status',
      label: t.tableStatus,
      value: () => 'unknown',
      sortable: false,
      searchable: false,
      render: () => <StatusChip status="standby" label={t.runtimeStatusUnknown} />,
    },
  ];

  const rowActions = (m: PortalModelMapping): RowAction[] => {
    const configured = Boolean(specsById[m.id]?.configured);
    const rowBusy = loadingEditFor === m.id;
    return [
      {
        key: 'edit',
        label: t.runtimeSpecEditAction,
        icon: <EditIcon fontSize="small" />,
        onClick: () => void openEdit(m),
        disabled: rowBusy,
      },
      {
        key: 'delete',
        label: configured ? t.runtimeSpecDelete : t.mappingDelete,
        color: 'error',
        icon: <DeleteIcon fontSize="small" />,
        onClick: () => setConfirmingDeleteId(m.id),
        disabled: rowBusy,
      },
    ];
  };

  // Create / edit sub-view (input mask) -- replaces the tabbed view entirely,
  // matching the rest of the portal's drill-down/sub-view convention.
  if (specMode !== 'list') {
    const editing = specMode !== 'create';
    return (
      <>
        <Breadcrumbs
          ariaLabel={t.breadcrumb}
          backLabel={t.back}
          items={[
            ...trail,
            { label: application.endpoint, onClick: () => setSpecMode('list') },
            { label: editing ? t.runtimeSpecEdit : t.runtimeSpecCreate },
          ]}
        />
        <Panel
          titleId="runtime-spec-form-heading"
          title={editing ? t.runtimeSpecEdit : t.runtimeSpecCreate}
        >
          <Box
            component="form"
            onSubmit={editing ? submitEdit : submitCreate}
            sx={{ display: 'grid', gridTemplateColumns: 'minmax(260px, 560px)', gap: 2.25 }}
          >
            <Typography variant="subtitle2" component="h3">
              {t.runtimeSpecMappingSection}
            </Typography>
            <Field
              id="runtime-spec-gateway-name"
              label={t.mappingGatewayName}
              value={gatewayName}
              onChange={(e) => setGatewayName(e.target.value)}
              required
            />
            <Field
              id="runtime-spec-app-name"
              label={t.mappingAppName}
              value={appName}
              onChange={(e) => setAppName(e.target.value)}
              required
            />
            <SelectField
              id="runtime-spec-status"
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

            <Typography variant="subtitle2" component="h3" sx={{ mt: 1 }}>
              {t.runtimeSpecConfigSection}
            </Typography>
            <FormControlLabel
              control={
                <Checkbox checked={enabled} onChange={(e) => setEnabled(e.target.checked)} />
              }
              label={t.runtimeSpecEnabled}
            />
            <Field
              id="runtime-spec-binary"
              label={t.runtimeSpecBinary}
              value={binary}
              onChange={(e) => setBinary(e.target.value)}
              required
              placeholder="/usr/local/bin/llama-server"
            />
            <Field
              id="runtime-spec-args"
              label={t.runtimeSpecArgs}
              value={argsText}
              onChange={(e) => setArgsText(e.target.value)}
              multiline
              minRows={3}
            />
            <Field
              id="runtime-spec-env"
              label={t.runtimeSpecEnv}
              value={envText}
              onChange={(e) => setEnvText(e.target.value)}
              multiline
              minRows={3}
              helperText={t.runtimeSpecEnvHint}
            />
            <Field
              id="runtime-spec-work-dir"
              label={t.runtimeSpecWorkDir}
              value={workDir}
              onChange={(e) => setWorkDir(e.target.value)}
            />
            <Field
              id="runtime-spec-listen-port"
              type="number"
              label={t.runtimeSpecListenPort}
              value={String(listenPort)}
              onChange={(e) => setListenPort(e.target.value === '' ? 0 : Number(e.target.value))}
              helperText={t.runtimeSpecListenPortHelp}
              inputProps={{ min: 0, max: 65535 }}
            />
            <Field
              id="runtime-spec-health-path"
              label={t.runtimeSpecHealthPath}
              value={healthPath}
              onChange={(e) => setHealthPath(e.target.value)}
              placeholder="/health"
            />
            <Field
              id="runtime-spec-health-timeout"
              type="number"
              label={t.runtimeSpecHealthTimeout}
              value={String(healthTimeoutSeconds)}
              onChange={(e) =>
                setHealthTimeoutSeconds(e.target.value === '' ? 0 : Number(e.target.value))
              }
              inputProps={{ min: 0 }}
            />
            <Field
              id="runtime-spec-startup-timeout"
              type="number"
              label={t.runtimeSpecStartupTimeout}
              value={String(startupTimeoutSeconds)}
              onChange={(e) =>
                setStartupTimeoutSeconds(e.target.value === '' ? 0 : Number(e.target.value))
              }
              inputProps={{ min: 0 }}
            />
            <Field
              id="runtime-spec-idle-timeout"
              type="number"
              label={t.runtimeSpecIdleTimeout}
              value={String(idleTimeoutSeconds)}
              onChange={(e) =>
                setIdleTimeoutSeconds(e.target.value === '' ? 0 : Number(e.target.value))
              }
              inputProps={{ min: 0 }}
            />
            <Field
              id="runtime-spec-admission-wait-timeout"
              type="number"
              label={t.runtimeSpecAdmissionWaitTimeout}
              value={String(admissionWaitTimeoutSeconds)}
              onChange={(e) =>
                setAdmissionWaitTimeoutSeconds(e.target.value === '' ? 0 : Number(e.target.value))
              }
              inputProps={{ min: 0 }}
            />
            <FormControlLabel
              control={<Checkbox checked={pinned} onChange={(e) => setPinned(e.target.checked)} />}
              label={t.runtimeSpecPinned}
            />
            <FormControlLabel
              control={
                <Checkbox checked={vramLocked} onChange={(e) => setVramLocked(e.target.checked)} />
              }
              label={t.runtimeSpecVramLocked}
            />
            <SelectField
              id="runtime-spec-admin-state"
              label={t.runtimeSpecAdminState}
              value={adminState}
              onChange={(e) => setAdminState(e.target.value)}
            >
              {adminStateOptions.map((o) => (
                <option value={o.value} key={o.value || 'auto'}>
                  {t[o.labelKey]}
                </option>
              ))}
            </SelectField>

            <Box sx={{ display: 'grid', gap: 1 }}>
              <Typography variant="subtitle2" component="h3">
                {t.runtimeSpecGpus}
              </Typography>
              {gpuRows.map((row, idx) => (
                <Box
                  key={idx}
                  sx={{ display: 'flex', gap: 1.5, alignItems: 'center', flexWrap: 'wrap' }}
                >
                  <Field
                    id={`runtime-spec-gpu-index-${idx}`}
                    label={t.runtimeSpecGpuIndex}
                    type="number"
                    value={String(row.index)}
                    onChange={(e) =>
                      updateGpuRow(idx, {
                        index: e.target.value === '' ? 0 : Number(e.target.value),
                      })
                    }
                    inputProps={{ min: 0 }}
                    sx={{ maxWidth: 140 }}
                  />
                  <Field
                    id={`runtime-spec-gpu-vram-${idx}`}
                    label={t.runtimeSpecVram}
                    type="number"
                    value={String(row.vramEstimateMb)}
                    onChange={(e) =>
                      updateGpuRow(idx, {
                        vramEstimateMb: e.target.value === '' ? 0 : Number(e.target.value),
                      })
                    }
                    inputProps={{ min: 0 }}
                    sx={{ maxWidth: 160 }}
                  />
                  <Typography variant="body2" color="text.secondary" sx={{ minWidth: 200 }}>
                    {t.runtimeSpecVramMeasured}: {row.vramMeasuredMb} MB
                  </Typography>
                  <Button
                    type="button"
                    size="small"
                    color="secondary"
                    onClick={() => removeGpuRow(idx)}
                  >
                    {t.runtimeSpecGpuRemove}
                  </Button>
                </Box>
              ))}
              <Box>
                <Button type="button" variant="outlined" size="small" onClick={addGpuRow}>
                  {t.runtimeSpecGpuAdd}
                </Button>
              </Box>
            </Box>

            <Box sx={{ display: 'flex', gap: 1.5 }}>
              <Button type="submit" variant="contained" disabled={busy}>
                {editing ? t.save : t.runtimeSpecCreate}
              </Button>
              <Button
                type="button"
                variant="text"
                color="secondary"
                onClick={() => setSpecMode('list')}
              >
                {t.cancel}
              </Button>
            </Box>
          </Box>
        </Panel>
      </>
    );
  }

  return (
    <>
      <Breadcrumbs
        ariaLabel={t.breadcrumb}
        backLabel={t.back}
        items={[...trail, { label: application.endpoint }]}
      />
      <Tabs
        value={tab}
        onChange={(_e, v: Tab) => setTab(v)}
        aria-label={t.runtimeAdmin}
        sx={{ mb: 2.5 }}
      >
        <Tab label={t.runtimeSpecs} value="specs" />
        <Tab label={t.runtimeMatrix} value="matrix" />
        <Tab label={t.runtimeLimits} value="limits" />
        <Tab label={t.runtimeLiveStatus} value="status" />
      </Tabs>

      {tab === 'specs' && (
        <Panel
          titleId="runtime-specs-heading"
          title={t.runtimeSpecs}
          subtitle={t.runtimeSpecsIntro}
          actions={
            <Button variant="contained" startIcon={<AddIcon />} onClick={openCreate}>
              {t.runtimeSpecCreate}
            </Button>
          }
        >
          {warnings.length > 0 && (
            <Box sx={{ display: 'grid', gap: 1, mb: 2 }}>
              {warnings.map((code) => (
                <Alert key={code} severity="warning">
                  {runtimeWarningLabelByCode[code] ? t[runtimeWarningLabelByCode[code]] : code}
                </Alert>
              ))}
            </Box>
          )}
          <ListTable
            rows={mappings}
            columns={columns}
            rowKey={(m) => m.id}
            actions={rowActions}
            storageKey="op.runtimeSpecs"
            minWidth={900}
            labels={listTableLabels(t)}
            loading={mappingsLoading || mappingsData === null}
          />
        </Panel>
      )}
      {tab === 'matrix' && (
        <Panel titleId="runtime-matrix-heading" title={t.runtimeMatrix}>
          <Typography color="text.secondary">{t.runtimeAreaPlaceholder}</Typography>
        </Panel>
      )}
      {tab === 'limits' && (
        <Panel titleId="runtime-limits-heading" title={t.runtimeLimits}>
          <Typography color="text.secondary">{t.runtimeAreaPlaceholder}</Typography>
        </Panel>
      )}
      {tab === 'status' && (
        <Panel titleId="runtime-status-heading" title={t.runtimeLiveStatus}>
          <Typography color="text.secondary">{t.runtimeAreaPlaceholder}</Typography>
        </Panel>
      )}

      <ConfirmDialog
        open={confirmingDeleteId !== ''}
        title={confirmIsSpecDelete ? t.runtimeSpecDeleteConfirm : t.mappingDeleteConfirm}
        confirmLabel={confirmIsSpecDelete ? t.runtimeSpecDelete : t.mappingDelete}
        cancelLabel={t.mappingCancel}
        onConfirm={() => void confirmDelete()}
        onCancel={() => setConfirmingDeleteId('')}
      />
    </>
  );
}
