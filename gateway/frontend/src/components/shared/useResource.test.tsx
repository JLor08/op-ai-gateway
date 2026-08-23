// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { useResource } from './useResource';
import { formatPortalError } from './format';
import { messages } from '../../i18n';
import { PortalApiError } from '../../api';

const t = messages.de;

function Probe({ loader }: { loader: () => Promise<string> }) {
  const { data, loading, error } = useResource(loader, [], t);
  return (
    <div>
      <span data-testid="loading">{loading ? 'loading' : 'idle'}</span>
      <span data-testid="data">{data ?? 'none'}</span>
      <span data-testid="error">{error}</span>
    </div>
  );
}

// Records whether `loading` was ever observed true across renders, so a test
// can assert the trackLoading:false path never flips the flag on.
function LoadingProbe({
  loader,
  options,
  sink,
}: {
  loader: () => Promise<string>;
  options?: { trackLoading?: boolean };
  sink: { sawLoading: boolean };
}) {
  const { data, loading } = useResource(loader, [], t, options);
  if (loading) {
    sink.sawLoading = true;
  }
  return <span data-testid="data">{data ?? 'none'}</span>;
}

function ReloadProbe({ loader }: { loader: () => Promise<string> }) {
  const { data, reload } = useResource(loader, [], t);
  return (
    <div>
      <span data-testid="data">{data ?? 'none'}</span>
      <button type="button" onClick={() => void reload()}>
        reload
      </button>
    </div>
  );
}

afterEach(cleanup);

describe('useResource', () => {
  it('exposes resolved data and ends loading false', async () => {
    render(<Probe loader={() => Promise.resolve('hello')} />);
    await waitFor(() => expect(screen.getByTestId('data')).toHaveTextContent('hello'));
    expect(screen.getByTestId('loading')).toHaveTextContent('idle');
    expect(screen.getByTestId('error').textContent).toBe('');
  });

  it('renders the formatPortalError-formatted string on rejection', async () => {
    const err = new PortalApiError(404, 'portal.token_not_found', 'raw message');
    const expected = formatPortalError(err, t);
    render(<Probe loader={() => Promise.reject(err)} />);
    await waitFor(() => expect(screen.getByTestId('error').textContent).toBe(expected));
    expect(screen.getByTestId('loading')).toHaveTextContent('idle');
    expect(screen.getByTestId('data')).toHaveTextContent('none');
  });

  it('never flips loading true when trackLoading is false', async () => {
    const sink = { sawLoading: false };
    render(
      <LoadingProbe
        loader={() => Promise.resolve('value')}
        options={{ trackLoading: false }}
        sink={sink}
      />,
    );
    await waitFor(() => expect(screen.getByTestId('data')).toHaveTextContent('value'));
    expect(sink.sawLoading).toBe(false);
  });

  it('flips loading true during the load by default (trackLoading on)', async () => {
    const sink = { sawLoading: false };
    render(<LoadingProbe loader={() => Promise.resolve('value')} sink={sink} />);
    await waitFor(() => expect(screen.getByTestId('data')).toHaveTextContent('value'));
    expect(sink.sawLoading).toBe(true);
  });

  it('re-fetches and updates data when reload is called', async () => {
    let call = 0;
    const loader = () => Promise.resolve(call++ === 0 ? 'first' : 'second');
    render(<ReloadProbe loader={loader} />);
    await waitFor(() => expect(screen.getByTestId('data')).toHaveTextContent('first'));
    fireEvent.click(screen.getByRole('button', { name: 'reload' }));
    await waitFor(() => expect(screen.getByTestId('data')).toHaveTextContent('second'));
  });
});
