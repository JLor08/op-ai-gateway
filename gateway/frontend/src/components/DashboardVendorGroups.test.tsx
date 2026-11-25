// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { DashboardVendorGroups } from './DashboardVendorGroups';
import { messages, type Locale } from '../i18n';
import type { VendorAccountGroup, VendorAccountGroupMember } from '../api';
import type { PortalApi } from './shared/types';

afterEach(cleanup);

const FREE_AT = '2026-11-24T14:20:00Z';
const COOL_UNTIL = '2026-11-24T14:05:00Z';
const localTime = (iso: string) =>
  new Date(iso).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });

function member(overrides: Partial<VendorAccountGroupMember> = {}): VendorAccountGroupMember {
  return {
    account_id: 'va_1',
    account_name: 'Max',
    auth_type: 'subscription',
    owner_name: '',
    position: 1,
    state: 'available',
    reason: '',
    cooldowns: [],
    ...overrides,
  };
}

function group(overrides: Partial<VendorAccountGroup> = {}): VendorAccountGroup {
  return {
    id: 'vag_1',
    vendor: 'anthropic',
    name: 'Claude pool',
    model_prefix: '',
    status: 'active',
    thresholds: { five_hour_pct: 99, weekly_pct: 99, credits_pct: 99 },
    active_member_id: 'va_1',
    members: [member({ state: 'active' })],
    created_at: '2026-11-24T10:00:00Z',
    updated_at: '2026-11-24T10:00:00Z',
    ...overrides,
  };
}

function renderGroups(t: (typeof messages)[Locale], groups: VendorAccountGroup[]) {
  const vendorAccountGroups = vi.fn().mockResolvedValue({ groups });
  const api = { vendorAccountGroups } as unknown as Pick<PortalApi, 'vendorAccountGroups'>;
  const view = render(<DashboardVendorGroups t={t} api={api} />);
  return { ...view, vendorAccountGroups };
}

function cardOf(name: string): HTMLElement {
  const card = screen.getByRole('heading', { level: 3, name }).closest('li');
  expect(card).not.toBeNull();
  return card as HTMLElement;
}

for (const locale of ['de', 'en'] as readonly Locale[]) {
  const t = messages[locale];

  describe(`DashboardVendorGroups [${locale}]`, () => {
    it('shows the group with its active member and an active line', async () => {
      renderGroups(t, [group()]);
      const card = await waitFor(() => cardOf('Claude pool'));
      expect(within(card).getByText(t.dashboardVendorGroupActiveMember('Max'))).toBeVisible();
      expect(within(card).getByText(t.dashboardVendorGroupLineActive)).toBeVisible();
    });

    it('shows category, percent and the local free-at time of an over-threshold member', async () => {
      renderGroups(t, [
        group({
          active_member_id: 'va_2',
          members: [
            member({
              state: 'over_threshold',
              reason: 'five_hour',
              pct: 99,
              free_at: FREE_AT,
              usage: {
                five_hour_pct: 99,
                five_hour_reset_at: FREE_AT,
                weekly_pct: 10,
                weekly_reset_at: null,
                credit_balance: '',
                spend_unit: '',
                spend_limit: '',
                spend_used: '',
                spend_remaining: '',
                spend_used_pct: -1,
                spend_reset_at: null,
                credit_status: '',
                updated_at: FREE_AT,
              },
            }),
            member({ account_id: 'va_2', account_name: 'Eva', position: 2, state: 'active' }),
          ],
        }),
      ]);
      const card = await waitFor(() => cardOf('Claude pool'));
      expect(
        within(card).getByText(
          t.dashboardVendorGroupLineOverThreshold(
            `${t.dashboardVendorGroupWindowFiveHour} 99 %`,
            localTime(FREE_AT),
          ),
          { exact: false },
        ),
      ).toBeVisible();
    });

    it('shows the category from the reason when the usage snapshot is absent', async () => {
      renderGroups(t, [
        group({
          members: [
            member({ state: 'over_threshold', reason: 'weekly', pct: 97, free_at: FREE_AT }),
          ],
        }),
      ]);
      const card = await waitFor(() => cardOf('Claude pool'));
      expect(
        within(card).getByText(
          t.dashboardVendorGroupLineOverThreshold(
            `${t.dashboardVendorGroupWindowWeekly} 97 %`,
            localTime(FREE_AT),
          ),
          { exact: false },
        ),
      ).toBeVisible();
    });

    it('labels a weekly block as weekly even when the five-hour percent is equal', async () => {
      const usage = {
        five_hour_pct: 100,
        five_hour_reset_at: null,
        weekly_pct: 100,
        weekly_reset_at: FREE_AT,
        credit_balance: '',
        spend_unit: '',
        spend_limit: '',
        spend_used: '',
        spend_remaining: '',
        spend_used_pct: -1,
        spend_reset_at: null,
        credit_status: '',
        updated_at: FREE_AT,
      };
      renderGroups(t, [
        group({
          members: [
            member({
              state: 'over_threshold',
              reason: 'weekly',
              pct: 100,
              free_at: FREE_AT,
              usage,
            }),
          ],
        }),
      ]);
      const card = await waitFor(() => cardOf('Claude pool'));
      expect(
        within(card).getByText(
          t.dashboardVendorGroupLineOverThreshold(
            `${t.dashboardVendorGroupWindowWeekly} 100 %`,
            localTime(FREE_AT),
          ),
          { exact: false },
        ),
      ).toBeVisible();
    });

    it('falls back to the bare state for a cooldown without entries', async () => {
      renderGroups(t, [
        group({ members: [member({ account_name: 'Idle', state: 'cooldown', cooldowns: [] })] }),
      ]);
      const card = await waitFor(() => cardOf('Claude pool'));
      const row = within(card).getByText('Idle').closest('li') as HTMLElement;
      expect(row.textContent).toContain(t.vendorGroupStateCooldown);
    });

    it('names the model and the local time in a cooldown line', async () => {
      renderGroups(t, [
        group({
          members: [
            member({
              state: 'cooldown',
              cooldowns: [{ model: 'claude-opus-4-5', until: COOL_UNTIL }],
            }),
          ],
        }),
      ]);
      const card = await waitFor(() => cardOf('Claude pool'));
      expect(
        within(card).getByText(
          t.dashboardVendorGroupLineCooldown(localTime(COOL_UNTIL), 'claude-opus-4-5'),
        ),
      ).toBeVisible();
    });

    it('shows the state only for a member without figures, and the reason when not usable', async () => {
      renderGroups(t, [
        group({
          members: [
            member({ account_name: 'Ready one', state: 'available' }),
            member({
              account_id: 'va_3',
              account_name: 'Broken',
              position: 2,
              state: 'not_usable',
              reason: 'needs_reconnect',
            }),
            member({
              account_id: 'va_4',
              account_name: 'Silent',
              position: 3,
              state: 'over_threshold',
            }),
          ],
        }),
      ]);
      const card = await waitFor(() => cardOf('Claude pool'));
      const ready = within(card).getByText('Ready one').closest('li') as HTMLElement;
      expect(ready.textContent).toContain(t.dashboardVendorGroupLineReady);
      expect(ready.textContent).not.toContain('%');
      const broken = within(card).getByText('Broken').closest('li') as HTMLElement;
      expect(broken.textContent).toContain(
        t.dashboardVendorGroupLineNotUsable(t.vendorGroupReasonNeedsReconnect),
      );
      const silent = within(card).getByText('Silent').closest('li') as HTMLElement;
      expect(silent.textContent).toContain(t.dashboardVendorGroupLineOverThresholdBare);
      expect(silent.textContent).not.toContain('%');
    });

    it('renders nothing without groups and skips inactive groups', async () => {
      const { container, vendorAccountGroups } = renderGroups(t, [group({ status: 'disabled' })]);
      await waitFor(() => expect(vendorAccountGroups).toHaveBeenCalled());
      expect(container).toBeEmptyDOMElement();
      expect(screen.queryByRole('heading', { level: 3 })).not.toBeInTheDocument();
    });
  });
}
