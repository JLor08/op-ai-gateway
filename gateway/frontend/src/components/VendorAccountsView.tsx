// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useEffect, useState, type SubmitEvent } from 'react';
import { Box, Button } from '@mui/material';
import AddIcon from '@mui/icons-material/Add';
import DeleteIcon from '@mui/icons-material/Delete';
import ListAltIcon from '@mui/icons-material/ListAlt';
import type { VendorAccount } from '../api';
import type { BadgeStatus, PortalApi, Translation } from './shared/types';
import { formatPortalError } from './shared/format';
import { useResource } from './shared/useResource';
import { PageTitle } from './shared/PageTitle';
import { Panel } from './shared/Panel';
import { Field } from './shared/Field';
import { SelectField } from './shared/SelectField';
import { StatusChip } from './shared/StatusChip';
import { Breadcrumbs } from './shared/Breadcrumbs';
import { ConfirmDialog } from './shared/ConfirmDialog';
import { ListTable, listTableLabels, type ListColumn } from './shared/ListTable';
import type { RowAction } from './shared/RowActionsMenu';
import { useToast } from './shared/ToastProvider';

type Mode = 'list' | 'create' | { kind: 'detail'; account: VendorAccount };

// The vendors a user can create an account for. The API-key path is the only
// one the page offers today; the subscription auth type (and its OAuth connect
// flow) is a later milestone, so there is deliberately no auth-type choice and
// no connect action here.
const VENDORS: VendorAccount['vendor'][] = ['openai', 'anthropic'];

function vendorLabel(t: Translation, vendor: string): string {
  switch (vendor) {
    case 'openai':
      return t.vendorOpenAI;
    case 'anthropic':
      return t.vendorAnthropic;
    default:
      return vendor;
  }
}

function authTypeLabel(t: Translation, authType: string): string {
  switch (authType) {
    case 'api_key':
      return t.vendorAuthApiKey;
    case 'subscription':
      return t.vendorAuthSubscription;
    default:
      return authType;
  }
}

function statusLabel(t: Translation, status: string): string {
  switch (status) {
    case 'active':
      return t.statusActive;
    case 'disabled':
      return t.statusDisabled;
    case 'needs_reconnect':
      return t.vendorAccountStatusNeedsReconnect;
    default:
      return status;
  }
}

// needs_reconnect is system-managed (a failed subscription token refresh) and
// reads as a warning, not as an active or a switched-off account.
function statusBadge(status: string): BadgeStatus {
  switch (status) {
    case 'active':
      return 'active';
    case 'needs_reconnect':
      return 'watch';
    default:
      return 'disabled';
  }
}

// Whether the account holds a credential, whichever auth type it uses.
function hasCredential(account: VendorAccount): boolean {
  return account.api_key_set || account.subscription_connected;
}

/**
 * Vendor accounts ("Anbieter"): the signed-in user's OWN accounts at external
 * AI vendors, kept as a CRUD list -> create form -> detail (rename / status /
 * key rotation / delete). Every authenticated user manages their own accounts,
 * so there is no role gate (the backend answers 404 for anybody else's id and
 * the list only ever carries the caller's accounts).
 *
 * The API key is a WRITE-ONLY secret, following ApplicationSection's api-token
 * pattern: the DTO only says `api_key_set`, never the value, so the detail
 * field starts empty and shows a "set" placeholder; a typed value replaces the
 * stored key, the Clear button commits "" on Save, and an untouched field
 * omits `api_key` from the PATCH so the stored key is kept.
 */
export function VendorAccountsView({
  t,
  api,
}: Readonly<{
  t: Translation;
  api: Pick<
    PortalApi,
    'vendorAccounts' | 'createVendorAccount' | 'updateVendorAccount' | 'deleteVendorAccount'
  >;
}>) {
  const { showError, showSuccess } = useToast();
  const {
    data: accountsData,
    setData: setAccountsData,
    loading,
    error,
  } = useResource(() => api.vendorAccounts().then((r) => r.data), [api, t], t, {
    trackLoading: false,
  });
  useEffect(() => {
    if (error) showError(error);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [error]);
  const accounts = accountsData ?? [];

  const [mode, setMode] = useState<Mode>('list');
  const [busy, setBusy] = useState(false);
  const [confirmingDeleteId, setConfirmingDeleteId] = useState('');

  // Form state, shared by the create form and the detail view's settings panel.
  const [name, setName] = useState('');
  const [status, setStatus] = useState('active');
  const [vendor, setVendor] = useState<string>('openai');
  // Create: the plain api-key field. Detail: the write-only replace input
  // (empty = keep the stored key) and the pending-clear flag.
  const [apiKey, setApiKey] = useState('');
  const [keyCleared, setKeyCleared] = useState(false);

  function resetKeyInput() {
    setApiKey('');
    setKeyCleared(false);
  }

  function openCreate() {
    setName('');
    setVendor('openai');
    resetKeyInput();
    setMode('create');
  }

  function openDetail(account: VendorAccount) {
    setName(account.name);
    setStatus(account.status);
    resetKeyInput();
    setMode({ kind: 'detail', account });
  }

  function backToList() {
    // Never carry a typed secret across views.
    resetKeyInput();
    setMode('list');
  }

  async function submitCreate(event: SubmitEvent<HTMLFormElement>) {
    event.preventDefault();
    setBusy(true);
    try {
      const created = await api.createVendorAccount({
        vendor,
        auth_type: 'api_key',
        name,
        api_key: apiKey,
      });
      setAccountsData((current) => [...(current ?? []), created]);
      backToList();
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setBusy(false);
    }
  }

  async function saveSettings() {
    if (typeof mode === 'string' || mode.kind !== 'detail') return;
    setBusy(true);
    try {
      // Key sentinel: a pending clear -> "" (clear); a typed value -> replace;
      // otherwise omit the field so the stored key is kept.
      let apiKeyPatch: { api_key?: string } = {};
      if (keyCleared) {
        apiKeyPatch = { api_key: '' };
      } else if (apiKey !== '') {
        apiKeyPatch = { api_key: apiKey };
      }
      const updated = await api.updateVendorAccount(mode.account.id, {
        name,
        status,
        ...apiKeyPatch,
      });
      setAccountsData((current) => (current ?? []).map((a) => (a.id === updated.id ? updated : a)));
      setMode({ kind: 'detail', account: updated });
      setStatus(updated.status);
      resetKeyInput();
      showSuccess(t.save);
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setBusy(false);
    }
  }

  async function removeAccount(id: string) {
    try {
      await api.deleteVendorAccount(id);
      setAccountsData((current) => (current ?? []).filter((a) => a.id !== id));
      setConfirmingDeleteId('');
      if (typeof mode !== 'string' && mode.kind === 'detail' && mode.account.id === id) {
        backToList();
      }
    } catch (err) {
      setConfirmingDeleteId('');
      showError(formatPortalError(err, t));
    }
  }

  const columns: ListColumn<VendorAccount>[] = [
    {
      id: 'vendor',
      label: t.vendorAccountColVendor,
      value: (a) => a.vendor,
      filter: 'enum',
      searchable: false,
      enumLabel: (v) => vendorLabel(t, v),
      render: (a) => vendorLabel(t, a.vendor),
    },
    {
      id: 'authType',
      label: t.vendorAccountColAuthType,
      value: (a) => a.auth_type,
      filter: 'enum',
      searchable: false,
      enumLabel: (v) => authTypeLabel(t, v),
      render: (a) => authTypeLabel(t, a.auth_type),
    },
    { id: 'name', label: t.tableName, value: (a) => a.name, filter: 'text' },
    {
      id: 'status',
      label: t.tableStatus,
      value: (a) => a.status,
      filter: 'enum',
      searchable: false,
      enumLabel: (v) => statusLabel(t, v),
      render: (a) => <StatusChip status={statusBadge(a.status)} label={statusLabel(t, a.status)} />,
    },
    {
      id: 'credential',
      label: t.vendorAccountColCredential,
      value: (a) => (hasCredential(a) ? 'set' : 'missing'),
      filter: 'enum',
      searchable: false,
      enumLabel: (v) =>
        v === 'set' ? t.vendorAccountCredentialSet : t.vendorAccountCredentialMissing,
      render: (a) =>
        hasCredential(a) ? t.vendorAccountCredentialSet : t.vendorAccountCredentialMissing,
    },
  ];

  const rowActions = (account: VendorAccount): RowAction[] => [
    {
      key: 'open',
      label: t.modelDetailsAction,
      icon: <ListAltIcon fontSize="small" />,
      onClick: () => openDetail(account),
    },
  ];

  const listLabels = listTableLabels(t, { empty: t.vendorAccountListEmpty });

  // Create sub-view: vendor + name + the (plain, required) api key. Created
  // accounts are api_key accounts; the subscription type arrives later.
  if (mode === 'create') {
    return (
      <>
        <Breadcrumbs
          ariaLabel={t.breadcrumb}
          backLabel={t.back}
          items={[{ label: t.providers, onClick: backToList }, { label: t.vendorAccountCreate }]}
        />
        <Panel titleId="vendor-account-form-heading" title={t.vendorAccountCreate}>
          <Box
            component="form"
            onSubmit={submitCreate}
            sx={{ display: 'grid', gridTemplateColumns: 'minmax(260px, 480px)', gap: 2.25 }}
          >
            <SelectField
              id="vendor-account-vendor"
              label={t.vendorAccountVendorLabel}
              value={vendor}
              onChange={(e) => setVendor(e.target.value)}
            >
              {VENDORS.map((v) => (
                <option value={v} key={v}>
                  {vendorLabel(t, v)}
                </option>
              ))}
            </SelectField>
            <Field
              id="vendor-account-name"
              label={t.vendorAccountNameLabel}
              value={name}
              onChange={(e) => setName(e.target.value)}
              required
            />
            <Field
              id="vendor-account-api-key"
              type="password"
              label={t.vendorAccountApiKeyLabel}
              value={apiKey}
              onChange={(e) => setApiKey(e.target.value)}
              autoComplete="new-password"
              helperText={t.vendorAccountApiKeyNote}
              required
            />
            <Box sx={{ display: 'flex', gap: 1.5 }}>
              <Button type="submit" variant="contained" disabled={busy}>
                {t.vendorAccountCreate}
              </Button>
              <Button type="button" variant="text" color="secondary" onClick={backToList}>
                {t.cancel}
              </Button>
            </Box>
          </Box>
        </Panel>
      </>
    );
  }

  // Detail sub-view: rename / status / key rotation + delete. Vendor and auth
  // type are immutable server-side, so they are shown read-only.
  if (typeof mode !== 'string' && mode.kind === 'detail') {
    const account = accounts.find((a) => a.id === mode.account.id) ?? mode.account;
    const keyStored = account.api_key_set && !keyCleared;
    return (
      <>
        <Breadcrumbs
          ariaLabel={t.breadcrumb}
          backLabel={t.back}
          items={[{ label: t.providers, onClick: backToList }, { label: account.name }]}
        />
        <Panel
          titleId="vendor-account-settings-heading"
          title={t.vendorAccountSettingsTitle}
          actions={
            <Button
              type="button"
              variant="outlined"
              color="error"
              startIcon={<DeleteIcon fontSize="small" />}
              onClick={() => setConfirmingDeleteId(account.id)}
            >
              {t.vendorAccountActionDelete}
            </Button>
          }
        >
          <Box
            component="form"
            onSubmit={(event) => {
              event.preventDefault();
              void saveSettings();
            }}
            sx={{ display: 'grid', gridTemplateColumns: 'minmax(260px, 480px)', gap: 2.25 }}
          >
            <Field
              id="vendor-account-detail-vendor"
              label={t.vendorAccountVendorLabel}
              value={vendorLabel(t, account.vendor)}
              onChange={() => undefined}
              readOnly
            />
            <Field
              id="vendor-account-detail-auth-type"
              label={t.vendorAccountAuthTypeLabel}
              value={authTypeLabel(t, account.auth_type)}
              onChange={() => undefined}
              readOnly
            />
            <Field
              id="vendor-account-detail-name"
              label={t.vendorAccountNameLabel}
              value={name}
              onChange={(e) => setName(e.target.value)}
              required
            />
            <SelectField
              id="vendor-account-detail-status"
              label={t.vendorAccountStatusLabel}
              value={status}
              onChange={(e) => setStatus(e.target.value)}
            >
              <option value="active">{t.statusActive}</option>
              <option value="disabled">{t.statusDisabled}</option>
              {/* System-managed: never assignable, but the select needs the
                  option while the account is in that state (the backend lets
                  an unchanged status ride along on a PATCH). */}
              {account.status === 'needs_reconnect' && (
                <option value="needs_reconnect" disabled>
                  {t.vendorAccountStatusNeedsReconnect}
                </option>
              )}
            </SelectField>
            {account.auth_type === 'api_key' && (
              <Box>
                <Field
                  id="vendor-account-detail-api-key"
                  type="password"
                  label={t.vendorAccountApiKeyLabel}
                  value={keyCleared ? '' : apiKey}
                  onChange={(e) => {
                    setApiKey(e.target.value);
                    setKeyCleared(false);
                  }}
                  autoComplete="new-password"
                  placeholder={keyStored ? t.vendorAccountApiKeySetPlaceholder : undefined}
                  helperText={t.vendorAccountApiKeyNote}
                />
                {keyStored && (
                  <Button
                    type="button"
                    size="small"
                    variant="text"
                    color="secondary"
                    onClick={() => {
                      setKeyCleared(true);
                      setApiKey('');
                    }}
                  >
                    {t.vendorAccountApiKeyClear}
                  </Button>
                )}
              </Box>
            )}
            <Box sx={{ display: 'flex', gap: 1.5 }}>
              <Button type="submit" variant="contained" disabled={busy}>
                {t.save}
              </Button>
            </Box>
          </Box>
        </Panel>

        <ConfirmDialog
          open={confirmingDeleteId !== ''}
          title={t.vendorAccountDeleteConfirm}
          confirmLabel={t.vendorAccountActionDelete}
          cancelLabel={t.cancel}
          onConfirm={() => void removeAccount(confirmingDeleteId)}
          onCancel={() => setConfirmingDeleteId('')}
        />
      </>
    );
  }

  // List view.
  return (
    <>
      <PageTitle
        title={t.providers}
        subtitle={t.providersIntro}
        action={
          <Button variant="contained" startIcon={<AddIcon />} onClick={openCreate}>
            {t.vendorAccountCreate}
          </Button>
        }
      />
      <Panel titleId="vendor-accounts-list-heading" title={t.vendorAccountListTitle}>
        <ListTable
          rows={accounts}
          columns={columns}
          rowKey={(a) => a.id}
          actions={rowActions}
          storageKey="op.vendor-accounts"
          labels={listLabels}
          loading={loading || accountsData === null}
        />
      </Panel>
    </>
  );
}
