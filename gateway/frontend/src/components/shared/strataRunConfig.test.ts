// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import fixtureText from './__fixtures__/strata-launch-q8_0.json?raw';
import { importStrataRunConfig, type StrataImport } from './strataRunConfig';

const fixture = JSON.parse(fixtureText) as Record<string, unknown>;

function run(patch: (o: Record<string, unknown>) => void): StrataImport {
  const o = structuredClone(fixture);
  patch(o);
  return importStrataRunConfig(JSON.stringify(o));
}

function ok(r: StrataImport) {
  if (!r.ok) throw new Error(`import failed: ${r.error}`);
  return r;
}

describe('importStrataRunConfig', () => {
  it('drops the bookkeeping keys and keeps the rest', () => {
    const r = ok(run((o) => (o.future_x = { a: 1 })));
    const file = JSON.parse(r.fileContent) as Record<string, unknown>;
    for (const k of [
      'args_written',
      'port',
      'host',
      'open_browser',
      'gpus_asked',
      'gpu',
      'gpu_roles',
      'gpu_cards',
      'api_key',
    ]) {
      expect(file).not.toHaveProperty(k);
    }
    expect(Object.keys(file).filter((k) => k.startsWith('ui_'))).toEqual([]);
    for (const k of [
      'exe',
      'args',
      'cwd',
      'tokenizer',
      'log',
      'lib_dirs',
      'vision',
      'layer_split',
      'vram_reserve_mib',
      'engine_ready_s',
      'future_x',
    ]) {
      expect(file).toHaveProperty(k);
    }
    expect(file.engine_ready_s).toBe(1845);
    expect(file.args).toEqual(fixture.args);
  });

  it('sets model_name to ${MODEL}, gpu_order to as_given, and pretty-prints', () => {
    const r = ok(importStrataRunConfig(fixtureText));
    const file = JSON.parse(r.fileContent) as Record<string, unknown>;
    expect(file.model_name).toBe('${MODEL}');
    expect(file.gpu_order).toBe('as_given');
    expect(r.fileContent).toBe(JSON.stringify(file, null, 2));
    expect(r.upstreamModel).toBe('qwen3.8-flash-next-unsloth-q8_0');
    expect(r.warnings).toEqual([]);
  });

  it('derives the GPU order from gpu_roles (main first)', () => {
    expect(ok(importStrataRunConfig(fixtureText)).gpuOrder).toEqual([2, 0, 1]);
  });

  it('derives the GPU order from a legacy gpu list without roles', () => {
    const r = ok(
      run((o) => {
        delete o.gpu_roles;
      }),
    );
    expect(r.gpuOrder).toEqual([2, 0, 1]);
  });

  it('accepts a single scalar gpu', () => {
    const r = ok(
      run((o) => {
        delete o.gpu_roles;
        o.gpu = 1;
      }),
    );
    expect(r.gpuOrder).toEqual([1]);
  });

  it('proposes max(engine_ready_s, 900) + 60 as the startup timeout', () => {
    expect(ok(importStrataRunConfig(fixtureText)).startupTimeoutSeconds).toBe(1905);
    expect(ok(run((o) => delete o.engine_ready_s)).startupTimeoutSeconds).toBe(960);
    expect(ok(run((o) => (o.engine_ready_s = 300))).startupTimeoutSeconds).toBe(960);
  });

  it('drops an api_key with a warning', () => {
    const r = ok(run((o) => (o.api_key = 'x')));
    expect(r.fileContent).not.toContain('api_key');
    expect(r.warnings).toContain('api_key_dropped');
  });

  it('drops an api_key at any depth, in any case and of any type, with a warning', () => {
    const nested = ok(
      run((o) => {
        o.vision = { ...(o.vision as object), api_key: 'secret-nested' };
        o.extra = [{ Api_Key: 12345 }];
      }),
    );
    expect(nested.fileContent.toLowerCase()).not.toContain('api_key');
    expect(nested.fileContent).not.toContain('secret-nested');
    expect(nested.fileContent).not.toContain('12345');
    expect(nested.warnings).toContain('api_key_dropped');
    expect(JSON.parse(nested.fileContent)).toHaveProperty('vision');

    const numeric = ok(run((o) => (o.API_KEY = 42)));
    expect(numeric.fileContent.toLowerCase()).not.toContain('api_key');
    expect(numeric.warnings).toContain('api_key_dropped');

    expect(ok(importStrataRunConfig(fixtureText)).warnings).not.toContain('api_key_dropped');
  });

  it('warns on a positional layer_split', () => {
    const r = ok(run((o) => (o.layer_split = '24')));
    expect(r.warnings).toContain('layer_split_positional');
    expect(ok(importStrataRunConfig(fixtureText)).warnings).not.toContain('layer_split_positional');
  });

  it('rejects bad input', () => {
    expect(importStrataRunConfig('{nope')).toEqual({ ok: false, error: 'invalid_json' });
    expect(importStrataRunConfig('[1]')).toEqual({ ok: false, error: 'invalid_json' });
    expect(run((o) => delete o.exe)).toEqual({ ok: false, error: 'missing_exe' });
    expect(run((o) => (o.args = 'x'))).toEqual({ ok: false, error: 'missing_args' });
  });

  it('keeps no real user name in the fixture', () => {
    expect(fixtureText).not.toMatch(/Users\\\\(?!<user>)/);
  });
});
