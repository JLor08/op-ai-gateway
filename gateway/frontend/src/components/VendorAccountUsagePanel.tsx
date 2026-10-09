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
  valueText,
}: Readonly<{
  t: Translation;
  label: string;
  pct: number;
  resetAt: string | null;
  now: number;
  // Replaces the "N % used" text, for a row that carries amounts as well (the
  // spend-control credits). With it, an unknown percentage (-1) keeps the text
  // but draws no bar, instead of the "no data yet" row.
  valueText?: string;
}>) {
  const known = pct >= 0;
  // -1 is "never observed", deliberately not a 0 % bar.
  if (!known && valueText === undefined) {
    return (
      <Box>
        <Typography sx={{ fontWeight: 600 }}>{label}</Typography>
        <Typography variant="body2" color="text.secondary">
          {t.vendorUsageNoData}
        </Typography>
      </Box>
    );
  }
  const shown = known ? Math.min(100, Math.round(pct)) : 0;
  const reset = resetLine(t, resetAt, now);
  return (
    <Box>
      <Box sx={{ display: 'flex', justifyContent: 'space-between', gap: 2, mb: 0.75 }}>
        <Typography sx={{ fontWeight: 600 }}>{label}</Typography>
        <Typography>{valueText ?? t.vendorUsagePercentUsed(shown)}</Typography>
      </Box>
      {known && (
        <LinearProgress
          variant="determinate"
          value={shown}
          color={barColor(pct)}
          aria-label={label}
          sx={{ height: 8, borderRadius: 4 }}
        />
      )}
      {reset !== null && (
        <Typography variant="body2" color="text.secondary" sx={{ mt: 0.75 }}>
          {reset}
        </Typography>
      )}
    </Box>
  );
}

// The vendor's spend-control strings are raw and not validated as numbers. Only a
// plain decimal that a double holds exactly is reformatted (one decimal, the portal
// locale); anything else -- "n/a", "1e3", "0x10", a 20-digit integer -- is shown as
// the vendor sent it rather than guessed at or turned into NaN.
const PLAIN_DECIMAL = /^-?\d+(\.\d+)?$/;

function formatSpendAmount(t: Translation, raw: string): string {
  const trimmed = raw.trim();
  if (!PLAIN_DECIMAL.test(trimmed)) return trimmed;
  const n = Number(trimmed);
  return Number.isFinite(n) && Math.abs(n) <= Number.MAX_SAFE_INTEGER
    ? t.vendorUsageSpendNumber(n)
    : trimmed;
}

// "credit"/"credits" (what the vendor reports today) and an unreported unit both
// read "Credits"; any other unit is shown as the vendor named it.
function spendUnit(t: Translation, raw: string): string {
  const unit = raw.trim();
  return unit === '' || /^credits?$/i.test(unit) ? t.vendorUsageCredits : unit;
}

// Spend control is present when the vendor reported a percentage or an amount.
function hasSpend(usage: VendorAccountUsage): boolean {
  return (
    usage.spend_used_pct >= 0 || usage.spend_limit.trim() !== '' || usage.spend_used.trim() !== ''
  );
}

// "42,5 / 6000 Credits (1 %)". The used amount is formatted for display; the
// limit is shown as the vendor sent it. A missing half degrades to "N Credits used"
// / "Limit: N Credits", and an unknown percentage leaves off the parenthesis.
function spendLine(t: Translation, usage: VendorAccountUsage): string {
  const unit = spendUnit(t, usage.spend_unit);
  const used = usage.spend_used.trim() === '' ? '' : formatSpendAmount(t, usage.spend_used);
  const limit = usage.spend_limit.trim();
  let amount = '';
  if (used !== '' && limit !== '') amount = `${used} / ${limit} ${unit}`;
  else if (used !== '') amount = t.vendorUsageSpendUsed(used, unit);
  else if (limit !== '') amount = t.vendorUsageSpendLimit(limit, unit);
  if (usage.spend_used_pct < 0) return amount;
  const shown = Math.min(100, Math.round(usage.spend_used_pct));
  return amount === '' ? t.vendorUsagePercentUsed(shown) : t.vendorUsageSpendLine(amount, shown);
}

function CreditRow({ label, value }: Readonly<{ label: string; value: string }>) {
  return (
    <Box sx={{ display: 'flex', justifyContent: 'space-between', gap: 2 }}>
      <Typography sx={{ fontWeight: 600 }}>{label}</Typography>
      <Typography>{value}</Typography>
    </Box>
  );
}

// The credit side of the panel, one state by priority: the spend-control credits
// (a Business plan) win; else "unlimited"; else the vendor's credit balance (the
// plans that report one); else an explicit "no credits". "has_credits" with no
// amounts or balance has nothing more to say, and an unknown status says nothing.
function CreditState({
  t,
  usage,
  now,
}: Readonly<{ t: Translation; usage: VendorAccountUsage; now: number }>) {
  if (hasSpend(usage)) {
    return (
      <UsageWindow
        t={t}
        label={t.vendorUsageSpendLabel}
        pct={usage.spend_used_pct}
        resetAt={usage.spend_reset_at}
        now={now}
        valueText={spendLine(t, usage)}
      />
    );
  }
  if (usage.credit_status === 'unlimited') {
    return <CreditRow label={t.vendorUsageSpendLabel} value={t.vendorUsageUnlimited} />;
  }
  if (usage.credit_balance !== '') {
    return <CreditRow label={t.vendorUsageCreditBalance} value={usage.credit_balance} />;
  }
  if (usage.credit_status === 'none') {
    return <CreditRow label={t.vendorUsageSpendLabel} value={t.vendorUsageNoCredits} />;
  }
  return null;
}

/**
 * The "Usage & limits" panel of a vendor account's detail view: the 5-hour and
 * the weekly window as progress bars with "resets in ..." under them, plus the
 * credit side: the vendor's credit balance when it reported one, or -- for a
 * Codex Business plan, whose quota is spend-control credits rather than rate-limit
 * windows -- "used / limit Credits" with a bar and its reset (#195), or the
 * unlimited / no-credits state. The windows are PERCENTAGES ONLY (neither vendor
 * publishes an absolute cap); only the spend-control credits carry amounts.
 *
 * It renders nothing at all without a snapshot, or when the snapshot knows
 * nothing yet: no window (every percentage -1), no credit balance, no spend
 * control and no credit status. An account the gateway has not seen a rate-limit
 * header for has nothing to show. A single unknown window next to a known one
 * reads "No data yet" instead of a 0 % bar.
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
  const spend = hasSpend(usage);
  if (!hasWindow && usage.credit_balance === '' && !spend && usage.credit_status === '') {
    return null;
  }

  const updatedAt = new Date(usage.updated_at).getTime();
  return (
    <Box sx={{ mt: 3 }}>
      <Panel
        titleId="vendor-account-usage-heading"
        title={t.vendorUsageTitle}
        subtitle={spend ? t.vendorUsageIntroSpend : t.vendorUsageIntro}
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
          <CreditState t={t} usage={usage} now={now} />
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
