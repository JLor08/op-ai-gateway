// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

/**
 * Turns the JSON run config `strata launch` writes into the runtime spec's
 * config file (`strata.json`) plus the spec fields it proposes. Pure; the
 * stored file is a normal, editable config file afterwards.
 */
export type StrataImport =
  | {
      ok: true;
      fileContent: string;
      gpuOrder: number[];
      startupTimeoutSeconds: number;
      upstreamModel?: string;
      warnings: ('layer_split_positional' | 'api_key_dropped')[];
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

export function importStrataRunConfig(text: string): StrataImport {
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

  const warnings: ('layer_split_positional' | 'api_key_dropped')[] = [];
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
  out.model_name = '${MODEL}';
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
    gpuOrder: gpuOrderOf(src),
    startupTimeoutSeconds,
    ...(upstreamModel === undefined ? {} : { upstreamModel }),
    warnings,
  };
}
