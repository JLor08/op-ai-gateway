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
    });
  });

  // server_agent's timeout default is 600000 (10 minutes), not the usual
  // 30000: it becomes a TOTAL request deadline that must cover a cold model
  // load, and 30s would fail every first request reproducibly (see the
  // backend default in portal service_applications.go / the task-19 brief).
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
});
