// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { useState } from 'react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { RpcMemberSection } from './RpcMemberSection';
import { ToastProvider } from './shared/ToastProvider';
import { PortalApiError } from '../api/transport';
import { messages } from '../i18n';
import type { PortalServer, RPCMemberSpec, RuntimeStatus } from '../api';

const t = messages.de;

const server = {
  id: 'srv_1',
  name: 'S1',
  admin_groups: [
    { id: 'g1', name: 'Ops' },
    { id: 'g2', name: 'Research' },
  ],
  rpc_member_enabled: false,
  rpc_allowed_head_groups: [],
} as unknown as PortalServer;

function spec(over: Partial<RPCMemberSpec> = {}): RPCMemberSpec {
  return {
    id: 'rpcm_1',
    server_id: 'srv_1',
    enabled: true,
    binary: '/opt/llama/rpc-server',
    args: ['-t', '8'],
    env: { FOO: 'bar' },
    gpus: [{ index: 0, vram_estimate_mb: 8000, vram_measured_mb: 0 }],
    cache_enabled: false,
    device_backend: 'auto',
    ram_estimate_mb: 0,
    allowed_head_groups: [],
    ...over,
  };
}

function memberRow(over: Partial<RuntimeStatus> = {}): RuntimeStatus {
  return {
    spec_id: 'rpcm_1',
    instance: 0,
    model: '',
    state: 'running',
    since: '2026-11-05T10:00:00Z',
    in_flight: 0,
    restarts: 0,
    context_size: 0,
    active_requests: 0,
    queue_depth: 0,
    metrics_probe: '',
    context_probe: '',
    kind: 'rpc_server',
    ...over,
  };
}

function setup(
  opts: {
    specs?: RPCMemberSpec[];
    rows?: RuntimeStatus[];
    deleteError?: unknown;
  } = {},
) {
  const api = {
    rpcMemberSpecs: vi.fn().mockResolvedValue({ data: opts.specs ?? [] }),
    createRPCMemberSpec: vi.fn().mockResolvedValue(spec()),
    updateRPCMemberSpec: vi.fn().mockResolvedValue(spec()),
    deleteRPCMemberSpec: vi
      .fn()
      .mockImplementation(() =>
        opts.deleteError ? Promise.reject(opts.deleteError) : Promise.resolve(undefined),
      ),
    updateServer: vi.fn().mockResolvedValue(server),
    subscribeRuntimeStatus: vi.fn((_id: string, onData: (rows: RuntimeStatus[]) => void) => {
      onData(opts.rows ?? []);
      return () => {};
    }),
  };
  const view = render(
    <ToastProvider>
      <RpcMemberSection t={t} api={api} server={server} />
    </ToastProvider>,
  );
  return Object.assign(api, {
    rerenderServer: (next: PortalServer) =>
      view.rerender(
        <ToastProvider>
          <RpcMemberSection t={t} api={api} server={next} />
        </ToastProvider>,
      ),
  });
}

afterEach(cleanup);

describe('RpcMemberSection server settings', () => {
  it('saves the member offer and the allowed head groups', async () => {
    const api = setup();
    fireEvent.click(await screen.findByLabelText(t.rpcMemberEnabledLabel));
    fireEvent.click(screen.getByLabelText('Ops'));
    fireEvent.click(screen.getByRole('button', { name: t.rpcSettingsSave }));
    await waitFor(() =>
      expect(api.updateServer).toHaveBeenCalledWith('srv_1', {
        rpc_member_enabled: true,
        rpc_allowed_head_groups: ['g1'],
      }),
    );
  });

  it('explains that an empty group list means the server own admin groups', async () => {
    setup();
    expect(await screen.findByText(t.rpcAllowedHeadGroupsDefault)).toBeTruthy();
  });
});

describe('RpcMemberSection manual member specs', () => {
  it('round-trips every field of a new spec', async () => {
    const api = setup();
    fireEvent.click(await screen.findByRole('button', { name: t.rpcMemberAdd }));
    fireEvent.change(screen.getByLabelText(t.rpcMemberBinary), {
      target: { value: '/opt/llama/rpc-server' },
    });
    fireEvent.change(screen.getByLabelText(t.runtimeSpecArgs), {
      target: { value: '-t\n8' },
    });
    fireEvent.change(screen.getByLabelText(t.runtimeSpecEnv), {
      target: { value: 'FOO=bar' },
    });
    fireEvent.click(screen.getByRole('button', { name: t.runtimeSpecGpuAdd }));
    fireEvent.change(screen.getByLabelText(`${t.rpcMemberGpuLabel} 1: ${t.runtimeSpecGpuIndex}`), {
      target: { value: '1' },
    });
    fireEvent.change(screen.getByLabelText(`${t.rpcMemberGpuLabel} 1: ${t.rpcMemberGpuVram}`), {
      target: { value: '8000' },
    });
    fireEvent.click(screen.getByLabelText(t.rpcMemberCache));
    fireEvent.change(screen.getByLabelText(t.runtimeSpecRamEstimate), {
      target: { value: '4096' },
    });
    fireEvent.click(within(screen.getByRole('form')).getByLabelText('Research'));
    fireEvent.click(screen.getByRole('button', { name: t.save }));
    await waitFor(() => expect(api.createRPCMemberSpec).toHaveBeenCalledTimes(1));
    expect(api.createRPCMemberSpec).toHaveBeenCalledWith(
      'srv_1',
      expect.objectContaining({
        binary: '/opt/llama/rpc-server',
        args: ['-t', '8'],
        env: { FOO: 'bar' },
        gpus: [{ index: 1, vram_estimate_mb: 8000, vram_measured_mb: 0 }],
        cache_enabled: true,
        device_backend: 'auto',
        ram_estimate_mb: 4096,
        allowed_head_groups: ['g2'],
      }),
    );
  });

  it('keeps GPU row order in the written document', async () => {
    const api = setup({
      specs: [
        spec({
          gpus: [
            { index: 0, vram_estimate_mb: 1, vram_measured_mb: 0 },
            { index: 1, vram_estimate_mb: 2, vram_measured_mb: 0 },
          ],
        }),
      ],
    });
    fireEvent.click(
      await screen.findByRole('button', { name: `${t.runtimeSpecEditAction}: rpcm_1` }),
    );
    fireEvent.click(
      screen.getByRole('button', { name: `${t.modelGroupMoveDown}: ${t.rpcMemberGpuLabel} 1` }),
    );
    fireEvent.click(screen.getByRole('button', { name: t.save }));
    await waitFor(() => expect(api.updateRPCMemberSpec).toHaveBeenCalled());
    const body = api.updateRPCMemberSpec.mock.calls[0][2];
    expect(body.gpus.map((g: { index: number }) => g.index)).toEqual([1, 0]);
  });

  it.each([
    '-H',
    '--host=0.0.0.0',
    '-p',
    '--port',
    '-d',
    '--device=CUDA0',
    '-c',
    '--cache',
    '-p8080',
  ])('refuses the reserved flag %s without calling the API', async (flag) => {
    const api = setup();
    fireEvent.click(await screen.findByRole('button', { name: t.rpcMemberAdd }));
    fireEvent.change(screen.getByLabelText(t.rpcMemberBinary), { target: { value: '/x' } });
    fireEvent.change(screen.getByLabelText(t.runtimeSpecArgs), { target: { value: flag } });
    fireEvent.click(screen.getByRole('button', { name: t.save }));
    expect(await screen.findByText(t.errorRuntimeSpecMemberFlagReserved)).toBeTruthy();
    expect(api.createRPCMemberSpec).not.toHaveBeenCalled();
  });

  it('accepts ordinary operator args', async () => {
    const api = setup();
    fireEvent.click(await screen.findByRole('button', { name: t.rpcMemberAdd }));
    fireEvent.change(screen.getByLabelText(t.rpcMemberBinary), { target: { value: '/x' } });
    fireEvent.change(screen.getByLabelText(t.runtimeSpecArgs), { target: { value: '-t\n8' } });
    fireEvent.click(screen.getByRole('button', { name: t.save }));
    await waitFor(() => expect(api.createRPCMemberSpec).toHaveBeenCalled());
  });

  it('refuses a relative binary path', async () => {
    const api = setup();
    fireEvent.click(await screen.findByRole('button', { name: t.rpcMemberAdd }));
    fireEvent.change(screen.getByLabelText(t.rpcMemberBinary), { target: { value: 'rpc-server' } });
    fireEvent.click(screen.getByRole('button', { name: t.save }));
    expect(await screen.findByText(t.errorBinaryPathInvalid)).toBeTruthy();
    expect(api.createRPCMemberSpec).not.toHaveBeenCalled();
  });

  it('labels a spec without GPU rows as a CPU member', async () => {
    setup({ specs: [spec({ id: 'rpcm_cpu', gpus: [] }), spec({ id: 'rpcm_gpu' })] });
    const cpuRow = (await screen.findByText('rpcm_cpu')).closest('[data-spec-row]') as HTMLElement;
    const gpuRow = screen.getByText('rpcm_gpu').closest('[data-spec-row]') as HTMLElement;
    expect(within(cpuRow).getByText(t.rpcMemberCpu)).toBeTruthy();
    expect(within(gpuRow).queryByText(t.rpcMemberCpu)).toBeNull();
  });

  it('maps a refused delete of a referenced member to its error label', async () => {
    const api = setup({
      specs: [spec()],
      deleteError: new PortalApiError(409, 'runtime.cluster_member_in_use', 'in use'),
    });
    fireEvent.click(await screen.findByRole('button', { name: `${t.runtimeSpecDelete}: rpcm_1` }));
    fireEvent.click(
      within(screen.getByRole('dialog')).getByRole('button', { name: t.runtimeSpecDelete }),
    );
    await waitFor(() => expect(api.deleteRPCMemberSpec).toHaveBeenCalledWith('srv_1', 'rpcm_1'));
    expect(await screen.findByText(new RegExp(t.errorClusterMemberInUse))).toBeTruthy();
  });
});

describe('RpcMemberSection member status', () => {
  it('shows the holding head only when the spec DTO exposes it', async () => {
    setup({
      specs: [
        spec({ id: 'rpcm_1', used_by_head_spec_id: 'rspec_visible' }),
        spec({ id: 'rpcm_2' }),
      ],
      rows: [
        memberRow({ spec_id: 'rpcm_1', rpc: { held_by: 'rspec_visible' } }),
        memberRow({ spec_id: 'rpcm_2', rpc: { held_by: 'rspec_hidden' } }),
      ],
    });
    const visible = (await screen.findByTestId('rpc-status-rpcm_1')) as HTMLElement;
    const hidden = screen.getByTestId('rpc-status-rpcm_2');
    expect(within(visible).getByText(/rspec_visible/)).toBeTruthy();
    expect(within(hidden).queryByText(/rspec_hidden/)).toBeNull();
    expect(within(hidden).getByText(t.rpcStatusHeldElsewhere)).toBeTruthy();
  });

  it('shows effective VRAM as max(measured, estimate), protocol and badges', async () => {
    setup({
      specs: [spec({ gpus: [{ index: 0, vram_estimate_mb: 8000, vram_measured_mb: 0 }] })],
      rows: [
        memberRow({
          gpus: [{ index: 0, vram_measured_mb: 9000 }],
          rpc: {
            protocol: '8.0.1',
            bind_mismatch: true,
            breaches: [{ peer: '100.64.0.9', at: '2026-11-05T10:00:00Z' }],
          },
        }),
      ],
    });
    const row = await screen.findByTestId('rpc-status-rpcm_1');
    expect(within(row).getByText(/9000/)).toBeTruthy();
    expect(within(row).getByText(/8\.0\.1/)).toBeTruthy();
    expect(within(row).getByText(t.rpcStatusBindMismatch)).toBeTruthy();
    expect(within(row).getByText(new RegExp(t.rpcStatusBreaches))).toBeTruthy();
  });

  it('ignores model rows', async () => {
    setup({ rows: [memberRow({ spec_id: 'model_x', kind: undefined })] });
    expect(await screen.findByText(t.rpcStatusEmpty)).toBeTruthy();
  });
});

describe('RpcMemberSection settings state', () => {
  it('keeps unsaved edits when a new server object with equal content arrives', async () => {
    const api = setup();
    fireEvent.click(await screen.findByLabelText('Ops'));
    api.rerenderServer({ ...server, rpc_allowed_head_groups: [] });
    expect(screen.getByLabelText('Ops')).toBeChecked();
  });

  it('only sends the fields that differ from the seed', async () => {
    const api = setup();
    fireEvent.click(await screen.findByLabelText(t.rpcMemberEnabledLabel));
    fireEvent.click(screen.getByRole('button', { name: t.rpcSettingsSave }));
    await waitFor(() => expect(api.updateServer).toHaveBeenCalled());
    expect(api.updateServer.mock.calls[0][1]).toEqual({ rpc_member_enabled: true });
  });

  it('shows the saved value after a remount when the parent feeds the result back', async () => {
    const api = setup();
    api.updateServer.mockResolvedValue({
      ...server,
      rpc_member_enabled: true,
      rpc_allowed_head_groups: ['g1'],
    });
    function Harness() {
      const [saved, setSaved] = useState<PortalServer | null>(null);
      const [shown, setShown] = useState(true);
      return (
        <ToastProvider>
          <button onClick={() => setShown((v) => !v)}>toggle</button>
          {shown && (
            <RpcMemberSection t={t} api={api} server={saved ?? server} onServerSaved={setSaved} />
          )}
        </ToastProvider>
      );
    }
    cleanup();
    render(<Harness />);
    fireEvent.click(await screen.findByLabelText('Ops'));
    fireEvent.click(screen.getByRole('button', { name: t.rpcSettingsSave }));
    await waitFor(() => expect(api.updateServer).toHaveBeenCalled());
    fireEvent.click(screen.getByText('toggle'));
    fireEvent.click(screen.getByText('toggle'));
    expect(await screen.findByLabelText('Ops')).toBeChecked();
    expect(screen.getByLabelText(t.rpcMemberEnabledLabel)).toBeChecked();
  });

  it('drops the stored measured VRAM when a GPU index is edited', async () => {
    const api = setup({
      specs: [spec({ gpus: [{ index: 0, vram_estimate_mb: 100, vram_measured_mb: 777 }] })],
    });
    fireEvent.click(
      await screen.findByRole('button', { name: `${t.runtimeSpecEditAction}: rpcm_1` }),
    );
    fireEvent.change(screen.getByLabelText(`${t.rpcMemberGpuLabel} 1: ${t.runtimeSpecGpuIndex}`), {
      target: { value: '2' },
    });
    fireEvent.click(screen.getByRole('button', { name: t.save }));
    await waitFor(() => expect(api.updateRPCMemberSpec).toHaveBeenCalled());
    expect(api.updateRPCMemberSpec.mock.calls[0][2].gpus[0].vram_measured_mb).toBe(0);
  });
});
