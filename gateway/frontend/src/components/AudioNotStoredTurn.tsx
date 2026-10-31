// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useId } from 'react';
import { Box, Button, Typography } from '@mui/material';
import ReplayIcon from '@mui/icons-material/Replay';
import type { Translation } from './shared/types';
import { AudioResynthControl } from './AudioResynthControl';

// The placeholder of a speech answer whose audio is gone (#281): pruned by the retention
// setting, deleted, never stored, or aborted. The stored answer carries one marker for all of
// them (`audio_removed`, see hasRemovedAudio), so this says one thing -- the audio is not
// stored -- and offers the one remedy: regenerate the answer. It is the audio twin of AudioTurn
// with nothing to play or save, so it has no player and no download; what the answer's text
// holds, if anything, is rendered by the bubble around it.
//
// Regenerating is the row's own handler, handed down by ChatMessage: the marker answer is a
// real transcript message with its own id, so the replay starts at its prompt exactly as the
// action stack's regenerate does. `disabled` is that control's gate too (no run may start
// while the model is unavailable). Without an `onRegenerate` the line stands alone.
//
// A spoken SECTION of a voice answer can be made again on its own (#324): the caller that can do
// that hands an `onResynth` (the section's re-synthesis, a request for its audio as a Blob), and
// the line gains the play control that runs it (AudioResynthControl) beside the regenerate. The
// control shares `disabled` with it. Without an `onResynth` the line is what it always was.
export function AudioNotStoredTurn({
  t,
  onRegenerate,
  onResynth,
  disabled = false,
}: Readonly<{
  t: Translation;
  onRegenerate?: () => void;
  onResynth?: () => Promise<Blob>;
  disabled?: boolean;
}>) {
  const labelId = useId();
  return (
    <Box
      role="group"
      aria-labelledby={labelId}
      sx={{
        display: 'inline-flex',
        flexWrap: 'wrap',
        alignItems: 'center',
        gap: 1,
        py: 0.5,
        pl: 1.5,
        pr: 0.75,
        mb: 1,
        maxWidth: '100%',
        border: '1px dashed var(--line)',
        borderRadius: '8px',
        bgcolor: 'var(--page)',
      }}
    >
      <Typography id={labelId} component="span" sx={{ color: 'var(--muted)', fontSize: 13 }}>
        {t.chatAudioNotStored}
      </Typography>
      {onResynth && <AudioResynthControl t={t} onResynth={onResynth} disabled={disabled} />}
      {onRegenerate && (
        <Button
          size="small"
          startIcon={<ReplayIcon fontSize="small" />}
          onClick={onRegenerate}
          disabled={disabled}
        >
          {t.chatRegenerate}
        </Button>
      )}
    </Box>
  );
}
