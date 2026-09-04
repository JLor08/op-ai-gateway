// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { ApplicationType, ApplicationScheme, EndpointMode } from '../../api';

// The type-specific fields that a type selection prefills. Everything else on
// the Application form (status, flavors, health mode/path/interval, tuning
// other than timeoutMs, token, benchmarks, path suffix) is deliberately
// excluded. timeoutMs is the one exception to "tuning stays out": server_agent
// needs a 10-minute default instead of the usual 30s (it becomes a TOTAL
// request deadline that must cover a cold model load), so it rides the same
// migrateTypeFields preservation contract as every other field here instead of
// a bespoke special case that would silently clobber a customized value.
export interface TypeDefaults {
  port: number;
  scheme: ApplicationScheme;
  responsesMode: EndpointMode; // Codex /v1/responses endpoint mode
  messagesMode: EndpointMode; // Claude Code /v1/messages endpoint mode
  loadedModelsPath: string;
  loadedModelsFormat: string;
  contextProbePath: string;
  timeoutMs: number;
}

// Per-type sensible defaults (researched 2026-08-02; see the design spec).
export const applicationTypeDefaults: Record<ApplicationType, TypeDefaults> = {
  ollama: {
    port: 11434,
    scheme: 'http',
    responsesMode: 'passthrough',
    messagesMode: 'passthrough',
    loadedModelsPath: '/api/ps',
    loadedModelsFormat: 'auto',
    contextProbePath: '',
    timeoutMs: 30000,
  },
  vllm: {
    port: 8000,
    scheme: 'http',
    responsesMode: 'passthrough',
    messagesMode: 'passthrough',
    loadedModelsPath: '/v1/models',
    loadedModelsFormat: 'openai',
    contextProbePath: '',
    timeoutMs: 30000,
  },
  llama_cpp: {
    port: 8080,
    scheme: 'http',
    responsesMode: 'passthrough',
    messagesMode: 'passthrough',
    loadedModelsPath: '/props',
    loadedModelsFormat: 'llama_cpp',
    contextProbePath: '/props',
    timeoutMs: 30000,
  },
  llama_swap: {
    port: 8080,
    scheme: 'http',
    responsesMode: 'passthrough',
    messagesMode: 'passthrough',
    loadedModelsPath: '/running',
    loadedModelsFormat: 'llama_swap',
    contextProbePath: '/upstream/{model}/props',
    timeoutMs: 30000,
  },
  litellm: {
    port: 4000,
    scheme: 'http',
    responsesMode: 'passthrough',
    messagesMode: 'passthrough',
    loadedModelsPath: '/v1/models',
    loadedModelsFormat: 'openai',
    contextProbePath: '',
    timeoutMs: 30000,
  },
  // Agent-managed model processes (agent-runtime-manager feature): the
  // gateway talks to the server-agent's own router, which fronts every
  // managed process the same way llama-swap does -- hence the llama_swap-
  // shaped loaded-models probe. timeout_ms defaults to 600000 (10 minutes)
  // on the backend (portal service_applications.go) because it is a TOTAL
  // request deadline covering a cold model load; 30s would fail every first
  // request reproducibly.
  server_agent: {
    port: 8081,
    scheme: 'http',
    responsesMode: 'passthrough',
    messagesMode: 'passthrough',
    loadedModelsPath: '/running',
    loadedModelsFormat: 'llama_swap',
    contextProbePath: '',
    timeoutMs: 600000,
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
