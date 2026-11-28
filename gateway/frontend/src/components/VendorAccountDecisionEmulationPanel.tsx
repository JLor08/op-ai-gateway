// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useState } from 'react';
import { Box, Checkbox, FormControlLabel, Typography } from '@mui/material';
import type { VendorAccount } from '../api';
import type { PortalApi, Translation } from './shared/types';
import { formatPortalError } from './shared/format';
import { Panel } from './shared/Panel';
import { useToast } from './shared/ToastProvider';

/**
 * The owner's opt-in (#380) to answer the decision endpoints by emulation on an
 * OpenAI subscription account's chat model. Offered only to the owner of such an
 * account: the backend refuses every other vendor, auth type and caller.
 */
export function VendorAccountDecisionEmulationPanel({
  t,
  api,
  account,
  disabled,
  onChanged,
}: Readonly<{
  t: Translation;
  api: Pick<PortalApi, 'setVendorAccountDecisionEmulation'>;
  account: VendorAccount;
  disabled: boolean;
  onChanged: (updated: VendorAccount) => void;
}>) {
  const { showError, showSuccess } = useToast();
  const [busy, setBusy] = useState(false);

  if (!account.is_owner || account.vendor !== 'openai' || account.auth_type !== 'subscription') {
    return null;
  }

  async function toggle(enabled: boolean) {
    setBusy(true);
    try {
      const updated = await api.setVendorAccountDecisionEmulation(account.id, enabled);
      showSuccess(t.vendorAccountDecisionEmulationSaved);
      onChanged(updated);
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setBusy(false);
    }
  }

  return (
    <Panel
      titleId="vendor-account-decision-emulation-heading"
      title={t.vendorAccountDecisionEmulationTitle}
    >
      <Box sx={{ display: 'grid', gridTemplateColumns: 'minmax(260px, 480px)', gap: 1 }}>
        <FormControlLabel
          control={
            <Checkbox
              checked={account.decision_emulation}
              disabled={disabled || busy}
              onChange={(e) => void toggle(e.target.checked)}
            />
          }
          label={t.vendorAccountDecisionEmulationLabel}
        />
        <Typography variant="body2" color="text.secondary">
          {t.vendorAccountDecisionEmulationHelp}
        </Typography>
      </Box>
    </Panel>
  );
}
