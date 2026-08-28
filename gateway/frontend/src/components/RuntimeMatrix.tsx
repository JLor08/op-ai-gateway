// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import {
  Box,
  Checkbox,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableRow,
  Tooltip,
  Typography,
} from '@mui/material';
import CheckBoxIcon from '@mui/icons-material/CheckBox';
import CheckBoxOutlineBlankIcon from '@mui/icons-material/CheckBoxOutlineBlank';
import CheckCircleIcon from '@mui/icons-material/CheckCircle';
import BlockIcon from '@mui/icons-material/Block';
import type { Translation } from './shared/types';

export type RuntimeMatrixSpec = {
  id: string;
  model: string;
  gpus: { index: number; vramMb: number }[];
};

// ONE inline-axis budget for both axes of the grid: `maxWidth` on the
// horizontal row labels, `maxHeight` on the vertical-rl column headers (in a
// vertical writing mode the HEIGHT is the inline size). A name that exceeds it
// WRAPS -- it never loses a character.
//
// That wrapping is load-bearing, not a preference. The specs are sorted by
// name (see compareSpecs), so builds of one model that share a 30+ character
// prefix are GUARANTEED to be neighbours, and any character-dropping rule at
// this width then renders two different rows as the same string. Measured in
// Chromium at 400 14px Roboto/Helvetica/Arial: an end-ellipsis at 160px turns
// both `Qwen3-Coder-30B-A3B-Instruct-UD-Q4_K_XL` and
// `Qwen3-Coder-30B-A3B-Instruct-UD-Q8_0` into `Qwen3-Coder-30B-A3…`, in a grid
// whose only question is "may THESE TWO run together". Wrapped, they differ on
// line 2 at exactly the token that distinguishes them.
//
// 160 is the number the rotated header already spent on its own scarce axis,
// so the grid teaches one number and the corner stays square.
const LABEL_MAX_PX = 160;

// The store is a dumb pair table; canonicalisation (which id sorts first) is
// entirely the portal's job -- see the task-21 brief. Comparing BOTH orders
// here means a pair that ever reaches this component non-canonical (e.g. a
// future caller that forgot to sort) still renders as allowed, rather than
// silently disagreeing with what the backend actually holds.
function pairAllowed(pairs: [string, string][], x: string, y: string): boolean {
  return pairs.some(([p, q]) => (p === x && q === y) || (p === y && q === x));
}

// DISPLAY order only, and deliberately NOT canonicalPair's ordering below:
// that one compares raw id code units because it is the WIRE format the
// backend stores and compares pairs by (SetCoResidency in
// service_runtime.go). Sorting ids with a collator, or emitting pairs in
// display order, would desynchronise the UI from what is stored.
//
// LOCALE is left at the runtime default, matching the house idiom
// (shared/ListTable.tsx uses a bare `localeCompare`). It does NOT follow the
// portal's de/en toggle and does not need to: measured, de and en produce an
// identical order for these names and even for umlauts.
//
// `numeric: true` is a deliberate deviation from the bare house form, and the
// one thing here a reviewer should challenge. Model names are size and
// quantisation ladders, and bare collation orders them "Llama-3.1-405B,
// Llama-3.1-70B, Llama-3.1-8B" -- an operator reads that as broken. Measured,
// it costs nothing: a quantisation family sorts identically under bare and
// numeric collation (siblings stay adjacent either way), and numeric
// additionally fixes the context ladder (…-32k before …-262k, not after).
// Reviewer trap: the `numeric: true` hits elsewhere in this frontend are
// ListColumn's right-align flag, not a collator option -- not precedent in
// either direction.
//
// Constructed once at module scope: this component re-renders on every
// telemetry poll.
const modelCollator = new Intl.Collator(undefined, { numeric: true });

function compareSpecs(a: RuntimeMatrixSpec, b: RuntimeMatrixSpec): number {
  const byModel = modelCollator.compare(a.model, b.model);
  if (byModel !== 0) return byModel;
  // The id tie-break is required, not decoration. Two specs may legally carry
  // the SAME model string in file mode: the agent's ParseConfig rejects
  // duplicate spec IDs and says nothing about Model, and the portal falls back
  // to the id only when `model` is EMPTY. `id` is the only field unique in
  // BOTH paths. Without it those rows fall back to caller order -- live, the
  // store's `order by id` over random hex -- so a re-fetch could swap which of
  // the two is the row, moving the checkbox under the operator's cursor. Raw
  // `<` rather than a collator: ids are opaque hex, and collating them would
  // be meaningless.
  return a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
}

function canonicalPair(x: string, y: string): [string, string] {
  return x < y ? [x, y] : [y, x];
}

/**
 * A column header, rotated to read bottom-to-top.
 *
 * The lower triangle means the column count grows with the spec count, so a
 * server with a dozen models would need more horizontal room than the viewport
 * has for horizontal headers -- and scrolling past headers you cannot see
 * while hunting for a cell is exactly how the matrix becomes unusable.
 *
 * `writing-mode: vertical-rl` plus `rotate(180deg)` is chosen over a bare
 * `rotate(-90deg)`: the writing-mode form gives the element a genuinely
 * VERTICAL layout box (width one line-height, height the text length), so
 * ordinary table layout reserves the right footprint and the header stays
 * glued to its column. A bare transform leaves the box horizontal, which is
 * what forces the explicit heights and absolute positioning that then drift a
 * few pixels per column. Bottom-to-top (rather than top-to-bottom) is the
 * Western data-table/chart-axis convention, and it also puts the START of the
 * name next to the cells it labels.
 *
 * The header WRAPS at LABEL_MAX_PX rather than eliding, and that is a
 * correction of what shipped here first. `writing-mode: vertical-rl` makes the
 * INLINE axis vertical, so `text-overflow: ellipsis` clipped at the inline end
 * = the END of the string; `rotate(180deg)` is a paint-time transform that
 * selects no different characters, it only moves the ellipsis GLYPH to the top,
 * away from the grid. So the old comment here ("truncates away from the grid")
 * was true about the glyph and misleading about the characters: the tail --
 * the quantisation/context suffix that is the ONLY difference between two
 * builds of one model -- was exactly what got dropped. Measured, two real
 * sibling names both rendered as `Qwen3-Coder-30B-A3…`. Now that the specs are
 * name-sorted those siblings are adjacent columns, so the ellipsis had to go.
 * Wrapping spends the abundant axis (a second 16px line beside the first)
 * instead of characters; reading order survives, because after the 180° turn
 * line 1 is the LEFT vertical line and line 2 the right, i.e. ordinary reading
 * order once your head is tilted the way the rotation already assumes.
 * `overflow: hidden` is gone rather than kept "as a backstop": with wrapping
 * the inline size cannot exceed the cap, so it could only ever hide something
 * silently -- the exact failure mode being removed.
 *
 * The rotation is pure CSS on intact text content: the header still contains
 * the model name as text, so screen readers, `getByText` and the cells'
 * `aria-label`s are all unaffected. Images or per-character markup would break
 * every one of those.
 */
function ColumnHeader({ model }: Readonly<{ model: string }>) {
  return (
    <TableCell
      scope="col"
      align="center"
      // Bottom-aligned so short and long names share the edge nearest the
      // grid instead of floating at different heights above it.
      sx={{ verticalAlign: 'bottom', p: 0.5 }}
    >
      {/* The header's tooltip is the model name and nothing else -- kept
          deliberately distinct from the cell tooltip below, which explains
          what a co-residency pair does. Conflating them would bury the
          consequence sentence under a list of column labels. */}
      <Tooltip title={model}>
        <Box
          component="span"
          sx={{
            display: 'block',
            writingMode: 'vertical-rl',
            transform: 'rotate(180deg)',
            // In a vertical writing mode the INLINE axis is vertical, so
            // `max-height` is the inline-size cap -- the same budget the
            // horizontal row labels spend as `max-width`. Long names meet it
            // by wrapping onto a second 16px line, never by losing characters.
            whiteSpace: 'normal',
            overflowWrap: 'anywhere',
            maxHeight: `${LABEL_MAX_PX}px`,
            mx: 'auto',
          }}
        >
          {model}
        </Box>
      </Tooltip>
    </TableCell>
  );
}

// The advisory tooltip: the summed estimated VRAM per GPU the two specs BOTH
// touch, against that GPU's configured budget -- "GPU 0: 44000 / 46000 MB",
// flagged when over. Disjoint GPUs never compete for VRAM, so there is
// nothing to sum for them; say that plainly instead of an empty table. This
// is guidance only -- the agent's own per-GPU arithmetic at admission time is
// the real veto, so the note at the bottom says so explicitly (a
// matrix-allowed pair can still be refused at runtime, and vice versa).
//
// ORDER IS THE POINT. The operator's question is "may these two run
// together?", and until this was written the tooltip answered a different one
// first: it led with "no shared GPU -- these two never compete for VRAM",
// which sitting next to the old crossed-circle off-state read as the REASON
// the pair was forbidden. A real operator concluded the cell could not be
// ticked and stopped. So the lead line is now what the cell controls and what
// leaving it off costs -- including that the matrix is checked over the whole
// running set REGARDLESS of GPU overlap, the rule that surprised them -- and
// the VRAM arithmetic follows as the secondary detail it always was.
//
// A budget of 0 is "no budget for this GPU" = unconstrained, identical to a
// GPU with no budget row at all (`routing.ServerGPUBudget.BudgetMB` in the
// backend defines it, `runtime.Admit` in the agent honours it). Since this
// tooltip's whole job is to predict what the agent will do, it must render a
// 0 the same way it renders an absent row -- a bare sum, never a "/ 0 MB"
// over-budget warning for a limit the agent does not enforce. 0 is reachable
// operator input: a fresh budget row starts at 0 MB here in the portal, and
// clearing the MB field yields 0 too.
function MatrixTooltip({
  t,
  a,
  b,
  budgets,
  disabledReason,
}: Readonly<{
  t: Translation;
  a: RuntimeMatrixSpec;
  b: RuntimeMatrixSpec;
  budgets: Record<number, number>;
  disabledReason?: string;
}>) {
  const bByIndex = new Map(b.gpus.map((g) => [g.index, g.vramMb]));
  const shared = a.gpus.filter((g) => bByIndex.has(g.index));
  return (
    <Box sx={{ display: 'grid', gap: 0.25 }}>
      {/* When the cell cannot be clicked at all, "why not" outranks even the
          consequence: an operator who does not know the matrix is read-only
          here will keep clicking a control that will never respond. */}
      {disabledReason !== undefined && (
        <Typography variant="caption" sx={{ fontWeight: 'medium' }}>
          {disabledReason}
        </Typography>
      )}
      <Typography variant="caption">{t.runtimeMatrixConsequence}</Typography>
      {shared.length === 0 ? (
        <Typography variant="caption">{t.runtimeMatrixNoSharedGpu}</Typography>
      ) : (
        shared.map((g) => {
          const sum = g.vramMb + (bByIndex.get(g.index) ?? 0);
          const raw = budgets[g.index];
          const budget = raw !== undefined && raw > 0 ? raw : undefined;
          const over = budget !== undefined && sum > budget;
          const base =
            budget !== undefined
              ? `GPU ${g.index}: ${sum} / ${budget} MB`
              : `GPU ${g.index}: ${sum} MB`;
          return (
            <Typography
              key={g.index}
              variant="caption"
              sx={over ? { color: 'error.main' } : undefined}
            >
              {over ? `${base} (${t.runtimeMatrixOverBudget})` : base}
            </Typography>
          );
        })
      )}
      <Typography variant="caption" color="text.secondary" sx={{ fontStyle: 'italic' }}>
        {t.runtimeMatrixAdvisory}
      </Typography>
    </Box>
  );
}

/**
 * The co-residency triangle: rows 2..n against columns 1..n-1, strictly below
 * the diagonal only -- co-residency is symmetric ("A with B" === "B with A")
 * and irreflexive (a spec is never paired with itself), so a full n×n grid
 * would draw every fact twice plus a meaningless diagonal. Renders inside its
 * own horizontally-scrolling container so a wide matrix never widens the page
 * (house pattern). Every cell carries a full accessible name
 * (`${t.runtimeMatrixCell}: ${row.model} + ${col.model}`) so tests and
 * screen readers can address it by role instead of DOM position.
 *
 * THE CELL IS A CHECKBOX, and that is a fix rather than a detail. The cell has
 * always been a boolean an operator sets, but it used to render as an
 * `IconButton` whose off state was a crossed-circle `BlockIcon` -- the
 * vocabulary of "forbidden by the system", not of "off, click to turn on". An
 * operator who wanted two models co-resident on DIFFERENT GPUs read the
 * prohibition symbol plus a tooltip about disjoint GPUs as a refusal and
 * stopped, even though nothing in the stack prevented them: `SetCoResidency`
 * rejects only empty, identical, foreign and duplicate pairs, and says nothing
 * about GPUs. The one configuration the matrix exists to express was the one
 * the UI discouraged hardest. An empty checkbox is the conventional, scannable
 * "you may tick this"; `role="checkbox"` + `aria-checked` is also the honest
 * semantics for a boolean in a grid, where the old `aria-pressed` only hinted
 * at it.
 *
 * FOUR renderings, not two, because "off" and "not editable" are different
 * facts and used to wear the same symbol, separated only by MUI's subtle
 * disabled styling:
 *
 *   |         | editable                | disabled                  |
 *   | on      | ticked box (success)    | filled circle-check       |
 *   | off     | empty box               | crossed circle (Block)    |
 *
 * The SQUARE family means "a control you can operate", the CIRCLE family means
 * "a read-out you cannot". Nothing was thrown away: `CheckCircleIcon` and
 * `BlockIcon` simply moved to the read-only state, which is the one they
 * actually describe -- a crossed circle finally means prohibition. And the
 * allowed/off fact stays legible in both columns, which a single "disabled
 * glyph" would have destroyed (a disabled ALLOWED pair must not read as
 * forbidden).
 *
 * `disabledReason` is why, and a disabled control must say why -- same rule
 * `RowActionsCell`/`IconAction` were fixed for earlier on this branch. It
 * leads the tooltip. The `<span>` wrapper is what makes that reachable at all:
 * a disabled MUI control sets `pointer-events: none`, so a Tooltip anchored
 * straight to it never fires.
 *
 * `onToggle` always fires with the pair sorted (a < b), regardless of which
 * of the two specs is the visual row vs. column -- the backend stores/reports
 * pairs canonically and expects the same on write (see SetCoResidency in
 * service_runtime.go); getting this wrong makes the UI and the backend
 * silently disagree about which cell is set.
 */
export function RuntimeMatrix({
  t,
  specs,
  pairs,
  onToggle,
  budgets,
  disabled = false,
  disabledReason,
}: Readonly<{
  t: Translation;
  // `readonly` because this component SORTS: the copy in `ordered` below is
  // what keeps the caller's array intact, and the type makes a future
  // `specs.sort(...)` a compile error rather than a latent mutation bug. Both
  // call sites hand in a fresh `.map()` result, which is assignable unchanged.
  specs: readonly RuntimeMatrixSpec[];
  pairs: [string, string][];
  onToggle: (a: string, b: string) => void;
  budgets: Record<number, number>;
  disabled?: boolean;
  disabledReason?: string;
}>) {
  if (specs.length < 2) {
    return <Typography color="text.secondary">{t.runtimeMatrixNeedTwo}</Typography>;
  }

  // ONE ordering for the whole grid, applied once, to the source of BOTH
  // slices. Sorting either slice on its own is the single way to break "cell k
  // sits under header k". The triangle itself is permutation-invariant: rows
  // are ordered[1..n-1] and row i draws ordered[0..i-1], so every unordered
  // pair still appears exactly once for ANY order, and nothing stored or sent
  // depends on display order because onToggle always emits
  // canonicalPair(rowSpec.id, colSpec.id).
  //
  // Sorted HERE and not at the callers: both source arrays are shared with
  // user-sortable ListTables on the same screen (RuntimeAdminSection passes
  // the same `mappings` / `reportConfig.specs` to a table and to this matrix),
  // and sorting upstream would silently re-order tables whose sort is the
  // operator's own, persisted choice. The matrix is always name-ordered; the
  // tables keep whatever the operator picked.
  //
  // Copy, never `specs.sort(...)`: that mutates a caller's array. Both call
  // sites happen to hand in a fresh `.map()` result today, so the mutation bug
  // would be latent rather than visible -- exactly how it survives review.
  const ordered = [...specs].sort(compareSpecs);
  const columnSpecs = ordered.slice(0, -1);
  const rowSpecs = ordered.slice(1);
  // Only meaningful while `disabled`; folded here so the tooltip and the
  // glyph choice below cannot disagree about which state they are rendering.
  const reason = disabled ? disabledReason : undefined;

  return (
    <Box sx={{ overflowX: 'auto' }}>
      {/* `width: auto` overrides MUI's `width: 100%`, and it is what makes the
          row-label cap above VISIBLE rather than merely declared. Under
          `table-layout: auto` a 100%-wide table hands its surplus width back
          to the columns in proportion to their content, so the capped 160px
          label still sat in a 491px cell (measured, 5 specs at a 1100px
          viewport) with ~330px of empty gutter between the name and its own
          first checkbox. Shrink-to-fit instead: measured, the label column is
          then exactly 192px (160 + MUI's 2x16px small padding) and the grid
          columns 46px, whatever the viewport -- and with 14 specs the table is
          850px, so the wrapping enclosure's `overflowX: 'auto'` still absorbs
          a matrix too wide for the page rather than widening the page. */}
      <Table size="small" sx={{ width: 'auto' }}>
        <TableHead>
          <TableRow>
            {/* The corner and the row labels stay horizontal; only the column
                headers rotate. */}
            <TableCell sx={{ verticalAlign: 'bottom' }} />
            {columnSpecs.map((col) => (
              <ColumnHeader key={col.id} model={col.model} />
            ))}
          </TableRow>
        </TableHead>
        <TableBody>
          {rowSpecs.map((rowSpec, rowOffset) => {
            // rowOffset is this row's index within rowSpecs (specs[1..n-1]);
            // its position in the FULL specs array is rowOffset+1, which is
            // also exactly how many columns (specs[0..rowOffset]) it draws --
            // the strictly-lower-triangle rule. The columns are taken from the
            // FRONT of the same `columnSpecs` the header row renders, which is
            // what keeps cell k under header k for every row.
            const visibleColumns = columnSpecs.slice(0, rowOffset + 1);
            return (
              <TableRow key={rowSpec.id}>
                <TableCell component="th" scope="row">
                  {/* The row-label column is the grid's remaining width hog.
                      Measured on the real component with five realistic
                      names at a 1100px viewport, it took 725px of a 1100px
                      table -- 66%, more than the whole rotated grid the
                      rotation had just bought. Capped here to the same budget
                      the header spends on its own scarce axis, it is 192px.

                      NO `text-overflow` and NO tooltip, deliberately: nothing
                      is hidden, so there is nothing to reveal on hover, and an
                      ellipsis at this width would render two sorted
                      neighbours identically. A horizontal label can meet the
                      cap by spending row height, which is the abundant axis.
                      The TableCell's own props stay bare so the <th> keeps its
                      `rowheader` role and its full accessible name. */}
                  <Box
                    component="span"
                    sx={{
                      // `display: block` is load-bearing, not tidiness:
                      // `max-width` has no effect on an inline box. Measured
                      // with this exact markup -- inline span: the column stays
                      // at its full content width; block span: 192px (160 +
                      // the 2x16px of MUI size="small" cell padding).
                      display: 'block',
                      maxWidth: `${LABEL_MAX_PX}px`,
                      // `anywhere`, not `break-word`: only `anywhere` lowers
                      // the box's MIN-content size, which is what actually
                      // forces the table column down to the cap under
                      // `table-layout: auto`. Hyphens are natural soft-break
                      // opportunities, so real model names break at '-' and
                      // `anywhere` only fires for a hyphen-free monster.
                      overflowWrap: 'anywhere',
                    }}
                  >
                    {rowSpec.model}
                  </Box>
                </TableCell>
                {visibleColumns.map((colSpec) => {
                  const [a, b] = canonicalPair(rowSpec.id, colSpec.id);
                  const allowed = pairAllowed(pairs, rowSpec.id, colSpec.id);
                  return (
                    <TableCell key={colSpec.id} align="center" sx={{ p: 0.5 }}>
                      <Tooltip
                        title={
                          <MatrixTooltip
                            t={t}
                            a={rowSpec}
                            b={colSpec}
                            budgets={budgets}
                            disabledReason={reason}
                          />
                        }
                      >
                        <span>
                          <Checkbox
                            size="small"
                            color="success"
                            checked={allowed}
                            disabled={disabled}
                            icon={
                              disabled ? (
                                <BlockIcon fontSize="small" />
                              ) : (
                                <CheckBoxOutlineBlankIcon fontSize="small" />
                              )
                            }
                            checkedIcon={
                              disabled ? (
                                <CheckCircleIcon fontSize="small" />
                              ) : (
                                <CheckBoxIcon fontSize="small" />
                              )
                            }
                            slotProps={{
                              input: {
                                'aria-label': `${t.runtimeMatrixCell}: ${rowSpec.model} + ${colSpec.model}`,
                              },
                            }}
                            // Guarded in the handler as well as via the
                            // `disabled` prop (the GroupsView idiom). A real
                            // browser delivers no click to a disabled input,
                            // but a dispatched change event still reaches
                            // React -- and here a toggle PUTs the complete
                            // replacement pair list, so "probably cannot
                            // happen" is not a good enough reason to leave the
                            // write path open.
                            onChange={() => {
                              if (disabled) return;
                              onToggle(a, b);
                            }}
                          />
                        </span>
                      </Tooltip>
                    </TableCell>
                  );
                })}
              </TableRow>
            );
          })}
        </TableBody>
      </Table>
    </Box>
  );
}
