// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { Brand } from './Brand';

afterEach(() => {
  cleanup();
});

describe('Brand', () => {
  it('renders a text mark with the title and container label', () => {
    render(
      <Brand
        brand={{ mark: { type: 'text', text: 'OP' }, title: 'AI Gateway' }}
        label="OP AI Gateway"
      />,
    );
    expect(screen.getByLabelText('OP AI Gateway')).toBeInTheDocument();
    expect(screen.getByText('OP')).toBeInTheDocument();
    expect(screen.getByText('AI Gateway')).toBeInTheDocument();
  });

  it('renders an <img> for an image mark, pointed at the given url', () => {
    render(
      <Brand
        brand={{
          mark: { type: 'image', url: '/api/system/themes/acme/logo' },
          title: 'AI Gateway',
        }}
        label="Acme AI Gateway"
      />,
    );
    expect(screen.getByLabelText('Acme AI Gateway')).toBeInTheDocument();
    const img = screen.getByRole('img', { name: 'Acme AI Gateway' });
    expect(img.tagName).toBe('IMG');
    expect(img).toHaveAttribute('src', '/api/system/themes/acme/logo');
    expect(screen.getByText('AI Gateway')).toBeInTheDocument();
  });

  it('renders the Matrix logo for a matrix logo mark', () => {
    render(
      <Brand
        brand={{ mark: { type: 'logo', id: 'matrix' }, title: 'AI Gateway' }}
        label="On-Prem AI Gateway"
      />,
    );
    expect(screen.getByRole('img', { name: 'Matrix' })).toBeInTheDocument();
    expect(screen.getByText('AI Gateway')).toBeInTheDocument();
  });
});
