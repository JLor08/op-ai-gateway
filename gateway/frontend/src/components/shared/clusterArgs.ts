// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// The portal's mirror of the gateway's cluster head save checks on a spec's
// args and env (gateway/backend/internal/portal/cluster_args.go and
// validateClusterHeadDocument in service_cluster.go). The form refuses what the
// gateway would refuse, in the gateway's order, so the operator reads the
// reason before the round trip; the gateway stays the authority.
//
// llama-server parses every environment variable first and then argv left to
// right, and --device resolves its names while it is parsed: RPC<n> only
// exists once --rpc has been handled. That is why where each option is set
// matters, not only whether it is set.

import type { MessageKey } from './types';

export const placeholderRPCServers = '${RPC_SERVERS}';
export const placeholderRPCDevices = '${RPC_DEVICES}';
export const placeholderDevices = '${DEVICES}';
export const placeholderFirstLocal = '${FIRST_LOCAL_DEVICE}';

const envLlamaArgRPC = 'LLAMA_ARG_RPC';
const envLlamaArgDevice = 'LLAMA_ARG_DEVICE';
const envLlamaArgTensorSplit = 'LLAMA_ARG_TENSOR_SPLIT';
const envMtmdBackendDevice = 'MTMD_BACKEND_DEVICE';

// llama-server's own spellings: --rpc only has a long form, --device also
// answers to -dev, --tensor-split to -ts and --mmproj-device to -mmdev.
const rpcFlags = ['--rpc'];
const deviceFlags = ['--device', '-dev'];
const tensorSplitFlags = ['--tensor-split', '-ts'];
const mmdevFlags = ['--mmproj-device', '-mmdev'];

// Every llama-server option that takes one value at the pinned llama.cpp build
// (2bc46baa9207d098d03741ac394e7faaf1a955b6), copied from the gateway's
// llamaServerValueFlags; --control-vector-layer-range takes two. The scan steps
// over an option's value, so "--alias -dev" is one option with its value, not a
// device flag.
//
// This is a second copy of gateway/backend/internal/portal/llama_flags.go.
// When the pinned llama.cpp build moves, regenerate that table from the new
// build's common/arg.cpp and then copy it here; a copy left behind makes the
// portal's pre-check read args differently from the gateway's save check.
const oneValueFlags = new Set(
  `
  --adaptive-decay --adaptive-target --alias --api-key --api-key-file --api-prefix --batch-size
  --cache-ram --cache-reuse --cache-type-k --cache-type-k-draft --cache-type-v
  --cache-type-v-draft --chat-template --chat-template-file --chat-template-kwargs
  --checkpoint-min-step --connect-code --control-vector --control-vector-scaled --cors-headers
  --cors-methods --cors-origins --cpu-mask --cpu-mask-batch --cpu-mask-batch-draft
  --cpu-mask-draft --cpu-range --cpu-range-batch --cpu-range-draft --cpu-strict
  --cpu-strict-batch --cpu-strict-batch-draft --cpu-strict-draft --ctx-checkpoints --ctx-size
  --defrag-thold --device --device-draft --docker-repo --draft --draft-max --draft-min --draft-n
  --draft-n-min --draft-p-min --draft-p-split --dry-allowed-length --dry-base --dry-multiplier
  --dry-penalty-last-n --dry-sequence-breaker --dynatemp-exp --dynatemp-range --embd-normalize
  --fit --fit-ctx --fit-target --flash-attn --frequency-penalty --gpu-layers --gpu-layers-draft
  --grammar --grammar-file --hf-file --hf-repo --hf-repo-draft --hf-token --host
  --image-max-tokens --image-min-tokens --json-schema --json-schema-file --keep
  --kv-unified-per-slot --lazy-mode --load-mode --log-colors --log-file --log-prompts-dir
  --log-verbosity --logit-bias --lookup-cache-dynamic --lookup-cache-static --lora --lora-scaled
  --main-gpu --mcp-servers-config --mcp-servers-json --media-path --min-p --mirostat
  --mirostat-ent --mirostat-lr --mmproj --mmproj-device --mmproj-url --model --model-draft
  --model-url --models-dir --models-max --models-memory-margin --models-preset --moe-cache-mib
  --mtmd-batch-max-tokens --n-cpu-ffn --n-cpu-moe --n-cpu-moe-draft --n-gpu-layers
  --n-gpu-layers-draft --n-predict --numa --override-kv --override-tensor --override-tensor-draft
  --parallel --path --poll --poll-batch --poll-batch-draft --poll-draft --pooling --port
  --predict --presence-penalty --prio --prio-batch --prio-batch-draft --prio-draft --reasoning
  --reasoning-budget --reasoning-budget-message --reasoning-effort --reasoning-format
  --repeat-last-n --repeat-penalty --reverse-prompt --rope-freq-base --rope-freq-scale
  --rope-scale --rope-scaling --rpc --sampler-seq --samplers --sampling-seq --seed
  --sleep-idle-seconds --slot-prompt-similarity --slot-save-path --spec-draft-cpu-mask
  --spec-draft-cpu-mask-batch --spec-draft-cpu-range --spec-draft-cpu-strict
  --spec-draft-cpu-strict-batch --spec-draft-device --spec-draft-hf --spec-draft-model
  --spec-draft-n-cpu-moe --spec-draft-n-max --spec-draft-n-min --spec-draft-n-ubatch
  --spec-draft-ncmoe --spec-draft-ngl --spec-draft-override-tensor --spec-draft-p-min
  --spec-draft-p-split --spec-draft-poll --spec-draft-poll-batch --spec-draft-prio
  --spec-draft-prio-batch --spec-draft-sampling --spec-draft-threads --spec-draft-threads-batch
  --spec-draft-type-k --spec-draft-type-v --spec-ngram-map-k-min-hits --spec-ngram-map-k-size-m
  --spec-ngram-map-k-size-n --spec-ngram-map-k4v-min-hits --spec-ngram-map-k4v-size-m
  --spec-ngram-map-k4v-size-n --spec-ngram-min-hits --spec-ngram-mod-n-match
  --spec-ngram-mod-n-max --spec-ngram-mod-n-min --spec-ngram-simple-min-hits
  --spec-ngram-simple-size-m --spec-ngram-simple-size-n --spec-ngram-size-m --spec-ngram-size-n
  --spec-synth-len --spec-synth-rates --spec-type --split-mode --sse-ping-interval
  --ssl-cert-file --ssl-key-file --swa-checkpoints --tags --temp --temperature --tensor-split
  --threads --threads-batch --threads-batch-draft --threads-draft --threads-http --timeout
  --tools --tools-runtime --top-k --top-n-sigma --top-nsigma --top-p --typical --typical-p
  --ubatch-size --ui-config --ui-config-file --verbosity --video-ffmpeg-dir --video-fps
  --video-timestamp-interval --webui-config --webui-config-file --xtc-probability --xtc-threshold
  --yarn-attn-factor --yarn-beta-fast --yarn-beta-slow --yarn-ext-factor --yarn-orig-ctx -C -Cb
  -Cbd -Cd -Cr -Crb -Crd -a -b -c -cms -cram -ctk -ctkd -ctv -ctvd -ctxcp -dev -devd -dr -dt -fa
  -fit -fitc -fitt -hf -hfd -hff -hfr -hfrd -hft -j -jf -l -lcd -lcs -lm -lv -lzm -m -md -mg -mm
  -mmdev -mmu -mu -n -ncffn -ncmoe -ncmoed -ngl -ngld -np -ot -otd -r -rea -s -sm -sps -t -tb
  -tbd -td -to -ts -ub dedup-cache-models load-on-startup stop-timeout
`
    .trim()
    .split(/\s+/),
);
const twoValueFlags = new Set(['--control-vector-layer-range']);

export function llamaServerValueCount(name: string): number {
  if (twoValueFlags.has(name)) return 2;
  return oneValueFlags.has(name) ? 1 : 0;
}

export interface ClusterArgScan {
  // --rpc in args / LLAMA_ARG_RPC in env.
  rpcInArgs: boolean;
  rpcInEnv: boolean;
  // Index of the first --rpc in args (-1 when absent); every --rpc counted.
  rpcArgIndex: number;
  rpcArgCount: number;
  // The value of --rpc or LLAMA_ARG_RPC carries ${RPC_SERVERS}.
  rpcValueHasPlaceholder: boolean;
  // ${RPC_SERVERS} occurrences across every arg and env value.
  rpcPlaceholderCount: number;
  // --device/-dev in args, LLAMA_ARG_DEVICE in env; the first one in args.
  deviceInArgs: boolean;
  deviceInEnv: boolean;
  deviceArgIndex: number;
  // The device list llama-server ends up with: args when given, else env.
  deviceList: string;
  // deviceList names the RPC devices (${DEVICES} or ${RPC_DEVICES}).
  deviceHasRPC: boolean;
  tensorSplit: boolean;
  mmdev: boolean;
  // ${RPC_SERVERS} or ${RPC_DEVICES} appears anywhere.
  clusterPlaceholder: boolean;
  // A scanned option written as "--flag=value", which llama-server refuses.
  equalsForm: boolean;
}

// The option name of one argv element and the value attached with "=".
// llama-server treats "_" and "-" alike in long options.
function normalizeFlag(arg: string): { name: string; value: string; attached: boolean } {
  const a = arg.trim();
  if (!a.startsWith('-')) return { name: a, value: '', attached: false };
  const eq = a.indexOf('=');
  let name = eq < 0 ? a : a.slice(0, eq);
  const value = eq < 0 ? '' : a.slice(eq + 1);
  if (name.startsWith('--')) name = name.replaceAll('_', '-');
  return { name, value, attached: eq >= 0 };
}

function countOf(text: string, token: string): number {
  return text.split(token).length - 1;
}

export function scanClusterArgs(args: string[], env: Record<string, string>): ClusterArgScan {
  const scan: ClusterArgScan = {
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
  for (const v of [...args, ...Object.values(env)]) {
    scan.rpcPlaceholderCount += countOf(v, placeholderRPCServers);
    if (v.includes(placeholderRPCServers) || v.includes(placeholderRPCDevices)) {
      scan.clusterPlaceholder = true;
    }
  }
  let argDeviceList = '';
  for (let i = 0; i < args.length; i++) {
    const { name, value, attached } = normalizeFlag(args[i]);
    const flagValue = attached ? value : (args[i + 1] ?? '');
    let scanned = true;
    if (rpcFlags.includes(name)) {
      scan.rpcInArgs = true;
      scan.rpcArgCount++;
      if (scan.rpcArgIndex < 0) scan.rpcArgIndex = i;
      if (flagValue.includes(placeholderRPCServers)) scan.rpcValueHasPlaceholder = true;
    } else if (deviceFlags.includes(name)) {
      if (!scan.deviceInArgs) scan.deviceArgIndex = i;
      scan.deviceInArgs = true;
      argDeviceList = flagValue;
    } else if (tensorSplitFlags.includes(name)) {
      scan.tensorSplit = true;
    } else if (mmdevFlags.includes(name)) {
      scan.mmdev = true;
    } else {
      scanned = false;
    }
    if (scanned && attached) scan.equalsForm = true;
    if (!attached) i += llamaServerValueCount(name);
  }
  // llama-server reads its variables by their exact upper-case names.
  if (envLlamaArgRPC in env) {
    scan.rpcInEnv = true;
    if (env[envLlamaArgRPC].includes(placeholderRPCServers)) scan.rpcValueHasPlaceholder = true;
  }
  scan.deviceInEnv = envLlamaArgDevice in env;
  if (envLlamaArgTensorSplit in env) scan.tensorSplit = true;
  if (envMtmdBackendDevice in env) scan.mmdev = true;
  if (scan.deviceInArgs) scan.deviceList = argDeviceList;
  else if (scan.deviceInEnv) scan.deviceList = env[envLlamaArgDevice];
  scan.deviceHasRPC =
    scan.deviceList.includes(placeholderDevices) || scan.deviceList.includes(placeholderRPCDevices);
  return scan;
}

export function hasDeviceList(scan: ClusterArgScan): boolean {
  return scan.deviceInArgs || scan.deviceInEnv;
}

// Whether the device list's first entry is an RPC device: ${DEVICES} renders
// the RPC devices first on a head, ${RPC_DEVICES} and a literal RPC<n> are RPC
// devices; on a head without GPU rows ${FIRST_LOCAL_DEVICE} falls back to RPC0.
export function deviceListStartsWithRPC(scan: ClusterArgScan, headHasGPUs: boolean): boolean {
  const first = scan.deviceList.split(',')[0].trim();
  if (!headHasGPUs && first.startsWith(placeholderFirstLocal)) return true;
  return (
    first.startsWith(placeholderDevices) ||
    first.startsWith(placeholderRPCDevices) ||
    first.startsWith('RPC')
  );
}

// The facts besides args and env the head document checks read.
export interface ClusterHeadDocumentFacts {
  // The explicit spec type; '' (auto) is the gateway's to resolve.
  type: string;
  memberCount: number;
  replicable: boolean;
  startupTimeoutSeconds: number;
}

// The first refusal the gateway's head document checks would answer, as the
// message key of its error code, in the gateway's order; null when none.
export function clusterHeadDocumentError(
  scan: ClusterArgScan,
  facts: ClusterHeadDocumentFacts,
): MessageKey | null {
  if (facts.type !== '' && facts.type !== 'llama_cpp') return 'errorClusterTypeInvalid';
  if (scan.equalsForm) return 'errorClusterFlagEqualsForm';
  if (scan.rpcPlaceholderCount === 0 || !scan.rpcValueHasPlaceholder) {
    return 'errorClusterRpcPlaceholderMissing';
  }
  // --rpc in env and argv registers every server twice, as does a second --rpc.
  if (scan.rpcPlaceholderCount > 1 || scan.rpcArgCount > 1 || (scan.rpcInArgs && scan.rpcInEnv)) {
    return 'errorClusterRpcTwice';
  }
  if (hasDeviceList(scan) && !scan.deviceHasRPC) return 'errorClusterDeviceListMissingRpc';
  if (
    (scan.deviceInArgs &&
      scan.rpcInArgs &&
      !scan.rpcInEnv &&
      scan.deviceArgIndex < scan.rpcArgIndex) ||
    (scan.deviceInEnv && scan.rpcInArgs)
  ) {
    return 'errorClusterDeviceOrder';
  }
  if (facts.memberCount === 0) return 'errorClusterMembersRequired';
  if (facts.replicable) return 'errorClusterHeadReplicable';
  // A cold cluster start waits for every member and then the head, so the
  // head's own bound is the operator's to set.
  if (facts.startupTimeoutSeconds <= 0) return 'errorClusterStartupTimeoutRequired';
  return null;
}
