// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { ReactNode } from 'react';
import { useEffect, useRef, useState } from 'react';
import { Box, IconButton, Paper, Typography } from '@mui/material';
import CheckIcon from '@mui/icons-material/Check';
import ContentCopyIcon from '@mui/icons-material/ContentCopy';

/** How long the "copied" confirmation stays visible before reverting. */
const COPIED_RESET_MS = 1500;

/**
 * Migriert OneTimeSecret. Ein <strong>-Titel (Typography component="strong") + das
 * Secret als `children`-Text-Node, sodass getByText(title).tagName === 'STRONG' und
 * getByText(secret) funktionieren. `data-testid="secret-reveal"` ist der semantische
 * Anker (ersetzt die frühere `.one-time-secret`-Klasse; von e2e genutzt).
 * Der optionale Copy-Button (copyValue gesetzt) ist rein additiv. Ein Klick zeigt
 * ~1.5s eine transiente Bestätigung (Check-Icon + optional `copiedLabel`-Text) — die
 * Komponente kennt `t` nicht, daher ist `copiedLabel` ein optionaler Prop mit
 * Default statt jeden Aufrufer anzufassen; ohne Label bleibt das Icon-Swap allein
 * die Bestätigung.
 */
export function SecretReveal({
  title,
  children,
  copyValue,
  copyLabel,
  copiedLabel,
}: Readonly<{
  title: string;
  children: ReactNode;
  copyValue?: string;
  copyLabel?: string;
  copiedLabel?: string;
}>) {
  const [copied, setCopied] = useState(false);
  const timeoutRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  useEffect(() => {
    return () => {
      if (timeoutRef.current) clearTimeout(timeoutRef.current);
    };
  }, []);

  const handleCopy = () => {
    if (!copyValue) return;
    void navigator.clipboard?.writeText(copyValue);
    setCopied(true);
    if (timeoutRef.current) clearTimeout(timeoutRef.current);
    timeoutRef.current = setTimeout(() => setCopied(false), COPIED_RESET_MS);
  };

  return (
    <Paper variant="outlined" data-testid="secret-reveal" sx={{ p: 2, display: 'grid', gap: 1 }}>
      <Typography component="strong" sx={{ fontWeight: 700 }}>
        {title}
      </Typography>
      <Box sx={{ overflowWrap: 'anywhere', wordBreak: 'break-all' }}>{children}</Box>
      {copyValue && (
        <Box sx={{ display: 'flex', alignItems: 'center', gap: 1 }}>
          <IconButton
            size="small"
            aria-label={copied && copiedLabel ? copiedLabel : copyLabel}
            onClick={handleCopy}
          >
            {copied ? (
              <CheckIcon fontSize="small" color="success" />
            ) : (
              <ContentCopyIcon fontSize="small" />
            )}
          </IconButton>
          {copied && copiedLabel && (
            <Typography variant="caption" color="success.main">
              {copiedLabel}
            </Typography>
          )}
        </Box>
      )}
    </Paper>
  );
}
