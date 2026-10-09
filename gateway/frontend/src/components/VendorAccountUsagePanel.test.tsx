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

// A Codex Business snapshot (#195): no rate-limit window, no credit balance, only
// the spend control (6000 credits, 42.5 used = 1 %) and the credit flag.
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

// Literal per-locale strings (not built from the translation functions), so the
// number format and the wording of the spend line are pinned for both locales.
const SPEND = {
  de: {
    line: '42,5 / 6000 Credits (1 %)',
    zero: '0 / 6000 Credits (0 %)',
    noPct: '42,5 / 6000 Credits',
    used: '42,5',
    pct: '1 %',
    fractional: '1500,3 / 6000 Credits (1 %)',
    unlimited: 'Unbegrenzt',
    none: 'Keine Credits',
  },
  en: {
    line: '42.5 / 6000 Credits (1%)',
    zero: '0 / 6000 Credits (0%)',
    noPct: '42.5 / 6000 Credits',
    used: '42.5',
    pct: '1%',
    fractional: '1500.3 / 6000 Credits (1%)',
    unlimited: 'Unlimited',
    none: 'No credits',
  },
} as const;

for (const locale of ['de', 'en'] as readonly Locale[]) {
  const t = messages[locale];
  const spend = SPEND[locale];

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

    it('renders nothing when the snapshot knows no window, no balance, no spend and no credit status', () => {
      const { container } = renderPanel(
        makeUsage({
          five_hour_pct: -1,
          weekly_pct: -1,
          credit_balance: '',
          spend_unit: '',
          spend_limit: '',
          spend_used: '',
          spend_remaining: '',
          spend_used_pct: -1,
          spend_reset_at: null,
          credit_status: '',
        }),
      );

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

    describe('spend-control credits (Codex Business)', () => {
      it('shows the panel for a Business snapshot and renders used / limit Credits with the percentage and the reset', () => {
        renderPanel(makeBusinessUsage());

        expect(screen.getByRole('heading', { name: t.vendorUsageTitle })).toBeInTheDocument();
        expect(screen.getByText(spend.line)).toBeInTheDocument();
        expect(screen.getByText(t.vendorUsageResetsIn('6 d 4 h'))).toBeInTheDocument();
        const bar = screen.getByRole('progressbar', { name: t.vendorUsageSpendLabel });
        expect(bar).toHaveAttribute('aria-valuenow', '1');
        // The windows stay "no data yet", never a 0 % bar.
        expect(screen.queryByRole('progressbar', { name: t.vendorUsageFiveHour })).toBeNull();
        expect(screen.queryByRole('progressbar', { name: t.vendorUsageWeekly })).toBeNull();
        expect(screen.getAllByText(t.vendorUsageNoData)).toHaveLength(2);
        // The credit-flag states stay out of the way when the spend line is shown.
        expect(screen.queryByText(spend.unlimited)).not.toBeInTheDocument();
        expect(screen.queryByText(spend.none)).not.toBeInTheDocument();
      });

      it('replaces the "absolute values are not published" intro, which the spend line contradicts', () => {
        renderPanel(makeBusinessUsage());
        expect(screen.getByText(t.vendorUsageIntroSpend)).toBeInTheDocument();
        expect(screen.queryByText(t.vendorUsageIntro)).not.toBeInTheDocument();

        cleanup();
        renderPanel(makeUsage());
        expect(screen.getByText(t.vendorUsageIntro)).toBeInTheDocument();
        expect(screen.queryByText(t.vendorUsageIntroSpend)).not.toBeInTheDocument();
      });

      it('formats the used amount to one decimal in the portal locale, without a thousands separator', () => {
        renderPanel(makeBusinessUsage({ spend_used: '1500.25' }));
        expect(screen.getByText(spend.fractional)).toBeInTheDocument();

        cleanup();
        renderPanel(makeBusinessUsage({ spend_used: '0', spend_used_pct: 0 }));
        expect(screen.getByText(spend.zero)).toBeInTheDocument();
      });

      it.each(['n/a', 'NaN', 'Infinity', '0x10', '1e3', '12abc', '99999999999999999999'])(
        'shows a spend_used that is not a plain finite number raw, without crashing: %s',
        (raw) => {
          renderPanel(makeBusinessUsage({ spend_used: raw }));

          expect(screen.getByText(`${raw} / 6000 Credits (${spend.pct})`)).toBeInTheDocument();
        },
      );

      it('names the unit the vendor reported when it is not the plain credit', () => {
        renderPanel(makeBusinessUsage({ spend_unit: 'usd' }));
        expect(screen.getByText(spend.line.replace('Credits', 'usd'))).toBeInTheDocument();

        cleanup();
        // An unknown unit falls back to "Credits".
        renderPanel(makeBusinessUsage({ spend_unit: '' }));
        expect(screen.getByText(spend.line)).toBeInTheDocument();
      });

      it('colors the credit bar amber then red as it fills, like a window', () => {
        const cls = () =>
          screen
            .getByRole('progressbar', { name: t.vendorUsageSpendLabel })
            .getAttribute('class') ?? '';

        renderPanel(makeBusinessUsage({ spend_used_pct: 80 }));
        expect(cls()).toContain('colorWarning');
        cleanup();
        renderPanel(makeBusinessUsage({ spend_used_pct: 95 }));
        expect(cls()).toContain('colorError');
        cleanup();
        renderPanel(makeBusinessUsage({ spend_used_pct: 10 }));
        expect(cls()).toContain('colorPrimary');
      });

      it('shows the amounts without a bar or a percentage when the percentage is unknown (-1)', () => {
        renderPanel(makeBusinessUsage({ spend_used_pct: -1 }));

        expect(screen.getByText(spend.noPct)).toBeInTheDocument();
        expect(screen.queryByRole('progressbar')).not.toBeInTheDocument();
      });

      it('shows a known percentage even when the vendor sent no amounts', () => {
        renderPanel(
          makeBusinessUsage({
            spend_limit: '',
            spend_used: '',
            spend_remaining: '',
            spend_used_pct: 12,
          }),
        );

        expect(screen.getByText(t.vendorUsagePercentUsed(12))).toBeInTheDocument();
        expect(screen.getByRole('progressbar', { name: t.vendorUsageSpendLabel })).toHaveAttribute(
          'aria-valuenow',
          '12',
        );
      });

      it('shows the panel and the amount when only the limit or only the used amount is known', () => {
        renderPanel(makeBusinessUsage({ spend_used: '', spend_remaining: '', spend_used_pct: -1 }));
        expect(screen.getByRole('heading', { name: t.vendorUsageTitle })).toBeInTheDocument();
        expect(screen.getByText(t.vendorUsageSpendLimit('6000', 'Credits'))).toBeInTheDocument();

        cleanup();
        renderPanel(
          makeBusinessUsage({ spend_limit: '', spend_remaining: '', spend_used_pct: -1 }),
        );
        expect(screen.getByRole('heading', { name: t.vendorUsageTitle })).toBeInTheDocument();
        expect(screen.getByText(t.vendorUsageSpendUsed(spend.used, 'Credits'))).toBeInTheDocument();
      });

      it('says the allowance has reset once its reset time has passed, and omits the line without one', () => {
        renderPanel(makeBusinessUsage({ spend_reset_at: at(-HOUR) }));
        expect(screen.getByText(t.vendorUsageResetPassed)).toBeInTheDocument();

        cleanup();
        renderPanel(makeBusinessUsage({ spend_reset_at: null }));
        expect(screen.getByText(spend.line)).toBeInTheDocument();
        expect(screen.queryByText(/6 d/)).not.toBeInTheDocument();
        expect(screen.queryByText(t.vendorUsageResetPassed)).not.toBeInTheDocument();
      });

      it('shows "Unlimited" for an unlimited plan that reports no amounts', () => {
        renderPanel(
          makeBusinessUsage({
            spend_unit: '',
            spend_limit: '',
            spend_used: '',
            spend_remaining: '',
            spend_used_pct: -1,
            spend_reset_at: null,
            credit_status: 'unlimited',
          }),
        );

        expect(screen.getByRole('heading', { name: t.vendorUsageTitle })).toBeInTheDocument();
        expect(screen.getByText(spend.unlimited)).toBeInTheDocument();
        expect(screen.queryByText(spend.none)).not.toBeInTheDocument();
        expect(screen.queryByRole('progressbar')).not.toBeInTheDocument();
      });

      it('shows "No credits" when the plan reports none and there is no spend or balance', () => {
        renderPanel(
          makeUsage({
            five_hour_pct: -1,
            weekly_pct: -1,
            credit_status: 'none',
          }),
        );

        expect(screen.getByRole('heading', { name: t.vendorUsageTitle })).toBeInTheDocument();
        expect(screen.getByText(spend.none)).toBeInTheDocument();
        expect(screen.queryByText(spend.unlimited)).not.toBeInTheDocument();
      });

      it('shows nothing special for has_credits without spend data or balance, but keeps the panel', () => {
        renderPanel(makeUsage({ five_hour_pct: -1, weekly_pct: -1, credit_status: 'has_credits' }));

        expect(screen.getByRole('heading', { name: t.vendorUsageTitle })).toBeInTheDocument();
        expect(screen.queryByText(spend.unlimited)).not.toBeInTheDocument();
        expect(screen.queryByText(spend.none)).not.toBeInTheDocument();
        expect(screen.queryByText(t.vendorUsageCreditBalance)).not.toBeInTheDocument();
        expect(screen.queryByText(t.vendorUsageSpendLabel)).not.toBeInTheDocument();
      });

      it('lets an unlimited plan win over a stored balance, and the balance win over "No credits"', () => {
        renderPanel(makeUsage({ credit_status: 'unlimited', credit_balance: '9.00' }));
        expect(screen.getByText(spend.unlimited)).toBeInTheDocument();
        expect(screen.queryByText(t.vendorUsageCreditBalance)).not.toBeInTheDocument();

        cleanup();
        renderPanel(makeUsage({ credit_status: 'none', credit_balance: '9.00' }));
        expect(screen.getByText(t.vendorUsageCreditBalance)).toBeInTheDocument();
        expect(screen.getByText('9.00')).toBeInTheDocument();
        expect(screen.queryByText(spend.none)).not.toBeInTheDocument();
      });

      it('lets the spend line win over the credit flags', () => {
        renderPanel(makeBusinessUsage({ credit_status: 'unlimited' }));

        expect(screen.getByText(spend.line)).toBeInTheDocument();
        expect(screen.queryByText(spend.unlimited)).not.toBeInTheDocument();
      });

      it('leaves an old-style snapshot (windows and balance, no spend fields) exactly as it was', () => {
        renderPanel(makeUsage({ credit_balance: '12.34' }));

        expect(
          screen.getByRole('progressbar', { name: t.vendorUsageFiveHour }),
        ).toBeInTheDocument();
        expect(screen.getByRole('progressbar', { name: t.vendorUsageWeekly })).toBeInTheDocument();
        expect(screen.getByText(t.vendorUsageCreditBalance)).toBeInTheDocument();
        expect(screen.getByText('12.34')).toBeInTheDocument();
        expect(screen.queryByText(t.vendorUsageSpendLabel)).not.toBeInTheDocument();
        expect(screen.queryByText(spend.unlimited)).not.toBeInTheDocument();
        expect(screen.queryByText(spend.none)).not.toBeInTheDocument();
        expect(screen.getAllByRole('progressbar')).toHaveLength(2);
      });
    });
  });
}
