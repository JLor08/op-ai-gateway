// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { Translation } from './shared/types';
import { RunPendingTurn } from './RunPendingTurn';

// The composer during a speech run's wait -- the audio twin of ImagePendingTurn, for
// the same reason. `/v1/audio/speech` answers ONE buffered WAV and refuses a streamed
// request, so between Send and the audio there are exactly ZERO incremental events. A
// progress bar, a percentage or an ETA
// would each assert a measurement that does not exist, and ChatMessage's ordinary
// text-pending state would read "0 Zeichen" for the whole run: a "measured zero" where
// nothing was ever measured.
//
// So it shows what RunPendingTurn shows -- liveness, a server-anchored clock and one
// sentence saying no intermediate news is coming -- worded for audio. The wait label
// words the WAIT, not the work ("waiting for audio", not "audio is being generated"):
// the request may still be queued for admission or waiting on a model load.
export function AudioPendingTurn({
  t,
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
      waitLabel={t.chatAudioRunPending}
      noNewsLabel={t.chatAudioNoIntermediateNews}
    />
  );
}
