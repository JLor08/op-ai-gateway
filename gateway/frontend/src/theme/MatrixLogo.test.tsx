// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { MatrixLogo } from './MatrixLogo';

describe('MatrixLogo', () => {
  it('renders an accessible Matrix emblem', () => {
    render(<MatrixLogo />);
    expect(screen.getByRole('img', { name: 'Matrix' })).toBeInTheDocument();
  });
});
