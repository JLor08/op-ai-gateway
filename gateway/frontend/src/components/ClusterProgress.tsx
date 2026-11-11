// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { Alert, Box, Chip, Step, StepLabel, Stepper, Typography } from '@mui/material';
import type { ClusterMemberState, ClusterPhase, ClusterState, RuntimeSpec } from '../api';
import type { Translation } from './shared/types';
import { formatChatRunErrorCode, formatDate } from './shared/format';
import {
  clusterStatusView,
  type ClusterBreachView,
  type ClusterFailureView,
  type ClusterMemberView,
  type ClusterPeakView,
} from './shared/clusterStatus';

const stateLabel = (s: ClusterState, t: Translation): string =>
  ({
    cold: t.clusterStateCold,
    forming: t.clusterStateForming,
    formed: t.clusterStateFormed,
    cooling_down: t.clusterStateCoolingDown,
    invalid: t.clusterStateInvalid,
  })[s] ?? s;

const phaseLabel = (p: ClusterPhase, t: Translation): string =>
  ({
    members_starting: t.clusterPhaseMembersStarting,
    members_ready: t.clusterPhaseMembersReady,
    head_starting: t.clusterPhaseHeadStarting,
    formed: t.clusterPhaseFormed,
  })[p] ?? p;

const memberStateLabel = (s: ClusterMemberState, t: Translation): string =>
  ({
    pending: t.clusterMemberStatePending,
    starting: t.clusterMemberStateStarting,
    ready: t.clusterMemberStateReady,
    held: t.clusterMemberStateHeld,
    failed: t.clusterMemberStateFailed,
  })[s] ?? s;

const memberStateColor = (
  s: ClusterMemberState,
): 'default' | 'info' | 'success' | 'warning' | 'error' =>
  (
    ({
      pending: 'default',
      starting: 'info',
      ready: 'success',
      held: 'warning',
      failed: 'error',
    }) as const
  )[s] ?? 'default';

const memberName = (m: { position: number; name?: string }, t: Translation): string =>
  m.name ?? t.clusterStatusMemberHidden(m.position + 1);

function failureWhere(f: ClusterFailureView, t: Translation): string {
  if (f.position === undefined) return t.clusterStatusHead;
  return f.memberName ?? t.clusterStatusMemberHidden(f.position + 1);
}

// A hidden member's breach or peak carries only its position, so its line
// says that something was reported and nothing more.
function breachText(b: ClusterBreachView, t: Translation): string {
  const who = memberName(b, t);
  const at = b.at ? formatDate(b.at, b.at) : '';
  if (!b.kind) return t.clusterBreachHidden(who);
  if (b.kind === 'peer') return t.clusterBreachPeer(who, b.peer ?? '—', at);
  if (b.kind === 'foreign_hold') return t.clusterBreachForeignHold(who, at);
  return t.clusterBreachOther(who, b.kind);
}

function peakText(p: ClusterPeakView, t: Translation): string {
  const who = memberName(p, t);
  const known = p.storedMb !== undefined && p.ceilingMb !== undefined;
  if (known && p.resource === 'vram' && p.gpuIndex !== undefined) {
    return t.clusterPeakVram(who, p.gpuIndex, p.storedMb!, p.ceilingMb!);
  }
  if (known && p.resource === 'ram') return t.clusterPeakRam(who, p.storedMb!, p.ceilingMb!);
  return t.clusterPeakHidden(who);
}

function MemberChip({ m, t }: Readonly<{ m: ClusterMemberView; t: Translation }>) {
  const code = m.code ? `: ${formatChatRunErrorCode(m.code, t)}` : '';
  return (
    <Chip
      size="small"
      variant="outlined"
      color={memberStateColor(m.state)}
      label={`${memberName(m, t)}: ${memberStateLabel(m.state, t)}${code}`}
      data-testid="cluster-progress-member"
    />
  );
}

/**
 * The cluster head's orchestration progress. Full: a step progress for a
 * forming or formed cluster, one indicator per member, the cooldown end and
 * the last failure. Compact (the live-status cell): the state, the current
 * step and the members' state in a few chips. Renders nothing for a spec
 * without a status, which is what an older gateway sends.
 */
export function ClusterProgress({
  spec,
  t,
  compact = false,
}: Readonly<{ spec: RuntimeSpec | undefined; t: Translation; compact?: boolean }>) {
  const view = clusterStatusView(spec);
  if (!view || (compact && view.idle)) return null;
  const failure = view.failure;
  const failureText = failure
    ? t.clusterLastFailure(formatChatRunErrorCode(failure.code, t), failureWhere(failure, t))
    : '';
  const cooldown = view.cooldownUntil
    ? t.clusterCooldownUntil(formatDate(view.cooldownUntil, view.cooldownUntil))
    : '';

  if (compact) {
    const forming = view.state === 'forming' && view.phase;
    return (
      <Box
        sx={{ display: 'grid', gap: 0.25, justifyItems: 'start' }}
        data-testid="cluster-progress-compact"
      >
        <Chip
          size="small"
          variant="outlined"
          color={view.state === 'cooling_down' || view.state === 'invalid' ? 'warning' : 'default'}
          label={forming ? phaseLabel(view.phase!, t) : stateLabel(view.state, t)}
          title={stateLabel(view.state, t)}
        />
        {view.members.map((m) => (
          <MemberChip key={m.position} m={m} t={t} />
        ))}
        {cooldown && (
          <Typography variant="caption" color="text.secondary">
            {cooldown}
          </Typography>
        )}
        {failureText && (
          <Typography variant="caption" color="text.secondary">
            {failureText}
          </Typography>
        )}
        {view.breaches.length > 0 && (
          <Chip
            size="small"
            variant="outlined"
            color="warning"
            label={t.clusterBreachesCount(view.breaches.length)}
            title={view.breaches.map((b) => breachText(b, t)).join('\n')}
          />
        )}
        {view.peaks.length > 0 && (
          <Chip
            size="small"
            variant="outlined"
            color="warning"
            label={t.clusterPeaksCount(view.peaks.length)}
            title={view.peaks.map((p) => peakText(p, t)).join('\n')}
          />
        )}
      </Box>
    );
  }

  const active = view.steps.findIndex((s) => s.state === 'active');
  return (
    <Box
      sx={{ display: 'grid', gap: 1 }}
      role="group"
      aria-label={t.clusterStatusTitle}
      data-testid="cluster-progress"
    >
      <Typography variant="subtitle2" component="h4">
        {`${t.clusterStatusTitle}: ${stateLabel(view.state, t)}`}
      </Typography>
      {view.steps.length > 0 && (
        <Stepper
          activeStep={active < 0 ? view.steps.length : active}
          data-testid="cluster-progress-steps"
        >
          {view.steps.map((s) => (
            <Step key={s.phase} completed={s.state === 'done'}>
              <StepLabel>{phaseLabel(s.phase, t)}</StepLabel>
            </Step>
          ))}
        </Stepper>
      )}
      {view.members.length > 0 && (
        <Box sx={{ display: 'flex', gap: 0.75, flexWrap: 'wrap' }}>
          {view.members.map((m) => (
            <MemberChip key={m.position} m={m} t={t} />
          ))}
        </Box>
      )}
      {cooldown && <Typography variant="body2">{cooldown}</Typography>}
      {failureText && (
        <Typography variant="body2" color="text.secondary">
          {failureText}
        </Typography>
      )}
      {view.breaches.length > 0 && (
        <Alert severity="warning" data-testid="cluster-breaches">
          <Typography variant="body2" sx={{ fontWeight: 600 }}>
            {t.clusterBreachesTitle}
          </Typography>
          {view.breaches.map((b, i) => (
            <Typography key={i} variant="body2" data-testid="cluster-breach">
              {breachText(b, t)}
            </Typography>
          ))}
        </Alert>
      )}
      {view.peaks.length > 0 && (
        <Alert severity="warning" data-testid="cluster-peaks">
          <Typography variant="body2" sx={{ fontWeight: 600 }}>
            {t.clusterPeaksTitle}
          </Typography>
          <Typography variant="body2">{t.clusterPeaksHint}</Typography>
          {view.peaks.map((p, i) => (
            <Typography key={i} variant="body2" data-testid="cluster-peak">
              {peakText(p, t)}
            </Typography>
          ))}
        </Alert>
      )}
    </Box>
  );
}
