// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { Box } from '@mui/material';
import { IconAction } from './IconAction';
import { RowActionsMenu, type RowAction } from './RowActionsMenu';

/**
 * Renders a row's actions. When they fit (count <= maxInline and every action
 * has an icon), the icons go straight into the Actions column — one click, no
 * menu. Otherwise they collapse into a "…" kebab menu.
 */
export function RowActionsCell({
  actions,
  menuLabel,
  maxInline = 4,
}: Readonly<{
  actions: RowAction[];
  menuLabel: string;
  maxInline?: number;
}>) {
  if (actions.length === 0) return null;
  const inline = actions.length <= maxInline && actions.every((a) => a.icon);
  if (inline) {
    // nowrap so the icons stay on one line (the column widens to fit) rather
    // than wrapping — that one-line row is the point of going menu-less.
    return (
      <Box sx={{ display: 'flex', flexWrap: 'nowrap', gap: 0.25 }}>
        {actions.map((action) => (
          <IconAction
            key={action.key}
            label={action.label}
            icon={action.icon}
            onClick={action.onClick}
            color={action.color}
            disabled={action.disabled}
          />
        ))}
      </Box>
    );
  }
  return <RowActionsMenu actions={actions} label={menuLabel} />;
}
