// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useEffect, useMemo, useState, type ReactNode } from 'react';
import {
  Box,
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
} from '@mui/material';
import SearchIcon from '@mui/icons-material/Search';
import { ColumnFilter, type ColumnFilterState } from './ColumnFilter';
import { ColumnVisibilityMenu } from './ColumnVisibilityMenu';
import { RowActionsCell } from './RowActionsCell';
import type { RowAction } from './RowActionsMenu';
import { useColumnDrag, columnDragSx } from './columnDrag';
import { usePreference } from './preferences';
import { useColumnSettings } from './useColumnSettings';
import type { Translation } from './types';

/** Column definition for a client-side ListTable. */
export type ListColumn<Row> = {
  id: string;
  label: string;
  /** Canonical string used for search, sorting and filtering. */
  value: (row: Row) => string;
  /** Cell content; defaults to `value(row)`. */
  render?: (row: Row) => ReactNode;
  /** Per-column filter kind; omit for no filter. */
  filter?: 'text' | 'enum';
  /** Default true. */
  sortable?: boolean;
  /** Include in the global search box; default true. */
  searchable?: boolean;
  /** Maps an enum value to its checklist display label. */
  enumLabel?: (value: string) => string;
  /** Right-aligns the column. */
  numeric?: boolean;
  /** Hidden by default (still toggle-able via the column menu). */
  defaultHidden?: boolean;
  /** When false, the column is unavailable in the current context (e.g. scope):
      not rendered, not in the column menu, not a drag target — but its id stays
      in the persisted order so it returns to position when it becomes available. */
  available?: boolean;
};

export type ListTableLabels = {
  searchPlaceholder: string;
  rowsPerPage: string;
  rangeOf: string;
  rowsSuffix: string;
  filter: string;
  filterClear: string;
  rowMenu: string;
  /** Text shown when there are genuinely no rows (data loaded, set empty). */
  empty: string;
  /** Text shown while the data is still loading. Falls back to `empty` when
      unset; only meaningful together with the `loading` prop. */
  loading?: string;
  columns: string;
  columnsReset: string;
  columnMoveLeft: string;
  columnMoveRight: string;
};

/**
 * Builds the standard `ListTable` labels blob from the translation table.
 * Every list view shares the same 12 base keys; pass `overrides` for the
 * per-view `empty` text (almost always needed) and, occasionally, `loading`.
 */
export function listTableLabels(
  t: Translation,
  overrides?: Partial<ListTableLabels>,
): ListTableLabels {
  return {
    searchPlaceholder: t.listSearchPlaceholder,
    rowsPerPage: t.listRowsPerPage,
    rangeOf: t.listRangeOf,
    rowsSuffix: t.listRowsSuffix,
    filter: t.listFilter,
    filterClear: t.listFilterClear,
    rowMenu: t.listRowMenu,
    empty: t.listEmpty,
    loading: t.loading,
    columns: t.listColumns,
    columnsReset: t.listColumnsReset,
    columnMoveLeft: t.listColumnMoveLeft,
    columnMoveRight: t.listColumnMoveRight,
    ...overrides,
  };
}

type SortState = { id: string | null; order: 'asc' | 'desc' };

const ROWS_PER_PAGE_OPTIONS = [10, 25, 50];

/**
 * Generic, config-driven list table: global search + per-column filters +
 * client-side sorting + pagination + a per-row "…" actions menu, over an
 * already-loaded `rows` array. Column order, visibility, sort and rows-per-page
 * persist at the user profile (keyed by `storageKey`); search and column filters
 * are transient.
 */
export function ListTable<Row>({
  rows,
  columns,
  rowKey,
  actions,
  labels,
  storageKey,
  minWidth = 680,
  maxInlineActions = 4,
  loading = false,
}: Readonly<{
  rows: Row[];
  columns: ListColumn<Row>[];
  rowKey: (row: Row) => string;
  actions?: (row: Row) => RowAction[];
  labels: ListTableLabels;
  storageKey?: string;
  minWidth?: number;
  /** Row actions render as inline icons when there are at most this many;
      otherwise they collapse into a "…" kebab menu. */
  maxInlineActions?: number;
  /** When true and there are no rows, show `labels.loading` instead of
      `labels.empty` — distinguishes an in-flight fetch from a truly empty set. */
  loading?: boolean;
}>) {
  const baseKey = `table.${storageKey ?? 'list'}`;
  const catalogue = useMemo(() => columns.map((c) => c.id), [columns]);
  const defaultHidden = useMemo(
    () => columns.filter((c) => c.defaultHidden).map((c) => c.id),
    [columns],
  );
  // Column order/visibility persist at the user profile via the shared
  // useColumnSettings hook (hidden/order reconciled against `catalogue` at read
  // time — a corrupt/foreign profile or mirror entry, or an unknown/renamed id,
  // must never crash the table). Sort + rows-per-page are separate preferences,
  // below.
  const {
    order,
    hidden,
    toggle: toggleColumn,
    reorder,
    reset: resetColumns,
  } = useColumnSettings<string>(baseKey, catalogue, defaultHidden);
  const [storedSort, setSort] = usePreference<SortState>(`${baseKey}.sort`, {
    id: null,
    order: 'asc',
  });
  const sort: SortState = useMemo(
    () =>
      storedSort && typeof storedSort === 'object' && 'id' in storedSort && 'order' in storedSort
        ? storedSort
        : { id: null, order: 'asc' },
    [storedSort],
  );
  const [storedRpp, setRowsPerPage] = usePreference<number>(`${baseKey}.rpp`, 10);
  const rowsPerPage = typeof storedRpp === 'number' && storedRpp > 0 ? storedRpp : 10;

  const [search, setSearch] = useState('');
  const [page, setPage] = useState(0);
  const [filters, setFilters] = useState<Record<string, ColumnFilterState>>(() => {
    const init: Record<string, ColumnFilterState> = {};
    for (const col of columns) {
      if (col.filter === 'text') init[col.id] = { type: 'text', value: '' };
      else if (col.filter === 'enum') init[col.id] = { type: 'enum', selected: [] };
    }
    return init;
  });

  // Columns in the persisted order (unknown ids dropped), then filtered by
  // visibility. Header/body iterate this, so the drag order drives layout.
  // Ordered columns available in the current context (order preserved even for
  // unavailable ones, which are simply skipped from render/menu/drag).
  const orderedColumns = useMemo(() => {
    const byId = new Map(columns.map((c) => [c.id, c]));
    return order
      .map((id) => byId.get(id))
      .filter((c): c is ListColumn<Row> => Boolean(c) && c!.available !== false);
  }, [columns, order]);
  const visibleColumns = useMemo(
    () => orderedColumns.filter((c) => !hidden.includes(c.id)),
    [orderedColumns, hidden],
  );
  const { dragProps, draggingId, overId, overPlace } = useColumnDrag(reorder);
  const columnById = useMemo(
    () => Object.fromEntries(visibleColumns.map((c) => [c.id, c])),
    [visibleColumns],
  );

  // Distinct enum options per enum-filtered column, derived from the data.
  // Iterate ALL columns (not visibleColumns): options depend only on `rows`, so a
  // hidden enum column must keep its options ready for when it is shown again —
  // and this keeps the [columns, rows] dependency array correct.
  const enumOptions = useMemo(() => {
    const out: Record<string, { value: string; label: string }[]> = {};
    for (const col of columns) {
      if (col.filter !== 'enum') continue;
      const seen = new Set<string>();
      const values: string[] = [];
      for (const row of rows) {
        const v = col.value(row);
        if (v !== '' && !seen.has(v)) {
          seen.add(v);
          values.push(v);
        }
      }
      values.sort((a, b) => a.localeCompare(b));
      out[col.id] = values.map((v) => ({ value: v, label: col.enumLabel ? col.enumLabel(v) : v }));
    }
    return out;
  }, [columns, rows]);

  const filtered = useMemo(() => {
    const q = search.trim().toLowerCase();
    return rows.filter((row) => {
      if (q) {
        const hit = visibleColumns.some(
          (col) => col.searchable !== false && col.value(row).toLowerCase().includes(q),
        );
        if (!hit) return false;
      }
      for (const col of visibleColumns) {
        const state = filters[col.id];
        if (!state) continue;
        if (state.type === 'text') {
          const needle = state.value.trim().toLowerCase();
          if (needle && !col.value(row).toLowerCase().includes(needle)) return false;
        } else if (state.type === 'enum') {
          if (state.selected.length > 0 && !state.selected.includes(col.value(row))) return false;
        }
      }
      return true;
    });
  }, [rows, visibleColumns, search, filters]);

  const sorted = useMemo(() => {
    if (!sort.id) return filtered;
    const col = columnById[sort.id];
    if (!col) return filtered;
    const dir = sort.order === 'asc' ? 1 : -1;
    return [...filtered].sort((a, b) => {
      const av = col.value(a);
      const bv = col.value(b);
      if (col.numeric) {
        const an = Number(av);
        const bn = Number(bv);
        // Cells without a magnitude have no place on the number line: keep them
        // at the bottom in both directions instead of letting NaN comparisons
        // freeze them wherever they happened to sit. Blank counts as missing
        // too — Number('') is 0, which would rank a blank cell as the smallest
        // real value.
        const aMissing = av.trim() === '' || Number.isNaN(an);
        const bMissing = bv.trim() === '' || Number.isNaN(bn);
        if (aMissing || bMissing) return aMissing && bMissing ? 0 : aMissing ? 1 : -1;
        return (an - bn) * dir;
      }
      return av.localeCompare(bv) * dir;
    });
  }, [filtered, sort, columnById]);

  // Keep the page in range as the filtered set shrinks.
  const pageCount = Math.max(1, Math.ceil(sorted.length / rowsPerPage));
  const safePage = Math.min(page, pageCount - 1);
  useEffect(() => {
    if (safePage !== page) setPage(safePage);
  }, [safePage, page]);

  const paged = sorted.slice(safePage * rowsPerPage, safePage * rowsPerPage + rowsPerPage);

  function toggleSort(id: string) {
    // Derive from the validated `sort` (the raw persisted value may be malformed).
    if (sort.id !== id) setSort({ id, order: 'asc' });
    else if (sort.order === 'asc') setSort({ id, order: 'desc' });
    else setSort({ id: null, order: 'asc' });
  }

  function setFilter(id: string, next: ColumnFilterState) {
    setFilters((prev) => ({ ...prev, [id]: next }));
    setPage(0);
  }

  const totalCols = visibleColumns.length + (actions ? 1 : 0);

  return (
    <Box>
      <Box sx={{ mb: 2, display: 'flex', alignItems: 'center', gap: 1 }}>
        <TextField
          size="small"
          placeholder={labels.searchPlaceholder}
          value={search}
          onChange={(e) => {
            setSearch(e.target.value);
            setPage(0);
          }}
          slotProps={{
            input: {
              startAdornment: (
                <InputAdornment position="start">
                  <SearchIcon fontSize="small" />
                </InputAdornment>
              ),
            },
            htmlInput: { 'aria-label': labels.searchPlaceholder },
          }}
          sx={{ flexGrow: 1, maxWidth: 380 }}
        />
        <Box sx={{ flexGrow: 1 }} />
        <ColumnVisibilityMenu
          columns={orderedColumns.map((c) => ({ id: c.id, label: c.label }))}
          hidden={hidden}
          onToggle={toggleColumn}
          onReorder={reorder}
          onReset={resetColumns}
          label={labels.columns}
          resetLabel={labels.columnsReset}
          moveLeftLabel={labels.columnMoveLeft}
          moveRightLabel={labels.columnMoveRight}
        />
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
        <Table size="small" sx={{ minWidth }}>
          <TableHead>
            <TableRow>
              {visibleColumns.map((col) => (
                <TableCell
                  key={col.id}
                  align={col.numeric ? 'right' : 'left'}
                  sortDirection={sort.id === col.id ? sort.order : false}
                  {...dragProps(col.id)}
                  sx={columnDragSx(col.id, draggingId, overId, overPlace)}
                >
                  <Box sx={{ display: 'inline-flex', alignItems: 'center', gap: 0.25 }}>
                    {col.sortable === false ? (
                      col.label
                    ) : (
                      <TableSortLabel
                        active={sort.id === col.id}
                        direction={sort.id === col.id ? sort.order : 'asc'}
                        onClick={() => toggleSort(col.id)}
                      >
                        {col.label}
                      </TableSortLabel>
                    )}
                    {col.filter && filters[col.id] && (
                      <ColumnFilter
                        columnLabel={col.label}
                        filterLabel={labels.filter}
                        clearLabel={labels.filterClear}
                        state={filters[col.id]}
                        options={enumOptions[col.id]}
                        onChange={(next) => setFilter(col.id, next)}
                      />
                    )}
                  </Box>
                </TableCell>
              ))}
              {actions && <TableCell>{labels.rowMenu}</TableCell>}
            </TableRow>
          </TableHead>
          <TableBody>
            {sorted.length === 0 && (
              <TableRow>
                <TableCell colSpan={totalCols}>
                  {loading ? (labels.loading ?? labels.empty) : labels.empty}
                </TableCell>
              </TableRow>
            )}
            {paged.map((row) => (
              <TableRow key={rowKey(row)}>
                {visibleColumns.map((col) => (
                  <TableCell key={col.id} align={col.numeric ? 'right' : 'left'}>
                    {col.render ? col.render(row) : col.value(row)}
                  </TableCell>
                ))}
                {actions && (
                  <TableCell>
                    <RowActionsCell
                      actions={actions(row)}
                      menuLabel={labels.rowMenu}
                      maxInline={maxInlineActions}
                    />
                  </TableCell>
                )}
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </TableContainer>

      <TablePagination
        component="div"
        count={sorted.length}
        page={safePage}
        onPageChange={(_e, next) => setPage(next)}
        rowsPerPage={rowsPerPage}
        onRowsPerPageChange={(e) => {
          setRowsPerPage(Number(e.target.value));
          setPage(0);
        }}
        rowsPerPageOptions={ROWS_PER_PAGE_OPTIONS}
        labelRowsPerPage={labels.rowsPerPage}
        labelDisplayedRows={({ from, to, count }) =>
          `${from}–${to} ${labels.rangeOf} ${count} ${labels.rowsSuffix}`
        }
      />
    </Box>
  );
}
