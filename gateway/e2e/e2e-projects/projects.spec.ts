// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { expect, test, type Browser, type Page } from "@playwright/test";
import { messages } from "../../frontend/src/i18n";
import { login, uniqueEmail, setPassword } from "../e2e/helpers";
import { PROJECTS_ADMIN_EMAIL, PROJECTS_ADMIN_NAME, PROJECTS_ADMIN_PASSWORD } from "../playwright.projects.config";

const t = messages.de;
const GW = "http://127.0.0.1:8091";

// Seeded on the fresh sqlite DB by cmd/gateway's seedDefaultServerIfEmpty --
// the SAME mock server/app/mappings as memory mode (qwen-coder / gpt-oss-20b);
// see playwright.projects.config.ts's header comment for why this suite runs
// in sqlite mode rather than the default memory mode.
const MODEL = "qwen-coder";

// >= 10 chars per the password policy in internal/auth. Test-only.
const USER_PASSWORD = "E2E-Projects-Pass-1";

// The Users-invite form now REQUIRES picking an ADMIN group (spec:
// 2026-08-09-group-visibility-admin-group-invite-design.md; replaces the old
// system-group multi-select this file used to drive), and enrolls the
// invitee DIRECTLY (state=member, no invite/accept step) in BOTH that admin
// group AND its parent system group in one shot
// (portal.Service.AddUserToAdminGroup). This test needs C to end up a
// member of SG_MAIN specifically -- the SYSTEM group later assigned
// DIRECTLY to the project as a project GROUP, so C's membership makes C an
// EFFECTIVE project member via that group (spec §4 rule 3) -- but D must end
// up a member of NEITHER SG_MAIN nor any group assigned to the project.
// So two disjoint admin-group hierarchies are built: AG_MAIN (parent
// SG_MAIN, into which B and C are invited) and AG_DECOY (parent SG_DECOY,
// into which D is invited). Were D invited into AG_MAIN too, D would ALSO
// become an SG_MAIN member and thus an unintended EFFECTIVE project member
// once SG_MAIN is assigned to the project below (portal.Service.
// memberProjectIDs resolves membership direct-OR-via-any-assigned-group) --
// defeating the "D is a member of NEITHER project_members NOR SG_MAIN, and
// the project must not appear in D's own landscape at all" distinction this
// test relies on. Two system groups (rather than one) also guarantee the
// admin-group CREATE form's parent picker (t.groupsParentLabel) actually
// renders for the bootstrap system_admin regardless of test-run order/
// isolation (it only auto-resolves with no picker when the system_admin's
// own system-tier list has exactly one entry -- see
// createAdminGroupWithParent's doc comment).
const SG_MAIN = "E2E Projects Main Group";
const SG_DECOY = "E2E Projects Decoy Group";
const AG_MAIN = "E2E Projects Main Admin Group";
const AG_DECOY = "E2E Projects Decoy Admin Group";

const PROJECT_NAME = "E2E Test Project";
const PROJECT_DESCRIPTION = "e2e project attribution + scope test";

// Distinct display names (no shared suffix) so the project's user-candidate
// MultiSelectField -- whose options render as the display name and email
// concatenated with NO separator (MultiSelectField.renderOption; see
// e2e-groups/groups.spec.ts's addCandidate doc comment for the identical
// gotcha) -- can be matched unambiguously by a plain (non-exact) substring.
const NAME_B = "E2E Project User B";
const NAME_C = "E2E Project User C";
const NAME_D = "E2E Project User D";

type Project = {
  id: string;
  name: string;
  description: string;
  owner_user_id: string;
  my_role: string;
  can_manage: boolean;
  member_count: number;
  group_count: number;
  // Coupled-projects (spec 2026-08-09): non-empty iff this project is coupled
  // to a user-tier group -- owner_user_id/my_role/can_manage above are then
  // the DERIVED (group-owner) values, not the project's own stored row.
  // Mirrors portal.ProjectDTO.CoupledGroupID/-Name exactly. Optional (and
  // absent from every assertion in the non-coupled scenario above) so this
  // extension is a no-op for the existing test.
  coupled_group_id?: string;
  coupled_group_name?: string;
};

type UsageGroupRow = {
  key: string;
  key_label: string;
  count: number;
  error_count: number;
};

// One API token attached to a project (owner/admin view via
// GET /api/portal/projects/{id}/tokens) -- mirrors portal.ProjectTokenDTO /
// frontend api.ts's ProjectToken. Never carries a secret/hash. The four usage
// fields are this token's ALL-TIME usage ATTRIBUTED TO THIS PROJECT (rows
// where project_id equals the project's id AND token_id equals this token's
// id) -- a token with no matching usage reads all zeros, never omitted.
type ProjectToken = {
  id: string;
  name: string;
  secret_prefix: string;
  owner_user_id: string;
  owner_name: string;
  status: string;
  created_at: string;
  last_used_at?: string;
  request_count: number;
  input_tokens: number;
  output_tokens: number;
  total_tokens: number;
};

// A project's TRUE total usage: the sum over EVERY usage_events row with this
// project's project_id, regardless of whether the row's token is still
// attached -- mirrors portal.ProjectTokenUsageTotalDTO. May EXCEED the sum of
// ProjectTokensView.tokens' per-row totals (a detached/deleted token's
// historical usage still counts here but not on any visible row).
type ProjectTokenUsageTotal = {
  request_count: number;
  input_tokens: number;
  output_tokens: number;
  total_tokens: number;
};

// GET /api/portal/projects/{id}/tokens' response shape -- mirrors
// portal.ProjectTokensView.
type ProjectTokensView = {
  tokens: ProjectToken[];
  total: ProjectTokenUsageTotal;
};

// The subset of a caller's OWN token (GET /api/portal/tokens) this suite
// needs -- just enough to confirm a detached token still exists for its
// owner with its project attribution cleared.
type OwnToken = {
  id: string;
  project_id?: string;
};

/**
 * Enters the System-Admin (step-up) mode. Since the step-up-mode merge, a
 * bootstrap system_admin holds only ADMIN scope+sight by default and must
 * explicitly elevate to gain the `system` scope before any system-tier action
 * (creating a system group, seeing the System panel). `system_admin_mode_
 * require_password` defaults TRUE, so the enter flow opens a password dialog.
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
  // The dialog closes once elevation lands (CurrentUser.system_admin_mode
  // flips true) -- wait for it to be gone before any caller proceeds, so a
  // subsequent system-scoped action never races the still-open dialog.
  await expect(dialog).toHaveCount(0);
}

/** Creates a system-tier group (system_admin only; no parent, no landscape-fetch race). */
async function createSystemGroup(page: Page, name: string): Promise<void> {
  await page.getByRole("button", { name: t.groupsCreateSystemTitle }).click();
  await page.locator("#group-name").fill(name);
  await page.getByRole("button", { name: t.save }).click();
  await expect(page.getByRole("row", { name: new RegExp(name) })).toBeVisible();
}

/**
 * Creates an admin-tier group as `page` (system_admin in every call site in
 * this file), picking `parentName` explicitly in the create form's parent
 * `SearchableSelect` (spec §7.2's "system_admin darf aus allen wählen" --
 * may choose ANY system group as parent, not just one it belongs to).
 * Mirrors e2e-groups/groups.spec.ts's createAdminGroupWithParent. The picker
 * only RENDERS when the caller manages more than one system group (with
 * exactly one it auto-resolves silently, no control at all) -- every call
 * site here creates >= 2 system groups first, so the picker always renders.
 * Returns the created group so callers that need its id (to add a member to
 * it directly, or to have someone leave it) don't need a second round trip.
 */
async function createAdminGroupWithParent(page: Page, name: string, parentName: string): Promise<{ id: string; name: string }> {
  await page.getByRole("button", { name: t.groupsCreateAdminTitle }).click();
  await page.locator("#group-name").fill(name);
  await page.getByRole("combobox", { name: t.groupsParentLabel }).click();
  await page.getByRole("option", { name: parentName, exact: true }).click();
  const [resp] = await Promise.all([
    page.waitForResponse((r) => new URL(r.url()).pathname === "/api/portal/groups" && r.request().method() === "POST"),
    page.getByRole("button", { name: t.save }).click()
  ]);
  expect(resp.ok()).toBe(true);
  const group = (await resp.json()) as { id: string; name: string };
  await expect(page.getByRole("row", { name: new RegExp(name) })).toBeVisible();
  return group;
}

/**
 * Invites a fresh "user"-role principal through the admin Users view,
 * picking `adminGroupName` in the invite form's MANDATORY admin-group
 * picker (spec: 2026-08-09-group-visibility-admin-group-invite-design.md's
 * `userInviteAdminGroupLabel` combobox -- replaces the old system-group
 * multi-select this file used to drive; mirrors
 * e2e-groups/groups.spec.ts's inviteWithAdminGroup, narrowed to a fixed
 * "user" role, nobody in this suite needs admin/system_admin here). On
 * submit the new user is enrolled DIRECTLY (state=member, no invite/accept
 * step) in BOTH the chosen admin group AND its parent system group in one
 * shot (portal.Service.AddUserToAdminGroup). Returns the invite URL +
 * generated email for redemption.
 */
async function inviteProjectsUser(adminPage: Page, displayName: string, adminGroupName: string): Promise<{ email: string; inviteUrl: string }> {
  await adminPage.getByRole("link", { name: t.users }).click();
  await adminPage.getByRole("button", { name: t.userCreate }).click();
  const email = uniqueEmail();
  await adminPage.locator("#user-email").fill(email);
  await adminPage.locator("#user-name").fill(displayName);
  await adminPage.getByRole("combobox", { name: t.userInviteAdminGroupLabel }).click();
  await adminPage.getByRole("option", { name: adminGroupName, exact: true }).click();
  await adminPage.getByRole("button", { name: t.userCreate }).click();
  const inviteUrl = (await adminPage.locator('[data-testid="secret-reveal"] code').innerText()).trim();
  await adminPage.getByRole("button", { name: t.captureClose }).click();
  return { email, inviteUrl };
}

/**
 * Redeems a set-password invite in a FRESH, isolated browser context (its own
 * cookies/session) and lands on the authenticated app -- mirrors
 * e2e-limits/limits.spec.ts's inviteUserWithLimitsAndToken /
 * e2e-groups/groups.spec.ts's redeemInvite.
 */
async function redeemInvite(browser: Browser, inviteUrl: string): Promise<Page> {
  const context = await browser.newContext({ baseURL: "http://127.0.0.1:4173" });
  const page = await context.newPage();
  await setPassword(page, inviteUrl, USER_PASSWORD);
  // account.Service.SetPassword redeems the token, sets the password, AND
  // creates a session -- the invited user is already logged in at this point.
  await page.goto("/portal/");
  await expect(page.getByText(t.welcome)).toBeVisible();
  return page;
}

/**
 * Creates a project via the ProjectsView create form and returns the created
 * ProjectDTO captured straight off the POST /api/portal/projects response
 * (the UI never displays the project's raw id, but later steps -- the
 * token-assign rejection + the usage/groups project_id_exact-style checks --
 * need it).
 */
async function createProjectViaUI(page: Page, name: string, description: string): Promise<Project> {
  await page.getByRole("button", { name: t.projectsCreateTitle }).click();
  await page.locator("#project-name").fill(name);
  await page.locator("#project-description").fill(description);
  const [resp] = await Promise.all([
    page.waitForResponse((r) => new URL(r.url()).pathname === "/api/portal/projects" && r.request().method() === "POST"),
    page.getByRole("button", { name: t.save }).click()
  ]);
  expect(resp.ok()).toBe(true);
  return (await resp.json()) as Project;
}

/** Opens the "Mitglieder verwalten" sub-view for the project row named `projectName`. */
async function openProjectMembers(page: Page, projectName: string): Promise<void> {
  const row = page.getByRole("row", { name: new RegExp(projectName) });
  await row.getByRole("button", { name: t.projectsActionMembers }).click();
}

/**
 * Adds `memberSubstring` (a user's display name) via the members sub-view's
 * FIRST <form> (the user-candidate MultiSelectField + its own "Hinzufügen"
 * submit) -- the members sub-view renders TWO forms with an IDENTICALLY
 * labelled submit button (t.projectsActionAdd: one for users, one for
 * groups), so both the combobox and the submit button are scoped to
 * `form:nth(0)`; only the popup's OPTION is looked up on `page` (MUI portals
 * the popup to document.body, outside either form's DOM subtree).
 */
async function addProjectMemberUser(page: Page, memberSubstring: string): Promise<void> {
  const usersForm = page.locator("form").nth(0);
  await usersForm.getByRole("combobox", { name: t.projectsAddMembersLabel }).click();
  await page.getByRole("option", { name: memberSubstring }).click();
  await usersForm.getByRole("button", { name: t.projectsActionAdd }).click();
}

/** Adds `groupName` via the members sub-view's SECOND <form> (the group-candidate MultiSelectField). */
async function addProjectMemberGroup(page: Page, groupName: string): Promise<void> {
  const groupsForm = page.locator("form").nth(1);
  await groupsForm.getByRole("combobox", { name: t.projectsAddGroupsLabel }).click();
  await page.getByRole("option", { name: groupName, exact: true }).click();
  await groupsForm.getByRole("button", { name: t.projectsActionAdd }).click();
}

/**
 * Creates a personal API token attributed to `projectName` via the Tokens
 * view's project SearchableSelect (t.tokenProjectLabel), returning the
 * one-time secret.
 */
async function createProjectToken(page: Page, tokenName: string, projectName: string): Promise<string> {
  await page.getByRole("link", { name: t.apiTokens }).click();
  await page.getByRole("button", { name: t.tokenCreate }).click();
  await page.locator("#token-name").fill(tokenName);
  await page.getByRole("combobox", { name: t.tokenProjectLabel }).click();
  await page.getByRole("option", { name: projectName, exact: true }).click();
  await page.getByRole("button", { name: t.tokenCreate }).click();
  const secret = (await page.locator('[data-testid="secret-reveal"] code').innerText()).trim();
  expect(secret.length).toBeGreaterThan(0);
  await page.getByRole("button", { name: t.captureClose }).click();
  return secret;
}

// --- Coupled-projects (spec 2026-08-09) helpers -----------------------------
// A coupled project's owner is DERIVED from a user-tier group's current
// owner, so an "owner user" for the coupled scenario below must first climb
// the hierarchy: System group -> (member of it) -> Admin group (auto-parent,
// owner+member of the group they create) -> (member of exactly one admin
// group) -> eligible to create_coupled_group. This mirrors
// e2e-groups/groups.spec.ts's createSystemGroup/createAdminGroup/
// createUserGroup recipe one tier further than the non-coupled scenario
// above needs. Getting the owner-to-be a SYSTEM-group membership now
// necessarily ALSO enrolls them in an admin group (the invite form's picker
// is mandatory, spec 2026-08-09-group-visibility-admin-group-invite-
// design.md) — so they LEAVE that landing admin group right after
// redemption (see the test body) before creating their OWN, restoring the
// "member of exactly one admin group" state createCoupledProjectViaUI needs.

/**
 * Invites a fresh "admin"-role principal through the admin Users view,
 * picking `adminGroupName` in the invite form's mandatory admin-group
 * picker -- mirrors inviteProjectsUser, narrowed to a fixed "admin" role
 * (this suite only ever needs ONE admin-role invite: the coupled project's
 * owner-to-be).
 */
async function inviteProjectsAdmin(adminPage: Page, displayName: string, adminGroupName: string): Promise<{ email: string; inviteUrl: string }> {
  await adminPage.getByRole("link", { name: t.users }).click();
  await adminPage.getByRole("button", { name: t.userCreate }).click();
  const email = uniqueEmail();
  await adminPage.locator("#user-email").fill(email);
  await adminPage.locator("#user-name").fill(displayName);
  await adminPage.getByRole("combobox", { name: t.tableRole }).click();
  await adminPage.getByRole("option", { name: t.roleAdmin, exact: true }).click();
  await adminPage.getByRole("combobox", { name: t.userInviteAdminGroupLabel }).click();
  await adminPage.getByRole("option", { name: adminGroupName, exact: true }).click();
  await adminPage.getByRole("button", { name: t.userCreate }).click();
  const inviteUrl = (await adminPage.locator('[data-testid="secret-reveal"] code').innerText()).trim();
  await adminPage.getByRole("button", { name: t.captureClose }).click();
  return { email, inviteUrl };
}

/**
 * Navigates to the Groups view and waits for the landscape fetch
 * (GET /api/portal/groups) to complete -- mirrors
 * e2e-groups/groups.spec.ts's gotoGroups (see its doc comment: without this
 * wait, opening the admin-group create form before the fetch lands would
 * see an empty parent-option list and refuse to auto-resolve).
 */
async function gotoGroups(page: Page): Promise<void> {
  const [resp] = await Promise.all([
    page.waitForResponse((r) => new URL(r.url()).pathname === "/api/portal/groups" && r.request().method() === "GET"),
    page.getByRole("link", { name: t.groups, exact: true }).click()
  ]);
  expect(resp.ok()).toBe(true);
}

/**
 * Creates an admin-tier group as `page` -- the actor must be a member of
 * EXACTLY ONE system group so the parent auto-resolves (spec §7.2, mirrors
 * e2e-groups/groups.spec.ts's createAdminGroup), and captures the create
 * response so the caller gets the new group's id without a second round
 * trip (the group id is needed below to add the coupled project's
 * group-only member directly to this admin group -- containment §5.2).
 */
async function createAdminGroup(page: Page, name: string): Promise<{ id: string; name: string }> {
  await page.getByRole("button", { name: t.groupsCreateAdminTitle }).click();
  await page.locator("#group-name").fill(name);
  const [resp] = await Promise.all([
    page.waitForResponse((r) => new URL(r.url()).pathname === "/api/portal/groups" && r.request().method() === "POST"),
    page.getByRole("button", { name: t.save }).click()
  ]);
  expect(resp.ok()).toBe(true);
  const group = (await resp.json()) as { id: string; name: string };
  await expect(page.getByRole("row", { name: new RegExp(name) })).toBeVisible();
  return group;
}

/**
 * Creates a COUPLED project (spec 2026-08-09) via the ProjectsView create
 * form's "Mit einer Gruppe koppeln" ("Couple to a group") toggle ->
 * "Neue Gruppe erstellen" ("Create a new group") radio mode -- mirrors
 * createProjectViaUI, capturing the POST response directly so the caller
 * gets the derived coupled_group_id/owner_user_id without a second round
 * trip. The actor must be a member of EXACTLY ONE admin group so the new
 * group's parent auto-resolves (ProjectsView.tsx's loadCoupleGroups
 * pre-fills the parent when the caller's admin-group landscape has exactly
 * one entry -- no parent-picker interaction needed here).
 */
async function createCoupledProjectViaUI(
  page: Page,
  name: string,
  description: string,
  newGroupName: string
): Promise<Project> {
  await page.getByRole("button", { name: t.projectsCreateTitle }).click();
  await page.locator("#project-name").fill(name);
  await page.locator("#project-description").fill(description);
  await page.getByRole("checkbox", { name: t.projectsCoupleToggle }).check();
  await page.getByRole("radio", { name: t.projectsCoupleModeCreate }).check();
  await page.locator("#project-couple-new-name").fill(newGroupName);
  const [resp] = await Promise.all([
    page.waitForResponse((r) => new URL(r.url()).pathname === "/api/portal/projects" && r.request().method() === "POST"),
    page.getByRole("button", { name: t.save }).click()
  ]);
  expect(resp.ok()).toBe(true);
  return (await resp.json()) as Project;
}

test.describe("Projekte (Projects) — creation, direct+group membership, token attribution, usage group-by, project scope", () => {
  test("end-to-end: create, assign, attribute, attempt-reject, infer, group-by-project, cross-member scope", async ({
    page: adminPage,
    request,
    browser
  }) => {
    await login(adminPage, PROJECTS_ADMIN_EMAIL, PROJECTS_ADMIN_PASSWORD);
    await enterSystemAdminMode(adminPage, PROJECTS_ADMIN_NAME, PROJECTS_ADMIN_PASSWORD);

    // --- Setup: two system groups + an admin group under each (see the
    // SG_MAIN/AG_MAIN/SG_DECOY/AG_DECOY doc comment above) --------------------
    await adminPage.getByRole("link", { name: t.groups, exact: true }).click();
    await createSystemGroup(adminPage, SG_MAIN);
    await createSystemGroup(adminPage, SG_DECOY);
    await createAdminGroupWithParent(adminPage, AG_MAIN, SG_MAIN);
    await createAdminGroupWithParent(adminPage, AG_DECOY, SG_DECOY);

    // --- Invite B into AG_MAIN (-> will be a DIRECT project member anyway),
    // C into AG_MAIN too (-> its parent SG_MAIN membership is what will make
    // C a project member ONLY via the group), D into the DISJOINT AG_DECOY
    // (-> never an SG_MAIN member, never added to the project -> the
    // non-member for both the token-assign rejection and the "does not see
    // the project's usage" checks) --------------------------------------------
    const inviteB = await inviteProjectsUser(adminPage, NAME_B, AG_MAIN);
    const inviteC = await inviteProjectsUser(adminPage, NAME_C, AG_MAIN);
    const inviteD = await inviteProjectsUser(adminPage, NAME_D, AG_DECOY);

    const bPage = await redeemInvite(browser, inviteB.inviteUrl);
    const cPage = await redeemInvite(browser, inviteC.inviteUrl);
    const dPage = await redeemInvite(browser, inviteD.inviteUrl);

    // --- (1) A (the bootstrap admin) creates a project ----------------------
    await adminPage.getByRole("link", { name: t.projects }).click();
    const project = await createProjectViaUI(adminPage, PROJECT_NAME, PROJECT_DESCRIPTION);
    expect(project.id.length).toBeGreaterThan(0);
    expect(project.name).toBe(PROJECT_NAME);
    expect(project.my_role).toBe("owner");

    // --- (2) A assigns B directly, and SG_MAIN (containing member C) --------
    await openProjectMembers(adminPage, PROJECT_NAME);
    await addProjectMemberUser(adminPage, NAME_B);
    await expect(adminPage.getByText(NAME_B)).toBeVisible();
    await addProjectMemberGroup(adminPage, SG_MAIN);
    await expect(adminPage.getByText(SG_MAIN)).toBeVisible();

    // C is NOT a direct project_members row -- eligibility flows entirely
    // through SG_MAIN (spec §4 rule 3: state=member of an assigned group).
    // Confirmed from C's own perspective: the project shows up in C's "Meine
    // Gruppen"-style landscape (GET /api/portal/projects) with my_role=member.
    await cPage.getByRole("link", { name: t.projects }).click();
    const cRow = cPage.getByRole("row", { name: new RegExp(PROJECT_NAME) });
    await expect(cRow).toBeVisible();
    await expect(cRow.getByText(t.projectsRoleMember)).toBeVisible();

    // B is a member too (direct row) -- same assertion from B's side.
    await bPage.getByRole("link", { name: t.projects }).click();
    const bRow = bPage.getByRole("row", { name: new RegExp(PROJECT_NAME) });
    await expect(bRow).toBeVisible();
    await expect(bRow.getByText(t.projectsRoleMember)).toBeVisible();

    // D is a member of NEITHER project_members NOR SG_MAIN -- the project must
    // not appear in D's own landscape at all.
    await dPage.getByRole("link", { name: t.projects }).click();
    await expect(dPage.getByRole("row", { name: new RegExp(PROJECT_NAME) })).toHaveCount(0);

    // --- (3) A attaches one of A's OWN tokens to the project (allowed) ------
    const secret = await createProjectToken(adminPage, "A Project Token", PROJECT_NAME);

    // A NON-member's (D's) attempt to attach a token to A's project is
    // rejected -- server-enforced (portal.Service.assignTokenProject), not
    // merely hidden by the UI's picker (D's own Tokens form would never even
    // OFFER this project, since its options come from api.myProjects()). D's
    // OWN authenticated session (page.request shares the browsing context's
    // cookies) drives a raw POST that bypasses the picker entirely.
    const rejectRes = await dPage.request.post("/api/portal/tokens", {
      // Cookie-authenticated state-changing requests require the CSRF header
      // (internal/gateway/auth.go's csrfOK) -- api.ts sets this on every
      // mutating call; a raw page.request bypasses api.ts entirely, so it
      // must be supplied here explicitly.
      headers: { "X-OP-CSRF": "1" },
      data: { name: "D Reject Token", scopes: ["gateway:use"], project_id: project.id }
    });
    expect(rejectRes.status(), `expected 403, got ${rejectRes.status()}: ${await rejectRes.text()}`).toBe(403);
    const rejectBody = await rejectRes.json();
    expect(rejectBody.error?.code).toBe("token.project_not_member");

    // --- (4) An inference call via A's project-attached token records
    // usage_events.project_id (asserted below, via the group-by-project read) --
    const infer = await request.post(`${GW}/v1/chat/completions`, {
      headers: { Authorization: `Bearer ${secret}`, "Content-Type": "application/json" },
      data: { model: MODEL, messages: [{ role: "user", content: "project attribution check" }], stream: false }
    });
    expect(infer.status(), `inference via the project-attached token must serve (200): ${await infer.text()}`).toBe(200);

    // --- (5) Activity group-by "project" shows a row for the project --------
    // Driven as A (admin scope, scope=all so it is unambiguous regardless of
    // which user's rows the admin branch would otherwise pin to).
    const groupsRes = await adminPage.request.get("/api/portal/usage/groups?group_by=project&range=all&scope=all");
    expect(groupsRes.ok()).toBe(true);
    const groupsBody = (await groupsRes.json()) as { data: UsageGroupRow[]; group_by: string };
    expect(groupsBody.group_by).toBe("project");
    const projectRow = groupsBody.data.find((r) => r.key === project.id);
    expect(projectRow, `expected a group-by-project row for ${project.id} in ${JSON.stringify(groupsBody.data)}`).toBeTruthy();
    expect(projectRow!.count).toBeGreaterThanOrEqual(1);
    // key_label resolves the project's CURRENT display name (portal.Service.usageGroupLabel).
    expect(projectRow!.key_label).toBe(PROJECT_NAME);

    // --- (5b) The project tokens endpoint ({tokens,total} shape) surfaces the
    // SAME inference usage: attributed to A's token (the one that actually ran
    // it) on its own row, AND folded into the project's TRUE total ------------
    const tokensRes = await adminPage.request.get(`/api/portal/projects/${project.id}/tokens`);
    expect(tokensRes.ok(), `owner listing project tokens must succeed: ${await tokensRes.text()}`).toBe(true);
    const tokensBody = (await tokensRes.json()) as ProjectTokensView;
    const aTokenRow = tokensBody.tokens.find((tok) => tok.name === "A Project Token");
    expect(aTokenRow, `expected A's project token in the tokens list: ${JSON.stringify(tokensBody.tokens)}`).toBeTruthy();
    expect(aTokenRow!.request_count, `A's token must show >=1 request: ${JSON.stringify(aTokenRow)}`).toBeGreaterThanOrEqual(1);
    expect(aTokenRow!.total_tokens, `A's token must show >0 total_tokens: ${JSON.stringify(aTokenRow)}`).toBeGreaterThan(0);
    expect(
      tokensBody.total.request_count,
      `project total must show >=1 request: ${JSON.stringify(tokensBody.total)}`
    ).toBeGreaterThanOrEqual(1);
    expect(tokensBody.total.total_tokens, `project total must show >0 total_tokens: ${JSON.stringify(tokensBody.total)}`).toBeGreaterThan(
      0
    );
    // The project total is AT LEAST the sum of the visible rows' totals -- it
    // can exceed it (a detached/deleted token's historical usage still
    // counts toward the total but not toward any visible row), never less.
    const visibleTotalTokens = tokensBody.tokens.reduce((sum, tok) => sum + tok.total_tokens, 0);
    expect(
      tokensBody.total.total_tokens,
      `project total (${tokensBody.total.total_tokens}) must be >= the sum of visible rows (${visibleTotalTokens})`
    ).toBeGreaterThanOrEqual(visibleTotalTokens);

    // --- (6) Cross-member view (design spec §8, the feature's one behavior
    // change on existing surface): B (a member, via B's own session) sees the
    // project's usage aggregated across ALL its members -- even though the
    // only request so far ran under A's token, not B's -- because
    // Service.applyUsageScope widens a project-scoped, non-admin query from
    // "my own rows" to "every row of every project I'm a member of". D (never
    // added, direct OR via a group) gets a query scoped to ZERO project ids ->
    // zero rows, never falling back to "show everything" ----------------------
    const bGroupsRes = await bPage.request.get("/api/portal/usage/groups?group_by=project&range=all");
    expect(bGroupsRes.ok()).toBe(true);
    const bGroupsBody = (await bGroupsRes.json()) as { data: UsageGroupRow[] };
    const bProjectRow = bGroupsBody.data.find((r) => r.key === project.id);
    expect(bProjectRow, `member B must see the project's aggregate: ${JSON.stringify(bGroupsBody.data)}`).toBeTruthy();
    expect(bProjectRow!.count).toBeGreaterThanOrEqual(1);

    const dGroupsRes = await dPage.request.get("/api/portal/usage/groups?group_by=project&range=all");
    expect(dGroupsRes.ok()).toBe(true);
    const dGroupsBody = (await dGroupsRes.json()) as { data: UsageGroupRow[] };
    expect(dGroupsBody.data.length, `non-member D must see NO project rows: ${JSON.stringify(dGroupsBody.data)}`).toBe(0);
  });

  // --- Coupled projects (spec 2026-08-09) ------------------------------------
  // A project COUPLED to a user-tier group: the group's OWNER is the
  // project's derived owner (no direct owner_user_id row), a MEMBER who
  // joined only via the group can still attribute a token + be attributed
  // usage, direct membership management is locked (409 project.coupled --
  // the UI itself hides those forms for a coupled project, so this is
  // asserted via the raw API, the only way to even attempt it), and deleting
  // the coupled group decouples the project back to a normal, ownerless one.
  test("coupled: create-group, derived owner, locked management, group-delete", async ({
    page: adminPage,
    request,
    browser
  }) => {
    const SG_COUPLED = "E2E Coupled System Group";
    // A second, otherwise-unused system group -- purely so the ADMIN-GROUP
    // create form's parent-picker combobox (t.groupsParentLabel) actually
    // RENDERS when this test runs on its own. GroupsView shows that picker
    // only when the system_admin manages more than one system group (with
    // exactly one it auto-resolves silently, no control at all -- see
    // createAdminGroupWithParent's doc comment); without a second group here,
    // THIS test run in isolation (no earlier test to have already created a
    // second system group) would see the parent auto-resolve instead of the
    // explicit-pick path -- a hidden, opaque ordering dependency on the
    // sibling test's fixtures.
    const SG_COUPLED_DECOY = "E2E Coupled Decoy Group";
    // A shared "landing" admin group under SG_COUPLED, used ONLY to get BOTH
    // OWNER and MEMBER a SG_COUPLED membership via the now-mandatory invite
    // admin-group picker (spec 2026-08-09-group-visibility-admin-group-
    // invite-design.md enrolls an invitee in BOTH the chosen admin group AND
    // its parent system group in one shot). Nobody manages or owns it beyond
    // the inviting system_admin; OWNER leaves it right after redemption (see
    // below) so their OWN admin-group membership count returns to exactly
    // zero before they create AG_COUPLED.
    const AG_COUPLED_LANDING = "E2E Coupled Landing Admin Group";
    const AG_COUPLED = "E2E Coupled Admin Group";
    const COUPLED_GROUP_NAME = "E2E Coupled User Group";
    const PROJECT_COUPLED_NAME = "E2E Coupled Project";
    const PROJECT_COUPLED_DESC = "e2e coupled-project scenario";
    const NAME_OWNER = "E2E Coupled Owner";
    const NAME_MEMBER = "E2E Coupled Member";

    await login(adminPage, PROJECTS_ADMIN_EMAIL, PROJECTS_ADMIN_PASSWORD);
    await enterSystemAdminMode(adminPage, PROJECTS_ADMIN_NAME, PROJECTS_ADMIN_PASSWORD);

    // --- Setup: two system groups (see SG_COUPLED_DECOY's doc comment) + the
    // shared landing admin group; an "admin"-role OWNER-to-be + a
    // "user"-role MEMBER-to-be, both landed in SG_COUPLED (via the landing
    // group) at invite time ----------------------------------------------------
    await adminPage.getByRole("link", { name: t.groups, exact: true }).click();
    await createSystemGroup(adminPage, SG_COUPLED);
    await createSystemGroup(adminPage, SG_COUPLED_DECOY);
    const landing = await createAdminGroupWithParent(adminPage, AG_COUPLED_LANDING, SG_COUPLED);

    const inviteOwner = await inviteProjectsAdmin(adminPage, NAME_OWNER, AG_COUPLED_LANDING);
    const inviteMember = await inviteProjectsUser(adminPage, NAME_MEMBER, AG_COUPLED_LANDING);

    const ownerPage = await redeemInvite(browser, inviteOwner.inviteUrl);
    const memberPage = await redeemInvite(browser, inviteMember.inviteUrl);

    // Own user ids (needed for the raw group/project API calls below) via the
    // portal's own-profile endpoint -- neither redeemInvite nor the invite
    // dialog surfaces a principal's id.
    const ownerMe = (await (await ownerPage.request.get("/api/portal/me")).json()) as { id: string };
    const memberMe = (await (await memberPage.request.get("/api/portal/me")).json()) as { id: string };
    const ownerId = ownerMe.id;
    const memberId = memberMe.id;

    // --- OWNER leaves the shared landing admin group. Self-removal is
    // always allowed (RemoveGroupMember has no manage-gate when userID ==
    // principal.UserID) and has no effect on OWNER's SG_COUPLED membership --
    // removeMemberCascade only cascades DOWNWARD to a group's own children,
    // never upward to its parent. Without this, OWNER would end up a member
    // of BOTH AG_COUPLED_LANDING and the AG_COUPLED they create next, making
    // the coupled-project create form's "exactly one admin group" auto-
    // parent resolve ambiguous below. ------------------------------------------
    const leaveRes = await ownerPage.request.delete(`/api/portal/groups/${landing.id}/members/${ownerId}`, {
      headers: { "X-OP-CSRF": "1" }
    });
    expect(leaveRes.ok(), `owner leaving the landing admin group must succeed: ${await leaveRes.text()}`).toBe(true);

    // --- OWNER climbs to admin-tier ownership (auto-parent: exactly one
    // system-group membership) -- this is what makes OWNER eligible for
    // create_coupled_group below (createUserGroup's "member of exactly one
    // admin group" rule; creating AG also enrolls OWNER as AG's member) -----
    await gotoGroups(ownerPage);
    const ag = await createAdminGroup(ownerPage, AG_COUPLED);

    // MEMBER must be a member of the PARENT admin group BEFORE they are
    // eligible for an invite into any user-tier group hanging under it
    // (containment, spec §5.2) -- admin tier is a direct add, no accept step.
    const addToAgRes = await ownerPage.request.post(`/api/portal/groups/${ag.id}/members`, {
      headers: { "X-OP-CSRF": "1" },
      data: { user_ids: [memberId] }
    });
    expect(addToAgRes.ok(), `owner adding member to the admin group must succeed: ${await addToAgRes.text()}`).toBe(true);

    // --- (1)+(2) OWNER creates a COUPLED project via the "create a group"
    // mode; assert the derived coupled_group_id + owner_user_id -------------
    await ownerPage.getByRole("link", { name: t.projects }).click();
    const project = await createCoupledProjectViaUI(ownerPage, PROJECT_COUPLED_NAME, PROJECT_COUPLED_DESC, COUPLED_GROUP_NAME);
    expect(project.coupled_group_id, `expected a non-empty coupled_group_id: ${JSON.stringify(project)}`).toBeTruthy();
    expect(project.owner_user_id).toBe(ownerId);
    expect(project.my_role).toBe("owner");
    const coupledGroupId = project.coupled_group_id!;

    // --- OWNER invites MEMBER into the coupled group (user tier: invite then
    // accept, spec §9) — MEMBER is now eligible only because they already
    // joined AG above --------------------------------------------------------
    const inviteToUgRes = await ownerPage.request.post(`/api/portal/groups/${coupledGroupId}/members`, {
      headers: { "X-OP-CSRF": "1" },
      data: { user_ids: [memberId] }
    });
    expect(inviteToUgRes.ok(), `owner inviting member into the coupled group must succeed: ${await inviteToUgRes.text()}`).toBe(
      true
    );
    const acceptRes = await memberPage.request.post(`/api/portal/groups/${coupledGroupId}/accept`, {
      headers: { "X-OP-CSRF": "1" }
    });
    expect(acceptRes.ok(), `member accepting the coupled-group invite must succeed: ${await acceptRes.text()}`).toBe(true);

    // --- (3) MEMBER — a member of the project ONLY via the group, never a
    // direct project_members row — attaches ONE OF THEIR OWN tokens to the
    // coupled project (allowed: group membership makes them a project
    // member, spec §4 rule 3), then infers with it --------------------------
    const secret = await createProjectToken(memberPage, "Member Coupled Token", PROJECT_COUPLED_NAME);
    const infer = await request.post(`${GW}/v1/chat/completions`, {
      headers: { Authorization: `Bearer ${secret}`, "Content-Type": "application/json" },
      data: { model: MODEL, messages: [{ role: "user", content: "coupled project attribution check" }], stream: false }
    });
    expect(infer.status(), `inference via the coupled-project token must serve (200): ${await infer.text()}`).toBe(200);

    const groupsRes = await adminPage.request.get("/api/portal/usage/groups?group_by=project&range=all&scope=all");
    expect(groupsRes.ok()).toBe(true);
    const groupsBody = (await groupsRes.json()) as { data: UsageGroupRow[]; group_by: string };
    const projectRow = groupsBody.data.find((r) => r.key === project.id);
    expect(projectRow, `expected a group-by-project row for the coupled project ${project.id}: ${JSON.stringify(groupsBody.data)}`).toBeTruthy();
    expect(projectRow!.count).toBeGreaterThanOrEqual(1);
    expect(projectRow!.key_label).toBe(PROJECT_COUPLED_NAME);

    // --- (4) Management is locked on a coupled project: the UI itself hides
    // the add-member/-group forms for a coupled project (ProjectsView's
    // isCoupled gate), so the ONLY way to even attempt this is the raw API —
    // which the backend rejects with 409 project.coupled regardless --------
    const lockedRes = await ownerPage.request.post(`/api/portal/projects/${project.id}/members`, {
      headers: { "X-OP-CSRF": "1" },
      data: { user_ids: [memberId] }
    });
    expect(lockedRes.status(), `expected 409, got ${lockedRes.status()}: ${await lockedRes.text()}`).toBe(409);
    const lockedBody = await lockedRes.json();
    expect(lockedBody.error?.code).toBe("project.coupled");

    // --- (5) Deleting the coupled group decouples the project: it becomes a
    // normal, ownerless project (FK on delete set null on
    // projects.coupled_group_id, and project_groups cascade-deletes the
    // linkage that made MEMBER a member) -------------------------------------
    const deleteRes = await ownerPage.request.delete(`/api/portal/groups/${coupledGroupId}`, {
      headers: { "X-OP-CSRF": "1" }
    });
    expect(deleteRes.ok(), `owner deleting the coupled group must succeed: ${await deleteRes.text()}`).toBe(true);

    // Admin scope lists EVERY project (portal.Service.ListProjects), so the
    // now-ownerless project is still findable there even though no principal
    // owns/is-a-member-of it anymore.
    const afterRes = await adminPage.request.get("/api/portal/projects");
    expect(afterRes.ok()).toBe(true);
    const afterBody = (await afterRes.json()) as { data: Project[] };
    const afterProject = afterBody.data.find((p) => p.id === project.id);
    expect(afterProject, `expected the decoupled project to still be listed for admin scope: ${JSON.stringify(afterBody.data)}`).toBeTruthy();
    expect(afterProject!.coupled_group_id ?? "").toBe("");

    // MEMBER's own (non-admin) landscape no longer includes it -- their only
    // path in was the now-deleted group's membership.
    const memberProjectsRes = await memberPage.request.get("/api/portal/projects");
    expect(memberProjectsRes.ok()).toBe(true);
    const memberProjectsBody = (await memberProjectsRes.json()) as { data: Project[] };
    expect(
      memberProjectsBody.data.find((p) => p.id === project.id),
      `member must no longer see the decoupled project: ${JSON.stringify(memberProjectsBody.data)}`
    ).toBeUndefined();
  });

  // --- Project-assigned tokens (list + detach) -------------------------------
  // A direct member attaches their own token to a project; the owner lists
  // it (no secret leaks), a non-manager gets a no-leak 404, the owner
  // detaches it (clearing attribution but NOT deleting the token), and a
  // detach attempt against a token attached to a DIFFERENT project is
  // rejected without touching that other attachment.
  test("assigned tokens: list (owner/admin, no-leak) + detach (clears attribution, keeps token) + cross-project guard", async ({
    page: adminPage,
    browser
  }) => {
    const SG_TOK = "E2E Token System Group";
    // A second, otherwise-unused system group -- purely so the admin-group
    // CREATE form's parent-picker combobox actually renders when this test
    // runs on its own (see SG_MAIN/SG_DECOY's doc comment above for why the
    // picker needs >= 2 system groups; nobody is ever invited into this
    // one). Neither B nor D's group membership matters to this test's
    // assertions (unlike the sibling non-coupled test, no group is ever
    // assigned to either project here), so both simply land in the SAME
    // admin group AG_TOK.
    const SG_TOK_DECOY = "E2E Token Decoy Group";
    const AG_TOK = "E2E Token Admin Group";
    const PROJECT_TOK_ONE = "E2E Token Project One";
    const PROJECT_TOK_TWO = "E2E Token Project Two";
    const NAME_TOK_B = "E2E Token User B";
    const NAME_TOK_D = "E2E Token User D";

    await login(adminPage, PROJECTS_ADMIN_EMAIL, PROJECTS_ADMIN_PASSWORD);
    await enterSystemAdminMode(adminPage, PROJECTS_ADMIN_NAME, PROJECTS_ADMIN_PASSWORD);

    // --- Setup: two system groups + an admin group under SG_TOK, then B
    // (will become a direct project-one member) and D (a stranger -- never
    // added to either project, used to prove the no-leak 404) -----------------
    await adminPage.getByRole("link", { name: t.groups, exact: true }).click();
    await createSystemGroup(adminPage, SG_TOK);
    await createSystemGroup(adminPage, SG_TOK_DECOY);
    await createAdminGroupWithParent(adminPage, AG_TOK, SG_TOK);

    const inviteB = await inviteProjectsUser(adminPage, NAME_TOK_B, AG_TOK);
    const inviteD = await inviteProjectsUser(adminPage, NAME_TOK_D, AG_TOK);
    const bPage = await redeemInvite(browser, inviteB.inviteUrl);
    const dPage = await redeemInvite(browser, inviteD.inviteUrl);
    const bMe = (await (await bPage.request.get("/api/portal/me")).json()) as { id: string };

    // --- (1) A (the bootstrap admin) creates TWO projects, both owned by A --
    await adminPage.getByRole("link", { name: t.projects }).click();
    const projectOne = await createProjectViaUI(adminPage, PROJECT_TOK_ONE, "e2e token attribution + detach");
    await adminPage.getByRole("link", { name: t.projects }).click();
    const projectTwo = await createProjectViaUI(adminPage, PROJECT_TOK_TWO, "e2e cross-project token guard target");

    // --- (2) B is added as a DIRECT member of project one, then attaches
    // ONE OF THEIR OWN tokens to it (reuse the existing attach flow) --------
    await openProjectMembers(adminPage, PROJECT_TOK_ONE);
    await addProjectMemberUser(adminPage, NAME_TOK_B);
    await expect(adminPage.getByText(NAME_TOK_B)).toBeVisible();

    const bSecret = await createProjectToken(bPage, "B's Project-One Token", PROJECT_TOK_ONE);
    expect(bSecret.length).toBeGreaterThan(0);

    // A attaches one of A's OWN tokens to project TWO -- this becomes the
    // cross-project guard's target below (a token that legitimately belongs
    // to a DIFFERENT project than the one we'll try to detach it from).
    const aSecretTwo = await createProjectToken(adminPage, "A's Project-Two Token", PROJECT_TOK_TWO);
    expect(aSecretTwo.length).toBeGreaterThan(0);

    // --- (3) Owner (A) lists project one's assigned tokens -- B's token
    // appears, attributed to B, with NO secret/hash in the response body ----
    const listOneRes = await adminPage.request.get(`/api/portal/projects/${projectOne.id}/tokens`);
    expect(listOneRes.ok(), `owner listing project-one tokens must succeed: ${await listOneRes.text()}`).toBe(true);
    const listOneRaw = await listOneRes.text();
    expect(listOneRaw).not.toMatch(/"secret"/);
    expect(listOneRaw).not.toMatch(/"secret_hash"/);
    const listOneBody = JSON.parse(listOneRaw) as ProjectTokensView;
    const bTokenRow = listOneBody.tokens.find((tok) => tok.name === "B's Project-One Token");
    expect(bTokenRow, `expected B's token in project one's list: ${listOneRaw}`).toBeTruthy();
    expect(bTokenRow!.owner_user_id).toBe(bMe.id);
    expect(bTokenRow!.owner_name).toBe(NAME_TOK_B);
    expect(bTokenRow!.status.length).toBeGreaterThan(0);

    // --- (4) A non-manager (D -- neither owner, nor admin, nor a member of
    // project one) gets a no-leak 404, not a 403 or an empty list -----------
    const strangerRes = await dPage.request.get(`/api/portal/projects/${projectOne.id}/tokens`);
    expect(strangerRes.status(), `expected 404, got ${strangerRes.status()}: ${await strangerRes.text()}`).toBe(404);
    const strangerBody = await strangerRes.json();
    expect(strangerBody.error?.code).toBe("project.not_found");

    // --- (5) Cross-project guard: A (owner of BOTH projects) cannot detach
    // project-two's token via PROJECT ONE's endpoint -- the guard checks the
    // token's OWN attribution, not merely the caller's ownership reach ------
    const listTwoRes = await adminPage.request.get(`/api/portal/projects/${projectTwo.id}/tokens`);
    expect(listTwoRes.ok()).toBe(true);
    const listTwoBody = (await listTwoRes.json()) as ProjectTokensView;
    const aTokenTwo = listTwoBody.tokens.find((tok) => tok.name === "A's Project-Two Token");
    expect(aTokenTwo, `expected A's token in project two's list: ${JSON.stringify(listTwoBody)}`).toBeTruthy();

    const crossRes = await adminPage.request.delete(`/api/portal/projects/${projectOne.id}/tokens/${aTokenTwo!.id}`, {
      headers: { "X-OP-CSRF": "1" }
    });
    expect(crossRes.status(), `expected 404, got ${crossRes.status()}: ${await crossRes.text()}`).toBe(404);
    const crossBody = await crossRes.json();
    expect(crossBody.error?.code).toBe("token.not_found");

    // Project two's attachment survives the failed cross-project attempt.
    const listTwoAfterRes = await adminPage.request.get(`/api/portal/projects/${projectTwo.id}/tokens`);
    const listTwoAfterBody = (await listTwoAfterRes.json()) as ProjectTokensView;
    expect(
      listTwoAfterBody.tokens.find((tok) => tok.id === aTokenTwo!.id),
      `project two must still list A's token after the rejected cross-project detach: ${JSON.stringify(listTwoAfterBody)}`
    ).toBeTruthy();

    // --- (6) Owner detaches B's token from project one ----------------------
    const detachRes = await adminPage.request.delete(`/api/portal/projects/${projectOne.id}/tokens/${bTokenRow!.id}`, {
      headers: { "X-OP-CSRF": "1" }
    });
    expect(detachRes.ok(), `owner detaching B's token must succeed: ${await detachRes.text()}`).toBe(true);
    const detachBody = (await detachRes.json()) as { ok: boolean };
    expect(detachBody.ok).toBe(true);

    // The token is gone from project one's list ...
    const listOneAfterRes = await adminPage.request.get(`/api/portal/projects/${projectOne.id}/tokens`);
    const listOneAfterBody = (await listOneAfterRes.json()) as ProjectTokensView;
    expect(
      listOneAfterBody.tokens.find((tok) => tok.id === bTokenRow!.id),
      `token must be gone from project one's list after detach: ${JSON.stringify(listOneAfterBody)}`
    ).toBeUndefined();

    // ... but B still OWNS the token, with its project attribution cleared --
    // detach removed the attribution, it did NOT delete the token itself.
    const bOwnTokensRes = await bPage.request.get("/api/portal/tokens");
    expect(bOwnTokensRes.ok()).toBe(true);
    const bOwnTokensBody = (await bOwnTokensRes.json()) as { data: OwnToken[] };
    const bOwnToken = bOwnTokensBody.data.find((tok) => tok.id === bTokenRow!.id);
    expect(bOwnToken, `B must still own the token after detach: ${JSON.stringify(bOwnTokensBody)}`).toBeTruthy();
    expect(bOwnToken!.project_id ?? "").toBe("");
  });
});
