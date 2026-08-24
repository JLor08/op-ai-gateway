// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
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

describe('ListTable numeric sorting', () => {
  type Metric = { id: string; amount: string };

  const metricColumns: ListColumn<Metric>[] = [
    { id: 'amount', label: 'Menge', value: (r) => r.amount, numeric: true },
  ];

  function renderMetrics(rows: Metric[]) {
    render(<ListTable rows={rows} columns={metricColumns} rowKey={(r) => r.id} labels={labels} />);
  }

  function amountOrder(): string[] {
    return screen
      .getAllByRole('cell')
      .map((cell) => cell.textContent ?? '')
      .filter((text) => text !== '');
  }

  it('orders a numeric column by magnitude, not lexically', async () => {
    // '100' sorts before '20' as text; the whole point of `numeric` is that it
    // does not here.
    renderMetrics([
      { id: 'a', amount: '100.00' },
      { id: 'b', amount: '9.00' },
      { id: 'c', amount: '20.00' },
    ]);

    fireEvent.click(screen.getByRole('button', { name: 'Menge' }));

    await waitFor(() => expect(amountOrder()).toEqual(['9.00', '20.00', '100.00']));
  });

  it('sorts cells with no numeric value last in BOTH directions', async () => {
    // A missing metric renders as the em-dash placeholder. It has no magnitude,
    // so it must not ride the direction flip to the top of a descending sort.
    renderMetrics([
      { id: 'a', amount: '—' },
      { id: 'b', amount: '9.00' },
      { id: 'c', amount: '20.00' },
    ]);

    const header = screen.getByRole('button', { name: 'Menge' });
    fireEvent.click(header);
    await waitFor(() => expect(amountOrder()).toEqual(['9.00', '20.00', '—']));

    fireEvent.click(header);
    await waitFor(() => expect(amountOrder()).toEqual(['20.00', '9.00', '—']));
  });

  it('treats a blank cell as missing, not as zero', async () => {
    // Some columns render "no value" as an empty string rather than a dash.
    // Number('') is 0, so a blank cell would otherwise sort as the smallest
    // real value — e.g. a certificate with no expiry date ranking as the most
    // urgent one.
    const labelled: ListColumn<Metric>[] = [
      { id: 'id', label: 'Name', value: (r) => r.id },
      ...metricColumns,
    ];
    render(
      <ListTable
        rows={[
          { id: 'blank', amount: '' },
          { id: 'nine', amount: '9.00' },
          { id: 'twenty', amount: '20.00' },
        ]}
        columns={labelled}
        rowKey={(r) => r.id}
        labels={labels}
      />,
    );

    fireEvent.click(screen.getByRole('button', { name: 'Menge' }));

    await waitFor(() => {
      const names = screen
        .getAllByRole('row')
        .slice(1)
        .map((row) => row.querySelector('td')?.textContent ?? '');
      expect(names).toEqual(['nine', 'twenty', 'blank']);
    });
  });
});
