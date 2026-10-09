// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { act, cleanup, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { Dashboard } from './Dashboard';
import { messages, type Locale } from '../i18n';
import type { DashboardResponse, VendorAccount, VendorAccountUsage } from '../api';
import type { PortalApi } from './shared/types';

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

const MIN = 60_000;
const HOUR = 60 * MIN;
const DAY = 24 * HOUR;
// Relative to the real clock: the dashboard does not inject `now` into the rows.
const at = (offsetMs: number) => new Date(Date.now() + offsetMs).toISOString();

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
function makeBusinessUsage(): VendorAccountUsage {
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
  });
}

// A snapshot the vendor said nothing usable about.
function makeEmptyUsage(overrides: Partial<VendorAccountUsage> = {}): VendorAccountUsage {
  return makeUsage({
    five_hour_pct: -1,
    five_hour_reset_at: null,
    weekly_pct: -1,
    weekly_reset_at: null,
    ...overrides,
  });
}

function makeAccount(overrides: Partial<VendorAccount> = {}): VendorAccount {
  return {
    id: 'va_1',
    vendor: 'openai',
    auth_type: 'api_key',
    name: 'Work OpenAI',
    status: 'active',
    model_prefix: '',
    api_key_set: true,
    subscription_connected: false,
    models: [],
    created_at: '2026-09-01T12:00:00Z',
    updated_at: '2026-09-01T12:00:00Z',
    ...overrides,
  };
}

const dashboard: DashboardResponse = {
  metrics: { requests_24h: 0, tokens_24h: 0, healthy_hosts: '1/1', latency_p95_ms: 0 },
  routes: [],
};

type UsageApi = Pick<PortalApi, 'vendorAccounts'>;

function makeApi(accounts: VendorAccount[]): {
  api: UsageApi;
  vendorAccounts: ReturnType<typeof vi.fn>;
} {
  const vendorAccounts = vi.fn().mockResolvedValue({ data: accounts });
  return { api: { vendorAccounts } as unknown as UsageApi, vendorAccounts };
}

function renderDashboard(
  t: (typeof messages)[Locale],
  props: { api?: UsageApi; vendorAccountsEnabled?: boolean },
) {
  return render(<Dashboard t={t} dashboard={dashboard} productName="X" {...props} />);
}

// The card of one account: the list item whose level-3 heading is the account name.
function cardOf(name: string): HTMLElement {
  const heading = screen.getByRole('heading', { level: 3, name });
  const card = heading.closest('li');
  expect(card).not.toBeNull();
  return card as HTMLElement;
}

function updatedPrefix(t: (typeof messages)[Locale]): string {
  return t.vendorUsageUpdatedAt('').trim();
}

for (const locale of ['de', 'en'] as readonly Locale[]) {
  const t = messages[locale];

  describe(`Dashboard vendor usage section [${locale}]`, () => {
    it('lists each usage-capable account with its visible rows', async () => {
      const { api, vendorAccounts } = makeApi([
        makeAccount({ id: 'va_oai', name: 'Work OpenAI', usage: makeUsage() }),
        makeAccount({
          id: 'va_ant',
          vendor: 'anthropic',
          auth_type: 'subscription',
          name: 'Team Claude Max',
          api_key_set: false,
          subscription_connected: true,
          // Only the weekly window was ever observed.
          usage: makeUsage({ five_hour_pct: -1, five_hour_reset_at: null, weekly_pct: 63 }),
        }),
      ]);
      renderDashboard(t, { api, vendorAccountsEnabled: true });

      const section = await screen.findByRole('region', { name: t.dashboardVendorUsageTitle });
      expect(within(section).getByRole('heading', { level: 2 })).toHaveTextContent(
        t.dashboardVendorUsageTitle,
      );
      expect(vendorAccounts).toHaveBeenCalledTimes(1);

      const openai = cardOf('Work OpenAI');
      expect(within(openai).getByText(t.vendorOpenAI)).toBeInTheDocument();
      expect(within(openai).getByText(t.vendorUsageFiveHour)).toBeInTheDocument();
      expect(within(openai).getByText(t.vendorUsagePercentUsed(42))).toBeInTheDocument();
      expect(within(openai).getByText(t.vendorUsageWeekly)).toBeInTheDocument();
      expect(within(openai).getByText(t.vendorUsagePercentUsed(7))).toBeInTheDocument();
      // Exactly one "last updated" caption per card: the rows component draws it.
      expect(within(openai).getAllByText(new RegExp(updatedPrefix(t)))).toHaveLength(1);

      const claude = cardOf('Team Claude Max');
      expect(within(claude).getByText(t.vendorAnthropic)).toBeInTheDocument();
      expect(within(claude).getByText(t.vendorUsageWeekly)).toBeInTheDocument();
      expect(within(claude).getByText(t.vendorUsagePercentUsed(63))).toBeInTheDocument();
      // No 5-hour row for a window the vendor never reported.
      expect(within(claude).queryByText(t.vendorUsageFiveHour)).toBeNull();
      expect(within(claude).getAllByText(new RegExp(updatedPrefix(t)))).toHaveLength(1);

      expect(within(section).getAllByRole('listitem')).toHaveLength(2);
    });

    it('draws the bars of a card dense (6px)', async () => {
      const { api } = makeApi([makeAccount({ usage: makeUsage() })]);
      renderDashboard(t, { api, vendorAccountsEnabled: true });
      const card = await waitFor(() => cardOf('Work OpenAI'));
      const bar = within(card).getByRole('progressbar', { name: t.vendorUsageFiveHour });
      expect(bar).toHaveStyle({ height: '6px' });
    });

    it('shows only the credit row for a Business account', async () => {
      const { api } = makeApi([
        makeAccount({
          id: 'va_biz',
          name: 'Codex Business',
          auth_type: 'subscription',
          api_key_set: false,
          subscription_connected: true,
          usage: makeBusinessUsage(),
        }),
      ]);
      renderDashboard(t, { api, vendorAccountsEnabled: true });

      const card = await waitFor(() => cardOf('Codex Business'));
      expect(within(card).getByText(t.vendorUsageSpendLabel)).toBeInTheDocument();
      expect(within(card).queryByText(t.vendorUsageFiveHour)).toBeNull();
      expect(within(card).queryByText(t.vendorUsageWeekly)).toBeNull();
      expect(within(card).getAllByRole('progressbar')).toHaveLength(1);
    });

    it('omits accounts without a visible row and accounts that cannot report usage', async () => {
      const { api } = makeApi([
        makeAccount({ id: 'va_shown', name: 'Shown', usage: makeUsage() }),
        // No snapshot at all.
        makeAccount({ id: 'va_none', name: 'No snapshot' }),
        // Every limit unknown.
        makeAccount({ id: 'va_empty', name: 'Nothing known', usage: makeEmptyUsage() }),
        // A bare has_credits flag has no row either.
        makeAccount({
          id: 'va_flag',
          name: 'Bare credits flag',
          usage: makeEmptyUsage({ credit_status: 'has_credits' }),
        }),
        // A subscription that is not connected cannot report usage, even with a
        // stale snapshot.
        makeAccount({
          id: 'va_sub',
          vendor: 'anthropic',
          auth_type: 'subscription',
          name: 'Not connected',
          api_key_set: false,
          subscription_connected: false,
          usage: makeUsage(),
        }),
      ]);
      renderDashboard(t, { api, vendorAccountsEnabled: true });

      await waitFor(() => cardOf('Shown'));
      expect(screen.getAllByRole('listitem')).toHaveLength(1);
      for (const name of ['No snapshot', 'Nothing known', 'Bare credits flag', 'Not connected']) {
        expect(screen.queryByText(name)).toBeNull();
      }
    });

    it('marks an account that is not active with a status chip, an active one with none', async () => {
      const { api } = makeApi([
        makeAccount({ id: 'va_a', name: 'Active one', usage: makeUsage() }),
        makeAccount({
          id: 'va_r',
          vendor: 'anthropic',
          auth_type: 'subscription',
          name: 'Needs reconnect',
          api_key_set: false,
          subscription_connected: true,
          status: 'needs_reconnect',
          usage: makeUsage(),
        }),
        makeAccount({ id: 'va_d', name: 'Switched off', status: 'disabled', usage: makeUsage() }),
      ]);
      renderDashboard(t, { api, vendorAccountsEnabled: true });

      await waitFor(() => cardOf('Active one'));
      const active = cardOf('Active one');
      expect(within(active).queryByText(t.statusActive)).toBeNull();
      expect(within(active).queryByText(t.statusDisabled)).toBeNull();
      expect(within(active).queryByText(t.vendorAccountStatusNeedsReconnect)).toBeNull();

      expect(
        within(cardOf('Needs reconnect')).getByText(t.vendorAccountStatusNeedsReconnect),
      ).toHaveAttribute('data-status', 'watch');
      expect(within(cardOf('Switched off')).getByText(t.statusDisabled)).toHaveAttribute(
        'data-status',
        'standby',
      );
    });

    it('is hidden, and never calls the API, when the vendor-accounts flag is off', async () => {
      const { api, vendorAccounts } = makeApi([makeAccount({ usage: makeUsage() })]);
      renderDashboard(t, { api, vendorAccountsEnabled: false });
      // Let any (wrongly) started effect and fetch settle.
      await act(async () => {
        await Promise.resolve();
      });
      expect(screen.queryByText(t.dashboardVendorUsageTitle)).toBeNull();
      expect(screen.queryByText('Work OpenAI')).toBeNull();
      expect(vendorAccounts).not.toHaveBeenCalled();
    });

    it('is hidden, and never calls the API, when the flag is not given', async () => {
      const { api, vendorAccounts } = makeApi([makeAccount({ usage: makeUsage() })]);
      renderDashboard(t, { api });
      await act(async () => {
        await Promise.resolve();
      });
      expect(screen.queryByText(t.dashboardVendorUsageTitle)).toBeNull();
      expect(vendorAccounts).not.toHaveBeenCalled();
    });

    it('is hidden without an api, even with the flag on', () => {
      renderDashboard(t, { vendorAccountsEnabled: true });
      expect(screen.queryByText(t.dashboardVendorUsageTitle)).toBeNull();
    });

    it('hides the whole section when no account has a visible row', async () => {
      const { api, vendorAccounts } = makeApi([
        makeAccount({ id: 'va_none', name: 'No snapshot' }),
        makeAccount({ id: 'va_empty', name: 'Nothing known', usage: makeEmptyUsage() }),
      ]);
      renderDashboard(t, { api, vendorAccountsEnabled: true });
      await waitFor(() => expect(vendorAccounts).toHaveBeenCalled());
      await act(async () => {
        await Promise.resolve();
      });
      expect(screen.queryByText(t.dashboardVendorUsageTitle)).toBeNull();
      expect(screen.queryByText(t.vendorUsageEmpty)).toBeNull();
      // The rest of the dashboard is untouched.
      expect(screen.getByRole('table')).toBeInTheDocument();
    });

    it('hides the section, and leaves the dashboard standing, when the read fails', async () => {
      const vendorAccounts = vi.fn().mockRejectedValue(new Error('boom'));
      const api = { vendorAccounts } as unknown as UsageApi;
      renderDashboard(t, { api, vendorAccountsEnabled: true });
      await waitFor(() => expect(vendorAccounts).toHaveBeenCalled());
      await act(async () => {
        await Promise.resolve();
      });
      expect(screen.queryByText(t.dashboardVendorUsageTitle)).toBeNull();
      expect(screen.getByRole('table')).toBeInTheDocument();
    });
  });

  describe(`Dashboard vendor usage section: reads only the stored snapshot [${locale}]`, () => {
    beforeEach(() => {
      vi.useFakeTimers({ shouldAdvanceTime: true });
    });

    it('re-reads the list on an interval and never asks the vendor to refresh', async () => {
      const refreshUsage = vi.fn();
      const vendorAccounts = vi
        .fn()
        .mockResolvedValueOnce({
          data: [makeAccount({ usage: makeUsage({ five_hour_pct: 42 }) })],
        })
        .mockResolvedValue({
          data: [makeAccount({ usage: makeUsage({ five_hour_pct: 55 }) })],
        });
      const api = { vendorAccounts, refreshUsage } as unknown as UsageApi;
      renderDashboard(t, { api, vendorAccountsEnabled: true });

      expect(await screen.findByText(t.vendorUsagePercentUsed(42))).toBeInTheDocument();
      expect(vendorAccounts).toHaveBeenCalledTimes(1);

      await act(async () => {
        await vi.advanceTimersByTimeAsync(60_000);
      });
      expect(await screen.findByText(t.vendorUsagePercentUsed(55))).toBeInTheDocument();
      expect(vendorAccounts.mock.calls.length).toBeGreaterThanOrEqual(2);
      expect(refreshUsage).not.toHaveBeenCalled();
    });
  });
}
