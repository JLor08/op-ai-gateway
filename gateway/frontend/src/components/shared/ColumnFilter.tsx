// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useState, type MouseEvent } from 'react';
import {
  Box,
  Button,
  Checkbox,
  FormControlLabel,
  IconButton,
  Popover,
  TextField,
  Tooltip,
} from '@mui/material';
import FilterListIcon from '@mui/icons-material/FilterList';

/** Per-column filter value:
 *  - text: a substring
 *  - enum: a set of allowed values
 *  - numeric: an optional min (>=) and/or max (<=)  [both empty = no filter]
 *  - datetime: an optional from/to (datetime-local strings) */
export type ColumnFilterState =
  | { type: 'text'; value: string }
  | { type: 'enum'; selected: string[] }
  | { type: 'numeric'; min: string; max: string }
  | { type: 'datetime'; from: string; to: string };

export function isFilterActive(state: ColumnFilterState): boolean {
  switch (state.type) {
    case 'text':
      return state.value.trim() !== '';
    case 'enum':
      return state.selected.length > 0;
    case 'numeric':
      return state.min.trim() !== '' || state.max.trim() !== '';
    case 'datetime':
      return state.from.trim() !== '' || state.to.trim() !== '';
  }
}

function emptyState(state: ColumnFilterState): ColumnFilterState {
  switch (state.type) {
    case 'text':
      return { type: 'text', value: '' };
    case 'enum':
      return { type: 'enum', selected: [] };
    case 'numeric':
      return { type: 'numeric', min: '', max: '' };
    case 'datetime':
      return { type: 'datetime', from: '', to: '' };
  }
}

/**
 * The filter icon in a table column header. Opens a popover whose contents match
 * the filter type: a "contains" text field, a value checklist (enum), a min/max
 * number pair (numeric: greater-than / less-than / range), or a from/to datetime
 * pair. The icon is highlighted while a filter is set. Fully controlled.
 */
export function ColumnFilter({
  columnLabel,
  filterLabel,
  clearLabel,
  state,
  options,
  onChange,
  minLabel,
  maxLabel,
  fromLabel,
  toLabel,
}: Readonly<{
  columnLabel: string;
  filterLabel: string;
  clearLabel: string;
  state: ColumnFilterState;
  /** enum options (value + display label); ignored for other filter types. */
  options?: { value: string; label: string }[];
  onChange: (next: ColumnFilterState) => void;
  minLabel?: string;
  maxLabel?: string;
  fromLabel?: string;
  toLabel?: string;
}>) {
  const [anchorEl, setAnchorEl] = useState<null | HTMLElement>(null);
  const open = Boolean(anchorEl);
  const active = isFilterActive(state);
  const title = `${filterLabel}: ${columnLabel}`;
  return (
    <>
      <Tooltip title={title}>
        <IconButton
          size="small"
          aria-label={title}
          color={active ? 'primary' : 'default'}
          onClick={(e: MouseEvent<HTMLElement>) => setAnchorEl(e.currentTarget)}
        >
          <FilterListIcon fontSize="small" />
        </IconButton>
      </Tooltip>
      <Popover
        anchorEl={anchorEl}
        open={open}
        onClose={() => setAnchorEl(null)}
        anchorOrigin={{ vertical: 'bottom', horizontal: 'left' }}
      >
        <Box sx={{ p: 1.5, minWidth: 220, display: 'flex', flexDirection: 'column', gap: 1 }}>
          {state.type === 'text' && (
            <TextField
              size="small"
              autoFocus
              label={columnLabel}
              value={state.value}
              onChange={(e) => onChange({ type: 'text', value: e.target.value })}
              slotProps={{ htmlInput: { 'aria-label': title } }}
            />
          )}
          {state.type === 'enum' && (
            <Box sx={{ display: 'flex', flexDirection: 'column' }}>
              {(options ?? []).map((opt) => (
                <FormControlLabel
                  key={opt.value}
                  control={
                    <Checkbox
                      size="small"
                      checked={state.selected.includes(opt.value)}
                      onChange={() =>
                        onChange({
                          type: 'enum',
                          selected: state.selected.includes(opt.value)
                            ? state.selected.filter((v) => v !== opt.value)
                            : [...state.selected, opt.value],
                        })
                      }
                    />
                  }
                  label={opt.label}
                />
              ))}
            </Box>
          )}
          {state.type === 'numeric' && (
            <Box sx={{ display: 'flex', gap: 1 }}>
              <TextField
                size="small"
                type="number"
                label={minLabel}
                value={state.min}
                onChange={(e) => onChange({ type: 'numeric', min: e.target.value, max: state.max })}
                slotProps={{ htmlInput: { 'aria-label': `${title} ${minLabel ?? ''}`.trim() } }}
              />
              <TextField
                size="small"
                type="number"
                label={maxLabel}
                value={state.max}
                onChange={(e) => onChange({ type: 'numeric', min: state.min, max: e.target.value })}
                slotProps={{ htmlInput: { 'aria-label': `${title} ${maxLabel ?? ''}`.trim() } }}
              />
            </Box>
          )}
          {state.type === 'datetime' && (
            <Box sx={{ display: 'flex', flexDirection: 'column', gap: 1 }}>
              <TextField
                size="small"
                type="datetime-local"
                label={fromLabel}
                value={state.from}
                onChange={(e) => onChange({ type: 'datetime', from: e.target.value, to: state.to })}
                slotProps={{
                  inputLabel: { shrink: true },
                  htmlInput: { 'aria-label': `${title} ${fromLabel ?? ''}`.trim() },
                }}
              />
              <TextField
                size="small"
                type="datetime-local"
                label={toLabel}
                value={state.to}
                onChange={(e) =>
                  onChange({ type: 'datetime', from: state.from, to: e.target.value })
                }
                slotProps={{
                  inputLabel: { shrink: true },
                  htmlInput: { 'aria-label': `${title} ${toLabel ?? ''}`.trim() },
                }}
              />
            </Box>
          )}
          <Button size="small" disabled={!active} onClick={() => onChange(emptyState(state))}>
            {clearLabel}
          </Button>
        </Box>
      </Popover>
    </>
  );
}
