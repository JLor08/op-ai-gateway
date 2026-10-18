// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useEffect, useState } from 'react';
import { Box, IconButton, Tooltip, Typography } from '@mui/material';
import DownloadIcon from '@mui/icons-material/Download';
import { downloadBinary, isFetchableUrl } from './shared/downloadBinary';
import type { Translation } from './shared/types';

// What the player is showing. A speech turn's REPLY is audio the gateway generated; a
// transcription turn's INPUT is the user's own clip (the user bubble). Playback, the
// object-URL lifecycle and the download are one and the same for both -- only the words
// differ, so the clip is neither announced nor saved as "generated" audio.
export type AudioVariant = 'reply' | 'input';

// A download name is built from the TURN's own id, for the reason ImageTurn spells out
// at length: without it every speech turn of a conversation would offer the same
// `generated-audio.wav` and the second save would silently become "(1)". The id is the
// transcript message's own (ChatUiMessage.id, threaded down through ChatMessage); it is
// filtered to the characters a file name can carry because it comes back out of a
// persisted document, and an empty id (only the standalone-props tests render
// ChatMessage without one) falls back to the bare name. An input clip is named
// `input-audio-...` rather than `generated-audio-...`: it is not something the gateway
// generated.
function downloadNameFor(variant: AudioVariant, turnId: string, extension: string): string {
  const safeId = turnId.replace(/[^A-Za-z0-9._-]/g, '-');
  const stem = variant === 'input' ? 'input-audio' : 'generated-audio';
  const base = safeId ? `${stem}-${safeId}` : stem;
  return `${base}.${extension}`;
}

// An inline reply: `data:audio/wav;base64,...`, what the run carries while it streams
// and what an old transcript may still hold. The element can play it as it is.
function isInlineAudio(url: string): boolean {
  return url.startsWith('data:audio/');
}

// What the <audio> element is given for one reply, resolved from the url a persisted
// transcript holds.
//
// A STORED reply is a short gateway url (/api/portal/chat-blobs/{id}). It is NOT handed
// to the element as it is: the blob endpoint is locked down (ADR-056) and answers no
// Range requests, and Safari/iOS will not play a media url that cannot be range-read.
// So the bytes are fetched ONCE, same-origin with the session cookie (the endpoint is
// owner-scoped), and the element plays an object URL made from them.
//
// An object URL pins the whole clip in memory until it is revoked, and a transcript can
// hold many audio turns, so the effect's cleanup revokes it -- when the turn unmounts
// and when the url changes. A request still in flight at that point is aborted, and a
// response that lands after the abort creates nothing (nothing would ever revoke it).
//
// Anything that is neither of the two shapes the gateway produces is `failed` outright:
// the url comes out of a persisted document, and only a rooted chat-blobs path is ever
// fetched with the cookie attached (isFetchableUrl), only `data:audio/` ever played.
type AudioSource = { src: string | undefined; failed: boolean };

function useAudioSource(url: string): AudioSource {
  const inline = isInlineAudio(url);
  const stored = !inline && isFetchableUrl(url);
  // `url` is part of the state so a result is only ever read for the url it was made
  // from: after a url change the old object URL (already revoked by the cleanup) is
  // never shown, without a reset-in-an-effect.
  const [resolved, setResolved] = useState<{ url: string; src: string | null } | null>(null);

  useEffect(() => {
    if (!stored) return;
    const controller = new AbortController();
    let objectUrl: string | undefined;
    const load = async () => {
      try {
        const response = await fetch(url, {
          credentials: 'same-origin',
          signal: controller.signal,
        });
        if (!response.ok) throw new Error(`audio fetch failed: ${response.status}`);
        const blob = await response.blob();
        if (controller.signal.aborted) return;
        objectUrl = URL.createObjectURL(blob);
        setResolved({ url, src: objectUrl });
      } catch {
        if (!controller.signal.aborted) setResolved({ url, src: null });
      }
    };
    void load();
    return () => {
      controller.abort();
      if (objectUrl) URL.revokeObjectURL(objectUrl);
    };
  }, [url, stored]);

  if (inline) return { src: url, failed: false };
  if (!stored) return { src: undefined, failed: true };
  const current = resolved?.url === url ? resolved : null;
  return { src: current?.src ?? undefined, failed: current !== null && current.src === null };
}

// Renders one audio_url part: an HTML5 `<audio controls>` player and a download button.
// It serves the speech ASSISTANT turn (the reply, variant "reply", the default -- exactly
// ONE WAV, the executor writes a single audio part) and the transcription USER turn (the
// attached or recorded clip, variant "input"). Unlike ImageTurn it takes one url, not a
// list; a turn with several parts renders one player per part.
//
// The download handler lives here, next to the url it downloads, rather than arriving
// from ChatMessage as an onDownload prop -- an inline arrow built in the parent's render
// would give every message a new callback identity on every render and defeat
// ChatMessage's shallow-compare memo. downloadBinary fetches a stored reply's bytes
// itself (a second request, only on click; the playback copy is not reused so the saved
// file's type and name come from what the server actually served).
//
// Both failure modes are visible inline next to the player, not a toast: AudioTurn,
// like ImageTurn, must render without ToastProvider context in its standalone-props
// tests.
export function AudioTurn({
  t,
  url,
  turnId,
  variant = 'reply',
}: Readonly<{
  t: Translation;
  url: string;
  turnId: string;
  variant?: AudioVariant;
}>) {
  const { src, failed } = useAudioSource(url);
  // Whether the most recent download attempt failed -- a truncated persisted data: URL
  // decodes to nothing, a gateway url whose fetch fails (network, 404 for a purged
  // blob) saves nothing. downloadBinary reports these as `false`; it must not fail
  // silently. Cleared on the next successful download.
  const [downloadFailed, setDownloadFailed] = useState(false);

  return (
    <Box
      sx={{
        display: 'inline-flex',
        flexDirection: 'column',
        gap: 0.5,
        p: 0.75,
        mb: 1,
        maxWidth: '100%',
        border: '1px solid var(--line)',
        borderRadius: '8px',
        bgcolor: 'var(--page)',
      }}
    >
      <Box sx={{ display: 'flex', alignItems: 'center', gap: 0.5, minWidth: 0 }}>
        {/* No src until a stored clip's bytes have arrived: the element renders as an
            inert player meanwhile, which is the loading state. */}
        <Box
          component="audio"
          controls
          preload="metadata"
          src={src}
          aria-label={variant === 'input' ? t.chatAttachedAudio : t.chatGeneratedAudio}
          sx={{ display: 'block', width: 320, maxWidth: '100%', minWidth: 0 }}
        />
        <Tooltip title={t.chatDownloadAudio}>
          <IconButton
            size="small"
            aria-label={t.chatDownloadAudio}
            onClick={() => {
              // The name is a function of the extension because a stored reply's media
              // type is only known once its bytes have been fetched. downloadBinary
              // resolves false for every failure it knows of; the catch is for the one
              // it does not (a browser API throwing while the file is built), so that
              // surfaces as the same inline error instead of an unhandled rejection.
              void downloadBinary((extension) => downloadNameFor(variant, turnId, extension), url)
                .then((ok) => setDownloadFailed(!ok))
                .catch(() => setDownloadFailed(true));
            }}
          >
            <DownloadIcon fontSize="small" />
          </IconButton>
        </Tooltip>
      </Box>
      {failed && (
        <Typography role="alert" color="error" sx={{ fontSize: 12 }}>
          {t.chatAudioLoadError}
        </Typography>
      )}
      {downloadFailed && (
        <Typography role="alert" color="error" sx={{ fontSize: 12 }}>
          {t.chatAudioDownloadError}
        </Typography>
      )}
    </Box>
  );
}
