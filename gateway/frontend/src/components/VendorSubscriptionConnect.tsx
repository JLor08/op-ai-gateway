// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useState, type SubmitEvent } from 'react';
import {
  Accordion,
  AccordionDetails,
  AccordionSummary,
  Box,
  Button,
  Divider,
  TextField,
  Typography,
} from '@mui/material';
import ExpandMoreIcon from '@mui/icons-material/ExpandMore';
import type { VendorAccount } from '../api';
import type { BadgeStatus, PortalApi, Translation } from './shared/types';
import { formatPortalError } from './shared/format';
import { isWebUrl } from './shared/webUrl';
import { Panel } from './shared/Panel';
import { Field } from './shared/Field';
import { StatusChip } from './shared/StatusChip';
import { useToast } from './shared/ToastProvider';
import { VendorDeviceConnect } from './VendorDeviceConnect';

type ConnectBusy = '' | 'import' | 'begin' | 'complete';

const FORM_GRID_SX = { display: 'grid', gridTemplateColumns: 'minmax(260px, 480px)', gap: 2.25 };

// The connection state a subscription account is in, from the credential-free
// DTO: subscription_connected says whether a token set is stored, status says
// whether the gateway still accepts it (needs_reconnect is system-managed, set
// when a token refresh failed).
function connectionBadge(
  t: Translation,
  account: VendorAccount,
): { status: BadgeStatus; label: string } {
  if (!account.subscription_connected) {
    return { status: 'disabled', label: t.vendorConnectStatusNotConnected };
  }
  if (account.status === 'needs_reconnect') {
    return { status: 'watch', label: t.vendorAccountStatusNeedsReconnect };
  }
  return { status: 'active', label: t.vendorConnectStatusConnected };
}

// The <input type="datetime-local"> value is a LOCAL wall-clock time without an
// offset; the backend wants an RFC 3339 instant. Empty or unparseable -> omit
// the field (the backend then treats the expiry as unknown).
function expiryToRfc3339(local: string): string | undefined {
  if (local === '') return undefined;
  const parsed = new Date(local);
  return Number.isNaN(parsed.getTime()) ? undefined : parsed.toISOString();
}

/**
 * The "connect a subscription" panel of a vendor account's detail view. Three
 * ways to attach the account to a consumer subscription (all owner-only POSTs,
 * see api/vendorAccounts.ts):
 *
 *  - Browser sign-in (OAuth code-paste): "Connect" begins the flow and returns
 *    the vendor sign-in URL; the user opens it in a NEW tab (a separate click,
 *    so a popup blocker never swallows it), signs in there, and pastes back the
 *    code the vendor shows (a bare code, "code#state" or a whole callback URL --
 *    the backend parses all three). A refused paste keeps the form, because the
 *    backend keeps the pending connect: the user simply retries.
 *  - Device code (OpenAI accounts ONLY -- Anthropic has no device login): see
 *    VendorDeviceConnect. Works for a remote gateway, and polls until approved.
 *  - Token import: tokens the user already holds, with a short guide to where
 *    the Claude Code / Codex command-line clients keep them.
 *
 * Tokens are WRITE-ONLY secrets: the DTO only says `subscription_connected`,
 * the token inputs are masked, never pre-filled, and cleared on success. The
 * component owns all of that state, so leaving the detail view (which unmounts
 * it) drops anything typed -- a secret is never carried across views.
 */
export function VendorSubscriptionConnect({
  t,
  account,
  api,
  onConnected,
}: Readonly<{
  t: Translation;
  account: VendorAccount;
  api: Pick<
    PortalApi,
    | 'connectVendorAccountImport'
    | 'beginVendorAccountConnect'
    | 'completeVendorAccountConnect'
    | 'beginVendorAccountDeviceConnect'
    | 'pollVendorAccountDeviceConnect'
    | 'vendorAccount'
  >;
  /** The credential-free account a successful connect answered with. */
  onConnected: (updated: VendorAccount) => void;
}>) {
  const { showError, showSuccess } = useToast();
  const [busy, setBusy] = useState<ConnectBusy>('');

  // Token import.
  const [accessToken, setAccessToken] = useState('');
  const [refreshToken, setRefreshToken] = useState('');
  const [expiresAt, setExpiresAt] = useState('');

  // Browser sign-in: the URL the begin call returned (empty = not begun) and
  // the pasted code.
  const [authorizeUrl, setAuthorizeUrl] = useState('');
  const [code, setCode] = useState('');

  // Bumped by every successful connect: the device-code section is keyed by it,
  // so a login another method just completed also stops a device poll that is
  // still waiting (remounting runs its cleanup) instead of racing it.
  const [connectEpoch, setConnectEpoch] = useState(0);

  const connection = connectionBadge(t, account);
  const connected = account.subscription_connected;

  function finishConnected(updated: VendorAccount) {
    setAccessToken('');
    setRefreshToken('');
    setExpiresAt('');
    setAuthorizeUrl('');
    setCode('');
    setConnectEpoch((epoch) => epoch + 1);
    onConnected(updated);
    showSuccess(t.vendorConnectSuccess);
  }

  async function submitImport(event: SubmitEvent<HTMLFormElement>) {
    event.preventDefault();
    setBusy('import');
    try {
      const refresh = refreshToken.trim();
      const expiry = expiryToRfc3339(expiresAt);
      const updated = await api.connectVendorAccountImport(account.id, {
        access_token: accessToken,
        ...(refresh !== '' ? { refresh_token: refresh } : {}),
        ...(expiry !== undefined ? { expires_at: expiry } : {}),
      });
      finishConnected(updated);
    } catch (err) {
      // The typed tokens stay so the user can correct and retry.
      showError(formatPortalError(err, t));
    } finally {
      setBusy('');
    }
  }

  async function beginConnect() {
    setBusy('begin');
    try {
      const { authorize_url } = await api.beginVendorAccountConnect(account.id);
      if (!isWebUrl(authorize_url)) {
        showError(t.errorRequestFailed);
        return;
      }
      setAuthorizeUrl(authorize_url);
      // A new begin replaces the backend's pending entry: a code pasted from
      // the previous sign-in no longer matches it.
      setCode('');
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setBusy('');
    }
  }

  async function completeConnect(event: SubmitEvent<HTMLFormElement>) {
    event.preventDefault();
    setBusy('complete');
    try {
      const updated = await api.completeVendorAccountConnect(account.id, code.trim());
      finishConnected(updated);
    } catch (err) {
      // The backend keeps the pending connect on a refused paste, so the form
      // (and the pasted value) stays for a retry.
      showError(formatPortalError(err, t));
    } finally {
      setBusy('');
    }
  }

  return (
    <Panel
      titleId="vendor-account-connect-heading"
      title={t.vendorConnectTitle}
      subtitle={t.vendorConnectIntro}
    >
      <Box sx={{ display: 'flex', alignItems: 'center', gap: 1.5, mb: 1 }}>
        <Typography>{t.vendorConnectStatusLabel}</Typography>
        <StatusChip status={connection.status} label={connection.label} />
      </Box>
      {account.status === 'needs_reconnect' && connected && (
        <Typography color="text.secondary" variant="body2">
          {t.vendorConnectNeedsReconnectNote}
        </Typography>
      )}
      {connected && account.status !== 'needs_reconnect' && (
        <Typography color="text.secondary" variant="body2">
          {t.vendorConnectReconnectNote}
        </Typography>
      )}

      <Divider sx={{ my: 2.5 }} />

      <Box component="section" aria-labelledby="vendor-connect-browser-heading">
        <Typography component="h3" variant="subtitle1" id="vendor-connect-browser-heading">
          {t.vendorConnectBrowserTitle}
        </Typography>
        <Typography color="text.secondary" variant="body2" sx={{ mb: 2 }}>
          {t.vendorConnectBrowserIntro}
        </Typography>
        <Box sx={FORM_GRID_SX}>
          <Box>
            <Button
              type="button"
              variant={authorizeUrl === '' ? 'contained' : 'outlined'}
              disabled={busy !== ''}
              onClick={() => void beginConnect()}
            >
              {authorizeUrl === '' ? t.vendorConnectBeginAction : t.vendorConnectBeginAgainAction}
            </Button>
          </Box>
          {authorizeUrl !== '' && (
            <>
              <Box>
                <Typography component="h4" variant="subtitle2">
                  {t.vendorConnectStepOpen}
                </Typography>
                <Typography color="text.secondary" variant="body2" sx={{ mb: 1 }}>
                  {t.vendorConnectStepOpenNote}
                </Typography>
                <Button
                  type="button"
                  variant="contained"
                  onClick={() => window.open(authorizeUrl, '_blank', 'noopener')}
                >
                  {t.vendorConnectOpenLogin}
                </Button>
              </Box>
              <Box component="form" onSubmit={completeConnect} sx={FORM_GRID_SX}>
                <Box>
                  <Typography component="h4" variant="subtitle2" sx={{ mb: 1 }}>
                    {t.vendorConnectStepPaste}
                  </Typography>
                  <Field
                    id="vendor-account-connect-code"
                    label={t.vendorConnectCodeLabel}
                    value={code}
                    onChange={(e) => setCode(e.target.value)}
                    autoComplete="off"
                    helperText={t.vendorConnectCodeNote}
                    required
                  />
                </Box>
                <Box>
                  <Button
                    type="submit"
                    variant="contained"
                    disabled={busy !== '' || code.trim() === ''}
                  >
                    {t.vendorConnectCompleteAction}
                  </Button>
                </Box>
              </Box>
            </>
          )}
        </Box>
      </Box>

      {/* The device-code flow is OpenAI-only: Anthropic offers no device login. */}
      {account.vendor === 'openai' && (
        <>
          <Divider sx={{ my: 2.5 }} />
          <VendorDeviceConnect
            key={connectEpoch}
            t={t}
            account={account}
            api={api}
            onConnected={finishConnected}
          />
        </>
      )}

      <Divider sx={{ my: 2.5 }} />

      <Box component="section" aria-labelledby="vendor-connect-import-heading">
        <Typography component="h3" variant="subtitle1" id="vendor-connect-import-heading">
          {t.vendorConnectImportTitle}
        </Typography>
        <Typography color="text.secondary" variant="body2" sx={{ mb: 2 }}>
          {t.vendorConnectImportIntro}
        </Typography>
        <Accordion
          disableGutters
          variant="outlined"
          slotProps={{ transition: { unmountOnExit: true } }}
          sx={{ mb: 2, '&::before': { display: 'none' } }}
        >
          <AccordionSummary
            expandIcon={<ExpandMoreIcon />}
            aria-controls="vendor-connect-guide-content"
            id="vendor-connect-guide-header"
          >
            <Typography variant="subtitle2">{t.vendorConnectGuideTitle}</Typography>
          </AccordionSummary>
          <AccordionDetails id="vendor-connect-guide-content">
            <Typography component="h4" variant="subtitle2">
              {t.vendorConnectGuideClaudeTitle}
            </Typography>
            <Typography color="text.secondary" variant="body2" sx={{ mb: 1.5 }}>
              {t.vendorConnectGuideClaudeBody}
            </Typography>
            <Typography component="h4" variant="subtitle2">
              {t.vendorConnectGuideCodexTitle}
            </Typography>
            <Typography color="text.secondary" variant="body2">
              {t.vendorConnectGuideCodexBody}
            </Typography>
          </AccordionDetails>
        </Accordion>
        <Box component="form" onSubmit={submitImport} sx={FORM_GRID_SX}>
          <Field
            id="vendor-account-connect-access-token"
            type="password"
            label={t.vendorConnectAccessTokenLabel}
            value={accessToken}
            onChange={(e) => setAccessToken(e.target.value)}
            autoComplete="new-password"
            required
          />
          <Field
            id="vendor-account-connect-refresh-token"
            type="password"
            label={t.vendorConnectRefreshTokenLabel}
            value={refreshToken}
            onChange={(e) => setRefreshToken(e.target.value)}
            autoComplete="new-password"
          />
          <TextField
            id="vendor-account-connect-expires-at"
            type="datetime-local"
            size="small"
            fullWidth
            label={t.vendorConnectExpiresAtLabel}
            value={expiresAt}
            onChange={(e) => setExpiresAt(e.target.value)}
            helperText={t.vendorConnectExpiresAtNote}
            slotProps={{ inputLabel: { shrink: true } }}
          />
          <Box>
            <Button
              type="submit"
              variant="contained"
              disabled={busy !== '' || accessToken.trim() === ''}
            >
              {t.vendorConnectImportAction}
            </Button>
          </Box>
        </Box>
      </Box>
    </Panel>
  );
}
