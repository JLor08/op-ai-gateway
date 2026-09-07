// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { BadgeStatus, MessageKey, Translation } from './types';

// The nine RuntimeState wire values (server-agent/internal/runtime/types.go)
// mapped to their labels. NOTE the deliberate name/value mismatch on
// `pending_vram_unknown`: the enum value is the long one, the i18n key is the
// shorter `runtimeStatePendingVram`. Neither is renamed here -- this map is
// exactly the place where the two vocabularies meet.
const runtimeStateLabelByValue: Record<string, MessageKey> = {
  stopped: 'runtimeStateStopped',
  starting: 'runtimeStateStarting',
  running: 'runtimeStateRunning',
  draining: 'runtimeStateDraining',
  backoff: 'runtimeStateBackoff',
  start_failed: 'runtimeStateStartFailed',
  crashed: 'runtimeStateCrashed',
  pending_vram_unknown: 'runtimeStatePendingVram',
  not_permitted: 'runtimeStateNotPermitted',
};

// A state this portal build does not know (a newer agent) renders its raw wire
// value rather than a misleading label -- the same forward-compat fallback
// runtimeWarningLabelByCode (RuntimeAdminSection.tsx) uses.
export function runtimeStateLabel(state: string, t: Translation): string {
  const key = runtimeStateLabelByValue[state];
  return key ? t[key] : state;
}

// The portal has exactly THREE status colours: the theme defines
// success/watch/standby pairs and nothing else (theme/ThemeRoot.tsx), and
// statusClassByKey collapses `error`/`disabled`/`expired` onto standby
// (components/shared/status.ts) -- there is no red anywhere in the portal, and
// adding one is a portal-wide design change, not this screen's call. So the
// colour can only carry the three coarse facts it genuinely has (loaded /
// on its way / neither), and the LABEL carries the rest. `last_error` -- "the
// last load attempt failed" -- is not a state at all and gets its own column.
export function runtimeStateBadge(state: string): BadgeStatus {
  if (state === 'running') return 'active';
  // Both are "waiting to be loaded", the user-visible "currently loading".
  if (state === 'starting' || state === 'pending_vram_unknown') return 'watch';
  return 'standby';
}
