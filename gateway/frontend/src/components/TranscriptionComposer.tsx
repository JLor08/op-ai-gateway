// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { SubmitEvent } from 'react';
import { Box, Button, CircularProgress, IconButton, Tooltip, Typography } from '@mui/material';
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
// checks and the same conversion, in the browser, to the WAV the transcription server reads, so
// the clip shown here (and played by its preview) is always an audio/wav. This view only offers
// the controls and shows the result. The conversion takes a moment for a long clip, so while it
// runs (c.audioConverting) the composer says so beside the attach controls and holds them -- and
// Send -- until the clip has landed or the attach has failed, so nothing is stacked on it and
// nothing is sent from under it.
export function TranscriptionComposer({ t }: Readonly<{ t: Translation }>) {
  const c = useChatStore();
  const labelId = 'chat-transcription-input-label';

  function submit(event: SubmitEvent<HTMLFormElement>) {
    event.preventDefault();
    c.send();
  }

  // What holds the attach controls and Send besides a running chat: a clip being converted.
  const busy = c.streaming || c.audioConverting;

  // Why Send is closed, when it is (and nothing is running): the model is unreachable, or there is
  // no clip yet. A running chat disables it without a word, like the text composer's, and so does
  // a conversion (the indicator beside the attach controls is what says so, and "attach audio
  // first" would contradict the clip that is just arriving).
  let sendHint = '';
  if (!c.streaming && !c.modelAvailable) sendHint = t.chatModelUnavailable;
  else if (!busy && c.audio === null) sendHint = t.chatTranscriptionNeedsAudio;

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
            disabled={busy}
            onRecorded={(blob) => void c.attachRecording(blob)}
          />
          {/* A label-wrapped file input, the same shape as the image attach of the text
            composer: the input is named by the button's text, and a disabled one (a run is in
            flight) is not clickable. `accept` offers any audio (the browser converts it to WAV),
            and is only a hint, which a file dialog's "all files" ignores, so the store checks the
            file again. One clip is the turn, so no `multiple`. */}
          <Button
            component="label"
            variant="outlined"
            size="small"
            startIcon={<AudioFileIcon />}
            disabled={busy}
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
          {/* The conversion in flight: a spinner and its label, announced as a status. The
            spinner is decoration (the label is the message), so it is hidden from the
            accessibility tree. */}
          {c.audioConverting && (
            <Box
              role="status"
              sx={{ display: 'inline-flex', alignItems: 'center', gap: 1, minHeight: 30 }}
            >
              <CircularProgress size={16} aria-hidden />
              <Typography variant="body2" color="text.secondary">
                {t.chatTranscriptionConverting}
              </Typography>
            </Box>
          )}
        </Box>
      </Box>
      <Box>
        <Tooltip title={sendHint} describeChild>
          <span>
            <Button
              type="submit"
              variant="contained"
              disabled={busy || !c.modelAvailable || c.audio === null}
            >
              {t.send}
            </Button>
          </span>
        </Tooltip>
      </Box>
    </Box>
  );
}
