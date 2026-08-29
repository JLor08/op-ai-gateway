// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { useEffect, useMemo, useState } from 'react';
import {
  Alert,
  AlertTitle,
  Box,
  Button,
  FormControlLabel,
  Stack,
  Switch,
  Typography,
} from '@mui/material';
import DownloadIcon from '@mui/icons-material/Download';
import AutorenewIcon from '@mui/icons-material/Autorenew';
import type {
  CertificateCA,
  CertificateMeshStatus,
  CertificateRow,
  HTTPSSwitchUnreachableApp,
  PortalServer,
} from '../api';
import type { Translation, PortalApi, BadgeStatus } from './shared/types';
import { formatPortalError } from './shared/format';
import { useResource } from './shared/useResource';
import { PageTitle } from './shared/PageTitle';
import { Panel } from './shared/Panel';
import { SelectField } from './shared/SelectField';
import { Field } from './shared/Field';
import { StatusChip } from './shared/StatusChip';
import { ListTable, listTableLabels, type ListColumn } from './shared/ListTable';
import type { RowAction } from './shared/RowActionsMenu';
import { ConfirmDialog } from './shared/ConfirmDialog';
import { useToast } from './shared/ToastProvider';
import { downloadText } from './shared/download';
import { EdgeCertificatePanel } from './EdgeCertificatePanel';
import { AcmeConfigFields } from './AcmeConfigFields';
import {
  ACME_DIRECTORY_PRODUCTION,
  ACME_DIRECTORY_STAGING,
  acmeDirectoryChoiceFor,
  fixedAcmeWeeklyLimitFor,
} from './shared/acmeDirectory';
import type { AcmeDirectoryChoice } from './shared/acmeDirectory';

// Mirror the backend's MinCertRenewBeforeDays/MinCARenewBeforeDays/
// MinSelfSignedValidityDays/MaxSelfSignedValidityDays (portal
// service_system_settings.go) so the client never lets a save through that the
// server will 400 on with cert.invalid.
const MIN_CERT_RENEW_BEFORE_DAYS = 7;
const MIN_CA_RENEW_BEFORE_DAYS = 30;
const MIN_SELF_SIGNED_VALIDITY_DAYS = 1;
const MAX_SELF_SIGNED_VALIDITY_DAYS = 3650;
// Mirrors the backend's MinCertProxyListenPortBase/MaxCertProxyListenPortBase
// (P4, service_system_settings.go) + DefaultCertProxyListenPortBase.
const MIN_CERT_PROXY_LISTEN_PORT_BASE = 1024;
const MAX_CERT_PROXY_LISTEN_PORT_BASE = 65535;
const DEFAULT_CERT_PROXY_LISTEN_PORT_BASE = 8600;

function emptyCertificateMeshStatus(): CertificateMeshStatus {
  return { tls_active: false, ca_rotation_pending_servers: [] };
}

function certKindLabel(t: Translation, kind: string): string {
  switch (kind) {
    case 'gateway':
      return t.certificatesKindGateway;
    case 'server':
      return t.certificatesKindServer;
    case 'public':
      return t.certificatesKindPublic;
    case 'edge':
      return t.certificatesKindEdge;
    default:
      return kind;
  }
}

function certStatusLabel(t: Translation, status: string): string {
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

// Maps a certificate status to the shared StatusChip's fixed BadgeStatus enum
// (StatusChip renders one of a small set of theme-bridged colors, not arbitrary
// strings) — "active"/"error" ride the union directly, "pending" reads as the
// in-progress "watch" color, "skipped" as neutral "standby".
function certStatusBadge(status: string): BadgeStatus {
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

// Remaining validity in whole days, computed CLIENT-SIDE from not_after so it
// never goes stale between polls; null when there is no not_after yet (a
// pending/never-issued certificate).
function remainingDays(notAfter?: string): number | null {
  if (!notAfter) return null;
  const ms = Date.parse(notAfter) - Date.now();
  if (Number.isNaN(ms)) return null;
  return Math.ceil(ms / 86400000);
}

type Severity = 'critical' | 'warn' | 'ok';

// Same three-way color coding for every remaining-validity display (the leaf
// certificate list AND the internal CA): under 7 days is critical (red),
// under the configured renewal window is a warning (amber), otherwise normal.
function severityFor(days: number | null, renewBeforeDays: number): Severity {
  if (days === null) return 'ok';
  if (days < 7) return 'critical';
  if (days < renewBeforeDays) return 'warn';
  return 'ok';
}

function severityColor(severity: Severity): string | undefined {
  if (severity === 'critical') return 'error.main';
  if (severity === 'warn') return 'warning.main';
  return undefined;
}

// The three states of the "Installiert" column, derived from what the row's
// ServerAgent last reported (Phase 2 distribution):
//   "yes"     — the reported leaf fingerprint EQUALS the issued one
//   "no"      — a report exists, but for a DIFFERENT leaf (stale install)
//   "unknown" — never reported, or a kind that has no agent at all
// "unknown" is deliberately NOT rendered as "not installed": the report registry
// is in-memory, so a gateway restart erases every report while the files on every
// server stay exactly where they are.
type InstalledState = 'yes' | 'no' | 'unknown';

function installedState(row: CertificateRow): InstalledState {
  if (row.kind !== 'server') return 'unknown';
  if (!row.installed_at) return 'unknown';
  return row.installed ? 'yes' : 'no';
}

function installedSymbol(state: InstalledState): string {
  if (state === 'yes') return '✓';
  if (state === 'no') return '✗';
  return '—';
}

// Tooltip text: when a report exists it carries its age and the reported mode (the
// two things that tell an operator whether to trust it), prefixed with the
// mismatch note when the install is stale.
function installedTitle(t: Translation, row: CertificateRow, state: InstalledState): string {
  if (state === 'unknown') return t.certificatesInstalledNever;
  const when = row.installed_at ? new Date(row.installed_at).toLocaleString() : '';
  const parts = [when, row.installed_mode ?? ''].filter((s) => s !== '');
  const detail = parts.join(' · ');
  return state === 'no' ? `${t.certificatesInstalledStale} — ${detail}` : detail;
}

// The three states of the "Transport" column (Phase 3 mesh TLS): whether the
// server's ServerAgent was last observed authenticating over TLS (HTTPS/WSS) or
// plaintext on the mesh listener.
//   "tls"     — the newest observed hop was TLS
//   "plain"   — the newest observed hop was plaintext
//   "unknown" — never observed, or a kind that has no agent at all
// Like "Installiert", "unknown" is NOT rendered as plaintext: the transport
// registry is in-memory, so a gateway restart erases every observation while the
// servers keep talking exactly as before.
type TransportState = 'tls' | 'plain' | 'unknown';

function transportStateOf(row: CertificateRow): TransportState {
  if (row.kind !== 'server' || !row.transport) return 'unknown';
  return row.transport === 'tls' ? 'tls' : 'plain';
}

function transportLabel(t: Translation, state: TransportState): string {
  if (state === 'tls') return `✓ ${t.certificatesTransportTLS}`;
  if (state === 'plain') return `✗ ${t.certificatesTransportPlain}`;
  return '—';
}

function transportTitle(t: Translation, row: CertificateRow, state: TransportState): string {
  if (state === 'unknown') return t.certificatesTransportNever;
  return row.transport_at ? new Date(row.transport_at).toLocaleString() : '';
}

// Extracts the FIRST "-----BEGIN CERTIFICATE----- … -----END CERTIFICATE-----"
// block from a PEM bundle (the bundle may carry a second, previous-root block
// during a rotation window) — used for the "download just the root" button.
// Falls back to the whole input when no PEM block is found (never crashes).
function firstPemBlock(pem: string): string {
  const match = /-----BEGIN CERTIFICATE-----[\s\S]*?-----END CERTIFICATE-----\r?\n?/.exec(pem);
  return match ? match[0] : pem;
}

/**
 * System-admin + module-gated "Zertifikate" view: three sections —
 *   1. Einstellungen — the issuer mode (ACME / self-signed) + its mode-dependent
 *      fields + the shared domain/scope/renewal config. Saves every cert_ and
 *      acme_ field EXCEPT cert_enabled (that checkbox lives in System Settings —
 *      a disjoint PUT partition, mirrors the NetBird module split).
 *   2. Interne CA — visible whenever the internal CA is present (including in
 *      acme mode, where the root may still be needed by existing internal
 *      certificates) — subject/validity/previous-root + download buttons +
 *      a confirm-gated rotate action; directly below it (mode-independent,
 *      rendered regardless of whether the CA panel itself is shown) the
 *      confirm-gated "re-issue everything now" action.
 *   3. Zertifikate — the full certificate inventory as a ListTable, with a
 *      client-computed, color-coded remaining-validity column and a per-row
 *      "renew now" action.
 */
export function CertificateSettings({
  t,
  api,
}: Readonly<{
  t: Translation;
  api: Pick<
    PortalApi,
    | 'certificateCA'
    | 'certificates'
    | 'edgeCertificate'
    | 'edgeCertificateBundle'
    | 'edgeCertificateKey'
    | 'edgeProxyConfig'
    | 'getSystemSettings'
    | 'probeEdgeTLS'
    | 'publicCertificateBundle'
    | 'publicCertificateKey'
    | 'reissueAllCertificates'
    | 'reissueEdgeCertificate'
    | 'renewCertificate'
    | 'rotateCertificateCA'
    | 'servers'
    | 'updateSystemSettings'
  >;
}>) {
  const { showSuccess, showError } = useToast();
  const {
    data: settings,
    setData: setSettings,
    loading,
    error,
  } = useResource(() => api.getSystemSettings(), [api, t], t);
  useEffect(() => {
    if (error) showError(error);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [error]);

  const [busy, setBusy] = useState(false);

  // Panel 1 pending edits (mirrors NetbirdSettings: null = follow the loaded
  // settings, non-null = the operator's in-progress edit).
  const [pendingIssuerMode, setPendingIssuerMode] = useState<string | null>(null);
  const [pendingAcmeEmail, setPendingAcmeEmail] = useState<string | null>(null);
  const [pendingAcmeDirectoryUrl, setPendingAcmeDirectoryUrl] = useState<string | null>(null);
  const [pendingSelfSignedValidity, setPendingSelfSignedValidity] = useState<string | null>(null);
  const [pendingCaRenewBeforeDays, setPendingCaRenewBeforeDays] = useState<string | null>(null);
  const [pendingBaseDomain, setPendingBaseDomain] = useState<string | null>(null);
  const [pendingGatewayDomain, setPendingGatewayDomain] = useState<string | null>(null);
  const [pendingServerScope, setPendingServerScope] = useState<string | null>(null);
  const [pendingRenewBeforeDays, setPendingRenewBeforeDays] = useState<string | null>(null);

  // "Öffentliche Domains" panel pending edits -- a DISJOINT save partition from
  // panel 1 above (mirrors the edge panel's own partition): the manage
  // toggle/domain list (moved out of panel 1) plus the new issuer-mode +
  // AcmeConfigFields state.
  const [pendingManagePublicDomain, setPendingManagePublicDomain] = useState<boolean | null>(null);
  const [pendingPublicDomains, setPendingPublicDomains] = useState<string | null>(null);
  const [pendingPublicIssuerMode, setPendingPublicIssuerMode] = useState<string | null>(null);
  const [pendingPublicAcmeShared, setPendingPublicAcmeShared] = useState<boolean | null>(null);
  const [pendingPublicAcmeEmail, setPendingPublicAcmeEmail] = useState<string | null>(null);
  const [pendingPublicAcmeDirectoryUrl, setPendingPublicAcmeDirectoryUrl] = useState<string | null>(
    null,
  );
  const [pendingPublicAcmeWeeklyLimit, setPendingPublicAcmeWeeklyLimit] = useState<number | null>(
    null,
  );
  const [publicBusy, setPublicBusy] = useState(false);

  const issuerMode = pendingIssuerMode ?? settings?.cert_issuer_mode ?? 'acme';
  const acmeEmail = pendingAcmeEmail ?? settings?.acme_email ?? '';
  const acmeDirectoryUrl =
    pendingAcmeDirectoryUrl ?? settings?.acme_directory_url ?? ACME_DIRECTORY_PRODUCTION;
  const acmeDirectoryChoice = acmeDirectoryChoiceFor(acmeDirectoryUrl);
  const selfSignedValidityValue =
    pendingSelfSignedValidity ?? String(settings?.cert_self_signed_validity_days ?? 365);
  const selfSignedValidityNum = Number(selfSignedValidityValue);
  const selfSignedValidityValid =
    Number.isInteger(selfSignedValidityNum) &&
    selfSignedValidityNum >= MIN_SELF_SIGNED_VALIDITY_DAYS &&
    selfSignedValidityNum <= MAX_SELF_SIGNED_VALIDITY_DAYS;
  const caRenewBeforeDaysValue =
    pendingCaRenewBeforeDays ?? String(settings?.cert_ca_renew_before_days ?? 365);
  const caRenewBeforeDaysNum = Number(caRenewBeforeDaysValue);
  const caRenewBeforeDaysValid =
    Number.isInteger(caRenewBeforeDaysNum) && caRenewBeforeDaysNum >= MIN_CA_RENEW_BEFORE_DAYS;
  const baseDomain = pendingBaseDomain ?? settings?.cert_base_domain ?? '';
  const gatewayDomain = pendingGatewayDomain ?? settings?.cert_gateway_domain ?? '';
  const serverScope = pendingServerScope ?? settings?.cert_server_scope ?? 'selected';
  const renewBeforeDaysValue =
    pendingRenewBeforeDays ?? String(settings?.cert_renew_before_days ?? 30);
  const renewBeforeDaysNum = Number(renewBeforeDaysValue);
  const renewBeforeDaysValid =
    Number.isInteger(renewBeforeDaysNum) && renewBeforeDaysNum >= MIN_CERT_RENEW_BEFORE_DAYS;

  const configValid =
    (issuerMode !== 'self_signed' || (selfSignedValidityValid && caRenewBeforeDaysValid)) &&
    renewBeforeDaysValid;

  async function save() {
    setBusy(true);
    try {
      const updated = await api.updateSystemSettings({
        cert_issuer_mode: issuerMode as 'acme' | 'self_signed',
        cert_self_signed_validity_days: selfSignedValidityNum,
        cert_ca_renew_before_days: caRenewBeforeDaysNum,
        acme_email: acmeEmail,
        acme_directory_url: acmeDirectoryUrl,
        cert_base_domain: baseDomain,
        cert_gateway_domain: gatewayDomain,
        cert_server_scope: serverScope as 'all' | 'selected',
        cert_renew_before_days: renewBeforeDaysNum,
      });
      setSettings(updated);
      setPendingIssuerMode(null);
      setPendingAcmeEmail(null);
      setPendingAcmeDirectoryUrl(null);
      setPendingSelfSignedValidity(null);
      setPendingCaRenewBeforeDays(null);
      setPendingBaseDomain(null);
      setPendingGatewayDomain(null);
      setPendingServerScope(null);
      setPendingRenewBeforeDays(null);
      showSuccess(t.systemSaved);
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setBusy(false);
    }
  }

  // "Öffentliche Domains" panel: the manage toggle/domain list + issuer mode +
  // AcmeConfigFields state, all derived the same pending-edit-or-loaded way as
  // panel 1 above.
  const managePublicDomain =
    pendingManagePublicDomain ?? settings?.cert_manage_public_domain ?? false;
  const publicDomainsCsv = pendingPublicDomains ?? (settings?.cert_public_domains ?? []).join(', ');
  // Round-1 review fix (byte-neutrality): "" is a legal, meaningful stored
  // value ("follow the global/internal issuer mode" -- CertPublicIssuerMode),
  // not just "not yet configured". Defaulting the DISPLAY to a concrete
  // "acme" would, on save, silently pin an unset/byte-neutral deployment away
  // from following the global mode -- so the fallback here stays "", and the
  // dropdown below offers "" as its own selectable "wie interne/globale
  // Einstellung" option (default-selected exactly when stored is "").
  const publicIssuerMode = pendingPublicIssuerMode ?? settings?.cert_public_issuer_mode ?? '';
  const publicAcmeShared = pendingPublicAcmeShared ?? settings?.cert_public_acme_shared ?? true;
  const publicAcmeEmail = pendingPublicAcmeEmail ?? settings?.cert_public_acme_email ?? '';
  const publicAcmeDirectoryUrl =
    pendingPublicAcmeDirectoryUrl ??
    (settings?.cert_public_acme_directory_url || ACME_DIRECTORY_PRODUCTION);
  const publicAcmeDirectoryChoice = acmeDirectoryChoiceFor(publicAcmeDirectoryUrl);
  // Round-1 review fix (weekly-limit divergence): the backend's real "unset"
  // default is 0 (nonNegativeIntSetting), which `?? 50` does not catch (0 is
  // not nullish) -- so `publicAcmeWeeklyLimitStored` is the honest
  // stored-or-pending-or-0 value, and the EFFECTIVE value actually displayed
  // AND sent on save is the fixed constant for a predefined directory
  // (never the raw stored number), falling back to the stored/edited value
  // only under Custom.
  const publicAcmeWeeklyLimitStored =
    pendingPublicAcmeWeeklyLimit ?? settings?.cert_public_acme_weekly_limit ?? 0;
  const publicAcmeWeeklyLimit =
    fixedAcmeWeeklyLimitFor(publicAcmeDirectoryChoice) ?? publicAcmeWeeklyLimitStored;

  function handlePublicAcmeChange(patch: {
    shared?: boolean;
    email?: string;
    directoryUrl?: string;
    weeklyLimit?: number;
  }) {
    if (patch.shared !== undefined) setPendingPublicAcmeShared(patch.shared);
    if (patch.email !== undefined) setPendingPublicAcmeEmail(patch.email);
    if (patch.directoryUrl !== undefined) setPendingPublicAcmeDirectoryUrl(patch.directoryUrl);
    if (patch.weeklyLimit !== undefined) setPendingPublicAcmeWeeklyLimit(patch.weeklyLimit);
  }

  async function savePublic() {
    setPublicBusy(true);
    try {
      const publicDomains = publicDomainsCsv
        .split(',')
        .map((s) => s.trim())
        .filter((s) => s !== '');
      const updated = await api.updateSystemSettings({
        cert_manage_public_domain: managePublicDomain,
        cert_public_domains: publicDomains,
        cert_public_issuer_mode: publicIssuerMode as 'acme' | 'self_signed' | '',
        cert_public_acme_shared: publicAcmeShared,
        cert_public_acme_email: publicAcmeEmail,
        cert_public_acme_directory_url: publicAcmeDirectoryUrl,
        cert_public_acme_weekly_limit: publicAcmeWeeklyLimit,
      });
      setSettings(updated);
      setPendingManagePublicDomain(null);
      setPendingPublicDomains(null);
      setPendingPublicIssuerMode(null);
      setPendingPublicAcmeShared(null);
      setPendingPublicAcmeEmail(null);
      setPendingPublicAcmeDirectoryUrl(null);
      setPendingPublicAcmeWeeklyLimit(null);
      showSuccess(t.systemSaved);
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setPublicBusy(false);
    }
  }

  // Task-9 export actions: per-public-domain bundle/private-key downloads,
  // using the STORED domain list (settings.cert_public_domains) -- an
  // unsaved, just-typed domain in the CSV field above has no issued
  // certificate yet, so it must not get a download button.
  async function downloadPublicBundle(domain: string) {
    try {
      const pem = await api.publicCertificateBundle(domain);
      downloadText(`public-${domain}-fullchain.pem`, pem, 'application/x-pem-file');
    } catch (err) {
      showError(formatPortalError(err, t));
    }
  }
  async function downloadPublicKey(domain: string) {
    try {
      const pem = await api.publicCertificateKey(domain);
      downloadText(`public-${domain}-key.pem`, pem, 'application/x-pem-file');
    } catch (err) {
      showError(formatPortalError(err, t));
    }
  }

  // Panel 2/3 data: the certificate inventory + the internal CA, each loaded
  // independently (a failure in one never blocks the other) and reloaded after
  // any action that changes them.
  const [certificates, setCertificates] = useState<CertificateRow[]>([]);
  const [mesh, setMesh] = useState<CertificateMeshStatus>(emptyCertificateMeshStatus);
  // P4: applications the gateway is refusing to downgrade to plaintext http and
  // that are therefore DOWN. Normally empty; non-empty is an outage.
  const [unreachableApps, setUnreachableApps] = useState<HTTPSSwitchUnreachableApp[]>([]);
  const [certificatesLoading, setCertificatesLoading] = useState(true);
  const [ca, setCa] = useState<CertificateCA | null>(null);
  const [bundlePem, setBundlePem] = useState('');
  // Loaded ONLY to compute the F3.2 scope-flip warning below (which servers
  // already opted IN via their per-server override); a load failure just makes
  // that warning conservative (see scopeFlipAffectedCount) rather than failing
  // the view.
  const [servers, setServers] = useState<PortalServer[]>([]);

  async function loadCertificates() {
    try {
      const res = await api.certificates();
      setCertificates(res.data);
      // The fallback keeps tests/rolling upgrades tolerant of an older response;
      // the live gateway always supplies mesh in the additive P3 contract.
      setMesh(res.mesh ?? emptyCertificateMeshStatus());
      setUnreachableApps(res.https_switch?.unreachable_apps ?? []);
    } catch {
      setCertificates([]);
      setMesh(emptyCertificateMeshStatus());
      setUnreachableApps([]);
    } finally {
      setCertificatesLoading(false);
    }
  }

  async function loadCA() {
    try {
      const res = await api.certificateCA();
      setCa(res.ca);
      setBundlePem(res.bundle_pem);
    } catch {
      setCa(null);
      setBundlePem('');
    }
  }

  async function loadServers() {
    try {
      const res = await api.servers();
      setServers(res.data);
    } catch {
      setServers([]);
    }
  }

  useEffect(() => {
    void loadCertificates();
    void loadCA();
    void loadServers();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [api]);

  // F3.2: how many currently-managed server certificates would fall OUT of the
  // desired set (and get pruned -- key and all -- on the next reconcile pass)
  // if the scope switched from "all" to "selected" right now. Mirrors the
  // backend's certManaged(scope, override) for scope="selected": only a server
  // whose override is explicitly "include" survives. A server certificate
  // listed under scope="all" always has an override != "exclude" already (an
  // excluded server never gets managed under "all" in the first place), so the
  // only two possibilities left are "" (the common default -> would be pruned)
  // and "include" (would survive). A server list that hasn't loaded yet (or
  // failed) leaves the override lookup empty, which is the SAFE direction here:
  // every server-kind certificate then counts as affected, so the warning
  // never under-fires.
  const overrideByServerId = useMemo(() => {
    const map = new Map<string, string>();
    for (const s of servers) map.set(s.id, s.certificate_override ?? '');
    return map;
  }, [servers]);
  const scopeFlipAffectedCount = useMemo(
    () =>
      certificates.filter(
        (c) =>
          c.kind === 'server' && c.server_id && overrideByServerId.get(c.server_id) !== 'include',
      ).length,
    [certificates, overrideByServerId],
  );

  // The CA panel shows whenever there IS a CA (any issuer mode — a root
  // switched away from can still be needed by existing certificates) OR the
  // mode is self_signed with none yet (shows the "no CA generated" hint); in
  // acme mode with no CA it is omitted entirely.
  const showCaPanel = ca !== null && (ca.present || issuerMode === 'self_signed');

  // F3.2: confirm-gates ONLY the all -> selected direction of the scope
  // dropdown when it would prune at least one existing server certificate.
  // `pendingServerScope` (which `save()` reads) is set ONLY once the user
  // confirms, so a cancel leaves the dropdown showing the OLD value (`serverScope`
  // still resolves to the prior state) and editing an unrelated field in the
  // meantime can't smuggle the new scope into save() -- there is nowhere else
  // that writes pendingServerScope.
  const [confirmingScopeFlip, setConfirmingScopeFlip] = useState(false);
  const [pendingScopeFlipValue, setPendingScopeFlipValue] = useState<string | null>(null);
  function requestServerScopeChange(next: string) {
    if (
      serverScope === 'all' &&
      next === 'selected' &&
      (certificatesLoading || scopeFlipAffectedCount > 0)
    ) {
      setPendingScopeFlipValue(next);
      setConfirmingScopeFlip(true);
      return;
    }
    setPendingServerScope(next);
  }

  const [confirmingRotateCa, setConfirmingRotateCa] = useState(false);
  async function rotateCA() {
    try {
      await api.rotateCertificateCA();
      await loadCA();
      await loadCertificates();
      showSuccess(t.systemSaved);
    } catch (err) {
      showError(formatPortalError(err, t));
    }
  }

  const [confirmingReissueAll, setConfirmingReissueAll] = useState(false);
  async function reissueAll() {
    try {
      await api.reissueAllCertificates();
      await loadCertificates();
      showSuccess(t.systemSaved);
    } catch (err) {
      showError(formatPortalError(err, t));
    }
  }

  async function renew(domain: string) {
    try {
      await api.renewCertificate(domain);
      await loadCertificates();
    } catch (err) {
      showError(formatPortalError(err, t));
    }
  }

  // The mesh plaintext-refusal gate (cert_mesh_require_tls, P3). Enabling opens a
  // confirm dialog that names every server it would lock out; disabling is never
  // gated and needs no dialog. Sent ALONE (never bundled) so the backend's arming
  // precondition is re-checked cleanly. It is STRICT: once on it refuses plaintext
  // unconditionally until turned off here (or via the env kill switch) -- see §6/§13.
  const [confirmingMeshRequireTLS, setConfirmingMeshRequireTLS] = useState(false);
  async function setMeshRequireTLS(next: boolean) {
    try {
      await api.updateSystemSettings({ cert_mesh_require_tls: next });
      await loadCertificates();
      showSuccess(t.systemSaved);
    } catch (err) {
      showError(formatPortalError(err, t));
    }
  }
  function requestMeshRequireTLS(next: boolean) {
    if (next) {
      setConfirmingMeshRequireTLS(true);
      return;
    }
    void setMeshRequireTLS(false);
  }

  // Encrypted agent-port mode (Task 8, agent-mesh-tls-port). Round-1 fix
  // (Important finding): changing this mode LIVE-rebinds the gateway's
  // agent-mesh listener topology (adds/removes the dedicated TLS bind, flips
  // the primary bind between plaintext-sniffer and raw TLS) -- at least as
  // disruptive as arming cert_mesh_require_tls, which this same panel already
  // gates behind a ConfirmDialog. So this is a two-step flow too: selecting a
  // DIFFERENT value stages it in `pendingMeshTlsMode` + opens the dialog
  // WITHOUT touching `settings` (the SelectField's `value` stays bound to the
  // confirmed `meshTlsMode` below, so a cancel needs no explicit "revert" --
  // the display was never changed in the first place). Only onConfirm does
  // the PUT, sent ALONE (never bundled) like its sibling switch above. The two
  // read-only fields (cert_mesh_tls_port/cert_mesh_tls_separate_active) ride
  // back on the same response and are rendered straight from `settings` -- no
  // separate load.
  const [meshTlsModeBusy, setMeshTlsModeBusy] = useState(false);
  const [confirmingMeshTlsMode, setConfirmingMeshTlsMode] = useState(false);
  const [pendingMeshTlsMode, setPendingMeshTlsMode] = useState<string | null>(null);
  const meshTlsMode = settings?.cert_mesh_tls_mode ?? '';
  async function setMeshTlsMode(next: string) {
    setMeshTlsModeBusy(true);
    try {
      const updated = await api.updateSystemSettings({
        cert_mesh_tls_mode: next as '' | 'combined' | 'separate',
      });
      setSettings(updated);
      showSuccess(t.systemSaved);
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setMeshTlsModeBusy(false);
    }
  }
  function requestMeshTlsMode(next: string) {
    if (next === meshTlsMode) return;
    setPendingMeshTlsMode(next);
    setConfirmingMeshTlsMode(true);
  }

  // P4 global https-auto-switch mode (Task 11). Unlike cert_mesh_tls_mode, a
  // mode change never rebinds a listener -- it only changes the reconcile's
  // forward-looking behavior -- so only the ONE transition that can flip many
  // untouched applications' scheme automatically (switching INTO "auto") is
  // confirm-gated; "manual"/"selected" apply immediately (they only turn auto-
  // switching off or restrict it to servers already explicitly opted in).
  const [httpsSwitchModeBusy, setHttpsSwitchModeBusy] = useState(false);
  const [confirmingHttpsSwitchAuto, setConfirmingHttpsSwitchAuto] = useState(false);
  const httpsSwitchMode = settings?.cert_https_switch_mode ?? 'manual';
  async function setHttpsSwitchMode(next: string) {
    setHttpsSwitchModeBusy(true);
    try {
      const updated = await api.updateSystemSettings({
        cert_https_switch_mode: next as 'manual' | 'auto' | 'selected',
      });
      setSettings(updated);
      showSuccess(t.systemSaved);
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setHttpsSwitchModeBusy(false);
    }
  }
  function requestHttpsSwitchMode(next: string) {
    if (next === httpsSwitchMode) return;
    if (next === 'auto') {
      setConfirmingHttpsSwitchAuto(true);
      return;
    }
    void setHttpsSwitchMode(next);
  }

  // cert_proxy_listen_port_base: a normal numeric setting alongside the mode
  // above -- no confirm, saved via its own small Save button (sent ALONE, same
  // as the mode, so it never accidentally bundles an in-progress mode edit).
  const [pendingProxyListenPortBase, setPendingProxyListenPortBase] = useState<string | null>(null);
  const [proxyListenPortBaseBusy, setProxyListenPortBaseBusy] = useState(false);
  const proxyListenPortBaseValue =
    pendingProxyListenPortBase ??
    String(settings?.cert_proxy_listen_port_base ?? DEFAULT_CERT_PROXY_LISTEN_PORT_BASE);
  const proxyListenPortBaseNum = Number(proxyListenPortBaseValue);
  const proxyListenPortBaseValid =
    Number.isInteger(proxyListenPortBaseNum) &&
    proxyListenPortBaseNum >= MIN_CERT_PROXY_LISTEN_PORT_BASE &&
    proxyListenPortBaseNum <= MAX_CERT_PROXY_LISTEN_PORT_BASE;
  async function saveProxyListenPortBase() {
    if (!proxyListenPortBaseValid) return;
    setProxyListenPortBaseBusy(true);
    try {
      const updated = await api.updateSystemSettings({
        cert_proxy_listen_port_base: proxyListenPortBaseNum,
      });
      setSettings(updated);
      setPendingProxyListenPortBase(null);
      showSuccess(t.systemSaved);
    } catch (err) {
      showError(formatPortalError(err, t));
    } finally {
      setProxyListenPortBaseBusy(false);
    }
  }

  const certColumns: ListColumn<CertificateRow>[] = [
    { id: 'domain', label: t.certificatesColDomain, value: (r) => r.domain, filter: 'text' },
    {
      id: 'kind',
      label: t.certificatesColKind,
      value: (r) => r.kind,
      filter: 'enum',
      enumLabel: (v) => certKindLabel(t, v),
      render: (r) => certKindLabel(t, r.kind),
    },
    {
      id: 'status',
      label: t.certificatesColStatus,
      value: (r) => r.status,
      filter: 'enum',
      enumLabel: (v) => certStatusLabel(t, v),
      render: (r) => (
        <StatusChip status={certStatusBadge(r.status)} label={certStatusLabel(t, r.status)} />
      ),
    },
    {
      id: 'issued_at',
      label: t.certificatesColIssued,
      value: (r) => (r.issued_at ? new Date(r.issued_at).toLocaleString() : ''),
    },
    {
      id: 'not_after',
      label: t.certificatesColExpires,
      value: (r) => (r.not_after ? new Date(r.not_after).toLocaleString() : ''),
    },
    {
      id: 'remaining',
      label: t.certificatesColRemaining,
      numeric: true,
      value: (r) => {
        const days = remainingDays(r.not_after);
        return days === null ? '' : String(days);
      },
      render: (r) => {
        const days = remainingDays(r.not_after);
        if (days === null) return '';
        const severity = severityFor(days, renewBeforeDaysNum);
        return (
          <Box
            component="span"
            data-testid={`cert-remaining-${r.domain}`}
            data-severity={severity}
            sx={{ color: severityColor(severity) }}
          >
            {days}
          </Box>
        );
      },
    },
    // Between "Restlaufzeit" and "Letzter Fehler": what the ServerAgent actually
    // has on disk, which is the one thing the certificate list could not show
    // before Phase 2 distribution.
    {
      id: 'installed',
      label: t.certificatesColInstalled,
      value: (r) => installedSymbol(installedState(r)),
      render: (r) => {
        const state = installedState(r);
        return (
          <Box
            component="span"
            data-testid={`cert-installed-${r.domain}`}
            data-state={state}
            title={installedTitle(t, r, state)}
            sx={{ color: state === 'no' ? 'warning.main' : undefined }}
          >
            {installedSymbol(state)}
          </Box>
        );
      },
    },
    // Next to "Installiert": which transport the ServerAgent last used on the mesh
    // listener (Phase 3). ✓ TLS / ✗ Klartext / — never observed.
    {
      id: 'transport',
      label: t.certificatesColTransport,
      value: (r) => transportLabel(t, transportStateOf(r)),
      render: (r) => {
        const state = transportStateOf(r);
        return (
          <Box
            component="span"
            data-testid={`cert-transport-${r.domain}`}
            data-state={state}
            title={transportTitle(t, r, state)}
            sx={{ color: state === 'plain' ? 'warning.main' : undefined }}
          >
            {transportLabel(t, state)}
          </Box>
        );
      },
    },
    {
      id: 'last_error',
      label: t.certificatesColError,
      value: (r) => r.last_error ?? '',
      filter: 'text',
    },
    // F3.3: hidden-by-default diagnostics for a domain stuck in `error` with a
    // backoff -- how many attempts have failed and when the next one is due.
    {
      id: 'attempt_count',
      label: t.certificatesColAttempts,
      numeric: true,
      value: (r) => String(r.attempt_count),
      defaultHidden: true,
    },
    {
      id: 'next_attempt_at',
      label: t.certificatesColNextAttempt,
      value: (r) => (r.next_attempt_at ? new Date(r.next_attempt_at).toLocaleString() : ''),
      defaultHidden: true,
    },
    {
      id: 'fingerprint',
      label: t.certificatesColFingerprint,
      value: (r) => r.fingerprint ?? '',
      filter: 'text',
      defaultHidden: true,
    },
  ];

  const certActions = (row: CertificateRow): RowAction[] => [
    {
      key: 'renew',
      label: t.certificatesRenewNow,
      icon: <AutorenewIcon fontSize="small" />,
      onClick: () => void renew(row.domain),
    },
  ];

  const caRemaining = ca ? remainingDays(ca.not_after) : null;
  const caSeverity = severityFor(caRemaining, caRenewBeforeDaysNum);

  return (
    <>
      <PageTitle title={t.settingsCertificatesTitle} />
      {loading && <p>{t.loading}</p>}
      <Box
        data-testid="certificate-mesh-status"
        sx={{ mb: 2.5, px: 2, py: 1.5, border: 1, borderColor: 'divider', borderRadius: 1 }}
      >
        <Typography variant="h6">{t.certificatesMeshTitle}</Typography>
        <Typography variant="body2" color={mesh.tls_active ? 'success.main' : 'text.secondary'}>
          {mesh.tls_active ? t.certificatesMeshTLSActive : t.certificatesMeshTLSInactive}
        </Typography>
        {mesh.address && (
          <Typography variant="body2">
            {t.certificatesMeshAddress}: {mesh.address}
          </Typography>
        )}
        {mesh.fingerprint && (
          <Typography variant="body2" sx={{ overflowWrap: 'anywhere' }}>
            {t.certificatesMeshFingerprint}: {mesh.fingerprint}
          </Typography>
        )}
        {mesh.not_after && (
          <Typography variant="body2">
            {t.certificatesMeshExpires}: {new Date(mesh.not_after).toLocaleString()}
          </Typography>
        )}
        <FormControlLabel
          sx={{ mt: 1 }}
          control={
            <Switch
              checked={!!mesh.require_tls}
              // Arming needs a fresh TLS observation; disarming is always allowed.
              disabled={!mesh.require_tls && !mesh.tls_observed}
              onChange={(e) => requestMeshRequireTLS(e.target.checked)}
              data-testid="certificate-mesh-require-tls"
            />
          }
          label={t.certificatesMeshRequireTLS}
        />
        <Typography variant="caption" color="text.secondary" sx={{ display: 'block' }}>
          {/* Show "not yet available" ONLY when the toggle is actually locked
              (same condition as the Switch's disabled) -- an already-armed gate
              stays bedienbar (to disarm), so it must never show that hint. */}
          {!mesh.require_tls && !mesh.tls_observed
            ? t.certificatesMeshRequireTLSNotObserved
            : t.certificatesMeshRequireTLSHint}
        </Typography>
        <SelectField
          id="certificate-mesh-tls-port-mode"
          label={t.certificatesMeshTLSPortMode}
          value={meshTlsMode}
          onChange={(e) => requestMeshTlsMode(e.target.value)}
          disabled={meshTlsModeBusy}
          sx={{ mt: 2, maxWidth: 360 }}
        >
          <option value="">{t.certificatesMeshTLSPortModeFollowEnv}</option>
          <option value="combined">{t.certificatesMeshTLSPortModeCombined}</option>
          <option value="separate">{t.certificatesMeshTLSPortModeSeparate}</option>
        </SelectField>
        {/* Read-only: server-computed, never sent back on a PUT. */}
        <Box data-testid="certificate-mesh-tls-port-status" sx={{ mt: 1 }}>
          <Typography variant="body2">
            {t.certificatesMeshTLSPort}: {settings?.cert_mesh_tls_port ?? 0}
          </Typography>
          <Typography
            variant="body2"
            color={settings?.cert_mesh_tls_separate_active ? 'success.main' : 'text.secondary'}
          >
            {settings?.cert_mesh_tls_separate_active
              ? t.certificatesMeshTLSPortSeparateActive
              : t.certificatesMeshTLSPortSeparateInactive}
          </Typography>
        </Box>
      </Box>
      {/* The gateway never downgrades an application to plaintext http on its
          own, so a broken agent TLS listener is an OUTAGE rather than a silent
          switch to unencrypted inference. That trade is only defensible if the
          outage is visible without anyone knowing to look for it, which is what
          this alert is. severity="error", not "warning": these applications are
          down right now. */}
      {unreachableApps.length > 0 && (
        <Alert severity="error" data-testid="certificate-https-switch-unreachable" sx={{ mb: 2.5 }}>
          <AlertTitle>{t.certificatesHTTPSSwitchUnreachableTitle}</AlertTitle>
          {t.certificatesHTTPSSwitchUnreachableBody}
          <ul style={{ margin: '0.5em 0 0', paddingInlineStart: '1.25em' }}>
            {unreachableApps.map((app) => (
              <li key={`${app.server_id}:${app.app_id}`}>
                <strong>
                  {app.server_name || app.server_id} — {app.app_type} :{app.proxy_listen_port}
                </strong>
                {app.route_state ? ` (${app.route_state})` : ''} — {app.action}
              </li>
            ))}
          </ul>
        </Alert>
      )}
      {/* P4 Task 11: global https-auto-switch mode + the proxy-listen-port
          floor, alongside it. Independent of the mesh box above -- this is a
          separate P4 feature (agent TLS proxy + scheme auto-switch), not the
          agent-mesh-port topology. */}
      <Box
        data-testid="certificate-https-switch-status"
        sx={{ mb: 2.5, px: 2, py: 1.5, border: 1, borderColor: 'divider', borderRadius: 1 }}
      >
        <Typography variant="h6">{t.certificatesHTTPSSwitchTitle}</Typography>
        <SelectField
          id="certificate-https-switch-mode"
          label={t.certificatesHTTPSSwitchMode}
          value={httpsSwitchMode}
          onChange={(e) => requestHttpsSwitchMode(e.target.value)}
          disabled={httpsSwitchModeBusy}
          sx={{ mt: 1, maxWidth: 360 }}
        >
          <option value="manual">{t.certificatesHTTPSSwitchModeManual}</option>
          <option value="auto">{t.certificatesHTTPSSwitchModeAuto}</option>
          <option value="selected">{t.certificatesHTTPSSwitchModeSelected}</option>
        </SelectField>
        <Box sx={{ display: 'flex', alignItems: 'flex-end', gap: 1, mt: 2, maxWidth: 360 }}>
          <Field
            id="certificate-proxy-listen-port-base"
            type="number"
            label={t.certificatesProxyListenPortBase}
            value={proxyListenPortBaseValue}
            onChange={(e) => setPendingProxyListenPortBase(e.target.value)}
            inputProps={{
              min: MIN_CERT_PROXY_LISTEN_PORT_BASE,
              max: MAX_CERT_PROXY_LISTEN_PORT_BASE,
            }}
          />
          <Button
            variant="outlined"
            disabled={proxyListenPortBaseBusy || !proxyListenPortBaseValid}
            onClick={() => void saveProxyListenPortBase()}
          >
            {t.save}
          </Button>
        </Box>
      </Box>
      {mesh.ca_rotation_pending_servers.length > 0 && (
        <Alert severity="warning" data-testid="certificate-mesh-ca-pending" sx={{ mb: 2.5 }}>
          <AlertTitle>{t.certificatesMeshCARotationPendingTitle}</AlertTitle>
          {t.certificatesMeshCARotationPending}:{' '}
          {mesh.ca_rotation_pending_servers.map((server) => server.name).join(', ')}
        </Alert>
      )}
      {/* F3.1: the reconcile's module-level abort note (backend cert_last_error) --
          set when a pass gave up before it could place or renew ANY order, so an
          operator otherwise sees this indistinguishably from "not reconciled yet".
          Rendered here, OUTSIDE the CA panel, so it is visible in BOTH issuer
          modes and even with no CA present at all (ca.last_error is populated by
          CertificateCAView regardless of `present`/mode). */}
      {ca?.last_error && (
        <Alert severity="error" data-testid="cert-last-error" sx={{ mb: 2.5 }}>
          <AlertTitle>{t.certificatesLastErrorTitle}</AlertTitle>
          {ca.last_error}
        </Alert>
      )}
      <Stack spacing={2.5}>
        {/* 1. Einstellungen: issuer mode + mode-dependent fields + shared config. */}
        <Panel
          titleId="cert-settings-heading"
          title={t.certificatesSettingsTitle}
          subtitle={t.settingsCertificatesIntro}
        >
          <Stack spacing={3}>
            <Box>
              <SelectField
                id="cert-issuer-mode"
                label={t.settingsCertIssuerMode}
                value={issuerMode}
                onChange={(e) => setPendingIssuerMode(e.target.value)}
              >
                <option value="acme">{t.settingsCertIssuerAcme}</option>
                <option value="self_signed">{t.settingsCertIssuerSelfSigned}</option>
              </SelectField>
              <Typography
                variant="caption"
                color="text.secondary"
                sx={{ display: 'block', mt: 0.5 }}
              >
                {t.certificatesSwitchHint}
              </Typography>
            </Box>

            {issuerMode === 'acme' ? (
              <>
                <Field
                  id="cert-acme-email"
                  label={t.settingsAcmeEmail}
                  value={acmeEmail}
                  onChange={(e) => setPendingAcmeEmail(e.target.value)}
                />
                <SelectField
                  id="cert-acme-directory"
                  label={t.settingsAcmeDirectory}
                  value={acmeDirectoryChoice}
                  onChange={(e) => {
                    const choice = e.target.value as AcmeDirectoryChoice;
                    if (choice === 'production')
                      setPendingAcmeDirectoryUrl(ACME_DIRECTORY_PRODUCTION);
                    else if (choice === 'staging')
                      setPendingAcmeDirectoryUrl(ACME_DIRECTORY_STAGING);
                    else
                      setPendingAcmeDirectoryUrl(
                        acmeDirectoryChoice === 'custom' ? acmeDirectoryUrl : '',
                      );
                  }}
                >
                  <option value="production">{t.settingsAcmeDirectoryProduction}</option>
                  <option value="staging">{t.settingsAcmeDirectoryStaging}</option>
                  <option value="custom">{t.settingsAcmeDirectoryCustom}</option>
                </SelectField>
                {acmeDirectoryChoice === 'custom' && (
                  <Field
                    id="cert-acme-directory-custom"
                    label={t.settingsAcmeDirectoryCustom}
                    value={acmeDirectoryUrl}
                    onChange={(e) => setPendingAcmeDirectoryUrl(e.target.value)}
                  />
                )}
              </>
            ) : (
              <>
                <Field
                  id="cert-self-signed-validity"
                  type="number"
                  label={t.settingsCertSelfSignedValidity}
                  value={selfSignedValidityValue}
                  onChange={(e) => setPendingSelfSignedValidity(e.target.value)}
                  inputProps={{
                    min: MIN_SELF_SIGNED_VALIDITY_DAYS,
                    max: MAX_SELF_SIGNED_VALIDITY_DAYS,
                    step: 1,
                  }}
                />
                <Field
                  id="cert-ca-renew-before-days"
                  type="number"
                  label={t.settingsCertCaRenewBeforeDays}
                  value={caRenewBeforeDaysValue}
                  onChange={(e) => setPendingCaRenewBeforeDays(e.target.value)}
                  inputProps={{ min: MIN_CA_RENEW_BEFORE_DAYS, step: 1 }}
                />
              </>
            )}

            <Field
              id="cert-base-domain"
              label={t.settingsCertBaseDomain}
              value={baseDomain}
              onChange={(e) => setPendingBaseDomain(e.target.value)}
            />
            <Field
              id="cert-gateway-domain"
              label={t.settingsCertGatewayDomain}
              value={gatewayDomain}
              onChange={(e) => setPendingGatewayDomain(e.target.value)}
            />
            <SelectField
              id="cert-server-scope"
              label={t.settingsCertServerScope}
              value={serverScope}
              onChange={(e) => requestServerScopeChange(e.target.value)}
            >
              <option value="all">{t.settingsCertScopeAll}</option>
              <option value="selected">{t.settingsCertScopeSelected}</option>
            </SelectField>
            <Field
              id="cert-renew-before-days"
              type="number"
              label={t.settingsCertRenewBeforeDays}
              value={renewBeforeDaysValue}
              onChange={(e) => setPendingRenewBeforeDays(e.target.value)}
              inputProps={{ min: MIN_CERT_RENEW_BEFORE_DAYS, step: 1 }}
            />
          </Stack>
          <Box sx={{ display: 'flex', justifyContent: 'flex-end', mt: 2 }}>
            <Button
              type="button"
              variant="contained"
              disabled={busy || loading || !configValid}
              onClick={save}
            >
              {t.save}
            </Button>
          </Box>
        </Panel>

        {/* Edge (gateway-nginx) certificate — the TLS leg between the upstream
            reverse proxy and this gateway's own nginx. A fully separate row/mode
            from the internal (mesh) settings above, hence its own panel with its
            own switch/issuer/names + delivery/download/reissue controls. */}
        <EdgeCertificatePanel t={t} api={api} />

        {/* U-T4: "Öffentliche Domains" — the second half of the unified
            publicly-trusted-certificates area (alongside the edge panel above).
            A DISJOINT save partition from panel 1: the manage toggle/domain list
            (moved out of panel 1) + the public issuer mode + AcmeConfigFields'
            shared-vs-own ACME account for this context, plus the Task-9
            per-domain bundle/key export actions. */}
        <Panel titleId="cert-public-heading" title={t.settingsCertPublicTitle}>
          <Stack spacing={3}>
            <Box>
              <FormControlLabel
                control={
                  <Switch
                    checked={managePublicDomain}
                    onChange={(e) => setPendingManagePublicDomain(e.target.checked)}
                  />
                }
                label={t.settingsCertManagePublicDomain}
              />
            </Box>
            <Field
              id="cert-public-domains"
              label={t.settingsCertPublicDomains}
              value={publicDomainsCsv}
              onChange={(e) => setPendingPublicDomains(e.target.value)}
            />
            <SelectField
              id="cert-public-issuer-mode"
              label={t.settingsCertPublicIssuerMode}
              value={publicIssuerMode}
              onChange={(e) => setPendingPublicIssuerMode(e.target.value)}
            >
              {/* Round-1 review fix (byte-neutrality): "" is cert_public_issuer_mode's
                  own legal "follow the global/internal issuer mode" value, not an
                  absent/placeholder state -- offering it as a real, selectable,
                  default-selected option means leaving it alone (e.g. only
                  toggling the manage checkbox and saving) preserves "" instead of
                  silently pinning "acme". */}
              <option value="">{t.settingsCertPublicIssuerModeFollowGlobal}</option>
              <option value="acme">{t.settingsCertIssuerAcme}</option>
              <option value="self_signed">{t.settingsCertIssuerSelfSigned}</option>
            </SelectField>
            <AcmeConfigFields
              prefix="public"
              t={t}
              values={{
                shared: publicAcmeShared,
                email: publicAcmeEmail,
                directoryUrl: publicAcmeDirectoryUrl,
                weeklyLimit: publicAcmeWeeklyLimit,
              }}
              onChange={handlePublicAcmeChange}
            />
          </Stack>
          <Box sx={{ display: 'flex', justifyContent: 'flex-end', mt: 2 }}>
            <Button
              type="button"
              variant="contained"
              disabled={publicBusy || loading}
              onClick={savePublic}
            >
              {t.save}
            </Button>
          </Box>
          {(settings?.cert_public_domains ?? []).length > 0 && (
            <Stack spacing={1} sx={{ mt: 3 }}>
              {(settings?.cert_public_domains ?? []).map((domain) => (
                <Box
                  key={domain}
                  data-testid={`cert-public-domain-${domain}`}
                  sx={{ display: 'flex', alignItems: 'center', gap: 1.5, flexWrap: 'wrap' }}
                >
                  <Typography variant="body2" sx={{ minWidth: 200 }}>
                    {domain}
                  </Typography>
                  <Button
                    type="button"
                    size="small"
                    variant="outlined"
                    startIcon={<DownloadIcon fontSize="small" />}
                    onClick={() => void downloadPublicBundle(domain)}
                  >
                    {t.certificatesPublicButtonDownloadBundle}
                  </Button>
                  <Button
                    type="button"
                    size="small"
                    variant="outlined"
                    startIcon={<DownloadIcon fontSize="small" />}
                    onClick={() => void downloadPublicKey(domain)}
                  >
                    {t.certificatesPublicButtonDownloadKey}
                  </Button>
                </Box>
              ))}
            </Stack>
          )}
        </Panel>

        {/* 2. Interne CA — shown whenever a CA is present (any issuer mode) or the
            mode is self_signed with none yet; omitted entirely in acme mode with no
            CA. The re-issue-all action below is MODE-INDEPENDENT and always renders,
            regardless of whether this panel does. */}
        {showCaPanel && ca && (
          <Panel titleId="cert-ca-heading" title={t.certificatesCaTitle}>
            {ca.present ? (
              <Stack spacing={1.5}>
                {issuerMode === 'acme' && (
                  <Typography variant="body2" color="warning.main">
                    {t.certificatesCaStillNeeded}
                  </Typography>
                )}
                <Box>
                  <Typography variant="body2" color="text.secondary">
                    {t.certificatesCaSubject}
                  </Typography>
                  <Typography variant="body1">{ca.subject}</Typography>
                </Box>
                <Box>
                  <Typography variant="body2" color="text.secondary">
                    {t.certificatesColIssued}
                  </Typography>
                  <Typography variant="body1">
                    {ca.not_before ? new Date(ca.not_before).toLocaleString() : ''}
                  </Typography>
                </Box>
                <Box>
                  <Typography variant="body2" color="text.secondary">
                    {t.certificatesColExpires}
                  </Typography>
                  <Typography variant="body1">
                    {ca.not_after ? new Date(ca.not_after).toLocaleString() : ''}
                  </Typography>
                </Box>
                <Box>
                  <Typography variant="body2" color="text.secondary">
                    {t.certificatesColRemaining}
                  </Typography>
                  <Typography variant="body1" sx={{ color: severityColor(caSeverity) }}>
                    {caRemaining ?? ''}
                  </Typography>
                </Box>
                {ca.previous_fingerprint && (
                  <Typography variant="body2" color="text.secondary">
                    {t.certificatesCaPrevious}: {ca.previous_fingerprint}
                    {ca.previous_not_after
                      ? ` — ${new Date(ca.previous_not_after).toLocaleString()}`
                      : ''}
                  </Typography>
                )}
                <Typography variant="caption" color="text.secondary">
                  {t.certificatesCaHint}
                </Typography>
                <Box sx={{ display: 'flex', gap: 1.5, flexWrap: 'wrap' }}>
                  <Button
                    type="button"
                    variant="outlined"
                    startIcon={<DownloadIcon fontSize="small" />}
                    onClick={() =>
                      downloadText(
                        'op-ai-gateway-root-ca.pem',
                        firstPemBlock(bundlePem),
                        'application/x-pem-file',
                      )
                    }
                  >
                    {t.certificatesCaDownloadRoot}
                  </Button>
                  <Button
                    type="button"
                    variant="outlined"
                    startIcon={<DownloadIcon fontSize="small" />}
                    onClick={() =>
                      downloadText(
                        'op-ai-gateway-ca-bundle.pem',
                        bundlePem,
                        'application/x-pem-file',
                      )
                    }
                  >
                    {t.certificatesCaDownloadBundle}
                  </Button>
                  <Button
                    type="button"
                    variant="outlined"
                    color="error"
                    onClick={() => setConfirmingRotateCa(true)}
                  >
                    {t.certificatesCaRotate}
                  </Button>
                </Box>
              </Stack>
            ) : (
              <Typography variant="body2" color="text.secondary">
                {t.certificatesCaNone}
              </Typography>
            )}
          </Panel>
        )}
        <Box sx={{ display: 'flex', justifyContent: 'flex-end' }}>
          <Button
            type="button"
            variant="outlined"
            color="error"
            onClick={() => setConfirmingReissueAll(true)}
          >
            {t.certificatesReissueAll}
          </Button>
        </Box>

        {/* 3. Zertifikate: the full inventory, client-computed remaining-validity
            column, per-row "renew now" action. */}
        <Panel titleId="cert-list-heading" title={t.certificates}>
          <ListTable<CertificateRow>
            rows={certificates}
            columns={certColumns}
            rowKey={(r) => r.domain}
            actions={certActions}
            storageKey="op.certificates"
            loading={certificatesLoading}
            labels={listTableLabels(t, { empty: t.certificatesEmpty })}
          />
        </Panel>
      </Stack>

      <ConfirmDialog
        open={confirmingRotateCa}
        title={t.certificatesCaRotateConfirmTitle}
        body={t.certificatesCaRotateConfirmBody}
        confirmLabel={t.confirm}
        cancelLabel={t.cancel}
        onConfirm={() => {
          setConfirmingRotateCa(false);
          void rotateCA();
        }}
        onCancel={() => setConfirmingRotateCa(false)}
      />

      <ConfirmDialog
        open={confirmingReissueAll}
        title={t.certificatesReissueAllConfirmTitle}
        body={t.certificatesReissueAllConfirmBody}
        confirmLabel={t.confirm}
        cancelLabel={t.cancel}
        onConfirm={() => {
          setConfirmingReissueAll(false);
          void reissueAll();
        }}
        onCancel={() => setConfirmingReissueAll(false)}
      />

      <ConfirmDialog
        open={confirmingScopeFlip}
        title={t.certificatesScopeFlipConfirmTitle}
        body={t.certificatesScopeFlipConfirmBody}
        confirmLabel={t.confirm}
        cancelLabel={t.cancel}
        onConfirm={() => {
          setConfirmingScopeFlip(false);
          if (pendingScopeFlipValue !== null) setPendingServerScope(pendingScopeFlipValue);
          setPendingScopeFlipValue(null);
        }}
        onCancel={() => {
          setConfirmingScopeFlip(false);
          setPendingScopeFlipValue(null);
        }}
      />

      <ConfirmDialog
        open={confirmingMeshRequireTLS}
        title={t.certificatesMeshRequireTLSConfirmTitle}
        body={t.certificatesMeshRequireTLSConfirmBody}
        extra={
          mesh.tls_pending_servers && mesh.tls_pending_servers.length > 0 ? (
            <Alert
              severity="warning"
              sx={{ mt: 1 }}
              data-testid="certificate-mesh-require-tls-pending"
            >
              <AlertTitle>{t.certificatesMeshRequireTLSPending}</AlertTitle>
              {mesh.tls_pending_servers.map((s) => s.name).join(', ')}
            </Alert>
          ) : undefined
        }
        confirmLabel={t.confirm}
        cancelLabel={t.cancel}
        onConfirm={() => {
          setConfirmingMeshRequireTLS(false);
          void setMeshRequireTLS(true);
        }}
        onCancel={() => setConfirmingMeshRequireTLS(false)}
      />

      <ConfirmDialog
        open={confirmingMeshTlsMode}
        title={t.certificatesMeshTLSPortModeConfirmTitle}
        body={t.certificatesMeshTLSPortModeConfirmBody}
        confirmLabel={t.confirm}
        cancelLabel={t.cancel}
        onConfirm={() => {
          setConfirmingMeshTlsMode(false);
          if (pendingMeshTlsMode !== null) void setMeshTlsMode(pendingMeshTlsMode);
          setPendingMeshTlsMode(null);
        }}
        onCancel={() => {
          setConfirmingMeshTlsMode(false);
          setPendingMeshTlsMode(null);
        }}
      />

      <ConfirmDialog
        open={confirmingHttpsSwitchAuto}
        title={t.certificatesHTTPSSwitchModeConfirmTitle}
        body={t.certificatesHTTPSSwitchModeConfirmBody}
        confirmLabel={t.confirm}
        cancelLabel={t.cancel}
        onConfirm={() => {
          setConfirmingHttpsSwitchAuto(false);
          void setHttpsSwitchMode('auto');
        }}
        onCancel={() => setConfirmingHttpsSwitchAuto(false)}
      />
    </>
  );
}
