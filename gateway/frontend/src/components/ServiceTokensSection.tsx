// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useEffect, useState, type SubmitEvent } from 'react';
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
import type { ModelOption, PortalService, ServiceTokenDTO } from '../api';
import type { MessageKey, Translation, PortalApi } from './shared/types';
import { formatPortalError, formatDate } from './shared/format';
import { Panel } from './shared/Panel';
import { Field } from './shared/Field';
import { StatusChip } from './shared/StatusChip';
import { SecretReveal } from './shared/SecretReveal';
import { ConfirmDialog } from './shared/ConfirmDialog';
import { SearchableSelect } from './shared/SearchableSelect';
import { ListTable, listTableLabels, type ListColumn } from './shared/ListTable';
import type { RowAction } from './shared/RowActionsMenu';
import {
  ModelOverrideEditor,
  buildOverrideMap,
  overrideRowsInvalid,
  overrideSummary,
  type OverrideRow,
} from './shared/ModelOverrideEditor';
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
 */
export function ServiceTokensSection({
  t,
  api,
  service,
  models,
  onTokenCountChanged,
}: Readonly<{
  t: Translation;
  api: Pick<
    PortalApi,
    'createServiceToken' | 'deleteServiceToken' | 'rotateServiceToken' | 'serviceTokens'
  >;
  service: PortalService;
  models: ModelOption[];
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
  const [tokenLogCommunication, setTokenLogCommunication] = useState(false);
  const [tokenSecretFlag, setTokenSecretFlag] = useState(false);
  const [tokenBusy, setTokenBusy] = useState(false);
  const tokenOverrideRowsInvalid = overrideRowsInvalid(tokenOverrideRows);

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
    setTokenLogCommunication(false);
    setTokenSecretFlag(false);
    setTokenFormOpen(true);
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
        log_communication: tokenLogCommunication,
        secret: tokenSecretFlag,
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
  ];

  // The unknown-model redirect's fallback picker offers the SAME
  // model-plus-group list `models` already carries (portal model listing,
  // groups marked `is_group`) — no separate fetch (Task 8).
  const unknownFallbackOptions = [
    { value: '', label: '-' },
    ...models.map((m) => ({ value: m.id, label: m.display_name })),
  ];

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
              models={models}
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
              <SearchableSelect
                id="service-token-unknown-fallback"
                label={t.tokenUnknownFallback}
                value={tokenUnknownFallback}
                onChange={setTokenUnknownFallback}
                disabled={!tokenUnknownRedirect}
                options={unknownFallbackOptions}
              />
              <Typography variant="caption" color="text.secondary">
                {t.tokenLastUsedModel}
              </Typography>
              {/* Create-only form: there is never a last-used value yet. */}
              <Typography variant="body2">{t.tokenLastUsedModelNone}</Typography>
            </Box>
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
              disabled={tokenBusy || tokenOverrideRowsInvalid}
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
        body={t.tokenRotateConfirmBody}
        confirmLabel={t.tokenRotateConfirm}
        cancelLabel={t.cancel}
        onConfirm={() => void rotateToken(confirmingTokenRotateId)}
        onCancel={() => setConfirmingTokenRotateId('')}
      />
    </>
  );
}
