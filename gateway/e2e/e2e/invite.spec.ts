// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { expect, test } from "@playwright/test";
import { messages } from "../../frontend/src/i18n";
import { inviteUser, login, setPassword } from "./helpers";

test("admin invites a user who sets a password and is auto-logged in", async ({ page, browser }) => {
  await login(page, "dev@example.test", "dev-secret");
  const { email, inviteUrl } = await inviteUser(page);

  // The invitee redeems the link in a fresh context (no existing session).
  const context = await browser.newContext();
  const invitee = await context.newPage();
  await setPassword(invitee, inviteUrl, "invitee-password-1");
  // set-password sets a session cookie; following the success link lands on the
  // dashboard without a manual login (auto-login).
  await invitee.getByRole("link", { name: messages.de.signIn }).click();
  await expect(invitee.getByText(messages.de.welcome)).toBeVisible();
  await context.close();

  // The invited user can also log in fresh with the new password.
  const ctx2 = await browser.newContext();
  const page2 = await ctx2.newPage();
  await login(page2, email, "invitee-password-1");
  await expect(page2.getByText(messages.de.welcome)).toBeVisible();
  await ctx2.close();
});

test("a non-admin user does not see the user management view", async ({ page, browser }) => {
  await login(page, "dev@example.test", "dev-secret");
  const { inviteUrl } = await inviteUser(page); // default role: user

  const context = await browser.newContext();
  const userPage = await context.newPage();
  await setPassword(userPage, inviteUrl, "user-password-1");
  // Reuse the auto-login session from set-password.
  await userPage.goto("/portal/");
  await expect(userPage.getByText(messages.de.welcome)).toBeVisible();
  await userPage.getByRole("link", { name: messages.de.users }).click();
  await expect(userPage.locator("#user-email")).toHaveCount(0); // no admin create form
  await expect(userPage.getByText(messages.de.stubIntro)).toBeVisible();
  await context.close();
});
