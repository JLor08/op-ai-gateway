// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useEffect, useState } from 'react';
import { Alert, Box, Button, Stack } from '@mui/material';
import type { PortalApi, Translation } from './shared/types';
import { PageTitle } from './shared/PageTitle';
import { Panel } from './shared/Panel';
import { SelectField } from './shared/SelectField';

/**
 * Admin-facing "Tools" view. Currently holds the Server-Ping tool (moved out of
 * System Settings): runs a real unprivileged ICMP echo from the gateway to a
 * picked AI-server, independent of the NetBird module.
 */
export function ToolsView({
  t,
  api,
}: Readonly<{
  t: Translation;
  api: Pick<PortalApi, 'pingServer' | 'servers'>;
}>) {
  const [pingServers, setPingServers] = useState<{ id: string; name: string }[]>([]);
  const [pingServerId, setPingServerId] = useState('');
  const [pinging, setPinging] = useState(false);
  const [pingResult, setPingResult] = useState<{ ok: boolean; text: string } | null>(null);

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const res = await api.servers();
        if (!cancelled) setPingServers(res.data.map((s) => ({ id: s.id, name: s.name })));
      } catch {
        /* leave the picker empty on a load error */
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [api]);

  async function pingServer() {
    if (!pingServerId) return;
    setPinging(true);
    setPingResult(null);
    try {
      const res = await api.pingServer(pingServerId);
      if (res.ok) setPingResult({ ok: true, text: t.settingsPingOk(res.latency_ms ?? 0) });
      else setPingResult({ ok: false, text: t.settingsPingFailed(res.error ?? '') });
    } catch {
      setPingResult({ ok: false, text: t.settingsPingFailed('') });
    } finally {
      setPinging(false);
    }
  }

  return (
    <>
      <PageTitle title={t.tools} subtitle={t.toolsIntro} />
      <Stack spacing={3}>
        <Panel titleId="tools-ping-heading" title={t.settingsPingTitle}>
          <Box sx={{ display: 'flex', gap: 1.5, flexWrap: 'wrap', alignItems: 'flex-end' }}>
            <SelectField
              id="tools-ping-server"
              label={t.settingsPingServerLabel}
              value={pingServerId}
              onChange={(e) => {
                setPingServerId(e.target.value);
                setPingResult(null);
              }}
            >
              <option value=""></option>
              {pingServers.map((s) => (
                <option key={s.id} value={s.id}>
                  {s.name}
                </option>
              ))}
            </SelectField>
            <Button
              type="button"
              variant="outlined"
              disabled={pinging || !pingServerId}
              onClick={pingServer}
            >
              {t.settingsPingButton}
            </Button>
          </Box>
          {pingResult && (
            <Alert severity={pingResult.ok ? 'success' : 'error'} sx={{ mt: 1 }}>
              {pingResult.text}
            </Alert>
          )}
        </Panel>
      </Stack>
    </>
  );
}
