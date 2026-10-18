// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { Translation } from './shared/types';
import { RunPendingTurn } from './RunPendingTurn';

// The wait of a transcription (speech-to-text) run -- the twin of AudioPendingTurn, for
// the same reason. `/v1/audio/transcriptions` answers ONE finished document and the run
// is asked for no stream, so between Send and the transcript there are exactly ZERO
// incremental events: no tokens to count, no audio progress to report. A progress bar,
// a percentage or an ETA would each assert a measurement that does not exist, and
// ChatMessage's ordinary text-pending state would read "Denkt ... 0 Zeichen" for the
// whole run -- a "measured zero" where nothing was ever measured, over a model that is
// not reasoning at all.
//
// So it shows what RunPendingTurn shows -- the run's own liveness, a server-anchored
// clock and one sentence saying no intermediate news is coming -- worded for a
// transcript. There is no player: the output is text, and the input clip is already in
// the user's bubble above. The wait label words the WAIT, not the work ("waiting for the
// transcript", not "the audio is being transcribed"): the request may still be queued
// for admission or waiting on a model load.
export function TranscriptionPendingTurn({
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
      waitLabel={t.chatTranscriptionRunPending}
      noNewsLabel={t.chatTranscriptionNoIntermediateNews}
    />
  );
}
