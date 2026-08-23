// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { act, cleanup, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ConnectionProvider, useConnectionStatus } from './connection';
import { messages } from '../../i18n';
import type { PortalApi } from './types';

const t = messages.de;

function Probe() {
  return <span data-testid="status">{useConnectionStatus()}</span>;
}

function setup(initialHealth: boolean) {
  let healthOk = initialHealth;
  let statusCb: (s: 'open' | 'error') => void = () => {};
  let reconnectCb: () => void = () => {};
  const checkHealth = vi.fn(async () => healthOk);
  const unsubscribe = vi.fn();
  const api: Pick<PortalApi, 'checkHealth' | 'subscribeActivity'> = {
    subscribeActivity: vi.fn(
      (
        _onActivity: () => void,
        onReconnect?: () => void,
        onStatus?: (s: 'open' | 'error') => void,
      ) => {
        if (onStatus) statusCb = onStatus;
        if (onReconnect) reconnectCb = onReconnect;
        return unsubscribe;
      },
    ),
    checkHealth,
  };

  render(
    <ConnectionProvider api={api} t={t}>
      <Probe />
    </ConnectionProvider>,
  );

  return {
    checkHealth,
    unsubscribe,
    setHealth: (v: boolean) => {
      healthOk = v;
    },
    fireStatus: (s: 'open' | 'error') => act(() => statusCb(s)),
    fireReconnect: () => act(() => reconnectCb()),
    status: () => screen.getByTestId('status').textContent,
  };
}

beforeEach(() => vi.useFakeTimers());
afterEach(() => {
  vi.runOnlyPendingTimers();
  vi.useRealTimers();
  cleanup();
});

// Advance timers a tick AND flush the checkHealth promise + resulting setState.
const flush = () =>
  act(async () => {
    await vi.advanceTimersByTimeAsync(1);
  });

describe('ConnectionProvider', () => {
  it('starts online and renders its children', () => {
    const { status } = setup(true);
    expect(status()).toBe('online');
    expect(screen.getByTestId('status')).toBeInTheDocument();
  });

  it('locks (offline) immediately when an SSE error is confirmed down by /healthz', async () => {
    const { fireStatus, status, checkHealth } = setup(false);
    fireStatus('error');
    await flush();
    expect(checkHealth).toHaveBeenCalled();
    expect(status()).toBe('offline');
    expect(screen.getByText(t.connectionLostTitle)).toBeInTheDocument();
  });

  it('does NOT lock when /healthz still succeeds (transient SSE blip)', async () => {
    const { fireStatus, status } = setup(true);
    fireStatus('error');
    await flush();
    expect(status()).toBe('online');
  });

  it('locks via the periodic /healthz poll even if the SSE never errors (half-open)', async () => {
    const { status } = setup(false);
    // Two consecutive failed soft polls (4s each) are required to lock.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(4100);
    });
    expect(status()).toBe('online'); // one failure only so far
    await act(async () => {
      await vi.advanceTimersByTimeAsync(4100);
    });
    expect(status()).toBe('offline');
  });

  it('clears the lock when the SSE reopens', async () => {
    const { fireStatus, status } = setup(false);
    fireStatus('error');
    await flush();
    expect(status()).toBe('offline');
    fireStatus('open');
    expect(status()).toBe('online');
  });

  it('clears the lock via the periodic poll once the backend recovers', async () => {
    const { fireStatus, status, setHealth } = setup(false);
    fireStatus('error');
    await flush();
    expect(status()).toBe('offline');
    setHealth(true);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(4100);
    });
    expect(status()).toBe('online');
  });

  it('unsubscribes from the SSE on unmount', () => {
    const { unsubscribe } = setup(true);
    cleanup();
    expect(unsubscribe).toHaveBeenCalled();
  });
});
