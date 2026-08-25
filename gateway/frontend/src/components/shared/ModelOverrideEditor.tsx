// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { Box, Button, Checkbox, FormControlLabel, IconButton, Typography } from '@mui/material';
import AddIcon from '@mui/icons-material/Add';
import DeleteIcon from '@mui/icons-material/Delete';
import type { ModelOverrideEntry } from '../../api';
import type { Translation } from './types';
import { Field } from './Field';
import { SearchableSelect } from './SearchableSelect';

// The DATA half of a row in the model-override editor: a requested-model
// free-text key -> a gateway model id, plus the two listing switches (see
// ModelOverrideEntry). `offer` lists `from` itself as an offered model name
// (inheriting `to`'s API flavors); `hideTarget` removes `to`'s own name from
// the offered list. Neither switch affects row completeness (see
// overrideRowsInvalid below). The catch-all (carried separately, see
// ModelOverrideEditor below) applies to any requested model with no row.
// Shared by TokenList (personal/chat tokens) and ServicesView (service tokens)
// — both build the same wire shape (buildOverrideMap) and render the same row
// editor.
//
// Everything that is serialized or validated lives here, and NOT the row's
// identity: buildOverrideMap and overrideRowsInvalid take this type so they
// stay independent of the editor's rendering concern.
export type OverrideRowValues = {
  from: string;
  to: string;
  offer: boolean;
  hideTarget: boolean;
};

// A row as the editor renders it: its values plus a stable `id`.
//
// `id` is the row's React key, and it MUST NOT be the array index. React keeps
// a row's mounted component instances per key, so with an index key removing a
// row re-points every later row at its predecessor's instances and destroys
// the last one — the surviving rows keep their (controlled) values but not
// their instances, so anything React holds ON the instance moves one row up or
// is thrown away: the caret/focus the user was in, and the uncontrolled
// internal state of the row's SearchableSelect (MUI Autocomplete's typed
// filter text). A stable id makes a removal remove exactly the one row.
export type OverrideRow = OverrideRowValues & { id: string };

// Row ids only have to be unique among the rows mounted in one editor: they
// are never persisted (not part of the wire shape — see buildOverrideMap),
// never rendered, and never sent anywhere, so a module-level counter is
// enough and keeps them stable and readable in test failures. Every row is
// created through here, so no row can ever be missing one.
let overrideRowSeq = 0;
export function newOverrideRow(values: OverrideRowValues): OverrideRow {
  overrideRowSeq += 1;
  return { ...values, id: `override-row-${overrideRowSeq}` };
}

// The "to" dropdown only ever needs id + display_name — both ModelOption
// (the full model list) and ServerModelOption (a server-override-narrowed
// list, see TokenList) satisfy this.
export type OverrideModelOption = { id: string; display_name: string };

// A row is incomplete when exactly one side is filled; such a row blocks
// submit.
export function overrideRowsInvalid(rows: readonly OverrideRowValues[]): boolean {
  return rows.some((r) => (r.from.trim() !== '') !== (r.to.trim() !== ''));
}

// buildOverrideMap serializes the row editor into the wire object, keeping
// only complete rows (both sides non-empty, trimmed); a duplicate
// requested-model key resolves to the last row. Both switches always ride
// along, defaulting to their current (possibly false) row value.
export function buildOverrideMap(
  rows: readonly OverrideRowValues[],
): Record<string, ModelOverrideEntry> {
  const map: Record<string, ModelOverrideEntry> = {};
  for (const row of rows) {
    const from = row.from.trim();
    const to = row.to.trim();
    if (from && to) map[from] = { to, offer: row.offer, hide_target: row.hideTarget };
  }
  return map;
}

// overrideSummary renders the list-column cell: each mapping as "from→to",
// plus the catch-all as "<Rest>→to" when set. Empty = "" (rendered as "-" by
// the caller).
export function overrideSummary(
  t: Translation,
  r: { model_override_map?: Record<string, ModelOverrideEntry>; model_override: string },
): string {
  const parts = Object.entries(r.model_override_map ?? {}).map(
    ([from, entry]) => `${from}→${entry.to}`,
  );
  if (r.model_override) parts.push(`${t.tokenOverrideCatchAllShort}→${r.model_override}`);
  return parts.join(', ');
}

/**
 * The shared model-override row editor, embedded both in TokenList's
 * create/edit form and ServicesView's service-token create dialog. Fully
 * controlled: no internal row/catch-all state — `onRowsChange`/
 * `onCatchAllChange` drive it, add/update/remove-row handlers live only here.
 *
 * Callers must build their rows with `newOverrideRow` so every row carries the
 * stable `id` this renders as its React key (see OverrideRow for why an index
 * key is wrong here).
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
    onRowsChange([...rows, newOverrideRow({ from: '', to: '', offer: false, hideTarget: false })]);
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
      <Typography variant="caption" color="text.secondary">
        {t.tokenOverrideOfferHint}
      </Typography>
      <Typography variant="caption" color="text.secondary">
        {t.tokenOverrideHideTargetHint}
      </Typography>
      {rows.map((row, i) => (
        // Keyed by the row's own id, never by `i` — see OverrideRow. The
        // index is still what the ids/aria-labels below are derived from:
        // those describe the row's POSITION ("… 2"), which is exactly what
        // should shift when a row above is removed.
        <Box key={row.id} sx={{ display: 'grid', gap: 0.5 }}>
          <Box sx={{ display: 'flex', gap: 1, alignItems: 'flex-start' }}>
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
          <Box sx={{ display: 'flex', gap: 2 }}>
            <FormControlLabel
              control={
                <Checkbox
                  size="small"
                  checked={row.offer}
                  onChange={(e) => updateRow(i, { offer: e.target.checked })}
                />
              }
              label={t.tokenOverrideOffer}
            />
            <FormControlLabel
              control={
                <Checkbox
                  size="small"
                  checked={row.hideTarget}
                  onChange={(e) => updateRow(i, { hideTarget: e.target.checked })}
                />
              }
              label={t.tokenOverrideHideTarget}
            />
          </Box>
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
