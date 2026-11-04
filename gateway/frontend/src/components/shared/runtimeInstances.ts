// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// The pure side of a spec's instances (#340): reading the per-instance status
// rows the agent reports, and the launch-spec form's instance settings. Kept
// out of RuntimeAdminSection so each rule is testable on its own.

import type { RuntimeStatus } from '../../api';
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
 * carries the spec-wide override actions, and the restart action's state gate.
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

/**
 * The instance settings' two refusals, checked before the round trip for the
 * same reason as the visible-devices pair: on Create a refused spec write would
 * leave the mapping behind without its spec. The backend refuses both too, in
 * this order, and checks the counts whether or not "Multiple instances" is
 * ticked -- the four settings are sent either way, so a pair that can never be
 * met would otherwise be stored to take effect the day the box is ticked.
 *
 *  - A count above the cap, or a minimum above a set maximum (an empty maximum
 *    is unlimited, so no minimum exceeds it).
 *  - Several instances on a fixed listen port: every instance is its own
 *    process on its own port, so only the agent's free-port pick (0) works.
 */
export function validateInstances(
  replicable: boolean,
  listenPort: number,
  minInstances: number,
  maxInstances: number,
  t: Pick<
    Translation,
    'errorRuntimeSpecInstancesInvalid' | 'errorRuntimeSpecInstancesNeedEphemeralPort'
  >,
): string | undefined {
  if (minInstances > INSTANCES_MAX || maxInstances > INSTANCES_MAX) {
    return t.errorRuntimeSpecInstancesInvalid;
  }
  if (maxInstances > 0 && minInstances > maxInstances) return t.errorRuntimeSpecInstancesInvalid;
  if (replicable && listenPort !== 0) return t.errorRuntimeSpecInstancesNeedEphemeralPort;
  return undefined;
}
