// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useEffect, useId, useMemo, useRef, useState } from 'react';
import {
  Alert,
  Box,
  Button,
  Collapse,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  Tab,
  Tabs,
  Typography,
} from '@mui/material';
import type { RuntimeLogBatch, RuntimeLogCommand, RuntimeLogEntry, RuntimeLogState } from '../api';
import type { PortalApi, Translation } from './shared/types';

/**
 * The live view of ONE managed model process's stdout+stderr, opened from a
 * row of the live-status tab. It is where an operator ends up when a row says
 * `crashed`, or when a model will not finish loading, and the question is
 * always the same: what is the process actually printing?
 *
 * Three properties shape everything here, and each of them is a rule about not
 * lying to the reader:
 *
 *  1. **Opening this view is what makes the agent stream.** The subscription
 *     is the request; the unsubscribe is the stop. So the effect's cleanup is
 *     not housekeeping -- it is what keeps an unwatched fleet quiet -- and the
 *     subscription is deliberately torn down when the dialog CLOSES, not merely
 *     when the screen unmounts.
 *  2. **An empty window always says why.** Three different silences reach here
 *     (see RuntimeLogState) plus a fourth -- a connected agent whose retained
 *     buffer is genuinely empty, which is what an agent restart leaves behind.
 *     All four are rendered as sentences. An unexplained empty window is
 *     indistinguishable from "this model prints nothing", which is exactly the
 *     question the operator opened this to answer.
 *  3. **Every gap is visible, in one unit.** `dropped_bytes` is rendered
 *     wherever it appears, and this component's own display cap produces the
 *     same kind of notice when it trims -- on BOTH arrival paths, live and
 *     scrollback. A gap shown as silence would be a lie about what the process
 *     printed. Both counts are UTF-8 bytes, because the agent's is
 *     (`len(e.Text)` in Go) and two numbers on one screen that are both called
 *     bytes have to be the same quantity -- see `utf8ByteLength`.
 *
 * Inside the output, attached to each generation's start marker, sits the
 * **resolved command** the agent actually executed -- see `InlineCommand`. It
 * answers the question the output alone cannot: the operator wrote a template,
 * and `${PORT}`, `${MODEL}`, `${HOST_GPU_IDS}` and `${AGENT_ENV:NAME}` were all
 * resolved at launch, so the gap between what they typed and what ran is exactly
 * where the bug tends to be.
 *
 * It lives WITH the marker rather than in a panel of its own, and that is the
 * fourth rule about not lying to the reader: a panel would have to claim which
 * generation it describes, while the marker already IS that generation's
 * boundary and carries its pid. In a crash loop the operator then reads each
 * attempt's own command beside its own output -- including that `${PORT}`
 * differed between attempts, which a single "latest command" view cannot show.
 *
 * A replicable spec runs several instances, and every one of them writes into
 * the spec's one buffer: the agent prefixes each line of instance k >= 1 with
 * `[#k] ` and names the instance on every entry (`instance`). The view then
 * offers tabs. "All" shows the interleaved history as it arrives, prefixes
 * included, and marks instance 0's unprefixed lines `[#0] ` -- see `allView`;
 * one tab per instance shows only that instance's entries, its prefix removed
 * -- see `instanceView`. Opened from an instance's row, the
 * view starts on that instance's tab. A gap belongs to no instance (the
 * bytes lost may have come from any of them), so every tab shows every gap.
 *
 * A spec may also declare log files (#430) that the agent tails beside the
 * process output, each as its own STREAM named after the file (`stream` on the
 * entry; absent for the process). The view then offers a SECOND row of tabs
 * below the instance row: the source -- "Process" and one tab per declared
 * file, in declaration order. The two rows are independent filters over the
 * history: each stream has its own bounded window (see StreamWindows; one
 * stream never evicts another), the source row picks one, and the instance
 * row's `allView` / `instanceView` then work on its entries exactly as they
 * do on process output, because a file line of instance k >= 1 carries
 * the same `[#k] ` prefix and `instance` field. Each row keeps its selection
 * while the other changes; every opening starts on the process source. A gap
 * names its stream, so it shows in its own source's tabs only. A spec without
 * log files shows no source row, and the dialog looks as it did before. An
 * empty file tab says why -- the file's latest status (see `fileStatus`;
 * under "All" only a status every instance shares), an agent without
 * `runtime_log_files`, or simply nothing written yet -- and a stream the spec
 * does not declare is never kept or shown at all. A file tab is titled
 * "Logs" and has its own intro; "Process output" stays the process tab's.
 */

/**
 * How many entries the browser keeps PER STREAM (see StreamWindows). The
 * agent's own buffer is the history (megabytes, operator-sized); this is only
 * what the DOM holds -- one stream is rendered at a time -- so it is sized for
 * rendering cost rather than for retention. Trimming past it is reported,
 * never silent -- see LogWindow.
 *
 * Exported so a test can name the cap instead of restating 4000, which would
 * silently stop testing the boundary the day this changes.
 */
export const maxRenderedEntries = 4000;

/**
 * The UTF-8 byte length of a string, which is what `dropped_bytes` already
 * means everywhere it appears: the agent produces it as Go's `len(e.Text)`,
 * i.e. real bytes.
 *
 * `text.length` was the obvious thing to reach for and is NOT that number --
 * it counts UTF-16 code units. The two agree on ASCII and diverge by up to 4x
 * on anything else (`ü` is 1 unit and 2 bytes, `😀` is 2 units and 4 bytes), so
 * a model printing German, Chinese or emoji made this view render two counts
 * side by side, both labelled bytes, that did not mean the same quantity.
 * `TextEncoder` is exact; nothing cheaper is.
 */
const utf8 = new TextEncoder();
function utf8ByteLength(text: string): number {
  return text === '' ? 0 : utf8.encode(text).length;
}

/**
 * What the view currently holds, and what it had to throw away to hold it.
 *
 * The two live in ONE state value because the second is a fact about the
 * first: the count is only correct if it was computed from the very slice that
 * was discarded, and keeping them apart is what let the scrollback path reset
 * the counter to zero while dropping a history's head. It also keeps the state
 * updater pure -- it derives the whole next value from the previous one rather
 * than calling a second setter from inside itself.
 *
 * `entries` is the loss in ENTRIES, `bytes` the loss in UTF-8 bytes. Both are
 * needed and neither implies the other: a trimmed run of lifecycle markers
 * carries no text at all, so `bytes` can legitimately be 0 while a start
 * marker -- and with it a generation's only copy of its command -- is gone.
 */
type LogWindow = {
  entries: RuntimeLogEntry[];
  trimmedEntries: number;
  trimmedBytes: number;
};

const emptyLogWindow: LogWindow = { entries: [], trimmedEntries: 0, trimmedBytes: 0 };

/**
 * Appends a batch and enforces the display cap, counting whatever the cap
 * discards.
 *
 * Both arrival paths go through here, and that is the point rather than tidy
 * reuse: a scrollback starts from `emptyLogWindow` (it REPLACES the view --
 * a reconnect delivers a fresh history and appending would duplicate it),
 * while a live batch continues from the previous window. What neither may do
 * is drop entries without saying so, and there is now exactly one place that
 * could.
 */
function appendBounded(prev: LogWindow, incoming: readonly RuntimeLogEntry[]): LogWindow {
  const next = prev.entries.concat(incoming);
  if (next.length <= maxRenderedEntries) return { ...prev, entries: next };
  const cut = next.slice(0, next.length - maxRenderedEntries);
  return {
    entries: next.slice(-maxRenderedEntries),
    trimmedEntries: prev.trimmedEntries + cut.length,
    trimmedBytes: prev.trimmedBytes + cut.reduce((sum, e) => sum + utf8ByteLength(e.text ?? ''), 0),
  };
}

/**
 * One `LogWindow` per stream, keyed like `streamView`'s source: `''` for the
 * process output, otherwise the declared log file's name.
 *
 * One window per stream, each under its own display cap, because the agent
 * keeps one ring per stream and replays them one after another: with a single
 * shared window a busy file pushed every process line out of the view (and the
 * process tab then said the agent retained nothing, beside a trimmed notice
 * that belonged to the file), and a process flood trimmed a file's status
 * event away. The trimmed count is per window as well, so a tab reports only
 * its own loss. A Map, not an object: `__proto__` is a valid stream name.
 */
type StreamWindows = ReadonlyMap<string, LogWindow>;

const noStreamWindows: StreamWindows = new Map();

/**
 * Appends a batch to the windows of the streams it names, each bounded by
 * `appendBounded`. Entries of a stream outside `declared` are dropped: they
 * are never shown, and keeping them would let an undeclared name grow the
 * view's memory past the per-stream bound.
 */
function appendToStreams(
  prev: StreamWindows,
  incoming: readonly RuntimeLogEntry[],
  declared: ReadonlySet<string>,
): StreamWindows {
  const keys = new Set(incoming.map((e) => e.stream ?? ''));
  let next: Map<string, LogWindow> | null = null;
  for (const key of keys) {
    if (!declared.has(key)) continue;
    next ??= new Map(prev);
    next.set(key, appendBounded(prev.get(key) ?? emptyLogWindow, streamView(incoming, key)));
  }
  return next ?? prev;
}

/** Which part of the history the view shows: everything, or one instance's. */
type LogTab = 'all' | number;

/**
 * The entries of one source: `''` is the process output (entries that name no
 * stream, which is all an agent without log files sends, gaps included), any
 * other value the declared log file of that name. Applied BEFORE `allView` /
 * `instanceView`, so the instance row filters within the selected source.
 */
export function streamView(entries: readonly RuntimeLogEntry[], source: string): RuntimeLogEntry[] {
  return entries.filter((e) => (e.stream ?? '') === source);
}

/** Why a file tab has nothing, or nothing more, to show. */
export type FileStatus = 'waiting' | 'not_permitted' | 'binary' | 'missing';

// The file events that set or clear a file's status. `file_opened` clears it:
// the file is there and being read. The remaining file events (rotated,
// truncated, line cut, stale) are markers inside the history and change no
// status.
const fileStatusByEvent = new Map<string, FileStatus | null>([
  ['file_opened', null],
  ['file_waiting', 'waiting'],
  ['file_not_permitted', 'not_permitted'],
  ['file_binary', 'binary'],
  ['file_missing', 'missing'],
]);

/**
 * The status of a file stream as the shown entries leave it: the latest
 * status-bearing event decides, and none -- or `file_opened` latest -- means
 * the file has no status to report. A generation of a `${CONFIG_DIR}` file
 * normally begins with `file_waiting` (the agent removed the file before exec)
 * followed by `file_opened`, so "waiting" stands only until the process
 * creates its file.
 */
export function fileStatus(entries: readonly RuntimeLogEntry[]): FileStatus | null {
  for (let i = entries.length - 1; i >= 0; i--) {
    const event = entries[i].event;
    if (event !== undefined && fileStatusByEvent.has(event)) {
      return fileStatusByEvent.get(event) ?? null;
    }
  }
  return null;
}

/**
 * A file's status under "All" while the view offers instance tabs: every
 * instance tails its own copy of the file, so the latest event of the
 * interleaved history says nothing about the others -- instance 1's
 * `file_opened` would hide instance 0's `file_not_permitted`, and instance 0's
 * refusal would be claimed for instance 1. A status is therefore stated only
 * when every known instance has that same one; otherwise none, and the
 * instance tabs carry the details.
 */
function sharedFileStatus(
  entries: readonly RuntimeLogEntry[],
  instances: readonly number[],
): FileStatus | null {
  let shared: FileStatus | null = null;
  for (const k of instances) {
    const status = fileStatus(entries.filter((e) => entryInstance(e) === k));
    if (status === null || (shared !== null && status !== shared)) return null;
    shared = status;
  }
  return shared;
}

/**
 * The portal's wording for a marker inside a file's history, or null for any
 * other event. Like the generation markers, the agent sends only the kind.
 */
function fileMarkerLabel(event: string | undefined, t: Translation): string | null {
  switch (event) {
    case 'file_opened':
      return t.runtimeLogsFileOpened;
    case 'file_rotated':
      return t.runtimeLogsFileRotated;
    case 'file_truncated':
      return t.runtimeLogsFileTruncated;
    case 'file_line_cut':
      return t.runtimeLogsFileLineCut;
    case 'file_stale':
      return t.runtimeLogsFileStale;
    default:
      return null;
  }
}

const noLogFiles: readonly string[] = [];

/**
 * The instance an entry belongs to. An agent from before instances sends no
 * field, and its output is the spec's own process: instance 0.
 */
function entryInstance(entry: RuntimeLogEntry): number {
  const k = entry.instance;
  return typeof k === 'number' && Number.isInteger(k) && k > 0 ? k : 0;
}

function hasGap(entry: RuntimeLogEntry): boolean {
  return (entry.dropped_bytes ?? 0) > 0;
}

/**
 * The instance indexes the view knows, ascending: instance 0, which every spec
 * has, the ones the opening row listed, and every one the stream has named
 * since the dialog opened.
 */
function knownInstances(listed: readonly number[], seen: ReadonlySet<number>): number[] {
  const all = new Set<number>([0, ...seen]);
  for (const k of listed) if (Number.isInteger(k) && k >= 0) all.add(k);
  return [...all].sort((a, b) => a - b);
}

/**
 * Whether the view offers tabs. A replicable spec always does, even while only
 * instance 0 runs. A spec that is not replicable, and for which no instance
 * k >= 1 is known (from the opening row or the open stream), looks exactly as
 * it did before instances existed: no tabs, and nothing said about instances.
 */
function showsTabs(allInstances: boolean, known: readonly number[]): boolean {
  return allInstances || known.length > 1;
}

const noInstancesSeen: ReadonlySet<number> = new Set();

/** `seen` plus every instance `entries` name -- `seen` itself when none is new. */
function withInstancesOf(
  seen: ReadonlySet<number>,
  entries: readonly RuntimeLogEntry[],
): ReadonlySet<number> {
  let next: Set<number> | null = null;
  for (const e of entries) {
    const k = entryInstance(e);
    if (k > 0 && !seen.has(k) && !next?.has(k)) {
      next ??= new Set(seen);
      next.add(k);
    }
  }
  return next ?? seen;
}

/**
 * Removes `prefix` from the start of every line of `text`, except the first
 * when `midLine` says the text continues a line an earlier chunk opened. Only a
 * prefix that is actually there is removed.
 */
function stripLinePrefixes(text: string, prefix: string, midLine: boolean): string {
  return text
    .split('\n')
    .map((line, i) =>
      (i === 0 && midLine) || !line.startsWith(prefix) ? line : line.slice(prefix.length),
    )
    .join('\n');
}

/**
 * Puts `prefix` at the start of every line of `text`, except the first when
 * `midLine` says the text continues a line an earlier chunk opened, and except
 * the empty piece after a trailing newline: a line is prefixed when its first
 * byte arrives, so a blank line gets one and the line not begun yet does not.
 */
function addLinePrefixes(text: string, prefix: string, midLine: boolean): string {
  const lines = text.split('\n');
  return lines
    .map((line, i) =>
      (i === 0 && midLine) || (i === lines.length - 1 && line === '') ? line : prefix + line,
    )
    .join('\n');
}

/**
 * What the "All" tab shows while the view offers tabs: the interleaved history
 * as it arrives, except that instance 0's lines -- which the agent writes
 * unprefixed, as before instances existed -- get the `[#0] ` prefix the others
 * carry, so no line in the tab is left unattributed. Gaps and generation
 * markers are the portal's own notices and stay as they are.
 *
 * The line state is instance 0's, tracked exactly as `instanceView` tracks it:
 * a chunk that continues instance 0's open line keeps its first bytes
 * unprefixed, and a gap or one of instance 0's generation markers opens a new
 * line. Other instances' chunks interleave without touching it.
 */
function allView(entries: readonly RuntimeLogEntry[]): RuntimeLogEntry[] {
  let midLine = false;
  return entries.map((e) => {
    const gap = hasGap(e);
    if (gap) midLine = false;
    const gapOnly = gap && !e.text && !e.event;
    if (gapOnly || entryInstance(e) !== 0) return e;
    const shown = ownEntryShown(e, midLine, (text, mid) => addLinePrefixes(text, '[#0] ', mid));
    midLine = shown.midLine;
    return shown.entry;
  });
}

/**
 * What instance k's tab shows: instance k's own output and generation markers,
 * with the `[#k] ` prefix the agent put at the start of each of its lines
 * removed, plus every gap in the history as a gap alone. `own` counts the
 * entries that are instance k's, which is what tells "this instance has printed
 * nothing yet" apart from a tab that shows only gaps.
 *
 * An entry is a CHUNK of output, not a line: a chunk can continue the line the
 * previous chunk of the same generation began, and the agent prefixes a line
 * only once, when its first byte arrives. So the line state is tracked per
 * instance here -- a chunk after one that did not end in a newline continues
 * that line and keeps its first bytes -- and a generation marker opens a new
 * line, as the agent's prefixing does. Where the state is not known -- at the
 * start of the window, and after a gap, whose lost bytes may have ended the
 * line -- a chunk is read as opening a line. That leaves one ambiguity, and it
 * cannot be resolved from the text: process output that itself begins with the
 * exact prefix right where an unknown chunk boundary falls loses those bytes in
 * this tab ("All" still shows them as written).
 */
function instanceView(
  entries: readonly RuntimeLogEntry[],
  k: number,
): { entries: RuntimeLogEntry[]; own: number } {
  // Instance 0's lines carry no prefix from the agent, so nothing is removed.
  const prefix = `[#${k}] `;
  const rewrite =
    k > 0
      ? (text: string, mid: boolean) => stripLinePrefixes(text, prefix, mid)
      : (text: string) => text;
  const out: RuntimeLogEntry[] = [];
  let own = 0;
  let midLine = false;
  for (const e of entries) {
    const gap = hasGap(e);
    if (gap) midLine = false;
    // An entry that is only a gap names no instance, which on the wire reads
    // as instance 0 -- but a gap is nobody's output, so it is shown in every
    // tab and counted as none, instance 0's no more than any other's.
    const gapOnly = gap && !e.text && !e.event;
    if (gapOnly || entryInstance(e) !== k) {
      if (gap) out.push({ dropped_bytes: e.dropped_bytes });
      continue;
    }
    own++;
    const shown = ownEntryShown(e, midLine, rewrite);
    out.push(shown.entry);
    midLine = shown.midLine;
  }
  return { entries: out, own };
}

/**
 * One of an instance's own entries as a tab shows it, and that instance's line
 * state after it: a generation marker opens a new line, an empty chunk leaves
 * the state as it was, and a text chunk is passed through `rewrite` (which
 * removes or adds the line prefixes) and leaves the line open unless it ends in
 * a newline.
 */
function ownEntryShown(
  e: RuntimeLogEntry,
  midLine: boolean,
  rewrite: (text: string, midLine: boolean) => string,
): { entry: RuntimeLogEntry; midLine: boolean } {
  if (e.event) return { entry: e, midLine: false };
  const text = e.text ?? '';
  if (text === '') return { entry: e, midLine };
  const rewritten = rewrite(text, midLine);
  const entry = rewritten === text ? e : { ...e, text: rewritten };
  return { entry, midLine: !text.endsWith('\n') };
}

/**
 * One rendered line: process or file output, or a boundary marker together
 * with the resolved command it carries, or a file marker, or a gap notice. A
 * file STATUS event renders nothing here: it is stated above the log instead
 * (see `fileStatus`).
 */
function LogLine({ entry, t }: Readonly<{ entry: RuntimeLogEntry; t: Translation }>) {
  const gap =
    entry.dropped_bytes && entry.dropped_bytes > 0 ? (
      <Box component="span" sx={{ color: 'warning.main', fontStyle: 'italic' }}>
        {`\n${t.runtimeLogsDropped(entry.dropped_bytes)}\n`}
      </Box>
    ) : null;

  // A marker inside a file's history. It carries no pid and no command: a file
  // is not a process.
  const fileMarker = fileMarkerLabel(entry.event, t);
  if (fileMarker !== null) {
    return (
      <>
        {gap}
        <Box
          component="span"
          sx={{ color: 'text.secondary', fontStyle: 'italic', display: 'block', my: 0.5 }}
        >
          {`── ${fileMarker} ──`}
        </Box>
      </>
    );
  }

  // A boundary between two runs of the same spec. The wording is OURS: the
  // backend allow-lists the event kind to a closed set precisely so that what
  // an operator reads as a portal statement cannot be text an agent chose.
  if (entry.event === 'started' || entry.event === 'exited' || entry.event === 'start_failed') {
    let label: string;
    if (entry.event === 'started') label = t.runtimeLogsProcessStarted(entry.pid ?? 0);
    else if (entry.event === 'exited') label = t.runtimeLogsProcessExited(entry.exit_code ?? 0);
    // No pid, and there never will be one: the exec itself failed.
    else label = t.runtimeLogsProcessStartFailed;
    return (
      <>
        {gap}
        <Box sx={{ my: 0.5 }}>
          <Box
            component="span"
            sx={{ color: 'text.secondary', fontStyle: 'italic', display: 'block' }}
          >
            {`── ${label} ──`}
          </Box>
          {entry.command && (
            // Keyed on the MARKER, not on the row. `InlineCommand` holds the
            // one piece of state in this subtree (its collapse toggle), and the
            // rendered window SLIDES past maxRenderedEntries, so a new
            // generation's marker can land on the row an operator had expanded
            // and inherit "expanded" from it -- reproduced with a full window:
            // the operator's block collapsed and the new generation's opened by
            // itself. Changing the key here remounts the toggle whenever the
            // generation beneath it changes, so "expanded or not" can no longer
            // cross between generations. (The command TEXT never could: it is a
            // prop of this marker.) A marker's identity is its timestamp and
            // pid -- `start_failed` has no pid and never will, which is why the
            // timestamp carries the pair.
            <InlineCommand
              key={`${entry.at ?? ''}#${entry.pid ?? 0}`}
              command={entry.command}
              startFailed={entry.event === 'start_failed'}
              t={t}
            />
          )}
        </Box>
      </>
    );
  }
  return (
    <>
      {gap}
      {entry.text}
    </>
  );
}

/**
 * One labelled block of the resolved command, rendered as monospace lines.
 *
 * `lines` is always a list, even for the single-valued fields, because that is
 * what makes an argument list readable: one argument per line, never joined
 * with spaces. Joining would be actively misleading here -- `--system-prompt`
 * followed by a sentence containing spaces is ONE argument, and a
 * space-separated rendering of it is a different command from the one that ran.
 * It is also the same shape the spec editor demands on input (one per line), so
 * the operator reads back what they wrote.
 */
function CommandField({
  label,
  lines,
  empty,
}: Readonly<{ label: string; lines: readonly string[]; empty?: string }>) {
  return (
    <Box sx={{ mb: 1 }}>
      <Typography variant="caption" color="text.secondary" sx={{ display: 'block' }}>
        {label}
      </Typography>
      {lines.length === 0 && empty !== undefined ? (
        <Typography variant="body2" color="text.secondary" sx={{ fontStyle: 'italic' }}>
          {empty}
        </Typography>
      ) : (
        <Box
          sx={{
            fontFamily: 'monospace',
            fontSize: '0.8rem',
            whiteSpace: 'pre',
            // Long paths and long arguments scroll inside their own block
            // rather than widening the dialog.
            overflowX: 'auto',
          }}
        >
          {lines.map((line, i) => (
            // The index is a legitimate key because each line is rendered as
            // STATELESS text -- no input, no ref, no focus target -- so there
            // is nothing an index shift could misbind. That, and NOT any
            // claim about the list being stable, is what is being relied on
            // here; it is also the property a future editor would break. Give
            // a line anything stateful (a per-line copy button, an expand
            // toggle, a selection) and the index stops being a valid key --
            // key on the line's own identity then, not on its position.
            <Box component="div" key={i}>
              {line}
            </Box>
          ))}
        </Box>
      )}
    </Box>
  );
}

/**
 * The resolved command of ONE generation, rendered inside that generation's
 * start-marker block.
 *
 * **Collapsed by default, and that is load-bearing rather than tidiness.** A
 * real command is thirty-odd lines once one-argument-per-line is respected, and
 * in a crash loop there is one per attempt -- rendered flat they would bury the
 * output, which is the very thing this view exists to show. The one exception
 * earns itself: a `start_failed` generation produced NO output at all, so its
 * command is the entire content there is and it opens expanded.
 *
 * Three further rules it must not break, each a way of lying to the operator:
 *
 *  1. **Withheld values are unmistakable, and the block says WHY in words.**
 *     A value resolved from `${AGENT_ENV:NAME}` is shown as that placeholder,
 *     not as bullets and never as the value: it cannot be mistaken for real
 *     text, and it names the variable the operator needs to check on the host.
 *     A file-mode agent additionally withholds every value its own spec sets in
 *     `env`, key intact, and that is a DIFFERENT reason with a different
 *     remedy -- `masked` and `env_redacted` are separate flags and get separate
 *     sentences. The sentences state their own scope too: masking covers the
 *     reported command, not the child's output, which streams into this very
 *     window with every placeholder already resolved.
 *  2. **A truncated list says it is truncated**, on the same reasoning as
 *     `dropped_bytes` in the output.
 *  3. **There is no copy button, deliberately.** Even fully unmasked this is
 *     not a runnable command line: `env` REPLACES the environment rather than
 *     adding to it, the port was ephemeral and is stale by now, and a
 *     `set_visible_devices` child renumbers its GPUs from zero. A copy button
 *     would promise reproduction and hand over a broken paste. The text is
 *     plain and selectable instead, which promises nothing.
 */
function InlineCommand({
  command,
  startFailed,
  t,
}: Readonly<{ command: RuntimeLogCommand; startFailed: boolean; t: Translation }>) {
  const [open, setOpen] = useState(startFailed);
  return (
    <Box sx={{ ml: 2, mb: 0.5 }}>
      <Button
        size="small"
        onClick={() => setOpen((v) => !v)}
        sx={{ textTransform: 'none', minWidth: 0, p: 0, fontSize: '0.75rem' }}
      >
        {`${open ? '▾' : '▸'} ${t.runtimeCommandTitle}`}
      </Button>
      <Collapse in={open} timeout={0} unmountOnExit>
        <Box sx={{ mt: 0.5 }}>
          {startFailed && (
            <Alert severity="warning" sx={{ mb: 1 }}>
              {t.runtimeCommandStartFailedHint}
            </Alert>
          )}
          <Typography variant="body2" color="text.secondary" sx={{ mb: 1 }}>
            {t.runtimeCommandIntro}
          </Typography>
          {/* One sentence per withholding REASON, and both may apply at once.
              `masked` is a ${AGENT_ENV:NAME} span replaced by its own
              placeholder -- there is a host variable to go and check.
              `env_redacted` is a file-mode agent withholding the values its
              local document sets -- there is no placeholder and nothing to look
              up. They were one flag under one sentence, and a file-mode
              operator then read `HF_TOKEN=•••` under an explanation of
              placeholder syntax. */}
          {command.masked && (
            <Alert severity="info" sx={{ mb: 1 }}>
              {t.runtimeCommandMasked}
            </Alert>
          )}
          {command.env_redacted && (
            <Alert severity="info" sx={{ mb: 1 }}>
              {t.runtimeCommandEnvRedacted}
            </Alert>
          )}
          {command.truncated && (
            <Alert severity="warning" sx={{ mb: 1 }}>
              {t.runtimeCommandTruncated}
            </Alert>
          )}
          <CommandField
            label={t.runtimeCommandBinary}
            lines={command.binary ? [command.binary] : []}
            empty={t.runtimeCommandUnknown}
          />
          <CommandField
            label={t.runtimeCommandArgs}
            lines={command.args ?? []}
            empty={t.runtimeCommandArgsNone}
          />
          <CommandField
            label={t.runtimeCommandWorkDir}
            lines={command.work_dir ? [command.work_dir] : []}
            empty={t.runtimeCommandWorkDirInherited}
          />
          <CommandField
            label={t.runtimeCommandEnv}
            lines={command.env ?? []}
            empty={t.runtimeCommandEnvNone}
          />
        </Box>
      </Collapse>
    </Box>
  );
}

/**
 * Whether the visible history begins with OUTPUT rather than with a
 * generation's opening marker -- which means that generation's marker, and with
 * it the only copy of its resolved command, is not on screen.
 *
 * It has to be stated rather than left blank, for the same reason
 * `dropped_bytes` does: missing information must never read as "there was
 * none". An operator who sees no command anywhere would otherwise conclude the
 * agent does not report one.
 *
 * It says only THAT the marker is missing, never WHY, because two different
 * things cause it and the caller is the one that knows which: the agent's own
 * bounded buffer evicted it (gone -- reopening cannot bring it back), or this
 * view's display cap trimmed the head (not gone -- the agent still holds it,
 * and reopening reloads it). Those need opposite sentences, and rendering the
 * "the agent no longer retains it" one for a local trim put it directly under
 * the trimmed notice, which promises the opposite in the same breath.
 */
function opensWithoutACommand(entries: readonly RuntimeLogEntry[]): boolean {
  const firstOpening = entries.findIndex(
    (e) => e.event === 'started' || e.event === 'start_failed',
  );
  const firstOutput = entries.findIndex((e) => (e.text ?? '') !== '');
  return firstOutput >= 0 && (firstOpening < 0 || firstOpening > firstOutput);
}

export function RuntimeLogView({
  open,
  onClose,
  api,
  t,
  serverId,
  specId,
  title,
  allInstances = false,
  instances = [],
  initialTab = 'all',
  logFiles = noLogFiles,
  logFilesSupported = true,
}: Readonly<{
  open: boolean;
  onClose: () => void;
  // Narrowed to the one call this view makes, matching the app's
  // Pick<PortalApi, …> prop convention: a component that can only subscribe
  // cannot accidentally grow a write.
  api: Pick<PortalApi, 'subscribeRuntimeLogs'>;
  t: Translation;
  serverId: string;
  specId: string;
  title: string;
  // #340: the spec runs several instances, which all write into its one log
  // buffer -- instance k's lines prefixed [#k], instance 0's marked [#0] in
  // "All" -- so the view offers one tab per instance beside "All", and says
  // whose lines are whose.
  allInstances?: boolean;
  // The instance indexes the opening row's spec lists now. The view adds every
  // instance the stream names while it is open.
  instances?: readonly number[];
  // The tab the view opens on: the instance whose row the operator opened it
  // from, or "All". A view that opens without tabs opens on "All".
  initialTab?: LogTab;
  // #430: the names of the spec's declared log files, in declaration order.
  // Non-empty, they add the source row; empty, the view is as before.
  logFiles?: readonly string[];
  // Whether the server's agent declares `runtime_log_files`. An agent without
  // it never sends a file stream, so each file tab says so instead of staying
  // blank. Unknown counts as supported.
  logFilesSupported?: boolean;
}>) {
  // The tab every opening starts on. Whether the view shows tabs at that
  // moment depends on the props alone, because the instances the stream names
  // are forgotten on every opening. A view without tabs opens on "All", and
  // stays there when the stream later names a further instance.
  const openingTab: LogTab = showsTabs(allInstances, knownInstances(instances, noInstancesSeen))
    ? initialTab
    : 'all';
  const [windows, setWindows] = useState<StreamWindows>(noStreamWindows);
  // The streams the view keeps: the process output and the declared files.
  // A ref, synced by an effect, so a changed declaration does not re-subscribe.
  const declaredRef = useRef<ReadonlySet<string>>(new Set(['', ...logFiles]));
  // Every instance the stream has named since the dialog opened. Kept across a
  // scrollback: an instance that has left the retained history keeps its tab
  // while the dialog is open, so a tab never vanishes under the operator.
  const [seenInstances, setSeenInstances] = useState<ReadonlySet<number>>(() => new Set());
  const [tab, setTab] = useState<LogTab>(openingTab);
  // The selected source: '' is the process output, otherwise a declared log
  // file's name. Independent of `tab`: switching one keeps the other.
  const [source, setSource] = useState('');
  const [state, setState] = useState<RuntimeLogState | null>(null);
  // `scrollbackSeen` is what separates "the agent's buffer is empty" from
  // "nothing has arrived yet". The agent always sends a scrollback batch on
  // subscribe -- an EMPTY one when it has nothing retained -- so its arrival is
  // the moment those two stop being the same state.
  const [scrollbackSeen, setScrollbackSeen] = useState(false);
  const [connectionError, setConnectionError] = useState(false);
  // Follow the tail unless the operator has scrolled away from it. Reading old
  // output while new output arrives is a normal thing to do here, and yanking
  // the viewport away mid-read would make the view unusable for its own
  // purpose.
  const followRef = useRef(true);
  const boxRef = useRef<HTMLDivElement | null>(null);
  // Every tab filters the one log region below it, so each tab names that
  // region as the one it controls.
  const logId = useId();

  useEffect(() => {
    declaredRef.current = new Set(['', ...logFiles]);
  }, [logFiles]);

  useEffect(() => {
    if (!open) return undefined;
    setWindows(noStreamWindows);
    setState(null);
    setScrollbackSeen(false);
    setConnectionError(false);
    setSeenInstances(new Set());
    followRef.current = true;

    const onBatch = (batch: RuntimeLogBatch) => {
      setSeenInstances((prev) => withInstancesOf(prev, batch.entries));
      if (batch.scrollback) {
        // REPLACE, never append: a reconnect delivers a fresh scrollback and
        // appending it to what is on screen would duplicate the history. The
        // counter is replaced too, not zeroed -- a retained history longer than
        // the display cap loses its head to that same slice, and this path used
        // to drop it with no counter and no notice. Every stream's window is
        // replaced: the scrollback replays all of them.
        setScrollbackSeen(true);
        setWindows(appendToStreams(noStreamWindows, batch.entries, declaredRef.current));
        return;
      }
      setWindows((prev) => appendToStreams(prev, batch.entries, declaredRef.current));
    };

    return api.subscribeRuntimeLogs(serverId, specId, onBatch, setState, (status) =>
      setConnectionError(status === 'error'),
    );
  }, [api, open, serverId, specId]);

  // Every opening starts on the opening tab, whichever tab was selected when
  // the dialog last closed. Its own effect, because changing it must not
  // re-subscribe.
  useEffect(() => {
    if (open) setTab(openingTab);
  }, [open, openingTab]);

  // Every opening starts on the process output, as before log files existed.
  useEffect(() => {
    if (open) setSource('');
  }, [open]);

  const tabInstances = useMemo(
    () => knownInstances(instances, seenInstances),
    [instances, seenInstances],
  );
  const showTabs = showsTabs(allInstances, tabInstances);
  const activeTab: LogTab = showTabs ? tab : 'all';
  const showSources = logFiles.length > 0;
  // A source the spec no longer declares falls back to the process output.
  const activeSource = logFiles.includes(source) ? source : '';
  const fileSource = activeSource !== '';
  // The selected stream's own window: its entries, and its own trim counts.
  const win = windows.get(activeSource) ?? emptyLogWindow;
  const streamEntries = win.entries;
  const shown = useMemo(
    () =>
      activeTab === 'all'
        ? {
            entries: showTabs ? allView(streamEntries) : streamEntries,
            own: streamEntries.length,
          }
        : instanceView(streamEntries, activeTab),
    [streamEntries, activeTab, showTabs],
  );

  useEffect(() => {
    if (!followRef.current) return;
    const el = boxRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [shown.entries]);

  const entries = shown.entries;
  // Per source: the agent keeps one ring per stream, so "nothing retained" is
  // a fact about the selected one. Without log files it is the whole history.
  const hasOutput = streamEntries.length > 0;
  // A file has no generation marker and no command, so only the process
  // output can be missing one.
  const commandGap = !fileSource && opensWithoutACommand(entries);
  const fileStatusText: Readonly<Record<FileStatus, string>> = {
    waiting: t.runtimeLogsFileWaiting,
    not_permitted: t.runtimeLogsFileNotPermitted,
    binary: t.runtimeLogsFileBinary,
    missing: t.runtimeLogsFileMissing,
  };
  const notice = (() => {
    if (connectionError) return { severity: 'warning' as const, text: t.runtimeLogsDisconnected };
    if (state === 'offline') return { severity: 'warning' as const, text: t.runtimeLogsOffline };
    if (state === 'unsupported')
      return { severity: 'warning' as const, text: t.runtimeLogsUnsupported };
    // An agent without runtime_log_files never sends this stream, whatever
    // else its history holds: say so before any sentence about the history.
    if (fileSource && !logFilesSupported)
      return { severity: 'warning' as const, text: t.runtimeLogsFileUnsupported };
    // A connected, capable agent that delivered an EMPTY history: say so, or
    // the blank area reads as "the process printed nothing". (An empty FILE
    // stream gets its own sentence below.)
    if (!fileSource && state === 'streaming' && scrollbackSeen && !hasOutput)
      return { severity: 'info' as const, text: t.runtimeLogsEmptyBuffer };
    if (!scrollbackSeen && !hasOutput)
      return { severity: 'info' as const, text: t.runtimeLogsWaiting };
    // The history has output, just none of this instance's: say so, or the
    // tab reads as "this instance prints nothing".
    if (activeTab !== 'all' && shown.own === 0)
      return { severity: 'info' as const, text: t.runtimeLogsInstanceEmpty(activeTab) };
    if (!fileSource) return null;
    // A file tab: its latest status (under "All" with instance tabs, the one
    // every instance shares), else -- with no line and no gap shown -- that
    // the file has written nothing yet.
    const status =
      activeTab === 'all' && showTabs
        ? sharedFileStatus(entries, tabInstances)
        : fileStatus(entries);
    if (status !== null) return { severity: 'info' as const, text: fileStatusText[status] };
    if (!entries.some((e) => (e.text ?? '') !== '' || hasGap(e)))
      return { severity: 'info' as const, text: t.runtimeLogsFileEmpty };
    return null;
  })();

  const logLabel = fileSource ? t.runtimeLogs : t.runtimeLogsTitle;

  return (
    <Dialog open={open} onClose={onClose} fullWidth maxWidth="lg">
      {/* "Process output" names the process stream; a file tab gets the
          neutral "Logs" instead. */}
      <DialogTitle>{`${logLabel} — ${title}`}</DialogTitle>
      <DialogContent>
        {showTabs && (
          <Tabs
            value={activeTab}
            onChange={(_event, value: LogTab) => {
              // A different tab is a different view: start it at its tail.
              followRef.current = true;
              setTab(value);
            }}
            variant="scrollable"
            scrollButtons="auto"
            aria-label={t.runtimeLogsTabsLabel}
            sx={{ mb: 1, borderBottom: 1, borderColor: 'divider' }}
          >
            <Tab label={t.runtimeLogsTabAll} value="all" aria-controls={logId} />
            {tabInstances.map((k) => (
              <Tab key={k} label={t.runtimeLogsTabInstance(k)} value={k} aria-controls={logId} />
            ))}
          </Tabs>
        )}
        {showSources && (
          <Tabs
            value={activeSource}
            onChange={(_event, value: string) => {
              // A different source is a different view: start it at its tail.
              followRef.current = true;
              setSource(value);
            }}
            variant="scrollable"
            scrollButtons="auto"
            aria-label={t.runtimeLogsSourceLabel}
            sx={{ mb: 1, borderBottom: 1, borderColor: 'divider' }}
          >
            <Tab label={t.runtimeLogsSourceProcess} value="" aria-controls={logId} />
            {logFiles.map((name) => (
              <Tab key={name} label={name} value={name} aria-controls={logId} />
            ))}
          </Tabs>
        )}
        <Typography variant="body2" color="text.secondary" sx={{ mb: 1 }}>
          {fileSource ? t.runtimeLogsFileIntro(activeSource) : t.runtimeLogsIntro}
        </Typography>
        {showTabs && (
          <Typography variant="body2" color="text.secondary" sx={{ mb: 1 }}>
            {t.runtimeLogsAllInstances}
          </Typography>
        )}
        {notice && (
          <Alert severity={notice.severity} sx={{ mb: 1 }}>
            {notice.text}
          </Alert>
        )}
        {win.trimmedBytes > 0 && (
          <Alert severity="warning" sx={{ mb: 1 }}>
            {t.runtimeLogsTrimmed(win.trimmedBytes)}
          </Alert>
        )}
        {commandGap && (
          <Alert severity="info" sx={{ mb: 1 }}>
            {/* Two causes, two sentences. A head lost UPSTREAM is gone: the
                agent sent its retained history whole and it began with output,
                so reopening cannot bring the marker back. A head trimmed HERE
                is not: the agent still holds it, which is what the trimmed
                notice directly above already promises ("reopen to reload the
                agent's retained history in full"). Saying "the agent no longer
                retains it" beside that promise put two alerts on screen that
                answered each other. */}
            {win.trimmedEntries > 0 ? t.runtimeCommandTrimmedHere : t.runtimeCommandNotRetained}
          </Alert>
        )}
        <Box
          ref={boxRef}
          id={logId}
          role="log"
          aria-label={logLabel}
          aria-live="polite"
          onScroll={(e) => {
            const el = e.currentTarget;
            followRef.current = el.scrollHeight - el.scrollTop - el.clientHeight < 24;
          }}
          sx={{
            fontFamily: 'monospace',
            fontSize: '0.8rem',
            whiteSpace: 'pre-wrap',
            // The one place the page may scroll sideways is inside this box:
            // a model server's output is full of long unbroken lines.
            overflowWrap: 'anywhere',
            overflowY: 'auto',
            overflowX: 'auto',
            maxHeight: '60vh',
            minHeight: '20vh',
            p: 1,
            border: 1,
            borderColor: 'divider',
            borderRadius: 1,
            bgcolor: 'action.hover',
          }}
        >
          {entries.map((entry, i) => (
            // The index is a legitimate key for the row's CONTENT, and NOT
            // because the indices are stable -- they are not. Past
            // maxRenderedEntries the append path above trims from the FRONT,
            // so the rendered window SLIDES and every surviving row's index
            // moves (and a scrollback replaces the list outright). What makes
            // the key safe is that `LogLine` holds no state of its own -- no
            // useState, no useRef, no input, no focus target -- so nothing the
            // operator typed, selected or focused can be misbound by a shift.
            //
            // `InlineCommand` DOES hold state (its collapse toggle), and that
            // is why it carries a key of its OWN, on the marker it describes --
            // see LogLine. Without it, a marker landing on an expanded row's
            // index inherited that toggle: reproduced with a full 4000-entry
            // window, one operator-expanded command and one new generation, the
            // new marker rendered expanded. What remains, and is accepted, is
            // that the operator's own expansion does not survive the slide --
            // the row it moves to is a fresh mount either way. The command TEXT
            // always comes from that marker's own props, so no generation is
            // ever shown another's command.
            //
            // So statelessness, not index stability, is the thing to check
            // before adding anything to a row. A per-row copy button, a
            // selection, or any focusable control makes the index key wrong in
            // a way that is no longer cosmetic -- and RuntimeLogEntry carries
            // no id, so keying on identity means MINTING one (a sequence
            // number assigned in onBatch), not picking an existing field.
            <LogLine key={i} entry={entry} t={t} />
          ))}
        </Box>
      </DialogContent>
      <DialogActions>
        <Button onClick={onClose}>{t.captureClose}</Button>
      </DialogActions>
    </Dialog>
  );
}
