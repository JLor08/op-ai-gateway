// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useState } from 'react';
import { Box, Checkbox, FormControlLabel, Typography } from '@mui/material';
import type { PortalServer } from '../api';
import type { Translation, PortalApi } from './shared/types';
import { formatPortalError } from './shared/format';
import { useToast } from './shared/ToastProvider';

type ServerPolicyOverridesProps = {
  t: Translation;
  api: Pick<PortalApi, 'setServerCertificateOverride' | 'setServerHTTPSSwitchOverride'>;
  // The server being edited — this panel only ever renders inside the
  // system-admin NetBird linkage editor (edit-only), so always defined.
  server: PortalServer;
  // Whether the TLS-certificate module is enabled + its configured server scope
  // ("all" / "selected") — system-admin-only settings, fetched by the caller
  // alongside the NetBird effective policy scope (same system-scoped settings
  // fetch) — drives which control (opt-out vs opt-in) is shown, or none at all
  // when unloaded/disabled.
  certEnabled: boolean;
  certServerScope: string | null;
  // The global P4 https-auto-switch mode ("manual" / "auto" / "selected"),
  // loaded alongside certEnabled/certServerScope (same system-scoped settings
  // fetch) — drives which control (opt-out vs opt-in vs none) is shown.
  httpsSwitchMode: string | null;
  // Called after a successful save with the fresh server DTO, so the caller
  // can update its `servers` list + edit-mode context.
  onSaved: (updated: PortalServer) => void;
};

// Certificate-management + https-auto-switch per-server opt-in/opt-out override
// controls, saved IMMEDIATELY on toggle (their own dedicated endpoints,
// independent of the main linkage-editor Save button) -- mirrors the
// instant-toggle pattern used for group-manager permission checkboxes
// elsewhere in the portal.
export function ServerPolicyOverrides({
  t,
  api,
  server,
  certEnabled,
  certServerScope,
  httpsSwitchMode,
  onSaved,
}: Readonly<ServerPolicyOverridesProps>) {
  const { showError } = useToast();
  // Per-server certificate-management opt-in/opt-out override ("" / "include" /
  // "exclude"), init from the server's certificate_override.
  const [certificateOverride, setCertificateOverride] = useState(
    () => server.certificate_override ?? '',
  );
  // Per-server https-auto-switch opt-in/opt-out override ("" / "include" /
  // "exclude", P4 Task 11) -- mirrors certificateOverride exactly. Unchecking
  // ALWAYS writes "" (never the opposite value), so a later flip of the global
  // cert_https_switch_mode can never resurrect a stale include/exclude.
  const [httpsSwitchOverride, setHttpsSwitchOverride] = useState(
    () => server.https_switch_override ?? '',
  );

  async function saveCertificateOverride(value: string) {
    setCertificateOverride(value);
    try {
      const updated = await api.setServerCertificateOverride(server.id, value);
      onSaved(updated);
    } catch (err) {
      showError(formatPortalError(err, t));
    }
  }

  async function saveHttpsSwitchOverride(value: string) {
    setHttpsSwitchOverride(value);
    try {
      const updated = await api.setServerHTTPSSwitchOverride(server.id, value);
      onSaved(updated);
    } catch (err) {
      showError(formatPortalError(err, t));
    }
  }

  return (
    <>
      {/* Per-server certificate-management opt-in/opt-out override, MODE-AWARE:
          "all" scope -> a RED opt-out; "selected" scope -> a normal opt-in. Only
          rendered when the certificate module is on AND this server is in the
          NetBird network (certificates are only issued for NetBird-internal
          names). Saved immediately on toggle. */}
      {certEnabled && server.netbird_enabled && certServerScope === 'all' && (
        <Box>
          <FormControlLabel
            control={
              <Checkbox
                color="error"
                checked={certificateOverride === 'exclude'}
                onChange={(e) => void saveCertificateOverride(e.target.checked ? 'exclude' : '')}
              />
            }
            label={
              <Typography component="span" sx={{ color: 'error.main' }}>
                {t.serverCertificateExclude}
              </Typography>
            }
          />
        </Box>
      )}
      {certEnabled && server.netbird_enabled && certServerScope === 'selected' && (
        <Box>
          <FormControlLabel
            control={
              <Checkbox
                checked={certificateOverride === 'include'}
                onChange={(e) => void saveCertificateOverride(e.target.checked ? 'include' : '')}
              />
            }
            label={t.serverCertificateInclude}
          />
        </Box>
      )}
      {/* Per-server https-auto-switch opt-in/opt-out override (P4 Task 11),
          MODE-AWARE like the certificate override above: "auto" mode -> a RED
          opt-out; "selected" mode -> a normal opt-in; "manual" mode (or the
          settings load failing) renders nothing at all. Saved immediately on
          toggle. */}
      {httpsSwitchMode === 'auto' && (
        <Box>
          <FormControlLabel
            control={
              <Checkbox
                color="error"
                checked={httpsSwitchOverride === 'exclude'}
                onChange={(e) => void saveHttpsSwitchOverride(e.target.checked ? 'exclude' : '')}
              />
            }
            label={
              <Typography component="span" sx={{ color: 'error.main' }}>
                {t.serverHTTPSSwitchExclude}
              </Typography>
            }
          />
        </Box>
      )}
      {httpsSwitchMode === 'selected' && (
        <Box>
          <FormControlLabel
            control={
              <Checkbox
                checked={httpsSwitchOverride === 'include'}
                onChange={(e) => void saveHttpsSwitchOverride(e.target.checked ? 'include' : '')}
              />
            }
            label={t.serverHTTPSSwitchInclude}
          />
        </Box>
      )}
    </>
  );
}
