// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// The live-status row actions that act on a managed model's processes: the
// transient Start and Stop (POST .../runtime/start and .../runtime/stop,
// which write nothing) and Pin, which sets the persistent admin override.
// Which row offers which of them, and why one is disabled, is decided here so
// each rule is testable on its own; RuntimeAdminSection only renders the plan.

import type { RuntimeSpec, RuntimeStatus } from '../../api';
import { INSTANCES_MAX, instanceIndex, primaryStatusRows } from './runtimeInstances';

/**
 * The agent feature naming the router's POST /stop/{model} route (agent
 * 0.17.0). The gateway refuses a stop for an agent that does not declare it
 * (409 runtime.agent_feature_missing), so the portal shows Stop disabled
 * there instead of offering a click that can only fail.
 */
export const RUNTIME_STOP_FEATURE = 'runtime_stop';

/** The spec fields the plan reads. */
export type RowPlanSpec = Pick<
  RuntimeSpec,
  'replicable' | 'min_instances' | 'max_instances' | 'pinned' | 'admin_state'
>;

/**
 * Whether an instance counts as up for the row actions: running, or on its way
 * there. Every other state has no process a stop could drain (or one already
 * leaving), and leaves room for a start.
 */
function instanceIsLive(state: string): boolean {
  return state === 'running' || state === 'starting';
}

/** The highest running or starting instance index of a spec, if any. */
export function highestLiveInstance(
  rows: readonly RuntimeStatus[],
  specId: string,
): number | undefined {
  let highest: number | undefined;
  for (const row of rows) {
    if (row.spec_id !== specId || !instanceIsLive(row.state)) continue;
    const k = instanceIndex(row);
    if (highest === undefined || k > highest) highest = k;
  }
  return highest;
}

/**
 * How many of a spec's instances the agent keeps up, so a manual stop of one
 * of them would only see it started again at once: indexes below the larger
 * of a replicable spec's min_instances and 1 for a spec pinned or under
 * force_running (the agent's ProtectedCount). The agent refuses a stop there
 * (409 runtime.instance_protected).
 *
 * Unlike the restart sequence's floor (stopProtectedCount in
 * runtimeInstances.ts) it counts force_running, which is in force while the
 * operator clicks Stop and gone once the restart's force_stopped lands, and
 * it does not count instance 0 as protected on its own: a spec that keeps
 * nothing up may stop its last instance.
 */
export function manualStopProtectedCount(
  spec: Pick<RuntimeSpec, 'replicable' | 'min_instances' | 'pinned' | 'admin_state'>,
): number {
  const floor = spec.replicable ? spec.min_instances : 0;
  const held = spec.pinned || spec.admin_state === 'force_running' ? 1 : 0;
  return Math.max(floor, held);
}

// The states a burst instance rests at after a failure: the agent keeps the
// row until the index is named again, so the gateway's index rule treats the
// index as free (instanceReusableStates in gateway/instance_claims.go).
const reusableInstanceStates = new Set(['crashed', 'start_failed', 'not_permitted']);

/**
 * Whether a replicable spec has an instance index left that a manual start
 * could use, by the gateway's index rule as far as the portal can see it:
 * indexes below max_instances (0 = no cap of its own: all 64 an address can
 * carry), instance 0 taken while it runs or starts, an index k >= 1 taken
 * while the stream lists it in any state but the three a failed burst
 * instance rests at. The gateway also skips an index another mapping's name
 * occupies or an ensure under way holds; those it answers with 409
 * runtime.max_instances_reached, which the portal translates.
 */
export function startRoomLeft(
  spec: Pick<RuntimeSpec, 'max_instances'>,
  rows: readonly RuntimeStatus[],
  specId: string,
): boolean {
  const limit =
    spec.max_instances > 0 ? Math.min(spec.max_instances, INSTANCES_MAX) : INSTANCES_MAX;
  const taken = new Set<number>();
  for (const row of rows) {
    if (row.spec_id !== specId) continue;
    const k = instanceIndex(row);
    if (k === 0 ? instanceIsLive(row.state) : !reusableInstanceStates.has(row.state)) taken.add(k);
  }
  for (let k = 0; k < limit; k++) if (!taken.has(k)) return true;
  return false;
}

/**
 * The process actions one live-status row offers. An absent member is an
 * action the row does not show at all; a `disabled` reason is one it shows
 * greyed out, with that reason on hover.
 *
 *  - `start` (Play) sits on the spec's primary row (primaryStatusRows). A spec
 *    that is not replicable runs one instance, so it is offered only while
 *    that instance neither runs nor starts. A replicable spec keeps it while
 *    instances run (`more`: it starts one more), disabled once no index is
 *    left below max_instances. Disabled on either while the spec is pinned
 *    as stopped: the gateway refuses every start then.
 *  - `stop` sits on the row of the spec's highest running or starting
 *    instance, since instances stop highest first. Disabled on an instance
 *    the spec keeps up (manualStopProtectedCount), and when the agent does
 *    not declare runtime_stop.
 *  - `pin` sits on the primary row while no override is set, and names the
 *    override it writes: force_running while any instance of the spec runs or
 *    starts, else force_stopped. The override pins the state the operator
 *    sees.
 */
export type RuntimeRowPlan = {
  start?: { disabled?: 'force_stopped' | 'max_instances'; more: boolean };
  stop?: { disabled?: 'protected' | 'feature' };
  pin?: 'force_running' | 'force_stopped';
};

export function runtimeRowPlan(
  row: RuntimeStatus,
  spec: RowPlanSpec,
  rows: readonly RuntimeStatus[],
  agentFeatures: readonly string[],
): RuntimeRowPlan {
  const plan: RuntimeRowPlan = {};
  const k = instanceIndex(row);
  const highest = highestLiveInstance(rows, row.spec_id);
  const live = highest !== undefined;
  const primary = primaryStatusRows(rows).get(row.spec_id);
  const isPrimary = primary !== undefined && instanceIndex(primary) === k;
  if (isPrimary && (spec.replicable || !live)) {
    const more = spec.replicable && live;
    if (spec.admin_state === 'force_stopped') {
      plan.start = { disabled: 'force_stopped', more };
    } else if (spec.replicable && !startRoomLeft(spec, rows, row.spec_id)) {
      plan.start = { disabled: 'max_instances', more };
    } else {
      plan.start = { more };
    }
  }
  if (instanceIsLive(row.state) && k === highest) {
    if (k < manualStopProtectedCount(spec)) plan.stop = { disabled: 'protected' };
    else if (!agentFeatures.includes(RUNTIME_STOP_FEATURE)) plan.stop = { disabled: 'feature' };
    else plan.stop = {};
  }
  if (isPrimary && spec.admin_state === '') {
    plan.pin = live ? 'force_running' : 'force_stopped';
  }
  return plan;
}
