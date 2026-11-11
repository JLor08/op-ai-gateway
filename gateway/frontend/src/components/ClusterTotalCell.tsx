// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useState } from 'react';
import { Alert, Box, Button, Chip, Popover, Tooltip, Typography } from '@mui/material';
import type { RuntimeSpec } from '../api';
import type { Translation } from './shared/types';
import { formatChatRunErrorCode } from './shared/format';
import {
  clusterCellText,
  clusterIncompleteCause,
  clusterTotalsView,
  type ClusterAxis,
} from './shared/clusterTotals';

function mbText(mb: number): string {
  return mb > 0 ? `${mb} MB` : '—';
}

/**
 * A resource-table cell of a cluster head: the cluster total marked "Cluster"
 * (a lower bound on this axis carries "≥" and, where visible, names the
 * members incomplete on it), the head's own value beneath, and a popover with the per-member
 * breakdown. Hidden members only count inside the totals.
 */
export function ClusterTotalCell({
  spec,
  name,
  axis,
  ownMb,
  t,
}: Readonly<{
  spec: RuntimeSpec;
  /** The head's model name, which tells this row's breakdown button apart. */
  name: string;
  axis: ClusterAxis;
  ownMb: number;
  t: Translation;
}>) {
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  const view = clusterTotalsView(spec);
  const total = view[axis];
  const cause = clusterIncompleteCause(spec, view, axis, ownMb);
  let hint = '';
  if (cause === 'members')
    hint = `${t.clusterIncompleteTooltip}: ${total.incompleteNames.join(', ')}`;
  else if (cause === 'hidden') hint = t.clusterIncompleteHidden;
  else if (cause === 'head') hint = t.clusterIncompleteHead;
  else if (cause === 'unknown') hint = t.clusterIncompleteNeutral;
  // Focusable only when it carries a hint, so a keyboard user can open it;
  // the tooltip describes the total rather than replacing its name.
  const text = (
    <Typography
      component="span"
      variant="body2"
      data-testid={`cluster-total-${axis}`}
      tabIndex={hint ? 0 : undefined}
    >
      {clusterCellText(total.mb, total.complete)}
    </Typography>
  );
  return (
    <Box sx={{ display: 'grid', gap: 0.25, justifyItems: 'start' }}>
      <Box sx={{ display: 'flex', gap: 0.5, alignItems: 'center' }}>
        <Chip size="small" variant="outlined" label={t.clusterTotalLabel} />
        {hint ? (
          <Tooltip title={hint} describeChild>
            {text}
          </Tooltip>
        ) : (
          text
        )}
      </Box>
      <Typography variant="caption" color="text.secondary">
        {`${t.clusterOwnValue}: ${mbText(ownMb)}`}
      </Typography>
      <Button
        type="button"
        size="small"
        sx={{ p: 0, minWidth: 0 }}
        aria-label={t.clusterBreakdownFor(axis === 'vram' ? 'VRAM' : 'RAM', name)}
        onClick={(e) => setAnchor(e.currentTarget)}
      >
        {t.clusterBreakdown}
      </Button>
      <Popover
        open={anchor !== null}
        anchorEl={anchor}
        onClose={() => setAnchor(null)}
        anchorOrigin={{ vertical: 'bottom', horizontal: 'left' }}
      >
        <Box sx={{ p: 1.5, display: 'grid', gap: 0.5 }} data-testid={`cluster-breakdown-${axis}`}>
          <Typography variant="body2">{`${t.clusterBreakdownHead}: ${mbText(ownMb)}`}</Typography>
          {view.members.map((m, i) => {
            const mb = axis === 'vram' ? m.vramMb : m.ramMb;
            return (
              <Typography key={i} variant="body2">
                {`${m.name}${m.cpu ? ` (${t.clusterBreakdownCpu})` : ''}${m.stale ? ` (${t.clusterBreakdownStale})` : ''}: ${axis === 'vram' && m.cpu ? '—' : mbText(mb)}`}
              </Typography>
            );
          })}
          {view.hiddenCount > 0 && (
            <Typography variant="caption" color="text.secondary">
              {t.clusterBreakdownHiddenCount(view.hiddenCount)}
            </Typography>
          )}
        </Box>
      </Popover>
    </Box>
  );
}

/**
 * Why the gateway does not send this cluster head to its agent. Renders
 * nothing for a head that is sent.
 */
export function ClusterWithheldNotice({
  reason,
  t,
  compact = false,
}: Readonly<{ reason: string; t: Translation; compact?: boolean }>) {
  if (!reason) return null;
  const text = `${t.clusterWithheldTitle}: ${formatChatRunErrorCode(reason, t)}`;
  if (compact) {
    return (
      <Tooltip title={text} describeChild>
        <Chip
          size="small"
          color="warning"
          variant="outlined"
          label={formatChatRunErrorCode(reason, t)}
          tabIndex={0}
          data-testid="cluster-withheld"
        />
      </Tooltip>
    );
  }
  return (
    <Alert severity="warning" data-testid="cluster-withheld">
      {text}
    </Alert>
  );
}
