// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { ClusterMember, RuntimeSpec } from '../../api';

// Pure formatting logic for the cluster totals the runtime table shows on a
// cluster head. The gateway derives the totals (cluster_vram_total_mb and
// friends) on every read; this module only decides how they are marked and
// which members the breakdown may name.

/** One visible member's share of the totals. */
export interface ClusterMemberShare {
  name: string;
  vramMb: number;
  ramMb: number;
  /** A member without GPU rows adds RAM only, never VRAM. */
  cpu: boolean;
  /** A measurement kept from before the distribution changed. */
  stale: boolean;
}

export interface ClusterTotalsView {
  vramMb: number;
  ramMb: number;
  /** False when a part was unknown, so the totals are lower bounds. */
  complete: boolean;
  /** Visible members only, in --rpc order. */
  members: ClusterMemberShare[];
  /** Visible members with an unknown part (named only while incomplete). */
  incompleteNames: string[];
  /** The totals are incomplete and a hidden member may be the reason. */
  hiddenIncomplete: boolean;
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
// unknown.
function memberShare(m: ClusterMember): { share: ClusterMemberShare; unknown: boolean } {
  const gpus = m.gpus ?? [];
  const vramMb = gpus.reduce((sum, g) => sum + Math.max(g.vram_measured_mb, g.vram_estimate_mb), 0);
  const ramMb = Math.max(m.ram_measured_mb ?? 0, m.ram_estimate_mb ?? 0);
  const unknown =
    ramMb === 0 || gpus.some((g) => Math.max(g.vram_measured_mb, g.vram_estimate_mb) === 0);
  return {
    share: {
      name: m.server_name,
      vramMb,
      ramMb,
      cpu: gpus.length === 0,
      stale: m.stale === true,
    },
    unknown,
  };
}

export function clusterTotalsView(spec: RuntimeSpec): ClusterTotalsView {
  const complete = spec.cluster_totals_complete !== false;
  const members: ClusterMemberShare[] = [];
  const incompleteNames: string[] = [];
  let hiddenCount = 0;
  for (const m of spec.cluster_members ?? []) {
    if (!m.visible) {
      hiddenCount++;
      continue;
    }
    const { share, unknown } = memberShare(m);
    members.push(share);
    if (unknown && !complete) incompleteNames.push(share.name);
  }
  return {
    vramMb: spec.cluster_vram_total_mb ?? 0,
    ramMb: spec.cluster_ram_total_mb ?? 0,
    complete,
    members,
    incompleteNames,
    hiddenIncomplete: !complete && hiddenCount > 0,
    hiddenCount,
  };
}

/** A total for a table cell: "≥ " before a lower bound, an em-dash when unknown. */
export function clusterCellText(mb: number, complete: boolean): string {
  if (!(mb > 0)) return '—';
  return `${complete ? '' : '≥ '}${mb} MB`;
}
