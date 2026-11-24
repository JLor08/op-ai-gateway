// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { Translation } from './types';

// Labels of a vendor account group member shared by the providers page and the
// dashboard cards.

/** Why a member cannot serve; an unknown reason is shown as the server sent it. */
export function reasonLabel(t: Translation, reason: string): string {
  switch (reason) {
    case 'disabled':
      return t.vendorGroupReasonDisabled;
    case 'needs_reconnect':
      return t.vendorGroupReasonNeedsReconnect;
    case 'subscriptions_disabled':
      return t.vendorGroupReasonSubscriptionsDisabled;
    case 'not_reachable':
      return t.vendorGroupReasonNotReachable;
    default:
      return reason;
  }
}
