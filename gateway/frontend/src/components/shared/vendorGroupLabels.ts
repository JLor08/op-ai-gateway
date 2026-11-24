// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { VendorAccountGroupMember } from '../../api';
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

/** The blocking window the backend reports in `reason` of an over-threshold member; "" for any other reason. */
export function windowLabel(t: Translation, reason: string): string {
  switch (reason) {
    case 'five_hour':
      return t.dashboardVendorGroupWindowFiveHour;
    case 'weekly':
      return t.dashboardVendorGroupWindowWeekly;
    case 'credits':
      return t.dashboardVendorGroupWindowCredits;
    default:
      return '';
  }
}

/** An over-threshold member's blocking window with its percent ("5h 99.5 %"), the window alone without one. */
export function overThresholdDetail(
  t: Translation,
  member: Pick<VendorAccountGroupMember, 'reason' | 'pct'>,
): string {
  const window = windowLabel(t, member.reason);
  return member.pct === undefined ? window : `${window} ${member.pct} %`.trim();
}

/**
 * The short caption next to a member's state: the reason of a not-usable member, the
 * blocking window of an over-threshold one, and nothing for every other state (a
 * cooldown's state and its "free at" line already say it all). The backend's reason
 * codes are never shown raw for a state that has a label of its own.
 */
export function memberReasonCaption(
  t: Translation,
  member: Pick<VendorAccountGroupMember, 'state' | 'reason' | 'pct'>,
): string {
  switch (member.state) {
    case 'not_usable':
      return reasonLabel(t, member.reason);
    case 'over_threshold':
      return overThresholdDetail(t, member);
    default:
      return '';
  }
}
