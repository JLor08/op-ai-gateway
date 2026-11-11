// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { ClusterMember, RuntimeSpec } from '../../api';

// Pure formatting logic for the cluster totals the runtime table shows on a
// cluster head. The gateway derives the totals (cluster_vram_total_mb and
// friends, with a completeness flag per axis) on every read; this module only
// decides how they are marked and which members the breakdown may name.

/** One visible member's share of the totals. */
export interface ClusterMemberShare {
  /** The member's place in the --rpc order, unique within the head. */
  position: number;
  name: string;
  vramMb: number;
  ramMb: number;
  /** A member without GPU rows adds RAM only, never VRAM. */
  cpu: boolean;
  /** A measurement kept from before the distribution changed. */
  stale: boolean;
}

export type ClusterAxis = 'vram' | 'ram';

/** One axis of the totals (VRAM or RAM). */
export interface ClusterAxisTotal {
  mb: number;
  /** False when a part of this total was unknown, so it is a lower bound. */
  complete: boolean;
  /** Visible members with an unknown part on this axis (named only while incomplete). */
  incompleteNames: string[];
  /** This total is incomplete and a hidden member may be the reason. */
  hiddenIncomplete: boolean;
}

export interface ClusterTotalsView {
  vram: ClusterAxisTotal;
  ram: ClusterAxisTotal;
  /** Visible members only, in --rpc order. */
  members: ClusterMemberShare[];
  /** Members the caller may not see: counted in the totals, never named. */
  hiddenCount: number;
}

export function isClusterHead(spec: Pick<RuntimeSpec, 'cluster_enabled'> | undefined): boolean {
  return spec?.cluster_enabled === true;
}

/** The stable code for why the head is withheld from its agent, or ''. */
export function clusterWithheldReason(
  spec: Pick<RuntimeSpec, 'cluster_enabled' | 'cluster_withheld_reason'> | undefined,
): string {
  return isClusterHead(spec) ? (spec?.cluster_withheld_reason ?? '') : '';
}

// The larger of measurement and estimate, the rule admission charges by; 0 is
// unknown. A CPU member has no VRAM part, so it is never unknown on VRAM.
function memberShare(m: ClusterMember): {
  share: ClusterMemberShare;
  unknown: Record<ClusterAxis, boolean>;
} {
  const gpus = m.gpus ?? [];
  const vramMb = gpus.reduce((sum, g) => sum + Math.max(g.vram_measured_mb, g.vram_estimate_mb), 0);
  const ramMb = Math.max(m.ram_measured_mb ?? 0, m.ram_estimate_mb ?? 0);
  return {
    share: {
      position: m.position,
      name: m.server_name,
      vramMb,
      ramMb,
      cpu: gpus.length === 0,
      stale: m.stale === true,
    },
    unknown: {
      vram: gpus.some((g) => Math.max(g.vram_measured_mb, g.vram_estimate_mb) === 0),
      ram: ramMb === 0,
    },
  };
}

// Whether one axis is complete. An older gateway sends only the combined
// flag, which then stands for both axes.
function axisComplete(spec: RuntimeSpec, axis: ClusterAxis): boolean {
  const own = axis === 'vram' ? spec.cluster_vram_total_complete : spec.cluster_ram_total_complete;
  return (own ?? spec.cluster_totals_complete) !== false;
}

export function clusterTotalsView(spec: RuntimeSpec): ClusterTotalsView {
  const axes: Record<ClusterAxis, ClusterAxisTotal> = {
    vram: {
      mb: spec.cluster_vram_total_mb ?? 0,
      complete: axisComplete(spec, 'vram'),
      incompleteNames: [],
      hiddenIncomplete: false,
    },
    ram: {
      mb: spec.cluster_ram_total_mb ?? 0,
      complete: axisComplete(spec, 'ram'),
      incompleteNames: [],
      hiddenIncomplete: false,
    },
  };
  const members: ClusterMemberShare[] = [];
  let hiddenCount = 0;
  for (const m of spec.cluster_members ?? []) {
    if (!m.visible) {
      hiddenCount++;
      continue;
    }
    const { share, unknown } = memberShare(m);
    members.push(share);
    for (const axis of ['vram', 'ram'] as const) {
      if (unknown[axis] && !axes[axis].complete) axes[axis].incompleteNames.push(share.name);
    }
  }
  for (const axis of ['vram', 'ram'] as const) {
    axes[axis].hiddenIncomplete = !axes[axis].complete && hiddenCount > 0;
  }
  return { ...axes, members, hiddenCount };
}

/** A total for a table cell: "≥ " before a lower bound, an em-dash when unknown. */
export function clusterCellText(mb: number, complete: boolean): string {
  if (mb <= 0 || Number.isNaN(mb)) return '—';
  return `${complete ? '' : '≥ '}${mb} MB`;
}

export type ClusterIncompleteCause = 'none' | 'members' | 'hidden' | 'head' | 'unknown';

/**
 * Why a total is a lower bound, for the cell's hint. A member the caller can
 * see wins, then a hidden one; the head's own value is blamed only when it is
 * missing and no member explains the gap (a CPU-only head legitimately has no
 * own VRAM, so zero there is not "missing"). 'unknown' means nothing
 * attributable explains it and the hint stays neutral.
 */
export function clusterIncompleteCause(
  spec: Pick<RuntimeSpec, 'gpus'>,
  view: ClusterTotalsView,
  axis: ClusterAxis,
  ownMb: number,
): ClusterIncompleteCause {
  const total = view[axis];
  if (total.complete) return 'none';
  if (total.incompleteNames.length > 0) return 'members';
  if (total.hiddenIncomplete) return 'hidden';
  const headHasPart = axis === 'ram' || (spec.gpus ?? []).length > 0;
  if (headHasPart && (ownMb <= 0 || Number.isNaN(ownMb))) return 'head';
  return 'unknown';
}
