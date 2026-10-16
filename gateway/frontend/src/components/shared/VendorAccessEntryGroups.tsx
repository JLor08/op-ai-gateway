// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { Box, Checkbox, FormControlLabel, Typography } from '@mui/material';
import type { Translation } from './types';
import { Field } from './Field';
import { isValidModelPrefix } from './vendorInputs';
import type { VendorAccessEntryState } from './vendorAccessAccounts';

/**
 * The per-account controls of a token's vendor opt-in, one group per opted-in
 * account: the "override prefix" toggle and, once it is on, the prefix field. Shared
 * by the user-token form (TokenList) and the service-token create form
 * (ServiceTokensSection), for an own and a shared account alike. `label` is the
 * account's name as the list shows it (owner-supplied text: rendered as text only).
 * `idPrefix` namespaces the DOM ids of one form.
 */
export function VendorAccessEntryGroups({
  items,
  idPrefix,
  onChange,
  t,
}: Readonly<{
  items: { id: string; label: string; entry: VendorAccessEntryState }[];
  idPrefix: string;
  onChange: (accountId: string, patch: Partial<VendorAccessEntryState>) => void;
  t: Translation;
}>) {
  return (
    <>
      {items.map(({ id, label, entry }) => {
        const prefixValid = isValidModelPrefix(entry.prefix);
        return (
          <Box
            key={id}
            role="group"
            aria-labelledby={`${idPrefix}-${id}-heading`}
            sx={{ display: 'grid', gap: 1, pl: 2 }}
          >
            <Typography id={`${idPrefix}-${id}-heading`} variant="body2" color="text.secondary">
              {label}
            </Typography>
            <FormControlLabel
              control={
                <Checkbox
                  checked={entry.overrideEnabled}
                  onChange={(e) => onChange(id, { overrideEnabled: e.target.checked })}
                />
              }
              label={t.tokenVendorAccessPrefixToggle}
            />
            {entry.overrideEnabled && (
              <Field
                id={`${idPrefix}-${id}-prefix`}
                label={t.tokenVendorAccessPrefixLabel}
                value={entry.prefix}
                onChange={(e) => onChange(id, { prefix: e.target.value })}
                autoComplete="off"
                error={!prefixValid}
                helperText={
                  prefixValid
                    ? t.tokenVendorAccessPrefixEmptyHint
                    : t.errorVendorAccountModelPrefixInvalid
                }
              />
            )}
          </Box>
        );
      })}
    </>
  );
}
