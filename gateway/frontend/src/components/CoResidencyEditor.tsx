// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useEffect, useMemo, useRef, useState, type KeyboardEvent, type PointerEvent } from 'react';
import {
  Box,
  Chip,
  IconButton,
  List,
  ListItemButton,
  ToggleButton,
  ToggleButtonGroup,
  Typography,
} from '@mui/material';
import ExpandMoreIcon from '@mui/icons-material/ExpandMore';
import ChevronRightIcon from '@mui/icons-material/ChevronRight';
import type {
  CoResidency,
  CoResidencyMode,
  CoResidencyPair,
  CoResidencyVerdict,
  ModelRole,
} from '../api';
import type { MessageKey, Translation } from './shared/types';
import { Field } from './shared/Field';

/**
 * One model the editor can pair: a mapping of the server's application. `role`
 * is the backend's DERIVED per-spec role (RuntimeSpec.role) and the ONLY thing
 * the grouping reads -- never a capability flag, which is an AND over every
 * mapping of a name and can disagree with the best-ranked role.
 */
export type CoResidencyItem = { id: string; name: string; role: ModelRole };

/** A cell is one of the two explicit verdicts or `default`, which the mode resolves. */
type Verdict = CoResidencyVerdict | 'default';

// Fixed group order: the specialised kinds first (the ones an operator carves
// exceptions for), the general chat models and the catch-all last.
const ROLE_ORDER: readonly ModelRole[] = [
  'asr',
  'tts',
  'image',
  'embedding',
  'reranker',
  'chat',
  'other',
];

const ROLE_LABEL_KEY: Record<
  ModelRole,
  Extract<
    MessageKey,
    | 'coresidencyRoleAsr'
    | 'coresidencyRoleTts'
    | 'coresidencyRoleImage'
    | 'coresidencyRoleEmbedding'
    | 'coresidencyRoleReranker'
    | 'coresidencyRoleChat'
    | 'coresidencyRoleOther'
  >
> = {
  asr: 'coresidencyRoleAsr',
  tts: 'coresidencyRoleTts',
  image: 'coresidencyRoleImage',
  embedding: 'coresidencyRoleEmbedding',
  reranker: 'coresidencyRoleReranker',
  chat: 'coresidencyRoleChat',
  other: 'coresidencyRoleOther',
};

/**
 * A role this build does not know (a newer backend's new kind) is still a
 * model the operator must be able to pair, so it lands in `other` instead of
 * vanishing from every group.
 */
function knownRole(role: string): ModelRole {
  return (ROLE_ORDER as readonly string[]).includes(role) ? (role as ModelRole) : 'other';
}

// ---- the pair document ----------------------------------------------------

// A pair is symmetric: A<->B from either side is ONE entry, stored with the
// smaller id first. The backend sorts whatever it receives and rejects a pair
// that collides after sorting, but the client sends canonical so the entry it
// shows is the entry it wrote. Raw `<` (not a collator): ids are opaque and the
// backend compares them bytewise.
function canonical(x: string, y: string): [string, string] {
  return x < y ? [x, y] : [y, x];
}

function pairKey(x: string, y: string): string {
  const [a, b] = canonical(x, y);
  return `${a}\u0000${b}`;
}

/**
 * The document after setting `verdict` between `selectedId` and every id in
 * `targetIds`. The result is the COMPLETE set of explicit pairs -- every pair it
 * does not touch rides along, shown or not, because the save is a full
 * replacement and a pair left out is a pair erased. `default` removes the
 * explicit entry: absence IS default, there is no stored default verdict.
 */
function withVerdicts(
  pairs: readonly CoResidencyPair[],
  selectedId: string,
  targetIds: readonly string[],
  verdict: Verdict,
): CoResidencyPair[] {
  const targeted = new Set(targetIds.map((id) => pairKey(selectedId, id)));
  const kept = pairs.filter((p) => !targeted.has(pairKey(p.a, p.b)));
  if (verdict === 'default') return kept;
  const added = targetIds.map((id): CoResidencyPair => {
    const [a, b] = canonical(selectedId, id);
    return { a, b, verdict };
  });
  return [...kept, ...added];
}

// ---- ordering --------------------------------------------------------------

// Model names are size and quantisation ladders, and bare collation orders them
// "Llama-3.1-405B, Llama-3.1-70B, Llama-3.1-8B" -- an operator reads that as
// broken. `numeric: true` fixes the ladder at no cost. Constructed once: the
// editor re-renders on every change.
const nameCollator = new Intl.Collator(undefined, { numeric: true });

/** Raw code-unit order of two strings: -1, 0 (equal) or 1. */
function compareCodeUnits(a: string, b: string): number {
  if (a < b) return -1;
  if (a > b) return 1;
  return 0;
}

function compareItems(a: CoResidencyItem, b: CoResidencyItem): number {
  const byName = nameCollator.compare(a.name, b.name);
  if (byName !== 0) return byName;
  // `numeric: true` equates 8 with 08, so a collator tie is not a name tie; fall
  // back to raw code units and then to the id so the order is total and stable
  // (two mappings may legally carry the same display name).
  if (a.name !== b.name) return compareCodeUnits(a.name, b.name);
  return compareCodeUnits(a.id, b.id);
}

// ---- the draggable divider --------------------------------------------------

/** localStorage key of the model list's width (per viewer, per device). */
export const CORESIDENCY_LIST_WIDTH_KEY = 'op.coresidency.listWidth';
const LIST_WIDTH_MIN = 160;
const LIST_WIDTH_MAX = 560;
const LIST_WIDTH_DEFAULT = 280;
const LIST_WIDTH_STEP = 16;
const SEPARATOR_WIDTH = 6;
// What the detail pane must keep. Its rows are a name, an optional "from group"
// note and the tri-state control, and the control never shrinks: measured at
// 13px it is 333px wide under opt-out and 378px in German under opt-in, to which
// a row adds its indent and the pane's padding and a few words of name. Below
// this the control scrolls out of view, which is exactly the thing the pane
// exists to prevent -- so the list may only take what is left over.
const DETAIL_WIDTH_MIN = 620;
// The whole split view never gets narrower than its three parts at their minima;
// a narrower viewport scrolls the view sideways instead.
const SPLIT_MIN_WIDTH = LIST_WIDTH_MIN + SEPARATOR_WIDTH + DETAIL_WIDTH_MIN;

/**
 * The widest the list may be inside a container `containerWidth` wide: the
 * static cap, or what leaves the detail pane its minimum, whichever is less --
 * and never below the list's own minimum. An unmeasured container (0: before
 * layout, or no layout engine) falls back to the static cap.
 */
function maxListWidth(containerWidth: number): number {
  // NaN is as unmeasured as 0: it fails `> 0`, so it must take this exit too (a
  // bare `containerWidth <= 0` would let it through to a NaN width).
  if (Number.isNaN(containerWidth) || containerWidth <= 0) return LIST_WIDTH_MAX;
  const room = Math.floor(containerWidth) - SEPARATOR_WIDTH - DETAIL_WIDTH_MIN;
  return Math.max(LIST_WIDTH_MIN, Math.min(LIST_WIDTH_MAX, room));
}

function clampWidth(px: number, max: number): number {
  return Math.min(max, Math.max(LIST_WIDTH_MIN, Math.round(px)));
}

// Storage is a convenience: it can be missing, blocked, full or hold anything
// (a private window, a cleared profile, another build's value), and the editor
// must look and work the same without it. Every access is guarded, and a stored
// value outside the bounds is IGNORED rather than clamped -- it is not a width
// this editor ever wrote.
function readListWidth(): number {
  try {
    const raw = window.localStorage?.getItem(CORESIDENCY_LIST_WIDTH_KEY);
    if (raw === null || raw === undefined) return LIST_WIDTH_DEFAULT;
    const px = Number(raw);
    return Number.isFinite(px) && px >= LIST_WIDTH_MIN && px <= LIST_WIDTH_MAX
      ? Math.round(px)
      : LIST_WIDTH_DEFAULT;
  } catch {
    return LIST_WIDTH_DEFAULT;
  }
}

function writeListWidth(px: number): void {
  try {
    window.localStorage?.setItem(CORESIDENCY_LIST_WIDTH_KEY, String(px));
  } catch {
    /* best-effort: the width simply is not remembered */
  }
}

// ---- the tri-state control --------------------------------------------------

/**
 * Allowed / Default / Forbidden for one pair, or for a whole group. `value` is
 * null for a group whose members diverge (nothing is selected then, and the
 * group says "mixed"). `default` is labelled with what it currently MEANS, so the
 * mode is never something to remember: "Default (allowed)" under opt-out,
 * "Default (not allowed)" under opt-in. Clicking the selected segment does
 * nothing (an exclusive group reports null for it; a cell has no "unset" state
 * beyond Default).
 */
function VerdictControl({
  t,
  mode,
  label,
  value,
  disabled,
  onSelect,
}: Readonly<{
  t: Translation;
  mode: CoResidencyMode;
  label: string;
  value: Verdict | null;
  disabled: boolean;
  onSelect: (verdict: Verdict) => void;
}>) {
  const defaultLabel =
    mode === 'opt_out' ? t.coresidencyVerdictDefaultAllowed : t.coresidencyVerdictDefaultDenied;
  return (
    <ToggleButtonGroup
      exclusive
      size="small"
      aria-label={label}
      value={value}
      disabled={disabled}
      onChange={(_event, next: Verdict | null) => {
        if (next !== null) onSelect(next);
      }}
      // Pinned: never shrinks, so a long model name gives way to it.
      sx={{ flexShrink: 0 }}
    >
      <ToggleButton value="allow" color="success">
        {t.coresidencyVerdictAllow}
      </ToggleButton>
      <ToggleButton value="default">{defaultLabel}</ToggleButton>
      <ToggleButton value="forbid" color="error">
        {t.coresidencyVerdictForbid}
      </ToggleButton>
    </ToggleButtonGroup>
  );
}

// ---- the editor --------------------------------------------------------------

/**
 * The role-grouped co-residency editor (design 4.4): a per-server mode, a
 * searchable model list with a per-model count of explicit settings, and the
 * selected model's relationships grouped by role -- each group with a bulk
 * Allowed / Default / Forbidden control and a per-member override.
 *
 * CONTROLLED: `mode` and `pairs` are the stored document, and every edit calls
 * `onChange` with the COMPLETE next document (mode + every explicit pair). That
 * is the save contract -- a full replacement -- so the editor never emits a
 * partial document and the parent never has to merge one. `pairs` holds the
 * EXPLICIT settings only; what an unlisted pair means is `mode`, and switching
 * it changes only that: the explicit pairs go back out untouched.
 *
 * Editing a pair from either side is the same single entry (see `canonical`).
 * `disabled` is the parent's write gate (a PUT in flight): it freezes every
 * control that would write, never the list or the search.
 */
export function CoResidencyEditor({
  t,
  items,
  mode,
  pairs,
  onChange,
  disabled = false,
  disabledReason,
}: Readonly<{
  t: Translation;
  items: readonly CoResidencyItem[];
  mode: CoResidencyMode;
  pairs: readonly CoResidencyPair[];
  onChange: (next: CoResidency) => void;
  disabled?: boolean;
  disabledReason?: string;
}>) {
  const [selectedId, setSelectedId] = useState('');
  const [query, setQuery] = useState('');
  const [collapsed, setCollapsed] = useState<ReadonlySet<ModelRole>>(new Set());
  // The width the operator chose (and storage remembers). What is shown is that
  // width held within what the container leaves room for, so a width picked on a
  // wide window survives a visit from a narrow one without being rewritten.
  const [chosenWidth, setChosenWidth] = useState(readListWidth);
  const [containerWidth, setContainerWidth] = useState(0);
  const [dragging, setDragging] = useState(false);
  // The drag handlers read the latest width synchronously (a pointer-up must
  // persist the value the last move set, before React re-renders).
  const widthRef = useRef(chosenWidth);
  const draggingRef = useRef(false);
  const containerRef = useRef<HTMLDivElement>(null);
  const maxWidth = maxListWidth(containerWidth);
  const shownWidth = Math.min(chosenWidth, maxWidth);

  const sorted = useMemo(() => [...items].sort(compareItems), [items]);
  // A selection that vanished (a mapping was deleted) falls back to the first.
  const selected = sorted.find((item) => item.id === selectedId) ?? sorted[0];
  const hasPanes = items.length >= 2 && selected !== undefined;

  // Tracks the split view's width so the divider's upper bound follows it. A
  // ResizeObserver where there is one (every browser); one measurement where
  // there is not (jsdom), which then reports 0 and leaves the static bounds.
  useEffect(() => {
    const el = containerRef.current;
    if (!hasPanes || !el) return undefined;
    const measure = () => setContainerWidth(el.getBoundingClientRect().width);
    measure();
    if (typeof ResizeObserver === 'undefined') return undefined;
    const observer = new ResizeObserver(measure);
    observer.observe(el);
    return () => observer.disconnect();
  }, [hasPanes]);

  const verdicts = useMemo(() => {
    const byPair = new Map<string, CoResidencyVerdict>();
    for (const p of pairs) byPair.set(pairKey(p.a, p.b), p.verdict);
    return byPair;
  }, [pairs]);

  // Explicit settings per model. Only pairs whose two ends are both listed count:
  // a pair naming a model that is not here cannot be seen or changed, so counting
  // it would show an exception nobody can find. It is still sent back on save.
  const exceptionCounts = useMemo(() => {
    const known = new Set(items.map((item) => item.id));
    const counts = new Map<string, number>();
    for (const p of pairs) {
      if (!known.has(p.a) || !known.has(p.b)) continue;
      counts.set(p.a, (counts.get(p.a) ?? 0) + 1);
      counts.set(p.b, (counts.get(p.b) ?? 0) + 1);
    }
    return counts;
  }, [items, pairs]);

  const needle = query.trim().toLowerCase();
  const visible = needle
    ? sorted.filter((item) => item.name.toLowerCase().includes(needle))
    : sorted;

  function verdictBetween(otherId: string): Verdict {
    if (!selected) return 'default';
    return verdicts.get(pairKey(selected.id, otherId)) ?? 'default';
  }

  function setVerdicts(targetIds: readonly string[], verdict: Verdict) {
    if (!selected) return;
    onChange({ mode, pairs: withVerdicts(pairs, selected.id, targetIds, verdict) });
  }

  // Members of each non-empty role group, never the selection itself (a pair is
  // irreflexive), in name order.
  const groups = useMemo(() => {
    if (!selected) return [];
    return ROLE_ORDER.map((role) => ({
      role,
      members: sorted.filter((item) => item.id !== selected.id && knownRole(item.role) === role),
    })).filter((group) => group.members.length > 0);
  }, [sorted, selected]);

  function toggleGroup(role: ModelRole) {
    setCollapsed((current) => {
      const next = new Set(current);
      if (next.has(role)) next.delete(role);
      else next.add(role);
      return next;
    });
  }

  // ---- divider ----
  // Read live, not from state: a pointer or key event must clamp against the
  // container as it is now, not as the last render saw it.
  function liveMaxWidth(): number {
    return maxListWidth(containerRef.current?.getBoundingClientRect().width ?? 0);
  }

  function setListWidth(px: number, persist: boolean) {
    const next = clampWidth(px, liveMaxWidth());
    widthRef.current = next;
    setChosenWidth(next);
    if (persist) writeListWidth(next);
  }

  function onDividerPointerDown(event: PointerEvent<HTMLElement>) {
    if (event.button !== 0) return; // the primary button only
    draggingRef.current = true;
    setDragging(true);
    try {
      event.currentTarget.setPointerCapture?.(event.pointerId);
    } catch {
      /* a synthetic or already-released pointer: the drag still tracks moves */
    }
    event.preventDefault();
  }

  function onDividerPointerMove(event: PointerEvent<HTMLElement>) {
    if (!draggingRef.current) return;
    const left = containerRef.current?.getBoundingClientRect().left ?? 0;
    setListWidth(event.clientX - left, false);
  }

  function endDividerDrag(event: PointerEvent<HTMLElement>) {
    if (!draggingRef.current) return;
    draggingRef.current = false;
    setDragging(false);
    try {
      event.currentTarget.releasePointerCapture?.(event.pointerId);
    } catch {
      /* nothing captured */
    }
    writeListWidth(widthRef.current);
  }

  function onDividerKeyDown(event: KeyboardEvent<HTMLElement>) {
    const step = event.shiftKey ? LIST_WIDTH_STEP * 4 : LIST_WIDTH_STEP;
    const max = liveMaxWidth();
    // Steps start from what is SHOWN, which a narrow container may have clamped
    // below the width the operator chose.
    const shown = Math.min(widthRef.current, max);
    let next: number | null = null;
    if (event.key === 'ArrowLeft') next = shown - step;
    else if (event.key === 'ArrowRight') next = shown + step;
    else if (event.key === 'Home') next = LIST_WIDTH_MIN;
    else if (event.key === 'End') next = max;
    if (next === null) return;
    event.preventDefault();
    setListWidth(next, true);
  }

  const defaultHint =
    mode === 'opt_out' ? t.coresidencyDefaultAllowedHint : t.coresidencyDefaultDeniedHint;

  return (
    <Box sx={{ display: 'grid', gap: 2 }}>
      <Box sx={{ display: 'grid', gap: 0.75, justifyItems: 'start' }}>
        <Typography variant="subtitle2" component="div">
          {t.coresidencyModeLabel}
        </Typography>
        <ToggleButtonGroup
          exclusive
          size="small"
          aria-label={t.coresidencyModeLabel}
          value={mode}
          disabled={disabled}
          onChange={(_event, next: CoResidencyMode | null) => {
            // The switch is lossless: it sends the pairs it was given, unchanged,
            // together with the new mode -- never a mode-only document.
            if (next !== null && next !== mode) onChange({ mode: next, pairs: [...pairs] });
          }}
        >
          <ToggleButton value="opt_out">{t.coresidencyModeOptOut}</ToggleButton>
          <ToggleButton value="opt_in">{t.coresidencyModeOptIn}</ToggleButton>
        </ToggleButtonGroup>
        <Typography variant="body2" color="text.secondary">
          {defaultHint}
        </Typography>
        {disabled && disabledReason && (
          <Typography variant="caption" color="text.secondary" role="status">
            {disabledReason}
          </Typography>
        )}
      </Box>

      {!hasPanes || !selected ? (
        <Typography color="text.secondary">{t.coresidencyNeedTwo}</Typography>
      ) : (
        // Narrow viewports scroll sideways instead of stacking: a fixed layout
        // keeps the pinned control and the divider where they are.
        <Box sx={{ overflowX: 'auto' }}>
          <Box
            ref={containerRef}
            sx={{
              display: 'flex',
              alignItems: 'stretch',
              minWidth: SPLIT_MIN_WIDTH,
              minHeight: 360,
              maxHeight: '70vh',
              border: 1,
              borderColor: 'divider',
              borderRadius: 1,
              overflow: 'hidden',
            }}
          >
            <Box
              sx={{
                width: shownWidth,
                flexShrink: 0,
                display: 'flex',
                flexDirection: 'column',
                minHeight: 0,
              }}
            >
              <Box sx={{ p: 1 }}>
                <Field
                  id="coresidency-search"
                  value={query}
                  onChange={(event) => setQuery(event.target.value)}
                  size="small"
                  fullWidth
                  placeholder={t.coresidencySearch}
                  inputProps={{ 'aria-label': t.coresidencySearch }}
                />
              </Box>
              <List
                role="listbox"
                aria-label={t.coresidencyModelList}
                dense
                disablePadding
                sx={{ overflowY: 'auto', flex: 1, minHeight: 0 }}
              >
                {visible.map((item) => {
                  const count = exceptionCounts.get(item.id) ?? 0;
                  const isSelected = item.id === selected.id;
                  return (
                    <ListItemButton
                      key={item.id}
                      role="option"
                      aria-selected={isSelected}
                      selected={isSelected}
                      onClick={() => setSelectedId(item.id)}
                      sx={{ gap: 1 }}
                    >
                      {/* Truncated with an ellipsis; the full name is the hover
                          text. The count never shrinks, so it is always visible. */}
                      <Typography noWrap title={item.name} sx={{ flex: '1 1 auto', minWidth: 0 }}>
                        {item.name}
                      </Typography>
                      {count > 0 && (
                        <Chip
                          size="small"
                          color="primary"
                          variant="outlined"
                          label={count}
                          aria-label={t.coresidencyExceptions(count)}
                          sx={{ flexShrink: 0 }}
                        />
                      )}
                    </ListItemButton>
                  );
                })}
              </List>
              {visible.length === 0 && (
                <Typography color="text.secondary" sx={{ p: 1.5 }}>
                  {t.coresidencyNoMatch}
                </Typography>
              )}
            </Box>

            <Box
              role="separator"
              aria-orientation="vertical"
              aria-label={t.coresidencyResize}
              aria-valuenow={shownWidth}
              aria-valuemin={LIST_WIDTH_MIN}
              aria-valuemax={maxWidth}
              tabIndex={0}
              data-dragging={dragging}
              onPointerDown={onDividerPointerDown}
              onPointerMove={onDividerPointerMove}
              onPointerUp={endDividerDrag}
              onPointerCancel={endDividerDrag}
              onKeyDown={onDividerKeyDown}
              sx={{
                width: SEPARATOR_WIDTH,
                flexShrink: 0,
                cursor: 'col-resize',
                touchAction: 'none',
                bgcolor: 'divider',
                '&:hover, &:focus-visible, &[data-dragging="true"]': { bgcolor: 'primary.main' },
                outline: 'none',
              }}
            />

            <Box sx={{ flex: '1 1 0', minWidth: 0, overflow: 'auto', p: 2 }}>
              {/* The selected model in full: it wraps rather than truncates, so the
                  one name the operator is editing is always readable. */}
              <Box sx={{ display: 'flex', alignItems: 'baseline', gap: 1, flexWrap: 'wrap' }}>
                <Typography component="h3" variant="h6" sx={{ overflowWrap: 'anywhere' }}>
                  {selected.name}
                </Typography>
                <Chip
                  size="small"
                  variant="outlined"
                  label={t[ROLE_LABEL_KEY[knownRole(selected.role)]]}
                />
              </Box>
              <Typography variant="body2" color="text.secondary" sx={{ mb: 1.5 }}>
                {t.coresidencyDetailHint}
              </Typography>

              {groups.map(({ role, members }) => {
                const roleLabel = t[ROLE_LABEL_KEY[role]];
                const memberVerdicts = members.map((m) => verdictBetween(m.id));
                const mixed = new Set(memberVerdicts).size > 1;
                const groupValue: Verdict | null = mixed ? null : memberVerdicts[0];
                const isCollapsed = collapsed.has(role);
                const memberIds = members.map((m) => m.id);
                return (
                  <Box
                    component="section"
                    key={role}
                    aria-label={roleLabel}
                    sx={{ mb: 2, '&:last-child': { mb: 0 } }}
                  >
                    <Box sx={{ display: 'flex', alignItems: 'center', gap: 1 }}>
                      <IconButton
                        size="small"
                        aria-expanded={!isCollapsed}
                        aria-label={t.coresidencyGroupToggle(roleLabel)}
                        onClick={() => toggleGroup(role)}
                      >
                        {isCollapsed ? <ChevronRightIcon /> : <ExpandMoreIcon />}
                      </IconButton>
                      {/* The one flexible part of the header: the name, its size
                          and the "mixed" marker. The control beside it never
                          shrinks, so what is left over is all this block gets;
                          the chips WRAP under the name when it is cramped (the
                          name itself gives way last, with an ellipsis and its
                          full text on hover) instead of squeezing the name to
                          nothing. Measured: German under opt-in, a mixed group
                          and the list at its widest leave ~160px. */}
                      <Box
                        sx={{
                          flex: '1 1 0',
                          minWidth: 0,
                          display: 'flex',
                          flexWrap: 'wrap',
                          alignItems: 'center',
                          columnGap: 1,
                          rowGap: 0.25,
                        }}
                      >
                        <Typography
                          component="h4"
                          variant="subtitle1"
                          noWrap
                          title={roleLabel}
                          sx={{ minWidth: 0, maxWidth: '100%' }}
                        >
                          {roleLabel}
                        </Typography>
                        <Chip
                          size="small"
                          variant="outlined"
                          label={t.coresidencyGroupSize(members.length)}
                          sx={{ flexShrink: 0 }}
                        />
                        {mixed && (
                          <Chip
                            size="small"
                            color="warning"
                            label={t.coresidencyGroupMixed}
                            sx={{ flexShrink: 0 }}
                          />
                        )}
                      </Box>
                      <VerdictControl
                        t={t}
                        mode={mode}
                        label={t.coresidencyGroupControl(roleLabel)}
                        value={groupValue}
                        disabled={disabled}
                        onSelect={(verdict) => setVerdicts(memberIds, verdict)}
                      />
                    </Box>
                    {!isCollapsed &&
                      members.map((member, index) => (
                        <Box
                          key={member.id}
                          sx={{
                            display: 'flex',
                            alignItems: 'center',
                            gap: 1.5,
                            py: 0.5,
                            pl: 5,
                          }}
                        >
                          <Typography
                            noWrap
                            title={member.name}
                            sx={{ flex: '1 1 auto', minWidth: 0 }}
                          >
                            {member.name}
                          </Typography>
                          {/* While the whole group agrees its members simply
                              inherit it; once one deviates the group says "mixed"
                              and each row speaks for itself. A lone member has no
                              group to inherit from beyond itself. */}
                          {!mixed && members.length > 1 && (
                            <Typography
                              variant="caption"
                              color="text.secondary"
                              sx={{ flexShrink: 0 }}
                            >
                              {t.coresidencyFromGroup}
                            </Typography>
                          )}
                          <VerdictControl
                            t={t}
                            mode={mode}
                            label={t.coresidencyMemberControl(member.name)}
                            value={memberVerdicts[index]}
                            disabled={disabled}
                            onSelect={(verdict) => setVerdicts([member.id], verdict)}
                          />
                        </Box>
                      ))}
                  </Box>
                );
              })}
            </Box>
          </Box>
        </Box>
      )}
    </Box>
  );
}
