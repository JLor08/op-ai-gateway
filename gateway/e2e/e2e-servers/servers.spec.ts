// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { expect, test, type Browser, type Locator, type Page } from "@playwright/test";
import { messages } from "../../frontend/src/i18n";
import { login, setPassword, uniqueEmail } from "../e2e/helpers";
import { SERVERS_ADMIN_EMAIL, SERVERS_ADMIN_NAME, SERVERS_ADMIN_PASSWORD } from "../playwright.servers.config";

const t = messages.de;

// >= 10 chars per the password policy in internal/auth. Test-only.
const USER_PASSWORD = "E2E-Servers-Pass-1";

// Distinct, non-substring-colliding names (see groups.spec.ts's identical
// naming note): several helpers below locate a row via `getByRole("row",
// { name: new RegExp(name) })`, a SUBSTRING match against the row's whole
// accessible text -- so no name here may be a literal substring of another.
const SG_NAME = "E2E-SB-System"; // the ONE system group every admin group below hangs under
const AG_ONE = "E2E-SB-Bravo"; // A co-manages this one (can_manage_servers only)
const AG_TWO = "E2E-SB-Charlie"; // A ALSO co-manages this one -- used for the add/remove test
const AG_OUT = "E2E-SB-Delta"; // A is never added here -- proves the 404-no-leak scoping
const ADMIN_A_NAME = "E2E-SB-Coordinator";
const SERVER_ALPHA = "E2E-SB-Server-Alpha"; // A creates this one, into AG_ONE
const SERVER_BETA = "E2E-SB-Server-Beta"; // system_admin creates this one, into AG_OUT

/** Server DTO shape as returned by the portal (see internal/portal/service.go ServerDTO). */
type ServerRow = {
  id: string;
  name: string;
  admin_groups: { id: string; name: string }[];
  system_group_id: string;
  system_group_name: string;
};
type ServerListBody = { data: ServerRow[] };
type AdminGroupCandidate = { id: string; name: string; parent_group_id: string; parent_group_name: string };

/**
 * Navigates to the Groups view and waits for the landscape fetch to complete
 * (mirrors groups.spec.ts's gotoGroups -- GroupsView's landscape fetch opts
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
 * Navigates to the Servers view and waits for the server-admin-group-
 * candidates fetch to land (a mount effect of ServerList, gated on the
 * caller being admin+) -- so the create-form's admin-group picker and the
 * edit-form's admin-groups editor render against a resolved candidate list
 * rather than racing an in-flight fetch (the exact race task-5-report calls
 * out for the ServerList unit tests).
 */
async function gotoServers(page: Page): Promise<void> {
  const [resp] = await Promise.all([
    page.waitForResponse(
      (r) => new URL(r.url()).pathname === "/api/portal/server-admin-group-candidates" && r.request().method() === "GET"
    ),
    page.getByRole("link", { name: t.servers }).click()
  ]);
  expect(resp.ok()).toBe(true);
}

/**
 * Enters System-Admin mode (step-up) for the bootstrap system_admin -- a
 * fresh session is NOT elevated by default (spec: 2026-08-10-system-admin-
 * step-up-mode-design.md), and the `system` scope this suite needs (to
 * create System/Admin-tier groups, and to see every admin group as a server-
 * admin-group candidate) is attached only after this.
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

/** Creates a system-tier group (system_admin only; no parent). */
async function createSystemGroup(page: Page, name: string): Promise<void> {
  await page.getByRole("button", { name: t.groupsCreateSystemTitle }).click();
  await page.locator("#group-name").fill(name);
  await page.getByRole("button", { name: t.save }).click();
  await expect(page.getByRole("row", { name: new RegExp(name) })).toBeVisible();
}

/**
 * Creates an admin-tier group with an EXPLICITLY chosen parent. The parent
 * picker always renders as a real dropdown here even though this suite only
 * creates ONE system group of its own: migration v44 seeds a system-wide
 * "Standard" system group (`store.DefaultSystemGroupID`) that a system-scope
 * caller's `landscape.system` always includes alongside it, so
 * `parentOptions.length` is never 1 in this suite -- the auto-select note
 * never applies (mirrors groups.spec.ts's own `createAdminGroupWithParent`,
 * which never relies on auto-selection either, for the same reason).
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
 * manages more than one admin group -- true here, the suite creates 3 before
 * its first invite). Returns the one-time invite URL + generated email.
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
  await setPassword(page, inviteUrl, USER_PASSWORD);
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
 * members sub-view (no invite/accept step at that tier -- see groups.spec.ts).
 * Non-exact (substring) option match: the candidate option's accessible name
 * concatenates the display name and email with no separator.
 */
async function addCandidate(page: Page, memberName: string): Promise<void> {
  await page.getByRole("combobox", { name: t.groupsAddMembersLabel }).click();
  await page.getByRole("option", { name: memberName }).click();
  await page.getByRole("button", { name: t.groupsActionAdd }).click();
}

/**
 * Promotes `memberName` to co-manager of the currently-open members sub-view
 * and narrows their flags to can_manage_servers ONLY (unchecking the other
 * two, which a fresh promotion defaults to true alongside can_manage_servers
 * -- Decision 7's "existing manager" bootstrap default). This proves the new
 * flag is a genuinely independent capability: everything this suite has A do
 * with a server must work off can_manage_servers alone, never off
 * can_manage_users/can_manage_group leaking it.
 */
async function promoteServersOnly(page: Page, memberName: string): Promise<void> {
  const row = page.getByRole("row", { name: new RegExp(memberName) });
  await expect(row).toBeVisible();
  await row.getByRole("button", { name: t.groupsActionPromote }).click();
  await expect(row.getByText(t.groupsRoleManager)).toBeVisible();
  const usersCheckbox = page.getByRole("checkbox", { name: `${t.groupsPermUsers} – ${memberName}` });
  const groupCheckbox = page.getByRole("checkbox", { name: `${t.groupsPermGroup} – ${memberName}` });
  const serversCheckbox = page.getByRole("checkbox", { name: `${t.groupsPermServers} – ${memberName}` });
  await expect(usersCheckbox).toBeChecked();
  await expect(groupCheckbox).toBeChecked();
  await expect(serversCheckbox).toBeChecked();
  await usersCheckbox.click();
  await expect(usersCheckbox).not.toBeChecked();
  await groupCheckbox.click();
  await expect(groupCheckbox).not.toBeChecked();
  // Never touched -- still checked, carried over verbatim by both PATCHes above.
  await expect(serversCheckbox).toBeChecked();
}

/**
 * Opens the create-server form, fills name/domain, optionally narrows to one
 * system group first, picks EXACTLY ONE admin group in the (required,
 * multi-select) admin-group picker, submits, and waits for the new row.
 *
 * `systemGroupName` is needed ONLY for a system-scope caller in this suite:
 * system_admin's candidate set is EVERY admin-tier group, which also
 * includes the seeded "Standard" admin group (parented under the seeded
 * "Standard" system group, NOT SG_NAME) -- so system_admin's candidates span
 * TWO distinct system groups and the create form requires picking one
 * first (ServerList.tsx's `createDistinctSystemGroups.length > 1` branch)
 * before the admin-group multi-select narrows to that group's children. A
 * (a non-system caller) only ever manages groups under SG_NAME, so her
 * candidates share exactly one parent and this step never renders for her
 * -- omit `systemGroupName` in that case.
 */
async function createServer(
  page: Page,
  opts: { name: string; domain: string; systemGroupName?: string; adminGroupName: string }
): Promise<void> {
  await page.getByRole("button", { name: t.serverCreate }).click();
  await page.locator("#server-name").fill(opts.name);
  await page.locator("#server-domain").fill(opts.domain);
  if (opts.systemGroupName) {
    await page.getByRole("combobox", { name: t.serverAdminGroupSystemGroupLabel }).click();
    await page.getByRole("option", { name: opts.systemGroupName, exact: true }).click();
  }
  await page.getByRole("combobox", { name: t.serverAdminGroupLabel }).click();
  await page.getByRole("option", { name: opts.adminGroupName, exact: true }).click();
  await page.getByRole("button", { name: t.serverCreate }).click();
  await expect(page.getByRole("row", { name: new RegExp(opts.name) })).toBeVisible();
}

/** Opens the edit-form for the server row named `serverName`. */
async function openEditServer(page: Page, serverName: string): Promise<void> {
  const row = page.getByRole("row", { name: new RegExp(serverName) });
  await row.getByRole("button", { name: t.serverActionEdit }).click();
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
      (r) => /\/api\/portal\/servers\/[^/]+\/admin-groups$/.test(new URL(r.url()).pathname) && r.request().method() === "PUT"
    ),
    page.getByRole("button", { name: t.serverAdminGroupsSave }).click()
  ]);
  expect(resp.ok(), `expected success, got ${resp.status()}: ${await resp.text()}`).toBe(true);
  return (await resp.json()) as { admin_groups: { id: string; name: string }[] };
}

/**
 * Removes a chip labelled `label` from a MultiSelectField (MUI Autocomplete)
 * scoped to `scope` -- clicks the chip's own delete icon (MUI's default
 * `MuiChip-deleteIcon`), mirroring how ServerList.test.tsx exercises the same
 * removal (`.querySelector("svg")` on the chip's own element).
 */
async function removeChip(scope: Locator, label: string): Promise<void> {
  const chip = scope.locator(".MuiChip-root").filter({ hasText: label });
  await chip.locator(".MuiChip-deleteIcon").click();
}

test.describe("Admin-group permissions Phase B — group-scoped server management", () => {
  test("can_manage_servers co-manager creates/manages a server via its admin group; a server in a group they don't manage is 404-no-leak; system_admin sees all; add/remove linkage", async ({
    page: systemAdminPage,
    browser
  }) => {
    // --- Setup: one system group, three admin groups under it ----------------
    await login(systemAdminPage, SERVERS_ADMIN_EMAIL, SERVERS_ADMIN_PASSWORD);
    await enterSystemAdminMode(systemAdminPage, SERVERS_ADMIN_NAME, SERVERS_ADMIN_PASSWORD);

    await gotoGroups(systemAdminPage);
    await createSystemGroup(systemAdminPage, SG_NAME);
    await createAdminGroup(systemAdminPage, AG_ONE, SG_NAME);
    await createAdminGroup(systemAdminPage, AG_TWO, SG_NAME);
    await createAdminGroup(systemAdminPage, AG_OUT, SG_NAME);

    // --- Invite A as a plain admin-tier member of AG_ONE ----------------------
    // (system_admin owns all three groups so far -- the invite's admin-group
    // picker renders as a real dropdown since it manages > 1 group.)
    const inviteA = await inviteWithAdminGroup(systemAdminPage, { role: "admin", adminGroupName: AG_ONE, displayName: ADMIN_A_NAME });
    const aPage = await redeemInvite(browser, inviteA.inviteUrl);

    // --- Add A directly to AG_TWO too (containment holds: A is already a
    // member of SG_NAME, AG_TWO's parent, via the AG_ONE invite above) --------
    await gotoGroups(systemAdminPage);
    await openMembers(systemAdminPage, AG_TWO);
    await addCandidate(systemAdminPage, ADMIN_A_NAME);
    await expect(systemAdminPage.getByText(ADMIN_A_NAME)).toBeVisible();

    // --- Promote A to co-manager of AG_ONE and AG_TWO, can_manage_servers
    // ONLY on each (can_manage_users/can_manage_group both narrowed false) ----
    // AG_ONE: re-open members fresh (the AG_TWO members view above is still open).
    await systemAdminPage.getByRole("button", { name: t.back }).click();
    await openMembers(systemAdminPage, AG_ONE);
    await promoteServersOnly(systemAdminPage, ADMIN_A_NAME);

    await systemAdminPage.getByRole("button", { name: t.back }).click();
    await openMembers(systemAdminPage, AG_TWO);
    await promoteServersOnly(systemAdminPage, ADMIN_A_NAME);

    // A is left a PLAIN member of neither group -- AG_OUT is untouched (system_admin
    // remains its sole owner+member) -- the deliberate "A manages nothing there" case.

    // --- server-admin-group-candidates: A sees AG_ONE + AG_TWO, never AG_OUT --
    const candidatesResp = await aPage.request.get("/api/portal/server-admin-group-candidates");
    expect(candidatesResp.ok(), `expected success, got ${candidatesResp.status()}: ${await candidatesResp.text()}`).toBe(true);
    const candidatesBody = (await candidatesResp.json()) as { data: AdminGroupCandidate[] };
    const candidateNames = candidatesBody.data.map((c) => c.name);
    expect(candidateNames).toContain(AG_ONE);
    expect(candidateNames).toContain(AG_TWO);
    expect(candidateNames).not.toContain(AG_OUT);

    // --- A creates a server into AG_ONE (its containment root, SG_NAME, is
    // auto-derived server-side from the group's own parent) -------------------
    await gotoServers(aPage);
    await createServer(aPage, { name: SERVER_ALPHA, domain: "e2e-sb-alpha.example.test", adminGroupName: AG_ONE });

    const listAsAAfterAlpha = (await (await aPage.request.get("/api/portal/servers")).json()) as ServerListBody;
    const alpha = listAsAAfterAlpha.data.find((s) => s.name === SERVER_ALPHA);
    expect(alpha, `expected ${SERVER_ALPHA} in A's server list: ${JSON.stringify(listAsAAfterAlpha)}`).toBeTruthy();
    const alphaId = alpha!.id;
    expect(alpha!.admin_groups.map((g) => g.name)).toEqual([AG_ONE]);
    expect(alpha!.system_group_name).toBe(SG_NAME);
    expect(listAsAAfterAlpha.data.some((s) => s.name === SERVER_BETA)).toBe(false);

    // --- system_admin creates a SECOND server into AG_OUT (a group A does not
    // manage) -- system_admin's candidate set spans all 3 groups (one shared
    // parent), so the picker is a required multi-select too ------------------
    await gotoServers(systemAdminPage);
    await createServer(systemAdminPage, {
      name: SERVER_BETA,
      domain: "e2e-sb-beta.example.test",
      systemGroupName: SG_NAME,
      adminGroupName: AG_OUT
    });

    const listAsSystemAfterBeta = (await (await systemAdminPage.request.get("/api/portal/servers")).json()) as ServerListBody;
    const beta = listAsSystemAfterBeta.data.find((s) => s.name === SERVER_BETA);
    expect(beta, `expected ${SERVER_BETA} in system_admin's server list: ${JSON.stringify(listAsSystemAfterBeta)}`).toBeTruthy();
    const betaId = beta!.id;
    expect(beta!.admin_groups.map((g) => g.name)).toEqual([AG_OUT]);
    // system_admin sees ALL servers, regardless of group scoping.
    expect(listAsSystemAfterBeta.data.some((s) => s.name === SERVER_ALPHA)).toBe(true);

    // --- A does NOT see Beta -- neither via the UI list nor a raw fetch -------
    await aPage.getByRole("link", { name: t.dashboard }).click();
    await gotoServers(aPage);
    await expect(aPage.getByRole("row", { name: new RegExp(SERVER_ALPHA) })).toBeVisible();
    await expect(aPage.getByRole("row", { name: new RegExp(SERVER_BETA) })).toHaveCount(0);
    const listAsAAfterBeta = (await (await aPage.request.get("/api/portal/servers")).json()) as ServerListBody;
    expect(listAsAAfterBeta.data.some((s) => s.name === SERVER_BETA)).toBe(false);

    // --- 404-no-leak: a raw GET and a raw PATCH against Beta as A both fail
    // with the SAME code a genuinely non-existent id would produce ------------
    const getBetaAsA = await aPage.request.get(`/api/portal/servers/${betaId}`);
    expect(getBetaAsA.status(), `expected 404, got ${getBetaAsA.status()}: ${await getBetaAsA.text()}`).toBe(404);
    expect((await getBetaAsA.json()).error?.code).toBe("server.not_found");

    const patchBetaAsA = await aPage.request.patch(`/api/portal/servers/${betaId}`, {
      // Cookie-authenticated state-changing requests require the CSRF header
      // (internal/gateway/auth.go's csrfOK) -- a raw page.request bypasses
      // api.ts (which sets this on every mutating call) entirely.
      headers: { "X-OP-CSRF": "1" },
      data: { name: "should-not-apply" }
    });
    expect(patchBetaAsA.status(), `expected 404, got ${patchBetaAsA.status()}: ${await patchBetaAsA.text()}`).toBe(404);
    expect((await patchBetaAsA.json()).error?.code).toBe("server.not_found");

    // The rejected rename had no effect (name genuinely unchanged).
    const listAfterRejectedRename = (await (await systemAdminPage.request.get("/api/portal/servers")).json()) as ServerListBody;
    expect(listAfterRejectedRename.data.some((s) => s.id === betaId && s.name === SERVER_BETA)).toBe(true);

    // --- system_admin sees ALL servers (both Alpha and Beta) via the UI too ---
    await systemAdminPage.getByRole("link", { name: t.dashboard }).click();
    await gotoServers(systemAdminPage);
    await expect(systemAdminPage.getByRole("row", { name: new RegExp(SERVER_ALPHA) })).toBeVisible();
    await expect(systemAdminPage.getByRole("row", { name: new RegExp(SERVER_BETA) })).toBeVisible();

    // --- Add/remove-linkage happy path: A edits Alpha's admin-group linkage --
    await aPage.getByRole("link", { name: t.dashboard }).click();
    await gotoServers(aPage);
    await openEditServer(aPage, SERVER_ALPHA);
    const groupsSection = aPage.getByRole("region", { name: t.serverAdminGroupsSectionTitle });
    await expect(groupsSection.getByText(AG_ONE)).toBeVisible();

    // Add AG_TWO (A manages it too, since its own can_manage_servers promotion above).
    await groupsSection.getByRole("combobox", { name: t.serverAdminGroupLabel }).click();
    await aPage.getByRole("option", { name: AG_TWO, exact: true }).click();
    let saved = await saveAdminGroupsAndWait(aPage);
    expect(saved.admin_groups.map((g) => g.name).sort()).toEqual([AG_ONE, AG_TWO].sort());
    await expect(aPage.getByText(t.serverAdminGroupsSaved)).toBeVisible();

    // Remove AG_ONE via its chip's own delete icon -- leaves AG_TWO only.
    await removeChip(groupsSection, AG_ONE);
    saved = await saveAdminGroupsAndWait(aPage);
    expect(saved.admin_groups.map((g) => g.name)).toEqual([AG_TWO]);

    // Remove AG_TWO too -- the Save button disables client-side the instant the
    // set would go to zero (never even reaches the network).
    await removeChip(groupsSection, AG_TWO);
    await expect(groupsSection.getByRole("button", { name: t.serverAdminGroupsSave })).toBeDisabled();

    // The backend enforces the same floor: a raw PUT with an empty set is
    // rejected 400 (the UI-blocked path, exercised directly).
    const emptyLinkage = await aPage.request.put(`/api/portal/servers/${alphaId}/admin-groups`, {
      headers: { "X-OP-CSRF": "1" },
      data: { admin_group_ids: [] }
    });
    expect(emptyLinkage.status(), `expected 400, got ${emptyLinkage.status()}: ${await emptyLinkage.text()}`).toBe(400);
    expect((await emptyLinkage.json()).error?.code).toBe("server.admin_group_required");

    // Alpha's linkage is unaffected by the rejected call -- still AG_TWO only,
    // from the last SUCCESSFUL save above.
    const listAfterRejectedEmpty = (await (await systemAdminPage.request.get("/api/portal/servers")).json()) as ServerListBody;
    const alphaAfterRejectedEmpty = listAfterRejectedEmpty.data.find((s) => s.id === alphaId);
    expect(alphaAfterRejectedEmpty?.admin_groups.map((g) => g.name)).toEqual([AG_TWO]);
  });
});
