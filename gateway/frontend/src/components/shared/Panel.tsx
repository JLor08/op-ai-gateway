// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { ReactNode } from 'react';
import { Box, Paper, Typography } from '@mui/material';

/** Sektions-Wrapper: <section> (Paper component="section") mit level-2-Heading als Label. */
export function Panel({
  titleId,
  title,
  subtitle,
  actions,
  children,
}: Readonly<{
  titleId: string;
  title: ReactNode;
  subtitle?: ReactNode;
  actions?: ReactNode;
  children: ReactNode;
}>) {
  return (
    <Paper component="section" variant="outlined" aria-labelledby={titleId} sx={{ p: 3 }}>
      <Box
        sx={{
          display: 'flex',
          alignItems: 'flex-start',
          justifyContent: 'space-between',
          gap: 2.5,
          mb: 2.25,
        }}
      >
        <Box>
          <Typography component="h2" id={titleId} variant="h6">
            {title}
          </Typography>
          {subtitle && <Typography color="text.secondary">{subtitle}</Typography>}
        </Box>
        {actions}
      </Box>
      {children}
    </Paper>
  );
}
