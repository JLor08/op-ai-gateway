// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useState, type MouseEvent, type ReactNode } from 'react';
import {
  Box,
  Button,
  Checkbox,
  Divider,
  FormControlLabel,
  IconButton,
  Menu,
  Tooltip,
} from '@mui/material';
import ViewColumnIcon from '@mui/icons-material/ViewColumn';
import ChevronLeftIcon from '@mui/icons-material/ChevronLeft';
import ChevronRightIcon from '@mui/icons-material/ChevronRight';
import { useColumnDrag, columnDragSx, type DragPlace } from './columnDrag';

/**
 * Column-visibility control: an icon (placed right of a table's search box) that
 * opens a checklist of columns plus per-column ←/→ reorder buttons (the
 * keyboard/touch-accessible alternative to header drag) and a "reset to default"
 * action. `columns` must be in current display order. Optional `extra` slot for
 * table-specific display toggles (e.g. owner-name/id, time absolute/relative).
 */
export function ColumnVisibilityMenu({
  columns,
  hidden,
  onToggle,
  onReorder,
  onReset,
  label,
  resetLabel,
  moveLeftLabel,
  moveRightLabel,
  extra,
}: Readonly<{
  columns: { id: string; label: string }[];
  hidden: string[];
  onToggle: (id: string) => void;
  onReorder?: (sourceId: string, targetId: string, place: DragPlace) => void;
  onReset: () => void;
  label: string;
  resetLabel: string;
  moveLeftLabel?: string;
  moveRightLabel?: string;
  extra?: ReactNode;
}>) {
  const [anchorEl, setAnchorEl] = useState<null | HTMLElement>(null);
  const open = Boolean(anchorEl);
  // Vertical drag-and-drop reorder of the menu rows (same onReorder path as the
  // ←/→ buttons and the header drag). No-op handler when reordering is disabled.
  const { dragProps, draggingId, overId, overPlace } = useColumnDrag(
    onReorder ?? (() => {}),
    'vertical',
  );
  return (
    <>
      <Tooltip title={label}>
        <IconButton
          size="small"
          aria-label={label}
          aria-haspopup="menu"
          aria-expanded={open ? 'true' : undefined}
          onClick={(e: MouseEvent<HTMLElement>) => setAnchorEl(e.currentTarget)}
        >
          <ViewColumnIcon fontSize="small" />
        </IconButton>
      </Tooltip>
      <Menu anchorEl={anchorEl} open={open} onClose={() => setAnchorEl(null)}>
        <Box
          sx={{
            px: 2,
            py: 0.5,
            maxHeight: 360,
            overflowY: 'auto',
            display: 'flex',
            flexDirection: 'column',
          }}
        >
          {columns.map((col, index) => (
            <Box
              key={col.id}
              {...(onReorder ? dragProps(col.id) : {})}
              sx={[
                { display: 'flex', alignItems: 'center', gap: 0.5 },
                onReorder ? columnDragSx(col.id, draggingId, overId, overPlace, 'vertical') : {},
              ]}
            >
              <FormControlLabel
                sx={{ flexGrow: 1, mr: 0 }}
                control={
                  <Checkbox
                    size="small"
                    checked={!hidden.includes(col.id)}
                    onChange={() => onToggle(col.id)}
                  />
                }
                label={col.label}
              />
              {onReorder && (
                <>
                  <Tooltip title={moveLeftLabel ?? ''}>
                    <span>
                      <IconButton
                        size="small"
                        aria-label={`${moveLeftLabel ?? 'move left'}: ${col.label}`}
                        disabled={index === 0}
                        onClick={() => onReorder(col.id, columns[index - 1].id, 'before')}
                      >
                        <ChevronLeftIcon fontSize="small" />
                      </IconButton>
                    </span>
                  </Tooltip>
                  <Tooltip title={moveRightLabel ?? ''}>
                    <span>
                      <IconButton
                        size="small"
                        aria-label={`${moveRightLabel ?? 'move right'}: ${col.label}`}
                        disabled={index === columns.length - 1}
                        onClick={() => onReorder(col.id, columns[index + 1].id, 'after')}
                      >
                        <ChevronRightIcon fontSize="small" />
                      </IconButton>
                    </span>
                  </Tooltip>
                </>
              )}
            </Box>
          ))}
        </Box>
        {extra && (
          <>
            <Divider />
            <Box sx={{ px: 2, py: 1 }}>{extra}</Box>
          </>
        )}
        <Divider />
        <Box sx={{ px: 2, py: 1 }}>
          <Button size="small" onClick={onReset}>
            {resetLabel}
          </Button>
        </Box>
      </Menu>
    </>
  );
}
