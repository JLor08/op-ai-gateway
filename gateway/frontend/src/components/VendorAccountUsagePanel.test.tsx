// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, render, screen, within } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { VendorAccountUsagePanel } from './VendorAccountUsagePanel';
import { messages, type Locale } from '../i18n';
import type { VendorAccountUsage } from '../api';

afterEach(cleanup);

// A fixed "now" so every countdown is deterministic.
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

for (const locale of ['de', 'en'] as readonly Locale[]) {
  const t = messages[locale];

  function renderPanel(usage: VendorAccountUsage | null | undefined) {
    return render(<VendorAccountUsagePanel t={t} usage={usage} now={NOW} />);
  }

  describe(`VendorAccountUsagePanel [${locale}]`, () => {
    it('shows the 5-hour and the weekly window as bars at their percentage, with the reset countdown', () => {
      renderPanel(makeUsage());

      expect(screen.getByRole('heading', { name: t.vendorUsageTitle })).toBeInTheDocument();
      const fiveHour = screen.getByRole('progressbar', { name: t.vendorUsageFiveHour });
      expect(fiveHour).toHaveAttribute('aria-valuenow', '42');
      const weekly = screen.getByRole('progressbar', { name: t.vendorUsageWeekly });
      expect(weekly).toHaveAttribute('aria-valuenow', '7');

      expect(screen.getByText(t.vendorUsagePercentUsed(42))).toBeInTheDocument();
      expect(screen.getByText(t.vendorUsagePercentUsed(7))).toBeInTheDocument();
      expect(screen.getByText(t.vendorUsageResetsIn('2 h 14 min'))).toBeInTheDocument();
      expect(screen.getByText(t.vendorUsageResetsIn('3 d 4 h'))).toBeInTheDocument();
    });

    it('rounds a fractional percentage and clamps the bar to 100', () => {
      renderPanel(makeUsage({ five_hour_pct: 41.6, weekly_pct: 130 }));

      expect(screen.getByRole('progressbar', { name: t.vendorUsageFiveHour })).toHaveAttribute(
        'aria-valuenow',
        '42',
      );
      expect(screen.getByRole('progressbar', { name: t.vendorUsageWeekly })).toHaveAttribute(
        'aria-valuenow',
        '100',
      );
    });

    it('renders a real 0 % as an empty bar, not as "no data"', () => {
      renderPanel(makeUsage({ five_hour_pct: 0 }));

      expect(screen.getByRole('progressbar', { name: t.vendorUsageFiveHour })).toHaveAttribute(
        'aria-valuenow',
        '0',
      );
      expect(screen.getByText(t.vendorUsagePercentUsed(0))).toBeInTheDocument();
      expect(screen.queryByText(t.vendorUsageNoData)).not.toBeInTheDocument();
    });

    it('shows "no data yet" instead of a bar for a window that was never observed (-1)', () => {
      renderPanel(makeUsage({ weekly_pct: -1, weekly_reset_at: null }));

      expect(screen.getByRole('progressbar', { name: t.vendorUsageFiveHour })).toBeInTheDocument();
      expect(
        screen.queryByRole('progressbar', { name: t.vendorUsageWeekly }),
      ).not.toBeInTheDocument();
      expect(screen.getByText(t.vendorUsageWeekly)).toBeInTheDocument();
      expect(screen.getByText(t.vendorUsageNoData)).toBeInTheDocument();
      // The unknown window is not a "0 % used".
      expect(screen.queryByText(t.vendorUsagePercentUsed(0))).not.toBeInTheDocument();
      expect(screen.queryByText(t.vendorUsagePercentUsed(-1))).not.toBeInTheDocument();
    });

    it('renders nothing when the snapshot knows no window and has no credit balance', () => {
      const { container } = renderPanel(makeUsage({ five_hour_pct: -1, weekly_pct: -1 }));

      expect(container).toBeEmptyDOMElement();
    });

    it('renders nothing without a snapshot', () => {
      expect(renderPanel(undefined).container).toBeEmptyDOMElement();
      cleanup();
      expect(renderPanel(null).container).toBeEmptyDOMElement();
    });

    it('shows the credit balance as the vendor reported it, and only when there is one', () => {
      renderPanel(makeUsage({ credit_balance: '12.34' }));

      expect(screen.getByText(t.vendorUsageCreditBalance)).toBeInTheDocument();
      expect(screen.getByText('12.34')).toBeInTheDocument();

      cleanup();
      renderPanel(makeUsage({ credit_balance: '' }));
      expect(screen.queryByText(t.vendorUsageCreditBalance)).not.toBeInTheDocument();
    });

    it('still shows a credit balance when no window is known yet', () => {
      renderPanel(makeUsage({ five_hour_pct: -1, weekly_pct: -1, credit_balance: '5.00' }));

      expect(screen.getByText('5.00')).toBeInTheDocument();
      expect(screen.getAllByText(t.vendorUsageNoData)).toHaveLength(2);
      expect(screen.queryByRole('progressbar')).not.toBeInTheDocument();
    });

    it('omits the reset line for a window with no reset time', () => {
      // Anthropic advertises one reset only, attributed to the 5-hour window.
      renderPanel(makeUsage({ weekly_reset_at: null }));

      const resetsIn = t.vendorUsageResetsIn('').trim();
      expect(screen.getAllByText(new RegExp(`^${resetsIn}`))).toHaveLength(1);
      expect(screen.getByRole('progressbar', { name: t.vendorUsageWeekly })).toBeInTheDocument();
    });

    it('says the window has reset once its reset time has passed, instead of a negative countdown', () => {
      renderPanel(makeUsage({ five_hour_reset_at: at(-10 * MIN) }));

      expect(screen.getByText(t.vendorUsageResetPassed)).toBeInTheDocument();
      expect(screen.queryByText(t.vendorUsageResetsIn('< 1 min'))).not.toBeInTheDocument();
    });

    it('reads a reset less than a minute away as "< 1 min"', () => {
      renderPanel(makeUsage({ five_hour_reset_at: at(30_000) }));

      expect(screen.getByText(t.vendorUsageResetsIn('< 1 min'))).toBeInTheDocument();
    });

    it('colors a bar amber then red as it fills, and plain below', () => {
      renderPanel(makeUsage({ five_hour_pct: 80, weekly_pct: 95 }));
      const cls = (name: string) =>
        screen.getByRole('progressbar', { name }).getAttribute('class') ?? '';

      expect(cls(t.vendorUsageFiveHour)).toContain('colorWarning');
      expect(cls(t.vendorUsageWeekly)).toContain('colorError');

      cleanup();
      renderPanel(makeUsage({ five_hour_pct: 74, weekly_pct: 10 }));
      expect(cls(t.vendorUsageFiveHour)).toContain('colorPrimary');
      expect(cls(t.vendorUsageWeekly)).toContain('colorPrimary');
    });

    it('states when the snapshot was last updated, and never invents an absolute cap', () => {
      renderPanel(makeUsage({ credit_balance: '1.00' }));

      expect(
        screen.getByText(t.vendorUsageUpdatedAt(t.activityRelativeTime(5 * 60))),
      ).toBeInTheDocument();
      const panel = screen.getByRole('region', { name: t.vendorUsageTitle });
      // Percentages only: no "N of M" / "N von M" pair anywhere in the panel.
      expect(within(panel).queryByText(/\d\s*(of|von|\/)\s*\d/i)).not.toBeInTheDocument();
    });
  });
}
