// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { FormControlLabel, Switch } from '@mui/material';
import type { Translation } from './shared/types';
import { Field } from './shared/Field';
import { SelectField } from './shared/SelectField';
import {
  ACME_DIRECTORY_PRODUCTION,
  ACME_DIRECTORY_STAGING,
  acmeDirectoryChoiceFor,
} from './shared/acmeDirectory';
import type { AcmeDirectoryChoice } from './shared/acmeDirectory';

export type AcmeConfigValues = {
  shared: boolean;
  email: string;
  directoryUrl: string;
  weeklyLimit: number;
};

export type AcmeConfigPatch = Partial<AcmeConfigValues>;

/**
 * Shared shared-vs-own ACME configuration sub-component, used identically for
 * the edge (gateway-nginx) and the public-domains certificate contexts (see
 * cert_edge_acme_* / cert_public_acme_* on SystemSettings). A single switch
 * ("eigene ACME-Einstellungen verwenden" -- off means "re-use the global/shared
 * ACME account", on means "use the fields below") reveals an email field, the
 * Production/Staging/Custom directory dropdown (via the shared acmeDirectory
 * module), and a weekly-limit field -- fixed and read-only for the two
 * well-known directories, editable only under Custom.
 *
 * Purely presentational + controlled: `values` is the CURRENT resolved state
 * (pending-edit-or-loaded, decided by the caller), `onChange` is called with a
 * partial patch on every edit -- the caller owns the pending-edit state and
 * persistence (mirrors every other field in CertificateSettings/
 * EdgeCertificatePanel). `prefix` only scopes element ids so an edge instance
 * and a public instance can render on the same page without id collisions.
 */
export function AcmeConfigFields({
  prefix,
  t,
  values,
  onChange,
}: Readonly<{
  prefix: 'edge' | 'public';
  t: Translation;
  values: AcmeConfigValues;
  onChange: (patch: AcmeConfigPatch) => void;
}>) {
  const directoryChoice = acmeDirectoryChoiceFor(values.directoryUrl);
  const own = !values.shared;

  function onDirectoryChange(choice: AcmeDirectoryChoice) {
    if (choice === 'production') {
      onChange({ directoryUrl: ACME_DIRECTORY_PRODUCTION });
    } else if (choice === 'staging') {
      onChange({ directoryUrl: ACME_DIRECTORY_STAGING });
    } else {
      // Coming FROM production/staging, "Custom" starts blank (an operator must
      // enter their own endpoint); already-custom keeps whatever is there.
      onChange({ directoryUrl: directoryChoice === 'custom' ? values.directoryUrl : '' });
    }
    // Deliberately NOT patching weeklyLimit here (round-1 review fix): the
    // caller derives the EFFECTIVE weekly limit from the directory choice
    // itself (fixedAcmeWeeklyLimitFor), so `values.weeklyLimit` is already
    // the fixed number for production/staging by the time it reaches this
    // component -- patching it here too would be redundant, and leaving a
    // custom edit in pending state means switching back to Custom later
    // restores what the operator last typed instead of resetting it.
  }

  return (
    <>
      <FormControlLabel
        control={
          <Switch
            checked={own}
            onChange={(e) => onChange({ shared: !e.target.checked })}
            data-testid={`cert-${prefix}-acme-own`}
          />
        }
        label={t.settingsAcmeOwnSettings}
      />
      {own && (
        <>
          <Field
            id={`cert-${prefix}-acme-email`}
            label={t.settingsAcmeEmail}
            value={values.email}
            onChange={(e) => onChange({ email: e.target.value })}
          />
          <SelectField
            id={`cert-${prefix}-acme-directory`}
            label={t.settingsAcmeDirectory}
            value={directoryChoice}
            onChange={(e) => onDirectoryChange(e.target.value as AcmeDirectoryChoice)}
          >
            <option value="production">{t.settingsAcmeDirectoryProduction}</option>
            <option value="staging">{t.settingsAcmeDirectoryStaging}</option>
            <option value="custom">{t.settingsAcmeDirectoryCustom}</option>
          </SelectField>
          {directoryChoice === 'custom' && (
            <Field
              id={`cert-${prefix}-acme-directory-custom`}
              label={t.settingsAcmeDirectoryCustom}
              value={values.directoryUrl}
              onChange={(e) => onChange({ directoryUrl: e.target.value })}
            />
          )}
          {directoryChoice === 'custom' ? (
            <Field
              id={`cert-${prefix}-acme-weekly-limit`}
              type="number"
              label={t.settingsAcmeWeeklyLimit}
              value={String(values.weeklyLimit)}
              onChange={(e) => onChange({ weeklyLimit: Number(e.target.value) })}
              inputProps={{ min: 0, step: 1 }}
            />
          ) : (
            // Round-1 review fix: displays `values.weeklyLimit` directly (the
            // SAME number the caller sends on save) rather than a locally
            // hardcoded constant -- displayed and persisted can no longer
            // diverge by construction. Staging's fixed value is 0, which
            // reads as "unlimited" (the label), not literally "0".
            <Field
              id={`cert-${prefix}-acme-weekly-limit`}
              label={t.settingsAcmeWeeklyLimit}
              value={
                directoryChoice === 'staging'
                  ? t.settingsAcmeWeeklyLimitUnlimited
                  : String(values.weeklyLimit)
              }
              onChange={() => {}}
              disabled
              inputProps={{ readOnly: true }}
            />
          )}
        </>
      )}
    </>
  );
}
