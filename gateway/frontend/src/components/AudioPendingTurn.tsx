// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { Translation } from './shared/types';
import { RunPendingTurn } from './RunPendingTurn';

// The composer during a speech run's wait -- the audio twin of ImagePendingTurn, for
// the same reason. A BUFFERED speech run (`/v1/audio/speech` answering one complete WAV)
// emits ZERO incremental events between Send and the audio. A progress bar, a percentage or
// an ETA would each assert a measurement that does not exist, and ChatMessage's ordinary
// text-pending state would read "0 Zeichen" for the whole run: a "measured zero" where
// nothing was ever measured.
//
// So it shows what RunPendingTurn shows -- liveness, a server-anchored clock and one
// sentence saying no intermediate news is coming -- worded for audio. The wait label
// words the WAIT, not the work ("waiting for audio", not "audio is being generated"):
// the request may still be queued for admission or waiting on a model load.
//
// A STREAMING model's run is the exception to the silence (ADR-068): its audio is played
// as it is generated, so once the first chunk is playing (`live`) "no intermediate news" would
// be false while the user is listening. The turn then says the audio is playing, and that
// the finished clip follows in this bubble -- it still ends as the one WAV every speech turn
// is, rendered by AudioTurn.
export function AudioPendingTurn({
  t,
  elapsedMs,
  roleLabel,
  live = false,
}: Readonly<{
  t: Translation;
  elapsedMs: number;
  roleLabel: string;
  // Whether the run is playing its audio while it is generated right now.
  live?: boolean;
}>) {
  return (
    <RunPendingTurn
      elapsedMs={elapsedMs}
      roleLabel={roleLabel}
      waitLabel={live ? t.chatAudioStreamLive : t.chatAudioRunPending}
      noNewsLabel={live ? t.chatAudioStreamLiveHint : t.chatAudioNoIntermediateNews}
    />
  );
}
