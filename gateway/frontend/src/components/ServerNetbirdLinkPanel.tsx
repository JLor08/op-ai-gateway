// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useEffect, useState, type ReactNode } from 'react';
import {
  Alert,
  Autocomplete,
  Box,
  Button,
  Checkbox,
  FormControlLabel,
  TextField,
  Typography,
} from '@mui/material';
import type { PortalServer } from '../api';
import { PortalApiError } from '../api';
import type { Translation, PortalApi } from './shared/types';
import { formatPortalError } from './shared/format';
import { Panel } from './shared/Panel';
import { Field } from './shared/Field';
import { StatusChip } from './shared/StatusChip';
import { useToast } from './shared/ToastProvider';
import { ServerPolicyOverrides } from './ServerPolicyOverrides';

// A NetBird peer as offered by the linkage-editor peer picker (the four fields the
// system-scoped /api/system/netbird/peers endpoint leaks).
type NetbirdPeerOption = { id: string; name: string; dns_label: string; connected: boolean };

// A NetBird policy group (id + display name) — offered by the linkage-editor group
// multiselect and the DTO's netbird_group_ids mirror (tracking group excluded).
type NetbirdGroupRef = { id: string; name: string };

function netbirdLinkStatus(server: PortalServer, t: Translation): ReactNode {
  if (server.netbird_peer_id === '') {
    return <StatusChip status="watch" label={t.serverNetbirdNotRegistered} />;
  }
  if (server.netbird_connected) {
    return <StatusChip status="active" label={t.serverNetbirdConnected} />;
  }
  return <StatusChip status="error" label={t.serverNetbirdDisconnected} />;
}

type ServerNetbirdLinkPanelProps = {
  t: Translation;
  api: Pick<
    PortalApi,
    | 'netbirdPeers'
    | 'netbirdGroups'
    | 'setServerNetbird'
    | 'setServerCertificateOverride'
    | 'setServerHTTPSSwitchOverride'
  >;
  // The server being edited — this panel is system-admin + edit-only, so
  // always defined at the call site.
  server: PortalServer;
  // Peer ids already linked to ANOTHER server (excludes this one) — so the
  // picker can disable/annotate them and the admin avoids the 409.
  linkedElsewhere: Set<string>;
  // Whether "Alle Server pingbar" (netbird_allow_ping_all_servers) is on
  // system-wide — flips the per-server ping control from an opt-in to a RED
  // opt-out.
  pingAllServers: boolean;
  // The EFFECTIVE policy scope ("all" / "selected"), or null when not loaded /
  // load failed (hides the per-server policy-override control entirely).
  netbirdEffectiveScope: string | null;
  // Whether "Nur NetBird-Transport" is on system-wide — drives a warning that
  // an excluded/unmanaged server may become unreachable.
  netbirdOnly: boolean;
  // Passed straight through to the nested ServerPolicyOverrides (same
  // system-scoped settings fetch as the props above).
  certEnabled: boolean;
  certServerScope: string | null;
  httpsSwitchMode: string | null;
  // Called after a successful save (this panel's own, or the nested
  // ServerPolicyOverrides') with the fresh server DTO, so the caller can
  // update its `servers` list + edit-mode context.
  onSaved: (updated: PortalServer) => void;
};

// System-admin NetBird linkage editor: link a manually-created peer to a
// server (the peer id + the netbird-enabled flag + policy groups + the
// per-server policy/ping overrides), plus the nested certificate/https-switch
// overrides (ServerPolicyOverrides) which share this panel's Panel/grid.
export function ServerNetbirdLinkPanel({
  t,
  api,
  server,
  linkedElsewhere,
  pingAllServers,
  netbirdEffectiveScope,
  netbirdOnly,
  certEnabled,
  certServerScope,
  httpsSwitchMode,
  onSaved,
}: Readonly<ServerNetbirdLinkPanelProps>) {
  const { showError, showSuccess } = useToast();
  const [netbirdLinkPeerId, setNetbirdLinkPeerId] = useState(() => server.netbird_peer_id ?? '');
  const [netbirdLinkEnabled, setNetbirdLinkEnabled] = useState(() => server.netbird_enabled);
  // "Treat as gateway-created peer" — governs the delete pre-selection and whether
  // the setup key may be regenerated.
  const [netbirdLinkPeerManaged, setNetbirdLinkPeerManaged] = useState(
    () => server.netbird_peer_managed,
  );
  const [netbirdLinkBusy, setNetbirdLinkBusy] = useState(false);
  // Peer picker: the peers offered by NetBird + whether the load failed. On a
  // load error the dropdown is hidden and the manual peer-id field remains the
  // source of truth.
  const [netbirdPeers, setNetbirdPeers] = useState<NetbirdPeerOption[]>([]);
  const [netbirdPeersFailed, setNetbirdPeersFailed] = useState(false);
  // Group multiselect: the policy groups offered by NetBird (options) + the
  // peer's selected groups (init from the server's mirror; pushed to NetBird
  // on save). The tracking group is excluded server-side.
  const [netbirdGroupOptions, setNetbirdGroupOptions] = useState<NetbirdGroupRef[]>([]);
  const [netbirdLinkGroups, setNetbirdLinkGroups] = useState<NetbirdGroupRef[]>(
    () => server.netbird_group_ids ?? [],
  );
  // Per-server policy-management opt-in/opt-out override ("" / "include" /
  // "exclude"); sent as the trailing arg to setServerNetbird on save.
  const [netbirdLinkPolicyOverride, setNetbirdLinkPolicyOverride] = useState(
    () => server.netbird_policy_override ?? '',
  );
  // Whether the gateway may ICMP-ping this server (managed op-gw-ping-servers
  // policy); sent as the trailing arg to setServerNetbird on save.
  const [netbirdLinkAllowPing, setNetbirdLinkAllowPing] = useState(() => server.netbird_allow_ping);
  // Per-server ping OPT-OUT (netbird_ping_exclude), the counterpart shown when
  // "Alle Server pingbar" is on system-wide; mutually exclusive with
  // allow-ping; sent as the trailing arg to setServerNetbird on save.
  const [netbirdLinkPingExclude, setNetbirdLinkPingExclude] = useState(
    () => server.netbird_ping_exclude,
  );

  // Lazily load the NetBird peers for the picker. Re-runs whenever `server`
  // gets a fresh reference (any of this edit session's dedicated saves, not
  // just this panel's own, replace it) — on ANY error (module off, endpoint
  // missing, network) hide the dropdown and fall back to the manual peer-id
  // field — never crash.
  useEffect(() => {
    let cancelled = false;
    setNetbirdPeersFailed(false);
    (async () => {
      try {
        const res = await api.netbirdPeers();
        if (!cancelled) setNetbirdPeers(res.data ?? []);
      } catch {
        if (!cancelled) {
          setNetbirdPeers([]);
          setNetbirdPeersFailed(true);
        }
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [api, server]);

  // Lazily load the NetBird policy groups for the group multiselect. On error
  // the options stay empty (the pre-filled chips from the server's mirror
  // still render) — never crash.
  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const res = await api.netbirdGroups();
        if (!cancelled) setNetbirdGroupOptions(res.data ?? []);
      } catch {
        if (!cancelled) setNetbirdGroupOptions([]);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [api, server]);

  // System-admin linkage editor save: set the netbird-enabled flag + peer id.
  async function saveNetbirdLink() {
    setNetbirdLinkBusy(true);
    try {
      const updated = await api.setServerNetbird(
        server.id,
        netbirdLinkEnabled,
        netbirdLinkPeerId.trim(),
        netbirdLinkGroups.map((g) => g.id),
        netbirdLinkPeerManaged,
        netbirdLinkPolicyOverride,
        netbirdLinkAllowPing,
        netbirdLinkPingExclude,
      );
      onSaved(updated);
      // Keep the editable fields in sync with the fresh DTO (the read-only
      // group/key/connected + the group mirror reflect immediately).
      setNetbirdLinkPeerId(updated.netbird_peer_id ?? '');
      setNetbirdLinkEnabled(updated.netbird_enabled);
      setNetbirdLinkPeerManaged(updated.netbird_peer_managed);
      setNetbirdLinkGroups(updated.netbird_group_ids ?? []);
      setNetbirdLinkPolicyOverride(updated.netbird_policy_override ?? '');
      setNetbirdLinkAllowPing(updated.netbird_allow_ping);
      setNetbirdLinkPingExclude(updated.netbird_ping_exclude);
      showSuccess(t.serverNetbirdLinkSaved);
    } catch (err) {
      // A peer id already linked to another server → a clear, specific toast (the
      // editor stays open so the admin can pick a different peer).
      if (err instanceof PortalApiError && err.code === 'netbird.peer_in_use') {
        showError(t.serverNetbirdPeerInUse);
      } else {
        showError(formatPortalError(err, t));
      }
    } finally {
      setNetbirdLinkBusy(false);
    }
  }

  // The peer currently named by the (editable) peer-id field, if it is one of the
  // offered options — drives the domain hint. A manually-typed id not in the list
  // yields null (the field stays the source of truth).
  const selectedPeer = netbirdPeers.find((p) => p.id === netbirdLinkPeerId) ?? null;
  // The policy-group multiselect is only meaningful once the peer is enrolled
  // (no peer id → nothing to push groups to). Disable it + show a note.
  const netbirdGroupsUnenrolled = server.netbird_peer_id === '';

  return (
    <Panel titleId="server-netbird-link-heading" title={t.serverNetbirdLinkTitle}>
      <Box sx={{ display: 'grid', gridTemplateColumns: 'minmax(260px, 480px)', gap: 2.25 }}>
        <FormControlLabel
          control={
            <Checkbox
              checked={netbirdLinkEnabled}
              onChange={(e) => setNetbirdLinkEnabled(e.target.checked)}
            />
          }
          label={t.serverNetbirdEnabledLabel}
        />
        {/* "Treat as gateway-created peer": governs the delete pre-selection
            and whether the setup key may be regenerated. Pre-filled from the
            server's netbird_peer_managed; sent as the trailing save arg. */}
        <Box>
          <FormControlLabel
            control={
              <Checkbox
                checked={netbirdLinkPeerManaged}
                onChange={(e) => setNetbirdLinkPeerManaged(e.target.checked)}
              />
            }
            label={t.serverNetbirdPeerManagedLabel}
          />
          <Typography variant="caption" color="text.secondary" sx={{ display: 'block' }}>
            {t.serverNetbirdPeerManagedHelp}
          </Typography>
        </Box>
        {/* Per-server ping control, MODE-AWARE (mirrors the policy override):
            when "Alle Server pingbar" is on system-wide it flips from an
            opt-in ("Ping erlauben", netbird_allow_ping) to a RED opt-out
            ("Ping … NICHT erlauben", netbird_ping_exclude). The two flags are
            mutually exclusive (checking one clears the other). */}
        {pingAllServers ? (
          <FormControlLabel
            control={
              <Checkbox
                color="error"
                checked={netbirdLinkPingExclude}
                onChange={(e) => {
                  setNetbirdLinkPingExclude(e.target.checked);
                  if (e.target.checked) setNetbirdLinkAllowPing(false); // mutual exclusivity
                }}
              />
            }
            label={
              <Typography component="span" sx={{ color: 'error.main' }}>
                {t.serverNetbirdPingExclude}
              </Typography>
            }
          />
        ) : (
          <FormControlLabel
            control={
              <Checkbox
                checked={netbirdLinkAllowPing}
                onChange={(e) => {
                  setNetbirdLinkAllowPing(e.target.checked);
                  if (e.target.checked) setNetbirdLinkPingExclude(false); // mutual exclusivity
                }}
              />
            }
            label={t.serverNetbirdAllowPing}
          />
        )}
        {/* Peer picker: offer the NetBird peers (name — dns_label, connected
            hint; already-linked peers disabled). Selecting one fills the
            editable peer-id field below (which stays the source of truth so a
            manual id still works). Hidden when the peers failed to load. */}
        {!netbirdPeersFailed && (
          <Box>
            <Autocomplete<NetbirdPeerOption>
              id="netbird-link-peer-pick"
              options={netbirdPeers}
              size="small"
              fullWidth
              autoHighlight
              value={selectedPeer}
              getOptionLabel={(o) => (o.dns_label ? `${o.name} — ${o.dns_label}` : o.name)}
              isOptionEqualToValue={(o, v) => o.id === v.id}
              getOptionDisabled={(o) => linkedElsewhere.has(o.id)}
              onChange={(_e, next) => {
                if (next) setNetbirdLinkPeerId(next.id);
              }}
              renderOption={(props, option) => {
                const { key, ...rest } = props as { key?: string } & Record<string, unknown>;
                const linked = linkedElsewhere.has(option.id);
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
              renderInput={(params) => <TextField {...params} label={t.serverNetbirdPeerPick} />}
            />
            {selectedPeer?.dns_label ? (
              <Typography
                variant="caption"
                color="text.secondary"
                sx={{ mt: 0.5, display: 'block' }}
              >
                {t.serverNetbirdPeerDomainHint(selectedPeer.dns_label)}
              </Typography>
            ) : null}
          </Box>
        )}
        <Field
          id="netbird-link-peer"
          label={t.serverNetbirdPeerId}
          value={netbirdLinkPeerId}
          onChange={(e) => setNetbirdLinkPeerId(e.target.value)}
        />
        {/* Policy-group multiselect: pick the NetBird groups the peer belongs
            to (pre-filled from the DTO's mirror, tracking group excluded
            server-side). Disabled until the peer is enrolled — pushed to
            NetBird on save. */}
        <Box>
          <Autocomplete<NetbirdGroupRef, true>
            multiple
            id="netbird-link-groups"
            options={netbirdGroupOptions}
            size="small"
            fullWidth
            disabled={netbirdGroupsUnenrolled}
            value={netbirdLinkGroups}
            getOptionLabel={(o) => o.name || o.id}
            isOptionEqualToValue={(o, v) => o.id === v.id}
            onChange={(_e, next) => setNetbirdLinkGroups(next)}
            renderInput={(params) => <TextField {...params} label={t.serverNetbirdGroups} />}
          />
          {netbirdGroupsUnenrolled ? (
            <Typography variant="caption" color="text.secondary" sx={{ mt: 0.5, display: 'block' }}>
              {t.serverNetbirdGroupsUnenrolled}
            </Typography>
          ) : null}
        </Box>
        {/* When "Nur NetBird-Transport" is on system-wide, a server that is
            excluded from (or not covered by) policy management gets no
            automatic access rule, so the gateway may lose reachability. */}
        {netbirdOnly && (
          <Alert severity="warning" sx={{ mt: 1 }}>
            {t.serverNetbirdOnlyPolicyWarning}
          </Alert>
        )}
        {/* Per-server policy-management opt-in/opt-out override. Which control
            is shown (and what it means) follows the EFFECTIVE scope: in "all"
            scope every server is managed unless it opts OUT; in "selected"
            scope only a server that opts IN is managed. Hidden entirely when
            the effective scope failed to load. */}
        {netbirdEffectiveScope === 'all' && (
          <Box>
            <FormControlLabel
              control={
                <Checkbox
                  color="error"
                  checked={netbirdLinkPolicyOverride === 'exclude'}
                  onChange={(e) => setNetbirdLinkPolicyOverride(e.target.checked ? 'exclude' : '')}
                />
              }
              label={
                <Typography component="span" sx={{ color: 'error.main' }}>
                  {t.serverNetbirdPolicyExclude}
                </Typography>
              }
            />
            <Typography variant="caption" color="text.secondary" sx={{ display: 'block' }}>
              {t.serverNetbirdPolicyExcludeHelp}
            </Typography>
          </Box>
        )}
        {netbirdEffectiveScope === 'selected' && (
          <Box>
            <FormControlLabel
              control={
                <Checkbox
                  checked={netbirdLinkPolicyOverride === 'include'}
                  onChange={(e) => setNetbirdLinkPolicyOverride(e.target.checked ? 'include' : '')}
                />
              }
              label={t.serverNetbirdPolicyInclude}
            />
            <Typography variant="caption" color="text.secondary" sx={{ display: 'block' }}>
              {t.serverNetbirdPolicyIncludeHelp}
            </Typography>
          </Box>
        )}
        <ServerPolicyOverrides
          t={t}
          api={api}
          server={server}
          certEnabled={certEnabled}
          certServerScope={certServerScope}
          httpsSwitchMode={httpsSwitchMode}
          onSaved={onSaved}
        />
        <Field
          id="netbird-link-group"
          label={t.serverNetbirdGroupId}
          value={server.netbird_group_id}
          onChange={() => {}}
          disabled
        />
        <Field
          id="netbird-link-key"
          label={t.serverNetbirdSetupKeyId}
          value={server.netbird_setup_key_id}
          onChange={() => {}}
          disabled
        />
        <Box>{netbirdLinkStatus(server, t)}</Box>
        <Box>
          <Button
            type="button"
            variant="contained"
            disabled={netbirdLinkBusy}
            onClick={saveNetbirdLink}
          >
            {t.serverNetbirdLinkSave}
          </Button>
        </Box>
      </Box>
    </Panel>
  );
}
