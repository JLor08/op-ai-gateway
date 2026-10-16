// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, it, expect, afterEach, vi } from 'vitest';
import { render, screen, cleanup, waitFor, fireEvent } from '@testing-library/react';
import { ServiceResourceGroupsSection } from './ServiceResourceGroupsSection';
import { messages, type Locale } from '../i18n';
import type { PortalService, ServiceResourceGroup } from '../api';

afterEach(() => cleanup());

const service = { id: 'svc-x', name: 'Nightly Batch' } as unknown as PortalService;

function makeApi(
  overrides: Partial<{
    serviceResourceGroups: (serviceId: string) => Promise<ServiceResourceGroup[]>;
    joinServiceResourceGroup: (serviceId: string, rgId: string) => Promise<{ ok: boolean }>;
    leaveServiceResourceGroup: (serviceId: string, rgId: string) => Promise<{ ok: boolean }>;
  }>,
) {
  return {
    serviceResourceGroups: vi.fn(async () => [] as ServiceResourceGroup[]),
    joinServiceResourceGroup: vi.fn(async () => ({ ok: true })),
    leaveServiceResourceGroup: vi.fn(async () => ({ ok: true })),
    ...overrides,
  };
}

for (const locale of ['de', 'en'] as readonly Locale[]) {
  const t = messages[locale];

  function renderSection(api: ReturnType<typeof makeApi>) {
    return render(<ServiceResourceGroupsSection t={t} api={api} service={service} />);
  }

  describe(`ServiceResourceGroupsSection [${locale}]`, () => {
    it('lists the eligible resource groups with their member state', async () => {
      const api = makeApi({
        serviceResourceGroups: vi.fn(
          async () =>
            [
              { id: 'rg-1', name: 'Alpha', member: true },
              { id: 'rg-2', name: 'Beta', member: false },
            ] as ServiceResourceGroup[],
        ),
      });
      renderSection(api);
      await waitFor(() => expect(api.serviceResourceGroups).toHaveBeenCalledWith('svc-x'));
      expect(await screen.findByText('Alpha')).toBeTruthy();
      expect(screen.getByText('Beta')).toBeTruthy();
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
        ] as ServiceResourceGroup[])
        .mockResolvedValueOnce([
          { id: 'rg-2', name: 'Beta', member: true },
        ] as ServiceResourceGroup[]);
      const api = makeApi({ serviceResourceGroups: list });
      renderSection(api);
      fireEvent.click(await screen.findByRole('switch', { name: 'Beta' }));
      await waitFor(() =>
        expect(api.joinServiceResourceGroup).toHaveBeenCalledWith('svc-x', 'rg-2'),
      );
      expect(api.leaveServiceResourceGroup).not.toHaveBeenCalled();
      await waitFor(() =>
        expect((screen.getByRole('switch', { name: 'Beta' }) as HTMLInputElement).checked).toBe(
          true,
        ),
      );
    });

    it('leaves a group when toggling a member off', async () => {
      const api = makeApi({
        serviceResourceGroups: vi.fn(
          async () => [{ id: 'rg-1', name: 'Alpha', member: true }] as ServiceResourceGroup[],
        ),
      });
      renderSection(api);
      fireEvent.click(await screen.findByRole('switch', { name: 'Alpha' }));
      await waitFor(() =>
        expect(api.leaveServiceResourceGroup).toHaveBeenCalledWith('svc-x', 'rg-1'),
      );
      expect(api.joinServiceResourceGroup).not.toHaveBeenCalled();
    });

    it('shows the join error and keeps the group listed when joining fails', async () => {
      const api = makeApi({
        serviceResourceGroups: vi.fn(
          async () => [{ id: 'rg-2', name: 'Beta', member: false }] as ServiceResourceGroup[],
        ),
        joinServiceResourceGroup: vi.fn(async () => {
          throw new Error('boom');
        }),
      });
      renderSection(api);
      fireEvent.click(await screen.findByRole('switch', { name: 'Beta' }));
      expect(await screen.findByText(t.serviceResourceGroupsJoinError)).toBeTruthy();
      expect((screen.getByRole('switch', { name: 'Beta' }) as HTMLInputElement).checked).toBe(
        false,
      );
    });

    it('shows the leave error and keeps the group listed when leaving fails', async () => {
      const api = makeApi({
        serviceResourceGroups: vi.fn(
          async () => [{ id: 'rg-1', name: 'Alpha', member: true }] as ServiceResourceGroup[],
        ),
        leaveServiceResourceGroup: vi.fn(async () => {
          throw new Error('boom');
        }),
      });
      renderSection(api);
      fireEvent.click(await screen.findByRole('switch', { name: 'Alpha' }));
      expect(await screen.findByText(t.serviceResourceGroupsLeaveError)).toBeTruthy();
      expect((screen.getByRole('switch', { name: 'Alpha' }) as HTMLInputElement).checked).toBe(
        true,
      );
    });

    it('shows the empty state when there are no eligible groups', async () => {
      const api = makeApi({
        serviceResourceGroups: vi.fn(async () => [] as ServiceResourceGroup[]),
      });
      renderSection(api);
      expect(await screen.findByText(t.serviceResourceGroupsEmpty)).toBeTruthy();
    });

    it('renders the empty state when the load fails (e.g. a non-manager 404)', async () => {
      const api = makeApi({
        serviceResourceGroups: vi.fn(async () => {
          throw new Error('not found');
        }),
      });
      renderSection(api);
      expect(await screen.findByText(t.serviceResourceGroupsEmpty)).toBeTruthy();
    });
  });
}
