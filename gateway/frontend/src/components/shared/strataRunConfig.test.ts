// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import fixtureText from './__fixtures__/strata-launch-q8_0.json?raw';
import { logFileIssues } from './logFiles';
import { importStrataRunConfig, type StrataImport } from './strataRunConfig';

// The agent declares runtime_log_files: the import points `log` into
// ${CONFIG_DIR} and declares it (#430).
const declared = { declareEngineLog: true };

const fixture = JSON.parse(fixtureText) as Record<string, unknown>;

function run(patch: (o: Record<string, unknown>) => void): StrataImport {
  const o = structuredClone(fixture);
  patch(o);
  return importStrataRunConfig(JSON.stringify(o), declared);
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

  it('rewrites the log key to the per-instance engine log and declares it', () => {
    const r = ok(importStrataRunConfig(fixtureText, declared));
    const file = JSON.parse(r.fileContent) as Record<string, unknown>;
    expect(file.log).toBe('${CONFIG_DIR}/engine.log');
    expect(r.fileContent).not.toContain('q8_0-256k.engine.log');
    expect(r.logFiles).toEqual([{ name: 'engine', path: '${CONFIG_DIR}/engine.log' }]);
    expect(r.warnings).not.toContain('log_files_capped');
  });

  // An agent older than 0.26.0 (or one whose report has not settled) does not
  // declare runtime_log_files: it writes ${CONFIG_DIR} in config-file content
  // in its native form, a C:\... path that is invalid JSON on Windows. The
  // import then keeps the source's log key, declares no log file, and says why.
  it('keeps the log key and declares nothing when the agent lacks runtime_log_files', () => {
    const r = ok(importStrataRunConfig(fixtureText, { declareEngineLog: false }));
    const file = JSON.parse(r.fileContent) as Record<string, unknown>;
    expect(file.log).toBe(fixture.log);
    expect(r.fileContent).not.toContain('${CONFIG_DIR}');
    expect(r.logFiles).toEqual([]);
    expect(r.warnings).toEqual(['engine_log_needs_agent']);
  });

  it('declares nothing and never caps for replicas when the agent lacks the feature', () => {
    const o = structuredClone(fixture);
    o.replicas = 6;
    const r = ok(importStrataRunConfig(JSON.stringify(o), { declareEngineLog: false }));
    expect(r.logFiles).toEqual([]);
    expect(r.warnings).not.toContain('log_files_capped');
    expect(r.warnings).toContain('engine_log_needs_agent');
  });

  it('declares the log even when the source named none', () => {
    const r = ok(run((o) => delete o.log));
    expect((JSON.parse(r.fileContent) as Record<string, unknown>).log).toBe(
      '${CONFIG_DIR}/engine.log',
    );
    expect(r.logFiles).toEqual([{ name: 'engine', path: '${CONFIG_DIR}/engine.log' }]);
  });

  it('declares one file per replica for a replica count, keeping log as is', () => {
    const r = ok(run((o) => (o.replicas = 3)));
    expect((JSON.parse(r.fileContent) as Record<string, unknown>).log).toBe(
      '${CONFIG_DIR}/engine.log',
    );
    expect(r.logFiles).toEqual([
      { name: 'engine-r0', path: '${CONFIG_DIR}/engine.r0.log' },
      { name: 'engine-r1', path: '${CONFIG_DIR}/engine.r1.log' },
      { name: 'engine-r2', path: '${CONFIG_DIR}/engine.r2.log' },
    ]);
    expect(r.warnings).not.toContain('log_files_capped');
  });

  it('declares one file per replica for a replica list', () => {
    const r = ok(run((o) => (o.replicas = [{ gpu: [0] }, { gpu: [1] }])));
    expect(r.logFiles.map((f) => f.name)).toEqual(['engine-r0', 'engine-r1']);
  });

  it('caps at 4 replica files and warns beyond that', () => {
    const r = ok(run((o) => (o.replicas = 6)));
    expect(r.logFiles.map((f) => f.name)).toEqual([
      'engine-r0',
      'engine-r1',
      'engine-r2',
      'engine-r3',
    ]);
    expect(r.warnings).toContain('log_files_capped');
    const four = ok(run((o) => (o.replicas = 4)));
    expect(four.warnings).not.toContain('log_files_capped');
  });

  it('treats one replica, or a non-count value, as the plain case', () => {
    for (const v of [1, [{}], 0, -2, 2.5, 'x', null, []]) {
      const r = ok(run((o) => (o.replicas = v)));
      expect(r.logFiles).toEqual([{ name: 'engine', path: '${CONFIG_DIR}/engine.log' }]);
    }
  });

  it('produces names and paths the log-file validation accepts', () => {
    const r = ok(run((o) => (o.replicas = 6)));
    expect(logFileIssues(r.logFiles, ['strata.json'])).toEqual(r.logFiles.map(() => null));
  });

  it('sets model_name to ${MODEL}, gpu_order to as_given, and pretty-prints', () => {
    const r = ok(importStrataRunConfig(fixtureText, declared));
    const file = JSON.parse(r.fileContent) as Record<string, unknown>;
    expect(file.model_name).toBe('${MODEL}');
    expect(file.gpu_order).toBe('as_given');
    // strata serve has no --gpu flag: the main-last card list lives in the file.
    expect(file.gpu).toBe('${HOST_GPU_IDS_MAIN_LAST}');
    expect(r.fileContent).toBe(JSON.stringify(file, null, 2));
    expect(r.upstreamModel).toBe('qwen3.8-flash-next-unsloth-q8_0');
    expect(r.warnings).toEqual([]);
  });

  it('derives the GPU order from gpu_roles (main first)', () => {
    expect(ok(importStrataRunConfig(fixtureText, declared)).gpuOrder).toEqual([2, 0, 1]);
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

  it('replaces the source card list with the main-last placeholder', () => {
    const r = ok(run(() => {}));
    expect(JSON.parse(r.fileContent).gpu).toBe('${HOST_GPU_IDS_MAIN_LAST}');
    expect(r.fileContent).not.toContain('"gpu": [');
  });

  it('writes no gpu key when the source names no cards', () => {
    const r = ok(
      run((o) => {
        delete o.gpu_roles;
        delete o.gpu;
      }),
    );
    expect(r.gpuOrder).toEqual([]);
    expect(JSON.parse(r.fileContent)).not.toHaveProperty('gpu');
  });

  it('proposes max(engine_ready_s, 900) + 60 as the startup timeout', () => {
    expect(ok(importStrataRunConfig(fixtureText, declared)).startupTimeoutSeconds).toBe(1905);
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

    expect(ok(importStrataRunConfig(fixtureText, declared)).warnings).not.toContain(
      'api_key_dropped',
    );
  });

  it('warns on a positional layer_split', () => {
    const r = ok(run((o) => (o.layer_split = '24')));
    expect(r.warnings).toContain('layer_split_positional');
    expect(ok(importStrataRunConfig(fixtureText, declared)).warnings).not.toContain(
      'layer_split_positional',
    );
  });

  it('rejects bad input', () => {
    expect(importStrataRunConfig('{nope', declared)).toEqual({ ok: false, error: 'invalid_json' });
    expect(importStrataRunConfig('[1]', declared)).toEqual({ ok: false, error: 'invalid_json' });
    expect(run((o) => delete o.exe)).toEqual({ ok: false, error: 'missing_exe' });
    expect(run((o) => (o.args = 'x'))).toEqual({ ok: false, error: 'missing_args' });
  });

  it('keeps no real user name in the fixture', () => {
    expect(fixtureText).not.toMatch(/Users\\\\(?!<user>)/);
  });
});
