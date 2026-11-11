// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import {
  clusterHeadDocumentError,
  deviceListStartsWithRPC,
  llamaServerValueCount,
  scanClusterArgs,
  type ClusterArgScan,
  type ClusterHeadDocumentFacts,
} from './clusterArgs';

const none: ClusterArgScan = {
  rpcInArgs: false,
  rpcInEnv: false,
  rpcArgIndex: -1,
  rpcArgCount: 0,
  rpcValueHasPlaceholder: false,
  rpcPlaceholderCount: 0,
  deviceInArgs: false,
  deviceInEnv: false,
  deviceArgIndex: -1,
  deviceList: '',
  deviceHasRPC: false,
  tensorSplit: false,
  mmdev: false,
  clusterPlaceholder: false,
  equalsForm: false,
};

// The same cases as the gateway's TestScanClusterArgs, so the two scans are
// compared on one table.
describe('scanClusterArgs', () => {
  it.each<{ name: string; args?: string[]; env?: Record<string, string>; want: ClusterArgScan }>([
    {
      name: 'rpc then device in args',
      args: ['-m', '/m.gguf', '--rpc', '${RPC_SERVERS}', '--device', '${DEVICES}'],
      want: {
        ...none,
        rpcInArgs: true,
        rpcArgIndex: 2,
        rpcArgCount: 1,
        rpcValueHasPlaceholder: true,
        rpcPlaceholderCount: 1,
        deviceInArgs: true,
        deviceArgIndex: 4,
        deviceList: '${DEVICES}',
        deviceHasRPC: true,
        clusterPlaceholder: true,
      },
    },
    {
      name: 'attached values and the short device flag',
      args: ['-dev', 'CUDA0', '--rpc=${RPC_SERVERS}', '--tensor_split=1,1', '-mmdev', 'CUDA0'],
      want: {
        ...none,
        rpcInArgs: true,
        rpcArgIndex: 2,
        rpcArgCount: 1,
        rpcValueHasPlaceholder: true,
        rpcPlaceholderCount: 1,
        deviceInArgs: true,
        deviceArgIndex: 0,
        deviceList: 'CUDA0',
        tensorSplit: true,
        mmdev: true,
        clusterPlaceholder: true,
        equalsForm: true,
      },
    },
    {
      name: 'env rpc and env device',
      env: {
        LLAMA_ARG_RPC: '${RPC_SERVERS}',
        LLAMA_ARG_DEVICE: '${RPC_DEVICES},CUDA0',
        LLAMA_ARG_TENSOR_SPLIT: '1,1',
        MTMD_BACKEND_DEVICE: 'CUDA0',
      },
      want: {
        ...none,
        rpcInEnv: true,
        rpcValueHasPlaceholder: true,
        rpcPlaceholderCount: 1,
        deviceInEnv: true,
        deviceList: '${RPC_DEVICES},CUDA0',
        deviceHasRPC: true,
        tensorSplit: true,
        mmdev: true,
        clusterPlaceholder: true,
      },
    },
    {
      name: 'args device list wins over env',
      args: ['--device', 'CUDA0'],
      env: { LLAMA_ARG_DEVICE: '${DEVICES}' },
      want: {
        ...none,
        deviceInArgs: true,
        deviceInEnv: true,
        deviceArgIndex: 0,
        deviceList: 'CUDA0',
      },
    },
    {
      name: 'placeholder outside an rpc value',
      args: ['--alias', '${RPC_SERVERS}', '--rpc', '10.0.0.1:50052', '--rpc', 'x'],
      want: {
        ...none,
        rpcInArgs: true,
        rpcArgIndex: 2,
        rpcArgCount: 2,
        rpcPlaceholderCount: 1,
        clusterPlaceholder: true,
      },
    },
  ])('$name', ({ args, env, want }) => {
    expect(scanClusterArgs(args ?? [], env ?? {})).toEqual(want);
  });

  it('steps over option values', () => {
    // "-dev" is the value of --alias, "--rpc" the value of --api-key.
    const scan = scanClusterArgs(
      ['--alias', '-dev', '--api-key', '--rpc', '-m', '/m.gguf', '--rpc', '${RPC_SERVERS}'],
      {},
    );
    expect(scan.deviceInArgs).toBe(false);
    expect(scan.rpcArgCount).toBe(1);
    expect(scan.rpcArgIndex).toBe(6);
    expect(scan.rpcValueHasPlaceholder).toBe(true);
    // A boolean option takes no value: the next element is an option again.
    const after = scanClusterArgs(['--jinja', '-dev', '${DEVICES}'], {});
    expect(after.deviceInArgs).toBe(true);
    expect(after.deviceList).toBe('${DEVICES}');
  });

  it('flags the = form only on the scanned options', () => {
    for (const args of [
      ['--rpc=${RPC_SERVERS}'],
      ['--device=${DEVICES}'],
      ['-dev=${DEVICES}'],
      ['--tensor-split=1,1'],
      ['-mmdev=CUDA0'],
    ]) {
      expect(scanClusterArgs(args, {}).equalsForm).toBe(true);
    }
    expect(scanClusterArgs(['--alias=x', '--rpc', '${RPC_SERVERS}'], {}).equalsForm).toBe(false);
  });

  it('knows the value-taking options of the pinned llama-server', () => {
    const want: Record<string, number> = {
      '--rpc': 1,
      '--device': 1,
      '-dev': 1,
      '-ts': 1,
      '-mmdev': 1,
      '--alias': 1,
      '-m': 1,
      '--control-vector-layer-range': 2,
      '--jinja': 0,
      '-h': 0,
    };
    for (const [flag, count] of Object.entries(want)) {
      expect(llamaServerValueCount(flag), flag).toBe(count);
    }
  });
});

describe('deviceListStartsWithRPC', () => {
  it.each<[string, boolean, boolean]>([
    ['${DEVICES}', true, true],
    ['${RPC_DEVICES},CUDA0', true, true],
    ['RPC0,RPC1,CUDA0', true, true],
    ['CUDA0,${RPC_DEVICES}', true, false],
    ['${FIRST_LOCAL_DEVICE},${RPC_DEVICES}', true, false],
    // Without GPU rows ${FIRST_LOCAL_DEVICE} falls back to RPC0.
    ['${FIRST_LOCAL_DEVICE},${RPC_DEVICES}', false, true],
  ])('%s (head GPUs %s) -> %s', (list, headHasGPUs, want) => {
    expect(deviceListStartsWithRPC({ ...none, deviceList: list }, headHasGPUs)).toBe(want);
  });
});

describe('clusterHeadDocumentError', () => {
  const ok: ClusterHeadDocumentFacts = {
    type: 'llama_cpp',
    memberCount: 1,
    replicable: false,
    startupTimeoutSeconds: 600,
  };
  const check = (
    args: string[],
    env: Record<string, string> = {},
    facts: Partial<ClusterHeadDocumentFacts> = {},
  ) => clusterHeadDocumentError(scanClusterArgs(args, env), { ...ok, ...facts });

  it('accepts the documented shapes', () => {
    expect(check(['--rpc', '${RPC_SERVERS}'])).toBeNull();
    expect(check(['--rpc', '${RPC_SERVERS}', '--device', '${DEVICES}'])).toBeNull();
    expect(
      check([], { LLAMA_ARG_RPC: '${RPC_SERVERS}', LLAMA_ARG_DEVICE: '${DEVICES}' }),
    ).toBeNull();
    expect(
      check(['--device', '${RPC_DEVICES},CUDA0'], { LLAMA_ARG_RPC: '${RPC_SERVERS}' }),
    ).toBeNull();
    // Auto type is the gateway's to resolve.
    expect(check(['--rpc', '${RPC_SERVERS}'], {}, { type: '' })).toBeNull();
  });

  it.each<[string, string[], Record<string, string>, Partial<ClusterHeadDocumentFacts>, string]>([
    ['another type', ['--rpc', '${RPC_SERVERS}'], {}, { type: 'vllm' }, 'errorClusterTypeInvalid'],
    ['= form', ['--rpc=${RPC_SERVERS}'], {}, {}, 'errorClusterFlagEqualsForm'],
    ['no placeholder', ['--rpc', '10.0.0.1:1'], {}, {}, 'errorClusterRpcPlaceholderMissing'],
    [
      'placeholder outside --rpc',
      ['--alias', '${RPC_SERVERS}'],
      {},
      {},
      'errorClusterRpcPlaceholderMissing',
    ],
    [
      'args and env',
      ['--rpc', '${RPC_SERVERS}'],
      { LLAMA_ARG_RPC: '${RPC_SERVERS}' },
      {},
      'errorClusterRpcTwice',
    ],
    ['two --rpc', ['--rpc', '${RPC_SERVERS}', '--rpc', 'x'], {}, {}, 'errorClusterRpcTwice'],
    [
      'device list without RPC devices',
      ['--rpc', '${RPC_SERVERS}', '--device', 'CUDA0'],
      {},
      {},
      'errorClusterDeviceListMissingRpc',
    ],
    [
      'device before rpc in args',
      ['--device', '${DEVICES}', '--rpc', '${RPC_SERVERS}'],
      {},
      {},
      'errorClusterDeviceOrder',
    ],
    [
      'device in env, rpc in args',
      ['--rpc', '${RPC_SERVERS}'],
      { LLAMA_ARG_DEVICE: '${DEVICES}' },
      {},
      'errorClusterDeviceOrder',
    ],
    [
      'no members',
      ['--rpc', '${RPC_SERVERS}'],
      {},
      { memberCount: 0 },
      'errorClusterMembersRequired',
    ],
    [
      'replicable',
      ['--rpc', '${RPC_SERVERS}'],
      {},
      { replicable: true },
      'errorClusterHeadReplicable',
    ],
    [
      'no startup timeout',
      ['--rpc', '${RPC_SERVERS}'],
      {},
      { startupTimeoutSeconds: 0 },
      'errorClusterStartupTimeoutRequired',
    ],
  ])('%s', (_name, args, env, facts, want) => {
    expect(check(args, env, facts)).toBe(want);
  });
});
