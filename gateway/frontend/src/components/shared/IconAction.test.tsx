// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { fireEvent, render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import EditIcon from '@mui/icons-material/Edit';
import { IconAction } from './IconAction';

describe('IconAction', () => {
  it('exposes the label as its accessible name and fires onClick', () => {
    const onClick = vi.fn();
    render(<IconAction label="Bearbeiten" icon={<EditIcon />} onClick={onClick} />);
    const button = screen.getByRole('button', { name: 'Bearbeiten' });
    fireEvent.click(button);
    expect(onClick).toHaveBeenCalledTimes(1);
  });

  it('forwards the disabled state', () => {
    render(<IconAction label="Löschen" icon={<EditIcon />} onClick={vi.fn()} disabled />);
    expect(screen.getByRole('button', { name: 'Löschen' })).toBeDisabled();
  });
});
