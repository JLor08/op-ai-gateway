// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useEffect, useState } from 'react';
import { Autocomplete, Box, Button, Stack, TextField } from '@mui/material';
import { ConfirmDialog } from '../shared/ConfirmDialog';
import { Field } from '../shared/Field';
import { Panel } from '../shared/Panel';
import { PortalApiError } from '../../api';
import type { NetbirdNetwork } from '../../api';
import type { PortalApi, Translation } from '../shared/types';
import { formatPortalError } from '../shared/format';
import { useToast } from '../shared/ToastProvider';

type GroupOption = { id: string; name: string };

/**
 * Globale NetBird-Konto-Netzwerkeinstellungen (DNS-Domain, IPv4-/IPv6-CIDR,
 * IPv6-Aktivierungsgruppen) — betreffen ALLE Peers, daher hinter einem
 * ConfirmDialog beim Speichern. `disabled` (das Modul ist noch nicht
 * konfiguriert/aktiv) unterdrückt den Fetch komplett und sperrt die Felder.
 */
export function NetbirdNetworkPanel({
  t,
  api,
  disabled,
}: Readonly<{
  t: Translation;
  api: Pick<PortalApi, 'netbirdGroups' | 'netbirdNetwork' | 'updateNetbirdNetwork'>;
  disabled: boolean;
}>) {
  const { showSuccess, showError } = useToast();
  const [net, setNet] = useState<NetbirdNetwork | null>(null);
  const [groupOptions, setGroupOptions] = useState<GroupOption[]>([]);
  const [busy, setBusy] = useState(false);
  const [confirming, setConfirming] = useState(false);

  useEffect(() => {
    if (disabled) return;
    let cancelled = false;
    (async () => {
      try {
        const n = await api.netbirdNetwork();
        if (!cancelled) setNet(n);
      } catch (err) {
        if (!cancelled) showError(formatPortalError(err, t));
      }
      try {
        const res = await api.netbirdGroups();
        if (!cancelled) setGroupOptions(res.data);
      } catch {
        if (!cancelled) setGroupOptions([]);
      }
    })();
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [api, disabled]);

  function patch(p: Partial<NetbirdNetwork>) {
    setNet((cur) => (cur ? { ...cur, ...p } : cur));
  }

  async function save() {
    if (!net) return;
    setBusy(true);
    try {
      const updated = await api.updateNetbirdNetwork(net);
      setNet(updated);
      showSuccess(t.settingsNetbirdNetworkSaved);
    } catch (err) {
      if (err instanceof PortalApiError && err.code === 'system.netbird_network_range_invalid') {
        showError(t.settingsNetbirdNetworkRangeInvalid);
      } else {
        showError(formatPortalError(err, t));
      }
    } finally {
      setBusy(false);
    }
  }

  const selectedGroups = groupOptions.filter((g) => net?.ipv6_enabled_groups.includes(g.id));

  return (
    <Panel
      titleId="netbird-network-heading"
      title={t.settingsNetbirdSectionNetwork}
      subtitle={t.settingsNetbirdSectionNetworkIntro}
    >
      <Stack spacing={3}>
        <Field
          id="netbird-dns-domain"
          label={t.settingsNetbirdDnsDomain}
          value={net?.dns_domain ?? ''}
          onChange={(e) => patch({ dns_domain: e.target.value })}
          disabled={disabled || !net}
        />
        <Field
          id="netbird-network-range"
          label={t.settingsNetbirdNetworkRange}
          value={net?.network_range ?? ''}
          onChange={(e) => patch({ network_range: e.target.value })}
          disabled={disabled || !net}
        />
        <Field
          id="netbird-network-range-v6"
          label={t.settingsNetbirdNetworkRangeV6}
          value={net?.network_range_v6 ?? ''}
          onChange={(e) => patch({ network_range_v6: e.target.value })}
          disabled={disabled || !net}
        />
        <Autocomplete<GroupOption, true>
          multiple
          id="netbird-ipv6-groups"
          options={groupOptions}
          value={selectedGroups}
          getOptionLabel={(o) => o.name}
          isOptionEqualToValue={(o, v) => o.id === v.id}
          onChange={(_e, value) => patch({ ipv6_enabled_groups: value.map((g) => g.id) })}
          size="small"
          fullWidth
          disabled={disabled || !net}
          renderInput={(params) => (
            <TextField
              {...params}
              label={t.settingsNetbirdIPv6Groups}
              helperText={t.settingsNetbirdIPv6GroupsHelp}
            />
          )}
        />
        <Box sx={{ display: 'flex', justifyContent: 'flex-end' }}>
          <Button
            type="button"
            variant="contained"
            disabled={disabled || busy || !net}
            onClick={() => setConfirming(true)}
          >
            {t.save}
          </Button>
        </Box>
      </Stack>
      <ConfirmDialog
        open={confirming}
        title={t.settingsNetbirdNetworkSaveConfirmTitle}
        body={t.settingsNetbirdNetworkSaveConfirmBody}
        confirmLabel={t.settingsNetbirdNetworkSaveConfirmAction}
        cancelLabel={t.cancel}
        onConfirm={() => {
          setConfirming(false);
          void save();
        }}
        onCancel={() => setConfirming(false)}
      />
    </Panel>
  );
}
