// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { VendorAccountGroupsSection } from './VendorAccountGroupsSection';
import { messages, type Locale } from '../i18n';
import { PortalApiError } from '../api';
import type { VendorAccount, VendorAccountGroup, VendorAccountGroupMember } from '../api';

afterEach(() => cleanup());

function member(overrides: Partial<VendorAccountGroupMember> = {}): VendorAccountGroupMember {
  return {
    account_id: 'va_a',
    account_name: 'Max A',
    auth_type: 'subscription',
    owner_name: '',
    position: 1,
    state: 'active',
    reason: '',
    cooldowns: [],
    ...overrides,
  };
}

function makeGroup(overrides: Partial<VendorAccountGroup> = {}): VendorAccountGroup {
  return {
    id: 'vag_1',
    vendor: 'anthropic',
    name: 'Team Claude',
    model_prefix: '',
    status: 'active',
    thresholds: { five_hour_pct: 99, weekly_pct: 90, credits_pct: 99 },
    active_member_id: 'va_a',
    members: [
      member(),
      member({
        account_id: 'va_b',
        account_name: 'Max B',
        position: 2,
        state: 'not_usable',
        reason: 'needs_reconnect',
      }),
    ],
    created_at: '2026-11-24T10:00:00Z',
    updated_at: '2026-11-24T10:00:00Z',
    ...overrides,
  };
}

function account(id: string, name: string, vendor: string, extra: Partial<VendorAccount> = {}) {
  return {
    id,
    name,
    vendor,
    auth_type: 'api_key',
    is_owner: true,
    shared_by: '',
    ...extra,
  } as VendorAccount;
}

const ACCOUNTS = [
  account('va_a', 'Max A', 'anthropic'),
  account('va_b', 'Max B', 'anthropic'),
  account('va_c', 'Spare C', 'anthropic'),
  account('va_s', 'Shared S', 'anthropic', { is_owner: false, shared_by: 'Alice' }),
  account('va_o', 'OpenAI O', 'openai'),
];

function makeApi(groups: VendorAccountGroup[], overrides: Record<string, unknown> = {}) {
  return {
    vendorAccountGroups: vi.fn(async () => ({ groups })),
    vendorAccounts: vi.fn(async () => ({ data: ACCOUNTS })),
    createVendorAccountGroup: vi.fn(async () => makeGroup()),
    updateVendorAccountGroup: vi.fn(async () => makeGroup()),
    deleteVendorAccountGroup: vi.fn(async () => ({ ok: true })),
    setVendorAccountGroupMembers: vi.fn(async () => makeGroup()),
    ...overrides,
  };
}

for (const locale of ['de', 'en'] as readonly Locale[]) {
  const t = messages[locale];

  function renderSection(api: ReturnType<typeof makeApi>) {
    return render(<VendorAccountGroupsSection t={t} api={api} />);
  }

  describe(`VendorAccountGroupsSection [${locale}]`, () => {
    it('renders the groups with their members and member states', async () => {
      const usage = {
        five_hour_pct: 42,
        five_hour_reset_at: null,
        weekly_pct: -1,
        weekly_reset_at: null,
        credit_balance: '',
        spend_unit: '',
        spend_limit: '',
        spend_used: '',
        spend_remaining: '',
        spend_used_pct: -1,
        spend_reset_at: null,
        credit_status: '',
        updated_at: new Date().toISOString(),
      };
      const group = makeGroup({
        members: [
          member({ usage }),
          member({
            account_id: 'va_b',
            account_name: 'Max B',
            owner_name: 'Alice',
            position: 2,
            state: 'not_usable',
            reason: 'needs_reconnect',
          }),
        ],
      });
      renderSection(makeApi([group]));

      expect(await screen.findByRole('region', { name: t.vendorGroupsTitle })).toBeTruthy();
      expect(await screen.findByText('Team Claude')).toBeTruthy();
      expect(screen.getByText('Max A')).toBeTruthy();
      expect(screen.getByText(`Max B (${t.vendorAccountSharedBy} Alice)`)).toBeTruthy();
      expect(screen.getByText(t.vendorGroupStateActive).getAttribute('data-status')).toBe('active');
      expect(screen.getByText(t.vendorGroupStateNotUsable)).toBeTruthy();
      expect(screen.getByText(t.vendorGroupReasonNeedsReconnect)).toBeTruthy();
      expect(screen.getByText(t.vendorUsageFiveHour)).toBeTruthy();
    });

    it('shows the empty state without groups', async () => {
      renderSection(makeApi([]));
      expect(await screen.findByText(t.vendorGroupsEmpty)).toBeTruthy();
    });

    it('creates a group with the default thresholds of 99', async () => {
      const api = makeApi([]);
      renderSection(api);
      fireEvent.click(await screen.findByRole('button', { name: t.vendorGroupCreate }));
      const dialog = await screen.findByRole('dialog');
      fireEvent.change(within(dialog).getByLabelText(t.vendorAccountNameLabel), {
        target: { value: 'Fresh' },
      });
      fireEvent.mouseDown(within(dialog).getByLabelText(t.vendorAccountVendorLabel));
      fireEvent.click(await screen.findByRole('option', { name: t.vendorAnthropic }));
      fireEvent.click(within(dialog).getByRole('button', { name: t.save }));

      await waitFor(() => expect(api.createVendorAccountGroup).toHaveBeenCalledTimes(1));
      expect(api.createVendorAccountGroup).toHaveBeenCalledWith({
        name: 'Fresh',
        vendor: 'anthropic',
        model_prefix: '',
        status: 'active',
        threshold_five_hour_pct: 99,
        threshold_weekly_pct: 99,
        threshold_credits_pct: 99,
        account_ids: [],
      });
    });

    it('sends the reordered account_ids when a member is moved down', async () => {
      const api = makeApi([makeGroup()]);
      renderSection(api);
      fireEvent.click(
        await screen.findByRole('button', { name: `${t.vendorGroupActionEdit} Team Claude` }),
      );
      const dialog = await screen.findByRole('dialog');
      fireEvent.click(
        within(dialog).getByRole('button', { name: t.vendorGroupMemberMoveDown('Max A') }),
      );
      fireEvent.click(within(dialog).getByRole('button', { name: t.save }));

      await waitFor(() =>
        expect(api.setVendorAccountGroupMembers).toHaveBeenCalledWith('vag_1', ['va_b', 'va_a']),
      );
    });

    it('offers only accounts of the group vendor that are not yet members', async () => {
      const api = makeApi([makeGroup()]);
      renderSection(api);
      fireEvent.click(
        await screen.findByRole('button', { name: `${t.vendorGroupActionEdit} Team Claude` }),
      );
      const dialog = await screen.findByRole('dialog');
      fireEvent.mouseDown(within(dialog).getByLabelText(t.vendorGroupMemberAddLabel));
      const options = (await screen.findAllByRole('option')).map((o) => o.textContent);
      expect(options).toEqual(['Spare C', `Shared S (${t.vendorAccountSharedBy} Alice)`]);
    });

    it('shows the translated message for a prefix conflict', async () => {
      const api = makeApi([makeGroup()], {
        updateVendorAccountGroup: vi.fn(async () => {
          throw new PortalApiError(409, 'vendor_account_group.prefix_conflict', 'conflict');
        }),
      });
      renderSection(api);
      fireEvent.click(
        await screen.findByRole('button', { name: `${t.vendorGroupActionEdit} Team Claude` }),
      );
      const dialog = await screen.findByRole('dialog');
      fireEvent.click(within(dialog).getByRole('button', { name: t.save }));

      expect(
        await within(dialog).findByText(new RegExp(t.errorVendorAccountGroupPrefixConflict)),
      ).toBeTruthy();
    });

    it('deletes a group after confirmation', async () => {
      const api = makeApi([makeGroup()]);
      renderSection(api);
      fireEvent.click(
        await screen.findByRole('button', { name: `${t.vendorAccountActionDelete} Team Claude` }),
      );
      expect(api.deleteVendorAccountGroup).not.toHaveBeenCalled();
      const dialog = await screen.findByRole('dialog');
      fireEvent.click(within(dialog).getByRole('button', { name: t.vendorAccountActionDelete }));
      await waitFor(() => expect(api.deleteVendorAccountGroup).toHaveBeenCalledWith('vag_1'));
    });
  });
}

describe('vendor group labels', () => {
  it('exist in German and English', () => {
    expect(messages.de.vendorGroupsTitle).toBe('Gruppen');
    expect(messages.en.vendorGroupsTitle).toBe('Groups');
    expect(messages.de.vendorGroupThresholdHint).not.toBe(messages.en.vendorGroupThresholdHint);
  });
});
