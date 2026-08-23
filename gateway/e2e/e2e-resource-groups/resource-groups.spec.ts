// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { expect, test, type APIRequestContext, type Browser, type Page } from "@playwright/test";
import { messages } from "../../frontend/src/i18n";
import { login, setPassword, uniqueEmail } from "../e2e/helpers";
import {
  RESOURCE_GROUPS_ADMIN_EMAIL,
  RESOURCE_GROUPS_ADMIN_NAME,
  RESOURCE_GROUPS_ADMIN_PASSWORD
} from "../playwright.resource-groups.config";

const t = messages.de;

// The gateway's own bound address (playwright.resource-groups.config.ts's
// OP_AI_GATEWAY_ADDR) -- used ONLY for the Task 8 bearer-token completion
// calls below, which must go through the cookie-less `request` fixture
// against this ABSOLUTE url (never `page.request`, whose baseURL is the
// portal preview server at :4173 and which would also silently attach
// whichever page's own session cookie, hijacking authenticateWeb onto the
// session-principal branch instead of the bearer path -- mirrors
// e2e-services/services.spec.ts's identical GW constant + its own
// request-fixture bearer calls).
const GW = "http://127.0.0.1:8091";

// >= 10 chars per the password policy in internal/auth. Test-only.
const USER_PASSWORD = "E2E-ResGrp-Pass-1";

// Distinct, non-substring-colliding names (see servers.spec.ts/services.spec.ts's
// identical naming note): several helpers/assertions below locate a row via
// `getByRole("row", { name: new RegExp(name) })`, a SUBSTRING match against
// the row's whole accessible text -- so no name here may be a literal
// substring of another.
const SG_NAME = "E2E-RG-System"; // the ONE system group RG_ALPHA's admin groups hang under
const SG_TWO = "E2E-RG-Zulu"; // a SECOND, unrelated system group -- used only for the cross-system-group mismatch server
const AG_ONE = "E2E-RG-Bravo"; // A co-manages this one (can_manage_resources only)
const AG_TWO = "E2E-RG-Charlie"; // A ALSO co-manages this one -- used for the add/remove-linkage test
const AG_OUT = "E2E-RG-Delta"; // A never manages this one -- RG_BETA's sole linkage, proves the 404-no-leak scoping
const AG_DIFF = "E2E-RG-Echo"; // under SG_TWO; A never joins it -- purely a vehicle for creating SERVER_DIFF
const ADMIN_A_NAME = "E2E-RG-Coordinator";
const SERVER_SAME = "E2E-RG-Server-Same"; // under SG_NAME, A is OWNER (not a co-manager) -- the "server they manage" case
const SERVER_DIFF = "E2E-RG-Server-Diff"; // under SG_TWO, A is also OWNER -- proves the system-group mismatch is independent of authorizeServer
const SERVER_OUT = "E2E-RG-Server-Out"; // under SG_NAME, A owns/manages NOTHING here -- the 404-no-leak-on-the-server case
const RG_ALPHA = "E2E-RG-Group-Alpha"; // A creates this one, into AG_ONE
const RG_BETA = "E2E-RG-Group-Beta"; // system_admin creates this one, into AG_OUT (a group A never manages)

// --- Server-owner self-service scenario names (spec:
// 2026-08-11-resource-groups-server-owner-self-service). Distinct `E2E-RGO-*`
// prefix so nothing here collides (as a substring) with the `E2E-RG-*` names
// above -- the two tests share ONE serial sqlite-backed backend, so every group
// name persists into the Groups table the second test's helpers assert against.
const OWN_SG = "E2E-RGO-System"; // the ONE system group RG_OWN + servers X/Z live under
const OWN_SG2 = "E2E-RGO-Zulu"; // a SECOND system group -- the cross-system-group server Y lives under
const OWN_AG = "E2E-RGO-Alpha"; // U + V are MEMBERS (no co-manager flags); RG_OWN is linked to this
const OWN_AGX = "E2E-RGO-Charlie"; // W is a MEMBER; RG_OWN is NOT linked to this -- proves the owner-not-a-member 404
const OWN_AG2 = "E2E-RGO-Bravo"; // under OWN_SG2 -- purely a vehicle for creating server Y
const OWN_RG = "E2E-RGO-Group"; // linked to OWN_AG only
const OWN_SERVER_X = "E2E-RGO-Server-X"; // under OWN_SG, owner U -- the joinable server
const OWN_SERVER_Y = "E2E-RGO-Server-Y"; // under OWN_SG2, owner U -- the cross-system-group mismatch server
const OWN_SERVER_Z = "E2E-RGO-Server-Z"; // under OWN_SG, owner W (not a member of OWN_AG) -- the no-leak server
const OWNER_U_NAME = "E2E-RGO-Owner-U";
const PEER_V_NAME = "E2E-RGO-Peer-V";
const OWNER_W_NAME = "E2E-RGO-Owner-W";

// --- Provisioning-enforcement scenario names (spec:
// 2026-08-12-resource-groups-phase-2-provisioning, Task 7). Distinct
// `E2E-RGP-*` prefix -- the "RGP" trigram never collides with "RG-" or
// "RGO-" (the char right after "RG" differs), and every individual NATO-
// alphabet word below is used EXACTLY ONCE across this whole file, so no
// name here is a literal substring of any other name in this suite (the
// row-search helpers above resolve a row via a SUBSTRING regex against the
// whole row's accessible text, and this suite is serial + shares ONE
// sqlite backend -- every group/server the two describes above created
// still exists on the SAME Groups/Servers tables this scenario renders).
const P_SG = "E2E-RGP-System"; // the one system group everything below hangs under
const P_AG = "E2E-RGP-Alpha"; // provisioned DIRECTLY (kind=admin_group); U is a plain MEMBER (no co-manager flags)
const P_AG_HOME = "E2E-RGP-Bravo"; // NEVER provisioned -- everyone else's admin-tier home + P_UG's containment parent
const P_UG = "E2E-RGP-Charlie"; // provisioned DIRECTLY (kind=user_group), parent=P_AG_HOME; U2 owns+is a member; U4 is INVITED but never accepts
const P_RG = "E2E-RGP-Delta"; // the resource group under test; sole server member = SERVER_X
const P_SVC = "E2E-RGP-Echo"; // provisioned DIRECTLY (kind=service)
const P_USER_U = "E2E-RGP-Foxtrot"; // member of P_AG -- exercises admin_group provisioning
const P_USER_U2 = "E2E-RGP-Golf"; // owner+member of P_UG -- exercises user_group provisioning
const P_USER_U3 = "E2E-RGP-Hotel"; // provisioned DIRECTLY (kind=user) -- independent of any group membership
const P_USER_U4 = "E2E-RGP-India"; // INVITED to P_UG, never accepts -- proves invited != member
const P_USER_V = "E2E-RGP-Juliett"; // provisioned nowhere -- the baseline non-provisioned principal
const P_MODEL_RESTRICTED = "E2E-RGP-Model-Kilo"; // offered ONLY by SERVER_X, the resource group's sole member
const P_MODEL_OPEN = "E2E-RGP-Model-Lima"; // offered ONLY by SERVER_Y, which joins NO resource group
const P_SERVER_X = "E2E-RGP-Server-Mike";
const P_SERVER_Y = "E2E-RGP-Server-November";

// --- Server-override scenario names (spec:
// 2026-08-12-server-override-and-portal-polish, Task 8). Distinct `E2E-SVO-*`
// prefix -- "SVO" (5th char "S") never collides (as a substring) with
// "RG-"/"RGO-"/"RGP-" (5th char "R") above, and every word below is used
// exactly once, so no name here is a literal substring of any other name in
// this file (this suite is serial + shares ONE sqlite backend across all
// four describes, so every group/server the earlier scenarios created still
// exists on the same Groups/Servers tables this scenario renders).
const SVO_SG = "E2E-SVO-System"; // the one system group everything below hangs under
const SVO_AG = "E2E-SVO-Alpha"; // U + V are plain MEMBERS (no co-manager flags, no admin role)
const SVO_OWNER_U = "E2E-SVO-Owner-U"; // owns SERVER_X via owner_ids ONLY -- the server-manager
const SVO_PEER_V = "E2E-SVO-Peer-V"; // a plain member of SVO_AG who owns/manages nothing -- the non-manager
const SVO_SERVER_X = "E2E-SVO-Server-X"; // domain 127.0.0.1 -- nothing listens on its app's port, so a routed dial fails fast + deterministically ("connection refused"): no DNS lookup, no real network egress
const SVO_MODEL = "E2E-SVO-Model"; // offered ONLY by SERVER_X

/** Resource-group DTO shape (see internal/portal/service_resource_groups.go ResourceGroupDTO). */
type GroupRef = { id: string; name: string };
type ResourceGroupRow = {
  id: string;
  name: string;
  status: string;
  system_group: GroupRef;
  admin_groups: GroupRef[];
  servers: { id: string; name: string }[];
};
type ResourceGroupListBody = { data: ResourceGroupRow[] };
type AdminGroupCandidate = { id: string; name: string; parent_group_id: string; parent_group_name: string };
type AdminGroupCandidatesBody = { data: AdminGroupCandidate[] };
/** Server DTO shape as returned by the portal (see internal/portal/service.go ServerDTO) -- only the fields this suite reads. */
type ServerRow = { id: string; name: string; system_group_id: string };
type CurrentUserResp = { id: string };
type ApiErrorBody = { error?: { code?: string } };

/**
 * Navigates to the Groups view and waits for the landscape fetch to complete
 * (mirrors servers.spec.ts/services.spec.ts's own gotoGroups -- GroupsView's
 * landscape fetch opts OUT of loading-tracking, so there is no visible
 * loading gate to wait on otherwise).
 */
async function gotoGroups(page: Page): Promise<void> {
  const [resp] = await Promise.all([
    page.waitForResponse((r) => new URL(r.url()).pathname === "/api/portal/groups" && r.request().method() === "GET"),
    // exact: true -- t.groups ("Gruppen") is a literal substring of
    // t.resourceGroups ("Ressourcengruppen"), whose nav link now coexists on
    // the same sidebar; a non-exact match would resolve to both.
    page.getByRole("link", { name: t.groups, exact: true }).click()
  ]);
  expect(resp.ok()).toBe(true);
}

/**
 * Navigates to the Resource Groups view and waits for the
 * resource-group-admin-group-candidates fetch to land (a mount effect of
 * ResourceGroupsView) -- so the later UI smoke assertions never race an
 * in-flight fetch (mirrors servers.spec.ts's gotoServers / services.spec.ts's
 * gotoServices exactly, for the resource-group-flavored candidates endpoint).
 */
async function gotoResourceGroups(page: Page): Promise<void> {
  const [resp] = await Promise.all([
    page.waitForResponse(
      (r) => new URL(r.url()).pathname === "/api/portal/resource-group-admin-group-candidates" && r.request().method() === "GET"
    ),
    page.getByRole("link", { name: t.resourceGroups }).click()
  ]);
  expect(resp.ok()).toBe(true);
}

/**
 * Enters System-Admin mode (step-up) for the bootstrap system_admin -- a
 * fresh session is NOT elevated by default (spec: 2026-08-10-system-admin-
 * step-up-mode-design.md), and the `system` scope this suite needs (to
 * create System/Admin-tier groups, to see every admin group as a
 * resource-group/server-admin-group candidate, and to create servers/
 * resource groups without any group-based reach of its own) is attached
 * only after this.
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
 * picker always renders as a real dropdown even in a suite that creates only
 * two system groups of its own: migration v44 seeds a system-wide "Standard"
 * system group (`store.DefaultSystemGroupID`) that a system-scope caller's
 * `landscape.system` always includes alongside them, so `parentOptions.length`
 * is never 1 here (mirrors servers.spec.ts's/services.spec.ts's own
 * createAdminGroup, which never relies on auto-selection either, for the
 * same reason).
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
 * manages more than one admin group -- true here, the suite creates several
 * before its first invite). Returns the one-time invite URL + generated email.
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
 * and narrows their flags to can_manage_resources ONLY (unchecking the other
 * four, which a fresh promotion defaults to true alongside can_manage_resources
 * -- Decision 7's "existing manager" bootstrap default). This proves the new
 * flag is a genuinely independent capability: everything this suite has A do
 * with a RESOURCE GROUP below must work off can_manage_resources alone, never
 * off can_manage_users/can_manage_group/can_manage_servers/can_manage_services
 * leaking it -- in particular, A is NEVER given can_manage_servers anywhere,
 * so every server she reaches in this suite does so purely via ownership
 * (owner_ids), never via admin-group server co-management.
 */
async function promoteResourcesOnly(page: Page, memberName: string): Promise<void> {
  const row = page.getByRole("row", { name: new RegExp(memberName) });
  await expect(row).toBeVisible();
  await row.getByRole("button", { name: t.groupsActionPromote }).click();
  await expect(row.getByText(t.groupsRoleManager)).toBeVisible();
  const usersCheckbox = page.getByRole("checkbox", { name: `${t.groupsPermUsers} – ${memberName}` });
  const groupCheckbox = page.getByRole("checkbox", { name: `${t.groupsPermGroup} – ${memberName}` });
  const serversCheckbox = page.getByRole("checkbox", { name: `${t.groupsPermServers} – ${memberName}` });
  const servicesCheckbox = page.getByRole("checkbox", { name: `${t.groupsPermServices} – ${memberName}` });
  const resourcesCheckbox = page.getByRole("checkbox", { name: `${t.groupsPermResources} – ${memberName}` });
  await expect(usersCheckbox).toBeChecked();
  await expect(groupCheckbox).toBeChecked();
  await expect(serversCheckbox).toBeChecked();
  await expect(servicesCheckbox).toBeChecked();
  await expect(resourcesCheckbox).toBeChecked();
  await usersCheckbox.click();
  await expect(usersCheckbox).not.toBeChecked();
  await groupCheckbox.click();
  await expect(groupCheckbox).not.toBeChecked();
  await serversCheckbox.click();
  await expect(serversCheckbox).not.toBeChecked();
  await servicesCheckbox.click();
  await expect(servicesCheckbox).not.toBeChecked();
  // Never touched -- still checked, carried over verbatim by every PATCH
  // above -- proving can_manage_resources is a genuinely INDEPENDENT flag.
  await expect(resourcesCheckbox).toBeChecked();
}

/** Opens the resource-group detail sub-view for the row named `groupName`. */
async function openResourceGroupDetail(page: Page, groupName: string): Promise<void> {
  const row = page.getByRole("row", { name: new RegExp(groupName) });
  await row.getByRole("button", { name: t.modelDetailsAction }).click();
}

// --- Raw-API setup/assertion helpers -----------------------------------
//
// Driving a multi-step group+server+resource-group setup entirely through
// the create-server/create-resource-group UI forms is brittle (NetBird-
// conditional fields, multi-parent system-group picker branching, MUI
// Autocomplete option-label plumbing for an "owner" selection) for
// comparatively little extra assurance -- the security-critical surface
// (authorizeResourceGroup's group-scoping, the dual authorizeResourceGroup +
// authorizeServer + same-system-group gate, the 404-no-leak/400-mismatch
// mapping) lives entirely in the backend and is exercised identically
// whether the HTTP request that reaches it was typed by a human via a form
// or issued directly. So SETUP (servers, resource groups) and the
// SECURITY-CRITICAL assertions below go through the raw JSON API via
// `page.request` (which shares the page's session cookie); only the final
// UI smoke section drives the real Ressourcengruppen view.
//
// Every mutating raw call needs the CSRF header (internal/gateway/auth.go's
// csrfOK) -- `page.request` bypasses api.ts, which sets this on every
// mutating call, entirely.
const CSRF = { "X-OP-CSRF": "1" };

/** As `page`'s principal (system scope expected), resolves admin-tier group names to ids via the resource-group candidates endpoint (which, for a system-scope caller, returns EVERY admin-tier group system-wide). */
async function resolveAdminGroupIds(page: Page, names: string[]): Promise<Record<string, string>> {
  const resp = await page.request.get("/api/portal/resource-group-admin-group-candidates");
  expect(resp.ok(), `expected success, got ${resp.status()}: ${await resp.text()}`).toBe(true);
  const body = (await resp.json()) as AdminGroupCandidatesBody;
  const out: Record<string, string> = {};
  for (const name of names) {
    const found = body.data.find((c) => c.name === name);
    expect(found, `expected admin-group candidate named ${name} in ${JSON.stringify(body.data)}`).toBeTruthy();
    out[name] = found!.id;
  }
  return out;
}

/** Creates an AI-server with exactly one admin-group linkage + an optional single owner, via the raw API. */
async function createServerRaw(page: Page, opts: { name: string; domain: string; adminGroupId: string; ownerId?: string }): Promise<ServerRow> {
  const resp = await page.request.post("/api/portal/servers", {
    headers: CSRF,
    data: {
      name: opts.name,
      domain: opts.domain,
      status: "active",
      admin_group_ids: [opts.adminGroupId],
      owner_ids: opts.ownerId ? [opts.ownerId] : []
    }
  });
  expect(resp.ok(), `expected success creating ${opts.name}, got ${resp.status()}: ${await resp.text()}`).toBe(true);
  return (await resp.json()) as ServerRow;
}

/** Creates a resource group with the given admin-group linkage, via the raw API. */
async function createResourceGroupRaw(page: Page, opts: { name: string; adminGroupIds: string[] }): Promise<ResourceGroupRow> {
  const resp = await page.request.post("/api/portal/resource-groups", {
    headers: CSRF,
    data: { name: opts.name, status: "active", admin_group_ids: opts.adminGroupIds }
  });
  expect(resp.ok(), `expected success creating ${opts.name}, got ${resp.status()}: ${await resp.text()}`).toBe(true);
  return (await resp.json()) as ResourceGroupRow;
}

async function getResourceGroupRaw(page: Page, id: string) {
  return page.request.get(`/api/portal/resource-groups/${id}`);
}

async function listResourceGroupsRaw(page: Page): Promise<ResourceGroupRow[]> {
  const resp = await page.request.get("/api/portal/resource-groups");
  expect(resp.ok(), `expected success, got ${resp.status()}: ${await resp.text()}`).toBe(true);
  return ((await resp.json()) as ResourceGroupListBody).data;
}

async function putServersRaw(page: Page, id: string, serverIds: string[]) {
  return page.request.put(`/api/portal/resource-groups/${id}/servers`, {
    headers: CSRF,
    data: { server_ids: serverIds }
  });
}

async function putAdminGroupsRaw(page: Page, id: string, groupIds: string[]) {
  return page.request.put(`/api/portal/resource-groups/${id}/admin-groups`, {
    headers: CSRF,
    data: { admin_group_ids: groupIds }
  });
}

// --- Server-owner self-service raw-API helpers (spec:
// 2026-08-11-resource-groups-server-owner-self-service; endpoints from
// internal/portal/service_resource_group_owner.go) --------------------------

/** One entry of the server-owner eligible list (ServerResourceGroupDTO). */
type ServerResourceGroupRow = { id: string; name: string; member: boolean };
type ServerResourceGroupListBody = { data: ServerResourceGroupRow[] };

/** GET the resource groups the caller may enter `serverId` into (owner-scoped). */
async function listServerResourceGroups(page: Page, serverId: string): Promise<ServerResourceGroupRow[]> {
  const resp = await page.request.get(`/api/portal/servers/${serverId}/resource-groups`);
  expect(resp.ok(), `expected success, got ${resp.status()}: ${await resp.text()}`).toBe(true);
  return ((await resp.json()) as ServerResourceGroupListBody).data;
}

/** As the caller, enter `serverId` into resource group `rgId` (owner self-service PUT). */
async function joinServerResourceGroup(page: Page, serverId: string, rgId: string) {
  return page.request.put(`/api/portal/servers/${serverId}/resource-groups/${rgId}`, { headers: CSRF });
}

/** As the caller, remove `serverId` from resource group `rgId` (owner self-service DELETE). */
async function leaveServerResourceGroup(page: Page, serverId: string, rgId: string) {
  return page.request.delete(`/api/portal/servers/${serverId}/resource-groups/${rgId}`, { headers: CSRF });
}

// --- Provisioning-enforcement raw-API helpers (spec:
// 2026-08-12-resource-groups-phase-2-provisioning, Task 7). Builds a model
// that is genuinely OFFERED without any real upstream (mirrors
// e2e-health/health.spec.ts's own always_reachable pattern -- a fresh
// application defaults to reachable=true with zero probe cycles to wait on,
// see internal/gateway/app_health.go's `Reachable` doc), plus the
// provisioning-editor + system-settings raw endpoints (spec §Task 5/§Task 3). --

/** Creates an always-reachable application on `serverId` (no health probing needed for its model to be OFFERED). */
async function createAppRaw(page: Page, serverId: string): Promise<{ id: string }> {
  const resp = await page.request.post(`/api/portal/servers/${serverId}/applications`, {
    headers: CSRF,
    data: { type: "vllm", port: 8000, scheme: "https", health_check_mode: "always_reachable" }
  });
  expect(resp.ok(), `expected success creating an application on ${serverId}, got ${resp.status()}: ${await resp.text()}`).toBe(true);
  return (await resp.json()) as { id: string };
}

/** Creates one active model mapping on `appId`, offering `gatewayModelName`. */
async function createMappingRaw(page: Page, appId: string, gatewayModelName: string, appModelName: string): Promise<void> {
  const resp = await page.request.post(`/api/portal/applications/${appId}/mappings`, {
    headers: CSRF,
    data: { gateway_model_name: gatewayModelName, app_model_name: appModelName }
  });
  expect(resp.ok(), `expected success creating mapping ${gatewayModelName}, got ${resp.status()}: ${await resp.text()}`).toBe(true);
}

/** Creates a USER-tier group as `page`'s principal, who becomes its owner + sole auto-enrolled member (createUserGroup's own behavior). The caller MUST already be a MEMBER of `parentGroupId` (createUserGroup's unconditional containment gate — no system-scope exemption). */
async function createUserGroupRaw(page: Page, name: string, parentGroupId: string): Promise<{ id: string; name: string }> {
  const resp = await page.request.post("/api/portal/groups", {
    headers: CSRF,
    data: { tier: "user", name, parent_group_id: parentGroupId }
  });
  expect(resp.ok(), `expected success creating user group ${name}, got ${resp.status()}: ${await resp.text()}`).toBe(true);
  return (await resp.json()) as { id: string; name: string };
}

/** Adds/invites `userIds` to `groupId` (direct member for admin/system tier; state=invited, pending accept, for user tier). */
async function addGroupMembersRaw(page: Page, groupId: string, userIds: string[]) {
  return page.request.post(`/api/portal/groups/${groupId}/members`, { headers: CSRF, data: { user_ids: userIds } });
}

/** Creates a Service Account linked to exactly one admin group (system scope may pick any group; no delegates/allowlist needed for this suite). */
async function createServiceRaw(page: Page, name: string, adminGroupId: string): Promise<{ id: string; name: string }> {
  const resp = await page.request.post("/api/portal/services", {
    headers: CSRF,
    data: { name, delegates: [], allowed_models: [], admin_group_ids: [adminGroupId] }
  });
  expect(resp.ok(), `expected success creating service ${name}, got ${resp.status()}: ${await resp.text()}`).toBe(true);
  return (await resp.json()) as { id: string; name: string };
}

/** Mints a service token (scope llm:invoke, fixed) and returns its one-time plaintext secret. */
async function createServiceTokenRaw(page: Page, serviceId: string): Promise<string> {
  const resp = await page.request.post(`/api/portal/services/${serviceId}/tokens`, {
    headers: CSRF,
    data: { name: "e2e-rgp-token" }
  });
  expect(resp.ok(), `expected success minting a service token, got ${resp.status()}: ${await resp.text()}`).toBe(true);
  return ((await resp.json()) as { secret: string }).secret;
}

/** Atomically REPLACES resource group `rgId`'s whole "provisioned for" target set. */
async function putProvisionsRaw(page: Page, rgId: string, provisions: { kind: string; target_id: string }[]) {
  return page.request.put(`/api/portal/resource-groups/${rgId}/provisions`, { headers: CSRF, data: { provisions } });
}

/** GET /api/portal/models as `page`'s principal (session-cookie auth), returning just the model ids. */
async function listModelIds(page: Page): Promise<string[]> {
  const resp = await page.request.get("/api/portal/models");
  expect(resp.ok(), `expected success, got ${resp.status()}: ${await resp.text()}`).toBe(true);
  return ((await resp.json()) as { data: { id: string }[] }).data.map((m) => m.id);
}

/** GET /v1/models with a service-token Bearer secret, returning just the model ids. */
async function listModelIdsViaBearer(page: Page, secret: string): Promise<string[]> {
  const resp = await page.request.get("/v1/models", { headers: { Authorization: `Bearer ${secret}` } });
  expect(resp.ok(), `expected success, got ${resp.status()}: ${await resp.text()}`).toBe(true);
  return ((await resp.json()) as { data: { id: string }[] }).data.map((m) => m.id);
}

/** Flips the system-wide resource_provisioning_enforce toggle (system scope required). */
async function setResourceProvisioningEnforce(page: Page, enforce: boolean): Promise<void> {
  const resp = await page.request.put("/api/system/settings", {
    headers: CSRF,
    data: { resource_provisioning_enforce: enforce }
  });
  expect(resp.ok(), `expected success setting resource_provisioning_enforce=${enforce}, got ${resp.status()}: ${await resp.text()}`).toBe(true);
}

// --- Server-override raw-API helpers (spec:
// 2026-08-12-server-override-and-portal-polish, Task 8; endpoints from
// internal/portal/service_server_override.go / service.go CreateToken/
// UpdateToken and internal/gateway/server.go handlePortalServerModels). ----

/** Minimal token shape this scenario reads (see internal/portal/service.go TokenDTO -- server_override/_force_unreachable are both `,omitempty`, so an unset override is simply ABSENT from the JSON). */
type TokenRow = { id: string; name: string; server_override?: string; server_override_force_unreachable?: boolean };

/** Creates an API token as `page`'s principal (session-cookie auth), optionally forcing every request on it onto `serverOverrideId`. Returns the TokenDTO + the one-time plaintext secret. */
async function createTokenRaw(
  page: Page,
  opts: { name: string; serverOverrideId?: string; serverOverrideForce?: boolean }
): Promise<{ token: TokenRow; secret: string }> {
  const resp = await page.request.post("/api/portal/tokens", {
    headers: CSRF,
    data: {
      name: opts.name,
      scopes: ["gateway:use"],
      server_override: opts.serverOverrideId ?? "",
      server_override_force_unreachable: opts.serverOverrideForce ?? false
    }
  });
  expect(resp.ok(), `expected success creating token ${opts.name}, got ${resp.status()}: ${await resp.text()}`).toBe(true);
  return (await resp.json()) as { token: TokenRow; secret: string };
}

/** PATCHes token `id` as `page`'s principal (session-cookie auth) with an arbitrary partial body. */
async function patchTokenRaw(page: Page, id: string, body: Record<string, unknown>) {
  return page.request.patch(`/api/portal/tokens/${id}`, { headers: CSRF, data: body });
}

/** GET the offered-model set for `serverId` (owner/co-manager-gated, 404-no-leak -- see internal/portal/service_server_override.go ServerModels). */
async function getServerModelsRaw(page: Page, serverId: string) {
  return page.request.get(`/api/portal/servers/${serverId}/models`);
}

/** PATCHes an AI-server's raw fields (owner_ids / status / ...) as the caller (system_admin expected here). */
async function patchServerRaw(page: Page, id: string, body: Record<string, unknown>) {
  return page.request.patch(`/api/portal/servers/${id}`, { headers: CSRF, data: body });
}

/** POSTs a non-streaming chat completion as `page`'s SESSION principal (cookie + CSRF -- mirrors the Phase 2 provisioning test's own vPage.request.post call). */
async function chatCompletionSession(page: Page, model: string) {
  return page.request.post("/v1/chat/completions", {
    headers: CSRF,
    data: { model, messages: [{ role: "user", content: "hello" }], stream: false }
  });
}

/** POSTs a non-streaming chat completion authenticated by a bearer secret, via the cookie-less `request` fixture + the gateway's OWN absolute origin (see the GW constant's doc comment for why this must never be `page.request`). */
async function chatCompletionBearer(request: APIRequestContext, secret: string, model: string) {
  return request.post(`${GW}/v1/chat/completions`, {
    headers: { Authorization: `Bearer ${secret}`, "Content-Type": "application/json" },
    data: { model, messages: [{ role: "user", content: "hello" }], stream: false }
  });
}

test.describe("Resource Groups Phase 1 — group-scoped resource-group + server-membership management", () => {
  test("can_manage_resources co-manager creates/manages a resource group via its admin group; enters an owned same-system-group server (200); is refused a cross-system-group server (400 mismatch) and an unmanaged server (404 no-leak); a resource group linked only to a group they don't manage is 404-no-leak; system_admin sees all; add/remove-linkage + add/remove-server happy paths", async ({
    page: systemAdminPage,
    browser
  }) => {
    // --- Setup: two system groups, four admin groups spread across them ------
    await login(systemAdminPage, RESOURCE_GROUPS_ADMIN_EMAIL, RESOURCE_GROUPS_ADMIN_PASSWORD);
    await enterSystemAdminMode(systemAdminPage, RESOURCE_GROUPS_ADMIN_NAME, RESOURCE_GROUPS_ADMIN_PASSWORD);

    await gotoGroups(systemAdminPage);
    await createSystemGroup(systemAdminPage, SG_NAME);
    await createAdminGroup(systemAdminPage, AG_ONE, SG_NAME);
    await createAdminGroup(systemAdminPage, AG_TWO, SG_NAME);
    await createAdminGroup(systemAdminPage, AG_OUT, SG_NAME);
    await createSystemGroup(systemAdminPage, SG_TWO);
    await createAdminGroup(systemAdminPage, AG_DIFF, SG_TWO);

    // --- Invite A as a plain admin-tier member of AG_ONE ----------------------
    const inviteA = await inviteWithAdminGroup(systemAdminPage, { role: "admin", adminGroupName: AG_ONE, displayName: ADMIN_A_NAME });
    const aPage = await redeemInvite(browser, inviteA.inviteUrl);

    // --- Add A directly to AG_TWO too (containment holds: A is already a
    // member of SG_NAME, AG_TWO's parent, via the AG_ONE invite above) --------
    await gotoGroups(systemAdminPage);
    await openMembers(systemAdminPage, AG_TWO);
    await addCandidate(systemAdminPage, ADMIN_A_NAME);
    await expect(systemAdminPage.getByText(ADMIN_A_NAME)).toBeVisible();

    // --- Promote A to co-manager of AG_ONE and AG_TWO, can_manage_resources
    // ONLY on each (can_manage_users/can_manage_group/can_manage_servers/
    // can_manage_services all narrowed false) --------------------------------
    await systemAdminPage.getByRole("button", { name: t.back }).click();
    await openMembers(systemAdminPage, AG_ONE);
    await promoteResourcesOnly(systemAdminPage, ADMIN_A_NAME);

    await systemAdminPage.getByRole("button", { name: t.back }).click();
    await openMembers(systemAdminPage, AG_TWO);
    await promoteResourcesOnly(systemAdminPage, ADMIN_A_NAME);

    // A is left a PLAIN member of neither AG_OUT nor AG_DIFF -- system_admin
    // remains their sole owner+member -- the deliberate "A manages nothing
    // there" case; A never joins AG_DIFF's tree at all (SG_TWO/AG_DIFF are
    // purely a vehicle for creating SERVER_DIFF below).

    // --- Resolve the admin-group ids (as the elevated system_admin, whose
    // candidate set is EVERY admin-tier group system-wide) + A's own user id -
    const groupIds = await resolveAdminGroupIds(systemAdminPage, [AG_ONE, AG_TWO, AG_OUT, AG_DIFF]);
    const meResp = await aPage.request.get("/api/portal/me");
    expect(meResp.ok(), `expected success, got ${meResp.status()}: ${await meResp.text()}`).toBe(true);
    const aUserId = ((await meResp.json()) as CurrentUserResp).id;

    // --- Three servers, all created by system_admin. A is made an OWNER
    // (owner_ids) of two of them -- NEVER a server co-manager (can_manage_servers
    // stays false on every group A touches) -- so every server A can reach in
    // this suite does so purely via ownership, proving the resource-group
    // dual-gate genuinely needs BOTH authorizeResourceGroup AND authorizeServer,
    // via two entirely independent authorization mechanisms. -----------------
    const serverSame = await createServerRaw(systemAdminPage, {
      name: SERVER_SAME,
      domain: "e2e-rg-same.example.test",
      adminGroupId: groupIds[AG_ONE],
      ownerId: aUserId
    });
    const serverDiff = await createServerRaw(systemAdminPage, {
      name: SERVER_DIFF,
      domain: "e2e-rg-diff.example.test",
      adminGroupId: groupIds[AG_DIFF],
      ownerId: aUserId
    });
    const serverOut = await createServerRaw(systemAdminPage, {
      name: SERVER_OUT,
      domain: "e2e-rg-out.example.test",
      adminGroupId: groupIds[AG_OUT]
      // no owner -- A owns/manages nothing here.
    });
    expect(serverSame.system_group_id).not.toBe("");
    expect(serverDiff.system_group_id).not.toBe(serverSame.system_group_id);

    // --- resource-group-admin-group-candidates: A sees AG_ONE + AG_TWO, never
    // AG_OUT, never AG_DIFF ----------------------------------------------------
    const candidatesResp = await aPage.request.get("/api/portal/resource-group-admin-group-candidates");
    expect(candidatesResp.ok(), `expected success, got ${candidatesResp.status()}: ${await candidatesResp.text()}`).toBe(true);
    const candidateNames = ((await candidatesResp.json()) as AdminGroupCandidatesBody).data.map((c) => c.name);
    expect(candidateNames).toContain(AG_ONE);
    expect(candidateNames).toContain(AG_TWO);
    expect(candidateNames).not.toContain(AG_OUT);
    expect(candidateNames).not.toContain(AG_DIFF);

    // === (1) A creates RG_ALPHA into AG_ONE; the system group is auto-derived =
    const alpha = await createResourceGroupRaw(aPage, { name: RG_ALPHA, adminGroupIds: [groupIds[AG_ONE]] });
    expect(alpha.system_group.name).toBe(SG_NAME);
    expect(alpha.admin_groups.map((g) => g.name)).toEqual([AG_ONE]);
    expect(alpha.servers).toEqual([]);

    const listAsAAfterAlpha = await listResourceGroupsRaw(aPage);
    expect(listAsAAfterAlpha.some((g) => g.name === RG_ALPHA)).toBe(true);
    expect(listAsAAfterAlpha.some((g) => g.name === RG_BETA)).toBe(false);

    // === (2) A enters SERVER_SAME (same system group, A is its OWNER) -> 200 ==
    const enterSame = await putServersRaw(aPage, alpha.id, [serverSame.id]);
    expect(enterSame.ok(), `expected success, got ${enterSame.status()}: ${await enterSame.text()}`).toBe(true);
    const afterEnterSame = (await enterSame.json()) as ResourceGroupRow;
    expect(afterEnterSame.servers).toEqual([{ id: serverSame.id, name: SERVER_SAME }]);

    // === (3) A attempts SERVER_DIFF (A also owns it, but a DIFFERENT system
    // group) -> 400 server_system_group_mismatch, no partial application =====
    const enterDiff = await putServersRaw(aPage, alpha.id, [serverDiff.id]);
    expect(enterDiff.status(), `expected 400, got ${enterDiff.status()}: ${await enterDiff.text()}`).toBe(400);
    expect((await enterDiff.json() as ApiErrorBody).error?.code).toBe("resource_group.server_system_group_mismatch");
    const alphaAfterMismatch = await getResourceGroupRaw(systemAdminPage, alpha.id);
    expect(((await alphaAfterMismatch.json()) as ResourceGroupRow).servers).toEqual([{ id: serverSame.id, name: SERVER_SAME }]);

    // === (4) A attempts SERVER_OUT (same system group, but A owns/manages
    // NOTHING there) -> 404 server_forbidden (no-leak on the SERVER), no
    // partial application ======================================================
    const enterOut = await putServersRaw(aPage, alpha.id, [serverOut.id]);
    expect(enterOut.status(), `expected 404, got ${enterOut.status()}: ${await enterOut.text()}`).toBe(404);
    expect((await enterOut.json() as ApiErrorBody).error?.code).toBe("resource_group.server_forbidden");
    const alphaAfterForbidden = await getResourceGroupRaw(systemAdminPage, alpha.id);
    expect(((await alphaAfterForbidden.json()) as ResourceGroupRow).servers).toEqual([{ id: serverSame.id, name: SERVER_SAME }]);

    // === (5) system_admin creates RG_BETA into AG_OUT (a group A never
    // manages) -- NOT visible/manageable to A: a raw GET, a raw admin-groups
    // PUT, and a raw servers PUT all fail with the SAME 404 a genuinely
    // non-existent id would produce (never 403 -- no existence leak) =========
    const beta = await createResourceGroupRaw(systemAdminPage, { name: RG_BETA, adminGroupIds: [groupIds[AG_OUT]] });
    expect(beta.admin_groups.map((g) => g.name)).toEqual([AG_OUT]);

    const getBetaAsA = await aPage.request.get(`/api/portal/resource-groups/${beta.id}`);
    expect(getBetaAsA.status(), `expected 404, got ${getBetaAsA.status()}: ${await getBetaAsA.text()}`).toBe(404);
    expect((await getBetaAsA.json() as ApiErrorBody).error?.code).toBe("resource_group.not_found");

    const adminGroupsPutBetaAsA = await putAdminGroupsRaw(aPage, beta.id, [groupIds[AG_ONE]]);
    expect(adminGroupsPutBetaAsA.status(), `expected 404, got ${adminGroupsPutBetaAsA.status()}: ${await adminGroupsPutBetaAsA.text()}`).toBe(404);
    expect((await adminGroupsPutBetaAsA.json() as ApiErrorBody).error?.code).toBe("resource_group.not_found");

    const serversPutBetaAsA = await putServersRaw(aPage, beta.id, [serverSame.id]);
    expect(serversPutBetaAsA.status(), `expected 404, got ${serversPutBetaAsA.status()}: ${await serversPutBetaAsA.text()}`).toBe(404);
    expect((await serversPutBetaAsA.json() as ApiErrorBody).error?.code).toBe("resource_group.not_found");

    // The three rejected calls had no effect (Beta genuinely unchanged: still
    // named RG_BETA, still linked only to AG_OUT, still no members).
    const betaAfterRejected = (await (await getResourceGroupRaw(systemAdminPage, beta.id)).json()) as ResourceGroupRow;
    expect(betaAfterRejected.name).toBe(RG_BETA);
    expect(betaAfterRejected.admin_groups.map((g) => g.name)).toEqual([AG_OUT]);
    expect(betaAfterRejected.servers).toEqual([]);

    // A's own list still excludes Beta entirely.
    const listAsAAfterBeta = await listResourceGroupsRaw(aPage);
    expect(listAsAAfterBeta.some((g) => g.name === RG_BETA)).toBe(false);

    // === (6) system_admin sees ALL resource groups, regardless of scoping ====
    const listAsSystem = await listResourceGroupsRaw(systemAdminPage);
    expect(listAsSystem.some((g) => g.id === alpha.id)).toBe(true);
    expect(listAsSystem.some((g) => g.id === beta.id)).toBe(true);

    // === (7a) Add/remove-LINKAGE happy path on RG_ALPHA (as A) ================
    const linkBoth = await putAdminGroupsRaw(aPage, alpha.id, [groupIds[AG_ONE], groupIds[AG_TWO]]);
    expect(linkBoth.ok(), `expected success, got ${linkBoth.status()}: ${await linkBoth.text()}`).toBe(true);
    expect(((await linkBoth.json()) as ResourceGroupRow).admin_groups.map((g) => g.name).sort()).toEqual([AG_ONE, AG_TWO].sort());

    const dropOne = await putAdminGroupsRaw(aPage, alpha.id, [groupIds[AG_TWO]]);
    expect(dropOne.ok(), `expected success, got ${dropOne.status()}: ${await dropOne.text()}`).toBe(true);
    expect(((await dropOne.json()) as ResourceGroupRow).admin_groups.map((g) => g.name)).toEqual([AG_TWO]);

    const emptyLinkage = await putAdminGroupsRaw(aPage, alpha.id, []);
    expect(emptyLinkage.status(), `expected 400, got ${emptyLinkage.status()}: ${await emptyLinkage.text()}`).toBe(400);
    expect((await emptyLinkage.json() as ApiErrorBody).error?.code).toBe("resource_group.admin_group_required");

    // Alpha's linkage is unaffected by the rejected empty-set call -- still
    // AG_TWO only, from the last SUCCESSFUL save above.
    const alphaAfterRejectedEmpty = (await (await getResourceGroupRaw(systemAdminPage, alpha.id)).json()) as ResourceGroupRow;
    expect(alphaAfterRejectedEmpty.admin_groups.map((g) => g.name)).toEqual([AG_TWO]);

    // === (7b) Add/remove-SERVER happy path on RG_ALPHA (as A) ==================
    // A still co-manages AG_TWO (can_manage_resources) after the linkage edit
    // above, so authorizeResourceGroup keeps passing for her.
    const clearServers = await putServersRaw(aPage, alpha.id, []);
    expect(clearServers.ok(), `expected success, got ${clearServers.status()}: ${await clearServers.text()}`).toBe(true);
    expect(((await clearServers.json()) as ResourceGroupRow).servers).toEqual([]);

    const reAddSame = await putServersRaw(aPage, alpha.id, [serverSame.id]);
    expect(reAddSame.ok(), `expected success, got ${reAddSame.status()}: ${await reAddSame.text()}`).toBe(true);
    expect(((await reAddSame.json()) as ResourceGroupRow).servers).toEqual([{ id: serverSame.id, name: SERVER_SAME }]);

    // === UI smoke (nice-to-have): the Ressourcengruppen view itself renders +
    // lists correctly for both principals, and the detail sub-view shows the
    // linkage/membership state the raw API calls above just produced ========
    await gotoResourceGroups(aPage);
    await expect(aPage.getByRole("row", { name: new RegExp(RG_ALPHA) })).toBeVisible();
    await expect(aPage.getByRole("row", { name: new RegExp(RG_BETA) })).toHaveCount(0);
    await openResourceGroupDetail(aPage, RG_ALPHA);
    const adminGroupsSection = aPage.getByRole("region", { name: t.resourceGroupAdminGroupsSectionTitle });
    await expect(adminGroupsSection.getByText(AG_TWO)).toBeVisible();
    const serversSection = aPage.getByRole("region", { name: t.resourceGroupServersSectionTitle });
    await expect(serversSection.getByText(SERVER_SAME)).toBeVisible();

    await gotoResourceGroups(systemAdminPage);
    await expect(systemAdminPage.getByRole("row", { name: new RegExp(RG_ALPHA) })).toBeVisible();
    await expect(systemAdminPage.getByRole("row", { name: new RegExp(RG_BETA) })).toBeVisible();
  });
});

test.describe("Resource Groups — server-owner self-service membership", () => {
  test("a plain-USER server OWNER (no admin/resource permission), member of a linked Verwaltungsgruppe, joins/leaves their own same-system-group server via the server-owner endpoints; a non-owner is 404-no-leak; a cross-system-group server is 400 mismatch; an owner-not-a-member is 404-no-leak; a manager-added server is leavable by the owner; and the ServerList sub-view drives the real endpoint", async ({
    page: systemAdminPage,
    browser
  }) => {
    // --- Setup (as the elevated bootstrap system_admin) ----------------------
    await login(systemAdminPage, RESOURCE_GROUPS_ADMIN_EMAIL, RESOURCE_GROUPS_ADMIN_PASSWORD);
    await enterSystemAdminMode(systemAdminPage, RESOURCE_GROUPS_ADMIN_NAME, RESOURCE_GROUPS_ADMIN_PASSWORD);

    await gotoGroups(systemAdminPage);
    await createSystemGroup(systemAdminPage, OWN_SG);
    await createAdminGroup(systemAdminPage, OWN_AG, OWN_SG);
    await createAdminGroup(systemAdminPage, OWN_AGX, OWN_SG);
    await createSystemGroup(systemAdminPage, OWN_SG2);
    await createAdminGroup(systemAdminPage, OWN_AG2, OWN_SG2);

    // U + V are plain MEMBERS of OWN_AG (role user -- NEITHER admin NOR any
    // co-manager flag: exactly the "weder Admin noch Resourcen berechtigung"
    // the feature grants for). W is a plain member of OWN_AGX only.
    const inviteU = await inviteWithAdminGroup(systemAdminPage, { role: "user", adminGroupName: OWN_AG, displayName: OWNER_U_NAME });
    const uPage = await redeemInvite(browser, inviteU.inviteUrl);
    const inviteV = await inviteWithAdminGroup(systemAdminPage, { role: "user", adminGroupName: OWN_AG, displayName: PEER_V_NAME });
    const vPage = await redeemInvite(browser, inviteV.inviteUrl);
    const inviteW = await inviteWithAdminGroup(systemAdminPage, { role: "user", adminGroupName: OWN_AGX, displayName: OWNER_W_NAME });
    const wPage = await redeemInvite(browser, inviteW.inviteUrl);

    const groupIds = await resolveAdminGroupIds(systemAdminPage, [OWN_AG, OWN_AGX, OWN_AG2]);
    const uId = ((await (await uPage.request.get("/api/portal/me")).json()) as CurrentUserResp).id;
    const wId = ((await (await wPage.request.get("/api/portal/me")).json()) as CurrentUserResp).id;

    // Three servers (all created by system_admin). U owns X (OWN_SG) + Y
    // (OWN_SG2); W owns Z (OWN_SG). Ownership is via owner_ids ONLY -- none of
    // U/V/W is a can_manage_servers co-manager anywhere.
    const serverX = await createServerRaw(systemAdminPage, { name: OWN_SERVER_X, domain: "e2e-rgo-x.example.test", adminGroupId: groupIds[OWN_AG], ownerId: uId });
    const serverY = await createServerRaw(systemAdminPage, { name: OWN_SERVER_Y, domain: "e2e-rgo-y.example.test", adminGroupId: groupIds[OWN_AG2], ownerId: uId });
    const serverZ = await createServerRaw(systemAdminPage, { name: OWN_SERVER_Z, domain: "e2e-rgo-z.example.test", adminGroupId: groupIds[OWN_AGX], ownerId: wId });
    expect(serverX.system_group_id).not.toBe("");
    expect(serverY.system_group_id).not.toBe(serverX.system_group_id);

    // system_admin creates RG_OWN linked to OWN_AG (U is a member; a plain-user
    // U could not create it -- CreateResourceGroup needs system OR
    // can_manage_resources -- which is exactly the point of the self-service
    // JOIN path below).
    const rgOwn = await createResourceGroupRaw(systemAdminPage, { name: OWN_RG, adminGroupIds: [groupIds[OWN_AG]] });
    expect(rgOwn.system_group.name).toBe(OWN_SG);

    // === (1) U sees RG_OWN eligible for X (member:false), joins -> member:true,
    // leaves -> member:false; join/leave are idempotent =======================
    let listX = await listServerResourceGroups(uPage, serverX.id);
    expect(listX.map((g) => g.name)).toEqual([OWN_RG]);
    expect(listX[0].member).toBe(false);

    const join1 = await joinServerResourceGroup(uPage, serverX.id, rgOwn.id);
    expect(join1.ok(), `expected success joining, got ${join1.status()}: ${await join1.text()}`).toBe(true);
    listX = await listServerResourceGroups(uPage, serverX.id);
    expect(listX.find((g) => g.name === OWN_RG)?.member).toBe(true);
    // Confirmed against the manager-facing RG detail (as system_admin): X is now a member.
    const rgAfterJoin = (await (await getResourceGroupRaw(systemAdminPage, rgOwn.id)).json()) as ResourceGroupRow;
    expect(rgAfterJoin.servers).toEqual([{ id: serverX.id, name: OWN_SERVER_X }]);

    // Idempotent re-join: still 200, still exactly one member.
    const join2 = await joinServerResourceGroup(uPage, serverX.id, rgOwn.id);
    expect(join2.ok(), `expected idempotent success, got ${join2.status()}: ${await join2.text()}`).toBe(true);
    expect(((await (await getResourceGroupRaw(systemAdminPage, rgOwn.id)).json()) as ResourceGroupRow).servers).toEqual([{ id: serverX.id, name: OWN_SERVER_X }]);

    const leave1 = await leaveServerResourceGroup(uPage, serverX.id, rgOwn.id);
    expect(leave1.ok(), `expected success leaving, got ${leave1.status()}: ${await leave1.text()}`).toBe(true);
    listX = await listServerResourceGroups(uPage, serverX.id);
    expect(listX.find((g) => g.name === OWN_RG)?.member).toBe(false);
    // Idempotent re-leave: still 200.
    const leave2 = await leaveServerResourceGroup(uPage, serverX.id, rgOwn.id);
    expect(leave2.ok(), `expected idempotent success, got ${leave2.status()}: ${await leave2.text()}`).toBe(true);

    // === (2) V (member of OWN_AG but NOT an owner of X) -> 404 server.not_found
    // on both GET and PUT (indistinguishable from a non-existent server) =======
    const vGetX = await vPage.request.get(`/api/portal/servers/${serverX.id}/resource-groups`);
    expect(vGetX.status(), `expected 404, got ${vGetX.status()}: ${await vGetX.text()}`).toBe(404);
    expect((await vGetX.json() as ApiErrorBody).error?.code).toBe("server.not_found");
    const vPutX = await joinServerResourceGroup(vPage, serverX.id, rgOwn.id);
    expect(vPutX.status(), `expected 404, got ${vPutX.status()}: ${await vPutX.text()}`).toBe(404);
    expect((await vPutX.json() as ApiErrorBody).error?.code).toBe("server.not_found");

    // === (3) U owns Y, but Y is in a DIFFERENT system group: RG_OWN is absent
    // from Y's eligible list, and a PUT is 400 server_system_group_mismatch ====
    const listY = await listServerResourceGroups(uPage, serverY.id);
    expect(listY.some((g) => g.name === OWN_RG)).toBe(false);
    const uPutY = await joinServerResourceGroup(uPage, serverY.id, rgOwn.id);
    expect(uPutY.status(), `expected 400, got ${uPutY.status()}: ${await uPutY.text()}`).toBe(400);
    expect((await uPutY.json() as ApiErrorBody).error?.code).toBe("resource_group.server_system_group_mismatch");

    // === (4) W owns Z (same system group as RG_OWN) but is NOT a member of any
    // admin group RG_OWN is linked to: RG_OWN is absent from Z's eligible list,
    // and a raw PUT is 404 resource_group.not_found (no existence leak) ========
    const listZ = await listServerResourceGroups(wPage, serverZ.id);
    expect(listZ.some((g) => g.name === OWN_RG)).toBe(false);
    const wPutZ = await joinServerResourceGroup(wPage, serverZ.id, rgOwn.id);
    expect(wPutZ.status(), `expected 404, got ${wPutZ.status()}: ${await wPutZ.text()}`).toBe(404);
    expect((await wPutZ.json() as ApiErrorBody).error?.code).toBe("resource_group.not_found");

    // === (5) A MANAGER (system_admin) adds X to RG_OWN via the manager
    // endpoint; U (the owner + a member of the linked group) then LEAVES it ====
    const mgrAdd = await putServersRaw(systemAdminPage, rgOwn.id, [serverX.id]);
    expect(mgrAdd.ok(), `expected success, got ${mgrAdd.status()}: ${await mgrAdd.text()}`).toBe(true);
    expect(((await mgrAdd.json()) as ResourceGroupRow).servers).toEqual([{ id: serverX.id, name: OWN_SERVER_X }]);
    const uLeaveMgrAdded = await leaveServerResourceGroup(uPage, serverX.id, rgOwn.id);
    expect(uLeaveMgrAdded.ok(), `expected success, got ${uLeaveMgrAdded.status()}: ${await uLeaveMgrAdded.text()}`).toBe(true);
    expect(((await (await getResourceGroupRaw(systemAdminPage, rgOwn.id)).json()) as ResourceGroupRow).servers).toEqual([]);

    // === (6) UI smoke: U drives the real ServerList -> Ressourcengruppen
    // sub-view for X (currently NOT a member) and toggles the join switch,
    // proving the frontend wiring hits the live PUT endpoint ==================
    const [serversFetch] = await Promise.all([
      uPage.waitForResponse((r) => new URL(r.url()).pathname === "/api/portal/servers" && r.request().method() === "GET"),
      uPage.getByRole("link", { name: t.servers, exact: true }).click()
    ]);
    expect(serversFetch.ok()).toBe(true);
    const xRow = uPage.getByRole("row", { name: new RegExp(OWN_SERVER_X) });
    await expect(xRow).toBeVisible();
    await xRow.getByRole("button", { name: t.serverResourceGroupsAction }).click();
    const rgSwitch = uPage.getByRole("switch", { name: OWN_RG });
    await expect(rgSwitch).toBeVisible();
    await expect(rgSwitch).not.toBeChecked();
    const [uiPut] = await Promise.all([
      uPage.waitForResponse(
        (r) => new URL(r.url()).pathname === `/api/portal/servers/${serverX.id}/resource-groups/${rgOwn.id}` && r.request().method() === "PUT"
      ),
      rgSwitch.click()
    ]);
    expect(uiPut.ok(), `expected the UI-driven join PUT to succeed, got ${uiPut.status()}`).toBe(true);
    await expect(rgSwitch).toBeChecked();
    // Confirmed against the manager-facing RG detail: the UI toggle really added X.
    expect(((await (await getResourceGroupRaw(systemAdminPage, rgOwn.id)).json()) as ResourceGroupRow).servers).toEqual([{ id: serverX.id, name: OWN_SERVER_X }]);
  });
});

test.describe("Resource Groups Phase 2 — provisioning enforcement (opt-in + deny)", () => {
  test("opt-in mode leaves an unrestricted server open to everyone while a provisioned server is gated by admin-group/user-group/direct-user/service provisioning (an invited-not-accepted membership does NOT grant access); deny mode denies an unprovisioned server to everyone and a provisioned server to a non-target; the routing layer itself (not just the model list) refuses a non-provisioned direct request without ever dialing the fake upstream", async ({
    page: systemAdminPage,
    browser
  }) => {
    await login(systemAdminPage, RESOURCE_GROUPS_ADMIN_EMAIL, RESOURCE_GROUPS_ADMIN_PASSWORD);
    await enterSystemAdminMode(systemAdminPage, RESOURCE_GROUPS_ADMIN_NAME, RESOURCE_GROUPS_ADMIN_PASSWORD);

    // Defensive baseline: no earlier test in this file touches
    // resource_provisioning_enforce, but pin it to false explicitly so this
    // test's starting state never depends on run order.
    await setResourceProvisioningEnforce(systemAdminPage, false);

    // --- Groups: P_AG is provisioned directly; P_AG_HOME is NEVER
    // provisioned and serves as everyone else's admin-tier home PLUS P_UG's
    // required containment parent; P_UG is provisioned directly too --------
    await gotoGroups(systemAdminPage);
    await createSystemGroup(systemAdminPage, P_SG);
    await createAdminGroup(systemAdminPage, P_AG, P_SG);
    await createAdminGroup(systemAdminPage, P_AG_HOME, P_SG);

    const inviteU = await inviteWithAdminGroup(systemAdminPage, { role: "user", adminGroupName: P_AG, displayName: P_USER_U });
    const uPage = await redeemInvite(browser, inviteU.inviteUrl);
    const inviteU2 = await inviteWithAdminGroup(systemAdminPage, { role: "user", adminGroupName: P_AG_HOME, displayName: P_USER_U2 });
    const u2Page = await redeemInvite(browser, inviteU2.inviteUrl);
    const inviteU3 = await inviteWithAdminGroup(systemAdminPage, { role: "user", adminGroupName: P_AG_HOME, displayName: P_USER_U3 });
    const u3Page = await redeemInvite(browser, inviteU3.inviteUrl);
    const inviteU4 = await inviteWithAdminGroup(systemAdminPage, { role: "user", adminGroupName: P_AG_HOME, displayName: P_USER_U4 });
    const u4Page = await redeemInvite(browser, inviteU4.inviteUrl);
    const inviteV = await inviteWithAdminGroup(systemAdminPage, { role: "user", adminGroupName: P_AG_HOME, displayName: P_USER_V });
    const vPage = await redeemInvite(browser, inviteV.inviteUrl);

    const groupIds = await resolveAdminGroupIds(systemAdminPage, [P_AG, P_AG_HOME]);
    const u4Id = ((await (await u4Page.request.get("/api/portal/me")).json()) as CurrentUserResp).id;
    const u3Id = ((await (await u3Page.request.get("/api/portal/me")).json()) as CurrentUserResp).id;

    // P_UG's owner must already be a MEMBER of its admin-tier parent
    // (createUserGroup's unconditional containment gate, no system-scope
    // exemption) -- U2 is, via the P_AG_HOME invite above. Creating it makes
    // U2 its owner + sole auto-enrolled member (createUserGroup's own
    // documented behavior) -- no separate invite/accept step needed for U2.
    const ug = await createUserGroupRaw(u2Page, P_UG, groupIds[P_AG_HOME]);

    // U4 is invited into P_UG (state=invited) and deliberately NEVER
    // accepts -- the "invited is not a member" case (containment requires
    // U4 already be a member of P_UG's parent P_AG_HOME, satisfied above).
    const inviteU4ToUg = await addGroupMembersRaw(systemAdminPage, ug.id, [u4Id]);
    expect(inviteU4ToUg.ok(), `expected success inviting U4 to ${P_UG}, got ${inviteU4ToUg.status()}: ${await inviteU4ToUg.text()}`).toBe(true);

    // --- Two servers under P_SG: X offers the restricted model + becomes
    // P_RG's sole member; Y offers the open model and joins NO resource
    // group at all ------------------------------------------------------------
    const serverX = await createServerRaw(systemAdminPage, { name: P_SERVER_X, domain: "e2e-rgp-mike.example.test", adminGroupId: groupIds[P_AG] });
    const serverY = await createServerRaw(systemAdminPage, { name: P_SERVER_Y, domain: "e2e-rgp-november.example.test", adminGroupId: groupIds[P_AG_HOME] });
    expect(serverX.system_group_id).not.toBe("");
    expect(serverX.system_group_id).toBe(serverY.system_group_id);

    // Each server gets ONE always-reachable application + ONE active
    // mapping, so its model is genuinely OFFERED (both Models() and
    // /v1/models read the SAME activeMappingViews pass this populates)
    // without a real upstream ever being contacted for LIST purposes.
    const appX = await createAppRaw(systemAdminPage, serverX.id);
    await createMappingRaw(systemAdminPage, appX.id, P_MODEL_RESTRICTED, "restricted-upstream-model");
    const appY = await createAppRaw(systemAdminPage, serverY.id);
    await createMappingRaw(systemAdminPage, appY.id, P_MODEL_OPEN, "open-upstream-model");

    // --- A Service Account, provisioned DIRECTLY (kind=service) -----------
    const svc = await createServiceRaw(systemAdminPage, P_SVC, groupIds[P_AG_HOME]);
    const svcSecret = await createServiceTokenRaw(systemAdminPage, svc.id);

    // --- The resource group under test: linked to P_AG (its containment
    // root becomes P_SG), with SERVER_X (never SERVER_Y) as its sole member -
    const rg = await createResourceGroupRaw(systemAdminPage, { name: P_RG, adminGroupIds: [groupIds[P_AG]] });
    expect(rg.system_group.name).toBe(P_SG);
    const enterX = await putServersRaw(systemAdminPage, rg.id, [serverX.id]);
    expect(enterX.ok(), `expected success, got ${enterX.status()}: ${await enterX.text()}`).toBe(true);
    expect(((await enterX.json()) as ResourceGroupRow).servers).toEqual([{ id: serverX.id, name: P_SERVER_X }]);

    // Provisioned FOR all four kinds at once: the admin group (grants U),
    // the user group (grants U2, NOT U4 who is merely invited), the direct
    // user U3, and the service.
    const setProv = await putProvisionsRaw(systemAdminPage, rg.id, [
      { kind: "admin_group", target_id: groupIds[P_AG] },
      { kind: "user_group", target_id: ug.id },
      { kind: "user", target_id: u3Id },
      { kind: "service", target_id: svc.id }
    ]);
    expect(setProv.ok(), `expected success, got ${setProv.status()}: ${await setProv.text()}`).toBe(true);

    // === Opt-in mode (the default; explicitly pinned above) =================
    const uModelIdsOptIn = await listModelIds(uPage);
    const u2ModelIdsOptIn = await listModelIds(u2Page);
    const u3ModelIdsOptIn = await listModelIds(u3Page);
    const u4ModelIdsOptIn = await listModelIds(u4Page);
    const vModelIdsOptIn = await listModelIds(vPage);
    const svcModelIdsOptIn = await listModelIdsViaBearer(systemAdminPage, svcSecret);

    // (1) N_OPEN (an unrestricted server -- not a member of ANY resource
    // group) is visible+usable to EVERY principal, provisioned or not --
    // opt-in's whole point: provisioning is a no-op for a server nobody has
    // opted into it for.
    expect(uModelIdsOptIn, "U should see the open model").toContain(P_MODEL_OPEN);
    expect(u2ModelIdsOptIn, "U2 should see the open model").toContain(P_MODEL_OPEN);
    expect(u3ModelIdsOptIn, "U3 should see the open model").toContain(P_MODEL_OPEN);
    expect(u4ModelIdsOptIn, "U4 should see the open model").toContain(P_MODEL_OPEN);
    expect(vModelIdsOptIn, "V should see the open model").toContain(P_MODEL_OPEN);
    expect(svcModelIdsOptIn, "the service token should see the open model").toContain(P_MODEL_OPEN);

    // (2) M_RESTRICTED IS visible to each of the four provisioned-for kinds.
    expect(uModelIdsOptIn, "U (admin_group provision) should see the restricted model").toContain(P_MODEL_RESTRICTED);
    expect(u2ModelIdsOptIn, "U2 (user_group provision) should see the restricted model").toContain(P_MODEL_RESTRICTED);
    expect(u3ModelIdsOptIn, "U3 (direct user provision) should see the restricted model").toContain(P_MODEL_RESTRICTED);
    expect(svcModelIdsOptIn, "the service token (direct service provision) should see the restricted model").toContain(P_MODEL_RESTRICTED);

    // (3) ... and hidden from V (provisioned nowhere) and U4 (INVITED to the
    // provisioned user group, never accepted -- state=invited never counts).
    expect(u4ModelIdsOptIn, "U4 (invited, not a member) should NOT see the restricted model").not.toContain(P_MODEL_RESTRICTED);
    expect(vModelIdsOptIn, "V (provisioned nowhere) should NOT see the restricted model").not.toContain(P_MODEL_RESTRICTED);

    // (4) Routing-layer enforcement, network-free + non-flaky (the "not just
    // the list" proof): V has ZERO candidate servers for M_RESTRICTED once
    // filterProvisioned runs (the ONLY server offering it, X, is excluded
    // for her), so Resolve() returns routing.ErrNoModelRoute straight from
    // `if len(candidates) == 0 { return Target{}, ErrNoModelRoute }` --
    // BEFORE any provider dial is attempted (see internal/routing/resolver.go).
    // This is a deterministic 502, never a real upstream connection attempt,
    // so it carries none of a live-inference test's network flakiness. A
    // request from an ALLOWED principal (which WOULD attempt to dial the
    // fake server domain) is deliberately not exercised here for that exact
    // reason; the pinned-affinity re-check (a provisioned server pinned via
    // a real API token, later un-provisioned, must stop being served) is
    // separately mutation-proven at the unit level in internal/routing.
    const vDenied = await vPage.request.post("/v1/chat/completions", {
      headers: CSRF,
      data: { model: P_MODEL_RESTRICTED, messages: [{ role: "user", content: "hello" }], stream: false }
    });
    expect(vDenied.status(), `expected 502 no-route, got ${vDenied.status()}: ${await vDenied.text()}`).toBe(502);
    expect((await vDenied.json() as ApiErrorBody).error?.code).toBe("routing.no_model_route");

    // === UI smoke: the provisioning editor shows the four targets just set ==
    await gotoResourceGroups(systemAdminPage);
    await openResourceGroupDetail(systemAdminPage, P_RG);
    const provisionsSection = systemAdminPage.getByRole("region", { name: t.resourceGroupProvisionsSectionTitle });
    await expect(provisionsSection.getByText(P_AG)).toBeVisible();
    await expect(provisionsSection.getByText(P_UG)).toBeVisible();
    await expect(provisionsSection.getByText(P_USER_U3)).toBeVisible();
    await expect(provisionsSection.getByText(P_SVC)).toBeVisible();

    // === Deny mode ============================================================
    await setResourceProvisioningEnforce(systemAdminPage, true);

    // Light UI smoke of the toggle itself: System Settings reflects the
    // freshly-flipped value on load. The fixed "System-Admin-Modus aktiv"
    // elevation banner geometrically overlaps the bottom nav items (a
    // pre-existing layout quirk, unrelated to this feature) -- it sits
    // directly on top of the "System" link's whole row, so even a real
    // pointer click at that link's coordinates lands on the banner instead
    // (force:true only skips Playwright's own pre-check, not the browser's
    // point-based hit test). `.evaluate(el => el.click())` calls the DOM
    // click() method directly on the anchor node, bypassing hit-testing
    // entirely while still firing React's onClick handler via normal event
    // bubbling.
    const [systemSettingsResp] = await Promise.all([
      systemAdminPage.waitForResponse((r) => new URL(r.url()).pathname === "/api/system/settings" && r.request().method() === "GET"),
      systemAdminPage.getByRole("link", { name: t.system }).evaluate((el: HTMLElement) => el.click())
    ]);
    expect(systemSettingsResp.ok()).toBe(true);
    await expect(systemAdminPage.getByRole("checkbox", { name: t.settingsResourceProvisioningEnforceLabel })).toBeChecked();

    // V: NEITHER model -- Y is unprovisioned, so deny-by-default now denies
    // it to everyone (not just to non-targets); X was already denied to V
    // under opt-in too.
    const vModelIdsDeny = await listModelIds(vPage);
    expect(vModelIdsDeny, "V under deny mode should see neither model").not.toContain(P_MODEL_RESTRICTED);
    expect(vModelIdsDeny, "V under deny mode should see neither model").not.toContain(P_MODEL_OPEN);

    // U: M_RESTRICTED still visible (still provisioned via P_AG); N_OPEN now
    // hidden (nobody is provisioned for Y under deny-by-default).
    const uModelIdsDeny = await listModelIds(uPage);
    expect(uModelIdsDeny, "U under deny mode should still see the restricted model").toContain(P_MODEL_RESTRICTED);
    expect(uModelIdsDeny, "U under deny mode should no longer see the open model").not.toContain(P_MODEL_OPEN);

    // Restore the default so this test's own side effect never leaks into a
    // later run of this file.
    await setResourceProvisioningEnforce(systemAdminPage, false);
  });
});

test.describe("Server override — provisioning bypass (Task 8, spec: 2026-08-12-server-override-and-portal-polish)", () => {
  test("a server-manager's token with server_override bypasses deny-mode provisioning (contrasted against a no-override token/session, which hits routing.no_model_route); GET .../models is manager-gated 404-no-leak; a maintenance-status target still routes via override; losing management is caught at REQUEST time (403, independent of any write) and separately self-heals at the next WRITE; the frontend token-create form's picker is present for the manager and absent for a non-manager", async ({
    page: systemAdminPage,
    browser,
    request
  }) => {
    await login(systemAdminPage, RESOURCE_GROUPS_ADMIN_EMAIL, RESOURCE_GROUPS_ADMIN_PASSWORD);
    await enterSystemAdminMode(systemAdminPage, RESOURCE_GROUPS_ADMIN_NAME, RESOURCE_GROUPS_ADMIN_PASSWORD);

    // Defensive baseline, independent of test order/earlier tests in this file
    // (the Phase 2 describe above always restores it to false on exit, but
    // pin it explicitly so this test's own starting state never depends on
    // that).
    await setResourceProvisioningEnforce(systemAdminPage, false);

    // --- Setup: one system group + one admin group; U (the future server
    // owner) and V (a plain peer who owns/manages nothing) both join as
    // ordinary MEMBERS -- neither is promoted to co-manager, and neither is
    // an admin/system_admin. Server X's management therefore comes from
    // owner_ids ALONE, which is exactly what step (5) below revokes. ---------
    await gotoGroups(systemAdminPage);
    await createSystemGroup(systemAdminPage, SVO_SG);
    await createAdminGroup(systemAdminPage, SVO_AG, SVO_SG);

    const inviteU = await inviteWithAdminGroup(systemAdminPage, { role: "user", adminGroupName: SVO_AG, displayName: SVO_OWNER_U });
    const uPage = await redeemInvite(browser, inviteU.inviteUrl);
    const inviteV = await inviteWithAdminGroup(systemAdminPage, { role: "user", adminGroupName: SVO_AG, displayName: SVO_PEER_V });
    const vPage = await redeemInvite(browser, inviteV.inviteUrl);

    const groupIds = await resolveAdminGroupIds(systemAdminPage, [SVO_AG]);
    const uId = ((await (await uPage.request.get("/api/portal/me")).json()) as CurrentUserResp).id;

    // Server X: owned by U (owner_ids ONLY). Domain 127.0.0.1 + the fixed
    // port/scheme createAppRaw always uses (8000/https) -- nothing listens
    // there in this test environment, so a genuinely ROUTED request fails
    // fast + deterministically with "connection refused" at the TCP layer
    // (no DNS lookup for a literal IP, no dependency on real network egress).
    // This is what lets the bypass proof below be a live, non-flaky, purely
    // loopback assertion rather than a slow/flaky real-domain dial.
    const serverX = await createServerRaw(systemAdminPage, {
      name: SVO_SERVER_X,
      domain: "127.0.0.1",
      adminGroupId: groupIds[SVO_AG],
      ownerId: uId
    });
    const appX = await createAppRaw(systemAdminPage, serverX.id);
    await createMappingRaw(systemAdminPage, appX.id, SVO_MODEL, "svo-upstream-model");

    // Deny-by-default, and SERVER_X is provisioned to NOBODY -- normal
    // routing must refuse it to every principal, U included.
    await setResourceProvisioningEnforce(systemAdminPage, true);

    // === (1) Baseline: with NO override in effect, deny-mode provisioning
    // blocks U just like anyone else -- both via U's own session AND via a
    // plain (non-override) token U mints for herself. This is the exact
    // contrast the override token in (2) must NOT hit. ========================
    const uSessionBaseline = await chatCompletionSession(uPage, SVO_MODEL);
    expect(uSessionBaseline.status(), `expected 502, got ${uSessionBaseline.status()}: ${await uSessionBaseline.text()}`).toBe(502);
    expect((await uSessionBaseline.json() as ApiErrorBody).error?.code).toBe("routing.no_model_route");

    const plainToken = await createTokenRaw(uPage, { name: "e2e-svo-plain" });
    expect(plainToken.token.server_override, "a token created with no override must persist none").toBeFalsy();
    const plainDenied = await chatCompletionBearer(request, plainToken.secret, SVO_MODEL);
    expect(plainDenied.status(), `expected 502, got ${plainDenied.status()}: ${await plainDenied.text()}`).toBe(502);
    expect((await plainDenied.json() as ApiErrorBody).error?.code).toBe("routing.no_model_route");

    // === (2) THE bypass (the core of this task): a token with
    // server_override=X -- settable because U genuinely manages X (owns it)
    // -- routes past provisioning entirely. Proof that the resolver's
    // OVERRIDE branch was truly hit (not merely "not blocked") is that the
    // SAME request now reaches -- and fails at -- the real upstream dial:
    // its error is neither routing.no_model_route (provisioning) NOR either
    // of the override's own gate sentinels (server_override.server_unavailable/
    // model_unavailable would mean the override was rejected before ever
    // dialing) -- it is the provider's own connection-refused failure. =======
    const overrideToken = await createTokenRaw(uPage, { name: "e2e-svo-override", serverOverrideId: serverX.id });
    expect(overrideToken.token.server_override, "the manager's override must persist").toBe(serverX.id);
    const bypassed = await chatCompletionBearer(request, overrideToken.secret, SVO_MODEL);
    expect(bypassed.status(), `expected the override to reach (and fail at) the fake upstream, got ${bypassed.status()}: ${await bypassed.text()}`).toBe(502);
    const bypassedCode = (await bypassed.json() as ApiErrorBody).error?.code;
    expect(bypassedCode, "the override must NOT be blocked by provisioning like (1) was").not.toBe("routing.no_model_route");
    expect(bypassedCode, "must not be rejected by the override's own disabled/unreachable gate (would mean it never actually dialed)").not.toBe("server_override.server_unavailable");
    expect(bypassedCode, "must not be rejected by the override's own model-gap gate").not.toBe("server_override.model_unavailable");
    expect(bypassedCode, "the actual failure mode: a real (mocked-closed-port) upstream dial that was refused").toBe("provider.unavailable");

    // === (3) GET .../servers/{id}/models: manager-gated, 404-no-leak for a
    // non-manager (V never owns/co-manages X) ================================
    const modelsAsU = await getServerModelsRaw(uPage, serverX.id);
    expect(modelsAsU.ok(), `expected success, got ${modelsAsU.status()}: ${await modelsAsU.text()}`).toBe(true);
    const uModelIds = ((await modelsAsU.json()) as { data: { id: string }[] }).data.map((m) => m.id);
    expect(uModelIds).toContain(SVO_MODEL);

    const modelsAsV = await getServerModelsRaw(vPage, serverX.id);
    expect(modelsAsV.status(), `expected 404, got ${modelsAsV.status()}: ${await modelsAsV.text()}`).toBe(404);
    expect((await modelsAsV.json() as ApiErrorBody).error?.code).toBe("server.not_found");

    // === UI smoke (light, per the brief; run HERE -- before step (5) below
    // revokes U's ownership, so U still genuinely manages X): the token-create
    // form's server-override picker is PRESENT for the manager U (App.tsx's
    // `servers` prop = api.servers(), the caller's own manageable set) and
    // OFFERS X as an option; the negative (absent-for-a-non-manager) case is
    // re-checked for BOTH U and V at the very end of this test, once U's
    // ownership has actually been revoked. ====================================
    await uPage.getByRole("link", { name: t.apiTokens, exact: true }).click();
    await uPage.getByRole("button", { name: t.tokenCreate }).click();
    const uOverridePicker = uPage.getByRole("combobox", { name: t.serverOverrideLabel });
    await expect(uOverridePicker).toBeVisible();
    await uOverridePicker.click();
    await expect(uPage.getByRole("option", { name: new RegExp(SVO_SERVER_X) })).toBeVisible();
    await uPage.keyboard.press("Escape");
    await uPage.getByRole("button", { name: t.cancel }).click();

    await vPage.getByRole("link", { name: t.apiTokens, exact: true }).click();
    await vPage.getByRole("button", { name: t.tokenCreate }).click();
    await expect(vPage.getByRole("combobox", { name: t.serverOverrideLabel })).toHaveCount(0);
    await vPage.getByRole("button", { name: t.cancel }).click();

    // === (4) A maintenance-status target is STILL routable via override --
    // resolveServerOverride only refuses `disabled` and unhealthy/unreachable
    // (force-gated); it deliberately does NOT apply serverSelectable's
    // Status==active exclusion (see internal/routing/resolver.go; the
    // disabled/unhealthy/force matrix itself is exhaustively mutation-proven
    // at the unit level in resolver_server_override_test.go -- this is just
    // the live "maintenance specifically is not treated as unavailable"
    // proof) ===================================================================
    const setMaintenance = await patchServerRaw(systemAdminPage, serverX.id, { status: "maintenance" });
    expect(setMaintenance.ok(), `expected success, got ${setMaintenance.status()}: ${await setMaintenance.text()}`).toBe(true);
    const duringMaintenance = await chatCompletionBearer(request, overrideToken.secret, SVO_MODEL);
    const maintenanceCode = (await duringMaintenance.json() as ApiErrorBody).error?.code;
    expect(maintenanceCode, "maintenance must not be treated as disabled/unavailable by the override gate").not.toBe("server_override.server_unavailable");
    expect(maintenanceCode, "still reaches (and fails at) the same fake upstream as before").toBe("provider.unavailable");
    const restoreActive = await patchServerRaw(systemAdminPage, serverX.id, { status: "active" });
    expect(restoreActive.ok(), `expected success, got ${restoreActive.status()}: ${await restoreActive.text()}`).toBe(true);

    // === (5) Losing management. First, the RUNTIME re-check
    // (AuthorizeServerManage, on every routed request -- independent of any
    // write-time self-heal) catches an outlived grant IMMEDIATELY: 403, never
    // a silent reroute or a stale success. =====================================
    const revokeOwnership = await patchServerRaw(systemAdminPage, serverX.id, { owner_ids: [] });
    expect(revokeOwnership.ok(), `expected success, got ${revokeOwnership.status()}: ${await revokeOwnership.text()}`).toBe(true);

    const outlivedGrant = await chatCompletionBearer(request, overrideToken.secret, SVO_MODEL);
    expect(outlivedGrant.status(), `expected 403, got ${outlivedGrant.status()}: ${await outlivedGrant.text()}`).toBe(403);
    expect((await outlivedGrant.json() as ApiErrorBody).error?.code).toBe("server_override.forbidden");

    // Second, the WRITE-TIME self-heal: U (still the token's owner, still
    // logged in) PATCHes the token with a field that has NOTHING to do with
    // the override -- proving the clear is UNCONDITIONAL, not gated on the
    // request touching server_override -- and the returned DTO comes back
    // with the override already cleared.
    const selfHealPatch = await patchTokenRaw(uPage, overrideToken.token.id, { name: "e2e-svo-override-renamed" });
    expect(selfHealPatch.ok(), `expected success, got ${selfHealPatch.status()}: ${await selfHealPatch.text()}`).toBe(true);
    const healedToken = (await selfHealPatch.json()) as TokenRow;
    expect(healedToken.name).toBe("e2e-svo-override-renamed");
    expect(healedToken.server_override, "an unrelated PATCH must still self-heal the stale override").toBeFalsy();

    // And the SAME still-valid secret, reused after the self-heal, now hits
    // the identical deny-mode wall the plain token hit in (1) -- proof the
    // clear took effect at RUNTIME, not merely in the returned DTO.
    const afterHeal = await chatCompletionBearer(request, overrideToken.secret, SVO_MODEL);
    expect(afterHeal.status(), `expected 502, got ${afterHeal.status()}: ${await afterHeal.text()}`).toBe(502);
    expect((await afterHeal.json() as ApiErrorBody).error?.code).toBe("routing.no_model_route");

    // === UI smoke, part 2: now that U's ownership of X has actually been
    // revoked (step 5 above), the SAME picker that was visible+populated for
    // U earlier is gone. App.tsx's `servers` prop is loaded once at mount
    // (bootstrap/loadPortalData), not on every navigation, so U's OWN browser
    // session is still holding the pre-revocation snapshot -- a reload forces
    // the fresh api.servers() fetch that reflects the CURRENT owner_ids (this
    // is a client-side staleness artifact, not a security gap: the runtime
    // 403 in step (5) above already proved the backend enforces the live
    // state regardless of what any client happens to be caching). V (who
    // never managed anything) needs no reload -- her set was always empty. ===
    await uPage.reload();
    await uPage.getByRole("link", { name: t.apiTokens, exact: true }).click();
    await uPage.getByRole("button", { name: t.tokenCreate }).click();
    await expect(uPage.getByRole("combobox", { name: t.serverOverrideLabel })).toHaveCount(0);

    await vPage.getByRole("link", { name: t.apiTokens, exact: true }).click();
    await vPage.getByRole("button", { name: t.tokenCreate }).click();
    await expect(vPage.getByRole("combobox", { name: t.serverOverrideLabel })).toHaveCount(0);

    // Restore the default so this test's own side effect never leaks into a
    // later run of this file.
    await setResourceProvisioningEnforce(systemAdminPage, false);
  });
});
