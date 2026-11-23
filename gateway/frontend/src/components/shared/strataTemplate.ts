// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { EndpointMode } from '../../api';

/**
 * The launch-spec form values a kind's template fills in. Only the fields a
 * template sets: scheduling (pinned, idle and admission timeouts) stays at the
 * form's stock create defaults, as for every other kind.
 */
export type RuntimeSpecDraft = {
  // Shown as the binary field's placeholder, never written into it: the agent
  // needs the absolute path, and %LOCALAPPDATA% is per user.
  binaryHint: string;
  args: string[];
  env: Record<string, string>;
  setVisibleDevices: boolean;
  visibleDevicesMode: 'env' | 'args';
  startupTimeoutSeconds: number;
  healthPath: string;
  flavors: string[];
  responsesMode: EndpointMode;
  messagesMode: EndpointMode;
  // Windows job-object process tree: Strata's launcher (strata.cmd) spawns a
  // child tree, and only a job stops it as a unit.
  processTree: 'direct' | 'job';
};

/**
 * The Strata (`strata serve`) launch template the Type select fills into a new
 * spec.
 *
 * - `--host 127.0.0.1`: the agent reaches every child on loopback (its proxy
 *   and probes use http://127.0.0.1:<port>), and the other templates the type
 *   notes give (SGLang, sglang-omni, vllm-srun) bind there too.
 * - No `--gpu`: `strata serve` has no such flag (only the internal
 *   `serve/server.py` does). The card list lives in the config file as
 *   `"gpu": [${HOST_GPU_IDS_MAIN_LAST}]`, which the run-config import writes
 *   and the agent resolves. Strata takes host (nvidia-smi) card numbers with
 *   the main card last.
 * - Visible devices ON in args mode: the agent injects no
 *   CUDA_VISIBLE_DEVICES (which would renumber the cards and break the
 *   host-numbered list); the config's `gpu` list is what restricts Strata to
 *   the spec's cards, and the backend accepts that placeholder in a config
 *   file as args-mode enforcement.
 * - `STRATA_API_KEY=${API_TOKEN}`: the key travels in the environment, never in
 *   argv or the config file.
 * - 960 s startup: Strata waits up to 900 s for its engine by default, so the
 *   agent must not give up before Strata does.
 */
export function strataSpecTemplate(): RuntimeSpecDraft {
  return {
    binaryHint: String.raw`%LOCALAPPDATA%\Strata\terminal\current\bin\strata.cmd`,
    args: [
      'serve',
      '--config',
      '${CONFIG_DIR}/strata.json',
      '--host',
      '127.0.0.1',
      '--port',
      '${PORT}',
      '--output',
      'jsonl',
      '--shutdown-timeout',
      '3',
    ],
    env: { STRATA_API_KEY: '${API_TOKEN}' },
    setVisibleDevices: true,
    visibleDevicesMode: 'args',
    startupTimeoutSeconds: 960,
    healthPath: '/health',
    flavors: ['openai', 'anthropic'],
    responsesMode: 'passthrough',
    messagesMode: 'passthrough',
    processTree: 'job',
  };
}
