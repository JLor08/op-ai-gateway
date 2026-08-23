// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { ReactNode } from 'react';
import { Box } from '@mui/material';
import type { Brand as BrandType } from './tokens';
import { MatrixLogo } from './MatrixLogo';

/**
 * Rendert die Produktmarke aus dem aktiven Theme: Marke (Text, Matrix-Logo
 * oder ein operator-geliefertes Bild-Logo) + vertikaler Divider + Titel.
 * Titel/Divider erben die Textfarbe des Kontexts (dunkle Topbar via
 * `--header-text`, helle Karte via `--text`); die Text-Marke nutzt
 * `--brand-accent`.
 *
 * Das Bild-Logo (externe Themes) wird IMMER über ein `<img>` eingebunden statt
 * das SVG inline zu rendern: der Server liefert das operator-Logo unter
 * `/api/system/themes/{id}/logo` roh mit `image/svg+xml`; ein `<img>` lädt es
 * als eigenständiges Dokument (kein Skript-Kontext), inline-SVG (`dangerouslySetInnerHTML`
 * o.ä.) würde darin enthaltenes Skript im Seitenkontext ausführen.
 */
export function Brand({ brand, label }: Readonly<{ brand: BrandType; label: string }>) {
  let mark: ReactNode;
  if (brand.mark.type === 'logo') {
    mark = <MatrixLogo />;
  } else if (brand.mark.type === 'image') {
    mark = <img src={brand.mark.url} alt={label} style={{ height: 34 }} />;
  } else {
    mark = (
      <Box
        component="span"
        sx={{
          color: 'var(--brand-accent)',
          fontSize: 34,
          fontWeight: 800,
          lineHeight: 1,
          whiteSpace: 'nowrap',
        }}
      >
        {brand.mark.text}
      </Box>
    );
  }
  return (
    <Box
      aria-label={label}
      sx={{ display: 'flex', alignItems: 'center', gap: '14px', minWidth: 0 }}
    >
      {mark}
      <Box
        component="span"
        aria-hidden="true"
        sx={{ width: '1px', height: 30, bgcolor: 'currentColor', opacity: 0.25, flex: '0 0 auto' }}
      />
      <Box component="span" sx={{ fontSize: 22, fontWeight: 700, whiteSpace: 'nowrap' }}>
        {brand.title}
      </Box>
    </Box>
  );
}
