// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useEffect, useRef, useState } from 'react';
import { Box, Button, Stack, Typography } from '@mui/material';
import CheckIcon from '@mui/icons-material/Check';
import DeleteIcon from '@mui/icons-material/Delete';
import FiberManualRecordIcon from '@mui/icons-material/FiberManualRecord';
import MicIcon from '@mui/icons-material/Mic';
import StopIcon from '@mui/icons-material/Stop';
import type { Translation } from './shared/types';

// How often the clock follows the recording. The clock shows whole seconds; a quarter of a second
// keeps it from lagging a visible beat behind the true second without re-rendering often.
const CLOCK_TICK_MS = 250;

// "m:ss": the clock of a recording (0:07, 1:05), always minutes and seconds like a voice recorder.
function formatClock(ms: number): string {
  const total = Math.max(0, Math.floor(ms / 1000));
  return `${Math.floor(total / 60)}:${String(total % 60).padStart(2, '0')}`;
}

// A getUserMedia refusal by the user or the browser's policy (NotAllowedError is the standard
// name, PermissionDeniedError the old one, SecurityError a page the browser will not give the
// microphone to). Anything else -- no device, a device that is busy, an unknown failure -- is just
// a failed recording: the wording differs because only the first has a fix the user can make in
// the browser's settings.
//
// Read by `name`, never `instanceof`: the rejection is a DOMException, which is not an Error of
// this realm in every environment (a cross-realm one, jsdom's), and the name is the contract.
function isPermissionDenied(err: unknown): boolean {
  const name = typeof err === 'object' && err !== null && 'name' in err ? String(err.name) : '';
  return name === 'NotAllowedError' || name === 'PermissionDeniedError' || name === 'SecurityError';
}

// Whether this browser can record at all: it has MediaRecorder and the microphone API. The latter
// exists only on a page the browser treats as secure (HTTPS, or localhost), so an insecure origin
// lands here too.
function canRecord(): boolean {
  return (
    typeof MediaRecorder !== 'undefined' &&
    typeof navigator !== 'undefined' &&
    typeof navigator.mediaDevices?.getUserMedia === 'function'
  );
}

// Asks the browser for the microphone (audio only): the granted stream, or whether the refusal was
// a permission one (see isPermissionDenied) as opposed to any other failure.
async function requestMicrophone(): Promise<{ stream: MediaStream } | { denied: boolean }> {
  try {
    return { stream: await navigator.mediaDevices.getUserMedia({ audio: true }) };
  } catch (err) {
    return { denied: isPermissionDenied(err) };
  }
}

function stopTracks(stream: MediaStream | null) {
  stream?.getTracks().forEach((track) => track.stop());
}

// Where the recorder is: waiting to be started, waiting for the browser's permission prompt,
// recording, or holding a finished take the user has not yet used or discarded.
type Phase = 'idle' | 'requesting' | 'recording' | 'recorded';

// The in-browser recorder of the transcription composer: ask for the microphone, record with a
// visible clock, stop, hear the take, then use it or discard it. "Use" hands the recording to the
// composer as a Blob typed as the browser's recorder reports it (audio/webm;codecs=opus in
// Chromium, audio/mp4 in Safari, audio/ogg in Firefox): converting that to the WAV the
// transcription server reads is the composer's job (audioAttach), the same for any clip, so the
// take is previewed here as it was recorded and attached as the WAV. Nothing is attached until
// the user says so.
//
// The microphone is a resource that must never be left held: the stream's tracks are stopped the
// moment a take ends, fails or is abandoned, and when the component goes away -- mid-recording, or
// while the browser's permission prompt is still open (a grant that arrives after that is let go
// at once). The preview's object URL pins the whole take in memory until it is revoked, so it is
// revoked when the take is used, discarded or abandoned.
export function AudioRecorder({
  t,
  disabled = false,
  onRecorded,
}: Readonly<{
  t: Translation;
  // True while a run is in flight: nothing new may be started or used then. A take already in
  // progress can still be stopped or discarded.
  disabled?: boolean;
  onRecorded: (blob: Blob) => void;
}>) {
  const [phase, setPhase] = useState<Phase>('idle');
  const [elapsedMs, setElapsedMs] = useState(0);
  const [error, setError] = useState('');
  // The finished take and the object URL its preview plays. They are one state so the URL is
  // only ever read together with the blob it was made from, and revoked when that changes.
  const [take, setTake] = useState<{ blob: Blob; url: string } | null>(null);

  // The live recording's resources, mutated by the recorder's events and read by the cleanup.
  const recorderRef = useRef<MediaRecorder | null>(null);
  const streamRef = useRef<MediaStream | null>(null);
  const chunksRef = useRef<Blob[]>([]);
  const clockRef = useRef<ReturnType<typeof setInterval> | null>(null);
  const startedAtRef = useRef(0);
  // False once the component is gone: an event or a promise that lands after that must not set
  // state, and must not leave a microphone open.
  const aliveRef = useRef(true);

  function stopClock() {
    if (clockRef.current !== null) {
      clearInterval(clockRef.current);
      clockRef.current = null;
    }
  }

  // Ends the live recorder without delivering anything: its handlers are detached first, so the
  // `dataavailable`/`stop` a stop() still fires (asynchronously, in a browser) reach nobody. Then
  // the clock stops and the microphone is released.
  function abandonRecording() {
    const recorder = recorderRef.current;
    recorderRef.current = null;
    if (recorder) {
      recorder.ondataavailable = null;
      recorder.onstop = null;
      recorder.onerror = null;
      if (recorder.state !== 'inactive') {
        try {
          recorder.stop();
        } catch {
          // Already ended: nothing left to stop.
        }
      }
    }
    stopClock();
    stopTracks(streamRef.current);
    streamRef.current = null;
    chunksRef.current = [];
  }

  // Everything is let go when the recorder leaves: a take in progress is stopped (and thrown
  // away), the clock and the microphone released.
  useEffect(() => {
    aliveRef.current = true;
    return () => {
      aliveRef.current = false;
      abandonRecording();
    };
    // abandonRecording only reads refs, so it is the same function in effect on every render.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // The preview's object URL lives exactly as long as its take: revoked when the take is used or
  // discarded (take becomes null) and when the recorder unmounts with one still on offer.
  useEffect(() => {
    if (!take) return;
    return () => URL.revokeObjectURL(take.url);
  }, [take]);

  function fail() {
    abandonRecording();
    if (!aliveRef.current) return;
    setError(t.chatRecordFailed);
    setPhase('idle');
  }

  // The recorder's `stop` event: every chunk has been delivered, so the take is whole.
  function finish(recorder: MediaRecorder) {
    if (recorderRef.current !== recorder) return;
    recorderRef.current = null;
    stopClock();
    stopTracks(streamRef.current);
    streamRef.current = null;
    const chunks = chunksRef.current;
    chunksRef.current = [];
    if (!aliveRef.current) return;
    const blob = new Blob(chunks, { type: recorder.mimeType || chunks[0]?.type || '' });
    // A take of no bytes (stopped before the first chunk, or a recorder that produced none) is
    // no recording: there is nothing to preview or send.
    if (blob.size === 0) {
      setError(t.chatRecordFailed);
      setPhase('idle');
      return;
    }
    setTake({ blob, url: URL.createObjectURL(blob) });
    setPhase('recorded');
  }

  // Starts a recorder on a granted microphone stream and the clock that follows it. A recorder
  // that cannot be built (a configuration the browser does not support) or cannot start (the device
  // ended between the grant and the start: InvalidStateError, NotSupportedError) is the same
  // failure: the stream is released again and the recorder is back to idle with an error, so the
  // microphone is not left live and Record is not left disabled. The recorder is only recorded in
  // the refs once it has started, so nothing of a failed one is left for the cleanup to find.
  function beginRecording(stream: MediaStream) {
    let recorder: MediaRecorder;
    try {
      recorder = new MediaRecorder(stream);
      chunksRef.current = [];
      recorder.ondataavailable = (event) => {
        if (event.data.size > 0) chunksRef.current.push(event.data);
      };
      recorder.onstop = () => finish(recorder);
      recorder.onerror = fail;
      recorder.start();
    } catch {
      stopTracks(stream);
      setError(t.chatRecordFailed);
      setPhase('idle');
      return;
    }
    streamRef.current = stream;
    recorderRef.current = recorder;
    startedAtRef.current = Date.now();
    setElapsedMs(0);
    clockRef.current = setInterval(
      () => setElapsedMs(Date.now() - startedAtRef.current),
      CLOCK_TICK_MS,
    );
    setPhase('recording');
  }

  async function start() {
    if (phase !== 'idle') return;
    setError('');
    // No microphone API at all is said before the permission prompt, which would otherwise never
    // be reached.
    if (!canRecord()) {
      setError(t.chatRecordUnsupported);
      return;
    }
    setPhase('requesting');
    const grant = await requestMicrophone();
    // A grant that came after the recorder left: nobody is there to record, so it is let go at
    // once rather than held open (the browser's recording indicator would stay on).
    if (!aliveRef.current) {
      if ('stream' in grant) stopTracks(grant.stream);
      return;
    }
    if ('stream' in grant) {
      beginRecording(grant.stream);
      return;
    }
    setError(grant.denied ? t.chatRecordDenied : t.chatRecordFailed);
    setPhase('idle');
  }

  function stop() {
    const recorder = recorderRef.current;
    if (recorder?.state === 'recording') {
      // The clock stops with the click; the take itself is finished by the recorder's own `stop`
      // event, once its last chunk is in.
      stopClock();
      recorder.stop();
    }
  }

  function useTake() {
    if (!take) return;
    onRecorded(take.blob);
    setTake(null);
    setPhase('idle');
  }

  function discardTake() {
    setTake(null);
    setPhase('idle');
  }

  return (
    <Box sx={{ display: 'grid', gap: 1 }}>
      <Stack direction="row" sx={{ flexWrap: 'wrap', alignItems: 'center', gap: 1 }}>
        {(phase === 'idle' || phase === 'requesting') && (
          <Button
            variant="outlined"
            size="small"
            startIcon={<MicIcon />}
            onClick={() => void start()}
            disabled={disabled || phase === 'requesting'}
          >
            {t.chatRecordStart}
          </Button>
        )}
        {phase === 'requesting' && (
          <Typography variant="caption" color="text.secondary">
            {t.chatRecordRequesting}
          </Typography>
        )}
        {phase === 'recording' && (
          <>
            <Button
              variant="contained"
              color="error"
              size="small"
              startIcon={<StopIcon />}
              onClick={stop}
            >
              {t.chatRecordStop}
            </Button>
            <Box sx={{ display: 'inline-flex', alignItems: 'center', gap: 0.5 }}>
              <FiberManualRecordIcon fontSize="small" color="error" aria-hidden />
              <Typography
                component="span"
                role="timer"
                aria-label={t.chatRecordElapsed}
                sx={{ fontVariantNumeric: 'tabular-nums' }}
              >
                {formatClock(elapsedMs)}
              </Typography>
            </Box>
          </>
        )}
      </Stack>
      {phase === 'recorded' && take && (
        <Stack direction="row" sx={{ flexWrap: 'wrap', alignItems: 'center', gap: 1 }}>
          <Box
            component="audio"
            controls
            preload="metadata"
            src={take.url}
            aria-label={t.chatRecordPreview}
            sx={{ display: 'block', width: 320, maxWidth: '100%', minWidth: 0 }}
          />
          <Button
            variant="contained"
            size="small"
            startIcon={<CheckIcon />}
            onClick={useTake}
            disabled={disabled}
          >
            {t.chatRecordUse}
          </Button>
          <Button variant="outlined" size="small" startIcon={<DeleteIcon />} onClick={discardTake}>
            {t.chatRecordDiscard}
          </Button>
        </Stack>
      )}
      {error && (
        <Typography role="alert" color="error" variant="caption">
          {error}
        </Typography>
      )}
    </Box>
  );
}
