// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { CertificateSettings } from './CertificateSettings';
import { ToastProvider } from './shared/ToastProvider';
import { messages } from '../i18n';
import type {
  CertificateCA,
  CertificateMeshStatus,
  CertificateRow,
  EdgeCertificate,
  PortalServer,
  SystemSettings as SystemSettingsDTO,
} from '../api';
import type { PortalApi } from './shared/types';

type CertificateSettingsApi = Pick<
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

// NOTE on test infra: this repo does NOT have @testing-library/user-event
// installed (verified: absent from package.json and node_modules, and no
// other *.test.tsx in the suite imports it) — every interaction below uses
// the existing fireEvent/waitFor idiom the rest of the suite uses instead.

const t = messages.de;
const de = t;

function settingsFixture(overrides: Partial<SystemSettingsDTO> = {}): SystemSettingsDTO {
  return {
    theme: 'default',
    available_themes: [{ id: 'default', name: 'Default' }],
    language: 'de',
    available_languages: ['de'],
    capture_retention_days: 30,
    capture_enabled: true,
    capture_override: false,
    health_check_interval_seconds: 30,
    agent_presence_timeout_seconds: 15,
    smtp_enabled: false,
    smtp_host: '',
    smtp_port: 587,
    smtp_username: '',
    smtp_password_set: false,
    smtp_from: '',
    smtp_from_name: '',
    smtp_tls_mode: 'starttls',
    totp_mode: 'off',
    route_affinity_session_mode: 'client_session',
    vision_probe_mode: 'accept',
    energy_default_price_per_kwh: 0,
    energy_default_pue: 0,
    energy_default_wh_per_token: 0,
    currency_usd_per_eur: 0,
    energy_default_price_unit: 'eur_cent',
    netbird_enabled: false,
    netbird_url: '',
    netbird_groups: [],
    netbird_token_set: false,
    netbird_only: false,
    netbird_gateway_peer_id: '',
    netbird_gateway_peer_name: '',
    netbird_manage_policies: false,
    netbird_policy_scope: 'auto',
    netbird_effective_policy_scope: 'selected',
    netbird_deny_by_default: false,
    netbird_deny_by_default_enforce: false,
    netbird_peer_sync_interval_seconds: 30,
    netbird_reconcile_interval_seconds: 60,
    netbird_allow_ping_gateway: false,
    netbird_allow_ping_all_servers: false,
    netbird_token_rotate_before_days: 14,
    netbird_agent_download_only: false,
    system_admin_mode_require_password: true,
    resource_provisioning_enforce: false,
    cert_enabled: false,
    cert_issuer_mode: 'acme',
    cert_self_signed_validity_days: 365,
    cert_ca_renew_before_days: 365,
    acme_email: '',
    acme_directory_url: 'https://acme-v02.api.letsencrypt.org/directory',
    cert_base_domain: '',
    cert_gateway_domain: '',
    cert_server_scope: 'selected',
    cert_manage_public_domain: false,
    cert_public_domains: [],
    cert_renew_before_days: 30,
    ...overrides,
  };
}

// Minimal valid PortalServer fixture (mirrors the same-purpose helper in
// TokenList.test.tsx / ServerList.test.tsx) -- used only to drive the F3.2
// scope-flip warning's per-server certificate_override lookup.
function serverFixture(overrides: Partial<PortalServer> = {}): PortalServer {
  return {
    id: 'srv_a',
    name: 'Server A',
    domain: 'srv-a.example.test',
    server_path_suffix: '',
    status: 'active',
    health_status: 'healthy',
    owners: [],
    last_seen_at: null,
    created_at: '2026-08-12T12:00:00Z',
    netbird_enabled: true,
    netbird_setup_key_id: '',
    netbird_group_id: '',
    netbird_peer_id: '',
    netbird_connected: false,
    netbird_group_ids: [],
    netbird_peer_managed: false,
    netbird_policy_override: '',
    netbird_allow_ping: false,
    netbird_ping_exclude: false,
    agent_status: 'unconfigured',
    agent_presence_timeout_seconds: 0,
    estimated_watts: 0,
    idle_watts: 0,
    price_per_kwh: 0,
    pue: 0,
    price_unit: 'eur_cent',
    admin_groups: [],
    system_group_id: '',
    system_group_name: '',
    ...overrides,
  };
}

function makeApi(overrides: Partial<CertificateSettingsApi> = {}): CertificateSettingsApi {
  return {
    getSystemSettings: vi.fn(async () => settingsFixture()),
    updateSystemSettings: vi.fn(async (_body: Record<string, unknown>) => settingsFixture()),
    certificates: vi.fn(async () => ({ data: [] as CertificateRow[] })),
    certificateCA: vi.fn(async () => ({ ca: { present: false } as CertificateCA, bundle_pem: '' })),
    servers: vi.fn(async () => ({ data: [] as PortalServer[] })),
    rotateCertificateCA: vi.fn(async () => ({ ok: true })),
    reissueAllCertificates: vi.fn(async () => ({ ok: true })),
    renewCertificate: vi.fn(async () => ({ ok: true })),
    // CertificateSettings now mounts EdgeCertificatePanel, which loads this on
    // its own mount -- a bare "nothing configured yet" default so it renders
    // without erroring (EdgeCertificatePanel.test.tsx exercises it in depth).
    edgeCertificate: vi.fn(async (): Promise<EdgeCertificate> => ({
      enabled: false,
      issuer_mode: 'acme',
      names: [],
      delivery_mode: 'download',
      key_download_available: false,
      require_https: false,
      https_observed: false,
    })),
    reissueEdgeCertificate: vi.fn(async () => ({ ok: true })),
    edgeCertificateBundle: vi.fn(async () => ''),
    edgeCertificateKey: vi.fn(async () => ''),
    edgeProxyConfig: vi.fn(async () => ''),
    publicCertificateBundle: vi.fn(async () => ''),
    publicCertificateKey: vi.fn(async () => ''),
    // Not exercised by most tests here (EdgeCertificatePanel.test.tsx covers the
    // probe button in depth); a bare non-blocking default keeps the panel quiet.
    probeEdgeTLS: vi.fn(async () => ({ ok: true, target: 'web:443' })),
    ...overrides,
  };
}

function renderCertificateSettings(api: CertificateSettingsApi) {
  render(
    <ToastProvider>
      <CertificateSettings t={t} api={api} />
    </ToastProvider>,
  );
}

// Panel 1 (Einstellungen) now shares the view with EdgeCertificatePanel, which
// renders its OWN "Speichern" button in a sibling section -- an ambiguous
// getByRole("button", { name: de.save }) would find both. Each Panel renders as
// an ARIA region named by its heading (mirrors NetbirdSettings.test.tsx's
// sectionSaveButton), so scope to Panel 1's region explicitly.
function settingsSaveButton(): HTMLElement {
  const region = screen.getByRole('region', { name: de.certificatesSettingsTitle });
  return within(region).getByRole('button', { name: de.save });
}

// Drives the non-native MUI Select (SelectField): open the popup, click the option.
async function pickServerScope(label: string) {
  fireEvent.mouseDown(await screen.findByLabelText(de.settingsCertServerScope));
  fireEvent.click(await screen.findByRole('option', { name: label }));
}

// U-T4: the "Öffentliche Domains" panel is its own ARIA region with its own
// "Speichern" button -- same disambiguation need as settingsSaveButton() above
// (this view now has THREE independent save buttons: panel 1, the edge panel,
// and this one).
function publicRegion(): HTMLElement {
  return screen.getByRole('region', { name: de.settingsCertPublicTitle });
}
function publicSaveButton(): HTMLElement {
  return within(publicRegion()).getByRole('button', { name: de.save });
}

afterEach(() => {
  cleanup();
  try {
    window.localStorage.clear();
  } catch {
    /* jsdom/private-mode guard */
  }
});

describe('CertificateSettings', () => {
  it('zeigt den realen Mesh-TLS-Status und warnt namentlich vor ausstehender CA-Propagation', async () => {
    const runtimeFingerprint = 'a'.repeat(64);
    renderCertificateSettings(
      makeApi({
        certificates: vi.fn(async () => ({
          data: [
            {
              domain: 'gateway.int.example.test',
              kind: 'gateway',
              status: 'active',
              attempt_count: 0,
              fingerprint: 'd'.repeat(64),
            },
          ],
          mesh: {
            tls_active: true,
            address: '100.64.0.1:8081',
            fingerprint: runtimeFingerprint,
            not_after: '2027-08-15T00:00:00Z',
            ca_rotation_pending_servers: [
              { id: 'srv-a', name: 'Alpha GPU' },
              { id: 'srv-z', name: 'Zulu GPU' },
            ],
          },
        })),
      }),
    );

    const mesh = await screen.findByTestId('certificate-mesh-status');
    expect(mesh).toHaveTextContent(de.certificatesMeshTitle);
    expect(mesh).toHaveTextContent(de.certificatesMeshTLSActive);
    expect(mesh).toHaveTextContent('100.64.0.1:8081');
    expect(mesh).toHaveTextContent(runtimeFingerprint);
    const pending = screen.getByTestId('certificate-mesh-ca-pending');
    expect(pending).toHaveTextContent(de.certificatesMeshCARotationPendingTitle);
    expect(pending).toHaveTextContent('Alpha GPU');
    expect(pending).toHaveTextContent('Zulu GPU');
  });

  it('macht eine wegen defektem TLS unerreichbare Anwendung sichtbar, statt still auf http zurückzufallen', async () => {
    renderCertificateSettings(
      makeApi({
        certificates: vi.fn(async () => ({
          data: [],
          mesh: { tls_active: true, ca_rotation_pending_servers: [] },
          https_switch: {
            unreachable_apps: [
              {
                server_id: 'srv-a',
                server_name: 'Alpha GPU',
                app_id: 'app-1',
                app_type: 'openai_compatible',
                proxy_listen_port: 8600,
                route_state: 'bind_failed',
                action: 'find what else is holding that port',
              },
            ],
          },
        })),
      }),
    );

    const alert = await screen.findByTestId('certificate-https-switch-unreachable');
    expect(alert).toHaveTextContent(de.certificatesHTTPSSwitchUnreachableTitle);
    expect(alert).toHaveTextContent('Alpha GPU');
    expect(alert).toHaveTextContent('8600');
    // The agent's own reason, and the remedy -- the alert has to say what to do,
    // not only that something is wrong.
    expect(alert).toHaveTextContent('bind_failed');
    expect(alert).toHaveTextContent('find what else is holding that port');
  });

  it('zeigt keine Unerreichbar-Warnung, wenn nichts unerreichbar ist', async () => {
    renderCertificateSettings(
      makeApi({
        certificates: vi.fn(async () => ({
          data: [],
          mesh: { tls_active: true, ca_rotation_pending_servers: [] },
          https_switch: { unreachable_apps: [] },
        })),
      }),
    );

    await screen.findByTestId('certificate-mesh-status');
    expect(screen.queryByTestId('certificate-https-switch-unreachable')).not.toBeInTheDocument();
  });

  it('zeigt einen inaktiven Mesh-Listener ohne erfundenes Zertifikatsmaterial', async () => {
    renderCertificateSettings(
      makeApi({
        certificates: vi.fn(async () => ({
          data: [],
          mesh: { tls_active: false, ca_rotation_pending_servers: [] },
        })),
      }),
    );

    const mesh = await screen.findByTestId('certificate-mesh-status');
    expect(mesh).toHaveTextContent(de.certificatesMeshTLSInactive);
    expect(screen.queryByTestId('certificate-mesh-ca-pending')).not.toBeInTheDocument();
    expect(mesh).not.toHaveTextContent('BEGIN CERTIFICATE');
    expect(mesh).not.toHaveTextContent('PRIVATE KEY');
  });

  it('zeigt Zertifikate mit Erstell-/Ablaufzeit und Restlaufzeit in Tagen', async () => {
    const now = new Date();
    const notAfter = new Date(now.getTime() + 42 * 24 * 3600 * 1000).toISOString();
    const api = makeApi({
      getSystemSettings: vi.fn(async () =>
        settingsFixture({ cert_enabled: true, acme_email: 'ops@example.test' }),
      ),
      certificates: vi.fn(async () => ({
        data: [
          {
            domain: 'a.int.example.test',
            kind: 'server',
            server_name: 'srv-a',
            status: 'active',
            issued_at: now.toISOString(),
            not_after: notAfter,
            attempt_count: 0,
          },
        ],
      })),
    });
    renderCertificateSettings(api);
    expect(await screen.findByText('a.int.example.test')).toBeInTheDocument();
    // Restlaufzeit wird im Frontend aus not_after gerechnet (nie veraltet).
    expect(screen.getByText('42')).toBeInTheDocument();
  });

  it('markiert eine Restlaufzeit unter 7 Tagen als kritisch', async () => {
    const notAfter = new Date(Date.now() + 3 * 24 * 3600 * 1000).toISOString();
    const api = makeApi({
      getSystemSettings: vi.fn(async () => settingsFixture({ cert_enabled: true })),
      certificates: vi.fn(async () => ({
        data: [
          {
            domain: 'soon.int.example.test',
            kind: 'server',
            status: 'active',
            not_after: notAfter,
            attempt_count: 0,
          },
        ],
      })),
    });
    renderCertificateSettings(api);
    const cell = await screen.findByTestId('cert-remaining-soon.int.example.test');
    expect(cell).toHaveTextContent('3');
    // Kritisch = die Fehlerfarbe des Themes (Markierung, nicht nur Text).
    expect(cell).toHaveAttribute('data-severity', 'critical');
  });

  it('speichert die Einstellungen ohne cert_enabled zu senden', async () => {
    const updateSystemSettings = vi.fn(async (_body: Record<string, unknown>) => settingsFixture());
    const api = makeApi({
      getSystemSettings: vi.fn(async () =>
        settingsFixture({ cert_enabled: true, acme_email: 'old@example.test' }),
      ),
      certificates: vi.fn(async () => ({ data: [] })),
      updateSystemSettings,
    });
    renderCertificateSettings(api);
    const email = (await screen.findByLabelText(de.settingsAcmeEmail)) as HTMLInputElement;
    fireEvent.change(email, { target: { value: 'ops@example.test' } });
    fireEvent.click(settingsSaveButton());
    await waitFor(() => expect(updateSystemSettings).toHaveBeenCalledTimes(1));
    const payload = updateSystemSettings.mock.calls[0][0];
    // Final-review I4: the third direction of the save partition, guarded EXACTLY
    // like the other two (System view / edge panel) -- with toEqual, not
    // toMatchObject, which ignores extra fields. This view now renders
    // EdgeCertificatePanel inside itself, so accidentally folding a cert_edge_*
    // field into THIS payload is the likeliest mistake left: it would silently
    // switch the gateway's own edge TLS off on every save here.
    expect(payload).toEqual({
      cert_issuer_mode: 'acme',
      cert_self_signed_validity_days: 365,
      cert_ca_renew_before_days: 365,
      acme_email: 'ops@example.test',
      acme_directory_url: 'https://acme-v02.api.letsencrypt.org/directory',
      cert_base_domain: '',
      cert_gateway_domain: '',
      cert_server_scope: 'selected',
      cert_renew_before_days: 30,
    });
    // U-T4: cert_manage_public_domain/cert_public_domains moved OUT of this
    // save partition entirely -- they belong to the new "Öffentliche Domains"
    // panel's own disjoint save now (see the "public domains" describe block).
    expect(payload).not.toHaveProperty('cert_manage_public_domain');
    expect(payload).not.toHaveProperty('cert_public_domains');
    // Disjunkte Partition, explizit für die beiden Fremd-Felder benannt: die
    // Modul-Checkbox gehört der System-Ansicht, der Edge-Schalter dem Edge-Panel.
    expect(payload).not.toHaveProperty('cert_enabled');
    expect(payload).not.toHaveProperty('cert_edge_enabled');
  });

  // Final-review I3: nothing asserted that the edge panel is REACHABLE -- deleting
  // <EdgeCertificatePanel/> from this view left all 1242 tests green, and the
  // panel is the only way to configure the gateway's own edge certificate.
  it('rendert das Edge-Zertifikats-Panel innerhalb der Zertifikats-Ansicht', async () => {
    const edgeCertificate = vi.fn(async () => ({
      enabled: false,
      issuer_mode: 'acme' as const,
      names: [] as string[],
      delivery_mode: 'download' as const,
      key_download_available: false,
      require_https: false,
      https_observed: false,
    }));
    const api = makeApi({
      getSystemSettings: vi.fn(async () => settingsFixture({ cert_enabled: true })),
      edgeCertificate,
    });
    renderCertificateSettings(api);
    // Its own ARIA region (own heading) INSIDE this view, and its own issuer
    // dropdown -- distinct from the internal one in panel 1.
    expect(
      await screen.findByRole('region', { name: de.certificatesKindEdge }),
    ).toBeInTheDocument();
    expect(await screen.findByLabelText(de.settingsCertEdgeIssuerMode)).toBeInTheDocument();
    // Mounted for real: it loaded its own data.
    await waitFor(() => expect(edgeCertificate).toHaveBeenCalled());
  });

  it('blockiert das Speichern bei cert_renew_before_days unterhalb des Backend-Minimums (7)', async () => {
    const updateSystemSettings = vi.fn(async (_body: Record<string, unknown>) => settingsFixture());
    const api = makeApi({
      getSystemSettings: vi.fn(async () => settingsFixture({ cert_enabled: true })),
      certificates: vi.fn(async () => ({ data: [] })),
      updateSystemSettings,
    });
    renderCertificateSettings(api);
    const renewBeforeDays = (await screen.findByLabelText(
      de.settingsCertRenewBeforeDays,
    )) as HTMLInputElement;
    // Der Backend-Floor ist MinCertRenewBeforeDays=7 (service_system_settings.go);
    // ein Wert darunter (hier 1, das alte Client-seitige Minimum) darf NICHT
    // speicherbar sein, sonst 500 statt eines clientseitigen Guards.
    fireEvent.change(renewBeforeDays, { target: { value: '1' } });
    const saveButton = settingsSaveButton() as HTMLButtonElement;
    expect(saveButton).toBeDisabled();
    fireEvent.click(saveButton);
    expect(updateSystemSettings).not.toHaveBeenCalled();

    // Am Floor selbst (7) ist es wieder speicherbar.
    fireEvent.change(renewBeforeDays, { target: { value: '7' } });
    expect(saveButton).not.toBeDisabled();
  });

  it('zeigt modusabhängige Felder und das CA-Panel nur bei self_signed', async () => {
    const api = makeApi({
      getSystemSettings: vi.fn(async () =>
        settingsFixture({
          cert_enabled: true,
          cert_issuer_mode: 'self_signed',
          cert_self_signed_validity_days: 365,
          cert_ca_renew_before_days: 365,
        }),
      ),
      certificates: vi.fn(async () => ({ data: [] })),
      certificateCA: vi.fn(async () => ({
        ca: {
          present: true,
          subject: 'OP AI Gateway Internal CA (int.example.test)',
          fingerprint: 'aa',
          not_before: new Date().toISOString(),
          not_after: new Date(Date.now() + 3650 * 24 * 3600 * 1000).toISOString(),
        },
        bundle_pem: '-----BEGIN CERTIFICATE-----\nAAA\n-----END CERTIFICATE-----\n',
      })),
    });
    renderCertificateSettings(api);
    // self_signed: das CA-Panel und das Laufzeit-Feld sind da, die ACME-Felder nicht.
    expect(await screen.findByText(de.certificatesCaTitle)).toBeInTheDocument();
    expect(screen.getByLabelText(de.settingsCertSelfSignedValidity)).toBeInTheDocument();
    expect(screen.queryByLabelText(de.settingsAcmeEmail)).toBeNull();
    expect(screen.getByText('OP AI Gateway Internal CA (int.example.test)')).toBeInTheDocument();
  });

  it('erneuert die CA nach Bestätigung', async () => {
    const rotateCertificateCA = vi.fn(async () => ({ ok: true }));
    const api = makeApi({
      getSystemSettings: vi.fn(async () =>
        settingsFixture({ cert_enabled: true, cert_issuer_mode: 'self_signed' }),
      ),
      certificates: vi.fn(async () => ({ data: [] })),
      certificateCA: vi.fn(async () => ({
        ca: {
          present: true,
          subject: 'ca',
          fingerprint: 'aa',
          not_after: new Date().toISOString(),
        },
        bundle_pem: '-----BEGIN CERTIFICATE-----\nAAA\n-----END CERTIFICATE-----\n',
      })),
      rotateCertificateCA,
    });
    renderCertificateSettings(api);
    fireEvent.click(await screen.findByRole('button', { name: de.certificatesCaRotate }));
    // Der Confirm-Dialog muss gaten -- die Rotation ändert den Vertrauensanker.
    expect(await screen.findByText(de.certificatesCaRotateConfirmTitle)).toBeInTheDocument();
    expect(rotateCertificateCA).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole('button', { name: de.confirm }));
    await waitFor(() => expect(rotateCertificateCA).toHaveBeenCalledTimes(1));
  });

  it('stellt nach Bestätigung alle Zertifikate neu aus', async () => {
    const reissueAllCertificates = vi.fn(async () => ({ ok: true }));
    const api = makeApi({
      getSystemSettings: vi.fn(async () =>
        settingsFixture({ cert_enabled: true, cert_issuer_mode: 'acme' }),
      ),
      certificates: vi.fn(async () => ({ data: [] })),
      certificateCA: vi.fn(async () => ({ ca: { present: false }, bundle_pem: '' })),
      reissueAllCertificates,
    });
    renderCertificateSettings(api);
    fireEvent.click(await screen.findByRole('button', { name: de.certificatesReissueAll }));
    expect(await screen.findByText(de.certificatesReissueAllConfirmTitle)).toBeInTheDocument();
    expect(reissueAllCertificates).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole('button', { name: de.confirm }));
    await waitFor(() => expect(reissueAllCertificates).toHaveBeenCalledTimes(1));
  });

  it("löst 'Jetzt erneuern' für die Zeile aus", async () => {
    const renewCertificate = vi.fn(async () => ({ ok: true }));
    const api = makeApi({
      getSystemSettings: vi.fn(async () => settingsFixture({ cert_enabled: true })),
      certificates: vi.fn(async () => ({
        data: [
          {
            domain: 'a.int.example.test',
            kind: 'server',
            status: 'error',
            last_error: 'boom',
            attempt_count: 2,
          },
        ],
      })),
      renewCertificate,
    });
    renderCertificateSettings(api);
    await screen.findByText('a.int.example.test');
    fireEvent.click(screen.getByRole('button', { name: de.certificatesRenewNow }));
    await waitFor(() => expect(renewCertificate).toHaveBeenCalledWith('a.int.example.test'));
  });
});

// F3.1: the CA's module-level abort note (backend cert_last_error) must surface
// regardless of issuer mode and regardless of whether a CA exists yet -- it was
// previously invisible, reading identically to a fresh install that simply
// hasn't reconciled yet.
describe('CertificateSettings CA last-error alert (F3.1)', () => {
  it('zeigt den Reconcile-Fehler im ACME-Modus an, auch ohne vorhandene CA', async () => {
    const api = makeApi({
      getSystemSettings: vi.fn(async () =>
        settingsFixture({ cert_enabled: true, cert_issuer_mode: 'acme' }),
      ),
      certificates: vi.fn(async () => ({ data: [] })),
      certificateCA: vi.fn(async () => ({
        ca: { present: false, last_error: 'no base domain is configured' },
        bundle_pem: '',
      })),
    });
    renderCertificateSettings(api);
    const alert = await screen.findByTestId('cert-last-error');
    expect(alert).toHaveTextContent(de.certificatesLastErrorTitle);
    expect(alert).toHaveTextContent('no base domain is configured');
    // Kein CA-Panel im ACME-Modus ohne CA -- der Alert steht davon unabhängig.
    expect(screen.queryByText(de.certificatesCaTitle)).not.toBeInTheDocument();
  });

  it('zeigt den Reconcile-Fehler auch im self-signed-Modus mit vorhandener CA an', async () => {
    const api = makeApi({
      getSystemSettings: vi.fn(async () =>
        settingsFixture({ cert_enabled: true, cert_issuer_mode: 'self_signed' }),
      ),
      certificates: vi.fn(async () => ({ data: [] })),
      certificateCA: vi.fn(async () => ({
        ca: {
          present: true,
          subject: 'ca',
          fingerprint: 'aa',
          not_after: new Date().toISOString(),
          last_error: "the internal CA's private key cannot be sealed",
        },
        bundle_pem: '-----BEGIN CERTIFICATE-----\nAAA\n-----END CERTIFICATE-----\n',
      })),
    });
    renderCertificateSettings(api);
    const alert = await screen.findByTestId('cert-last-error');
    expect(alert).toHaveTextContent("the internal CA's private key cannot be sealed");
    // Die CA existiert hier -- das Panel zeigt sich normal daneben.
    expect(await screen.findByText(de.certificatesCaTitle)).toBeInTheDocument();
  });

  it('zeigt KEINEN Alert, wenn last_error leer ist', async () => {
    const api = makeApi({
      getSystemSettings: vi.fn(async () => settingsFixture({ cert_enabled: true })),
      certificates: vi.fn(async () => ({ data: [] })),
      certificateCA: vi.fn(async () => ({
        ca: { present: false, last_error: '' },
        bundle_pem: '',
      })),
    });
    renderCertificateSettings(api);
    await screen.findByLabelText(de.settingsAcmeEmail);
    expect(screen.queryByTestId('cert-last-error')).not.toBeInTheDocument();
  });
});

// F3.2: switching cert_server_scope from "all" to "selected" prunes every
// certificate for a server without the "Manage a certificate for this server"
// checkbox checked on the next reconcile pass, deleting its sealed key too.
// Only that direction must gate.
describe('CertificateSettings scope-flip confirmation (F3.2)', () => {
  function apiWithScopeAll(overrides: Partial<CertificateSettingsApi> = {}) {
    return makeApi({
      getSystemSettings: vi.fn(async () =>
        settingsFixture({ cert_enabled: true, cert_issuer_mode: 'acme', cert_server_scope: 'all' }),
      ),
      certificates: vi.fn(async () => ({
        data: [
          {
            domain: 'a.int.example.test',
            kind: 'server',
            server_id: 'srv_a',
            status: 'active',
            attempt_count: 0,
          },
        ],
      })),
      certificateCA: vi.fn(async () => ({ ca: { present: false }, bundle_pem: '' })),
      servers: vi.fn(async () => ({
        data: [serverFixture({ id: 'srv_a', certificate_override: '' })],
      })),
      ...overrides,
    });
  }

  it("fragt vor dem Speichern nach, wenn Server ohne 'include'-Override betroffen wären", async () => {
    const updateSystemSettings = vi.fn(async (_body: Record<string, unknown>) => settingsFixture());
    const api = apiWithScopeAll({ updateSystemSettings });
    renderCertificateSettings(api);
    await screen.findByText('a.int.example.test');

    await pickServerScope(de.settingsCertScopeSelected);
    expect(await screen.findByText(de.certificatesScopeFlipConfirmTitle)).toBeInTheDocument();
    // Speichern kann noch nicht mit dem neuen Umfang passiert sein -- der Dialog
    // sperrt (MUI-Modal, hintergrund aria-hidden), bevor überhaupt bestätigt wurde.
    expect(updateSystemSettings).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole('button', { name: de.confirm }));
    // Erst NACH der Bestätigung übernimmt das Dropdown "selected".
    await waitFor(() =>
      expect(screen.getByRole('combobox', { name: de.settingsCertServerScope })).toHaveTextContent(
        de.settingsCertScopeSelected,
      ),
    );

    fireEvent.click(settingsSaveButton());
    await waitFor(() => expect(updateSystemSettings).toHaveBeenCalledTimes(1));
    expect(updateSystemSettings.mock.calls[0][0]).toMatchObject({ cert_server_scope: 'selected' });
  });

  it('übernimmt die Wahl NICHT bei Abbruch -- auch ein anschließend bearbeitetes Nebenfeld schleust sie nicht ein', async () => {
    const updateSystemSettings = vi.fn(async (_body: Record<string, unknown>) => settingsFixture());
    const api = apiWithScopeAll({ updateSystemSettings });
    renderCertificateSettings(api);
    await screen.findByText('a.int.example.test');

    await pickServerScope(de.settingsCertScopeSelected);
    expect(await screen.findByText(de.certificatesScopeFlipConfirmTitle)).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: de.cancel }));
    await waitFor(() =>
      expect(screen.queryByText(de.certificatesScopeFlipConfirmTitle)).not.toBeInTheDocument(),
    );
    // Der Dialog ist zu -- das Dropdown blieb bei "all" (nie übernommen).
    expect(screen.getByRole('combobox', { name: de.settingsCertServerScope })).toHaveTextContent(
      de.settingsCertScopeAll,
    );

    // Ein unbeteiligtes Feld bearbeiten -- das darf die abgebrochene
    // Umfangs-Änderung nicht wiederbeleben.
    const email = screen.getByLabelText(de.settingsAcmeEmail) as HTMLInputElement;
    fireEvent.change(email, { target: { value: 'ops@example.test' } });

    fireEvent.click(settingsSaveButton());
    await waitFor(() => expect(updateSystemSettings).toHaveBeenCalledTimes(1));
    expect(updateSystemSettings.mock.calls[0][0]).toMatchObject({
      cert_server_scope: 'all',
      acme_email: 'ops@example.test',
    });
  });

  it("fragt NICHT nach, wenn alle betroffenen Server bereits 'include' gesetzt haben", async () => {
    const updateSystemSettings = vi.fn(async (_body: Record<string, unknown>) => settingsFixture());
    const api = apiWithScopeAll({
      updateSystemSettings,
      servers: vi.fn(async () => ({
        data: [serverFixture({ id: 'srv_a', certificate_override: 'include' })],
      })),
    });
    renderCertificateSettings(api);
    await screen.findByText('a.int.example.test');

    await pickServerScope(de.settingsCertScopeSelected);
    await waitFor(() =>
      expect(screen.getByRole('combobox', { name: de.settingsCertServerScope })).toHaveTextContent(
        de.settingsCertScopeSelected,
      ),
    );
    expect(screen.queryByText(de.certificatesScopeFlipConfirmTitle)).not.toBeInTheDocument();
  });

  it("fragt NICHT nach für die Gegenrichtung ('selected' -> 'all'), selbst wenn dieselben Daten in der Vorwärtsrichtung nachfragen würden", async () => {
    const updateSystemSettings = vi.fn(async (_body: Record<string, unknown>) => settingsFixture());
    const api = makeApi({
      getSystemSettings: vi.fn(async () =>
        settingsFixture({
          cert_enabled: true,
          cert_issuer_mode: 'acme',
          cert_server_scope: 'selected',
        }),
      ),
      // Dieselbe "würde betroffen sein"-Datenlage wie im ersten Test oben
      // (Server ohne 'include'-Override) -- beweist, dass NICHT die
      // betroffene-Zertifikate-Zählung die Gegenrichtung verschont, sondern
      // die Richtungs-Prüfung selbst.
      certificates: vi.fn(async () => ({
        data: [
          {
            domain: 'a.int.example.test',
            kind: 'server',
            server_id: 'srv_a',
            status: 'active',
            attempt_count: 0,
          },
        ],
      })),
      certificateCA: vi.fn(async () => ({ ca: { present: false }, bundle_pem: '' })),
      servers: vi.fn(async () => ({
        data: [serverFixture({ id: 'srv_a', certificate_override: '' })],
      })),
      updateSystemSettings,
    });
    renderCertificateSettings(api);
    await screen.findByText('a.int.example.test');

    await pickServerScope(de.settingsCertScopeAll);
    await waitFor(() =>
      expect(screen.getByRole('combobox', { name: de.settingsCertServerScope })).toHaveTextContent(
        de.settingsCertScopeAll,
      ),
    );
    expect(screen.queryByText(de.certificatesScopeFlipConfirmTitle)).not.toBeInTheDocument();
  });
});

// F3.3: attempt_count/next_attempt_at/fingerprint are on the DTO but were never
// rendered -- exactly the fields an operator needs to diagnose a stuck domain.
// Hidden by default so the default view stays as clean as before.
describe('CertificateSettings diagnostic columns (F3.3)', () => {
  it('blendet Versuche/nächster Versuch/Fingerprint standardmäßig aus und zeigt sie nach Aktivierung', async () => {
    const nextAttempt = new Date(Date.now() + 3600_000).toISOString();
    const api = makeApi({
      getSystemSettings: vi.fn(async () => settingsFixture({ cert_enabled: true })),
      certificates: vi.fn(async () => ({
        data: [
          {
            domain: 'a.int.example.test',
            kind: 'server',
            status: 'error',
            attempt_count: 4,
            next_attempt_at: nextAttempt,
            fingerprint: 'AA:BB:CC:DD:EE:FF',
            last_error: 'boom',
          },
        ],
      })),
    });
    renderCertificateSettings(api);
    await screen.findByText('a.int.example.test');

    // Standardmäßig aus -- weder die Spaltenköpfe noch die Werte sind da.
    expect(
      screen.queryByRole('columnheader', { name: de.certificatesColAttempts }),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByRole('columnheader', { name: de.certificatesColNextAttempt }),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByRole('columnheader', { name: de.certificatesColFingerprint }),
    ).not.toBeInTheDocument();
    expect(screen.queryByText('AA:BB:CC:DD:EE:FF')).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: de.listColumns }));
    fireEvent.click(screen.getByRole('checkbox', { name: de.certificatesColAttempts }));
    fireEvent.click(screen.getByRole('checkbox', { name: de.certificatesColNextAttempt }));
    const fingerprintCheckbox = screen.getByRole('checkbox', {
      name: de.certificatesColFingerprint,
    });
    fireEvent.click(fingerprintCheckbox);
    // Close the column menu (Escape dispatched on a node inside the Menu's
    // portal so it bubbles to the Modal's keydown handler) -- the rest of the
    // page is aria-hidden while the Menu is open, so a getByRole("columnheader")
    // below would otherwise find nothing (mirrors Activity.columns.test.tsx's
    // same-shaped column-toggle test).
    fireEvent.keyDown(fingerprintCheckbox, { key: 'Escape' });

    expect(
      await screen.findByRole('columnheader', { name: de.certificatesColAttempts }),
    ).toBeInTheDocument();
    expect(screen.getByText('4')).toBeInTheDocument();
    expect(screen.getByText('AA:BB:CC:DD:EE:FF')).toBeInTheDocument();
    expect(screen.getByText(new Date(nextAttempt).toLocaleString())).toBeInTheDocument();
  });

  it("rendert eine Zeile ohne next_attempt_at/fingerprint als leer, nicht als 'Invalid Date' oder 'NaN'", async () => {
    const api = makeApi({
      getSystemSettings: vi.fn(async () => settingsFixture({ cert_enabled: true })),
      certificates: vi.fn(async () => ({
        data: [
          { domain: 'b.int.example.test', kind: 'server', status: 'pending', attempt_count: 0 },
        ],
      })),
    });
    renderCertificateSettings(api);
    await screen.findByText('b.int.example.test');

    fireEvent.click(screen.getByRole('button', { name: de.listColumns }));
    fireEvent.click(screen.getByRole('checkbox', { name: de.certificatesColAttempts }));
    fireEvent.click(screen.getByRole('checkbox', { name: de.certificatesColNextAttempt }));
    fireEvent.click(screen.getByRole('checkbox', { name: de.certificatesColFingerprint }));

    expect(screen.queryByText('Invalid Date')).not.toBeInTheDocument();
    expect(screen.queryByText('NaN')).not.toBeInTheDocument();
    // attempt_count=0 still renders as its own cell (not swallowed as empty).
    expect(screen.getByText('0')).toBeInTheDocument();
  });
});

// Phase 2 distribution: the "Installiert" column shows what the row's ServerAgent
// last reported it has on disk -- ✓ / ✗ / — plus a tooltip carrying the report's
// age and mode.
describe('CertificateSettings installed column', () => {
  const reportedAt = '2026-08-14T10:11:12Z';

  function installedApi(rows: CertificateRow[]) {
    return makeApi({
      getSystemSettings: vi.fn(async () => settingsFixture({ cert_enabled: true })),
      certificates: vi.fn(async () => ({ data: rows })),
    });
  }

  it('zeigt ✓ mit Report-Zeit und Modus, wenn der Agent genau dieses Zertifikat meldet', async () => {
    renderCertificateSettings(
      installedApi([
        {
          domain: 'a.int.example.test',
          kind: 'server',
          status: 'active',
          attempt_count: 0,
          fingerprint: 'aa11',
          installed: true,
          installed_fingerprint: 'aa11',
          installed_at: reportedAt,
          installed_mode: 'files',
        },
      ]),
    );
    const cell = await screen.findByTestId('cert-installed-a.int.example.test');
    expect(cell).toHaveAttribute('data-state', 'yes');
    expect(cell).toHaveTextContent('✓');
    const title = cell.getAttribute('title') ?? '';
    expect(title).toContain(new Date(reportedAt).toLocaleString());
    expect(title).toContain('files');
    expect(title).not.toContain(de.certificatesInstalledStale);
  });

  it('zeigt ✗ mit Hinweis, wenn der Agent ein ANDERES Zertifikat meldet', async () => {
    renderCertificateSettings(
      installedApi([
        {
          domain: 'b.int.example.test',
          kind: 'server',
          status: 'active',
          attempt_count: 0,
          fingerprint: 'bb22',
          installed: false,
          installed_fingerprint: 'cc33',
          installed_at: reportedAt,
          installed_mode: 'proxy',
        },
      ]),
    );
    const cell = await screen.findByTestId('cert-installed-b.int.example.test');
    expect(cell).toHaveAttribute('data-state', 'no');
    expect(cell).toHaveTextContent('✗');
    const title = cell.getAttribute('title') ?? '';
    expect(title).toContain(de.certificatesInstalledStale);
    expect(title).toContain(new Date(reportedAt).toLocaleString());
  });

  it('zeigt — für eine nie gemeldete Zeile und für eine Zeile ohne Agent (kind != server)', async () => {
    renderCertificateSettings(
      installedApi([
        {
          domain: 'quiet.int.example.test',
          kind: 'server',
          status: 'active',
          attempt_count: 0,
          fingerprint: 'dd44',
        },
        // An edge row can never have an agent -- even if a report somehow carried
        // the same values, this must stay "—".
        {
          domain: 'edge.int.example.test',
          kind: 'edge',
          status: 'active',
          attempt_count: 0,
          fingerprint: 'ee55',
          installed: true,
          installed_fingerprint: 'ee55',
          installed_at: reportedAt,
          installed_mode: 'files',
        },
      ]),
    );
    const quiet = await screen.findByTestId('cert-installed-quiet.int.example.test');
    expect(quiet).toHaveAttribute('data-state', 'unknown');
    expect(quiet).toHaveTextContent('—');
    expect(quiet).toHaveAttribute('title', de.certificatesInstalledNever);

    const edge = screen.getByTestId('cert-installed-edge.int.example.test');
    expect(edge).toHaveAttribute('data-state', 'unknown');
    expect(edge).toHaveTextContent('—');
  });

  it('rendert die Spalte zwischen Restlaufzeit und Letzter Fehler', async () => {
    renderCertificateSettings(
      installedApi([
        { domain: 'a.int.example.test', kind: 'server', status: 'active', attempt_count: 0 },
      ]),
    );
    await screen.findByText('a.int.example.test');
    const headers = screen.getAllByRole('columnheader').map((h) => h.textContent ?? '');
    const remaining = headers.findIndex((h) => h.includes(de.certificatesColRemaining));
    const installed = headers.findIndex((h) => h.includes(de.certificatesColInstalled));
    const transport = headers.findIndex((h) => h.includes(de.certificatesColTransport));
    const lastError = headers.findIndex((h) => h.includes(de.certificatesColError));
    expect(remaining).toBeGreaterThanOrEqual(0);
    expect(installed).toBe(remaining + 1);
    expect(transport).toBe(installed + 1);
    expect(lastError).toBe(transport + 1);
  });
});

describe('CertificateSettings transport column', () => {
  const observedAt = '2026-08-15T09:10:11Z';

  function transportApi(rows: CertificateRow[]) {
    return makeApi({
      getSystemSettings: vi.fn(async () => settingsFixture({ cert_enabled: true })),
      certificates: vi.fn(async () => ({ data: rows })),
    });
  }

  it('zeigt ✓ TLS mit Beobachtungszeit, wenn der Agent zuletzt über TLS verband', async () => {
    renderCertificateSettings(
      transportApi([
        {
          domain: 'a.int.example.test',
          kind: 'server',
          status: 'active',
          attempt_count: 0,
          fingerprint: 'aa11',
          transport: 'tls',
          transport_at: observedAt,
        },
      ]),
    );
    const cell = await screen.findByTestId('cert-transport-a.int.example.test');
    expect(cell).toHaveAttribute('data-state', 'tls');
    expect(cell).toHaveTextContent(`✓ ${de.certificatesTransportTLS}`);
    expect(cell.getAttribute('title') ?? '').toContain(new Date(observedAt).toLocaleString());
  });

  it('zeigt ✗ Klartext, wenn der Agent zuletzt im Klartext verband', async () => {
    renderCertificateSettings(
      transportApi([
        {
          domain: 'b.int.example.test',
          kind: 'server',
          status: 'active',
          attempt_count: 0,
          fingerprint: 'bb22',
          transport: 'plain',
          transport_at: observedAt,
        },
      ]),
    );
    const cell = await screen.findByTestId('cert-transport-b.int.example.test');
    expect(cell).toHaveAttribute('data-state', 'plain');
    expect(cell).toHaveTextContent(`✗ ${de.certificatesTransportPlain}`);
  });

  it('zeigt — für eine nie beobachtete Zeile und für eine Zeile ohne Agent (kind != server)', async () => {
    renderCertificateSettings(
      transportApi([
        {
          domain: 'quiet.int.example.test',
          kind: 'server',
          status: 'active',
          attempt_count: 0,
          fingerprint: 'dd44',
        },
        {
          domain: 'edge.int.example.test',
          kind: 'edge',
          status: 'active',
          attempt_count: 0,
          fingerprint: 'ee55',
          transport: 'tls',
          transport_at: observedAt,
        },
      ]),
    );
    const quiet = await screen.findByTestId('cert-transport-quiet.int.example.test');
    expect(quiet).toHaveAttribute('data-state', 'unknown');
    expect(quiet).toHaveTextContent('—');
    expect(quiet).toHaveAttribute('title', de.certificatesTransportNever);

    const edge = screen.getByTestId('cert-transport-edge.int.example.test');
    expect(edge).toHaveAttribute('data-state', 'unknown');
    expect(edge).toHaveTextContent('—');
  });
});

describe('CertificateSettings mesh require-tls gate (P3)', () => {
  function meshApi(mesh: CertificateMeshStatus, overrides: Partial<CertificateSettingsApi> = {}) {
    return makeApi({
      getSystemSettings: vi.fn(async () => settingsFixture({ cert_enabled: true })),
      certificates: vi.fn(async () => ({ data: [], mesh })),
      certificateCA: vi.fn(async () => ({ ca: { present: false }, bundle_pem: '' })),
      ...overrides,
    });
  }

  it('disables the toggle until a TLS hop has been observed', async () => {
    renderCertificateSettings(
      meshApi({
        tls_active: false,
        ca_rotation_pending_servers: [],
        require_tls: false,
        tls_observed: false,
      }),
    );
    const toggle = (await screen.findByTestId('certificate-mesh-require-tls')).querySelector(
      'input',
    )!;
    expect(toggle).toBeDisabled();
  });

  it('names the servers that would be locked out and arms only after confirmation', async () => {
    const updateSystemSettings = vi.fn(async (_body: Record<string, unknown>) => settingsFixture());
    renderCertificateSettings(
      meshApi(
        {
          tls_active: true,
          ca_rotation_pending_servers: [],
          require_tls: false,
          tls_observed: true,
          tls_pending_servers: [{ id: 'srv-lag', name: 'Laggard GPU' }],
        },
        { updateSystemSettings },
      ),
    );
    const toggle = (await screen.findByTestId('certificate-mesh-require-tls')).querySelector(
      'input',
    )!;
    expect(toggle).not.toBeDisabled();

    fireEvent.click(toggle);
    // The confirm dialog names the laggard, and nothing is armed before confirming.
    const pending = await screen.findByTestId('certificate-mesh-require-tls-pending');
    expect(pending).toHaveTextContent('Laggard GPU');
    expect(updateSystemSettings).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole('button', { name: de.confirm }));
    await waitFor(() => expect(updateSystemSettings).toHaveBeenCalledTimes(1));
    expect(updateSystemSettings.mock.calls[0][0]).toEqual({ cert_mesh_require_tls: true });
  });

  it('disarms without a dialog, sending the switch alone', async () => {
    const updateSystemSettings = vi.fn(async (_body: Record<string, unknown>) => settingsFixture());
    renderCertificateSettings(
      meshApi(
        {
          tls_active: true,
          ca_rotation_pending_servers: [],
          require_tls: true,
          tls_observed: true,
        },
        { updateSystemSettings },
      ),
    );
    const toggle = (await screen.findByTestId('certificate-mesh-require-tls')).querySelector(
      'input',
    )!;
    expect(toggle).toBeChecked();
    fireEvent.click(toggle);
    await waitFor(() => expect(updateSystemSettings).toHaveBeenCalledTimes(1));
    expect(updateSystemSettings.mock.calls[0][0]).toEqual({ cert_mesh_require_tls: false });
    expect(screen.queryByText(de.certificatesMeshRequireTLSConfirmTitle)).not.toBeInTheDocument();
  });
});

// Task 8 (agent-mesh-tls-port): the runtime mode toggle for the encrypted
// agent port + the read-only display of the effective TLS port and whether a
// separate TLS listener is actually active. cert_mesh_tls_mode is writable
// (sent ALONE, mirroring cert_mesh_require_tls above); cert_mesh_tls_port and
// cert_mesh_tls_separate_active are server-computed and never appear in a PUT.
describe('CertificateSettings mesh TLS-port mode (Task 8)', () => {
  it('renders the mode select with all three options and the read-only port/state', async () => {
    renderCertificateSettings(
      makeApi({
        getSystemSettings: vi.fn(async () =>
          settingsFixture({
            cert_enabled: true,
            cert_mesh_tls_mode: '',
            cert_mesh_tls_port: 8443,
            cert_mesh_tls_separate_active: false,
          }),
        ),
      }),
    );

    const select = await screen.findByLabelText(de.certificatesMeshTLSPortMode);
    expect(select).toBeInTheDocument();
    fireEvent.mouseDown(select);
    expect(
      await screen.findByRole('option', { name: de.certificatesMeshTLSPortModeFollowEnv }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole('option', { name: de.certificatesMeshTLSPortModeCombined }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole('option', { name: de.certificatesMeshTLSPortModeSeparate }),
    ).toBeInTheDocument();
    // Close the popup without picking anything so it doesn't linger over the
    // read-only assertions below.
    fireEvent.click(screen.getByRole('option', { name: de.certificatesMeshTLSPortModeFollowEnv }));

    const status = screen.getByTestId('certificate-mesh-tls-port-status');
    expect(status).toHaveTextContent(`${de.certificatesMeshTLSPort}: 8443`);
    expect(status).toHaveTextContent(de.certificatesMeshTLSPortSeparateInactive);
  });

  it('shows the active read-only state when a separate TLS listener is up', async () => {
    renderCertificateSettings(
      makeApi({
        getSystemSettings: vi.fn(async () =>
          settingsFixture({
            cert_enabled: true,
            cert_mesh_tls_mode: 'separate',
            cert_mesh_tls_port: 8444,
            cert_mesh_tls_separate_active: true,
          }),
        ),
      }),
    );

    const status = await screen.findByTestId('certificate-mesh-tls-port-status');
    expect(status).toHaveTextContent(`${de.certificatesMeshTLSPort}: 8444`);
    expect(status).toHaveTextContent(de.certificatesMeshTLSPortSeparateActive);
  });

  // Round-1 fix (Important finding): a mode change LIVE-rebinds the gateway's
  // agent-mesh listener (may drop/reconnect every connected ServerAgent) --
  // at least as disruptive as arming cert_mesh_require_tls, which this same
  // panel already gates behind a ConfirmDialog. So selecting a different
  // value must NOT PUT immediately; it must stage a confirm dialog first.
  it('selecting a different mode opens a confirm dialog and does NOT PUT yet', async () => {
    const updateSystemSettings = vi.fn(async (_body: Record<string, unknown>) => settingsFixture());
    renderCertificateSettings(
      makeApi({
        getSystemSettings: vi.fn(async () =>
          settingsFixture({
            cert_enabled: true,
            cert_mesh_tls_mode: '',
            cert_mesh_tls_port: 8443,
            cert_mesh_tls_separate_active: false,
          }),
        ),
        updateSystemSettings,
      }),
    );

    fireEvent.mouseDown(await screen.findByLabelText(de.certificatesMeshTLSPortMode));
    fireEvent.click(
      await screen.findByRole('option', { name: de.certificatesMeshTLSPortModeCombined }),
    );

    const dialogTitle = await screen.findByText(de.certificatesMeshTLSPortModeConfirmTitle);
    expect(dialogTitle).toBeInTheDocument();
    expect(screen.getByText(de.certificatesMeshTLSPortModeConfirmBody)).toBeInTheDocument();
    expect(updateSystemSettings).not.toHaveBeenCalled();
  });

  it('PUTs cert_mesh_tls_mode alone -- exactly once -- after confirming, and refreshes the read-only fields', async () => {
    const updateSystemSettings = vi.fn(async (_body: Record<string, unknown>) =>
      settingsFixture({
        cert_enabled: true,
        cert_mesh_tls_mode: 'combined',
        cert_mesh_tls_port: 8081,
        cert_mesh_tls_separate_active: false,
      }),
    );
    renderCertificateSettings(
      makeApi({
        getSystemSettings: vi.fn(async () =>
          settingsFixture({
            cert_enabled: true,
            cert_mesh_tls_mode: '',
            cert_mesh_tls_port: 8443,
            cert_mesh_tls_separate_active: false,
          }),
        ),
        updateSystemSettings,
      }),
    );

    fireEvent.mouseDown(await screen.findByLabelText(de.certificatesMeshTLSPortMode));
    fireEvent.click(
      await screen.findByRole('option', { name: de.certificatesMeshTLSPortModeCombined }),
    );
    await screen.findByText(de.certificatesMeshTLSPortModeConfirmTitle);

    fireEvent.click(screen.getByRole('button', { name: de.confirm }));

    await waitFor(() => expect(updateSystemSettings).toHaveBeenCalledTimes(1));
    expect(updateSystemSettings.mock.calls[0][0]).toEqual({ cert_mesh_tls_mode: 'combined' });
    // MUI's Dialog keeps the title in the DOM through its close transition, so
    // this needs a waitFor (mirrors the scope-flip confirm-close assertion).
    await waitFor(() =>
      expect(
        screen.queryByText(de.certificatesMeshTLSPortModeConfirmTitle),
      ).not.toBeInTheDocument(),
    );

    // The response's read-only fields (a different port here) land on screen --
    // proof the panel re-renders from the PUT's response, not a stale cache.
    const status = await screen.findByTestId('certificate-mesh-tls-port-status');
    expect(status).toHaveTextContent(`${de.certificatesMeshTLSPort}: 8081`);
  });

  it('cancelling fires no PUT and leaves the select at the prior value', async () => {
    const updateSystemSettings = vi.fn(async (_body: Record<string, unknown>) => settingsFixture());
    renderCertificateSettings(
      makeApi({
        getSystemSettings: vi.fn(async () =>
          settingsFixture({
            cert_enabled: true,
            cert_mesh_tls_mode: '',
            cert_mesh_tls_port: 8443,
            cert_mesh_tls_separate_active: false,
          }),
        ),
        updateSystemSettings,
      }),
    );

    fireEvent.mouseDown(await screen.findByLabelText(de.certificatesMeshTLSPortMode));
    fireEvent.click(
      await screen.findByRole('option', { name: de.certificatesMeshTLSPortModeCombined }),
    );
    await screen.findByText(de.certificatesMeshTLSPortModeConfirmTitle);

    fireEvent.click(screen.getByRole('button', { name: de.cancel }));

    await waitFor(() =>
      expect(
        screen.queryByText(de.certificatesMeshTLSPortModeConfirmTitle),
      ).not.toBeInTheDocument(),
    );
    expect(updateSystemSettings).not.toHaveBeenCalled();
    // The select was never updated ahead of confirmation, so it still shows
    // the original ("" -> "follows env default") value.
    expect(
      screen.getByRole('combobox', { name: de.certificatesMeshTLSPortMode }),
    ).toHaveTextContent(de.certificatesMeshTLSPortModeFollowEnv);
    const status = screen.getByTestId('certificate-mesh-tls-port-status');
    expect(status).toHaveTextContent(`${de.certificatesMeshTLSPort}: 8443`);
  });
});

// U-T4: the unified "publicly-trusted certificates" area's NEW public-domains
// block -- the manage toggle/domain list (moved out of panel 1), the
// cert_public_issuer_mode dropdown, and AcmeConfigFields' shared-vs-own ACME
// account for this context, plus the Task-9 per-domain export buttons.
describe('CertificateSettings public domains panel (U-T4)', () => {
  it("zeigt bei 'eigene ACME-Einstellungen' E-Mail, Directory und Wochenlimit", async () => {
    const api = makeApi({
      getSystemSettings: vi.fn(async () => settingsFixture({ cert_enabled: true })),
    });
    renderCertificateSettings(api);
    await screen.findByRole('region', { name: de.settingsCertPublicTitle });
    const region = publicRegion();

    // Vor dem Umschalten: nur der Schalter selbst, keine Unterfelder.
    expect(within(region).queryByLabelText(de.settingsAcmeEmail)).toBeNull();
    expect(within(region).queryByLabelText(de.settingsAcmeDirectory)).toBeNull();
    expect(within(region).queryByLabelText(de.settingsAcmeWeeklyLimit)).toBeNull();

    fireEvent.click(within(region).getByRole('switch', { name: de.settingsAcmeOwnSettings }));

    expect(within(region).getByLabelText(de.settingsAcmeEmail)).toBeInTheDocument();
    expect(within(region).getByLabelText(de.settingsAcmeDirectory)).toBeInTheDocument();
    expect(within(region).getByLabelText(de.settingsAcmeWeeklyLimit)).toBeInTheDocument();
  });

  it("zeigt bei Let's-Encrypt-Produktion das feste Wochenlimit 50 (nur lesbar); bei 'Eigene URL' wird es editierbar", async () => {
    const api = makeApi({
      getSystemSettings: vi.fn(async () => settingsFixture({ cert_enabled: true })),
    });
    renderCertificateSettings(api);
    await screen.findByRole('region', { name: de.settingsCertPublicTitle });
    const region = publicRegion();
    fireEvent.click(within(region).getByRole('switch', { name: de.settingsAcmeOwnSettings }));

    // Voreinstellung ist Produktion -- fest auf 50, nicht editierbar.
    const weeklyLimit = within(region).getByLabelText(
      de.settingsAcmeWeeklyLimit,
    ) as HTMLInputElement;
    expect(weeklyLimit.value).toBe('50');
    expect(weeklyLimit).toBeDisabled();

    // Auf "Eigene URL" umstellen -- editierbar.
    fireEvent.mouseDown(within(region).getByLabelText(de.settingsAcmeDirectory));
    fireEvent.click(await screen.findByRole('option', { name: de.settingsAcmeDirectoryCustom }));

    const weeklyLimitAfter = within(region).getByLabelText(
      de.settingsAcmeWeeklyLimit,
    ) as HTMLInputElement;
    expect(weeklyLimitAfter).not.toBeDisabled();
    fireEvent.change(weeklyLimitAfter, { target: { value: '12' } });
    expect(weeklyLimitAfter.value).toBe('12');
  });

  it("speichert den Block mit cert_public_acme_shared/_email/_directory_url/_weekly_limit + cert_public_issuer_mode (unverändert '' -- folgt weiter der globalen Einstellung)", async () => {
    const updateSystemSettings = vi.fn(async (_body: Record<string, unknown>) => settingsFixture());
    const api = makeApi({
      getSystemSettings: vi.fn(async () => settingsFixture({ cert_enabled: true })),
      updateSystemSettings,
    });
    renderCertificateSettings(api);
    await screen.findByRole('region', { name: de.settingsCertPublicTitle });
    const region = publicRegion();

    fireEvent.click(within(region).getByRole('switch', { name: de.settingsAcmeOwnSettings }));
    const email = within(region).getByLabelText(de.settingsAcmeEmail) as HTMLInputElement;
    fireEvent.change(email, { target: { value: 'public-ops@example.test' } });

    fireEvent.click(publicSaveButton());
    await waitFor(() => expect(updateSystemSettings).toHaveBeenCalledTimes(1));
    const payload = updateSystemSettings.mock.calls[0][0];
    // Round-1 review fix: cert_public_issuer_mode was NEVER touched here (the
    // fixture leaves it unset, i.e. "") -- it must stay "" (follow the
    // global/internal mode), not get silently pinned to "acme" just because
    // this save touched OTHER fields in the same block.
    expect(payload).toEqual({
      cert_manage_public_domain: false,
      cert_public_domains: [],
      cert_public_issuer_mode: '',
      cert_public_acme_shared: false,
      cert_public_acme_email: 'public-ops@example.test',
      cert_public_acme_directory_url: 'https://acme-v02.api.letsencrypt.org/directory',
      cert_public_acme_weekly_limit: 50,
    });
    // Disjunkte Partition: dieser Save darf keine Panel-1- oder Edge-Felder senden.
    expect(payload).not.toHaveProperty('cert_issuer_mode');
    expect(payload).not.toHaveProperty('cert_edge_enabled');
  });

  // Round-1 review, finding 1 (weekly-limit divergence): the backend's REAL
  // "unset" default is 0 (nonNegativeIntSetting), not undefined/absent -- a
  // fixture that merely omits the field never actually reproduces the bug
  // (a `?? 50` fallback DOES catch `undefined`; it does NOT catch a literal
  // `0`). This fixture sets it explicitly to 0, exactly like a real GET would.
  it('zeigt und speichert bei Produktion weiterhin 50, auch wenn der reale Stand 0 ist (Wochenlimit-Divergenz)', async () => {
    const updateSystemSettings = vi.fn(async (_body: Record<string, unknown>) => settingsFixture());
    const api = makeApi({
      getSystemSettings: vi.fn(async () =>
        settingsFixture({
          cert_enabled: true,
          cert_public_acme_directory_url: 'https://acme-v02.api.letsencrypt.org/directory',
          cert_public_acme_weekly_limit: 0,
        }),
      ),
      updateSystemSettings,
    });
    renderCertificateSettings(api);
    await screen.findByRole('region', { name: de.settingsCertPublicTitle });
    const region = publicRegion();
    fireEvent.click(within(region).getByRole('switch', { name: de.settingsAcmeOwnSettings }));

    const weeklyLimit = within(region).getByLabelText(
      de.settingsAcmeWeeklyLimit,
    ) as HTMLInputElement;
    expect(weeklyLimit.value).toBe('50');

    fireEvent.click(publicSaveButton());
    await waitFor(() => expect(updateSystemSettings).toHaveBeenCalledTimes(1));
    // The saved number must equal what was just displayed -- 50, never the
    // real stored 0 the fixture seeded.
    expect(updateSystemSettings.mock.calls[0][0]).toMatchObject({
      cert_public_acme_weekly_limit: 50,
    });
  });

  // Round-1 review, finding 2 (byte-neutrality): explicit selections must
  // still work -- the fix must not make "acme"/"self_signed" unreachable.
  it("sendet einen explizit gewählten Aussteller-Modus statt '' zu erhalten", async () => {
    const updateSystemSettings = vi.fn(async (_body: Record<string, unknown>) => settingsFixture());
    const api = makeApi({
      getSystemSettings: vi.fn(async () => settingsFixture({ cert_enabled: true })),
      updateSystemSettings,
    });
    renderCertificateSettings(api);
    await screen.findByRole('region', { name: de.settingsCertPublicTitle });
    const region = publicRegion();

    fireEvent.mouseDown(within(region).getByLabelText(de.settingsCertPublicIssuerMode));
    fireEvent.click(await screen.findByRole('option', { name: de.settingsCertIssuerSelfSigned }));

    fireEvent.click(publicSaveButton());
    await waitFor(() => expect(updateSystemSettings).toHaveBeenCalledTimes(1));
    expect(updateSystemSettings.mock.calls[0][0]).toMatchObject({
      cert_public_issuer_mode: 'self_signed',
    });
  });

  it('bietet je konfigurierter öffentlicher Domain eigene Bundle-/Schlüssel-Downloads an', async () => {
    const publicCertificateBundle = vi.fn(
      async () => '-----BEGIN CERTIFICATE-----\nAAA\n-----END CERTIFICATE-----\n',
    );
    const publicCertificateKey = vi.fn(
      async () => '-----BEGIN PRIVATE KEY-----\nBBB\n-----END PRIVATE KEY-----\n',
    );
    const api = makeApi({
      getSystemSettings: vi.fn(async () =>
        settingsFixture({
          cert_enabled: true,
          cert_manage_public_domain: true,
          cert_public_domains: ['pub.example.test'],
        }),
      ),
      publicCertificateBundle,
      publicCertificateKey,
    });
    renderCertificateSettings(api);
    const row = await screen.findByTestId('cert-public-domain-pub.example.test');
    expect(row).toHaveTextContent('pub.example.test');

    fireEvent.click(
      within(row).getByRole('button', { name: de.certificatesPublicButtonDownloadBundle }),
    );
    await waitFor(() => expect(publicCertificateBundle).toHaveBeenCalledWith('pub.example.test'));

    fireEvent.click(
      within(row).getByRole('button', { name: de.certificatesPublicButtonDownloadKey }),
    );
    await waitFor(() => expect(publicCertificateKey).toHaveBeenCalledWith('pub.example.test'));
  });

  it('zeigt keine Download-Zeile, solange keine öffentliche Domain konfiguriert ist', async () => {
    const api = makeApi({
      getSystemSettings: vi.fn(async () => settingsFixture({ cert_enabled: true })),
    });
    renderCertificateSettings(api);
    await screen.findByRole('region', { name: de.settingsCertPublicTitle });
    expect(
      screen.queryByRole('button', { name: de.certificatesPublicButtonDownloadBundle }),
    ).not.toBeInTheDocument();
  });
});

describe('CertificateSettings https-switch mode + proxy-listen-port base (P4 Task 11)', () => {
  it('defaults the mode select to manual and PUTs a non-auto change (selected) immediately, no confirm', async () => {
    const updateSystemSettings = vi.fn(async (_body: Record<string, unknown>) =>
      settingsFixture({ cert_https_switch_mode: 'selected' }),
    );
    renderCertificateSettings(makeApi({ updateSystemSettings }));

    const select = await screen.findByLabelText(de.certificatesHTTPSSwitchMode);
    expect(select).toHaveTextContent(de.certificatesHTTPSSwitchModeManual);

    fireEvent.mouseDown(select);
    fireEvent.click(
      await screen.findByRole('option', { name: de.certificatesHTTPSSwitchModeSelected }),
    );

    await waitFor(() => expect(updateSystemSettings).toHaveBeenCalledTimes(1));
    expect(updateSystemSettings.mock.calls[0][0]).toEqual({ cert_https_switch_mode: 'selected' });
    expect(screen.queryByText(de.certificatesHTTPSSwitchModeConfirmTitle)).not.toBeInTheDocument();
  });

  it('selecting auto opens a confirm dialog and does NOT PUT yet', async () => {
    const updateSystemSettings = vi.fn(async (_body: Record<string, unknown>) => settingsFixture());
    renderCertificateSettings(
      makeApi({
        getSystemSettings: vi.fn(async () => settingsFixture({ cert_https_switch_mode: 'manual' })),
        updateSystemSettings,
      }),
    );

    fireEvent.mouseDown(await screen.findByLabelText(de.certificatesHTTPSSwitchMode));
    fireEvent.click(
      await screen.findByRole('option', { name: de.certificatesHTTPSSwitchModeAuto }),
    );

    expect(await screen.findByText(de.certificatesHTTPSSwitchModeConfirmTitle)).toBeInTheDocument();
    expect(screen.getByText(de.certificatesHTTPSSwitchModeConfirmBody)).toBeInTheDocument();
    expect(updateSystemSettings).not.toHaveBeenCalled();
  });

  it('PUTs cert_https_switch_mode alone -- exactly once -- after confirming auto', async () => {
    const updateSystemSettings = vi.fn(async (_body: Record<string, unknown>) =>
      settingsFixture({ cert_https_switch_mode: 'auto' }),
    );
    renderCertificateSettings(
      makeApi({
        getSystemSettings: vi.fn(async () => settingsFixture({ cert_https_switch_mode: 'manual' })),
        updateSystemSettings,
      }),
    );

    fireEvent.mouseDown(await screen.findByLabelText(de.certificatesHTTPSSwitchMode));
    fireEvent.click(
      await screen.findByRole('option', { name: de.certificatesHTTPSSwitchModeAuto }),
    );
    await screen.findByText(de.certificatesHTTPSSwitchModeConfirmTitle);

    fireEvent.click(screen.getByRole('button', { name: de.confirm }));

    await waitFor(() => expect(updateSystemSettings).toHaveBeenCalledTimes(1));
    expect(updateSystemSettings.mock.calls[0][0]).toEqual({ cert_https_switch_mode: 'auto' });
    await waitFor(() =>
      expect(
        screen.queryByText(de.certificatesHTTPSSwitchModeConfirmTitle),
      ).not.toBeInTheDocument(),
    );
  });

  it('cancelling the auto confirm fires no PUT and leaves the select at the prior value', async () => {
    const updateSystemSettings = vi.fn(async (_body: Record<string, unknown>) => settingsFixture());
    renderCertificateSettings(
      makeApi({
        getSystemSettings: vi.fn(async () => settingsFixture({ cert_https_switch_mode: 'manual' })),
        updateSystemSettings,
      }),
    );

    const select = await screen.findByLabelText(de.certificatesHTTPSSwitchMode);
    fireEvent.mouseDown(select);
    fireEvent.click(
      await screen.findByRole('option', { name: de.certificatesHTTPSSwitchModeAuto }),
    );
    await screen.findByText(de.certificatesHTTPSSwitchModeConfirmTitle);

    fireEvent.click(screen.getByRole('button', { name: de.cancel }));

    await waitFor(() =>
      expect(
        screen.queryByText(de.certificatesHTTPSSwitchModeConfirmTitle),
      ).not.toBeInTheDocument(),
    );
    expect(updateSystemSettings).not.toHaveBeenCalled();
    expect(await screen.findByLabelText(de.certificatesHTTPSSwitchMode)).toHaveTextContent(
      de.certificatesHTTPSSwitchModeManual,
    );
  });

  it('saves cert_proxy_listen_port_base alone when its Save button is clicked', async () => {
    const updateSystemSettings = vi.fn(async (_body: Record<string, unknown>) =>
      settingsFixture({ cert_proxy_listen_port_base: 9000 }),
    );
    renderCertificateSettings(
      makeApi({
        getSystemSettings: vi.fn(async () =>
          settingsFixture({ cert_proxy_listen_port_base: 8600 }),
        ),
        updateSystemSettings,
      }),
    );

    const field = await screen.findByLabelText(de.certificatesProxyListenPortBase);
    expect(field).toHaveValue(8600);
    fireEvent.change(field, { target: { value: '9000' } });

    const section = await screen.findByTestId('certificate-https-switch-status');
    fireEvent.click(within(section).getByRole('button', { name: de.save }));

    await waitFor(() => expect(updateSystemSettings).toHaveBeenCalledTimes(1));
    expect(updateSystemSettings.mock.calls[0][0]).toEqual({ cert_proxy_listen_port_base: 9000 });
  });
});
