// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { type MouseEvent, type ReactNode } from 'react';
import {
  Box,
  Chip,
  IconButton,
  InputAdornment,
  Paper,
  Table,
  TableBody,
  TableCell,
  TableContainer,
  TableHead,
  TablePagination,
  TableRow,
  TableSortLabel,
  TextField,
  Tooltip,
} from '@mui/material';
import SearchIcon from '@mui/icons-material/Search';
import ViewColumnIcon from '@mui/icons-material/ViewColumn';
import { Eye, Lock } from 'lucide-react';
import type { UsageEvent } from '../api';
import { formatCost, type CurrencyUnit } from '../currency';
import { formatMetric } from './shared/format';
import type { Translation } from './shared/types';
import { IconAction } from './shared/IconAction';
import { StatusChip } from './shared/StatusChip';
import { usageChipStatus } from './shared/status';
import { ColumnFilter } from './shared/ColumnFilter';
import { useColumnDrag, columnDragSx, type DragPlace } from './shared/columnDrag';
import type { ColumnDef, ColumnId } from './activityColumns';
import {
  BILLING_UNIT_HELP_KEYS,
  ENERGY_SOURCE_HELP_KEYS,
  isTokenMetered,
  type HelpMessageKey,
} from './billingUnit';

const LIMIT_OPTIONS = [25, 50, 100];

// Columns that support a numeric min/max (>=, <=, or range) filter server-side.
const NUMERIC_FILTER_COLUMNS = new Set<ColumnId>([
  'total_tokens',
  'latency_ms',
  'input_tokens',
  'output_tokens',
  'prompt_per_second',
  'tokens_per_second',
  'cached_tokens',
  'cache_write_tokens',
  'energy_wh',
  'energy_marginal_wh',
  'cost_eur',
]);

// Text-filter columns (substring, server-side).
const TEXT_FILTER_COLUMNS = new Set<ColumnId>([
  'owner',
  'model',
  'server_name',
  'req_path',
  'content_type',
  'provider_path',
  'provider_model',
  'session',
  'agent_id',
  'service_name',
]);

// A token count on a row that is not token-metered is NOT APPLICABLE, not zero.
// The em dash carries a tooltip, so the reason is one hover away rather than a
// mystery.
//
// renderCell and these two helpers are module-level and therefore do NOT close
// over the component's `t`; it is threaded through as an explicit parameter
// instead. A module-level mutable would be the alternative, and it would make
// the rendered language depend on call order.
function tokenCountCell(row: UsageEvent, value: number, t: Translation): ReactNode {
  if (isTokenMetered(row)) return value;
  return (
    <Tooltip title={t.activityNotTokenMetered}>
      <span>—</span>
    </Tooltip>
  );
}

// An opaque wire enum, rendered as a chip with its RAW value plus an explanatory
// tooltip. The raw label is the portal's forward-compatibility convention: an
// unrecognised value from a newer backend must show as itself rather than as a
// misleading label -- and it gets NO tooltip, because a wrong explanation is
// worse than none.
function chipWithHelp(
  value: string,
  helpKeys: Record<string, HelpMessageKey>,
  t: Translation,
): ReactNode {
  const body = value ? <Chip size="small" variant="outlined" label={value} /> : <span>—</span>;
  const helpKey = helpKeys[value];
  // No cast on this lookup: HelpMessageKey is derived from the keys of
  // Translation, so a missing or misspelled key fails the build here instead of
  // silently rendering nothing.
  //
  // The lookup CAN be undefined at runtime even though its type says otherwise:
  // tsconfig has `strict` but not `noUncheckedIndexedAccess`, so an index into a
  // Record<string, ...> is typed as if it always hit. An unrecognised wire value
  // (a newer backend's energy_source, any billing_unit outside the two known
  // ones) yields undefined here. `billingUnit.test.ts` pins that lookup.
  //
  // The early return below is a SIMPLIFICATION, not a correctness guard: MUI's
  // Tooltip already suppresses itself on a falsy `title`, so deleting it would
  // render the same DOM -- it only avoids wrapping `body` in a Tooltip that
  // would do nothing. What actually holds the "an unknown value gets NO tooltip"
  // rule is the test: ActivityTable.test.tsx hovers an unrecognised
  // energy_source and waits past MUI's enter delay before asserting the absence,
  // so inventing a fallback tooltip here fails it.
  if (!helpKey) return body;
  return <Tooltip title={t[helpKey]}>{body}</Tooltip>;
}

function renderCell(row: UsageEvent, id: ColumnId, t: Translation): ReactNode {
  switch (id) {
    case 'created_at':
      return new Date(row.created_at).toLocaleString();
    case 'requested_model':
      return row.requested_model || '—';
    case 'model':
      return row.model;
    case 'server_name':
      return row.server_name || row.host;
    case 'http_status':
      return (
        <StatusChip
          status={usageChipStatus(row.status, row.http_status)}
          label={String(row.http_status)}
        />
      );
    case 'total_tokens':
      return tokenCountCell(row, row.total_tokens, t);
    case 'latency_ms':
      return `${row.latency_ms} ms`;
    case 'input_tokens':
      return tokenCountCell(row, row.input_tokens, t);
    case 'output_tokens':
      return tokenCountCell(row, row.output_tokens, t);
    case 'prompt_per_second':
      return formatMetric(row.prompt_per_second, 1);
    case 'tokens_per_second':
      return formatMetric(row.tokens_per_second, 1);
    case 'req_path':
      return row.req_path;
    case 'content_type':
      return row.content_type;
    case 'provider_path':
      return row.provider_path;
    case 'cached_tokens':
      return tokenCountCell(row, row.cached_tokens, t);
    case 'cache_write_tokens':
      return tokenCountCell(row, row.cache_write_tokens, t);
    case 'stream':
      return row.stream ? '✓' : '–';
    case 'provider_model':
      return row.provider_model;
    case 'energy_wh':
      return row.energy_wh ? row.energy_wh.toFixed(3) : '—';
    case 'energy_marginal_wh':
      return row.energy_marginal_wh ? row.energy_marginal_wh.toFixed(3) : '—';
    case 'energy_source':
      return chipWithHelp(row.energy_source ?? '', ENERGY_SOURCE_HELP_KEYS, t);
    case 'billing_unit':
      return chipWithHelp(row.billing_unit ?? '', BILLING_UNIT_HELP_KEYS, t);
    case 'billing_quantity':
      // Guarded on the UNIT, not on the value -- unlike the energy cells above,
      // and deliberately so. This column IS the measure for a non-token row, so
      // a quantity of 0 is a measurement (every FAILED image request has one)
      // and must read as `0`; only a token-metered row, where the quantity does
      // not apply at all, gets the em dash. Same rule as tokenCountCell, applied
      // in the other direction.
      return isTokenMetered(row) ? '—' : row.billing_quantity;
    case 'session':
      return row.session_id ? (
        <span title={row.session_id}>
          <Chip size="small" variant="outlined" label={row.session_source || '?'} />{' '}
          {row.session_id.length > 12 ? row.session_id.slice(0, 12) + '…' : row.session_id}
        </span>
      ) : (
        '—'
      );
    case 'agent_id':
      return row.agent_id || '—';
    case 'service_name':
      return row.service_name || '—';
    default:
      return null;
  }
}

export function ActivityTable({
  t,
  rows,
  columns,
  ownerDisplay,
  sort,
  order,
  onSort,
  page,
  limit,
  total,
  onPageChange,
  onLimitChange,
  isEmpty,
  emptyLabel,
  onView,
  q,
  onQChange,
  textFilters,
  onTextFilter,
  filterStatus,
  onFilterStatus,
  filterStream,
  onFilterStream,
  numericFilters,
  onNumericFilter,
  timeFrom,
  timeTo,
  onTimeFilter,
  timeDisplay,
  costUnit,
  currencyFactor,
  onOpenColumns,
  onReorderColumn,
}: Readonly<{
  t: Translation;
  rows: UsageEvent[];
  columns: ColumnDef[];
  ownerDisplay: 'name' | 'id';
  sort: string;
  order: 'asc' | 'desc';
  onSort: (columnId: string) => void;
  page: number; // 1-based
  limit: number;
  total: number;
  onPageChange: (page: number) => void; // 1-based
  onLimitChange: (limit: number) => void;
  isEmpty: boolean;
  emptyLabel: string;
  onView: (row: UsageEvent) => void;
  // Backend-driven search + per-column filters (the table stays server-side).
  q: string;
  onQChange: (value: string) => void;
  // Text substring filters keyed by column id ("owner" for the owner column).
  textFilters: Record<string, string>;
  onTextFilter: (key: string, value: string) => void;
  filterStatus: '' | 'success' | 'error';
  onFilterStatus: (value: '' | 'success' | 'error') => void;
  filterStream: '' | 'true' | 'false';
  onFilterStream: (value: '' | 'true' | 'false') => void;
  // Per-column numeric min/max (keyed by column id) + a created_at from/to range.
  numericFilters: Record<string, { min: string; max: string }>;
  onNumericFilter: (columnId: string, next: { min: string; max: string }) => void;
  timeFrom: string;
  timeTo: string;
  onTimeFilter: (next: { from: string; to: string }) => void;
  timeDisplay: 'absolute' | 'relative';
  // Cost-unit display, driven by the Activity toolbar selector (mirrors
  // timeDisplay): which currency unit the cost_eur column renders in, and the
  // USD-per-EUR factor formatCost needs to convert (0 => USD unreachable).
  costUnit: CurrencyUnit;
  currencyFactor: number;
  onOpenColumns: (event: MouseEvent<HTMLElement>) => void;
  // Reorder the data columns (the trailing view/Ansicht column stays fixed).
  onReorderColumn: (sourceId: string, targetId: string, place: DragPlace) => void;
}>) {
  // +1 for the trailing view/Ansicht action column. Owner is a normal column now
  // (present in `columns` when the all-scope view includes it).
  const colCount = columns.length + 1;
  const { dragProps, draggingId, overId, overPlace } = useColumnDrag(onReorderColumn);

  function timeCell(iso: string): string {
    if (timeDisplay === 'relative')
      return t.activityRelativeTime((Date.now() - new Date(iso).getTime()) / 1000);
    return new Date(iso).toLocaleString();
  }

  function ownerCell(row: UsageEvent): string {
    return ownerDisplay === 'name' ? row.user_name || row.user_id : row.user_id;
  }

  // Token column: a token-less chat request has an empty token_id (its
  // token_name is the user's display name), so show the "no token" label
  // instead — matching the running-connections panel and the §5 filter option.
  function tokenCell(row: UsageEvent): string {
    return row.token_id ? row.token_name : t.activityActiveSession;
  }

  // A handful of columns need per-row/per-view formatting (relative time,
  // owner-display toggle, no-token label, currency) instead of the plain
  // per-column-id switch in module-level renderCell().
  function cellContent(row: UsageEvent, col: ColumnDef): ReactNode {
    switch (col.id) {
      case 'created_at':
        return timeCell(row.created_at);
      case 'owner':
        return ownerCell(row);
      case 'token_name':
        return tokenCell(row);
      case 'cost_eur':
        return formatCost(row.cost_eur, costUnit, currencyFactor);
      default:
        return renderCell(row, col.id, t);
    }
  }

  function captureCell(row: UsageEvent): ReactNode {
    if (row.has_capture) {
      return (
        <IconAction
          label={t.activityColView}
          icon={<Eye size={16} />}
          onClick={() => onView(row)}
        />
      );
    }
    if (row.capture_locked) {
      return (
        <Tooltip title={t.captureLocked}>
          <Box
            component="span"
            aria-label={t.captureLocked}
            sx={{ display: 'inline-flex', color: 'text.disabled' }}
          >
            <Lock size={16} />
          </Box>
        </Tooltip>
      );
    }
    return null;
  }

  // Per-column filter icon → real backend param (text substring, status/stream
  // enum, numeric min/max, created_at range). Other columns have no server-side
  // filter, so no icon.
  function headerFilter(col: ColumnDef): ReactNode {
    if (TEXT_FILTER_COLUMNS.has(col.id)) {
      return (
        <ColumnFilter
          columnLabel={t[col.labelKey]}
          filterLabel={t.listFilter}
          clearLabel={t.listFilterClear}
          state={{ type: 'text', value: textFilters[col.id] ?? '' }}
          onChange={(next) => onTextFilter(col.id, next.type === 'text' ? next.value : '')}
        />
      );
    }
    if (col.id === 'stream') {
      return (
        <ColumnFilter
          columnLabel={t[col.labelKey]}
          filterLabel={t.listFilter}
          clearLabel={t.listFilterClear}
          state={{ type: 'enum', selected: filterStream ? [filterStream] : [] }}
          options={[
            { value: 'true', label: t.yes },
            { value: 'false', label: t.no },
          ]}
          // The backend stream param is single-valued: one selected -> that
          // value; none or both -> no filter.
          onChange={(next) => {
            if (next.type !== 'enum') return;
            onFilterStream(
              next.selected.length === 1 ? (next.selected[0] as 'true' | 'false') : '',
            );
          }}
        />
      );
    }
    if (col.id === 'http_status') {
      return (
        <ColumnFilter
          columnLabel={t[col.labelKey]}
          filterLabel={t.listFilter}
          clearLabel={t.listFilterClear}
          state={{ type: 'enum', selected: filterStatus ? [filterStatus] : [] }}
          options={[
            { value: 'success', label: t.statusSuccess },
            { value: 'error', label: t.statusError },
          ]}
          // The backend status param is single-valued: one selected -> that
          // value; none or both -> no filter.
          onChange={(next) => {
            if (next.type !== 'enum') return;
            onFilterStatus(
              next.selected.length === 1 ? (next.selected[0] as 'success' | 'error') : '',
            );
          }}
        />
      );
    }
    if (col.id === 'created_at') {
      return (
        <ColumnFilter
          columnLabel={t[col.labelKey]}
          filterLabel={t.listFilter}
          clearLabel={t.listFilterClear}
          fromLabel={t.listFilterFrom}
          toLabel={t.listFilterTo}
          state={{ type: 'datetime', from: timeFrom, to: timeTo }}
          onChange={(next) => {
            if (next.type === 'datetime') onTimeFilter({ from: next.from, to: next.to });
          }}
        />
      );
    }
    if (NUMERIC_FILTER_COLUMNS.has(col.id)) {
      const nf = numericFilters[col.id] ?? { min: '', max: '' };
      return (
        <ColumnFilter
          columnLabel={t[col.labelKey]}
          filterLabel={t.listFilter}
          clearLabel={t.listFilterClear}
          minLabel={t.listFilterMin}
          maxLabel={t.listFilterMax}
          state={{ type: 'numeric', min: nf.min, max: nf.max }}
          onChange={(next) => {
            if (next.type === 'numeric') onNumericFilter(col.id, { min: next.min, max: next.max });
          }}
        />
      );
    }
    return null;
  }

  return (
    <>
      <Box sx={{ mb: 2, display: 'flex', alignItems: 'center', gap: 1 }}>
        <TextField
          size="small"
          placeholder={t.listSearchPlaceholder}
          value={q}
          onChange={(event) => onQChange(event.target.value)}
          slotProps={{
            input: {
              startAdornment: (
                <InputAdornment position="start">
                  <SearchIcon fontSize="small" />
                </InputAdornment>
              ),
            },
            htmlInput: { 'aria-label': t.activitySearchLabel },
          }}
          sx={{ flexGrow: 1, maxWidth: 380 }}
        />
        <Box sx={{ flexGrow: 1 }} />
        <Tooltip title={t.listColumns}>
          <IconButton size="small" aria-label={t.listColumns} onClick={onOpenColumns}>
            <ViewColumnIcon fontSize="small" />
          </IconButton>
        </Tooltip>
      </Box>
      <TableContainer
        component={Paper}
        variant="outlined"
        sx={{
          overflowX: 'auto',
          borderTop: '4px solid transparent',
          borderImage: 'linear-gradient(90deg, var(--brand-accent), var(--brand-primary)) 1',
        }}
      >
        <Table size="small" sx={{ minWidth: 720 }}>
          <TableHead>
            <TableRow>
              {columns.map((col) => (
                <TableCell
                  key={col.id}
                  align={col.numeric ? 'right' : 'left'}
                  sortDirection={sort === col.id ? order : false}
                  {...dragProps(col.id)}
                  sx={columnDragSx(col.id, draggingId, overId, overPlace)}
                >
                  <Box sx={{ display: 'inline-flex', alignItems: 'center', gap: 0.25 }}>
                    {col.sortable ? (
                      <TableSortLabel
                        active={sort === col.id}
                        direction={sort === col.id ? order : 'asc'}
                        onClick={() => onSort(col.id)}
                      >
                        {t[col.labelKey]}
                      </TableSortLabel>
                    ) : (
                      t[col.labelKey]
                    )}
                    {headerFilter(col)}
                  </Box>
                </TableCell>
              ))}
              <TableCell align="right">{t.activityColView}</TableCell>
            </TableRow>
          </TableHead>
          <TableBody>
            {isEmpty && (
              <TableRow>
                <TableCell colSpan={colCount}>{emptyLabel}</TableCell>
              </TableRow>
            )}
            {rows.map((row) => (
              <TableRow key={row.id}>
                {columns.map((col) => (
                  <TableCell key={col.id} align={col.numeric ? 'right' : 'left'}>
                    {cellContent(row, col)}
                  </TableCell>
                ))}
                <TableCell align="right">{captureCell(row)}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </TableContainer>
      <TablePagination
        component="div"
        count={total}
        page={page - 1}
        rowsPerPage={limit}
        rowsPerPageOptions={LIMIT_OPTIONS}
        onPageChange={(_event, next) => onPageChange(next + 1)}
        onRowsPerPageChange={(event) => onLimitChange(Number(event.target.value))}
        labelRowsPerPage={t.activityRowsPerPage}
        labelDisplayedRows={({ from, to, count }) => `${from}–${to} ${t.activityOf} ${count}`}
        getItemAriaLabel={(type) => {
          if (type === 'next') return t.activityNextPage;
          if (type === 'previous') return t.activityPrevPage;
          return type;
        }}
        slotProps={{
          select: { native: true, inputProps: { 'aria-label': t.activityRowsPerPage } },
        }}
      />
    </>
  );
}
