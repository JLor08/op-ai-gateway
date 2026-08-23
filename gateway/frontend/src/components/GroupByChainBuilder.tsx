// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useState, type MouseEvent } from 'react';
import { Box, Button, Chip, Menu, MenuItem, Typography } from '@mui/material';
import AddIcon from '@mui/icons-material/Add';
import ClearIcon from '@mui/icons-material/Clear';
import type { Translation } from './shared/types';
import { columnDragSx, moveColumn, useColumnDrag } from './shared/columnDrag';

// The groupable dimensions in menu order, each with its existing i18n label.
// "service" (Phase 1 service accounts) mirrors "token"; "project" (spec:
// 2026-08-08-projects-design.md §7) mirrors "service" — both appended last,
// matching the backend's knownUsageGroupBy whitelist order.
export const GROUP_DIMS: { id: string; labelKey: keyof Translation }[] = [
  { id: 'session', labelKey: 'activityGroupSession' },
  { id: 'server', labelKey: 'activityGroupServer' },
  { id: 'user', labelKey: 'activityGroupUser' },
  { id: 'token', labelKey: 'activityGroupToken' },
  { id: 'model', labelKey: 'activityGroupModel' },
  { id: 'service', labelKey: 'activityGroupService' },
  { id: 'project', labelKey: 'activityGroupProject' },
];

// Human label for a dimension id (falls back to the raw id for an unknown value).
export function dimLabel(t: Translation, id: string): string {
  const d = GROUP_DIMS.find((x) => x.id === id);
  return d ? (t[d.labelKey] as string) : id;
}

/**
 * Ordered, editable group-by dimension chain shown as removable, drag-reorderable
 * chips + a "+ Ebene" menu of the remaining dimensions. Fully controlled: every
 * edit calls onChange with the next chain. No repeats (used dims drop out of the menu).
 */
export function GroupByChainBuilder({
  t,
  chain,
  onChange,
}: Readonly<{
  t: Translation;
  chain: string[];
  onChange: (next: string[]) => void;
}>) {
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  const { dragProps, draggingId, overId, overPlace } = useColumnDrag(
    (source, target, place) => onChange(moveColumn(chain, source, target, place)),
    'horizontal',
  );
  const remaining = GROUP_DIMS.filter((d) => !chain.includes(d.id));

  return (
    <Box sx={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 0.75 }}>
      <Typography variant="body2" sx={{ color: 'text.secondary', mr: 0.5 }}>
        {t.activityGroupBy}:
      </Typography>
      {chain.map((id, i) => (
        <Box key={id} sx={{ display: 'flex', alignItems: 'center', gap: 0.75 }}>
          {i > 0 && (
            <Typography aria-hidden variant="body2" sx={{ color: 'text.secondary' }}>
              →
            </Typography>
          )}
          <Chip
            size="small"
            label={dimLabel(t, id)}
            onDelete={() => onChange(chain.filter((x) => x !== id))}
            deleteIcon={
              <span role="button" aria-label={`${t.activityGroupRemoveLevel}: ${dimLabel(t, id)}`}>
                ✕
              </span>
            }
            {...dragProps(id)}
            sx={[{ cursor: 'grab' }, columnDragSx(id, draggingId, overId, overPlace, 'horizontal')]}
          />
        </Box>
      ))}
      {remaining.length > 0 && (
        <>
          <Button
            size="small"
            startIcon={<AddIcon fontSize="small" />}
            onClick={(e: MouseEvent<HTMLElement>) => setAnchor(e.currentTarget)}
          >
            {t.activityGroupAddLevel}
          </Button>
          <Menu open={Boolean(anchor)} anchorEl={anchor} onClose={() => setAnchor(null)}>
            {remaining.map((d) => (
              <MenuItem
                key={d.id}
                onClick={() => {
                  onChange([...chain, d.id]);
                  setAnchor(null);
                }}
              >
                {t[d.labelKey] as string}
              </MenuItem>
            ))}
          </Menu>
        </>
      )}
      {chain.length > 0 && (
        <Button
          size="small"
          color="inherit"
          startIcon={<ClearIcon fontSize="small" />}
          onClick={() => onChange([])}
          sx={{ color: 'text.secondary' }}
        >
          {t.activityGroupClear}
        </Button>
      )}
    </Box>
  );
}
