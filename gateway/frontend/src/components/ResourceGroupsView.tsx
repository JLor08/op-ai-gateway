// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useEffect, useState, type SubmitEvent } from 'react';
import {
  Box,
  Button,
  IconButton,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableRow,
  Typography,
} from '@mui/material';
import AddIcon from '@mui/icons-material/Add';
import DeleteIcon from '@mui/icons-material/Delete';
import ListAltIcon from '@mui/icons-material/ListAlt';
import type {
  AdminGroupCandidate,
  CreateResourceGroupRequest,
  ResourceGroup,
  ResourceGroupProvision,
  ResourceGroupProvisionCandidates,
  ResourceGroupProvisionKind,
  ResourceGroupServerRef,
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
import { useToast } from './shared/ToastProvider';
import { AdminGroupPicker } from './shared/AdminGroupPicker';
import { AdminGroupsEditor } from './shared/AdminGroupsEditor';
import {
  candidatesUnderSystemGroup,
  distinctParentGroups,
  editAdminGroupOptions,
} from './shared/adminGroupLinkage';

// Fixed display order for the provisioning editor's kind column/picker
// (Phase 2, spec 2026-08-12-resource-groups-phase-2-provisioning).
const PROVISION_KINDS: ResourceGroupProvisionKind[] = [
  'user',
  'user_group',
  'admin_group',
  'service',
];

const EMPTY_PROVISION_CANDIDATES: ResourceGroupProvisionCandidates = {
  users: [],
  user_groups: [],
  admin_groups: [],
  services: [],
};

function provisionKindLabel(t: Translation, kind: ResourceGroupProvisionKind): string {
  switch (kind) {
    case 'user':
      return t.resourceGroupProvisionKindUser;
    case 'user_group':
      return t.resourceGroupProvisionKindUserGroup;
    case 'admin_group':
      return t.resourceGroupProvisionKindAdminGroup;
    case 'service':
      return t.resourceGroupProvisionKindService;
    default:
      return kind;
  }
}

// The candidate targets for one kind, as plain {id,name} options -- users
// come back as UserRef (email/display_name), the other three kinds are
// already {id,name}.
function candidatesForKind(
  candidates: ResourceGroupProvisionCandidates,
  kind: ResourceGroupProvisionKind,
): { id: string; name: string }[] {
  switch (kind) {
    case 'user':
      return candidates.users.map((u) => ({ id: u.id, name: u.display_name || u.email }));
    case 'user_group':
      return candidates.user_groups;
    case 'admin_group':
      return candidates.admin_groups;
    case 'service':
      return candidates.services;
    default:
      return [];
  }
}

type Mode = 'list' | 'create' | { kind: 'detail'; group: ResourceGroup };

// Resource-group<->admin-group linkage (Resource Groups Phase 1, spec
// 2026-08-11). The distinctParentGroups/candidatesUnderSystemGroup/
// editAdminGroupOptions helpers are shared with ServicesView.tsx/
// ServerList.tsx via ./shared/adminGroupLinkage (FV-1); see that module for
// their docs. ResourceGroup nests its containment root as
// system_group.{id,name} rather than the flat system_group_id/_name fields
// PortalServer/PortalService use -- editAdminGroupOptions's accessor param
// handles that. A resource group ALWAYS has a root after create
// (CreateResourceGroup requires >=1 admin group), so -- unlike
// ServicesView/ServerList -- there is no "ungrouped-recovery" branch here
// (AdminGroupsEditor's `ungrouped` prop is simply omitted below).

/**
 * Resource Groups (Phase 1, spec 2026-08-11): a management structure that
 * bundles AI-servers under an admin-group linkage, so a server is managed
 * only by the co-managers of ITS resource group's linked admin groups --
 * managed like a server/service (create + admin-group linkage editor +
 * server-membership editor), minus any owner/delegate model of its own.
 * Reachable only by admin/system_admin (App.tsx's nav gate); the CREATE
 * action is additionally gated on `isAdmin` here (mirrors ServicesView/
 * ServerList), even though in practice the whole view is already
 * admin-gated one level up.
 */
export function ResourceGroupsView({
  t,
  api,
  role,
}: Readonly<{
  t: Translation;
  api: Pick<
    PortalApi,
    | 'createResourceGroup'
    | 'deleteResourceGroup'
    | 'resourceGroupAdminGroupCandidates'
    | 'resourceGroupProvisionCandidates'
    | 'resourceGroupProvisions'
    | 'resourceGroupServerCandidates'
    | 'resourceGroups'
    | 'setResourceGroupAdminGroups'
    | 'setResourceGroupProvisions'
    | 'setResourceGroupServers'
    | 'updateResourceGroup'
  >;
  role: string;
}>) {
  const isAdmin = role === 'admin' || role === 'system_admin';
  const { showError, showSuccess } = useToast();
  const {
    data: groupsData,
    setData: setGroupsData,
    loading,
    error,
  } = useResource(() => api.resourceGroups().then((r) => r.data), [api, t], t, {
    trackLoading: false,
  });
  useEffect(() => {
    if (error) showError(error);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [error]);
  const groups = groupsData ?? [];

  const [mode, setMode] = useState<Mode>('list');
  const [busy, setBusy] = useState(false);
  const [confirmingDeleteId, setConfirmingDeleteId] = useState('');

  // Settings form state (shared by create + the detail view's settings panel).
  const [name, setName] = useState('');
  const [status, setStatus] = useState('active');

  // Admin-group linkage (create picker + edit-form editor). Fetched once for
  // ANY viewer of this view (which is already admin/system_admin-gated one
  // level up) -- the candidates endpoint itself is the authority (empty for
  // a non-manager, populated for an owner/can_manage_resources co-manager or
  // system scope).
  const [groupCandidates, setGroupCandidates] = useState<AdminGroupCandidate[]>([]);
  const [createSystemGroupId, setCreateSystemGroupId] = useState('');
  const [createAdminGroupIds, setCreateAdminGroupIds] = useState<string[]>([]);
  const [editAdminGroupIds, setEditAdminGroupIds] = useState<string[]>([]);
  const [adminGroupsBusy, setAdminGroupsBusy] = useState(false);

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const candidates = await api.resourceGroupAdminGroupCandidates();
        if (!cancelled) setGroupCandidates(candidates);
      } catch {
        // create form / admin-groups editor degrade to "no candidates" (the
        // resource-group list itself still works)
        if (!cancelled) setGroupCandidates([]);
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
  // -- mirrors ServicesView/ServerList's create picker).
  const createDistinctSystemGroups = distinctParentGroups(groupCandidates);
  const createEffectiveSystemGroupId =
    createDistinctSystemGroups.length === 1
      ? createDistinctSystemGroups[0].id
      : createSystemGroupId;
  const createEffectiveCandidates = candidatesUnderSystemGroup(
    groupCandidates,
    createDistinctSystemGroups,
    createEffectiveSystemGroupId,
  );
  const createEffectiveAdminGroupIds =
    createEffectiveCandidates.length === 1
      ? [createEffectiveCandidates[0].id]
      : createAdminGroupIds;

  // Server-membership editor state (Task 5, spec 2026-08-11): the resource
  // group's manageable server candidates (already filtered server-side to
  // its own system group) + the currently-selected member ids.
  const [serverCandidates, setServerCandidates] = useState<ResourceGroupServerRef[]>([]);
  const [editServerIds, setEditServerIds] = useState<string[]>([]);
  const [serversBusy, setServersBusy] = useState(false);

  // Provisioning editor state (Phase 2, spec
  // 2026-08-12-resource-groups-phase-2-provisioning): the group's current
  // "provisioned for" set (users/user groups/admin groups/services) +
  // the caller's own candidate landscape for it. Edits are LOCAL until Save
  // (a full-replace PUT, mirroring the admin-groups/servers editors above).
  const [provisions, setProvisions] = useState<ResourceGroupProvision[]>([]);
  const [provisionCandidates, setProvisionCandidates] = useState<ResourceGroupProvisionCandidates>(
    EMPTY_PROVISION_CANDIDATES,
  );
  const [provisionsBusy, setProvisionsBusy] = useState(false);
  const [addProvisionKind, setAddProvisionKind] = useState<ResourceGroupProvisionKind>('user');
  const [addProvisionTargetId, setAddProvisionTargetId] = useState('');

  // Tokens for the currently open detail view. Keyed on the group id (not
  // the whole mode object) so a settings/admin-groups save -- which re-sets
  // mode with a fresh group DTO -- does not needlessly refetch candidates.
  const detailGroupId = typeof mode !== 'string' && mode.kind === 'detail' ? mode.group.id : null;
  useEffect(() => {
    if (!detailGroupId) {
      setServerCandidates([]);
      return;
    }
    let cancelled = false;
    api
      .resourceGroupServerCandidates(detailGroupId)
      .then((list) => {
        if (!cancelled) setServerCandidates(list);
      })
      .catch(() => {
        if (!cancelled) setServerCandidates([]);
      });
    return () => {
      cancelled = true;
    };
  }, [api, detailGroupId]);

  useEffect(() => {
    if (!detailGroupId) {
      setProvisions([]);
      setProvisionCandidates(EMPTY_PROVISION_CANDIDATES);
      return;
    }
    let cancelled = false;
    Promise.all([
      api.resourceGroupProvisions(detailGroupId),
      api.resourceGroupProvisionCandidates(detailGroupId),
    ])
      .then(([list, candidates]) => {
        if (cancelled) return;
        setProvisions(list);
        setProvisionCandidates(candidates);
      })
      .catch(() => {
        if (cancelled) return;
        // A load failure (e.g. a non-manager 404) degrades to an empty,
        // still-editable set -- consistent with the admin-groups/servers
        // editors' own load-failure handling.
        setProvisions([]);
        setProvisionCandidates(EMPTY_PROVISION_CANDIDATES);
      });
    return () => {
      cancelled = true;
    };
  }, [api, detailGroupId]);

  function openCreate() {
    setName('');
    setStatus('active');
    setCreateSystemGroupId('');
    setCreateAdminGroupIds([]);
    setMode('create');
  }

  function openDetail(group: ResourceGroup) {
    setName(group.name);
    setStatus(group.status);
    setEditAdminGroupIds(group.admin_groups.map((g) => g.id));
    setEditServerIds(group.servers.map((s) => s.id));
    setAddProvisionKind('user');
    setAddProvisionTargetId('');
    setMode({ kind: 'detail', group });
  }

  async function submitCreate(event: SubmitEvent<HTMLFormElement>) {
    event.preventDefault();
    // Defensive guard mirroring the submit button's disabled state: the
    // backend rejects an empty admin_group_ids set outright, but never even
    // attempt the call when the picker hasn't resolved one yet.
    if (createEffectiveAdminGroupIds.length === 0) return;
    setBusy(true);
    try {
      const created = await api.createResourceGroup({
        name,
        status,
        admin_group_ids: createEffectiveAdminGroupIds,
      } as CreateResourceGroupRequest);
      setGroupsData((current) => [created, ...(current ?? [])]);
      setMode('list');
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setBusy(false);
    }
  }

  async function saveSettings() {
    if (typeof mode === 'string' || mode.kind !== 'detail') return;
    setBusy(true);
    try {
      const updated = await api.updateResourceGroup(mode.group.id, { name, status });
      setGroupsData((current) => (current ?? []).map((g) => (g.id === updated.id ? updated : g)));
      setMode({ kind: 'detail', group: updated });
      showSuccess(t.save);
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setBusy(false);
    }
  }

  // Edit-form admin-groups editor save (fixed root -- mirrors ServicesView's
  // saveAdminGroups exactly): a full-replace of the group's linked
  // admin-group set via its own endpoint, independent of the settings
  // form's Save.
  async function saveAdminGroups(ids: string[]) {
    if (typeof mode === 'string' || mode.kind !== 'detail') return;
    setAdminGroupsBusy(true);
    try {
      const updated = await api.setResourceGroupAdminGroups(mode.group.id, ids);
      setGroupsData((current) => (current ?? []).map((g) => (g.id === updated.id ? updated : g)));
      setMode({ kind: 'detail', group: updated });
      setEditAdminGroupIds(updated.admin_groups.map((g) => g.id));
      showSuccess(t.resourceGroupAdminGroupsSaved);
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setAdminGroupsBusy(false);
    }
  }

  // Server-membership editor save (Task 5, spec 2026-08-11): a full-replace
  // of the group's member-server set via its own endpoint. A rejection
  // (unmanaged server / system-group mismatch) surfaces as a toast; the
  // selection is left untouched so the operator can adjust and retry.
  async function saveServers() {
    if (typeof mode === 'string' || mode.kind !== 'detail') return;
    setServersBusy(true);
    try {
      const updated = await api.setResourceGroupServers(mode.group.id, editServerIds);
      setGroupsData((current) => (current ?? []).map((g) => (g.id === updated.id ? updated : g)));
      setMode({ kind: 'detail', group: updated });
      setEditServerIds(updated.servers.map((s) => s.id));
      showSuccess(t.resourceGroupServersSaved);
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setServersBusy(false);
    }
  }

  // The add-flow's target options for the currently chosen kind, minus
  // whatever is already in the pending provisions list (so re-adding the
  // same target twice is impossible by construction).
  const addProvisionOptions = candidatesForKind(provisionCandidates, addProvisionKind).filter(
    (c) => !provisions.some((p) => p.kind === addProvisionKind && p.target_id === c.id),
  );

  // Adds the chosen target to the LOCAL pending list only -- committed to
  // the backend on Save (mirrors the admin-groups/servers editors' local
  // MultiSelectField edits).
  function addProvision() {
    const option = addProvisionOptions.find((c) => c.id === addProvisionTargetId);
    if (!option) return;
    setProvisions((current) => [
      ...current,
      { kind: addProvisionKind, target_id: option.id, target_name: option.name },
    ]);
    setAddProvisionTargetId('');
  }

  function removeProvision(kind: ResourceGroupProvisionKind, targetId: string) {
    setProvisions((current) =>
      current.filter((p) => !(p.kind === kind && p.target_id === targetId)),
    );
  }

  // Provisioning editor save: a full-replace of the group's "provisioned
  // for" set. A rejection (invalid/non-visible target) surfaces as a toast
  // and leaves the pending list untouched so the operator can adjust and
  // retry; a success re-fetches so the displayed target_name stays exact.
  async function saveProvisions() {
    if (typeof mode === 'string' || mode.kind !== 'detail') return;
    setProvisionsBusy(true);
    try {
      await api.setResourceGroupProvisions(
        mode.group.id,
        provisions.map((p) => ({ kind: p.kind, target_id: p.target_id })),
      );
      const refreshed = await api.resourceGroupProvisions(mode.group.id);
      setProvisions(refreshed);
      showSuccess(t.resourceGroupProvisionsSaved);
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setProvisionsBusy(false);
    }
  }

  async function removeGroup(id: string) {
    try {
      await api.deleteResourceGroup(id);
      setGroupsData((current) => (current ?? []).filter((g) => g.id !== id));
      setConfirmingDeleteId('');
      if (typeof mode !== 'string' && mode.kind === 'detail' && mode.group.id === id)
        setMode('list');
    } catch (err) {
      showError(formatPortalError(err, t));
    }
  }

  const columns: ListColumn<ResourceGroup>[] = [
    { id: 'name', label: t.tableName, value: (g) => g.name, filter: 'text' },
    {
      id: 'status',
      label: t.tableStatus,
      value: (g) => g.status,
      filter: 'enum',
      searchable: false,
      enumLabel: (v) => (v === 'disabled' ? t.statusDisabled : t.statusActive),
      render: (g) => (
        <StatusChip
          status={g.status}
          label={g.status === 'disabled' ? t.statusDisabled : t.statusActive}
        />
      ),
    },
    {
      id: 'systemGroup',
      label: t.resourceGroupColSystemGroup,
      value: (g) => g.system_group.name,
      filter: 'text',
      render: (g) => g.system_group.name || '-',
    },
    {
      id: 'adminGroups',
      label: t.resourceGroupColAdminGroups,
      value: (g) => g.admin_groups.map((a) => a.name).join(', '),
      filter: 'text',
      render: (g) => g.admin_groups.map((a) => a.name).join(', ') || '-',
    },
    {
      id: 'servers',
      label: t.resourceGroupColServers,
      value: (g) => String(g.servers.length),
      numeric: true,
    },
  ];

  const rowActions = (group: ResourceGroup): RowAction[] => [
    {
      key: 'open',
      label: t.modelDetailsAction,
      icon: <ListAltIcon fontSize="small" />,
      onClick: () => openDetail(group),
    },
  ];

  const listLabels = listTableLabels(t);

  // Create sub-view (admin only -- the list gates the "Neu" action, but this
  // guards a direct mode transition too).
  if (mode === 'create') {
    return (
      <>
        <Breadcrumbs
          ariaLabel={t.breadcrumb}
          backLabel={t.back}
          items={[
            { label: t.resourceGroups, onClick: () => setMode('list') },
            { label: t.resourceGroupCreate },
          ]}
        />
        <Panel titleId="resource-group-form-heading" title={t.resourceGroupCreate}>
          <Box
            component="form"
            onSubmit={submitCreate}
            sx={{ display: 'grid', gridTemplateColumns: 'minmax(260px, 480px)', gap: 2.25 }}
          >
            <Field
              id="resource-group-name"
              label={t.resourceGroupNameLabel}
              value={name}
              onChange={(e) => setName(e.target.value)}
              required
            />
            {/* Resource-group<->admin-group linkage: >=1 admin group required
                for EVERY caller, including system_admin. Exactly one
                candidate -> auto-selected (no field); several -> a required
                multi-select, narrowed to one system group first when the
                candidates span more than one; none -> a hint + the create
                action stays disabled (mirrors ServicesView/ServerList's
                create picker exactly). */}
            <AdminGroupPicker
              idPrefix="resource-group"
              candidates={groupCandidates}
              systemGroupId={createSystemGroupId}
              onSystemGroupIdChange={setCreateSystemGroupId}
              adminGroupIds={createAdminGroupIds}
              onAdminGroupIdsChange={setCreateAdminGroupIds}
              labels={{
                noCandidatesHint: t.resourceGroupNoAdminGroupHint,
                systemGroupLabel: t.resourceGroupAdminGroupSystemGroupLabel,
                systemGroupAuto: t.resourceGroupAdminGroupSystemGroupAuto,
                adminGroupLabel: t.resourceGroupAdminGroupLabel,
                adminGroupAuto: t.resourceGroupAdminGroupAuto,
              }}
            />
            <SelectField
              id="resource-group-status"
              label={t.resourceGroupStatusLabel}
              value={status}
              onChange={(e) => setStatus(e.target.value)}
            >
              <option value="active">{t.statusActive}</option>
              <option value="disabled">{t.statusDisabled}</option>
            </SelectField>
            <Box sx={{ display: 'flex', gap: 1.5 }}>
              <Button
                type="submit"
                variant="contained"
                disabled={busy || createEffectiveAdminGroupIds.length === 0}
              >
                {t.resourceGroupCreate}
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

  // Detail sub-view: settings (name/status + delete) + the admin-group
  // linkage editor + the server-membership editor.
  if (typeof mode !== 'string' && mode.kind === 'detail') {
    const group = groups.find((g) => g.id === mode.group.id) ?? mode.group;
    return (
      <>
        <Breadcrumbs
          ariaLabel={t.breadcrumb}
          backLabel={t.back}
          items={[
            { label: t.resourceGroups, onClick: () => setMode('list') },
            { label: group.name },
          ]}
        />
        <Panel
          titleId="resource-group-settings-heading"
          title={t.resourceGroupSettingsTitle}
          actions={
            <Button
              type="button"
              variant="outlined"
              color="error"
              startIcon={<DeleteIcon fontSize="small" />}
              onClick={() => setConfirmingDeleteId(group.id)}
            >
              {t.resourceGroupActionDelete}
            </Button>
          }
        >
          <Box
            component="form"
            onSubmit={(event) => {
              event.preventDefault();
              void saveSettings();
            }}
            sx={{ display: 'grid', gridTemplateColumns: 'minmax(260px, 480px)', gap: 2.25 }}
          >
            <Field
              id="resource-group-detail-name"
              label={t.resourceGroupNameLabel}
              value={name}
              onChange={(e) => setName(e.target.value)}
              required
            />
            <SelectField
              id="resource-group-detail-status"
              label={t.resourceGroupStatusLabel}
              value={status}
              onChange={(e) => setStatus(e.target.value)}
            >
              <option value="active">{t.statusActive}</option>
              <option value="disabled">{t.statusDisabled}</option>
            </SelectField>
            <Box sx={{ display: 'flex', gap: 1.5 }}>
              <Button type="submit" variant="contained" disabled={busy}>
                {t.save}
              </Button>
            </Box>
          </Box>
        </Panel>

        {/* Admin-groups editor: add/remove the resource group's linked
            admin-tier groups, filtered to its FIXED containment root -- a
            resource group always has a root after create, so (unlike
            ServicesView/ServerList) there is no ungrouped-recovery flow
            here. >=1 group must remain (the backend rejects an empty set;
            the Save button mirrors that). */}
        <Box sx={{ mt: 3 }}>
          <Panel
            titleId="resource-group-admin-groups-heading"
            title={t.resourceGroupAdminGroupsSectionTitle}
          >
            <AdminGroupsEditor
              idPrefix="resource-group"
              candidates={groupCandidates}
              fixedOptions={editAdminGroupOptions(groupCandidates, group, (g) => g.system_group)}
              adminGroupIds={editAdminGroupIds}
              onAdminGroupIdsChange={setEditAdminGroupIds}
              busy={adminGroupsBusy}
              onSave={saveAdminGroups}
              labels={{
                noCandidatesHint: t.resourceGroupNoAdminGroupHint,
                systemGroupLabel: t.resourceGroupAdminGroupSystemGroupLabel,
                systemGroupAuto: t.resourceGroupAdminGroupSystemGroupAuto,
                adminGroupLabel: t.resourceGroupAdminGroupLabel,
                adminGroupAuto: t.resourceGroupAdminGroupAuto,
                saveLabel: t.resourceGroupAdminGroupsSave,
              }}
            />
          </Panel>
        </Box>

        {/* Server-membership editor (Task 5, spec 2026-08-11): add/remove
            member servers, offered from resourceGroupServerCandidates --
            already filtered server-side to the group's own system group, so
            the picker never offers a server the write path would reject.
            Adding an unmanaged/mismatched server (a raw API bypass, or a
            candidate that became unmanageable between fetch and save)
            surfaces as a toast via saveServers' catch. */}
        <Box sx={{ mt: 3 }}>
          <Panel
            titleId="resource-group-servers-heading"
            title={t.resourceGroupServersSectionTitle}
          >
            <Box sx={{ display: 'grid', gridTemplateColumns: 'minmax(260px, 480px)', gap: 2.25 }}>
              <MultiSelectField
                id="resource-group-servers-edit"
                label={t.resourceGroupServersLabel}
                options={serverCandidates.map((c) => ({ value: c.id, label: c.name }))}
                selected={editServerIds}
                onChange={setEditServerIds}
              />
              <Box>
                <Button
                  type="button"
                  variant="contained"
                  disabled={serversBusy}
                  onClick={saveServers}
                >
                  {t.resourceGroupServersSave}
                </Button>
              </Box>
            </Box>
          </Panel>
        </Box>

        {/* Provisioning editor (Phase 2, spec
            2026-08-12-resource-groups-phase-2-provisioning): grants use of
            the resource group's member servers to users/user groups/admin
            groups/services. A full-replace PUT, mirroring the admin-groups/
            servers editors above -- edits are LOCAL until Save; a rejection
            (invalid/non-visible target) surfaces as a toast via
            saveProvisions' catch and leaves the pending list untouched. */}
        <Box sx={{ mt: 3 }}>
          <Panel
            titleId="resource-group-provisions-heading"
            title={t.resourceGroupProvisionsSectionTitle}
          >
            {provisions.length === 0 ? (
              <Typography color="text.secondary" sx={{ mb: 2 }}>
                {t.resourceGroupProvisionsEmpty}
              </Typography>
            ) : (
              <Table size="small" sx={{ mb: 2 }}>
                <TableHead>
                  <TableRow>
                    <TableCell>{t.resourceGroupProvisionsColKind}</TableCell>
                    <TableCell>{t.resourceGroupProvisionsColTarget}</TableCell>
                    <TableCell align="right" />
                  </TableRow>
                </TableHead>
                <TableBody>
                  {[...provisions]
                    .sort((a, b) => {
                      const ai = PROVISION_KINDS.indexOf(a.kind);
                      const bi = PROVISION_KINDS.indexOf(b.kind);
                      return ai !== bi ? ai - bi : a.target_name.localeCompare(b.target_name);
                    })
                    .map((p) => (
                      <TableRow key={`${p.kind}:${p.target_id}`}>
                        <TableCell>{provisionKindLabel(t, p.kind)}</TableCell>
                        <TableCell>{p.target_name || p.target_id}</TableCell>
                        <TableCell align="right">
                          <IconButton
                            size="small"
                            aria-label={t.resourceGroupProvisionsRemoveAction}
                            onClick={() => removeProvision(p.kind, p.target_id)}
                          >
                            <DeleteIcon fontSize="small" />
                          </IconButton>
                        </TableCell>
                      </TableRow>
                    ))}
                </TableBody>
              </Table>
            )}
            <Box sx={{ display: 'flex', gap: 1.5, alignItems: 'flex-end', flexWrap: 'wrap' }}>
              <Box sx={{ minWidth: 200 }}>
                <SelectField
                  id="resource-group-provision-add-kind"
                  label={t.resourceGroupProvisionsAddKindLabel}
                  value={addProvisionKind}
                  onChange={(event) => {
                    setAddProvisionKind(event.target.value as ResourceGroupProvisionKind);
                    setAddProvisionTargetId('');
                  }}
                >
                  {PROVISION_KINDS.map((kind) => (
                    <option value={kind} key={kind}>
                      {provisionKindLabel(t, kind)}
                    </option>
                  ))}
                </SelectField>
              </Box>
              <Box sx={{ minWidth: 260, flex: 1 }}>
                {addProvisionOptions.length === 0 ? (
                  <Typography color="text.secondary" variant="body2">
                    {t.resourceGroupProvisionsNoTargetsHint}
                  </Typography>
                ) : (
                  <SearchableSelect
                    id="resource-group-provision-add-target"
                    label={t.resourceGroupProvisionsAddTargetLabel}
                    value={addProvisionTargetId}
                    onChange={setAddProvisionTargetId}
                    options={addProvisionOptions.map((c) => ({ value: c.id, label: c.name }))}
                  />
                )}
              </Box>
              <Button
                type="button"
                variant="outlined"
                disabled={!addProvisionTargetId}
                onClick={addProvision}
              >
                {t.resourceGroupProvisionsAddAction}
              </Button>
            </Box>
            <Box sx={{ mt: 2 }}>
              <Button
                type="button"
                variant="contained"
                disabled={provisionsBusy}
                onClick={saveProvisions}
              >
                {t.resourceGroupProvisionsSave}
              </Button>
            </Box>
          </Panel>
        </Box>

        <ConfirmDialog
          open={confirmingDeleteId !== ''}
          title={t.resourceGroupDeleteConfirm}
          confirmLabel={t.resourceGroupActionDelete}
          cancelLabel={t.cancel}
          onConfirm={() => void removeGroup(confirmingDeleteId)}
          onCancel={() => setConfirmingDeleteId('')}
        />
      </>
    );
  }

  // List view.
  return (
    <>
      <PageTitle
        title={t.resourceGroups}
        subtitle={t.resourceGroupsIntro}
        action={
          isAdmin ? (
            <Button variant="contained" startIcon={<AddIcon />} onClick={openCreate}>
              {t.resourceGroupCreate}
            </Button>
          ) : undefined
        }
      />
      <Panel titleId="resource-groups-list-heading" title={t.resourceGroupListTitle}>
        <ListTable
          rows={groups}
          columns={columns}
          rowKey={(g) => g.id}
          actions={rowActions}
          storageKey="op.resource-groups"
          labels={listLabels}
          loading={loading || groupsData === null}
        />
      </Panel>
    </>
  );
}
