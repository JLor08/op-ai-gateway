// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useEffect, useState, type SubmitEvent } from 'react';
import { Box, Button, IconButton, Typography } from '@mui/material';
import AddIcon from '@mui/icons-material/Add';
import DeleteIcon from '@mui/icons-material/Delete';
import BlockIcon from '@mui/icons-material/Block';
import CheckCircleIcon from '@mui/icons-material/CheckCircle';
import ListAltIcon from '@mui/icons-material/ListAlt';
import { EMPTY_LIMIT_CONFIG } from '../api';
import type {
  AdminGroupCandidate,
  AdminUser,
  LimitConfig,
  ModelOption,
  PortalService,
  ServiceDelegate,
} from '../api';
import type { Translation, PortalApi } from './shared/types';
import { formatPortalError } from './shared/format';
import { useResource } from './shared/useResource';
import { PageTitle } from './shared/PageTitle';
import { Panel } from './shared/Panel';
import { Field } from './shared/Field';
import { SelectField } from './shared/SelectField';
import { MultiSelectField } from './shared/MultiSelectField';
import { SearchableSelect } from './shared/SearchableSelect';
import { StatusChip } from './shared/StatusChip';
import { Breadcrumbs } from './shared/Breadcrumbs';
import { ConfirmDialog } from './shared/ConfirmDialog';
import { ListTable, listTableLabels, type ListColumn } from './shared/ListTable';
import type { RowAction } from './shared/RowActionsMenu';
import { LimitsEditor, limitConfigHasPairMismatch } from './shared/LimitsEditor';
import { useToast } from './shared/ToastProvider';
import { AdminGroupPicker } from './shared/AdminGroupPicker';
import { AdminGroupsEditor } from './shared/AdminGroupsEditor';
import {
  candidatesUnderSystemGroup,
  distinctParentGroups,
  editAdminGroupOptions,
} from './shared/adminGroupLinkage';
import { ServiceTokensSection } from './ServiceTokensSection';

type Mode = 'list' | 'create' | { kind: 'detail'; service: PortalService };

// Service<->admin-group linkage (Phase C, spec 2026-08-10). The
// distinctParentGroups/candidatesUnderSystemGroup/editAdminGroupOptions
// helpers are shared with ServerList.tsx/ResourceGroupsView.tsx via
// ./shared/adminGroupLinkage (FV-1); see that module for their docs.

function delegatesText(svc: PortalService): string {
  return svc.delegates.map((d) => d.user_name || d.user_id).join(', ');
}

export function ServicesView({
  t,
  api,
  models,
  role,
  userId,
}: Readonly<{
  t: Translation;
  api: Pick<
    PortalApi,
    | 'adminUsers'
    | 'createService'
    | 'createServiceToken'
    | 'deleteService'
    | 'deleteServiceToken'
    | 'rotateServiceToken'
    | 'serviceAdminGroupCandidates'
    | 'serviceTokens'
    | 'services'
    | 'setServiceAdminGroups'
    | 'updateService'
  >;
  models: ModelOption[];
  role: string;
  userId: string;
}>) {
  const isAdmin = role === 'admin' || role === 'system_admin';
  const { showError, showSuccess } = useToast();
  const {
    data: servicesData,
    setData: setServicesData,
    loading,
    error,
  } = useResource(() => api.services().then((r) => r.data), [api, t], t, { trackLoading: false });
  useEffect(() => {
    if (error) showError(error);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [error]);
  const services = servicesData ?? [];

  const [mode, setMode] = useState<Mode>('list');
  const [busy, setBusy] = useState(false);
  const [confirmingDeleteId, setConfirmingDeleteId] = useState('');

  // Settings form state (shared by create + the detail view's settings panel).
  const [name, setName] = useState('');
  const [description, setDescription] = useState('');
  const [status, setStatus] = useState('active');
  const [allowedModels, setAllowedModels] = useState<string[]>([]);
  const [delegates, setDelegates] = useState<ServiceDelegate[]>([]);
  const [limits, setLimits] = useState<LimitConfig>(EMPTY_LIMIT_CONFIG);

  // Candidate users for the "add delegate" picker. Only fetchable by an admin
  // (the endpoint is admin-scoped); a non-admin full delegate degrades to an
  // empty list (no crash — the add-picker simply has nothing to offer, but they
  // can still reassign/remove EXISTING delegates and edit every other field).
  const [adminUsers, setAdminUsers] = useState<AdminUser[]>([]);

  // Per-service delegate level for the current user (admin => always full).
  function delegateLevel(svc: PortalService): ServiceDelegate | undefined {
    return svc.delegates.find((d) => d.user_id === userId);
  }
  function canEditSettings(svc: PortalService): boolean {
    return isAdmin || !!delegateLevel(svc)?.can_manage_settings;
  }

  // Fetch the admin-user directory whenever it could actually be used (create —
  // always admin-only — or an editable detail view). On error it stays empty;
  // the add-delegate picker just has nothing to offer.
  useEffect(() => {
    const needsCandidates =
      mode === 'create' ||
      (typeof mode !== 'string' && mode.kind === 'detail' && canEditSettings(mode.service));
    if (!needsCandidates) return;
    let cancelled = false;
    api
      .adminUsers()
      .then((r) => {
        if (!cancelled) setAdminUsers(r.data);
      })
      .catch(() => {
        if (!cancelled) setAdminUsers([]);
      });
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [api, mode]);

  // Admin-group linkage (Phase C, spec 2026-08-10): the admin-tier groups the
  // caller may create/link a service into. Fetched once for ANY authenticated
  // user (NOT isAdmin-gated, unlike ServerList's analogous effect): the
  // create form is admin-only via the page-level button, but the edit-form
  // linkage editor is ALSO reachable by a non-admin Full-Delegate
  // (canEditSettings), and the candidates endpoint itself is the authority --
  // it returns the caller's own manageable groups (empty for the
  // unprivileged, populated for an admin/system OR a can_manage_services
  // co-manager), so gating the fetch here would just hand a Full-Delegate an
  // empty picker despite them having real candidates.
  const [serviceGroupCandidates, setServiceGroupCandidates] = useState<AdminGroupCandidate[]>([]);
  // Create-form picker state: the chosen system (parent) group -- meaningful
  // only when the candidates span more than one -- and the chosen admin-group
  // id(s) (only meaningful when there is more than one candidate under the
  // effective system group; a single candidate auto-selects, see below).
  const [createSystemGroupId, setCreateSystemGroupId] = useState('');
  const [createAdminGroupIds, setCreateAdminGroupIds] = useState<string[]>([]);
  // Edit-form admin-groups editor state: the service's linked group ids, saved
  // via its own button (mirrors ServerList's saveAdminGroups).
  const [editAdminGroupIds, setEditAdminGroupIds] = useState<string[]>([]);
  // Edit-form system-group choice, used ONLY when editing a service that has
  // no containment root yet (system_group_id==""; pre-Phase-C/migrated
  // services) — mirrors createSystemGroupId so the edit picker can offer the
  // same choose-a-system-group step create does. Ignored once a root is set.
  const [editSystemGroupId, setEditSystemGroupId] = useState('');
  const [adminGroupsBusy, setAdminGroupsBusy] = useState(false);

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const candidates = await api.serviceAdminGroupCandidates();
        if (!cancelled) setServiceGroupCandidates(candidates);
      } catch {
        // create form / admin-groups editor degrade to "no candidates" (the
        // service list itself still works)
        if (!cancelled) setServiceGroupCandidates([]);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [api]);

  // Create-form admin-group derivation: the system-group step is shown ONLY
  // when the candidates span more than one distinct parent; the admin-group
  // picker then narrows to that effective system group's children, and a
  // single remaining candidate auto-selects (no visible multi-select at all
  // -- mirrors GroupsView's parent-derivation / ServerList's create picker).
  const createDistinctSystemGroups = distinctParentGroups(serviceGroupCandidates);
  const createEffectiveSystemGroupId =
    createDistinctSystemGroups.length === 1
      ? createDistinctSystemGroups[0].id
      : createSystemGroupId;
  const createEffectiveCandidates = candidatesUnderSystemGroup(
    serviceGroupCandidates,
    createDistinctSystemGroups,
    createEffectiveSystemGroupId,
  );
  const createEffectiveAdminGroupIds =
    createEffectiveCandidates.length === 1
      ? [createEffectiveCandidates[0].id]
      : createAdminGroupIds;

  function openCreate() {
    setName('');
    setDescription('');
    setStatus('active');
    setAllowedModels([]);
    setDelegates([]);
    setLimits(EMPTY_LIMIT_CONFIG);
    setCreateSystemGroupId('');
    setCreateAdminGroupIds([]);
    setMode('create');
  }

  function openDetail(svc: PortalService) {
    setName(svc.name);
    setDescription(svc.description);
    setStatus(svc.status);
    setAllowedModels(svc.allowed_models);
    setDelegates(svc.delegates.map((d) => ({ ...d })));
    setLimits(svc.limits ?? EMPTY_LIMIT_CONFIG);
    setEditAdminGroupIds(svc.admin_groups.map((g) => g.id));
    setEditSystemGroupId('');
    setMode({ kind: 'detail', service: svc });
  }

  async function submitCreate(event: SubmitEvent<HTMLFormElement>) {
    event.preventDefault();
    // Defensive guard mirroring the submit button's disabled state (Phase C,
    // mirrors ServerList's submitCreate): the backend rejects an empty
    // admin_group_ids set outright, but never even attempt the call when the
    // picker hasn't resolved one yet.
    if (createEffectiveAdminGroupIds.length === 0) return;
    setBusy(true);
    try {
      const created = await api.createService({
        name,
        description,
        status,
        delegates: delegates.map((d) => ({
          user_id: d.user_id,
          can_manage_settings: d.can_manage_settings,
        })),
        allowed_models: allowedModels,
        limits,
        admin_group_ids: createEffectiveAdminGroupIds,
      });
      setServicesData((current) => [created, ...(current ?? [])]);
      setMode('list');
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setBusy(false);
    }
  }

  // Edit-form admin-groups editor save (Phase C, spec 2026-08-10, mirrors
  // ServerList's saveAdminGroups exactly): a full-replace of the service's
  // linked admin-group set via its own endpoint, independent of the main
  // settings form's Save.
  async function saveAdminGroups(ids: string[]) {
    if (typeof mode === 'string' || mode.kind !== 'detail') return;
    setAdminGroupsBusy(true);
    try {
      const updated = await api.setServiceAdminGroups(mode.service.id, ids);
      setServicesData((current) => (current ?? []).map((s) => (s.id === updated.id ? updated : s)));
      setMode({ kind: 'detail', service: updated });
      setEditAdminGroupIds(updated.admin_groups.map((g) => g.id));
      showSuccess(t.serviceAdminGroupsSaved);
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setAdminGroupsBusy(false);
    }
  }

  async function saveSettings() {
    if (typeof mode === 'string' || mode.kind !== 'detail') return;
    setBusy(true);
    try {
      const updated = await api.updateService(mode.service.id, {
        name,
        description,
        status,
        delegates: delegates.map((d) => ({
          user_id: d.user_id,
          can_manage_settings: d.can_manage_settings,
        })),
        allowed_models: allowedModels,
        limits,
      });
      setServicesData((current) => (current ?? []).map((s) => (s.id === updated.id ? updated : s)));
      setMode({ kind: 'detail', service: updated });
      setName(updated.name);
      setDescription(updated.description);
      setStatus(updated.status);
      setAllowedModels(updated.allowed_models);
      setDelegates(updated.delegates.map((d) => ({ ...d })));
      setLimits(updated.limits ?? EMPTY_LIMIT_CONFIG);
      showSuccess(t.save);
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setBusy(false);
    }
  }

  async function toggleStatus(svc: PortalService) {
    const nextStatus = svc.status === 'active' ? 'disabled' : 'active';
    try {
      const updated = await api.updateService(svc.id, { status: nextStatus });
      setServicesData((current) => (current ?? []).map((s) => (s.id === updated.id ? updated : s)));
    } catch (err) {
      showError(formatPortalError(err, t));
    }
  }

  async function removeService(id: string) {
    try {
      await api.deleteService(id);
      setServicesData((current) => (current ?? []).filter((s) => s.id !== id));
      setConfirmingDeleteId('');
      if (typeof mode !== 'string' && mode.kind === 'detail' && mode.service.id === id)
        setMode('list');
    } catch (err) {
      showError(formatPortalError(err, t));
    }
  }

  const columns: ListColumn<PortalService>[] = [
    { id: 'name', label: t.tableName, value: (s) => s.name, filter: 'text' },
    {
      id: 'status',
      label: t.tableStatus,
      value: (s) => s.status,
      filter: 'enum',
      searchable: false,
      enumLabel: (v) => (v === 'disabled' ? t.statusDisabled : t.statusActive),
      render: (s) => (
        <StatusChip
          status={s.status}
          label={s.status === 'disabled' ? t.statusDisabled : t.statusActive}
        />
      ),
    },
    {
      id: 'tokenCount',
      label: t.serviceColTokenCount,
      value: (s) => String(s.token_count),
      numeric: true,
    },
    {
      id: 'delegates',
      label: t.serviceColDelegates,
      value: (s) => delegatesText(s),
      filter: 'text',
      render: (s) => delegatesText(s) || '-',
    },
  ];

  const rowActions = (svc: PortalService): RowAction[] => {
    const actions: RowAction[] = [
      {
        key: 'open',
        label: t.modelDetailsAction,
        icon: <ListAltIcon fontSize="small" />,
        onClick: () => openDetail(svc),
      },
    ];
    if (canEditSettings(svc)) {
      actions.push(
        {
          key: 'toggle',
          label: svc.status === 'active' ? t.tokenActionDisable : t.tokenActionEnable,
          icon:
            svc.status === 'active' ? (
              <BlockIcon fontSize="small" />
            ) : (
              <CheckCircleIcon fontSize="small" />
            ),
          onClick: () => void toggleStatus(svc),
        },
        {
          key: 'delete',
          label: t.serviceActionDelete,
          color: 'error',
          icon: <DeleteIcon fontSize="small" />,
          onClick: () => setConfirmingDeleteId(svc.id),
        },
      );
    }
    return actions;
  };

  const listLabels = listTableLabels(t);

  // Create sub-view (admin only — the list gates the "Neu" action, but this
  // guards a direct mode transition too).
  if (mode === 'create') {
    return (
      <>
        <Breadcrumbs
          ariaLabel={t.breadcrumb}
          backLabel={t.back}
          items={[
            { label: t.services, onClick: () => setMode('list') },
            { label: t.serviceCreate },
          ]}
        />
        <Panel titleId="service-form-heading" title={t.serviceCreate}>
          <Box
            component="form"
            onSubmit={submitCreate}
            sx={{ display: 'grid', gridTemplateColumns: 'minmax(260px, 480px)', gap: 2.25 }}
          >
            <Field
              id="service-name"
              label={t.serviceNameLabel}
              value={name}
              onChange={(e) => setName(e.target.value)}
              required
            />
            <Field
              id="service-description"
              label={t.serviceDescriptionLabel}
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              multiline
              minRows={2}
            />
            {/* Service<->admin-group linkage (Phase C, spec 2026-08-10): >=1
                admin group required for EVERY caller, including
                system_admin. Exactly one candidate -> auto-selected (no
                field); several -> a required multi-select, narrowed to one
                system group first when the candidates span more than one;
                none -> a hint + the create action stays disabled (mirrors
                ServerList's create picker exactly). */}
            <AdminGroupPicker
              idPrefix="service"
              candidates={serviceGroupCandidates}
              systemGroupId={createSystemGroupId}
              onSystemGroupIdChange={setCreateSystemGroupId}
              adminGroupIds={createAdminGroupIds}
              onAdminGroupIdsChange={setCreateAdminGroupIds}
              labels={{
                noCandidatesHint: t.serviceNoAdminGroupHint,
                systemGroupLabel: t.serviceAdminGroupSystemGroupLabel,
                systemGroupAuto: t.serviceAdminGroupSystemGroupAuto,
                adminGroupLabel: t.serviceAdminGroupLabel,
                adminGroupAuto: t.serviceAdminGroupAuto,
              }}
            />
            <SelectField
              id="service-status"
              label={t.serviceStatusLabel}
              value={status}
              onChange={(e) => setStatus(e.target.value)}
            >
              <option value="active">{t.statusActive}</option>
              <option value="disabled">{t.statusDisabled}</option>
            </SelectField>
            <Box>
              <MultiSelectField
                id="service-allowed-models"
                label={t.serviceAllowedModelsLabel}
                placeholder={t.listSearchPlaceholder}
                options={models.map((m) => ({ value: m.id, label: m.display_name }))}
                selected={allowedModels}
                onChange={setAllowedModels}
              />
              <Typography variant="caption" color="text.secondary">
                {t.serviceAllowedModelsHelp}
              </Typography>
            </Box>
            <DelegatesEditor
              t={t}
              delegates={delegates}
              onChange={setDelegates}
              candidates={adminUsers}
              readOnly={false}
            />
            <LimitsEditor t={t} idPrefix="service-create" value={limits} onChange={setLimits} />
            <Box sx={{ display: 'flex', gap: 1.5 }}>
              <Button
                type="submit"
                variant="contained"
                disabled={
                  busy ||
                  limitConfigHasPairMismatch(limits) ||
                  createEffectiveAdminGroupIds.length === 0
                }
              >
                {t.serviceCreate}
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

  // Detail sub-view: settings (editable for admin/full-delegate, read-only for a
  // token-delegate) + token management (available to any delegate/admin).
  if (typeof mode !== 'string' && mode.kind === 'detail') {
    const svc = services.find((s) => s.id === mode.service.id) ?? mode.service;
    const editable = canEditSettings(svc);
    return (
      <>
        <Breadcrumbs
          ariaLabel={t.breadcrumb}
          backLabel={t.back}
          items={[{ label: t.services, onClick: () => setMode('list') }, { label: svc.name }]}
        />
        <Panel
          titleId="service-settings-heading"
          title={t.serviceSettingsTitle}
          subtitle={editable ? undefined : t.serviceSettingsReadOnlyNote}
        >
          <Box
            component="form"
            onSubmit={(e) => {
              e.preventDefault();
              void saveSettings();
            }}
            sx={{ display: 'grid', gridTemplateColumns: 'minmax(260px, 480px)', gap: 2.25 }}
          >
            <Field
              id="service-detail-name"
              label={t.serviceNameLabel}
              value={name}
              onChange={(e) => setName(e.target.value)}
              required
              disabled={!editable}
            />
            <Field
              id="service-detail-description"
              label={t.serviceDescriptionLabel}
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              multiline
              minRows={2}
              disabled={!editable}
            />
            <SelectField
              id="service-detail-status"
              label={t.serviceStatusLabel}
              value={status}
              onChange={(e) => setStatus(e.target.value)}
              disabled={!editable}
            >
              <option value="active">{t.statusActive}</option>
              <option value="disabled">{t.statusDisabled}</option>
            </SelectField>
            <Box>
              <MultiSelectField
                id="service-detail-allowed-models"
                label={t.serviceAllowedModelsLabel}
                placeholder={t.listSearchPlaceholder}
                options={models.map((m) => ({ value: m.id, label: m.display_name }))}
                selected={allowedModels}
                onChange={setAllowedModels}
                disabled={!editable}
              />
              <Typography variant="caption" color="text.secondary">
                {t.serviceAllowedModelsHelp}
              </Typography>
            </Box>
            <DelegatesEditor
              t={t}
              delegates={delegates}
              onChange={setDelegates}
              candidates={adminUsers}
              readOnly={!editable}
            />
            <LimitsEditor
              t={t}
              idPrefix="service-detail"
              value={limits}
              onChange={setLimits}
              usage={svc.limits_usage}
              disabled={!editable}
            />
            {editable && (
              <Box sx={{ display: 'flex', gap: 1.5 }}>
                <Button
                  type="submit"
                  variant="contained"
                  disabled={busy || limitConfigHasPairMismatch(limits)}
                >
                  {t.save}
                </Button>
              </Box>
            )}
          </Box>
        </Panel>

        {/* Admin-groups editor (Phase C, spec 2026-08-10): add/remove the
            service's linked admin-tier groups. For a service that ALREADY
            has a containment root, narrowed to that root (system_group_id)'s
            children. For a service with NO root yet (pre-Phase-C/migrated:
            system_group_id==""), the create-style choose-a-system-group flow
            so the root can be SET. >=1 group must remain (the backend
            rejects an empty set; the Save button mirrors that). Gated on
            canEditSettings(svc) -- admin/system_admin OR a per-service
            Full-Delegate (can_manage_settings) -- mirroring the SAME gate
            the rest of THIS form uses (`editable` above) and the backend's
            authorizeServiceSettings, which permits exactly that set to call
            SetServiceAdminGroups. Unlike ServerList (whose owners never see
            the analogous section), ServicesView already exposes settings
            editing to non-admin Full-Delegates, and the design spec's
            ungrouped-recovery path explicitly wants a Full-Delegate to be
            able to assign an ungrouped service's admin groups -- so this
            must NOT be isAdmin-only. */}
        {editable && (
          <Box sx={{ mt: 3 }}>
            <Panel titleId="service-admin-groups-heading" title={t.serviceAdminGroupsSectionTitle}>
              <AdminGroupsEditor
                idPrefix="service"
                candidates={serviceGroupCandidates}
                fixedOptions={editAdminGroupOptions(serviceGroupCandidates, svc, (s) => ({
                  id: s.system_group_id,
                  name: s.system_group_name,
                }))}
                adminGroupIds={editAdminGroupIds}
                onAdminGroupIdsChange={setEditAdminGroupIds}
                busy={adminGroupsBusy}
                onSave={saveAdminGroups}
                labels={{
                  noCandidatesHint: t.serviceNoAdminGroupHint,
                  systemGroupLabel: t.serviceAdminGroupSystemGroupLabel,
                  systemGroupAuto: t.serviceAdminGroupSystemGroupAuto,
                  adminGroupLabel: t.serviceAdminGroupLabel,
                  adminGroupAuto: t.serviceAdminGroupAuto,
                  saveLabel: t.serviceAdminGroupsSave,
                }}
                ungrouped={{
                  isUngrouped: svc.system_group_id === '',
                  systemGroupId: editSystemGroupId,
                  onSystemGroupIdChange: setEditSystemGroupId,
                }}
              />
            </Panel>
          </Box>
        )}

        <ServiceTokensSection
          t={t}
          api={api}
          service={svc}
          models={models}
          onTokenCountChanged={(delta) =>
            setServicesData((current) =>
              (current ?? []).map((s) =>
                s.id === svc.id ? { ...s, token_count: Math.max(0, s.token_count + delta) } : s,
              ),
            )
          }
        />

        <ConfirmDialog
          open={confirmingDeleteId !== ''}
          title={t.serviceDeleteConfirm}
          confirmLabel={t.serviceActionDelete}
          cancelLabel={t.cancel}
          onConfirm={() => void removeService(confirmingDeleteId)}
          onCancel={() => setConfirmingDeleteId('')}
        />
      </>
    );
  }

  // List view.
  return (
    <>
      <PageTitle
        title={t.services}
        subtitle={t.servicesIntro}
        action={
          isAdmin ? (
            <Button variant="contained" startIcon={<AddIcon />} onClick={openCreate}>
              {t.serviceCreate}
            </Button>
          ) : undefined
        }
      />
      <Panel titleId="services-list-heading" title={t.serviceListTitle}>
        <ListTable
          rows={services}
          columns={columns}
          rowKey={(s) => s.id}
          actions={rowActions}
          storageKey="op.services"
          labels={listLabels}
          loading={loading || servicesData === null}
        />
      </Panel>
      <ConfirmDialog
        open={confirmingDeleteId !== ''}
        title={t.serviceDeleteConfirm}
        confirmLabel={t.serviceActionDelete}
        cancelLabel={t.cancel}
        onConfirm={() => void removeService(confirmingDeleteId)}
        onCancel={() => setConfirmingDeleteId('')}
      />
    </>
  );
}

/**
 * Delegate picker rendered as two groups — "Voll-Delegierte" (can_manage_settings)
 * and "Token-Delegierte" — each entry movable between groups and removable.
 * Read-only mode (a token-delegate viewing settings) hides every control and
 * shows plain names. The "add" row (only when editable AND at least one
 * not-yet-added candidate exists) picks a user + a level and appends them.
 */
function DelegatesEditor({
  t,
  delegates,
  onChange,
  candidates,
  readOnly,
}: Readonly<{
  t: Translation;
  delegates: ServiceDelegate[];
  onChange: (next: ServiceDelegate[]) => void;
  candidates: AdminUser[];
  readOnly: boolean;
}>) {
  const [addUserId, setAddUserId] = useState('');
  const [addLevel, setAddLevel] = useState<'full' | 'token'>('token');
  const full = delegates.filter((d) => d.can_manage_settings);
  const tokenLevel = delegates.filter((d) => !d.can_manage_settings);
  const addedIds = new Set(delegates.map((d) => d.user_id));
  const available = candidates.filter((u) => !addedIds.has(u.id));

  function move(id: string, toFull: boolean) {
    onChange(delegates.map((d) => (d.user_id === id ? { ...d, can_manage_settings: toFull } : d)));
  }
  function remove(id: string) {
    onChange(delegates.filter((d) => d.user_id !== id));
  }
  function add() {
    const candidate = candidates.find((u) => u.id === addUserId);
    if (!candidate) return;
    onChange([
      ...delegates,
      {
        user_id: candidate.id,
        user_name: candidate.display_name || candidate.email,
        can_manage_settings: addLevel === 'full',
      },
    ]);
    setAddUserId('');
  }

  function group(label: string, help: string, rows: ServiceDelegate[], isFullGroup: boolean) {
    return (
      <Box sx={{ display: 'grid', gap: 0.75 }}>
        <Typography variant="subtitle2">{label}</Typography>
        <Typography variant="caption" color="text.secondary">
          {help}
        </Typography>
        {rows.length === 0 && (
          <Typography variant="body2" color="text.secondary">
            {t.serviceDelegatesEmpty}
          </Typography>
        )}
        {rows.map((d) => (
          <Box key={d.user_id} sx={{ display: 'flex', alignItems: 'center', gap: 1 }}>
            <Typography sx={{ flex: 1 }}>{d.user_name || d.user_id}</Typography>
            {!readOnly && (
              <>
                <Button
                  size="small"
                  variant="outlined"
                  type="button"
                  onClick={() => move(d.user_id, !isFullGroup)}
                >
                  {isFullGroup ? t.serviceDelegatesTokenGroup : t.serviceDelegatesFullGroup}
                </Button>
                <IconButton
                  aria-label={`${t.serviceDelegatesRemove} ${d.user_name || d.user_id}`}
                  onClick={() => remove(d.user_id)}
                >
                  <DeleteIcon fontSize="small" />
                </IconButton>
              </>
            )}
          </Box>
        ))}
      </Box>
    );
  }

  return (
    <Box sx={{ display: 'grid', gap: 2 }}>
      <Typography variant="subtitle2">{t.serviceDelegatesLabel}</Typography>
      {group(t.serviceDelegatesFullGroup, t.serviceDelegatesFullHelp, full, true)}
      {group(t.serviceDelegatesTokenGroup, t.serviceDelegatesTokenHelp, tokenLevel, false)}
      {!readOnly && available.length > 0 && (
        <Box sx={{ display: 'flex', gap: 1, alignItems: 'flex-end', flexWrap: 'wrap' }}>
          <Box sx={{ flex: 1, minWidth: 200 }}>
            <SearchableSelect
              id="service-delegate-add"
              label={t.serviceDelegatesAddLabel}
              value={addUserId}
              onChange={setAddUserId}
              options={available.map((u) => ({ value: u.id, label: u.display_name || u.email }))}
            />
          </Box>
          <SelectField
            id="service-delegate-add-level"
            label={t.serviceDelegatesLabel}
            value={addLevel}
            onChange={(e) => setAddLevel(e.target.value as 'full' | 'token')}
          >
            <option value="token">{t.serviceDelegatesTokenGroup}</option>
            <option value="full">{t.serviceDelegatesFullGroup}</option>
          </SelectField>
          <Button type="button" variant="outlined" onClick={add} disabled={!addUserId}>
            {t.serviceDelegatesAdd}
          </Button>
        </Box>
      )}
    </Box>
  );
}
