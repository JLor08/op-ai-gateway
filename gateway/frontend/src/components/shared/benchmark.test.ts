// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it, vi } from 'vitest';
import type { BenchmarkStatus } from '../../api';
import { BenchmarkPollTimeoutError, pollBenchmarkStatus } from './benchmark';

const running: BenchmarkStatus = {
  running: true,
  server_id: 'srv_1',
  scope: 'mapping',
  total: 1,
  done: 0,
};

describe('pollBenchmarkStatus', () => {
  it('rejects with a BenchmarkPollTimeoutError at the poll cap, keeping the message', async () => {
    const benchmarkStatus = vi.fn(async () => running);
    const err = await pollBenchmarkStatus({ benchmarkStatus }, 'srv_1', {
      intervalMs: 0,
      maxPolls: 3,
    }).catch((e: unknown) => e);

    expect(err).toBeInstanceOf(BenchmarkPollTimeoutError);
    expect(err).toBeInstanceOf(Error);
    expect((err as Error).name).toBe('BenchmarkPollTimeoutError');
    expect((err as Error).message).toBe('benchmark poll timed out');
    expect(benchmarkStatus).toHaveBeenCalledTimes(3);
  });

  it('rethrows the fetch error itself, not a poll timeout, after consecutive failures', async () => {
    // A run the portal can no longer read is not a run that is still going:
    // the callers show the pending text only for the cap.
    const down = new Error('network down');
    const benchmarkStatus = vi.fn(async (): Promise<BenchmarkStatus> => {
      throw down;
    });
    const err = await pollBenchmarkStatus({ benchmarkStatus }, 'srv_1', { intervalMs: 0 }).catch(
      (e: unknown) => e,
    );

    expect(err).toBe(down);
    expect(err).not.toBeInstanceOf(BenchmarkPollTimeoutError);
  });
});
