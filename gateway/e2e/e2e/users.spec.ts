// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { expect, test } from "@playwright/test";
import { messages } from "../../frontend/src/i18n";
import { inviteUser, login, setPassword } from "./helpers";

test("admin creates a user who appears in the list", async ({ page }) => {
  await login(page, "dev@example.test", "dev-secret");
  const { email } = await inviteUser(page);
  await expect(page.getByRole("cell", { name: email })).toBeVisible();
});

test("a disabled user can no longer log in", async ({ page, browser }) => {
  await login(page, "dev@example.test", "dev-secret");
  const { email, inviteUrl } = await inviteUser(page);

  // Activate the user and confirm they can log in.
  const ctx = await browser.newContext();
  const userPage = await ctx.newPage();
  await setPassword(userPage, inviteUrl, "to-be-disabled-1");
  await userPage.goto("/portal/");
  await expect(userPage.getByText(messages.de.welcome)).toBeVisible();
  await ctx.close();

  // Admin disables that user's row; the action button flips to "enable".
  const row = page.locator("tr", { hasText: email });
  await row.getByRole("button", { name: messages.de.userActionDisable }).click();
  await expect(row.getByRole("button", { name: messages.de.userActionEnable })).toBeVisible();

  // Login now fails with the generic credentials error.
  const ctx2 = await browser.newContext();
  const page2 = await ctx2.newPage();
  await page2.goto("/portal/");
  await page2.locator("#login-email").fill(email);
  await page2.locator("#login-password").fill("to-be-disabled-1");
  await page2.getByRole("button", { name: messages.de.loginButton }).click();
  await expect(page2.getByRole("alert")).toContainText(messages.de.errorAuthInvalidCredentials);
  await ctx2.close();
});
