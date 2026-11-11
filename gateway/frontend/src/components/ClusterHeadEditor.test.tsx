// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { useState } from 'react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  ClusterHeadEditor,
  clusterBodyFields,
  clusterSaveError,
  clusterStateFromSpec,
  emptyClusterState,
  type ClusterHeadState,
} from './ClusterHeadEditor';
import { messages } from '../i18n';
import { PortalApiError } from '../api';
import type {
  ClusterCandidate,
  ClusterCandidateList,
  ClusterMember,
  HardwareResponse,
  PortalServer,
  RPCMemberSpec,
  RuntimeSpec,
} from '../api';

const t = messages.de;

afterEach(cleanup);

const headServer = {
  id: 'srv_head',
  name: 'Head',
  netbird_enabled: false,
  lan_address: '',
} as unknown as PortalServer;

function member(over: Partial<ClusterMember> = {}): ClusterMember {
  return {
    position: 0,
    server_id: 'srv_a',
    server_name: 'Alpha',
    member_spec_id: '',
    gpus: [],
    threads: 0,
    cache_enabled: false,
    device_backend: 'auto',
    ram_estimate_mb: 0,
    ram_measured_mb: 0,
    stale: false,
    port: 0,
    visible: true,
    ...over,
  };
}

// What the read view sends for a member on a server the caller may not
// manage: its position and nothing else.
function hiddenMember(position: number): ClusterMember {
  return member({ position, server_id: '', server_name: '', gpus: [], visible: false });
}

function memberSpec(id: string, over: Partial<RPCMemberSpec> = {}): RPCMemberSpec {
  return {
    id,
    server_id: 'srv_a',
    enabled: true,
    binary: '/opt/llama/ggml-rpc-server',
    args: [],
    env: {},
    gpus: [],
    cache_enabled: false,
    device_backend: 'auto',
    ram_estimate_mb: 0,
    allowed_head_groups: [],
    ...over,
  };
}

function hardware(indices: number[]): HardwareResponse {
  return {
    available: true,
    report: {
      collected_at: '',
      agent_version: '0.18.0',
      os: 'linux',
      arch: 'amd64',
      cpu: {} as never,
      memory: {} as never,
      mainboard: {} as never,
      bios: {} as never,
      gpus: indices.map((index) => ({
        index,
        name: `RTX ${index}`,
        memory_total_bytes: 24 * 1024 ** 3,
      })),
    },
  };
}

function state(over: Partial<ClusterHeadState> = {}): ClusterHeadState {
  return { ...emptyClusterState(), enabled: true, ...over };
}

function setup(
  opts: {
    initial?: ClusterHeadState;
    stored?: Partial<RuntimeSpec>;
    args?: string[];
    env?: Record<string, string>;
    mappingId?: string;
    candidates?: ClusterCandidate[];
    hardwareByServer?: Record<string, HardwareResponse>;
    hardwareFailing?: string[];
    headHasGpus?: boolean;
    headBinaryVersion?: string;
    server?: PortalServer;
    resetMemberUsage?: (serverId: string) => Promise<RuntimeSpec>;
    headBinary?: string;
    // The listing per head binary the request names; others answer `candidates`.
    candidatesByBinary?: Record<string, ClusterCandidateList>;
  } = {},
) {
  const api = {
    clusterCandidates: vi.fn(
      async (_mappingId: string, _mode: string, binary = ''): Promise<ClusterCandidateList> =>
        opts.candidatesByBinary?.[binary] ?? { data: opts.candidates ?? [] },
    ),
    applicationClusterCandidates: vi.fn(
      async (_appId: string, _mode: string, binary = ''): Promise<ClusterCandidateList> =>
        opts.candidatesByBinary?.[binary] ?? { data: opts.candidates ?? [] },
    ),
    serverHardware: vi.fn(async (id: string) => {
      if (opts.hardwareFailing?.includes(id)) throw new Error('hardware read failed');
      return opts.hardwareByServer?.[id] ?? ({ available: false } as HardwareResponse);
    }),
  };
  const latest: { value: ClusterHeadState } = { value: opts.initial ?? state() };
  const binary = { set: (_b: string) => {} };
  function Harness() {
    const [value, setValue] = useState<ClusterHeadState>(latest.value);
    const [headBinary, setHeadBinary] = useState(opts.headBinary ?? '');
    binary.set = setHeadBinary;
    return (
      <ClusterHeadEditor
        t={t}
        api={api}
        mappingId={opts.mappingId ?? 'map_1'}
        applicationId="app_1"
        headServer={opts.server ?? headServer}
        stored={opts.stored as RuntimeSpec | undefined}
        value={value}
        onChange={(next) => {
          latest.value = next;
          setValue(next);
        }}
        args={opts.args ?? ['--rpc', '${RPC_SERVERS}']}
        env={opts.env ?? {}}
        headHasGpus={opts.headHasGpus ?? false}
        startupTimeoutSeconds={600}
        headBinaryVersion={opts.headBinaryVersion}
        headBinary={headBinary}
        resetMemberUsage={opts.resetMemberUsage}
      />
    );
  }
  render(<Harness />);
  return { api, latest, setHeadBinary: (b: string) => act(() => binary.set(b)) };
}

// MUI's select is not native: open it, then pick the option.
function pick(label: string, option: string) {
  fireEvent.mouseDown(screen.getByRole('combobox', { name: label }));
  fireEvent.click(within(screen.getByRole('listbox')).getByRole('option', { name: option }));
}

describe('ClusterHeadEditor cluster switch', () => {
  it('asks for ${RPC_SERVERS} once the cluster is on and the args lack it', () => {
    const { latest } = setup({ initial: emptyClusterState(), args: ['-m', '/m.gguf'] });
    expect(screen.queryByText(t.clusterRpcHint)).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole('checkbox', { name: t.clusterEnabledLabel }));
    expect(latest.value.enabled).toBe(true);
    expect(screen.getByText(t.clusterRpcHint)).toBeInTheDocument();
  });

  it('does not ask once --rpc ${RPC_SERVERS} is there', () => {
    setup({ args: ['--rpc', '${RPC_SERVERS}'] });
    expect(screen.queryByText(t.clusterRpcHint)).not.toBeInTheDocument();
  });

  it('sets up a head on a spec that is not saved yet from the application listing', async () => {
    const { api, latest } = setup({
      initial: emptyClusterState(),
      mappingId: '',
      headBinary: '/opt/llama/llama-server',
      candidates: [
        { server_id: 'srv_a', server_name: 'Alpha', compatible: true, rpc_binary: '/b' },
      ],
    });
    fireEvent.click(screen.getByRole('checkbox', { name: t.clusterEnabledLabel }));
    expect(latest.value.enabled).toBe(true);
    fireEvent.click(screen.getByRole('button', { name: t.clusterMemberAdd }));
    await screen.findByRole('combobox', { name: t.clusterMemberServer });
    pick(t.clusterMemberServer, 'Alpha');
    await waitFor(() =>
      expect(screen.getByRole('button', { name: t.clusterMemberAddConfirm })).toBeEnabled(),
    );
    fireEvent.click(screen.getByRole('button', { name: t.clusterMemberAddConfirm }));
    expect(latest.value.members.map((m) => m.server_id)).toEqual(['srv_a']);
    expect(api.applicationClusterCandidates).toHaveBeenCalledWith(
      'app_1',
      'automatic',
      '/opt/llama/llama-server',
    );
    expect(api.clusterCandidates).not.toHaveBeenCalled();
  });

  it('keeps the device backend label short and names the placeholders beneath it', () => {
    setup();
    const select = screen.getByRole('combobox', { name: t.clusterDeviceBackend });
    expect(t.clusterDeviceBackend).not.toContain('${');
    expect(select).toHaveAccessibleDescription(t.clusterDeviceBackendHint);
    expect(t.clusterDeviceBackendHint).toContain('${DEVICES}');
    expect(t.clusterDeviceBackendHint).toContain('${FIRST_LOCAL_DEVICE}');
  });

  it('shows the expected cold start against the head startup timeout', () => {
    setup();
    expect(screen.getByText(t.clusterColdStart(600))).toBeInTheDocument();
  });
});

describe('ClusterHeadEditor member mode', () => {
  it('confirms a switch that replaces the member list', async () => {
    const { latest } = setup({ initial: state({ members: clusterRows([member()]) }) });
    pick(t.clusterMemberMode, t.clusterMemberModeManual);
    expect(screen.getByText(t.clusterModeSwitchBody)).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: t.cancel }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(latest.value.memberMode).toBe('automatic');
    expect(latest.value.members).toHaveLength(1);

    pick(t.clusterMemberMode, t.clusterMemberModeManual);
    fireEvent.click(screen.getByRole('button', { name: t.clusterModeSwitchConfirm }));
    expect(latest.value.memberMode).toBe('manual');
    expect(latest.value.members).toEqual([]);
  });

  it('switches without asking while there are no members', () => {
    const { latest } = setup();
    pick(t.clusterMemberMode, t.clusterMemberModeManual);
    expect(screen.queryByText(t.clusterModeSwitchBody)).not.toBeInTheDocument();
    expect(latest.value.memberMode).toBe('manual');
  });

  it('closes an open member picker on a switch, since it lists the other mode', async () => {
    const { api } = setup({
      candidates: [{ server_id: 'srv_a', server_name: 'Alpha' }],
    });
    fireEvent.click(screen.getByRole('button', { name: t.clusterMemberAdd }));
    await screen.findByRole('combobox', { name: t.clusterMemberServer });
    pick(t.clusterMemberMode, t.clusterMemberModeManual);
    expect(screen.queryByRole('combobox', { name: t.clusterMemberServer })).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: t.clusterMemberAdd })).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: t.clusterMemberAdd }));
    await waitFor(() =>
      expect(api.clusterCandidates).toHaveBeenLastCalledWith('map_1', 'manual', ''),
    );
  });
});

describe('ClusterHeadEditor member binaries', () => {
  it('notes that members use the matching rpc-server and names the head version', () => {
    setup({
      initial: state({ members: clusterRows([member({})]) }),
      headBinaryVersion: 'b1234',
    });
    expect(screen.getByTestId('cluster-members-binary').textContent).toBe(
      t.clusterMembersMatchingBinary('b1234'),
    );
  });

  it('says the version is unknown when the inventory has none for the head binary', () => {
    setup({ initial: state({ members: clusterRows([member({})]) }) });
    expect(screen.getByTestId('cluster-members-binary').textContent).toBe(
      t.clusterMembersMatchingBinary(t.binariesVersionUnknown),
    );
  });

  it('shows the matched rpc binary and version of an automatic candidate', async () => {
    setup({
      candidates: [
        {
          server_id: 'srv_a',
          server_name: 'Alpha',
          rpc_binary: '/opt/llama/ggml-rpc-server',
          rpc_binary_version: 'b1234',
        },
      ],
    });
    fireEvent.click(screen.getByRole('button', { name: t.clusterMemberAdd }));
    await screen.findByRole('combobox', { name: t.clusterMemberServer });
    pick(t.clusterMemberServer, 'Alpha');
    expect(screen.getByTestId('cluster-candidate-binary').textContent).toBe(
      '/opt/llama/ggml-rpc-server (b1234)',
    );
  });
});

describe('ClusterHeadEditor head binary change', () => {
  const alpha = (compatible: boolean): ClusterCandidate => ({
    server_id: 'srv_a',
    server_name: 'Alpha',
    compatible,
    ...(compatible ? { rpc_binary: '/opt/llama/ggml-rpc-server', rpc_binary_version: 'b1' } : {}),
  });
  const beta: ClusterCandidate = {
    server_id: 'srv_b',
    server_name: 'Beta',
    compatible: true,
    rpc_binary: '/opt/b2/ggml-rpc-server',
    rpc_binary_version: 'b2',
  };

  it('re-checks the members against a changed head binary', async () => {
    const { api, setHeadBinary } = setup({
      initial: state({
        members: clusterRows([member({ server_id: 'srv_a', server_name: 'Alpha' })]),
      }),
      headBinary: '/opt/llama/llama-server',
      candidatesByBinary: {
        '/opt/llama/llama-server': { data: [alpha(true)], head_binary_known: true },
        '/opt/b2/llama-server': { data: [alpha(false), beta], head_binary_known: true },
      },
    });
    await waitFor(() =>
      expect(api.clusterCandidates).toHaveBeenCalledWith(
        'map_1',
        'automatic',
        '/opt/llama/llama-server',
      ),
    );
    expect(screen.queryByTestId('cluster-member-problem')).not.toBeInTheDocument();

    setHeadBinary('/opt/b2/llama-server');
    expect(await screen.findByTestId('cluster-member-problem')).toHaveTextContent(
      t.clusterMemberNoMatchingRpcBinary,
    );
    expect(api.clusterCandidates).toHaveBeenLastCalledWith(
      'map_1',
      'automatic',
      '/opt/b2/llama-server',
    );
  });

  it('flags an automatic member whose server is no longer offered', async () => {
    setup({
      initial: state({
        members: clusterRows([member({ server_id: 'srv_a', server_name: 'Alpha' })]),
      }),
      headBinary: '/opt/llama/llama-server',
      candidatesByBinary: { '/opt/llama/llama-server': { data: [beta], head_binary_known: true } },
    });
    expect(await screen.findByTestId('cluster-member-problem')).toHaveTextContent(
      t.clusterMemberNotOffered,
    );
  });

  it('flags a manual member whose spec is no longer offered', async () => {
    setup({
      initial: state({
        memberMode: 'manual',
        members: clusterRows([
          member({ server_id: 'srv_a', server_name: 'Alpha', member_spec_id: 'rpcm_gone' }),
        ]),
      }),
      headBinary: '/opt/llama/llama-server',
      candidatesByBinary: {
        '/opt/llama/llama-server': {
          data: [
            {
              server_id: 'srv_a',
              server_name: 'Alpha',
              member_specs: [memberSpec('rpcm_other')],
              compatible: true,
            },
          ],
          head_binary_known: true,
        },
      },
    });
    expect(await screen.findByTestId('cluster-member-problem')).toHaveTextContent(
      t.clusterMemberSpecNotOffered,
    );
  });

  it('says so once when the head binary has no known version', async () => {
    setup({
      initial: state({
        members: clusterRows([member({ server_id: 'srv_a', server_name: 'Alpha' })]),
      }),
      headBinary: '/somewhere/llama-server',
      candidatesByBinary: {
        '/somewhere/llama-server': { data: [alpha(false)], head_binary_known: false },
      },
    });
    expect(await screen.findByTestId('cluster-head-version-unknown')).toHaveTextContent(
      t.errorClusterHeadBinaryVersionUnknown,
    );
    expect(screen.queryByTestId('cluster-member-problem')).not.toBeInTheDocument();
  });

  it('reads a quickly typed binary once, after the pause', async () => {
    const { api, setHeadBinary } = setup({ headBinary: '/opt/llama/llama-server' });
    await waitFor(() => expect(api.clusterCandidates).toHaveBeenCalledTimes(1));
    setHeadBinary('/opt/b');
    setHeadBinary('/opt/b2');
    setHeadBinary('/opt/b2/llama-server');
    await waitFor(() =>
      expect(api.clusterCandidates).toHaveBeenLastCalledWith(
        'map_1',
        'automatic',
        '/opt/b2/llama-server',
      ),
    );
    expect(api.clusterCandidates).toHaveBeenCalledTimes(2);
  });

  it('refreshes an open picker for the new head binary', async () => {
    const { api, setHeadBinary } = setup({
      headBinary: '/opt/llama/llama-server',
      candidatesByBinary: {
        '/opt/llama/llama-server': { data: [alpha(true)], head_binary_known: true },
        '/opt/b2/llama-server': { data: [alpha(false), beta], head_binary_known: true },
      },
    });
    fireEvent.click(screen.getByRole('button', { name: t.clusterMemberAdd }));
    await screen.findByRole('combobox', { name: t.clusterMemberServer });
    pick(t.clusterMemberServer, 'Alpha');
    expect(screen.getByTestId('cluster-candidate-binary')).toBeInTheDocument();

    setHeadBinary('/opt/b2/llama-server');
    await waitFor(() =>
      expect(api.clusterCandidates).toHaveBeenLastCalledWith(
        'map_1',
        'automatic',
        '/opt/b2/llama-server',
      ),
    );
    // Alpha has no matching ggml-rpc-server any more: the choice is gone and
    // only Beta is offered.
    await waitFor(() =>
      expect(screen.queryByTestId('cluster-candidate-binary')).not.toBeInTheDocument(),
    );
    fireEvent.mouseDown(screen.getByRole('combobox', { name: t.clusterMemberServer }));
    const options = within(screen.getByRole('listbox')).getAllByRole('option');
    expect(options.map((o) => o.textContent)).toEqual(['—', 'Beta']);
  });

  it('flags a manual member whose spec binary is of another version, and offers only matching specs', async () => {
    setup({
      initial: state({
        memberMode: 'manual',
        members: clusterRows([
          member({ server_id: 'srv_a', server_name: 'Alpha', member_spec_id: 'rpcm_old' }),
        ]),
      }),
      headBinary: '/opt/llama/llama-server',
      candidatesByBinary: {
        '/opt/llama/llama-server': {
          data: [
            {
              server_id: 'srv_a',
              server_name: 'Alpha',
              member_specs: [memberSpec('rpcm_old')],
              incompatible_member_specs: ['rpcm_old'],
              compatible: false,
            },
            {
              server_id: 'srv_b',
              server_name: 'Beta',
              member_specs: [memberSpec('rpcm_new'), memberSpec('rpcm_stale')],
              incompatible_member_specs: ['rpcm_stale'],
              compatible: true,
            },
          ],
          head_binary_known: true,
        },
      },
    });
    expect(await screen.findByTestId('cluster-member-problem')).toHaveTextContent(
      t.clusterMemberVersionMismatch,
    );
    fireEvent.click(screen.getByRole('button', { name: t.clusterMemberAdd }));
    pick(t.clusterMemberServer, 'Beta');
    // One compatible spec left: it is taken without asking.
    expect(screen.queryByText(t.clusterMemberSpecPick)).not.toBeInTheDocument();
    await waitFor(() =>
      expect(screen.getByRole('button', { name: t.clusterMemberAddConfirm })).toBeEnabled(),
    );
  });
});

describe('ClusterHeadEditor member GPU names', () => {
  const gpu = (index: number) => ({ index, vram_estimate_mb: 0, vram_measured_mb: 0 });

  it('names an automatic member GPU as the picker did', async () => {
    setup({
      initial: state({
        members: clusterRows([
          member({ server_id: 'srv_a', server_name: 'Alpha', gpus: [gpu(1)] }),
        ]),
      }),
      hardwareByServer: { srv_a: hardware([0, 1]) },
    });
    const row = screen.getByTestId('cluster-member-row');
    expect(await within(row).findByText('GPU 1 · RTX 1')).toBeInTheDocument();
    // The labels of the row's controls stay keyed on the GPU index.
    expect(
      within(row).getByRole('button', { name: `${t.clusterMemberGpuRemove}: Alpha GPU 1` }),
    ).toBeInTheDocument();
  });

  it('falls back to the index without a hardware report', async () => {
    const { api } = setup({
      initial: state({
        members: clusterRows([
          member({ server_id: 'srv_a', server_name: 'Alpha', gpus: [gpu(1)] }),
        ]),
      }),
    });
    await waitFor(() => expect(api.serverHardware).toHaveBeenCalledWith('srv_a'));
    expect(await screen.findByText(t.clusterMemberHardwareUnavailable)).toBeInTheDocument();
    expect(within(screen.getByTestId('cluster-member-row')).getByText('GPU 1')).toBeInTheDocument();
  });

  it('names a manual member GPU from its server report too', async () => {
    setup({
      initial: state({
        memberMode: 'manual',
        members: clusterRows([
          member({
            server_id: 'srv_a',
            server_name: 'Alpha',
            member_spec_id: 'rpcm_1',
            gpus: [gpu(0), gpu(1)],
          }),
        ]),
      }),
      hardwareByServer: { srv_a: hardware([0, 1]) },
    });
    expect(await screen.findByText('GPU 0 · RTX 0, GPU 1 · RTX 1')).toBeInTheDocument();
  });

  it('confirms an added member with "Hinzufügen" / "Add"', () => {
    expect(messages.de.clusterMemberAddConfirm).toBe('Hinzufügen');
    expect(messages.en.clusterMemberAddConfirm).toBe('Add');
  });
});

describe('ClusterHeadEditor member order', () => {
  it('moves a member and sends the new order and positions', () => {
    const { latest } = setup({
      initial: state({
        members: clusterRows([
          member({ server_id: 'srv_a', server_name: 'Alpha', position: 0 }),
          member({ server_id: 'srv_b', server_name: 'Beta', position: 1 }),
        ]),
      }),
    });
    fireEvent.click(screen.getByRole('button', { name: `${t.modelGroupMoveDown}: Alpha` }));
    const body = clusterBodyFields(latest.value);
    expect(body.cluster_members?.map((m) => [m.server_id, m.position])).toEqual([
      ['srv_b', 0],
      ['srv_a', 1],
    ]);
  });

  it('removes a member', () => {
    const { latest } = setup({
      initial: state({ members: clusterRows([member({ server_name: 'Alpha' })]) }),
    });
    fireEvent.click(screen.getByRole('button', { name: `${t.clusterMemberRemove}: Alpha` }));
    expect(latest.value.members).toEqual([]);
  });
});

describe('ClusterHeadEditor automatic members', () => {
  it('adds a server from the automatic candidates and orders its GPUs', async () => {
    const { api, latest } = setup({
      candidates: [
        { server_id: 'srv_a', server_name: 'Alpha', rpc_binary: '/b', rpc_binary_version: 'b1' },
      ],
      hardwareByServer: { srv_a: hardware([0, 1]) },
    });
    fireEvent.click(screen.getByRole('button', { name: t.clusterMemberAdd }));
    await waitFor(() =>
      expect(api.clusterCandidates).toHaveBeenCalledWith('map_1', 'automatic', ''),
    );
    await screen.findByRole('combobox', { name: t.clusterMemberServer });
    pick(t.clusterMemberServer, 'Alpha');
    fireEvent.click(screen.getByRole('button', { name: t.clusterMemberAddConfirm }));
    expect(latest.value.members.map((m) => m.server_id)).toEqual(['srv_a']);

    await waitFor(() => expect(api.serverHardware).toHaveBeenCalledWith('srv_a'));
    const gpuLabel = `${t.clusterMemberGpuAdd}: Alpha`;
    await screen.findByRole('combobox', { name: gpuLabel });
    pick(gpuLabel, 'GPU 1 · RTX 1');
    pick(gpuLabel, 'GPU 0 · RTX 0');
    expect(latest.value.members[0].gpus.map((g) => g.index)).toEqual([1, 0]);

    fireEvent.click(screen.getByRole('button', { name: `${t.modelGroupMoveUp}: Alpha GPU 0` }));
    const body = clusterBodyFields(latest.value);
    expect(body.cluster_members?.[0].gpus.map((g) => g.index)).toEqual([0, 1]);
  });

  it('edits the member settings', () => {
    const { latest } = setup({
      initial: state({ members: clusterRows([member({ server_name: 'Alpha' })]) }),
    });
    fireEvent.change(screen.getByLabelText(`Alpha: ${t.clusterMemberThreads}`), {
      target: { value: '8' },
    });
    fireEvent.change(screen.getByLabelText(`Alpha: ${t.clusterMemberRamEstimate}`), {
      target: { value: '2048' },
    });
    fireEvent.click(screen.getByRole('checkbox', { name: `Alpha: ${t.rpcMemberCache}` }));
    expect(latest.value.members[0]).toMatchObject({
      threads: 8,
      ram_estimate_mb: 2048,
      cache_enabled: true,
    });
  });

  it('tells a failed hardware read from a server without GPUs or a report', async () => {
    setup({
      initial: state({
        members: clusterRows([
          member({ server_id: 'srv_a', server_name: 'Alpha' }),
          member({ server_id: 'srv_b', server_name: 'Beta' }),
          member({ server_id: 'srv_c', server_name: 'Gamma' }),
        ]),
      }),
      hardwareFailing: ['srv_a'],
      hardwareByServer: { srv_b: hardware([]) },
    });
    const rows = screen.getAllByTestId('cluster-member-row');
    expect(await within(rows[0]).findByText(t.clusterMemberHardwareFailed)).toBeInTheDocument();
    expect(await within(rows[1]).findByText(t.clusterMemberNoGpusReported)).toBeInTheDocument();
    expect(
      await within(rows[2]).findByText(t.clusterMemberHardwareUnavailable),
    ).toBeInTheDocument();
  });

  it('says what a server needs when none is offered', async () => {
    setup({ candidates: [] });
    fireEvent.click(screen.getByRole('button', { name: t.clusterMemberAdd }));
    expect(await screen.findByText(t.clusterCandidatesEmptyAutomatic)).toBeInTheDocument();
  });
});

describe('ClusterHeadEditor manual members', () => {
  it('asks for the spec when a server has several free ones', async () => {
    const { api, latest } = setup({
      initial: state({ memberMode: 'manual' }),
      candidates: [
        {
          server_id: 'srv_a',
          server_name: 'Alpha',
          member_specs: [
            memberSpec('rpcm_1'),
            memberSpec('rpcm_2', {
              binary: '/opt/b2/ggml-rpc-server',
              gpus: [
                { index: 3, vram_estimate_mb: 0, vram_measured_mb: 0 },
                { index: 1, vram_estimate_mb: 0, vram_measured_mb: 0 },
              ],
            }),
          ],
        },
      ],
    });
    fireEvent.click(screen.getByRole('button', { name: t.clusterMemberAdd }));
    await waitFor(() => expect(api.clusterCandidates).toHaveBeenCalledWith('map_1', 'manual', ''));
    await screen.findByRole('combobox', { name: t.clusterMemberServer });
    pick(t.clusterMemberServer, 'Alpha');
    expect(screen.getByText(t.clusterMemberSpecPick)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: t.clusterMemberAddConfirm })).toBeDisabled();
    // Each offered spec names its devices and binary, not only its id.
    fireEvent.mouseDown(screen.getByRole('combobox', { name: t.clusterMemberSpec }));
    const options = within(screen.getByRole('listbox')).getAllByRole('option');
    expect(options.map((o) => o.textContent)).toEqual([
      '—',
      `rpcm_1 · ${t.rpcMemberCpu} · /opt/llama/ggml-rpc-server`,
      'rpcm_2 · GPU 3, GPU 1 · /opt/b2/ggml-rpc-server',
    ]);
    fireEvent.click(options[2]);
    fireEvent.click(screen.getByRole('button', { name: t.clusterMemberAddConfirm }));
    expect(latest.value.members[0]).toMatchObject({ server_id: 'srv_a', member_spec_id: 'rpcm_2' });
  });

  it('resolves a server with one free spec to it', async () => {
    const { latest } = setup({
      initial: state({ memberMode: 'manual' }),
      candidates: [
        { server_id: 'srv_a', server_name: 'Alpha', member_specs: [memberSpec('rpcm_1')] },
      ],
    });
    fireEvent.click(screen.getByRole('button', { name: t.clusterMemberAdd }));
    await screen.findByRole('combobox', { name: t.clusterMemberServer });
    pick(t.clusterMemberServer, 'Alpha');
    expect(screen.queryByText(t.clusterMemberSpecPick)).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: t.clusterMemberAddConfirm }));
    expect(latest.value.members[0].member_spec_id).toBe('rpcm_1');
  });
});

describe('ClusterHeadEditor network and transport', () => {
  it('shows the stored defaults with their reasons', () => {
    setup({
      stored: { cluster_network_default: 'mesh', cluster_transport_default: 'relay' },
    });
    expect(screen.getByRole('combobox', { name: t.clusterNetwork })).toHaveTextContent(
      t.clusterDefaultOption(t.clusterNetworkMesh),
    );
    expect(screen.getByText(t.clusterNetworkReasonMesh)).toBeInTheDocument();
    expect(screen.getByText(t.clusterTransportReasonMeshRelay)).toBeInTheDocument();
  });

  it('confirms the LAN exposure before direct + LAN takes effect', async () => {
    const { latest } = setup({ initial: state({ network: 'lan', transport: 'relay' }) });
    expect(screen.getByText(t.clusterLanAddressHint)).toBeInTheDocument();
    pick(t.clusterTransport, t.clusterTransportDirect);
    expect(screen.getByText(t.clusterLanAckBody)).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: t.cancel }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(latest.value.transport).toBe('relay');
    expect(latest.value.lanAck).toBe(false);

    pick(t.clusterTransport, t.clusterTransportDirect);
    fireEvent.click(screen.getByRole('button', { name: t.clusterLanAckConfirm }));
    expect(latest.value).toMatchObject({ transport: 'direct', lanAck: true });
    expect(screen.getByText(t.clusterLanAckGiven)).toBeInTheDocument();
    expect(clusterBodyFields(latest.value).lan_exposure_ack).toBe(true);
  });

  it('flags a NetBird head without a LAN address in a LAN cluster', () => {
    setup({
      initial: state({ network: 'lan', transport: 'relay' }),
      server: { ...headServer, netbird_enabled: true, lan_address: '' } as PortalServer,
    });
    expect(screen.getByText(t.clusterLanAddressHeadMissing)).toBeInTheDocument();
  });
});

describe('ClusterHeadEditor hints and warnings', () => {
  it('renders the stored cluster warnings with their labels', () => {
    setup({
      stored: {
        cluster_enabled: true,
        cluster_warnings: ['cluster_cpu_member_without_tensor_split', 'cluster_member_gpu_overlap'],
      },
    });
    expect(screen.getByText(t.clusterWarnCpuMemberWithoutTensorSplit)).toBeInTheDocument();
    expect(screen.getByText(t.clusterWarnGpuOverlap)).toBeInTheDocument();
  });

  it('recommends the RPC devices first', () => {
    setup({
      args: ['--rpc', '${RPC_SERVERS}', '--device', 'CUDA0,${RPC_DEVICES}'],
      headHasGpus: true,
    });
    expect(screen.getByText(t.clusterDeviceOrderHint)).toBeInTheDocument();
    expect(screen.queryByText(t.clusterMmdevHint)).not.toBeInTheDocument();
  });

  it('recommends -mmdev when the list starts with RPC devices', () => {
    setup({ args: ['--rpc', '${RPC_SERVERS}', '--device', '${DEVICES}'], headHasGpus: true });
    expect(screen.getByText(t.clusterMmdevHint)).toBeInTheDocument();
    expect(screen.queryByText(t.clusterDeviceOrderHint)).not.toBeInTheDocument();
  });
});

describe('ClusterHeadEditor hidden members', () => {
  it('shows a hidden member in place, locks the membership and sends it back unchanged', () => {
    const hidden = hiddenMember(1);
    const spec = {
      cluster_enabled: true,
      member_mode: 'automatic',
      cluster_network: 'lan',
      rpc_transport: 'relay',
      lan_exposure_ack: false,
      device_backend: 'auto',
      cluster_members: [
        member({ server_id: 'srv_a', server_name: 'Alpha', position: 0 }),
        hidden,
        member({ server_id: 'srv_b', server_name: 'Beta', position: 2 }),
      ],
    } as RuntimeSpec;
    const { latest } = setup({ initial: clusterStateFromSpec(spec), stored: spec });

    const rows = screen.getAllByTestId('cluster-member-row');
    expect(rows).toHaveLength(3);
    expect(rows[1]).toHaveTextContent(t.clusterMemberHidden);
    expect(screen.getByText(t.clusterMembersLocked)).toBeInTheDocument();
    expect(screen.getByRole('checkbox', { name: t.clusterEnabledLabel })).toBeEnabled();
    expect(screen.getByRole('button', { name: t.clusterMemberAdd })).toBeDisabled();
    expect(screen.getByRole('button', { name: `${t.modelGroupMoveDown}: Alpha` })).toBeDisabled();
    expect(screen.getByRole('button', { name: `${t.clusterMemberRemove}: Beta` })).toBeDisabled();
    expect(within(rows[1]).queryAllByRole('button')).toHaveLength(0);

    const body = clusterBodyFields(latest.value);
    expect(body.cluster_members?.[1]).toEqual(hidden);
    expect(body.cluster_members?.map((m) => m.server_id)).toEqual(['srv_a', '', 'srv_b']);
  });
});

describe('ClusterHeadEditor cluster switch with a hidden member', () => {
  const hiddenHead = {
    cluster_enabled: true,
    member_mode: 'automatic',
    cluster_network: 'lan',
    rpc_transport: 'relay',
    lan_exposure_ack: false,
    device_backend: 'auto',
    cluster_members: [
      member({ server_id: 'srv_a', server_name: 'Alpha', position: 0 }),
      hiddenMember(1),
    ],
  } as RuntimeSpec;

  it('turns the cluster off with the head rights alone', () => {
    const { latest } = setup({ initial: clusterStateFromSpec(hiddenHead), stored: hiddenHead });
    fireEvent.click(screen.getByRole('checkbox', { name: t.clusterEnabledLabel }));
    expect(latest.value.enabled).toBe(false);
    expect(clusterBodyFields(latest.value)).toMatchObject({
      cluster_enabled: false,
      cluster_members: [],
    });
    expect(screen.getByText(t.clusterMembersLocked)).toBeInTheDocument();
  });

  it('can undo the switch-off back to the stored head', () => {
    const { latest } = setup({ initial: clusterStateFromSpec(hiddenHead), stored: hiddenHead });
    const toggle = screen.getByRole('checkbox', { name: t.clusterEnabledLabel });
    fireEvent.click(toggle);
    fireEvent.click(toggle);
    expect(latest.value.enabled).toBe(true);
    expect(clusterBodyFields(latest.value).cluster_members?.map((m) => m.server_id)).toEqual([
      'srv_a',
      '',
    ]);
  });

  it('cannot turn a cluster on that the stored spec does not run', () => {
    setup({
      initial: { ...clusterStateFromSpec(hiddenHead), enabled: false },
      stored: { ...hiddenHead, cluster_enabled: false },
    });
    expect(screen.getByRole('checkbox', { name: t.clusterEnabledLabel })).toBeDisabled();
  });

  it('says that only turning the cluster off is open', () => {
    for (const m of [messages.de, messages.en]) {
      expect(m.clusterMembersLocked).not.toMatch(/Cluster-Schalter|cluster switch/);
    }
    expect(messages.de.clusterMembersLocked).toContain('ausschalten');
    expect(messages.en.clusterMembersLocked).toContain('turn the cluster off');
  });
});

describe('ClusterHeadEditor cluster totals', () => {
  it('marks only the incomplete axis as a lower bound', () => {
    setup({
      stored: {
        cluster_enabled: true,
        cluster_vram_total_mb: 30000,
        cluster_ram_total_mb: 20000,
        cluster_totals_complete: false,
        cluster_vram_total_complete: true,
        cluster_ram_total_complete: false,
        cluster_members: [],
      },
    });
    expect(screen.getByTestId('cluster-totals')).toHaveTextContent(
      `${t.clusterTotals}: VRAM 30000 MB, RAM ≥ 20000 MB (${t.clusterTotalsLowerBound})`,
    );
  });

  it('shows complete totals without a lower-bound note', () => {
    setup({
      stored: {
        cluster_enabled: true,
        cluster_vram_total_mb: 30000,
        cluster_ram_total_mb: 20000,
        cluster_totals_complete: true,
        cluster_vram_total_complete: true,
        cluster_ram_total_complete: true,
        cluster_members: [],
      },
    });
    expect(screen.getByTestId('cluster-totals').textContent).toBe(
      `${t.clusterTotals}: VRAM 30000 MB, RAM 20000 MB`,
    );
  });
});

describe('ClusterHeadEditor member usage reset', () => {
  const measured = member({
    server_name: 'Alpha',
    ram_estimate_mb: 1000,
    ram_measured_mb: 70000,
    measured_at: '2026-01-01T09:00:00Z',
    gpus: [{ index: 0, vram_estimate_mb: 8000, vram_measured_mb: 9000 }],
  });
  const unmeasured = member({ position: 1, server_id: 'srv_b', server_name: 'Beta' });
  const storedHead = {
    cluster_enabled: true,
    cluster_members: [measured, unmeasured],
  } as Partial<RuntimeSpec>;

  it('offers the reset only on a stored member with stored usage', () => {
    setup({
      initial: clusterStateFromSpec(storedHead as RuntimeSpec),
      stored: storedHead,
      resetMemberUsage: vi.fn(),
    });
    expect(
      screen.getByRole('button', { name: `${t.clusterMemberResetUsage}: Alpha` }),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole('button', { name: `${t.clusterMemberResetUsage}: Beta` }),
    ).not.toBeInTheDocument();
  });

  it('offers no reset without the action (a spec not saved yet)', () => {
    setup({ initial: clusterStateFromSpec(storedHead as RuntimeSpec), stored: storedHead });
    expect(
      screen.queryByRole('button', { name: `${t.clusterMemberResetUsage}: Alpha` }),
    ).not.toBeInTheDocument();
  });

  it('resets after a confirmation and shows the answer, keeping unsaved edits', async () => {
    const answer = {
      ...storedHead,
      cluster_members: [
        {
          ...measured,
          ram_measured_mb: 0,
          measured_at: undefined,
          gpus: [{ index: 0, vram_estimate_mb: 8000, vram_measured_mb: 0 }],
        },
        unmeasured,
      ],
    } as RuntimeSpec;
    const reset = vi.fn(async () => answer);
    const { latest } = setup({
      initial: clusterStateFromSpec(storedHead as RuntimeSpec),
      stored: storedHead,
      resetMemberUsage: reset,
    });
    // An unsaved edit made before the reset must survive it.
    fireEvent.change(screen.getByLabelText(`Alpha: ${t.clusterMemberThreads}`), {
      target: { value: '12' },
    });
    expect(screen.getByText(`${t.clusterMemberMeasured} 70000 MB`)).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: `${t.clusterMemberResetUsage}: Alpha` }));
    expect(screen.getByText(t.clusterMemberResetUsageBody)).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: t.clusterMemberResetUsageConfirm }));

    await waitFor(() => expect(reset).toHaveBeenCalledWith('srv_a'));
    await waitFor(() =>
      expect(screen.queryByText(`${t.clusterMemberMeasured} 70000 MB`)).not.toBeInTheDocument(),
    );
    expect(screen.queryByText(`${t.clusterMemberMeasured} 9000 MB`)).not.toBeInTheDocument();
    const alpha = latest.value.members.find((m) => m.server_id === 'srv_a')!;
    expect(alpha.threads).toBe(12);
    expect(alpha.ram_measured_mb).toBe(0);
    expect(alpha.gpus[0]).toEqual({ index: 0, vram_estimate_mb: 8000, vram_measured_mb: 0 });
  });

  it('does nothing when the confirmation is cancelled', async () => {
    const reset = vi.fn();
    setup({
      initial: clusterStateFromSpec(storedHead as RuntimeSpec),
      stored: storedHead,
      resetMemberUsage: reset,
    });
    fireEvent.click(screen.getByRole('button', { name: `${t.clusterMemberResetUsage}: Alpha` }));
    fireEvent.click(screen.getByRole('button', { name: t.cancel }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(reset).not.toHaveBeenCalled();
  });

  it('shows a refused reset by its label', async () => {
    const reset = vi.fn(async () => {
      throw new PortalApiError(
        404,
        'runtime_spec.cluster_member_not_found',
        'the server is no member of this cluster head',
      );
    });
    setup({
      initial: clusterStateFromSpec(storedHead as RuntimeSpec),
      stored: storedHead,
      resetMemberUsage: reset,
    });
    fireEvent.click(screen.getByRole('button', { name: `${t.clusterMemberResetUsage}: Alpha` }));
    fireEvent.click(screen.getByRole('button', { name: t.clusterMemberResetUsageConfirm }));
    expect(await screen.findByTestId('cluster-reset-error')).toHaveTextContent(
      t.errorClusterMemberNotFound,
    );
  });
});

describe('clusterSaveError', () => {
  const base = {
    type: 'llama_cpp' as const,
    replicable: false,
    startupTimeoutSeconds: 600,
    binary: '/opt/llama/llama-server',
  };

  it('refuses a head binary switch while a member is hidden', () => {
    const stored = {
      configured: true,
      binary: '/opt/llama/llama-server',
    } as RuntimeSpec;
    const s = state({ members: clusterRows([member(), hiddenMember(1)]) });
    const args = ['--rpc', '${RPC_SERVERS}'];
    expect(clusterSaveError(s, { ...base, args, env: {}, stored })).toBeNull();
    expect(
      clusterSaveError(s, { ...base, args, env: {}, stored, binary: '/opt/b2/llama-server' }),
    ).toBe('errorClusterHeadBinaryLocked');
    // Every member visible: the gateway runs the full member checks instead.
    const visible = state({ members: clusterRows([member()]) });
    expect(
      clusterSaveError(visible, { ...base, args, env: {}, stored, binary: '/opt/b2/llama-server' }),
    ).toBeNull();
  });

  it('refuses the cluster placeholders without a cluster', () => {
    expect(
      clusterSaveError(emptyClusterState(), {
        ...base,
        args: ['--rpc', '${RPC_SERVERS}'],
        env: {},
      }),
    ).toBe('errorClusterRpcPlaceholderWithoutCluster');
    expect(clusterSaveError(emptyClusterState(), { ...base, args: [], env: {} })).toBeNull();
  });

  it('runs the head document checks', () => {
    const s = state({ members: clusterRows([member()]) });
    expect(clusterSaveError(s, { ...base, args: ['--rpc', '${RPC_SERVERS}'], env: {} })).toBeNull();
    expect(clusterSaveError(s, { ...base, args: ['--rpc', 'x'], env: {} })).toBe(
      'errorClusterRpcPlaceholderMissing',
    );
    expect(clusterSaveError(state(), { ...base, args: ['--rpc', '${RPC_SERVERS}'], env: {} })).toBe(
      'errorClusterMembersRequired',
    );
    expect(
      clusterSaveError(s, {
        ...base,
        startupTimeoutSeconds: 0,
        args: ['--rpc', '${RPC_SERVERS}'],
        env: {},
      }),
    ).toBe('errorClusterStartupTimeoutRequired');
  });

  it('refuses one server twice', () => {
    const s = state({
      members: clusterRows([member({ server_id: 'srv_a' }), member({ server_id: 'srv_a' })]),
    });
    expect(clusterSaveError(s, { ...base, args: ['--rpc', '${RPC_SERVERS}'], env: {} })).toBe(
      'errorClusterMemberServerTwice',
    );
  });

  it('refuses direct + LAN without the exposure confirmation', () => {
    const s = state({
      network: 'lan',
      transport: 'direct',
      members: clusterRows([member()]),
    });
    expect(clusterSaveError(s, { ...base, args: ['--rpc', '${RPC_SERVERS}'], env: {} })).toBe(
      'errorClusterLanAckRequired',
    );
    expect(
      clusterSaveError(
        { ...s, lanAck: true },
        { ...base, args: ['--rpc', '${RPC_SERVERS}'], env: {} },
      ),
    ).toBeNull();
  });
});

describe('clusterStateFromSpec / clusterBodyFields', () => {
  it('round-trips every cluster field of a head', () => {
    const spec = {
      cluster_enabled: true,
      member_mode: 'manual',
      cluster_network: 'lan',
      rpc_transport: 'direct',
      lan_exposure_ack: true,
      device_backend: 'vulkan',
      cluster_members: [
        member({
          server_id: 'srv_a',
          member_spec_id: 'rpcm_1',
          gpus: [{ index: 2, vram_estimate_mb: 1000, vram_measured_mb: 900 }],
          port: 26001,
        }),
      ],
    } as RuntimeSpec;
    expect(clusterBodyFields(clusterStateFromSpec(spec))).toEqual({
      cluster_enabled: true,
      member_mode: 'manual',
      cluster_network: 'lan',
      rpc_transport: 'direct',
      lan_exposure_ack: true,
      device_backend: 'vulkan',
      cluster_members: spec.cluster_members,
    });
  });

  it('reads a spec from before the cluster fields as no cluster', () => {
    expect(clusterBodyFields(clusterStateFromSpec({} as RuntimeSpec))).toEqual({
      cluster_enabled: false,
      member_mode: 'automatic',
      cluster_network: '',
      rpc_transport: '',
      lan_exposure_ack: false,
      device_backend: 'auto',
      cluster_members: [],
    });
  });
});

// Member rows as the editor holds them, built through the same reader the
// form hydrates with.
function clusterRows(members: ClusterMember[]) {
  return clusterStateFromSpec({ cluster_members: members } as RuntimeSpec).members;
}
