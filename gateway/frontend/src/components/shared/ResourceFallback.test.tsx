// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ResourceFallback, resourceState } from './ResourceFallback';

// This project runs vitest without `globals`, so RTL never registers its
// auto-cleanup and every test's DOM would stay in the document.
afterEach(cleanup);

describe('resourceState', () => {
  it('reports the in-flight fetch as loading', () => {
    expect(resourceState({ loading: true, error: '', data: null })).toBe('loading');
    // Still loading even with an old payload and an old error in hand.
    expect(resourceState({ loading: true, error: 'boom', data: { a: 1 } })).toBe('loading');
  });

  // The pre-first-fetch ('idle') window: `useLatestFetch` has not started yet,
  // so there is neither data nor an error. That is a loading state, not a
  // failure -- reporting it as `error` would flash a failure banner on mount.
  it('reports the idle window as loading, not as a failure', () => {
    expect(resourceState({ loading: false, error: '', data: null })).toBe('loading');
  });

  it('reports a first-fetch failure as error', () => {
    expect(resourceState({ loading: false, error: 'boom', data: null })).toBe('error');
  });

  // The order matters: `data !== null` used to be tested FIRST, so a resource
  // that had loaded once and then failed to reload reported `ready` and the
  // error was invisible. `useResource` never clears `data` on failure, and a
  // deps change (a server switch) re-runs the loader while `data` keeps the
  // previous payload -- so this is reachable, not theoretical.
  it('reports a failed reload over an existing payload as stale-error', () => {
    expect(resourceState({ loading: false, error: 'boom', data: { a: 1 } })).toBe('stale-error');
  });

  it('reports a settled payload as ready', () => {
    expect(resourceState({ loading: false, error: '', data: { a: 1 } })).toBe('ready');
    // An empty list is a payload, not the absence of one.
    expect(resourceState({ loading: false, error: '', data: [] })).toBe('ready');
  });
});

describe('ResourceFallback', () => {
  it('renders nothing when the resource is ready', () => {
    const { container } = render(
      <ResourceFallback state="ready" loadingLabel="Lädt…" errorLabel="Fehlgeschlagen" />,
    );
    expect(container).toBeEmptyDOMElement();
  });

  it('renders the plain loading line', () => {
    render(<ResourceFallback state="loading" loadingLabel="Lädt…" errorLabel="Fehlgeschlagen" />);
    expect(screen.getByText('Lädt…')).toBeInTheDocument();
    expect(screen.queryByText('Fehlgeschlagen')).not.toBeInTheDocument();
  });

  it('renders the failure with its detail and a working retry', () => {
    const onRetry = vi.fn();
    render(
      <ResourceFallback
        state="error"
        loadingLabel="Lädt…"
        errorLabel="Fehlgeschlagen"
        errorDetail="503 upstream"
        retry={{ label: 'Erneut versuchen', onRetry }}
      />,
    );
    expect(screen.getByText('Fehlgeschlagen')).toBeInTheDocument();
    expect(screen.getByText('503 upstream')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Erneut versuchen' }));
    expect(onRetry).toHaveBeenCalledTimes(1);
  });

  it('omits the retry button when no retry is given', () => {
    render(<ResourceFallback state="error" loadingLabel="Lädt…" errorLabel="Fehlgeschlagen" />);
    expect(screen.queryByRole('button')).not.toBeInTheDocument();
  });

  // The stale case is what lets a call site keep the last known values on
  // screen: this component contributes the banner, nothing else, so the data
  // the caller renders alongside it is untouched.
  it('names the stale failure separately from the hard one', () => {
    render(
      <ResourceFallback
        state="stale-error"
        loadingLabel="Lädt…"
        errorLabel="Fehlgeschlagen"
        staleErrorLabel="Letzter bekannter Stand — die Aktualisierung ist fehlgeschlagen"
        errorDetail="503 upstream"
      />,
    );
    expect(
      screen.getByText('Letzter bekannter Stand — die Aktualisierung ist fehlgeschlagen'),
    ).toBeInTheDocument();
    expect(screen.queryByText('Fehlgeschlagen')).not.toBeInTheDocument();
    expect(screen.getByText('503 upstream')).toBeInTheDocument();
  });

  it('falls back to the error label when no stale label is given', () => {
    render(
      <ResourceFallback state="stale-error" loadingLabel="Lädt…" errorLabel="Fehlgeschlagen" />,
    );
    expect(screen.getByText('Fehlgeschlagen')).toBeInTheDocument();
  });

  it('carries the severity through to the alert', () => {
    render(
      <ResourceFallback
        state="stale-error"
        loadingLabel="Lädt…"
        errorLabel="Fehlgeschlagen"
        severity="info"
      />,
    );
    expect(screen.getByRole('alert').className).toContain('MuiAlert-colorInfo');
  });

  it('defaults the severity to warning', () => {
    render(
      <ResourceFallback
        state="error"
        loadingLabel="Lädt…"
        errorLabel="Fehlgeschlagen (Standard)"
      />,
    );
    expect(screen.getByRole('alert').className).toContain('MuiAlert-colorWarning');
  });
});
