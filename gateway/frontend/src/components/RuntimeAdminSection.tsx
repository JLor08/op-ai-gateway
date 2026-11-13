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
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableRow,
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
import PushPinIcon from '@mui/icons-material/PushPin';
import ArticleIcon from '@mui/icons-material/Article';
import SpeedIcon from '@mui/icons-material/Speed';
import DragIndicatorIcon from '@mui/icons-material/DragIndicator';
import ArrowUpwardIcon from '@mui/icons-material/ArrowUpward';
import ArrowDownwardIcon from '@mui/icons-material/ArrowDownward';
import type {
  ApplicationStatus,
  BenchmarkRunDTO,
  CoResidency,
  DecisionsMode,
  EndpointMode,
  SystemOneMode,
  GPUBudget,
  HardwareGPU,
  PortalApplication,
  PortalModelMapping,
  PortalServer,
  PutRuntimeSpecRequest,
  RuntimeActionAnswer,
  RuntimeError,
  RuntimeSpec,
  RuntimeSpecGPU,
  RuntimeStatus,
  UpdateMappingRequest,
  VRAMGPUItemDTO,
  VRAMReportDTO,
} from '../api';
import type { Translation, PortalApi, MessageKey, BadgeStatus } from './shared/types';
import { errorLabelByCode, formatPortalError, formatMetric, formatDate } from './shared/format';
import { useResource } from './shared/useResource';
import { ResourceFallback, resourceState, type ResourceState } from './shared/ResourceFallback';
import { useLatestFetch } from './shared/useLatestFetch';
import { StatusChip } from './shared/StatusChip';
import { runtimeStateBadge, runtimeStateLabel } from './shared/runtimeState';
import { Panel } from './shared/Panel';
import { Field } from './shared/Field';
import { SelectField } from './shared/SelectField';
import { ApiVariantControls } from './shared/ApiVariantControls';
import { runtimeSpecTemplate } from './shared/runtimeSpecTemplate';
import {
  formSpecIsLlamaCpp,
  formSpecIsStableDiffusion,
  specFlavorsAfterTypeSwitch,
  textFlavorsOnStableDiffusion,
  tickedFlavorMissingOnApplication,
  type LoadedSpecType,
  type RuntimeSpecFlavorValues,
} from './shared/runtimeSpecTypeFlavors';
import {
  decisionsModeForKind,
  decisionsModeOrDefault,
  decisionsPassthroughOffered,
  systemOneModeForKind,
  systemOneTranslateOffered,
  defaultDecisionsMode,
  defaultSystemOneMode,
  systemOneModeOrDefault,
} from './shared/decisionsModes';
import { runtimeSpecLiveTimingsKind, runtimeSpecSendsLiveTimings } from './shared/liveTimings';
import { CONFIG_FILES_MAX, configFileIssues, type ConfigFileIssue } from './shared/configFiles';
import { ConfirmDialog } from './shared/ConfirmDialog';
import { formatArgsText, formatEnvText, parseArgsText, parseEnvText } from './shared/specText';
import { Breadcrumbs, type BreadcrumbItem } from './shared/Breadcrumbs';
import { ListTable, listTableLabels, type ListColumn } from './shared/ListTable';
import { mappingColumns, MAPPING_TABLE_STORAGE_KEY } from './shared/mappingColumns';
import type { RowAction } from './shared/RowActionsMenu';
import { useToast } from './shared/ToastProvider';
import { useColumnDrag, columnDragSx, moveColumn } from './shared/columnDrag';
import {
  latestApplicableVramRun,
  vramApplyNumber,
  vramCardCheck,
  vramCardCheckLabelKey,
} from './shared/vram';
import { MappingForm, type MappingFormValues } from './MappingForm';
import { BenchmarkSection, type BenchmarkScope } from './BenchmarkSection';
import { CoResidencyEditor, type CoResidencyItem } from './CoResidencyEditor';
import { RuntimeLogView } from './RuntimeLogView';
import { RpcMemberName, RpcMemberSection } from './RpcMemberSection';
import { BinariesSection } from './BinariesSection';
import { BinaryPicker, binaryVersionFor, useServerBinaries } from './shared/BinaryPicker';
import {
  ClusterHeadEditor,
  clusterBodyFields,
  clusterSaveError,
  clusterStateFromSpec,
  emptyClusterState,
} from './ClusterHeadEditor';
import {
  INSTANCE_PARALLELISM_MAX,
  INSTANCES_MAX,
  instanceAddress,
  instanceCountFromText,
  instanceCountText,
  instanceIndex,
  instanceSettingsBody,
  instancesSummary,
  listedIdleTimeout,
  logInstanceIndexes,
  primaryStatusRows,
  runningInstancesBySpec,
  showsAllInstanceLogs,
  restartGate,
  specStopState,
  validateInstances,
  type InstanceForm,
} from './shared/runtimeInstances';
import { minRealtimeWarning } from './shared/audioMetrics';
import {
  RUNTIME_EXEC_WSL_FEATURE,
  runtimeRowPlan,
  startInstanceLimit,
} from './shared/runtimeActions';
import { ClusterProgress } from './ClusterProgress';
import { ClusterTotalCell, ClusterWithheldNotice } from './ClusterTotalCell';
import { clusterStatusView } from './shared/clusterStatus';
import { clusterWithheldReason, isClusterHead } from './shared/clusterTotals';
import { useClusterHeadRefresh } from './shared/useClusterHeadRefresh';

// Area 1 (Task 20) is "Launch specs"; areas 2-3 (Task 21) are co-residency and
// server limits; area 4 (Task 22) is "Live status". One tab strip carries them
// all, next to the model-mapping tab.
type Tab = 'mapping' | 'specs' | 'coresidency' | 'rpc' | 'binaries' | 'limits' | 'status';

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

export {
  CLUSTER_SPEC_REFRESH_MS,
  CLUSTER_START_NOTICE_MS,
  CLUSTER_START_REFRESH_MS,
} from './shared/useClusterHeadRefresh';

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

// One row of the spec form's config-file editor (#257). `rowKey` is the same
// render-only identity as GpuRow's (mid-list removal must not shift one row's
// DOM state onto its neighbour) and never reaches the wire: buildSpecBody
// sends {name, content} only.
type ConfigFileRow = {
  rowKey: string;
  name: string;
  content: string;
};

// The sentence each config-file problem is shown as, inline on the offending
// field while typing and (the first one) as the refusal at submit.
const configFileIssueLabel: Record<ConfigFileIssue, MessageKey> = {
  name_blank: 'runtimeSpecConfigFileNameBlank',
  name_invalid: 'runtimeSpecConfigFileNameInvalid',
  name_duplicate: 'runtimeSpecConfigFileNameDuplicate',
  content_too_large: 'runtimeSpecConfigFileTooLarge',
  content_secret: 'runtimeSpecConfigFileSecretPlaceholder',
};

// The warning codes RuntimeWarnings emits today (see
// gateway/backend/internal/portal/service_runtime.go); an unmapped future
// code falls back to its raw wire value rather than a misleading label.
const runtimeWarningLabelByCode: Record<string, MessageKey> = {
  timeout_ms_below_startup_timeout: 'runtimeTimeoutWarning',
  binary_path_os_mismatch: 'runtimeBinaryPathOsMismatchWarning',
  api_flavors_not_on_application: 'runtimeFlavorNotOnApplicationWarning',
  api_flavors_text_on_stable_diffusion: 'runtimeTextOnStableDiffusionWarning',
  api_flavors_text_on_vllm_sr: 'runtimeTextOnVllmSrWarning',
};

// admin_state's three valid wire values (service_runtime.go): "" (no
// override), "force_running", "force_stopped".
const adminStateOptions: { value: string; labelKey: MessageKey }[] = [
  { value: '', labelKey: 'runtimeClearOverride' },
  { value: 'force_running', labelKey: 'runtimeForceStart' },
  { value: 'force_stopped', labelKey: 'runtimeForceStop' },
];

// The document a mapping without a spec row has (RuntimeSpec's own doc
// comment), reproduced here so a delete can be reflected without waiting on
// another GET: `configured: false`, the spec's own fields at their zero
// values, and visible_devices_mode/api_token_mode/api_token_header_source at
// their defaults ('env'/'app'/'app'), as GetRuntimeSpec synthesizes it. It
// differs from the wire in three places: both endpoint modes are
// 'passthrough' where the wire sends "" (EndpointMode cannot hold ""), the
// app-derived echoes are left false/'' rather than copied from the parent
// application, and effective_type is '' where the backend resolves 'custom'.
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
    set_visible_devices: false,
    visible_devices_mode: 'env',
    execution_mode: 'native',
    wsl_distro: '',
    gpus: [],
    ram_estimate_mb: 0,
    ram_measured_mb: 0,
    replicable: false,
    min_instances: 0,
    max_instances: 0,
    instance_parallelism: 0,
    startup_measured_ms: 0,
    effective_idle_timeout_seconds: 0,
    instances_min_exceeds_realtime: false,
    api_token_mode: 'app',
    api_token_set: false,
    api_token_header_source: 'app',
    api_token_header: '',
    app_api_token_set: false,
    app_api_token_header: '',
    api_flavors: [],
    responses_mode: 'passthrough',
    messages_mode: 'passthrough',
    decisions_mode: 'auto',
    systemone_mode: 'auto',
    responses_live_timings_enabled: false,
    type: '',
    metrics_path: '',
    context_probe_path: '',
    config_files: [],
    device_backend: 'auto',
    cluster_enabled: false,
    member_mode: 'automatic',
    cluster_network: '',
    rpc_transport: '',
    lan_exposure_ack: false,
    cluster_members: [],
    effective_type: '',
    resolved_metrics_path: '',
    resolved_context_probe_path: '',
    // A placeholder, like effective_type: the role is derived server-side from
    // verdicts this document does not carry, and nothing may group by it. The
    // co-residency editor reads every spec again when it is shown.
    role: 'other',
  };
}

// The file-mode `parse_error` codes, mapped to the sentence the operator
// reads. The field carries a CODE from a closed set and NEVER free text: the
// agent classifies its own load failure (`json_syntax`, `duplicate_spec_id`,
// `invalid_config_file_name`, `file_missing`, `read_failed`, plus an
// `unclassified` floor) and the gateway allow-lists the five real codes,
// degrading anything else to its own generic constant. That contract is
// THREE-SIDED and no compiler checks either seam -- adding a code means the
// agent's set, the gateway's allow-list, and this map plus its two i18n keys.
//
// `invalid_config_file_name` (#257) is a structural failure inside the
// document: a spec's `config_files` carries a name that is not one plain file
// name, or two names that collide case-insensitively.
//
// `file_missing` and `read_failed` are named for parsing only because the WIRE
// FIELD is: the agent raises them before any parsing happens, for a
// runtime.json that is not there or cannot be read. They exist because the
// agent used to report neither, so a file-mode server with a missing file
// rendered exactly like one with no specs configured -- the empty state and
// the absent state, told apart nowhere.
const runtimeParseErrorReasonByCode: Record<string, MessageKey> = {
  json_syntax: 'runtimeParseErrorJsonSyntax',
  duplicate_spec_id: 'runtimeParseErrorDuplicateSpecId',
  invalid_config_file_name: 'runtimeParseErrorInvalidConfigFileName',
  file_missing: 'runtimeParseErrorFileMissing',
  read_failed: 'runtimeParseErrorReadFailed',
};

// Unlike runtimeStateLabel (shared/runtimeState.ts), an unrecognised value does NOT fall back to
// the raw wire value: the whole point of the closed set is that the operator
// is never shown an identifier. A code this build does not know -- a newer
// agent, the agent's `unclassified` floor, or the gateway's generic constant
// for a value outside its allow-list -- says so in a sentence instead.
function runtimeParseErrorReason(code: string, t: Translation): string {
  const key = runtimeParseErrorReasonByCode[code];
  return key ? t[key] : t.runtimeParseErrorUnknown;
}

/**
 * Task 6 (probe-reachability-and-model-status): one probe's chip status,
 * visible label and tooltip, shared by both the metrics ("M") and context
 * ("C") chips in the live-status table's "Probes" column so their
 * state -> appearance mapping has exactly one source of truth.
 *
 * `""` (not reported -- a legacy agent, or a non-running child) renders
 * nothing at all: `undefined` here, not a placeholder chip.
 *
 * Colour choice, given runtimeState.ts's constraint that the portal has only
 * THREE status colours (success/watch/standby -- no red anywhere):
 *  - "ok"          -> success (green): the endpoint was reachable and queried.
 *  - "unreachable" -> watch (the portal's only non-green, non-neutral
 *                     colour): a path IS configured/derived but the last
 *                     probe failed -- the "forgot llama.cpp --metrics" case,
 *                     and the one state this column exists to surface.
 *  - "na" and any value this build does not recognise -> standby (neutral
 *                     grey): "na" means this runtime type has no such
 *                     endpoint at all (Ollama has no /metrics) and must NEVER
 *                     look like "unreachable", or a healthy Ollama model
 *                     looks broken. `error` is deliberately NOT used here --
 *                     statusClassByKey collapses it onto the same grey as
 *                     standby (components/shared/status.ts), which would make
 *                     "unreachable" indistinguishable from "na" instead of
 *                     standing out as the warning it is.
 */
function probeChipInfo(
  probeState: string,
  prefix: string,
  t: Translation,
): { status: BadgeStatus; label: string; tooltip: string } | undefined {
  if (probeState === '') return undefined;
  if (probeState === 'ok') {
    return {
      status: 'success',
      label: `${prefix} ${t.runtimeProbeStateOk}`,
      tooltip: t.runtimeProbeTooltipOk,
    };
  }
  if (probeState === 'unreachable') {
    return {
      status: 'watch',
      label: `${prefix} ${t.runtimeProbeStateUnreachable}`,
      tooltip: t.runtimeProbeTooltipUnreachable,
    };
  }
  if (probeState === 'router') {
    // A llama.cpp multi-model router server: this endpoint cannot answer for
    // the server as a whole (issue #55). Neutral, not a warning -- it is a
    // valid server shape, not a fault -- but its own label and tooltip so the
    // operator is not left guessing, which was the whole point of the issue.
    return {
      status: 'standby',
      label: `${prefix} ${t.runtimeProbeStateRouter}`,
      tooltip: t.runtimeProbeTooltipRouter,
    };
  }
  // "na", and any future/unrecognised non-empty value: neutral, never a
  // false warning for a probe state this build does not know.
  return {
    status: 'standby',
    label: `${prefix} ${t.runtimeProbeStateNa}`,
    tooltip: t.runtimeProbeTooltipNa,
  };
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
//   waiting  -> the stream has to report this spec `stopped`: every one of
//               its instance rows, since force_stopped drains them all at
//               once and in no fixed order (specStopState)
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
  visibleDevicesMode: 'env' | 'args';
  idleTimeoutSeconds: number;
};

type ReportConfig = {
  maxProcesses: number | null;
  budgets: { index: number; budgetMb: number }[];
  specs: ReportSpec[];
  /** Something, at some level, did not have the shape we expected. */
  unrecognised: boolean;
};

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

// A spec entry with no usable identity cannot be rendered as a row at all
// (it would collide with every other identity-less entry as a React key and
// could not be told apart from the others) -- those are reported as
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
    // Absent or unrecognised defaults to "env" — the same default the backend
    // and agent apply (see RuntimeSpecDTO.VisibleDevicesMode).
    visibleDevicesMode: value.visible_devices_mode === 'args' ? 'args' : 'env',
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
  return out;
}

// The busy key of a live-status Start or Stop: a start is the spec's (the
// gateway picks the instance), a stop the instance's.
function runtimeActionKey(action: 'start' | 'stop', row: RuntimeStatus): string {
  return action === 'start' ? `start:${row.spec_id}` : `stop:${row.spec_id}#${instanceIndex(row)}`;
}

// The override actions' PUT body. putRuntimeSpec takes the ENTIRE spec
// (Omit<RuntimeSpec, 'configured' | 'id' | 'mapping_id'>), never a patch, so
// the body is built by spreading the spec as the action has just read it
// (`readSpecForWrite`) and replacing exactly one field. A synthesized or
// defaulted body would silently overwrite the operator's configured
// binary/args/env/gpus/timeouts on a single override click -- the same
// full-replace hazard as the co-residency and GPU-budget writes (task-21
// review). The rest-spread is deliberate over an explicit field list: a field
// added to RuntimeSpec later carries through by itself instead of being
// silently dropped here.
function specBodyWithAdminState(spec: RuntimeSpec, adminState: string): PutRuntimeSpecRequest {
  // api_token_set/app_api_token_set/app_api_token_header, the RuntimeSpec
  // Type block's own effective_type/resolved_metrics_path/
  // resolved_context_probe_path and the derived `role` are READ-ONLY echoes
  // (PutRuntimeSpecRequest excludes all seven, same as configured/id/
  // mapping_id) -- dropped here too
  // so an override click never round-trips them back as if they were
  // writable. type/metrics_path/context_probe_path (the writable trio) DO
  // belong in `rest`: an override must preserve them unchanged, exactly like
  // every other field this full-document PUT carries verbatim. So does
  // config_files (#257): the PUT treats an absent list as "none", so dropping
  // it here would delete the operator's files on a Start/Stop/Clear click.
  // ram_estimate_mb is the same kind of writable field and rides `rest` for
  // the same reason: the PUT applies an absent estimate as 0 and clears the
  // stored one, so an override click must restate it. ram_measured_mb is the
  // opposite: agent-owned with no request counterpart (PutRuntimeSpecRequest
  // excludes it), so it is dropped here rather than echoed back. The instance
  // settings split the same way: replicable/min_instances/max_instances/
  // instance_parallelism are the operator's and ride `rest`, while
  // startup_measured_ms (agent-owned), effective_idle_timeout_seconds and
  // instances_min_exceeds_realtime (both derived on every read) have no
  // request counterpart and are dropped.
  //
  // The cluster configuration (cluster_enabled, member_mode, cluster_network,
  // rpc_transport, lan_exposure_ack, device_backend, cluster_members) rides
  // `rest`: an absent cluster_enabled turns the cluster off and clears its
  // membership. Its read view (the totals, the network and transport
  // defaults, the warnings and the orchestration status) is derived on every
  // read and dropped.
  //
  // responses_live_timings_enabled is dropped for a DIFFERENT reason: it is
  // writable, but on the request shape it is OPTIONAL, and absent means "keep
  // the stored value". Dropping it is therefore exactly what an override
  // click wants -- and it is also the only safe thing to send, because the
  // store is policy-free by design and a stored true can outlive a retype to
  // an incapable kind; restating such a pair would earn a 400 and turn a
  // Start/Stop/Clear click into a failed write.
  const {
    configured,
    id,
    mapping_id,
    api_token_set,
    app_api_token_set,
    app_api_token_header,
    effective_type,
    resolved_metrics_path,
    resolved_context_probe_path,
    role,
    responses_live_timings_enabled,
    ram_measured_mb,
    startup_measured_ms,
    effective_idle_timeout_seconds,
    instances_min_exceeds_realtime,
    cluster_vram_total_mb,
    cluster_ram_total_mb,
    cluster_totals_complete,
    cluster_vram_total_complete,
    cluster_ram_total_complete,
    cluster_network_default,
    cluster_transport_default,
    cluster_warnings,
    cluster_withheld_reason,
    cluster_status,
    ...rest
  } = spec;
  return { ...rest, admin_state: adminState };
}

// runtimeSpecHealthPathDefault decides two things in THIS form, both keyed on
// the WRITABLE Type select alone (never on the read-only effective_type
// echo, which is unknown until the backend resolves it): what the
// health-path field DISPLAYS while untouched (its initial value, and the
// target a type switch moves an untouched field to when the NEW type is
// explicit) and the field's placeholder for an explicit type. On a FIRST
// write, buildSpecBody sends '' instead of the displayed text whenever the
// operator never typed into the field (healthPathEdited), so the backend's
// OWN default decides -- deliberately NOT by comparing the field to this
// function's answer for the CURRENT type, since a type switch can leave the
// field showing a PRIOR type's default (see the Type select below) and that
// would misread as an edit. The backend's default matters here because it is
// keyed on the spec's EFFECTIVE type: under Auto this form cannot know what
// `binary` will detect to (that detection is Go-only), so this function's
// answer for Auto is necessarily a guess ('/health') that would be WRONG for
// an Auto + sd-server-binary spec -- sending it verbatim pins the exact path
// measured to 404 and gets the child killed. For the same reason a switch TO
// Auto moves nothing, and Auto shows no placeholder. This function otherwise
// mirrors runtimeSpecHealthPathDefault in
// gateway/backend/internal/portal/service_runtime.go, the actual source of
// truth: sd-server answers GET /health with 404 (measured), so its health
// path defaults to /v1/models instead of the stock /health every other type
// gets.
function runtimeSpecHealthPathDefault(type: RuntimeSpec['type']): string {
  return type === 'stable_diffusion_cpp' ? '/v1/models' : '/health';
}

// The Type select's helper text: the note of a kind whose defaults diverge
// from the stock text server's (sd-server and audio.cpp each serve one non-text
// API), nothing for every other kind, Auto included.
function runtimeSpecTypeNote(type: RuntimeSpec['type'], t: Translation): string | undefined {
  if (type === 'stable_diffusion_cpp') return t.runtimeSpecTypeStableDiffusionCppNote;
  if (type === 'audio_cpp') return t.runtimeSpecTypeAudioCppNote;
  if (type === 'vllm_sr') return t.runtimeSpecTypeVllmSrNote;
  return undefined;
}

function basename(path: string): string {
  const parts = path.split(/[\\/]/);
  return parts[parts.length - 1] || path;
}

// The short table label of a spec's execution mode. A mode this build does not
// know yet (a later Docker mode, say) shows its raw value rather than nothing,
// so the column and its filter stay truthful across a gateway/portal skew.
function executionModeLabel(mode: string, t: Translation): string {
  if (mode === '') return '—';
  if (mode === 'native') return t.runtimeSpecExecutionModeNative;
  if (mode === 'wsl') return t.runtimeExecutionWSL;
  return mode;
}

// The VRAM the admission arithmetic actually charges for one GPU row of a
// spec, which is NOT always the operator's estimate: the larger of the agent's
// measurement and the operator's estimate, so the estimate is a floor and a
// higher measurement raises the charge. A missing measurement (0 = UNKNOWN,
// never "needs none") therefore falls back to the estimate. The table shows this
// number so it cannot disagree with what admission does with the same spec;
// this is the one place the rule lives.
function effectiveGpuVram(g: RuntimeSpecGPU): number {
  return Math.max(g.vram_measured_mb, g.vram_estimate_mb);
}

// The sum of every GPU row's effective VRAM: what the spec demands across the
// host's cards in total. A card whose demand is still unknown (0) adds nothing,
// so the total of a partly unknown spec is a floor; 0 means every card is
// unknown (or there is none).
function totalGpuVram(spec: RuntimeSpec | undefined): number {
  if (!spec) return 0;
  return spec.gpus.reduce((sum, g) => sum + effectiveGpuVram(g), 0);
}

// RAM has no per-card rows: the larger of the agent's measurement and the
// operator's estimate, the same max() rule as the VRAM above. `|| 0` also
// covers a document from a gateway that predates the fields, which would
// otherwise surface as NaN.
//
// `live` is the spec's row on the live status stream, when it has one: its
// ram_measured_mb is the running process's CURRENT resident RAM, which is
// fresher than the durable spec.ram_measured_mb (that one is only re-read when
// the specs are next fetched). It joins the same max() as a third operand, so a
// live figure below a larger durable one does not shrink the cell, and an
// absent or 0 live figure (UNKNOWN: no measurer, or no live process) simply
// drops out, leaving the durable value -- which is also all a spec with no live
// row (a stopped one) ever has.
function effectiveRam(spec: RuntimeSpec | undefined, live?: RuntimeStatus): number {
  return Math.max(
    live?.ram_measured_mb || 0,
    spec?.ram_measured_mb || 0,
    spec?.ram_estimate_mb || 0,
  );
}

function formatGpus(spec: RuntimeSpec | undefined): string {
  if (!spec || spec.gpus.length === 0) return '—';
  return spec.gpus.map((g) => `${g.index}: ${effectiveGpuVram(g)} MB`).join(', ');
}

// A megabyte quantity for the two summary columns: the em-dash when nothing is
// known (0 is the UNKNOWN sentinel throughout this feature, never a real zero).
function formatMb(mb: number): string {
  return mb > 0 ? `${mb} MB` : '—';
}

function renderBoolChip(active: boolean, label: string) {
  return active ? <Chip size="small" variant="outlined" label={label} /> : '–';
}

// The GPU-assignment column collapses set_visible_devices + visible_devices_mode
// into ONE value an operator can read at a glance: off, or the mechanism that
// enforces it (arguments vs environment variable).
type VisibilityValue = 'off' | 'args' | 'env';

function visibilityValue(setVisible: boolean, mode: 'env' | 'args'): VisibilityValue {
  if (!setVisible) return 'off';
  return mode === 'args' ? 'args' : 'env';
}

function visibilityValueLabel(v: VisibilityValue, t: Translation): string {
  switch (v) {
    case 'off':
      return t.runtimeSpecVisibleDevicesValueOff;
    case 'args':
      return t.runtimeSpecVisibleDevicesValueArgs;
    default:
      return t.runtimeSpecVisibleDevicesValueEnv;
  }
}

// The two enforced states get an outlined chip; "off" is muted so a glance
// separates "enforced (and how)" from "not enforced".
function renderVisibilityChip(v: VisibilityValue, t: Translation) {
  return (
    <Chip
      size="small"
      variant="outlined"
      label={visibilityValueLabel(v, t)}
      sx={v === 'off' ? { color: 'text.secondary' } : undefined}
    />
  );
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
// the next line), "--port=50395", and "--port 50395" squeezed onto one line --
// and sd-server's OWN listen flag,
// "--listen-port", which names the exact same endpoint under a different
// spelling (sd-server has no bare "--port" at all). Only those two EXACT
// names -- never "--rpc-port" or another "*-port", which name a DIFFERENT
// endpoint that a spec may legitimately pin to a fixed number. The separator
// is "=" (optionally padded) or whitespace, written so that no quantifier
// overlaps the value's \S+ -- a "[=\s]+" run would, and backtracks
// super-linearly on a long run of "=".
const portFlagPattern = /^--?(?:listen-)?port(?:(?:\s*=\s*|\s+)(\S+))?$/i;

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
// task's UI has no live-status view yet to surface a failure there), so
// the portal form must refuse
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
// The per-backend reference table: the Env var name each of these
// three model servers reads for its own API key, the equivalent --api-key
// argument, and the client header the token then answers to. Shown whenever
// api_token_mode !== 'off', regardless of which backend a spec actually
// launches -- see the "IMPORTANT design resolution" note above the table's
// render site for why this cannot be keyed off app.Type.
const apiTokenBackendRows: {
  key: string;
  backendKey: MessageKey;
  envKey: MessageKey;
  argsKey: MessageKey;
  clientKey: MessageKey;
}[] = [
  {
    key: 'vllm',
    backendKey: 'runtimeSpecApiTokenRowVllmBackend',
    envKey: 'runtimeSpecApiTokenRowVllmEnv',
    argsKey: 'runtimeSpecApiTokenRowVllmArgs',
    clientKey: 'runtimeSpecApiTokenRowVllmClient',
  },
  {
    key: 'llama-cpp',
    backendKey: 'runtimeSpecApiTokenRowLlamaCppBackend',
    envKey: 'runtimeSpecApiTokenRowLlamaCppEnv',
    argsKey: 'runtimeSpecApiTokenRowLlamaCppArgs',
    clientKey: 'runtimeSpecApiTokenRowLlamaCppClient',
  },
  {
    key: 'tgi',
    backendKey: 'runtimeSpecApiTokenRowTgiBackend',
    envKey: 'runtimeSpecApiTokenRowTgiEnv',
    argsKey: 'runtimeSpecApiTokenRowTgiArgs',
    clientKey: 'runtimeSpecApiTokenRowTgiClient',
  },
];

// What the co-residency tab may say about the roles its editor groups by: the
// mappings GET failing hard is `error` (nothing to read the specs of), the
// spec fan-out not having settled is `loading`, anything else `ready`.
function coresidencyRolesStateOf(
  mappingsStatus: ResourceState,
  rolesRead: { unreadable: ReadonlySet<string> } | null,
): ResourceState {
  if (mappingsStatus === 'error') return 'error';
  if (rolesRead === null) return 'loading';
  return 'ready';
}

export function RuntimeAdminSection({
  t,
  api,
  // Area 1 doesn't read this (server context already folds into `trail`
  // before this component ever mounts, same as MappingSection); the later
  // areas are server-scoped -- the "server limits" tab needs server.id and
  // reads/writes server.runtime_max_processes, and so does live status.
  server,
  application,
  trail = [],
  pollIntervalMs,
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
    // The RPC tab's manual member specs (the server settings use updateServer
    // below; the live member list reads this section's status stream). Its
    // consent picker lists the admin-tier groups the caller can name.
    | 'rpcMemberSpecs'
    | 'serverAdminGroupCandidates'
    | 'createRPCMemberSpec'
    | 'updateRPCMemberSpec'
    | 'deleteRPCMemberSpec'
    // The Binaries tab and the binary pickers of the spec form and the RPC tab.
    | 'serverBinaries'
    | 'createServerBinary'
    | 'updateServerBinary'
    | 'deleteServerBinary'
    // The cluster head editor's member picker (it reads member GPUs through
    // serverHardware below).
    | 'clusterCandidates'
    | 'applicationClusterCandidates'
    | 'resetClusterMemberUsage'
    // The live-status Start and Stop: transient, nothing is written.
    | 'startRuntime'
    | 'stopRuntime'
    // T3: the live-status tab's per-row log view.
    | 'subscribeRuntimeLogs'
    // Task 21 (server limits): the process-limit field is saved
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
    | 'probeMappingVram'
    // The mapping tab's per-row "Benchmark" action opens the consolidated
    // benchmark sub-view (`BenchmarkSection`), which needs its OWN slice of the
    // api. Exactly the methods it calls -- `mappings` and `benchmarkStatus` are
    // already listed above, so this adds only the remaining six.
    | 'applications'
    | 'benchmarkApplication'
    | 'benchmarkMapping'
    | 'benchmarkServer'
    | 'mappingBenchmarks'
    | 'subscribeBenchmark'
  >;
  server: PortalServer;
  application: PortalApplication;
  trail?: BreadcrumbItem[];
  /**
   * Cadence (ms) of the context probe's benchmark-status poll, forwarded
   * verbatim to `MappingForm`. Exactly the prop `MappingSection` carries for
   * the same reason and with the same reach: nothing in the render tree above
   * either screen passes it, so in the running portal it is `undefined` and the
   * shared helper's ~2 s cadence applies. It exists so a test can drive the
   * probe to completion without a real wait -- and the probe is the one part of
   * the shared mask with an async loop, so without it that loop is reachable on
   * the ordinary screen's tests and not on this tab's, which is a coverage gap
   * rather than a saved prop.
   */
  pollIntervalMs?: number;
}>) {
  const { showError, showSuccess } = useToast();
  const [tab, setTab] = useState<Tab>('specs');
  const [specMode, setSpecMode] = useState<SpecMode>('list');
  const [busy, setBusy] = useState(false);
  const [confirmingDeleteId, setConfirmingDeleteId] = useState('');
  // The cluster head whose Stop awaits its confirmation, and the checkbox in it.
  const [clusterStop, setClusterStop] = useState<{ row: RuntimeStatus; spec: RuntimeSpec } | null>(
    null,
  );
  const [stopMembersToo, setStopMembersToo] = useState(false);
  const [loadingEditFor, setLoadingEditFor] = useState('');
  // Bumped by every launch-spec form open (openEdit, openCreate): an Edit GET
  // fills the form only while it is still the latest one.
  const editOpenSeqRef = useRef(0);
  // The mapping row the model-mapping tab is editing, or null. Kept separate
  // from `specMode`: they are two different sub-views over two different
  // documents, and the tab strip is hidden while either is open, so neither can
  // start the other.
  const [mappingEdit, setMappingEdit] = useState<PortalModelMapping | null>(null);
  // When set, the consolidated benchmark sub-view is shown, pre-scoped to the
  // clicked mapping row's model. A third mutually-exclusive sub-view alongside
  // `mappingEdit` and `specMode`; the tab strip is hidden while any is open.
  const [benchmarkScope, setBenchmarkScope] = useState<BenchmarkScope | null>(null);

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
  // This resource feeds all four tabs (the list, the co-residency model list, the
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
   * `commitSpecRead`; nothing else may call `setSpecsById`, and those three
   * keep `cachedSpecsRef` in step with it.
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
   *    back again: the row then offers Pin / Restart and
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
  // The entries of `specsById` as the last accepted commit left them. Code
  // that runs between renders reads the cache here (`readSpecForWrite`):
  // `specsById` in its closure is the copy of the render it started in.
  const cachedSpecsRef = useRef<Map<string, RuntimeSpec>>(new Map());
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
    cachedSpecsRef.current.set(mappingId, spec);
    setSpecsById((cur) => ({ ...cur, [mappingId]: spec }));
  }
  /** The mapping-delete half: the entry is gone, and that fact is ordered too. */
  function forgetSpecCache(mappingId: string, ticket: number) {
    if (!acceptSpecWrite(mappingId, ticket)) return;
    cachedSpecsRef.current.delete(mappingId);
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
   * mirror image of C2, and the same poison: an override write takes ticket 1, the
   * operator switches tab (never disabled) and hits Edit, the PUT commits
   * `force_stopped`, and the GET lands with `issued(1) === seen(1)` and writes
   * its pre-PUT snapshot over it. Snapshotting `committed` refuses that: the
   * counter is monotone, so "unchanged" means "nothing committed inside my
   * window" no matter when the write was issued.
   */
  function beginSpecRead(mappingId: string): number {
    return specCommittedSeqRef.current.get(mappingId) ?? 0;
  }
  /** Commits a read unless a write committed inside it; true when it did. */
  function commitSpecRead(mappingId: string, seen: number, spec: RuntimeSpec): boolean {
    if ((specCommittedSeqRef.current.get(mappingId) ?? 0) !== seen) return false;
    cachedSpecsRef.current.set(mappingId, spec);
    setSpecsById((cur) => ({ ...cur, [mappingId]: spec }));
    return true;
  }

  // ---- Area 2: co-residency ----------------------------------------------
  // The stored document is `{mode, pairs}`: the per-server mode and every
  // EXPLICIT pair with its verdict. CoResidencyEditor is controlled and reports
  // the COMPLETE next document on every edit (all explicit pairs + the mode); an
  // edit is saved by PUTting exactly that -- a full replace, never a delta and
  // never a mode-only update (that would erase every explicit verdict). That is
  // how the backend models it, and it avoids lost updates between two admins
  // editing concurrently. Canonicalisation (which of the two mapping ids sorts
  // first) is the editor's job on the way out and the BACKEND's on the way in
  // (SetCoResidency sorts each pair before storing/comparing).
  const {
    data: coresidencyData,
    setData: setCoresidencyData,
    error: coresidencyError,
    loading: coresidencyLoading,
    reload: reloadCoresidency,
  } = useResource(() => api.runtimeCoresidency(application.id), [api, application.id, t], t);
  // `null` (not loaded yet) and a document with no pairs (loaded, genuinely
  // empty) are DIFFERENT facts -- collapsing them with a defaulted empty
  // document would make an edit that lands before the GET settles compute its
  // full-replace PUT from nothing, wiping every pair a previous admin already
  // saved. `ready` gates both rendering (a loading message instead of the
  // editor) and writing (see the missing-loading-state fix, task-21 review
  // round 1).
  //
  // It is `resourceState`'s `ready` rather than the old
  // `!loading && data !== null`, which is STRICTLY LOOSER: that test reports a
  // failed RELOAD over an existing payload as ready
  // (shared/ResourceFallback.tsx), and this loader's deps include `t`, so a
  // language switch whose re-GET fails reaches exactly that state with the
  // component still mounted. The editor would then render, editable, off a
  // document we just failed to refresh -- and an edit PUTs the FULL replacement,
  // so a pair another admin added meanwhile would be silently deleted. That is
  // task-21's Critical reached through a different door, so this gate gets
  // tighter here, never looser.
  const coresidencyStatus = resourceState({
    loading: coresidencyLoading,
    error: coresidencyError,
    data: coresidencyData,
  });
  const coresidencyReady = coresidencyStatus === 'ready';
  useEffect(() => {
    if (coresidencyError) showError(coresidencyError);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [coresidencyError]);

  // The editor groups by each spec's DERIVED role (RuntimeSpec.role -- the
  // backend's routing.DeriveRole over the spec's effective type and its
  // mapping's verdicts), read off the per-mapping spec cache. Not the mapping's
  // capability flags: those are an AND across every mapping of a name and can
  // disagree with the best-ranked role.
  //
  // The cache cannot be trusted for it as it stands, because that cache is
  // filled once per mount (`loadedIdsRef` never re-fetches) and the role moves
  // for reasons it never sees: an operator marks a model embedding on the
  // mapping tab (`updateMapping`, which touches no spec), a spec is deleted
  // (the cache is rewritten with a locally built document whose role is only a
  // placeholder), or a probe writes a verdict in the background (an Ollama
  // `embedding`, an audio.cpp `transcription`). A role read once and kept would
  // file the model under its OLD group until the page was reloaded, and a group
  // bulk-set would then write verdicts for the wrong set of models. So every
  // time the editor is shown, every spec is read AGAIN and the editor waits for
  // those reads: the role is read when it is needed instead of invalidated by
  // each path that can change it (and a path added later cannot be missed).
  //
  // The reads go through the cache's ordered-read primitives (`beginSpecRead` /
  // `commitSpecRead`), so one that overlaps a write of this section's own is
  // refused exactly like the lazy one. A spec that cannot be read is filed under
  // "other" -- never under a role this entry could not confirm.
  const coresidencyMappingIds = mappings.map((m) => m.id).join('\u0000');
  // Keyed on the list being IN HAND, not on its status: a reload (a language
  // switch re-GETs it) passes through `loading` with the previous list intact,
  // and that must not re-read every spec.
  const rolesReadable = tab === 'coresidency' && mappingsData !== null;
  const [rolesRead, setRolesRead] = useState<{ unreadable: ReadonlySet<string> } | null>(null);
  useEffect(() => {
    if (!rolesReadable) return undefined;
    let cancelled = false;
    (async () => {
      const unreadable = new Set<string>();
      await Promise.all(
        mappings.map(async (m) => {
          const seen = beginSpecRead(m.id);
          try {
            const spec = await api.runtimeSpec(m.id);
            if (!cancelled) commitSpecRead(m.id, seen, spec);
          } catch (err) {
            unreadable.add(m.id);
            if (!cancelled) showError(formatPortalError(err, t));
          }
        }),
      );
      if (!cancelled) setRolesRead({ unreadable });
    })();
    return () => {
      cancelled = true;
      // Leaving the tab (or the mapping set changing) ends this read's claim to
      // be current: the next time the editor is shown starts from "not read".
      setRolesRead(null);
    };
    // `mappings` is covered by its id key: a language switch re-GETs the list
    // into a new array with the same ids and must not re-read every spec.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [rolesReadable, coresidencyMappingIds, api]);

  const coresidencyItems: CoResidencyItem[] = mappings.map((m) => ({
    id: m.id,
    name: m.gateway_model_name,
    role: rolesRead?.unreadable.has(m.id) ? 'other' : (specsById[m.id]?.role ?? 'other'),
  }));
  // A hard-failed mappings GET never reaches the read above, and the
  // screen-wide banner already says why; the tab must not claim to be loading.
  const coresidencyRolesState = coresidencyRolesStateOf(mappingsStatus, rolesRead);

  // Serializes edits: while a PUT is in flight, the editor is disabled (mirrors
  // Area 3's limitsBusy) so a second edit cannot compute its next full-replace
  // document from state a still-outstanding first response might later
  // overwrite. Chosen over a queue (buffering edits and replaying them once the
  // in-flight PUT settles) because every other full-replace write in this
  // section (spec save, limits save) already uses the same "disable until the
  // response lands" idiom, and a silently queued edit is easy to mistake for a
  // dropped one -- the operator sees the editor visibly busy instead.
  const [coresidencyBusy, setCoresidencyBusy] = useState(false);

  async function saveCoresidency(next: CoResidency) {
    // Unreachable while the editor is gated on `ready`; stated so the rollback
    // below never has to restore a document that was never loaded.
    if (coresidencyData === null) return;
    const previous = coresidencyData;
    setCoresidencyData(next); // optimistic
    setCoresidencyBusy(true);
    try {
      const stored = await api.putRuntimeCoresidency(application.id, next);
      setCoresidencyData(stored);
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
  // co-residency document above: `budgetRows` starts `[]` and would look
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

  // The server as the RPC tab last saved it: the tab remounts on every tab
  // switch and the `server` prop is only as fresh as the parent's last fetch.
  const [rpcSaved, setRpcSaved] = useState<{ server: PortalServer; propKey: string } | null>(null);
  const rpcContentKey = `${server.id}|${server.rpc_member_enabled ?? false}|${[...(server.rpc_allowed_head_groups ?? [])].sort((a, b) => a.localeCompare(b)).join(',')}`;
  // The override lasts only while the prop still shows what it showed at save
  // time; once the prop's RPC content moves, the prop is fresher.
  const rpcSavedServer = rpcSaved?.propKey === rpcContentKey ? rpcSaved.server : null;
  const [maxProcesses, setMaxProcesses] = useState(() => server.runtime_max_processes ?? 0);
  const [limitsBusy, setLimitsBusy] = useState(false);

  function addBudgetRow() {
    budgetRowsDirtyRef.current = true;
    setBudgetRows((rows) => {
      const used = new Set(rows.map((r) => r.index));
      // Prefer the next telemetry-reported GPU not yet configured -- the
      // agent already told us its index AND its total VRAM, so the operator
      // shouldn't have to look either up
      // (docs/architecture/cross-cutting/agent-runtime-manager.md §11.5).
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

  // The name a row is shown by: its mapping's gateway name, a cluster
  // member's display name, else what the agent reports.
  function statusRowName(row: RuntimeStatus): string {
    return gatewayNameFor(row) || row.display_name || row.model || row.spec_id;
  }

  // The spec whose managed-process output is being watched, or null. Opening
  // this is what MAKES the agent stream that spec (the gateway asks on the
  // first viewer and stops on the last), so it is deliberately per-row state
  // that clears on close rather than a mounted-but-hidden panel: a hidden
  // panel would keep an agent streaming output nobody is reading.
  const [logSpec, setLogSpec] = useState<{
    specId: string;
    title: string;
    allInstances: boolean;
    instances: number[];
    initialTab: number;
  } | null>(null);
  const [overrideBusy, setOverrideBusy] = useState(false);
  // The live-status Start and Stop requests in flight, keyed per action and
  // row (runtimeActionKey). Each disables its own button, and a start the
  // spec's Stop as well; a start or stop writes nothing, so it does not take
  // the override lock the PUTs share.
  const [runtimeActionsBusy, setRuntimeActionsBusy] = useState<ReadonlySet<string>>(
    () => new Set(),
  );
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
  // Spec DELETEs in flight, counted per mapping id (confirmDelete's spec
  // branch); a count because a second confirm can overlap the first.
  const specDeletesInFlightRef = useRef<Map<string, number>>(new Map());
  function specDeletesInFlight(mappingId: string): boolean {
    return (specDeletesInFlightRef.current.get(mappingId) ?? 0) > 0;
  }
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
  // ---- Cluster heads: keep their status current -------------------------
  // The spec list, its editor and the Live-Status tab show a head's cluster
  // status off its spec (see useClusterHeadRefresh).
  const clusterRefresh = useClusterHeadRefresh({
    api,
    mappings,
    specsById,
    statusRows,
    view: tab,
    active: tab === 'specs' || tab === 'status',
    beginRead: beginSpecRead,
    commitRead: commitSpecRead,
    mountedRef,
  });

  /** Whether override write `run` is over: unmounted, or abandoned. */
  function overrideRunOver(run: number): boolean {
    return !mountedRef.current || overrideRunRef.current !== run;
  }
  /** Whether restart sequence `run` is over: unmounted, or abandoned. */
  function restartRunOver(run: number): boolean {
    return !mountedRef.current || restartRunRef.current !== run;
  }
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

  // Advances the sequence from the live stream: a frame in which THIS spec is
  // `stopped` is the only signal that force_stopped actually took effect.
  useEffect(() => {
    if (restart === null || restart.phase !== 'waiting') return;
    // Every instance's row, not one of them: force_stopped drains all of a
    // spec's instances at once, in no fixed order, so neither instance 0 nor
    // any other stopping first says the spec is down -- clearing the override
    // then would let a sibling still draining start again (specStopState).
    const stop = specStopState(statusRows, restart.specId);
    if (stop === 'absent') {
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
    if (stop === 'stopped' && frameSeqRef.current > restart.waitFrom) {
      // A spec DELETE for this mapping still in flight holds the clear: the
      // clear's own read can still answer the configured document while the
      // DELETE is in flight, and the spec PUT is an upsert, so a clear handled
      // after the DELETE would create the deleted spec again. This check
      // holds a clear whose DELETE was confirmed before it started;
      // finishRestart checks again after its read, for a DELETE confirmed
      // while the read was in flight, and puts the flow back in `waiting`. A
      // frame after the DELETE has settled decides: after a committed delete
      // the clear finds configured: false and finishRestart reports the spec
      // vanished, after a failed one the spec is still there to clear. The
      // deadline above still bounds the wait.
      if (specDeletesInFlight(restart.mappingId)) return;
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

  /**
   * The spec as the server stores it now, read for one override write to
   * build its body from. The cache is loaded once per mapping, and only this
   * section's own reads and writes refresh it, so it can be older than the
   * stored document: a benchmark run unpins the server's pinned specs for its
   * duration and pins them again after it, and a body built from a cache
   * loaded during the run would write `pinned: false` back. Committed to the
   * cache like any other read, ordered against this mapping's writes
   * (`commitSpecRead`).
   *
   * Returns the newest spec this section knows. That is the read, unless a
   * write of this section committed while it was in flight: the cache then
   * refuses the read, and the cache's current entry is the newer document.
   * After a form save that is the saved document, so the body keeps what the
   * form changed. After a spec delete it is the unconfigured document, and
   * after a mapping delete there is no entry, which reads as the unconfigured
   * document too; either way the caller writes nothing.
   */
  async function readSpecForWrite(mappingId: string): Promise<RuntimeSpec> {
    const seen = beginSpecRead(mappingId);
    const fresh = await api.runtimeSpec(mappingId);
    if (commitSpecRead(mappingId, seen, fresh)) return fresh;
    return cachedSpecsRef.current.get(mappingId) ?? emptySpec(mappingId);
  }

  /**
   * Runs one live-status Start or Stop: marks `key` busy while the request is
   * in flight, then says what the agent answered, or why it refused, through
   * the screen's toasts. Bounded by the gateway, which answers a start within
   * its early-answer window (a longer start answers 202 and goes on in the
   * background) and bounds a stop's agent call, so it needs no watchdog of
   * its own.
   */
  async function runRuntimeAction(
    key: string,
    call: () => Promise<RuntimeActionAnswer>,
    describe: (answer: RuntimeActionAnswer) => string,
  ) {
    setRuntimeActionsBusy((busy) => new Set(busy).add(key));
    try {
      const answer = await call();
      if (mountedRef.current) showSuccess(describe(answer));
    } catch (err) {
      if (mountedRef.current) showError(formatPortalError(err, t));
    } finally {
      if (mountedRef.current) {
        setRuntimeActionsBusy((busy) => {
          const next = new Set(busy);
          next.delete(key);
          return next;
        });
      }
    }
  }

  /**
   * The name a start's or stop's message gives the instance: the mapping's
   * gateway name, with the instance index for a replicable spec, whose
   * instances are told apart by it.
   */
  function instanceTargetName(row: RuntimeStatus, spec: RuntimeSpec, k: number): string {
    const name = statusRowName(row);
    return spec.replicable || k > 0 ? `${name} #${k}` : name;
  }

  function startRuntimeInstance(row: RuntimeStatus, spec: RuntimeSpec) {
    void runRuntimeAction(
      runtimeActionKey('start', row),
      () => (spec.cluster_enabled ? startClusterHead(spec) : api.startRuntime(spec.mapping_id)),
      (answer) => {
        const target = instanceTargetName(row, spec, answer.instance);
        return answer.state === 'starting'
          ? t.runtimeStartedStarting(target)
          : t.runtimeStartedRunning(target);
      },
    );
  }

  // A cluster head's start: its row says the start was asked for at once, and
  // the head is re-read quickly until the start shows in its cluster status.
  async function startClusterHead(spec: RuntimeSpec): Promise<RuntimeActionAnswer> {
    clusterRefresh.requestStart(spec.mapping_id);
    try {
      const answer = await api.startRuntime(spec.mapping_id);
      clusterRefresh.settleStart(spec.mapping_id, true);
      return answer;
    } catch (err) {
      clusterRefresh.settleStart(spec.mapping_id, false);
      throw err;
    }
  }

  // A cluster head's Stop asks first whether its members stop too; any other
  // spec stops at once.
  function requestStop(row: RuntimeStatus, spec: RuntimeSpec) {
    if (spec.cluster_enabled) {
      setStopMembersToo(false);
      setClusterStop({ row, spec });
      return;
    }
    stopRuntimeInstance(row, spec);
  }

  function stopRuntimeInstance(row: RuntimeStatus, spec: RuntimeSpec, stopMembers = false) {
    const k = instanceIndex(row);
    void runRuntimeAction(
      runtimeActionKey('stop', row),
      () =>
        stopMembers
          ? api.stopRuntime(spec.mapping_id, k, true)
          : api.stopRuntime(spec.mapping_id, k),
      (answer) => {
        const target = instanceTargetName(row, spec, answer.instance);
        return answer.state === 'stopped'
          ? t.runtimeStopStopped(target)
          : t.runtimeStopStopping(target);
      },
    );
  }

  async function setOverride(spec: RuntimeSpec, adminState: string) {
    // A timeout/aborted notice tells the operator an override was left in
    // place; acting on any override is exactly the moment it stops being
    // news, so it is cleared here rather than lingering until the next
    // restart or a server switch.
    setRestartNotice(null);
    const run = ++overrideRunRef.current;
    setOverrideBusy(true);
    // Bounds the lock. `overrideBusy` disables EVERY action on EVERY row, and
    // nothing else in the stack ever gives up on the request (no
    // AbortController in api/transport.ts), so without this a single PUT that
    // never settles freezes the whole table until the page is reloaded.
    const watchdog = setTimeout(() => {
      if (overrideRunOver(run)) return;
      overrideRunRef.current += 1; // abandon: a late response must stay silent
      setOverrideBusy(false);
      showError(t.runtimeWriteTimeout);
    }, OVERRIDE_WRITE_TIMEOUT_MS);
    try {
      // The body comes from a fresh read, inside the watchdog's bound, or,
      // when the cache refused that read because a write of this section
      // committed while it was in flight, from the newer document that write
      // left in the cache (`readSpecForWrite`). A read that fails aborts the
      // write through the catch below, and one that lands after the watchdog
      // gave the write up sends nothing: the operator has already been told
      // it was abandoned. The ticket is issued when the PUT is sent, so a
      // write that commits during the read is the older one.
      const fresh = await readSpecForWrite(spec.mapping_id);
      if (overrideRunOver(run)) return;
      // Nothing is written for a spec that is being deleted or is no longer
      // configured: the spec PUT is an upsert, so it would create the spec
      // again. A spec DELETE may still be in flight (one confirmed during the
      // read, say), or may have committed during the read and left the
      // unconfigured document in the cache (`readSpecForWrite`). The action
      // then ends without a toast.
      if (specDeletesInFlight(spec.mapping_id) || !fresh.configured) return;
      const ticket = beginSpecWrite(spec.mapping_id);
      const updated = await api.putRuntimeSpec(
        spec.mapping_id,
        specBodyWithAdminState(fresh, adminState),
      );
      if (!mountedRef.current) return;
      // ABOVE the run-token guard, deliberately. `specsById` is a cache of
      // server truth with no user-visible side effect of its own, and the
      // server has just told us what it now holds. Behind the token, a write
      // abandoned by its watchdog that then resolved left the cache stale
      // forever: the row kept `admin_state: ''`, so it offered Pin and
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
    // behind a timeout notice. Read the LIVE frame, not the captured row: the
    // row the action is offered on (the one that speaks for the spec,
    // primaryStatusRows) and every other instance row of the spec, since the
    // wait needs all of them `stopped` (restartGate).
    if (restartGate(statusRowsRef.current, specId, restartableStates, spec) !== 'open') return;
    const run = ++restartRunRef.current;
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
      // From a fresh read inside the stop deadline, or from the newer cached
      // document when the cache refused it, as in `setOverride`, with the
      // ticket issued when the PUT is sent. A failed read ends the sequence
      // through the catch below; a read that lands after the deadline gave
      // the sequence up writes nothing.
      const fresh = await readSpecForWrite(spec.mapping_id);
      if (restartRunOver(run)) return;
      // As in `setOverride`, nothing is written for a spec that is being
      // deleted or is no longer configured. The sequence ends without a
      // notice: it has forced nothing down.
      if (specDeletesInFlight(spec.mapping_id) || !fresh.configured) {
        setRestart((cur) => (cur?.specId === specId ? null : cur));
        return;
      }
      const ticket = beginSpecWrite(spec.mapping_id);
      const updated = await api.putRuntimeSpec(
        spec.mapping_id,
        specBodyWithAdminState(fresh, 'force_stopped'),
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
      if (restartRunOver(run)) return;
      showError(formatPortalError(err, t));
      setRestart((cur) => (cur?.specId === specId ? null : cur));
    }
  }

  async function finishRestart(flow: RestartFlow) {
    const run = restartRunRef.current;
    // Its own, much shorter deadline: from here the sequence waits on two
    // HTTP round trips, the read and the clear, not on a process lifecycle.
    // Set before the read, so the next frame finds the flow past `waiting`
    // and does not start a second clear.
    setRestart({
      ...flow,
      phase: 'clearing',
      deadline: Date.now() + OVERRIDE_WRITE_TIMEOUT_MS,
    });
    let heldForDelete = false;
    try {
      // Re-read the spec by MAPPING id (captured when the flow started), not
      // by the stream's spec id: the clear PUT must not depend on the spec-id
      // join still resolving. And from the server, not the cache, for the
      // reason given at `readSpecForWrite`, unless the cache refused the read
      // because a write of this section committed while it was in flight: the
      // newer document that write left in the cache is then the one cleared.
      // A failed read ends the sequence through the catch below; one that
      // lands after the deadline gave the sequence up writes nothing.
      const spec = await readSpecForWrite(flow.mappingId);
      if (restartRunOver(run)) return;
      // A spec DELETE confirmed while the read was in flight holds the clear,
      // as the stream effect holds one confirmed before it: the read may
      // still have answered the configured document. The flow goes back to
      // `waiting`, and a frame after the DELETE has settled decides.
      if (specDeletesInFlight(flow.mappingId)) {
        heldForDelete = true;
        setRestart(flow);
        return;
      }
      // Deleted while the restart waited (configured: false, from the read or
      // from the cache entry a delete committed during it), or deleted and
      // created again, which gives the mapping a spec with another id: in
      // each case the spec this restart forced down is gone.
      // PUTting the empty document would only earn a refusal, and clearing the
      // new spec would overwrite an override the operator set on it.
      if (!spec.configured || spec.id !== flow.specId) {
        setRestartNotice('vanished');
        return;
      }
      const ticket = beginSpecWrite(flow.mappingId);
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
      // A clear held for a delete is back in `waiting` and still running.
      if (!heldForDelete && mountedRef.current && restartRunRef.current === run) setRestart(null);
    }
  }

  // ---- create/edit form state -----------------------------------------
  const [gatewayName, setGatewayName] = useState('');
  const [appName, setAppName] = useState('');
  const [enabled, setEnabled] = useState(true);
  const [binary, setBinary] = useState('');
  const serverBinaries = useServerBinaries(api, server.id);
  const [argsText, setArgsText] = useState('');
  const [envText, setEnvText] = useState('');
  const [workDir, setWorkDir] = useState('');
  const [listenPort, setListenPort] = useState(0);
  const [healthPath, setHealthPath] = useState('/health');
  // Whether the OPERATOR typed into the health-path field since the form was
  // opened (create) or hydrated (edit) -- set only by the field's own
  // onChange, never by a type switch moving the field to the new type's
  // default (see the Type select below), so the two cannot be confused with
  // each other. buildSpecBody reads this, not an equality check against the
  // displayed default, to decide whether a first write is "untouched" (see
  // there for why equality was wrong).
  const [healthPathEdited, setHealthPathEdited] = useState(false);
  const [healthTimeoutSeconds, setHealthTimeoutSeconds] = useState(5);
  const [startupTimeoutSeconds, setStartupTimeoutSeconds] = useState(180);
  const [idleTimeoutSeconds, setIdleTimeoutSeconds] = useState(0);
  const [admissionWaitTimeoutSeconds, setAdmissionWaitTimeoutSeconds] = useState(0);
  const [pinned, setPinned] = useState(false);
  // The instance settings. Bound to state and emitted by buildSpecBody on
  // every save, for the same full-document reason as ramEstimateMb below: an
  // absent field is applied as its zero value. Unticked, the save sends the
  // backend's neutral 0/0/0 instead of the hidden fields (instanceSettingsBody).
  // The two counts are held as the field's TEXT, because an empty field is a
  // value of its own (0: none / unlimited); the parallelism too, so a field
  // cleared while typing is not forced back to a number.
  const [replicable, setReplicable] = useState(false);
  const [minInstancesText, setMinInstancesText] = useState('');
  const [maxInstancesText, setMaxInstancesText] = useState('');
  const [instanceParallelismText, setInstanceParallelismText] = useState('1');
  const [adminState, setAdminState] = useState('');
  // The spec's operator RAM estimate in MB: ONE spec-level scalar (RAM is
  // host-scoped, so it is not a GpuRow field). It must be bound to state and
  // emitted by buildSpecBody even where the form shows no field for it: the
  // PUT is a full-document upsert, and an absent estimate is applied as 0,
  // clearing one set through the API.
  const [ramEstimateMb, setRamEstimateMb] = useState(0);
  // Last measured host RAM (agent-reported, persisted on the spec), shown
  // read-only under the estimate field the way each GPU row shows its own
  // measured VRAM — so the operator sees what the override is being set against.
  const [ramMeasuredMb, setRamMeasuredMb] = useState(0);
  const [setVisibleDevices, setSetVisibleDevices] = useState(false);
  const [visibleDevicesMode, setVisibleDevicesMode] = useState<'env' | 'args'>('env');
  const [executionMode, setExecutionMode] = useState<'native' | 'wsl'>('native');
  const [wslDistro, setWslDistro] = useState('');
  // Runtime-Spec API Token: mode + its per-mode write-only state.
  // `apiTokenHeaderSource`/`apiTokenHeader` are declared here alongside mode,
  // even though their own select + inherited-header display render later in
  // the form (see the API-token header block below) -- all three must still
  // round-trip regardless: this form's PUT is a full-document upsert
  // (buildSpecBody below), so leaving them constant would silently reset
  // whatever header choice the operator made. `apiTokenInput`/`apiTokenCleared`
  // mirror ApplicationSection's own app-token sentinel (tokenInput/tokenCleared)
  // for THIS spec's token; `apiTokenRotate` is 'random' mode's one-shot
  // rotate-on-next-save flag.
  const [apiTokenMode, setApiTokenMode] = useState<RuntimeSpec['api_token_mode']>('app');
  const [apiTokenHeaderSource, setApiTokenHeaderSource] =
    useState<RuntimeSpec['api_token_header_source']>('app');
  const [apiTokenHeader, setApiTokenHeader] = useState('');
  const [apiTokenInput, setApiTokenInput] = useState('');
  const [apiTokenCleared, setApiTokenCleared] = useState(false);
  const [apiTokenRotate, setApiTokenRotate] = useState(false);
  const [gpuRows, setGpuRows] = useState<GpuRow[]>([]);
  const [specApiFlavors, setSpecApiFlavors] = useState<string[]>([]);
  // Whether the flavor group has been touched in this form session -- ticked
  // or unticked, or a save refused over it. An empty selection is shown as an
  // error only then, so a spec stored with [] opens without one; the refusal
  // itself (specFlavorsMissing, at submit) does not depend on it.
  const [specFlavorsTouched, setSpecFlavorsTouched] = useState(false);
  // A refused save must be perceivable, not just a button that does nothing:
  // each refusal moves focus to the flavor group, whose description is the
  // reason. Counted rather than flagged so a second refusal refocuses, and
  // applied in an effect so focus lands after the message has rendered and is
  // wired to the group. Same mechanism as the application form's.
  const specFlavorsGroupRef = useRef<HTMLFieldSetElement>(null);
  const [specFlavorRefusals, setSpecFlavorRefusals] = useState(0);
  useEffect(() => {
    if (specFlavorRefusals > 0) specFlavorsGroupRef.current?.focus();
  }, [specFlavorRefusals]);
  const [specResponsesMode, setSpecResponsesMode] = useState<EndpointMode>('passthrough');
  const [specMessagesMode, setSpecMessagesMode] = useState<EndpointMode>('passthrough');
  // The decisions modes are NOT part of the parent template (runtimeSpecTemplate):
  // a spec starts at the defaults, and a PUT that omits them resets them, so the
  // body always carries both.
  const [specDecisionsMode, setSpecDecisionsMode] = useState<DecisionsMode>(defaultDecisionsMode);
  const [specSystemOneMode, setSpecSystemOneMode] = useState<SystemOneMode>(defaultSystemOneMode);
  // The API-variant values a switch of Type to stable_diffusion_cpp or
  // audio_cpp replaced, so a switch back can restore them
  // (specFlavorsAfterTypeSwitch). null when no such switch happened since the
  // form opened, or after a switch back restored them.
  const [specFlavorsBeforeKindDefault, setSpecFlavorsBeforeKindDefault] =
    useState<RuntimeSpecFlavorValues | null>(null);
  // The Type, binary and resolved type of the spec the form was hydrated from,
  // for the sd flavor hint under Auto (formSpecIsStableDiffusion). null on
  // Create.
  const [specLoadedType, setSpecLoadedType] = useState<LoadedSpecType | null>(null);
  // RuntimeSpec Type: the explicit runtime-server kind ('' = auto-detect from
  // `binary`) plus the two per-type probe-path overrides. Mirrors the
  // api_token_mode trio above -- writable state here, the resolved values
  // (effective_type/resolved_metrics_path/resolved_context_probe_path) are
  // read straight from the loaded spec at render time (see `currentSpec`
  // below), never copied into state, since nothing here ever writes them.
  const [specType, setSpecType] = useState<RuntimeSpec['type']>('');
  const [metricsPath, setMetricsPath] = useState('');
  const [contextProbePath, setContextProbePath] = useState('');
  // The spec's config files (#257), bound from the LOADED spec on Edit and sent
  // back by buildSpecBody. The PUT is a full document and an absent
  // `config_files` clears the stored set, so this state is what keeps a save of
  // an unrelated field from deleting files that were set through the API.
  const [configFiles, setConfigFiles] = useState<ConfigFileRow[]>([]);
  // The cluster head configuration and the spec's device backend, bound from
  // the loaded spec and sent back by buildSpecBody for the same full-document
  // reason: a save without them turns a cluster head into a plain spec.
  const [cluster, setCluster] = useState(emptyClusterState);
  // undefined = "no opinion", and the request field is optional so this form
  // can say it. It genuinely has to: with Type on "Auto" the effective kind
  // is detected from the binary's basename, and that detection is Go-only --
  // so a `true` here might be refused and a `false` would silently disagree
  // with the llama.cpp create default. The only correct thing to send is
  // nothing, and the backend then applies the kind's own default on a first
  // write or keeps the stored value on a later one.
  const [specLiveTimings, setSpecLiveTimings] = useState<boolean | undefined>(undefined);
  const specLiveTimingsKind = runtimeSpecLiveTimingsKind(specType);
  // Whether the save this form leads to is the spec's FIRST write: always on
  // create, and on an edit of a mapping that has no spec row yet
  // (`configured: false`). Edit is offered on every row, "Delete spec" keeps
  // the mapping, and a failed create leaves a mapping without a spec to
  // retry through Edit -- so `specMode` alone cannot tell. Captured when the
  // form opens, from the same GET payload the fields are hydrated from.
  const [specFirstWrite, setSpecFirstWrite] = useState(true);

  /**
   * The newest VRAM measurement this mapping has, for the per-GPU APPLY
   * affordance below -- and it is an offer, never a writer.
   *
   * The VRAM benchmark reports a number and writes NEITHER VRAM field: not
   * `vram_measured_mb`, which is agent-owned and feeds admission arithmetic as
   * the spec's own declared demand (a breach by a measured value is terminal,
   * so a gateway-computed delta that overshot would refuse every future start
   * of a model that had been working); and not `vram_estimate_mb`, which is
   * operator-owned and whose whole meaning is "what the operator declares".
   * So the run leaves its number in a `kind = "vram"` history row as EVIDENCE,
   * and this is where an operator picks it up: Apply fills the estimate field
   * beside it and they save.
   *
   * Read from the mapping's own benchmark history rather than passed in,
   * because the run happens on another screen (the benchmark area) and minutes
   * or days earlier. `latestApplicableVramRun` is the gate: right kind,
   * definitive result, proven isolation, at least one positive number.
   */
  const [vramEvidence, setVramEvidence] = useState<{
    run: BenchmarkRunDTO;
    report: VRAMReportDTO;
  } | null>(null);
  const editingSpecMappingId = typeof specMode === 'object' ? specMode.mapping.id : '';
  useEffect(() => {
    // No mapping, no history: the CREATE form has nothing to read a
    // measurement for, and must not fetch one.
    if (!editingSpecMappingId) {
      setVramEvidence(null);
      return;
    }
    let cancelled = false;
    api
      .mappingBenchmarks(editingSpecMappingId)
      .then((runs) => {
        if (!cancelled) setVramEvidence(latestApplicableVramRun(runs));
      })
      .catch(() => {
        // Non-blocking: the affordance is a bonus on top of the form, so a
        // failed history GET must not break the launch config the operator
        // actually came here to edit.
        if (!cancelled) setVramEvidence(null);
      });
    return () => {
      cancelled = true;
    };
  }, [api, editingSpecMappingId]);

  // index -> that card's measured item, so a row finds its own number and
  // never a sibling's. The run reports the indexes IT watched, which need not
  // be the rows the spec declares today.
  const vramMeasuredByIndex = useMemo(() => {
    const byIndex = new Map<number, VRAMGPUItemDTO>();
    for (const gpu of vramEvidence?.report.gpus ?? []) byIndex.set(gpu.index, gpu);
    return byIndex;
  }, [vramEvidence]);

  function resetSpecFields() {
    setEnabled(true);
    setBinary('');
    setArgsText('');
    setEnvText('');
    setWorkDir('');
    setListenPort(0);
    setHealthPath('/health');
    setHealthPathEdited(false);
    setHealthTimeoutSeconds(5);
    setStartupTimeoutSeconds(180);
    setIdleTimeoutSeconds(0);
    setAdmissionWaitTimeoutSeconds(0);
    setPinned(false);
    setReplicable(false);
    setMinInstancesText('');
    setMaxInstancesText('');
    setInstanceParallelismText('1');
    setAdminState('');
    setRamEstimateMb(0);
    setRamMeasuredMb(0);
    setSetVisibleDevices(false);
    setVisibleDevicesMode('env');
    setExecutionMode('native');
    setWslDistro('');
    setApiTokenMode('app');
    setApiTokenHeaderSource('app');
    setApiTokenHeader('');
    setApiTokenInput('');
    setApiTokenCleared(false);
    setApiTokenRotate(false);
    setGpuRows([]);
    setSpecDecisionsMode(defaultDecisionsMode);
    setSpecSystemOneMode(defaultSystemOneMode);
    setSpecType('');
    setSpecLiveTimings(undefined);
    setMetricsPath('');
    setContextProbePath('');
    setConfigFiles([]);
    setCluster(emptyClusterState());
    setSpecFlavorsBeforeKindDefault(null);
    setSpecLoadedType(null);
  }

  function hydrateSpecFields(spec: RuntimeSpec) {
    setEnabled(spec.enabled);
    setBinary(spec.binary);
    setArgsText(formatArgsText(spec.args));
    setEnvText(formatEnvText(spec.env));
    setWorkDir(spec.work_dir);
    setListenPort(spec.listen_port);
    setHealthPath(spec.health_path || '/health');
    setHealthPathEdited(false);
    setHealthTimeoutSeconds(spec.health_timeout_seconds || 5);
    setStartupTimeoutSeconds(spec.startup_timeout_seconds || 180);
    setIdleTimeoutSeconds(spec.idle_timeout_seconds);
    setAdmissionWaitTimeoutSeconds(spec.admission_wait_timeout_seconds);
    setPinned(spec.pinned);
    setReplicable(spec.replicable);
    setMinInstancesText(instanceCountText(spec.min_instances));
    setMaxInstancesText(instanceCountText(spec.max_instances));
    // 0 is the backend's "default", which is 1: shown as what it means.
    setInstanceParallelismText(String(spec.instance_parallelism || 1));
    setAdminState(spec.admin_state);
    setRamEstimateMb(spec.ram_estimate_mb);
    setRamMeasuredMb(spec.ram_measured_mb);
    setSetVisibleDevices(spec.set_visible_devices);
    setVisibleDevicesMode(spec.visible_devices_mode);
    setExecutionMode(spec.execution_mode === 'wsl' ? 'wsl' : 'native');
    setWslDistro(spec.wsl_distro ?? '');
    setApiTokenMode(spec.api_token_mode);
    setApiTokenHeaderSource(spec.api_token_header_source);
    setApiTokenHeader(spec.api_token_header);
    // Write-only: a load never shows what is stored, only whether it is
    // (spec.api_token_set, read directly from specsById at render time).
    setApiTokenInput('');
    setApiTokenCleared(false);
    setApiTokenRotate(false);
    setGpuRows(
      spec.gpus.map((g) => ({
        rowKey: makeRowKey(),
        index: g.index,
        vramEstimateMb: g.vram_estimate_mb,
        vramMeasuredMb: g.vram_measured_mb,
      })),
    );
    setSpecApiFlavors([...spec.api_flavors]);
    setSpecResponsesMode(spec.responses_mode);
    setSpecMessagesMode(spec.messages_mode);
    setSpecDecisionsMode(decisionsModeOrDefault(spec.decisions_mode));
    setSpecSystemOneMode(systemOneModeOrDefault(spec.systemone_mode));
    setSpecType(spec.type);
    // `configured: false` means the mapping has no spec row, so what the
    // document carries are zero values, defaults and app-derived echoes
    // rather than stored choices -- that `false` is not an operator decision
    // and must not become one (the flavors, the modes and Enabled are seeded
    // in `openEdit` for the same reason). Edit is ungated and reaches exactly
    // that document, and the write it leads to is a FIRST write.
    setSpecLiveTimings(spec.configured ? spec.responses_live_timings_enabled : undefined);
    // The same zero-value argument for the health path: with no spec row the
    // field shows this form's own fallback, not a stored value, so the save
    // is judged as a first write (buildSpecBody).
    setSpecFirstWrite(!spec.configured);
    setMetricsPath(spec.metrics_path);
    setContextProbePath(spec.context_probe_path);
    // Optional on the type, always present on the wire; a document from before
    // the field existed is "no files".
    setConfigFiles(
      (spec.config_files ?? []).map((f) => ({
        rowKey: makeRowKey(),
        name: f.name,
        content: f.content,
      })),
    );
    setCluster(clusterStateFromSpec(spec));
    setSpecFlavorsBeforeKindDefault(null);
    setSpecLoadedType({
      type: spec.type,
      binary: spec.binary,
      effective_type: spec.effective_type,
    });
  }

  function openCreate() {
    // Supersedes an Edit whose GET is still in flight (see openEdit). That GET
    // no longer clears its row's loading lock, so this does.
    editOpenSeqRef.current += 1;
    setLoadingEditFor('');
    setGatewayName('');
    setAppName('');
    resetSpecFields();
    setSpecFlavorsTouched(false);
    setSpecFirstWrite(true);
    // The parent application's template (runtimeSpecTemplate, which carries
    // the openai_images rule and its reasoning); Edit of a spec-less mapping
    // starts from the same one.
    const template = runtimeSpecTemplate(application);
    setSpecApiFlavors(template.apiFlavors);
    setSpecResponsesMode(template.responsesMode);
    setSpecMessagesMode(template.messagesMode);
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
  //
  // Only the LATEST form open may fill the form. The list stays on screen
  // while this GET is in flight, and `rowActions` locks only the loading row,
  // so the operator can click Edit on another mapping, or Create, before it
  // lands. Without the check, a GET for an earlier click that lands last would
  // switch the form to its own spec while the names stay the later mapping's,
  // and the save would write the later mapping's upstream model name onto the
  // earlier mapping. The cache update before that check stays unconditional:
  // the payload is still a read of server truth.
  async function openEdit(mapping: PortalModelMapping) {
    const seq = ++editOpenSeqRef.current;
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
      if (seq !== editOpenSeqRef.current) return;
      setGatewayName(mapping.gateway_model_name);
      setAppName(mapping.app_model_name);
      setSpecFlavorsTouched(false);
      hydrateSpecFields(spec);
      // No spec row yet: the document's [], "" and `enabled: false` are zero
      // values, not a stored choice, and until this first write the mapping
      // routes on its application's flavors and modes. Start from those, and
      // with Enabled ticked, exactly as Create does -- this is also where the
      // operator retries a Create whose spec write failed.
      if (!spec.configured) {
        const template = runtimeSpecTemplate(application);
        setSpecApiFlavors(template.apiFlavors);
        setSpecResponsesMode(template.responsesMode);
        setSpecMessagesMode(template.messagesMode);
        setEnabled(true);
      }
      setSpecMode({ kind: 'edit', mapping });
    } catch (err) {
      if (seq === editOpenSeqRef.current) showError(formatPortalError(err, t));
    } finally {
      if (seq === editOpenSeqRef.current) setLoadingEditFor('');
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

  function addConfigFile() {
    setConfigFiles((rows) =>
      rows.length >= CONFIG_FILES_MAX
        ? rows
        : [...rows, { rowKey: makeRowKey(), name: '', content: '' }],
    );
  }
  function removeConfigFile(idx: number) {
    setConfigFiles((rows) => rows.filter((_, i) => i !== idx));
  }
  function updateConfigFile(idx: number, patch: Partial<Pick<ConfigFileRow, 'name' | 'content'>>) {
    setConfigFiles((rows) => rows.map((r, i) => (i === idx ? { ...r, ...patch } : r)));
  }
  // The early refusal at submit (create AND edit): the gateway rejects the same
  // lists with a 400, and on a CREATE the mapping is written before its spec,
  // so a bad name caught only there would leave a spec-less mapping behind.
  // Names the offending file, never its content -- that can hold the secret
  // that tripped the check.
  function configFilesError(): string | undefined {
    const issues = configFileIssues(configFiles);
    const idx = issues.findIndex((issue) => issue !== null);
    if (idx < 0) return undefined;
    const issue = issues[idx]!;
    const label = configFiles[idx].name.trim() === '' ? `#${idx + 1}` : configFiles[idx].name;
    return `${t.runtimeSpecConfigFiles} (${label}): ${t[configFileIssueLabel[issue]]}`;
  }

  // Reorder the GPU rows by rowKey (moveColumn works on a string[] of ids;
  // rebuild the GpuRow[] in the new key order). Array order is the wire
  // contract (buildSpecBody sends gpuRows verbatim), so this is the whole of
  // Part A on the client.
  function reorderGpuRows(sourceKey: string, targetKey: string, place: 'before' | 'after') {
    setGpuRows((rows) => {
      const order = moveColumn(
        rows.map((r) => r.rowKey),
        sourceKey,
        targetKey,
        place,
      );
      return order.map((k) => rows.find((r) => r.rowKey === k)!);
    });
  }
  function swapGpuRow(idx: number, delta: number) {
    setGpuRows((rows) => {
      const target = idx + delta;
      if (target < 0 || target >= rows.length) return rows;
      const next = [...rows];
      [next[idx], next[target]] = [next[target], next[idx]];
      return next;
    });
  }
  const gpuDrag = useColumnDrag(reorderGpuRows, 'vertical');

  /**
   * The APPLY affordance for one GPU row: the benchmark's number for that card,
   * read-only, plus a button that FILLS the operator's estimate field beside
   * it. Nothing is written -- `updateGpuRow` touches form state only, and the
   * operator saves.
   *
   * Returns null when this card has no positive number in the newest
   * applicable run, which covers the create form, an inconclusive run, a run
   * whose isolation was never proven, and a declared card the run watched but
   * measured nothing on. Offering a 0 there would put "this model is free"
   * into the admission arithmetic through the operator's own field.
   *
   * Matched BY GPU INDEX, never by row position: the run reports the indexes it
   * watched, and the rows may have been reordered, added to or removed since.
   *
   * And an index is not an identity, which is why the recorded card
   * fingerprint is COMPARED here rather than merely displayed. A driver reset
   * or a hardware swap renumbers the cards; without the comparison this
   * affordance would go on offering a run's 21500 MB for whatever card now
   * sits at index 1, days later, while the history table still named the UUID
   * it had captured. On drift the number is stated and withheld; when nothing
   * could be compared the offer stands but says so -- "cannot verify" must
   * never read as "verified".
   */
  function renderVramApply(row: GpuRow, rowIdx: number) {
    const item = vramMeasuredByIndex.get(row.index);
    const applicable = vramApplyNumber(item);
    if (!applicable) return null;
    const check = vramCardCheck(
      item,
      telemetryGpus.find((gpu) => gpu.index === row.index),
    );
    if (check.state === 'drifted') {
      // No number field and no Apply: the measurement is real but belongs to
      // other hardware, and applying it would put a foreign card's cost into
      // the admission arithmetic through the operator's own field. Both
      // fingerprints are named, because "which card was it then, and which is
      // it now" is the operator's next question.
      return (
        <Alert severity="warning" sx={{ width: '100%' }}>
          <Box sx={{ display: 'grid', gap: 0.25 }}>
            <span>{t.runtimeSpecVramCardDrift}</span>
            <Typography variant="caption">
              {t.runtimeGpuDriftExpected}: {check.recorded}
            </Typography>
            <Typography variant="caption">
              {t.runtimeGpuDriftCurrent}: {check.live}
            </Typography>
          </Box>
        </Alert>
      );
    }
    // Which of the two reported quantities this is. They are never averaged
    // and never silently interchanged, so the source is on screen next to the
    // number the operator is about to adopt.
    const source =
      applicable.source === 'delta'
        ? t.runtimeSpecVramBenchmarkSourceDelta
        : t.runtimeSpecVramBenchmarkSourceMeasured;
    return (
      <>
        <Field
          id={`runtime-spec-gpu-vram-benchmark-${rowIdx}`}
          label={t.runtimeSpecVramBenchmark}
          value={String(applicable.mb)}
          // The no-op is load-bearing, not decoration: jsdom's fireEvent.change
          // fires on a readOnly input, so a live handler would still drive state
          // and give a test a false green on a write no real user can perform.
          onChange={() => {}}
          // `readOnly`, not `disabled` and not `inputProps={{readOnly:true}}`:
          // this number's whole job is to be READ, and the prop is what also
          // strips the editable outline chrome.
          readOnly
          helperText={item?.unified_memory ? `${source} — ${t.benchmarkVramUnifiedMemory}` : source}
          sx={{ maxWidth: 240 }}
        />
        <Button
          type="button"
          variant="outlined"
          size="small"
          onClick={() => updateGpuRow(rowIdx, { vramEstimateMb: applicable.mb })}
        >
          {t.runtimeSpecVramApply}
        </Button>
      </>
    );
  }

  /**
   * What the fingerprint comparison established for one row, on a line of its
   * own beneath the offer.
   *
   * Rendered in every state the offer survives -- not only the interesting one
   * -- because the absence of a caveat is itself a claim: silence next to a
   * number would let an UNVERIFIABLE card read as a checked one, which is the
   * drift bug with better manners. The drift case is not here: it is inside
   * the affordance, because there the sentence replaces the number rather than
   * qualifying it.
   *
   * Separate from renderVramApply purely for layout: the row is a wrapping
   * flex line, so a full-width element in the middle of it would push the
   * row's own Remove button onto the line below and orphan it from its row.
   */
  function renderVramCardCheck(row: GpuRow) {
    const item = vramMeasuredByIndex.get(row.index);
    if (!vramApplyNumber(item)) return null;
    const check = vramCardCheck(
      item,
      telemetryGpus.find((gpu) => gpu.index === row.index),
    );
    if (check.state === 'drifted') return null;
    return (
      <Typography variant="caption" color="text.secondary" sx={{ width: '100%' }}>
        {t[vramCardCheckLabelKey(check)]}
      </Typography>
    );
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

  // The instance settings as typed, for instanceSettingsBody and
  // validateInstances.
  function instanceForm(): InstanceForm {
    return {
      replicable,
      minText: minInstancesText,
      maxText: maxInstancesText,
      parallelismText: instanceParallelismText,
    };
  }

  function buildSpecBody(args: string[], env: Record<string, string>): PutRuntimeSpecRequest {
    // Write-only token sentinel, mirroring ApplicationSection's own
    // apiToken/buildBody: cleared -> '' (clear the stored token); a typed
    // value -> replace-and-seal; otherwise omit so an unrelated save never
    // disturbs a token already stored under 'set' mode. Meaningless outside
    // 'set' mode, so it is never even computed there.
    let apiToken: string | null | undefined;
    if (apiTokenMode === 'set') {
      if (apiTokenCleared) apiToken = '';
      else if (apiTokenInput !== '') apiToken = apiTokenInput;
    }
    const healthPathTrimmed = healthPath.trim();
    // FIRST write only (a create, or an edit of a mapping with no spec yet --
    // specFirstWrite): while the field is UNTOUCHED -- the operator never
    // typed into it (healthPathEdited) -- send '' instead of its displayed
    // text, so the backend's OWN default (keyed on the spec's EFFECTIVE type,
    // resolved server-side -- auto-detected from `binary` under Auto, which
    // this form cannot do) decides. "Untouched" is judged by that flag, not
    // by comparing the field to runtimeSpecHealthPathDefault(specType): a
    // Type switch moves an untouched field to the NEW type's default too
    // (see the Type select below), so after picking an explicit type and
    // switching back to Auto the field can display that explicit type's
    // default while Auto's own default differs -- an equality check would
    // then read it as "edited" and send it verbatim, pinning a path this
    // form only guessed for whatever Auto detects `binary` to. An edit of a
    // stored spec always sends the field as-is -- a stored explicit value
    // must never be silently re-derived away from what it displays.
    const healthPathForRequest = specFirstWrite && !healthPathEdited ? '' : healthPathTrimmed;
    return {
      enabled,
      binary: binary.trim(),
      args,
      env,
      work_dir: workDir.trim(),
      listen_port: listenPort,
      health_path: healthPathForRequest,
      health_timeout_seconds: healthTimeoutSeconds,
      startup_timeout_seconds: startupTimeoutSeconds,
      idle_timeout_seconds: idleTimeoutSeconds,
      admission_wait_timeout_seconds: admissionWaitTimeoutSeconds,
      pinned,
      ...instanceSettingsBody(instanceForm()),
      admin_state: adminState,
      ram_estimate_mb: ramEstimateMb,
      set_visible_devices: setVisibleDevices,
      visible_devices_mode: visibleDevicesMode,
      execution_mode: executionMode,
      wsl_distro: executionMode === 'wsl' ? wslDistro.trim() : '',
      api_token_mode: apiTokenMode,
      api_token_header_source: apiTokenHeaderSource,
      api_token_header: apiTokenHeader,
      ...(apiToken !== undefined ? { api_token: apiToken } : {}),
      // Only meaningful under 'random' mode; a stray true from a since-changed
      // mode would ask the backend to rotate a token that no longer applies.
      ...(apiTokenMode === 'random' && apiTokenRotate ? { api_token_rotate: true } : {}),
      gpus: gpuRows.map((r) => ({
        index: r.index,
        vram_estimate_mb: r.vramEstimateMb,
        vram_measured_mb: r.vramMeasuredMb,
      })),
      api_flavors: specApiFlavors,
      responses_mode: specResponsesMode,
      messages_mode: specMessagesMode,
      // Always sent: omitting them on this full-document PUT would reset them to
      // the defaults. passthrough is refused for llama_cpp, so it is coerced.
      decisions_mode: decisionsModeForKind(specDecisionsMode, specType),
      systemone_mode: systemOneModeForKind(specSystemOneMode, specType),
      // Two independent reasons to omit, and both must hold before the key is
      // sent.
      //
      // Kind: this is a FULL-DOCUMENT upsert that restates `type` on every
      // save, and putRuntimeSpec refuses an explicit true against an
      // incapable effective kind with 400 BEFORE any store write -- so the
      // whole save would fail, on a retype the operator made for another
      // reason entirely. Omitting is what the backend normalises: it clears a
      // stored true whose document can no longer honour it, deliberately and
      // silently, because the caller said nothing about the flag.
      //
      // Value: undefined is "no opinion", which IS the absent key. Under Type
      // "Auto" that is the only honest thing to send, since the kind is
      // detected from the binary's basename by Go and not by this form.
      //
      // But under "Auto" the value is NOT reliably undefined, and that is the
      // case to be exact about: openEdit seeds this state from the STORED row,
      // and putRuntimeSpec's own first-write default stored a true for every
      // binary its detector read as llama.cpp. So the box can arrive ticked
      // without the operator ever touching it, and an edit to `binary` alone --
      // to a wrapper the detector does not recognise, run-llama.sh or a
      // versioned llama-srv-b10448 -- then sends that true beside a document
      // that now resolves to `custom`, and the 400 takes the WHOLE save, on a
      // save that was about the binary path. Accepted and recoverable (untick,
      // save again); what it is not is a consequence of TICKING the box, so do
      // not describe it as one.
      ...(runtimeSpecSendsLiveTimings(specLiveTimingsKind) && specLiveTimings !== undefined
        ? { responses_live_timings_enabled: specLiveTimings }
        : {}),
      type: specType,
      metrics_path: metricsPath.trim(),
      context_probe_path: contextProbePath.trim(),
      // Always sent, [] included: absent clears the stored set on the backend,
      // and this is the one place that decides what the full document says.
      // Plain {name, content} -- the row key is a render identity only.
      config_files: configFiles.map(({ name, content }) => ({ name, content })),
      ...clusterBodyFields(cluster),
    };
  }

  // The cluster save checks the form can make itself (create and edit).
  function clusterError(args: string[], env: Record<string, string>): string | undefined {
    const key = clusterSaveError(cluster, {
      args,
      env,
      type: specType,
      replicable,
      startupTimeoutSeconds,
      binary,
      stored: editingSpecMappingId ? specsById[editingSpecMappingId] : undefined,
    });
    return key ? t[key] : undefined;
  }

  // A member usage reset is a write to the head's spec: the answer is the
  // head as it reads afterwards, and it takes a ticket like every other write
  // so a slower, earlier write cannot land over it (see beginSpecWrite).
  async function resetClusterMemberUsage(mappingId: string, serverId: string) {
    const ticket = beginSpecWrite(mappingId);
    const spec = await api.resetClusterMemberUsage(mappingId, serverId);
    commitSpecCache(mappingId, ticket, spec);
    return spec;
  }

  // Refreshes what a committed spec write (create, edit, spec delete) can
  // change: the warnings, and each mapping row's `images_only` marker, which
  // the backend computes from the spec's API flavors when the mapping has a
  // spec and from the application's otherwise. The rows in hand were read
  // before the write.
  function reloadAfterSpecWrite() {
    void reloadWarnings();
    void reloadMappings();
  }

  // An empty flavor list cannot be saved honestly: the backend stores [] as
  // [openai, anthropic] (normalizeFlavors, on every spec write), so a form
  // showing nothing ticked would save a text candidate -- on a
  // stable_diffusion_cpp spec, the one its operator unticked the text flavors
  // to avoid. The save is refused and the reason shown, before the mapping is
  // written, so a refusal never leaves a half-created mapping behind.
  function specFlavorsMissing(): boolean {
    if (specApiFlavors.length > 0) return false;
    setSpecFlavorsTouched(true);
    setSpecFlavorRefusals((count) => count + 1);
    return true;
  }

  // The Type select's API-variant defaults (specFlavorsAfterTypeSwitch, which
  // holds the rule): the sd default on a switch to stable_diffusion_cpp, the
  // audio default on a switch to audio_cpp, the replaced values on a switch
  // back, and nothing for values the operator changed.
  function applySpecTypeFlavorDefaults(next: RuntimeSpec['type']) {
    const change = specFlavorsAfterTypeSwitch({
      from: specType,
      to: next,
      current: {
        apiFlavors: specApiFlavors,
        responsesMode: specResponsesMode,
        messagesMode: specMessagesMode,
      },
      template: runtimeSpecTemplate(application),
      remembered: specFlavorsBeforeKindDefault,
    });
    if (change === null) return;
    setSpecApiFlavors(change.values.apiFlavors);
    setSpecResponsesMode(change.values.responsesMode);
    setSpecMessagesMode(change.values.messagesMode);
    setSpecFlavorsBeforeKindDefault(change.remembered);
  }

  async function submitCreate(event: SubmitEvent<HTMLFormElement>) {
    event.preventDefault();
    if (specFlavorsMissing()) return;
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
    const configFilesProblem = configFilesError();
    if (configFilesProblem) {
      showError(configFilesProblem);
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
    const instancesError = validateInstances(instanceForm(), listenPort, t);
    if (instancesError) {
      showError(instancesError);
      return;
    }
    const clusterProblem = clusterError(args, parsedEnv.env);
    if (clusterProblem) {
      showError(clusterProblem);
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
      // Gateway name == app model name on CREATE. The operator enters ONLY the
      // application model name (there is no gateway field on create -- see the
      // form below); the gateway name is DERIVED from it, exactly as model
      // discovery seeds a new mapping for an ordinary application
      // (GatewayModelName == AppModelName == the discovered name,
      // service_applications.go). The backend requires a non-empty gateway name
      // (ErrMappingGatewayNameRequired) and sending the app name satisfies it.
      //
      // Ownership rule: a DISTINCT gateway alias is set LATER on the
      // Modell-Zuordnung tab's edit, where the gateway name is editable -- the
      // same discovery-then-rename flow non-agent applications use. Do NOT
      // re-add a gateway field to this create form: that would make the operator
      // enter one name twice and re-create the two-writers hazard the ownership
      // split removes.
      mapping = await api.createMapping(application.id, {
        gateway_model_name: appName,
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
      reloadAfterSpecWrite();
      setSpecMode('list');
    } catch (err) {
      showError(`${t.runtimeSpecPartialFailure}: ${formatPortalError(err, t)}`);
      // The mapping exists now, so the form stays open as its Edit with every
      // entry kept -- a cluster head's members and their GPUs included -- and
      // the operator fixes what was refused and saves again. Leaving for the
      // list would throw the entries away.
      setGatewayName(mapping.gateway_model_name);
      setSpecMode({ kind: 'edit', mapping });
    } finally {
      setBusy(false);
    }
  }

  async function submitEdit(event: SubmitEvent<HTMLFormElement>) {
    event.preventDefault();
    if (typeof specMode === 'string' || specMode.kind !== 'edit') return;
    if (specFlavorsMissing()) return;
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
    const configFilesProblem = configFilesError();
    if (configFilesProblem) {
      showError(configFilesProblem);
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
    const instancesError = validateInstances(instanceForm(), listenPort, t);
    if (instancesError) {
      showError(instancesError);
      return;
    }
    const clusterProblem = clusterError(args, parsedEnv.env);
    if (clusterProblem) {
      showError(clusterProblem);
      return;
    }
    setBusy(true);
    let mapping: PortalModelMapping;
    try {
      // A form does not send a field it does not let you edit. Read-only here
      // => not sent from here: the gateway name and the status belong to the
      // mapping and are edited on the Modell-Zuordnung tab, and this form owns
      // only `app_model_name` (the spec's upstream_model).
      //
      // OMITTED, not echoed. The PATCH is pointer-gated per field, so an absent
      // key carries no value for that field -- which is what makes the two
      // screens non-overlapping WRITERS. It does not make the write atomic:
      // `Service.UpdateMapping` re-writes the whole row it loaded, with no
      // compare-and-set, so a mapping-tab PATCH landing between this one's read
      // and its write is still lost. Omission removes the routine clobber, not
      // the race (11-risks-and-technical-debt.md §11.1). Echoing
      // `status` here would be worse than redundant: it is a snapshot taken when
      // this form opened, so a spec save would silently PATCH a deliberately
      // DISABLED model back into service, with no error and no column on the
      // specs tab that contradicts it. (The gateway name is the milder case --
      // no 409 hazard, the conflict check self-excludes -- but the same
      // lost-update argument applies, so it goes for the same reason.)
      const updated = await api.updateMapping(id, { app_model_name: appName });
      setMappings((current) => (current ?? []).map((m) => (m.id === id ? updated : m)));
      mapping = updated;
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
      reloadAfterSpecWrite();
      setSpecMode('list');
    } catch (err) {
      showError(`${t.runtimeSpecPartialFailure}: ${formatPortalError(err, t)}`);
      // As after a refused create: the form stays open with every entry kept,
      // a cluster head's members and their GPUs included. The mapping PATCH
      // above did land, so the Edit now carries the mapping as it answered;
      // the form's app model name already is that value, and the next save
      // PATCHes the same name again (a no-op) before it retries the spec.
      setSpecMode({ kind: 'edit', mapping });
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
        // Counted while in flight, so that no override write that has not sent
        // its PUT yet sends one before the DELETE has settled: a waiting
        // restart holds its clear (the stream effect above), and each override
        // write checks after its read: `setOverride` and `startRestart`, for
        // which that is the only check, then write nothing, and `finishRestart`,
        // which the stream effect checked before its read, holds its clear.
        specDeletesInFlightRef.current.set(id, (specDeletesInFlightRef.current.get(id) ?? 0) + 1);
        try {
          await api.deleteRuntimeSpec(id);
        } finally {
          const left = (specDeletesInFlightRef.current.get(id) ?? 1) - 1;
          if (left > 0) specDeletesInFlightRef.current.set(id, left);
          else specDeletesInFlightRef.current.delete(id);
        }
        const deleted = emptySpec(id);
        commitSpecCache(id, ticket, deleted);
        clearRestartNoticeAfter(deleted);
        reloadAfterSpecWrite();
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
    // "Benchmark bleibt": measures a model, never creates or destroys a row, so
    // the argument that dropped Delete/create/sync from this tab does not reach
    // it. Opens the consolidated benchmark sub-view scoped to this mapping,
    // exactly as `MappingSection`'s own row action does.
    {
      key: 'benchmark',
      label: t.runBenchmark,
      icon: <SpeedIcon fontSize="small" />,
      onClick: () =>
        setBenchmarkScope({
          kind: 'mapping',
          id: row.id,
          name: row.gateway_model_name,
          applicationId: row.application_id,
        }),
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
      // pointer-gated per field, so an ABSENT key carries no value for it.
      // Re-stating the value we happen to hold is not equivalent: it is a
      // snapshot from when this form opened, and it would overwrite a spec edit
      // made in between with no error and nothing on screen to show it.
      //
      // That is the CLOBBER, and omission is what removes it. The RACE is still
      // open and is not ours to close here: the backend re-writes the whole row
      // it loaded, so a spec-form PATCH that lands between this call's read and
      // its write is reverted anyway (11-risks-and-technical-debt.md §11.1).
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

  // The row that speaks for each spec (primaryStatusRows): the spec list's
  // state, and the one status row that carries the spec-wide overrides.
  const statusBySpecId = primaryStatusRows(statusRows);
  // Running instances per spec id, for the spec list's summary.
  const runningInstancesBySpecId = runningInstancesBySpec(statusRows);
  function statusForMapping(m: PortalModelMapping): RuntimeStatus | undefined {
    const specId = specsById[m.id]?.id;
    return specId ? statusBySpecId.get(specId) : undefined;
  }
  function statusLabelForMapping(m: PortalModelMapping): string {
    const live = statusForMapping(m);
    return live ? runtimeStateLabel(live.state, t) : t.runtimeStatusUnknown;
  }

  function liveStatusChip(m: PortalModelMapping) {
    const live = statusForMapping(m);
    const withheld = clusterWithheldReason(specsById[m.id]);
    if (!live && withheld) {
      return (
        <Box sx={{ display: 'grid', gap: 0.25, justifyItems: 'start' }}>
          <StatusChip status="standby" label={t.runtimeStatusUnknown} />
          <ClusterWithheldNotice reason={withheld} t={t} compact />
        </Box>
      );
    }
    if (!live) return <StatusChip status="standby" label={t.runtimeStatusUnknown} />;
    const baseChip = (
      <StatusChip status={runtimeStateBadge(live.state)} label={runtimeStateLabel(live.state, t)} />
    );
    const chip = withheld ? (
      <Box sx={{ display: 'grid', gap: 0.25, justifyItems: 'start' }}>
        {baseChip}
        <ClusterWithheldNotice reason={withheld} t={t} compact />
      </Box>
    ) : (
      baseChip
    );
    // A replicable spec's chip is the state of the row that speaks for it
    // (a running one first, see primaryStatusRows); how many instances run,
    // against the bounds, is the line under it. Only once the agent
    // reports the spec at all -- with no row, "0 running" would be a
    // claim nobody made.
    const spec = specsById[m.id];
    if (!spec?.replicable || spec.id === undefined) return chip;
    return (
      <Box sx={{ display: 'grid', gap: 0.25, justifyItems: 'start' }}>
        {chip}
        <Typography variant="caption" color="text.secondary">
          {instancesSummary(spec.id, spec, runningInstancesBySpecId, t)}
        </Typography>
      </Box>
    );
  }

  // The live-status chip with the cluster progress of a head beneath it.
  function liveStatusCell(m: PortalModelMapping) {
    const chip = liveStatusChip(m);
    if (!clusterStatusView(specsById[m.id])) return chip;
    return (
      <Box sx={{ display: 'grid', gap: 0.5, justifyItems: 'start' }}>
        {chip}
        <ClusterProgress spec={specsById[m.id]} t={t} compact />
      </Box>
    );
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
      // HOW the spec runs (native process, WSL2, later Docker) is a launch
      // property of the spec, so it lives here and never in the model-mapping
      // table, which carries capabilities and speeds only. The value is the
      // raw mode so the enum filter keeps working for a mode this build does
      // not know yet; an unconfigured row has none.
      id: 'execution',
      label: t.runtimeSpecExecutionMode,
      value: (m) => {
        const spec = specsById[m.id];
        return spec?.configured ? (spec.execution_mode ?? 'native') : '';
      },
      filter: 'enum',
      searchable: false,
      enumLabel: (v) => executionModeLabel(v, t),
      render: (m) => {
        const spec = specsById[m.id];
        if (!spec?.configured) return '—';
        const mode = spec.execution_mode ?? 'native';
        if (mode !== 'wsl') return executionModeLabel(mode, t);
        return (
          <Box sx={{ display: 'grid', gap: 0.25 }}>
            <span>{executionModeLabel(mode, t)}</span>
            {spec.wsl_distro && (
              <Typography variant="caption" color="text.secondary">
                {spec.wsl_distro}
              </Typography>
            )}
          </Box>
        );
      },
    },
    {
      id: 'gpus',
      label: t.runtimeSpecGpus,
      value: (m) => formatGpus(specsById[m.id]),
      filter: 'text',
      searchable: false,
    },
    {
      // Deliberately adjacent to the GPU column: together the two say whether
      // the cards listed there are what the process actually gets, or only
      // what the admission arithmetic believes it gets.
      id: 'set_visible_devices',
      label: t.runtimeSpecSetVisibleDevices,
      value: (m) =>
        visibilityValue(
          Boolean(specsById[m.id]?.set_visible_devices),
          specsById[m.id]?.visible_devices_mode ?? 'env',
        ),
      filter: 'enum',
      searchable: false,
      enumLabel: (v) => visibilityValueLabel(v as VisibilityValue, t),
      render: (m) =>
        renderVisibilityChip(
          visibilityValue(
            Boolean(specsById[m.id]?.set_visible_devices),
            specsById[m.id]?.visible_devices_mode ?? 'env',
          ),
          t,
        ),
    },
    {
      // The two resource summaries, one per axis the admission arithmetic
      // budgets. The cell TEXT carries the unit, but `value` is the bare
      // number (or the em-dash) so a numeric sort and the text filter work on
      // the magnitude, and an unknown row sorts to the bottom.
      id: 'vram_total',
      label: t.runtimeSpecVramTotal,
      value: (m) => {
        const spec = specsById[m.id];
        const total = isClusterHead(spec) ? (spec?.cluster_vram_total_mb ?? 0) : totalGpuVram(spec);
        return total > 0 ? String(total) : '—';
      },
      render: (m) => {
        const spec = specsById[m.id];
        if (spec && isClusterHead(spec)) {
          return (
            <ClusterTotalCell
              spec={spec}
              name={m.gateway_model_name}
              axis="vram"
              ownMb={totalGpuVram(spec)}
              t={t}
            />
          );
        }
        // How the spec runs is the execution column's job; this cell only says
        // where a WSL spec's VRAM figure comes from. Only a measured figure is
        // attributed from the VM: an estimate alone is the operator's, and
        // labelling it would claim a measurement that has not happened.
        if (spec?.execution_mode === 'wsl' && spec.gpus.some((g) => g.vram_measured_mb > 0)) {
          return (
            <Box sx={{ display: 'grid', gap: 0.25, justifyItems: 'end' }}>
              <span>{formatMb(totalGpuVram(spec))}</span>
              <Typography variant="caption" color="text.secondary">
                {t.runtimeStatusVRAMFromVM}
              </Typography>
            </Box>
          );
        }
        return formatMb(totalGpuVram(spec));
      },
      filter: 'text',
      searchable: false,
      numeric: true,
    },
    {
      id: 'ram',
      label: t.runtimeSpecRam,
      value: (m) => {
        const spec = specsById[m.id];
        const ram = isClusterHead(spec)
          ? (spec?.cluster_ram_total_mb ?? 0)
          : effectiveRam(spec, statusForMapping(m));
        return ram > 0 ? String(ram) : '—';
      },
      render: (m) => {
        const spec = specsById[m.id];
        const own = effectiveRam(spec, statusForMapping(m));
        return spec && isClusterHead(spec) ? (
          <ClusterTotalCell spec={spec} name={m.gateway_model_name} axis="ram" ownMb={own} t={t} />
        ) : (
          formatMb(own)
        );
      },
      filter: 'text',
      searchable: false,
      numeric: true,
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
      // For a spec stored as replicable, the idle timeout in effect (0:
      // never unloaded), not the manual override the form edits
      // (listedIdleTimeout).
      id: 'idle_timeout',
      label: t.runtimeSpecIdleTimeout,
      value: (m) => listedIdleTimeout(specsById[m.id], t.runtimeSpecIdleTimeoutNever),
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
      render: (m) => liveStatusCell(m),
    },
    {
      // Task 6: whether the agent's last per-model /metrics and context
      // probe actually reached the server -- so an operator who forgot
      // llama.cpp's --metrics flag sees it, instead of silently reading a 0
      // everywhere. Same spec-id join as `live_status` above, deliberately
      // adjacent to it: one column says "is it running", the next says
      // "can we actually see its numbers". Two independent chips (metrics /
      // context) because a runtime can serve one probe and not the other.
      id: 'probes',
      label: t.runtimeProbesColumn,
      value: (m) => {
        const live = statusForMapping(m);
        return [live?.metrics_probe, live?.context_probe].filter(Boolean).join(' ');
      },
      sortable: false,
      searchable: false,
      render: (m) => {
        const live = statusForMapping(m);
        const metrics = probeChipInfo(live?.metrics_probe ?? '', t.runtimeProbeMetricsPrefix, t);
        const context = probeChipInfo(live?.context_probe ?? '', t.runtimeProbeContextPrefix, t);
        // Both absent (no live status joined at all, or both probes are ""):
        // the shared "not measured" em-dash (#51), not `null` -- issue #57 is
        // exactly an empty cell here reading as a column that does not exist.
        if (!metrics && !context) return '—';
        // Tooltip + <span> wrapper, NOT Tooltip + StatusChip directly: MUI's
        // Tooltip works by cloning its event handlers (and a ref) onto its
        // child, and the shared StatusChip destructures only { status, label }
        // -- it spreads nothing onto the inner Chip and is not forwardRef, so
        // every tooltip prop handed to it is silently dropped and the tooltip
        // never opens. Wrapping in a plain element the Tooltip CAN attach to
        // is the same fix RowActionsMenu already uses for its disabled menu
        // items (Tooltip > span > item), and it leaves the shared chip's
        // contract untouched.
        return (
          <Box sx={{ display: 'flex', gap: 0.5, alignItems: 'center', flexWrap: 'wrap' }}>
            {metrics && (
              <Tooltip title={metrics.tooltip}>
                <span>
                  <StatusChip status={metrics.status} label={metrics.label} />
                </span>
              </Tooltip>
            )}
            {context && (
              <Tooltip title={context.tooltip}>
                <span>
                  <StatusChip status={context.status} label={context.label} />
                </span>
              </Tooltip>
            )}
          </Box>
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
  // tooltip), on every state including stopped. A failure with a stable code
  // this build knows reads as the code's label (the agent's message is
  // English); the code and the raw message then move into the tooltip.
  function renderLastError(err: RuntimeError | undefined) {
    if (!err) return '–';
    const labelKey = err.code ? errorLabelByCode[err.code] : undefined;
    return (
      <Tooltip
        title={
          <Box sx={{ display: 'grid', gap: 0.25 }}>
            {labelKey && <Typography variant="caption">{`${err.code}: ${err.message}`}</Typography>}
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
          <Typography variant="body2">{labelKey ? t[labelKey] : err.message}</Typography>
        </Box>
      </Tooltip>
    );
  }

  // A cluster head's progress on its Live-Status row: the spec list's compact
  // view, off the same spec, and a notice from the press of Start until the
  // start shows in it.
  function clusterLiveProgress(row: RuntimeStatus) {
    const mapping = mappingForStatus(row);
    const spec = mapping ? specsById[mapping.id] : undefined;
    if (!mapping || !spec?.cluster_enabled || instanceIndex(row) !== 0) return null;
    return (
      <Box sx={{ display: 'grid', gap: 0.25, justifyItems: 'start', flexBasis: '100%' }}>
        {/* A live region that is always there, so a screen reader announces
            the notice when it appears. */}
        <Box role="status" aria-live="polite">
          {clusterRefresh.startPending(mapping.id) && (
            <Chip
              size="small"
              variant="outlined"
              color="info"
              label={t.clusterStartRequested}
              data-testid="cluster-start-requested"
            />
          )}
        </Box>
        <ClusterProgress spec={spec} t={t} compact />
      </Box>
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
      value: (row) =>
        [gatewayNameFor(row), row.display_name, row.model, row.spec_id].filter(Boolean).join(' '),
      filter: 'text',
      render: (row) => {
        // A cluster member is no mapping row: its name, without the
        // unresolved marker.
        if (row.kind === 'rpc_server') return <RpcMemberName row={row} variant="body2" />;
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
              {row.model !== '' && row.model !== instanceAddress(mapping.app_model_name, row) && (
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
      // Which process of its spec the row is. Instance 0 -- the only row of a
      // spec that is not replicable -- reads #0 like any other, so the column
      // says the same thing on every row.
      id: 'instance',
      label: t.runtimeStatusInstance,
      value: (row) => String(instanceIndex(row)),
      render: (row) => `#${instanceIndex(row)}`,
      numeric: true,
      searchable: false,
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
          {clusterLiveProgress(row)}
        </Box>
      ),
    },
    {
      // Task 6 (#50) shipped this column into the MAPPING table's own
      // `columns` above, by the letter of its own design spec ("add one
      // compact Probes column to the mapping live-status table"). The
      // operator who went looking for it typed "Live-Status" and meant THIS
      // tab -- see #57. A Live-Status row IS a RuntimeStatus, so unlike the
      // mapping variant's `statusForMapping` spec-id join, this one reads
      // `row.metrics_probe`/`row.context_probe` directly. Positioned right
      // after `state`, mirroring the mapping table's `probes` column sitting
      // right after its own `live_status`: one column says "is it running",
      // the next says "can we actually see its numbers".
      id: 'probes',
      label: t.runtimeProbesColumn,
      value: (row) => [row.metrics_probe, row.context_probe].filter(Boolean).join(' '),
      sortable: false,
      searchable: false,
      render: (row) => {
        const metrics = probeChipInfo(row.metrics_probe, t.runtimeProbeMetricsPrefix, t);
        const context = probeChipInfo(row.context_probe, t.runtimeProbeContextPrefix, t);
        // Both "" (a legacy agent, no spec status has arrived yet, or the
        // child is not running): the shared "not measured" em-dash (#51), not
        // `null` -- see the identical comment on the mapping table's `probes`
        // column above.
        if (!metrics && !context) return '—';
        // Tooltip + <span> wrapper, NOT Tooltip + StatusChip directly: see
        // the mapping table's `probes` column above for why the wrapper is
        // load-bearing (MUI's Tooltip clones handlers onto its child; the
        // shared StatusChip is not forwardRef).
        return (
          <Box sx={{ display: 'flex', gap: 0.5, alignItems: 'center', flexWrap: 'wrap' }}>
            {metrics && (
              <Tooltip title={metrics.tooltip}>
                <span>
                  <StatusChip status={metrics.status} label={metrics.label} />
                </span>
              </Tooltip>
            )}
            {context && (
              <Tooltip title={context.tooltip}>
                <span>
                  <StatusChip status={context.status} label={context.label} />
                </span>
              </Tooltip>
            )}
          </Box>
        );
      },
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
            title: statusRowName(row),
            // The view is spec-wide: every instance writes into the spec's
            // one buffer, so for a replicable spec it says whose lines are
            // whose -- from whichever instance row it is opened.
            allInstances: showsAllInstanceLogs(
              specByRuntimeId.get(row.spec_id),
              statusRows,
              row.spec_id,
            ),
            // The view's per-instance tabs start from the instances the
            // stream lists for the spec now, and grow with every instance its
            // log entries name while it is open.
            instances: logInstanceIndexes(statusRows, row.spec_id),
            // It opens on the tab of the instance whose row was clicked.
            initialTab: instanceIndex(row),
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
    // Start, Stop and Pin, on the rows runtimeRowPlan puts them on: Start and
    // Pin on the row that speaks for the spec, Stop on its highest running or
    // starting instance. Start and Stop act once and write nothing; Pin sets
    // the persistent override through the same full-spec PUT as Clear
    // override. All of them wait while a restart or an override write runs:
    // the restart drives admin_state itself, and a start under its
    // force_stopped would only be refused.
    const plan = runtimeRowPlan(row, spec, statusRows, agentFeatures);
    if (plan.start) {
      const startKey = runtimeActionKey('start', row);
      let startTitle: string | undefined;
      if (plan.start.disabled === 'feature') startTitle = t.runtimeStartFeatureMissing;
      else if (plan.start.disabled === 'force_stopped') startTitle = t.runtimeStartForceStopped;
      else if (plan.start.disabled === 'max_instances') {
        startTitle = t.runtimeStartMaxInstances(startInstanceLimit(spec));
      } else if (plan.start.more) startTitle = t.runtimeStartMore;
      actions.push({
        key: 'start',
        label: t.runtimeStart,
        icon: <PlayArrowIcon fontSize="small" />,
        disabled:
          plan.start.disabled !== undefined || overridesLocked || runtimeActionsBusy.has(startKey),
        title: startTitle,
        onClick: () => startRuntimeInstance(row, spec),
      });
    }
    if (plan.stop) {
      const stopKey = runtimeActionKey('stop', row);
      let stopTitle: string | undefined;
      if (plan.stop.disabled === 'feature') stopTitle = t.runtimeStopFeatureMissing;
      else if (plan.stop.disabled === 'protected') stopTitle = t.runtimeStopProtected;
      else if (plan.stop.disabled === 'draining') stopTitle = t.runtimeStopHigherDraining;
      actions.push({
        key: 'stop',
        label: t.runtimeStop,
        icon: <StopIcon fontSize="small" />,
        // It also waits while a Start of the spec is in flight: the agent
        // answers a stop of the instance that start is waiting on by failing
        // the start, which would then read as a refusal for lack of room.
        disabled:
          plan.stop.disabled !== undefined ||
          overridesLocked ||
          runtimeActionsBusy.has(stopKey) ||
          runtimeActionsBusy.has(runtimeActionKey('start', row)),
        title: stopTitle,
        onClick: () => requestStop(row, spec),
      });
    }
    const pin = plan.pin;
    if (pin) {
      actions.push({
        key: 'pin',
        label: pin === 'force_running' ? t.runtimePinRunning : t.runtimePinStopped,
        icon: <PushPinIcon fontSize="small" />,
        disabled: overridesLocked,
        onClick: () => void setOverride(spec, pin),
      });
    }
    // Clear override and Restart act on the whole spec, all of its instances
    // at once, so they are offered once, on the row that speaks for the spec:
    // a Restart beside instance 1 would read as restarting instance 1 alone.
    const primary = statusBySpecId.get(row.spec_id);
    if (primary === undefined || instanceIndex(primary) !== instanceIndex(row)) return actions;
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
      // Over every instance row of the spec, not this row alone: the wait
      // needs all of them `stopped` (restartGate).
      const gate = restartGate(statusRows, row.spec_id, restartableStates, spec);
      const restartBlocked = gate !== 'open';
      // A successful restart ends with the row `stopped`, where Restart is
      // disabled -- so this is the RESTING state of a healthy row, not an edge
      // case, and every operator meets it. An unexplained grey button is what
      // makes a correct gate look like a portal bug. (`overridesLocked` gets
      // no reason of its own: it is transient and already narrated by the
      // row's own "stopping…" / "clearing…" chip.)
      let restartTitle: string | undefined;
      if (gate === 'state') restartTitle = t.runtimeRestartUnavailable;
      else if (gate === 'instance') restartTitle = t.runtimeRestartUnavailableInstance;
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
        // a portal bug -- but never clickable there. Start, offered above, is
        // the action that does something on those three.
        disabled: overridesLocked || restartBlocked,
        // …and SAY so on hover (restartTitle).
        title: restartTitle,
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
      value: (s) => visibilityValue(s.setVisibleDevices, s.visibleDevicesMode),
      searchable: false,
      render: (s) =>
        renderVisibilityChip(visibilityValue(s.setVisibleDevices, s.visibleDevicesMode), t),
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

  // Consolidated benchmark sub-view, pre-scoped to the clicked mapping row's
  // model. Same full-render-takeover convention as the two sub-views below.
  //
  // The one hazard the copy from `MappingSection` must NOT reproduce: that
  // component's benchmark wrapper renders its OWN <Breadcrumbs> bar, and this
  // section already renders one -- a naive copy stacks two. So the bar here is
  // built from THIS section's `trail` (the single source every sub-view uses)
  // and `BenchmarkSection` carries no bar of its own, which leaves exactly one.
  if (benchmarkScope) {
    return (
      <>
        <Breadcrumbs
          ariaLabel={t.breadcrumb}
          backLabel={t.back}
          items={[
            ...trail,
            { label: application.endpoint, onClick: () => setBenchmarkScope(null) },
            { label: t.benchmarkArea },
          ]}
        />
        <BenchmarkSection
          key={`bench-${server.id}`}
          t={t}
          api={api}
          server={server}
          initialScope={benchmarkScope}
          // A completed run may have discovered a model's context size, so the
          // one copy of the mappings these tabs share is refreshed on completion.
          onModelsChanged={() => void reloadMappings()}
          pollIntervalMs={pollIntervalMs}
        />
      </>
    );
  }

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
            // One difference from the ordinary mapping screen: this screen only
            // ever shows a server_agent application, whose context probe the
            // gateway derives itself (the agent router's
            // /upstream/{model}/props, or the agent's telemetry). The
            // application form clears its context_probe_path, so the probe
            // does not wait for one.
            contextProbeDerived
            row={mappingEdit}
            // The other difference, and it is an ownership boundary: this
            // application HAS a runtime spec, and the spec owns the application
            // model name (its `upstream_model`).
            appNameReadOnly
            busy={busy}
            onSubmit={(values) => void saveMappingFromTab(mappingEdit.id, values)}
            onCancel={() => setMappingEdit(null)}
            pollIntervalMs={pollIntervalMs}
          />
        </Panel>
      </>
    );
  }

  // Create / edit sub-view (input mask) -- replaces the tabbed view entirely,
  // matching the rest of the portal's drill-down/sub-view convention.
  if (specMode !== 'list') {
    const editing = specMode !== 'create';
    // The loaded spec's read-only token echoes, read straight from the same
    // cache the table columns use (specsById) rather than duplicated into
    // form state -- exactly how ApplicationSection reads `editApp?.api_token_set`
    // from the app object it was opened with instead of copying it into a
    // useState. On CREATE there is no spec yet, so `app_api_token_set` falls
    // back to the parent application's OWN current token state (the same
    // snapshot-from-the-parent convention openCreate already uses for
    // api_flavors/responses_mode/messages_mode above).
    const currentSpec = editingSpecMappingId ? specsById[editingSpecMappingId] : undefined;
    const currentApiTokenSet = currentSpec?.api_token_set ?? false;
    const appApiTokenSet = currentSpec?.app_api_token_set ?? application.api_token_set;
    // The inherited-header display below uses the same read-only-echo-with-
    // create-time fallback as appApiTokenSet immediately above, but for the
    // header NAME rather than the set/unset fact.
    const appApiTokenHeader = currentSpec?.app_api_token_header ?? application.api_token_header;
    // Recomputed on every keystroke on purpose: the whole defect being fixed
    // is that the field's contract was invisible until a foreign program
    // rejected it, so the feedback has to land at paste time, not at submit.
    const argsWarnings = collectArgsWarnings(parseArgsText(argsText), listenPort, t);
    // Part A/B agent-capability + platform hints (non-blocking). agentFeatures /
    // agentVersion are the report-derived values (:1703-1704); the host OS comes
    // from the hardware report already fetched (:1394-1396) — NOT a new DTO field.
    const agentHasGpuSelection = agentFeatures.includes('gpu_selection');
    // The execution-mode select appears only where wsl is usable: an agent
    // declaring runtime_exec_wsl, or a spec already stored as wsl (which must
    // stay editable, and says why it will not reach this agent).
    const agentHasExecWsl = agentFeatures.includes(RUNTIME_EXEC_WSL_FEATURE);
    const showExecutionMode = agentHasExecWsl || executionMode === 'wsl';
    const showAgentNoWsl = executionMode === 'wsl' && reportReady && !agentHasExecWsl;
    // The config-files editor's live feedback and its agent-capability hint
    // (#257). Advisory like the GPU hints: an old agent still accepts the
    // spec, it just never writes the files -- and ${CONFIG_DIR} stays literal
    // -- so the warning is shown only once the operator actually has files
    // configured, never as standing noise on every spec. `reportReady` keeps it
    // from guessing about an agent whose features have not been read yet.
    const configFileRowIssues = configFileIssues(configFiles);
    const showAgentNoConfigFiles =
      configFiles.length > 0 && reportReady && !agentFeatures.includes('runtime_config_files');
    const gpuOrderIsCustom = gpuRows.some((r, i, all) => i > 0 && r.index < all[i - 1].index);
    const argsHaveMetalDevices = parseArgsText(argsText).some((a) =>
      a.includes('${METAL_DEVICES}'),
    );
    // The OPTIONAL per-backend highlight below, layered onto the general
    // unsupported-backend note. A runtime spec's application.type is
    // ALWAYS 'server_agent' (that is what owns a runtime spec at all), so it
    // carries no signal about which child model server this spec launches --
    // the binary path is the only one available, and even that is used only
    // to highlight a specific backend by name, never to gate the general note
    // itself (see collectArgsWarnings-style live feedback elsewhere in this
    // form for the same non-blocking-hint idiom).
    const apiTokenBinaryLower = basename(binary).toLowerCase();
    let apiTokenBackendHintKey: MessageKey | null = null;
    if (apiTokenBinaryLower.includes('ollama')) {
      apiTokenBackendHintKey = 'runtimeSpecApiTokenBackendBannerOllamaHint';
    } else if (
      apiTokenBinaryLower.includes('llama-swap') ||
      apiTokenBinaryLower.includes('llama_swap')
    ) {
      apiTokenBackendHintKey = 'runtimeSpecApiTokenBackendBannerLlamaSwapHint';
    }
    const agentOs = hardware.data?.available && hardware.data.report ? hardware.data.report.os : '';
    const agentOsKnown = agentOs !== '';
    const isMacOsAgent = /darwin|mac ?os/i.test(agentOs);
    // Prominent when args mode (the process would fail to start on an old agent);
    // informational when only the order is custom (order is ignored until >=0.4.0).
    const showAgentTooOldArgs =
      setVisibleDevices && visibleDevicesMode === 'args' && reportReady && !agentHasGpuSelection;
    const showAgentTooOldOrder =
      !showAgentTooOldArgs && gpuOrderIsCustom && reportReady && !agentHasGpuSelection;
    const showMetalNonMacos =
      setVisibleDevices &&
      visibleDevicesMode === 'args' &&
      argsHaveMetalDevices &&
      agentOsKnown &&
      !isMacOsAgent;
    // The two flavor warnings of RuntimeWarnings, stated in the form, which
    // hides the warnings banner while it is open.
    const showFlavorsNotOnApplication = tickedFlavorMissingOnApplication(
      specApiFlavors,
      application.api_flavors,
    );
    const showTextFlavorsOnSd = textFlavorsOnStableDiffusion(
      specApiFlavors,
      formSpecIsStableDiffusion(specType, binary, specLoadedType),
    );
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
                argv. So on EDIT the gateway name is SHOWN read-only (a spec is
                unreadable without knowing which model it serves) and edited one
                tab to the left, and the status select is gone entirely: it
                edited the MAPPING and never reached putRuntimeSpec, whose
                request type has no status field at all.

                On CREATE there is NO gateway field: the operator enters only
                the application model name and `submitCreate` DERIVES the gateway
                name from it (gateway == app), exactly as model discovery seeds a
                new mapping. A distinct gateway alias is set later on the
                Modell-Zuordnung tab's edit -- the discovery-then-rename flow.
                Ownership rule: do not re-add a gateway field here, or the
                operator types one name twice and two screens write it again.

                Do not confuse the removed status select with the "Aktiv"
                checkbox below. That one is `spec.enabled`, which decides whether
                this spec enters the agent's runtime-config document -- a
                different question from whether the gateway routes the model. */}
            <Typography variant="subtitle2" component="h3">
              {t.runtimeSpecModelSection}
            </Typography>
            {editing && (
              <Field
                id="runtime-spec-gateway-name"
                label={t.mappingGatewayName}
                value={gatewayName}
                // Read-only on EDIT, and the no-op is load-bearing: jsdom fires
                // `change` on a readOnly input, so a live handler would still
                // drive state and give a test a false green. The mapping owns
                // this name; the Modell-Zuordnung tab edits it.
                onChange={() => {}}
                readOnly
                helperText={t.runtimeSpecGatewayNameReadOnly}
              />
            )}
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
            <ApiVariantControls
              t={t}
              apiFlavors={specApiFlavors}
              responsesMode={specResponsesMode}
              messagesMode={specMessagesMode}
              liveTimings={specLiveTimings}
              liveTimingsKind={specLiveTimingsKind}
              onFlavorsChange={(next) => {
                setSpecApiFlavors(next);
                setSpecFlavorsTouched(true);
              }}
              flavorsError={
                specFlavorsTouched && specApiFlavors.length === 0
                  ? t.runtimeSpecFlavorsRequired
                  : undefined
              }
              flavorsGroupRef={specFlavorsGroupRef}
              onResponsesModeChange={setSpecResponsesMode}
              onMessagesModeChange={setSpecMessagesMode}
              decisionsMode={specDecisionsMode}
              systemOneMode={specSystemOneMode}
              decisionsPassthroughOffered={decisionsPassthroughOffered(specType)}
              systemOneTranslateOffered={systemOneTranslateOffered(specType)}
              onDecisionsModeChange={setSpecDecisionsMode}
              onSystemOneModeChange={setSpecSystemOneMode}
              onLiveTimingsChange={setSpecLiveTimings}
            />
            {showFlavorsNotOnApplication && (
              <Alert severity="warning">{t.runtimeSpecFlavorsNotOnApplicationHint}</Alert>
            )}
            {showTextFlavorsOnSd && (
              <Alert severity="warning">{t.runtimeSpecTextFlavorsOnStableDiffusionHint}</Alert>
            )}
            <FormControlLabel
              control={
                <Checkbox checked={enabled} onChange={(e) => setEnabled(e.target.checked)} />
              }
              label={t.runtimeSpecEnabled}
            />
            <BinaryPicker
              id="runtime-spec-binary"
              label={executionMode === 'wsl' ? t.runtimeSpecBinaryLinux : t.runtimeSpecBinary}
              t={t}
              kind="llama_server"
              value={binary}
              onChange={setBinary}
              onSelect={(b) => {
                // An inventory row carries its own execution mode and distro.
                setExecutionMode(b.execution === 'wsl' ? 'wsl' : 'native');
                if (b.execution === 'wsl') setWslDistro(b.distro);
              }}
              binaries={serverBinaries}
              required
              placeholder="/usr/local/bin/llama-server"
            />
            {/* RuntimeSpec Type: explicit backend kind ('' = auto-detect from
                `binary` above), plus the per-type metrics/context-probe path
                overrides. Mirrors the api_token_mode SelectField below --
                writable state, options from i18n. The read-only echoes right
                after it (effective_type/resolved_metrics_path/
                resolved_context_probe_path) show what the backend actually
                resolves these three to, straight from the loaded spec
                (`currentSpec`) rather than form state, since nothing here
                ever writes them -- so an operator relying on Auto still sees
                the detected outcome without guessing. */}
            {showExecutionMode && (
              <>
                <SelectField
                  id="runtime-spec-execution-mode"
                  label={t.runtimeSpecExecutionMode}
                  value={executionMode}
                  onChange={(e) => setExecutionMode(e.target.value as 'native' | 'wsl')}
                  sx={{ maxWidth: 340 }}
                >
                  <option value="native">{t.runtimeSpecExecutionModeNative}</option>
                  <option value="wsl">{t.runtimeSpecExecutionModeWSL}</option>
                </SelectField>
                {executionMode === 'wsl' && (
                  <>
                    <Field
                      id="runtime-spec-wsl-distro"
                      label={t.runtimeSpecWSLDistro}
                      value={wslDistro}
                      onChange={(e) => setWslDistro(e.target.value)}
                      sx={{ maxWidth: 340 }}
                    />
                    <Typography variant="caption" color="text.secondary" sx={{ mt: -1 }}>
                      {t.runtimeSpecWSLHint}
                    </Typography>
                  </>
                )}
                {showAgentNoWsl && (
                  <Alert severity="warning">{t.runtimeSpecWSLAgentUnsupported}</Alert>
                )}
              </>
            )}
            <SelectField
              id="runtime-spec-type"
              label={t.runtimeSpecType}
              value={specType}
              onChange={(e) => {
                const next = e.target.value as RuntimeSpec['type'];
                // Move an UNTOUCHED health path to the new type's default;
                // leave an edited one alone. Mirrors migrateTypeFields'
                // preservation contract on the application form. Only an
                // explicit type has a known default: Auto's '/health' is a
                // guess (the kind is detected from `binary` in Go), so a
                // switch TO Auto moves nothing -- on an edit it would
                // otherwise save '/health' over a stored '/v1/models'.
                if (next !== '' && healthPath === runtimeSpecHealthPathDefault(specType)) {
                  setHealthPath(runtimeSpecHealthPathDefault(next));
                }
                applySpecTypeFlavorDefaults(next);
                setSpecDecisionsMode(decisionsModeForKind(specDecisionsMode, next));
                setSpecSystemOneMode(systemOneModeForKind(specSystemOneMode, next));
                setSpecType(next);
              }}
              sx={{ maxWidth: 340 }}
              helperText={runtimeSpecTypeNote(specType, t)}
            >
              <option value="">{t.runtimeSpecTypeAuto}</option>
              <option value="vllm">{t.runtimeSpecTypeVllm}</option>
              <option value="llama_cpp">{t.runtimeSpecTypeLlamaCpp}</option>
              <option value="tgi">{t.runtimeSpecTypeTgi}</option>
              <option value="ollama">{t.runtimeSpecTypeOllama}</option>
              <option value="stable_diffusion_cpp">{t.runtimeSpecTypeStableDiffusionCpp}</option>
              <option value="audio_cpp">{t.runtimeSpecTypeAudioCpp}</option>
              <option value="vllm_sr">{t.runtimeSpecTypeVllmSr}</option>
              <option value="custom">{t.runtimeSpecTypeCustom}</option>
            </SelectField>
            <Field
              id="runtime-spec-metrics-path"
              label={t.runtimeSpecMetricsPath}
              value={metricsPath}
              onChange={(e) => setMetricsPath(e.target.value)}
              helperText={t.runtimeSpecMetricsPathHelp}
            />
            <Field
              id="runtime-spec-context-probe-path"
              label={t.runtimeSpecContextProbePath}
              value={contextProbePath}
              onChange={(e) => setContextProbePath(e.target.value)}
              helperText={t.runtimeSpecContextProbePathHelp}
            />
            <Field
              id="runtime-spec-effective-type"
              label={t.runtimeSpecEffectiveType}
              value={currentSpec?.effective_type ?? ''}
              // See the vram-benchmark read-only Field above: the no-op
              // matters because jsdom's fireEvent.change fires even on a
              // readOnly input.
              onChange={() => {}}
              readOnly
              sx={{ maxWidth: 340 }}
            />
            <Field
              id="runtime-spec-resolved-metrics-path"
              label={t.runtimeSpecResolvedMetricsPath}
              value={currentSpec?.resolved_metrics_path ?? ''}
              onChange={() => {}}
              readOnly
            />
            <Field
              id="runtime-spec-resolved-context-probe-path"
              label={t.runtimeSpecResolvedContextProbePath}
              value={currentSpec?.resolved_context_probe_path ?? ''}
              onChange={() => {}}
              readOnly
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
                    {specType === 'stable_diffusion_cpp'
                      ? t.runtimeSpecArgsExampleSdcpp
                      : t.runtimeSpecArgsExample}
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
            <Box sx={{ display: 'grid', gap: 1 }}>
              <Typography variant="subtitle2" component="h3">
                {t.runtimeSpecConfigFiles}
              </Typography>
              <Typography variant="caption" color="text.secondary">
                {t.runtimeSpecConfigFilesHint}
              </Typography>
              {showAgentNoConfigFiles && (
                <Alert severity="warning">
                  {`${t.runtimeSpecAgentNoConfigFiles} (${t.runtimeAgentVersion}: ${agentVersion || '—'})`}
                </Alert>
              )}
              {configFiles.map((row, idx) => {
                const issue = configFileRowIssues[idx];
                // A blank name is refused at submit but not painted red while
                // the row is still new and empty.
                const nameIssue =
                  issue === 'name_invalid' || issue === 'name_duplicate' ? issue : null;
                const contentIssue =
                  issue === 'content_too_large' || issue === 'content_secret' ? issue : null;
                return (
                  <Box
                    key={row.rowKey}
                    sx={{
                      display: 'grid',
                      gap: 1,
                      p: 1.5,
                      border: 1,
                      borderColor: 'divider',
                      borderRadius: 1,
                    }}
                  >
                    <Box sx={{ display: 'flex', gap: 1.5, alignItems: 'flex-start' }}>
                      <Field
                        id={`runtime-spec-config-file-name-${idx}`}
                        label={t.runtimeSpecConfigFileName}
                        value={row.name}
                        onChange={(e) => updateConfigFile(idx, { name: e.target.value })}
                        placeholder={t.runtimeSpecConfigFileNamePlaceholder}
                        error={nameIssue !== null}
                        helperText={nameIssue ? t[configFileIssueLabel[nameIssue]] : undefined}
                        inputProps={{ spellCheck: false }}
                      />
                      <Button
                        type="button"
                        size="small"
                        color="secondary"
                        sx={{ flexShrink: 0, mt: 0.5 }}
                        onClick={() => removeConfigFile(idx)}
                      >
                        {t.runtimeSpecConfigFileRemove}
                      </Button>
                    </Box>
                    <Field
                      id={`runtime-spec-config-file-content-${idx}`}
                      label={t.runtimeSpecConfigFileContent}
                      value={row.content}
                      onChange={(e) => updateConfigFile(idx, { content: e.target.value })}
                      multiline
                      minRows={4}
                      error={contentIssue !== null}
                      helperText={contentIssue ? t[configFileIssueLabel[contentIssue]] : undefined}
                      inputProps={{ spellCheck: false }}
                      sx={{ '& textarea': { fontFamily: 'monospace' } }}
                    />
                  </Box>
                );
              })}
              <Box>
                <Button
                  type="button"
                  variant="outlined"
                  size="small"
                  disabled={configFiles.length >= CONFIG_FILES_MAX}
                  onClick={addConfigFile}
                >
                  {t.runtimeSpecConfigFileAdd}
                </Button>
              </Box>
            </Box>
            <Field
              id="runtime-spec-work-dir"
              label={executionMode === 'wsl' ? t.runtimeSpecWorkDirLinux : t.runtimeSpecWorkDir}
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
              onChange={(e) => {
                setHealthPath(e.target.value);
                setHealthPathEdited(true);
              }}
              // An empty field is sent empty and the backend's default for
              // the EFFECTIVE type decides, so the placeholder names that
              // default only where this form knows it: for an explicit
              // type. Under Auto it would be a guess, so there is none.
              placeholder={specType === '' ? undefined : runtimeSpecHealthPathDefault(specType)}
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
              // With several instances this field is the manual override of
              // an automatic timeout, and its 0 changes meaning from "never
              // unload" to "no override" -- said where the 0 is typed.
              helperText={replicable ? t.runtimeSpecIdleTimeoutOverrideHint : undefined}
              inputProps={{ min: 0 }}
            />
            {/* While the box is ticked: the measured startup, which the agent
                measures for every spec (none yet on Create), and beside it the
                timeout the gateway actually pushes -- only for a spec STORED
                as replicable: for any other the figure is the stored manual
                value, not an automatic one, so ticking the box on such a spec
                shows the startup alone until a save has made the gateway
                derive it. A timeout in effect of 0 reads "never unloaded". */}
            {replicable && (
              <Typography variant="caption" color="text.secondary" sx={{ mt: -1 }}>
                {currentSpec?.configured && currentSpec.replicable
                  ? t.runtimeSpecAutoIdleTimeout(
                      currentSpec.effective_idle_timeout_seconds,
                      currentSpec.startup_measured_ms,
                    )
                  : t.runtimeSpecMeasuredStartup(currentSpec?.startup_measured_ms ?? 0)}
              </Typography>
            )}
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
            {/* Opt-in: unticked, the spec runs exactly one process, as it did
                before instances existed. The bounds and the parallelism mean
                nothing without it, so they are shown only with it, and
                unticked a save sends the neutral 0/0/0 in their place and
                never checks them (instanceSettingsBody, validateInstances). */}
            <FormControlLabel
              control={
                <Checkbox checked={replicable} onChange={(e) => setReplicable(e.target.checked)} />
              }
              label={t.runtimeSpecReplicable}
            />
            <Typography variant="caption" color="text.secondary" sx={{ mt: -1 }}>
              {t.runtimeSpecReplicableHint}
            </Typography>
            {replicable && (
              <>
                <Field
                  id="runtime-spec-min-instances"
                  type="number"
                  label={t.runtimeSpecMinInstances}
                  value={minInstancesText}
                  onChange={(e) => setMinInstancesText(e.target.value)}
                  inputProps={{ min: 0, max: INSTANCES_MAX }}
                />
                {/* The minimum is above the real-time concurrency the audio
                    ramp measured as a ceiling for this mapping (#340). A
                    warning, never a refusal -- an operator may keep instances
                    up for other reasons. It follows the minimum as typed,
                    because the save closes this editor: the stored spec's
                    flag speaks for the stored minimum, and an edited one is
                    judged by the gateway's rule against the mapping's
                    measured figures (minRealtimeWarning). */}
                {minRealtimeWarning(
                  instanceCountFromText(minInstancesText),
                  currentSpec,
                  editingSpecMappingId ? mappingById.get(editingSpecMappingId) : undefined,
                ) && <Alert severity="warning">{t.runtimeSpecMinExceedsRealtime}</Alert>}
                <Field
                  id="runtime-spec-max-instances"
                  type="number"
                  label={t.runtimeSpecMaxInstances}
                  value={maxInstancesText}
                  onChange={(e) => setMaxInstancesText(e.target.value)}
                  helperText={t.runtimeSpecMaxInstancesUnlimited}
                  inputProps={{ min: 0, max: INSTANCES_MAX }}
                />
                <Field
                  id="runtime-spec-instance-parallelism"
                  type="number"
                  label={t.runtimeSpecInstanceParallelism}
                  value={instanceParallelismText}
                  onChange={(e) => setInstanceParallelismText(e.target.value)}
                  inputProps={{ min: 1, max: INSTANCE_PARALLELISM_MAX }}
                />
              </>
            )}
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
            {setVisibleDevices && (
              <>
                <SelectField
                  id="runtime-spec-visible-devices-mode"
                  label={t.runtimeSpecVisibleDevicesMode}
                  value={visibleDevicesMode}
                  onChange={(e) => setVisibleDevicesMode(e.target.value as 'env' | 'args')}
                  sx={{ maxWidth: 340 }}
                >
                  <option value="env">{t.runtimeSpecVisibleDevicesModeEnv}</option>
                  <option value="args">{t.runtimeSpecVisibleDevicesModeArgs}</option>
                </SelectField>
                {visibleDevicesMode === 'args' && (
                  <Typography variant="caption" color="text.secondary" sx={{ mt: -1 }}>
                    {t.runtimeSpecVisibleDevicesModeArgsHint}
                  </Typography>
                )}
              </>
            )}
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

            {/* Runtime-Spec API Token: the MODE controls. */}
            <Box sx={{ display: 'grid', gap: 1 }}>
              <SelectField
                id="runtime-spec-api-token-mode"
                label={t.runtimeSpecApiTokenMode}
                value={apiTokenMode}
                onChange={(e) => {
                  const mode = e.target.value as RuntimeSpec['api_token_mode'];
                  setApiTokenMode(mode);
                  // Leaving 'random' abandons a not-yet-saved rotate request --
                  // it would otherwise silently reappear if the operator
                  // switched back to 'random' later in the same edit.
                  if (mode !== 'random') setApiTokenRotate(false);
                }}
                sx={{ maxWidth: 340 }}
              >
                <option value="app">{t.runtimeSpecApiTokenModeApp}</option>
                <option value="set">{t.runtimeSpecApiTokenModeSet}</option>
                <option value="random">{t.runtimeSpecApiTokenModeRandom}</option>
                <option value="off">{t.runtimeSpecApiTokenModeOff}</option>
              </SelectField>
              {apiTokenMode === 'app' && appApiTokenSet === false && (
                <Alert severity="info">{t.runtimeSpecApiTokenAppUnsetHint}</Alert>
              )}
              {apiTokenMode === 'set' && (
                <Box>
                  <Field
                    id="runtime-spec-api-token"
                    type="password"
                    label={t.runtimeSpecApiTokenLabel}
                    value={apiTokenCleared ? '' : apiTokenInput}
                    onChange={(e) => {
                      setApiTokenInput(e.target.value);
                      setApiTokenCleared(false);
                    }}
                    autoComplete="new-password"
                    placeholder={
                      editing && currentApiTokenSet && !apiTokenCleared
                        ? t.runtimeSpecApiTokenSetPlaceholder
                        : undefined
                    }
                    helperText={t.runtimeSpecApiTokenNote}
                  />
                  <Typography variant="caption" color="text.secondary">
                    {currentApiTokenSet && !apiTokenCleared
                      ? t.runtimeSpecApiTokenSetIndicator
                      : t.runtimeSpecApiTokenUnsetIndicator}
                  </Typography>
                  {editing && currentApiTokenSet && !apiTokenCleared && (
                    <Button
                      type="button"
                      size="small"
                      variant="text"
                      color="secondary"
                      onClick={() => {
                        setApiTokenCleared(true);
                        setApiTokenInput('');
                      }}
                    >
                      {t.runtimeSpecApiTokenClear}
                    </Button>
                  )}
                </Box>
              )}
              {apiTokenMode === 'random' && (
                <Box sx={{ display: 'grid', gap: 0.5 }}>
                  <Typography variant="caption" color="text.secondary">
                    {t.runtimeSpecApiTokenRandomHint}
                  </Typography>
                  <Button
                    type="button"
                    size="small"
                    variant="outlined"
                    disabled={apiTokenRotate}
                    onClick={() => setApiTokenRotate(true)}
                    sx={{ justifySelf: 'start' }}
                  >
                    {apiTokenRotate
                      ? t.runtimeSpecApiTokenRotatePending
                      : t.runtimeSpecApiTokenRotate}
                  </Button>
                </Box>
              )}
            </Box>

            {/* Runtime-Spec API Token header: which header CARRIES the token
                above, as opposed to the mode block's WHAT the token IS.
                Hidden entirely under 'off' -- there is no token to attach a
                header to, and the effective-header question the app-source
                display and the custom-header warning both answer does not
                arise. `apiTokenHeaderSource`/`apiTokenHeader` are declared
                alongside apiTokenMode above (buildSpecBody/hydrateSpecFields
                already round-trip them) -- this block is UI only, no new
                state. */}
            {apiTokenMode !== 'off' && (
              <Box sx={{ display: 'grid', gap: 1 }}>
                <SelectField
                  id="runtime-spec-api-token-header-source"
                  label={t.runtimeSpecApiTokenHeaderSource}
                  value={apiTokenHeaderSource}
                  onChange={(e) =>
                    setApiTokenHeaderSource(
                      e.target.value as RuntimeSpec['api_token_header_source'],
                    )
                  }
                  sx={{ maxWidth: 340 }}
                >
                  <option value="app">{t.runtimeSpecApiTokenHeaderSourceApp}</option>
                  <option value="custom">{t.runtimeSpecApiTokenHeaderSourceCustom}</option>
                </SelectField>
                {apiTokenHeaderSource === 'app' && (
                  <Field
                    id="runtime-spec-api-token-header-inherited"
                    label={t.runtimeSpecApiTokenHeaderInheritedLabel}
                    value={
                      appApiTokenHeader.trim() === ''
                        ? t.runtimeSpecApiTokenHeaderDefault
                        : appApiTokenHeader
                    }
                    // See the vram-benchmark read-only Field above: the no-op
                    // matters because jsdom's fireEvent.change fires even on a
                    // readOnly input.
                    onChange={() => {}}
                    readOnly
                    sx={{ maxWidth: 340 }}
                  />
                )}
                {apiTokenHeaderSource === 'custom' && (
                  <Box>
                    <Field
                      id="runtime-spec-api-token-header-custom"
                      label={t.runtimeSpecApiTokenHeaderLabel}
                      value={apiTokenHeader}
                      onChange={(e) => setApiTokenHeader(e.target.value)}
                      placeholder="Authorization: Bearer"
                      helperText={t.runtimeSpecApiTokenHeaderHelp}
                      sx={{ maxWidth: 340 }}
                    />
                    {apiTokenHeader.trim() !== '' && (
                      <Alert severity="warning" sx={{ mt: 1 }}>
                        {t.runtimeSpecApiTokenHeaderMismatchWarning}
                      </Alert>
                    )}
                  </Box>
                )}
              </Box>
            )}

            {/* Runtime-Spec API Token operator guidance: the
                per-backend variable table, the loud Args-leak warning, and the
                structural unsupported-backend note. Shown whenever the mode is
                not 'off' -- once there IS a token to wire up, the operator
                needs to know how to hand it to the child process and where
                that mechanism cannot be trusted. The banner is deliberately a
                GENERAL note rather than a per-app.Type switch: a runtime spec
                always belongs to a 'server_agent' application (that field
                never distinguishes Ollama from vLLM from llama.cpp), and the
                spec's `binary` path is the only signal at all -- used above
                only for the optional per-backend highlight, never to gate
                this block's visibility. */}
            {apiTokenMode !== 'off' && (
              <Box sx={{ display: 'grid', gap: 1 }}>
                <Alert severity="info">{t.runtimeSpecApiTokenBackendHint}</Alert>
                <Table size="small" sx={{ maxWidth: 640 }}>
                  <TableHead>
                    <TableRow>
                      <TableCell>{t.runtimeSpecApiTokenTableBackend}</TableCell>
                      <TableCell>{t.runtimeSpecApiTokenTableEnv}</TableCell>
                      <TableCell>{t.runtimeSpecApiTokenTableArgs}</TableCell>
                      <TableCell>{t.runtimeSpecApiTokenTableClient}</TableCell>
                    </TableRow>
                  </TableHead>
                  <TableBody>
                    {apiTokenBackendRows.map((row) => (
                      <TableRow key={row.key}>
                        <TableCell>{t[row.backendKey]}</TableCell>
                        <TableCell>
                          <code>{t[row.envKey]}</code>
                        </TableCell>
                        <TableCell>
                          <code>{t[row.argsKey]}</code> {t.runtimeSpecApiTokenArgsReadableMark}
                        </TableCell>
                        <TableCell>{t[row.clientKey]}</TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
                {argsText.includes('${API_TOKEN}') && (
                  <Alert severity="warning">{t.runtimeSpecApiTokenArgsLeakWarning}</Alert>
                )}
                <Alert severity="warning">
                  {t.runtimeSpecApiTokenBackendBanner}
                  {apiTokenBackendHintKey && <> {t[apiTokenBackendHintKey]}</>}
                </Alert>
              </Box>
            )}

            {/* RAM is HOST-scoped, so this is ONE spec-level field and not a
                column of the per-GPU rows below (GpuRow stays VRAM-only). It
                has to stay bound to the form's state and to buildSpecBody
                regardless of how it is laid out: the PUT is a full-document
                upsert, and a save that omitted the estimate would clear one
                set through the API. */}
            <Field
              id="runtime-spec-ram-estimate"
              type="number"
              label={t.runtimeSpecRamEstimate}
              value={String(ramEstimateMb)}
              onChange={(e) => setRamEstimateMb(e.target.value === '' ? 0 : Number(e.target.value))}
              inputProps={{ min: 0 }}
            />
            <Typography variant="caption" color="text.secondary" sx={{ mt: -1 }}>
              {t.runtimeSpecRamEstimateHint}
            </Typography>
            <Typography variant="body2" color="text.secondary">
              {t.runtimeSpecRamMeasured}: {ramMeasuredMb} MB
            </Typography>

            <Box sx={{ display: 'grid', gap: 1 }}>
              <Typography variant="subtitle2" component="h3">
                {t.runtimeSpecGpus}
              </Typography>
              {showAgentTooOldArgs && (
                <Alert severity="warning">
                  {`${t.runtimeSpecAgentTooOldArgs} (${t.runtimeAgentVersion}: ${agentVersion || '—'})`}
                </Alert>
              )}
              {showAgentTooOldOrder && (
                <Alert severity="info">
                  {`${t.runtimeSpecAgentTooOldOrder} (${t.runtimeAgentVersion}: ${agentVersion || '—'})`}
                </Alert>
              )}
              {showMetalNonMacos && <Alert severity="warning">{t.runtimeSpecMetalNonMacos}</Alert>}
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
                  {...gpuDrag.dragProps(row.rowKey)}
                  sx={{
                    display: 'flex',
                    gap: 1.5,
                    alignItems: 'center',
                    flexWrap: 'wrap',
                    ...columnDragSx(
                      row.rowKey,
                      gpuDrag.draggingId,
                      gpuDrag.overId,
                      gpuDrag.overPlace,
                      'vertical',
                    ),
                  }}
                >
                  <DragIndicatorIcon
                    fontSize="small"
                    sx={{ color: 'text.secondary', cursor: 'grab' }}
                    aria-hidden
                  />
                  <IconButton
                    size="small"
                    aria-label={`${t.modelGroupMoveUp}: GPU ${row.index}`}
                    disabled={idx === 0}
                    onClick={() => swapGpuRow(idx, -1)}
                  >
                    <ArrowUpwardIcon fontSize="small" />
                  </IconButton>
                  <IconButton
                    size="small"
                    aria-label={`${t.modelGroupMoveDown}: GPU ${row.index}`}
                    disabled={idx === gpuRows.length - 1}
                    onClick={() => swapGpuRow(idx, 1)}
                  >
                    <ArrowDownwardIcon fontSize="small" />
                  </IconButton>
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
                    {executionMode === 'wsl' &&
                      row.vramMeasuredMb > 0 &&
                      ` (${t.runtimeStatusVRAMFromVM})`}
                  </Typography>
                  {/* THE THIRD NUMBER, and the only one this form merely
                      OFFERS. Rendered read-only through `Field`'s own prop
                      (which also strips the editable chrome) so it can never be
                      mistaken for the field to the left: that one is the
                      operator's estimate, the line above is the agent's
                      measurement, and this is the benchmark's. Three numbers,
                      three meanings, one owner each.

                      Present only when this GPU index actually carries a
                      positive number in the newest applicable run. A zero is
                      UNKNOWN throughout this feature, so an unmeasured card
                      gets no affordance rather than an Apply that would write
                      "this model is free" into the admission arithmetic. */}
                  {renderVramApply(row, idx)}
                  <Button
                    type="button"
                    size="small"
                    color="secondary"
                    onClick={() => removeGpuRow(idx)}
                  >
                    {t.runtimeSpecGpuRemove}
                  </Button>
                  {/* Last in the row, on its own full-width line: what the
                      card-fingerprint comparison established for the number
                      above. */}
                  {renderVramCardCheck(row)}
                </Box>
              ))}
              {vramEvidence && (
                <Box>
                  <Typography variant="caption" color="text.secondary" component="p">
                    {t.runtimeSpecVramBenchmarkFrom}:{' '}
                    {new Date(vramEvidence.run.created_at).toLocaleString()}
                  </Typography>
                  <Typography variant="caption" color="text.secondary" component="p">
                    {t.runtimeSpecVramApplyHint}
                  </Typography>
                </Box>
              )}
              <Box>
                <Button type="button" variant="outlined" size="small" onClick={addGpuRow}>
                  {t.runtimeSpecGpuAdd}
                </Button>
              </Box>
            </Box>

            {/* Shown for llama.cpp (explicit, or Auto resolving to it, a new
                spec included), and for any spec that is still a head so the
                operator can turn the cluster off after a type change. */}
            {(formSpecIsLlamaCpp(specType, binary, specLoadedType) || cluster.enabled) && (
              <ClusterHeadEditor
                t={t}
                api={api}
                mappingId={editingSpecMappingId}
                applicationId={application.id}
                headServer={server}
                stored={currentSpec}
                value={cluster}
                onChange={setCluster}
                args={parseArgsText(argsText)}
                env={parseEnvText(envText, t).env}
                headHasGpus={gpuRows.length > 0}
                startupTimeoutSeconds={startupTimeoutSeconds}
                headBinary={binary}
                headBinaryVersion={binaryVersionFor(serverBinaries, binary.trim())}
                resetMemberUsage={
                  editingSpecMappingId && currentSpec?.configured
                    ? (serverId) => resetClusterMemberUsage(editingSpecMappingId, serverId)
                    : undefined
                }
              />
            )}

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

  // The co-residency tab's body, one state at a time. Each early return is a
  // distinct fact about what the tab may show, in the order they gate one
  // another; none of them is the editor with an empty document.
  function renderCoresidencyBody() {
    if (fileMode) {
      // The agent owns its own config file, which is where its co-residency
      // is kept: there is nothing here to edit, and the gateway-side mode
      // and verdicts the live editor writes are not what is in force.
      return <Typography color="text.secondary">{t.coresidencyFileMode}</Typography>;
    }
    if (reportStatus !== 'ready') {
      // Deliberately NOT the editor with an empty document: the report
      // GET (which decides whether this screen is writable at all) has
      // not settled, so there is nothing to edit from. A FAILED report
      // GET is a different fact from a pending one and must not read as
      // "loading" -- the screen-wide banner above carries the
      // explanation and the retry.
      return (
        <ResourceFallback
          state={reportFailed ? reportStatus : 'loading'}
          loadingLabel={t.loading}
          errorLabel={t.runtimeModeUnknownShort}
        />
      );
    }
    if (!coresidencyReady) {
      // The pairs GET itself (task-21 review's Critical finding), now
      // with its failures named instead of shown as an endless
      // "loading…": on a hard failure there is nothing to render, on a
      // failed reload there is a payload we could not refresh and an
      // edit would PUT the full replacement document computed from it.
      // Neither may be edited, and both get the retry this tab never
      // had -- the resource's `reload()` was not even destructured.
      //
      // Both states hide the editor, but they are not the same sentence
      // (fix round 1, M7): "could not be loaded" is false once a payload
      // is in hand and only the refresh failed.
      return (
        <ResourceFallback
          state={coresidencyStatus}
          loadingLabel={t.loading}
          errorLabel={t.runtimeCoresidencyUnavailable}
          staleErrorLabel={t.runtimeCoresidencyStale}
          errorDetail={coresidencyError}
          retry={{ label: t.resourceRetry, onRetry: () => void reloadCoresidency() }}
        />
      );
    }
    if (coresidencyRolesState !== 'ready' || coresidencyData === null) {
      // The document is in hand but the roles it is grouped by are not
      // (see `coresidencyItems`): nothing is offered until every spec GET
      // has settled.
      return (
        <ResourceFallback
          state={coresidencyRolesState}
          loadingLabel={t.loading}
          errorLabel={t.runtimeMappingsUnavailable}
        />
      );
    }
    return (
      <CoResidencyEditor
        t={t}
        items={coresidencyItems}
        mode={coresidencyData.mode}
        pairs={coresidencyData.pairs}
        onChange={(next) => void saveCoresidency(next)}
        disabled={coresidencyBusy}
        // Transient -- but for the few hundred milliseconds it lasts, an
        // unexplained dead editor is the same defect as an unexplained
        // read-only one. Undefined when not busy, so a live editor never
        // carries a reason it does not have.
        disabledReason={coresidencyBusy ? t.coresidencySaving : undefined}
      />
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
              (the list itself, the co-residency model list, and the status table's
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
        <Tab label={t.runtimeCoresidency} value="coresidency" />
        {/* File mode ignores gateway-side documents, so member specs written here would never apply. */}
        {!fileMode && <Tab label={t.rpcTab} value="rpc" />}
        <Tab label={t.binariesTab} value="binaries" />
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
            // Shares the ordinary mapping screen's saved layout on purpose --
            // see MAPPING_TABLE_STORAGE_KEY. The operator wants one config for
            // both tables (column order and which columns show), not two.
            storageKey={MAPPING_TABLE_STORAGE_KEY}
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
      {tab === 'coresidency' && (
        <Panel
          titleId="runtime-coresidency-heading"
          title={t.runtimeCoresidency}
          subtitle={t.runtimeCoresidencyHint}
        >
          {renderCoresidencyBody()}
        </Panel>
      )}
      {tab === 'rpc' && !fileMode && (
        <RpcMemberSection
          t={t}
          api={api}
          server={rpcSavedServer ?? server}
          statusRows={statusRows}
          onServerSaved={(saved) => setRpcSaved({ server: saved, propKey: rpcContentKey })}
        />
      )}
      {tab === 'binaries' && <BinariesSection t={t} api={api} serverId={server.id} />}
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
              // A spec's instances share its spec_id, so the id alone would
              // fold them into one row.
              rowKey={(row) => `${row.spec_id}#${instanceIndex(row)}`}
              actions={statusActions}
              // The primary row of a replicable spec carries five: Logs,
              // Start, Stop, Pin and Restart. Kept inline, one click each.
              maxInlineActions={5}
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
        open={clusterStop !== null}
        title={
          clusterStop
            ? t.runtimeStopClusterTitle(
                instanceTargetName(
                  clusterStop.row,
                  clusterStop.spec,
                  instanceIndex(clusterStop.row),
                ),
              )
            : ''
        }
        body={t.runtimeStopClusterBody}
        extra={
          <FormControlLabel
            control={
              <Checkbox
                checked={stopMembersToo}
                onChange={(e) => setStopMembersToo(e.target.checked)}
              />
            }
            label={t.runtimeStopMembersToo}
          />
        }
        confirmLabel={t.runtimeStop}
        cancelLabel={t.cancel}
        onConfirm={() => {
          const pending = clusterStop;
          setClusterStop(null);
          if (pending) stopRuntimeInstance(pending.row, pending.spec, stopMembersToo);
        }}
        onCancel={() => setClusterStop(null)}
      />
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
          allInstances={logSpec.allInstances}
          instances={logSpec.instances}
          initialTab={logSpec.initialTab}
        />
      )}
    </>
  );
}
