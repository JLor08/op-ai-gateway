// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { Box, Breadcrumbs as MuiBreadcrumbs, Button, Link, Typography } from '@mui/material';
import ArrowBackIcon from '@mui/icons-material/ArrowBack';

/** One breadcrumb node. Ancestor nodes carry an onClick (navigate up); the
    current (last) node is plain text. */
export type BreadcrumbItem = { label: string; onClick?: () => void };

/**
 * Breadcrumb trail for nested sub-views (create/edit masks + drill-downs),
 * replacing stacked "Back" headers: each ancestor is a clickable link back to
 * that level, the last item is the current location. A right-aligned "Back"
 * button navigates one level up — shown only when a parent level exists.
 */
export function Breadcrumbs({
  items,
  ariaLabel,
  backLabel,
}: Readonly<{
  items: BreadcrumbItem[];
  ariaLabel: string;
  backLabel: string;
}>) {
  // The parent is the second-to-last node (the last is the current level).
  const parent = items.length >= 2 ? items.at(-2) : undefined;
  return (
    <Box
      sx={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 2, mb: 3 }}
    >
      <MuiBreadcrumbs aria-label={ariaLabel}>
        {items.map((item, index) => {
          const last = index === items.length - 1;
          if (item.onClick && !last) {
            return (
              <Link
                key={index}
                component="button"
                type="button"
                underline="hover"
                color="inherit"
                onClick={item.onClick}
                sx={{ font: 'inherit', cursor: 'pointer' }}
              >
                {item.label}
              </Link>
            );
          }
          return (
            <Typography
              key={index}
              component="span"
              color={last ? 'text.primary' : 'inherit'}
              sx={{ fontWeight: last ? 600 : 400 }}
            >
              {item.label}
            </Typography>
          );
        })}
      </MuiBreadcrumbs>
      {parent?.onClick && (
        <Button
          variant="outlined"
          size="small"
          color="secondary"
          startIcon={<ArrowBackIcon />}
          onClick={parent.onClick}
          sx={{ flexShrink: 0 }}
        >
          {backLabel}
        </Button>
      )}
    </Box>
  );
}
