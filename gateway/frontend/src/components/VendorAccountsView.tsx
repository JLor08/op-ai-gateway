// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useEffect, useRef, useState, type SubmitEvent } from 'react';
import { Box, Button, Typography } from '@mui/material';
import AddIcon from '@mui/icons-material/Add';
import DeleteIcon from '@mui/icons-material/Delete';
import ListAltIcon from '@mui/icons-material/ListAlt';
import type { VendorAccount, VendorModelsRefresh } from '../api';
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
import { vendorLabel } from './shared/vendorLabel';
import { isValidModelPrefix, normalizeModelPrefix } from './shared/vendorInputs';
import { VendorAccountModels } from './VendorAccountModels';
import { VendorAccountUsage } from './VendorAccountUsagePanel';
import { VendorConnectionTest } from './VendorConnectionTest';
import { VendorSubscriptionConnect } from './VendorSubscriptionConnect';

type Mode = 'list' | 'create' | { kind: 'detail'; account: VendorAccount };

// The vendors and authentication types a user can create an account for. An
// api_key account is created with its key; a subscription account is created
// disconnected and connected from its detail view (VendorSubscriptionConnect).
const VENDORS: VendorAccount['vendor'][] = ['openai', 'anthropic'];
const AUTH_TYPES: VendorAccount['auth_type'][] = ['api_key', 'subscription'];

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
 *
 * A subscription account has no key: it is created disconnected (the create
 * form then opens its detail view) and connected from the detail view's
 * "connect subscription" panel, which only ever sees `subscription_connected`.
 *
 * An account may carry a model prefix (an optional input on the create form and
 * the settings panel, validated inline against the shape the backend accepts):
 * every model it serves is then listed and requested as prefix + the vendor's
 * id. The detail view's "Models" panel (VendorAccountModels) lists the current
 * models and refreshes them from the vendor. Saving the settings always sends the
 * prefix -- the backend re-labels the account's models on any request that carries
 * one, which also heals rows a failed earlier re-label left behind -- and a failed
 * save re-reads the account, so the view shows what was truly persisted.
 *
 * The detail view also shows a "Check credentials" panel
 * (VendorConnectionTest): a "Test connection" button whose verdict is about the
 * stored credential only, not about any model -- and a "Usage & limits" panel
 * (VendorAccountUsage) for any account that can have a rate-limit snapshot -- in
 * practice a connected subscription. The list carries no snapshot, so the panel
 * reads it from the single-account GET.
 */
export function VendorAccountsView({
  t,
  api,
}: Readonly<{
  t: Translation;
  api: Pick<
    PortalApi,
    | 'vendorAccounts'
    | 'createVendorAccount'
    | 'updateVendorAccount'
    | 'deleteVendorAccount'
    | 'connectVendorAccountImport'
    | 'beginVendorAccountConnect'
    | 'completeVendorAccountConnect'
    | 'beginVendorAccountDeviceConnect'
    | 'pollVendorAccountDeviceConnect'
    | 'vendorAccount'
    | 'testConnection'
    | 'refreshModels'
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
  // The latest mode, for callbacks an async connect holds across renders: the
  // closure it captured would still see the detail view it was started from.
  const modeRef = useRef<Mode>(mode);
  useEffect(() => {
    modeRef.current = mode;
  }, [mode]);
  const [busy, setBusy] = useState(false);
  const [confirmingDeleteId, setConfirmingDeleteId] = useState('');

  // Form state, shared by the create form and the detail view's settings panel.
  const [name, setName] = useState('');
  const [status, setStatus] = useState('active');
  const [vendor, setVendor] = useState<string>('openai');
  // Create only: api_key | subscription (immutable once created).
  const [authType, setAuthType] = useState<string>('api_key');
  // Create: the plain api-key field. Detail: the write-only replace input
  // (empty = keep the stored key) and the pending-clear flag.
  const [apiKey, setApiKey] = useState('');
  const [keyCleared, setKeyCleared] = useState(false);
  // The optional model prefix, as typed (create and detail); trimmed on send.
  const [modelPrefix, setModelPrefix] = useState('');
  const modelPrefixValid = isValidModelPrefix(modelPrefix);

  function resetKeyInput() {
    setApiKey('');
    setKeyCleared(false);
  }

  function openCreate() {
    setName('');
    setVendor('openai');
    setAuthType('api_key');
    setModelPrefix('');
    resetKeyInput();
    setMode('create');
  }

  function openDetail(account: VendorAccount) {
    setName(account.name);
    setStatus(account.status);
    setModelPrefix(account.model_prefix);
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
    // A malformed prefix is already flagged inline; never round-trip it for a 400.
    if (!modelPrefixValid) return;
    setBusy(true);
    try {
      // An empty prefix is simply omitted (= none).
      const prefix = normalizeModelPrefix(modelPrefix);
      const prefixField = prefix === '' ? {} : { model_prefix: prefix };
      // A subscription account carries no key: it is created disconnected.
      const created = await api.createVendorAccount(
        authType === 'subscription'
          ? { vendor, auth_type: 'subscription', name, ...prefixField }
          : { vendor, auth_type: 'api_key', name, api_key: apiKey, ...prefixField },
      );
      setAccountsData((current) => [...(current ?? []), created]);
      if (created.auth_type === 'subscription') {
        // Straight to the detail view, where the account is connected.
        openDetail(created);
      } else {
        backToList();
      }
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setBusy(false);
    }
  }

  async function saveSettings() {
    if (typeof mode === 'string' || mode.kind !== 'detail') return;
    // A malformed prefix is already flagged inline; never round-trip it for a 400.
    if (!modelPrefixValid) return;
    const before = accounts.find((a) => a.id === mode.account.id) ?? mode.account;
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
      const updated = await api.updateVendorAccount(before.id, {
        name,
        status,
        // Always sent, even unchanged: it makes the backend re-label the models.
        model_prefix: normalizeModelPrefix(modelPrefix),
        ...apiKeyPatch,
      });
      setAccountsData((current) => (current ?? []).map((a) => (a.id === updated.id ? updated : a)));
      setMode({ kind: 'detail', account: updated });
      setStatus(updated.status);
      setModelPrefix(updated.model_prefix);
      resetKeyInput();
      showSuccess(t.save);
    } catch (err) {
      showError(formatPortalError(err, t));
      await reloadAfterFailedSave(before);
    } finally {
      setBusy(false);
    }
  }

  // A save that failed may still have written: the backend persists the account
  // BEFORE it re-labels its models, so an error on that follow-on step leaves the
  // new name, status and prefix stored. Re-read the account, so the list, the
  // model list and (when the row was in fact written) the form show what is truly
  // persisted rather than what was typed. A row that was not written (a refused
  // value) keeps what the user typed, so it can be corrected. A re-read that fails
  // too changes nothing: the error toast above already says what went wrong.
  async function reloadAfterFailedSave(before: VendorAccount) {
    let fresh: VendorAccount;
    try {
      fresh = await api.vendorAccount(before.id);
    } catch {
      return;
    }
    accountRefreshed(fresh);
    const current = modeRef.current;
    if (typeof current === 'string' || current.kind !== 'detail') return;
    if (current.account.id !== fresh.id || fresh.updated_at === before.updated_at) return;
    setName(fresh.name);
    setStatus(fresh.status);
    setModelPrefix(fresh.model_prefix);
    resetKeyInput();
  }

  // A connect succeeded: the account now reads as connected (and active), so
  // refresh it in the list. When the user is still on that account's detail view,
  // re-seed it too -- only the status select from the server; an unsaved rename in
  // the settings form is left alone. A connect that completes after the user has
  // navigated away (back to the list, or into another account) must NOT touch the
  // view: it would re-open the wrong account, and a later Save would PATCH the
  // wrong name/status onto it.
  function accountConnected(updated: VendorAccount) {
    setAccountsData((current) => (current ?? []).map((a) => (a.id === updated.id ? updated : a)));
    const current = modeRef.current;
    if (typeof current === 'string' || current.kind !== 'detail') return;
    if (current.account.id !== updated.id) return;
    setMode({ kind: 'detail', account: updated });
    setStatus(updated.status);
  }

  // The models were refreshed (or the account was re-read after a failed save):
  // the account in the list is replaced, and when the user is still on that
  // account's detail view, so is the one it shows. The settings form is left
  // alone -- an unsaved rename or prefix survives -- except for the status select,
  // and only when the status itself changed: a refresh that finds the account's
  // refresh token rejected flips it to needs_reconnect, and a Save with the stale
  // select value would otherwise quietly re-activate it. Like accountConnected,
  // it must not touch the view of an account the user has since left.
  function accountRefreshed(updated: VendorAccount) {
    setAccountsData((current) => (current ?? []).map((a) => (a.id === updated.id ? updated : a)));
    const current = modeRef.current;
    if (typeof current === 'string' || current.kind !== 'detail') return;
    if (current.account.id !== updated.id) return;
    if (current.account.status !== updated.status) setStatus(updated.status);
    setMode({ kind: 'detail', account: updated });
  }

  // The explicit "refresh models": ask the gateway to re-discover the models from
  // the vendor, adopt the account it answers with, and hand the outcome to the
  // panel. `busy` keeps it and a settings save from overlapping. A thrown error
  // is the panel's to show.
  async function refreshAccountModels(id: string): Promise<VendorModelsRefresh> {
    setBusy(true);
    try {
      const { account, refresh } = await api.refreshModels(id);
      accountRefreshed(account);
      return refresh;
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

  // Create sub-view: vendor + authentication + name, and the (plain, required)
  // api key for an api_key account. A subscription account has no key field: it
  // is created disconnected and connected from its detail view.
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
            <SelectField
              id="vendor-account-auth-type"
              label={t.vendorAccountAuthTypeLabel}
              value={authType}
              onChange={(e) => {
                setAuthType(e.target.value);
                // Never carry a typed key into a subscription account.
                resetKeyInput();
              }}
            >
              {AUTH_TYPES.map((a) => (
                <option value={a} key={a}>
                  {authTypeLabel(t, a)}
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
              id="vendor-account-model-prefix"
              label={t.vendorAccountModelPrefixLabel}
              value={modelPrefix}
              onChange={(e) => setModelPrefix(e.target.value)}
              autoComplete="off"
              error={!modelPrefixValid}
              helperText={
                modelPrefixValid
                  ? t.vendorAccountModelPrefixNote
                  : t.errorVendorAccountModelPrefixInvalid
              }
            />
            {authType === 'subscription' ? (
              <Typography color="text.secondary" variant="body2">
                {t.vendorAccountSubscriptionCreateNote}
              </Typography>
            ) : (
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
            )}
            <Box sx={{ display: 'flex', gap: 1.5 }}>
              <Button type="submit" variant="contained" disabled={busy || !modelPrefixValid}>
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

  // Detail sub-view: rename / status / key rotation + delete, plus the
  // credential-check and usage panels and the connect panel of a subscription
  // account. Vendor and auth type are immutable server-side, so they are shown
  // read-only.
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
            <Field
              id="vendor-account-detail-model-prefix"
              label={t.vendorAccountModelPrefixLabel}
              value={modelPrefix}
              onChange={(e) => setModelPrefix(e.target.value)}
              autoComplete="off"
              error={!modelPrefixValid}
              helperText={
                modelPrefixValid
                  ? t.vendorAccountModelPrefixNote
                  : t.errorVendorAccountModelPrefixInvalid
              }
            />
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
              <Button type="submit" variant="contained" disabled={busy || !modelPrefixValid}>
                {t.save}
              </Button>
            </Box>
          </Box>
        </Panel>

        {/* The account's current models, and the explicit refresh from the
            vendor. Keyed by id only: a refresh answer must not drop its own
            outcome, but another account never inherits one. */}
        <Box sx={{ mt: 3 }}>
          <VendorAccountModels
            key={account.id}
            t={t}
            account={account}
            busy={busy}
            onRefresh={refreshAccountModels}
          />
        </Box>

        {/* The credential check works for every account: one without a stored
            credential simply answers "unverifiable". Keyed by id and updated_at,
            so saving or reconnecting the account drops a verdict that described
            the previous credential. */}
        <Box sx={{ mt: 3 }}>
          <VendorConnectionTest
            key={`${account.id}:${account.updated_at}`}
            t={t}
            api={api}
            accountId={account.id}
          />
        </Box>

        {/* A subscription that was never connected has served nothing, so it
            has no usage snapshot to read; everything else may. */}
        {(account.auth_type === 'api_key' || account.subscription_connected) && (
          <VendorAccountUsage key={account.id} t={t} api={api} accountId={account.id} />
        )}

        {account.auth_type === 'subscription' && (
          <Box sx={{ mt: 3 }}>
            <VendorSubscriptionConnect
              t={t}
              account={account}
              api={api}
              onConnected={accountConnected}
            />
          </Box>
        )}

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
