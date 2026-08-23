// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useState, type MouseEvent } from 'react';
import { BrightnessAuto, Check, DarkMode, LightMode } from '@mui/icons-material';
import { IconButton, ListItemIcon, ListItemText, Menu, MenuItem, Tooltip } from '@mui/material';
import type { ColorModePref } from './useColorMode';
import { useThemeControls } from './useThemeControls';
import type { Translation } from '../components/shared/types';

const TRIGGER_ICON = { system: BrightnessAuto, light: LightMode, dark: DarkMode } as const;

export function ColorModeMenu({ t }: Readonly<{ t: Translation }>) {
  const { pref, setMode, hasDark } = useThemeControls();
  const [anchorEl, setAnchorEl] = useState<null | HTMLElement>(null);
  const open = Boolean(anchorEl);
  if (!hasDark) return null;

  const Current = TRIGGER_ICON[pref];
  const items: { mode: ColorModePref; label: string; Icon: typeof LightMode }[] = [
    { mode: 'system', label: t.colorModeAuto, Icon: BrightnessAuto },
    { mode: 'light', label: t.colorModeLight, Icon: LightMode },
    { mode: 'dark', label: t.colorModeDark, Icon: DarkMode },
  ];

  return (
    <>
      <Tooltip title={t.colorMode}>
        <IconButton
          color="inherit"
          size="small"
          aria-label={t.colorMode}
          aria-haspopup="menu"
          aria-expanded={open ? 'true' : undefined}
          onClick={(e: MouseEvent<HTMLElement>) => setAnchorEl(e.currentTarget)}
        >
          <Current fontSize="small" />
        </IconButton>
      </Tooltip>
      <Menu anchorEl={anchorEl} open={open} onClose={() => setAnchorEl(null)}>
        {items.map(({ mode, label, Icon }) => (
          <MenuItem
            key={mode}
            selected={pref === mode}
            aria-selected={pref === mode ? 'true' : undefined}
            onClick={() => {
              setMode(mode);
              setAnchorEl(null);
            }}
          >
            <ListItemIcon>
              <Icon fontSize="small" />
            </ListItemIcon>
            <ListItemText>{label}</ListItemText>
            {pref === mode && <Check fontSize="small" aria-hidden="true" />}
          </MenuItem>
        ))}
      </Menu>
    </>
  );
}
