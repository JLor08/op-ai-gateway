// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useEffect, useRef, useState } from 'react';
import {
  Alert,
  Box,
  Button,
  Collapse,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
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
 *  3. **Every gap is visible.** `dropped_bytes` is rendered wherever it
 *     appears, and this component's own display cap produces the same kind of
 *     marker when it trims. A gap shown as silence would be a lie about what
 *     the process printed.
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
 */

/**
 * How many entries the browser keeps. The agent's own buffer is the history
 * (megabytes, operator-sized); this is only what the DOM holds, so it is sized
 * for rendering cost rather than for retention. Trimming past it is reported,
 * never silent -- see trimmedBytes.
 */
const maxRenderedEntries = 4000;

/**
 * One rendered line: process output, or a boundary marker together with the
 * resolved command it carries, or a gap notice.
 */
function LogLine({ entry, t }: Readonly<{ entry: RuntimeLogEntry; t: Translation }>) {
  const gap =
    entry.dropped_bytes && entry.dropped_bytes > 0 ? (
      <Box component="span" sx={{ color: 'warning.main', fontStyle: 'italic' }}>
        {`\n${t.runtimeLogsDropped(entry.dropped_bytes)}\n`}
      </Box>
    ) : null;

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
            <InlineCommand
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
 *  1. **Masked values are unmistakable.** A value resolved from
 *     `${AGENT_ENV:NAME}` is shown as that placeholder, not as bullets and
 *     never as the value: it cannot be mistaken for real text, and it names the
 *     variable the operator needs to check on the host. When anything is
 *     masked, the block says so in words too.
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
          {command.masked && (
            <Alert severity="info" sx={{ mb: 1 }}>
              {t.runtimeCommandMasked}
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
 * Whether the visible history begins with OUTPUT rather than with a generation's
 * opening marker -- which means that generation's marker, and with it the only
 * copy of its resolved command, has been evicted from the agent's bounded
 * buffer (or trimmed by this view's own display cap).
 *
 * It has to be stated rather than left blank, for the same reason
 * `dropped_bytes` does: missing information must never read as "there was
 * none". An operator who sees no command anywhere would otherwise conclude the
 * agent does not report one.
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
}>) {
  const [entries, setEntries] = useState<RuntimeLogEntry[]>([]);
  const [trimmedBytes, setTrimmedBytes] = useState(0);
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

  useEffect(() => {
    if (!open) return undefined;
    setEntries([]);
    setTrimmedBytes(0);
    setState(null);
    setScrollbackSeen(false);
    setConnectionError(false);
    followRef.current = true;

    const onBatch = (batch: RuntimeLogBatch) => {
      if (batch.scrollback) {
        // REPLACE, never append: a reconnect delivers a fresh scrollback and
        // appending it to what is on screen would duplicate the history.
        setScrollbackSeen(true);
        setTrimmedBytes(0);
        setEntries(batch.entries.slice(-maxRenderedEntries));
        return;
      }
      setEntries((prev) => {
        const next = [...prev, ...batch.entries];
        if (next.length <= maxRenderedEntries) return next;
        const cut = next.slice(0, next.length - maxRenderedEntries);
        const lost = cut.reduce((sum, e) => sum + (e.text?.length ?? 0), 0);
        if (lost > 0) setTrimmedBytes((n) => n + lost);
        return next.slice(-maxRenderedEntries);
      });
    };

    return api.subscribeRuntimeLogs(serverId, specId, onBatch, setState, (status) =>
      setConnectionError(status === 'error'),
    );
  }, [api, open, serverId, specId]);

  useEffect(() => {
    if (!followRef.current) return;
    const el = boxRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, [entries]);

  const hasOutput = entries.length > 0;
  const commandGap = opensWithoutACommand(entries);
  const notice = (() => {
    if (connectionError) return { severity: 'warning' as const, text: t.runtimeLogsDisconnected };
    if (state === 'offline') return { severity: 'warning' as const, text: t.runtimeLogsOffline };
    if (state === 'unsupported')
      return { severity: 'warning' as const, text: t.runtimeLogsUnsupported };
    // A connected, capable agent that delivered an EMPTY history: say so, or
    // the blank area reads as "the process printed nothing".
    if (state === 'streaming' && scrollbackSeen && !hasOutput)
      return { severity: 'info' as const, text: t.runtimeLogsEmptyBuffer };
    if (!scrollbackSeen && !hasOutput)
      return { severity: 'info' as const, text: t.runtimeLogsWaiting };
    return null;
  })();

  return (
    <Dialog open={open} onClose={onClose} fullWidth maxWidth="lg">
      <DialogTitle>{`${t.runtimeLogsTitle} — ${title}`}</DialogTitle>
      <DialogContent>
        <Typography variant="body2" color="text.secondary" sx={{ mb: 1 }}>
          {t.runtimeLogsIntro}
        </Typography>
        {notice && (
          <Alert severity={notice.severity} sx={{ mb: 1 }}>
            {notice.text}
          </Alert>
        )}
        {trimmedBytes > 0 && (
          <Alert severity="warning" sx={{ mb: 1 }}>
            {t.runtimeLogsTrimmed(trimmedBytes)}
          </Alert>
        )}
        {commandGap && (
          <Alert severity="info" sx={{ mb: 1 }}>
            {t.runtimeCommandNotRetained}
          </Alert>
        )}
        <Box
          ref={boxRef}
          role="log"
          aria-label={t.runtimeLogsTitle}
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
            // One consequence is real, known and cosmetic: `InlineCommand`
            // DOES hold state (its collapse toggle), so a marker landing on an
            // expanded row's index inherits that toggle. Reproduced with a
            // full 4000-entry window, one operator-expanded command and one
            // new generation: the new marker rendered expanded and the old one
            // collapsed. The command TEXT always comes from that marker's own
            // props, so no generation is ever shown another's command -- only
            // "expanded or not" can be wrong.
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
