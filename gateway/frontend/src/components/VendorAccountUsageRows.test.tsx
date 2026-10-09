// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { VendorAccountUsageRows, visibleUsageRows } from './VendorAccountUsageRows';
import { messages, type Locale } from '../i18n';
import type { VendorAccountUsage } from '../api';

afterEach(cleanup);

const NOW = Date.parse('2026-10-08T12:00:00Z');
const MIN = 60_000;
const HOUR = 60 * MIN;
const DAY = 24 * HOUR;
const at = (offsetMs: number) => new Date(NOW + offsetMs).toISOString();

function makeUsage(overrides: Partial<VendorAccountUsage> = {}): VendorAccountUsage {
  return {
    five_hour_pct: 42,
    five_hour_reset_at: at(2 * HOUR + 14 * MIN),
    weekly_pct: 7,
    weekly_reset_at: at(3 * DAY + 4 * HOUR),
    credit_balance: '',
    spend_unit: '',
    spend_limit: '',
    spend_used: '',
    spend_remaining: '',
    spend_used_pct: -1,
    spend_reset_at: null,
    credit_status: '',
    updated_at: at(-5 * MIN),
    ...overrides,
  };
}

// A Codex Business snapshot: no rate-limit window at all, only the spend control.
function makeBusinessUsage(overrides: Partial<VendorAccountUsage> = {}): VendorAccountUsage {
  return makeUsage({
    five_hour_pct: -1,
    five_hour_reset_at: null,
    weekly_pct: -1,
    weekly_reset_at: null,
    spend_unit: 'credit',
    spend_limit: '6000',
    spend_used: '42.5',
    spend_remaining: '5957.5',
    spend_used_pct: 1,
    spend_reset_at: at(6 * DAY + 4 * HOUR),
    credit_status: 'has_credits',
    ...overrides,
  });
}

// A snapshot the vendor said nothing about: every window and credit field unknown.
function makeEmptyUsage(overrides: Partial<VendorAccountUsage> = {}): VendorAccountUsage {
  return makeUsage({
    five_hour_pct: -1,
    five_hour_reset_at: null,
    weekly_pct: -1,
    weekly_reset_at: null,
    ...overrides,
  });
}

describe('visibleUsageRows', () => {
  it('lists no row for no snapshot', () => {
    expect(visibleUsageRows(null)).toEqual([]);
    expect(visibleUsageRows(undefined)).toEqual([]);
  });

  it('lists both windows, in order, when the vendor reported both', () => {
    expect(visibleUsageRows(makeUsage())).toEqual(['fiveHour', 'weekly']);
  });

  it('lists the windows and then the credit row when both are known', () => {
    expect(visibleUsageRows(makeUsage({ credit_balance: '12.34' }))).toEqual([
      'fiveHour',
      'weekly',
      'credit',
    ]);
  });

  it('lists only the window the vendor reported (-1 is "not provided", not 0 %)', () => {
    expect(visibleUsageRows(makeUsage({ five_hour_pct: -1, five_hour_reset_at: null }))).toEqual([
      'weekly',
    ]);
    expect(visibleUsageRows(makeUsage({ weekly_pct: -1, weekly_reset_at: null }))).toEqual([
      'fiveHour',
    ]);
  });

  it('keeps a window whose real value is 0 %', () => {
    expect(visibleUsageRows(makeUsage({ five_hour_pct: 0, weekly_pct: 0 }))).toEqual([
      'fiveHour',
      'weekly',
    ]);
  });

  it('lists only the credit row for a Business snapshot (spend control, no window)', () => {
    expect(visibleUsageRows(makeBusinessUsage())).toEqual(['credit']);
  });

  it.each([
    ['spend limit only', { spend_limit: '6000' }],
    ['spend used only', { spend_used: '1' }],
    ['spend percentage only', { spend_used_pct: 12 }],
    ['an unlimited plan', { credit_status: 'unlimited' }],
    ['no credits', { credit_status: 'none' }],
    ['a credit balance', { credit_balance: '5.00' }],
  ] as const)('lists the credit row for %s', (_name, overrides) => {
    expect(visibleUsageRows(makeEmptyUsage(overrides))).toEqual(['credit']);
  });

  it('lists nothing for a snapshot that knows no window and no credit data', () => {
    expect(visibleUsageRows(makeEmptyUsage())).toEqual([]);
  });

  it('lists nothing for has_credits alone: the flag has no amount or balance to show', () => {
    expect(visibleUsageRows(makeEmptyUsage({ credit_status: 'has_credits' }))).toEqual([]);
    expect(visibleUsageRows(makeEmptyUsage({ credit_status: 'something-new' }))).toEqual([]);
  });
});

for (const locale of ['de', 'en'] as readonly Locale[]) {
  const t = messages[locale];
  // The retired "no data yet" wording, per locale: it must not come back.
  const noData = locale === 'de' ? 'Noch keine Daten' : 'No data yet';

  function renderRows(usage: VendorAccountUsage, dense?: boolean) {
    return render(<VendorAccountUsageRows t={t} usage={usage} now={NOW} dense={dense} />);
  }

  describe(`VendorAccountUsageRows [${locale}]`, () => {
    it('renders both windows with their reset countdown for a windows-only snapshot', () => {
      renderRows(makeUsage());

      expect(screen.getByRole('progressbar', { name: t.vendorUsageFiveHour })).toHaveAttribute(
        'aria-valuenow',
        '42',
      );
      expect(screen.getByRole('progressbar', { name: t.vendorUsageWeekly })).toHaveAttribute(
        'aria-valuenow',
        '7',
      );
      expect(screen.getByText(t.vendorUsageResetsIn('2 h 14 min'))).toBeInTheDocument();
      expect(screen.getByText(t.vendorUsageResetsIn('3 d 4 h'))).toBeInTheDocument();
      expect(
        screen.getByText(t.vendorUsageUpdatedAt(t.activityRelativeTime(5 * 60))),
      ).toBeVisible();
    });

    it('renders ONLY the credit row for a Business snapshot, with no window rows and no placeholder', () => {
      renderRows(makeBusinessUsage());

      expect(screen.getByRole('progressbar', { name: t.vendorUsageSpendLabel })).toHaveAttribute(
        'aria-valuenow',
        '1',
      );
      expect(screen.getAllByRole('progressbar')).toHaveLength(1);
      expect(screen.queryByText(t.vendorUsageFiveHour)).not.toBeInTheDocument();
      expect(screen.queryByText(t.vendorUsageWeekly)).not.toBeInTheDocument();
      expect(screen.queryByText(noData)).not.toBeInTheDocument();
    });

    it('renders both windows AND the credit row for a windows-plus-credits snapshot', () => {
      renderRows(makeUsage({ credit_balance: '12.34' }));

      expect(screen.getByRole('progressbar', { name: t.vendorUsageFiveHour })).toBeInTheDocument();
      expect(screen.getByRole('progressbar', { name: t.vendorUsageWeekly })).toBeInTheDocument();
      expect(screen.getByText(t.vendorUsageCreditBalance)).toBeInTheDocument();
      expect(screen.getByText('12.34')).toBeInTheDocument();
    });

    it('renders only the reported window when the other one is unknown', () => {
      renderRows(makeUsage({ weekly_pct: -1, weekly_reset_at: null }));

      expect(screen.getByRole('progressbar', { name: t.vendorUsageFiveHour })).toBeInTheDocument();
      expect(screen.queryByText(t.vendorUsageWeekly)).not.toBeInTheDocument();
      expect(screen.queryByText(noData)).not.toBeInTheDocument();
      // The unknown window is not a "0 % used".
      expect(screen.queryByText(t.vendorUsagePercentUsed(0))).not.toBeInTheDocument();
    });

    it('still renders a window whose real value is 0 % as an empty bar', () => {
      renderRows(makeUsage({ five_hour_pct: 0 }));

      expect(screen.getByRole('progressbar', { name: t.vendorUsageFiveHour })).toHaveAttribute(
        'aria-valuenow',
        '0',
      );
      expect(screen.getByText(t.vendorUsagePercentUsed(0))).toBeInTheDocument();
    });

    it('renders nothing at all when no row is visible, not even the updated-at caption', () => {
      const { container } = renderRows(makeEmptyUsage({ credit_status: 'has_credits' }));

      expect(container).toBeEmptyDOMElement();
    });

    it('renders no title or landmark of its own: the caller supplies the frame', () => {
      renderRows(makeUsage());

      expect(screen.queryByRole('heading')).not.toBeInTheDocument();
      expect(screen.queryByRole('region')).not.toBeInTheDocument();
    });

    it('renders the same rows in dense mode, with thinner bars', () => {
      const { unmount } = renderRows(makeUsage());
      const normal = getComputedStyle(
        screen.getByRole('progressbar', { name: t.vendorUsageFiveHour }),
      ).height;
      unmount();

      renderRows(makeUsage(), true);
      const dense = getComputedStyle(
        screen.getByRole('progressbar', { name: t.vendorUsageFiveHour }),
      ).height;

      expect(screen.getByRole('progressbar', { name: t.vendorUsageWeekly })).toBeInTheDocument();
      expect(parseFloat(dense)).toBeLessThan(parseFloat(normal));
    });
  });
}
