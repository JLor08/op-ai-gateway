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
  const submit = adminPage.getByRole("button", { name: messages.de.userCreate });
  // Submit is disabled whenever no admin group can be auto-selected
  // (adminGroupMissing in UsersView.tsx) -- the precondition is that the
  // actor must manage at least one admin group. Asserting this BEFORE the
  // click turns a silent 30s timeout (waiting on a permanently-disabled
  // button) into an immediate assertion failure that names what is missing.
  await expect(submit).toBeEnabled();
  await submit.click();
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

// The dev seed's two models (both visibility: shown) -- see main.go's
// seedDefaultServer. Any test that just needs A model, not a specific one,
// defaults to this.
const DEFAULT_CHAT_MODEL = "gpt-oss-20b";

// Select a chat model BY NAME (not position, so the test says what it
// actually depends on) in the currently-open chat. A fresh chat starts with
// no model selected (DEFAULTS.model = '' in chatDoc.ts -- a deliberate
// product choice, not a bug), so any test that wants to send a message must
// pick one itself: Send's modelAvailable gate stays false, and disabled,
// until a model is chosen. The picker is a MUI SearchableSelect (role
// "combobox"), not a native <select> -- same open-then-click-option pattern
// as inviteUser's role select above.
export async function selectChatModel(page: Page, modelName = DEFAULT_CHAT_MODEL): Promise<void> {
  await page.getByRole("combobox", { name: messages.de.chatModel }).click();
  await page.getByRole("option", { name: modelName }).click();
}

// Chats persist server-side and the newest is auto-opened on load, so each
// test starts in a FRESH chat (via the sidebar's "Neuer Chat") to isolate its
// transcript from any chat a prior test left on the shared (memory) gateway.
// Also picks a model (see selectChatModel): a fresh chat has none selected.
export async function openFreshChat(page: Page, modelName = DEFAULT_CHAT_MODEL): Promise<void> {
  await page.getByRole("link", { name: messages.de.chat }).click();
  await page.getByRole("button", { name: messages.de.chatNewChat }).click();
  await expect(page.locator('[data-role="assistant"]')).toHaveCount(0);
  await selectChatModel(page, modelName);
}
