// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useEffect, useRef, useState } from 'react';
import { Alert, AlertTitle, Box, Button, CircularProgress, Typography } from '@mui/material';
import RefreshIcon from '@mui/icons-material/Refresh';
import type { VendorAccount, VendorAccountUsage, VendorUsageRefresh } from '../api';
import type { PortalApi, Translation } from './shared/types';
import { formatPortalError } from './shared/format';
import { Panel } from './shared/Panel';
import { useLatestFetch } from './shared/useLatestFetch';
import { useToast } from './shared/ToastProvider';
import {
  hasSpendControl,
  VendorAccountUsageRows,
  visibleUsageRows,
} from './VendorAccountUsageRows';

// Vendors whose stored api key the gateway can use to ask for usage/credits
// (mirrors the routing preset's UsageVia != ""): OpenRouter and, best effort, Kilo.
// x.ai, Google and a user-supplied OpenAI-compatible endpoint offer no such query.
const API_KEY_USAGE_VENDORS: ReadonlySet<string> = new Set(['openrouter', 'kilo']);

/**
 * Whether the gateway can ASK the vendor for this account's usage, as opposed to
 * only learning it passively from the responses it serves: a connected OpenAI
 * subscription (the account whose usage endpoint the gateway pulls) and an api-key
 * account of a vendor with a usage/credits query (OpenRouter, Kilo) whose key is
 * stored. Every other account -- another vendor's api key, an Anthropic
 * subscription -- has no active pull (the backend answers "unsupported"), so it
 * gets neither the refresh button nor the on-view refresh.
 */
export function hasActiveUsagePull(
  account: Pick<VendorAccount, 'vendor' | 'auth_type' | 'subscription_connected' | 'api_key_set'>,
): boolean {
  if (account.auth_type === 'subscription') {
    return account.vendor === 'openai' && account.subscription_connected;
  }
  return account.api_key_set && API_KEY_USAGE_VENDORS.has(account.vendor);
}

// What a usage-refresh answer reads as. ok and fresh (the server's TTL skipped the
// vendor call) are both a success: the figures are as current as they can be.
// unsupported says the account has no active pull. Anything else -- unverifiable, or
// a status a newer backend adds -- is NEUTRAL (info): the stored figures were kept
// and it says nothing about the credential, so it is neither success nor error. The
// answer is keyed by `status` only; the backend's English `detail` is never shown.
function outcomePresentation(
  t: Translation,
  refresh: VendorUsageRefresh,
): { severity: 'success' | 'info'; headline: string; unchanged: boolean } {
  switch (refresh.status) {
    case 'ok':
    case 'fresh':
      return { severity: 'success', headline: t.vendorUsageRefreshed, unchanged: false };
    case 'unsupported':
      return { severity: 'info', headline: t.vendorUsageRefreshUnsupported, unchanged: false };
    default:
      return { severity: 'info', headline: t.vendorUsageRefreshUnverifiable, unchanged: true };
  }
}

// A snapshot carrying figures the gateway pulled (the spend-control credits, or a
// credit balance / unlimited state such as OpenRouter's and Kilo's), as opposed to
// percentages learned from every served request. The intro sentence follows it: the
// "refreshed with every request" wording would be wrong for these.
function isPulledSnapshot(usage: VendorAccountUsage): boolean {
  return (
    hasSpendControl(usage) || usage.credit_balance !== '' || usage.credit_status === 'unlimited'
  );
}

/**
 * The "Usage & limits" panel of a vendor account's detail view: a titled frame
 * around the account's limits (VendorAccountUsageRows -- the 5-hour and the weekly
 * window as bars with "resets in ...", the credit side), showing only the limits
 * the vendor actually provides (visibleUsageRows). An account whose snapshot has no
 * visible row -- none yet, or one that reported nothing usable, e.g. only a bare
 * `has_credits` flag -- keeps the frame and shows one short empty-state line instead
 * of an empty shell, so the refresh button below stays reachable.
 *
 * `onRefresh` is present only for an account with an active usage pull
 * (hasActiveUsagePull): it puts an "Aktualisieren" button into the frame's actions
 * that asks the parent to refresh now (the parent re-reads the snapshot) and shows
 * the answer inline by its `status`, styled apart from a failure, like the models
 * refresh: an answer the gateway delivered is not an error. A thrown error means the
 * call itself failed and is a toast. Absent, there is no button at all.
 *
 * `pending` is true while the first read of the snapshot is still in flight, so the
 * empty-state line does not flash before the rows arrive. `now` is injectable so the
 * countdowns are deterministic in tests.
 */
export function VendorAccountUsagePanel({
  t,
  usage,
  now = Date.now(),
  pending = false,
  onRefresh,
}: Readonly<{
  t: Translation;
  usage: VendorAccountUsage | null | undefined;
  now?: number;
  pending?: boolean;
  onRefresh?: () => Promise<VendorUsageRefresh>;
}>) {
  const { showError } = useToast();
  const [running, setRunning] = useState(false);
  const [outcome, setOutcome] = useState<VendorUsageRefresh | null>(null);
  // Latest-wins token, bumped on unmount: an answer that arrives after the panel
  // went away (the user left the account) is dropped, with no outcome and no toast.
  const requestRef = useRef(0);
  useEffect(
    () => () => {
      ++requestRef.current;
    },
    [],
  );

  async function refresh() {
    if (!onRefresh) return;
    const request = ++requestRef.current;
    setRunning(true);
    // Never leave an earlier outcome up beside a refresh that is running.
    setOutcome(null);
    try {
      const result = await onRefresh();
      if (request !== requestRef.current) return;
      setOutcome(result);
    } catch (err) {
      if (request !== requestRef.current) return;
      showError(formatPortalError(err, t));
    } finally {
      if (request === requestRef.current) setRunning(false);
    }
  }

  const rowsUsage = usage != null && visibleUsageRows(usage).length > 0 ? usage : null;
  const presentation = outcome === null ? null : outcomePresentation(t, outcome);
  return (
    <Box sx={{ mt: 3 }}>
      <Panel
        titleId="vendor-account-usage-heading"
        title={t.vendorUsageTitle}
        subtitle={
          usage != null && isPulledSnapshot(usage) ? t.vendorUsageIntroSpend : t.vendorUsageIntro
        }
        actions={
          onRefresh && (
            <Button
              type="button"
              variant="outlined"
              disabled={running}
              startIcon={
                running ? (
                  <CircularProgress size={16} color="inherit" />
                ) : (
                  <RefreshIcon fontSize="small" />
                )
              }
              onClick={() => void refresh()}
            >
              {t.vendorUsageRefreshAction}
            </Button>
          )
        }
      >
        <Box sx={{ display: 'grid', gap: 2.25 }}>
          {presentation !== null && (
            // role="status": a polite live region, so the outcome is announced when it
            // lands and is told apart from the assertive toasts (role="alert").
            <Alert severity={presentation.severity} role="status">
              <AlertTitle sx={presentation.unchanged ? undefined : { mb: 0 }}>
                {presentation.headline}
              </AlertTitle>
              {presentation.unchanged && (
                <Typography variant="body2" color="text.secondary">
                  {t.vendorUsageRefreshUnchanged}
                </Typography>
              )}
            </Alert>
          )}
          {rowsUsage !== null ? (
            <VendorAccountUsageRows t={t} usage={rowsUsage} now={now} />
          ) : (
            !pending && <Typography color="text.secondary">{t.vendorUsageEmpty}</Typography>
          )}
        </Box>
      </Panel>
    </Box>
  );
}

/**
 * Fetches the account's usage snapshot and renders the panel. The list rows do
 * carry a snapshot, but the detail view reads the single-account GET so that the
 * panel can re-read it on its own after a refresh. The panel is auxiliary: a failed
 * read leaves the frame with its empty-state line rather than raising a toast on
 * every detail open.
 *
 * `refreshKey` is a counter the parent bumps when something happened that can
 * have changed the snapshot behind this panel's back -- the models refresh pulls
 * the vendor's usage, and so does the refresh button, and neither `account.id` nor
 * `account.updated_at` change with it. A new value re-reads the snapshot; the panel
 * keeps showing the previous one meanwhile instead of blinking out.
 *
 * `onRefresh` is given only for an account with an active usage pull
 * (hasActiveUsagePull). It is the explicit refresh the button runs (the parent
 * forces the pull and bumps `refreshKey`); and its presence also makes the panel fire
 * ONE lazy `refreshUsage` (no `force`) when it mounts -- i.e. when the account's
 * detail view is opened. The server skips the vendor call while its last pull is
 * younger than its TTL, so that is a cheap stored read unless the figures are stale;
 * an answer that actually re-pulled (`ok`) makes the panel read the snapshot again.
 * It is best effort: a failed lazy call leaves the stored snapshot, silently. There
 * is no polling and no loop -- one call per mount.
 */
export function VendorAccountUsage({
  t,
  api,
  accountId,
  refreshKey = 0,
  onRefresh,
}: Readonly<{
  t: Translation;
  api: Pick<PortalApi, 'vendorAccount' | 'refreshUsage'>;
  accountId: VendorAccount['id'];
  refreshKey?: number;
  onRefresh?: (id: VendorAccount['id']) => Promise<VendorUsageRefresh>;
}>) {
  // Bumped when the lazy refresh stored a new snapshot, to read it again.
  const [lazyReads, setLazyReads] = useState(0);
  const { data, status } = useLatestFetch(
    () => api.vendorAccount(accountId).then((account) => account.usage ?? null),
    [api, accountId, refreshKey, lazyReads],
  );

  const activePull = onRefresh !== undefined;
  useEffect(() => {
    if (!activePull) return;
    let cancelled = false;
    api
      .refreshUsage(accountId)
      .then(({ refresh }) => {
        // Only a call that really pulled changed the stored snapshot ("fresh" is
        // the TTL skipping the vendor, so the read already made shows it).
        if (!cancelled && refresh.status === 'ok') setLazyReads((n) => n + 1);
      })
      .catch(() => {
        // Best effort: the stored snapshot stays.
      });
    return () => {
      cancelled = true;
    };
  }, [api, accountId, activePull]);

  return (
    <VendorAccountUsagePanel
      t={t}
      usage={data}
      pending={data === null && (status === 'idle' || status === 'loading')}
      onRefresh={onRefresh && (() => onRefresh(accountId))}
    />
  );
}

/**
 * The detail view's usage block for one account. A subscription that was never
 * connected has served nothing, so it has no usage to show and renders nothing;
 * every other account (an api key, a connected subscription) gets the usage panel.
 * Only an account the gateway can actively ask (hasActiveUsagePull: a connected
 * OpenAI subscription, an OpenRouter or Kilo api key) is handed `onRefresh`, which
 * gives it the refresh button and the one on-view refresh; the others only learn
 * their usage from the requests they serve. The panel is keyed by the account id,
 * so another account never inherits the previous one's snapshot, outcome or
 * on-view refresh.
 *
 * `canRefresh` is false for a viewer the backend would refuse the usage refresh (a
 * co-manager of a shared SUBSCRIPTION account: the pull opens the owner's single-use
 * token): the panel then still shows the stored snapshot, but there is neither the
 * refresh button nor the on-view pull.
 */
export function VendorAccountUsageSection({
  t,
  api,
  account,
  refreshKey,
  onRefresh,
  canRefresh = true,
}: Readonly<{
  t: Translation;
  api: Pick<PortalApi, 'vendorAccount' | 'refreshUsage'>;
  account: VendorAccount;
  refreshKey: number;
  onRefresh: (id: VendorAccount['id']) => Promise<VendorUsageRefresh>;
  canRefresh?: boolean;
}>) {
  const usageCapable = account.auth_type === 'api_key' || account.subscription_connected;
  if (!usageCapable) return null;
  return (
    <VendorAccountUsage
      key={account.id}
      t={t}
      api={api}
      accountId={account.id}
      refreshKey={refreshKey}
      onRefresh={canRefresh && hasActiveUsagePull(account) ? onRefresh : undefined}
    />
  );
}
