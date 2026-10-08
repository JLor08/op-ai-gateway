// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useEffect, useRef, useState } from 'react';
import {
  Alert,
  AlertTitle,
  Box,
  Button,
  CircularProgress,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableRow,
  Typography,
} from '@mui/material';
import RefreshIcon from '@mui/icons-material/Refresh';
import type { VendorAccount, VendorModelsRefresh } from '../api';
import type { Translation } from './shared/types';
import { formatPortalError } from './shared/format';
import { Panel } from './shared/Panel';
import { useToast } from './shared/ToastProvider';

// What the models-refresh outcome reads as. `ok` is the only success. Anything
// else -- `unverifiable`, or a status a newer backend adds -- is NEUTRAL (info):
// the models were left as they were, and it says nothing about the credential,
// so it is deliberately neither success nor error.
function outcomePresentation(
  t: Translation,
  refresh: VendorModelsRefresh,
): { severity: 'success' | 'info'; headline: string } {
  if (refresh.status === 'ok') {
    return { severity: 'success', headline: t.vendorModelsRefreshed(refresh.discovered) };
  }
  return { severity: 'info', headline: t.vendorModelsRefreshUnverifiable };
}

/**
 * The "Models" panel of a vendor account's detail view: the models the account
 * currently serves, and one button that asks the gateway to refresh them from the
 * vendor (POST .../models/refresh, run by the parent through `onRefresh`).
 *
 * Every model shows the vendor's display name and the id clients request it by
 * at the gateway (the account's prefix plus the vendor's id). Both come from the
 * vendor and can hold markup characters (< > " &), so they are only ever rendered
 * as React text children, never as HTML.
 *
 * The outcome is shown inline, styled apart from a failure, like the credential
 * check: a refresh that could not reach the vendor is an ANSWER the gateway
 * delivered (the models are unchanged), while a thrown error means the call
 * itself failed and is a toast. When a rejected refresh token is why nothing
 * could be refreshed, the account now reads needs_reconnect and the outcome adds
 * the reconnect hint. The outcome is only ever the backend's credential-free
 * {status, discovered, detail}: this component never sees, sends or shows a
 * secret.
 *
 * `busy` is the parent's "an account write is running" flag: the refresh and a
 * settings save never overlap, because the answer of one would otherwise overwrite
 * the other's account. The parent keys this panel by the account's id, so
 * another account never inherits an outcome.
 */
export function VendorAccountModels({
  t,
  account,
  busy,
  onRefresh,
}: Readonly<{
  t: Translation;
  account: VendorAccount;
  busy: boolean;
  onRefresh: (id: VendorAccount['id']) => Promise<VendorModelsRefresh>;
}>) {
  const { showError } = useToast();
  const [running, setRunning] = useState(false);
  const [outcome, setOutcome] = useState<VendorModelsRefresh | null>(null);
  // Latest-wins token, bumped on unmount: an answer that arrives after the panel
  // went away (the user left the account) is dropped, with no outcome and no toast.
  // The parent has already taken the account it carried into its list by then.
  const requestRef = useRef(0);
  useEffect(
    () => () => {
      ++requestRef.current;
    },
    [],
  );

  async function refresh() {
    const request = ++requestRef.current;
    setRunning(true);
    // Never leave an earlier outcome up beside a refresh that is running.
    setOutcome(null);
    try {
      const result = await onRefresh(account.id);
      if (request !== requestRef.current) return;
      setOutcome(result);
    } catch (err) {
      if (request !== requestRef.current) return;
      showError(formatPortalError(err, t));
    } finally {
      if (request === requestRef.current) setRunning(false);
    }
  }

  const presentation = outcome === null ? null : outcomePresentation(t, outcome);
  const unverifiable = outcome !== null && outcome.status !== 'ok';
  return (
    <Panel
      titleId="vendor-account-models-heading"
      title={t.vendorModelsTitle}
      subtitle={t.vendorModelsIntro}
      actions={
        <Button
          type="button"
          variant="outlined"
          disabled={busy || running}
          startIcon={
            running ? (
              <CircularProgress size={16} color="inherit" />
            ) : (
              <RefreshIcon fontSize="small" />
            )
          }
          onClick={() => void refresh()}
        >
          {t.vendorModelsRefreshAction}
        </Button>
      }
    >
      <Box sx={{ display: 'grid', gap: 2.25 }}>
        {outcome !== null && presentation !== null && (
          // role="status": a polite live region, so the outcome is announced when it
          // lands and is told apart from the assertive toasts (role="alert").
          <Alert severity={presentation.severity} role="status">
            <AlertTitle sx={unverifiable ? undefined : { mb: 0 }}>
              {presentation.headline}
            </AlertTitle>
            {unverifiable && (
              <Box sx={{ display: 'grid', gap: 0.5 }}>
                {outcome.detail !== '' && (
                  <Typography
                    variant="body2"
                    color="text.secondary"
                    sx={{ overflowWrap: 'anywhere' }}
                  >
                    {t.vendorCheckDetail(outcome.detail)}
                  </Typography>
                )}
                <Typography variant="body2" color="text.secondary">
                  {t.vendorModelsRefreshUnchanged}
                </Typography>
                {account.status === 'needs_reconnect' && (
                  <Typography variant="body2">
                    {t.vendorModelsRefreshReconnect(t.vendorConnectTitle)}
                  </Typography>
                )}
              </Box>
            )}
          </Alert>
        )}
        {account.models.length === 0 ? (
          <Typography color="text.secondary">{t.vendorModelsEmpty}</Typography>
        ) : (
          <Table size="small" aria-label={t.vendorModelsListLabel}>
            <TableHead>
              <TableRow>
                <TableCell>{t.vendorModelsColName}</TableCell>
                <TableCell>{t.vendorModelsColId}</TableCell>
              </TableRow>
            </TableHead>
            <TableBody>
              {account.models.map((model) => (
                <TableRow key={model.gateway_model}>
                  <TableCell sx={{ overflowWrap: 'anywhere' }}>
                    {model.display_name === '' ? '—' : model.display_name}
                  </TableCell>
                  <TableCell
                    sx={{ fontFamily: 'var(--font-mono, monospace)', overflowWrap: 'anywhere' }}
                  >
                    {model.gateway_model}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </Box>
    </Panel>
  );
}
