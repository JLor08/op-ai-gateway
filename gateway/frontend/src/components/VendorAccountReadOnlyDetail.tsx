// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { Box } from '@mui/material';
import type { VendorAccount } from '../api';
import type { Translation } from './shared/types';
import { Panel } from './shared/Panel';
import { Field } from './shared/Field';
import { authTypeLabel, vendorLabel } from './shared/vendorLabel';
import { vendorStatusLabel } from './shared/vendorStatus';
import { VendorAccountModels } from './VendorAccountModels';

/**
 * The read-only detail of a vendor account the viewer may use but not manage: a
 * MEMBER, who can only route to the account through a resource group (spec
 * 2026-10-15 section 10). Its name, vendor, authentication, status, model prefix and
 * owner (`shared_by`), and the models it serves by the name a client requests --
 * and nothing of the management.
 *
 * The member DTO is deliberately minimal: share_level, the management groups, the
 * containment root, base_url, the usage snapshot, each model's upstream id and both
 * timestamps are withheld (zero values on the wire). They are not "empty", they are
 * NOT SHOWN: nothing here reads one, so there is no admin-groups panel, no share
 * control, no usage panel, no base-URL field and no date (the withheld timestamps
 * would read 0001-01-01). The panels that act on the account (the settings form, the
 * credential check, the connect flows, the models refresh) are not rendered either.
 *
 * `status` is the status the list shows (a subscription account is shown disabled
 * while the subscriptions switch is off), not necessarily the stored one.
 */
export function VendorAccountReadOnlyDetail({
  t,
  account,
  status,
}: Readonly<{
  t: Translation;
  account: VendorAccount;
  status: string;
}>) {
  const noop = () => undefined;
  return (
    <>
      <Panel
        titleId="vendor-account-member-heading"
        title={t.vendorAccountMemberTitle}
        subtitle={t.vendorAccountMemberNote}
      >
        <Box sx={{ display: 'grid', gridTemplateColumns: 'minmax(260px, 480px)', gap: 2.25 }}>
          <Field
            id="vendor-account-detail-vendor"
            label={t.vendorAccountVendorLabel}
            value={vendorLabel(t, account.vendor)}
            onChange={noop}
            readOnly
          />
          <Field
            id="vendor-account-detail-auth-type"
            label={t.vendorAccountAuthTypeLabel}
            value={authTypeLabel(t, account.auth_type)}
            onChange={noop}
            readOnly
          />
          <Field
            id="vendor-account-detail-name"
            label={t.vendorAccountNameLabel}
            value={account.name}
            onChange={noop}
            readOnly
          />
          <Field
            id="vendor-account-detail-status"
            label={t.vendorAccountStatusLabel}
            value={vendorStatusLabel(t, status)}
            onChange={noop}
            readOnly
          />
          {account.model_prefix !== '' && (
            <Field
              id="vendor-account-detail-model-prefix"
              label={t.vendorAccountModelPrefixLabel}
              value={account.model_prefix}
              onChange={noop}
              readOnly
            />
          )}
          {account.shared_by !== '' && (
            <Field
              id="vendor-account-detail-shared-by"
              label={t.vendorAccountSharedBy}
              value={account.shared_by}
              onChange={noop}
              readOnly
            />
          )}
        </Box>
      </Panel>

      {/* The models by gateway name, with no refresh (no onRefresh). */}
      <Box sx={{ mt: 3 }}>
        <VendorAccountModels t={t} account={account} busy={false} />
      </Box>
    </>
  );
}
