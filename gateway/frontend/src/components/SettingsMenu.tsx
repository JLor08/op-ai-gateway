// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useState, type MouseEvent } from 'react';
import {
  Box,
  Button,
  Checkbox,
  Divider,
  FormControlLabel,
  FormLabel,
  IconButton,
  Menu,
  MenuItem,
  Tooltip,
} from '@mui/material';
import ChevronLeftIcon from '@mui/icons-material/ChevronLeft';
import ChevronRightIcon from '@mui/icons-material/ChevronRight';
import TuneIcon from '@mui/icons-material/Tune';
import { useColumnDrag, columnDragSx, type DragPlace } from './shared/columnDrag';

export type SettingsMenuItem = { id: string; label: string };

/**
 * Generic visibility + order + reset menu, cloned from the REUSABLE part of
 * ColumnMenu (the icon-button -> Menu with a per-item checkbox + drag handle +
 * chevron move buttons + a Reset item), minus ColumnMenu's Activity-specific
 * time-display / owner-display radio blocks. Reuses the same shared column-drag
 * primitives so its interaction/markup matches the table column menu exactly.
 *
 * Self-contained: it owns its own anchor state + trigger IconButton. `items` may
 * be in any order; they are displayed in the current `order` (unknown-to-order
 * ids appended defensively). Reorder/toggle/reset are applied by the caller.
 */
export function SettingsMenu({
  items,
  hidden,
  order,
  onToggle,
  onReorder,
  onReset,
  buttonLabel,
  title,
  resetLabel,
  moveLeftLabel,
  moveRightLabel,
}: Readonly<{
  items: SettingsMenuItem[];
  hidden: string[];
  order: string[];
  onToggle: (id: string) => void;
  onReorder: (sourceId: string, targetId: string, place: DragPlace) => void;
  onReset: () => void;
  buttonLabel: string;
  title: string;
  resetLabel: string;
  moveLeftLabel: string;
  moveRightLabel: string;
}>) {
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  const { dragProps, draggingId, overId, overPlace } = useColumnDrag(onReorder, 'vertical');

  const labelById = new Map(items.map((it) => [it.id, it.label]));
  // Display in the current order; append any known item not present in order
  // (defensive — a newly-added tile before its order pref reconciles).
  const orderedIds = [
    ...order.filter((id) => labelById.has(id)),
    ...items.map((it) => it.id).filter((id) => !order.includes(id)),
  ];

  return (
    <>
      <Tooltip title={buttonLabel}>
        <IconButton
          size="small"
          aria-label={buttonLabel}
          onClick={(e: MouseEvent<HTMLElement>) => setAnchor(e.currentTarget)}
        >
          <TuneIcon fontSize="small" />
        </IconButton>
      </Tooltip>
      <Menu open={Boolean(anchor)} anchorEl={anchor} onClose={() => setAnchor(null)}>
        <MenuItem key="__title" disableRipple disabled sx={{ opacity: 1, py: 0.5 }}>
          <FormLabel sx={{ fontSize: 13 }}>{title}</FormLabel>
        </MenuItem>
        {orderedIds.map((id, index) => {
          const label = labelById.get(id) ?? id;
          return (
            <MenuItem
              key={id}
              disableRipple
              {...dragProps(id)}
              sx={[
                { py: 0, display: 'flex', gap: 0.5 },
                columnDragSx(id, draggingId, overId, overPlace, 'vertical'),
              ]}
            >
              <FormControlLabel
                sx={{ flexGrow: 1, mr: 0 }}
                control={
                  <Checkbox
                    size="small"
                    checked={!hidden.includes(id)}
                    onChange={() => onToggle(id)}
                  />
                }
                label={label}
              />
              <Tooltip title={moveLeftLabel}>
                <span>
                  <IconButton
                    size="small"
                    aria-label={`${moveLeftLabel}: ${label}`}
                    disabled={index === 0}
                    onClick={() => onReorder(id, orderedIds[index - 1], 'before')}
                  >
                    <ChevronLeftIcon fontSize="small" />
                  </IconButton>
                </span>
              </Tooltip>
              <Tooltip title={moveRightLabel}>
                <span>
                  <IconButton
                    size="small"
                    aria-label={`${moveRightLabel}: ${label}`}
                    disabled={index === orderedIds.length - 1}
                    onClick={() => onReorder(id, orderedIds[index + 1], 'after')}
                  >
                    <ChevronRightIcon fontSize="small" />
                  </IconButton>
                </span>
              </Tooltip>
            </MenuItem>
          );
        })}
        <Divider key="reset-divider" />
        <Box key="reset" sx={{ px: 2, py: 1 }}>
          <Button size="small" onClick={onReset}>
            {resetLabel}
          </Button>
        </Box>
      </Menu>
    </>
  );
}
