// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { Translation } from './shared/types';
import { RunPendingTurn } from './RunPendingTurn';

// The wait of a transcription (speech-to-text) run -- the twin of AudioPendingTurn, for
// the same reason. The run asks `/v1/audio/transcriptions` for a stream and the gateway
// answers per mapped model: a one-shot model replies with ONE finished document, a
// streaming model (audio.cpp mode=streaming) with `transcript.text.delta` events. Either
// way, between Send and the FIRST text there are exactly ZERO incremental events: no
// tokens to count, no audio progress to report. A progress bar, a percentage or an ETA
// would each assert a measurement that does not exist, and ChatMessage's ordinary
// text-pending state would read "Denkt ... 0 Zeichen" for the whole wait -- a "measured
// zero" where nothing was ever measured, over a model that is not reasoning at all.
//
// This turn is mounted only until text arrives (ChatMessage's `text.length === 0`
// guard): for a one-shot model that is the end of the run, for a streaming one the turn
// is replaced by the growing transcript at its first delta. So the sentence it carries
// must hold for both and may not promise a finished-or-nothing transcript.
//
// It shows what RunPendingTurn shows -- the run's own liveness, a server-anchored
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
