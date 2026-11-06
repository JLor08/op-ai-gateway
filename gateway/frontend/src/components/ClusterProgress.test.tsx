// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, render, screen, within } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { ClusterProgress } from './ClusterProgress';
import { formatDate } from './shared/format';
import { messages } from '../i18n';
import type { ClusterMember, ClusterStatus, RuntimeSpec } from '../api';

const t = messages.de;

afterEach(cleanup);

function member(position: number, name: string, visible = true): ClusterMember {
  return { position, server_name: name, visible } as ClusterMember;
}

function head(status: ClusterStatus | undefined): RuntimeSpec {
  return {
    cluster_enabled: true,
    cluster_members: [member(0, 'Alpha'), member(1, 'Beta', false)],
    cluster_status: status,
  } as RuntimeSpec;
}

describe('ClusterProgress', () => {
  it('renders nothing without a status (older gateway)', () => {
    const { container } = render(<ClusterProgress spec={head(undefined)} t={t} />);
    expect(container).toBeEmptyDOMElement();
  });

  it('shows the state of a cold head and nothing in the compact cell', () => {
    const spec = head({ state: 'cold', members: [] });
    render(<ClusterProgress spec={spec} t={t} />);
    expect(screen.getByText(`${t.clusterStatusTitle}: ${t.clusterStateCold}`)).toBeInTheDocument();
    cleanup();
    const { container } = render(<ClusterProgress spec={spec} t={t} compact />);
    expect(container).toBeEmptyDOMElement();
  });

  it.each([
    ['members_starting', t.clusterPhaseMembersStarting],
    ['members_ready', t.clusterPhaseMembersReady],
    ['head_starting', t.clusterPhaseHeadStarting],
  ] as const)('forming in %s marks that step active', (phase, label) => {
    render(<ClusterProgress spec={head({ state: 'forming', phase, members: [] })} t={t} />);
    const steps = screen.getByTestId('cluster-progress-steps');
    for (const l of [
      t.clusterPhaseMembersStarting,
      t.clusterPhaseMembersReady,
      t.clusterPhaseHeadStarting,
      t.clusterPhaseFormed,
    ]) {
      expect(within(steps).getByText(l)).toBeInTheDocument();
    }
    expect(steps.querySelector('.MuiStepLabel-label.Mui-active')).toHaveTextContent(label);
  });

  it('formed completes every step', () => {
    render(
      <ClusterProgress spec={head({ state: 'formed', phase: 'formed', members: [] })} t={t} />,
    );
    const steps = screen.getByTestId('cluster-progress-steps');
    expect(steps.querySelectorAll('.MuiStepLabel-label.Mui-completed')).toHaveLength(4);
    expect(steps.querySelector('.MuiStepLabel-label.Mui-active')).toBeNull();
  });

  it('shows each member by visible name, a hidden one by position, and a failure code', () => {
    render(
      <ClusterProgress
        spec={head({
          state: 'forming',
          phase: 'members_starting',
          members: [
            { position: 0, state: 'ready' },
            { position: 1, state: 'failed', code: 'runtime.cluster_member_unreachable' },
          ],
        })}
        t={t}
      />,
    );
    const chips = screen.getAllByTestId('cluster-progress-member');
    expect(chips[0]).toHaveTextContent(`Alpha: ${t.clusterMemberStateReady}`);
    expect(chips[1]).toHaveTextContent(
      `${t.clusterStatusMemberHidden(2)}: ${t.clusterMemberStateFailed}: ${t.errorRuntimeClusterMemberUnreachable}`,
    );
  });

  it('labels pending, starting and held members', () => {
    render(
      <ClusterProgress
        spec={head({
          state: 'forming',
          phase: 'members_starting',
          members: [
            { position: 0, state: 'pending' },
            { position: 1, state: 'held' },
          ],
        })}
        t={t}
      />,
    );
    const chips = screen.getAllByTestId('cluster-progress-member');
    expect(chips[0]).toHaveTextContent(t.clusterMemberStatePending);
    expect(chips[1]).toHaveTextContent(t.clusterMemberStateHeld);
  });

  it('cooling_down shows the cooldown end and the failed member', () => {
    const until = '2026-01-01T10:00:00Z';
    render(
      <ClusterProgress
        spec={head({
          state: 'cooling_down',
          members: [],
          cooldown_until: until,
          last_failure: {
            code: 'runtime.cluster_start_failed',
            position: 0,
            at: '2026-01-01T09:55:00Z',
          },
        })}
        t={t}
      />,
    );
    expect(screen.getByText(t.clusterCooldownUntil(formatDate(until, '')))).toBeInTheDocument();
    expect(
      screen.getByText(t.clusterLastFailure(t.errorClusterStartFailed, 'Alpha')),
    ).toBeInTheDocument();
  });

  it('names the head when the head failed', () => {
    render(
      <ClusterProgress
        spec={head({
          state: 'invalid',
          members: [],
          last_failure: { code: 'runtime.cluster_start_failed', head: true, at: 'x' },
        })}
        t={t}
      />,
    );
    expect(screen.getByText(t.clusterStateInvalid, { exact: false })).toBeInTheDocument();
    expect(
      screen.getByText(t.clusterLastFailure(t.errorClusterStartFailed, t.clusterStatusHead)),
    ).toBeInTheDocument();
  });

  it('the compact cell shows the current phase and the members', () => {
    render(
      <ClusterProgress
        spec={head({
          state: 'forming',
          phase: 'head_starting',
          members: [{ position: 0, state: 'ready' }],
        })}
        t={t}
        compact
      />,
    );
    const cell = screen.getByTestId('cluster-progress-compact');
    expect(within(cell).getByText(t.clusterPhaseHeadStarting)).toBeInTheDocument();
    expect(within(cell).getByTestId('cluster-progress-member')).toHaveTextContent('Alpha');
  });
});
