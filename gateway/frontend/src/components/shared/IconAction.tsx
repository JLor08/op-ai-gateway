// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { ReactNode } from 'react';
import { IconButton, Tooltip } from '@mui/material';

/**
 * Zeilen-Aktion als Icon-Button mit Tooltip; `label` ist zugleich der accessible name.
 *
 * `title` is the optional "why is this disabled" hint (RowAction.title, which
 * RowActionsCell forwards here on the inline path). It takes precedence over
 * `label` in the tooltip because the reason is strictly more informative than
 * the name, which the icon and `aria-label` already carry.
 */
export function IconAction({
  label,
  icon,
  onClick,
  color,
  disabled,
  title,
}: Readonly<{
  label: string;
  icon: ReactNode;
  onClick: () => void;
  color?: 'inherit' | 'error';
  disabled?: boolean;
  title?: string;
}>) {
  const button = (
    <IconButton size="small" aria-label={label} color={color} disabled={disabled} onClick={onClick}>
      {icon}
    </IconButton>
  );
  // A disabled MUI IconButton sets `pointer-events: none`, so a Tooltip
  // anchored straight to it never fires -- precisely the case where the hint
  // matters most, since `title` exists to explain a disabled action. A <span>
  // gives the Tooltip a live anchor. Only the disabled path is wrapped: an
  // enabled button is its own anchor, and leaving that path's DOM untouched
  // keeps every existing call site byte-identical.
  return (
    <Tooltip title={title ?? label}>
      {disabled ? <span style={{ display: 'inline-flex' }}>{button}</span> : button}
    </Tooltip>
  );
}
