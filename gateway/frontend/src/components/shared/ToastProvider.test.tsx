// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ToastProvider, useToast } from './ToastProvider';

function Harness() {
  const { showSuccess, showError } = useToast();
  return (
    <>
      <button onClick={() => showSuccess('gespeichert')}>ok</button>
      <button onClick={() => showError('kaputt')}>fail</button>
    </>
  );
}

describe('ToastProvider', () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => {
    cleanup();
    vi.useRealTimers();
  });

  it('shows an error alert (role=alert) that stays until dismissed', () => {
    render(
      <ToastProvider>
        <Harness />
      </ToastProvider>,
    );
    fireEvent.click(screen.getByRole('button', { name: 'fail' }));
    expect(screen.getByRole('alert')).toHaveTextContent('kaputt');
    act(() => {
      vi.advanceTimersByTime(10000);
    });
    expect(screen.getByRole('alert')).toHaveTextContent('kaputt');
    fireEvent.click(screen.getByRole('button', { name: /close/i }));
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('auto-dismisses a success alert after ~4s', () => {
    render(
      <ToastProvider>
        <Harness />
      </ToastProvider>,
    );
    fireEvent.click(screen.getByRole('button', { name: 'ok' }));
    expect(screen.getByRole('alert')).toHaveTextContent('gespeichert');
    act(() => {
      vi.advanceTimersByTime(4000);
    });
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('clears a pending success auto-dismiss timer when it unmounts', () => {
    const { unmount } = render(
      <ToastProvider>
        <Harness />
      </ToastProvider>,
    );
    // A success toast schedules its 4s auto-dismiss timer.
    fireEvent.click(screen.getByRole('button', { name: 'ok' }));
    expect(vi.getTimerCount()).toBe(1);
    // Unmounting before it fires must clear it: otherwise the orphan tick calls
    // setItems after the host (jsdom window) is torn down, which crashes other
    // suites' tests with "ReferenceError: window is not defined".
    unmount();
    expect(vi.getTimerCount()).toBe(0);
  });
});
