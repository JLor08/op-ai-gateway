// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { EdgeCertificatePanel } from './EdgeCertificatePanel';
import { ToastProvider } from './shared/ToastProvider';
import { messages } from '../i18n';
import { PortalApiError } from '../api';
import type { EdgeCertificate, SystemSettings } from '../api';
import type { PortalApi } from './shared/types';

const t = messages.de;
const de = t;

function edgeFixture(overrides: Partial<EdgeCertificate> = {}): EdgeCertificate {
  return {
    enabled: true,
    issuer_mode: 'acme',
    names: ['edge.example.test'],
    domain: 'edge.example.test',
    status: 'active',
    fingerprint: 'AA:BB',
    not_before: new Date().toISOString(),
    not_after: new Date(Date.now() + 42 * 24 * 3600 * 1000).toISOString(),
    issued_at: new Date().toISOString(),
    delivery_mode: 'local',
    output_dir: '/etc/op-gateway/edge',
    written_at: new Date().toISOString(),
    key_download_available: false,
    require_https: false,
    https_observed: false,
    ...overrides,
  };
}

// U-T4: EdgeCertificatePanel now also loads api.getSystemSettings() for the
// cert_edge_acme_* fields AcmeConfigFields edits -- those live on SystemSettings,
// not on the edge DTO above. Defaults mirror the fixed-value default the
// component itself falls back to (shared account, production directory, the
// LE-production weekly ceiling) so a test that never touches the ACME switch
// still exercises a realistic, self-consistent state.
function settingsFixture(overrides: Record<string, unknown> = {}): SystemSettings {
  return {
    cert_edge_acme_shared: true,
    cert_edge_acme_email: '',
    cert_edge_acme_directory_url: 'https://acme-v02.api.letsencrypt.org/directory',
    cert_edge_acme_weekly_limit: 50,
    ...overrides,
  } as unknown as SystemSettings;
}

type EdgeCertificatePanelApi = Pick<
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

function makeApi(overrides: Partial<EdgeCertificatePanelApi> = {}): EdgeCertificatePanelApi {
  return {
    edgeCertificate: vi.fn(async () => edgeFixture()),
    reissueEdgeCertificate: vi.fn(async () => ({ ok: true })),
    edgeCertificateBundle: vi.fn(
      async () => '-----BEGIN CERTIFICATE-----\nAAA\n-----END CERTIFICATE-----\n',
    ),
    edgeCertificateKey: vi.fn(
      async () => '-----BEGIN PRIVATE KEY-----\nBBB\n-----END PRIVATE KEY-----\n',
    ),
    edgeProxyConfig: vi.fn(async () => 'server {\n  # ...\n}\n'),
    getSystemSettings: vi.fn(async () => settingsFixture()),
    updateSystemSettings: vi.fn(async (_body: Record<string, unknown>) => ({}) as never),
    probeEdgeTLS: vi.fn(async () => ({
      ok: true,
      target: 'web:443',
      message: 'the edge listener presents a valid, trusted certificate',
    })),
    ...overrides,
  } as unknown as EdgeCertificatePanelApi;
}

function renderPanel(api: EdgeCertificatePanelApi) {
  render(
    <ToastProvider>
      <EdgeCertificatePanel t={t} api={api} />
    </ToastProvider>,
  );
}

beforeEach(() => {
  // jsdom does not implement URL.createObjectURL/revokeObjectURL -- the shared
  // downloadText() helper calls both. Stub them so a download click exercises
  // the real code path instead of surfacing as an unrelated error toast (mirrors
  // no OTHER test file in this suite actually asserting on the resulting Blob --
  // the meaningful assertion is which API method the click reaches).
  window.URL.createObjectURL = vi.fn(() => 'blob:mock');
  window.URL.revokeObjectURL = vi.fn();
});

afterEach(() => {
  cleanup();
});

describe('EdgeCertificatePanel', () => {
  it('zeigt den Download-Weg und verbirgt den Key-Knopf, wenn das Gateway selbst ausliefert', async () => {
    const api = makeApi({
      edgeCertificate: vi.fn(async () =>
        edgeFixture({
          delivery_mode: 'local',
          output_dir: '/etc/op-gateway/edge',
          written_at: '2026-08-13T10:00:00Z',
          key_download_available: false,
        }),
      ),
    });
    renderPanel(api);
    await screen.findByText('edge.example.test');
    // Nennt den Pfad -- die einzige verlässliche Aussage, dass hier "lokal
    // ausgeliefert" gemeint ist, nicht nur eine generische Erfolgsmeldung.
    expect(screen.getByTestId('cert-edge-delivery')).toHaveTextContent('/etc/op-gateway/edge');
    expect(
      screen.queryByRole('button', { name: de.certificatesEdgeButtonDownloadKey }),
    ).not.toBeInTheDocument();
  });

  it("zeigt einen Hinweis auf 'noch nicht geschrieben', solange written_at fehlt", async () => {
    const api = makeApi({
      edgeCertificate: vi.fn(async () =>
        edgeFixture({
          delivery_mode: 'local',
          output_dir: '/etc/op-gateway/edge',
          written_at: undefined,
        }),
      ),
    });
    renderPanel(api);
    await screen.findByText('edge.example.test');
    expect(screen.getByTestId('cert-edge-delivery')).toHaveTextContent('noch nicht geschrieben');
  });

  // Fix round 1, IMPORTANT 1: the reconcile only ever writes when the row is
  // actually WANTED (backend edgeWanted = EdgeEnabled && len(EdgeNames)>0,
  // service_edge_cert.go:74-92 / DeliverEdgeCertificate's edgeWanted gate,
  // service_certificates.go:673-679). A fresh install ships
  // OP_AI_GATEWAY_CERT_EDGE_OUTPUT_DIR set while cert_edge_enabled defaults to
  // false -- delivery_mode still reads "local" (EdgeDeliveryCapable only checks
  // the output dir/write-error, independent of the enabled toggle) with no
  // written_at, so the panel must NOT claim the next reconcile pass will write
  // there: it never will until the switch is actually on. edgeFixture()
  // hard-codes enabled:true, so this scenario needs an explicit override.
  it('verspricht KEINE Auslieferung, wenn das Feature aus ist (lokal, pending, aber enabled=false)', async () => {
    const api = makeApi({
      edgeCertificate: vi.fn(async () =>
        edgeFixture({
          enabled: false,
          delivery_mode: 'local',
          output_dir: '/etc/op-gateway/edge',
          written_at: undefined,
        }),
      ),
    });
    renderPanel(api);
    await screen.findByLabelText(de.settingsCertEdgeIssuerMode);
    const deliveryText = screen.getByTestId('cert-edge-delivery');
    // Must NOT make the "will be written at the next reconcile pass" promise --
    // that promise is false while the feature is off.
    expect(deliveryText).not.toHaveTextContent('nächsten Reconcile-Durchlauf');
    expect(deliveryText).toHaveTextContent('/etc/op-gateway/edge');
  });

  // Same root cause: an enabled switch with NO names configured is equally
  // "not wanted" (edgeDesired requires len(EdgeNames)>0 too) -- the same
  // honest branch must cover this half of the gate as well.
  it('verspricht KEINE Auslieferung, wenn kein Name konfiguriert ist (enabled=true, aber keine Namen)', async () => {
    const api = makeApi({
      edgeCertificate: vi.fn(async () =>
        edgeFixture({
          enabled: true,
          names: [],
          delivery_mode: 'local',
          output_dir: '/etc/op-gateway/edge',
          written_at: undefined,
        }),
      ),
    });
    renderPanel(api);
    await screen.findByLabelText(de.settingsCertEdgeIssuerMode);
    expect(screen.getByTestId('cert-edge-delivery')).not.toHaveTextContent(
      'nächsten Reconcile-Durchlauf',
    );
  });

  // Same root cause, the reissue button: marking a row due is inert while the
  // reconcile will never act on it (not wanted) -- clicking it anyway is
  // misleading. Only !hasRow was gating it before this fix.
  it("sperrt 'Jetzt neu ausstellen', wenn das Feature deaktiviert ist, auch mit vorhandener Zeile", async () => {
    const api = makeApi({
      edgeCertificate: vi.fn(async () =>
        edgeFixture({ enabled: false, domain: 'edge.example.test' }),
      ),
    });
    renderPanel(api);
    const reissueButton = await screen.findByRole('button', {
      name: de.certificatesEdgeButtonReissue,
    });
    expect(reissueButton).toBeDisabled();
  });

  it('bietet den Key-Download nur an, wenn das Gateway nicht ausliefern kann', async () => {
    const edgeCertificateKey = vi.fn(
      async () => '-----BEGIN PRIVATE KEY-----\nBBB\n-----END PRIVATE KEY-----\n',
    );
    const api = makeApi({
      edgeCertificate: vi.fn(async () =>
        edgeFixture({
          delivery_mode: 'download',
          output_dir: '',
          written_at: undefined,
          key_download_available: true,
        }),
      ),
      edgeCertificateKey,
    });
    renderPanel(api);
    const keyButton = await screen.findByRole('button', {
      name: de.certificatesEdgeButtonDownloadKey,
    });
    fireEvent.click(keyButton);
    await waitFor(() => expect(edgeCertificateKey).toHaveBeenCalledTimes(1));
  });

  // Final-review I2: the key button's guard is key_download_available, NOT the
  // delivery mode -- and the two fixtures above co-vary them, so swapping the
  // guard for delivery_mode === "download" kept the whole file green. THIS is the
  // discriminating state, and it is the ordinary one on a k8s deployment before
  // the first issuance: no output directory (=> "download") and nothing stored
  // yet (=> key_download_available false, pinned backend-side by
  // TestEdgeCertificateViewOffersNoKeyDownloadWithoutAStoredKey). A button here
  // would 404 against EdgeCertificateKeyPEM's second condition.
  it('bietet KEINEN Key-Download an, solange nichts gespeichert ist (Download-Modus, aber kein Key)', async () => {
    const edgeCertificateKey = vi.fn(async () => '');
    const api = makeApi({
      edgeCertificate: vi.fn(async () =>
        edgeFixture({
          delivery_mode: 'download',
          output_dir: '',
          written_at: undefined,
          domain: undefined,
          status: undefined,
          key_download_available: false,
        }),
      ),
      edgeCertificateKey,
    });
    renderPanel(api);
    await screen.findByText(de.certificatesEdgeNone);
    expect(
      screen.queryByRole('button', { name: de.certificatesEdgeButtonDownloadKey }),
    ).not.toBeInTheDocument();
    expect(edgeCertificateKey).not.toHaveBeenCalled();
  });

  it('zeigt die generierte Proxy-Konfiguration im Dialog mit Kopier-Knopf', async () => {
    const edgeProxyConfig = vi.fn(async () => 'server {\n  # ...\n}\n');
    const api = makeApi({ edgeProxyConfig });
    renderPanel(api);
    await screen.findByText('edge.example.test');
    fireEvent.click(
      await screen.findByRole('button', { name: de.certificatesEdgeButtonShowProxyConfig }),
    );
    await waitFor(() => expect(edgeProxyConfig).toHaveBeenCalledTimes(1));
    expect(await screen.findByText(de.certificatesEdgeProxyConfigDialogTitle)).toBeInTheDocument();
    expect(screen.getByTestId('cert-edge-proxy-config-text')).toHaveTextContent('server { # ... }');
    expect(
      screen.getByRole('button', { name: de.certificatesEdgeProxyConfigCopy }),
    ).toBeInTheDocument();
    // Die Momentaufnahme-Warnung steht dabei -- niemand soll einen Marker
    // fuer bar nehmen und einfuegen.
    expect(screen.getByText(de.certificatesEdgeProxyConfigSnapshotNote)).toBeInTheDocument();
  });

  it('zeigt die Namenskollision, wenn der Backend-DTO sie meldet', async () => {
    const api = makeApi({
      edgeCertificate: vi.fn(async () => edgeFixture({ name_conflict: 'edge.example.test' })),
    });
    renderPanel(api);
    expect(
      await screen.findByText(de.certificatesEdgeNameConflict('edge.example.test')),
    ).toBeInTheDocument();
  });

  it("zeigt 'noch kein Zertifikat' ohne gespeicherte Zeile", async () => {
    const api = makeApi({
      edgeCertificate: vi.fn(async () => edgeFixture({ domain: undefined, status: undefined })),
    });
    renderPanel(api);
    expect(await screen.findByText(de.certificatesEdgeNone)).toBeInTheDocument();
  });

  it('speichert Schalter/Aussteller/Namen als eigene, disjunkte PUT-Partition (nur cert_edge_*)', async () => {
    const updateSystemSettings = vi.fn(async (_body: Record<string, unknown>) => ({}) as never);
    const api = makeApi({ updateSystemSettings });
    renderPanel(api);
    const namesField = (await screen.findByLabelText(de.settingsCertEdgeNames)) as HTMLInputElement;
    fireEvent.change(namesField, { target: { value: 'edge.example.test, 10.0.0.5' } });
    fireEvent.click(screen.getByRole('button', { name: de.save }));
    await waitFor(() => expect(updateSystemSettings).toHaveBeenCalledTimes(1));
    const payload = updateSystemSettings.mock.calls[0][0];
    // U-T4: the edge's own AcmeConfigFields (shared-vs-own ACME account) now
    // saves together with the switch/issuer/names above, in the SAME disjoint
    // partition -- still never cert_enabled and never the internal cert_*/
    // acme_* fields CertificateSettings' own save sends.
    expect(payload).toEqual({
      cert_edge_enabled: true,
      cert_edge_issuer_mode: 'acme',
      cert_edge_names: ['edge.example.test', '10.0.0.5'],
      cert_edge_acme_shared: true,
      cert_edge_acme_email: '',
      cert_edge_acme_directory_url: 'https://acme-v02.api.letsencrypt.org/directory',
      cert_edge_acme_weekly_limit: 50,
    });
  });

  // U-T4: AcmeConfigFields wired into this panel -- the shared-vs-own switch,
  // plus a fixed read-only weekly limit for Production vs. an editable one
  // under Custom.
  describe('ACME-Konfiguration (U-T4, AcmeConfigFields)', () => {
    it("zeigt bei 'eigene ACME-Einstellungen' E-Mail, Directory und Wochenlimit", async () => {
      const api = makeApi();
      renderPanel(api);
      await screen.findByLabelText(de.settingsCertEdgeIssuerMode);
      expect(screen.queryByLabelText(de.settingsAcmeEmail)).toBeNull();

      fireEvent.click(screen.getByRole('switch', { name: de.settingsAcmeOwnSettings }));

      expect(screen.getByLabelText(de.settingsAcmeEmail)).toBeInTheDocument();
      expect(screen.getByLabelText(de.settingsAcmeDirectory)).toBeInTheDocument();
      const weeklyLimit = screen.getByLabelText(de.settingsAcmeWeeklyLimit) as HTMLInputElement;
      expect(weeklyLimit.value).toBe('50');
      expect(weeklyLimit).toBeDisabled();
    });

    it('speichert eigene ACME-Einstellungen zusammen mit Schalter/Aussteller/Namen', async () => {
      const updateSystemSettings = vi.fn(async (_body: Record<string, unknown>) => ({}) as never);
      const api = makeApi({ updateSystemSettings });
      renderPanel(api);
      await screen.findByLabelText(de.settingsCertEdgeIssuerMode);
      fireEvent.click(screen.getByRole('switch', { name: de.settingsAcmeOwnSettings }));
      const email = screen.getByLabelText(de.settingsAcmeEmail) as HTMLInputElement;
      fireEvent.change(email, { target: { value: 'edge-ops@example.test' } });

      fireEvent.click(screen.getByRole('button', { name: de.save }));
      await waitFor(() => expect(updateSystemSettings).toHaveBeenCalledTimes(1));
      expect(updateSystemSettings.mock.calls[0][0]).toMatchObject({
        cert_edge_acme_shared: false,
        cert_edge_acme_email: 'edge-ops@example.test',
        cert_edge_acme_directory_url: 'https://acme-v02.api.letsencrypt.org/directory',
        cert_edge_acme_weekly_limit: 50,
      });
    });

    // Round-1 review, finding 1 (weekly-limit divergence): the backend's REAL
    // "unset" default is 0 (nonNegativeIntSetting), not undefined/absent --
    // this fixture sets it explicitly to 0, exactly like a real GET would (a
    // fixture that merely omits the field never actually reproduces the bug).
    it('zeigt und speichert bei Produktion weiterhin 50, auch wenn der reale Stand 0 ist (Wochenlimit-Divergenz)', async () => {
      const updateSystemSettings = vi.fn(async (_body: Record<string, unknown>) => ({}) as never);
      const api = makeApi({
        getSystemSettings: vi.fn(async () => settingsFixture({ cert_edge_acme_weekly_limit: 0 })),
        updateSystemSettings,
      });
      renderPanel(api);
      await screen.findByLabelText(de.settingsCertEdgeIssuerMode);
      fireEvent.click(screen.getByRole('switch', { name: de.settingsAcmeOwnSettings }));

      const weeklyLimit = screen.getByLabelText(de.settingsAcmeWeeklyLimit) as HTMLInputElement;
      expect(weeklyLimit.value).toBe('50');

      fireEvent.click(screen.getByRole('button', { name: de.save }));
      await waitFor(() => expect(updateSystemSettings).toHaveBeenCalledTimes(1));
      // The saved number must equal what was just displayed -- 50, never the
      // real stored 0 the fixture seeded.
      expect(updateSystemSettings.mock.calls[0][0]).toMatchObject({
        cert_edge_acme_weekly_limit: 50,
      });
    });
  });

  it('hat ein eigenes, vom internen Aussteller-Dropdown getrenntes Aussteller-Feld', async () => {
    const api = makeApi();
    renderPanel(api);
    const select = await screen.findByLabelText(de.settingsCertEdgeIssuerMode);
    expect(select).toBeInTheDocument();
    // Der interne Aussteller-Dropdown lebt in CertificateSettings, nicht hier --
    // dieses Panel rendert eigenstaendig, also darf dessen Label hier nicht
    // auftauchen (waere es dasselbe Feld, gaebe es das Label zweimal).
    expect(screen.queryByLabelText(de.settingsCertIssuerMode)).not.toBeInTheDocument();
  });

  it('meldet einen 409 certificate.edge_key_managed unterscheidbar von einem generischen Fehler', async () => {
    const edgeCertificateKey = vi.fn(async () => {
      throw new PortalApiError(
        409,
        'certificate.edge_key_managed',
        'the gateway delivers this key itself',
      );
    });
    const api = makeApi({
      edgeCertificate: vi.fn(async () =>
        edgeFixture({
          delivery_mode: 'download',
          output_dir: '',
          written_at: undefined,
          key_download_available: true,
        }),
      ),
      edgeCertificateKey,
    });
    renderPanel(api);
    const keyButton = await screen.findByRole('button', {
      name: de.certificatesEdgeButtonDownloadKey,
    });
    fireEvent.click(keyButton);
    // Der geworfene Fehler traegt den Code unversehrt -- das ist die
    // Unterscheidbarkeit, die api.ts garantieren muss (PortalApiError.code).
    // formatPortalError prefixes every unmapped code onto the toast message, so
    // the 409 reads distinctly from any other failure without a dedicated i18n
    // mapping (mirrors CertificateSettings' own generic-error handling).
    await waitFor(() => expect(edgeCertificateKey).toHaveBeenCalledTimes(1));
    expect(await screen.findByRole('alert')).toHaveTextContent('certificate.edge_key_managed');
  });

  it("loest 'Jetzt neu ausstellen' erst nach Bestaetigung aus", async () => {
    const reissueEdgeCertificate = vi.fn(async () => ({ ok: true }));
    const api = makeApi({ reissueEdgeCertificate });
    renderPanel(api);
    fireEvent.click(await screen.findByRole('button', { name: de.certificatesEdgeButtonReissue }));
    expect(await screen.findByText(de.certificatesEdgeReissueConfirmTitle)).toBeInTheDocument();
    expect(reissueEdgeCertificate).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole('button', { name: de.confirm }));
    await waitFor(() => expect(reissueEdgeCertificate).toHaveBeenCalledTimes(1));
  });

  describe('Klartext-Riegel (cert_edge_require_https)', () => {
    it('sperrt den Schalter, solange keine verschluesselte Anfrage beobachtet wurde', async () => {
      const api = makeApi({
        edgeCertificate: vi.fn(async () =>
          edgeFixture({ require_https: false, https_observed: false }),
        ),
      });
      renderPanel(api);
      const toggle = await screen.findByRole('switch', { name: de.settingsCertEdgeRequireHttps });
      expect(toggle).toBeDisabled();
      expect(screen.getByText(de.certificatesEdgeGateDisabledHint)).toBeInTheDocument();
    });

    it('gibt den Schalter frei, sobald eine verschluesselte Anfrage beobachtet wurde', async () => {
      const api = makeApi({
        edgeCertificate: vi.fn(async () =>
          edgeFixture({ require_https: false, https_observed: true }),
        ),
      });
      renderPanel(api);
      const toggle = await screen.findByRole('switch', { name: de.settingsCertEdgeRequireHttps });
      expect(toggle).not.toBeDisabled();
      expect(screen.queryByText(de.certificatesEdgeGateDisabledHint)).not.toBeInTheDocument();
    });

    it('sperrt einen BEREITS armierten Schalter (auf Ausschalten), sobald die Beobachtung abgerissen ist', async () => {
      // require_https already true but https_observed false: this is the
      // "armed, observation lapsed" state, distinct from "never armed yet" --
      // what the disabled switch blocks here is turning the gate OFF, not on.
      // The disabled hint must still show (the switch is disabled either way)
      // and must not claim the switch can only ever be turned "on".
      const api = makeApi({
        edgeCertificate: vi.fn(async () =>
          edgeFixture({ require_https: true, https_observed: false }),
        ),
      });
      renderPanel(api);
      const toggle = await screen.findByRole('switch', { name: de.settingsCertEdgeRequireHttps });
      expect(toggle).toBeChecked();
      expect(toggle).toBeDisabled();
      expect(screen.getByText(de.certificatesEdgeGateDisabledHint)).toBeInTheDocument();
      // A CHECKED switch reads as "plaintext is being refused right now", which is
      // false in this state -- the runtime gate self-extinguishes without a fresh
      // observation. The panel must say so, or the operator will chase a lockout
      // that is not happening.
      expect(screen.getByTestId('cert-edge-gate-not-enforcing')).toHaveTextContent(
        de.certificatesEdgeGateArmedNotEnforcingHint,
      );
    });

    it("behauptet NICHT 'verweigert nichts', solange der Riegel gar nicht armiert ist", async () => {
      // Anti-false-positive for the hint above: unobserved AND unarmed must show
      // only the lock explanation, never the "armed but enforcing nothing" line.
      const api = makeApi({
        edgeCertificate: vi.fn(async () =>
          edgeFixture({ require_https: false, https_observed: false }),
        ),
      });
      renderPanel(api);
      await screen.findByRole('switch', { name: de.settingsCertEdgeRequireHttps });
      expect(screen.queryByTestId('cert-edge-gate-not-enforcing')).not.toBeInTheDocument();
    });

    it("behauptet NICHT 'verweigert nichts', wenn der armierte Riegel tatsaechlich greift", async () => {
      const api = makeApi({
        edgeCertificate: vi.fn(async () =>
          edgeFixture({ require_https: true, https_observed: true }),
        ),
      });
      renderPanel(api);
      await screen.findByRole('switch', { name: de.settingsCertEdgeRequireHttps });
      expect(screen.queryByTestId('cert-edge-gate-not-enforcing')).not.toBeInTheDocument();
    });

    it('fragt vor dem Einschalten nach Bestaetigung, nennt die vier Ausnahmen, und sendet NUR das eine Feld', async () => {
      const updateSystemSettings = vi.fn(async (_body: Record<string, unknown>) => ({}) as never);
      const api = makeApi({
        edgeCertificate: vi.fn(async () =>
          edgeFixture({ require_https: false, https_observed: true }),
        ),
        updateSystemSettings,
      });
      renderPanel(api);
      const toggle = await screen.findByRole('switch', { name: de.settingsCertEdgeRequireHttps });
      fireEvent.click(toggle);
      // The confirm dialog names the four always-open paths -- the operator must
      // not believe arming means "zero plaintext".
      expect(await screen.findByText(de.certificatesEdgeGateArmConfirmTitle)).toBeInTheDocument();
      expect(screen.getByText(de.certificatesEdgeGateArmConfirmBody)).toBeInTheDocument();
      expect(updateSystemSettings).not.toHaveBeenCalled();
      fireEvent.click(screen.getByRole('button', { name: de.confirm }));
      await waitFor(() => expect(updateSystemSettings).toHaveBeenCalledTimes(1));
      // A SEPARATE call from the other cert_edge_* fields -- never bundled
      // (api.ts's own comment: re-bundling would re-run the arming precondition
      // on every unrelated save).
      expect(updateSystemSettings.mock.calls[0][0]).toEqual({ cert_edge_require_https: true });
    });

    it('schaltet ohne Bestaetigung sofort aus (Entwaffnen ist nie gegated)', async () => {
      const updateSystemSettings = vi.fn(async (_body: Record<string, unknown>) => ({}) as never);
      const api = makeApi({
        edgeCertificate: vi.fn(async () =>
          edgeFixture({ require_https: true, https_observed: true }),
        ),
        updateSystemSettings,
      });
      renderPanel(api);
      const toggle = await screen.findByRole('switch', { name: de.settingsCertEdgeRequireHttps });
      expect(toggle).toBeChecked();
      fireEvent.click(toggle);
      expect(screen.queryByText(de.certificatesEdgeGateArmConfirmTitle)).not.toBeInTheDocument();
      await waitFor(() => expect(updateSystemSettings).toHaveBeenCalledTimes(1));
      expect(updateSystemSettings.mock.calls[0][0]).toEqual({ cert_edge_require_https: false });
    });

    it('zeigt beide Hop-Zeitstempel, mit einem eigenen Hinweis, wenn noch nie beobachtet', async () => {
      const api = makeApi({
        edgeCertificate: vi.fn(async () =>
          edgeFixture({
            https_observed: true,
            last_encrypted_at: '2026-08-14T10:00:00Z',
            last_plain_at: undefined,
          }),
        ),
      });
      renderPanel(api);
      await screen.findByRole('switch', { name: de.settingsCertEdgeRequireHttps });
      expect(screen.getByTestId('cert-edge-gate-last-encrypted')).not.toHaveTextContent(
        de.certificatesEdgeGateLastEncryptedNever,
      );
      expect(screen.getByTestId('cert-edge-gate-last-plain')).toHaveTextContent(
        de.certificatesEdgeGateLastPlainNever,
      );
    });

    it('zeigt die Ursache eines fehlgeschlagenen TLS-Selbsttests (nicht nur bestanden/fehlgeschlagen)', async () => {
      const probeEdgeTLS = vi.fn(async () => ({
        ok: false,
        reason: 'name_mismatch',
        target: 'web:443',
        expected_name: 'edge.example.test',
        sans: ['other.example.test'],
      }));
      const api = makeApi({ probeEdgeTLS });
      renderPanel(api);
      fireEvent.click(
        await screen.findByRole('button', { name: de.certificatesEdgeGateProbeButton }),
      );
      await waitFor(() => expect(probeEdgeTLS).toHaveBeenCalledTimes(1));
      expect(await screen.findByTestId('cert-edge-probe-result')).toHaveTextContent(
        de.certificatesEdgeProbeNameMismatch('edge.example.test', 'other.example.test'),
      );
    });

    it('zeigt einen eigenen Hinweis (nicht nur einen Toast), wenn der Selbsttest nicht konfiguriert ist', async () => {
      const probeEdgeTLS = vi.fn(async () => {
        throw new PortalApiError(409, 'certificate.edge_probe_not_configured', 'not configured');
      });
      const api = makeApi({ probeEdgeTLS });
      renderPanel(api);
      fireEvent.click(
        await screen.findByRole('button', { name: de.certificatesEdgeGateProbeButton }),
      );
      await waitFor(() => expect(probeEdgeTLS).toHaveBeenCalledTimes(1));
      expect(await screen.findByTestId('cert-edge-probe-result')).toHaveTextContent(
        de.certificatesEdgeProbeNotConfigured,
      );
    });
  });
});
