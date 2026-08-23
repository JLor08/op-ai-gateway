// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useState, type MouseEvent } from 'react';
import { Button, Menu, MenuItem } from '@mui/material';
import LanguageIcon from '@mui/icons-material/Language';
import KeyboardArrowDownIcon from '@mui/icons-material/KeyboardArrowDown';
import type { Locale } from '../../i18n';
import type { Translation } from './types';

const LOCALES: Locale[] = ['de', 'en'];

export function LanguageMenu({
  locale,
  onSelect,
  t,
}: Readonly<{
  locale: Locale;
  onSelect: (l: Locale) => void;
  t: Translation;
}>) {
  const [anchorEl, setAnchorEl] = useState<null | HTMLElement>(null);
  const open = Boolean(anchorEl);
  return (
    <>
      <Button
        color="inherit"
        aria-label={t.language}
        aria-haspopup="menu"
        aria-expanded={open ? 'true' : undefined}
        onClick={(e: MouseEvent<HTMLElement>) => setAnchorEl(e.currentTarget)}
        startIcon={<LanguageIcon aria-hidden="true" />}
        endIcon={<KeyboardArrowDownIcon aria-hidden="true" />}
        sx={{ fontWeight: 700 }}
      >
        {locale.toUpperCase()}
      </Button>
      <Menu anchorEl={anchorEl} open={open} onClose={() => setAnchorEl(null)}>
        {LOCALES.map((l) => (
          <MenuItem
            key={l}
            selected={l === locale}
            aria-selected={l === locale ? 'true' : undefined}
            onClick={() => {
              onSelect(l);
              setAnchorEl(null);
            }}
          >
            {l.toUpperCase()}
          </MenuItem>
        ))}
      </Menu>
    </>
  );
}
