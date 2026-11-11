// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// The llama.cpp RPC cluster part of a launch spec: whether the spec is a
// cluster head, its members (in --rpc order), the member mode, network and
// transport, and the hints the head's args need. The launch spec form owns
// the state and sends it with every save: the spec PUT is a full document,
// and a save without the cluster fields would turn the cluster off and clear
// its membership.

import { useEffect, useRef, useState } from 'react';
import {
  Alert,
  Box,
  Button,
  Checkbox,
  FormControlLabel,
  IconButton,
  Typography,
} from '@mui/material';
import AddIcon from '@mui/icons-material/Add';
import ArrowDownwardIcon from '@mui/icons-material/ArrowDownward';
import ArrowUpwardIcon from '@mui/icons-material/ArrowUpward';
import CloseIcon from '@mui/icons-material/Close';
import RestartAltIcon from '@mui/icons-material/RestartAlt';
import type {
  ClusterCandidate,
  ClusterMember,
  ClusterMemberGPU,
  ClusterMemberMode,
  ClusterNetwork,
  DeviceBackend,
  HardwareGPU,
  PortalServer,
  PutRuntimeSpecRequest,
  RPCMemberSpec,
  RpcTransport,
  RuntimeSpec,
} from '../api';
import type { MessageKey, PortalApi, Translation } from './shared/types';
import { Field } from './shared/Field';
import { ClusterProgress } from './ClusterProgress';
import { ClusterWithheldNotice } from './ClusterTotalCell';
import {
  clusterCellText,
  clusterTotalsView,
  clusterWithheldReason,
  type ClusterTotalsView,
} from './shared/clusterTotals';
import { SelectField } from './shared/SelectField';
import { ConfirmDialog } from './shared/ConfirmDialog';
import { clusterWarningLabelByCode, formatPortalError } from './shared/format';
import {
  clusterHeadDocumentError,
  deviceListStartsWithRPC,
  hasDeviceList,
  scanClusterArgs,
  type ClusterArgScan,
} from './shared/clusterArgs';

const deviceBackends: DeviceBackend[] = ['auto', 'cuda', 'rocm', 'vulkan', 'metal', 'sycl'];

// A member as the form holds it: the wire entry plus a render identity.
export type ClusterMemberRow = ClusterMember & { rowKey: string };

export interface ClusterHeadState {
  enabled: boolean;
  memberMode: ClusterMemberMode;
  // '' = the gateway's default, resolved on save.
  network: ClusterNetwork | '';
  transport: RpcTransport | '';
  lanAck: boolean;
  // Every spec has one (the generic device placeholders render with it), so
  // it rides here to be round-tripped even when the cluster is off.
  deviceBackend: DeviceBackend;
  members: ClusterMemberRow[];
}

let rowKeySeq = 0;
const nextRowKey = () => `cm-${++rowKeySeq}`;

export function emptyClusterState(): ClusterHeadState {
  return {
    enabled: false,
    memberMode: 'automatic',
    network: '',
    transport: '',
    lanAck: false,
    deviceBackend: 'auto',
    members: [],
  };
}

// The form state of a loaded spec. The cluster fields are optional on the
// type (fixtures from before them), and a missing one reads as the backend's
// own zero value.
export function clusterStateFromSpec(spec: RuntimeSpec): ClusterHeadState {
  return {
    enabled: spec.cluster_enabled ?? false,
    memberMode: spec.member_mode || 'automatic',
    network: spec.cluster_network ?? '',
    transport: spec.rpc_transport ?? '',
    lanAck: spec.lan_exposure_ack ?? false,
    deviceBackend: spec.device_backend || 'auto',
    members: (spec.cluster_members ?? []).map((m) => ({ ...m, rowKey: nextRowKey() })),
  };
}

// The cluster part of the PUT body. A member the caller cannot see goes back
// exactly as it was read: the gateway keeps the stored member at that
// position for an entry without a server id. Visible members take their list
// position.
export function clusterBodyFields(
  s: ClusterHeadState,
): Pick<
  PutRuntimeSpecRequest,
  | 'cluster_enabled'
  | 'member_mode'
  | 'cluster_network'
  | 'rpc_transport'
  | 'lan_exposure_ack'
  | 'device_backend'
  | 'cluster_members'
> {
  return {
    cluster_enabled: s.enabled,
    member_mode: s.memberMode,
    cluster_network: s.network,
    rpc_transport: s.transport,
    lan_exposure_ack: s.lanAck,
    device_backend: s.deviceBackend,
    // A cluster that is off has no members; the form keeps them only so the
    // switch can be undone before the save.
    cluster_members: s.enabled
      ? s.members.map(({ rowKey: _rowKey, ...m }, i) => (m.visible ? { ...m, position: i } : m))
      : [],
  };
}

// The stored usage of the member on serverId as the head reads after a
// reset, put into the form: only the read-only measurements change (each
// GPU's measured VRAM by host index, the measured RAM, when and whether it is
// stale), so the operator's unsaved edits to the member stay.
export function mergeMemberUsage(
  s: ClusterHeadState,
  spec: RuntimeSpec,
  serverId: string,
): ClusterHeadState {
  const fresh = (spec.cluster_members ?? []).find((m) => m.visible && m.server_id === serverId);
  if (!fresh) return s;
  const vramByIndex = new Map((fresh.gpus ?? []).map((g) => [g.index, g.vram_measured_mb]));
  return {
    ...s,
    members: s.members.map((row) =>
      row.visible && row.server_id === serverId
        ? {
            ...row,
            ram_measured_mb: fresh.ram_measured_mb,
            measured_at: fresh.measured_at,
            stale: fresh.stale,
            gpus: row.gpus.map((g) => ({ ...g, vram_measured_mb: vramByIndex.get(g.index) ?? 0 })),
          }
        : row,
    ),
  };
}

// Whether a stored member carries usage a reset would clear.
function hasStoredUsage(m: ClusterMember | undefined): boolean {
  if (!m) return false;
  return (
    Boolean(m.measured_at) ||
    m.ram_measured_mb > 0 ||
    (m.gpus ?? []).some((g) => g.vram_measured_mb > 0)
  );
}

// What an empty network resolves to, as far as the loaded spec tells.
function effectiveNetwork(s: ClusterHeadState, stored?: RuntimeSpec): ClusterNetwork | '' {
  return s.network || stored?.cluster_network_default || '';
}

// The transport default for network: the read view's when it was derived
// for this network, direct on a LAN, else unknown (the mesh default turns
// on the gateway's NetBird policy settings).
function transportDefault(network: ClusterNetwork | '', stored?: RuntimeSpec): RpcTransport | '' {
  const storedNetwork = stored?.cluster_network || stored?.cluster_network_default || '';
  if (network !== '' && network === storedNetwork && stored?.cluster_transport_default) {
    return stored.cluster_transport_default;
  }
  return network === 'lan' ? 'direct' : '';
}

function effectiveTransport(s: ClusterHeadState, stored?: RuntimeSpec): RpcTransport | '' {
  return s.transport || transportDefault(effectiveNetwork(s, stored), stored);
}

function needsLanAck(s: ClusterHeadState, stored?: RuntimeSpec): boolean {
  return effectiveNetwork(s, stored) === 'lan' && effectiveTransport(s, stored) === 'direct';
}

export interface ClusterSaveFacts {
  args: string[];
  env: Record<string, string>;
  type: RuntimeSpec['type'];
  replicable: boolean;
  startupTimeoutSeconds: number;
  // The head binary as the form will send it.
  binary: string;
  stored?: RuntimeSpec;
}

// The first cluster refusal the gateway would answer for this save, as far
// as the form can tell, in the gateway's order; null when none. The checks
// that read other servers (rights, consent, binaries, NetBird) stay the
// gateway's.
export function clusterSaveError(s: ClusterHeadState, facts: ClusterSaveFacts): MessageKey | null {
  const scan = scanClusterArgs(facts.args, facts.env);
  if (!s.enabled) {
    return scan.clusterPlaceholder ? 'errorClusterRpcPlaceholderWithoutCluster' : null;
  }
  const docError = clusterHeadDocumentError(scan, {
    type: facts.type,
    memberCount: s.members.length,
    replicable: facts.replicable,
    startupTimeoutSeconds: facts.startupTimeoutSeconds,
  });
  if (docError) return docError;
  const servers = s.members.filter((m) => m.visible).map((m) => m.server_id);
  if (new Set(servers).size !== servers.length) return 'errorClusterMemberServerTwice';
  if (needsLanAck(s, facts.stored) && !s.lanAck) return 'errorClusterLanAckRequired';
  // The gateway lets a save through a member the caller cannot see only
  // while the membership and the head binary stay as stored: the binary
  // decides which member builds are compatible, so switching it runs every
  // member check, and the hidden member's server then answers not found.
  if (
    s.members.some((m) => !m.visible) &&
    facts.stored?.configured &&
    facts.binary.trim() !== facts.stored.binary.trim()
  ) {
    return 'errorClusterHeadBinaryLocked';
  }
  return null;
}

type HostGpus = HardwareGPU[] | 'unavailable' | 'failed';

// A manual member spec as the picker offers it: id, devices and binary.
function memberSpecLabel(s: RPCMemberSpec, t: Translation): string {
  const devices =
    s.gpus.length === 0 ? t.rpcMemberCpu : s.gpus.map((g) => `GPU ${g.index}`).join(', ');
  return `${s.id} · ${devices} · ${s.binary}`;
}

function toCount(text: string): number {
  const n = Number.parseInt(text, 10);
  return Number.isFinite(n) && n > 0 ? n : 0;
}

function gpuLabel(gpu: HardwareGPU): string {
  const name = gpu.name.trim();
  return name ? `GPU ${gpu.index} · ${name}` : `GPU ${gpu.index}`;
}

function swap<T>(items: T[], index: number, delta: number): T[] {
  const target = index + delta;
  if (target < 0 || target >= items.length) return items;
  const next = [...items];
  [next[index], next[target]] = [next[target], next[index]];
  return next;
}

// Per automatic member server: its reported GPUs, 'unavailable' without a
// hardware report, 'failed' when the read failed. Each server is read once.
function useMemberHardware(
  api: Pick<PortalApi, 'serverHardware'>,
  value: ClusterHeadState,
): Record<string, HostGpus> {
  const [hardwareByServer, setHardwareByServer] = useState<Record<string, HostGpus>>({});
  const automaticServers =
    value.memberMode === 'automatic'
      ? value.members.filter((m) => m.visible).map((m) => m.server_id)
      : [];
  const missingHardware = automaticServers.filter((id) => !(id in hardwareByServer));
  const missingKey = missingHardware.join(',');

  useEffect(() => {
    if (missingKey === '') return;
    let cancelled = false;
    for (const id of missingKey.split(',')) {
      api
        .serverHardware(id)
        .then((hw) => {
          if (cancelled) return;
          const gpus: HostGpus = hw.available && hw.report ? hw.report.gpus : 'unavailable';
          setHardwareByServer((cur) => ({ ...cur, [id]: gpus }));
        })
        .catch(() => {
          if (!cancelled) setHardwareByServer((cur) => ({ ...cur, [id]: 'failed' }));
        });
    }
    return () => {
      cancelled = true;
    };
  }, [api, missingKey]);
  return hardwareByServer;
}

// The text of the network select's default option: the default the gateway
// derived, or a neutral entry when the spec was not loaded with one.
function networkDefaultLabel(networkDefault: string, t: Translation): string {
  if (!networkDefault) return t.clusterDefaultOptionUnknown;
  return t.clusterDefaultOption(
    networkDefault === 'mesh' ? t.clusterNetworkMesh : t.clusterNetworkLan,
  );
}

// Why the network default is what it is.
function networkDefaultReason(networkDefault: string, t: Translation): string {
  if (networkDefault === 'mesh') return t.clusterNetworkReasonMesh;
  if (networkDefault === 'lan') return t.clusterNetworkReasonLan;
  return t.clusterNetworkReasonUnknown;
}

// The text of the transport select's default option.
function transportDefaultLabel(transportDefaultValue: RpcTransport | '', t: Translation): string {
  if (!transportDefaultValue) return t.clusterDefaultOptionUnknown;
  return t.clusterDefaultOption(
    transportDefaultValue === 'direct' ? t.clusterTransportDirect : t.clusterTransportRelay,
  );
}

// Why the transport default is what it is on the effective network.
function transportDefaultReason(
  network: ClusterNetwork | '',
  transportDefaultValue: RpcTransport | '',
  t: Translation,
): string {
  if (network === 'lan') return t.clusterTransportReasonLan;
  if (network === 'mesh' && transportDefaultValue === 'direct') {
    return t.clusterTransportReasonMeshDirect;
  }
  if (network === 'mesh' && transportDefaultValue === 'relay') {
    return t.clusterTransportReasonMeshRelay;
  }
  return t.clusterTransportReasonUnknown;
}

// The member spec a manual member is added with: the only free one of the
// chosen server, else the one picked. Automatic members have none.
function pickedMemberSpecId(
  memberMode: ClusterMemberMode,
  specs: RPCMemberSpec[],
  pickedSpecId: string,
): string {
  if (memberMode !== 'manual') return '';
  if (specs.length === 1) return specs[0].id;
  return pickedSpecId;
}

// Why a member server offers no GPUs to pick.
function hostGpusNote(hostGpus: HostGpus, t: Translation): string {
  if (hostGpus === 'failed') return t.clusterMemberHardwareFailed;
  if (hostGpus === 'unavailable') return t.clusterMemberHardwareUnavailable;
  return t.clusterMemberNoGpusReported;
}

// The hints about the head's own args: the missing ${RPC_SERVERS}, the
// gateway's advisory warnings, the cold start and the device list order.
function ClusterHeadHints({
  t,
  scan,
  warnings,
  headHasGpus,
  startupTimeoutSeconds,
}: Readonly<{
  t: Translation;
  scan: ClusterArgScan;
  warnings: string[];
  headHasGpus: boolean;
  startupTimeoutSeconds: number;
}>) {
  const deviceListGiven = hasDeviceList(scan);
  const rpcFirst = deviceListGiven && deviceListStartsWithRPC(scan, headHasGpus);
  return (
    <>
      {scan.rpcPlaceholderCount === 0 && <Alert severity="warning">{t.clusterRpcHint}</Alert>}
      {warnings.length > 0 && (
        <Alert severity="warning">
          <Typography variant="subtitle2" component="h4">
            {t.clusterWarnings}
          </Typography>
          <Box component="ul" sx={{ m: 0, pl: 2 }}>
            {warnings.map((code) => {
              const key = clusterWarningLabelByCode[code];
              return <li key={code}>{key ? t[key] : code}</li>;
            })}
          </Box>
        </Alert>
      )}
      <Typography variant="body2" color="text.secondary">
        {t.clusterColdStart(startupTimeoutSeconds)}
      </Typography>
      {deviceListGiven && !rpcFirst && (
        <Typography variant="body2" color="text.secondary">
          {t.clusterDeviceOrderHint}
        </Typography>
      )}
      {rpcFirst && !scan.mmdev && (
        <Typography variant="body2" color="text.secondary">
          {t.clusterMmdevHint}
        </Typography>
      )}
      {!deviceListGiven && (
        <Typography variant="body2" color="text.secondary">
          {t.clusterNoDeviceListHint}
        </Typography>
      )}
    </>
  );
}

// The LAN exposure acknowledgement of a direct LAN cluster: given (with a
// way to withdraw it) or missing (with a way to review and give it).
function LanAckNotice({
  t,
  given,
  onWithdraw,
  onReview,
}: Readonly<{ t: Translation; given: boolean; onWithdraw: () => void; onReview: () => void }>) {
  if (given) {
    return (
      <Alert
        severity="info"
        action={
          <Button color="inherit" size="small" onClick={onWithdraw}>
            {t.clusterLanAckWithdraw}
          </Button>
        }
      >
        {t.clusterLanAckGiven}
      </Alert>
    );
  }
  return (
    <Alert
      severity="warning"
      action={
        <Button color="inherit" size="small" onClick={onReview}>
          {t.clusterLanAckReview}
        </Button>
      }
    >
      {t.clusterLanAckMissing}
    </Alert>
  );
}

// A member the caller may not see: its place in the order, nothing else.
function HiddenMemberRow({ t, index }: Readonly<{ t: Translation; index: number }>) {
  return (
    <Box
      component="li"
      data-testid="cluster-member-row"
      sx={{ display: 'flex', gap: 1.5, alignItems: 'center' }}
    >
      <Typography component="span" sx={{ minWidth: 24 }}>
        {`${index + 1}.`}
      </Typography>
      <Typography component="span" color="text.secondary" sx={{ fontStyle: 'italic' }}>
        {t.clusterMemberHidden}
      </Typography>
    </Box>
  );
}

// The stored cluster totals, marked as a lower bound when an axis is.
function ClusterTotalsLine({ t, totals }: Readonly<{ t: Translation; totals: ClusterTotalsView }>) {
  const vram = clusterCellText(totals.vram.mb, totals.vram.complete);
  const ram = clusterCellText(totals.ram.mb, totals.ram.complete);
  const lowerBound = !totals.vram.complete || !totals.ram.complete;
  return (
    <Typography variant="body2" data-testid="cluster-totals">
      {`${t.clusterTotals}: VRAM ${vram}, RAM ${ram}`}
      {lowerBound && ` (${t.clusterTotalsLowerBound})`}
    </Typography>
  );
}

type PickerState = {
  loading: boolean;
  failed: boolean;
  candidates: ClusterCandidate[];
  serverId: string;
  specId: string;
};

// The open member picker: the servers on offer, the chosen one's
// ggml-rpc-server, and in manual mode the member spec when it has several.
function MemberPicker({
  t,
  picker,
  onPickerChange,
  offered,
  chosen,
  chosenSpecs,
  memberMode,
  canAdd,
  onAdd,
  onCancel,
}: Readonly<{
  t: Translation;
  picker: PickerState;
  onPickerChange: (next: PickerState) => void;
  offered: ClusterCandidate[];
  chosen: ClusterCandidate | undefined;
  chosenSpecs: RPCMemberSpec[];
  memberMode: ClusterMemberMode;
  canAdd: boolean;
  onAdd: () => void;
  onCancel: () => void;
}>) {
  const settledEmpty = !picker.loading && !picker.failed && offered.length === 0;
  return (
    <Box sx={{ display: 'grid', gap: 1, maxWidth: 480 }}>
      {picker.failed && <Alert severity="error">{t.clusterCandidatesFailed}</Alert>}
      {settledEmpty && (
        <Alert severity="info">
          {memberMode === 'automatic'
            ? t.clusterCandidatesEmptyAutomatic
            : t.clusterCandidatesEmptyManual}
        </Alert>
      )}
      {offered.length > 0 && (
        <SelectField
          id="cluster-member-server"
          label={t.clusterMemberServer}
          value={picker.serverId}
          onChange={(e) =>
            onPickerChange({ ...picker, serverId: String(e.target.value), specId: '' })
          }
        >
          <option value="" disabled>
            {'—'}
          </option>
          {offered.map((c) => (
            <option key={c.server_id} value={c.server_id}>
              {c.server_name}
            </option>
          ))}
        </SelectField>
      )}
      {chosen?.rpc_binary && (
        <Typography variant="caption" color="text.secondary" data-testid="cluster-candidate-binary">
          {`${chosen.rpc_binary} (${chosen.rpc_binary_version || t.binariesVersionUnknown})`}
        </Typography>
      )}
      {memberMode === 'manual' && chosenSpecs.length > 1 && (
        <>
          <Typography variant="caption" color="text.secondary">
            {t.clusterMemberSpecPick}
          </Typography>
          <SelectField
            id="cluster-member-spec"
            label={t.clusterMemberSpec}
            value={picker.specId}
            onChange={(e) => onPickerChange({ ...picker, specId: String(e.target.value) })}
          >
            <option value="" disabled>
              {'—'}
            </option>
            {chosenSpecs.map((spec) => (
              <option key={spec.id} value={spec.id}>
                {memberSpecLabel(spec, t)}
              </option>
            ))}
          </SelectField>
        </>
      )}
      <Box sx={{ display: 'flex', gap: 1 }}>
        <Button variant="contained" disabled={!canAdd} onClick={onAdd}>
          {t.clusterMemberAddConfirm}
        </Button>
        <Button onClick={onCancel}>{t.cancel}</Button>
      </Box>
    </Box>
  );
}

export function ClusterHeadEditor({
  t,
  api,
  mappingId,
  headServer,
  stored,
  value,
  onChange,
  args,
  env,
  headHasGpus,
  startupTimeoutSeconds,
  headBinaryVersion = '',
  resetMemberUsage,
}: Readonly<{
  t: Translation;
  api: Pick<PortalApi, 'clusterCandidates' | 'serverHardware'>;
  // '' while the spec is being created: the candidates are read per mapping.
  mappingId: string;
  headServer: PortalServer;
  // The loaded spec: the read view's defaults, warnings and totals.
  stored?: RuntimeSpec;
  value: ClusterHeadState;
  onChange: (next: ClusterHeadState) => void;
  // The head's args and env as currently typed, for the live hints.
  args: string[];
  env: Record<string, string>;
  headHasGpus: boolean;
  startupTimeoutSeconds: number;
  // The version of the head's binary from the server's inventory ('' when
  // unknown). Members carry no binary of their own: each runs the server's
  // ggml-rpc-server matching this one.
  headBinaryVersion?: string;
  // Clears the stored usage of the member on a server and answers the head's
  // spec as it reads afterwards (the caller keeps that as the loaded spec).
  // Without it, as for a spec not saved yet, no member offers a reset.
  resetMemberUsage?: (serverId: string) => Promise<RuntimeSpec>;
}>) {
  const [pendingMode, setPendingMode] = useState<ClusterMemberMode | null>(null);
  const [pendingAck, setPendingAck] = useState<ClusterHeadState | null>(null);
  const [picker, setPicker] = useState<PickerState | null>(null);
  const [pendingReset, setPendingReset] = useState<{ serverId: string; name: string } | null>(null);
  const [resetBusy, setResetBusy] = useState(false);
  const [resetError, setResetError] = useState('');
  // A reset answers after the operator may have edited on: the merge reads
  // the form as it is then, not as it was at the click.
  const valueRef = useRef(value);
  valueRef.current = value;
  const onChangeRef = useRef(onChange);
  onChangeRef.current = onChange;
  const mountedRef = useRef(true);
  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
    };
  }, []);

  // A member on a server the caller cannot manage makes every membership
  // change a refusal (the gateway needs rights on every member server it
  // keeps), so the membership, mode, network and transport are shown, not
  // edited (the head binary is held by clusterSaveError for the same reason).
  // Turning the cluster off drops every member and needs the head's rights
  // only, so the switch stays open in that direction; turning it on again is
  // open only as the undo back to the stored head.
  const locked = value.members.some((m) => !m.visible);
  const switchLocked = locked && !value.enabled && !stored?.cluster_enabled;
  const hardwareByServer = useMemberHardware(api, value);

  const scan = scanClusterArgs(args, env);
  const network = effectiveNetwork(value, stored);
  const networkDefault = stored?.cluster_network_default || '';
  const transportDefaultValue = transportDefault(network, stored);

  function patch(next: Partial<ClusterHeadState>) {
    onChange({ ...value, ...next });
  }

  // A network or transport change that makes the cluster direct on a LAN
  // waits for the exposure confirmation; cancelling keeps the old choice.
  function applyNetworkTransport(next: Partial<ClusterHeadState>) {
    const candidate = { ...value, ...next };
    if (needsLanAck(candidate, stored) && !candidate.lanAck) setPendingAck(candidate);
    else onChange(candidate);
  }

  function changeMode(mode: ClusterMemberMode) {
    if (mode === value.memberMode) return;
    // The open picker lists the other mode's candidates.
    setPicker(null);
    if (value.members.length > 0) setPendingMode(mode);
    else patch({ memberMode: mode, members: [] });
  }

  function patchMember(rowKey: string, next: Partial<ClusterMember>) {
    patch({ members: value.members.map((m) => (m.rowKey === rowKey ? { ...m, ...next } : m)) });
  }

  async function openPicker() {
    setPicker({ loading: true, failed: false, candidates: [], serverId: '', specId: '' });
    try {
      const res = await api.clusterCandidates(mappingId, value.memberMode);
      setPicker({ loading: false, failed: false, candidates: res.data, serverId: '', specId: '' });
    } catch {
      setPicker({ loading: false, failed: true, candidates: [], serverId: '', specId: '' });
    }
  }

  const taken = new Set(value.members.map((m) => m.server_id));
  const offered = picker?.candidates.filter((c) => !taken.has(c.server_id)) ?? [];
  const chosen = offered.find((c) => c.server_id === picker?.serverId);
  const chosenSpecs = chosen?.member_specs ?? [];
  const chosenSpecId = pickedMemberSpecId(value.memberMode, chosenSpecs, picker?.specId ?? '');
  const canAdd = !!chosen && (value.memberMode === 'automatic' || chosenSpecId !== '');

  function addChosen() {
    if (!chosen || !canAdd) return;
    const row: ClusterMemberRow = {
      rowKey: nextRowKey(),
      position: value.members.length,
      server_id: chosen.server_id,
      server_name: chosen.server_name,
      member_spec_id: chosenSpecId,
      gpus: [],
      threads: 0,
      cache_enabled: false,
      device_backend: 'auto',
      ram_estimate_mb: 0,
      ram_measured_mb: 0,
      stale: false,
      port: 0,
      visible: true,
    };
    patch({ members: [...value.members, row] });
    setPicker(null);
  }

  async function confirmReset() {
    const target = pendingReset;
    setPendingReset(null);
    if (!target || !resetMemberUsage) return;
    setResetBusy(true);
    setResetError('');
    try {
      const spec = await resetMemberUsage(target.serverId);
      if (mountedRef.current) {
        onChangeRef.current(mergeMemberUsage(valueRef.current, spec, target.serverId));
      }
    } catch (e) {
      if (mountedRef.current) setResetError(formatPortalError(e, t));
    } finally {
      if (mountedRef.current) setResetBusy(false);
    }
  }

  // A reset clears a stored row, so it is offered only on a member as it is
  // stored, and only where there is usage to clear.
  const storedMembers = stored?.cluster_enabled ? (stored.cluster_members ?? []) : [];
  const resettable = (serverId: string) =>
    resetMemberUsage !== undefined &&
    mappingId !== '' &&
    hasStoredUsage(storedMembers.find((m) => m.visible && m.server_id === serverId));

  const totals =
    stored?.cluster_enabled && stored.cluster_vram_total_mb !== undefined
      ? clusterTotalsView(stored)
      : undefined;

  const warnings = stored?.cluster_enabled ? (stored.cluster_warnings ?? []) : [];

  return (
    <Box
      component="fieldset"
      aria-labelledby="cluster-head-heading"
      sx={{ border: 0, p: 0, m: 0, display: 'grid', gap: 1.5 }}
    >
      <Typography id="cluster-head-heading" variant="subtitle1" component="h3">
        {t.clusterTitle}
      </Typography>
      <ClusterWithheldNotice reason={clusterWithheldReason(stored)} t={t} />
      <ClusterProgress spec={stored} t={t} />
      <Typography variant="body2" color="text.secondary">
        {t.clusterIntro}
      </Typography>
      <SelectField
        id="cluster-device-backend"
        label={t.clusterDeviceBackend}
        helperText={t.clusterDeviceBackendHint}
        value={value.deviceBackend}
        onChange={(e) => patch({ deviceBackend: e.target.value as DeviceBackend })}
        sx={{ maxWidth: 360 }}
      >
        {deviceBackends.map((b) => (
          <option key={b} value={b}>
            {b}
          </option>
        ))}
      </SelectField>
      <FormControlLabel
        control={
          <Checkbox
            checked={value.enabled}
            disabled={switchLocked || (mappingId === '' && !value.enabled)}
            onChange={(e) => patch({ enabled: e.target.checked })}
          />
        }
        label={t.clusterEnabledLabel}
      />
      {mappingId === '' && !value.enabled && (
        <Typography variant="caption" color="text.secondary">
          {t.clusterCreateFirst}
        </Typography>
      )}
      {locked && <Alert severity="info">{t.clusterMembersLocked}</Alert>}
      {value.enabled && (
        <>
          <ClusterHeadHints
            t={t}
            scan={scan}
            warnings={warnings}
            headHasGpus={headHasGpus}
            startupTimeoutSeconds={startupTimeoutSeconds}
          />

          <SelectField
            id="cluster-member-mode"
            label={t.clusterMemberMode}
            value={value.memberMode}
            disabled={locked}
            onChange={(e) => changeMode(e.target.value as ClusterMemberMode)}
            sx={{ maxWidth: 480 }}
          >
            <option value="automatic">{t.clusterMemberModeAutomatic}</option>
            <option value="manual">{t.clusterMemberModeManual}</option>
          </SelectField>

          <SelectField
            id="cluster-network"
            label={t.clusterNetwork}
            value={value.network}
            disabled={locked}
            onChange={(e) =>
              applyNetworkTransport({ network: e.target.value as ClusterNetwork | '' })
            }
            sx={{ maxWidth: 480 }}
          >
            <option value="">{networkDefaultLabel(networkDefault, t)}</option>
            <option value="mesh">{t.clusterNetworkMesh}</option>
            <option value="lan">{t.clusterNetworkLan}</option>
          </SelectField>
          <Typography variant="caption" color="text.secondary">
            {networkDefaultReason(networkDefault, t)}
          </Typography>
          {network === 'lan' && (
            <Typography variant="body2" color="text.secondary">
              {t.clusterLanAddressHint}
            </Typography>
          )}
          {network === 'lan' && headServer.netbird_enabled && !headServer.lan_address && (
            <Alert severity="warning">{t.clusterLanAddressHeadMissing}</Alert>
          )}

          <SelectField
            id="cluster-transport"
            label={t.clusterTransport}
            value={value.transport}
            disabled={locked}
            onChange={(e) =>
              applyNetworkTransport({ transport: e.target.value as RpcTransport | '' })
            }
            sx={{ maxWidth: 480 }}
          >
            <option value="">{transportDefaultLabel(transportDefaultValue, t)}</option>
            <option value="direct">{t.clusterTransportDirect}</option>
            <option value="relay">{t.clusterTransportRelay}</option>
          </SelectField>
          <Typography variant="caption" color="text.secondary">
            {transportDefaultReason(network, transportDefaultValue, t)}
          </Typography>
          <Typography variant="body2" color="text.secondary">
            {t.clusterTransportExplain}
          </Typography>
          {needsLanAck(value, stored) && (
            <LanAckNotice
              t={t}
              given={value.lanAck}
              onWithdraw={() => patch({ lanAck: false })}
              onReview={() => setPendingAck(value)}
            />
          )}

          <Typography variant="subtitle2" component="h4">
            {t.clusterMembers}
          </Typography>
          <Typography variant="caption" color="text.secondary">
            {t.clusterMembersHint}
          </Typography>
          {value.members.length === 0 && (
            <Typography color="text.secondary">{t.clusterMembersEmpty}</Typography>
          )}
          <Box component="ol" sx={{ listStyle: 'none', m: 0, p: 0, display: 'grid', gap: 1.5 }}>
            {value.members.map((m, i) =>
              m.visible ? (
                <MemberRow
                  key={m.rowKey}
                  t={t}
                  member={m}
                  index={i}
                  count={value.members.length}
                  locked={locked}
                  automatic={value.memberMode === 'automatic'}
                  hostGpus={hardwareByServer[m.server_id]}
                  onMove={(delta) => patch({ members: swap(value.members, i, delta) })}
                  onRemove={() =>
                    patch({ members: value.members.filter((x) => x.rowKey !== m.rowKey) })
                  }
                  onPatch={(next) => patchMember(m.rowKey, next)}
                  onResetUsage={
                    resettable(m.server_id)
                      ? () =>
                          setPendingReset({
                            serverId: m.server_id,
                            name: m.server_name || m.server_id,
                          })
                      : undefined
                  }
                  resetBusy={resetBusy}
                />
              ) : (
                <HiddenMemberRow key={m.rowKey} t={t} index={i} />
              ),
            )}
          </Box>
          {value.members.length > 0 && (
            <Typography
              variant="caption"
              color="text.secondary"
              data-testid="cluster-members-binary"
            >
              {t.clusterMembersMatchingBinary(headBinaryVersion || t.binariesVersionUnknown)}
            </Typography>
          )}
          {resetError && (
            <Alert severity="error" data-testid="cluster-reset-error">
              {resetError}
            </Alert>
          )}
          {totals && <ClusterTotalsLine t={t} totals={totals} />}

          {picker === null ? (
            <Box>
              <Button
                startIcon={<AddIcon />}
                disabled={locked || mappingId === ''}
                onClick={() => void openPicker()}
              >
                {t.clusterMemberAdd}
              </Button>
            </Box>
          ) : (
            <MemberPicker
              t={t}
              picker={picker}
              onPickerChange={setPicker}
              offered={offered}
              chosen={chosen}
              chosenSpecs={chosenSpecs}
              memberMode={value.memberMode}
              canAdd={canAdd}
              onAdd={addChosen}
              onCancel={() => setPicker(null)}
            />
          )}
        </>
      )}

      <ConfirmDialog
        open={pendingMode !== null}
        title={t.clusterModeSwitchTitle}
        body={t.clusterModeSwitchBody}
        confirmLabel={t.clusterModeSwitchConfirm}
        cancelLabel={t.cancel}
        onConfirm={() => {
          if (pendingMode) patch({ memberMode: pendingMode, members: [] });
          setPicker(null);
          setPendingMode(null);
        }}
        onCancel={() => setPendingMode(null)}
      />
      <ConfirmDialog
        open={pendingAck !== null}
        title={t.clusterLanAckTitle}
        body={t.clusterLanAckBody}
        confirmLabel={t.clusterLanAckConfirm}
        cancelLabel={t.cancel}
        onConfirm={() => {
          if (pendingAck) onChange({ ...pendingAck, lanAck: true });
          setPendingAck(null);
        }}
        onCancel={() => setPendingAck(null)}
      />
      <ConfirmDialog
        open={pendingReset !== null}
        title={t.clusterMemberResetUsageTitle(pendingReset?.name ?? '')}
        body={t.clusterMemberResetUsageBody}
        confirmLabel={t.clusterMemberResetUsageConfirm}
        cancelLabel={t.cancel}
        onConfirm={() => void confirmReset()}
        onCancel={() => setPendingReset(null)}
      />
    </Box>
  );
}

function MemberRow({
  t,
  member,
  index,
  count,
  locked,
  automatic,
  hostGpus,
  onMove,
  onRemove,
  onPatch,
  onResetUsage,
  resetBusy = false,
}: Readonly<{
  t: Translation;
  member: ClusterMemberRow;
  index: number;
  count: number;
  locked: boolean;
  automatic: boolean;
  // undefined while the server's hardware report loads.
  hostGpus?: HostGpus;
  onMove: (delta: number) => void;
  onRemove: () => void;
  onPatch: (next: Partial<ClusterMember>) => void;
  // Undefined when the member has no stored usage to reset.
  onResetUsage?: () => void;
  resetBusy?: boolean;
}>) {
  const name = member.server_name || member.server_id;
  const selected = new Set(member.gpus.map((g) => g.index));
  const addable = (Array.isArray(hostGpus) ? hostGpus : []).filter((g) => !selected.has(g.index));

  function setGpus(gpus: ClusterMemberGPU[]) {
    onPatch({ gpus });
  }

  return (
    <Box
      component="li"
      data-testid="cluster-member-row"
      sx={{ display: 'grid', gap: 1, borderLeft: 2, borderColor: 'divider', pl: 1.5 }}
    >
      <Box sx={{ display: 'flex', gap: 1, alignItems: 'center', flexWrap: 'wrap' }}>
        <Typography component="span" sx={{ minWidth: 24 }}>
          {`${index + 1}.`}
        </Typography>
        <Typography component="span" sx={{ fontWeight: 600 }}>
          {name}
        </Typography>
        {member.member_spec_id && (
          <Typography component="span" variant="body2" sx={{ fontFamily: 'monospace' }}>
            {member.member_spec_id}
          </Typography>
        )}
        {!automatic && (
          <Typography component="span" variant="body2" color="text.secondary">
            {member.gpus.length === 0
              ? t.rpcMemberCpu
              : member.gpus.map((g) => `GPU ${g.index}`).join(', ')}
          </Typography>
        )}
        {member.port > 0 && (
          <Typography component="span" variant="body2" color="text.secondary">
            {`${t.clusterMemberPort} ${member.port}`}
          </Typography>
        )}
        <Box sx={{ ml: 'auto' }}>
          {onResetUsage && (
            <IconButton
              size="small"
              aria-label={`${t.clusterMemberResetUsage}: ${name}`}
              title={t.clusterMemberResetUsage}
              disabled={resetBusy}
              onClick={onResetUsage}
            >
              <RestartAltIcon fontSize="small" />
            </IconButton>
          )}
          <IconButton
            size="small"
            aria-label={`${t.modelGroupMoveUp}: ${name}`}
            disabled={locked || index === 0}
            onClick={() => onMove(-1)}
          >
            <ArrowUpwardIcon fontSize="small" />
          </IconButton>
          <IconButton
            size="small"
            aria-label={`${t.modelGroupMoveDown}: ${name}`}
            disabled={locked || index === count - 1}
            onClick={() => onMove(1)}
          >
            <ArrowDownwardIcon fontSize="small" />
          </IconButton>
          <IconButton
            size="small"
            aria-label={`${t.clusterMemberRemove}: ${name}`}
            disabled={locked}
            onClick={onRemove}
          >
            <CloseIcon fontSize="small" />
          </IconButton>
        </Box>
      </Box>
      {automatic && (
        <>
          <Typography variant="caption" color="text.secondary">
            {t.clusterMemberGpus}
          </Typography>
          {member.gpus.length === 0 && (
            <Typography variant="body2" color="text.secondary">
              {t.rpcMemberCpu}
            </Typography>
          )}
          {member.gpus.map((g, gi) => (
            <Box
              key={g.index}
              sx={{ display: 'flex', gap: 1, alignItems: 'center', flexWrap: 'wrap' }}
            >
              <IconButton
                size="small"
                aria-label={`${t.modelGroupMoveUp}: ${name} GPU ${g.index}`}
                disabled={locked || gi === 0}
                onClick={() => setGpus(swap(member.gpus, gi, -1))}
              >
                <ArrowUpwardIcon fontSize="small" />
              </IconButton>
              <IconButton
                size="small"
                aria-label={`${t.modelGroupMoveDown}: ${name} GPU ${g.index}`}
                disabled={locked || gi === member.gpus.length - 1}
                onClick={() => setGpus(swap(member.gpus, gi, 1))}
              >
                <ArrowDownwardIcon fontSize="small" />
              </IconButton>
              <Typography component="span" sx={{ minWidth: 64 }}>
                {`GPU ${g.index}`}
              </Typography>
              <Field
                id={`${member.rowKey}-gpu-${g.index}-vram`}
                label={t.rpcMemberGpuVram}
                inputProps={{ 'aria-label': `${name} GPU ${g.index}: ${t.rpcMemberGpuVram}` }}
                type="number"
                value={g.vram_estimate_mb ? String(g.vram_estimate_mb) : ''}
                disabled={locked}
                onChange={(e) =>
                  setGpus(
                    member.gpus.map((x) =>
                      x.index === g.index ? { ...x, vram_estimate_mb: toCount(e.target.value) } : x,
                    ),
                  )
                }
                sx={{ maxWidth: 200 }}
              />
              {g.vram_measured_mb > 0 && (
                <Typography component="span" variant="caption" color="text.secondary">
                  {`${t.clusterMemberMeasured} ${g.vram_measured_mb} MB`}
                </Typography>
              )}
              <IconButton
                size="small"
                aria-label={`${t.clusterMemberGpuRemove}: ${name} GPU ${g.index}`}
                disabled={locked}
                onClick={() => setGpus(member.gpus.filter((x) => x.index !== g.index))}
              >
                <CloseIcon fontSize="small" />
              </IconButton>
            </Box>
          ))}
          {hostGpus !== undefined && !(Array.isArray(hostGpus) && hostGpus.length > 0) && (
            <Typography variant="caption" color="text.secondary">
              {hostGpusNote(hostGpus, t)}
            </Typography>
          )}
          {addable.length > 0 && (
            <SelectField
              id={`${member.rowKey}-gpu-add`}
              label={`${t.clusterMemberGpuAdd}: ${name}`}
              value=""
              disabled={locked}
              onChange={(e) => {
                const index = Number(e.target.value);
                setGpus([...member.gpus, { index, vram_estimate_mb: 0, vram_measured_mb: 0 }]);
              }}
              sx={{ maxWidth: 360 }}
            >
              <option value="" disabled>
                {'—'}
              </option>
              {addable.map((g) => (
                <option key={g.index} value={String(g.index)}>
                  {gpuLabel(g)}
                </option>
              ))}
            </SelectField>
          )}
          <Box sx={{ display: 'flex', gap: 1.5, alignItems: 'center', flexWrap: 'wrap' }}>
            <Field
              id={`${member.rowKey}-threads`}
              label={t.clusterMemberThreads}
              inputProps={{ 'aria-label': `${name}: ${t.clusterMemberThreads}` }}
              type="number"
              value={member.threads ? String(member.threads) : ''}
              disabled={locked}
              onChange={(e) => onPatch({ threads: toCount(e.target.value) })}
              sx={{ maxWidth: 200 }}
            />
            <Field
              id={`${member.rowKey}-ram`}
              label={t.clusterMemberRamEstimate}
              inputProps={{ 'aria-label': `${name}: ${t.clusterMemberRamEstimate}` }}
              type="number"
              value={member.ram_estimate_mb ? String(member.ram_estimate_mb) : ''}
              disabled={locked}
              onChange={(e) => onPatch({ ram_estimate_mb: toCount(e.target.value) })}
              sx={{ maxWidth: 200 }}
            />
            {member.ram_measured_mb > 0 && (
              <Typography component="span" variant="caption" color="text.secondary">
                {`${t.clusterMemberMeasured} ${member.ram_measured_mb} MB`}
              </Typography>
            )}
            <SelectField
              id={`${member.rowKey}-backend`}
              label={`${name}: ${t.rpcMemberBackend}`}
              value={member.device_backend || 'auto'}
              disabled={locked}
              onChange={(e) => onPatch({ device_backend: e.target.value as DeviceBackend })}
              sx={{ maxWidth: 200 }}
            >
              {deviceBackends.map((b) => (
                <option key={b} value={b}>
                  {b}
                </option>
              ))}
            </SelectField>
            <FormControlLabel
              control={
                <Checkbox
                  checked={member.cache_enabled}
                  disabled={locked}
                  slotProps={{ input: { 'aria-label': `${name}: ${t.rpcMemberCache}` } }}
                  onChange={(e) => onPatch({ cache_enabled: e.target.checked })}
                />
              }
              label={t.rpcMemberCache}
            />
          </Box>
        </>
      )}
    </Box>
  );
}
