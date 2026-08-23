// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useEffect, useState, type ReactNode } from 'react';
import { Box, Chip } from '@mui/material';
import ListAltIcon from '@mui/icons-material/ListAlt';
import type { ModelOption, ModelVisibility } from '../api';
import type { PortalApi, Translation } from './shared/types';
import { PageTitle } from './shared/PageTitle';
import { Panel } from './shared/Panel';
import { SelectField } from './shared/SelectField';
import { StatusChip } from './shared/StatusChip';
import { Breadcrumbs } from './shared/Breadcrumbs';
import { ListTable, listTableLabels, type ListColumn } from './shared/ListTable';
import { makeVisionColumn } from './shared/visionColumn';
import type { RowAction } from './shared/RowActionsMenu';
import { ModelServersSection } from './ModelServersSection';
import { GroupServersSection } from './GroupServersSection';
import { useToast } from './shared/ToastProvider';
import { formatPortalError } from './shared/format';

// "list" or the per-model detail sub-view (the servers offering that model).
type Mode = 'list' | { kind: 'detail'; model: ModelOption };

export function ModelList({
  t,
  models,
  api,
  isAdmin = false,
  loading = false,
  onModelsChanged,
  onDetailViewChange,
}: Readonly<{
  t: Translation;
  models: ModelOption[];
  api?: PortalApi;
  isAdmin?: boolean;
  loading?: boolean;
  onModelsChanged?: () => void;
  onDetailViewChange?: (open: boolean) => void;
}>) {
  const { showError } = useToast();
  const [mode, setMode] = useState<Mode>('list');
  // Report the detail sub-view open/closed state so the parent can hide siblings
  // (e.g. the admin group table) while a model's per-server detail is shown.
  useEffect(() => {
    onDetailViewChange?.(typeof mode !== 'string' && mode.kind === 'detail');
  }, [mode, onDetailViewChange]);
  // Optimistic per-model visibility overrides (id -> visibility) applied while a
  // setModelVisibility call is in flight / until the models list refetches.
  const [pending, setPending] = useState<Record<string, ModelVisibility>>({});

  const visibilityLabel = (v: ModelVisibility) => {
    switch (v) {
      case 'hidden':
        return t.modelVisibilityHidden;
      case 'locked':
        return t.modelVisibilityLocked;
      default:
        return t.modelVisibilityShown;
    }
  };

  const effectiveVisibility = (model: ModelOption): ModelVisibility =>
    pending[model.id] ?? model.visibility ?? 'shown';

  async function changeVisibility(model: ModelOption, next: ModelVisibility) {
    if (!api) return;
    setPending((prev) => ({ ...prev, [model.id]: next }));
    try {
      await api.setModelVisibility(model.id, next);
      onModelsChanged?.();
    } catch (err) {
      // Revert the optimistic value and re-sync from the server.
      setPending((prev) => {
        const copy = { ...prev };
        delete copy[model.id];
        return copy;
      });
      showError(formatPortalError(err, t));
      onModelsChanged?.();
    }
  }

  // A group NAME is itself a gateway_model_name, so its visibility is editable
  // exactly like a model's (setModelVisibility accepts group names). Whether a row
  // is a group is shown in its own "Typ" column, not here.
  function renderVisibility(model: ModelOption) {
    const current = effectiveVisibility(model);
    if (isAdmin && api) {
      return (
        <Box sx={{ minWidth: 140 }}>
          <SelectField
            id={`model-visibility-${model.id}`}
            value={current}
            onChange={(e) => void changeVisibility(model, e.target.value as ModelVisibility)}
            inputProps={{ 'aria-label': `${t.modelVisibility}: ${model.id}` }}
          >
            <option value="shown">{t.modelVisibilityShown}</option>
            <option value="hidden">{t.modelVisibilityHidden}</option>
            <option value="locked">{t.modelVisibilityLocked}</option>
          </SelectField>
        </Box>
      );
    }
    return <Chip size="small" variant="outlined" label={visibilityLabel(current)} />;
  }

  const columns: ListColumn<ModelOption>[] = [
    { id: 'id', label: t.tableModel, value: (m) => m.id, filter: 'text' },
    {
      id: 'type',
      label: t.tableModelType,
      value: (m) => (m.is_group ? 'group' : 'model'),
      searchable: false,
      filter: 'enum',
      enumLabel: (v) => (v === 'group' ? t.modelGroupChip : t.tableModel),
      render: (m) => <Chip size="small" label={m.is_group ? t.modelGroupChip : t.tableModel} />,
    },
    {
      id: 'flavors',
      label: t.tableApis,
      value: (m) => m.flavors.join(', ') || '-',
      filter: 'text',
    },
    {
      id: 'offered',
      label: t.tableModelOffered,
      numeric: true,
      value: (m) => String(m.offered_on_count ?? 0),
      render: (m) =>
        (m.offered_on_count ?? 0) > 0 ? (
          <StatusChip status="standby" label={String(m.offered_on_count)} />
        ) : (
          '-'
        ),
    },
    {
      id: 'loaded',
      label: t.tableModelLoaded,
      numeric: true,
      value: (m) => String(m.loaded_on?.length ?? 0),
      render: (m) =>
        (m.loaded_on?.length ?? 0) > 0 ? (
          <StatusChip status="success" label={String(m.loaded_on!.length)} />
        ) : (
          '-'
        ),
    },
    makeVisionColumn(t, (m) => !!m.vision),
    {
      id: 'visibility',
      label: t.modelVisibility,
      value: (m) => effectiveVisibility(m),
      searchable: false,
      filter: 'enum',
      enumLabel: (v) => visibilityLabel(v as ModelVisibility),
      render: (m) => renderVisibility(m),
    },
  ];

  const rowActions = (model: ModelOption): RowAction[] => [
    {
      key: 'details',
      label: t.modelDetailsAction,
      icon: <ListAltIcon fontSize="small" />,
      onClick: () => setMode({ kind: 'detail', model }),
    },
  ];

  // Per-model detail sub-view: the servers offering this gateway model.
  if (typeof mode !== 'string' && mode.kind === 'detail') {
    let detail: ReactNode = null;
    if (api) {
      detail = mode.model.is_group ? (
        <GroupServersSection t={t} api={api} group={mode.model} isAdmin={isAdmin} />
      ) : (
        <ModelServersSection t={t} api={api} model={mode.model} isAdmin={isAdmin} />
      );
    }
    return (
      <>
        <Breadcrumbs
          ariaLabel={t.breadcrumb}
          backLabel={t.back}
          items={[{ label: t.models, onClick: () => setMode('list') }, { label: mode.model.id }]}
        />
        {detail}
      </>
    );
  }

  return (
    <>
      <PageTitle title={t.models} subtitle={t.modelsIntro} />

      <Panel
        titleId="models-heading"
        title={t.models}
        subtitle={isAdmin ? t.modelVisibilityHelp : undefined}
      >
        <ListTable
          rows={models}
          columns={columns}
          rowKey={(m) => m.id}
          actions={rowActions}
          storageKey="op.models"
          labels={listTableLabels(t, { empty: t.modelsEmpty })}
          loading={loading}
        />
      </Panel>
    </>
  );
}
