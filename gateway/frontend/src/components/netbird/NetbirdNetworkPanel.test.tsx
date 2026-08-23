// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { NetbirdNetworkPanel } from './NetbirdNetworkPanel';
import { ToastProvider } from '../shared/ToastProvider';
import { messages } from '../../i18n';
import type { NetbirdNetwork } from '../../api';
import type { PortalApi } from '../shared/types';

const t = messages.de;

function makeNetwork(overrides: Partial<NetbirdNetwork> = {}): NetbirdNetwork {
  return {
    dns_domain: 'nb.io',
    network_range: '100.64.0.0/10',
    network_range_v6: '',
    ipv6_enabled_groups: [],
    ...overrides,
  };
}

type NetbirdNetworkPanelApi = Pick<
  PortalApi,
  'netbirdGroups' | 'netbirdNetwork' | 'updateNetbirdNetwork'
>;

function renderPanel(disabled: boolean, overrides: Partial<NetbirdNetworkPanelApi> = {}) {
  const netbirdNetwork = vi.fn(async () => makeNetwork());
  const updateNetbirdNetwork = vi.fn(async (body: NetbirdNetwork) => body);
  const netbirdGroups = vi.fn(async () => ({ data: [] }));
  const fakeApi: NetbirdNetworkPanelApi = {
    netbirdNetwork,
    updateNetbirdNetwork,
    netbirdGroups,
    ...overrides,
  };

  render(
    <ToastProvider>
      <NetbirdNetworkPanel t={t} api={fakeApi} disabled={disabled} />
    </ToastProvider>,
  );

  return { fakeApi, netbirdNetwork, updateNetbirdNetwork, netbirdGroups };
}

afterEach(cleanup);

describe('NetbirdNetworkPanel', () => {
  it('loads and renders the network settings on mount when enabled', async () => {
    const { netbirdNetwork } = renderPanel(false);

    await waitFor(() => expect(netbirdNetwork).toHaveBeenCalled());
    const field = (await screen.findByLabelText(t.settingsNetbirdDnsDomain)) as HTMLInputElement;
    expect(field.value).toBe('nb.io');
  });

  it('does not fetch and disables Save when disabled', async () => {
    const { netbirdNetwork } = renderPanel(true);

    // Give any accidental effect a tick to fire before asserting it never did.
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(netbirdNetwork).not.toHaveBeenCalled();

    const save = screen.getByRole('button', { name: t.save });
    expect(save).toBeDisabled();
  });

  it('opens the confirm dialog on Save and calls updateNetbirdNetwork with the edited body on confirm', async () => {
    const { updateNetbirdNetwork } = renderPanel(false);

    const field = (await screen.findByLabelText(t.settingsNetbirdDnsDomain)) as HTMLInputElement;
    await waitFor(() => expect(field.value).toBe('nb.io'));
    fireEvent.change(field, { target: { value: 'nb2.io' } });

    const save = screen.getByRole('button', { name: t.save });
    fireEvent.click(save);

    await screen.findByText(t.settingsNetbirdNetworkSaveConfirmTitle);
    await screen.findByText(t.settingsNetbirdNetworkSaveConfirmBody);

    const confirm = screen.getByRole('button', { name: t.settingsNetbirdNetworkSaveConfirmAction });
    fireEvent.click(confirm);

    await waitFor(() =>
      expect(updateNetbirdNetwork).toHaveBeenCalledWith(
        expect.objectContaining({ dns_domain: 'nb2.io' }),
      ),
    );
  });
});
