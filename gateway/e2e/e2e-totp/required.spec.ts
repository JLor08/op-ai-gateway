// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { expect, test } from "@playwright/test";
import { messages } from "../../frontend/src/i18n";
import { inviteUser, login } from "../e2e/helpers";
import { totpCode } from "./totp";
import { setTotpMode } from "./totp.helpers";

const t = messages.de;

test("required mode: set-password forces enrollment before a session", async ({ page, browser }) => {
  // Dev admin logs in (mode is still optional/off here → no gate), then enables required.
  await login(page, "dev@example.test", "dev-secret");
  await setTotpMode(page, "required");

  // Invite a fresh user.
  const { email, inviteUrl } = await inviteUser(page);
  const password = "totp-required-1";

  // Redeem the invite in a fresh context. In required mode set-password returns
  // the enrollment payload (secret_base32) and NO session — read the secret off it.
  const ctx = await browser.newContext();
  const invitee = await ctx.newPage();
  await invitee.goto(inviteUrl);
  await invitee.locator("#sp-password").fill(password);
  await invitee.locator("#sp-confirm").fill(password);

  const [spRes] = await Promise.all([
    invitee.waitForResponse(
      (r) => r.url().includes("/api/auth/set-password") && r.request().method() === "POST"
    ),
    invitee.getByRole("button", { name: t.setPasswordButton }).click()
  ]);
  expect(spRes.ok()).toBeTruthy();
  const body = (await spRes.json()) as { totp_enrollment_required?: boolean; secret_base32?: string };
  expect(body.totp_enrollment_required).toBe(true);
  expect(body.secret_base32).toMatch(/^[A-Z2-7]+$/);

  // No session yet: the enrollment step (QR + code field) is on screen.
  await expect(invitee.getByText(t.welcome)).toHaveCount(0);
  await expect(invitee.locator("#sp-totp")).toBeVisible();

  // Complete enrollment with a computed code → confirms, enables, issues the session.
  // (The enrollment step's submit button is labeled with loginVerifyButton, not
  // setPasswordButton — SetPassword.tsx reuses the login-flow "Verify" button here,
  // confirmed by SetPassword.test.tsx.)
  await invitee.locator("#sp-totp").fill(totpCode(body.secret_base32!));
  await invitee.getByRole("button", { name: t.loginVerifyButton }).click();
  // confirmEnroll() sets the session cookie but SetPassword's own "done" state just
  // shows a static success alert with a sign-in link — it does not auto-navigate into
  // the authenticated app (SetPassword.tsx's success Alert action button, href to
  // BASE_URL). Follow that link to land on the (already-authenticated) Dashboard.
  await invitee.getByRole("link", { name: t.signIn }).click();
  await expect(invitee.getByText(t.welcome)).toBeVisible();

  await ctx.close();

  // Restore a clean gate for retries (dev page still authenticated).
  await setTotpMode(page, "off");
});
