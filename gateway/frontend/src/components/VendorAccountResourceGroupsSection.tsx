// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useState } from 'react';
import {
  Switch,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableRow,
  Typography,
} from '@mui/material';
import type { VendorAccount, VendorAccountResourceGroup } from '../api';
import type { PortalApi, Translation } from './shared/types';
import { Panel } from './shared/Panel';
import { useLatestFetch } from './shared/useLatestFetch';

// VendorAccountResourceGroupsSection is the vendor-account-OWNER self-service
// view (#183, spec 2026-10-15 sections 9 and 14; the analogue of
// ServerResourceGroupsSection): it lists the resource groups linked to an admin
// group the caller is a MEMBER of and whose root is the account's -- each with a
// member switch -- and joins/leaves THIS account via PUT/DELETE. The caller mounts
// it only for the owner of a `shared` or `full` account; the backend GET is still
// the authority, so anything else it answers (a 403/404, a 400 for a private
// account, a 409 while sharing is off) renders as the empty state (nothing
// actionable), never a scary error.
export function VendorAccountResourceGroupsSection({
  t,
  api,
  account,
}: Readonly<{
  t: Translation;
  api: Pick<
    PortalApi,
    | 'joinVendorAccountResourceGroup'
    | 'leaveVendorAccountResourceGroup'
    | 'vendorAccountResourceGroups'
  >;
  account: VendorAccount;
}>) {
  // Any load failure -> nothing actionable to show, so `groups` folds to [] on the
  // error state too. useLatestFetch keeps its last successful `data` on a rejection
  // (it never overwrites a good list with an error), so we gate on
  // `status === 'error'` explicitly to clear the list on a failed (re)load -- e.g. a
  // reload that rejects after a successful join/leave toggle.
  const load = useLatestFetch(() => api.vendorAccountResourceGroups(account.id), [api, account.id]);
  const groups = load.status === 'error' ? [] : (load.data ?? []);
  const pending = load.status === 'idle' || load.status === 'loading';
  const [busyId, setBusyId] = useState<string | null>(null);
  const [error, setError] = useState<string>('');

  const toggle = (rg: VendorAccountResourceGroup) => {
    if (busyId) return;
    setBusyId(rg.id);
    setError('');
    const action = rg.member
      ? api.leaveVendorAccountResourceGroup(account.id, rg.id)
      : api.joinVendorAccountResourceGroup(account.id, rg.id);
    action
      .then(() => load.reload())
      .catch(() =>
        setError(
          rg.member
            ? t.vendorAccountResourceGroupsLeaveError
            : t.vendorAccountResourceGroupsJoinError,
        ),
      )
      .finally(() => setBusyId(null));
  };

  if (pending) {
    return (
      <Panel titleId="vendor-account-rg-heading" title={t.vendorAccountResourceGroupsTitle}>
        <Typography color="text.secondary">{t.loading}</Typography>
      </Panel>
    );
  }

  return (
    <Panel titleId="vendor-account-rg-heading" title={t.vendorAccountResourceGroupsTitle}>
      {error !== '' && (
        <Typography color="error.main" sx={{ mb: 1 }}>
          {error}
        </Typography>
      )}
      {groups.length === 0 ? (
        <Typography color="text.secondary">{t.vendorAccountResourceGroupsEmpty}</Typography>
      ) : (
        <Table size="small">
          <TableHead>
            <TableRow>
              <TableCell>{t.vendorAccountResourceGroupsColGroup}</TableCell>
              <TableCell align="right">{t.vendorAccountResourceGroupsColMember}</TableCell>
            </TableRow>
          </TableHead>
          <TableBody>
            {groups.map((rg) => (
              <TableRow key={rg.id}>
                <TableCell>{rg.name}</TableCell>
                <TableCell align="right">
                  <Switch
                    checked={rg.member}
                    disabled={busyId !== null}
                    onChange={() => toggle(rg)}
                    slotProps={{ input: { 'aria-label': rg.name } }}
                  />
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </Panel>
  );
}
