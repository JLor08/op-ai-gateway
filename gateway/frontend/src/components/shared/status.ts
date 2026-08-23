// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { BadgeStatus } from './types';

export const statusClassByKey: Record<BadgeStatus, string> = {
  active: 'active',
  disabled: 'standby',
  expired: 'standby',
  watch: 'watch',
  standby: 'standby',
  success: 'active',
  error: 'standby',
};

// Additive: MUI-Chip-Hintergrund/-Vordergrund je aufgelöstem Status-Key, aus
// denselben CSS-Custom-Properties (--success-bg/--watch-bg/--standby-bg …) der
// Theme-Bridge, damit der Chip in jedem Theme zum Legacy-Badge passt.
export const statusChipSx: Record<'active' | 'watch' | 'standby', { bg: string; color: string }> = {
  active: { bg: 'var(--success-bg)', color: 'var(--success-text)' },
  watch: { bg: 'var(--watch-bg)', color: 'var(--watch-text)' },
  standby: { bg: 'var(--standby-bg)', color: 'var(--standby-text)' },
};

// SP-B error predicate, shared with the list `status` filter and the error stat tile:
// a request is an error when status is "error" OR http_status >= 400. Falls back to
// "standby" for unexpected input so the exhaustive statusClassByKey lookup never breaks.
export function usageChipStatus(status: string, httpStatus: number): BadgeStatus {
  if (status === 'error' || httpStatus >= 400) return 'error';
  if (status === 'success' || (httpStatus >= 200 && httpStatus < 400)) return 'success';
  return 'standby';
}
