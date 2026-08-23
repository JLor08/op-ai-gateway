// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { LucideIcon } from 'lucide-react';
import type { createPortalApi, DashboardResponse, PortalToken, UsageEvent } from '../../api';
import type { messages } from '../../i18n';

export type Translation = (typeof messages)['de'];
// String-valued message keys only. Excludes function-valued entries (e.g.
// activityNewRequests) so generic `t[key]` label lookups stay typed as `string`
// and remain valid ReactNode children.
export type MessageKey = {
  [K in keyof Translation]: Translation[K] extends string ? K : never;
}[keyof Translation];
export type PortalApi = ReturnType<typeof createPortalApi>;
export type View =
  | 'dashboard'
  | 'chat'
  | 'tokens'
  | 'usage'
  | 'models'
  | 'servers'
  | 'resourceGroups'
  | 'services'
  | 'users'
  | 'groups'
  | 'projects'
  | 'tools'
  | 'management'
  | 'system'
  | 'netbird'
  | 'certificates'
  | 'logs'
  | 'impressum'
  | 'nutzungsbedingungen'
  | 'datenschutz';
export type NavItem = { id: View; labelKey: MessageKey; href: string; icon: LucideIcon };
export type RouteStatus = DashboardResponse['routes'][number]['status'];
export type BadgeStatus = RouteStatus | PortalToken['status'] | UsageEvent['status'];
