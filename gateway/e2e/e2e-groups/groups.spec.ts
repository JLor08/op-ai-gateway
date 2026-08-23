// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { expect, test, type Browser, type Page } from "@playwright/test";
import { messages } from "../../frontend/src/i18n";
import { login, setPassword, uniqueEmail } from "../e2e/helpers";
import { GROUPS_ADMIN_EMAIL, GROUPS_ADMIN_NAME, GROUPS_ADMIN_PASSWORD } from "../playwright.groups.config";

const t = messages.de;

// >= 10 chars per the password policy in internal/auth. Test-only.
const USER_PASSWORD = "E2E-Groups-Pass-1";

// Distinct, non-prefix-colliding names (spec §4.1's per-scope uniqueness is
// case-insensitive-exact -- and, more importantly for this e2e, several
// helpers below locate a table row via `getByRole("row", { name: new
// RegExp(name) })`, a SUBSTRING match against the row's whole accessible
// text -- so no group/person name here may be a literal substring of
// another, or the regex would match more than one row and Playwright's
// strict-mode locator would throw. "Alpha"/"Beta"/"Gamma" (rather than
// "One"/"One B"/"Two", where "One" is a substring of "One B") are chosen
// for exactly this reason.
const SG_ONE = "E2E System Group One";
const SG_TWO = "E2E System Group Two";
const AG_MAIN = "E2E Admin Group Alpha"; // parent SG_ONE; A + U1 land here
const AG_SECONDARY = "E2E Admin Group Beta"; // parent SG_ONE too; U2 lands here
const AG_OTHER = "E2E Admin Group Gamma"; // parent SG_TWO; B lands here
// A system-admin-created "for another admin" group (owner = A, spec:
// 2026-08-10-system-admin-create-admin-group-for-owner) -- a distinct name so
// it can never collide, as a substring, with AG_MAIN/AG_SECONDARY/AG_OTHER or
// any person name below (see the naming-collision note further up).
const AG_OWNED = "E2E Admin Group Delta";
const UG_NAME = "E2E User Group";
// The name AG_MAIN is renamed to at the very end of the run (permission-flag
// scenario below) -- a wholly distinct name (no Greek-letter relation to
// AG_MAIN/AG_SECONDARY/AG_OTHER/AG_OWNED) so it can never collide, as a
// substring, with any name still in play once the rename actually lands.
const AG_MAIN_RENAMED = "E2E Admin Group Zulu";

const NAME_ADMIN_A = "E2E Admin A";
const NAME_ADMIN_B = "E2E Admin B";
const NAME_USER_ONE = "E2E User One";
const NAME_USER_TWO = "E2E User Two";
// Invited by A in the permission-flag scenario below, once A holds
// can_manage_users on AG_MAIN as a co-manager (not its owner).
const NAME_USER_THREE = "E2E User Three";

/**
 * Navigates to the Groups view and waits for the landscape fetch
 * (GET /api/portal/groups) to complete. GroupsView's landscape `useResource`
 * call opts OUT of loading-tracking (trackLoading:false — see GroupsView.tsx),
 * so there is no visible "loading" gate a Playwright action could wait on; the
 * admin/user-tier CREATE form's auto-parent-resolve logic reads whatever
 * landscape.system/.admin holds AT CLICK TIME. Without this wait, opening the
 * create-admin-group form before the fetch lands would see an EMPTY parent
 * option list and refuse to auto-resolve — a real, previously-unguarded race.
 */
async function gotoGroups(page: Page): Promise<void> {
  const [resp] = await Promise.all([
    page.waitForResponse((r) => new URL(r.url()).pathname === "/api/portal/groups" && r.request().method() === "GET"),
    page.getByRole("link", { name: t.groups, exact: true }).click()
  ]);
  expect(resp.ok()).toBe(true);
}

/**
 * Enters System-Admin mode (step-up) for the bootstrap system_admin, whose
 * fresh session is NOT elevated by default (spec:
 * 2026-08-10-system-admin-step-up-mode-design.md) -- the `system` scope is
 * attached to the session ONLY after this, so it is REQUIRED before any
 * system-scoped action this suite drives: creating System groups, seeing the
 * GroupsView System panel, and (new in this revision) the create-admin-
 * group-FOR-another-admin owner picker (`AdminOwnerCandidates` 403s a
 * non-elevated system_admin -- see internal/portal/service_user_groups.go).
 * Elevating ONCE right after login is sufficient for the rest of the run --
 * `GET /api/portal/me` overlays the session's real elevation state on every
 * fetch (incl. the `loadPortalData` refetch App.tsx fires on each top-level
 * navigation), so `systemAdminMode` stays true across navigation for the
 * whole TTL (900s default), not just until the first nav.
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

/**
 * Invites a user (via the Users view), picking `adminGroupName` in the
 * invite form's MANDATORY admin-group picker (spec: 2026-08-09-group-
 * visibility-admin-group-invite-design.md §Frontend — the
 * `userInviteAdminGroupLabel` combobox; replaces the old system-group
 * multi-select). The combobox only RENDERS when the actor manages MORE THAN
 * ONE admin group (exactly one auto-selects with no control at all; the
 * suite always creates >= 2 admin groups before its first invite, so the
 * combobox is always present here). On submit the new user is enrolled
 * DIRECTLY (state=member, no invite/accept step) in BOTH the chosen admin
 * group AND its parent system group in one shot
 * (portal.Service.AddUserToAdminGroup) — the admin/system tiers have no
 * invite/accept flow at all (only the user tier does, spec §9). Returns the
 * invite URL + generated email for redemption.
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

/**
 * Redeems a set-password invite in a FRESH, isolated browser context (its own
 * cookies/session — mirrors e2e-limits/limits.spec.ts's
 * inviteUserWithLimitsAndToken) and lands on the authenticated app. Returns
 * the live, logged-in Page for that principal.
 */
async function redeemInvite(browser: Browser, inviteUrl: string): Promise<Page> {
  const context = await browser.newContext({ baseURL: "http://127.0.0.1:4173" });
  const page = await context.newPage();
  await setPassword(page, inviteUrl, USER_PASSWORD);
  // account.Service.SetPassword redeems the token, sets the password, AND
  // creates a session -- the invited user is already logged in at this point
  // (see limits.spec.ts's inviteUserWithLimitsAndToken for the same note).
  await page.goto("/portal/");
  await expect(page.getByText(t.welcome)).toBeVisible();
  return page;
}

/** Creates a system-tier group (system_admin only; no parent). */
async function createSystemGroup(page: Page, name: string): Promise<void> {
  await page.getByRole("button", { name: t.groupsCreateSystemTitle }).click();
  await page.locator("#group-name").fill(name);
  await page.getByRole("button", { name: t.save }).click();
  await expect(page.getByRole("row", { name: new RegExp(name) })).toBeVisible();
}

/**
 * Creates an admin-tier group as `page`, picking `parentName` explicitly in
 * the create form's parent `SearchableSelect` (spec §7.2) — the picker only
 * renders when the actor is a member of MORE THAN ONE system group (the
 * bootstrap system_admin here has zero own memberships but sees the FULL
 * system-group list for the purpose of this picker, and "system_admin darf
 * aus allen wählen" — may choose ANY system group as parent, not just one it
 * belongs to).
 */
async function createAdminGroupWithParent(page: Page, name: string, parentName: string): Promise<void> {
  await page.getByRole("button", { name: t.groupsCreateAdminTitle }).click();
  await page.locator("#group-name").fill(name);
  await page.getByRole("combobox", { name: t.groupsParentLabel }).click();
  await page.getByRole("option", { name: parentName, exact: true }).click();
  await page.getByRole("button", { name: t.save }).click();
  await expect(page.getByRole("row", { name: new RegExp(name) })).toBeVisible();
}

/**
 * Creates an admin-tier group FOR ANOTHER admin as its owner (spec:
 * 2026-08-10-system-admin-create-admin-group-for-owner) -- system-admin-only,
 * REQUIRES elevation (the owner-picker's backing endpoint,
 * `GET /api/portal/admin-owner-candidates`, 403s a non-elevated system_admin
 * -- internal/portal/service_user_groups.go `AdminOwnerCandidates`). Opening
 * the create-admin form kicks off that fetch; waiting for its response
 * before interacting with the owner combobox avoids a race against the
 * still-empty candidate list. `ownerLabelSubstring` matches the picked
 * owner's DISPLAY NAME (the option's accessible name is
 * `${display_name} (${email})` -- see SearchableSelect's renderOption, a
 * single plain-text span, so a substring match against the name alone is
 * sufficient and unambiguous given this suite's non-colliding names). The
 * parent system group auto-resolves from the OWNER's own system-group
 * memberships (not the caller's) when they belong to exactly one -- true
 * here, since the owner is expected to already be enrolled via
 * `inviteWithAdminGroup` before this helper runs.
 */
async function createAdminGroupForOwner(page: Page, name: string, ownerLabelSubstring: string): Promise<void> {
  const [resp] = await Promise.all([
    page.waitForResponse((r) => new URL(r.url()).pathname === "/api/portal/admin-owner-candidates" && r.request().method() === "GET"),
    page.getByRole("button", { name: t.groupsCreateAdminTitle }).click()
  ]);
  expect(resp.ok()).toBe(true);
  await page.locator("#group-name").fill(name);
  await page.getByRole("combobox", { name: t.groupsOwnerLabel }).click();
  await page.getByRole("option", { name: new RegExp(ownerLabelSubstring) }).click();
  await page.getByRole("button", { name: t.save }).click();
  await expect(page.getByRole("row", { name: new RegExp(name) })).toBeVisible();
}

/**
 * Creates a user-tier group as `page` — the actor must be a member of
 * EXACTLY ONE admin group so the parent auto-resolves (spec §7.3).
 */
async function createUserGroup(page: Page, name: string): Promise<void> {
  await page.getByRole("button", { name: t.groupsCreateUserTitle }).click();
  await page.locator("#group-name").fill(name);
  await page.getByRole("button", { name: t.save }).click();
  await expect(page.getByRole("row", { name: new RegExp(name) })).toBeVisible();
}

/**
 * Opens the "Mitglieder verwalten" sub-view for the group row named
 * `groupName`. Each of the three group tables (System/Admin/User) is a
 * `<Panel>` -- an ARIA `region` labelled by its own section heading -- so an
 * optional `regionLabel` (e.g. t.groupsAdminTitle) scopes the row search to
 * ONE specific table. Scoping matters once a system_admin's page renders
 * ALL three tables together: a child group's row embeds its PARENT group's
 * name verbatim in its own "Parent" column (adminColumns/userColumns'
 * `parent` cell), so an unscoped page-wide substring search for an ADMIN
 * group's name can ALSO match a USER-tier row whose parent happens to be
 * that admin group -- a real strict-mode-violation this suite hit (opening
 * an admin group's members after a child user group already exists).
 * `regionLabel` omitted (the default) is safe on a plain "user"/"admin"
 * page that never renders more than one group table containing `groupName`.
 */
async function openMembers(page: Page, groupName: string, regionLabel?: string): Promise<void> {
  const scope = regionLabel ? page.getByRole("region", { name: regionLabel }) : page;
  const row = scope.getByRole("row", { name: new RegExp(groupName) });
  await row.getByRole("button", { name: t.groupsActionMembers }).click();
}

/**
 * Selects `memberName` in the currently-open members sub-view's Kandidaten
 * multi-select and submits — `submitLabel` differs by tier: "Hinzufügen"
 * (direct add, system/admin tier) vs "Einladen" (invite, user tier; spec §9).
 *
 * Uses a NON-exact (substring) name match: GroupsView's candidateOptions
 * render each option as the display name AND email in two adjacent <span>s
 * with only CSS margin between them (MultiSelectField.renderOption) — no
 * actual whitespace/text node — so the option's ACCESSIBLE NAME is the two
 * concatenated with no separator (e.g. "E2E User Onee2e-…@example.test").
 * An exact match against the display name alone therefore never matches.
 */
async function addCandidate(page: Page, memberName: string, submitLabel: string): Promise<void> {
  await page.getByRole("combobox", { name: t.groupsAddMembersLabel }).click();
  await page.getByRole("option", { name: memberName }).click();
  await page.getByRole("button", { name: submitLabel }).click();
}

test.describe("Benutzergruppen (user groups) — 3-tier hierarchy, invites, visibility, succession", () => {
  test("system+admin+user hierarchy, containment on invite, admin-visibility partitioning, owner succession on disable", async ({
    page: systemAdminPage,
    browser
  }) => {
    // --- Setup: two system groups, three admin groups spanning both --------
    await login(systemAdminPage, GROUPS_ADMIN_EMAIL, GROUPS_ADMIN_PASSWORD);
    // Restores this suite on the system-admin-step-up-mode base (spec:
    // 2026-08-10-system-admin-step-up-mode-design.md): a fresh system_admin
    // session carries NO `system` scope until elevated, so without this the
    // GroupsView System panel (and the create-System-group control below)
    // would not even render. Elevating ONCE here, right after login, is
    // sufficient for the whole run (fix round 1: `GET /api/portal/me` now
    // overlays the session's real elevation state on every navigation-
    // triggered refetch -- see internal/gateway/server.go handlePortalMe --
    // so `systemAdminMode` no longer reverts to false the moment
    // systemAdminPage navigates away from Groups; the earlier "re-elevate
    // immediately before every gated action" workaround is gone).
    await enterSystemAdminMode(systemAdminPage, GROUPS_ADMIN_NAME, GROUPS_ADMIN_PASSWORD);

    await gotoGroups(systemAdminPage);
    await createSystemGroup(systemAdminPage, SG_ONE);
    await createSystemGroup(systemAdminPage, SG_TWO);
    // AG_MAIN + AG_SECONDARY both hang under SG_ONE (so U2, added to
    // AG_SECONDARY, is already a SG_ONE member by the time containment for
    // AG_MAIN is exercised below); AG_OTHER hangs under the DIFFERENT
    // SG_TWO (the admin-visibility-partitioning check below relies on B
    // landing in a system group disjoint from A/U1/U2's).
    await createAdminGroupWithParent(systemAdminPage, AG_MAIN, SG_ONE);
    await createAdminGroupWithParent(systemAdminPage, AG_SECONDARY, SG_ONE);
    await createAdminGroupWithParent(systemAdminPage, AG_OTHER, SG_TWO);

    // --- Invite A + U1 into AG_MAIN, U2 into AG_SECONDARY, B into AG_OTHER --
    // Each invite enrolls the new user DIRECTLY (state=member) in BOTH the
    // chosen admin group AND its parent system group — no invite/accept step
    // at the admin/system tier (only user-tier groups have one, spec §9).
    const inviteA = await inviteWithAdminGroup(systemAdminPage, { role: "admin", adminGroupName: AG_MAIN, displayName: NAME_ADMIN_A });
    const inviteU1 = await inviteWithAdminGroup(systemAdminPage, { role: "user", adminGroupName: AG_MAIN, displayName: NAME_USER_ONE });
    const inviteU2 = await inviteWithAdminGroup(systemAdminPage, { role: "user", adminGroupName: AG_SECONDARY, displayName: NAME_USER_TWO });
    const inviteB = await inviteWithAdminGroup(systemAdminPage, { role: "admin", adminGroupName: AG_OTHER, displayName: NAME_ADMIN_B });

    const aPage = await redeemInvite(browser, inviteA.inviteUrl);
    const u1Page = await redeemInvite(browser, inviteU1.inviteUrl);
    const u2Page = await redeemInvite(browser, inviteU2.inviteUrl);
    const bPage = await redeemInvite(browser, inviteB.inviteUrl);

    // --- (0) system_admin creates an admin group FOR A, assigning A (not
    // itself) as owner — the create-admin-group-for-owner extension (spec:
    // 2026-08-10-system-admin-create-admin-group-for-owner). A is already
    // enrolled in exactly one system group (SG_ONE, via the AG_MAIN invite
    // above), so the parent auto-resolves from A's OWN system-group
    // memberships (not the elevated system_admin's — the system_admin here
    // has zero own memberships, so a self-relative resolve would fail). The
    // creating system_admin does NOT join as a member of the new group; A
    // alone is enrolled, as its sole owner (backend: portal.Service.
    // createAdminGroup's "forAnother" branch — see internal/portal/
    // service_user_groups.go). Requires elevation -- the owner-picker's
    // backing endpoint, GET /api/portal/admin-owner-candidates, 403s a
    // non-elevated system_admin -- but the Step-1 elevation above already
    // covers this: `systemAdminMode` survives the four `inviteWithAdminGroup`
    // navigations (Users view and back) via the `GET /api/portal/me`
    // elevation-overlay fix, so no re-elevation is needed here.
    await gotoGroups(systemAdminPage);
    await createAdminGroupForOwner(systemAdminPage, AG_OWNED, NAME_ADMIN_A);
    await expect(
      systemAdminPage.getByRole("region", { name: t.groupsAdminTitle }).getByRole("row", { name: new RegExp(AG_OWNED) })
    ).toBeVisible();

    // A's OWN session (not the creating system_admin, who never joined) sees
    // the new group with role Owner — A hasn't visited Groups yet, so this is
    // a genuinely fresh landscape fetch (no navigate-away-and-back needed).
    await gotoGroups(aPage);
    const aOwnedRow = aPage.getByRole("region", { name: t.groupsAdminTitle }).getByRole("row", { name: new RegExp(AG_OWNED) });
    await expect(aOwnedRow).toBeVisible();
    await expect(aOwnedRow.getByText(t.groupsRoleOwner)).toBeVisible();

    // systemAdminPage's `view` is now "groups" — navigate it away so the
    // LATER `gotoGroups(systemAdminPage)` calls below trigger a genuine
    // remount + fresh GET /api/portal/groups (clicking the SAME already-
    // active nav item is a no-op — GroupsView never unmounts, so its
    // mount-effect landscape fetch never re-fires, and `gotoGroups`'s
    // `waitForResponse` would then hang until the test timeout — see the
    // near-identical U2 comment further below for the same gotcha).
    await systemAdminPage.getByRole("link", { name: t.dashboard }).click();

    // --- (1) U1 creates UG under AG_MAIN (auto-parent, exactly one admin
    // group -- U1's only admin-tier membership is AG_MAIN, spec §7.3) -------
    await gotoGroups(u1Page);
    await createUserGroup(u1Page, UG_NAME);

    // --- (2) Containment gate: U2 (only a member of AG_SECONDARY, not
    // AG_MAIN) is NOT yet a candidate for UG; A (a fellow AG_MAIN member) is
    // (spec §5.2/§9 — a user-tier invitee must first be a member of UG's
    // PARENT admin group, AG_MAIN) --------------------------------------------
    await openMembers(u1Page, UG_NAME);
    const u1CandidatesCombo = u1Page.getByRole("combobox", { name: t.groupsAddMembersLabel });
    await u1CandidatesCombo.click();
    // Assert A's presence FIRST (with Playwright's normal retry/auto-wait) so
    // the popup has genuinely rendered its real, fully-loaded option list
    // before the absence check below is evaluated -- an absence assertion
    // checked against an as-yet-unrendered popup would pass vacuously and
    // prove nothing.
    // Non-exact (substring) matches -- see addCandidate's doc comment on why
    // an exact match against the display name alone never matches here.
    await expect(u1Page.getByRole("option", { name: NAME_ADMIN_A })).toBeVisible();
    await expect(u1Page.getByRole("option", { name: NAME_USER_TWO })).toHaveCount(0);
    await u1Page.keyboard.press("Escape");

    // --- (2) system_admin adds U2 directly to AG_MAIN — containment holds
    // because U2 is ALREADY a member of SG_ONE (AG_MAIN's parent) via their
    // AG_SECONDARY invite above, so U2 now satisfies AG_MAIN's own
    // containment (member of its parent system group) ------------------------
    await gotoGroups(systemAdminPage);
    await openMembers(systemAdminPage, AG_MAIN, t.groupsAdminTitle);
    await addCandidate(systemAdminPage, NAME_USER_TWO, t.groupsActionAdd);
    await expect(systemAdminPage.getByText(NAME_USER_TWO)).toBeVisible();

    // --- (2) U1 invites U2 to UG; assert the INVITED state before accept ----
    // Re-opens the members sub-view so the candidate list is fetched fresh
    // (U1's earlier session snapshot predates U2 joining AG_MAIN).
    await u1Page.getByRole("button", { name: t.back }).click();
    await openMembers(u1Page, UG_NAME);
    await addCandidate(u1Page, NAME_USER_TWO, t.groupsActionInvite);
    await expect(u1Page.getByText(t.groupsMemberStateInvited)).toBeVisible();

    // --- (2) U2 sees + accepts the invitation --------------------------------
    await gotoGroups(u2Page);
    await expect(u2Page.getByText(UG_NAME)).toBeVisible();
    await u2Page.getByRole("button", { name: t.groupsActionAccept }).click();
    // Accept flips U2 to a real member in U2's own "Meine Gruppen" list.
    const u2UgRow = u2Page.getByRole("row", { name: new RegExp(UG_NAME) });
    await expect(u2UgRow).toBeVisible();
    await expect(u2UgRow.getByText(t.groupsRoleMember)).toBeVisible();

    // --- (2) Back on U1's side: the roster now shows U2 as a plain member ---
    // (re-open fresh — the earlier snapshot still shows "invited").
    await u1Page.getByRole("button", { name: t.back }).click();
    await openMembers(u1Page, UG_NAME);
    await expect(u1Page.getByText(t.groupsMemberStateInvited)).not.toBeVisible();
    await expect(u1Page.getByText(NAME_USER_TWO)).toBeVisible();

    // --- (3) Admin-visibility partitioning ------------------------------------
    // B (owns/manages no admin group) must NOT see U1 in the admin Users
    // list. GET /api/admin/users is scoped to ManageableUserIDs (spec:
    // 2026-08-10-admin-group-permissions-phase-a-design.md, Task 3) --
    // "yourself plus the member roster of every admin group you OWN or
    // CO-MANAGE with can_manage_users=true" -- STRICTLY NARROWER than the
    // old shared-system-group rule this endpoint used before the per-
    // Admin-Group co-manager permissions model landed. B owns/co-manages no
    // admin group, so B's manageable set is just {B}.
    await bPage.getByRole("link", { name: t.users }).click();
    await expect(bPage.getByRole("row", { name: new RegExp(inviteU1.email) })).toHaveCount(0);
    // B still sees itself (ManageableUserIDs always includes the principal).
    await expect(bPage.getByRole("row", { name: new RegExp(inviteB.email) })).toBeVisible();

    // A system_admin sees EVERYONE regardless of group membership
    // (ManageableUserIDs bypasses entirely for the `system` scope).
    await systemAdminPage.getByRole("link", { name: t.users }).click();
    await expect(systemAdminPage.getByRole("row", { name: new RegExp(inviteU1.email) })).toBeVisible();
    await expect(systemAdminPage.getByRole("row", { name: new RegExp(inviteB.email) })).toBeVisible();

    // --- (3.5) systemAdminPage promotes A to co-manager of AG_MAIN -------------
    // Per-Admin-Group co-manager permissions (spec: 2026-08-10-admin-group-
    // permissions-phase-a-design.md) replaced the OLD "any admin sharing a
    // system group may manage its members" rule with the strictly-per-
    // Admin-Group ManageableUserIDs above: admin user-management now
    // requires OWNING or CO-MANAGING (with can_manage_users) a shared ADMIN
    // group. A is so far only a plain MEMBER of AG_MAIN, which is no longer
    // enough to manage U1 in step (4) below -- promoting A first (Befoerdern
    // defaults BOTH flags true, the spec's "existing manager" bootstrap
    // default reproduced byte-for-byte for a brand-new co-manager too) gives
    // A real can_manage_users over every AG_MAIN member, U1 included.
    await systemAdminPage.getByRole("link", { name: t.dashboard }).click();
    await gotoGroups(systemAdminPage);
    // AG_MAIN's row lives in the Admin-Gruppen table; scoping is REQUIRED
    // here (see openMembers' doc comment) -- UG_NAME (a User-tier group
    // created earlier under AG_MAIN) embeds "E2E Admin Group Alpha" verbatim
    // in its own Parent column, so an unscoped search would strict-mode-
    // violate.
    await openMembers(systemAdminPage, AG_MAIN, t.groupsAdminTitle);
    const aMemberRow = systemAdminPage.getByRole("row", { name: new RegExp(NAME_ADMIN_A) });
    await expect(aMemberRow).toBeVisible();
    await aMemberRow.getByRole("button", { name: t.groupsActionPromote }).click();
    await expect(aMemberRow.getByText(t.groupsRoleManager)).toBeVisible();
    // Freshly promoted -- both flags default true (Decision 7's "existing
    // manager" bootstrap default; PromoteManager's own default mirrors it
    // for a BRAND NEW co-manager too, so the pre-feature "a co-manager can
    // do everything" behavior is exactly reproduced until the owner narrows
    // it below in step (5)).
    const usersCheckbox = systemAdminPage.getByRole("checkbox", { name: `${t.groupsPermUsers} – ${NAME_ADMIN_A}` });
    const groupCheckbox = systemAdminPage.getByRole("checkbox", { name: `${t.groupsPermGroup} – ${NAME_ADMIN_A}` });
    await expect(usersCheckbox).toBeChecked();
    await expect(groupCheckbox).toBeChecked();

    // --- (4) Owner succession on disable --------------------------------------
    // A disables U1 -- authorized because A now holds can_manage_users on
    // AG_MAIN (the freshly-promoted co-manager role above), which covers
    // every AG_MAIN member, U1 included. This triggers ReassignGroupsOwnedBy
    // for every group U1 owns (just UG). With no managers and exactly one
    // other member (U2), succession picks U2 as UG's new owner (spec §8.1's
    // user-tier chain: manager -> member -> delete).
    await aPage.getByRole("link", { name: t.users }).click();
    const u1Row = aPage.getByRole("row", { name: new RegExp(inviteU1.email) });
    await expect(u1Row).toBeVisible();
    await u1Row.getByRole("button", { name: t.userActionDisable }).click();
    await expect(u1Row.getByText(t.statusDisabled)).toBeVisible();

    // U2 re-fetches its group landscape (its last snapshot predates the
    // disable). Clicking the SAME nav item twice is a no-op (view state
    // unchanged -> GroupsView never unmounts -> its mount-effect landscape
    // fetch never re-fires) — navigate away and back to force a genuine
    // remount + fresh GET /api/portal/groups.
    await u2Page.getByRole("link", { name: t.dashboard }).click();
    await gotoGroups(u2Page);
    const u2UgRowAfter = u2Page.getByRole("row", { name: new RegExp(UG_NAME) });
    await expect(u2UgRowAfter).toBeVisible();
    await expect(u2UgRowAfter.getByText(t.groupsRoleOwner)).toBeVisible();

    // --- (5) Per-Admin-Group co-manager permission flags, continued (spec:
    // 2026-08-10-admin-group-permissions-phase-a-design.md) -- can_manage_users
    // and can_manage_group are now INDEPENDENT per co-manager, replacing the
    // old binary "a co-manager can do everything" role. A was already
    // promoted to AG_MAIN co-manager in step (3.5) above (defaults BOTH
    // flags true) so U1's disable would be authorized; systemAdminPage's
    // members sub-view for AG_MAIN is STILL open (nothing since step (3.5)
    // navigated it away) -- narrow A to Benutzer-Verwaltung ONLY via the same
    // per-permission checkboxes, still in scope from step (3.5). ---

    // The owner (systemAdminPage) narrows A to Benutzer-Verwaltung ONLY --
    // unchecking Gruppen-Aenderung drives PATCH .../managers/{userId} with
    // can_manage_users staying true (carried over from the roster's current
    // value) and can_manage_group flipping false.
    await groupCheckbox.click();
    await expect(groupCheckbox).not.toBeChecked();
    await expect(usersCheckbox).toBeChecked();

    // --- (5a) A CAN invite a new user into AG_MAIN (can_manage_users=true) --
    // UsersView's invite-admin-group picker filters on can_manage_users (not
    // the structure facet can_manage_group), so AG_MAIN still surfaces as an
    // eligible target for A even though A's can_manage_group on it is now
    // false. A also owns AG_OWNED outright (>= 2 manageable admin groups), so
    // the picker renders as a real dropdown requiring an explicit choice --
    // inviteWithAdminGroup's own doc comment covers why that combobox only
    // renders past one manageable group.
    const inviteU3 = await inviteWithAdminGroup(aPage, { role: "user", adminGroupName: AG_MAIN, displayName: NAME_USER_THREE });

    // Resolve AG_MAIN's id (needed for the raw rename attempts below) and, in
    // the same pass, confirm AddUserToAdminGroup actually enrolled U3 as a
    // real AG_MAIN member -- not merely a UI-side success toast.
    type GroupLandscapeSnapshot = { admin: { id: string; name: string }[] };
    const landscapeAfterInvite = (await (await systemAdminPage.request.get("/api/portal/groups")).json()) as GroupLandscapeSnapshot;
    const agMain = landscapeAfterInvite.admin.find((g) => g.name === AG_MAIN);
    expect(agMain, `expected ${AG_MAIN} in the system_admin's landscape: ${JSON.stringify(landscapeAfterInvite)}`).toBeTruthy();
    const agMainId = agMain!.id;
    type RosterSnapshot = { data: { email: string }[] };
    const rosterAfterInvite = (await (await systemAdminPage.request.get(`/api/portal/groups/${agMainId}/members`)).json()) as RosterSnapshot;
    expect(
      rosterAfterInvite.data.some((m) => m.email === inviteU3.email),
      `expected ${inviteU3.email} in AG_MAIN's roster after the invite: ${JSON.stringify(rosterAfterInvite)}`
    ).toBe(true);

    // --- (5b) A CANNOT rename AG_MAIN (can_manage_group=false) -----------------
    // The UI itself already hides the "Umbenennen" row action for A (GroupsView's
    // list-view rowActions gates rename on group.can_manage, which groupDTO
    // computes from the SAME stored can_manage_group flag this scenario just
    // cleared) -- so a raw page.request PATCH (sharing aPage's own session
    // cookies) is the only way to actually exercise -- and mutation-prove --
    // authorizeGroupManage's needGroup gate on RenameGroup's OWN call site,
    // independent of whatever the frontend does or doesn't render.
    const rejectedRename = await aPage.request.patch(`/api/portal/groups/${agMainId}`, {
      // Cookie-authenticated state-changing requests require the CSRF header
      // (internal/gateway/auth.go's csrfOK) -- a raw page.request bypasses
      // api.ts (which sets this on every mutating call) entirely.
      headers: { "X-OP-CSRF": "1" },
      data: { name: AG_MAIN_RENAMED }
    });
    expect(rejectedRename.status(), `expected 404, got ${rejectedRename.status()}: ${await rejectedRename.text()}`).toBe(404);
    const rejectedRenameBody = await rejectedRename.json();
    // No-leak: the SAME code a true non-member would get, never a
    // distinguishable "forbidden" that would confirm the group's existence.
    expect(rejectedRenameBody.error?.code).toBe("group.not_found");
    // The name is genuinely unchanged (the rejected attempt had no effect).
    const landscapeAfterRejectedRename = (await (await systemAdminPage.request.get("/api/portal/groups")).json()) as GroupLandscapeSnapshot;
    expect(landscapeAfterRejectedRename.admin.some((g) => g.id === agMainId && g.name === AG_MAIN)).toBe(true);

    // --- (5c) Owner widens A back to Gruppen-Aenderung too, via the SAME
    // checkbox -- the members sub-view for AG_MAIN is still open on
    // systemAdminPage (nothing above navigated it away) ------------------------
    await groupCheckbox.click();
    await expect(groupCheckbox).toBeChecked();

    // --- (5d) A can NOW rename AG_MAIN -----------------------------------------
    const acceptedRename = await aPage.request.patch(`/api/portal/groups/${agMainId}`, {
      headers: { "X-OP-CSRF": "1" },
      data: { name: AG_MAIN_RENAMED }
    });
    expect(acceptedRename.ok(), `expected success, got ${acceptedRename.status()}: ${await acceptedRename.text()}`).toBe(true);
    const acceptedRenameBody = (await acceptedRename.json()) as { name: string };
    expect(acceptedRenameBody.name).toBe(AG_MAIN_RENAMED);
    const landscapeAfterAcceptedRename = (await (await systemAdminPage.request.get("/api/portal/groups")).json()) as GroupLandscapeSnapshot;
    expect(landscapeAfterAcceptedRename.admin.some((g) => g.id === agMainId && g.name === AG_MAIN_RENAMED)).toBe(true);
  });
});
