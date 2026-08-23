// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useEffect, useState } from 'react';
import {
  Alert,
  Autocomplete,
  Box,
  Button,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  FormControlLabel,
  FormHelperText,
  Stack,
  Switch,
  TextField,
  Typography,
} from '@mui/material';
import { ConfirmDialog } from './shared/ConfirmDialog';
import { SecretReveal } from './shared/SecretReveal';
import { NetbirdNetworkPanel } from './netbird/NetbirdNetworkPanel';
import { PortalApiError } from '../api';
import type { NetbirdStatus, NetbirdTokenStatus } from '../api';
import type { Translation, PortalApi } from './shared/types';

// Local mirror of the four fields the peer picker needs (same shape as api.netbirdPeers()).
type NetbirdPeerOption = { id: string; name: string; dns_label: string; connected: boolean };
import { formatPortalError } from './shared/format';
import { useResource } from './shared/useResource';
import { PageTitle } from './shared/PageTitle';
import { Panel } from './shared/Panel';
import { SelectField } from './shared/SelectField';
import { Field } from './shared/Field';
import { useToast } from './shared/ToastProvider';

/**
 * System-admin + module-gated "NetBird" view, split into FOUR ordered sections,
 * each with its OWN Save button:
 *   1. Admin-Verbindung — the module-config (url / write-only token / test / token
 *      rotation + auto-rotate threshold / group names / peer-sync interval).
 *   2. Netzwerk — a LIVE editor of the NetBird account's network settings
 *      (dns_domain / CIDRs / IPv6 groups), rendered by NetbirdNetworkPanel; it
 *      writes the NetBird account directly (NOT system_settings).
 *   3. Peer-Einstellungen — netbird_only transport, gateway-peer linkage,
 *      setup-key/sidecar-enroll actions + live transport status.
 *   4. Policies-Einstellungen — least-privilege policy management posture +
 *      ping-allow toggles + reconcile interval.
 *
 * The three system_settings sections PUT DISJOINT field sets (nil = keep, since
 * UpdateSystemSettingsRequest is fully pointer-based), so saving one section never
 * clobbers another. The enable checkbox itself lives in the always-reachable
 * System Settings view (this view is only reachable once it's on).
 *
 * Sections 2–4 are GATED on a working, SAVED admin connection (adminConnectionOk):
 * a successful token-status read against the stored token proves the admin API is
 * reachable. Until then those sections are locked (inputs disabled + a hint) so an
 * operator sets up + saves the admin connection first.
 */
export function NetbirdSettings({
  t,
  api,
}: Readonly<{
  t: Translation;
  api: Pick<
    PortalApi,
    | 'createGatewaySetupKey'
    | 'enrollGatewaySidecar'
    | 'getSystemSettings'
    | 'netbirdGroups'
    | 'netbirdNetwork'
    | 'netbirdPeers'
    | 'netbirdStatus'
    | 'netbirdTokenStatus'
    | 'rotateNetbirdToken'
    | 'servers'
    | 'testNetbird'
    | 'updateNetbirdNetwork'
    | 'updateSystemSettings'
  >;
}>) {
  const { showSuccess, showError } = useToast();
  const {
    data: settings,
    setData: setSettings,
    loading,
    error,
  } = useResource(() => api.getSystemSettings(), [api, t], t);
  useEffect(() => {
    if (error) showError(error);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [error]);
  const [busy, setBusy] = useState(false);
  // A working, SAVED admin connection reaches the NetBird admin API (proven by a
  // successful token-status read against the STORED token). Sections 2–4 are
  // locked until this is true. Re-probed on load + after an Admin save (statusNonce).
  const [adminConnectionOk, setAdminConnectionOk] = useState(false);
  // NetBird-only transport: the runtime enforcement toggle + the selected gateway
  // peer (the agent-listener bind target; takes effect on restart).
  const [pendingNetbirdOnly, setPendingNetbirdOnly] = useState<boolean | null>(null);
  const [pendingNetbirdGatewayPeerId, setPendingNetbirdGatewayPeerId] = useState<string | null>(
    null,
  );
  // Desired name for the gateway peer — applied automatically before/after enroll
  // (rename via NetBird). Empty = no rename.
  const [pendingNetbirdGatewayPeerName, setPendingNetbirdGatewayPeerName] = useState<string | null>(
    null,
  );
  // Gateway-peer picker options, loaded once the module is enabled + a token is
  // stored (empty on any error → the picker just shows no options, never crashes).
  const [netbirdPeerOptions, setNetbirdPeerOptions] = useState<NetbirdPeerOption[]>([]);
  // Peer ids already linked to an AI-server — the gateway-peer picker greys these out
  // + annotates them (mirrors the server linkage editor). Empty on any error.
  const [linkedServerPeerIds, setLinkedServerPeerIds] = useState<Set<string>>(new Set());
  // Confirm dialog before a gateway-peer change is saved (agents may lose the gateway).
  const [confirmingPeerSave, setConfirmingPeerSave] = useState(false);
  // Live transport status (null = not loaded / call failed → the block is hidden).
  // statusNonce is bumped after a save so the block reflects the just-saved state.
  const [netbirdStatus, setNetbirdStatus] = useState<NetbirdStatus | null>(null);
  const [statusNonce, setStatusNonce] = useState(0);
  // Admin-token validity display + rotation. netbirdTokenStatus is loaded/reloaded
  // on the same statusNonce as the transport status above (bumped after Save AND
  // after a rotation). The new token's value is NEVER stored/rendered.
  const [netbirdTokenStatus, setNetbirdTokenStatus] = useState<NetbirdTokenStatus | null>(null);
  const [rotating, setRotating] = useState(false);
  const [confirmingRotate, setConfirmingRotate] = useState(false);
  const [pendingNetbirdTokenRotateBeforeDays, setPendingNetbirdTokenRotateBeforeDays] = useState<
    string | null
  >(null);
  // Gateway setup-key mint: the display-once reveal ("" = closed) + its command +
  // an in-flight guard while the POST runs.
  const [gatewayKey, setGatewayKey] = useState('');
  const [gatewayKeyCommand, setGatewayKeyCommand] = useState('');
  const [creatingGatewayKey, setCreatingGatewayKey] = useState(false);
  // Sidecar self-enroll: mints a key + writes it to the shared volume so the
  // custom NetBird sidecar enrolls itself. An in-flight guard while the POST runs.
  const [enrollingSidecar, setEnrollingSidecar] = useState(false);
  // Re-enroll confirm: when a gateway peer already exists, both the setup-key
  // mint AND the sidecar enroll first ask for confirmation ("" = no pending
  // action; the dialog is closed). Holds WHICH action to run on confirm.
  const [confirmingReenroll, setConfirmingReenroll] = useState<'setup-key' | 'sidecar' | ''>('');
  // NetBird policy management: least-privilege access-policy maintenance + its
  // scope/deny-by-default posture + the two loop cadences. netbird_effective_policy_scope
  // is READ-ONLY (server-derived) and has no pending state — it is never sent back.
  const [pendingNetbirdManagePolicies, setPendingNetbirdManagePolicies] = useState<boolean | null>(
    null,
  );
  const [pendingNetbirdPolicyScope, setPendingNetbirdPolicyScope] = useState<string | null>(null);
  const [pendingNetbirdDenyByDefault, setPendingNetbirdDenyByDefault] = useState<boolean | null>(
    null,
  );
  const [pendingNetbirdDenyByDefaultEnforce, setPendingNetbirdDenyByDefaultEnforce] = useState<
    boolean | null
  >(null);
  const [pendingNetbirdPeerSyncInterval, setPendingNetbirdPeerSyncInterval] = useState<
    string | null
  >(null);
  const [pendingNetbirdReconcileInterval, setPendingNetbirdReconcileInterval] = useState<
    string | null
  >(null);
  // Ping-allow settings (the ping ACTION itself lives in the Tools view).
  const [pendingNetbirdAllowPingGateway, setPendingNetbirdAllowPingGateway] = useState<
    boolean | null
  >(null);
  const [pendingNetbirdAllowPingAllServers, setPendingNetbirdAllowPingAllServers] = useState<
    boolean | null
  >(null);
  // Module-config fields (Admin section). The enable checkbox itself stays in
  // System Settings (always reachable), so the module can be turned on before this
  // view becomes reachable; these fields live here since this view is only
  // reachable once the checkbox is on.
  const [pendingNetbirdUrl, setPendingNetbirdUrl] = useState<string | null>(null);
  // The module-level group names as a multiselect list. pendingNetbirdGroups holds
  // the edited list (Autocomplete path); pendingNetbirdGroupsCsv holds the raw
  // comma-separated text used by the load-error fallback field (split on save).
  const [pendingNetbirdGroups, setPendingNetbirdGroups] = useState<string[] | null>(null);
  const [pendingNetbirdGroupsCsv, setPendingNetbirdGroupsCsv] = useState<string | null>(null);
  // Write-only token: "" = keep (untouched), a typed value = replace, cleared flag = send "".
  const [netbirdTokenInput, setNetbirdTokenInput] = useState('');
  const [netbirdTokenCleared, setNetbirdTokenCleared] = useState(false);
  const [netbirdTesting, setNetbirdTesting] = useState(false);
  // Group picker: the names of the groups configured in NetBird, loaded lazily
  // once the module is enabled + a token is stored. A load error → groupsError,
  // which falls the picker back to a plain (still editable) text field. Never
  // blocks Save.
  const [netbirdGroupOptions, setNetbirdGroupOptions] = useState<string[]>([]);
  const [netbirdGroupsError, setNetbirdGroupsError] = useState(false);
  // Test-without-save: after a successful "Verbindung testen" with unsaved Admin
  // changes, we ask whether to persist them (then run the Admin save on confirm).
  const [confirmingTestSave, setConfirmingTestSave] = useState(false);

  // Module-config derived values. The enable checkbox itself lives in System
  // Settings — this view only READS settings?.netbird_enabled (no pending state
  // of its own for it) to gate Save/rendering.
  const netbirdEnabled = settings?.netbird_enabled ?? false;
  const netbirdUrl = pendingNetbirdUrl ?? settings?.netbird_url ?? '';
  const netbirdGroups = pendingNetbirdGroups ?? settings?.netbird_groups ?? [];
  // The CSV fallback field shows the current list joined; typing edits the raw text
  // (parsed on save). Falls back to the joined list until the operator touches it.
  const netbirdGroupsCsv = pendingNetbirdGroupsCsv ?? netbirdGroups.join(', ');
  const netbirdTokenPresent =
    (settings?.netbird_token_set && !netbirdTokenCleared) || netbirdTokenInput !== '';
  // When the module is enabled, the backend requires a URL + a token (stored or
  // typed). Gate the Admin Save so we never fire a guaranteed-400.
  const netbirdConfigOk = !netbirdEnabled || (netbirdUrl.trim() !== '' && netbirdTokenPresent);
  // Auto-rotation threshold (days before expiry); 0 = auto-rotation off. Defaults
  // to the server-reported value (itself env/KV-defaulted to 14).
  const netbirdTokenRotateBeforeDaysValue =
    pendingNetbirdTokenRotateBeforeDays ?? String(settings?.netbird_token_rotate_before_days ?? 14);
  const netbirdTokenRotateBeforeDaysNum = Number(netbirdTokenRotateBeforeDaysValue);

  const netbirdOnly = pendingNetbirdOnly ?? settings?.netbird_only ?? false;
  // Restrict the agent-token curl download to the NetBird network (the portal
  // file download stays available regardless).
  const [pendingNetbirdAgentDownloadOnly, setPendingNetbirdAgentDownloadOnly] = useState<
    boolean | null
  >(null);
  const netbirdAgentDownloadOnly =
    pendingNetbirdAgentDownloadOnly ?? settings?.netbird_agent_download_only ?? false;
  const netbirdGatewayPeerId =
    pendingNetbirdGatewayPeerId ?? settings?.netbird_gateway_peer_id ?? '';
  const selectedGatewayPeer = netbirdPeerOptions.find((p) => p.id === netbirdGatewayPeerId) ?? null;
  // Peers linked to an AI-server, EXCLUDING the current gateway peer (so it stays
  // selectable) — disabled + annotated "(already linked)" in the picker below.
  const linkedServerPeersElsewhere = new Set(
    [...linkedServerPeerIds].filter((id) => id !== netbirdGatewayPeerId),
  );
  const netbirdGatewayPeerName =
    pendingNetbirdGatewayPeerName ?? settings?.netbird_gateway_peer_name ?? '';
  // The FIELD shows the CURRENT (live) peer name first (what the operator sees
  // right now), falling back to the stored desired name, then empty. Save keeps
  // sending netbirdGatewayPeerName above (the desired/wish name) — NOT this live-
  // first value — so a no-touch Save never accidentally pins the live name as the
  // desired name. Once the operator types, pendingNetbirdGatewayPeerName is
  // non-null and both this display value AND the save payload show the edit.
  const netbirdGatewayPeerNameField =
    pendingNetbirdGatewayPeerName ??
    (netbirdStatus?.gateway_peer_name || settings?.netbird_gateway_peer_name || '');

  // NetBird policy management.
  const netbirdManagePolicies =
    pendingNetbirdManagePolicies ?? settings?.netbird_manage_policies ?? false;
  const netbirdPolicyScope = pendingNetbirdPolicyScope ?? settings?.netbird_policy_scope ?? 'auto';
  // Read-only, server-derived — resolves "auto" against deny-by-default. Never
  // has a pending state and is never included in the save payload.
  const netbirdEffectivePolicyScope = settings?.netbird_effective_policy_scope ?? 'selected';
  let policyScopeHelperText: string | undefined;
  if (netbirdPolicyScope === 'auto') {
    const effectiveLabel =
      netbirdEffectivePolicyScope === 'all'
        ? t.settingsNetbirdPolicyScopeAll
        : t.settingsNetbirdPolicyScopeSelected;
    policyScopeHelperText = t.settingsNetbirdPolicyScopeEffective(effectiveLabel);
  }
  const netbirdDenyByDefault =
    pendingNetbirdDenyByDefault ?? settings?.netbird_deny_by_default ?? false;
  const netbirdDenyByDefaultEnforce =
    pendingNetbirdDenyByDefaultEnforce ?? settings?.netbird_deny_by_default_enforce ?? false;
  const netbirdAllowPingGateway =
    pendingNetbirdAllowPingGateway ?? settings?.netbird_allow_ping_gateway ?? false;
  const netbirdAllowPingAllServers =
    pendingNetbirdAllowPingAllServers ?? settings?.netbird_allow_ping_all_servers ?? false;
  const netbirdPeerSyncIntervalValue =
    pendingNetbirdPeerSyncInterval ?? String(settings?.netbird_peer_sync_interval_seconds ?? 30);
  const netbirdPeerSyncIntervalNum = Number(netbirdPeerSyncIntervalValue);
  const netbirdReconcileIntervalValue =
    pendingNetbirdReconcileInterval ?? String(settings?.netbird_reconcile_interval_seconds ?? 60);
  const netbirdReconcileIntervalNum = Number(netbirdReconcileIntervalValue);
  // Per-section interval validation (the two intervals live in different sections /
  // PUTs, §7): each validates against the last-SAVED value of the other. The
  // backend system.netbird_interval_order 400 remains the cross-section backstop.
  const savedReconcile = settings?.netbird_reconcile_interval_seconds ?? 60;
  const savedPeerSync = settings?.netbird_peer_sync_interval_seconds ?? 30;
  const peerSyncValid =
    Number.isInteger(netbirdPeerSyncIntervalNum) &&
    netbirdPeerSyncIntervalNum >= 10 &&
    netbirdPeerSyncIntervalNum <= savedReconcile;
  const reconcileValid =
    Number.isInteger(netbirdReconcileIntervalNum) &&
    netbirdReconcileIntervalNum >= 10 &&
    netbirdReconcileIntervalNum >= savedPeerSync;

  // Admin section is dirty when any of its pending fields differs from the saved
  // value (drives the test-without-save "save now?" prompt).
  const adminDirty =
    pendingNetbirdUrl !== null ||
    pendingNetbirdGroups !== null ||
    pendingNetbirdGroupsCsv !== null ||
    netbirdTokenInput !== '' ||
    netbirdTokenCleared ||
    pendingNetbirdPeerSyncInterval !== null ||
    pendingNetbirdTokenRotateBeforeDays !== null;

  // Sections 2–4 are locked until a saved admin connection works.
  const locked = !adminConnectionOk;

  // Load the NetBird group names for the picker once the module is enabled + a
  // token is stored (the groups endpoint needs both). A failure falls the picker
  // back to a plain text field; it never blocks saving.
  useEffect(() => {
    if (!settings?.netbird_enabled || !settings?.netbird_token_set) return;
    let cancelled = false;
    (async () => {
      try {
        const res = await api.netbirdGroups();
        if (!cancelled) {
          setNetbirdGroupOptions(res.data.map((g) => g.name).filter((n) => n !== ''));
          setNetbirdGroupsError(false);
        }
      } catch {
        if (!cancelled) setNetbirdGroupsError(true);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [api, settings?.netbird_enabled, settings?.netbird_token_set]);

  // Load the NetBird peers for the gateway-peer picker once the module is enabled +
  // a token is stored. On ANY error the options stay empty (the picker still renders)
  // — never blocks saving. Reloaded after each save (statusNonce) so a just-applied
  // peer rename shows the CURRENT name in the dropdown's option labels.
  useEffect(() => {
    if (!settings?.netbird_enabled || !settings?.netbird_token_set) return;
    let cancelled = false;
    (async () => {
      try {
        const res = await api.netbirdPeers();
        if (!cancelled) setNetbirdPeerOptions(res.data ?? []);
      } catch {
        if (!cancelled) setNetbirdPeerOptions([]);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [api, settings?.netbird_enabled, settings?.netbird_token_set, statusNonce]);

  // Load the AI-server list so the gateway-peer picker can grey out + annotate peers
  // already linked to a server (mirrors the server linkage editor's peer picker). On
  // any error the set stays empty (nothing greyed out) — never blocks the picker.
  useEffect(() => {
    if (!settings?.netbird_enabled || !settings?.netbird_token_set) return;
    let cancelled = false;
    (async () => {
      try {
        const res = await api.servers();
        if (!cancelled) {
          setLinkedServerPeerIds(
            new Set((res.data ?? []).map((s) => s.netbird_peer_id).filter((id) => id !== '')),
          );
        }
      } catch {
        if (!cancelled) setLinkedServerPeerIds(new Set());
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [api, settings?.netbird_enabled, settings?.netbird_token_set]);

  // Load the live transport status on mount + after each save (statusNonce). On any
  // error the block is hidden (netbirdStatus = null) — the status endpoint is
  // best-effort diagnostics, never blocks the settings page.
  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const s = await api.netbirdStatus();
        if (!cancelled) setNetbirdStatus(s);
      } catch {
        if (!cancelled) setNetbirdStatus(null);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [api, statusNonce]);

  // Load the current admin token's live validity once the module is enabled + a
  // token is stored (mirrors the groups/peers load gate above). statusNonce
  // (bumped after Save AND after a rotation) triggers a reload. A SUCCESSFUL read
  // means the STORED token reached the NetBird admin API ⇒ adminConnectionOk. On
  // any error the line falls back to "unknown" AND the admin connection is marked
  // not-OK (sections 2–4 lock). If the module is not enabled/token-set, there is
  // no connection to probe (locked, no call).
  useEffect(() => {
    if (!settings?.netbird_enabled || !settings?.netbird_token_set) {
      setAdminConnectionOk(false);
      return;
    }
    let cancelled = false;
    (async () => {
      try {
        const s = await api.netbirdTokenStatus();
        if (!cancelled) {
          setNetbirdTokenStatus(s);
          setAdminConnectionOk(true);
        }
      } catch {
        if (!cancelled) {
          setNetbirdTokenStatus(null);
          setAdminConnectionOk(false);
        }
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [api, settings?.netbird_enabled, settings?.netbird_token_set, statusNonce]);

  // Shared: PUT a partial settings body + adopt the returned settings.
  async function putSettings(body: Record<string, unknown>) {
    const updated = await api.updateSystemSettings(body);
    setSettings(updated);
    return updated;
  }

  // A peer-sync interval > the reconcile interval (or either below the 10s floor)
  // is rejected by the backend even though each section already blocks Save for it
  // — this is the defense-in-depth backstop (e.g. a stale form / cross-section).
  function handleSaveError(err: unknown) {
    if (err instanceof PortalApiError && err.code === 'system.netbird_interval_order') {
      showError(t.settingsNetbirdIntervalOrder);
    } else {
      showError(formatPortalError(err, t));
    }
  }

  // Admin-Verbindung save: url / groups / write-only token / peer-sync interval /
  // auto-rotate threshold. On success re-probes adminConnectionOk + the token/
  // transport status (statusNonce).
  async function saveAdmin() {
    setBusy(true);
    try {
      // NetBird token: send "" to clear, the typed value to replace, otherwise omit (keep).
      let netbirdToken: string | undefined;
      if (netbirdTokenCleared) {
        netbirdToken = '';
      } else if (netbirdTokenInput !== '') {
        netbirdToken = netbirdTokenInput;
      } else {
        netbirdToken = undefined;
      }
      // In the load-error fallback the list comes from the CSV text (split/trim);
      // otherwise it is the multiselect list. Both are sent as a string[].
      const netbirdGroupsToSave = netbirdGroupsError
        ? netbirdGroupsCsv
            .split(',')
            .map((s) => s.trim())
            .filter((s) => s !== '')
        : netbirdGroups;
      await putSettings({
        netbird_url: netbirdUrl,
        netbird_groups: netbirdGroupsToSave,
        ...(netbirdToken !== undefined ? { netbird_token: netbirdToken } : {}),
        netbird_peer_sync_interval_seconds: netbirdPeerSyncIntervalNum,
        netbird_token_rotate_before_days: Number.isFinite(netbirdTokenRotateBeforeDaysNum)
          ? netbirdTokenRotateBeforeDaysNum
          : 0,
      });
      setPendingNetbirdUrl(null);
      setPendingNetbirdGroups(null);
      setPendingNetbirdGroupsCsv(null);
      setNetbirdTokenInput('');
      setNetbirdTokenCleared(false);
      setPendingNetbirdPeerSyncInterval(null);
      setPendingNetbirdTokenRotateBeforeDays(null);
      setStatusNonce((n) => n + 1); // re-probe adminConnectionOk + token/transport status
      showSuccess(t.systemSaved);
    } catch (err) {
      handleSaveError(err);
    } finally {
      setBusy(false);
    }
  }

  // The gateway-peer id/name is about to be saved. If either changed vs the saved
  // state, confirm first (changing the gateway peer can drop the ServerAgents'
  // connection to the gateway + require an agent-config change); an unchanged save
  // (e.g. only netbird_only toggled) runs directly.
  function savePeer() {
    const idChanged = netbirdGatewayPeerId !== (settings?.netbird_gateway_peer_id ?? '');
    const nameChanged = netbirdGatewayPeerName !== (settings?.netbird_gateway_peer_name ?? '');
    if (idChanged || nameChanged) {
      setConfirmingPeerSave(true);
    } else {
      void doSavePeer();
    }
  }

  // Peer-Einstellungen save: netbird_only + gateway-peer id + gateway-peer name.
  async function doSavePeer() {
    setBusy(true);
    try {
      await putSettings({
        netbird_only: netbirdOnly,
        netbird_gateway_peer_id: netbirdGatewayPeerId,
        netbird_gateway_peer_name: netbirdGatewayPeerName,
        netbird_agent_download_only: netbirdAgentDownloadOnly,
      });
      setPendingNetbirdOnly(null);
      setPendingNetbirdGatewayPeerId(null);
      setPendingNetbirdGatewayPeerName(null);
      setPendingNetbirdAgentDownloadOnly(null);
      setStatusNonce((n) => n + 1); // refresh the transport status block
      showSuccess(t.systemSaved);
    } catch (err) {
      handleSaveError(err);
    } finally {
      setBusy(false);
    }
  }

  // Policies-Einstellungen save: manage/scope/deny/enforce + ping-allow + reconcile
  // interval. netbird_effective_policy_scope is server-derived and never sent.
  async function savePolicies() {
    setBusy(true);
    try {
      await putSettings({
        netbird_manage_policies: netbirdManagePolicies,
        netbird_policy_scope: netbirdPolicyScope,
        netbird_deny_by_default: netbirdDenyByDefault,
        netbird_deny_by_default_enforce: netbirdDenyByDefaultEnforce,
        netbird_allow_ping_gateway: netbirdAllowPingGateway,
        netbird_allow_ping_all_servers: netbirdAllowPingAllServers,
        netbird_reconcile_interval_seconds: netbirdReconcileIntervalNum,
      });
      setPendingNetbirdManagePolicies(null);
      setPendingNetbirdPolicyScope(null);
      setPendingNetbirdDenyByDefault(null);
      setPendingNetbirdDenyByDefaultEnforce(null);
      setPendingNetbirdReconcileInterval(null);
      setPendingNetbirdAllowPingGateway(null);
      setPendingNetbirdAllowPingAllServers(null);
      showSuccess(t.systemSaved);
    } catch (err) {
      handleSaveError(err);
    } finally {
      setBusy(false);
    }
  }

  // Tests the CURRENTLY ENTERED (possibly unsaved) admin credentials — no prior
  // Save required. Sends url always; sends token only when the operator typed a
  // new value (replace) or cleared it (""), otherwise omits it (⇒ the stored
  // token). On success with unsaved Admin changes, offers to persist them.
  async function testNetbird() {
    setNetbirdTesting(true);
    try {
      let tokenOverride: string | undefined;
      if (netbirdTokenCleared) {
        tokenOverride = '';
      } else if (netbirdTokenInput !== '') {
        tokenOverride = netbirdTokenInput;
      } else {
        tokenOverride = undefined;
      }
      const res = await api.testNetbird({
        url: netbirdUrl,
        ...(tokenOverride !== undefined ? { token: tokenOverride } : {}),
      });
      if (res.ok) {
        showSuccess(t.settingsNetbirdTestOk);
        if (adminDirty) setConfirmingTestSave(true);
      } else {
        showError(t.settingsNetbirdTestFailed(res.error ?? ''));
      }
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setNetbirdTesting(false);
    }
  }

  // Creates a fresh NetBird admin API token, verifies it, switches to it, and
  // best-effort deletes the old one (rollback on any failure — the old token
  // stays active, per the backend contract). The new token's value is NEVER
  // shown. Reloads the validity line on success via statusNonce.
  async function rotate() {
    setRotating(true);
    try {
      const res = await api.rotateNetbirdToken();
      const base = t.settingsNetbirdRotateOk(res.expiration_date, res.days_remaining);
      let tail = '';
      if (res.old_deleted) {
        tail = t.settingsNetbirdRotateOldDeleted;
      } else if (res.old_unknown) {
        tail = t.settingsNetbirdRotateOldUnknown;
      }
      showSuccess(tail ? `${base} ${tail}` : base);
      setStatusNonce((n) => n + 1);
    } catch {
      showError(t.settingsNetbirdRotateFailed);
    } finally {
      setRotating(false);
    }
  }

  function requestRotate() {
    setConfirmingRotate(true);
  }
  function confirmRotate() {
    setConfirmingRotate(false);
    void rotate();
  }

  // Mints a one-off setup key for the gateway's own NetBird peer and opens the
  // display-once reveal. On failure only a toast is shown (no reveal), and the
  // revealed values are reset when the dialog closes (no stale key in state).
  async function createGatewaySetupKey() {
    setCreatingGatewayKey(true);
    try {
      const res = await api.createGatewaySetupKey();
      setGatewayKey(res.setup_key);
      setGatewayKeyCommand(res.netbird_setup_command);
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setCreatingGatewayKey(false);
    }
  }

  // Mints a setup key AND writes it to the shared-volume key file so the custom
  // NetBird sidecar self-enrolls. On success a toast confirms the write and the
  // display-once reveal opens as a copy-paste fallback. A 409
  // netbird.key_file_not_configured (OP_AI_GATEWAY_NETBIRD_KEY_FILE unset) → a
  // specific "see the runbook" toast; any other error → the generic error toast.
  async function enrollGatewaySidecar() {
    setEnrollingSidecar(true);
    try {
      const res = await api.enrollGatewaySidecar();
      setGatewayKey(res.setup_key);
      setGatewayKeyCommand(res.netbird_setup_command);
      showSuccess(t.settingsNetbirdSidecarEnrolled);
    } catch (err) {
      if (err instanceof PortalApiError && err.code === 'netbird.key_file_not_configured') {
        showError(t.settingsNetbirdSidecarNoKeyFile);
      } else {
        showError(formatPortalError(err, t));
      }
    } finally {
      setEnrollingSidecar(false);
    }
  }

  // Both mint actions ask for confirmation first when a gateway peer already
  // exists (re-enroll could change the current configuration); with no peer
  // (first-time setup) they run directly.
  function requestGatewaySetupKey() {
    if (netbirdStatus?.gateway_peer_id) {
      setConfirmingReenroll('setup-key');
    } else {
      void createGatewaySetupKey();
    }
  }
  function requestSidecarEnroll() {
    if (netbirdStatus?.gateway_peer_id) {
      setConfirmingReenroll('sidecar');
    } else {
      void enrollGatewaySidecar();
    }
  }
  function confirmReenroll() {
    const action = confirmingReenroll;
    setConfirmingReenroll('');
    if (action === 'setup-key') void createGatewaySetupKey();
    else if (action === 'sidecar') void enrollGatewaySidecar();
  }

  return (
    <>
      <PageTitle title={t.settingsNetbirdTitle} />
      {loading && <p>{t.loading}</p>}
      <Stack spacing={2.5}>
        {/* 1. Admin-Verbindung: the module-config (url / write-only token / test /
            token rotation + threshold / group names / peer-sync interval). Never
            locked — this is the section that establishes the admin connection. */}
        <Panel
          titleId="netbird-admin-heading"
          title={t.settingsNetbirdSectionAdmin}
          subtitle={t.settingsNetbirdIntro}
        >
          <Stack spacing={3}>
            <Field
              id="netbird-url"
              label={t.settingsNetbirdUrl}
              value={netbirdUrl}
              onChange={(e) => setPendingNetbirdUrl(e.target.value)}
            />
            <Box>
              <Field
                id="netbird-token"
                type="password"
                label={t.settingsNetbirdToken}
                value={netbirdTokenCleared ? '' : netbirdTokenInput}
                onChange={(e) => {
                  setNetbirdTokenInput(e.target.value);
                  setNetbirdTokenCleared(false);
                }}
                autoComplete="new-password"
                placeholder={
                  settings?.netbird_token_set && !netbirdTokenCleared
                    ? t.settingsNetbirdTokenSet
                    : undefined
                }
                helperText={t.settingsNetbirdTokenNote}
              />
              {settings?.netbird_token_set && !netbirdTokenCleared && (
                <Button
                  type="button"
                  size="small"
                  variant="text"
                  color="secondary"
                  onClick={() => {
                    setNetbirdTokenCleared(true);
                    setNetbirdTokenInput('');
                  }}
                >
                  {t.settingsNetbirdTokenClear}
                </Button>
              )}
            </Box>
            <Box>
              <Button
                type="button"
                variant="outlined"
                disabled={netbirdTesting || busy}
                onClick={testNetbird}
              >
                {t.settingsNetbirdTest}
              </Button>
            </Box>

            {/* Admin-token validity + rotation. Needs a saved module config (enabled +
                a stored token); the new token's value is NEVER shown. */}
            {settings?.netbird_enabled && settings?.netbird_token_set && (
              <Box>
                <Typography
                  variant="body2"
                  color={
                    netbirdTokenStatus?.known && netbirdTokenStatus.days_remaining <= 14
                      ? 'warning.main'
                      : 'text.secondary'
                  }
                >
                  {netbirdTokenStatus?.known
                    ? t.settingsNetbirdTokenValid(
                        netbirdTokenStatus.expiration_date,
                        netbirdTokenStatus.days_remaining,
                      )
                    : t.settingsNetbirdTokenValidUnknown}
                </Typography>
                <Button
                  type="button"
                  variant="outlined"
                  disabled={busy || rotating}
                  onClick={requestRotate}
                  sx={{ mt: 1 }}
                >
                  {t.settingsNetbirdRotate}
                </Button>
              </Box>
            )}

            <Field
              id="netbird-token-rotate-before"
              type="number"
              label={t.settingsNetbirdTokenRotateBefore}
              value={netbirdTokenRotateBeforeDaysValue}
              onChange={(e) => setPendingNetbirdTokenRotateBeforeDays(e.target.value)}
              helperText={t.settingsNetbirdTokenRotateBeforeHelp}
              inputProps={{ min: 0, step: 1 }}
            />

            {netbirdGroupsError ? (
              // Load failed (module not reachable / auth) → a plain, still-editable
              // comma-separated text field so group names can be typed before the
              // module works (split/trimmed into the list on save).
              <Field
                id="netbird-group"
                label={t.settingsNetbirdGroups}
                value={netbirdGroupsCsv}
                onChange={(e) => setPendingNetbirdGroupsCsv(e.target.value)}
              />
            ) : (
              // multiple + freeSolo: pick existing groups OR type new names (created
              // on the next server enroll). The stored values stay the group NAMES.
              <Autocomplete
                multiple
                freeSolo
                id="netbird-group"
                options={netbirdGroupOptions}
                value={netbirdGroups}
                onChange={(_event, value) => setPendingNetbirdGroups(value as string[])}
                size="small"
                fullWidth
                renderInput={(params) => <TextField {...params} label={t.settingsNetbirdGroups} />}
              />
            )}

            <Field
              id="netbird-peer-sync-interval"
              type="number"
              label={t.settingsNetbirdPeerInterval}
              value={netbirdPeerSyncIntervalValue}
              onChange={(e) => setPendingNetbirdPeerSyncInterval(e.target.value)}
              error={!peerSyncValid}
              helperText={
                peerSyncValid ? t.settingsNetbirdPeerIntervalHelp : t.settingsNetbirdIntervalError
              }
              inputProps={{ min: 10, step: 1 }}
            />
          </Stack>
          <Box sx={{ display: 'flex', justifyContent: 'flex-end', mt: 2 }}>
            <Button
              type="button"
              variant="contained"
              disabled={busy || loading || !netbirdConfigOk || !peerSyncValid}
              onClick={saveAdmin}
            >
              {t.save}
            </Button>
          </Box>
        </Panel>

        {/* 2. Netzwerk: a live editor of the NetBird account's network settings
            (writes the account directly, NOT system_settings). */}
        <NetbirdNetworkPanel t={t} api={api} disabled={locked} />

        {/* 3. Peer-Einstellungen: netbird_only transport, gateway-peer linkage,
            setup-key/sidecar enroll + live transport status. */}
        <Panel titleId="netbird-peer-heading" title={t.settingsNetbirdSectionPeer}>
          {locked && (
            <Alert severity="info" sx={{ mb: 2 }}>
              {t.settingsNetbirdAdminRequired}
            </Alert>
          )}
          <Stack spacing={3}>
            {/* NetBird-only transport: the runtime enforcement toggle. */}
            <Box>
              <FormControlLabel
                control={
                  <Switch
                    checked={netbirdOnly}
                    onChange={(e) => setPendingNetbirdOnly(e.target.checked)}
                    disabled={locked}
                  />
                }
                label={t.settingsNetbirdOnly}
              />
              <FormHelperText sx={{ ml: 0, mt: 0.25 }}>{t.settingsNetbirdOnlyHelp}</FormHelperText>
            </Box>

            {/* Restrict the agent-token curl download to the NetBird network (the
                portal file download stays available regardless). */}
            <Box>
              <FormControlLabel
                control={
                  <Switch
                    checked={netbirdAgentDownloadOnly}
                    onChange={(e) => setPendingNetbirdAgentDownloadOnly(e.target.checked)}
                    disabled={locked}
                  />
                }
                label={t.settingsNetbirdAgentDownloadOnly}
              />
              <FormHelperText sx={{ ml: 0, mt: 0.25 }}>
                {t.settingsNetbirdAgentDownloadOnlyHint}
              </FormHelperText>
            </Box>

            {/* Gateway-peer picker: which NetBird peer represents the gateway (the
                agent-listener bind target). Options load once the module is enabled +
                a token is stored; selecting one stores its peer id. Takes effect on
                the next gateway restart (caption). */}
            <Box>
              <Autocomplete<NetbirdPeerOption>
                id="netbird-gateway-peer"
                options={netbirdPeerOptions}
                size="small"
                fullWidth
                autoHighlight
                disabled={locked}
                value={selectedGatewayPeer}
                getOptionLabel={(o) => (o.dns_label ? `${o.name} — ${o.dns_label}` : o.name)}
                isOptionEqualToValue={(o, v) => o.id === v.id}
                getOptionDisabled={(o) => linkedServerPeersElsewhere.has(o.id)}
                onChange={(_e, next) => setPendingNetbirdGatewayPeerId(next ? next.id : '')}
                renderOption={(props, option) => {
                  const { key, ...rest } = props as { key?: string } & Record<string, unknown>;
                  const linked = linkedServerPeersElsewhere.has(option.id);
                  return (
                    <Box
                      component="li"
                      key={key}
                      {...rest}
                      sx={{ display: 'flex', gap: 0.75, whiteSpace: 'nowrap' }}
                    >
                      <Box component="span">
                        {option.dns_label ? `${option.name} — ${option.dns_label}` : option.name}
                      </Box>
                      {option.connected ? (
                        <Box component="span" sx={{ color: 'success.main', fontSize: '0.8em' }}>
                          ({t.serverNetbirdConnected})
                        </Box>
                      ) : null}
                      {linked ? (
                        <Box component="span" sx={{ color: 'text.disabled', fontSize: '0.8em' }}>
                          ({t.serverNetbirdPeerLinked})
                        </Box>
                      ) : null}
                    </Box>
                  );
                }}
                renderInput={(params) => (
                  <TextField {...params} label={t.settingsNetbirdGatewayPeer} />
                )}
              />
              <FormHelperText sx={{ ml: 0, mt: 0.25 }}>
                {t.settingsNetbirdGatewayPeerRestartHelp}
              </FormHelperText>
            </Box>

            {/* Desired name for the gateway peer, applied automatically before/after
                enroll (rename via NetBird). Empty = no rename. */}
            <Field
              id="netbird-gateway-peer-name"
              label={t.settingsNetbirdGatewayPeerName}
              value={netbirdGatewayPeerNameField}
              onChange={(e) => setPendingNetbirdGatewayPeerName(e.target.value)}
              helperText={t.settingsNetbirdGatewayPeerNameHelp}
              disabled={locked}
            />

            {/* Gateway setup-key mint: needs the SAVED module config (enabled + a
                stored token) — the same inner gate as the peer-picker option load —
                plus the panel lock. */}
            {settings?.netbird_enabled && settings?.netbird_token_set && (
              <Box sx={{ display: 'flex', gap: 1.5, flexWrap: 'wrap' }}>
                <Button
                  type="button"
                  variant="outlined"
                  disabled={locked || busy || creatingGatewayKey}
                  onClick={requestGatewaySetupKey}
                >
                  {t.settingsNetbirdGatewayKeyCreate}
                </Button>
                {/* "Sidecar enrollen" only when an autonomous-enroll sidecar is wired
                    (a shared key file is configured, reported by the status endpoint). */}
                {netbirdStatus?.sidecar_enroll_available && (
                  <Button
                    type="button"
                    variant="outlined"
                    disabled={locked || busy || enrollingSidecar}
                    onClick={requestSidecarEnroll}
                  >
                    {t.settingsNetbirdSidecarEnroll}
                  </Button>
                )}
              </Box>
            )}

            {/* Read-only transport status: is the NetBird-bound agent listener up +
                is the selected gateway peer connected. When netbird_only is ON but no
                listener is active, a prominent warning that inbound isolation is NOT
                yet in effect. Hidden entirely if the status call failed. */}
            {netbirdStatus && (
              <Box>
                <Typography variant="subtitle2" sx={{ mb: 0.5 }}>
                  {t.settingsNetbirdStatusTitle}
                </Typography>
                <Typography variant="body2" color="text.secondary">
                  {netbirdStatus.agent_listener_active
                    ? t.settingsNetbirdListenerActive(netbirdStatus.agent_listener_addr)
                    : t.settingsNetbirdListenerInactive}
                </Typography>
                {netbirdStatus.gateway_peer_id !== '' && (
                  <Typography variant="body2" color="text.secondary">
                    {netbirdStatus.gateway_peer_connected
                      ? t.settingsNetbirdGatewayPeerConnected
                      : t.settingsNetbirdGatewayPeerDisconnected}
                  </Typography>
                )}
                {netbirdStatus.netbird_only && !netbirdStatus.agent_listener_active && (
                  <Alert severity="warning" sx={{ mt: 1 }}>
                    {t.settingsNetbirdOnlyNoListenerWarning}
                  </Alert>
                )}
              </Box>
            )}
          </Stack>
          <Box sx={{ display: 'flex', justifyContent: 'flex-end', mt: 2 }}>
            <Button
              type="button"
              variant="contained"
              disabled={locked || busy || loading}
              onClick={savePeer}
            >
              {t.save}
            </Button>
          </Box>
        </Panel>

        {/* 4. Policies-Einstellungen: least-privilege access-policy maintenance +
            posture + ping-allow toggles + reconcile interval. */}
        <Panel titleId="netbird-policies-heading" title={t.settingsNetbirdSectionPolicies}>
          {locked && (
            <Alert severity="info" sx={{ mb: 2 }}>
              {t.settingsNetbirdAdminRequired}
            </Alert>
          )}
          <Stack spacing={3}>
            <Box>
              <FormControlLabel
                control={
                  <Switch
                    checked={netbirdManagePolicies}
                    onChange={(e) => setPendingNetbirdManagePolicies(e.target.checked)}
                    disabled={locked}
                  />
                }
                label={t.settingsNetbirdManagePolicies}
              />
              <FormHelperText sx={{ ml: 0, mt: 0.25 }}>
                {t.settingsNetbirdManagePoliciesHelp}
              </FormHelperText>
            </Box>

            <SelectField
              id="netbird-policy-scope"
              label={t.settingsNetbirdPolicyScope}
              value={netbirdPolicyScope}
              onChange={(e) => setPendingNetbirdPolicyScope(e.target.value)}
              disabled={locked}
              helperText={policyScopeHelperText}
            >
              <option value="auto">{t.settingsNetbirdPolicyScopeAuto}</option>
              <option value="all">{t.settingsNetbirdPolicyScopeAll}</option>
              <option value="selected">{t.settingsNetbirdPolicyScopeSelected}</option>
            </SelectField>

            <Box>
              <FormControlLabel
                control={
                  <Switch
                    checked={netbirdDenyByDefault}
                    onChange={(e) => setPendingNetbirdDenyByDefault(e.target.checked)}
                    disabled={locked}
                  />
                }
                label={t.settingsNetbirdDenyByDefault}
              />
              <FormHelperText sx={{ ml: 0, mt: 0.25 }}>
                {t.settingsNetbirdDenyByDefaultHelp}
              </FormHelperText>
              {netbirdDenyByDefault && (
                <Alert severity="warning" sx={{ mt: 1 }}>
                  {t.settingsNetbirdDenyByDefaultWarn}
                </Alert>
              )}
              {netbirdDenyByDefault && !netbirdManagePolicies && (
                <Alert severity="warning" sx={{ mt: 1 }}>
                  {t.settingsNetbirdPolicyCouplingWarn}
                </Alert>
              )}
            </Box>

            <Box>
              <FormControlLabel
                control={
                  <Switch
                    checked={netbirdDenyByDefaultEnforce}
                    onChange={(e) => setPendingNetbirdDenyByDefaultEnforce(e.target.checked)}
                    disabled={locked || !netbirdDenyByDefault}
                  />
                }
                label={t.settingsNetbirdDenyEnforce}
              />
              <FormHelperText sx={{ ml: 0, mt: 0.25 }}>
                {t.settingsNetbirdDenyEnforceHelp}
              </FormHelperText>
            </Box>

            {/* ICMP ping-allow: keep the gateway peer pingable / all servers
                pingable from the gateway (managed op-gw-ping-* policies). */}
            <Box>
              <FormControlLabel
                control={
                  <Switch
                    checked={netbirdAllowPingGateway}
                    onChange={(e) => setPendingNetbirdAllowPingGateway(e.target.checked)}
                    disabled={locked}
                  />
                }
                label={t.settingsNetbirdAllowPingGateway}
              />
              <FormHelperText sx={{ ml: 0, mt: 0.25 }}>
                {t.settingsNetbirdAllowPingGatewayHelp}
              </FormHelperText>
            </Box>

            <Box>
              <FormControlLabel
                control={
                  <Switch
                    checked={netbirdAllowPingAllServers}
                    onChange={(e) => setPendingNetbirdAllowPingAllServers(e.target.checked)}
                    disabled={locked}
                  />
                }
                label={t.settingsNetbirdAllowPingAllServers}
              />
              <FormHelperText sx={{ ml: 0, mt: 0.25 }}>
                {t.settingsNetbirdAllowPingAllServersHelp}
              </FormHelperText>
            </Box>

            <Field
              id="netbird-reconcile-interval"
              type="number"
              label={t.settingsNetbirdReconcileInterval}
              value={netbirdReconcileIntervalValue}
              onChange={(e) => setPendingNetbirdReconcileInterval(e.target.value)}
              error={!reconcileValid}
              helperText={
                reconcileValid
                  ? t.settingsNetbirdReconcileIntervalHelp
                  : t.settingsNetbirdIntervalError
              }
              inputProps={{ min: 10, step: 1 }}
              disabled={locked}
            />
          </Stack>
          <Box sx={{ display: 'flex', justifyContent: 'flex-end', mt: 2 }}>
            <Button
              type="button"
              variant="contained"
              disabled={locked || busy || loading || !reconcileValid}
              onClick={savePolicies}
            >
              {t.save}
            </Button>
          </Box>
        </Panel>
      </Stack>

      {/* Display-once reveal for the freshly minted gateway setup key + the ready
          netbird up command. Resetting both on close leaves no stale key in state. */}
      <Dialog
        open={gatewayKey !== ''}
        onClose={() => {
          setGatewayKey('');
          setGatewayKeyCommand('');
        }}
        maxWidth="md"
        fullWidth
      >
        <DialogTitle>{t.settingsNetbirdGatewayKeyTitle}</DialogTitle>
        <DialogContent>
          <SecretReveal
            title={t.settingsNetbirdGatewayKeyTitle}
            copyValue={gatewayKey}
            copyLabel={t.settingsNetbirdGatewayKeyTitle}
          >
            <code>{gatewayKey}</code>
          </SecretReveal>
          {gatewayKeyCommand && (
            <SecretReveal
              title={t.serverNetbirdSetupCommand}
              copyValue={gatewayKeyCommand}
              copyLabel={t.serverNetbirdSetupCommand}
            >
              <code>{gatewayKeyCommand}</code>
            </SecretReveal>
          )}
          <Typography variant="body2" sx={{ mt: 1 }}>
            {t.settingsNetbirdGatewayKeyHelp}
          </Typography>
        </DialogContent>
        <DialogActions>
          <Button
            onClick={() => {
              setGatewayKey('');
              setGatewayKeyCommand('');
            }}
          >
            {t.captureClose}
          </Button>
        </DialogActions>
      </Dialog>

      {/* Gateway-peer change confirm: changing the selected peer (its NetBird IP) or
          its name (its dns_label) can drop the ServerAgents' connection to the gateway;
          their config (gateway_url) may need adjusting. Shown only on a real change. */}
      <ConfirmDialog
        open={confirmingPeerSave}
        title={t.settingsNetbirdGatewayPeerChangeConfirmTitle}
        body={t.settingsNetbirdGatewayPeerChangeConfirmBody}
        confirmLabel={t.settingsNetbirdGatewayPeerChangeConfirmAction}
        cancelLabel={t.cancel}
        onConfirm={() => {
          setConfirmingPeerSave(false);
          void doSavePeer();
        }}
        onCancel={() => setConfirmingPeerSave(false)}
      />

      {/* Re-enroll confirm: shown only when a gateway peer already exists (a
          fresh setup has none, so the mint runs directly without prompting). */}
      <ConfirmDialog
        open={confirmingReenroll !== ''}
        title={t.settingsNetbirdReenrollConfirmTitle}
        body={t.settingsNetbirdReenrollConfirmBody}
        confirmLabel={t.settingsNetbirdReenrollConfirmAction}
        cancelLabel={t.cancel}
        onConfirm={confirmReenroll}
        onCancel={() => setConfirmingReenroll('')}
      />

      {/* Rotate-token confirm: creates a fresh token, verifies + switches to it,
          and best-effort deletes the old one; the new token's value is never shown. */}
      <ConfirmDialog
        open={confirmingRotate}
        title={t.settingsNetbirdRotateConfirmTitle}
        body={t.settingsNetbirdRotateConfirmBody}
        confirmLabel={t.settingsNetbirdRotateConfirmAction}
        cancelLabel={t.cancel}
        onConfirm={confirmRotate}
        onCancel={() => setConfirmingRotate(false)}
      />

      {/* Test-without-save: after a successful test with unsaved Admin changes,
          offer to persist them (running the Admin save on confirm). */}
      <ConfirmDialog
        open={confirmingTestSave}
        title={t.settingsNetbirdTestSaveConfirmTitle}
        body={t.settingsNetbirdTestSaveConfirmBody}
        confirmLabel={t.settingsNetbirdTestSaveConfirmAction}
        cancelLabel={t.cancel}
        onConfirm={() => {
          setConfirmingTestSave(false);
          void saveAdmin();
        }}
        onCancel={() => setConfirmingTestSave(false)}
      />
    </>
  );
}
