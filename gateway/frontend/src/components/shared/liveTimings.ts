// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { ApplicationType, RuntimeSpec } from '../../api';

/**
 * What a form knows about whether its upstream can honour
 * `responses_live_timings_enabled`.
 *
 * Deliberately NOT a yes/no: each state below exists because some form has a
 * question the other two answers would misreport. Keep this list and the union
 * in step, and do not state a COUNT here -- an earlier version of this
 * docblock said "three states, not two", the shared control's suppression was
 * written to match that reading by testing a single value, and a fourth state
 * then fell silently into the checkbox branch. Nothing typechecks the
 * branches: all three consumers use plain comparisons, not an exhaustive
 * switch, so a missed state is a rendering bug rather than a compile error.
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
 *   delegated -- only the application form, only for server_agent: the flag is
 *                real for this application but is decided ELSEWHERE. The
 *                resolver overwrites the application's stored value with the
 *                runtime spec's, and the request-path gate judges the spec's
 *                effective kind rather than the application type. Like
 *                `incapable` it sends no key and shows no checkbox -- a
 *                control here would be a second switch the spec's value
 *                overrides -- but it must NOT reuse `incapable`'s sentence,
 *                which tells the operator the feature is unavailable to them
 *                when in fact runtime specs exist only under this type, so
 *                every managed llama.cpp runtime is reached through this form.
 */
export type LiveTimingsKind = 'capable' | 'incapable' | 'unknown' | 'delegated';

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
 * one lookup serves an application `type` and a spec `type` alike.
 *
 * A Set, mirroring the Go side's map: every read below is a membership test and
 * nothing here is ever ordered or indexed. One member is the measurement, not
 * the data structure -- see the Go file for why vllm left.
 */
const liveTimingsCapableKinds: ReadonlySet<string> = new Set(['llama_cpp']);

/**
 * The application form's gate: it always knows its own `type`.
 *
 * server_agent is answered `delegated`, not `incapable`. Its own type says
 * nothing about what actually serves -- routing.Resolver.targetFrom replaces
 * the application's flag with the runtime spec's whenever one exists, and
 * gateway.wantsResponsesLiveTimings then asks about the spec's
 * EffectiveRuntimeSpecType. So the answer for this type is "ask the launch
 * spec", which is a different sentence from "no kind here can do it".
 */
export function applicationLiveTimingsKind(type: ApplicationType): LiveTimingsKind {
  if (type === 'server_agent') return 'delegated';
  return liveTimingsCapableKinds.has(type) ? 'capable' : 'incapable';
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
  return liveTimingsCapableKinds.has(specType) ? 'capable' : 'incapable';
}
