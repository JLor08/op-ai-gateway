// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { ToolsView } from './ToolsView';
import { messages } from '../i18n';
import type { PortalApi } from './shared/types';

const t = messages.de;

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

type ToolsViewApi = Pick<PortalApi, 'pingServer' | 'servers'>;

function makeApi(overrides: Partial<ToolsViewApi> = {}): ToolsViewApi {
  return {
    servers: vi.fn(async () => ({
      data: [
        { id: 'srv_1', name: 'S1' },
        { id: 'srv_2', name: 'S2' },
      ],
    })),
    pingServer: vi.fn(async () => ({ ok: true, latency_ms: 5 })),
    ...overrides,
  } as unknown as ToolsViewApi;
}

function renderToolsView(api: ToolsViewApi) {
  return render(<ToolsView t={t} api={api} />);
}

// Drives the non-native MUI Select (SelectField): open the popup, click the option.
async function pickServer(label: string) {
  fireEvent.mouseDown(await screen.findByLabelText(t.settingsPingServerLabel));
  fireEvent.click(await screen.findByRole('option', { name: label }));
}

describe('ToolsView', () => {
  it('renders the page title and the ping tool', async () => {
    renderToolsView(makeApi());
    expect(await screen.findByText(t.tools)).toBeInTheDocument();
    expect(screen.getByText(t.toolsIntro)).toBeInTheDocument();
    expect(screen.getByText(t.settingsPingTitle)).toBeInTheDocument();
  });

  it('pings the selected server and renders a success alert with the latency', async () => {
    const pingServer = vi.fn(async () => ({ ok: true, latency_ms: 5 }));
    const api = makeApi({ pingServer });
    renderToolsView(api);

    await pickServer('S1');

    const button = screen.getByRole('button', { name: t.settingsPingButton });
    await waitFor(() => expect(button).toBeEnabled());
    fireEvent.click(button);

    await waitFor(() => expect(pingServer).toHaveBeenCalledWith('srv_1'));
    expect(await screen.findByText(t.settingsPingOk(5))).toBeInTheDocument();
  });

  it('renders an error alert when the ping fails', async () => {
    const pingServer = vi.fn(async () => ({ ok: false, error: 'no reply' }));
    const api = makeApi({ pingServer });
    renderToolsView(api);

    await pickServer('S2');

    const button = screen.getByRole('button', { name: t.settingsPingButton });
    await waitFor(() => expect(button).toBeEnabled());
    fireEvent.click(button);

    await waitFor(() => expect(pingServer).toHaveBeenCalledWith('srv_2'));
    expect(await screen.findByText(t.settingsPingFailed('no reply'))).toBeInTheDocument();
  });

  it('keeps the button disabled until a server is picked, and while a load error leaves the picker empty', async () => {
    const api = makeApi({
      servers: vi.fn(async () => {
        throw new Error('boom');
      }),
    });
    renderToolsView(api);

    // Load-error path: the picker stays empty (never blocks the page).
    expect(await screen.findByText(t.settingsPingTitle)).toBeInTheDocument();
    const button = screen.getByRole('button', { name: t.settingsPingButton });
    expect(button).toBeDisabled();
  });
});
