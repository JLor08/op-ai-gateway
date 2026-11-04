// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// The pure side of a spec's instances (#340): reading the per-instance status
// rows the agent reports, and the launch-spec form's instance settings. Kept
// out of RuntimeAdminSection so each rule is testable on its own.

import type { PutRuntimeSpecRequest, RuntimeSpec, RuntimeStatus } from '../../api';
import { formatMetric } from './format';
import type { Translation } from './types';

// The agent's cap on a spec's instances (indexes 0..63); the backend refuses a
// minimum or maximum above it.
export const INSTANCES_MAX = 64;

/**
 * A status row's instance index. The wire always carries it, but a row
 * without a usable one -- from a gateway that predates instances -- is the
 * one row every spec has: instance 0. Never `undefined`, so nothing built from
 * it (a row key, the `#k` cell, an address) can read "undefined".
 */
export function instanceIndex(row: Pick<RuntimeStatus, 'instance'>): number {
  const k = row.instance;
  return typeof k === 'number' && Number.isInteger(k) && k > 0 ? k : 0;
}

/**
 * The row that speaks for its whole spec wherever one row has to, per spec id:
 * the spec list's state chip, RAM and probe columns, the one status row that
 * carries the spec-wide override actions, and the first half of the restart
 * action's state gate (restartGate, which also reads every other row).
 *
 * The pick, per spec id: the first instance-0 row wherever the stream lists
 * it; else the first `running` row; else the first row. The first two steps
 * are the gateway's own fold pick (foldInstanceRows / primaryRowIndexes in
 * agent_ingest.go), which reads its context size and capability verdicts from
 * that row; the gateway stops there and leaves a spec with neither without a
 * speaking row, while the portal falls back to the first row so the spec still
 * shows a state. Instance 0 first because it is the only row a spec that is not
 * replicable has, and the one its plain address names.
 *
 * It is NOT the spec's stop signal: under force_stopped every instance drains
 * at once, so instance 0 need not stop last -- the restart sequence waits on
 * every row (specStopState).
 */
export function primaryStatusRows(rows: readonly RuntimeStatus[]): Map<string, RuntimeStatus> {
  const rank = (row: RuntimeStatus) => {
    if (instanceIndex(row) === 0) return 0;
    return row.state === 'running' ? 1 : 2;
  };
  const bySpec = new Map<string, RuntimeStatus>();
  for (const row of rows) {
    const current = bySpec.get(row.spec_id);
    if (current === undefined || rank(row) < rank(current)) bySpec.set(row.spec_id, row);
  }
  return bySpec;
}

/**
 * How far a spec is down, for the restart sequence's wait: `absent` while the
 * stream lists no row of the spec at all, `stopped` once every row it lists is
 * `stopped`, `pending` while any other row remains.
 *
 * Every row, not one: force_stopped drains all of a spec's instances at once,
 * in no fixed order, and the override may only be cleared once none of them
 * still runs the old process -- a clear while a sibling drains lets it start
 * again under the old one's feet. An instance whose row is gone (a burst
 * instance the agent retired once it stopped) runs nothing, so only the listed
 * rows count.
 */
export function specStopState(
  rows: readonly RuntimeStatus[],
  specId: string,
): 'absent' | 'stopped' | 'pending' {
  let listed = false;
  for (const row of rows) {
    if (row.spec_id !== specId) continue;
    if (row.state !== 'stopped') return 'pending';
    listed = true;
  }
  return listed ? 'stopped' : 'absent';
}

/**
 * Whether the restart sequence may start on a spec, and if not, why: `open`;
 * `state` when the row that speaks for the spec (primaryStatusRows) is not in
 * a restartable state; `instance` when it is, but another listed row of a
 * protected instance of the spec rests in a state that is neither restartable
 * nor `stopped`.
 *
 * The sequence's wait (specStopState) needs EVERY listed row of the spec to
 * read `stopped`, so the gate has to hold for every row that stays listed. A
 * protected sibling at `not_permitted` or `pending_vram_unknown` has no
 * process for force_stopped to drain, and the agent's reset of a changed spec
 * deliberately leaves those two states alone, so that row would never turn
 * `stopped`: the sequence would time out and leave the force_stopped override
 * in force, admission-blocking the model. A burst sibling (an index at or
 * above stopProtectedCount) is different: the force_stopped push is an Apply,
 * and the agent retires every burst instance with no process and nothing
 * queued on it, so its row leaves the stream and does not count in the wait.
 * A sibling already `stopped` is fine (it counts as stopped), and so is one in
 * a restartable state (it drains, or the reset moves it to `stopped`).
 */
export function restartGate(
  rows: readonly RuntimeStatus[],
  specId: string,
  restartable: ReadonlySet<string>,
  spec: Pick<RuntimeSpec, 'replicable' | 'min_instances' | 'pinned'> | undefined,
): 'open' | 'state' | 'instance' {
  const primary = primaryStatusRows(rows).get(specId);
  if (primary === undefined || !restartable.has(primary.state)) return 'state';
  const protectedCount = stopProtectedCount(spec);
  const stuck = rows.some(
    (row) =>
      row.spec_id === specId &&
      instanceIndex(row) < protectedCount &&
      row.state !== 'stopped' &&
      !restartable.has(row.state),
  );
  return stuck ? 'instance' : 'open';
}

/**
 * How many instances of a spec the agent protects once the restart
 * sequence's force_stopped push reaches it -- instances 0 .. count-1, the
 * rest being burst capacity it retires when it has no process (the agent's
 * ProtectedCount and IsBurst, instance.go): the larger of a replicable spec's
 * min_instances, 1 for a pinned spec (force_running, the other source of that
 * 1, is gone under force_stopped), and 1 for instance 0, which is never
 * burst. Without the stored spec nothing is known about the floor, so every
 * instance counts as protected.
 */
function stopProtectedCount(
  spec: Pick<RuntimeSpec, 'replicable' | 'min_instances' | 'pinned'> | undefined,
): number {
  if (spec === undefined) return Number.POSITIVE_INFINITY;
  const floor = spec.replicable ? spec.min_instances : 0;
  return Math.max(floor, spec.pinned ? 1 : 0, 1);
}

/**
 * The spec list's idle-timeout cell. A spec STORED as replicable lists the
 * idle timeout in effect -- the one the gateway pushes and the agent unloads
 * its instances after, derived from the measured startup -- because its manual
 * value is only an override there (0 = no override; the form shows it), and
 * listing that 0 would hide an unload after, say, 150 s. A timeout in effect
 * of 0 is `never` (never unloaded: an agent without instance support keeps the
 * manual 0), not "0". Any other spec lists its stored value as before
 * instances existed, its 0 as the em-dash.
 */
export function listedIdleTimeout(
  spec:
    | Pick<
        RuntimeSpec,
        'configured' | 'replicable' | 'idle_timeout_seconds' | 'effective_idle_timeout_seconds'
      >
    | undefined,
  never: string,
): string {
  if (spec?.configured && spec.replicable) {
    const seconds = spec.effective_idle_timeout_seconds;
    return seconds > 0 ? String(seconds) : never;
  }
  return formatMetric(spec?.idle_timeout_seconds, 0);
}

/**
 * Whether a spec's log view shows several instances' output. The agent writes
 * every instance's output into the spec's one log buffer, the lines of
 * instance k >= 1 prefixed `[#k] ` (instance 0's unprefixed), so the spec-id
 * log view is spec-wide whichever instance row it is opened from. Said for a
 * spec stored as replicable, and for any spec the stream lists an instance
 * k >= 1 of (one turned off whose burst instances still drain, or one the
 * portal has no stored document for).
 */
export function showsAllInstanceLogs(
  spec: Pick<RuntimeSpec, 'replicable'> | undefined,
  rows: readonly RuntimeStatus[],
  specId: string,
): boolean {
  if (spec?.replicable) return true;
  return rows.some((row) => row.spec_id === specId && instanceIndex(row) > 0);
}

/**
 * A model-server row's running instances (ModelServerRow.running_instances).
 * Always on the wire from a current gateway; a row without a usable count is
 * read as 0 running, so the catalog never renders "undefined".
 */
export function runningInstanceCount(row: { running_instances?: number }): number {
  const n = row.running_instances;
  return typeof n === 'number' && Number.isFinite(n) && n > 0 ? n : 0;
}

/**
 * Whether the models catalog names a row's running instances: always for a
 * replicable spec -- one running instance of it is a count, not the model's
 * only process, and none running is worth saying too -- and otherwise only
 * once more than one runs (a spec turned off while its burst instances still
 * drain). One running process is what every other model has, so a count of 1
 * there would only add noise.
 */
export function showsRunningInstances(row: {
  replicable?: boolean;
  running_instances?: number;
}): boolean {
  return row.replicable === true || runningInstanceCount(row) > 1;
}

// The upstream name a status row reports when it runs what its mapping says:
// the mapping's own name for instance 0, `<name>#<k>` -- the instance's
// address -- for instance k. Only the row's own suffix counts, so on instance 0
// a name that merely looks like an address is still a different name.
export function instanceAddress(
  appModelName: string,
  row: Pick<RuntimeStatus, 'instance'>,
): string {
  const k = instanceIndex(row);
  return k > 0 ? `${appModelName}#${k}` : appModelName;
}

// Instances counted per spec id for the spec list's summary: only `running`
// ones, since a starting instance serves nothing yet. A spec with none running
// has no entry.
export function runningInstancesBySpec(rows: readonly RuntimeStatus[]): Map<string, number> {
  const counts = new Map<string, number>();
  for (const row of rows) {
    if (row.state !== 'running') continue;
    counts.set(row.spec_id, (counts.get(row.spec_id) ?? 0) + 1);
  }
  return counts;
}

// The spec list's line under a replicable spec's state chip: how many of its
// instances run (runningInstancesBySpec's counts, none when the spec has no
// entry) against its bounds, a maximum of 0 being unlimited.
export function instancesSummary(
  specId: string,
  spec: Pick<RuntimeSpec, 'min_instances' | 'max_instances'>,
  runningBySpec: ReadonlyMap<string, number>,
  t: Pick<Translation, 'runtimeInstancesSummary'>,
): string {
  return t.runtimeInstancesSummary(
    runningBySpec.get(specId) ?? 0,
    spec.min_instances,
    spec.max_instances > 0 ? spec.max_instances : null,
  );
}

// An instance-count field's text as the request's number: an empty field is 0,
// which the backend reads as "none" for the minimum and "unlimited" for the
// maximum -- so the form never has to show a 0 that means "no limit".
export function instanceCountFromText(text: string): number {
  return text.trim() === '' ? 0 : Number(text);
}

// The other direction, for a stored count: 0 shows as the empty field it is
// sent from.
export function instanceCountText(count: number): string {
  return count > 0 ? String(count) : '';
}

/** The launch-spec form's instance settings as typed: the box and three fields' text. */
export type InstanceForm = {
  replicable: boolean;
  minText: string;
  maxText: string;
  parallelismText: string;
};

/** The four instance settings as the spec PUT carries them. */
export type InstanceSettings = Pick<
  PutRuntimeSpecRequest,
  'replicable' | 'min_instances' | 'max_instances' | 'instance_parallelism'
>;

// The parallelism field's text as a number: empty is its default, 1.
function parallelismFromText(text: string): number {
  return text.trim() === '' ? 1 : Number(text);
}

/**
 * The four settings a form save sends. Unticked, "Multiple instances" hides
 * the three fields and they mean nothing, so the save sends the backend's
 * neutral values -- 0 for both counts and for the parallelism -- never what
 * the hidden fields still hold: a value nobody can see is no setting. Ticked,
 * it sends what is typed (validateInstances has checked it): an empty count is
 * 0 (none / unlimited), an empty parallelism its default 1.
 */
export function instanceSettingsBody(form: InstanceForm): InstanceSettings {
  if (!form.replicable) {
    return { replicable: false, min_instances: 0, max_instances: 0, instance_parallelism: 0 };
  }
  return {
    replicable: true,
    min_instances: instanceCountFromText(form.minText),
    max_instances: instanceCountFromText(form.maxText),
    instance_parallelism: parallelismFromText(form.parallelismText),
  };
}

function isWholeIn(n: number, min: number, max: number): boolean {
  return Number.isInteger(n) && n >= min && n <= max;
}

/**
 * The instance settings' refusals, checked before the round trip for the
 * same reason as the visible-devices pair: on Create a refused spec write would
 * leave the mapping behind without its spec.
 *
 * Only while "Multiple instances" is ticked: unticked, the fields are hidden
 * and the save sends the neutral 0/0/0 (instanceSettingsBody), so nothing in
 * them can be wrong -- an error about fields that are not on screen would be
 * one the operator cannot act on. Ticked, in this order:
 *
 *  - Each count a whole number from 0 to the cap (empty is 0), the
 *    parallelism a whole number of at least 1 (empty is its default 1), and a
 *    minimum no higher than the maximum when both are set (an empty maximum is
 *    unlimited, so no minimum exceeds it). The backend refuses a negative
 *    count as a tuning value and a count above the cap or a minimum above the
 *    maximum as `runtime_spec.instances_invalid`; the form names all of them
 *    with that one message, since they are all the instance fields' own.
 *  - Several instances on a fixed listen port: every instance is its own
 *    process on its own port, so only the agent's free-port pick (0) works.
 */
export function validateInstances(
  form: InstanceForm,
  listenPort: number,
  t: Pick<
    Translation,
    'errorRuntimeSpecInstancesInvalid' | 'errorRuntimeSpecInstancesNeedEphemeralPort'
  >,
): string | undefined {
  if (!form.replicable) return undefined;
  const min = instanceCountFromText(form.minText);
  const max = instanceCountFromText(form.maxText);
  if (!isWholeIn(min, 0, INSTANCES_MAX) || !isWholeIn(max, 0, INSTANCES_MAX)) {
    return t.errorRuntimeSpecInstancesInvalid;
  }
  if (!isWholeIn(parallelismFromText(form.parallelismText), 1, Number.MAX_SAFE_INTEGER)) {
    return t.errorRuntimeSpecInstancesInvalid;
  }
  if (min > 0 && max > 0 && min > max) return t.errorRuntimeSpecInstancesInvalid;
  if (listenPort !== 0) return t.errorRuntimeSpecInstancesNeedEphemeralPort;
  return undefined;
}
