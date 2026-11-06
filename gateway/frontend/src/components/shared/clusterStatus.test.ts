// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import type { ClusterMember, ClusterStatus, RuntimeSpec } from '../../api';
import { clusterStatusView } from './clusterStatus';

function member(position: number, name: string, visible = true): ClusterMember {
  return { position, server_name: name, visible } as ClusterMember;
}

function head(status: ClusterStatus | undefined, members: ClusterMember[] = []): RuntimeSpec {
  return { cluster_enabled: true, cluster_members: members, cluster_status: status } as RuntimeSpec;
}

const two = [member(0, 'gpu-a'), member(1, 'gpu-b', false)];

describe('clusterStatusView', () => {
  it('is undefined for a gateway that reports no status, and for a non-head', () => {
    expect(clusterStatusView(head(undefined, two))).toBeUndefined();
    expect(clusterStatusView(undefined)).toBeUndefined();
    const notHead = { ...head({ state: 'cold', members: [] }), cluster_enabled: false };
    expect(clusterStatusView(notHead)).toBeUndefined();
  });

  it('a cold head with no members is idle and has no steps', () => {
    const v = clusterStatusView(head({ state: 'cold', members: [] }, two))!;
    expect(v.idle).toBe(true);
    expect(v.steps).toEqual([]);
  });

  it.each([
    ['members_starting', ['active', 'pending', 'pending', 'pending']],
    ['members_ready', ['done', 'active', 'pending', 'pending']],
    ['head_starting', ['done', 'done', 'active', 'pending']],
  ] as const)('forming in %s marks the steps', (phase, expected) => {
    const v = clusterStatusView(head({ state: 'forming', phase, members: [] }, two))!;
    expect(v.steps.map((s) => s.state)).toEqual(expected);
  });

  it('forming without a phase starts at the first step', () => {
    const v = clusterStatusView(head({ state: 'forming', members: [] }, two))!;
    expect(v.steps.map((s) => s.state)).toEqual(['active', 'pending', 'pending', 'pending']);
  });

  it('formed completes every step', () => {
    const v = clusterStatusView(head({ state: 'formed', phase: 'formed', members: [] }, two))!;
    expect(v.steps.map((s) => s.state)).toEqual(['done', 'done', 'done', 'done']);
  });

  it('joins members to visible names by position and hides the rest', () => {
    const v = clusterStatusView(
      head(
        {
          state: 'forming',
          phase: 'members_starting',
          members: [
            { position: 1, state: 'failed', code: 'runtime.cluster_member_unreachable' },
            { position: 0, state: 'ready' },
          ],
        },
        two,
      ),
    )!;
    expect(v.members).toEqual([
      { position: 0, name: 'gpu-a', state: 'ready' },
      { position: 1, name: undefined, state: 'failed', code: 'runtime.cluster_member_unreachable' },
    ]);
  });

  it('cooling_down carries the cooldown end and the failure with its member', () => {
    const v = clusterStatusView(
      head(
        {
          state: 'cooling_down',
          members: [],
          cooldown_until: '2026-01-01T10:00:00Z',
          last_failure: {
            code: 'runtime.cluster_start_failed',
            position: 0,
            at: '2026-01-01T09:55:00Z',
          },
        },
        two,
      ),
    )!;
    expect(v.cooldownUntil).toBe('2026-01-01T10:00:00Z');
    expect(v.failure).toMatchObject({
      code: 'runtime.cluster_start_failed',
      position: 0,
      memberName: 'gpu-a',
      head: false,
    });
  });

  it('names the head as the failure site and leaves a hidden member nameless', () => {
    const headFail = clusterStatusView(
      head(
        { state: 'cooling_down', members: [], last_failure: { code: 'c', head: true, at: 'x' } },
        two,
      ),
    )!;
    expect(headFail.failure?.head).toBe(true);
    expect(headFail.failure?.position).toBeUndefined();
    const hidden = clusterStatusView(
      head(
        { state: 'invalid', members: [], last_failure: { code: 'c', position: 1, at: 'x' } },
        two,
      ),
    )!;
    expect(hidden.failure?.position).toBe(1);
    expect(hidden.failure?.memberName).toBeUndefined();
  });

  it('drops a stale failure and cooldown outside cooling_down/invalid', () => {
    const v = clusterStatusView(
      head(
        {
          state: 'formed',
          phase: 'formed',
          members: [],
          cooldown_until: '2026-01-01T10:00:00Z',
          last_failure: { code: 'c', at: 'x' },
        },
        two,
      ),
    )!;
    expect(v.cooldownUntil).toBeUndefined();
    expect(v.failure).toBeUndefined();
  });
});
