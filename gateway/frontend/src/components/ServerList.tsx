// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import {
  useEffect,
  useRef,
  useState,
  type Dispatch,
  type SubmitEvent,
  type SetStateAction,
} from 'react';
import {
  Alert,
  Box,
  Button,
  Checkbox,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  FormControlLabel,
  Typography,
} from '@mui/material';
import AddIcon from '@mui/icons-material/Add';
import AppsIcon from '@mui/icons-material/Apps';
import VpnKeyIcon from '@mui/icons-material/VpnKey';
import AutorenewIcon from '@mui/icons-material/Autorenew';
import ShowChartIcon from '@mui/icons-material/ShowChart';
import MonitorHeartIcon from '@mui/icons-material/MonitorHeart';
import SpeedIcon from '@mui/icons-material/Speed';
import EditIcon from '@mui/icons-material/Edit';
import DeleteIcon from '@mui/icons-material/Delete';
import MemoryIcon from '@mui/icons-material/Memory';
import LayersIcon from '@mui/icons-material/Layers';
import type {
  AdminGroupCandidate,
  AdminUser,
  AgentStatus,
  BenchmarkStatus,
  CreateServerRequest,
  PortalServer,
  ServerHealthStatus,
  ServerOwner,
  ServerStatus,
  UpdateServerRequest,
} from '../api';
import { PortalApiError } from '../api';
import type { Translation, MessageKey, PortalApi, BadgeStatus } from './shared/types';
import { formatPortalError } from './shared/format';
import { StatusChip } from './shared/StatusChip';
import { Panel } from './shared/Panel';
import { PageTitle } from './shared/PageTitle';
import { Field } from './shared/Field';
import { SelectField } from './shared/SelectField';
import { MultiSelectField } from './shared/MultiSelectField';
import { ConfirmDialog } from './shared/ConfirmDialog';
import { SecretReveal } from './shared/SecretReveal';
import { Breadcrumbs } from './shared/Breadcrumbs';
import { ListTable, listTableLabels, type ListColumn } from './shared/ListTable';
import type { RowAction } from './shared/RowActionsMenu';
import { useToast } from './shared/ToastProvider';
import { AdminGroupPicker } from './shared/AdminGroupPicker';
import { AdminGroupsEditor } from './shared/AdminGroupsEditor';
import {
  candidatesUnderSystemGroup,
  distinctParentGroups,
  editAdminGroupOptions,
} from './shared/adminGroupLinkage';
import { AgentTokenSection } from './AgentTokenSection';
import { ApplicationSection } from './ApplicationSection';
import { PerformanceSection } from './PerformanceSection';
import { AvailabilitySection } from './AvailabilitySection';
import { HardwareSection } from './HardwareSection';
import { BenchmarkSection } from './BenchmarkSection';
import { ServerResourceGroupsSection } from './ServerResourceGroupsSection';
import { ServerEnergyPanel, type ServerEnergyPanelHandle } from './ServerEnergyPanel';
import { ServerNetbirdLinkPanel } from './ServerNetbirdLinkPanel';

const serverStatusOptions = ['active', 'disabled', 'maintenance'] as const;

// Cadence for the live server-list poll (status/health/netbird change on the 30–60s health/sync
// loops, so 5s already leads the underlying changes).
const SERVER_LIST_POLL_MS = 5000;

// Id of the caption that explains the managed_runtime_only checkbox, referenced
// from the checkbox's own input via aria-describedby. A checkbox has no
// helperText slot of its own, so the two halves are wired by id -- the reason
// must be announced on focus, not hidden behind a hover.
const MANAGED_RUNTIME_ONLY_HELP_ID = 'server-managed-runtime-only-help';

const serverStatusClassByKey: Record<ServerStatus, BadgeStatus> = {
  active: 'active',
  disabled: 'standby',
  maintenance: 'watch',
};

const serverStatusLabelByKey: Record<ServerStatus, MessageKey> = {
  active: 'statusActive',
  disabled: 'statusDisabled',
  maintenance: 'statusMaintenance',
};

const serverHealthLabelByKey: Record<ServerHealthStatus, MessageKey> = {
  healthy: 'healthHealthy',
  degraded: 'healthDegraded',
  unhealthy: 'healthUnhealthy',
  unknown: 'healthUnknown',
};

const serverHealthClassByKey: Record<ServerHealthStatus, BadgeStatus> = {
  healthy: 'active',
  degraded: 'watch',
  unhealthy: 'standby',
  unknown: 'standby',
};

const agentStatusLabelByKey: Record<AgentStatus, MessageKey> = {
  active: 'agentStatusActive',
  inactive: 'agentStatusInactive',
  unconfigured: 'agentStatusUnconfigured',
};

// unconfigured (no ServerAgent token yet) uses the same muted chip as inactive
// (has a token but isn't currently reporting) — the label text distinguishes them.
const agentStatusClassByKey: Record<AgentStatus, BadgeStatus> = {
  active: 'active',
  inactive: 'standby',
  unconfigured: 'standby',
};

function ownersText(owners: ServerOwner[]): string {
  return owners.map((owner) => owner.display_name || owner.email).join(', ');
}

// Plain-text value for the NetBird connection column (search/filter/sort). Empty
// for non-NetBird servers; the render function draws the coloured chip.
function netbirdCellText(s: PortalServer): string {
  if (!s.netbird_enabled) return '';
  if (s.netbird_peer_id === '') return 'notRegistered';
  return s.netbird_connected ? 'connected' : 'disconnected';
}

// Server<->admin-group linkage (Phase B, spec 2026-08-10). The
// distinctParentGroups/candidatesUnderSystemGroup/editAdminGroupOptions
// helpers are shared with ServicesView.tsx/ResourceGroupsView.tsx via
// ./shared/adminGroupLinkage (FV-1); see that module for their docs.

type Mode =
  | 'list'
  | 'create'
  | { kind: 'edit'; server: PortalServer }
  | { kind: 'applications'; server: PortalServer }
  | { kind: 'agentToken'; server: PortalServer }
  | { kind: 'performance'; server: PortalServer }
  | { kind: 'availability'; server: PortalServer }
  | { kind: 'hardware'; server: PortalServer }
  | { kind: 'resourceGroups'; server: PortalServer }
  | { kind: 'benchmark'; server: PortalServer };

export function ServerList({
  t,
  api,
  servers,
  setServers,
  role,
  isSystemAdmin = false,
  onModelsChanged,
  loading = false,
  pollIntervalMs,
}: Readonly<{
  t: Translation;
  api: Pick<
    PortalApi,
    | 'activeBenchmarks'
    | 'adminUsers'
    | 'agentBinaries'
    | 'agentPresenceTimeout'
    | 'agentTokenStatus'
    | 'applications'
    | 'benchmarkApplication'
    | 'benchmarkMapping'
    | 'benchmarkServer'
    | 'benchmarkStatus'
    | 'createApplication'
    | 'createMapping'
    | 'createServer'
    | 'deleteApplication'
    | 'deleteMapping'
    | 'deleteServer'
    | 'downloadAgentBinary'
    | 'generateAgentToken'
    | 'getCurrency'
    | 'getSystemSettings'
    | 'healthCheckInterval'
    | 'joinResourceGroup'
    | 'leaveResourceGroup'
    | 'mappingBenchmarks'
    | 'mappings'
    | 'netbirdEnabled'
    | 'netbirdGroups'
    | 'netbirdPeers'
    | 'probeMappingContext'
    | 'regenerateNetbirdKey'
    | 'revokeAgentToken'
    // Agent-runtime-manager (Task 20): forwarded on to ApplicationSection ->
    // RuntimeAdminSection for a server_agent application's launch specs.
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
    | 'subscribeRuntimeLogs'
    | 'serverAdminGroupCandidates'
    | 'serverAvailability'
    | 'serverHardware'
    | 'serverPerfHistory'
    | 'serverResourceGroups'
    | 'servers'
    | 'setServerAdminGroups'
    | 'setServerCertificateOverride'
    | 'setServerEnergy'
    | 'setServerHTTPSSwitchOverride'
    | 'setServerNetbird'
    | 'subscribeBenchmark'
    | 'subscribeServerPerf'
    | 'syncApplicationModels'
    | 'updateApplication'
    | 'updateMapping'
    | 'updateServer'
    | 'usageTimeSeries'
  >;
  servers: PortalServer[];
  setServers: Dispatch<SetStateAction<PortalServer[]>>;
  role: string;
  // Gates the system-admin-only NetBird linkage editor in the server edit form.
  isSystemAdmin?: boolean;
  onModelsChanged?: () => void;
  loading?: boolean;
  // Benchmark status-poll cadence (ms); injectable for tests. Defaults to the
  // shared helper's cadence.
  pollIntervalMs?: number;
}>) {
  const isAdmin = role === 'admin' || role === 'system_admin';
  const { showError, showSuccess } = useToast();
  const [adminUsers, setAdminUsers] = useState<AdminUser[]>([]);
  const [mode, setMode] = useState<Mode>('list');
  const [busy, setBusy] = useState(false);
  const [confirmingDeleteId, setConfirmingDeleteId] = useState('');
  // Delete flow: whether to also delete the server's NetBird peer + setup key.
  // Initialised to the target server's netbird_peer_managed when the dialog opens
  // (pre-checked for a portal-enrolled peer); reset on cancel/confirm.
  const [deletePeerChecked, setDeletePeerChecked] = useState(false);
  // Live per-server "benchmark running" indicator: while the list is shown, poll
  // the running benchmarks the caller may see (one per server) and key them by id.
  const [activeByServer, setActiveByServer] = useState<Record<string, BenchmarkStatus>>({});
  // Imperative handle onto the mounted ServerEnergyPanel, used ONLY by
  // submitCreate: unlike every other panel here, create has no dedicated
  // energy endpoint, so its current field values must fold into the single
  // combined create POST.
  const energyPanelRef = useRef<ServerEnergyPanelHandle>(null);

  const [name, setName] = useState('');
  const [domain, setDomain] = useState('');
  const [serverPathSuffix, setServerPathSuffix] = useState('');
  const [status, setStatus] = useState('active');
  const [ownerIds, setOwnerIds] = useState<string[]>([]);
  // managed_runtime_only (Task 6, migration 66; issue #25): the form's checkbox
  // state, plus the value it was SEEDED with. Two variables and not one,
  // because the Go UpdateServerRequest.ManagedRuntimeOnly is a `*bool` and
  // submitEdit has to be able to say nothing at all about it -- see the comment
  // there. `Seed` is captured once per open (openCreate/openEdit set both) and
  // is deliberately NOT re-synced by applyServerUpdate: a panel save that
  // happens to bring back a fresher DTO must not turn an untouched checkbox
  // into a policy write.
  const [managedRuntimeOnly, setManagedRuntimeOnly] = useState(false);
  const [managedRuntimeOnlySeed, setManagedRuntimeOnlySeed] = useState(false);
  // Admin-group linkage (Phase B, spec 2026-08-10): the admin-tier groups the
  // caller may create/link a server into. Fetched once (mirrors adminUsers,
  // gated on isAdmin -- only an admin/system_admin ever reaches the create
  // form or a server's own admin-groups editor).
  const [serverGroupCandidates, setServerGroupCandidates] = useState<AdminGroupCandidate[]>([]);
  // Create-form picker state: the chosen system (parent) group -- meaningful
  // only when the candidates span more than one -- and the chosen admin-group
  // id(s) (only meaningful when there is more than one candidate under the
  // effective system group; a single candidate auto-selects, see below).
  const [createSystemGroupId, setCreateSystemGroupId] = useState('');
  const [createAdminGroupIds, setCreateAdminGroupIds] = useState<string[]>([]);
  // Edit-form admin-groups editor state: the server's linked group ids, saved
  // via its own button (mirrors ServerNetbirdLinkPanel/ServerEnergyPanel's
  // dedicated saves).
  const [editAdminGroupIds, setEditAdminGroupIds] = useState<string[]>([]);
  // Edit-form system-group choice, used ONLY when editing a server that has no
  // containment root yet (system_group_id==""; pre-Phase-B/migrated servers) —
  // mirrors createSystemGroupId so the edit picker can offer the same
  // choose-a-system-group step create does. Ignored once a root is set.
  const [editSystemGroupId, setEditSystemGroupId] = useState('');
  const [adminGroupsBusy, setAdminGroupsBusy] = useState(false);
  // USD-per-EUR conversion factor, fetched once on mount (best-effort; on error
  // it stays 0, which availableUnits() treats as "USD unavailable"). Drives the
  // price-unit dropdown's available options + every EUR<->unit conversion in
  // ServerEnergyPanel (kept here, not in that panel, so every open of the
  // create/edit form sees the SAME warm value instead of refetching per-open).
  const [currencyFactor, setCurrencyFactor] = useState(0);
  // NetBird: whether the module is enabled (gates the create checkbox + the
  // enroll/regenerate action + the connection indicator). Read from the
  // portal-scoped GET /api/portal/netbird/enabled (a boolean only), so a normal
  // admin — not just a system-admin — sees the NetBird UI.
  const [netbirdModuleEnabled, setNetbirdModuleEnabled] = useState(false);
  const [netbirdChecked, setNetbirdChecked] = useState(false);
  // Shared display-once reveal dialog for a freshly generated setup key (create
  // hook + regenerate). Empty = closed.
  const [revealKey, setRevealKey] = useState('');
  // The ready-to-paste `netbird up …` console command that accompanies a freshly
  // generated setup key (display-once, contains the key). Empty = no command line.
  const [revealCommand, setRevealCommand] = useState('');
  const [confirmingRegenId, setConfirmingRegenId] = useState('');
  // Whether the TLS-certificate module is enabled + its configured server scope
  // ("all" / "selected"), loaded alongside the NetBird effective policy scope
  // (the same system-scoped settings fetch, system-admin only) -- drives which
  // control (opt-out vs opt-in) ServerPolicyOverrides shows, or none at all
  // when unloaded/disabled.
  const [certEnabled, setCertEnabled] = useState(false);
  const [certServerScope, setCertServerScope] = useState<string | null>(null);
  // The global P4 https-auto-switch mode ("manual" / "auto" / "selected"),
  // loaded alongside certEnabled/certServerScope (same system-scoped settings
  // fetch) -- drives which control (opt-out vs opt-in vs none)
  // ServerPolicyOverrides shows.
  const [httpsSwitchMode, setHttpsSwitchMode] = useState<string | null>(null);
  // Whether "Alle Server pingbar" (netbird_allow_ping_all_servers) is on
  // system-wide. Loaded with the effective scope; flips the per-server ping
  // control from an opt-in ("Ping erlauben") to a RED opt-out. false on any load
  // error (never blocks the form).
  const [pingAllServers, setPingAllServers] = useState(false);
  // The EFFECTIVE policy scope ("all" / "selected"), fetched via the system
  // settings endpoint when a system-admin opens the edit form — drives which
  // override control (opt-out vs opt-in) is shown. null = not loaded / load
  // failed, in which case the control is hidden entirely.
  const [netbirdEffectiveScope, setNetbirdEffectiveScope] = useState<string | null>(null);
  // Whether "Nur NetBird-Transport" (netbird_only) is on system-wide. Loaded with
  // the effective scope; drives a warning that an excluded/unmanaged server may
  // become unreachable. false on any load error (never blocks the form).
  const [netbirdOnly, setNetbirdOnly] = useState(false);
  // Policy-management state for the CREATE-form override control (read from the
  // portal-scoped netbirdEnabled() flag so a normal admin gets it too):
  // whether policy management is on, the effective scope ("all"/"selected"), and
  // whether deny-by-default is on. The create-form control's shape + forcing
  // follow these three + the role.
  const [netbirdManagePolicies, setNetbirdManagePolicies] = useState(false);
  const [netbirdDenyByDefault, setNetbirdDenyByDefault] = useState(false);
  // The create-form per-server policy override ("" / "include" / "exclude");
  // pre-set in openCreate, sent as netbird_policy_override on create.
  const [createPolicyOverride, setCreatePolicyOverride] = useState('');
  // When "Nur NetBird-Transport" is on system-wide, a NORMAL admin's new server
  // MUST be in the NetBird network (an off-mesh server would be unreachable), so
  // the create checkbox is forced on + locked (the backend enforces this too). A
  // system-admin sees it merely pre-selected + editable (they may deliberately
  // create an off-mesh server) and gets a warning instead.
  const forceNetbird = netbirdModuleEnabled && netbirdOnly && !isSystemAdmin;
  // The create-form policy-override control renders only when the effective netbird
  // checkbox is on AND policy management is on. Under selected scope + deny-by-default
  // a NORMAL admin's opt-in is FORCED (include, locked) — the backend enforces this.
  const showCreatePolicy = (forceNetbird || netbirdChecked) && netbirdManagePolicies;
  const forcePolicyInclude =
    netbirdEffectiveScope === 'selected' && netbirdDenyByDefault && !isSystemAdmin;

  // Admin-group linkage derivation (Phase B): the create form's system-group
  // step is shown ONLY when the candidates span more than one distinct
  // parent; the admin-group picker then narrows to that effective system
  // group's children, and a single remaining candidate auto-selects (no
  // visible multi-select at all — mirrors GroupsView's parent-derivation).
  const createDistinctSystemGroups = distinctParentGroups(serverGroupCandidates);
  const createEffectiveSystemGroupId =
    createDistinctSystemGroups.length === 1
      ? createDistinctSystemGroups[0].id
      : createSystemGroupId;
  const createEffectiveCandidates = candidatesUnderSystemGroup(
    serverGroupCandidates,
    createDistinctSystemGroups,
    createEffectiveSystemGroupId,
  );
  const createEffectiveAdminGroupIds =
    createEffectiveCandidates.length === 1
      ? [createEffectiveCandidates[0].id]
      : createAdminGroupIds;

  useEffect(() => {
    if (!isAdmin) return;
    let cancelled = false;
    (async () => {
      try {
        const candidates = await api.serverAdminGroupCandidates();
        if (!cancelled) setServerGroupCandidates(candidates);
      } catch {
        // create form / admin-groups editor degrade to "no candidates" (the
        // server list itself still works)
        if (!cancelled) setServerGroupCandidates([]);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [api, isAdmin]);

  useEffect(() => {
    if (!isAdmin) return;
    let cancelled = false;
    (async () => {
      try {
        const response = await api.adminUsers();
        if (!cancelled) setAdminUsers(response.data);
      } catch {
        // owner picker degrades to an empty list; the server list itself still works
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [api, isAdmin]);

  // Learn whether the NetBird module is enabled so the create checkbox, the
  // connection indicator and the enroll/regenerate action can gate on it. This
  // portal-scoped flag returns a boolean ONLY (no config leak), so it works for
  // a normal admin too — unlike the system-scoped settings endpoint.
  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const res = await api.netbirdEnabled();
        if (!cancelled) {
          setNetbirdModuleEnabled(Boolean(res.enabled));
          setNetbirdOnly(Boolean(res.netbird_only));
          setNetbirdManagePolicies(Boolean(res.manage_policies));
          setNetbirdEffectiveScope(res.effective_policy_scope ?? null);
          setNetbirdDenyByDefault(Boolean(res.deny_by_default));
        }
      } catch {
        // not reachable / not configured → the NetBird UI stays hidden
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [api]);

  // USD-per-EUR conversion factor, fetched once on mount (best-effort; on error
  // it stays 0, which availableUnits() treats as "USD unavailable" — Euro-only).
  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const res = await api.getCurrency();
        if (!cancelled) setCurrencyFactor(res.usd_per_eur);
      } catch {
        /* best-effort: keep 0 (USD unavailable) */
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [api]);

  // Lazily load the EFFECTIVE policy scope for the per-server override control
  // when a system-admin opens a server edit form. The system settings endpoint
  // is system-scoped (matches isSystemAdmin); on ANY error the control is hidden
  // (null) — never crash, never block the rest of the edit form.
  useEffect(() => {
    if (!isSystemAdmin) return;
    if (typeof mode === 'string' || mode.kind !== 'edit') return;
    let cancelled = false;
    (async () => {
      try {
        const res = await api.getSystemSettings();
        if (!cancelled) {
          setNetbirdEffectiveScope(res.netbird_effective_policy_scope);
          setNetbirdOnly(res.netbird_only);
          setPingAllServers(res.netbird_allow_ping_all_servers);
          setCertEnabled(res.cert_enabled);
          setCertServerScope(res.cert_server_scope);
          setHttpsSwitchMode(res.cert_https_switch_mode ?? 'manual');
        }
      } catch {
        if (!cancelled) {
          setNetbirdEffectiveScope(null);
          setCertEnabled(false);
          setCertServerScope(null);
          setHttpsSwitchMode(null);
        }
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [api, isSystemAdmin, mode]);

  // Poll the running benchmarks every 3s while the list view is shown so the
  // per-server "Benchmark läuft" chip stays live; stops when leaving the list.
  useEffect(() => {
    if (mode !== 'list') return;
    let cancelled = false;
    const tick = () => {
      api
        .activeBenchmarks()
        .then((runs) => {
          if (!cancelled) setActiveByServer(Object.fromEntries(runs.map((r) => [r.server_id, r])));
        })
        .catch(() => {
          /* non-blocking — the list itself still works without the indicator */
        });
    };
    tick();
    const id = setInterval(tick, 3000);
    return () => {
      cancelled = true;
      clearInterval(id);
    };
  }, [api, mode]);

  // Poll the server list every ~5s while the list view is shown so status / health /
  // netbird-connection stay live (the list endpoint already returns freshly-read state). Gated on
  // the list view so it never runs — or clobbers — a create/edit/sub-view. Latest-wins so a slow
  // tick can't overwrite a newer one; a transient error keeps the last-known list.
  useEffect(() => {
    if (mode !== 'list') return;
    let cancelled = false;
    let seq = 0;
    const tick = () => {
      const mine = ++seq;
      api
        .servers()
        .then((res) => {
          if (!cancelled && mine === seq) setServers(res.data);
        })
        .catch(() => {
          /* non-blocking — keep the last-known list on a transient error */
        });
    };
    const id = setInterval(tick, SERVER_LIST_POLL_MS);
    return () => {
      cancelled = true;
      clearInterval(id);
    };
  }, [api, mode, setServers]);

  function openCreate() {
    setName('');
    setDomain('');
    setServerPathSuffix('');
    setStatus('active');
    setOwnerIds([]);
    setManagedRuntimeOnly(false);
    setManagedRuntimeOnlySeed(false);
    setCreateSystemGroupId('');
    setCreateAdminGroupIds([]);
    setNetbirdChecked(netbirdOnly);
    // Pre-select the opt-in only under selected scope + deny-by-default (the forced
    // case) AND when policy management is on — else no control renders, so a stale
    // "include" must not leak into the submit. Otherwise start off (opt-in / opt-out
    // default off).
    setCreatePolicyOverride(
      netbirdManagePolicies && netbirdEffectiveScope === 'selected' && netbirdDenyByDefault
        ? 'include'
        : '',
    );
    setMode('create');
  }

  function openEdit(server: PortalServer) {
    setName(server.name);
    setDomain(server.domain);
    setServerPathSuffix(server.server_path_suffix ?? '');
    setStatus(server.status);
    setOwnerIds(server.owners.map((owner) => owner.id));
    // Optional on PortalServer (so the suite's server fixtures compile), never
    // omitted on the real wire DTO -- Boolean() so an absent field reads as
    // "off" rather than leaking undefined into a controlled checkbox.
    setManagedRuntimeOnly(Boolean(server.managed_runtime_only));
    setManagedRuntimeOnlySeed(Boolean(server.managed_runtime_only));
    setEditAdminGroupIds(server.admin_groups.map((g) => g.id));
    setEditSystemGroupId('');
    setMode({ kind: 'edit', server });
  }

  // Shared onSaved callback for ServerEnergyPanel / ServerNetbirdLinkPanel /
  // ServerPolicyOverrides: update the row in the list + keep the edit-mode
  // context in sync with the fresh DTO (each panel already re-syncs its own
  // fields from `updated` itself).
  function applyServerUpdate(updated: PortalServer) {
    setServers((current) => current.map((row) => (row.id === updated.id ? updated : row)));
    setMode({ kind: 'edit', server: updated });
  }

  async function submitCreate(event: SubmitEvent<HTMLFormElement>) {
    event.preventDefault();
    // Defensive guard mirroring the submit button's disabled state (Phase B):
    // the backend rejects an empty admin_group_ids set outright, but never
    // even attempt the call when the picker hasn't resolved one yet.
    if (createEffectiveAdminGroupIds.length === 0) return;
    setBusy(true);
    try {
      const useNetbird = netbirdModuleEnabled && (forceNetbird || netbirdChecked);
      // The energy fields live in ServerEnergyPanel (create has no dedicated
      // energy endpoint — unlike every other panel here, its values fold into
      // this single combined POST), so read its current values via the
      // imperative handle.
      const energy = energyPanelRef.current?.getCreatePayload();
      const body: CreateServerRequest = {
        name,
        domain,
        server_path_suffix: serverPathSuffix.trim(),
        status,
        estimated_watts: energy?.estimated_watts ?? 0,
        idle_watts: energy?.idle_watts ?? 0,
        price_per_kwh: energy?.price_per_kwh ?? 0,
        price_unit: energy?.price_unit ?? 'eur_cent',
        pue: energy?.pue ?? 0,
        admin_group_ids: createEffectiveAdminGroupIds,
        // Stated outright, unlike on edit: a row that does not exist yet has no
        // value to leave unchanged, so `false` and "omitted" both mean the
        // column's default. Offering it on create at all is the point of
        // putting the control here — a managed-only server can be provisioned
        // in one call instead of created and then PATCHed.
        managed_runtime_only: managedRuntimeOnly,
      };
      if (isAdmin) body.owner_ids = ownerIds;
      if (useNetbird) {
        body.netbird_enabled = true;
        body.netbird_policy_override =
          forcePolicyInclude && showCreatePolicy ? 'include' : createPolicyOverride;
      }
      const created = await api.createServer(body);
      // The setup key + console command + hook error are display-once (reveal modal
      // / toast); never let them linger in the persistent servers state (a plain
      // server row).
      const { netbird_setup_key, netbird_setup_command, netbird_error, ...serverRow } = created;
      setServers((current) => [serverRow, ...current]);
      setMode('list');
      // Display-once setup key (+ its console command) on success; a best-effort
      // hook error is a toast (the server was still created).
      if (netbird_setup_key) {
        setRevealKey(netbird_setup_key);
        setRevealCommand(netbird_setup_command ?? '');
      } else if (netbird_error) showError(t.serverNetbirdError(netbird_error));
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setBusy(false);
    }
  }

  // Enroll-or-regenerate a setup key for a server. On a non-NetBird server the
  // backend flips it to NetBird-enabled, so refresh the rows afterwards to show
  // the new connection state (best-effort — the reveal is the primary result).
  async function regenerateNetbirdKey(id: string) {
    setConfirmingRegenId('');
    try {
      const res = await api.regenerateNetbirdKey(id);
      if (res.setup_key) {
        setRevealKey(res.setup_key);
        setRevealCommand(res.netbird_setup_command ?? '');
      }
      try {
        const refreshed = await api.servers();
        setServers(refreshed.data);
      } catch {
        // the reveal already succeeded; a stale row corrects on the next load
      }
    } catch (err) {
      // The backend gates key generation on a gateway-managed (or unenrolled) peer;
      // a non-managed existing peer → 409 netbird.peer_not_managed → a specific toast
      // (the UI already disables the action for that case; this is the backstop).
      if (err instanceof PortalApiError && err.code === 'netbird.peer_not_managed') {
        showError(t.serverNetbirdPeerNotManaged);
      } else {
        showError(formatPortalError(err, t));
      }
    }
  }

  // Edit-form admin-groups editor save (Phase B, spec 2026-08-10): a
  // full-replace of the server's linked admin-group set via its own
  // endpoint, independent of the main form's Save. Mirrors saveNetbirdLink/
  // saveServerEnergy (update rows + edit context, re-sync from the fresh DTO,
  // success toast).
  async function saveAdminGroups(ids: string[]) {
    if (typeof mode === 'string' || mode.kind !== 'edit') return;
    setAdminGroupsBusy(true);
    try {
      const updated = await api.setServerAdminGroups(mode.server.id, ids);
      setServers((current) => current.map((row) => (row.id === updated.id ? updated : row)));
      setMode({ kind: 'edit', server: updated });
      setEditAdminGroupIds(updated.admin_groups.map((g) => g.id));
      showSuccess(t.serverAdminGroupsSaved);
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setAdminGroupsBusy(false);
    }
  }

  async function submitEdit(event: SubmitEvent<HTMLFormElement>) {
    event.preventDefault();
    if (typeof mode === 'string' || mode.kind !== 'edit') return;
    setBusy(true);
    try {
      // Energy fields are intentionally NOT sent here — the "Energy & cost" section
      // has its own Save button (saveServerEnergy) and the backend PATCH leaves the
      // energy columns untouched when omitted (nil = keep).
      const body: UpdateServerRequest = {
        name,
        domain,
        server_path_suffix: serverPathSuffix.trim(),
        status,
      };
      if (isAdmin) body.owner_ids = ownerIds;
      // managed_runtime_only is the one field in this body whose absence and
      // whose `false` mean DIFFERENT things: the Go request struct holds it as
      // a `*bool` and UpdateServer applies it under `if != nil`. So it is sent
      // only when the operator actually moved the checkbox, and then in the
      // direction they moved it -- `false` included, which is how the
      // restriction gets lifted.
      //
      // Sending it unconditionally would compile, pass a "the switch works"
      // test, and still be a defect: every save made for an unrelated reason
      // would restate the policy from whatever this form last read. On a server
      // another operator flipped in the meantime that restates it WRONG, clears
      // the flag, and returns an ordinary 200 with nothing to notice. The
      // comparison is against the seed captured when the form opened, not
      // against mode.server, which applyServerUpdate can replace mid-edit.
      if (managedRuntimeOnly !== managedRuntimeOnlySeed) {
        body.managed_runtime_only = managedRuntimeOnly;
      }
      const updated = await api.updateServer(mode.server.id, body);
      setServers((current) => current.map((row) => (row.id === updated.id ? updated : row)));
      setMode('list');
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setBusy(false);
    }
  }

  async function removeServer(id: string) {
    try {
      const res = await api.deleteServer(id, deletePeerChecked);
      setServers((current) => current.filter((row) => row.id !== id));
      setConfirmingDeleteId('');
      setDeletePeerChecked(false);
      setMode('list');
      // The row delete always succeeds; a best-effort NetBird peer/key cleanup
      // failure is surfaced as a (non-fatal) warning toast — the row is gone.
      if (res.netbird_peer_delete_failed) showError(t.serverNetbirdPeerDeleteWarning);
    } catch (err) {
      showError(formatPortalError(err, t));
    }
  }

  const columns: ListColumn<PortalServer>[] = [
    // Optional (hidden by default): the internal server id, first so it leads the
    // row when enabled.
    {
      id: 'server_id',
      label: t.tableServerId,
      value: (s) => s.id,
      filter: 'text',
      defaultHidden: true,
    },
    { id: 'name', label: t.tableName, value: (s) => s.name, filter: 'text' },
    { id: 'domain', label: t.tableDomain, value: (s) => s.domain, filter: 'text' },
    // Optional (hidden by default): the server's linked admin-group set
    // (Phase B, spec 2026-08-10).
    {
      id: 'admin_groups',
      label: t.serverAdminGroupLabel,
      value: (s) => s.admin_groups.map((g) => g.name).join(', '),
      filter: 'text',
      defaultHidden: true,
    },
    {
      id: 'status',
      label: t.tableStatus,
      value: (s) => s.status,
      filter: 'enum',
      searchable: false,
      enumLabel: (v) => t[serverStatusLabelByKey[v as ServerStatus] ?? 'statusActive'],
      render: (s) => (
        <StatusChip
          status={serverStatusClassByKey[s.status] ?? 'standby'}
          label={t[serverStatusLabelByKey[s.status] ?? 'statusActive']}
        />
      ),
    },
    // Live "benchmark running" indicator (amber chip w/ progress) fed by the 3s
    // activeBenchmarks poll; renders nothing for a server with no running run.
    {
      id: 'benchmark_running',
      label: t.benchmarkRunning,
      value: (s) => (activeByServer[s.id] ? '1' : ''),
      render: (s) => {
        const run = activeByServer[s.id];
        return run ? (
          <StatusChip status="watch" label={`${t.benchmarkRunning} (${run.done}/${run.total})`} />
        ) : null;
      },
    },
    {
      id: 'owners',
      label: t.tableOwners,
      value: (s) => ownersText(s.owners),
      filter: 'text',
      render: (s) => ownersText(s.owners) || '-',
    },
    {
      id: 'health',
      label: t.health,
      value: (s) => s.health_status,
      filter: 'enum',
      searchable: false,
      enumLabel: (v) => t[serverHealthLabelByKey[v as ServerHealthStatus] ?? 'healthUnknown'],
      render: (s) => (
        <StatusChip
          status={serverHealthClassByKey[s.health_status] ?? 'standby'}
          label={t[serverHealthLabelByKey[s.health_status] ?? 'healthUnknown']}
        />
      ),
    },
    // Live-derived ServerAgent presence (active/inactive/unconfigured), from the
    // effective (per-server or system-default) agent_presence_timeout_seconds
    // window. Purely visual (mirrors the health column): keep the raw enum
    // tokens out of the global search.
    {
      id: 'agent',
      label: t.tableAgent,
      value: (s) => s.agent_status,
      filter: 'enum',
      searchable: false,
      enumLabel: (v) => t[agentStatusLabelByKey[v as AgentStatus] ?? 'agentStatusUnconfigured'],
      render: (s) => (
        <StatusChip
          status={agentStatusClassByKey[s.agent_status] ?? 'standby'}
          label={t[agentStatusLabelByKey[s.agent_status] ?? 'agentStatusUnconfigured']}
        />
      ),
    },
    // NetBird connection tri-state (only when the module is enabled): not
    // registered (peer not yet enrolled) / connected / disconnected. Non-NetBird
    // servers render nothing.
    ...(netbirdModuleEnabled
      ? [
          {
            id: 'netbird',
            label: t.settingsNetbirdTitle,
            value: (s: PortalServer) => netbirdCellText(s),
            // Purely visual (mirrors the health column): keep its raw tokens
            // (connected/disconnected/notRegistered) out of the global search.
            searchable: false,
            render: (s: PortalServer) => {
              if (!s.netbird_enabled) return null;
              if (s.netbird_peer_id === '')
                return <StatusChip status="watch" label={t.serverNetbirdNotRegistered} />;
              return s.netbird_connected ? (
                <StatusChip status="active" label={t.serverNetbirdConnected} />
              ) : (
                <StatusChip status="error" label={t.serverNetbirdDisconnected} />
              );
            },
          } as ListColumn<PortalServer>,
        ]
      : []),
  ];

  const rowActions = (server: PortalServer): RowAction[] => [
    {
      key: 'applications',
      label: t.applicationManage,
      icon: <AppsIcon fontSize="small" />,
      onClick: () => setMode({ kind: 'applications', server }),
    },
    {
      key: 'agent',
      label: t.agentToken,
      icon: <VpnKeyIcon fontSize="small" />,
      onClick: () => setMode({ kind: 'agentToken', server }),
    },
    {
      key: 'performance',
      label: t.serverPerformance,
      icon: <ShowChartIcon fontSize="small" />,
      onClick: () => setMode({ kind: 'performance', server }),
    },
    {
      key: 'availability',
      label: t.serverAvailability,
      icon: <MonitorHeartIcon fontSize="small" />,
      onClick: () => setMode({ kind: 'availability', server }),
    },
    {
      key: 'hardware',
      label: t.serverHardware,
      icon: <MemoryIcon fontSize="small" />,
      onClick: () => setMode({ kind: 'hardware', server }),
    },
    {
      key: 'resource-groups',
      label: t.serverResourceGroupsAction,
      icon: <LayersIcon fontSize="small" />,
      onClick: () => setMode({ kind: 'resourceGroups', server }),
    },
    {
      key: 'benchmark',
      label: t.runBenchmark,
      icon: <SpeedIcon fontSize="small" />,
      onClick: () => setMode({ kind: 'benchmark', server }),
    },
    // Enroll-or-regenerate: shown for ANY server when the module is enabled (a
    // normal admin can enroll a non-NetBird server too). The label reflects state.
    // DISABLED (with a tooltip) unless the peer is gateway-managed OR there is no
    // peer yet (peer_id===""): generating a key would proactively delete the
    // server's existing peer, which must never touch a non-managed / foreign peer.
    // The backend enforces the same gate (409 netbird.peer_not_managed); this is a
    // UX affordance. Kept present-but-disabled (not hidden) so the row action count
    // is stable (the row stays a kebab menu).
    ...(netbirdModuleEnabled
      ? [
          {
            key: 'netbird-regen',
            label: server.netbird_enabled ? t.serverNetbirdRegenerate : t.serverNetbirdEnroll,
            icon: <AutorenewIcon fontSize="small" />,
            disabled: server.netbird_peer_id !== '' && !server.netbird_peer_managed,
            title:
              server.netbird_peer_id !== '' && !server.netbird_peer_managed
                ? t.serverNetbirdPeerNotManaged
                : undefined,
            onClick: () => setConfirmingRegenId(server.id),
          },
        ]
      : []),
    {
      key: 'edit',
      label: t.serverActionEdit,
      icon: <EditIcon fontSize="small" />,
      onClick: () => openEdit(server),
    },
    {
      key: 'delete',
      label: t.serverActionDelete,
      color: 'error',
      icon: <DeleteIcon fontSize="small" />,
      onClick: () => {
        setDeletePeerChecked(server.netbird_peer_managed);
        setConfirmingDeleteId(server.id);
      },
    },
  ];

  // Applications sub-view for one server. ApplicationSection renders its own
  // breadcrumb (appending the server + deeper levels) from this trail.
  if (typeof mode !== 'string' && mode.kind === 'applications') {
    const server = servers.find((s) => s.id === mode.server.id) ?? mode.server;
    return (
      <ApplicationSection
        key={`app-${server.id}`}
        t={t}
        api={api}
        server={server}
        onModelsChanged={onModelsChanged}
        trail={[{ label: t.servers, onClick: () => setMode('list') }]}
      />
    );
  }

  // Server-reporting-agent token sub-view for one server (its own action).
  if (typeof mode !== 'string' && mode.kind === 'agentToken') {
    const server = servers.find((s) => s.id === mode.server.id) ?? mode.server;
    return (
      <>
        <Breadcrumbs
          ariaLabel={t.breadcrumb}
          backLabel={t.back}
          items={[{ label: t.servers, onClick: () => setMode('list') }, { label: server.name }]}
        />
        <AgentTokenSection key={`agent-${server.id}`} t={t} api={api} server={server} />
      </>
    );
  }

  // Live per-server performance sub-view (its own action).
  if (typeof mode !== 'string' && mode.kind === 'performance') {
    const server = servers.find((s) => s.id === mode.server.id) ?? mode.server;
    return (
      <>
        <Breadcrumbs
          ariaLabel={t.breadcrumb}
          backLabel={t.back}
          items={[{ label: t.servers, onClick: () => setMode('list') }, { label: server.name }]}
        />
        <PerformanceSection key={`perf-${server.id}`} t={t} api={api} server={server} />
      </>
    );
  }

  // Per-server availability sub-view (its own action) — health + agent timelines.
  if (typeof mode !== 'string' && mode.kind === 'availability') {
    const server = servers.find((s) => s.id === mode.server.id) ?? mode.server;
    return (
      <>
        <Breadcrumbs
          ariaLabel={t.breadcrumb}
          backLabel={t.back}
          items={[{ label: t.servers, onClick: () => setMode('list') }, { label: server.name }]}
        />
        <AvailabilitySection key={`avail-${server.id}`} t={t} api={api} server={server} />
      </>
    );
  }

  // Per-server hardware inventory sub-view (its own action) — static hardware report.
  if (typeof mode !== 'string' && mode.kind === 'hardware') {
    const server = servers.find((s) => s.id === mode.server.id) ?? mode.server;
    return (
      <>
        <Breadcrumbs
          ariaLabel={t.breadcrumb}
          backLabel={t.back}
          items={[{ label: t.servers, onClick: () => setMode('list') }, { label: server.name }]}
        />
        <HardwareSection key={`hw-${server.id}`} t={t} api={api} server={server} />
      </>
    );
  }

  // Per-server owner resource-group self-service sub-view (its own action).
  if (typeof mode !== 'string' && mode.kind === 'resourceGroups') {
    const server = servers.find((s) => s.id === mode.server.id) ?? mode.server;
    return (
      <>
        <Breadcrumbs
          ariaLabel={t.breadcrumb}
          backLabel={t.back}
          items={[
            { label: t.servers, onClick: () => setMode('list') },
            { label: server.name },
            { label: t.serverResourceGroupsAction },
          ]}
        />
        <ServerResourceGroupsSection key={`rg-${server.id}`} t={t} api={api} server={server} />
      </>
    );
  }

  // Per-server benchmark sub-view (its own action) — pre-scoped to the whole server.
  if (typeof mode !== 'string' && mode.kind === 'benchmark') {
    const server = servers.find((s) => s.id === mode.server.id) ?? mode.server;
    return (
      <>
        <Breadcrumbs
          ariaLabel={t.breadcrumb}
          backLabel={t.back}
          items={[
            { label: t.servers, onClick: () => setMode('list') },
            { label: server.name },
            { label: t.benchmarkArea },
          ]}
        />
        <BenchmarkSection
          key={`bench-${server.id}`}
          t={t}
          api={api}
          server={server}
          initialScope={{ kind: 'server' }}
          onModelsChanged={onModelsChanged}
          pollIntervalMs={pollIntervalMs}
        />
      </>
    );
  }

  // Create / edit sub-view (input mask).
  if (mode !== 'list') {
    const editing = mode !== 'create';
    // A create-time NetBird server auto-manages its domain, so lock the field.
    const createNetbird = !editing && netbirdModuleEnabled && (forceNetbird || netbirdChecked);
    // The server being edited (null on create) — drives the system-admin linkage
    // editor's read-only group/key + connection state.
    const editServer = typeof mode === 'string' ? null : mode.server;
    // Peer ids already linked to ANOTHER server (exclude the one being edited) — so
    // the picker can disable/annotate them and the admin avoids the 409.
    const linkedElsewhere = new Set(
      servers
        .filter((s) => editServer && s.id !== editServer.id && s.netbird_peer_id)
        .map((s) => s.netbird_peer_id),
    );
    return (
      <>
        <Breadcrumbs
          ariaLabel={t.breadcrumb}
          backLabel={t.back}
          items={[
            { label: t.servers, onClick: () => setMode('list') },
            { label: editing ? t.serverEditTitle : t.serverCreate },
          ]}
        />
        <Panel titleId="server-form-heading" title={editing ? t.serverEditTitle : t.serverCreate}>
          <Box
            component="form"
            onSubmit={editing ? submitEdit : submitCreate}
            sx={{ display: 'grid', gridTemplateColumns: 'minmax(260px, 480px)', gap: 2.25 }}
          >
            <Field
              id="server-name"
              label={t.serverNameLabel}
              value={name}
              onChange={(e) => setName(e.target.value)}
              required
            />
            {!editing && netbirdModuleEnabled && (
              <Box>
                <FormControlLabel
                  control={
                    <Checkbox
                      checked={forceNetbird ? true : netbirdChecked}
                      disabled={forceNetbird}
                      onChange={(e) => setNetbirdChecked(e.target.checked)}
                    />
                  }
                  label={t.serverNetbirdEnable}
                />
                {forceNetbird && (
                  <Typography variant="caption" color="text.secondary" sx={{ display: 'block' }}>
                    {t.serverNetbirdOnlyForcedNote}
                  </Typography>
                )}
                {netbirdModuleEnabled && netbirdOnly && isSystemAdmin && (
                  <Alert severity="warning" sx={{ mt: 1 }}>
                    {t.serverNetbirdOnlyPrecheckWarning}
                  </Alert>
                )}
              </Box>
            )}
            {/* Create-form per-server policy override, role/scope/deny aware (the
                backend enforces the same). Selected scope → an opt-in (include):
                under deny-by-default a NORMAL admin's is FORCED checked + locked,
                a SYSTEM admin's is pre-checked but editable. */}
            {!editing && showCreatePolicy && netbirdEffectiveScope === 'selected' && (
              <Box>
                <FormControlLabel
                  control={
                    <Checkbox
                      checked={forcePolicyInclude ? true : createPolicyOverride === 'include'}
                      disabled={forcePolicyInclude}
                      onChange={(e) => setCreatePolicyOverride(e.target.checked ? 'include' : '')}
                    />
                  }
                  label={t.serverNetbirdPolicyInclude}
                />
                {netbirdDenyByDefault && (
                  <Typography
                    variant="caption"
                    color={forcePolicyInclude ? 'error.main' : 'text.secondary'}
                    sx={{ display: 'block' }}
                  >
                    {forcePolicyInclude
                      ? t.serverNetbirdPolicyForcedNote
                      : t.serverNetbirdPolicyPrecheckNote}
                  </Typography>
                )}
              </Box>
            )}
            {/* All scope → a RED opt-out (exclude), system-admin only (a normal admin
                gets no policy control in all scope). */}
            {!editing && showCreatePolicy && netbirdEffectiveScope === 'all' && isSystemAdmin && (
              <Box>
                <FormControlLabel
                  control={
                    <Checkbox
                      color="error"
                      checked={createPolicyOverride === 'exclude'}
                      onChange={(e) => setCreatePolicyOverride(e.target.checked ? 'exclude' : '')}
                    />
                  }
                  label={
                    <Typography component="span" sx={{ color: 'error.main' }}>
                      {t.serverNetbirdPolicyExclude}
                    </Typography>
                  }
                />
                {netbirdDenyByDefault && (
                  <Alert severity="warning" sx={{ mt: 1 }}>
                    {t.serverNetbirdPolicyOptOutDenyNote}
                  </Alert>
                )}
              </Box>
            )}
            <Field
              id="server-domain"
              label={t.serverDomainLabel}
              value={domain}
              onChange={(e) => setDomain(e.target.value)}
              required={!createNetbird}
              disabled={createNetbird}
              helperText={createNetbird ? t.serverNetbirdDomainAuto : undefined}
            />
            <Field
              id="server-path-suffix"
              label={t.serverPathSuffixLabel}
              value={serverPathSuffix}
              onChange={(e) => setServerPathSuffix(e.target.value)}
              helperText={t.serverPathSuffixHelp}
            />
            {/* Server<->admin-group linkage (Phase B, spec 2026-08-10): >=1 admin
                group required for EVERY caller, including system_admin. Exactly
                one candidate -> auto-selected (no field); several -> a
                required multi-select, narrowed to one system group first when
                the candidates span more than one; none -> a hint + the create
                action stays disabled (see the page-level Server-erstellen
                button + the submit button below). */}
            {!editing && (
              <AdminGroupPicker
                idPrefix="server"
                candidates={serverGroupCandidates}
                systemGroupId={createSystemGroupId}
                onSystemGroupIdChange={setCreateSystemGroupId}
                adminGroupIds={createAdminGroupIds}
                onAdminGroupIdsChange={setCreateAdminGroupIds}
                labels={{
                  noCandidatesHint: t.serverNoAdminGroupHint,
                  systemGroupLabel: t.serverAdminGroupSystemGroupLabel,
                  systemGroupAuto: t.serverAdminGroupSystemGroupAuto,
                  adminGroupLabel: t.serverAdminGroupLabel,
                  adminGroupAuto: t.serverAdminGroupAuto,
                }}
              />
            )}
            <SelectField
              id="server-status"
              label={t.serverStatusLabel}
              value={status}
              onChange={(e) => setStatus(e.target.value)}
            >
              {serverStatusOptions.map((s) => (
                <option value={s} key={s}>
                  {t[serverStatusLabelByKey[s]]}
                </option>
              ))}
            </SelectField>
            {/* managed_runtime_only (Task 6, migration 66; issue #25). Shown on
                BOTH paths and to EVERY caller who reached this form.

                Not gated on isAdmin, deliberately, even though the owners field
                directly below it is: that gate exists because UpdateServer has a
                matching one (`req.OwnerIDs != nil && !isAdmin(principal)` ->
                ErrServerForbidden). ManagedRuntimeOnly has no field-level check
                at all -- authorizeServer (system scope OR a server owner OR an
                admin-group manager) is the whole rule, and the HTTP layer asks
                only for scopeGatewayUse -- so a server owner, who reaches this
                form through the ungated row action, may set it. Copying the
                owners gate here would hide a control the backend accepts;
                inventing a stricter one would be a portal-only rule nothing
                enforces.

                The reason line is a real caption wired through aria-describedby
                rather than a `title` or a Tooltip, so it is announced on focus
                and reachable by keyboard -- the same call ApplicationSection's
                type field makes with its helperText, and what issue #26 asks
                for. It has to carry two things the label cannot: the flag is
                NOT retroactive (the backend reads it inside CreateApplication
                only), and once the server's one server_agent application exists
                the applications view stops offering a create button at all. */}
            <Box>
              <FormControlLabel
                control={
                  <Checkbox
                    checked={managedRuntimeOnly}
                    onChange={(e) => setManagedRuntimeOnly(e.target.checked)}
                    slotProps={{ input: { 'aria-describedby': MANAGED_RUNTIME_ONLY_HELP_ID } }}
                  />
                }
                label={t.serverManagedRuntimeOnlyLabel}
              />
              <Typography
                id={MANAGED_RUNTIME_ONLY_HELP_ID}
                variant="caption"
                color="text.secondary"
                sx={{ display: 'block' }}
              >
                {t.serverManagedRuntimeOnlyHelp}
              </Typography>
            </Box>
            {isAdmin && (
              <MultiSelectField
                id="server-owners"
                label={t.serverOwnersLabel}
                placeholder={t.listSearchPlaceholder}
                options={adminUsers.map((u) => ({
                  value: u.id,
                  label: u.display_name || u.email,
                  sublabel: u.display_name ? u.email : undefined,
                }))}
                selected={ownerIds}
                onChange={setOwnerIds}
              />
            )}
            <Box sx={{ display: 'flex', gap: 1.5 }}>
              <Button
                type="submit"
                variant="contained"
                disabled={busy || (!editing && createEffectiveAdminGroupIds.length === 0)}
              >
                {editing ? t.serverActionSave : t.serverCreate}
              </Button>
              <Button
                type="button"
                variant="text"
                color="secondary"
                onClick={() => setMode('list')}
              >
                {t.cancel}
              </Button>
            </Box>
          </Box>
        </Panel>

        {/* Energy & cost config: its own section (like the NetBird linkage), saved
            by the main form's Save button on create (the create payload reads this
            state via the imperative handle) or by its own button on edit. Shown for
            both create and edit. */}
        <ServerEnergyPanel
          ref={energyPanelRef}
          t={t}
          api={api}
          server={editServer}
          currencyFactor={currencyFactor}
          onSaved={applyServerUpdate}
        />

        {/* Admin-groups editor (Phase B, spec 2026-08-10): add/remove the
            server's linked admin-tier groups. For a server that ALREADY has a
            containment root, narrowed to that root (system_group_id)'s children
            -- any admin+ who reached the edit form at all, not just
            system_admin. For a server with NO root yet (pre-Phase-B/migrated:
            system_group_id==""), the create-style choose-a-system-group flow so
            the root can be SET (in practice only system_admin or a ServerOwner
            reaches such a server per authorizeServer). >=1 group must remain
            (the backend rejects an empty set; the Save button mirrors that).
            Hidden on create (a brand-new server's linkage is set by the create
            picker above). */}
        {isAdmin && editServer && (
          <Panel titleId="server-admin-groups-heading" title={t.serverAdminGroupsSectionTitle}>
            <AdminGroupsEditor
              idPrefix="server"
              candidates={serverGroupCandidates}
              fixedOptions={editAdminGroupOptions(serverGroupCandidates, editServer, (s) => ({
                id: s.system_group_id,
                name: s.system_group_name,
              }))}
              adminGroupIds={editAdminGroupIds}
              onAdminGroupIdsChange={setEditAdminGroupIds}
              busy={adminGroupsBusy}
              onSave={saveAdminGroups}
              labels={{
                noCandidatesHint: t.serverNoAdminGroupHint,
                systemGroupLabel: t.serverAdminGroupSystemGroupLabel,
                systemGroupAuto: t.serverAdminGroupSystemGroupAuto,
                adminGroupLabel: t.serverAdminGroupLabel,
                adminGroupAuto: t.serverAdminGroupAuto,
                saveLabel: t.serverAdminGroupsSave,
              }}
              ungrouped={{
                isUngrouped: editServer.system_group_id === '',
                systemGroupId: editSystemGroupId,
                onSystemGroupIdChange: setEditSystemGroupId,
              }}
            />
          </Panel>
        )}

        {/* System-admin NetBird linkage editor (+ the nested certificate/https-switch
            overrides): link a manually-created peer to this server. Hidden for
            non-system-admins and on create. */}
        {isSystemAdmin && editServer && (
          <ServerNetbirdLinkPanel
            t={t}
            api={api}
            server={editServer}
            linkedElsewhere={linkedElsewhere}
            pingAllServers={pingAllServers}
            netbirdEffectiveScope={netbirdEffectiveScope}
            netbirdOnly={netbirdOnly}
            certEnabled={certEnabled}
            certServerScope={certServerScope}
            httpsSwitchMode={httpsSwitchMode}
            onSaved={applyServerUpdate}
          />
        )}
      </>
    );
  }

  const regenTarget = servers.find((s) => s.id === confirmingRegenId);
  const regenLabel =
    regenTarget && !regenTarget.netbird_enabled ? t.serverNetbirdEnroll : t.serverNetbirdRegenerate;

  // The server being deleted (drives the optional "also delete peer" checkbox).
  // The checkbox is offered only when the module is on AND this is a NetBird
  // server that actually has a peer OR a setup key to clean up.
  const deleteTarget = servers.find((s) => s.id === confirmingDeleteId);
  const showDeletePeerCheckbox =
    !!deleteTarget &&
    netbirdModuleEnabled &&
    deleteTarget.netbird_enabled &&
    (deleteTarget.netbird_peer_id !== '' || deleteTarget.netbird_setup_key_id !== '');

  return (
    <>
      <PageTitle
        title={t.servers}
        subtitle={t.serversIntro}
        action={
          // NOT gated on serverGroupCandidates here (that fetch is async and
          // resolves after mount) -- the entry action always opens the create
          // form; the form itself (below) shows the hint + blocks Save when
          // the caller has no admin-group candidate to link the new server
          // into. Mirrors every other async-loaded create-form affordance in
          // this component (netbird_only, policy scope, ...): never block the
          // primary action on a fetch that hasn't resolved yet.
          isAdmin ? (
            <Button variant="contained" startIcon={<AddIcon />} onClick={openCreate}>
              {t.serverCreate}
            </Button>
          ) : undefined
        }
      />
      <Panel titleId="server-heading" title={t.serverListTitle}>
        <ListTable
          rows={servers}
          columns={columns}
          rowKey={(s) => s.id}
          actions={rowActions}
          maxInlineActions={9}
          storageKey="op.servers"
          minWidth={860}
          labels={listTableLabels(t)}
          loading={loading}
        />
      </Panel>

      <ConfirmDialog
        open={confirmingDeleteId !== ''}
        title={t.serverDeleteConfirm}
        confirmLabel={t.serverActionDelete}
        cancelLabel={t.serverActionCancel}
        extra={
          showDeletePeerCheckbox ? (
            <FormControlLabel
              control={
                <Checkbox
                  checked={deletePeerChecked}
                  onChange={(e) => setDeletePeerChecked(e.target.checked)}
                />
              }
              label={t.serverNetbirdDeletePeer}
            />
          ) : undefined
        }
        onConfirm={() => removeServer(confirmingDeleteId)}
        onCancel={() => {
          setConfirmingDeleteId('');
          setDeletePeerChecked(false);
        }}
      />

      <ConfirmDialog
        open={confirmingRegenId !== ''}
        title={regenLabel}
        body={t.serverNetbirdRegenerateConfirm}
        confirmLabel={regenLabel}
        cancelLabel={t.serverActionCancel}
        onConfirm={() => regenerateNetbirdKey(confirmingRegenId)}
        onCancel={() => setConfirmingRegenId('')}
      />

      <Dialog
        open={revealKey !== ''}
        onClose={() => {
          setRevealKey('');
          setRevealCommand('');
        }}
        maxWidth="md"
        fullWidth
      >
        <DialogTitle>{t.serverNetbirdKeyTitle}</DialogTitle>
        <DialogContent>
          <SecretReveal
            title={t.serverNetbirdKeyTitle}
            copyValue={revealKey}
            copyLabel={t.serverNetbirdKeyTitle}
          >
            <code>{revealKey}</code>
          </SecretReveal>
          {revealCommand && (
            <SecretReveal
              title={t.serverNetbirdSetupCommand}
              copyValue={revealCommand}
              copyLabel={t.serverNetbirdSetupCommand}
            >
              <code>{revealCommand}</code>
            </SecretReveal>
          )}
          <Typography variant="body2" sx={{ mt: 1 }}>
            {t.serverNetbirdKeyHint}
          </Typography>
        </DialogContent>
        <DialogActions>
          <Button
            onClick={() => {
              setRevealKey('');
              setRevealCommand('');
            }}
          >
            {t.captureClose}
          </Button>
        </DialogActions>
      </Dialog>
    </>
  );
}
