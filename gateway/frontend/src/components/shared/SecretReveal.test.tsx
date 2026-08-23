// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { SecretReveal } from './SecretReveal';

// This project runs Vitest without `globals`, so Testing Library's auto
// cleanup (which hooks the global afterEach) is not registered — unmount
// rendered trees explicitly so multiple tests querying by the same
// aria-label/text don't accumulate across renders.
afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

describe('SecretReveal', () => {
  it('renders a <strong> title, the secret text, and a copy button', () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.assign(navigator, { clipboard: { writeText } });
    render(
      <SecretReveal title="Neues Token" copyValue="opaigw_created_secret" copyLabel="Kopieren">
        opaigw_created_secret
      </SecretReveal>,
    );
    expect(screen.getByText('Neues Token').tagName).toBe('STRONG');
    expect(screen.getByText('opaigw_created_secret')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Kopieren' }));
    expect(writeText).toHaveBeenCalledWith('opaigw_created_secret');
  });

  it('shows a transient copied confirmation after clicking copy, then reverts', () => {
    vi.useFakeTimers();
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.assign(navigator, { clipboard: { writeText } });
    render(
      <SecretReveal
        title="Neues Token"
        copyValue="opaigw_created_secret"
        copyLabel="Kopieren"
        copiedLabel="Kopiert!"
      >
        opaigw_created_secret
      </SecretReveal>,
    );

    expect(screen.queryByText('Kopiert!')).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Kopieren' }));
    expect(screen.getByText('Kopiert!')).toBeInTheDocument();

    act(() => {
      vi.advanceTimersByTime(1500);
    });
    expect(screen.queryByText('Kopiert!')).not.toBeInTheDocument();
  });

  it('still swaps the icon to a check mark when no copiedLabel is given', () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.assign(navigator, { clipboard: { writeText } });
    render(
      <SecretReveal title="Neues Token" copyValue="opaigw_created_secret" copyLabel="Kopieren">
        opaigw_created_secret
      </SecretReveal>,
    );

    fireEvent.click(screen.getByRole('button', { name: 'Kopieren' }));
    expect(document.querySelector('[data-testid="CheckIcon"]')).toBeInTheDocument();
  });

  it('wraps a long secret so it never clips', () => {
    const long = 'opaigw_' + 'a'.repeat(80);
    render(
      <SecretReveal title="Neues Token">
        <code>{long}</code>
      </SecretReveal>,
    );
    // The <code> lives inside a wrapper Box that forces breaking of long tokens.
    expect(screen.getByText(long).parentElement).toHaveStyle({ overflowWrap: 'anywhere' });
  });
});
