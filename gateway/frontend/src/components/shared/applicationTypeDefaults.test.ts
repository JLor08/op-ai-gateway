// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, it, expect } from 'vitest';
import {
  applicationTypeDefaults,
  migrateTypeFields,
  type TypeDefaults,
} from './applicationTypeDefaults';

describe('applicationTypeDefaults', () => {
  it('llama_swap has the loaded/probe/passthrough/port defaults', () => {
    expect(applicationTypeDefaults.llama_swap).toEqual({
      port: 8080,
      scheme: 'http',
      nativeResponses: true,
      nativeMessages: true,
      loadedModelsPath: '/running',
      loadedModelsFormat: 'llama_swap',
      contextProbePath: '/upstream/{model}/props',
    });
  });

  it('ollama defaults Claude-on, Codex-off, /api/ps auto, no probe', () => {
    expect(applicationTypeDefaults.ollama).toEqual({
      port: 11434,
      scheme: 'http',
      nativeResponses: false,
      nativeMessages: true,
      loadedModelsPath: '/api/ps',
      loadedModelsFormat: 'auto',
      contextProbePath: '',
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
    expect(patch.nativeResponses).toBe(true);
  });
});
