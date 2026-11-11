// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useEffect, useRef, useState } from 'react';
import type { ClusterCandidate, ClusterMember, ClusterMemberMode, RPCMemberSpec } from '../../api';
import type { PortalApi } from './types';

// The member candidates of a cluster head, read for the head binary the form
// has. The gateway matches them with the same functions its save check uses
// (the automatic member's ggml-rpc-server, the manual member spec's binary
// version), so what this marks incompatible is what the save refuses.

export interface CandidatesState {
  loading: boolean;
  failed: boolean;
  candidates: ClusterCandidate[];
  // Whether the head binary's version is known; automatic members are
  // matched on it.
  headKnown: boolean;
}

// How long a changed head binary waits before the list is read again, so
// typing a path does not read it once per key.
export const CANDIDATES_DEBOUNCE_MS = 400;

const idle: CandidatesState = { loading: false, failed: false, candidates: [], headKnown: true };

// Where the head lives: its mapping once the spec is saved, else (a new spec)
// the application it is created on.
export interface CandidatesScope {
  mappingId: string;
  applicationId: string;
}

function readCandidates(
  api: Pick<PortalApi, 'clusterCandidates' | 'applicationClusterCandidates'>,
  scope: CandidatesScope,
  mode: ClusterMemberMode,
  headBinary: string,
) {
  return scope.mappingId
    ? api.clusterCandidates(scope.mappingId, mode, headBinary)
    : api.applicationClusterCandidates(scope.applicationId, mode, headBinary);
}

/**
 * Reads the candidates of the head in scope in mode for headBinary while
 * active, again whenever one of them or reloadKey changes. A changed binary
 * is read after CANDIDATES_DEBOUNCE_MS; the previous list stays, marked
 * loading, until the new one lands.
 */
export function useClusterCandidates(
  api: Pick<PortalApi, 'clusterCandidates' | 'applicationClusterCandidates'>,
  scope: CandidatesScope,
  mode: ClusterMemberMode,
  headBinary: string,
  active: boolean,
  reloadKey: number,
): CandidatesState {
  const [state, setState] = useState<CandidatesState>(idle);
  const { mappingId, applicationId } = scope;
  const lastBinaryRef = useRef<string | null>(null);
  useEffect(() => {
    if (!active) return undefined;
    const binaryChanged = lastBinaryRef.current !== null && lastBinaryRef.current !== headBinary;
    lastBinaryRef.current = headBinary;
    let cancelled = false;
    setState((cur) => ({ ...cur, loading: true, failed: false }));
    const timer = setTimeout(
      () => {
        readCandidates(api, { mappingId, applicationId }, mode, headBinary).then(
          (res) => {
            if (cancelled) return;
            setState({
              loading: false,
              failed: false,
              candidates: res.data,
              headKnown: res.head_binary_known ?? true,
            });
          },
          () => {
            if (!cancelled) setState({ ...idle, failed: true });
          },
        );
      },
      binaryChanged ? CANDIDATES_DEBOUNCE_MS : 0,
    );
    return () => {
      cancelled = true;
      clearTimeout(timer);
    };
  }, [api, mappingId, applicationId, mode, headBinary, active, reloadKey]);
  return active ? state : idle;
}

/** Whether a candidate can serve the head; an older gateway sends no flag. */
export function candidateCompatible(c: ClusterCandidate): boolean {
  return c.compatible !== false;
}

/** A manual candidate's member specs whose binary the save accepts. */
export function compatibleMemberSpecs(c: ClusterCandidate): RPCMemberSpec[] {
  const incompatible = new Set(c.incompatible_member_specs ?? []);
  return (c.member_specs ?? []).filter((s) => !incompatible.has(s.id));
}

/** The candidates the picker offers: compatible, and not a member yet. */
export function offeredCandidates(
  state: CandidatesState,
  taken: ReadonlySet<string>,
): ClusterCandidate[] {
  return state.candidates.filter((c) => candidateCompatible(c) && !taken.has(c.server_id));
}

export type MemberProblem =
  'no_matching_binary' | 'not_offered' | 'version_mismatch' | 'spec_not_offered';

// A manual member's problem: its spec is no longer offered to the head
// (disabled, its consent withdrawn, or taken by another head), or its
// binary is of another version than the head's.
function manualMemberProblem(
  specId: string,
  c: ClusterCandidate | undefined,
): MemberProblem | undefined {
  if (!c?.member_specs?.some((s) => s.id === specId)) return 'spec_not_offered';
  return c.incompatible_member_specs?.includes(specId) ? 'version_mismatch' : undefined;
}

/**
 * Why a visible member would make the save refuse the head with the form's
 * head binary, or undefined. Nothing is judged while the list loads or after
 * it failed, and an automatic member is not judged while the head binary's
 * version is unknown (the editor says that once for the head).
 */
export function memberProblem(
  member: Pick<ClusterMember, 'server_id' | 'member_spec_id' | 'visible'>,
  mode: ClusterMemberMode,
  state: CandidatesState,
): MemberProblem | undefined {
  if (!member.visible || state.loading || state.failed) return undefined;
  const c = state.candidates.find((x) => x.server_id === member.server_id);
  if (mode === 'manual') return manualMemberProblem(member.member_spec_id, c);
  if (!state.headKnown) return undefined;
  if (!c) return 'not_offered';
  return candidateCompatible(c) ? undefined : 'no_matching_binary';
}
