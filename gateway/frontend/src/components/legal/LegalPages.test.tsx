// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { LegalPages } from './LegalPages';

describe('LegalPages', () => {
  it('shows the template banner and a placeholder on Impressum (de)', () => {
    render(<LegalPages page="impressum" locale="de" />);
    expect(screen.getByText(/Vorlage/i)).toBeInTheDocument();
    expect(screen.getByText(/\[Name \/ Firma\]/)).toBeInTheDocument();
    expect(screen.queryByText(/Lorenz/)).not.toBeInTheDocument();
  });

  it('pre-fills data categories on Datenschutz (de)', () => {
    render(<LegalPages page="datenschutz" locale="de" />);
    expect(screen.getByText('Datenschutzerklärung')).toBeInTheDocument();
    expect(screen.getAllByText(/Payload-Capture/i).length).toBeGreaterThan(0);
  });

  it('renders the English terms of use', () => {
    render(<LegalPages page="nutzungsbedingungen" locale="en" />);
    expect(screen.getByText('Terms of use')).toBeInTheDocument();
    expect(screen.getByText(/not legal advice/i)).toBeInTheDocument();
  });
});
