// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { expect, test } from "@playwright/test";
import { messages } from "../../frontend/src/i18n";
import { login } from "../e2e/helpers";
import { SAMODE_ADMIN_EMAIL, SAMODE_ADMIN_NAME, SAMODE_ADMIN_PASSWORD } from "../playwright.system-admin-mode.config";

const t = messages.de;

test.describe("System-Admin step-up mode", () => {
  test("system nav hidden until elevated; password step-up; leave drops it", async ({ page }) => {
    await login(page, SAMODE_ADMIN_EMAIL, SAMODE_ADMIN_PASSWORD);

    // As of 2026-08-12 (commit `9267344`) the step-up control moved OFF the
    // sidebar and INTO the user dropdown (SystemAdminModeControl.tsx +
    // UserMenu.tsx): its trigger is a button whose accessible name is the
    // logged-in user's OWN display name, and "Enter"/"Leave" render as
    // `menuitem`s inside that dropdown (above "Profil"), not top-level
    // buttons. The "System-Admin-Modus aktiv" text this test also checks is
    // NOT part of the dropdown -- it is a SEPARATE, always-visible NavSidebar
    // banner added in the same commit (role="status", reuses the identical
    // `systemAdminModeActive` i18n key) that sits above "Dashboard" whenever
    // elevated, independent of whether the dropdown is open.
    const userMenuTrigger = page.getByRole("button", { name: SAMODE_ADMIN_NAME });

    // The step-up control itself is present (system_admin role) -- opening
    // the dropdown and finding the "Enter" menuitem is the presence-first
    // assertion before we check the nav absence.
    await userMenuTrigger.click();
    const enterItem = page.getByRole("menuitem", { name: t.systemAdminModeEnter });
    await expect(enterItem).toBeVisible();

    // Bootstrap system_admin starts NOT elevated -> the System nav item is
    // hidden and the NavSidebar "active" banner is absent. Close the dropdown
    // first so it isn't itself contributing a (disabled, elevated-only) match
    // for either locator.
    await page.keyboard.press("Escape");
    await expect(page.getByRole("link", { name: t.system })).toHaveCount(0);
    await expect(page.getByText(t.systemAdminModeActive)).toHaveCount(0);

    // Enter mode with the password (require-password is the default).
    await userMenuTrigger.click();
    await page.getByRole("menuitem", { name: t.systemAdminModeEnter }).click();
    const dialog = page.getByRole("dialog");
    await expect(dialog).toBeVisible();
    await dialog.getByLabel(t.systemAdminModePasswordLabel).fill(SAMODE_ADMIN_PASSWORD);
    // The dialog's confirm button reuses the same label as the "Enter" item
    // and the dialog title -- scope to the dialog role to hit the submit button.
    await dialog.getByRole("button", { name: t.systemAdminModeEnter }).click();
    await expect(dialog).toHaveCount(0);

    // System nav appears + the NavSidebar active-status banner shows, with no
    // dropdown open (proving the banner is independent of the menu). The
    // dropdown's own Menu is `keepMounted` (see UserMenu.tsx), so its now-
    // closed-but-still-in-the-DOM disabled "active" menuitem carries the SAME
    // `systemAdminModeActive` text as the banner -- an unscoped getByText
    // resolves to both and strict mode throws. Scope to the nav landmark
    // (`aria-label={t.portalNavigation}`) to hit only the banner.
    await expect(page.getByRole("link", { name: t.system })).toBeVisible();
    await expect(page.getByLabel(t.portalNavigation).getByText(t.systemAdminModeActive)).toBeVisible();

    // Leave (via the dropdown's "leave" menuitem, no password step) -> nav
    // hidden again, active banner gone, "Enter" menuitem is back.
    await userMenuTrigger.click();
    await page.getByRole("menuitem", { name: t.systemAdminModeLeave }).click();
    await expect(page.getByRole("link", { name: t.system })).toHaveCount(0);
    await expect(page.getByText(t.systemAdminModeActive)).toHaveCount(0);
    await userMenuTrigger.click();
    await expect(page.getByRole("menuitem", { name: t.systemAdminModeEnter })).toBeVisible();
  });
});
