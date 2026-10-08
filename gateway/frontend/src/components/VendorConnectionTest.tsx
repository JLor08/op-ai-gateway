// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useEffect, useRef, useState } from 'react';
import { Alert, AlertTitle, Box, Button, CircularProgress, Typography } from '@mui/material';
import type { AlertColor } from '@mui/material';
import type { VendorAccount, VendorConnectionCheck } from '../api';
import type { PortalApi, Translation } from './shared/types';
import { formatPortalError } from './shared/format';
import { Panel } from './shared/Panel';
import { useToast } from './shared/ToastProvider';

// How each verdict reads. The headline is localized by `status`: the backend's
// `detail` is an English technical phrase and never stands in for it. An
// unverifiable check is NEUTRAL (info), deliberately neither success nor error:
// it says nothing about the credential. A status this build does not know (a
// newer backend) falls back to that neutral reading rather than to a verdict it
// cannot vouch for.
function verdictPresentation(
  t: Translation,
  status: string,
): { severity: AlertColor; headline: string } {
  switch (status) {
    case 'valid':
      return { severity: 'success', headline: t.vendorCheckValid };
    case 'invalid':
      return { severity: 'error', headline: t.vendorCheckInvalid };
    default:
      return { severity: 'info', headline: t.vendorCheckUnverifiable };
  }
}

/**
 * The "test connection" panel of a vendor account's detail view: one button that
 * asks the gateway to check, with the vendor, whether the account's stored
 * credential is accepted (POST .../check), and the verdict it answers with.
 *
 * It checks the CREDENTIAL only, never whether a particular model is served, and
 * the panel says so: a chat that fails while this reads "valid" points at the
 * model, not the login. That is why a verdict is shown inline and styled apart
 * from a failure (an error here is a toast, never a verdict): a rejected
 * credential is an answer the check delivered, a thrown error means the check
 * itself could not run.
 *
 * The verdict is only ever the backend's credential-free {status, detail}: this
 * component never sees, sends or shows a secret. A verdict describes the
 * credential as it was when it was asked, so the parent keys this panel by the
 * account's id and updated_at: saving or reconnecting the account remounts it,
 * which drops a now stale verdict (and the answer of a check still in flight).
 */
export function VendorConnectionTest({
  t,
  api,
  accountId,
}: Readonly<{
  t: Translation;
  api: Pick<PortalApi, 'testConnection'>;
  accountId: VendorAccount['id'];
}>) {
  const { showError } = useToast();
  const [busy, setBusy] = useState(false);
  const [check, setCheck] = useState<VendorConnectionCheck | null>(null);
  // Latest-wins token, bumped on unmount: an answer that arrives after the panel
  // went away (the user left the account, or it was saved) is dropped, with no
  // verdict and no toast.
  const requestRef = useRef(0);
  useEffect(
    () => () => {
      ++requestRef.current;
    },
    [],
  );

  async function runCheck() {
    const request = ++requestRef.current;
    setBusy(true);
    // Never leave an earlier verdict up beside a check that is running.
    setCheck(null);
    try {
      const result = await api.testConnection(accountId);
      if (request !== requestRef.current) return;
      setCheck(result);
    } catch (err) {
      if (request !== requestRef.current) return;
      showError(formatPortalError(err, t));
    } finally {
      if (request === requestRef.current) setBusy(false);
    }
  }

  const presentation = check === null ? null : verdictPresentation(t, check.status);
  return (
    <Panel
      titleId="vendor-account-check-heading"
      title={t.vendorCheckTitle}
      subtitle={t.vendorCheckIntro}
    >
      <Box sx={{ display: 'grid', gridTemplateColumns: 'minmax(260px, 480px)', gap: 2.25 }}>
        <Box>
          <Button
            type="button"
            variant="contained"
            disabled={busy}
            startIcon={busy ? <CircularProgress size={16} color="inherit" /> : undefined}
            onClick={() => void runCheck()}
          >
            {t.vendorCheckAction}
          </Button>
        </Box>
        {check !== null && presentation !== null && (
          // role="status": a polite live region, so the verdict is announced when it
          // lands and is told apart from the assertive toasts (role="alert").
          <Alert severity={presentation.severity} role="status">
            <AlertTitle sx={check.detail === '' ? { mb: 0 } : undefined}>
              {presentation.headline}
            </AlertTitle>
            {check.detail !== '' && (
              <Typography variant="body2" color="text.secondary" sx={{ overflowWrap: 'anywhere' }}>
                {t.vendorCheckDetail(check.detail)}
              </Typography>
            )}
          </Alert>
        )}
      </Box>
    </Panel>
  );
}
