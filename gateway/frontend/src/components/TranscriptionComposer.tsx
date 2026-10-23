// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useEffect, useRef, useState, type SubmitEvent } from 'react';
import {
  Box,
  Button,
  CircularProgress,
  IconButton,
  ToggleButton,
  ToggleButtonGroup,
  Tooltip,
  Typography,
} from '@mui/material';
import AudioFileIcon from '@mui/icons-material/AudioFile';
import CloseIcon from '@mui/icons-material/Close';
import FiberManualRecordIcon from '@mui/icons-material/FiberManualRecord';
import MicIcon from '@mui/icons-material/Mic';
import StopIcon from '@mui/icons-material/Stop';
import type { Translation } from './shared/types';
import { AudioRecorder } from './AudioRecorder';
import {
  TRANSCRIPTION_AUDIO_ACCEPT,
  liveSessionRunning,
  useChatStore,
  type LiveTranscriptionPhase,
} from './chat/ChatStore';

// The ways to give the turn its audio. `live` streams the microphone while the user speaks
// (offered only for a model the gateway says can stream a transcription); `file` and `record`
// are the clip attached or recorded in the browser and sent whole.
type InputMode = 'file' | 'record' | 'live';

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
//
// For a model that can stream (c.transcriptionStreaming) there is a third way, LIVE: the user
// speaks and the transcript appears as they talk, and ending the recording puts the finished
// transcript into the chat as a turn of its own -- there is no clip, so nothing waits for Send.
// A switch then chooses between the three, showing only the chosen one's input; without that
// offer there is no switch and the file and the recorder are both there, as they always were.
export function TranscriptionComposer({ t }: Readonly<{ t: Translation }>) {
  const c = useChatStore();
  const labelId = 'chat-transcription-input-label';
  const [chosen, setChosen] = useState<InputMode>('file');
  const live = c.liveSession;
  const liveRunning = liveSessionRunning(live.state);
  const mode: InputMode | 'both' = c.transcriptionStreaming ? chosen : 'both';

  // The composer is only mounted while the thread is a transcription one on the chat view: when
  // it goes -- another model or thread, or another page of the portal -- a recording must not
  // stay behind with the microphone on and nothing on screen to stop it, nor an error with its
  // transcript to reappear later. The one thing left to run is a recording that is already
  // finishing: its microphone is off, and its transcript is still on its way to the thread.
  const liveStateRef = useRef<LiveTranscriptionPhase>(live.state);
  liveStateRef.current = live.state;
  const abortLiveRef = useRef(c.abortLiveTranscription);
  abortLiveRef.current = c.abortLiveTranscription;
  useEffect(
    () => () => {
      if (liveStateRef.current !== 'idle' && liveStateRef.current !== 'finalizing') {
        abortLiveRef.current();
      }
    },
    [],
  );

  function chooseMode(next: InputMode | null) {
    if (next === null || next === chosen) return;
    // Leaving the live panel drops a failed recording's error and transcript with it.
    if (live.state === 'error') c.abortLiveTranscription();
    setChosen(next);
  }

  function submit(event: SubmitEvent<HTMLFormElement>) {
    event.preventDefault();
    c.send();
  }

  // What holds the attach controls and Send besides a running chat: a clip being converted, or a
  // live recording under way (it commits its own turn).
  const busy = c.streaming || c.audioConverting || liveRunning;

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
        {c.transcriptionStreaming && (
          <ToggleButtonGroup
            exclusive
            size="small"
            value={chosen}
            onChange={(_event, next: InputMode | null) => chooseMode(next)}
            disabled={liveRunning}
            aria-label={t.chatTranscriptionMode}
            sx={{ justifySelf: 'start' }}
          >
            <ToggleButton value="file">{t.chatTranscriptionModeFile}</ToggleButton>
            <ToggleButton value="record">{t.chatTranscriptionModeRecord}</ToggleButton>
            <ToggleButton value="live">{t.chatTranscriptionModeLive}</ToggleButton>
          </ToggleButtonGroup>
        )}
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
          {(mode === 'both' || mode === 'record') && (
            <AudioRecorder
              t={t}
              disabled={busy}
              onRecorded={(blob) => void c.attachRecording(blob)}
            />
          )}
          {/* A label-wrapped file input, the same shape as the image attach of the text
            composer: the input is named by the button's text, and a disabled one (a run is in
            flight) is not clickable. `accept` offers any audio (the browser converts it to WAV),
            and is only a hint, which a file dialog's "all files" ignores, so the store checks the
            file again. One clip is the turn, so no `multiple`. */}
          {(mode === 'both' || mode === 'file') && (
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
          )}
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
        {mode === 'live' && <LiveRecording t={t} />}
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

// The live recording's panel: what it is (the hint), Start and Stop, the state it is in, the
// transcript as it grows, and -- after a failure -- what went wrong and what can still be done
// with the text that came before it. The recording itself is the store's (c.liveSession and its
// controls); this only draws it.
function LiveRecording({ t }: Readonly<{ t: Translation }>) {
  const c = useChatStore();
  const live = c.liveSession;
  const running = liveSessionRunning(live.state);
  // After a failure only: the transcript that came before it can still be kept, or thrown away.
  const salvageable = live.state === 'error' && live.transcript.trim() !== '';

  // A spinner with its label, or the recording dot with its label, announced as a status. The
  // spinner and the dot are decoration (the label is the message), so they are hidden from the
  // accessibility tree.
  let status: { label: string; busy: boolean } | null = null;
  if (live.state === 'connecting') {
    status = { label: t.chatTranscriptionLiveConnecting, busy: true };
  } else if (live.state === 'listening') {
    status = { label: t.chatTranscriptionLiveListening, busy: false };
  } else if (live.state === 'reconnecting') {
    status = { label: t.chatTranscriptionLiveReconnecting, busy: true };
  } else if (live.state === 'finalizing') {
    status = { label: t.chatTranscriptionRunPending, busy: true };
  }

  return (
    <Box role="group" aria-label={t.chatTranscriptionLive} sx={{ display: 'grid', gap: 1 }}>
      <Typography variant="caption" color="text.secondary">
        {t.chatTranscriptionLiveHint}
      </Typography>
      <Box sx={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 1 }}>
        {!running && (
          <Button
            variant="outlined"
            size="small"
            startIcon={<MicIcon />}
            // Straight from the click, with nothing awaited first: the microphone and the
            // browser's audio are only granted to a call made inside the user's gesture.
            onClick={() => c.startLiveTranscription()}
            // Not while a transcript is waiting to be used or discarded: Start begins from a clean
            // slate, so pressing it would throw that text away without the user having decided to.
            disabled={c.streaming || c.audioConverting || !c.modelAvailable || salvageable}
          >
            {t.chatTranscriptionLiveStart}
          </Button>
        )}
        {running && live.state !== 'finalizing' && (
          <Button
            variant="contained"
            color="error"
            size="small"
            startIcon={<StopIcon />}
            onClick={() => c.stopLiveTranscription()}
          >
            {t.chatTranscriptionLiveStop}
          </Button>
        )}
        {status && (
          <Box
            role="status"
            sx={{ display: 'inline-flex', alignItems: 'center', gap: 1, minHeight: 30 }}
          >
            {status.busy ? (
              <CircularProgress size={16} aria-hidden />
            ) : (
              <FiberManualRecordIcon fontSize="small" color="error" aria-hidden />
            )}
            <Typography variant="body2" color="text.secondary">
              {status.label}
            </Typography>
          </Box>
        )}
      </Box>
      {live.state !== 'idle' && (
        <Box
          role="region"
          aria-label={t.chatTranscriptionLiveTranscript}
          aria-live="polite"
          sx={{
            minHeight: 56,
            maxHeight: 240,
            overflowY: 'auto',
            p: 1,
            border: '1px solid var(--line)',
            borderRadius: '8px',
            bgcolor: 'var(--page)',
            color: 'var(--text)',
            lineHeight: 1.55,
            whiteSpace: 'pre-wrap',
            overflowWrap: 'anywhere',
          }}
        >
          {live.transcript}
        </Box>
      )}
      {live.state === 'error' && (
        <Typography role="alert" color="error" variant="caption">
          {live.error}
        </Typography>
      )}
      {salvageable && (
        <Box sx={{ display: 'flex', flexWrap: 'wrap', gap: 1 }}>
          <Button variant="contained" size="small" onClick={() => c.keepLiveTranscript()}>
            {t.chatTranscriptionLiveUse}
          </Button>
          <Button variant="outlined" size="small" onClick={() => c.abortLiveTranscription()}>
            {t.chatRecordDiscard}
          </Button>
        </Box>
      )}
    </Box>
  );
}
