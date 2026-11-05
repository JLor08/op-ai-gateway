// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ClusterTotalCell, ClusterWithheldNotice } from './ClusterTotalCell';
import { messages } from '../i18n';
import type { ClusterMember, RuntimeSpec } from '../api';

const t = messages.de;

afterEach(cleanup);

function member(over: Partial<ClusterMember>): ClusterMember {
  return {
    position: 0,
    server_id: 'srv',
    server_name: 'Alpha',
    member_spec_id: '',
    gpus: [],
    threads: 0,
    cache_enabled: false,
    device_backend: 'auto',
    ram_estimate_mb: 1000,
    ram_measured_mb: 0,
    stale: false,
    port: 0,
    visible: true,
    ...over,
  };
}

function head(members: ClusterMember[], complete = true): RuntimeSpec {
  return {
    cluster_enabled: true,
    cluster_vram_total_mb: 24000,
    cluster_ram_total_mb: 9000,
    cluster_totals_complete: complete,
    cluster_members: members,
    gpus: [],
  } as unknown as RuntimeSpec;
}

describe('ClusterTotalCell', () => {
  it('marks the cluster total, shows the own value and prefixes an incomplete total', () => {
    const spec = head(
      [
        member({
          server_name: 'Beta',
          gpus: [{ index: 0, vram_estimate_mb: 0, vram_measured_mb: 0 }],
        }),
      ],
      false,
    );
    render(<ClusterTotalCell spec={spec} axis="vram" ownMb={8000} t={t} />);
    expect(screen.getByText(t.clusterTotalLabel)).toBeTruthy();
    expect(screen.getByTestId('cluster-total-vram').textContent).toBe('≥ 24000 MB');
    expect(screen.getByText(`${t.clusterOwnValue}: 8000 MB`)).toBeTruthy();
  });

  it('breaks down visible members only and keeps a CPU member out of VRAM', () => {
    const spec = head([
      member({
        server_name: 'GpuBox',
        gpus: [{ index: 0, vram_estimate_mb: 16000, vram_measured_mb: 0 }],
      }),
      member({ position: 1, server_name: 'CpuBox' }),
      member({ position: 2, server_id: '', server_name: '', visible: false }),
    ]);
    render(<ClusterTotalCell spec={spec} axis="vram" ownMb={8000} t={t} />);
    fireEvent.click(screen.getByText(t.clusterBreakdown));
    const panel = screen.getByTestId('cluster-breakdown-vram');
    expect(panel.textContent).toContain('GpuBox: 16000 MB');
    expect(panel.textContent).toContain(`CpuBox (${t.clusterBreakdownCpu}): —`);
    expect(panel.textContent).toContain(t.clusterBreakdownHiddenCount(1));
  });

  it('names the incomplete visible member in the tooltip target on RAM too', () => {
    const spec = head([member({ server_name: 'CpuBox', ram_estimate_mb: 0 })], false);
    render(<ClusterTotalCell spec={spec} axis="ram" ownMb={0} t={t} />);
    expect(screen.getByTestId('cluster-total-ram').textContent).toBe('≥ 9000 MB');
    expect(screen.getByLabelText(`${t.clusterIncompleteTooltip}: CpuBox`)).toBeTruthy();
  });
});

describe('ClusterTotalCell incomplete causes', () => {
  it('says the head own value is missing when no member explains the lower bound', () => {
    const spec = head([member({ ram_estimate_mb: 1000 })], false);
    render(<ClusterTotalCell spec={spec} axis="ram" ownMb={0} t={t} />);
    expect(screen.getByLabelText(t.clusterIncompleteHead)).toBeTruthy();
  });

  it('shows a neutral lower bound with no attributable cause', () => {
    const spec = head([member({ ram_estimate_mb: 1000 })], false);
    render(<ClusterTotalCell spec={spec} axis="ram" ownMb={4000} t={t} />);
    expect(screen.getByLabelText(t.clusterIncompleteNeutral)).toBeTruthy();
  });

  it('keys breakdown rows by position so duplicate member names both show', () => {
    const spec = head([
      member({ server_name: 'Twin', ram_estimate_mb: 1000 }),
      member({ position: 1, server_name: 'Twin', ram_estimate_mb: 2000 }),
    ]);
    const spy = vi.spyOn(console, 'error').mockImplementation(() => {});
    render(<ClusterTotalCell spec={spec} axis="ram" ownMb={500} t={t} />);
    fireEvent.click(screen.getByText(t.clusterBreakdown));
    const panel = screen.getByTestId('cluster-breakdown-ram');
    expect(panel.textContent).toContain(`Twin (${t.clusterBreakdownCpu}): 1000 MB`);
    expect(panel.textContent).toContain(`Twin (${t.clusterBreakdownCpu}): 2000 MB`);
    expect(spy.mock.calls.some((c) => String(c[0]).includes('same key'))).toBe(false);
    spy.mockRestore();
  });
});

describe('ClusterWithheldNotice', () => {
  it('shows the localized reason prominently', () => {
    render(<ClusterWithheldNotice reason="runtime.cluster_port_unknown" t={t} />);
    expect(screen.getByRole('alert').textContent).toBe(
      `${t.clusterWithheldTitle}: ${t.errorClusterPortUnknown}`,
    );
  });

  it('renders nothing without a reason', () => {
    const { container } = render(<ClusterWithheldNotice reason="" t={t} />);
    expect(container.textContent).toBe('');
  });
});
