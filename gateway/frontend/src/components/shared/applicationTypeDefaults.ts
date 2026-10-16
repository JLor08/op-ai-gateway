// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type {
  ApplicationType,
  ApplicationScheme,
  ApplicationHealthMode,
  EndpointMode,
} from '../../api';

// The type-specific fields that a type selection prefills. Most of the
// Application form is deliberately left OUT of this contract -- status,
// health interval, tuning other than timeoutMs, token, benchmarks, path
// suffix -- because those are operator preferences, not properties of the
// type, and a type switch must never clobber a preference. Four fields are
// the documented exceptions, all on the same argument: for at least one type
// the form's ordinary stock value is not a preference but a reproducible
// failure, so leaving it out would silently ship a type nobody could use
// without already knowing to hand-edit it away.
//
//   - timeoutMs: server_agent needs a 10-minute default instead of the usual
//     30s (it becomes a TOTAL request deadline that must cover a cold model
//     load).
//   - healthCheckMode: stable_diffusion_cpp's server (sd_server) exposes no
//     /v1/health, so the stock 'health_path' mode takes the application
//     permanently unreachable after one failed probe cycle, and every
//     request then 404s with no route -- a symptom that does not point back
//     at the health check that caused it.
//   - apiFlavors: the stock pair ['openai', 'anthropic'] would offer
//     stable_diffusion_cpp as a TEXT candidate. It serves images only, via
//     the coarse 'openai_images' flavor -- which is opt-in everywhere else in
//     this system precisely so a type must name it explicitly rather than
//     inherit it by accident.
//   - healthCheckPath: audio_cpp's server answers GET /health (200) and GET
//     /v1/health with 404, so the stock '/v1/health' path under the stock
//     'health_path' mode takes the application unreachable after one failed
//     probe cycle, the same silent failure as above. Only the PATH diverges
//     for it; the mode stays.
//
// All four ride the same migrateTypeFields preservation contract as every
// other field here instead of a bespoke special case: a field still holding
// the OLD type's default migrates to the NEW type's default, and a value the
// operator already changed is left alone. Every pre-existing type keeps its
// current effective values for healthCheckMode ('health_path'),
// healthCheckPath ('/v1/health') and apiFlavors (['openai', 'anthropic']), so
// this widening changes nothing for them.
//
// audio_cpp is the second type to diverge, on three of the four: its stock
// apiFlavors pair would offer a speech-only server as a text candidate (it
// serves speech only, via the coarse 'openai_audio' flavor, which is opt-in
// everywhere else for the same reason as 'openai_images'), its timeout is a
// long synthesis's, not 30s, and its health path is /health. It keeps the
// stock healthCheckMode: audio.cpp has a real health endpoint, unlike
// sd-server, so 'health_path' works for it once the path is right.
export interface TypeDefaults {
  port: number;
  scheme: ApplicationScheme;
  responsesMode: EndpointMode; // Codex /v1/responses endpoint mode
  messagesMode: EndpointMode; // Claude Code /v1/messages endpoint mode
  loadedModelsPath: string;
  loadedModelsFormat: string;
  contextProbePath: string;
  timeoutMs: number;
  // The exception to "health mode/path/interval stays out" -- see the doc
  // comment above. The MODE and the PATH (healthCheckPath, below) are type
  // properties; health_check_interval_seconds remains an operator preference
  // and is not part of this contract.
  healthCheckMode: ApplicationHealthMode;
  // The exception to "health path stays out" -- see the doc comment above. Used
  // when healthCheckMode is 'health_path'; every type but audio_cpp keeps the
  // stock value.
  healthCheckPath: string;
  // The exception to "flavors stay out" -- see the doc comment above.
  apiFlavors: string[];
}

// The stock health path the application form starts from, and every type but
// audio_cpp keeps.
export const stockApplicationHealthPath = '/v1/health';

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
    healthCheckMode: 'health_path',
    healthCheckPath: stockApplicationHealthPath,
    apiFlavors: ['openai', 'anthropic'],
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
    healthCheckMode: 'health_path',
    healthCheckPath: stockApplicationHealthPath,
    apiFlavors: ['openai', 'anthropic'],
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
    healthCheckMode: 'health_path',
    healthCheckPath: stockApplicationHealthPath,
    apiFlavors: ['openai', 'anthropic'],
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
    healthCheckMode: 'health_path',
    healthCheckPath: stockApplicationHealthPath,
    apiFlavors: ['openai', 'anthropic'],
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
    healthCheckMode: 'health_path',
    healthCheckPath: stockApplicationHealthPath,
    apiFlavors: ['openai', 'anthropic'],
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
    healthCheckMode: 'health_path',
    healthCheckPath: stockApplicationHealthPath,
    apiFlavors: ['openai', 'anthropic'],
  },
  // Measured against a real stable-diffusion.cpp server (FLUX.1-dev): a
  // 512x512 generation took ~17s, comfortably inside the 600000ms
  // (10-minute) timeout -- the same headroom server_agent needs for a cold
  // model load, borrowed here so an ordinary generation never times out.
  // model_sync replaces health_path because this server has no /v1/health: it
  // counts the application healthy when model discovery succeeds, and the
  // backend derives this type's discovery endpoint (/sdapi/v1/sd-models) from
  // the type itself -- loadedModelsPath below only drives the loaded-models
  // indicator. contextProbePath is empty because it has no /props either.
  // Port 7860 is the port of the MEASURED deployment, not the sd_server
  // binary's own default (1234) -- do not treat it as authoritative for every
  // install.
  stable_diffusion_cpp: {
    port: 7860,
    scheme: 'http',
    responsesMode: 'disabled',
    messagesMode: 'disabled',
    loadedModelsPath: '/sdapi/v1/sd-models',
    loadedModelsFormat: 'sdcpp_models',
    contextProbePath: '',
    timeoutMs: 600000,
    healthCheckMode: 'model_sync',
    healthCheckPath: stockApplicationHealthPath,
    apiFlavors: ['openai_images'],
  },
  // audio.cpp (audiocpp_server), probed live: GET /health answers
  // {"status":"ok",...} while GET /v1/health is a 404, so the stock health_path
  // mode stays but its PATH is /health (the stock /v1/health would take a
  // freshly created application unreachable after one probe cycle); GET /v1/models
  // lists the LOADED models in the standard OpenAI shape, which is exactly what
  // the loaded-models indicator reads (format 'openai'); there is no /props and
  // no chat endpoint, so the context probe is empty and both coding-agent
  // endpoint modes are 'disabled' (the openai and anthropic flavors are off by
  // default, and a gated dropdown shows Deaktiviert anyway). timeout_ms matches
  // the backend's default for this type (portal service_applications.go,
  // defaultAudioCppTimeoutMS): a long synthesis routinely outlasts 30s. Port
  // 8080 is a conventional placeholder, not a property of the binary -- the
  // operator sets the port the server was started on.
  audio_cpp: {
    port: 8080,
    scheme: 'http',
    responsesMode: 'disabled',
    messagesMode: 'disabled',
    loadedModelsPath: '/v1/models',
    loadedModelsFormat: 'openai',
    contextProbePath: '',
    timeoutMs: 600000,
    healthCheckMode: 'health_path',
    healthCheckPath: '/health',
    apiFlavors: ['openai_audio'],
  },
};

// Value equality across the union of field types this contract carries.
// apiFlavors is array-valued and needs TWO corrections over a plain `===`:
//
//   1. Reference: the live `current.apiFlavors` the caller passes in
//      (ApplicationSection's `flavors` state) is never the SAME array
//      instance as a defaults entry -- it comes from component state,
//      rebuilt on every checkbox toggle -- so `===` would always read
//      "customized" for this field, even right after a fresh create, and
//      the migration this function exists to do would silently never fire
//      for it.
//   2. Order: apiFlavors is a SET of enabled flavors, not a sequence, but a
//      positional comparison (current[i] === default[i]) still treats it as
//      one. ApiVariantControls' toggleFlavor removes a flavor with `filter`
//      and re-adds it with `[...list, flavor]`, so an operator who unchecks
//      then rechecks the SAME flavor before switching type ends up with the
//      same set in a different order (['anthropic', 'openai'] instead of
//      ['openai', 'anthropic']). A positional check would call that
//      "customized" too and skip the migration -- silently leaving a
//      type switch to stable_diffusion_cpp with the stock text-flavor pair
//      instead of ['openai_images'], which is exactly the failure this
//      field exists to prevent.
//
// Sorting both copies before comparing handles arbitrary reordering (and,
// as a side effect, repeated values) correctly; it is not merely a same-set
// check. Every other field in TypeDefaults today is a primitive, for which
// this is exactly `===`.
function typeDefaultFieldEqual(a: unknown, b: unknown): boolean {
  if (Array.isArray(a) && Array.isArray(b)) {
    if (a.length !== b.length) return false;
    const byText = (x: unknown, y: unknown) => String(x).localeCompare(String(y));
    const sortedA = [...a].sort(byText);
    const sortedB = [...b].sort(byText);
    return sortedA.every((v, i) => v === sortedB[i]);
  }
  return a === b;
}

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
    if (typeDefaultFieldEqual(current[key], oldDefaults[key])) {
      // Same-typed assignment across the union of field types.
      (patch as Record<string, unknown>)[key] = newDefaults[key];
    }
  });
  return patch;
}
