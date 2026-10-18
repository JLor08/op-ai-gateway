// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { SubmitEvent } from 'react';
import { Box, Button, IconButton, Tooltip, Typography } from '@mui/material';
import AudioFileIcon from '@mui/icons-material/AudioFile';
import CloseIcon from '@mui/icons-material/Close';
import type { Translation } from './shared/types';
import { AudioRecorder } from './AudioRecorder';
import { TRANSCRIPTION_AUDIO_ACCEPT, useChatStore } from './chat/ChatStore';

// The composer of a transcription (speech-to-text) thread. There is no text prompt: the turn IS an
// audio clip, attached from a file or recorded in the browser, and the reply is its transcript. So
// the composer is the two ways to get a clip (the attach button and the recorder), the clip that
// is waiting to be sent -- with a player, so the user can check it is the right one -- and Send,
// which stays closed until there is one.
//
// A clip is attached by the store (handleFiles / attachRecording): both ways go through the same
// format and size checks, and a clip leaves under its canonical media type. This view only offers
// the controls and shows the result.
export function TranscriptionComposer({ t }: Readonly<{ t: Translation }>) {
  const c = useChatStore();
  const labelId = 'chat-transcription-input-label';

  function submit(event: SubmitEvent<HTMLFormElement>) {
    event.preventDefault();
    c.send();
  }

  // Why Send is closed, when it is (and nothing is running): the model is unreachable, or there is
  // no clip yet. A running chat disables it without a word, like the text composer's.
  let sendHint = '';
  if (!c.streaming && !c.modelAvailable) sendHint = t.chatModelUnavailable;
  else if (!c.streaming && c.audio === null) sendHint = t.chatTranscriptionNeedsAudio;

  return (
    <Box component="form" sx={{ display: 'grid', gap: 1.5 }} onSubmit={submit}>
      <Box role="group" aria-labelledby={labelId} sx={{ display: 'grid', gap: 1 }}>
        <Box>
          <Typography id={labelId} sx={{ fontWeight: 700 }}>
            {t.chatTranscriptionInputLabel}
          </Typography>
          <Typography variant="caption" color="text.secondary">
            {t.chatTranscriptionOnlyHint}
          </Typography>
        </Box>
        {c.audio !== null && (
          <Box
            sx={{
              display: 'flex',
              flexWrap: 'wrap',
              alignItems: 'center',
              gap: 1,
              p: 0.75,
              maxWidth: '100%',
              border: '1px solid var(--line)',
              borderRadius: '8px',
              bgcolor: 'var(--page)',
            }}
          >
            {/* A picked file is shown under its own name; a recording has none. */}
            <Typography noWrap sx={{ maxWidth: 240 }}>
              {c.audio.name || t.chatAudioRecordingLabel}
            </Typography>
            <Box
              component="audio"
              controls
              preload="metadata"
              src={c.audio.url}
              aria-label={t.chatAttachedAudio}
              sx={{ display: 'block', width: 320, maxWidth: '100%', minWidth: 0 }}
            />
            <IconButton
              size="small"
              aria-label={t.chatRemoveAudio}
              onClick={c.removeAudio}
              disabled={c.streaming}
            >
              <CloseIcon fontSize="small" />
            </IconButton>
          </Box>
        )}
        <Box sx={{ display: 'flex', flexWrap: 'wrap', alignItems: 'flex-start', gap: 1 }}>
          <AudioRecorder
            t={t}
            disabled={c.streaming}
            onRecorded={(blob) => void c.attachRecording(blob)}
          />
          {/* A label-wrapped file input, the same shape as the image attach of the text
            composer: the input is named by the button's text, and a disabled one (a run is in
            flight) is not clickable. `accept` is only a hint, which a file dialog's "all files"
            ignores, so the store checks the type again. One clip is the turn, so no `multiple`. */}
          <Button
            component="label"
            variant="outlined"
            size="small"
            startIcon={<AudioFileIcon />}
            disabled={c.streaming}
          >
            {t.chatAttachAudio}
            <input
              type="file"
              accept={TRANSCRIPTION_AUDIO_ACCEPT}
              hidden
              onChange={(event) => {
                void c.handleFiles(event.target.files);
                event.target.value = '';
              }}
            />
          </Button>
        </Box>
      </Box>
      <Box>
        <Tooltip title={sendHint} describeChild>
          <span>
            <Button
              type="submit"
              variant="contained"
              disabled={c.streaming || !c.modelAvailable || c.audio === null}
            >
              {t.send}
            </Button>
          </span>
        </Tooltip>
      </Box>
    </Box>
  );
}
