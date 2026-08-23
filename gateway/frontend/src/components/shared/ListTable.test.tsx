// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { ListTable, type ListColumn, type ListTableLabels } from './ListTable';

type Row = { id: string; name: string };

const columns: ListColumn<Row>[] = [{ id: 'name', label: 'Name', value: (r) => r.name }];

const labels: ListTableLabels = {
  searchPlaceholder: 'Suchen',
  rowsPerPage: 'Zeilen pro Seite',
  rangeOf: 'von',
  rowsSuffix: 'Einträge',
  filter: 'Filter',
  filterClear: 'Zurücksetzen',
  rowMenu: 'Aktionen',
  empty: 'Keine Einträge',
  loading: 'Laden...',
  columns: 'Spalten',
  columnsReset: 'Standard',
  columnMoveLeft: 'Nach links',
  columnMoveRight: 'Nach rechts',
};

function renderTable(props: { rows: Row[]; loading?: boolean }) {
  render(
    <ListTable
      rows={props.rows}
      columns={columns}
      rowKey={(r) => r.id}
      labels={labels}
      loading={props.loading}
    />,
  );
}

afterEach(cleanup);

describe('ListTable loading vs empty', () => {
  it('shows the loading label when loading and there are no rows', () => {
    renderTable({ rows: [], loading: true });
    expect(screen.getByText('Laden...')).toBeInTheDocument();
    expect(screen.queryByText('Keine Einträge')).not.toBeInTheDocument();
  });

  it('shows the empty label when not loading and there are no rows', () => {
    renderTable({ rows: [], loading: false });
    expect(screen.getByText('Keine Einträge')).toBeInTheDocument();
    expect(screen.queryByText('Laden...')).not.toBeInTheDocument();
  });

  it('defaults to not-loading (empty label) when the loading prop is omitted', () => {
    renderTable({ rows: [] });
    expect(screen.getByText('Keine Einträge')).toBeInTheDocument();
  });

  it('shows neither placeholder once rows are present', () => {
    renderTable({ rows: [{ id: '1', name: 'Alpha' }], loading: false });
    expect(screen.queryByText('Keine Einträge')).not.toBeInTheDocument();
    expect(screen.queryByText('Laden...')).not.toBeInTheDocument();
    expect(screen.getByText('Alpha')).toBeInTheDocument();
  });

  it('falls back to the empty label while loading when no loading label is set', () => {
    render(
      <ListTable
        rows={[]}
        columns={columns}
        rowKey={(r) => r.id}
        labels={{ ...labels, loading: undefined }}
        loading
      />,
    );
    expect(screen.getByText('Keine Einträge')).toBeInTheDocument();
  });
});
