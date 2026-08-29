// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useCallback, useEffect, useMemo, useRef, useState, type SubmitEvent } from 'react';
import {
  Alert,
  Box,
  Button,
  Checkbox,
  Chip,
  FormControlLabel,
  IconButton,
  Tab,
  Tabs,
  Tooltip,
  Typography,
} from '@mui/material';
import AddIcon from '@mui/icons-material/Add';
import EditIcon from '@mui/icons-material/Edit';
import DeleteIcon from '@mui/icons-material/Delete';
import BlockIcon from '@mui/icons-material/Block';
import CheckCircleIcon from '@mui/icons-material/CheckCircle';
import WarningAmberIcon from '@mui/icons-material/WarningAmber';
import PlayArrowIcon from '@mui/icons-material/PlayArrow';
import StopIcon from '@mui/icons-material/Stop';
import ClearIcon from '@mui/icons-material/Clear';
import ReplayIcon from '@mui/icons-material/Replay';
import ArticleIcon from '@mui/icons-material/Article';
import type {
  ApplicationStatus,
  GPUBudget,
  HardwareGPU,
  PortalApplication,
  PortalModelMapping,
  PortalServer,
  PutRuntimeSpecRequest,
  RuntimeError,
  RuntimeSpec,
  RuntimeSpecGPU,
  RuntimeStatus,
  UpdateMappingRequest,
} from '../api';
import type { Translation, PortalApi, MessageKey, BadgeStatus } from './shared/types';
import { formatPortalError, formatMetric, formatDate } from './shared/format';
import { useResource } from './shared/useResource';
import { ResourceFallback, resourceState } from './shared/ResourceFallback';
import { useLatestFetch } from './shared/useLatestFetch';
import { StatusChip } from './shared/StatusChip';
import { Panel } from './shared/Panel';
import { Field } from './shared/Field';
import { SelectField } from './shared/SelectField';
import { ConfirmDialog } from './shared/ConfirmDialog';
import { Breadcrumbs, type BreadcrumbItem } from './shared/Breadcrumbs';
import { ListTable, listTableLabels, type ListColumn } from './shared/ListTable';
import { mappingColumns } from './shared/mappingColumns';
import type { RowAction } from './shared/RowActionsMenu';
import { useToast } from './shared/ToastProvider';
import { MappingForm, type MappingFormValues } from './MappingForm';
import { RuntimeMatrix, type RuntimeMatrixSpec } from './RuntimeMatrix';
import { RuntimeLogView } from './RuntimeLogView';

// Area 1 (Task 20) is "Launch specs"; areas 2-3 (this task, Task 21) are the
// co-residency matrix and server limits. Area 4 (Task 22, "Live status") is
// still a stub rendered from the same tab strip so the whole section's
// navigation is visible/testable now.
type Tab = 'mapping' | 'specs' | 'matrix' | 'limits' | 'status';

type SpecMode = 'list' | 'create' | { kind: 'edit'; mapping: PortalModelMapping };

/**
 * Upper bound on the restart sequence's "wait for this spec to report
 * `stopped`" step. There is no restart endpoint (design §10.1: every action
 * is state-shaped), so a restart is force_stopped -> await `stopped` ->
 * clear the override; a wedged child that never stops must surface a
 * timeout instead of spinning forever.
 *
 * Derived from the SLOWEST legitimate path, not guessed: a POST-transport
 * agent picks the changed desired state up on its own
 * `runtimePollInterval` (60 s, server-agent/internal/agent/agent.go), then
 * the manager's drain-stop is bounded by `drainGrace` (10 s) + `killGrace`
 * (5 s) (server-agent/internal/runtime/manager.go), then the new state
 * reaches us on the 1 s sample: 76 s. Rounded up to 120 s so an ordinary
 * slow stop is never reported as a failure -- and 120 s is also exactly what
 * the OTHER slow legitimate path needs: a `backoff` row's own crash-backoff
 * timer is capped at `backoffCap` (60 s, same file) and only then does the
 * poll interval's 60 s apply, i.e. 60 + 60 lands precisely on this bound. A
 * WS-connected agent gets the push immediately and normally finishes in a
 * couple of seconds.
 */
export const RESTART_STOP_TIMEOUT_MS = 120_000;

/**
 * How long the awaited spec may be ABSENT from the status stream before the
 * restart sequence treats it as deleted. Absence is not immediately fatal:
 * the gateway keeps runtime status in volatile RAM (design §7), so a gateway
 * restart empties the list and the next sample (<= 1 s) refills it. Only a
 * sustained absence means the spec is genuinely gone.
 */
export const RESTART_VANISH_GRACE_MS = 8_000;

/**
 * Upper bound on a SINGLE admin_state PUT settling. `request()` in
 * api/transport.ts has no AbortController, so nothing else ever gives up on a
 * request: a PUT that never settles would otherwise leave `overrideBusy` (or
 * the restart sequence's `clearing` phase) set for the life of the page,
 * disabling every action on every row with no escape but a reload.
 *
 * Deliberately much shorter than RESTART_STOP_TIMEOUT_MS: that one bounds an
 * agent-side process lifecycle, this one bounds a gateway HTTP round trip
 * writing one document.
 */
export const OVERRIDE_WRITE_TIMEOUT_MS = 30_000;

/**
 * A render-only identity for one row of the two editable row lists (a spec's
 * GPU rows and a server's GPU budgets).
 *
 * Both lists support MID-LIST removal (`rows.filter((_, i) => i !== idx)`), and
 * an array index is not an identity there: deleting row n shifts every later
 * row down, so a `key={idx}` makes React reconcile row n+1's data onto row n's
 * element and destroy the LAST element rather than the deleted one. Everything
 * the DOM node owns rather than the props then lands on the wrong row -- focus
 * and caret above all, which is what an operator mid-edit actually notices
 * (Sonar typescript:S6479). The `index` field cannot serve as the key either:
 * it is operator-editable and deliberately allowed to collide while two rows
 * are being swapped (see `updateGpuRow`).
 *
 * Process-local and never sent anywhere: the PUT bodies carry `index` /
 * `budget_mb` only, so this counter has no meaning outside one page load.
 */
let nextRowKey = 0;
function makeRowKey(): string {
  nextRowKey += 1;
  return `row-${nextRowKey}`;
}

type GpuRow = {
  rowKey: string;
  index: number;
  vramEstimateMb: number;
  vramMeasuredMb: number;
};

// The warning codes RuntimeWarnings emits today (see
// gateway/backend/internal/portal/service_runtime.go); an unmapped future
// code falls back to its raw wire value rather than a misleading label.
const runtimeWarningLabelByCode: Record<string, MessageKey> = {
  timeout_ms_below_startup_timeout: 'runtimeTimeoutWarning',
  binary_path_os_mismatch: 'runtimeBinaryPathOsMismatchWarning',
};

// admin_state's three valid wire values (service_runtime.go): "" (no
// override), "force_running", "force_stopped".
const adminStateOptions: { value: string; labelKey: MessageKey }[] = [
  { value: '', labelKey: 'runtimeClearOverride' },
  { value: 'force_running', labelKey: 'runtimeForceStart' },
  { value: 'force_stopped', labelKey: 'runtimeForceStop' },
];

// A brand-new/never-configured spec is `configured: false` with every other
// field at its zero value (RuntimeSpec's own doc comment) -- reproduced here
// so a delete or an unloaded row can be rendered/edited without waiting on a
// network round trip.
function emptySpec(mappingId: string): RuntimeSpec {
  return {
    configured: false,
    mapping_id: mappingId,
    enabled: false,
    binary: '',
    args: [],
    env: {},
    work_dir: '',
    listen_port: 0,
    health_path: '',
    health_timeout_seconds: 0,
    startup_timeout_seconds: 0,
    idle_timeout_seconds: 0,
    admission_wait_timeout_seconds: 0,
    pinned: false,
    admin_state: '',
    vram_locked: false,
    set_visible_devices: false,
    gpus: [],
  };
}

// The nine RuntimeState wire values (server-agent/internal/runtime/types.go)
// mapped to their labels. NOTE the deliberate name/value mismatch on
// `pending_vram_unknown`: the enum value is the long one, the i18n key is the
// shorter `runtimeStatePendingVram`. Neither is renamed here -- this map is
// exactly the place where the two vocabularies meet.
const runtimeStateLabelByValue: Record<string, MessageKey> = {
  stopped: 'runtimeStateStopped',
  starting: 'runtimeStateStarting',
  running: 'runtimeStateRunning',
  draining: 'runtimeStateDraining',
  backoff: 'runtimeStateBackoff',
  start_failed: 'runtimeStateStartFailed',
  crashed: 'runtimeStateCrashed',
  pending_vram_unknown: 'runtimeStatePendingVram',
  not_permitted: 'runtimeStateNotPermitted',
};

// A state this portal build does not know (a newer agent) renders its raw wire
// value rather than a misleading label -- the same forward-compat fallback
// runtimeWarningLabelByCode above uses.
function runtimeStateLabel(state: string, t: Translation): string {
  const key = runtimeStateLabelByValue[state];
  return key ? t[key] : state;
}

// The file-mode `parse_error` codes, mapped to the sentence the operator
// reads. The field carries a CODE from a closed set and NEVER free text: the
// agent classifies its own load failure (`json_syntax`, `duplicate_spec_id`,
// `file_missing`, `read_failed`, plus an `unclassified` floor) and the gateway
// allow-lists the four real codes, degrading anything else to its own generic
// constant. That contract is THREE-SIDED and no compiler checks either seam --
// adding a code means the agent's set, the gateway's allow-list, and this map
// plus its two i18n keys.
//
// The last two are named for parsing only because the WIRE FIELD is: the agent
// raises them before any parsing happens, for a runtime.json that is not there
// or cannot be read. They exist because the agent used to report neither, so a
// file-mode server with a missing file rendered exactly like one with no specs
// configured -- the empty state and the absent state, told apart nowhere.
const runtimeParseErrorReasonByCode: Record<string, MessageKey> = {
  json_syntax: 'runtimeParseErrorJsonSyntax',
  duplicate_spec_id: 'runtimeParseErrorDuplicateSpecId',
  file_missing: 'runtimeParseErrorFileMissing',
  read_failed: 'runtimeParseErrorReadFailed',
};

// Unlike runtimeStateLabel above, an unrecognised value does NOT fall back to
// the raw wire value: the whole point of the closed set is that the operator
// is never shown an identifier. A code this build does not know -- a newer
// agent, the agent's `unclassified` floor, or the gateway's generic constant
// for a value outside its allow-list -- says so in a sentence instead.
function runtimeParseErrorReason(code: string, t: Translation): string {
  const key = runtimeParseErrorReasonByCode[code];
  return key ? t[key] : t.runtimeParseErrorUnknown;
}

// The portal has exactly THREE status colours: the theme defines
// success/watch/standby pairs and nothing else (theme/ThemeRoot.tsx), and
// statusClassByKey collapses `error`/`disabled`/`expired` onto standby
// (components/shared/status.ts) -- there is no red anywhere in the portal, and
// adding one is a portal-wide design change, not this screen's call. So the
// colour can only carry the three coarse facts it genuinely has (loaded /
// on its way / neither), and the LABEL carries the rest. `last_error` -- "the
// last load attempt failed" -- is not a state at all and gets its own column.
function runtimeStateBadge(state: string): BadgeStatus {
  if (state === 'running') return 'active';
  // Both are "waiting to be loaded", the user-visible "currently loading".
  if (state === 'starting' || state === 'pending_vram_unknown') return 'watch';
  return 'standby';
}

/**
 * The states a restart sequence can actually complete from.
 *
 * A restart is force_stopped -> await `stopped` -> clear the override, and on
 * the agent side that middle step only ever arrives when there is either a
 * live process to drain or a resting state `applyConfig` resets:
 *
 *  - `force_stopped` on a spec with no live process does NOTHING
 *    (server-agent/internal/runtime/manager.go:724-728: `if st.proc != nil {
 *    beginDrain } ; continue`), and
 *  - `applyConfig`'s changed-spec reset covers `start_failed`, `crashed` and
 *    `backoff` -- but DELIBERATELY excludes `not_permitted` and
 *    `pending_vram_unknown` (manager.go:676-698, the "I6" comment).
 *
 * So on `stopped`, `pending_vram_unknown` and `not_permitted` the wait can
 * never be satisfied: the UI would spin for the full RESTART_STOP_TIMEOUT_MS,
 * report a timeout, and leave force_stopped in force -- which leaves the model
 * admission-blocked (manager.go's ErrAdmissionBlocked) until a human clears it
 * by hand. A UI action would have made a model unavailable and reported it as
 * a timeout. `force_running` is the action that does something on those three,
 * and it is offered there.
 *
 * An UNKNOWN state (a newer agent than this portal build) is treated as
 * non-restartable for the same reason: it may well be another one the reset
 * excludes, and the cost of being wrong is asymmetric.
 */
const restartableStates = new Set([
  'running',
  'starting',
  'draining',
  'backoff',
  'start_failed',
  'crashed',
]);

// One restart sequence. There is no restart endpoint (design §10.1: every
// action is state-shaped), so a restart is a three-step UI sequence:
//   stopping -> the force_stopped PUT is in flight
//   waiting  -> the stream has to report this spec `stopped`
//   clearing -> the admin_state="" PUT is in flight
// `deadline` is absolute so the overall wait stays bounded even though the
// timer is re-armed on each phase change.
type RestartFlow = {
  specId: string;
  mappingId: string;
  phase: 'stopping' | 'waiting' | 'clearing';
  deadline: number;
  /**
   * Stream frame counter as of the moment the force_stopped write LANDED.
   * Only a `stopped` observation from a strictly LATER frame can complete the
   * sequence: `stopped` in a frame that predates our own write is a STATE the
   * process was already in for some unrelated reason (an idle stop, a drain
   * that was already running), never the TRANSITION this step waits for.
   * Without the watermark the sequence confirms a state and can therefore
   * "succeed" having started nothing at all.
   */
  waitFrom: number;
};

// One launch spec as read back from a file-mode agent's report. This is the
// defensively narrowed form of RuntimeReportContent.config, which is typed
// `unknown` on purpose: it is whatever the agent's local file contained.
type ReportSpec = {
  id: string;
  model: string;
  binary: string;
  args: string[];
  gpus: { index: number; vramMb: number }[];
  pinned: boolean;
  setVisibleDevices: boolean;
  idleTimeoutSeconds: number;
};

type ReportConfig = {
  maxProcesses: number | null;
  budgets: { index: number; budgetMb: number }[];
  specs: ReportSpec[];
  coresident: [string, string][];
  /** Something, at some level, did not have the shape we expected. */
  unrecognised: boolean;
};

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

// A spec entry with no usable identity cannot be rendered as a row at all
// (it would collide with every other identity-less entry as a React key and
// could not be addressed in the co-residency matrix) -- those are reported as
// unrecognised by the caller. Everything else degrades field by field.
function narrowReportSpec(value: unknown, flag: { unrecognised: boolean }): ReportSpec | null {
  if (!isRecord(value)) return null;
  if (typeof value.id !== 'string' || value.id === '') return null;
  const gpus: { index: number; vramMb: number }[] = [];
  if (Array.isArray(value.gpus)) {
    for (const gpu of value.gpus) {
      if (isRecord(gpu) && typeof gpu.index === 'number') {
        gpus.push({ index: gpu.index, vramMb: typeof gpu.vram_mb === 'number' ? gpu.vram_mb : 0 });
      } else {
        flag.unrecognised = true;
      }
    }
  } else if (value.gpus !== undefined && value.gpus !== null) {
    flag.unrecognised = true;
  }
  const args: string[] = [];
  if (Array.isArray(value.args)) {
    for (const arg of value.args) {
      if (typeof arg === 'string') args.push(arg);
      else flag.unrecognised = true;
    }
  } else if (value.args !== undefined && value.args !== null) {
    flag.unrecognised = true;
  }
  return {
    id: value.id,
    model: typeof value.model === 'string' && value.model !== '' ? value.model : value.id,
    binary: typeof value.binary === 'string' ? value.binary : '',
    args,
    gpus,
    pinned: value.pinned === true,
    setVisibleDevices: value.set_visible_devices === true,
    idleTimeoutSeconds:
      typeof value.idle_timeout_seconds === 'number' ? value.idle_timeout_seconds : 0,
  };
}

/**
 * Narrows an agent-reported runtime config (`unknown` by design) into
 * something renderable, checking `typeof`/`Array.isArray` at every level and
 * recording whether anything had to be dropped. A malformed agent-supplied
 * config must never blank or crash the admin screen, and must never be cast
 * into shape either: what parses is rendered, what does not is reported as
 * "unrecognised shape".
 *
 * A null/absent config is an EMPTY document, not a malformed one (the gateway
 * stores `{}` when the agent's config failed to sanitize at all -- see
 * sanitizeRuntimeReportConfig).
 */
function narrowReportConfig(config: unknown): ReportConfig {
  const out: ReportConfig = {
    maxProcesses: null,
    budgets: [],
    specs: [],
    coresident: [],
    unrecognised: false,
  };
  if (!isRecord(config)) {
    out.unrecognised = config !== undefined && config !== null;
    return out;
  }
  const maxProcesses = config.max_processes;
  if (typeof maxProcesses === 'number' && Number.isFinite(maxProcesses)) {
    out.maxProcesses = maxProcesses;
  } else if (maxProcesses !== undefined && maxProcesses !== null) {
    out.unrecognised = true;
  }
  const budgets = config.gpu_budgets;
  if (Array.isArray(budgets)) {
    for (const entry of budgets) {
      if (
        isRecord(entry) &&
        typeof entry.index === 'number' &&
        typeof entry.budget_mb === 'number'
      ) {
        out.budgets.push({ index: entry.index, budgetMb: entry.budget_mb });
      } else {
        out.unrecognised = true;
      }
    }
  } else if (budgets !== undefined && budgets !== null) {
    out.unrecognised = true;
  }
  const specs = config.specs;
  if (Array.isArray(specs)) {
    for (const entry of specs) {
      const spec = narrowReportSpec(entry, out);
      if (spec) out.specs.push(spec);
      else out.unrecognised = true;
    }
  } else if (specs !== undefined && specs !== null) {
    out.unrecognised = true;
  }
  const coresident = config.coresident;
  if (Array.isArray(coresident)) {
    for (const entry of coresident) {
      if (
        Array.isArray(entry) &&
        entry.length === 2 &&
        typeof entry[0] === 'string' &&
        typeof entry[1] === 'string'
      ) {
        out.coresident.push([entry[0], entry[1]]);
      } else {
        out.unrecognised = true;
      }
    }
  } else if (coresident !== undefined && coresident !== null) {
    out.unrecognised = true;
  }
  return out;
}

// The override actions' PUT body. putRuntimeSpec takes the ENTIRE spec
// (Omit<RuntimeSpec, 'configured' | 'id' | 'mapping_id'>), never a patch, so
// the body is built by spreading the ACTUAL loaded spec and replacing exactly
// one field. A synthesized or defaulted body would silently overwrite the
// operator's configured binary/args/env/gpus/timeouts on a single override
// click -- the same full-replace hazard as the co-residency and GPU-budget
// writes (task-21 review). The rest-spread is deliberate over an explicit
// field list: a field added to RuntimeSpec later carries through by itself
// instead of being silently dropped here.
function specBodyWithAdminState(spec: RuntimeSpec, adminState: string): PutRuntimeSpecRequest {
  const { configured, id, mapping_id, ...rest } = spec;
  return { ...rest, admin_state: adminState };
}

function basename(path: string): string {
  const parts = path.split(/[\\/]/);
  return parts[parts.length - 1] || path;
}

function formatGpus(gpus: RuntimeSpecGPU[]): string {
  return gpus.length === 0
    ? '—'
    : gpus.map((g) => `${g.index}: ${g.vram_estimate_mb} MB`).join(', ');
}

function renderBoolChip(active: boolean, label: string) {
  return active ? <Chip size="small" variant="outlined" label={label} /> : '–';
}

// One argument per line. Splitting on spaces would corrupt any argument that
// legitimately contains one (model server command lines routinely have
// these), so a textarea line IS the argument. Lines are preserved verbatim
// -- including an internally blank line and one carrying only whitespace --
// because this form's whole job is faithfully representing a command line:
// filtering "empty-looking" lines would make a load -> edit(no-op) -> save
// round trip silently rewrite the spec's args. The ONE exception is a
// single trailing blank line, which is a textarea artifact (the user's
// cursor sitting on a fresh last line, or a copy-pasted trailing newline),
// not a deliberate trailing empty argument.
function parseArgsText(text: string): string[] {
  if (text === '') return [];
  const lines = text.split('\n').map((line) => line.replace(/\r$/, ''));
  if (lines.length > 1 && lines[lines.length - 1] === '') {
    lines.pop();
  }
  return lines;
}

function formatArgsText(args: string[]): string {
  return args.join('\n');
}

/**
 * A whitespace-separated token shaped like a command-line FLAG: one or two
 * leading dashes followed by a LETTER.
 *
 * Deliberately narrower than "starts with a dash", because a legitimate
 * argument VALUE is routinely full of tokens that merely do:
 *   - a Jinja chat template's whitespace-control markers -- "{%- if x -%}"
 *     alone yields "-%}", and a real template yields a dozen of them;
 *   - a negative number or sentinel ("-1", "-inf") -- no letter follows;
 *   - a horizontal rule or a dash run inside a system-prompt value ("---").
 * Requiring a letter after the dashes leaves every one of those alone, which
 * is what keeps the check below off correct input.
 */
const flagTokenPattern = /^--?[A-Za-z]/;

/**
 * Whether ONE argument -- i.e. one line of the textarea -- looks like a whole
 * command line pasted onto it rather than a single argv element.
 *
 * The signal is deliberately NOT "contains whitespace": an argument may
 * legitimately contain some, and often must -- a Windows model path
 * ("C:\Program Files\models\x.gguf"), a chat template, a system prompt. What
 * a pasted command line has and a value does not is TWO OR MORE flag-shaped
 * tokens separated by whitespace ("--port 50395 --mmproj C:\...\mmproj.gguf"),
 * for which there is no reading as a single argv element. One flag plus a
 * value ("--port 50395") is deliberately NOT enough on its own: "--opt=a b"
 * and "-p some prompt text" are shapes a foreign CLI may genuinely define.
 */
function looksLikePastedCommandLine(arg: string): boolean {
  if (!/\s/.test(arg)) return false;
  return arg.split(/\s+/).filter((token) => flagTokenPattern.test(token)).length > 1;
}

// The port flag in the three spellings an operator writes it: alone (value on
// the next line), "--port=50395", and "--port 50395" squeezed onto one line.
// Only that EXACT name -- never "--rpc-port" or another "*-port", which name a
// DIFFERENT endpoint that a spec may legitimately pin to a fixed number.
const portFlagPattern = /^--?port(?:[=\s]+(\S+))?$/i;

// A bare, in-range TCP port: what a hard-coded `--port` value looks like, and
// what "${PORT}" deliberately is not.
function isLiteralPort(value: string): boolean {
  if (!/^\d{1,5}$/.test(value)) return false;
  const port = Number(value);
  return port > 0 && port <= 65535;
}

/**
 * The literal port a spec pins on its command line, or null.
 *
 * Scoped to a port-NAMING flag rather than "an argument that is a number":
 * "--ctx-size 32768", "-ngl 99" and "--threads 8" are all bare numbers in the
 * same range, so the number alone carries no signal at all.
 */
function findHardcodedPort(args: string[]): string | null {
  for (let i = 0; i < args.length; i += 1) {
    const match = portFlagPattern.exec(args[i].trim());
    if (!match) continue;
    const value = (match[1] ?? args[i + 1] ?? '').trim();
    if (isLiteralPort(value)) return value;
  }
  return null;
}

// A pasted command line is long; 60 characters is enough for the operator to
// recognise WHICH line the warning means without pushing the panel sideways.
function argExcerpt(arg: string): string {
  const trimmed = arg.trim();
  return trimmed.length > 60 ? `${trimmed.slice(0, 60)}…` : trimmed;
}

// Whitespace the operator cannot see, made visible. Applied ONLY to a
// leading/trailing run, never to the whole argument: a Windows path's internal
// spaces are correct and dotting them would bury the one space that is not.
function visualiseWhitespaceRun(run: string): string {
  return run.replace(/[\s\S]/g, (ch) => (ch === '\t' ? '→' : '·'));
}

// The offending argument as ONE display line: "3: ·--model·". The core keeps
// its real characters (so the operator can recognise the line) but is elided
// in the MIDDLE when long -- eliding the tail would drop the trailing marker,
// which is the entire thing being pointed at.
function whitespaceDetailLine(arg: string, lineNumber: number): string {
  const leadLength = arg.length - arg.trimStart().length;
  const tailLength = arg.length - arg.trimEnd().length;
  const core = arg.slice(leadLength, arg.length - tailLength);
  const shownCore =
    core.length > 48 ? `${core.slice(0, 24)}…${core.slice(core.length - 24)}` : core;
  const lead = visualiseWhitespaceRun(arg.slice(0, leadLength));
  const tail = visualiseWhitespaceRun(arg.slice(arg.length - tailLength));
  return `${lineNumber}: ${lead}${shownCore}${tail}`;
}

// A line the operator would read as empty but that IS an argument, made of
// whitespace. Rendered whole, since there is no core to keep.
function blankDetailLine(arg: string, lineNumber: number): string {
  return `${lineNumber}: ${visualiseWhitespaceRun(arg)}`;
}

type ArgsWarning = { key: string; message: string; detail?: string };

/**
 * The four argument shapes that are almost certainly a mistake, reported live
 * under the field as the operator types: a whole command line pasted into one
 * line, whitespace at an argument's edge, a line made only of whitespace, and a
 * literal port while the agent owns the port.
 *
 * These WARN and never block the save, unlike the placeholder mirror below --
 * and the difference is the point, not an oversight. `findPlaceholderViolation`
 * restates a rule the AGENT itself enforces: a spec that trips it provably
 * cannot start, so refusing it in the form forecloses nothing. These four are
 * guesses about INTENT over a field whose legitimate contents are arbitrary
 * strings from a foreign program's CLI. A heuristic that refuses is a wall with
 * no way around it for the one operator whose legitimate value trips it (a
 * system-prompt value quoting two flags would), and this form has no "save
 * anyway". A live warning costs that operator one ignored sentence, and still
 * reaches the operator who pasted a command line -- at paste time, which is
 * earlier than a submit-time refusal could manage.
 */
function collectArgsWarnings(args: string[], listenPort: number, t: Translation): ArgsWarning[] {
  const warnings: ArgsWarning[] = [];
  const pasted = args.find(looksLikePastedCommandLine);
  if (pasted !== undefined) {
    warnings.push({
      key: 'pasted-command-line',
      message: `${t.runtimeSpecArgsCommandLine}: ${argExcerpt(pasted)}`,
    });
  }
  // Whitespace at an argument's EDGE, split into two facts because they read
  // differently to the operator and have different remedies. Neither is
  // trimmed: `parseArgsText` preserves a line verbatim on purpose (an argument
  // may legitimately be a separator or a formatting string), so the invisible
  // character is named rather than removed. A line the operator left EMPTY is
  // not reported at all -- an empty line is at least visible as one, and the
  // documented round trip already treats it as a deliberate empty argument.
  const edge: string[] = [];
  const blank: string[] = [];
  args.forEach((arg, index) => {
    if (arg === '' || arg === arg.trim()) return;
    if (arg.trim() === '') blank.push(blankDetailLine(arg, index + 1));
    else edge.push(whitespaceDetailLine(arg, index + 1));
  });
  if (edge.length > 0) {
    warnings.push({
      key: 'edge-whitespace',
      message: t.runtimeSpecArgsEdgeWhitespace,
      detail: edge.join('\n'),
    });
  }
  if (blank.length > 0) {
    warnings.push({
      key: 'blank-line',
      message: t.runtimeSpecArgsBlankLine,
      detail: blank.join('\n'),
    });
  }
  // Only while the AGENT owns the port: a `listen_port` of 0 makes it grab an
  // ephemeral one and route there (server-agent internal/runtime/manager.go
  // startProcess), so a literal in the args puts the child somewhere the health
  // probe never looks. With a listen port set, the same literal is at worst
  // redundant, so there is nothing to say.
  if (listenPort === 0) {
    const port = findHardcodedPort(args);
    if (port !== null) {
      warnings.push({
        key: 'hardcoded-port',
        message: `${t.runtimeSpecArgsHardcodedPort}: ${port}`,
      });
    }
  }
  return warnings;
}

// A "${...}" occurrence's classification, ported line-for-line from
// server-agent/internal/runtime/policy_local.go's ExpandPlaceholders -- see
// that file for the full rationale. Both an argument string and an env
// value pass through the SAME agent-side check at process-start time (this
// task's UI has no live-status view yet to surface a failure there -- see
// the task-20 report's Deviation 2), so the portal form must refuse
// everything the agent would refuse and accept everything it would accept:
//   - "${PORT}" (exact) -> valid, becomes the assigned port at start.
//   - "${MODEL}" (exact) -> valid, becomes the mapping's app_model_name.
//   - "${AGENT_ENV:NAME}" with a non-empty NAME -> valid, UNLESS NAME starts
//     with "OP_AGENT_" (the agent's own credential namespace) -> reserved.
//   - anything else whose upper-cased inner text STARTS WITH "PORT" or
//     "AGENT_ENV" -> a malformed near-miss (a typo of one of those forms):
//     "${PORTX}", "${port}", "${AGENT_ENV:}", "${AGENT_ENVV:x}". MODEL is
//     deliberately NOT in that prefix list -- "${MODEL_PATH}",
//     "${MODELS_DIR}" and "${MODEL_ID}" are plausible pass-through tokens, so
//     the agent exact-matches "${MODEL}" and lets every variant through.
//   - everything else -- arbitrary "${...}" text a model server's own
//     templating syntax might use, e.g. "${TRANSPORT}", "${EXPORT_DIR}",
//     "${MY_AGENT_ENVIRONMENT}" -- passes through untouched.
// This MUST be a prefix match, never a substring/Contains check, or those
// last three examples would be wrongly refused (see the Go source's doc
// comment for the full explanation of that prior mistake).
const placeholderPattern = /\$\{[^}]*\}/g;
const agentEnvPrefix = 'AGENT_ENV:';
const agentOwnEnvPrefix = 'OP_AGENT_';

// The agent's base-environment names (server-agent internal/runtime
// policy_local.go `baseEnvNames`), which it copies from its OWN environment
// into every child and therefore refuses as spec env keys. Mirrored here so
// the form says no before the round trip rather than letting the spec save
// and fail at process start as `not_permitted`.
//
// UPPER-CASE and compared upper-cased, exactly as the agent does: Windows
// resolves environment names case-insensitively, so "Path" and "SystemRoot"
// -- the only spellings a Windows operator would type -- are the same
// variables. The four Windows names are not decoration: the agent copies
// USERPROFILE/LOCALAPPDATA (per-user cache roots) and SYSTEMROOT/WINDIR
// (system DLLs, and Winsock initialisation) so a Windows child can resolve a
// home directory and open a socket at all.
const reservedEnvKeys = ['PATH', 'HOME', 'USERPROFILE', 'LOCALAPPDATA', 'SYSTEMROOT', 'WINDIR'];

function findPlaceholderViolation(
  text: string,
): { kind: 'reserved' | 'malformed'; match: string } | null {
  const matches = text.match(placeholderPattern);
  if (!matches) return null;
  for (const match of matches) {
    const inner = match.slice(2, -1); // strip "${" and "}"
    if (inner === 'PORT') continue;
    if (inner.startsWith(agentEnvPrefix) && inner.length > agentEnvPrefix.length) {
      const name = inner.slice(agentEnvPrefix.length);
      if (name.startsWith(agentOwnEnvPrefix)) return { kind: 'reserved', match };
      continue;
    }
    const upper = inner.toUpperCase();
    if (upper.startsWith('PORT') || upper.startsWith('AGENT_ENV')) {
      return { kind: 'malformed', match };
    }
  }
  return null;
}

// Runs findPlaceholderViolation over every arg AND every env value (never
// env keys -- those have their own PATH/HOME check in parseEnvText below).
function validatePlaceholders(values: string[], t: Translation): string | undefined {
  for (const value of values) {
    const violation = findPlaceholderViolation(value);
    if (!violation) continue;
    return violation.kind === 'reserved'
      ? t.runtimeSpecEnvReserved
      : `${t.runtimeSpecPlaceholderInvalid}: ${violation.match}`;
  }
  return undefined;
}

// The environment variables the `set_visible_devices` option OWNS: a spec may
// not hand-set any of them while the option is on. Mirrors the backend's
// runtimeSpecVisibleDevicesVars (portal/service_runtime.go) and, through it,
// the agent's visibleDevicesOwnedVars -- all three lists must refuse exactly
// the same names, or the form starts rejecting specs the backend accepts.
//
// HIP_VISIBLE_DEVICES is included although the agent never SETS it: it selects
// from what ROCR_VISIBLE_DEVICES already left visible, so combining a hand-set
// HIP list with agent-managed ROCR filtering leaves the child with no usable
// device. Runtime-specific selectors the agent neither sets nor filters through
// (ONEAPI_DEVICE_SELECTOR, GPU_DEVICE_ORDINAL) are deliberately NOT here --
// composing one of those with the option is the documented escape hatch.
const visibleDevicesOwnedVars = [
  'CUDA_VISIBLE_DEVICES',
  'ROCR_VISIBLE_DEVICES',
  'HIP_VISIBLE_DEVICES',
];

/**
 * The two combinations `set_visible_devices` may not be saved in, checked
 * before the round trip so the operator reads a message in the form instead of
 * a backend error code.
 *
 * Both are refused by the backend too (and by the agent again at launch); this
 * is the same early-feedback mirror the reserved-env-key and duplicate-GPU-index
 * checks are, not a second source of truth. The order matches the backend's, so
 * a spec that is wrong in both ways reports the same one either way.
 *
 *  - A hand-set visibility variable is two sources for one value: whichever
 *    wins, the config reads identically, so an operator cannot tell which cards
 *    the model is actually on.
 *  - An empty GPU list would make the agent emit `CUDA_VISIBLE_DEVICES=`, and an
 *    EMPTY visibility value does not mean "no restriction" -- it means NOTHING
 *    is visible.
 */
function validateVisibleDevices(
  on: boolean,
  env: Record<string, string>,
  gpuCount: number,
  t: Translation,
): string | undefined {
  if (!on) return undefined;
  for (const key of Object.keys(env)) {
    if (visibleDevicesOwnedVars.includes(key.trim().toUpperCase())) {
      return `${t.errorRuntimeSpecVisibleDevicesConflict}: ${key}`;
    }
  }
  if (gpuCount === 0) return t.errorRuntimeSpecVisibleDevicesNoGpus;
  return undefined;
}

/**
 * The first GPU index that appears more than once, or `null`.
 *
 * Both server-side writes that carry per-GPU rows REFUSE a repeated index
 * outright -- `validateRuntimeSpecGPUs` and `SetServerGPUBudgets`, each
 * returning its own sentinel (backend `internal/portal/service_runtime.go`).
 * Checked, not assumed: neither dedupes, so a row the operator filled in is
 * never silently discarded, which is the outcome that would have been worse
 * than a message. What they do produce is a generic "invalid GPU
 * configuration" / "invalid GPU budget" AFTER the round trip, naming neither
 * the field nor the reason -- and both forms let an operator type a colliding
 * index by hand (`addGpuRow`/`addBudgetRow` only keep the auto-PROPOSED value
 * clear of collisions). So the collision is named here, before the write, in
 * the same validate-on-submit idiom the env and placeholder checks use.
 */
function duplicateGpuIndex(indices: number[]): number | null {
  const seen = new Set<number>();
  for (const index of indices) {
    if (seen.has(index)) return index;
    seen.add(index);
  }
  return null;
}

/**
 * gpuOptionLabel renders one telemetry-reported GPU as a picker option.
 *
 * THE DESIGN CONSTRAINT IS EIGHT IDENTICAL CARDS. 4x or 8x of the same model
 * is the normal AI-server build, so `name` is a recognition aid and never a
 * handle: a list that reads as eight copies of "NVIDIA RTX 4090" has failed
 * even though every value behind it differs. The label is therefore built as
 * `GPU <index> · <name> · <handle>`:
 *
 *  - the INDEX leads because it is the value being set. Whatever else the row
 *    shows, the operator must be able to see which number they just chose --
 *    and it is the only part that is always present, which is what makes the
 *    eight-identical-cards case work at all.
 *  - the NAME is what a human recognises the card by.
 *  - the HANDLE (stableGpuHandle) is the tie-breaker, and it is the part worth
 *    extending: see its own note.
 *
 * memory_total_bytes is deliberately NOT here. On the host this exists for it
 * is identical across all eight cards, so it adds a wide column and breaks no
 * tie; on a mixed host the name already differs. (Live free memory would
 * genuinely answer "which card should I use", but it is not in this data
 * source -- the hardware inventory is static -- and it is not identity.)
 */
function gpuOptionLabel(gpu: HardwareGPU): string {
  const parts = [`GPU ${gpu.index}`];
  const name = gpu.name.trim();
  if (name) parts.push(name);
  const handle = stableGpuHandle(gpu);
  if (handle) parts.push(handle);
  return parts.join(' · ');
}

/**
 * stableGpuHandle picks the most physically meaningful identifier telemetry
 * actually reported for this card, in descending order of what an operator can
 * act on. ADD NEW IDENTIFIERS HERE, in priority order -- that is the whole
 * point of the indirection.
 *
 *  1. `pci_bus_id` — maps to a physical slot and survives the index
 *     renumbering across reboots that the GPU budget rows'
 *     expected_uuid/expected_name drift detection exists to catch. NVIDIA
 *     only.
 *  2. a shortened `uuid` — unique and stable but opaque; enough to tell two
 *     otherwise identical rows apart, which is all this needs to do.
 *  3. nothing — AMD and Apple report neither. The label falls back to index +
 *     name, which still distinguishes every row.
 */
function stableGpuHandle(gpu: HardwareGPU): string {
  const busID = gpu.pci_bus_id?.trim() ?? '';
  if (busID) return busID;
  const uuid = gpu.uuid?.trim() ?? '';
  if (!uuid) return '';
  // "GPU-a1b2c3d4-...." -> "a1b2c3d4…": the prefix is constant across every
  // NVIDIA card and the tail is never read, so neither disambiguates.
  const body = uuid.replace(/^GPU-/i, '');
  return body.length > 8 ? `${body.slice(0, 8)}…` : body;
}

// KEY=value per line, one env var per line. Blank/whitespace-only lines are
// skipped; a malformed (no "=") line is reported via the existing
// runtime_spec.env_invalid label so it reads the same way a backend
// rejection would. Only the KEY portion is trimmed (stray leading
// indentation from a paste) -- the VALUE is preserved byte-for-byte after
// the first "=" (a value may legitimately contain "=" itself, and trimming
// the whole line before splitting would silently eat meaningful leading/
// trailing whitespace IN the value). A key naming one of the agent's base
// environment variables is refused outright, in any capitalisation -- see
// reservedEnvKeys; the ${AGENT_ENV:OP_AGENT_*} / malformed-placeholder checks
// run separately, over both args and env values together, via
// validatePlaceholders.
function parseEnvText(
  text: string,
  t: Translation,
): { env: Record<string, string>; error?: string } {
  const env: Record<string, string> = {};
  for (const raw of text.split('\n')) {
    const line = raw.replace(/\r$/, '');
    if (line.trim() === '') continue;
    const eq = line.indexOf('=');
    if (eq <= 0) {
      return { env: {}, error: t.errorRuntimeSpecEnvInvalid };
    }
    const key = line.slice(0, eq).trim();
    const value = line.slice(eq + 1);
    if (reservedEnvKeys.includes(key.toUpperCase())) {
      return { env: {}, error: t.runtimeSpecEnvReserved };
    }
    env[key] = value;
  }
  return { env };
}

function formatEnvText(env: Record<string, string>): string {
  return Object.entries(env)
    .map(([k, v]) => `${k}=${v}`)
    .join('\n');
}

export function RuntimeAdminSection({
  t,
  api,
  // Area 1 doesn't read this (server context already folds into `trail`
  // before this component ever mounts, same as MappingSection); areas 2-3
  // (this task) are server-scoped -- the matrix's budget-sum tooltip and the
  // "server limits" tab both need server.id, and the latter also reads/writes
  // server.runtime_max_processes. Live status (Task 22) will need it too.
  server,
  application,
  trail = [],
}: Readonly<{
  t: Translation;
  api: Pick<
    PortalApi,
    | 'mappings'
    | 'createMapping'
    | 'updateMapping'
    | 'deleteMapping'
    | 'runtimeSpec'
    | 'putRuntimeSpec'
    | 'deleteRuntimeSpec'
    | 'runtimeCoresidency'
    | 'putRuntimeCoresidency'
    | 'runtimeWarnings'
    | 'gpuBudgets'
    | 'putGpuBudgets'
    | 'runtimeReport'
    | 'subscribeRuntimeStatus'
    // T3: the live-status tab's per-row log view.
    | 'subscribeRuntimeLogs'
    // Task 21 (matrix + server limits): the process-limit field is saved
    // through the general server PATCH, and a new budget row is prefilled
    // from the same live-telemetry hardware report the Hardware tab reads.
    | 'updateServer'
    | 'serverHardware'
    // The model-mapping tab renders the SAME edit mask an ordinary application
    // gets (`MappingForm`), context-size probe included -- "der selbe Edit" was
    // the requirement, and the probe fills a field IN that form rather than
    // navigating away from it. These three serve only that.
    | 'activeBenchmarks'
    | 'benchmarkStatus'
    | 'probeMappingContext'
  >;
  server: PortalServer;
  application: PortalApplication;
  trail?: BreadcrumbItem[];
}>) {
  const { showError, showSuccess } = useToast();
  const [tab, setTab] = useState<Tab>('specs');
  const [specMode, setSpecMode] = useState<SpecMode>('list');
  const [busy, setBusy] = useState(false);
  const [confirmingDeleteId, setConfirmingDeleteId] = useState('');
  const [loadingEditFor, setLoadingEditFor] = useState('');
  // The mapping row the model-mapping tab is editing, or null. Kept separate
  // from `specMode`: they are two different sub-views over two different
  // documents, and the tab strip is hidden while either is open, so neither can
  // start the other.
  const [mappingEdit, setMappingEdit] = useState<PortalModelMapping | null>(null);

  const {
    data: mappingsData,
    setData: setMappings,
    error: mappingsError,
    loading: mappingsLoading,
    reload: reloadMappings,
  } = useResource(
    () => api.mappings(application.id).then((r) => r.data),
    [api, application.id, t],
    t,
  );
  const mappings = mappingsData ?? [];
  // FOUR states, not two (shared/ResourceFallback.tsx). `mappingsLoading ||
  // mappingsData === null` -- the convention every other resource on this
  // screen copied from here -- reports a FAILED GET as "still loading", and
  // `useResource` leaves `data` null on a first failure, so that is forever:
  // the list claims to be loading indefinitely, and `specsSettled` below can
  // never become true either, which leaves EVERY live-status row unresolvable,
  // its actions cell blank and its `Unmatched` chip suppressed -- the silent
  // blank the chip exists to prevent.
  //
  // This resource feeds all four tabs (the list, the matrix's spec set, the
  // status table's spec_id -> mapping join and with it every override action),
  // so its failure is a screen-wide fact and its banner sits above the tab
  // strip rather than in one tab.
  const mappingsStatus = resourceState({
    loading: mappingsLoading,
    error: mappingsError,
    data: mappingsData,
  });
  const mappingsFailed = mappingsStatus === 'error' || mappingsStatus === 'stale-error';
  useEffect(() => {
    if (mappingsError) showError(mappingsError);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [mappingsError]);

  const {
    data: warningsData,
    error: warningsError,
    loading: warningsLoading,
    reload: reloadWarnings,
  } = useResource(
    () => api.runtimeWarnings(application.id).then((r) => r.warnings),
    [api, application.id, t],
    t,
  );
  const warnings = warningsData ?? [];
  // The fourth resource, and the one C1 left behind although its own title was
  // "every remaining resource" (fix round 1, M8). It gates nothing and its
  // failure is not silent (the error toast below does not auto-dismiss), so
  // this is the mildest instance of the pattern -- but it is the same one: with
  // `warningsData ?? []` an operator cannot tell "this application has no
  // advisory warnings" from "we failed to find out", and the list carries facts
  // like a timeout configured below the startup timeout. Hence `info`, not
  // `warning`: a failed advisory read is not the same event as a failed read
  // that turns a screen read-only.
  const warningsStatus = resourceState({
    loading: warningsLoading,
    error: warningsError,
    data: warningsData,
  });
  const warningsFailed = warningsStatus === 'error' || warningsStatus === 'stale-error';
  useEffect(() => {
    if (warningsError) showError(warningsError);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [warningsError]);

  // Per-mapping runtime spec, loaded lazily and once per mapping id into a
  // map (the brief's "lazy... loaded once" recipe) -- a bulk endpoint does
  // not exist, so the list view fans out one GET per row.
  const [specsById, setSpecsById] = useState<Record<string, RuntimeSpec>>({});
  const loadedIdsRef = useRef<Set<string>>(new Set());
  // Which mapping ids' spec GET has SETTLED -- success or failure. A failed
  // GET never lands in `specsById`, so "specsById has no entry" cannot stand
  // in for "settled", and the live-status table needs the difference: "no
  // spec matches this row's spec_id" is only a fact once every one of these
  // one-GET-per-mapping requests is done. Before that it is indistinguishable
  // from "not loaded yet" -- and rendering the unresolved marker in that
  // window would make every perfectly normal row flash "Unmatched".
  const [specLoadSettled, setSpecLoadSettled] = useState<ReadonlySet<string>>(new Set());
  // Which mapping ids' spec GET settled by FAILING. `specLoadSettled` counts
  // both outcomes deliberately -- it answers "is the fan-out over", which is
  // all the `Unmatched` marker needs -- but "does this mapping have a spec" is
  // a different question, and a rejected GET does not answer it. Both leave
  // `specsById` without an entry, so this set is the only thing that separates
  // "the server said there is no spec" from "we never found out" (see
  // `specStateKnown`, which is where the difference is consumed).
  const [specReadFailedIds, setSpecReadFailedIds] = useState<ReadonlySet<string>>(new Set());
  useEffect(() => {
    const toLoad = mappings.filter((m) => !loadedIdsRef.current.has(m.id));
    if (toLoad.length === 0) return undefined;
    let cancelled = false;
    (async () => {
      for (const m of toLoad) {
        loadedIdsRef.current.add(m.id);
        // A read, ordered against this mapping's writes (`commitSpecRead`,
        // declared below). A mapping with no cached spec renders no override
        // actions, so THIS loop cannot be overtaken by an override write -- but
        // `openEdit` is clickable on a row whose lazy GET is still in flight,
        // and a save from that form is a write this GET would otherwise land on
        // top of with a pre-save snapshot.
        const seen = beginSpecRead(m.id);
        try {
          const spec = await api.runtimeSpec(m.id);
          if (!cancelled) commitSpecRead(m.id, seen, spec);
        } catch (err) {
          if (!cancelled) showError(formatPortalError(err, t));
          // Not guarded by `cancelled` either, and for the same reason as the
          // `finally` below: this mapping's one lazy read is over and answered
          // nothing, and that stays true across the deps change that cancelled
          // it. `loadedIdsRef` never retries, so it stays true until something
          // else answers the question (`openEdit`'s own GET).
          setSpecReadFailedIds((cur) => new Set(cur).add(m.id));
        } finally {
          // Deliberately NOT guarded by `cancelled`: this records that the
          // request is over, which stays true across the deps change that
          // cancelled it (loadedIdsRef means it is never retried, so a
          // skipped record would wedge `specsSettled` at false forever).
          setSpecLoadSettled((cur) => new Set(cur).add(m.id));
        }
      }
    })();
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [mappings, api]);
  // NOT `!loading && data !== null`, and not `ready` alone either.
  //
  // `loading` must be excluded: a status row is only "unmatched" once every
  // one-GET-per-mapping request has settled, or the marker flashes on every
  // ordinary row during the initial fan-out.
  //
  // `stale-error` is deliberately INCLUDED (fix round 1, C4). Excluding it read
  // as caution but was the opposite: `mappings` and `specsById` still hold the
  // previous payload, so the rows that resolve go on resolving and go on
  // offering their overrides -- and the ONLY thing that disappeared was the
  // marker on the rows that genuinely do not resolve, i.e. the silent blank
  // task 22's N3 removed, reached through the fix for N3. The chip's claim is
  // about the list this row was already matched against; that the list could
  // not be REFRESHED is what the screen-wide banner is for, and it now says so
  // in its own words instead of borrowing the hard-failure text.
  const specsSettled =
    (mappingsStatus === 'ready' || mappingsStatus === 'stale-error') &&
    mappings.every((m) => specLoadSettled.has(m.id));

  /**
   * Per-mapping ordering for `specsById`. Every write to that cache goes
   * through `commitSpecCache`/`forgetSpecCache` and every read through
   * `commitSpecRead`; nothing else may call `setSpecsById`.
   *
   * `specsById` is a cache of server truth, and the three override/restart
   * writes deliberately update it ABOVE their run token (fix round 2, N1) so a
   * write abandoned by its watchdog that later succeeds still corrects the row.
   * That left ARRIVAL ORDER deciding what the cache holds when two operations
   * on the SAME mapping overlap: `RuntimeSpec` carries no version/etag, so a
   * late response is indistinguishable from a current one. The sequence that
   * hurts is reachable: an override PUT hangs past the 30 s watchdog, the row
   * unlocks, a second write on the same mapping resolves, and then the first
   * finally resolves and overwrites the cache with the outcome of the write the
   * operator was already told had been given up on -- so the row offers the
   * inverse of the actions that apply.
   *
   * Two counters -- an ISSUE order and a COMMIT order -- because a write must
   * be tested against what has already been COMMITTED, never against what has
   * merely been issued (fix round 1, C1/C2; the first version had one counter
   * and used it for both roles). Both tests fall out of ONE rule: writes
   * advance the commit order, and reads must not observe it advancing.
   *
   *  - a WRITE's payload is what the server acknowledged storing, so it may
   *    become the cache entry unless a LATER write has already COMMITTED one.
   *    Comparing against the last ISSUED ticket instead made any later write
   *    burn the comparison even when it wrote nothing at all -- so a later
   *    write that FAILED (its retry rejecting, a spec delete failing, a form
   *    save's spec PUT failing) permanently discarded an earlier
   *    abandoned-then-successful write, leaving the cache at `admin_state: ''`
   *    while the server held `force_stopped`. That is precisely the N1 defect,
   *    back again: the row then offers Force start / Force stop / Restart and
   *    NO Clear override while the model is admission-blocked, with the write
   *    timeout notice on screen telling the operator to clear by hand an
   *    override the row denies exists. A failure must NOT retire its ticket by
   *    decrementing the counter either -- that races a third write.
   *  - a READ's payload is only a snapshot, so it may become the cache entry
   *    only while the COMMIT order has not moved since the read started.
   *    `openEdit`'s GET is issued from the specs tab, which `overrideBusy`
   *    does not lock (`rowActions` gates only on `loadingEditFor`) and whose
   *    tab strip is never disabled, so it overlaps an override write in BOTH
   *    directions -- and nothing re-fetches on the way back (`loadedIdsRef`
   *    blocks the lazy loader), so whichever way the poison lands it is
   *    permanent:
   *      * GET issued first, lands last: the write commits inside the read's
   *        window, so the counter has moved and the snapshot is refused;
   *      * write issued first, lands first (fix round 2, F1 -- the ordering the
   *        first version of this guard MISSED): the read began after that
   *        write's ticket was ISSUED, so an issue-order snapshot still matched
   *        on the way back and the pre-PUT document went in over
   *        `force_stopped`. A commit-order snapshot does not match, because
   *        the commit happened inside the window.
   *    Testing the commit order also stops punishing a write that wrote
   *    NOTHING: a FAILED write never advances it, so it no longer discards the
   *    single lazy read a mapping gets (`loadedIdsRef` never retries), which
   *    used to leave a false `Unmatched` chip, no override actions at all, and
   *    a Delete silently downgraded from delete-spec to delete-MAPPING.
   *    A read never advances the commit order itself: an older write still in
   *    flight is more authoritative than a snapshot that may have been served
   *    before that write applied.
   *
   * Every write path takes a ticket, deliberately including the create/edit
   * form and both delete branches: a hung override write resolving after a form
   * save is the same bug with only ONE hang, and a delete is a write to the
   * same cache entry.
   */
  const specWriteSeqRef = useRef<Map<string, number>>(new Map());
  const specCommittedSeqRef = useRef<Map<string, number>>(new Map());
  /** Issues the next write ticket for this mapping's cache entry. */
  function beginSpecWrite(mappingId: string): number {
    const ticket = (specWriteSeqRef.current.get(mappingId) ?? 0) + 1;
    specWriteSeqRef.current.set(mappingId, ticket);
    return ticket;
  }
  /** True when no LATER write has already committed for this mapping. */
  function acceptSpecWrite(mappingId: string, ticket: number): boolean {
    if (ticket < (specCommittedSeqRef.current.get(mappingId) ?? 0)) return false;
    specCommittedSeqRef.current.set(mappingId, ticket);
    return true;
  }
  function commitSpecCache(mappingId: string, ticket: number, spec: RuntimeSpec) {
    if (!acceptSpecWrite(mappingId, ticket)) return;
    setSpecsById((cur) => ({ ...cur, [mappingId]: spec }));
  }
  /** The mapping-delete half: the entry is gone, and that fact is ordered too. */
  function forgetSpecCache(mappingId: string, ticket: number) {
    if (!acceptSpecWrite(mappingId, ticket)) return;
    setSpecsById((cur) => {
      const next = { ...cur };
      delete next[mappingId];
      return next;
    });
  }
  /**
   * Snapshots the COMMIT counter for a READ -- not the issue counter (fix
   * round 2, F1). Deliberately does not increment: a GET issues no write, so
   * it must not stop a write that is already in flight from committing.
   *
   * The issue counter only catches writes STARTED after the read started, and
   * a write already in flight when the read starts is invisible to it -- the
   * mirror image of C2, and the same poison: Force stop takes ticket 1, the
   * operator switches tab (never disabled) and hits Edit, the PUT commits
   * `force_stopped`, and the GET lands with `issued(1) === seen(1)` and writes
   * its pre-PUT snapshot over it. Snapshotting `committed` refuses that: the
   * counter is monotone, so "unchanged" means "nothing committed inside my
   * window" no matter when the write was issued.
   */
  function beginSpecRead(mappingId: string): number {
    return specCommittedSeqRef.current.get(mappingId) ?? 0;
  }
  function commitSpecRead(mappingId: string, seen: number, spec: RuntimeSpec) {
    if ((specCommittedSeqRef.current.get(mappingId) ?? 0) !== seen) return;
    setSpecsById((cur) => ({ ...cur, [mappingId]: spec }));
  }

  // ---- Area 2: co-residency matrix --------------------------------------
  // Canonicalisation (which of the two mapping ids sorts first) is entirely
  // the BACKEND's job (SetCoResidency sorts each pair before storing/
  // comparing -- see service_runtime.go) -- RuntimeMatrix mirrors the same
  // rule client-side purely so the UI never has to wait on a round trip to
  // know which cell a click means. `pairs` is always the FULL current set;
  // toggling computes the complete new set and PUTs it (full replace, never a
  // delta -- this is how the backend models it and avoids lost updates
  // between two admins editing concurrently).
  const {
    data: coresidencyData,
    setData: setCoresidencyData,
    error: coresidencyError,
    loading: coresidencyLoading,
    reload: reloadCoresidency,
  } = useResource(
    () => api.runtimeCoresidency(application.id).then((r) => r.pairs),
    [api, application.id, t],
    t,
  );
  // `null` (not loaded yet) and `[]` (loaded, genuinely empty) are DIFFERENT
  // facts -- collapsing them with `coresidencyData ?? []` would make a click
  // that lands before the GET settles compute its full-replace PUT from an
  // empty list, wiping every pair a previous admin already saved. `ready`
  // gates both rendering (a loading message instead of the matrix) and
  // writing (see the missing-loading-state fix, task-21 review round 1).
  //
  // It is `resourceState`'s `ready` rather than the old
  // `!loading && data !== null`, which is STRICTLY LOOSER: that test reports a
  // failed RELOAD over an existing payload as ready
  // (shared/ResourceFallback.tsx), and this loader's deps include `t`, so a
  // language switch whose re-GET fails reaches exactly that state with the
  // component still mounted. The matrix would then render, clickable, off
  // pairs we just failed to refresh -- and a toggle PUTs the FULL replacement
  // list, so a pair another admin added meanwhile would be silently deleted.
  // That is task-21's Critical reached through a different door, so this gate
  // gets tighter here, never looser.
  const coresidencyStatus = resourceState({
    loading: coresidencyLoading,
    error: coresidencyError,
    data: coresidencyData,
  });
  const coresidencyReady = coresidencyStatus === 'ready';
  const coresidencyPairs = coresidencyData ?? [];
  useEffect(() => {
    if (coresidencyError) showError(coresidencyError);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [coresidencyError]);

  const matrixSpecs: RuntimeMatrixSpec[] = mappings.map((m) => ({
    id: m.id,
    model: m.gateway_model_name,
    gpus: (specsById[m.id]?.gpus ?? []).map((g) => ({
      index: g.index,
      vramMb: g.vram_estimate_mb,
    })),
  }));

  // Serializes toggles: while a PUT is in flight, the matrix is disabled
  // (mirrors Area 3's limitsBusy) so a second click cannot compute its next
  // full-replace list from state a still-outstanding first response might
  // later overwrite. Chosen over a toggle QUEUE (buffering clicks and
  // replaying them once the in-flight PUT settles) because every other
  // full-replace write in this section (spec save, limits save) already
  // uses the same "disable until the response lands" idiom, and a silently
  // queued click is easy to mistake for a dropped one -- the operator sees
  // the matrix visibly busy instead.
  const [coresidencyBusy, setCoresidencyBusy] = useState(false);

  async function toggleCoresidency(a: string, b: string) {
    const exists = coresidencyPairs.some(([p, q]) => (p === a && q === b) || (p === b && q === a));
    const next: [string, string][] = exists
      ? coresidencyPairs.filter(([p, q]) => !((p === a && q === b) || (p === b && q === a)))
      : [...coresidencyPairs, [a, b]];
    const previous = coresidencyPairs;
    setCoresidencyData(next); // optimistic
    setCoresidencyBusy(true);
    try {
      const stored = await api.putRuntimeCoresidency(application.id, { pairs: next });
      setCoresidencyData(stored.pairs);
    } catch (err) {
      setCoresidencyData(previous);
      showError(formatPortalError(err, t));
    } finally {
      setCoresidencyBusy(false);
    }
  }

  // ---- Area 3: server limits ---------------------------------------------
  // Live telemetry -- the SAME hardware report the Hardware tab reads
  // (api.serverHardware -> HardwareResponse.report.gpus, each carrying
  // index/name/uuid/memory_total_bytes) -- drives two things here: prefilling
  // a NEW budget row's index+VRAM so the operator never has to hand-look-up a
  // card's memory, and the expected_uuid drift check below.
  const hardware = useLatestFetch(() => api.serverHardware(server.id), [api, server.id]);
  const telemetryGpus: HardwareGPU[] =
    hardware.data?.available && hardware.data.report ? hardware.data.report.gpus : [];
  // Whether the hardware fetch has actually SETTLED, as distinct from having
  // returned nothing yet. The spec form's GPU picker degrades to "this server
  // has reported no GPUs" when the list is empty, and flashing that sentence
  // during the initial load would tell an operator something false about their
  // hardware for as long as the request takes.
  const hardwareSettled = hardware.status === 'ok' || hardware.status === 'error';

  const {
    data: gpuBudgetsData,
    setData: setGpuBudgetsData,
    error: gpuBudgetsError,
    loading: gpuBudgetsLoading,
    reload: reloadGpuBudgets,
  } = useResource(() => api.gpuBudgets(server.id).then((r) => r.budgets), [api, server.id, t], t);
  // Same "null (not loaded) vs. [] (loaded, empty)" distinction as the
  // co-residency matrix above: `budgetRows` starts `[]` and would look
  // exactly like "no budgets configured" if Save were reachable before this
  // GET settles -- and Save PUTs whatever `budgetRows` holds as the FULL
  // replacement set, so that would erase every previously configured
  // per-GPU budget on a single premature click.
  //
  // And the same reason for `resourceState`'s `ready` over the old
  // `!loading && data !== null`, one degree worse here: on a failed reload
  // `budgetRows` still holds the payload from BEFORE it (the re-seed effect
  // below returns early while `data` is unchanged), so the looser test showed a
  // filled-in form whose Save would write values we had just failed to
  // re-read, as the full replacement set, with the failure invisible.
  const gpuBudgetsStatus = resourceState({
    loading: gpuBudgetsLoading,
    error: gpuBudgetsError,
    data: gpuBudgetsData,
  });
  const gpuBudgetsReady = gpuBudgetsStatus === 'ready';
  useEffect(() => {
    if (gpuBudgetsError) showError(gpuBudgetsError);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [gpuBudgetsError]);
  // The matrix's advisory tooltip reflects the last SAVED budgets, never
  // Area 3's possibly-unsaved edits below (budgetRows) -- an in-progress
  // draft on another tab must not change what the matrix claims is "the
  // budget".
  const savedBudgetsByGpuIndex: Record<number, number> = Object.fromEntries(
    (gpuBudgetsData ?? []).map((b) => [b.index, b.budget_mb]),
  );

  type BudgetRow = {
    rowKey: string;
    index: number;
    budgetMb: number;
    expectedUuid: string;
    expectedName: string;
  };
  const [budgetRows, setBudgetRows] = useState<BudgetRow[]>([]);
  // Whether the operator has touched the rows since the last seed. Only the
  // three mutators below set it, and only a successful save clears it -- the
  // seed does NOT: it early-returns while the flag is up and leaves it alone,
  // so the draft survives every later re-GET too, not just the first.
  const budgetRowsDirtyRef = useRef(false);
  // Re-seeds the editable rows whenever gpuBudgetsData gets a genuinely NEW
  // value. That has THREE triggers, not one (fix round 1, M9 -- this comment
  // used to claim "it is never refreshed by a background poll, so this can't
  // clobber an in-progress edit", and two of the three make that false):
  //
  //  - the initial load, and the fresh authoritative list this component feeds
  //    back via setGpuBudgetsData after a successful save (see saveLimits);
  //  - a LANGUAGE SWITCH: `t` is a dep of this loader, so switching language
  //    re-GETs the budgets with the component mounted and hands back a new
  //    array identity -- no failure needed;
  //  - C1's new Retry button, after a failed reload.
  //
  // In the last two an unsaved edit used to snap back to the server's values
  // with no toast and no indication. So the seed is skipped while the draft is
  // dirty: the operator keeps their edits, and their Save still PUTs the
  // complete list they can see. Nothing here loosens the `gpuBudgetsReady`
  // gate, which is what actually decides whether Save exists at all.
  useEffect(() => {
    if (gpuBudgetsData === null) return;
    if (budgetRowsDirtyRef.current) return;
    setBudgetRows(
      gpuBudgetsData.map((b) => ({
        rowKey: makeRowKey(),
        index: b.index,
        budgetMb: b.budget_mb,
        expectedUuid: b.expected_uuid,
        expectedName: b.expected_name,
      })),
    );
  }, [gpuBudgetsData]);

  const [maxProcesses, setMaxProcesses] = useState(() => server.runtime_max_processes ?? 0);
  const [limitsBusy, setLimitsBusy] = useState(false);

  function addBudgetRow() {
    budgetRowsDirtyRef.current = true;
    setBudgetRows((rows) => {
      const used = new Set(rows.map((r) => r.index));
      // Prefer the next telemetry-reported GPU not yet configured -- the
      // agent already told us its index AND its total VRAM, so the operator
      // shouldn't have to look either up (see the task-21 brief).
      const fromTelemetry = telemetryGpus.find((g) => !used.has(g.index));
      if (fromTelemetry) {
        return [
          ...rows,
          {
            rowKey: makeRowKey(),
            index: fromTelemetry.index,
            budgetMb: Math.round(fromTelemetry.memory_total_bytes / (1024 * 1024)),
            expectedUuid: '',
            expectedName: '',
          },
        ];
      }
      let index = 0;
      while (used.has(index)) index++;
      return [
        ...rows,
        { rowKey: makeRowKey(), index, budgetMb: 0, expectedUuid: '', expectedName: '' },
      ];
    });
  }
  function removeBudgetRow(idx: number) {
    budgetRowsDirtyRef.current = true;
    setBudgetRows((rows) => rows.filter((_, i) => i !== idx));
  }
  function updateBudgetRow(idx: number, patch: Partial<Pick<BudgetRow, 'index' | 'budgetMb'>>) {
    budgetRowsDirtyRef.current = true;
    setBudgetRows((rows) =>
      rows.map((r, i) => {
        if (i !== idx) return r;
        // Changing the index changes the row's identity: the loaded
        // expected_* snapshot belonged to the OLD index and would misreport
        // drift against whatever card now sits at the new one. The backend
        // re-snapshots it from telemetry on save if the new index is
        // genuinely unconfigured.
        if (patch.index !== undefined && patch.index !== r.index) {
          return { ...r, ...patch, expectedUuid: '', expectedName: '' };
        }
        return { ...r, ...patch };
      }),
    );
  }

  // expected_uuid/expected_name are a DRIFT DETECTOR, never a setting: the
  // backend snapshots them once, from telemetry, when a budget row is first
  // created, and never overwrites them again. A live mismatch means the card
  // at this index changed (driver update renumbering GPUs, or a hardware
  // swap) -- worth a warning, never a block: a renumbered card must not take
  // the server out of service. UUID is NVIDIA-only (AMD reports none, Apple
  // is a single unified-memory device), so an absent UUID on either side
  // means "no drift detection available here", not "drift detected".
  function driftFor(row: BudgetRow): HardwareGPU | undefined {
    if (!row.expectedUuid) return undefined;
    const live = telemetryGpus.find((g) => g.index === row.index);
    if (!live?.uuid) return undefined;
    return live.uuid !== row.expectedUuid ? live : undefined;
  }

  async function saveLimits() {
    // Same collision check as the spec form's GPU rows, same reason: an index
    // retyped onto another row's collides, the backend refuses the whole PUT
    // with a message that names neither row, and this Save is a FULL replace.
    const duplicateGpu = duplicateGpuIndex(budgetRows.map((r) => r.index));
    if (duplicateGpu !== null) {
      showError(`${t.runtimeGpuIndexDuplicate}: GPU ${duplicateGpu}`);
      return;
    }
    setLimitsBusy(true);
    try {
      const body: { budgets: GPUBudget[] } = {
        budgets: budgetRows.map((r) => ({
          index: r.index,
          budget_mb: r.budgetMb,
          expected_uuid: r.expectedUuid,
          expected_name: r.expectedName,
        })),
      };
      const savedBudgets = await api.putGpuBudgets(server.id, body);
      // Cleared BEFORE the state that re-arms the seed effect: what comes back
      // is the authoritative post-save list, and re-seeding from it (including
      // the expected_* snapshots the backend just took) is the whole reason
      // saveLimits feeds it back.
      budgetRowsDirtyRef.current = false;
      setGpuBudgetsData(savedBudgets.budgets);
      const updatedServer = await api.updateServer(server.id, {
        runtime_max_processes: maxProcesses,
      });
      setMaxProcesses(updatedServer.runtime_max_processes ?? 0);
      showSuccess(t.systemSaved);
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setLimitsBusy(false);
    }
  }

  // ---- Area 4: live status stream ----------------------------------------
  // Mirrors PerformanceSection's SSE effect: keyed on [api, server.id] only,
  // returning the unsubscribe. Both `snapshot` and `update` frames are FULL
  // replacements of the server's whole managed-process list (the api layer
  // already unwraps `{runtimes: [...]}` and swallows a malformed frame), so
  // `setStatusRows` replaces and never appends. The 'loading' value is owned
  // here -- `onStatus` only ever reports 'open' | 'error'.
  const [statusRows, setStatusRows] = useState<RuntimeStatus[]>([]);
  const [streamStatus, setStreamStatus] = useState<'open' | 'error' | 'loading'>('loading');
  // Monotonic count of frames received on the CURRENT subscription. The
  // restart sequence needs to tell "this spec IS stopped" (a state, possibly
  // reported before its own write even landed) apart from "this spec reported
  // stopped AFTER our write landed" (the transition it actually waits for) --
  // see RestartFlow.waitFrom. A ref, not state: the counter must not itself
  // trigger a render or re-arm an effect.
  const frameSeqRef = useRef(0);
  // The LATEST frame, readable outside a render. `startRestart` has to
  // re-check the state gate against what the stream says NOW, not against the
  // row its onClick closure captured when the cell rendered -- frames arrive
  // ~1/s, and a state change landing between that render's commit and the
  // click's dispatch would otherwise let a non-restartable row start a
  // sequence that can never complete. `statusRows` state cannot serve: the
  // handler closes over the same stale render.
  const statusRowsRef = useRef<RuntimeStatus[]>([]);
  const onStatusFrame = useCallback((rows: RuntimeStatus[]) => {
    frameSeqRef.current += 1;
    statusRowsRef.current = rows;
    setStatusRows(rows);
  }, []);
  useEffect(() => {
    // A server switch must not leave the previous server's processes on
    // screen while the new stream connects.
    setStatusRows([]);
    statusRowsRef.current = [];
    setStreamStatus('loading');
    frameSeqRef.current = 0;
    return api.subscribeRuntimeStatus(server.id, onStatusFrame, setStreamStatus);
  }, [api, server.id, onStatusFrame]);

  // ---- File mode + feature negotiation (spec §9, §10.2) -------------------
  const {
    data: reportData,
    error: reportError,
    loading: reportLoading,
    reload: reloadReport,
  } = useResource(() => api.runtimeReport(server.id), [api, server.id, t], t);
  // NOT two states. `!loading && data !== null` collapsed "the GET is still in
  // flight" and "the GET failed" (useResource sets `error` and leaves `data`
  // untouched) into one value -- so a failed report GET made this WHOLE screen
  // permanently read-only while every tab claimed to be loading, with the only
  // signal a toast that scrolls away. The failure now has its own rendering and
  // a retry, and it deliberately does NOT also raise a toast: a permanent
  // banner replaces a transient one.
  const reportStatus = resourceState({
    loading: reportLoading,
    error: reportError,
    data: reportData,
  });
  // `stale-error` is a FAILED report GET that left an earlier payload behind.
  // Reached here by the LANGUAGE SWITCH (fix round 1, M10 -- this used to name
  // a server switch, which task 22b established does not reach it): `t` is a
  // dep of every loader on this screen, so switching language re-runs them all
  // with the component mounted, while a real server switch REMOUNTS
  // (`ServerList.tsx` renders `ApplicationSection` with `key={`app-${id}`}`).
  // `server.id` is a dep too, so the cross-server payload is the defensive case
  // rather than the reachable one -- and either way the last known values are
  // not worth showing here, because "which runtime mode is this server in" is
  // exactly the fact that must not be answered from an unconfirmed payload. So
  // both failures get the same treatment: read-only, say so, offer the retry.
  const reportFailed = reportStatus === 'error' || reportStatus === 'stale-error';
  // `source` lives on the NESTED report object; RuntimeReport itself has none.
  // Gated on `ready` for the same reason: a stale payload must not decide
  // whether THIS server is in file mode.
  const reportContent =
    reportStatus === 'ready' && reportData?.available ? reportData.report : undefined;
  const fileMode = reportContent?.source === 'file';
  // The agent telling us it could not parse its own config file. In that state
  // `config` is whatever survived (possibly the zero value), so it must not be
  // rendered at all -- and this, not a missing tooltip, is what the operator
  // needs to see.
  //
  // The value is a CODE (runtimeParseErrorReasonByCode), and every gate below
  // tests TRUTHINESS rather than membership of that map, deliberately: a code
  // this build does not recognise still means "the agent could not parse its
  // file", so the unusable config must stay suppressed. The field is
  // `omitempty` on the wire, so a HEALTHY agent sends nothing here and none of
  // these gates fire.
  const parseError = reportContent?.parse_error ?? '';
  const reportConfig = useMemo(
    () => (fileMode && !parseError ? narrowReportConfig(reportContent?.config) : null),
    [fileMode, parseError, reportContent],
  );
  // Same discipline as areas 2/3, one step further out: until the report GET
  // has settled we do not know whether this whole screen is read-only, so no
  // writable affordance is presented at all. Gated on BOTH signals.
  const writesAllowed = reportStatus === 'ready' && !fileMode;

  const configuredSpecCount = mappings.filter((m) => specsById[m.id]?.configured).length;
  // Both derived once: `agent_features` is pinned non-nil by the backend
  // today, but the two places that read it must not disagree about whether
  // that is guaranteed.
  //
  // Gated on `ready` at the SOURCE, like `reportContent` above. Ungated they
  // hold the PREVIOUS server's (or the pre-failure) values during
  // `stale-error`, which is safe only for as long as their single render site
  // stays behind `featureMismatch`/`agentNeverReported` -- both of which
  // require `reportStatus === 'ready'` themselves. That is one edit away from
  // reintroducing the cross-server bug fixed elsewhere on this screen, and the
  // gate costs nothing: no reachable render changes, because every reader is
  // already behind the same condition.
  const reportReady = reportStatus === 'ready';
  const agentFeatures = reportReady ? (reportData?.agent_features ?? []) : [];
  const agentVersion = reportReady ? (reportData?.agent_version ?? '') : '';
  // Spec §9's visible half: gateway-side specs exist and the agent reports no
  // managed process at all. Without a banner here an operator configures specs
  // and watches nothing happen, with no clue why.
  //
  // But that silence has TWO causes, and they need different advice. The
  // backend yields `[]` features when there is no telemetry ROW at all, so the
  // single old banner told a server whose agent has never connected to
  // "update its agent", under an "Agent version: —". `server.agent_status`
  // separates the facts and was already in props: 'unconfigured'/'inactive'
  // plus an empty version and feature list is "no agent has ever reported",
  // not "the agent is too old".
  const runtimeSilent =
    reportStatus === 'ready' && configuredSpecCount > 0 && statusRows.length === 0;
  const agentNeverReported =
    runtimeSilent &&
    agentVersion === '' &&
    agentFeatures.length === 0 &&
    server.agent_status !== 'active';
  const featureMismatch =
    runtimeSilent && !agentNeverReported && !agentFeatures.includes('runtime_manager');

  // ---- Row overrides + the restart sequence ------------------------------
  // The live stream keys rows by SPEC id; every write is keyed by MAPPING id.
  // Only a CONFIGURED spec has an id, and only a loaded one can be re-sent
  // verbatim, so this map is also the "may this row offer overrides at all"
  // test.
  const specByRuntimeId = new Map<string, RuntimeSpec>();
  for (const spec of Object.values(specsById)) {
    if (spec.configured && spec.id) specByRuntimeId.set(spec.id, spec);
  }
  const mappingById = new Map<string, PortalModelMapping>(mappings.map((m) => [m.id, m]));

  /**
   * The model mapping behind a live status row, resolved BACKWARDS through
   * the spec that the stream's `spec_id` keys: spec_id -> RuntimeSpec ->
   * mapping_id -> mapping. `undefined` means the row cannot be resolved to a
   * mapping of THIS application -- a fact the status table renders
   * explicitly, see the model column.
   */
  function mappingForStatus(row: RuntimeStatus): PortalModelMapping | undefined {
    const spec = specByRuntimeId.get(row.spec_id);
    if (spec === undefined) return undefined;
    return mappingById.get(spec.mapping_id);
  }

  function gatewayNameFor(row: RuntimeStatus): string {
    return mappingForStatus(row)?.gateway_model_name ?? '';
  }

  // The spec whose managed-process output is being watched, or null. Opening
  // this is what MAKES the agent stream that spec (the gateway asks on the
  // first viewer and stops on the last), so it is deliberately per-row state
  // that clears on close rather than a mounted-but-hidden panel: a hidden
  // panel would keep an agent streaming output nobody is reading.
  const [logSpec, setLogSpec] = useState<{ specId: string; title: string } | null>(null);
  const [overrideBusy, setOverrideBusy] = useState(false);
  const [restart, setRestart] = useState<RestartFlow | null>(null);
  const [restartNotice, setRestartNotice] = useState<
    'timeout' | 'clear-timeout' | 'vanished' | null
  >(null);
  // How long the awaited spec has been missing from the stream. A ref, not
  // state, so tracking it never re-arms the timeout effect below.
  const absentSinceRef = useRef<number | null>(null);
  // Run tokens for the two write flows. Bumping one ABANDONS its flow: a
  // response landing after the flow was given up on (its watchdog fired) must
  // not toast success, must not re-lock the UI, and must not resurrect a
  // sequence the operator has already been told is over.
  const overrideRunRef = useRef(0);
  const restartRunRef = useRef(0);
  const mountedRef = useRef(true);
  // Set in the effect BODY as well as its cleanup -- matching
  // MappingSection/ModelServersSection/GroupServersSection, which all do the
  // same for exactly this reason. Under StrictMode's mount/unmount/remount
  // the cleanup runs before the second mount, so a ref only ever set to
  // `false` would stay false for the component's whole life and turn every
  // guarded setState into a permanent no-op -- one override click would then
  // lock the UI for good.
  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
    };
  }, []);
  // Any admin_state write while a restart runs would fight the sequence, so
  // ALL override actions lock, not just the restart one (the visibly-disabled
  // idiom areas 2/3 already use for coresidencyBusy/limitsBusy).
  const overridesLocked = restart !== null || overrideBusy;

  useEffect(() => {
    // A server switch abandons any in-flight restart: its remaining step only
    // means anything while we are still watching THAT server's stream.
    setRestart(null);
    setRestartNotice(null);
    absentSinceRef.current = null;
    // ...and closes any open log view, which additionally tells the previous
    // server's agent to stop streaming. A spec id is server-scoped, so leaving
    // it open would at best show nothing and at worst keep the old server's
    // agent producing output for a view that has moved on.
    setLogSpec(null);
  }, [server.id]);

  // Bounds the wait. The cleanup clears the timer whenever `restart` changes,
  // so this callback can only fire while the flow it captured is still the
  // current one -- no stale notice. `deadline` being absolute keeps the
  // overall bound honest across the phase change that re-arms the timer.
  // `clearing` used to be EXEMPT from this bound, which made it unbounded:
  // api/transport.ts has no AbortController, so a clear PUT that never
  // settled kept `restart !== null` -- and with it `overridesLocked` --
  // forever, disabling every action on every row behind a "clearing…" chip
  // with no escape but a page reload. It is bounded here too now; finishRestart
  // gives that phase its own, much shorter deadline.
  useEffect(() => {
    if (restart === null) return undefined;
    const { specId, phase } = restart;
    const timer = setTimeout(
      () => {
        if (!mountedRef.current) return;
        absentSinceRef.current = null;
        // Give the flow up: a response arriving later must not toast success
        // or reset state behind this notice.
        restartRunRef.current += 1;
        setRestart((cur) => (cur?.specId === specId ? null : cur));
        setRestartNotice(phase === 'clearing' ? 'clear-timeout' : 'timeout');
      },
      Math.max(0, restart.deadline - Date.now()),
    );
    return () => clearTimeout(timer);
  }, [restart]);

  // Advances the sequence from the live stream: a `stopped` frame for THIS
  // spec is the only signal that force_stopped actually took effect.
  useEffect(() => {
    if (restart === null || restart.phase !== 'waiting') return;
    const row = statusRows.find((r) => r.spec_id === restart.specId);
    if (row === undefined) {
      // Absence is not immediately "deleted": the gateway holds runtime
      // status in volatile RAM (spec §7), so a gateway restart empties the
      // list and the next sample (<= 1 s) refills it. Only a SUSTAINED
      // absence is terminal. Re-evaluated per frame, which is enough --
      // frames arrive ~1/s while the stream is up, and if they stop entirely
      // the deadline above catches it.
      const now = Date.now();
      if (absentSinceRef.current === null) {
        absentSinceRef.current = now;
        return;
      }
      if (now - absentSinceRef.current < RESTART_VANISH_GRACE_MS) return;
      absentSinceRef.current = null;
      setRestart(null);
      setRestartNotice('vanished');
      return;
    }
    absentSinceRef.current = null;
    // A TRANSITION, not a state: this effect re-runs the moment the phase
    // flips to `waiting`, and the frame it then sees may well predate the
    // force_stopped write it is supposed to be observing the effect of. The
    // watermark accepts only a strictly later frame -- without it a restart
    // of an already-resting spec fires both PUTs back to back, starts
    // nothing, and reports success.
    if (row.state === 'stopped' && frameSeqRef.current > restart.waitFrom) {
      void finishRestart(restart);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [statusRows, restart]);

  /**
   * The form/delete half of the notice clearing `setOverride` already does
   * (fix round 1, M6).
   *
   * C7 lifted the three restart notices to screen level, and their advice
   * points at the specs tab ("check it and clear the override by hand"). But
   * nothing on the specs tab cleared them, and the form sub-view early-returns
   * without the banner stack -- so the operator did exactly what the notice
   * said, saved, and landed back on the specs tab with that same notice as the
   * first thing they saw. A spec write that lands NO override, and a delete
   * that removes the spec (or the mapping) outright, are both that remediation.
   *
   * Not correlated to the notice's own mapping, matching `setOverride`, which
   * clears on an override action taken on ANY row: the notice carries no
   * mapping id, and "the operator has just resolved an override by hand" is the
   * fact both call sites are reading off. A save that leaves an override IN
   * place (`admin_state` set in the form) deliberately does not clear it.
   */
  function clearRestartNoticeAfter(spec: RuntimeSpec) {
    if (spec.admin_state === '') setRestartNotice(null);
  }

  async function setOverride(spec: RuntimeSpec, adminState: string) {
    // A timeout/aborted notice tells the operator an override was left in
    // place; acting on any override is exactly the moment it stops being
    // news, so it is cleared here rather than lingering until the next
    // restart or a server switch.
    setRestartNotice(null);
    const run = ++overrideRunRef.current;
    const ticket = beginSpecWrite(spec.mapping_id);
    setOverrideBusy(true);
    // Bounds the lock. `overrideBusy` disables EVERY action on EVERY row, and
    // nothing else in the stack ever gives up on the request (no
    // AbortController in api/transport.ts), so without this a single PUT that
    // never settles freezes the whole table until the page is reloaded.
    const watchdog = setTimeout(() => {
      if (!mountedRef.current || overrideRunRef.current !== run) return;
      overrideRunRef.current += 1; // abandon: a late response must stay silent
      setOverrideBusy(false);
      showError(t.runtimeWriteTimeout);
    }, OVERRIDE_WRITE_TIMEOUT_MS);
    try {
      const updated = await api.putRuntimeSpec(
        spec.mapping_id,
        specBodyWithAdminState(spec, adminState),
      );
      if (!mountedRef.current) return;
      // ABOVE the run-token guard, deliberately. `specsById` is a cache of
      // server truth with no user-visible side effect of its own, and the
      // server has just told us what it now holds. Behind the token, a write
      // abandoned by its watchdog that then resolved left the cache stale
      // forever: the row kept `admin_state: ''`, so it offered Force stop and
      // Restart and NO Clear override, while the server actually held
      // force_stopped -- and the timeout notice was simultaneously telling the
      // operator to clear an override by hand that the row denied existed.
      // The toast and the lock release below stay behind the token: those
      // ARE user-visible, and an abandoned flow must not resurrect them.
      // The per-mapping TICKET is a different guard from the run token: it
      // drops this payload only when a LATER write to the same mapping has
      // already COMMITTED one, i.e. only when it is genuinely stale. Not when
      // a later write merely STARTED -- a later write that failed writes
      // nothing, and discarding this payload for it re-opens N1 (see the
      // ticket block's own comment).
      commitSpecCache(spec.mapping_id, ticket, updated);
      if (overrideRunRef.current !== run) return;
      showSuccess(t.systemSaved);
    } catch (err) {
      if (mountedRef.current && overrideRunRef.current === run)
        showError(formatPortalError(err, t));
    } finally {
      clearTimeout(watchdog);
      if (mountedRef.current && overrideRunRef.current === run) setOverrideBusy(false);
    }
  }

  async function startRestart(specId: string, spec: RuntimeSpec) {
    if (restart !== null) return;
    // Re-assert the state gate here, not only in the `disabled` prop. That
    // prop is evaluated at render time and the onClick closure carries the row
    // from that same render, so a frame landing between the commit and the
    // click's dispatch (they arrive ~1/s) let a `stopped` /
    // `pending_vram_unknown` / `not_permitted` row start the sequence anyway --
    // the exact harm the gate exists to prevent: a force_stopped override that
    // no `stopped` frame can ever clear, leaving the model admission-blocked
    // behind a timeout notice. Read the LIVE frame, not the captured row.
    const live = statusRowsRef.current.find((r) => r.spec_id === specId);
    if (live === undefined || !restartableStates.has(live.state)) return;
    const run = ++restartRunRef.current;
    const ticket = beginSpecWrite(spec.mapping_id);
    absentSinceRef.current = null;
    setRestartNotice(null);
    setRestart({
      specId,
      mappingId: spec.mapping_id,
      phase: 'stopping',
      deadline: Date.now() + RESTART_STOP_TIMEOUT_MS,
      waitFrom: frameSeqRef.current,
    });
    try {
      const updated = await api.putRuntimeSpec(
        spec.mapping_id,
        specBodyWithAdminState(spec, 'force_stopped'),
      );
      if (!mountedRef.current) return;
      // Read the watermark HERE, not inside the updater below: the updater may
      // run a render later, by which time another frame could have arrived and
      // a legitimate `stopped` transition would be skipped. What we want is
      // "frames from after this write landed", which is exactly now.
      const waitFrom = frameSeqRef.current;
      // Above the run-token guard for the reason spelled out in setOverride:
      // the cache must not stay stale just because the sequence this write
      // belonged to was abandoned. Everything below IS user-visible and stays
      // behind the token.
      commitSpecCache(spec.mapping_id, ticket, updated);
      if (restartRunRef.current !== run) return;
      setRestart((cur) => (cur?.specId === specId ? { ...cur, phase: 'waiting', waitFrom } : cur));
    } catch (err) {
      if (!mountedRef.current || restartRunRef.current !== run) return;
      showError(formatPortalError(err, t));
      setRestart((cur) => (cur?.specId === specId ? null : cur));
    }
  }

  async function finishRestart(flow: RestartFlow) {
    const run = restartRunRef.current;
    // Re-read the spec by MAPPING id (captured when the flow started), not by
    // the stream's spec id: the clear PUT must not depend on the spec-id join
    // still resolving, and it must still be the actual stored document.
    const spec = specsById[flow.mappingId];
    if (spec === undefined) {
      setRestart(null);
      setRestartNotice('vanished');
      return;
    }
    // Its own, much shorter deadline: from here the sequence waits on one
    // HTTP round trip writing one document, not on a process lifecycle.
    setRestart({
      ...flow,
      phase: 'clearing',
      deadline: Date.now() + OVERRIDE_WRITE_TIMEOUT_MS,
    });
    const ticket = beginSpecWrite(flow.mappingId);
    try {
      const updated = await api.putRuntimeSpec(flow.mappingId, specBodyWithAdminState(spec, ''));
      if (!mountedRef.current) return;
      // Same reasoning as the other two writes: an abandoned clear PUT that
      // then succeeds must still refresh the cache, or the row goes on
      // offering a Clear override that is already cleared and hiding the
      // Restart that is now available again.
      commitSpecCache(flow.mappingId, ticket, updated);
      if (restartRunRef.current !== run) return;
      showSuccess(t.systemSaved);
    } catch (err) {
      if (mountedRef.current && restartRunRef.current === run) showError(formatPortalError(err, t));
    } finally {
      if (mountedRef.current && restartRunRef.current === run) setRestart(null);
    }
  }

  // ---- create/edit form state -----------------------------------------
  const [gatewayName, setGatewayName] = useState('');
  const [appName, setAppName] = useState('');
  const [enabled, setEnabled] = useState(true);
  const [binary, setBinary] = useState('');
  const [argsText, setArgsText] = useState('');
  const [envText, setEnvText] = useState('');
  const [workDir, setWorkDir] = useState('');
  const [listenPort, setListenPort] = useState(0);
  const [healthPath, setHealthPath] = useState('/health');
  const [healthTimeoutSeconds, setHealthTimeoutSeconds] = useState(5);
  const [startupTimeoutSeconds, setStartupTimeoutSeconds] = useState(180);
  const [idleTimeoutSeconds, setIdleTimeoutSeconds] = useState(0);
  const [admissionWaitTimeoutSeconds, setAdmissionWaitTimeoutSeconds] = useState(0);
  const [pinned, setPinned] = useState(false);
  const [adminState, setAdminState] = useState('');
  const [vramLocked, setVramLocked] = useState(false);
  const [setVisibleDevices, setSetVisibleDevices] = useState(false);
  const [gpuRows, setGpuRows] = useState<GpuRow[]>([]);

  function resetSpecFields() {
    setEnabled(true);
    setBinary('');
    setArgsText('');
    setEnvText('');
    setWorkDir('');
    setListenPort(0);
    setHealthPath('/health');
    setHealthTimeoutSeconds(5);
    setStartupTimeoutSeconds(180);
    setIdleTimeoutSeconds(0);
    setAdmissionWaitTimeoutSeconds(0);
    setPinned(false);
    setAdminState('');
    setVramLocked(false);
    setSetVisibleDevices(false);
    setGpuRows([]);
  }

  function hydrateSpecFields(spec: RuntimeSpec) {
    setEnabled(spec.enabled);
    setBinary(spec.binary);
    setArgsText(formatArgsText(spec.args));
    setEnvText(formatEnvText(spec.env));
    setWorkDir(spec.work_dir);
    setListenPort(spec.listen_port);
    setHealthPath(spec.health_path || '/health');
    setHealthTimeoutSeconds(spec.health_timeout_seconds || 5);
    setStartupTimeoutSeconds(spec.startup_timeout_seconds || 180);
    setIdleTimeoutSeconds(spec.idle_timeout_seconds);
    setAdmissionWaitTimeoutSeconds(spec.admission_wait_timeout_seconds);
    setPinned(spec.pinned);
    setAdminState(spec.admin_state);
    setVramLocked(spec.vram_locked);
    setSetVisibleDevices(spec.set_visible_devices);
    setGpuRows(
      spec.gpus.map((g) => ({
        rowKey: makeRowKey(),
        index: g.index,
        vramEstimateMb: g.vram_estimate_mb,
        vramMeasuredMb: g.vram_measured_mb,
      })),
    );
  }

  function openCreate() {
    setGatewayName('');
    setAppName('');
    resetSpecFields();
    setSpecMode('create');
  }

  // Re-reads the spec fresh (rather than trusting the bulk-loaded map, which
  // may not have settled yet for a just-rendered row) so the full-document
  // PUT this form issues on save never clobbers fields it never saw.
  //
  // `hydrateSpecFields` deliberately uses this GET's OWN payload even when the
  // cache refuses it: the form is about to PUT that document back, so it must
  // show what it will send. The stale FORM is the etag problem this cache
  // cannot solve; the stale CACHE is not (fix round 1, C2).
  async function openEdit(mapping: PortalModelMapping) {
    setGatewayName(mapping.gateway_model_name);
    setAppName(mapping.app_model_name);
    setLoadingEditFor(mapping.id);
    const seen = beginSpecRead(mapping.id);
    try {
      const spec = await api.runtimeSpec(mapping.id);
      commitSpecRead(mapping.id, seen, spec);
      loadedIdsRef.current.add(mapping.id);
      // This GET doubles as the RETRY for a lazy read that failed: the fan-out
      // never re-fetches (`loadedIdsRef`), so without this a mapping whose one
      // spec GET rejected would stay unknown -- and its Delete gated -- for the
      // rest of the mount, with no way back. Cleared even if `commitSpecRead`
      // refused this payload, because a refusal means a LATER write already
      // committed for this mapping: either way the state is no longer unknown.
      setSpecReadFailedIds((cur) => {
        if (!cur.has(mapping.id)) return cur;
        const next = new Set(cur);
        next.delete(mapping.id);
        return next;
      });
      hydrateSpecFields(spec);
      setSpecMode({ kind: 'edit', mapping });
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setLoadingEditFor('');
    }
  }

  function addGpuRow() {
    setGpuRows((rows) => {
      // rows.length collides with an existing index once a row has been
      // removed (e.g. remove row 0 of two -> the survivor already holds
      // index 1, but rows.length is now 1 too) -- propose the lowest index
      // not already in use instead.
      const used = new Set(rows.map((r) => r.index));
      let index = 0;
      while (used.has(index)) index++;
      return [...rows, { rowKey: makeRowKey(), index, vramEstimateMb: 0, vramMeasuredMb: 0 }];
    });
  }
  function removeGpuRow(idx: number) {
    setGpuRows((rows) => rows.filter((_, i) => i !== idx));
  }
  // Deliberately does NOT reject a collision as it is typed: an operator
  // swapping two rows' indices has to pass through a colliding intermediate
  // state, and refusing the keystroke would make that edit impossible. The
  // collision is caught at submit instead (duplicateGpuIndex), which is also
  // where the backend's own refusal would land.
  function updateGpuRow(idx: number, patch: Partial<Pick<GpuRow, 'index' | 'vramEstimateMb'>>) {
    setGpuRows((rows) => rows.map((r, i) => (i === idx ? { ...r, ...patch } : r)));
  }

  // The reported GPUs row `rowIdx` may be switched to: every telemetry card
  // except the ones a SIBLING row already holds. duplicateGpuIndex refuses a
  // collision at submit and the backend refuses it again, so offering an index
  // only to fail validation afterwards is worse than not offering it.
  //
  // The row's OWN index is deliberately still in the list -- that is what lets
  // the select display the current selection rather than reading as unset.
  // This does NOT tighten updateGpuRow, which still accepts a typed collision
  // on purpose: swapping two rows' indices has to pass through a colliding
  // intermediate state, and refusing the keystroke would make that edit
  // impossible. The picker simply never OFFERS one.
  function gpuOptionsFor(rowIdx: number): HardwareGPU[] {
    const takenBySiblings = new Set(gpuRows.filter((_, i) => i !== rowIdx).map((r) => r.index));
    return telemetryGpus.filter((g) => !takenBySiblings.has(g.index));
  }

  // The select shows a card only when the row's index actually matches one the
  // server reported. A hand-typed index for a card telemetry does not know
  // about (a machine that has not reported yet, a card about to be installed)
  // leaves the select on its placeholder while the numeric field keeps the
  // real value -- honest in both halves, rather than the select silently
  // implying the index is something else.
  function gpuSelectValue(row: GpuRow, rowIdx: number): string {
    return gpuOptionsFor(rowIdx).some((g) => g.index === row.index) ? String(row.index) : '';
  }

  function buildSpecBody(args: string[], env: Record<string, string>): PutRuntimeSpecRequest {
    return {
      enabled,
      binary: binary.trim(),
      args,
      env,
      work_dir: workDir.trim(),
      listen_port: listenPort,
      health_path: healthPath.trim(),
      health_timeout_seconds: healthTimeoutSeconds,
      startup_timeout_seconds: startupTimeoutSeconds,
      idle_timeout_seconds: idleTimeoutSeconds,
      admission_wait_timeout_seconds: admissionWaitTimeoutSeconds,
      pinned,
      admin_state: adminState,
      vram_locked: vramLocked,
      set_visible_devices: setVisibleDevices,
      gpus: gpuRows.map((r) => ({
        index: r.index,
        vram_estimate_mb: r.vramEstimateMb,
        vram_measured_mb: r.vramMeasuredMb,
      })),
    };
  }

  async function submitCreate(event: SubmitEvent<HTMLFormElement>) {
    event.preventDefault();
    const args = parseArgsText(argsText);
    const parsedEnv = parseEnvText(envText, t);
    if (parsedEnv.error) {
      showError(parsedEnv.error);
      return;
    }
    const placeholderError = validatePlaceholders([...args, ...Object.values(parsedEnv.env)], t);
    if (placeholderError) {
      showError(placeholderError);
      return;
    }
    const duplicateGpu = duplicateGpuIndex(gpuRows.map((r) => r.index));
    if (duplicateGpu !== null) {
      showError(`${t.runtimeGpuIndexDuplicate}: GPU ${duplicateGpu}`);
      return;
    }
    const visibleDevicesError = validateVisibleDevices(
      setVisibleDevices,
      parsedEnv.env,
      gpuRows.length,
      t,
    );
    if (visibleDevicesError) {
      showError(visibleDevicesError);
      return;
    }
    setBusy(true);
    let mapping: PortalModelMapping;
    try {
      // No `status`: the mapping owns it and the Modell-Zuordnung tab edits it.
      // CreateMapping normalises an absent status to active, byte-for-byte what
      // the removed hard-coded 'active' produced -- so this form carries ONE
      // rule (it never sends status) rather than a create/edit special case.
      //
      // The gateway name, by contrast, MUST be sent here and must stay
      // editable: this call creates the mapping whose id keys the spec PUT
      // below, the backend refuses an empty gateway name, and this form is the
      // only mapping-create path a server_agent application has.
      mapping = await api.createMapping(application.id, {
        gateway_model_name: gatewayName,
        app_model_name: appName,
      });
      setMappings((current) => [mapping, ...(current ?? [])]);
      // Pre-seeding `loadedIdsRef` keeps the lazy loader off a row we are about
      // to write ourselves -- but `specLoadSettled`'s only writer is that
      // loader's `finally`, which the pre-seed then skips. Without this line
      // `specsSettled` was false for the rest of the mount the moment anyone
      // created a mapping, so the `Unmatched` chip vanished from every
      // genuinely unresolved status row (fix round 1, C3). This id's spec state
      // IS known either way: committed below on success, or genuinely absent
      // after the partial failure -- which is what "settled" means.
      loadedIdsRef.current.add(mapping.id);
      setSpecLoadSettled((cur) => new Set(cur).add(mapping.id));
    } catch (err) {
      showError(formatPortalError(err, t));
      setBusy(false);
      return;
    }
    // The mapping now exists regardless of what happens next -- a failure
    // from here on is reported as a partial failure, never silently, so the
    // operator knows the spec (not the mapping) needs another attempt.
    const ticket = beginSpecWrite(mapping.id);
    try {
      const spec = await api.putRuntimeSpec(mapping.id, buildSpecBody(args, parsedEnv.env));
      commitSpecCache(mapping.id, ticket, spec);
      clearRestartNoticeAfter(spec);
      void reloadWarnings();
      setSpecMode('list');
    } catch (err) {
      showError(`${t.runtimeSpecPartialFailure}: ${formatPortalError(err, t)}`);
      setSpecMode('list');
    } finally {
      setBusy(false);
    }
  }

  async function submitEdit(event: SubmitEvent<HTMLFormElement>) {
    event.preventDefault();
    if (typeof specMode === 'string' || specMode.kind !== 'edit') return;
    const id = specMode.mapping.id;
    const args = parseArgsText(argsText);
    const parsedEnv = parseEnvText(envText, t);
    if (parsedEnv.error) {
      showError(parsedEnv.error);
      return;
    }
    const placeholderError = validatePlaceholders([...args, ...Object.values(parsedEnv.env)], t);
    if (placeholderError) {
      showError(placeholderError);
      return;
    }
    const duplicateGpu = duplicateGpuIndex(gpuRows.map((r) => r.index));
    if (duplicateGpu !== null) {
      showError(`${t.runtimeGpuIndexDuplicate}: GPU ${duplicateGpu}`);
      return;
    }
    const visibleDevicesError = validateVisibleDevices(
      setVisibleDevices,
      parsedEnv.env,
      gpuRows.length,
      t,
    );
    if (visibleDevicesError) {
      showError(visibleDevicesError);
      return;
    }
    setBusy(true);
    try {
      // A form does not send a field it does not let you edit. Read-only here
      // => not sent from here: the gateway name and the status belong to the
      // mapping and are edited on the Modell-Zuordnung tab, and this form owns
      // only `app_model_name` (the spec's upstream_model).
      //
      // OMITTED, not echoed. The PATCH is pointer-gated per field, so an absent
      // key leaves that field byte-for-byte alone -- which is what makes the two
      // screens non-overlapping writers instead of two writers racing. Echoing
      // `status` here would be worse than redundant: it is a snapshot taken when
      // this form opened, so a spec save would silently PATCH a deliberately
      // DISABLED model back into service, with no error and no column on the
      // specs tab that contradicts it. (The gateway name is the milder case --
      // no 409 hazard, the conflict check self-excludes -- but the same
      // lost-update argument applies, so it goes for the same reason.)
      const updated = await api.updateMapping(id, { app_model_name: appName });
      setMappings((current) => (current ?? []).map((m) => (m.id === id ? updated : m)));
    } catch (err) {
      showError(formatPortalError(err, t));
      setBusy(false);
      return;
    }
    const ticket = beginSpecWrite(id);
    try {
      const spec = await api.putRuntimeSpec(id, buildSpecBody(args, parsedEnv.env));
      commitSpecCache(id, ticket, spec);
      clearRestartNoticeAfter(spec);
      void reloadWarnings();
      setSpecMode('list');
    } catch (err) {
      showError(`${t.runtimeSpecPartialFailure}: ${formatPortalError(err, t)}`);
      setSpecMode('list');
    } finally {
      setBusy(false);
    }
  }

  /**
   * Whether this mapping's spec state is a KNOWN fact rather than an open
   * question.
   *
   * "No entry in `specsById`" means BOTH things at once: it is what a settled
   * read of a mapping that has no spec looks like, and it is equally what a
   * read that never answered looks like -- a rejected GET never lands in the
   * cache, and a still-running one has not landed yet. Conflating the two is
   * not a cosmetic slip, because one of them is the licence to delete the
   * MAPPING (see `deleteMeaning`).
   */
  function specStateKnown(mappingId: string): boolean {
    // An entry is an answer, whoever wrote it: a lazy GET, `openEdit`'s own
    // GET, or one of our own spec writes (a spec delete commits `emptySpec`).
    if (specsById[mappingId] !== undefined) return true;
    // No entry, so the only remaining answer is a read that came back empty
    // handed on purpose -- settled AND not settled by failing. `submitCreate`
    // seeds both sets for a mapping it created, whose spec PUT may have failed:
    // no entry, and yet genuinely no spec.
    return specLoadSettled.has(mappingId) && !specReadFailedIds.has(mappingId);
  }

  /**
   * What this row's single Delete means -- decided in ONE place, because the
   * defect this replaces was two places agreeing on a guess: the label was
   * `specsById[id]?.configured` and `confirmDelete` then chose its endpoint
   * from the same expression, with nothing anywhere saying whether that
   * expression was even answerable yet.
   *
   * `'mapping'` -- the strictly LARGER operation, which destroys the model
   * route and not just its launch configuration -- is returned only when we
   * know there is no spec. An unknown state falls to `'spec'`, the smaller of
   * the two, so that even a stale render that somehow got past `disabled`
   * cannot delete a route the operator never asked to delete.
   */
  function deleteMeaning(mappingId: string): 'spec' | 'mapping' | 'unknown' {
    if (specsById[mappingId]?.configured) return 'spec';
    return specStateKnown(mappingId) ? 'mapping' : 'unknown';
  }

  const confirmIsSpecDelete = confirmingDeleteId
    ? deleteMeaning(confirmingDeleteId) !== 'mapping'
    : false;

  async function confirmDelete() {
    const id = confirmingDeleteId;
    if (!id) return;
    try {
      if (confirmIsSpecDelete) {
        // A delete is a write to the same cache entry, so it takes a ticket
        // too: an override PUT still in flight for this mapping must not
        // resurrect the spec that has just been deleted.
        const ticket = beginSpecWrite(id);
        await api.deleteRuntimeSpec(id);
        const deleted = emptySpec(id);
        commitSpecCache(id, ticket, deleted);
        clearRestartNoticeAfter(deleted);
        void reloadWarnings();
      } else {
        // The other half of the same rule (fix round 1, M5): dropping the
        // entry is a write to it too. Without a ticket a spec write still in
        // flight for this mapping resurrected it -- reachable because the
        // create form's spec PUT has no watchdog and `busy` disables only
        // Submit, so Cancel returns to a list that already holds the new
        // mapping with nothing cached for it, i.e. to THIS branch. The status
        // row then offered four override actions that all PUT to a mapping id
        // that no longer exists, on a row `mappingForStatus` could not name.
        const ticket = beginSpecWrite(id);
        await api.deleteMapping(id);
        setMappings((current) => (current ?? []).filter((m) => m.id !== id));
        forgetSpecCache(id, ticket);
        loadedIdsRef.current.delete(id);
        clearRestartNoticeAfter(emptySpec(id));
      }
      setConfirmingDeleteId('');
    } catch (err) {
      showError(formatPortalError(err, t));
    }
  }

  // ---- model-mapping tab ------------------------------------------------
  //
  // WHO OWNS WHAT, and why this tab exists at all. A mapping and its launch
  // spec are two documents with one id, and the two model names are NOT
  // interchangeable:
  //
  //   the MAPPING owns `gateway_model_name` (the name the gateway routes and
  //     offers) and `status` (whether it routes it at all -- the runtime-config
  //     document never reads that field);
  //   the RUNTIME SPEC owns `app_model_name`, because it IS the spec's
  //     `upstream_model` -- the only thing `${MODEL}` expands to when the agent
  //     builds the process argv, and re-keying it under a live process points
  //     the agent's upstream route at a name that process does not serve.
  //
  // So each screen shows both names and edits only its own. Read-only is that
  // boundary, not a convenience: nothing server-side enforces it (the mapping
  // PATCH accepts all three fields from anyone, and the spec PUT accepts none
  // of them), so re-enabling a field here silently recreates two writers of one
  // value with no error to show for it.
  const mappingRowActions = (row: PortalModelMapping): RowAction[] => [
    {
      key: 'edit',
      label: t.mappingEdit,
      icon: <EditIcon fontSize="small" />,
      onClick: () => setMappingEdit(row),
    },
    {
      key: 'toggle',
      label: row.status === 'active' ? t.tokenActionDisable : t.tokenActionEnable,
      icon:
        row.status === 'active' ? (
          <BlockIcon fontSize="small" />
        ) : (
          <CheckCircleIcon fontSize="small" />
        ),
      onClick: () => void toggleMappingStatus(row),
    },
    // No Delete, and not only because it was asked for: `agent_runtime_specs`
    // references the mapping ON DELETE CASCADE, so deleting a row here would
    // silently destroy a launch spec the operator never saw. The specs tab
    // already answers that question properly (`deleteMeaning` refuses to guess
    // while the spec state is unknown); a second, dumber answer beside it is
    // worse than none.
  ];

  async function toggleMappingStatus(row: PortalModelMapping) {
    const next: ApplicationStatus = row.status === 'active' ? 'disabled' : 'active';
    try {
      const updated = await api.updateMapping(row.id, { status: next });
      setMappings((current) => (current ?? []).map((m) => (m.id === row.id ? updated : m)));
    } catch (err) {
      showError(formatPortalError(err, t));
    }
  }

  async function saveMappingFromTab(id: string, values: MappingFormValues) {
    setBusy(true);
    try {
      // Read-only here => not sent from here. `app_model_name` belongs to the
      // runtime spec (see the ownership note above) and the mapping PATCH is
      // pointer-gated per field, so an ABSENT key leaves it byte-for-byte
      // alone. Re-stating the value we happen to hold is not equivalent: it is
      // a snapshot from when this form opened, and it would overwrite a spec
      // edit made in between with no error and nothing on screen to show it.
      const body: UpdateMappingRequest = { ...values };
      delete body.app_model_name;
      const updated = await api.updateMapping(id, body);
      setMappings((current) => (current ?? []).map((m) => (m.id === id ? updated : m)));
      setMappingEdit(null);
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setBusy(false);
    }
  }

  const statusBySpecId = new Map(statusRows.map((row) => [row.spec_id, row]));
  function statusForMapping(m: PortalModelMapping): RuntimeStatus | undefined {
    const specId = specsById[m.id]?.id;
    return specId ? statusBySpecId.get(specId) : undefined;
  }
  function statusLabelForMapping(m: PortalModelMapping): string {
    const live = statusForMapping(m);
    return live ? runtimeStateLabel(live.state, t) : t.runtimeStatusUnknown;
  }

  const columns: ListColumn<PortalModelMapping>[] = [
    {
      id: 'gateway',
      label: t.mappingGatewayName,
      value: (m) => m.gateway_model_name,
      filter: 'text',
    },
    { id: 'app', label: t.mappingAppName, value: (m) => m.app_model_name, filter: 'text' },
    {
      id: 'enabled',
      label: t.runtimeSpecEnabled,
      value: (m) => (specsById[m.id]?.enabled ? 'yes' : 'no'),
      filter: 'enum',
      searchable: false,
      enumLabel: (v) => (v === 'yes' ? t.runtimeSpecEnabled : '–'),
      render: (m) => renderBoolChip(Boolean(specsById[m.id]?.enabled), t.runtimeSpecEnabled),
    },
    {
      id: 'binary',
      label: t.runtimeSpecBinary,
      value: (m) => basename(specsById[m.id]?.binary ?? ''),
      filter: 'text',
    },
    {
      id: 'gpus',
      label: t.runtimeSpecGpus,
      value: (m) => formatGpus(specsById[m.id]?.gpus ?? []),
      filter: 'text',
      searchable: false,
    },
    {
      // Deliberately adjacent to the GPU column: together the two say whether
      // the cards listed there are what the process actually gets, or only
      // what the admission arithmetic believes it gets.
      id: 'set_visible_devices',
      label: t.runtimeSpecSetVisibleDevices,
      value: (m) => (specsById[m.id]?.set_visible_devices ? 'yes' : 'no'),
      filter: 'enum',
      searchable: false,
      enumLabel: (v) => (v === 'yes' ? t.runtimeSpecSetVisibleDevices : '–'),
      render: (m) =>
        renderBoolChip(
          Boolean(specsById[m.id]?.set_visible_devices),
          t.runtimeSpecSetVisibleDevices,
        ),
    },
    {
      id: 'pinned',
      label: t.runtimeSpecPinned,
      value: (m) => (specsById[m.id]?.pinned ? 'yes' : 'no'),
      filter: 'enum',
      searchable: false,
      enumLabel: (v) => (v === 'yes' ? t.runtimeSpecPinned : '–'),
      render: (m) => renderBoolChip(Boolean(specsById[m.id]?.pinned), t.runtimeSpecPinned),
    },
    {
      id: 'idle_timeout',
      label: t.runtimeSpecIdleTimeout,
      value: (m) => formatMetric(specsById[m.id]?.idle_timeout_seconds, 0),
      filter: 'text',
      numeric: true,
    },
    {
      // Area 4's live state joined back into area 1's list by SPEC id -- the
      // stream's own key. A mapping with no configured spec, or a configured
      // spec the agent has never reported, is "unknown": deliberately NOT
      // "stopped", which would be a claim we cannot make.
      id: 'live_status',
      // NOT `t.tableStatus` any more: the tab immediately to the left now
      // carries a "Status" column meaning the MAPPING's active/disabled, while
      // this one means the process's running/stopped/unknown. Two adjacent tabs
      // must not label two different facts with one word. The column `id` is
      // unchanged, so persisted column preferences survive.
      label: t.runtimeLiveStatus,
      value: (m) => statusLabelForMapping(m),
      filter: 'enum',
      sortable: false,
      searchable: false,
      render: (m) => {
        const live = statusForMapping(m);
        return live ? (
          <StatusChip
            status={runtimeStateBadge(live.state)}
            label={runtimeStateLabel(live.state, t)}
          />
        ) : (
          <StatusChip status="standby" label={t.runtimeStatusUnknown} />
        );
      },
    },
  ];

  const rowActions = (m: PortalModelMapping): RowAction[] => {
    const meaning = deleteMeaning(m.id);
    const rowBusy = loadingEditFor === m.id;
    // Why the Delete is locked, which is a different sentence in each of the
    // two unknown states: one resolves itself, the other needs the operator.
    let deleteBlocked: string | undefined;
    if (meaning === 'unknown') {
      deleteBlocked = specReadFailedIds.has(m.id)
        ? t.runtimeSpecDeleteStateUnknown
        : t.runtimeSpecDeleteStateLoading;
    }
    return [
      {
        // Left ungated on purpose: it is non-destructive, and its own GET is
        // what re-answers the question for a row whose lazy read failed.
        key: 'edit',
        label: t.runtimeSpecEditAction,
        icon: <EditIcon fontSize="small" />,
        onClick: () => void openEdit(m),
        disabled: rowBusy,
      },
      {
        // The label is not decoration: `confirmDelete` performs whatever it
        // says, so a Delete labelled from a guess IS a delete performed from a
        // guess -- and the guess used to default to destroying the mapping
        // during the whole per-row fan-out, and permanently on any row whose
        // spec GET failed. Offered-but-locked instead, with the reason in the
        // tooltip: `disabled` alone would be a dead control with no account of
        // itself, and `rowBusy` never covered this window at all.
        key: 'delete',
        label: meaning === 'mapping' ? t.mappingDelete : t.runtimeSpecDelete,
        color: 'error',
        icon: <DeleteIcon fontSize="small" />,
        onClick: () => setConfirmingDeleteId(m.id),
        disabled: rowBusy || meaning === 'unknown',
        title: deleteBlocked,
      },
    ];
  };

  // `last_error` is cleared ONLY by the next successful start, never by a
  // state change (server-agent/internal/runtime/types.go) -- so a `stopped`
  // spec can still carry "last attempt failed yesterday 14:32, exit code 1".
  // That makes "last load failed" a fact no state chip can ever convey, hence
  // its own column, always visible (the message inline, the details in the
  // tooltip), on every state including stopped.
  function renderLastError(err: RuntimeError | undefined) {
    if (!err) return '–';
    return (
      <Tooltip
        title={
          <Box sx={{ display: 'grid', gap: 0.25 }}>
            <Typography variant="caption">
              {t.runtimeLastErrorAt}: {formatDate(err.at, '—')}
            </Typography>
            <Typography variant="caption">
              {t.runtimeLastErrorExitCode}: {err.exit_code}
            </Typography>
            <Typography variant="caption">
              {t.runtimeLastErrorFailures}: {err.failures}
            </Typography>
            {err.stderr_tail ? (
              <>
                <Typography variant="caption">{t.runtimeLastErrorStderr}:</Typography>
                <Box
                  component="pre"
                  sx={{ m: 0, whiteSpace: 'pre-wrap', wordBreak: 'break-all', fontSize: 12 }}
                >
                  {err.stderr_tail}
                </Box>
              </>
            ) : null}
          </Box>
        }
      >
        <Box sx={{ display: 'flex', gap: 0.5, alignItems: 'center' }}>
          <WarningAmberIcon fontSize="small" color="warning" />
          <Typography variant="body2">{err.message}</Typography>
        </Box>
      </Tooltip>
    );
  }

  const statusColumns: ListColumn<RuntimeStatus>[] = [
    {
      id: 'model',
      label: t.tableModel,
      // The GATEWAY model name is the primary label, resolved backwards
      // through the spec the stream's spec_id keys (spec_id ->
      // RuntimeSpec.mapping_id -> mapping.gateway_model_name -- no extra
      // fetch, both halves are already loaded for area 1). It is the only name
      // that appears anywhere else in the portal, and area 1 of THIS screen
      // labels its rows with it while joining to this table by spec_id: two
      // tabs naming the same process differently, with no visible bridge, is
      // worse than the disagreement that showing only the agent's name would
      // expose. So both are shown -- gateway name on top, the agent's own
      // upstream name beneath it -- with an explicit marker when they differ.
      //
      // The fallback is REACHABLE, not theoretical, because this stream is
      // SERVER-scoped while the spec/mapping join is APPLICATION-scoped. Two
      // causes survive on a healthy deployment:
      //
      //   - a spec deleted here while the agent still runs it: the agent keeps
      //     reporting that spec_id until its next config sync, and the mapping
      //     it resolved through is already gone;
      //   - the fan-out race on mount: the stream can deliver a snapshot
      //     before every per-mapping spec GET has settled, so the join is
      //     merely INCOMPLETE rather than final (which is why the marker below
      //     waits for `specsSettled` before calling it a fact).
      //
      // A second `server_agent` application on one server is NOT one of them
      // any more, whatever an older version of this comment said: the portal
      // refuses that on both write paths
      // (portal.ErrServerAgentApplicationExists, on create AND on retype),
      // MemoryStore mirrors it, and migration 68 adds a partial unique index
      // on applications(server_id) where type='server_agent'. It survives only
      // on a pre-invariant development database of this branch, where
      // migration 68 deliberately skipped index creation over existing
      // duplicates -- `server_agent` is not a type any released version can
      // write, so no live deployment can be in that state.
      //
      // On such a server most rows land here, and since they get no override
      // actions either (T3's log view is the one action such a row still
      // offers -- reading output is not a write), the row SAYS so rather than
      // leaving the operator to infer it from a nearly empty actions cell.
      //
      // Search/sort key carries every name a searching operator might type.
      value: (row) => [gatewayNameFor(row), row.model, row.spec_id].filter(Boolean).join(' '),
      filter: 'text',
      render: (row) => {
        const mapping = mappingForStatus(row);
        const reported = row.model || row.spec_id;
        if (mapping === undefined) {
          return (
            <Box sx={{ display: 'grid', gap: 0.25, justifyItems: 'start' }}>
              <Typography variant="body2">{reported}</Typography>
              {/* Only once every per-mapping spec GET has settled is this a
                  FACT rather than "not loaded yet" -- two states that would
                  otherwise look identical, with the marker flashing on every
                  ordinary row during the initial fan-out. */}
              {specsSettled && (
                <Tooltip title={t.runtimeStatusUnresolved}>
                  <Chip size="small" variant="outlined" label={t.runtimeStatusUnresolvedShort} />
                </Tooltip>
              )}
            </Box>
          );
        }
        return (
          <Box sx={{ display: 'grid', gap: 0.25 }}>
            <Box sx={{ display: 'flex', gap: 0.5, alignItems: 'center' }}>
              <Typography variant="body2">{mapping.gateway_model_name}</Typography>
              {/* The agent reports the UPSTREAM model name, so the meaningful
                  comparison is against the mapping's own app_model_name --
                  not against the gateway name, which is a different name by
                  design. A disagreement here means the spec launches a
                  different model than the mapping routes to. */}
              {row.model !== '' && row.model !== mapping.app_model_name && (
                <WarningAmberIcon
                  fontSize="small"
                  color="warning"
                  titleAccess={t.runtimeStatusNameMismatch}
                />
              )}
            </Box>
            <Typography variant="caption" color="text.secondary">
              {t.runtimeStatusUpstream}: {reported}
            </Typography>
          </Box>
        );
      },
    },
    {
      id: 'state',
      label: t.tableStatus,
      value: (row) => runtimeStateLabel(row.state, t),
      filter: 'enum',
      searchable: false,
      render: (row) => (
        <Box sx={{ display: 'flex', gap: 0.75, alignItems: 'center', flexWrap: 'wrap' }}>
          <StatusChip
            status={runtimeStateBadge(row.state)}
            label={runtimeStateLabel(row.state, t)}
          />
          {restart?.specId === row.spec_id && (
            <Chip
              size="small"
              variant="outlined"
              label={
                restart.phase === 'clearing' ? t.runtimeRestartClearing : t.runtimeRestartStopping
              }
            />
          )}
        </Box>
      ),
    },
    {
      id: 'since',
      label: t.runtimeStatusSince,
      value: (row) => formatDate(row.since, '–'),
      searchable: false,
    },
    {
      id: 'pid',
      label: t.runtimeStatusPid,
      value: (row) => (row.pid ? String(row.pid) : '–'),
      numeric: true,
      searchable: false,
    },
    {
      id: 'port',
      label: t.runtimeStatusPort,
      value: (row) => (row.port ? String(row.port) : '–'),
      numeric: true,
      searchable: false,
    },
    {
      id: 'in_flight',
      label: t.runtimeStatusInFlight,
      value: (row) => String(row.in_flight),
      numeric: true,
      searchable: false,
    },
    {
      id: 'restarts',
      label: t.runtimeStatusRestarts,
      value: (row) => String(row.restarts),
      numeric: true,
      searchable: false,
    },
    {
      id: 'last_error',
      label: t.runtimeLastError,
      value: (row) => row.last_error?.message ?? '',
      sortable: false,
      searchable: false,
      render: (row) => renderLastError(row.last_error),
    },
  ];

  function statusActions(row: RuntimeStatus): RowAction[] {
    // The log view comes FIRST, and before every gate below it, deliberately.
    // Reading what a process printed is not a write, so it is available in
    // file mode, before the report GET settles, and on a row this application
    // cannot resolve to one of its own mappings -- all three of which are
    // states an operator can be stuck in with no other way to find out what is
    // happening. It is also exactly the row a `crashed` state sends them to.
    const actions: RowAction[] = [
      {
        key: 'logs',
        label: t.runtimeLogs,
        icon: <ArticleIcon fontSize="small" />,
        onClick: () =>
          setLogSpec({
            specId: row.spec_id,
            title: gatewayNameFor(row) || row.model || row.spec_id,
          }),
      },
    ];
    // File mode has no admin override at all (the override lives in the
    // gateway document, which a file-mode agent never consumes -- spec §10.2:
    // "a dead button is worse than none"), and before the report GET settles
    // we do not yet know which mode this is.
    if (!writesAllowed) return actions;
    const spec = specByRuntimeId.get(row.spec_id);
    // No loaded spec for this spec_id (a spec created after the list loaded, a
    // spec belonging to another application, a file-mode leftover): render NO
    // buttons rather than synthesizing a full-document body that would
    // overwrite the operator's command line.
    if (spec === undefined) return actions;
    if (spec.admin_state !== 'force_running') {
      actions.push({
        key: 'force-start',
        label: t.runtimeForceStart,
        icon: <PlayArrowIcon fontSize="small" />,
        disabled: overridesLocked,
        onClick: () => void setOverride(spec, 'force_running'),
      });
    }
    if (spec.admin_state !== 'force_stopped') {
      actions.push({
        key: 'force-stop',
        label: t.runtimeForceStop,
        icon: <StopIcon fontSize="small" />,
        disabled: overridesLocked,
        onClick: () => void setOverride(spec, 'force_stopped'),
      });
    }
    if (spec.admin_state !== '') {
      actions.push({
        key: 'clear-override',
        label: t.runtimeClearOverride,
        icon: <ClearIcon fontSize="small" />,
        disabled: overridesLocked,
        onClick: () => void setOverride(spec, ''),
      });
    }
    // A restart ENDS with no override (force_stopped -> clear), so offering it
    // on a row that already carries one would silently drop that override --
    // the operator asked for a restart, not for their force_running to be
    // forgotten. While this row's own sequence runs the action stays visible
    // but disabled, so it is obvious why nothing else responds.
    if (spec.admin_state === '' || restart?.specId === row.spec_id) {
      const restartBlocked = !restartableStates.has(row.state);
      actions.push({
        key: 'restart',
        label: t.runtimeRestart,
        icon: <ReplayIcon fontSize="small" />,
        // The state gate (see restartableStates): on `stopped`,
        // `pending_vram_unknown` and `not_permitted` the agent can never
        // report the `stopped` transition this sequence waits for, so
        // clicking here would spin for two minutes, report a timeout, and
        // leave the model admission-blocked behind a force_stopped override.
        // Kept VISIBLE rather than hidden -- the row's own state is the
        // explanation, and an action that silently comes and goes reads like
        // a portal bug -- but never clickable there. `force_running`, offered
        // above, is the action that does something on those three.
        disabled: overridesLocked || restartBlocked,
        // …and SAY so on hover. A successful restart ends with the row
        // `stopped`, where Restart is disabled -- so this is the RESTING state
        // of a healthy row, not an edge case, and every operator meets it. An
        // unexplained grey button is what makes a correct gate look like a
        // portal bug. (`overridesLocked` gets no reason of its own: it is
        // transient and already narrated by the row's own "stopping…" /
        // "clearing…" chip.)
        title: restartBlocked ? t.runtimeRestartUnavailable : undefined,
        onClick: () => void startRestart(row.spec_id, spec),
      });
    }
    return actions;
  }

  // File-mode read-only spec list: rendered from the agent's report instead of
  // the gateway CRUD data, and never editable.
  const reportSpecColumns: ListColumn<ReportSpec>[] = [
    { id: 'model', label: t.tableModel, value: (s) => s.model, filter: 'text' },
    { id: 'binary', label: t.runtimeSpecBinary, value: (s) => basename(s.binary), filter: 'text' },
    {
      id: 'gpus',
      label: t.runtimeSpecGpus,
      value: (s) =>
        s.gpus.length === 0 ? '—' : s.gpus.map((g) => `${g.index}: ${g.vramMb} MB`).join(', '),
      searchable: false,
    },
    {
      id: 'pinned',
      label: t.runtimeSpecPinned,
      value: (s) => (s.pinned ? 'yes' : 'no'),
      searchable: false,
      render: (s) => renderBoolChip(s.pinned, t.runtimeSpecPinned),
    },
    {
      // Worth a column HERE in particular: a file-mode document never passes
      // through this portal's validation, so this read-only echo is where an
      // operator sees what their hand-written file actually asked for.
      id: 'set_visible_devices',
      label: t.runtimeSpecSetVisibleDevices,
      value: (s) => (s.setVisibleDevices ? 'yes' : 'no'),
      searchable: false,
      render: (s) => renderBoolChip(s.setVisibleDevices, t.runtimeSpecSetVisibleDevices),
    },
    {
      id: 'idle_timeout',
      label: t.runtimeSpecIdleTimeout,
      value: (s) => formatMetric(s.idleTimeoutSeconds, 0),
      numeric: true,
      searchable: false,
    },
  ];

  const streamChip: { status: BadgeStatus; label: string } =
    streamStatus === 'open'
      ? { status: 'active', label: t.runtimeStreamOpen }
      : streamStatus === 'error'
        ? { status: 'standby', label: t.runtimeStreamOffline }
        : { status: 'watch', label: t.runtimeStreamConnecting };

  // Model-mapping edit sub-view. Same full-render-takeover convention as the
  // spec form below, and the SAME mask an ordinary application's model screen
  // uses -- one definition (`MappingForm`), so "die selbe Tabelle / der selbe
  // Edit" is enforced rather than merely intended.
  if (mappingEdit) {
    return (
      <>
        <Breadcrumbs
          ariaLabel={t.breadcrumb}
          backLabel={t.back}
          items={[
            ...trail,
            { label: application.endpoint, onClick: () => setMappingEdit(null) },
            { label: t.mappingEditTitle },
          ]}
        />
        <Panel titleId="runtime-mapping-form-heading" title={t.mappingEditTitle}>
          <MappingForm
            key={mappingEdit.id}
            t={t}
            api={api}
            serverId={server.id}
            contextProbePath={application.context_probe_path ?? ''}
            row={mappingEdit}
            // The one difference from the ordinary mapping screen, and it is an
            // ownership boundary: this application HAS a runtime spec, and the
            // spec owns the application model name (its `upstream_model`).
            appNameReadOnly
            busy={busy}
            onSubmit={(values) => void saveMappingFromTab(mappingEdit.id, values)}
            onCancel={() => setMappingEdit(null)}
          />
        </Panel>
      </>
    );
  }

  // Create / edit sub-view (input mask) -- replaces the tabbed view entirely,
  // matching the rest of the portal's drill-down/sub-view convention.
  if (specMode !== 'list') {
    const editing = specMode !== 'create';
    // Recomputed on every keystroke on purpose: the whole defect being fixed
    // is that the field's contract was invisible until a foreign program
    // rejected it, so the feedback has to land at paste time, not at submit.
    const argsWarnings = collectArgsWarnings(parseArgsText(argsText), listenPort, t);
    return (
      <>
        <Breadcrumbs
          ariaLabel={t.breadcrumb}
          backLabel={t.back}
          items={[
            ...trail,
            { label: application.endpoint, onClick: () => setSpecMode('list') },
            { label: editing ? t.runtimeSpecEdit : t.runtimeSpecCreate },
          ]}
        />
        <Panel
          titleId="runtime-spec-form-heading"
          title={editing ? t.runtimeSpecEdit : t.runtimeSpecCreate}
        >
          <Box
            component="form"
            onSubmit={editing ? submitEdit : submitCreate}
            sx={{ display: 'grid', gridTemplateColumns: 'minmax(260px, 560px)', gap: 2.25 }}
          >
            {/* Two model NAMES, and this form owns exactly one of them.
                The MAPPING owns the gateway-facing name and the active/disabled
                status; the RUNTIME SPEC owns the application model name,
                because that name IS the spec's `upstream_model` -- the only
                thing ${MODEL} expands to when the agent builds the process
                argv. So the gateway name is shown here (a spec is unreadable
                without knowing which model it serves) and edited one tab to
                the left, and the status select is gone entirely: it edited the
                MAPPING and never reached putRuntimeSpec, whose request type has
                no status field at all.

                Do not confuse the removed select with the "Aktiv" checkbox
                below. That one is `spec.enabled`, which decides whether this
                spec enters the agent's runtime-config document -- a different
                question from whether the gateway routes the model. */}
            <Typography variant="subtitle2" component="h3">
              {t.runtimeSpecModelSection}
            </Typography>
            <Field
              id="runtime-spec-gateway-name"
              label={t.mappingGatewayName}
              value={gatewayName}
              // EDIT-only read-only, and the no-op is load-bearing: jsdom fires
              // `change` on a readOnly input, so a live handler would still
              // drive state and give a test a false green. CREATE keeps the
              // field writable -- see `submitCreate`.
              onChange={editing ? () => {} : (e) => setGatewayName(e.target.value)}
              required
              // readOnly, never `disabled`: the value's whole job here is to be
              // READ, and a readonly input is barred from HTML constraint
              // validation, so `required` cannot block submit either.
              {...(editing
                ? {
                    inputProps: { readOnly: true },
                    helperText: t.runtimeSpecGatewayNameReadOnly,
                  }
                : {})}
            />
            <Field
              id="runtime-spec-app-name"
              label={t.mappingAppName}
              value={appName}
              onChange={(e) => setAppName(e.target.value)}
              required
            />

            <Typography variant="subtitle2" component="h3" sx={{ mt: 1 }}>
              {t.runtimeSpecConfigSection}
            </Typography>
            <FormControlLabel
              control={
                <Checkbox checked={enabled} onChange={(e) => setEnabled(e.target.checked)} />
              }
              label={t.runtimeSpecEnabled}
            />
            <Field
              id="runtime-spec-binary"
              label={t.runtimeSpecBinary}
              value={binary}
              onChange={(e) => setBinary(e.target.value)}
              required
              placeholder="/usr/local/bin/llama-server"
            />
            <Field
              id="runtime-spec-args"
              label={t.runtimeSpecArgs}
              value={argsText}
              onChange={(e) => setArgsText(e.target.value)}
              multiline
              minRows={3}
              // The rule stated, then SHOWN: the sentence alone never conveyed
              // that a flag and its value are two lines, and the example is
              // also where "${PORT}, not a number" becomes concrete.
              helperText={
                <>
                  {t.runtimeSpecArgsHint}
                  <Box
                    component="span"
                    sx={{
                      display: 'block',
                      mt: 0.5,
                      fontFamily: 'monospace',
                      whiteSpace: 'pre',
                      overflowX: 'auto',
                    }}
                  >
                    {t.runtimeSpecArgsExample}
                  </Box>
                </>
              }
            />
            {argsWarnings.map((warning) => (
              <Alert key={warning.key} severity="warning" sx={{ mt: -1 }}>
                {warning.message}
                {warning.detail !== undefined && (
                  <Box
                    component="span"
                    sx={{
                      display: 'block',
                      mt: 0.5,
                      fontFamily: 'monospace',
                      whiteSpace: 'pre',
                      overflowX: 'auto',
                    }}
                  >
                    {warning.detail}
                  </Box>
                )}
              </Alert>
            ))}
            <Field
              id="runtime-spec-env"
              label={t.runtimeSpecEnv}
              value={envText}
              onChange={(e) => setEnvText(e.target.value)}
              multiline
              minRows={3}
              helperText={t.runtimeSpecEnvHint}
            />
            <Field
              id="runtime-spec-work-dir"
              label={t.runtimeSpecWorkDir}
              value={workDir}
              onChange={(e) => setWorkDir(e.target.value)}
            />
            <Field
              id="runtime-spec-listen-port"
              type="number"
              label={t.runtimeSpecListenPort}
              value={String(listenPort)}
              onChange={(e) => setListenPort(e.target.value === '' ? 0 : Number(e.target.value))}
              helperText={t.runtimeSpecListenPortHelp}
              inputProps={{ min: 0, max: 65535 }}
            />
            <Field
              id="runtime-spec-health-path"
              label={t.runtimeSpecHealthPath}
              value={healthPath}
              onChange={(e) => setHealthPath(e.target.value)}
              placeholder="/health"
            />
            <Field
              id="runtime-spec-health-timeout"
              type="number"
              label={t.runtimeSpecHealthTimeout}
              value={String(healthTimeoutSeconds)}
              onChange={(e) =>
                setHealthTimeoutSeconds(e.target.value === '' ? 0 : Number(e.target.value))
              }
              inputProps={{ min: 0 }}
            />
            <Field
              id="runtime-spec-startup-timeout"
              type="number"
              label={t.runtimeSpecStartupTimeout}
              value={String(startupTimeoutSeconds)}
              onChange={(e) =>
                setStartupTimeoutSeconds(e.target.value === '' ? 0 : Number(e.target.value))
              }
              inputProps={{ min: 0 }}
            />
            <Field
              id="runtime-spec-idle-timeout"
              type="number"
              label={t.runtimeSpecIdleTimeout}
              value={String(idleTimeoutSeconds)}
              onChange={(e) =>
                setIdleTimeoutSeconds(e.target.value === '' ? 0 : Number(e.target.value))
              }
              inputProps={{ min: 0 }}
            />
            <Field
              id="runtime-spec-admission-wait-timeout"
              type="number"
              label={t.runtimeSpecAdmissionWaitTimeout}
              value={String(admissionWaitTimeoutSeconds)}
              onChange={(e) =>
                setAdmissionWaitTimeoutSeconds(e.target.value === '' ? 0 : Number(e.target.value))
              }
              inputProps={{ min: 0 }}
            />
            <FormControlLabel
              control={<Checkbox checked={pinned} onChange={(e) => setPinned(e.target.checked)} />}
              label={t.runtimeSpecPinned}
            />
            {/* The label used to read "VRAM locked (not evictable)", which
                described `pinned` (directly above) and not this flag at all --
                two adjacent checkboxes both promising non-eviction, one of them
                falsely. vram_locked decides which VRAM number the AGENT is
                told: locked, the operator's estimate wins over the agent's
                measurement, in the config document and in the write-back
                alike. That is the operator's only lever when a measurement
                above the GPU budget has made a spec terminally
                `not_permitted`, so the hint has to say so where they are
                looking. */}
            <FormControlLabel
              control={
                <Checkbox checked={vramLocked} onChange={(e) => setVramLocked(e.target.checked)} />
              }
              label={t.runtimeSpecVramLocked}
            />
            <Typography variant="caption" color="text.secondary" sx={{ mt: -1 }}>
              {t.runtimeSpecVramLockedHint}
            </Typography>
            {/* Without this, the GPU rows below are a DECLARATION: they drive
                the admission arithmetic and the VRAM measurement mapping, and
                nothing stops the process from landing on a different card --
                after which the accounting is confidently wrong and nothing
                warns. With it on, the agent sets the visibility variable its
                own hardware wants (CUDA_VISIBLE_DEVICES on NVIDIA,
                ROCR_VISIBLE_DEVICES on AMD; nothing on Apple) from these rows.

                The hint carries the renumbering consequence because this is
                the only place an operator meets it: a child launched with
                CUDA_VISIBLE_DEVICES=3,4 enumerates its devices as 0 and 1, so
                any ARGUMENT naming a device number (--main-gpu,
                --tensor-split) is in the child's numbering from then on while
                these rows stay in the host's. Nothing can enforce that -- the
                agent cannot parse an arbitrary model server's argv -- so
                saying it here is the whole of the mitigation. */}
            <FormControlLabel
              control={
                <Checkbox
                  checked={setVisibleDevices}
                  onChange={(e) => setSetVisibleDevices(e.target.checked)}
                />
              }
              label={t.runtimeSpecSetVisibleDevices}
            />
            <Typography variant="caption" color="text.secondary" sx={{ mt: -1 }}>
              {t.runtimeSpecSetVisibleDevicesHint}
            </Typography>
            <SelectField
              id="runtime-spec-admin-state"
              label={t.runtimeSpecAdminState}
              value={adminState}
              onChange={(e) => setAdminState(e.target.value)}
            >
              {adminStateOptions.map((o) => (
                <option value={o.value} key={o.value || 'auto'}>
                  {t[o.labelKey]}
                </option>
              ))}
            </SelectField>

            <Box sx={{ display: 'grid', gap: 1 }}>
              <Typography variant="subtitle2" component="h3">
                {t.runtimeSpecGpus}
              </Typography>
              {/* No reported GPUs: say so ONCE, and omit the picker rather
                  than rendering an empty dropdown that reads as broken. The
                  numeric index below stays fully editable -- a machine that
                  has not reported yet, or a CPU-only host being prepared, must
                  still be configurable by hand. Suppressed until the fetch has
                  settled so the sentence is never a lie about hardware we have
                  simply not heard about yet. */}
              {hardwareSettled && telemetryGpus.length === 0 && (
                <Typography variant="caption" color="text.secondary">
                  {t.runtimeSpecGpuNoTelemetry}
                </Typography>
              )}
              {gpuRows.map((row, idx) => (
                <Box
                  key={row.rowKey}
                  sx={{ display: 'flex', gap: 1.5, alignItems: 'center', flexWrap: 'wrap' }}
                >
                  {/* Picks a card BY NAME and writes its index into the field
                      beside it. It augments that field and never replaces it:
                      telemetry can be stale, absent, or behind the hardware
                      actually in the machine. */}
                  {telemetryGpus.length > 0 && (
                    <SelectField
                      id={`runtime-spec-gpu-pick-${idx}`}
                      label={t.runtimeSpecGpuPick}
                      value={gpuSelectValue(row, idx)}
                      onChange={(e) => {
                        // The placeholder is a display state, not a choice:
                        // selecting it would otherwise blank the index.
                        if (e.target.value === '') return;
                        updateGpuRow(idx, { index: Number(e.target.value) });
                      }}
                      sx={{ maxWidth: 340 }}
                    >
                      <option value="">{t.runtimeSpecGpuPickPlaceholder}</option>
                      {gpuOptionsFor(idx).map((g) => (
                        <option value={String(g.index)} key={g.index}>
                          {gpuOptionLabel(g)}
                        </option>
                      ))}
                    </SelectField>
                  )}
                  <Field
                    id={`runtime-spec-gpu-index-${idx}`}
                    label={t.runtimeSpecGpuIndex}
                    type="number"
                    value={String(row.index)}
                    onChange={(e) =>
                      updateGpuRow(idx, {
                        index: e.target.value === '' ? 0 : Number(e.target.value),
                      })
                    }
                    inputProps={{ min: 0 }}
                    sx={{ maxWidth: 140 }}
                  />
                  <Field
                    id={`runtime-spec-gpu-vram-${idx}`}
                    label={t.runtimeSpecVram}
                    type="number"
                    value={String(row.vramEstimateMb)}
                    onChange={(e) =>
                      updateGpuRow(idx, {
                        vramEstimateMb: e.target.value === '' ? 0 : Number(e.target.value),
                      })
                    }
                    inputProps={{ min: 0 }}
                    sx={{ maxWidth: 160 }}
                  />
                  <Typography variant="body2" color="text.secondary" sx={{ minWidth: 200 }}>
                    {t.runtimeSpecVramMeasured}: {row.vramMeasuredMb} MB
                  </Typography>
                  <Button
                    type="button"
                    size="small"
                    color="secondary"
                    onClick={() => removeGpuRow(idx)}
                  >
                    {t.runtimeSpecGpuRemove}
                  </Button>
                </Box>
              ))}
              <Box>
                <Button type="button" variant="outlined" size="small" onClick={addGpuRow}>
                  {t.runtimeSpecGpuAdd}
                </Button>
              </Box>
            </Box>

            <Box sx={{ display: 'flex', gap: 1.5 }}>
              <Button type="submit" variant="contained" disabled={busy}>
                {editing ? t.save : t.runtimeSpecCreate}
              </Button>
              <Button
                type="button"
                variant="text"
                color="secondary"
                onClick={() => setSpecMode('list')}
              >
                {t.cancel}
              </Button>
            </Box>
          </Box>
        </Panel>
      </>
    );
  }

  return (
    <>
      <Breadcrumbs
        ariaLabel={t.breadcrumb}
        backLabel={t.back}
        items={[...trail, { label: application.endpoint }]}
      />
      {/* Screen-wide facts, deliberately above the tab strip: which mode this
          server is in, why a configured runtime might be doing nothing, and
          how a restart sequence ended. All are true on every tab, so none may
          hide behind one. */}
      {(fileMode ||
        featureMismatch ||
        agentNeverReported ||
        reportFailed ||
        mappingsFailed ||
        restartNotice !== null) && (
        <Box sx={{ display: 'grid', gap: 1, mb: 2 }}>
          {/* The runtime mode decides whether this whole screen is writable,
              so a failed report GET is a screen-wide fact and belongs here --
              with a retry, because "keep everything read-only forever" is not
              an acceptable resting place for one failed request. */}
          {reportFailed && (
            <ResourceFallback
              state={reportStatus}
              loadingLabel={t.loading}
              errorLabel={t.runtimeModeUnknown}
              errorDetail={reportError}
              retry={{ label: t.resourceRetry, onRetry: () => void reloadReport() }}
            />
          )}
          {/* Also screen-wide: the mappings are what every tab joins against
              (the list itself, the matrix's spec set, and the status table's
              spec_id -> mapping resolution, without which no row can be
              named or overridden). Left as the old two-state test this said
              "loading" forever and every status row went quietly blank.
              The two states need DIFFERENT words (fix round 1, C4): the
              hard-error text states that no process can be matched and no
              override actions are available, and on a failed RELOAD both
              clauses are false -- the previous payload is still in hand, the
              rows resolve and the overrides work. Only the refresh failed. */}
          {mappingsFailed && (
            <ResourceFallback
              state={mappingsStatus}
              loadingLabel={t.loading}
              errorLabel={t.runtimeMappingsUnavailable}
              staleErrorLabel={t.runtimeMappingsStale}
              errorDetail={mappingsError}
              retry={{ label: t.resourceRetry, onRetry: () => void reloadMappings() }}
            />
          )}
          {fileMode && (
            <Alert severity="info">
              <Box sx={{ display: 'grid', gap: 0.25 }}>
                <span>{t.runtimeManagedLocally}</span>
                {reportContent?.collected_at && (
                  <Typography variant="caption">
                    {t.runtimeReportCollectedAt}: {formatDate(reportContent.collected_at, '—')}
                  </Typography>
                )}
              </Box>
            </Alert>
          )}
          {fileMode && parseError && (
            <Alert severity="warning">{`${t.runtimeParseError} ${runtimeParseErrorReason(parseError, t)}`}</Alert>
          )}
          {fileMode && configuredSpecCount > 0 && (
            <Alert severity="warning">{t.runtimeIneffectiveSpecs}</Alert>
          )}
          {/* No version/feature list here: there is no telemetry row to
              report one from, and printing "Agent version: —" is what made
              the old single banner misdiagnose this case as "too old". */}
          {agentNeverReported && <Alert severity="warning">{t.runtimeAgentNeverReported}</Alert>}
          {featureMismatch && (
            <Alert severity="warning">
              <Box sx={{ display: 'grid', gap: 0.25 }}>
                <span>{t.runtimeFeatureMismatch}</span>
                <Typography variant="caption">
                  {t.runtimeAgentVersion}: {agentVersion || '—'}
                </Typography>
                <Typography variant="caption">
                  {t.runtimeAgentFeatures}: {agentFeatures.length ? agentFeatures.join(', ') : '—'}
                </Typography>
              </Box>
            </Alert>
          )}
          {/* The restart sequence's three terminal problems get their own
              banner rather than a chip: each leaves an override in place that
              the operator now has to decide about, and the advice they carry
              points at ANOTHER tab ("check the Runtime specs tab and clear it
              by hand"). They used to render inside the status tab only, which
              made them invisible to an operator who switched tabs during a
              sequence that is bounded at 120 s -- likely rather than exotic,
              and the one notice you must not miss is the one saying a model is
              still admission-blocked. Kept LAST in this stack so the standing
              facts above (mode, agent) are not pushed down by a transient one.
              Cleared the same way as before: by the next override action, the
              next restart, or a server switch. */}
          {restartNotice === 'timeout' && (
            <Alert severity="warning">{t.runtimeRestartTimeout}</Alert>
          )}
          {restartNotice === 'clear-timeout' && (
            <Alert severity="warning">{t.runtimeRestartClearTimeout}</Alert>
          )}
          {restartNotice === 'vanished' && (
            <Alert severity="warning">{t.runtimeRestartVanished}</Alert>
          )}
        </Box>
      )}
      <Tabs
        value={tab}
        onChange={(_e, v: Tab) => setTab(v)}
        aria-label={t.runtimeAdmin}
        sx={{ mb: 2.5 }}
      >
        <Tab label={t.runtimeMappingTab} value="mapping" />
        <Tab label={t.runtimeSpecs} value="specs" />
        <Tab label={t.runtimeMatrix} value="matrix" />
        <Tab label={t.runtimeLimits} value="limits" />
        <Tab label={t.runtimeLiveStatus} value="status" />
      </Tabs>

      {tab === 'mapping' && (
        <Panel
          titleId="runtime-mapping-heading"
          title={t.modelMappings}
          subtitle={t.modelMappingsIntro}
        >
          {/* Deliberately NOT gated on `writesAllowed`. ADR-029's "no write
              control before its own GET has resolved" governs the FULL-DOCUMENT
              replaces (spec PUT, co-residency PUT, budget PUT); the mapping
              PATCH merges per field and is exempt. Substantively: a mapping is
              a gateway ROUTE, so on a file-mode server the specs are
              ineffective but taking a model out of service still matters --
              copying the gate would leave an operator unable to do it. */}
          <Typography variant="caption" color="text.secondary" sx={{ display: 'block', mb: 1.5 }}>
            {t.runtimeMappingCreateHint}
          </Typography>
          <ListTable
            rows={mappings}
            columns={mappingColumns(t)}
            rowKey={(m) => m.id}
            actions={mappingRowActions}
            // NOT 'op.mappings': ListTable persists column order/visibility,
            // sort and page size under this key, and sharing it would fuse this
            // tab's layout with the ordinary mapping screen's into one setting.
            storageKey="op.runtimeMappings"
            labels={listTableLabels(t)}
            loading={mappingsStatus === 'loading'}
          />
        </Panel>
      )}
      {tab === 'specs' && (
        <Panel
          titleId="runtime-specs-heading"
          title={t.runtimeSpecs}
          subtitle={t.runtimeSpecsIntro}
          // No create button until we know this screen is writable -- and
          // never in file mode.
          actions={
            writesAllowed ? (
              <Button variant="contained" startIcon={<AddIcon />} onClick={openCreate}>
                {t.runtimeSpecCreate}
              </Button>
            ) : undefined
          }
        >
          {fileMode ? (
            parseError ? (
              // The agent could not read its own file: `config` is unusable,
              // so nothing is rendered from it. The parse error itself is
              // already surfaced above the tabs.
              <Typography color="text.secondary">{t.runtimeConfigUnavailable}</Typography>
            ) : (
              <Box sx={{ display: 'grid', gap: 1.5 }}>
                {reportConfig?.unrecognised && (
                  <Alert severity="warning">{t.runtimeConfigUnrecognised}</Alert>
                )}
                <ListTable
                  rows={reportConfig?.specs ?? []}
                  columns={reportSpecColumns}
                  rowKey={(s) => s.id}
                  storageKey="op.runtimeReportSpecs"
                  minWidth={780}
                  labels={listTableLabels(t)}
                />
              </Box>
            )
          ) : (
            <>
              {(warningsFailed || warnings.length > 0) && (
                <Box sx={{ display: 'grid', gap: 1, mb: 2 }}>
                  {/* Sits with the warnings it replaces, not in the screen-wide
                      stack: these are advisory facts about this application's
                      specs, which is this tab. */}
                  {warningsFailed && (
                    <ResourceFallback
                      state={warningsStatus}
                      loadingLabel={t.loading}
                      errorLabel={t.runtimeWarningsUnavailable}
                      staleErrorLabel={t.runtimeWarningsStale}
                      errorDetail={warningsError}
                      severity="info"
                      retry={{ label: t.resourceRetry, onRetry: () => void reloadWarnings() }}
                    />
                  )}
                  {warnings.map((code) => (
                    <Alert key={code} severity="warning">
                      {runtimeWarningLabelByCode[code] ? t[runtimeWarningLabelByCode[code]] : code}
                    </Alert>
                  ))}
                </Box>
              )}
              <ListTable
                rows={mappings}
                columns={columns}
                rowKey={(m) => m.id}
                actions={writesAllowed ? rowActions : undefined}
                storageKey="op.runtimeSpecs"
                minWidth={900}
                labels={listTableLabels(t)}
                // Only a GET that is genuinely in flight. `mappingsLoading ||
                // mappingsData === null` also caught the FAILED GET, which
                // left this table saying "loading" for as long as the page
                // stayed open; the screen-wide banner names that state and
                // offers the retry, and the table falls back to its ordinary
                // empty text.
                loading={mappingsStatus === 'loading'}
              />
            </>
          )}
        </Panel>
      )}
      {tab === 'matrix' && (
        <Panel
          titleId="runtime-matrix-heading"
          title={t.runtimeMatrix}
          subtitle={t.runtimeMatrixHint}
        >
          {fileMode ? (
            parseError ? (
              <Typography color="text.secondary">{t.runtimeConfigUnavailable}</Typography>
            ) : (
              <Box sx={{ display: 'grid', gap: 1.5 }}>
                {reportConfig?.unrecognised && (
                  <Alert severity="warning">{t.runtimeConfigUnrecognised}</Alert>
                )}
                {/* Task 21 built and tested `disabled` but deliberately left
                    it unwired: this is the signal it was waiting for. The
                    onToggle can never fire, but a no-op stays honest about
                    the prop's contract. */}
                <RuntimeMatrix
                  t={t}
                  specs={(reportConfig?.specs ?? []).map((s) => ({
                    id: s.id,
                    model: s.model,
                    gpus: s.gpus,
                  }))}
                  pairs={reportConfig?.coresident ?? []}
                  onToggle={() => {}}
                  budgets={Object.fromEntries(
                    (reportConfig?.budgets ?? []).map((b) => [b.index, b.budgetMb]),
                  )}
                  disabled
                  // A disabled cell must say why (the RowActionsCell /
                  // IconAction rule): "read-only" and "not allowed yet" look
                  // alike otherwise, and this one is permanently disabled for
                  // as long as the agent owns its own config file.
                  disabledReason={t.runtimeMatrixDisabledFileMode}
                />
              </Box>
            )
          ) : reportStatus !== 'ready' ? (
            // Deliberately NOT the matrix with an empty pair list: the report
            // GET (which decides whether this screen is writable at all) has
            // not settled, so there is nothing to toggle from. A FAILED report
            // GET is a different fact from a pending one and must not read as
            // "loading" -- the screen-wide banner above carries the
            // explanation and the retry.
            <ResourceFallback
              state={reportFailed ? reportStatus : 'loading'}
              loadingLabel={t.loading}
              errorLabel={t.runtimeModeUnknownShort}
            />
          ) : !coresidencyReady ? (
            // The pairs GET itself (task-21 review's Critical finding), now
            // with its failures named instead of shown as an endless
            // "loading…": on a hard failure there is nothing to render, on a
            // failed reload there is a payload we could not refresh and a
            // toggle would PUT the full replacement list computed from it.
            // Neither may be clicked, and both get the retry this tab never
            // had -- the resource's `reload()` was not even destructured.
            //
            // Both states hide the matrix, but they are not the same sentence
            // (fix round 1, M7): "could not be loaded" is false once a payload
            // is in hand and only the refresh failed.
            <ResourceFallback
              state={coresidencyStatus}
              loadingLabel={t.loading}
              errorLabel={t.runtimeCoresidencyUnavailable}
              staleErrorLabel={t.runtimeCoresidencyStale}
              errorDetail={coresidencyError}
              retry={{ label: t.resourceRetry, onRetry: () => void reloadCoresidency() }}
            />
          ) : (
            <RuntimeMatrix
              t={t}
              specs={matrixSpecs}
              pairs={coresidencyPairs}
              onToggle={(a, b) => void toggleCoresidency(a, b)}
              budgets={savedBudgetsByGpuIndex}
              disabled={coresidencyBusy}
              // Transient, unlike file mode above -- but for the few hundred
              // milliseconds it lasts, an unexplained dead grid is the same
              // defect. Undefined when not busy, so a live matrix never
              // carries a reason it does not have.
              disabledReason={coresidencyBusy ? t.runtimeMatrixDisabledSaving : undefined}
            />
          )}
        </Panel>
      )}
      {tab === 'limits' && (
        <Panel
          titleId="runtime-limits-heading"
          title={t.runtimeLimits}
          subtitle={t.runtimeLimitsIntro}
        >
          {fileMode ? (
            parseError ? (
              <Typography color="text.secondary">{t.runtimeConfigUnavailable}</Typography>
            ) : (
              // Read-only: the limits live in the agent's local file, and the
              // gateway document file mode ignores is not what is in force.
              <Box sx={{ display: 'grid', gap: 1 }}>
                {reportConfig?.unrecognised && (
                  <Alert severity="warning">{t.runtimeConfigUnrecognised}</Alert>
                )}
                <Box sx={{ display: 'flex', gap: 1 }}>
                  <Typography variant="body2" color="text.secondary">
                    {t.runtimeMaxProcesses}
                  </Typography>
                  <Typography variant="body2">
                    {reportConfig?.maxProcesses === null || reportConfig === null
                      ? '—'
                      : String(reportConfig.maxProcesses)}
                  </Typography>
                </Box>
                <Typography variant="subtitle2" component="h3">
                  {t.runtimeGpuBudget}
                </Typography>
                {(reportConfig?.budgets ?? []).length === 0 ? (
                  <Typography variant="body2" color="text.secondary">
                    —
                  </Typography>
                ) : (
                  (reportConfig?.budgets ?? []).map((b) => (
                    <Typography key={b.index} variant="body2">
                      {`GPU ${b.index}: ${b.budgetMb} MB`}
                    </Typography>
                  ))
                )}
              </Box>
            )
          ) : reportStatus !== 'ready' ? (
            // Until the report resolves we do not know whether this form should
            // exist at all. No form, no click, no write. A FAILED report GET
            // says so instead of claiming to still be loading.
            <ResourceFallback
              state={reportFailed ? reportStatus : 'loading'}
              loadingLabel={t.loading}
              errorLabel={t.runtimeModeUnknownShort}
            />
          ) : !gpuBudgetsReady ? (
            // The budgets GET itself. While it is in flight `budgetRows` is
            // still its initial `[]`, indistinguishable from "no budgets
            // configured", and Save PUTs it as the FULL replacement -- so a
            // premature click would erase every previously configured budget
            // (task-21 review's Critical finding). A FAILED GET is the same
            // hazard with a worse disguise: on a failed RELOAD `budgetRows`
            // still holds the pre-failure values, so the form would look
            // perfectly normal and Save would write values we had just failed
            // to re-read. Both states get the message and the retry instead --
            // each in its own words (fix round 1, M7), because on a failed
            // reload "could not be loaded" is simply not what happened.
            <ResourceFallback
              state={gpuBudgetsStatus}
              loadingLabel={t.loading}
              errorLabel={t.runtimeBudgetsUnavailable}
              staleErrorLabel={t.runtimeBudgetsStale}
              errorDetail={gpuBudgetsError}
              retry={{ label: t.resourceRetry, onRetry: () => void reloadGpuBudgets() }}
            />
          ) : (
            <Box sx={{ display: 'grid', gap: 2.5 }}>
              <Box sx={{ display: 'grid', gap: 1 }}>
                <Typography variant="subtitle2" component="h3">
                  {t.runtimeGpuBudget}
                </Typography>
                {budgetRows.map((row, idx) => {
                  const live = driftFor(row);
                  return (
                    <Box
                      key={row.rowKey}
                      sx={{ display: 'flex', gap: 1.5, alignItems: 'center', flexWrap: 'wrap' }}
                    >
                      <Field
                        id={`runtime-budget-index-${idx}`}
                        label={t.runtimeSpecGpuIndex}
                        type="number"
                        value={String(row.index)}
                        onChange={(e) =>
                          updateBudgetRow(idx, {
                            index: e.target.value === '' ? 0 : Number(e.target.value),
                          })
                        }
                        inputProps={{ min: 0 }}
                        sx={{ maxWidth: 140 }}
                      />
                      <Field
                        id={`runtime-budget-mb-${idx}`}
                        label={t.runtimeGpuBudget}
                        type="number"
                        value={String(row.budgetMb)}
                        onChange={(e) =>
                          updateBudgetRow(idx, {
                            budgetMb: e.target.value === '' ? 0 : Number(e.target.value),
                          })
                        }
                        inputProps={{ min: 0 }}
                        sx={{ maxWidth: 200 }}
                      />
                      {live && (
                        <Tooltip
                          title={
                            <Box sx={{ display: 'grid', gap: 0.25 }}>
                              <Typography variant="caption">{t.runtimeGpuDriftWarning}</Typography>
                              <Typography variant="caption">
                                {t.runtimeGpuDriftExpected}: {row.expectedName || '—'} (
                                {row.expectedUuid})
                              </Typography>
                              <Typography variant="caption">
                                {t.runtimeGpuDriftCurrent}: {live.name || '—'} ({live.uuid})
                              </Typography>
                            </Box>
                          }
                        >
                          <IconButton
                            size="small"
                            color="warning"
                            aria-label={`${t.runtimeGpuDriftIconLabel}: GPU ${row.index}`}
                          >
                            <WarningAmberIcon fontSize="small" />
                          </IconButton>
                        </Tooltip>
                      )}
                      <Button
                        type="button"
                        size="small"
                        color="secondary"
                        onClick={() => removeBudgetRow(idx)}
                      >
                        {t.runtimeSpecGpuRemove}
                      </Button>
                    </Box>
                  );
                })}
                <Box>
                  <Button type="button" variant="outlined" size="small" onClick={addBudgetRow}>
                    {t.runtimeSpecGpuAdd}
                  </Button>
                </Box>
              </Box>

              <Field
                id="runtime-max-processes"
                type="number"
                label={t.runtimeMaxProcesses}
                value={String(maxProcesses)}
                onChange={(e) =>
                  setMaxProcesses(e.target.value === '' ? 0 : Number(e.target.value))
                }
                inputProps={{ min: 0 }}
                sx={{ maxWidth: 280 }}
              />

              <Box>
                <Button
                  type="button"
                  variant="contained"
                  disabled={limitsBusy}
                  onClick={() => void saveLimits()}
                >
                  {t.save}
                </Button>
              </Box>
            </Box>
          )}
        </Panel>
      )}
      {tab === 'status' && (
        <Panel
          titleId="runtime-status-heading"
          title={t.runtimeLiveStatus}
          actions={<StatusChip status={streamChip.status} label={streamChip.label} />}
        >
          <Box sx={{ display: 'grid', gap: 1.5 }}>
            {/* The restart notices used to render here. They are screen-wide
                facts now (see the banner stack above the tab strip): the
                sequence runs for up to 120 s and its notices tell the operator
                to go and clear an override on a DIFFERENT tab, so they must not
                disappear the moment they do. */}
            <ListTable
              rows={statusRows}
              columns={statusColumns}
              rowKey={(row) => row.spec_id}
              actions={statusActions}
              storageKey="op.runtimeStatus"
              minWidth={980}
              // "Nothing is running" and "we cannot see what is running" are
              // different facts and must not read alike: the empty cell says
              // which one it is, and the panel's chip says it again.
              labels={listTableLabels(t, {
                empty: streamStatus === 'error' ? t.runtimeStreamError : t.runtimeStatusEmpty,
                loading: t.runtimeStreamConnecting,
              })}
              loading={streamStatus === 'loading' && statusRows.length === 0}
            />
          </Box>
        </Panel>
      )}

      <ConfirmDialog
        open={confirmingDeleteId !== ''}
        title={confirmIsSpecDelete ? t.runtimeSpecDeleteConfirm : t.mappingDeleteConfirm}
        confirmLabel={confirmIsSpecDelete ? t.runtimeSpecDelete : t.mappingDelete}
        cancelLabel={t.mappingCancel}
        onConfirm={() => void confirmDelete()}
        onCancel={() => setConfirmingDeleteId('')}
      />

      {/* Mounted only while a row's log view is open: mounting it means
          subscribing, and subscribing is what makes the agent stream. */}
      {logSpec !== null && (
        <RuntimeLogView
          open
          onClose={() => setLogSpec(null)}
          api={api}
          t={t}
          serverId={server.id}
          specId={logSpec.specId}
          title={logSpec.title}
        />
      )}
    </>
  );
}
