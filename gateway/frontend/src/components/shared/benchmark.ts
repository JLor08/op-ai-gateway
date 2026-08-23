// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { BenchmarkStatus, PortalApi } from '../../api';

// Poll cadence + safety cap. ~2s per poll, capped so a stuck run can't poll
// forever (150 × 2s ≈ 5 min). A handful of consecutive transient errors is
// tolerated before giving up.
const DEFAULT_INTERVAL_MS = 2000;
const DEFAULT_MAX_POLLS = 150;
const MAX_CONSECUTIVE_ERRORS = 5;

export type BenchmarkPollOptions = {
  // Delay between polls. Injectable so tests can drive the loop without waiting.
  intervalMs?: number;
  maxPolls?: number;
};

const sleep = (ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms));

/**
 * Poll the per-server benchmark status until the run finishes (running===false)
 * or the safety cap is reached. A transient fetch error is swallowed and polling
 * continues, up to MAX_CONSECUTIVE_ERRORS in a row; exceeding that — or hitting
 * the poll cap — rejects so the caller can surface a failure toast. Resolves with
 * the final status (so the caller can toast a summary + refresh the metrics).
 */
export async function pollBenchmarkStatus(
  api: Pick<PortalApi, 'benchmarkStatus'>,
  serverId: string,
  options: BenchmarkPollOptions = {},
): Promise<BenchmarkStatus> {
  const intervalMs = options.intervalMs ?? DEFAULT_INTERVAL_MS;
  const maxPolls = options.maxPolls ?? DEFAULT_MAX_POLLS;
  let consecutiveErrors = 0;
  for (let i = 0; i < maxPolls; i += 1) {
    await sleep(intervalMs);
    try {
      const status = await api.benchmarkStatus(serverId);
      consecutiveErrors = 0;
      if (!status.running) return status;
    } catch (err) {
      consecutiveErrors += 1;
      if (consecutiveErrors >= MAX_CONSECUTIVE_ERRORS) throw err;
    }
  }
  throw new Error('benchmark poll timed out');
}
