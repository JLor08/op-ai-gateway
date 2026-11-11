// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import {
  clusterCellText,
  clusterIncompleteCause,
  clusterTotalsView,
  clusterWithheldReason,
  isClusterHead,
} from './clusterTotals';
import type { ClusterMember, RuntimeSpec } from '../../api';

function member(over: Partial<ClusterMember> = {}): ClusterMember {
  return {
    position: 0,
    server_id: 'srv_a',
    server_name: 'Alpha',
    member_spec_id: '',
    gpus: [],
    threads: 0,
    cache_enabled: false,
    device_backend: 'auto',
    ram_estimate_mb: 0,
    ram_measured_mb: 0,
    stale: false,
    port: 0,
    visible: true,
    ...over,
  };
}

function head(over: Partial<RuntimeSpec> = {}): RuntimeSpec {
  return {
    cluster_enabled: true,
    cluster_vram_total_mb: 30000,
    cluster_ram_total_mb: 20000,
    cluster_totals_complete: true,
    cluster_members: [],
    gpus: [],
    ...over,
  } as unknown as RuntimeSpec;
}

describe('isClusterHead', () => {
  it('is true only for an enabled head', () => {
    expect(isClusterHead(head())).toBe(true);
    expect(isClusterHead(head({ cluster_enabled: false }))).toBe(false);
    expect(isClusterHead(undefined)).toBe(false);
  });
});

describe('clusterTotalsView', () => {
  it('takes the totals from the document and marks a complete one without a lower bound', () => {
    const v = clusterTotalsView(head());
    expect(v.vramMb).toBe(30000);
    expect(v.ramMb).toBe(20000);
    expect(v.complete).toBe(true);
    expect(clusterCellText(v.vramMb, v.complete)).toBe('30000 MB');
  });

  it('prefixes a lower bound and names the visible incomplete member', () => {
    const v = clusterTotalsView(
      head({
        cluster_totals_complete: false,
        cluster_members: [
          member({
            ram_estimate_mb: 1000,
            gpus: [{ index: 0, vram_estimate_mb: 8000, vram_measured_mb: 0 }],
          }),
          member({
            position: 1,
            server_name: 'Beta',
            gpus: [{ index: 0, vram_estimate_mb: 0, vram_measured_mb: 0 }],
          }),
        ],
      }),
    );
    expect(clusterCellText(v.vramMb, v.complete)).toBe('≥ 30000 MB');
    expect(v.incompleteNames).toEqual(['Beta']);
    expect(v.hiddenIncomplete).toBe(false);
  });

  it('never names a hidden member and flags it only as hidden', () => {
    const v = clusterTotalsView(
      head({
        cluster_totals_complete: false,
        cluster_members: [member({ server_id: '', server_name: '', visible: false })],
      }),
    );
    expect(v.incompleteNames).toEqual([]);
    expect(v.hiddenIncomplete).toBe(true);
    expect(v.members).toEqual([]);
    expect(v.hiddenCount).toBe(1);
  });

  it('counts a CPU member for RAM only', () => {
    const v = clusterTotalsView(
      head({
        cluster_members: [
          member({ server_name: 'Cpu', ram_estimate_mb: 4000, ram_measured_mb: 5000 }),
        ],
      }),
    );
    expect(v.members).toEqual([{ name: 'Cpu', vramMb: 0, ramMb: 5000, cpu: true, stale: false }]);
  });

  it('takes max(measured, estimate) per GPU and sums the card rows', () => {
    const v = clusterTotalsView(
      head({
        cluster_members: [
          member({
            gpus: [
              { index: 0, vram_estimate_mb: 8000, vram_measured_mb: 9000 },
              { index: 1, vram_estimate_mb: 8000, vram_measured_mb: 0 },
            ],
            ram_estimate_mb: 1000,
            stale: true,
          }),
        ],
      }),
    );
    expect(v.members[0]).toMatchObject({ vramMb: 17000, ramMb: 1000, cpu: false, stale: true });
  });

  it('shows an em-dash for an unknown total', () => {
    expect(clusterCellText(0, true)).toBe('—');
    expect(clusterCellText(0, false)).toBe('—');
  });
});

describe('clusterWithheldReason', () => {
  it('is the code on a head and empty otherwise', () => {
    expect(
      clusterWithheldReason(head({ cluster_withheld_reason: 'runtime.cluster_port_unknown' })),
    ).toBe('runtime.cluster_port_unknown');
    expect(clusterWithheldReason(head())).toBe('');
    expect(clusterWithheldReason(undefined)).toBe('');
  });
});

describe('clusterIncompleteCause', () => {
  const incomplete = (members: ClusterMember[], over: Partial<RuntimeSpec> = {}) =>
    head({ cluster_totals_complete: false, cluster_members: members, ...over });

  it('is none for a complete total', () => {
    const spec = head();
    expect(clusterIncompleteCause(spec, clusterTotalsView(spec), 'ram', 0)).toBe('none');
  });

  it('blames a visible member first, then a hidden one', () => {
    const named = incomplete([member({ ram_estimate_mb: 0 })]);
    expect(clusterIncompleteCause(named, clusterTotalsView(named), 'ram', 0)).toBe('members');
    const hidden = incomplete([member({ server_id: '', server_name: '', visible: false })]);
    expect(clusterIncompleteCause(hidden, clusterTotalsView(hidden), 'ram', 4000)).toBe('hidden');
  });

  it('blames the head when its own value is the missing part and no member is', () => {
    const spec = incomplete([member({ ram_estimate_mb: 1000 })]);
    expect(clusterIncompleteCause(spec, clusterTotalsView(spec), 'ram', 0)).toBe('head');
  });

  it('does not call a CPU-only head own VRAM of zero missing', () => {
    const spec = incomplete([member({ ram_estimate_mb: 1000 })], { gpus: [] });
    expect(clusterIncompleteCause(spec, clusterTotalsView(spec), 'vram', 0)).toBe('unknown');
    const gpuHead = incomplete([member({ ram_estimate_mb: 1000 })], {
      gpus: [{ index: 0 }] as unknown as RuntimeSpec['gpus'],
    });
    expect(clusterIncompleteCause(gpuHead, clusterTotalsView(gpuHead), 'vram', 0)).toBe('head');
  });

  it('is unknown when nothing attributable explains the lower bound', () => {
    const spec = incomplete([member({ ram_estimate_mb: 1000 })]);
    expect(clusterIncompleteCause(spec, clusterTotalsView(spec), 'ram', 4000)).toBe('unknown');
  });
});
