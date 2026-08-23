// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

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
  Radio,
  RadioGroup,
  Tooltip,
} from '@mui/material';
import ChevronLeftIcon from '@mui/icons-material/ChevronLeft';
import ChevronRightIcon from '@mui/icons-material/ChevronRight';
import type { Translation } from './shared/types';
import type { ColumnDef, ColumnId } from './activityColumns';
import { useColumnDrag, columnDragSx, type DragPlace } from './shared/columnDrag';

export function ColumnMenu({
  t,
  open,
  anchorEl,
  onClose,
  columns,
  hidden,
  onToggle,
  onReorder,
  onReset,
  scope,
  ownerDisplay,
  onOwnerDisplayChange,
  timeDisplay,
  onTimeDisplayChange,
  moveLeftLabel,
  moveRightLabel,
}: Readonly<{
  t: Translation;
  open: boolean;
  anchorEl: HTMLElement | null;
  onClose: () => void;
  columns: ColumnDef[];
  hidden: ColumnId[];
  onToggle: (id: ColumnId) => void;
  onReorder: (sourceId: string, targetId: string, place: DragPlace) => void;
  onReset: () => void;
  scope: 'own' | 'all' | 'user';
  ownerDisplay: 'name' | 'id';
  onOwnerDisplayChange: (value: 'name' | 'id') => void;
  timeDisplay: 'absolute' | 'relative';
  onTimeDisplayChange: (value: 'absolute' | 'relative') => void;
  moveLeftLabel: string;
  moveRightLabel: string;
}>) {
  const { dragProps, draggingId, overId, overPlace } = useColumnDrag(onReorder, 'vertical');
  return (
    <Menu open={open} anchorEl={anchorEl} onClose={onClose}>
      {columns.map((col, index) => (
        <MenuItem
          key={col.id}
          disableRipple
          {...dragProps(col.id)}
          sx={[
            { py: 0, display: 'flex', gap: 0.5 },
            columnDragSx(col.id, draggingId, overId, overPlace, 'vertical'),
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
            label={t[col.labelKey]}
          />
          <Tooltip title={moveLeftLabel}>
            <span>
              <IconButton
                size="small"
                aria-label={`${moveLeftLabel}: ${t[col.labelKey]}`}
                disabled={index === 0}
                onClick={() => onReorder(col.id, columns[index - 1].id, 'before')}
              >
                <ChevronLeftIcon fontSize="small" />
              </IconButton>
            </span>
          </Tooltip>
          <Tooltip title={moveRightLabel}>
            <span>
              <IconButton
                size="small"
                aria-label={`${moveRightLabel}: ${t[col.labelKey]}`}
                disabled={index === columns.length - 1}
                onClick={() => onReorder(col.id, columns[index + 1].id, 'after')}
              >
                <ChevronRightIcon fontSize="small" />
              </IconButton>
            </span>
          </Tooltip>
        </MenuItem>
      ))}

      <Divider key="time-divider" />
      <MenuItem key="time-display" disableRipple sx={{ display: 'block' }}>
        <FormLabel sx={{ fontSize: 13 }}>{t.activityTimeDisplayLabel}</FormLabel>
        <RadioGroup
          value={timeDisplay}
          onChange={(_event, value) =>
            onTimeDisplayChange(value === 'relative' ? 'relative' : 'absolute')
          }
        >
          <FormControlLabel
            value="absolute"
            control={<Radio size="small" />}
            label={t.activityTimeAbsolute}
          />
          <FormControlLabel
            value="relative"
            control={<Radio size="small" />}
            label={t.activityTimeRelative}
          />
        </RadioGroup>
      </MenuItem>

      {scope === 'all' && [
        <Divider key="owner-divider" />,
        <MenuItem key="owner-toggle" disableRipple sx={{ display: 'block' }}>
          <FormLabel sx={{ fontSize: 13 }}>{t.activityOwnerDisplayLabel}</FormLabel>
          <RadioGroup
            row
            value={ownerDisplay}
            onChange={(_event, value) => onOwnerDisplayChange(value === 'id' ? 'id' : 'name')}
          >
            <FormControlLabel
              value="name"
              control={<Radio size="small" />}
              label={t.activityOwnerName}
            />
            <FormControlLabel
              value="id"
              control={<Radio size="small" />}
              label={t.activityOwnerId}
            />
          </RadioGroup>
        </MenuItem>,
      ]}

      <Divider key="reset-divider" />
      <Box key="reset" sx={{ px: 2, py: 1 }}>
        <Button size="small" onClick={onReset}>
          {t.listColumnsReset}
        </Button>
      </Box>
    </Menu>
  );
}
