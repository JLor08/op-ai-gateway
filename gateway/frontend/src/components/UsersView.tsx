// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useEffect, useState, type SubmitEvent } from 'react';
import {
  Alert,
  Box,
  Button,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  Stack,
  Typography,
} from '@mui/material';
import AddIcon from '@mui/icons-material/Add';
import EditIcon from '@mui/icons-material/Edit';
import BlockIcon from '@mui/icons-material/Block';
import CheckCircleIcon from '@mui/icons-material/CheckCircle';
import ForwardToInboxIcon from '@mui/icons-material/ForwardToInbox';
import LockResetIcon from '@mui/icons-material/LockReset';
import TuneIcon from '@mui/icons-material/Tune';
import { EMPTY_LIMIT_CONFIG } from '../api';
import type { AdminUser, LimitConfig, LimitUsage, UserGroup } from '../api';
import type { Translation, PortalApi } from './shared/types';
import { formatPortalError } from './shared/format';
import { useResource } from './shared/useResource';
import { PageTitle } from './shared/PageTitle';
import { Panel } from './shared/Panel';
import { Field } from './shared/Field';
import { SelectField } from './shared/SelectField';
import { SecretReveal } from './shared/SecretReveal';
import { StatusChip } from './shared/StatusChip';
import { Breadcrumbs } from './shared/Breadcrumbs';
import { ListTable, listTableLabels, type ListColumn } from './shared/ListTable';
import type { RowAction } from './shared/RowActionsMenu';
import { LimitsEditor, limitConfigHasPairMismatch } from './shared/LimitsEditor';
import { useToast } from './shared/ToastProvider';
import { ConfirmDialog } from './shared/ConfirmDialog';

type Mode = 'list' | 'create' | { edit: AdminUser };

type InvitePreview = { url: string; email: string; emailSent: boolean; emailError: string };

export function UsersView({
  t,
  api,
  canAssignSystemAdmin,
}: Readonly<{
  t: Translation;
  api: Pick<
    PortalApi,
    | 'adminResetTotp'
    | 'adminUsers'
    | 'createUser'
    | 'groups'
    | 'reinviteUser'
    | 'setUserLimits'
    | 'updateUser'
    | 'userLimits'
  >;
  canAssignSystemAdmin: boolean;
  // NOTE: the invite-form admin-group picker does NOT need the caller's own
  // role: api.groups() returns an actor-scoped landscape whose per-group
  // can_manage_users flag already encodes what THIS caller may invite into
  // (see loadAdminGroupsForInvite below), so no client-side role branching
  // exists here.
}>) {
  const { showError, showSuccess } = useToast();
  const {
    data: usersData,
    error,
    reload,
    loading,
  } = useResource(() => api.adminUsers().then((r) => r.data), [api, t], t, { trackLoading: false });
  useEffect(() => {
    if (error) showError(error);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [error]);
  const users = usersData ?? [];

  const [mode, setMode] = useState<Mode>('list');
  const [invite, setInvite] = useState<InvitePreview | null>(null);
  const [busy, setBusy] = useState(false);
  const [resetTarget, setResetTarget] = useState<AdminUser | null>(null);

  // Principal Limits (Phase 2, design spec §7.2) — per-user rate/quota/budget
  // limits, admin-only (no self-service path exists on the backend), opened
  // as its own dialog rather than a sub-view (mirrors ConfirmDialog/InviteDialog
  // — UsersView has no per-row detail sub-view to embed it in).
  const [limitsTarget, setLimitsTarget] = useState<AdminUser | null>(null);
  const [limitsValue, setLimitsValue] = useState<LimitConfig>(EMPTY_LIMIT_CONFIG);
  const [limitsUsage, setLimitsUsage] = useState<LimitUsage | undefined>(undefined);
  const [limitsLoading, setLimitsLoading] = useState(false);
  const [limitsBusy, setLimitsBusy] = useState(false);

  const [email, setEmail] = useState('');
  const [displayName, setDisplayName] = useState('');
  const [role, setRole] = useState('user');
  const [status, setStatus] = useState('active');

  // Invite-form admin-group picker state (spec:
  // 2026-08-09-group-visibility-admin-group-invite-design.md). Fetched fresh
  // each time the invite sub-view opens, not on list-view mount, so the plain
  // list view never makes this call.
  const [adminGroups, setAdminGroups] = useState<UserGroup[]>([]);
  const [groupsLoading, setGroupsLoading] = useState(false);
  const [selectedAdminGroup, setSelectedAdminGroup] = useState<string>('');

  async function loadAdminGroupsForInvite() {
    setGroupsLoading(true);
    try {
      const landscape = await api.groups();
      // Only admin groups the actor may assign USERS into (owner, a
      // co-manager whose stored can_manage_users flag is set, or a
      // system_admin who manages all) -- spec 2026-08-10 split can_manage
      // (group STRUCTURE) from can_manage_users (USER assignment); a
      // co-manager narrowed to structure-only must not see this group as an
      // invite target. Exactly one -> auto-select (no field).
      const manageable = landscape.admin.filter((g) => g.can_manage_users);
      setAdminGroups(manageable);
      setSelectedAdminGroup(manageable.length === 1 ? manageable[0].id : '');
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setGroupsLoading(false);
    }
  }

  function openCreate() {
    setEmail('');
    setDisplayName('');
    setRole('user');
    setSelectedAdminGroup('');
    setAdminGroups([]);
    setMode('create');
    void loadAdminGroupsForInvite();
  }

  function openEdit(user: AdminUser) {
    setDisplayName(user.display_name);
    setRole(user.role);
    setStatus(user.status);
    setMode({ edit: user });
  }

  async function submitCreate(event: SubmitEvent<HTMLFormElement>) {
    event.preventDefault();
    setBusy(true);
    try {
      const response = await api.createUser({
        email,
        display_name: displayName,
        role,
        admin_group_id: selectedAdminGroup,
      });
      setInvite({
        url: response.invite_url,
        email: response.user?.email ?? email,
        emailSent: !!response.email_sent,
        emailError: response.email_error ?? '',
      });
      setMode('list');
      await reload();
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setBusy(false);
    }
  }

  async function submitEdit(event: SubmitEvent<HTMLFormElement>) {
    event.preventDefault();
    if (mode === 'list' || mode === 'create') return;
    setBusy(true);
    try {
      await api.updateUser(mode.edit.id, { display_name: displayName, role, status });
      setMode('list');
      await reload();
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setBusy(false);
    }
  }

  async function toggleStatus(user: AdminUser) {
    const nextStatus = user.status === 'disabled' ? 'active' : 'disabled';
    try {
      await api.updateUser(user.id, { status: nextStatus });
      await reload();
    } catch (err) {
      showError(formatPortalError(err, t));
    }
  }

  async function reinvite(user: AdminUser) {
    try {
      const response = await api.reinviteUser(user.id);
      setInvite({
        url: response.invite_url,
        email: response.user?.email ?? user.email,
        emailSent: !!response.email_sent,
        emailError: response.email_error ?? '',
      });
    } catch (err) {
      showError(formatPortalError(err, t));
    }
  }

  async function confirmResetTotp() {
    if (!resetTarget) return;
    try {
      await api.adminResetTotp(resetTarget.id);
      showSuccess(t.userResetTotpSuccess);
      await reload();
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setResetTarget(null);
    }
  }

  function openLimits(user: AdminUser) {
    setLimitsTarget(user);
    setLimitsValue(EMPTY_LIMIT_CONFIG);
    setLimitsUsage(undefined);
    setLimitsLoading(true);
    api
      .userLimits(user.id)
      .then((dto) => {
        setLimitsValue(dto.limits);
        setLimitsUsage(dto.usage);
      })
      .catch((err) => {
        showError(formatPortalError(err, t));
        setLimitsTarget(null);
      })
      .finally(() => setLimitsLoading(false));
  }

  async function saveLimits() {
    if (!limitsTarget) return;
    setLimitsBusy(true);
    try {
      const dto = await api.setUserLimits(limitsTarget.id, limitsValue);
      setLimitsValue(dto.limits);
      setLimitsUsage(dto.usage);
      showSuccess(t.save);
      setLimitsTarget(null);
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setLimitsBusy(false);
    }
  }

  const columns: ListColumn<AdminUser>[] = [
    { id: 'email', label: t.tableEmail, value: (u) => u.email, filter: 'text' },
    { id: 'name', label: t.tableName, value: (u) => u.display_name, filter: 'text' },
    {
      id: 'role',
      label: t.tableRole,
      value: (u) => u.role,
      filter: 'enum',
      searchable: false,
      enumLabel: (v) => roleLabel(v, t),
      render: (u) => roleLabel(u.role, t),
    },
    {
      id: 'status',
      label: t.tableStatus,
      value: (u) => u.status,
      filter: 'enum',
      searchable: false,
      enumLabel: (v) => userStatusLabel(v, t),
      render: (u) => (
        <StatusChip
          status={u.status === 'active' ? 'active' : 'disabled'}
          label={userStatusLabel(u.status, t)}
        />
      ),
    },
  ];

  const rowActions = (user: AdminUser): RowAction[] => {
    // Edit + deactivate on a system_admin are refused by the backend for anyone
    // without the system scope (account.UpdateUser -> ErrForbiddenRole), so a
    // plain admin (or a non-elevated system_admin) must not be able to click
    // them -- canAssignSystemAdmin is exactly the system-admin-mode flag. The
    // support ops (limits / re-invite / TOTP reset) stay enabled: they are the
    // deliberately-allowed non-system operations on a system_admin group member.
    const lockSystemAdmin = user.role === 'system_admin' && !canAssignSystemAdmin;
    return [
      {
        key: 'edit',
        label: t.userActionEdit,
        icon: <EditIcon fontSize="small" />,
        onClick: () => openEdit(user),
        disabled: lockSystemAdmin,
      },
      {
        key: 'limits',
        label: t.userActionLimits,
        icon: <TuneIcon fontSize="small" />,
        onClick: () => openLimits(user),
      },
      {
        key: 'toggle',
        label: user.status === 'disabled' ? t.userActionEnable : t.userActionDisable,
        icon:
          user.status === 'disabled' ? (
            <CheckCircleIcon fontSize="small" />
          ) : (
            <BlockIcon fontSize="small" />
          ),
        onClick: () => void toggleStatus(user),
        disabled: lockSystemAdmin,
      },
      {
        key: 'reinvite',
        label: t.userActionReinvite,
        icon: <ForwardToInboxIcon fontSize="small" />,
        onClick: () => void reinvite(user),
      },
      ...(user.totp_enabled
        ? [
            {
              key: 'reset-totp',
              label: t.userActionResetTotp,
              icon: <LockResetIcon fontSize="small" />,
              onClick: () => setResetTarget(user),
            },
          ]
        : []),
    ];
  };

  // Create / edit sub-view (input mask) — replaces the list in place.
  if (mode !== 'list') {
    const editing = mode !== 'create';
    // Mandatory for every role: submit is blocked until an admin group is
    // selected. Exactly one -> pre-selected on load (no field). None -> the
    // hint is shown and submit stays disabled (selectedAdminGroup === "").
    const adminGroupMissing = !editing && selectedAdminGroup === '';
    return (
      <>
        <Breadcrumbs
          ariaLabel={t.breadcrumb}
          backLabel={t.back}
          items={[
            { label: t.users, onClick: () => setMode('list') },
            { label: editing ? t.userEditTitle : t.userCreateTitle },
          ]}
        />
        <Panel titleId="users-form-heading" title={editing ? t.userEditTitle : t.userCreateTitle}>
          <Box
            component="form"
            onSubmit={editing ? submitEdit : submitCreate}
            sx={{ display: 'grid', gridTemplateColumns: 'minmax(220px, 360px)', gap: 2.25 }}
          >
            <Field
              id="user-email"
              label={t.tableEmail}
              type="email"
              value={editing ? mode.edit.email : email}
              onChange={(e) => setEmail(e.target.value)}
              required={!editing}
              disabled={editing}
            />
            <Field
              id="user-name"
              label={t.tableName}
              value={displayName}
              onChange={(e) => setDisplayName(e.target.value)}
            />
            <SelectField
              id="user-role"
              label={t.tableRole}
              value={role}
              onChange={(e) => setRole(e.target.value)}
            >
              <option value="user">{t.roleUser}</option>
              <option value="admin">{t.roleAdmin}</option>
              {canAssignSystemAdmin && <option value="system_admin">{t.roleSystemAdmin}</option>}
            </SelectField>
            {/* Invite-form admin-group picker (create-only, mandatory for
                every role -- spec: 2026-08-09-group-visibility-admin-group-
                invite-design.md). Shown ONLY when more than one manageable
                admin group is available; with exactly one it is taken
                automatically (no control, pre-selected on load); with zero, a
                hint explains why the invite will fail (submit stays disabled,
                see adminGroupMissing above). */}
            {!editing && adminGroups.length > 1 && (
              <SelectField
                id="user-invite-admin-group"
                label={t.userInviteAdminGroupLabel}
                value={selectedAdminGroup}
                onChange={(e) => setSelectedAdminGroup(e.target.value)}
                disabled={groupsLoading}
              >
                <option value="">{t.userInviteAdminGroupSelect}</option>
                {adminGroups.map((g) => (
                  <option key={g.id} value={g.id}>
                    {g.name}
                  </option>
                ))}
              </SelectField>
            )}
            {!editing && !groupsLoading && adminGroups.length === 0 && (
              <Alert severity="warning">{t.userInviteNoAdminGroupHint}</Alert>
            )}
            {editing && (
              <SelectField
                id="user-status"
                label={t.tableStatus}
                value={status}
                onChange={(e) => setStatus(e.target.value)}
              >
                <option value="active">{t.statusActive}</option>
                <option value="disabled">{t.statusDisabled}</option>
              </SelectField>
            )}
            <Box sx={{ display: 'flex', gap: 1.5 }}>
              <Button type="submit" variant="contained" disabled={busy || adminGroupMissing}>
                {editing ? t.userSave : t.userCreate}
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
        <InviteDialog t={t} invite={invite} onClose={() => setInvite(null)} />
        <ConfirmDialog
          open={resetTarget !== null}
          title={t.userResetTotpConfirmTitle}
          body={t.userResetTotpConfirmBody}
          confirmLabel={t.userResetTotpConfirm}
          cancelLabel={t.cancel}
          onConfirm={() => void confirmResetTotp()}
          onCancel={() => setResetTarget(null)}
        />
      </>
    );
  }

  return (
    <>
      <PageTitle
        title={t.users}
        subtitle={t.usersIntro}
        action={
          <Button variant="contained" startIcon={<AddIcon />} onClick={openCreate}>
            {t.userCreate}
          </Button>
        }
      />
      <Panel titleId="users-list-heading" title={t.usersTableTitle}>
        <ListTable
          rows={users}
          columns={columns}
          rowKey={(u) => u.id}
          actions={rowActions}
          storageKey="op.users"
          // Default is 4; a TOTP-enrolled user now has 5 (edit, limits,
          // toggle, reinvite, reset-totp) — keep them all inline rather than
          // folding into the kebab menu once the "limits" action was added.
          maxInlineActions={5}
          labels={listTableLabels(t)}
          loading={loading || usersData === null}
        />
      </Panel>
      <InviteDialog t={t} invite={invite} onClose={() => setInvite(null)} />
      <ConfirmDialog
        open={resetTarget !== null}
        title={t.userResetTotpConfirmTitle}
        body={t.userResetTotpConfirmBody}
        confirmLabel={t.userResetTotpConfirm}
        cancelLabel={t.cancel}
        onConfirm={() => void confirmResetTotp()}
        onCancel={() => setResetTarget(null)}
      />
      <UserLimitsDialog
        t={t}
        target={limitsTarget}
        value={limitsValue}
        onChange={setLimitsValue}
        usage={limitsUsage}
        loading={limitsLoading}
        busy={limitsBusy}
        onSave={() => void saveLimits()}
        onClose={() => setLimitsTarget(null)}
      />
    </>
  );
}

/**
 * Admin-only per-user rate/quota/budget limits editor (design spec §7.2).
 * Fetched + saved via api.userLimits/setUserLimits — there is deliberately
 * no self-service path on the backend, so this dialog is only ever opened
 * from the (already admin-gated) Users list row action.
 */
function UserLimitsDialog({
  t,
  target,
  value,
  onChange,
  usage,
  loading,
  busy,
  onSave,
  onClose,
}: Readonly<{
  t: Translation;
  target: AdminUser | null;
  value: LimitConfig;
  onChange: (next: LimitConfig) => void;
  usage: LimitUsage | undefined;
  loading: boolean;
  busy: boolean;
  onSave: () => void;
  onClose: () => void;
}>) {
  return (
    <Dialog open={target !== null} onClose={onClose} maxWidth="sm" fullWidth>
      <DialogTitle>
        {t.userActionLimits} — {target?.display_name || target?.email}
      </DialogTitle>
      <DialogContent sx={{ display: 'grid', gap: 2 }}>
        <Typography variant="caption" color="text.secondary">
          {t.userLimitsDialogSubtitle}
        </Typography>
        {loading ? (
          <Typography variant="body2" color="text.secondary">
            {t.loading}
          </Typography>
        ) : (
          <LimitsEditor
            t={t}
            idPrefix="user-limits"
            value={value}
            onChange={onChange}
            usage={usage}
          />
        )}
      </DialogContent>
      <DialogActions>
        <Button type="button" onClick={onClose}>
          {t.cancel}
        </Button>
        <Button
          type="button"
          variant="contained"
          onClick={onSave}
          disabled={loading || busy || limitConfigHasPairMismatch(value)}
        >
          {t.save}
        </Button>
      </DialogActions>
    </Dialog>
  );
}

/** Shows a freshly created / re-issued one-time invite link in a modal. */
function InviteDialog({
  t,
  invite,
  onClose,
}: Readonly<{
  t: Translation;
  invite: InvitePreview | null;
  onClose: () => void;
}>) {
  return (
    <Dialog open={invite !== null} onClose={onClose} maxWidth="md" fullWidth>
      <DialogTitle>{t.userInviteLink}</DialogTitle>
      <DialogContent>
        <Stack spacing={1.5}>
          <SecretReveal
            title={t.userInviteLink}
            copyValue={invite?.url ?? ''}
            copyLabel={t.userInviteCopy}
          >
            <code>{invite?.url}</code>
          </SecretReveal>
          {invite?.emailSent && (
            <Typography variant="body2" color="success.main">
              {t.userInviteEmailSent(invite.email)}
            </Typography>
          )}
          {invite && !invite.emailSent && invite.emailError !== '' && (
            <Alert severity="warning">{t.userInviteEmailFailed(invite.emailError)}</Alert>
          )}
        </Stack>
      </DialogContent>
      <DialogActions>
        <Button onClick={onClose}>{t.captureClose}</Button>
      </DialogActions>
    </Dialog>
  );
}

function roleLabel(role: string, t: Translation): string {
  if (role === 'system_admin') return t.roleSystemAdmin;
  if (role === 'admin') return t.roleAdmin;
  return t.roleUser;
}

function userStatusLabel(status: string, t: Translation): string {
  if (status === 'active') return t.statusActive;
  if (status === 'disabled') return t.statusDisabled;
  if (status === 'invited') return t.statusInvited;
  return status;
}
