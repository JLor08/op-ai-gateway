// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useState, useEffect, useRef, type SubmitEvent } from 'react';
import {
  Box,
  Button,
  Checkbox,
  CircularProgress,
  FormControlLabel,
  Typography,
} from '@mui/material';
import AddIcon from '@mui/icons-material/Add';
import EditIcon from '@mui/icons-material/Edit';
import DeleteIcon from '@mui/icons-material/Delete';
import BlockIcon from '@mui/icons-material/Block';
import CheckCircleIcon from '@mui/icons-material/CheckCircle';
import SpeedIcon from '@mui/icons-material/Speed';
import type {
  ApplicationStatus,
  CreateMappingRequest,
  PortalApplication,
  PortalModelMapping,
  PortalServer,
  SyncResult,
  UpdateMappingRequest,
} from '../api';
import type { Translation, PortalApi } from './shared/types';
import { formatMetric, formatPortalError } from './shared/format';
import { useResource } from './shared/useResource';
import { applicationStatusOptions, applicationStatusLabelByKey } from './shared/application';
import { Panel } from './shared/Panel';
import { StatusChip } from './shared/StatusChip';
import { SecretReveal } from './shared/SecretReveal';
import { Field } from './shared/Field';
import { SelectField } from './shared/SelectField';
import { RowActions } from './shared/RowActions';
import { ConfirmDialog } from './shared/ConfirmDialog';
import { Breadcrumbs, type BreadcrumbItem } from './shared/Breadcrumbs';
import { ListTable, listTableLabels, type ListColumn } from './shared/ListTable';
import { makeVisionColumn } from './shared/visionColumn';
import type { RowAction } from './shared/RowActionsMenu';
import { useToast } from './shared/ToastProvider';
import { pollBenchmarkStatus } from './shared/benchmark';
import { BenchmarkSection, type BenchmarkScope } from './BenchmarkSection';

type Mode = 'list' | 'create' | { edit: PortalModelMapping };

export function MappingSection({
  t,
  api,
  server,
  application,
  onModelsChanged,
  trail = [],
  pollIntervalMs,
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
    | 'createMapping'
    | 'deleteMapping'
    | 'mappingBenchmarks'
    | 'mappings'
    | 'probeMappingContext'
    | 'subscribeBenchmark'
    | 'syncApplicationModels'
    | 'updateMapping'
  >;
  server: PortalServer;
  application: PortalApplication;
  onModelsChanged?: () => void;
  trail?: BreadcrumbItem[];
  // Benchmark status-poll cadence (ms); injectable so tests drive the loop
  // without a real 2s wait. Defaults to the shared helper's cadence.
  pollIntervalMs?: number;
}>) {
  const {
    data: mappingsData,
    setData: setMappings,
    error,
    setError,
    reload,
    loading,
  } = useResource(
    () => api.mappings(application.id).then((r) => r.data),
    [api, application.id, t],
    t,
  );
  const mappings = mappingsData ?? [];
  const { showError } = useToast();
  useEffect(() => {
    if (error) showError(error);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [error]);

  const [mode, setMode] = useState<Mode>('list');
  const [busy, setBusy] = useState(false);
  const [confirmingDeleteId, setConfirmingDeleteId] = useState('');
  const [syncing, setSyncing] = useState(false);
  const [syncResult, setSyncResult] = useState<SyncResult | null>(null);
  // When set, the consolidated benchmark area is shown pre-scoped to this slice
  // (a mapping row → its model; the panel action → the whole application).
  const [benchmarkScope, setBenchmarkScope] = useState<BenchmarkScope | null>(null);
  // Manual context-size probe (edit form): running state + whether this server is
  // busy with a benchmark/probe run (polled while editing so the button disables).
  const [probing, setProbing] = useState(false);
  const [serverBusy, setServerBusy] = useState(false);
  // Guards the async probe against a setState after the component unmounts mid-run.
  const mountedRef = useRef(true);
  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
    };
  }, []);

  const [gatewayName, setGatewayName] = useState('');
  const [appName, setAppName] = useState('');
  const [status, setStatus] = useState<ApplicationStatus>('active');
  const [contextSize, setContextSize] = useState('');
  const [energyWhPerToken, setEnergyWhPerToken] = useState('');
  const [genTps, setGenTps] = useState('');
  const [promptTps, setPromptTps] = useState('');
  const [loadTimeMs, setLoadTimeMs] = useState('');
  const [isMtp, setIsMtp] = useState(false);
  const [visionCapable, setVisionCapable] = useState(false);
  const [metricsLocked, setMetricsLocked] = useState(false);
  const [maxConcurrency, setMaxConcurrency] = useState('');
  const [recommendedConcurrency, setRecommendedConcurrency] = useState('');
  const [genTpsAtCapacity, setGenTpsAtCapacity] = useState('');

  // Parse a free-text numeric input into a non-negative number; blank/invalid → 0
  // (the backend treats 0 as "unknown").
  const num = (s: string) => {
    const n = Number(s.trim());
    return s.trim() === '' || Number.isNaN(n) || n < 0 ? 0 : n;
  };

  function openCreate() {
    setError('');
    setGatewayName('');
    setAppName('');
    setStatus('active');
    setContextSize('');
    setEnergyWhPerToken('');
    setGenTps('');
    setPromptTps('');
    setLoadTimeMs('');
    setIsMtp(false);
    setVisionCapable(false);
    setMetricsLocked(false);
    setMaxConcurrency('');
    setRecommendedConcurrency('');
    setGenTpsAtCapacity('');
    setMode('create');
  }

  function openEdit(row: PortalModelMapping) {
    setError('');
    setGatewayName(row.gateway_model_name);
    setAppName(row.app_model_name);
    setStatus(row.status);
    setContextSize(row.context_size ? String(row.context_size) : '');
    setEnergyWhPerToken(row.energy_wh_per_token ? String(row.energy_wh_per_token) : '');
    setGenTps(row.gen_tokens_per_second ? String(row.gen_tokens_per_second) : '');
    setPromptTps(row.prompt_tokens_per_second ? String(row.prompt_tokens_per_second) : '');
    setLoadTimeMs(row.load_time_ms ? String(row.load_time_ms) : '');
    setIsMtp(row.is_mtp);
    setVisionCapable(row.vision_capable);
    setMetricsLocked(row.metrics_locked);
    setMaxConcurrency(row.max_concurrency ? String(row.max_concurrency) : '');
    setRecommendedConcurrency(
      row.recommended_concurrency ? String(row.recommended_concurrency) : '',
    );
    setGenTpsAtCapacity(
      row.gen_tokens_per_second_at_capacity ? String(row.gen_tokens_per_second_at_capacity) : '',
    );
    setMode({ edit: row });
  }

  async function submitCreate(event: SubmitEvent<HTMLFormElement>) {
    event.preventDefault();
    setBusy(true);
    try {
      const body: CreateMappingRequest = {
        gateway_model_name: gatewayName,
        app_model_name: appName,
        status,
        gen_tokens_per_second: num(genTps),
        prompt_tokens_per_second: num(promptTps),
        load_time_ms: num(loadTimeMs),
        context_size: num(contextSize),
        energy_wh_per_token: num(energyWhPerToken),
        is_mtp: isMtp,
        vision_capable: visionCapable,
        metrics_locked: metricsLocked,
        max_concurrency: num(maxConcurrency),
        recommended_concurrency: num(recommendedConcurrency),
        gen_tokens_per_second_at_capacity: num(genTpsAtCapacity),
      };
      const created = await api.createMapping(application.id, body);
      setMappings((current) => [created, ...(current ?? [])]);
      setMode('list');
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setBusy(false);
    }
  }

  async function submitEdit(event: SubmitEvent<HTMLFormElement>) {
    event.preventDefault();
    if (mode === 'list' || mode === 'create') return;
    const id = mode.edit.id;
    setBusy(true);
    try {
      const body: UpdateMappingRequest = {
        gateway_model_name: gatewayName,
        app_model_name: appName,
        status,
        gen_tokens_per_second: num(genTps),
        prompt_tokens_per_second: num(promptTps),
        load_time_ms: num(loadTimeMs),
        context_size: num(contextSize),
        energy_wh_per_token: num(energyWhPerToken),
        is_mtp: isMtp,
        vision_capable: visionCapable,
        metrics_locked: metricsLocked,
        max_concurrency: num(maxConcurrency),
        recommended_concurrency: num(recommendedConcurrency),
        gen_tokens_per_second_at_capacity: num(genTpsAtCapacity),
      };
      const updated = await api.updateMapping(id, body);
      setMappings((current) => (current ?? []).map((row) => (row.id === id ? updated : row)));
      setMode('list');
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setBusy(false);
    }
  }

  async function toggleStatus(row: PortalModelMapping) {
    const nextStatus: ApplicationStatus = row.status === 'active' ? 'disabled' : 'active';
    try {
      const updated = await api.updateMapping(row.id, { status: nextStatus });
      setMappings((current) =>
        (current ?? []).map((item) => (item.id === row.id ? updated : item)),
      );
    } catch (err) {
      showError(formatPortalError(err, t));
    }
  }

  async function removeMapping(id: string) {
    try {
      await api.deleteMapping(id);
      setMappings((current) => (current ?? []).filter((row) => row.id !== id));
      setConfirmingDeleteId('');
    } catch (err) {
      showError(formatPortalError(err, t));
    }
  }

  async function syncModels() {
    setSyncing(true);
    try {
      const result = await api.syncApplicationModels(application.id);
      setSyncResult(result);
      await reload();
      onModelsChanged?.();
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setSyncing(false);
    }
  }

  // While the edit form is open, poll the running benchmarks so the probe button
  // disables whenever THIS server is busy (a benchmark OR our own probe run appears
  // here → the button stays disabled until it finishes). Mirrors the ServerList chip
  // cadence (~3s). Idle (list/create) → no poll.
  const editingId = mode !== 'list' && mode !== 'create' ? mode.edit.id : '';
  useEffect(() => {
    if (!editingId) {
      setServerBusy(false);
      return;
    }
    let cancelled = false;
    const tick = () => {
      api
        .activeBenchmarks()
        .then((runs) => {
          if (!cancelled) setServerBusy(runs.some((r) => r.server_id === server.id));
        })
        .catch(() => {
          /* non-blocking — the button just falls back to its other gates */
        });
    };
    tick();
    const id = setInterval(tick, 3000);
    return () => {
      cancelled = true;
      clearInterval(id);
    };
  }, [api, server.id, editingId]);

  // Manual context-size probe: warm-load the model + read its context via the app's
  // context_probe_path, then fill the field (no auto-save). POST → poll the benchmark
  // status to completion → read this mapping's reported context_size. Errors / 0 →
  // toast, field unchanged.
  async function probeContext(row: PortalModelMapping) {
    setProbing(true);
    try {
      await api.probeMappingContext(row.id);
      const status = await pollBenchmarkStatus(api, server.id, { intervalMs: pollIntervalMs });
      if (!mountedRef.current) return;
      const result = (status.results ?? []).find((r) => r.mapping_id === row.id);
      const ctxSize = result?.context_size ?? 0;
      if (ctxSize > 0) {
        setContextSize(String(ctxSize)); // fill only — the user saves via Save
      } else {
        showError(
          result?.error
            ? `${t.mappingProbeContextFailed}: ${result.error}`
            : t.mappingProbeContextFailed,
        );
      }
    } catch (err) {
      // 409 (already_running / server_in_use) + any poll/network failure land here.
      if (mountedRef.current) showError(formatPortalError(err, t));
    } finally {
      if (mountedRef.current) setProbing(false);
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
      id: 'status',
      label: t.tableStatus,
      value: (m) => m.status,
      filter: 'enum',
      searchable: false,
      enumLabel: (v) => t[applicationStatusLabelByKey[v as ApplicationStatus] ?? 'statusActive'],
      render: (m) => (
        <StatusChip status={m.status} label={t[applicationStatusLabelByKey[m.status]]} />
      ),
    },
    {
      id: 'context_size',
      label: t.mappingContextSize,
      value: (m) => formatMetric(m.context_size, 0),
      filter: 'text',
      numeric: true,
      defaultHidden: true,
    },
    {
      id: 'energy_wh_per_token',
      label: t.mappingEnergyWhPerToken,
      // Watt-hours per single token: the significant digits live far behind the
      // decimal point, so this column needs a much longer tail than the others.
      value: (m) => formatMetric(m.energy_wh_per_token, 10),
      filter: 'text',
      numeric: true,
      defaultHidden: true,
    },
    {
      id: 'gen_tps',
      label: t.mappingGenTokensPerSecond,
      value: (m) => formatMetric(m.gen_tokens_per_second, 2),
      filter: 'text',
      numeric: true,
      defaultHidden: true,
    },
    {
      id: 'prompt_tps',
      label: t.mappingPromptTokensPerSecond,
      value: (m) => formatMetric(m.prompt_tokens_per_second, 2),
      filter: 'text',
      numeric: true,
      defaultHidden: true,
    },
    {
      id: 'load_time_ms',
      label: t.mappingLoadTimeMs,
      value: (m) => formatMetric(m.load_time_ms, 0),
      filter: 'text',
      numeric: true,
      defaultHidden: true,
    },
    {
      id: 'is_mtp',
      label: t.mappingIsMtp,
      value: (m) => (m.is_mtp ? 'MTP' : '—'),
      filter: 'text',
      defaultHidden: true,
    },
    makeVisionColumn(t, (m) => m.vision_capable),
    {
      id: 'max_concurrency',
      label: t.mappingMaxConcurrency,
      value: (m) => formatMetric(m.max_concurrency, 0),
      filter: 'text',
      numeric: true,
      defaultHidden: true,
    },
    {
      id: 'recommended_concurrency',
      label: t.mappingRecommendedConcurrency,
      value: (m) => formatMetric(m.recommended_concurrency, 0),
      filter: 'text',
      numeric: true,
      defaultHidden: true,
    },
    {
      id: 'gen_tps_at_capacity',
      label: t.mappingGenTpsAtCapacity,
      value: (m) => formatMetric(m.gen_tokens_per_second_at_capacity, 2),
      filter: 'text',
      numeric: true,
      defaultHidden: true,
    },
  ];

  const rowActions = (row: PortalModelMapping): RowAction[] => [
    {
      key: 'edit',
      label: t.mappingEdit,
      icon: <EditIcon fontSize="small" />,
      onClick: () => openEdit(row),
    },
    {
      key: 'benchmark',
      label: t.runBenchmark,
      icon: <SpeedIcon fontSize="small" />,
      onClick: () =>
        setBenchmarkScope({ kind: 'mapping', id: row.id, name: row.gateway_model_name }),
    },
    {
      key: 'toggle',
      label: row.status === 'active' ? t.tokenActionDisable : t.tokenActionEnable,
      icon:
        row.status === 'active' ? (
          <BlockIcon fontSize="small" />
        ) : (
          <CheckCircleIcon fontSize="small" />
        ),
      onClick: () => void toggleStatus(row),
    },
    {
      key: 'delete',
      label: t.mappingDelete,
      color: 'error',
      icon: <DeleteIcon fontSize="small" />,
      onClick: () => setConfirmingDeleteId(row.id),
    },
  ];

  // Consolidated benchmark sub-view, pre-scoped to the clicked slice (a mapping
  // row → that model; the panel action → the whole application).
  if (benchmarkScope) {
    return (
      <>
        <Breadcrumbs
          ariaLabel={t.breadcrumb}
          backLabel={t.back}
          items={[
            ...trail,
            { label: application.endpoint, onClick: () => setBenchmarkScope(null) },
            { label: t.benchmarkArea },
          ]}
        />
        <BenchmarkSection
          key={`bench-${server.id}`}
          t={t}
          api={api}
          server={server}
          initialScope={benchmarkScope}
          onModelsChanged={onModelsChanged}
          pollIntervalMs={pollIntervalMs}
        />
      </>
    );
  }

  // Create / edit sub-view (input mask).
  if (mode !== 'list') {
    const editing = mode !== 'create';
    // The mapping being edited (null on the create form → no probe button).
    const editRow = mode === 'create' ? null : mode.edit;
    return (
      <>
        <Breadcrumbs
          ariaLabel={t.breadcrumb}
          backLabel={t.back}
          items={[
            ...trail,
            { label: application.endpoint, onClick: () => setMode('list') },
            { label: editing ? t.mappingEditTitle : t.mappingCreate },
          ]}
        />
        <Panel
          titleId="mapping-form-heading"
          title={editing ? t.mappingEditTitle : t.mappingCreate}
        >
          <Box
            component="form"
            onSubmit={editing ? submitEdit : submitCreate}
            sx={{ display: 'grid', gridTemplateColumns: 'minmax(260px, 480px)', gap: 2.25 }}
          >
            <Field
              id="mapping-gateway-name"
              label={t.mappingGatewayName}
              value={gatewayName}
              onChange={(e) => setGatewayName(e.target.value)}
              required
            />
            <Field
              id="mapping-app-name"
              label={t.mappingAppName}
              value={appName}
              onChange={(e) => setAppName(e.target.value)}
              required
            />
            <SelectField
              id="mapping-status"
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
            <Box sx={{ display: 'grid', gap: 1.25 }}>
              <Typography variant="subtitle2" component="h3" sx={{ mt: 0.5 }}>
                {t.mappingMetricsSection}
              </Typography>
              <Typography variant="caption" color="text.secondary">
                {t.mappingMetricsHint}
              </Typography>
              <Field
                id="mapping-context-size"
                type="number"
                label={t.mappingContextSize}
                value={contextSize}
                onChange={(e) => setContextSize(e.target.value)}
                inputProps={{ min: 0, step: 1 }}
              />
              {editRow && (
                <Box>
                  <Button
                    type="button"
                    variant="outlined"
                    size="small"
                    disabled={
                      probing || serverBusy || (application.context_probe_path ?? '').trim() === ''
                    }
                    startIcon={probing ? <CircularProgress size={16} color="inherit" /> : undefined}
                    onClick={() => void probeContext(editRow)}
                  >
                    {probing ? t.mappingProbeContextRunning : t.mappingProbeContext}
                  </Button>
                </Box>
              )}
              <Field
                id="mapping-energy-wh-per-token"
                type="number"
                label={t.mappingEnergyWhPerToken}
                value={energyWhPerToken}
                onChange={(e) => setEnergyWhPerToken(e.target.value)}
                inputProps={{ min: 0, step: 'any' }}
              />
              <Field
                id="mapping-gen-tps"
                type="number"
                label={t.mappingGenTokensPerSecond}
                value={genTps}
                onChange={(e) => setGenTps(e.target.value)}
                inputProps={{ min: 0, step: 'any' }}
              />
              <Field
                id="mapping-prompt-tps"
                type="number"
                label={t.mappingPromptTokensPerSecond}
                value={promptTps}
                onChange={(e) => setPromptTps(e.target.value)}
                inputProps={{ min: 0, step: 'any' }}
              />
              <Field
                id="mapping-load-ms"
                type="number"
                label={t.mappingLoadTimeMs}
                value={loadTimeMs}
                onChange={(e) => setLoadTimeMs(e.target.value)}
                inputProps={{ min: 0, step: 1 }}
              />
              <Field
                id="mapping-max-concurrency"
                type="number"
                label={t.mappingMaxConcurrency}
                value={maxConcurrency}
                onChange={(e) => setMaxConcurrency(e.target.value)}
                inputProps={{ min: 0, step: 1 }}
              />
              <Field
                id="mapping-recommended-concurrency"
                type="number"
                label={t.mappingRecommendedConcurrency}
                value={recommendedConcurrency}
                onChange={(e) => setRecommendedConcurrency(e.target.value)}
                inputProps={{ min: 0, step: 1 }}
              />
              <Field
                id="mapping-gen-tps-at-capacity"
                type="number"
                label={t.mappingGenTpsAtCapacity}
                value={genTpsAtCapacity}
                onChange={(e) => setGenTpsAtCapacity(e.target.value)}
                inputProps={{ min: 0, step: 'any' }}
              />
              <FormControlLabel
                control={<Checkbox checked={isMtp} onChange={(e) => setIsMtp(e.target.checked)} />}
                label={t.mappingIsMtp}
              />
              <FormControlLabel
                control={
                  <Checkbox
                    checked={visionCapable}
                    onChange={(e) => setVisionCapable(e.target.checked)}
                  />
                }
                label={t.mappingVisionCapable}
              />
              <FormControlLabel
                control={
                  <Checkbox
                    checked={metricsLocked}
                    onChange={(e) => setMetricsLocked(e.target.checked)}
                  />
                }
                label={t.mappingMetricsLocked}
              />
            </Box>
            <Box sx={{ display: 'flex', gap: 1.5 }}>
              <Button type="submit" variant="contained" disabled={busy}>
                {editing ? t.mappingSave : t.mappingCreate}
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

  return (
    <>
      <Breadcrumbs
        ariaLabel={t.breadcrumb}
        backLabel={t.back}
        items={[...trail, { label: application.endpoint }]}
      />
      <Panel
        titleId="mapping-heading"
        title={t.modelMappings}
        subtitle={t.modelMappingsIntro}
        actions={
          <RowActions>
            <Button
              variant="outlined"
              size="small"
              type="button"
              onClick={() => void syncModels()}
              disabled={syncing}
            >
              {t.syncModels}
            </Button>
            <Button
              variant="outlined"
              size="small"
              type="button"
              startIcon={<SpeedIcon />}
              onClick={() =>
                setBenchmarkScope({
                  kind: 'application',
                  id: application.id,
                  name: application.endpoint,
                })
              }
              disabled={syncing}
            >
              {t.runBenchmark}
            </Button>
            <Button
              variant="contained"
              size="small"
              startIcon={<AddIcon />}
              type="button"
              onClick={openCreate}
            >
              {t.mappingCreate}
            </Button>
          </RowActions>
        }
      >
        {syncResult && (
          <SecretReveal title={t.syncModels}>
            <p>
              {t.syncAdded}: {syncResult.added}
            </p>
            <p>
              {t.syncDisabled}: {syncResult.disabled}
            </p>
            <p>
              {t.syncUnchanged}: {syncResult.unchanged}
            </p>
            <p>
              {t.syncConflicted}: {syncResult.conflicted}
            </p>
          </SecretReveal>
        )}

        <ListTable
          rows={mappings}
          columns={columns}
          rowKey={(m) => m.id}
          actions={rowActions}
          maxInlineActions={4}
          storageKey="op.mappings"
          labels={listTableLabels(t)}
          loading={loading || mappingsData === null}
        />

        <ConfirmDialog
          open={Boolean(confirmingDeleteId)}
          title={t.mappingDeleteConfirm}
          confirmLabel={t.mappingDelete}
          cancelLabel={t.mappingCancel}
          onConfirm={() => removeMapping(confirmingDeleteId)}
          onCancel={() => setConfirmingDeleteId('')}
        />
      </Panel>
    </>
  );
}
