// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { StatusChip } from './StatusChip';

afterEach(cleanup);

describe('StatusChip', () => {
  it('renders the translated label inside a span carrying `data-status <key>`', () => {
    render(<StatusChip status="active" label="Aktiv" />);
    expect(screen.getByText('Aktiv')).toHaveAttribute('data-status', 'active');
  });
  it('maps status keys (success -> active, error -> standby)', () => {
    const { rerender } = render(<StatusChip status="success" label="Erfolgreich" />);
    expect(screen.getByText('Erfolgreich')).toHaveAttribute('data-status', 'active');
    rerender(<StatusChip status="error" label="Fehler" />);
    expect(screen.getByText('Fehler')).toHaveAttribute('data-status', 'standby');
  });
});
