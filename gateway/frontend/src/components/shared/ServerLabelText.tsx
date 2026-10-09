// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { Translation } from './types';
import { serverLabel, type ServerLabelRow } from './vendorServerLabel';

/**
 * The Server-cell content of an Activity row (see serverLabel). `fallback` is
 * shown when the row has no server attribution at all; omit it for an empty cell.
 */
export function ServerLabelText({
  row,
  t,
  fallback = '',
}: Readonly<{
  row: ServerLabelRow;
  t: Translation;
  fallback?: string;
}>) {
  const label = serverLabel(t, row);
  if (label.text === '') return <>{fallback}</>;
  return label.title ? <span title={label.title}>{label.text}</span> : <>{label.text}</>;
}
