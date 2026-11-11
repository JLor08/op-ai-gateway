// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { BinariesSection } from './BinariesSection';
import { ToastProvider } from './shared/ToastProvider';
import { PortalApiError } from '../api/transport';
import { messages } from '../i18n';
import type { ServerBinary } from '../api';

const t = messages.de;

function bin(over: Partial<ServerBinary> = {}): ServerBinary {
  return {
    path: '/opt/llama/llama-server',
    kind: 'llama_server',
    version: 'b1234',
    version_override: '',
    effective_version: 'b1234',
    build: '1234',
    commit: 'abc',
    version_source: 'agent',
    source: 'agent',
    allowed: true,
    reported_at: '2026-11-05T10:00:00Z',
    ...over,
  };
}

const manual = bin({
  path: '/srv/custom/rpc-server',
  kind: 'rpc_server',
  version: '',
  effective_version: '',
  build: '',
  commit: '',
  version_source: '',
  source: 'manual',
});

function setup(rows: ServerBinary[], over: Record<string, unknown> = {}) {
  const api = {
    serverBinaries: vi.fn().mockResolvedValue({ data: rows }),
    createServerBinary: vi.fn().mockResolvedValue(manual),
    updateServerBinary: vi.fn().mockResolvedValue(manual),
    deleteServerBinary: vi.fn().mockResolvedValue(undefined),
    ...over,
  };
  render(
    <ToastProvider>
      <BinariesSection t={t} api={api} serverId="srv_1" />
    </ToastProvider>,
  );
  return api;
}

afterEach(cleanup);

describe('BinariesSection', () => {
  it('lists agent and manual rows with kind and effective version', async () => {
    setup([bin(), manual]);
    const row = (await screen.findByText('/opt/llama/llama-server')).closest('tr') as HTMLElement;
    expect(within(row).getByText('b1234')).toBeTruthy();
    expect(within(row).getByText(t.binariesSourceAgent)).toBeTruthy();
    const mrow = screen.getByText('/srv/custom/rpc-server').closest('tr') as HTMLElement;
    expect(within(mrow).getByText(t.binariesSourceManual)).toBeTruthy();
    expect(within(mrow).getByText(t.binariesVersionUnknown)).toBeTruthy();
  });

  it('marks a path the agent does not allow', async () => {
    setup([bin({ allowed: false })]);
    expect(await screen.findByText(t.binariesNotAllowed)).toBeTruthy();
  });

  it('shows an override as the effective version', async () => {
    setup([bin({ version_override: 'b9999', effective_version: 'b9999' })]);
    expect(await screen.findByText('b9999')).toBeTruthy();
  });

  it('keeps an agent row read-only except for the version override', async () => {
    const api = setup([bin()]);
    await screen.findByText('/opt/llama/llama-server');
    expect(screen.queryByLabelText(`${t.binariesDelete}: /opt/llama/llama-server`)).toBeNull();
    expect(screen.queryByLabelText(`${t.binariesEdit}: /opt/llama/llama-server`)).toBeNull();
    fireEvent.click(screen.getByLabelText(`${t.binariesOverrideAction}: /opt/llama/llama-server`));
    expect(screen.queryByLabelText(t.binariesKind)).toBeNull();
    fireEvent.change(screen.getByLabelText(t.binariesOverride), { target: { value: 'b9999' } });
    fireEvent.click(screen.getByRole('button', { name: t.save }));
    await waitFor(() =>
      expect(api.updateServerBinary).toHaveBeenCalledWith('srv_1', {
        path: '/opt/llama/llama-server',
        version_override: 'b9999',
      }),
    );
  });

  it('adds a manual row', async () => {
    const api = setup([]);
    fireEvent.click(await screen.findByRole('button', { name: t.binariesAdd }));
    fireEvent.change(screen.getByLabelText(t.binariesPath), {
      target: { value: '/srv/custom/rpc-server' },
    });
    fireEvent.mouseDown(screen.getByRole('combobox', { name: t.binariesKind }));
    fireEvent.click(await screen.findByRole('option', { name: t.binariesKindRpcServer }));
    fireEvent.change(screen.getByLabelText(t.binariesVersion), { target: { value: 'b1234' } });
    fireEvent.click(screen.getByRole('button', { name: t.save }));
    await waitFor(() =>
      expect(api.createServerBinary).toHaveBeenCalledWith('srv_1', {
        path: '/srv/custom/rpc-server',
        kind: 'rpc_server',
        version: 'b1234',
        build: '',
        commit: '',
      }),
    );
    await waitFor(() => expect(api.serverBinaries).toHaveBeenCalledTimes(2));
  });

  it('edits a manual row with the path fixed', async () => {
    const api = setup([manual]);
    fireEvent.click(await screen.findByLabelText(`${t.binariesEdit}: /srv/custom/rpc-server`));
    expect((screen.getByLabelText(t.binariesPath) as HTMLInputElement).readOnly).toBe(true);
    fireEvent.change(screen.getByLabelText(t.binariesVersion), { target: { value: 'b77' } });
    fireEvent.click(screen.getByRole('button', { name: t.save }));
    await waitFor(() =>
      expect(api.updateServerBinary).toHaveBeenCalledWith('srv_1', {
        path: '/srv/custom/rpc-server',
        kind: 'rpc_server',
        version: 'b77',
        build: '',
        commit: '',
      }),
    );
  });

  it('deletes a manual row after confirming', async () => {
    const api = setup([manual]);
    fireEvent.click(await screen.findByLabelText(`${t.binariesDelete}: /srv/custom/rpc-server`));
    fireEvent.click(await screen.findByRole('button', { name: t.binariesDeleteConfirm }));
    await waitFor(() =>
      expect(api.deleteServerBinary).toHaveBeenCalledWith('srv_1', '/srv/custom/rpc-server'),
    );
  });

  it('closes the editor when the edited row is gone', async () => {
    const api = setup([manual]);
    fireEvent.click(await screen.findByLabelText(`${t.binariesEdit}: /srv/custom/rpc-server`));
    expect(screen.getByLabelText(t.binariesVersion)).toBeTruthy();
    api.serverBinaries.mockResolvedValue({ data: [] });
    fireEvent.click(screen.getByLabelText(`${t.binariesDelete}: /srv/custom/rpc-server`));
    fireEvent.click(await screen.findByRole('button', { name: t.binariesDeleteConfirm }));
    await waitFor(() => expect(api.serverBinaries).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(screen.queryByLabelText(t.binariesVersion)).toBeNull());
  });

  it('keeps a new row editor open across a reload', async () => {
    const api = setup([manual]);
    fireEvent.click(await screen.findByRole('button', { name: t.binariesAdd }));
    api.serverBinaries.mockResolvedValue({ data: [] });
    fireEvent.click(screen.getByLabelText(`${t.binariesDelete}: /srv/custom/rpc-server`));
    fireEvent.click(await screen.findByRole('button', { name: t.binariesDeleteConfirm }));
    await waitFor(() =>
      expect(screen.queryByLabelText(`${t.binariesEdit}: /srv/custom/rpc-server`)).toBeNull(),
    );
    expect(screen.getByLabelText(t.binariesPath)).toBeTruthy();
  });

  it('shows the localized error when a save is refused', async () => {
    setup([], {
      createServerBinary: vi
        .fn()
        .mockRejectedValue(new PortalApiError(400, 'binary.path_invalid', 'raw')),
    });
    fireEvent.click(await screen.findByRole('button', { name: t.binariesAdd }));
    fireEvent.change(screen.getByLabelText(t.binariesPath), { target: { value: 'relative' } });
    fireEvent.click(screen.getByRole('button', { name: t.save }));
    expect(await screen.findByText(new RegExp(t.errorBinaryPathInvalid.slice(0, 20)))).toBeTruthy();
  });

  it('shows the load failure instead of an empty table', async () => {
    setup([], { serverBinaries: vi.fn().mockRejectedValue(new Error('boom')) });
    expect(await screen.findByRole('alert')).toBeTruthy();
  });
});
