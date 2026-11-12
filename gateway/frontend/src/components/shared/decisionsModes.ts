// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { DecisionsMode, SystemOneMode } from '../../api';

/** The create defaults of the two modes; also what a spec starts from. */
export const defaultDecisionsMode: DecisionsMode = 'auto';
export const defaultSystemOneMode: SystemOneMode = 'passthrough';

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
 * llama_cpp type. Every other kind, "" (auto-detect) included, may pass
 * through: under Auto the form cannot know the detected kind, and the backend
 * decides.
 */
export function decisionsPassthroughOffered(kind: string): boolean {
  return kind !== 'llama_cpp';
}

/**
 * The mode to keep after the type changed: a selected passthrough becomes
 * "auto" for a kind that cannot serve it, anything else stays. Mirrors the
 * backend, which resets a stored passthrough on a type change to llama_cpp.
 */
export function decisionsModeForKind(mode: DecisionsMode, kind: string): DecisionsMode {
  return mode === 'passthrough' && !decisionsPassthroughOffered(kind) ? 'auto' : mode;
}
