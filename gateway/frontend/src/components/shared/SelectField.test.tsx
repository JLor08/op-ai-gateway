// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { SelectField } from './SelectField';

afterEach(cleanup);

describe('SelectField', () => {
  it('exposes a select labelled by label with option roles', () => {
    const onChange = vi.fn();
    render(
      <SelectField id="role" label="Rolle" value="admin" onChange={onChange}>
        <option value="admin">Admin</option>
        <option value="system_admin">System-Admin</option>
      </SelectField>,
    );
    const select = screen.getByRole('combobox', { name: 'Rolle' });
    expect(select).toHaveAttribute('id', 'role');
    // Non-native MUI Select: open the menu, then read the options rendered in a portal.
    fireEvent.mouseDown(select);
    expect(screen.getAllByRole('option').map((o) => o.textContent)).toEqual([
      'Admin',
      'System-Admin',
    ]);
    fireEvent.click(screen.getByRole('option', { name: 'System-Admin' }));
    expect(onChange).toHaveBeenCalled();
  });

  // The select always shows its selected option, so the floating label
  // must stay shrunk even when the value is empty ("" default option) — otherwise
  // the label sits on top of the option text (visual overlap bug).
  it('keeps the label shrunk even when the selected value is empty', () => {
    render(
      <SelectField id="empty" label="Leer" value="" onChange={vi.fn()}>
        <option value="">Ohne</option>
        <option value="a">A</option>
      </SelectField>,
    );
    const label = screen.getByText('Leer', { selector: '.MuiInputLabel-root' });
    expect(label).toHaveClass('MuiInputLabel-shrink');
  });
});
