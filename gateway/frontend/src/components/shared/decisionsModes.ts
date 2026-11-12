// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { DecisionsMode, SystemOneMode } from '../../api';

/** The create defaults of the two modes; also what a spec starts from. */
export const defaultDecisionsMode: DecisionsMode = 'auto';
export const defaultSystemOneMode: SystemOneMode = 'auto';

/**
 * Reads a stored mode, treating an absent or empty one (a fixture, an older
 * backend, or the `configured: false` spec document, which carries "") as the
 * default rather than as a choice.
 */
export function decisionsModeOrDefault(mode: DecisionsMode | '' | undefined): DecisionsMode {
  return mode || defaultDecisionsMode;
}

export function systemOneModeOrDefault(mode: SystemOneMode | '' | undefined): SystemOneMode {
  return mode || defaultSystemOneMode;
}

/**
 * Whether decisions_mode "passthrough" can be chosen for a server kind. A
 * HAND-COPY of the backend's refusal (application.decisions_passthrough_
 * unsupported / runtime_spec.decisions_passthrough_unsupported): llama.cpp
 * never answers OpenAI Decisions natively, so passthrough is refused for the
 * llama_cpp type; vllm_sr answers only /v1/systemone, so it is refused there
 * too. Every other kind, "" (auto-detect) included, may pass
 * through: under Auto the form cannot know the detected kind, and the backend
 * decides.
 */
export function decisionsPassthroughOffered(kind: string): boolean {
  return kind !== 'llama_cpp' && kind !== 'vllm_sr';
}

/**
 * The mode to keep after the type changed: a selected passthrough becomes
 * "auto" for a kind that cannot serve it, anything else stays. Mirrors the
 * backend, which resets a stored passthrough on a type change to llama_cpp or
 * vllm_sr.
 */
export function decisionsModeForKind(mode: DecisionsMode, kind: string): DecisionsMode {
  return mode === 'passthrough' && !decisionsPassthroughOffered(kind) ? 'auto' : mode;
}

/**
 * Whether systemone_mode "translate" can be chosen for a server kind. A
 * HAND-COPY of the backend's refusal (application.systemone_translate_
 * unsupported / runtime_spec.systemone_translate_unsupported): llama.cpp
 * answers neither endpoint natively and vllm_sr is System One itself, so
 * translation to OpenAI Decisions is refused for both. Every other kind, ""
 * (auto-detect) included, may translate; the backend decides.
 */
export function systemOneTranslateOffered(kind: string): boolean {
  return kind !== 'llama_cpp' && kind !== 'vllm_sr';
}

/**
 * The System One mode to keep after the type changed: a selected translate
 * becomes "auto" for a kind that cannot serve it. Mirrors the backend reset.
 */
export function systemOneModeForKind(mode: SystemOneMode, kind: string): SystemOneMode {
  return mode === 'translate' && !systemOneTranslateOffered(kind) ? 'auto' : mode;
}
