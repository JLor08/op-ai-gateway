// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { RowActions } from './RowActions';

describe('RowActions', () => {
  it('wraps its buttons together in a single row stack', () => {
    render(
      <RowActions>
        <button>Bearbeiten</button>
        <button>Loeschen</button>
      </RowActions>,
    );
    const wrapper = screen.getByRole('button', { name: 'Bearbeiten' }).parentElement;
    expect(wrapper).not.toBeNull();
    expect(screen.getByRole('button', { name: 'Loeschen' }).parentElement).toBe(wrapper);
  });
});
