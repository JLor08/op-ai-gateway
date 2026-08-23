// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { Box, Button, TextField } from '@mui/material';
import type { Translation } from './shared/types';
import { SelectField } from './shared/SelectField';
import { GroupByChainBuilder } from './GroupByChainBuilder';

export type ActivityRange = '24h' | '7d' | '30d' | 'all' | 'custom';
export type ActivityScope = 'own' | 'all' | 'user';
export type ActivityStatusFilter = '' | 'success' | 'error';

/**
 * Cross-cutting controls for the activity view that are NOT per-column: the time
 * range (always has a value) and refresh. The global search, the per-column
 * filters, and the column-visibility menu now live in the table itself (search
 * box + per-column filter icons + a columns icon right of the search box).
 */
export function ActivityToolbar({
  t,
  range,
  onRange,
  customFrom,
  customTo,
  onCustomFrom,
  onCustomTo,
  groupChain,
  onGroupChain,
  onRefresh,
}: Readonly<{
  t: Translation;
  range: ActivityRange;
  onRange: (value: ActivityRange) => void;
  customFrom: string;
  customTo: string;
  onCustomFrom: (value: string) => void;
  onCustomTo: (value: string) => void;
  // Ordered group-by dimension chain ([] = off = the flat usage table). Lives
  // here, next to the range controls, so it reads with the Usage-metadata table
  // it governs.
  groupChain: string[];
  onGroupChain: (value: string[]) => void;
  onRefresh: () => void;
}>) {
  return (
    <Box sx={{ display: 'flex', flexWrap: 'wrap', alignItems: 'flex-end', gap: 1.5 }}>
      <GroupByChainBuilder t={t} chain={groupChain} onChange={onGroupChain} />
      <SelectField
        id="activity-range"
        label={t.activityRangeLabel}
        value={range}
        onChange={(e) => onRange(e.target.value as ActivityRange)}
      >
        <option value="24h">{t.activityRange24h}</option>
        <option value="7d">{t.activityRange7d}</option>
        <option value="30d">{t.activityRange30d}</option>
        <option value="all">{t.activityRangeAll}</option>
        <option value="custom">{t.activityRangeCustom}</option>
      </SelectField>
      {range === 'custom' && (
        <>
          <TextField
            size="small"
            type="datetime-local"
            label={t.activityRangeFrom}
            value={customFrom}
            onChange={(e) => onCustomFrom(e.target.value)}
            slotProps={{ inputLabel: { shrink: true } }}
          />
          <TextField
            size="small"
            type="datetime-local"
            label={t.activityRangeTo}
            value={customTo}
            onChange={(e) => onCustomTo(e.target.value)}
            slotProps={{ inputLabel: { shrink: true } }}
          />
        </>
      )}
      <Button variant="outlined" size="small" onClick={onRefresh}>
        {t.activityRefresh}
      </Button>
    </Box>
  );
}
