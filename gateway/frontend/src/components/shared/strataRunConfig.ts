// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { LOG_FILES_MAX, type LogFile } from './logFiles';

/**
 * Turns the JSON run config `strata launch` writes into the runtime spec's
 * config file (`strata.json`) plus the spec fields it proposes. Pure; the
 * stored file is a normal, editable config file afterwards.
 */
export type StrataImportWarning =
  'layer_split_positional' | 'api_key_dropped' | 'log_files_capped' | 'engine_log_needs_agent';

export type StrataImportOptions = {
  // The server's agent declares runtime_log_files (agent 0.26.0, #430): it
  // writes ${CONFIG_DIR} with forward slashes inside config-file content on
  // Windows and tails declared log files. Only then is `log` pointed into
  // ${CONFIG_DIR} and declared. An older agent would write the native
  // C:\... form into the JSON string, which is invalid JSON, so the import
  // keeps the source's `log` and warns (engine_log_needs_agent). A report
  // that has not settled counts as not declared.
  declareEngineLog: boolean;
};

export type StrataImport =
  | {
      ok: true;
      fileContent: string;
      gpuOrder: number[];
      startupTimeoutSeconds: number;
      upstreamModel?: string;
      // The engine log(s) the rewritten `log` key produces, ready for the
      // spec's log_files (#430); empty when the agent does not declare
      // runtime_log_files (StrataImportOptions).
      logFiles: LogFile[];
      warnings: StrataImportWarning[];
    }
  | { ok: false; error: 'invalid_json' | 'missing_exe' | 'missing_args' };

// Launcher bookkeeping that must not reach the file. `engine_ready_s` is NOT
// here: it is Strata's own engine-wait ceiling and stays in the file. An
// `api_key` is dropped separately, at any depth (withoutApiKeys).
const DROPPED_KEYS = new Set([
  'args_written',
  'port',
  'host',
  'open_browser',
  'gpus_asked',
  'gpu',
  'gpu_roles',
  'gpu_cards',
  'gpu_order',
]);

// Strata waits up to 900 s for its engine by default; the agent must outlast
// it. The gateway only refuses a negative timeout (no upper bound), so the
// clamp is a floor at 1.
const STARTUP_BASE_SECONDS = 900;
const STARTUP_MARGIN_SECONDS = 60;

function isApiKeyName(k: string): boolean {
  return k.toLowerCase() === 'api_key';
}

// withoutApiKeys copies v with every key named api_key (any case) removed at
// any depth -- a run config may nest one, e.g. inside `vision` -- whatever its
// value, and counts the removals in dropped.count. A secret never reaches the
// stored config file (the spec's API token carries it instead).
function withoutApiKeys(v: unknown, dropped: { count: number }): unknown {
  if (Array.isArray(v)) return v.map((e) => withoutApiKeys(e, dropped));
  if (v === null || typeof v !== 'object') return v;
  const out: Record<string, unknown> = {};
  for (const [k, e] of Object.entries(v as Record<string, unknown>)) {
    if (isApiKeyName(k)) {
      dropped.count++;
      continue;
    }
    out[k] = withoutApiKeys(e, dropped);
  }
  return out;
}

// The per-instance engine log. The agent provisions ${CONFIG_DIR} per
// instance and, from 0.26.0 on, writes it with forward slashes inside
// config-file content on Windows, so the JSON stays valid
// (StrataImportOptions).
const CONFIG_DIR_PLACEHOLDER = '${CONFIG_DIR}';
const ENGINE_LOG = `${CONFIG_DIR_PLACEHOLDER}/engine.log`;

// replicaCount reads the run config's `replicas` the way Strata's replica_cfg
// does: a list is one entry per replica, a positive integer is a count.
// Anything else (absent, 0, 1, non-integer) is the plain single-engine case.
function replicaCount(v: unknown): number {
  if (Array.isArray(v)) return v.length;
  return typeof v === 'number' && Number.isInteger(v) && v > 0 ? v : 1;
}

// Strata suffixes the log path itself when there are replicas
// (serve/replicas.py _suffixed: engine.log -> engine.r<i>.log).
function engineLogFiles(replicas: unknown): { logFiles: LogFile[]; capped: boolean } {
  const n = replicaCount(replicas);
  if (n < 2) return { logFiles: [{ name: 'engine', path: ENGINE_LOG }], capped: false };
  const shown = Math.min(n, LOG_FILES_MAX);
  return {
    logFiles: Array.from({ length: shown }, (_, i) => ({
      name: `engine-r${i}`,
      path: `${CONFIG_DIR_PLACEHOLDER}/engine.r${i}.log`,
    })),
    capped: n > LOG_FILES_MAX,
  };
}

function isIndex(v: unknown): v is number {
  return typeof v === 'number' && Number.isInteger(v) && v >= 0;
}

function gpuOrderOf(o: Record<string, unknown>): number[] {
  const roles = o.gpu_roles;
  let order: number[] = [];
  if (roles !== null && typeof roles === 'object' && !Array.isArray(roles)) {
    const entries = Object.entries(roles as Record<string, unknown>)
      .map(([k, v]) => ({ idx: Number(k), role: v }))
      .filter((e) => /^\d+$/.test(String(e.idx)) && isIndex(e.idx));
    order = [
      ...entries.filter((e) => e.role === 'main').map((e) => e.idx),
      ...entries.filter((e) => e.role !== 'main').map((e) => e.idx),
    ];
  } else {
    const gpu = o.gpu;
    let list: number[] = [];
    if (Array.isArray(gpu)) list = gpu.filter(isIndex);
    else if (isIndex(gpu)) list = [gpu];
    // The engine's main card is the list's last entry; position 1 is main.
    order = list.length > 0 ? [list.at(-1) as number, ...list.slice(0, -1)] : [];
  }
  return [...new Set(order)];
}

export function importStrataRunConfig(text: string, options: StrataImportOptions): StrataImport {
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    return { ok: false, error: 'invalid_json' };
  }
  if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) {
    return { ok: false, error: 'invalid_json' };
  }
  const src = parsed as Record<string, unknown>;
  if (typeof src.exe !== 'string' || src.exe.trim() === '') {
    return { ok: false, error: 'missing_exe' };
  }
  if (!Array.isArray(src.args)) return { ok: false, error: 'missing_args' };

  const warnings: StrataImportWarning[] = [];
  const dropped = { count: 0 };
  const out = withoutApiKeys(src, dropped) as Record<string, unknown>;
  for (const k of Object.keys(out)) {
    if (DROPPED_KEYS.has(k) || k.startsWith('ui_')) delete out[k];
  }
  if (dropped.count > 0) warnings.push('api_key_dropped');
  const ls = src.layer_split;
  if (ls !== undefined && ls !== 'auto' && ls !== null && ls !== '') {
    warnings.push('layer_split_positional');
  }
  // The source's `log` names an absolute path on the machine that ran
  // `strata launch`, where the diagnostics are unreachable. Point it into the
  // per-instance config directory and declare it for the live log -- only for
  // an agent that can take ${CONFIG_DIR} in the content (StrataImportOptions);
  // otherwise `log` is kept as written.
  let logFiles: LogFile[] = [];
  if (options.declareEngineLog) {
    out.log = ENGINE_LOG;
    const engine = engineLogFiles(src.replicas);
    logFiles = engine.logFiles;
    if (engine.capped) warnings.push('log_files_capped');
  } else {
    warnings.push('engine_log_needs_agent');
  }
  out.model_name = '${MODEL}';
  // `strata serve` has no --gpu flag, so the card list goes into the file. A
  // comma-separated string is valid JSON before the agent expands the
  // placeholder, and Strata reads "0,1,2" like [0, 1, 2]
  // (serve/server_parts/engines.py _gpu_numbers). Only when the source named
  // cards: the placeholder is a hard error on a spec with no GPUs.
  const gpuOrder = gpuOrderOf(src);
  if (gpuOrder.length > 0) out.gpu = '${HOST_GPU_IDS_MAIN_LAST}';
  out.gpu_order = 'as_given';

  const ready = typeof src.engine_ready_s === 'number' ? src.engine_ready_s : 0;
  const startupTimeoutSeconds = Math.max(
    1,
    Math.round(Math.max(ready, STARTUP_BASE_SECONDS) + STARTUP_MARGIN_SECONDS),
  );
  const upstreamModel =
    typeof src.model_name === 'string' && src.model_name.trim() !== '' ? src.model_name : undefined;
  return {
    ok: true,
    fileContent: JSON.stringify(out, null, 2),
    gpuOrder,
    startupTimeoutSeconds,
    logFiles,
    ...(upstreamModel === undefined ? {} : { upstreamModel }),
    warnings,
  };
}
