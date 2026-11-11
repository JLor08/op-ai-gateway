// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { useState } from 'react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { RpcMemberSection } from './RpcMemberSection';
import { ToastProvider } from './shared/ToastProvider';
import { PortalApiError } from '../api/transport';
import { messages } from '../i18n';
import type {
  AdminGroupCandidate,
  PortalServer,
  RPCMemberSpec,
  RuntimeStatus,
  ServerBinary,
} from '../api';

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
    server?: PortalServer;
    binaries?: ServerBinary[];
    groupCandidates?: AdminGroupCandidate[];
    groupCandidatesError?: unknown;
  } = {},
) {
  const api = {
    serverBinaries: vi.fn().mockResolvedValue({ data: opts.binaries ?? [] }),
    rpcMemberSpecs: vi.fn().mockResolvedValue({ data: opts.specs ?? [] }),
    serverAdminGroupCandidates: vi
      .fn()
      .mockImplementation(() =>
        opts.groupCandidatesError
          ? Promise.reject(opts.groupCandidatesError)
          : Promise.resolve(opts.groupCandidates ?? []),
      ),
    createRPCMemberSpec: vi.fn().mockResolvedValue(spec()),
    updateRPCMemberSpec: vi.fn().mockResolvedValue(spec()),
    deleteRPCMemberSpec: vi
      .fn()
      .mockImplementation(() =>
        opts.deleteError ? Promise.reject(opts.deleteError) : Promise.resolve(undefined),
      ),
    updateServer: vi.fn().mockResolvedValue(server),
  };
  const rows = opts.rows ?? [];
  const view = render(
    <ToastProvider>
      <RpcMemberSection t={t} api={api} server={opts.server ?? server} statusRows={rows} />
    </ToastProvider>,
  );
  return Object.assign(api, {
    rerenderServer: (next: PortalServer) =>
      view.rerender(
        <ToastProvider>
          <RpcMemberSection t={t} api={api} server={next} statusRows={rows} />
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
    fireEvent.change(screen.getByLabelText(`${t.rpcMemberGpuRow(1)}: ${t.runtimeSpecGpuIndex}`), {
      target: { value: '1' },
    });
    fireEvent.change(screen.getByLabelText(`${t.rpcMemberGpuRow(1)}: ${t.rpcMemberGpuVram}`), {
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
      screen.getByRole('button', { name: `${t.modelGroupMoveDown}: ${t.rpcMemberGpuRow(1)}` }),
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

describe('RpcMemberSection binary picker', () => {
  const rpcBinary = {
    path: '/opt/llama/rpc-server',
    kind: 'rpc_server',
    version: 'b1',
    version_override: '',
    effective_version: 'b1',
    build: '',
    commit: '',
    version_source: 'agent',
    source: 'agent',
    allowed: false,
    reported_at: '',
  } satisfies ServerBinary;

  it('offers the inventory with the allow mark and fills the binary field', async () => {
    const api = setup({ binaries: [rpcBinary] });
    fireEvent.click(await screen.findByRole('button', { name: t.rpcMemberAdd }));
    const field = await screen.findByLabelText(t.rpcMemberBinary);
    fireEvent.mouseDown(field);
    expect(await screen.findByText(t.binariesNotAllowed)).toBeTruthy();
    fireEvent.click(screen.getByText('/opt/llama/rpc-server'));
    expect((field as HTMLInputElement).value).toBe('/opt/llama/rpc-server');
    expect(api.serverBinaries).toHaveBeenCalledWith('srv_1');
  });

  it('saves a typed path that is not in the inventory', async () => {
    const api = setup({ binaries: [rpcBinary] });
    fireEvent.click(await screen.findByRole('button', { name: t.rpcMemberAdd }));
    fireEvent.change(await screen.findByLabelText(t.rpcMemberBinary), {
      target: { value: '/unknown/rpc-server' },
    });
    fireEvent.click(screen.getByRole('button', { name: t.save }));
    await waitFor(() => expect(api.createRPCMemberSpec).toHaveBeenCalled());
    expect(api.createRPCMemberSpec.mock.calls[0][1].binary).toBe('/unknown/rpc-server');
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

  it('names a member by the gateway display name and keeps its id at hand', async () => {
    setup({
      rows: [
        memberRow({
          spec_id: 'rpca_rspec_1_srv_1',
          display_name: 'RPC (unsloth/Qwen3.8-27B)(efyga)',
        }),
      ],
    });
    const row = await screen.findByTestId('rpc-status-rpca_rspec_1_srv_1');
    const name = within(row).getByText('RPC (unsloth/Qwen3.8-27B)(efyga)');
    expect(name).toHaveAccessibleDescription('rpca_rspec_1_srv_1');
    expect(within(row).queryByText('rpca_rspec_1_srv_1')).toBeNull();
  });

  it('shows the member id when the gateway sends no display name', async () => {
    setup({ rows: [memberRow({ spec_id: 'rpcm_1' })] });
    const row = await screen.findByTestId('rpc-status-rpcm_1');
    expect(within(row).getByText('rpcm_1')).toBeTruthy();
  });

  it('ignores model rows', async () => {
    setup({ rows: [memberRow({ spec_id: 'model_x', kind: undefined })] });
    expect(await screen.findByText(t.rpcStatusEmpty)).toBeTruthy();
  });
});

describe('RpcMemberSection settings state', () => {
  it('keeps an unsaved edit when a new server object with equal non-empty content arrives', async () => {
    const api = setup({ server: { ...server, rpc_allowed_head_groups: ['g1'] } });
    fireEvent.click(await screen.findByLabelText('Research'));
    api.rerenderServer({ ...server, rpc_allowed_head_groups: ['g1'] });
    expect(screen.getByLabelText('Research')).toBeChecked();
    expect(screen.getByLabelText('Ops')).toBeChecked();
  });

  it('shows an external change on a clean form', async () => {
    const api = setup();
    await screen.findByLabelText('Ops');
    api.rerenderServer({ ...server, rpc_member_enabled: true, rpc_allowed_head_groups: ['g2'] });
    await waitFor(() => expect(screen.getByLabelText('Research')).toBeChecked());
    expect(screen.getByLabelText(t.rpcMemberEnabledLabel)).toBeChecked();
  });

  it('keeps the unsaved edit and sends only it when the saved state changes externally', async () => {
    const api = setup();
    fireEvent.click(await screen.findByLabelText('Ops'));
    api.rerenderServer({ ...server, rpc_member_enabled: true });
    expect(screen.getByLabelText('Ops')).toBeChecked();
    fireEvent.click(screen.getByRole('button', { name: t.rpcSettingsSave }));
    await waitFor(() => expect(api.updateServer).toHaveBeenCalled());
    expect(api.updateServer.mock.calls[0][1]).toEqual({ rpc_allowed_head_groups: ['g1'] });
  });

  it('says so when there is nothing to save', async () => {
    const api = setup();
    fireEvent.click(await screen.findByRole('button', { name: t.rpcSettingsSave }));
    expect(await screen.findByText(t.rpcSettingsNoChanges)).toBeTruthy();
    expect(api.updateServer).not.toHaveBeenCalled();
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
            <RpcMemberSection
              t={t}
              api={api}
              server={saved ?? server}
              statusRows={[]}
              onServerSaved={setSaved}
            />
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

  it('keeps the GPU row names while an index is typed', async () => {
    const api = setup({ specs: [spec()] });
    fireEvent.click(
      await screen.findByRole('button', { name: `${t.runtimeSpecEditAction}: rpcm_1` }),
    );
    const index = screen.getByLabelText(`${t.rpcMemberGpuRow(1)}: ${t.runtimeSpecGpuIndex}`);
    fireEvent.change(index, { target: { value: '' } });
    fireEvent.change(index, { target: { value: '2' } });
    expect(screen.getByLabelText(`${t.rpcMemberGpuRow(1)}: ${t.runtimeSpecGpuIndex}`)).toBe(index);
    fireEvent.click(screen.getByRole('button', { name: t.save }));
    await waitFor(() => expect(api.updateRPCMemberSpec).toHaveBeenCalled());
    expect(api.updateRPCMemberSpec.mock.calls[0][2].gpus[0]).toEqual({
      index: 2,
      vram_estimate_mb: 8000,
      vram_measured_mb: 0,
    });
  });
});

describe('RpcMemberSection consent groups', () => {
  const lab: AdminGroupCandidate = {
    id: 'g3',
    name: 'Lab',
    parent_group_id: 'sys',
    parent_group_name: 'System',
  };

  it('offers every admin group the caller can list, beyond the server own', async () => {
    const api = setup({ groupCandidates: [lab] });
    fireEvent.click(await screen.findByLabelText('Lab'));
    fireEvent.click(screen.getByRole('button', { name: t.rpcSettingsSave }));
    await waitFor(() =>
      expect(api.updateServer).toHaveBeenCalledWith('srv_1', { rpc_allowed_head_groups: ['g3'] }),
    );
    expect(screen.getByLabelText('Ops')).toBeTruthy();
  });

  it('offers the listed groups in the member spec editor too', async () => {
    const api = setup({ groupCandidates: [lab] });
    await screen.findByLabelText('Lab');
    fireEvent.click(screen.getByRole('button', { name: t.rpcMemberAdd }));
    fireEvent.change(screen.getByLabelText(t.rpcMemberBinary), { target: { value: '/x' } });
    fireEvent.click(within(screen.getByRole('form')).getByLabelText('Lab'));
    fireEvent.click(screen.getByRole('button', { name: t.save }));
    await waitFor(() => expect(api.createRPCMemberSpec).toHaveBeenCalled());
    expect(api.createRPCMemberSpec.mock.calls[0][1].allowed_head_groups).toEqual(['g3']);
  });

  it('shows a stored group the caller cannot list as checked', async () => {
    setup({ server: { ...server, rpc_allowed_head_groups: ['g9'] } });
    const unlisted = await screen.findByLabelText(t.rpcAllowedHeadGroupUnlisted('g9'));
    expect(unlisted).toBeChecked();
    expect(screen.queryByText(t.rpcAllowedHeadGroupsDefault)).toBeNull();
    fireEvent.click(unlisted);
    expect(screen.getByText(t.rpcAllowedHeadGroupsDefault)).toBeTruthy();
  });

  it('shows a member spec group the caller cannot list as checked', async () => {
    setup({ specs: [spec({ allowed_head_groups: ['g9'] })] });
    fireEvent.click(
      await screen.findByRole('button', { name: `${t.runtimeSpecEditAction}: rpcm_1` }),
    );
    expect(
      within(screen.getByRole('form')).getByLabelText(t.rpcAllowedHeadGroupUnlisted('g9')),
    ).toBeChecked();
  });

  it('falls back to the server own groups when the list cannot be read', async () => {
    setup({ groupCandidatesError: new Error('denied') });
    expect(await screen.findByLabelText('Ops')).toBeTruthy();
    expect(screen.getByLabelText('Research')).toBeTruthy();
  });
});

describe('RpcMemberSection editor lifetime', () => {
  it('closes the editor when the edited spec is gone', async () => {
    const api = setup({ specs: [spec()] });
    fireEvent.click(
      await screen.findByRole('button', { name: `${t.runtimeSpecEditAction}: rpcm_1` }),
    );
    expect(screen.getByRole('form')).toBeTruthy();
    api.rpcMemberSpecs.mockResolvedValue({ data: [] });
    fireEvent.click(screen.getByRole('button', { name: `${t.runtimeSpecDelete}: rpcm_1` }));
    fireEvent.click(
      within(screen.getByRole('dialog')).getByRole('button', { name: t.runtimeSpecDelete }),
    );
    await waitFor(() => expect(api.rpcMemberSpecs).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(screen.queryByRole('form', { hidden: true })).toBeNull());
  });

  it('keeps a new spec editor open across a reload', async () => {
    const api = setup({ specs: [spec()] });
    fireEvent.click(await screen.findByRole('button', { name: t.rpcMemberAdd }));
    api.rpcMemberSpecs.mockResolvedValue({ data: [] });
    fireEvent.click(screen.getByRole('button', { name: `${t.runtimeSpecDelete}: rpcm_1` }));
    fireEvent.click(
      within(screen.getByRole('dialog')).getByRole('button', { name: t.runtimeSpecDelete }),
    );
    await waitFor(() => expect(screen.queryByText('rpcm_1')).toBeNull());
    expect(screen.getByRole('form', { hidden: true })).toBeTruthy();
  });
});
