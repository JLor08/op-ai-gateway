// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useState, useEffect } from 'react';
import { Button } from '@mui/material';
import AddIcon from '@mui/icons-material/Add';
import EditIcon from '@mui/icons-material/Edit';
import DeleteIcon from '@mui/icons-material/Delete';
import BlockIcon from '@mui/icons-material/Block';
import CheckCircleIcon from '@mui/icons-material/CheckCircle';
import SpeedIcon from '@mui/icons-material/Speed';
import type {
  ApplicationStatus,
  PortalApplication,
  PortalModelMapping,
  PortalServer,
  SyncResult,
} from '../api';
import type { Translation, PortalApi } from './shared/types';
import { formatPortalError } from './shared/format';
import { useResource } from './shared/useResource';
import { Panel } from './shared/Panel';
import { SecretReveal } from './shared/SecretReveal';
import { RowActions } from './shared/RowActions';
import { ConfirmDialog } from './shared/ConfirmDialog';
import { Breadcrumbs, type BreadcrumbItem } from './shared/Breadcrumbs';
import { ListTable, listTableLabels } from './shared/ListTable';
import { mappingColumns, MAPPING_TABLE_STORAGE_KEY } from './shared/mappingColumns';
import type { RowAction } from './shared/RowActionsMenu';
import { useToast } from './shared/ToastProvider';
import { BenchmarkSection, type BenchmarkScope } from './BenchmarkSection';
import { MappingForm, type MappingFormValues } from './MappingForm';

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
    | 'probeMappingVram'
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
  function openCreate() {
    setError('');
    setMode('create');
  }

  function openEdit(row: PortalModelMapping) {
    setError('');
    setMode({ edit: row });
  }

  async function submitCreate(values: MappingFormValues) {
    setBusy(true);
    try {
      // An ordinary application's mapping has no runtime spec behind it, so
      // this screen is the ONLY writer of every one of these fields -- it sends
      // the whole form. (The agent-runtime tab, which shares this mask, omits
      // the app model name: there the runtime spec owns it.)
      const created = await api.createMapping(application.id, values);
      setMappings((current) => [created, ...(current ?? [])]);
      setMode('list');
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setBusy(false);
    }
  }

  async function submitEdit(id: string, values: MappingFormValues) {
    setBusy(true);
    try {
      const updated = await api.updateMapping(id, values);
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

  const columns = mappingColumns(t);

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

  // Create / edit sub-view (input mask). The mask itself is `MappingForm`, the
  // one definition the agent runtime's "model mapping" tab renders too -- "the
  // same edit form" is a requirement there, so it is one component rather than
  // two copies that agree today.
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
          <MappingForm
            // Re-initialises the mask when the edited row changes; the form
            // derives its fields from `row` once and never re-syncs from props.
            key={editRow?.id ?? 'create'}
            t={t}
            api={api}
            serverId={server.id}
            contextProbePath={application.context_probe_path ?? ''}
            row={editRow}
            // An ordinary application has no runtime spec, so nothing else
            // writes its app_model_name: this screen owns BOTH names.
            busy={busy}
            onSubmit={(values) =>
              void (editRow ? submitEdit(editRow.id, values) : submitCreate(values))
            }
            onCancel={() => setMode('list')}
            pollIntervalMs={pollIntervalMs}
          />
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
          storageKey={MAPPING_TABLE_STORAGE_KEY}
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
