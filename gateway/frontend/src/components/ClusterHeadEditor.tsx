// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// The llama.cpp RPC cluster part of a launch spec: whether the spec is a
// cluster head, its members (in --rpc order), the member mode, network and
// transport, and the hints the head's args need. The launch spec form owns
// the state and sends it with every save: the spec PUT is a full document,
// and a save without the cluster fields would turn the cluster off and clear
// its membership.

import { useEffect, useState } from 'react';
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
import { SelectField } from './shared/SelectField';
import { ConfirmDialog } from './shared/ConfirmDialog';
import { clusterWarningLabelByCode } from './shared/format';
import {
  clusterHeadDocumentError,
  deviceListStartsWithRPC,
  hasDeviceList,
  scanClusterArgs,
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
    cluster_members: s.members.map(({ rowKey: _rowKey, ...m }, i) =>
      m.visible ? { ...m, position: i } : m,
    ),
  };
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
}>) {
  const [pendingMode, setPendingMode] = useState<ClusterMemberMode | null>(null);
  const [pendingAck, setPendingAck] = useState<ClusterHeadState | null>(null);
  const [picker, setPicker] = useState<{
    loading: boolean;
    failed: boolean;
    candidates: ClusterCandidate[];
    serverId: string;
    specId: string;
  } | null>(null);
  // Per member server: its reported GPUs, 'unavailable' without a hardware
  // report, 'failed' when the read failed.
  const [hardwareByServer, setHardwareByServer] = useState<Record<string, HostGpus>>({});

  // A member on a server the caller cannot manage makes every membership
  // change a refusal (the gateway needs rights on every member server), so
  // the membership, mode, network and transport are shown, not edited (the
  // head binary is held by clusterSaveError for the same reason).
  const locked = value.members.some((m) => !m.visible);
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
  const chosenSpecId =
    value.memberMode === 'manual'
      ? chosenSpecs.length === 1
        ? chosenSpecs[0].id
        : (picker?.specId ?? '')
      : '';
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

  const warnings = stored?.cluster_enabled ? (stored.cluster_warnings ?? []) : [];
  const deviceListGiven = hasDeviceList(scan);
  const rpcFirst = deviceListGiven && deviceListStartsWithRPC(scan, headHasGpus);

  return (
    <Box
      component="fieldset"
      aria-labelledby="cluster-head-heading"
      sx={{ border: 0, p: 0, m: 0, display: 'grid', gap: 1.5 }}
    >
      <Typography id="cluster-head-heading" variant="subtitle1" component="h3">
        {t.clusterTitle}
      </Typography>
      <Typography variant="body2" color="text.secondary">
        {t.clusterIntro}
      </Typography>
      <SelectField
        id="cluster-device-backend"
        label={t.clusterDeviceBackend}
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
            disabled={locked || (mappingId === '' && !value.enabled)}
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
            <option value="">
              {networkDefault
                ? t.clusterDefaultOption(
                    networkDefault === 'mesh' ? t.clusterNetworkMesh : t.clusterNetworkLan,
                  )
                : t.clusterDefaultOptionUnknown}
            </option>
            <option value="mesh">{t.clusterNetworkMesh}</option>
            <option value="lan">{t.clusterNetworkLan}</option>
          </SelectField>
          <Typography variant="caption" color="text.secondary">
            {networkDefault === 'mesh'
              ? t.clusterNetworkReasonMesh
              : networkDefault === 'lan'
                ? t.clusterNetworkReasonLan
                : t.clusterNetworkReasonUnknown}
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
            <option value="">
              {transportDefaultValue
                ? t.clusterDefaultOption(
                    transportDefaultValue === 'direct'
                      ? t.clusterTransportDirect
                      : t.clusterTransportRelay,
                  )
                : t.clusterDefaultOptionUnknown}
            </option>
            <option value="direct">{t.clusterTransportDirect}</option>
            <option value="relay">{t.clusterTransportRelay}</option>
          </SelectField>
          <Typography variant="caption" color="text.secondary">
            {network === 'lan'
              ? t.clusterTransportReasonLan
              : network === 'mesh' && transportDefaultValue === 'direct'
                ? t.clusterTransportReasonMeshDirect
                : network === 'mesh' && transportDefaultValue === 'relay'
                  ? t.clusterTransportReasonMeshRelay
                  : t.clusterTransportReasonUnknown}
          </Typography>
          <Typography variant="body2" color="text.secondary">
            {t.clusterTransportExplain}
          </Typography>
          {needsLanAck(value, stored) &&
            (value.lanAck ? (
              <Alert
                severity="info"
                action={
                  <Button color="inherit" size="small" onClick={() => patch({ lanAck: false })}>
                    {t.clusterLanAckWithdraw}
                  </Button>
                }
              >
                {t.clusterLanAckGiven}
              </Alert>
            ) : (
              <Alert
                severity="warning"
                action={
                  <Button color="inherit" size="small" onClick={() => setPendingAck(value)}>
                    {t.clusterLanAckReview}
                  </Button>
                }
              >
                {t.clusterLanAckMissing}
              </Alert>
            ))}

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
                />
              ) : (
                <Box
                  key={m.rowKey}
                  component="li"
                  data-testid="cluster-member-row"
                  sx={{ display: 'flex', gap: 1.5, alignItems: 'center' }}
                >
                  <Typography component="span" sx={{ minWidth: 24 }}>
                    {`${i + 1}.`}
                  </Typography>
                  <Typography component="span" color="text.secondary" sx={{ fontStyle: 'italic' }}>
                    {t.clusterMemberHidden}
                  </Typography>
                </Box>
              ),
            )}
          </Box>
          {stored?.cluster_enabled && stored.cluster_vram_total_mb !== undefined && (
            <Typography variant="body2">
              {`${t.clusterTotals}: VRAM ${stored.cluster_vram_total_mb} MB, RAM ${stored.cluster_ram_total_mb ?? 0} MB`}
              {stored.cluster_totals_complete === false && ` (${t.clusterTotalsLowerBound})`}
            </Typography>
          )}

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
            <Box sx={{ display: 'grid', gap: 1, maxWidth: 480 }}>
              {picker.failed && <Alert severity="error">{t.clusterCandidatesFailed}</Alert>}
              {!picker.loading && !picker.failed && offered.length === 0 && (
                <Alert severity="info">
                  {value.memberMode === 'automatic'
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
                    setPicker({ ...picker, serverId: String(e.target.value), specId: '' })
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
              {value.memberMode === 'manual' && chosenSpecs.length > 1 && (
                <>
                  <Typography variant="caption" color="text.secondary">
                    {t.clusterMemberSpecPick}
                  </Typography>
                  <SelectField
                    id="cluster-member-spec"
                    label={t.clusterMemberSpec}
                    value={picker.specId}
                    onChange={(e) => setPicker({ ...picker, specId: String(e.target.value) })}
                  >
                    <option value="" disabled>
                      {'—'}
                    </option>
                    {chosenSpecs.map((s) => (
                      <option key={s.id} value={s.id}>
                        {memberSpecLabel(s, t)}
                      </option>
                    ))}
                  </SelectField>
                </>
              )}
              <Box sx={{ display: 'flex', gap: 1 }}>
                <Button variant="contained" disabled={!canAdd} onClick={addChosen}>
                  {t.clusterMemberAddConfirm}
                </Button>
                <Button onClick={() => setPicker(null)}>{t.cancel}</Button>
              </Box>
            </Box>
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
              {hostGpus === 'failed'
                ? t.clusterMemberHardwareFailed
                : hostGpus === 'unavailable'
                  ? t.clusterMemberHardwareUnavailable
                  : t.clusterMemberNoGpusReported}
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
