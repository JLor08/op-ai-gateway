// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { ApplicationType, ApplicationScheme } from '../../api';

// The type-specific fields that a type selection prefills. Everything else on
// the Application form (status, flavors, health mode/path/interval, tuning,
// token, benchmarks, path suffix) is deliberately excluded.
export interface TypeDefaults {
  port: number;
  scheme: ApplicationScheme;
  nativeResponses: boolean; // Codex /v1/responses native passthrough
  nativeMessages: boolean; // Claude /v1/messages native passthrough
  loadedModelsPath: string;
  loadedModelsFormat: string;
  contextProbePath: string;
}

// Per-type sensible defaults (researched 2026-08-02; see the design spec).
export const applicationTypeDefaults: Record<ApplicationType, TypeDefaults> = {
  ollama: {
    port: 11434,
    scheme: 'http',
    nativeResponses: false, // Ollama has no /v1/responses
    nativeMessages: true, // Ollama /v1/messages since v0.14.0
    loadedModelsPath: '/api/ps',
    loadedModelsFormat: 'auto',
    contextProbePath: '',
  },
  vllm: {
    port: 8000,
    scheme: 'http',
    nativeResponses: true,
    nativeMessages: true,
    loadedModelsPath: '/v1/models',
    loadedModelsFormat: 'openai',
    contextProbePath: '',
  },
  llama_cpp: {
    port: 8080,
    scheme: 'http',
    nativeResponses: true,
    nativeMessages: true,
    loadedModelsPath: '/props',
    loadedModelsFormat: 'llama_cpp',
    contextProbePath: '/props',
  },
  llama_swap: {
    port: 8080,
    scheme: 'http',
    nativeResponses: true,
    nativeMessages: true,
    loadedModelsPath: '/running',
    loadedModelsFormat: 'llama_swap',
    contextProbePath: '/upstream/{model}/props',
  },
  litellm: {
    port: 4000,
    scheme: 'http',
    nativeResponses: true,
    nativeMessages: true,
    loadedModelsPath: '/v1/models',
    loadedModelsFormat: 'openai',
    contextProbePath: '',
  },
};

// migrateTypeFields returns the subset of fields to change when the type switches
// from oldType to newType: a field still holding the OLD type's default adopts the
// NEW type's default; a field the operator customized is omitted (left untouched).
export function migrateTypeFields(
  oldType: ApplicationType,
  newType: ApplicationType,
  current: TypeDefaults,
): Partial<TypeDefaults> {
  const oldDefaults = applicationTypeDefaults[oldType];
  const newDefaults = applicationTypeDefaults[newType];
  const patch: Partial<TypeDefaults> = {};
  (Object.keys(newDefaults) as (keyof TypeDefaults)[]).forEach((key) => {
    if (current[key] === oldDefaults[key]) {
      // Same-typed assignment across the union of field types.
      (patch as Record<string, unknown>)[key] = newDefaults[key];
    }
  });
  return patch;
}
