// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { ApplicationType, RuntimeSpec } from '../../api';

/**
 * What a form knows about whether its upstream can honour
 * `responses_live_timings_enabled`.
 *
 * Three states, not two, because the launch-spec form genuinely has a third:
 * with Type on "Auto" the effective kind is detected from the binary's
 * basename, and that detection (routing.DetectRuntimeSpecType) is Go-only.
 * Mirroring it here would be a second, uncompiled copy of a matching rule --
 * exactly the drift this file's own list is already at risk of -- so Auto
 * stays UNKNOWN and the form sends no opinion rather than a guess.
 *
 *   capable   -- the form may send either value
 *   incapable -- the form must send NO key at all. An explicit true here is
 *                refused (application.responses_live_timings_unsupported /
 *                runtime_spec.responses_live_timings_unsupported), and since
 *                both forms restate `type` on every save, that refusal blocks
 *                the WHOLE save rather than just the flag. Omitting instead is
 *                what the backend normalises: a create takes the kind's own
 *                default and an update clears a stale true.
 *   unknown   -- only the launch-spec form, only under Auto.
 */
export type LiveTimingsKind = 'capable' | 'incapable' | 'unknown';

/**
 * The kinds whose request schema is known to tolerate `timings_per_token`.
 *
 * A HAND-COPY of `liveTimingsCapableKinds` in
 * `gateway/backend/internal/routing/live_timings.go`, which is the authority.
 * Nothing compiles the two together, so the exhaustive both-directions test in
 * liveTimings.test.ts is the only guard -- edit the two together.
 *
 * The Go map is keyed on the string BOTH vocabularies share
 * (ProviderLlamaCPP == "llama_cpp" == RuntimeSpecTypeLlamaCpp), which is why
 * one list serves an application `type` and a spec `type` alike.
 */
const liveTimingsCapableKinds: readonly string[] = ['llama_cpp'];

/** The application form's gate: it always knows its own `type`. */
export function applicationLiveTimingsKind(type: ApplicationType): LiveTimingsKind {
  return liveTimingsCapableKinds.includes(type) ? 'capable' : 'incapable';
}

/**
 * The launch-spec form's gate, from the WRITABLE Type select alone -- not from
 * the read-only `effective_type` echo, which is undefined on create and stale
 * the moment `binary` is edited. An explicit Type is the first branch of
 * routing.EffectiveRuntimeSpecType, so when it is set the form knows the
 * effective kind exactly; only "" (Auto) falls through to the Go-only
 * detection, and that is the one case answered `unknown`.
 */
export function runtimeSpecLiveTimingsKind(specType: RuntimeSpec['type']): LiveTimingsKind {
  if (specType === '') return 'unknown';
  return liveTimingsCapableKinds.includes(specType) ? 'capable' : 'incapable';
}
