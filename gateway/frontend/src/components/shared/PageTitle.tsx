// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { ReactNode } from 'react';
import { Box, Typography } from '@mui/material';

/** Seitenkopf: Titel als level-1-Heading, optionale Subtitle + trailing Aktion. */
export function PageTitle({
  title,
  subtitle,
  action,
  titleId,
}: Readonly<{
  title: ReactNode;
  subtitle?: ReactNode;
  action?: ReactNode;
  titleId?: string;
}>) {
  return (
    <Box
      sx={{
        display: 'flex',
        alignItems: 'flex-start',
        justifyContent: 'space-between',
        gap: 2.5,
        mb: 3.5,
      }}
    >
      <Box>
        <Typography variant="h4" component="h1" id={titleId}>
          {title}
        </Typography>
        {subtitle && <Typography color="text.secondary">{subtitle}</Typography>}
      </Box>
      {action}
    </Box>
  );
}
