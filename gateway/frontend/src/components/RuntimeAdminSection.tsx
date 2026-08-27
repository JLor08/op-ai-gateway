// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useEffect, useRef, useState, type SubmitEvent } from 'react';
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
import type {
  ApplicationStatus,
  GPUBudget,
  HardwareGPU,
  PortalApplication,
  PortalModelMapping,
  PortalServer,
  PutRuntimeSpecRequest,
  RuntimeSpec,
  RuntimeSpecGPU,
} from '../api';
import type { Translation, PortalApi, MessageKey } from './shared/types';
import { formatPortalError, formatMetric } from './shared/format';
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
      // Filled by Task 22's live-status map; today this column is a
      // placeholder so the section's final shape is visible/testable.
      id: 'live_status',
      label: t.tableStatus,
      value: () => 'unknown',
      sortable: false,
      searchable: false,
      render: () => <StatusChip status="standby" label={t.runtimeStatusUnknown} />,
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
          actions={
            <Button variant="contained" startIcon={<AddIcon />} onClick={openCreate}>
              {t.runtimeSpecCreate}
            </Button>
          }
        >
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
            actions={rowActions}
            storageKey="op.runtimeSpecs"
            minWidth={900}
            labels={listTableLabels(t)}
            loading={mappingsLoading || mappingsData === null}
          />
        </Panel>
      )}
      {tab === 'matrix' && (
        <Panel
          titleId="runtime-matrix-heading"
          title={t.runtimeMatrix}
          subtitle={t.runtimeMatrixHint}
        >
          {coresidencyReady ? (
            <RuntimeMatrix
              t={t}
              specs={matrixSpecs}
              pairs={coresidencyPairs}
              onToggle={(a, b) => void toggleCoresidency(a, b)}
              budgets={savedBudgetsByGpuIndex}
              disabled={coresidencyBusy}
            />
          ) : (
            // Deliberately NOT the matrix with an empty pair list: the GET
            // hasn't settled yet, so there is no "current pairs" to render
            // or toggle from -- see the task-21 review's Critical finding.
            <Typography color="text.secondary">{t.loading}</Typography>
          )}
        </Panel>
      )}
      {tab === 'limits' && (
        <Panel
          titleId="runtime-limits-heading"
          title={t.runtimeLimits}
          subtitle={t.runtimeLimitsIntro}
        >
          {!gpuBudgetsReady ? (
            // The GET hasn't settled yet -- `budgetRows` is still its
            // initial `[]`, indistinguishable from "no budgets configured".
            // Rendering the form (and its Save button) here would let a
            // premature click PUT that empty list as the full replacement,
            // erasing every previously configured budget (task-21 review's
            // Critical finding). No form, no click, no write.
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
        <Panel titleId="runtime-status-heading" title={t.runtimeLiveStatus}>
          <Typography color="text.secondary">{t.runtimeAreaPlaceholder}</Typography>
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
