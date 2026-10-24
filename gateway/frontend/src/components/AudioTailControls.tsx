// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { Button, Stack } from '@mui/material';
import type { Translation } from './shared/types';

// The two ways to stop listening to a streaming speech answer (#281), which are not the same
// thing: MUTE silences this tab's speakers only (a reversible toggle -- the run and what is
// stored of it are untouched), ABORT stops the audio for good (while the run is still running it
// is the composer's own Stop, the cancel; once the run has committed and only the streamed tail
// is still playing out, it just silences the tail -- the stored answer is kept either way).
//
// Factored out of AudioPendingTurn so the SAME row of controls can ride either place the audio is
// heard: the live pending bubble before the answer commits, and beside the saved AudioTurn player
// while the streamed tail drains afterwards. A control whose handler is not supplied is not drawn,
// and with neither handler nothing is rendered at all.
export function AudioTailControls({
  t,
  muted = false,
  onMute,
  onAbort,
}: Readonly<{
  t: Translation;
  // Whether the playback is muted (the Mute control then reads Unmute).
  muted?: boolean;
  // Mute or unmute the playback: called with the state the click asks for.
  onMute?: (muted: boolean) => void;
  // Stop the audio (cancel the run while it runs, or silence the drained tail afterwards).
  onAbort?: () => void;
}>) {
  if (!onMute && !onAbort) return null;
  return (
    <Stack direction="row" sx={{ mt: 1.25, gap: 1, flexWrap: 'wrap' }}>
      {onMute && (
        <Button variant="outlined" size="small" aria-pressed={muted} onClick={() => onMute(!muted)}>
          {muted ? t.chatAudioUnmute : t.chatAudioMute}
        </Button>
      )}
      {onAbort && (
        <Button variant="outlined" size="small" color="error" onClick={onAbort}>
          {t.chatAudioAbort}
        </Button>
      )}
    </Stack>
  );
}
