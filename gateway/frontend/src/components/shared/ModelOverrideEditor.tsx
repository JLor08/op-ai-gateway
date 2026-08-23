// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { Box, Button, IconButton, Typography } from '@mui/material';
import AddIcon from '@mui/icons-material/Add';
import DeleteIcon from '@mui/icons-material/Delete';
import type { Translation } from './types';
import { Field } from './Field';
import { SearchableSelect } from './SearchableSelect';

// A row in the model-override editor: a requested-model free-text key -> a
// gateway model id. The catch-all (carried separately, see ModelOverrideEditor
// below) applies to any requested model with no row. Shared by TokenList
// (personal/chat tokens) and ServicesView (service tokens) — both build the
// same wire shape (buildOverrideMap) and render the same row editor.
export type OverrideRow = { from: string; to: string };

// The "to" dropdown only ever needs id + display_name — both ModelOption
// (the full model list) and ServerModelOption (a server-override-narrowed
// list, see TokenList) satisfy this.
export type OverrideModelOption = { id: string; display_name: string };

// A row is incomplete when exactly one side is filled; such a row blocks
// submit.
export function overrideRowsInvalid(rows: OverrideRow[]): boolean {
  return rows.some((r) => (r.from.trim() !== '') !== (r.to.trim() !== ''));
}

// buildOverrideMap serializes the row editor into the wire object, keeping
// only complete rows (both sides non-empty, trimmed); a duplicate
// requested-model key resolves to the last row.
export function buildOverrideMap(rows: OverrideRow[]): Record<string, string> {
  const map: Record<string, string> = {};
  for (const row of rows) {
    const from = row.from.trim();
    const to = row.to.trim();
    if (from && to) map[from] = to;
  }
  return map;
}

// overrideSummary renders the list-column cell: each mapping as "from→to",
// plus the catch-all as "<Rest>→to" when set. Empty = "" (rendered as "-" by
// the caller).
export function overrideSummary(
  t: Translation,
  r: { model_override_map?: Record<string, string>; model_override: string },
): string {
  const parts = Object.entries(r.model_override_map ?? {}).map(([from, to]) => `${from}→${to}`);
  if (r.model_override) parts.push(`${t.tokenOverrideCatchAllShort}→${r.model_override}`);
  return parts.join(', ');
}

/**
 * The shared model-override row editor, embedded both in TokenList's
 * create/edit form and ServicesView's service-token create dialog. Fully
 * controlled: no internal row/catch-all state — `onRowsChange`/
 * `onCatchAllChange` drive it, add/update/remove-row handlers live only here.
 *
 * `idPrefix` derives each row's Field/SearchableSelect ids as
 * `${idPrefix}-override-from-${i}` / `${idPrefix}-override-to-${i}`, matching
 * each caller's own historical prefix ("token" / "service-token") so their
 * DOM ids don't shift. `catchAllId` is the catch-all SearchableSelect's id;
 * it does NOT follow the row prefix in either caller (TokenList's is
 * "token-model-catchall", not "token-catchall") so it is threaded separately
 * rather than derived from `idPrefix`.
 */
export function ModelOverrideEditor({
  rows,
  onRowsChange,
  catchAll,
  onCatchAllChange,
  models,
  t,
  idPrefix,
  catchAllId,
}: Readonly<{
  rows: OverrideRow[];
  onRowsChange: (rows: OverrideRow[]) => void;
  catchAll: string;
  onCatchAllChange: (value: string) => void;
  models: OverrideModelOption[];
  t: Translation;
  idPrefix: string;
  catchAllId: string;
}>) {
  function updateRow(index: number, patch: Partial<OverrideRow>) {
    onRowsChange(rows.map((r, i) => (i === index ? { ...r, ...patch } : r)));
  }
  function addRow() {
    onRowsChange([...rows, { from: '', to: '' }]);
  }
  function removeRow(index: number) {
    onRowsChange(rows.filter((_, i) => i !== index));
  }

  return (
    <Box sx={{ display: 'grid', gap: 1 }}>
      <Typography variant="subtitle2">{t.tokenOverrideMapTitle}</Typography>
      <Typography variant="caption" color="text.secondary">
        {t.tokenOverrideMapNote}
      </Typography>
      {rows.map((row, i) => (
        <Box key={i} sx={{ display: 'flex', gap: 1, alignItems: 'flex-start' }}>
          <Box sx={{ flex: 1 }}>
            <Field
              id={`${idPrefix}-override-from-${i}`}
              value={row.from}
              onChange={(e) => updateRow(i, { from: e.target.value })}
              placeholder={t.tokenOverrideFromPlaceholder}
              inputProps={{ 'aria-label': `${t.tokenOverrideFromLabel} ${i + 1}` }}
            />
          </Box>
          <Box sx={{ pt: 1 }} aria-hidden>
            →
          </Box>
          <Box sx={{ flex: 1 }}>
            <SearchableSelect
              id={`${idPrefix}-override-to-${i}`}
              label={`${t.tokenOverrideToLabel} ${i + 1}`}
              value={row.to}
              onChange={(v) => updateRow(i, { to: v })}
              options={[
                { value: '', label: '-' },
                ...models.map((m) => ({ value: m.id, label: m.display_name })),
              ]}
            />
          </Box>
          <IconButton
            aria-label={`${t.tokenOverrideRemoveRow} ${i + 1}`}
            onClick={() => removeRow(i)}
          >
            <DeleteIcon fontSize="small" />
          </IconButton>
        </Box>
      ))}
      <Box>
        <Button type="button" size="small" startIcon={<AddIcon />} onClick={addRow}>
          {t.tokenOverrideAddRow}
        </Button>
      </Box>
      <SearchableSelect
        id={catchAllId}
        label={t.tokenOverrideCatchAllLabel}
        value={catchAll}
        onChange={onCatchAllChange}
        helperText={t.tokenOverrideCatchAllNote}
        options={[
          { value: '', label: '-' },
          ...models.map((m) => ({ value: m.id, label: m.display_name })),
        ]}
      />
    </Box>
  );
}
