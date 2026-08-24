// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useEffect, useState, type SubmitEvent } from 'react';
import { Box, Button, Checkbox, FormControlLabel, Typography } from '@mui/material';
import AddIcon from '@mui/icons-material/Add';
import EditIcon from '@mui/icons-material/Edit';
import DeleteIcon from '@mui/icons-material/Delete';
import BlockIcon from '@mui/icons-material/Block';
import CheckCircleIcon from '@mui/icons-material/CheckCircle';
import {
  PortalApiError,
  type ApplicationStatus,
  type CreateModelGroupRequest,
  type ModelGroupMemberOrder,
  type ModelGroupMinSpeedFallback,
  type ModelOption,
  type ModelVisibility,
  type PortalModelGroup,
  type UpdateModelGroupRequest,
} from '../api';
import type { PortalApi, Translation } from './shared/types';
import { formatPortalError } from './shared/format';
import { useResource } from './shared/useResource';
import { applicationStatusOptions, applicationStatusLabelByKey } from './shared/application';
import { Panel } from './shared/Panel';
import { StatusChip } from './shared/StatusChip';
import { Field } from './shared/Field';
import { SelectField } from './shared/SelectField';
import { RowActions } from './shared/RowActions';
import { ConfirmDialog } from './shared/ConfirmDialog';
import { Breadcrumbs } from './shared/Breadcrumbs';
import { ListTable, listTableLabels, type ListColumn } from './shared/ListTable';
import type { RowAction } from './shared/RowActionsMenu';
import { useToast } from './shared/ToastProvider';
import { OrderedMemberList } from './shared/OrderedMemberList';

type Mode = 'list' | 'create' | { edit: PortalModelGroup };

const failoverModes = ['sticky', 'climb_up'] as const;
const traversalModes = ['depth', 'breadth', 'round_robin'] as const;
const memberOrders: ModelGroupMemberOrder[] = ['priority', 'speed'];
const minSpeedFallbacks: ModelGroupMinSpeedFallback[] = ['error', 'ignore'];

// Parse a free-text numeric input into a non-negative number; blank/invalid ->
// 0 (min_tokens_per_second treats 0 as "off").
function nonNegativeNumber(raw: string): number {
  const n = Number(raw.trim());
  return raw.trim() === '' || Number.isNaN(n) || n < 0 ? 0 : n;
}

export function ModelGroupSection({
  t,
  api,
  models,
  onModelsChanged,
}: Readonly<{
  t: Translation;
  api: Pick<
    PortalApi,
    'createModelGroup' | 'deleteModelGroup' | 'modelGroups' | 'updateModelGroup'
  >;
  models: ModelOption[];
  onModelsChanged?: () => void;
}>) {
  const { data, setData, error, loading } = useResource(
    () => api.modelGroups().then((r) => r.data),
    [api, t],
    t,
  );
  const groups = data ?? [];
  const { showError } = useToast();
  useEffect(() => {
    if (error) showError(error);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [error]);

  const [mode, setMode] = useState<Mode>('list');
  const [busy, setBusy] = useState(false);
  const [confirmingDeleteId, setConfirmingDeleteId] = useState('');

  const [gatewayName, setGatewayName] = useState('');
  const [displayName, setDisplayName] = useState('');
  const [status, setStatus] = useState<ApplicationStatus>('active');
  const [failoverMode, setFailoverMode] = useState<string>('sticky');
  const [visibility, setVisibility] = useState<ModelVisibility>('shown');
  const [members, setMembers] = useState<string[]>([]);
  const [traversal, setTraversal] = useState<string>('round_robin');
  const [loadedOnly, setLoadedOnly] = useState(false);
  const [memberOrder, setMemberOrder] = useState<ModelGroupMemberOrder>('priority');
  // Left blank means "not set" (omit from the request so the server's own
  // default/unchanged-value handling applies); a typed "0" is an explicit
  // margin of zero and IS sent.
  const [climbSpeedMargin, setClimbSpeedMargin] = useState('');
  const [minTokensPerSecond, setMinTokensPerSecond] = useState('');
  const [minSpeedFallback, setMinSpeedFallback] = useState<ModelGroupMinSpeedFallback>('error');

  function openCreate() {
    setGatewayName('');
    setDisplayName('');
    setStatus('active');
    setFailoverMode('sticky');
    setVisibility('shown');
    setMembers([]);
    setTraversal('round_robin');
    setLoadedOnly(false);
    setMemberOrder('priority');
    setClimbSpeedMargin('');
    setMinTokensPerSecond('');
    setMinSpeedFallback('error');
    setMode('create');
  }

  function openEdit(row: PortalModelGroup) {
    setGatewayName(row.gateway_model_name);
    setDisplayName(row.display_name);
    setStatus(row.status);
    setFailoverMode(row.failover_mode === 'climb_up' ? 'climb_up' : 'sticky');
    setVisibility(row.visibility ?? 'shown');
    setMembers(row.members.map((m) => m.member_gateway_name));
    setTraversal(row.traversal || 'round_robin');
    setLoadedOnly(row.loaded_only);
    setMemberOrder(row.member_order === 'speed' ? 'speed' : 'priority');
    setClimbSpeedMargin(String(row.climb_speed_margin_percent));
    setMinTokensPerSecond(row.min_tokens_per_second ? String(row.min_tokens_per_second) : '');
    setMinSpeedFallback(row.min_speed_fallback === 'ignore' ? 'ignore' : 'error');
    setMode({ edit: row });
  }

  function reportError(err: unknown) {
    if (err instanceof PortalApiError && err.code === 'model_group.name_conflict') {
      showError(t.modelGroupNameConflict);
      return;
    }
    if (err instanceof PortalApiError && err.code === 'model_group.cycle') {
      showError(t.modelGroupCycleError);
      return;
    }
    showError(formatPortalError(err, t));
  }

  async function submit(event: SubmitEvent<HTMLFormElement>) {
    event.preventDefault();
    setBusy(true);
    try {
      const memberInputs = members.map((name) => ({ member_gateway_name: name }));
      const marginTrimmed = climbSpeedMargin.trim();
      const marginParsed = Number(marginTrimmed);
      // Only include the margin when the operator actually set it to a valid
      // number, so leaving it blank never overrides the server's
      // default/current value with 0.
      const climbSpeedMarginPercent =
        marginTrimmed === '' || Number.isNaN(marginParsed) ? undefined : Math.max(0, marginParsed);
      const selectionFields = {
        loaded_only: loadedOnly,
        member_order: memberOrder,
        min_tokens_per_second: nonNegativeNumber(minTokensPerSecond),
        min_speed_fallback: minSpeedFallback,
        ...(climbSpeedMarginPercent !== undefined
          ? { climb_speed_margin_percent: climbSpeedMarginPercent }
          : {}),
      };
      if (mode === 'create') {
        const body: CreateModelGroupRequest = {
          gateway_model_name: gatewayName,
          display_name: displayName,
          status,
          failover_mode: failoverMode,
          visibility,
          members: memberInputs,
          traversal,
          ...selectionFields,
        };
        const created = await api.createModelGroup(body);
        setData((current) => [created, ...(current ?? [])]);
      } else if (mode !== 'list') {
        const body: UpdateModelGroupRequest = {
          gateway_model_name: gatewayName,
          display_name: displayName,
          status,
          failover_mode: failoverMode,
          visibility,
          members: memberInputs,
          traversal,
          ...selectionFields,
        };
        const updated = await api.updateModelGroup(mode.edit.id, body);
        setData((current) => (current ?? []).map((row) => (row.id === updated.id ? updated : row)));
      }
      setMode('list');
      onModelsChanged?.();
    } catch (err) {
      reportError(err);
    } finally {
      setBusy(false);
    }
  }

  async function toggleStatus(row: PortalModelGroup) {
    const nextStatus: ApplicationStatus = row.status === 'active' ? 'disabled' : 'active';
    try {
      const updated = await api.updateModelGroup(row.id, { status: nextStatus });
      setData((current) =>
        (current ?? []).map((item) => (item.id === updated.id ? updated : item)),
      );
      onModelsChanged?.();
    } catch (err) {
      reportError(err);
    }
  }

  async function removeGroup(id: string) {
    try {
      await api.deleteModelGroup(id);
      setData((current) => (current ?? []).filter((row) => row.id !== id));
      setConfirmingDeleteId('');
      onModelsChanged?.();
    } catch (err) {
      reportError(err);
    }
  }

  const modeLabel = (m: string) =>
    m === 'climb_up' ? t.modelGroupModeClimb : t.modelGroupModeSticky;
  const traversalLabel = (v: string) => {
    switch (v) {
      case 'depth':
        return t.modelGroupTraversalDepth;
      case 'breadth':
        return t.modelGroupTraversalBreadth;
      default:
        return t.modelGroupTraversalRoundRobin;
    }
  };

  const columns: ListColumn<PortalModelGroup>[] = [
    {
      id: 'gateway',
      label: t.modelGroupGatewayName,
      value: (g) => g.gateway_model_name,
      filter: 'text',
    },
    {
      id: 'members',
      label: t.modelGroupMemberCount,
      value: (g) => String(g.members.length),
      numeric: true,
    },
    {
      id: 'mode',
      label: t.modelGroupMode,
      value: (g) => (g.failover_mode === 'climb_up' ? 'climb_up' : 'sticky'),
      filter: 'enum',
      searchable: false,
      enumLabel: (v) => modeLabel(v),
      render: (g) => modeLabel(g.failover_mode),
    },
    {
      id: 'status',
      label: t.tableStatus,
      value: (g) => g.status,
      filter: 'enum',
      searchable: false,
      enumLabel: (v) => t[applicationStatusLabelByKey[v as ApplicationStatus] ?? 'statusActive'],
      render: (g) => (
        <StatusChip status={g.status} label={t[applicationStatusLabelByKey[g.status]]} />
      ),
    },
  ];

  const rowActions = (row: PortalModelGroup): RowAction[] => [
    {
      key: 'edit',
      label: t.modelGroupEdit,
      icon: <EditIcon fontSize="small" />,
      onClick: () => openEdit(row),
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
      label: t.modelGroupDelete,
      color: 'error',
      icon: <DeleteIcon fontSize="small" />,
      onClick: () => setConfirmingDeleteId(row.id),
    },
  ];

  if (mode !== 'list') {
    const editing = mode !== 'create';
    return (
      <>
        <Breadcrumbs
          ariaLabel={t.breadcrumb}
          backLabel={t.back}
          items={[
            { label: t.modelGroups, onClick: () => setMode('list') },
            { label: editing ? t.modelGroupEditTitle : t.modelGroupCreate },
          ]}
        />
        <Panel
          titleId="model-group-form-heading"
          title={editing ? t.modelGroupEditTitle : t.modelGroupCreate}
        >
          <Box
            component="form"
            onSubmit={submit}
            sx={{ display: 'grid', gridTemplateColumns: 'minmax(260px, 480px)', gap: 2.25 }}
          >
            <Field
              id="model-group-gateway-name"
              label={t.modelGroupGatewayName}
              value={gatewayName}
              onChange={(e) => setGatewayName(e.target.value)}
              required
            />
            <Field
              id="model-group-display-name"
              label={t.modelGroupDisplayName}
              value={displayName}
              onChange={(e) => setDisplayName(e.target.value)}
            />
            <SelectField
              id="model-group-status"
              label={t.modelGroupStatus}
              value={status}
              onChange={(e) => setStatus(e.target.value as ApplicationStatus)}
            >
              {applicationStatusOptions.map((s) => (
                <option value={s} key={s}>
                  {t[applicationStatusLabelByKey[s]]}
                </option>
              ))}
            </SelectField>
            <Box sx={{ display: 'grid', gap: 0.5 }}>
              <SelectField
                id="model-group-mode"
                label={t.modelGroupMode}
                value={failoverMode}
                onChange={(e) => setFailoverMode(e.target.value)}
              >
                {failoverModes.map((m) => (
                  <option value={m} key={m}>
                    {m === 'climb_up' ? t.modelGroupModeClimb : t.modelGroupModeSticky}
                  </option>
                ))}
              </SelectField>
              <Typography variant="caption" color="text.secondary">
                {t.modelGroupModeHelp}
              </Typography>
            </Box>
            <Box sx={{ display: 'grid', gap: 0.5 }}>
              <SelectField
                id="model-group-traversal"
                label={t.modelGroupTraversal}
                value={traversal}
                onChange={(e) => setTraversal(e.target.value)}
              >
                {traversalModes.map((v) => (
                  <option value={v} key={v}>
                    {traversalLabel(v)}
                  </option>
                ))}
              </SelectField>
              <Typography variant="caption" color="text.secondary">
                {t.modelGroupTraversalHelp}
              </Typography>
            </Box>
            <Box sx={{ display: 'grid', gap: 0.5 }}>
              <SelectField
                id="model-group-visibility"
                label={t.modelVisibility}
                value={visibility}
                onChange={(e) => setVisibility(e.target.value as ModelVisibility)}
              >
                <option value="shown">{t.modelVisibilityShown}</option>
                <option value="hidden">{t.modelVisibilityHidden}</option>
                <option value="locked">{t.modelVisibilityLocked}</option>
              </SelectField>
              <Typography variant="caption" color="text.secondary">
                {t.modelVisibilityHelp}
              </Typography>
            </Box>
            <FormControlLabel
              control={
                <Checkbox checked={loadedOnly} onChange={(e) => setLoadedOnly(e.target.checked)} />
              }
              label={t.modelGroupLoadedOnly}
            />
            <SelectField
              id="model-group-member-order"
              label={t.modelGroupMemberOrder}
              value={memberOrder}
              onChange={(e) => setMemberOrder(e.target.value as ModelGroupMemberOrder)}
            >
              {memberOrders.map((v) => (
                <option value={v} key={v}>
                  {v === 'speed' ? t.modelGroupMemberOrderSpeed : t.modelGroupMemberOrderPriority}
                </option>
              ))}
            </SelectField>
            <Field
              id="model-group-climb-speed-margin"
              type="number"
              label={t.modelGroupClimbSpeedMargin}
              value={climbSpeedMargin}
              onChange={(e) => setClimbSpeedMargin(e.target.value)}
              inputProps={{ min: 0, step: 1 }}
            />
            <Field
              id="model-group-min-tokens-per-second"
              type="number"
              label={t.modelGroupMinTokensPerSecond}
              value={minTokensPerSecond}
              onChange={(e) => setMinTokensPerSecond(e.target.value)}
              inputProps={{ min: 0, step: 'any' }}
            />
            <SelectField
              id="model-group-min-speed-fallback"
              label={t.modelGroupMinSpeedFallback}
              value={minSpeedFallback}
              onChange={(e) => setMinSpeedFallback(e.target.value as ModelGroupMinSpeedFallback)}
            >
              {minSpeedFallbacks.map((v) => (
                <option value={v} key={v}>
                  {v === 'ignore'
                    ? t.modelGroupMinSpeedFallbackIgnore
                    : t.modelGroupMinSpeedFallbackError}
                </option>
              ))}
            </SelectField>
            <OrderedMemberList
              members={members}
              onChange={setMembers}
              available={models}
              t={t}
              disabled={busy}
              selfName={mode !== 'create' ? mode.edit.gateway_model_name : gatewayName}
            />
            <Box sx={{ display: 'flex', gap: 1.5 }}>
              <Button type="submit" variant="contained" disabled={busy}>
                {editing ? t.modelGroupSave : t.modelGroupCreate}
              </Button>
              <Button
                type="button"
                variant="text"
                color="secondary"
                onClick={() => setMode('list')}
              >
                {t.modelGroupCancel}
              </Button>
            </Box>
          </Box>
        </Panel>
      </>
    );
  }

  return (
    <Panel
      titleId="model-groups-heading"
      title={t.modelGroups}
      actions={
        <RowActions>
          <Button
            variant="contained"
            size="small"
            startIcon={<AddIcon />}
            type="button"
            onClick={openCreate}
          >
            {t.modelGroupCreate}
          </Button>
        </RowActions>
      }
    >
      <ListTable
        rows={groups}
        columns={columns}
        rowKey={(g) => g.id}
        actions={rowActions}
        storageKey="op.model-groups"
        labels={listTableLabels(t, { empty: t.modelGroupEmpty })}
        loading={loading || data === null}
      />

      <ConfirmDialog
        open={Boolean(confirmingDeleteId)}
        title={t.modelGroupDeleteConfirm}
        confirmLabel={t.modelGroupDelete}
        cancelLabel={t.modelGroupCancel}
        onConfirm={() => removeGroup(confirmingDeleteId)}
        onCancel={() => setConfirmingDeleteId('')}
      />
    </Panel>
  );
}
