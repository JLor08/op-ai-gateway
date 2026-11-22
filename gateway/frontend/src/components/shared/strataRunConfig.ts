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
// here: it is Strata's own engine-wait ceiling and stays in the file.
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
  'api_key',
]);

// Strata waits up to 900 s for its engine by default; the agent must outlast
// it. The gateway only refuses a negative timeout (no upper bound), so the
// clamp is a floor at 1.
const STARTUP_BASE_SECONDS = 900;
const STARTUP_MARGIN_SECONDS = 60;

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
    const list = Array.isArray(gpu) ? gpu.filter(isIndex) : isIndex(gpu) ? [gpu] : [];
    // The engine's main card is the list's last entry; position 1 is main.
    order = list.length > 0 ? [list[list.length - 1], ...list.slice(0, -1)] : [];
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
  const out: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(src)) {
    if (DROPPED_KEYS.has(k) || k.startsWith('ui_')) continue;
    out[k] = v;
  }
  if (typeof src.api_key === 'string' && src.api_key !== '') warnings.push('api_key_dropped');
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
