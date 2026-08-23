// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cloneElement, useState, type MouseEvent, type ReactElement } from 'react';
import { IconButton, ListItemIcon, ListItemText, Menu, MenuItem, Tooltip } from '@mui/material';
import MoreVertIcon from '@mui/icons-material/MoreVert';

/** One entry in a row's "…" actions menu. */
export type RowAction = {
  key: string;
  label: string;
  // A concrete element (never a raw string/number/Promise etc.) so a
  // `.every((a) => a.icon)` truthiness check (RowActionsCell's inline-icons
  // gate) can never be fooled by ReactNode's `Promise<...>` member.
  icon?: ReactElement;
  onClick: () => void;
  /** "error" tints the item for destructive actions (e.g. delete). */
  color?: 'error';
  disabled?: boolean;
  /**
   * Optional hover tooltip (e.g. why a disabled action is disabled). When set, the
   * menu item is wrapped in a Tooltip + span so the hint shows even for a disabled
   * item (a disabled MUI item alone would swallow pointer events).
   */
  title?: string;
};

/**
 * Kebab ("⋮") button opening a MUI Menu of row actions — the per-row actions
 * cell for ListTable. Follows the app's existing Menu idiom (UserMenu). Renders
 * nothing when there are no actions.
 */
export function RowActionsMenu({
  actions,
  label,
}: Readonly<{ actions: RowAction[]; label: string }>) {
  const [anchorEl, setAnchorEl] = useState<null | HTMLElement>(null);
  const open = Boolean(anchorEl);
  if (actions.length === 0) return null;
  return (
    <>
      <Tooltip title={label}>
        <IconButton
          size="small"
          aria-label={label}
          aria-haspopup="menu"
          aria-expanded={open ? 'true' : undefined}
          onClick={(e: MouseEvent<HTMLElement>) => setAnchorEl(e.currentTarget)}
        >
          <MoreVertIcon fontSize="small" />
        </IconButton>
      </Tooltip>
      <Menu anchorEl={anchorEl} open={open} onClose={() => setAnchorEl(null)}>
        {actions.map((action) => {
          const item = (
            <MenuItem
              disabled={action.disabled}
              onClick={() => {
                setAnchorEl(null);
                action.onClick();
              }}
              sx={action.color === 'error' ? { color: 'error.main' } : undefined}
            >
              {action.icon && <ListItemIcon sx={{ color: 'inherit' }}>{action.icon}</ListItemIcon>}
              <ListItemText>{action.label}</ListItemText>
            </MenuItem>
          );
          // A title wraps the item in a Tooltip + <span> so the hint fires even for a
          // disabled item (a disabled MUI item alone swallows pointer events). The
          // common (no-title) path keeps the MenuItem as a direct Menu child so MUI's
          // keyboard/focus handling is unchanged — just key it via cloneElement.
          return action.title ? (
            <Tooltip key={action.key} title={action.title}>
              <span>{item}</span>
            </Tooltip>
          ) : (
            cloneElement(item, { key: action.key })
          );
        })}
      </Menu>
    </>
  );
}
