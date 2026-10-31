// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useEffect, useRef, useState, type RefObject } from 'react';
import { Box, Button, Typography } from '@mui/material';
import PlayArrowIcon from '@mui/icons-material/PlayArrow';
import type { Translation } from './shared/types';

// What the control shows: the bare play button, the same button while the audio is being made,
// the player over the result, or the failure line. `src` is only ever the object URL this
// control made and still owns.
type View = { kind: 'idle' | 'loading' | 'failed' } | { kind: 'ready'; src: string };

// Revokes the object URL the control holds, if any. An object URL pins its whole clip in memory
// until it is revoked, so it is let go of whenever the control stops showing it.
function release(held: RefObject<string | undefined>): void {
  if (held.current) URL.revokeObjectURL(held.current);
  held.current = undefined;
}

// The play control of a spoken section whose stored audio is gone (pruned, deleted): the audio
// is made again on demand instead of regenerating the whole answer. It is decoupled from the
// gateway client on purpose -- `onResynth` is the caller's request and hands back the audio as a
// Blob -- so this owns only what is the player's: the wait, the object URL the element plays,
// and the failure line.
//
// The object-URL lifecycle is AudioTurn's (loadObjectUrl), adapted to a source that is a promise
// rather than urls to fetch. The URL is revoked when the control unmounts and before the next
// synthesis replaces it. A result (or a rejection) that lands after the unmount is dropped, so
// nothing is created that nothing would ever revoke.
//
// A failed synthesis -- a rejection, or an empty result, which would be a player with nothing to
// play -- is a visible line next to the button, not a toast: like AudioTurn this must render
// without ToastProvider context. The section's own text, which the parent renders, stays as the
// fallback. `disabled` is the parent's gate (no run may start while the model is unavailable).
export function AudioResynthControl({
  t,
  onResynth,
  disabled = false,
}: Readonly<{
  t: Translation;
  onResynth: () => Promise<Blob>;
  disabled?: boolean;
}>) {
  const [view, setView] = useState<View>({ kind: 'idle' });
  const objectUrl = useRef<string | undefined>(undefined);
  // False once the control has unmounted. Set again on mount so a strict-mode remount (which
  // runs the cleanup once) does not leave a live control looking unmounted.
  const mounted = useRef(true);

  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
      release(objectUrl);
    };
  }, []);

  const resynth = async () => {
    release(objectUrl);
    setView({ kind: 'loading' });
    try {
      const blob = await onResynth();
      if (!mounted.current) return;
      if (blob.size === 0) {
        setView({ kind: 'failed' });
        return;
      }
      objectUrl.current = URL.createObjectURL(blob);
      setView({ kind: 'ready', src: objectUrl.current });
    } catch {
      if (mounted.current) setView({ kind: 'failed' });
    }
  };

  return (
    <Box sx={{ display: 'inline-flex', flexDirection: 'column', gap: 0.5, maxWidth: '100%' }}>
      <Box sx={{ display: 'flex', alignItems: 'center', gap: 0.5, minWidth: 0 }}>
        <Button
          size="small"
          startIcon={<PlayArrowIcon fontSize="small" />}
          loading={view.kind === 'loading'}
          loadingPosition="start"
          disabled={disabled}
          onClick={() => void resynth()}
        >
          {t.chatVoiceResynth}
        </Button>
        {view.kind === 'ready' && (
          <Box
            component="audio"
            controls
            autoPlay
            src={view.src}
            aria-label={t.chatGeneratedAudio}
            sx={{ display: 'block', width: 320, maxWidth: '100%', minWidth: 0 }}
          />
        )}
      </Box>
      {view.kind === 'failed' && (
        <Typography role="alert" color="error" sx={{ fontSize: 12 }}>
          {t.chatVoiceResynthError}
        </Typography>
      )}
    </Box>
  );
}
