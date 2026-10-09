// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import type { ComponentProps } from 'react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { VendorAccountUsagePanel } from './VendorAccountUsagePanel';
import { ToastProvider } from './shared/ToastProvider';
import { messages, type Locale } from '../i18n';
import { PortalApiError } from '../api';
import type { VendorAccountUsage, VendorUsageRefresh } from '../api';

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

  // The "retired" placeholder of a limit the vendor does not provide, per locale:
  // it must never come back now that such a limit has no row at all.
  const noData = locale === 'de' ? 'Noch keine Daten' : 'No data yet';

  // The panel can toast a failed refresh, so it always renders inside a provider.
  function renderPanel(
    usage: VendorAccountUsage | null | undefined,
    extra: Partial<ComponentProps<typeof VendorAccountUsagePanel>> = {},
  ) {
    return render(
      <ToastProvider>
        <VendorAccountUsagePanel t={t} usage={usage} now={NOW} {...extra} />
      </ToastProvider>,
    );
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
      expect(screen.queryByText(noData)).not.toBeInTheDocument();
    });

    it('draws no row at all for a window that was never observed (-1), not even a placeholder', () => {
      renderPanel(makeUsage({ weekly_pct: -1, weekly_reset_at: null }));

      expect(screen.getByRole('progressbar', { name: t.vendorUsageFiveHour })).toBeInTheDocument();
      expect(
        screen.queryByRole('progressbar', { name: t.vendorUsageWeekly }),
      ).not.toBeInTheDocument();
      expect(screen.queryByText(t.vendorUsageWeekly)).not.toBeInTheDocument();
      expect(screen.queryByText(noData)).not.toBeInTheDocument();
      // The unknown window is not a "0 % used".
      expect(screen.queryByText(t.vendorUsagePercentUsed(0))).not.toBeInTheDocument();
      expect(screen.queryByText(t.vendorUsagePercentUsed(-1))).not.toBeInTheDocument();
    });

    it('draws only the window the vendor reported when the 5-hour one is unknown', () => {
      renderPanel(makeUsage({ five_hour_pct: -1, five_hour_reset_at: null }));

      expect(screen.getByRole('progressbar', { name: t.vendorUsageWeekly })).toBeInTheDocument();
      expect(screen.queryByText(t.vendorUsageFiveHour)).not.toBeInTheDocument();
      expect(screen.getAllByRole('progressbar')).toHaveLength(1);
    });

    it('keeps the titled frame with an empty-state line when the snapshot knows no window, no balance, no spend and no credit status', () => {
      renderPanel(
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

      expect(screen.getByRole('region', { name: t.vendorUsageTitle })).toBeInTheDocument();
      expect(screen.getByText(t.vendorUsageEmpty)).toBeInTheDocument();
      expect(screen.queryByRole('progressbar')).not.toBeInTheDocument();
      // No stale caption for rows that are not there.
      expect(
        screen.queryByText(t.vendorUsageUpdatedAt(t.activityRelativeTime(5 * 60))),
      ).not.toBeInTheDocument();
    });

    it('keeps the titled frame with an empty-state line without a snapshot', () => {
      renderPanel(undefined);
      expect(screen.getByRole('region', { name: t.vendorUsageTitle })).toBeInTheDocument();
      expect(screen.getByText(t.vendorUsageEmpty)).toBeInTheDocument();

      cleanup();
      renderPanel(null);
      expect(screen.getByRole('region', { name: t.vendorUsageTitle })).toBeInTheDocument();
      expect(screen.getByText(t.vendorUsageEmpty)).toBeInTheDocument();
    });

    it('withholds the empty-state line while the first read of the snapshot is still pending', () => {
      renderPanel(null, { pending: true });

      expect(screen.getByRole('region', { name: t.vendorUsageTitle })).toBeInTheDocument();
      expect(screen.queryByText(t.vendorUsageEmpty)).not.toBeInTheDocument();
    });

    it('shows no empty-state line next to rows', () => {
      renderPanel(makeUsage());

      expect(screen.queryByText(t.vendorUsageEmpty)).not.toBeInTheDocument();
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
      // Only the credit row: the unknown windows have no rows, not "no data" ones.
      expect(screen.queryByText(t.vendorUsageFiveHour)).not.toBeInTheDocument();
      expect(screen.queryByText(t.vendorUsageWeekly)).not.toBeInTheDocument();
      expect(screen.queryByText(noData)).not.toBeInTheDocument();
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
        // ONLY the credit row: the windows the vendor does not provide have no row
        // at all (no 0 % bar, no "no data yet" placeholder).
        expect(screen.queryByRole('progressbar', { name: t.vendorUsageFiveHour })).toBeNull();
        expect(screen.queryByRole('progressbar', { name: t.vendorUsageWeekly })).toBeNull();
        expect(screen.queryByText(t.vendorUsageFiveHour)).not.toBeInTheDocument();
        expect(screen.queryByText(t.vendorUsageWeekly)).not.toBeInTheDocument();
        expect(screen.queryByText(noData)).not.toBeInTheDocument();
        expect(screen.getAllByRole('progressbar')).toHaveLength(1);
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

      it('shows no rows for has_credits without spend data or balance, only the frame with its empty-state line', () => {
        renderPanel(makeUsage({ five_hour_pct: -1, weekly_pct: -1, credit_status: 'has_credits' }));

        expect(screen.getByRole('heading', { name: t.vendorUsageTitle })).toBeInTheDocument();
        expect(screen.getByText(t.vendorUsageEmpty)).toBeInTheDocument();
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

      it('lets the spend line win over a stored credit balance', () => {
        renderPanel(makeBusinessUsage({ credit_balance: '9.00' }));

        expect(screen.getByText(spend.line)).toBeInTheDocument();
        expect(screen.queryByText(t.vendorUsageCreditBalance)).not.toBeInTheDocument();
        expect(screen.queryByText('9.00')).not.toBeInTheDocument();
      });

      it('still shows the panel for spend data alone: no window, no balance and an empty credit status', () => {
        // The backend emits credit_status "" when the credits flags are absent
        // while spend_control is present, so the spend data is the ONLY reason
        // the panel may show. This pins the spend clause of the render guard.
        renderPanel(makeBusinessUsage({ credit_status: '' }));

        expect(screen.getByRole('heading', { name: t.vendorUsageTitle })).toBeInTheDocument();
        expect(screen.getByText(spend.line)).toBeInTheDocument();
        expect(screen.getByText(t.vendorUsageResetsIn('6 d 4 h'))).toBeInTheDocument();
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

    describe('refresh button', () => {
      const answer = (
        status: VendorUsageRefresh['status'],
        detail = 'raw English detail',
      ): VendorUsageRefresh => ({ status, detail });
      const button = () => screen.getByRole('button', { name: t.vendorUsageRefreshAction });
      const panel = () => screen.getByRole('region', { name: t.vendorUsageTitle });

      it('shows no button for an account without an active usage pull', () => {
        renderPanel(makeUsage());

        expect(
          screen.queryByRole('button', { name: t.vendorUsageRefreshAction }),
        ).not.toBeInTheDocument();
      });

      it('shows the frame, the empty-state line and the button even without a snapshot', () => {
        renderPanel(null, { onRefresh: vi.fn(async () => answer('ok')) });

        expect(within(panel()).getByText(t.vendorUsageEmpty)).toBeInTheDocument();
        expect(
          within(panel()).getByRole('button', { name: t.vendorUsageRefreshAction }),
        ).toBeEnabled();
      });

      it('keeps the button next to the rows once there is a snapshot', () => {
        renderPanel(makeUsage(), { onRefresh: vi.fn(async () => answer('ok')) });

        expect(
          within(panel()).getByRole('button', { name: t.vendorUsageRefreshAction }),
        ).toBeEnabled();
        expect(
          screen.getByRole('progressbar', { name: t.vendorUsageFiveHour }),
        ).toBeInTheDocument();
      });

      it('asks the parent to refresh once per click and is disabled with a spinner while it runs', async () => {
        let resolve!: (value: VendorUsageRefresh) => void;
        const onRefresh = vi.fn(
          () =>
            new Promise<VendorUsageRefresh>((r) => {
              resolve = r;
            }),
        );
        renderPanel(makeUsage(), { onRefresh });

        fireEvent.click(button());

        expect(onRefresh).toHaveBeenCalledTimes(1);
        expect(button()).toBeDisabled();
        // The button's spinner is a circular progressbar next to the two bars.
        expect(within(button()).getByRole('progressbar')).toBeInTheDocument();
        fireEvent.click(button());
        expect(onRefresh).toHaveBeenCalledTimes(1);

        await act(async () => resolve(answer('ok')));
        expect(button()).toBeEnabled();
        expect(within(button()).queryByRole('progressbar')).not.toBeInTheDocument();
      });

      it.each(['ok', 'fresh'] as const)('reads a %s answer as a plain success', async (status) => {
        renderPanel(makeUsage(), { onRefresh: vi.fn(async () => answer(status)) });

        fireEvent.click(button());

        const note = await screen.findByRole('status');
        expect(within(note).getByText(t.vendorUsageRefreshed)).toBeInTheDocument();
        expect(screen.queryByText(t.vendorUsageRefreshUnchanged)).not.toBeInTheDocument();
        // The raw English detail is never what the user reads.
        expect(screen.queryByText(/raw English detail/)).not.toBeInTheDocument();
      });

      it('reads an unverifiable answer as a gentle note that the figures are unchanged, not as an error', async () => {
        renderPanel(makeUsage(), { onRefresh: vi.fn(async () => answer('unverifiable')) });

        fireEvent.click(button());

        const note = await screen.findByRole('status');
        expect(within(note).getByText(t.vendorUsageRefreshUnverifiable)).toBeInTheDocument();
        expect(within(note).getByText(t.vendorUsageRefreshUnchanged)).toBeInTheDocument();
        expect(screen.queryByRole('alert')).not.toBeInTheDocument();
        expect(screen.queryByText(/raw English detail/)).not.toBeInTheDocument();
      });

      it('reads an unsupported answer as a gentle note', async () => {
        renderPanel(null, { onRefresh: vi.fn(async () => answer('unsupported')) });

        fireEvent.click(button());

        const note = await screen.findByRole('status');
        expect(within(note).getByText(t.vendorUsageRefreshUnsupported)).toBeInTheDocument();
        expect(screen.queryByRole('alert')).not.toBeInTheDocument();
      });

      it('reads a status a newer backend adds as the neutral unverifiable note', async () => {
        renderPanel(makeUsage(), {
          onRefresh: vi.fn(async () => answer('something-new' as VendorUsageRefresh['status'])),
        });

        fireEvent.click(button());

        const note = await screen.findByRole('status');
        expect(within(note).getByText(t.vendorUsageRefreshUnverifiable)).toBeInTheDocument();
      });

      it('toasts the localized error code when the call fails, and leaves the button usable', async () => {
        const onRefresh = vi
          .fn<() => Promise<VendorUsageRefresh>>()
          .mockRejectedValueOnce(
            new PortalApiError(500, 'vendor_account.usage_refresh_failed', 'raw server text'),
          )
          .mockResolvedValueOnce(answer('ok'));
        renderPanel(makeUsage(), { onRefresh });

        fireEvent.click(button());

        const toast = await screen.findByRole('alert');
        expect(toast).toHaveTextContent(t.errorVendorAccountUsageRefreshFailed);
        expect(toast).not.toHaveTextContent('raw server text');
        expect(screen.queryByRole('status')).not.toBeInTheDocument();
        await waitFor(() => expect(button()).toBeEnabled());

        // The next click works and clears nothing it should not.
        fireEvent.click(button());
        expect(await screen.findByRole('status')).toBeInTheDocument();
        expect(onRefresh).toHaveBeenCalledTimes(2);
      });

      it('drops the previous outcome as soon as the next refresh starts', async () => {
        let resolve!: (value: VendorUsageRefresh) => void;
        const onRefresh = vi
          .fn<() => Promise<VendorUsageRefresh>>()
          .mockResolvedValueOnce(answer('unverifiable'))
          .mockImplementationOnce(
            () =>
              new Promise<VendorUsageRefresh>((r) => {
                resolve = r;
              }),
          );
        renderPanel(makeUsage(), { onRefresh });

        fireEvent.click(button());
        await screen.findByText(t.vendorUsageRefreshUnverifiable);
        fireEvent.click(button());

        expect(screen.queryByText(t.vendorUsageRefreshUnverifiable)).not.toBeInTheDocument();
        await act(async () => resolve(answer('ok')));
        expect(await screen.findByText(t.vendorUsageRefreshed)).toBeInTheDocument();
      });
    });
  });
}
