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
import type { PortalService, ServiceResourceGroup } from '../api';
import type { PortalApi, Translation } from './shared/types';
import { Panel } from './shared/Panel';
import { useLatestFetch } from './shared/useLatestFetch';

// ServiceResourceGroupsSection is the service-MANAGER self-service view (#183
// Phase 0, the analogue of ServerResourceGroupsSection): it lists the resource
// groups the caller can reach -- those linked to an admin group they are a
// MEMBER of -- each with a member switch, and joins/leaves THIS service via
// PUT/DELETE. The backend GET is the authority: a caller who does not manage the
// service (a plain member, a delegate, a stranger) gets 404, which we render as
// the empty state (nothing actionable), never a scary error.
export function ServiceResourceGroupsSection({
  t,
  api,
  service,
}: Readonly<{
  t: Translation;
  api: Pick<
    PortalApi,
    'joinServiceResourceGroup' | 'leaveServiceResourceGroup' | 'serviceResourceGroups'
  >;
  service: PortalService;
}>) {
  // A non-manager 404 or any load failure -> nothing actionable to show, so
  // `groups` folds to [] on the error state too. useLatestFetch keeps its last
  // successful `data` on a rejection (it never overwrites a good list with an
  // error), so we gate on `status === 'error'` explicitly to clear the list on a
  // failed (re)load -- e.g. a reload that rejects after a successful join/leave
  // toggle.
  const load = useLatestFetch(() => api.serviceResourceGroups(service.id), [api, service.id]);
  const groups = load.status === 'error' ? [] : (load.data ?? []);
  const pending = load.status === 'idle' || load.status === 'loading';
  const [busyId, setBusyId] = useState<string | null>(null);
  const [error, setError] = useState<string>('');

  const toggle = (rg: ServiceResourceGroup) => {
    if (busyId) return;
    setBusyId(rg.id);
    setError('');
    const action = rg.member
      ? api.leaveServiceResourceGroup(service.id, rg.id)
      : api.joinServiceResourceGroup(service.id, rg.id);
    action
      .then(() => load.reload())
      .catch(() =>
        setError(rg.member ? t.serviceResourceGroupsLeaveError : t.serviceResourceGroupsJoinError),
      )
      .finally(() => setBusyId(null));
  };

  if (pending) {
    return (
      <Panel titleId="service-rg-heading" title={t.serviceResourceGroupsTitle}>
        <Typography color="text.secondary">{t.loading}</Typography>
      </Panel>
    );
  }

  return (
    <Panel titleId="service-rg-heading" title={t.serviceResourceGroupsTitle}>
      {error !== '' && (
        <Typography color="error.main" sx={{ mb: 1 }}>
          {error}
        </Typography>
      )}
      {groups.length === 0 ? (
        <Typography color="text.secondary">{t.serviceResourceGroupsEmpty}</Typography>
      ) : (
        <Table size="small">
          <TableHead>
            <TableRow>
              <TableCell>{t.serviceResourceGroupsColGroup}</TableCell>
              <TableCell align="right">{t.serviceResourceGroupsColMember}</TableCell>
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
