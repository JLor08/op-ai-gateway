// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { StatTile } from './StatTile';

afterEach(cleanup);

describe('StatTile', () => {
  it('renders value, label and optional detail', () => {
    render(<StatTile value="42" label="Requests" detail="+18%" />);
    expect(screen.getByText('42')).toBeInTheDocument();
    expect(screen.getByText('Requests')).toBeInTheDocument();
    expect(screen.getByText('+18%')).toBeInTheDocument();
  });

  it('omits the detail line when no detail is given', () => {
    const { container } = render(<StatTile value="7" label="Errors" />);
    expect(screen.getByText('7')).toBeInTheDocument();
    expect(screen.getByText('Errors')).toBeInTheDocument();
    expect(container.querySelector('small')).toBeNull();
  });
});
