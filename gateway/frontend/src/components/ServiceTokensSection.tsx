// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useEffect, useRef, useState, type SubmitEvent } from 'react';
import {
  Box,
  Button,
  Checkbox,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  FormControlLabel,
  IconButton,
  Typography,
} from '@mui/material';
import AddIcon from '@mui/icons-material/Add';
import DeleteIcon from '@mui/icons-material/Delete';
import AutorenewIcon from '@mui/icons-material/Autorenew';
import ContentCopyIcon from '@mui/icons-material/ContentCopy';
import type {
  ModelOption,
  PortalService,
  ServiceTokenDTO,
  VendorAccessDTO,
  VendorAccount,
} from '../api';
import type { MessageKey, Translation, PortalApi } from './shared/types';
import { formatPortalError, formatDate } from './shared/format';
import { Panel } from './shared/Panel';
import { Field } from './shared/Field';
import { CheckboxGroup } from './shared/CheckboxGroup';
import { StatusChip } from './shared/StatusChip';
import { SecretReveal } from './shared/SecretReveal';
import { ConfirmDialog } from './shared/ConfirmDialog';
import { ListTable, listTableLabels, type ListColumn } from './shared/ListTable';
import type { RowAction } from './shared/RowActionsMenu';
import {
  ModelOverrideEditor,
  buildOverrideMap,
  overrideRowsInvalid,
  overrideSummary,
  type OverrideRow,
} from './shared/ModelOverrideEditor';
import { OverrideTargetSelect, overrideTargets } from './shared/OverrideTargetSelect';
import {
  offerableVendorAccounts,
  patchVendorAccessEntry,
  toggleVendorAccessEntry,
  vendorAccessEntryDTO,
  vendorAccessEntryInvalid,
  vendorAccountOptionLabel,
  type VendorAccessEntryState,
} from './shared/vendorAccessAccounts';
import { VendorAccessEntryGroups } from './shared/VendorAccessEntryGroups';
import { useToast } from './shared/ToastProvider';

const serviceTokenStatusLabelByKey: Record<string, MessageKey> = {
  active: 'statusActive',
  disabled: 'statusDisabled',
  expired: 'statusExpired',
};

// Narrows a service-token's plain status string to the subset StatusChip
// accepts; anything unrecognized reads as "active" (never crashes on an
// unexpected backend value).
function serviceTokenBadgeStatus(status: string): 'active' | 'disabled' | 'expired' {
  return status === 'disabled' || status === 'expired' ? status : 'active';
}

// A ready-to-paste example inference call using the freshly revealed secret
// (Bearer + /v1/chat/completions), mirroring AgentTokenSection's curl pattern.
function serviceTokenCurlCommand(secret: string): string {
  const base = window.location.origin;
  return `curl -s ${base}/v1/chat/completions -H "Authorization: Bearer ${secret}" -H "Content-Type: application/json" -d '{"model":"<model>","messages":[{"role":"user","content":"Hello"}]}'`;
}

/**
 * The service's token sub-feature — list, create (with the shared
 * ModelOverrideEditor), rotate, and delete — extracted from ServicesView
 * (FV-4) following the Section convention used for per-entity sub-features
 * elsewhere (see AgentTokenSection.tsx). ServicesView keeps its own
 * `token_count` display in sync via `onTokenCountChanged`, called with +1 on
 * create and -1 on delete (mirroring the exact clamped update ServicesView
 * used to perform inline: `Math.max(0, token_count + delta)`).
 *
 * Vendor access (#183 sharing): while the vendor-accounts master flag is on, the
 * create dialog also offers the accounts a service could be given, each named
 * explicitly with an optional prefix override. A service owns no account, so these
 * are accounts SHARED with it (the service provisioned into a resource group that
 * contains them). The opt-in is create-only, like every other setting of a service
 * token: there is no edit action, and a rotation keeps it.
 */
export function ServiceTokensSection({
  t,
  api,
  service,
  models,
  vendorAccountsEnabled = false,
  onTokenCountChanged,
}: Readonly<{
  t: Translation;
  api: Pick<
    PortalApi,
    | 'createServiceToken'
    | 'deleteServiceToken'
    | 'rotateServiceToken'
    | 'serviceTokens'
    | 'vendorAccounts'
  >;
  service: PortalService;
  models: ModelOption[];
  // The vendor-accounts MASTER flag (see TokenList): off hides the vendor-access
  // picker, skips the account fetch (the endpoint 409s while the flag is off) and
  // keeps vendor_access out of the create body. Optional so pre-existing renders
  // keep working; defaults to false.
  vendorAccountsEnabled?: boolean;
  onTokenCountChanged?: (delta: number) => void;
}>) {
  const { showError } = useToast();

  const [tokens, setTokens] = useState<ServiceTokenDTO[]>([]);
  const [tokensLoading, setTokensLoading] = useState(false);
  useEffect(() => {
    let cancelled = false;
    setTokensLoading(true);
    api
      .serviceTokens(service.id)
      .then((r) => {
        if (!cancelled) setTokens(r.data);
      })
      .catch((err) => {
        if (!cancelled) showError(formatPortalError(err, t));
      })
      .finally(() => {
        if (!cancelled) setTokensLoading(false);
      });
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [api, service.id]);

  // Token create-dialog state.
  const [tokenFormOpen, setTokenFormOpen] = useState(false);
  const [tokenName, setTokenName] = useState('');
  const [tokenOverrideRows, setTokenOverrideRows] = useState<OverrideRow[]>([]);
  const [tokenCatchAll, setTokenCatchAll] = useState('');
  // The unknown-model redirect (Task 8) — see TokenList for the field
  // meanings. This form is create-only (no update endpoint for a service
  // token), so there is never a last-used value to show; the display always
  // reads the placeholder.
  const [tokenUnknownRedirect, setTokenUnknownRedirect] = useState(false);
  const [tokenUnknownRedirectBlocked, setTokenUnknownRedirectBlocked] = useState(false);
  const [tokenUnknownFallback, setTokenUnknownFallback] = useState('');
  // Opt-in Claude-compatible model aliases — see TokenList.
  const [tokenClaudeAliases, setTokenClaudeAliases] = useState(false);
  // Opt-in Claude Code effort compatibility — see TokenList.
  const [tokenClaudeEffort, setTokenClaudeEffort] = useState(false);
  const [tokenLogCommunication, setTokenLogCommunication] = useState(false);
  const [tokenSecretFlag, setTokenSecretFlag] = useState(false);
  const [tokenBusy, setTokenBusy] = useState(false);
  const tokenOverrideRowsInvalid = overrideRowsInvalid(tokenOverrideRows);
  // Vendor access (flag-gated, see vendorAccountsEnabled): the explicit per-account
  // opt-in. There is no `all` for a service (it owns no account, so `all` would
  // cover nothing). vendorAccounts is the CREATOR's account list
  // api.vendorAccounts() returns, loaded each time the dialog opens with a
  // latest-wins guard; null = not loaded for this open (in flight or failed). It
  // is the best the portal can offer: whether the SERVICE reaches an account is
  // decided by its resource-group provisioning, which the list does not carry, so
  // the backend save check (an unreachable id is a 400) and routing are the gate.
  const [tokenVaEntries, setTokenVaEntries] = useState<VendorAccessEntryState[]>([]);
  const [vendorAccounts, setVendorAccounts] = useState<VendorAccount[] | null>(null);
  const vendorAccountsReqIdRef = useRef(0);
  const sharedVendorAccounts = offerableVendorAccounts(vendorAccounts ?? [], 'service').shared;
  // The entries the create carries: only accounts the dialog offered (a picked entry
  // can only come from an offered one, so this guards a list that reloaded).
  const offeredVaIds = new Set(sharedVendorAccounts.map((acc) => acc.id));
  const sentVaEntries = tokenVaEntries.filter((entry) => offeredVaIds.has(entry.accountId));
  const tokenVendorAccessInvalid =
    vendorAccountsEnabled && sentVaEntries.some(vendorAccessEntryInvalid);

  useEffect(() => {
    if (!tokenFormOpen || !vendorAccountsEnabled) return;
    const reqId = ++vendorAccountsReqIdRef.current;
    api
      .vendorAccounts()
      .then((resp) => {
        if (vendorAccountsReqIdRef.current === reqId) setVendorAccounts(resp.data);
      })
      .catch(() => {
        /* best-effort: the picker stays without options and the token is created without vendor access */
      });
  }, [tokenFormOpen, api, vendorAccountsEnabled]);

  // The one-time reveal (create or rotate) — empty = closed.
  const [revealedSecret, setRevealedSecret] = useState('');
  const [confirmingTokenDeleteId, setConfirmingTokenDeleteId] = useState('');
  const [confirmingTokenRotateId, setConfirmingTokenRotateId] = useState('');

  function openTokenForm() {
    setTokenName('');
    setTokenOverrideRows([]);
    setTokenCatchAll('');
    setTokenUnknownRedirect(false);
    setTokenUnknownRedirectBlocked(false);
    setTokenUnknownFallback('');
    setTokenClaudeAliases(false);
    setTokenClaudeEffort(false);
    setTokenLogCommunication(false);
    setTokenSecretFlag(false);
    setTokenVaEntries([]);
    setVendorAccounts(null);
    setTokenFormOpen(true);
  }

  // The vendor_access create value: undefined while the flag is off or nothing is
  // picked (the strict default: no vendor access, which an omitted field means);
  // otherwise the explicit list, `all` off. An override that is off omits
  // prefix_override entirely, an enabled one sends its trimmed value.
  function buildVendorAccess(): VendorAccessDTO | undefined {
    if (!vendorAccountsEnabled || sentVaEntries.length === 0) return undefined;
    return { all: false, accounts: sentVaEntries.map(vendorAccessEntryDTO) };
  }

  async function submitCreateToken(event: SubmitEvent<HTMLFormElement>) {
    event.preventDefault();
    setTokenBusy(true);
    try {
      const resp = await api.createServiceToken(service.id, {
        name: tokenName,
        model_override: tokenCatchAll,
        model_override_map: buildOverrideMap(tokenOverrideRows),
        // last_used_model is deliberately never sent: it is read-only and
        // not part of CreateServiceTokenRequest at all — a fresh token has
        // never routed a request.
        unknown_model_redirect: tokenUnknownRedirect,
        unknown_model_redirect_blocked: tokenUnknownRedirectBlocked,
        unknown_model_fallback: tokenUnknownFallback,
        claude_model_aliases: tokenClaudeAliases,
        claude_effort_compatibility: tokenClaudeEffort,
        log_communication: tokenLogCommunication,
        secret: tokenSecretFlag,
        vendor_access: buildVendorAccess(),
      });
      setTokens((current) => [resp.token, ...current]);
      onTokenCountChanged?.(1);
      setTokenFormOpen(false);
      setRevealedSecret(resp.secret);
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setTokenBusy(false);
    }
  }

  async function rotateToken(tokenId: string) {
    try {
      const resp = await api.rotateServiceToken(service.id, tokenId);
      setTokens((current) => current.map((tok) => (tok.id === tokenId ? resp.token : tok)));
      setConfirmingTokenRotateId('');
      setRevealedSecret(resp.secret);
    } catch (err) {
      showError(formatPortalError(err, t));
    }
  }

  async function removeToken(tokenId: string) {
    try {
      await api.deleteServiceToken(service.id, tokenId);
      setTokens((current) => current.filter((tok) => tok.id !== tokenId));
      onTokenCountChanged?.(-1);
      setConfirmingTokenDeleteId('');
    } catch (err) {
      showError(formatPortalError(err, t));
    }
  }

  const tokenColumns: ListColumn<ServiceTokenDTO>[] = [
    { id: 'name', label: t.tableName, value: (r) => r.name, filter: 'text' },
    { id: 'prefix', label: t.serviceTokenColPrefix, value: (r) => r.secret_prefix, filter: 'text' },
    {
      id: 'status',
      label: t.tableStatus,
      value: (r) => r.status,
      filter: 'enum',
      searchable: false,
      enumLabel: (v) => t[serviceTokenStatusLabelByKey[v] ?? 'statusActive'],
      render: (r) => (
        <StatusChip
          status={serviceTokenBadgeStatus(r.status)}
          label={t[serviceTokenStatusLabelByKey[r.status] ?? 'statusActive']}
        />
      ),
    },
    {
      id: 'expires',
      label: t.serviceTokenColExpires,
      value: (r) => r.expires_at ?? '',
      render: (r) => formatDate(r.expires_at, t.agentTokenNever),
    },
    {
      id: 'model',
      label: t.tokenModelOverrideColumn,
      value: (r) => overrideSummary(t, r),
      filter: 'text',
      render: (r) => overrideSummary(t, r) || '-',
    },
    {
      id: 'lastUsed',
      label: t.serviceTokenColLastUsed,
      value: (r) => r.last_used_at ?? '',
      render: (r) => formatDate(r.last_used_at, t.agentTokenNever),
    },
    {
      id: 'last_used_model',
      label: t.tokenLastUsedModel,
      value: (r) => r.last_used_model || '',
      filter: 'text',
      defaultHidden: true,
      render: (r) => r.last_used_model || '—',
    },
  ];

  // Every model-valued setting of the form (each rule's target, the
  // catch-all and the unknown-model redirect's fallback) picks from the SAME
  // model-plus-group list `models` already carries (portal model listing,
  // groups marked `is_group`) — no separate fetch — minus the names no API
  // serves (see overrideTargets).
  const modelTargets = overrideTargets(models, null);

  const tokenRowActions = (r: ServiceTokenDTO): RowAction[] => [
    {
      key: 'rotate',
      label: t.tokenActionRotate,
      icon: <AutorenewIcon fontSize="small" />,
      onClick: () => setConfirmingTokenRotateId(r.id),
    },
    {
      key: 'delete',
      label: t.tokenActionDelete,
      color: 'error',
      icon: <DeleteIcon fontSize="small" />,
      onClick: () => setConfirmingTokenDeleteId(r.id),
    },
  ];

  const listLabels = listTableLabels(t);

  // Reveal dialog (create + rotate share it): the secret + a ready-to-paste
  // example curl call.
  const revealDialog = (
    <Dialog
      open={revealedSecret !== ''}
      onClose={() => setRevealedSecret('')}
      maxWidth="md"
      fullWidth
    >
      <DialogTitle>{t.oneTimeSecret}</DialogTitle>
      <DialogContent>
        <SecretReveal
          title={t.oneTimeSecret}
          copyValue={revealedSecret}
          copyLabel={t.agentTokenCopy}
          copiedLabel={t.copied}
        >
          <code>{revealedSecret}</code>
        </SecretReveal>
        <Box sx={{ mt: 2 }}>
          <Typography variant="subtitle2">{t.serviceTokenCurlLabel}</Typography>
          <Box sx={{ display: 'flex', alignItems: 'center', gap: 1, minWidth: 0, mt: 1 }}>
            <Box
              component="pre"
              sx={{
                flexGrow: 1,
                minWidth: 0,
                m: 0,
                overflowX: 'auto',
                p: 1,
                bgcolor: 'action.hover',
                borderRadius: 1,
              }}
            >
              <code>{serviceTokenCurlCommand(revealedSecret)}</code>
            </Box>
            <IconButton
              size="small"
              aria-label={t.agentDownloadCopyCurl}
              onClick={() =>
                navigator.clipboard?.writeText(serviceTokenCurlCommand(revealedSecret))
              }
            >
              <ContentCopyIcon fontSize="small" />
            </IconButton>
          </Box>
        </Box>
      </DialogContent>
      <DialogActions>
        <Button onClick={() => setRevealedSecret('')}>{t.captureClose}</Button>
      </DialogActions>
    </Dialog>
  );

  return (
    <>
      <Box sx={{ mt: 3 }}>
        <Panel
          titleId="service-tokens-heading"
          title={t.serviceTokensTitle}
          subtitle={t.serviceTokensIntro}
          actions={
            <Button variant="contained" startIcon={<AddIcon />} onClick={openTokenForm}>
              {t.serviceTokenCreate}
            </Button>
          }
        >
          <ListTable
            rows={tokens}
            columns={tokenColumns}
            rowKey={(r) => r.id}
            actions={tokenRowActions}
            storageKey="op.service-tokens"
            labels={listLabels}
            loading={tokensLoading}
          />
        </Panel>
      </Box>

      <Dialog open={tokenFormOpen} onClose={() => setTokenFormOpen(false)} maxWidth="sm" fullWidth>
        <Box component="form" onSubmit={submitCreateToken}>
          <DialogTitle>{t.serviceTokenCreate}</DialogTitle>
          <DialogContent sx={{ display: 'grid', gap: 2 }}>
            <Field
              id="service-token-name"
              label={t.tokenNameLabel}
              value={tokenName}
              onChange={(e) => setTokenName(e.target.value)}
              required
            />
            <ModelOverrideEditor
              rows={tokenOverrideRows}
              onRowsChange={setTokenOverrideRows}
              catchAll={tokenCatchAll}
              onCatchAllChange={setTokenCatchAll}
              targets={modelTargets}
              t={t}
              idPrefix="service-token"
              catchAllId="service-token-catchall"
            />
            <Box sx={{ display: 'grid', gap: 1 }}>
              <FormControlLabel
                control={
                  <Checkbox
                    checked={tokenUnknownRedirect}
                    onChange={(e) => {
                      const checked = e.target.checked;
                      setTokenUnknownRedirect(checked);
                      // See TokenList's identical handler: the redirect being
                      // off must clear both sub-settings in STATE, not just
                      // their disabled rendering — submitCreateToken sends
                      // them unconditionally.
                      if (!checked) {
                        setTokenUnknownRedirectBlocked(false);
                        setTokenUnknownFallback('');
                      }
                    }}
                  />
                }
                label={t.tokenUnknownRedirect}
              />
              <Typography variant="caption" color="text.secondary">
                {t.tokenUnknownRedirectHint}
              </Typography>
              <FormControlLabel
                control={
                  <Checkbox
                    checked={tokenUnknownRedirectBlocked}
                    disabled={!tokenUnknownRedirect}
                    onChange={(e) => setTokenUnknownRedirectBlocked(e.target.checked)}
                  />
                }
                label={t.tokenUnknownRedirectBlocked}
              />
              <Typography variant="caption" color="text.secondary">
                {t.tokenUnknownRedirectBlockedHint}
              </Typography>
              <OverrideTargetSelect
                id="service-token-unknown-fallback"
                label={t.tokenUnknownFallback}
                value={tokenUnknownFallback}
                onChange={setTokenUnknownFallback}
                disabled={!tokenUnknownRedirect}
                targets={modelTargets}
                t={t}
              />
              <Typography variant="caption" color="text.secondary">
                {t.tokenLastUsedModel}
              </Typography>
              {/* Create-only form: there is never a last-used value yet. */}
              <Typography variant="body2">{t.tokenLastUsedModelNone}</Typography>
            </Box>
            <Box sx={{ display: 'grid', gap: 1 }}>
              <FormControlLabel
                control={
                  <Checkbox
                    checked={tokenClaudeAliases}
                    onChange={(e) => setTokenClaudeAliases(e.target.checked)}
                  />
                }
                label={t.tokenClaudeAliases}
              />
              <Typography variant="caption" color="text.secondary">
                {t.tokenClaudeAliasesHint}
              </Typography>
            </Box>
            <Box sx={{ display: 'grid', gap: 1 }}>
              <FormControlLabel
                control={
                  <Checkbox
                    checked={tokenClaudeEffort}
                    onChange={(e) => setTokenClaudeEffort(e.target.checked)}
                  />
                }
                label={t.tokenClaudeEffort}
              />
              <Typography variant="caption" color="text.secondary">
                {t.tokenClaudeEffortHint}
              </Typography>
            </Box>
            {/* Hidden entirely while the vendor-accounts master flag is off. Create
                only: a service token has no edit action and a rotation keeps the
                opt-in (the note says so). Account names are owner-supplied text:
                rendered as text only. */}
            {vendorAccountsEnabled && (
              <Box
                role="group"
                aria-labelledby="service-token-vendor-access-heading"
                sx={{ display: 'grid', gap: 1 }}
              >
                <Typography id="service-token-vendor-access-heading" variant="subtitle2">
                  {t.tokenVendorAccessLabel}
                </Typography>
                <Typography variant="caption" color="text.secondary">
                  {t.serviceTokenVendorAccessNote}
                </Typography>
                {vendorAccounts !== null && sharedVendorAccounts.length === 0 && (
                  <Typography variant="body2" color="text.secondary">
                    {t.serviceTokenVendorAccessNone}
                  </Typography>
                )}
                {sharedVendorAccounts.length > 0 && (
                  <CheckboxGroup
                    legend={t.serviceTokenVendorAccessAccountsLabel}
                    options={sharedVendorAccounts.map((acc) => ({
                      value: acc.id,
                      label: vendorAccountOptionLabel(acc, t),
                    }))}
                    selected={tokenVaEntries.map((entry) => entry.accountId)}
                    onToggle={(accountId) =>
                      setTokenVaEntries((current) => toggleVendorAccessEntry(current, accountId))
                    }
                  />
                )}
                <VendorAccessEntryGroups
                  items={sharedVendorAccounts.flatMap((acc) => {
                    const entry = tokenVaEntries.find((e) => e.accountId === acc.id);
                    return entry
                      ? [{ id: acc.id, label: vendorAccountOptionLabel(acc, t), entry }]
                      : [];
                  })}
                  idPrefix="service-token-va"
                  onChange={(accountId, patch) =>
                    setTokenVaEntries((current) =>
                      patchVendorAccessEntry(current, accountId, patch),
                    )
                  }
                  t={t}
                />
              </Box>
            )}
            <FormControlLabel
              control={
                <Checkbox
                  checked={tokenLogCommunication}
                  onChange={(e) => setTokenLogCommunication(e.target.checked)}
                />
              }
              label={t.tokenLogCommunicationLabel}
            />
            <FormControlLabel
              control={
                <Checkbox
                  checked={tokenSecretFlag}
                  onChange={(e) => setTokenSecretFlag(e.target.checked)}
                />
              }
              label={t.tokenSecretLabel}
            />
          </DialogContent>
          <DialogActions>
            <Button type="button" onClick={() => setTokenFormOpen(false)}>
              {t.cancel}
            </Button>
            <Button
              type="submit"
              variant="contained"
              disabled={tokenBusy || tokenOverrideRowsInvalid || tokenVendorAccessInvalid}
            >
              {t.serviceTokenCreate}
            </Button>
          </DialogActions>
        </Box>
      </Dialog>

      {revealDialog}

      <ConfirmDialog
        open={confirmingTokenDeleteId !== ''}
        title={t.tokenDeleteConfirm}
        confirmLabel={t.tokenActionDelete}
        cancelLabel={t.cancel}
        onConfirm={() => void removeToken(confirmingTokenDeleteId)}
        onCancel={() => setConfirmingTokenDeleteId('')}
      />
      <ConfirmDialog
        open={confirmingTokenRotateId !== ''}
        title={t.tokenRotateConfirmTitle}
        body={
          vendorAccountsEnabled
            ? `${t.tokenRotateConfirmBody} ${t.serviceTokenRotateKeepsVendorAccess}`
            : t.tokenRotateConfirmBody
        }
        confirmLabel={t.tokenRotateConfirm}
        cancelLabel={t.cancel}
        onConfirm={() => void rotateToken(confirmingTokenRotateId)}
        onCancel={() => setConfirmingTokenRotateId('')}
      />
    </>
  );
}
