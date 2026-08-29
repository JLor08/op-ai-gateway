// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { execSync, spawn, type ChildProcess } from "node:child_process";
import * as crypto from "node:crypto";
import * as fs from "node:fs";
import * as http from "node:http";
import * as https from "node:https";
import * as net from "node:net";
import * as os from "node:os";
import * as path from "node:path";
import { expect, test, type Page } from "@playwright/test";
import { messages } from "../../frontend/src/i18n";
import { login } from "../e2e/helpers";
import {
  CERTS_ADMIN_EMAIL,
  CERTS_ADMIN_NAME,
  CERTS_ADMIN_PASSWORD,
  FAKEACME_DIRECTORY_URL
} from "../playwright.certificates.config";

// Live end-to-end proof of the TLS-certificate management feature (design:
// docs/superpowers/specs/2026-08-12-certificates-p1-design.md, extended by
// docs/superpowers/specs/2026-08-13-gateway-edge-tls-design.md for the
// gateway's OWN edge/nginx certificate) in BOTH issuer modes, through the
// real portal UI, against a REAL SQLITE-BACKED gateway + a standalone fake
// ACME directory (e2e-certificates/fakeacme).
//
// Three scenarios, self-signed FIRST (spec's own ordering: it needs no
// external dependency, so it fails more legibly on a real defect than the
// ACME scenario would). The gateway process (and its sqlite file) persists
// across ALL THREE tests in this file — Scenario 2 continues on the
// module/issuer configuration + the "a.int.example.test" server + certificate
// Scenario 1 already established, exactly as the design brief frames it ("vor
// dem Wechsel" implies the internal-CA state from Scenario 1 is still live);
// Scenario 3 in turn depends on Scenario 2 having already switched the
// INTERNAL issuer mode to acme (its "before" baseline is server A's
// acme-issued certificate) and adds the gateway's OWN edge certificate — a
// fully separate row/mode, its own switch/issuer/name set, configured and
// switched with ZERO effect on the internal certificates the first two
// scenarios drive, which is the whole point of Scenario 3.
//
// State-changing actions go through the real UI (nav clicks, form fills,
// button clicks); numeric/timestamp assertions that need EXACT precision
// (unchanged-vs-changed expiry across a reconcile pass) read the same portal
// API the UI itself calls (GET /api/system/certificates[/ca]) via
// page.request, which is session-cookie authenticated (the elevated
// system_admin's own session) — the brief explicitly sanctions this same mix
// (its own step 7 reads the CA bundle via `request`). One deviation from the
// brief's literal wording, documented where it happens below: server
// creation uses the elevated session (page.request), not a raw Bearer
// Authorization header with the bootstrap API token — see createServerRaw's
// doc comment for why.

const t = messages.de;

const BASE_DOMAIN = "int.example.test";
const SERVER_A_NAME = "E2E Cert Server Alpha";
const SERVER_A_DOMAIN = `a.${BASE_DOMAIN}`;
const SERVER_B_NAME = "E2E Cert Server Beta";
const SERVER_B_DOMAIN = "b.other.example.net";

// Scenario 3 (the edge/gateway-nginx certificate): a DNS name under the same
// base domain (so an eventual ACME switch's HTTP-01 challenge is fetched by
// the SAME fake-ACME-callback wiring Scenario 2 already proved works) plus a
// bare IP address -- RFC 5737 TEST-NET-3, reserved for documentation and
// never routable, so there is no real-world collision risk.
const EDGE_HOST_NAME = `edge.${BASE_DOMAIN}`;
const EDGE_IP = "203.0.113.77";

// Scenario 5 (Phase 2 distribution: the REAL ServerAgent binary installs a REAL
// certificate). Under the base domain, so the internal issuer -- which the
// earlier scenarios left switched to `acme` -- can actually order it against the
// fake ACME directory (b.other.example.net proves the opposite case: outside the
// base domain, HTTP-01 cannot serve it).
//
// The domain is deliberately NOT the alphabetically first of this suite's
// certificate rows ("a.int.example.test" < "b.other.example.net" <
// "c.int.example.test" < "edge.int.example.test"): the cross-server assert below
// would otherwise pass by accident against a gateway that ignored the agent
// token and simply served the first row it found.
const SERVER_C_NAME = "E2E Cert Server Gamma";
const SERVER_C_DOMAIN = `c.${BASE_DOMAIN}`;

// Scenario 7 (the unified public-domain block's OWN, non-shared ACME
// account): deliberately OUTSIDE the base domain -- unlike the internal
// (server/gateway) names, a public domain is never subject to the
// under-base-domain ACME rule (desiredCertificates adds it unconditionally
// whenever cert_manage_public_domain is on; see service_certificates.go's
// desiredCertificates and the "public-only pass" exemptions in
// ReconcileCertificates), so picking a name OUTSIDE int.example.test is the
// stronger proof that this really is an independent, standalone-FQDN context,
// not a special case of the internal base-domain names. The fake ACME
// server's HTTP-01 challenge fetch (fakeacme/main.go's handleChal) calls back
// into the gateway's public listener by FIXED ADDRESS, not by the domain
// being validated, so no separate callback wiring is needed for a name
// outside the base domain -- the existing FAKEACME_CHALLENGE_BASE wiring
// Scenario 2/3 already exercises covers this domain too.
const PUBLIC_DOMAIN = "public.example.net";

// Scenario 7's own directory URL: the SAME fake-ACME server Scenario 2/3
// already prove works (identical host:port, `FAKEACME_ADDR`), reached via the
// hostname "localhost" instead of the literal "127.0.0.1". This is NOT
// cosmetic: the write path rejects a bare-IP HOST for a context's OWN
// (non-shared) ACME directory whenever its effective issuer mode is acme
// (`acmeDirectoryHostIsBareIP`, service_system_settings.go -- well
// unit-tested, e.g. TestUpdateSystemSettingsRejectsAnIPACMEDirectoryHost) --
// deliberately, since a real ACME directory is always DNS-named and this
// catches the common misconfiguration of pasting a bare address where a
// hostname belongs. That guard does NOT apply to the GLOBAL/shared
// `acme_directory_url` Scenario 2/3 use (only to the edge/public *_acme_shared=
// false path), which is why those scenarios can use FAKEACME_DIRECTORY_URL's
// literal IP directly while this one cannot. "localhost" resolves to
// 127.0.0.1 via the OS resolver like it does everywhere else, so this reaches
// the exact same fakeacme process -- only the write-time validation differs.
const PUBLIC_ACME_DIRECTORY_URL = FAKEACME_DIRECTORY_URL.replace("127.0.0.1", "localhost");

const CSRF = { "X-OP-CSRF": "1" };

type CertRow = {
  domain: string;
  kind: string;
  status: string;
  fingerprint?: string;
  not_before?: string;
  not_after?: string;
  issued_at?: string;
  next_attempt_at?: string;
  attempt_count: number;
  last_error?: string;
  // Phase 2 distribution feedback (what the row's ServerAgent last reported it
  // actually has on disk) -- the four json tags CertificateDTO pins.
  installed?: boolean;
  installed_fingerprint?: string;
  installed_at?: string;
  installed_mode?: string;
  // Phase 3 mesh-transport observation (only on kind=server rows).
  transport?: "tls" | "plain";
  transport_at?: string;
};

type CertCA = {
  present: boolean;
  subject?: string;
  fingerprint?: string;
  not_before?: string;
  not_after?: string;
  previous_fingerprint?: string;
  previous_not_after?: string;
};

function escapeRegExp(s: string): string {
  return s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

/**
 * Enters System-Admin mode (step-up): a fresh system_admin session is NOT
 * elevated by default, and every action this suite drives (System Settings,
 * the Zertifikate view, the NetBird linkage editor, all `/api/system/*`
 * endpoints) requires the `system` scope that only elevation attaches to the
 * session.
 *
 * As of 2026-08-12 (commit `9267344`) the step-up control lives INSIDE the
 * user dropdown, not as a standalone sidebar button — see
 * SystemAdminModeControl.tsx + UserMenu.tsx (the trigger's accessible name is
 * the logged-in user's display name; the control renders as `menuitem`s
 * above "Profil"). This mirrors the working reference in
 * frontend/src/App.test.tsx ("re-fetches scope-dependent content when
 * switching System-Admin mode"): open the dropdown, click the enter
 * menuitem, fill the password in the dialog, click the dialog's own enter
 * button, wait for the dialog to close.
 */
async function enterSystemAdminMode(page: Page, displayName: string, password: string): Promise<void> {
  await page.getByRole("button", { name: displayName }).click();
  await page.getByRole("menuitem", { name: t.systemAdminModeEnter }).click();
  const dialog = page.getByRole("dialog");
  await dialog.getByLabel(t.systemAdminModePasswordLabel).fill(password);
  await dialog.getByRole("button", { name: t.systemAdminModeEnter }).click();
  await expect(dialog).toHaveCount(0);
}

async function gotoCertificates(page: Page): Promise<void> {
  await page.getByRole("link", { name: t.settingsCertificatesTitle, exact: true }).click();
}

/**
 * Selects the issuer mode in the (already-open) Zertifikate view's
 * Einstellungen panel.
 *
 * `exact: true` on the combobox lookup is load-bearing, not defensive style:
 * Playwright's default `name` matching is a case-insensitive SUBSTRING match,
 * and t.settingsCertIssuerMode ("Aussteller") is literally a substring of the
 * Edge-Zertifikat panel's own combobox name, t.settingsCertEdgeIssuerMode
 * ("Aussteller (Edge-Zertifikat)") -- since that panel (T9,
 * EdgeCertificatePanel.tsx) renders unconditionally on the same page, an
 * unscoped lookup here is a strict-mode ambiguity (two matches) the moment it
 * exists, regardless of whether the edge feature is even touched.
 */
async function selectIssuerMode(page: Page, mode: "self_signed" | "acme"): Promise<void> {
  await page.getByRole("combobox", { name: t.settingsCertIssuerMode, exact: true }).click();
  const label = mode === "self_signed" ? t.settingsCertIssuerSelfSigned : t.settingsCertIssuerAcme;
  await page.getByRole("option", { name: label, exact: true }).click();
}

async function selectServerScope(page: Page, scope: "all" | "selected"): Promise<void> {
  await page.getByRole("combobox", { name: t.settingsCertServerScope, exact: true }).click();
  const label = scope === "all" ? t.settingsCertScopeAll : t.settingsCertScopeSelected;
  await page.getByRole("option", { name: label, exact: true }).click();
}

/**
 * Clicks the Zertifikate panel's own "Speichern" (PUT /api/system/settings, the
 * cert_/acme_-field-only partition) and waits for it to land.
 *
 * Scoped to the internal-settings Panel (`aria-labelledby="cert-settings-heading"`,
 * see CertificateSettings.tsx) because the Edge-Zertifikat panel (T9,
 * EdgeCertificatePanel.tsx) renders unconditionally right below it on the SAME
 * "Zertifikate" view and has its OWN, identically-labelled "Speichern" button
 * (t.save, "Speichern", in both) -- an unscoped `getByRole("button", { name:
 * t.save })` would be a strict-mode ambiguity (two matching elements) from the
 * moment that panel exists, breaking this helper for every caller (Scenario 1
 * step 4, Scenario 2 step 2) regardless of whether the edge feature is even
 * touched.
 */
async function saveCertSettings(page: Page): Promise<void> {
  const panel = page.locator('[aria-labelledby="cert-settings-heading"]');
  const [resp] = await Promise.all([
    page.waitForResponse((r) => new URL(r.url()).pathname === "/api/system/settings" && r.request().method() === "PUT"),
    panel.getByRole("button", { name: t.save }).click()
  ]);
  expect(resp.ok(), `expected success saving certificate settings, got ${resp.status()}: ${await resp.text()}`).toBe(true);
}

/**
 * Selects the issuer mode in the Edge-Zertifikat panel's OWN dropdown
 * (`t.settingsCertEdgeIssuerMode`) -- a separate control from the internal
 * `t.settingsCertIssuerMode` selectIssuerMode operates on above, reusing the
 * same two option labels (t.settingsCertIssuerSelfSigned/Acme) since both
 * dropdowns offer the identical two modes.
 */
async function selectEdgeIssuerMode(page: Page, mode: "self_signed" | "acme"): Promise<void> {
  await page.getByRole("combobox", { name: t.settingsCertEdgeIssuerMode, exact: true }).click();
  const label = mode === "self_signed" ? t.settingsCertIssuerSelfSigned : t.settingsCertIssuerAcme;
  await page.getByRole("option", { name: label, exact: true }).click();
}

/**
 * Clicks the Edge-Zertifikat panel's OWN "Speichern" -- the disjoint
 * cert_edge_*-only PUT partition (EdgeCertificatePanel.saveSettings) -- and
 * waits for it to land. Scoped to that panel
 * (`aria-labelledby="cert-edge-heading"`) for the exact same reason
 * saveCertSettings above is scoped to the internal one.
 */
async function saveEdgeCertSettings(page: Page): Promise<void> {
  const panel = page.locator('[aria-labelledby="cert-edge-heading"]');
  const [resp] = await Promise.all([
    page.waitForResponse((r) => new URL(r.url()).pathname === "/api/system/settings" && r.request().method() === "PUT"),
    panel.getByRole("button", { name: t.save }).click()
  ]);
  expect(resp.ok(), `expected success saving edge certificate settings, got ${resp.status()}: ${await resp.text()}`).toBe(true);
}

/**
 * Scenario 7's own locator for the "Öffentliche Domains" panel
 * (`aria-labelledby="cert-public-heading"`, CertificateSettings.tsx) -- needed
 * as a SCOPE, not just for its own Save button: AcmeConfigFields renders an
 * identically-labelled "Eigene ACME-Einstellungen verwenden" switch inside
 * BOTH this panel (prefix="public") and the Edge-Zertifikat panel
 * (prefix="edge") right above it on the same page, so an unscoped
 * `getByRole("switch", { name: t.settingsAcmeOwnSettings })` is a strict-mode
 * ambiguity the moment both panels are rendered -- which they always are here,
 * regardless of whether either "own settings" switch is actually toggled.
 */
function publicCertPanel(page: Page) {
  return page.locator('[aria-labelledby="cert-public-heading"]');
}

/**
 * Selects the issuer mode in the "Öffentliche Domains" panel's OWN dropdown
 * (`t.settingsCertPublicIssuerMode`) -- a THIRD independent combobox from the
 * two `selectIssuerMode`/`selectEdgeIssuerMode` operate on, offering a third,
 * itself-meaningful value ("" -- follow the global/internal issuer mode).
 * `exact: true` matters for the exact reason selectIssuerMode's doc comment
 * explains: t.settingsCertIssuerMode ("Aussteller") is a literal substring of
 * every one of these three combobox names, and CertificateSettings.tsx renders
 * all three unconditionally on the same page.
 */
async function selectPublicIssuerMode(page: Page, mode: "" | "self_signed" | "acme"): Promise<void> {
  await page.getByRole("combobox", { name: t.settingsCertPublicIssuerMode, exact: true }).click();
  const label =
    mode === ""
      ? t.settingsCertPublicIssuerModeFollowGlobal
      : mode === "self_signed"
        ? t.settingsCertIssuerSelfSigned
        : t.settingsCertIssuerAcme;
  await page.getByRole("option", { name: label, exact: true }).click();
}

/**
 * Clicks the "Öffentliche Domains" panel's OWN "Speichern" -- the disjoint
 * cert_public_*-only PUT partition (CertificateSettings.savePublic) -- and
 * waits for it to land. Scoped to that panel for the exact same reason
 * saveCertSettings/saveEdgeCertSettings are scoped to theirs (this panel's
 * button carries the identical "Speichern" label).
 */
async function savePublicCertSettings(page: Page): Promise<void> {
  const [resp] = await Promise.all([
    page.waitForResponse((r) => new URL(r.url()).pathname === "/api/system/settings" && r.request().method() === "PUT"),
    publicCertPanel(page).getByRole("button", { name: t.save }).click()
  ]);
  expect(
    resp.ok(),
    `expected success saving public-domain certificate settings, got ${resp.status()}: ${await resp.text()}`
  ).toBe(true);
}

/** Enables the "Zertifikatsverwaltung aktiv" checkbox in System Settings (the caller must already be on that view) and saves. */
async function enableCertificateModule(page: Page): Promise<void> {
  await page.getByRole("checkbox", { name: t.settingsCertEnabled }).check();
  const [resp] = await Promise.all([
    page.waitForResponse((r) => new URL(r.url()).pathname === "/api/system/settings" && r.request().method() === "PUT"),
    page.getByRole("button", { name: t.save }).click()
  ]);
  expect(resp.ok(), `expected success enabling the certificate module, got ${resp.status()}: ${await resp.text()}`).toBe(true);
}

/** Resolves the seeded default admin-tier group's id ("Standard", migration v44) via the candidates endpoint the elevated system_admin session can always see. */
async function resolveDefaultAdminGroupId(page: Page): Promise<string> {
  const resp = await page.request.get("/api/portal/server-admin-group-candidates");
  expect(resp.ok(), `expected success, got ${resp.status()}: ${await resp.text()}`).toBe(true);
  const body = (await resp.json()) as { data: { id: string; name: string }[] };
  const found = body.data.find((c) => c.name === "Standard");
  expect(found, `expected a "Standard" admin-group candidate in ${JSON.stringify(body.data)}`).toBeTruthy();
  return found!.id;
}

/**
 * Creates an AI-server via the raw portal API. DEVIATION from the brief's
 * literal wording ("request.post mit dem Bootstrap-Token"): the bootstrap
 * API token's scopes are ["gateway:use","admin"] (never "system" — no
 * Bearer/API token can carry it, by design), and the seeded default admin
 * group's owner is NULL with no seeded co-manager row (migration v44), so a
 * bearer-token principal without "system" is not a manager of ANY admin
 * group and CreateServer's admin_group_ids gate (added by the later Phase B
 * admin-group-permissions feature) 403s it outright. The elevated
 * system_admin's own SESSION (page.request, "system" scope) is the
 * established, already-working pattern every sibling suite in this repo
 * uses for the exact same purpose — see
 * e2e-resource-groups/resource-groups.spec.ts's createServerRaw and
 * e2e-servers/servers.spec.ts's createServer. Still genuinely "über die
 * API" (a raw POST, not a filled-out form) — only the credential differs.
 */
async function createServerRaw(page: Page, opts: { name: string; domain: string; adminGroupId: string }): Promise<string> {
  const resp = await page.request.post("/api/portal/servers", {
    headers: CSRF,
    data: { name: opts.name, domain: opts.domain, status: "active", admin_group_ids: [opts.adminGroupId] }
  });
  expect(resp.ok(), `expected success creating ${opts.name}, got ${resp.status()}: ${await resp.text()}`).toBe(true);
  const body = (await resp.json()) as { id: string };
  return body.id;
}

/** Opens the edit-form for the server row named `serverName` (mirrors e2e-servers/servers.spec.ts's openEditServer). */
async function openEditServer(page: Page, serverName: string): Promise<void> {
  const row = page.getByRole("row", { name: new RegExp(escapeRegExp(serverName)) });
  await row.getByRole("button", { name: t.serverActionEdit }).click();
}

/**
 * Opts a server into NetBird via the system-admin-only linkage editor
 * (`SetServerNetbird` — module-INDEPENDENT: it writes `netbird_enabled`
 * unconditionally, only best-effort NetBird API calls are gated on the
 * module being configured, which it deliberately is not in this suite). The
 * caller must already have the server's edit form open.
 */
async function enableServerNetbirdLink(page: Page, serverId: string): Promise<void> {
  await page.getByRole("checkbox", { name: t.serverNetbirdEnabledLabel }).check();
  const [resp] = await Promise.all([
    page.waitForResponse((r) => new URL(r.url()).pathname === `/api/system/servers/${serverId}/netbird` && r.request().method() === "PUT"),
    page.getByRole("button", { name: t.serverNetbirdLinkSave }).click()
  ]);
  expect(resp.ok(), `expected success linking netbird for ${serverId}, got ${resp.status()}: ${await resp.text()}`).toBe(true);
  await expect(page.getByText(t.serverNetbirdLinkSaved)).toBeVisible();
}

/**
 * Opts a server into certificate management — saved IMMEDIATELY on toggle
 * (its own dedicated endpoint, no separate Save button). Only rendered once
 * `netbird_enabled` is true AND the cert module's server scope is
 * "selected" (see CertificateSettings/ServerList wiring) — this suite always
 * runs in "selected" scope, so it is always the include (opt-in) checkbox.
 */
async function optInServerCertificate(page: Page, serverId: string): Promise<void> {
  const [resp] = await Promise.all([
    page.waitForResponse((r) => new URL(r.url()).pathname === `/api/portal/servers/${serverId}/certificate` && r.request().method() === "PUT"),
    page.getByRole("checkbox", { name: t.serverCertificateInclude }).check()
  ]);
  expect(resp.ok(), `expected success opting ${serverId} into certificate management, got ${resp.status()}: ${await resp.text()}`).toBe(true);
}

/**
 * Mints the server's reporting-agent token via the SAME portal route the
 * AgentTokenSection UI uses (`POST /api/portal/servers/{id}/agent-token`, see
 * handlePortalServerAgentToken / api.generateAgentToken).
 *
 * This is a PRECONDITION for certificate management since Phase 2, not
 * incidental setup: a kind=server name only enters the reconcile's desired set
 * when the server has an agent token, because that token is the only thing that
 * can authenticate against GET /api/agent/v1/certificate -- without it there is
 * no distribution path and the pass deliberately places no order at all (see
 * portal.Service.serverHasAgentToken).
 */
async function mintAgentToken(page: Page, serverId: string): Promise<string> {
  const resp = await page.request.post(`/api/portal/servers/${serverId}/agent-token`, { headers: CSRF });
  expect(resp.ok(), `expected success minting an agent token for ${serverId}, got ${resp.status()}: ${await resp.text()}`).toBe(true);
  const body = (await resp.json()) as { secret?: string };
  expect(body.secret, "the one-time agent-token secret must come back on creation").toBeTruthy();
  return body.secret!;
}

async function fetchCertificates(page: Page): Promise<CertRow[]> {
  const resp = await page.request.get("/api/system/certificates");
  expect(resp.ok(), `expected success, got ${resp.status()}: ${await resp.text()}`).toBe(true);
  const body = (await resp.json()) as { data: CertRow[] };
  return body.data;
}

async function fetchCertificateByDomain(page: Page, domain: string): Promise<CertRow | undefined> {
  return (await fetchCertificates(page)).find((r) => r.domain === domain);
}

async function fetchCA(page: Page): Promise<{ ca: CertCA; bundle_pem: string }> {
  const resp = await page.request.get("/api/system/certificates/ca");
  expect(resp.ok(), `expected success, got ${resp.status()}: ${await resp.text()}`).toBe(true);
  return (await resp.json()) as { ca: CertCA; bundle_pem: string };
}

/**
 * Waits (up to timeoutMs) for `domain`'s row to render as "aktiv" in the
 * REAL UI table — the brief's own framing for the reconcile-pass poll ("die
 * Zeile … steht auf aktiv"). Every attempt does a genuine page.reload() +
 * re-navigate into the Zertifikate view, so each check is a fresh fetch —
 * CertificateSettings only fetches once per mount (no live poll of its own).
 */
async function waitForRowActiveInUI(page: Page, domain: string, timeoutMs: number): Promise<void> {
  await expect(async () => {
    await page.reload();
    await gotoCertificates(page);
    const row = page.getByRole("table").getByRole("row", { name: new RegExp(escapeRegExp(domain)) });
    await expect(row).toBeVisible({ timeout: 3000 });
    await expect(row.getByText(t.certificatesStatusActive, { exact: true })).toBeVisible({ timeout: 3000 });
  }).toPass({ timeout: timeoutMs, intervals: [3000] });
}

/** Reads the `not_after` column's rendered cell text for `domain`'s row (must already be on the Zertifikate view). Column order: domain, kind, status, issued_at, not_after, remaining, last_error. */
async function readNotAfterCellText(page: Page, domain: string): Promise<string> {
  const row = page.getByRole("table").getByRole("row", { name: new RegExp(escapeRegExp(domain)) });
  return (await row.locator("td").nth(4).innerText()).trim();
}

/** Reads the `data-testid="cert-remaining-<domain>"` badge (exact days + severity) for `domain`'s row. Must already be on the Zertifikate view. */
async function readRemainingDays(page: Page, domain: string): Promise<{ days: number; severity: string | null }> {
  const badge = page.locator(`[data-testid="cert-remaining-${domain}"]`);
  await expect(badge).toBeVisible();
  const text = (await badge.innerText()).trim();
  return { days: Number(text), severity: await badge.getAttribute("data-severity") };
}

/**
 * Parses the internal-CA panel's label/value Typography pairs from its
 * rendered innerText (a label line immediately followed by its value line —
 * see CertificateSettings.tsx's `<Box><Typography>label</Typography>
 * <Typography>value</Typography></Box>` layout). Scoped via
 * `aria-labelledby="cert-ca-heading"` (the Panel's own wiring), which
 * disambiguates it from the identically-worded "Zertifikate"/"Erstellt"/
 * "Läuft ab"/"Restlaufzeit (Tage)" strings that ALSO appear as the leaf
 * table's own headings/columns elsewhere on the same page.
 */
async function readCAPanel(page: Page): Promise<{ subject: string; remainingDays: number; previousLine: string | undefined }> {
  const panel = page.locator('[aria-labelledby="cert-ca-heading"]');
  await expect(panel).toBeVisible();
  const text = await panel.innerText();
  const lines = text
    .split("\n")
    .map((l) => l.trim())
    .filter(Boolean);
  function valueAfter(label: string): string {
    const idx = lines.indexOf(label);
    if (idx === -1 || idx + 1 >= lines.length) {
      throw new Error(`CA panel: label "${label}" not found in:\n${text}`);
    }
    return lines[idx + 1];
  }
  return {
    subject: valueAfter(t.certificatesCaSubject),
    remainingDays: Number(valueAfter(t.certificatesColRemaining)),
    previousLine: lines.find((l) => l.startsWith(t.certificatesCaPrevious))
  };
}

/** Resolves an already-created AI-server's id by its display name (Scenario 5 needs Scenario 1's server, whose id it never saw). */
async function resolveServerIdByName(page: Page, name: string): Promise<string> {
  const resp = await page.request.get("/api/portal/servers");
  expect(resp.ok(), `expected success, got ${resp.status()}: ${await resp.text()}`).toBe(true);
  const body = (await resp.json()) as { data: { id: string; name: string }[] };
  const found = body.data.find((s) => s.name === name);
  expect(found, `expected a server named ${name} in ${JSON.stringify(body.data.map((s) => s.name))}`).toBeTruthy();
  return found!.id;
}

/**
 * The leaf fingerprint of a PEM chain, computed EXACTLY like both sides of the
 * feature compute it -- sha256 over the first certificate's DER, lowercase hex
 * (gateway: certissue.FingerprintPEM; agent: certinstall.fingerprintDER). Recomputing
 * it here rather than trusting either implementation is what makes "the file on disk
 * carries the certificate the gateway just issued" a real cross-check.
 */
function leafFingerprintFromPEM(pemText: string): string {
  const match = pemText.match(/-----BEGIN CERTIFICATE-----([\s\S]*?)-----END CERTIFICATE-----/);
  if (!match) return "";
  const der = Buffer.from(match[1].replace(/\s+/g, ""), "base64");
  return crypto.createHash("sha256").update(der).digest("hex");
}

/**
 * Waits for the "Installiert" column to render ✓ (`data-state="yes"`) for
 * `domain` in the REAL UI table. A poll, not a single read, for the same reason
 * waitForRowActiveInUI is one: the column is fed by the agent's telemetry report,
 * which lands asynchronously after the install, and CertificateSettings fetches
 * once per mount.
 */
async function waitForInstalledYesInUI(page: Page, domain: string, timeoutMs: number): Promise<void> {
  await expect(async () => {
    await page.reload();
    await gotoCertificates(page);
    const badge = page.locator(`[data-testid="cert-installed-${domain}"]`);
    await expect(badge).toBeVisible({ timeout: 3000 });
    await expect(badge).toHaveAttribute("data-state", "yes", { timeout: 3000 });
  }).toPass({ timeout: timeoutMs, intervals: [3000] });
}

test.describe.configure({ mode: "serial" });

test("Szenario 1 — interne CA (self_signed): Modul-Flag, Laufzeit, CA-Panel, CA-Rotation", async ({ page }) => {
  test.setTimeout(600000);
  // Step 1: login + elevate.
  await login(page, CERTS_ADMIN_EMAIL, CERTS_ADMIN_PASSWORD);
  await enterSystemAdminMode(page, CERTS_ADMIN_NAME, CERTS_ADMIN_PASSWORD);

  // Step 2+3: the "Zertifikate" nav item is absent until the module is on —
  // proves the module flag + the reachable-before-configured design.
  await page.getByRole("link", { name: t.system, exact: true }).click();
  await expect(page.getByRole("link", { name: t.settingsCertificatesTitle, exact: true })).toHaveCount(0);
  await enableCertificateModule(page);
  await expect(page.getByRole("link", { name: t.settingsCertificatesTitle, exact: true })).toBeVisible();

  // Step 4: configure the internal-CA issuer + shared config; the ACME
  // fields must not be visible in this mode.
  await gotoCertificates(page);
  await selectIssuerMode(page, "self_signed");
  await expect(page.locator("#cert-acme-email")).toHaveCount(0);
  await expect(page.getByRole("combobox", { name: t.settingsAcmeDirectory })).toHaveCount(0);
  await page.locator("#cert-self-signed-validity").fill("90");
  await page.locator("#cert-base-domain").fill(BASE_DOMAIN);
  await selectServerScope(page, "selected");
  await saveCertSettings(page);

  // Step 5: create the server via the raw API, then opt it into NetBird +
  // certificate management through the real UI.
  const adminGroupId = await resolveDefaultAdminGroupId(page);
  const serverAId = await createServerRaw(page, { name: SERVER_A_NAME, domain: SERVER_A_DOMAIN, adminGroupId });
  await page.getByRole("link", { name: t.servers, exact: true }).click();
  await expect(page.getByRole("row", { name: new RegExp(escapeRegExp(SERVER_A_NAME)) })).toBeVisible();
  await openEditServer(page, SERVER_A_NAME);
  await enableServerNetbirdLink(page, serverAId);
  await optInServerCertificate(page, serverAId);
  // A reporting-agent token is a PRECONDITION for the reconcile to want this
  // server at all (Phase 2, see mintAgentToken) -- without it no order is placed.
  await mintAgentToken(page, serverAId);

  // Step 6: wait for the reconcile pass (up to 90s), assert "aktiv" +
  // remaining validity in [85, 90] days (proves the configured 90-day
  // lifetime), and that the expiry cell carries a time component.
  await waitForRowActiveInUI(page, SERVER_A_DOMAIN, 90000);
  const remainingBefore = await readRemainingDays(page, SERVER_A_DOMAIN);
  expect(remainingBefore.days).toBeGreaterThanOrEqual(85);
  expect(remainingBefore.days).toBeLessThanOrEqual(90);
  expect(remainingBefore.severity).toBe("ok");
  const notAfterCellText = await readNotAfterCellText(page, SERVER_A_DOMAIN);
  expect(notAfterCellText).not.toBe("");
  expect(notAfterCellText).toMatch(/:/); // a locale date-time string always carries a colon

  // Step 7: the CA panel shows a subject + a remaining validity > 3000 days;
  // the CA bundle endpoint carries a public certificate and never a key.
  const caPanel = await readCAPanel(page);
  expect(caPanel.subject).not.toBe("");
  expect(caPanel.remainingDays).toBeGreaterThan(3000);
  const caBefore = await fetchCA(page);
  expect(caBefore.ca.present).toBe(true);
  expect(caBefore.ca.fingerprint).toBeTruthy();
  expect(caBefore.bundle_pem).toContain("BEGIN CERTIFICATE");
  expect(caBefore.bundle_pem).not.toContain("PRIVATE KEY");

  const certBeforeRotate = await fetchCertificateByDomain(page, SERVER_A_DOMAIN);
  expect(certBeforeRotate?.status).toBe("active");
  const notAfterBeforeRotate = certBeforeRotate!.not_after;

  // Step 8: rotate the CA (a synchronous action — the new root + the
  // previous-root line show up immediately, no reconcile pass needed for
  // the CA itself).
  await page.getByRole("button", { name: t.certificatesCaRotate }).click();
  const rotateDialog = page.getByRole("dialog");
  await rotateDialog.getByRole("button", { name: t.confirm }).click();
  await expect(rotateDialog).toHaveCount(0);
  await expect(page.getByText(t.systemSaved)).toBeVisible();

  // The CA panel doesn't render the CURRENT fingerprint as text (only the
  // PREVIOUS one, in the "Vorheriger Root" line) — read the current one via
  // the same API the panel itself is backed by (api.certificateCA()).
  const caAfter = await fetchCA(page);
  expect(caAfter.ca.fingerprint).toBeTruthy();
  expect(caAfter.ca.fingerprint).not.toBe(caBefore.ca.fingerprint);
  expect(caAfter.ca.previous_fingerprint).toBe(caBefore.ca.fingerprint);
  const caPanelAfterRotate = await readCAPanel(page);
  expect(caPanelAfterRotate.previousLine).toBeTruthy();
  expect(caPanelAfterRotate.previousLine).toContain(caBefore.ca.fingerprint!);

  // Wait for the NEXT reconcile pass to re-issue a.int.example.test's leaf
  // under the new root (the issuer-fingerprint-mismatch rule in
  // service_certificates.go's renewDue) — no manual "renew now" needed.
  await expect(async () => {
    const cert = await fetchCertificateByDomain(page, SERVER_A_DOMAIN);
    expect(cert, `no certificate row for ${SERVER_A_DOMAIN} yet`).toBeTruthy();
    expect(cert!.status).toBe("active");
    expect(cert!.not_after).not.toBe(notAfterBeforeRotate);
  }).toPass({ timeout: 90000, intervals: [3000] });

  // Final UI sanity: the row still reads "aktiv" with a fresh expiry after the re-issue.
  await waitForRowActiveInUI(page, SERVER_A_DOMAIN, 20000);
});

test("Szenario 2 — Wechsel auf Let's Encrypt gegen den Fake-Server, ohne Abriss", async ({ page }) => {
  test.setTimeout(900000);
  await login(page, CERTS_ADMIN_EMAIL, CERTS_ADMIN_PASSWORD);
  await enterSystemAdminMode(page, CERTS_ADMIN_NAME, CERTS_ADMIN_PASSWORD);

  // Step 1: before the switch, create a SECOND server whose domain lies
  // OUTSIDE the base domain, opt it in, and let it get an internal-CA
  // certificate too (the module/issuer configuration from Scenario 1 is
  // still live on this same gateway process). Record both rows' expiry.
  const adminGroupId = await resolveDefaultAdminGroupId(page);
  const serverBId = await createServerRaw(page, { name: SERVER_B_NAME, domain: SERVER_B_DOMAIN, adminGroupId });
  await page.getByRole("link", { name: t.servers, exact: true }).click();
  await expect(page.getByRole("row", { name: new RegExp(escapeRegExp(SERVER_B_NAME)) })).toBeVisible();
  await openEditServer(page, SERVER_B_NAME);
  await enableServerNetbirdLink(page, serverBId);
  await optInServerCertificate(page, serverBId);
  // A reporting-agent token is a PRECONDITION for the reconcile to want this
  // server at all (Phase 2, see mintAgentToken) -- without it no order is placed.
  await mintAgentToken(page, serverBId);
  await waitForRowActiveInUI(page, SERVER_B_DOMAIN, 90000);

  const beforeA = await fetchCertificateByDomain(page, SERVER_A_DOMAIN);
  const beforeB = await fetchCertificateByDomain(page, SERVER_B_DOMAIN);
  expect(beforeA?.status, "server A must still be active from Scenario 1").toBe("active");
  expect(beforeB?.status).toBe("active");
  const notAfterA0 = beforeA!.not_after;
  const notAfterB0 = beforeB!.not_after;

  // Step 2: switch the issuer to Let's Encrypt, pointed at the fake ACME
  // directory. The ACME fields appear, the self-signed fields disappear,
  // and the CA panel stays visible (the root is still needed by the
  // existing self_signed leaves).
  await gotoCertificates(page);
  await selectIssuerMode(page, "acme");
  await expect(page.locator("#cert-self-signed-validity")).toHaveCount(0);
  await expect(page.locator("#cert-ca-renew-before-days")).toHaveCount(0);
  await page.getByLabel(t.settingsAcmeEmail).fill("ops@example.test");
  await page.getByRole("combobox", { name: t.settingsAcmeDirectory }).click();
  await page.getByRole("option", { name: t.settingsAcmeDirectoryCustom, exact: true }).click();
  await page.getByLabel(t.settingsAcmeDirectoryCustom).fill(FAKEACME_DIRECTORY_URL);
  await saveCertSettings(page);

  await expect(page.getByLabel(t.settingsAcmeEmail)).toBeVisible();
  await expect(page.getByRole("combobox", { name: t.settingsAcmeDirectory })).toBeVisible();
  await expect(page.locator("#cert-self-signed-validity")).toHaveCount(0);
  await expect(page.locator('[aria-labelledby="cert-ca-heading"]')).toBeVisible();
  await expect(page.getByText(t.certificatesCaStillNeeded)).toBeVisible();

  // Step 3 (the core of this scenario): wait at least TWO reconcile
  // intervals (>= 120s at the 60s floor) and confirm NOTHING changed. This
  // is a deliberate real-time wait, not a condition-based poll — proving the
  // ABSENCE of a change over wall-clock time is exactly what a poll cannot
  // do (a poll only proves a condition eventually becomes true).
  await page.waitForTimeout(130000);

  const afterWaitA = await fetchCertificateByDomain(page, SERVER_A_DOMAIN);
  const afterWaitB = await fetchCertificateByDomain(page, SERVER_B_DOMAIN);
  expect(afterWaitA?.not_after, "a.int.example.test's expiry must survive the mode switch unchanged").toBe(notAfterA0);
  expect(afterWaitB?.not_after, "b.other.example.net's expiry must survive the mode switch unchanged").toBe(notAfterB0);
  expect(afterWaitB?.status, "b.other.example.net must stay active, not skipped").toBe("active");
  expect(afterWaitB?.last_error ?? "").toContain("base domain");

  // UI sanity: b.other.example.net reads "aktiv" (not "übersprungen"), and
  // now carries a reason in the Fehler-Spalte.
  await waitForRowActiveInUI(page, SERVER_B_DOMAIN, 20000);
  const rowB = page.getByRole("table").getByRole("row", { name: new RegExp(escapeRegExp(SERVER_B_DOMAIN)) });
  await expect(rowB).toContainText(/base domain/);

  // Step 4: force an immediate re-issue of everything. a.int.example.test
  // (under the base domain) is really re-ordered against the fake ACME;
  // b.other.example.net (HTTP-01 cannot serve that name) keeps its old,
  // still-valid material instead of being dropped.
  await page.getByRole("button", { name: t.certificatesReissueAll }).click();
  const reissueDialog = page.getByRole("dialog");
  await reissueDialog.getByRole("button", { name: t.confirm }).click();
  await expect(reissueDialog).toHaveCount(0);

  await expect(async () => {
    const a = await fetchCertificateByDomain(page, SERVER_A_DOMAIN);
    expect(a, `no certificate row for ${SERVER_A_DOMAIN} yet`).toBeTruthy();
    expect(a!.status).toBe("active");
    expect(a!.not_after).not.toBe(notAfterA0);
  }).toPass({ timeout: 90000, intervals: [3000] });

  const bAfterReissue = await fetchCertificateByDomain(page, SERVER_B_DOMAIN);
  expect(bAfterReissue?.status).toBe("active");
  expect(bAfterReissue?.not_after, "b.other.example.net must keep its OLD expiry").toBe(notAfterB0);

  await waitForRowActiveInUI(page, SERVER_A_DOMAIN, 20000);

  // Step 5: "Jetzt erneuern" on the out-of-base-domain (ACME-ineligible) row
  // — no error, status stays valid. Waits on the actual network completion of
  // the renew call (not a fixed sleep) so the "no error" check cannot pass
  // merely because it ran before the click's round trip finished.
  const [renewResp] = await Promise.all([
    page.waitForResponse((r) => new URL(r.url()).pathname === "/api/system/certificates/renew" && r.request().method() === "POST"),
    rowB.getByRole("button", { name: t.certificatesRenewNow }).click()
  ]);
  expect(renewResp.ok(), `expected success renewing ${SERVER_B_DOMAIN}, got ${renewResp.status()}: ${await renewResp.text()}`).toBe(true);
  await expect(page.locator(".MuiAlert-filledError")).toHaveCount(0);

  await expect(async () => {
    const b = await fetchCertificateByDomain(page, SERVER_B_DOMAIN);
    expect(b, `no certificate row for ${SERVER_B_DOMAIN} yet`).toBeTruthy();
    expect(b!.status).toBe("active");
    expect(b!.not_after).toBe(notAfterB0);
  }).toPass({ timeout: 90000, intervals: [3000] });
});

test("Szenario 3 — Edge-Zertifikat (vorgeschalteter Reverse-Proxy): eigener Modus, Mehrnamen inkl. IP, Key-Gate, Unabhängigkeit vom internen Zertifikat", async ({
  page
}) => {
  test.setTimeout(300000);
  await login(page, CERTS_ADMIN_EMAIL, CERTS_ADMIN_PASSWORD);
  await enterSystemAdminMode(page, CERTS_ADMIN_NAME, CERTS_ADMIN_PASSWORD);

  // Baseline: server A's INTERNAL certificate (issued under acme in Scenario 2,
  // carried over unchanged on this same gateway process/sqlite file) must
  // still be active -- its not_after is the value this whole scenario proves
  // an EDGE issuer-mode switch never touches.
  const beforeInternal = await fetchCertificateByDomain(page, SERVER_A_DOMAIN);
  expect(beforeInternal?.status, "server A must still be active from the earlier scenarios").toBe("active");
  const internalNotAfterBefore = beforeInternal!.not_after;

  // Step 1: enable the edge (gateway-nginx) certificate in its OWN self_signed
  // mode, with TWO names -- a DNS name plus a bare IP address. The internal
  // CA can sign an IP SAN with no validation problem at all (unlike ACME,
  // which cannot -- see Step 5), so self_signed is the mode that actually
  // exercises the multi-name/IP-SAN issuance (certissue T1) end to end.
  await gotoCertificates(page);
  await selectEdgeIssuerMode(page, "self_signed");
  await page.getByLabel(t.settingsCertEdgeNames).fill(`${EDGE_HOST_NAME}, ${EDGE_IP}`);
  // EdgeCertificatePanel uses an MUI <Switch> (ARIA role="switch"), not a
  // <Checkbox> (role="checkbox") like the module-enable/server-linkage
  // checkboxes elsewhere in this suite -- confirmed against the real DOM
  // (mirrors resource-groups.spec.ts's own getByRole("switch", ...) usage).
  await page.getByRole("switch", { name: t.settingsCertEdgeEnabled }).click();
  await saveEdgeCertSettings(page);

  // Step 2: a kind=edge row appears in the SAME leaf table Scenario 1/2 use
  // for the internal certificates -- active, with a remaining-validity figure.
  await waitForRowActiveInUI(page, EDGE_HOST_NAME, 90000);
  const edgeAfterIssue = await fetchCertificateByDomain(page, EDGE_HOST_NAME);
  expect(edgeAfterIssue?.kind).toBe("edge");
  expect(edgeAfterIssue?.status).toBe("active");
  const edgeRemaining = await readRemainingDays(page, EDGE_HOST_NAME);
  expect(edgeRemaining.days).toBeGreaterThan(0);
  // The FINGERPRINT is the discriminator for "a fresh certificate was really
  // ordered" below: `not_after` is only second-resolution, and the self_signed
  // issuance and the acme re-issuance can legitimately land in the SAME second
  // (the settings PUT triggers a reconcile pass immediately, so the two are
  // often less than a second apart), which made the old not_after comparison
  // pass or fail on sub-second luck. A re-issue always produces a new key and
  // serial, so a differing fingerprint is both strictly stronger and stable.
  const edgeFingerprintSelfSigned = edgeAfterIssue!.fingerprint;
  expect(edgeFingerprintSelfSigned, "the self_signed edge row must carry a fingerprint").toBeTruthy();

  // Step 3: the bundle download carries the certificate's PUBLIC chain.
  const bundleResp = await page.request.get("/api/system/certificates/edge/bundle");
  expect(bundleResp.ok(), `expected success, got ${bundleResp.status()}: ${await bundleResp.text()}`).toBe(true);
  expect(await bundleResp.text()).toContain("BEGIN CERTIFICATE");

  // Step 4: the private-key download is REFUSED with 409 -- the test gateway
  // runs with OP_AI_GATEWAY_CERT_EDGE_OUTPUT_DIR set (see
  // playwright.certificates.config.ts), so the gateway can (and does) deliver
  // the key to its own nginx directly, and it must never additionally leave
  // the process over HTTP.
  const keyResp = await page.request.get("/api/system/certificates/edge/key");
  expect(keyResp.status()).toBe(409);
  const keyBody = (await keyResp.json()) as { error?: { code?: string } };
  expect(keyBody.error?.code).toBe("certificate.edge_key_managed");

  // Step 5 -- the point of the whole feature: switch the EDGE issuer mode
  // (self_signed -> acme), independently of the internal issuer mode.
  //
  // ACME cannot issue for a bare IP address, and the backend validates this
  // at WRITE time (a PUT that would leave cert_edge_issuer_mode=acme with an
  // IP still in cert_edge_names is rejected with cert.invalid) -- so the IP
  // is dropped from the name list in the SAME save, exactly what a real
  // operator moving the edge certificate to Let's Encrypt would have to do.
  // Dropping a name changes the configured SAN set, which the reconcile's
  // sanDrift check picks up on the very next pass regardless of the stored
  // certificate's remaining validity -- so this is a REAL mode transition
  // (a fresh certificate actually gets ordered against the fake ACME server),
  // not an inert settings write, which makes the independence proof below
  // meaningful rather than trivial.
  await gotoCertificates(page);
  await page.getByLabel(t.settingsCertEdgeNames).fill(EDGE_HOST_NAME);
  await selectEdgeIssuerMode(page, "acme");
  await saveEdgeCertSettings(page);

  // Wait out at least two real reconcile intervals (mirrors Scenario 2's own
  // proof technique exactly): proving the ABSENCE of a change over wall-clock
  // time is exactly what a condition-based poll cannot do.
  await page.waitForTimeout(130000);

  const edgeAfterSwitch = await fetchCertificateByDomain(page, EDGE_HOST_NAME);
  expect(edgeAfterSwitch?.status, "the edge certificate must be re-issued successfully under acme").toBe("active");
  expect(
    edgeAfterSwitch?.fingerprint,
    "the edge switch to acme must really have happened (a fresh certificate, not the earlier self_signed one)"
  ).not.toBe(edgeFingerprintSelfSigned);
  expect(edgeAfterSwitch?.not_after, "the re-issued edge certificate still has an expiry").toBeTruthy();

  const afterInternal = await fetchCertificateByDomain(page, SERVER_A_DOMAIN);
  expect(afterInternal?.status, "the internal certificate must stay active").toBe("active");
  expect(
    afterInternal?.not_after,
    "the internal certificate's expiry must be completely unchanged by an EDGE issuer-mode switch"
  ).toBe(internalNotAfterBefore);
});

// Scenario 4 (Plan B, the plaintext-refusal gate cert_edge_require_https):
// arming without an observation refuses with 400; making one observation makes
// arming succeed; a plaintext API call is then refused with 403; the four
// always-open paths (the ACME challenge route, /healthz, the agent route, and
// the internal/loopback path) stay reachable throughout. Driven entirely via
// `page.request` (raw API calls with custom headers) rather than the portal
// UI: a browser navigation that gets 307-redirected mid-flow would be its own
// complication, and every fact this scenario needs is directly observable at
// the HTTP layer.
//
// This scenario needs a mechanism the first three don't: OP_AI_GATEWAY_CERT_EDGE_GATE_TEST_REMOTE_ADDR
// (set on the gateway webServer in playwright.certificates.config.ts) makes the
// gateway's listener report a FIXED, non-loopback fake address for every
// accepted connection. Without it, this whole scenario would be unwritable --
// this e2e stack has NO nginx in front (the frontend's vite preview proxies
// /api directly to the gateway binary), so every real connection between the
// test client and the gateway is genuinely loopback, and
// internal/gateway/edge_scheme.go PERMANENTLY exempts loopback callers (the
// gateway's own background chat runs depend on exactly that exemption) --
// both the arming precondition's observation AND the gate's own refusal read
// the identical RemoteAddr and would both silently no-op. See
// cmd/gateway/main.go's certEdgeGateTestRemoteAddrEnv doc comment for the full
// story; it does not touch (and is never active outside of) this one env var.
test("Szenario 4 — Klartext-Riegel (cert_edge_require_https): Armieren ohne Beobachtung -> 400, danach armierbar, ein Klartext-Aufruf -> 403, die vier offenen Pfade bleiben erreichbar", async ({
  page
}) => {
  await login(page, CERTS_ADMIN_EMAIL, CERTS_ADMIN_PASSWORD);
  await enterSystemAdminMode(page, CERTS_ADMIN_NAME, CERTS_ADMIN_PASSWORD);

  // Step 1: arming with NO encrypted observation yet -> 400
  // certificate.edge_https_not_observed. Nothing in Scenarios 1-3 ever sends
  // the encrypted-hop header, so the tracker is still empty at this point.
  //
  // NOTE (non-idempotent within one gateway process): the observation tracker
  // this step depends on being empty is process-wide, and step 2 below makes
  // it non-empty for 5 minutes. A full `npm run e2e:certificates` run always
  // starts a fresh gateway process, so this is unaffected in normal use -- but
  // retrying just this test (or a repeated `--grep "Szenario 4"` invocation)
  // against an ALREADY-RUNNING server within 5 minutes of a prior pass will
  // find the tracker still warm from that prior run's step 2, and this
  // assertion will fail. There is no reset endpoint; a genuinely fresh
  // gateway process is the only way to re-run this step in isolation.
  const armWithoutObservation = await page.request.put("/api/system/settings", {
    headers: CSRF,
    data: { cert_edge_require_https: true }
  });
  expect(
    armWithoutObservation.status(),
    `expected 400, got ${armWithoutObservation.status()}: ${await armWithoutObservation.text()}`
  ).toBe(400);
  expect((await armWithoutObservation.json()).error.code).toBe("certificate.edge_https_not_observed");

  // Step 2: make an observation. Setting X-OP-Edge-Scheme:https here simulates
  // "the fronting reverse proxy terminated TLS on this hop" -- EXACTLY what a
  // real external client behind a real nginx can never do: nginx sets this
  // header itself from $scheme in every one of its header-setting blocks,
  // unconditionally overwriting any client-supplied value (see
  // internal/gateway/edge_scheme.go's hopEncrypted doc comment; the nginx
  // config test in gateway/deploy pins that nginx behavior). It only works
  // here because this e2e stack has no nginx in the path at all -- the test
  // client talks to the gateway directly.
  const observe = await page.request.get("/api/system/certificates/edge", {
    headers: { "X-OP-Edge-Scheme": "https" }
  });
  expect(observe.ok(), `expected success, got ${observe.status()}: ${await observe.text()}`).toBe(true);
  const observeBody = await observe.json();
  expect(observeBody.https_observed, "the observation-registering GET must see its own hop reflected back").toBe(
    true
  );
  expect(observeBody.last_encrypted_at).toBeTruthy();

  // Step 2b: the observation alone is NOT enough -- the arming request's own hop
  // must be encrypted too. Condition 1 is satisfiable by somebody else's traffic
  // (exactly what step 2 just simulated), so without this an operator whose own
  // route is plaintext could arm the gate against a stranger's TLS and be 403'd by
  // their very next request (runbook 8.2a). The refusal carries its OWN code:
  // reporting "not observed" here would flatly contradict the fresh
  // last_encrypted_at the panel is showing.
  const armFromPlainHop = await page.request.put("/api/system/settings", {
    headers: CSRF,
    data: { cert_edge_require_https: true }
  });
  expect(
    armFromPlainHop.status(),
    `expected 400, got ${armFromPlainHop.status()}: ${await armFromPlainHop.text()}`
  ).toBe(400);
  expect((await armFromPlainHop.json()).error.code).toBe("certificate.edge_arm_requires_https");

  // Arming now succeeds. The X-OP-Edge-Scheme header on THIS call IS load-bearing:
  // it is arming condition 2 (see step 2b) -- dropping it turns this into the 400
  // above.
  const arm = await page.request.put("/api/system/settings", {
    headers: { ...CSRF, "X-OP-Edge-Scheme": "https" },
    data: { cert_edge_require_https: true }
  });
  expect(arm.ok(), `expected success arming, got ${arm.status()}: ${await arm.text()}`).toBe(true);
  expect((await arm.json()).cert_edge_require_https).toBe(true);

  // Step 3: a plaintext (no X-OP-Edge-Scheme) API call is now refused. PUT is
  // not GET/HEAD, so this is the hard 403, not a redirect -- an empty body
  // ({}) makes this a genuine no-op PUT even in the hypothetical case the gate
  // failed to refuse it.
  const plainApiCall = await page.request.put("/api/system/settings", {
    headers: CSRF,
    data: {}
  });
  expect(
    plainApiCall.status(),
    `expected 403, got ${plainApiCall.status()}: ${await plainApiCall.text()}`
  ).toBe(403);
  expect((await plainApiCall.json()).error.code).toBe("certificate.https_required");

  // The SAME gate answers a plaintext GET differently: a 307 redirect to the
  // https form of the same URL (a browser navigating the portal recovers on
  // its own), never a bare refusal.
  const plainGet = await page.request.get("/api/portal/me", { maxRedirects: 0 });
  expect(plainGet.status()).toBe(307);
  expect(plainGet.headers()["location"]).toMatch(/^https:\/\//);

  // Step 4: the four always-open paths stay reachable in the clear, even now
  // that the gate is armed and has just proven it refuses everything else.
  const healthz = await page.request.get("/healthz");
  expect(healthz.ok(), `expected /healthz to stay reachable, got ${healthz.status()}`).toBe(true);

  const acmeChallenge = await page.request.get("/.well-known/acme-challenge/does-not-exist");
  // A public-mux 404 (unknown token, the route's own decoy-pinned behavior) --
  // NOT the gate's 403/307 -- proves this request reached the ACME handler
  // rather than being intercepted.
  expect(acmeChallenge.status()).toBe(404);

  const agentRoute = await page.request.post("/api/agent/v1/telemetry");
  // No bearer token -> the agent route's OWN 401 (auth.invalid_token), not the
  // gate's refusal -- proves this request reached handleAgentTelemetry too.
  expect(agentRoute.status()).toBe(401);

  // Disarm, so this suite's shared gateway process is left in a clean,
  // unarmed state. The X-OP-Edge-Scheme header on THIS call IS load-bearing:
  // the gate is still armed at this point (we have not disarmed yet) and this
  // PUT is not GET/HEAD, so a plaintext version of this exact call would
  // itself be hard-refused with 403 certificate.https_required -- the same
  // fate as step 3's plainApiCall above. This call is, in fact, the test's
  // only live proof that a genuinely encrypted hop is still served normally
  // while the gate is armed (the mirror image of step 3, which proves a
  // plaintext hop is refused while armed).
  const disarm = await page.request.put("/api/system/settings", {
    headers: { ...CSRF, "X-OP-Edge-Scheme": "https" },
    data: { cert_edge_require_https: false }
  });
  expect(disarm.ok(), `expected success disarming, got ${disarm.status()}: ${await disarm.text()}`).toBe(true);
  expect((await disarm.json()).cert_edge_require_https).toBe(false);
});

// Scenario 5 (Phase 2 -- the distribution itself): the REAL server-agent binary,
// with the REAL agent token of a REAL server, fetches its REAL certificate from
// GET /api/agent/v1/certificate and installs it on disk; the gateway shows it as
// installed; a re-issue reaches the agent through the WebSocket doorbell; and one
// token can only ever fetch its own server's material.
//
// Its own describe with DESCRIBE-SCOPED beforeAll/afterAll: the pattern this is
// modelled on (e2e-agent/agent.spec.ts) is file-wide, which here would build and
// spawn an agent around Scenarios 1-4 as well. The agent binary is built into
// this scenario's own mkdtemp directory rather than a fixed /tmp path, so it can
// never collide with e2e-agent's own build.
test.describe("Szenario 5 — der echte ServerAgent installiert ein echtes Zertifikat", () => {
  // The five files an install writes (privkey.pem last, 0600; the rest 0644).
  // The sidecar `.op-cert-etag` is deliberately not listed -- it is the agent's
  // own conditional-GET memo, not part of the delivered material.
  const CERT_FILES = ["fullchain.pem", "cert.pem", "chain.pem", "ca.pem", "privkey.pem"] as const;

  let workDir = "";
  let certDir = "";
  let agentBin = "";
  let agent: ChildProcess | undefined;

  test.beforeAll(() => {
    workDir = fs.mkdtempSync(path.join(os.tmpdir(), "op-cert-agent-e2e-"));
    certDir = path.join(workDir, "certs");
    fs.mkdirSync(certDir, { recursive: true });
    agentBin = path.join(workDir, "server-agent");
    execSync(`go build -o ${agentBin} .`, { cwd: "../../server-agent", stdio: "inherit" });
  });

  test.afterAll(() => {
    agent?.kill("SIGTERM");
    if (workDir) fs.rmSync(workDir, { recursive: true, force: true });
  });

  test("Verteilung: fünf Dateien mit korrekten Rechten, Rückmeldung im Portal, Türklingel nach Neuausstellung, Cross-Server-Isolation", async ({
    page
  }) => {
    test.setTimeout(600000);
    await login(page, CERTS_ADMIN_EMAIL, CERTS_ADMIN_PASSWORD);
    await enterSystemAdminMode(page, CERTS_ADMIN_NAME, CERTS_ADMIN_PASSWORD);

    // Setup: a third server, opted into NetBird + certificate management, with a
    // reporting-agent token -- the same sequence Scenarios 1 and 2 use, and the
    // token is a hard precondition (no token => no distribution path => the
    // reconcile deliberately places no order at all).
    const adminGroupId = await resolveDefaultAdminGroupId(page);
    const serverCId = await createServerRaw(page, { name: SERVER_C_NAME, domain: SERVER_C_DOMAIN, adminGroupId });
    await page.getByRole("link", { name: t.servers, exact: true }).click();
    await expect(page.getByRole("row", { name: new RegExp(escapeRegExp(SERVER_C_NAME)) })).toBeVisible();
    await openEditServer(page, SERVER_C_NAME);
    await enableServerNetbirdLink(page, serverCId);
    await optInServerCertificate(page, serverCId);
    const tokenC = await mintAgentToken(page, serverCId);

    await waitForRowActiveInUI(page, SERVER_C_DOMAIN, 120000);
    const issued = await fetchCertificateByDomain(page, SERVER_C_DOMAIN);
    expect(issued?.status).toBe("active");
    expect(issued?.fingerprint, "the issued row must carry a fingerprint").toBeTruthy();
    const fingerprintBefore = issued!.fingerprint!;

    // Spawn the REAL agent for THIS server. cert_poll_interval is pinned to 1h so
    // the periodic poll can never be the explanation for anything observed below
    // within this test's lifetime -- only the startup sync and the WebSocket
    // doorbell can be.
    agent = spawn(agentBin, [], {
      env: {
        ...process.env,
        OP_AGENT_GATEWAY_URL: "http://127.0.0.1:8091",
        OP_AGENT_TOKEN: tokenC,
        OP_AGENT_TRANSPORT: "websocket",
        OP_AGENT_CERT_MODE: "files",
        OP_AGENT_CERT_DIR: certDir,
        OP_AGENT_CERT_POLL_INTERVAL: "1h"
      },
      stdio: "inherit"
    });

    // --- Assert 1: the installation itself. ---
    // NOTE on ca.pem: by this point the suite's INTERNAL issuer mode is `acme`
    // (Scenario 2 switched it), yet ca.pem still carries the internal root -- and
    // that is correct, not a contradiction. The trust bundle is mode-INDEPENDENT:
    // switching to acme deliberately keeps the internal root alive as long as
    // internally-signed leaves exist (see AgentCertificate's own comment), so all
    // five files legitimately appear in either mode.
    await expect
      .poll(async () => CERT_FILES.every((f) => fs.existsSync(path.join(certDir, f))), {
        timeout: 60000,
        intervals: [500]
      })
      .toBe(true);

    for (const f of CERT_FILES) {
      const want = f === "privkey.pem" ? 0o600 : 0o644;
      const got = fs.statSync(path.join(certDir, f)).mode & 0o777;
      expect(got.toString(8), `${f} must be mode ${want.toString(8)}`).toBe(want.toString(8));
    }
    const fullchainPath = path.join(certDir, "fullchain.pem");
    const fullchainPEM = fs.readFileSync(fullchainPath, "utf8");
    expect(fullchainPEM).toContain("BEGIN CERTIFICATE");
    expect(leafFingerprintFromPEM(fullchainPEM), "the installed leaf must be the issued one").toBe(fingerprintBefore);
    expect(fs.readFileSync(path.join(certDir, "privkey.pem"), "utf8")).toContain("PRIVATE KEY");

    const ca = await fetchCA(page);
    expect(ca.bundle_pem, "the internal root must still exist (its leaves from Scenario 1 do)").toContain(
      "BEGIN CERTIFICATE"
    );
    expect(
      fs.readFileSync(path.join(certDir, "ca.pem"), "utf8"),
      "ca.pem must be byte-identical to the trust bundle the portal hands out"
    ).toBe(ca.bundle_pem);

    // --- Assert 2: the feedback loop. ---
    // The agent reports what it installed on its telemetry sample; the gateway's
    // in-memory report registry feeds both the DTO and the "Installiert" column.
    await expect
      .poll(
        async () => {
          const row = await fetchCertificateByDomain(page, SERVER_C_DOMAIN);
          return row?.installed === true && row?.installed_fingerprint === fingerprintBefore;
        },
        { timeout: 60000, intervals: [1000] }
      )
      .toBe(true);
    const reported = await fetchCertificateByDomain(page, SERVER_C_DOMAIN);
    expect(reported?.installed_mode).toBe("files");
    expect(reported?.installed_at).toBeTruthy();
    await waitForInstalledYesInUI(page, SERVER_C_DOMAIN, 60000);

    // --- Assert 3: the WebSocket doorbell. ---
    // Anchored in two stages, because "Jetzt erneuern" does NOT run a reconcile
    // pass -- it only clears the row's backoff and marks it pending; the pass that
    // actually re-issues comes from this suite's 60s reconcile ticker. So:
    //   (a) poll the portal until the ISSUED fingerprint changes, and record when
    //       that change first became observable;
    //   (b) only THEN require the file on disk to carry the new fingerprint,
    //       within ~15s OF THAT MOMENT.
    // Only window (b) carries the doorbell claim, and nothing else can explain it:
    // the certificate poll interval is 1h, and the SAME agent process keeps running
    // with no connection drop (asserted below), so neither a poll tick nor a
    // reconnect-triggered sync is available as an alternative explanation.
    const renewResp = await page.request.post("/api/system/certificates/renew", {
      headers: CSRF,
      data: { domain: SERVER_C_DOMAIN }
    });
    expect(renewResp.ok(), `expected success scheduling a renewal, got ${renewResp.status()}`).toBe(true);

    // 120s (not 90s) because the reconcile ticker is 60s and this click can land
    // immediately after a tick: worst case is a full interval plus the real ACME
    // round trip against the fake directory.
    let newFingerprint = "";
    let changedAt = 0;
    const reissueDeadline = Date.now() + 120000;
    while (Date.now() < reissueDeadline) {
      const row = await fetchCertificateByDomain(page, SERVER_C_DOMAIN);
      if (row?.fingerprint && row.fingerprint !== fingerprintBefore) {
        newFingerprint = row.fingerprint;
        changedAt = Date.now();
        break;
      }
      await page.waitForTimeout(2000);
    }
    expect(newFingerprint, `${SERVER_C_DOMAIN} was never re-issued within 120s`).toBeTruthy();

    let onDisk = "";
    while (Date.now() - changedAt < 15000) {
      onDisk = leafFingerprintFromPEM(fs.readFileSync(fullchainPath, "utf8"));
      if (onDisk === newFingerprint) break;
      await page.waitForTimeout(500);
    }
    expect(
      onDisk,
      `the re-issued certificate did not reach the agent within 15s of the change becoming observable ` +
        `(${Date.now() - changedAt}ms) — the doorbell is the only mechanism that could deliver it here`
    ).toBe(newFingerprint);
    // The doorbell claim rests on this: the agent was never restarted, so its
    // startup sync cannot be the explanation either.
    expect(agent?.exitCode, "the agent process must still be the same, still-running one").toBeNull();

    // --- Assert 4: cross-server isolation, in BOTH directions. ---
    // Two tokens, two certificates: each token must receive exactly its own. Server
    // A's token is (re-)minted here because Scenario 1 never surfaced its secret;
    // nothing else depends on the old one.
    const serverAId = await resolveServerIdByName(page, SERVER_A_NAME);
    const tokenA = await mintAgentToken(page, serverAId);
    const rowA = await fetchCertificateByDomain(page, SERVER_A_DOMAIN);
    const rowC = await fetchCertificateByDomain(page, SERVER_C_DOMAIN);
    expect(rowA?.fingerprint, "server A must still hold a certificate from the earlier scenarios").toBeTruthy();
    expect(rowC?.fingerprint).toBeTruthy();
    expect(rowA!.fingerprint).not.toBe(rowC!.fingerprint);

    const asC = await page.request.get("/api/agent/v1/certificate", {
      headers: { Authorization: `Bearer ${tokenC}` }
    });
    expect(asC.ok(), `expected success, got ${asC.status()}: ${await asC.text()}`).toBe(true);
    const bodyC = (await asC.json()) as { domain: string; fingerprint: string; key_pem: string };
    expect(bodyC.domain).toBe(SERVER_C_DOMAIN);
    expect(bodyC.fingerprint).toBe(rowC!.fingerprint);
    expect(bodyC.fingerprint, "server C's token must never receive server A's certificate").not.toBe(
      rowA!.fingerprint
    );
    expect(bodyC.key_pem).toContain("PRIVATE KEY");

    const asA = await page.request.get("/api/agent/v1/certificate", {
      headers: { Authorization: `Bearer ${tokenA}` }
    });
    expect(asA.ok(), `expected success, got ${asA.status()}: ${await asA.text()}`).toBe(true);
    const bodyA = (await asA.json()) as { domain: string; fingerprint: string; key_pem: string };
    expect(bodyA.domain).toBe(SERVER_A_DOMAIN);
    expect(bodyA.fingerprint).toBe(rowA!.fingerprint);
    expect(bodyA.fingerprint, "server A's token must never receive server C's certificate").not.toBe(
      rowC!.fingerprint
    );
    expect(bodyA.key_pem).toContain("PRIVATE KEY");
  });
});

// Scenario 6 (Phase 3 -- the mesh endpoint on HTTPS/WSS): the REAL server-agent
// binary connects to the SECOND listener (OP_AI_GATEWAY_AGENT_ADDR=127.0.0.1:8094)
// over **wss**, trusting ONLY the internal CA bundle it was bootstrapped with (no
// tls_insecure). The gateway then observes that hop as TLS (the portal transport
// column + mesh.tls_observed), and the strict cert_mesh_require_tls gate refuses a
// plaintext call on the mesh listener while /healthz stays open -- then disarms.
//
// This proves the NEW P3 wire contract end-to-end with the real binary. The
// CA-rotation-under-P3 and agent-restart-from-cache paths are covered by unit
// tests (the Task-6 gateway-leaf brake tests, the internal/trust store tests, and
// Scenario 1's CA rotation) rather than duplicated in this slow suite.
//
// Own describe with a describe-scoped agent build/spawn (like Scenario 5), so the
// real agent process only exists around this test.
test.describe("Szenario 6 — der echte ServerAgent verbindet über WSS auf den Mesh-Listener", () => {
  const GW_DOMAIN = `gw.${BASE_DOMAIN}`;
  const SERVER_D_NAME = "E2E Cert Server Delta";
  const SERVER_D_DOMAIN = `d.${BASE_DOMAIN}`;
  // The mesh listener's PLAINTEXT base, used only to exercise the gate directly
  // (the real agent uses wss). page.request can dial an absolute URL, so these
  // hit 127.0.0.1:8094 directly rather than the frontend's vite proxy.
  const MESH_PLAIN = "http://127.0.0.1:8094";

  let workDir = "";
  let agentBin = "";
  let agent: ChildProcess | undefined;

  test.beforeAll(() => {
    workDir = fs.mkdtempSync(path.join(os.tmpdir(), "op-cert-mesh-e2e-"));
    agentBin = path.join(workDir, "server-agent");
    execSync(`go build -o ${agentBin} .`, { cwd: "../../server-agent", stdio: "inherit" });
  });

  test.afterAll(() => {
    agent?.kill("SIGTERM");
    if (workDir) fs.rmSync(workDir, { recursive: true, force: true });
  });

  test("WSS-Telemetrie mit echter Vertrauensprüfung, Transportanzeige TLS, strikter Riegel refuse/allow/disarm", async ({
    page
  }) => {
    test.setTimeout(600000);
    await login(page, CERTS_ADMIN_EMAIL, CERTS_ADMIN_PASSWORD);
    await enterSystemAdminMode(page, CERTS_ADMIN_NAME, CERTS_ADMIN_PASSWORD);

    // Step 1: switch the INTERNAL issuer mode back to self_signed (Scenario 2 left
    // it acme) and set a gateway domain, so a kind=gateway leaf is issued. With
    // OP_AI_GATEWAY_AGENT_ADDR=127.0.0.1:8094 the bind host is 127.0.0.1, so the
    // self_signed gateway leaf carries 127.0.0.1 as an IP-SAN -- which is what the
    // agent verifies when it dials https://127.0.0.1:8094.
    const back = await page.request.put("/api/system/settings", {
      headers: CSRF,
      data: { cert_issuer_mode: "self_signed", cert_gateway_domain: GW_DOMAIN }
    });
    expect(back.ok(), `expected success, got ${back.status()}: ${await back.text()}`).toBe(true);
    await expect
      .poll(async () => (await fetchCertificateByDomain(page, GW_DOMAIN))?.status, { timeout: 240000, intervals: [3000] })
      .toBe("active");

    // Step 2: a server opted into NetBird + certificate management, with a
    // reporting-agent token -- so it has a kind=server row that carries the
    // transport column.
    const adminGroupId = await resolveDefaultAdminGroupId(page);
    const serverDId = await createServerRaw(page, { name: SERVER_D_NAME, domain: SERVER_D_DOMAIN, adminGroupId });
    await page.getByRole("link", { name: t.servers, exact: true }).click();
    await expect(page.getByRole("row", { name: new RegExp(escapeRegExp(SERVER_D_NAME)) })).toBeVisible();
    await openEditServer(page, SERVER_D_NAME);
    await enableServerNetbirdLink(page, serverDId);
    await optInServerCertificate(page, serverDId);
    const tokenD = await mintAgentToken(page, serverDId);

    // Step 3: the internal CA bundle -- the agent's ONLY trust anchor for the mesh
    // connection (mirrors the ca_pem the generated config would inline).
    const { bundle_pem } = await fetchCA(page);
    expect(bundle_pem, "the internal CA bundle must be available for the agent's trust anchor").toContain("BEGIN CERTIFICATE");

    // Step 4: spawn the REAL agent against the mesh listener over wss. cert_mode=off
    // (the DEFAULT of a fresh install -- so it never writes ca.pem itself and MUST
    // rely on the inline ca_pem), and NO tls_insecure: this connection only works
    // if the agent genuinely verifies the gateway leaf against the bundle.
    agent = spawn(agentBin, [], {
      env: {
        ...process.env,
        OP_AGENT_GATEWAY_URL: "https://127.0.0.1:8094",
        OP_AGENT_TOKEN: tokenD,
        OP_AGENT_TRANSPORT: "websocket",
        OP_AGENT_CERT_MODE: "off",
        OP_AGENT_CA_PEM: bundle_pem,
        OP_AGENT_INTERVAL: "1s",
        OP_AGENT_CERT_POLL_INTERVAL: "1h"
      },
      stdio: "inherit"
    });

    // Step 5: the gateway observes the agent's wss hop as TLS. mesh.tls_observed
    // flips true (the arming precondition), and the server's own row shows the
    // transport column as "tls".
    await expect
      .poll(
        async () => {
          const resp = await page.request.get("/api/system/certificates");
          if (!resp.ok()) return false;
          const body = (await resp.json()) as { mesh?: { tls_observed?: boolean } };
          return body.mesh?.tls_observed === true;
        },
        { timeout: 180000, intervals: [2000] }
      )
      .toBe(true);
    await expect
      .poll(async () => (await fetchCertificateByDomain(page, SERVER_D_DOMAIN))?.transport, {
        timeout: 60000,
        intervals: [2000]
      })
      .toBe("tls");

    // Step 6: arm the strict gate. It is now allowed because a fresh TLS hop was
    // observed; the response echoes the stored switch.
    const arm = await page.request.put("/api/system/settings", {
      headers: CSRF,
      data: { cert_mesh_require_tls: true }
    });
    expect(arm.ok(), `expected success arming, got ${arm.status()}: ${await arm.text()}`).toBe(true);
    expect((await arm.json()).cert_mesh_require_tls).toBe(true);

    // Step 7: on the MESH listener directly, a plaintext agent call is refused with
    // 403 certificate.mesh_tls_required, while /healthz stays open. The agent's own
    // wss telemetry keeps flowing (proven by tls_observed staying true below), so
    // the TLS path is still served while armed.
    const plain = await page.request.post(`${MESH_PLAIN}/api/agent/v1/telemetry`);
    expect(plain.status(), `plaintext mesh call should be refused, got ${plain.status()}`).toBe(403);
    expect((await plain.json()).error.code).toBe("certificate.mesh_tls_required");

    const health = await page.request.get(`${MESH_PLAIN}/healthz`);
    expect(health.ok(), `/healthz must stay open on the mesh listener, got ${health.status()}`).toBe(true);

    const stillTLS = await page.request.get("/api/system/certificates");
    const stillBody = (await stillTLS.json()) as { mesh?: { tls_observed?: boolean; require_tls?: boolean } };
    expect(stillBody.mesh?.require_tls, "the gate must report armed").toBe(true);
    expect(stillBody.mesh?.tls_observed, "the agent's wss hop is still served while armed").toBe(true);

    // Step 8: disarm, leaving this suite's shared gateway process in a clean state.
    const disarm = await page.request.put("/api/system/settings", {
      headers: CSRF,
      data: { cert_mesh_require_tls: false }
    });
    expect(disarm.ok(), `expected success disarming, got ${disarm.status()}: ${await disarm.text()}`).toBe(true);
    expect((await disarm.json()).cert_mesh_require_tls).toBe(false);
  });
});

// Scenario 7 (the unified public-domain block, U-T3/U-T4): a public domain
// gets its OWN issuer mode (cert_public_issuer_mode, independent of whatever
// the global cert_issuer_mode currently is -- self_signed since Scenario 6)
// and its OWN, non-shared ACME account (cert_public_acme_shared=false, own
// email + own directory) instead of the global/shared one. Directory-keyed
// ACME accounts (accountFor, service_certificates.go) mean a context's own
// directory registers (or reuses) an account SEPARATE from whatever account
// backs the edge/internal certificates -- so issuing this public leaf must
// have ZERO effect on either. That is this scenario's actual assertion: the
// edge certificate (acme since Scenario 3) and server A's internal
// certificate (acme since Scenario 2) are read BEFORE and AFTER, and both
// must come back byte-identical (same fingerprint, same not_after).
//
// Reuses the SAME fake-ACME server Scenario 2/3 already proved works end to
// end (FAKEACME_ADDR, via PUBLIC_ACME_DIRECTORY_URL -- see its own doc
// comment for why this scenario reaches it through "localhost" rather than
// Scenario 2/3's literal "127.0.0.1") -- the point is the *_acme_shared=false
// wiring (own email/own directory fields, resolved per-context by
// CertSettings.certAcmeConfigFor), not a second fake ACME server. See
// PUBLIC_DOMAIN's own doc comment above for why no additional
// challenge-callback wiring is needed for a domain outside the base domain.
test("Szenario 7 — Öffentliche Domain mit eigenem, nicht-geteiltem ACME-Konto: unabhängig ausgestellt, Edge/intern unverändert", async ({
  page
}) => {
  test.setTimeout(180000);
  await login(page, CERTS_ADMIN_EMAIL, CERTS_ADMIN_PASSWORD);
  await enterSystemAdminMode(page, CERTS_ADMIN_NAME, CERTS_ADMIN_PASSWORD);

  // Baseline: the edge certificate and server A's internal certificate must
  // both still be exactly as the earlier scenarios left them.
  const edgeBefore = await fetchCertificateByDomain(page, EDGE_HOST_NAME);
  const internalBefore = await fetchCertificateByDomain(page, SERVER_A_DOMAIN);
  expect(edgeBefore?.status, "the edge certificate must still be active from Scenario 3").toBe("active");
  expect(internalBefore?.status, "server A must still be active from the earlier scenarios").toBe("active");
  const edgeFingerprintBefore = edgeBefore!.fingerprint;
  const edgeNotAfterBefore = edgeBefore!.not_after;
  const internalFingerprintBefore = internalBefore!.fingerprint;
  const internalNotAfterBefore = internalBefore!.not_after;

  // Step 1: turn on public-domain management, add PUBLIC_DOMAIN, give it its
  // OWN issuer mode (acme -- explicit, since the global mode is self_signed
  // at this point in the suite and cert_public_issuer_mode="" would just
  // follow that) and its OWN ACME account: "Eigene ACME-Einstellungen
  // verwenden" on, own email, directory switched to "Eigene URL" and filled
  // with PUBLIC_ACME_DIRECTORY_URL (the same fake-ACME server the earlier
  // scenarios use, reached via "localhost" -- see that constant's own doc
  // comment for why the literal FAKEACME_DIRECTORY_URL cannot be reused
  // here).
  await gotoCertificates(page);
  await page.getByRole("switch", { name: t.settingsCertManagePublicDomain }).click();
  await page.locator("#cert-public-domains").fill(PUBLIC_DOMAIN);
  await selectPublicIssuerMode(page, "acme");
  const publicPanel = publicCertPanel(page);
  await publicPanel.getByRole("switch", { name: t.settingsAcmeOwnSettings }).click();
  await page.locator("#cert-public-acme-email").fill("public-ops@example.test");
  await publicPanel.getByRole("combobox", { name: t.settingsAcmeDirectory }).click();
  await page.getByRole("option", { name: t.settingsAcmeDirectoryCustom, exact: true }).click();
  await page.locator("#cert-public-acme-directory-custom").fill(PUBLIC_ACME_DIRECTORY_URL);
  await savePublicCertSettings(page);

  // Step 2: the settings change triggers an immediate extra reconcile pass
  // (OnCertSettingsChanged, cmd/gateway/cert_reconcile.go) on top of the
  // suite's 60s ticker, so the new public leaf is issued promptly.
  await waitForRowActiveInUI(page, PUBLIC_DOMAIN, 90000);
  const publicRow = await fetchCertificateByDomain(page, PUBLIC_DOMAIN);
  expect(publicRow?.kind, "the new row must be kind=public").toBe("public");
  expect(publicRow?.status).toBe("active");
  expect(publicRow?.fingerprint, "the public leaf must carry a fingerprint").toBeTruthy();
  expect(publicRow?.not_after).toBeTruthy();

  // Step 3 -- the point of the whole scenario: the edge and internal
  // certificates are BYTE-IDENTICAL to the baseline. Adding, configuring and
  // successfully issuing a public domain under its own, independent ACME
  // account must have zero side effect on either.
  const edgeAfter = await fetchCertificateByDomain(page, EDGE_HOST_NAME);
  const internalAfter = await fetchCertificateByDomain(page, SERVER_A_DOMAIN);
  expect(
    edgeAfter?.fingerprint,
    "the edge certificate must be untouched by the public-domain issuance"
  ).toBe(edgeFingerprintBefore);
  expect(edgeAfter?.not_after).toBe(edgeNotAfterBefore);
  expect(
    internalAfter?.fingerprint,
    "server A's internal certificate must be untouched by the public-domain issuance"
  ).toBe(internalFingerprintBefore);
  expect(internalAfter?.not_after).toBe(internalNotAfterBefore);
});

// Scenario 8 (the separate encrypted agent-port mode, feature agent-mesh-tls-port):
// switching cert_mesh_tls_mode from "combined" (the env default carried through
// Scenarios 1-7) to "separate" via the portal UI toggle -- gated behind Task 8's
// ConfirmDialog -- brings up a DEDICATED TLS-only agent listener on the TLS port
// (OP_AI_GATEWAY_AGENT_TLS_ADDR=127.0.0.1:8095) alongside the unchanged plaintext
// agent port (OP_AI_GATEWAY_AGENT_ADDR=127.0.0.1:8094). The mesh panel then shows
// the TLS listener active on the TLS port; the generated agent config's
// gateway_url points at https://<gateway-domain>:<tls-port>; the plaintext agent
// port is still reachable and cert_mesh_require_tls is unaffected by the mode
// switch; and toggling back to "combined" restores the single-listener state
// (the TLS listener returns to the combined port, the dedicated bind is torn down).
//
// Preconditions carried in from the earlier scenarios (this suite is SERIAL, one
// long-lived gateway process): the certificate module is on, the INTERNAL issuer
// mode is self_signed (Scenario 6 set it back), a kind=gateway leaf for
// GW_DOMAIN is active, and the combined mesh listener has been serving TLS on
// 127.0.0.1:8094 since Scenario 6. cert_mesh_tls_mode is still "" (combined via
// the env default OP_AI_GATEWAY_AGENT_TLS_SEPARATE=false, which is unset here).
//
// NetBird is NOT available in this stack, so this scenario deliberately does NOT
// assert the op-gw-agent-ingest-tls policy (unit-tested elsewhere). Both agent
// addresses are explicit env values (AGENT_ADDR / AGENT_TLS_ADDR), so
// resolveAgentAddr / resolveAgentTLSAddr return fixed addresses without a NetBird
// peer, and the mode toggle rebinds within one reconcile tick
// (OP_AI_GATEWAY_NETBIRD_SYNC_INTERVAL_SECONDS=30).
test.describe("Szenario 8 — separater verschlüsselter Agent-Port (cert_mesh_tls_mode)", () => {
  const GW_DOMAIN = `gw.${BASE_DOMAIN}`;
  const AGENT_TLS_PORT = "8095"; // port of OP_AI_GATEWAY_AGENT_TLS_ADDR in the config
  const AGENT_TLS_ADDR = `127.0.0.1:${AGENT_TLS_PORT}`;
  const AGENT_PLAIN_ADDR = "127.0.0.1:8094"; // OP_AI_GATEWAY_AGENT_ADDR (unchanged)
  // The plaintext mesh listener's base, dialed directly (page.request accepts an
  // absolute URL), exactly like Scenario 6's MESH_PLAIN reaches 8094.
  const AGENT_PLAIN_BASE = "http://127.0.0.1:8094";

  type MeshView = { tls_active?: boolean; address?: string; require_tls?: boolean };
  type CertSettings = {
    cert_mesh_tls_mode?: string;
    cert_mesh_tls_port?: number;
    cert_mesh_tls_separate_active?: boolean;
    cert_mesh_require_tls?: boolean;
  };

  async function fetchMesh(page: Page): Promise<MeshView> {
    const resp = await page.request.get("/api/system/certificates");
    expect(resp.ok(), `expected success reading certificates, got ${resp.status()}: ${await resp.text()}`).toBe(true);
    return ((await resp.json()) as { mesh?: MeshView }).mesh ?? {};
  }

  async function fetchSettings(page: Page): Promise<CertSettings> {
    const resp = await page.request.get("/api/system/settings");
    expect(resp.ok(), `expected success reading settings, got ${resp.status()}: ${await resp.text()}`).toBe(true);
    return (await resp.json()) as CertSettings;
  }

  /**
   * Reads the running mesh listener's TLS-carrying address, but ONLY when the
   * listener actually reports it active -- the backend sets `mesh.address` only
   * while `tls_active` (see handleSystemCertificates), so a poll returns the
   * address for a coherent active listener and null otherwise. Used to wait out
   * the reconcile tick that rebinds the listener on a mode toggle.
   */
  async function activeTlsAddress(page: Page): Promise<string | null> {
    const resp = await page.request.get("/api/system/certificates");
    if (!resp.ok()) return null;
    const mesh = ((await resp.json()) as { mesh?: MeshView }).mesh;
    return mesh?.tls_active ? (mesh.address ?? null) : null;
  }

  /**
   * Drives the mesh-panel "Verschlüsselter Agent-Port" select (a SelectField =
   * MUI non-native Select, same component + interaction as selectIssuerMode) to
   * `mode`, then confirms Task 8's ConfirmDialog and waits for the settings PUT
   * to land. `exact: true` on the combobox/option lookups mirrors selectIssuerMode:
   * the certificate view renders several comboboxes and the read-only status text
   * "Separater TLS-Port aktiv/nicht aktiv" repeats the option's wording elsewhere.
   */
  async function toggleMeshTlsMode(page: Page, mode: "combined" | "separate"): Promise<void> {
    await page.getByRole("combobox", { name: t.certificatesMeshTLSPortMode, exact: true }).click();
    const optionLabel =
      mode === "separate" ? t.certificatesMeshTLSPortModeSeparate : t.certificatesMeshTLSPortModeCombined;
    await page.getByRole("option", { name: optionLabel, exact: true }).click();
    // Task 8: selecting a different value stages it and opens a ConfirmDialog that
    // WARNS the operator the live mesh listener will rebind before it is applied.
    const dialog = page.getByRole("dialog");
    await expect(dialog.getByText(t.certificatesMeshTLSPortModeConfirmTitle)).toBeVisible();
    const [resp] = await Promise.all([
      page.waitForResponse(
        (r) => new URL(r.url()).pathname === "/api/system/settings" && r.request().method() === "PUT"
      ),
      dialog.getByRole("button", { name: t.confirm }).click()
    ]);
    expect(
      resp.ok(),
      `expected success setting cert_mesh_tls_mode=${mode}, got ${resp.status()}: ${await resp.text()}`
    ).toBe(true);
    await expect(dialog).toHaveCount(0);
  }

  /** Extracts the "gateway_url" value from the generated (JSONC) server-agent.json. */
  function gatewayURLFromAgentConfig(configText: string): string {
    const match = configText.match(/"gateway_url"\s*:\s*"([^"]*)"/);
    expect(match, `no gateway_url in the generated agent config:\n${configText}`).toBeTruthy();
    return match![1];
  }

  test("Umschalten separate: dedizierter TLS-Listener auf dem TLS-Port, Agent-Config -> TLS-Port, Klartext-Port erreichbar, zurück auf combined", async ({
    page
  }) => {
    test.setTimeout(600000);
    await login(page, CERTS_ADMIN_EMAIL, CERTS_ADMIN_PASSWORD);
    await enterSystemAdminMode(page, CERTS_ADMIN_NAME, CERTS_ADMIN_PASSWORD);

    // Baseline: the suite arrives here in COMBINED mode -- a single mesh listener
    // on 8094 carrying TLS, the mode setting still "" (env default), and the
    // read-only TLS port reflecting OP_AI_GATEWAY_AGENT_TLS_ADDR's port (8095).
    const gwCert = await fetchCertificateByDomain(page, GW_DOMAIN);
    expect(gwCert?.status, "the kind=gateway leaf must still be active from Scenario 6").toBe("active");
    // Poll (not a bare read): every other reconcile-timing-dependent assertion in
    // this scenario polls, and so must the baseline -- a coincidental cert-rotation
    // reconcile pass landing between Scenario 6's last write and this read could
    // otherwise catch the listener mid-republish. activeTlsAddress() returns the
    // address only while tls_active, so polling it to AGENT_PLAIN_ADDR asserts both
    // "combined listener serving TLS" and "on the plaintext agent port (8094)".
    await expect
      .poll(async () => activeTlsAddress(page), { timeout: 120000, intervals: [2000] })
      .toBe(AGENT_PLAIN_ADDR);
    const settingsBefore = await fetchSettings(page);
    expect(settingsBefore.cert_mesh_tls_mode ?? "").toBe("");
    expect(settingsBefore.cert_mesh_tls_separate_active, "combined by the env default").toBe(false);
    expect(settingsBefore.cert_mesh_tls_port, "the read-only TLS port is the env value").toBe(Number(AGENT_TLS_PORT));

    // Step 1: switch to separate via the portal toggle + confirm the dialog.
    await gotoCertificates(page);
    await toggleMeshTlsMode(page, "separate");

    // The mode + read-only display flip synchronously with the stored value.
    const settingsSeparate = await fetchSettings(page);
    expect(settingsSeparate.cert_mesh_tls_mode).toBe("separate");
    expect(settingsSeparate.cert_mesh_tls_separate_active).toBe(true);
    expect(settingsSeparate.cert_mesh_tls_port).toBe(Number(AGENT_TLS_PORT));

    // Step 2: the reconcile loop (30s ticker) brings up the DEDICATED TLS bind on
    // the TLS port. The TLS-carrying listener address moves from 8094 to 8095.
    await expect
      .poll(async () => activeTlsAddress(page), { timeout: 120000, intervals: [2000] })
      .toBe(AGENT_TLS_ADDR);

    // Step 3: the mesh panel shows the TLS listener active on the TLS port.
    await page.reload();
    await gotoCertificates(page);
    const statusBox = page.locator('[data-testid="certificate-mesh-status"]');
    await expect(statusBox).toContainText(t.certificatesMeshTLSActive);
    await expect(statusBox).toContainText(AGENT_TLS_ADDR);
    const portStatusBox = page.locator('[data-testid="certificate-mesh-tls-port-status"]');
    await expect(portStatusBox).toContainText(t.certificatesMeshTLSPortSeparateActive);
    await expect(portStatusBox).toContainText(AGENT_TLS_PORT);

    // Step 4: a real agent config download's gateway_url points at the TLS port.
    // The URL is gateway-wide mesh material, so any valid agent token surfaces it;
    // re-mint server A's (its Scenario-1 secret was never surfaced).
    const serverAId = await resolveServerIdByName(page, SERVER_A_NAME);
    const tokenA = await mintAgentToken(page, serverAId);
    const configResp = await page.request.get("/api/agent/v1/download/config", {
      headers: { Authorization: `Bearer ${tokenA}` }
    });
    expect(configResp.ok(), `expected the agent config, got ${configResp.status()}: ${await configResp.text()}`).toBe(
      true
    );
    expect(gatewayURLFromAgentConfig(await configResp.text())).toBe(`https://${GW_DOMAIN}:${AGENT_TLS_PORT}`);

    // Step 5: the plaintext agent port is still reachable and gate-able -- the mode
    // switch does not touch cert_mesh_require_tls (still off). /healthz stays open;
    // an unauthenticated telemetry POST reaches the agent handler (401, its own auth
    // refusal) rather than being cut off, proving the plaintext bind still serves.
    const plainHealth = await page.request.get(`${AGENT_PLAIN_BASE}/healthz`);
    expect(plainHealth.ok(), `the plaintext mesh port must stay reachable, got ${plainHealth.status()}`).toBe(true);
    const plainTelemetry = await page.request.post(`${AGENT_PLAIN_BASE}/api/agent/v1/telemetry`);
    expect(
      plainTelemetry.status(),
      `the plaintext agent port must reach the agent handler (401, not gated), got ${plainTelemetry.status()}`
    ).toBe(401);
    expect((await fetchMesh(page)).require_tls, "cert_mesh_require_tls is unaffected by the mode switch").toBe(false);

    // Step 6: toggle back to combined + confirm. The dedicated TLS bind is torn
    // down and the TLS-carrying listener returns to the single combined port.
    await page.reload();
    await gotoCertificates(page);
    await toggleMeshTlsMode(page, "combined");

    const settingsCombined = await fetchSettings(page);
    expect(settingsCombined.cert_mesh_tls_mode).toBe("combined");
    expect(settingsCombined.cert_mesh_tls_separate_active).toBe(false);

    await expect
      .poll(async () => activeTlsAddress(page), { timeout: 120000, intervals: [2000] })
      .toBe(AGENT_PLAIN_ADDR);

    // Final UI sanity: the panel shows the separate TLS port inactive again and the
    // single mesh listener back on the combined port.
    await page.reload();
    await gotoCertificates(page);
    await expect(page.locator('[data-testid="certificate-mesh-tls-port-status"]')).toContainText(
      t.certificatesMeshTLSPortSeparateInactive
    );
    await expect(page.locator('[data-testid="certificate-mesh-status"]')).toContainText(AGENT_PLAIN_ADDR);
  });
});

// Scenario 9 (Phase 4 -- the agent-side TLS proxy + automatic https switch): the
// REAL server-agent, in cert_mode=proxy, stands up a TLS-terminating reverse
// proxy in front of a REAL local plaintext application, and the gateway then
// auto-switches that Application to https and routes over the proxy port --
// end-to-end, verified against the internal CA, with NO tls_insecure anywhere.
//
// It is also the only place in the repo where ADR-017's two OPPOSITE automatic
// moves are decided against a real agent rather than a fake status snapshot: a
// SCOPE EXIT reverts the application to plain http unconditionally (the operator
// asked, and the gateway itself withdrew the routes), while a broken TLS listener
// -- an explicit tls_active:false -- is DECLINED: the application stays https and
// unreachable, is named in https_switch.unreachable_apps with the agent's own
// reason, and recovers with no operator action, because it was never moved
// (docs/architecture/cross-cutting/certificates-tls.md §7.1).
//
// Why this scenario needs a REAL upstream (and thus its own fixture): every
// other "applications" suite in this repo uses the gateway's in-process mock
// provider, which never makes a real outbound TCP connection -- so it cannot
// exercise EITHER the agent's real reverse proxy OR the gateway's CA-trusting
// outbound transport. Scenario 9 therefore spins up a bare node:http app on an
// OS-assigned loopback port (no fixed port, no config.ts change) that records
// each hit and returns a recognizable body.
//
// Why the server's domain is the bare IP "127.0.0.1" (a HARD requirement, not a
// stylistic choice): routing.ApplicationEndpoint composes the gateway's outbound
// URL from server.Domain VERBATIM (no NetBird-IP resolution), and the agent's
// proxy Manager derives its listener bind host from the installed leaf's first
// IP SAN. The certissue SAN-builders route every name through net.ParseIP, so a
// bare-IP domain becomes the leaf's IP SAN (exactly like Scenario 3's EDGE_IP);
// only "127.0.0.1" makes a leaf whose SAN the agent binds AND against which a
// real local https round trip verifies.
//
// The proxy's own listen port is NOT hardcoded: the gateway auto-assigns it from
// cert_proxy_listen_port_base (default 8600) the first time the agent fetches its
// routes, and the test reads it back from the Application DTO / proxy-routes
// response rather than assuming a value.
//
// Own describe with a describe-scoped agent build/spawn (like Scenario 5/6), so
// the real agent process(es) only exist around this test. The scenario is
// self-contained: it configures the certificate module itself (enable +
// self_signed + base domain + scope selected) via the settings PUT, so it runs
// correctly both standalone (-g "Szenario 9" against a fresh gateway) and as the
// last member of the shared serial suite.
test.describe("Szenario 9 — echter ServerAgent terminiert TLS als Reverse-Proxy, Gateway schaltet die Application automatisch auf https", () => {
  const SERVER_E_NAME = "E2E Cert Server Proxy";
  // HARD requirement -- see the block comment above. A bare IP domain becomes the
  // kind=server leaf's IP SAN, which is (a) where the agent's proxy binds and (b)
  // what a Node https dial to https://127.0.0.1:<port> verifies against.
  const SERVER_E_DOMAIN = "127.0.0.1";
  // The recognizable body the fake plaintext app returns, so a response reaching
  // it THROUGH the agent's TLS proxy is unambiguously identifiable.
  const FAKE_APP_BODY = "op-cert-p4-fake-app-OK";

  let workDir = "";
  let certDir = "";
  let agentBin = "";
  let agent: ChildProcess | undefined;
  let fakeApp: http.Server | undefined;
  let fakeAppPort = 0;
  let fakeAppHits = 0;
  let squat: net.Server | undefined;

  type AppDTO = { id: string; scheme: string; endpoint: string; proxy_listen_port: number; port: number };
  type ProxyRoute = { listen: number; upstream: string; app_id: string };
  /**
   * One entry of https_switch.unreachable_apps -- an application the gateway is
   * REFUSING to downgrade to plaintext: proxy-switched to https, its agent
   * explicitly reporting the proxy listener not terminating TLS. `route_state` is
   * the agent's own proxy.RouteState verbatim, `action` the remedy
   * (portal.HTTPSSwitchUnreachableDTO / frontend HTTPSSwitchUnreachableApp).
   */
  type UnreachableAppDTO = {
    server_id: string;
    server_name: string;
    app_id: string;
    app_type: string;
    proxy_listen_port: number;
    route_state?: string;
    action: string;
  };

  async function fetchApplications(page: Page, serverId: string): Promise<AppDTO[]> {
    const resp = await page.request.get(`/api/portal/servers/${serverId}/applications`);
    expect(resp.ok(), `expected applications for ${serverId}, got ${resp.status()}: ${await resp.text()}`).toBe(true);
    return ((await resp.json()) as { data: AppDTO[] }).data;
  }

  async function fetchApplication(page: Page, serverId: string, appId: string): Promise<AppDTO | undefined> {
    return (await fetchApplications(page, serverId)).find((a) => a.id === appId);
  }

  /**
   * Fetches the desired TLS-proxy topology exactly as the ServerAgent would --
   * Bearer-authed with the server's agent token, through the same public listener
   * the agent uses in this suite. DATA only (listen/upstream/app_id + etag), never
   * a command.
   */
  async function fetchProxyRoutesAsAgent(page: Page, token: string): Promise<{ routes: ProxyRoute[]; etag: string }> {
    const resp = await page.request.get("/api/agent/v1/proxy-routes", {
      headers: { Authorization: `Bearer ${token}` }
    });
    expect(resp.ok(), `expected proxy-routes success, got ${resp.status()}: ${await resp.text()}`).toBe(true);
    return (await resp.json()) as { routes: ProxyRoute[]; etag: string };
  }

  /**
   * Reads https_switch.unreachable_apps off the SAME portal endpoint the certificate
   * view renders (`GET /api/system/certificates`, session-cookie authenticated as the
   * elevated system_admin). The list is DERIVED from mode + applications + the agent's
   * proxy-route status snapshot, so it needs no reconcile pass to become accurate --
   * which is exactly why the recovery leg can poll it read-only. Absent/omitted reads
   * as empty, never as a failure: a healthy deployment has nothing to report here.
   */
  async function fetchUnreachableApps(page: Page): Promise<UnreachableAppDTO[]> {
    const resp = await page.request.get("/api/system/certificates");
    expect(resp.ok(), `expected success reading certificates, got ${resp.status()}: ${await resp.text()}`).toBe(true);
    const body = (await resp.json()) as { https_switch?: { unreachable_apps?: UnreachableAppDTO[] } };
    return body.https_switch?.unreachable_apps ?? [];
  }

  /** Sets cert_https_switch_mode via the same settings PUT pattern the rest of the suite uses. */
  async function putHttpsSwitchMode(page: Page, mode: "manual" | "auto" | "selected"): Promise<void> {
    const resp = await page.request.put("/api/system/settings", {
      headers: CSRF,
      data: { cert_https_switch_mode: mode }
    });
    expect(resp.ok(), `expected success setting cert_https_switch_mode=${mode}, got ${resp.status()}: ${await resp.text()}`).toBe(
      true
    );
  }

  /** The env every agent generation in this scenario shares (only the token is constant here). */
  function agentEnv(token: string): NodeJS.ProcessEnv {
    return {
      ...process.env,
      OP_AGENT_GATEWAY_URL: "http://127.0.0.1:8091",
      OP_AGENT_TOKEN: token,
      OP_AGENT_TRANSPORT: "websocket",
      OP_AGENT_CERT_MODE: "proxy",
      OP_AGENT_CERT_DIR: certDir,
      // Snappy telemetry so a reported tls_active flip lands within a couple of
      // seconds; the certificate/route poll is floored at 1m by config, but the
      // agent ALWAYS runs an immediate sync (fetch routes + apply) at startup, so
      // every generation binds/rebinds on spawn regardless of the poll floor.
      OP_AGENT_INTERVAL: "1s",
      OP_AGENT_CERT_POLL_INTERVAL: "3s"
    };
  }

  /** Terminates an agent generation and resolves once the process has actually exited (so its proxy port is released). */
  function stopAgent(a: ChildProcess): Promise<void> {
    return new Promise<void>((resolve) => {
      if (a.exitCode !== null || a.signalCode !== null) {
        resolve();
        return;
      }
      a.once("exit", () => resolve());
      a.kill("SIGTERM");
    });
  }

  /**
   * Occupies 127.0.0.1:<port> from the TEST itself. This is the deterministic lever
   * for a REAL broken TLS listener: once this listener holds the port, a freshly
   * (re)started agent's proxy.Manager cannot net.Listen it, so startProxyLocked leaves
   * the route PENDING in state "bind_failed" and Manager.Status() reports
   * {listen, tls_active:false} WITH the route still present -- the EXPLICIT
   * tls_active=false, as opposed to the missing route a merely-silent agent produces
   * (which the reconcile deliberately treats as neither a forward nor a revert).
   * Poll-binds because the previous agent generation releases the port asynchronously
   * on shutdown.
   *
   * NOTE (verified against server-agent/internal/proxy/proxy.go): squatting a port
   * a STILL-RUNNING agent already holds does NOT work -- the agent owns it and the
   * bind simply fails on the test side; and on a stable route the agent gets 304s
   * and never re-Applies, so it never re-attempts the bind. Forcing a fresh bind
   * attempt against the already-squatted port is what makes the false deterministic,
   * hence: stop the agent -> squat -> restart the agent. The same 304 property is why
   * the recovery leg restarts the agent once more after releasing the port.
   */
  async function bindSquat(port: number, deadlineMs: number): Promise<net.Server> {
    const start = Date.now();
    for (;;) {
      try {
        return await new Promise<net.Server>((resolve, reject) => {
          const s = net.createServer();
          s.once("error", reject);
          s.listen(port, "127.0.0.1", () => {
            s.removeListener("error", reject);
            resolve(s);
          });
        });
      } catch (err) {
        if (Date.now() - start > deadlineMs) throw err;
        await new Promise((r) => setTimeout(r, 200));
      }
    }
  }

  /**
   * A CA-verified https GET straight to the agent's TLS proxy: trusts ONLY the
   * internal CA bundle (never rejectUnauthorized:false), so the connection
   * completes iff the proxy presents a leaf that chains to the internal CA.
   * Returns the status, body, and the peer leaf's sha256 fingerprint (colon-hex)
   * so the caller can also prove it is EXACTLY the issued leaf.
   */
  function caDial(
    port: number,
    caPem: string,
    urlPath: string
  ): Promise<{ status: number; body: string; peerFingerprint256: string }> {
    return new Promise((resolve, reject) => {
      const req = https.request(
        { host: "127.0.0.1", port, path: urlPath, method: "GET", ca: caPem, rejectUnauthorized: true },
        (res) => {
          // Capture the peer leaf NOW, while the TLS socket is still attached to
          // the response -- reading res.socket at the 'end' event is too late (the
          // socket has been detached/released by then, yielding null).
          const socket = res.socket as import("node:tls").TLSSocket;
          const peer = typeof socket?.getPeerCertificate === "function" ? socket.getPeerCertificate() : undefined;
          let data = "";
          res.setEncoding("utf8");
          res.on("data", (c) => {
            data += c;
          });
          res.on("end", () => {
            resolve({ status: res.statusCode ?? 0, body: data, peerFingerprint256: peer?.fingerprint256 ?? "" });
          });
        }
      );
      req.on("error", reject);
      req.end();
    });
  }

  test.beforeAll(async () => {
    workDir = fs.mkdtempSync(path.join(os.tmpdir(), "op-cert-proxy-e2e-"));
    certDir = path.join(workDir, "certs");
    fs.mkdirSync(certDir, { recursive: true });
    agentBin = path.join(workDir, "server-agent");
    execSync(`go build -o ${agentBin} .`, { cwd: "../../server-agent", stdio: "inherit" });

    // The REAL local plaintext upstream. It binds 127.0.0.1:<port> (D3: the gateway
    // hardcodes the proxy upstream to http://127.0.0.1:<app.Port>), records every
    // hit, and returns a recognizable body.
    fakeApp = http.createServer((_req, res) => {
      fakeAppHits += 1;
      res.writeHead(200, { "content-type": "text/plain" });
      res.end(FAKE_APP_BODY);
    });
    await new Promise<void>((resolve) => fakeApp!.listen(0, "127.0.0.1", () => resolve()));
    fakeAppPort = (fakeApp!.address() as net.AddressInfo).port;
  });

  test.afterAll(async () => {
    if (agent) await stopAgent(agent);
    if (squat) await new Promise<void>((resolve) => squat!.close(() => resolve()));
    if (fakeApp) await new Promise<void>((resolve) => fakeApp!.close(() => resolve()));
    if (workDir) fs.rmSync(workDir, { recursive: true, force: true });
  });

  test("cert_mode=proxy: manual liefert keine Route, auto -> TLS-Proxy aktiv -> Application auf https (gegen interne CA verifiziert), Scope-Exit -> zurück auf http, defektes TLS -> kein Rückfall auf Klartext, sondern https + Meldung als unerreichbar, Erholung ohne Eingriff", async ({
    page
  }) => {
    test.setTimeout(600000);
    await login(page, CERTS_ADMIN_EMAIL, CERTS_ADMIN_PASSWORD);
    await enterSystemAdminMode(page, CERTS_ADMIN_NAME, CERTS_ADMIN_PASSWORD);

    // --- Setup: configure the certificate module (self-contained). ---
    // Enable + self_signed + a base domain (its existence is a precondition for the
    // reconcile to run at all; the bare-IP server leaf is issued regardless of being
    // under it) + selected server scope. Start in manual switch mode -- the
    // byte-neutral default -- so the manual no-op below is a genuine assertion.
    const cfg = await page.request.put("/api/system/settings", {
      headers: CSRF,
      data: {
        cert_enabled: true,
        cert_issuer_mode: "self_signed",
        cert_base_domain: BASE_DOMAIN,
        cert_server_scope: "selected",
        cert_https_switch_mode: "manual"
      }
    });
    expect(cfg.ok(), `expected success configuring the certificate module, got ${cfg.status()}: ${await cfg.text()}`).toBe(
      true
    );

    // --- Setup: the AI-server (domain "127.0.0.1") + NetBird + cert opt-in + token. ---
    const adminGroupId = await resolveDefaultAdminGroupId(page);
    const serverEId = await createServerRaw(page, { name: SERVER_E_NAME, domain: SERVER_E_DOMAIN, adminGroupId });
    await page.getByRole("link", { name: t.servers, exact: true }).click();
    await expect(page.getByRole("row", { name: new RegExp(escapeRegExp(SERVER_E_NAME)) })).toBeVisible();
    await openEditServer(page, SERVER_E_NAME);
    await enableServerNetbirdLink(page, serverEId);
    await optInServerCertificate(page, serverEId);
    const tokenE = await mintAgentToken(page, serverEId);

    // --- Setup: one http Application pointing at the fake app's plaintext port. ---
    // scheme "http" makes it a forward-switch candidate; the port is exactly the
    // fake app's bind port, which is what the gateway hands the agent as the proxy
    // upstream (http://127.0.0.1:<port>).
    const appResp = await page.request.post(`/api/portal/servers/${serverEId}/applications`, {
      headers: CSRF,
      data: { type: "vllm", port: fakeAppPort, scheme: "http", health_check_mode: "always_reachable" }
    });
    expect(appResp.ok(), `expected success creating the application, got ${appResp.status()}: ${await appResp.text()}`).toBe(
      true
    );
    const appId = ((await appResp.json()) as { id: string }).id;

    // --- Setup: wait for the kind=server leaf (with 127.0.0.1 as its IP SAN) to be issued. ---
    await expect
      .poll(async () => (await fetchCertificateByDomain(page, SERVER_E_DOMAIN))?.status, {
        timeout: 180000,
        intervals: [3000]
      })
      .toBe("active");
    const issued = await fetchCertificateByDomain(page, SERVER_E_DOMAIN);
    expect(issued?.fingerprint, "the issued server leaf must carry a fingerprint").toBeTruthy();
    const issuedFingerprint = issued!.fingerprint!;

    // The internal CA bundle -- the ONLY trust anchor the Node https dial will use
    // later (no tls_insecure), and what the gateway's own outbound transport also
    // verifies against.
    const { bundle_pem } = await fetchCA(page);
    expect(bundle_pem, "the internal CA bundle must be available as the dial's trust anchor").toContain(
      "BEGIN CERTIFICATE"
    );

    // --- Assert (manual mode is a no-op): the gateway hands the agent NO routes and
    // assigns NO proxy port while cert_https_switch_mode=manual. This is the
    // mechanism by which no app can ever be switched in manual mode
    // (httpsSwitchInScope=false => AgentProxyRoutes returns empty, and the switch
    // reconcile skips the server entirely). Proven directly at the agent-facing
    // endpoint + the Application DTO, no agent process required. ---
    const manualRoutes = await fetchProxyRoutesAsAgent(page, tokenE);
    expect(manualRoutes.routes, "manual mode must hand the agent an empty route set").toEqual([]);
    const appManual = await fetchApplication(page, serverEId, appId);
    expect(appManual?.scheme, "manual mode must not switch the app").toBe("http");
    expect(appManual?.proxy_listen_port, "manual mode must not assign a proxy port").toBe(0);

    // --- Flip to auto: the SAME endpoint now hands a route for the candidate app,
    // and the proxy listen port is lazily assigned. The contrast with manual above
    // is the point. ---
    await putHttpsSwitchMode(page, "auto");
    const autoRoutes = await fetchProxyRoutesAsAgent(page, tokenE);
    expect(autoRoutes.routes.length, "auto mode must hand the candidate app a route").toBe(1);
    expect(autoRoutes.routes[0].upstream, "the route's upstream must be the fake app's plaintext port").toBe(
      `http://127.0.0.1:${fakeAppPort}`
    );
    const proxyPort = autoRoutes.routes[0].listen;
    expect(proxyPort, "the gateway must have assigned a proxy listen port").toBeGreaterThan(0);

    // --- Launch the REAL agent in cert_mode=proxy. It installs the leaf, fetches the
    // route, binds a TLS proxy on proxyPort (bind host = the leaf's 127.0.0.1 IP
    // SAN), and forwards decrypted traffic to the fake app. ---
    agent = spawn(agentBin, [], { env: agentEnv(tokenE), stdio: "inherit" });

    // --- Assert (forward): once the agent reports its proxy listener terminating TLS
    // (tls_active:true), the switch reconcile flips the app to https and routes over
    // the proxy port. Re-PUTting auto inside the poll fires an immediate reconcile
    // (touchesCert), so this converges as soon as the agent has bound + reported,
    // rather than waiting out the 60s ticker. ---
    await expect
      .poll(
        async () => {
          await putHttpsSwitchMode(page, "auto");
          return (await fetchApplication(page, serverEId, appId))?.scheme;
        },
        { timeout: 180000, intervals: [3000] }
      )
      .toBe("https");
    const switched = await fetchApplication(page, serverEId, appId);
    expect(switched?.proxy_listen_port, "the switched app must carry the assigned proxy port").toBe(proxyPort);
    expect(switched?.endpoint, "routing must now target the agent's TLS proxy port").toBe(
      `https://127.0.0.1:${proxyPort}`
    );

    // --- Assert (the forward path really works, verified against the internal CA):
    // a Node https dial to the proxy, trusting ONLY the internal CA bundle, reaches
    // the fake app THROUGH the agent's TLS terminator. The dial only completes if the
    // proxy's leaf chains to the internal CA; the fake app records the hit; and the
    // peer leaf is EXACTLY the one the gateway issued. NEVER rejectUnauthorized:false. ---
    const hitsBefore = fakeAppHits;
    const dialed = await caDial(proxyPort, bundle_pem, "/probe");
    expect(dialed.status, "the CA-verified request must reach the fake app through the proxy").toBe(200);
    expect(dialed.body, "the response must be the fake app's recognizable body").toBe(FAKE_APP_BODY);
    expect(fakeAppHits, "the fake app must have recorded the CA-verified hit").toBeGreaterThan(hitsBefore);
    expect(
      dialed.peerFingerprint256.replace(/:/g, "").toLowerCase(),
      "the proxy's TLS peer leaf must be exactly the internally-issued server leaf"
    ).toBe(issuedFingerprint);

    // --- Assert (scope-exit revert — the P4 final-review fix): narrowing the switch
    // scope must revert an already-switched app UNCONDITIONALLY. With the agent
    // still running and TLS active on proxyPort, flip cert_https_switch_mode to
    // manual. The server leaves scope, so AgentProxyRoutes hands the agent an EMPTY
    // set (the agent drains + closes its TLS listener) AND the switch reconcile's
    // scope-exit pass flips the app back to http — WITHOUT consulting the proxy
    // status snapshot, because the gateway ITSELF withdrew the route. Before the fix
    // the app stayed https on a now-dead proxy port (connection-refused for every
    // request and the health probe -> dropped from routing); here routing must
    // return to the plaintext upstream. This is the ONE automatic move to plaintext
    // ADR-017 keeps, and it is deliberately the OPPOSITE decision from the explicit
    // tls_active:false leg below (same-looking move, but there nobody asked for
    // anything; here the operator did, and the gateway itself withdrew the routes).
    // Re-PUTting manual inside the poll fires an immediate reconcile (touchesCert),
    // matching the forward leg's convergence pattern. ---
    await expect
      .poll(
        async () => {
          await putHttpsSwitchMode(page, "manual");
          return (await fetchApplication(page, serverEId, appId))?.scheme;
        },
        { timeout: 180000, intervals: [3000] }
      )
      .toBe("http");
    const scopeExitReverted = await fetchApplication(page, serverEId, appId);
    expect(
      scopeExitReverted?.endpoint,
      "scope-exit revert must return routing to the plaintext upstream port"
    ).toBe(`http://127.0.0.1:${fakeAppPort}`);

    // --- Re-arm for the explicit-tls_active:false leg below: flip back to auto so the
    // route reappears and the app forwards to https again on a reported
    // tls_active:true for the (same, idempotently reassigned) proxy port. This
    // restores the switched-and-serving https precondition the port-squat leg starts
    // from -- that leg only means anything against an app that IS on https.
    //
    // This usually converges within a poll or two rather than waiting for the agent
    // to rebind: SyncRoutes rides the certificate-poll cadence, which config floors at
    // 60s, so the scope exit above has typically not yet reached the agent and its
    // listener is still up and still reported tls_active:true. Either way the
    // precondition is the same; only the speed differs. ---
    await expect
      .poll(
        async () => {
          await putHttpsSwitchMode(page, "auto");
          return (await fetchApplication(page, serverEId, appId))?.scheme;
        },
        { timeout: 180000, intervals: [3000] }
      )
      .toBe("https");
    expect(
      (await fetchApplication(page, serverEId, appId))?.proxy_listen_port,
      "re-arm must reuse the same proxy port (idempotent assignment)"
    ).toBe(proxyPort);

    // --- Assert (NO automatic downgrade on explicit tls_active:false — ADR-017 /
    // certificates-tls §7.1): stop the agent (releasing the proxy port), squat that
    // port from the test, then restart the agent. The restarted agent's proxy cannot
    // bind the squatted port, so it reports the route tls_active:false WITH the route
    // still present -- the EXPLICIT false, which is exactly the input the reconcile
    // used to answer by flipping the app back to plain http. It must not: an
    // automatic switch to unencrypted is a security problem, not a mitigation. The
    // app stays https and becomes UNREACHABLE, and the gateway says so out loud.
    // (A merely-killed/silent agent reports a MISSING route, which is never either
    // a forward or a revert; this is why the port-squat + restart is the
    // deterministic lever, verified against proxy.go's
    // Manager.Status/startProxyLocked.) ---
    await stopAgent(agent);
    squat = await bindSquat(proxyPort, 30000);
    agent = spawn(agentBin, [], { env: agentEnv(tokenE), stdio: "inherit" });

    // Each poll iteration re-PUTs auto, which fires an immediate reconcile pass
    // (touchesCert) — so this waits for the failure to be OBSERVED BY THE RECONCILE,
    // not merely reported. That is what makes the poll a real assertion of the
    // policy: under the old downgrade behaviour one of these passes would flip the
    // app to http, and an http app can never appear in unreachable_apps (the view's
    // predicate requires a proxy-switched https app), so this would never converge.
    await expect
      .poll(
        async () => {
          await putHttpsSwitchMode(page, "auto");
          const app = await fetchApplication(page, serverEId, appId);
          const entry = (await fetchUnreachableApps(page)).find((u) => u.app_id === appId);
          return { scheme: app?.scheme, unreachablePort: entry?.proxy_listen_port };
        },
        { timeout: 180000, intervals: [3000] }
      )
      .toEqual({ scheme: "https", unreachablePort: proxyPort });

    // (1) The application stayed https and still points at the proxy port: it was
    // never moved, so there is nothing to move back later.
    const declined = await fetchApplication(page, serverEId, appId);
    expect(declined?.scheme, "a broken TLS listener must NEVER downgrade the app to plaintext http").toBe("https");
    expect(declined?.proxy_listen_port, "the declined revert must leave the assigned proxy port alone").toBe(proxyPort);
    expect(declined?.endpoint, "routing must still target the agent's TLS proxy port, not the plaintext upstream").toBe(
      `https://127.0.0.1:${proxyPort}`
    );

    // (2) The outage is SURFACED, not silent -- the whole justification for paying
    // availability here. GET /api/system/certificates carries the application under
    // https_switch.unreachable_apps, identified by server + app, with the AGENT'S OWN
    // RouteState as the reason ("bind_failed": the port is held by this test's squat,
    // the leaf is installed and its 127.0.0.1 SAN gives a bind host, so bind is the
    // only step left to fail) and a non-empty remedy.
    const unreachable = (await fetchUnreachableApps(page)).find((u) => u.app_id === appId);
    expect(unreachable, "the refusing gateway must name the unreachable application in the certificates view").toBeTruthy();
    expect(unreachable!.server_id, "the alert must name the server the application lives on").toBe(serverEId);
    expect(unreachable!.server_name, "the alert must carry the server's display name").toBe(SERVER_E_NAME);
    expect(unreachable!.app_type, "the alert must carry the application's type").toBe("vllm");
    expect(unreachable!.proxy_listen_port, "the alert must name the port whose listener is down").toBe(proxyPort);
    expect(unreachable!.route_state, "the reason must be the agent's own RouteState, relayed verbatim").toBe(
      "bind_failed"
    );
    expect(unreachable!.action, "the alert must say what to do, not only what happened").toBeTruthy();

    // --- Assert (3) (recovery needs NO operator action): release the squatted port
    // and let the agent bind it again. Nothing is told to the gateway, no switch mode
    // is touched and the application is never edited -- the property the no-downgrade
    // policy buys in exchange for the outage is precisely that there is no forward
    // switch to re-run and no window in which the app is briefly plaintext.
    //
    // The agent generation is restarted because proxy.Manager only re-attempts a
    // bind_failed route when a route set is re-APPLIED, and Driver.SyncRoutes returns
    // early on the 304 an unchanged topology answers (verified against
    // routes_client.go) -- a restart runs its immediate startup sync against an empty
    // ETag cache, so the retry is deterministic instead of waiting for an unrelated
    // topology change. That is the ENVIRONMENT healing, the same class of event as
    // the operator freeing the port; it is not a gateway-side re-switch. ---
    await new Promise<void>((resolve) => squat!.close(() => resolve()));
    squat = undefined;
    await stopAgent(agent);
    agent = spawn(agentBin, [], { env: agentEnv(tokenE), stdio: "inherit" });

    // Polls READ-ONLY -- no settings PUT, no reconcile nudge, nothing that could be
    // mistaken for an operator re-switching the application. Both halves must hold in
    // the SAME iteration: the derived alert has cleared AND a CA-verified dial gets
    // the fake app's body back through the rebuilt TLS terminator. Requiring the dial
    // is not decoration -- an agent generation that has restarted but not yet applied
    // its routes reports NO route for that port, which alone would clear the alert
    // (missing != explicitly false) before the listener is actually serving. The dial
    // is caught rather than thrown, because expect.poll evaluates the callback OUTSIDE
    // its retry try/catch: an ECONNREFUSED on an early attempt must retry, not fail.
    const hitsBeforeRecovery = fakeAppHits;
    let redialed: { status: number; body: string; peerFingerprint256: string } | undefined;
    await expect
      .poll(
        async () => {
          if ((await fetchUnreachableApps(page)).some((u) => u.app_id === appId)) return "still reported unreachable";
          try {
            redialed = await caDial(proxyPort, bundle_pem, "/probe-after-recovery");
          } catch (err) {
            return `proxy not serving yet: ${String(err)}`;
          }
          return redialed.body;
        },
        { timeout: 180000, intervals: [3000] }
      )
      .toBe(FAKE_APP_BODY);
    expect(redialed!.status, "the CA-verified request must reach the fake app again after recovery").toBe(200);
    expect(fakeAppHits, "the fake app must have recorded the post-recovery hit").toBeGreaterThan(hitsBeforeRecovery);
    expect(
      redialed!.peerFingerprint256.replace(/:/g, "").toLowerCase(),
      "the recovered proxy must still present exactly the internally-issued server leaf"
    ).toBe(issuedFingerprint);

    // The app is exactly where it was before the outage -- proof that recovery needed
    // no re-switch, because nothing was ever switched away.
    const recovered = await fetchApplication(page, serverEId, appId);
    expect(recovered?.scheme, "recovery must not have required a re-switch: the app was https the whole time").toBe(
      "https"
    );
    expect(recovered?.proxy_listen_port, "recovery must reuse the very same proxy port").toBe(proxyPort);
    expect(recovered?.endpoint, "routing must still target the agent's TLS proxy port").toBe(
      `https://127.0.0.1:${proxyPort}`
    );
  });
});
