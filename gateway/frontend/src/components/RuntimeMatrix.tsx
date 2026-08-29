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
  // A collator tie is not always a NAME tie. `numeric: true` parses digit runs
  // as numbers, and 8 === 08 === 008, so the collator returns 0 for names that
  // differ only by a leading zero: measured, compare('Qwen3-8B','Qwen3-08B')
  // and compare('Llama-3.1-8B','Llama-3.01-8B') are both 0, where the bare
  // house collator returns 1. Without this line those two DISTINCT names would
  // fall through to the id tie-break below and be ordered by opaque hex --
  // deterministic, but silently contradicting "ordered by model name" in the
  // one place an operator could check it. Raw code units rather than a second
  // collator: this only ever runs inside one collator-equal bucket, so all it
  // has to be is a stable, total, name-derived order. Contrived (leading zeros
  // in model names are rare) and cosmetic, but it costs one comparison that
  // cannot fire for any name pair the collator already separates.
  if (a.model !== b.model) return a.model < b.model ? -1 : 1;
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
 * VERTICAL layout box (width one line-height, height the text length), which
 * a bare transform does not -- a transform is paint-time and reserves no
 * layout, so it forces the explicit heights and absolute positioning that
 * then drift a few pixels per column. Bottom-to-top (rather than
 * top-to-bottom) is the Western data-table/chart-axis convention, and it also
 * puts the START of the name next to the cells it labels.
 *
 * THE GRID WRAPPER IS THE FIX FOR A REAL BUG, and the sentence it replaces
 * was wrong. That sentence said the vertical layout box means "ordinary table
 * layout reserves the right footprint and the header stays glued to its
 * column". True in Blink and Gecko. False in WebKit -- i.e. Safari on macOS
 * and iOS, and every WKWebView, which is where an operator reported the
 * headers overprinting each other. The same sentence stood in the canonical
 * architecture doc (cross-cutting/agent-runtime-manager.md) for one commit
 * longer than it stood here; it is corrected there now, and the numbers below
 * are repeated there rather than living only in a commit message.
 *
 * A rotated box is an ORTHOGONAL FLOW: its BLOCK size runs along the table's
 * inline axis, so a column can only be wide enough for a wrapped header if
 * the layout asks the rotated box for that block size. Measured with the
 * <tbody> removed entirely, so the column is sized by its header alone (one
 * 51-character name, 3 line boxes, rotated box 72px, cell padding 4+4px):
 *
 *     Chromium 80px    Firefox 80px    WebKit 8px
 *
 * 8px is the padding. WebKit contributes NOTHING from the rotated box -- not
 * a reduced amount, none. In the real grid the column then falls back to its
 * only other content, the 46px checkbox cell, for ANY line count; the rotated
 * box still lays out at its correct 24px-per-line block size and simply hangs
 * out of the cell to the right, onto the next header's glyphs.
 *
 * TWO EXTENTS OVERLAP AT DIFFERENT LINE COUNTS, so say which one you mean.
 * The label's BORDER BOX is 24px per line box. Its INK -- the union of the
 * text-run rects, i.e. what is actually painted -- is 8px narrower, 4px of
 * half-leading on each side. Measured in WebKit for a header of L >= 2 line
 * boxes in a 46px column, against the NEXT header:
 *
 *     box into next header's box   24*L - 46    +2  +26  +50  +74  (L = 2..5)
 *     ink over next header's ink   24*L - 54    -6  +18  +42  +66  (L = 2..5)
 *
 * So the BOX crosses the column boundary from TWO line boxes on, while the
 * GLYPHS -- the overprinting an operator can see, and the thing that was
 * reported -- first touch at THREE. Both hold at every container width
 * measured (1400 down to 360) and at 5, 8 and 14 specs. The one exception is
 * in the safe direction: a ONE-LINE neighbour's 24px label still fits its
 * 46px column, so `mx: auto` centres it and buys 7px more clearance. Three
 * line boxes is a 44-character name at this font and this 160px cap, which
 * real GGUF names reach easily.
 *
 * `display: grid` on the wrapper is what makes every engine ask. Measured one
 * declaration at a time in WebKit with a 3-line name: grid and flex wrappers
 * both give the same 80px column Blink and Gecko already gave; an inline-block
 * wrapper, `width: max-content`, `width: fit-content`, a float, a
 * `display: table` box, `overflow: hidden` (on the label or on the cell),
 * `transform: none`, `vertical-align: top` and `min-width: 0` all leave it at
 * 46px. So the wrapper is not a spacer, and swapping it for a plainer box
 * reopens the bug.
 *
 * GRID AND NOT FLEX, and that is the one thing here that cannot be guessed
 * from the CSS. Both are correct on a freshly loaded page, in all three
 * engines. Only grid stays correct when the component RE-RENDERS, which this
 * one does on every telemetry poll: measured over 400 randomised re-renders
 * from a SINGLE mount (2-9 specs, 360-1499px container, names of 1 to 6 line
 * boxes, seeded), in WebKit grid produced 0 bad renders and flex 31 -- 0 and
 * 32 bad columns out of 1750. Chromium and Firefox: 0 for both, so no amount
 * of testing in those two would have separated them.
 *
 * FLEX FAILS THE SAME WAY THE BUG DOES, NOT WORSE, and the claim that it
 * CLIPS characters -- which is what this comment said first -- measures
 * FALSE. In the failing renders `overflow` computes to `visible` on both the
 * wrapper and the label, every line box is present and painted (a 5-line name
 * still reports 5 line-box rects spanning 120px), and no character is lost.
 * What goes stale is the flex CONTAINER, which keeps the previous render's
 * width; the rotated label overflows it and hangs out, exactly as it does
 * unfixed. Measured on the deterministic worst case (mount a 1-line name,
 * re-render to L lines, 5 specs, 1100px), the wrapper stays 38px wide against
 * a scrollWidth of 55 to 127, and flex's ink-over-ink overlap is
 * -6 / +18 / +18 / +42 / +66
 * at L = 2..6 -- IDENTICAL to the un-fixed component; only the BOX overlap is
 * smaller (-22px, constant), because the stale box is one line wide. So flex
 * is this same defect on ~8% of re-renders instead of all of them. That is a
 * sufficient reason to reject it and it is the TRUE one. A rejected
 * alternative dismissed for a reason that does not measure is how it gets
 * reinstated by the next person who checks.
 *
 * The wrapper costs one element. In BLINK it changes nothing: measured
 * byte-identical before and after at all 54 sampled configurations (24 with
 * the realistic 14-name set at 3 spec counts x 8 container widths, 30 with
 * uniform names at 1-6 line boxes x 5 container widths) -- column widths, row
 * heights, header-row height, table box and the label's offset inside its
 * cell. In GECKO IT IS NOT byte-identical, and the `justify-content` paragraph
 * below says why: every configuration containing a ONE-LINE header moves,
 * because that is the second defect this fixes. Everything else in Gecko is
 * identical.
 *
 * THE COST IS HORIZONTAL, ONLY WEBKIT PAYS IT, AND IT IS NOT SMALL. Each
 * column whose header wraps to L >= 2 line boxes grows by 24*L - 38 px (10px
 * at 2 lines, 34px at 3, 58px at 4, 82px at 5); a one-line header costs
 * nothing. Measured in WebKit with fourteen realistic specs -- thirteen
 * columns, eight of them two-line and five one-line, so 8 x 10px = 80px; the
 * same set the `width: auto` note below is measured on, and it reproduces
 * that note's 870 x 828px table in Chromium to the pixel:
 *
 *     container >= 900px   table 790 -> 870px wide, height unchanged (828px)
 *     container    800px   table pinned at 800px; the row-label column gives
 *                          up 192 -> 122px and the table grows 828 -> 1107px
 *                          TALL (+34%). Nothing scrolls in either
 *                          (scrollWidth === clientWidth === 800).
 *     container <= 700px   table 707 -> 787px, which now overflows the
 *                          enclosure and scrolls; height unchanged (1287px)
 *
 * At five specs it is 376 -> 396px and at eight 514 -> 564px. Do not quote
 * the five-identical-3-line-names case (376 -> 512px) as "the cost": that is
 * a synthetic worst case, not a realistic grid. `overflowX: auto` absorbs the
 * growth only where the table already exceeds its container; in the band
 * between the two shrink-to-fit widths it is paid in ROW HEIGHT instead,
 * which is the lossless degradation the row-label note below documents.
 *
 * `justify-content: center` is where the label's centring lives now, and it
 * quietly fixes a second, smaller cross-engine defect. It replaces
 * `mx: 'auto'` on the rotated box, which was the less honest half of the old
 * arrangement: horizontal is the BLOCK axis under a vertical writing mode,
 * and the engines disagreed about auto margins there. Measured, a one-line
 * header in its 46px column: Chromium and WebKit resolved the margins to
 * 7px/7px and centred it, Firefox resolved both to 0px and left it FLUSH
 * AGAINST the left edge of its own column. With the grid wrapper all three
 * put it at 11px/11px from the cell edge -- Chromium and WebKit unchanged to
 * the pixel, Firefox corrected. The auto margins never caused the overlap
 * (in every failing case they were over-constrained to 0px in all three
 * engines), so that part is a simplification, not a second fix.
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
 * Wrapping spends the abundant axis instead of characters: a second 24px-wide
 * vertical line beside the first, so the header's own footprint is 24*L plus
 * the cell's 2x4px padding -- 32px at one line, 56px at two -- and the header
 * row is 168.5px once a name reaches the cap (168px in WebKit, which rounds
 * the row differently; the column widths above are identical in all three).
 * The COLUMN is 46px until two lines, but that floor is the checkbox body
 * cell, not the header, and the distinction is worth keeping straight: "the
 * column is 46px whatever the header needs" is precisely the broken WebKit
 * behaviour described above. Reading order survives, because
 * after the 180° turn line 1 is the LEFT vertical line and line 2 the right,
 * i.e. ordinary reading order once your head is tilted the way the rotation
 * already assumes.
 *
 * `overflow: hidden` is gone rather than kept "as a backstop": with wrapping
 * the inline size cannot exceed the cap, so it could only ever hide something
 * silently -- the exact failure mode being removed. It is also, measured, not
 * the missing guard against the overlap above and never was: the rotated box
 * holds all of its own content exactly (clientWidth === scrollWidth === 72 in
 * the failing case), so there is nothing there to clip. What left the cell
 * was the BOX, not its overflow.
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
      // No `align="center"`: it sets `text-align`, which centres INLINE
      // content, and the label now sits in a block-level grid container that
      // text-align cannot move. The wrapper's `justify-content` centres it
      // instead -- one centring mechanism, on the box that owns the free
      // space. (Measured: removing it changes no geometry in any engine.)
      //
      // Bottom-aligned so short and long names share the edge nearest the
      // grid instead of floating at different heights above it.
      sx={{ verticalAlign: 'bottom', p: 0.5 }}
    >
      {/* The header's tooltip is the model name and nothing else -- kept
          deliberately distinct from the cell tooltip below, which explains
          what a co-residency pair does. Conflating them would bury the
          consequence sentence under a list of column labels.

          WHAT IT REVEALS IS THE ORIENTATION, NOT HIDDEN CHARACTERS -- and
          that is the whole reason this axis has a tooltip while the row label
          below deliberately has none. Since both axes wrap, neither hides
          anything, so "reveal the truncated tail" is no longer a reason for
          EITHER of them and the doc no longer claims it. What is left is that
          this header is rotated 90 degrees and wrapped onto two vertical
          lines, which is genuinely harder to read than running text; the
          tooltip renders the same string horizontally on one line. A row
          label is already horizontal, so a tooltip there would repeat
          identical text in an identical orientation -- pure noise. Same
          premise (nothing is hidden), different conclusions, because the two
          axes differ in something else: only one of them is rotated. */}
      <Tooltip title={model}>
        {/* THE SIZING WRAPPER. Not a spacer and not tidiness: it is the one
            box shape every engine measures an orthogonal child through, and
            `grid` specifically -- `flex` measures it right on a fresh load
            and stale on 31 of 400 WebKit re-renders, which reopens exactly
            this bug. See the doc block above for the measurements. */}
        <Box sx={{ display: 'grid', justifyContent: 'center' }}>
          <Box
            component="span"
            sx={{
              display: 'block',
              writingMode: 'vertical-rl',
              transform: 'rotate(180deg)',
              // In a vertical writing mode the INLINE axis is vertical, so
              // `max-height` is the inline-size cap -- the same budget the
              // horizontal row labels spend as `max-width`. Long names meet it
              // by wrapping onto another 24px line, never by losing characters.
              whiteSpace: 'normal',
              // The SAME wrap mode as the row label, on purpose: one
              // declaration means one thing in this component. On this axis
              // the choice is measurably free -- `anywhere` and `break-word`
              // give byte-identical geometry (header row 168.5px, span 160px,
              // widest header column 56px, 2 line boxes, at BOTH a 1100px and
              // a 600px container). Re-measured after the wrapper landed:
              // identical in Chromium, Firefox AND WebKit -- column widths,
              // header-row height, table box, row heights, line counts and
              // the label's offset in its cell -- over 48 comparisons (1 to 5
              // line boxes x 1100px and 600px containers x 3 engines), 0
              // differences.
              //
              // Say only that. The version of this note that shipped first
              // reasoned "table layout distributes WIDTH" and concluded the
              // header's WIDTH was therefore safe -- a claim about the other
              // axis, and a false one: see the doc block above. The wrap mode
              // is free here; the width was not.
              overflowWrap: 'break-word',
              maxHeight: `${LABEL_MAX_PX}px`,
            }}
          >
            {model}
          </Box>
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
      {/* `width: auto` overrides MUI's `width: 100%`, and THIS -- not the wrap
          mode -- is what makes the row-label cap above reach the COLUMN rather
          than only the text. Under `table-layout: auto` a 100%-wide table
          hands its surplus width back to the columns in proportion to their
          content, so the capped 160px label still sat in a 491px cell
          (measured, 5 specs at a 1100px viewport) with ~330px of empty gutter
          between the name and its own first checkbox. Shrink-to-fit instead:
          measured with 14 realistic specs, the label column is 192px (160 +
          MUI's 2x16px small padding) and the grid columns 46px, and the table
          is 870px wide / 828px tall.

          Those numbers hold while the container can afford the table's
          shrink-to-fit width -- here ~870px, i.e. any container at or above
          it. BELOW that the table has to give something up, and auto layout
          takes it from the widest column: measured, the label column is still
          192px at 900px, 182px at 860px and 122px at 800px, with rows growing
          taller as the name wraps onto more lines. That degradation is
          lossless (no character is dropped) and bounded by `break-word` above,
          which floors the column at the longest unbreakable run -- 109px for
          these names, and 192px again as soon as one name has no hyphen at
          all. It is NOT true that it only happens where the grid already
          scrolls: at an 800px container the enclosure's scrollWidth equals its
          width (800 = 800, measured), so nothing scrolls yet and the label has
          already given up 70px. `overflowX: 'auto'` is what absorbs the rest,
          and it does take over further down: at 600px the box is 600px around
          a 787px table. */}
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

                      NO `text-overflow`: an ellipsis at this width would
                      render two sorted neighbours identically. A horizontal
                      label can meet the cap by spending row height, which is
                      the abundant axis.

                      NO tooltip either, and the reason is NOT "nothing is
                      hidden" -- nothing is hidden on the column axis either,
                      yet that one keeps its tooltip. The difference is
                      orientation: the column header is rotated 90 degrees and
                      its tooltip un-rotates it, while this label is already
                      horizontal running text, so a tooltip would repeat the
                      same characters in the same orientation. See the note at
                      ColumnHeader's Tooltip.
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
                      // `break-word`, and deliberately NOT `anywhere`. Both
                      // wrap and neither drops a character; they differ only
                      // in the MIN-content size the box reports to
                      // `table-layout: auto`, and on that axis `anywhere` is
                      // a defect. It makes min-content ONE CHARACTER, so auto
                      // layout always prefers squeezing this column over
                      // letting the table overflow -- which disarms the
                      // enclosure's `overflowX: auto` instead of using it.
                      // Measured in Chromium, 14 realistic specs, default MUI
                      // theme, container width varied (label column / tallest
                      // row / table height):
                      //
                      //   1100px   192 /  53 /   828    (both modes)
                      //    800px   122 /  93 / 1,099    (both modes)
                      //    600px   `anywhere`   43.8 / 734 / 6,703
                      //            `break-word`  109 / 133 / 1,379
                      //
                      // At 600px under `anywhere` every label renders ONE
                      // CHARACTER PER LINE (36 line boxes: "Q","w","e","n",
                      // "3","-",...). The three sorted-adjacent Qwen siblings
                      // are then identical for the first ~26 lines and differ
                      // hundreds of pixels down a row taller than the
                      // viewport, so no two of them can be seen together --
                      // "two different facts drawn alike", the exact defect
                      // this component exists to remove, relocated from the
                      // ellipsis axis onto the viewport axis. It is reachable
                      // on ordinary hardware: with the 264px NavSidebar and
                      // App.tsx's `px: clamp(20px,4vw,54px)`, a 1280px window
                      // leaves ~866px of content and a 1024px window ~630px.
                      //
                      // `break-word` reports the longest UNBREAKABLE run
                      // instead. Hyphens are soft-break opportunities, so a
                      // real model name floors at its longest hyphen-free
                      // segment and the table overflows and scrolls as
                      // designed. A hyphen-free name is still bounded, and by
                      // `max-width` rather than by the wrap mode: max-width
                      // clamps the box's min-content CONTRIBUTION, so a
                      // 43-character hyphen-free name measured exactly 160px
                      // at 1100px AND at 600px inside a 15-spec grid.
                      overflowWrap: 'break-word',
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
