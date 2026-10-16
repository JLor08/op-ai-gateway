// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, it, expect, afterEach, vi } from 'vitest';
import { render, screen, cleanup, waitFor, fireEvent } from '@testing-library/react';
import { VendorAccountResourceGroupsSection } from './VendorAccountResourceGroupsSection';
import { messages, type Locale } from '../i18n';
import type { VendorAccount, VendorAccountResourceGroup } from '../api';

afterEach(() => cleanup());

const account = { id: 'va-x', name: 'Team OpenAI' } as unknown as VendorAccount;

function makeApi(
  overrides: Partial<{
    vendorAccountResourceGroups: (id: string) => Promise<VendorAccountResourceGroup[]>;
    joinVendorAccountResourceGroup: (id: string, rgId: string) => Promise<{ ok: boolean }>;
    leaveVendorAccountResourceGroup: (id: string, rgId: string) => Promise<{ ok: boolean }>;
  }>,
) {
  return {
    vendorAccountResourceGroups: vi.fn(async () => [] as VendorAccountResourceGroup[]),
    joinVendorAccountResourceGroup: vi.fn(async () => ({ ok: true })),
    leaveVendorAccountResourceGroup: vi.fn(async () => ({ ok: true })),
    ...overrides,
  };
}

for (const locale of ['de', 'en'] as readonly Locale[]) {
  const t = messages[locale];

  function renderSection(api: ReturnType<typeof makeApi>) {
    return render(<VendorAccountResourceGroupsSection t={t} api={api} account={account} />);
  }

  describe(`VendorAccountResourceGroupsSection [${locale}]`, () => {
    it('lists the eligible resource groups with their member state under its own heading', async () => {
      const api = makeApi({
        vendorAccountResourceGroups: vi.fn(
          async () =>
            [
              { id: 'rg-1', name: 'Alpha', member: true },
              { id: 'rg-2', name: 'Beta', member: false },
            ] as VendorAccountResourceGroup[],
        ),
      });
      renderSection(api);
      await waitFor(() => expect(api.vendorAccountResourceGroups).toHaveBeenCalledWith('va-x'));
      expect(await screen.findByText('Alpha')).toBeTruthy();
      expect(screen.getByText('Beta')).toBeTruthy();
      expect(screen.getByRole('region', { name: t.vendorAccountResourceGroupsTitle })).toBeTruthy();
      expect(screen.getByText(t.vendorAccountResourceGroupsColGroup)).toBeTruthy();
      expect(screen.getByText(t.vendorAccountResourceGroupsColMember)).toBeTruthy();
      expect((screen.getByRole('switch', { name: 'Alpha' }) as HTMLInputElement).checked).toBe(
        true,
      );
      expect((screen.getByRole('switch', { name: 'Beta' }) as HTMLInputElement).checked).toBe(
        false,
      );
    });

    it('joins a group when toggling a non-member on, then refetches', async () => {
      const list = vi
        .fn()
        .mockResolvedValueOnce([
          { id: 'rg-2', name: 'Beta', member: false },
        ] as VendorAccountResourceGroup[])
        .mockResolvedValueOnce([
          { id: 'rg-2', name: 'Beta', member: true },
        ] as VendorAccountResourceGroup[]);
      const api = makeApi({ vendorAccountResourceGroups: list });
      renderSection(api);
      fireEvent.click(await screen.findByRole('switch', { name: 'Beta' }));
      await waitFor(() =>
        expect(api.joinVendorAccountResourceGroup).toHaveBeenCalledWith('va-x', 'rg-2'),
      );
      expect(api.leaveVendorAccountResourceGroup).not.toHaveBeenCalled();
      await waitFor(() =>
        expect((screen.getByRole('switch', { name: 'Beta' }) as HTMLInputElement).checked).toBe(
          true,
        ),
      );
    });

    it('leaves a group when toggling a member off', async () => {
      const api = makeApi({
        vendorAccountResourceGroups: vi.fn(
          async () => [{ id: 'rg-1', name: 'Alpha', member: true }] as VendorAccountResourceGroup[],
        ),
      });
      renderSection(api);
      fireEvent.click(await screen.findByRole('switch', { name: 'Alpha' }));
      await waitFor(() =>
        expect(api.leaveVendorAccountResourceGroup).toHaveBeenCalledWith('va-x', 'rg-1'),
      );
      expect(api.joinVendorAccountResourceGroup).not.toHaveBeenCalled();
    });

    it('shows the join error and keeps the group listed when joining fails', async () => {
      const api = makeApi({
        vendorAccountResourceGroups: vi.fn(
          async () => [{ id: 'rg-2', name: 'Beta', member: false }] as VendorAccountResourceGroup[],
        ),
        joinVendorAccountResourceGroup: vi.fn(async () => {
          throw new Error('boom');
        }),
      });
      renderSection(api);
      fireEvent.click(await screen.findByRole('switch', { name: 'Beta' }));
      expect(await screen.findByText(t.vendorAccountResourceGroupsJoinError)).toBeTruthy();
      expect((screen.getByRole('switch', { name: 'Beta' }) as HTMLInputElement).checked).toBe(
        false,
      );
    });

    it('shows the leave error and keeps the group listed when leaving fails', async () => {
      const api = makeApi({
        vendorAccountResourceGroups: vi.fn(
          async () => [{ id: 'rg-1', name: 'Alpha', member: true }] as VendorAccountResourceGroup[],
        ),
        leaveVendorAccountResourceGroup: vi.fn(async () => {
          throw new Error('boom');
        }),
      });
      renderSection(api);
      fireEvent.click(await screen.findByRole('switch', { name: 'Alpha' }));
      expect(await screen.findByText(t.vendorAccountResourceGroupsLeaveError)).toBeTruthy();
      expect((screen.getByRole('switch', { name: 'Alpha' }) as HTMLInputElement).checked).toBe(
        true,
      );
    });

    it('shows the empty state when there are no eligible groups', async () => {
      const api = makeApi({
        vendorAccountResourceGroups: vi.fn(async () => [] as VendorAccountResourceGroup[]),
      });
      renderSection(api);
      expect(await screen.findByText(t.vendorAccountResourceGroupsEmpty)).toBeTruthy();
    });

    it('renders the empty state when the load fails (e.g. a 404 / the sharing switch off)', async () => {
      const api = makeApi({
        vendorAccountResourceGroups: vi.fn(async () => {
          throw new Error('not found');
        }),
      });
      renderSection(api);
      expect(await screen.findByText(t.vendorAccountResourceGroupsEmpty)).toBeTruthy();
    });
  });
}
