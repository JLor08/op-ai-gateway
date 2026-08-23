// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { ReactNode } from 'react';
import { IconButton, Tooltip } from '@mui/material';

/** Zeilen-Aktion als Icon-Button mit Tooltip; `label` ist zugleich der accessible name. */
export function IconAction({
  label,
  icon,
  onClick,
  color,
  disabled,
}: Readonly<{
  label: string;
  icon: ReactNode;
  onClick: () => void;
  color?: 'inherit' | 'error';
  disabled?: boolean;
}>) {
  return (
    <Tooltip title={label}>
      <IconButton
        size="small"
        aria-label={label}
        color={color}
        disabled={disabled}
        onClick={onClick}
      >
        {icon}
      </IconButton>
    </Tooltip>
  );
}
