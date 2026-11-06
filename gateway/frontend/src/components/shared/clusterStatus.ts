// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { ClusterMemberState, ClusterPhase, ClusterState, RuntimeSpec } from '../../api';

// Pure state-to-view logic for the cluster progress the runtime screen shows
// on a cluster head. The gateway reports each member by position only; this
// module joins the position to the member's visible name.

export const clusterPhases: readonly ClusterPhase[] = [
  'members_starting',
  'members_ready',
  'head_starting',
  'formed',
];

export type ClusterStepState = 'done' | 'active' | 'pending';

export interface ClusterStepView {
  phase: ClusterPhase;
  state: ClusterStepState;
}

export interface ClusterMemberView {
  position: number;
  /** The visible member's server name; undefined for a hidden or unknown one. */
  name?: string;
  state: ClusterMemberState;
  /** The failure code of a failed member. */
  code?: string;
}

export interface ClusterFailureView {
  code: string;
  at: string;
  head: boolean;
  /** The failed member's position, when it was a member. */
  position?: number;
  /** The failed member's visible name; undefined for a hidden one. */
  memberName?: string;
}

export interface ClusterStatusView {
  state: ClusterState;
  phase?: ClusterPhase;
  /** The four progress steps; empty while the cluster is not forming or formed. */
  steps: ClusterStepView[];
  members: ClusterMemberView[];
  cooldownUntil?: string;
  failure?: ClusterFailureView;
  /** The head is cold and nothing about it is worth a line. */
  idle: boolean;
}

function stepsFor(state: ClusterState, phase: ClusterPhase | undefined): ClusterStepView[] {
  if (state !== 'forming' && state !== 'formed') return [];
  // A formed cluster has completed every step, whatever phase text came with it.
  const at =
    state === 'formed'
      ? clusterPhases.length - 1
      : clusterPhases.indexOf(phase ?? 'members_starting');
  const current = at < 0 ? 0 : at;
  return clusterPhases.map((p, i) => {
    let step: ClusterStepState = 'pending';
    if (i < current || state === 'formed') step = 'done';
    else if (i === current) step = 'active';
    return { phase: p, state: step };
  });
}

/**
 * The view of a head's cluster status, or undefined when there is none to show:
 * a spec that is no cluster head, or one from a gateway that does not report it.
 */
export function clusterStatusView(spec: RuntimeSpec | undefined): ClusterStatusView | undefined {
  const status = spec?.cluster_status;
  if (!spec?.cluster_enabled || !status) return undefined;
  const nameAt = (position: number): string | undefined => {
    const m = (spec.cluster_members ?? []).find((c) => c.position === position);
    return m?.visible ? m.server_name : undefined;
  };
  const members = [...(status.members ?? [])]
    .sort((a, b) => a.position - b.position)
    .map((m) => ({
      position: m.position,
      name: nameAt(m.position),
      state: m.state,
      ...(m.code ? { code: m.code } : {}),
    }));
  const f = status.last_failure;
  const failure: ClusterFailureView | undefined = f
    ? {
        code: f.code,
        at: f.at,
        head: f.head ?? false,
        ...(f.position === undefined
          ? {}
          : { position: f.position, memberName: nameAt(f.position) }),
      }
    : undefined;
  return {
    state: status.state,
    phase: status.phase,
    steps: stepsFor(status.state, status.phase),
    members,
    cooldownUntil: status.state === 'cooling_down' ? status.cooldown_until : undefined,
    failure: status.state === 'cooling_down' || status.state === 'invalid' ? failure : undefined,
    idle: status.state === 'cold' && members.length === 0,
  };
}
