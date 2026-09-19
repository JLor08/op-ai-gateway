// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Elapsed since a request started: "Xs" under a minute, otherwise "m:ss".
// Lifted out of ActiveRequestsPanel (where it lived unexported) so a second
// copy cannot drift from this one -- ImagePendingTurn's image-run wait clock
// needs the exact same formatting.
export function formatElapsed(ms: number): string {
  const total = Math.max(0, Math.floor(ms / 1000));
  if (total < 60) return `${total}s`;
  const minutes = Math.floor(total / 60);
  const seconds = total % 60;
  return `${minutes}:${String(seconds).padStart(2, '0')}`;
}
