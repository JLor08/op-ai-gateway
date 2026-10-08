// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useCallback, useEffect, useRef, useState } from 'react';
import { Alert, Box, Button, CircularProgress, Typography } from '@mui/material';
import { PortalApiError, type VendorAccount } from '../api';
import type { PortalApi, Translation } from './shared/types';
import { formatPortalError } from './shared/format';
import { isWebUrl } from './shared/webUrl';
import { useToast } from './shared/ToastProvider';

/** Time between two backend polls of a begun device login. */
export const DEVICE_POLL_INTERVAL_MS = 5_000;
/**
 * How long the portal waits for the user to approve the code before it gives up.
 * The backend drops a pending login after 15 minutes too (the Codex CLI polls
 * for about as long), so polling any further would only answer "expired".
 */
export const DEVICE_POLL_TIMEOUT_MS = 15 * 60 * 1_000;

type Phase = 'idle' | 'starting' | 'waiting';

const COLUMN_SX = { display: 'grid', gridTemplateColumns: 'minmax(260px, 480px)', gap: 2 };

/**
 * Whether a failed poll is a blip worth riding out. The backend KEEPS the
 * pending login on a transient vendor failure (502
 * vendor_account.connect_upstream_failed), so the next poll simply retries; the
 * same goes for a proxy-level 502/503/504 and for the browser itself failing to
 * reach the portal (fetch rejects with a plain error, not a PortalApiError). A
 * poll loop runs unattended for up to 15 minutes, and one blip -- even right
 * after the user approved the code -- must not force a full restart.
 *
 * Everything else is terminal: a vendor refusal (400 connect_rejected -- the
 * backend dropped the pending login), an expired or unknown login (400
 * device_connect_state), a disabled module (409), an expired session (401), ...
 */
function isTransientPollError(err: unknown): boolean {
  if (!(err instanceof PortalApiError)) return true;
  return (
    err.code === 'vendor_account.connect_upstream_failed' ||
    err.status === 502 ||
    err.status === 503 ||
    err.status === 504
  );
}

/**
 * The "device code" connect method of a subscription account (OpenAI only --
 * Anthropic has no device login; the parent only mounts this for `openai`).
 * Unlike the code-paste flow it needs no browser callback the gateway can see,
 * so it works for a REMOTE gateway too:
 *
 *  1. "Start" begins the login and the portal shows the `user_code` (the public
 *     pairing code, NOT a secret) plus the vendor's verification page, which
 *     opens in a NEW tab on its own click (a popup after an async call would be
 *     blocked).
 *  2. The portal then polls the backend every few seconds (a chained timeout,
 *     so a slow poll never overlaps the next one) until the user has approved
 *     the code there. The poll is resilient: it keeps going on
 *     `{connected:false}` and on a transient failure, and stops on
 *     `{connected:true}` (the account is re-read and handed to `onConnected`),
 *     on a terminal error (shown, then the start button is back), or after
 *     DEVICE_POLL_TIMEOUT_MS (told so, start button back).
 *
 * Every timer and in-flight step belongs to one "run": starting again, cancelling
 * or unmounting (leaving the detail view) invalidates the run, so a late response
 * is dropped and no timer leaks. No token ever reaches this component -- the
 * poll answers only `{connected}`.
 */
export function VendorDeviceConnect({
  t,
  account,
  api,
  onConnected,
}: Readonly<{
  t: Translation;
  account: VendorAccount;
  api: Pick<
    PortalApi,
    'beginVendorAccountDeviceConnect' | 'pollVendorAccountDeviceConnect' | 'vendorAccount'
  >;
  /** The credential-free account, re-read after the poll reported it connected. */
  onConnected: (updated: VendorAccount) => void;
}>) {
  const { showError } = useToast();
  const [phase, setPhase] = useState<Phase>('idle');
  const [device, setDevice] = useState<{ userCode: string; verificationUrl: string } | null>(null);
  // The last poll failed transiently: the portal is still trying.
  const [retrying, setRetrying] = useState(false);
  const [timedOut, setTimedOut] = useState(false);

  // The current run's id; every async step compares it before touching state.
  const runRef = useRef(0);
  const timerRef = useRef<number | undefined>(undefined);
  // The latest account, for the fallback that must not resurrect a stale copy.
  const accountRef = useRef(account);
  useEffect(() => {
    accountRef.current = account;
  }, [account]);

  // Invalidate the current run and drop its pending poll timer.
  const stop = useCallback(() => {
    runRef.current += 1;
    window.clearTimeout(timerRef.current);
    timerRef.current = undefined;
  }, []);
  // Leaving the detail view (unmount) must leave no timer behind.
  useEffect(() => stop, [stop]);

  function backToIdle() {
    stop();
    setPhase('idle');
    setDevice(null);
    setRetrying(false);
  }

  function scheduleNextPoll(run: number, startedAt: number) {
    timerRef.current = window.setTimeout(() => {
      void pollOnce(run, startedAt);
    }, DEVICE_POLL_INTERVAL_MS);
  }

  async function pollOnce(run: number, startedAt: number) {
    timerRef.current = undefined;
    let connected = false;
    try {
      connected = (await api.pollVendorAccountDeviceConnect(account.id)).connected;
      if (run !== runRef.current) return;
      setRetrying(false);
    } catch (err) {
      if (run !== runRef.current) return;
      if (!isTransientPollError(err)) {
        backToIdle();
        showError(formatPortalError(err, t));
        return;
      }
      setRetrying(true);
    }
    if (connected) {
      await finish(run);
      return;
    }
    // Evaluated only between polls, so an approval that lands on the last poll
    // is never thrown away.
    if (Date.now() - startedAt >= DEVICE_POLL_TIMEOUT_MS) {
      backToIdle();
      setTimedOut(true);
      return;
    }
    scheduleNextPoll(run, startedAt);
  }

  // The poll only says {connected:true}: re-read the account so the view shows
  // what the backend stored. If that read fails the account IS connected anyway
  // (the poll said so), so fall back to what a connect writes: connected + active.
  async function finish(run: number) {
    let updated: VendorAccount;
    try {
      updated = await api.vendorAccount(account.id);
    } catch {
      updated = { ...accountRef.current, subscription_connected: true, status: 'active' };
    }
    if (run !== runRef.current) return;
    backToIdle();
    onConnected(updated);
  }

  async function start() {
    stop();
    const run = runRef.current;
    setPhase('starting');
    setTimedOut(false);
    setRetrying(false);
    setDevice(null);
    try {
      const begun = await api.beginVendorAccountDeviceConnect(account.id);
      if (run !== runRef.current) return;
      if (!isWebUrl(begun.verification_url)) {
        backToIdle();
        showError(t.errorRequestFailed);
        return;
      }
      setDevice({ userCode: begun.user_code, verificationUrl: begun.verification_url });
      setPhase('waiting');
      scheduleNextPoll(run, Date.now());
    } catch (err) {
      if (run !== runRef.current) return;
      backToIdle();
      showError(formatPortalError(err, t));
    }
  }

  return (
    <Box component="section" aria-labelledby="vendor-connect-device-heading">
      <Typography component="h3" variant="subtitle1" id="vendor-connect-device-heading">
        {t.vendorConnectDeviceTitle}
      </Typography>
      <Typography color="text.secondary" variant="body2" sx={{ mb: 2 }}>
        {t.vendorConnectDeviceIntro}
      </Typography>
      {timedOut && (
        <Alert severity="warning" sx={{ mb: 2 }}>
          {t.vendorConnectDeviceTimedOut}
        </Alert>
      )}
      {device === null ? (
        <Button
          type="button"
          variant="contained"
          disabled={phase === 'starting'}
          onClick={() => void start()}
        >
          {t.vendorConnectDeviceStartAction}
        </Button>
      ) : (
        <Box sx={COLUMN_SX}>
          <Box>
            <Typography component="h4" variant="subtitle2" id="vendor-connect-device-code-label">
              {t.vendorConnectDeviceCodeLabel}
            </Typography>
            {/* The pairing code is public by design (it only pairs the browser
                approval with this login): show it big and easy to select. */}
            <Typography
              component="p"
              variant="h4"
              aria-labelledby="vendor-connect-device-code-label"
              sx={{ fontFamily: 'monospace', letterSpacing: '0.12em', userSelect: 'all', my: 0.5 }}
            >
              {device.userCode}
            </Typography>
          </Box>
          <Typography color="text.secondary" variant="body2">
            {t.vendorConnectDeviceInstructions}
          </Typography>
          <Box>
            <Button
              type="button"
              variant="contained"
              onClick={() => window.open(device.verificationUrl, '_blank', 'noopener')}
            >
              {t.vendorConnectDeviceOpenAction}
            </Button>
            {/* Also as text: the page can be opened on another device. */}
            <Typography
              variant="caption"
              color="text.secondary"
              sx={{ display: 'block', mt: 0.5, wordBreak: 'break-all' }}
            >
              {device.verificationUrl}
            </Typography>
          </Box>
          <Box role="status" sx={{ display: 'flex', alignItems: 'center', gap: 1 }}>
            <CircularProgress size={16} />
            <Typography color="text.secondary" variant="body2">
              {retrying ? t.vendorConnectDeviceRetrying : t.vendorConnectDeviceWaiting}
            </Typography>
          </Box>
          <Box sx={{ display: 'flex', gap: 1.5 }}>
            <Button type="button" variant="outlined" onClick={() => void start()}>
              {t.vendorConnectBeginAgainAction}
            </Button>
            <Button type="button" variant="text" color="secondary" onClick={backToIdle}>
              {t.cancel}
            </Button>
          </Box>
        </Box>
      )}
    </Box>
  );
}
