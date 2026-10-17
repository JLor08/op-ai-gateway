// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { Translation } from './shared/types';
import { RunPendingTurn } from './RunPendingTurn';

// The composer during an image run's wait (ADR-043 (d),
// docs/architecture/09-architecture-decisions.md).
// `/v1/images/generations` refuses `stream` outright, so between Send and the
// finished image there are exactly ZERO incremental events -- tens of
// seconds to minutes of nothing. A progress bar, a percentage or an ETA
// would each assert a measurement that does not exist. Rendering ChatMessage's
// ordinary text-pending state instead would be just as dishonest in the other
// direction: its character counter reads `reasoningText.length`, which is 0
// for the ENTIRE run (an image run never streams any text either), so it
// would show "0 Zeichen" from the first millisecond to the last -- a
// "measured zero" where nothing was ever measured.
//
// So this renders exactly three things, each backed by a value that exists:
// liveness (this component is only mounted while the run's own
// server-reported status is "running" -- see ChatMessage's guard), an
// elapsed clock anchored on the server's own measurement, and one static
// sentence stating that no intermediate news is coming. The mechanics are
// RunPendingTurn's, shared with the speech run's AudioPendingTurn.
export function ImagePendingTurn({
  t,
  // The run's server-measured age in ms, AS OF THE LAST SNAPSHOT the caller
  // received (useChatRuns' elapsedMsOf) -- an anchor, not a live value; see
  // RunPendingTurn for how the display re-anchors on it.
  elapsedMs,
  roleLabel,
}: Readonly<{
  t: Translation;
  elapsedMs: number;
  roleLabel: string;
}>) {
  return (
    <RunPendingTurn
      elapsedMs={elapsedMs}
      roleLabel={roleLabel}
      waitLabel={t.chatImageRunPending}
      noNewsLabel={t.chatImageNoIntermediateNews}
    />
  );
}
