// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useRef, useState, type ChangeEvent, type SubmitEvent } from 'react';
import {
  Accordion,
  AccordionDetails,
  AccordionSummary,
  Alert,
  Box,
  Button,
  Divider,
  TextField,
  Typography,
} from '@mui/material';
import ExpandMoreIcon from '@mui/icons-material/ExpandMore';
import UploadFileIcon from '@mui/icons-material/UploadFile';
import type { ConnectVendorAccountImportRequest, VendorAccount } from '../api';
import type { BadgeStatus, MessageKey, PortalApi, Translation } from './shared/types';
import { formatPortalError } from './shared/format';
import { isWebUrl } from './shared/webUrl';
import { Panel } from './shared/Panel';
import { Field } from './shared/Field';
import { StatusChip } from './shared/StatusChip';
import { useToast } from './shared/ToastProvider';
import { vendorLabel } from './shared/vendorLabel';
import {
  isParsedCredentialError,
  parseCredentialFile,
  type ParsedCredential,
  type ParsedCredentialError,
  type ParsedCredentialErrorCode,
} from './parseCredentialFile';
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

// The inverse, to show an extracted RFC 3339 expiry in the datetime-local field:
// the instant as a LOCAL wall-clock "YYYY-MM-DDTHH:mm". The field only displays it
// (minutes are all it holds); a file import submits the exact instant, not this.
function rfc3339ToLocalInput(instant: string): string {
  const date = new Date(instant);
  if (Number.isNaN(date.getTime())) return '';
  const pad = (n: number) => String(n).padStart(2, '0');
  return (
    `${String(date.getFullYear()).padStart(4, '0')}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}` +
    `T${pad(date.getHours())}:${pad(date.getMinutes())}`
  );
}

function importRequest(
  accessToken: string,
  refreshToken: string,
  expiresAt: string | undefined,
): ConnectVendorAccountImportRequest {
  const refresh = refreshToken.trim();
  return {
    access_token: accessToken,
    ...(refresh !== '' ? { refresh_token: refresh } : {}),
    ...(expiresAt !== undefined ? { expires_at: expiresAt } : {}),
  };
}

/**
 * The largest credential file the picker reads. A Codex auth.json or Claude Code
 * .credentials.json is a few KiB; the cap keeps a wrong (huge) pick from being
 * pulled into the page. The size is checked BEFORE the file is read.
 */
const MAX_CREDENTIAL_FILE_BYTES = 1024 * 1024;

// Each parser error code's localized text. A Record, so a code the parser gains is
// a compile error here until it has a message in both locales.
const FILE_ERROR_MESSAGE: Record<ParsedCredentialErrorCode, MessageKey> = {
  not_json: 'vendorConnectFileErrorNotJson',
  not_object: 'vendorConnectFileErrorNotObject',
  unrecognised: 'vendorConnectFileErrorUnrecognised',
  ambiguous: 'vendorConnectFileErrorAmbiguous',
  claude_no_access_token: 'vendorConnectFileErrorClaudeNoAccessToken',
  codex_id_token_only: 'vendorConnectFileErrorCodexIdTokenOnly',
  codex_no_access_token: 'vendorConnectFileErrorCodexNoAccessToken',
};

// The parser's English `error` is only the fallback for a code this table does not
// know (a stale build); it is token-free by construction, so showing it leaks nothing.
function fileErrorText(t: Translation, error: ParsedCredentialError): string {
  const key: MessageKey | undefined = FILE_ERROR_MESSAGE[error.code];
  return key === undefined ? error.error : t[key];
}

function readFileText(file: File): Promise<string | ArrayBuffer | null> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve(reader.result);
    reader.onerror = () => reject(reader.error ?? new Error('file read failed'));
    reader.onabort = () => reject(new Error('file read aborted'));
    reader.readAsText(file);
  });
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
 *    the Claude Code / Codex command-line clients keep them. The user can paste
 *    the fields, or pick the client's credential file: it is read and parsed in
 *    the browser (parseCredentialFile), the right fields are extracted and the
 *    SAME import is submitted. The raw file never leaves the page.
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
  // Why the chosen credential file could not be imported (localized; never carries
  // any of the file's content), shown inline beside the picker.
  const [fileError, setFileError] = useState('');
  const fileInputRef = useRef<HTMLInputElement>(null);

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
    setFileError('');
    setAuthorizeUrl('');
    setCode('');
    setConnectEpoch((epoch) => epoch + 1);
    onConnected(updated);
    showSuccess(t.vendorConnectSuccess);
  }

  // The one place the token import is sent, from the form and from a credential file.
  async function runImport(body: ConnectVendorAccountImportRequest) {
    setBusy('import');
    try {
      const updated = await api.connectVendorAccountImport(account.id, body);
      finishConnected(updated);
    } catch (err) {
      // The tokens stay in the form so the user can correct and retry.
      showError(formatPortalError(err, t));
    } finally {
      setBusy('');
    }
  }

  async function submitImport(event: SubmitEvent<HTMLFormElement>) {
    event.preventDefault();
    await runImport(importRequest(accessToken, refreshToken, expiryToRfc3339(expiresAt)));
  }

  // Reads and parses the chosen file in the browser. On a problem it sets the
  // inline error and answers undefined; it never throws.
  async function readCredential(file: File): Promise<ParsedCredential | undefined> {
    let text: string | ArrayBuffer | null;
    try {
      text = await readFileText(file);
    } catch {
      setFileError(t.vendorConnectFileReadFailed);
      return undefined;
    }
    const parsed = parseCredentialFile(file.name, text);
    if (isParsedCredentialError(parsed)) {
      setFileError(fileErrorText(t, parsed));
      return undefined;
    }
    // The vendor comes from the file's CONTENT; importing it into the other
    // vendor's account would only be refused (or, worse, accepted and broken).
    if (parsed.vendor !== account.vendor) {
      setFileError(
        t.vendorConnectFileVendorMismatch(
          vendorLabel(t, parsed.vendor),
          vendorLabel(t, account.vendor),
        ),
      );
      return undefined;
    }
    return parsed;
  }

  async function importFile(event: ChangeEvent<HTMLInputElement>) {
    const file = event.target.files?.[0];
    // Emptied so that choosing the same file again (after fixing it) still fires.
    event.target.value = '';
    if (file === undefined) return;
    setFileError('');
    if (file.size > MAX_CREDENTIAL_FILE_BYTES) {
      setFileError(t.vendorConnectFileTooLarge);
      return;
    }
    setBusy('import');
    const credential = await readCredential(file);
    if (credential === undefined) {
      setBusy('');
      return;
    }
    // Show what was extracted, then submit it through the same import as the form.
    const refresh = credential.refreshToken ?? '';
    setAccessToken(credential.accessToken);
    setRefreshToken(refresh);
    setExpiresAt(
      credential.expiresAt === undefined ? '' : rfc3339ToLocalInput(credential.expiresAt),
    );
    await runImport(importRequest(credential.accessToken, refresh, credential.expiresAt));
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
        <Box sx={{ ...FORM_GRID_SX, mb: 2 }}>
          <Box>
            <Button
              type="button"
              variant="outlined"
              startIcon={<UploadFileIcon />}
              disabled={busy !== ''}
              onClick={() => fileInputRef.current?.click()}
            >
              {t.vendorConnectFileAction}
            </Button>
            <input
              ref={fileInputRef}
              type="file"
              accept=".json,application/json"
              hidden
              tabIndex={-1}
              aria-label={t.vendorConnectFileAction}
              onChange={(event) => void importFile(event)}
            />
            <Typography color="text.secondary" variant="body2" sx={{ mt: 1 }}>
              {t.vendorConnectFileNote}
            </Typography>
          </Box>
          {fileError !== '' && <Alert severity="error">{fileError}</Alert>}
          <Typography color="text.secondary" variant="body2">
            {t.vendorConnectManualHint}
          </Typography>
        </Box>
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
