// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useEffect, useRef, useState } from 'react';
import { Box, IconButton, Tooltip, Typography } from '@mui/material';
import DownloadIcon from '@mui/icons-material/Download';
import {
  dataUrlToBlob,
  downloadBinary,
  downloadBlob,
  isFetchableUrl,
  type DownloadName,
} from './shared/downloadBinary';
import type { Translation } from './shared/types';
import { concatWavBlobs } from './shared/wavConcat';

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

// What the <audio> element is given for one reply, resolved from the url(s) a persisted
// transcript holds.
//
// A STORED reply is a short gateway url (/api/portal/chat-blobs/{id}). It is NOT handed
// to the element as it is: the blob endpoint is locked down (ADR-056) and answers no
// Range requests, and Safari/iOS will not play a media url that cannot be range-read.
// So the bytes are fetched ONCE, same-origin with the session cookie (the endpoint is
// owner-scoped), and the element plays an object URL made from them.
//
// A long answer is saved as SEVERAL ordered parts (each a standalone WAV). They are all
// fetched the same way and joined into one clip (wavConcat) -- one object URL, so the
// element plays the answer without a gap at the seams and the download is one file. The
// joined blob is handed back too, so the download saves exactly what plays. A single part
// takes no part in any of that: it is the blob the server served, as it is.
//
// An object URL pins the whole clip in memory until it is revoked, and a transcript can
// hold many audio turns, so the effect's cleanup revokes it -- when the turn unmounts
// and when the urls change. A request still in flight at that point is aborted, and a
// response that lands after the abort creates nothing (nothing would ever revoke it).
//
// Anything that is neither of the two shapes the gateway produces is `failed` outright:
// the url comes out of a persisted document, and only a rooted chat-blobs path is ever
// fetched with the cookie attached (isFetchableUrl), only `data:audio/` ever played.
type AudioSource = { src: string | undefined; failed: boolean; blob: Blob | null };

// One part's bytes: a stored part is fetched (same-origin, with the session cookie), an
// inline one -- what the run's own result carries until the chat is stored -- is decoded in
// place. The caller has checked the shape (playableParts), so nothing else is fetched.
async function loadPart(url: string, signal: AbortSignal | undefined): Promise<Blob> {
  if (isInlineAudio(url)) {
    const blob = dataUrlToBlob(url);
    if (!blob) throw new Error('audio part is not decodable');
    return blob;
  }
  const response = await fetch(url, { credentials: 'same-origin', signal });
  if (!response.ok) throw new Error(`audio fetch failed: ${response.status}`);
  return response.blob();
}

// Whether every part is something the gateway produces (see above). A single inline url
// is not a part to load at all -- the element plays it as it is -- so it never gets here.
function playableParts(urls: readonly string[]): boolean {
  return urls.length > 0 && urls.every((url) => isInlineAudio(url) || isFetchableUrl(url));
}

// The whole answer as one blob: the parts, in order, joined (a single part is returned as it
// is). Rejects when a part cannot be loaded or the parts cannot be joined.
async function loadClip(urls: readonly string[], signal?: AbortSignal): Promise<Blob> {
  if (!playableParts(urls)) throw new Error('audio part is not playable');
  return concatWavBlobs(await Promise.all(urls.map((url) => loadPart(url, signal))));
}

// The same list of urls as an array with a stable identity: the one passed in the first time,
// until its CONTENT changes. The caller builds a new array on every render, so the effect that
// loads the clip is keyed on this instead -- an unchanged answer is not fetched again just
// because its message re-rendered. It compares the urls themselves (a pointer check for the
// same string, which an unchanged message hands back), never a serialized copy: an inline
// reply is megabytes of base64, and this runs on every render.
function useStableUrls(urls: readonly string[]): readonly string[] {
  const ref = useRef(urls);
  const held = ref.current;
  if (held !== urls && (held.length !== urls.length || held.some((url, i) => url !== urls[i]))) {
    ref.current = urls;
  }
  return ref.current;
}

// What loading the clip for a list of urls came to: the object URL the element plays and the
// blob it was made from, or neither when it failed.
type Loaded = { src: string | null; blob: Blob | null };

// Loads the clip for `urls` into an object URL and reports the outcome to `onLoaded`; returns
// the cleanup, which aborts a request still in flight and revokes the object URL. A result that
// lands after the abort is dropped, so nothing is created that the cleanup could not revoke.
function loadObjectUrl(urls: readonly string[], onLoaded: (loaded: Loaded) => void): () => void {
  const controller = new AbortController();
  let objectUrl: string | undefined;
  const load = async () => {
    try {
      const blob = await loadClip(urls, controller.signal);
      if (controller.signal.aborted) return;
      objectUrl = URL.createObjectURL(blob);
      onLoaded({ src: objectUrl, blob });
    } catch {
      if (!controller.signal.aborted) onLoaded({ src: null, blob: null });
    }
  };
  void load();
  return () => {
    controller.abort();
    if (objectUrl) URL.revokeObjectURL(objectUrl);
  };
}

function useAudioSource(passed: readonly string[]): AudioSource {
  const urls = useStableUrls(passed);
  const inline = urls.length === 1 && isInlineAudio(urls[0]);
  const stored = !inline && playableParts(urls);
  // `urls` is part of the state so a result is only ever read for the urls it was made
  // from: after a change the old object URL (already revoked by the cleanup) is
  // never shown, without a reset-in-an-effect.
  const [resolved, setResolved] = useState<(Loaded & { urls: readonly string[] }) | null>(null);

  useEffect(() => {
    if (!stored) return;
    return loadObjectUrl(urls, (loaded) => setResolved({ urls, ...loaded }));
  }, [urls, stored]);

  if (inline) return { src: urls[0], failed: false, blob: null };
  if (!stored) return { src: undefined, failed: true, blob: null };
  const current = resolved?.urls === urls ? resolved : null;
  return {
    src: current?.src ?? undefined,
    failed: current !== null && current.src === null,
    blob: current?.blob ?? null,
  };
}

// The download of a multi-part answer: ONE file, the clip the player holds. When playback has
// none (its parts are still on the way, or one failed to load) the clip is built the same way,
// so a transient failure does not make the answer unsaveable. Resolves false for every
// failure, like downloadBinary, so the caller can tell the user instead of failing silently.
async function saveWholeClip(
  name: DownloadName,
  urls: readonly string[],
  playing: Blob | null,
): Promise<boolean> {
  try {
    downloadBlob(name, playing ?? (await loadClip(urls)));
    return true;
  } catch {
    return false;
  }
}

// Renders the audio of one turn: an HTML5 `<audio controls>` player and a download button.
// It serves the speech ASSISTANT turn (the reply, variant "reply", the default) and the
// transcription USER turn (the attached or recorded clip, variant "input").
//
// `urls` are the turn's audio parts, in order. A reply is ONE part when it is short -- and
// every reply saved before long answers were split is -- and the player is then exactly the
// one-url player it has always been. A long answer is several parts, and the reply player
// stitches them into one gapless clip (see useAudioSource) with one whole-clip download. The
// input variant is always given a single part: a transcription turn with several clips
// renders one player per clip.
//
// The download handler lives here, next to the url it downloads, rather than arriving
// from ChatMessage as an onDownload prop -- an inline arrow built in the parent's render
// would give every message a new callback identity on every render and defeat
// ChatMessage's shallow-compare memo. For a single part downloadBinary fetches a stored
// reply's bytes itself (a second request, only on click; the playback copy is not reused so
// the saved file's type and name come from what the server actually served).
//
// Both failure modes are visible inline next to the player, not a toast: AudioTurn,
// like ImageTurn, must render without ToastProvider context in its standalone-props
// tests.
export function AudioTurn({
  t,
  urls,
  turnId,
  variant = 'reply',
}: Readonly<{
  t: Translation;
  urls: readonly string[];
  turnId: string;
  variant?: AudioVariant;
}>) {
  const { src, failed, blob } = useAudioSource(urls);
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
              const name = (extension: string) => downloadNameFor(variant, turnId, extension);
              const saved =
                urls.length === 1 ? downloadBinary(name, urls[0]) : saveWholeClip(name, urls, blob);
              void saved.then((ok) => setDownloadFailed(!ok)).catch(() => setDownloadFailed(true));
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
