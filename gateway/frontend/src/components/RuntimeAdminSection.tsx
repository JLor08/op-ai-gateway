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
import WarningAmberIcon from '@mui/icons-material/WarningAmber';
import PlayArrowIcon from '@mui/icons-material/PlayArrow';
import StopIcon from '@mui/icons-material/Stop';
import ClearIcon from '@mui/icons-material/Clear';
import ReplayIcon from '@mui/icons-material/Replay';
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
} from '../api';
import type { Translation, PortalApi, MessageKey, BadgeStatus } from './shared/types';
import { formatPortalError, formatMetric, formatDate } from './shared/format';
import { useResource } from './shared/useResource';
import { useLatestFetch } from './shared/useLatestFetch';
import { StatusChip } from './shared/StatusChip';
import { Panel } from './shared/Panel';
import { Field } from './shared/Field';
import { SelectField } from './shared/SelectField';
import { ConfirmDialog } from './shared/ConfirmDialog';
import { Breadcrumbs, type BreadcrumbItem } from './shared/Breadcrumbs';
import { ListTable, listTableLabels, type ListColumn } from './shared/ListTable';
import type { RowAction } from './shared/RowActionsMenu';
import { useToast } from './shared/ToastProvider';
import { applicationStatusOptions, applicationStatusLabelByKey } from './shared/application';
import { RuntimeMatrix, type RuntimeMatrixSpec } from './RuntimeMatrix';

// Area 1 (Task 20) is "Launch specs"; areas 2-3 (this task, Task 21) are the
// co-residency matrix and server limits. Area 4 (Task 22, "Live status") is
// still a stub rendered from the same tab strip so the whole section's
// navigation is visible/testable now.
type Tab = 'specs' | 'matrix' | 'limits' | 'status';

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
 * reaches us on the 1 s sample. 90 s of that, rounded up to 120 s so an
 * ordinary slow stop is never reported as a failure. A WS-connected agent
 * gets the push immediately and normally finishes in a couple of seconds.
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

type GpuRow = { index: number; vramEstimateMb: number; vramMeasuredMb: number };

// The sole warning code RuntimeWarnings emits today (see
// gateway/backend/internal/portal/service_runtime.go); an unmapped future
// code falls back to its raw wire value rather than a misleading label.
const runtimeWarningLabelByCode: Record<string, MessageKey> = {
  timeout_ms_below_startup_timeout: 'runtimeTimeoutWarning',
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

// A "${...}" occurrence's classification, ported line-for-line from
// server-agent/internal/runtime/policy_local.go's ExpandPlaceholders -- see
// that file for the full rationale. Both an argument string and an env
// value pass through the SAME agent-side check at process-start time (this
// task's UI has no live-status view yet to surface a failure there -- see
// the task-20 report's Deviation 2), so the portal form must refuse
// everything the agent would refuse and accept everything it would accept:
//   - "${PORT}" (exact) -> valid, becomes the assigned port at start.
//   - "${AGENT_ENV:NAME}" with a non-empty NAME -> valid, UNLESS NAME starts
//     with "OP_AGENT_" (the agent's own credential namespace) -> reserved.
//   - anything else whose upper-cased inner text STARTS WITH "PORT" or
//     "AGENT_ENV" -> a malformed near-miss (a typo of one of the two forms
//     above): "${PORTX}", "${port}", "${AGENT_ENV:}", "${AGENT_ENVV:x}".
//   - everything else -- arbitrary "${...}" text a model server's own
//     templating syntax might use, e.g. "${TRANSPORT}", "${EXPORT_DIR}",
//     "${MY_AGENT_ENVIRONMENT}" -- passes through untouched.
// This MUST be a prefix match, never a substring/Contains check, or those
// last three examples would be wrongly refused (see the Go source's doc
// comment for the full explanation of that prior mistake).
const placeholderPattern = /\$\{[^}]*\}/g;
const agentEnvPrefix = 'AGENT_ENV:';
const agentOwnEnvPrefix = 'OP_AGENT_';

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

// KEY=value per line, one env var per line. Blank/whitespace-only lines are
// skipped; a malformed (no "=") line is reported via the existing
// runtime_spec.env_invalid label so it reads the same way a backend
// rejection would. Only the KEY portion is trimmed (stray leading
// indentation from a paste) -- the VALUE is preserved byte-for-byte after
// the first "=" (a value may legitimately contain "=" itself, and trimming
// the whole line before splitting would silently eat meaningful leading/
// trailing whitespace IN the value). PATH/HOME keys are refused outright
// (reserved by the agent's own base environment); the ${AGENT_ENV:OP_AGENT_*}
// / malformed-placeholder checks run separately, over both args and env
// values together, via validatePlaceholders.
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
    if (key === 'PATH' || key === 'HOME') {
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
    // Task 21 (matrix + server limits): the process-limit field is saved
    // through the general server PATCH, and a new budget row is prefilled
    // from the same live-telemetry hardware report the Hardware tab reads.
    | 'updateServer'
    | 'serverHardware'
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

  const {
    data: mappingsData,
    setData: setMappings,
    error: mappingsError,
    loading: mappingsLoading,
  } = useResource(
    () => api.mappings(application.id).then((r) => r.data),
    [api, application.id, t],
    t,
  );
  const mappings = mappingsData ?? [];
  useEffect(() => {
    if (mappingsError) showError(mappingsError);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [mappingsError]);

  const {
    data: warningsData,
    error: warningsError,
    reload: reloadWarnings,
  } = useResource(
    () => api.runtimeWarnings(application.id).then((r) => r.warnings),
    [api, application.id, t],
    t,
  );
  const warnings = warningsData ?? [];
  useEffect(() => {
    if (warningsError) showError(warningsError);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [warningsError]);

  // Per-mapping runtime spec, loaded lazily and once per mapping id into a
  // map (the brief's "lazy... loaded once" recipe) -- a bulk endpoint does
  // not exist, so the list view fans out one GET per row.
  const [specsById, setSpecsById] = useState<Record<string, RuntimeSpec>>({});
  const loadedIdsRef = useRef<Set<string>>(new Set());
  useEffect(() => {
    const toLoad = mappings.filter((m) => !loadedIdsRef.current.has(m.id));
    if (toLoad.length === 0) return undefined;
    let cancelled = false;
    (async () => {
      for (const m of toLoad) {
        loadedIdsRef.current.add(m.id);
        try {
          const spec = await api.runtimeSpec(m.id);
          if (!cancelled) setSpecsById((cur) => ({ ...cur, [m.id]: spec }));
        } catch (err) {
          if (!cancelled) showError(formatPortalError(err, t));
        }
      }
    })();
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [mappings, api]);

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
  // writing (see the missing-loading-state fix, task-21 review round 1) on
  // the SAME `data !== null` signal the specs list already uses
  // (`mappingsLoading || mappingsData === null`, above).
  const coresidencyReady = !coresidencyLoading && coresidencyData !== null;
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

  const {
    data: gpuBudgetsData,
    setData: setGpuBudgetsData,
    error: gpuBudgetsError,
    loading: gpuBudgetsLoading,
  } = useResource(() => api.gpuBudgets(server.id).then((r) => r.budgets), [api, server.id, t], t);
  // Same "null (not loaded) vs. [] (loaded, empty)" distinction as the
  // co-residency matrix above: `budgetRows` starts `[]` and would look
  // exactly like "no budgets configured" if Save were reachable before this
  // GET settles -- and Save PUTs whatever `budgetRows` holds as the FULL
  // replacement set, so that would erase every previously configured
  // per-GPU budget on a single premature click.
  const gpuBudgetsReady = !gpuBudgetsLoading && gpuBudgetsData !== null;
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

  type BudgetRow = { index: number; budgetMb: number; expectedUuid: string; expectedName: string };
  const [budgetRows, setBudgetRows] = useState<BudgetRow[]>([]);
  // Re-seeds the editable rows whenever gpuBudgetsData gets a genuinely NEW
  // value -- the initial load, or the fresh authoritative list this
  // component itself feeds back via setGpuBudgetsData after a successful
  // save (see saveLimits). It is never refreshed by a background poll, so
  // this can't clobber an in-progress edit.
  useEffect(() => {
    if (gpuBudgetsData === null) return;
    setBudgetRows(
      gpuBudgetsData.map((b) => ({
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
            index: fromTelemetry.index,
            budgetMb: Math.round(fromTelemetry.memory_total_bytes / (1024 * 1024)),
            expectedUuid: '',
            expectedName: '',
          },
        ];
      }
      let index = 0;
      while (used.has(index)) index++;
      return [...rows, { index, budgetMb: 0, expectedUuid: '', expectedName: '' }];
    });
  }
  function removeBudgetRow(idx: number) {
    setBudgetRows((rows) => rows.filter((_, i) => i !== idx));
  }
  function updateBudgetRow(idx: number, patch: Partial<Pick<BudgetRow, 'index' | 'budgetMb'>>) {
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
  const onStatusFrame = useCallback((rows: RuntimeStatus[]) => {
    frameSeqRef.current += 1;
    setStatusRows(rows);
  }, []);
  useEffect(() => {
    // A server switch must not leave the previous server's processes on
    // screen while the new stream connects.
    setStatusRows([]);
    setStreamStatus('loading');
    frameSeqRef.current = 0;
    return api.subscribeRuntimeStatus(server.id, onStatusFrame, setStreamStatus);
  }, [api, server.id, onStatusFrame]);

  // ---- File mode + feature negotiation (spec §9, §10.2) -------------------
  const {
    data: reportData,
    error: reportError,
    loading: reportLoading,
  } = useResource(() => api.runtimeReport(server.id), [api, server.id, t], t);
  useEffect(() => {
    if (reportError) showError(reportError);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [reportError]);
  const reportReady = !reportLoading && reportData !== null;
  // `source` lives on the NESTED report object; RuntimeReport itself has none.
  const reportContent = reportData?.available ? reportData.report : undefined;
  const fileMode = reportContent?.source === 'file';
  // The agent telling us it could not parse its own config file. In that state
  // `config` is whatever survived (possibly the zero value), so it must not be
  // rendered at all -- and this, not a missing tooltip, is what the operator
  // needs to see.
  const parseError = reportContent?.parse_error ?? '';
  const reportConfig = useMemo(
    () => (fileMode && !parseError ? narrowReportConfig(reportContent?.config) : null),
    [fileMode, parseError, reportContent],
  );
  // Same discipline as areas 2/3, one step further out: until the report GET
  // has settled we do not know whether this whole screen is read-only, so no
  // writable affordance is presented at all. Gated on BOTH signals.
  const writesAllowed = reportReady && !fileMode;

  const configuredSpecCount = mappings.filter((m) => specsById[m.id]?.configured).length;
  // Spec §9's visible half: gateway-side specs exist, the agent reports no
  // managed process at all, and it never declared `runtime_manager`. Without
  // this banner an operator configures specs against an old agent and watches
  // nothing happen, with no clue why -- so the banner names the reported
  // version and feature list, i.e. what to upgrade.
  const featureMismatch =
    reportReady &&
    configuredSpecCount > 0 &&
    statusRows.length === 0 &&
    !(reportData?.agent_features ?? []).includes('runtime_manager');

  // ---- Row overrides + the restart sequence ------------------------------
  // The live stream keys rows by SPEC id; every write is keyed by MAPPING id.
  // Only a CONFIGURED spec has an id, and only a loaded one can be re-sent
  // verbatim, so this map is also the "may this row offer overrides at all"
  // test.
  const specByRuntimeId = new Map<string, RuntimeSpec>();
  for (const spec of Object.values(specsById)) {
    if (spec.configured && spec.id) specByRuntimeId.set(spec.id, spec);
  }

  const [overrideBusy, setOverrideBusy] = useState(false);
  const [restart, setRestart] = useState<RestartFlow | null>(null);
  const [restartNotice, setRestartNotice] = useState<'timeout' | 'vanished' | null>(null);
  // How long the awaited spec has been missing from the stream. A ref, not
  // state, so tracking it never re-arms the timeout effect below.
  const absentSinceRef = useRef<number | null>(null);
  const mountedRef = useRef(true);
  useEffect(
    () => () => {
      mountedRef.current = false;
    },
    [],
  );
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
  }, [server.id]);

  // Bounds the wait. The cleanup clears the timer whenever `restart` changes,
  // so this callback can only fire while the flow it captured is still the
  // current one -- no stale notice. `deadline` being absolute keeps the
  // overall bound honest across the phase change that re-arms the timer.
  useEffect(() => {
    if (restart === null || restart.phase === 'clearing') return undefined;
    const specId = restart.specId;
    const timer = setTimeout(
      () => {
        if (!mountedRef.current) return;
        absentSinceRef.current = null;
        setRestart((cur) => (cur?.specId === specId ? null : cur));
        setRestartNotice('timeout');
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

  async function setOverride(spec: RuntimeSpec, adminState: string) {
    // A timeout/aborted notice tells the operator an override was left in
    // place; acting on any override is exactly the moment it stops being
    // news, so it is cleared here rather than lingering until the next
    // restart or a server switch.
    setRestartNotice(null);
    setOverrideBusy(true);
    try {
      const updated = await api.putRuntimeSpec(
        spec.mapping_id,
        specBodyWithAdminState(spec, adminState),
      );
      if (!mountedRef.current) return;
      setSpecsById((cur) => ({ ...cur, [spec.mapping_id]: updated }));
      showSuccess(t.systemSaved);
    } catch (err) {
      if (mountedRef.current) showError(formatPortalError(err, t));
    } finally {
      if (mountedRef.current) setOverrideBusy(false);
    }
  }

  async function startRestart(specId: string, spec: RuntimeSpec) {
    if (restart !== null) return;
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
      setSpecsById((cur) => ({ ...cur, [spec.mapping_id]: updated }));
      setRestart((cur) => (cur?.specId === specId ? { ...cur, phase: 'waiting', waitFrom } : cur));
    } catch (err) {
      if (!mountedRef.current) return;
      showError(formatPortalError(err, t));
      setRestart((cur) => (cur?.specId === specId ? null : cur));
    }
  }

  async function finishRestart(flow: RestartFlow) {
    setRestart({ ...flow, phase: 'clearing' });
    // Re-read the spec by MAPPING id (captured when the flow started), not by
    // the stream's spec id: the clear PUT must not depend on the spec-id join
    // still resolving, and it must still be the actual stored document.
    const spec = specsById[flow.mappingId];
    if (spec === undefined) {
      setRestart(null);
      setRestartNotice('vanished');
      return;
    }
    try {
      const updated = await api.putRuntimeSpec(flow.mappingId, specBodyWithAdminState(spec, ''));
      if (!mountedRef.current) return;
      setSpecsById((cur) => ({ ...cur, [flow.mappingId]: updated }));
      showSuccess(t.systemSaved);
    } catch (err) {
      if (mountedRef.current) showError(formatPortalError(err, t));
    } finally {
      if (mountedRef.current) setRestart(null);
    }
  }

  // ---- create/edit form state -----------------------------------------
  const [gatewayName, setGatewayName] = useState('');
  const [appName, setAppName] = useState('');
  const [status, setStatus] = useState<ApplicationStatus>('active');
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
    setGpuRows(
      spec.gpus.map((g) => ({
        index: g.index,
        vramEstimateMb: g.vram_estimate_mb,
        vramMeasuredMb: g.vram_measured_mb,
      })),
    );
  }

  function openCreate() {
    setGatewayName('');
    setAppName('');
    setStatus('active');
    resetSpecFields();
    setSpecMode('create');
  }

  // Re-reads the spec fresh (rather than trusting the bulk-loaded map, which
  // may not have settled yet for a just-rendered row) so the full-document
  // PUT this form issues on save never clobbers fields it never saw.
  async function openEdit(mapping: PortalModelMapping) {
    setGatewayName(mapping.gateway_model_name);
    setAppName(mapping.app_model_name);
    setStatus(mapping.status);
    setLoadingEditFor(mapping.id);
    try {
      const spec = await api.runtimeSpec(mapping.id);
      setSpecsById((cur) => ({ ...cur, [mapping.id]: spec }));
      loadedIdsRef.current.add(mapping.id);
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
      return [...rows, { index, vramEstimateMb: 0, vramMeasuredMb: 0 }];
    });
  }
  function removeGpuRow(idx: number) {
    setGpuRows((rows) => rows.filter((_, i) => i !== idx));
  }
  function updateGpuRow(idx: number, patch: Partial<Pick<GpuRow, 'index' | 'vramEstimateMb'>>) {
    setGpuRows((rows) => rows.map((r, i) => (i === idx ? { ...r, ...patch } : r)));
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
    setBusy(true);
    let mapping: PortalModelMapping;
    try {
      mapping = await api.createMapping(application.id, {
        gateway_model_name: gatewayName,
        app_model_name: appName,
        status,
      });
      setMappings((current) => [mapping, ...(current ?? [])]);
      loadedIdsRef.current.add(mapping.id);
    } catch (err) {
      showError(formatPortalError(err, t));
      setBusy(false);
      return;
    }
    // The mapping now exists regardless of what happens next -- a failure
    // from here on is reported as a partial failure, never silently, so the
    // operator knows the spec (not the mapping) needs another attempt.
    try {
      const spec = await api.putRuntimeSpec(mapping.id, buildSpecBody(args, parsedEnv.env));
      setSpecsById((cur) => ({ ...cur, [mapping.id]: spec }));
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
    setBusy(true);
    try {
      const updated = await api.updateMapping(id, {
        gateway_model_name: gatewayName,
        app_model_name: appName,
        status,
      });
      setMappings((current) => (current ?? []).map((m) => (m.id === id ? updated : m)));
    } catch (err) {
      showError(formatPortalError(err, t));
      setBusy(false);
      return;
    }
    try {
      const spec = await api.putRuntimeSpec(id, buildSpecBody(args, parsedEnv.env));
      setSpecsById((cur) => ({ ...cur, [id]: spec }));
      void reloadWarnings();
      setSpecMode('list');
    } catch (err) {
      showError(`${t.runtimeSpecPartialFailure}: ${formatPortalError(err, t)}`);
      setSpecMode('list');
    } finally {
      setBusy(false);
    }
  }

  const confirmTargetSpec = confirmingDeleteId ? specsById[confirmingDeleteId] : undefined;
  const confirmIsSpecDelete = Boolean(confirmTargetSpec?.configured);

  async function confirmDelete() {
    const id = confirmingDeleteId;
    if (!id) return;
    try {
      if (confirmIsSpecDelete) {
        await api.deleteRuntimeSpec(id);
        setSpecsById((cur) => ({ ...cur, [id]: emptySpec(id) }));
        void reloadWarnings();
      } else {
        await api.deleteMapping(id);
        setMappings((current) => (current ?? []).filter((m) => m.id !== id));
        setSpecsById((cur) => {
          const next = { ...cur };
          delete next[id];
          return next;
        });
        loadedIdsRef.current.delete(id);
      }
      setConfirmingDeleteId('');
    } catch (err) {
      showError(formatPortalError(err, t));
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
      label: t.tableStatus,
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
    const configured = Boolean(specsById[m.id]?.configured);
    const rowBusy = loadingEditFor === m.id;
    return [
      {
        key: 'edit',
        label: t.runtimeSpecEditAction,
        icon: <EditIcon fontSize="small" />,
        onClick: () => void openEdit(m),
        disabled: rowBusy,
      },
      {
        key: 'delete',
        label: configured ? t.runtimeSpecDelete : t.mappingDelete,
        color: 'error',
        icon: <DeleteIcon fontSize="small" />,
        onClick: () => setConfirmingDeleteId(m.id),
        disabled: rowBusy,
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
      // What the AGENT reports (the spec's upstream model name), not a
      // gateway-side name joined in: this column's job is showing what the
      // live stream actually says.
      value: (row) => row.model || row.spec_id,
      filter: 'text',
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
    // File mode has no admin override at all (the override lives in the
    // gateway document, which a file-mode agent never consumes -- spec §10.2:
    // "a dead button is worse than none"), and before the report GET settles
    // we do not yet know which mode this is.
    if (!writesAllowed) return [];
    const spec = specByRuntimeId.get(row.spec_id);
    // No loaded spec for this spec_id (a spec created after the list loaded, a
    // spec belonging to another application, a file-mode leftover): render NO
    // buttons rather than synthesizing a full-document body that would
    // overwrite the operator's command line.
    if (spec === undefined) return [];
    const actions: RowAction[] = [];
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
        disabled: overridesLocked || !restartableStates.has(row.state),
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

  // Create / edit sub-view (input mask) -- replaces the tabbed view entirely,
  // matching the rest of the portal's drill-down/sub-view convention.
  if (specMode !== 'list') {
    const editing = specMode !== 'create';
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
            <Typography variant="subtitle2" component="h3">
              {t.runtimeSpecMappingSection}
            </Typography>
            <Field
              id="runtime-spec-gateway-name"
              label={t.mappingGatewayName}
              value={gatewayName}
              onChange={(e) => setGatewayName(e.target.value)}
              required
            />
            <Field
              id="runtime-spec-app-name"
              label={t.mappingAppName}
              value={appName}
              onChange={(e) => setAppName(e.target.value)}
              required
            />
            <SelectField
              id="runtime-spec-status"
              label={t.tableStatus}
              value={status}
              onChange={(e) => setStatus(e.target.value as ApplicationStatus)}
            >
              {applicationStatusOptions.map((s) => (
                <option value={s} key={s}>
                  {t[applicationStatusLabelByKey[s]]}
                </option>
              ))}
            </SelectField>

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
            />
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
            <FormControlLabel
              control={
                <Checkbox checked={vramLocked} onChange={(e) => setVramLocked(e.target.checked)} />
              }
              label={t.runtimeSpecVramLocked}
            />
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
              {gpuRows.map((row, idx) => (
                <Box
                  key={idx}
                  sx={{ display: 'flex', gap: 1.5, alignItems: 'center', flexWrap: 'wrap' }}
                >
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
          server is in, and why a configured runtime might be doing nothing.
          Both are true on every tab, so neither may hide behind one. */}
      {(fileMode || featureMismatch) && (
        <Box sx={{ display: 'grid', gap: 1, mb: 2 }}>
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
            <Alert severity="warning">{`${t.runtimeParseError} (${parseError})`}</Alert>
          )}
          {fileMode && configuredSpecCount > 0 && (
            <Alert severity="warning">{t.runtimeIneffectiveSpecs}</Alert>
          )}
          {featureMismatch && (
            <Alert severity="warning">
              <Box sx={{ display: 'grid', gap: 0.25 }}>
                <span>{t.runtimeFeatureMismatch}</span>
                <Typography variant="caption">
                  {t.runtimeAgentVersion}: {reportData?.agent_version || '—'}
                </Typography>
                <Typography variant="caption">
                  {t.runtimeAgentFeatures}:{' '}
                  {reportData?.agent_features.length ? reportData.agent_features.join(', ') : '—'}
                </Typography>
              </Box>
            </Alert>
          )}
        </Box>
      )}
      <Tabs
        value={tab}
        onChange={(_e, v: Tab) => setTab(v)}
        aria-label={t.runtimeAdmin}
        sx={{ mb: 2.5 }}
      >
        <Tab label={t.runtimeSpecs} value="specs" />
        <Tab label={t.runtimeMatrix} value="matrix" />
        <Tab label={t.runtimeLimits} value="limits" />
        <Tab label={t.runtimeLiveStatus} value="status" />
      </Tabs>

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
              {warnings.length > 0 && (
                <Box sx={{ display: 'grid', gap: 1, mb: 2 }}>
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
                loading={mappingsLoading || mappingsData === null}
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
                />
              </Box>
            )
          ) : !reportReady || !coresidencyReady ? (
            // Deliberately NOT the matrix with an empty pair list: neither the
            // pairs GET (task-21 review's Critical finding) nor the report GET
            // (which decides whether this screen is writable at all) has
            // settled, so there is nothing to render or toggle from.
            <Typography color="text.secondary">{t.loading}</Typography>
          ) : (
            <RuntimeMatrix
              t={t}
              specs={matrixSpecs}
              pairs={coresidencyPairs}
              onToggle={(a, b) => void toggleCoresidency(a, b)}
              budgets={savedBudgetsByGpuIndex}
              disabled={coresidencyBusy}
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
          ) : !reportReady || !gpuBudgetsReady ? (
            // Neither GET has settled yet. `budgetRows` is still its initial
            // `[]`, indistinguishable from "no budgets configured", and Save
            // PUTs it as the FULL replacement -- so a premature click would
            // erase every previously configured budget (task-21 review's
            // Critical finding). And until the report resolves we do not know
            // whether this form should exist at all. No form, no click, no
            // write.
            <Typography color="text.secondary">{t.loading}</Typography>
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
                      key={idx}
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
            {/* The restart sequence's two terminal problems get their own
                banner rather than a chip: both leave an override in place
                that the operator now has to decide about. */}
            {restartNotice === 'timeout' && (
              <Alert severity="warning">{t.runtimeRestartTimeout}</Alert>
            )}
            {restartNotice === 'vanished' && (
              <Alert severity="warning">{t.runtimeRestartVanished}</Alert>
            )}
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
    </>
  );
}
