// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { PageTitle } from './PageTitle';

describe('PageTitle', () => {
  it('renders the title as an h1 plus the subtitle', () => {
    render(<PageTitle title="Modelle" subtitle="Alle Modelle" />);
    expect(screen.getByRole('heading', { name: 'Modelle', level: 1 })).toBeInTheDocument();
    expect(screen.getByText('Alle Modelle')).toBeInTheDocument();
  });
});
