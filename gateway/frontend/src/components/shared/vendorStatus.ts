// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { BadgeStatus, Translation } from './types';

/** The display label of a vendor-account status; an unknown status is shown as sent. */
export function vendorStatusLabel(t: Translation, status: string): string {
  switch (status) {
    case 'active':
      return t.statusActive;
    case 'disabled':
      return t.statusDisabled;
    case 'needs_reconnect':
      return t.vendorAccountStatusNeedsReconnect;
    default:
      return status;
  }
}

// needs_reconnect is system-managed (a failed subscription token refresh) and
// reads as a warning, not as an active or a switched-off account.
export function vendorStatusBadge(status: string): BadgeStatus {
  switch (status) {
    case 'active':
      return 'active';
    case 'needs_reconnect':
      return 'watch';
    default:
      return 'disabled';
  }
}
