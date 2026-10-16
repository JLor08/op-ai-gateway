// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useEffect, useState, type ReactNode } from 'react';
import { Alert, Box, Button, Typography } from '@mui/material';
import type { AdminGroupCandidate, VendorAccount, VendorAccountShareLevel } from '../api';
import type { PortalApi, Translation } from './shared/types';
import { formatPortalError } from './shared/format';
import { Panel } from './shared/Panel';
import { Field } from './shared/Field';
import { SelectField } from './shared/SelectField';
import { ConfirmDialog } from './shared/ConfirmDialog';
import { AdminGroupPicker } from './shared/AdminGroupPicker';
import { AdminGroupsEditor } from './shared/AdminGroupsEditor';
import {
  candidatesUnderSystemGroup,
  distinctParentGroups,
  editAdminGroupOptions,
  type AdminGroupLinkageLabels,
} from './shared/adminGroupLinkage';
import type { VendorAccountAccess } from './shared/vendorAccountAccess';
import { useToast } from './shared/ToastProvider';

function levelLabel(t: Translation, level: VendorAccountShareLevel): string {
  switch (level) {
    case 'shared':
      return t.vendorAccountShareLevelShared;
    case 'full':
      return t.vendorAccountShareLevelFull;
    default:
      return t.vendorAccountShareLevelNone;
  }
}

function levelNote(t: Translation, level: VendorAccountShareLevel): string {
  switch (level) {
    case 'shared':
      return t.vendorAccountShareLevelSharedNote;
    case 'full':
      return t.vendorAccountShareLevelFullNote;
    default:
      return t.vendorAccountShareLevelNoneNote;
  }
}

/**
 * The "Sharing" area of a vendor account's detail view (#183, spec 2026-10-15
 * sections 6, 7 and 14): the account's share level, the actions on it the viewer is
 * entitled to (`access`, see vendorAccountAccess) and, for the viewer who may change
 * them, the management-group editor in a panel of its own.
 *
 * It only ever OFFERS what the backend would accept; the backend stays the gate and
 * answers a stale or crafted request with 403/404/409, which is a toast here.
 *
 *   - RAISE (an admin owner): to the levels `access.raiseTargets` names (never `full`
 *     for a subscription account), always naming >= 1 management group. From `none`
 *     the groups are picked here (the same system-group-then-groups picker the server
 *     create form uses, over the candidates the backend lists for the caller), which
 *     also fixes the account's containment root. From `shared` to `full` the account
 *     already has its groups and the raise carries them unchanged; an account that
 *     has lost its last one cannot be raised until the editor below gives it a group.
 *   - LOWER (the owner, or a co-manager at `full`): to `shared` from `full`, or stop
 *     sharing (to `none`), which also removes the management groups and the
 *     resource-group links of the account and so asks for confirmation.
 *   - The management-group editor is AdminGroupsEditor, shared with the server,
 *     service and resource-group views. It has no ungrouped-recovery step (the
 *     `ungrouped` option the server editor has): a raise always sets the account's
 *     containment root, so a shared account always has one.
 *
 * The panel reports every answered change through `onChanged` with the account as the
 * viewer now reads it; the parent owns the account list. `busy` is the parent's "an
 * account write is running" flag, so a settings save and a sharing change never
 * overlap. The candidates are read once, and only when this panel can use them.
 */
export function VendorAccountSharePanel({
  t,
  api,
  account,
  access,
  busy: parentBusy,
  onChanged,
}: Readonly<{
  t: Translation;
  api: Pick<
    PortalApi,
    | 'setVendorAccountShareLevel'
    | 'setVendorAccountAdminGroups'
    | 'vendorAccountAdminGroupCandidates'
  >;
  account: VendorAccount;
  access: VendorAccountAccess;
  busy: boolean;
  onChanged: (updated: VendorAccount) => void;
}>) {
  const { showError, showSuccess } = useToast();
  const { level, raiseTargets, lowerTargets } = access;
  const raising = raiseTargets.length > 0;
  const raisingFromNone = raising && level === 'none';

  // The admin-tier groups the viewer may name, null until read. A viewer that may
  // neither pick groups for a raise from `none` nor edit them never asks.
  const needsCandidates = raisingFromNone || access.canEditAdminGroups;
  const [candidates, setCandidates] = useState<AdminGroupCandidate[] | null>(null);
  useEffect(() => {
    if (!needsCandidates) return;
    let cancelled = false;
    (async () => {
      try {
        const list = await api.vendorAccountAdminGroupCandidates();
        if (!cancelled) setCandidates(list);
      } catch {
        // The picker and the editor degrade to "no candidates" (the sharing switch
        // being off is a 409 here); the account itself is unaffected.
        if (!cancelled) setCandidates([]);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [api, needsCandidates]);

  const [busy, setBusy] = useState(false);
  const disabled = busy || parentBusy;
  const [confirmingStop, setConfirmingStop] = useState(false);

  // The raise form: the level chosen (the lowest by default) and, from `none`, the
  // picked system group and management groups.
  const [chosenLevel, setChosenLevel] = useState('');
  const target = raiseTargets.find((l) => l === chosenLevel) ?? raiseTargets[0];
  const [systemGroupId, setSystemGroupId] = useState('');
  const [pickedGroupIds, setPickedGroupIds] = useState<string[]>([]);
  // The system-group step shows only when the candidates span several roots, and a
  // single remaining candidate selects itself (the server create form's derivation).
  const distinct = distinctParentGroups(candidates ?? []);
  const effectiveSystemGroupId = distinct.length === 1 ? distinct[0].id : systemGroupId;
  const pickable = candidatesUnderSystemGroup(candidates ?? [], distinct, effectiveSystemGroupId);
  // The management groups the raise names: the picked ones from `none`, the
  // account's own when it is already shared.
  function groupsOfRaise(): string[] {
    if (!raisingFromNone) return account.admin_groups.map((g) => g.id);
    return pickable.length === 1 ? [pickable[0].id] : pickedGroupIds;
  }
  const raiseGroupIds = groupsOfRaise();
  const canRaise = !disabled && target !== undefined && raiseGroupIds.length > 0;

  const linkageLabels: AdminGroupLinkageLabels = {
    noCandidatesHint: t.vendorAccountNoAdminGroupHint,
    systemGroupLabel: t.vendorAccountAdminGroupSystemGroupLabel,
    systemGroupAuto: t.vendorAccountAdminGroupSystemGroupAuto,
    adminGroupLabel: t.vendorAccountAdminGroupLabel,
    adminGroupAuto: t.vendorAccountAdminGroupAuto,
  };

  // The one place a sharing write is run: report what it answered, or toast why not.
  async function run(write: () => Promise<VendorAccount>, saved: string) {
    setBusy(true);
    try {
      const updated = await write();
      showSuccess(saved);
      onChanged(updated);
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setBusy(false);
    }
  }

  function raise() {
    if (target === undefined) return;
    void run(
      () => api.setVendorAccountShareLevel(account.id, target, raiseGroupIds),
      t.vendorAccountShareSaved,
    );
  }

  // A lower names only the level: the groups are the backend's to keep or to clear.
  function lower(next: VendorAccountShareLevel) {
    void run(() => api.setVendorAccountShareLevel(account.id, next), t.vendorAccountShareSaved);
  }

  function saveAdminGroups(ids: string[]) {
    void run(
      () => api.setVendorAccountAdminGroups(account.id, ids),
      t.vendorAccountAdminGroupsSaved,
    );
  }

  let raiseGroups: ReactNode = null;
  if (raisingFromNone) {
    raiseGroups =
      candidates === null ? null : (
        <AdminGroupPicker
          idPrefix="vendor-account"
          candidates={candidates}
          systemGroupId={systemGroupId}
          onSystemGroupIdChange={setSystemGroupId}
          adminGroupIds={pickedGroupIds}
          onAdminGroupIdsChange={setPickedGroupIds}
          labels={linkageLabels}
        />
      );
  } else if (raising) {
    raiseGroups =
      account.admin_groups.length === 0 ? (
        <Alert severity="warning">{t.vendorAccountShareNoGroupsHint}</Alert>
      ) : (
        <Typography variant="body2" color="text.secondary">
          {t.vendorAccountShareKeepGroupsNote}
        </Typography>
      );
  }

  return (
    <>
      <Panel
        titleId="vendor-account-share-heading"
        title={t.vendorAccountShareTitle}
        subtitle={t.vendorAccountShareIntro}
      >
        <Box sx={{ display: 'grid', gridTemplateColumns: 'minmax(260px, 480px)', gap: 2.25 }}>
          <Field
            id="vendor-account-share-level"
            label={t.vendorAccountShareLevelLabel}
            value={levelLabel(t, level)}
            onChange={() => undefined}
            readOnly
            helperText={levelNote(t, level)}
          />
          {level !== 'none' && account.system_group_name !== '' && (
            <Typography variant="body2" color="text.secondary">
              {t.vendorAccountAdminGroupSystemGroupAuto(account.system_group_name)}
            </Typography>
          )}
          {account.auth_type === 'subscription' && (
            <Typography variant="body2" color="text.secondary">
              {t.vendorAccountShareSubscriptionNote}
            </Typography>
          )}
          {raising && (
            <>
              {raiseTargets.length > 1 && (
                <SelectField
                  id="vendor-account-share-target"
                  label={t.vendorAccountShareTargetLabel}
                  value={target ?? ''}
                  onChange={(e) => setChosenLevel(e.target.value)}
                >
                  {raiseTargets.map((l) => (
                    <option value={l} key={l}>
                      {levelLabel(t, l)}
                    </option>
                  ))}
                </SelectField>
              )}
              {/* What the level it is raised to means: a raise to `full` hands the
                  co-managers the credential, so it is spelled out before the click. */}
              {target !== undefined && (
                <Typography variant="body2" color="text.secondary">
                  {levelNote(t, target)}
                </Typography>
              )}
              {raiseGroups}
              <Box>
                <Button type="button" variant="contained" disabled={!canRaise} onClick={raise}>
                  {t.vendorAccountShareRaiseAction(levelLabel(t, target ?? 'shared'))}
                </Button>
              </Box>
            </>
          )}
          {lowerTargets.length > 0 && (
            <Box sx={{ display: 'flex', flexWrap: 'wrap', gap: 1.5 }}>
              {lowerTargets.map((next) =>
                next === 'none' ? (
                  <Button
                    key={next}
                    type="button"
                    variant="outlined"
                    color="error"
                    disabled={disabled}
                    onClick={() => setConfirmingStop(true)}
                  >
                    {t.vendorAccountShareStopAction}
                  </Button>
                ) : (
                  <Button
                    key={next}
                    type="button"
                    variant="outlined"
                    disabled={disabled}
                    onClick={() => lower(next)}
                  >
                    {t.vendorAccountShareLowerAction(levelLabel(t, next))}
                  </Button>
                ),
              )}
            </Box>
          )}
          {!raising && lowerTargets.length === 0 && (
            <Typography variant="body2" color="text.secondary">
              {t.vendorAccountShareReadOnlyNote}
            </Typography>
          )}
        </Box>
        <ConfirmDialog
          open={confirmingStop}
          title={t.vendorAccountShareStopConfirm}
          confirmLabel={t.vendorAccountShareStopAction}
          cancelLabel={t.cancel}
          onConfirm={() => {
            setConfirmingStop(false);
            lower('none');
          }}
          onCancel={() => setConfirmingStop(false)}
        />
      </Panel>

      {/* Replace the management groups: the owner at `shared`, the owner or a
          co-manager at `full`. Narrowed to the account's containment root. Keyed by
          the account's groups, so the pending selection restarts from what was
          saved and survives any other change to the account. */}
      {access.canEditAdminGroups && (
        <Box sx={{ mt: 3 }}>
          <ManagementGroupsPanel
            key={account.admin_groups.map((g) => g.id).join(',')}
            t={t}
            account={account}
            candidates={candidates}
            labels={linkageLabels}
            busy={disabled}
            onSave={saveAdminGroups}
          />
        </Box>
      )}
    </>
  );
}

/**
 * What the "Sharing" area shows INSTEAD of VendorAccountSharePanel while sharing is
 * switched off (the vendor_account_sharing_enabled master flag; spec 2026-10-15
 * section 13): no control, because the backend answers every one of them 409. Every
 * account behaves as personal, so an account that was already shared is read the
 * same way (the stored level and links are kept, and apply again once the flag is
 * back on); the stored level is named so the owner can see what is paused.
 */
export function VendorAccountSharingOffPanel({
  t,
  access,
}: Readonly<{
  t: Translation;
  access: VendorAccountAccess;
}>) {
  return (
    <Panel titleId="vendor-account-share-heading" title={t.vendorAccountShareTitle}>
      <Box sx={{ display: 'grid', gridTemplateColumns: 'minmax(260px, 480px)', gap: 2.25 }}>
        <Alert severity="info" role="status">
          {t.vendorAccountSharingOffNotice}
        </Alert>
        {access.level !== 'none' && (
          <Typography variant="body2" color="text.secondary">
            {t.vendorAccountSharingOffStoredLevel(levelLabel(t, access.level))}
          </Typography>
        )}
      </Box>
    </Panel>
  );
}

// The management-group editor panel. Owns the pending selection, seeded from the
// account's groups: the parent re-keys it whenever those change.
function ManagementGroupsPanel({
  t,
  account,
  candidates,
  labels,
  busy,
  onSave,
}: Readonly<{
  t: Translation;
  account: VendorAccount;
  candidates: AdminGroupCandidate[] | null;
  labels: AdminGroupLinkageLabels;
  busy: boolean;
  onSave: (ids: string[]) => void;
}>) {
  const [adminGroupIds, setAdminGroupIds] = useState<string[]>(() =>
    account.admin_groups.map((g) => g.id),
  );
  return (
    <Panel
      titleId="vendor-account-admin-groups-heading"
      title={t.vendorAccountAdminGroupsSectionTitle}
    >
      {candidates !== null && (
        <AdminGroupsEditor
          idPrefix="vendor-account"
          candidates={candidates}
          fixedOptions={editAdminGroupOptions(candidates, account, (a) => ({
            id: a.system_group_id,
            name: a.system_group_name,
          }))}
          adminGroupIds={adminGroupIds}
          onAdminGroupIdsChange={setAdminGroupIds}
          busy={busy}
          onSave={onSave}
          labels={{ ...labels, saveLabel: t.vendorAccountAdminGroupsSave }}
        />
      )}
    </Panel>
  );
}
