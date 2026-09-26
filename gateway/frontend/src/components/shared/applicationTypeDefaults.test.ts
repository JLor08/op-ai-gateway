// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, it, expect } from 'vitest';
import {
  applicationTypeDefaults,
  migrateTypeFields,
  type TypeDefaults,
} from './applicationTypeDefaults';

describe('applicationTypeDefaults', () => {
  it('llama_swap has the loaded/probe/mode/port defaults', () => {
    expect(applicationTypeDefaults.llama_swap).toEqual({
      port: 8080,
      scheme: 'http',
      responsesMode: 'passthrough',
      messagesMode: 'passthrough',
      loadedModelsPath: '/running',
      loadedModelsFormat: 'llama_swap',
      contextProbePath: '/upstream/{model}/props',
      timeoutMs: 30000,
      healthCheckMode: 'health_path',
      apiFlavors: ['openai', 'anthropic'],
    });
  });

  it('ollama defaults both endpoint modes to passthrough, /api/ps auto, no probe', () => {
    expect(applicationTypeDefaults.ollama).toEqual({
      port: 11434,
      scheme: 'http',
      responsesMode: 'passthrough',
      messagesMode: 'passthrough',
      loadedModelsPath: '/api/ps',
      loadedModelsFormat: 'auto',
      contextProbePath: '',
      timeoutMs: 30000,
      healthCheckMode: 'health_path',
      apiFlavors: ['openai', 'anthropic'],
    });
  });

  // server_agent's timeout default is 600000 (10 minutes), not the usual
  // 30000: it becomes a TOTAL request deadline that must cover a cold model
  // load, and 30s would fail every first request reproducibly (see the
  // backend default in portal service_applications.go and
  // docs/architecture/cross-cutting/compatibility-and-inference.md §7.1).
  it('server_agent defaults llama-swap-shaped loaded models, passthrough modes, plus a 10-minute timeout', () => {
    expect(applicationTypeDefaults.server_agent).toEqual({
      port: 8081,
      scheme: 'http',
      responsesMode: 'passthrough',
      messagesMode: 'passthrough',
      loadedModelsPath: '/running',
      loadedModelsFormat: 'llama_swap',
      contextProbePath: '',
      timeoutMs: 600000,
      healthCheckMode: 'health_path',
      apiFlavors: ['openai', 'anthropic'],
    });
  });

  it("migrates every field when current equals the old type's defaults", () => {
    const current = { ...applicationTypeDefaults.ollama };
    const patch = migrateTypeFields('ollama', 'llama_swap', current);
    expect(patch).toEqual(applicationTypeDefaults.llama_swap);
  });

  it('preserves a customized field and migrates untouched ones', () => {
    const current: TypeDefaults = { ...applicationTypeDefaults.ollama, port: 9999 };
    const patch = migrateTypeFields('ollama', 'vllm', current);
    expect(patch.port).toBeUndefined(); // customized → kept
    expect(patch.loadedModelsPath).toBe('/v1/models'); // untouched → migrated
    expect(patch.loadedModelsFormat).toBe('openai');
  });

  // Both endpoint modes now default to passthrough for every type, so a switch
  // between types that both hold the default is a no-op for those two fields:
  // migrateTypeFields (unchanged, field-agnostic) still re-asserts the field
  // in the patch because current equals the old type's default — the same
  // mechanism that keeps e.g. `scheme` in the full-snapshot migrate test
  // above even though every type shares 'http' — so the meaningful assertion
  // is the resulting (merged) value, not whether the key is omitted.
  it('leaves an already-passthrough mode alone across a type switch', () => {
    const current = { ...applicationTypeDefaults.ollama };
    const patch = migrateTypeFields('ollama', 'vllm', current);
    const merged = { ...current, ...patch };
    expect(merged.responsesMode).toBe('passthrough');
    expect(merged.messagesMode).toBe('passthrough');
  });

  // A mode the operator moved off the shared default follows the general
  // contract: it is preserved (never clobbered back to passthrough).
  it('preserves a customized endpoint mode across a type switch', () => {
    const current: TypeDefaults = { ...applicationTypeDefaults.ollama, responsesMode: 'translate' };
    const patch = migrateTypeFields('ollama', 'vllm', current);
    expect(patch.responsesMode).toBeUndefined();
  });

  // The preservation contract (migrateTypeFields' whole reason to exist)
  // applies to timeoutMs exactly like every other field: a value still at
  // the OLD type's default follows the new type, but a value the operator
  // customized survives the switch untouched.
  it('migrates timeoutMs to the new type default when untouched', () => {
    const current = { ...applicationTypeDefaults.ollama };
    const patch = migrateTypeFields('ollama', 'server_agent', current);
    expect(patch.timeoutMs).toBe(600000);
  });

  it('preserves a customized timeoutMs across a type switch', () => {
    const current: TypeDefaults = { ...applicationTypeDefaults.ollama, timeoutMs: 45000 };
    const patch = migrateTypeFields('ollama', 'server_agent', current);
    expect(patch.timeoutMs).toBeUndefined(); // customized → kept, never clobbered to 600000
  });

  // apiFlavors is array-valued, and the live value migrateTypeFields is
  // called with (ApplicationSection's `flavors` state) is never the SAME
  // array instance as an applicationTypeDefaults entry -- it is rebuilt by
  // every checkbox toggle. A naive `current[key] === oldDefaults[key]` would
  // therefore always read "customized" for this one field, even right after
  // a fresh, untouched create, and this migration would silently never fire
  // for it. Build `current` the same way -- a fresh array holding the same
  // values, not the same reference -- to pin that this is fixed.
  it("migrates apiFlavors to the new type's default when current holds an equal-by-value, different-reference array", () => {
    const current: TypeDefaults = {
      ...applicationTypeDefaults.ollama,
      apiFlavors: [...applicationTypeDefaults.ollama.apiFlavors],
    };
    const patch = migrateTypeFields('ollama', 'stable_diffusion_cpp', current);
    expect(patch.apiFlavors).toEqual(['openai_images']);
  });

  // A positional comparison (current[i] === default[i]) is not enough:
  // apiFlavors is a SET of enabled flavors, not a sequence, and
  // ApiVariantControls' toggleFlavor removes a flavor with `filter` and
  // re-adds it with `[...list, flavor]` -- so an operator who unchecks then
  // rechecks the SAME flavor before switching type ends up with the same
  // set in a different order (['anthropic', 'openai'] instead of
  // ['openai', 'anthropic']). That must still read as "untouched" and
  // migrate -- the alternative is a silent, order-dependent failure to
  // switch away from a stock flavor pair, exactly what this field exists to
  // prevent. Every array literal elsewhere in this file happens to already
  // be in canonical order, which is why only a reordered array pins this.
  it('migrates apiFlavors to the new type default when current holds the same set in a different order', () => {
    const current: TypeDefaults = {
      ...applicationTypeDefaults.ollama,
      apiFlavors: ['anthropic', 'openai'],
    };
    const patch = migrateTypeFields('ollama', 'stable_diffusion_cpp', current);
    expect(patch.apiFlavors).toEqual(['openai_images']);
  });

  it('migrates healthCheckMode to the new type default when untouched', () => {
    const current = { ...applicationTypeDefaults.ollama };
    const patch = migrateTypeFields('ollama', 'stable_diffusion_cpp', current);
    expect(patch.healthCheckMode).toBe('model_sync');
  });

  it('preserves a customized apiFlavors and healthCheckMode across a type switch', () => {
    const current: TypeDefaults = {
      ...applicationTypeDefaults.ollama,
      apiFlavors: ['anthropic'],
      healthCheckMode: 'always_reachable',
    };
    const patch = migrateTypeFields('ollama', 'stable_diffusion_cpp', current);
    expect(patch.apiFlavors).toBeUndefined();
    expect(patch.healthCheckMode).toBeUndefined();
  });
});

// Every field whose stock value would break stable_diffusion_cpp is
// overridden.
describe('stable_diffusion_cpp defaults', () => {
  it('defaults away from every field whose stock value breaks this type', () => {
    const d = applicationTypeDefaults.stable_diffusion_cpp;
    // /v1/health does not exist on this server: health_path would take the
    // application permanently unreachable.
    expect(d.healthCheckMode).toBe('model_sync');
    // The real model name lives here, not in /v1/models (a placeholder).
    expect(d.loadedModelsPath).toBe('/sdapi/v1/sd-models');
    expect(d.loadedModelsFormat).toBe('sdcpp_models');
    // Images only: the coarse openai flavor would make it a text candidate.
    expect(d.apiFlavors).toEqual(['openai_images']);
    // A 512x512 generation measured ~17s; limits permit 4096x4096.
    expect(d.timeoutMs).toBe(600000);
    // The server serves no /props.
    expect(d.contextProbePath).toBe('');
  });

  it('leaves every other type untouched', () => {
    expect(applicationTypeDefaults.vllm.timeoutMs).toBe(30000);
    expect(applicationTypeDefaults.vllm.apiFlavors).toEqual(['openai', 'anthropic']);
    expect(applicationTypeDefaults.llama_cpp.contextProbePath).toBe('/props');
  });
});
