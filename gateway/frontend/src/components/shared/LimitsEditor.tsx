// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { Box, Typography } from '@mui/material';
import type { LimitConfig, LimitPeriod, LimitUsage } from '../../api';
import type { Translation } from './types';
import { Field } from './Field';
import { SelectField } from './SelectField';

// The 4 calendar periods a quota/budget threshold may be aligned to, plus ""
// (= that limit is off) — mirrors routing.ValidLimitPeriod on the backend
// (design spec §5/§7.3).
const PERIODS: LimitPeriod[] = ['', 'hour', 'day', 'week', 'month'];

function periodLabel(t: Translation, period: LimitPeriod): string {
  switch (period) {
    case 'hour':
      return t.limitPeriodHour;
    case 'day':
      return t.limitPeriodDay;
    case 'week':
      return t.limitPeriodWeek;
    case 'month':
      return t.limitPeriodMonth;
    default:
      return t.limitPeriodOff;
  }
}

// Parses a numeric <input>'s raw text into a non-negative number; blank or
// invalid input becomes 0 (== "this limit is off" for every LimitConfig
// field) — mirrors the `num()` helper used by every other numeric-field form
// in this codebase (e.g. MappingSection's context/metric fields).
function parseNonNegative(raw: string): number {
  if (raw.trim() === '') return 0;
  const n = Number(raw);
  return Number.isNaN(n) || n < 0 ? 0 : n;
}

/**
 * The shared rate/quota/budget limits editor (design spec §9), embedded
 * both in the Service settings form (ServicesView) and the per-user limits
 * dialog (UsersView, admin-only). Four independently optional blocks:
 *
 *   - Rate: max N requests per a short window (seconds) — no calendar
 *     period; the backend enforces this in-memory, so there is no
 *     persistent usage to show for it.
 *   - Request quota / Token quota / Cost budget: a threshold + one of the 4
 *     calendar periods (hour/day/week/month), or "" (that limit is off).
 *
 * `usage` (when supplied) renders a read-only "used / threshold this
 * period" line under each of the three period-bound blocks, but ONLY when
 * that specific limit is actually configured (its period != "") — an
 * unconfigured limit has no meaningful usage to show, and the backend
 * itself performs no aggregate read for it either (portal.limitUsage).
 *
 * Fully controlled: no internal state. `disabled` renders every field
 * read-only (a token-delegate viewing a Service's settings, or — for
 * UsersView — never, since that dialog is admin-only to begin with).
 */
export function LimitsEditor({
  t,
  idPrefix,
  value,
  onChange,
  usage,
  disabled = false,
}: Readonly<{
  t: Translation;
  idPrefix: string;
  value: LimitConfig;
  onChange: (next: LimitConfig) => void;
  usage?: LimitUsage;
  disabled?: boolean;
}>) {
  function setField<K extends keyof LimitConfig>(key: K, v: LimitConfig[K]) {
    onChange({ ...value, [key]: v });
  }

  function periodOptions(idBase: string) {
    return PERIODS.map((p) => (
      <option key={p === '' ? `${idBase}-off` : p} value={p}>
        {periodLabel(t, p)}
      </option>
    ));
  }

  return (
    <Box sx={{ display: 'grid', gap: 2.5 }}>
      <Box>
        <Typography variant="subtitle2">{t.limitsTitle}</Typography>
        <Typography variant="caption" color="text.secondary">
          {t.limitsSubtitle}
        </Typography>
      </Box>

      {/* Rate limit: requests + window in seconds, no calendar period. */}
      <Box sx={{ display: 'grid', gap: 1 }}>
        <Typography variant="body2" sx={{ fontWeight: 600 }}>
          {t.limitRateTitle}
        </Typography>
        <Box sx={{ display: 'flex', gap: 1.5, flexWrap: 'wrap' }}>
          <Box sx={{ minWidth: 160 }}>
            <Field
              id={`${idPrefix}-limit-rate-requests`}
              type="number"
              label={t.limitRateRequestsLabel}
              value={String(value.rate_requests)}
              onChange={(e) => setField('rate_requests', parseNonNegative(e.target.value))}
              disabled={disabled}
              inputProps={{ min: 0, step: 1 }}
            />
          </Box>
          <Box sx={{ minWidth: 160 }}>
            <Field
              id={`${idPrefix}-limit-rate-window`}
              type="number"
              label={t.limitRateWindowLabel}
              value={String(value.rate_window_seconds)}
              onChange={(e) => setField('rate_window_seconds', parseNonNegative(e.target.value))}
              disabled={disabled}
              inputProps={{ min: 0, step: 1 }}
            />
          </Box>
        </Box>
      </Box>

      {/* Request quota: threshold + period. */}
      <Box sx={{ display: 'grid', gap: 1 }}>
        <Typography variant="body2" sx={{ fontWeight: 600 }}>
          {t.limitRequestQuotaTitle}
        </Typography>
        <Box sx={{ display: 'flex', gap: 1.5, flexWrap: 'wrap' }}>
          <Box sx={{ minWidth: 160 }}>
            <Field
              id={`${idPrefix}-limit-request-quota`}
              type="number"
              label={t.limitRequestQuotaLabel}
              value={String(value.request_quota)}
              onChange={(e) => setField('request_quota', parseNonNegative(e.target.value))}
              disabled={disabled}
              inputProps={{ min: 0, step: 1 }}
            />
          </Box>
          <Box sx={{ minWidth: 160 }}>
            <SelectField
              id={`${idPrefix}-limit-request-quota-period`}
              label={t.limitPeriodLabel}
              value={value.request_quota_period}
              onChange={(e) => setField('request_quota_period', e.target.value as LimitPeriod)}
              disabled={disabled}
            >
              {periodOptions(`${idPrefix}-request`)}
            </SelectField>
          </Box>
        </Box>
        {usage && value.request_quota_period !== '' && (
          <Typography variant="caption" color="text.secondary">
            {t.limitUsageRequestsLine(usage.requests_this_period, value.request_quota)}
          </Typography>
        )}
      </Box>

      {/* Token quota: threshold + period. */}
      <Box sx={{ display: 'grid', gap: 1 }}>
        <Typography variant="body2" sx={{ fontWeight: 600 }}>
          {t.limitTokenQuotaTitle}
        </Typography>
        <Box sx={{ display: 'flex', gap: 1.5, flexWrap: 'wrap' }}>
          <Box sx={{ minWidth: 160 }}>
            <Field
              id={`${idPrefix}-limit-token-quota`}
              type="number"
              label={t.limitTokenQuotaLabel}
              value={String(value.token_quota)}
              onChange={(e) => setField('token_quota', parseNonNegative(e.target.value))}
              disabled={disabled}
              inputProps={{ min: 0, step: 1 }}
            />
          </Box>
          <Box sx={{ minWidth: 160 }}>
            <SelectField
              id={`${idPrefix}-limit-token-quota-period`}
              label={t.limitPeriodLabel}
              value={value.token_quota_period}
              onChange={(e) => setField('token_quota_period', e.target.value as LimitPeriod)}
              disabled={disabled}
            >
              {periodOptions(`${idPrefix}-token`)}
            </SelectField>
          </Box>
        </Box>
        {usage && value.token_quota_period !== '' && (
          <Typography variant="caption" color="text.secondary">
            {t.limitUsageTokensLine(usage.tokens_this_period, value.token_quota)}
          </Typography>
        )}
      </Box>

      {/* Cost budget: amount + period. */}
      <Box sx={{ display: 'grid', gap: 1 }}>
        <Typography variant="body2" sx={{ fontWeight: 600 }}>
          {t.limitCostBudgetTitle}
        </Typography>
        <Box sx={{ display: 'flex', gap: 1.5, flexWrap: 'wrap' }}>
          <Box sx={{ minWidth: 160 }}>
            <Field
              id={`${idPrefix}-limit-cost-budget`}
              type="number"
              label={t.limitCostBudgetLabel}
              value={String(value.cost_budget)}
              onChange={(e) => setField('cost_budget', parseNonNegative(e.target.value))}
              disabled={disabled}
              inputProps={{ min: 0, step: 'any' }}
            />
          </Box>
          <Box sx={{ minWidth: 160 }}>
            <SelectField
              id={`${idPrefix}-limit-cost-budget-period`}
              label={t.limitPeriodLabel}
              value={value.cost_budget_period}
              onChange={(e) => setField('cost_budget_period', e.target.value as LimitPeriod)}
              disabled={disabled}
            >
              {periodOptions(`${idPrefix}-cost`)}
            </SelectField>
          </Box>
        </Box>
        {usage && value.cost_budget_period !== '' && (
          <Typography variant="caption" color="text.secondary">
            {t.limitUsageCostLine(usage.cost_this_period, value.cost_budget)}
          </Typography>
        )}
      </Box>
    </Box>
  );
}

// Shared client-side pre-check mirroring the backend's pair-consistency rule
// (validateLimitConfig, design spec §7.3): a threshold and its period must
// be both-set or both-empty. Used by ServicesView/UsersView to disable Save
// on an obviously-invalid shape without round-tripping to the server first;
// the backend still re-validates (this is a UX nicety, not the source of
// truth).
export function limitConfigHasPairMismatch(cfg: LimitConfig): boolean {
  return (
    cfg.rate_requests > 0 !== cfg.rate_window_seconds > 0 ||
    cfg.request_quota > 0 !== (cfg.request_quota_period !== '') ||
    cfg.token_quota > 0 !== (cfg.token_quota_period !== '') ||
    cfg.cost_budget > 0 !== (cfg.cost_budget_period !== '')
  );
}
