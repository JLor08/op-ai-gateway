// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, it, expect, afterEach, vi } from 'vitest';
import { render, screen, cleanup, waitFor, fireEvent } from '@testing-library/react';
import { ServerResourceGroupsSection } from './ServerResourceGroupsSection';
import { messages } from '../i18n';
import type { PortalServer, ServerResourceGroup } from '../api';

const t = messages.de;
afterEach(() => cleanup());

const server = { id: 'srv-x', name: 'GPU-Box' } as unknown as PortalServer;

function makeApi(
  overrides: Partial<{
    serverResourceGroups: (serverId: string) => Promise<ServerResourceGroup[]>;
    joinResourceGroup: (serverId: string, rgId: string) => Promise<{ ok: boolean }>;
    leaveResourceGroup: (serverId: string, rgId: string) => Promise<{ ok: boolean }>;
  }>,
) {
  return {
    serverResourceGroups: vi.fn(async () => [] as ServerResourceGroup[]),
    joinResourceGroup: vi.fn(async () => ({ ok: true })),
    leaveResourceGroup: vi.fn(async () => ({ ok: true })),
    ...overrides,
  };
}

function renderSection(api: ReturnType<typeof makeApi>) {
  return render(<ServerResourceGroupsSection t={t} api={api} server={server} />);
}

describe('ServerResourceGroupsSection', () => {
  it('lists the eligible resource groups with their member state', async () => {
    const api = makeApi({
      serverResourceGroups: vi.fn(
        async () =>
          [
            { id: 'rg-1', name: 'Alpha', member: true },
            { id: 'rg-2', name: 'Beta', member: false },
          ] as ServerResourceGroup[],
      ),
    });
    renderSection(api);
    await waitFor(() => expect(api.serverResourceGroups).toHaveBeenCalledWith('srv-x'));
    expect(await screen.findByText('Alpha')).toBeTruthy();
    expect(screen.getByText('Beta')).toBeTruthy();
    expect((screen.getByRole('switch', { name: 'Alpha' }) as HTMLInputElement).checked).toBe(true);
    expect((screen.getByRole('switch', { name: 'Beta' }) as HTMLInputElement).checked).toBe(false);
  });

  it('joins a group when toggling a non-member on, then refetches', async () => {
    const list = vi
      .fn()
      .mockResolvedValueOnce([{ id: 'rg-2', name: 'Beta', member: false }] as ServerResourceGroup[])
      .mockResolvedValueOnce([{ id: 'rg-2', name: 'Beta', member: true }] as ServerResourceGroup[]);
    const api = makeApi({ serverResourceGroups: list });
    renderSection(api);
    fireEvent.click(await screen.findByRole('switch', { name: 'Beta' }));
    await waitFor(() => expect(api.joinResourceGroup).toHaveBeenCalledWith('srv-x', 'rg-2'));
    expect(api.leaveResourceGroup).not.toHaveBeenCalled();
    await waitFor(() =>
      expect((screen.getByRole('switch', { name: 'Beta' }) as HTMLInputElement).checked).toBe(true),
    );
  });

  it('leaves a group when toggling a member off', async () => {
    const api = makeApi({
      serverResourceGroups: vi.fn(
        async () => [{ id: 'rg-1', name: 'Alpha', member: true }] as ServerResourceGroup[],
      ),
    });
    renderSection(api);
    fireEvent.click(await screen.findByRole('switch', { name: 'Alpha' }));
    await waitFor(() => expect(api.leaveResourceGroup).toHaveBeenCalledWith('srv-x', 'rg-1'));
    expect(api.joinResourceGroup).not.toHaveBeenCalled();
  });

  it('shows the empty state when there are no eligible groups', async () => {
    const api = makeApi({ serverResourceGroups: vi.fn(async () => [] as ServerResourceGroup[]) });
    renderSection(api);
    expect(await screen.findByText(t.serverResourceGroupsEmpty)).toBeTruthy();
  });

  it('renders the empty state when the load fails (e.g. a non-owner 404)', async () => {
    const api = makeApi({
      serverResourceGroups: vi.fn(async () => {
        throw new Error('not found');
      }),
    });
    renderSection(api);
    expect(await screen.findByText(t.serverResourceGroupsEmpty)).toBeTruthy();
  });
});
