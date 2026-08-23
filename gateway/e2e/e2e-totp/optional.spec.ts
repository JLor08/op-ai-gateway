// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { expect, test } from "@playwright/test";
import { messages } from "../../frontend/src/i18n";
import { inviteUser, login, setPassword } from "../e2e/helpers";
import { totpCode, wrongCode } from "./totp";
import { enrollFromProfile, goToProfile, setTotpMode } from "./totp.helpers";

const t = messages.de;

test("optional mode: profile enrollment then the login TOTP gate", async ({ page, browser }) => {
  // 1. Dev admin enables optional TOTP.
  await login(page, "dev@example.test", "dev-secret");
  await setTotpMode(page, "optional");

  // 2. Invite a fresh user and set their password (no forced enrollment yet).
  const { email, inviteUrl } = await inviteUser(page);
  const password = "totp-optional-1";
  const ctx = await browser.newContext();
  const user = await ctx.newPage();
  // set-password auto-logs-in in optional mode (issues a session cookie), so
  // the invitee page is ALREADY authenticated — do NOT call login() again.
  await setPassword(user, inviteUrl, password);

  // 3. Go straight to the profile and enroll from there.
  await user.goto("/portal/");
  await goToProfile(user, "E2E User");
  const secret = await enrollFromProfile(user);
  await user.locator("#totp-code").fill(totpCode(secret));
  await user.getByRole("button", { name: t.totpConfirmButton }).click();
  await expect(user.getByText(t.totpConfirmSuccess)).toBeVisible();

  // 4. Log out.
  await user.getByRole("button", { name: "E2E User" }).click();
  await user.getByRole("menuitem", { name: t.logout }).click();
  await expect(user.getByRole("heading", { name: t.signIn })).toBeVisible();

  // 5a. Password only → gated (no session, code field appears, still on login).
  await user.locator("#login-email").fill(email);
  await user.locator("#login-password").fill(password);
  await user.getByRole("button", { name: t.loginButton }).click();
  await expect(user.locator("#login-totp")).toBeVisible();
  await expect(user.getByText(t.welcome)).toHaveCount(0);

  // 5b. Wrong code → 401 auth.totp_invalid, still gated.
  await user.locator("#login-totp").fill(wrongCode(secret));
  await user.getByRole("button", { name: t.loginVerifyButton }).click();
  await expect(user.getByRole("alert")).toContainText(t.errorAuthTotpInvalid);
  await expect(user.getByText(t.welcome)).toHaveCount(0);

  // 5c. Correct code → session. (The app remembers the pre-logout "profile"
  // view across the re-login, so navigate to Dashboard explicitly to assert
  // the authenticated welcome text.)
  await user.locator("#login-totp").fill(totpCode(secret));
  await user.getByRole("button", { name: t.loginVerifyButton }).click();
  await user.getByRole("link", { name: t.dashboard }).click();
  await expect(user.getByText(t.welcome)).toBeVisible();

  await ctx.close();
});
