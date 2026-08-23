// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { expect, type Page } from "@playwright/test";
import { messages } from "../../frontend/src/i18n";

let counter = 0;

// Unique per test run so created users never collide in the shared memory-mode gateway.
export function uniqueEmail(prefix = "e2e"): string {
  counter += 1;
  return `${prefix}-${Date.now()}-${counter}@example.test`;
}

export async function login(page: Page, email: string, password: string): Promise<void> {
  await page.goto("/portal/");
  await page.locator("#login-email").fill(email);
  await page.locator("#login-password").fill(password);
  await page.getByRole("button", { name: messages.de.loginButton }).click();
  await expect(page.getByText(messages.de.welcome)).toBeVisible();
}

// Invite a new user through the admin Users view; returns the one-time invite URL.
export async function inviteUser(
  adminPage: Page,
  opts: { role?: "user" | "admin" } = {}
): Promise<{ email: string; inviteUrl: string }> {
  await adminPage.getByRole("link", { name: messages.de.users }).click();
  // The create form is now a sub-view: open it via the list's "create" action
  // (same label as the form's submit button, but never on screen simultaneously).
  await adminPage.getByRole("button", { name: messages.de.userCreate }).click();
  const email = uniqueEmail();
  await adminPage.locator("#user-email").fill(email);
  await adminPage.locator("#user-name").fill("E2E User");
  if (opts.role === "admin") {
    // Role is a MUI Select now (not a native <select>): open it, then pick the option.
    // exact: "Admin" would otherwise also match "System-Admin".
    await adminPage.getByRole("combobox", { name: messages.de.tableRole }).click();
    await adminPage.getByRole("option", { name: messages.de.roleAdmin, exact: true }).click();
  }
  await adminPage.getByRole("button", { name: messages.de.userCreate }).click();
  const inviteUrl = (await adminPage.locator('[data-testid="secret-reveal"] code').innerText()).trim();
  // Close the invite modal so the caller can interact with the user list beneath it.
  await adminPage.getByRole("button", { name: messages.de.captureClose }).click();
  return { email, inviteUrl };
}

// Redeem a set-password invite link and assert success.
export async function setPassword(page: Page, inviteUrl: string, password: string): Promise<void> {
  await page.goto(inviteUrl);
  await page.locator("#sp-password").fill(password);
  await page.locator("#sp-confirm").fill(password);
  await page.getByRole("button", { name: messages.de.setPasswordButton }).click();
  await expect(page.getByText(messages.de.setPasswordSuccess)).toBeVisible();
}
