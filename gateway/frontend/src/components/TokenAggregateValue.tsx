// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { type ReactNode } from 'react';
import { Tooltip } from '@mui/material';
import { tokenAggregate } from './billingUnit';
import type { Translation } from './shared/types';

/**
 * One token-denominated aggregate, rendered with the three-state rule of #70 and
 * -- the part that is easy to drop -- with the tooltip that EXPLAINS each of the
 * two exceptional states.
 *
 * This exists as one component rather than one per surface because the marker
 * and its explanation have to be identical everywhere they appear: a bare "*"
 * with no way to learn what it excludes is barely better than the silently wrong
 * number it replaced, and a per-surface copy is how one of them ends up without
 * a tooltip. The grouped table, the four Activity stat tiles, the Dashboard
 * 24h-token tile and the project-token rollups all render through here.
 *
 * The plain (all-token-metered) state deliberately returns a BARE string with no
 * wrapper element: it keeps the value in its parent's own text node, so callers
 * that render "<label>: <value>" in one line stay a single matchable string.
 */
export function TokenAggregateValue({
  value,
  nonTokenRequests,
  totalRequests,
  t,
  format = String,
}: Readonly<{
  value: number;
  /** How many requests of the population are NOT token-metered. */
  nonTokenRequests: number;
  /** The population size this aggregate sums over (requests, not tokens). */
  totalRequests: number;
  t: Translation;
  /** Number formatter for the applicable states; defaults to String. Pass e.g. toLocaleString to keep a surface's existing thousands separators. */
  format?: (value: number) => string;
}>): ReactNode {
  const agg = tokenAggregate(value, nonTokenRequests, totalRequests);
  if (!agg.applicable) {
    return (
      <Tooltip title={t.activityNotTokenMetered}>
        <span>{agg.text}</span>
      </Tooltip>
    );
  }
  if (agg.mixed) {
    return (
      <Tooltip title={t.activityMixedUnitsHint(nonTokenRequests)}>
        <span>{`${format(value)}*`}</span>
      </Tooltip>
    );
  }
  return format(value);
}
