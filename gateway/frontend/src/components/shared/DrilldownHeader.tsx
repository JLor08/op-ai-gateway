// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { Box, Button, Typography } from '@mui/material';
import ArrowBackIcon from '@mui/icons-material/ArrowBack';
import type { Translation } from './types';

export function DrilldownHeader({
  t,
  title,
  onBack,
}: Readonly<{
  t: Translation;
  title: string;
  onBack: () => void;
}>) {
  return (
    <Box
      sx={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 1, mb: 2 }}
    >
      <Typography sx={{ minWidth: 0, fontSize: 18, fontWeight: 700, overflowWrap: 'anywhere' }}>
        {title}
      </Typography>
      <Button
        variant="outlined"
        size="small"
        color="secondary"
        startIcon={<ArrowBackIcon />}
        onClick={onBack}
      >
        {t.back}
      </Button>
    </Box>
  );
}
