// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { Field } from './Field';

afterEach(cleanup);

describe('Field', () => {
  it('wires label -> input id and forwards changes', () => {
    const onChange = vi.fn();
    render(<Field id="token-name" label="Name" value="" onChange={onChange} />);
    const input = screen.getByLabelText('Name');
    expect(input).toHaveAttribute('id', 'token-name');
    fireEvent.change(input, { target: { value: 'x' } });
    expect(onChange).toHaveBeenCalled();
  });
  it('supports a label-less input with an aria-label (inline edit)', () => {
    render(
      <Field
        id="edit-name"
        value="Dev Token"
        onChange={() => {}}
        inputProps={{ 'aria-label': 'Name Dev Token' }}
      />,
    );
    expect(screen.getByLabelText('Name Dev Token')).toHaveAttribute('id', 'edit-name');
  });
});
