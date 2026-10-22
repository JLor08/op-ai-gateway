// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { Button, Stack } from '@mui/material';
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
//
// While it plays, the turn also carries the two ways to stop listening (#281), which are not
// the same thing: MUTE silences this tab's speakers only (a toggle -- the run goes on, and
// finishes and is stored per the retention as ever), ABORT cancels the run itself (the
// composer's own Stop; nothing is stored, and the turn ends as a not-stored answer). They
// exist only while `live`: with no audio playing there is nothing to mute, and a buffered
// run's cancel is the composer's Stop. A control whose handler is not supplied is not drawn.
export function AudioPendingTurn({
  t,
  elapsedMs,
  roleLabel,
  live = false,
  muted = false,
  onMute,
  onAbort,
}: Readonly<{
  t: Translation;
  elapsedMs: number;
  roleLabel: string;
  // Whether the run is playing its audio while it is generated right now.
  live?: boolean;
  // Whether the user has muted that playback (the Mute control then reads Unmute).
  muted?: boolean;
  // Mute or unmute the playback: called with the state the click asks for.
  onMute?: (muted: boolean) => void;
  // Cancel the run.
  onAbort?: () => void;
}>) {
  return (
    <RunPendingTurn
      elapsedMs={elapsedMs}
      roleLabel={roleLabel}
      waitLabel={live ? t.chatAudioStreamLive : t.chatAudioRunPending}
      noNewsLabel={live ? t.chatAudioStreamLiveHint : t.chatAudioNoIntermediateNews}
      actions={
        live && (onMute || onAbort) ? (
          <Stack direction="row" sx={{ mt: 1.25, gap: 1, flexWrap: 'wrap' }}>
            {onMute && (
              <Button
                variant="outlined"
                size="small"
                aria-pressed={muted}
                onClick={() => onMute(!muted)}
              >
                {muted ? t.chatAudioUnmute : t.chatAudioMute}
              </Button>
            )}
            {onAbort && (
              <Button variant="outlined" size="small" color="error" onClick={onAbort}>
                {t.chatAudioAbort}
              </Button>
            )}
          </Stack>
        ) : undefined
      }
    />
  );
}
