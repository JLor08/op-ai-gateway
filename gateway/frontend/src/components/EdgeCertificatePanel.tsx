// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useEffect, useState } from 'react';
import {
  Alert,
  Box,
  Button,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  FormControlLabel,
  IconButton,
  Stack,
  Switch,
  Typography,
} from '@mui/material';
import DownloadIcon from '@mui/icons-material/Download';
import ContentCopyIcon from '@mui/icons-material/ContentCopy';
import { PortalApiError } from '../api';
import type { EdgeCertificate, EdgeTLSProbeResult } from '../api';
import type { Translation, PortalApi, BadgeStatus } from './shared/types';
import { formatPortalError, formatDate } from './shared/format';
import { useResource } from './shared/useResource';
import { Panel } from './shared/Panel';
import { SelectField } from './shared/SelectField';
import { Field } from './shared/Field';
import { StatusChip } from './shared/StatusChip';
import { ConfirmDialog } from './shared/ConfirmDialog';
import { useToast } from './shared/ToastProvider';
import { downloadText } from './shared/download';
import { AcmeConfigFields } from './AcmeConfigFields';
import {
  ACME_DIRECTORY_PRODUCTION,
  acmeDirectoryChoiceFor,
  fixedAcmeWeeklyLimitFor,
} from './shared/acmeDirectory';

// Remaining validity in whole days, computed CLIENT-SIDE from not_after so it
// never goes stale between polls -- the exact same rule the certificate list
// uses, kept local rather than shared: it is six lines, and CertificateSettings'
// version additionally drives its color-coded severity, which this panel does
// not need. null when there is no not_after yet (nothing issued).
function remainingDays(notAfter?: string): number | null {
  if (!notAfter) return null;
  const ms = Date.parse(notAfter) - Date.now();
  if (Number.isNaN(ms)) return null;
  return Math.ceil(ms / 86400000);
}

// Maps the stored row's status to the shared StatusChip's fixed BadgeStatus enum
// (mirrors CertificateSettings' certStatusBadge -- kept local for the same
// reason as remainingDays above: a handful of lines, no color-severity coupling).
function statusBadge(status: string): BadgeStatus {
  switch (status) {
    case 'active':
      return 'active';
    case 'error':
      return 'error';
    case 'pending':
      return 'watch';
    default:
      return 'standby';
  }
}

function statusLabel(t: Translation, status: string): string {
  switch (status) {
    case 'active':
      return t.certificatesStatusActive;
    case 'pending':
      return t.certificatesStatusPending;
    case 'error':
      return t.certificatesStatusError;
    case 'skipped':
      return t.certificatesStatusSkipped;
    default:
      return status;
  }
}

// The reconcile only ever acts on this row when it is actually WANTED --
// mirrors the backend's edgeDesired gate exactly (service_edge_cert.go:74-92:
// `!set.EdgeEnabled || len(set.EdgeNames) == 0` => not wanted) and is what
// DeliverEdgeCertificate itself is gated on (service_certificates.go:673-679,
// via ReconcileCertificates' edgeWanted). `edge.names` already mirrors the
// configured list REGARDLESS of the enabled switch (configuredEdgeNames probes
// with EdgeEnabled forced true), so this check is meaningful even when
// `enabled` is false. Fix round 1, IMPORTANT 1: `delivery_mode` and
// `key_download_available` are computed from EdgeDeliveryCapable(), which is
// INDEPENDENT of the enabled toggle (it only looks at the output dir / last
// write error) -- so a fresh install with an output dir configured but the
// edge feature still off reads delivery_mode="local" with no written_at, and
// without this gate the panel would promise a write that will never happen.
function edgeWanted(edge: EdgeCertificate): boolean {
  return edge.enabled && edge.names.length > 0;
}

// The delivery-mode plain-text line -- exactly one of five renders, matching
// EdgeCertificate.delivery_mode/written_at/write_error precisely. Under
// delivery_mode "local", written_at absent vs. present is the ONLY signal that
// distinguishes "will be written at the next reconcile pass" from "already on
// nginx's disk" -- collapsing it into a boolean, or dropping it, would erase
// that distinction, so both states get their own sentence rather than a shared
// "delivered: yes/no" line. The fifth state (added in fix round 1) covers
// "local, nothing written yet, but NOT wanted" -- the reconcile will never act
// here until the switch/names are actually configured, so the pending
// "will be written at the next pass" promise would be false.
function deliveryText(t: Translation, edge: EdgeCertificate): string {
  if (edge.delivery_mode === 'local') {
    const path = edge.output_dir ?? '';
    if (edge.written_at) {
      return t.certificatesEdgeDeliveryLocalWritten(path, formatDate(edge.written_at, ''));
    }
    if (!edgeWanted(edge)) {
      return t.certificatesEdgeDeliveryLocalNotWanted(path);
    }
    return t.certificatesEdgeDeliveryLocalPending(path);
  }
  // "download": a configured output_dir with a write_error means the LAST local
  // write attempt failed and delivery fell back to "download" -- a distinct,
  // more actionable state from "no output_dir was ever configured".
  if (edge.output_dir && edge.write_error) {
    return t.certificatesEdgeDeliveryFailedNote(edge.output_dir, edge.write_error);
  }
  return t.certificatesEdgeDeliveryDownloadNote;
}

// "not_configured" is a LOCAL sentinel this frontend synthesizes when the probe
// endpoint itself answers 409 certificate.edge_probe_not_configured (no
// OP_AI_GATEWAY_CERT_EDGE_PROBE_TARGET set) -- the backend never puts this
// string in a real EdgeTLSProbeResult.reason (that shape only ever comes back
// on a 200), but routing it through the SAME display path keeps "why didn't
// this work" in one place (the Alert) instead of a generic toast.
const PROBE_NOT_CONFIGURED = 'not_configured';

// Maps ONE synthetic-TLS-self-probe result onto a localized sentence naming the
// CAUSE (mirrors EdgeProbeDTO's own doc comment: "the certificate is still the
// bootstrap pair" vs. "the proxy never terminates TLS at all" call for
// entirely different operator action). Falls back to the raw backend message
// for a reason this frontend does not yet know about, rather than showing
// nothing -- a future backend reason still surfaces something actionable.
function probeReasonText(t: Translation, probe: EdgeTLSProbeResult): string {
  if (probe.ok) return t.certificatesEdgeProbeOk(probe.expected_name ?? '');
  switch (probe.reason) {
    case 'unreachable':
      return t.certificatesEdgeProbeUnreachable(probe.target);
    case 'bootstrap_certificate':
      return t.certificatesEdgeProbeBootstrap;
    case 'name_mismatch':
      return t.certificatesEdgeProbeNameMismatch(
        probe.expected_name ?? '',
        (probe.sans ?? []).join(', '),
      );
    case 'chain_untrusted':
      return t.certificatesEdgeProbeChainUntrusted;
    case 'expired':
      return t.certificatesEdgeProbeExpired;
    case PROBE_NOT_CONFIGURED:
      return t.certificatesEdgeProbeNotConfigured;
    default:
      return probe.message ?? '';
  }
}

/**
 * EdgeCertificatePanel -- the gateway's OWN edge (nginx) certificate: the TLS
 * leg between the upstream reverse proxy and this gateway's own nginx. A fully
 * separate row/mode from the internal (mesh) certificates CertificateSettings
 * configures -- its own switch, its own issuer dropdown (visibly distinct from
 * the internal one), and its own name set -- plus how it reaches nginx
 * (delivery_mode), downloads, the generated reverse-proxy configuration, and a
 * confirm-gated re-issue action. Its settings form seeds from TWO resources:
 * api.edgeCertificate() for enabled/issuer_mode/names (the exact values
 * cert_edge_enabled/cert_edge_issuer_mode/cert_edge_names resolve to) and
 * api.getSystemSettings() for the cert_edge_acme_* fields (U-T4) AcmeConfigFields
 * edits below -- those live on SystemSettings, not on the edge DTO.
 */
export function EdgeCertificatePanel({
  t,
  api,
}: Readonly<{
  t: Translation;
  api: Pick<
    PortalApi,
    | 'edgeCertificate'
    | 'edgeCertificateBundle'
    | 'edgeCertificateKey'
    | 'edgeProxyConfig'
    | 'getSystemSettings'
    | 'probeEdgeTLS'
    | 'reissueEdgeCertificate'
    | 'updateSystemSettings'
  >;
}>) {
  const { showSuccess, showError } = useToast();
  const {
    data: edge,
    loading: edgeLoading,
    error,
    reload,
  } = useResource(() => api.edgeCertificate(), [api, t], t);
  const {
    data: settings,
    loading: settingsLoading,
    error: settingsError,
    reload: reloadSettings,
  } = useResource(() => api.getSystemSettings(), [api, t], t);
  const loading = edgeLoading || settingsLoading;
  useEffect(() => {
    if (error) showError(error);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [error]);
  useEffect(() => {
    if (settingsError) showError(settingsError);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [settingsError]);

  // Pending edits (null = follow the loaded view, mirrors CertificateSettings).
  const [pendingEnabled, setPendingEnabled] = useState<boolean | null>(null);
  const [pendingIssuerMode, setPendingIssuerMode] = useState<string | null>(null);
  const [pendingNamesCsv, setPendingNamesCsv] = useState<string | null>(null);
  // U-T4: the edge's own shared-vs-own ACME account (AcmeConfigFields).
  const [pendingAcmeShared, setPendingAcmeShared] = useState<boolean | null>(null);
  const [pendingAcmeEmail, setPendingAcmeEmail] = useState<string | null>(null);
  const [pendingAcmeDirectoryUrl, setPendingAcmeDirectoryUrl] = useState<string | null>(null);
  const [pendingAcmeWeeklyLimit, setPendingAcmeWeeklyLimit] = useState<number | null>(null);
  const [savingSettings, setSavingSettings] = useState(false);

  const enabled = pendingEnabled ?? edge?.enabled ?? false;
  const issuerMode = pendingIssuerMode ?? edge?.issuer_mode ?? 'acme';
  const namesCsv = pendingNamesCsv ?? (edge?.names ?? []).join(', ');
  const acmeShared = pendingAcmeShared ?? settings?.cert_edge_acme_shared ?? true;
  const acmeEmail = pendingAcmeEmail ?? settings?.cert_edge_acme_email ?? '';
  const acmeDirectoryUrl =
    pendingAcmeDirectoryUrl ??
    (settings?.cert_edge_acme_directory_url || ACME_DIRECTORY_PRODUCTION);
  const acmeDirectoryChoice = acmeDirectoryChoiceFor(acmeDirectoryUrl);
  // Round-1 review fix (weekly-limit divergence): the backend's real "unset"
  // default is 0 (nonNegativeIntSetting), which `?? 50` does not catch (0 is
  // not nullish) -- so `acmeWeeklyLimitStored` is the honest
  // stored-or-pending-or-0 value, and the EFFECTIVE value actually displayed
  // AND sent on save is the fixed constant for a predefined directory (never
  // the raw stored number), falling back to the stored/edited value only
  // under Custom. See shared/acmeDirectory.ts's fixedAcmeWeeklyLimitFor.
  const acmeWeeklyLimitStored =
    pendingAcmeWeeklyLimit ?? settings?.cert_edge_acme_weekly_limit ?? 0;
  const acmeWeeklyLimit = fixedAcmeWeeklyLimitFor(acmeDirectoryChoice) ?? acmeWeeklyLimitStored;

  function handleAcmeChange(patch: {
    shared?: boolean;
    email?: string;
    directoryUrl?: string;
    weeklyLimit?: number;
  }) {
    if (patch.shared !== undefined) setPendingAcmeShared(patch.shared);
    if (patch.email !== undefined) setPendingAcmeEmail(patch.email);
    if (patch.directoryUrl !== undefined) setPendingAcmeDirectoryUrl(patch.directoryUrl);
    if (patch.weeklyLimit !== undefined) setPendingAcmeWeeklyLimit(patch.weeklyLimit);
  }

  async function saveSettings() {
    setSavingSettings(true);
    try {
      const names = namesCsv
        .split(',')
        .map((s) => s.trim())
        .filter((s) => s !== '');
      // A THIRD disjoint PUT partition (mirrors System vs. CertificateSettings):
      // ONLY these cert_edge_* fields, never cert_enabled and never the
      // internal cert_*/acme_* fields CertificateSettings' own save sends.
      await api.updateSystemSettings({
        cert_edge_enabled: enabled,
        cert_edge_issuer_mode: issuerMode as 'acme' | 'self_signed',
        cert_edge_names: names,
        cert_edge_acme_shared: acmeShared,
        cert_edge_acme_email: acmeEmail,
        cert_edge_acme_directory_url: acmeDirectoryUrl,
        cert_edge_acme_weekly_limit: acmeWeeklyLimit,
      });
      setPendingEnabled(null);
      setPendingIssuerMode(null);
      setPendingNamesCsv(null);
      setPendingAcmeShared(null);
      setPendingAcmeEmail(null);
      setPendingAcmeDirectoryUrl(null);
      setPendingAcmeWeeklyLimit(null);
      await reload();
      await reloadSettings();
      showSuccess(t.systemSaved);
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setSavingSettings(false);
    }
  }

  const [reissuing, setReissuing] = useState(false);
  const [confirmingReissue, setConfirmingReissue] = useState(false);
  async function reissue() {
    setReissuing(true);
    try {
      await api.reissueEdgeCertificate();
      await reload();
      showSuccess(t.systemSaved);
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setReissuing(false);
    }
  }

  // The plaintext-refusal gate (Plan B, cert_edge_require_https). Applied
  // IMMEDIATELY (no pending-edit staging, no shared Save button with the
  // enabled/issuer/names section above) -- see api.ts's updateSystemSettings
  // comment on why: bundling it with an unrelated field re-triggers the
  // backend's arming precondition on every such save.
  const requireHttps = edge?.require_https ?? false;
  const httpsObserved = edge?.https_observed ?? false;
  const [applyingRequireHttps, setApplyingRequireHttps] = useState(false);
  const [confirmingArm, setConfirmingArm] = useState(false);
  async function applyRequireHttps(next: boolean) {
    setApplyingRequireHttps(true);
    try {
      await api.updateSystemSettings({ cert_edge_require_https: next });
      await reload();
      showSuccess(t.systemSaved);
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setApplyingRequireHttps(false);
    }
  }
  // Turning the switch OFF (disarming) is never gated -- an operator must
  // always be able to back out. Turning it ON asks for confirmation FIRST
  // (naming the four paths the gate does not cover) before the PUT that the
  // backend's own arming precondition may still reject with 400
  // certificate.edge_https_not_observed if the observation has meanwhile
  // lapsed.
  function onRequireHttpsToggle(next: boolean) {
    if (next) {
      setConfirmingArm(true);
    } else {
      void applyRequireHttps(false);
    }
  }

  const [probing, setProbing] = useState(false);
  const [probeResult, setProbeResult] = useState<EdgeTLSProbeResult | null>(null);
  async function runProbe() {
    setProbing(true);
    setProbeResult(null);
    try {
      const result = await api.probeEdgeTLS();
      setProbeResult(result);
    } catch (err) {
      if (err instanceof PortalApiError && err.code === 'certificate.edge_probe_not_configured') {
        // Route this one specific, expected failure through the SAME Alert the
        // probe's own result renders (naming the cause), not a generic toast.
        setProbeResult({ ok: false, target: '', reason: PROBE_NOT_CONFIGURED });
        return;
      }
      showError(formatPortalError(err, t));
    } finally {
      setProbing(false);
    }
  }

  async function downloadBundle() {
    try {
      const pem = await api.edgeCertificateBundle();
      downloadText('edge-fullchain.pem', pem, 'application/x-pem-file');
    } catch (err) {
      showError(formatPortalError(err, t));
    }
  }

  async function downloadKey() {
    try {
      const pem = await api.edgeCertificateKey();
      downloadText('edge-key.pem', pem, 'application/x-pem-file');
    } catch (err) {
      showError(formatPortalError(err, t));
    }
  }

  const [proxyConfigOpen, setProxyConfigOpen] = useState(false);
  const [proxyConfigText, setProxyConfigText] = useState('');
  const [proxyConfigLoading, setProxyConfigLoading] = useState(false);
  async function openProxyConfig() {
    setProxyConfigLoading(true);
    try {
      const text = await api.edgeProxyConfig();
      setProxyConfigText(text);
      setProxyConfigOpen(true);
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setProxyConfigLoading(false);
    }
  }

  const remaining = edge ? remainingDays(edge.not_after) : null;
  const hasRow = !!edge?.domain;
  // Marking a row due is inert while the reconcile will never act on it (not
  // wanted) -- gate the reissue button on the same edgeWanted() the delivery
  // text uses, not just row-existence.
  const wanted = edge ? edgeWanted(edge) : false;

  return (
    <>
      <Panel
        titleId="cert-edge-heading"
        title={t.certificatesKindEdge}
        subtitle={t.certificatesEdgeIntro}
      >
        {loading && <Typography component="p">{t.loading}</Typography>}
        <Stack spacing={3}>
          <Box>
            <FormControlLabel
              control={
                <Switch checked={enabled} onChange={(e) => setPendingEnabled(e.target.checked)} />
              }
              label={t.settingsCertEdgeEnabled}
            />
          </Box>
          <SelectField
            id="cert-edge-issuer-mode"
            label={t.settingsCertEdgeIssuerMode}
            value={issuerMode}
            onChange={(e) => setPendingIssuerMode(e.target.value)}
          >
            <option value="acme">{t.settingsCertIssuerAcme}</option>
            <option value="self_signed">{t.settingsCertIssuerSelfSigned}</option>
          </SelectField>
          <Field
            id="cert-edge-names"
            label={t.settingsCertEdgeNames}
            value={namesCsv}
            onChange={(e) => setPendingNamesCsv(e.target.value)}
          />
          <AcmeConfigFields
            prefix="edge"
            t={t}
            values={{
              shared: acmeShared,
              email: acmeEmail,
              directoryUrl: acmeDirectoryUrl,
              weeklyLimit: acmeWeeklyLimit,
            }}
            onChange={handleAcmeChange}
          />
          <Box sx={{ display: 'flex', justifyContent: 'flex-end' }}>
            <Button
              type="button"
              variant="contained"
              disabled={savingSettings || loading}
              onClick={saveSettings}
            >
              {t.save}
            </Button>
          </Box>

          {edge?.name_conflict && (
            <Alert severity="warning" data-testid="cert-edge-name-conflict">
              {t.certificatesEdgeNameConflict(edge.name_conflict)}
            </Alert>
          )}

          {edge && hasRow ? (
            <Stack spacing={1}>
              <Box>
                <Typography variant="body2" color="text.secondary">
                  {t.certificatesColDomain}
                </Typography>
                <Typography variant="body1">{edge.domain}</Typography>
              </Box>
              <Box>
                <Typography variant="body2" color="text.secondary">
                  {t.certificatesColStatus}
                </Typography>
                <StatusChip
                  status={statusBadge(edge.status ?? '')}
                  label={statusLabel(t, edge.status ?? '')}
                />
              </Box>
              <Box>
                <Typography variant="body2" color="text.secondary">
                  {t.certificatesColIssued}
                </Typography>
                <Typography variant="body1">{formatDate(edge.issued_at, '')}</Typography>
              </Box>
              <Box>
                <Typography variant="body2" color="text.secondary">
                  {t.certificatesColExpires}
                </Typography>
                <Typography variant="body1">{formatDate(edge.not_after, '')}</Typography>
              </Box>
              <Box>
                <Typography variant="body2" color="text.secondary">
                  {t.certificatesColRemaining}
                </Typography>
                <Typography variant="body1">{remaining ?? ''}</Typography>
              </Box>
              {edge.last_error && (
                <Typography variant="body2" color="error.main">
                  {edge.last_error}
                </Typography>
              )}
            </Stack>
          ) : (
            <Typography variant="body2" color="text.secondary">
              {t.certificatesEdgeNone}
            </Typography>
          )}

          <Typography variant="body2" data-testid="cert-edge-delivery">
            {edge ? deliveryText(t, edge) : ''}
          </Typography>

          <Box sx={{ display: 'flex', gap: 1.5, flexWrap: 'wrap' }}>
            <Button
              type="button"
              variant="outlined"
              startIcon={<DownloadIcon fontSize="small" />}
              disabled={!hasRow}
              onClick={downloadBundle}
            >
              {t.certificatesEdgeButtonDownloadBundle}
            </Button>
            {/* The key button appears ONLY when the gateway cannot deliver
                locally (key_download_available reflects EdgeCertificateKeyPEM's
                own gate) -- never a disabled button that would 409. */}
            {edge?.key_download_available && (
              <Button
                type="button"
                variant="outlined"
                startIcon={<DownloadIcon fontSize="small" />}
                onClick={downloadKey}
              >
                {t.certificatesEdgeButtonDownloadKey}
              </Button>
            )}
            <Button
              type="button"
              variant="outlined"
              disabled={proxyConfigLoading}
              onClick={openProxyConfig}
            >
              {t.certificatesEdgeButtonShowProxyConfig}
            </Button>
          </Box>

          <Box sx={{ display: 'flex', justifyContent: 'flex-end' }}>
            <Button
              type="button"
              variant="outlined"
              color="error"
              disabled={reissuing || !hasRow || !wanted}
              onClick={() => setConfirmingReissue(true)}
            >
              {t.certificatesEdgeButtonReissue}
            </Button>
          </Box>
        </Stack>
      </Panel>

      <Panel
        titleId="cert-edge-gate-heading"
        title={t.certificatesEdgeGateTitle}
        subtitle={t.certificatesEdgeGateIntro}
      >
        <Stack spacing={2}>
          <Box>
            <FormControlLabel
              control={
                <Switch
                  checked={requireHttps}
                  disabled={applyingRequireHttps || !httpsObserved}
                  onChange={(e) => onRequireHttpsToggle(e.target.checked)}
                />
              }
              label={t.settingsCertEdgeRequireHttps}
            />
            {/* Explains the disabled state -- without it a locked switch looks
                broken rather than "waiting on evidence". Shown regardless of the
                current value: even an ALREADY-armed switch loses its "off"
                affordance once the observation lapses. */}
            {!httpsObserved && (
              <Typography variant="body2" color="text.secondary">
                {t.certificatesEdgeGateDisabledHint}
              </Typography>
            )}
            {/* The armed-but-lapsed state renders a CHECKED switch, which reads as
                "plaintext is being refused right now" -- and that is false: the
                runtime gate self-extinguishes without a fresh observation, so it is
                enforcing nothing. The hint above only explains the LOCK; this says
                what the gate is actually doing. */}
            {requireHttps && !httpsObserved && (
              <Typography
                variant="body2"
                color="warning.main"
                data-testid="cert-edge-gate-not-enforcing"
              >
                {t.certificatesEdgeGateArmedNotEnforcingHint}
              </Typography>
            )}
          </Box>
          <Box>
            <Typography
              variant="body2"
              color="text.secondary"
              data-testid="cert-edge-gate-last-encrypted"
            >
              {edge?.last_encrypted_at
                ? t.certificatesEdgeGateLastEncrypted(formatDate(edge.last_encrypted_at, ''))
                : t.certificatesEdgeGateLastEncryptedNever}
            </Typography>
            <Typography
              variant="body2"
              color="text.secondary"
              data-testid="cert-edge-gate-last-plain"
            >
              {edge?.last_plain_at
                ? t.certificatesEdgeGateLastPlain(formatDate(edge.last_plain_at, ''))
                : t.certificatesEdgeGateLastPlainNever}
            </Typography>
          </Box>
          <Box>
            <Button
              type="button"
              variant="outlined"
              disabled={probing}
              onClick={() => void runProbe()}
            >
              {t.certificatesEdgeGateProbeButton}
            </Button>
            {probeResult && (
              <Alert
                severity={probeResult.ok ? 'success' : 'warning'}
                sx={{ mt: 1.5 }}
                data-testid="cert-edge-probe-result"
              >
                {probeReasonText(t, probeResult)}
              </Alert>
            )}
          </Box>
        </Stack>
      </Panel>

      <Dialog
        open={proxyConfigOpen}
        onClose={() => setProxyConfigOpen(false)}
        maxWidth="md"
        fullWidth
      >
        <DialogTitle>{t.certificatesEdgeProxyConfigDialogTitle}</DialogTitle>
        <DialogContent>
          {/* The config is a point-in-time snapshot -- said once, right beside the
              rendered text, so nobody pastes a marker and assumes it is final. */}
          <Typography variant="caption" color="text.secondary" sx={{ display: 'block', mb: 1.5 }}>
            {t.certificatesEdgeProxyConfigSnapshotNote}
          </Typography>
          <Box sx={{ display: 'flex', alignItems: 'flex-start', gap: 1, minWidth: 0 }}>
            <Box
              data-testid="cert-edge-proxy-config-text"
              component="pre"
              sx={{
                flexGrow: 1,
                minWidth: 0,
                m: 0,
                maxHeight: '50vh',
                overflow: 'auto',
                p: 1,
                bgcolor: 'action.hover',
                borderRadius: 1,
              }}
            >
              <code>{proxyConfigText}</code>
            </Box>
            <IconButton
              size="small"
              aria-label={t.certificatesEdgeProxyConfigCopy}
              onClick={() => navigator.clipboard?.writeText(proxyConfigText)}
            >
              <ContentCopyIcon fontSize="small" />
            </IconButton>
          </Box>
        </DialogContent>
        <DialogActions>
          <Button onClick={() => setProxyConfigOpen(false)}>
            {t.certificatesEdgeProxyConfigClose}
          </Button>
        </DialogActions>
      </Dialog>

      <ConfirmDialog
        open={confirmingReissue}
        title={t.certificatesEdgeReissueConfirmTitle}
        body={t.certificatesEdgeReissueConfirmBody}
        confirmLabel={t.confirm}
        cancelLabel={t.cancel}
        onConfirm={() => {
          setConfirmingReissue(false);
          void reissue();
        }}
        onCancel={() => setConfirmingReissue(false)}
      />

      <ConfirmDialog
        open={confirmingArm}
        title={t.certificatesEdgeGateArmConfirmTitle}
        body={t.certificatesEdgeGateArmConfirmBody}
        confirmLabel={t.confirm}
        cancelLabel={t.cancel}
        onConfirm={() => {
          setConfirmingArm(false);
          void applyRequireHttps(true);
        }}
        onCancel={() => setConfirmingArm(false)}
      />
    </>
  );
}
