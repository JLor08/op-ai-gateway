// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useState } from 'react';
import {
  Alert,
  Box,
  Button,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  IconButton,
  Typography,
} from '@mui/material';
import AddIcon from '@mui/icons-material/Add';
import ArrowDownwardIcon from '@mui/icons-material/ArrowDownward';
import ArrowUpwardIcon from '@mui/icons-material/ArrowUpward';
import CloseIcon from '@mui/icons-material/Close';
import DeleteIcon from '@mui/icons-material/Delete';
import EditIcon from '@mui/icons-material/Edit';
import type { BadgeStatus, PortalApi, Translation } from './shared/types';
import type { VendorAccount, VendorAccountGroup, VendorAccountGroupMember } from '../api';
import { formatPortalError } from './shared/format';
import { useLatestFetch } from './shared/useLatestFetch';
import { Panel } from './shared/Panel';
import { Field } from './shared/Field';
import { SelectField } from './shared/SelectField';
import { StatusChip } from './shared/StatusChip';
import { ConfirmDialog } from './shared/ConfirmDialog';
import { vendorLabel } from './shared/vendorLabel';
import { vendorStatusBadge, vendorStatusLabel } from './shared/vendorStatus';
import { isValidModelPrefix, normalizeModelPrefix } from './shared/vendorInputs';
import { CONFIGURABLE_ROOT_VENDORS } from './shared/vendorPresets';
import { VendorAccountUsageRows } from './VendorAccountUsageRows';

const VENDORS = ['openai', 'anthropic', ...CONFIGURABLE_ROOT_VENDORS];
const DEFAULT_THRESHOLD = '99';

type GroupApi = Pick<
  PortalApi,
  | 'vendorAccountGroups'
  | 'vendorAccounts'
  | 'createVendorAccountGroup'
  | 'updateVendorAccountGroup'
  | 'deleteVendorAccountGroup'
  | 'setVendorAccountGroupMembers'
>;

function stateBadge(state: string): BadgeStatus {
  switch (state) {
    case 'active':
      return 'active';
    case 'available':
      return 'standby';
    case 'over_threshold':
    case 'cooldown':
      return 'watch';
    default:
      return 'disabled';
  }
}

function stateLabel(t: Translation, state: string): string {
  switch (state) {
    case 'active':
      return t.vendorGroupStateActive;
    case 'available':
      return t.vendorGroupStateAvailable;
    case 'over_threshold':
      return t.vendorGroupStateOverThreshold;
    case 'cooldown':
      return t.vendorGroupStateCooldown;
    default:
      return t.vendorGroupStateNotUsable;
  }
}

function reasonLabel(t: Translation, reason: string): string {
  switch (reason) {
    case 'disabled':
      return t.vendorGroupReasonDisabled;
    case 'needs_reconnect':
      return t.vendorGroupReasonNeedsReconnect;
    case 'subscriptions_disabled':
      return t.vendorGroupReasonSubscriptionsDisabled;
    case 'not_reachable':
      return t.vendorGroupReasonNotReachable;
    default:
      return reason;
  }
}

// An account's display name, with its owner for one somebody else owns.
function ownedLabel(t: Translation, name: string, owner: string): string {
  return owner ? `${name} (${t.vendorAccountSharedBy} ${owner})` : name;
}

function isThreshold(raw: string): boolean {
  const n = Number(raw);
  return raw.trim() !== '' && Number.isInteger(n) && n >= 1 && n <= 100;
}

function MemberRow({ t, member }: Readonly<{ t: Translation; member: VendorAccountGroupMember }>) {
  return (
    <Box sx={{ py: 1, borderTop: 1, borderColor: 'divider' }}>
      <Box sx={{ display: 'flex', alignItems: 'center', gap: 1.5, flexWrap: 'wrap' }}>
        <Typography variant="caption" color="text.secondary">
          {t.vendorGroupMemberPosition(member.position)}
        </Typography>
        <Typography>{ownedLabel(t, member.account_name, member.owner_name)}</Typography>
        <StatusChip status={stateBadge(member.state)} label={stateLabel(t, member.state)} />
        {member.reason !== '' && (
          <Typography variant="caption" color="text.secondary">
            {reasonLabel(t, member.reason)}
          </Typography>
        )}
        {member.free_at && (
          <Typography variant="caption" color="text.secondary">
            {t.vendorGroupMemberFreeAt(new Date(member.free_at).toLocaleString())}
          </Typography>
        )}
      </Box>
      {member.usage && (
        <Box sx={{ mt: 1 }}>
          <VendorAccountUsageRows t={t} usage={member.usage} dense />
        </Box>
      )}
    </Box>
  );
}

function GroupDialog({
  t,
  api,
  group,
  accounts,
  onClose,
  onSaved,
}: Readonly<{
  t: Translation;
  api: GroupApi;
  // null = create.
  group: VendorAccountGroup | null;
  accounts: VendorAccount[];
  onClose: () => void;
  onSaved: () => void;
}>) {
  const editing = group !== null;
  const [name, setName] = useState(group?.name ?? '');
  const [vendor, setVendor] = useState(group?.vendor ?? '');
  const [prefix, setPrefix] = useState(group?.model_prefix ?? '');
  const [status, setStatus] = useState(group?.status ?? 'active');
  const [fiveHour, setFiveHour] = useState(
    group ? String(group.thresholds.five_hour_pct) : DEFAULT_THRESHOLD,
  );
  const [weekly, setWeekly] = useState(
    group ? String(group.thresholds.weekly_pct) : DEFAULT_THRESHOLD,
  );
  const [credits, setCredits] = useState(
    group ? String(group.thresholds.credits_pct) : DEFAULT_THRESHOLD,
  );
  const originalIds = group?.members.map((m) => m.account_id) ?? [];
  const [memberIds, setMemberIds] = useState<string[]>(originalIds);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');

  // Names for the current members: the account list first, then what the group
  // itself reported (a member the list no longer shows keeps its label).
  const labels = new Map<string, string>();
  for (const m of group?.members ?? []) {
    labels.set(m.account_id, ownedLabel(t, m.account_name, m.owner_name));
  }
  for (const a of accounts) {
    labels.set(a.id, ownedLabel(t, a.name, a.is_owner ? '' : a.shared_by));
  }
  const labelOf = (id: string) => labels.get(id) ?? id;
  const plainName = (id: string) =>
    accounts.find((a) => a.id === id)?.name ??
    group?.members.find((m) => m.account_id === id)?.account_name ??
    id;

  const candidates = accounts.filter((a) => a.vendor === vendor && !memberIds.includes(a.id));
  const prefixValid = isValidModelPrefix(prefix);
  const thresholdsValid = isThreshold(fiveHour) && isThreshold(weekly) && isThreshold(credits);
  const canSave = name.trim() !== '' && vendor !== '' && prefixValid && thresholdsValid && !busy;

  const move = (index: number, delta: number) => {
    const next = [...memberIds];
    [next[index], next[index + delta]] = [next[index + delta], next[index]];
    setMemberIds(next);
  };

  const save = async () => {
    setBusy(true);
    setError('');
    const thresholds = {
      threshold_five_hour_pct: Number(fiveHour),
      threshold_weekly_pct: Number(weekly),
      threshold_credits_pct: Number(credits),
    };
    try {
      if (group) {
        await api.updateVendorAccountGroup(group.id, {
          name: name.trim(),
          model_prefix: normalizeModelPrefix(prefix),
          status,
          ...thresholds,
        });
        if (memberIds.join('\n') !== originalIds.join('\n')) {
          await api.setVendorAccountGroupMembers(group.id, memberIds);
        }
      } else {
        await api.createVendorAccountGroup({
          name: name.trim(),
          vendor,
          model_prefix: normalizeModelPrefix(prefix),
          status,
          ...thresholds,
          account_ids: memberIds,
        });
      }
      onSaved();
    } catch (err) {
      setError(formatPortalError(err, t));
      setBusy(false);
    }
  };

  return (
    <Dialog open onClose={onClose} fullWidth maxWidth="sm">
      <DialogTitle>{editing ? t.vendorGroupEditTitle : t.vendorGroupCreateTitle}</DialogTitle>
      <DialogContent>
        <Box sx={{ display: 'grid', gap: 2, pt: 1 }}>
          {error !== '' && <Alert severity="error">{error}</Alert>}
          <Field
            id="vendor-group-name"
            label={t.vendorAccountNameLabel}
            value={name}
            onChange={(e) => setName(e.target.value)}
            required
          />
          <SelectField
            id="vendor-group-vendor"
            label={t.vendorAccountVendorLabel}
            value={vendor}
            disabled={editing}
            onChange={(e) => {
              setVendor(e.target.value);
              setMemberIds([]);
            }}
          >
            {VENDORS.map((v) => (
              <option key={v} value={v}>
                {vendorLabel(t, v)}
              </option>
            ))}
          </SelectField>
          <Field
            id="vendor-group-prefix"
            label={t.vendorAccountModelPrefixLabel}
            value={prefix}
            onChange={(e) => setPrefix(e.target.value)}
            error={!prefixValid}
            helperText={
              prefixValid ? t.vendorAccountModelPrefixNote : t.errorVendorAccountModelPrefixInvalid
            }
          />
          <SelectField
            id="vendor-group-status"
            label={t.vendorAccountStatusLabel}
            value={status}
            onChange={(e) => setStatus(e.target.value)}
          >
            <option value="active">{vendorStatusLabel(t, 'active')}</option>
            <option value="disabled">{vendorStatusLabel(t, 'disabled')}</option>
          </SelectField>
          <Box sx={{ display: 'grid', gridTemplateColumns: 'repeat(3, 1fr)', gap: 1.5 }}>
            <Field
              id="vendor-group-th-five"
              type="number"
              label={t.vendorGroupThresholdFiveHour}
              value={fiveHour}
              onChange={(e) => setFiveHour(e.target.value)}
              error={!isThreshold(fiveHour)}
              inputProps={{ min: 1, max: 100 }}
            />
            <Field
              id="vendor-group-th-weekly"
              type="number"
              label={t.vendorGroupThresholdWeekly}
              value={weekly}
              onChange={(e) => setWeekly(e.target.value)}
              error={!isThreshold(weekly)}
              inputProps={{ min: 1, max: 100 }}
            />
            <Field
              id="vendor-group-th-credits"
              type="number"
              label={t.vendorGroupThresholdCredits}
              value={credits}
              onChange={(e) => setCredits(e.target.value)}
              error={!isThreshold(credits)}
              inputProps={{ min: 1, max: 100 }}
            />
          </Box>
          <Typography variant="caption" color={thresholdsValid ? 'text.secondary' : 'error'}>
            {thresholdsValid ? t.vendorGroupThresholdHint : t.vendorGroupThresholdInvalid}
          </Typography>

          <Box>
            <Typography variant="subtitle2" sx={{ mb: 0.5 }}>
              {t.vendorGroupMembersTitle}
            </Typography>
            {memberIds.length === 0 ? (
              <Typography color="text.secondary">{t.vendorGroupMembersEmpty}</Typography>
            ) : (
              memberIds.map((id, i) => (
                <Box key={id} sx={{ display: 'flex', alignItems: 'center', gap: 1, py: 0.25 }}>
                  <Typography variant="caption" color="text.secondary">
                    {t.vendorGroupMemberPosition(i + 1)}
                  </Typography>
                  <Typography sx={{ flex: 1 }}>{labelOf(id)}</Typography>
                  <IconButton
                    size="small"
                    aria-label={t.vendorGroupMemberMoveUp(plainName(id))}
                    disabled={i === 0}
                    onClick={() => move(i, -1)}
                  >
                    <ArrowUpwardIcon fontSize="small" />
                  </IconButton>
                  <IconButton
                    size="small"
                    aria-label={t.vendorGroupMemberMoveDown(plainName(id))}
                    disabled={i === memberIds.length - 1}
                    onClick={() => move(i, 1)}
                  >
                    <ArrowDownwardIcon fontSize="small" />
                  </IconButton>
                  <IconButton
                    size="small"
                    aria-label={t.vendorGroupMemberRemove(plainName(id))}
                    onClick={() => setMemberIds(memberIds.filter((m) => m !== id))}
                  >
                    <CloseIcon fontSize="small" />
                  </IconButton>
                </Box>
              ))
            )}
            {vendor !== '' &&
              (candidates.length === 0 ? (
                <Typography variant="caption" color="text.secondary">
                  {t.vendorGroupMemberAddNone}
                </Typography>
              ) : (
                <SelectField
                  id="vendor-group-add-member"
                  label={t.vendorGroupMemberAddLabel}
                  value=""
                  size="small"
                  sx={{ mt: 1 }}
                  onChange={(e) => setMemberIds([...memberIds, e.target.value])}
                >
                  {candidates.map((a) => (
                    <option key={a.id} value={a.id}>
                      {labelOf(a.id)}
                    </option>
                  ))}
                </SelectField>
              ))}
          </Box>
        </Box>
      </DialogContent>
      <DialogActions>
        <Button onClick={onClose} color="secondary">
          {t.cancel}
        </Button>
        <Button variant="contained" onClick={() => void save()} disabled={!canSave}>
          {t.save}
        </Button>
      </DialogActions>
    </Dialog>
  );
}

/**
 * The "Groups" section of the Anbieter page: the user's vendor account groups
 * (ordered failover groups of accounts at one vendor, spec 2026-11-24 section 9)
 * with each member's computed state, and the create / edit / delete flows. The
 * member list is edited in the dialog and committed with the save (a PATCH for the
 * fields, then the member replace when the order or the set changed).
 */
export function VendorAccountGroupsSection({
  t,
  api,
}: Readonly<{ t: Translation; api: GroupApi }>) {
  const groupsLoad = useLatestFetch(() => api.vendorAccountGroups().then((r) => r.groups), [api]);
  const accountsLoad = useLatestFetch(() => api.vendorAccounts().then((r) => r.data), [api]);
  const groups = groupsLoad.data ?? [];
  const accounts = accountsLoad.data ?? [];
  // undefined = closed, null = create, a group = edit.
  const [editing, setEditing] = useState<VendorAccountGroup | null | undefined>(undefined);
  const [deleting, setDeleting] = useState<VendorAccountGroup | null>(null);
  const [error, setError] = useState('');

  const remove = async (group: VendorAccountGroup) => {
    setDeleting(null);
    try {
      await api.deleteVendorAccountGroup(group.id);
      groupsLoad.reload();
    } catch (err) {
      setError(formatPortalError(err, t));
    }
  };

  return (
    <Panel
      titleId="vendor-account-groups-heading"
      title={t.vendorGroupsTitle}
      subtitle={t.vendorGroupsIntro}
      actions={
        <Button variant="contained" startIcon={<AddIcon />} onClick={() => setEditing(null)}>
          {t.vendorGroupCreate}
        </Button>
      }
    >
      {error !== '' && (
        <Alert severity="error" sx={{ mb: 1.5 }}>
          {error}
        </Alert>
      )}
      {groupsLoad.status === 'error' && (
        <Alert severity="error" sx={{ mb: 1.5 }}>
          {formatPortalError(groupsLoad.error, t)}
        </Alert>
      )}
      {groupsLoad.status === 'ok' && groups.length === 0 && (
        <Typography color="text.secondary">{t.vendorGroupsEmpty}</Typography>
      )}
      {groups.map((g) => (
        <Box key={g.id} sx={{ mb: 2.5 }}>
          <Box sx={{ display: 'flex', alignItems: 'center', gap: 1.5, flexWrap: 'wrap' }}>
            <Typography variant="subtitle1" sx={{ fontWeight: 600 }}>
              {g.name}
            </Typography>
            <Typography variant="caption" color="text.secondary">
              {vendorLabel(t, g.vendor)}
              {g.model_prefix !== '' && ` · ${g.model_prefix}`}
            </Typography>
            <StatusChip
              status={vendorStatusBadge(g.status)}
              label={vendorStatusLabel(t, g.status)}
            />
            <Box sx={{ flex: 1 }} />
            <IconButton
              size="small"
              aria-label={`${t.vendorGroupActionEdit} ${g.name}`}
              onClick={() => setEditing(g)}
            >
              <EditIcon fontSize="small" />
            </IconButton>
            <IconButton
              size="small"
              aria-label={`${t.vendorAccountActionDelete} ${g.name}`}
              onClick={() => setDeleting(g)}
            >
              <DeleteIcon fontSize="small" />
            </IconButton>
          </Box>
          {g.members.length === 0 ? (
            <Typography color="text.secondary">{t.vendorGroupMembersEmpty}</Typography>
          ) : (
            g.members.map((m) => <MemberRow key={m.account_id} t={t} member={m} />)
          )}
        </Box>
      ))}
      {editing !== undefined && (
        <GroupDialog
          t={t}
          api={api}
          group={editing}
          accounts={accounts}
          onClose={() => setEditing(undefined)}
          onSaved={() => {
            setEditing(undefined);
            groupsLoad.reload();
          }}
        />
      )}
      <ConfirmDialog
        open={deleting !== null}
        title={t.vendorGroupDeleteConfirm}
        confirmLabel={t.vendorAccountActionDelete}
        cancelLabel={t.cancel}
        onConfirm={() => deleting && void remove(deleting)}
        onCancel={() => setDeleting(null)}
      />
    </Panel>
  );
}
