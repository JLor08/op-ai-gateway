// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { Box, LinearProgress, Typography } from '@mui/material';
import type { VendorAccount, VendorAccountUsage } from '../api';
import type { PortalApi, Translation } from './shared/types';
import { Panel } from './shared/Panel';
import { formatCountdown } from './shared/countdown';
import { useLatestFetch } from './shared/useLatestFetch';

// A bar turns amber, then red, as a window fills up. Presentation only: the
// vendors publish no thresholds, so these are just visual warning steps.
const WARNING_PCT = 75;
const CRITICAL_PCT = 90;

function barColor(pct: number): 'primary' | 'warning' | 'error' {
  if (pct >= CRITICAL_PCT) return 'error';
  if (pct >= WARNING_PCT) return 'warning';
  return 'primary';
}

// "Resets in ..." for a window whose reset time is known: the countdown while
// it is still ahead of us, and a note once it has passed (the stored percentage
// then describes the window BEFORE the reset until the next served request
// refreshes the snapshot). No reset time -> no line at all.
function resetLine(t: Translation, resetAt: string | null, now: number): string | null {
  if (!resetAt) return null;
  const at = new Date(resetAt).getTime();
  if (Number.isNaN(at)) return null;
  return at > now ? t.vendorUsageResetsIn(formatCountdown(at - now)) : t.vendorUsageResetPassed;
}

function UsageWindow({
  t,
  label,
  pct,
  resetAt,
  now,
}: Readonly<{
  t: Translation;
  label: string;
  pct: number;
  resetAt: string | null;
  now: number;
}>) {
  // -1 is "never observed", deliberately not a 0 % bar.
  if (pct < 0) {
    return (
      <Box>
        <Typography sx={{ fontWeight: 600 }}>{label}</Typography>
        <Typography variant="body2" color="text.secondary">
          {t.vendorUsageNoData}
        </Typography>
      </Box>
    );
  }
  const shown = Math.min(100, Math.round(pct));
  const reset = resetLine(t, resetAt, now);
  return (
    <Box>
      <Box sx={{ display: 'flex', justifyContent: 'space-between', gap: 2, mb: 0.75 }}>
        <Typography sx={{ fontWeight: 600 }}>{label}</Typography>
        <Typography>{t.vendorUsagePercentUsed(shown)}</Typography>
      </Box>
      <LinearProgress
        variant="determinate"
        value={shown}
        color={barColor(pct)}
        aria-label={label}
        sx={{ height: 8, borderRadius: 4 }}
      />
      {reset !== null && (
        <Typography variant="body2" color="text.secondary" sx={{ mt: 0.75 }}>
          {reset}
        </Typography>
      )}
    </Box>
  );
}

/**
 * The "Usage & limits" panel of a vendor account's detail view: the 5-hour and
 * the weekly window as progress bars with "resets in ..." under them, plus the
 * vendor's credit balance when it reported one. PERCENTAGES ONLY -- neither
 * vendor publishes an absolute cap, so there is deliberately no "N of M".
 *
 * It renders nothing at all without a snapshot, or when the snapshot knows no
 * window yet and carries no credit balance (every percentage -1): an account
 * the gateway has not seen a rate-limit header for has nothing to show. A single
 * unknown window next to a known one reads "No data yet" instead of a 0 % bar.
 * `now` is injectable so the countdowns are deterministic in tests.
 */
export function VendorAccountUsagePanel({
  t,
  usage,
  now = Date.now(),
}: Readonly<{
  t: Translation;
  usage: VendorAccountUsage | null | undefined;
  now?: number;
}>) {
  if (!usage) return null;
  const hasWindow = usage.five_hour_pct >= 0 || usage.weekly_pct >= 0;
  if (!hasWindow && usage.credit_balance === '') return null;

  const updatedAt = new Date(usage.updated_at).getTime();
  return (
    <Box sx={{ mt: 3 }}>
      <Panel
        titleId="vendor-account-usage-heading"
        title={t.vendorUsageTitle}
        subtitle={t.vendorUsageIntro}
      >
        <Box sx={{ display: 'grid', gridTemplateColumns: 'minmax(260px, 480px)', gap: 2.25 }}>
          <UsageWindow
            t={t}
            label={t.vendorUsageFiveHour}
            pct={usage.five_hour_pct}
            resetAt={usage.five_hour_reset_at}
            now={now}
          />
          <UsageWindow
            t={t}
            label={t.vendorUsageWeekly}
            pct={usage.weekly_pct}
            resetAt={usage.weekly_reset_at}
            now={now}
          />
          {usage.credit_balance !== '' && (
            <Box sx={{ display: 'flex', justifyContent: 'space-between', gap: 2 }}>
              <Typography sx={{ fontWeight: 600 }}>{t.vendorUsageCreditBalance}</Typography>
              <Typography>{usage.credit_balance}</Typography>
            </Box>
          )}
          {!Number.isNaN(updatedAt) && (
            <Typography variant="caption" color="text.secondary">
              {t.vendorUsageUpdatedAt(t.activityRelativeTime((now - updatedAt) / 1000))}
            </Typography>
          )}
        </Box>
      </Panel>
    </Box>
  );
}

/**
 * Fetches the account's usage snapshot and renders the panel. The list carries
 * no snapshot (one read per row would be an N+1), so the detail view reads it
 * from the single-account GET. The panel is auxiliary: a failed read simply
 * leaves it out rather than raising a toast on every detail open.
 *
 * `refreshKey` is a counter the parent bumps when something happened that can
 * have changed the snapshot behind this panel's back -- the models refresh pulls
 * the vendor's usage, and neither `account.id` nor `account.updated_at` change
 * with it. A new value re-reads the snapshot; the panel keeps showing the
 * previous one meanwhile instead of blinking out.
 */
export function VendorAccountUsage({
  t,
  api,
  accountId,
  refreshKey = 0,
}: Readonly<{
  t: Translation;
  api: Pick<PortalApi, 'vendorAccount'>;
  accountId: VendorAccount['id'];
  refreshKey?: number;
}>) {
  const { data } = useLatestFetch(
    () => api.vendorAccount(accountId).then((account) => account.usage ?? null),
    [api, accountId, refreshKey],
  );
  return <VendorAccountUsagePanel t={t} usage={data} />;
}
