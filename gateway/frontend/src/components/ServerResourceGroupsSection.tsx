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
import type { PortalServer, ServerResourceGroup } from '../api';
import type { PortalApi, Translation } from './shared/types';
import { Panel } from './shared/Panel';
import { useLatestFetch } from './shared/useLatestFetch';

// ServerResourceGroupsSection is the server-OWNER self-service view (spec
// 2026-08-11-resource-groups-server-owner-self-service): it lists the resource
// groups linked to an admin group the caller is a MEMBER of (each with a member
// switch) and joins/leaves THIS server via PUT/DELETE. The backend GET is the
// authority -- a non-owner gets 404, which we render as the empty state (nothing
// actionable), never a scary error.
export function ServerResourceGroupsSection({
  t,
  api,
  server,
}: Readonly<{
  t: Translation;
  api: Pick<PortalApi, 'joinResourceGroup' | 'leaveResourceGroup' | 'serverResourceGroups'>;
  server: PortalServer;
}>) {
  // A non-owner 404 or any load failure -> nothing actionable to show, so
  // `groups` folds to [] on the error state too. useLatestFetch keeps its last
  // successful `data` on a rejection (it never overwrites a good list with an
  // error), so we must gate on `status === 'error'` explicitly to reproduce the
  // pre-hook behavior of clearing the list on a failed (re)load — e.g. a reload
  // that rejects after a successful join/leave toggle.
  const load = useLatestFetch(() => api.serverResourceGroups(server.id), [api, server.id]);
  const groups = load.status === 'error' ? [] : (load.data ?? []);
  const pending = load.status === 'idle' || load.status === 'loading';
  const [busyId, setBusyId] = useState<string | null>(null);
  const [error, setError] = useState<string>('');

  const toggle = (rg: ServerResourceGroup) => {
    if (busyId) return;
    setBusyId(rg.id);
    setError('');
    const action = rg.member
      ? api.leaveResourceGroup(server.id, rg.id)
      : api.joinResourceGroup(server.id, rg.id);
    action
      .then(() => load.reload())
      .catch(() =>
        setError(rg.member ? t.serverResourceGroupsLeaveError : t.serverResourceGroupsJoinError),
      )
      .finally(() => setBusyId(null));
  };

  if (pending) {
    return (
      <Panel titleId="server-rg-heading" title={t.serverResourceGroupsTitle}>
        <Typography color="text.secondary">{t.loading}</Typography>
      </Panel>
    );
  }

  return (
    <Panel titleId="server-rg-heading" title={t.serverResourceGroupsTitle}>
      {error !== '' && (
        <Typography color="error.main" sx={{ mb: 1 }}>
          {error}
        </Typography>
      )}
      {groups.length === 0 ? (
        <Typography color="text.secondary">{t.serverResourceGroupsEmpty}</Typography>
      ) : (
        <Table size="small">
          <TableHead>
            <TableRow>
              <TableCell>{t.serverResourceGroupsColGroup}</TableCell>
              <TableCell align="right">{t.serverResourceGroupsColMember}</TableCell>
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
