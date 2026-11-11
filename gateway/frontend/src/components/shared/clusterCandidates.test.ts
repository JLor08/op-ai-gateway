// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import {
  compatibleMemberSpecs,
  memberProblem,
  offeredCandidates,
  type CandidatesState,
} from './clusterCandidates';
import type { ClusterCandidate, RPCMemberSpec } from '../../api';

const spec = (id: string) => ({ id }) as RPCMemberSpec;

function state(
  candidates: ClusterCandidate[],
  over: Partial<CandidatesState> = {},
): CandidatesState {
  return { loading: false, failed: false, candidates, headKnown: true, ...over };
}

const member = (server_id: string, member_spec_id = '') => ({
  server_id,
  member_spec_id,
  visible: true,
});

describe('memberProblem', () => {
  const list = state([
    { server_id: 'a', server_name: 'A', compatible: true },
    { server_id: 'b', server_name: 'B', compatible: false },
  ]);

  it('judges automatic members by the listing', () => {
    expect(memberProblem(member('a'), 'automatic', list)).toBeUndefined();
    expect(memberProblem(member('b'), 'automatic', list)).toBe('no_matching_binary');
    expect(memberProblem(member('c'), 'automatic', list)).toBe('not_offered');
  });

  it('judges nothing while loading, after a failure, for a hidden member or an unknown head version', () => {
    expect(memberProblem(member('b'), 'automatic', { ...list, loading: true })).toBeUndefined();
    expect(memberProblem(member('b'), 'automatic', { ...list, failed: true })).toBeUndefined();
    expect(memberProblem({ ...member('b'), visible: false }, 'automatic', list)).toBeUndefined();
    expect(memberProblem(member('b'), 'automatic', { ...list, headKnown: false })).toBeUndefined();
  });

  it('flags a manual member only for its spec binary version', () => {
    const manual = state([
      {
        server_id: 'a',
        server_name: 'A',
        member_specs: [spec('s1'), spec('s2')],
        incompatible_member_specs: ['s2'],
      },
    ]);
    expect(memberProblem(member('a', 's1'), 'manual', manual)).toBeUndefined();
    expect(memberProblem(member('a', 's2'), 'manual', manual)).toBe('version_mismatch');
    expect(memberProblem(member('z', 's9'), 'manual', manual)).toBeUndefined();
  });
});

describe('the picker offer', () => {
  it('offers compatible candidates that are no member yet, an older gateway counting as compatible', () => {
    const list = state([
      { server_id: 'a', server_name: 'A', compatible: true },
      { server_id: 'b', server_name: 'B', compatible: false },
      { server_id: 'c', server_name: 'C' },
      { server_id: 'd', server_name: 'D', compatible: true },
    ]);
    expect(offeredCandidates(list, new Set(['d'])).map((c) => c.server_id)).toEqual(['a', 'c']);
  });

  it('leaves out the member specs of another version', () => {
    const c: ClusterCandidate = {
      server_id: 'a',
      server_name: 'A',
      member_specs: [spec('s1'), spec('s2')],
      incompatible_member_specs: ['s1'],
    };
    expect(compatibleMemberSpecs(c).map((s) => s.id)).toEqual(['s2']);
  });
});
