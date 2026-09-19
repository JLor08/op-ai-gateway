// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useState } from 'react';
import { Box, IconButton, Tooltip, Typography } from '@mui/material';
import DownloadIcon from '@mui/icons-material/Download';
import { downloadBinary, extensionFor } from './shared/downloadBinary';
import type { Translation } from './shared/types';

// Renders the image_url parts of a generated-image assistant turn. Unlike the
// user-branch attachment thumbnail (a fixed 72x72 objectFit:'cover' crop --
// right for "here is what I attached", wrong for "here is the thing you
// asked me to make"), each image here renders at its own natural size,
// capped only so it never overflows the chat bubble.
//
// The download handler lives here, next to the data URL it downloads, rather
// than arriving from ChatMessage as an onDownload prop -- an inline arrow
// built in the parent's render would give every message a new callback
// identity on every render and defeat ChatMessage's shallow-compare memo.
export function ImageTurn({
  t,
  images,
  prompt,
}: Readonly<{
  t: Translation;
  images: string[];
  prompt?: string;
}>) {
  // The prompt is the accessible name when we have one; chatGeneratedImage is
  // the fallback for a transcript whose preceding user turn is gone (an old
  // transcript, or a regenerated turn whose prompt message was edited away).
  const alt = prompt || t.chatGeneratedImage;
  // Tracks which image (by index) most recently failed to download -- a
  // truncated/corrupted persisted data URL decodes to nothing. downloadBinary
  // never throws for this, but it also must not fail silently: the whole
  // point of this feature is that a failed download is visible, so an
  // inline message is shown next to that image's own button rather than a
  // global toast (which couldn't say which of several images failed, and
  // would require ImageTurn to depend on ToastProvider context that its
  // standalone-props tests, matching ChatMessage's own, don't provide).
  // Cleared on the next successful download.
  const [failedIndex, setFailedIndex] = useState<number | null>(null);

  return (
    <Box sx={{ display: 'flex', flexWrap: 'wrap', gap: 1, mb: 1 }}>
      {images.map((url, index) => (
        <Box
          key={index}
          sx={{
            display: 'inline-flex',
            flexDirection: 'column',
            gap: 0.5,
            p: 0.75,
            maxWidth: '100%',
            border: '1px solid var(--line)',
            borderRadius: '8px',
            bgcolor: 'var(--page)',
          }}
        >
          <Box
            component="img"
            src={url}
            alt={alt}
            sx={{
              display: 'block',
              maxWidth: '100%',
              width: 'auto',
              height: 'auto',
              maxHeight: 480,
              borderRadius: '6px',
            }}
          />
          <Tooltip title={t.chatDownloadImage}>
            <IconButton
              size="small"
              aria-label={t.chatDownloadImage}
              onClick={() => {
                const ok = downloadBinary(`generated-image-${index + 1}.${extensionFor(url)}`, url);
                setFailedIndex(ok ? null : index);
              }}
              sx={{ alignSelf: 'flex-end' }}
            >
              <DownloadIcon fontSize="small" />
            </IconButton>
          </Tooltip>
          {failedIndex === index && (
            <Typography role="alert" sx={{ color: 'var(--brand-accent)', fontSize: 12 }}>
              {t.chatImageDownloadError}
            </Typography>
          )}
        </Box>
      ))}
    </Box>
  );
}
