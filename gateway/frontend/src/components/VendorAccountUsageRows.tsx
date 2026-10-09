// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { Box, LinearProgress, Typography } from '@mui/material';
import type { VendorAccountUsage } from '../api';
import type { Translation } from './shared/types';
import { formatCountdown } from './shared/countdown';

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

// One limit as a labelled row with a bar. Only ever rendered for a limit the
// vendor actually reported: a window with an unknown percentage (-1) has no row at
// all (see visibleUsageRows), so there is no "no data yet" state to draw here.
function UsageWindow({
  t,
  label,
  pct,
  resetAt,
  now,
  dense,
  valueText,
}: Readonly<{
  t: Translation;
  label: string;
  pct: number;
  resetAt: string | null;
  now: number;
  dense: boolean;
  // Replaces the "N % used" text, for a row that carries amounts as well (the
  // spend-control credits). With it, an unknown percentage (-1) keeps the text
  // but draws no bar.
  valueText?: string;
}>) {
  const known = pct >= 0;
  const shown = known ? Math.min(100, Math.round(pct)) : 0;
  const reset = resetLine(t, resetAt, now);
  return (
    <Box>
      <Box
        sx={{ display: 'flex', justifyContent: 'space-between', gap: 2, mb: dense ? 0.5 : 0.75 }}
      >
        <Typography sx={{ fontWeight: 600 }}>{label}</Typography>
        <Typography>{valueText ?? t.vendorUsagePercentUsed(shown)}</Typography>
      </Box>
      {known && (
        <LinearProgress
          variant="determinate"
          value={shown}
          color={barColor(pct)}
          aria-label={label}
          sx={{ height: dense ? 6 : 8, borderRadius: dense ? 3 : 4 }}
        />
      )}
      {reset !== null && (
        <Typography variant="body2" color="text.secondary" sx={{ mt: dense ? 0.5 : 0.75 }}>
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

/**
 * Spend control is present when the vendor reported a percentage or an amount.
 * The detail panel picks its intro sentence by it: the spend-control credits carry
 * amounts, which the "percentages only" intro would contradict.
 */
export function hasSpendControl(usage: VendorAccountUsage): boolean {
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

// The credit side of a snapshot, one state by priority: the spend-control credits
// (a Business plan) win; else "unlimited"; else the vendor's credit balance (the
// plans that report one); else an explicit "no credits". "has_credits" with no
// amounts or balance has nothing more to say, and an unknown status says nothing:
// null, so there is no credit row. The single source for both whether the row exists
// (visibleUsageRows) and what it shows (CreditState).
type CreditKind = 'spend' | 'unlimited' | 'balance' | 'none';

function creditKind(usage: VendorAccountUsage): CreditKind | null {
  if (hasSpendControl(usage)) return 'spend';
  if (usage.credit_status === 'unlimited') return 'unlimited';
  if (usage.credit_balance !== '') return 'balance';
  if (usage.credit_status === 'none') return 'none';
  return null;
}

function CreditState({
  t,
  usage,
  kind,
  now,
  dense,
}: Readonly<{
  t: Translation;
  usage: VendorAccountUsage;
  kind: CreditKind;
  now: number;
  dense: boolean;
}>) {
  switch (kind) {
    case 'spend':
      return (
        <UsageWindow
          t={t}
          label={t.vendorUsageSpendLabel}
          pct={usage.spend_used_pct}
          resetAt={usage.spend_reset_at}
          now={now}
          dense={dense}
          valueText={spendLine(t, usage)}
        />
      );
    case 'unlimited':
      return <CreditRow label={t.vendorUsageSpendLabel} value={t.vendorUsageUnlimited} />;
    case 'balance':
      return <CreditRow label={t.vendorUsageCreditBalance} value={usage.credit_balance} />;
    case 'none':
      return <CreditRow label={t.vendorUsageSpendLabel} value={t.vendorUsageNoCredits} />;
  }
}

/** The rows a snapshot can show, in display order. */
export type UsageRow = 'fiveHour' | 'weekly' | 'credit';

/**
 * Which rows of a usage snapshot are worth showing: only the limits the vendor
 * actually provides. The 5-hour (weekly) row exists when `five_hour_pct`
 * (`weekly_pct`) is known, i.e. >= 0 -- a real 0 % stays, a "never observed" -1 is
 * absent, not drawn as "no data". The credit row exists when there are spend-control
 * amounts, an unlimited plan, a credit balance or an explicit "no credits"; a
 * `has_credits` flag alone does not count. No snapshot lists nothing.
 *
 * Pure, so a caller can tell an empty snapshot from a populated one without
 * rendering it (the detail panel's empty state, the dashboard's omitted card).
 */
export function visibleUsageRows(usage: VendorAccountUsage | null | undefined): UsageRow[] {
  if (!usage) return [];
  const rows: UsageRow[] = [];
  if (usage.five_hour_pct >= 0) rows.push('fiveHour');
  if (usage.weekly_pct >= 0) rows.push('weekly');
  if (creditKind(usage) !== null) rows.push('credit');
  return rows;
}

/**
 * The rows of a usage snapshot without any frame: the 5-hour and the weekly window
 * as progress bars with "resets in ..." under them, the credit side (spend-control
 * "used / limit Credits" with a bar and its reset for a Codex Business plan, the
 * unlimited / no-credits state, or the vendor's credit balance), and a "last
 * updated" caption. Only the rows visibleUsageRows lists are drawn, so a Business
 * account shows just its credit row and no empty window placeholders.
 *
 * It carries no `Panel`, no title and no landmark: the caller supplies the frame
 * (the detail view's titled panel, a dashboard card) and is expected to show its own
 * empty state, because with no visible row this renders nothing at all. The windows
 * are PERCENTAGES ONLY (neither vendor publishes an absolute cap); only the
 * spend-control credits carry amounts. `dense` tightens the spacing and the bars for
 * a compact card; `now` is injectable so the countdowns are deterministic in tests.
 */
export function VendorAccountUsageRows({
  t,
  usage,
  now = Date.now(),
  dense = false,
}: Readonly<{
  t: Translation;
  usage: VendorAccountUsage;
  now?: number;
  dense?: boolean;
}>) {
  const rows = visibleUsageRows(usage);
  if (rows.length === 0) return null;
  const credit = creditKind(usage);
  const updatedAt = new Date(usage.updated_at).getTime();
  return (
    <Box
      sx={{
        display: 'grid',
        gridTemplateColumns: dense ? 'minmax(0, 1fr)' : 'minmax(260px, 480px)',
        gap: dense ? 1.5 : 2.25,
      }}
    >
      {rows.includes('fiveHour') && (
        <UsageWindow
          t={t}
          label={t.vendorUsageFiveHour}
          pct={usage.five_hour_pct}
          resetAt={usage.five_hour_reset_at}
          now={now}
          dense={dense}
        />
      )}
      {rows.includes('weekly') && (
        <UsageWindow
          t={t}
          label={t.vendorUsageWeekly}
          pct={usage.weekly_pct}
          resetAt={usage.weekly_reset_at}
          now={now}
          dense={dense}
        />
      )}
      {credit !== null && <CreditState t={t} usage={usage} kind={credit} now={now} dense={dense} />}
      {!Number.isNaN(updatedAt) && (
        <Typography variant="caption" color="text.secondary">
          {t.vendorUsageUpdatedAt(t.activityRelativeTime((now - updatedAt) / 1000))}
        </Typography>
      )}
    </Box>
  );
}
