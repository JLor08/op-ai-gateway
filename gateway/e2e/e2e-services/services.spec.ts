// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { expect, test, type Browser, type Locator, type Page } from "@playwright/test";
import { messages } from "../../frontend/src/i18n";
import { login, inviteUser, setPassword, uniqueEmail } from "../e2e/helpers";
import { SERVICES_ADMIN_EMAIL, SERVICES_ADMIN_NAME, SERVICES_ADMIN_PASSWORD } from "../playwright.services.config";

const t = messages.de;
const GW = "http://127.0.0.1:8091";

// Seeded in memory mode (cmd/gateway seedDefaultServer): the only two gateway
// model names actually routable in this test environment. This suite now
// runs SQLITE-backed (see playwright.services.config.ts) rather than the
// default memory mode -- but seedDefaultServerIfEmpty seeds the identical
// mock server + these same two model mappings on a fresh sqlite DB too, so
// these names still resolve.
const ALLOWED_MODEL = "qwen-coder";
const OTHER_MODEL = "gpt-oss-20b";

// A fresh bootstrap system_admin (SQLITE mode) owns ZERO admin groups, and
// Phase B/C made an admin group MANDATORY on both the create-service form
// AND the invite-user form (>= 1 candidate required, else the submit button
// stays permanently disabled -- see ServicesView.tsx's
// createEffectiveAdminGroupIds/UsersView's admin-group picker). The very
// first thing this suite does, before either of the two original tests
// below, is create exactly ONE admin group so both forms auto-select it (a
// SINGLE candidate renders no picker at all -- see
// candidatesUnderSystemGroup/createEffectiveCandidates in ServicesView.tsx)
// and neither original test's body needs to change beyond its login
// credentials. See BASELINE_SG/BASELINE_AG below.
const BASELINE_SG = "E2E-SVC-Base-System";
const BASELINE_AG = "E2E-SVC-Base-Admin";

// Admin-group permissions Phase C scenario (spec:
// docs/superpowers/specs/2026-08-10-admin-group-permissions-phase-c-design.md)
// naming, mirroring servers.spec.ts's own naming note: distinct,
// non-substring-colliding names -- several helpers below locate a row via
// `getByRole("row", { name: new RegExp(name) })`, a SUBSTRING match against
// the row's whole accessible text -- so no name here may be a literal
// substring of another (nor of BASELINE_SG/BASELINE_AG above).
const SG_NAME = "E2E-SVC-Scope-System"; // the ONE system group every admin group below hangs under
const AG_ONE = "E2E-SVC-Scope-Bravo"; // A co-manages this one (can_manage_services only)
const AG_TWO = "E2E-SVC-Scope-Charlie"; // A ALSO co-manages this one -- used for the add/remove test
const AG_OUT = "E2E-SVC-Scope-Delta"; // A is never added here -- proves the 404-no-leak scoping
const ADMIN_A_NAME = "E2E-SVC-Coordinator";
const SERVICE_ALPHA = "E2E-SVC-Service-Alpha"; // A creates this one, into AG_ONE
const SERVICE_BETA = "E2E-SVC-Service-Beta"; // system_admin creates this one, into AG_OUT

// >= 10 chars per the password policy in internal/auth. Test-only.
const SCOPE_USER_PASSWORD = "E2E-Services-Pass-1";

/** Service DTO shape as returned by the portal (see internal/portal/service_services.go ServiceDTO). */
type ServiceRow = {
  id: string;
  name: string;
  admin_groups: { id: string; name: string }[];
  system_group_id: string;
  system_group_name: string;
};
type ServiceListBody = { data: ServiceRow[] };
type AdminGroupCandidate = { id: string; name: string; parent_group_id: string; parent_group_name: string };

// Drive the admin UI to create a service (optionally allowlisted to a
// model) and mint a token for it, capturing the one-time secret from the
// reveal dialog. Unmodified from the pre-Phase-C version of this suite: it
// never touches an admin-group picker, relying on there being EXACTLY ONE
// candidate (auto-selected, no field rendered at all) at every call site
// that still uses it -- see BASELINE_SG/BASELINE_AG above and the "setup"
// test below.
async function createServiceWithToken(
  page: Page,
  opts: { name: string; allowedModel?: string }
): Promise<{ secret: string }> {
  await page.getByRole("link", { name: t.services }).click();
  await page.getByRole("button", { name: t.serviceCreate }).click();

  await page.locator("#service-name").fill(opts.name);

  if (opts.allowedModel) {
    await page.getByRole("combobox", { name: t.serviceAllowedModelsLabel }).click();
    await page.getByRole("option", { name: opts.allowedModel, exact: true }).click();
    // Close the multi-select popup so it doesn't obscure the submit button.
    await page.keyboard.press("Escape");
  }

  await page.getByRole("button", { name: t.serviceCreate }).click();

  // Back on the list — open the freshly created service's detail view.
  const row = page.getByRole("row", { name: new RegExp(opts.name) });
  await row.getByRole("button", { name: t.modelDetailsAction }).click();

  // Mint a token.
  await page.getByRole("button", { name: t.serviceTokenCreate }).click();
  const createDialog = page.getByRole("dialog");
  await createDialog.getByLabel(t.tokenNameLabel).fill("e2e-token");
  await createDialog.getByRole("button", { name: t.serviceTokenCreate }).click();

  // The reveal dialog shows the one-time secret + a ready-to-paste curl.
  const revealDialog = page.getByRole("dialog");
  await expect(revealDialog.getByText(/\/v1\/chat\/completions/)).toBeVisible();
  const secret = (await revealDialog.locator('[data-testid="secret-reveal"] code').innerText()).trim();
  expect(secret.length).toBeGreaterThan(0);
  await revealDialog.getByRole("button", { name: t.captureClose }).click();

  // Back to the list (the detail view's breadcrumb "Back" button) so a
  // caller can immediately act on the list row (e.g. toggle disable).
  await page.getByRole("button", { name: t.back }).click();

  return { secret };
}

/**
 * Navigates to the Groups view and waits for the landscape fetch to complete
 * (mirrors servers.spec.ts's gotoGroups -- GroupsView's landscape fetch opts
 * OUT of loading-tracking, so there is no visible loading gate to wait on
 * otherwise).
 */
async function gotoGroups(page: Page): Promise<void> {
  const [resp] = await Promise.all([
    page.waitForResponse((r) => new URL(r.url()).pathname === "/api/portal/groups" && r.request().method() === "GET"),
    page.getByRole("link", { name: t.groups, exact: true }).click()
  ]);
  expect(resp.ok()).toBe(true);
}

/**
 * Navigates to the Services view and waits for the service-admin-group-
 * candidates fetch to land (a mount effect of ServicesView, NOT gated on the
 * caller being admin -- the create form's admin-group picker AND the
 * detail-view linkage editor both need it resolved before they render, so
 * this avoids racing an in-flight fetch).
 */
async function gotoServices(page: Page): Promise<void> {
  const [resp] = await Promise.all([
    page.waitForResponse(
      (r) => new URL(r.url()).pathname === "/api/portal/service-admin-group-candidates" && r.request().method() === "GET"
    ),
    page.getByRole("link", { name: t.services }).click()
  ]);
  expect(resp.ok()).toBe(true);
}

/**
 * Enters System-Admin mode (step-up) for the bootstrap system_admin -- a
 * fresh session is NOT elevated by default (spec: 2026-08-10-system-admin-
 * step-up-mode-design.md), and the `system` scope needed to create System-
 * tier groups (and, for a system-scope caller, to see EVERY admin-tier group
 * as a service-admin-group candidate) is attached only after this.
 *
 * As of 2026-08-12 (commit `9267344`) the step-up control lives INSIDE the
 * user dropdown, not as a standalone sidebar button -- see
 * SystemAdminModeControl.tsx + UserMenu.tsx (the trigger's accessible name is
 * the logged-in user's display name; the control renders as `menuitem`s
 * above "Profil"). Mirrors the working reference in
 * e2e-certificates/certificates.spec.ts: open the dropdown, click the enter
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

/**
 * Leaves System-Admin mode. Needed between the one-off group-bootstrap step
 * (which must run elevated) and the pre-existing service/invite flows below:
 * a system-scope caller's service-admin-group-candidates is EVERY admin-tier
 * group system-wide (including the migration-seeded, memberless "Standard"
 * group under its own "Standard" system group), which would make the
 * bootstrap admin's candidate set span >1 distinct parent the instant she
 * creates her own group too -- breaking the "exactly one candidate ->
 * auto-selected, no picker at all" assumption the two ORIGINAL (unmodified)
 * tests below rely on. Un-elevating drops her back to the plain-admin
 * branch (own groups only: just the one she created — ownership is a
 * persisted DB fact, unaffected by the session's later elevation state), so
 * the single-candidate auto-select holds again.
 *
 * As of 2026-08-12 (commit `9267344`) "leave" is a `menuitem` inside the same
 * user dropdown "enter" lives in (see enterSystemAdminMode above) -- open the
 * dropdown, click leave (no password step for leaving), then re-open the
 * dropdown to confirm the "enter" item is back (elevation dropped) and close
 * it again (Escape) so the page is left in its normal closed-menu state for
 * whatever runs next.
 */
async function exitSystemAdminMode(page: Page, displayName: string): Promise<void> {
  await page.getByRole("button", { name: displayName }).click();
  await page.getByRole("menuitem", { name: t.systemAdminModeLeave }).click();
  await page.getByRole("button", { name: displayName }).click();
  await expect(page.getByRole("menuitem", { name: t.systemAdminModeEnter })).toBeVisible();
  await page.keyboard.press("Escape");
}

/** Creates a system-tier group (system_admin only; no parent). */
async function createSystemGroup(page: Page, name: string): Promise<void> {
  await page.getByRole("button", { name: t.groupsCreateSystemTitle }).click();
  await page.locator("#group-name").fill(name);
  await page.getByRole("button", { name: t.save }).click();
  await expect(page.getByRole("row", { name: new RegExp(name) })).toBeVisible();
}

/**
 * Creates an admin-tier group with an EXPLICITLY chosen parent (mirrors
 * servers.spec.ts's own createAdminGroup verbatim -- see its comment: the
 * parent picker always renders as a real dropdown because migration v44
 * seeds a system-wide "Standard" system group alongside whatever this suite
 * creates, so `parentOptions.length` is never 1).
 */
async function createAdminGroup(page: Page, name: string, parentName: string): Promise<void> {
  await page.getByRole("button", { name: t.groupsCreateAdminTitle }).click();
  await page.locator("#group-name").fill(name);
  await page.getByRole("combobox", { name: t.groupsParentLabel }).click();
  await page.getByRole("option", { name: parentName, exact: true }).click();
  await page.getByRole("button", { name: t.save }).click();
  await expect(page.getByRole("row", { name: new RegExp(name) })).toBeVisible();
}

/**
 * Invites a user via the Users view, picking `adminGroupName` in the invite
 * form's mandatory admin-group picker (renders as a combobox once the actor
 * manages more than one admin group). Returns the one-time invite URL + the
 * generated email.
 */
async function inviteWithAdminGroup(
  adminPage: Page,
  opts: { role: "user" | "admin"; adminGroupName: string; displayName: string }
): Promise<{ email: string; inviteUrl: string }> {
  await adminPage.getByRole("link", { name: t.users }).click();
  await adminPage.getByRole("button", { name: t.userCreate }).click();
  const email = uniqueEmail();
  await adminPage.locator("#user-email").fill(email);
  await adminPage.locator("#user-name").fill(opts.displayName);
  if (opts.role === "admin") {
    await adminPage.getByRole("combobox", { name: t.tableRole }).click();
    await adminPage.getByRole("option", { name: t.roleAdmin, exact: true }).click();
  }
  await adminPage.getByRole("combobox", { name: t.userInviteAdminGroupLabel }).click();
  await adminPage.getByRole("option", { name: opts.adminGroupName, exact: true }).click();
  await adminPage.getByRole("button", { name: t.userCreate }).click();
  const inviteUrl = (await adminPage.locator('[data-testid="secret-reveal"] code').innerText()).trim();
  await adminPage.getByRole("button", { name: t.captureClose }).click();
  return { email, inviteUrl };
}

/** Redeems a set-password invite in a FRESH, isolated browser context. */
async function redeemInvite(browser: Browser, inviteUrl: string): Promise<Page> {
  const context = await browser.newContext({ baseURL: "http://127.0.0.1:4173" });
  const page = await context.newPage();
  await setPassword(page, inviteUrl, SCOPE_USER_PASSWORD);
  await page.goto("/portal/");
  await expect(page.getByText(t.welcome)).toBeVisible();
  return page;
}

/**
 * Opens the "Mitglieder verwalten" sub-view for the admin-tier group row
 * named `groupName`, scoped to the Admin-Gruppen table (system_admin's
 * landscape also renders the System-tier table alongside it).
 */
async function openMembers(page: Page, groupName: string): Promise<void> {
  const region = page.getByRole("region", { name: t.groupsAdminTitle });
  const row = region.getByRole("row", { name: new RegExp(groupName) });
  await row.getByRole("button", { name: t.groupsActionMembers }).click();
}

/**
 * Adds an EXISTING candidate directly to the currently-open admin-tier
 * members sub-view (no invite/accept step at that tier).
 */
async function addCandidate(page: Page, memberName: string): Promise<void> {
  await page.getByRole("combobox", { name: t.groupsAddMembersLabel }).click();
  await page.getByRole("option", { name: memberName }).click();
  await page.getByRole("button", { name: t.groupsActionAdd }).click();
}

/**
 * Promotes `memberName` to co-manager of the currently-open members sub-view
 * and narrows their flags to can_manage_services ONLY (unchecking the other
 * three, which a fresh promotion defaults to true alongside
 * can_manage_services -- Decision 7's "existing manager" bootstrap default).
 * This proves the flag is a genuinely independent capability: everything
 * this suite has A do with a SERVICE below must work off can_manage_services
 * alone, never off can_manage_users/can_manage_group/can_manage_servers
 * leaking it.
 */
async function promoteServicesOnly(page: Page, memberName: string): Promise<void> {
  const row = page.getByRole("row", { name: new RegExp(memberName) });
  await expect(row).toBeVisible();
  await row.getByRole("button", { name: t.groupsActionPromote }).click();
  await expect(row.getByText(t.groupsRoleManager)).toBeVisible();
  const usersCheckbox = page.getByRole("checkbox", { name: `${t.groupsPermUsers} – ${memberName}` });
  const groupCheckbox = page.getByRole("checkbox", { name: `${t.groupsPermGroup} – ${memberName}` });
  const serversCheckbox = page.getByRole("checkbox", { name: `${t.groupsPermServers} – ${memberName}` });
  const servicesCheckbox = page.getByRole("checkbox", { name: `${t.groupsPermServices} – ${memberName}` });
  await expect(usersCheckbox).toBeChecked();
  await expect(groupCheckbox).toBeChecked();
  await expect(serversCheckbox).toBeChecked();
  await expect(servicesCheckbox).toBeChecked();
  await usersCheckbox.click();
  await expect(usersCheckbox).not.toBeChecked();
  await groupCheckbox.click();
  await expect(groupCheckbox).not.toBeChecked();
  await serversCheckbox.click();
  await expect(serversCheckbox).not.toBeChecked();
  // Never touched -- still checked, carried over verbatim by every PATCH
  // above -- proving can_manage_services is a genuinely INDEPENDENT flag.
  await expect(servicesCheckbox).toBeChecked();
}

/**
 * Opens the create-service form, fills the name, optionally narrows to one
 * system group first, picks EXACTLY ONE admin group in the (required,
 * multi-select) admin-group picker, submits, and waits for the new row.
 *
 * `systemGroupName` is needed ONLY for a system-scope caller in this suite
 * (mirrors servers.spec.ts's createServer exactly): a system-scope caller's
 * candidate set is EVERY admin-tier group system-wide, which by this point
 * spans several distinct parents (the seeded "Standard" group's parent, the
 * suite's own BASELINE_SG, and SG_NAME) -- so the create form requires
 * picking one first. A (a non-system caller) only ever manages groups under
 * SG_NAME, so her candidates share exactly one parent and this step never
 * renders for her -- omit `systemGroupName` in that case.
 */
async function createService(
  page: Page,
  opts: { name: string; systemGroupName?: string; adminGroupName: string }
): Promise<void> {
  await page.getByRole("button", { name: t.serviceCreate }).click();
  await page.locator("#service-name").fill(opts.name);
  if (opts.systemGroupName) {
    await page.getByRole("combobox", { name: t.serviceAdminGroupSystemGroupLabel }).click();
    await page.getByRole("option", { name: opts.systemGroupName, exact: true }).click();
  }
  await page.getByRole("combobox", { name: t.serviceAdminGroupLabel }).click();
  await page.getByRole("option", { name: opts.adminGroupName, exact: true }).click();
  await page.getByRole("button", { name: t.serviceCreate }).click();
  await expect(page.getByRole("row", { name: new RegExp(opts.name) })).toBeVisible();
}

/** Opens the detail sub-view for the service row named `serviceName`. */
async function openServiceDetail(page: Page, serviceName: string): Promise<void> {
  const row = page.getByRole("row", { name: new RegExp(serviceName) });
  await row.getByRole("button", { name: t.modelDetailsAction }).click();
}

/**
 * Clicks the "Admin-Gruppen speichern" button and waits for the underlying
 * PUT .../admin-groups response, returning its parsed body -- a stronger
 * assertion than the (also-checked) success toast, since it proves the
 * PERSISTED linkage rather than just that a request was fired.
 */
async function saveAdminGroupsAndWait(page: Page): Promise<{ admin_groups: { id: string; name: string }[] }> {
  const [resp] = await Promise.all([
    page.waitForResponse(
      (r) => /\/api\/portal\/services\/[^/]+\/admin-groups$/.test(new URL(r.url()).pathname) && r.request().method() === "PUT"
    ),
    page.getByRole("button", { name: t.serviceAdminGroupsSave }).click()
  ]);
  expect(resp.ok(), `expected success, got ${resp.status()}: ${await resp.text()}`).toBe(true);
  return (await resp.json()) as { admin_groups: { id: string; name: string }[] };
}

/**
 * Removes a chip labelled `label` from a MultiSelectField (MUI Autocomplete)
 * scoped to `scope` -- clicks the chip's own delete icon.
 */
async function removeChip(scope: Locator, label: string): Promise<void> {
  const chip = scope.locator(".MuiChip-root").filter({ hasText: label });
  await chip.locator(".MuiChip-deleteIcon").click();
}

test.describe("Service Accounts (Phase 1) — end-to-end security guarantees", () => {
  // Runs FIRST (declaration order, workers:1/fullyParallel:false — see
  // playwright.services.config.ts): creates the ONE admin group both
  // original tests below need in order for their create-service /
  // invite-user forms to have an auto-selectable candidate at all. See the
  // BASELINE_SG/BASELINE_AG comment above.
  test("setup: bootstrap admin creates one admin group for the create-service/invite-user forms", async ({ page }) => {
    await login(page, SERVICES_ADMIN_EMAIL, SERVICES_ADMIN_PASSWORD);
    await enterSystemAdminMode(page, SERVICES_ADMIN_NAME, SERVICES_ADMIN_PASSWORD);
    await gotoGroups(page);
    await createSystemGroup(page, BASELINE_SG);
    await createAdminGroup(page, BASELINE_AG, BASELINE_SG);
    await exitSystemAdminMode(page, SERVICES_ADMIN_NAME);
  });

  test("a service token can invoke its allowlisted model, is refused a non-allowlisted model, cannot reach the portal, and is revoked when its service is disabled", async ({
    page,
    request
  }) => {
    await login(page, SERVICES_ADMIN_EMAIL, SERVICES_ADMIN_PASSWORD);

    const { secret } = await createServiceWithToken(page, {
      name: "E2E Batch Service",
      allowedModel: ALLOWED_MODEL
    });

    // (1) The allowlisted model is NOT rejected by the auth/allowlist gate.
    // The mock provider actually completes the request, so we expect a real
    // 200 with the mock's echoed text — the strongest possible proof that
    // the token was accepted, the model was allowed, AND the request
    // reached routing/the upstream.
    const okResp = await request.post(`${GW}/v1/chat/completions`, {
      headers: { Authorization: `Bearer ${secret}`, "Content-Type": "application/json" },
      data: { model: ALLOWED_MODEL, messages: [{ role: "user", content: "hello from a service token" }], stream: false }
    });
    expect(okResp.status()).toBe(200);
    const okBody = await okResp.json();
    expect(okBody.choices?.[0]?.message?.content).toContain(`Mock response for ${ALLOWED_MODEL}`);

    // (2) A model outside the allowlist is refused with 403 model.not_allowed
    // — before ever reaching routing/the upstream.
    const deniedResp = await request.post(`${GW}/v1/chat/completions`, {
      headers: { Authorization: `Bearer ${secret}`, "Content-Type": "application/json" },
      data: { model: OTHER_MODEL, messages: [{ role: "user", content: "should be refused" }], stream: false }
    });
    expect(deniedResp.status()).toBe(403);
    const deniedBody = await deniedResp.json();
    expect(deniedBody.error?.code).toBe("model.not_allowed");

    // (3) The SAME token can never reach the portal API — llm:invoke is
    // LLM-only; a service-kind bearer is rejected regardless of scope.
    const portalResp = await request.get(`${GW}/api/portal/services`, {
      headers: { Authorization: `Bearer ${secret}` }
    });
    expect([401, 403]).toContain(portalResp.status());
    const portalBody = await portalResp.json();
    expect(portalBody.error?.code).toBe("auth.invalid_token");

    // (4) Disabling the service revokes the token immediately (no caching):
    // the list row's toggle action flips status active -> disabled.
    const row = page.getByRole("row", { name: /E2E Batch Service/ });
    await row.getByRole("button", { name: t.tokenActionDisable }).click();
    await expect(row.getByText(t.statusDisabled)).toBeVisible();

    const revokedResp = await request.post(`${GW}/v1/chat/completions`, {
      headers: { Authorization: `Bearer ${secret}`, "Content-Type": "application/json" },
      data: { model: ALLOWED_MODEL, messages: [{ role: "user", content: "should now be rejected" }], stream: false }
    });
    expect(revokedResp.status()).toBe(401);
  });

  // Bonus: prove delegate ASSIGNMENT works end-to-end through the UI (the
  // delegate authorization gate matrix itself — what a Token- vs Full-Delegate
  // may/may not do — is already unit-tested in ServicesView.test.tsx, so this
  // only checks that adding a delegate at service-create time round-trips).
  test("a full delegate can be assigned to a service at creation time", async ({ page }) => {
    await login(page, SERVICES_ADMIN_EMAIL, SERVICES_ADMIN_PASSWORD);

    // Invite a second user so there is a real candidate for the delegate
    // picker. inviteUser always names the invitee "E2E User" (see
    // e2e/helpers.ts) — that display name is what the delegate picker (and
    // later the delegates column) shows.
    await inviteUser(page, { role: "user" });

    await page.getByRole("link", { name: t.services }).click();
    await page.getByRole("button", { name: t.serviceCreate }).click();
    await page.locator("#service-name").fill("E2E Delegated Service");

    // Add the invited user as a Full Delegate.
    await page.getByRole("combobox", { name: t.serviceDelegatesAddLabel }).click();
    await page.getByRole("option", { name: "E2E User", exact: true }).click();
    await page.getByRole("combobox", { name: t.serviceDelegatesLabel, exact: true }).click();
    await page.getByRole("option", { name: t.serviceDelegatesFullGroup }).click();
    await page.getByRole("button", { name: t.serviceDelegatesAdd }).click();

    await page.getByRole("button", { name: t.serviceCreate }).click();

    // Back on the list: the delegates column shows the invited user for the
    // newly created service row.
    const row = page.getByRole("row", { name: /E2E Delegated Service/ });
    await expect(row).toBeVisible();
    await expect(row).toContainText("E2E User");
  });
});

test.describe("Admin-group permissions Phase C — group-scoped service management", () => {
  test("can_manage_services co-manager creates/manages a service via its admin group; a service linked to a DIFFERENT admin group they don't manage is 404-no-leak; system_admin sees all; add/remove linkage", async ({
    page: systemAdminPage,
    browser
  }) => {
    // --- Setup: one system group, three admin groups under it ----------------
    await login(systemAdminPage, SERVICES_ADMIN_EMAIL, SERVICES_ADMIN_PASSWORD);
    await enterSystemAdminMode(systemAdminPage, SERVICES_ADMIN_NAME, SERVICES_ADMIN_PASSWORD);

    await gotoGroups(systemAdminPage);
    await createSystemGroup(systemAdminPage, SG_NAME);
    await createAdminGroup(systemAdminPage, AG_ONE, SG_NAME);
    await createAdminGroup(systemAdminPage, AG_TWO, SG_NAME);
    await createAdminGroup(systemAdminPage, AG_OUT, SG_NAME);

    // --- Invite A as a plain admin-tier member of AG_ONE ----------------------
    const inviteA = await inviteWithAdminGroup(systemAdminPage, {
      role: "admin",
      adminGroupName: AG_ONE,
      displayName: ADMIN_A_NAME
    });
    const aPage = await redeemInvite(browser, inviteA.inviteUrl);

    // --- Add A directly to AG_TWO too (containment holds: A is already a
    // member of SG_NAME, AG_TWO's parent, via the AG_ONE invite above) --------
    await gotoGroups(systemAdminPage);
    await openMembers(systemAdminPage, AG_TWO);
    await addCandidate(systemAdminPage, ADMIN_A_NAME);
    await expect(systemAdminPage.getByText(ADMIN_A_NAME)).toBeVisible();

    // --- Promote A to co-manager of AG_ONE and AG_TWO, can_manage_services
    // ONLY on each (can_manage_users/can_manage_group/can_manage_servers all
    // narrowed false) -----------------------------------------------------------
    await systemAdminPage.getByRole("button", { name: t.back }).click();
    await openMembers(systemAdminPage, AG_ONE);
    await promoteServicesOnly(systemAdminPage, ADMIN_A_NAME);

    await systemAdminPage.getByRole("button", { name: t.back }).click();
    await openMembers(systemAdminPage, AG_TWO);
    await promoteServicesOnly(systemAdminPage, ADMIN_A_NAME);

    // A is left a PLAIN member of neither group -- AG_OUT is untouched
    // (system_admin remains its sole owner+member) -- the deliberate "A
    // manages nothing there" case.

    // --- service-admin-group-candidates: A sees AG_ONE + AG_TWO, never AG_OUT,
    // never BASELINE_AG (owned by system_admin, not A) -------------------------
    const candidatesResp = await aPage.request.get("/api/portal/service-admin-group-candidates");
    expect(candidatesResp.ok(), `expected success, got ${candidatesResp.status()}: ${await candidatesResp.text()}`).toBe(true);
    const candidatesBody = (await candidatesResp.json()) as { data: AdminGroupCandidate[] };
    const candidateNames = candidatesBody.data.map((c) => c.name);
    expect(candidateNames).toContain(AG_ONE);
    expect(candidateNames).toContain(AG_TWO);
    expect(candidateNames).not.toContain(AG_OUT);
    expect(candidateNames).not.toContain(BASELINE_AG);

    // --- A creates a service into AG_ONE (its containment root, SG_NAME, is
    // auto-derived server-side from the group's own parent) -------------------
    await gotoServices(aPage);
    await createService(aPage, { name: SERVICE_ALPHA, adminGroupName: AG_ONE });

    const listAsAAfterAlpha = (await (await aPage.request.get("/api/portal/services")).json()) as ServiceListBody;
    const alpha = listAsAAfterAlpha.data.find((s) => s.name === SERVICE_ALPHA);
    expect(alpha, `expected ${SERVICE_ALPHA} in A's service list: ${JSON.stringify(listAsAAfterAlpha)}`).toBeTruthy();
    const alphaId = alpha!.id;
    expect(alpha!.admin_groups.map((g) => g.name)).toEqual([AG_ONE]);
    expect(alpha!.system_group_name).toBe(SG_NAME);
    expect(listAsAAfterAlpha.data.some((s) => s.name === SERVICE_BETA)).toBe(false);

    // --- A can MANAGE Alpha too (authorizeServiceSettings's group branch
    // mirrors authorizeServiceRead's — "the group grant is FULL"): the
    // row-level disable/enable toggle is gated on canEditSettings AND
    // actually calls UpdateService (the *Settings* object-gate) -- a mere
    // read-only visitor would never see this row action at all. -------------
    const alphaRow = aPage.getByRole("row", { name: new RegExp(SERVICE_ALPHA) });
    await alphaRow.getByRole("button", { name: t.tokenActionDisable }).click();
    await expect(alphaRow.getByText(t.statusDisabled)).toBeVisible();
    await alphaRow.getByRole("button", { name: t.tokenActionEnable }).click();
    await expect(alphaRow.getByText(t.statusActive)).toBeVisible();

    // --- system_admin creates a SECOND service into AG_OUT (a group A does
    // not manage) -- system_admin's candidate set spans several groups (the
    // seeded "Standard" group, BASELINE_AG, and the three under SG_NAME), so
    // the picker requires picking a system group first ------------------------
    await gotoServices(systemAdminPage);
    await createService(systemAdminPage, { name: SERVICE_BETA, systemGroupName: SG_NAME, adminGroupName: AG_OUT });

    const listAsSystemAfterBeta = (await (await systemAdminPage.request.get("/api/portal/services")).json()) as ServiceListBody;
    const beta = listAsSystemAfterBeta.data.find((s) => s.name === SERVICE_BETA);
    expect(beta, `expected ${SERVICE_BETA} in system_admin's service list: ${JSON.stringify(listAsSystemAfterBeta)}`).toBeTruthy();
    const betaId = beta!.id;
    expect(beta!.admin_groups.map((g) => g.name)).toEqual([AG_OUT]);
    // system_admin sees ALL services, regardless of group scoping.
    expect(listAsSystemAfterBeta.data.some((s) => s.name === SERVICE_ALPHA)).toBe(true);

    // --- A does NOT see Beta -- neither via the UI list nor a raw fetch -------
    await aPage.getByRole("link", { name: t.dashboard }).click();
    await gotoServices(aPage);
    await expect(aPage.getByRole("row", { name: new RegExp(SERVICE_ALPHA) })).toBeVisible();
    await expect(aPage.getByRole("row", { name: new RegExp(SERVICE_BETA) })).toHaveCount(0);
    const listAsAAfterBeta = (await (await aPage.request.get("/api/portal/services")).json()) as ServiceListBody;
    expect(listAsAAfterBeta.data.some((s) => s.name === SERVICE_BETA)).toBe(false);

    // --- 404-no-leak: a raw GET, a raw settings PUT, and a raw admin-groups
    // PUT against Beta as A all fail with the SAME code a genuinely
    // non-existent id would produce (never 403 — no existence leak) ----------
    const getBetaAsA = await aPage.request.get(`/api/portal/services/${betaId}`);
    expect(getBetaAsA.status(), `expected 404, got ${getBetaAsA.status()}: ${await getBetaAsA.text()}`).toBe(404);
    expect((await getBetaAsA.json()).error?.code).toBe("service.not_found");

    const putBetaAsA = await aPage.request.put(`/api/portal/services/${betaId}`, {
      // Cookie-authenticated state-changing requests require the CSRF header
      // (internal/gateway/auth.go's csrfOK) -- a raw page.request bypasses
      // api.ts (which sets this on every mutating call) entirely.
      headers: { "X-OP-CSRF": "1" },
      data: { name: "should-not-apply" }
    });
    expect(putBetaAsA.status(), `expected 404, got ${putBetaAsA.status()}: ${await putBetaAsA.text()}`).toBe(404);
    expect((await putBetaAsA.json()).error?.code).toBe("service.not_found");

    const adminGroupsPutBetaAsA = await aPage.request.put(`/api/portal/services/${betaId}/admin-groups`, {
      headers: { "X-OP-CSRF": "1" },
      data: { admin_group_ids: [AG_ONE] }
    });
    expect(adminGroupsPutBetaAsA.status(), `expected 404, got ${adminGroupsPutBetaAsA.status()}: ${await adminGroupsPutBetaAsA.text()}`).toBe(
      404
    );
    expect((await adminGroupsPutBetaAsA.json()).error?.code).toBe("service.not_found");

    // The rejected rename + rejected re-linkage had no effect (Beta genuinely
    // unchanged: still named SERVICE_BETA, still linked only to AG_OUT).
    const listAfterRejected = (await (await systemAdminPage.request.get("/api/portal/services")).json()) as ServiceListBody;
    const betaAfterRejected = listAfterRejected.data.find((s) => s.id === betaId);
    expect(betaAfterRejected?.name).toBe(SERVICE_BETA);
    expect(betaAfterRejected?.admin_groups.map((g) => g.name)).toEqual([AG_OUT]);

    // --- system_admin sees ALL services (both Alpha and Beta) via the UI too --
    await systemAdminPage.getByRole("link", { name: t.dashboard }).click();
    await gotoServices(systemAdminPage);
    await expect(systemAdminPage.getByRole("row", { name: new RegExp(SERVICE_ALPHA) })).toBeVisible();
    await expect(systemAdminPage.getByRole("row", { name: new RegExp(SERVICE_BETA) })).toBeVisible();

    // --- Add/remove-linkage happy path: A edits Alpha's admin-group linkage --
    await aPage.getByRole("link", { name: t.dashboard }).click();
    await gotoServices(aPage);
    await openServiceDetail(aPage, SERVICE_ALPHA);
    const groupsSection = aPage.getByRole("region", { name: t.serviceAdminGroupsSectionTitle });
    await expect(groupsSection.getByText(AG_ONE)).toBeVisible();

    // Add AG_TWO (A manages it too, since its own can_manage_services promotion above).
    await groupsSection.getByRole("combobox", { name: t.serviceAdminGroupLabel }).click();
    await aPage.getByRole("option", { name: AG_TWO, exact: true }).click();
    let saved = await saveAdminGroupsAndWait(aPage);
    expect(saved.admin_groups.map((g) => g.name).sort()).toEqual([AG_ONE, AG_TWO].sort());
    await expect(aPage.getByText(t.serviceAdminGroupsSaved)).toBeVisible();

    // Remove AG_ONE via its chip's own delete icon -- leaves AG_TWO only.
    await removeChip(groupsSection, AG_ONE);
    saved = await saveAdminGroupsAndWait(aPage);
    expect(saved.admin_groups.map((g) => g.name)).toEqual([AG_TWO]);

    // Remove AG_TWO too -- the Save button disables client-side the instant
    // the set would go to zero (never even reaches the network).
    await removeChip(groupsSection, AG_TWO);
    await expect(groupsSection.getByRole("button", { name: t.serviceAdminGroupsSave })).toBeDisabled();

    // The backend enforces the same floor: a raw PUT with an empty set is
    // rejected 400 (the UI-blocked path, exercised directly).
    const emptyLinkage = await aPage.request.put(`/api/portal/services/${alphaId}/admin-groups`, {
      headers: { "X-OP-CSRF": "1" },
      data: { admin_group_ids: [] }
    });
    expect(emptyLinkage.status(), `expected 400, got ${emptyLinkage.status()}: ${await emptyLinkage.text()}`).toBe(400);
    expect((await emptyLinkage.json()).error?.code).toBe("service.admin_group_required");

    // Alpha's linkage is unaffected by the rejected call -- still AG_TWO
    // only, from the last SUCCESSFUL save above.
    const listAfterRejectedEmpty = (await (await systemAdminPage.request.get("/api/portal/services")).json()) as ServiceListBody;
    const alphaAfterRejectedEmpty = listAfterRejectedEmpty.data.find((s) => s.id === alphaId);
    expect(alphaAfterRejectedEmpty?.admin_groups.map((g) => g.name)).toEqual([AG_TWO]);
  });
});
