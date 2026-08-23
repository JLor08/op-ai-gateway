// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, render, screen, within } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { DataTable } from './DataTable';

afterEach(cleanup);

describe('DataTable', () => {
  it('renders column headers as columnheaders and body cells as cells', () => {
    render(
      <DataTable
        columns={['Modell', 'Provider', 'Server', 'Status']}
        isEmpty={false}
        emptyLabel="leer"
      >
        <tr>
          <td>qwen-coder</td>
          <td>mock</td>
          <td>mock-host-qwen</td>
          <td>Aktiv</td>
        </tr>
      </DataTable>,
    );
    const table = screen.getByRole('table');
    expect(within(table).getByRole('columnheader', { name: 'Modell' })).toBeInTheDocument();
    expect(within(table).getByRole('cell', { name: 'qwen-coder' })).toBeInTheDocument();
  });

  it('renders a single empty-state cell spanning all columns', () => {
    render(
      <DataTable columns={['A', 'B']} isEmpty emptyLabel="Keine Daten">
        {null}
      </DataTable>,
    );
    expect(screen.getByRole('cell', { name: 'Keine Daten' })).toHaveAttribute('colspan', '2');
  });
});
