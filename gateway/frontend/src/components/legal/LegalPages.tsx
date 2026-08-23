// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { Alert, Box, Typography } from '@mui/material';
import type { Locale } from '../../i18n';
import { legalContent, type LegalPage } from './content';

interface LegalPagesProps {
  page: LegalPage;
  locale: Locale;
}

// Renders one operator legal template (Impressum / Nutzungsbedingungen /
// Datenschutz) for the active locale. Content is a TEMPLATE with placeholders and
// a prominent warning banner — see content.ts.
export function LegalPages({ page, locale }: Readonly<LegalPagesProps>) {
  const doc = legalContent[locale][page];
  return (
    <Box sx={{ maxWidth: 820 }}>
      <Typography variant="h4" component="h1" sx={{ fontWeight: 700, mb: 1.5 }}>
        {doc.title}
      </Typography>
      <Alert severity="warning" role="note" sx={{ mb: 3 }}>
        {doc.banner}
      </Alert>
      {doc.sections.map((section) => (
        <Box key={section.heading} component="section" sx={{ mb: 2.5 }}>
          <Typography variant="h6" component="h2" sx={{ fontWeight: 600, mb: 0.75 }}>
            {section.heading}
          </Typography>
          {section.body.map((line, i) => (
            <Typography key={i} variant="body2" sx={{ mb: 0.5, color: 'var(--muted)' }}>
              {line}
            </Typography>
          ))}
        </Box>
      ))}
    </Box>
  );
}
