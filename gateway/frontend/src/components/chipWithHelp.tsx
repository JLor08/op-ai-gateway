// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { ReactNode } from 'react';
import { Chip, Tooltip } from '@mui/material';
import type { HelpMessageKey } from './billingUnit';
import type { Translation } from './shared/types';

// An opaque wire enum, rendered as a chip with its RAW value plus an explanatory
// tooltip. The raw label is the portal's forward-compatibility convention: an
// unrecognised value from a newer backend must show as itself rather than as a
// misleading label -- and it gets NO tooltip, because a wrong explanation is
// worse than none.
export function chipWithHelp(
  value: string,
  helpKeys: Record<string, HelpMessageKey>,
  t: Translation,
): ReactNode {
  const body = value ? <Chip size="small" variant="outlined" label={value} /> : <span>—</span>;
  const helpKey = helpKeys[value];
  // No cast on this lookup: HelpMessageKey is derived from the keys of
  // Translation, so a missing or misspelled key fails the build here instead of
  // silently rendering nothing.
  //
  // The lookup CAN be undefined at runtime even though its type says otherwise:
  // tsconfig has `strict` but not `noUncheckedIndexedAccess`, so an index into a
  // Record<string, ...> is typed as if it always hit. An unrecognised wire value
  // (a newer backend's energy_source, any billing_unit outside the two known
  // ones) yields undefined here. `billingUnit.test.ts` pins that lookup.
  //
  // The early return below is a SIMPLIFICATION, not a correctness guard: MUI's
  // Tooltip already suppresses itself on a falsy `title`, so deleting it would
  // render the same DOM -- it only avoids wrapping `body` in a Tooltip that
  // would do nothing. What actually holds the "an unknown value gets NO tooltip"
  // rule is the test: ActivityTable.test.tsx hovers an unrecognised
  // energy_source and waits past MUI's enter delay before asserting the absence,
  // so inventing a fallback tooltip here fails it.
  if (!helpKey) return body;
  return <Tooltip title={t[helpKey]}>{body}</Tooltip>;
}
