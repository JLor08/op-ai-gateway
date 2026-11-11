// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// The "RPC" tab of a server's runtime admin: the server's own RPC member
// settings, the manual member specs an operator configures by hand, and the
// live state of the RPC members running on the server.

import { useEffect, useMemo, useRef, useState } from 'react';
import {
  Alert,
  Box,
  Button,
  Checkbox,
  FormControlLabel,
  IconButton,
  Tooltip,
  Typography,
} from '@mui/material';
import AddIcon from '@mui/icons-material/Add';
import ArrowDownwardIcon from '@mui/icons-material/ArrowDownward';
import ArrowUpwardIcon from '@mui/icons-material/ArrowUpward';
import DeleteIcon from '@mui/icons-material/Delete';
import EditIcon from '@mui/icons-material/Edit';
import type {
  AdminGroupCandidate,
  DeviceBackend,
  PortalServer,
  PutRPCMemberSpecRequest,
  RPCMemberSpec,
  RuntimeStatus,
  UpdateServerRequest,
} from '../api';
import type { PortalApi, Translation } from './shared/types';
import { formatPortalError } from './shared/format';
import { useResource } from './shared/useResource';
import { Panel } from './shared/Panel';
import { Field } from './shared/Field';
import { BinaryPicker, useServerBinaries } from './shared/BinaryPicker';
import { SelectField } from './shared/SelectField';
import { CheckboxGroup } from './shared/CheckboxGroup';
import { ConfirmDialog } from './shared/ConfirmDialog';
import { ListTable, listTableLabels, type ListColumn } from './shared/ListTable';
import { StatusChip } from './shared/StatusChip';
import { runtimeStateBadge, runtimeStateLabel } from './shared/runtimeState';
import { useToast } from './shared/ToastProvider';
import { formatArgsText, formatEnvText, parseArgsText, parseEnvText } from './shared/specText';

const deviceBackends: DeviceBackend[] = ['auto', 'cuda', 'rocm', 'vulkan', 'metal', 'sycl'];

// The flags the agent injects itself on a member. Mirrors the backend's
// validateRPCMemberArgs: a long flag is refused in its plain and "=" forms, a
// short flag also when its value is attached ("-p8080"), so the form says no
// before the round trip.
const reservedLongFlags = new Set(['--host', '--port', '--device', '--cache']);
const reservedShortFlags = ['-H', '-p', '-d', '-c'];

export function hasReservedMemberFlag(args: string[]): boolean {
  return args.some((raw) => {
    const a = raw.trim();
    if (a.startsWith('--')) {
      return reservedLongFlags.has(a.split('=')[0]);
    }
    return reservedShortFlags.some((f) => a.startsWith(f));
  });
}

function isAbsolutePath(p: string): boolean {
  if (p.length === 0 || p.length > 4096) return false;
  return p.startsWith('/') || /^[A-Za-z]:[\\/]/.test(p) || p.startsWith('\\\\');
}

type GpuRow = { rowKey: number; index: string; vramMb: string };

type EditorState = {
  // '' while creating.
  id: string;
  enabled: boolean;
  binary: string;
  argsText: string;
  envText: string;
  gpuRows: GpuRow[];
  cache: boolean;
  backend: DeviceBackend;
  ramMb: string;
  groups: string[];
};

let rowKeySeq = 0;
const nextRowKey = () => ++rowKeySeq;

function newEditor(): EditorState {
  return {
    id: '',
    enabled: true,
    binary: '',
    argsText: '',
    envText: '',
    gpuRows: [],
    cache: false,
    backend: 'auto',
    ramMb: '',
    groups: [],
  };
}

function editorFor(spec: RPCMemberSpec): EditorState {
  return {
    id: spec.id,
    enabled: spec.enabled,
    binary: spec.binary,
    argsText: formatArgsText(spec.args),
    envText: formatEnvText(spec.env),
    gpuRows: spec.gpus.map((g) => ({
      rowKey: nextRowKey(),
      index: String(g.index),
      vramMb: g.vram_estimate_mb ? String(g.vram_estimate_mb) : '',
    })),
    cache: spec.cache_enabled,
    backend: spec.device_backend || 'auto',
    ramMb: spec.ram_estimate_mb ? String(spec.ram_estimate_mb) : '',
    groups: spec.allowed_head_groups,
  };
}

function groupsKey(groups: string[]): string {
  return [...groups].sort((a, b) => a.localeCompare(b)).join(',');
}

function toggle(list: string[], value: string): string[] {
  return list.includes(value) ? list.filter((v) => v !== value) : [...list, value];
}

function nonNegativeInt(text: string): number | null {
  if (text.trim() === '') return 0;
  if (!/^\d+$/.test(text.trim())) return null;
  return Number(text.trim());
}

type Errors = { binary?: string; args?: string; env?: string; gpus?: string; ram?: string };

function buildBody(
  ed: EditorState,
  t: Translation,
): { body: PutRPCMemberSpecRequest } | { errors: Errors } {
  const errors: Errors = {};
  if (!isAbsolutePath(ed.binary.trim())) errors.binary = t.errorBinaryPathInvalid;
  const args = parseArgsText(ed.argsText);
  if (hasReservedMemberFlag(args)) errors.args = t.errorRuntimeSpecMemberFlagReserved;
  const parsedEnv = parseEnvText(ed.envText, t);
  if (parsedEnv.error) errors.env = parsedEnv.error;
  const gpus: PutRPCMemberSpecRequest['gpus'] = [];
  const seen = new Set<number>();
  for (const r of ed.gpuRows) {
    const index = nonNegativeInt(r.index);
    const vram = nonNegativeInt(r.vramMb);
    if (r.index.trim() === '' || index === null || vram === null || seen.has(index)) {
      errors.gpus = t.errorRuntimeSpecGpuInvalid;
      break;
    }
    seen.add(index);
    // A member spec stores no measurement (member usage lives on the head's
    // membership rows), and the gateway ignores the field on write.
    gpus.push({ index, vram_estimate_mb: vram, vram_measured_mb: 0 });
  }
  const ram = nonNegativeInt(ed.ramMb);
  if (ram === null) errors.ram = t.errorRuntimeSpecGpuInvalid;
  if (Object.keys(errors).length > 0) return { errors };
  return {
    body: {
      enabled: ed.enabled,
      binary: ed.binary.trim(),
      args,
      env: parsedEnv.env,
      gpus,
      cache_enabled: ed.cache,
      device_backend: ed.backend,
      ram_estimate_mb: ram ?? 0,
      allowed_head_groups: ed.groups,
    },
  };
}

// max(live measurement, operator estimate), summed over the member's GPUs.
// A member without a stored spec (an automatic one) has only the live
// measurement. The spec carries no stored measurement: what a member used is
// kept per head on the membership rows, not on the spec.
function effectiveVramMb(row: RuntimeStatus, spec: RPCMemberSpec | undefined): number {
  const live = new Map((row.gpus ?? []).map((g) => [g.index, g.vram_measured_mb]));
  if (!spec) return [...live.values()].reduce((a, b) => a + b, 0);
  const specIdx = new Set(spec.gpus.map((g) => g.index));
  let sum = 0;
  for (const g of spec.gpus) {
    sum += Math.max(live.get(g.index) ?? 0, g.vram_estimate_mb);
  }
  for (const [idx, mb] of live) if (!specIdx.has(idx)) sum += mb;
  return sum;
}

// A measurement for a table cell: MB, or an em-dash when unknown.
function mbOrDash(mb: number): string {
  return mb > 0 ? `${mb} MB` : '—';
}

// Whether a head holds a member. Which head it serves is in the member's
// name, resolved for what the caller may see.
function heldText(row: RuntimeStatus, t: Translation): string {
  return row.rpc?.held_by ? t.rpcStatusHeldElsewhere : t.rpcStatusNotHeld;
}

function effectiveRamMb(row: RuntimeStatus, spec: RPCMemberSpec | undefined): number {
  return Math.max(row.ram_measured_mb ?? 0, spec?.ram_estimate_mb ?? 0);
}

// The consent picker's options: the server's own admin groups, then every
// other admin-tier group the caller can list (a system admin lists them all),
// then any selected id neither list names -- a group set through the API or
// one the caller cannot list -- so a stored consent is never invisible.
function groupOptionsFor(
  own: readonly { id: string; name: string }[],
  listed: readonly AdminGroupCandidate[],
  selected: readonly string[],
  t: Translation,
): { value: string; label: string }[] {
  const options = own.map((g) => ({ value: g.id, label: g.name }));
  const known = new Set(options.map((o) => o.value));
  for (const g of listed) {
    if (known.has(g.id)) continue;
    known.add(g.id);
    options.push({ value: g.id, label: g.name });
  }
  for (const id of selected) {
    if (known.has(id)) continue;
    known.add(id);
    options.push({ value: id, label: t.rpcAllowedHeadGroupUnlisted(id) });
  }
  return options;
}

// A member's name as the table shows it: the gateway's display name, or its
// spec id from an older gateway that sends none.
function memberName(row: RuntimeStatus): string {
  return row.display_name || row.spec_id;
}

function memberRowKey(row: RuntimeStatus): string {
  return `${row.spec_id}#${row.instance}`;
}

// A measurement as a sortable cell value: blank when unknown, so the table
// keeps it at the bottom.
function mbValue(mb: number): string {
  return mb > 0 ? String(mb) : '';
}

// The columns of the running members' table. Bind mismatch and breaches show
// as chips beside the state; the spec id and the RPC protocol are columns
// hidden by default.
function memberStatusColumns(
  specById: ReadonlyMap<string, RPCMemberSpec>,
  t: Translation,
): ListColumn<RuntimeStatus>[] {
  return [
    {
      id: 'name',
      label: t.tableName,
      value: memberName,
      render: (row) => <RpcMemberName row={row} variant="body2" />,
      filter: 'text',
    },
    {
      id: 'status',
      label: t.tableStatus,
      value: (row) => runtimeStateLabel(row.state, t),
      render: (row) => <MemberStatusCell row={row} t={t} />,
      filter: 'enum',
    },
    {
      id: 'occupancy',
      label: t.rpcStatusOccupancy,
      value: (row) => heldText(row, t),
      filter: 'enum',
    },
    {
      id: 'vram',
      label: t.rpcStatusVram,
      value: (row) => mbValue(effectiveVramMb(row, specById.get(row.spec_id))),
      render: (row) => mbOrDash(effectiveVramMb(row, specById.get(row.spec_id))),
      numeric: true,
      searchable: false,
    },
    {
      id: 'ram',
      label: t.rpcStatusRam,
      value: (row) => mbValue(effectiveRamMb(row, specById.get(row.spec_id))),
      render: (row) => mbOrDash(effectiveRamMb(row, specById.get(row.spec_id))),
      numeric: true,
      searchable: false,
    },
    {
      id: 'id',
      label: t.rpcStatusId,
      value: (row) => row.spec_id,
      render: (row) => (
        <Typography component="span" variant="body2" sx={{ fontFamily: 'monospace' }}>
          {row.spec_id}
        </Typography>
      ),
      defaultHidden: true,
    },
    {
      id: 'protocol',
      label: t.rpcStatusProtocol,
      value: (row) => row.rpc?.protocol ?? '',
      defaultHidden: true,
    },
  ];
}

// A member's state, with chips for a bind mismatch and for breaches.
function MemberStatusCell({ row, t }: Readonly<{ row: RuntimeStatus; t: Translation }>) {
  const breaches = row.rpc?.breaches ?? [];
  return (
    <Box sx={{ display: 'flex', gap: 1, alignItems: 'center', flexWrap: 'wrap' }}>
      <StatusChip status={runtimeStateBadge(row.state)} label={runtimeStateLabel(row.state, t)} />
      {row.rpc?.bind_mismatch && <StatusChip status="watch" label={t.rpcStatusBindMismatch} />}
      {breaches.length > 0 && (
        <StatusChip status="watch" label={`${t.rpcStatusBreaches}: ${breaches.length}`} />
      )}
    </Box>
  );
}

/**
 * A cluster member's row label: its display name from the gateway, with its
 * spec id (the identity support asks for) in a tooltip; the id itself from
 * an older gateway that sends no name.
 */
export function RpcMemberName({
  row,
  variant,
}: Readonly<{
  row: RuntimeStatus;
  /** The text variant of the surrounding names (the RPC tab's body1 by default). */
  variant?: 'body1' | 'body2';
}>) {
  if (!row.display_name) {
    return (
      <Typography component="span" variant={variant} sx={{ fontFamily: 'monospace' }}>
        {row.spec_id}
      </Typography>
    );
  }
  return (
    <Tooltip title={row.spec_id} describeChild>
      <Typography component="span" variant={variant} tabIndex={0} data-testid="rpc-member-name">
        {row.display_name}
      </Typography>
    </Tooltip>
  );
}

export function RpcMemberSection({
  t,
  api,
  server,
  statusRows,
  onServerSaved,
}: Readonly<{
  t: Translation;
  api: Pick<
    PortalApi,
    | 'rpcMemberSpecs'
    | 'createRPCMemberSpec'
    | 'updateRPCMemberSpec'
    | 'deleteRPCMemberSpec'
    | 'serverBinaries'
    | 'updateServer'
    | 'serverAdminGroupCandidates'
  >;
  server: PortalServer;
  // The server's live runtime status, from the parent's status stream: one
  // stream per server, not one per section.
  statusRows: readonly RuntimeStatus[];
  // Receives the server returned by a settings save, so a parent that keeps
  // the tab's state across switches can feed it back as `server`.
  onServerSaved?: (server: PortalServer) => void;
}>) {
  const { showError, showSuccess } = useToast();
  // The admin-tier groups the caller can list. A failed read leaves the
  // server's own groups, which the picker always offers.
  const [listedGroups, setListedGroups] = useState<AdminGroupCandidate[]>([]);
  useEffect(() => {
    let cancelled = false;
    api.serverAdminGroupCandidates().then(
      (groups) => {
        if (!cancelled) setListedGroups(groups);
      },
      () => {
        if (!cancelled) setListedGroups([]);
      },
    );
    return () => {
      cancelled = true;
    };
  }, [api]);

  // ---- 1. Server RPC settings ---------------------------------------------
  // The form holds unsaved edits. `seed` is the saved state the server prop
  // currently reports; `base` is the saved state the form was last aligned
  // with. A new server object with equal content (the server list poll) changes
  // nothing; changed content re-aligns the form only if it had no unsaved edits
  // against `base`, so an external change never overwrites an edit, and a save
  // only sends what the operator changed relative to `base`.
  const seedEnabled = Boolean(server.rpc_member_enabled);
  const seedGroups = server.rpc_allowed_head_groups ?? [];
  const seedKey = `${seedEnabled}|${groupsKey(seedGroups)}`;
  const [base, setBase] = useState({ id: server.id, enabled: seedEnabled, groups: seedGroups });
  const [memberEnabled, setMemberEnabled] = useState(seedEnabled);
  const [headGroups, setHeadGroups] = useState<string[]>(seedGroups);
  const [settingsBusy, setSettingsBusy] = useState(false);
  const formRef = useRef({ enabled: memberEnabled, groups: headGroups });
  formRef.current = { enabled: memberEnabled, groups: headGroups };
  const baseKey = `${base.enabled}|${groupsKey(base.groups)}`;
  useEffect(() => {
    if (base.id === server.id && baseKey === seedKey) return;
    const form = formRef.current;
    const formKey = `${form.enabled}|${groupsKey(form.groups)}`;
    // Align on a server switch, when the form is clean against its base, or
    // when the form already equals the new saved state (a save landing).
    if (base.id !== server.id || formKey === baseKey || formKey === seedKey) {
      setMemberEnabled(seedEnabled);
      setHeadGroups(seedGroups);
      setBase({ id: server.id, enabled: seedEnabled, groups: seedGroups });
    }
    // seedEnabled/seedGroups are covered by seedKey (content).
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [seedKey, server.id]);

  async function saveSettings() {
    // Each field goes in the PATCH only when the operator changed it, so a
    // save never restates a value another operator changed meanwhile.
    const body: UpdateServerRequest = {};
    if (memberEnabled !== base.enabled) body.rpc_member_enabled = memberEnabled;
    if (groupsKey(headGroups) !== groupsKey(base.groups)) body.rpc_allowed_head_groups = headGroups;
    if (Object.keys(body).length === 0) {
      showSuccess(t.rpcSettingsNoChanges);
      return;
    }
    setSettingsBusy(true);
    try {
      const updated = await api.updateServer(server.id, body);
      onServerSaved?.(updated);
      showSuccess(t.rpcSettingsSaved);
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setSettingsBusy(false);
    }
  }

  // ---- 2. Manual member specs ---------------------------------------------
  const { data: specsData, reload } = useResource(
    () => api.rpcMemberSpecs(server.id),
    [api, server.id, t],
    t,
  );
  const specs = useMemo(() => specsData?.data ?? [], [specsData]);
  const serverBinaries = useServerBinaries(api, server.id);
  const [editor, setEditor] = useState<EditorState | null>(null);
  const [errors, setErrors] = useState<Errors>({});
  const [saving, setSaving] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<RPCMemberSpec | null>(null);

  function patchEditor(patch: Partial<EditorState>) {
    setEditor((e) => (e ? { ...e, ...patch } : e));
  }
  function patchGpu(idx: number, patch: Partial<GpuRow>) {
    setEditor((e) =>
      e ? { ...e, gpuRows: e.gpuRows.map((r, i) => (i === idx ? { ...r, ...patch } : r)) } : e,
    );
  }
  function addGpu() {
    setEditor((e) => {
      if (!e) return e;
      const used = e.gpuRows.map((r) => Number(r.index)).filter((n) => Number.isFinite(n));
      const next = used.length === 0 ? 0 : Math.max(...used) + 1;
      return {
        ...e,
        gpuRows: [...e.gpuRows, { rowKey: nextRowKey(), index: String(next), vramMb: '' }],
      };
    });
  }
  function moveGpu(idx: number, delta: -1 | 1) {
    setEditor((e) => {
      if (!e) return e;
      const j = idx + delta;
      if (j < 0 || j >= e.gpuRows.length) return e;
      const rows = [...e.gpuRows];
      [rows[idx], rows[j]] = [rows[j], rows[idx]];
      return { ...e, gpuRows: rows };
    });
  }

  async function saveSpec() {
    if (!editor) return;
    const built = buildBody(editor, t);
    if ('errors' in built) {
      setErrors(built.errors);
      return;
    }
    setErrors({});
    setSaving(true);
    try {
      if (editor.id === '') await api.createRPCMemberSpec(server.id, built.body);
      else await api.updateRPCMemberSpec(server.id, editor.id, built.body);
      showSuccess(t.rpcMemberSaved);
      setEditor(null);
      reload();
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setSaving(false);
    }
  }

  async function confirmDelete() {
    const target = deleteTarget;
    if (!target) return;
    setDeleteTarget(null);
    try {
      await api.deleteRPCMemberSpec(server.id, target.id);
      showSuccess(t.rpcMemberDeleted);
      reload();
    } catch (err) {
      showError(formatPortalError(err, t));
    }
  }

  // An editor for a spec the list no longer has (deleted here or elsewhere)
  // closes: its save could only answer not-found.
  useEffect(() => {
    if (!specsData || !editor || editor.id === '') return;
    if (!specsData.data.some((s) => s.id === editor.id)) setEditor(null);
  }, [specsData, editor]);

  // ---- 3. Member status ---------------------------------------------------
  // The status stream sends its rows in no fixed order, so they are put in a
  // stable one (name, then id and instance) before the table sorts them: the
  // rows stay put between frames whatever column is sorted, or none.
  const memberRows = useMemo(
    () =>
      statusRows
        .filter((r) => r.kind === 'rpc_server')
        .sort(
          (a, b) =>
            memberName(a).localeCompare(memberName(b)) ||
            a.spec_id.localeCompare(b.spec_id) ||
            a.instance - b.instance,
        ),
    [statusRows],
  );
  const specById = useMemo(() => new Map(specs.map((s) => [s.id, s])), [specs]);
  const memberColumns = useMemo(() => memberStatusColumns(specById, t), [specById, t]);

  return (
    <Box sx={{ display: 'grid', gap: 3 }}>
      <Panel
        titleId="rpc-settings-heading"
        title={t.rpcServerSettingsTitle}
        subtitle={t.rpcTabIntro}
      >
        <Box sx={{ display: 'grid', gap: 1.5 }}>
          <FormControlLabel
            control={
              <Checkbox
                checked={memberEnabled}
                onChange={(e) => setMemberEnabled(e.target.checked)}
              />
            }
            label={t.rpcMemberEnabledLabel}
          />
          <CheckboxGroup
            legend={t.rpcAllowedHeadGroups}
            options={groupOptionsFor(server.admin_groups, listedGroups, headGroups, t)}
            selected={headGroups}
            onToggle={(v) => setHeadGroups((g) => toggle(g, v))}
          />
          {headGroups.length === 0 && (
            <Typography variant="caption" color="text.secondary">
              {t.rpcAllowedHeadGroupsDefault}
            </Typography>
          )}
          <Box>
            <Button variant="contained" onClick={saveSettings} disabled={settingsBusy}>
              {t.rpcSettingsSave}
            </Button>
          </Box>
        </Box>
      </Panel>

      <Panel
        titleId="rpc-members-heading"
        title={t.rpcManualMembersTitle}
        actions={
          <Button
            startIcon={<AddIcon />}
            onClick={() => {
              setErrors({});
              setEditor(newEditor());
            }}
          >
            {t.rpcMemberAdd}
          </Button>
        }
      >
        {specs.length === 0 && !editor && (
          <Typography color="text.secondary">{t.rpcManualMembersEmpty}</Typography>
        )}
        <Box sx={{ display: 'grid', gap: 1 }}>
          {specs.map((s) => (
            <Box
              key={s.id}
              data-spec-row
              sx={{ display: 'flex', gap: 1.5, alignItems: 'center', flexWrap: 'wrap' }}
            >
              <Typography component="span" sx={{ fontFamily: 'monospace' }}>
                {s.id}
              </Typography>
              <Typography component="span" variant="body2" color="text.secondary">
                {s.binary}
              </Typography>
              {s.gpus.length === 0 && <StatusChip status="standby" label={t.rpcMemberCpu} />}
              {!s.enabled && (
                <Typography component="span" variant="caption" color="text.secondary">
                  {t.statusDisabled}
                </Typography>
              )}
              {s.used_by_head_spec_id && (
                <Typography component="span" variant="caption" color="text.secondary">
                  {`${t.rpcMemberUsedBy} ${s.used_by_head_spec_id}`}
                </Typography>
              )}
              <Box sx={{ ml: 'auto' }}>
                <IconButton
                  size="small"
                  aria-label={`${t.runtimeSpecEditAction}: ${s.id}`}
                  onClick={() => {
                    setErrors({});
                    setEditor(editorFor(s));
                  }}
                >
                  <EditIcon fontSize="small" />
                </IconButton>
                <IconButton
                  size="small"
                  aria-label={`${t.runtimeSpecDelete}: ${s.id}`}
                  onClick={() => setDeleteTarget(s)}
                >
                  <DeleteIcon fontSize="small" />
                </IconButton>
              </Box>
            </Box>
          ))}
        </Box>

        {editor && (
          <Box
            component="form"
            aria-label={editor.id === '' ? t.rpcMemberNewTitle : t.rpcMemberEditTitle}
            onSubmit={(e) => {
              e.preventDefault();
              void saveSpec();
            }}
            sx={{ display: 'grid', gap: 1.5, mt: 2 }}
          >
            <Typography variant="subtitle1" component="h3">
              {editor.id === '' ? t.rpcMemberNewTitle : `${t.rpcMemberEditTitle}: ${editor.id}`}
            </Typography>
            <FormControlLabel
              control={
                <Checkbox
                  checked={editor.enabled}
                  onChange={(e) => patchEditor({ enabled: e.target.checked })}
                />
              }
              label={t.rpcMemberEnabled}
            />
            <BinaryPicker
              id="rpc-member-binary"
              label={t.rpcMemberBinary}
              t={t}
              kind="rpc_server"
              value={editor.binary}
              onChange={(binary) => patchEditor({ binary })}
              binaries={serverBinaries}
              error={!!errors.binary}
              helperText={errors.binary}
            />
            <Field
              id="rpc-member-args"
              label={t.runtimeSpecArgs}
              value={editor.argsText}
              onChange={(e) => patchEditor({ argsText: e.target.value })}
              multiline
              minRows={2}
              error={!!errors.args}
              helperText={errors.args ?? t.rpcMemberArgsHint}
            />
            <Field
              id="rpc-member-env"
              label={t.runtimeSpecEnv}
              value={editor.envText}
              onChange={(e) => patchEditor({ envText: e.target.value })}
              multiline
              minRows={2}
              error={!!errors.env}
              helperText={errors.env}
            />
            <Box sx={{ display: 'grid', gap: 1 }}>
              <Typography variant="subtitle2">{t.runtimeSpecGpus}</Typography>
              <Typography variant="caption" color="text.secondary">
                {t.rpcMemberGpuHint}
              </Typography>
              {editor.gpuRows.map((row, idx) => (
                <Box
                  key={row.rowKey}
                  sx={{ display: 'flex', gap: 1.5, alignItems: 'center', flexWrap: 'wrap' }}
                >
                  <IconButton
                    size="small"
                    aria-label={`${t.modelGroupMoveUp}: ${t.rpcMemberGpuRow(idx + 1)}`}
                    disabled={idx === 0}
                    onClick={() => moveGpu(idx, -1)}
                  >
                    <ArrowUpwardIcon fontSize="small" />
                  </IconButton>
                  <IconButton
                    size="small"
                    aria-label={`${t.modelGroupMoveDown}: ${t.rpcMemberGpuRow(idx + 1)}`}
                    disabled={idx === editor.gpuRows.length - 1}
                    onClick={() => moveGpu(idx, 1)}
                  >
                    <ArrowDownwardIcon fontSize="small" />
                  </IconButton>
                  <Field
                    id={`rpc-member-gpu-index-${row.rowKey}`}
                    label={t.runtimeSpecGpuIndex}
                    inputProps={{
                      'aria-label': `${t.rpcMemberGpuRow(idx + 1)}: ${t.runtimeSpecGpuIndex}`,
                    }}
                    type="number"
                    value={row.index}
                    onChange={(e) => patchGpu(idx, { index: e.target.value })}
                    sx={{ maxWidth: 160 }}
                  />
                  <Field
                    id={`rpc-member-gpu-vram-${row.rowKey}`}
                    label={t.rpcMemberGpuVram}
                    inputProps={{
                      'aria-label': `${t.rpcMemberGpuRow(idx + 1)}: ${t.rpcMemberGpuVram}`,
                    }}
                    type="number"
                    value={row.vramMb}
                    onChange={(e) => patchGpu(idx, { vramMb: e.target.value })}
                    sx={{ maxWidth: 220 }}
                  />
                  <Button
                    size="small"
                    aria-label={`${t.runtimeSpecGpuRemove}: ${t.rpcMemberGpuRow(idx + 1)}`}
                    onClick={() =>
                      patchEditor({ gpuRows: editor.gpuRows.filter((_, i) => i !== idx) })
                    }
                  >
                    {t.runtimeSpecGpuRemove}
                  </Button>
                </Box>
              ))}
              {errors.gpus && <Alert severity="error">{errors.gpus}</Alert>}
              <Box>
                <Button size="small" startIcon={<AddIcon />} onClick={addGpu}>
                  {t.runtimeSpecGpuAdd}
                </Button>
              </Box>
              {editor.gpuRows.length === 0 && (
                <StatusChip status="standby" label={t.rpcMemberCpu} />
              )}
            </Box>
            <FormControlLabel
              control={
                <Checkbox
                  checked={editor.cache}
                  onChange={(e) => patchEditor({ cache: e.target.checked })}
                />
              }
              label={t.rpcMemberCache}
            />
            <SelectField
              id="rpc-member-backend"
              label={t.rpcMemberBackend}
              value={editor.backend}
              onChange={(e) => patchEditor({ backend: e.target.value as DeviceBackend })}
            >
              {deviceBackends.map((b) => (
                <option value={b} key={b}>
                  {b}
                </option>
              ))}
            </SelectField>
            <Field
              id="rpc-member-ram"
              label={t.runtimeSpecRamEstimate}
              type="number"
              value={editor.ramMb}
              onChange={(e) => patchEditor({ ramMb: e.target.value })}
              error={!!errors.ram}
              helperText={errors.ram}
            />
            <CheckboxGroup
              legend={t.rpcAllowedHeadGroups}
              options={groupOptionsFor(server.admin_groups, listedGroups, editor.groups, t)}
              selected={editor.groups}
              onToggle={(v) => patchEditor({ groups: toggle(editor.groups, v) })}
            />
            {editor.groups.length === 0 && (
              <Typography variant="caption" color="text.secondary">
                {t.rpcAllowedHeadGroupsDefault}
              </Typography>
            )}
            <Box sx={{ display: 'flex', gap: 1 }}>
              <Button type="submit" variant="contained" disabled={saving}>
                {t.save}
              </Button>
              <Button color="secondary" onClick={() => setEditor(null)}>
                {t.cancel}
              </Button>
            </Box>
          </Box>
        )}
      </Panel>

      <Panel titleId="rpc-status-heading" title={t.rpcStatusTitle}>
        <ListTable
          rows={memberRows}
          columns={memberColumns}
          rowKey={memberRowKey}
          storageKey="op.rpc-members"
          defaultSort={{ id: 'name', order: 'asc' }}
          labels={listTableLabels(t, { empty: t.rpcStatusEmpty })}
        />
      </Panel>

      <ConfirmDialog
        open={deleteTarget !== null}
        title={t.rpcMemberDeleteTitle}
        body={t.rpcMemberDeleteBody}
        confirmLabel={t.runtimeSpecDelete}
        cancelLabel={t.cancel}
        onConfirm={() => void confirmDelete()}
        onCancel={() => setDeleteTarget(null)}
      />
    </Box>
  );
}
